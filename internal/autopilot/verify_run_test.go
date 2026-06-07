package autopilot

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRunRepoVerify_Success(t *testing.T) {
	dir := t.TempDir()
	r := runRepoVerify(context.Background(), dir, "echo hello && echo world", 30)
	if r.Err != nil {
		t.Fatalf("Err: %v", r.Err)
	}
	if r.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", r.ExitCode)
	}
	if !strings.Contains(r.Output, "hello") || !strings.Contains(r.Output, "world") {
		t.Errorf("Output missing expected lines: %q", r.Output)
	}
	if r.TimedOut {
		t.Error("TimedOut unexpected")
	}
}

func TestRunRepoVerify_NonZeroExit(t *testing.T) {
	dir := t.TempDir()
	// `false` reliably exits 1. Stderr message lets us verify combined-output
	// capture as well.
	r := runRepoVerify(context.Background(), dir, "echo build error 1>&2 && exit 7", 30)
	if r.Err != nil {
		t.Fatalf("Err: %v", r.Err)
	}
	if r.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7", r.ExitCode)
	}
	if !strings.Contains(r.Output, "build error") {
		t.Errorf("Output missing stderr line: %q", r.Output)
	}
}

func TestRunRepoVerify_Timeout(t *testing.T) {
	dir := t.TempDir()
	// 0 timeoutSec falls back to 600; use ctx with very short deadline instead
	// so the test stays fast.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	r := runRepoVerify(ctx, dir, "sleep 5", 10)
	if !r.TimedOut {
		t.Fatalf("expected TimedOut=true, got %#v", r)
	}
	if r.Err == nil || !errors.Is(r.Err, context.DeadlineExceeded) {
		t.Errorf("Err = %v, want wrapped DeadlineExceeded", r.Err)
	}
}

func TestRunRepoVerify_EmptyCmd(t *testing.T) {
	r := runRepoVerify(context.Background(), t.TempDir(), "   ", 30)
	if r.Err == nil {
		t.Fatal("expected Err for empty verifyCmd")
	}
	if !strings.Contains(r.Err.Error(), "empty verifyCmd") {
		t.Errorf("Err message %q does not name the empty-cmd case", r.Err.Error())
	}
}

func TestRunRepoVerify_OutputCap(t *testing.T) {
	dir := t.TempDir()
	// Emit well over 8 KiB to force truncation. Distinctive tail string lets
	// us assert the cap kept the END (which is the meaningful part for build
	// errors).
	r := runRepoVerify(context.Background(), dir,
		"for i in $(seq 1 200); do printf 'line-pad-pad-pad-pad-pad-pad-pad-pad-pad-pad-%05d\n' $i; done; echo TAIL_MARKER", 30)
	if r.Err != nil {
		t.Fatalf("Err: %v", r.Err)
	}
	if r.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", r.ExitCode)
	}
	if !strings.Contains(r.Output, "TAIL_MARKER") {
		t.Errorf("cap dropped the tail; Output ends with: %q", tail(r.Output, 200))
	}
	if !strings.Contains(r.Output, "output truncated") {
		t.Errorf("cap did not record a truncation marker; Output starts with: %q", head(r.Output, 200))
	}
	if len(r.Output) > verifyOutputCapBytes+200 { // marker prefix adds a few bytes
		t.Errorf("Output longer than cap + marker: %d > %d", len(r.Output), verifyOutputCapBytes+200)
	}
}

func TestRunRepoVerify_CommandNotFound(t *testing.T) {
	// A non-existent command exits non-zero via sh's "command not found"
	// path — sh itself runs fine and returns 127, so this is treated as a
	// normal non-zero exit (ExitCode set, Err nil).
	r := runRepoVerify(context.Background(), t.TempDir(), "definitely_not_a_real_command_42 --flag", 30)
	if r.Err != nil {
		t.Fatalf("Err: %v (expected nil — sh ran and exited)", r.Err)
	}
	if r.ExitCode == 0 {
		t.Errorf("ExitCode = 0, want non-zero (sh reports command not found)")
	}
}

// --- runVerifyWithRetry ---

// touchScript installs a shell helper at <dir>/verify.sh that fails until a
// "marker" file in <dir>/.fail-until exists with the right contents. Lets a
// test drive a fake "claude fixed the bug" interaction: claudeRunner can
// remove the marker to make the next build pass.
func writeFakeBuild(t *testing.T, dir, body string) string {
	t.Helper()
	path := dir + "/verify.sh"
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write fake build: %v", err)
	}
	return "sh " + path
}

func TestRunVerifyWithRetry_PassesFirstAttempt(t *testing.T) {
	dir := t.TempDir()
	cmd := writeFakeBuild(t, dir, "echo ok\nexit 0\n")
	claudeCalled := 0
	runner := func(ctx context.Context, wt, prompt string) (string, error) {
		claudeCalled++
		return "", nil
	}
	r := runVerifyWithRetry(context.Background(), dir, cmd, 30, 2, runner)
	if !r.Passed {
		t.Fatalf("expected Passed=true, got %#v", r)
	}
	if r.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", r.Attempts)
	}
	if claudeCalled != 0 {
		t.Errorf("claudeRunner called %d times; expected 0 (no retry needed)", claudeCalled)
	}
	if r.LastClaudeReply != "" {
		t.Errorf("LastClaudeReply set without a retry: %q", r.LastClaudeReply)
	}
}

func TestRunVerifyWithRetry_PassesAfterOneRetry(t *testing.T) {
	dir := t.TempDir()
	// Fail until <dir>/.fixed exists, then pass.
	cmd := writeFakeBuild(t, dir, "if [ -f \""+dir+"/.fixed\" ]; then echo good; exit 0; fi\necho first-attempt-fail 1>&2\nexit 1\n")
	claudeCalled := 0
	runner := func(ctx context.Context, wt, prompt string) (string, error) {
		claudeCalled++
		// Simulate claude fixing the build.
		if err := os.WriteFile(dir+"/.fixed", []byte("ok"), 0o644); err != nil {
			t.Fatalf("simulate claude fix: %v", err)
		}
		return "fixed it", nil
	}
	r := runVerifyWithRetry(context.Background(), dir, cmd, 30, 2, runner)
	if !r.Passed {
		t.Fatalf("expected Passed=true after retry; got %#v", r)
	}
	if r.Attempts != 2 {
		t.Errorf("Attempts = %d, want 2 (1 fail + 1 success)", r.Attempts)
	}
	if claudeCalled != 1 {
		t.Errorf("claudeRunner called %d times; expected 1", claudeCalled)
	}
	if r.LastClaudeReply != "fixed it" {
		t.Errorf("LastClaudeReply = %q, want %q", r.LastClaudeReply, "fixed it")
	}
}

func TestRunVerifyWithRetry_AllAttemptsFail(t *testing.T) {
	dir := t.TempDir()
	// Always fails.
	cmd := writeFakeBuild(t, dir, "echo always-broken 1>&2\nexit 1\n")
	claudeCalled := 0
	runner := func(ctx context.Context, wt, prompt string) (string, error) {
		claudeCalled++
		return "tried but failed", nil
	}
	r := runVerifyWithRetry(context.Background(), dir, cmd, 30, 2, runner)
	if r.Passed {
		t.Fatalf("expected Passed=false; got %#v", r)
	}
	if r.Attempts != 3 {
		t.Errorf("Attempts = %d, want 3 (1 initial + 2 retries)", r.Attempts)
	}
	if claudeCalled != 2 {
		t.Errorf("claudeRunner called %d times; expected 2 (retries only, no claude on final fail)", claudeCalled)
	}
	if !strings.Contains(r.LastBuildOutput, "always-broken") {
		t.Errorf("LastBuildOutput missing failure marker: %q", r.LastBuildOutput)
	}
}

func TestRunVerifyWithRetry_MaxRetriesZero(t *testing.T) {
	dir := t.TempDir()
	cmd := writeFakeBuild(t, dir, "exit 1\n")
	claudeCalled := 0
	runner := func(ctx context.Context, wt, prompt string) (string, error) {
		claudeCalled++
		return "", nil
	}
	r := runVerifyWithRetry(context.Background(), dir, cmd, 30, 0, runner)
	if r.Passed {
		t.Fatal("expected Passed=false with maxRetries=0")
	}
	if r.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1 (no retries allowed)", r.Attempts)
	}
	if claudeCalled != 0 {
		t.Errorf("claudeRunner called %d times; expected 0 (no retry budget)", claudeCalled)
	}
}

func TestRunVerifyWithRetry_ClaudeRunnerError(t *testing.T) {
	dir := t.TempDir()
	cmd := writeFakeBuild(t, dir, "exit 1\n") // always fail so we always retry
	runner := func(ctx context.Context, wt, prompt string) (string, error) {
		return "", errors.New("claude binary missing")
	}
	r := runVerifyWithRetry(context.Background(), dir, cmd, 30, 2, runner)
	if r.Passed {
		t.Fatal("expected Passed=false when claudeRunner errors")
	}
	if r.LaunchErr == "" {
		t.Fatal("expected LaunchErr to be set when claudeRunner returns error")
	}
	if !strings.Contains(r.LaunchErr, "claude binary missing") {
		t.Errorf("LaunchErr should carry the underlying error: %q", r.LaunchErr)
	}
	// First build ran (1 attempt) before retry tried + failed.
	if r.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1 (build ran once, retry-claude failed to launch)", r.Attempts)
	}
}

func TestRunVerifyWithRetry_EmptyVerifyCmd(t *testing.T) {
	runner := func(ctx context.Context, wt, prompt string) (string, error) { return "", nil }
	r := runVerifyWithRetry(context.Background(), t.TempDir(), "  ", 30, 2, runner)
	if r.Passed {
		t.Error("expected Passed=false on empty verifyCmd")
	}
	if r.LaunchErr == "" {
		t.Error("expected LaunchErr to flag the programmer error of empty verifyCmd")
	}
}

func TestRunVerifyWithRetry_NilClaudeRunner(t *testing.T) {
	r := runVerifyWithRetry(context.Background(), t.TempDir(), "exit 0", 30, 2, nil)
	if r.Passed {
		t.Error("expected Passed=false on nil claudeRunner")
	}
	if r.LaunchErr == "" {
		t.Error("expected LaunchErr to flag nil claudeRunner")
	}
}

func TestBuildVerifyRetryPrompt_IncludesEssentials(t *testing.T) {
	br := VerifyRunResult{ExitCode: 7, Output: "error: type mismatch on line 42"}
	p := buildVerifyRetryPrompt("swift build", br, 2, 3)
	for _, want := range []string{
		"FAILED",
		"Retry attempt 2 of 3",
		"swift build",
		"Exit code: 7",
		"type mismatch on line 42",
		"fix forward",
		"do NOT revert or open a new PR",
		"MUST exit 0",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("retry prompt missing %q\n---\n%s", want, p)
		}
	}
}

func TestBuildVerifyRetryPrompt_TimeoutBranch(t *testing.T) {
	br := VerifyRunResult{TimedOut: true, Output: "still running..."}
	p := buildVerifyRetryPrompt("go test ./...", br, 1, 3)
	if !strings.Contains(p, "TIMED OUT") {
		t.Errorf("retry prompt missing TIMED OUT branch: %s", p)
	}
}

func head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
