package autopilot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	ghpkg "github.com/georgehuang/admiral/internal/github"
	"github.com/georgehuang/admiral/internal/linear"
	"github.com/georgehuang/admiral/internal/store"
)

// resetGhReadDelays is the backoff schedule for transient gh *read* failures
// during a reset, mirroring internal/github's ghReadRetry. The merged-PR guard
// must not abort the whole reset on a transient network blip (e.g. a GitHub
// GraphQL EOF) — only on a real, persistent failure.
var resetGhReadDelays = []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second}

// ghReadWithRetry runs a gh read command, retrying on transient failures
// (EOF / 5xx / connection reset / i/o timeout) per the given backoff. A
// non-transient error, or transient failures that outlast the schedule, return
// the last error — so callers (the merged-PR guard) still fail safe.
func ghReadWithRetry(ctx context.Context, gh ghRunner, delays []time.Duration, args ...string) (string, error) {
	var (
		out string
		err error
	)
	for attempt := 0; attempt <= len(delays); attempt++ {
		out, err = gh(ctx, args...)
		if err == nil {
			return out, nil
		}
		if !ghpkg.IsTransientGhFailure(out, err) || attempt == len(delays) {
			return out, err
		}
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-time.After(delays[attempt]):
		}
	}
	return out, err
}

// reset-task admin endpoint (POST /admin/reset-task). Fully resets a botched
// or test task — the parent issue and all its sub-issues — across GitHub
// (close PR + delete branch), Linear (sub state → backlog, drop the
// require_label), and the admiral DB (cascade delete). Whole-task granularity:
// the body is just the parent identifier.
//
// Guard: if ANY sub-issue's PR is already merged, the whole call fails and
// nothing is touched — admiral never unwinds work that already landed.

// errResetMergedPR is returned by runResetTask when the merged-PR guard trips.
// The handler maps it to 409 Conflict.
var errResetMergedPR = errors.New("reset aborted: a sub-issue PR is already merged")

// errResetParentNotFound is returned when the parent issue can't be resolved
// in Linear. The handler maps it to 404.
var errResetParentNotFound = errors.New("parent issue not found")

// resetLinear is the slice of *linear.Client the reset flow needs. Declared as
// an interface so tests can inject a fake without a live Linear server.
type resetLinear interface {
	GetIssue(ctx context.Context, id string) (*linear.Issue, error)
	GetSubIssues(ctx context.Context, parentID string) ([]linear.SubIssue, error)
	GetWorkflowStates(ctx context.Context, teamID string) ([]linear.WorkflowState, error)
	GetTeamLabelID(ctx context.Context, teamID, name string) (string, error)
	IssueUpdate(ctx context.Context, issueID, stateID string) error
	RemoveIssueLabel(ctx context.Context, issueID, labelID string) error
}

// ghRunner runs the gh binary and returns combined output. The real impl wraps
// captureCmd with the configured ghBin; tests inject a fake.
type ghRunner func(ctx context.Context, args ...string) (string, error)

// resetStore is the slice of the store the reset flow needs. Declared as an
// interface (not *store.Store) so both the admin handler (concrete *store.Store)
// and the orchestrator's /reset command (storeInterface) can drive runResetTask.
type resetStore interface {
	GetAdmiralTaskByIssue(issueID string) (*store.AdmiralTask, error)
	ResetIssueRows(issueID string) error
	DeleteTaskVerification(parentIssueID string) error
}

type resetTaskRequest struct {
	Parent string `json:"parent"`
}

type resetSubResult struct {
	IssueID    string `json:"issue_id"`
	Identifier string `json:"identifier"`
	PRURL      string `json:"pr_url,omitempty"`
	PRClosed   bool   `json:"pr_closed"`
	// Warning carries a non-fatal problem (e.g. gh close failed, Linear
	// state update errored) so the operator can act without the whole reset
	// being rolled back. Empty when the sub reset cleanly.
	Warning string `json:"warning,omitempty"`
}

type resetTaskResponse struct {
	Parent    string           `json:"parent"`
	SubsReset int              `json:"subs_reset"`
	Subs      []resetSubResult `json:"subs"`
	// Warning carries a non-fatal problem in the parent-cleanup phase (after
	// all subs were already processed). Surfaced rather than turned into a
	// bare 500 so the operator still sees which subs were reset; the reset is
	// idempotent and can be re-run.
	Warning string `json:"warning,omitempty"`
}

// resetTaskHandler handles POST /admin/reset-task.
func (s *adminServer) resetTaskHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	if s.lc == nil {
		http.Error(w, `{"error":"linear client not configured"}`, http.StatusServiceUnavailable)
		return
	}
	var req resetTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"bad json body"}`, http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Parent) == "" {
		http.Error(w, `{"error":"parent is required"}`, http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()

	gh := func(ctx context.Context, args ...string) (string, error) {
		return captureCmd(ctx, "", s.ghBin, args...)
	}
	resp, err := runResetTask(ctx, s.lc, gh, s.db, s.requireLabel, s.logger, strings.TrimSpace(req.Parent))
	if err != nil {
		switch {
		case errors.Is(err, errResetMergedPR):
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusConflict)
		case errors.Is(err, errResetParentNotFound):
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusNotFound)
		default:
			s.logger.Warn("admin_reset_task_failed", "parent", req.Parent, "err", err)
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// runResetTask is the dependency-injected core of the reset flow. Order:
//  1. Resolve parent + subs + the team's backlog state + require_label UUID.
//  2. Guard: check every sub's PR; if any is MERGED, abort touching nothing.
//  3. Act per sub: gh pr close --delete-branch, Linear state→backlog + drop
//     label, cascade-delete DB rows.
//  4. Reset the parent's own DB rows + verification loop.
//
// The parent's Linear state/label is left untouched — it holds the PRD and is
// not a worked issue. Act-phase failures are collected as per-sub warnings
// rather than aborting, since the merged guard has already ruled out the one
// unrecoverable case; the operator can re-run the (idempotent) reset.
func runResetTask(
	ctx context.Context,
	lc resetLinear,
	gh ghRunner,
	db resetStore,
	requireLabel string,
	logger *slog.Logger,
	parentID string,
) (*resetTaskResponse, error) {
	parent, err := lc.GetIssue(ctx, parentID)
	if err != nil {
		if errors.Is(err, linear.ErrIssueNotFound) {
			return nil, errResetParentNotFound
		}
		return nil, fmt.Errorf("get parent issue: %w", err)
	}
	if parent == nil {
		return nil, errResetParentNotFound
	}
	subs, err := lc.GetSubIssues(ctx, parent.ID)
	if err != nil {
		return nil, fmt.Errorf("get sub-issues: %w", err)
	}

	// Resolve the backlog state + label UUID once from the parent's team.
	// admiral keeps a task and its sub-issues inside one Linear team, so a
	// single resolution applies to every sub (mirrors the planner, which
	// resolves the pickup label once per team).
	backlogStateID, err := resolveBacklogStateID(ctx, lc, parent.TeamID)
	if err != nil {
		return nil, err
	}
	var labelID string
	if requireLabel != "" {
		labelID, err = lc.GetTeamLabelID(ctx, parent.TeamID, requireLabel)
		if err != nil {
			return nil, fmt.Errorf("resolve require_label %q: %w", requireLabel, err)
		}
	}

	// Pair each sub with the PR admiral opened for it (if any), from the DB.
	type subPR struct {
		sub   linear.SubIssue
		prURL string
	}
	paired := make([]subPR, 0, len(subs))
	for _, sub := range subs {
		prURL := ""
		if task, err := db.GetAdmiralTaskByIssue(sub.ID); err != nil {
			return nil, fmt.Errorf("lookup task for %s: %w", sub.Identifier, err)
		} else if task != nil {
			prURL = task.PRURL
		}
		paired = append(paired, subPR{sub: sub, prURL: prURL})
	}

	// Guard phase: any merged PR aborts the whole reset, untouched.
	for _, p := range paired {
		if p.prURL == "" {
			continue
		}
		merged, err := prIsMerged(ctx, gh, p.prURL)
		if err != nil {
			return nil, fmt.Errorf("check PR state for %s: %w", p.sub.Identifier, err)
		}
		if merged {
			return nil, fmt.Errorf("%w (%s: %s)", errResetMergedPR, p.sub.Identifier, p.prURL)
		}
	}

	// Act phase.
	resp := &resetTaskResponse{Parent: parent.Identifier}
	for _, p := range paired {
		res := resetSubResult{IssueID: p.sub.ID, Identifier: p.sub.Identifier, PRURL: p.prURL}
		var warns []string

		if p.prURL != "" {
			if _, err := gh(ctx, "pr", "close", p.prURL, "--delete-branch"); err != nil {
				warns = append(warns, fmt.Sprintf("gh pr close failed: %v", err))
			} else {
				res.PRClosed = true
			}
		}
		if err := lc.IssueUpdate(ctx, p.sub.ID, backlogStateID); err != nil {
			warns = append(warns, fmt.Sprintf("set state→backlog failed: %v", err))
		}
		if labelID != "" {
			if err := lc.RemoveIssueLabel(ctx, p.sub.ID, labelID); err != nil {
				warns = append(warns, fmt.Sprintf("remove label failed: %v", err))
			}
		}
		if err := db.ResetIssueRows(p.sub.ID); err != nil {
			warns = append(warns, fmt.Sprintf("db reset failed: %v", err))
		}

		if len(warns) > 0 {
			res.Warning = strings.Join(warns, "; ")
			logger.Warn("admin_reset_sub_partial", "identifier", p.sub.Identifier, "warning", res.Warning)
		}
		resp.Subs = append(resp.Subs, res)
		resp.SubsReset++
	}

	// Reset the parent's own DB footprint + the verification loop. The parent
	// rarely has admiral_tasks/discoverer_picks rows (it's not agent-ready),
	// so ResetIssueRows is usually a no-op — included for completeness.
	//
	// Failures here are surfaced as a top-level warning rather than a bare
	// 500: every sub has already been processed, so discarding resp would hide
	// that progress. The reset is idempotent — the operator can re-run.
	var parentWarns []string
	if err := db.ResetIssueRows(parent.ID); err != nil {
		parentWarns = append(parentWarns, fmt.Sprintf("reset parent rows failed: %v", err))
	}
	if err := db.DeleteTaskVerification(parent.ID); err != nil {
		parentWarns = append(parentWarns, fmt.Sprintf("delete task verification failed: %v", err))
	}
	if len(parentWarns) > 0 {
		resp.Warning = strings.Join(parentWarns, "; ")
		logger.Warn("admin_reset_parent_partial", "parent", parent.Identifier, "warning", resp.Warning)
	}

	logger.Info("admin_reset_task_done", "parent", parent.Identifier, "subs_reset", resp.SubsReset)
	return resp, nil
}

// resolveBacklogStateID returns the team's workflow state whose type is
// "backlog". Errors when the team has no backlog state, since the reset can't
// otherwise return a sub to an un-started state.
func resolveBacklogStateID(ctx context.Context, lc resetLinear, teamID string) (string, error) {
	states, err := lc.GetWorkflowStates(ctx, teamID)
	if err != nil {
		return "", fmt.Errorf("get workflow states: %w", err)
	}
	for _, st := range states {
		if st.Type == "backlog" {
			return st.ID, nil
		}
	}
	return "", fmt.Errorf("team %s has no backlog workflow state", teamID)
}

// prIsMerged reports whether the PR at prURL is in the MERGED state, via
// `gh pr view <url> --json state`.
func prIsMerged(ctx context.Context, gh ghRunner, prURL string) (bool, error) {
	out, err := ghReadWithRetry(ctx, gh, resetGhReadDelays, "pr", "view", prURL, "--json", "state")
	if err != nil {
		return false, fmt.Errorf("gh pr view: %v (output: %s)", err, truncate(out, 200))
	}
	var v struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return false, fmt.Errorf("parse gh pr view output: %w", err)
	}
	return v.State == "MERGED", nil
}
