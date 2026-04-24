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
	// WAL avoids SQLITE_BUSY between concurrent cursor + messages writes
	// (event-push goroutine + inbound long-poll goroutine both write).
	if _, err := db.Exec(`PRAGMA journal_mode=WAL;`); err != nil {
		return nil, fmt.Errorf("set WAL: %w", err)
	}
	if _, err := db.Exec(migration0001); err != nil {
		return nil, fmt.Errorf("apply migration: %w", err)
	}
	return &Store{DB: db}, nil
}

func (s *Store) Close() error {
	return s.DB.Close()
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
