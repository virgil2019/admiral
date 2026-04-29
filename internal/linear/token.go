package linear

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"

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
}

// TokenRefresher handles lazy 401-triggered OAuth token refresh with
// singleflight: concurrent API calls that all receive 401 will only trigger
// one refresh, and all callers share the result.
type TokenRefresher struct {
	clientID     string
	clientSecret string
	store        TokenStore
	logger       *slog.Logger
	tokenEndpoint string

	mu          sync.Mutex
	waitCh      chan struct{} // closed when refresh is done
	sfResult    tokenRefreshResult
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
		clientID:     clientID,
		clientSecret: clientSecret,
		store:        store,
		logger:       logger,
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
		close(ch)  // unblocks all waiters
		tr.waitCh = nil
		tr.mu.Unlock()
	}()

	// Wait for completion.
	<-ch
	return tr.sfResult.token, tr.sfResult.err
}

func (tr *TokenRefresher) doRefresh(ctx context.Context) (string, error) {
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

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if tr.logger != nil {
			tr.logger.Warn("linear_token_refresh_network_error", "err", err)
		}
		return "", fmt.Errorf("token refresh failed: %w", ErrTokenRefreshFailed)
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
		return "", tr.classifyError(resp.StatusCode, raw)
	}

	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token,omitempty"`
		ExpiresIn   int    `json:"expires_in,omitempty"`
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

	if tr.logger != nil {
		tr.logger.Info("linear_token_refreshed")
	}
	return result.AccessToken, nil
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