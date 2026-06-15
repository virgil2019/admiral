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

// newProductVerifyOrchestrator builds an Orchestrator wired for
// product-verify tests: stubbed clients, default logger, and a
// productVerifyRunner the test supplies.
func newProductVerifyOrchestrator(db *mockStore, lc *mockLinearClient, maxRounds int, runner func(ctx context.Context, repoDir, prompt string) (string, error)) *Orchestrator {
	return &Orchestrator{
		db:                  db,
		lc:                  lc,
		logger:              slog.Default(),
		cfg:                 &config.Autopilot{VerifyMaxRounds: maxRounds, MaxRunSeconds: 60},
		productVerifyRunner: runner,
	}
}

func TestBuildProductVerifyPrompt_WithDocPath(t *testing.T) {
	p := buildProductVerifyPrompt(productVerifyMaterial{
		ProjectName:    "Login Product",
		ProductDocPath: "docs/prd.md",
		Features: []productFeature{
			{Identifier: "GEO-1", Title: "email auth", StateName: "Done", Shipped: true},
			{Identifier: "GEO-2", Title: "oauth", StateName: "In Progress", Shipped: false},
		},
	})
	if !strings.Contains(p, "docs/prd.md") {
		t.Errorf("prompt should name the configured doc path:\n%s", p)
	}
	if !strings.Contains(p, "GEO-1") || !strings.Contains(p, "email auth") ||
		!strings.Contains(p, "shipped") || !strings.Contains(p, "NOT shipped") {
		t.Errorf("prompt should list features with shipped status:\n%s", p)
	}
	if !strings.Contains(p, `"complete"`) || !strings.Contains(p, `"gaps"`) {
		t.Errorf("prompt should specify the verdict JSON shape:\n%s", p)
	}
}

func TestBuildProductVerifyPrompt_NoDocPath_DiscoverInstruction(t *testing.T) {
	p := buildProductVerifyPrompt(productVerifyMaterial{ProjectName: "X"})
	if !strings.Contains(p, "Discover") {
		t.Errorf("prompt with no doc path should instruct discovery:\n%s", p)
	}
	if !strings.Contains(p, "no top-level feature issues exist yet") {
		t.Errorf("prompt should note the empty feature list:\n%s", p)
	}
}

func TestHandleProductVerifyEvent_SkipTerminalStatus(t *testing.T) {
	db := &mockStore{ProductVerification: &store.ProductVerification{
		ProjectID: "proj-1", Rounds: 2, Status: store.TaskVerifyClosed,
	}}
	o := newProductVerifyOrchestrator(db, &mockLinearClient{}, 3, nil)

	o.HandleProductVerifyEvent(context.Background(), "proj-1")

	if len(db.ProductBumpCalls) != 0 {
		t.Errorf("expected no bump on a closed product verification, got %v", db.ProductBumpCalls)
	}
}

func TestHandleProductVerifyEvent_RoundCapEscalates(t *testing.T) {
	db := &mockStore{
		ProductVerification:       &store.ProductVerification{ProjectID: "proj-1", Rounds: 3, Status: store.TaskVerifyActive},
		BumpedProductVerification: &store.ProductVerification{ProjectID: "proj-1", Rounds: 4, Status: store.TaskVerifyActive},
	}
	o := newProductVerifyOrchestrator(db, &mockLinearClient{}, 3, func(context.Context, string, string) (string, error) {
		t.Fatal("productVerifyRunner must not run past the round cap")
		return "", nil
	})

	o.HandleProductVerifyEvent(context.Background(), "proj-1")

	if len(db.ProductSetStatusCalls) != 1 || db.ProductSetStatusCalls[0].Status != store.TaskVerifyEscalated {
		t.Fatalf("expected status set to escalated, got %v", db.ProductSetStatusCalls)
	}
}

func TestGatherProductVerifyMaterial(t *testing.T) {
	lc := &mockLinearClient{
		ProjectTeamID: "team-1",
		TopLevelIssues: []linear.Issue{
			{Identifier: "GEO-1", Title: "email auth", StateName: "Done", StateType: "completed"},
			{Identifier: "GEO-2", Title: "oauth", StateName: "Todo", StateType: "unstarted"},
		},
	}
	db := &mockStore{Repo: &store.Repo{RepoDir: "/repo", ProjectName: "Login Product", ProductDocPath: "docs/prd.md"}}
	o := newProductVerifyOrchestrator(db, lc, 3, nil)

	mat, repoDir, teamID, err := o.gatherProductVerifyMaterial(context.Background(), "proj-1")
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	if repoDir != "/repo" || teamID != "team-1" {
		t.Errorf("unexpected repo/team: %q %q", repoDir, teamID)
	}
	if mat.ProjectName != "Login Product" || mat.ProductDocPath != "docs/prd.md" {
		t.Errorf("unexpected project meta: %+v", mat)
	}
	if len(mat.Features) != 2 {
		t.Fatalf("expected 2 features, got %d", len(mat.Features))
	}
	if !mat.Features[0].Shipped {
		t.Errorf("GEO-1 (completed state) should be shipped: %+v", mat.Features[0])
	}
	if mat.Features[1].Shipped {
		t.Errorf("GEO-2 (unstarted state) should not be shipped: %+v", mat.Features[1])
	}
}

func TestApplyProductVerifyVerdict_CompleteClosesProduct(t *testing.T) {
	lc := &mockLinearClient{}
	db := &mockStore{}
	o := newProductVerifyOrchestrator(db, lc, 3, nil)

	o.applyProductVerifyVerdict(context.Background(), "proj-1", "team-1",
		&verifyVerdict{Complete: true, Summary: "fully realised"})

	if len(db.ProductSetStatusCalls) != 1 || db.ProductSetStatusCalls[0].Status != store.TaskVerifyClosed {
		t.Fatalf("expected product verification closed, got %v", db.ProductSetStatusCalls)
	}
	if len(lc.IssueCreateInputs) != 0 {
		t.Errorf("complete verdict must not file gap features, got %v", lc.IssueCreateInputs)
	}
}

func TestApplyProductVerifyVerdict_GapsFileTopLevelIssues(t *testing.T) {
	lc := &mockLinearClient{}
	db := &mockStore{}
	o := newProductVerifyOrchestrator(db, lc, 3, nil)

	o.applyProductVerifyVerdict(context.Background(), "proj-1", "team-1", &verifyVerdict{
		Complete: false,
		Gaps: []verifyGap{
			{Title: "Password reset", Description: "doc requires reset flow", AcceptanceCriteria: "user can reset via email link"},
		},
	})

	if len(lc.IssueCreateInputs) != 1 {
		t.Fatalf("expected one gap feature issue, got %d", len(lc.IssueCreateInputs))
	}
	in := lc.IssueCreateInputs[0]
	if in.TeamID != "team-1" || in.ProjectID != "proj-1" {
		t.Errorf("gap feature not routed correctly: %+v", in)
	}
	// Product-level gaps are top-level (no parent), unlabeled (not auto-shipped),
	// and use the team's default state (no StateID) — the human gate.
	if in.ParentID != "" {
		t.Errorf("gap feature must be top-level (no parent), got ParentID=%q", in.ParentID)
	}
	if len(in.LabelIDs) != 0 {
		t.Errorf("gap feature must be unlabeled (not auto-picked), got %v", in.LabelIDs)
	}
	if in.StateID != "" {
		t.Errorf("gap feature must use default state, got StateID=%q", in.StateID)
	}
	if !strings.Contains(in.Description, "user can reset via email link") {
		t.Errorf("acceptance criteria not in body: %q", in.Description)
	}
	// Gaps leave the verification active (loop continues) — no status write.
	if len(db.ProductSetStatusCalls) != 0 {
		t.Errorf("gaps must not set a terminal status, got %v", db.ProductSetStatusCalls)
	}
}

func TestHandleProductVerifyEvent_HappyPathCompletes(t *testing.T) {
	lc := &mockLinearClient{
		ProjectTeamID:  "team-1",
		TopLevelIssues: nil,
	}
	db := &mockStore{
		ProductVerification:       nil, // first ever product verify
		BumpedProductVerification: &store.ProductVerification{ProjectID: "proj-1", Rounds: 1, Status: store.TaskVerifyActive},
		Repo:                      &store.Repo{RepoDir: "/repo", ProjectName: "P"},
	}
	done := make(chan struct{})
	runner := func(_ context.Context, repoDir, _ string) (string, error) {
		defer close(done)
		if repoDir != "/repo" {
			t.Errorf("productVerifyRunner repoDir = %q", repoDir)
		}
		return `{"complete": true, "summary": "ok", "gaps": []}`, nil
	}
	o := newProductVerifyOrchestrator(db, lc, 3, runner)

	o.HandleProductVerifyEvent(context.Background(), "proj-1")

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("productVerifyRunner never ran")
	}
	// Poll for the async apply to land.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		db.mu.Lock()
		n := len(db.ProductSetStatusCalls)
		db.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if len(db.ProductBumpCalls) != 1 {
		t.Errorf("expected one bump, got %v", db.ProductBumpCalls)
	}
	if len(db.ProductSetStatusCalls) != 1 || db.ProductSetStatusCalls[0].Status != store.TaskVerifyClosed {
		t.Errorf("expected verification closed after complete verdict, got %v", db.ProductSetStatusCalls)
	}
}
