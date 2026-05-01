package linear

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/georgehuang/admiral/internal/store"
)

func TestGetWorkflowStates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req graphQLRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Query == "" {
			t.Fatal("query is empty")
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"workflowStates": map[string]any{
					"nodes": []map[string]any{
						{"id": "state-1", "name": "Backlog", "type": "backlog", "position": 0.0},
						{"id": "state-2", "name": "Todo", "type": "unstarted", "position": 1.0},
						{"id": "state-3", "name": "In Progress", "type": "started", "position": 2.0},
						{"id": "state-4", "name": "Done", "type": "completed", "position": 3.0},
					},
				},
			},
		})
	}))
	defer server.Close()

	c := NewClient(server.URL, "test-token")
	states, err := c.GetWorkflowStates(context.Background(), "team-abc")
	if err != nil {
		t.Fatalf("GetWorkflowStates failed: %v", err)
	}
	if len(states) != 4 {
		t.Fatalf("expected 4 states, got %d", len(states))
	}
	if states[2].Type != "started" {
		t.Errorf("expected type 'started', got %q", states[2].Type)
	}
	if states[2].ID != "state-3" {
		t.Errorf("expected id 'state-3', got %q", states[2].ID)
	}
}

func TestIssueUpdate(t *testing.T) {
	var receivedBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&receivedBody); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"issueUpdate": map[string]any{"success": true},
			},
		})
	}))
	defer server.Close()

	c := NewClient(server.URL, "test-token")
	err := c.IssueUpdate(context.Background(), "issue-123", "state-done")
	if err != nil {
		t.Fatalf("IssueUpdate failed: %v", err)
	}

	vars := receivedBody["variables"].(map[string]any)
	input := vars["input"].(map[string]any)
	if input["stateId"] != "state-done" {
		t.Errorf("expected stateId 'state-done', got %v", input["stateId"])
	}
	if vars["id"] != "issue-123" {
		t.Errorf("expected id 'issue-123', got %v", vars["id"])
	}
}

// mockStore is a test double for store.Store.
type mockStore struct {
	mu             sync.Mutex
	tokens         []*store.LinearOAuthToken
	saveCalled     int
	saveToken      *store.LinearOAuthToken
	authErr        store.AuthErrorState
	markBrokenArgs []string
	clearCalled    int
}

func (m *mockStore) GetLinearOAuthToken() (*store.LinearOAuthToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.tokens) == 0 {
		return nil, nil
	}
	return m.tokens[len(m.tokens)-1], nil
}

func (m *mockStore) SaveLinearOAuthToken(accessToken, refreshToken, expiresAt string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saveCalled++
	m.saveToken = &store.LinearOAuthToken{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresAt:    expiresAt,
		UpdatedAt:    time.Now().UTC().Format(time.RFC3339),
	}
	m.tokens = append(m.tokens, &store.LinearOAuthToken{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresAt:    expiresAt,
		UpdatedAt:    time.Now().UTC().Format(time.RFC3339),
	})
	return nil
}

func (m *mockStore) SaveCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.saveCalled
}

func (m *mockStore) GetAuthError() (store.AuthErrorState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.authErr, nil
}

func (m *mockStore) MarkAuthBroken(reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.markBrokenArgs = append(m.markBrokenArgs, reason)
	if m.authErr.Reason == "" {
		m.authErr = store.AuthErrorState{
			Reason: reason,
			ErrAt:  time.Now().UTC().Format(time.RFC3339),
		}
	}
	return nil
}

func (m *mockStore) ClearAuthError() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clearCalled++
	m.authErr = store.AuthErrorState{}
	return nil
}

// Test 1: API call → 401 → token refresh succeeds → retry succeeds.
func TestClient_401TriggersRefreshAndRetry(t *testing.T) {
	var refreshCalls, apiCalls atomic.Int32

	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCalls.Add(1)
		json.NewEncoder(w).Encode(map[string]string{
			"access_token":  "new_access_token",
			"refresh_token": "new_refresh_token",
		})
	}))
	defer tokenServer.Close()

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiCalls.Add(1)
		auth := r.Header.Get("Authorization")
		if auth == "Bearer new_access_token" {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"ok": true}})
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error": "unauthorized"}`))
	}))
	defer apiServer.Close()

	ms := &mockStore{tokens: []*store.LinearOAuthToken{
		{AccessToken: "old_token", RefreshToken: "refresh_token"},
	}}

	tr, _ := NewTokenRefresher("client_id", "client_secret", ms, nil, tokenServer.URL)

	c := NewClient(apiServer.URL, "old_token")
	c.SetTokenRefresher(tr)

	ctx := context.Background()
	var out map[string]any
	err := c.do(ctx, graphQLRequest{Query: `{ ok }`}, &out)
	if err != nil {
		t.Fatalf("do failed: %v", err)
	}
	if refreshCalls.Load() != 1 {
		t.Errorf("expected 1 refresh call, got %d", refreshCalls.Load())
	}
	if apiCalls.Load() != 2 {
		t.Errorf("expected 2 API calls (fail + retry), got %d", apiCalls.Load())
	}
}

// Test 2: Token endpoint returns 400 invalid_grant → expect ErrTokenRefreshFailed.
func TestClient_401InvalidGrant(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
	}))
	defer tokenServer.Close()

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error": "unauthorized"}`))
	}))
	defer apiServer.Close()

	ms := &mockStore{tokens: []*store.LinearOAuthToken{
		{AccessToken: "old_token", RefreshToken: "bad_refresh_token"},
	}}

	tr, _ := NewTokenRefresher("client_id", "client_secret", ms, nil, tokenServer.URL)

	c := NewClient(apiServer.URL, "old_token")
	c.SetTokenRefresher(tr)

	ctx := context.Background()
	var out map[string]any
	err := c.do(ctx, graphQLRequest{Query: `{ ok }`}, &out)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	// The error should mention token refresh failure.
	if err.Error() == "" {
		t.Fatal("error message empty")
	}
}

// Test 3: Concurrent 5 API calls all 401 → only 1 refresh.
func TestClient_Concurrent401_OnlyOneRefresh(t *testing.T) {
	var refreshCalls atomic.Int32
	var blockCh = make(chan struct{})

	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCalls.Add(1)
		<-blockCh // simulate slow network
		json.NewEncoder(w).Encode(map[string]string{
			"access_token":  "new_access_token",
			"refresh_token": "new_refresh_token",
		})
	}))
	defer tokenServer.Close()

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth == "Bearer new_access_token" {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"ok": true}})
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error": "unauthorized"}`))
	}))
	defer apiServer.Close()

	ms := &mockStore{tokens: []*store.LinearOAuthToken{
		{AccessToken: "old_token", RefreshToken: "refresh_token"},
	}}

	tr, _ := NewTokenRefresher("client_id", "client_secret", ms, nil, tokenServer.URL)

	c := NewClient(apiServer.URL, "old_token")
	c.SetTokenRefresher(tr)

	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var out map[string]any
			_ = c.do(ctx, graphQLRequest{Query: `{ ok }`}, &out)
		}()
	}

	// Give goroutines time to all hit the 401 simultaneously and enter wait.
	time.Sleep(100 * time.Millisecond)

	// Now unblock the single refresh.
	close(blockCh)
	wg.Wait()

	// Singleflight: only 1 refresh should have happened.
	if refreshCalls.Load() != 1 {
		t.Errorf("expected 1 refresh call (singleflight), got %d", refreshCalls.Load())
	}
}

// Test 4: No token refresher configured → 401 propagates as error.
func TestClient_401NoRefresher(t *testing.T) {
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error": "unauthorized"}`))
	}))
	defer apiServer.Close()

	c := NewClient(apiServer.URL, "old_token")
	// No refresher set.

	ctx := context.Background()
	var out map[string]any
	err := c.do(ctx, graphQLRequest{Query: `{ ok }`}, &out)
	if err == nil {
		t.Fatal("expected error for 401 without refresher")
	}
}

// Test 5: NewTokenRefresher returns false when clientID or clientSecret is empty.
func TestNewTokenRefresher_MissingCredentials(t *testing.T) {
	ms := &mockStore{}
	_, available := NewTokenRefresher("", "", ms, nil, "")
	if available {
		t.Error("expected false when clientID is empty")
	}
	_, available = NewTokenRefresher("id", "", ms, nil, "")
	if available {
		t.Error("expected false when clientSecret is empty")
	}
	_, available = NewTokenRefresher("id", "secret", ms, nil, "")
	if !available {
		t.Error("expected true when both credentials are present")
	}
}

// Test 6: doWithToken retry uses the new token for retry.
func TestClient_doWithTokenRetry(t *testing.T) {
	var calls atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		auth := r.Header.Get("Authorization")
		if auth == "Bearer new_token" {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"ok": true}})
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error": "unauthorized"}`))
	}))
	defer server.Close()

	c := NewClient(server.URL, "old_token")
	tr := &testRefresher{token: "new_token"}
	c.SetTokenRefresher(tr)

	ctx := context.Background()
	var out map[string]any
	err := c.do(ctx, graphQLRequest{Query: `{ ok }`}, &out)
	if err != nil {
		t.Fatalf("do failed: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("expected 2 calls, got %d", calls.Load())
	}
}

// testRefresher is a minimal refresher for testing retry.
type testRefresher struct{ token string }

func (r *testRefresher) RefreshAndRetry(ctx context.Context) (string, error) {
	return r.token, nil
}

// Test: refresh request uses application/x-www-form-urlencoded with correct fields.
func TestTokenRefresher_DoRefresh_FormURLEncoded(t *testing.T) {
	var receivedContentType string
	var receivedBody map[string]string

	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedContentType = r.Header.Get("Content-Type")
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		receivedBody = map[string]string{
			"client_id":     r.FormValue("client_id"),
			"client_secret": r.FormValue("client_secret"),
			"grant_type":    r.FormValue("grant_type"),
			"refresh_token": r.FormValue("refresh_token"),
		}
		json.NewEncoder(w).Encode(map[string]string{
			"access_token":  "new_access_token",
			"refresh_token": "new_refresh_token",
		})
	}))
	defer tokenServer.Close()

	ms := &mockStore{tokens: []*store.LinearOAuthToken{
		{AccessToken: "old_token", RefreshToken: "refresh_token"},
	}}
	tr, _ := NewTokenRefresher("test_client_id", "test_client_secret", ms, nil, tokenServer.URL)

	_, _ = tr.RefreshAndRetry(context.Background())

	if receivedContentType != "application/x-www-form-urlencoded" {
		t.Errorf("expected Content-Type 'application/x-www-form-urlencoded', got %q", receivedContentType)
	}
	if receivedBody["client_id"] != "test_client_id" {
		t.Errorf("expected client_id 'test_client_id', got %q", receivedBody["client_id"])
	}
	if receivedBody["client_secret"] != "test_client_secret" {
		t.Errorf("expected client_secret 'test_client_secret', got %q", receivedBody["client_secret"])
	}
	if receivedBody["grant_type"] != "refresh_token" {
		t.Errorf("expected grant_type 'refresh_token', got %q", receivedBody["grant_type"])
	}
	if receivedBody["refresh_token"] != "refresh_token" {
		t.Errorf("expected refresh_token 'refresh_token', got %q", receivedBody["refresh_token"])
	}
}

// Test: error attribution - invalid_grant.
func TestTokenRefresher_DoRefresh_InvalidGrant(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{
			"error":             "invalid_grant",
			"error_description": "Refresh token expired",
		})
	}))
	defer tokenServer.Close()

	ms := &mockStore{tokens: []*store.LinearOAuthToken{
		{AccessToken: "old_token", RefreshToken: "bad_refresh_token"},
	}}
	tr, _ := NewTokenRefresher("client_id", "client_secret", ms, nil, tokenServer.URL)

	_, err := tr.RefreshAndRetry(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	errStr := err.Error()
	if !strings.Contains(errStr, "invalid_grant") {
		t.Errorf("expected error to contain 'invalid_grant', got %q", errStr)
	}
	if strings.Contains(errStr, "or network error") {
		t.Errorf("error should not contain 'or network error', got %q", errStr)
	}
	if !errors.Is(err, ErrTokenRefreshFailed) {
		t.Errorf("expected errors.Is(err, ErrTokenRefreshFailed) to be true")
	}
}

// Test: error attribution - invalid_request.
func TestTokenRefresher_DoRefresh_InvalidRequest(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{
			"error":             "invalid_request",
			"error_description": "Invalid request: content must be application/x-www-form-urlencoded",
		})
	}))
	defer tokenServer.Close()

	ms := &mockStore{tokens: []*store.LinearOAuthToken{
		{AccessToken: "old_token", RefreshToken: "refresh_token"},
	}}
	tr, _ := NewTokenRefresher("client_id", "client_secret", ms, nil, tokenServer.URL)

	_, err := tr.RefreshAndRetry(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	errStr := err.Error()
	if !strings.Contains(errStr, "invalid_request") {
		t.Errorf("expected error to contain 'invalid_request', got %q", errStr)
	}
	if !errors.Is(err, ErrTokenRefreshFailed) {
		t.Errorf("expected errors.Is(err, ErrTokenRefreshFailed) to be true")
	}
}

// Test: error attribution - 5xx returns transient error after retry exhaustion.
// With retry, the token server 5xx is retried (1s+2s+4s delays) before returning.
func TestTokenRefresher_DoRefresh_5xxTransient(t *testing.T) {
	var calls atomic.Int32
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "server_error",
		})
	}))
	defer tokenServer.Close()

	ms := &mockStore{tokens: []*store.LinearOAuthToken{
		{AccessToken: "old_token", RefreshToken: "refresh_token"},
	}}
	tr, _ := NewTokenRefresher("client_id", "client_secret", ms, nil, tokenServer.URL)

	_, err := tr.RefreshAndRetry(context.Background())
	if err == nil {
		t.Fatal("expected error after retries, got nil")
	}
	// Retries: 1 initial + 3 retries = 4 calls (7s total backoff)
	if calls.Load() != 4 {
		t.Errorf("expected 4 calls (1 initial + 3 retries), got %d", calls.Load())
	}
	if !errors.Is(err, ErrTokenRefreshFailed) {
		t.Errorf("expected errors.Is(err, ErrTokenRefreshFailed) to be true, got: %v", err)
	}
}

// --- retryHTTP tests ---

// Test: mock returns 500 three times → retry 3 times, eventually fail.
func TestClient_RetryHTTP_500_ThreeTimes(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"boom"}`))
	}))
	defer server.Close()

	c := NewClient(server.URL, "test-token")
	ctx := context.Background()
	var out map[string]any
	err := c.do(ctx, graphQLRequest{Query: `{ ok }`}, &out)
	if err == nil {
		t.Fatal("expected error after 3 retries, got nil")
	}
	if calls.Load() != 4 {
		t.Errorf("expected 4 calls (3 retries + 1 initial), got %d", calls.Load())
	}
}

// Test: mock returns 500 once, then 200 → retry once then succeed.
func TestClient_RetryHTTP_500_ThenSuccess(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if calls.Load() <= 1 {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"error":"boom"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"ok": true}})
	}))
	defer server.Close()

	c := NewClient(server.URL, "test-token")
	ctx := context.Background()
	var out map[string]any
	err := c.do(ctx, graphQLRequest{Query: `{ ok }`}, &out)
	if err != nil {
		t.Fatalf("expected success after retry, got error: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("expected 2 calls, got %d", calls.Load())
	}
}

// Test: mock returns 4xx (non 408/429) → no retry, immediate error.
func TestClient_RetryHTTP_4xxNoRetry(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"errors":[{"message":"bad request"}]}`))
	}))
	defer server.Close()

	c := NewClient(server.URL, "test-token")
	ctx := context.Background()
	var out map[string]any
	err := c.do(ctx, graphQLRequest{Query: `{ ok }`}, &out)
	if err == nil {
		t.Fatal("expected error for 400, got nil")
	}
	if calls.Load() != 1 {
		t.Errorf("expected 1 call (no retry for 4xx), got %d", calls.Load())
	}
}

// Test: mock returns 429 → retry (verified not misclassified as permanent).
func TestClient_RetryHTTP_429Retries(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if calls.Load() <= 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"ok": true}})
	}))
	defer server.Close()

	c := NewClient(server.URL, "test-token")
	ctx := context.Background()
	var out map[string]any
	err := c.do(ctx, graphQLRequest{Query: `{ ok }`}, &out)
	if err != nil {
		t.Fatalf("expected success after 429 retry, got error: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("expected 2 calls (429 then success), got %d", calls.Load())
	}
}

// Test: mock returns 408 → retry (verified as transient).
func TestClient_RetryHTTP_408Retries(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if calls.Load() <= 1 {
			w.WriteHeader(http.StatusRequestTimeout)
			return
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"ok": true}})
	}))
	defer server.Close()

	c := NewClient(server.URL, "test-token")
	ctx := context.Background()
	var out map[string]any
	err := c.do(ctx, graphQLRequest{Query: `{ ok }`}, &out)
	if err != nil {
		t.Fatalf("expected success after 408 retry, got error: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("expected 2 calls (408 then success), got %d", calls.Load())
	}
}

// Test: invalid_grant from Linear flips the circuit breaker so the worker
// can short-circuit. The mark must include the error reason so the user
// knows what to fix.
func TestRefresh_InvalidGrant_MarksAuthBroken(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"Refresh token revoked"}`))
	}))
	defer tokenServer.Close()

	ms := &mockStore{tokens: []*store.LinearOAuthToken{
		{AccessToken: "stale", RefreshToken: "revoked_rt"},
	}}
	tr, _ := NewTokenRefresher("cid", "csec", ms, nil, tokenServer.URL)

	_, err := tr.RefreshAndRetry(context.Background())
	if err == nil {
		t.Fatal("expected error from invalid_grant refresh")
	}
	if len(ms.markBrokenArgs) != 1 {
		t.Fatalf("expected MarkAuthBroken to be called once, got %d calls", len(ms.markBrokenArgs))
	}
	if !strings.Contains(ms.markBrokenArgs[0], "invalid_grant") {
		t.Fatalf("MarkAuthBroken reason should mention invalid_grant; got %q", ms.markBrokenArgs[0])
	}
	if ms.authErr.Reason == "" {
		t.Fatal("authErr should be set after invalid_grant")
	}
}

// Test: when the breaker is already open, doRefresh returns immediately
// without making an HTTP call to Linear (no log spam, no quota burn).
func TestRefresh_CircuitBreakerOpen_SkipsHTTP(t *testing.T) {
	var calls atomic.Int32
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"access_token":"new"}`))
	}))
	defer tokenServer.Close()

	ms := &mockStore{
		tokens: []*store.LinearOAuthToken{
			{AccessToken: "stale", RefreshToken: "rt"},
		},
		authErr: store.AuthErrorState{Reason: "invalid_grant", ErrAt: "2026-04-30T12:00:00Z"},
	}
	tr, _ := NewTokenRefresher("cid", "csec", ms, nil, tokenServer.URL)

	_, err := tr.RefreshAndRetry(context.Background())
	if err == nil {
		t.Fatal("expected error when circuit breaker is open")
	}
	if calls.Load() != 0 {
		t.Errorf("expected 0 HTTP calls (breaker open), got %d", calls.Load())
	}
}

// Test: a successful refresh clears the breaker, so a transient mis-flag
// (or a recovery between admiral restarts) self-heals.
func TestRefresh_Success_ClearsAuthError(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "new_access",
			"refresh_token": "new_refresh",
			"expires_in":    86400,
		})
	}))
	defer tokenServer.Close()

	ms := &mockStore{tokens: []*store.LinearOAuthToken{
		{AccessToken: "stale", RefreshToken: "still_valid"},
	}}
	tr, _ := NewTokenRefresher("cid", "csec", ms, nil, tokenServer.URL)

	tok, err := tr.RefreshAndRetry(context.Background())
	if err != nil {
		t.Fatalf("RefreshAndRetry: %v", err)
	}
	if tok != "new_access" {
		t.Fatalf("expected new_access, got %q", tok)
	}
	if ms.clearCalled == 0 {
		t.Error("ClearAuthError should be called on successful refresh")
	}
}