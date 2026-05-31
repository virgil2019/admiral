package discoverer

import (
	"context"
	"testing"

	"github.com/georgehuang/admiral/internal/linear"
	"github.com/georgehuang/admiral/internal/store"
)

// seedTeamGEO populates a fake linear client with a single team's
// workflow states resembling Linear's defaults plus the customary
// "In Review" extra started state.
func seedTeamGEO(lc *fakeLinear) {
	lc.workflowStates = map[string][]linear.WorkflowState{
		"team-GEO": {
			{ID: "s-backlog", Name: "Backlog", Type: "backlog", Position: 0},
			{ID: "s-todo", Name: "Todo", Type: "unstarted", Position: 1},
			{ID: "s-prog", Name: "In Progress", Type: "started", Position: 2},
			{ID: "s-review", Name: "In Review", Type: "started", Position: 3},
			{ID: "s-reviewed", Name: "Reviewed", Type: "started", Position: 4},
			{ID: "s-done", Name: "Done", Type: "completed", Position: 5},
			{ID: "s-cancel", Name: "Cancelled", Type: "canceled", Position: 6},
		},
	}
}

func newPRURL() string { return "https://github.com/owner/repo/pull/77" }

func newGEOTaskDone(prURL string) store.AdmiralTask {
	return store.AdmiralTask{
		IssueID:         "iss-A",
		IssueIdentifier: "GEO-77",
		PRURL:           prURL,
		State:           store.JobStateDone,
		Branch:          "linear/geo-77",
	}
}

func newGEOIssue(stateName string) *linear.Issue {
	return &linear.Issue{
		ID:         "iss-A",
		Identifier: "GEO-77",
		Title:      "x",
		TeamID:     "team-GEO",
		ProjectID:  "proj-A", // matches fakeStore.projectIDs default
		StateName:  stateName,
	}
}

func TestAdvanceMergedWritesDoneAndUpdatesTask(t *testing.T) {
	prURL := newPRURL()
	task := newGEOTaskDone(prURL)
	ts := newFakeStore()
	ts.tasksByState[task.IssueID] = &task

	lc := &fakeLinear{}
	lc.issues = map[string]*linear.Issue{"iss-A": newGEOIssue("In Progress")}
	seedTeamGEO(lc)

	pr := &fakePR{statuses: map[string]PRStatus{
		prURL: {State: "MERGED", MergedAt: "2026-05-25T06:00:00Z"},
	}}

	svc := newSvcWithPR(Config{AdmiralUserID: "u-1"}, lc, pr, ts, nil)
	svc.advanceLinearStates(context.Background())

	if len(lc.issueUpdates) != 1 || lc.issueUpdates[0].StateID != "s-done" {
		t.Fatalf("expected one IssueUpdate to s-done, got %+v", lc.issueUpdates)
	}
	if ts.tasksByState["iss-A"].State != store.JobStateDoneMerged {
		t.Errorf("task state: got %q, want %q", ts.tasksByState["iss-A"].State, store.JobStateDoneMerged)
	}
}

func TestAdvanceClosedUnmergedWritesCancelled(t *testing.T) {
	prURL := newPRURL()
	task := newGEOTaskDone(prURL)
	ts := newFakeStore()
	ts.tasksByState[task.IssueID] = &task

	lc := &fakeLinear{}
	lc.issues = map[string]*linear.Issue{"iss-A": newGEOIssue("In Progress")}
	seedTeamGEO(lc)

	pr := &fakePR{statuses: map[string]PRStatus{
		prURL: {State: "CLOSED"},
	}}

	svc := newSvcWithPR(Config{AdmiralUserID: "u-1"}, lc, pr, ts, nil)
	svc.advanceLinearStates(context.Background())

	if len(lc.issueUpdates) != 1 || lc.issueUpdates[0].StateID != "s-cancel" {
		t.Fatalf("expected one IssueUpdate to s-cancel, got %+v", lc.issueUpdates)
	}
	if ts.tasksByState["iss-A"].State != store.JobStateCancelled {
		t.Errorf("task state: got %q, want CANCELLED", ts.tasksByState["iss-A"].State)
	}
}

func TestAdvanceOpenWithApprovalWritesReviewed(t *testing.T) {
	prURL := newPRURL()
	task := newGEOTaskDone(prURL)
	ts := newFakeStore()
	ts.tasksByState[task.IssueID] = &task

	lc := &fakeLinear{}
	lc.issues = map[string]*linear.Issue{"iss-A": newGEOIssue("In Progress")}
	seedTeamGEO(lc)

	pr := &fakePR{statuses: map[string]PRStatus{
		prURL: {State: "OPEN", HasApprovedReview: true},
	}}

	cfg := Config{
		AdmiralUserID: "u-1",
		LinearStates: LinearStateMap{
			InReview: "In Review",
			Reviewed: "Reviewed",
		},
	}
	svc := newSvcWithPR(cfg, lc, pr, ts, nil)
	svc.advanceLinearStates(context.Background())

	if len(lc.issueUpdates) != 1 || lc.issueUpdates[0].StateID != "s-reviewed" {
		t.Fatalf("expected IssueUpdate to s-reviewed, got %+v", lc.issueUpdates)
	}
	// admiral_tasks state should stay DONE — discoverer only flips it on
	// merge/cancel terminal events.
	if ts.tasksByState["iss-A"].State != store.JobStateDone {
		t.Errorf("task state: got %q, want DONE (unchanged)", ts.tasksByState["iss-A"].State)
	}
}

func TestAdvanceOpenWithoutApprovalWritesInReview(t *testing.T) {
	prURL := newPRURL()
	task := newGEOTaskDone(prURL)
	ts := newFakeStore()
	ts.tasksByState[task.IssueID] = &task

	lc := &fakeLinear{}
	lc.issues = map[string]*linear.Issue{"iss-A": newGEOIssue("In Progress")}
	seedTeamGEO(lc)

	pr := &fakePR{statuses: map[string]PRStatus{
		prURL: {State: "OPEN", HasApprovedReview: false},
	}}

	cfg := Config{
		AdmiralUserID: "u-1",
		LinearStates:  LinearStateMap{InReview: "In Review"},
	}
	svc := newSvcWithPR(cfg, lc, pr, ts, nil)
	svc.advanceLinearStates(context.Background())

	if len(lc.issueUpdates) != 1 || lc.issueUpdates[0].StateID != "s-review" {
		t.Fatalf("expected IssueUpdate to s-review, got %+v", lc.issueUpdates)
	}
}

func TestAdvanceOpenWithoutMappingSkipsLinearWrite(t *testing.T) {
	prURL := newPRURL()
	task := newGEOTaskDone(prURL)
	ts := newFakeStore()
	ts.tasksByState[task.IssueID] = &task

	lc := &fakeLinear{}
	lc.issues = map[string]*linear.Issue{"iss-A": newGEOIssue("In Progress")}
	seedTeamGEO(lc)

	pr := &fakePR{statuses: map[string]PRStatus{
		prURL: {State: "OPEN", HasApprovedReview: true},
	}}

	// LinearStates not set — discoverer should skip the Linear write
	// for OPEN PRs (merge / cancel still happen via type-fallback).
	svc := newSvcWithPR(Config{AdmiralUserID: "u-1"}, lc, pr, ts, nil)
	svc.advanceLinearStates(context.Background())

	if len(lc.issueUpdates) != 0 {
		t.Errorf("expected no Linear write when mapping empty, got %+v", lc.issueUpdates)
	}
}

func TestAdvanceSkipsIfAlreadyInTargetState(t *testing.T) {
	prURL := newPRURL()
	task := newGEOTaskDone(prURL)
	ts := newFakeStore()
	ts.tasksByState[task.IssueID] = &task

	lc := &fakeLinear{}
	lc.issues = map[string]*linear.Issue{"iss-A": newGEOIssue("Done")} // already Done
	seedTeamGEO(lc)

	pr := &fakePR{statuses: map[string]PRStatus{
		prURL: {State: "MERGED", MergedAt: "2026-05-25T06:00:00Z"},
	}}

	svc := newSvcWithPR(Config{AdmiralUserID: "u-1"}, lc, pr, ts, nil)
	svc.advanceLinearStates(context.Background())

	if len(lc.issueUpdates) != 0 {
		t.Errorf("expected no Linear write when already in target type, got %+v", lc.issueUpdates)
	}
	// task still gets bumped to DONE_MERGED — Linear push was a no-op
	// but the admiral-side terminal transition still happens.
	if ts.tasksByState["iss-A"].State != store.JobStateDoneMerged {
		t.Errorf("task state: got %q, want %q", ts.tasksByState["iss-A"].State, store.JobStateDoneMerged)
	}
}

func TestAdvanceSkipsWhenNoPR(t *testing.T) {
	task := store.AdmiralTask{IssueID: "iss-B", State: store.JobStateDone}
	ts := newFakeStore()
	ts.tasksByState[task.IssueID] = &task

	lc := &fakeLinear{}
	pr := &fakePR{}
	svc := newSvcWithPR(Config{AdmiralUserID: "u-1"}, lc, pr, ts, nil)
	svc.advanceLinearStates(context.Background())

	if len(lc.issueUpdates) != 0 {
		t.Errorf("expected no API calls for task without PR, got %+v", lc.issueUpdates)
	}
}

func TestAdvanceSkipsWhenProjectNotEnabled(t *testing.T) {
	// Regression: discoverer must not touch Linear state for tasks
	// whose issue lives in a project that's no longer opted in via
	// admin UI (repos.auto_pick_enabled). Otherwise historical
	// admiral_tasks rows from disabled projects keep getting state
	// pushes every tick.
	prURL := newPRURL()
	task := newGEOTaskDone(prURL)
	ts := newFakeStore()
	ts.tasksByState[task.IssueID] = &task

	lc := &fakeLinear{}
	iss := newGEOIssue("In Progress")
	iss.ProjectID = "proj-X" // NOT in fakeStore.projectIDs ([]string{"proj-A"})
	lc.issues = map[string]*linear.Issue{"iss-A": iss}
	seedTeamGEO(lc)

	pr := &fakePR{statuses: map[string]PRStatus{
		prURL: {State: "MERGED", MergedAt: "2026-05-25T06:00:00Z"},
	}}

	svc := newSvcWithPR(Config{AdmiralUserID: "u-1"}, lc, pr, ts, nil)
	svc.advanceLinearStates(context.Background())

	if len(lc.issueUpdates) != 0 {
		t.Errorf("expected no Linear write for disabled-project task, got %+v", lc.issueUpdates)
	}
	if ts.tasksByState["iss-A"].State != store.JobStateDone {
		t.Errorf("admiral_tasks state must stay DONE (no transition), got %q",
			ts.tasksByState["iss-A"].State)
	}
}

func TestAdvanceSkipsWhenIssueHasNoProject(t *testing.T) {
	// Edge case: Linear allows project-less issues. The skip path must
	// fire its own log key (state_advance_skip_issue_has_no_project)
	// distinct from the disabled-project path, so on-call doesn't
	// misread the enabled-set lookup as broken.
	prURL := newPRURL()
	task := newGEOTaskDone(prURL)
	ts := newFakeStore()
	ts.tasksByState[task.IssueID] = &task

	lc := &fakeLinear{}
	iss := newGEOIssue("In Progress")
	iss.ProjectID = "" // project-less issue
	lc.issues = map[string]*linear.Issue{"iss-A": iss}
	seedTeamGEO(lc)

	pr := &fakePR{statuses: map[string]PRStatus{
		prURL: {State: "MERGED"},
	}}

	svc := newSvcWithPR(Config{AdmiralUserID: "u-1"}, lc, pr, ts, nil)
	svc.advanceLinearStates(context.Background())

	if len(lc.issueUpdates) != 0 {
		t.Errorf("expected no Linear write for project-less issue, got %+v", lc.issueUpdates)
	}
	if ts.tasksByState["iss-A"].State != store.JobStateDone {
		t.Errorf("admiral_tasks state must stay DONE, got %q", ts.tasksByState["iss-A"].State)
	}
}

func TestAdvanceSkipsWhenNoEnabledProjects(t *testing.T) {
	// All projects disabled → discoverer must short-circuit before
	// touching admiral_tasks at all (no PR lookup, no GetIssue).
	prURL := newPRURL()
	task := newGEOTaskDone(prURL)
	ts := newFakeStore()
	ts.projectIDs = nil // empty enabled set
	ts.tasksByState[task.IssueID] = &task

	lc := &fakeLinear{}
	lc.issues = map[string]*linear.Issue{"iss-A": newGEOIssue("In Progress")}
	seedTeamGEO(lc)

	pr := &fakePR{statuses: map[string]PRStatus{
		prURL: {State: "MERGED"},
	}}

	svc := newSvcWithPR(Config{AdmiralUserID: "u-1"}, lc, pr, ts, nil)
	svc.advanceLinearStates(context.Background())

	if len(lc.issueUpdates) != 0 {
		t.Errorf("expected no Linear write when no enabled projects, got %+v", lc.issueUpdates)
	}
}

// mergedVerifyFixture builds the common setup for the verify-trigger tests:
// a merged GEO-77 sub-issue (iss-A) under parent "parent-1".
func mergedVerifyFixture() (*fakeLinear, *fakeStore, *fakePR) {
	prURL := newPRURL()
	task := newGEOTaskDone(prURL)
	ts := newFakeStore()
	ts.tasksByState[task.IssueID] = &task

	lc := &fakeLinear{}
	lc.issues = map[string]*linear.Issue{"iss-A": newGEOIssue("In Progress")}
	lc.parents = map[string]string{"iss-A": "parent-1"}
	seedTeamGEO(lc)

	pr := &fakePR{statuses: map[string]PRStatus{
		prURL: {State: "MERGED", MergedAt: "2026-05-25T06:00:00Z"},
	}}
	return lc, ts, pr
}

func TestAdvanceMergedEnqueuesVerifyWhenAllSubsComplete(t *testing.T) {
	lc, ts, pr := mergedVerifyFixture()
	lc.subIssues = map[string][]linear.SubIssue{
		"parent-1": {
			{ID: "iss-A", Identifier: "GEO-77", StateType: "completed"},
			{ID: "sub-2", Identifier: "GEO-78", StateType: "completed"},
		},
	}

	svc := newSvcWithPR(Config{AdmiralUserID: "u-1"}, lc, pr, ts, nil)
	svc.advanceLinearStates(context.Background())

	if len(ts.enqueued) != 1 {
		t.Fatalf("expected one verify enqueue, got %+v", ts.enqueued)
	}
	e := ts.enqueued[0]
	if e.Source != "verify" || e.SessionID != "parent-1" || e.IssueID != "parent-1" {
		t.Errorf("verify event misrouted: %+v", e)
	}
	if e.WebhookID != "verify-parent-1-iss-A" {
		t.Errorf("webhook_id = %q, want verify-parent-1-iss-A", e.WebhookID)
	}
}

func TestAdvanceMergedSkipsVerifyWhenSubIncomplete(t *testing.T) {
	lc, ts, pr := mergedVerifyFixture()
	lc.subIssues = map[string][]linear.SubIssue{
		"parent-1": {
			{ID: "iss-A", StateType: "completed"},
			{ID: "sub-2", StateType: "started"}, // sibling still in flight
		},
	}

	svc := newSvcWithPR(Config{AdmiralUserID: "u-1"}, lc, pr, ts, nil)
	svc.advanceLinearStates(context.Background())

	if len(ts.enqueued) != 0 {
		t.Errorf("expected no verify enqueue while a sibling is incomplete, got %+v", ts.enqueued)
	}
	// The merge transition itself must still happen.
	if ts.tasksByState["iss-A"].State != store.JobStateDoneMerged {
		t.Errorf("task state: got %q, want DONE_MERGED", ts.tasksByState["iss-A"].State)
	}
}

func TestAdvanceMergedSkipsVerifyWhenNoParent(t *testing.T) {
	lc, ts, pr := mergedVerifyFixture()
	lc.parents = nil // top-level issue, not a decomposed sub-task

	svc := newSvcWithPR(Config{AdmiralUserID: "u-1"}, lc, pr, ts, nil)
	svc.advanceLinearStates(context.Background())

	if len(ts.enqueued) != 0 {
		t.Errorf("expected no verify enqueue for a parentless issue, got %+v", ts.enqueued)
	}
}

func TestAdvanceSkipsWhenPRClientNil(t *testing.T) {
	prURL := newPRURL()
	task := newGEOTaskDone(prURL)
	ts := newFakeStore()
	ts.tasksByState[task.IssueID] = &task

	lc := &fakeLinear{}
	seedTeamGEO(lc)

	svc := newSvc(Config{AdmiralUserID: "u-1"}, lc, ts, nil)
	// pr is nil — must short-circuit without panic
	svc.advanceLinearStates(context.Background())

	if len(lc.issueUpdates) != 0 {
		t.Errorf("expected no API calls when pr client nil, got %+v", lc.issueUpdates)
	}
}
