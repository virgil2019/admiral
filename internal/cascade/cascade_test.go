package cascade

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/georgehuang/admiral/internal/linear"
)

type fakeLinear struct {
	parents map[string]string
	subs    map[string][]linear.SubIssue
	parErr  error
	subErr  error
}

func (f *fakeLinear) GetParentID(_ context.Context, childID string) (string, error) {
	if f.parErr != nil {
		return "", f.parErr
	}
	return f.parents[childID], nil
}

func (f *fakeLinear) GetSubIssues(_ context.Context, parentID string) ([]linear.SubIssue, error) {
	if f.subErr != nil {
		return nil, f.subErr
	}
	return f.subs[parentID], nil
}

type fakeStore struct {
	calls []enqArg
	fresh bool
	err   error
}

type enqArg struct {
	source, webhookID, action, sessionID, issueID, payload, commentID string
}

func (f *fakeStore) EnqueueEventWithSource(source, webhookID, action, sessionID, issueID, payloadJSON, commentID string) (bool, error) {
	f.calls = append(f.calls, enqArg{source, webhookID, action, sessionID, issueID, payloadJSON, commentID})
	if f.err != nil {
		return false, f.err
	}
	return f.fresh, nil
}

func TestMaybeEnqueueVerify_EnqueuesWhenAllSiblingsCompleted(t *testing.T) {
	lc := &fakeLinear{
		parents: map[string]string{"leaf": "task-parent"},
		subs: map[string][]linear.SubIssue{
			"task-parent": {
				{ID: "leaf", StateType: "completed"},
				{ID: "sib", StateType: "completed"},
			},
		},
	}
	st := &fakeStore{fresh: true}

	MaybeEnqueueVerify(context.Background(), lc, st, slog.Default(), "leaf", "GEO-1")

	if len(st.calls) != 1 {
		t.Fatalf("expected one enqueue, got %d", len(st.calls))
	}
	got := st.calls[0]
	if got.source != "verify" || got.action != "verify.task_complete" {
		t.Errorf("wrong event header: %+v", got)
	}
	if got.sessionID != "task-parent" || got.issueID != "task-parent" {
		t.Errorf("event not routed to parent: %+v", got)
	}
	if got.webhookID != "verify-task-parent-leaf" {
		t.Errorf("webhookID = %q, want verify-task-parent-leaf", got.webhookID)
	}
}

func TestMaybeEnqueueVerify_NoParentReturnsCleanly(t *testing.T) {
	lc := &fakeLinear{parents: map[string]string{}} // top-of-tree
	st := &fakeStore{}

	MaybeEnqueueVerify(context.Background(), lc, st, slog.Default(), "top", "GEO-T")

	if len(st.calls) != 0 {
		t.Errorf("top-of-tree must not enqueue, got %v", st.calls)
	}
}

func TestMaybeEnqueueVerify_SkipsWhenSiblingsIncomplete(t *testing.T) {
	lc := &fakeLinear{
		parents: map[string]string{"leaf": "task-parent"},
		subs: map[string][]linear.SubIssue{
			"task-parent": {
				{ID: "leaf", StateType: "completed"},
				{ID: "sib", StateType: "started"}, // still in flight
			},
		},
	}
	st := &fakeStore{}

	MaybeEnqueueVerify(context.Background(), lc, st, slog.Default(), "leaf", "GEO-1")

	if len(st.calls) != 0 {
		t.Errorf("incomplete siblings must not enqueue, got %v", st.calls)
	}
}

func TestMaybeEnqueueVerify_GetParentIDErrorDoesNotEnqueue(t *testing.T) {
	lc := &fakeLinear{parErr: errors.New("linear down")}
	st := &fakeStore{}

	MaybeEnqueueVerify(context.Background(), lc, st, slog.Default(), "leaf", "GEO-1")

	if len(st.calls) != 0 {
		t.Errorf("parent lookup error must short-circuit, got %v", st.calls)
	}
}

func TestAllSubsCompleted(t *testing.T) {
	if AllSubsCompleted(nil) {
		t.Error("nil should not be complete")
	}
	if AllSubsCompleted([]linear.SubIssue{}) {
		t.Error("empty list should not be complete (anomaly, not done)")
	}
	if !AllSubsCompleted([]linear.SubIssue{{StateType: "completed"}, {StateType: "completed"}}) {
		t.Error("all completed should be true")
	}
	if AllSubsCompleted([]linear.SubIssue{{StateType: "completed"}, {StateType: "canceled"}}) {
		t.Error("canceled blocks completion (must be human-resolved)")
	}
}
