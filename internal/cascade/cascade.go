// Package cascade carries the verify-trigger walk shared by the discoverer
// (leaf merge → check task parent) and the autopilot (intermediate verify
// pass → check grandparent). One helper, two call sites, same dedup key
// shape: this is the one path that turns a just-completed Linear issue into
// a verify event for whatever sits above it, if anything does.
package cascade

import (
	"context"
	"log/slog"

	"github.com/georgehuang/admiral/internal/linear"
)

// LinearClient is the slice of the Linear client the cascade walk needs:
// hop up one level (GetParentID), then enumerate the siblings under that
// parent (GetSubIssues). Both discoverer and autopilot client interfaces
// satisfy this surface without changes.
type LinearClient interface {
	GetParentID(ctx context.Context, childID string) (string, error)
	GetSubIssues(ctx context.Context, parentID string) ([]linear.SubIssue, error)
}

// EventStore is the slice of the store the cascade walk writes to: it
// hands a source="verify" event to the autopilot worker's queue.
type EventStore interface {
	EnqueueEventWithSource(source, webhookID, action, sessionID, issueID, payloadJSON, commentID string) (bool, error)
}

// MaybeEnqueueVerify walks from a just-completed issue (a merged leaf
// sub-issue OR a non-leaf parent whose own verify just passed) up to its
// own parent and, when EVERY sibling has reached a completed Linear state,
// enqueues a source="verify" event so the autopilot worker judges that
// parent against its PRD. Best-effort: any failure logs and returns — the
// next completion re-checks, so a transient error just defers the trigger.
//
// Each cascade hop has a unique webhook_id ("verify-<parent>-<trigger>"),
// keyed on the immediate trigger, so a multi-hop cascade does NOT dedup
// against itself (every level's trigger differs from the next).
//
// The parent reached by this walk is never labelled/pickable on its own
// (only leaf sub-issues are); this walk is the sole path that escalates
// completion up the tree.
func MaybeEnqueueVerify(ctx context.Context, lc LinearClient, db EventStore, logger *slog.Logger, completedIssueID, completedIdentifier string) {
	if logger == nil {
		logger = slog.Default()
	}
	parentID, err := lc.GetParentID(ctx, completedIssueID)
	if err != nil {
		logger.Warn("verify_enqueue_get_parent_failed",
			"issue", completedIdentifier, "err", err)
		return
	}
	if parentID == "" {
		// Top of tree — nothing above to cascade into.
		return
	}

	subs, err := lc.GetSubIssues(ctx, parentID)
	if err != nil {
		logger.Warn("verify_enqueue_get_subs_failed",
			"parent", parentID, "issue", completedIdentifier, "err", err)
		return
	}
	if !AllSubsCompleted(subs) {
		logger.Debug("verify_enqueue_skip_incomplete",
			"parent", parentID, "subs", len(subs))
		return
	}

	webhookID := "verify-" + parentID + "-" + completedIssueID
	fresh, err := db.EnqueueEventWithSource(
		"verify", webhookID, "verify.task_complete", parentID, parentID, "{}", "")
	if err != nil {
		logger.Warn("verify_enqueue_failed",
			"parent", parentID, "issue", completedIdentifier, "err", err)
		return
	}
	if fresh {
		logger.Info("verify_enqueued",
			"parent", parentID, "trigger_sub", completedIdentifier)
	} else {
		logger.Debug("verify_enqueue_deduped",
			"parent", parentID, "webhook_id", webhookID)
	}
}

// AllSubsCompleted reports whether every sub-issue is in a completed-type
// Linear state. An empty list is NOT complete — the caller only reaches
// here after walking up from a real child, so a parent reporting zero
// children is an anomaly (partial Linear read) we must not treat as done.
//
// "completed" is the ONLY accepted terminal type, by design: a sub left in
// "canceled" (its PR was closed unmerged) blocks the parent's verification
// indefinitely rather than letting it auto-pass with a missing part. A
// human resolves the canceled sub to unblock.
func AllSubsCompleted(subs []linear.SubIssue) bool {
	if len(subs) == 0 {
		return false
	}
	for _, sub := range subs {
		if sub.StateType != "completed" {
			return false
		}
	}
	return true
}
