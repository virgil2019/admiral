package autopilot

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/georgehuang/admiral/internal/store"
)

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
	release := o.acquireRunSlot()
	defer release()

	ctx, cancel := context.WithTimeout(ctx, prFixBudget(o.cfg.MaxRunSeconds, o.cfg.VerifyMaxRetries))
	defer cancel()

	deps, stage, err := o.resolvePRFixDeps(ctx, task)
	if err != nil {
		return fmt.Errorf("prepare fix round (%s): %w", stage, err)
	}

	prompt := buildCIFixPrompt(o.cfg.ReviewSkill, task.PRURL, task.Branch, deps.baseBranch, failure)
	out, err := o.runPRFix(ctx, task, deps, prompt)
	if err != nil {
		var gateFail *gateFailureError
		if errors.As(err, &gateFail) {
			_ = deps.replier.Fail(ctx, gateFail.message())
			return err
		}
		_ = deps.replier.Fail(ctx, fmt.Sprintf("admiral: CI fix round failed: %v", err))
		return err
	}

	if !out.Pushed {
		return fmt.Errorf("push to origin failed — the fix is not on the PR (%s)",
			truncate(out.Output, 200))
	}

	output := out.Output
	if output == "" {
		output = "addressed the CI failure"
	}
	if out.GateAttempts > 0 {
		output += fmt.Sprintf("\n\n_(admiral build gate: passed after %d attempt(s))_", out.GateAttempts)
	}
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
	b.WriteString("Investigate the failure logs (you are authenticated as the repo's registered account — use `gh pr checks` and `gh run view --log-failed`), fix the underlying problem, and run the project's build / test command " +
		"(e.g. `swift build`, `go build ./...`, `cargo build`, `npm test` — whichever this repo uses; " +
		"check the repo's README or common build files if unsure) and ensure it passes locally. ")
	b.WriteString("Do not commit broken code. ")
	fmt.Fprintf(&b, "Stage and commit your changes to branch %q. Do NOT open a new PR.\n", branch)
	b.WriteString("Summarize what you changed in 1–3 sentences.")
	return b.String()
}
