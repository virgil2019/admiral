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
