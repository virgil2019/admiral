// Package autopilot is the v0.3 happy-path orchestrator: pick up an
// assignment from Linear, create a worktree, run `claude -p`, ensure a
// PR was opened, write the result back to Linear.
//
// Concurrency: single-flight. Only one job runs at a time. A second
// assignment that arrives while a job is in flight is rejected with a
// short comment back to Linear ("busy with <other-issue>") and the new
// issue's webhook event is dropped. v0.3 does not queue.
package autopilot

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/georgehuang/admiral/internal/config"
	"github.com/georgehuang/admiral/internal/linear"
	"github.com/georgehuang/admiral/internal/store"
)

type Orchestrator struct {
	cfg    *config.Autopilot
	lcfg   *config.Linear
	lc     *linear.Client
	db     *store.Store
	logger *slog.Logger

	mu      sync.Mutex
	running bool
}

func New(cfg *config.Autopilot, lcfg *config.Linear, lc *linear.Client, db *store.Store, logger *slog.Logger) *Orchestrator {
	return &Orchestrator{cfg: cfg, lcfg: lcfg, lc: lc, db: db, logger: logger}
}

// HandleAssignment is wired up as the linear.AssignmentHandler. Returns
// quickly: the actual run happens on a background goroutine.
func (o *Orchestrator) HandleAssignment(ev linear.AssignmentEvent) {
	o.mu.Lock()
	if o.running {
		o.mu.Unlock()
		o.logger.Info("autopilot_busy_skip", "issue", ev.IssueIdentifier)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = o.lc.PostComment(ctx, ev.IssueID,
			"admiral is busy with another issue and skipped this assignment. Reassign once the current run finishes.")
		return
	}
	o.running = true
	o.mu.Unlock()

	go func() {
		defer func() {
			o.mu.Lock()
			o.running = false
			o.mu.Unlock()
		}()
		o.run(ev)
	}()
}

func (o *Orchestrator) run(ev linear.AssignmentEvent) {
	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(o.cfg.MaxRunSeconds+120)*time.Second)
	defer cancel()

	claimed, err := o.db.ClaimAutopilotJob(ev.IssueID, ev.IssueIdentifier)
	if err != nil {
		o.logger.Error("claim_job_failed", "err", err, "issue", ev.IssueIdentifier)
		return
	}
	if !claimed {
		o.logger.Info("job_already_in_flight", "issue", ev.IssueIdentifier)
		return
	}

	flow := newFlow(o, ctx, ev)
	if err := flow.execute(); err != nil {
		o.logger.Error("autopilot_failed",
			"issue", ev.IssueIdentifier, "err", err)
		flow.markFailed(err)
		return
	}
	o.logger.Info("autopilot_done",
		"issue", ev.IssueIdentifier, "pr", flow.prURL)
}

// flow carries the per-job state across the run() steps. It exists so that
// markFailed can post a meaningful comment using whatever was set so far
// (worktree path, branch) without having run() pass everything through args.
type flow struct {
	o   *Orchestrator
	ctx context.Context
	ev  linear.AssignmentEvent

	branch       string
	worktreePath string
	prURL        string
}

func newFlow(o *Orchestrator, ctx context.Context, ev linear.AssignmentEvent) *flow {
	return &flow{o: o, ctx: ctx, ev: ev}
}

func (f *flow) execute() error {
	issue, err := f.o.lc.GetIssue(f.ctx, f.ev.IssueID)
	if err != nil {
		return fmt.Errorf("fetch issue: %w", err)
	}
	if err := f.o.lc.SetIssueState(f.ctx, f.ev.IssueID, f.o.lcfg.ExecutingStateID); err != nil {
		return fmt.Errorf("set executing state: %w", err)
	}
	if err := f.o.lc.PostComment(f.ctx, f.ev.IssueID,
		"admiral picked up this issue and is running autopilot."); err != nil {
		return fmt.Errorf("post pickup comment: %w", err)
	}

	f.branch = branchName(issue)
	f.worktreePath = filepath.Join(
		absWorktreeRoot(f.o.cfg),
		"linear-"+sanitizeForPath(issue.Identifier),
	)
	if err := f.o.db.UpdateAutopilotJob(f.ev.IssueID, func(j *store.AutopilotJob) {
		j.State = store.JobStateExecuting
		j.WorktreePath = f.worktreePath
		j.Branch = f.branch
	}); err != nil {
		return fmt.Errorf("update job to EXECUTING: %w", err)
	}

	if err := f.createWorktree(); err != nil {
		return fmt.Errorf("create worktree: %w", err)
	}

	if err := f.runClaude(issue); err != nil {
		return fmt.Errorf("claude run: %w", err)
	}

	prURL, err := f.ensurePR(issue)
	if err != nil {
		return fmt.Errorf("ensure PR: %w", err)
	}
	f.prURL = prURL

	if err := f.o.db.UpdateAutopilotJob(f.ev.IssueID, func(j *store.AutopilotJob) {
		j.State = store.JobStateDone
		j.PRURL = prURL
		j.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	}); err != nil {
		return fmt.Errorf("update job to DONE: %w", err)
	}
	if err := f.o.lc.SetIssueState(f.ctx, f.ev.IssueID, f.o.lcfg.DoneStateID); err != nil {
		return fmt.Errorf("set done state: %w", err)
	}
	if err := f.o.lc.PostComment(f.ctx, f.ev.IssueID,
		fmt.Sprintf("admiral finished. PR: %s", prURL)); err != nil {
		return fmt.Errorf("post done comment: %w", err)
	}
	return nil
}

func (f *flow) markFailed(runErr error) {
	now := time.Now().UTC().Format(time.RFC3339)
	_ = f.o.db.UpdateAutopilotJob(f.ev.IssueID, func(j *store.AutopilotJob) {
		j.State = store.JobStateFailed
		j.Error = runErr.Error()
		j.FinishedAt = now
	})
	// We deliberately don't bail on context.Done here; markFailed runs
	// even if the parent ctx already expired (use a fresh short ctx).
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = f.o.lc.SetIssueState(ctx, f.ev.IssueID, f.o.lcfg.FailedStateID)
	body := "admiral failed: " + truncate(runErr.Error(), 1500)
	if f.worktreePath != "" {
		body += "\n\nWorktree: `" + f.worktreePath + "`"
	}
	if f.branch != "" {
		body += "\nBranch: `" + f.branch + "`"
	}
	_ = f.o.lc.PostComment(ctx, f.ev.IssueID, body)
}

// createWorktree fetches origin/<base> and creates a fresh worktree. If the
// directory already exists (e.g. a prior failed run), it's removed first.
func (f *flow) createWorktree() error {
	repo := f.o.cfg.RepoDir
	base := f.o.cfg.BaseBranch
	if err := runCmd(f.ctx, repo, "git", "fetch", "origin", base); err != nil {
		return fmt.Errorf("git fetch origin %s: %w", base, err)
	}
	// Best-effort cleanup of a prior worktree at the same path. `git worktree
	// remove` will fail if the path doesn't exist; ignore that. If a stale
	// branch with the same name exists locally, drop it too.
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
// drains stdout until exit. We log each line as a child event but don't try
// to interpret the protocol — v0.3 trusts claude to either complete or fail.
func (f *flow) runClaude(issue *linear.Issue) error {
	prompt := buildPrompt(f.o.cfg.AutopilotSkill, issue)
	args := []string{
		"-p", prompt,
		"--output-format", "stream-json",
		"--verbose",
		"--permission-mode", "acceptEdits",
	}
	cctx, cancel := context.WithTimeout(f.ctx, time.Duration(f.o.cfg.MaxRunSeconds)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, f.o.cfg.ClaudeBin, args...)
	cmd.Dir = f.worktreePath
	cmd.Env = append(os.Environ(), "CLAUDE_AUTOPILOT_ISSUE="+issue.Identifier)

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

// drainStreamJSON reads claude's stream-json output line by line and logs
// the type/subtype + a short text snippet. We do NOT need the full content
// for v0.3 — committing + PR creation is delegated to claude itself, and
// our success signal is exit code 0 + presence of a PR.
func (f *flow) drainStreamJSON(r io.Reader) {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	for s.Scan() {
		line := s.Text()
		var msg struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
		}
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			f.o.logger.Debug("claude_stream_nonjson",
				"issue", f.ev.IssueIdentifier, "line", truncate(line, 200))
			continue
		}
		f.o.logger.Info("claude_stream",
			"issue", f.ev.IssueIdentifier,
			"type", msg.Type,
			"subtype", msg.Subtype)
	}
}

// ensurePR returns the URL of an open PR with HEAD = the working branch.
// If claude already opened one, we use that. Otherwise fall back to
// `gh pr create`.
func (f *flow) ensurePR(issue *linear.Issue) (string, error) {
	url, err := f.lookupPR()
	if err != nil {
		return "", err
	}
	if url != "" {
		return url, nil
	}
	// Push the branch to origin first so gh pr create has a remote head.
	if err := runCmd(f.ctx, f.worktreePath, "git", "push", "-u", "origin", f.branch); err != nil {
		return "", fmt.Errorf("git push: %w", err)
	}
	body := fmt.Sprintf("Autopilot run for %s — %s\n\n%s",
		issue.Identifier, issue.URL, truncate(issue.Description, 4000))
	out, err := captureCmd(f.ctx, f.worktreePath,
		f.o.cfg.GhBin, "pr", "create",
		"--base", f.o.cfg.BaseBranch,
		"--head", f.branch,
		"--title", fmt.Sprintf("[%s] %s", issue.Identifier, issue.Title),
		"--body", body,
	)
	if err != nil {
		return "", fmt.Errorf("gh pr create: %w (output: %s)", err, truncate(out, 400))
	}
	url = strings.TrimSpace(extractFirstURL(out))
	if url == "" {
		// gh prints the URL as the only output line on success; if we
		// didn't find one, retry the lookup once.
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

func branchName(i *linear.Issue) string {
	if i.Identifier != "" {
		return "linear/" + sanitizeForPath(strings.ToLower(i.Identifier))
	}
	return "linear/" + sanitizeForPath(i.ID)
}

// sanitizeForPath collapses anything outside [a-z0-9-] to '-' and trims.
// The branch and worktree subdir names go through this so a hostile
// identifier can't escape into shell or filesystem traversal.
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

func buildPrompt(skill string, i *linear.Issue) string {
	header := fmt.Sprintf("Linear issue %s: %s", i.Identifier, i.Title)
	body := i.Description
	if body == "" {
		body = "(no description)"
	}
	footer := "When done, commit your changes and open a PR via `gh pr create` against the base branch."
	parts := []string{header, "", body, "", footer}
	if skill != "" {
		parts = append([]string{"/" + skill, ""}, parts...)
	}
	return strings.Join(parts, "\n")
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
