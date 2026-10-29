// admiral-discoverer scans Linear for assignable issues that belong to
// projects opted-in via the admiral-autopilot admin UI (repos table
// auto_pick_enabled flag) and self-assigns them to admiral. The
// Linear webhook does the rest — admiral-autopilot picks up the
// AgentSessionEvent and runs the issue normally.
//
// Standalone binary by design: shares only the linear client and the
// SQLite store with admiral-autopilot, and writes only to its own
// discoverer_picks table. The token refresher in this main does write
// to linear_oauth / auth_error (admiral-owned tables) — TODO when this
// binary moves to its own repo: replace with a read-only TokenStore
// shim and route refresh through autopilot's admin API.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/georgehuang/admiral/internal/config"
	"github.com/georgehuang/admiral/internal/discoverer"
	ghpkg "github.com/georgehuang/admiral/internal/github"
	"github.com/georgehuang/admiral/internal/linear"
	"github.com/georgehuang/admiral/internal/store"
)

func main() {
	var cfgPath string
	flag.StringVar(&cfgPath, "config", config.DefaultConfigPath(), "path to config.yaml")
	flag.Parse()

	cfg, err := config.LoadDiscoverer(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}
	logger := newLogger(cfg.Logging)
	for _, msg := range cfg.Warnings {
		logger.Warn("config_warning", "msg", msg)
	}

	db, err := store.Open(cfg.Storage.SQLitePath)
	if err != nil {
		logger.Error("sqlite open failed", "err", err)
		os.Exit(1)
	}
	defer db.Close()

	// The discoverer advances a task's Linear state by polling its PR, and
	// every one of those calls runs as the account the repo registered. A
	// repo with no gh_user is skipped at runtime, so without this boot-time
	// warning an upgraded deployment (migration0024 backfills '') would look
	// like the state machine silently froze.
	warnReposMissingGhUser(db, logger)

	lc := linear.NewClient(cfg.Linear.APIBase, cfg.Linear.APIToken)
	tr, refreshAvailable := linear.NewTokenRefresher(
		cfg.Linear.ClientID, cfg.Linear.ClientSecret, db, logger, "")
	if refreshAvailable {
		lc.SetTokenRefresher(tr)
		logger.Info("linear_token_refresh_enabled")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	userID, err := discoverer.ResolveAdmiralUserID(ctx, cfg.Discoverer.AdmiralUserID, lc, db, logger)
	if err != nil {
		logger.Error("viewer_lookup_failed",
			"err", err,
			"hint", "set discoverer.admiral_user_id explicitly to skip viewer lookup, or delete the cached row if it is stale (KV key: "+discoverer.AdmiralUserIDKVKey+")")
		os.Exit(1)
	}

	judgeEnabled := cfg.Discoverer.Judge.Enabled != nil && *cfg.Discoverer.Judge.Enabled
	svc := discoverer.New(discoverer.Config{
		PollInterval:    cfg.Discoverer.PollInterval,
		StateTypes:      cfg.Discoverer.StateTypes,
		RequireLabel:    cfg.Discoverer.RequireLabel,
		MaxPickPerRound: cfg.Discoverer.MaxPickPerRound,
		AdmiralUserID:   userID,
		Judge: discoverer.JudgeConfig{
			Enabled:   judgeEnabled,
			ClaudeBin: cfg.Discoverer.Judge.ClaudeBin,
			Timeout:   cfg.Discoverer.Judge.Timeout,
		},
		LinearStates: discoverer.LinearStateMap{
			InReview: cfg.Discoverer.LinearStates.InReview,
			Reviewed: cfg.Discoverer.LinearStates.Reviewed,
		},
		AutoMerge: cfg.Discoverer.AutoMerge,
	}, lc, &prAdapter{accounts: ghpkg.NewAccounts(cfg.Autopilot.GhBin), ghBin: cfg.Autopilot.GhBin}, db, nil, logger)
	// Boot-time recovery for verify rows left behind by historical
	// failures (e.g. the GEO-267 argv-overflow incident — PR #187
	// fixed the bug, this re-enqueues the rows that were stranded).
	svc.SetBootRescanDB(db)

	logger.Info("admiral-discoverer starting",
		"sqlite", cfg.Storage.SQLitePath,
		"poll_interval", cfg.Discoverer.PollInterval,
	)

	if err := svc.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("discoverer exited with error", "err", err)
		os.Exit(1)
	}
	logger.Info("admiral-discoverer stopped")
}

func parseLevel(lvl string) slog.Level {
	switch lvl {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// prAdapter bridges internal/github.Client to the discoverer's narrow
// prClient interface, keeping the discoverer package free of an
// internal/github import. When the discoverer is split off into its
// own repo, the adapter is the only piece that has to move with it.
//
// Every call builds a client for the gh account the repo registered
// (repos.gh_user): the token is read from the gh keyring per call and never
// cached, so admiral acts as that account — never as whichever one happens
// to be active on the host.
type prAdapter struct {
	accounts *ghpkg.Accounts
	// ghBin is the gh binary the client shells out to (autopilot.gh_bin).
	ghBin string
}

func (a *prAdapter) clientFor(ctx context.Context, ghUser string) (*ghpkg.Client, error) {
	tok, err := a.accounts.Token(ctx, ghUser)
	if err != nil {
		return nil, fmt.Errorf("resolve gh account %q: %w", ghUser, err)
	}
	return ghpkg.NewClientWithBin(a.ghBin, tok), nil
}

func (a *prAdapter) GetPRStatus(ctx context.Context, prURL, ghUser string) (discoverer.PRStatus, error) {
	c, err := a.clientFor(ctx, ghUser)
	if err != nil {
		return discoverer.PRStatus{}, err
	}
	s, err := c.GetPRStatus(ctx, prURL)
	if err != nil {
		return discoverer.PRStatus{}, err
	}
	return discoverer.PRStatus{
		State:             s.State,
		MergedAt:          s.MergedAt,
		HasApprovedReview: s.HasApprovedReview,
		Mergeable:         s.Mergeable,
		ChecksState:       s.ChecksState,
	}, nil
}

func (a *prAdapter) MergePR(ctx context.Context, prURL, ghUser string) error {
	c, err := a.clientFor(ctx, ghUser)
	if err != nil {
		return err
	}
	return c.MergePR(ctx, prURL)
}

func newLogger(c config.Logging) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLevel(c.Level)}
	var w io.Writer = os.Stderr
	if c.File != "" {
		if err := os.MkdirAll(filepath.Dir(c.File), 0o755); err == nil {
			if f, err := os.OpenFile(c.File, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
				w = io.MultiWriter(os.Stderr, f)
			} else {
				fmt.Fprintf(os.Stderr, "log file open failed (stderr-only): %v\n", err)
			}
		}
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

// warnReposMissingGhUser emits a boot-time WARN per repo with no gh_user —
// typically rows that predate migration0024. A repo without an account cannot
// have its PR polled, so its tasks never advance (no MERGED → Completed, no
// CLOSED → Canceled, no verify enqueue, no auto-merge).
func warnReposMissingGhUser(db *store.Store, logger *slog.Logger) {
	repos, err := db.ListRepos()
	if err != nil {
		logger.Warn("gh_user_boot_check_list_failed", "err", err)
		return
	}
	for _, r := range repos {
		if strings.TrimSpace(r.GhUser) == "" {
			logger.Warn("gh_user_unconfigured",
				"project_id", r.ProjectID,
				"project_name", r.ProjectName,
				"hint", `no gh account configured for this repo: its PR state advancement is skipped. Set it with PATCH /admin/repos/<project_id> {"gh_user":"<account from gh auth status>"}; config gh_user only applies when the repos table is empty.`)
		}
	}
}
