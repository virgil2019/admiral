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
}

// NewWorker creates a Worker that consumes from the events_inbox queue.
func NewWorker(db *store.Store, orch *Orchestrator, logger *slog.Logger, signal <-chan struct{}) *Worker {
	return &Worker{
		db:          db,
		orch:        orch,
		logger:      logger,
		signal:      signal,
		pollEvery:   60 * time.Second,
		maxAttempts: 5,
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

// drain repeatedly claims and dispatches until the queue is empty.
func (w *Worker) drain() {
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

// dispatch processes a single claimed event. It parses the payload JSON,
// calls the orchestrator, and marks the event done (or failed).
func (w *Worker) dispatch(ctx context.Context, row *store.EventInboxRow) {
	var ev linear.AgentEvent
	if err := json.Unmarshal([]byte(row.PayloadJSON), &ev); err != nil {
		w.logger.Error("worker_parse_payload_failed",
			"err", err, "webhook_id", row.WebhookID)
		_ = w.db.MarkEventFailed(row.WebhookID, "parse error: "+err.Error(), false)
		return
	}

	w.logger.Info("worker_dispatch",
		"webhook_id", row.WebhookID,
		"action", row.Action,
		"session", row.SessionID)

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
