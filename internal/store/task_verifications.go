package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Task-verification status values. 'active' while the verification loop
// runs; both other values are terminal and stop further triggers.
const (
	TaskVerifyActive    = "active"
	TaskVerifyEscalated = "escalated"
	TaskVerifyClosed    = "closed"
)

// ErrInvalidTaskStatus is returned by SetTaskVerificationStatus for a value
// outside the allowlist, so callers get a typed error instead of an opaque
// "CHECK constraint failed" (mirrors planner.go's ErrInvalidVerdict).
var ErrInvalidTaskStatus = errors.New("invalid task verification status")

func isValidTaskVerifyStatus(s string) bool {
	switch s {
	case TaskVerifyActive, TaskVerifyEscalated, TaskVerifyClosed:
		return true
	}
	return false
}

// TaskVerification tracks the autonomous verification loop for one parent
// "task" issue. Rounds bounds the self-converging loop; Status gates whether
// the loop is still running. Summary captures the latest judge's one-line
// verdict (consumed by an upper-layer verify hop as the digest of an
// intermediate sub's shipped work). See migration0019 / migration0023.
type TaskVerification struct {
	ParentIssueID string
	Rounds        int
	Status        string
	Summary       string
	UpdatedAt     string // RFC3339 UTC
}

// GetTaskVerification returns the verification row for a parent issue, or
// (nil, nil) when none has been recorded yet.
func (s *Store) GetTaskVerification(parentIssueID string) (*TaskVerification, error) {
	var tv TaskVerification
	err := s.DB.QueryRow(`
		SELECT parent_issue_id, rounds, status, summary, updated_at
		FROM task_verifications WHERE parent_issue_id=?
	`, parentIssueID).Scan(&tv.ParentIssueID, &tv.Rounds, &tv.Status, &tv.Summary, &tv.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &tv, nil
}

// BumpTaskVerificationRound increments (or initialises at 1) the round
// counter for a parent issue and returns the updated row. Status is left
// untouched on an existing row — and defaults to 'active' on insert.
//
// Bump does NOT enforce terminal-state gating: calling it on a 'closed' /
// 'escalated' row still increments rounds without reactivating the loop. The
// store is mechanism, not policy — the caller is responsible for checking
// status (via GetTaskVerification) before deciding to bump and re-verify.
func (s *Store) BumpTaskVerificationRound(parentIssueID string) (*TaskVerification, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.DB.Exec(`
		INSERT INTO task_verifications(parent_issue_id, rounds, status, updated_at)
		VALUES(?, 1, 'active', ?)
		ON CONFLICT(parent_issue_id) DO UPDATE SET
			rounds = rounds + 1,
			updated_at = excluded.updated_at
	`, parentIssueID, now); err != nil {
		return nil, fmt.Errorf("bump task verification: %w", err)
	}
	// Read-back is race-free only because the store runs on a single SQLite
	// connection (MaxOpenConns=1, see Open): no other writer can interleave
	// between the UPSERT and this SELECT.
	return s.GetTaskVerification(parentIssueID)
}

// SetTaskVerificationSummary stores the judge's one-line summary from the
// most recent verify round for a parent issue. Errors if no row exists —
// callers are expected to have BumpTaskVerificationRound'd first (the verify
// dispatcher always does, so this is a defensive invariant).
func (s *Store) SetTaskVerificationSummary(parentIssueID, summary string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.DB.Exec(`
		UPDATE task_verifications SET summary=?, updated_at=? WHERE parent_issue_id=?
	`, summary, now, parentIssueID)
	if err != nil {
		return fmt.Errorf("set task verification summary: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set task verification summary: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("task verification for %s not found", parentIssueID)
	}
	return nil
}

// SetTaskVerificationStatus moves a parent issue's verification to a
// terminal status ('escalated' / 'closed') or back to 'active'. Errors if
// no row exists — the caller must BumpTaskVerificationRound first.
func (s *Store) SetTaskVerificationStatus(parentIssueID, status string) error {
	if !isValidTaskVerifyStatus(status) {
		return fmt.Errorf("%w: %q", ErrInvalidTaskStatus, status)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.DB.Exec(`
		UPDATE task_verifications SET status=?, updated_at=? WHERE parent_issue_id=?
	`, status, now, parentIssueID)
	if err != nil {
		return fmt.Errorf("set task verification status: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set task verification status: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("task verification for %s not found", parentIssueID)
	}
	return nil
}
