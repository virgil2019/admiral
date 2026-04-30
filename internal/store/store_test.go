package store

import (
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestOpen_IdempotentMigrations(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	s1, err := Open(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("close first: %v", err)
	}

	// Re-open against the same DB. Migration 0004 (ALTER TABLE ADD COLUMN)
	// already ran in the first Open and would otherwise return
	// "duplicate column name" — Open must tolerate that.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })
}

func TestOpen_SetsBusyTimeout(t *testing.T) {
	s := newTestStore(t)

	var ms int
	if err := s.DB.QueryRow(`PRAGMA busy_timeout;`).Scan(&ms); err != nil {
		t.Fatalf("query busy_timeout: %v", err)
	}
	if ms < 1000 {
		// We set 5000 in Open. Anything less is a regression — concurrent
		// writers would fail-fast with SQLITE_BUSY instead of waiting.
		t.Errorf("busy_timeout = %d ms, want >= 1000 (Open should set 5000)", ms)
	}
}

func TestInsertTGUpdate_DedupeByUpdateID(t *testing.T) {
	s := newTestStore(t)

	fresh, err := s.InsertTGUpdate(100, 1, 2, "hello")
	if err != nil || !fresh {
		t.Fatalf("first insert: fresh=%v err=%v", fresh, err)
	}
	fresh, err = s.InsertTGUpdate(100, 1, 2, "hello")
	if err != nil || fresh {
		t.Fatalf("second insert (same update_id) should be fresh=false; got fresh=%v err=%v", fresh, err)
	}
	fresh, err = s.InsertTGUpdate(101, 1, 2, "world")
	if err != nil || !fresh {
		t.Fatalf("different update_id must be fresh; got fresh=%v err=%v", fresh, err)
	}
}

func TestUnprocessedTGUpdates_OrderedByUpdateID(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []int64{42, 10, 99} {
		if _, err := s.InsertTGUpdate(id, 1, 2, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.MarkTGUpdateDone(10); err != nil {
		t.Fatal(err)
	}
	pending, err := s.UnprocessedTGUpdates()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 || pending[0].UpdateID != 42 || pending[1].UpdateID != 99 {
		t.Fatalf("expected [42, 99] unprocessed, got %+v", pending)
	}
}

func TestMarkTGUpdateProcessed_Transactional(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.InsertTGUpdate(7, 11, 22, "msg"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkTGUpdateProcessed(7, "team-msg-1", "msg", 11); err != nil {
		t.Fatal(err)
	}
	pending, err := s.UnprocessedTGUpdates()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("expected no unprocessed; got %+v", pending)
	}
	var count int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM messages WHERE team_message_id='team-msg-1'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected 1 messages row; got %d", count)
	}
}

func TestKV_RoundTrip(t *testing.T) {
	s := newTestStore(t)
	if v, _ := s.KVGet("missing"); v != "" {
		t.Fatalf("expected empty for missing key, got %q", v)
	}
	if err := s.KVSet("k1", "v1"); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.KVGet("k1"); v != "v1" {
		t.Fatalf("expected v1, got %q", v)
	}
	if err := s.KVSet("k1", "v2"); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.KVGet("k1"); v != "v2" {
		t.Fatalf("expected v2 on upsert, got %q", v)
	}
}

func TestGetLastAutopilotJob_EmptyDB(t *testing.T) {
	s := newTestStore(t)
	j, err := s.GetLastAutopilotJob()
	if err != nil {
		t.Fatalf("expected (nil,nil), got err=%v", err)
	}
	if j != nil {
		t.Fatalf("expected nil job for empty db, got %+v", j)
	}
}

func TestGetLastAutopilotJob_SingleJob(t *testing.T) {
	s := newTestStore(t)
	claimed, err := s.ClaimAutopilotJob("session-1", "issue-1", "TEST-1")
	if err != nil || !claimed {
		t.Fatalf("claim failed: claimed=%v err=%v", claimed, err)
	}
	j, err := s.GetLastAutopilotJob()
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if j == nil || j.AgentSessionID != "session-1" {
		t.Fatalf("expected session-1, got %+v", j)
	}
}

func TestGetLastAutopilotJob_MultipleJobs_ReturnsLatest(t *testing.T) {
	s := newTestStore(t)
	// Create two jobs by claiming them at different times (using raw SQL to control ordering)
	_, err := s.DB.Exec(`
		INSERT INTO autopilot_jobs(agent_session_id, issue_id, issue_identifier, state, started_at, finished_at)
		VALUES('older-session', 'issue-old', 'OLD-1', 'DONE', '2024-01-01T00:00:00Z', '2024-01-01T01:00:00Z')
	`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.DB.Exec(`
		INSERT INTO autopilot_jobs(agent_session_id, issue_id, issue_identifier, state, started_at, finished_at)
		VALUES('newer-session', 'issue-new', 'NEW-1', 'DONE', '2024-01-02T00:00:00Z', '2024-01-02T01:00:00Z')
	`)
	if err != nil {
		t.Fatal(err)
	}
	j, err := s.GetLastAutopilotJob()
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if j == nil || j.AgentSessionID != "newer-session" {
		t.Fatalf("expected newer-session, got %+v", j)
	}
}

func TestGetLatestDoneJobByIssue_NoHistory(t *testing.T) {
	s := newTestStore(t)
	j, err := s.GetLatestDoneJobByIssue("ISSUE-NONE")
	if err != nil {
		t.Fatalf("expected (nil,nil), got err=%v", err)
	}
	if j != nil {
		t.Fatalf("expected nil job for unknown issue, got %+v", j)
	}
}

func TestGetLatestDoneJobByIssue_FailedButNoDone(t *testing.T) {
	s := newTestStore(t)
	_, err := s.DB.Exec(`
		INSERT INTO autopilot_jobs(agent_session_id, issue_id, issue_identifier, state, started_at, finished_at)
		VALUES('session-fail', 'issue-x', 'X-1', 'FAILED', '2024-01-01T00:00:00Z', '2024-01-01T01:00:00Z')
	`)
	if err != nil {
		t.Fatal(err)
	}
	j, err := s.GetLatestDoneJobByIssue("issue-x")
	if err != nil {
		t.Fatalf("expected (nil,nil), got err=%v", err)
	}
	if j != nil {
		t.Fatalf("expected nil for FAILED-only issue, got %+v", j)
	}
}

func TestGetLatestDoneJobByIssue_MultipleDone_ReturnsLatest(t *testing.T) {
	s := newTestStore(t)
	_, err := s.DB.Exec(`
		INSERT INTO autopilot_jobs(agent_session_id, issue_id, issue_identifier, state, started_at, finished_at, pr_url)
		VALUES('older-done', 'issue-y', 'Y-1', 'DONE', '2024-01-01T00:00:00Z', '2024-01-01T01:00:00Z', 'https://github.com/x/y/pull/old')
	`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.DB.Exec(`
		INSERT INTO autopilot_jobs(agent_session_id, issue_id, issue_identifier, state, started_at, finished_at, pr_url)
		VALUES('newer-done', 'issue-y', 'Y-1', 'DONE', '2024-01-02T00:00:00Z', '2024-01-02T01:00:00Z', 'https://github.com/x/y/pull/new')
	`)
	if err != nil {
		t.Fatal(err)
	}
	j, err := s.GetLatestDoneJobByIssue("issue-y")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if j == nil || j.AgentSessionID != "newer-done" {
		t.Fatalf("expected newer-done, got %+v", j)
	}
	if j.PRURL != "https://github.com/x/y/pull/new" {
		t.Fatalf("expected newer PR URL, got %q", j.PRURL)
	}
}
