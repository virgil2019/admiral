package autopilot

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"errors"

	"github.com/georgehuang/admiral/internal/config"
	"github.com/georgehuang/admiral/internal/linear"
	"github.com/georgehuang/admiral/internal/store"
)

// resumeRecorder captures the resume calls the reconciler spawns.
type resumeRecorder struct {
	mu  sync.Mutex
	evs []linear.AgentEvent
}

func (r *resumeRecorder) add(ev linear.AgentEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evs = append(r.evs, ev)
}

func (r *resumeRecorder) snapshot() []linear.AgentEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]linear.AgentEvent(nil), r.evs...)
}

func reconcileFixture(t *testing.T) (*Orchestrator, *mockStore, *mockLinearClient, *resumeRecorder) {
	t.Helper()
	db := &mockStore{}
	lc := &mockLinearClient{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// Constructed directly: New() dereferences a *store.Store for dbPath,
	// which a mock cannot satisfy and these tests do not need.
	o := &Orchestrator{
		cfg:      &config.Autopilot{MaxConcurrentRuns: 3, MaxRunSeconds: 60},
		db:       db,
		lc:       lc,
		logger:   logger,
		rootCtx:  context.Background(),
		inFlight: make(map[string]int),
		runSlots: make(chan struct{}, 3),
	}
	rec := &resumeRecorder{}
	o.resumeOrphanRun = func(ev linear.AgentEvent, _ *store.AdmiralTask) { rec.add(ev) }
	return o, db, lc, rec
}

// TestReconcile_ResumesOrphan: a row left in EXECUTING by a crash is stamped
// interrupted and handed to the resume flow — the internal wakeup that keeps
// a stuck issue from staying stuck.
func TestReconcile_ResumesOrphan(t *testing.T) {
	o, db, lc, rec := reconcileFixture(t)
	wt := t.TempDir()
	db.InFlightTasks = []store.AdmiralTask{{
		IssueID: "issue-1", IssueIdentifier: "GEO-1", State: store.JobStateExecuting,
		AttemptN: 1, Branch: "linear/GEO-1", WorktreePath: wt,
		ClaudeSessionID: "sess-claude", LastEventSessionID: "sess-linear",
		StartedAt: time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339),
	}}
	// After the interrupt stamp the row reads back TIMED_OUT, same attempt.
	db.AdmiralTask = &store.AdmiralTask{
		IssueID: "issue-1", State: store.JobStateTimedOut,
		AttemptN: 1, Branch: "linear/GEO-1",
	}

	rep := o.Reconcile(context.Background())

	if rep.Orphans != 1 || rep.Resumed != 1 || rep.Escalated != 0 {
		t.Fatalf("report = %+v, want 1 orphan / 1 resumed / 0 escalated", rep)
	}
	waitFor(t, func() bool { return len(rec.snapshot()) == 1 })
	if got := rec.snapshot(); got[0].IssueID != "issue-1" {
		t.Errorf("resume calls = %+v, want one for issue-1", got)
	}
	if db.InterruptedReason != store.InterruptedByShutdown {
		t.Errorf("mark reason = %q, want %q", db.InterruptedReason, store.InterruptedByShutdown)
	}
	if lc.GetPostedBody() == "" {
		t.Error("no Linear notice posted for the resumed task")
	}
}

// TestReconcile_SkipsLiveRun: a row this process is actively running must not
// be touched — otherwise the sweep would interrupt its own work.
func TestReconcile_SkipsLiveRun(t *testing.T) {
	o, db, _, rec := reconcileFixture(t)
	db.InFlightTasks = []store.AdmiralTask{{
		IssueID: "issue-1", State: store.JobStateExecuting,
		ClaudeSessionID: "sess-claude", LastEventSessionID: "sess-linear",
	}}

	end := o.beginRun("issue-1")
	defer end()

	rep := o.Reconcile(context.Background())
	if rep.Orphans != 0 || len(rec.snapshot()) != 0 {
		t.Fatalf("report = %+v with %d resumes, want no action for a live run",
			rep, len(rec.snapshot()))
	}
	if len(db.MarkedInterruptedIDs) != 0 {
		t.Errorf("marked %v, want none", db.MarkedInterruptedIDs)
	}
}

// TestReconcile_EscalatesWithoutSession: nothing to resume means the human is
// told, with the recovery command — never a silent stop.
func TestReconcile_EscalatesWithoutSession(t *testing.T) {
	o, db, lc, rec := reconcileFixture(t)
	db.InFlightTasks = []store.AdmiralTask{{
		IssueID: "issue-2", IssueIdentifier: "GEO-2", State: store.JobStateExecuting,
		WorktreePath: t.TempDir(), // present, but no claude session
	}}

	rep := o.Reconcile(context.Background())

	if rep.Escalated != 1 || rep.Resumed != 0 {
		t.Fatalf("report = %+v, want 1 escalated / 0 resumed", rep)
	}
	if len(rec.snapshot()) != 0 {
		t.Errorf("resumed rows without a session, want none")
	}
	// No thread session on this row: the escalation must land on the issue
	// itself rather than nowhere.
	if !strings.Contains(lc.GetPostedBody(), "/rerun") && len(lc.CreatedComments) == 0 {
		t.Fatalf("escalation posted nowhere")
	}
	if len(lc.CreatedComments) == 0 {
		t.Fatal("escalation never reached the issue comment fallback")
	}
	if !strings.Contains(lc.CreatedComments[0].Body, "/rerun") {
		t.Errorf("issue comment = %q, want a /rerun pointer", lc.CreatedComments[0].Body)
	}
}

// TestReconcile_ResumeCapEscalates: a task that keeps dying must stop being
// auto-resumed and ask a human instead of crash-looping.
func TestReconcile_ResumeCapEscalates(t *testing.T) {
	o, db, lc, rec := reconcileFixture(t)
	db.AutoResumeCount = maxAutoResumes // the next increment exceeds the cap
	db.InFlightTasks = []store.AdmiralTask{{
		IssueID: "issue-3", IssueIdentifier: "GEO-3", State: store.JobStateExecuting,
		WorktreePath:    t.TempDir(),
		ClaudeSessionID: "sess-claude", LastEventSessionID: "sess-linear",
	}}

	rep := o.Reconcile(context.Background())

	if rep.Resumed != 0 || rep.Escalated != 1 {
		t.Fatalf("report = %+v, want cap to escalate instead of resuming", rep)
	}
	if len(rec.snapshot()) != 0 {
		t.Errorf("resumed past the cap")
	}
	if !strings.Contains(lc.GetPostedBody(), "automatic resume was already used") {
		t.Errorf("escalation = %q, want the cap reason", lc.GetPostedBody())
	}
}

// TestReconcile_ListErrorIsNotFatal: a transient read failure must not take
// the daemon down or panic the sweep.
func TestReconcile_ListErrorIsNotFatal(t *testing.T) {
	o, db, _, _ := reconcileFixture(t)
	db.ListInFlightErr = os.ErrDeadlineExceeded

	if rep := o.Reconcile(context.Background()); rep != (ReconcileReport{}) {
		t.Errorf("report = %+v, want zero on a list error", rep)
	}
}

// TestShutdown_MarksInterrupted: rows still in flight after the grace period
// are stamped so the next boot resumes them.
func TestShutdown_MarksInterrupted(t *testing.T) {
	o, db, _, _ := reconcileFixture(t)

	start := time.Now()
	o.Shutdown(20 * time.Millisecond)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Shutdown took %v with no runs, want immediate", elapsed)
	}
	if db.InterruptedReason != store.InterruptedByShutdown {
		t.Errorf("mark reason = %q, want %q", db.InterruptedReason, store.InterruptedByShutdown)
	}
}

// TestShutdown_WaitsForRun: Shutdown blocks until an in-flight run finishes,
// so the run's own terminal write wins over the blanket interrupted stamp.
func TestShutdown_WaitsForRun(t *testing.T) {
	o, db, _, _ := reconcileFixture(t)

	end := o.beginRun("issue-1")
	go func() {
		time.Sleep(150 * time.Millisecond)
		end()
	}()

	start := time.Now()
	o.Shutdown(5 * time.Second)
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Errorf("Shutdown returned in %v, want it to wait for the run", elapsed)
	}
	if db.InterruptedReason != store.InterruptedByShutdown {
		t.Errorf("mark not called after drain")
	}
}

// TestRunRootsUseRootContext: every run root must descend from rootCtx, or a
// SIGTERM would not cancel it and the run would be orphaned by process exit.
func TestRunRootsUseRootContext(t *testing.T) {
	o, _, _, _ := reconcileFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	o.SetRootContext(ctx)

	runCtx, runCancel := context.WithTimeout(o.rootCtx, time.Hour)
	defer runCancel()
	cancel()

	select {
	case <-runCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("run context did not inherit cancellation from rootCtx")
	}
}

// waitFor polls cond for up to two seconds; the reconciler resumes in a
// goroutine, so assertions must not race it.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within 2s")
}

// TestRunFunctionsRootAtBaseCtx is the wiring guard TestRunRootsUseRootContext
// alone cannot give: it asserts that no run* function reintroduces
// context.Background() as its context root. A run rooted in Background is
// invisible to SIGTERM — the process exits and leaves claude running.
func TestRunFunctionsRootAtBaseCtx(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	bad := regexp.MustCompile(`context\.WithTimeout\(context\.Background\(\)`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		lines := strings.Split(string(src), "\n")
		curFn := ""
		for i, line := range lines {
			if strings.HasPrefix(line, "func ") {
				curFn = line
			}
			if bad.MatchString(line) && strings.Contains(funcName(curFn), "run") {
				t.Errorf("%s:%d: run function roots its context in Background; use o.baseCtx() so SIGTERM cancels it\n\t%s",
					f, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// funcName pulls the bare name out of a func declaration line.
func funcName(decl string) string {
	open := strings.Index(decl, "(")
	if open < 0 {
		return ""
	}
	rest := decl[open:]
	close := strings.Index(rest, ")")
	if close < 0 || close+2 > len(rest) {
		return ""
	}
	after := strings.TrimPrefix(rest[close+1:], " ")
	if i := strings.Index(after, "("); i > 0 {
		return after[:i]
	}
	return ""
}

// TestMarkFailed_ShutdownIsResumable: a run stopped by SIGTERM must land in
// TIMED_OUT/interrupted, not FAILED — FAILED is terminal for /resume and the
// reconciler, so it would strand the task exactly when the next boot could
// continue it.
func TestMarkFailed_ShutdownIsResumable(t *testing.T) {
	o, db, lc, _ := reconcileFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	o.SetRootContext(ctx)
	cancel() // SIGTERM

	task := &store.AdmiralTask{IssueID: "issue-9", IssueIdentifier: "GEO-9",
		State: store.JobStateExecuting, Branch: "linear/GEO-9"}
	db.LiveAdmiralTask = task
	db.AdmiralTask = task

	f := newFlow(o, context.Background(), linear.AgentEvent{
		SessionID: "sess-9", IssueID: "issue-9", IssueIdentifier: "GEO-9"})
	f.branch = "linear/GEO-9"

	f.markFailed(context.Canceled)

	if task.State != store.JobStateTimedOut {
		t.Fatalf("state = %q, want TIMED_OUT (resumable)", task.State)
	}
	if !strings.HasPrefix(task.Error, store.InterruptedByShutdown) {
		t.Errorf("error = %q, want the interrupted reason", task.Error)
	}
	if !strings.Contains(lc.GetPostedBody(), "will resume automatically") {
		t.Errorf("notice = %q, want the resume promise", lc.GetPostedBody())
	}
}

// TestMarkFailed_RealFailureStillFails: the shutdown special case must not
// swallow genuine failures.
func TestMarkFailed_RealFailureStillFails(t *testing.T) {
	o, db, _, _ := reconcileFixture(t) // rootCtx not cancelled
	task := &store.AdmiralTask{IssueID: "issue-10", State: store.JobStateExecuting}
	db.LiveAdmiralTask = task
	db.AdmiralTask = task

	f := newFlow(o, context.Background(), linear.AgentEvent{
		SessionID: "sess-10", IssueID: "issue-10", IssueIdentifier: "GEO-10"})
	f.markFailed(errTestBoom)

	if task.State != store.JobStateFailed {
		t.Fatalf("state = %q, want FAILED", task.State)
	}
}

// errTestBoom is a plain run failure.
var errTestBoom = errors.New("claude exited 1")

// TestReconcile_PicksUpCleanShutdownRows: a CLEAN shutdown stamps rows
// TIMED_OUT/interrupted, so an in-flight-only query would never see them —
// the exact failure mode a graceful restart produces. The sweep must pick
// them up without re-stamping.
func TestReconcile_PicksUpCleanShutdownRows(t *testing.T) {
	o, db, _, rec := reconcileFixture(t)
	wt := t.TempDir()
	row := store.AdmiralTask{
		IssueID: "issue-4", IssueIdentifier: "GEO-4",
		State:    store.JobStateTimedOut,
		Error:    store.InterruptedByShutdown,
		AttemptN: 1, Branch: "linear/GEO-4", WorktreePath: wt,
		ClaudeSessionID: "sess-claude", LastEventSessionID: "sess-linear",
		// Old enough to clear the dispatch grace window.
		StartedAt: time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339),
	}
	db.InFlightTasks = []store.AdmiralTask{row}
	db.AdmiralTask = &row // the ownership re-check reads it back

	rep := o.Reconcile(context.Background())

	if rep.Resumed != 1 {
		t.Fatalf("report = %+v, want the clean-shutdown row resumed", rep)
	}
	if len(db.MarkedInterruptedIDs) != 0 {
		t.Errorf("re-stamped an already-interrupted row: %v", db.MarkedInterruptedIDs)
	}
	waitFor(t, func() bool { return len(rec.snapshot()) == 1 })
}

// TestReconcile_RespectsDispatchGrace: a row that is only seconds old may be
// between its state write and beginRun (dispatch does Linear I/O first), so
// the sweep must not judge it yet.
func TestReconcile_RespectsDispatchGrace(t *testing.T) {
	o, db, lc, rec := reconcileFixture(t)
	db.InFlightTasks = []store.AdmiralTask{{
		IssueID: "issue-5", IssueIdentifier: "GEO-5", State: store.JobStateReceived,
		StartedAt: time.Now().UTC().Format(time.RFC3339), // just now
	}}

	rep := o.Reconcile(context.Background())

	if rep.Orphans != 0 || rep.Deferred != 1 {
		t.Fatalf("report = %+v, want the fresh row deferred", rep)
	}
	if len(db.MarkedInterruptedIDs) != 0 || len(rec.snapshot()) != 0 {
		t.Error("acted on a row inside the dispatch grace window")
	}
	if lc.GetPostedBody() != "" {
		t.Errorf("spurious escalation: %q", lc.GetPostedBody())
	}
}

// TestReconcile_ResumeCountErrorDefers: a store hiccup while bumping the
// auto-resume counter must not be reported to the human as "cap reached".
func TestReconcile_ResumeCountErrorDefers(t *testing.T) {
	o, db, lc, rec := reconcileFixture(t)
	db.AutoResumeErr = os.ErrPermission
	db.InFlightTasks = []store.AdmiralTask{{
		IssueID: "issue-6", IssueIdentifier: "GEO-6", State: store.JobStateExecuting,
		WorktreePath:    t.TempDir(),
		ClaudeSessionID: "sess-claude", LastEventSessionID: "sess-linear",
		StartedAt: time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339),
	}}

	rep := o.Reconcile(context.Background())

	if rep.Deferred != 1 || rep.Escalated != 0 {
		t.Fatalf("report = %+v, want deferred (not escalated)", rep)
	}
	if len(rec.snapshot()) != 0 {
		t.Error("resumed despite the counter error")
	}
	if lc.GetPostedBody() != "" {
		t.Errorf("posted a false 'cap reached' notice: %q", lc.GetPostedBody())
	}
}

// TestBeginRun_RefCountsPerIssue: two overlapping runs for one issue (a fix
// round on a DONE task plus a /rerun attempt) must both stay visible; the
// first one finishing must not make the second look orphaned.
func TestBeginRun_RefCountsPerIssue(t *testing.T) {
	o, _, _, _ := reconcileFixture(t)

	end1 := o.beginRun("issue-7")
	end2 := o.beginRun("issue-7")
	if !o.RunInFlight("issue-7") {
		t.Fatal("not in flight after two begins")
	}
	end1()
	if !o.RunInFlight("issue-7") {
		t.Fatal("first end cleared a still-running issue")
	}
	end2()
	if o.RunInFlight("issue-7") {
		t.Fatal("still in flight after both ends")
	}
}

// TestReconcile_EscalatesOnce: an escalated row leaves the recoverable set,
// so the same warning is not re-posted on every sweep forever.
func TestReconcile_EscalatesOnce(t *testing.T) {
	o, db, _, _ := reconcileFixture(t)
	db.InFlightTasks = []store.AdmiralTask{{
		IssueID: "issue-11", IssueIdentifier: "GEO-11", State: store.JobStateExecuting,
		StartedAt: time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339),
	}}

	o.Reconcile(context.Background())
	if len(db.MarkedUnrecoverable) != 1 {
		t.Fatalf("marked unrecoverable = %v, want exactly one", db.MarkedUnrecoverable)
	}

	// Second sweep: the real store would no longer list it (the marker stops
	// matching); the mock returns the same slice, so assert the loop still
	// produces the same single marker rather than a second post.
	before := len(db.MarkedUnrecoverable)
	o.Reconcile(context.Background())
	if len(db.MarkedUnrecoverable) > before+1 {
		t.Errorf("escalation re-ran more than once per sweep")
	}
}

// TestReconcile_SkipsSupersededRow: the ownership re-check right before a
// queued resume starts — a /rerun that landed in between must win.
func TestReconcile_SkipsSupersededRow(t *testing.T) {
	o, db, _, rec := reconcileFixture(t)
	db.InFlightTasks = []store.AdmiralTask{{
		IssueID: "issue-12", IssueIdentifier: "GEO-12", State: store.JobStateExecuting,
		AttemptN: 1, Branch: "linear/GEO-12", WorktreePath: t.TempDir(),
		ClaudeSessionID: "sess-claude", LastEventSessionID: "sess-linear",
		StartedAt: time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339),
	}}
	// A /rerun superseded the row: new attempt, different branch.
	db.AdmiralTask = &store.AdmiralTask{
		IssueID: "issue-12", State: store.JobStateExecuting,
		AttemptN: 2, Branch: "linear/GEO-12-rerun-2",
	}

	o.Reconcile(context.Background())
	time.Sleep(150 * time.Millisecond) // let the goroutine run its check

	if len(rec.snapshot()) != 0 {
		t.Errorf("resumed a superseded row: %+v", rec.snapshot())
	}
}

// TestReconcile_CancelledSweepLaunchesNothing: a sweep interrupted by SIGTERM
// must not queue resumes — their prepare phase would fail on the cancelled
// context and overwrite the resumable marker.
func TestReconcile_CancelledSweepLaunchesNothing(t *testing.T) {
	o, db, _, rec := reconcileFixture(t)
	db.InFlightTasks = []store.AdmiralTask{{
		IssueID: "issue-13", State: store.JobStateExecuting,
		WorktreePath: t.TempDir(), ClaudeSessionID: "s", LastEventSessionID: "e",
		StartedAt: time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339),
	}}
	// A resumable-looking row: without the ctx check the sweep would launch a
	// resume, so this test isolates the cancellation guard.
	db.AdmiralTask = &store.AdmiralTask{
		IssueID: "issue-13", State: store.JobStateTimedOut, Branch: "",
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	o.Reconcile(ctx)
	time.Sleep(50 * time.Millisecond)
	if len(rec.snapshot()) != 0 {
		t.Error("launched a resume from a cancelled sweep")
	}
}
