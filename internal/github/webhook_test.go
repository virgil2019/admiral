package github

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubStore captures EnqueueEventWithSource calls so tests can assert on them
// without touching SQLite.
type stubStore struct {
	calls []enqueueCall
	fresh bool
	err   error
}

type enqueueCall struct {
	source, webhookID, action, sessionID, issueID, payloadJSON, commentID string
}

func (s *stubStore) EnqueueEventWithSource(source, webhookID, action, sessionID, issueID, payloadJSON, commentID string) (bool, error) {
	s.calls = append(s.calls, enqueueCall{source, webhookID, action, sessionID, issueID, payloadJSON, commentID})
	if s.err != nil {
		return false, s.err
	}
	return s.fresh, nil
}

func newTestWebhook(t *testing.T, botLogin string) (*Webhook, *stubStore) {
	t.Helper()
	st := &stubStore{fresh: true}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewWebhook("the-secret", botLogin, st, logger), st
}

// signedRequest builds a POST request with a valid HMAC signature so tests
// don't have to repeat the sign + header dance.
func signedRequest(t *testing.T, secret string, eventType, deliveryID string, body []byte) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/hooks/github", bytes.NewReader(body))
	req.Header.Set(headerSignature, SignBody([]byte(secret), body))
	req.Header.Set(headerEvent, eventType)
	if deliveryID != "" {
		req.Header.Set(headerDelivery, deliveryID)
	}
	return req
}

func TestSignBody_VerifyRoundtrip(t *testing.T) {
	secret := []byte("shhh")
	body := []byte(`{"hello":"world"}`)
	sig := SignBody(secret, body)
	if !strings.HasPrefix(sig, "sha256=") {
		t.Fatalf("missing sha256= prefix: %q", sig)
	}
	if !verifySignature(secret, sig, body) {
		t.Errorf("verifySignature: expected true for roundtrip")
	}
}

func TestVerifySignature_TamperedBody(t *testing.T) {
	secret := []byte("shhh")
	sig := SignBody(secret, []byte("original"))
	if verifySignature(secret, sig, []byte("tampered")) {
		t.Error("verifySignature: expected false when body changed")
	}
}

func TestVerifySignature_WrongSecret(t *testing.T) {
	body := []byte("payload")
	sig := SignBody([]byte("secret-a"), body)
	if verifySignature([]byte("secret-b"), sig, body) {
		t.Error("verifySignature: expected false when secret differs")
	}
}

func TestVerifySignature_RejectsMissingPrefix(t *testing.T) {
	secret := []byte("shhh")
	body := []byte("payload")
	sig := SignBody(secret, body)
	naked := strings.TrimPrefix(sig, "sha256=")
	if verifySignature(secret, naked, body) {
		t.Error("verifySignature: expected false for header missing 'sha256=' prefix")
	}
}

func TestServeHTTP_RejectsNonPOST(t *testing.T) {
	wh, st := newTestWebhook(t, "")
	req := httptest.NewRequest(http.MethodGet, "/hooks/github", nil)
	rec := httptest.NewRecorder()
	wh.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status: got %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
	if len(st.calls) != 0 {
		t.Errorf("store should not be called, got %d", len(st.calls))
	}
}

func TestServeHTTP_RejectsBadSignature(t *testing.T) {
	wh, st := newTestWebhook(t, "")
	body := []byte(`{"action":"submitted"}`)
	req := httptest.NewRequest(http.MethodPost, "/hooks/github", bytes.NewReader(body))
	req.Header.Set(headerSignature, "sha256=deadbeef")
	req.Header.Set(headerEvent, eventReview)
	rec := httptest.NewRecorder()
	wh.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if len(st.calls) != 0 {
		t.Errorf("store should not be called on bad sig, got %d", len(st.calls))
	}
}

func TestServeHTTP_RejectsMissingSignature(t *testing.T) {
	wh, _ := newTestWebhook(t, "")
	req := httptest.NewRequest(http.MethodPost, "/hooks/github", bytes.NewReader([]byte(`{}`)))
	req.Header.Set(headerEvent, eventReview)
	rec := httptest.NewRecorder()
	wh.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestServeHTTP_IgnoresOtherEventTypes(t *testing.T) {
	wh, st := newTestWebhook(t, "")
	body := []byte(`{"zen":"keep it simple"}`)
	req := signedRequest(t, "the-secret", "ping", "deliver-ping", body)
	rec := httptest.NewRecorder()
	wh.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", rec.Code)
	}
	if len(st.calls) != 0 {
		t.Errorf("ping events must not enqueue, got %d", len(st.calls))
	}
}

func TestServeHTTP_IgnoresUnwantedReviewActions(t *testing.T) {
	wh, st := newTestWebhook(t, "")
	body := []byte(`{
		"action":"edited",
		"pull_request":{"html_url":"https://github.com/x/y/pull/1","number":1,"state":"open"},
		"review":{"id":42,"user":{"login":"someone"}},
		"sender":{"login":"someone"}
	}`)
	req := signedRequest(t, "the-secret", eventReview, "d-1", body)
	rec := httptest.NewRecorder()
	wh.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", rec.Code)
	}
	if len(st.calls) != 0 {
		t.Errorf("edited review must not enqueue, got %d", len(st.calls))
	}
}

func TestServeHTTP_IgnoresUnwantedReviewCommentActions(t *testing.T) {
	wh, st := newTestWebhook(t, "")
	body := []byte(`{
		"action":"deleted",
		"pull_request":{"html_url":"https://github.com/x/y/pull/1"},
		"comment":{"id":7,"user":{"login":"someone"}},
		"sender":{"login":"someone"}
	}`)
	req := signedRequest(t, "the-secret", eventReviewComment, "d-2", body)
	rec := httptest.NewRecorder()
	wh.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", rec.Code)
	}
	if len(st.calls) != 0 {
		t.Errorf("deleted comment must not enqueue, got %d", len(st.calls))
	}
}

func TestServeHTTP_SkipsBotSender(t *testing.T) {
	wh, st := newTestWebhook(t, "admiral-bot")
	body := []byte(`{
		"action":"submitted",
		"pull_request":{"html_url":"https://github.com/x/y/pull/1"},
		"review":{"id":42,"user":{"login":"someone"}},
		"sender":{"login":"admiral-bot"}
	}`)
	req := signedRequest(t, "the-secret", eventReview, "d-self-sender", body)
	rec := httptest.NewRecorder()
	wh.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", rec.Code)
	}
	if len(st.calls) != 0 {
		t.Errorf("bot sender must not enqueue, got %d", len(st.calls))
	}
}

func TestServeHTTP_SkipsBotReviewAuthor(t *testing.T) {
	wh, st := newTestWebhook(t, "admiral-bot")
	body := []byte(`{
		"action":"submitted",
		"pull_request":{"html_url":"https://github.com/x/y/pull/1"},
		"review":{"id":42,"user":{"login":"admiral-bot"}},
		"sender":{"login":"someone-else"}
	}`)
	req := signedRequest(t, "the-secret", eventReview, "d-self-author", body)
	rec := httptest.NewRecorder()
	wh.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", rec.Code)
	}
	if len(st.calls) != 0 {
		t.Errorf("bot review author must not enqueue, got %d", len(st.calls))
	}
}

func TestServeHTTP_EnqueuesPullRequestReview(t *testing.T) {
	wh, st := newTestWebhook(t, "admiral-bot")
	body := []byte(`{
		"action":"submitted",
		"pull_request":{"html_url":"https://github.com/x/y/pull/42","number":42,"state":"open"},
		"review":{"id":12345,"user":{"login":"reviewer"}},
		"sender":{"login":"reviewer"}
	}`)
	req := signedRequest(t, "the-secret", eventReview, "deliver-42", body)
	rec := httptest.NewRecorder()
	wh.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if len(st.calls) != 1 {
		t.Fatalf("enqueue calls: got %d, want 1", len(st.calls))
	}
	c := st.calls[0]
	if c.source != "github" {
		t.Errorf("source: got %q, want 'github'", c.source)
	}
	if c.webhookID != "deliver-42" {
		t.Errorf("webhookID: got %q, want 'deliver-42'", c.webhookID)
	}
	if c.action != "pull_request_review.submitted" {
		t.Errorf("action: got %q, want 'pull_request_review.submitted'", c.action)
	}
	if c.sessionID != "https://github.com/x/y/pull/42" {
		t.Errorf("sessionID: got %q", c.sessionID)
	}
	if c.commentID != "12345" {
		t.Errorf("commentID: got %q, want '12345'", c.commentID)
	}
	if c.payloadJSON != string(body) {
		t.Errorf("payloadJSON not preserved verbatim")
	}
}

func TestServeHTTP_EnqueuesPullRequestReviewComment(t *testing.T) {
	wh, st := newTestWebhook(t, "admiral-bot")
	body := []byte(`{
		"action":"created",
		"pull_request":{"html_url":"https://github.com/x/y/pull/7"},
		"comment":{"id":9999,"user":{"login":"nitpicker"}},
		"sender":{"login":"nitpicker"}
	}`)
	req := signedRequest(t, "the-secret", eventReviewComment, "deliver-7", body)
	rec := httptest.NewRecorder()
	wh.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if len(st.calls) != 1 {
		t.Fatalf("enqueue calls: got %d, want 1", len(st.calls))
	}
	c := st.calls[0]
	if c.action != "pull_request_review_comment.created" {
		t.Errorf("action: got %q, want 'pull_request_review_comment.created'", c.action)
	}
	if c.commentID != "9999" {
		t.Errorf("commentID: got %q, want '9999'", c.commentID)
	}
}

func TestServeHTTP_IgnoresBadJSONAfterSignaturePasses(t *testing.T) {
	wh, st := newTestWebhook(t, "")
	body := []byte(`not-json-at-all`)
	req := signedRequest(t, "the-secret", eventReview, "d-bad", body)
	rec := httptest.NewRecorder()
	wh.Handler().ServeHTTP(rec, req)

	// 200 is intentional — a bad-JSON body with a valid signature is
	// almost certainly an admiral-side parser bug, not GitHub trying to
	// redeliver. Returning 200 prevents GitHub retry storms.
	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", rec.Code)
	}
	if len(st.calls) != 0 {
		t.Errorf("bad json must not enqueue, got %d", len(st.calls))
	}
}

func TestServeHTTP_SkipsMissingPRURL(t *testing.T) {
	wh, st := newTestWebhook(t, "")
	body := []byte(`{
		"action":"submitted",
		"pull_request":{},
		"review":{"id":1,"user":{"login":"x"}},
		"sender":{"login":"x"}
	}`)
	req := signedRequest(t, "the-secret", eventReview, "d-no-pr", body)
	rec := httptest.NewRecorder()
	wh.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", rec.Code)
	}
	if len(st.calls) != 0 {
		t.Errorf("missing PR URL must not enqueue, got %d", len(st.calls))
	}
}

func TestServeHTTP_DedupOnRepeatedDelivery(t *testing.T) {
	wh, st := newTestWebhook(t, "")
	st.fresh = false // store reports "already enqueued"
	body := []byte(`{
		"action":"submitted",
		"pull_request":{"html_url":"https://github.com/x/y/pull/1"},
		"review":{"id":42,"user":{"login":"r"}},
		"sender":{"login":"r"}
	}`)
	req := signedRequest(t, "the-secret", eventReview, "d-dup", body)
	rec := httptest.NewRecorder()
	wh.Handler().ServeHTTP(rec, req)

	// Dedup is silent — the store call still happens (that's how we
	// discover the dup); the response is still 200.
	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", rec.Code)
	}
	if len(st.calls) != 1 {
		t.Errorf("store should be called exactly once on dup, got %d", len(st.calls))
	}
}

func TestServeHTTP_SkipsMissingDeliveryHeader(t *testing.T) {
	wh, st := newTestWebhook(t, "")
	body := []byte(`{
		"action":"submitted",
		"pull_request":{"html_url":"https://github.com/x/y/pull/1"},
		"review":{"id":1,"user":{"login":"r"}},
		"sender":{"login":"r"}
	}`)
	// Build signed request manually so we can omit X-GitHub-Delivery.
	req := httptest.NewRequest(http.MethodPost, "/hooks/github", bytes.NewReader(body))
	req.Header.Set(headerSignature, SignBody([]byte("the-secret"), body))
	req.Header.Set(headerEvent, eventReview)
	rec := httptest.NewRecorder()
	wh.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", rec.Code)
	}
	if len(st.calls) != 0 {
		t.Errorf("missing delivery header must skip enqueue, got %d", len(st.calls))
	}
}

func TestServeHTTP_SkipsZeroCommentID(t *testing.T) {
	wh, st := newTestWebhook(t, "")
	body := []byte(`{
		"action":"submitted",
		"pull_request":{"html_url":"https://github.com/x/y/pull/1"},
		"review":{"id":0,"user":{"login":"r"}},
		"sender":{"login":"r"}
	}`)
	req := signedRequest(t, "the-secret", eventReview, "d-zero", body)
	rec := httptest.NewRecorder()
	wh.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", rec.Code)
	}
	if len(st.calls) != 0 {
		t.Errorf("zero comment_id must skip enqueue to protect partial unique index, got %d", len(st.calls))
	}
}

func TestServeHTTP_StoreErrorIsLoggedNotPropagated(t *testing.T) {
	wh, st := newTestWebhook(t, "")
	st.err = errors.New("db down")
	body := []byte(`{
		"action":"submitted",
		"pull_request":{"html_url":"https://github.com/x/y/pull/1"},
		"review":{"id":42,"user":{"login":"r"}},
		"sender":{"login":"r"}
	}`)
	req := signedRequest(t, "the-secret", eventReview, "d-err", body)
	rec := httptest.NewRecorder()
	wh.Handler().ServeHTTP(rec, req)

	// 200 was already written before the enqueue attempt, so the store
	// error doesn't propagate to the response. The error is logged for
	// alerting; GitHub will not retry. This matches Linear's behavior.
	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", rec.Code)
	}
	if len(st.calls) != 1 {
		t.Errorf("store should be called once even when it errors, got %d", len(st.calls))
	}
}
