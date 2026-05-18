package autopilot

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/georgehuang/admiral/internal/linear"
	"github.com/georgehuang/admiral/internal/store"
)

// BlockerWatcher polls admiral_tasks rows in BLOCKED state and re-queues
// them once all their Linear blocked_by relations are resolved.
type BlockerWatcher struct {
	orch     *Orchestrator
	interval time.Duration
	logger   *slog.Logger
}

func newBlockerWatcher(orch *Orchestrator, interval time.Duration) *BlockerWatcher {
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	return &BlockerWatcher{orch: orch, interval: interval, logger: orch.logger}
}

// Run starts the poll loop. Blocks until ctx is cancelled.
func (w *BlockerWatcher) Run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.checkAll(ctx)
		}
	}
}

func (w *BlockerWatcher) checkAll(ctx context.Context) {
	tasks, err := w.orch.db.GetBlockedAdmiralTasks()
	if err != nil {
		w.logger.Error("blocker_watcher_list_failed", "err", err)
		return
	}
	for _, t := range tasks {
		w.checkOne(ctx, t)
	}
}

func (w *BlockerWatcher) checkOne(ctx context.Context, task store.BlockedTask) {
	blockers, err := w.orch.lc.GetIssueBlockers(ctx, task.IssueID)
	if err != nil {
		w.logger.Warn("blocker_watcher_check_failed",
			"issue", task.IssueIdentifier, "err", err)
		return
	}
	if len(blockers) > 0 {
		return // still blocked
	}

	ok, err := w.orch.db.TransitionBlockedToReceived(task.IssueID)
	if err != nil {
		w.logger.Error("blocker_watcher_transition_failed",
			"issue", task.IssueIdentifier, "err", err)
		return
	}
	if !ok {
		return // another goroutine already transitioned it
	}

	w.logger.Info("blocker_watcher_unblocked", "issue", task.IssueIdentifier)

	if task.LastEventSessionID != "" {
		replyCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		_ = w.orch.lc.PostAgentActivity(replyCtx, task.LastEventSessionID,
			linear.Response("admiral: all blockers resolved — resuming now."))
		cancel()
	}

	syntheticEv := linear.AgentEvent{
		IssueID:         task.IssueID,
		IssueIdentifier: task.IssueIdentifier,
		SessionID:       task.LastEventSessionID,
		Action:          linear.ActionCreated,
	}
	go w.orch.runWithAttempt(syntheticEv, task.AttemptN)
}

// blockerIDsJSON encodes a slice of IssueBlockers as a JSON array of issue IDs.
func blockerIDsJSON(blockers []linear.IssueBlocker) string {
	ids := make([]string, len(blockers))
	for i, b := range blockers {
		ids[i] = b.IssueID
	}
	b, _ := json.Marshal(ids)
	return string(b)
}

// blockerIdentifiers returns a comma-separated list of issue identifiers
// (e.g. "GEO-1, GEO-2") for use in human-readable messages.
func blockerIdentifiers(blockers []linear.IssueBlocker) string {
	names := make([]string, len(blockers))
	for i, b := range blockers {
		names[i] = b.IssueIdentifier
	}
	return strings.Join(names, ", ")
}

func blockedMessage(blockers []linear.IssueBlocker) string {
	return fmt.Sprintf(
		"admiral: not starting — this issue is blocked by %s (unresolved). "+
			"Admiral will retry automatically once all blockers are resolved.",
		blockerIdentifiers(blockers),
	)
}
