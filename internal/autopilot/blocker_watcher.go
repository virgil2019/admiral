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
	// runFn is called to resume a newly-unblocked task. Defaults to
	// orch.runWithAttempt; overridable in tests.
	runFn func(linear.AgentEvent, int)
}

func newBlockerWatcher(orch *Orchestrator, interval time.Duration) *BlockerWatcher {
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	w := &BlockerWatcher{orch: orch, interval: interval, logger: orch.logger}
	w.runFn = orch.runWithAttempt
	return w
}

// Run starts the poll loop. Blocks until ctx is cancelled. An initial check
// is performed immediately on startup so tasks blocked before a restart are
// not delayed by a full interval.
func (w *BlockerWatcher) Run(ctx context.Context) {
	w.checkAll(ctx)
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
	// Rate-limit: re-queue at most MaxConcurrentRuns tasks per tick so a
	// large backlog doesn't fan out unbounded goroutines at once.
	limit := w.orch.cfg.MaxConcurrentRuns
	if limit <= 0 {
		limit = 3
	}
	requeued := 0
	for _, t := range tasks {
		if requeued >= limit {
			w.logger.Info("blocker_watcher_rate_limited",
				"remaining", len(tasks)-requeued)
			break
		}
		if w.checkOne(ctx, t) {
			requeued++
		}
	}
}

// checkOne re-checks a single BLOCKED task and re-queues it if all blockers
// are resolved. Returns true when the task was successfully re-queued.
func (w *BlockerWatcher) checkOne(ctx context.Context, task store.BlockedTask) bool {
	checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	blockers, err := w.orch.lc.GetIssueBlockers(checkCtx, task.IssueID)
	if err != nil {
		w.logger.Warn("blocker_watcher_check_failed",
			"issue", task.IssueIdentifier, "err", err)
		return false
	}
	if len(blockers) > 0 {
		return false // still blocked
	}

	ok, err := w.orch.db.TransitionBlockedToReceived(task.IssueID)
	if err != nil {
		w.logger.Error("blocker_watcher_transition_failed",
			"issue", task.IssueIdentifier, "err", err)
		return false
	}
	if !ok {
		return false // another goroutine already transitioned it
	}

	// Re-verify after claiming RECEIVED: a new blocker could have been added
	// between GetIssueBlockers and the transition.
	reverifyCtx, rcancel := context.WithTimeout(ctx, 30*time.Second)
	defer rcancel()
	if blockers2, err2 := w.orch.lc.GetIssueBlockers(reverifyCtx, task.IssueID); err2 == nil && len(blockers2) > 0 {
		w.logger.Info("blocker_watcher_reblocked_after_transition",
			"issue", task.IssueIdentifier, "new_blockers", blockerIdentifiers(blockers2))
		_ = w.orch.db.SetAdmiralTaskBlocked(task.IssueID, blockerIDsJSON(blockers2))
		return false
	}

	w.logger.Info("blocker_watcher_unblocked", "issue", task.IssueIdentifier)

	if task.LastEventSessionID != "" {
		replyCtx, replyCancel := context.WithTimeout(ctx, 15*time.Second)
		_ = w.orch.lc.PostAgentActivity(replyCtx, task.LastEventSessionID,
			linear.Response("admiral: all blockers resolved — resuming now."))
		replyCancel()
	}

	syntheticEv := linear.AgentEvent{
		IssueID:         task.IssueID,
		IssueIdentifier: task.IssueIdentifier,
		SessionID:       task.LastEventSessionID,
		Action:          linear.ActionCreated,
	}
	go w.runFn(syntheticEv, task.AttemptN)
	return true
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
