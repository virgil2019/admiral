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
	db := &mockStore{}
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

	ciWatchTaskFailure(t, lc, "could not fix it after 2 fix round(s)")
	body := ciWatchTaskFailure(t, lc, "comment `/fix` on the issue")
	if !strings.Contains(body, "could not fix it after 2 fix round(s)") {
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

	ciWatchTaskFailure(t, lc, "could not fix it after 0 fix round(s)")
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
