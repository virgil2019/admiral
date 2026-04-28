package linear

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func newTestWebhook(t *testing.T, h AgentHandler) *Webhook {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewWebhook("test-secret", h, logger)
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
			"creator":{"id":"user-1"}
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
		t.Errorf("creator: %q", got.CreatorID)
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
