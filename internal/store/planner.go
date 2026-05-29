package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrFeatureNotFound is returned by CloseFeature when the target id
// does not exist. Distinguishes "typo / stale id" from "already
// closed" — the latter returns (false, nil).
var ErrFeatureNotFound = errors.New("feature not found")

// ErrInvalidVerdict is returned by InsertPRVerification when the
// supplied Verdict is outside the PRVerdict* allowlist. Kept as an
// in-code guard on top of the DB CHECK constraint so callers see a
// crisp Go error rather than an opaque "CHECK constraint failed".
var ErrInvalidVerdict = errors.New("invalid PR verdict")

// Feature is the planner-mcp aggregate over a Linear project: one
// feature corresponds 1:1 with a Linear project and carries the
// original requirements text so the host agent can perform L2
// acceptance against the user's intent (not against any spec the
// agent itself wrote).
type Feature struct {
	ID                string
	Name              string
	LinearProjectID   string
	RequirementsText  string
	SourceAgent       string // "claude" / "codex" / "" — telemetry
	CreatedAt         string // RFC3339 UTC
	ClosedAt          string // RFC3339 UTC; "" while open
}

// FeatureIssue is the L1 acceptance contract for a single Linear
// issue inside a feature. Written by the host agent at decomposition
// time and read back at PR-verification time as the judging standard.
type FeatureIssue struct {
	FeatureID           string
	LinearIssueID       string
	AcceptanceCriteria  string
	CreatedAt           string
}

// PRVerdict enumerates the L1 outcomes the planner can submit to a
// GitHub PR. Mirrored by a CHECK constraint on pr_verifications.verdict
// (migration0018) so adding a new verdict requires both a new const
// here and a new migration.
const (
	PRVerdictApprove        = "approve"
	PRVerdictRequestChanges = "request_changes"
	PRVerdictNeedsRebase    = "needs_rebase"
)

// isValidVerdict reports whether v matches the PRVerdict* allowlist.
// Single source of truth for callers that want to validate before
// hitting the DB.
func isValidVerdict(v string) bool {
	switch v {
	case PRVerdictApprove, PRVerdictRequestChanges, PRVerdictNeedsRebase:
		return true
	}
	return false
}

// PRVerification is one audit row per verdict the planner submitted
// for a PR. Multiple rows per PR are expected (re-review after
// request_changes is addressed).
type PRVerification struct {
	PRURL     string
	Verdict   string
	Reasoning string
	Agent     string
	CreatedAt string
}

// InsertFeature creates a feature row. Returns an error if
// linear_project_id is already taken (UNIQUE constraint) so callers
// can detect "this project already has a feature, attach to it
// instead of creating a duplicate."
func (s *Store) InsertFeature(f Feature) error {
	if f.ID == "" || f.Name == "" || f.LinearProjectID == "" {
		return fmt.Errorf("InsertFeature: id, name, linear_project_id required")
	}
	if f.CreatedAt == "" {
		f.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	_, err := s.DB.Exec(`
		INSERT INTO features(
			id, name, linear_project_id, requirements_text,
			source_agent, created_at, closed_at
		) VALUES(?, ?, ?, ?, ?, ?, ?)
	`, f.ID, f.Name, f.LinearProjectID, f.RequirementsText,
		nullIfEmpty(f.SourceAgent), f.CreatedAt, nullIfEmpty(f.ClosedAt))
	return err
}

// InsertFeatureWithIssues writes one feature row plus N feature_issues
// rows atomically. This is the realistic decomposition flow — host
// agent splits a requirement, then writes the feature + all its issue
// criteria in one shot. Without a tx, a mid-loop crash would leave a
// feature with partial issues; verification later would silently judge
// only the issues that made it in.
func (s *Store) InsertFeatureWithIssues(f Feature, issues []FeatureIssue) error {
	if f.ID == "" || f.Name == "" || f.LinearProjectID == "" {
		return fmt.Errorf("InsertFeatureWithIssues: id, name, linear_project_id required")
	}
	if f.CreatedAt == "" {
		f.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`
		INSERT INTO features(
			id, name, linear_project_id, requirements_text,
			source_agent, created_at, closed_at
		) VALUES(?, ?, ?, ?, ?, ?, ?)
	`, f.ID, f.Name, f.LinearProjectID, f.RequirementsText,
		nullIfEmpty(f.SourceAgent), f.CreatedAt, nullIfEmpty(f.ClosedAt)); err != nil {
		return err
	}

	for _, fi := range issues {
		if fi.LinearIssueID == "" {
			return fmt.Errorf("InsertFeatureWithIssues: issue at index %d missing linear_issue_id", len(issues))
		}
		// Force the issue's feature_id to match — caller mistakes here
		// would otherwise produce a feature with mismatched children.
		fi.FeatureID = f.ID
		if fi.CreatedAt == "" {
			fi.CreatedAt = f.CreatedAt
		}
		if _, err := tx.Exec(`
			INSERT INTO feature_issues(
				feature_id, linear_issue_id, acceptance_criteria, created_at
			) VALUES(?, ?, ?, ?)
		`, fi.FeatureID, fi.LinearIssueID, fi.AcceptanceCriteria, fi.CreatedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// GetFeature returns the feature row by primary key, or (nil, nil)
// when not found.
func (s *Store) GetFeature(id string) (*Feature, error) {
	return scanFeature(s.DB.QueryRow(`
		SELECT id, name, linear_project_id, requirements_text,
		       COALESCE(source_agent,''), created_at, COALESCE(closed_at,'')
		FROM features WHERE id=?
	`, id))
}

// GetFeatureByLinearProject looks the feature up by the Linear
// project ID it is bound to. Used when the host agent only knows
// the project (e.g. resuming work on an existing feature).
func (s *Store) GetFeatureByLinearProject(projectID string) (*Feature, error) {
	return scanFeature(s.DB.QueryRow(`
		SELECT id, name, linear_project_id, requirements_text,
		       COALESCE(source_agent,''), created_at, COALESCE(closed_at,'')
		FROM features WHERE linear_project_id=?
	`, projectID))
}

// scanFeature is the shared (*sql.Row).Scan helper for the three
// single-row feature SELECTs above. Centralized so adding a column to
// `features` only touches one Scan call site.
func scanFeature(row *sql.Row) (*Feature, error) {
	var f Feature
	err := row.Scan(&f.ID, &f.Name, &f.LinearProjectID, &f.RequirementsText,
		&f.SourceAgent, &f.CreatedAt, &f.ClosedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// ListFeaturesByName returns features whose name matches exactly.
// Name is not UNIQUE on purpose — same name can be reused after a
// feature is closed — so this returns a slice. Most-recent first.
func (s *Store) ListFeaturesByName(name string) ([]Feature, error) {
	rows, err := s.DB.Query(`
		SELECT id, name, linear_project_id, requirements_text,
		       COALESCE(source_agent,''), created_at, COALESCE(closed_at,'')
		FROM features WHERE name=?
		ORDER BY created_at DESC
	`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Feature
	for rows.Next() {
		var f Feature
		if err := rows.Scan(&f.ID, &f.Name, &f.LinearProjectID, &f.RequirementsText,
			&f.SourceAgent, &f.CreatedAt, &f.ClosedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// CloseFeature stamps closed_at on an open feature. Returns:
//   - (true, nil)               — feature was open, now closed.
//   - (false, nil)              — feature already closed (idempotent no-op).
//   - (false, ErrFeatureNotFound) — id does not exist (likely caller bug).
// The atomic UPDATE...WHERE closed_at IS NULL handles the open→closed
// transition race-free; the lookup afterwards is only consulted when
// nothing was updated, to distinguish "already closed" from "missing".
func (s *Store) CloseFeature(id string) (bool, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.DB.Exec(`
		UPDATE features SET closed_at=?
		WHERE id=? AND closed_at IS NULL
	`, now, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		return true, nil
	}
	// 0 rows affected: either no such id, or already closed. Resolve.
	var dummy string
	err = s.DB.QueryRow(`SELECT id FROM features WHERE id=?`, id).Scan(&dummy)
	if err == sql.ErrNoRows {
		return false, ErrFeatureNotFound
	}
	if err != nil {
		return false, err
	}
	return false, nil
}

// UpsertFeatureIssue writes an L1 acceptance criterion for a Linear
// issue inside a feature. Re-writing an existing (feature_id,
// linear_issue_id) row overwrites the criteria — the host agent may
// refine acceptance criteria during decomposition iteration. created_at
// is NOT bumped on conflict so list order remains stable.
func (s *Store) UpsertFeatureIssue(fi FeatureIssue) error {
	if fi.FeatureID == "" || fi.LinearIssueID == "" {
		return fmt.Errorf("UpsertFeatureIssue: feature_id and linear_issue_id required")
	}
	if fi.CreatedAt == "" {
		fi.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	_, err := s.DB.Exec(`
		INSERT INTO feature_issues(
			feature_id, linear_issue_id, acceptance_criteria, created_at
		) VALUES(?, ?, ?, ?)
		ON CONFLICT(feature_id, linear_issue_id) DO UPDATE SET
			acceptance_criteria=excluded.acceptance_criteria
	`, fi.FeatureID, fi.LinearIssueID, fi.AcceptanceCriteria, fi.CreatedAt)
	return err
}

// GetFeatureIssue returns the row for one (feature, issue) pair, or
// (nil, nil) when no criteria has been recorded.
func (s *Store) GetFeatureIssue(featureID, linearIssueID string) (*FeatureIssue, error) {
	var fi FeatureIssue
	err := s.DB.QueryRow(`
		SELECT feature_id, linear_issue_id, acceptance_criteria, created_at
		FROM feature_issues
		WHERE feature_id=? AND linear_issue_id=?
	`, featureID, linearIssueID).Scan(
		&fi.FeatureID, &fi.LinearIssueID, &fi.AcceptanceCriteria, &fi.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &fi, nil
}

// ListFeatureIssues returns all issue rows for a feature, ordered by
// creation. Issues without recorded criteria are not surfaced here —
// the host agent should detect "issues belonging to the Linear project
// but missing from this list" and prompt the user to add criteria.
func (s *Store) ListFeatureIssues(featureID string) ([]FeatureIssue, error) {
	rows, err := s.DB.Query(`
		SELECT feature_id, linear_issue_id, acceptance_criteria, created_at
		FROM feature_issues WHERE feature_id=?
		ORDER BY created_at ASC
	`, featureID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FeatureIssue
	for rows.Next() {
		var fi FeatureIssue
		if err := rows.Scan(&fi.FeatureID, &fi.LinearIssueID,
			&fi.AcceptanceCriteria, &fi.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, fi)
	}
	return out, rows.Err()
}

// FindFeatureByIssue reverse-looks a Linear issue back to its
// owning feature. The schema permits a Linear issue to appear under
// multiple feature_ids (e.g. issue moved between projects mid-flight),
// so the query picks the most recently linked one — deterministic and
// matches the user's likely intent ("the current home of this issue").
// Returns (nil, nil) when the issue is not part of any feature
// (e.g. legacy issues created before planner-mcp existed).
func (s *Store) FindFeatureByIssue(linearIssueID string) (*Feature, error) {
	return scanFeature(s.DB.QueryRow(`
		SELECT f.id, f.name, f.linear_project_id, f.requirements_text,
		       COALESCE(f.source_agent,''), f.created_at, COALESCE(f.closed_at,'')
		FROM features f
		JOIN feature_issues fi ON fi.feature_id = f.id
		WHERE fi.linear_issue_id=?
		ORDER BY fi.created_at DESC
		LIMIT 1
	`, linearIssueID))
}

// InsertPRVerification appends one audit row for a planner verdict.
// Rows are not deduplicated — the planner may legitimately submit
// multiple verdicts for the same PR (e.g. request_changes, then
// approve after re-review). Idempotency at the "should we actually
// call `gh pr review` again" level is enforced at the tool layer by
// reading the most recent row via GetLatestPRVerification.
//
// Verdict is validated against the PRVerdict* allowlist before the
// INSERT so the caller sees ErrInvalidVerdict rather than the opaque
// CHECK-constraint error from SQLite.
func (s *Store) InsertPRVerification(v PRVerification) error {
	if v.PRURL == "" || v.Verdict == "" {
		return fmt.Errorf("InsertPRVerification: pr_url and verdict required")
	}
	if !isValidVerdict(v.Verdict) {
		return fmt.Errorf("InsertPRVerification: %w: %q", ErrInvalidVerdict, v.Verdict)
	}
	if v.CreatedAt == "" {
		v.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	_, err := s.DB.Exec(`
		INSERT INTO pr_verifications(
			pr_url, verdict, reasoning, agent, created_at
		) VALUES(?, ?, ?, ?, ?)
	`, v.PRURL, v.Verdict, v.Reasoning, nullIfEmpty(v.Agent), v.CreatedAt)
	return err
}

// GetLatestPRVerification returns the most recent verdict for a PR,
// or (nil, nil) when no verdict has been recorded. Used for
// idempotency: if the latest verdict already matches the new one, the
// tool layer skips the GitHub call. Tiebreak on id DESC handles two
// rows in the same RFC3339 second (autoincrement id always advances).
func (s *Store) GetLatestPRVerification(prURL string) (*PRVerification, error) {
	var v PRVerification
	err := s.DB.QueryRow(`
		SELECT pr_url, verdict, reasoning, COALESCE(agent,''), created_at
		FROM pr_verifications
		WHERE pr_url=?
		ORDER BY created_at DESC, id DESC LIMIT 1
	`, prURL).Scan(&v.PRURL, &v.Verdict, &v.Reasoning, &v.Agent, &v.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// ListPRVerifications returns the full audit trail for a PR, oldest
// first. Used by feature_get_materials to surface review history.
// Tiebreak on id ASC for same-second inserts.
func (s *Store) ListPRVerifications(prURL string) ([]PRVerification, error) {
	rows, err := s.DB.Query(`
		SELECT pr_url, verdict, reasoning, COALESCE(agent,''), created_at
		FROM pr_verifications WHERE pr_url=?
		ORDER BY created_at ASC, id ASC
	`, prURL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PRVerification
	for rows.Next() {
		var v PRVerification
		if err := rows.Scan(&v.PRURL, &v.Verdict, &v.Reasoning,
			&v.Agent, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
