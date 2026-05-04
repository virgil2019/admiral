package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
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

func TestOpen_LimitsToSingleConnection(t *testing.T) {
	s := newTestStore(t)

	// Without MaxOpenConns=1, *sql.DB will spawn extra connections under
	// concurrent load and PRAGMAs (busy_timeout, WAL) won't carry over,
	// reproducing the original "SQLITE_BUSY (5)" race we observed in prod.
	if got := s.DB.Stats().MaxOpenConnections; got != 1 {
		t.Errorf("MaxOpenConnections = %d, want 1 (PRAGMAs only persist on a single conn)", got)
	}
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

func TestApplyMigrations_FreshDB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fresh.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	var count int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	if count != len(migrations) {
		t.Fatalf("expected %d migrations recorded, got %d", len(migrations), count)
	}
}

func TestApplyMigrations_Idempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "idempotent.db")

	s1, err := Open(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("close first: %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })

	var count int
	if err := s2.DB.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	if count != len(migrations) {
		t.Fatalf("expected %d migrations (no duplicates), got %d", len(migrations), count)
	}
}

func TestBackfillMigrations_LegacyDB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.db")

	// Simulate a pre-migration DB: run all 6 migration SQLs without schema_migrations
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}

	if _, err := db.Exec(migration0001); err != nil {
		t.Fatalf("migration0001: %v", err)
	}
	if _, err := db.Exec(migration0002); err != nil {
		t.Fatalf("migration0002: %v", err)
	}
	if _, err := db.Exec(migration0003); err != nil {
		t.Fatalf("migration0003: %v", err)
	}
	if _, err := db.Exec(migration0004); err != nil {
		t.Fatalf("migration0004: %v", err)
	}
	if _, err := db.Exec(migration0005); err != nil {
		t.Fatalf("migration0005: %v", err)
	}
	if _, err := db.Exec(migration0006); err != nil {
		t.Fatalf("migration0006: %v", err)
	}
	db.Close()

	// Open should backfill schema_migrations
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open after backfill: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	var count int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	if count != len(migrations) {
		t.Fatalf("expected %d migrations (6 backfilled + remaining applied at boot), got %d", len(migrations), count)
	}
}

func TestBackfillMigrations_PartialDB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "partial.db")

	// Simulate a DB that ran migrations 0001-0003 only
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := db.Exec(migration0001); err != nil {
		t.Fatalf("migration0001: %v", err)
	}
	if _, err := db.Exec(migration0002); err != nil {
		t.Fatalf("migration0002: %v", err)
	}
	if _, err := db.Exec(migration0003); err != nil {
		t.Fatalf("migration0003: %v", err)
	}
	db.Close()

	// Open: backfill should record 1-3, applyMigrations should add 4-6
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open after backfill: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	var count int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	if count != len(migrations) {
		t.Fatalf("expected %d migrations (3 backfilled + remaining applied), got %d", len(migrations), count)
	}
}

func TestAuthError_HealthyByDefault(t *testing.T) {
	s := newTestStore(t)
	st, err := s.GetAuthError()
	if err != nil {
		t.Fatalf("GetAuthError: %v", err)
	}
	if st.Reason != "" || st.ErrAt != "" || st.NotifiedAt != "" {
		t.Fatalf("expected zero AuthErrorState, got %+v", st)
	}
}

func TestAuthError_MarkAndClearRoundtrip(t *testing.T) {
	s := newTestStore(t)

	if err := s.MarkAuthBroken("invalid_grant: refresh token revoked"); err != nil {
		t.Fatalf("MarkAuthBroken: %v", err)
	}
	st, err := s.GetAuthError()
	if err != nil {
		t.Fatalf("GetAuthError: %v", err)
	}
	if st.Reason != "invalid_grant: refresh token revoked" {
		t.Fatalf("Reason: got %q", st.Reason)
	}
	if st.ErrAt == "" {
		t.Fatal("ErrAt: expected RFC3339, got empty")
	}

	// MarkAuthBroken is idempotent on the timestamp — calling again should
	// keep the original ErrAt so the caller can tell how long we've been
	// broken.
	firstErrAt := st.ErrAt
	if err := s.MarkAuthBroken("invalid_grant: still broken"); err != nil {
		t.Fatalf("MarkAuthBroken (second): %v", err)
	}
	st, _ = s.GetAuthError()
	if st.ErrAt != firstErrAt {
		t.Fatalf("ErrAt should be sticky: was %q, became %q", firstErrAt, st.ErrAt)
	}
	if st.Reason != "invalid_grant: still broken" {
		t.Fatalf("Reason should update on re-mark: got %q", st.Reason)
	}

	if err := s.ClearAuthError(); err != nil {
		t.Fatalf("ClearAuthError: %v", err)
	}
	st, _ = s.GetAuthError()
	if st.Reason != "" || st.ErrAt != "" || st.NotifiedAt != "" {
		t.Fatalf("after Clear, expected zero state, got %+v", st)
	}
}

func TestAuthError_MarkNotified(t *testing.T) {
	s := newTestStore(t)
	if err := s.MarkAuthBroken("invalid_grant"); err != nil {
		t.Fatalf("MarkAuthBroken: %v", err)
	}
	if err := s.MarkAuthNotified("2026-04-30T12:00:00Z"); err != nil {
		t.Fatalf("MarkAuthNotified: %v", err)
	}
	st, _ := s.GetAuthError()
	if st.NotifiedAt != "2026-04-30T12:00:00Z" {
		t.Fatalf("NotifiedAt: got %q", st.NotifiedAt)
	}
}

// --- admiral_tasks (GEO-50, PR-A) ---

func TestClaimAdmiralTask_FirstTime(t *testing.T) {
	s := newTestStore(t)
	fresh, err := s.ClaimAdmiralTask("issue-A", "GEO-A", "session-1")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !fresh {
		t.Fatalf("expected fresh=true on first claim")
	}
	got, err := s.GetAdmiralTaskByIssue("issue-A")
	if err != nil || got == nil {
		t.Fatalf("get after claim: got=%v err=%v", got, err)
	}
	if got.IssueID != "issue-A" || got.IssueIdentifier != "GEO-A" {
		t.Errorf("identity wrong: %+v", got)
	}
	if got.State != JobStateReceived {
		t.Errorf("state: got %q want RECEIVED", got.State)
	}
	if got.AttemptN != 1 {
		t.Errorf("attempt_n: got %d want 1", got.AttemptN)
	}
	if got.LastEventSessionID != "session-1" {
		t.Errorf("last_event_session_id: got %q", got.LastEventSessionID)
	}
}

func TestClaimAdmiralTask_Idempotent(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.ClaimAdmiralTask("issue-B", "GEO-B", "s1"); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	again, err := s.ClaimAdmiralTask("issue-B", "GEO-B-other", "s2")
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if again {
		t.Errorf("expected fresh=false on second claim")
	}
	// Original identifier and session preserved.
	got, _ := s.GetAdmiralTaskByIssue("issue-B")
	if got.IssueIdentifier != "GEO-B" || got.LastEventSessionID != "s1" {
		t.Errorf("second claim must not mutate; got %+v", got)
	}
}

func TestGetAdmiralTaskByIssue_NotFound(t *testing.T) {
	s := newTestStore(t)
	got, err := s.GetAdmiralTaskByIssue("issue-missing")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for missing issue, got %+v", got)
	}
}

func TestUpdateAdmiralTask_RoundTrip(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.ClaimAdmiralTask("issue-C", "GEO-C", "s1"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := s.UpdateAdmiralTask("issue-C", func(t *AdmiralTask) {
		t.State = JobStateExecuting
		t.Branch = "linear/geo-c"
		t.WorktreePath = "/tmp/worktrees/geo-c"
		t.PRURL = "https://github.com/x/y/pull/1"
		t.ClaudeSessionID = "claude-session-abc"
		t.LastEventSessionID = "s2"
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ := s.GetAdmiralTaskByIssue("issue-C")
	if got.State != JobStateExecuting {
		t.Errorf("state: %q", got.State)
	}
	if got.Branch != "linear/geo-c" {
		t.Errorf("branch: %q", got.Branch)
	}
	if got.PRURL != "https://github.com/x/y/pull/1" {
		t.Errorf("pr_url: %q", got.PRURL)
	}
	if got.ClaudeSessionID != "claude-session-abc" {
		t.Errorf("claude_session_id: %q", got.ClaudeSessionID)
	}
	if got.LastEventSessionID != "s2" {
		t.Errorf("last_event_session_id: %q", got.LastEventSessionID)
	}
	if got.AttemptN != 1 {
		t.Errorf("attempt_n must remain 1 on plain update; got %d", got.AttemptN)
	}
}

func TestUpdateAdmiralTask_NoRow(t *testing.T) {
	s := newTestStore(t)
	err := s.UpdateAdmiralTask("issue-nope", func(t *AdmiralTask) {
		t.State = JobStateDone
	})
	if err == nil {
		t.Errorf("expected error updating non-existent task; got nil")
	}
}

func TestListAdmiralTaskHistoryByIssue_Empty(t *testing.T) {
	s := newTestStore(t)
	rows, err := s.ListAdmiralTaskHistoryByIssue("issue-empty")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected empty history, got %d rows", len(rows))
	}
}

// TestMigration0012_BackfillsLatestPerIssue verifies migration 0012
// populates admiral_tasks from autopilot_jobs by selecting the latest
// (max started_at) row per issue_id. Older rows for the same issue are
// NOT pulled into the live table — they remain in autopilot_jobs only.
func TestMigration0012_BackfillsLatestPerIssue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	// Open and run all migrations through 0011 to set up autopilot_jobs +
	// admiral_tasks empty. Then manually insert autopilot_jobs rows
	// emulating pre-migration data and re-trigger backfill.
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	// Seed autopilot_jobs with three rows for issue-X (oldest, middle,
	// latest) and one row for issue-Y. Migration 0012 already ran on
	// fresh DB, so its INSERT was a no-op (autopilot_jobs was empty).
	// We re-execute the backfill SQL manually after seeding.
	insert := func(sessID, issueID, identifier, state, started string) {
		t.Helper()
		_, err := s.DB.Exec(`
			INSERT INTO autopilot_jobs(agent_session_id, issue_id, issue_identifier, state, started_at, pr_url, branch, claude_session_id)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?)
		`, sessID, issueID, identifier, state, started,
			"https://github.com/x/y/pull/"+sessID, "linear/"+identifier, "claude-"+sessID)
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	insert("s1", "issue-X", "GEO-X", "DONE", "2026-04-28T10:00:00Z")
	insert("s2", "issue-X", "GEO-X", "DONE", "2026-04-29T10:00:00Z")   // middle
	insert("s3", "issue-X", "GEO-X", "FAILED", "2026-04-30T10:00:00Z") // latest
	insert("s4", "issue-Y", "GEO-Y", "DONE", "2026-04-29T12:00:00Z")

	// Re-run the backfill SQL manually (migration 0012's INSERT statement).
	if _, err := s.DB.Exec(migration0012); err != nil {
		t.Fatalf("re-run backfill: %v", err)
	}

	// issue-X live row should be the latest one (s3, FAILED).
	x, err := s.GetAdmiralTaskByIssue("issue-X")
	if err != nil || x == nil {
		t.Fatalf("get issue-X: got=%v err=%v", x, err)
	}
	if x.State != "FAILED" {
		t.Errorf("issue-X state: got %q want FAILED (latest)", x.State)
	}
	if x.LastEventSessionID != "s3" {
		t.Errorf("issue-X last_event_session_id: got %q want s3", x.LastEventSessionID)
	}
	if x.PRURL != "https://github.com/x/y/pull/s3" {
		t.Errorf("issue-X pr_url: got %q", x.PRURL)
	}

	// issue-Y has only one row; it lives in admiral_tasks.
	y, err := s.GetAdmiralTaskByIssue("issue-Y")
	if err != nil || y == nil {
		t.Fatalf("get issue-Y: got=%v err=%v", y, err)
	}
	if y.LastEventSessionID != "s4" {
		t.Errorf("issue-Y last_event_session_id: got %q", y.LastEventSessionID)
	}

	// admiral_task_history is intentionally empty after PR-A backfill —
	// only future supersessions populate it.
	hist, _ := s.ListAdmiralTaskHistoryByIssue("issue-X")
	if len(hist) != 0 {
		t.Errorf("expected empty history (PR-A doesn't backfill it), got %d rows", len(hist))
	}

	// Re-running backfill is idempotent (ON CONFLICT DO NOTHING).
	if _, err := s.DB.Exec(migration0012); err != nil {
		t.Fatalf("re-run backfill (idempotent): %v", err)
	}
	x2, _ := s.GetAdmiralTaskByIssue("issue-X")
	if x2.LastEventSessionID != "s3" {
		t.Errorf("idempotent re-run mutated state; got last_event_session_id=%q", x2.LastEventSessionID)
	}
}

func TestMoveAdmiralTaskToHistoryAndClaimNew(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.ClaimAdmiralTask("issue-R", "GEO-R", "s-1"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := s.UpdateAdmiralTask("issue-R", func(t *AdmiralTask) {
		t.State = JobStateDone
		t.Branch = "linear/geo-r"
		t.PRURL = "https://github.com/x/y/pull/1"
		t.ClaudeSessionID = "claude-1"
		t.FinishedAt = "2026-05-03T10:00:00Z"
	}); err != nil {
		t.Fatalf("update to DONE: %v", err)
	}

	newN, err := s.MoveAdmiralTaskToHistoryAndClaimNew("issue-R", "superseded_by_rerun", "GEO-R", "s-2")
	if err != nil {
		t.Fatalf("supersede: %v", err)
	}
	if newN != 2 {
		t.Errorf("new attempt_n: got %d want 2", newN)
	}

	live, _ := s.GetAdmiralTaskByIssue("issue-R")
	if live == nil {
		t.Fatal("live row missing after supersede")
	}
	if live.AttemptN != 2 || live.State != JobStateReceived {
		t.Errorf("live: attempt_n=%d state=%q", live.AttemptN, live.State)
	}
	if live.LastEventSessionID != "s-2" {
		t.Errorf("live last_event_session_id: %q", live.LastEventSessionID)
	}
	if live.PRURL != "" || live.Branch != "" {
		t.Errorf("live row should start clean; got branch=%q pr=%q", live.Branch, live.PRURL)
	}

	hist, err := s.ListAdmiralTaskHistoryByIssue("issue-R")
	if err != nil {
		t.Fatalf("list history: %v", err)
	}
	if len(hist) != 1 {
		t.Fatalf("history rows: got %d want 1", len(hist))
	}
	h := hist[0]
	if h.AttemptN != 1 || h.State != JobStateDone {
		t.Errorf("history attempt: got n=%d state=%q", h.AttemptN, h.State)
	}
	if h.PRURL != "https://github.com/x/y/pull/1" || h.Branch != "linear/geo-r" {
		t.Errorf("history fields lost: %+v", h)
	}
	if h.SupersededReason != "superseded_by_rerun" {
		t.Errorf("supersede reason: %q", h.SupersededReason)
	}
	if h.SupersededAt == "" {
		t.Errorf("supersede timestamp empty")
	}

	if err := s.UpdateAdmiralTask("issue-R", func(t *AdmiralTask) {
		t.State = JobStateDone
	}); err != nil {
		t.Fatalf("set new live to DONE: %v", err)
	}
	newN2, err := s.MoveAdmiralTaskToHistoryAndClaimNew("issue-R", "superseded_by_rerun", "GEO-R", "s-3")
	if err != nil {
		t.Fatalf("second supersede: %v", err)
	}
	if newN2 != 3 {
		t.Errorf("second new attempt_n: got %d want 3", newN2)
	}
	hist2, _ := s.ListAdmiralTaskHistoryByIssue("issue-R")
	if len(hist2) != 2 {
		t.Errorf("history after 2 supersessions: got %d rows want 2", len(hist2))
	}
	if hist2[0].AttemptN != 1 || hist2[1].AttemptN != 2 {
		t.Errorf("history attempts not in order: got %d, %d", hist2[0].AttemptN, hist2[1].AttemptN)
	}
}

func TestMoveAdmiralTaskToHistoryAndClaimNew_NoLiveRow(t *testing.T) {
	s := newTestStore(t)
	_, err := s.MoveAdmiralTaskToHistoryAndClaimNew("issue-missing", "rerun", "GEO-X", "s-1")
	if err == nil {
		t.Errorf("expected error superseding missing live row; got nil")
	}
}

// TestMigration0012_SkipsEmptyIssueID verifies the backfill ignores
// autopilot_jobs rows with empty issue_id (legacy/test fixtures may have
// these). admiral_tasks PK is issue_id NOT NULL — empty would either
// collide or fail the insert.
func TestMigration0012_SkipsEmptyIssueID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	_, err = s.DB.Exec(`
		INSERT INTO autopilot_jobs(agent_session_id, issue_id, state, started_at)
		VALUES('s-empty', '', 'DONE', '2026-04-30T10:00:00Z')
	`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := s.DB.Exec(migration0012); err != nil {
		t.Fatalf("re-run backfill: %v", err)
	}
	// admiral_tasks should still be empty.
	var count int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM admiral_tasks`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("admiral_tasks should not contain rows from empty issue_id; got %d", count)
	}
}
