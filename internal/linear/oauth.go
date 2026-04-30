package linear

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
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

// ExchangeCode exchanges an authorization code for an access token.
// tokenEndpoint should be "https://api.linear.app/oauth/token".
func ExchangeCode(ctx context.Context, tokenEndpoint, clientID, clientSecret, redirectURI, code string) (*TokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	form.Set("redirect_uri", redirectURI)
	form.Set("code", code)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token exchange request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read token response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var errResp struct {
			Error       string `json:"error"`
			Description string `json:"error_description"`
		}
		json.Unmarshal(raw, &errResp)
		if errResp.Description != "" {
			return nil, fmt.Errorf("linear returned error: %s — %s", errResp.Error, errResp.Description)
		}
		return nil, fmt.Errorf("linear token exchange failed with status %d: %s", resp.StatusCode, string(raw))
	}

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
		Scope:       result.Scope,
	}, nil
}