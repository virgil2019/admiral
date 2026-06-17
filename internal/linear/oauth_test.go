package linear

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
)

func TestBuildAuthorizeURL(t *testing.T) {
	authURL := BuildAuthorizeURL("my-client-id", "http://127.0.0.1:8080/callback", OAuthScopes, "csrf-state-123")
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("BuildAuthorizeURL returned invalid URL: %v", err)
	}

	if u.Scheme != "https" {
		t.Errorf("scheme = %q, want %q", u.Scheme, "https")
	}
	if u.Host != "linear.app" {
		t.Errorf("host = %q, want %q", u.Host, "linear.app")
	}
	if u.Path != "/oauth/authorize" {
		t.Errorf("path = %q, want %q", u.Path, "/oauth/authorize")
	}

	q := u.Query()
	if q.Get("response_type") != "code" {
		t.Errorf("response_type = %q, want %q", q.Get("response_type"), "code")
	}
	if q.Get("client_id") != "my-client-id" {
		t.Errorf("client_id = %q, want %q", q.Get("client_id"), "my-client-id")
	}
	if q.Get("redirect_uri") != "http://127.0.0.1:8080/callback" {
		t.Errorf("redirect_uri = %q, want %q", q.Get("redirect_uri"), "http://127.0.0.1:8080/callback")
	}
	if q.Get("scope") != OAuthScopes {
		t.Errorf("scope = %q, want %q", q.Get("scope"), OAuthScopes)
	}
	if q.Get("actor") != "app" {
		t.Errorf("actor = %q, want %q", q.Get("actor"), "app")
	}
	if q.Get("prompt") != "consent" {
		t.Errorf("prompt = %q, want %q", q.Get("prompt"), "consent")
	}
	if q.Get("state") != "csrf-state-123" {
		t.Errorf("state = %q, want %q", q.Get("state"), "csrf-state-123")
	}
}

func TestExchangeCode_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("method = %q, want %q", r.Method, "POST")
		}
		if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Errorf("Content-Type = %q, want %q", r.Header.Get("Content-Type"), "application/x-www-form-urlencoded")
		}

		err := r.ParseForm()
		if err != nil {
			t.Fatalf("parse form: %v", err)
		}
		if r.PostForm.Get("grant_type") != "authorization_code" {
			t.Errorf("grant_type = %q, want %q", r.PostForm.Get("grant_type"), "authorization_code")
		}
		if r.PostForm.Get("client_id") != "my-client-id" {
			t.Errorf("client_id = %q, want %q", r.PostForm.Get("client_id"), "my-client-id")
		}
		if r.PostForm.Get("client_secret") != "my-client-secret" {
			t.Errorf("client_secret = %q, want %q", r.PostForm.Get("client_secret"), "my-client-secret")
		}
		if r.PostForm.Get("redirect_uri") != "http://127.0.0.1:8080/callback" {
			t.Errorf("redirect_uri = %q, want %q", r.PostForm.Get("redirect_uri"), "http://127.0.0.1:8080/callback")
		}
		if r.PostForm.Get("code") != "auth-code-xyz" {
			t.Errorf("code = %q, want %q", r.PostForm.Get("code"), "auth-code-xyz")
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token":  "lin_oauth_newtoken123",
			"refresh_token": "lin_oauth_refreshtoken456",
			"expires_in":    3600,
			"scope":         "read,write,app:mentionable,app:assignable",
		})
	}))
	defer srv.Close()

	resp, err := ExchangeCode(context.Background(), srv.URL, "my-client-id", "my-client-secret", "http://127.0.0.1:8080/callback", "auth-code-xyz")
	if err != nil {
		t.Fatalf("ExchangeCode failed: %v", err)
	}

	if resp.AccessToken != "lin_oauth_newtoken123" {
		t.Errorf("AccessToken = %q, want %q", resp.AccessToken, "lin_oauth_newtoken123")
	}
	if resp.RefreshToken != "lin_oauth_refreshtoken456" {
		t.Errorf("RefreshToken = %q, want %q", resp.RefreshToken, "lin_oauth_refreshtoken456")
	}
	if resp.ExpiresIn != 3600 {
		t.Errorf("ExpiresIn = %d, want %d", resp.ExpiresIn, 3600)
	}
	if resp.Scope != "read,write,app:mentionable,app:assignable" {
		t.Errorf("Scope = %q, want %q", resp.Scope, "read,write,app:mentionable,app:assignable")
	}
}

func TestExchangeCode_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error":             "invalid_grant",
			"error_description": "The authorization code has expired",
		})
	}))
	defer srv.Close()

	_, err := ExchangeCode(context.Background(), srv.URL, "my-client-id", "my-client-secret", "http://127.0.0.1:8080/callback", "expired-code")
	if err == nil {
		t.Fatal("ExchangeCode expected error for 400 response, got nil")
	}
	errStr := err.Error()
	if !contains(errStr, "invalid_grant") {
		t.Errorf("error contains %q: %v", "invalid_grant", errStr)
	}
	if !contains(errStr, "The authorization code has expired") {
		t.Errorf("error contains %q: %v", "The authorization code has expired", errStr)
	}
}

// TestExchangeCode_RetriesTransientEOF drops the first connection before
// responding (the client sees io.EOF), then succeeds — the exact failure
// shape that motivated the retry. A single transport EOF must not fail the
// one-shot exchange.
func TestExchangeCode_RetriesTransientEOF(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&attempts, 1) == 1 {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("ResponseWriter is not a Hijacker")
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Fatalf("hijack: %v", err)
			}
			conn.Close() // drop without responding → client sees EOF
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"access_token": "tok-after-retry"})
	}))
	defer srv.Close()

	resp, err := ExchangeCode(context.Background(), srv.URL, "id", "secret", "http://127.0.0.1:8080/callback", "code")
	if err != nil {
		t.Fatalf("ExchangeCode failed after transient EOF: %v", err)
	}
	if resp.AccessToken != "tok-after-retry" {
		t.Errorf("AccessToken = %q, want %q", resp.AccessToken, "tok-after-retry")
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Errorf("attempts = %d, want 2 (1 EOF + 1 success)", got)
	}
}

// TestExchangeCode_RetriesTransient5xx retries a 503 then succeeds — the
// code is not consumed on a 5xx, so the retry is safe.
func TestExchangeCode_RetriesTransient5xx(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&attempts, 1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"access_token": "tok-after-503"})
	}))
	defer srv.Close()

	resp, err := ExchangeCode(context.Background(), srv.URL, "id", "secret", "http://127.0.0.1:8080/callback", "code")
	if err != nil {
		t.Fatalf("ExchangeCode failed after transient 503: %v", err)
	}
	if resp.AccessToken != "tok-after-503" {
		t.Errorf("AccessToken = %q, want %q", resp.AccessToken, "tok-after-503")
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Errorf("attempts = %d, want 2", got)
	}
}

// TestExchangeCode_NoRetryOn4xx ensures a permanent 4xx (e.g. invalid_grant)
// is returned immediately without burning retries — a retry would just
// waste time on a code that is already dead.
func TestExchangeCode_NoRetryOn4xx(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"error": "invalid_grant"})
	}))
	defer srv.Close()

	_, err := ExchangeCode(context.Background(), srv.URL, "id", "secret", "http://127.0.0.1:8080/callback", "code")
	if err == nil {
		t.Fatal("expected error for 400, got nil")
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("attempts = %d, want 1 (4xx must not retry)", got)
	}
}

// TestExchangeCode_Exhausts5xx confirms the loop terminates and returns the
// last error after exhausting all attempts on a persistent 5xx.
func TestExchangeCode_Exhausts5xx(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	_, err := ExchangeCode(context.Background(), srv.URL, "id", "secret", "http://127.0.0.1:8080/callback", "code")
	if err == nil {
		t.Fatal("expected error after exhausting retries, got nil")
	}
	if got := atomic.LoadInt32(&attempts); got != tokenMaxAttempts {
		t.Errorf("attempts = %d, want %d", got, tokenMaxAttempts)
	}
}

func TestIsTransientHTTPErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain io.EOF", io.EOF, true},
		{"wrapped io.EOF", fmt.Errorf(`Post "https://api.linear.app/oauth/token": %w`, io.EOF), true},
		{"unexpected EOF", io.ErrUnexpectedEOF, true},
		{"deadline exceeded", context.DeadlineExceeded, true},
		{"connection reset", errors.New("read tcp 1.2.3.4:5->6.7.8.9:443: connection reset by peer"), true},
		{"tls handshake timeout", errors.New("net/http: TLS handshake timeout"), true},
		{"permanent error", errors.New("invalid_grant"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTransientHTTPErr(tc.err); got != tc.want {
				t.Errorf("isTransientHTTPErr(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
