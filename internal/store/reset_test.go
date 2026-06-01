package store

import "testing"

func TestResetIssueRows_DeletesAllPerIssueRows(t *testing.T) {
	s := newTestStore(t)
	const issueID = "issue-A"

	// Seed one row in every per-issue table the reset is meant to clear.
	if _, err := s.ClaimAdmiralTask(issueID, "GEO-1", "sess-1"); err != nil {
		t.Fatalf("claim task: %v", err)
	}
	if err := s.UpsertDiscovererPick(DiscovererPick{
		IssueID: issueID, IssueIdentifier: "GEO-1", PickedState: "Todo",
	}); err != nil {
		t.Fatalf("upsert pick: %v", err)
	}
	if _, err := s.EnqueueEvent("wh-1", "issueCommentCreate", "sess-1", issueID, "{}"); err != nil {
		t.Fatalf("enqueue event: %v", err)
	}
	if err := s.InsertPendingQuestion(PendingQuestion{
		ID: "q-1", IssueID: issueID, ClaudeSessionID: "sess-1",
		Question: "?", OptionsJSON: "[]", CreatedAt: "2026-06-01T00:00:00Z",
	}); err != nil {
		t.Fatalf("insert question: %v", err)
	}

	if err := s.ResetIssueRows(issueID); err != nil {
		t.Fatalf("reset: %v", err)
	}

	if task, err := s.GetAdmiralTaskByIssue(issueID); err != nil || task != nil {
		t.Fatalf("admiral_tasks not cleared: task=%v err=%v", task, err)
	}
	if pick, err := s.GetDiscovererPick(issueID); err != nil || pick != nil {
		t.Fatalf("discoverer_picks not cleared: pick=%v err=%v", pick, err)
	}
	if q, err := s.GetOpenPendingQuestionByIssue(issueID); err != nil || q != nil {
		t.Fatalf("pending_questions not cleared: q=%v err=%v", q, err)
	}
	var n int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM events_inbox WHERE issue_id=?`, issueID).Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if n != 0 {
		t.Fatalf("events_inbox not cleared: %d rows remain", n)
	}
}

func TestResetIssueRows_LeavesOtherIssuesUntouched(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.ClaimAdmiralTask("issue-A", "GEO-1", "sess-1"); err != nil {
		t.Fatalf("claim A: %v", err)
	}
	if _, err := s.ClaimAdmiralTask("issue-B", "GEO-2", "sess-2"); err != nil {
		t.Fatalf("claim B: %v", err)
	}

	if err := s.ResetIssueRows("issue-A"); err != nil {
		t.Fatalf("reset: %v", err)
	}

	if task, err := s.GetAdmiralTaskByIssue("issue-B"); err != nil || task == nil {
		t.Fatalf("issue-B task should survive: task=%v err=%v", task, err)
	}
}

func TestDeleteTaskVerification(t *testing.T) {
	s := newTestStore(t)
	const parent = "parent-1"
	if _, err := s.BumpTaskVerificationRound(parent); err != nil {
		t.Fatalf("bump: %v", err)
	}

	if err := s.DeleteTaskVerification(parent); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if tv, err := s.GetTaskVerification(parent); err != nil || tv != nil {
		t.Fatalf("task_verifications not cleared: tv=%v err=%v", tv, err)
	}

	// Absent row → no-op, no error.
	if err := s.DeleteTaskVerification("never-existed"); err != nil {
		t.Fatalf("delete absent: %v", err)
	}
}
