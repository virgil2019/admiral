package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ProductVerification tracks the autonomous product-level verification loop
// for one product (a Linear project). Mirrors TaskVerification but keyed by
// project_id: Rounds bounds the self-converging loop, Status gates whether
// the loop is still running. Reuses the task-verification status values
// (TaskVerifyActive / Escalated / Closed) and validation — the state machine
// is identical, only the granularity differs (project vs parent issue).
// See migration0022.
type ProductVerification struct {
	ProjectID string
	Rounds    int
	Status    string
	UpdatedAt string // RFC3339 UTC
}

// GetProductVerification returns the verification row for a project, or
// (nil, nil) when none has been recorded yet.
func (s *Store) GetProductVerification(projectID string) (*ProductVerification, error) {
	var pv ProductVerification
	err := s.DB.QueryRow(`
		SELECT project_id, rounds, status, updated_at
		FROM product_verifications WHERE project_id=?
	`, projectID).Scan(&pv.ProjectID, &pv.Rounds, &pv.Status, &pv.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &pv, nil
}

// BumpProductVerificationRound increments (or initialises at 1) the round
// counter for a project and returns the updated row. Status is left
// untouched on an existing row — and defaults to 'active' on insert.
//
// Like BumpTaskVerificationRound, this does NOT enforce terminal-state
// gating: the caller checks status (via GetProductVerification) before
// deciding to bump and re-verify. The read-back is race-free because the
// store runs on a single SQLite connection (MaxOpenConns=1).
func (s *Store) BumpProductVerificationRound(projectID string) (*ProductVerification, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.DB.Exec(`
		INSERT INTO product_verifications(project_id, rounds, status, updated_at)
		VALUES(?, 1, 'active', ?)
		ON CONFLICT(project_id) DO UPDATE SET
			rounds = rounds + 1,
			updated_at = excluded.updated_at
	`, projectID, now); err != nil {
		return nil, fmt.Errorf("bump product verification: %w", err)
	}
	return s.GetProductVerification(projectID)
}

// ListStuckProductVerifications mirrors ListStuckTaskVerifications for
// the product-level table. product_verifications has no `summary`
// column — the auto-pass marker for the task side has no analog here —
// so the stuck shape is solely `status='active' AND rounds=1`.
//
// Same recovery rationale as ListStuckTaskVerifications: a row bumped
// to rounds=1 whose apply step never ran. See PR #187 / GEO-267.
func (s *Store) ListStuckProductVerifications(ctx context.Context) ([]ProductVerification, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT project_id, rounds, status, updated_at
		FROM product_verifications
		WHERE status='active' AND rounds=1
		ORDER BY updated_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("list stuck product verifications: %w", err)
	}
	defer rows.Close()

	var out []ProductVerification
	for rows.Next() {
		var pv ProductVerification
		if err := rows.Scan(&pv.ProjectID, &pv.Rounds, &pv.Status, &pv.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan stuck product verification: %w", err)
		}
		out = append(out, pv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate stuck product verifications: %w", err)
	}
	return out, nil
}

// SetProductVerificationStatus moves a project's verification to a terminal
// status ('escalated' / 'closed') or back to 'active'. Errors if no row
// exists — the caller must BumpProductVerificationRound first. Reuses the
// task-verification status allowlist (isValidTaskVerifyStatus) and typed
// error (ErrInvalidTaskStatus).
func (s *Store) SetProductVerificationStatus(projectID, status string) error {
	if !isValidTaskVerifyStatus(status) {
		return fmt.Errorf("%w: %q", ErrInvalidTaskStatus, status)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.DB.Exec(`
		UPDATE product_verifications SET status=?, updated_at=? WHERE project_id=?
	`, status, now, projectID)
	if err != nil {
		return fmt.Errorf("set product verification status: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set product verification status: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("product verification for %s not found", projectID)
	}
	return nil
}
