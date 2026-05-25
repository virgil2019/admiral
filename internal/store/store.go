package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

const migration0007 = `
CREATE TABLE IF NOT EXISTS repos (
  team_id          TEXT PRIMARY KEY,
  team_name        TEXT NOT NULL,
  repo_dir         TEXT NOT NULL,
  base_branch      TEXT NOT NULL DEFAULT 'main',
  enabled          INTEGER NOT NULL DEFAULT 1,
  created_at       TEXT NOT NULL,
  updated_at       TEXT NOT NULL
);
`

// migration0008 switches repo routing from Linear team_id to Linear project_id.
// The team_id field cannot be reinterpreted as a project_id (different UUID
// namespace), so the table is dropped and reseeded from config on next boot.
const migration0008 = `
DROP TABLE IF EXISTS repos;
CREATE TABLE repos (
  project_id   TEXT PRIMARY KEY,
  project_name TEXT NOT NULL,
  repo_dir     TEXT NOT NULL,
  base_branch  TEXT NOT NULL DEFAULT 'main',
  enabled      INTEGER NOT NULL DEFAULT 1,
  created_at   TEXT NOT NULL,
  updated_at   TEXT NOT NULL
);
`

// migration0009 adds OAuth circuit-breaker state to linear_oauth so the
// daemon can detect a permanently-revoked refresh token, stop retrying
// against Linear, and dedupe the user-facing alert.
const migration0009 = `
ALTER TABLE linear_oauth ADD COLUMN auth_error      TEXT;
ALTER TABLE linear_oauth ADD COLUMN auth_error_at   TEXT;
ALTER TABLE linear_oauth ADD COLUMN notified_at     TEXT;
`

// migration0010 introduces admiral_tasks — the issue-keyed task model
// (GEO-50). One live row per Linear issue. The legacy autopilot_jobs
// table stays in place as audit log; lookups in PR-B will switch to
// admiral_tasks. PR-A only adds the schema; nothing yet writes to it
// outside the backfill in migration0012.
const migration0010 = `
CREATE TABLE IF NOT EXISTS admiral_tasks (
  issue_id              TEXT PRIMARY KEY,
  issue_identifier      TEXT,
  state                 TEXT NOT NULL,
  attempt_n             INTEGER NOT NULL DEFAULT 1,
  branch                TEXT,
  worktree_path         TEXT,
  pr_url                TEXT,
  claude_session_id     TEXT,
  last_event_session_id TEXT,
  started_at            TEXT NOT NULL,
  finished_at           TEXT,
  error                 TEXT,
  stream_log_path       TEXT
);
CREATE INDEX IF NOT EXISTS admiral_tasks_state_started
  ON admiral_tasks(state, started_at);
`

// migration0011 introduces admiral_task_history — append-only log of
// superseded admiral_tasks rows (after /rerun in the new model). Empty
// at install time; PR-B populates it on /rerun. Read by /rerun protocol
// to compute the next attempt_n + cross-link old PRs.
const migration0011 = `
CREATE TABLE IF NOT EXISTS admiral_task_history (
  history_id            INTEGER PRIMARY KEY AUTOINCREMENT,
  issue_id              TEXT NOT NULL,
  attempt_n             INTEGER NOT NULL,
  state                 TEXT NOT NULL,
  branch                TEXT,
  worktree_path         TEXT,
  pr_url                TEXT,
  claude_session_id     TEXT,
  agent_session_ids     TEXT,
  started_at            TEXT NOT NULL,
  finished_at           TEXT,
  error                 TEXT,
  stream_log_path       TEXT,
  superseded_at         TEXT NOT NULL,
  superseded_reason     TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS admiral_task_history_issue_attempt
  ON admiral_task_history(issue_id, attempt_n);
`

// migration0012 backfills admiral_tasks from autopilot_jobs: for each
// issue_id with prior activity, the row with the latest started_at
// becomes the live admiral_tasks row. Older rows stay in autopilot_jobs
// only — admiral_task_history starts empty (it captures supersessions
// going forward, not historic state we never had a "rerun" event for).
//
// Idempotency: ON CONFLICT(issue_id) DO NOTHING — re-applying this
// migration on a partially-backfilled DB is safe.
const migration0012 = `
INSERT INTO admiral_tasks (
  issue_id, issue_identifier, state, attempt_n, branch, worktree_path,
  pr_url, claude_session_id, last_event_session_id, started_at,
  finished_at, error, stream_log_path
)
SELECT
  j.issue_id,
  j.issue_identifier,
  j.state,
  1,
  j.branch,
  j.worktree_path,
  j.pr_url,
  j.claude_session_id,
  j.agent_session_id,
  j.started_at,
  j.finished_at,
  j.error,
  j.stream_log_path
FROM autopilot_jobs j
INNER JOIN (
  SELECT issue_id, MAX(started_at) AS max_started
  FROM autopilot_jobs
  WHERE issue_id != ''
  GROUP BY issue_id
) latest ON j.issue_id = latest.issue_id AND j.started_at = latest.max_started
WHERE j.issue_id != ''
ON CONFLICT(issue_id) DO NOTHING;
`

// migration0013 widens events_inbox to support multiple webhook sources
// (linear, github, …). source defaults to 'linear' so pre-existing rows
// keep their semantics. comment_id is nullable; the partial unique index
// gives per-source dedup for sources that carry a comment id (e.g. a
// GitHub review comment) without constraining linear rows that don't.
const migration0013 = `
ALTER TABLE events_inbox ADD COLUMN source TEXT NOT NULL DEFAULT 'linear';
ALTER TABLE events_inbox ADD COLUMN comment_id TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS events_inbox_source_comment_idx
  ON events_inbox(source, comment_id) WHERE comment_id IS NOT NULL;
`

// migration0014 adds blocker_ids to admiral_tasks for tracking the Linear
// issue IDs that are blocking a task in BLOCKED state.
const migration0014 = `
ALTER TABLE admiral_tasks ADD COLUMN blocker_ids TEXT;
`

// migration0015 introduces the pending_questions table for the async
// ask_user HITL flow and adds pending_question_id to admiral_tasks so
// the orchestrator can correlate an incoming Linear reply with the open
// question that is holding the task in AWAITING_INPUT state.
const migration0015 = `
CREATE TABLE IF NOT EXISTS pending_questions (
    id                   TEXT PRIMARY KEY,
    issue_id             TEXT NOT NULL,
    issue_identifier     TEXT NOT NULL DEFAULT '',
    claude_session_id    TEXT NOT NULL,
    last_event_session_id TEXT NOT NULL DEFAULT '',
    worktree_path        TEXT NOT NULL DEFAULT '',
    question             TEXT NOT NULL,
    options_json         TEXT NOT NULL DEFAULT '[]',
    created_at           TEXT NOT NULL,
    answered_at          TEXT,
    answer               TEXT
);
ALTER TABLE admiral_tasks ADD COLUMN pending_question_id TEXT;
`

// migration0016 adds the per-repo opt-in flag the admiral-discoverer
// service reads to decide which Linear projects it is allowed to scan.
// Default 0 (off) so existing deploys never auto-pick until the operator
// flips the toggle in the admin UI.
const migration0016 = `
ALTER TABLE repos ADD COLUMN auto_pick_enabled INTEGER NOT NULL DEFAULT 0;
`

// migration0017 records every Linear issue admiral-discoverer has
// elected to self-assign. The dedup signal is picked_state: when the
// issue's current Linear state matches picked_state, the discoverer
// skips it; when the state changes (an "external reset" signal from a
// human), the row is overwritten and the issue may be picked again.
const migration0017 = `
CREATE TABLE IF NOT EXISTS discoverer_picks (
    issue_id          TEXT PRIMARY KEY,
    issue_identifier  TEXT NOT NULL,
    picked_at         TEXT NOT NULL,
    picked_state      TEXT NOT NULL,
    updated_at        TEXT NOT NULL
);
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
	{7, migration0007},
	{8, migration0008},
	{9, migration0009},
	{10, migration0010},
	{11, migration0011},
	{12, migration0012},
	{13, migration0013},
	{14, migration0014},
	{15, migration0015},
	{16, migration0016},
	{17, migration0017},
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
	DB   *sql.DB
	Path string // absolute path to the SQLite file
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
	return &Store{DB: db, Path: path}, nil
}

// Autopilot job state constants. RECEIVED -> EXECUTING -> DONE|FAILED|TIMED_OUT.
// CANCELLED is set on jobs short-circuited at pre-flight (e.g. duplicate
// dispatch when another session for the same issue is already active).
const (
	JobStateReceived               = "RECEIVED"
	JobStateExecuting              = "EXECUTING"
	JobStateDone                   = "DONE"
	JobStateFailed                 = "FAILED"
	JobStateTimedOut               = "TIMED_OUT"
	JobStateDoneThreadInconsistent = "DONE_THREAD_INCONSISTENT"
	JobStateCancelled              = "CANCELLED"
	JobStateBlocked                = "BLOCKED"
	JobStateAwaitingInput          = "AWAITING_INPUT"
	JobStateAborted                = "ABORTED"
	// JobStateDoneMerged is the true terminal state for a successful
	// admiral task: the PR admiral opened was merged into the base
	// branch on GitHub. Written by admiral-discoverer (admiral-autopilot
	// itself does not observe merge events) when it polls a DONE task's
	// PR and finds merged_at non-empty.
	JobStateDoneMerged = "DONE_MERGED"
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
		WHERE state NOT IN (?, ?, ?, ?, ?)
		ORDER BY started_at ASC LIMIT 1
	`, JobStateDone, JobStateFailed, JobStateTimedOut, JobStateDoneThreadInconsistent, JobStateCancelled).Scan(&sessionID)
	if err == sql.ErrNoRows {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	return true, sessionID, nil
}

// FindActiveJobByIssue returns the most recent non-terminal autopilot_jobs
// row for the given Linear issue, excluding the row identified by
// excludeSessionID (typically the caller's own session, which has just been
// claimed via ClaimAutopilotJob and would otherwise match itself).
//
// Returns (nil, nil) when no such row exists. Used by the pre-flight
// duplicate-dispatch short-circuit (GEO-47): if a prior session for the
// same issue is still in flight, the new dispatch is cancelled.
func (s *Store) FindActiveJobByIssue(issueID, excludeSessionID string) (*AutopilotJob, error) {
	var j AutopilotJob
	err := s.DB.QueryRow(`
		SELECT agent_session_id, issue_id, issue_identifier, state,
		       COALESCE(worktree_path,''), COALESCE(branch,''),
		       COALESCE(pr_url,''), COALESCE(error,''),
		       started_at, COALESCE(finished_at,''),
		       COALESCE(stream_log_path,''),
		       COALESCE(claude_session_id,'')
		FROM autopilot_jobs
		WHERE issue_id=?
		  AND agent_session_id != ?
		  AND state NOT IN (?, ?, ?, ?, ?)
		ORDER BY started_at DESC
		LIMIT 1
	`, issueID, excludeSessionID,
		JobStateDone, JobStateFailed, JobStateTimedOut, JobStateDoneThreadInconsistent, JobStateCancelled,
	).Scan(&j.AgentSessionID, &j.IssueID, &j.IssueIdentifier, &j.State,
		&j.WorktreePath, &j.Branch, &j.PRURL, &j.Error, &j.StartedAt, &j.FinishedAt,
		&j.StreamLogPath, &j.ClaudeSessionID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &j, err
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

// HasAnyAutopilotJobForIssue reports whether autopilot_jobs has at least
// one row for the given issue, regardless of state. Used by the GEO-50
// dispatch to decide whether an event is "first-time" or a follow-up.
func (s *Store) HasAnyAutopilotJobForIssue(issueID string) (bool, error) {
	if issueID == "" {
		return false, nil
	}
	var n int
	err := s.DB.QueryRow(
		`SELECT 1 FROM autopilot_jobs WHERE issue_id=? LIMIT 1`,
		issueID,
	).Scan(&n)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// ListJobsByIssueAndStates returns all autopilot jobs for the given issue ID
// whose state is in states. Ordered by started_at DESC.
func (s *Store) ListJobsByIssueAndStates(issueID string, states []string) ([]AutopilotJob, error) {
	if len(states) == 0 {
		return nil, nil
	}
	placeholders := strings.Repeat(",?", len(states)-1)
	query := fmt.Sprintf(`
		SELECT agent_session_id, issue_id, issue_identifier, state,
		       COALESCE(worktree_path,''), COALESCE(branch,''),
		       COALESCE(pr_url,''), COALESCE(error,''),
		       started_at, COALESCE(finished_at,''),
		       COALESCE(stream_log_path,''),
		       COALESCE(claude_session_id,'')
		FROM autopilot_jobs
		WHERE issue_id=? AND state IN (?%s)
		ORDER BY started_at DESC
	`, placeholders)
	args := make([]any, 0, len(states)+1)
	args = append(args, issueID)
	for _, st := range states {
		args = append(args, st)
	}
	rows, err := s.DB.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AutopilotJob
	for rows.Next() {
		var j AutopilotJob
		if err := rows.Scan(&j.AgentSessionID, &j.IssueID, &j.IssueIdentifier, &j.State,
			&j.WorktreePath, &j.Branch, &j.PRURL, &j.Error, &j.StartedAt, &j.FinishedAt,
			&j.StreamLogPath, &j.ClaudeSessionID); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// ListAutopilotJobsByProject returns jobs whose worktree_path lives under the
// repo_dir of the given project_id. The link relies on the default config
// (worktree_root relative to repo_dir, so worktrees nest under repo_dir/) —
// callers that configure an absolute worktree_root will see no jobs match.
// projectID == "" is treated as no filter and behaves like ListAutopilotJobs.
func (s *Store) ListAutopilotJobsByProject(projectID, status, issueID string, since *time.Time, limit int) ([]AutopilotJob, error) {
	if projectID == "" {
		return s.ListAutopilotJobs(status, issueID, since, limit)
	}
	query := `
		SELECT j.agent_session_id, j.issue_id, j.issue_identifier, j.state,
		       COALESCE(j.worktree_path,''), COALESCE(j.branch,''),
		       COALESCE(j.pr_url,''), COALESCE(j.error,''),
		       j.started_at, COALESCE(j.finished_at,''),
		       COALESCE(j.stream_log_path,''),
		       COALESCE(j.claude_session_id,'')
		FROM autopilot_jobs j
		JOIN repos r ON j.worktree_path LIKE rtrim(r.repo_dir, '/') || '/%'
		WHERE r.project_id=?`
	args := []any{projectID}
	if status != "" {
		query += " AND j.state=?"
		args = append(args, status)
	}
	if issueID != "" {
		query += " AND j.issue_id=?"
		args = append(args, issueID)
	}
	if since != nil {
		query += " AND j.started_at>=?"
		args = append(args, since.UTC().Format(time.RFC3339))
	}
	query += " ORDER BY j.started_at DESC"
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.DB.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AutopilotJob
	for rows.Next() {
		var j AutopilotJob
		if err := rows.Scan(&j.AgentSessionID, &j.IssueID, &j.IssueIdentifier, &j.State,
			&j.WorktreePath, &j.Branch, &j.PRURL, &j.Error, &j.StartedAt, &j.FinishedAt,
			&j.StreamLogPath, &j.ClaudeSessionID); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// ListAutopilotJobs returns jobs matching the given filters, ordered by started_at desc.
func (s *Store) ListAutopilotJobs(status, issueID string, since *time.Time, limit int) ([]AutopilotJob, error) {
	query := `
		SELECT agent_session_id, issue_id, issue_identifier, state,
		       COALESCE(worktree_path,''), COALESCE(branch,''),
		       COALESCE(pr_url,''), COALESCE(error,''),
		       started_at, COALESCE(finished_at,''),
		       COALESCE(stream_log_path,''),
		       COALESCE(claude_session_id,'')
		FROM autopilot_jobs WHERE 1=1`
	args := []any{}
	if status != "" {
		query += " AND state=?"
		args = append(args, status)
	}
	if issueID != "" {
		query += " AND issue_id=?"
		args = append(args, issueID)
	}
	if since != nil {
		query += " AND started_at>=?"
		args = append(args, since.UTC().Format(time.RFC3339))
	}
	query += " ORDER BY started_at DESC"
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.DB.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AutopilotJob
	for rows.Next() {
		var j AutopilotJob
		if err := rows.Scan(&j.AgentSessionID, &j.IssueID, &j.IssueIdentifier, &j.State,
			&j.WorktreePath, &j.Branch, &j.PRURL, &j.Error, &j.StartedAt, &j.FinishedAt,
			&j.StreamLogPath, &j.ClaudeSessionID); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// --- admiral_tasks (GEO-50, issue-keyed task model) ---
//
// PR-A only ships the schema + Store CRUD methods. Dispatch (handleCreated /
// handlePrompted) still writes to autopilot_jobs in this PR. PR-B will
// switch lookups to admiral_tasks and dual-write through both tables.

// AdmiralTask is the live task row for a Linear issue under the new
// issue-keyed model. One row per issue. After /rerun the row is moved to
// admiral_task_history and a new live row is inserted with attempt_n+1.
type AdmiralTask struct {
	IssueID            string
	IssueIdentifier    string // denormalized "GEO-50" style identifier for log/UI
	State              string
	AttemptN           int
	Branch             string
	WorktreePath       string
	PRURL              string
	ClaudeSessionID    string
	LastEventSessionID string // most recent Linear AgentSession.id (used for PostAgentActivity)
	StartedAt          string
	FinishedAt         string
	Error              string
	StreamLogPath      string
	PendingQuestionID  string // non-empty when state=AWAITING_INPUT
}

// AdmiralTaskHistory is one entry in the supersession log for an issue.
type AdmiralTaskHistory struct {
	HistoryID        int64
	IssueID          string
	AttemptN         int
	State            string
	Branch           string
	WorktreePath     string
	PRURL            string
	ClaudeSessionID  string
	AgentSessionIDs  string // JSON array; populated when supersession packs multiple sessions in one row
	StartedAt        string
	FinishedAt       string
	Error            string
	StreamLogPath    string
	SupersededAt     string
	SupersededReason string
}

// ListAdmiralTasksByStates returns admiral_tasks rows whose state is
// in the given list, ordered by started_at ASC. Used by
// admiral-discoverer to find tasks whose PRs may need Linear-state
// advancement (e.g. all DONE tasks → poll GitHub to learn merge /
// approval status). Returns an empty slice when states is empty.
func (s *Store) ListAdmiralTasksByStates(states []string) ([]AdmiralTask, error) {
	if len(states) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(states))
	args := make([]any, len(states))
	for i, st := range states {
		placeholders[i] = "?"
		args[i] = st
	}
	q := `
		SELECT issue_id, COALESCE(issue_identifier,''), state, attempt_n,
		       COALESCE(branch,''), COALESCE(worktree_path,''),
		       COALESCE(pr_url,''), COALESCE(claude_session_id,''),
		       COALESCE(last_event_session_id,''),
		       started_at, COALESCE(finished_at,''),
		       COALESCE(error,''), COALESCE(stream_log_path,''),
		       COALESCE(pending_question_id,'')
		FROM admiral_tasks
		WHERE state IN (` + strings.Join(placeholders, ",") + `)
		ORDER BY started_at ASC
	`
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AdmiralTask
	for rows.Next() {
		var t AdmiralTask
		if err := rows.Scan(&t.IssueID, &t.IssueIdentifier, &t.State, &t.AttemptN,
			&t.Branch, &t.WorktreePath, &t.PRURL, &t.ClaudeSessionID,
			&t.LastEventSessionID, &t.StartedAt, &t.FinishedAt,
			&t.Error, &t.StreamLogPath, &t.PendingQuestionID); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetAdmiralTaskByIssue returns the live task row for an issue, or
// (nil, nil) when no task has been claimed yet.
func (s *Store) GetAdmiralTaskByIssue(issueID string) (*AdmiralTask, error) {
	var t AdmiralTask
	err := s.DB.QueryRow(`
		SELECT issue_id, COALESCE(issue_identifier,''), state, attempt_n,
		       COALESCE(branch,''), COALESCE(worktree_path,''),
		       COALESCE(pr_url,''), COALESCE(claude_session_id,''),
		       COALESCE(last_event_session_id,''),
		       started_at, COALESCE(finished_at,''),
		       COALESCE(error,''), COALESCE(stream_log_path,''),
		       COALESCE(pending_question_id,'')
		FROM admiral_tasks WHERE issue_id=?
	`, issueID).Scan(&t.IssueID, &t.IssueIdentifier, &t.State, &t.AttemptN,
		&t.Branch, &t.WorktreePath, &t.PRURL, &t.ClaudeSessionID,
		&t.LastEventSessionID, &t.StartedAt, &t.FinishedAt,
		&t.Error, &t.StreamLogPath, &t.PendingQuestionID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &t, err
}

// GetAdmiralTaskByPRURL returns the live task row whose pr_url matches, or
// (nil, nil) when no task has that PR. Used by the GitHub review dispatcher
// to correlate an inbound review event with its originating admiral task.
func (s *Store) GetAdmiralTaskByPRURL(prURL string) (*AdmiralTask, error) {
	var t AdmiralTask
	err := s.DB.QueryRow(`
		SELECT issue_id, COALESCE(issue_identifier,''), state, attempt_n,
		       COALESCE(branch,''), COALESCE(worktree_path,''),
		       COALESCE(pr_url,''), COALESCE(claude_session_id,''),
		       COALESCE(last_event_session_id,''),
		       started_at, COALESCE(finished_at,''),
		       COALESCE(error,''), COALESCE(stream_log_path,''),
		       COALESCE(pending_question_id,'')
		FROM admiral_tasks WHERE pr_url=?
	`, prURL).Scan(&t.IssueID, &t.IssueIdentifier, &t.State, &t.AttemptN,
		&t.Branch, &t.WorktreePath, &t.PRURL, &t.ClaudeSessionID,
		&t.LastEventSessionID, &t.StartedAt, &t.FinishedAt,
		&t.Error, &t.StreamLogPath, &t.PendingQuestionID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &t, err
}

// BlockedTask is the minimal projection of an admiral_tasks row that the
// blocker watcher needs to re-check and re-queue blocked tasks.
type BlockedTask struct {
	IssueID            string
	IssueIdentifier    string
	LastEventSessionID string
	AttemptN           int
	BlockerIDs         string // JSON array of Linear issue IDs
}

// SetAdmiralTaskBlocked transitions an existing admiral_tasks row to BLOCKED
// and records the JSON-encoded list of blocking issue IDs.
func (s *Store) SetAdmiralTaskBlocked(issueID, blockerIDs string) error {
	_, err := s.DB.Exec(`
		UPDATE admiral_tasks SET state=?, blocker_ids=? WHERE issue_id=?
	`, JobStateBlocked, blockerIDs, issueID)
	return err
}

// GetBlockedAdmiralTasks returns all tasks currently in BLOCKED state.
func (s *Store) GetBlockedAdmiralTasks() ([]BlockedTask, error) {
	rows, err := s.DB.Query(`
		SELECT issue_id, COALESCE(issue_identifier,''),
		       COALESCE(last_event_session_id,''), attempt_n,
		       COALESCE(blocker_ids,'')
		FROM admiral_tasks WHERE state=?
	`, JobStateBlocked)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BlockedTask
	for rows.Next() {
		var t BlockedTask
		if err := rows.Scan(&t.IssueID, &t.IssueIdentifier,
			&t.LastEventSessionID, &t.AttemptN, &t.BlockerIDs); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TransitionBlockedToReceived atomically moves a BLOCKED task back to
// RECEIVED, clearing blocker_ids. Returns true when the row was updated
// (i.e. it was still BLOCKED), false when another goroutine already
// changed the state.
func (s *Store) TransitionBlockedToReceived(issueID string) (bool, error) {
	res, err := s.DB.Exec(`
		UPDATE admiral_tasks SET state=?, blocker_ids=NULL WHERE issue_id=? AND state=?
	`, JobStateReceived, issueID, JobStateBlocked)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ClaimAdmiralTask inserts a new task row for issueID iff no row exists.
// Mirror of ClaimAutopilotJob but keyed on issue_id and reserving the
// initial attempt_n=1. Returns (true, nil) when the caller now owns the
// task; (false, nil) when a row already exists for that issue.
func (s *Store) ClaimAdmiralTask(issueID, identifier, lastEventSessionID string) (bool, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.DB.Exec(`
		INSERT OR IGNORE INTO admiral_tasks(
			issue_id, issue_identifier, state, attempt_n,
			last_event_session_id, started_at
		) VALUES(?, ?, ?, 1, ?, ?)
	`, issueID, identifier, JobStateReceived, lastEventSessionID, now)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// UpdateAdmiralTask loads the task row for issueID, applies fn, writes
// back. Atomic via single transaction. Returns sql.ErrNoRows if the row
// is missing — callers should ClaimAdmiralTask first.
func (s *Store) UpdateAdmiralTask(issueID string, fn func(*AdmiralTask)) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var t AdmiralTask
	err = tx.QueryRow(`
		SELECT issue_id, COALESCE(issue_identifier,''), state, attempt_n,
		       COALESCE(branch,''), COALESCE(worktree_path,''),
		       COALESCE(pr_url,''), COALESCE(claude_session_id,''),
		       COALESCE(last_event_session_id,''),
		       started_at, COALESCE(finished_at,''),
		       COALESCE(error,''), COALESCE(stream_log_path,'')
		FROM admiral_tasks WHERE issue_id=?
	`, issueID).Scan(&t.IssueID, &t.IssueIdentifier, &t.State, &t.AttemptN,
		&t.Branch, &t.WorktreePath, &t.PRURL, &t.ClaudeSessionID,
		&t.LastEventSessionID, &t.StartedAt, &t.FinishedAt,
		&t.Error, &t.StreamLogPath)
	if err != nil {
		return err
	}
	fn(&t)
	_, err = tx.Exec(`
		UPDATE admiral_tasks
		SET issue_identifier=?, state=?, attempt_n=?, branch=?, worktree_path=?,
		    pr_url=?, claude_session_id=?, last_event_session_id=?,
		    finished_at=?, error=?, stream_log_path=?
		WHERE issue_id=?
	`, nullIfEmpty(t.IssueIdentifier), t.State, t.AttemptN,
		nullIfEmpty(t.Branch), nullIfEmpty(t.WorktreePath),
		nullIfEmpty(t.PRURL), nullIfEmpty(t.ClaudeSessionID),
		nullIfEmpty(t.LastEventSessionID),
		nullIfEmpty(t.FinishedAt), nullIfEmpty(t.Error),
		nullIfEmpty(t.StreamLogPath), issueID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// MoveAdmiralTaskToHistoryAndClaimNew atomically moves the live
// admiral_tasks row for issueID into admiral_task_history (with the
// given supersession reason) and inserts a fresh live row with
// attempt_n incremented by one. Used by /rerun in the unified
// dispatch (GEO-50). Returns the new attempt_n on success.
//
// Caller must ensure the old row exists. If it doesn't the function
// returns sql.ErrNoRows.
func (s *Store) MoveAdmiralTaskToHistoryAndClaimNew(issueID, reason, identifier, lastEventSessionID string) (int, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var t AdmiralTask
	err = tx.QueryRow(`
		SELECT issue_id, COALESCE(issue_identifier,''), state, attempt_n,
		       COALESCE(branch,''), COALESCE(worktree_path,''),
		       COALESCE(pr_url,''), COALESCE(claude_session_id,''),
		       COALESCE(last_event_session_id,''),
		       started_at, COALESCE(finished_at,''),
		       COALESCE(error,''), COALESCE(stream_log_path,'')
		FROM admiral_tasks WHERE issue_id=?
	`, issueID).Scan(&t.IssueID, &t.IssueIdentifier, &t.State, &t.AttemptN,
		&t.Branch, &t.WorktreePath, &t.PRURL, &t.ClaudeSessionID,
		&t.LastEventSessionID, &t.StartedAt, &t.FinishedAt,
		&t.Error, &t.StreamLogPath)
	if err != nil {
		return 0, err
	}

	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := tx.Exec(`
		INSERT INTO admiral_task_history(
			issue_id, attempt_n, state, branch, worktree_path, pr_url,
			claude_session_id, agent_session_ids, started_at, finished_at,
			error, stream_log_path, superseded_at, superseded_reason
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, t.IssueID, t.AttemptN, t.State,
		nullIfEmpty(t.Branch), nullIfEmpty(t.WorktreePath), nullIfEmpty(t.PRURL),
		nullIfEmpty(t.ClaudeSessionID), nullIfEmpty(t.LastEventSessionID),
		t.StartedAt, nullIfEmpty(t.FinishedAt),
		nullIfEmpty(t.Error), nullIfEmpty(t.StreamLogPath),
		now, reason); err != nil {
		return 0, fmt.Errorf("insert history: %w", err)
	}

	if _, err := tx.Exec(`DELETE FROM admiral_tasks WHERE issue_id=?`, issueID); err != nil {
		return 0, fmt.Errorf("delete old live: %w", err)
	}
	newAttempt := t.AttemptN + 1
	if _, err := tx.Exec(`
		INSERT INTO admiral_tasks(
			issue_id, issue_identifier, state, attempt_n,
			last_event_session_id, started_at
		) VALUES(?, ?, ?, ?, ?, ?)
	`, issueID, identifier, JobStateReceived, newAttempt,
		nullIfEmpty(lastEventSessionID), now); err != nil {
		return 0, fmt.Errorf("insert new live: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return newAttempt, nil
}

// ListAdmiralTaskHistoryByIssue returns supersession history for an
// issue, ordered by attempt_n ascending (oldest first).
func (s *Store) ListAdmiralTaskHistoryByIssue(issueID string) ([]AdmiralTaskHistory, error) {
	rows, err := s.DB.Query(`
		SELECT history_id, issue_id, attempt_n, state,
		       COALESCE(branch,''), COALESCE(worktree_path,''),
		       COALESCE(pr_url,''), COALESCE(claude_session_id,''),
		       COALESCE(agent_session_ids,''),
		       started_at, COALESCE(finished_at,''),
		       COALESCE(error,''), COALESCE(stream_log_path,''),
		       superseded_at, superseded_reason
		FROM admiral_task_history
		WHERE issue_id=?
		ORDER BY attempt_n ASC
	`, issueID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AdmiralTaskHistory
	for rows.Next() {
		var h AdmiralTaskHistory
		if err := rows.Scan(&h.HistoryID, &h.IssueID, &h.AttemptN, &h.State,
			&h.Branch, &h.WorktreePath, &h.PRURL, &h.ClaudeSessionID,
			&h.AgentSessionIDs, &h.StartedAt, &h.FinishedAt,
			&h.Error, &h.StreamLogPath, &h.SupersededAt, &h.SupersededReason); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
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

// AuthErrorState describes the OAuth circuit-breaker state. Reason is empty
// when auth is healthy; non-empty means refresh hit a permanent failure
// (invalid_grant / invalid_client) and the user must run admiral-oauth.
// ErrAt is when the failure was first detected; NotifiedAt is when we last
// alerted the user (used to dedupe the TG message).
type AuthErrorState struct {
	Reason     string
	ErrAt      string // RFC3339 UTC, empty if healthy
	NotifiedAt string // RFC3339 UTC, empty if not yet notified
}

// GetAuthError returns the current auth-error state. Healthy state is
// (AuthErrorState{}, nil) — caller checks `Reason != ""` to decide.
func (s *Store) GetAuthError() (AuthErrorState, error) {
	var st AuthErrorState
	err := s.DB.QueryRow(`
		SELECT COALESCE(auth_error,''),
		       COALESCE(auth_error_at,''),
		       COALESCE(notified_at,'')
		FROM linear_oauth WHERE id=1
	`).Scan(&st.Reason, &st.ErrAt, &st.NotifiedAt)
	if err == sql.ErrNoRows {
		return AuthErrorState{}, nil
	}
	return st, err
}

// MarkAuthBroken records a permanent OAuth failure. The first-seen timestamp
// (auth_error_at) is preserved across repeated calls so callers can tell how
// long the daemon has been broken.
func (s *Store) MarkAuthBroken(reason string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.DB.Exec(`
		UPDATE linear_oauth
		SET auth_error=?,
		    auth_error_at=COALESCE(NULLIF(auth_error_at,''), ?)
		WHERE id=1
	`, reason, now)
	return err
}

// MarkAuthNotified records that the user-facing alert was sent at `at`.
// Stamping is idempotent — caller decides whether to overwrite (e.g. when
// re-alerting after a stale window).
func (s *Store) MarkAuthNotified(at string) error {
	_, err := s.DB.Exec(`
		UPDATE linear_oauth SET notified_at=? WHERE id=1
	`, at)
	return err
}

// ClearAuthError marks auth as healthy again. Called after a successful
// token refresh or a successful admiral-oauth re-bootstrap.
func (s *Store) ClearAuthError() error {
	_, err := s.DB.Exec(`
		UPDATE linear_oauth
		SET auth_error=NULL, auth_error_at=NULL, notified_at=NULL
		WHERE id=1
	`)
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
	WebhookID   string
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
	Source      string // "linear" (default) or "github"
}

// EnqueueEvent inserts a pending row from the Linear source. Returns true
// when a fresh row was inserted, false when webhook_id already existed
// (Linear retry / dup). source defaults to 'linear' via the schema, so
// rows from this function are tagged correctly without explicit mention.
// New non-Linear callers should use EnqueueEventWithSource.
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

// EnqueueEventWithSource inserts a pending row for any webhook source.
// Returns true when fresh, false on either webhook_id collision (primary
// dedup) or (source, comment_id) collision (secondary dedup for sources
// that re-deliver the same comment under a new delivery id). Pass
// commentID="" to skip comment-based dedup — the partial unique index
// only constrains rows with a non-NULL comment_id.
func (s *Store) EnqueueEventWithSource(source, webhookID, action, sessionID, issueID, payloadJSON, commentID string) (bool, error) {
	now := time.Now().UTC()
	// Empty commentID maps to SQL NULL so the partial unique index
	// (WHERE comment_id IS NOT NULL) does not constrain it.
	var commentArg interface{}
	if commentID != "" {
		commentArg = commentID
	}
	result, err := s.DB.Exec(`
		INSERT OR IGNORE INTO events_inbox(
			webhook_id, action, session_id, issue_id, payload_json,
			status, attempts, received_at, source, comment_id
		) VALUES(?, ?, ?, ?, ?, 'pending', 0, ?, ?, ?)
	`, webhookID, action, sessionID, issueID, payloadJSON, now.Unix(), source, commentArg)
	if err != nil {
		return false, err
	}
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
		       COALESCE(last_error, ''),
		       COALESCE(source, 'linear')
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
		&row.Source,
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

// --- repos ---

// Repo holds a Linear project → repo mapping.
type Repo struct {
	ProjectID       string
	ProjectName     string
	RepoDir         string
	BaseBranch      string
	Enabled         bool
	AutoPickEnabled bool
}

// ListRepos returns all repos ordered by project_name.
func (s *Store) ListRepos() ([]Repo, error) {
	rows, err := s.DB.Query(`
		SELECT project_id, project_name, repo_dir, base_branch, enabled, auto_pick_enabled
		FROM repos ORDER BY project_name ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Repo
	for rows.Next() {
		var r Repo
		var enabled, autoPick int
		if err := rows.Scan(&r.ProjectID, &r.ProjectName, &r.RepoDir, &r.BaseBranch, &enabled, &autoPick); err != nil {
			return nil, err
		}
		r.Enabled = enabled == 1
		r.AutoPickEnabled = autoPick == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetRepoByProjectID returns the repo for a given Linear project ID.
// Returns (nil, nil) when no repo is configured for that project.
func (s *Store) GetRepoByProjectID(projectID string) (*Repo, error) {
	var r Repo
	var enabled, autoPick int
	err := s.DB.QueryRow(`
		SELECT project_id, project_name, repo_dir, base_branch, enabled, auto_pick_enabled
		FROM repos WHERE project_id=?
	`, projectID).Scan(&r.ProjectID, &r.ProjectName, &r.RepoDir, &r.BaseBranch, &enabled, &autoPick)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.Enabled = enabled == 1
	r.AutoPickEnabled = autoPick == 1
	return &r, nil
}

// UpsertRepo inserts or replaces a repo.
func (s *Store) UpsertRepo(r Repo) error {
	now := time.Now().UTC().Format(time.RFC3339)
	enabled := 0
	if r.Enabled {
		enabled = 1
	}
	autoPick := 0
	if r.AutoPickEnabled {
		autoPick = 1
	}
	_, err := s.DB.Exec(`
		INSERT INTO repos(project_id, project_name, repo_dir, base_branch, enabled, auto_pick_enabled, created_at, updated_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(project_id) DO UPDATE SET
			project_name=excluded.project_name,
			repo_dir=excluded.repo_dir,
			base_branch=excluded.base_branch,
			enabled=excluded.enabled,
			auto_pick_enabled=excluded.auto_pick_enabled,
			updated_at=excluded.updated_at
	`, r.ProjectID, r.ProjectName, r.RepoDir, r.BaseBranch, enabled, autoPick, now, now)
	return err
}

// DeleteRepo removes a repo by project_id.
func (s *Store) DeleteRepo(projectID string) error {
	_, err := s.DB.Exec(`DELETE FROM repos WHERE project_id=?`, projectID)
	return err
}

// ListAutoPickEnabledProjectIDs returns the project IDs of repos that
// are both enabled (auto_pick implies enabled) AND opted-in to the
// discoverer's auto-pick scan. Drives the discoverer's per-tick scope.
func (s *Store) ListAutoPickEnabledProjectIDs() ([]string, error) {
	rows, err := s.DB.Query(`
		SELECT project_id FROM repos
		WHERE enabled=1 AND auto_pick_enabled=1
		ORDER BY project_name ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// --- discoverer_picks ---

// DiscovererPick is one row in the discoverer_picks table: a record
// that admiral-discoverer has elected to self-assign this issue at
// least once. When picked_state still matches the issue's current
// Linear state, the discoverer treats the issue as "already handled"
// and skips. When the state diverges (external reset), the row is
// overwritten on the next pick.
type DiscovererPick struct {
	IssueID         string
	IssueIdentifier string
	PickedAt        string
	PickedState     string
	UpdatedAt       string
}

// GetDiscovererPick returns the pick record for an issue, or (nil, nil)
// when none exists.
func (s *Store) GetDiscovererPick(issueID string) (*DiscovererPick, error) {
	var p DiscovererPick
	err := s.DB.QueryRow(`
		SELECT issue_id, issue_identifier, picked_at, picked_state, updated_at
		FROM discoverer_picks WHERE issue_id=?
	`, issueID).Scan(&p.IssueID, &p.IssueIdentifier, &p.PickedAt, &p.PickedState, &p.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// UpsertDiscovererPick records (or refreshes) a pick. Used both for
// the initial assign and for re-picks after an external state reset.
func (s *Store) UpsertDiscovererPick(p DiscovererPick) error {
	now := time.Now().UTC().Format(time.RFC3339)
	if p.PickedAt == "" {
		p.PickedAt = now
	}
	_, err := s.DB.Exec(`
		INSERT INTO discoverer_picks(issue_id, issue_identifier, picked_at, picked_state, updated_at)
		VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(issue_id) DO UPDATE SET
			issue_identifier=excluded.issue_identifier,
			picked_at=excluded.picked_at,
			picked_state=excluded.picked_state,
			updated_at=excluded.updated_at
	`, p.IssueID, p.IssueIdentifier, p.PickedAt, p.PickedState, now)
	return err
}

// --- pending_questions (HITL ask_user flow) ---

// PendingQuestion is a question that a claude run has asked the user via
// Linear thread. The run has exited and the task is in AWAITING_INPUT state.
// When answered_at is non-empty the question has been resolved.
type PendingQuestion struct {
	ID                 string
	IssueID            string
	IssueIdentifier    string
	ClaudeSessionID    string
	LastEventSessionID string
	WorktreePath       string
	Question           string
	OptionsJSON        string // JSON array of option strings, may be empty
	CreatedAt          string
	AnsweredAt         string // empty when still open
	Answer             string // empty when still open
}

// InsertPendingQuestion records a new open question from a claude run.
// id must be a fresh UUID (generated by the caller or MCP tool).
func (s *Store) InsertPendingQuestion(q PendingQuestion) error {
	_, err := s.DB.Exec(`
		INSERT INTO pending_questions(
			id, issue_id, issue_identifier, claude_session_id,
			last_event_session_id, worktree_path,
			question, options_json, created_at
		) VALUES(?,?,?,?,?,?,?,?,?)
	`, q.ID, q.IssueID, q.IssueIdentifier, q.ClaudeSessionID,
		q.LastEventSessionID, q.WorktreePath,
		q.Question, q.OptionsJSON, q.CreatedAt)
	return err
}

// GetOpenPendingQuestionByIssue returns the most recent unanswered question
// for an issue, or (nil, nil) when none exists.
func (s *Store) GetOpenPendingQuestionByIssue(issueID string) (*PendingQuestion, error) {
	var q PendingQuestion
	err := s.DB.QueryRow(`
		SELECT id, issue_id, COALESCE(issue_identifier,''), claude_session_id,
		       COALESCE(last_event_session_id,''), COALESCE(worktree_path,''),
		       question, COALESCE(options_json,'[]'), created_at,
		       COALESCE(answered_at,''), COALESCE(answer,'')
		FROM pending_questions
		WHERE issue_id=? AND answered_at IS NULL
		ORDER BY created_at DESC LIMIT 1
	`, issueID).Scan(&q.ID, &q.IssueID, &q.IssueIdentifier, &q.ClaudeSessionID,
		&q.LastEventSessionID, &q.WorktreePath,
		&q.Question, &q.OptionsJSON, &q.CreatedAt,
		&q.AnsweredAt, &q.Answer)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &q, err
}

// GetPendingQuestionByID returns a pending question by its UUID, or
// (nil, nil) when not found. Used by dispatchAwaitingReply to look up
// the exact question correlated to the task via task.PendingQuestionID.
func (s *Store) GetPendingQuestionByID(id string) (*PendingQuestion, error) {
	var q PendingQuestion
	err := s.DB.QueryRow(`
		SELECT id, issue_id, COALESCE(issue_identifier,''), claude_session_id,
		       COALESCE(last_event_session_id,''), COALESCE(worktree_path,''),
		       question, COALESCE(options_json,'[]'), created_at,
		       COALESCE(answered_at,''), COALESCE(answer,'')
		FROM pending_questions WHERE id=?
	`, id).Scan(&q.ID, &q.IssueID, &q.IssueIdentifier, &q.ClaudeSessionID,
		&q.LastEventSessionID, &q.WorktreePath,
		&q.Question, &q.OptionsJSON, &q.CreatedAt,
		&q.AnsweredAt, &q.Answer)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &q, err
}

// CancelOpenPendingQuestionsForIssue marks all unanswered questions for an
// issue as cancelled. Called by dispatchRerun to clean up orphaned questions
// before superseding the task row.
func (s *Store) CancelOpenPendingQuestionsForIssue(issueID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.DB.Exec(`
		UPDATE pending_questions
		SET answered_at=?, answer='<superseded>'
		WHERE issue_id=? AND answered_at IS NULL
	`, now, issueID)
	return err
}

// AnswerPendingQuestion records the user's reply and timestamps it.
func (s *Store) AnswerPendingQuestion(id, answer string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.DB.Exec(`
		UPDATE pending_questions SET answered_at=?, answer=? WHERE id=? AND answered_at IS NULL
	`, now, answer, id)
	return err
}

// SetAdmiralTaskAwaitingInput transitions a task to AWAITING_INPUT and
// records the pending question ID so the resume path can look it up.
func (s *Store) SetAdmiralTaskAwaitingInput(issueID, pendingQuestionID string) error {
	_, err := s.DB.Exec(`
		UPDATE admiral_tasks SET state=?, pending_question_id=? WHERE issue_id=?
	`, JobStateAwaitingInput, pendingQuestionID, issueID)
	return err
}

// TransitionAwaitingInputToExecuting atomically moves an AWAITING_INPUT task
// back to EXECUTING. Returns true when the row was updated (i.e. this caller
// won the race), false when another goroutine already changed the state.
func (s *Store) TransitionAwaitingInputToExecuting(issueID string) (bool, error) {
	res, err := s.DB.Exec(`
		UPDATE admiral_tasks
		SET state=?, pending_question_id=NULL
		WHERE issue_id=? AND state=?
	`, JobStateExecuting, issueID, JobStateAwaitingInput)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// --- admin: stale awaiting-input job queries ---

// AwaitingInputJob is a denormalised view of a task stuck in AWAITING_INPUT,
// enriched with the pending question details needed for the admin CLI.
type AwaitingInputJob struct {
	IssueID          string
	IssueIdentifier  string
	WorktreePath     string
	Branch           string
	LastEventSession string // for posting a Linear comment on abort
	PendingQuestion  string
	PendingCreatedAt string // RFC3339; age is computed from this
}

// ListAwaitingInputJobs returns all tasks in AWAITING_INPUT state, optionally
// filtered to those whose pending question was created more than olderThan ago.
// Results are sorted oldest-first.
func (s *Store) ListAwaitingInputJobs(olderThan time.Duration) ([]AwaitingInputJob, error) {
	cutoff := ""
	if olderThan > 0 {
		cutoff = time.Now().UTC().Add(-olderThan).Format(time.RFC3339)
	}
	// LEFT JOIN so orphan tasks (pending_question_id is NULL or stale) are
	// still visible. When a cutoff is set we include:
	//   - tasks with a linked question older than the cutoff, AND
	//   - tasks with no linked question at all (orphans — stuck even longer).
	// Rows where the linked question is newer than the cutoff are excluded.
	// NULL created_at sorts last (NULLS LAST via the IS NULL trick).
	query := `
		SELECT t.issue_id, COALESCE(t.issue_identifier,''),
		       COALESCE(t.worktree_path,''), COALESCE(t.branch,''),
		       COALESCE(t.last_event_session_id,''),
		       COALESCE(pq.question,''), COALESCE(pq.created_at,'')
		FROM admiral_tasks t
		LEFT JOIN pending_questions pq ON pq.id = t.pending_question_id
		WHERE t.state = ?`
	args := []any{JobStateAwaitingInput}
	if cutoff != "" {
		query += " AND (pq.created_at IS NULL OR pq.created_at <= ?)"
		args = append(args, cutoff)
	}
	query += " ORDER BY pq.created_at IS NULL, pq.created_at ASC"
	rows, err := s.DB.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AwaitingInputJob
	for rows.Next() {
		var j AwaitingInputJob
		if err := rows.Scan(&j.IssueID, &j.IssueIdentifier,
			&j.WorktreePath, &j.Branch, &j.LastEventSession,
			&j.PendingQuestion, &j.PendingCreatedAt); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// AbortAdmiralTask transitions a task from AWAITING_INPUT to ABORTED and
// cancels its open pending question. Returns false if the task was not in
// AWAITING_INPUT (already resumed or aborted by another caller).
func (s *Store) AbortAdmiralTask(issueID string) (bool, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.DB.Exec(`
		UPDATE admiral_tasks
		SET state=?, pending_question_id=NULL, finished_at=?
		WHERE issue_id=? AND state=?
	`, JobStateAborted, now, issueID, JobStateAwaitingInput)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return false, nil
	}
	_ = s.CancelOpenPendingQuestionsForIssue(issueID)
	return true, nil
}
