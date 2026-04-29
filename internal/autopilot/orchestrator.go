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

// storeInterface abstracts the store methods used by the orchestrator.
type storeInterface interface {
	AnyAutopilotJobActive() (bool, string, error)
	GetLastAutopilotJob() (*store.AutopilotJob, error)
	GetAutopilotJob(sessionID string) (*store.AutopilotJob, error)
	UpdateAutopilotJob(sessionID string, fn func(*store.AutopilotJob)) error
	ClaimAutopilotJob(sessionID, issueID, identifier string) (bool, error)
}

// linearClientInterface abstracts the linear client methods used by the orchestrator.
type linearClientInterface interface {
	PostAgentActivity(ctx context.Context, sessionID string, a linear.AgentActivity) error
	GetIssue(ctx context.Context, id string) (*linear.Issue, error)
}

type Orchestrator struct {
	cfg    *config.Autopilot
	lc     linearClientInterface
	db     storeInterface
	logger *slog.Logger

	mu      sync.Mutex
	running bool
}

func New(cfg *config.Autopilot, lc *linear.Client, db *store.Store, logger *slog.Logger) *Orchestrator {
	o := &Orchestrator{cfg: cfg, lc: lc, db: db, logger: logger}
	// Ensure job_streams_dir exists on startup.
	if err := os.MkdirAll(cfg.JobStreamsDir, 0o755); err != nil {
		logger.Warn("job_streams_dir_mkdir", "dir", cfg.JobStreamsDir, "err", err)
	}
	return o
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

	o.mu.Lock()
	if o.running {
		o.mu.Unlock()
		o.logger.Info("autopilot_busy_skip",
			"issue", ev.IssueIdentifier, "session", ev.SessionID)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = o.lc.PostAgentActivity(ctx, ev.SessionID, linear.Response(
			"admiral is busy with another session. Reassign or @mention again "+
				"once the current run finishes."))
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

// handlePrompted is the v0.3 stub for follow-up messages in an existing
// agent thread. We post a polite "not yet" reply and don't touch any
// running flow. The full clarify/iterate loop is deferred to v0.4.
func (o *Orchestrator) handlePrompted(ev linear.AgentEvent) {
	o.logger.Info("autopilot_prompted_stub",
		"session", ev.SessionID, "msg_len", len(ev.UserMessage))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = o.lc.PostAgentActivity(ctx, ev.SessionID, linear.Response(
		"v0.3 doesn't handle follow-up messages yet — this thread is "+
			"single-shot. Open a new issue or reassign to start a fresh run."))
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
		o.logger.Error("autopilot_failed",
			"issue", ev.IssueIdentifier, "session", ev.SessionID, "err", err)
		flow.markFailed(err)
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

	branch       string
	worktreePath string
	prURL        string
	streamFile   *os.File
}

func newFlow(o *Orchestrator, ctx context.Context, ev linear.AgentEvent) *flow {
	return &flow{o: o, ctx: ctx, ev: ev}
}

func (f *flow) postActivity(a linear.AgentActivity) {
	if err := f.o.lc.PostAgentActivity(f.ctx, f.ev.SessionID, a); err != nil {
		f.o.logger.Warn("post_activity_failed",
			"session", f.ev.SessionID, "type", a.Type, "err", err)
	}
}

func (f *flow) execute() error {
	f.postActivity(linear.Thought("Reading issue context...", true))

	issue, err := f.o.lc.GetIssue(f.ctx, f.ev.IssueID)
	if err != nil {
		return fmt.Errorf("fetch issue: %w", err)
	}

	f.branch = branchName(issue)
	f.worktreePath = filepath.Join(
		absWorktreeRoot(f.o.cfg),
		"linear-"+sanitizeForPath(issue.Identifier),
	)
	if err := f.o.db.UpdateAutopilotJob(f.ev.SessionID, func(j *store.AutopilotJob) {
		j.State = store.JobStateExecuting
		j.WorktreePath = f.worktreePath
		j.Branch = f.branch
	}); err != nil {
		return fmt.Errorf("update job to EXECUTING: %w", err)
	}

	f.postActivity(linear.Action("worktree_create",
		fmt.Sprintf("%s @ %s", f.branch, f.o.cfg.BaseBranch),
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
		fmt.Sprintf("gh pr (%s -> %s)", f.branch, f.o.cfg.BaseBranch),
		""))
	prURL, err := f.ensurePR(issue)
	if err != nil {
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

	f.postActivity(linear.Response(fmt.Sprintf(
		"Done. PR opened: %s\n\nWorktree: `%s`\nBranch: `%s`",
		prURL, f.worktreePath, f.branch)))
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
	body := "admiral failed: " + truncate(runErr.Error(), 1500)
	if f.worktreePath != "" {
		body += "\n\nWorktree: `" + f.worktreePath + "`"
	}
	if f.branch != "" {
		body += "\nBranch: `" + f.branch + "`"
	}
	if j, err := f.o.db.GetAutopilotJob(f.ev.SessionID); err == nil && j.StreamLogPath != "" {
		body += "\n\nStream log: " + j.StreamLogPath
	}
	_ = f.o.lc.PostAgentActivity(ctx, f.ev.SessionID, linear.ErrorActivity(body))
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
	repo := f.o.cfg.RepoDir
	base := f.o.cfg.BaseBranch
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
	prompt := buildPrompt(f.o.cfg.AutopilotSkill, issue, f.ev, f.branch, f.o.cfg.BaseBranch)
	args := []string{
		"-p", prompt,
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

func (f *flow) drainStreamJSON(r io.Reader) {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	for s.Scan() {
		line := s.Text()
		// Write raw line to stream file (do NOT re-marshal).
		if f.streamFile != nil {
			f.streamFile.WriteString(line + "\n")
		}
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
