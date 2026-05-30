package autopilot

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/georgehuang/admiral/internal/linear"
	"github.com/georgehuang/admiral/internal/store"
)

// Worker pulls events from the events_inbox queue and dispatches them to
// the orchestrator. Single-instance for now; #16 will scale to N workers.
type Worker struct {
	db          *store.Store
	orch        *Orchestrator
	logger      *slog.Logger
	signal      <-chan struct{}
	pollEvery   time.Duration
	maxAttempts int
	// AuthAlerter, when non-nil, is invoked once per broken-auth window to
	// surface the failure to the user (e.g. via Telegram). If it returns
	// nil, the worker stamps notified_at to dedupe; otherwise the alert
	// is retried on the next tick.
	AuthAlerter func(reason string) error
	// AlertReNotifyAfter is the minimum gap between repeat alerts when the
	// breaker stays open. Zero disables re-alerting (one alert per outage).
	AlertReNotifyAfter time.Duration
}

// NewWorker creates a Worker that consumes from the events_inbox queue.
func NewWorker(db *store.Store, orch *Orchestrator, logger *slog.Logger, signal <-chan struct{}) *Worker {
	return &Worker{
		db:                 db,
		orch:               orch,
		logger:             logger,
		signal:             signal,
		pollEvery:          60 * time.Second,
		maxAttempts:        5,
		AlertReNotifyAfter: 6 * time.Hour,
	}
}

// Run loops until ctx is done. On entry it drains any leftover pending
// events before starting the main loop.
func (w *Worker) Run(ctx context.Context) {
	w.logger.Info("worker_starting")

	// Drain backlog on startup — natural recovery after restart
	w.drain()

	for {
		select {
		case <-ctx.Done():
			w.logger.Info("worker_stopping", "reason", "context_done")
			return
		case <-w.signal:
			// New event enqueued; drain until empty
			w.drain()
		case <-time.After(w.pollEvery):
			// Fallback polling; also catches missed signals
			w.drain()
		}
	}
}

// drain repeatedly claims and dispatches until the queue is empty. If the
// OAuth circuit breaker is open it short-circuits without touching the
// queue — events stay pending and naturally drain once the user re-OAuths
// and ClearAuthError fires.
func (w *Worker) drain() {
	if w.authBroken() {
		return
	}
	for {
		row, err := w.db.ClaimNextPendingEvent()
		if err != nil {
			w.logger.Error("worker_claim_failed", "err", err)
			break
		}
		if row == nil {
			// Queue is empty
			break
		}
		w.dispatch(context.Background(), row)
	}
}

// authBroken reports whether the Linear OAuth circuit breaker is open. When
// it is, the worker skips the queue entirely and (best-effort) fires a
// user-facing alert deduped via linear_oauth.notified_at.
func (w *Worker) authBroken() bool {
	st, err := w.db.GetAuthError()
	if err != nil {
		w.logger.Warn("worker_auth_state_check_failed", "err", err)
		return false // fail-open: rather process events than block on a check
	}
	if st.Reason == "" {
		return false
	}
	w.logger.Warn("worker_auth_broken_skip_drain",
		"reason", st.Reason, "since", st.ErrAt)
	w.maybeAlert(st)
	return true
}

// maybeAlert sends the user-facing alert if (a) one hasn't been sent yet,
// or (b) the previous alert is older than AlertReNotifyAfter. Either path
// stamps notified_at on success so we don't spam.
func (w *Worker) maybeAlert(st store.AuthErrorState) {
	if w.AuthAlerter == nil {
		return
	}
	now := time.Now().UTC()
	if st.NotifiedAt != "" {
		if w.AlertReNotifyAfter <= 0 {
			return
		}
		last, err := time.Parse(time.RFC3339, st.NotifiedAt)
		if err == nil && now.Sub(last) < w.AlertReNotifyAfter {
			return
		}
	}
	if err := w.AuthAlerter(st.Reason); err != nil {
		w.logger.Warn("worker_auth_alert_send_failed", "err", err)
		return
	}
	if err := w.db.MarkAuthNotified(now.Format(time.RFC3339)); err != nil {
		w.logger.Warn("worker_auth_notified_persist_failed", "err", err)
	} else {
		w.logger.Info("worker_auth_alert_sent", "reason", st.Reason)
	}
}

// dispatch processes a single claimed event. Routes on row.Source:
// "github" events go to HandleReviewEvent; everything else is treated as a
// Linear AgentEvent and forwarded to HandleAgentEvent.
func (w *Worker) dispatch(ctx context.Context, row *store.EventInboxRow) {
	w.logger.Info("worker_dispatch",
		"webhook_id", row.WebhookID,
		"source", row.Source,
		"action", row.Action,
		"session", row.SessionID)

	if row.Source == "github" {
		w.dispatchReview(ctx, row)
		return
	}

	if row.Source == "verify" {
		w.dispatchVerify(ctx, row)
		return
	}

	var ev linear.AgentEvent
	if err := json.Unmarshal([]byte(row.PayloadJSON), &ev); err != nil {
		w.logger.Error("worker_parse_payload_failed",
			"err", err, "webhook_id", row.WebhookID)
		_ = w.db.MarkEventFailed(row.WebhookID, "parse error: "+err.Error(), false)
		return
	}

	// Dispatch is synchronous: HandleAgentEvent returns once the orchestrator
	// has started its background goroutine (or rejected due to busy state).
	// We intentionally do NOT wait for the full task to complete — the
	// autopilot_jobs table tracks task lifecycle separately.
	defer func() {
		if r := recover(); r != nil {
			w.logger.Error("worker_panic", "panic", r, "webhook_id", row.WebhookID)
			retryAvailable := row.Attempts < w.maxAttempts
			_ = w.db.MarkEventFailed(row.WebhookID, panicToString(r), retryAvailable)
		}
	}()

	w.orch.HandleAgentEvent(ev)

	// Mark done immediately after HandleAgentEvent returns (even if the
	// background goroutine is still running). The orchestrator owns the
	// autopilot_jobs lifecycle; events_inbox only tracks delivery.
	if err := w.db.MarkEventDone(row.WebhookID); err != nil {
		w.logger.Error("worker_mark_done_failed", "err", err, "webhook_id", row.WebhookID)
	}
}

// dispatchVerify handles a source='verify' event from the discoverer: the
// row's session_id carries the parent issue id of a task whose sub-issues
// have all reached completed. HandleVerifyEvent returns quickly (guard is
// synchronous, the judge run is on a background goroutine), so the event is
// marked done immediately after the call — events_inbox tracks delivery only,
// while the task_verifications row tracks loop state.
func (w *Worker) dispatchVerify(ctx context.Context, row *store.EventInboxRow) {
	defer func() {
		if r := recover(); r != nil {
			w.logger.Error("worker_panic", "panic", r, "webhook_id", row.WebhookID)
			_ = w.db.MarkEventFailed(row.WebhookID, panicToString(r), false)
		}
	}()
	w.orch.HandleVerifyEvent(ctx, row.SessionID)
	if err := w.db.MarkEventDone(row.WebhookID); err != nil {
		w.logger.Error("worker_mark_done_failed", "err", err, "webhook_id", row.WebhookID)
	}
}

// dispatchReview handles a source='github' event. HandleReviewEvent returns
// quickly (the claude run is on a background goroutine), so the event is
// marked done immediately after the call.
func (w *Worker) dispatchReview(ctx context.Context, row *store.EventInboxRow) {
	defer func() {
		if r := recover(); r != nil {
			w.logger.Error("worker_panic", "panic", r, "webhook_id", row.WebhookID)
			_ = w.db.MarkEventFailed(row.WebhookID, panicToString(r), false)
		}
	}()
	w.orch.HandleReviewEvent(ctx, row)
	if err := w.db.MarkEventDone(row.WebhookID); err != nil {
		w.logger.Error("worker_mark_done_failed", "err", err, "webhook_id", row.WebhookID)
	}
}

func panicToString(r any) string {
	switch v := r.(type) {
	case string:
		return v
	case error:
		return v.Error()
	default:
		return "unknown panic"
	}
}
