package autopilot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	ghpkg "github.com/georgehuang/admiral/internal/github"
	"github.com/georgehuang/admiral/internal/linear"
	"github.com/georgehuang/admiral/internal/store"
	"log/slog"
)

// CIWatcher polls GitHub check runs for a PR and reports results to Linear.
type CIWatcher struct {
	lc        linearClientInterface
	db        storeInterface
	logger    *slog.Logger
	pollInter time.Duration
	timeout   time.Duration
	ghBin     string
	// maxFixRounds caps the automatic fix rounds after a CI failure (0 =
	// report only). Each round runs one fixRound call, pushes, and then
	// re-watches CI with a fresh deadline.
	maxFixRounds int
	// fixRound runs one automatic fix round on the task's PR branch (claude
	// + build gate + push). It owns its own timeout and run slot; the
	// watcher only times the polling between rounds. Nil disables the
	// fix loop entirely (GEO-54 report-only behavior).
	fixRound func(ctx context.Context, task *store.AdmiralTask, failure string) error
	// accounts resolves the repo's registered gh account to a token for
	// each poll, so the check-run lookup runs as that account.
	accounts *ghpkg.Accounts
}

// checkRun represents a single GitHub check run.
type checkRun struct {
	Name       string `json:"name"`
	Status     string `json:"status"`     // "queued", "in_progress", "completed"
	Conclusion string `json:"conclusion"` // "success", "failure", "cancelled", "neutral", etc.
	URL        string `json:"url"`        // Link to the run logs
}

// newCIWatcher creates a CIWatcher with the given poll interval and timeout.
func newCIWatcher(lc linearClientInterface, db storeInterface, logger *slog.Logger, ghBin string, pollInter, timeout time.Duration, maxFixRounds int) *CIWatcher {
	return &CIWatcher{
		lc:           lc,
		db:           db,
		logger:       logger,
		pollInter:    pollInter,
		timeout:      timeout,
		ghBin:        ghBin,
		maxFixRounds: maxFixRounds,
		accounts:     ghpkg.NewAccounts(ghBin),
	}
}

// WatchPR polls GitHub check runs for the given PR URL and reports to the
// Linear thread. It spawns a goroutine and returns immediately — the caller
// does not block. repoDir is the directory where the git repo is located
// (used for gh commands). ghUser is the repo's registered gh account; the
// poll authenticates as it.
//
// On a CI failure the watcher runs up to maxFixRounds automatic fix rounds
// (when a fixRound is wired); once the rounds are exhausted — or when no
// fixRound is configured — the admiral_tasks row is transitioned to FAILED
// with reason "ci_failed".
//
// The caller's ctx belongs to the run that opened the PR and is cancelled the
// moment that run returns — exactly when CI watching needs to start. So the
// watcher detaches from it (WithoutCancel) and terminates on its own deadline
// (w.timeout) instead.
func (w *CIWatcher) WatchPR(ctx context.Context, prURL, repoDir, sessionID, issueID, ghUser string) {
	ctx = context.WithoutCancel(ctx)
	go w.watch(ctx, prURL, repoDir, sessionID, issueID, ghUser)
}

func (w *CIWatcher) watch(ctx context.Context, prURL, repoDir, sessionID, issueID, ghUser string) {
	prNumber, err := extractPRNumberFromURL(prURL)
	if err != nil {
		w.logger.Warn("ci_watch_parse_failed", "pr_url", prURL, "err", err)
		return
	}

	// One loop iteration = one CI observation + (on failure) one fix round.
	// Each iteration gets a fresh polling deadline: a fix round can run far
	// longer than ci_watch_timeout, and the re-run CI after its push needs
	// the full window again.
	for round := 0; ; round++ {
		runs, ok := w.pollUntilFinal(ctx, repoDir, prNumber, ghUser, prURL)
		if !ok {
			return
		}

		failed := failedChecks(runs)
		if len(failed) == 0 {
			checkNames := checkNamesList(runs)
			msg := fmt.Sprintf("✅ All CI checks passed (%s). PR is ready for review: %s", checkNames, prURL)
			if round > 0 {
				msg += fmt.Sprintf("\n\n_(admiral: green after %d automatic fix round(s))_", round)
			}
			w.postResult(ctx, sessionID, linear.Response(msg))
			return
		}

		failure := describeFailedChecks(failed)

		task, err := w.db.GetAdmiralTaskByIssue(issueID)
		if err != nil || task == nil {
			w.logger.Warn("ci_fix_task_lookup_failed", "issue", issueID, "err", err)
			w.escalateCI(ctx, failure, prURL, sessionID, issueID, round)
			return
		}
		// A terminal task's branch/worktree may be gone (merged, cancelled,
		// already FAILED); fixing its PR is impossible or pointless. Report
		// and stop — do not touch the row.
		if isTerminalTaskState(task.State) {
			w.logger.Info("ci_fix_skip_terminal_task",
				"pr_url", prURL, "issue", task.IssueIdentifier, "state", task.State)
			w.postResult(ctx, sessionID, linear.Response(fmt.Sprintf(
				"❌ CI failed on PR %s, but the task is %s — not attempting a fix.\n%s",
				prURL, task.State, failure)))
			return
		}

		if round >= w.maxFixRounds || w.fixRound == nil {
			w.escalateCI(ctx, failure, prURL, sessionID, issueID, round)
			return
		}

		w.postResult(ctx, sessionID, linear.Response(fmt.Sprintf(
			"❌ CI failed:\n%s\n\nadmiral is attempting an automatic fix (round %d/%d).",
			failure, round+1, w.maxFixRounds)))

		if err := w.fixRound(ctx, task, failure); err != nil {
			w.logger.Warn("ci_fix_round_failed", "pr_url", prURL, "round", round+1, "err", err)
			w.escalateCI(ctx, fmt.Sprintf("%s\n\nFix round %d failed: %v", failure, round+1, err),
				prURL, sessionID, issueID, round+1)
			return
		}
	}
}

// pollUntilFinal polls check runs until every check has a terminal status.
// Returns the final runs, or ok=false when the watch should stop (ctx done
// or the per-round deadline exhausted without a conclusion).
func (w *CIWatcher) pollUntilFinal(ctx context.Context, repoDir string, prNumber int, ghUser, prURL string) ([]checkRun, bool) {
	ticker := time.NewTicker(w.pollInter)
	defer ticker.Stop()

	deadline := time.Now().Add(w.timeout)

	for {
		select {
		case <-ctx.Done():
			w.logger.Info("ci_watch_cancelled", "pr_url", prURL)
			return nil, false
		case <-ticker.C:
			runs, err := w.fetchCheckRuns(ctx, repoDir, prNumber, ghUser)
			if err != nil {
				w.logger.Warn("ci_fetch_runs_failed", "pr_url", prURL, "err", err)
				continue
			}

			if len(runs) == 0 {
				// No checks yet, keep polling
			} else if allFinal(runs) {
				return runs, true
			}

			if time.Now().After(deadline) {
				w.logger.Info("ci_watch_timeout", "pr_url", prURL, "timeout", w.timeout)
				return nil, false
			}
		}
	}
}

// escalateCI reports an unfixable CI failure to the Linear thread and marks
// the admiral_tasks row FAILED with reason ci_failed — the human-recovery
// path (comment /fix on the issue) starts from there.
func (w *CIWatcher) escalateCI(ctx context.Context, failure, prURL, sessionID, issueID string, roundsDone int) {
	msg := fmt.Sprintf("❌ CI failed:\n%s\n\nadmiral could not fix it after %d fix round(s). Manual intervention needed — comment `/fix` on the issue. PR: %s",
		failure, roundsDone, prURL)
	w.postResult(ctx, sessionID, linear.Response(msg))

	now := time.Now().UTC().Format(time.RFC3339)
	err := w.db.UpdateAdmiralTask(issueID, func(t *store.AdmiralTask) {
		t.State = store.JobStateFailed
		t.Error = "ci_failed"
		t.FinishedAt = now
	})
	if err != nil {
		w.logger.Warn("ci_watch_update_task_failed", "issue", issueID, "err", err)
	}
}

// describeFailedChecks renders the failing checks as a human-readable block:
// one line per check with its conclusion and (for the first) the logs URL.
func describeFailedChecks(failed []checkRun) string {
	var b strings.Builder
	for _, r := range failed {
		fmt.Fprintf(&b, "- %s (%s)\n", r.Name, r.Conclusion)
	}
	if len(failed) > 0 && failed[0].URL != "" {
		fmt.Fprintf(&b, "\nLogs: %s", failed[0].URL)
	}
	return strings.TrimRight(b.String(), "\n")
}

func (w *CIWatcher) fetchCheckRuns(ctx context.Context, repoDir string, prNumber int, ghUser string) ([]checkRun, error) {
	dir := repoDir
	if dir == "" {
		dir = "."
	}
	// The check-run lookup runs as the repo's registered gh account, so a
	// private repo's checks are visible to admiral at all.
	env, err := w.accounts.EnvFor(ctx, ghUser)
	if err != nil {
		return nil, fmt.Errorf("resolve gh account %q: %w", ghUser, err)
	}
	out, err := captureCmdEnv(ctx, dir, env, w.ghBin,
		"pr", "checks", fmt.Sprintf("%d", prNumber),
		"--json", "name,status,conclusion,url",
	)
	if err != nil {
		return nil, fmt.Errorf("gh pr checks: %w (output: %s)", err, truncate(out, 200))
	}
	var runs []checkRun
	if err := json.Unmarshal([]byte(out), &runs); err != nil {
		return nil, fmt.Errorf("parse gh pr checks output: %w", err)
	}
	return runs, nil
}

func (w *CIWatcher) postResult(ctx context.Context, sessionID string, a linear.AgentActivity) {
	delays := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second}
	var lastErr error
	for attempt := 0; attempt <= len(delays); attempt++ {
		err := w.lc.PostAgentActivity(ctx, sessionID, a)
		if err == nil {
			return
		}
		lastErr = err
		if attempt < len(delays) {
			time.Sleep(delays[attempt])
		}
	}
	w.logger.Warn("ci_watch_post_failed", "session", sessionID, "err", lastErr)
}

// extractPRNumberFromURL extracts the PR number from a GitHub PR URL.
// Supported formats:
//   - https://github.com/owner/repo/pull/123
//   - https://github.com/owner/repo/pull/123/files
func extractPRNumberFromURL(prURL string) (int, error) {
	u, err := url.Parse(prURL)
	if err != nil {
		return 0, fmt.Errorf("parse pr url: %w", err)
	}

	if u.Host == "" || !strings.Contains(u.Path, "/pull/") {
		return 0, fmt.Errorf("not a GitHub PR URL: %s", prURL)
	}

	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	for i, p := range parts {
		if p == "pull" && i+1 < len(parts) {
			if n, ok := parseInt(parts[i+1]); ok {
				return n, nil
			}
		}
	}
	return 0, fmt.Errorf("PR number not found in URL: %s", prURL)
}

func parseInt(s string) (int, bool) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, n > 0
}

// allFinal returns true when every check run has a terminal status.
func allFinal(runs []checkRun) bool {
	for _, r := range runs {
		if r.Status != "completed" {
			return false
		}
	}
	return len(runs) > 0
}

func failedChecks(runs []checkRun) []checkRun {
	var failed []checkRun
	for _, r := range runs {
		if r.Status == "completed" && r.Conclusion != "success" && r.Conclusion != "neutral" && r.Conclusion != "skipped" {
			failed = append(failed, r)
		}
	}
	return failed
}

func checkNamesList(runs []checkRun) string {
	names := make([]string, 0, len(runs))
	seen := make(map[string]bool)
	for _, r := range runs {
		if !seen[r.Name] {
			seen[r.Name] = true
			names = append(names, r.Name)
		}
	}
	return strings.Join(names, ", ")
}
