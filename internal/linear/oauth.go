package linear

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Linear OAuth scopes required by admiral.
const OAuthScopes = "read,write,app:mentionable,app:assignable"

// BuildAuthorizeURL constructs the Linear OAuth authorization URL.
func BuildAuthorizeURL(clientID, redirectURI, scopes, state string) string {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", scopes)
	q.Set("actor", "app")
	q.Set("prompt", "consent")
	q.Set("state", state)
	return "https://linear.app/oauth/authorize?" + q.Encode()
}

// TokenResponse holds the token exchange response from Linear.
type TokenResponse struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int // seconds
	Scope        string
}

// Retry policy for the token POST. The exchange runs once per login and
// over networks (proxies, tunnels, GFW-adjacent paths) the connection can
// be dropped before any response arrives — a transient EOF / reset that
// the next attempt clears. A single failure is unrecoverable because the
// authorization code is one-time-use, so we retry transport hiccups in
// place. We only retry when no response was received (transport error) or
// the edge returned a 5xx — in both cases Linear has NOT consumed the
// code, so a retry can't double-spend it. A 4xx (invalid_grant, code
// already used) is permanent and returned immediately.
const (
	tokenMaxAttempts    = 3
	tokenRetryBaseDelay = 500 * time.Millisecond
	tokenAttemptTimeout = 15 * time.Second
)

// ExchangeCode exchanges an authorization code for an access token.
// tokenEndpoint should be "https://api.linear.app/oauth/token".
func ExchangeCode(ctx context.Context, tokenEndpoint, clientID, clientSecret, redirectURI, code string) (*TokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	form.Set("redirect_uri", redirectURI)
	form.Set("code", code)
	body := form.Encode()

	client := &http.Client{}

	var lastErr error
	for attempt := 1; attempt <= tokenMaxAttempts; attempt++ {
		status, raw, err := postToken(ctx, client, tokenEndpoint, body)
		if err != nil {
			// Parent cancellation/deadline is terminal — don't retry.
			// This check MUST precede isTransientHTTPErr below: a per-attempt
			// timeout surfaces as context.DeadlineExceeded (which the
			// classifier treats as transient), so the parent-ctx check is the
			// only thing that distinguishes "attempt timed out, retry" from
			// "caller gave up, stop". Ordering is load-bearing.
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = fmt.Errorf("token exchange request: %w", err)
			if isTransientHTTPErr(err) && attempt < tokenMaxAttempts {
				if werr := waitTokenBackoff(ctx, attempt); werr != nil {
					return nil, werr
				}
				continue
			}
			return nil, lastErr
		}

		if status == http.StatusOK {
			return parseTokenResponse(raw)
		}

		// 5xx from the edge: code not consumed, retry is safe.
		if isTransientStatus(status) && attempt < tokenMaxAttempts {
			lastErr = fmt.Errorf("linear token exchange failed with status %d: %s", status, string(raw))
			if werr := waitTokenBackoff(ctx, attempt); werr != nil {
				return nil, werr
			}
			continue
		}

		return nil, tokenErrorFromResponse(status, raw)
	}
	return nil, lastErr
}

// postToken issues a single token POST with its own per-attempt deadline
// derived from ctx, returning the status code and raw body. A nil error
// means a response was received (even a non-2xx one); a non-nil error
// means no response (transport failure).
func postToken(ctx context.Context, client *http.Client, endpoint, body string) (int, []byte, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, tokenAttemptTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, raw, nil
}

func parseTokenResponse(raw []byte) (*TokenResponse, error) {
	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token,omitempty"`
		ExpiresIn    int    `json:"expires_in,omitempty"`
		Scope        string `json:"scope,omitempty"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("parse token response: %w", err)
	}
	if result.AccessToken == "" {
		return nil, fmt.Errorf("token response missing access_token")
	}
	return &TokenResponse{
		AccessToken:  result.AccessToken,
		RefreshToken: result.RefreshToken,
		ExpiresIn:    result.ExpiresIn,
		Scope:        result.Scope,
	}, nil
}

func tokenErrorFromResponse(status int, raw []byte) error {
	var errResp struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	json.Unmarshal(raw, &errResp)
	if errResp.Description != "" {
		return fmt.Errorf("linear returned error: %s — %s", errResp.Error, errResp.Description)
	}
	return fmt.Errorf("linear token exchange failed with status %d: %s", status, string(raw))
}

func waitTokenBackoff(ctx context.Context, attempt int) error {
	t := time.NewTimer(time.Duration(attempt) * tokenRetryBaseDelay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// isTransientHTTPErr reports whether a transport error (no response
// received) is worth retrying. Typed-error checks mirror token.go's
// isTokenTransientNetErr; the difference is we also treat plain io.EOF as
// transient — the connection closed before any response headers arrived,
// which is the exact failure seen on this path (Go formats it as
// `Post "<url>": EOF`). The per-attempt context.DeadlineExceeded is
// retryable here because parent-context cancellation is checked separately
// by the caller before this is consulted.
func isTransientHTTPErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	// connection reset / TLS handshake timeout have no single sentinel
	// error; fall back to the substring shapes (matches the github path).
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "connection reset") ||
		strings.Contains(s, "tls handshake timeout")
}

func isTransientStatus(code int) bool {
	return code == http.StatusBadGateway ||
		code == http.StatusServiceUnavailable ||
		code == http.StatusGatewayTimeout
}
