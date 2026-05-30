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

	mat, repoDir, teamID, projectID, err := o.gatherVerifyMaterial(context.Background(), "p1")
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	if repoDir != "/repo" || teamID != "team-1" || projectID != "proj-1" {
		t.Errorf("unexpected repo/team/project: %q %q %q", repoDir, teamID, projectID)
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

	o.applyVerifyVerdict(context.Background(), "p1", "team-1", "proj-1",
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

	o.applyVerifyVerdict(context.Background(), "p1", "team-1", "proj-1", &verifyVerdict{
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

func TestApplyVerifyVerdict_UnpickableGatesEscalate(t *testing.T) {
	// Label gate configured but unresolvable (GetTeamLabelID returns "") →
	// filing orphan follow-ups would stall the loop, so escalate instead.
	lc := &mockLinearClient{TeamLabelID: ""}
	db := &mockStore{}
	o := newVerifyOrchestrator(db, lc, &mockPRClient{}, 3, nil)
	o.verifyLabel = "agent-ready"
	o.verifyStateTypes = nil // state gate unconfigured = valid, skipped

	o.applyVerifyVerdict(context.Background(), "p1", "team-1", "proj-1", &verifyVerdict{
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
