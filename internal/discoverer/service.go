// Package discoverer scans Linear for assignable issues whose Linear
// project has been opted in (admin UI toggle on the repos table),
// optionally runs a `claude -p` "can this be done autonomously?"
// judge, then self-assigns matches to admiral's Linear user. The
// downstream Linear webhook is what admiral-autopilot picks up — no
// orchestrator or worker changes are needed.
//
// Designed to be split off into its own repo later: the linear and
// store dependencies are read-mostly and gated behind narrow
// interfaces, and the writes admiral-discoverer DOES perform
// (discoverer_picks) are namespaced to its own table.
//
// Dedup model:
//   - admiral_tasks live row → skip (autopilot is already running on it)
//   - discoverer_picks row with picked_state == current Linear state
//     → skip (we've handled this issue, no external reset signal)
//   - Otherwise, judge (if enabled) and assign. After assign, upsert a
//     fresh picks row with the new state.
//
// "External reset" = the Linear state changed since we last picked.
// Operators flip the state to retry; admiral does not retry by itself.
package discoverer

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/georgehuang/admiral/internal/linear"
	"github.com/georgehuang/admiral/internal/store"
)

// Config holds the runtime configuration for the discoverer service.
// Project scope is NOT here — it lives in the admin UI / repos table.
type Config struct {
	PollInterval    time.Duration
	StateTypes      []string
	RequireLabel    string
	MaxPickPerRound int
	AdmiralUserID   string
	Judge           JudgeConfig
	// LinearStates maps the admiral workflow stages onto Linear
	// workflow-state names. Empty entries are skipped (transition is
	// not pushed to Linear). The merged-PR transition does not consult
	// this map: it uses Linear state.type=completed as a stable target.
	LinearStates LinearStateMap
}

// LinearStateMap holds optional per-team Linear state names.
type LinearStateMap struct {
	// InReview is the Linear state to push when admiral's PR is open
	// and has no approval yet (typically "In Review"). Empty = skip.
	InReview string
	// Reviewed is the Linear state to push when admiral's PR is open
	// and has at least one approval (typically "Reviewed"). Empty =
	// skip.
	Reviewed string
}

// JudgeConfig configures the claude -p judge step.
type JudgeConfig struct {
	Enabled   bool
	ClaudeBin string
	Timeout   time.Duration
}

// linearClient is the slice of the Linear client the discoverer uses.
type linearClient interface {
	SearchAssignableIssues(ctx context.Context, f linear.SearchFilter) ([]linear.Issue, error)
	AssignIssue(ctx context.Context, issueID, userID string) error
	GetIssueBlockers(ctx context.Context, issueID string) ([]linear.IssueBlocker, error)
	GetIssue(ctx context.Context, id string) (*linear.Issue, error)
	GetWorkflowStates(ctx context.Context, teamID string) ([]linear.WorkflowState, error)
	IssueUpdate(ctx context.Context, issueID, stateID string) error
}

// prClient is the slice of the GitHub client the discoverer uses for
// state-advance polling.
type prClient interface {
	GetPRStatus(ctx context.Context, prURL string) (PRStatus, error)
}

// PRStatus is the discoverer's view of a GitHub PR. cmd/admiral-discoverer
// adapts from internal/github.PRStatus so this package does not pull
// internal/github into its own dependency surface — keeps the split-
// off path clean.
type PRStatus struct {
	State             string
	MergedAt          string
	HasApprovedReview bool
}

// taskRegistry is the slice of the store the discoverer needs:
//   - GetAdmiralTaskByIssue: tertiary dedup against in-flight autopilot work
//   - ListAutoPickEnabledProjectIDs: source of truth for scan scope
//   - GetDiscovererPick / UpsertDiscovererPick: own dedup record
//   - ListAdmiralTasksByStates / UpdateAdmiralTask: drives the
//     Linear-state advancement on DONE tasks
type taskRegistry interface {
	GetAdmiralTaskByIssue(issueID string) (*store.AdmiralTask, error)
	ListAutoPickEnabledProjectIDs() ([]string, error)
	GetDiscovererPick(issueID string) (*store.DiscovererPick, error)
	UpsertDiscovererPick(p store.DiscovererPick) error
	ListAdmiralTasksByStates(states []string) ([]store.AdmiralTask, error)
	UpdateAdmiralTask(issueID string, fn func(*store.AdmiralTask)) error
}

// judger is the judge interface — easier to fake than the concrete
// claude -p runner in unit tests.
type judger interface {
	Judge(ctx context.Context, iss linear.Issue) (Verdict, error)
}

// Service is the discoverer daemon.
type Service struct {
	cfg    Config
	linear linearClient
	pr     prClient
	store  taskRegistry
	judge  judger
	logger *slog.Logger

	// workflowStatesCache memoises Linear team workflow-state lookups
	// across ticks — they rarely change.
	workflowStatesCache map[string][]linear.WorkflowState
}

// New constructs a discoverer Service. When cfg.Judge.Enabled is true
// and judge is nil, a default claudeJudge is wired. pr may be nil; the
// state-advance phase is then a no-op (the scan phase still runs).
func New(cfg Config, lc linearClient, pr prClient, tr taskRegistry, j judger, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	logger = logger.With("service", "discoverer")
	if cfg.Judge.Enabled && j == nil {
		j = &claudeJudge{
			claudeBin: cfg.Judge.ClaudeBin,
			timeout:   cfg.Judge.Timeout,
			logger:    logger,
		}
	}
	return &Service{
		cfg:                 cfg,
		linear:              lc,
		pr:                  pr,
		store:               tr,
		judge:               j,
		logger:              logger,
		workflowStatesCache: map[string][]linear.WorkflowState{},
	}
}

// Run blocks until ctx is cancelled. Performs an immediate scan on
// entry, then ticks every cfg.PollInterval.
func (s *Service) Run(ctx context.Context) error {
	if s.cfg.PollInterval <= 0 {
		return fmt.Errorf("PollInterval must be > 0")
	}
	if s.cfg.AdmiralUserID == "" {
		return fmt.Errorf("AdmiralUserID is required")
	}
	s.logger.Info("discoverer_started",
		"poll_interval", s.cfg.PollInterval,
		"label", s.cfg.RequireLabel,
		"judge_enabled", s.cfg.Judge.Enabled,
		"max_pick_per_round", s.cfg.MaxPickPerRound,
	)

	t := time.NewTicker(s.cfg.PollInterval)
	defer t.Stop()
	s.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			s.logger.Info("discoverer_stopped")
			return ctx.Err()
		case <-t.C:
			s.tick(ctx)
		}
	}
}

func (s *Service) tick(ctx context.Context) {
	s.scanAndPick(ctx)
	if ctx.Err() != nil {
		return
	}
	s.advanceLinearStates(ctx)
}

func (s *Service) scanAndPick(ctx context.Context) {
	projectIDs, err := s.store.ListAutoPickEnabledProjectIDs()
	if err != nil {
		s.logger.Error("project_list_failed", "err", err)
		return
	}
	if len(projectIDs) == 0 {
		s.logger.Debug("scan_skipped_no_auto_pick_projects")
		return
	}

	filter := linear.SearchFilter{
		ProjectIDs:     projectIDs,
		StateTypes:     s.cfg.StateTypes,
		RequireLabel:   s.cfg.RequireLabel,
		UnassignedOnly: true,
	}
	issues, err := s.linear.SearchAssignableIssues(ctx, filter)
	if err != nil {
		s.logger.Error("scan_failed",
			"err", err,
			"projects", projectIDs,
			"label", s.cfg.RequireLabel,
		)
		return
	}
	s.logger.Info("scan_complete", "candidates", len(issues), "projects", len(projectIDs))

	picked := 0
	for _, iss := range issues {
		if s.cfg.MaxPickPerRound > 0 && picked >= s.cfg.MaxPickPerRound {
			s.logger.Info("scan_round_pick_limit_reached", "limit", s.cfg.MaxPickPerRound)
			return
		}
		if ctx.Err() != nil {
			return
		}
		if s.consider(ctx, iss) {
			picked++
		}
	}
}

// consider returns true iff the issue was assigned to admiral in this
// round. Skipping (dedup, judge=no, error) returns false.
//
// Dedup runs in two phases to keep the race window narrow:
//
//	phase 1 (cheap, pre-judge): autopilot live row + own picks row
//	phase 2 (after judge, pre-assign): re-check autopilot live row so a
//	  60s judge window doesn't let a parallel human assign sneak past
//
// The ultimate idempotency boundary is Linear itself: assigning the
// same userID twice is a no-op (success=true, no webhook fires) and
// autopilot's INSERT-OR-IGNORE on admiral_tasks tolerates a double
// webhook. Both phase-2 race and same-issue twice-pick from concurrent
// processes are safe; we narrow the window mostly to reduce log noise.
func (s *Service) consider(ctx context.Context, iss linear.Issue) bool {
	if s.alreadyTracked(iss) {
		return false
	}
	if pick := s.lookupPick(iss); pick != nil {
		if pick.PickedState == iss.StateName {
			s.logger.Debug("skip_already_picked",
				"issue", iss.Identifier,
				"picked_state", pick.PickedState,
				"picked_at", pick.PickedAt,
			)
			return false
		}
		s.logger.Info("repick_state_changed",
			"issue", iss.Identifier,
			"old_state", pick.PickedState,
			"new_state", iss.StateName,
		)
	}

	// Skip issues with unresolved blocked_by relations before the judge so we
	// don't pay claude -p cost on something autopilot would just park as
	// BLOCKED. We deliberately do NOT upsert a pick row here: leaving picks
	// untouched lets the next scan tick re-evaluate as blockers resolve.
	if s.hasUnresolvedBlockers(ctx, iss) {
		return false
	}

	if s.cfg.Judge.Enabled {
		v, err := s.judge.Judge(ctx, iss)
		if err != nil {
			s.logger.Warn("judge_failed", "issue", iss.Identifier, "err", err)
			return false
		}
		if v.Decision != "yes" {
			s.logger.Info("judge_rejected", "issue", iss.Identifier, "reason", v.Reason)
			return false
		}
		s.logger.Info("judge_accepted", "issue", iss.Identifier, "reason", v.Reason)
	}

	if s.alreadyTracked(iss) {
		return false
	}

	if err := s.linear.AssignIssue(ctx, iss.ID, s.cfg.AdmiralUserID); err != nil {
		s.logger.Error("assign_failed", "issue", iss.Identifier, "err", err)
		return false
	}
	s.logger.Info("assigned", "issue", iss.Identifier, "title", iss.Title, "url", iss.URL)

	if err := s.store.UpsertDiscovererPick(store.DiscovererPick{
		IssueID:         iss.ID,
		IssueIdentifier: iss.Identifier,
		PickedState:     iss.StateName,
	}); err != nil {
		s.logger.Warn("pick_record_failed", "issue", iss.Identifier, "err", err)
	}
	return true
}

func (s *Service) alreadyTracked(iss linear.Issue) bool {
	existing, err := s.store.GetAdmiralTaskByIssue(iss.ID)
	if err != nil {
		s.logger.Warn("dedup_check_failed", "issue", iss.Identifier, "err", err)
		return true
	}
	if existing != nil {
		s.logger.Debug("skip_already_tracked", "issue", iss.Identifier, "state", existing.State)
		return true
	}
	return false
}

func (s *Service) lookupPick(iss linear.Issue) *store.DiscovererPick {
	p, err := s.store.GetDiscovererPick(iss.ID)
	if err != nil {
		s.logger.Warn("pick_lookup_failed", "issue", iss.Identifier, "err", err)
		return nil
	}
	return p
}

// hasUnresolvedBlockers returns true iff the issue has at least one
// unresolved blocked_by relation. On API error we log and return false
// (fail-open) so a Linear hiccup does not stall the scanner; the
// orchestrator's dispatchFreshAssign performs the same check as a backstop.
func (s *Service) hasUnresolvedBlockers(ctx context.Context, iss linear.Issue) bool {
	bctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	blockers, err := s.linear.GetIssueBlockers(bctx, iss.ID)
	if err != nil {
		s.logger.Warn("blocker_check_failed_proceeding",
			"issue", iss.Identifier, "err", err)
		return false
	}
	if len(blockers) == 0 {
		return false
	}
	ids := make([]string, len(blockers))
	for i, b := range blockers {
		ids[i] = b.IssueIdentifier
	}
	s.logger.Info("skip_blocked",
		"issue", iss.Identifier,
		"blockers", ids,
	)
	return true
}
