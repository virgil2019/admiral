package autopilot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/georgehuang/admiral/internal/linear"
	"github.com/georgehuang/admiral/internal/store"
)

// fakeResetLinear is a hand-rolled resetLinear for the reset-task tests. It
// records mutating calls so the guard / ordering invariants can be asserted.
type fakeResetLinear struct {
	parent       *linear.Issue
	subs         []linear.SubIssue
	states       []linear.WorkflowState
	labelID      string
	stateUpdates map[string]string // issueID -> stateID
	labelRemoved map[string]string // issueID -> labelID
	unassigned   map[string]bool   // issueID -> unassigned
}

func (f *fakeResetLinear) GetIssue(ctx context.Context, id string) (*linear.Issue, error) {
	if f.parent == nil {
		// Mirror the real client, which returns a wrapped ErrIssueNotFound
		// (never (nil, nil)) when Linear has no such issue.
		return nil, fmt.Errorf("%w: %s", linear.ErrIssueNotFound, id)
	}
	return f.parent, nil
}
func (f *fakeResetLinear) GetSubIssues(ctx context.Context, parentID string) ([]linear.SubIssue, error) {
	return f.subs, nil
}
func (f *fakeResetLinear) GetWorkflowStates(ctx context.Context, teamID string) ([]linear.WorkflowState, error) {
	return f.states, nil
}
func (f *fakeResetLinear) GetTeamLabelID(ctx context.Context, teamID, name string) (string, error) {
	return f.labelID, nil
}
func (f *fakeResetLinear) IssueUpdate(ctx context.Context, issueID, stateID string) error {
	if f.stateUpdates == nil {
		f.stateUpdates = map[string]string{}
	}
	f.stateUpdates[issueID] = stateID
	return nil
}
func (f *fakeResetLinear) UnassignIssue(ctx context.Context, issueID string) error {
	if f.unassigned == nil {
		f.unassigned = map[string]bool{}
	}
	f.unassigned[issueID] = true
	return nil
}
func (f *fakeResetLinear) RemoveIssueLabel(ctx context.Context, issueID, labelID string) error {
	if f.labelRemoved == nil {
		f.labelRemoved = map[string]string{}
	}
	f.labelRemoved[issueID] = labelID
	return nil
}

func newResetTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// seedSubWithPR claims an admiral_tasks row for issueID and sets its PR URL.
func seedSubWithPR(t *testing.T, s *store.Store, issueID, identifier, prURL string) {
	t.Helper()
	if _, err := s.ClaimAdmiralTask(issueID, identifier, "sess"); err != nil {
		t.Fatalf("claim %s: %v", identifier, err)
	}
	if err := s.UpdateAdmiralTask(issueID, func(tk *store.AdmiralTask) { tk.PRURL = prURL }); err != nil {
		t.Fatalf("set pr for %s: %v", identifier, err)
	}
}

func baseFakeLinear() *fakeResetLinear {
	return &fakeResetLinear{
		parent: &linear.Issue{ID: "parent-id", Identifier: "GEO-100", TeamID: "team-1"},
		subs: []linear.SubIssue{
			{ID: "sub-1", Identifier: "GEO-101"},
			{ID: "sub-2", Identifier: "GEO-102"},
		},
		states:  []linear.WorkflowState{{ID: "state-backlog", Name: "Backlog", Type: "backlog"}},
		labelID: "label-ready",
	}
}

func TestRunResetTask_MergedGuardAbortsUntouched(t *testing.T) {
	s := newResetTestStore(t)
	lc := baseFakeLinear()
	seedSubWithPR(t, s, "sub-1", "GEO-101", "https://github.com/o/r/pull/1")
	if _, err := s.BumpTaskVerificationRound("parent-id"); err != nil {
		t.Fatalf("bump verify: %v", err)
	}

	// sub-1's PR reports MERGED.
	gh := func(ctx context.Context, args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "pr" && args[1] == "view" {
			return `{"state":"MERGED"}`, nil
		}
		t.Fatalf("unexpected gh call during guard abort: %v", args)
		return "", nil
	}

	_, err := runResetTask(context.Background(), lc, gh, s, "agent-ready", slog.Default(), "GEO-100")
	if !errors.Is(err, errResetMergedPR) {
		t.Fatalf("expected errResetMergedPR, got %v", err)
	}
	// Nothing touched: no Linear state changes, DB row + verification survive.
	if len(lc.stateUpdates) != 0 {
		t.Fatalf("guard should not mutate Linear state, got %v", lc.stateUpdates)
	}
	if len(lc.unassigned) != 0 {
		t.Fatalf("guard should not unassign, got %v", lc.unassigned)
	}
	if task, _ := s.GetAdmiralTaskByIssue("sub-1"); task == nil {
		t.Fatal("guard should not delete admiral_tasks row")
	}
	if tv, _ := s.GetTaskVerification("parent-id"); tv == nil {
		t.Fatal("guard should not delete task_verifications row")
	}
}

func TestRunResetTask_HappyPath(t *testing.T) {
	s := newResetTestStore(t)
	lc := baseFakeLinear()
	// sub-1 has an open PR; sub-2 was never worked (no admiral_tasks row).
	seedSubWithPR(t, s, "sub-1", "GEO-101", "https://github.com/o/r/pull/1")
	if err := s.UpsertDiscovererPick(store.DiscovererPick{IssueID: "sub-1", IssueIdentifier: "GEO-101", PickedState: "Todo"}); err != nil {
		t.Fatalf("pick: %v", err)
	}
	if _, err := s.BumpTaskVerificationRound("parent-id"); err != nil {
		t.Fatalf("bump verify: %v", err)
	}

	var closed []string
	gh := func(ctx context.Context, args ...string) (string, error) {
		switch {
		case len(args) >= 2 && args[0] == "pr" && args[1] == "view":
			return `{"state":"OPEN"}`, nil
		case len(args) >= 2 && args[0] == "pr" && args[1] == "close":
			closed = append(closed, args[2])
			return "", nil
		}
		t.Fatalf("unexpected gh call: %v", args)
		return "", nil
	}

	resp, err := runResetTask(context.Background(), lc, gh, s, "agent-ready", slog.Default(), "GEO-100")
	if err != nil {
		t.Fatalf("reset: %v", err)
	}

	if resp.SubsReset != 2 {
		t.Fatalf("expected 2 subs reset, got %d", resp.SubsReset)
	}
	// Both subs → backlog, label removed.
	for _, id := range []string{"sub-1", "sub-2"} {
		if lc.stateUpdates[id] != "state-backlog" {
			t.Errorf("sub %s not set to backlog: %v", id, lc.stateUpdates[id])
		}
		if lc.labelRemoved[id] != "label-ready" {
			t.Errorf("sub %s label not removed: %v", id, lc.labelRemoved[id])
		}
		if !lc.unassigned[id] {
			t.Errorf("sub %s not unassigned", id)
		}
	}
	// Only sub-1 had a PR to close.
	if len(closed) != 1 || closed[0] != "https://github.com/o/r/pull/1" {
		t.Errorf("expected sub-1 PR closed, got %v", closed)
	}
	// DB fully reset.
	if task, _ := s.GetAdmiralTaskByIssue("sub-1"); task != nil {
		t.Error("sub-1 admiral_tasks not cleared")
	}
	if pick, _ := s.GetDiscovererPick("sub-1"); pick != nil {
		t.Error("sub-1 discoverer_pick not cleared")
	}
	if tv, _ := s.GetTaskVerification("parent-id"); tv != nil {
		t.Error("parent task_verifications not cleared")
	}
}

func TestRunResetTask_ParentNotFound(t *testing.T) {
	s := newResetTestStore(t)
	lc := baseFakeLinear()
	lc.parent = nil // GetIssue returns (nil, nil)
	gh := func(ctx context.Context, args ...string) (string, error) { return "", nil }

	_, err := runResetTask(context.Background(), lc, gh, s, "agent-ready", slog.Default(), "GEO-404")
	if !errors.Is(err, errResetParentNotFound) {
		t.Fatalf("expected errResetParentNotFound, got %v", err)
	}
}

func TestResetTaskHandler_StatusMapping(t *testing.T) {
	s := newResetTestStore(t)
	as := newAdminServer(s, nil, "gh", slog.Default(), 3, "", "agent-ready")

	// lc == nil → 503.
	req := httptest.NewRequest("POST", "/admin/reset-task", strings.NewReader(`{"parent":"GEO-1"}`))
	w := httptest.NewRecorder()
	as.resetTaskHandler(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil linear: expected 503, got %d", w.Code)
	}

	// Bad JSON → 400 (give it a linear client so it gets past the nil guard).
	as.lc = &linear.Client{}
	req = httptest.NewRequest("POST", "/admin/reset-task", strings.NewReader(`not json`))
	w = httptest.NewRecorder()
	as.resetTaskHandler(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad json: expected 400, got %d", w.Code)
	}

	// Missing parent → 400.
	req = httptest.NewRequest("POST", "/admin/reset-task", strings.NewReader(`{"parent":""}`))
	w = httptest.NewRecorder()
	as.resetTaskHandler(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("empty parent: expected 400, got %d", w.Code)
	}

	// Wrong method → 405.
	req = httptest.NewRequest("GET", "/admin/reset-task", nil)
	w = httptest.NewRecorder()
	as.resetTaskHandler(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: expected 405, got %d", w.Code)
	}
}
