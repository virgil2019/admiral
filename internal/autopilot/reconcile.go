package autopilot

import (
	"context"
	"errors"
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

	var pending []orphanEscalation
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

		reason, canResume := resumability(row)
		if !canResume {
			// Collect and escalate AFTER the loop: a Linear post blocks for
			// its whole timeout, and a host restart commonly takes Linear
			// down with it — serialising 30s per orphan would starve the
			// healthy rows behind them.
			rc := row
			pending = append(pending, orphanEscalation{row: &rc, reason: reason})
			rep.Escalated++
			continue
		}
		if blocked, berr := o.issueBlocked(ctx, row.IssueID); berr != nil {
			o.logger.Warn("reconcile_blocker_check_failed",
				"issue", row.IssueIdentifier, "err", berr)
			rep.Deferred++
			continue
		} else if blocked {
			// A dependency added while the daemon was down still holds the
			// task back. Defer instead of parking: the blocker watcher
			// resumes a parked row with a FRESH run (createWorktree wipes
			// the preserved worktree and branch), which would destroy the
			// very work this sweep is trying to continue.
			o.logger.Info("reconcile_resume_blocked", "issue", row.IssueIdentifier)
			rep.Deferred++
			continue
		}
		o.launchResume(ctx, &row)
		rep.Resumed++
	}

	for _, e := range pending {
		o.escalateOrphan(e.row, e.reason)
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

// orphanEscalation is an orphan that needs a human, deferred until every
// resumable row has been launched.
type orphanEscalation struct {
	row    *store.AdmiralTask
	reason string
}

// resumability reports whether the reconciler can continue this row, and the
// human-facing reason when it cannot.
func resumability(row store.AdmiralTask) (string, bool) {
	switch {
	case row.ClaudeSessionID == "" || row.LastEventSessionID == "":
		return "the run was interrupted and has no recoverable claude session", false
	case row.WorktreePath == "":
		return "the run was interrupted and its worktree is no longer recorded", false
	}
	if _, err := os.Stat(row.WorktreePath); err != nil {
		return "the run was interrupted and its worktree is gone", false
	}
	return "", true
}

// issueBlocked is a read-only blocked_by check. It deliberately does not use
// parkIfBlocked: parking hands the row to the blocker watcher, which resumes
// with a fresh run and wipes the preserved worktree.
func (o *Orchestrator) issueBlocked(ctx context.Context, issueID string) (bool, error) {
	bctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	blockers, err := o.lc.GetIssueBlockers(bctx, issueID)
	if err != nil {
		return false, err
	}
	return len(blockers) > 0, nil
}

// launchResume queues the resume. Every side effect — the credit, the notice,
// the resume itself — happens in the goroutine and only after ownership is
// re-verified: a /rerun can supersede the row at any point, and spending a
// credit or promising a resume for a row we no longer own is a silent lie.
func (o *Orchestrator) launchResume(ctx context.Context, row *store.AdmiralTask) {
	ev := linear.AgentEvent{
		SessionID:       row.LastEventSessionID,
		IssueID:         row.IssueID,
		IssueIdentifier: row.IssueIdentifier,
	}
	go func() {
		if !o.stillResumable(*row) {
			o.logger.Info("reconcile_resume_superseded",
				"issue", row.IssueIdentifier, "attempt_n", row.AttemptN)
			return
		}
		n, err := o.db.IncrementAutoResumeCount(row.IssueID, row.AttemptN)
		if err != nil {
			if errors.Is(err, store.ErrNoSuchAttempt) {
				o.logger.Info("reconcile_resume_superseded",
					"issue", row.IssueIdentifier, "attempt_n", row.AttemptN)
			} else {
				// A store hiccup is not "the cap was reached"; the row stays
				// recoverable and the next sweep retries.
				o.logger.Warn("reconcile_resume_count_failed",
					"issue", row.IssueIdentifier, "err", err)
			}
			return
		}
		if n > maxAutoResumes {
			o.escalateOrphan(row, fmt.Sprintf(
				"the run was interrupted and automatic resume was already used %d times", maxAutoResumes))
			return
		}
		o.logger.Info("reconcile_resuming",
			"issue", row.IssueIdentifier, "attempt_n", row.AttemptN, "auto_resume", n)
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
}

// escalateOrphan tells a human that this row needs them, and only then stops
// sweeping it — marking first would drop the row from every later sweep even
// when both delivery paths failed (Linear restarting is the common case).
func (o *Orchestrator) escalateOrphan(row *store.AdmiralTask, reason string) {
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
