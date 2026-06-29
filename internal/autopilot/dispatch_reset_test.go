package autopilot

import (
	"strings"
	"testing"

	"github.com/georgehuang/admiral/internal/linear"
	"github.com/georgehuang/admiral/internal/store"
)

func resetTestMocks() (*mockStore, *mockLinearClient) {
	ms := &mockStore{} // AdmiralTask nil → subs have no PR (gh never invoked)
	mlc := &mockLinearClient{
		SubIssues: []linear.SubIssue{
			{ID: "sub-1", Identifier: "GEO-101"},
			{ID: "sub-2", Identifier: "GEO-102"},
		},
		WorkflowStates: []linear.WorkflowState{{ID: "st-backlog", Name: "Backlog", Type: "backlog"}},
		TeamLabelID:    "lbl-ready",
	}
	return ms, mlc
}

func resetEvent() linear.AgentEvent {
	return linear.AgentEvent{
		Action:          linear.ActionPrompted,
		SessionID:       "sess-1",
		IssueID:         "issue-test",
		IssueIdentifier: "TEST-1",
		UserMessage:     "/reset",
	}
}

func TestDispatchReset_PreviewDoesNotMutate(t *testing.T) {
	ms, mlc := resetTestMocks()
	o := newTestOrchestrator(t, ms, mlc, nil)

	o.dispatchReset(resetEvent(), "")

	if mlc.PostedActivity.Type != linear.ActivityResponse {
		t.Fatalf("preview should post a Response, got %q", mlc.PostedActivity.Type)
	}
	body := mlc.GetPostedBody()
	for _, want := range []string{"GEO-101", "GEO-102", "/reset confirm"} {
		if !strings.Contains(body, want) {
			t.Errorf("preview body missing %q:\n%s", want, body)
		}
	}
	// Nothing mutated.
	if len(mlc.IssueUpdateCalls) != 0 || len(mlc.RemovedLabels) != 0 || len(ms.ResetIssueRowsCalls) != 0 {
		t.Fatalf("preview mutated state: updates=%v labels=%v resets=%v",
			mlc.IssueUpdateCalls, mlc.RemovedLabels, ms.ResetIssueRowsCalls)
	}
}

func TestDispatchReset_PreviewWarnsInFlight(t *testing.T) {
	ms, mlc := resetTestMocks()
	// GetAdmiralTaskByIssue returns this for every sub → both shown as running.
	ms.AdmiralTask = &store.AdmiralTask{State: store.JobStateExecuting}
	o := newTestOrchestrator(t, ms, mlc, nil)

	o.dispatchReset(resetEvent(), "")

	body := mlc.GetPostedBody()
	if !strings.Contains(body, "currently running") {
		t.Errorf("preview should flag in-flight subs:\n%s", body)
	}
	if !strings.Contains(body, "still running") {
		t.Errorf("preview should warn about tearing down work in progress:\n%s", body)
	}
	// Still a preview — no mutation.
	if len(ms.ResetIssueRowsCalls) != 0 {
		t.Fatalf("preview mutated DB: %v", ms.ResetIssueRowsCalls)
	}
}

func TestDispatchReset_ConfirmExecutes(t *testing.T) {
	ms, mlc := resetTestMocks()
	o := newTestOrchestrator(t, ms, mlc, nil)
	o.verifyLabel = "agent-ready" // require_label to drop from subs

	o.dispatchReset(resetEvent(), "confirm")

	// Both subs → backlog state.
	if len(mlc.IssueUpdateCalls) != 2 {
		t.Fatalf("expected 2 state updates, got %v", mlc.IssueUpdateCalls)
	}
	for _, u := range mlc.IssueUpdateCalls {
		if u.StateID != "st-backlog" {
			t.Errorf("sub %s set to %q, want st-backlog", u.IssueID, u.StateID)
		}
	}
	// Both subs → label dropped.
	if len(mlc.RemovedLabels) != 2 {
		t.Errorf("expected 2 label removals, got %v", mlc.RemovedLabels)
	}
	// DB reset for both subs + the parent; parent verification deleted.
	for _, want := range []string{"sub-1", "sub-2", "issue-test"} {
		if !contains(ms.ResetIssueRowsCalls, want) {
			t.Errorf("ResetIssueRows missing %q: %v", want, ms.ResetIssueRowsCalls)
		}
	}
	if !contains(ms.DeletedTaskVerifications, "issue-test") {
		t.Errorf("parent task verification not deleted: %v", ms.DeletedTaskVerifications)
	}
	if mlc.PostedActivity.Type != linear.ActivityResponse {
		t.Fatalf("confirm should post a Response, got %q", mlc.PostedActivity.Type)
	}
	if !strings.Contains(mlc.GetPostedBody(), "Reset task TEST-1") {
		t.Errorf("confirm body missing success line:\n%s", mlc.GetPostedBody())
	}
}

func TestDispatchReset_NoSubsRejects(t *testing.T) {
	ms, mlc := resetTestMocks()
	mlc.SubIssues = nil
	o := newTestOrchestrator(t, ms, mlc, nil)

	o.dispatchReset(resetEvent(), "")

	if mlc.PostedActivity.Type != linear.ActivityError {
		t.Fatalf("expected an Error activity, got %q", mlc.PostedActivity.Type)
	}
	if !strings.Contains(mlc.GetPostedBody(), "no sub-issues") {
		t.Errorf("expected 'no sub-issues' message, got:\n%s", mlc.GetPostedBody())
	}
	if len(ms.ResetIssueRowsCalls) != 0 {
		t.Fatalf("no-subs reset should not mutate DB: %v", ms.ResetIssueRowsCalls)
	}
}

// TestDispatch_ResetInterceptsRowlessParent proves /reset routes to the reset
// flow even when the mentioned (parent) issue has NO admiral_tasks row — i.e.
// the interception sits ahead of dispatch's task==nil rejection.
func TestDispatch_ResetInterceptsRowlessParent(t *testing.T) {
	ms, mlc := resetTestMocks()
	ms.AdmiralTask = nil // parent has no task row
	o := newTestOrchestrator(t, ms, mlc, nil)

	o.dispatch(resetEvent())

	body := mlc.GetPostedBody()
	if !strings.Contains(body, "/reset confirm") {
		t.Fatalf("dispatch should have routed /reset to preview, not rejected; got:\n%s", body)
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
