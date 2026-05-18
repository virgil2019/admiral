package autopilot

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	"github.com/georgehuang/admiral/internal/config"
	"github.com/georgehuang/admiral/internal/linear"
	"github.com/georgehuang/admiral/internal/store"
)

// newBlockerOrchestrator builds a minimal orchestrator for blocker tests —
// only db and lc are needed; runSlots / ciWatcher / blockerWatcher are not
// used by the paths under test.
func newBlockerOrchestrator(db *mockStore, lc *mockLinearClient) *Orchestrator {
	return &Orchestrator{
		cfg:    &config.Autopilot{MaxRunSeconds: 60, MaxConcurrentRuns: 3},
		db:     db,
		lc:     lc,
		logger: slog.Default(),
	}
}

// --- dispatchFreshAssign blocker branch ---

func TestDispatchFreshAssign_Blocked(t *testing.T) {
	db := &mockStore{} // ClaimAdmiralTask returns fresh=true by default
	lc := &mockLinearClient{
		IssueBlockers: []linear.IssueBlocker{
			{IssueID: "blocker-1", IssueIdentifier: "GEO-1"},
		},
	}
	o := newBlockerOrchestrator(db, lc)
	// runSlots deliberately nil — acquireRunSlot returns immediately for nil,
	// and the blocked path must not reach run() at all.

	ev := linear.AgentEvent{
		IssueID:         "issue-42",
		IssueIdentifier: "GEO-42",
		SessionID:       "sess-42",
		Action:          linear.ActionCreated,
	}
	o.dispatchFreshAssign(ev)

	if len(db.SetBlockedCalls) != 1 {
		t.Fatalf("expected SetAdmiralTaskBlocked called once, got %d", len(db.SetBlockedCalls))
	}
	if db.SetBlockedCalls[0] != "issue-42" {
		t.Errorf("SetAdmiralTaskBlocked called with %q, want %q", db.SetBlockedCalls[0], "issue-42")
	}
	// PostAgentActivity should have been called with a Thought (non-terminal).
	lc.PostedBodyMu.Lock()
	body := lc.PostedBody
	lc.PostedBodyMu.Unlock()
	if body == "" {
		t.Error("expected PostAgentActivity to be called for blocked notice")
	}
}

func TestDispatchFreshAssign_BlockerCheckFails_Proceeds(t *testing.T) {
	db := &mockStore{}
	lc := &mockLinearClient{
		IssueBlockersErr: context.DeadlineExceeded,
	}
	o := newBlockerOrchestrator(db, lc)

	ev := linear.AgentEvent{
		IssueID:         "issue-43",
		IssueIdentifier: "GEO-43",
		SessionID:       "sess-43",
		Action:          linear.ActionCreated,
	}
	// Should not panic or block; graceful degradation proceeds to run.
	o.dispatchFreshAssign(ev)

	if len(db.SetBlockedCalls) != 0 {
		t.Errorf("expected no SetAdmiralTaskBlocked on API error, got %d calls", len(db.SetBlockedCalls))
	}
}

// --- BlockerWatcher ---

func TestBlockerWatcher_CheckOne_Unblocks(t *testing.T) {
	db := &mockStore{
		BlockedTasks: []store.BlockedTask{
			{
				IssueID:            "issue-99",
				IssueIdentifier:    "GEO-99",
				LastEventSessionID: "sess-99",
				AttemptN:           1,
			},
		},
		TransitionBlockedOK: true,
	}
	lc := &mockLinearClient{
		IssueBlockers: nil, // no blockers — all resolved
	}
	o := newBlockerOrchestrator(db, lc)

	done := make(chan struct{}, 1)
	var ranEv linear.AgentEvent
	var ranAttempt int
	w := newBlockerWatcher(o, 0)
	w.runFn = func(ev linear.AgentEvent, attempt int) {
		ranEv = ev
		ranAttempt = attempt
		done <- struct{}{}
	}

	got := w.checkOne(context.Background(), db.BlockedTasks[0])
	if !got {
		t.Fatal("expected checkOne to return true (task unblocked)")
	}

	select {
	case <-done:
	case <-context.Background().Done():
		t.Fatal("runFn was never called")
	}
	if ranEv.IssueID != "issue-99" {
		t.Errorf("runFn called with IssueID %q, want %q", ranEv.IssueID, "issue-99")
	}
	if ranAttempt != 1 {
		t.Errorf("runFn called with attempt %d, want 1", ranAttempt)
	}
}

func TestBlockerWatcher_CheckOne_StillBlocked(t *testing.T) {
	db := &mockStore{TransitionBlockedOK: true}
	lc := &mockLinearClient{
		IssueBlockers: []linear.IssueBlocker{{IssueID: "b-1", IssueIdentifier: "GEO-1"}},
	}
	o := newBlockerOrchestrator(db, lc)
	w := newBlockerWatcher(o, 0)
	called := false
	w.runFn = func(ev linear.AgentEvent, attempt int) { called = true }

	task := store.BlockedTask{IssueID: "issue-5", IssueIdentifier: "GEO-5"}
	got := w.checkOne(context.Background(), task)
	if got {
		t.Error("expected checkOne to return false (still blocked)")
	}
	if called {
		t.Error("runFn must not be called when task is still blocked")
	}
}

func TestBlockerWatcher_CheckOne_TransitionLost(t *testing.T) {
	db := &mockStore{TransitionBlockedOK: false} // another goroutine won
	lc := &mockLinearClient{IssueBlockers: nil}
	o := newBlockerOrchestrator(db, lc)
	w := newBlockerWatcher(o, 0)
	called := false
	w.runFn = func(ev linear.AgentEvent, attempt int) { called = true }

	task := store.BlockedTask{IssueID: "issue-6", IssueIdentifier: "GEO-6"}
	got := w.checkOne(context.Background(), task)
	if got {
		t.Error("expected checkOne to return false when transition was lost")
	}
	if called {
		t.Error("runFn must not be called when transition was not won")
	}
}

func TestBlockerWatcher_CheckAll_RateLimit(t *testing.T) {
	// Build more blocked tasks than MaxConcurrentRuns (3).
	tasks := make([]store.BlockedTask, 6)
	for i := range tasks {
		tasks[i] = store.BlockedTask{
			IssueID:         "issue-rl-" + string(rune('0'+i)),
			IssueIdentifier: "GEO-RL",
			AttemptN:        1,
		}
	}
	db := &mockStore{
		BlockedTasks:        tasks,
		TransitionBlockedOK: true,
	}
	lc := &mockLinearClient{IssueBlockers: nil}
	o := newBlockerOrchestrator(db, lc)

	var mu sync.Mutex
	requeued := 0
	w := newBlockerWatcher(o, 0)
	w.runFn = func(_ linear.AgentEvent, _ int) {
		mu.Lock()
		requeued++
		mu.Unlock()
	}

	w.checkAll(context.Background())

	mu.Lock()
	defer mu.Unlock()
	// MaxConcurrentRuns = 3, so at most 3 tasks should be re-queued per tick.
	if requeued > 3 {
		t.Errorf("rate limit breached: %d tasks re-queued, want ≤ 3", requeued)
	}
}
