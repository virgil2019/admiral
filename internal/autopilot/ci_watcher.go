package autopilot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
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
	// fetchTimeout bounds a single `gh pr checks` invocation. The watcher
	// is detached from the run ctx, so a hung gh (credential-helper
	// prompt, network black hole) would otherwise block this goroutine —
	// and its child process — forever; exec only kills the child on ctx
	// cancellation.
	fetchTimeout time.Duration

	// activePRs dedups concurrent watchers on the same PR: multiple runs
	// can land on one PR (ensurePR reuses it across /rerun attempts), and
	// with the fix loop a duplicate watcher means double claude spend and
	// double escalation messages, not just a duplicate report.
	activeMu sync.Mutex
	activePR map[string]*prWatchSlot
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

// ciFetchTimeout is the default per-poll budget for one `gh pr checks`
// invocation.
const ciFetchTimeout = 60 * time.Second

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
		fetchTimeout: ciFetchTimeout,
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
	go func() {
		// The goroutine runs claude, git, and Linear posts via fixRound; a
		// panic here would take down the whole daemon (Go kills the process
		// on any unrecovered panic). Same guard as runReview.
		defer func() {
			if r := recover(); r != nil {
				w.logger.Error("ci_watch_panic", "pr_url", prURL, "panic", r)
			}
		}()
		// Claim from inside the goroutine: the caller (a run holding a
		// run-slot lease) must not block on it.
		if !w.acquireSlot(prURL, ciClaimWait) {
			w.logger.Info("ci_watch_duplicate_suppressed",
				"pr_url", prURL, "issue", issueID)
			// CI continues to be reported on the earlier session's thread;
			// tell THIS attempt's thread where to look instead of leaving
			// it silent.
			noteCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			w.postResult(noteCtx, sessionID, linear.Response(fmt.Sprintf(
				"(CI for PR %s is already being watched — results are posted to the earlier session thread.)",
				prURL)))
			return
		}
		defer w.releaseSlot(prURL)
		w.watch(ctx, prURL, repoDir, sessionID, issueID, ghUser)
	}()
}

// ciClaimWait bounds how long a new watcher waits for a same-PR watcher
// that is finishing up (its final post retries + DB write can take seconds).
// A watcher still held after this is a genuine long-lived duplicate. Var for
// test overrides.
var ciClaimWait = 15 * time.Second

// prWatchSlot is the per-PR ownership record. done is closed when the
// active watcher releases, waking any waiter to retry.
type prWatchSlot struct {
	done chan struct{}
}

// acquireSlot becomes the PR's watcher, or waits up to wait for the current
// one to release. false = a watcher was active the whole time.
//
// The wait runs inside the watcher goroutine, never on the caller's path:
// WatchPR is called from a run that still holds a run-slot lease, and
// blocking there would idle claude capacity for the wait's duration.
func (w *CIWatcher) acquireSlot(prURL string, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for {
		w.activeMu.Lock()
		if w.activePR == nil {
			w.activePR = make(map[string]*prWatchSlot)
		}
		slot, active := w.activePR[prURL]
		if !active {
			w.activePR[prURL] = &prWatchSlot{done: make(chan struct{})}
			w.activeMu.Unlock()
			return true
		}
		done := slot.done
		w.activeMu.Unlock()

		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		select {
		case <-done:
		case <-time.After(remaining):
			return false
		}
	}
}

// releaseSlot hands the PR's watch to the next waiter (or nobody).
func (w *CIWatcher) releaseSlot(prURL string) {
	w.activeMu.Lock()
	defer w.activeMu.Unlock()
	if slot, ok := w.activePR[prURL]; ok {
		close(slot.done)
		delete(w.activePR, prURL)
	}
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
	// fixedSHA is the head commit whose failing checks we already spent a
	// fix round on. GitHub registers a new head's check runs asynchronously,
	// and `gh pr checks` keeps serving the OLD head's completed results
	// until then; binding each observation to the head SHA it belongs to is
	// what stops a stale failure from burning another round on code that is
	// already fixed. Empty = nothing fixed yet (or the SHA was unreadable),
	// in which case every observation is trusted.
	fixedSHA := ""
	staleObservations := 0
	// round counts FIX ROUNDS spent, not loop iterations.
	for round := 0; ; {
		runs, sha, ok := w.pollUntilFinal(ctx, repoDir, prNumber, ghUser, prURL)
		if !ok {
			// After announcing fix rounds, silence would leave the thread
			// dangling on "attempting an automatic fix". Round 0 timeouts
			// stay silent — the pre-existing GEO-54 behavior (a PR with no
			// checks, or checks that never conclude, is common).
			if round > 0 {
				w.postResult(ctx, sessionID, linear.Response(fmt.Sprintf(
					"⏳ CI did not reach a conclusion within %s after the fix push. No verdict — check the PR: %s",
					w.timeout, prURL)))
			}
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

		// Still looking at the head we already fixed: its check results are
		// the failure that round was spent on, not a new verdict on the fix.
		if fixedSHA != "" && sha == fixedSHA {
			staleObservations++
			w.logger.Info("ci_watch_stale_head",
				"pr_url", prURL, "head", sha, "observations", staleObservations)
			// The push landed but the PR head never moved (fix produced no
			// new commit) or GitHub never propagated it — waiting longer
			// cannot resolve it.
			if staleObservations >= 2 {
				w.escalateCI(ctx, fmt.Sprintf(
					"❌ CI failed on PR %s and the fix round produced no new commit (head is still %s):\n%s\n\nManual intervention needed — use `/rerun` on the issue.",
					prURL, shortSHA(sha), describeFailedChecks(failed)), sessionID, issueID, prURL)
				return
			}
			continue
		}
		staleObservations = 0

		failure := describeFailedChecks(failed)

		// The row may have been superseded since the watch started (a
		// /rerun on the issue reuses it for a new attempt, possibly on a
		// different branch): act only when it still points at THIS watch's
		// PR. A stale watcher must not run fix rounds against — or mark
		// FAILED — the live row of another attempt.
		task, err := w.db.GetAdmiralTaskByIssue(issueID)
		if err != nil {
			// One retry — the realistic failure is a transient sqlite lock,
			// and leaving the row DONE under red CI (no ci_failed marker,
			// no recovery pointer) is worse than the retry costing a second.
			time.Sleep(time.Second)
			task, err = w.db.GetAdmiralTaskByIssue(issueID)
		}
		if err != nil {
			w.logger.Warn("ci_fix_task_lookup_failed", "issue", issueID, "err", err)
			w.postResult(ctx, sessionID, linear.Response(fmt.Sprintf(
				"❌ CI failed on PR %s:\n%s\n\n(admiral could not read the task row to record the failure: %v)",
				prURL, failure, err)))
			return
		}
		if task == nil {
			// The row was deleted (admin reset) while we were polling. We
			// cannot mark it FAILED, but the thread — and the human — still
			// need to know the PR is red.
			w.logger.Info("ci_fix_no_task", "issue", issueID)
			w.postResult(ctx, sessionID, linear.Response(fmt.Sprintf(
				"❌ CI failed on PR %s:\n%s\n\n(no admiral task row for this issue — nothing to record the failure on.)",
				prURL, failure)))
			return
		}
		if task.PRURL != prURL {
			w.logger.Info("ci_watch_stale_task",
				"pr_url", prURL, "row_pr_url", task.PRURL, "issue", issueID)
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
		// An in-flight or parked run (/fix, /rerun execution, a pending
		// ask_user answer) owns the task row and the worktree right now —
		// never race it with an automatic round; its own lifecycle reports
		// the outcome. AWAITING_INPUT especially: resetting the worktree
		// would clobber the parked run's state, and marking the row FAILED
		// would make the user's pending answer bounce.
		if task.State == store.JobStateExecuting || task.State == store.JobStateReceived ||
			task.State == store.JobStateAwaitingInput {
			w.logger.Info("ci_fix_skip_run_in_flight",
				"pr_url", prURL, "issue", task.IssueIdentifier, "state", task.State)
			w.postResult(ctx, sessionID, linear.Response(fmt.Sprintf(
				"❌ CI failed on PR %s, but a fix run is already in progress — skipping the automatic fix.\n%s",
				prURL, failure)))
			return
		}

		// Report-only when no runner is wired OR the operator configured
		// zero rounds (ci_fix_max_rounds: 0). New() always wires a runner,
		// so the config path is the one that matters in production.
		if w.fixRound == nil || w.maxFixRounds <= 0 {
			w.escalateCI(ctx, fmt.Sprintf(
				"❌ CI failed on PR %s:\n%s\n\nAutomatic CI fixing is disabled (ci_fix_max_rounds is 0). Manual intervention needed — use `/rerun` on the issue.",
				prURL, failure), sessionID, issueID, prURL)
			return
		}
		if round >= w.maxFixRounds {
			w.escalateCI(ctx, fmt.Sprintf(
				"❌ CI failed on PR %s:\n%s\n\nadmiral attempted %d automatic fix round(s) without success. Manual intervention needed — use `/rerun` on the issue.",
				prURL, failure, round), sessionID, issueID, prURL)
			return
		}

		w.postResult(ctx, sessionID, linear.Response(fmt.Sprintf(
			"❌ CI failed:\n%s\n\nadmiral is attempting an automatic fix (round %d/%d).",
			failure, round+1, w.maxFixRounds)))

		if err := w.fixRound(ctx, task, failure); err != nil {
			if errors.Is(err, errFixRoundInterrupted) {
				// The daemon is going down mid-round: leave the task DONE and
				// say so. Stamping FAILED here would strand a healthy task
				// (FAILED is terminal for /resume and the reconciler).
				w.logger.Info("ci_fix_round_interrupted", "pr_url", prURL, "round", round+1)
				// Do NOT promise continuation: CI watches are in-process
				// only, so nothing re-attaches after a restart (persisting
				// them is the follow-up). Say what actually happened.
				w.postResult(ctx, sessionID, linear.Response(fmt.Sprintf(
					"⏸ admiral is restarting — the automatic CI fix round was stopped and was NOT resumed. The task stays DONE; check the PR's CI and comment `/fix` if it still fails. PR: %s",
					prURL)))
				return
			}
			w.logger.Warn("ci_fix_round_failed", "pr_url", prURL, "round", round+1, "err", err)
			w.escalateCI(ctx, fmt.Sprintf(
				"❌ CI failed on PR %s:\n%s\n\nFix round %d failed: %v. Manual intervention needed — use `/rerun` on the issue.",
				prURL, failure, round+1, err), sessionID, issueID, prURL)
			return
		}

		// Give GitHub a moment to register the new head's check runs before
		// the re-poll: a poll landing too early resolves the previous
		// head's completed failing checks and burns a round on stale data.
		// The head-SHA binding in the loop above is what catches that case:
		// an observation tied to the head we already fixed is rejected, and
		// two consecutive rejections escalate rather than loop. (As in pollUntilFinal, the Done branch
		// is by-construction dead here — the ctx is detached — and kept
		// for future cancelable callers.)
		round++
		fixedSHA = sha
		select {
		case <-ctx.Done():
			return
		case <-time.After(w.pollInter):
		}
	}
}

// pollUntilFinal polls check runs until every check has a terminal status.
// Returns the final runs, or ok=false when the watch should stop (ctx done
// or the per-round deadline exhausted without a conclusion).
func (w *CIWatcher) pollUntilFinal(ctx context.Context, repoDir string, prNumber int, ghUser, prURL string) ([]checkRun, string, bool) {
	ticker := time.NewTicker(w.pollInter)
	defer ticker.Stop()

	deadline := time.Now().Add(w.timeout)

	for {
		select {
		// By construction this ctx is never cancelled (WatchPR detaches it
		// with WithoutCancel); the branch stays so a future cancelable
		// caller remains correct instead of polling into shutdown.
		case <-ctx.Done():
			w.logger.Info("ci_watch_cancelled", "pr_url", prURL)
			return nil, "", false
		case <-ticker.C:
			runs, head, err := w.fetchCheckRuns(ctx, repoDir, prNumber, ghUser)
			if err != nil {
				w.logger.Warn("ci_fetch_runs_failed", "pr_url", prURL, "err", err)
				// The deadline check must also cover this path: the watcher
				// is detached from the run ctx (no cancel will arrive), so
				// a persistent fetch failure — expired token, removed gh
				// binary, network partition — would otherwise poll forever.
				if time.Now().After(deadline) {
					w.logger.Info("ci_watch_timeout", "pr_url", prURL, "timeout", w.timeout)
					return nil, "", false
				}
				continue
			}

			if len(runs) == 0 {
				// No checks registered for this head yet, keep polling.
			} else if allFinal(runs) {
				// The verdict is already bound to `head`: fetchCheckRuns
				// queried that commit's checks specifically. Only the
				// head-agnostic fallback leaves it empty, in which case it is
				// resolved here.
				if head == "" {
					sha, shaErr := w.fetchHeadSHA(ctx, repoDir, prNumber, ghUser)
					if shaErr != nil {
						w.logger.Warn("ci_watch_head_sha_failed", "pr_url", prURL, "err", shaErr)
					}
					head = sha
				}
				return runs, head, true
			}

			if time.Now().After(deadline) {
				w.logger.Info("ci_watch_timeout", "pr_url", prURL, "timeout", w.timeout)
				return nil, "", false
			}
		}
	}
}

// escalateCI reports an unfixable CI failure to the Linear thread and marks
// the admiral_tasks row FAILED with reason ci_failed — the human-recovery
// path (use /rerun on the issue; /fix requires a DONE run) starts from
// there.
func (w *CIWatcher) escalateCI(ctx context.Context, msg, sessionID, issueID, prURL string) {
	w.postResult(ctx, sessionID, linear.Response(msg))

	now := time.Now().UTC().Format(time.RFC3339)
	err := w.db.UpdateAdmiralTask(issueID, func(t *store.AdmiralTask) {
		// Re-validate at WRITE time: a fix round can run for hours, and a
		// /rerun reuses the row mid-round (new attempt, different PR,
		// non-terminal state). The loop-start check alone is TOCTOU — the
		// mutation must not clobber a row that no longer points at this
		// watch's PR, nor a row that went terminal (merged mid-round) —
		// that row is the audit trail the discoverer advances on.
		if t.PRURL != prURL || isTerminalTaskState(t.State) ||
			t.State == store.JobStateExecuting || t.State == store.JobStateReceived ||
			t.State == store.JobStateAwaitingInput {
			return
		}
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

// fetchHeadSHA returns the PR's current head commit SHA — the identity a
// check-run verdict is tied to.
func (w *CIWatcher) fetchHeadSHA(ctx context.Context, repoDir string, prNumber int, ghUser string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, w.fetchTimeout)
	defer cancel()

	dir := repoDir
	if dir == "" {
		dir = "."
	}
	env, err := w.accounts.EnvFor(ctx, ghUser)
	if err != nil {
		return "", fmt.Errorf("resolve gh account %q: %w", ghUser, err)
	}
	out, err := captureCmdEnv(ctx, dir, env, w.ghBin,
		"pr", "view", fmt.Sprintf("%d", prNumber), "--json", "headRefOid")
	if err != nil {
		return "", fmt.Errorf("gh pr view: %w (output: %s)", err, truncate(out, 200))
	}
	var v struct {
		HeadRefOid string `json:"headRefOid"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return "", fmt.Errorf("parse gh pr view output: %w", err)
	}
	return v.HeadRefOid, nil
}

// shortSHA abbreviates a commit SHA for human-facing text.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func (w *CIWatcher) fetchCheckRuns(ctx context.Context, repoDir string, prNumber int, ghUser string) ([]checkRun, string, error) {
	// Bound each gh invocation: exec only kills the child on ctx
	// cancellation, and the watcher goroutine is detached from the run ctx
	// (no cancel will arrive) — a hung gh must not block the watcher, and
	// its child process, forever.
	ctx, cancel := context.WithTimeout(ctx, w.fetchTimeout)
	defer cancel()

	dir := repoDir
	if dir == "" {
		dir = "."
	}
	// The check-run lookup runs as the repo's registered gh account, so a
	// private repo's checks are visible to admiral at all.
	env, err := w.accounts.EnvFor(ctx, ghUser)
	if err != nil {
		return nil, "", fmt.Errorf("resolve gh account %q: %w", ghUser, err)
	}
	// Resolve the head FIRST and ask for that commit's check runs
	// specifically. `gh pr checks` serves whatever GitHub has finished
	// reporting for the PR, which after a push can still be the PREVIOUS
	// head's completed failures — reading that as the new head's verdict
	// spends a fix round on code that is already fixed, and with the round
	// cap reached it escalates a PR that is actually green.
	head, err := w.fetchHeadSHA(ctx, dir, prNumber, ghUser)
	if err != nil || head == "" {
		// Without a head we cannot bind the verdict; fall back to the PR
		// view and let the stale-head guard in watch() judge it.
		runs, ferr := w.fetchPRChecks(ctx, dir, prNumber, env)
		return runs, "", ferr
	}
	out, err := captureCmdEnv(ctx, dir, env, w.ghBin,
		"api", "--paginate",
		fmt.Sprintf("repos/{owner}/{repo}/commits/%s/check-runs", head),
		// One object per line (NDJSON): --paginate concatenates pages, so a
		// single JSON array per page would not parse.
		"--jq", `.check_runs[] | {name: .name, status: .status, conclusion: (.conclusion // ""), url: (.html_url // "")}`,
	)
	if err != nil {
		// An older gh or an enterprise host without the endpoint.
		runs, ferr := w.fetchPRChecks(ctx, dir, prNumber, env)
		return runs, "", ferr
	}
	runs, perr := parseCheckRunLines(out)
	if perr != nil {
		runs, ferr := w.fetchPRChecks(ctx, dir, prNumber, env)
		return runs, "", ferr
	}
	// Zero runs is a legitimate answer — GitHub has not registered this
	// head's checks yet. Returning it (rather than falling back to the
	// head-agnostic view) is what stops the previous head's failures from
	// being read as this head's verdict.
	return runs, head, nil
}

// parseCheckRunLines decodes one checkRun per line, skipping blanks.
func parseCheckRunLines(out string) ([]checkRun, error) {
	var runs []checkRun
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var r checkRun
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return nil, err
		}
		runs = append(runs, r)
	}
	return runs, nil
}

// fetchPRChecks is the head-agnostic fallback: whatever GitHub reports for
// the PR. Used only when the head SHA could not be resolved.
func (w *CIWatcher) fetchPRChecks(ctx context.Context, dir string, prNumber int, env []string) ([]checkRun, error) {
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
