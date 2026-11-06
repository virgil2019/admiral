package autopilot

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/georgehuang/admiral/internal/linear"
	"github.com/georgehuang/admiral/internal/store"
)

// maxAutoResumes caps how many times the reconciler resumes one task. A task
// that kills the daemon on every attempt must escalate to a human instead of
// crash-looping forever.
const maxAutoResumes = 2

// ReconcileInterval is how often the reconciler sweeps for orphaned work.
const ReconcileInterval = 5 * time.Minute

// reconcileGrace is how long a row must have existed before the sweep trusts
// its in-flight state. Dispatch does Linear I/O (blocker checks, notices)
// before beginRun registers the run, so a row can legitimately look
// unowned for a few seconds.
const reconcileGrace = 2 * time.Minute

// orphanOutcome is tryResumeOrphan's result.
type orphanOutcome int

const (
	orphanEscalated orphanOutcome = iota
	orphanResumed
	orphanDeferred
)

// ReconcileReport summarizes one sweep.
type ReconcileReport struct {
	// Orphans are in-flight rows no live run in this process owns.
	Orphans int
	// Resumed rows were handed back to the resume flow.
	Resumed int
	// Escalated rows were reported to their Linear thread for a human —
	// no recoverable session, or the auto-resume cap was reached.
	Escalated int
	// Deferred rows were left for the next sweep (too fresh to judge, or a
	// transient store error). Not a terminal outcome.
	Deferred int
}

// Reconcile is the internal wakeup: it sweeps for work that no live run is
// driving and either resumes it or asks a human.
//
// Why it is needed even with a clean shutdown: a SIGKILL, an OOM, or a
// panic outside a guarded goroutine leaves a row in RECEIVED/EXECUTING with
// nothing to move it, and every subsequent /rerun or /fix on that issue is
// rejected as "currently processing" — the issue is stuck until someone
// edits the database. The sweep makes that state self-healing.
//
// A row counts as orphaned when it is in RECEIVED/EXECUTING and this process
// has no run for its issue id: single-instance deployment makes ownership
// unambiguous. On boot the in-memory set is empty, so every leftover row is
// orphaned by definition.
func (o *Orchestrator) Reconcile(ctx context.Context) ReconcileReport {
	var rep ReconcileReport

	rows, err := o.db.ListRecoverableTasks()
	if err != nil {
		o.logger.Warn("reconcile_list_failed", "err", err)
		return rep
	}

	for i := range rows {
		if ctx.Err() != nil {
			// Shutting down: launching a resume now would fail in its
			// prepare phase and overwrite the interrupted marker with an
			// error the next sweep does not recognise.
			return rep
		}
		row := rows[i]
		if o.RunInFlight(row.IssueID) {
			continue // a live run owns it; leave it alone
		}
		// A row younger than reconcileGrace may be between its state write
		// and beginRun (dispatch pre-flight does Linear I/O first). Judging
		// it now would escalate work that never stopped.
		if startedAt, err := time.Parse(time.RFC3339, row.StartedAt); err == nil {
			if time.Since(startedAt) < reconcileGrace {
				rep.Deferred++
				continue
			}
		}
		rep.Orphans++

		// A clean shutdown already stamped the row interrupted; a crash left
		// it in flight and needs the stamp now.
		if row.State != store.JobStateTimedOut {
			changed, err := o.db.MarkTaskInterrupted(row.IssueID, store.InterruptedByShutdown)
			if err != nil {
				o.logger.Warn("reconcile_mark_failed", "issue", row.IssueIdentifier, "err", err)
				rep.Deferred++
				continue
			}
			if !changed {
				continue // a live run finished it between the read and the write
			}
		}
		row.State = store.JobStateTimedOut
		row.Error = store.InterruptedByShutdown

		switch o.tryResumeOrphan(ctx, &row) {
		case orphanResumed:
			rep.Resumed++
		case orphanDeferred:
			rep.Deferred++
		default:
			rep.Escalated++
		}
	}

	if rep.Orphans > 0 || rep.Deferred > 0 {
		o.logger.Info("reconcile_swept",
			"orphans", rep.Orphans, "resumed", rep.Resumed,
			"escalated", rep.Escalated, "deferred", rep.Deferred)
	}
	return rep
}

// StartReconciler runs one sweep immediately (boot recovery) and then on a
// ticker until ctx is done.
func (o *Orchestrator) StartReconciler(ctx context.Context) {
	if rep := o.Reconcile(ctx); rep.Orphans > 0 {
		o.logger.Info("reconcile_boot_recovery",
			"orphans", rep.Orphans, "resumed", rep.Resumed, "escalated", rep.Escalated)
	}
	t := time.NewTicker(ReconcileInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			o.Reconcile(ctx)
		}
	}
}

// tryResumeOrphan hands the row back to the resume flow when that can work,
// and otherwise escalates to the Linear thread. Returns true when a resume
// was started.
func (o *Orchestrator) tryResumeOrphan(ctx context.Context, row *store.AdmiralTask) orphanOutcome {
	canResume := row.ClaudeSessionID != "" && row.LastEventSessionID != "" &&
		row.WorktreePath != ""
	if canResume {
		if _, err := os.Stat(row.WorktreePath); err != nil {
			canResume = false
		}
	}

	if canResume {
		n, err := o.db.IncrementAutoResumeCount(row.IssueID, row.AttemptN)
		if err != nil {
			// A store hiccup is not "the cap was reached" — escalating here
			// would demote a resumable task to manual recovery (and the row
			// stays recoverable, so the next sweep retries).
			o.logger.Warn("reconcile_resume_count_failed", "issue", row.IssueIdentifier, "err", err)
			return orphanDeferred
		}
		if n <= maxAutoResumes {
			ev := linear.AgentEvent{
				SessionID:       row.LastEventSessionID,
				IssueID:         row.IssueID,
				IssueIdentifier: row.IssueIdentifier,
			}
			o.logger.Info("reconcile_resuming",
				"issue", row.IssueIdentifier, "attempt_n", row.AttemptN, "auto_resume", n)
			go func() {
				// Ownership is re-checked first: a /rerun can supersede the
				// row between the sweep's read and this launch, and telling
				// the thread "resuming" for a resume we then abandon is a
				// promise nobody keeps.
				if !o.stillResumable(*row) {
					o.logger.Info("reconcile_resume_superseded",
						"issue", row.IssueIdentifier, "attempt_n", row.AttemptN)
					return
				}
				o.postToTaskThread(row, linear.Response(fmt.Sprintf(
					"admiral restarted mid-run and is resuming this task (attempt %d, automatic resume %d/%d).",
					row.AttemptN, n, maxAutoResumes)))
				o.resumeOrphanRun(ev, row)
			}()
			return orphanResumed
		}
	}

	// Cannot resume (or the cap is spent): say so where a human is looking,
	// with the command that recovers.
	var reason string
	switch {
	case row.ClaudeSessionID == "" || row.LastEventSessionID == "":
		reason = "the run was interrupted and has no recoverable claude session"
	case row.WorktreePath == "":
		reason = "the run was interrupted and its worktree is no longer recorded"
	case !canResume:
		reason = "the run was interrupted and its worktree is gone"
	default:
		reason = fmt.Sprintf("the run was interrupted and automatic resume was already used %d times", maxAutoResumes)
	}
	o.logger.Warn("reconcile_escalate", "issue", row.IssueIdentifier, "reason", reason)
	posted := o.postToTaskThread(row, linear.Response(fmt.Sprintf(
		"⚠️ admiral stopped this task mid-run (%s).\n\nWorktree: `%s`\nBranch: `%s`\nPR: %s\n\nReply `/rerun` on the issue to start a fresh attempt.",
		reason, row.WorktreePath, row.Branch, row.PRURL)))

	// Only leave the recoverable set once the human was actually told —
	// marking first would drop the row from every later sweep even when both
	// delivery paths failed (Linear restarting is the common case).
	if posted {
		if err := o.db.MarkTaskUnrecoverable(row.IssueID, reason); err != nil {
			o.logger.Warn("reconcile_mark_unrecoverable_failed",
				"issue", row.IssueIdentifier, "err", err)
		}
	}
	return orphanEscalated
}

// stillResumable re-checks the row right before a queued resume starts. The
// sweep reads rows, then launches a goroutine that blocks on the run slot —
// and a /rerun drained in that window supersedes the row (new attempt, no
// auto_resume row). Resuming then would run the OLD branch/worktree while
// writing EXECUTING/DONE onto the NEW attempt's row.
func (o *Orchestrator) stillResumable(row store.AdmiralTask) bool {
	cur, err := o.db.GetAdmiralTaskByIssue(row.IssueID)
	if err != nil || cur == nil {
		return false
	}
	return cur.AttemptN == row.AttemptN &&
		cur.State == store.JobStateTimedOut &&
		cur.Branch == row.Branch
}

// postToTaskThread posts an activity on the task's Linear agent session
// thread. When no session is recorded (a task created by a path that never
// stored one) the message goes onto the issue itself instead: an escalation
// that lands nowhere is the silent failure this whole sweep exists to
// prevent.
func (o *Orchestrator) postToTaskThread(task *store.AdmiralTask, a linear.AgentActivity) bool {
	if task == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if task.LastEventSessionID != "" {
		if err := o.lc.PostAgentActivity(ctx, task.LastEventSessionID, a); err == nil {
			return true
		} else {
			o.logger.Warn("task_thread_post_failed",
				"issue", task.IssueIdentifier, "err", err)
		}
	}
	if task.IssueID == "" {
		return false
	}
	if err := o.lc.CreateComment(ctx, task.IssueID, a.Body); err != nil {
		o.logger.Error("task_issue_comment_failed",
			"issue", task.IssueIdentifier, "err", err)
		return false
	}
	return true
}
