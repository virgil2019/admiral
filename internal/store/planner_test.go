package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// --- features ---

func TestInsertFeature_RoundTrip(t *testing.T) {
	s := newTestStore(t)
	f := Feature{
		ID:               "f-1",
		Name:             "user-login",
		LinearProjectID:  "proj-1",
		RequirementsText: "build login with email + password",
		SourceAgent:      "claude",
	}
	if err := s.InsertFeature(f); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := s.GetFeature("f-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil {
		t.Fatal("expected feature, got nil")
	}
	if got.Name != "user-login" || got.LinearProjectID != "proj-1" ||
		got.RequirementsText != "build login with email + password" ||
		got.SourceAgent != "claude" {
		t.Fatalf("unexpected feature: %+v", got)
	}
	if got.CreatedAt == "" {
		t.Fatal("CreatedAt should be auto-populated")
	}
	if got.ClosedAt != "" {
		t.Fatalf("ClosedAt should be empty on insert, got %q", got.ClosedAt)
	}
}

func TestInsertFeature_RequiresFields(t *testing.T) {
	s := newTestStore(t)
	cases := []struct {
		name string
		f    Feature
	}{
		{"no id", Feature{Name: "x", LinearProjectID: "p"}},
		{"no name", Feature{ID: "f", LinearProjectID: "p"}},
		{"no project", Feature{ID: "f", Name: "x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.InsertFeature(tc.f); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestInsertFeature_DuplicateProjectFails(t *testing.T) {
	s := newTestStore(t)
	if err := s.InsertFeature(Feature{ID: "f-1", Name: "a", LinearProjectID: "p-1"}); err != nil {
		t.Fatalf("first: %v", err)
	}
	err := s.InsertFeature(Feature{ID: "f-2", Name: "b", LinearProjectID: "p-1"})
	if err == nil {
		t.Fatal("expected UNIQUE violation on linear_project_id, got nil")
	}
	if !strings.Contains(err.Error(), "UNIQUE") && !strings.Contains(err.Error(), "constraint") {
		t.Fatalf("expected UNIQUE/constraint error, got: %v", err)
	}
}

func TestGetFeature_NotFound(t *testing.T) {
	s := newTestStore(t)
	got, err := s.GetFeature("nope")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
}

func TestGetFeatureByLinearProject(t *testing.T) {
	s := newTestStore(t)
	if err := s.InsertFeature(Feature{ID: "f-1", Name: "a", LinearProjectID: "p-1"}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := s.GetFeatureByLinearProject("p-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil || got.ID != "f-1" {
		t.Fatalf("unexpected: %+v", got)
	}
	miss, err := s.GetFeatureByLinearProject("p-nope")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if miss != nil {
		t.Fatalf("expected nil for missing project, got %+v", miss)
	}
}

func TestListFeaturesByName_MultipleMatches_NewestFirst(t *testing.T) {
	s := newTestStore(t)
	older := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	newer := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	if err := s.InsertFeature(Feature{
		ID: "f-old", Name: "auth", LinearProjectID: "p-old", CreatedAt: older,
	}); err != nil {
		t.Fatalf("insert old: %v", err)
	}
	if err := s.InsertFeature(Feature{
		ID: "f-new", Name: "auth", LinearProjectID: "p-new", CreatedAt: newer,
	}); err != nil {
		t.Fatalf("insert new: %v", err)
	}
	got, err := s.ListFeaturesByName("auth")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2, got %d", len(got))
	}
	if got[0].ID != "f-new" || got[1].ID != "f-old" {
		t.Fatalf("expected newest first, got %+v", got)
	}
}

func TestCloseFeature_TransitionsAndIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	if err := s.InsertFeature(Feature{ID: "f-1", Name: "a", LinearProjectID: "p-1"}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// First close: open → closed, returns true.
	closed, err := s.CloseFeature("f-1")
	if err != nil {
		t.Fatalf("close 1: %v", err)
	}
	if !closed {
		t.Fatal("first close should return true")
	}
	got, _ := s.GetFeature("f-1")
	if got.ClosedAt == "" {
		t.Fatal("ClosedAt should be set after first close")
	}
	firstClosedAt := got.ClosedAt

	// Second close: already closed, returns (false, nil), no stamp overwrite.
	time.Sleep(10 * time.Millisecond)
	closed, err = s.CloseFeature("f-1")
	if err != nil {
		t.Fatalf("close 2: %v", err)
	}
	if closed {
		t.Fatal("re-close on already-closed feature should return false")
	}
	got, _ = s.GetFeature("f-1")
	if got.ClosedAt != firstClosedAt {
		t.Fatalf("ClosedAt should not change on re-close, was %q now %q",
			firstClosedAt, got.ClosedAt)
	}
}

func TestCloseFeature_MissingIDReturnsErrFeatureNotFound(t *testing.T) {
	s := newTestStore(t)
	closed, err := s.CloseFeature("does-not-exist")
	if !errors.Is(err, ErrFeatureNotFound) {
		t.Fatalf("expected ErrFeatureNotFound, got: %v", err)
	}
	if closed {
		t.Fatal("missing id must not report closed=true")
	}
}

// --- InsertFeatureWithIssues (transactional) ---

func TestInsertFeatureWithIssues_AtomicSuccess(t *testing.T) {
	s := newTestStore(t)
	f := Feature{
		ID: "f-tx", Name: "tx-feat", LinearProjectID: "p-tx",
		RequirementsText: "req",
	}
	issues := []FeatureIssue{
		{LinearIssueID: "i-1", AcceptanceCriteria: "c1"},
		{LinearIssueID: "i-2", AcceptanceCriteria: "c2"},
		{LinearIssueID: "i-3", AcceptanceCriteria: "c3"},
	}
	if err := s.InsertFeatureWithIssues(f, issues); err != nil {
		t.Fatalf("insert: %v", err)
	}
	gotF, _ := s.GetFeature("f-tx")
	if gotF == nil {
		t.Fatal("feature missing")
	}
	gotIssues, _ := s.ListFeatureIssues("f-tx")
	if len(gotIssues) != 3 {
		t.Fatalf("want 3 issues, got %d", len(gotIssues))
	}
	for _, fi := range gotIssues {
		if fi.FeatureID != "f-tx" {
			t.Fatalf("FeatureID should be forced to f-tx, got %q", fi.FeatureID)
		}
	}
}

func TestInsertFeatureWithIssues_RollsBackOnIssueError(t *testing.T) {
	s := newTestStore(t)
	f := Feature{ID: "f-rb", Name: "rb", LinearProjectID: "p-rb"}
	issues := []FeatureIssue{
		{LinearIssueID: "i-1", AcceptanceCriteria: "c1"},
		{LinearIssueID: "", AcceptanceCriteria: "missing id"}, // triggers validation error
	}
	err := s.InsertFeatureWithIssues(f, issues)
	if err == nil {
		t.Fatal("expected error from empty linear_issue_id")
	}
	// Feature row must NOT exist — tx rolled back.
	gotF, _ := s.GetFeature("f-rb")
	if gotF != nil {
		t.Fatalf("feature should not exist after rollback, got %+v", gotF)
	}
	gotIssues, _ := s.ListFeatureIssues("f-rb")
	if len(gotIssues) != 0 {
		t.Fatalf("issues should not exist after rollback, got %d", len(gotIssues))
	}
}

// --- feature_issues ---

func TestUpsertFeatureIssue_InsertAndUpdate(t *testing.T) {
	s := newTestStore(t)
	_ = s.InsertFeature(Feature{ID: "f-1", Name: "a", LinearProjectID: "p-1"})

	if err := s.UpsertFeatureIssue(FeatureIssue{
		FeatureID:          "f-1",
		LinearIssueID:      "issue-1",
		AcceptanceCriteria: "v1 criteria",
	}); err != nil {
		t.Fatalf("upsert 1: %v", err)
	}
	got, _ := s.GetFeatureIssue("f-1", "issue-1")
	if got == nil || got.AcceptanceCriteria != "v1 criteria" {
		t.Fatalf("after insert: %+v", got)
	}

	// Refine criteria — must overwrite.
	if err := s.UpsertFeatureIssue(FeatureIssue{
		FeatureID:          "f-1",
		LinearIssueID:      "issue-1",
		AcceptanceCriteria: "v2 criteria refined",
	}); err != nil {
		t.Fatalf("upsert 2: %v", err)
	}
	got, _ = s.GetFeatureIssue("f-1", "issue-1")
	if got.AcceptanceCriteria != "v2 criteria refined" {
		t.Fatalf("expected refined criteria, got %q", got.AcceptanceCriteria)
	}
}

func TestUpsertFeatureIssue_RequiresFields(t *testing.T) {
	s := newTestStore(t)
	if err := s.UpsertFeatureIssue(FeatureIssue{LinearIssueID: "i-1"}); err == nil {
		t.Fatal("expected error: missing feature_id")
	}
	if err := s.UpsertFeatureIssue(FeatureIssue{FeatureID: "f-1"}); err == nil {
		t.Fatal("expected error: missing linear_issue_id")
	}
}

func TestUpsertFeatureIssue_FKEnforcedOnBogusFeature(t *testing.T) {
	// FK on feature_issues.feature_id is enforced because
	// PRAGMA foreign_keys=ON is set in Open(). Inserting an issue
	// against a non-existent feature must error.
	s := newTestStore(t)
	err := s.UpsertFeatureIssue(FeatureIssue{
		FeatureID:          "nope",
		LinearIssueID:      "i-1",
		AcceptanceCriteria: "c",
	})
	if err == nil {
		t.Fatal("expected FK violation, got nil")
	}
	if !strings.Contains(err.Error(), "FOREIGN KEY") && !strings.Contains(err.Error(), "constraint") {
		t.Fatalf("expected FK error, got: %v", err)
	}
}

func TestListFeatureIssues_OrderedByCreation(t *testing.T) {
	s := newTestStore(t)
	_ = s.InsertFeature(Feature{ID: "f-1", Name: "a", LinearProjectID: "p-1"})

	t1 := time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339)
	t2 := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	t3 := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	_ = s.UpsertFeatureIssue(FeatureIssue{FeatureID: "f-1", LinearIssueID: "i-a", AcceptanceCriteria: "a", CreatedAt: t2})
	_ = s.UpsertFeatureIssue(FeatureIssue{FeatureID: "f-1", LinearIssueID: "i-b", AcceptanceCriteria: "b", CreatedAt: t1})
	_ = s.UpsertFeatureIssue(FeatureIssue{FeatureID: "f-1", LinearIssueID: "i-c", AcceptanceCriteria: "c", CreatedAt: t3})

	got, err := s.ListFeatureIssues("f-1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3, got %d", len(got))
	}
	want := []string{"i-b", "i-a", "i-c"} // oldest first
	for i, fi := range got {
		if fi.LinearIssueID != want[i] {
			t.Fatalf("pos %d: want %s, got %s", i, want[i], fi.LinearIssueID)
		}
	}
}

func TestFindFeatureByIssue_PicksMostRecentLink(t *testing.T) {
	// When the same Linear issue is linked under two features, the
	// query must return the most recently linked one (deterministic).
	s := newTestStore(t)
	_ = s.InsertFeature(Feature{ID: "f-old", Name: "a", LinearProjectID: "p-old"})
	_ = s.InsertFeature(Feature{ID: "f-new", Name: "b", LinearProjectID: "p-new"})

	t1 := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	t2 := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	_ = s.UpsertFeatureIssue(FeatureIssue{
		FeatureID: "f-old", LinearIssueID: "moved", AcceptanceCriteria: "old", CreatedAt: t1,
	})
	_ = s.UpsertFeatureIssue(FeatureIssue{
		FeatureID: "f-new", LinearIssueID: "moved", AcceptanceCriteria: "new", CreatedAt: t2,
	})

	got, err := s.FindFeatureByIssue("moved")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if got == nil || got.ID != "f-new" {
		t.Fatalf("expected f-new (most recent link), got %+v", got)
	}
}

func TestFindFeatureByIssue_OrphanReturnsNil(t *testing.T) {
	s := newTestStore(t)
	miss, err := s.FindFeatureByIssue("never-linked")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if miss != nil {
		t.Fatalf("expected nil for orphan issue, got %+v", miss)
	}
}

// --- pr_verifications ---

func TestInsertPRVerification_RoundTrip(t *testing.T) {
	s := newTestStore(t)
	v := PRVerification{
		PRURL:     "https://github.com/o/r/pull/1",
		Verdict:   PRVerdictApprove,
		Reasoning: "matches all criteria",
		Agent:     "claude",
	}
	if err := s.InsertPRVerification(v); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := s.GetLatestPRVerification(v.PRURL)
	if err != nil {
		t.Fatalf("get latest: %v", err)
	}
	if got == nil || got.Verdict != PRVerdictApprove || got.Reasoning != "matches all criteria" {
		t.Fatalf("unexpected: %+v", got)
	}
	if got.CreatedAt == "" {
		t.Fatal("CreatedAt should be auto-populated")
	}
}

func TestInsertPRVerification_RequiresFields(t *testing.T) {
	s := newTestStore(t)
	if err := s.InsertPRVerification(PRVerification{Verdict: "approve"}); err == nil {
		t.Fatal("expected error: missing pr_url")
	}
	if err := s.InsertPRVerification(PRVerification{PRURL: "x"}); err == nil {
		t.Fatal("expected error: missing verdict")
	}
}

func TestInsertPRVerification_RejectsInvalidVerdict(t *testing.T) {
	s := newTestStore(t)
	// Typo (common bug: "approved" instead of "approve").
	err := s.InsertPRVerification(PRVerification{
		PRURL:   "https://github.com/o/r/pull/1",
		Verdict: "approved",
	})
	if !errors.Is(err, ErrInvalidVerdict) {
		t.Fatalf("expected ErrInvalidVerdict, got: %v", err)
	}
}

func TestInsertPRVerification_AllowsSameSecondInserts(t *testing.T) {
	// Synthetic INTEGER PK + auto-populated RFC3339 second-precision
	// timestamp must NOT collide on rapid back-to-back inserts.
	s := newTestStore(t)
	pr := "https://github.com/o/r/pull/9"
	v := PRVerification{PRURL: pr, Verdict: PRVerdictRequestChanges, Reasoning: "x"}
	for i := 0; i < 5; i++ {
		if err := s.InsertPRVerification(v); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	got, _ := s.ListPRVerifications(pr)
	if len(got) != 5 {
		t.Fatalf("want 5 rows, got %d", len(got))
	}
}

func TestGetLatestPRVerification_PicksMostRecent(t *testing.T) {
	s := newTestStore(t)
	pr := "https://github.com/o/r/pull/2"

	older := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	newer := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	_ = s.InsertPRVerification(PRVerification{
		PRURL: pr, Verdict: PRVerdictRequestChanges, Reasoning: "v1", CreatedAt: older,
	})
	_ = s.InsertPRVerification(PRVerification{
		PRURL: pr, Verdict: PRVerdictApprove, Reasoning: "v2", CreatedAt: newer,
	})
	got, err := s.GetLatestPRVerification(pr)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Verdict != PRVerdictApprove {
		t.Fatalf("expected newest (approve), got %q", got.Verdict)
	}
}

func TestGetLatestPRVerification_SameSecondTieBreakOnID(t *testing.T) {
	// Two verdicts with identical created_at — the autoincrement id
	// tiebreak (id DESC) must return the later-inserted one.
	s := newTestStore(t)
	pr := "https://github.com/o/r/pull/tie"
	stamp := time.Now().UTC().Format(time.RFC3339)
	_ = s.InsertPRVerification(PRVerification{
		PRURL: pr, Verdict: PRVerdictRequestChanges, Reasoning: "first", CreatedAt: stamp,
	})
	_ = s.InsertPRVerification(PRVerification{
		PRURL: pr, Verdict: PRVerdictApprove, Reasoning: "second", CreatedAt: stamp,
	})
	got, err := s.GetLatestPRVerification(pr)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Reasoning != "second" {
		t.Fatalf("expected later-inserted row, got %+v", got)
	}
}

func TestGetLatestPRVerification_NotFound(t *testing.T) {
	s := newTestStore(t)
	got, err := s.GetLatestPRVerification("https://nope")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
}

func TestListPRVerifications_OldestFirst(t *testing.T) {
	s := newTestStore(t)
	pr := "https://github.com/o/r/pull/3"

	t1 := time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339)
	t2 := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	t3 := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	_ = s.InsertPRVerification(PRVerification{PRURL: pr, Verdict: PRVerdictRequestChanges, CreatedAt: t2})
	_ = s.InsertPRVerification(PRVerification{PRURL: pr, Verdict: PRVerdictNeedsRebase, CreatedAt: t1})
	_ = s.InsertPRVerification(PRVerification{PRURL: pr, Verdict: PRVerdictApprove, CreatedAt: t3})

	got, err := s.ListPRVerifications(pr)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3, got %d", len(got))
	}
	want := []string{PRVerdictNeedsRebase, PRVerdictRequestChanges, PRVerdictApprove}
	for i, v := range got {
		if v.Verdict != want[i] {
			t.Fatalf("pos %d: want %s, got %s", i, want[i], v.Verdict)
		}
	}
}

// --- migration smoke ---

func TestMigration0018_TablesPresent(t *testing.T) {
	s := newTestStore(t)
	for _, tbl := range []string{"features", "feature_issues", "pr_verifications"} {
		if !tableExists(s.DB, tbl) {
			t.Fatalf("table %s missing after migrations", tbl)
		}
	}
}

func TestMigration0018_CheckConstraintRejectsBogusVerdict(t *testing.T) {
	// Belt-and-suspenders: even if a future caller bypasses
	// isValidVerdict(), the DB CHECK must reject bogus verdicts.
	s := newTestStore(t)
	_, err := s.DB.Exec(`
		INSERT INTO pr_verifications(pr_url, verdict, reasoning, agent, created_at)
		VALUES(?, ?, ?, ?, ?)
	`, "https://x", "bogus_verdict", "", "", time.Now().UTC().Format(time.RFC3339))
	if err == nil {
		t.Fatal("expected CHECK constraint error, got nil")
	}
	if !strings.Contains(err.Error(), "CHECK") && !strings.Contains(err.Error(), "constraint") {
		t.Fatalf("expected CHECK/constraint error, got: %v", err)
	}
}
