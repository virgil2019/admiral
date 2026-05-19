package autopilot

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/georgehuang/admiral/internal/linear"
	"github.com/georgehuang/admiral/internal/store"
)

// reviewPayload is the minimal subset of GitHub PR review webhook payloads
// that the review dispatcher needs. Covers both pull_request_review and
// pull_request_review_comment event types.
type reviewPayload struct {
	Review *struct {
		Body  string `json:"body"`
		State string `json:"state"`
	} `json:"review,omitempty"`
	Comment *struct {
		Body string `json:"body"`
	} `json:"comment,omitempty"`
}

// GitHub PR review states (lowercase in webhook payloads).
const (
	reviewStateApproved         = "approved"
	reviewStateChangesRequested = "changes_requested"
	reviewStateCommented        = "commented"
)

// HandleReviewEvent processes a source='github' event from events_inbox.
// Called by the worker; returns quickly — the claude run is dispatched to a
// background goroutine via runReview.
func (o *Orchestrator) HandleReviewEvent(ctx context.Context, row *store.EventInboxRow) {
	prURL := row.SessionID
	if prURL == "" {
		o.logger.Warn("review_dispatch_empty_pr_url", "webhook_id", row.WebhookID)
		return
	}

	task, err := o.db.GetAdmiralTaskByPRURL(prURL)
	if err != nil {
		o.logger.Error("review_dispatch_lookup_failed",
			"pr", prURL, "err", err, "webhook_id", row.WebhookID)
		return
	}
	if task == nil {
		o.logger.Info("review_dispatch_no_task", "pr", prURL)
		return
	}
	if task.Branch == "" {
		o.logger.Warn("review_dispatch_no_branch",
			"pr", prURL, "issue", task.IssueIdentifier)
		return
	}

	reviewBody := extractReviewBody(row.PayloadJSON, row.Action)
	reviewState := extractReviewState(row.PayloadJSON, row.Action)

	// APPROVED with no body has no code feedback to address. Skip the claude
	// run and just notify the Linear thread that the PR is ready for human
	// verification. Reviewers who Approve *and* leave inline comments produce
	// pull_request_review_comment events separately, so that feedback is not
	// dropped on the floor.
	if reviewState == reviewStateApproved && reviewBody == "" {
		go o.postReviewApprovedNotice(task)
		return
	}

	replier := NewGitHubReplier(o.prClient, prURL)
	go o.runReview(task, reviewBody, replier)
}

// runReview is the background goroutine for a GitHub review event. Steps:
//  1. Resolve the repo directory via the Linear issue.
//  2. Fetch the PR diff for context (non-fatal if unavailable).
//  3. Ensure the worktree exists (rebuild from remote if cleaned up).
//  4. Run `claude -p` with a review prompt.
//  5. Push the branch to origin.
//  6. Post the claude output as a PR comment.
//
// Events_inbox lifecycle: the events_inbox row is marked done by the worker
// immediately after HandleReviewEvent returns — same as the Linear dispatch
// model where events_inbox tracks delivery only, not task completion. Failures
// inside runReview are surfaced via the PR comment (best-effort) and logs.
func (o *Orchestrator) runReview(task *store.AdmiralTask, reviewBody string, replier Replier) {
	defer func() {
		if r := recover(); r != nil {
			o.logger.Error("review_run_panic", "pr", task.PRURL, "panic", r)
		}
	}()

	release := o.acquireRunSlot()
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(o.cfg.MaxRunSeconds+120)*time.Second)
	defer cancel()

	// Fetch the PR diff inside the goroutine so the worker's drain loop is
	// not blocked by the network call.
	diff, err := o.prClient.GetDiff(ctx, task.PRURL)
	if err != nil {
		o.logger.Warn("review_run_get_diff_failed", "pr", task.PRURL, "err", err)
		// Non-fatal: proceed without diff context.
	}

	repoDir, baseBranch, err := o.resolveRepoForReview(ctx, task)
	if err != nil {
		o.logger.Error("review_run_resolve_repo_failed",
			"issue", task.IssueIdentifier, "err", err)
		_ = replier.Fail(ctx, "admiral: cannot address review — could not resolve repo for this issue.")
		return
	}

	worktreePath, err := ensureReviewWorktree(ctx, repoDir, task)
	if err != nil {
		o.logger.Error("review_run_worktree_failed",
			"pr", task.PRURL, "branch", task.Branch, "err", err)
		_ = replier.Fail(ctx, fmt.Sprintf(
			"admiral: cannot address review — failed to prepare worktree for branch %s: %v",
			task.Branch, err))
		return
	}

	prompt := buildReviewPrompt(task.PRURL, task.Branch, baseBranch, reviewBody, diff)

	output, err := runClaudeForReview(ctx, o.cfg.ClaudeBin, o.cfg.MaxRunSeconds, worktreePath, prompt, o.logger)
	if err != nil {
		o.logger.Error("review_run_claude_failed", "pr", task.PRURL, "err", err)
		_ = replier.Fail(ctx, fmt.Sprintf("admiral: review run failed: %v", err))
		return
	}

	if pushErr := runCmd(ctx, worktreePath, "git", "push", "origin", task.Branch); pushErr != nil {
		o.logger.Warn("review_run_push_failed",
			"pr", task.PRURL, "branch", task.Branch, "err", pushErr)
		output += "\n\n(note: push to origin failed — commits are local only)"
	}

	if output == "" {
		output = "addressed"
	}
	_ = replier.Reply(ctx, output)

	// Best-effort: notify the Linear thread that the review has been addressed
	// so the issue creator knows to come re-review. Failures are logged at
	// warn level — the PR comment above is the authoritative signal.
	o.postReviewHandledNotice(task)
}

// resolveRepoForReview fetches the Linear issue for the task and returns the
// configured repo directory and base branch.
func (o *Orchestrator) resolveRepoForReview(ctx context.Context, task *store.AdmiralTask) (repoDir, baseBranch string, err error) {
	issue, err := o.lc.GetIssue(ctx, task.IssueID)
	if err != nil {
		return "", "", fmt.Errorf("get issue: %w", err)
	}
	repo, err := o.db.GetRepoByProjectID(issue.ProjectID)
	if err != nil {
		return "", "", fmt.Errorf("get repo for project %s: %w", issue.ProjectID, err)
	}
	if repo == nil {
		return "", "", fmt.Errorf("no repo configured for project %s", issue.ProjectID)
	}
	return repo.RepoDir, repo.BaseBranch, nil
}

// ensureReviewWorktree returns a ready worktree path for task.Branch. If
// task.WorktreePath still exists on disk it is reused directly. Otherwise
// the branch is fetched from origin and a new worktree is created at the
// recorded path (or a derived path when WorktreePath is empty).
func ensureReviewWorktree(ctx context.Context, repoDir string, task *store.AdmiralTask) (string, error) {
	worktreePath := task.WorktreePath
	if worktreePath != "" {
		if _, err := os.Stat(worktreePath); err == nil {
			return worktreePath, nil
		}
	}

	// Derive a path from the branch name when none is recorded or the
	// recorded one was cleaned up.
	if worktreePath == "" {
		name := strings.TrimPrefix(task.Branch, "linear/")
		worktreePath = filepath.Join(repoDir, ".worktrees", "review-"+name)
	}

	if err := os.MkdirAll(filepath.Dir(worktreePath), 0o755); err != nil {
		return "", fmt.Errorf("mkdir worktree parent: %w", err)
	}
	// Remove any stale worktree registration before recreating.
	_ = runCmd(ctx, repoDir, "git", "worktree", "remove", "--force", worktreePath)

	if err := runCmd(ctx, repoDir, "git", "fetch", "origin", task.Branch); err != nil {
		return "", fmt.Errorf("git fetch %s: %w", task.Branch, err)
	}
	if err := runCmd(ctx, repoDir, "git", "worktree", "add", worktreePath, task.Branch); err != nil {
		return "", fmt.Errorf("git worktree add: %w", err)
	}
	return worktreePath, nil
}

// runClaudeForReview runs `claude -p <prompt> --dangerously-skip-permissions`
// in the worktree and captures stdout as the text reply. Plain text output
// mode is used (no stream-json) because progress streaming to a PR comment
// doesn't apply.
func runClaudeForReview(ctx context.Context, claudeBin string, maxRunSeconds int, worktreePath, prompt string, logger *slog.Logger) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, time.Duration(maxRunSeconds)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(cctx, claudeBin,
		"-p", prompt,
		"--dangerously-skip-permissions",
	)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second
	cmd.Dir = worktreePath
	cmd.Env = os.Environ()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("claude stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", fmt.Errorf("claude stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("claude start: %w", err)
	}

	var sb strings.Builder
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 1024*1024), 4*1024*1024)
		for sc.Scan() {
			sb.WriteString(sc.Text())
			sb.WriteByte('\n')
		}
	}()
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 0, 1024*1024), 4*1024*1024)
		for sc.Scan() {
			logger.Warn("claude_review_stderr", "line", sc.Text())
		}
	}()
	wg.Wait()

	if err := cmd.Wait(); err != nil {
		if cctx.Err() != nil {
			return "", fmt.Errorf("claude exit: %w: %w", err, context.DeadlineExceeded)
		}
		return "", fmt.Errorf("claude exit: %w", err)
	}
	return strings.TrimSpace(sb.String()), nil
}

// extractReviewBody returns the human-readable text from the GitHub webhook
// payload JSON. row.Action is "pull_request_review.submitted" or
// "pull_request_review_comment.created".
func extractReviewBody(payloadJSON, action string) string {
	var p reviewPayload
	if err := json.Unmarshal([]byte(payloadJSON), &p); err != nil {
		return ""
	}
	if strings.HasPrefix(action, "pull_request_review_comment") {
		if p.Comment != nil {
			return strings.TrimSpace(p.Comment.Body)
		}
	}
	if p.Review != nil {
		return strings.TrimSpace(p.Review.Body)
	}
	return ""
}

// extractReviewState returns the lowercased review state ("approved",
// "changes_requested", "commented") for pull_request_review.submitted events.
// Returns "" for pull_request_review_comment events, which carry no state.
func extractReviewState(payloadJSON, action string) string {
	if strings.HasPrefix(action, "pull_request_review_comment") {
		return ""
	}
	var p reviewPayload
	if err := json.Unmarshal([]byte(payloadJSON), &p); err != nil {
		return ""
	}
	if p.Review == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(p.Review.State))
}

// postReviewApprovedNotice notifies the Linear thread that an external
// reviewer approved the PR. No claude run happens — the PR is awaiting human
// verification + merge.
func (o *Orchestrator) postReviewApprovedNotice(task *store.AdmiralTask) {
	body := fmt.Sprintf(
		"Reviewer approved PR — human verify and merge: %s",
		task.PRURL,
	)
	o.postReviewLinearNotice(task, linear.Response(body))
}

// postReviewHandledNotice notifies the Linear thread that admiral addressed a
// reviewer's feedback by pushing follow-up commits. Reviewer is invited to
// re-review.
func (o *Orchestrator) postReviewHandledNotice(task *store.AdmiralTask) {
	body := fmt.Sprintf(
		"Addressed review feedback — please re-review: %s",
		task.PRURL,
	)
	o.postReviewLinearNotice(task, linear.Response(body))
}

// postReviewLinearNotice posts a single activity to the Linear agent session
// recorded on the admiral task. Skips silently when no session id is recorded
// (e.g. task created by a pre-LastEventSessionID-flow path).
func (o *Orchestrator) postReviewLinearNotice(task *store.AdmiralTask, a linear.AgentActivity) {
	if task.LastEventSessionID == "" {
		o.logger.Info("review_notice_skip_no_session",
			"pr", task.PRURL, "issue", task.IssueIdentifier)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := o.lc.PostAgentActivity(ctx, task.LastEventSessionID, a); err != nil {
		o.logger.Warn("review_notice_post_failed",
			"pr", task.PRURL, "issue", task.IssueIdentifier, "err", err)
	}
}

// buildReviewPrompt constructs the prompt passed to claude when addressing a
// PR review comment.
func buildReviewPrompt(prURL, branch, baseBranch, reviewBody, diff string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "A reviewer has left feedback on PR %s (branch: %s, base: %s).\n\n",
		prURL, branch, baseBranch)
	if reviewBody != "" {
		fmt.Fprintf(&b, "Review comment:\n%s\n\n", reviewBody)
	}
	if diff != "" {
		fmt.Fprintf(&b, "PR diff for context:\n```diff\n%s\n```\n\n", diff)
	}
	b.WriteString("Please address the review feedback by making the appropriate code changes in this repository. ")
	fmt.Fprintf(&b, "Stage and commit your changes to branch %q. Do NOT open a new PR.\n", branch)
	b.WriteString("Summarize what you changed in 1–3 sentences for the review reply.")
	return b.String()
}
