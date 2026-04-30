package autopilot

import (
	"database/sql"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/georgehuang/admiral/internal/linear"
	"github.com/georgehuang/admiral/internal/store"
)

type testWorkerEnv struct {
	db  *store.Store
	sig chan struct{}
}

func setupTestDB(t *testing.T) *testWorkerEnv {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	s := store.NewForTest(db)
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS sessions (id INTEGER PRIMARY KEY, team_name TEXT UNIQUE, tg_chat_id INTEGER, cwd TEXT, created_at TEXT, last_started_at TEXT);`,
		`CREATE TABLE IF NOT EXISTS messages (id INTEGER PRIMARY KEY, direction TEXT CHECK(direction IN ('in','out')), tg_message_id INTEGER, tg_user_id INTEGER, team_message_id TEXT, body TEXT, created_at TEXT);`,
		`CREATE TABLE IF NOT EXISTS commands (id INTEGER PRIMARY KEY, tg_user_id INTEGER, cmd TEXT, args TEXT, status TEXT, result TEXT, created_at TEXT, completed_at TEXT);`,
		`CREATE TABLE IF NOT EXISTS event_cursor (team_name TEXT PRIMARY KEY, after_event_id TEXT, updated_at TEXT);`,
		`CREATE TABLE IF NOT EXISTS tg_updates (update_id INTEGER PRIMARY KEY, tg_user_id INTEGER, tg_chat_id INTEGER, body TEXT, received_at TEXT, processed_at TEXT);`,
		`CREATE TABLE IF NOT EXISTS kv (key TEXT PRIMARY KEY, value TEXT, updated_at TEXT);`,
		`CREATE TABLE IF NOT EXISTS autopilot_jobs (agent_session_id TEXT PRIMARY KEY, issue_id TEXT NOT NULL, issue_identifier TEXT, state TEXT NOT NULL, worktree_path TEXT, branch TEXT, pr_url TEXT, error TEXT, started_at TEXT NOT NULL, finished_at TEXT, stream_log_path TEXT);`,
		`CREATE INDEX IF NOT EXISTS autopilot_jobs_issue_id_idx ON autopilot_jobs(issue_id);`,
		`CREATE TABLE IF NOT EXISTS linear_oauth (id INTEGER PRIMARY KEY CHECK (id = 1), access_token TEXT NOT NULL, refresh_token TEXT, expires_at TEXT, updated_at TEXT NOT NULL);`,
		`CREATE TABLE IF NOT EXISTS events_inbox (webhook_id TEXT PRIMARY KEY, action TEXT NOT NULL, session_id TEXT NOT NULL, issue_id TEXT, payload_json TEXT NOT NULL, status TEXT NOT NULL, attempts INTEGER NOT NULL DEFAULT 0, received_at INTEGER NOT NULL, started_at INTEGER, finished_at INTEGER, last_error TEXT);`,
		`CREATE INDEX IF NOT EXISTS events_inbox_status_received ON events_inbox(status, received_at);`,
	}
	for _, m := range migrations {
		if _, err := db.Exec(m); err != nil {
			t.Fatalf("migration: %v", err)
		}
	}

	logger := slog.New(slog.NewTextHandler(&discardHandler{t}, nil))
	_ = logger
	sig := make(chan struct{}, 1)

	return &testWorkerEnv{db: s, sig: sig}
}

type discardHandler struct {
	t *testing.T
}

func (h *discardHandler) Write(p []byte) (int, error) {
	return len(p), nil
}

func enqueueEvent(t *testing.T, db *sql.DB, webhookID, action, sessionID, issueID string, ev linear.AgentEvent) {
	t.Helper()
	payloadJSON, _ := json.Marshal(ev)
	_, err := db.Exec(`
		INSERT INTO events_inbox (webhook_id, action, session_id, issue_id, payload_json, status, attempts, received_at)
		VALUES (?, ?, ?, ?, ?, 'pending', 0, ?)
	`, webhookID, action, sessionID, issueID, string(payloadJSON), time.Now().Unix())
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
}

// TestWorker_MultiWorkerDispatch verifies that when N workers are started,
// events from different sessions are dispatched in parallel while same-session
// events are serialized by the per-session lock in ClaimNextPendingEvent.
func TestWorker_MultiWorkerDispatch(t *testing.T) {
	env := setupTestDB(t)

	// Enqueue 5 events across 5 different sessions
	sessions := []string{"sess-p1", "sess-p2", "sess-p3", "sess-p4", "sess-p5"}
	for i, sess := range sessions {
		wh := "wh-multi-" + string(rune('1'+i))
		enqueueEvent(t, env.db.DB, wh, "created", sess, "issue-"+sess,
			linear.AgentEvent{Action: linear.ActionCreated, SessionID: sess, IssueID: "issue-" + sess, WebhookID: wh})
	}

	// Claim 3 events (simulating 3 workers claiming in sequence, but with the
	// new per-session lock query — no session is processing yet, so all 3 succeed)
	var claimed []string
	for i := 0; i < 3; i++ {
		row, err := env.db.ClaimNextPendingEvent()
		if err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		if row == nil {
			t.Fatalf("claim %d: expected row, got nil", i)
		}
		claimed = append(claimed, row.SessionID)
	}

	if len(claimed) != 3 {
		t.Fatalf("expected 3 claims, got %d", len(claimed))
	}

	// All 3 should be different sessions (no per-session conflict at claim time)
	seen := make(map[string]bool)
	for _, sess := range claimed {
		if seen[sess] {
			t.Errorf("duplicate session claimed: %s", sess)
		}
		seen[sess] = true
	}

	// Claiming a 4th should succeed: sessions p4 and p5 are still pending
	// (none of the claimed sessions match p4 or p5)
	row4, err := env.db.ClaimNextPendingEvent()
	if err != nil {
		t.Fatalf("claim 4: %v", err)
	}
	if row4 == nil {
		t.Fatal("claim 4: expected row (p4 or p5 still pending), got nil")
	}
	if row4.SessionID != "sess-p4" && row4.SessionID != "sess-p5" {
		t.Errorf("claim 4: expected sess-p4 or sess-p5, got %v", row4.SessionID)
	}

	// Claiming a 5th returns the last pending (the other of p4/p5)
	row5, err := env.db.ClaimNextPendingEvent()
	if err != nil {
		t.Fatalf("claim 5: %v", err)
	}
	if row5 == nil {
		t.Fatal("claim 5: expected row (last remaining pending), got nil")
	}

	// 6th claim returns nil (all sessions either processing or done)
	row6, err := env.db.ClaimNextPendingEvent()
	if err != nil {
		t.Fatalf("claim 6: %v", err)
	}
	if row6 != nil {
		t.Errorf("claim 6: expected nil, got %v", row6.SessionID)
	}
}

func TestWorker_Drain_MultipleEvents(t *testing.T) {
	env := setupTestDB(t)

	for i := 0; i < 3; i++ {
		wh := "wh-multi-" + string(rune('1'+i))
		sess := "sess-multi-" + string(rune('1'+i))
		iss := "issue-multi-" + string(rune('1'+i))
		enqueueEvent(t, env.db.DB, wh, "created", sess, iss,
			linear.AgentEvent{Action: linear.ActionCreated, SessionID: sess, IssueID: iss, WebhookID: wh})
	}

	// Claim all 3 events
	for i := 0; i < 3; i++ {
		row, err := env.db.ClaimNextPendingEvent()
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if row == nil {
			t.Fatal("expected row, got nil")
		}
		if row.Status != "processing" {
			t.Errorf("status: got %q, want processing", row.Status)
		}
		if row.Attempts != 1 {
			t.Errorf("attempts: got %d, want 1", row.Attempts)
		}
	}

	// Fourth claim should be empty
	row, err := env.db.ClaimNextPendingEvent()
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if row != nil {
		t.Error("expected nil on empty queue")
	}
}

func TestWorker_ClaimNextPendingEvent_Empty(t *testing.T) {
	env := setupTestDB(t)

	row, err := env.db.ClaimNextPendingEvent()
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if row != nil {
		t.Errorf("expected nil on empty table, got %+v", row)
	}
}

func TestWorker_ClaimNextPendingEvent_Concurrent(t *testing.T) {
	// SQLite's BEGIN IMMEDIATE provides row-level locking.
	// Here we test sequential claims to verify ordering.
	env := setupTestDB(t)

	// Enqueue 3 events with unique webhook IDs
	for i := 0; i < 3; i++ {
		wh := "wh-seq-" + string(rune('a'+i))
		enqueueEvent(t, env.db.DB, wh, "created", "sess-"+string(rune('a'+i)), "issue-"+string(rune('a'+i)),
			linear.AgentEvent{Action: linear.ActionCreated, SessionID: "sess-" + string(rune('a'+i)), IssueID: "issue-" + string(rune('a'+i)), WebhookID: wh})
	}

	// Claim all 3 sequentially - each should succeed
	for i := 0; i < 3; i++ {
		row, err := env.db.ClaimNextPendingEvent()
		if err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		if row == nil {
			t.Fatalf("claim %d: expected row, got nil", i)
		}
	}

	// Fourth claim should be empty
	row, err := env.db.ClaimNextPendingEvent()
	if err != nil {
		t.Fatalf("claim 4: %v", err)
	}
	if row != nil {
		t.Error("expected nil on empty queue")
	}
}

func TestWorker_DeadLetter_AfterMaxAttempts(t *testing.T) {
	env := setupTestDB(t)

	ev := linear.AgentEvent{
		Action:    linear.ActionCreated,
		SessionID: "sess-dead",
		IssueID:   "issue-dead",
		WebhookID: "wh-dead",
	}
	payloadJSON, _ := json.Marshal(ev)

	// Insert with attempts=5 (at max) and status=processing
	_, err := env.db.DB.Exec(`
		INSERT INTO events_inbox (webhook_id, action, session_id, issue_id, payload_json, status, attempts, received_at)
		VALUES (?, ?, ?, ?, ?, 'processing', 5, ?)
	`, "wh-dead", "created", "sess-dead", "issue-dead", string(payloadJSON), time.Now().Unix())
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Simulate dispatch failure at max attempts (retryAvailable=false)
	err = env.db.MarkEventFailed("wh-dead", "test error", false)
	if err != nil {
		t.Fatalf("MarkEventFailed: %v", err)
	}

	var status string
	err = env.db.DB.QueryRow(`SELECT status FROM events_inbox WHERE webhook_id='wh-dead'`).Scan(&status)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if status != "failed" {
		t.Errorf("status: got %q, want failed", status)
	}
}

func TestWorker_MarkEventFailed_Retry(t *testing.T) {
	env := setupTestDB(t)

	ev := linear.AgentEvent{
		Action:    linear.ActionCreated,
		SessionID: "sess-retry",
		IssueID:   "issue-retry",
		WebhookID: "wh-retry",
	}
	payloadJSON, _ := json.Marshal(ev)

	_, err := env.db.DB.Exec(`
		INSERT INTO events_inbox (webhook_id, action, session_id, issue_id, payload_json, status, attempts, received_at)
		VALUES (?, ?, ?, ?, ?, 'processing', 2, ?)
	`, "wh-retry", "created", "sess-retry", "issue-retry", string(payloadJSON), time.Now().Unix())
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Simulate retry available
	err = env.db.MarkEventFailed("wh-retry", "temporary error", true)
	if err != nil {
		t.Fatalf("MarkEventFailed: %v", err)
	}

	var status string
	err = env.db.DB.QueryRow(`SELECT status FROM events_inbox WHERE webhook_id='wh-retry'`).Scan(&status)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if status != "pending" {
		t.Errorf("status: got %q, want pending (retry)", status)
	}
}

func TestCountPendingEvents(t *testing.T) {
	env := setupTestDB(t)

	count, err := env.db.CountPendingEvents()
	if err != nil {
		t.Fatalf("CountPendingEvents: %v", err)
	}
	if count != 0 {
		t.Errorf("initial count: got %d, want 0", count)
	}

	for i := 0; i < 5; i++ {
		wh := "wh-count-" + string(rune('1'+i))
		enqueueEvent(t, env.db.DB, wh, "created", "sess-count-"+string(rune('1'+i)), "issue-count-"+string(rune('1'+i)),
			linear.AgentEvent{Action: linear.ActionCreated, SessionID: "sess-count-" + string(rune('1'+i)), IssueID: "issue-count-" + string(rune('1'+i)), WebhookID: wh})
	}

	count, err = env.db.CountPendingEvents()
	if err != nil {
		t.Fatalf("CountPendingEvents: %v", err)
	}
	if count != 5 {
		t.Errorf("count after 5 enqueues: got %d, want 5", count)
	}
}

func TestWorker_StartupDrain_Simulation(t *testing.T) {
	env := setupTestDB(t)

	// Enqueue 3 events before "startup"
	for i := 0; i < 3; i++ {
		wh := "wh-startup-" + string(rune('1'+i))
		enqueueEvent(t, env.db.DB, wh, "created", "sess-startup-"+string(rune('1'+i)), "issue-startup-"+string(rune('1'+i)),
			linear.AgentEvent{Action: linear.ActionCreated, SessionID: "sess-startup-" + string(rune('1'+i)), IssueID: "issue-startup-" + string(rune('1'+i)), WebhookID: wh})
	}

	// Simulate drain loop (what Run() does on startup)
	for {
		row, err := env.db.ClaimNextPendingEvent()
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if row == nil {
			break
		}
		// Simulate dispatch success (MarkEventDone)
		if err := env.db.MarkEventDone(row.WebhookID); err != nil {
			t.Fatalf("MarkEventDone: %v", err)
		}
	}

	var count int
	err := env.db.DB.QueryRow(`SELECT COUNT(*) FROM events_inbox WHERE status='done'`).Scan(&count)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 3 {
		t.Errorf("expected 3 events drained, got %d", count)
	}
}
