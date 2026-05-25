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
// All Linear writes are best-effort: failures log a warning and the
// task stays in its current state, so the next tick retries naturally.
func (s *Service) advanceLinearStates(ctx context.Context) {
	if s.pr == nil {
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
	s.logger.Debug("state_advance_start", "tasks", len(tasks))
	for i := range tasks {
		if ctx.Err() != nil {
			return
		}
		s.advanceOne(ctx, &tasks[i])
	}
}

func (s *Service) advanceOne(ctx context.Context, t *store.AdmiralTask) {
	if t.PRURL == "" {
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
		s.advanceMerged(ctx, t)
	case "CLOSED":
		s.advanceCancelled(ctx, t)
	case "OPEN":
		s.advanceOpen(ctx, t, status.HasApprovedReview)
	}
}

func (s *Service) advanceMerged(ctx context.Context, t *store.AdmiralTask) {
	if err := s.pushLinearStateByType(ctx, t, "completed"); err != nil {
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
}

func (s *Service) advanceCancelled(ctx context.Context, t *store.AdmiralTask) {
	if err := s.pushLinearStateByType(ctx, t, "canceled"); err != nil {
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

func (s *Service) advanceOpen(ctx context.Context, t *store.AdmiralTask, hasApproval bool) {
	var target string
	if hasApproval {
		target = s.cfg.LinearStates.Reviewed
	} else {
		target = s.cfg.LinearStates.InReview
	}
	if target == "" {
		return
	}
	if err := s.pushLinearStateByName(ctx, t, target); err != nil {
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
func (s *Service) pushLinearStateByType(ctx context.Context, t *store.AdmiralTask, wantType string) error {
	issue, err := s.linear.GetIssue(ctx, t.IssueID)
	if err != nil {
		return fmt.Errorf("get issue: %w", err)
	}
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
func (s *Service) pushLinearStateByName(ctx context.Context, t *store.AdmiralTask, wantName string) error {
	issue, err := s.linear.GetIssue(ctx, t.IssueID)
	if err != nil {
		return fmt.Errorf("get issue: %w", err)
	}
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
