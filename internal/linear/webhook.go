package linear

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"

	"github.com/georgehuang/admiral/internal/store"
)

// AgentEventAction is the verb on an AgentSessionEvent: "created" when an
// agent session is opened (assign / @mention / explicit delegate); "prompted"
// when the user posts a follow-up message inside an existing thread.
type AgentEventAction string

const (
	ActionCreated  AgentEventAction = "created"
	ActionPrompted AgentEventAction = "prompted"
)

// AgentEvent is the parsed shape the orchestrator consumes. SessionID is
// the durable handle for `agentActivityCreate` calls; IssueID is what
// GetIssue takes; PromptContext / UserMessage carry whatever the user
// said (depending on action).
type AgentEvent struct {
	Action          AgentEventAction
	SessionID       string
	IssueID         string
	IssueIdentifier string
	IssueTitle      string
	// PromptContext is set on action=created. For @mention triggers it's the
	// comment text; for raw assignment it's empty; for delegate-to-agent
	// it's whatever the user typed in the prompt input. Use SourceCommentID
	// (not text emptiness) to distinguish @mention from delegate.
	PromptContext string
	// SourceCommentID is set on action=created when the AgentSession was
	// opened via an @mention inside an existing comment. Empty when the
	// session was opened via delegation (assign-to-agent), regardless of
	// whether the user typed an initial prompt. This is the protocol-level
	// signal admiral uses to disambiguate the two trigger paths.
	SourceCommentID string
	// UserMessage is set on action=prompted: the user's follow-up text in
	// the agent thread.
	UserMessage string
	CreatorID   string
	// CreatorName is the creator's full display name (e.g. "George Huang").
	// Set when Linear includes it in the webhook payload; empty otherwise.
	CreatorName string
	// CreatorDisplayName is the creator's Linear handle (e.g. "george.huang"),
	// which is what `@<handle>` mentions in markdown require to actually
	// notify the user. Falls back to CreatorName, then "" — see flow.creatorMention.
	CreatorDisplayName string
	WebhookID          string
}

// AgentHandler is invoked once per accepted (signature-verified, recognized
// shape) AgentSessionEvent. Implementations should not block — kick off
// async work and return.
type AgentHandler func(AgentEvent)

// rawAgentSessionPayload is the subset of Linear's webhook envelope we
// care about. Linear sends many more fields; unknown ones are ignored.
type rawAgentSessionPayload struct {
	Type      string `json:"type"`
	Action    string `json:"action"`
	WebhookID string `json:"webhookId"`

	// Linear has been observed to deliver the session under either the
	// top-level `agentSession` key or a `data.agentSession` envelope —
	// mirror agent.ts which checks both.
	AgentSession *rawAgentSession `json:"agentSession"`
	Data         *struct {
		AgentSession  *rawAgentSession  `json:"agentSession"`
		AgentActivity *rawAgentActivity `json:"agentActivity"`
		PromptContext string            `json:"promptContext"`
	} `json:"data"`

	PromptContext string            `json:"promptContext"`
	AgentActivity *rawAgentActivity `json:"agentActivity"`
}

type rawAgentSession struct {
	ID    string `json:"id"`
	Issue *struct {
		ID         string `json:"id"`
		Identifier string `json:"identifier"`
		Title      string `json:"title"`
	} `json:"issue"`
	Creator *struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		DisplayName string `json:"displayName"`
	} `json:"creator"`
	// SourceCommentID is set when the session was opened from an @mention
	// inside a comment. Empty on delegate triggers — this is what admiral
	// uses to tell the two paths apart.
	SourceCommentID string `json:"sourceCommentId"`
}

type rawAgentActivity struct {
	Body string `json:"body"`
}

// Webhook is the HTTP receiver. Mount Handler at /webhook (matches Linear's
// agent webhook URL convention) — main.go also exposes /linear/webhook as
// an alias.
type Webhook struct {
	secret  []byte
	onAgent AgentHandler // retained for test compatibility; production uses store enqueue path
	logger  *slog.Logger
	store   *store.Store
	signal  chan<- struct{}
}

// NewWebhook constructs a receiver. Unlike v0.3 method-A, no admiral user
// UUID is needed: AgentSessionEvent is already routed by Linear to the
// specific agent app behind this webhook, so the filtering Linear does
// for us replaces the per-user check.
//
// The store+signal parameters enable the at-least-once queue path: events
// are persisted to events_inbox rather than processed synchronously.
// The onAgent callback is retained for test compatibility.
func NewWebhook(secret string, store *store.Store, signal chan<- struct{}, logger *slog.Logger, onAgent AgentHandler) *Webhook {
	return &Webhook{
		secret:  []byte(secret),
		onAgent: onAgent,
		logger:  logger,
		store:   store,
		signal:  signal,
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
	// Linear-Delivery header is unique per delivery attempt — the right key
	// for events_inbox dedup. Body's webhookId is the agent app's webhook
	// configuration id and stays the same across all events from one OAuth
	// app, so it would collapse every event into one row.
	deliveryID := r.Header.Get("Linear-Delivery")
	sig := r.Header.Get("Linear-Signature")
	if !verifySignature(w.secret, sig, body) {
		w.logger.Warn("linear_webhook_bad_signature", "remote", r.RemoteAddr)
		http.Error(rw, "bad signature", http.StatusUnauthorized)
		return
	}

	var p rawAgentSessionPayload
	if err := json.Unmarshal(body, &p); err != nil {
		w.logger.Warn("linear_webhook_bad_payload", "err", err)
		http.Error(rw, "bad payload", http.StatusBadRequest)
		return
	}

	// Always 200 once signature passes — Linear retries on non-2xx, and
	// our internal filtering decisions (wrong type / unknown action) are
	// not retry-worthy.
	rw.WriteHeader(http.StatusOK)
	_, _ = rw.Write([]byte("ok"))

	if p.Type != "AgentSessionEvent" {
		w.logger.Debug("linear_webhook_skip_non_agent",
			"type", p.Type, "action", p.Action)
		return
	}
	action := AgentEventAction(p.Action)
	if action != ActionCreated && action != ActionPrompted {
		w.logger.Debug("linear_webhook_skip_unknown_action",
			"action", p.Action)
		return
	}

	session := p.AgentSession
	if session == nil && p.Data != nil {
		session = p.Data.AgentSession
	}
	if session == nil || session.ID == "" {
		w.logger.Warn("linear_webhook_missing_session", "action", p.Action)
		return
	}

	// Prefer the Linear-Delivery header (unique per delivery attempt) for
	// the dedup key. Fall back to body.webhookId only when the header is
	// absent (test fixtures / non-Linear callers); admiral's prod path
	// always sees a real header from Linear.
	dedupKey := deliveryID
	if dedupKey == "" {
		dedupKey = p.WebhookID
	}

	ev := AgentEvent{
		Action:    action,
		SessionID: session.ID,
		WebhookID: dedupKey,
	}
	if session.Issue != nil {
		ev.IssueID = session.Issue.ID
		ev.IssueIdentifier = session.Issue.Identifier
		ev.IssueTitle = session.Issue.Title
	}
	if session.Creator != nil {
		ev.CreatorID = session.Creator.ID
		ev.CreatorName = session.Creator.Name
		ev.CreatorDisplayName = session.Creator.DisplayName
	}
	ev.SourceCommentID = session.SourceCommentID
	switch action {
	case ActionCreated:
		ev.PromptContext = firstNonEmpty(p.PromptContext, dataPromptContext(p))
	case ActionPrompted:
		if p.AgentActivity != nil {
			ev.UserMessage = p.AgentActivity.Body
		} else if p.Data != nil && p.Data.AgentActivity != nil {
			ev.UserMessage = p.Data.AgentActivity.Body
		}
	}

	if w.store == nil && w.onAgent == nil {
		return
	}

	payload, _ := json.Marshal(ev)

	if w.store != nil {
		// at-least-once queue path: enqueue to SQLite and signal worker
		fresh, err := w.store.EnqueueEvent(dedupKey, string(action), session.ID, ev.IssueID, string(payload))
		if err != nil {
			w.logger.Error("enqueue_event_failed", "err", err, "webhook_id", dedupKey)
			// Still return 200 — Linear shouldn't retry; the error is ours
			return
		}
		if fresh {
			w.logger.Info("event_enqueued",
				"webhook_id", dedupKey,
				"action", string(action),
				"session", session.ID)
			select {
			case w.signal <- struct{}{}:
			default:
				// non-blocking: worker has 60s pollEvery as fallback
			}
		} else {
			w.logger.Debug("event_duplicate_dropped", "webhook_id", dedupKey)
		}
		return
	}

	// Legacy synchronous path (tests only, when store is nil but onAgent is set)
	w.logger.Info("linear_agent_event",
		"action", string(action),
		"session", ev.SessionID,
		"issue", ev.IssueIdentifier)
	go w.onAgent(ev)
}

func dataPromptContext(p rawAgentSessionPayload) string {
	if p.Data == nil {
		return ""
	}
	return p.Data.PromptContext
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// verifySignature is constant-time HMAC-SHA256 hex compare against the
// Linear-Signature header.
func verifySignature(secret []byte, headerSig string, body []byte) bool {
	if len(secret) == 0 || headerSig == "" {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	expected := mac.Sum(nil)
	got, err := hex.DecodeString(headerSig)
	if err != nil {
		return false
	}
	return hmac.Equal(got, expected)
}

// SignBody is exported for tests / smoke scripts.
func SignBody(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
