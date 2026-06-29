package store

// Cascade-delete helpers for the reset-task admin endpoint. A reset returns
// an issue to a never-worked state by removing every row admiral writes while
// it owns that issue, so the discoverer can pick it up fresh after re-activation.

// ResetIssueRows deletes all per-issue rows admiral writes while working a
// single issue: the live task (admiral_tasks), the discoverer pick record
// (discoverer_picks), any queued/processed webhook events (events_inbox), and
// any pending HITL question (pending_questions). Runs in one transaction so a
// partial delete can't leave a half-reset issue.
//
// pending_questions is included even though it is keyed independently: a stale
// unanswered question would make the re-run immediately re-enter AWAITING_INPUT
// (GetOpenPendingQuestionByIssue matches on issue_id), defeating the reset.
//
// admiral_task_history is intentionally NOT touched — it is an append-only
// audit log of prior attempts and is meant to survive a reset.
func (s *Store) ResetIssueRows(issueID string) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, stmt := range []string{
		`DELETE FROM admiral_tasks WHERE issue_id=?`,
		`DELETE FROM discoverer_picks WHERE issue_id=?`,
		`DELETE FROM events_inbox WHERE issue_id=?`,
		`DELETE FROM pending_questions WHERE issue_id=?`,
	} {
		if _, err := tx.Exec(stmt, issueID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteTaskVerification removes the autonomous-verify-loop row for a parent
// issue, so a re-shipped task starts verification from round 0. No-op (returns
// nil) when no row exists.
func (s *Store) DeleteTaskVerification(parentIssueID string) error {
	_, err := s.DB.Exec(`DELETE FROM task_verifications WHERE parent_issue_id=?`, parentIssueID)
	return err
}
