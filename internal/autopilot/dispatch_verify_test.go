package autopilot

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/georgehuang/admiral/internal/config"
	"github.com/georgehuang/admiral/internal/linear"
	"github.com/georgehuang/admiral/internal/store"
)

// newVerifyOrchestrator builds an Orchestrator wired for verify-loop tests:
// stubbed clients, a no-op run slot, and a verifyRunner the test supplies.
func newVerifyOrchestrator(db *mockStore, lc *mockLinearClient, pr *mockPRClient, maxRounds int, runner func(ctx context.Context, repoDir, prompt string) (string, error)) *Orchestrator {
	return &Orchestrator{
		db:           db,
		lc:           lc,
		prClient:     pr,
		logger:       slog.Default(),
		cfg:          &config.Autopilot{VerifyMaxRounds: maxRounds, MaxRunSeconds: 60},
		verifyRunner: runner,
	}
}

func TestHandleVerifyEvent_SkipTerminalStatus(t *testing.T) {
	db := &mockStore{TaskVerification: &store.TaskVerification{
		ParentIssueID: "p1", Rounds: 2, Status: store.TaskVerifyClosed,
	}}
	o := newVerifyOrchestrator(db, &mockLinearClient{}, &mockPRClient{}, 3, nil)

	o.HandleVerifyEvent(context.Background(), "p1")

	if len(db.BumpCalls) != 0 {
		t.Errorf("expected no bump on a closed verification, got %v", db.BumpCalls)
	}
}

func TestHandleVerifyEvent_RoundCapEscalates(t *testing.T) {
	db := &mockStore{
		TaskVerification:       &store.TaskVerification{ParentIssueID: "p1", Rounds: 3, Status: store.TaskVerifyActive},
		BumpedTaskVerification: &store.TaskVerification{ParentIssueID: "p1", Rounds: 4, Status: store.TaskVerifyActive},
	}
	lc := &mockLinearClient{}
	o := newVerifyOrchestrator(db, lc, &mockPRClient{}, 3, func(context.Context, string, string) (string, error) {
		t.Fatal("verifyRunner must not run past the round cap")
		return "", nil
	})

	o.HandleVerifyEvent(context.Background(), "p1")

	if len(lc.CreatedComments) != 1 || lc.CreatedComments[0].IssueID != "p1" {
		t.Fatalf("expected one escalation comment on p1, got %v", lc.CreatedComments)
	}
	if len(db.SetStatusCalls) != 1 || db.SetStatusCalls[0].Status != store.TaskVerifyEscalated {
		t.Fatalf("expected status set to escalated, got %v", db.SetStatusCalls)
	}
}

func TestGatherVerifyMaterial(t *testing.T) {
	parent := &linear.Issue{Identifier: "GEO-1", Description: "Build login", TeamID: "team-1", ProjectID: "proj-1"}
	lc := &mockLinearClient{
		GetIssueByID: map[string]*linear.Issue{
			"p1":    parent,
			"sub-1": {Identifier: "GEO-2", Title: "email form"},
		},
		SubIssues: []linear.SubIssue{{ID: "sub-1", Identifier: "GEO-2", StateType: "completed"}},
	}
	db := &mockStore{
		Repo:        &store.Repo{RepoDir: "/repo"},
		AdmiralTask: &store.AdmiralTask{PRURL: "https://gh/pr/1"},
	}
	pr := &mockPRClient{getDiffVal: "+ login code"}
	o := newVerifyOrchestrator(db, lc, pr, 3, nil)

	mat, repoDir, teamID, projectID, verifyCmd, err := o.gatherVerifyMaterial(context.Background(), "p1")
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	if repoDir != "/repo" || teamID != "team-1" || projectID != "proj-1" {
		t.Errorf("unexpected repo/team/project: %q %q %q", repoDir, teamID, projectID)
	}
	if verifyCmd != "" {
		t.Errorf("expected empty verifyCmd (none configured in mock repo), got %q", verifyCmd)
	}
	if mat.PRD != "Build login" || len(mat.Subs) != 1 {
		t.Fatalf("unexpected material: %+v", mat)
	}
	s := mat.Subs[0]
	if s.Identifier != "GEO-2" || s.Title != "email form" || s.PRURL != "https://gh/pr/1" || s.Diff != "+ login code" {
		t.Errorf("sub material not wired: %+v", s)
	}
}

func TestApplyVerifyVerdict_CompleteClosesTask(t *testing.T) {
	lc := &mockLinearClient{WorkflowStates: []linear.WorkflowState{
		{ID: "st-done", Type: "completed", Position: 0},
	}}
	db := &mockStore{}
	o := newVerifyOrchestrator(db, lc, &mockPRClient{}, 3, nil)

	o.applyVerifyVerdict(context.Background(), "p1", "GEO-1", "team-1", "proj-1",
		&verifyVerdict{Complete: true, Summary: "all shipped"})

	if len(lc.IssueUpdateCalls) != 1 || lc.IssueUpdateCalls[0].StateID != "st-done" {
		t.Fatalf("expected parent flipped to completed state, got %v", lc.IssueUpdateCalls)
	}
	if len(db.SetStatusCalls) != 1 || db.SetStatusCalls[0].Status != store.TaskVerifyClosed {
		t.Fatalf("expected verification closed, got %v", db.SetStatusCalls)
	}
	if len(lc.IssueCreateInputs) != 0 {
		t.Errorf("complete verdict must not file gaps, got %v", lc.IssueCreateInputs)
	}
}

func TestApplyVerifyVerdict_GapsFileSubIssues(t *testing.T) {
	lc := &mockLinearClient{
		WorkflowStates: []linear.WorkflowState{{ID: "st-backlog", Type: "backlog", Position: 0}},
		TeamLabelID:    "lbl-agent-ready",
	}
	db := &mockStore{}
	o := newVerifyOrchestrator(db, lc, &mockPRClient{}, 3, nil)
	o.verifyLabel = "agent-ready"
	o.verifyStateTypes = []string{"backlog"}

	o.applyVerifyVerdict(context.Background(), "p1", "GEO-1", "team-1", "proj-1", &verifyVerdict{
		Complete: false,
		Gaps: []verifyGap{
			{Title: "Add logout", Description: "no logout", AcceptanceCriteria: "DELETE /session clears cookie"},
		},
	})

	if len(lc.IssueCreateInputs) != 1 {
		t.Fatalf("expected one follow-up sub-issue, got %d", len(lc.IssueCreateInputs))
	}
	in := lc.IssueCreateInputs[0]
	if in.ParentID != "p1" || in.TeamID != "team-1" || in.ProjectID != "proj-1" {
		t.Errorf("follow-up not attached/routed correctly: %+v", in)
	}
	if in.StateID != "st-backlog" || len(in.LabelIDs) != 1 || in.LabelIDs[0] != "lbl-agent-ready" {
		t.Errorf("follow-up missing pickup gates: %+v", in)
	}
	if !strings.Contains(in.Description, "DELETE /session clears cookie") {
		t.Errorf("acceptance criteria not in body: %q", in.Description)
	}
	// Gaps leave the verification active (loop continues) — no status write.
	if len(db.SetStatusCalls) != 0 {
		t.Errorf("gaps must not set a terminal status, got %v", db.SetStatusCalls)
	}
}

func TestApplyVerifyVerdict_CompleteCascadesToGrandparent(t *testing.T) {
	// Three-level tree: grandparent (G) → parent (P) → leaves L1, L2.
	// L1, L2 already shipped + L1 just triggered P's verify; verdict is
	// complete. After applying, the cascade hop must enqueue a verify
	// event for G — assuming G's other children (just P here) are now
	// completed too.
	lc := &mockLinearClient{
		WorkflowStates: []linear.WorkflowState{{ID: "st-done", Type: "completed", Position: 0}},
		ParentByChild:  map[string]string{"P": "G"},
		SubIssuesByParent: map[string][]linear.SubIssue{
			"G": {{ID: "P", Identifier: "GEO-P", StateType: "completed"}},
		},
	}
	db := &mockStore{}
	o := newVerifyOrchestrator(db, lc, &mockPRClient{}, 3, nil)

	o.applyVerifyVerdict(context.Background(), "P", "GEO-P", "team-1", "proj-1",
		&verifyVerdict{Complete: true, Summary: "P shipped its sub-tasks"})

	if len(db.Enqueued) != 1 {
		t.Fatalf("expected one cascade enqueue, got %+v", db.Enqueued)
	}
	e := db.Enqueued[0]
	if e.Source != "verify" || e.IssueID != "G" || e.SessionID != "G" {
		t.Errorf("cascade event misrouted: %+v", e)
	}
	if e.WebhookID != "verify-G-P" {
		t.Errorf("webhook_id = %q, want verify-G-P (each hop's trigger differs)", e.WebhookID)
	}
	// Summary must be persisted for upper-layer verify to pick up later.
	if len(db.SetSummaryCalls) != 1 ||
		db.SetSummaryCalls[0].ParentID != "P" ||
		db.SetSummaryCalls[0].Summary != "P shipped its sub-tasks" {
		t.Errorf("summary not persisted: %+v", db.SetSummaryCalls)
	}
}

func TestApplyVerifyVerdict_CompleteStopsAtTopOfTree(t *testing.T) {
	// P has no parent — applying the complete verdict must NOT enqueue.
	lc := &mockLinearClient{
		WorkflowStates: []linear.WorkflowState{{ID: "st-done", Type: "completed", Position: 0}},
		ParentByChild:  map[string]string{}, // no parent for P
	}
	db := &mockStore{}
	o := newVerifyOrchestrator(db, lc, &mockPRClient{}, 3, nil)

	o.applyVerifyVerdict(context.Background(), "P", "GEO-P", "team-1", "proj-1",
		&verifyVerdict{Complete: true, Summary: "top-of-tree task done"})

	if len(db.Enqueued) != 0 {
		t.Errorf("top-of-tree must not cascade, got %+v", db.Enqueued)
	}
	if len(db.SetSummaryCalls) != 1 {
		t.Errorf("summary should still be persisted at top-of-tree, got %+v", db.SetSummaryCalls)
	}
}

func TestApplyVerifyVerdict_CompleteDoesNotCascadeWhenSiblingIncomplete(t *testing.T) {
	// G has two children: P (just completed) and SibP (still in flight).
	// The cascade walk reads G's children and finds SibP incomplete, so
	// no verify enqueues for G yet — it'll re-check when SibP finishes.
	lc := &mockLinearClient{
		WorkflowStates: []linear.WorkflowState{{ID: "st-done", Type: "completed", Position: 0}},
		ParentByChild:  map[string]string{"P": "G"},
		SubIssuesByParent: map[string][]linear.SubIssue{
			"G": {
				{ID: "P", StateType: "completed"},
				{ID: "SibP", StateType: "started"},
			},
		},
	}
	db := &mockStore{}
	o := newVerifyOrchestrator(db, lc, &mockPRClient{}, 3, nil)

	o.applyVerifyVerdict(context.Background(), "P", "GEO-P", "team-1", "proj-1",
		&verifyVerdict{Complete: true, Summary: "P done; sibling still pending"})

	if len(db.Enqueued) != 0 {
		t.Errorf("must not cascade while a G-child is incomplete, got %+v", db.Enqueued)
	}
}

func TestApplyVerifyVerdict_GapsPersistSummary(t *testing.T) {
	// Even on a not-complete verdict (gaps filed), the judge's summary is
	// persisted so the same row carries the latest digest for any
	// upper-layer verify that later reads it.
	lc := &mockLinearClient{
		WorkflowStates: []linear.WorkflowState{{ID: "st-backlog", Type: "backlog", Position: 0}},
		TeamLabelID:    "lbl-agent-ready",
	}
	db := &mockStore{}
	o := newVerifyOrchestrator(db, lc, &mockPRClient{}, 3, nil)
	o.verifyLabel = "agent-ready"
	o.verifyStateTypes = []string{"backlog"}

	o.applyVerifyVerdict(context.Background(), "p1", "GEO-1", "team-1", "proj-1", &verifyVerdict{
		Complete: false,
		Summary:  "missing logout",
		Gaps:     []verifyGap{{Title: "Add logout", Description: "no logout"}},
	})

	if len(db.SetSummaryCalls) != 1 || db.SetSummaryCalls[0].Summary != "missing logout" {
		t.Errorf("summary not persisted on gaps branch: %+v", db.SetSummaryCalls)
	}
}

func TestApplyVerifyVerdict_UnpickableGatesEscalate(t *testing.T) {
	// Label gate configured but unresolvable (GetTeamLabelID returns "") →
	// filing orphan follow-ups would stall the loop, so escalate instead.
	lc := &mockLinearClient{TeamLabelID: ""}
	db := &mockStore{}
	o := newVerifyOrchestrator(db, lc, &mockPRClient{}, 3, nil)
	o.verifyLabel = "agent-ready"
	o.verifyStateTypes = nil // state gate unconfigured = valid, skipped

	o.applyVerifyVerdict(context.Background(), "p1", "GEO-1", "team-1", "proj-1", &verifyVerdict{
		Complete: false,
		Gaps:     []verifyGap{{Title: "Add logout", Description: "no logout"}},
	})

	if len(lc.IssueCreateInputs) != 0 {
		t.Errorf("must not file orphan follow-ups when a gate is unresolvable, got %v", lc.IssueCreateInputs)
	}
	if len(lc.CreatedComments) != 1 || lc.CreatedComments[0].IssueID != "p1" {
		t.Fatalf("expected one escalation comment on p1, got %v", lc.CreatedComments)
	}
	if len(db.SetStatusCalls) != 1 || db.SetStatusCalls[0].Status != store.TaskVerifyEscalated {
		t.Fatalf("expected status escalated, got %v", db.SetStatusCalls)
	}
}

func TestHandleVerifyEvent_HappyPathCompletes(t *testing.T) {
	parent := &linear.Issue{Identifier: "GEO-1", Description: "Build login", TeamID: "team-1", ProjectID: "proj-1"}
	lc := &mockLinearClient{
		GetIssueByID:   map[string]*linear.Issue{"p1": parent},
		SubIssues:      nil,
		WorkflowStates: []linear.WorkflowState{{ID: "st-done", Type: "completed", Position: 0}},
	}
	db := &mockStore{
		TaskVerification:       nil, // first ever verify
		BumpedTaskVerification: &store.TaskVerification{ParentIssueID: "p1", Rounds: 1, Status: store.TaskVerifyActive},
		Repo:                   &store.Repo{RepoDir: "/repo"},
	}
	done := make(chan struct{})
	runner := func(_ context.Context, repoDir, prompt string) (string, error) {
		defer close(done)
		if repoDir != "/repo" {
			t.Errorf("verifyRunner repoDir = %q", repoDir)
		}
		return `{"complete": true, "summary": "ok", "gaps": []}`, nil
	}
	o := newVerifyOrchestrator(db, lc, &mockPRClient{}, 3, runner)

	o.HandleVerifyEvent(context.Background(), "p1")

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("verifyRunner never ran")
	}
	// Poll for the async apply to land.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		db.mu.Lock()
		n := len(db.SetStatusCalls)
		db.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if len(db.BumpCalls) != 1 {
		t.Errorf("expected one bump, got %v", db.BumpCalls)
	}
	if len(db.SetStatusCalls) != 1 || db.SetStatusCalls[0].Status != store.TaskVerifyClosed {
		t.Errorf("expected verification closed after complete verdict, got %v", db.SetStatusCalls)
	}
}

func TestParseVerifyVerdict_CompleteCleanJSON(t *testing.T) {
	raw := `{"complete": true, "summary": "all good", "gaps": []}`
	v, err := parseVerifyVerdict(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !v.Complete || len(v.Gaps) != 0 || v.Summary != "all good" {
		t.Fatalf("unexpected verdict: %+v", v)
	}
}

func TestParseVerifyVerdict_GapsWithProseAndFences(t *testing.T) {
	// Models often wrap the object in prose / ```json fences.
	raw := "Here is my verdict:\n```json\n" +
		`{"complete": false, "summary": "missing logout", "gaps": [` +
		`{"title": "Add logout", "description": "no logout endpoint", "acceptance_criteria": "DELETE /session clears cookie"}]}` +
		"\n```\nHope that helps!"
	v, err := parseVerifyVerdict(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if v.Complete || len(v.Gaps) != 1 {
		t.Fatalf("unexpected verdict: %+v", v)
	}
	if v.Gaps[0].Title != "Add logout" || v.Gaps[0].AcceptanceCriteria == "" {
		t.Fatalf("gap not parsed: %+v", v.Gaps[0])
	}
}

func TestParseVerifyVerdict_NoJSON(t *testing.T) {
	if _, err := parseVerifyVerdict("I could not decide."); err == nil {
		t.Error("expected error when no JSON object present")
	}
}

func TestParseVerifyVerdict_Malformed(t *testing.T) {
	if _, err := parseVerifyVerdict(`{"complete": tru`); err == nil {
		t.Error("expected error on malformed JSON")
	}
}

func TestParseVerifyVerdict_InconsistentCompleteWithGaps(t *testing.T) {
	raw := `{"complete": true, "gaps": [{"title": "x"}]}`
	if _, err := parseVerifyVerdict(raw); err == nil {
		t.Error("expected error: complete=true but gaps present")
	}
}

func TestParseVerifyVerdict_InconsistentIncompleteNoGaps(t *testing.T) {
	raw := `{"complete": false, "gaps": []}`
	if _, err := parseVerifyVerdict(raw); err == nil {
		t.Error("expected error: complete=false but no gaps")
	}
}

func TestParseVerifyVerdict_GapMissingTitle(t *testing.T) {
	raw := `{"complete": false, "gaps": [{"title": "  ", "description": "d"}]}`
	if _, err := parseVerifyVerdict(raw); err == nil {
		t.Error("expected error when a gap has an empty title")
	}
}

func TestBuildVerifyPrompt_IncludesPRDAndDiffs(t *testing.T) {
	p := buildVerifyPrompt(verifyMaterial{
		ParentIdentifier: "GEO-10",
		PRD:              "Build a login feature with email + password.",
		Subs: []verifySubMaterial{
			{Identifier: "GEO-11", Title: "email form", PRURL: "https://gh/pr/1", Diff: "+ <input email>"},
			{Identifier: "GEO-12", Title: "session", Diff: ""},
		},
	})
	for _, want := range []string{
		"Build a login feature", "GEO-11: email form", "https://gh/pr/1",
		"+ <input email>", "GEO-12: session", "(diff unavailable)", `"complete"`,
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

func TestBuildVerifyPrompt_NoSubs(t *testing.T) {
	p := buildVerifyPrompt(verifyMaterial{PRD: "do things"})
	if !strings.Contains(p, "no sub-task diffs available") {
		t.Error("expected a marker when there are no subs")
	}
}

func TestBuildVerifyPrompt_IntermediateSummary(t *testing.T) {
	// A non-leaf sub has no diff but does have a VerifySummary from its
	// own verify. The prompt should surface the summary as evidence of
	// what was shipped, not the "(diff unavailable)" fallback.
	p := buildVerifyPrompt(verifyMaterial{
		ParentIdentifier: "GEO-10",
		PRD:              "Build login feature.",
		Subs: []verifySubMaterial{
			{Identifier: "GEO-11", Title: "email form", PRURL: "https://gh/pr/1", Diff: "+ form"},
			{Identifier: "GEO-12", Title: "session subtree", VerifySummary: "session lifecycle shipped: create + revoke + expiry"},
		},
	})
	for _, want := range []string{
		"GEO-12: session subtree",
		"Internal sub-task (verified, no single PR)",
		"Verified summary:",
		"> session lifecycle shipped: create + revoke + expiry",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("intermediate-sub prompt missing %q\n%s", want, p)
		}
	}
	if strings.Contains(p, "(diff unavailable)") {
		t.Error("intermediate sub with a summary should not show '(diff unavailable)' fallback")
	}
}

func TestBuildVerifyPrompt_DiffOverSummary(t *testing.T) {
	// When BOTH Diff and VerifySummary are set (rare — admiral_task
	// exists for a sub that also has its own task_verifications row),
	// the diff is the stronger evidence and must take priority.
	p := buildVerifyPrompt(verifyMaterial{
		PRD: "do things",
		Subs: []verifySubMaterial{{
			Identifier:    "GEO-11",
			Title:         "leaf with prior verify",
			PRURL:         "https://gh/pr/1",
			Diff:          "+ actual code change",
			VerifySummary: "shipped earlier per cascade",
		}},
	})
	if !strings.Contains(p, "+ actual code change") {
		t.Error("expected diff to win when both Diff and VerifySummary present")
	}
	if strings.Contains(p, "Verified summary:") {
		t.Error("must not render Verified summary block when a real diff is available")
	}
}

func TestBuildVerifyPrompt_DiffUnavailableFallback(t *testing.T) {
	// Default arm: sub has PRURL but no Diff and no VerifySummary (a
	// human-shipped item whose diff couldn't be fetched, or pre-verify
	// state). Must still emit the PR link AND the "(diff unavailable)"
	// marker so the judge knows something existed but the body is empty.
	p := buildVerifyPrompt(verifyMaterial{
		PRD: "do things",
		Subs: []verifySubMaterial{{
			Identifier: "GEO-13", Title: "human-shipped", PRURL: "https://gh/pr/2",
		}},
	})
	if !strings.Contains(p, "PR: https://gh/pr/2") {
		t.Errorf("expected PR url in fallback arm, prompt:\n%s", p)
	}
	if !strings.Contains(p, "(diff unavailable)") {
		t.Errorf("expected '(diff unavailable)' marker in fallback arm, prompt:\n%s", p)
	}
}

// TestGatherVerifyMaterial_IntermediateSubFallsBackToSummary covers the
// happy path: a sub with NO admiral_task triggers GetTaskVerification and
// surfaces the prior round's summary into the upper-layer material.
func TestGatherVerifyMaterial_IntermediateSubFallsBackToSummary(t *testing.T) {
	parent := &linear.Issue{Identifier: "GEO-G", Description: "Feature parent", TeamID: "team-1", ProjectID: "proj-1"}
	lc := &mockLinearClient{
		GetIssueByID: map[string]*linear.Issue{
			"G": parent,
			"P": {Identifier: "GEO-P", Title: "intermediate task"},
		},
		SubIssues: []linear.SubIssue{
			{ID: "P", Identifier: "GEO-P", StateType: "completed"},
		},
	}
	db := &mockStore{
		Repo: &store.Repo{RepoDir: "/repo"},
		// Force GetAdmiralTaskByIssue to return nil for ANY id by leaving
		// AdmiralTask unset — that simulates "no admiral_task for this sub".
		AdmiralTask: nil,
		TaskVerificationByID: map[string]*store.TaskVerification{
			"P": {ParentIssueID: "P", Status: store.TaskVerifyClosed, Summary: "P shipped its sub-tree"},
		},
	}
	pr := &mockPRClient{}
	o := newVerifyOrchestrator(db, lc, pr, 3, nil)

	mat, _, _, _, _, err := o.gatherVerifyMaterial(context.Background(), "G")
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	if len(mat.Subs) != 1 {
		t.Fatalf("expected 1 sub in material, got %d", len(mat.Subs))
	}
	got := mat.Subs[0]
	if got.VerifySummary != "P shipped its sub-tree" {
		t.Errorf("expected intermediate sub to carry verify summary, got %+v", got)
	}
	if got.Diff != "" || got.PRURL != "" {
		t.Errorf("intermediate sub must have empty Diff/PRURL: %+v", got)
	}
}

func TestGatherVerifyMaterial_IntermediateSubSkipsActiveVerification(t *testing.T) {
	// Only a CLOSED verification's summary should be surfaced. An
	// 'active' row means the sub-tree is still mid-loop; its summary may
	// still describe gaps. Don't propagate it as completed evidence.
	parent := &linear.Issue{Identifier: "GEO-G", Description: "Feature parent", TeamID: "team-1", ProjectID: "proj-1"}
	lc := &mockLinearClient{
		GetIssueByID: map[string]*linear.Issue{
			"G": parent,
			"P": {Identifier: "GEO-P", Title: "intermediate task"},
		},
		SubIssues: []linear.SubIssue{
			{ID: "P", Identifier: "GEO-P", StateType: "started"},
		},
	}
	db := &mockStore{
		Repo:        &store.Repo{RepoDir: "/repo"},
		AdmiralTask: nil,
		TaskVerificationByID: map[string]*store.TaskVerification{
			"P": {ParentIssueID: "P", Status: store.TaskVerifyActive, Summary: "partial work"},
		},
	}
	o := newVerifyOrchestrator(db, lc, &mockPRClient{}, 3, nil)

	mat, _, _, _, _, err := o.gatherVerifyMaterial(context.Background(), "G")
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	if got := mat.Subs[0]; got.VerifySummary != "" {
		t.Errorf("active verification must not propagate summary, got %q", got.VerifySummary)
	}
}

// --- build result rendering (verify_cmd integration) ---

// TestBuildVerifyPrompt_NoBuildResult guards the backward-compat path: when
// the repo has no verify_cmd configured, BuildResult is nil and the prompt
// must NOT include the build section. Existing pre-#161 behavior preserved.
func TestBuildVerifyPrompt_NoBuildResult(t *testing.T) {
	p := buildVerifyPrompt(verifyMaterial{
		ParentIdentifier: "GEO-10",
		PRD:              "do things",
		BuildResult:      nil,
	})
	if strings.Contains(p, "Project build / test result") {
		t.Errorf("did not expect a build section when BuildResult is nil; got:\n%s", p)
	}
	if strings.Contains(p, "Hard rule on the build result") {
		t.Error("hard-rule line leaked into the no-build-result prompt")
	}
}

func TestBuildVerifyPrompt_BuildPassed(t *testing.T) {
	p := buildVerifyPrompt(verifyMaterial{
		ParentIdentifier: "GEO-10",
		PRD:              "do things",
		BuildResult: &buildResultMaterial{
			Command:  "swift build",
			ExitCode: 0,
			Output:   "Build complete!\n",
		},
	})
	for _, want := range []string{
		"Project build / test result",
		"Command: `swift build`",
		"Exit code: 0",
		"Build complete!",
		"Hard rule on the build result", // judge must still see the rule
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

func TestBuildVerifyPrompt_BuildFailed(t *testing.T) {
	p := buildVerifyPrompt(verifyMaterial{
		ParentIdentifier: "GEO-10",
		PRD:              "do things",
		BuildResult: &buildResultMaterial{
			Command:  "swift build",
			ExitCode: 1,
			Output:   "error: cannot convert Binding<String?> to Binding<String>\n",
		},
	})
	for _, want := range []string{
		"Exit code: 1",
		"cannot convert Binding<String?>",
		"you MUST set",        // hard instruction
		"Build/test failure",  // judge is named the gap title to use
		"admiral will reject", // defense-in-depth signaled
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

func TestBuildVerifyPrompt_BuildTimedOut(t *testing.T) {
	p := buildVerifyPrompt(verifyMaterial{
		PRD: "do things",
		BuildResult: &buildResultMaterial{
			Command:  "go test ./...",
			TimedOut: true,
			Output:   "...running TestBigCompile",
		},
	})
	if !strings.Contains(p, "TIMED OUT") {
		t.Errorf("prompt missing TIMED OUT marker: %s", p)
	}
}

func TestBuildVerifyPrompt_BuildLaunchError(t *testing.T) {
	p := buildVerifyPrompt(verifyMaterial{
		PRD: "do things",
		BuildResult: &buildResultMaterial{
			Command:   "swift build",
			LaunchErr: "exec: \"sh\": executable file not found in $PATH",
		},
	})
	if !strings.Contains(p, "Launch error:") {
		t.Errorf("prompt missing launch-error marker: %s", p)
	}
}

// --- overrideVerdictOnBuildFail ---

func TestOverrideVerdictOnBuildFail_NoBuildResult(t *testing.T) {
	v := &verifyVerdict{Complete: true, Summary: "looks fine"}
	overrideVerdictOnBuildFail(v, nil)
	if !v.Complete {
		t.Error("nil BuildResult must NOT touch the verdict")
	}
	if len(v.Gaps) != 0 {
		t.Error("nil BuildResult must NOT synthesize a gap")
	}
}

func TestOverrideVerdictOnBuildFail_BuildPassed(t *testing.T) {
	v := &verifyVerdict{Complete: true, Summary: "looks fine"}
	overrideVerdictOnBuildFail(v, &buildResultMaterial{Command: "swift build", ExitCode: 0})
	if !v.Complete {
		t.Error("passing build must NOT flip complete=true")
	}
	if len(v.Gaps) != 0 {
		t.Error("passing build must NOT synthesize a gap")
	}
}

func TestOverrideVerdictOnBuildFail_BuildFailedJudgeSaidComplete(t *testing.T) {
	// Hostile case: build failed (exit != 0) but judge slipped up and
	// returned complete=true with no gaps. admiral must force complete=false
	// AND synthesize a Build/test failure gap so applyVerifyVerdict has
	// something to file (parseVerifyVerdict rejects complete=false + 0 gaps).
	v := &verifyVerdict{Complete: true, Summary: "all good"}
	overrideVerdictOnBuildFail(v, &buildResultMaterial{
		Command:  "swift build",
		ExitCode: 1,
		Output:   "error: type mismatch",
	})
	if v.Complete {
		t.Error("failed build must override complete=true → false")
	}
	if len(v.Gaps) != 1 {
		t.Fatalf("expected 1 synthesized gap, got %d: %#v", len(v.Gaps), v.Gaps)
	}
	g := v.Gaps[0]
	if !strings.Contains(strings.ToLower(g.Title), "build") {
		t.Errorf("synthesized gap title not build-related: %q", g.Title)
	}
	if !strings.Contains(g.Description, "swift build") {
		t.Errorf("gap description must name the command: %q", g.Description)
	}
	if !strings.Contains(g.Description, "error: type mismatch") {
		t.Errorf("gap description must include the output tail: %q", g.Description)
	}
	if !strings.Contains(g.AcceptanceCriteria, "exits 0") {
		t.Errorf("gap AC must require the verify cmd to pass: %q", g.AcceptanceCriteria)
	}
}

func TestOverrideVerdictOnBuildFail_BuildFailedJudgeAlreadyFiledBuildGap(t *testing.T) {
	// Friendly case: judge correctly followed the prompt and filed its own
	// Build/test failure gap. admiral must NOT duplicate it — flip complete
	// (no-op already) and leave the gap list as-is.
	existing := verifyGap{
		Title:              "Build/test failure: Swift type mismatch",
		Description:        "judge-written body",
		AcceptanceCriteria: "swift build passes",
	}
	v := &verifyVerdict{Complete: false, Summary: "broken", Gaps: []verifyGap{existing}}
	overrideVerdictOnBuildFail(v, &buildResultMaterial{Command: "swift build", ExitCode: 1, Output: "boom"})
	if v.Complete {
		t.Error("complete must remain false on failed build")
	}
	if len(v.Gaps) != 1 {
		t.Fatalf("expected exactly 1 gap (no duplicate synthesis), got %d", len(v.Gaps))
	}
	if v.Gaps[0].Title != existing.Title {
		t.Errorf("admiral overwrote judge's gap; want %q got %q", existing.Title, v.Gaps[0].Title)
	}
}

func TestOverrideVerdictOnBuildFail_BuildTimedOut(t *testing.T) {
	v := &verifyVerdict{Complete: true}
	overrideVerdictOnBuildFail(v, &buildResultMaterial{Command: "swift build", TimedOut: true})
	if v.Complete {
		t.Error("timed-out build counts as non-passing → complete must be false")
	}
	if len(v.Gaps) != 1 {
		t.Fatalf("expected synthesized gap on timeout, got %d", len(v.Gaps))
	}
}

func TestOverrideVerdictOnBuildFail_LaunchErr(t *testing.T) {
	v := &verifyVerdict{Complete: true}
	overrideVerdictOnBuildFail(v, &buildResultMaterial{Command: "swift build", LaunchErr: "exec failed"})
	if v.Complete {
		t.Error("launch-err build counts as non-passing → complete must be false")
	}
	if len(v.Gaps) != 1 {
		t.Fatalf("expected synthesized gap on launch-err, got %d", len(v.Gaps))
	}
}

func TestBuildResultMaterial_Passed(t *testing.T) {
	cases := []struct {
		name string
		br   *buildResultMaterial
		want bool
	}{
		{"nil", nil, false},
		{"exit0", &buildResultMaterial{ExitCode: 0}, true},
		{"exit-nonzero", &buildResultMaterial{ExitCode: 1}, false},
		{"timed-out", &buildResultMaterial{TimedOut: true}, false},
		{"launch-err", &buildResultMaterial{LaunchErr: "x"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.br.passed(); got != tc.want {
				t.Errorf("passed() = %v, want %v", got, tc.want)
			}
		})
	}
}
