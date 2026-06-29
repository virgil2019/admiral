package autopilot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/georgehuang/admiral/internal/linear"
)

// dispatchReset handles the /reset Linear command. /reset is a parent-level
// command: it resets a whole task (the parent issue + all its sub-issues)
// across GitHub, Linear, and admiral's DB, reusing the same runResetTask the
// admin endpoint calls.
//
// Two-step and stateless: a bare /reset previews exactly what will be destroyed
// and asks for confirmation; only `/reset confirm` actually executes. There is
// no stored pending-reset state — the `confirm` argument IS the gate, which
// matches the async Linear-thread UX (read the preview, then send a second,
// deliberate comment).
func (o *Orchestrator) dispatchReset(ev linear.AgentEvent, remainder string) {
	if strings.EqualFold(strings.TrimSpace(remainder), "confirm") {
		o.executeReset(ev)
		return
	}
	o.postResetPreview(ev)
}

// postResetPreview lists the sub-issues (and the PRs that will be closed) and
// asks the user to confirm. It reads only Linear + DB state — no gh calls — so
// it stays fast; the merged-PR guard runs at execute time.
func (o *Orchestrator) postResetPreview(ev linear.AgentEvent) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	parent, err := o.lc.GetIssue(ctx, ev.IssueID)
	if err != nil {
		o.postRejection(ev.SessionID, fmt.Sprintf("admiral: couldn't load %s for /reset: %v", ev.IssueIdentifier, err))
		return
	}
	subs, err := o.lc.GetSubIssues(ctx, parent.ID)
	if err != nil {
		o.postRejection(ev.SessionID, fmt.Sprintf("admiral: couldn't list sub-issues of %s: %v", parent.Identifier, err))
		return
	}
	if len(subs) == 0 {
		o.postRejection(ev.SessionID, fmt.Sprintf(
			"admiral: %s has no sub-issues. /reset operates on a parent task and all its sub-issues — mention it on the parent task issue.",
			parent.Identifier))
		return
	}

	var b strings.Builder
	fmt.Fprintf(&b, "⚠️ /reset will fully reset task %s and its %d sub-issue(s):\n\n", parent.Identifier, len(subs))
	inFlight := 0
	for _, sub := range subs {
		line := "  • " + sub.Identifier
		// Error is swallowed on purpose: a transient DB hiccup degrades the
		// preview to "no PR / run info shown" for that sub rather than failing
		// the whole preview. The execute path (runResetTask) treats the same
		// lookup error as fatal.
		if t, _ := o.db.GetAdmiralTaskByIssue(sub.ID); t != nil {
			if t.PRURL != "" {
				line += " — PR " + t.PRURL
			}
			if taskInFlight(t.State) {
				line += "  ⚠️ currently running (" + t.State + ")"
				inFlight++
			}
		}
		b.WriteString(line + "\n")
	}
	if inFlight > 0 {
		fmt.Fprintf(&b, "\n⚠️ %d sub-issue(s) are still running — resetting will tear down work in progress.\n", inFlight)
	}
	b.WriteString("\nFor every sub-issue this will: close its PR and DELETE its branch, " +
		"reset its Linear state to backlog, drop the agent-ready label, and wipe admiral's DB rows. " +
		"It also resets the parent's verification loop. This is irreversible.\n")
	b.WriteString("If any sub-issue's PR is already merged, the whole reset is refused " +
		"(undo the merge on GitHub first).\n\n")
	b.WriteString("Reply `/reset confirm` to proceed.")

	o.postResetReply(ev.SessionID, b.String())
}

// executeReset runs the actual reset (the /reset confirm path) and reports the
// outcome back into the Linear thread.
func (o *Orchestrator) executeReset(ev linear.AgentEvent) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	gh := func(ctx context.Context, args ...string) (string, error) {
		return captureCmd(ctx, "", o.cfg.GhBin, args...)
	}
	resp, err := runResetTask(ctx, o.lc, gh, o.db, o.verifyLabel, o.logger, ev.IssueID)
	if err != nil {
		switch {
		case errors.Is(err, errResetMergedPR):
			o.postRejection(ev.SessionID, fmt.Sprintf(
				"admiral: reset refused — %v. Undo the merge on GitHub first if you really mean to reset this task.", err))
		case errors.Is(err, errResetParentNotFound):
			o.postRejection(ev.SessionID, fmt.Sprintf("admiral: issue %s not found.", ev.IssueIdentifier))
		default:
			o.logger.Warn("dispatch_reset_failed", "issue", ev.IssueIdentifier, "err", err)
			o.postRejection(ev.SessionID, fmt.Sprintf("admiral: reset failed: %v", err))
		}
		return
	}

	var b strings.Builder
	fmt.Fprintf(&b, "✅ Reset task %s — %d sub-issue(s) returned to backlog; PRs closed, branches deleted, admiral state wiped.\n",
		resp.Parent, resp.SubsReset)
	for _, s := range resp.Subs {
		if s.Warning != "" {
			fmt.Fprintf(&b, "  ⚠️ %s: %s\n", s.Identifier, s.Warning)
		}
	}
	if resp.Warning != "" {
		fmt.Fprintf(&b, "  ⚠️ %s\n", resp.Warning)
	}
	b.WriteString("\nRe-arm it with /activate (or re-assign) when you're ready to ship again.")

	o.postResetReply(ev.SessionID, b.String())
}

// postResetReply posts a successful (non-error, terminal) Response activity for
// the /reset preview and result. Distinct from postRejection (ErrorActivity),
// which would mark the AgentSession as errored.
func (o *Orchestrator) postResetReply(sessionID, body string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = o.lc.PostAgentActivity(ctx, sessionID, linear.Response(body))
}
