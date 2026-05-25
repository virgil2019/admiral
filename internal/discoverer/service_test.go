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
	mu          sync.Mutex
	scanRet     []linear.Issue
	scanErr     error
	assigns     []assignCall
	assnErr     error
	blockers    map[string][]linear.IssueBlocker
	blockersErr error
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

func (f *fakeLinear) GetIssueBlockers(_ context.Context, issueID string) ([]linear.IssueBlocker, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.blockersErr != nil {
		return nil, f.blockersErr
	}
	return f.blockers[issueID], nil
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
	mu         sync.Mutex
	tracked    map[string]*store.AdmiralTask
	picks      map[string]*store.DiscovererPick
	projectIDs []string
	taskErr    error
	pickGetErr error
	pickUpsErr error
	projErr    error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		tracked:    map[string]*store.AdmiralTask{},
		picks:      map[string]*store.DiscovererPick{},
		projectIDs: []string{"proj-A"},
	}
}

func (s *fakeStore) GetAdmiralTaskByIssue(id string) (*store.AdmiralTask, error) {
	if s.taskErr != nil {
		return nil, s.taskErr
	}
	return s.tracked[id], nil
}

func (s *fakeStore) ListAutoPickEnabledProjectIDs() ([]string, error) {
	if s.projErr != nil {
		return nil, s.projErr
	}
	return s.projectIDs, nil
}

func (s *fakeStore) GetDiscovererPick(id string) (*store.DiscovererPick, error) {
	if s.pickGetErr != nil {
		return nil, s.pickGetErr
	}
	return s.picks[id], nil
}

func (s *fakeStore) UpsertDiscovererPick(p store.DiscovererPick) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pickUpsErr != nil {
		return s.pickUpsErr
	}
	cp := p
	if cp.PickedAt == "" {
		cp.PickedAt = "now"
	}
	cp.UpdatedAt = "now"
	s.picks[p.IssueID] = &cp
	return nil
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
	ts := newFakeStore()
	ts.tracked["iss-1"] = &store.AdmiralTask{IssueID: "iss-1", State: "RECEIVED"}
	svc := newSvc(Config{AdmiralUserID: "u-1"}, lc, ts, nil)
	if svc.consider(context.Background(), linear.Issue{ID: "iss-1", Identifier: "GEO-1"}) {
		t.Fatal("expected consider to skip existing task")
	}
	if len(lc.assignedIDs()) != 0 {
		t.Fatal("AssignIssue must not be called when task exists")
	}
}

func TestConsiderSkipsAlreadyPickedSameState(t *testing.T) {
	lc := &fakeLinear{}
	ts := newFakeStore()
	ts.picks["iss-2"] = &store.DiscovererPick{IssueID: "iss-2", PickedState: "Backlog"}
	svc := newSvc(Config{AdmiralUserID: "u-1"}, lc, ts, nil)
	if svc.consider(context.Background(), linear.Issue{ID: "iss-2", Identifier: "GEO-2", StateName: "Backlog"}) {
		t.Fatal("expected consider to skip when same state still")
	}
	if len(lc.assignedIDs()) != 0 {
		t.Fatal("AssignIssue must not be called on stale repick")
	}
}

func TestConsiderRepicksOnStateChange(t *testing.T) {
	lc := &fakeLinear{}
	ts := newFakeStore()
	ts.picks["iss-3"] = &store.DiscovererPick{IssueID: "iss-3", PickedState: "Backlog"}
	svc := newSvc(Config{AdmiralUserID: "u-1"}, lc, ts, nil)
	if !svc.consider(context.Background(), linear.Issue{ID: "iss-3", Identifier: "GEO-3", StateName: "Todo"}) {
		t.Fatal("expected consider to repick after state change")
	}
	if got := lc.assignedIDs(); len(got) != 1 || got[0] != "iss-3" {
		t.Errorf("unexpected assigns: %v", got)
	}
	if p := ts.picks["iss-3"]; p == nil || p.PickedState != "Todo" {
		t.Errorf("picks row should have been refreshed to Todo, got %+v", p)
	}
}

func TestConsiderJudgeRejects(t *testing.T) {
	lc := &fakeLinear{}
	ts := newFakeStore()
	j := &fakeJudge{verdicts: map[string]Verdict{"iss-4": {Decision: "no", Reason: "too vague"}}}
	svc := newSvc(Config{
		AdmiralUserID: "u-1",
		Judge:         JudgeConfig{Enabled: true},
	}, lc, ts, j)
	if svc.consider(context.Background(), linear.Issue{ID: "iss-4", Identifier: "GEO-4"}) {
		t.Fatal("expected consider to skip when judge rejects")
	}
	if len(lc.assignedIDs()) != 0 {
		t.Fatal("AssignIssue must not be called when judge says no")
	}
	if _, ok := ts.picks["iss-4"]; ok {
		t.Fatal("picks row must not be written when judge rejects")
	}
}

func TestConsiderJudgeApprovesAndAssigns(t *testing.T) {
	lc := &fakeLinear{}
	ts := newFakeStore()
	j := &fakeJudge{verdicts: map[string]Verdict{"iss-5": {Decision: "yes", Reason: "clear task"}}}
	svc := newSvc(Config{
		AdmiralUserID: "u-1",
		Judge:         JudgeConfig{Enabled: true},
	}, lc, ts, j)
	if !svc.consider(context.Background(), linear.Issue{ID: "iss-5", Identifier: "GEO-5", StateName: "Todo"}) {
		t.Fatal("expected consider to accept")
	}
	if got := lc.assignedIDs(); len(got) != 1 || got[0] != "iss-5" {
		t.Errorf("unexpected assigns: %v", got)
	}
	if p := ts.picks["iss-5"]; p == nil || p.PickedState != "Todo" {
		t.Errorf("expected picks row with state Todo, got %+v", p)
	}
}

func TestConsiderJudgeDisabledAlwaysAssigns(t *testing.T) {
	lc := &fakeLinear{}
	ts := newFakeStore()
	svc := newSvc(Config{AdmiralUserID: "u-1"}, lc, ts, nil)
	if !svc.consider(context.Background(), linear.Issue{ID: "iss-6", Identifier: "GEO-6", StateName: "Backlog"}) {
		t.Fatal("expected consider to accept (judge disabled)")
	}
	if got := lc.assignedIDs(); len(got) != 1 || got[0] != "iss-6" {
		t.Errorf("unexpected assigns: %v", got)
	}
}

func TestConsiderAssignErrorDoesNotWritePicks(t *testing.T) {
	lc := &fakeLinear{assnErr: errors.New("boom")}
	ts := newFakeStore()
	svc := newSvc(Config{AdmiralUserID: "u-1"}, lc, ts, nil)
	if svc.consider(context.Background(), linear.Issue{ID: "iss-7", Identifier: "GEO-7"}) {
		t.Fatal("expected consider to return false on assign error")
	}
	if _, ok := ts.picks["iss-7"]; ok {
		t.Fatal("picks row must not be written when assign fails")
	}
}

func TestConsiderSkipsBlockedIssue(t *testing.T) {
	lc := &fakeLinear{
		blockers: map[string][]linear.IssueBlocker{
			"iss-blk": {{IssueID: "b-1", IssueIdentifier: "GEO-100"}},
		},
	}
	ts := newFakeStore()
	svc := newSvc(Config{AdmiralUserID: "u-1"}, lc, ts, nil)
	if svc.consider(context.Background(), linear.Issue{ID: "iss-blk", Identifier: "GEO-9", StateName: "Todo"}) {
		t.Fatal("expected consider to skip blocked issue")
	}
	if len(lc.assignedIDs()) != 0 {
		t.Fatal("AssignIssue must not be called when blocked")
	}
	if _, ok := ts.picks["iss-blk"]; ok {
		t.Fatal("picks row must not be written when blocked (so next tick can re-evaluate)")
	}
}

func TestConsiderBlockerCheckRunsBeforeJudge(t *testing.T) {
	lc := &fakeLinear{
		blockers: map[string][]linear.IssueBlocker{
			"iss-blk2": {{IssueID: "b-1", IssueIdentifier: "GEO-100"}},
		},
	}
	ts := newFakeStore()
	j := &fakeJudge{verdicts: map[string]Verdict{"iss-blk2": {Decision: "yes"}}}
	svc := newSvc(Config{
		AdmiralUserID: "u-1",
		Judge:         JudgeConfig{Enabled: true},
	}, lc, ts, j)
	if svc.consider(context.Background(), linear.Issue{ID: "iss-blk2", Identifier: "GEO-10"}) {
		t.Fatal("expected consider to skip blocked issue")
	}
	if j.calls != 0 {
		t.Errorf("judge must not be invoked for blocked issues, got %d calls", j.calls)
	}
}

func TestConsiderBlockerCheckFailsOpen(t *testing.T) {
	lc := &fakeLinear{blockersErr: errors.New("linear 5xx")}
	ts := newFakeStore()
	svc := newSvc(Config{AdmiralUserID: "u-1"}, lc, ts, nil)
	if !svc.consider(context.Background(), linear.Issue{ID: "iss-err", Identifier: "GEO-11", StateName: "Todo"}) {
		t.Fatal("expected consider to fail-open on blocker API error")
	}
	if got := lc.assignedIDs(); len(got) != 1 || got[0] != "iss-err" {
		t.Errorf("expected assign despite blocker API error, got %v", got)
	}
}

func TestConsiderUnblockedAssigns(t *testing.T) {
	lc := &fakeLinear{
		blockers: map[string][]linear.IssueBlocker{}, // no blockers
	}
	ts := newFakeStore()
	svc := newSvc(Config{AdmiralUserID: "u-1"}, lc, ts, nil)
	if !svc.consider(context.Background(), linear.Issue{ID: "iss-ok", Identifier: "GEO-12", StateName: "Todo"}) {
		t.Fatal("expected consider to assign when no blockers")
	}
	if got := lc.assignedIDs(); len(got) != 1 || got[0] != "iss-ok" {
		t.Errorf("expected assign, got %v", got)
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
	ts := newFakeStore()
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

func TestTickSkippedWhenNoAutoPickProjects(t *testing.T) {
	lc := &fakeLinear{scanRet: []linear.Issue{{ID: "a", Identifier: "GEO-A"}}}
	ts := newFakeStore()
	ts.projectIDs = nil
	svc := newSvc(Config{AdmiralUserID: "u-1"}, lc, ts, nil)
	svc.tick(context.Background())
	if len(lc.assignedIDs()) != 0 {
		t.Fatal("expected scan to be skipped when no auto-pick projects exist")
	}
}

func TestTickHonorsContextCancellation(t *testing.T) {
	lc := &fakeLinear{
		scanRet: []linear.Issue{
			{ID: "a", Identifier: "GEO-A"},
			{ID: "b", Identifier: "GEO-B"},
		},
	}
	ts := newFakeStore()
	svc := newSvc(Config{AdmiralUserID: "u-1"}, lc, ts, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	svc.tick(ctx)
	if got := lc.assignedIDs(); len(got) > 1 {
		t.Errorf("expected cancellation to short-circuit, got %d assigns", len(got))
	}
}

func TestTickScanFailureDoesNotPanic(t *testing.T) {
	lc := &fakeLinear{scanErr: errors.New("net down")}
	ts := newFakeStore()
	svc := newSvc(Config{AdmiralUserID: "u-1"}, lc, ts, nil)
	svc.tick(context.Background())
	if len(lc.assignedIDs()) != 0 {
		t.Fatal("expected no assigns on scan failure")
	}
}

func TestRunRejectsZeroInterval(t *testing.T) {
	svc := newSvc(Config{AdmiralUserID: "u-1"}, &fakeLinear{}, newFakeStore(), nil)
	if err := svc.Run(context.Background()); err == nil {
		t.Fatal("expected Run to error on zero PollInterval")
	}
}

func TestRunRejectsMissingUserID(t *testing.T) {
	svc := newSvc(Config{PollInterval: time.Second}, &fakeLinear{}, newFakeStore(), nil)
	if err := svc.Run(context.Background()); err == nil {
		t.Fatal("expected Run to error on missing AdmiralUserID")
	}
}
