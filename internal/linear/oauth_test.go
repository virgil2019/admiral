package linear

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
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
