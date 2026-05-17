// Package github implements admiral's inbound GitHub webhook surface.
// PR #2 of the GitHub PR review feedback loop landing series: this file
// only receives, validates, filters, and enqueues. The downstream worker
// that consumes events_inbox rows tagged source='github' is added in a
// later PR.
package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
)

// EventEnqueuer is the subset of *store.Store the webhook needs. Decoupled
// so tests can inject a stub without a real DB.
type EventEnqueuer interface {
	EnqueueEventWithSource(source, webhookID, action, sessionID, issueID, payloadJSON, commentID string) (bool, error)
}

// Webhook is the inbound HTTP receiver for GitHub PR review events. It
// validates HMAC, filters to the actionable subset (pull_request_review
// submitted + pull_request_review_comment created), drops events authored
// by the configured bot identity (loop guard), and enqueues into
// events_inbox so the worker can pick them up.
type Webhook struct {
	secret   []byte
	botLogin string
	store    EventEnqueuer
	logger   *slog.Logger
}

// NewWebhook builds a Webhook. secret is the HMAC verification key
// configured on the GitHub side. botLogin is the GitHub login of the
// account admiral itself uses to post PR comments — any event whose
// sender or comment author matches is dropped to avoid self-triggered
// loops. Empty botLogin disables the self-filter (only suitable for
// tests) and emits a startup WARN so the configuration footgun is
// visible at process boot rather than after the first runaway loop.
func NewWebhook(secret, botLogin string, st EventEnqueuer, logger *slog.Logger) *Webhook {
	if botLogin == "" {
		logger.Warn("github_webhook_no_bot_login",
			"detail", "self-filter disabled; admiral comments can re-trigger admiral")
	}
	return &Webhook{
		secret:   []byte(secret),
		botLogin: botLogin,
		store:    st,
		logger:   logger,
	}
}

// Handler returns the http.Handler that admiral mounts at /hooks/github
// (route wiring lives in a later PR).
func (h *Webhook) Handler() http.Handler {
	return http.HandlerFunc(h.serveHTTP)
}

const (
	headerSignature = "X-Hub-Signature-256"
	headerDelivery  = "X-GitHub-Delivery"
	headerEvent     = "X-GitHub-Event"

	eventReview        = "pull_request_review"
	eventReviewComment = "pull_request_review_comment"

	actionSubmitted = "submitted"
	actionCreated   = "created"

	// GitHub caps webhook payloads at 25 MiB; review events are vastly
	// smaller. Pick a generous-but-bounded cap so a malformed sender
	// can't exhaust memory.
	maxBodyBytes = 5 << 20
)

// rawReviewEvent is the subset of GitHub's review-event payloads admiral
// cares about. Both pull_request_review and pull_request_review_comment
// fit this shape because we only look at one of Review / Comment at a
// time depending on event type.
type rawReviewEvent struct {
	Action      string `json:"action"`
	PullRequest struct {
		HTMLURL string `json:"html_url"`
	} `json:"pull_request"`
	Review *struct {
		ID   int64 `json:"id"`
		User struct {
			Login string `json:"login"`
		} `json:"user"`
	} `json:"review,omitempty"`
	Comment *struct {
		ID   int64 `json:"id"`
		User struct {
			Login string `json:"login"`
		} `json:"user"`
	} `json:"comment,omitempty"`
	Sender struct {
		Login string `json:"login"`
	} `json:"sender"`
}

func (h *Webhook) serveHTTP(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(rw, r.Body, maxBodyBytes))
	if err != nil {
		// MaxBytesReader has already flagged the response writer with a
		// 413; calling http.Error here would trip a "superfluous
		// WriteHeader" log. For other read errors (client disconnect,
		// truncated body) 400 is still the right answer.
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			h.logger.Warn("github_webhook_body_too_large",
				"remote", r.RemoteAddr, "limit", maxErr.Limit)
			return
		}
		http.Error(rw, "read body", http.StatusBadRequest)
		return
	}

	if !verifySignature(h.secret, r.Header.Get(headerSignature), body) {
		h.logger.Warn("github_webhook_bad_signature", "remote", r.RemoteAddr)
		http.Error(rw, "bad signature", http.StatusUnauthorized)
		return
	}

	deliveryID := r.Header.Get(headerDelivery)
	eventType := r.Header.Get(headerEvent)

	// Always 200 once the signature passes — GitHub retries on non-2xx
	// and admiral's filtering decisions (wrong event / wrong action /
	// self-loop) are not retry-worthy.
	rw.WriteHeader(http.StatusOK)
	_, _ = rw.Write([]byte("ok"))

	// events_inbox keys on (webhook_id) for primary dedup. An empty
	// delivery id from a real GitHub event would be a protocol violation
	// (the header is always set); if absent we'd collide future empty-id
	// events into the same row. Drop instead of enqueuing junk.
	if deliveryID == "" {
		h.logger.Warn("github_webhook_missing_delivery_header",
			"event", eventType, "remote", r.RemoteAddr)
		return
	}

	if eventType != eventReview && eventType != eventReviewComment {
		h.logger.Debug("github_webhook_skip_event",
			"event", eventType, "delivery", deliveryID)
		return
	}

	var p rawReviewEvent
	if err := json.Unmarshal(body, &p); err != nil {
		h.logger.Warn("github_webhook_bad_payload",
			"err", err, "delivery", deliveryID, "event", eventType)
		return
	}

	switch eventType {
	case eventReview:
		if p.Action != actionSubmitted {
			h.logger.Debug("github_webhook_skip_action",
				"event", eventType, "action", p.Action, "delivery", deliveryID)
			return
		}
	case eventReviewComment:
		if p.Action != actionCreated {
			h.logger.Debug("github_webhook_skip_action",
				"event", eventType, "action", p.Action, "delivery", deliveryID)
			return
		}
	}

	if p.PullRequest.HTMLURL == "" {
		h.logger.Warn("github_webhook_missing_pr_url",
			"event", eventType, "delivery", deliveryID)
		return
	}

	var commentID int64
	var authorLogin string
	switch eventType {
	case eventReview:
		if p.Review == nil {
			h.logger.Warn("github_webhook_missing_review",
				"delivery", deliveryID)
			return
		}
		commentID = p.Review.ID
		authorLogin = p.Review.User.Login
	case eventReviewComment:
		if p.Comment == nil {
			h.logger.Warn("github_webhook_missing_comment",
				"delivery", deliveryID)
			return
		}
		commentID = p.Comment.ID
		authorLogin = p.Comment.User.Login
	}

	// GitHub assigns positive int64 ids; a zero id implies a corrupt or
	// stripped payload. Letting "0" flow into events_inbox.comment_id
	// would poison the partial unique index — any future zero-id event
	// from this source would be silently deduped against this row.
	if commentID == 0 {
		h.logger.Warn("github_webhook_zero_comment_id",
			"event", eventType, "delivery", deliveryID)
		return
	}

	// Loop guard: skip events authored by admiral's own bot identity.
	// Check both the top-level sender and the review/comment author —
	// they usually match but defense in depth costs nothing.
	if h.botLogin != "" && (p.Sender.Login == h.botLogin || authorLogin == h.botLogin) {
		h.logger.Debug("github_webhook_skip_self",
			"bot", h.botLogin,
			"sender", p.Sender.Login,
			"author", authorLogin,
			"delivery", deliveryID)
		return
	}

	// session_id = PR URL so ClaimNextPendingEvent's per-session_id
	// single-flight naturally enforces "one in-flight job per PR".
	// Linear's session_id namespace is UUIDs, structurally distinct from
	// PR URLs, so the source-blind claim logic stays correct.
	sessionID := p.PullRequest.HTMLURL
	action := eventType + "." + p.Action
	commentIDStr := strconv.FormatInt(commentID, 10)

	fresh, err := h.store.EnqueueEventWithSource(
		"github",
		deliveryID,
		action,
		sessionID,
		"", // issue_id is resolved later by the worker via PR URL lookup
		string(body),
		commentIDStr,
	)
	if err != nil {
		h.logger.Error("github_webhook_enqueue_failed",
			"delivery", deliveryID, "err", err)
		return
	}
	if !fresh {
		h.logger.Debug("github_webhook_dedup",
			"delivery", deliveryID, "comment_id", commentIDStr)
		return
	}
	h.logger.Info("github_webhook_enqueued",
		"event", eventType,
		"action", p.Action,
		"pr", sessionID,
		"comment_id", commentIDStr,
		"delivery", deliveryID)
}

// verifySignature decodes GitHub's "sha256=<hex>" header and does a
// constant-time HMAC-SHA256 compare against the body.
func verifySignature(secret []byte, header string, body []byte) bool {
	if len(secret) == 0 || header == "" {
		return false
	}
	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	got, err := hex.DecodeString(header[len(prefix):])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// SignBody is exported for tests / smoke scripts. Returns the value
// GitHub would put in X-Hub-Signature-256 ("sha256=<hex>").
func SignBody(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
