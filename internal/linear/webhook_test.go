package linear

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/georgehuang/admiral/internal/store"
)

func newTestWebhook(t *testing.T, h AgentHandler) *Webhook {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sig := make(chan struct{}, 1)
	return NewWebhook("test-secret", nil, sig, logger, h)
}

func newTestWebhookWithStore(t *testing.T, s *store.Store, sig chan<- struct{}) *Webhook {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewWebhook("test-secret", s, sig, logger, nil)
}

func post(t *testing.T, w *Webhook, body []byte, sig string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	if sig != "" {
		req.Header.Set("Linear-Signature", sig)
	}
	rec := httptest.NewRecorder()
	w.Handler().ServeHTTP(rec, req)
	return rec
}

func TestWebhook_BadSignature_Rejects(t *testing.T) {
	w := newTestWebhook(t, nil)
	rec := post(t, w, []byte(`{}`), "deadbeef")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401", rec.Code)
	}
}

func TestWebhook_NonAgentEvent_Ignored(t *testing.T) {
	called := false
	w := newTestWebhook(t, func(AgentEvent) { called = true })
	body := []byte(`{"type":"Issue","action":"update"}`)
	sig := SignBody([]byte("test-secret"), body)
	rec := post(t, w, body, sig)
	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", rec.Code)
	}
	time.Sleep(10 * time.Millisecond)
	if called {
		t.Error("handler invoked on non-AgentSessionEvent")
	}
}

func TestWebhook_AgentSessionCreated_Mention_Fires(t *testing.T) {
	var (
		wg  sync.WaitGroup
		got AgentEvent
	)
	wg.Add(1)
	w := newTestWebhook(t, func(e AgentEvent) {
		got = e
		wg.Done()
	})
	body := []byte(`{
		"type":"AgentSessionEvent",
		"action":"created",
		"webhookId":"wh-1",
		"agentSession":{
			"id":"sess-1",
			"issue":{"id":"issue-1","identifier":"TST-1","title":"hello"},
			"creator":{"id":"user-1","name":"Test User","displayName":"test.user"}
		},
		"promptContext":"please refactor the auth module"
	}`)
	sig := SignBody([]byte("test-secret"), body)
	rec := post(t, w, body, sig)
	if rec.Code != http.StatusOK {
		t.Errorf("status: %d", rec.Code)
	}
	waitWithTimeout(t, &wg, time.Second)
	if got.Action != ActionCreated {
		t.Errorf("action: %q", got.Action)
	}
	if got.SessionID != "sess-1" || got.IssueID != "issue-1" {
		t.Errorf("ids: %+v", got)
	}
	if got.IssueIdentifier != "TST-1" || got.IssueTitle != "hello" {
		t.Errorf("issue meta: %+v", got)
	}
	if got.PromptContext != "please refactor the auth module" {
		t.Errorf("promptContext: %q", got.PromptContext)
	}
	if got.CreatorID != "user-1" {
		t.Errorf("creator id: %q", got.CreatorID)
	}
	if got.CreatorName != "Test User" {
		t.Errorf("creator name: %q", got.CreatorName)
	}
	if got.CreatorDisplayName != "test.user" {
		t.Errorf("creator displayName: %q", got.CreatorDisplayName)
	}
}

func TestWebhook_AgentSessionCreated_Assign_EmptyPromptContext(t *testing.T) {
	var (
		wg  sync.WaitGroup
		got AgentEvent
	)
	wg.Add(1)
	w := newTestWebhook(t, func(e AgentEvent) { got = e; wg.Done() })
	body := []byte(`{
		"type":"AgentSessionEvent",
		"action":"created",
		"agentSession":{"id":"sess-2","issue":{"id":"issue-2","identifier":"TST-2","title":"x"}}
	}`)
	sig := SignBody([]byte("test-secret"), body)
	rec := post(t, w, body, sig)
	if rec.Code != http.StatusOK {
		t.Errorf("status: %d", rec.Code)
	}
	waitWithTimeout(t, &wg, time.Second)
	if got.PromptContext != "" {
		t.Errorf("expected empty promptContext on assign, got %q", got.PromptContext)
	}
}

func TestWebhook_AgentSessionPrompted_CarriesUserMessage(t *testing.T) {
	var (
		wg  sync.WaitGroup
		got AgentEvent
	)
	wg.Add(1)
	w := newTestWebhook(t, func(e AgentEvent) { got = e; wg.Done() })
	body := []byte(`{
		"type":"AgentSessionEvent",
		"action":"prompted",
		"agentSession":{"id":"sess-3","issue":{"id":"issue-3","identifier":"TST-3","title":"x"}},
		"agentActivity":{"body":"can you also update the README?"}
	}`)
	sig := SignBody([]byte("test-secret"), body)
	rec := post(t, w, body, sig)
	if rec.Code != http.StatusOK {
		t.Errorf("status: %d", rec.Code)
	}
	waitWithTimeout(t, &wg, time.Second)
	if got.Action != ActionPrompted {
		t.Errorf("action: %q", got.Action)
	}
	if got.UserMessage != "can you also update the README?" {
		t.Errorf("userMessage: %q", got.UserMessage)
	}
}

func TestWebhook_AgentSession_DataEnvelope_Tolerated(t *testing.T) {
	// Some Linear deliveries nest under data.* — agent.ts handles both
	// shapes and so do we.
	var (
		wg  sync.WaitGroup
		got AgentEvent
	)
	wg.Add(1)
	w := newTestWebhook(t, func(e AgentEvent) { got = e; wg.Done() })
	body := []byte(`{
		"type":"AgentSessionEvent",
		"action":"created",
		"data":{
			"agentSession":{"id":"sess-4","issue":{"id":"issue-4","identifier":"TST-4","title":"y"}},
			"promptContext":"do the thing"
		}
	}`)
	sig := SignBody([]byte("test-secret"), body)
	rec := post(t, w, body, sig)
	if rec.Code != http.StatusOK {
		t.Errorf("status: %d", rec.Code)
	}
	waitWithTimeout(t, &wg, time.Second)
	if got.SessionID != "sess-4" || got.PromptContext != "do the thing" {
		t.Errorf("data envelope: %+v", got)
	}
}

func TestWebhook_UnknownAction_Ignored(t *testing.T) {
	called := false
	w := newTestWebhook(t, func(AgentEvent) { called = true })
	body := []byte(`{
		"type":"AgentSessionEvent",
		"action":"weird",
		"agentSession":{"id":"sess-x","issue":{"id":"i","identifier":"I","title":"t"}}
	}`)
	sig := SignBody([]byte("test-secret"), body)
	rec := post(t, w, body, sig)
	if rec.Code != http.StatusOK {
		t.Errorf("status: %d", rec.Code)
	}
	time.Sleep(10 * time.Millisecond)
	if called {
		t.Error("handler invoked on unknown action")
	}
}

func waitWithTimeout(t *testing.T, wg *sync.WaitGroup, d time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatal("timed out waiting for handler")
	}
}

// newMigratedStore opens a real Store (applies migrations) on a temp dir.
// Different from newTestDB which uses NewForTest without migrations.
func newMigratedStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// agentSessionBody returns a valid AgentSessionEvent with the given body
// webhookId. Linear-Delivery is set on the request, not the body — these
// are intentionally distinct so we can verify which one becomes the
// dedup key.
func agentSessionBody(bodyWebhookID, sessionID string) []byte {
	return []byte(`{
		"type":"AgentSessionEvent",
		"action":"created",
		"webhookId":"` + bodyWebhookID + `",
		"agentSession":{
			"id":"` + sessionID + `",
			"issue":{"id":"i","identifier":"TST-1","title":"hello"},
			"creator":{"id":"u"}
		},
		"promptContext":"do the thing"
	}`)
}

func postWithDelivery(t *testing.T, w *Webhook, body []byte, sig, deliveryID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	if sig != "" {
		req.Header.Set("Linear-Signature", sig)
	}
	if deliveryID != "" {
		req.Header.Set("Linear-Delivery", deliveryID)
	}
	rec := httptest.NewRecorder()
	w.Handler().ServeHTTP(rec, req)
	return rec
}

// TestWebhook_DedupesByLinearDeliveryHeader is the regression guard for
// the events_inbox PK bug observed in prod: body.webhookId is the agent
// app's webhook configuration id and stays constant across every event,
// so using it as the dedup key collapsed all events into one row. The
// fix uses Linear-Delivery (unique per delivery attempt).
func TestWebhook_DedupesByLinearDeliveryHeader(t *testing.T) {
	s := newMigratedStore(t)
	sig := make(chan struct{}, 8)
	w := newTestWebhookWithStore(t, s, sig)

	// Two distinct events with SAME body.webhookId (mimicking real Linear
	// behavior) but DIFFERENT Linear-Delivery headers.
	body1 := agentSessionBody("app-config-id-same", "sess-1")
	body2 := agentSessionBody("app-config-id-same", "sess-2")
	sig1 := SignBody([]byte("test-secret"), body1)
	sig2 := SignBody([]byte("test-secret"), body2)

	if rec := postWithDelivery(t, w, body1, sig1, "delivery-1"); rec.Code != http.StatusOK {
		t.Fatalf("post1 status: %d body: %s", rec.Code, rec.Body)
	}
	if rec := postWithDelivery(t, w, body2, sig2, "delivery-2"); rec.Code != http.StatusOK {
		t.Fatalf("post2 status: %d body: %s", rec.Code, rec.Body)
	}

	var n int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM events_inbox`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Errorf("expected 2 rows (distinct Linear-Delivery), got %d — looks like body.webhookId is being used as PK", n)
	}

	// Linear retry: same Linear-Delivery as #1 → must dedup
	if rec := postWithDelivery(t, w, body1, sig1, "delivery-1"); rec.Code != http.StatusOK {
		t.Fatalf("post3 status: %d body: %s", rec.Code, rec.Body)
	}
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM events_inbox`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Errorf("expected 2 rows after Linear-Delivery retry (delivery-1 dedup), got %d", n)
	}

	// Verify the rows used Linear-Delivery as the key, not body webhookId
	var keys []string
	rows, err := s.DB.Query(`SELECT webhook_id FROM events_inbox ORDER BY received_at`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, k)
	}
	rows.Close()
	if len(keys) != 2 || keys[0] != "delivery-1" || keys[1] != "delivery-2" {
		t.Errorf("expected dedup keys [delivery-1 delivery-2], got %v", keys)
	}
}

// TestWebhook_FallbackToBodyWebhookID_WhenHeaderMissing covers test/legacy
// callers that don't set the Linear-Delivery header. Production always
// sees a real header from Linear, but tests + tooling may not.
func TestWebhook_FallbackToBodyWebhookID_WhenHeaderMissing(t *testing.T) {
	s := newMigratedStore(t)
	sig := make(chan struct{}, 8)
	w := newTestWebhookWithStore(t, s, sig)

	body := agentSessionBody("body-id-1", "sess-1")
	sigHeader := SignBody([]byte("test-secret"), body)

	// No Linear-Delivery header → falls back to body.webhookId
	if rec := postWithDelivery(t, w, body, sigHeader, ""); rec.Code != http.StatusOK {
		t.Fatalf("post status: %d body: %s", rec.Code, rec.Body)
	}

	var k string
	if err := s.DB.QueryRow(`SELECT webhook_id FROM events_inbox`).Scan(&k); err != nil {
		t.Fatalf("query: %v", err)
	}
	if k != "body-id-1" {
		t.Errorf("expected fallback to body webhookId 'body-id-1', got %q", k)
	}
}

// Sanity: client compiles + Bearer auth applied.
func TestClient_Compiles(t *testing.T) {
	c := NewClient("https://example.invalid", "tok")
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	_, _ = c.GetIssue(ctx, "x")
	_ = c.PostAgentActivity(ctx, "s", Thought("x", true))
}

func TestBearerHelper(t *testing.T) {
	if got := bearer("lin_oauth_xyz"); got != "Bearer lin_oauth_xyz" {
		t.Errorf("bare token: got %q", got)
	}
	if got := bearer("Bearer already-prefixed"); got != "Bearer already-prefixed" {
		t.Errorf("already prefixed: got %q", got)
	}
}
