package discoverer

import (
	"context"
	"fmt"
	"strings"

	"github.com/georgehuang/admiral/internal/linear"
	"github.com/georgehuang/admiral/internal/store"
)

// advanceLinearStates walks every DONE / DONE_THREAD_INCONSISTENT
// admiral_task, polls its GitHub PR, and pushes the right Linear
// workflow state when the PR has progressed:
//
//	PR merged           → Linear state.type=completed + task=DONE_MERGED
//	PR closed unmerged  → Linear state.type=canceled  + task=CANCELLED
//	PR open + approved  → Linear LinearStates.Reviewed (if configured)
//	PR open + no review → Linear LinearStates.InReview (if configured)
//
// Scoped to projects with auto_pick_enabled — a task whose issue lives
// in a project that is no longer opted in is skipped, leaving its
// Linear state where it is. Re-enabling the project resumes
// transitions on the next tick.
//
// All Linear writes are best-effort: failures log a warning and the
// task stays in its current state, so the next tick retries naturally.
func (s *Service) advanceLinearStates(ctx context.Context) {
	if s.pr == nil {
		return
	}
	enabled, err := s.enabledProjectSet()
	if err != nil {
		s.logger.Error("state_advance_project_list_failed", "err", err)
		return
	}
	if len(enabled) == 0 {
		s.logger.Debug("state_advance_skipped_no_enabled_projects")
		return
	}
	tasks, err := s.store.ListAdmiralTasksByStates([]string{
		store.JobStateDone,
		store.JobStateDoneThreadInconsistent,
	})
	if err != nil {
		s.logger.Error("state_advance_list_failed", "err", err)
		return
	}
	if len(tasks) == 0 {
		return
	}
	s.logger.Debug("state_advance_start", "tasks", len(tasks), "enabled_projects", len(enabled))
	for i := range tasks {
		if ctx.Err() != nil {
			return
		}
		s.advanceOne(ctx, &tasks[i], enabled)
	}
}

func (s *Service) enabledProjectSet() (map[string]struct{}, error) {
	ids, err := s.store.ListAutoPickEnabledProjectIDs()
	if err != nil {
		return nil, err
	}
	out := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		out[id] = struct{}{}
	}
	return out, nil
}

func (s *Service) advanceOne(ctx context.Context, t *store.AdmiralTask, enabled map[string]struct{}) {
	if t.PRURL == "" {
		return
	}

	// Resolve the issue first: project_id gates the rest, so it pays
	// off to filter via one cheap GraphQL call before the more
	// expensive `gh pr view` subprocess. The same payload (team_id /
	// state_name) is then reused by the downstream push* helpers,
	// saving a redundant GetIssue call per task on the happy path.
	issue, err := s.linear.GetIssue(ctx, t.IssueID)
	if err != nil {
		s.logger.Warn("state_advance_get_issue_failed",
			"issue", t.IssueIdentifier, "err", err)
		return
	}
	if issue.ProjectID == "" {
		s.logger.Debug("state_advance_skip_issue_has_no_project",
			"issue", t.IssueIdentifier)
		return
	}
	if _, ok := enabled[issue.ProjectID]; !ok {
		s.logger.Debug("state_advance_skip_not_enabled_project",
			"issue", t.IssueIdentifier,
			"project", issue.ProjectID)
		return
	}

	status, err := s.pr.GetPRStatus(ctx, t.PRURL)
	if err != nil {
		s.logger.Warn("state_advance_pr_lookup_failed",
			"issue", t.IssueIdentifier, "pr", t.PRURL, "err", err)
		return
	}
	if status.State == "" {
		s.logger.Debug("state_advance_pr_unresolved",
			"issue", t.IssueIdentifier, "pr", t.PRURL)
		return
	}

	switch status.State {
	case "MERGED":
		s.advanceMerged(ctx, t, issue)
	case "CLOSED":
		s.advanceCancelled(ctx, t, issue)
	case "OPEN":
		s.advanceOpen(ctx, t, issue, status.HasApprovedReview)
	}
}

func (s *Service) advanceMerged(ctx context.Context, t *store.AdmiralTask, issue *linear.Issue) {
	if err := s.pushLinearStateByType(ctx, t, issue, "completed"); err != nil {
		s.logger.Warn("state_advance_linear_completed_failed",
			"issue", t.IssueIdentifier, "err", err)
		return
	}
	if err := s.store.UpdateAdmiralTask(t.IssueID, func(at *store.AdmiralTask) {
		at.State = store.JobStateDoneMerged
	}); err != nil {
		s.logger.Warn("state_advance_task_done_merged_failed",
			"issue", t.IssueIdentifier, "err", err)
		return
	}
	s.logger.Info("state_advance_merged",
		"issue", t.IssueIdentifier, "pr", t.PRURL)

	// A merged sub-issue may have completed its parent "task". Check and, if
	// so, enqueue an autonomous L2 verification of the whole task (D).
	s.maybeEnqueueVerify(ctx, t)
}

// maybeEnqueueVerify walks from a just-merged sub-issue up to its parent
// "task" and, when EVERY sibling sub-issue has reached a completed Linear
// state, enqueues a source="verify" event so the autopilot worker judges the
// whole task against its PRD. Best-effort: any failure logs and returns — the
// next merge re-checks, so a transient error just defers the trigger.
//
// The parent is intentionally never labelled/pickable (only sub-issues are),
// so it never surfaces in the assign scan; this walk is the sole path that
// acts on it.
func (s *Service) maybeEnqueueVerify(ctx context.Context, t *store.AdmiralTask) {
	parentID, err := s.linear.GetParentID(ctx, t.IssueID)
	if err != nil {
		s.logger.Warn("verify_enqueue_get_parent_failed",
			"issue", t.IssueIdentifier, "err", err)
		return
	}
	if parentID == "" {
		// Top-level issue, not a decomposed sub-task — nothing to verify.
		return
	}

	subs, err := s.linear.GetSubIssues(ctx, parentID)
	if err != nil {
		s.logger.Warn("verify_enqueue_get_subs_failed",
			"parent", parentID, "issue", t.IssueIdentifier, "err", err)
		return
	}
	if !allSubsCompleted(subs) {
		s.logger.Debug("verify_enqueue_skip_incomplete",
			"parent", parentID, "subs", len(subs))
		return
	}

	// webhook_id is the queue's dedup key. Keying on (parent, triggering sub)
	// keeps each round's trigger unique: a later round's follow-up sub merges
	// under a different id, so it enqueues a fresh verify instead of being
	// silently deduped against an earlier round.
	webhookID := "verify-" + parentID + "-" + t.IssueID
	fresh, err := s.store.EnqueueEventWithSource(
		"verify", webhookID, "verify.task_complete", parentID, parentID, "{}", "")
	if err != nil {
		s.logger.Warn("verify_enqueue_failed",
			"parent", parentID, "issue", t.IssueIdentifier, "err", err)
		return
	}
	if fresh {
		s.logger.Info("verify_enqueued",
			"parent", parentID, "trigger_sub", t.IssueIdentifier)
	} else {
		s.logger.Debug("verify_enqueue_deduped",
			"parent", parentID, "webhook_id", webhookID)
	}
}

// allSubsCompleted reports whether every sub-issue is in a completed-type
// Linear state. An empty list is NOT complete — the caller only reaches here
// after walking up from a real sub-issue, so a parent reporting zero children
// is an anomaly (e.g. a partial Linear read) we must not treat as done.
//
// "completed" is the ONLY accepted terminal type, by design: a sub left in
// "canceled" (its PR was closed unmerged — a dropped piece of work) blocks
// verification indefinitely rather than letting the task auto-verify with a
// missing part. Auto-closing a task that is missing work is worse than a
// visible stall (the parent simply stays out of a completed state); a human
// resolves the canceled sub to unblock.
func allSubsCompleted(subs []linear.SubIssue) bool {
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

func (s *Service) advanceCancelled(ctx context.Context, t *store.AdmiralTask, issue *linear.Issue) {
	if err := s.pushLinearStateByType(ctx, t, issue, "canceled"); err != nil {
		s.logger.Warn("state_advance_linear_canceled_failed",
			"issue", t.IssueIdentifier, "err", err)
		return
	}
	if err := s.store.UpdateAdmiralTask(t.IssueID, func(at *store.AdmiralTask) {
		at.State = store.JobStateCancelled
	}); err != nil {
		s.logger.Warn("state_advance_task_cancelled_failed",
			"issue", t.IssueIdentifier, "err", err)
		return
	}
	s.logger.Info("state_advance_cancelled",
		"issue", t.IssueIdentifier, "pr", t.PRURL)
}

func (s *Service) advanceOpen(ctx context.Context, t *store.AdmiralTask, issue *linear.Issue, hasApproval bool) {
	var target string
	if hasApproval {
		target = s.cfg.LinearStates.Reviewed
	} else {
		target = s.cfg.LinearStates.InReview
	}
	if target == "" {
		return
	}
	if err := s.pushLinearStateByName(ctx, t, issue, target); err != nil {
		s.logger.Warn("state_advance_linear_open_failed",
			"issue", t.IssueIdentifier, "target", target, "err", err)
	}
}

// pushLinearStateByType pushes the issue's Linear workflow state to the
// state matching wantType (e.g. "completed", "canceled"). Skips when
// the issue is already in a state of that type (avoids redundant API
// calls and Linear-side state churn). Returns nil for both
// "wrote successfully" and "skipped because already in target type" —
// the latter logs a debug line so the silent skip is observable.
func (s *Service) pushLinearStateByType(ctx context.Context, t *store.AdmiralTask, issue *linear.Issue, wantType string) error {
	states, err := s.workflowStates(ctx, issue.TeamID)
	if err != nil {
		return err
	}
	cur := lookupStateByName(states, issue.StateName)
	if cur != nil && cur.Type == wantType {
		s.logger.Debug("state_advance_linear_already_in_target_type",
			"issue", t.IssueIdentifier,
			"current_state", issue.StateName,
			"want_type", wantType)
		return nil
	}
	target := lookupStateByType(states, wantType)
	if target == nil {
		return fmt.Errorf("no Linear state of type %q in team %s", wantType, issue.TeamID)
	}
	return s.linear.IssueUpdate(ctx, t.IssueID, target.ID)
}

// pushLinearStateByName pushes the issue's Linear workflow state to
// the state with the given (case-insensitive) name. Skips when the
// issue is already there.
func (s *Service) pushLinearStateByName(ctx context.Context, t *store.AdmiralTask, issue *linear.Issue, wantName string) error {
	if eqFoldTrim(issue.StateName, wantName) {
		return nil
	}
	states, err := s.workflowStates(ctx, issue.TeamID)
	if err != nil {
		return err
	}
	target := lookupStateByName(states, wantName)
	if target == nil {
		return fmt.Errorf("no Linear state named %q in team %s", wantName, issue.TeamID)
	}
	return s.linear.IssueUpdate(ctx, t.IssueID, target.ID)
}

func (s *Service) workflowStates(ctx context.Context, teamID string) ([]linear.WorkflowState, error) {
	if cached, ok := s.workflowStatesCache[teamID]; ok {
		return cached, nil
	}
	states, err := s.linear.GetWorkflowStates(ctx, teamID)
	if err != nil {
		return nil, fmt.Errorf("get workflow states for team %s: %w", teamID, err)
	}
	s.workflowStatesCache[teamID] = states
	return states, nil
}

func lookupStateByName(states []linear.WorkflowState, name string) *linear.WorkflowState {
	for i := range states {
		if eqFoldTrim(states[i].Name, name) {
			return &states[i]
		}
	}
	return nil
}

// lookupStateByType returns the lowest-position state of wantType, or
// nil. Linear orders states by position within a team; the smallest
// completed/canceled state by position is the canonical target (e.g.
// "Done" before "Done — archived", "Cancelled" before "Duplicate").
func lookupStateByType(states []linear.WorkflowState, wantType string) *linear.WorkflowState {
	var best *linear.WorkflowState
	for i := range states {
		if states[i].Type != wantType {
			continue
		}
		if best == nil || states[i].Position < best.Position {
			best = &states[i]
		}
	}
	return best
}

func eqFoldTrim(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}
