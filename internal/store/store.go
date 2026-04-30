package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

const migration0001 = `
CREATE TABLE IF NOT EXISTS sessions (
  id INTEGER PRIMARY KEY,
  team_name TEXT UNIQUE,
  tg_chat_id INTEGER,
  cwd TEXT,
  created_at TEXT,
  last_started_at TEXT
);

CREATE TABLE IF NOT EXISTS messages (
  id INTEGER PRIMARY KEY,
  direction TEXT CHECK(direction IN ('in','out')),
  tg_message_id INTEGER,
  tg_user_id INTEGER,
  team_message_id TEXT,
  body TEXT,
  created_at TEXT
);

CREATE TABLE IF NOT EXISTS commands (
  id INTEGER PRIMARY KEY,
  tg_user_id INTEGER,
  cmd TEXT,
  args TEXT,
  status TEXT,
  result TEXT,
  created_at TEXT,
  completed_at TEXT
);

CREATE TABLE IF NOT EXISTS event_cursor (
  team_name TEXT PRIMARY KEY,
  after_event_id TEXT,
  updated_at TEXT
);

CREATE TABLE IF NOT EXISTS tg_updates (
  update_id INTEGER PRIMARY KEY,
  tg_user_id INTEGER,
  tg_chat_id INTEGER,
  body TEXT,
  received_at TEXT,
  processed_at TEXT
);

CREATE TABLE IF NOT EXISTS kv (
  key TEXT PRIMARY KEY,
  value TEXT,
  updated_at TEXT
);
`

const migration0002 = `
CREATE TABLE IF NOT EXISTS autopilot_jobs (
  agent_session_id TEXT PRIMARY KEY,
  issue_id         TEXT NOT NULL,
  issue_identifier TEXT,
  state            TEXT NOT NULL,
  worktree_path    TEXT,
  branch           TEXT,
  pr_url           TEXT,
  error            TEXT,
  started_at       TEXT NOT NULL,
  finished_at      TEXT
);
CREATE INDEX IF NOT EXISTS autopilot_jobs_issue_id_idx
  ON autopilot_jobs(issue_id);
`

const migration0003 = `
CREATE TABLE IF NOT EXISTS linear_oauth (
  id              INTEGER PRIMARY KEY CHECK (id = 1),
  access_token    TEXT NOT NULL,
  refresh_token   TEXT,
  expires_at      TEXT,
  updated_at      TEXT NOT NULL
);
INSERT OR IGNORE INTO linear_oauth (id, access_token, refresh_token, expires_at, updated_at)
VALUES (1, '', '', '', '');
`

const migration0004 = `
ALTER TABLE autopilot_jobs ADD COLUMN stream_log_path TEXT;
`

const migration0005 = `
CREATE TABLE IF NOT EXISTS events_inbox (
  webhook_id    TEXT PRIMARY KEY,
  action        TEXT NOT NULL,
  session_id    TEXT NOT NULL,
  issue_id      TEXT,
  payload_json  TEXT NOT NULL,
  status        TEXT NOT NULL,
  attempts      INTEGER NOT NULL DEFAULT 0,
  received_at   INTEGER NOT NULL,
  started_at    INTEGER,
  finished_at   INTEGER,
  last_error    TEXT
);
CREATE INDEX IF NOT EXISTS events_inbox_status_received
  ON events_inbox(status, received_at);
`

const migration0006 = `
ALTER TABLE autopilot_jobs ADD COLUMN claude_session_id TEXT;
`

type migration struct {
	Version int
	SQL     string
}

var migrations = []migration{
	{1, migration0001},
	{2, migration0002},
	{3, migration0003},
	{4, migration0004},
	{5, migration0005},
	{6, migration0006},
}

func tableExists(db *sql.DB, name string) bool {
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n)
	return n > 0
}

func columnExists(db *sql.DB, table, column string) bool {
	var n int
	db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name=?`,
		table, column,
	).Scan(&n)
	return n > 0
}

func loadAppliedVersions(db *sql.DB) (map[int]bool, error) {
	rows, err := db.Query(`SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int]bool)
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out[v] = true
	}
	return out, rows.Err()
}

func backfillMigrations(db *sql.DB) error {
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}

	if !tableExists(db, "sessions") {
		return nil
	}

	for _, v := range []int{1, 2, 3, 4, 5, 6} {
		applied := false
		switch v {
		case 1:
			applied = tableExists(db, "sessions")
		case 2:
			applied = tableExists(db, "autopilot_jobs")
		case 3:
			applied = tableExists(db, "linear_oauth")
		case 4:
			applied = columnExists(db, "autopilot_jobs", "stream_log_path")
		case 5:
			applied = tableExists(db, "events_inbox")
		case 6:
			applied = columnExists(db, "autopilot_jobs", "claude_session_id")
		}
		if applied {
			_, err := db.Exec(
				`INSERT INTO schema_migrations(version, applied_at) VALUES(?, ?)`,
				v, time.Now().UTC().Format(time.RFC3339),
			)
			if err != nil {
				return fmt.Errorf("backfill version %d: %w", v, err)
			}
		}
	}
	return nil
}

func applyMigrations(db *sql.DB) error {
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY,
			applied_at TEXT NOT NULL
		)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	if err := backfillMigrations(db); err != nil {
		return err
	}

	applied, err := loadAppliedVersions(db)
	if err != nil {
		return err
	}

	for _, m := range migrations {
		if applied[m.Version] {
			continue
		}
		if _, err := db.Exec(m.SQL); err != nil {
			return fmt.Errorf("apply migration %d: %w", m.Version, err)
		}
		if _, err := db.Exec(
			`INSERT INTO schema_migrations(version, applied_at) VALUES(?, ?)`,
			m.Version, time.Now().UTC().Format(time.RFC3339),
		); err != nil {
			return fmt.Errorf("record migration %d: %w", m.Version, err)
		}
	}
	return nil
}

type Store struct {
	DB *sql.DB
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir sqlite dir: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// SQLite is single-writer; *sql.DB pooling spawns extra connections
	// under concurrent load. PRAGMAs (set just below) only apply to the
	// connection that ran the Exec, so a freshly-opened pool connection
	// would lose busy_timeout / WAL settings and fail-fast on contention.
	// Cap the pool at 1 connection: Go-side serializes access, every
	// query reuses the same conn, PRAGMAs persist. admiral's load is
	// trivial (1 webhook receiver + 1 worker + few task goroutines, all
	// sub-ms writes) — single conn is plenty and far simpler than a
	// connection-init hook.
	db.SetMaxOpenConns(1)
	// WAL avoids SQLITE_BUSY between concurrent cursor + messages writes
	// (event-push goroutine + inbound long-poll goroutine both write).
	if _, err := db.Exec(`PRAGMA journal_mode=WAL;`); err != nil {
		return nil, fmt.Errorf("set WAL: %w", err)
	}
	// busy_timeout makes concurrent writers wait instead of failing fast.
	// Default is 0ms — without this, two writers racing return SQLITE_BUSY
	// immediately. With MaxOpenConns=1 above, this PRAGMA is set on the
	// only conn admiral ever uses, so it's effectively permanent.
	if _, err := db.Exec(`PRAGMA busy_timeout=5000;`); err != nil {
		return nil, fmt.Errorf("set busy_timeout: %w", err)
	}
	if err := applyMigrations(db); err != nil {
		return nil, fmt.Errorf("migrations: %w", err)
	}
	return &Store{DB: db}, nil
}

// Autopilot job state constants. RECEIVED -> EXECUTING -> DONE|FAILED|TIMED_OUT.
const (
	JobStateReceived               = "RECEIVED"
	JobStateExecuting              = "EXECUTING"
	JobStateDone                   = "DONE"
	JobStateFailed                 = "FAILED"
	JobStateTimedOut               = "TIMED_OUT"
	JobStateDoneThreadInconsistent = "DONE_THREAD_INCONSISTENT"
)

type AutopilotJob struct {
	AgentSessionID  string
	IssueID         string
	IssueIdentifier string
	State           string
	WorktreePath    string
	Branch          string
	PRURL           string
	Error           string
	StartedAt       string
	FinishedAt      string
	StreamLogPath   string
	ClaudeSessionID string
}

// ClaimAutopilotJob inserts a RECEIVED row for sessionID iff no row exists.
// AgentSession UUIDs are unique per Linear-side session, so a duplicate
// webhook delivery (Linear retries on non-2xx) re-claims a no-op. Returns
// (true, nil) when the caller now owns the job; (false, nil) when the
// session is already claimed (in any state, including DONE/FAILED — we
// don't auto-restart a closed session).
func (s *Store) ClaimAutopilotJob(sessionID, issueID, identifier string) (bool, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.DB.Exec(`
		INSERT OR IGNORE INTO autopilot_jobs(
			agent_session_id, issue_id, issue_identifier, state, started_at
		) VALUES(?, ?, ?, ?, ?)
	`, sessionID, issueID, identifier, JobStateReceived, now)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// AnyAutopilotJobActive reports whether any job is in a non-terminal state.
// Used by the orchestrator's single-flight gate (alongside the in-process
// mutex).
func (s *Store) AnyAutopilotJobActive() (bool, string, error) {
	var sessionID string
	err := s.DB.QueryRow(`
		SELECT agent_session_id FROM autopilot_jobs
		WHERE state NOT IN (?, ?, ?, ?)
		ORDER BY started_at ASC LIMIT 1
	`, JobStateDone, JobStateFailed, JobStateTimedOut, JobStateDoneThreadInconsistent).Scan(&sessionID)
	if err == sql.ErrNoRows {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	return true, sessionID, nil
}

func (s *Store) UpdateAutopilotJob(sessionID string, fn func(*AutopilotJob)) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var j AutopilotJob
	err = tx.QueryRow(`
		SELECT agent_session_id, issue_id, issue_identifier, state,
		       COALESCE(worktree_path,''), COALESCE(branch,''),
		       COALESCE(pr_url,''), COALESCE(error,''),
		       started_at, COALESCE(finished_at,''),
		       COALESCE(stream_log_path,''),
		       COALESCE(claude_session_id,'')
		FROM autopilot_jobs WHERE agent_session_id=?
	`, sessionID).Scan(&j.AgentSessionID, &j.IssueID, &j.IssueIdentifier, &j.State,
		&j.WorktreePath, &j.Branch, &j.PRURL, &j.Error, &j.StartedAt, &j.FinishedAt,
		&j.StreamLogPath, &j.ClaudeSessionID)
	if err != nil {
		return err
	}
	fn(&j)
	_, err = tx.Exec(`
		UPDATE autopilot_jobs
		SET issue_identifier=?, state=?, worktree_path=?, branch=?, pr_url=?,
		    error=?, finished_at=?, stream_log_path=?, claude_session_id=?
		WHERE agent_session_id=?
	`, j.IssueIdentifier, j.State, nullIfEmpty(j.WorktreePath), nullIfEmpty(j.Branch),
		nullIfEmpty(j.PRURL), nullIfEmpty(j.Error), nullIfEmpty(j.FinishedAt),
		nullIfEmpty(j.StreamLogPath), nullIfEmpty(j.ClaudeSessionID), sessionID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) GetAutopilotJob(sessionID string) (*AutopilotJob, error) {
	var j AutopilotJob
	err := s.DB.QueryRow(`
		SELECT agent_session_id, issue_id, issue_identifier, state,
		       COALESCE(worktree_path,''), COALESCE(branch,''),
		       COALESCE(pr_url,''), COALESCE(error,''),
		       started_at, COALESCE(finished_at,''),
		       COALESCE(stream_log_path,''),
		       COALESCE(claude_session_id,'')
		FROM autopilot_jobs WHERE agent_session_id=?
	`, sessionID).Scan(&j.AgentSessionID, &j.IssueID, &j.IssueIdentifier, &j.State,
		&j.WorktreePath, &j.Branch, &j.PRURL, &j.Error, &j.StartedAt, &j.FinishedAt,
		&j.StreamLogPath, &j.ClaudeSessionID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &j, err
}

// GetLastAutopilotJob returns the most recent autopilot job by started_at.
// Returns (nil, nil) when the table is empty.
func (s *Store) GetLastAutopilotJob() (*AutopilotJob, error) {
	var j AutopilotJob
	err := s.DB.QueryRow(`
		SELECT agent_session_id, issue_id, issue_identifier, state,
		       COALESCE(worktree_path,''), COALESCE(branch,''),
		       COALESCE(pr_url,''), COALESCE(error,''),
		       started_at, COALESCE(finished_at,''),
		       COALESCE(stream_log_path,''),
		       COALESCE(claude_session_id,'')
		FROM autopilot_jobs
		ORDER BY started_at DESC
		LIMIT 1
	`).Scan(&j.AgentSessionID, &j.IssueID, &j.IssueIdentifier, &j.State,
		&j.WorktreePath, &j.Branch, &j.PRURL, &j.Error, &j.StartedAt, &j.FinishedAt,
		&j.StreamLogPath, &j.ClaudeSessionID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &j, err
}

// GetLatestDoneJobByIssue returns the most recent DONE autopilot job for
// the given Linear issue ID. Returns (nil, nil) when no DONE job exists
// for that issue. Used by handleCreated to short-circuit re-spawning a
// task that's already been completed.
func (s *Store) GetLatestDoneJobByIssue(issueID string) (*AutopilotJob, error) {
	var j AutopilotJob
	err := s.DB.QueryRow(`
		SELECT agent_session_id, issue_id, issue_identifier, state,
		       COALESCE(worktree_path,''), COALESCE(branch,''),
		       COALESCE(pr_url,''), COALESCE(error,''),
		       started_at, COALESCE(finished_at,''),
		       COALESCE(stream_log_path,''),
		       COALESCE(claude_session_id,'')
		FROM autopilot_jobs
		WHERE issue_id=? AND state=?
		ORDER BY started_at DESC
		LIMIT 1
	`, issueID, JobStateDone).Scan(&j.AgentSessionID, &j.IssueID, &j.IssueIdentifier, &j.State,
		&j.WorktreePath, &j.Branch, &j.PRURL, &j.Error, &j.StartedAt, &j.FinishedAt,
		&j.StreamLogPath, &j.ClaudeSessionID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &j, err
}

// GetLatestTimedOutJobByIssue returns the most recent TIMED_OUT autopilot
// job for the given Linear issue ID. Used by handleCreated to detect
// resume scenarios. Returns (nil, nil) when no TIMED_OUT job exists.
func (s *Store) GetLatestTimedOutJobByIssue(issueID string) (*AutopilotJob, error) {
	var j AutopilotJob
	err := s.DB.QueryRow(`
		SELECT agent_session_id, issue_id, issue_identifier, state,
		       COALESCE(worktree_path,''), COALESCE(branch,''),
		       COALESCE(pr_url,''), COALESCE(error,''),
		       started_at, COALESCE(finished_at,''),
		       COALESCE(stream_log_path,''),
		       COALESCE(claude_session_id,'')
		FROM autopilot_jobs
		WHERE issue_id=? AND state=?
		ORDER BY started_at DESC
		LIMIT 1
	`, issueID, JobStateTimedOut).Scan(&j.AgentSessionID, &j.IssueID, &j.IssueIdentifier, &j.State,
		&j.WorktreePath, &j.Branch, &j.PRURL, &j.Error, &j.StartedAt, &j.FinishedAt,
		&j.StreamLogPath, &j.ClaudeSessionID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &j, err
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (s *Store) Close() error {
	return s.DB.Close()
}

// NewForTest creates a Store wrapper around an already-open *sql.DB.
// Caller is responsible for applying migrations before use.
// This is for unit tests only — not part of the public API.
func NewForTest(db *sql.DB) *Store {
	return &Store{DB: db}
}

// LinearOAuthToken holds the persisted OAuth token set.
type LinearOAuthToken struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    string // RFC3339 UTC; empty if server didn't return one
	UpdatedAt    string // RFC3339 UTC
}

// GetLinearOAuthToken returns the current token row (id=1). Returns
// (nil, nil) when no token has been persisted yet.
func (s *Store) GetLinearOAuthToken() (*LinearOAuthToken, error) {
	var t LinearOAuthToken
	err := s.DB.QueryRow(`
		SELECT access_token, COALESCE(refresh_token,''), COALESCE(expires_at,''), updated_at
		FROM linear_oauth WHERE id=1
	`).Scan(&t.AccessToken, &t.RefreshToken, &t.ExpiresAt, &t.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// SaveLinearOAuthToken upserts the token row (id=1). Passing an empty
// refreshToken leaves the stored refresh_token unchanged.
func (s *Store) SaveLinearOAuthToken(accessToken, refreshToken, expiresAt string) error {
	_, err := s.DB.Exec(`
		UPDATE linear_oauth
		SET access_token=?,
		    refresh_token=CASE WHEN ?='' THEN refresh_token ELSE ? END,
		    expires_at=?,
		    updated_at=?
		WHERE id=1
	`, accessToken, refreshToken, refreshToken, expiresAt, time.Now().UTC().Format(time.RFC3339))
	return err
}

func (s *Store) GetCursor(teamName string) (string, error) {
	var cur string
	err := s.DB.QueryRow(
		`SELECT after_event_id FROM event_cursor WHERE team_name = ?`, teamName,
	).Scan(&cur)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return cur, err
}

func (s *Store) SetCursor(teamName, afterEventID string) error {
	_, err := s.DB.Exec(`
		INSERT INTO event_cursor(team_name, after_event_id, updated_at)
		VALUES(?, ?, ?)
		ON CONFLICT(team_name) DO UPDATE SET after_event_id=excluded.after_event_id, updated_at=excluded.updated_at
	`, teamName, afterEventID, time.Now().UTC().Format(time.RFC3339))
	return err
}

func (s *Store) RecordMessage(direction string, tgMessageID, tgUserID int64, teamMessageID, body string) error {
	_, err := s.DB.Exec(`
		INSERT INTO messages(direction, tg_message_id, tg_user_id, team_message_id, body, created_at)
		VALUES(?, ?, ?, ?, ?, ?)
	`, direction, tgMessageID, tgUserID, teamMessageID, body, time.Now().UTC().Format(time.RFC3339))
	return err
}

func (s *Store) RecordCommand(tgUserID int64, cmd, args, status, result string) error {
	_, err := s.DB.Exec(`
		INSERT INTO commands(tg_user_id, cmd, args, status, result, created_at, completed_at)
		VALUES(?, ?, ?, ?, ?, ?, ?)
	`, tgUserID, cmd, args, status, result,
		time.Now().UTC().Format(time.RFC3339),
		time.Now().UTC().Format(time.RFC3339))
	return err
}

// InsertTGUpdate inserts a TG update row if and only if update_id is new.
// Returns true when a new row was inserted (update is fresh), false when
// it was already present (duplicate — drop silently).
func (s *Store) InsertTGUpdate(updateID, tgUserID, tgChatID int64, body string) (bool, error) {
	res, err := s.DB.Exec(`
		INSERT OR IGNORE INTO tg_updates(update_id, tg_user_id, tg_chat_id, body, received_at)
		VALUES(?, ?, ?, ?, ?)
	`, updateID, tgUserID, tgChatID, body, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// MarkTGUpdateProcessed + RecordOutboundMessage atomically: same tx.
func (s *Store) MarkTGUpdateProcessed(updateID int64, teamMessageID, body string, tgUserID int64) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := tx.Exec(
		`UPDATE tg_updates SET processed_at=? WHERE update_id=?`,
		now, updateID,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`INSERT INTO messages(direction, tg_message_id, tg_user_id, team_message_id, body, created_at)
		 VALUES('out', ?, ?, ?, ?, ?)`,
		updateID, tgUserID, teamMessageID, body, now,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// UnprocessedTGUpdates returns all rows where processed_at IS NULL, ordered by update_id ascending.
type PendingTGUpdate struct {
	UpdateID int64
	UserID   int64
	ChatID   int64
	Body     string
}

func (s *Store) UnprocessedTGUpdates() ([]PendingTGUpdate, error) {
	rows, err := s.DB.Query(`
		SELECT update_id, tg_user_id, tg_chat_id, body
		FROM tg_updates WHERE processed_at IS NULL ORDER BY update_id ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingTGUpdate
	for rows.Next() {
		var p PendingTGUpdate
		if err := rows.Scan(&p.UpdateID, &p.UserID, &p.ChatID, &p.Body); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// MarkTGUpdateDone marks an update processed without writing a messages row
// (used for command-only updates or non-deliverable failures we don't want to retry).
func (s *Store) MarkTGUpdateDone(updateID int64) error {
	_, err := s.DB.Exec(
		`UPDATE tg_updates SET processed_at=? WHERE update_id=?`,
		time.Now().UTC().Format(time.RFC3339), updateID,
	)
	return err
}

// KVGet / KVSet — simple string kv store used for wall-clock markers like last_successful_poll_at.
func (s *Store) KVGet(key string) (string, error) {
	var v string
	err := s.DB.QueryRow(`SELECT value FROM kv WHERE key=?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

func (s *Store) KVSet(key, value string) error {
	_, err := s.DB.Exec(`
		INSERT INTO kv(key, value, updated_at) VALUES(?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at
	`, key, value, time.Now().UTC().Format(time.RFC3339))
	return err
}

// --- events_inbox ---

// EventInboxRow represents a queued webhook event awaiting processing.
type EventInboxRow struct {
	WebhookID    string
	Action      string
	SessionID   string
	IssueID     string
	PayloadJSON string
	Status      string
	Attempts    int
	ReceivedAt  time.Time
	StartedAt   *time.Time
	FinishedAt  *time.Time
	LastError   string
}

// EnqueueEvent inserts a pending row. Returns true when a fresh row was
// inserted, false when webhook_id already existed (Linear retry / dup).
func (s *Store) EnqueueEvent(webhookID, action, sessionID, issueID, payloadJSON string) (bool, error) {
	now := time.Now().UTC()
	result, err := s.DB.Exec(`
		INSERT OR IGNORE INTO events_inbox(
			webhook_id, action, session_id, issue_id, payload_json,
			status, attempts, received_at
		) VALUES(?, ?, ?, ?, ?, 'pending', 0, ?)
	`, webhookID, action, sessionID, issueID, payloadJSON, now.Unix())
	if err != nil {
		return false, err
	}
	// Check if we actually inserted (RowsAffected=0 means conflict)
	n, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ClaimNextPendingEvent atomically picks one pending row and marks it
// processing. It skips sessions that already have a row in 'processing' state,
// ensuring at most one in-flight job per session at any time (per-session FIFO).
// Returns (nil, nil) when nothing pending.
func (s *Store) ClaimNextPendingEvent() (*EventInboxRow, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// BEGIN IMMEDIATE acquires a write lock immediately, preventing
	// concurrent claims on the same row.
	var row EventInboxRow
	var receivedUnix, startedUnix, finishedUnix int64
	err = tx.QueryRow(`
		SELECT webhook_id, action, session_id, issue_id, payload_json,
		       status, attempts, received_at,
		       COALESCE(started_at, 0),
		       COALESCE(finished_at, 0),
		       COALESCE(last_error, '')
		FROM events_inbox
		WHERE status = 'pending'
		  AND session_id NOT IN (
		      SELECT session_id FROM events_inbox WHERE status = 'processing'
		  )
		ORDER BY received_at ASC
		LIMIT 1
	`).Scan(
		&row.WebhookID, &row.Action, &row.SessionID, &row.IssueID, &row.PayloadJSON,
		&row.Status, &row.Attempts, &receivedUnix, &startedUnix, &finishedUnix, &row.LastError,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	row.ReceivedAt = time.Unix(receivedUnix, 0).UTC()
	if startedUnix > 0 {
		t := time.Unix(startedUnix, 0).UTC()
		row.StartedAt = &t
	}
	if finishedUnix > 0 {
		t := time.Unix(finishedUnix, 0).UTC()
		row.FinishedAt = &t
	}

	now := time.Now().UTC().Unix()
	_, err = tx.Exec(`
		UPDATE events_inbox
		SET status='processing', attempts=attempts+1, started_at=?
		WHERE webhook_id=? AND status='pending'
	`, now, row.WebhookID)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	row.Status = "processing"
	row.Attempts++
	started := time.Unix(now, 0).UTC()
	row.StartedAt = &started

	return &row, nil
}

// MarkEventDone sets status=done and finished_at.
func (s *Store) MarkEventDone(webhookID string) error {
	now := time.Now().UTC().Unix()
	_, err := s.DB.Exec(`
		UPDATE events_inbox SET status='done', finished_at=? WHERE webhook_id=?
	`, now, webhookID)
	return err
}

// MarkEventFailed sets status=failed (dead letter) with last_error;
// retryAvailable=true means status returns to 'pending' for retry,
// false means status='failed' permanently.
func (s *Store) MarkEventFailed(webhookID string, errMsg string, retryAvailable bool) error {
	now := time.Now().UTC().Unix()
	var status string
	if retryAvailable {
		status = "pending"
	} else {
		status = "failed"
	}
	_, err := s.DB.Exec(`
		UPDATE events_inbox SET status=?, last_error=?, finished_at=? WHERE webhook_id=?
	`, status, errMsg, now, webhookID)
	return err
}

// CountPendingEvents returns count of pending+processing rows for backlog observability.
func (s *Store) CountPendingEvents() (int, error) {
	var count int
	err := s.DB.QueryRow(`
		SELECT COUNT(*) FROM events_inbox WHERE status IN ('pending', 'processing')
	`).Scan(&count)
	return count, err
}
