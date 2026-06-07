package autopilot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// verifyOutputCapBytes is how much combined stdout+stderr runRepoVerify keeps
// from the tail of a build/test run. Most build tools emit the meaningful
// error at the end (Go panics, swift / cargo / npm / pytest summaries); 8 KiB
// gives the LLM judge enough context to reason about a failure without
// blowing context budget.
const verifyOutputCapBytes = 8 * 1024

// VerifyRunResult is what runRepoVerify returns to its caller. It deliberately
// separates "the command ran and exited" (ExitCode + Output) from "the command
// could not be run" (Err): a non-zero exit code is normal data the judge must
// see, but a launch / timeout failure is a runtime problem the caller has to
// surface differently.
type VerifyRunResult struct {
	// ExitCode is the process exit code. 0 means success. Defined only when
	// Err is nil.
	ExitCode int
	// Output is the last verifyOutputCapBytes bytes of combined stdout+stderr,
	// in the order they were written. Prefixed with a truncation marker when
	// the original output was longer.
	Output string
	// TimedOut is true when the context deadline elapsed before the command
	// finished. ExitCode in that case is not meaningful; Err carries
	// context.DeadlineExceeded.
	TimedOut bool
	// Err is non-nil only when the command itself could not be run (sh not
	// found, IO error, context cancelled / deadline) — NOT when the command
	// exited non-zero. Callers must check this before reading ExitCode.
	Err error
}

// runRepoVerify executes verifyCmd in repoDir via `sh -c`, with a timeout of
// timeoutSec seconds (defaulting to a sensible cap when <= 0). It is used as
// admiral's hard gate before the LLM judge in L2 verify, and (in a follow-up)
// as the gate after claude in the autopilot first-run and review-dispatch
// paths.
//
// verifyCmd is run as a single shell string so operators can compose commands
// (`swift build && swift test`). admiral config is trusted — shell-injection
// is not a concern for this caller.
//
// Empty verifyCmd is a programmer error: the caller must check + skip
// upstream. Passing "" returns an Err result.
func runRepoVerify(ctx context.Context, repoDir, verifyCmd string, timeoutSec int) VerifyRunResult {
	verifyCmd = strings.TrimSpace(verifyCmd)
	if verifyCmd == "" {
		return VerifyRunResult{Err: errors.New("runRepoVerify: empty verifyCmd (caller must check + skip)")}
	}

	if timeoutSec <= 0 {
		// Fallback when the caller passes a bad timeout. 10 min is enough for
		// any reasonable single build/test invocation; longer runs likely
		// indicate a hang, which is exactly what we want timing out.
		timeoutSec = 600
	}

	cctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(cctx, "sh", "-c", verifyCmd)
	cmd.Dir = repoDir
	cmd.Env = os.Environ()
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second

	// Use a single buffer for combined stdout+stderr so the tail-cap below
	// preserves interleaved write order (most build tools put errors and
	// progress on stderr; mixing them is what the judge needs to read).
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	runErr := cmd.Run()
	out := capTail(buf.Bytes(), verifyOutputCapBytes)

	if cctx.Err() == context.DeadlineExceeded {
		return VerifyRunResult{
			Output:   out,
			TimedOut: true,
			Err:      fmt.Errorf("verify command timed out after %ds: %w", timeoutSec, context.DeadlineExceeded),
		}
	}

	if runErr != nil {
		// ExitError = the command ran and exited non-zero; that is data, not
		// failure. Any other error type (start failed, IO error) is a runtime
		// problem the caller must distinguish.
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			return VerifyRunResult{
				ExitCode: ee.ExitCode(),
				Output:   out,
			}
		}
		return VerifyRunResult{
			Output: out,
			Err:    fmt.Errorf("run verify command: %w", runErr),
		}
	}

	return VerifyRunResult{
		ExitCode: 0,
		Output:   out,
	}
}

// capTail returns the last n bytes of b, prefixed with a truncation marker
// when the original was longer. Used to keep the LLM-judge prompt under
// budget while still surfacing the most informative tail of a build log.
func capTail(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return fmt.Sprintf("... (output truncated to last %d bytes of %d) ...\n", n, len(b)) + string(b[len(b)-n:])
}

// VerifyRetryResult is what runVerifyWithRetry returns. Separates the
// observable outcome (Passed) from diagnostic context (LastBuildOutput,
// LastClaudeReply, Attempts) and launch failures (LaunchErr).
type VerifyRetryResult struct {
	// Passed is true when some build attempt exited 0. False otherwise — the
	// caller MUST NOT push when this is false.
	Passed bool
	// Attempts is the number of build invocations actually made — at least 1
	// (the initial verify after claude's first run), at most maxRetries+1.
	Attempts int
	// LastBuildOutput is the capped tail of the last build's combined
	// stdout+stderr. Set on both pass and fail (for success-side telemetry +
	// failure-side reporting to the user).
	LastBuildOutput string
	// LastClaudeReply is the trimmed stdout from the last claude retry run.
	// Empty when no retry happened (i.e. Attempts == 1).
	LastClaudeReply string
	// LaunchErr is set when a build or claude invocation could not be launched
	// at all (sh missing, claude binary missing, IO error). When non-empty,
	// Passed is false and the loop bailed early — the caller should treat
	// this as a runtime problem, not a build failure.
	LaunchErr string
}

// runVerifyWithRetry is admiral's hard gate around the verify_cmd. Designed
// to be called AFTER an initial claude run has already produced commits in
// worktreePath:
//
//  1. Run verifyCmd via runRepoVerify.
//  2. Exit 0 → return Passed=true.
//  3. Non-zero / timeout → if retries remain, call claudeRunner with a retry
//     prompt naming the failure + asking for a fix-forward commit. Loop.
//  4. Out of retries → return Passed=false. Caller MUST NOT push.
//
// claudeRunner is a closure the caller injects so this helper does not have
// to know about claude binaries, skills, or per-path prompt scaffolding. The
// caller wires its own claude invocation (typically `runClaudeForReview` /
// `runClaudeForAutopilot` with whatever skill prefix applies). claudeRunner's
// returned output is captured into LastClaudeReply for the caller's reply
// channel (PR comment / Linear thread).
//
// Empty verifyCmd is a programmer error — the caller MUST check + skip
// upstream (this helper exists to gate, not to decide whether gating is
// enabled).
func runVerifyWithRetry(
	ctx context.Context,
	worktreePath, verifyCmd string,
	buildTimeoutSec, maxRetries int,
	claudeRunner func(ctx context.Context, wt, prompt string) (output string, err error),
) VerifyRetryResult {
	verifyCmd = strings.TrimSpace(verifyCmd)
	if verifyCmd == "" {
		return VerifyRetryResult{LaunchErr: "runVerifyWithRetry: empty verifyCmd (caller must check + skip)"}
	}
	if claudeRunner == nil {
		return VerifyRetryResult{LaunchErr: "runVerifyWithRetry: nil claudeRunner"}
	}
	if maxRetries < 0 {
		maxRetries = 0
	}

	result := VerifyRetryResult{}

	for attempt := 0; attempt <= maxRetries; attempt++ {
		br := runRepoVerify(ctx, worktreePath, verifyCmd, buildTimeoutSec)
		result.Attempts = attempt + 1
		result.LastBuildOutput = br.Output

		// Build launched + exited cleanly → done, regardless of retry budget.
		if br.Err == nil && !br.TimedOut && br.ExitCode == 0 {
			result.Passed = true
			return result
		}

		// Distinguish "build couldn't run at all" (LaunchErr) from "build ran
		// and reported failure" (drop into retry). A launch error is a runtime
		// problem the caller must surface differently — retry won't help when
		// sh is missing.
		if br.Err != nil && !br.TimedOut {
			// Real launch failure (vs ExitError, which runRepoVerify already
			// folds into ExitCode). Bail.
			result.LaunchErr = br.Err.Error()
			return result
		}

		// We have a real build failure (non-zero exit OR timeout). If we've
		// used our last retry attempt, give up.
		if attempt == maxRetries {
			return result
		}

		// Retry: ask claude to fix the build, then loop back to verify.
		retryPrompt := buildVerifyRetryPrompt(verifyCmd, br, attempt+1, maxRetries+1)
		claudeOut, claudeErr := claudeRunner(ctx, worktreePath, retryPrompt)
		result.LastClaudeReply = claudeOut
		if claudeErr != nil {
			// claude itself couldn't run (binary missing, context cancelled).
			// Without a successful retry run, the next verify would just repeat
			// the same failure. Bail with LaunchErr.
			result.LaunchErr = fmt.Sprintf("claude retry attempt %d failed to run: %v", attempt+1, claudeErr)
			return result
		}
	}

	return result
}

// buildVerifyRetryPrompt is the generic "your previous attempt's build failed,
// fix it" prompt fed to claude between verify rounds. Intentionally path-
// agnostic: claude has the worktree state (including its own prior commits)
// to figure out what was being attempted. Path-specific framing belongs in
// the INITIAL prompt (set up by `buildReviewPrompt` / `buildPrompt`).
func buildVerifyRetryPrompt(verifyCmd string, br VerifyRunResult, attempt, total int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "admiral just ran the configured verify command in this worktree and it FAILED. ")
	fmt.Fprintf(&b, "Retry attempt %d of %d.\n\n", attempt, total)
	fmt.Fprintf(&b, "Command: `%s`\n", verifyCmd)
	switch {
	case br.TimedOut:
		b.WriteString("Result: TIMED OUT (treat as a non-passing build)\n\n")
	default:
		fmt.Fprintf(&b, "Exit code: %d\n\n", br.ExitCode)
	}
	if out := strings.TrimSpace(br.Output); out != "" {
		b.WriteString("Output tail (combined stdout+stderr, capped):\n```\n")
		b.WriteString(out)
		b.WriteString("\n```\n\n")
	} else {
		b.WriteString("(no output captured)\n\n")
	}
	b.WriteString("Diagnose the failure, fix the code, and stage + commit the fix on this branch. ")
	b.WriteString("Your previous commit(s) from this session are still in place; fix forward — do NOT revert or open a new PR. ")
	b.WriteString("The verify command MUST exit 0 before the change can ship. Summarize what you changed in 1–3 sentences.")
	return b.String()
}
