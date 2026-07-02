package discoverer

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/georgehuang/admiral/internal/store"
)

// openTestStore returns a fresh *store.Store backed by a tempdir SQLite —
// mirrors TestBackfillMigrations_LegacyDB's fixture shape (raw open via
// store.Open, t.Cleanup close). The rescan helper only needs read access
// to task_verifications + product_verifications + events_inbox, all of
// which Open+migrations creates.
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "rescan.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// silentLogger swallows log output during tests so a failing assertion
// isn't drowned in Info noise. Errors are preserved by routing through
// the helper's own warn paths anyway.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// insertTaskRow inserts a task_verifications row with the given status /
// rounds / summary. usedErrf is a tiny helper to keep the test bodies
// readable — it calls t.Fatalf on a SQL error.
func insertTaskRow(t *testing.T, s *store.Store, parentID string, status string, rounds int, summary string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.DB.Exec(`
		INSERT INTO task_verifications(parent_issue_id, rounds, status, summary, updated_at)
		VALUES(?, ?, ?, ?, ?)
	`, parentID, rounds, status, summary, now); err != nil {
		t.Fatalf("insert task row: %v", err)
	}
}

// insertProductRow inserts a product_verifications row.
func insertProductRow(t *testing.T, s *store.Store, projectID string, status string, rounds int) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.DB.Exec(`
		INSERT INTO product_verifications(project_id, rounds, status, updated_at)
		VALUES(?, ?, ?, ?)
	`, projectID, rounds, status, now); err != nil {
		t.Fatalf("insert product row: %v", err)
	}
}

// insertInFlightEvent manually seeds events_inbox with status='pending'
// for the (source, action, sessionID) tuple — exercises the in-flight
// short-circuit branch in the rescan helper.
func insertInFlightEvent(t *testing.T, s *store.Store, source, action, sessionID, webhookID string) {
	t.Helper()
	now := time.Now().UTC().Unix()
	if _, err := s.DB.Exec(`
		INSERT INTO events_inbox(webhook_id, action, session_id, issue_id, payload_json,
			status, attempts, received_at, source, comment_id)
		VALUES(?, ?, ?, ?, '{}', 'pending', 0, ?, ?, '')
	`, webhookID, action, sessionID, sessionID, now, source); err != nil {
		t.Fatalf("insert in-flight event: %v", err)
	}
}

// countInboxForParent returns the number of events_inbox rows whose
// session_id matches `parentID`.
func countInboxForParent(t *testing.T, s *store.Store, parentID string) int {
	t.Helper()
	var n int
	if err := s.DB.QueryRow(`
		SELECT COUNT(*) FROM events_inbox WHERE session_id=?
	`, parentID).Scan(&n); err != nil {
		t.Fatalf("count inbox: %v", err)
	}
	return n
}

// --- Tests ---

// TestRescan_StuckTaskRowEnqueues is the happy path: a single stuck row
// becomes a verify.task_complete event in events_inbox with the
// expected webhook_id shape.
func TestRescan_StuckTaskRowEnqueues(t *testing.T) {
	s := openTestStore(t)
	insertTaskRow(t, s, "p1", "active", 1, "")

	rep, err := RescanStuckVerifications(context.Background(), s, silentLogger())
	if err != nil {
		t.Fatalf("RescanStuckVerifications: %v", err)
	}
	if rep.TaskScanned != 1 || rep.TaskReEnqueued != 1 {
		t.Errorf("unexpected report: %+v", rep)
	}
	if n := countInboxForParent(t, s, "p1"); n != 1 {
		t.Fatalf("expected 1 inbox row for parent p1, got %d", n)
	}

	// Webhook ID shape + source/action.
	var webhookID, source, action, status string
	if err := s.DB.QueryRow(`
		SELECT webhook_id, source, action, status FROM events_inbox WHERE session_id='p1'
	`).Scan(&webhookID, &source, &action, &status); err != nil {
		t.Fatalf("scan inbox row: %v", err)
	}
	if want := "rescan-p1-1"; webhookID != want {
		t.Errorf("webhook_id = %q, want %q", webhookID, want)
	}
	if source != "verify" || action != "verify.task_complete" {
		t.Errorf("source/action = (%q, %q), want (verify, verify.task_complete)", source, action)
	}
	if status != "pending" {
		t.Errorf("status = %q, want pending (worker will claim on next drain)", status)
	}
}

// TestRescan_ClosedRowSkipped ensures rows already in a terminal state
// are NOT touched (the bug is *only* about 'active' stuck rows).
func TestRescan_ClosedRowSkipped(t *testing.T) {
	s := openTestStore(t)
	insertTaskRow(t, s, "p-closed", "closed", 1, "")

	rep, err := RescanStuckVerifications(context.Background(), s, silentLogger())
	if err != nil {
		t.Fatalf("RescanStuckVerifications: %v", err)
	}
	if rep.TaskScanned != 0 || rep.TaskReEnqueued != 0 {
		t.Errorf("closed row must not be scanned: %+v", rep)
	}
	if n := countInboxForParent(t, s, "p-closed"); n != 0 {
		t.Fatalf("expected 0 inbox rows, got %d", n)
	}
}

// TestRescan_EscalatedRowSkipped: same as Closed for the other terminal
// status. (escalated is what HandleVerifyEvent / applyVerifyVerdict
// falls back to when a verify round can't pick a state and asks for a
// human.)
func TestRescan_EscalatedRowSkipped(t *testing.T) {
	s := openTestStore(t)
	insertTaskRow(t, s, "p-esc", "escalated", 1, "")

	rep, err := RescanStuckVerifications(context.Background(), s, silentLogger())
	if err != nil {
		t.Fatalf("RescanStuckVerifications: %v", err)
	}
	if rep.TaskScanned != 0 || rep.TaskReEnqueued != 0 {
		t.Errorf("escalated row must not be scanned: %+v", rep)
	}
}

// TestRescan_NonEmptySummarySkipped guards the "is this a stuck row?"
// discriminator: summary != '' means a past round completed and wrote
// a digest (e.g. via the auto-pass branch in runVerify). Status is
// still 'active' because the apply step didn't run yet, but the row
// is NOT in the E2BIG-stuck shape — re-running verify on it would
// repeat work.
func TestRescan_NonEmptySummarySkipped(t *testing.T) {
	s := openTestStore(t)
	insertTaskRow(t, s, "p-summary", "active", 1, "auto-pass: parent description has no '## Acceptance Criteria' section")

	rep, err := RescanStuckVerifications(context.Background(), s, silentLogger())
	if err != nil {
		t.Fatalf("RescanStuckVerifications: %v", err)
	}
	if rep.TaskScanned != 0 || rep.TaskReEnqueued != 0 {
		t.Errorf("non-empty summary must not be scanned: %+v", rep)
	}
	if n := countInboxForParent(t, s, "p-summary"); n != 0 {
		t.Fatalf("expected 0 inbox rows, got %d", n)
	}
}

// TestRescan_HigherRoundsSkipped: a row stuck at rounds=2 (one
// successful round, then a second round that didn't finish) is NOT
// the canonical E2BIG shape. The lower bound `rounds=1` is intentional
// — bumping it would re-fire a parent whose first round's work might
// already be stale on the disk. Operators can clear the row manually.
func TestRescan_HigherRoundsSkipped(t *testing.T) {
	s := openTestStore(t)
	insertTaskRow(t, s, "p-higher", "active", 2, "")

	rep, err := RescanStuckVerifications(context.Background(), s, silentLogger())
	if err != nil {
		t.Fatalf("RescanStuckVerifications: %v", err)
	}
	if rep.TaskScanned != 0 || rep.TaskReEnqueued != 0 {
		t.Errorf("rounds>1 row must not be scanned: %+v", rep)
	}
}

// TestRescan_InFlightEventSkipped: a stuck task row + a matching
// in-flight events_inbox row → the rescan must skip the duplicate
// (the worker will deliver the in-flight one). Counter incremented
// on TaskSkippedInFlight.
func TestRescan_InFlightEventSkipped(t *testing.T) {
	s := openTestStore(t)
	insertTaskRow(t, s, "p-inflight", "active", 1, "")
	insertInFlightEvent(t, s, "verify", "verify.task_complete", "p-inflight", "verify-p-inflight-predecessor")

	rep, err := RescanStuckVerifications(context.Background(), s, silentLogger())
	if err != nil {
		t.Fatalf("RescanStuckVerifications: %v", err)
	}
	if rep.TaskScanned != 1 || rep.TaskSkippedInFlight != 1 || rep.TaskReEnqueued != 0 {
		t.Errorf("unexpected report: %+v", rep)
	}
	if n := countInboxForParent(t, s, "p-inflight"); n != 1 {
		t.Fatalf("expected still 1 inbox row (the in-flight one), got %d", n)
	}
	var status string
	if err := s.DB.QueryRow(`SELECT status FROM events_inbox WHERE session_id='p-inflight'`).Scan(&status); err != nil {
		t.Fatalf("scan status: %v", err)
	}
	if status != "pending" {
		t.Errorf("status = %q, want pending (rescan must not have mutated the existing in-flight row)", status)
	}
}

// TestRescan_IdempotentAcrossReinvocations: invoking the helper twice
// cannot double-fire the same parent. Second call hits the
// webhook_id PRIMARY KEY dedup and returns (false, nil) from
// EnqueueEventWithSource; report counts zero new rows.
func TestRescan_IdempotentAcrossReinvocations(t *testing.T) {
	s := openTestStore(t)
	insertTaskRow(t, s, "p-idem", "active", 1, "")

	rep1, err := RescanStuckVerifications(context.Background(), s, silentLogger())
	if err != nil {
		t.Fatalf("first rescan: %v", err)
	}
	if rep1.TaskReEnqueued != 1 {
		t.Errorf("first rescan: %+v", rep1)
	}

	rep2, err := RescanStuckVerifications(context.Background(), s, silentLogger())
	if err != nil {
		t.Fatalf("second rescan: %v", err)
	}
	// Second call still scans (sees the row), but the dedup turns
	// the enqueue into a no-op. ReEnqueued stays at 0 — the row's
	// webhook_id is already present.
	if rep2.TaskScanned != 1 || rep2.TaskReEnqueued != 0 {
		t.Errorf("second rescan unexpectedly enqueued: %+v", rep2)
	}
	if n := countInboxForParent(t, s, "p-idem"); n != 1 {
		t.Fatalf("expected 1 inbox row after two rescans, got %d", n)
	}
}

// TestRescan_ProductRowEnqueues: parallel shape for the product-side
// table. Different source + action + webhook_id prefix.
func TestRescan_ProductRowEnqueues(t *testing.T) {
	s := openTestStore(t)
	insertProductRow(t, s, "proj-1", "active", 1)

	rep, err := RescanStuckVerifications(context.Background(), s, silentLogger())
	if err != nil {
		t.Fatalf("RescanStuckVerifications: %v", err)
	}
	if rep.ProductScanned != 1 || rep.ProductReEnqueued != 1 {
		t.Errorf("unexpected report: %+v", rep)
	}

	var webhookID, source, action string
	if err := s.DB.QueryRow(`
		SELECT webhook_id, source, action FROM events_inbox WHERE session_id='proj-1'
	`).Scan(&webhookID, &source, &action); err != nil {
		t.Fatalf("scan inbox row: %v", err)
	}
	if want := "rescan-product-proj-1-1"; webhookID != want {
		t.Errorf("webhook_id = %q, want %q", webhookID, want)
	}
	if source != "product-verify" {
		t.Errorf("source = %q, want product-verify", source)
	}
	if action != "product_verify.task_complete" {
		t.Errorf("action = %q, want product_verify.task_complete", action)
	}
}

// TestRescan_BothTablesInOneCall exercises both legs of the helper
// simultaneously and asserts each side's report counters are
// independent (i.e. the task loop's bug cannot bleed into the product
// loop's count).
func TestRescan_BothTablesInOneCall(t *testing.T) {
	s := openTestStore(t)
	insertTaskRow(t, s, "task-A", "active", 1, "")
	insertTaskRow(t, s, "task-B", "active", 1, "")
	insertProductRow(t, s, "prod-A", "active", 1)

	rep, err := RescanStuckVerifications(context.Background(), s, silentLogger())
	if err != nil {
		t.Fatalf("RescanStuckVerifications: %v", err)
	}
	if rep.TaskScanned != 2 || rep.TaskReEnqueued != 2 {
		t.Errorf("task side: %+v", rep)
	}
	if rep.ProductScanned != 1 || rep.ProductReEnqueued != 1 {
		t.Errorf("product side: %+v", rep)
	}
	if rep.TaskSkippedInFlight != 0 || rep.ProductSkippedInFlight != 0 {
		t.Errorf("nothing should have been skipped: %+v", rep)
	}
	if taskN, prodN := countInboxForParent(t, s, "task-A"), countInboxForParent(t, s, "prod-A"); taskN != 1 || prodN != 1 {
		t.Errorf("per-parent inbox counts wrong: task-A=%d prod-A=%d", taskN, prodN)
	}
}
