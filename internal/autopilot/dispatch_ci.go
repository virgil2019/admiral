package autopilot

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/georgehuang/admiral/internal/store"
)

// errFixRoundInterrupted marks a fix round the process stopped for shutdown.
// The round did not fail — the watcher must leave the task DONE so the next
// boot continues it, instead of escalating a healthy task to FAILED/ci_failed.
var errFixRoundInterrupted = errors.New("fix round interrupted by shutdown")

// runCIFixRound is the CI watcher's fixRound: one claude fix round on the
// task's PR branch addressing a CI failure, build-gated and pushed exactly
// like a review round (runPRFix). Progress lands on the PR as the repo's
// registered account; Linear reporting belongs to the watcher (escalateCI /
// the round announcements) so the thread gets one message per event, not
// two.
//
// ctx is the watcher's (cancel-detached) context; the round adds its own
// budget on top, sized to cover the build gate's full retry allowance.
func (o *Orchestrator) runCIFixRound(ctx context.Context, task *store.AdmiralTask, failure string) error {
	defer o.beginRun(task.IssueID)()
	// Branch lock BEFORE the run slot (see runReview for the ordering note).
	unlockPRFix := o.lockPRFix(task.Branch)
	defer unlockPRFix()
	release := o.acquireRunSlot()
	defer release()

	// The watcher's context is deliberately detached from the run that
	// opened the PR (it must outlive it), so it does NOT inherit SIGTERM.
	// Tie the round to the root context as well, or a shutdown would leave
	// this claude child running against a dead parent.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopOnShutdown := context.AfterFunc(o.baseCtx(), cancel)
	defer stopOnShutdown()

	ctx, cancelTimeout := context.WithTimeout(ctx, prFixBudget(o.cfg.MaxRunSeconds, o.cfg.VerifyMaxRetries))
	defer cancelTimeout()

	deps, stage, err := o.resolvePRFixDeps(ctx, task)
	if err != nil {
		if o.shuttingDown() || errors.Is(err, context.Canceled) {
			return errFixRoundInterrupted
		}
		return fmt.Errorf("prepare fix round (%s): %w", stage, err)
	}

	prompt := buildCIFixPrompt(o.cfg.ReviewSkill, task.PRURL, task.Branch, deps.baseBranch, failure)
	out, err := o.runPRFix(ctx, task, deps, prompt)
	if err != nil {
		if o.shuttingDown() || errors.Is(err, context.Canceled) {
			return errFixRoundInterrupted
		}
		var gateFail *gateFailureError
		if errors.As(err, &gateFail) {
			_ = deps.replier.Fail(ctx, gateFail.message())
			return err
		}
		_ = deps.replier.Fail(ctx, fmt.Sprintf("admiral: CI fix round failed: %v", err))
		return err
	}

	if !out.Pushed {
		if o.shuttingDown() || ctx.Err() != nil {
			// runPRFix swallows a cancelled push into Pushed=false with a nil
			// error; reporting that as a push failure makes the watcher
			// escalate a restart and mark the DONE task FAILED/ci_failed.
			return errFixRoundInterrupted
		}
		return fmt.Errorf("push to origin failed — the fix is not on the PR (%s)",
			truncate(out.PushErr, 300))
	}

	output := renderFixReply(out.Output, "addressed the CI failure", out.GateAttempts)
	_ = deps.replier.Reply(ctx, output)
	return nil
}

// buildCIFixPrompt constructs the prompt for one CI-failure fix round. When
// skill is non-empty the prompt starts with "/<skill>\n\n" — same convention
// as the review prompt.
func buildCIFixPrompt(skill, prURL, branch, baseBranch, failure string) string {
	var b strings.Builder
	if skill != "" {
		fmt.Fprintf(&b, "/%s\n\n", skill)
	}
	fmt.Fprintf(&b, "CI failed on PR %s (branch: %s, base: %s).\n\n", prURL, branch, baseBranch)
	fmt.Fprintf(&b, "Failing checks:\n%s\n\n", failure)
	b.WriteString("Investigate the failure logs (you are authenticated as the repo's registered account — use `gh pr checks` and `gh run view --log-failed`), fix the underlying problem, and ")
	b.WriteString(fixBuildInstructions)
	fmt.Fprintf(&b, "Stage and commit your changes to branch %q. Do NOT open a new PR.\n", branch)
	b.WriteString("Summarize what you changed in 1–3 sentences.")
	return b.String()
}
