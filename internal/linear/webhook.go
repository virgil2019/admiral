package linear

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
)

// AssignmentEvent is what the webhook handler hands off to the orchestrator
// when an Issue.update event flips the assignee to the configured admiral
// user.
type AssignmentEvent struct {
	IssueID         string
	IssueIdentifier string
	IssueTitle      string
	IssueBody       string
	IssueURL        string
	AssigneeID      string
	WebhookID       string
}

// AssignmentHandler receives an event whose webhook signature has already been
// verified and which has already been filtered down to "assigned to admiral".
// Implementations should NOT block — kick off async work and return.
type AssignmentHandler func(AssignmentEvent)

// rawWebhookPayload is the subset of the Linear webhook envelope we care
// about. Linear sends many more fields; unknown ones are ignored.
type rawWebhookPayload struct {
	Action      string          `json:"action"`
	Type        string          `json:"type"`
	Data        json.RawMessage `json:"data"`
	UpdatedFrom json.RawMessage `json:"updatedFrom"`
	WebhookID   string          `json:"webhookId"`
}

type rawIssueData struct {
	ID         string  `json:"id"`
	Identifier string  `json:"identifier"`
	Title      string  `json:"title"`
	Body       string  `json:"description"`
	URL        string  `json:"url"`
	AssigneeID *string `json:"assigneeId"`
	Assignee   *struct {
		ID string `json:"id"`
	} `json:"assignee"`
}

// Webhook is the HTTP receiver. Mount Handler at /linear/webhook (or
// wherever).
type Webhook struct {
	secret        []byte
	admiralUserID string
	onAssign      AssignmentHandler
	logger        *slog.Logger
}

func NewWebhook(secret, admiralUserID string, onAssign AssignmentHandler, logger *slog.Logger) *Webhook {
	return &Webhook{
		secret:        []byte(secret),
		admiralUserID: admiralUserID,
		onAssign:      onAssign,
		logger:        logger,
	}
}

func (w *Webhook) Handler() http.Handler {
	return http.HandlerFunc(w.serveHTTP)
}

func (w *Webhook) serveHTTP(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(rw, r.Body, 5<<20))
	if err != nil {
		http.Error(rw, "read body", http.StatusBadRequest)
		return
	}
	sig := r.Header.Get("Linear-Signature")
	if !verifySignature(w.secret, sig, body) {
		w.logger.Warn("linear_webhook_bad_signature", "remote", r.RemoteAddr)
		http.Error(rw, "bad signature", http.StatusUnauthorized)
		return
	}

	var p rawWebhookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		w.logger.Warn("linear_webhook_bad_payload", "err", err)
		http.Error(rw, "bad payload", http.StatusBadRequest)
		return
	}

	// Linear delivers many event types; we only act on Issue.update with
	// an assignee change to admiral. Everything else: 200 OK, ignored.
	rw.WriteHeader(http.StatusOK)
	_, _ = rw.Write([]byte("ok"))

	if p.Type != "Issue" || p.Action != "update" {
		return
	}
	var issue rawIssueData
	if err := json.Unmarshal(p.Data, &issue); err != nil {
		w.logger.Warn("linear_webhook_issue_decode", "err", err)
		return
	}
	// Linear's `updatedFrom` only includes keys that actually changed, so
	// the literal presence of `assigneeId` is the gate — its value can be
	// null when the previous assignee was unset.
	if len(p.UpdatedFrom) == 0 {
		return
	}
	var diff map[string]json.RawMessage
	_ = json.Unmarshal(p.UpdatedFrom, &diff)
	if _, hadAssigneeKey := diff["assigneeId"]; !hadAssigneeKey {
		return
	}

	newAssignee := assigneeID(issue)
	if newAssignee != w.admiralUserID {
		return
	}
	if w.onAssign == nil {
		return
	}
	w.logger.Info("linear_assigned",
		"issue_id", issue.ID,
		"identifier", issue.Identifier,
		"title", issue.Title)
	go w.onAssign(AssignmentEvent{
		IssueID:         issue.ID,
		IssueIdentifier: issue.Identifier,
		IssueTitle:      issue.Title,
		IssueBody:       issue.Body,
		IssueURL:        issue.URL,
		AssigneeID:      newAssignee,
		WebhookID:       p.WebhookID,
	})
}

func assigneeID(i rawIssueData) string {
	if i.AssigneeID != nil {
		return *i.AssigneeID
	}
	if i.Assignee != nil {
		return i.Assignee.ID
	}
	return ""
}

// verifySignature is constant-time HMAC-SHA256 hex compare against the
// Linear-Signature header. Returns false if header is empty/malformed.
func verifySignature(secret []byte, headerSig string, body []byte) bool {
	if len(secret) == 0 || headerSig == "" {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	// hmac.Equal needs equal-length slices; hex strings of the same content
	// are guaranteed same length.
	got, err := hex.DecodeString(headerSig)
	if err != nil {
		return false
	}
	exp, _ := hex.DecodeString(expected)
	return hmac.Equal(got, exp)
}

// SignBody is exported for tests / smoke scripts: produces the same hex digest
// Linear would send, given the same secret.
func SignBody(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

