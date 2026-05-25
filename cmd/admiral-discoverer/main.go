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
	"syscall"

	"github.com/georgehuang/admiral/internal/config"
	"github.com/georgehuang/admiral/internal/discoverer"
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

	lc := linear.NewClient(cfg.Linear.APIBase, cfg.Linear.APIToken)
	tr, refreshAvailable := linear.NewTokenRefresher(
		cfg.Linear.ClientID, cfg.Linear.ClientSecret, db, logger, "")
	if refreshAvailable {
		lc.SetTokenRefresher(tr)
		logger.Info("linear_token_refresh_enabled")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	userID := cfg.Discoverer.AdmiralUserID
	if userID == "" {
		v, err := lc.GetViewer(ctx)
		if err != nil {
			logger.Error("viewer_lookup_failed",
				"err", err,
				"hint", "set discoverer.admiral_user_id explicitly to skip viewer lookup")
			os.Exit(1)
		}
		userID = v.ID
		logger.Info("admiral_user_resolved", "user_id", v.ID, "name", v.Name)
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
	}, lc, db, nil, logger)

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
