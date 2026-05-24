package discoverer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/georgehuang/admiral/internal/linear"
	"github.com/georgehuang/admiral/internal/store"
)

type fakeLinear struct {
	mu      sync.Mutex
	scanRet []linear.Issue
	scanErr error
	assigns []assignCall
	assnErr error
}

type assignCall struct {
	IssueID string
	UserID  string
}

func (f *fakeLinear) SearchAssignableIssues(_ context.Context, _ linear.SearchFilter) ([]linear.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.scanRet, f.scanErr
}

func (f *fakeLinear) AssignIssue(_ context.Context, issueID, userID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.assnErr != nil {
		return f.assnErr
	}
	f.assigns = append(f.assigns, assignCall{IssueID: issueID, UserID: userID})
	return nil
}

func (f *fakeLinear) assignedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.assigns))
	for _, a := range f.assigns {
		out = append(out, a.IssueID)
	}
	return out
}

type fakeStore struct {
	tracked map[string]*store.AdmiralTask
	err     error
}

func (s *fakeStore) GetAdmiralTaskByIssue(id string) (*store.AdmiralTask, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.tracked[id], nil
}

type fakeJudge struct {
	verdicts map[string]Verdict
	calls    int
	err      error
}

func (j *fakeJudge) Judge(_ context.Context, iss linear.Issue) (Verdict, error) {
	j.calls++
	if j.err != nil {
		return Verdict{}, j.err
	}
	if v, ok := j.verdicts[iss.ID]; ok {
		return v, nil
	}
	return Verdict{Decision: "no", Reason: "default"}, nil
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func newSvc(cfg Config, lc linearClient, ts taskRegistry, j judger) *Service {
	return &Service{cfg: cfg, linear: lc, store: ts, judge: j, logger: discardLogger()}
}

func TestConsiderSkipsExistingTask(t *testing.T) {
	lc := &fakeLinear{}
	ts := &fakeStore{tracked: map[string]*store.AdmiralTask{
		"iss-1": {IssueID: "iss-1", State: "RECEIVED"},
	}}
	svc := newSvc(Config{AdmiralUserID: "u-1"}, lc, ts, nil)
	ok := svc.consider(context.Background(), linear.Issue{ID: "iss-1", Identifier: "GEO-1"})
	if ok {
		t.Fatal("expected consider to skip existing task")
	}
	if len(lc.assignedIDs()) != 0 {
		t.Fatal("AssignIssue must not be called when task exists")
	}
}

func TestConsiderJudgeRejects(t *testing.T) {
	lc := &fakeLinear{}
	ts := &fakeStore{}
	j := &fakeJudge{verdicts: map[string]Verdict{"iss-2": {Decision: "no", Reason: "too vague"}}}
	svc := newSvc(Config{
		AdmiralUserID: "u-1",
		Judge:         JudgeConfig{Enabled: true},
	}, lc, ts, j)
	ok := svc.consider(context.Background(), linear.Issue{ID: "iss-2", Identifier: "GEO-2"})
	if ok {
		t.Fatal("expected consider to skip when judge rejects")
	}
	if len(lc.assignedIDs()) != 0 {
		t.Fatal("AssignIssue must not be called when judge says no")
	}
	if j.calls != 1 {
		t.Errorf("expected 1 judge call, got %d", j.calls)
	}
}

func TestConsiderJudgeApprovesAndAssigns(t *testing.T) {
	lc := &fakeLinear{}
	ts := &fakeStore{}
	j := &fakeJudge{verdicts: map[string]Verdict{"iss-3": {Decision: "yes", Reason: "clear task"}}}
	svc := newSvc(Config{
		AdmiralUserID: "u-1",
		Judge:         JudgeConfig{Enabled: true},
	}, lc, ts, j)
	ok := svc.consider(context.Background(), linear.Issue{ID: "iss-3", Identifier: "GEO-3"})
	if !ok {
		t.Fatal("expected consider to accept")
	}
	got := lc.assignedIDs()
	if len(got) != 1 || got[0] != "iss-3" {
		t.Errorf("unexpected assigns: %v", got)
	}
}

func TestConsiderJudgeDisabledAlwaysAssigns(t *testing.T) {
	lc := &fakeLinear{}
	ts := &fakeStore{}
	svc := newSvc(Config{AdmiralUserID: "u-1"}, lc, ts, nil)
	ok := svc.consider(context.Background(), linear.Issue{ID: "iss-4", Identifier: "GEO-4"})
	if !ok {
		t.Fatal("expected consider to accept (judge disabled)")
	}
	if got := lc.assignedIDs(); len(got) != 1 || got[0] != "iss-4" {
		t.Errorf("unexpected assigns: %v", got)
	}
}

func TestConsiderAssignErrorReturnsFalse(t *testing.T) {
	lc := &fakeLinear{assnErr: errors.New("boom")}
	ts := &fakeStore{}
	svc := newSvc(Config{AdmiralUserID: "u-1"}, lc, ts, nil)
	if svc.consider(context.Background(), linear.Issue{ID: "iss-5", Identifier: "GEO-5"}) {
		t.Fatal("expected consider to return false on assign error")
	}
}

func TestTickRespectsMaxPickPerRound(t *testing.T) {
	lc := &fakeLinear{
		scanRet: []linear.Issue{
			{ID: "a", Identifier: "GEO-A"},
			{ID: "b", Identifier: "GEO-B"},
			{ID: "c", Identifier: "GEO-C"},
			{ID: "d", Identifier: "GEO-D"},
		},
	}
	ts := &fakeStore{}
	svc := newSvc(Config{AdmiralUserID: "u-1", MaxPickPerRound: 2}, lc, ts, nil)
	svc.tick(context.Background())
	got := lc.assignedIDs()
	if len(got) != 2 {
		t.Fatalf("expected 2 assigns, got %d (%v)", len(got), got)
	}
	if got[0] != "a" || got[1] != "b" {
		t.Errorf("expected first two issues to be picked, got %v", got)
	}
}

func TestTickHonorsContextCancellation(t *testing.T) {
	lc := &fakeLinear{
		scanRet: []linear.Issue{
			{ID: "a", Identifier: "GEO-A"},
			{ID: "b", Identifier: "GEO-B"},
		},
	}
	ts := &fakeStore{}
	svc := newSvc(Config{AdmiralUserID: "u-1"}, lc, ts, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	svc.tick(ctx)
	// Allow at most one assign before cancellation bites between iterations.
	if got := lc.assignedIDs(); len(got) > 1 {
		t.Errorf("expected cancellation to short-circuit, got %d assigns", len(got))
	}
}

func TestTickScanFailureDoesNotPanic(t *testing.T) {
	lc := &fakeLinear{scanErr: errors.New("net down")}
	ts := &fakeStore{}
	svc := newSvc(Config{AdmiralUserID: "u-1"}, lc, ts, nil)
	svc.tick(context.Background())
	if len(lc.assignedIDs()) != 0 {
		t.Fatal("expected no assigns on scan failure")
	}
}

func TestRunRejectsZeroInterval(t *testing.T) {
	svc := newSvc(Config{AdmiralUserID: "u-1"}, &fakeLinear{}, &fakeStore{}, nil)
	err := svc.Run(context.Background())
	if err == nil || err.Error() == "" {
		t.Fatal("expected Run to error on zero PollInterval")
	}
}

func TestRunRejectsMissingUserID(t *testing.T) {
	svc := newSvc(Config{PollInterval: time.Second}, &fakeLinear{}, &fakeStore{}, nil)
	if err := svc.Run(context.Background()); err == nil {
		t.Fatal("expected Run to error on missing AdmiralUserID")
	}
}
