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

func newTestWebhook(t *testing.T, admiralID string, h AssignmentHandler) *Webhook {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewWebhook("test-secret", admiralID, h, logger)
}

func post(t *testing.T, w *Webhook, body []byte, sig string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/linear/webhook", bytes.NewReader(body))
	if sig != "" {
		req.Header.Set("Linear-Signature", sig)
	}
	rec := httptest.NewRecorder()
	w.Handler().ServeHTTP(rec, req)
	return rec
}

func TestWebhook_BadSignature_Rejects(t *testing.T) {
	w := newTestWebhook(t, "admiral-uuid", nil)
	rec := post(t, w, []byte(`{}`), "deadbeef")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401", rec.Code)
	}
}

func TestWebhook_NonIssueEvent_Ignored(t *testing.T) {
	called := false
	w := newTestWebhook(t, "admiral-uuid", func(AssignmentEvent) { called = true })
	body := []byte(`{"action":"create","type":"Comment","data":{},"updatedFrom":{}}`)
	sig := SignBody([]byte("test-secret"), body)
	rec := post(t, w, body, sig)
	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", rec.Code)
	}
	if called {
		t.Error("handler invoked on non-Issue event")
	}
}

func TestWebhook_IssueUpdate_NotAssignedToAdmiral_Ignored(t *testing.T) {
	called := false
	w := newTestWebhook(t, "admiral-uuid", func(AssignmentEvent) { called = true })
	body := []byte(`{
		"action":"update","type":"Issue",
		"data":{"id":"issue-1","identifier":"TST-1","title":"x","description":"y","url":"u","assigneeId":"someone-else"},
		"updatedFrom":{"assigneeId":null}
	}`)
	sig := SignBody([]byte("test-secret"), body)
	rec := post(t, w, body, sig)
	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d", rec.Code)
	}
	// onAssign is async; give it a tick to mis-fire.
	time.Sleep(10 * time.Millisecond)
	if called {
		t.Error("handler invoked when assignee != admiral")
	}
}

func TestWebhook_IssueUpdate_NoAssigneeChange_Ignored(t *testing.T) {
	called := false
	w := newTestWebhook(t, "admiral-uuid", func(AssignmentEvent) { called = true })
	// title changed but assignee did not — updatedFrom must NOT contain assigneeId.
	body := []byte(`{
		"action":"update","type":"Issue",
		"data":{"id":"issue-1","identifier":"TST-1","title":"new","description":"y","url":"u","assigneeId":"admiral-uuid"},
		"updatedFrom":{"title":"old"}
	}`)
	sig := SignBody([]byte("test-secret"), body)
	rec := post(t, w, body, sig)
	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d", rec.Code)
	}
	time.Sleep(10 * time.Millisecond)
	if called {
		t.Error("handler invoked when assignee field unchanged")
	}
}

func TestWebhook_IssueUpdate_AssignedToAdmiral_Fires(t *testing.T) {
	var (
		wg  sync.WaitGroup
		got AssignmentEvent
	)
	wg.Add(1)
	w := newTestWebhook(t, "admiral-uuid", func(e AssignmentEvent) {
		got = e
		wg.Done()
	})
	body := []byte(`{
		"action":"update","type":"Issue",
		"data":{"id":"issue-1","identifier":"TST-1","title":"hello","description":"do the thing","url":"https://linear.app/.../TST-1","assigneeId":"admiral-uuid"},
		"updatedFrom":{"assigneeId":null}
	}`)
	sig := SignBody([]byte("test-secret"), body)
	rec := post(t, w, body, sig)
	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d", rec.Code)
	}
	waitWithTimeout(t, &wg, time.Second)
	if got.IssueID != "issue-1" || got.IssueIdentifier != "TST-1" {
		t.Errorf("event: %+v", got)
	}
	if got.IssueTitle != "hello" || got.IssueBody != "do the thing" {
		t.Errorf("event body: %+v", got)
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

// Sanity: the GraphQL client compiles with a context arg. (No live network.)
func TestClient_Compiles(t *testing.T) {
	c := NewClient("https://example.invalid", "tok")
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	_, _ = c.GetIssue(ctx, "x")
}
