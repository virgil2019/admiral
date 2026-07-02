package discoverer

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/georgehuang/admiral/internal/store"
)

// RescanReport is the per-table tally returned by RescanStuckVerifications.
// Scanned counts every row matching the stuck shape; ReEnqueued counts
// those that had a fresh events_inbox row inserted by this call;
// SkippedInFlight counts rows where an in-flight verify event was
// already queued (so the worker would have processed it).
type RescanReport struct {
	TaskScanned         int
	TaskReEnqueued      int
	TaskSkippedInFlight int
	ProductScanned      int
	ProductReEnqueued   int
	ProductSkippedInFlight int
}

// rescan constants. Centralised so the test fixtures and the boot-time
// wire-in cannot drift on strings.
const (
	rescanTaskSource    = "verify"
	rescanProductSource = "product-verify"

	rescanTaskAction    = "verify.task_complete"
	rescanProductAction = "product_verify.task_complete"
)

// RescanStuckVerifications is a one-shot boot-time recovery for verify
// rows left behind by historical failures (the canonical case is GEO-267,
// where runClaudeForVerify failed before claude even started because the
// prompt exceeded Linux's argv size cap — PR #187 fixed the bug; this
// recovers the historical stuck rows).
//
// The function walks two tables:
//   - task_verifications   : status='active' AND rounds=1 AND summary=''
//   - product_verifications: status='active' AND rounds=1
//
// For each row it checks whether an in-flight verify event already
// exists in events_inbox (status IN ('pending','processing')) for the
// same session. If one does, the rescan skips silently — the worker
// will deliver it on its next drain. Otherwise it enqueues a fresh
// events_inbox row with a webhook_id of the shape
// "rescan-<parent>-<rounds>" (or "rescan-product-<project>-<rounds>"),
// which EnqueueEventWithSource's INSERT OR IGNORE dedups against any
// previous rescan invocation.
//
// Called exactly once per process boot from Service.Run, before the
// ticker loop starts, so the worker drains the re-enqueued events on
// its first tick.
//
// Idempotent: a second call (operator-initiated, debug command) cannot
// double-fire the same parent because the webhook_id is identical and
// PRIMARY KEY-conflict. Returns a RescanReport summarising what
// happened so callers can log useful boot lines.
func RescanStuckVerifications(
	ctx context.Context,
	db *store.Store,
	logger *slog.Logger,
) (RescanReport, error) {
	if logger == nil {
		logger = slog.Default()
	}
	rep := RescanReport{}

	// 1. Task rows.
	taskRows, err := db.ListStuckTaskVerifications(ctx)
	if err != nil {
		return rep, fmt.Errorf("list stuck task_verifications: %w", err)
	}
	rep.TaskScanned = len(taskRows)
	for _, row := range taskRows {
		inFlight, ferr := db.HasInFlightEvent(ctx, rescanTaskSource, rescanTaskAction, row.ParentIssueID)
		if ferr != nil {
			logger.Warn("stuck_verify_rescan_inflight_check_failed",
				"table", "task",
				"parent", row.ParentIssueID,
				"err", ferr,
			)
			continue
		}
		webhookID := fmt.Sprintf("rescan-%s-%d", row.ParentIssueID, row.Rounds)
		if inFlight {
			logger.Info("stuck_verify_rescan",
				"table", "task",
				"parent", row.ParentIssueID,
				"rounds", row.Rounds,
				"in_flight", true,
				"webhook_id", webhookID,
			)
			rep.TaskSkippedInFlight++
			continue
		}
		enqueued, eerr := db.EnqueueEventWithSource(
			rescanTaskSource, webhookID, rescanTaskAction,
			row.ParentIssueID, row.ParentIssueID, "{}", "",
		)
		if eerr != nil {
			logger.Warn("stuck_verify_rescan_enqueue_failed",
				"table", "task",
				"parent", row.ParentIssueID,
				"err", eerr,
			)
			continue
		}
		logger.Info("stuck_verify_rescan",
			"table", "task",
			"parent", row.ParentIssueID,
			"rounds", row.Rounds,
			"webhook_id", webhookID,
			"fresh", enqueued,
		)
		if enqueued {
			rep.TaskReEnqueued++
		}
	}

	// 2. Product rows — same shape, separate webhook_id prefix and
	// source/action pair (the worker dispatches on source, see
	// worker.go).
	productRows, err := db.ListStuckProductVerifications(ctx)
	if err != nil {
		return rep, fmt.Errorf("list stuck product_verifications: %w", err)
	}
	rep.ProductScanned = len(productRows)
	for _, row := range productRows {
		inFlight, ferr := db.HasInFlightEvent(ctx, rescanProductSource, rescanProductAction, row.ProjectID)
		if ferr != nil {
			logger.Warn("stuck_verify_rescan_inflight_check_failed",
				"table", "product",
				"project", row.ProjectID,
				"err", ferr,
			)
			continue
		}
		webhookID := fmt.Sprintf("rescan-product-%s-%d", row.ProjectID, row.Rounds)
		if inFlight {
			logger.Info("stuck_verify_rescan",
				"table", "product",
				"project", row.ProjectID,
				"rounds", row.Rounds,
				"in_flight", true,
				"webhook_id", webhookID,
			)
			rep.ProductSkippedInFlight++
			continue
		}
		enqueued, eerr := db.EnqueueEventWithSource(
			rescanProductSource, webhookID, rescanProductAction,
			row.ProjectID, row.ProjectID, "{}", "",
		)
		if eerr != nil {
			logger.Warn("stuck_verify_rescan_enqueue_failed",
				"table", "product",
				"project", row.ProjectID,
				"err", eerr,
			)
			continue
		}
		logger.Info("stuck_verify_rescan",
			"table", "product",
			"project", row.ProjectID,
			"rounds", row.Rounds,
			"webhook_id", webhookID,
			"fresh", enqueued,
		)
		if enqueued {
			rep.ProductReEnqueued++
		}
	}

	return rep, nil
}
