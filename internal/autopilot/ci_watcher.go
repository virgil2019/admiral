package autopilot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

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
}

// checkRun represents a single GitHub check run.
type checkRun struct {
	Name       string `json:"name"`
	Status     string `json:"status"`     // "queued", "in_progress", "completed"
	Conclusion string `json:"conclusion"` // "success", "failure", "cancelled", "neutral", etc.
	URL        string `json:"url"`        // Link to the run logs
}

// newCIWatcher creates a CIWatcher with the given poll interval and timeout.
func newCIWatcher(lc linearClientInterface, db storeInterface, logger *slog.Logger, ghBin string, pollInter, timeout time.Duration) *CIWatcher {
	return &CIWatcher{
		lc:        lc,
		db:        db,
		logger:    logger,
		pollInter: pollInter,
		timeout:   timeout,
		ghBin:     ghBin,
	}
}

// WatchPR polls GitHub check runs for the given PR URL and posts results to Linear.
// It spawns a goroutine and returns immediately — the caller does not block.
// repoDir is the directory where the git repo is located (used for gh commands).
// On CI failure, the admiral_tasks row is transitioned to FAILED with reason "ci_failed".
func (w *CIWatcher) WatchPR(ctx context.Context, prURL, repoDir, sessionID, issueID string) {
	go w.watch(ctx, prURL, repoDir, sessionID, issueID)
}

func (w *CIWatcher) watch(ctx context.Context, prURL, repoDir, sessionID, issueID string) {
	prNumber, err := extractPRNumberFromURL(prURL)
	if err != nil {
		w.logger.Warn("ci_watch_parse_failed", "pr_url", prURL, "err", err)
		return
	}

	ticker := time.NewTicker(w.pollInter)
	defer ticker.Stop()

	deadline := time.Now().Add(w.timeout)

	for {
		select {
		case <-ctx.Done():
			w.logger.Info("ci_watch_cancelled", "pr_url", prURL)
			return
		case <-ticker.C:
			runs, err := w.fetchCheckRuns(ctx, repoDir, prNumber)
			if err != nil {
				w.logger.Warn("ci_fetch_runs_failed", "pr_url", prURL, "err", err)
				continue
			}

			if len(runs) == 0 {
				// No checks yet, keep polling
			} else if allFinal(runs) {
				w.handleResult(ctx, runs, prURL, sessionID, issueID)
				return
			}

			if time.Now().After(deadline) {
				w.logger.Info("ci_watch_timeout", "pr_url", prURL, "timeout", w.timeout)
				return
			}
		}
	}
}

func (w *CIWatcher) fetchCheckRuns(ctx context.Context, repoDir string, prNumber int) ([]checkRun, error) {
	dir := repoDir
	if dir == "" {
		dir = "."
	}
	out, err := captureCmd(ctx, dir, w.ghBin,
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

func (w *CIWatcher) handleResult(ctx context.Context, runs []checkRun, prURL, sessionID, issueID string) {
	failed := failedChecks(runs)
	if len(failed) == 0 {
		checkNames := checkNamesList(runs)
		msg := fmt.Sprintf("✅ All CI checks passed (%s). PR is ready for review: %s", checkNames, prURL)
		w.postResult(ctx, sessionID, linear.Response(msg))
	} else {
		firstFail := failed[0]
		// Truncate failure output to 1k chars as per spec
		failOutput := firstFail.Conclusion
		if len(failOutput) > 1000 {
			failOutput = failOutput[:1000] + "..."
		}
		msg := fmt.Sprintf("❌ CI failed: %s - %s. Logs: %s", firstFail.Name, failOutput, firstFail.URL)
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
