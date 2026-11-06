package autopilot

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/georgehuang/admiral/internal/store"
)

func TestAllFinal(t *testing.T) {
	tests := []struct {
		name string
		runs []checkRun
		want bool
	}{
		{
			name: "empty runs returns false (no checks yet)",
			runs: []checkRun{},
			want: false,
		},
		{
			name: "all completed returns true",
			runs: []checkRun{
				{Name: "test", Status: "completed", Conclusion: "success"},
				{Name: "lint", Status: "completed", Conclusion: "success"},
			},
			want: true,
		},
		{
			name: "one in_progress returns false",
			runs: []checkRun{
				{Name: "test", Status: "completed", Conclusion: "success"},
				{Name: "lint", Status: "in_progress", Conclusion: ""},
			},
			want: false,
		},
		{
			name: "one queued returns false",
			runs: []checkRun{
				{Name: "test", Status: "queued", Conclusion: ""},
			},
			want: false,
		},
		{
			name: "mixed completed and queued returns false",
			runs: []checkRun{
				{Name: "test", Status: "completed", Conclusion: "success"},
				{Name: "lint", Status: "queued", Conclusion: ""},
				{Name: "build", Status: "queued", Conclusion: ""},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := allFinal(tt.runs)
			if got != tt.want {
				t.Errorf("allFinal() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFailedChecks(t *testing.T) {
	tests := []struct {
		name string
		runs []checkRun
		want int
	}{
		{
			name: "no runs returns empty",
			runs: []checkRun{},
			want: 0,
		},
		{
			name: "all success returns empty",
			runs: []checkRun{
				{Name: "test", Status: "completed", Conclusion: "success"},
			},
			want: 0,
		},
		{
			name: "one failure returns one",
			runs: []checkRun{
				{Name: "test", Status: "completed", Conclusion: "failure"},
			},
			want: 1,
		},
		{
			name: "mixed success and failure returns failures",
			runs: []checkRun{
				{Name: "test", Status: "completed", Conclusion: "success"},
				{Name: "lint", Status: "completed", Conclusion: "failure"},
				{Name: "build", Status: "completed", Conclusion: "success"},
			},
			want: 1,
		},
		{
			name: "neutral is not a failure",
			runs: []checkRun{
				{Name: "test", Status: "completed", Conclusion: "neutral"},
			},
			want: 0,
		},
		{
			name: "skipped is not a failure",
			runs: []checkRun{
				{Name: "test", Status: "completed", Conclusion: "skipped"},
			},
			want: 0,
		},
		{
			name: "cancelled is a failure",
			runs: []checkRun{
				{Name: "test", Status: "completed", Conclusion: "cancelled"},
			},
			want: 1,
		},
		{
			name: "in_progress is not a failure (not final)",
			runs: []checkRun{
				{Name: "test", Status: "in_progress", Conclusion: ""},
			},
			want: 0,
		},
		{
			name: "multiple failures",
			runs: []checkRun{
				{Name: "test", Status: "completed", Conclusion: "failure"},
				{Name: "lint", Status: "completed", Conclusion: "failure"},
			},
			want: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := failedChecks(tt.runs)
			if len(got) != tt.want {
				t.Errorf("failedChecks() returned %d, want %d", len(got), tt.want)
			}
		})
	}
}

func TestCheckNamesList(t *testing.T) {
	tests := []struct {
		name string
		runs []checkRun
		want string
	}{
		{
			name: "empty returns empty string",
			runs: []checkRun{},
			want: "",
		},
		{
			name: "single check",
			runs: []checkRun{
				{Name: "test"},
			},
			want: "test",
		},
		{
			name: "multiple checks unique names",
			runs: []checkRun{
				{Name: "test"},
				{Name: "lint"},
				{Name: "build"},
			},
			want: "test, lint, build",
		},
		{
			name: "duplicate names deduplicated",
			runs: []checkRun{
				{Name: "test"},
				{Name: "test"},
				{Name: "lint"},
			},
			want: "test, lint",
		},
		{
			name: "order preserved first-seen",
			runs: []checkRun{
				{Name: "lint"},
				{Name: "test"},
				{Name: "lint"}, // duplicate, should not change order
			},
			want: "lint, test",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := checkNamesList(tt.runs)
			if got != tt.want {
				t.Errorf("checkNamesList() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractPRNumberFromURL(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		want    int
		wantErr bool
	}{
		{
			name:    "standard github PR URL",
			url:     "https://github.com/owner/repo/pull/123",
			want:    123,
			wantErr: false,
		},
		{
			name:    "PR URL with files suffix",
			url:     "https://github.com/owner/repo/pull/456/files",
			want:    456,
			wantErr: false,
		},
		{
			name:    "PR URL with conversation suffix",
			url:     "https://github.com/owner/repo/pull/789/conversations",
			want:    789,
			wantErr: false,
		},
		{
			name:    "PR URL with extra path segments after number",
			url:     "https://github.com/owner/repo/pull/100/checks",
			want:    100,
			wantErr: false,
		},
		{
			name:    "invalid URL - not a PR",
			url:     "https://github.com/owner/repo/issues/1",
			want:    0,
			wantErr: true,
		},
		{
			name:    "invalid URL - empty",
			url:     "",
			want:    0,
			wantErr: true,
		},
		{
			name:    "invalid URL - no host",
			url:     "/pull/123",
			want:    0,
			wantErr: true,
		},
		{
			name:    "invalid URL - no PR segment",
			url:     "https://github.com/owner/repo",
			want:    0,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := extractPRNumberFromURL(tt.url)
			if (err != nil) != tt.wantErr {
				t.Errorf("extractPRNumberFromURL() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("extractPRNumberFromURL() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseInt(t *testing.T) {
	tests := []struct {
		name string
		s    string
		want int
		ok   bool
	}{
		{"simple number", "123", 123, true},
		{"zero not valid for PR numbers", "0", 0, false},
		{"single digit", "5", 5, true},
		{"empty string", "", 0, false},
		{"non-numeric", "abc", 0, false},
		{"mixed", "12abc", 0, false},
		{"leading zeros", "007", 7, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseInt(tt.s)
			if ok != tt.ok {
				t.Errorf("parseInt() ok = %v, want %v", ok, tt.ok)
			}
			if got != tt.want {
				t.Errorf("parseInt() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNewCIWatcher(t *testing.T) {
	w := newCIWatcher(nil, nil, nil, "gh", 30*time.Second, 15*time.Minute, 2)
	if w == nil {
		t.Fatal("newCIWatcher returned nil")
	}
	if w.pollInter != 30*time.Second {
		t.Errorf("pollInter = %v, want 30s", w.pollInter)
	}
	if w.timeout != 15*time.Minute {
		t.Errorf("timeout = %v, want 15m", w.timeout)
	}
	if w.ghBin != "gh" {
		t.Errorf("ghBin = %q, want gh", w.ghBin)
	}
}

// TestWatchPR_PollsAfterCallerCancels pins the watcher's lifetime contract:
// the caller's ctx belongs to the run that opened the PR and is cancelled as
// soon as that run returns — CI watching must survive that cancellation and
// still deliver the check result. Red-check: remove the WithoutCancel in
// WatchPR and this test fails (the goroutine dies on ctx.Done before its
// first tick, nothing is ever posted).
func TestWatchPR_PollsAfterCallerCancels(t *testing.T) {
	dir := t.TempDir()
	ghBin := filepath.Join(dir, "fake-gh")
	script := "#!/bin/sh\n" +
		`echo '[{"name":"test","status":"completed","conclusion":"failure","url":"https://ci.example.com/run/1"}]'` + "\n"
	if err := os.WriteFile(ghBin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}

	lc := &mockLinearClient{}
	db := &mockStore{
		AdmiralTask: &store.AdmiralTask{
			IssueID: "issue-1", State: store.JobStateDone,
			PRURL: "https://github.com/acme/repo/pull/7",
		},
	}
	db.LiveAdmiralTask = db.AdmiralTask
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	w := newCIWatcher(lc, db, logger, ghBin, 10*time.Millisecond, 5*time.Second, 2)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the run is over before watching even starts — exactly the production shape
	w.WatchPR(ctx, "https://github.com/acme/repo/pull/7", dir, "sess-1", "issue-1", defaultTestGhUser)

	deadline := time.Now().Add(3 * time.Second)
	for {
		lc.PostedBodyMu.Lock()
		posted := lc.PostedBody
		lc.PostedBodyMu.Unlock()
		if posted != "" {
			if !strings.Contains(posted, "CI failed") {
				t.Errorf("posted body = %q, want a CI failure notice", posted)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("watcher never polled: no CI result posted after caller cancelled its ctx")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ciWatchFakeGh writes a fake gh binary whose check-run JSON depends on how
// many times `gh pr checks` has been invoked: the first calls (before
// fixRounds) return the given failure JSON, later calls return all-success.
// `gh auth token` calls (made by Accounts.EnvFor with the same binary) are
// answered separately and not counted. Returns the binary path and a
// function reading back the checks-invocation count.
func ciWatchFakeGh(t *testing.T, firstRuns int) (string, func() int) {
	t.Helper()
	dir := t.TempDir()
	ghBin := filepath.Join(dir, "fake-gh")
	cnt := filepath.Join(dir, "calls")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" != \"pr\" ]; then echo fake-token; exit 0; fi\n" +
		// pr view → the head SHA the check results belong to. Tests that
		// need the head to move write a `sha` file next to this script.
		"if [ \"$2\" = \"view\" ]; then s=$(cat \"$(dirname \"$0\")/sha\" 2>/dev/null); [ -n \"$s\" ] || s=sha-const; printf '{\"headRefOid\":\"%s\"}\\n' \"$s\"; exit 0; fi\n" +
		fmt.Sprintf("n=$(cat %s 2>/dev/null || echo 0); n=$((n+1)); echo $n > %s\n", cnt, cnt) +
		fmt.Sprintf("if [ $n -le %d ]; then\n", firstRuns) +
		`  echo '[{"name":"test","status":"completed","conclusion":"failure","url":"https://ci.example.com/run/1"}]'` + "\n" +
		"else\n" +
		`  echo '[{"name":"test","status":"completed","conclusion":"success"}]'` + "\n" +
		"fi\n"
	if err := os.WriteFile(ghBin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	count := func() int {
		b, err := os.ReadFile(cnt)
		if err != nil {
			return 0
		}
		n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		return n
	}
	return ghBin, count
}

// ciWatchTaskFailure polls the mock's posted body until it contains want
// (or times out), returning the last body seen.
func ciWatchTaskFailure(t *testing.T, lc *mockLinearClient, want string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		lc.PostedBodyMu.Lock()
		posted := lc.PostedBody
		lc.PostedBodyMu.Unlock()
		if strings.Contains(posted, want) {
			return posted
		}
		if time.Now().After(deadline) {
			t.Fatalf("no Linear post containing %q within deadline; last: %q", want, posted)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func ciWatchFixFixture(ghBin string, maxFixRounds int) (*CIWatcher, *mockLinearClient, *mockStore, *int) {
	lc := &mockLinearClient{}
	db := &mockStore{
		AdmiralTask: &store.AdmiralTask{
			IssueID:         "issue-1",
			IssueIdentifier: "TEST-1",
			State:           store.JobStateDone,
			Branch:          "linear/TEST-1",
			PRURL:           "https://github.com/acme/repo/pull/7",
			WorktreePath:    "/tmp/wt",
		},
	}
	db.LiveAdmiralTask = db.AdmiralTask
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	w := newCIWatcher(lc, db, logger, ghBin, 10*time.Millisecond, 5*time.Second, maxFixRounds)
	fixCalls := 0
	w.fixRound = func(_ context.Context, _ *store.AdmiralTask, _ string) error {
		fixCalls++
		return nil
	}
	return w, lc, db, &fixCalls
}

// TestWatchPR_CIFailureTriggersFixThenGreen: a failing first poll runs one
// fix round, the fix push makes CI re-run green, and the Linear thread gets
// the success notice with the fix-round count. The task row must NOT be
// marked FAILED anywhere on this path.
func TestWatchPR_CIFailureTriggersFixThenGreen(t *testing.T) {
	ghBin, ghCalls := ciWatchFakeGh(t, 1)
	w, lc, db, fixCalls := ciWatchFixFixture(ghBin, 2)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.WatchPR(ctx, db.AdmiralTask.PRURL, t.TempDir(), "sess-1", db.AdmiralTask.IssueID, defaultTestGhUser)

	body := ciWatchTaskFailure(t, lc, "✅ All CI checks passed")
	if !strings.Contains(body, "1 automatic fix round") {
		t.Errorf("success notice missing fix-round count: %q", body)
	}
	if *fixCalls != 1 {
		t.Errorf("fixRound called %d times, want 1", *fixCalls)
	}
	if ghCalls() < 2 {
		t.Errorf("gh polled %d times, want >= 2 (re-poll after fix)", ghCalls())
	}
	db.LiveAdmiralTask = db.AdmiralTask // LiveAdmiralTask points at the same struct; re-assert below
	if state, _ := taskStateSnapshot(db); state != store.JobStateDone {
		t.Errorf("task state = %q, want DONE (fix succeeded, no FAILED marker)", state)
	}
}

// TestWatchPR_CIFixCapEscalates: when CI keeps failing, the watcher runs
// exactly maxFixRounds fix rounds, then escalates to the human (Linear
// notice with a /fix pointer) and marks the task FAILED ci_failed. This is
// the anti-infinite-loop contract.
func TestWatchPR_CIFixCapEscalates(t *testing.T) {
	ghBin := filepath.Join(t.TempDir(), "fake-gh")
	script := "#!/bin/sh\n" +
		`echo '[{"name":"test","status":"completed","conclusion":"failure","url":"https://ci.example.com/run/1"}]'` + "\n"
	if err := os.WriteFile(ghBin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	w, lc, db, fixCalls := ciWatchFixFixture(ghBin, 2)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.WatchPR(ctx, db.AdmiralTask.PRURL, t.TempDir(), "sess-1", db.AdmiralTask.IssueID, defaultTestGhUser)

	ciWatchTaskFailure(t, lc, "attempted 2 automatic fix round(s) without success")
	body := ciWatchTaskFailure(t, lc, "use `/rerun` on the issue")
	if !strings.Contains(body, "attempted 2 automatic fix round(s) without success") {
		t.Errorf("escalation notice lost the round count: %q", body)
	}
	if *fixCalls != 2 {
		t.Errorf("fixRound called %d times, want exactly 2 (cap enforced)", *fixCalls)
	}
	if state, errMsg := taskStateSnapshot(db); state != store.JobStateFailed || errMsg != "ci_failed" {
		t.Errorf("task = %q/%q, want FAILED/ci_failed", state, errMsg)
	}
}

// TestWatchPR_ReportOnlyWhenNoFixRound: with fixRound nil the watcher keeps
// GEO-54 behavior — one failure notice, immediate FAILED ci_failed, no fix
// attempts.
func TestWatchPR_ReportOnlyWhenNoFixRound(t *testing.T) {
	ghBin := filepath.Join(t.TempDir(), "fake-gh")
	script := "#!/bin/sh\n" +
		`echo '[{"name":"test","status":"completed","conclusion":"failure","url":"https://ci.example.com/run/1"}]'` + "\n"
	if err := os.WriteFile(ghBin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	lc := &mockLinearClient{}
	db := &mockStore{
		AdmiralTask: &store.AdmiralTask{
			IssueID: "issue-1", State: store.JobStateDone,
			PRURL: "https://github.com/acme/repo/pull/7",
		},
	}
	db.LiveAdmiralTask = db.AdmiralTask
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	w := newCIWatcher(lc, db, logger, ghBin, 10*time.Millisecond, 5*time.Second, 2)
	// w.fixRound deliberately left nil — the report-only mode.

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.WatchPR(ctx, db.AdmiralTask.PRURL, t.TempDir(), "sess-1", db.AdmiralTask.IssueID, defaultTestGhUser)

	ciWatchTaskFailure(t, lc, "Automatic CI fixing is disabled")
	if state, errMsg := taskStateSnapshot(db); state != store.JobStateFailed || errMsg != "ci_failed" {
		t.Errorf("task = %q/%q, want FAILED/ci_failed", state, errMsg)
	}
}

// TestWatchPR_TerminalTaskNotFixed: a PR whose task already reached a
// terminal state (merged, cancelled, FAILED) gets a report but never a fix
// round and no row mutation.
func TestWatchPR_TerminalTaskNotFixed(t *testing.T) {
	ghBin := filepath.Join(t.TempDir(), "fake-gh")
	script := "#!/bin/sh\n" +
		`echo '[{"name":"test","status":"completed","conclusion":"failure","url":"https://ci.example.com/run/1"}]'` + "\n"
	if err := os.WriteFile(ghBin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	w, lc, db, fixCalls := ciWatchFixFixture(ghBin, 2)
	db.AdmiralTask.State = store.JobStateDoneMerged

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.WatchPR(ctx, db.AdmiralTask.PRURL, t.TempDir(), "sess-1", db.AdmiralTask.IssueID, defaultTestGhUser)

	ciWatchTaskFailure(t, lc, "not attempting a fix")
	if *fixCalls != 0 {
		t.Errorf("fixRound called %d times, want 0 (terminal task)", *fixCalls)
	}
	if db.AdmiralTask.State != store.JobStateDoneMerged {
		t.Errorf("terminal task state mutated to %q", db.AdmiralTask.State)
	}
}

// TestBuildCIFixPrompt: the prompt names the PR, branch, failing checks and
// forbids opening a new PR; the skill prefix follows the review convention.
func TestBuildCIFixPrompt(t *testing.T) {
	p := buildCIFixPrompt("", "https://github.com/acme/repo/pull/7", "linear/TEST-1", "main",
		"- test (failure)\n\nLogs: https://ci.example.com/run/1")
	for _, want := range []string{
		"https://github.com/acme/repo/pull/7",
		"linear/TEST-1",
		"test (failure)",
		"Do NOT open a new PR",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q:\n%s", want, p)
		}
	}
	if strings.HasPrefix(p, "/") {
		t.Errorf("empty skill must not produce a skill prefix: %q", p[:20])
	}
	withSkill := buildCIFixPrompt("some-skill", "u", "b", "main", "f")
	if !strings.HasPrefix(withSkill, "/some-skill\n\n") {
		t.Errorf("skill prefix missing: %q", withSkill[:30])
	}
}

// taskStateSnapshot reads the task row under the mock's lock — the watcher
// goroutine mutates the same struct via UpdateAdmiralTask, so a direct read
// from the test goroutine races.
func taskStateSnapshot(db *mockStore) (state, errMsg string) {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.AdmiralTask.State, db.AdmiralTask.Error
}

// TestPollUntilFinal_StopsOnPersistentFetchErrors: the watcher is detached
// from the run ctx (no cancel will arrive), so a persistent fetch failure
// — expired token, removed gh binary — must still end the poll via the
// per-round deadline, not loop forever.
func TestPollUntilFinal_StopsOnPersistentFetchErrors(t *testing.T) {
	dir := t.TempDir()
	ghBin := filepath.Join(dir, "fake-gh")
	if err := os.WriteFile(ghBin, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	w := newCIWatcher(&mockLinearClient{}, &mockStore{}, logger, ghBin, 10*time.Millisecond, 150*time.Millisecond, 2)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	runs, sha, ok := w.pollUntilFinal(ctx, dir, 7, defaultTestGhUser, "https://github.com/acme/repo/pull/7")
	if ok {
		t.Fatalf("pollUntilFinal reported success with a permanently failing gh: %v / %q", runs, sha)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("pollUntilFinal took %v to give up, want ~deadline (150ms); the deadline check is not covering the fetch-error path", elapsed)
	}
}

// TestWatchPR_TimeoutAfterFixPostsNotice: when the re-run CI after a fix
// push never concludes, the watcher must not leave the thread dangling on
// "attempting an automatic fix" — it posts a no-verdict notice. Round 0
// timeouts (no fix announced) stay silent, preserving GEO-54 behavior for
// checkless PRs.
func TestWatchPR_TimeoutAfterFixPostsNotice(t *testing.T) {
	dir := t.TempDir()
	ghBin := filepath.Join(dir, "fake-gh")
	cnt := filepath.Join(dir, "calls")
	// First checks call: completed+failure (triggers round 1). Later calls:
	// still in_progress, forever — the re-run never concludes.
	script := "#!/bin/sh\n" +
		"if [ \"$1\" != \"pr\" ]; then echo fake-token; exit 0; fi\n" +
		// pr view → the head SHA the check results belong to. Tests that
		// need the head to move write a `sha` file next to this script.
		"if [ \"$2\" = \"view\" ]; then s=$(cat \"$(dirname \"$0\")/sha\" 2>/dev/null); [ -n \"$s\" ] || s=sha-const; printf '{\"headRefOid\":\"%s\"}\\n' \"$s\"; exit 0; fi\n" +
		fmt.Sprintf("n=$(cat %s 2>/dev/null || echo 0); n=$((n+1)); echo $n > %s\n", cnt, cnt) +
		"if [ $n -le 1 ]; then\n" +
		`  echo '[{"name":"test","status":"completed","conclusion":"failure","url":"https://ci.example.com/run/1"}]'` + "\n" +
		"else\n" +
		`  echo '[{"name":"test","status":"in_progress","conclusion":""}]'` + "\n" +
		"fi\n"
	if err := os.WriteFile(ghBin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	lc := &mockLinearClient{}
	db := &mockStore{
		AdmiralTask: &store.AdmiralTask{
			IssueID: "issue-1", State: store.JobStateDone,
			PRURL: "https://github.com/acme/repo/pull/7",
		},
	}
	db.LiveAdmiralTask = db.AdmiralTask
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// Per-round deadline 150ms; the post-fix settle (one pollInter) is 10ms.
	w := newCIWatcher(lc, db, logger, ghBin, 10*time.Millisecond, 150*time.Millisecond, 2)
	w.fixRound = func(_ context.Context, _ *store.AdmiralTask, _ string) error { return nil }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.WatchPR(ctx, db.AdmiralTask.PRURL, dir, "sess-1", db.AdmiralTask.IssueID, defaultTestGhUser)

	ciWatchTaskFailure(t, lc, "did not reach a conclusion")
	if state, errMsg := taskStateSnapshot(db); state != store.JobStateDone || errMsg != "" {
		t.Errorf("timeout is not a failure: task = %q/%q, want untouched DONE", state, errMsg)
	}
}

// TestEscalateCI_RespectsTerminalTask: the row can go terminal while a fix
// round is running (PR merged mid-round); escalateCI must not clobber it.
func TestEscalateCI_RespectsTerminalTask(t *testing.T) {
	db := &mockStore{
		AdmiralTask: &store.AdmiralTask{
			IssueID: "issue-1", State: store.JobStateDoneMerged,
		},
	}
	db.LiveAdmiralTask = db.AdmiralTask
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	w := newCIWatcher(&mockLinearClient{}, db, logger, "gh", time.Second, time.Minute, 2)

	w.escalateCI(context.Background(), "escalation message", "sess-1", db.AdmiralTask.IssueID, "https://github.com/acme/repo/pull/7")

	if state, errMsg := taskStateSnapshot(db); state != store.JobStateDoneMerged || errMsg != "" {
		t.Errorf("terminal row clobbered: task = %q/%q, want DONE_MERGED untouched", state, errMsg)
	}
}

// TestWatchPR_StaleTaskRowNotActedOn: the row may have been superseded by a
// /rerun (it now points at a different attempt's PR). A watcher for the OLD
// PR must not run fix rounds against, post to, or mark FAILED the live row
// of the new attempt.
func TestWatchPR_StaleTaskRowNotActedOn(t *testing.T) {
	ghBin := filepath.Join(t.TempDir(), "fake-gh")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" != \"pr\" ]; then echo fake-token; exit 0; fi\n" +
		// pr view → the head SHA the check results belong to. Tests that
		// need the head to move write a `sha` file next to this script.
		"if [ \"$2\" = \"view\" ]; then s=$(cat \"$(dirname \"$0\")/sha\" 2>/dev/null); [ -n \"$s\" ] || s=sha-const; printf '{\"headRefOid\":\"%s\"}\\n' \"$s\"; exit 0; fi\n" +
		`echo '[{"name":"test","status":"completed","conclusion":"failure","url":"https://ci.example.com/run/1"}]'` + "\n"
	if err := os.WriteFile(ghBin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	w, lc, db, fixCalls := ciWatchFixFixture(ghBin, 2)
	// The row was reused by a /rerun: it now points at a different PR.
	db.AdmiralTask.PRURL = "https://github.com/acme/repo/pull/99"
	db.AdmiralTask.State = store.JobStateExecuting

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.WatchPR(ctx, "https://github.com/acme/repo/pull/7", t.TempDir(), "sess-1", db.AdmiralTask.IssueID, defaultTestGhUser)

	// Nothing must EVER be posted or fixed for a stale watcher — keep
	// asserting over a window several poll intervals wide (a fixed short
	// sleep can miss the first poll on a loaded machine).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		lc.PostedBodyMu.Lock()
		posted := lc.PostedBody
		lc.PostedBodyMu.Unlock()
		if posted != "" {
			t.Fatalf("stale watcher posted to the thread: %q", posted)
		}
		if *fixCalls != 0 {
			t.Fatalf("stale watcher ran fixRound %d times, want 0", *fixCalls)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if state, errMsg := taskStateSnapshot(db); state != store.JobStateExecuting || errMsg != "" {
		t.Errorf("stale watcher mutated the live row: %q/%q", state, errMsg)
	}
}

// TestFetchTimeout_KillsHungGh: the watcher is detached from the run ctx,
// and exec only kills the child on ctx cancellation — so each `gh pr
// checks` invocation needs its own deadline or a hung gh blocks the
// watcher (and its child process) forever.
func TestFetchTimeout_KillsHungGh(t *testing.T) {
	dir := t.TempDir()
	ghBin := filepath.Join(dir, "fake-gh")
	// `exec` replaces sh with sleep so the ctx kill targets the process
	// holding the stdout pipe; a plain `sleep` child would orphan-hold the
	// pipe and Run would keep waiting even after sh dies.
	if err := os.WriteFile(ghBin, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	w := newCIWatcher(&mockLinearClient{}, &mockStore{}, logger, ghBin, time.Second, time.Minute, 2)
	w.fetchTimeout = 100 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	if _, _, err := w.fetchCheckRuns(ctx, dir, 7, defaultTestGhUser); err == nil {
		t.Fatal("fetchCheckRuns succeeded against a hung gh, want deadline error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("fetchCheckRuns took %v against a hung gh, want ~fetchTimeout (100ms)", elapsed)
	}
}

// TestLockPRFix_SerializesSameBranch: the runPRFix callers must not share a
// worktree — the second lock acquisition for the same branch blocks until
// the first round releases; a different branch is independent.
func TestLockPRFix_SerializesSameBranch(t *testing.T) {
	o := &Orchestrator{}
	unlock := o.lockPRFix("linear/TEST-1")

	acquired := make(chan struct{})
	go func() {
		u := o.lockPRFix("linear/TEST-1")
		close(acquired)
		u()
	}()

	select {
	case <-acquired:
		t.Fatal("second lockPRFix for the same branch acquired while the first is held")
	case <-time.After(50 * time.Millisecond):
	}

	unlock()
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("second lockPRFix never acquired after release")
	}

	u2 := o.lockPRFix("linear/OTHER")
	if u2 == nil {
		t.Fatal("different branch should lock independently")
	}
	u2()
}

// TestEscalateCI_RespectsSupersededRow: the write-side twin of the loop's
// stale guard — a /rerun can reuse the row (new attempt, different PR,
// non-terminal) WHILE a fix round or escalation is in flight; the mutation
// must re-validate PRURL at write time, not just at loop start.
func TestEscalateCI_RespectsSupersededRow(t *testing.T) {
	db := &mockStore{
		AdmiralTask: &store.AdmiralTask{
			IssueID: "issue-1", State: store.JobStateExecuting,
			PRURL: "https://github.com/acme/repo/pull/9", // reused by /rerun
		},
	}
	db.LiveAdmiralTask = db.AdmiralTask
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	w := newCIWatcher(&mockLinearClient{}, db, logger, "gh", time.Second, time.Minute, 2)

	w.escalateCI(context.Background(), "escalation message", "sess-1", db.AdmiralTask.IssueID,
		"https://github.com/acme/repo/pull/7") // this watch's PR

	if state, errMsg := taskStateSnapshot(db); state != store.JobStateExecuting || errMsg != "" {
		t.Errorf("superseded row clobbered: %q/%q, want EXECUTING untouched", state, errMsg)
	}
}

// TestPRWatchSlot_SuppressesWhileActive pins the dedup contract directly:
// a second watcher cannot own a PR its peer already owns, and takes over
// as soon as that peer releases. (The earlier version of this test drove
// two real WatchPR calls; the first finished inside the wait window, so it
// silently exercised handoff rather than suppression and raced on the
// fix-round count.)
func TestPRWatchSlot_SuppressesWhileActive(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	w := newCIWatcher(&mockLinearClient{}, &mockStore{}, logger, "gh", time.Second, time.Minute, 2)
	const pr = "https://github.com/acme/repo/pull/7"

	if !w.acquireSlot(pr, 0) {
		t.Fatal("first acquire failed on a free PR")
	}
	if w.acquireSlot(pr, 150*time.Millisecond) {
		t.Fatal("second acquire succeeded while a watcher owns the PR")
	}
	w.releaseSlot(pr)
	if !w.acquireSlot(pr, time.Second) {
		t.Fatal("acquire failed after the owner released")
	}
	w.releaseSlot(pr)

	// A different PR is independent.
	if !w.acquireSlot("https://github.com/acme/repo/pull/8", 0) {
		t.Fatal("independent PR should not be gated")
	}
}

// TestWatchPR_RunInFlightNotFixed: a /fix or /rerun execution owns the row
// and the worktree right now — the watcher must not race it with an
// automatic round, and must not mark the EXECUTING row FAILED.
func TestWatchPR_RunInFlightNotFixed(t *testing.T) {
	ghBin := filepath.Join(t.TempDir(), "fake-gh")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" != \"pr\" ]; then echo fake-token; exit 0; fi\n" +
		// pr view → the head SHA the check results belong to. Tests that
		// need the head to move write a `sha` file next to this script.
		"if [ \"$2\" = \"view\" ]; then s=$(cat \"$(dirname \"$0\")/sha\" 2>/dev/null); [ -n \"$s\" ] || s=sha-const; printf '{\"headRefOid\":\"%s\"}\\n' \"$s\"; exit 0; fi\n" +
		`echo '[{"name":"test","status":"completed","conclusion":"failure","url":"https://ci.example.com/run/1"}]'` + "\n"
	if err := os.WriteFile(ghBin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	w, lc, db, fixCalls := ciWatchFixFixture(ghBin, 2)
	db.AdmiralTask.State = store.JobStateExecuting // /fix run in flight, same PR

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.WatchPR(ctx, db.AdmiralTask.PRURL, t.TempDir(), "sess-1", db.AdmiralTask.IssueID, defaultTestGhUser)

	ciWatchTaskFailure(t, lc, "a fix run is already in progress")
	if *fixCalls != 0 {
		t.Errorf("fixRound called %d times, want 0 (run in flight)", *fixCalls)
	}
	if state, errMsg := taskStateSnapshot(db); state != store.JobStateExecuting || errMsg != "" {
		t.Errorf("in-flight row clobbered: %q/%q, want EXECUTING untouched", state, errMsg)
	}
}

// TestWatchPR_DuplicateWaitThenWatch: a previous watcher finishing its
// final post must not leave the new attempt unreported — the new WatchPR
// waits for release and then watches (only a still-held claim after
// ciClaimWait suppresses).
func TestWatchPR_DuplicateWaitThenWatch(t *testing.T) {
	ghBin := filepath.Join(t.TempDir(), "fake-gh")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" != \"pr\" ]; then echo fake-token; exit 0; fi\n" +
		// pr view → the head SHA the check results belong to. Tests that
		// need the head to move write a `sha` file next to this script.
		"if [ \"$2\" = \"view\" ]; then s=$(cat \"$(dirname \"$0\")/sha\" 2>/dev/null); [ -n \"$s\" ] || s=sha-const; printf '{\"headRefOid\":\"%s\"}\\n' \"$s\"; exit 0; fi\n" +
		`echo '[{"name":"test","status":"completed","conclusion":"failure","url":"https://ci.example.com/run/1"}]'` + "\n"
	if err := os.WriteFile(ghBin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	w, lc, db, _ := ciWatchFixFixture(ghBin, 2)
	w.fixRound = nil // report-only: the assertion keys on the disabled-fix escalation
	oldWait := ciClaimWait
	ciClaimWait = 3 * time.Second
	t.Cleanup(func() { ciClaimWait = oldWait })

	// A finishing watcher owns the slot, releases after 300ms.
	if !w.acquireSlot(db.AdmiralTask.PRURL, 0) {
		t.Fatal("initial claim failed")
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		w.releaseSlot(db.AdmiralTask.PRURL)
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.WatchPR(ctx, db.AdmiralTask.PRURL, t.TempDir(), "sess-1", db.AdmiralTask.IssueID, defaultTestGhUser)

	ciWatchTaskFailure(t, lc, "Automatic CI fixing is disabled")
}

// TestWatchPR_TransientLookupRetried: one transient read failure at
// failure-detection time must not leave the row DONE under red CI — the
// watcher retries once and then records ci_failed normally.
func TestWatchPR_TransientLookupRetried(t *testing.T) {
	ghBin := filepath.Join(t.TempDir(), "fake-gh")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" != \"pr\" ]; then echo fake-token; exit 0; fi\n" +
		// pr view → the head SHA the check results belong to. Tests that
		// need the head to move write a `sha` file next to this script.
		"if [ \"$2\" = \"view\" ]; then s=$(cat \"$(dirname \"$0\")/sha\" 2>/dev/null); [ -n \"$s\" ] || s=sha-const; printf '{\"headRefOid\":\"%s\"}\\n' \"$s\"; exit 0; fi\n" +
		`echo '[{"name":"test","status":"completed","conclusion":"failure","url":"https://ci.example.com/run/1"}]'` + "\n"
	if err := os.WriteFile(ghBin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	w, lc, db, _ := ciWatchFixFixture(ghBin, 2)
	w.fixRound = nil // report-only: assert the ci_failed marker lands despite the transient
	db.AdmiralTaskErrOnce = true

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.WatchPR(ctx, db.AdmiralTask.PRURL, t.TempDir(), "sess-1", db.AdmiralTask.IssueID, defaultTestGhUser)

	ciWatchTaskFailure(t, lc, "Automatic CI fixing is disabled")
	if state, errMsg := taskStateSnapshot(db); state != store.JobStateFailed || errMsg != "ci_failed" {
		t.Errorf("task = %q/%q after transient lookup failure, want FAILED/ci_failed", state, errMsg)
	}
}

// TestWatchPR_AwaitingInputNotFixed: a parked ask_user run owns the row and
// the worktree — the watcher must not reset the worktree with an automatic
// round, and must not flip the row FAILED (the user's pending answer would
// bounce).
func TestWatchPR_AwaitingInputNotFixed(t *testing.T) {
	ghBin := filepath.Join(t.TempDir(), "fake-gh")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" != \"pr\" ]; then echo fake-token; exit 0; fi\n" +
		// pr view → the head SHA the check results belong to. Tests that
		// need the head to move write a `sha` file next to this script.
		"if [ \"$2\" = \"view\" ]; then s=$(cat \"$(dirname \"$0\")/sha\" 2>/dev/null); [ -n \"$s\" ] || s=sha-const; printf '{\"headRefOid\":\"%s\"}\\n' \"$s\"; exit 0; fi\n" +
		`echo '[{"name":"test","status":"completed","conclusion":"failure","url":"https://ci.example.com/run/1"}]'` + "\n"
	if err := os.WriteFile(ghBin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	w, lc, db, fixCalls := ciWatchFixFixture(ghBin, 2)
	db.AdmiralTask.State = store.JobStateAwaitingInput

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.WatchPR(ctx, db.AdmiralTask.PRURL, t.TempDir(), "sess-1", db.AdmiralTask.IssueID, defaultTestGhUser)

	ciWatchTaskFailure(t, lc, "a fix run is already in progress")
	if *fixCalls != 0 {
		t.Errorf("fixRound called %d times, want 0 (AWAITING_INPUT)", *fixCalls)
	}
	if state, errMsg := taskStateSnapshot(db); state != store.JobStateAwaitingInput || errMsg != "" {
		t.Errorf("parked row clobbered: %q/%q, want AWAITING_INPUT untouched", state, errMsg)
	}
}

// TestWatchPR_StaleHeadReObservedBeforeSpending: the first failure
// observation after a fix push can still be the OLD head's completed
// failing checks — it must be re-observed once (uncounted) before a fix
// round is spent, so the round count reflects the new head's CI.
func TestWatchPR_StaleHeadReObservedBeforeSpending(t *testing.T) {
	dir := t.TempDir()
	ghBin := filepath.Join(dir, "fake-gh")
	cnt := filepath.Join(dir, "calls")
	// checks call 1: failure (triggers fix round 1). call 2: stale failure
	// from the old head. call 3+: the fix landed — green.
	script := "#!/bin/sh\n" +
		"if [ \"$1\" != \"pr\" ]; then echo fake-token; exit 0; fi\n" +
		// pr view → the head SHA the check results belong to. Tests that
		// need the head to move write a `sha` file next to this script.
		"if [ \"$2\" = \"view\" ]; then s=$(cat \"$(dirname \"$0\")/sha\" 2>/dev/null); [ -n \"$s\" ] || s=sha-const; printf '{\"headRefOid\":\"%s\"}\\n' \"$s\"; exit 0; fi\n" +
		fmt.Sprintf("n=$(cat %s 2>/dev/null || echo 0); n=$((n+1)); echo $n > %s\n", cnt, cnt) +
		"if [ $n -le 1 ]; then\n" +
		`  echo '[{"name":"test","status":"completed","conclusion":"failure","url":"https://ci.example.com/run/1"}]'` + "\n" +
		"elif [ $n -eq 2 ]; then\n" +
		`  echo '[{"name":"test","status":"completed","conclusion":"failure","url":"https://ci.example.com/run/1"}]'` + "\n" +
		"else\n" +
		`  echo '[{"name":"test","status":"completed","conclusion":"success"}]'` + "\n" +
		"fi\n"
	if err := os.WriteFile(ghBin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	w, lc, db, fixCalls := ciWatchFixFixture(ghBin, 2)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.WatchPR(ctx, db.AdmiralTask.PRURL, dir, "sess-1", db.AdmiralTask.IssueID, defaultTestGhUser)

	body := ciWatchTaskFailure(t, lc, "✅ All CI checks passed")
	if !strings.Contains(body, "green after 1 automatic fix round") {
		t.Errorf("stale observation consumed a round: %q", body)
	}
	if *fixCalls != 1 {
		t.Errorf("fixRound called %d times, want 1 (stale head not acted on)", *fixCalls)
	}
}

// TestWatchPR_SuppressedSessionGetsPointer: when this attempt's watcher is
// suppressed (another one is already on the PR), its session thread is told
// where the CI results land instead of staying silent.
func TestWatchPR_SuppressedSessionGetsPointer(t *testing.T) {
	ghBin := filepath.Join(t.TempDir(), "fake-gh")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" != \"pr\" ]; then echo fake-token; exit 0; fi\n" +
		// pr view → the head SHA the check results belong to. Tests that
		// need the head to move write a `sha` file next to this script.
		"if [ \"$2\" = \"view\" ]; then s=$(cat \"$(dirname \"$0\")/sha\" 2>/dev/null); [ -n \"$s\" ] || s=sha-const; printf '{\"headRefOid\":\"%s\"}\\n' \"$s\"; exit 0; fi\n" +
		`echo '[{"name":"test","status":"completed","conclusion":"failure","url":"https://ci.example.com/run/1"}]'` + "\n"
	if err := os.WriteFile(ghBin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	w, lc, db, _ := ciWatchFixFixture(ghBin, 2)
	oldWait := ciClaimWait
	ciClaimWait = 300 * time.Millisecond
	t.Cleanup(func() { ciClaimWait = oldWait })

	// A long-lived watcher owns the PR for the whole test.
	if !w.acquireSlot(db.AdmiralTask.PRURL, 0) {
		t.Fatal("initial claim failed")
	}
	t.Cleanup(func() { w.releaseSlot(db.AdmiralTask.PRURL) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.WatchPR(ctx, db.AdmiralTask.PRURL, t.TempDir(), "sess-NEW", db.AdmiralTask.IssueID, defaultTestGhUser)

	ciWatchTaskFailure(t, lc, "already being watched")
}

// TestWatchPR_StaleHeadBySHA: after a fix round the PR head never moves (the
// fix produced no commit, or GitHub never propagated it). Check results for
// the SAME head are the failure we already spent a round on — the watcher
// must not spend another, and must end in a clear escalation instead of
// looping to the timeout.
func TestWatchPR_StaleHeadBySHA(t *testing.T) {
	ghBin := filepath.Join(t.TempDir(), "fake-gh")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" != \"pr\" ]; then echo fake-token; exit 0; fi\n" +
		"if [ \"$2\" = \"view\" ]; then s=$(cat \"$(dirname \"$0\")/sha\" 2>/dev/null); [ -n \"$s\" ] || s=sha-const; printf '{\"headRefOid\":\"%s\"}\n' \"$s\"; exit 0; fi\n" +
		`echo '[{"name":"test","status":"completed","conclusion":"failure","url":"https://ci.example.com/run/1"}]'` + "\n"
	if err := os.WriteFile(ghBin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	w, lc, db, fixCalls := ciWatchFixFixture(ghBin, 2)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.WatchPR(ctx, db.AdmiralTask.PRURL, t.TempDir(), "sess-1", db.AdmiralTask.IssueID, defaultTestGhUser)

	ciWatchTaskFailure(t, lc, "produced no new commit")
	if *fixCalls != 1 {
		t.Errorf("fixRound called %d times, want 1 (stale head must not spend a second round)", *fixCalls)
	}
}

// TestWatchPR_ReportOnlyWhenRoundsZero: ci_fix_max_rounds: 0 means report
// only even though New() always wires a fix runner — the message must say
// the feature is disabled, not "attempted 0 rounds".
func TestWatchPR_ReportOnlyWhenRoundsZero(t *testing.T) {
	ghBin := filepath.Join(t.TempDir(), "fake-gh")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" != \"pr\" ]; then echo fake-token; exit 0; fi\n" +
		"if [ \"$2\" = \"view\" ]; then echo '{\"headRefOid\":\"sha-a\"}'; exit 0; fi\n" +
		`echo '[{"name":"test","status":"completed","conclusion":"failure","url":"https://ci.example.com/run/1"}]'` + "\n"
	if err := os.WriteFile(ghBin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	w, lc, db, fixCalls := ciWatchFixFixture(ghBin, 0) // zero rounds, runner still wired

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.WatchPR(ctx, db.AdmiralTask.PRURL, t.TempDir(), "sess-1", db.AdmiralTask.IssueID, defaultTestGhUser)

	ciWatchTaskFailure(t, lc, "Automatic CI fixing is disabled")
	if *fixCalls != 0 {
		t.Errorf("fixRound called %d times, want 0 (zero rounds configured)", *fixCalls)
	}
	if state, errMsg := taskStateSnapshot(db); state != store.JobStateFailed || errMsg != "ci_failed" {
		t.Errorf("task = %q/%q, want FAILED/ci_failed", state, errMsg)
	}
}

// TestWatchPR_DeletedRowStillReports: an admin reset can delete the
// admiral_tasks row mid-watch. There is nothing to mark FAILED, but the
// thread must still learn the PR is red (the pre-refactor behavior).
func TestWatchPR_DeletedRowStillReports(t *testing.T) {
	ghBin := filepath.Join(t.TempDir(), "fake-gh")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" != \"pr\" ]; then echo fake-token; exit 0; fi\n" +
		"if [ \"$2\" = \"view\" ]; then echo '{\"headRefOid\":\"sha-a\"}'; exit 0; fi\n" +
		`echo '[{"name":"test","status":"completed","conclusion":"failure","url":"https://ci.example.com/run/1"}]'` + "\n"
	if err := os.WriteFile(ghBin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	w, lc, db, _ := ciWatchFixFixture(ghBin, 2)
	db.AdmiralTask = nil // row deleted by admin reset
	db.LiveAdmiralTask = nil

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.WatchPR(ctx, "https://github.com/acme/repo/pull/7", t.TempDir(), "sess-1", "issue-1", defaultTestGhUser)

	ciWatchTaskFailure(t, lc, "no admiral task row")
}

// TestWatchPR_InterruptedFixRoundDoesNotFailTask: a CI fix round cancelled by
// shutdown is not a CI failure. Marking the (DONE) task FAILED/ci_failed would
// be terminal for /resume and the reconciler — stranding a healthy task at
// exactly the restart the reconciler is meant to recover from.
func TestWatchPR_InterruptedFixRoundDoesNotFailTask(t *testing.T) {
	ghBin := filepath.Join(t.TempDir(), "fake-gh")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" != \"pr\" ]; then echo fake-token; exit 0; fi\n" +
		"if [ \"$2\" = \"view\" ]; then echo '{\"headRefOid\":\"sha-a\"}'; exit 0; fi\n" +
		`echo '[{"name":"test","status":"completed","conclusion":"failure","url":"https://ci.example.com/run/1"}]'` + "\n"
	if err := os.WriteFile(ghBin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	w, lc, db, _ := ciWatchFixFixture(ghBin, 2)
	w.fixRound = func(_ context.Context, _ *store.AdmiralTask, _ string) error {
		return errFixRoundInterrupted
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.WatchPR(ctx, db.AdmiralTask.PRURL, t.TempDir(), "sess-1", db.AdmiralTask.IssueID, defaultTestGhUser)

	ciWatchTaskFailure(t, lc, "was NOT resumed")
	if state, errMsg := taskStateSnapshot(db); state != store.JobStateDone || errMsg != "" {
		t.Errorf("task = %q/%q, want it left DONE for the next boot", state, errMsg)
	}
}

// TestFetchCheckRuns_EmptyHeadIsNotFallback: right after a push GitHub has no
// check runs for the new head yet. Falling back to the head-agnostic view
// there re-introduces the stale-head verdict — the previous head's failures
// read as the new head's.
func TestFetchCheckRuns_EmptyHeadIsNotFallback(t *testing.T) {
	ghBin := filepath.Join(t.TempDir(), "fake-gh")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"auth\" ]; then echo fake-token; exit 0; fi\n" +
		"if [ \"$1\" = \"api\" ]; then exit 0; fi\n" + // empty body: no runs yet
		"if [ \"$2\" = \"view\" ]; then echo '{\"headRefOid\":\"sha-b\"}'; exit 0; fi\n" +
		`echo '[{"name":"old-head","status":"completed","conclusion":"failure","url":"u"}]'` + "\n"
	if err := os.WriteFile(ghBin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	w := newCIWatcher(&mockLinearClient{}, &mockStore{}, logger, ghBin, time.Second, time.Minute, 2)

	runs, head, err := w.fetchCheckRuns(context.Background(), t.TempDir(), 7, defaultTestGhUser)
	if err != nil {
		t.Fatalf("fetchCheckRuns: %v", err)
	}
	if len(runs) != 0 {
		t.Errorf("runs = %+v, want none (the old head's failures must not be served as this head's)", runs)
	}
	if head != "sha-b" {
		t.Errorf("head = %q, want sha-b", head)
	}
}
