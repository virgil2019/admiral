// Package autopilot is the v0.3 happy-path orchestrator: pick up a Linear
// AgentSessionEvent, create a worktree, run `claude -p`, ensure a PR was
// opened, post agent activities back into the Linear agent thread.
//
// Concurrency: single-flight. Only one job runs at a time. A second
// AgentSessionEvent that arrives while a job is in flight is rejected with
// a short "busy" response activity into the new session and dropped.
// v0.3 does not queue.
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
	"github.com/georgehuang/admiral/internal/linear"
	"github.com/georgehuang/admiral/internal/store"
	"github.com/google/uuid"
)

// storeInterface abstracts the store methods used by the orchestrator.
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
}

// linearClientInterface abstracts the linear client methods used by the orchestrator.
type linearClientInterface interface {
	PostAgentActivity(ctx context.Context, sessionID string, a linear.AgentActivity) error
	GetIssue(ctx context.Context, id string) (*linear.Issue, error)
	GetWorkflowStates(ctx context.Context, teamID string) ([]linear.WorkflowState, error)
	IssueUpdate(ctx context.Context, issueID, stateID string) error
}

type Orchestrator struct {
	cfg    *config.Autopilot
	lc     linearClientInterface
	db     storeInterface
	gh     ghProbe
	logger *slog.Logger
	ghUser string // login of the configured gh user (for open-PR author comparison)

	// workflowStatesByTeam caches workflow states per team, keyed by teamID.
	workflowStatesByTeam map[string][]linear.WorkflowState
	workflowStatesMu     sync.Mutex
}

func New(cfg *config.Autopilot, lc *linear.Client, db *store.Store, logger *slog.Logger) *Orchestrator {
	o := &Orchestrator{
		cfg:    cfg,
		lc:     lc,
		db:     db,
		gh:     newGhCLIProbe(cfg.GhBin),
		logger: logger,
		ghUser: cfg.GhUser,
	}
	// Ensure job_streams_dir exists on startup.
	if err := os.MkdirAll(cfg.JobStreamsDir, 0o755); err != nil {
		logger.Warn("job_streams_dir_mkdir", "dir", cfg.JobStreamsDir, "err", err)
	}
	return o
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
	switch ev.Action {
	case linear.ActionCreated:
		o.handleCreated(ev)
	case linear.ActionPrompted:
		o.handlePrompted(ev)
	default:
		o.logger.Warn("autopilot_unknown_action", "action", ev.Action)
	}
}

func (o *Orchestrator) handleCreated(ev linear.AgentEvent) {
	// Check for command mode: first line of PromptContext starts with /
	if ev.PromptContext != "" {
		if cmd := extractCommand(ev.PromptContext); cmd != "" {
			o.handleCommand(ev, cmd)
			return
		}
	}

	// Prior-DONE dispatch (GEO-38). When this issue already has a DONE row
	// in admiral's local DB, the user is following up on completed work.
	// We branch on the prior PR's lifecycle:
	//
	//   OPEN + claude_session_id present  → resume the same session, append
	//                                       commits to the live PR.
	//   OPEN + claude_session_id missing  → legacy row, can't resume; respond
	//                                       once and stop. The user can open
	//                                       a fresh issue for new work.
	//   MERGED / CLOSED                   → fresh follow-up flow on a new
	//                                       branch (the original is gone or
	//                                       finalized; reusing it is wrong).
	//   gh unreachable / unknown          → fall through to fresh flow rather
	//                                       than block the user.
	if ev.IssueID != "" {
		prior, err := o.db.GetLatestDoneJobByIssue(ev.IssueID)
		if err != nil {
			o.logger.Warn("get_latest_done_job_failed", "err", err, "issue", ev.IssueID)
		} else if prior != nil && prior.PRURL != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			state, stateErr := o.gh.PRState(ctx, prior.PRURL)
			cancel()
			if stateErr != nil {
				o.logger.Warn("pr_state_check_failed",
					"err", stateErr, "pr", prior.PRURL,
					"action", "falling through to fresh flow")
			}
			switch state {
			case "OPEN":
				if prior.ClaudeSessionID != "" {
					o.logger.Info("autopilot_followup_resume",
						"session", ev.SessionID, "issue", ev.IssueIdentifier,
						"prior_pr", prior.PRURL,
						"prior_claude_session", prior.ClaudeSessionID)
					go o.runFollowupResume(ev, prior)
					return
				}
				o.logger.Info("autopilot_followup_legacy_session",
					"session", ev.SessionID, "issue", ev.IssueIdentifier,
					"prior_pr", prior.PRURL)
				replyCtx, replyCancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer replyCancel()
				body := fmt.Sprintf(
					"This issue has an open PR (%s) but the original session was created before resume support landed, so I can't continue from where it left off. Open a new issue with the changes you'd like — I'll start fresh.",
					prior.PRURL)
				_ = o.lc.PostAgentActivity(replyCtx, ev.SessionID, linear.Response(body))
				return
			case "MERGED", "CLOSED":
				// Fall through to fresh flow with a follow-up branch.
				o.logger.Info("autopilot_followup_fresh_after_merged_or_closed",
					"session", ev.SessionID, "issue", ev.IssueIdentifier,
					"prior_pr", prior.PRURL, "prior_state", state)
				go o.runFollowup(ev, prior)
				return
			default:
				// "" (unlocatable) or other unknown — log and fall through.
				o.logger.Warn("pr_state_unhandled",
					"state", state, "pr", prior.PRURL,
					"action", "falling through to fresh flow")
			}
		}
	}

	// Check if this issue has a TIMED_OUT job that can be resumed.
	if ev.IssueID != "" {
		timedOut, err := o.db.GetLatestTimedOutJobByIssue(ev.IssueID)
		if err != nil {
			o.logger.Warn("get_latest_timed_out_job_failed", "err", err, "issue", ev.IssueID)
		} else if timedOut != nil && timedOut.ClaudeSessionID != "" {
			o.logger.Info("autopilot_resuming_timed_out_job",
				"session", ev.SessionID,
				"issue", ev.IssueIdentifier,
				"prior_session", timedOut.AgentSessionID,
				"claude_session", timedOut.ClaudeSessionID)
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(),
					time.Duration(o.cfg.MaxRunSeconds+120)*time.Second)
				defer cancel()
				resumeFlow := newResumeFlow(o, ctx, ev, timedOut)
				if err := resumeFlow.executeResume(); err != nil {
					o.logger.Error("autopilot_resume_timed_out_failed",
						"issue", ev.IssueIdentifier, "session", ev.SessionID, "err", err)
					resumeFlow.markFailed(err)
					return
				}
				o.logger.Info("autopilot_resume_timed_out_done",
					"issue", ev.IssueIdentifier, "session", ev.SessionID, "pr", resumeFlow.prURL)
			}()
			return
		}
	}

	// Per-session FIFO is guaranteed by the DB-level lock in ClaimNextPendingEvent,
	// so we can spawn directly without any in-process lock.
	go o.run(ev)
}

// handlePrompted handles follow-up messages in an existing agent thread
// by resuming the previous claude session on the original PR/branch.
func (o *Orchestrator) handlePrompted(ev linear.AgentEvent) {
	o.logger.Info("autopilot_prompted",
		"session", ev.SessionID, "msg_len", len(ev.UserMessage))

	job, err := o.db.GetAutopilotJob(ev.SessionID)
	if err != nil {
		o.logger.Error("get_job_for_prompted_failed", "err", err, "session", ev.SessionID)
		return
	}
	if job == nil || job.IssueID == "" {
		o.logger.Warn("prompted_without_history", "session", ev.SessionID)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = o.lc.PostAgentActivity(ctx, ev.SessionID, linear.Response(
			"I don't have history for this session — please start a fresh issue."))
		return
	}
	if job.ClaudeSessionID == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = o.lc.PostAgentActivity(ctx, ev.SessionID, linear.Response(
			"This session was created before resume support. Please open a new issue."))
		return
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(),
			time.Duration(o.cfg.MaxRunSeconds+120)*time.Second)
		defer cancel()

		resumeFlow := newResumeFlow(o, ctx, ev, job)
		if err := resumeFlow.executeResume(); err != nil {
			o.logger.Error("autopilot_resume_failed",
				"issue", ev.IssueIdentifier, "session", ev.SessionID, "err", err)
			resumeFlow.markFailed(err)
			return
		}
		o.logger.Info("autopilot_resume_done",
			"issue", ev.IssueIdentifier, "session", ev.SessionID, "pr", resumeFlow.prURL)
	}()
}

// runFollowup spawns a fresh flow for a Linear event whose issue already
// has a prior DONE job whose PR is MERGED or CLOSED. The fresh flow uses a
// follow-up branch name (suffix derived from the new session ID) so it
// doesn't collide with the original (already-merged) branch in admiral's
// local refs or the .worktrees dir. Otherwise identical to run().
func (o *Orchestrator) runFollowup(ev linear.AgentEvent, prior *store.AutopilotJob) {
	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(o.cfg.MaxRunSeconds+120)*time.Second)
	defer cancel()

	claimed, err := o.db.ClaimAutopilotJob(ev.SessionID, ev.IssueID, ev.IssueIdentifier)
	if err != nil {
		o.logger.Error("claim_followup_job_failed", "err", err, "session", ev.SessionID)
		return
	}
	if !claimed {
		o.logger.Info("followup_session_already_claimed",
			"session", ev.SessionID, "issue", ev.IssueIdentifier)
		return
	}

	flow := newFlow(o, ctx, ev)
	flow.followupSuffix = followupSuffix(ev.SessionID)
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
	o.logger.Info("autopilot_followup_done",
		"issue", ev.IssueIdentifier, "session", ev.SessionID, "pr", flow.prURL,
		"prior_pr", prior.PRURL)
}

// runFollowupResume spawns a resume flow for a Linear event whose issue
// has a prior DONE job whose PR is OPEN. claude --resume reuses the prior
// session id and pushes additional commits to the same branch (and
// therefore the same PR). After the resume, the new session's row is
// marked DONE pointing at the same artifacts so future prompted events on
// this thread can be looked up via GetAutopilotJob(ev.SessionID).
func (o *Orchestrator) runFollowupResume(ev linear.AgentEvent, prior *store.AutopilotJob) {
	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(o.cfg.MaxRunSeconds+120)*time.Second)
	defer cancel()

	claimed, err := o.db.ClaimAutopilotJob(ev.SessionID, ev.IssueID, ev.IssueIdentifier)
	if err != nil {
		o.logger.Error("claim_followup_resume_job_failed", "err", err, "session", ev.SessionID)
		return
	}
	if !claimed {
		o.logger.Info("followup_resume_session_already_claimed",
			"session", ev.SessionID, "issue", ev.IssueIdentifier)
		return
	}

	resumeFlow := newResumeFlow(o, ctx, ev, prior)
	// Hydrate the same per-flow context that flow.execute would normally
	// resolve from the issue + repos table — executeResume's helpers
	// (ensureWorktree, git fetch) need repoDir set.
	if issue, err := o.lc.GetIssue(ctx, ev.IssueID); err == nil {
		resumeFlow.teamID = issue.TeamID
		if repo, err := o.db.GetRepoByProjectID(issue.ProjectID); err == nil && repo != nil {
			resumeFlow.repoDir = repo.RepoDir
			resumeFlow.baseBranch = repo.BaseBranch
		}
	}

	if err := resumeFlow.executeResume(); err != nil {
		o.logger.Error("autopilot_followup_resume_failed",
			"issue", ev.IssueIdentifier, "session", ev.SessionID, "err", err)
		resumeFlow.markFailed(err)
		return
	}

	// executeResume updated prior.AgentSessionID's row → DONE (idempotent;
	// it was already DONE). Now also mark the new session's row DONE
	// pointing at the same artifacts so handlePrompted can resolve future
	// follow-ups via GetAutopilotJob(ev.SessionID).
	now := time.Now().UTC().Format(time.RFC3339)
	if err := o.db.UpdateAutopilotJob(ev.SessionID, func(j *store.AutopilotJob) {
		j.State = store.JobStateDone
		j.PRURL = prior.PRURL
		j.WorktreePath = prior.WorktreePath
		j.Branch = prior.Branch
		j.ClaudeSessionID = prior.ClaudeSessionID
		j.FinishedAt = now
	}); err != nil {
		o.logger.Warn("followup_resume_new_session_mark_done_failed",
			"err", err, "session", ev.SessionID)
	}

	o.logger.Info("autopilot_followup_resume_done",
		"issue", ev.IssueIdentifier, "session", ev.SessionID, "pr", prior.PRURL)
}

// followupSuffix derives a deterministic, short, fs-safe suffix from a
// Linear agent session id. The first 8 hex chars of a UUID are unique
// enough across a single workspace and stable across event retries, so
// retries land on the same branch.
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

func (o *Orchestrator) run(ev linear.AgentEvent) {
	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(o.cfg.MaxRunSeconds+120)*time.Second)
	defer cancel()

	claimed, err := o.db.ClaimAutopilotJob(ev.SessionID, ev.IssueID, ev.IssueIdentifier)
	if err != nil {
		o.logger.Error("claim_job_failed", "err", err, "session", ev.SessionID)
		return
	}
	if !claimed {
		o.logger.Info("session_already_claimed",
			"session", ev.SessionID, "issue", ev.IssueIdentifier)
		return
	}

	flow := newFlow(o, ctx, ev)
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
		"issue", ev.IssueIdentifier, "session", ev.SessionID, "pr", flow.prURL)
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
	repoDir        string
	baseBranch     string

	// followupSuffix, when non-empty, is appended to the deterministic
	// branch name and worktree path so a follow-up after a merged PR
	// doesn't collide with the (possibly still-cached) original branch.
	// Set by handleCreated for the GEO-38 fresh-follow-up path; empty for
	// the normal first run.
	followupSuffix string
}

func newFlow(o *Orchestrator, ctx context.Context, ev linear.AgentEvent) *flow {
	return &flow{o: o, ctx: ctx, ev: ev}
}

// newResumeFlow creates a flow for resuming an existing session.
func newResumeFlow(o *Orchestrator, ctx context.Context, ev linear.AgentEvent, job *store.AutopilotJob) *flow {
	return &flow{o: o, ctx: ctx, ev: ev, job: job}
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

	f.postActivity(linear.Action("ensure_pr",
		fmt.Sprintf("gh pr (%s -> %s)", f.branch, f.baseBranch),
		""))
	prURL, err := f.ensurePR(issue)
	isNoop := errors.Is(err, errPRNoCommits)
	if err != nil && !isNoop {
		return fmt.Errorf("ensure PR: %w", err)
	}
	f.prURL = prURL

	if err := f.o.db.UpdateAutopilotJob(f.ev.SessionID, func(j *store.AutopilotJob) {
		j.State = store.JobStateDone
		j.PRURL = prURL
		j.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	}); err != nil {
		return fmt.Errorf("update job to DONE: %w", err)
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
		// Add PR body footer as fallback signal.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = f.addInconsistencyFooter(ctx)
		cancel()
	}

	// Update Linear issue status to "completed" asynchronously (non-blocking).
	// Skip on noop: claude produced no diff, so admiral does not assert the
	// issue is actually done — leave that judgment to the user.
	if !isNoop && f.o.cfg.UpdateIssueStatus != nil && *f.o.cfg.UpdateIssueStatus && f.teamID != "" {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if id, err := f.o.stateIDByType(ctx, f.teamID, "completed"); err == nil && id != "" {
				if err := f.o.lc.IssueUpdate(ctx, f.ev.IssueID, id); err != nil {
					f.o.logger.Warn("issue_update_completed_failed", "err", err)
				}
			}
		}()
	}

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

// ensureWorktree re-enters the worktree if it still exists, or recreates it
// on the original branch if it was cleaned up.
func (f *flow) ensureWorktree() error {
	if _, err := os.Stat(f.job.WorktreePath); err == nil {
		f.worktreePath = f.job.WorktreePath
		f.branch = f.job.Branch
		return nil
	}

	// Worktree was cleaned up; recreate it on the original branch.
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
	return nil
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
	cctx, cancel := context.WithTimeout(f.ctx, time.Duration(f.o.cfg.MaxRunSeconds)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, f.o.cfg.ClaudeBin, args...)
	cmd.Dir = f.worktreePath
	cmd.Env = append(os.Environ(),
		"CLAUDE_AUTOPILOT_ISSUE="+f.ev.IssueIdentifier,
		"CLAUDE_AUTOPILOT_SESSION="+f.ev.SessionID,
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
	f.cleanupWorktree(cleanupArchive)
}

func (f *flow) markTimedOut(runErr error) {
	now := time.Now().UTC().Format(time.RFC3339)
	_ = f.o.db.UpdateAutopilotJob(f.ev.SessionID, func(j *store.AutopilotJob) {
		j.State = store.JobStateTimedOut
		j.Error = runErr.Error()
		j.FinishedAt = now
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
	cleanupDelete  cleanupMode = iota
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
		return fmt.Errorf("claude exit: %w", err)
	}
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

	// assistant message wrapper (tool_use lives here in Anthropic stream-json)
	Message struct {
		Content []struct {
			Type  string          `json:"type"`
			Name  string          `json:"name,omitempty"`
			Input json.RawMessage `json:"input,omitempty"`
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
	case "Edit": {
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
	case "Write": {
		var inp struct {
			FilePath string `json:"file_path"`
			Content  string `json:"content"`
		}
		if err := json.Unmarshal(rawInput, &inp); err != nil {
			return truncateString(string(rawInput), 200)
		}
		return fmt.Sprintf("file=%s (~%d chars)", inp.FilePath, len(inp.Content))
	}
	case "Read": {
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
	case "Bash": {
		var inp struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(rawInput, &inp); err != nil {
			return truncateString(string(rawInput), 200)
		}
		return fmt.Sprintf("cmd=%s", truncateString(inp.Command, 200))
	}
	case "TodoWrite": {
		var inp struct {
			Todos []struct{} `json:"todos"`
		}
		if err := json.Unmarshal(rawInput, &inp); err != nil {
			return truncateString(string(rawInput), 200)
		}
		return fmt.Sprintf("count=%d todos", len(inp.Todos))
	}
	case "Grep": {
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
	case "Glob": {
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

		// 1. tool_use events (nested inside assistant message.content)
		if msg.Type == "assistant" {
			for _, c := range msg.Message.Content {
				if c.Type == "tool_use" {
					f.o.logger.Info("claude_tool_use",
						"session", f.ev.SessionID,
						"issue", f.ev.IssueIdentifier,
						"tool", c.Name,
						"summary", summarizeToolUse(c.Name, c.Input))
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
	out, err := captureCmd(f.ctx, f.worktreePath,
		f.o.cfg.GhBin, "pr", "list",
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

func absWorktreeRoot(c *config.Autopilot) string {
	if filepath.IsAbs(c.WorktreeRoot) {
		return c.WorktreeRoot
	}
	return filepath.Join(c.RepoDir, c.WorktreeRoot)
}

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

// extractCommand returns the command word (without leading /) if the first
// non-empty line of text looks like /xxx, otherwise empty string.
func extractCommand(text string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "/") {
			parts := strings.SplitN(line, " ", 2)
			cmd := strings.TrimSpace(parts[0])
			return strings.ToLower(strings.TrimPrefix(cmd, "/"))
		}
		break // first non-empty line not starting with /, no command
	}
	return ""
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
			_ = o.lc.PostAgentActivity(ctx, ev.SessionID, linear.Response(body))
		} else {
			// idle state — show last job info
			lastJob, err := o.db.GetLastAutopilotJob()
			if err != nil {
				o.logger.Warn("get_last_autopilot_job_failed", "err", err)
			}
			if lastJob == nil {
				_ = o.lc.PostAgentActivity(ctx, ev.SessionID, linear.Response("admiral status: idle\n\n(no previous jobs)"))
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
			_ = o.lc.PostAgentActivity(ctx, ev.SessionID, linear.Response(body))
		}
	case "help":
		_ = o.lc.PostAgentActivity(ctx, ev.SessionID, linear.Response(availableCommandsHelp))
	default:
		body := fmt.Sprintf("Unknown command: /%s\n\n%s", cmd, availableCommandsHelp)
		_ = o.lc.PostAgentActivity(ctx, ev.SessionID, linear.Response(body))
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
