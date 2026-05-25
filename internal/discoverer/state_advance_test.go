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
