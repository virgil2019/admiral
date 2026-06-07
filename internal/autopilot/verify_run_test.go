package autopilot

import (
	"context"
	"errors"
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
