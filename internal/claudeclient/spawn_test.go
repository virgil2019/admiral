package claudeclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- Helpers ---

// fakeClaudeScript writes a tiny shell script to a temp file that echoes its
// argv (one arg per line) AND, when consumeStdin is true, reads stdin and
// prepends it to stdout. The script exits with `exit` and emits `stderrLine`
// to stderr. Both status and stderr are baked into the script body — env
// vars set after `exit` are unreachable to the body, which a previous
// incarnation of this function tripped over.
func fakeClaudeScript(t *testing.T, exit int, stderrLine string, consumeStdin bool) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-claude.sh")
	var body strings.Builder
	body.WriteString("#!/bin/sh\n")
	body.WriteString("printf 'ARGS:\\n'\n")
	body.WriteString("for a in \"$@\"; do printf '  %s\\n' \"$a\"; done\n")
	if consumeStdin {
		// Read stdin into $first. With no newline in the prompt, `head -n 1`
		// reads until EOF and returns the entire buffer; the test asserts the
		// captured stdin's size. We echo the length, not the contents, to
		// keep stdout small and avoid embedding large prompts in test
		// failure messages.
		body.WriteString("first=$(head -n 1)\n")
		body.WriteString("if [ -n \"$first\" ]; then printf 'STDIN:len=%d\\n' \"${#first}\"; fi\n")
	}
	if stderrLine != "" {
		body.WriteString("printf '%s\\n' " + shquote(stderrLine) + " 1>&2\n")
	}
	body.WriteString("exit " + itoa(exit) + "\n")
	if err := os.WriteFile(path, []byte(body.String()), 0o755); err != nil {
		t.Fatalf("write fake script: %v", err)
	}
	return path
}

// itoa is hand-rolled because strconv is intentionally not imported for
// these tests (keeping dependencies minimal — every byte of test code that
// lands in coverage counts).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	negative := n < 0
	if negative {
		n = -n
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if negative {
		digits = append([]byte{'-'}, digits...)
	}
	return string(digits)
}

// shquote wraps s in single quotes for safe inclusion in the generated
// shell script. Empty input becomes the literal "" (no quotes).
func shquote(s string) string {
	if s == "" {
		return ""
	}
	// Single-quote everything; replace any ' inside with '\''.
	out := []byte{'\''}
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			out = append(out, '\'', '\'', '\'')
		} else {
			out = append(out, s[i])
		}
	}
	out = append(out, '\'')
	return string(out)
}

// readFakeOutput runs Run with the given fake-claude script and returns the
// captured stdout. Caller specifies prompt + extraArgs; logger is silenced
// for predictability (otherwise the test's stderr gets flooded).
func runWithFake(t *testing.T, claudeBin string, prompt string, extraArgs []string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	silent := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
	out, err := Run(context.Background(), claudeBin, 30,
		"", nil, prompt, extraArgs, dir, silent)
	return out, err
}

// --- Tests ---

// TestRun_SmallPromptUsesArgv guards the happy path: prompts under
// SafeArgvBudget pass positionally as the `-p` argv, no temp dump file is
// written, no stdin plumbing happens.
func TestRun_SmallPromptUsesArgv(t *testing.T) {
	claudeBin := fakeClaudeScript(t, 0, "", false)
	const prompt = "hello, claude"
	out, err := runWithFake(t, claudeBin, prompt, []string{"--dangerously-skip-permissions"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "  -p") {
		t.Errorf("expected `-p` arg in argv:\n%s", out)
	}
	if !strings.Contains(out, "  hello, claude") {
		t.Errorf("expected prompt as positional arg:\n%s", out)
	}
	if strings.Contains(out, "STDIN:") {
		t.Errorf("small prompt must not be sent via stdin:\n%s", out)
	}
}

// TestRun_LargePromptUsesStdin guards the bug we're fixing: a prompt past
// SafeArgvBudget is routed via cmd.Stdin (so kernel argv is bounded), the
// positional `-p` flag has NO value attached (claude reads stdin), and on
// success the dump file is removed from disk.
func TestRun_LargePromptUsesStdin(t *testing.T) {
	claudeBin := fakeClaudeScript(t, 0, "", true) // echo stdin length
	// Build a prompt > SafeArgvBudget (96 KiB) so the helper takes the
	// stdin transport. Keep it cheap by repeating a short string.
	promptLen := SafeArgvBudget + 1024             // 1 KiB over budget
	prompt := strings.Repeat("X", promptLen)
	dir := t.TempDir() // capture any dump the helper *might* leave

	out, err := runWithFake(t, claudeBin, prompt, []string{"--dangerously-skip-permissions"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Argv should NOT contain the prompt body. The fake script prints
	// each arg on its own line; the prompt would never fit a line
	// (SafeArgvBudget is 96 KiB). We assert the absence of long X runs.
	if strings.Contains(out, strings.Repeat("X", 256)) {
		t.Errorf("large prompt must not appear in argv:\n%.200s...", out)
	}
	// Stdin marker must show the SAME length as the prompt.
	wantMarker := fmt.Sprintf("STDIN:len=%d", promptLen)
	if !strings.Contains(out, wantMarker) {
		t.Errorf("expected stdin marker %q in output:\n%s", wantMarker, out)
	}
	// On success the dump file should be removed.
	matches, _ := filepath.Glob(filepath.Join(dir, "claude-prompt-*.md"))
	if len(matches) != 0 {
		t.Errorf("dump file should have been removed on success, found: %v", matches)
	}
}

// TestRun_LargePromptFailurePreservesDump covers the post-mortem path: when
// the child exits non-zero the dump file is KEPT on disk and the returned
// error mentions its path so an operator can `cat` it.
func TestRun_LargePromptFailurePreservesDump(t *testing.T) {
	claudeBin := fakeClaudeScript(t, 1, "", true) // exit 1
	prompt := strings.Repeat("Y", SafeArgvBudget+512)
	dir := t.TempDir()
	silent := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
	_, err := Run(context.Background(), claudeBin, 30,
		"", nil, prompt, []string{"--dangerously-skip-permissions"}, dir, silent)
	if err == nil {
		t.Fatal("expected non-zero exit to surface as error")
	}
	if !strings.Contains(err.Error(), "prompt dumped to") {
		t.Errorf("error must mention dump path: %v", err)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "claude-prompt-*.md"))
	if len(matches) != 1 {
		t.Fatalf("expected exactly one dump file kept, found %d: %v", len(matches), matches)
	}
	body, rerr := os.ReadFile(matches[0])
	if rerr != nil {
		t.Fatalf("read dump: %v", rerr)
	}
	// The dump file carries the EXACT prompt that was about to be sent —
	// this is the whole reason we keep it on failure.
	if !bytes.Equal([]byte(prompt), body) {
		t.Errorf("dump file content differs from prompt (got %d bytes, want %d)", len(body), len(prompt))
	}
}

// TestRun_StderrRoutedToLogger covers the non-error stderr path. The helper
// should drain each stderr line into a Warn log; we hook the handler with a
// strings.Builder buffer and assert the line is captured.
func TestRun_StderrRoutedToLogger(t *testing.T) {
	claudeBin := fakeClaudeScript(t, 0, "stderr line from claude", false)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	if _, err := Run(context.Background(), claudeBin, 30,
		"", nil, "hi", []string{"--dangerously-skip-permissions"}, "", logger); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(buf.String(), "stderr line from claude") {
		t.Errorf("stderr line not captured by logger: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "claude_stderr") {
		t.Errorf("expected claude_stderr log key, got: %q", buf.String())
	}
}

// TestRun_ContextDeadlineDistinguished makes sure ctx-deadline still
// surfaces as a wrapped DeadlineExceeded (the existing call sites rely on
// this distinction for retry vs hard-failure decisions).
func TestRun_ContextDeadlineDistinguished(t *testing.T) {
	// Use a fake claude that sleeps just long enough to trip the ctx.
	claudeBin := fakeClaudeScriptSleep(t, 5*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	silent := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
	_, err := Run(ctx, claudeBin, 30,
		"", nil, "x", []string{"--dangerously-skip-permissions"}, "", silent)
	if err == nil {
		t.Fatal("expected deadline error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error chain must include DeadlineExceeded, got: %v", err)
	}
}

// fakeClaudeScriptSleep writes a sh script that sleeps for `seconds` then
// exits 0. Used by TestRun_ContextDeadlineDistinguished.
func fakeClaudeScriptSleep(t *testing.T, seconds time.Duration) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-claude-sleep.sh")
	body := "#!/bin/sh\nsleep " + itoa(int(seconds/time.Second)) + "\nexit 0\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write sleep script: %v", err)
	}
	return path
}

// TestTotalArgvBytes_Boundary is the exact-on-the-96KiB-line check. A
// miscount here would either: (a) trigger the stdin path on inputs the
// argv could absorb (pathologically slow because the file dump write +
// reopen wastes I/O), or (b) overflow ARG_MAX (the bug we're fixing).
// The boundary case has to land on SafeArgvBudget itself.
func TestTotalArgvBytes_Boundary(t *testing.T) {
	cases := []struct {
		name       string
		claudeBin  string
		prompt     string
		extraArgs  []string
		wantBudget bool // true → fits under SafeArgvBudget; false → over
	}{
		{
			name:      "empty-everything",
			claudeBin: "/usr/bin/claude",
			wantBudget: true,
		},
		{
			name:      "exact-budget-prompt",
			claudeBin: "/usr/bin/claude",
			// We need totalArgvBytes == SafeArgvBudget. Working backwards:
			// bin (15) + "-p" (2) + extraArgs (0) + NULs (1) + prompt = 96KiB.
			// prompt = 96*1024 - 18 = 98304 - 18 = 98286.
			prompt:     strings.Repeat("p", SafeArgvBudget-18),
			wantBudget: true,
		},
		{
			name:      "one-byte-over-budget-prompt",
			claudeBin: "/usr/bin/claude",
			prompt:    strings.Repeat("p", SafeArgvBudget-17),
			wantBudget: false,
		},
		{
			name:      "empty-prompt-but-fat-extra-args-tip-over",
			claudeBin: "/usr/bin/claude",
			extraArgs: []string{strings.Repeat("x", SafeArgvBudget)},
			wantBudget: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := totalArgvBytes(tc.claudeBin, tc.prompt, tc.extraArgs)
			actual := got <= SafeArgvBudget
			if actual != tc.wantBudget {
				t.Errorf("totalArgvBytes(%q, %q, %d args of len ~%d) = %d (≤ %v → %v); want %v",
					tc.claudeBin, tc.prompt, len(tc.extraArgs),
					avgArgLen(tc.extraArgs), got, SafeArgvBudget, actual, tc.wantBudget)
			}
		})
	}
}

func avgArgLen(args []string) int {
	if len(args) == 0 {
		return 0
	}
	n := 0
	for _, a := range args {
		n += len(a)
	}
	return n / len(args)
}

// TestPrepareCmd_StdinFileReopenedForReadOnly guards an internal invariant:
// the file descriptor that becomes cmd.Stdin must be open for reading so
// the child can read it. We assert indirectly by passing a fake claude that
// reads stdin and asserting the prompt length shows up.
func TestPrepareCmd_StdinFileReopenedForReadOnly(t *testing.T) {
	claudeBin := fakeClaudeScript(t, 0, "", true)
	dir := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	promptLen := SafeArgvBudget + 256
	prompt := strings.Repeat("Z", promptLen)
	cmd, dumpPath := PrepareCmd(ctx, claudeBin, "", nil, prompt,
		[]string{"--dangerously-skip-permissions"}, dir)
	if dumpPath == "" {
		t.Fatal("expected non-empty dumpPath for over-budget prompt")
	}
	if cmd.Stdin == nil {
		t.Fatal("expected cmd.Stdin set to file when prompt is large")
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	got, _ := io.ReadAll(stdout)
	cmd.Wait()
	wantMarker := fmt.Sprintf("STDIN:len=%d", promptLen)
	if !strings.Contains(string(got), wantMarker) {
		t.Errorf("fake claude did not receive %s: %q", wantMarker, string(got))
	}
	// PrepareCmd leaves lifecycle to caller; the test cleans up.
	os.Remove(dumpPath)
}
