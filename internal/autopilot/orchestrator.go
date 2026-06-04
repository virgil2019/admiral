// Package autopilot is the orchestrator: pick up a Linear
// AgentSessionEvent, create a worktree, run `claude -p`, ensure a PR was
// opened, post agent activities back into the Linear agent thread.
//
// Concurrency model (post-GEO-50 + GEO-51):
//
//   - Per-issue: strictly serial. The dispatch state machine on the live
//     admiral_tasks row rejects /rerun and /fix when a prior run is still
//     in flight (RECEIVED / EXECUTING).
//   - Cross-issue: bounded parallelism via the runSlots semaphore. The
//     ceiling is autopilot.max_concurrent_runs (default 3, override via
//     ADMIRAL_MAX_CONCURRENT_RUNS env). When the ceiling is full the
//     spawn goroutine blocks on acquire — events are not rejected; they
//     wait in a goroutine each.
//   - Webhook → run handoff: worker.drain claims one events_inbox row at
//     a time, calls HandleAgentEvent (which spawns and returns), then
//     claims the next. The semaphore inside the run goroutines does the
//     real backpressure.
package autopilot

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/georgehuang/admiral/internal/config"
	ghpkg "github.com/georgehuang/admiral/internal/github"
	"github.com/georgehuang/admiral/internal/linear"
	"github.com/georgehuang/admiral/internal/store"
	"github.com/google/uuid"
)

// storeInterface abstracts the store methods used by the orchestrator.
//
// admiral_tasks methods (PR-B-v2 source of truth for issue-level state):
//
//	GetAdmiralTaskByIssue, ClaimAdmiralTask, UpdateAdmiralTask,
//	MoveAdmiralTaskToHistoryAndClaimNew
//
// autopilot_jobs methods are kept for the audit-log dual-write in
// flow.execute and for legacy queries; PR-C will drop the unused ones.
type storeInterface interface {
	AnyAutopilotJobActive() (bool, string, error)
	GetLastAutopilotJob() (*store.AutopilotJob, error)
	GetAutopilotJob(sessionID string) (*store.AutopilotJob, error)
	UpdateAutopilotJob(sessionID string, fn func(*store.AutopilotJob)) error
	ClaimAutopilotJob(sessionID, issueID, identifier string) (bool, error)
	GetLatestDoneJobByIssue(issueID string) (*store.AutopilotJob, error)
	GetLatestTimedOutJobByIssue(issueID string) (*store.AutopilotJob, error)
	FindActiveJobByIssue(issueID, excludeSessionID string) (*store.AutopilotJob, error)
	GetRepoByProjectID(projectID string) (*store.Repo, error)
	ListJobsByIssueAndStates(issueID string, states []string) ([]store.AutopilotJob, error)
	HasAnyAutopilotJobForIssue(issueID string) (bool, error)

	GetAdmiralTaskByIssue(issueID string) (*store.AdmiralTask, error)
	GetAdmiralTaskByPRURL(prURL string) (*store.AdmiralTask, error)
	ClaimAdmiralTask(issueID, identifier, lastEventSessionID string) (bool, error)
	UpdateAdmiralTask(issueID string, fn func(*store.AdmiralTask)) error
	MoveAdmiralTaskToHistoryAndClaimNew(issueID, reason, identifier, lastEventSessionID string) (int, error)
	SetAdmiralTaskBlocked(issueID, blockerIDs string) error
	GetBlockedAdmiralTasks() ([]store.BlockedTask, error)
	TransitionBlockedToReceived(issueID string) (bool, error)

	InsertPendingQuestion(q store.PendingQuestion) error
	GetOpenPendingQuestionByIssue(issueID string) (*store.PendingQuestion, error)
	GetPendingQuestionByID(id string) (*store.PendingQuestion, error)
	AnswerPendingQuestion(id, answer string) error
	CancelOpenPendingQuestionsForIssue(issueID string) error
	SetAdmiralTaskAwaitingInput(issueID, pendingQuestionID string) error
	TransitionAwaitingInputToExecuting(issueID string) (bool, error)

	// task_verifications: the autonomous L2 verification loop (C4).
	GetTaskVerification(parentIssueID string) (*store.TaskVerification, error)
	BumpTaskVerificationRound(parentIssueID string) (*store.TaskVerification, error)
	SetTaskVerificationStatus(parentIssueID, status string) error

	// /reset command (PR-B): cascade-delete a task's per-issue + verify rows.
	ResetIssueRows(issueID string) error
	DeleteTaskVerification(parentIssueID string) error
}

// linearClientInterface abstracts the linear client methods used by the orchestrator.
type linearClientInterface interface {
	PostAgentActivity(ctx context.Context, sessionID string, a linear.AgentActivity) error
	GetIssue(ctx context.Context, id string) (*linear.Issue, error)
	GetWorkflowStates(ctx context.Context, teamID string) ([]linear.WorkflowState, error)
	IssueUpdate(ctx context.Context, issueID, stateID string) error
	GetIssueBlockers(ctx context.Context, issueID string) ([]linear.IssueBlocker, error)

	// Verify loop (C4): enumerate a task's sub-issues, file follow-up gaps
	// as sub-issues, and escalate to a human via a parent-issue comment.
	GetSubIssues(ctx context.Context, parentID string) ([]linear.SubIssue, error)
	IssueCreate(ctx context.Context, in linear.IssueCreateInput) (*linear.Issue, error)
	GetTeamLabelID(ctx context.Context, teamID, name string) (string, error)
	CreateComment(ctx context.Context, issueID, body string) error

	// /reset command (PR-B): drop the require_label from a sub-issue and
	// clear its assignee so the discoverer can re-pick it after re-activation.
	RemoveIssueLabel(ctx context.Context, issueID, labelID string) error
	UnassignIssue(ctx context.Context, issueID string) error
}

type Orchestrator struct {
	cfg    *config.Autopilot
	lc     linearClientInterface
	db     storeInterface
	gh     ghProbe
	logger *slog.Logger
	ghUser string // login of the configured gh user (for open-PR author comparison)

	// runSlots gates how many `claude -p` runs can be live concurrently
	// across all issues (GEO-51). Acquired in runWithAttempt and runFix,
	// released on return. Capacity = cfg.MaxConcurrentRuns (default 3).
	// A buffered chan struct{} is the idiomatic Go counted-semaphore.
	runSlots chan struct{}

	// workflowStatesByTeam caches workflow states per team, keyed by teamID.
	workflowStatesByTeam map[string][]linear.WorkflowState
	workflowStatesMu     sync.Mutex

	// ciWatcher polls GitHub check runs after a PR is opened and reports
	// results back into the Linear thread (GEO-54).
	ciWatcher *CIWatcher

	// blockerWatcher polls BLOCKED tasks and re-queues them once all their
	// Linear blocked_by relations are resolved.
	blockerWatcher *BlockerWatcher

	// replier is the semantic reply layer for Linear AgentSession threads.
	replier *agentSessionReplier

	// prClient is the outbound GitHub PR client used by the review dispatcher.
	prClient ghpkg.PRClient

	// dbPath is the absolute path to the SQLite file. Passed as
	// ADMIRAL_DB_PATH to the admiral-mcp-ask subprocess.
	dbPath string

	// verifyLabel / verifyStateTypes are the discoverer's pickup gates,
	// plumbed in via SetVerifyPickupRules so follow-up sub-issues the verify
	// loop files are auto-picked and re-shipped (single source of truth with
	// the discoverer — no drift). Empty verifyLabel means no label is set.
	verifyLabel      string
	verifyStateTypes []string

	// verifyRunner runs the headless verify judge. Defaults to
	// runClaudeForVerify; tests inject a stub so the guard/apply logic is
	// exercised without spawning a real claude process.
	verifyRunner func(ctx context.Context, repoDir, prompt string) (string, error)
}

// SetVerifyPickupRules wires the discoverer's pickup gates (require_label +
// state_types) into the orchestrator so the verify loop's follow-up
// sub-issues are created in a pickable state with the right label. Called
// once from main after New. The orchestrator only holds *config.Autopilot,
// so these cross-component values are injected rather than read from cfg.
func (o *Orchestrator) SetVerifyPickupRules(label string, stateTypes []string) {
	o.verifyLabel = label
	o.verifyStateTypes = stateTypes
}

func New(cfg *config.Autopilot, lc *linear.Client, db *store.Store, logger *slog.Logger) *Orchestrator {
	slots := cfg.MaxConcurrentRuns
	if slots <= 0 {
		// Defensive: config defaulting normally pins this to 3, but if a
		// caller constructs Autopilot{} directly we still want a sane cap.
		slots = 3
	}
	o := &Orchestrator{
		cfg:      cfg,
		lc:       lc,
		db:       db,
		dbPath:   db.Path,
		gh:       newGhCLIProbe(cfg.GhBin),
		logger:   logger,
		ghUser:   cfg.GhUser,
		runSlots: make(chan struct{}, slots),
		ciWatcher: newCIWatcher(lc, db, logger, cfg.GhBin,
			cfg.CIWatchPollInterval, cfg.CIWatchTimeout),
		replier:  NewAgentSessionReplier(lc),
		prClient: ghpkg.NewClient(cfg.GhToken),
	}
	o.verifyRunner = func(ctx context.Context, repoDir, prompt string) (string, error) {
		return runClaudeForVerify(ctx, o.cfg.ClaudeBin, o.cfg.MaxRunSeconds, repoDir, prompt, o.logger)
	}
	o.blockerWatcher = newBlockerWatcher(o, cfg.BlockerPollInterval)
	// Ensure job_streams_dir exists on startup.
	if err := os.MkdirAll(cfg.JobStreamsDir, 0o755); err != nil {
		logger.Warn("job_streams_dir_mkdir", "dir", cfg.JobStreamsDir, "err", err)
	}
	return o
}

// acquireRunSlot blocks until a slot is available in the runSlots
// semaphore. Returns a release func the caller must invoke (typically
// via defer) when the run finishes — even on error or panic.
//
// A nil runSlots (e.g. tests that construct Orchestrator directly
// instead of via New) skips the bound and returns a no-op release.
// Production always has a bounded chan.
func (o *Orchestrator) acquireRunSlot() func() {
	if o.runSlots == nil {
		return func() {}
	}
	o.runSlots <- struct{}{}
	return func() { <-o.runSlots }
}

// stateIDByType returns the workflow state ID for the given type in the given
// team. It uses an in-memory cache;首次调用会从Linear拉取.
func (o *Orchestrator) stateIDByType(ctx context.Context, teamID, stateType string) (string, error) {
	o.workflowStatesMu.Lock()
	states, ok := o.workflowStatesByTeam[teamID]
	if !ok {
		var err error
		states, err = o.lc.GetWorkflowStates(ctx, teamID)
		if err != nil {
			o.workflowStatesMu.Unlock()
			return "", fmt.Errorf("get_workflow_states: %w", err)
		}
		if o.workflowStatesByTeam == nil {
			o.workflowStatesByTeam = make(map[string][]linear.WorkflowState)
		}
		o.workflowStatesByTeam[teamID] = states
	}
	o.workflowStatesMu.Unlock()

	var bestID string
	var bestPos float64 = -1
	for _, s := range states {
		if s.Type == stateType {
			if bestPos < 0 || s.Position < bestPos {
				bestPos = s.Position
				bestID = s.ID
			}
		}
	}
	return bestID, nil
}

// HandleAgentEvent is wired up as the linear.AgentHandler. Returns quickly:
// the actual run happens on a background goroutine.
func (o *Orchestrator) HandleAgentEvent(ev linear.AgentEvent) {
	if ev.Action != linear.ActionCreated && ev.Action != linear.ActionPrompted {
		o.logger.Warn("autopilot_unknown_action", "action", ev.Action)
		return
	}
	o.dispatch(ev)
}

// dispatch is the unified entry point per the GEO-50 model. admiral
// treats each Linear issue as a single task: at most one live row per
// issue. Events on the issue either initiate the task (first-time
// assign) or send a /command to it; everything else is rejected without
// modifying state. The two-path handleCreated/handlePrompted split is
// gone — same rules apply to both.
//
// PR-B-v2: admiral_tasks is now the source of truth. The live task row
// for an issue (if any) drives all dispatch decisions including
// state-aware /rerun. autopilot_jobs is still written by flow.execute
// as audit log; PR-C drops that write.
//
// Also gone: GEO-38's auto-resume on follow-up @mention, GEO-37's
// timed-out auto-resume. Both behaviors are subsumed by explicit
// /rerun (start fresh) or /fix (resume current PR; coming in GEO-49).
// admiral no longer guesses what the user meant — they say it.
func (o *Orchestrator) dispatch(ev linear.AgentEvent) {
	text := ev.PromptContext
	if text == "" && ev.Action == linear.ActionPrompted {
		text = ev.UserMessage
	}

	o.logger.Info("dispatch",
		"session", ev.SessionID,
		"issue", ev.IssueIdentifier,
		"action", ev.Action,
		"text_preview", truncate(text, 100),
	)

	if ev.IssueID == "" {
		o.logger.Warn("dispatch_skip_no_issue_id", "session", ev.SessionID)
		return
	}

	// Delegate vs @mention: a created event with no SourceCommentID is a
	// delegate (assign-to-agent), regardless of whether the user typed an
	// initial prompt. SourceCommentID set means the session was opened
	// from an @mention inside a comment — parse it as a command on a live
	// task, or reject as "assign first" if there's no live task yet.
	isDelegate := ev.Action == linear.ActionCreated && ev.SourceCommentID == ""

	// /reset is a parent-level command: it resets a whole task (parent + all
	// sub-issues), and the parent issue usually has no admiral_tasks row of its
	// own. Intercept it here, before the task==nil gate below would reject a
	// command on a row-less parent. A genuine delegate/assign is never /reset.
	//
	// This fires for any @mention whose first token is /reset, including on a
	// sub-issue (where it'll report "no sub-issues") or an AWAITING_INPUT task
	// (where a reply literally starting with /reset is treated as the command,
	// not the answer). Both are acceptable: /reset is always a command, and
	// mentioning it anywhere but the parent task is a no-op-with-explanation.
	if !isDelegate {
		if name, remainder, ok := parseMentionCommand(text); ok && name == "reset" {
			o.dispatchReset(ev, remainder)
			return
		}
	}

	task, err := o.db.GetAdmiralTaskByIssue(ev.IssueID)
	if err != nil {
		o.logger.Error("dispatch_lookup_failed", "err", err, "issue", ev.IssueID)
		return
	}

	if task == nil {
		// First-time event for this issue.
		if isDelegate {
			o.dispatchFreshAssign(ev)
			return
		}
		o.logger.Info("dispatch_reject_no_task",
			"session", ev.SessionID, "issue", ev.IssueIdentifier)
		o.postRejection(ev.SessionID, assignFirstHelp)
		return
	}

	// Live task exists. Update last_event_session_id so future replies
	// land on the current Linear session even if the user opened a new
	// AgentSession for this comment.
	if task.LastEventSessionID != ev.SessionID {
		_ = o.db.UpdateAdmiralTask(ev.IssueID, func(t *store.AdmiralTask) {
			t.LastEventSessionID = ev.SessionID
		})
	}

	if isDelegate {
		o.logger.Info("dispatch_reject_repeat_assign",
			"session", ev.SessionID, "issue", ev.IssueIdentifier)
		o.replyRejection(ev.SessionID, task.State, repeatAssignHelp)
		return
	}

	cmdName, remainder, ok := parseMentionCommand(text)
	if !ok {
		// A bare message (no /command) on an AWAITING_INPUT task is the
		// user's reply to the pending ask_user question.
		if task.State == store.JobStateAwaitingInput {
			o.dispatchAwaitingReply(ev, task, text)
			return
		}
		o.logger.Info("dispatch_reject_bare",
			"session", ev.SessionID, "issue", ev.IssueIdentifier)
		o.replyRejection(ev.SessionID, task.State, mentionCommandHelp)
		return
	}

	switch cmdName {
	case "rerun":
		o.dispatchRerun(ev, task, remainder)
	case "fix":
		o.dispatchFix(ev, task, remainder)
	case "resume":
		o.dispatchResume(ev, task)
	case "status", "help":
		o.handleCommand(ev, cmdName)
	default:
		o.logger.Info("autopilot_reject_unknown_command",
			"session", ev.SessionID, "issue", ev.IssueIdentifier, "command", "/"+cmdName)
		o.replyRejection(ev.SessionID, task.State, unknownCommandHelp("/"+cmdName))
	}
}

// replyRejection chooses between postRejection (ErrorActivity, terminal) and
// postBusyAck (Thought, non-terminal) based on whether the task is still in
// flight. Used by dispatch sites that may fire on either a live or terminal
// task (bare @mention, unknown /command, re-toggled delegate).
func (o *Orchestrator) replyRejection(sessionID, taskState, body string) {
	if taskInFlight(taskState) {
		o.postBusyAck(sessionID, body)
		return
	}
	o.postRejection(sessionID, body)
}

// StartBlockerWatcher starts the background loop that re-queues BLOCKED tasks
// once their Linear blocked_by relations are resolved. Call once from main.
func (o *Orchestrator) StartBlockerWatcher(ctx context.Context) {
	o.blockerWatcher.Run(ctx)
}

// dispatchFreshAssign claims a fresh admiral_tasks row for the issue,
// checks for unresolved Linear blocked_by relations, and either parks the
// task as BLOCKED (with an automatic re-queue via BlockerWatcher) or spawns
// the run goroutine immediately.
func (o *Orchestrator) dispatchFreshAssign(ev linear.AgentEvent) {
	fresh, err := o.db.ClaimAdmiralTask(ev.IssueID, ev.IssueIdentifier, ev.SessionID)
	if err != nil {
		o.logger.Error("claim_admiral_task_failed",
			"err", err, "issue", ev.IssueID)
		return
	}
	if !fresh {
		o.logger.Info("dispatch_race_claim_lost",
			"session", ev.SessionID, "issue", ev.IssueIdentifier)
		return
	}

	if o.parkIfBlocked(ev) {
		return
	}

	go o.run(ev)
}

// parkIfBlocked checks for unresolved Linear blocked_by relations on ev's
// issue. If any exist it parks the live admiral_tasks row as BLOCKED (the
// BlockerWatcher resumes it once the blockers clear) and returns true — the
// caller must NOT spawn a run. On a Linear API error it logs and returns false
// (fail-open) so a transient hiccup does not permanently stall the task.
//
// Shared by the fresh-assign and /rerun paths so re-running an issue respects
// the same dependency gate as first dispatch. Relies on the live row already
// being claimed (fresh assign) or freshly superseded (/rerun) so
// SetAdmiralTaskBlocked targets the right attempt; the BlockerWatcher resumes
// with that row's attempt_n, preserving the rerun branch naming.
func (o *Orchestrator) parkIfBlocked(ev linear.AgentEvent) bool {
	bctx, bcancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer bcancel()
	blockers, err := o.lc.GetIssueBlockers(bctx, ev.IssueID)
	if err != nil {
		o.logger.Warn("blocker_check_failed_proceeding",
			"issue", ev.IssueIdentifier, "err", err)
		return false
	}
	if len(blockers) == 0 {
		return false
	}
	o.logger.Info("dispatch_blocked",
		"issue", ev.IssueIdentifier, "blockers", blockerIdentifiers(blockers))
	if setErr := o.db.SetAdmiralTaskBlocked(ev.IssueID, blockerIDsJSON(blockers)); setErr != nil {
		o.logger.Error("set_blocked_failed", "issue", ev.IssueIdentifier, "err", setErr)
	}
	// Use Thought (non-terminal) so the AgentSession stays alive for the
	// watcher's follow-up "resuming now" post when blockers are resolved.
	o.postBlockedNotice(ev.SessionID, blockedMessage(blockers))
	return true
}

// dispatchRerun handles /rerun on an existing admiral_tasks row.
// State machine:
//
//	RECEIVED / EXECUTING (in-flight) → reject "currently processing"
//	DONE / FAILED / TIMED_OUT / CANCELLED → supersede the live row
//	  (atomic move to history + claim a fresh attempt_n+1 row), spawn
//	  a fresh run on a new branch named linear/<id>-rerun-<N>.
//
// Works for both @mention (created) and thread (prompted) triggers
// because admiral_tasks is keyed by issue_id, not by Linear's reused
// AgentSession.id. The new run gets a synthesised audit row id for
// autopilot_jobs (still SessionID-keyed for now); thread replies use
// ev.SessionID via PostAgentActivity as before.
func (o *Orchestrator) dispatchRerun(ev linear.AgentEvent, task *store.AdmiralTask, notes string) {
	switch task.State {
	case store.JobStateReceived, store.JobStateExecuting:
		o.logger.Info("dispatch_reject_rerun_currently_processing",
			"session", ev.SessionID,
			"issue", ev.IssueIdentifier,
			"prior_state", task.State,
			"attempt_n", task.AttemptN)
		// Task is in flight by definition of this case branch — Thought
		// keeps the live AgentSession alive for the running flow.
		o.postBusyAck(ev.SessionID, rerunCurrentlyProcessingHelp(ev.IssueIdentifier))
		return
	case store.JobStateBlocked:
		o.logger.Info("dispatch_reject_rerun_blocked",
			"session", ev.SessionID, "issue", ev.IssueIdentifier)
		// Thought keeps the session alive; watcher will auto-resume when blockers clear.
		o.postBusyAck(ev.SessionID, "admiral is waiting for blockers to resolve. Use /status to check; will resume automatically.")
		return
	case store.JobStateAwaitingInput:
		// Cancel the open pending question so the superseded row doesn't match
		// a future reply to the new attempt.
		if err := o.db.CancelOpenPendingQuestionsForIssue(ev.IssueID); err != nil {
			o.logger.Warn("dispatch_rerun_cancel_pending_q_failed",
				"err", err, "issue", ev.IssueID)
		}
		// Fall through to the normal supersession path below.
	}

	newAttempt, err := o.db.MoveAdmiralTaskToHistoryAndClaimNew(
		ev.IssueID, "superseded_by_rerun", ev.IssueIdentifier, ev.SessionID,
	)
	if err != nil {
		o.logger.Error("rerun_supersede_failed",
			"err", err, "issue", ev.IssueID)
		o.postRejection(ev.SessionID, "Internal error processing /rerun. Try again or assign the issue manually.")
		return
	}
	o.logger.Info("dispatch_rerun_superseded",
		"session", ev.SessionID,
		"issue", ev.IssueIdentifier,
		"new_attempt_n", newAttempt,
		"prior_pr", task.PRURL)

	// Fold rerun notes into PromptContext so the fresh claude run sees
	// what the user actually wants this time.
	rerunEv := ev
	if notes != "" {
		rerunEv.PromptContext = notes
	} else {
		rerunEv.PromptContext = ""
	}

	// Re-running must respect blockers just like a fresh assign: a task whose
	// dependencies aren't done yet should park BLOCKED, not ship out of order.
	// The just-claimed attempt-n+1 row is parked; the BlockerWatcher resumes it
	// (with this attempt_n) once the blockers clear.
	if o.parkIfBlocked(rerunEv) {
		o.logger.Info("dispatch_rerun_parked_blocked",
			"issue", ev.IssueIdentifier, "new_attempt_n", newAttempt)
		return
	}

	go o.runWithAttempt(rerunEv, newAttempt)
}

// dispatchFix handles /fix on an existing admiral_tasks row. State machine:
//
//	RECEIVED / EXECUTING (in-flight) → reject "currently processing"
//	FAILED / TIMED_OUT / CANCELLED   → reject + suggest /rerun
//	DONE                             → resume the prior claude session,
//	  push fix commits onto the same branch, no new PR. Same admiral_tasks
//	  row stays — its state cycles DONE → EXECUTING → DONE on success.
//
// Requires task.PRURL and task.ClaudeSessionID populated. If either is
// missing (legacy row pre-claude-session-tracking), reject with help.
func (o *Orchestrator) dispatchFix(ev linear.AgentEvent, task *store.AdmiralTask, description string) {
	switch task.State {
	case store.JobStateReceived, store.JobStateExecuting:
		o.logger.Info("dispatch_reject_fix_currently_processing",
			"session", ev.SessionID, "issue", ev.IssueIdentifier,
			"prior_state", task.State)
		// Task is in flight by definition of this case branch — Thought
		// keeps the live AgentSession alive for the running flow.
		o.postBusyAck(ev.SessionID, rerunCurrentlyProcessingHelp(ev.IssueIdentifier))
		return
	case store.JobStateBlocked:
		o.logger.Info("dispatch_reject_fix_blocked",
			"session", ev.SessionID, "issue", ev.IssueIdentifier)
		o.postBusyAck(ev.SessionID, "admiral is waiting for blockers to resolve. Will resume automatically.")
		return
	case store.JobStateAwaitingInput:
		o.logger.Info("dispatch_reject_fix_awaiting_input",
			"session", ev.SessionID, "issue", ev.IssueIdentifier)
		o.postBusyAck(ev.SessionID, "admiral is waiting for your reply to a question. Reply to the question or use /rerun to start fresh.")
		return
	case store.JobStateFailed, store.JobStateTimedOut, store.JobStateCancelled:
		o.logger.Info("dispatch_reject_fix_terminal_non_done",
			"session", ev.SessionID, "issue", ev.IssueIdentifier,
			"prior_state", task.State)
		o.postRejection(ev.SessionID, fmt.Sprintf(
			"/fix only works on a previous DONE run. The current attempt is %s. Use /rerun to start over.",
			task.State,
		))
		return
	case store.JobStateDoneMerged:
		// PR was merged → branch may already be deleted on origin and the
		// worktree cleaned up. /fix would either fail at worktree
		// recreation or push to a dead branch. Tell the user to /rerun.
		o.logger.Info("dispatch_reject_fix_done_merged",
			"session", ev.SessionID, "issue", ev.IssueIdentifier)
		o.postRejection(ev.SessionID,
			"PR was already merged — /fix can't reopen merged work. Use /rerun to start fresh on a new branch.")
		return
	case store.JobStateDone:
		// proceed
	default:
		o.logger.Warn("dispatch_fix_unhandled_state",
			"session", ev.SessionID, "issue", ev.IssueIdentifier,
			"state", task.State)
		o.postRejection(ev.SessionID, fmt.Sprintf("/fix not supported in state %q.", task.State))
		return
	}

	if task.PRURL == "" || task.ClaudeSessionID == "" {
		o.logger.Info("dispatch_reject_fix_legacy_row",
			"session", ev.SessionID, "issue", ev.IssueIdentifier,
			"has_pr", task.PRURL != "", "has_claude", task.ClaudeSessionID != "")
		o.postRejection(ev.SessionID,
			"/fix needs a prior run with both an open PR and a recoverable claude session, but this task is missing one of those. Use /rerun to start over.")
		return
	}

	if description == "" {
		o.postRejection(ev.SessionID,
			"/fix needs a description of what to change, e.g. `/fix the typo in line 12`.")
		return
	}

	o.logger.Info("dispatch_fix_starting",
		"session", ev.SessionID, "issue", ev.IssueIdentifier,
		"prior_pr", task.PRURL, "attempt_n", task.AttemptN)

	// Reframe the user's text as a correction prompt before the resume
	// claude run sees it. The original PR url and branch context anchor
	// claude to the right work product.
	framed := fmt.Sprintf(
		"Previous attempt opened %s on branch %s. The user reports the following issue with the previous output:\n\n%s\n\nPlease patch the prior work to address this. Push commits onto the existing branch — do NOT open a new PR.",
		task.PRURL, task.Branch, description,
	)
	fixEv := ev
	fixEv.UserMessage = framed

	go o.runFix(fixEv, task)
}

// runFix executes a /fix run: claude --resume on the prior session id,
// commits pushed to the existing branch, no new PR opened. Mirrors
// runWithAttempt's lifecycle but reuses the existing admiral_tasks row
// rather than claiming a new one (attempt_n unchanged).
func (o *Orchestrator) runFix(ev linear.AgentEvent, task *store.AdmiralTask) {
	release := o.acquireRunSlot()
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(o.cfg.MaxRunSeconds+120)*time.Second)
	defer cancel()

	// autopilot_jobs claim is best-effort under the new model. ev.SessionID
	// is fresh (mention path) or reused (prompted path); either way
	// admiral_tasks is the truth.
	if _, err := o.db.ClaimAutopilotJob(ev.SessionID, ev.IssueID, ev.IssueIdentifier); err != nil {
		o.logger.Warn("claim_fix_audit_log_failed",
			"err", err, "session", ev.SessionID)
	}

	// Transition admiral_tasks: DONE → EXECUTING. Future events on this
	// task will see "currently processing" until /fix completes.
	if err := o.db.UpdateAdmiralTask(ev.IssueID, func(t *store.AdmiralTask) {
		t.State = store.JobStateExecuting
		t.LastEventSessionID = ev.SessionID
	}); err != nil {
		o.logger.Warn("fix_admiral_task_to_executing_failed",
			"err", err, "issue", ev.IssueID)
	}

	// executeResume operates on a flow.job (*store.AutopilotJob); synthesize
	// one from the live admiral_tasks row so the resume helpers see the
	// branch / worktree / claude_session_id they expect. PR-C will fold
	// admiral_tasks awareness into executeResume directly; for now this
	// adapter keeps the diff small.
	syntheticPrior := &store.AutopilotJob{
		AgentSessionID:  task.LastEventSessionID,
		IssueID:         task.IssueID,
		IssueIdentifier: task.IssueIdentifier,
		State:           store.JobStateDone,
		WorktreePath:    task.WorktreePath,
		Branch:          task.Branch,
		PRURL:           task.PRURL,
		ClaudeSessionID: task.ClaudeSessionID,
	}
	resumeFlow := newResumeFlow(o, ctx, ev, syntheticPrior)
	if issue, err := o.lc.GetIssue(ctx, ev.IssueID); err == nil {
		resumeFlow.teamID = issue.TeamID
		if repo, err := o.db.GetRepoByProjectID(issue.ProjectID); err == nil && repo != nil {
			resumeFlow.repoDir = repo.RepoDir
			resumeFlow.baseBranch = repo.BaseBranch
		}
	}

	if err := resumeFlow.executeResume(); err != nil {
		o.logger.Error("autopilot_fix_failed",
			"issue", ev.IssueIdentifier, "session", ev.SessionID, "err", err)
		// markFailed updates admiral_tasks → FAILED (PR-B-v2 dual-write).
		resumeFlow.markFailed(err)
		return
	}

	// /fix succeeded: admiral_tasks back to DONE. PRURL / claude_session_id
	// are unchanged because we reused the prior task.
	now := time.Now().UTC().Format(time.RFC3339)
	if err := o.db.UpdateAdmiralTask(ev.IssueID, func(t *store.AdmiralTask) {
		t.State = store.JobStateDone
		t.FinishedAt = now
	}); err != nil {
		o.logger.Warn("fix_admiral_task_to_done_failed",
			"err", err, "issue", ev.IssueID)
	}

	o.logger.Info("autopilot_fix_done",
		"issue", ev.IssueIdentifier, "session", ev.SessionID,
		"pr", task.PRURL, "attempt_n", task.AttemptN)
}

// dispatchResume handles /resume on an existing admiral_tasks row. Only
// TIMED_OUT is supported — the command exists to continue a run that was
// killed by the per-task timeout, not to retry FAILED or rerun DONE.
//
// Requires task.ClaudeSessionID populated (admiral writes it before launching
// claude, so any timed-out task will have it) and the prior worktree still on
// disk. The branch may or may not have an associated PR — /resume creates one
// after claude finishes if the timed-out run died before it could.
func (o *Orchestrator) dispatchResume(ev linear.AgentEvent, task *store.AdmiralTask) {
	switch task.State {
	case store.JobStateReceived, store.JobStateExecuting:
		o.logger.Info("dispatch_reject_resume_currently_processing",
			"session", ev.SessionID, "issue", ev.IssueIdentifier,
			"prior_state", task.State, "attempt_n", task.AttemptN)
		o.postBusyAck(ev.SessionID, fmt.Sprintf(
			"admiral is currently running on this issue (attempt %d). Wait for it to finish before /resume.",
			task.AttemptN))
		return
	case store.JobStateTimedOut:
		// proceed
	default:
		o.replyRejection(ev.SessionID, task.State, fmt.Sprintf(
			"/resume only works on a TIMED_OUT task. Current state is %q. Use /rerun to start over or /fix <description> to patch the current PR.",
			task.State))
		return
	}

	if task.ClaudeSessionID == "" {
		o.logger.Info("dispatch_reject_resume_no_session",
			"session", ev.SessionID, "issue", ev.IssueIdentifier)
		o.postRejection(ev.SessionID,
			"/resume needs a recoverable claude session id, but this task has none. Use /rerun to start over.")
		return
	}
	if task.WorktreePath == "" {
		o.logger.Info("dispatch_reject_resume_no_worktree_path",
			"session", ev.SessionID, "issue", ev.IssueIdentifier)
		o.postRejection(ev.SessionID,
			"/resume needs the worktree from the prior run, but this task has none. Use /rerun to start over.")
		return
	}
	if _, err := os.Stat(task.WorktreePath); err != nil {
		o.logger.Info("dispatch_reject_resume_worktree_missing",
			"session", ev.SessionID, "issue", ev.IssueIdentifier,
			"worktree", task.WorktreePath, "err", err)
		o.postRejection(ev.SessionID,
			"/resume needs the prior worktree on disk, but it was cleaned up. Use /rerun to start over.")
		return
	}

	o.logger.Info("dispatch_resume_starting",
		"session", ev.SessionID, "issue", ev.IssueIdentifier,
		"prior_pr", task.PRURL, "attempt_n", task.AttemptN)

	go o.runResume(ev, task)
}

// runResume executes a /resume run: claude --resume on the prior session id
// inside the existing worktree, then opens (or reuses) a PR. Reuses the
// existing admiral_tasks row — attempt_n is unchanged because /resume is
// a continuation of the same attempt, not a new one.
func (o *Orchestrator) runResume(ev linear.AgentEvent, task *store.AdmiralTask) {
	release := o.acquireRunSlot()
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(o.cfg.MaxRunSeconds+120)*time.Second)
	defer cancel()

	if _, err := o.db.ClaimAutopilotJob(ev.SessionID, ev.IssueID, ev.IssueIdentifier); err != nil {
		o.logger.Warn("claim_resume_audit_log_failed",
			"err", err, "session", ev.SessionID)
	}

	if err := o.db.UpdateAdmiralTask(ev.IssueID, func(t *store.AdmiralTask) {
		t.State = store.JobStateExecuting
		t.LastEventSessionID = ev.SessionID
	}); err != nil {
		o.logger.Warn("resume_admiral_task_to_executing_failed",
			"err", err, "issue", ev.IssueID)
	}

	syntheticPrior := &store.AutopilotJob{
		AgentSessionID:  task.LastEventSessionID,
		IssueID:         task.IssueID,
		IssueIdentifier: task.IssueIdentifier,
		State:           store.JobStateTimedOut,
		WorktreePath:    task.WorktreePath,
		Branch:          task.Branch,
		PRURL:           task.PRURL,
		ClaudeSessionID: task.ClaudeSessionID,
	}
	resumeFlow := newResumeFlow(o, ctx, ev, syntheticPrior)
	if issue, err := o.lc.GetIssue(ctx, ev.IssueID); err == nil {
		resumeFlow.teamID = issue.TeamID
		if repo, err := o.db.GetRepoByProjectID(issue.ProjectID); err == nil && repo != nil {
			resumeFlow.repoDir = repo.RepoDir
			resumeFlow.baseBranch = repo.BaseBranch
		}
	}

	if err := resumeFlow.executeResumeFromTimeout(); err != nil {
		o.logger.Error("autopilot_resume_failed",
			"issue", ev.IssueIdentifier, "session", ev.SessionID, "err", err)
		resumeFlow.markFailed(err)
		return
	}

	o.logger.Info("autopilot_resume_done",
		"issue", ev.IssueIdentifier, "session", ev.SessionID,
		"pr", resumeFlow.prURL, "attempt_n", task.AttemptN)
}

func followupSuffix(sessionID string) string {
	clean := sanitizeForPath(sessionID)
	if len(clean) == 0 {
		return "followup"
	}
	if len(clean) > 8 {
		clean = clean[:8]
	}
	return "followup-" + clean
}

// dispatchAwaitingReply handles a bare-text reply from the user when the
// task is in AWAITING_INPUT state. It matches the reply to the open pending
// question and re-spawns the claude session with the answer.
func (o *Orchestrator) dispatchAwaitingReply(ev linear.AgentEvent, task *store.AdmiralTask, reply string) {
	// Look up the exact question correlated to this task by ID (not just by
	// issue) so stale orphaned rows from a prior rerun don't get matched.
	if task.PendingQuestionID == "" {
		o.logger.Warn("dispatch_awaiting_reply_no_question_id",
			"session", ev.SessionID, "issue", ev.IssueIdentifier)
		o.postBusyAck(ev.SessionID, "admiral: no pending question found. Use /rerun to start a new run.")
		return
	}
	pq, err := o.db.GetPendingQuestionByID(task.PendingQuestionID)
	if err != nil {
		o.logger.Error("dispatch_awaiting_reply_lookup_failed",
			"err", err, "question_id", task.PendingQuestionID)
		o.postBusyAck(ev.SessionID, "admiral: internal error looking up the pending question. Try /rerun.")
		return
	}
	if pq == nil || pq.AnsweredAt != "" {
		o.logger.Warn("dispatch_awaiting_reply_question_gone",
			"session", ev.SessionID, "issue", ev.IssueIdentifier,
			"question_id", task.PendingQuestionID)
		o.postBusyAck(ev.SessionID, "admiral: pending question not found or already answered. Use /rerun to start a new run.")
		return
	}

	if err := o.db.AnswerPendingQuestion(pq.ID, reply); err != nil {
		o.logger.Error("dispatch_awaiting_reply_answer_failed",
			"err", err, "question_id", pq.ID)
		o.postBusyAck(ev.SessionID, "admiral: internal error recording your answer. Try again.")
		return
	}

	ok, err := o.db.TransitionAwaitingInputToExecuting(ev.IssueID)
	if err != nil {
		o.logger.Error("dispatch_awaiting_reply_transition_failed",
			"err", err, "issue", ev.IssueID)
		return
	}
	if !ok {
		o.logger.Warn("dispatch_awaiting_reply_transition_lost",
			"session", ev.SessionID, "issue", ev.IssueIdentifier)
		return
	}

	o.logger.Info("dispatch_awaiting_resume",
		"session", ev.SessionID,
		"issue", ev.IssueIdentifier,
		"question_id", pq.ID,
		"reply_preview", truncate(reply, 80))

	go o.runAwaitingResume(ev, task, pq, reply)
}

// runAwaitingResume resumes a claude session after the user replied to an
// ask_user question. It acquires a run slot, calls claude --resume with the
// answer injected, then opens a PR as usual.
func (o *Orchestrator) runAwaitingResume(ev linear.AgentEvent, task *store.AdmiralTask, pq *store.PendingQuestion, reply string) {
	release := o.acquireRunSlot()
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(o.cfg.MaxRunSeconds+120)*time.Second)
	defer cancel()

	if pq.WorktreePath == "" || pq.ClaudeSessionID == "" {
		o.logger.Error("awaiting_resume_missing_fields",
			"issue", ev.IssueIdentifier,
			"worktree", pq.WorktreePath,
			"claude_session", pq.ClaudeSessionID)
		o.postBusyAck(ev.SessionID, "admiral: resume failed — missing worktree or claude session. Use /rerun.")
		_ = o.db.UpdateAdmiralTask(ev.IssueID, func(t *store.AdmiralTask) {
			t.State = store.JobStateFailed
			t.Error = "awaiting_resume: missing worktree or claude_session_id"
		})
		return
	}

	// Synthetic autopilot_jobs row so runClaudeResume can access session ID.
	job := &store.AutopilotJob{
		AgentSessionID:  ev.SessionID,
		ClaudeSessionID: pq.ClaudeSessionID,
		WorktreePath:    pq.WorktreePath,
		Branch:          task.Branch,
	}
	f := newResumeFlow(o, ctx, ev, job)
	f.worktreePath = pq.WorktreePath
	f.branch = task.Branch
	f.claudeSessionID = pq.ClaudeSessionID

	issue, err := o.lc.GetIssue(ctx, ev.IssueID)
	if err != nil {
		o.logger.Error("awaiting_resume_fetch_issue_failed",
			"err", err, "issue", ev.IssueID)
		_ = o.db.UpdateAdmiralTask(ev.IssueID, func(t *store.AdmiralTask) {
			t.State = store.JobStateFailed
			t.Error = fmt.Sprintf("fetch issue: %v", err)
		})
		return
	}
	if repo, err := o.db.GetRepoByProjectID(issue.ProjectID); err == nil && repo != nil {
		f.repoDir = repo.RepoDir
		f.baseBranch = repo.BaseBranch
	}

	if err := f.openStreamFile(); err != nil {
		o.logger.Warn("awaiting_resume_stream_open_failed", "err", err)
	}
	defer f.closeStreamFile()

	userMsg := fmt.Sprintf(
		"The user has answered the pending question.\n\nQuestion: %s\nAnswer: %s\n\nContinue from where you left off.",
		pq.Question, reply)
	f.postActivity(linear.Action("claude_resume",
		fmt.Sprintf("claude -p --resume (awaiting-input reply) in %s", f.worktreePath), ""))
	if err := f.runClaudeResume(userMsg); err != nil {
		o.logger.Error("awaiting_resume_claude_failed",
			"issue", ev.IssueIdentifier, "err", err)
		_ = o.db.UpdateAdmiralTask(ev.IssueID, func(t *store.AdmiralTask) {
			t.State = store.JobStateFailed
			t.Error = err.Error()
		})
		f.postActivity(linear.ErrorActivity(fmt.Sprintf("claude resume failed: %v", err)))
		return
	}

	// claude called ask_user again — park and wait for the next reply.
	if f.awaitPendingID != "" {
		_ = f.parkAwaitingInput()
		return
	}

	f.postActivity(linear.Action("ensure_pr",
		fmt.Sprintf("gh pr (%s -> %s)", f.branch, f.baseBranch), ""))
	prURL, err := f.ensurePR(issue)
	isNoop := errors.Is(err, errPRNoCommits)
	if err != nil && !isNoop {
		o.logger.Error("awaiting_resume_pr_failed",
			"issue", ev.IssueIdentifier, "err", err)
		_ = o.db.UpdateAdmiralTask(ev.IssueID, func(t *store.AdmiralTask) {
			t.State = store.JobStateFailed
			t.Error = err.Error()
		})
		return
	}
	f.prURL = prURL

	now := time.Now().UTC().Format(time.RFC3339)
	_ = o.db.UpdateAdmiralTask(ev.IssueID, func(t *store.AdmiralTask) {
		t.State = store.JobStateDone
		t.PRURL = prURL
		t.ClaudeSessionID = pq.ClaudeSessionID
		t.FinishedAt = now
	})
	if !isNoop && prURL != "" && o.ciWatcher != nil {
		o.ciWatcher.WatchPR(ctx, prURL, f.repoDir, ev.SessionID, ev.IssueID)
	}
	o.logger.Info("awaiting_resume_done",
		"issue", ev.IssueIdentifier, "pr", prURL)
}

func (o *Orchestrator) run(ev linear.AgentEvent) {
	o.runWithAttempt(ev, 1)
}

// runWithAttempt is the rerun-aware variant of run. attemptN > 1 names
// the branch as linear/<id>-rerun-<N> so the new run does not collide
// with prior PRs. attemptN == 1 is the original first-time path.
//
// The autopilot_jobs row is keyed by ev.SessionID; for /rerun via @mention
// Linear gives a fresh SessionID and Claim succeeds. For /rerun via thread
// (prompted), ev.SessionID is reused — the existing autopilot_jobs row
// stays in its old state but admiral_tasks is the source of truth, so
// the run still proceeds correctly. PR-C will drop the autopilot_jobs
// dual-write entirely.
func (o *Orchestrator) runWithAttempt(ev linear.AgentEvent, attemptN int) {
	release := o.acquireRunSlot()
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(o.cfg.MaxRunSeconds+120)*time.Second)
	defer cancel()

	// autopilot_jobs claim is best-effort under the new model — admiral_tasks
	// is the truth. A duplicate ev.SessionID (prompted /rerun) means
	// ClaimAutopilotJob returns false; we keep going regardless.
	claimed, err := o.db.ClaimAutopilotJob(ev.SessionID, ev.IssueID, ev.IssueIdentifier)
	if err != nil {
		o.logger.Error("claim_job_failed", "err", err, "session", ev.SessionID)
		return
	}
	if !claimed {
		o.logger.Info("autopilot_jobs_claim_skipped_duplicate_session",
			"session", ev.SessionID, "issue", ev.IssueIdentifier,
			"reason", "audit-log-only-under-admiral_tasks-truth")
	}

	flow := newFlow(o, ctx, ev)
	flow.attemptN = attemptN
	if err := flow.execute(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) ||
			strings.Contains(err.Error(), "signal: killed") ||
			strings.Contains(err.Error(), "signal: terminated") {
			flow.markTimedOut(err)
		} else {
			flow.markFailed(err)
		}
		return
	}
	o.logger.Info("autopilot_done",
		"issue", ev.IssueIdentifier, "session", ev.SessionID,
		"pr", flow.prURL, "attempt_n", attemptN)
}

// flow carries the per-job state across the run() steps.
type flow struct {
	o   *Orchestrator
	ctx context.Context
	ev  linear.AgentEvent
	job *store.AutopilotJob // populated for resume flows

	branch          string
	worktreePath    string
	prURL           string
	streamFile      *os.File
	claudeSessionID string
	teamID          string
	repoDir         string
	baseBranch      string

	// attemptN is the admiral_tasks attempt counter for this run.
	// attemptN == 1 → first run on linear/<id>.
	// attemptN  > 1 → /rerun, branch becomes linear/<id>-rerun-<N>.
	// Set by Orchestrator.runWithAttempt before flow.execute.
	attemptN int

	// followupSuffix, when non-empty, is appended to the deterministic
	// branch name and worktree path so a follow-up after a merged PR
	// doesn't collide with the (possibly still-cached) original branch.
	// Set by handleCreated for the GEO-38 fresh-follow-up path; empty for
	// the normal first run.
	followupSuffix string

	// awaitPendingID is set by drainStreamJSON when it detects the
	// ADMIRAL_AWAIT:<id> marker in claude's output. A non-empty value
	// means the run called ask_user and parked itself; execute() will
	// transition the task to AWAITING_INPUT instead of opening a PR.
	awaitPendingID string
}

func newFlow(o *Orchestrator, ctx context.Context, ev linear.AgentEvent) *flow {
	return &flow{o: o, ctx: ctx, ev: ev}
}

// newResumeFlow creates a flow for resuming an existing session.
func newResumeFlow(o *Orchestrator, ctx context.Context, ev linear.AgentEvent, job *store.AutopilotJob) *flow {
	return &flow{o: o, ctx: ctx, ev: ev, job: job}
}

// persistAdmiralTask updates the live admiral_tasks row for the issue.
// Failures are logged but do not abort the flow — admiral_tasks is the
// source of truth for dispatch but flow.execute can still complete its
// autopilot_jobs path even if the admiral_tasks write transiently
// fails (next event will reconcile by reading whatever sticks).
func (f *flow) persistAdmiralTask(fn func(*store.AdmiralTask)) {
	if f.ev.IssueID == "" {
		return
	}
	if err := f.o.db.UpdateAdmiralTask(f.ev.IssueID, fn); err != nil {
		f.o.logger.Warn("update_admiral_task_failed",
			"err", err, "issue", f.ev.IssueID)
	}
}

func (f *flow) postActivity(a linear.AgentActivity) {
	if err := f.o.lc.PostAgentActivity(f.ctx, f.ev.SessionID, a); err != nil {
		f.o.logger.Warn("post_activity_failed",
			"session", f.ev.SessionID, "type", a.Type, "err", err)
	}
}

// postActivityWithRetry posts the activity with up to 3 retries using exponential
// backoff. If all attempts fail, it returns the last error.
func (f *flow) postActivityWithRetry(a linear.AgentActivity) error {
	delays := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second}
	var lastErr error
	for attempt := 0; attempt <= len(delays); attempt++ {
		err := f.o.lc.PostAgentActivity(f.ctx, f.ev.SessionID, a)
		if err == nil {
			return nil
		}
		lastErr = err
		if attempt < len(delays) {
			time.Sleep(delays[attempt])
		}
	}
	return lastErr
}

func (f *flow) execute() error {
	f.postActivity(linear.Thought("Reading issue context...", true))

	issue, err := f.o.lc.GetIssue(f.ctx, f.ev.IssueID)
	if err != nil {
		return fmt.Errorf("fetch issue: %w", err)
	}

	f.teamID = issue.TeamID

	// Route to repo by Linear project ID. The team is intentionally not used
	// for routing — a Linear team can own multiple repos, and project↔repo
	// is the cleaner 1:1 mapping. Issues without a project are rejected;
	// the user must assign a Linear project that's configured in
	// autopilot.repos.
	if strings.TrimSpace(issue.ProjectID) == "" {
		return fmt.Errorf("issue %s has no Linear project; admiral routes by project_id, please assign a project that is configured in autopilot.repos", issue.Identifier)
	}
	repo, err := f.o.db.GetRepoByProjectID(issue.ProjectID)
	if err != nil {
		return fmt.Errorf("get repo for project %s: %w", issue.ProjectID, err)
	}
	if repo == nil || !repo.Enabled {
		return fmt.Errorf("no enabled repo configured for project %s", issue.ProjectID)
	}
	f.repoDir = repo.RepoDir
	f.baseBranch = repo.BaseBranch
	worktreeName := "linear-" + sanitizeForPath(issue.Identifier)
	f.branch = branchName(issue)
	// /rerun: append `-rerun-<N>` to both branch and worktree so a new
	// attempt does not collide with the prior PR's branch on the remote.
	// attemptN is set by Orchestrator.runWithAttempt; default 1 means
	// no suffix.
	if f.attemptN > 1 {
		suf := fmt.Sprintf("rerun-%d", f.attemptN)
		f.branch = f.branch + "-" + suf
		worktreeName = worktreeName + "-" + suf
	}
	if f.followupSuffix != "" {
		// Append a unique suffix so a fresh-follow-up flow (after a merged
		// or closed prior PR) doesn't collide with the still-tracked
		// original branch / worktree dir.
		suf := sanitizeForPath(f.followupSuffix)
		f.branch = f.branch + "-" + suf
		worktreeName = worktreeName + "-" + suf
	}

	// Already-merged short-circuit. The deterministic branch name for this
	// issue may already point to a merged PR — most commonly because a
	// human authored and merged the PR directly via the GitHub UI while
	// admiral was offline / queued. We don't want to spawn a worktree and
	// run claude only to discover there's nothing to change. The check is
	// best-effort: gh failures fall through to the normal flow. Skipped on
	// follow-up flows since the suffixed branch is intentionally fresh.
	if f.followupSuffix == "" {
		if url, sha, found, err := f.o.gh.FindMergedPRForBranch(f.ctx, f.repoDir, f.branch); err != nil {
			f.o.logger.Warn("merged_pr_check_failed",
				"err", err, "branch", f.branch)
		} else if found {
			return f.markAlreadyMerged(url, sha)
		}
	}

	// Active-session short-circuit. A prior autopilot session for this
	// issue may still be in flight (RECEIVED/EXECUTING/etc.) when a fresh
	// AgentSessionEvent.created arrives — typically because admiral
	// dispatched in parallel with a human edit, or because Linear delivered
	// a duplicate event. Spawning a second worker would race the same
	// branch. Skipped on follow-up flows (suffixed branch is unique).
	if f.followupSuffix == "" {
		if prior, err := f.o.db.FindActiveJobByIssue(f.ev.IssueID, f.ev.SessionID); err != nil {
			f.o.logger.Warn("active_job_check_failed",
				"err", err, "issue", f.ev.IssueID)
		} else if prior != nil {
			return f.markActiveSessionDuplicate(prior.AgentSessionID)
		}
	}

	// Open-PR-by-human short-circuit. A human may have opened a PR for the
	// same deterministic branch while admiral was queued or offline. We
	// detect this by checking for an open PR on the branch authored by
	// someone other than admiral. If found, short-circuit so we don't
	// create a duplicate PR. Skipped on follow-up flows (suffixed branch
	// is intentionally unique).
	if f.followupSuffix == "" && f.o.ghUser != "" {
		if url, author, found, err := f.o.gh.FindOpenPRForBranch(f.ctx, f.repoDir, f.branch); err != nil {
			f.o.logger.Warn("open_pr_check_failed",
				"err", err, "branch", f.branch)
		} else if found && author != f.o.ghUser {
			return f.markOpenPRByOther(url, author)
		}
	}

	f.worktreePath = filepath.Join(
		absWorktreeRootWithRepo(f.o.cfg, f.repoDir),
		worktreeName,
	)
	if err := f.o.db.UpdateAutopilotJob(f.ev.SessionID, func(j *store.AutopilotJob) {
		j.State = store.JobStateExecuting
		j.WorktreePath = f.worktreePath
		j.Branch = f.branch
	}); err != nil {
		return fmt.Errorf("update job to EXECUTING: %w", err)
	}
	// Mirror to admiral_tasks — source of truth for dispatch (GEO-50).
	f.persistAdmiralTask(func(t *store.AdmiralTask) {
		t.State = store.JobStateExecuting
		t.WorktreePath = f.worktreePath
		t.Branch = f.branch
	})

	// Update Linear issue status to "started" asynchronously (non-blocking).
	if f.o.cfg.UpdateIssueStatus != nil && *f.o.cfg.UpdateIssueStatus && f.teamID != "" {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if id, err := f.o.stateIDByType(ctx, f.teamID, "started"); err == nil && id != "" {
				if err := f.o.lc.IssueUpdate(ctx, f.ev.IssueID, id); err != nil {
					f.o.logger.Warn("issue_update_started_failed", "err", err)
				}
			}
		}()
	}

	f.postActivity(linear.Action("worktree_create",
		fmt.Sprintf("%s @ %s", f.branch, f.baseBranch),
		""))
	if err := f.createWorktree(); err != nil {
		return fmt.Errorf("create worktree: %w", err)
	}
	if err := f.configureWorktreeIgnores(); err != nil {
		return fmt.Errorf("configure worktree ignores: %w", err)
	}

	f.postActivity(linear.Action("claude_run",
		fmt.Sprintf("claude -p in %s", f.worktreePath),
		""))
	if err := f.openStreamFile(); err != nil {
		return fmt.Errorf("open stream file: %w", err)
	}
	defer f.closeStreamFile()
	if err := f.runClaude(issue); err != nil {
		return fmt.Errorf("claude run: %w", err)
	}
	// ask_user was called: park task as AWAITING_INPUT and return without
	// opening a PR. The worktree stays on disk for the resume run.
	if f.awaitPendingID != "" {
		return f.parkAwaitingInput()
	}

	f.postActivity(linear.Action("ensure_pr",
		fmt.Sprintf("gh pr (%s -> %s)", f.branch, f.baseBranch),
		""))
	prURL, err := f.ensurePR(issue)
	isNoop := errors.Is(err, errPRNoCommits)
	if err != nil && !isNoop {
		return fmt.Errorf("ensure PR: %w", err)
	}
	f.prURL = prURL

	now := time.Now().UTC().Format(time.RFC3339)
	if err := f.o.db.UpdateAutopilotJob(f.ev.SessionID, func(j *store.AutopilotJob) {
		j.State = store.JobStateDone
		j.PRURL = prURL
		j.FinishedAt = now
	}); err != nil {
		return fmt.Errorf("update job to DONE: %w", err)
	}
	// Mirror to admiral_tasks — source of truth for dispatch (GEO-50).
	f.persistAdmiralTask(func(t *store.AdmiralTask) {
		t.State = store.JobStateDone
		t.PRURL = prURL
		t.ClaudeSessionID = f.claudeSessionID
		t.FinishedAt = now
	})

	// Spawn CI watcher (non-blocking). Watches GitHub check runs and posts
	// results to Linear thread. On failure, transitions admiral_tasks to
	// FAILED with reason "ci_failed" (GEO-54).
	if !isNoop && prURL != "" && f.o.ciWatcher != nil {
		f.o.ciWatcher.WatchPR(f.ctx, prURL, f.repoDir, f.ev.SessionID, f.ev.IssueID)
	}

	mention := f.creatorMention()
	var doneBody string
	if isNoop {
		doneBody = fmt.Sprintf(
			"%sNo diff produced — task understood as noop, no PR opened.\n\nWorktree: `%s`\nBranch: `%s`",
			mention, f.worktreePath, f.branch)
	} else {
		doneBody = fmt.Sprintf(
			"%sDone. PR opened: %s\n\nWorktree: `%s`\nBranch: `%s`",
			mention, prURL, f.worktreePath, f.branch)
	}
	if err := f.postActivityWithRetry(linear.Response(doneBody)); err != nil {
		f.o.logger.Error("final_activity_push_failed",
			"session", f.ev.SessionID, "err", err)
		_ = f.o.db.UpdateAutopilotJob(f.ev.SessionID, func(j *store.AutopilotJob) {
			j.State = store.JobStateDoneThreadInconsistent
		})
		f.persistAdmiralTask(func(t *store.AdmiralTask) {
			t.State = store.JobStateDoneThreadInconsistent
		})
		// Add PR body footer as fallback signal.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = f.addInconsistencyFooter(ctx)
		cancel()
	}

	// Linear post-PR state transitions (in_review / reviewed / completed /
	// canceled) are now driven by admiral-discoverer based on the live
	// GitHub PR state — autopilot only owns the "started" transition above
	// since it needs the in-process knowledge of "claude run is starting
	// now". The completed transition used to live here; removed in the
	// task-lifecycle refactor.

	f.cleanupWorktree(cleanupDelete)
	return nil
}

// executeResume resumes a claude session on the original branch/PR.
// For TIMED_OUT jobs, updates the original job row to DONE.
// For prompted events, creates a new DONE row for the new session.
func (f *flow) executeResume() error {
	f.postActivity(linear.Thought("Resuming previous session...", true))

	if err := f.ensureWorktree(); err != nil {
		return fmt.Errorf("ensure worktree: %w", err)
	}

	f.postActivity(linear.Action("claude_resume",
		fmt.Sprintf("claude -p --resume in %s", f.worktreePath),
		""))
	if err := f.openStreamFile(); err != nil {
		return fmt.Errorf("open stream file: %w", err)
	}
	defer f.closeStreamFile()
	if err := f.runClaudeResume(f.ev.UserMessage); err != nil {
		return fmt.Errorf("claude resume: %w", err)
	}

	if err := f.pushBranch(); err != nil {
		return fmt.Errorf("push: %w", err)
	}

	f.prURL = f.job.PRURL
	f.postActivity(linear.Response("Updated PR with follow-up: " + f.prURL))

	// Use the original job's session ID so TIMED_OUT resumes update the
	// original row (not a new one) and prompted resumes create a new row.
	sessionID := f.job.AgentSessionID
	if err := f.o.db.UpdateAutopilotJob(sessionID, func(j *store.AutopilotJob) {
		j.State = store.JobStateDone
		j.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	}); err != nil {
		return fmt.Errorf("update job to DONE: %w", err)
	}

	f.cleanupWorktree(cleanupDelete)
	return nil
}

// executeResumeFromTimeout continues a timed-out claude session in its
// original worktree, then opens (or reuses) a PR. Mirrors execute()'s
// post-claude path but skips fresh-worktree setup — the timed-out attempt
// already created one, and dispatchResume guarantees it's still on disk.
func (f *flow) executeResumeFromTimeout() error {
	f.postActivity(linear.Thought("Resuming previous timed-out session...", true))

	issue, err := f.o.lc.GetIssue(f.ctx, f.ev.IssueID)
	if err != nil {
		return fmt.Errorf("fetch issue: %w", err)
	}
	f.teamID = issue.TeamID
	if f.repoDir == "" {
		repo, err := f.o.db.GetRepoByProjectID(issue.ProjectID)
		if err != nil || repo == nil {
			return fmt.Errorf("repo lookup failed for project %s: %w", issue.ProjectID, err)
		}
		f.repoDir = repo.RepoDir
		f.baseBranch = repo.BaseBranch
	}

	if err := f.ensureWorktree(); err != nil {
		return fmt.Errorf("ensure worktree: %w", err)
	}

	f.postActivity(linear.Action("claude_resume",
		fmt.Sprintf("claude -p --resume in %s", f.worktreePath),
		""))
	if err := f.openStreamFile(); err != nil {
		return fmt.Errorf("open stream file: %w", err)
	}
	defer f.closeStreamFile()

	// A non-empty prompt cues claude to continue rather than wait for input.
	// The actual continuation context lives in the resumed session itself.
	if err := f.runClaudeResume("Continue from where the previous run was interrupted by timeout."); err != nil {
		return fmt.Errorf("claude resume: %w", err)
	}

	f.postActivity(linear.Action("ensure_pr",
		fmt.Sprintf("gh pr (%s -> %s)", f.branch, f.baseBranch),
		""))
	prURL, err := f.ensurePR(issue)
	isNoop := errors.Is(err, errPRNoCommits)
	if err != nil && !isNoop {
		return fmt.Errorf("ensure PR: %w", err)
	}
	f.prURL = prURL

	now := time.Now().UTC().Format(time.RFC3339)
	if err := f.o.db.UpdateAutopilotJob(f.ev.SessionID, func(j *store.AutopilotJob) {
		j.State = store.JobStateDone
		j.PRURL = prURL
		j.FinishedAt = now
	}); err != nil {
		return fmt.Errorf("update job to DONE: %w", err)
	}
	f.persistAdmiralTask(func(t *store.AdmiralTask) {
		t.State = store.JobStateDone
		t.PRURL = prURL
		t.FinishedAt = now
	})

	if !isNoop && prURL != "" && f.o.ciWatcher != nil {
		f.o.ciWatcher.WatchPR(f.ctx, prURL, f.repoDir, f.ev.SessionID, f.ev.IssueID)
	}

	mention := f.creatorMention()
	var doneBody string
	if isNoop {
		doneBody = fmt.Sprintf(
			"%sResumed from timeout but produced no new diff.\n\nWorktree: `%s`\nBranch: `%s`",
			mention, f.worktreePath, f.branch)
	} else {
		doneBody = fmt.Sprintf(
			"%sResumed from timeout. PR: %s\n\nWorktree: `%s`\nBranch: `%s`",
			mention, prURL, f.worktreePath, f.branch)
	}
	if err := f.postActivityWithRetry(linear.Response(doneBody)); err != nil {
		f.o.logger.Error("final_activity_push_failed",
			"session", f.ev.SessionID, "err", err)
	}

	f.cleanupWorktree(cleanupDelete)
	return nil
}

// ensureWorktree re-enters the worktree if it still exists, or recreates it
// on the original branch if it was cleaned up. After the worktree is ready,
// it fetches origin/<branch> and refuses to proceed if origin has commits
// that are not ancestors of HEAD — i.e. someone pushed to the branch while
// admiral's previous run was timed out. Auto-rebase isn't safe (claude's
// unpushed work could conflict with the external commits), so this surfaces
// the divergence as an error and lets the caller mark the task failed.
func (f *flow) ensureWorktree() error {
	if _, err := os.Stat(f.job.WorktreePath); err == nil {
		f.worktreePath = f.job.WorktreePath
		f.branch = f.job.Branch
	} else {
		// Worktree was cleaned up; recreate it on the original branch.
		// Local refs/heads/<branch> still has admiral's prior commits in the
		// parent repo's object store (worktree removal doesn't delete refs),
		// so checking out the local branch ref preserves work in progress.
		cmd := exec.Command("git", "fetch", "origin", f.job.Branch)
		cmd.Dir = f.repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git fetch %s: %w (%s)", f.job.Branch, err, out)
		}

		cmd = exec.Command("git", "worktree", "add", f.job.WorktreePath, f.job.Branch)
		cmd.Dir = f.repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git worktree add: %w (%s)", err, out)
		}

		f.worktreePath = f.job.WorktreePath
		f.branch = f.job.Branch
	}

	if err := f.checkBranchDiverged(); err != nil {
		return err
	}
	return nil
}

// errBranchDiverged is the sentinel wrapped into the error returned by
// checkBranchDiverged when origin has commits not in HEAD. Used by markFailed
// to keep the worktree on disk so the user can manually rebase and push.
var errBranchDiverged = errors.New("branch diverged")

// checkBranchDiverged fetches origin/<branch> and returns an error wrapping
// errBranchDiverged if origin has commits that are not in the worktree's HEAD.
// The reuse path of ensureWorktree doesn't fetch on its own, so this also
// covers the case where the worktree was kept across a timed-out run while
// someone pushed externally.
//
// Precondition: origin/<branch> must exist (i.e. admiral has pushed the branch
// in a prior run). If it doesn't, `git fetch` fails before merge-base is
// reached and the caller sees the wrapped fetch error.
func (f *flow) checkBranchDiverged() error {
	if f.branch == "" {
		return fmt.Errorf("checkBranchDiverged: f.branch is empty")
	}
	cmd := exec.Command("git", "fetch", "origin", f.branch)
	cmd.Dir = f.worktreePath
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git fetch origin %s in worktree: %w (%s)", f.branch, err, out)
	}

	// `merge-base --is-ancestor origin/<branch> HEAD` exits 0 when origin is
	// an ancestor of HEAD (local equal or ahead — push will fast-forward) and
	// 1 when origin has commits HEAD doesn't have (push would be rejected).
	originRef := "origin/" + f.branch
	cmd = exec.Command("git", "merge-base", "--is-ancestor", originRef, "HEAD")
	cmd.Dir = f.worktreePath
	err := cmd.Run()
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return fmt.Errorf(
			"%w: %s has commits not in the resume worktree at %s (external push?); "+
				"admiral will not auto-rebase. Inside that worktree, run "+
				"`git pull --rebase origin %s` and resolve conflicts, then "+
				"`git push origin %s`. admiral's task is marked failed and will not retry",
			errBranchDiverged, originRef, f.worktreePath, f.branch, f.branch)
	}
	return fmt.Errorf("git merge-base --is-ancestor %s HEAD: %w", originRef, err)
}

// runClaudeResume spawns `claude -p --resume` in stream-json mode inside the worktree.
func (f *flow) runClaudeResume(userMessage string) error {
	args := []string{
		"-p", userMessage,
		"--resume", f.job.ClaudeSessionID,
		"--output-format", "stream-json",
		"--verbose",
		"--dangerously-skip-permissions",
	}
	if mcpCfgPath, err := f.writeMCPConfig(); err != nil {
		f.o.logger.Warn("mcp_config_write_failed", "err", err)
	} else if mcpCfgPath != "" {
		args = append(args, "--mcp-config", mcpCfgPath)
		defer os.Remove(mcpCfgPath)
	}
	cctx, cancel := context.WithTimeout(f.ctx, time.Duration(f.o.cfg.MaxRunSeconds)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, f.o.cfg.ClaudeBin, args...)
	cmd.Dir = f.worktreePath
	cmd.Env = append(os.Environ(),
		"CLAUDE_AUTOPILOT_ISSUE="+f.ev.IssueIdentifier,
		"CLAUDE_AUTOPILOT_SESSION="+f.ev.SessionID,
		"ADMIRAL_DB_PATH="+f.o.dbPath,
		"ADMIRAL_ISSUE_ID="+f.ev.IssueID,
		"ADMIRAL_ISSUE_IDENTIFIER="+f.ev.IssueIdentifier,
		"ADMIRAL_LINEAR_SESSION="+f.ev.SessionID,
		"ADMIRAL_CLAUDE_SESSION="+f.job.ClaudeSessionID,
		"ADMIRAL_WORKTREE_PATH="+f.worktreePath,
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("claude stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("claude stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("claude start: %w", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); f.drainStreamJSON(stdout) }()
	go func() {
		defer wg.Done()
		s := bufio.NewScanner(stderr)
		s.Buffer(make([]byte, 0, 1024*1024), 4*1024*1024)
		for s.Scan() {
			f.o.logger.Warn("claude_stderr", "issue", f.ev.IssueIdentifier, "line", s.Text())
		}
	}()
	wg.Wait()
	if err := cmd.Wait(); err != nil {
		if cctx.Err() != nil {
			return fmt.Errorf("claude exit: %w: %w", err, context.DeadlineExceeded)
		}
		return fmt.Errorf("claude exit: %w", err)
	}
	return nil
}

// pushBranch pushes the branch to origin without creating a PR.
func (f *flow) pushBranch() error {
	cmd := exec.Command("git", "push", "origin", f.branch)
	cmd.Dir = f.worktreePath
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git push: %w (%s)", err, out)
	}
	return nil
}

// openStreamFile opens the per-job stream log file for appending. The file
// is created if it doesn't exist (0o644).
func (f *flow) openStreamFile() error {
	path := filepath.Join(f.o.cfg.JobStreamsDir, f.ev.SessionID+".jsonl")
	fh, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	f.streamFile = fh
	if f.o.db != nil {
		_ = f.o.db.UpdateAutopilotJob(f.ev.SessionID, func(j *store.AutopilotJob) {
			j.StreamLogPath = path
		})
	}
	return nil
}

// closeStreamFile closes the stream log file. Safe to call on nil.
func (f *flow) closeStreamFile() {
	if f.streamFile != nil {
		f.streamFile.Close()
		f.streamFile = nil
	}
}

// markAlreadyMerged is the success path for the "branch already in main"
// short-circuit. It posts a courtesy Linear response, marks the autopilot
// job DONE pointing at the existing PR, and skips worktree creation
// entirely. Returns nil so flow.run() treats the job as cleanly finished.
func (f *flow) markAlreadyMerged(prURL, mergeSHA string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	if err := f.o.db.UpdateAutopilotJob(f.ev.SessionID, func(j *store.AutopilotJob) {
		j.State = store.JobStateDone
		j.PRURL = prURL
		j.Branch = f.branch
		j.FinishedAt = now
	}); err != nil {
		f.o.logger.Warn("mark_already_merged_update_failed",
			"err", err, "session", f.ev.SessionID)
	}
	f.persistAdmiralTask(func(t *store.AdmiralTask) {
		t.State = store.JobStateDone
		t.PRURL = prURL
		t.Branch = f.branch
		t.FinishedAt = now
	})

	mention := f.creatorMention()
	shortSHA := mergeSHA
	if len(shortSHA) > 12 {
		shortSHA = shortSHA[:12]
	}
	body := fmt.Sprintf(
		"%sAlready merged. Nothing to do.\n\nPR: %s\nMerge: `%s`",
		mention, prURL, shortSHA)
	if err := f.postActivityWithRetry(linear.Response(body)); err != nil {
		f.o.logger.Warn("mark_already_merged_post_failed",
			"err", err, "session", f.ev.SessionID)
	}
	f.o.logger.Info("autopilot_already_merged",
		"issue", f.ev.IssueIdentifier, "session", f.ev.SessionID,
		"pr", prURL, "sha", mergeSHA)
	return nil
}

// creatorMention returns the "@<handle> " prefix used to ping the agent
// session creator on Linear thread replies. Linear's mention syntax is
// `@<displayName>` (the user's handle); using the user UUID does not
// trigger a notification. Falls back through DisplayName → Name → "" so
// we always emit something when the webhook payload omits a field, and
// returns "" when no human-readable identifier is known (in which case
// callers prepend nothing).
func (f *flow) creatorMention() string {
	handle := f.ev.CreatorDisplayName
	if handle == "" {
		handle = f.ev.CreatorName
	}
	if handle == "" {
		return ""
	}
	return "@" + handle + " "
}

// markActiveSessionDuplicate is the short-circuit path when another
// autopilot session for the same Linear issue is still in flight. The new
// session is recorded as CANCELLED (so it doesn't sit forever in
// RECEIVED), a courtesy reply is posted on the new thread, and no
// worktree is created. The prior session is left untouched — it owns the
// work.
func (f *flow) markActiveSessionDuplicate(priorSessionID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	if err := f.o.db.UpdateAutopilotJob(f.ev.SessionID, func(j *store.AutopilotJob) {
		j.State = store.JobStateCancelled
		j.Branch = f.branch
		j.Error = "duplicate_active_session: prior session " + priorSessionID
		j.FinishedAt = now
	}); err != nil {
		f.o.logger.Warn("mark_active_session_duplicate_update_failed",
			"err", err, "session", f.ev.SessionID)
	}
	// Note: we do NOT update admiral_tasks here. The active prior session
	// owns the live admiral_tasks row and is mid-execute; mutating it from
	// this duplicate path would corrupt the prior run's state.

	body := fmt.Sprintf(
		"%sadmiral is already working on this issue (session `%s`). Not dispatching a duplicate. Wait for it to finish, or cancel the prior session and re-mention with /rerun.",
		f.creatorMention(), priorSessionID)
	if err := f.postActivityWithRetry(linear.Response(body)); err != nil {
		f.o.logger.Warn("mark_active_session_duplicate_post_failed",
			"err", err, "session", f.ev.SessionID)
	}
	f.o.logger.Info("autopilot_active_session_duplicate",
		"issue", f.ev.IssueIdentifier, "session", f.ev.SessionID,
		"prior_session", priorSessionID)
	return nil
}

// markOpenPRByOther is the short-circuit path when a human has already opened
// a PR for the deterministic branch. It posts a courtesy Linear response,
// marks the job DONE pointing at the existing PR, and skips worktree creation
// entirely. Returns nil so flow.run() treats the job as cleanly finished.
func (f *flow) markOpenPRByOther(prURL, author string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	if err := f.o.db.UpdateAutopilotJob(f.ev.SessionID, func(j *store.AutopilotJob) {
		j.State = store.JobStateDone
		j.PRURL = prURL
		j.Branch = f.branch
		j.FinishedAt = now
	}); err != nil {
		f.o.logger.Warn("mark_open_pr_by_other_update_failed",
			"err", err, "session", f.ev.SessionID)
	}
	f.persistAdmiralTask(func(t *store.AdmiralTask) {
		t.State = store.JobStateDone
		t.PRURL = prURL
		t.Branch = f.branch
		t.FinishedAt = now
	})

	body := fmt.Sprintf(
		"%sOpen PR already exists at %s by @%s — admiral is not duplicating work. Re-mention me with /rerun after that PR is merged or closed if you still need follow-up.",
		f.creatorMention(), prURL, author)
	if err := f.postActivityWithRetry(linear.Response(body)); err != nil {
		f.o.logger.Warn("mark_open_pr_by_other_post_failed",
			"err", err, "session", f.ev.SessionID)
	}
	f.o.logger.Info("autopilot_open_pr_by_other",
		"issue", f.ev.IssueIdentifier, "session", f.ev.SessionID,
		"pr", prURL, "author", author)
	return nil
}

func (f *flow) markFailed(runErr error) {
	now := time.Now().UTC().Format(time.RFC3339)
	_ = f.o.db.UpdateAutopilotJob(f.ev.SessionID, func(j *store.AutopilotJob) {
		j.State = store.JobStateFailed
		j.Error = runErr.Error()
		j.FinishedAt = now
	})
	f.persistAdmiralTask(func(t *store.AdmiralTask) {
		t.State = store.JobStateFailed
		t.Error = runErr.Error()
		t.FinishedAt = now
	})
	// Use a fresh short ctx in case the parent is already done.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	mention := f.creatorMention()
	body := mention + "admiral failed: " + truncate(runErr.Error(), 1500)
	if f.worktreePath != "" {
		body += "\n\nWorktree: `" + f.worktreePath + "`"
	}
	if f.branch != "" {
		body += "\nBranch: `" + f.branch + "`"
	}
	if j, err := f.o.db.GetAutopilotJob(f.ev.SessionID); err == nil && j.StreamLogPath != "" {
		body += "\n\nStream log: " + j.StreamLogPath
	}
	err := f.postActivityWithRetry(linear.ErrorActivity(body))
	if err != nil {
		f.o.logger.Error("final_activity_push_failed",
			"session", f.ev.SessionID, "err", err)
		_ = f.o.db.UpdateAutopilotJob(f.ev.SessionID, func(j *store.AutopilotJob) {
			j.State = store.JobStateDoneThreadInconsistent
		})
		_ = f.addInconsistencyFooter(ctx)
	}
	// On branch divergence the error message instructs the user to rebase
	// inside the worktree and push manually — keep it on disk so that
	// instruction is actionable. The worktree will be cleaned up on the next
	// admiral task lifecycle (e.g. /rerun) that reuses or replaces it.
	if errors.Is(runErr, errBranchDiverged) {
		f.o.logger.Info("mark_failed_keep_worktree_for_manual_rebase",
			"session", f.ev.SessionID, "worktree", f.worktreePath)
		return
	}
	f.cleanupWorktree(cleanupArchive)
}

func (f *flow) markTimedOut(runErr error) {
	now := time.Now().UTC().Format(time.RFC3339)
	_ = f.o.db.UpdateAutopilotJob(f.ev.SessionID, func(j *store.AutopilotJob) {
		j.State = store.JobStateTimedOut
		j.Error = runErr.Error()
		j.FinishedAt = now
	})
	f.persistAdmiralTask(func(t *store.AdmiralTask) {
		t.State = store.JobStateTimedOut
		t.Error = runErr.Error()
		t.FinishedAt = now
	})
	// Use a fresh short ctx in case the parent is already done.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	mention := f.creatorMention()
	truncatedSession := f.claudeSessionID
	if len(truncatedSession) > 8 {
		truncatedSession = truncatedSession[:8]
	}
	body := mention + fmt.Sprintf(
		"Task timed out after %ds. State preserved — re-mention me on this issue to resume from where I left off. (claude session: %s...)",
		f.o.cfg.MaxRunSeconds, truncatedSession)
	_ = f.o.lc.PostAgentActivity(ctx, f.ev.SessionID, linear.Response(body))
	// NO cleanupWorktree: keep worktree for resume
}

// cleanupMode controls whether cleanupWorktree deletes or archives the worktree.
type cleanupMode int

const (
	cleanupDelete cleanupMode = iota
	cleanupArchive
)

// cleanupWorktree removes or archives the worktree depending on mode.
// Best-effort: failures are logged at WARN but don't change job state.
// Order matters for delete: remove worktree before deleting branch (worktree
// references the branch). Archive mode preserves the branch.
func (f *flow) cleanupWorktree(mode cleanupMode) {
	if f.worktreePath == "" {
		return
	}
	switch mode {
	case cleanupDelete:
		f.removeWorktreeAndBranch()
	case cleanupArchive:
		f.archiveWorktree()
	}
}

// removeWorktreeAndBranch deletes the worktree and local branch.
// Order matters: remove worktree before deleting branch.
func (f *flow) removeWorktreeAndBranch() {
	cmd := exec.Command("git", "worktree", "remove", "--force", f.worktreePath)
	cmd.Dir = f.repoDir
	if out, err := cmd.CombinedOutput(); err != nil {
		f.o.logger.Warn("cleanup_worktree_remove_failed",
			"err", err, "out", string(out), "path", f.worktreePath)
	} else {
		f.o.logger.Info("cleanup_worktree_removed", "path", f.worktreePath)
	}

	if f.branch != "" {
		cmd := exec.Command("git", "branch", "-D", f.branch)
		cmd.Dir = f.repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			f.o.logger.Warn("cleanup_branch_delete_failed",
				"err", err, "out", string(out), "branch", f.branch)
		} else {
			f.o.logger.Info("cleanup_branch_deleted", "branch", f.branch)
		}
	}
}

// archiveWorktree copies the worktree to .worktrees-archive/ then removes
// the git registration. The branch is preserved for manual follow-up.
// Best-effort: if archive copy fails, the worktree is NOT removed to avoid
// data loss.
func (f *flow) archiveWorktree() {
	archiveRoot := filepath.Join(f.repoDir, ".worktrees-archive")
	if err := os.MkdirAll(archiveRoot, 0o755); err != nil {
		f.o.logger.Warn("archive_mkdir_failed", "err", err, "path", archiveRoot)
		return
	}

	issue := f.ev.IssueIdentifier
	if issue == "" {
		issue = f.ev.IssueID
	}
	if issue == "" {
		issue = "unknown"
	}
	archivePath := filepath.Join(
		archiveRoot,
		fmt.Sprintf("%s-%d", sanitizeForPath(issue), time.Now().Unix()),
	)

	// 1. Copy worktree to archive path.
	if err := copyDir(f.worktreePath, archivePath); err != nil {
		f.o.logger.Warn("archive_copy_failed",
			"err", err, "src", f.worktreePath, "dst", archivePath)
		return
	}

	// 2. Remove the worktree from git registration + filesystem.
	// 'git worktree remove --force' can fail in subtle ways: another
	// process still holds a handle to the dir, the directory drifted out
	// of git's metadata, fs permission flapped, etc. The archive copy
	// already succeeded so the diff is preserved; if git's cooperative
	// removal fails we fall back to a hard `os.RemoveAll` + `git worktree
	// prune` so the active worktrees dir doesn't leak (this was the
	// failure mode that motivated GEO-37 Bug B). If THAT also fails we
	// log loudly so a human can investigate.
	cmd := exec.Command("git", "worktree", "remove", "--force", f.worktreePath)
	cmd.Dir = f.repoDir
	if out, err := cmd.CombinedOutput(); err != nil {
		f.o.logger.Warn("archive_worktree_remove_failed",
			"err", err, "out", string(out), "path", f.worktreePath,
			"action", "falling back to os.RemoveAll + git worktree prune")
		if removeErr := os.RemoveAll(f.worktreePath); removeErr != nil {
			f.o.logger.Error("archive_worktree_force_remove_failed",
				"err", removeErr, "path", f.worktreePath,
				"action", "manual cleanup required — directory will leak")
		} else {
			pruneCmd := exec.Command("git", "worktree", "prune")
			pruneCmd.Dir = f.repoDir
			if pruneOut, pruneErr := pruneCmd.CombinedOutput(); pruneErr != nil {
				f.o.logger.Warn("archive_worktree_prune_failed",
					"err", pruneErr, "out", string(pruneOut),
					"hint", "git's worktree list may show a stale entry until the next prune")
			} else {
				f.o.logger.Info("archive_worktree_force_removed",
					"path", f.worktreePath)
			}
		}
	}

	// 3. Branch is intentionally NOT deleted — user can checkout later.

	f.o.logger.Info("archive_worktree_done",
		"issue", issue, "path", archivePath)
}

// copyDir copies src to dst recursively using pure Go (no external commands).
func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		return copyFile(path, target, info.Mode())
	})
}

// copyFile copies a single file from src to dst.
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// configureWorktreeIgnores writes .git/info/exclude entries inside the
// worktree so claude's session artifacts (currently `.omc/`) don't get
// staged when the orchestrator does its post-run `git add -A`. Worktree
// ignores live alongside the worktree's gitdir, which we resolve via
// `git rev-parse --git-dir` (in a worktree, `.git` is a file, not a dir,
// so we can't hardcode the path).
func (f *flow) configureWorktreeIgnores() error {
	out, err := captureCmd(f.ctx, f.worktreePath, "git", "rev-parse", "--git-dir")
	if err != nil {
		return fmt.Errorf("git rev-parse --git-dir: %w (output: %s)", err, truncate(out, 200))
	}
	gitDir := strings.TrimSpace(out)
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(f.worktreePath, gitDir)
	}
	infoDir := filepath.Join(gitDir, "info")
	if err := os.MkdirAll(infoDir, 0o755); err != nil {
		return fmt.Errorf("mkdir info: %w", err)
	}
	excludeFile := filepath.Join(infoDir, "exclude")
	existing, _ := os.ReadFile(excludeFile)
	if strings.Contains(string(existing), "# admiral-autopilot") {
		return nil
	}
	fh, err := os.OpenFile(excludeFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open exclude: %w", err)
	}
	defer fh.Close()
	_, err = fh.WriteString("\n# admiral-autopilot — claude/omc session artifacts\n.omc/\n")
	return err
}

// createWorktree fetches origin/<base> and creates a fresh worktree. If the
// directory already exists (e.g. a prior failed run), it's removed first.
func (f *flow) createWorktree() error {
	repo := f.repoDir
	base := f.baseBranch
	if err := runCmd(f.ctx, repo, "git", "fetch", "origin", base); err != nil {
		return fmt.Errorf("git fetch origin %s: %w", base, err)
	}
	_ = runCmd(f.ctx, repo, "git", "worktree", "remove", "--force", f.worktreePath)
	_ = os.RemoveAll(f.worktreePath)
	_ = runCmd(f.ctx, repo, "git", "branch", "-D", f.branch)

	if err := os.MkdirAll(filepath.Dir(f.worktreePath), 0o755); err != nil {
		return fmt.Errorf("mkdir worktree parent: %w", err)
	}
	if err := runCmd(f.ctx, repo, "git", "worktree", "add",
		"-b", f.branch, f.worktreePath, "origin/"+base); err != nil {
		return fmt.Errorf("git worktree add: %w", err)
	}
	return nil
}

// runClaude spawns `claude -p` in stream-json mode inside the worktree and
// drains stdout until exit.
func (f *flow) runClaude(issue *linear.Issue) error {
	prompt := buildPrompt(f.o.cfg.AutopilotSkill, issue, f.ev, f.branch, f.baseBranch)

	claudeSessionID := uuid.NewString()
	if err := f.o.db.UpdateAutopilotJob(f.ev.SessionID, func(j *store.AutopilotJob) {
		j.ClaudeSessionID = claudeSessionID
	}); err != nil {
		f.o.logger.Warn("update_claude_session_id_failed", "session", f.ev.SessionID, "err", err)
	}
	f.claudeSessionID = claudeSessionID

	args := []string{
		"-p", prompt,
		"--session-id", claudeSessionID,
		"--output-format", "stream-json",
		"--verbose",
		// --dangerously-skip-permissions is Claude Code's canonical
		// "non-interactive headless" switch: auto-allows Edit/Write AND Bash
		// (so git/gh actually run). Without it, -p silently denies bash
		// because the permission prompt has nobody to answer it. Acceptable
		// here because the worktree is throwaway + the daemon already
		// chose this binary on purpose.
		"--dangerously-skip-permissions",
	}
	if mcpCfgPath, err := f.writeMCPConfig(); err != nil {
		f.o.logger.Warn("mcp_config_write_failed", "err", err)
	} else if mcpCfgPath != "" {
		args = append(args, "--mcp-config", mcpCfgPath)
		defer os.Remove(mcpCfgPath)
	}
	cctx, cancel := context.WithTimeout(f.ctx, time.Duration(f.o.cfg.MaxRunSeconds)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, f.o.cfg.ClaudeBin, args...)
	cmd.Cancel = func() error {
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = 5 * time.Second
	cmd.Dir = f.worktreePath
	cmd.Env = append(os.Environ(),
		"CLAUDE_AUTOPILOT_ISSUE="+issue.Identifier,
		"CLAUDE_AUTOPILOT_SESSION="+f.ev.SessionID,
		// Inherited by admiral-mcp-ask subprocess (via claude's env).
		"ADMIRAL_DB_PATH="+f.o.dbPath,
		"ADMIRAL_ISSUE_ID="+f.ev.IssueID,
		"ADMIRAL_ISSUE_IDENTIFIER="+f.ev.IssueIdentifier,
		"ADMIRAL_LINEAR_SESSION="+f.ev.SessionID,
		"ADMIRAL_CLAUDE_SESSION="+claudeSessionID,
		"ADMIRAL_WORKTREE_PATH="+f.worktreePath,
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("claude stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("claude stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("claude start: %w", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); f.drainStreamJSON(stdout) }()
	go func() {
		defer wg.Done()
		s := bufio.NewScanner(stderr)
		s.Buffer(make([]byte, 0, 1024*1024), 4*1024*1024)
		for s.Scan() {
			f.o.logger.Warn("claude_stderr", "issue", issue.Identifier, "line", s.Text())
		}
	}()
	wg.Wait()
	if err := cmd.Wait(); err != nil {
		if cctx.Err() != nil {
			return fmt.Errorf("claude exit: %w: %w", err, context.DeadlineExceeded)
		}
		return fmt.Errorf("claude exit: %w", err)
	}
	return nil
}

// writeMCPConfig writes a temporary MCP server config JSON file for the
// admiral-ask-mcp server and returns its path. Returns ("", nil) when
// McpAskBin is not found on PATH — the caller omits --mcp-config gracefully.
func (f *flow) writeMCPConfig() (string, error) {
	bin, err := exec.LookPath(f.o.cfg.McpAskBin)
	if err != nil {
		// MCP ask binary not installed — skip gracefully.
		return "", nil
	}
	cfg := map[string]any{
		"mcpServers": map[string]any{
			"admiral-ask": map[string]any{
				"command": bin,
				"args":    []string{},
			},
		},
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("marshal mcp config: %w", err)
	}
	tmp, err := os.CreateTemp("", "admiral-mcp-*.json")
	if err != nil {
		return "", fmt.Errorf("create mcp config temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", fmt.Errorf("write mcp config: %w", err)
	}
	tmp.Close()
	return tmp.Name(), nil
}

// parkAwaitingInput transitions the task to AWAITING_INPUT after claude has
// called ask_user and exited. The worktree is kept on disk so the resume
// path can continue file edits in the same directory.
func (f *flow) parkAwaitingInput() error {
	if err := f.o.db.SetAdmiralTaskAwaitingInput(f.ev.IssueID, f.awaitPendingID); err != nil {
		return fmt.Errorf("set task awaiting_input: %w", err)
	}
	_ = f.o.db.UpdateAutopilotJob(f.ev.SessionID, func(j *store.AutopilotJob) {
		j.State = store.JobStateAwaitingInput
	})
	f.o.logger.Info("task_awaiting_input",
		"issue", f.ev.IssueIdentifier,
		"pending_id", f.awaitPendingID)
	return nil
}

// streamMsg is the full structure emitted by claude -p stream-json.
// tool_use events live inside message.content[].type=="tool_use".
type streamMsg struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	IsError bool   `json:"is_error,omitempty"`

	// result event terminal fields
	DurationMs int    `json:"duration_ms,omitempty"`
	StopReason string `json:"stop_reason,omitempty"`

	// assistant message wrapper (tool_use and text live here in Anthropic stream-json)
	Message struct {
		Content []struct {
			Type  string          `json:"type"`
			Name  string          `json:"name,omitempty"`
			Input json.RawMessage `json:"input,omitempty"`
			Text  string          `json:"text,omitempty"`
		} `json:"content,omitempty"`
	} `json:"message,omitempty"`

	// standalone error type
	Error struct {
		Message string `json:"message,omitempty"`
	} `json:"error,omitempty"`
}

// truncateString truncates a string to at most n characters,
// appending "(...truncated)" if it was cut.
func truncateString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "(...truncated)"
}

// summarizeToolUse builds a 1-line summary string for a tool_use event,
// truncating long fields to <= 200 chars.
func summarizeToolUse(name string, rawInput json.RawMessage) string {
	if len(rawInput) == 0 {
		return "input=<empty>"
	}

	switch name {
	case "Edit":
		{
			var inp struct {
				FilePath  string `json:"file_path"`
				OldString string `json:"old_string"`
				NewString string `json:"new_string"`
			}
			if err := json.Unmarshal(rawInput, &inp); err != nil {
				return truncateString(string(rawInput), 200)
			}
			return fmt.Sprintf("file=%s (~%d chars old, ~%d chars new)",
				inp.FilePath, len(inp.OldString), len(inp.NewString))
		}
	case "Write":
		{
			var inp struct {
				FilePath string `json:"file_path"`
				Content  string `json:"content"`
			}
			if err := json.Unmarshal(rawInput, &inp); err != nil {
				return truncateString(string(rawInput), 200)
			}
			return fmt.Sprintf("file=%s (~%d chars)", inp.FilePath, len(inp.Content))
		}
	case "Read":
		{
			var inp struct {
				FilePath string `json:"file_path"`
				Offset   int    `json:"offset,omitempty"`
			}
			if err := json.Unmarshal(rawInput, &inp); err != nil {
				return truncateString(string(rawInput), 200)
			}
			if inp.Offset > 0 {
				return fmt.Sprintf("file=%s offset=%d", inp.FilePath, inp.Offset)
			}
			return fmt.Sprintf("file=%s", inp.FilePath)
		}
	case "Bash":
		{
			var inp struct {
				Command string `json:"command"`
			}
			if err := json.Unmarshal(rawInput, &inp); err != nil {
				return truncateString(string(rawInput), 200)
			}
			return fmt.Sprintf("cmd=%s", truncateString(inp.Command, 200))
		}
	case "TodoWrite":
		{
			var inp struct {
				Todos []struct{} `json:"todos"`
			}
			if err := json.Unmarshal(rawInput, &inp); err != nil {
				return truncateString(string(rawInput), 200)
			}
			return fmt.Sprintf("count=%d todos", len(inp.Todos))
		}
	case "Grep":
		{
			var inp struct {
				Pattern string `json:"pattern"`
				Path    string `json:"path,omitempty"`
			}
			if err := json.Unmarshal(rawInput, &inp); err != nil {
				return truncateString(string(rawInput), 200)
			}
			if inp.Path != "" {
				return fmt.Sprintf("pattern=%s path=%s", truncateString(inp.Pattern, 200), inp.Path)
			}
			return fmt.Sprintf("pattern=%s", truncateString(inp.Pattern, 200))
		}
	case "Glob":
		{
			var inp struct {
				Pattern string `json:"pattern"`
			}
			if err := json.Unmarshal(rawInput, &inp); err != nil {
				return truncateString(string(rawInput), 200)
			}
			return fmt.Sprintf("pattern=%s", inp.Pattern)
		}
	default:
		return fmt.Sprintf("input=%s", truncateString(string(rawInput), 200))
	}
}

func (f *flow) drainStreamJSON(r io.Reader) {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	for s.Scan() {
		line := s.Text()
		// Write raw line to stream file (do NOT re-marshal).
		if f.streamFile != nil {
			f.streamFile.WriteString(line + "\n")
		}
		var msg streamMsg
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			f.o.logger.Debug("claude_stream_nonjson",
				"issue", f.ev.IssueIdentifier, "line", truncate(line, 200))
			continue
		}
		f.o.logger.Info("claude_stream",
			"issue", f.ev.IssueIdentifier,
			"type", msg.Type,
			"subtype", msg.Subtype)

		// Emit structured logs for the 3 critical event types.

		// 1. tool_use and text events (nested inside assistant message.content)
		if msg.Type == "assistant" {
			for _, c := range msg.Message.Content {
				if c.Type == "tool_use" {
					f.o.logger.Info("claude_tool_use",
						"session", f.ev.SessionID,
						"issue", f.ev.IssueIdentifier,
						"tool", c.Name,
						"summary", summarizeToolUse(c.Name, c.Input))
				}
				if c.Type == "text" && f.awaitPendingID == "" {
					trimmed := strings.TrimSpace(c.Text)
					if strings.HasPrefix(trimmed, "ADMIRAL_AWAIT:") {
						f.awaitPendingID = strings.TrimSpace(
							strings.TrimPrefix(trimmed, "ADMIRAL_AWAIT:"))
					}
				}
			}
		}

		// 2. result events (terminal state)
		if msg.Type == "result" {
			if msg.IsError {
				f.o.logger.Warn("claude_error",
					"session", f.ev.SessionID,
					"issue", f.ev.IssueIdentifier,
					"subtype", msg.Subtype)
			} else {
				f.o.logger.Info("claude_result",
					"session", f.ev.SessionID,
					"issue", f.ev.IssueIdentifier,
					"success", true,
					"duration_ms", msg.DurationMs,
					"stop_reason", msg.StopReason,
					"subtype", msg.Subtype)
			}
		}

		// 3. standalone error type
		if msg.Type == "error" {
			f.o.logger.Warn("claude_error",
				"session", f.ev.SessionID,
				"issue", f.ev.IssueIdentifier,
				"err", truncateString(msg.Error.Message, 200))
		}
	}
}

// errPRNoCommits is returned by ensurePR when `gh pr create` fails with
// "No commits between <base> and <head>" — i.e. claude finished cleanly
// but produced no diff. The caller treats this as a soft success: mark
// the job DONE without a PR URL and post a noop reply on the thread.
var errPRNoCommits = errors.New("no commits between base and head — task understood as noop")

// ghCreateErrorKind classifies the (non-zero exit) result of `gh pr create`
// using stdout/stderr substring matching. gh has no typed error codes, so
// this is best-effort; unknown shapes fall through as fatal.
type ghCreateErrorKind int

const (
	ghCreateFatal ghCreateErrorKind = iota
	ghCreateNoCommits
	ghCreateAlreadyExists
)

// classifyGhCreateError inspects the combined stdout+stderr of a failed
// `gh pr create` invocation and tags the two known benign cases. Anything
// else is treated as a real failure to be surfaced to the caller.
//
// TODO: replace with structured-error parsing once gh CLI exposes typed
// error codes (https://github.com/cli/cli — none today).
func classifyGhCreateError(combined string) ghCreateErrorKind {
	s := strings.ToLower(combined)
	if strings.Contains(s, "no commits between") {
		return ghCreateNoCommits
	}
	if strings.Contains(s, "already exists") {
		return ghCreateAlreadyExists
	}
	return ghCreateFatal
}

// ensurePR returns the URL of an open PR with HEAD = the working branch.
// If claude already opened one, we use that. Otherwise fall back to
// `gh pr create`. Two benign failure modes from `gh pr create` are
// translated into soft outcomes:
//   - "No commits between" → returns ("", errPRNoCommits) sentinel; caller
//     marks the job DONE with no PR URL.
//   - "already exists" → looks up the existing PR with `gh pr list` and
//     returns its URL. If the lookup fails, the original create error is
//     surfaced as fatal.
func (f *flow) ensurePR(issue *linear.Issue) (string, error) {
	url, err := f.lookupPR()
	if err != nil {
		return "", err
	}
	if url != "" {
		return url, nil
	}
	if err := runCmd(f.ctx, f.worktreePath, "git", "push", "-u", "origin", f.branch); err != nil {
		return "", fmt.Errorf("git push: %w", err)
	}
	body := fmt.Sprintf("Autopilot run for %s — %s\n\n%s",
		issue.Identifier, issue.URL, truncate(issue.Description, 4000))
	out, err := captureCmd(f.ctx, f.worktreePath,
		f.o.cfg.GhBin, "pr", "create",
		"--base", f.baseBranch,
		"--head", f.branch,
		"--title", fmt.Sprintf("[%s] %s", issue.Identifier, issue.Title),
		"--body", body,
	)
	if err != nil {
		switch classifyGhCreateError(out) {
		case ghCreateNoCommits:
			return "", errPRNoCommits
		case ghCreateAlreadyExists:
			if url2, lerr := f.lookupPR(); lerr == nil && url2 != "" {
				return url2, nil
			}
			return "", fmt.Errorf("gh pr create reported already-exists but lookupPR could not recover URL: %w (output: %s)", err, truncate(out, 400))
		default:
			return "", fmt.Errorf("gh pr create: %w (output: %s)", err, truncate(out, 400))
		}
	}
	url = strings.TrimSpace(extractFirstURL(out))
	if url == "" {
		url2, lerr := f.lookupPR()
		if lerr == nil && url2 != "" {
			return url2, nil
		}
		return "", fmt.Errorf("gh pr create succeeded but no URL found in output: %s", truncate(out, 400))
	}
	return url, nil
}

func (f *flow) lookupPR() (string, error) {
	// Retry on transient gh failures (GitHub GraphQL EOF / 5xx / connection
	// reset): a single network blip here must not fail the whole job and leave
	// an already-created PR orphaned — which would also stall every sub-issue
	// blocked on this one. Mirrors the reset merged-PR guard's retry.
	gh := func(ctx context.Context, args ...string) (string, error) {
		return captureCmd(ctx, f.worktreePath, f.o.cfg.GhBin, args...)
	}
	out, err := ghReadWithRetry(f.ctx, gh, ghReadDelays,
		"pr", "list",
		"--head", f.branch,
		"--state", "open",
		"--json", "url",
		"--jq", ".[0].url",
	)
	if err != nil {
		return "", fmt.Errorf("gh pr list: %w (output: %s)", err, truncate(out, 200))
	}
	return strings.TrimSpace(out), nil
}

// --- helpers ---

func absWorktreeRootWithRepo(c *config.Autopilot, repoDir string) string {
	if filepath.IsAbs(c.WorktreeRoot) {
		return c.WorktreeRoot
	}
	return filepath.Join(repoDir, c.WorktreeRoot)
}

func branchName(i *linear.Issue) string {
	if i.Identifier != "" {
		return "linear/" + sanitizeForPath(strings.ToLower(i.Identifier))
	}
	return "linear/" + sanitizeForPath(i.ID)
}

// sanitizeForPath collapses anything outside [a-z0-9-] to '-' and trims.
// Branch and worktree subdir names go through this so a hostile identifier
// can't escape into shell or filesystem traversal.
func sanitizeForPath(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	prevDash := true
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// buildPrompt mirrors the existing TS demo's issueToContext + agent prompt.
// Format: full issue context (state / labels / description / recent comments)
// followed by whatever the user said when triggering the agent (mention
// text, delegate prompt, or "(assigned, no explicit prompt)" for raw
// assign), and a closing "Operating procedure" that tells claude exactly
// the shape of the deliverable: edit, then `git add` / `commit` / `push`
// the working branch, then `gh pr create`.
func buildPrompt(skill string, i *linear.Issue, ev linear.AgentEvent, branch, baseBranch string) string {
	var sb strings.Builder
	if skill != "" {
		sb.WriteString("/")
		sb.WriteString(skill)
		sb.WriteString("\n\n")
	}
	fmt.Fprintf(&sb, "# Issue %s: %s\n\n", i.Identifier, i.Title)
	if i.StateName != "" {
		fmt.Fprintf(&sb, "State: %s\n", i.StateName)
	}
	if len(i.Labels) > 0 {
		fmt.Fprintf(&sb, "Labels: %s\n", strings.Join(i.Labels, ", "))
	}
	sb.WriteString("\n## Description\n")
	if i.Description == "" {
		sb.WriteString("(empty)\n")
	} else {
		sb.WriteString(i.Description)
		sb.WriteString("\n")
	}
	if len(i.Comments) > 0 {
		sb.WriteString("\n## Recent comments\n")
		for _, c := range i.Comments {
			name := c.UserName
			if name == "" {
				name = "?"
			}
			fmt.Fprintf(&sb, "- %s: %s\n", name, truncate(c.Body, 400))
		}
	}
	sb.WriteString("\n## User trigger\n")
	if ev.PromptContext != "" {
		sb.WriteString(ev.PromptContext)
	} else {
		sb.WriteString("(assigned, no explicit prompt — act on the issue context above)")
	}
	fmt.Fprintf(&sb, `

## Operating procedure (you MUST follow this exactly)

You are running inside a fresh git worktree on branch %q, forked from %q.
Your final deliverable is a pull request. Do these steps in order:

1. Edit files as needed to address the issue above.
2. Stage your changes: `+"`git add -A`"+`
   (a `+"`.git/info/exclude`"+` is already configured to skip session-internal
   directories like `+"`.omc/`"+`; everything else is fair game.)
3. Commit with a clear conventional message referencing %s:
   `+"`git commit -m \"<type>(<scope>): <summary> (%s)\""+`
   (e.g. `+"`feat: add hello-world line to README (%s)`"+`).
4. Push the branch: `+"`git push -u origin %s`"+`
5. Open the PR: `+"`gh pr create --base %s --head %s --title \"[%s] %s\" --body \"<short summary plus the issue link>\"`"+`
6. Print the PR URL on a line by itself, then exit.

Do not skip any step. If you have nothing to change, still create a tiny
no-op commit (e.g. clarify a comment) so a PR can be opened — admiral's
flow expects a PR per session.

## Asking the user a question (ask_user MCP tool)

If you genuinely cannot proceed without human input, call the `+"`ask_user`"+` tool
(provided by the admiral-ask MCP server). It posts your question to the Linear
thread and returns immediately with {"status":"pending","pending_id":"<id>"}.

When ask_user returns status=pending you MUST:
1. Stop all further work immediately.
2. Output exactly one line: `+"`ADMIRAL_AWAIT:<pending_id>`"+` (replace
   <pending_id> with the id value from the tool result).
3. Exit — do NOT commit, push, or open a PR.

Admiral will resume this session automatically once the user replies.
Only use ask_user when truly blocked; never use it as a confirmation step.
`,
		branch, baseBranch,
		i.Identifier, i.Identifier, i.Identifier,
		branch,
		baseBranch, branch, i.Identifier, i.Title)
	return sb.String()
}

func runCmd(ctx context.Context, dir, name string, args ...string) error {
	out, err := captureCmd(ctx, dir, name, args...)
	if err != nil {
		return fmt.Errorf("%s %s: %w (output: %s)", name, strings.Join(args, " "), err, truncate(out, 400))
	}
	return nil
}

func captureCmd(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func extractFirstURL(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "https://") {
			return line
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// relativeTime returns a human-friendly relative time string from an RFC3339
// timestamp. Falls back to the raw string on parse failure.
func relativeTime(rfc3339 string) string {
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		return rfc3339
	}
	elapsed := time.Since(t)
	seconds := int(elapsed.Seconds())
	if seconds < 60 {
		return fmt.Sprintf("%ds ago", seconds)
	}
	minutes := seconds / 60
	if minutes < 60 {
		return fmt.Sprintf("%d min ago", minutes)
	}
	hours := minutes / 60
	if hours < 24 {
		remainingMin := minutes % 60
		return fmt.Sprintf("%dh %d min ago", hours, remainingMin)
	}
	days := hours / 24
	return fmt.Sprintf("%dd ago", days)
}

// parseMentionCommand inspects the leading non-empty, whitespace-trimmed
// token of text. Returns (name, remainder, ok) where:
//   - name is the lowercased command word without leading '/'
//   - remainder is text after the command line (may be empty)
//   - ok is true when text starts with a recognised /command
//
// Use this for ActionCreated (PromptContext) and ActionPrompted (UserMessage).
func parseMentionCommand(text string) (name, remainder string, ok bool) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "/") {
			parts := strings.SplitN(line, " ", 2)
			cmd := strings.TrimSpace(parts[0])
			name = strings.ToLower(strings.TrimPrefix(cmd, "/"))
			if len(parts) == 2 {
				remainder = strings.TrimSpace(parts[1])
			}
			return name, remainder, true
		}
		break // first non-empty line not starting with /, bare mention
	}
	return "", "", false
}

// mentionCommandHelp is the reply sent when a bare @mention is rejected.
const mentionCommandHelp = `admiral does not respond to bare @mentions. Use one of:
  /rerun <optional notes>     — start over from scratch on this issue
  /fix <description>          — patch the previous run on the existing PR with these notes
  /resume                     — continue a TIMED_OUT run from where it stopped (same session, same worktree)`

// assignFirstHelp is the reply sent when admiral receives an @mention
// or thread-prompted event for an issue it has never seen before. The
// new model (GEO-50) requires assign as the explicit kickoff signal —
// admiral does not auto-dispatch on stray mentions.
const assignFirstHelp = `Issue not assigned to admiral. Assign the issue to me first to dispatch the task; @mentions and thread messages without prior assign are not actionable.`

// repeatAssignHelp is the reply sent when an assign event arrives for
// an issue that already has prior admiral activity. New runs require
// /rerun (start over) or /fix (patch the existing PR) instead of a
// second assign.
const repeatAssignHelp = `Task already exists for this issue. Use /rerun in a comment or thread to start over, or /fix <description> to patch the current PR (planned, see GEO-49).`

// rerunCurrentlyProcessingHelp builds the reply sent when /rerun is
// requested on an issue whose live admiral_tasks row is still in a
// non-terminal state. admiral runs are serial per issue; the user
// either waits for the current run to finish or cancels it before
// starting a new one.
func rerunCurrentlyProcessingHelp(issueIdentifier string) string {
	if issueIdentifier == "" {
		return "Cannot /rerun: admiral is currently processing this issue. Wait for it to finish before retrying."
	}
	return fmt.Sprintf(
		"Cannot /rerun: admiral is currently processing %s. Wait for it to finish before retrying.",
		issueIdentifier,
	)
}

// unknownCommandHelp is the reply sent when an unknown /command is received.
func unknownCommandHelp(cmd string) string {
	return fmt.Sprintf("admiral did not recognize %q. Currently supported: /rerun, /fix.", cmd)
}

// postRejection posts an ErrorActivity for early-exit rejection paths where
// no flow has been created yet. ErrorActivity (vs Response) signals to Linear
// that the AgentSession ended in failure, so user-visible rejections show as
// errored sessions rather than completed ones.
//
// MUST NOT be used while the issue's admiral_task is still in flight — Linear
// treats ActivityError as terminal and would mark the live working session as
// errored, killing the running flow's progress updates. Use postBusyAck for
// rejection-class responses on in-flight tasks instead.
func (o *Orchestrator) postRejection(sessionID, body string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = o.lc.PostAgentActivity(ctx, sessionID, linear.ErrorActivity(body))
}

// postBlockedNotice posts a non-terminal Thought activity when a task is
// parked in BLOCKED state. Thought keeps the AgentSession alive so the
// BlockerWatcher's "resuming now" post lands in the same thread later.
func (o *Orchestrator) postBlockedNotice(sessionID, body string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = o.lc.PostAgentActivity(ctx, sessionID, linear.Thought(body, false))
}

// postBusyAck posts a non-terminal Thought activity. Use this for
// rejection-class responses delivered while the task is in flight (bare
// @mention, unknown command, re-toggled delegate, /rerun or /fix while busy)
// — Thought keeps the AgentSession alive so the running flow's subsequent
// progress posts continue to land in the same Linear thread.
func (o *Orchestrator) postBusyAck(sessionID, body string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = o.lc.PostAgentActivity(ctx, sessionID, linear.Thought(body, false))
}

// taskInFlight reports whether the admiral_task is in a state where its
// AgentSession must stay open (non-terminal). Used by replyRejection to
// choose between terminal ErrorActivity and non-terminal Thought.
func taskInFlight(state string) bool {
	return state == store.JobStateReceived ||
		state == store.JobStateExecuting ||
		state == store.JobStateAwaitingInput
}

const availableCommandsHelp = `Available commands:
  /status — show admiral state (idle/busy + current job info)
  /help   — show this help`

func (o *Orchestrator) handleCommand(ev linear.AgentEvent, cmd string) {
	o.logger.Info("command_invoked",
		"session", ev.SessionID,
		"cmd", cmd,
		"creator_id", ev.CreatorID,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	reply := func(ctx context.Context, sessionID, body string) error {
		return o.lc.PostAgentActivity(ctx, sessionID, linear.Response(body))
	}
	if o.replier != nil {
		reply = o.replier.Reply
	}

	switch cmd {
	case "status":
		active, sessionID, err := o.db.AnyAutopilotJobActive()
		if err != nil {
			o.logger.Warn("any_autopilot_job_active_failed", "err", err)
		}

		if active && sessionID != "" {
			// busy state — show current job details
			job, _ := o.db.GetAutopilotJob(sessionID)
			body := "admiral status: busy\n\nCurrent job:\n"
			if job != nil {
				issueStr := job.IssueIdentifier
				if issueStr == "" {
					issueStr = job.IssueID
				}
				body += fmt.Sprintf("  - Issue: %s\n", issueStr)
				body += fmt.Sprintf("  - Started: %s\n", relativeTime(job.StartedAt))
				body += fmt.Sprintf("  - Worktree: %s\n", orDefault(job.WorktreePath, "(none)"))
				body += fmt.Sprintf("  - Branch: %s\n", orDefault(job.Branch, "(none)"))
				body += fmt.Sprintf("  - PR: %s\n", orDefault(job.PRURL, "(not yet)"))
				body += fmt.Sprintf("  - Stream log: %s\n", orDefault(job.StreamLogPath, "(none)"))
			} else {
				body += "  - (job details unavailable)\n"
			}
			_ = reply(ctx, ev.SessionID, body)
		} else {
			// idle state — show last job info
			lastJob, err := o.db.GetLastAutopilotJob()
			if err != nil {
				o.logger.Warn("get_last_autopilot_job_failed", "err", err)
			}
			if lastJob == nil {
				_ = reply(ctx, ev.SessionID, "admiral status: idle\n\n(no previous jobs)")
				return
			}
			body := "admiral status: idle\n\nLast job:\n"
			body += fmt.Sprintf("  - Issue: %s\n", lastJob.IssueIdentifier)
			body += fmt.Sprintf("  - State: %s\n", lastJob.State)
			if lastJob.FinishedAt == "" && lastJob.State == store.JobStateDone {
				body += "  - Finished: still running?\n"
			} else {
				body += fmt.Sprintf("  - Finished: %s\n", relativeTime(lastJob.FinishedAt))
			}
			body += fmt.Sprintf("  - PR: %s\n", orDefault(lastJob.PRURL, "(none)"))
			if lastJob.State == store.JobStateFailed && lastJob.Error != "" {
				errStr := lastJob.Error
				if len(errStr) > 200 {
					errStr = errStr[:200] + "..."
				}
				body += fmt.Sprintf("  - Error: %s\n", errStr)
			}
			_ = reply(ctx, ev.SessionID, body)
		}
	case "help":
		_ = reply(ctx, ev.SessionID, availableCommandsHelp)
	default:
		body := fmt.Sprintf("Unknown command: /%s\n\n%s", cmd, availableCommandsHelp)
		_ = reply(ctx, ev.SessionID, body)
	}
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// addInconsistencyFooter appends a warning footer to the PR body when the final
// Linear activity could not be delivered, indicating the task completed successfully
// despite the thread inconsistency.
func (f *flow) addInconsistencyFooter(ctx context.Context) error {
	if f.prURL == "" {
		return nil
	}
	timestamp := time.Now().UTC().Format(time.RFC3339)
	footer := fmt.Sprintf("\n---\n\n> **Linear thread inconsistency**: admiral failed to post the final response activity to Linear thread (session: %q). The task itself completed successfully. Check admiral logs around %q for `final_activity_push_failed`.", f.ev.SessionID, timestamp)

	// Get current PR body via gh pr view.
	out, err := captureCmd(ctx, f.repoDir, f.o.cfg.GhBin, "pr", "view", extractPRNumber(f.prURL), "--json", "body", "--jq", ".body")
	if err != nil {
		f.o.logger.Warn("pr_view_for_footer_failed", "err", err, "pr", f.prURL)
		return err
	}
	currentBody := strings.TrimSpace(out)
	newBody := currentBody + footer

	_, err = captureCmd(ctx, f.repoDir, f.o.cfg.GhBin, "pr", "edit", extractPRNumber(f.prURL), "--body", newBody)
	if err != nil {
		f.o.logger.Warn("pr_edit_footer_failed", "err", err, "pr", f.prURL)
		return err
	}
	return nil
}

// extractPRNumber extracts the PR number from a PR URL like https://github.com/owner/repo/pull/123.
func extractPRNumber(prURL string) string {
	parts := strings.Split(prURL, "/")
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}
