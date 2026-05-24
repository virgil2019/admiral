// Package discoverer scans Linear for assignable issues, optionally
// asks a `claude -p` judge whether the issue is autonomously doable,
// then self-assigns matches to admiral's own Linear user. The Linear
// webhook (handled by admiral-autopilot) does the rest.
//
// The discoverer is a standalone service (cmd/admiral-discoverer) so it
// can be split off into a separate repo later. It depends on:
//   - linear: read-only search + write assign
//   - store: read-only AdmiralTask lookup (dedup against in-flight runs)
//
// It deliberately does NOT touch autopilot internals or write any
// admiral-owned tables — the only side effect is a Linear API call,
// which loops back through the normal webhook → autopilot path.
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
type Config struct {
	PollInterval    time.Duration
	TeamKeys        []string
	ProjectIDs      []string
	StateTypes      []string
	RequireLabel    string
	MaxPickPerRound int
	AdmiralUserID   string
	Judge           JudgeConfig
}

// JudgeConfig configures the claude -p judge step.
type JudgeConfig struct {
	Enabled   bool
	ClaudeBin string
	Timeout   time.Duration
}

// linearClient is the slice of the Linear client the discoverer uses.
// Kept narrow so tests can mock it and future HTTP extraction is cheap.
type linearClient interface {
	SearchAssignableIssues(ctx context.Context, f linear.SearchFilter) ([]linear.Issue, error)
	AssignIssue(ctx context.Context, issueID, userID string) error
}

// taskRegistry is the read-only slice of the store the discoverer uses
// for dedup. When the service is extracted from this repo this becomes
// an HTTP call into admiral-autopilot.
type taskRegistry interface {
	GetAdmiralTaskByIssue(issueID string) (*store.AdmiralTask, error)
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
	store  taskRegistry
	judge  judger
	logger *slog.Logger
}

// New constructs a discoverer Service. If cfg.Judge.Enabled is true, a
// default claudeJudge is wired; pass nil for judge to use the default.
func New(cfg Config, lc linearClient, tr taskRegistry, j judger, logger *slog.Logger) *Service {
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
		cfg:    cfg,
		linear: lc,
		store:  tr,
		judge:  j,
		logger: logger,
	}
}

// Run blocks until ctx is cancelled. It performs an immediate scan on
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
		"teams", s.cfg.TeamKeys,
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
	issues, err := s.linear.SearchAssignableIssues(ctx, s.buildFilter())
	if err != nil {
		s.logger.Error("scan_failed", "err", err)
		return
	}
	s.logger.Info("scan_complete", "candidates", len(issues))
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

func (s *Service) buildFilter() linear.SearchFilter {
	return linear.SearchFilter{
		TeamKeys:       s.cfg.TeamKeys,
		ProjectIDs:     s.cfg.ProjectIDs,
		StateTypes:     s.cfg.StateTypes,
		RequireLabel:   s.cfg.RequireLabel,
		UnassignedOnly: true,
	}
}

// consider returns true iff the issue was assigned to admiral in this
// round. Skipping (dedup, judge=no, error) returns false.
func (s *Service) consider(ctx context.Context, iss linear.Issue) bool {
	if existing, err := s.store.GetAdmiralTaskByIssue(iss.ID); err != nil {
		s.logger.Warn("dedup_check_failed", "issue", iss.Identifier, "err", err)
		return false
	} else if existing != nil {
		s.logger.Debug("skip_already_tracked", "issue", iss.Identifier, "state", existing.State)
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

	if err := s.linear.AssignIssue(ctx, iss.ID, s.cfg.AdmiralUserID); err != nil {
		s.logger.Error("assign_failed", "issue", iss.Identifier, "err", err)
		return false
	}
	s.logger.Info("assigned", "issue", iss.Identifier, "title", iss.Title, "url", iss.URL)
	return true
}
