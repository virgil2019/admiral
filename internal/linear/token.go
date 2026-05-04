package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/georgehuang/admiral/internal/store"
)

// ErrTokenRefreshFailed is returned when token refresh fails (e.g.
// invalid_grant because the refresh token is expired or revoked).
var ErrTokenRefreshFailed = errors.New("token refresh failed")

// TokenStore is the interface that TokenRefresher needs from the DB layer.
// *store.Store implements this interface.
type TokenStore interface {
	GetLinearOAuthToken() (*store.LinearOAuthToken, error)
	SaveLinearOAuthToken(accessToken, refreshToken, expiresAt string) error
	GetAuthError() (store.AuthErrorState, error)
	MarkAuthBroken(reason string) error
	ClearAuthError() error
}

// TokenRefresher handles lazy 401-triggered OAuth token refresh with
// singleflight: concurrent API calls that all receive 401 will only trigger
// one refresh, and all callers share the result.
type TokenRefresher struct {
	clientID      string
	clientSecret  string
	store         TokenStore
	logger        *slog.Logger
	tokenEndpoint string

	mu       sync.Mutex
	waitCh   chan struct{} // closed when refresh is done
	sfResult tokenRefreshResult
}

// NewTokenRefresher creates a refresher. refreshAvailable reports whether
// all required credentials (clientID, clientSecret, stored refreshToken) are
// present — if false, the refresher logs a warning at construction and all
// refresh attempts will fast-fail. tokenEndpoint defaults to
// "https://api.linear.app/oauth/token" if not provided.
func NewTokenRefresher(clientID, clientSecret string, store TokenStore, logger *slog.Logger, tokenEndpoint string) (*TokenRefresher, bool) {
	if tokenEndpoint == "" {
		tokenEndpoint = "https://api.linear.app/oauth/token"
	}
	available := clientID != "" && clientSecret != ""
	if !available {
		if logger != nil {
			logger.Warn("linear_token_refresh_disabled",
				"reason", "client_id/client_secret not configured")
		}
	}
	return &TokenRefresher{
		clientID:      clientID,
		clientSecret:  clientSecret,
		store:         store,
		logger:        logger,
		tokenEndpoint: tokenEndpoint,
	}, available
}

type tokenRefreshResult struct {
	token string
	err   error
}

// Refresh returns the current valid access token, attempting a refresh if
// the stored token is empty. It is safe for concurrent use.
func (tr *TokenRefresher) Refresh(ctx context.Context) (string, error) {
	tok, err := tr.store.GetLinearOAuthToken()
	if err != nil {
		return "", fmt.Errorf("get token from store: %w", err)
	}
	if tok != nil && tok.AccessToken != "" {
		return tok.AccessToken, nil
	}
	if tr.clientID == "" || tr.clientSecret == "" {
		return "", errors.New("no access token available and token refresh not configured")
	}
	return "", errors.New("token store is empty")
}

// RefreshAndRetry executes a token refresh, updates the store, and returns
// the new token. On failure it returns ErrTokenRefreshFailed so callers can
// fail fast without retrying the original request. Concurrent callers all
// share the same refresh result (singleflight via mutex + channel).
func (tr *TokenRefresher) RefreshAndRetry(ctx context.Context) (string, error) {
	tr.mu.Lock()
	if tr.waitCh != nil {
		// Another goroutine is already refreshing; wait on existing channel.
		waitCh := tr.waitCh
		tr.mu.Unlock()
		<-waitCh
		return tr.sfResult.token, tr.sfResult.err
	}
	// We are the first; create the wait channel and start refresh.
	ch := make(chan struct{})
	tr.waitCh = ch
	tr.mu.Unlock()

	// Run refresh in background so waiters can join.
	go func() {
		newToken, refreshErr := tr.doRefresh(ctx)
		tr.mu.Lock()
		tr.sfResult = tokenRefreshResult{newToken, refreshErr}
		close(ch) // unblocks all waiters
		tr.waitCh = nil
		tr.mu.Unlock()
	}()

	// Wait for completion.
	<-ch
	return tr.sfResult.token, tr.sfResult.err
}

func (tr *TokenRefresher) doRefresh(ctx context.Context) (string, error) {
	// Circuit breaker: if a previous refresh permanently failed
	// (invalid_grant / invalid_client), every retry will fail the same way
	// and just spam the logs. Short-circuit until admiral-oauth clears the
	// flag via ClearAuthError on a fresh authorization.
	if st, err := tr.store.GetAuthError(); err == nil && st.Reason != "" {
		return "", fmt.Errorf("token refresh skipped (circuit breaker open since %s: %s): %w", st.ErrAt, st.Reason, ErrTokenRefreshFailed)
	}

	tok, err := tr.store.GetLinearOAuthToken()
	if err != nil {
		return "", fmt.Errorf("get current token: %w", err)
	}
	if tok == nil || tok.RefreshToken == "" {
		return "", errors.New("no refresh token available in store")
	}

	form := url.Values{}
	form.Set("client_id", tr.clientID)
	form.Set("client_secret", tr.clientSecret)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", tok.RefreshToken)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tr.tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("build refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := tr.doHTTPWithRetry(req)
	if err != nil {
		if tr.logger != nil {
			tr.logger.Warn("linear_token_refresh_network_error", "err", err)
		}
		return "", fmt.Errorf("token refresh failed: %w", ErrTokenRefreshFailed)
	}
	if resp == nil {
		return "", fmt.Errorf("token refresh failed: doHTTPWithRetry returned nil resp")
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read refresh response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		if tr.logger != nil {
			tr.logger.Warn("linear_token_refresh_http_error",
				"status", resp.StatusCode, "body", string(raw))
		}
		classified := tr.classifyError(resp.StatusCode, raw)
		// Permanent failure → flip the circuit breaker so subsequent
		// refresh attempts (and the worker, via store.GetAuthError) skip
		// Linear entirely until the user re-OAuths.
		if reason, ok := permanentFailureReason(raw); ok {
			if err := tr.store.MarkAuthBroken(reason); err != nil && tr.logger != nil {
				tr.logger.Warn("linear_token_mark_broken_failed", "err", err)
			}
		}
		return "", classified
	}

	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token,omitempty"`
		ExpiresIn    int    `json:"expires_in,omitempty"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("parse refresh response: %w", err)
	}
	if result.AccessToken == "" {
		return "", errors.New("refresh response missing access_token")
	}

	expiresAt := ""
	if result.ExpiresIn > 0 {
		expiresAt = "REFRESHED"
	}

	newRefreshToken := tok.RefreshToken
	if result.RefreshToken != "" {
		newRefreshToken = result.RefreshToken
	}

	if err := tr.store.SaveLinearOAuthToken(result.AccessToken, newRefreshToken, expiresAt); err != nil {
		if tr.logger != nil {
			tr.logger.Warn("linear_token_refresh_save_failed", "err", err)
		}
		return "", fmt.Errorf("save new token: %w", err)
	}

	// Defensive recovery: if the breaker was tripped earlier (e.g. a flaky
	// rotation race) and a later refresh actually succeeds, clear the flag
	// so the worker resumes draining events. Normal recovery path is
	// admiral-oauth, which clears it directly after exchange.
	if err := tr.store.ClearAuthError(); err != nil && tr.logger != nil {
		tr.logger.Warn("linear_token_clear_auth_error_failed", "err", err)
	}

	if tr.logger != nil {
		tr.logger.Info("linear_token_refreshed")
	}
	return result.AccessToken, nil
}

// permanentFailureReason inspects an OAuth error response body and returns
// (reason, true) when the error code is one Linear treats as permanent —
// i.e. no amount of retry can recover. invalid_grant means the refresh
// token was revoked / migrated away / expired; invalid_client means the
// app's client_secret was rotated. Both require manual re-authorization.
func permanentFailureReason(body []byte) (string, bool) {
	var resp struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	_ = json.Unmarshal(body, &resp)
	switch resp.Error {
	case "invalid_grant", "invalid_client":
		if resp.Description != "" {
			return fmt.Sprintf("%s: %s", resp.Error, resp.Description), true
		}
		return resp.Error, true
	}
	return "", false
}

// doHTTPWithRetry executes the request with exponential backoff for transient errors.
func (tr *TokenRefresher) doHTTPWithRetry(req *http.Request) (*http.Response, error) {
	delays := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second}
	// Read and clone body so each attempt gets a fresh reader.
	var bodyBytes []byte
	if req.Body != nil {
		bodyBytes, _ = io.ReadAll(req.Body)
		req.Body.Close()
	}
	var lastErr error
	client := &http.Client{Timeout: 30 * time.Second}
	for attempt := 0; attempt <= len(delays); attempt++ {
		if len(bodyBytes) > 0 {
			req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			if !isTokenTransientNetErr(err) || attempt == len(delays) {
				return nil, err
			}
		} else {
			if !isTokenTransientHTTPStatus(resp.StatusCode) {
				return resp, nil
			}
			resp.Body.Close()
			lastErr = fmt.Errorf("transient http %d", resp.StatusCode)
			if attempt == len(delays) {
				return nil, lastErr
			}
		}
		time.Sleep(delays[attempt])
	}
	return nil, lastErr
}

func isTokenTransientHTTPStatus(s int) bool {
	return s >= 500 || s == http.StatusRequestTimeout || s == http.StatusTooManyRequests
}

func isTokenTransientNetErr(err error) bool {
	// net.Error.Temporary deprecated since Go 1.18; Timeout() alone is
	// the right contract.
	var ne net.Error
	if errors.As(err, &ne) {
		return ne.Timeout()
	}
	return errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, io.ErrUnexpectedEOF)
}

func (tr *TokenRefresher) classifyError(status int, body []byte) error {
	var resp struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	json.Unmarshal(body, &resp)

	switch resp.Error {
	case "invalid_grant":
		return fmt.Errorf("token refresh failed: invalid_grant (refresh token expired or revoked, manual re-OAuth required): %w", ErrTokenRefreshFailed)
	case "invalid_request":
		return fmt.Errorf("token refresh failed: invalid_request (client bug — check request format): %w", ErrTokenRefreshFailed)
	case "invalid_client":
		return fmt.Errorf("token refresh failed: invalid_client (check client_id/client_secret config): %w", ErrTokenRefreshFailed)
	}

	if status >= 500 {
		return fmt.Errorf("token refresh failed: linear http %d (transient — will fail this attempt): %w", status, ErrTokenRefreshFailed)
	}

	return fmt.Errorf("token refresh failed: %w", ErrTokenRefreshFailed)
}
