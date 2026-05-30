// admiral-autopilot is the v0.3 Linear-driven autopilot daemon. It runs an
// HTTP webhook receiver, picks up Linear issue assignments, and drives a
// `claude -p` run inside a per-issue worktree to completion.
package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/georgehuang/admiral/internal/autopilot"
	"github.com/georgehuang/admiral/internal/config"
	ghpkg "github.com/georgehuang/admiral/internal/github"
	"github.com/georgehuang/admiral/internal/linear"
	"github.com/georgehuang/admiral/internal/store"
	"github.com/georgehuang/admiral/internal/tg"
)

func main() {
	var cfgPath string
	flag.StringVar(&cfgPath, "config", config.DefaultConfigPath(), "path to config.yaml")
	flag.Parse()

	cfg, err := config.LoadAutopilot(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}
	logger := newLogger(cfg.Logging)

	db, err := store.Open(cfg.Storage.SQLitePath)
	if err != nil {
		logger.Error("sqlite open failed", "err", err)
		os.Exit(1)
	}
	defer db.Close()

	// Seed token store from config if empty (DB takes priority over config
	// once a token has been stored, so we only seed on first run).
	if err := seedTokenStore(db, &cfg.Linear, logger); err != nil {
		logger.Warn("token_store_seed_failed", "err", err)
	}

	// Seed repos from config if DB is empty (DB takes priority over config
	// once repos have been stored, so we only seed on first run).
	if err := seedRepos(db, &cfg.Autopilot, logger); err != nil {
		logger.Warn("repos_seed_failed", "err", err)
	}

	// Build Linear client and optionally wire token refresh.
	lc := linear.NewClient(cfg.Linear.APIBase, cfg.Linear.APIToken)
	tr, refreshAvailable := linear.NewTokenRefresher(
		cfg.Linear.ClientID, cfg.Linear.ClientSecret, db, logger, "")
	if refreshAvailable {
		lc.SetTokenRefresher(tr)
		logger.Info("linear_token_refresh_enabled")
	}
	orch := autopilot.New(&cfg.Autopilot, lc, db, logger)
	// Plumb the discoverer's pickup gates into the verify loop so the
	// follow-up sub-issues it files are auto-picked and re-shipped — single
	// source of truth with the discoverer, no drift. LoadPickupRules applies
	// the same defaults the discoverer uses (the autopilot config load path
	// does not validate/default the discoverer block).
	if pickup, err := config.LoadPickupRules(cfgPath); err != nil {
		logger.Warn("verify_pickup_rules_load_failed", "err", err)
	} else {
		orch.SetVerifyPickupRules(pickup.RequireLabel, pickup.StateTypes)
	}

	// signal channel for the webhook to notify worker of new events
	sig := make(chan struct{}, 1)

	// webhook uses the enqueue path (store + signal); onAgent is nil
	wh := linear.NewWebhook(cfg.Linear.WebhookSecret, db, sig, logger, nil)

	ghwh := ghpkg.NewWebhook(cfg.Autopilot.GhWebhookSecret, cfg.Autopilot.GhBotLogin, db, logger)

	mux := http.NewServeMux()
	// /webhook matches Linear's typical agent webhook URL convention
	// (the path used in the existing oauth-callback.ts demo). /linear/webhook
	// is kept as an alias.
	mux.Handle("/webhook", wh.Handler())
	mux.Handle("/linear/webhook", wh.Handler())
	mux.Handle("/github/webhook", ghwh.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:              cfg.Autopilot.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	n := cfg.Autopilot.WorkerCount
	logger.Info("admiral-autopilot starting",
		"listen", cfg.Autopilot.ListenAddr,
		"repos", len(cfg.Autopilot.Repos),
		"claude_bin", cfg.Autopilot.ClaudeBin,
		"sqlite", cfg.Storage.SQLitePath,
		"worker_count", n,
	)

	// Optional Telegram alerter: surfaces a permanent-OAuth-failure to the
	// user once per outage so the daemon doesn't silently drift while
	// Linear-side state updates fail. Returns nil when not configured —
	// the worker logs but doesn't send.
	alerter := newAuthAlerter(cfg, db, logger)

	go orch.StartBlockerWatcher(ctx)

	// worker pool: N workers consume from events_inbox and dispatch to orchestrator
	for i := 0; i < n; i++ {
		w := autopilot.NewWorker(db, orch, logger.With("worker", i), sig)
		w.AuthAlerter = alerter
		go w.Run(ctx)
	}

	// Build admin token: use config/env, or generate a transient token.
	adminToken := cfg.Autopilot.AdminToken
	if adminToken == "" {
		// Generate a random hex token for this run.
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			logger.Error("admin token generation failed", "err", err)
			os.Exit(1)
		}
		adminToken = fmt.Sprintf("%x", b)
		logger.Info("admin_token_not_set_generated_transient",
			"token", adminToken,
			"hint", "set autopilot.admin_token in config or ADMIRAL_ADMIN_TOKEN env to persist")
	}
	// Admin API server — token auth (M5); bound to localhost by default.
	adminAddr := cfg.Autopilot.AdminListenAddr
	logger.Info("admin server starting",
		"listen", adminAddr,
	)
	go func() {
		if err := autopilot.ServeAdminHTTP(adminAddr, db, lc, cfg.Autopilot.GhBin, logger, n, adminToken); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("admin server failed", "err", err)
			cancel()
		}
	}()

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "err", err)
			cancel()
		}
	}()

	<-ctx.Done()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown error", "err", err)
	}
	logger.Info("admiral-autopilot stopped")
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

// seedTokenStore writes access_token + refresh_token from config into the
// SQLite store only when the DB is empty (DB takes priority over config once
// a token has been stored, to avoid overwriting a refreshed token with an
// older config value).
func seedTokenStore(db *store.Store, linCfg *config.Linear, logger *slog.Logger) error {
	tok, err := db.GetLinearOAuthToken()
	if err != nil {
		return fmt.Errorf("get token: %w", err)
	}
	// DB already has a token — don't overwrite.
	if tok != nil && tok.AccessToken != "" {
		return nil
	}
	// DB is empty — seed from config.
	if linCfg.APIToken == "" {
		return nil // nothing to seed
	}
	logger.Info("linear_token_store_seeding_from_config")
	return db.SaveLinearOAuthToken(linCfg.APIToken, linCfg.RefreshToken, "")
}

// newAuthAlerter returns the function the worker calls when the Linear
// OAuth circuit breaker trips. The alert goes to the first user in
// allowed_tg_user_ids; if BotToken or AllowedTGUserIDs is missing we log
// once at startup and return nil — the worker still short-circuits its
// drain loop, the user just won't get a Telegram nudge.
func newAuthAlerter(cfg *config.Config, db *store.Store, logger *slog.Logger) func(reason string) error {
	if strings.TrimSpace(cfg.BotToken) == "" || len(cfg.AllowedTGUserIDs) == 0 {
		logger.Warn("auth_alert_disabled",
			"reason", "bot_token or allowed_tg_user_ids not set in config; user will not be notified on OAuth failure")
		return nil
	}
	chatID := cfg.AllowedTGUserIDs[0]
	bot, err := tg.New(cfg.BotToken, chatID, 0)
	if err != nil {
		logger.Warn("auth_alert_disabled", "reason", "tg bot init failed", "err", err)
		return nil
	}
	logger.Info("auth_alert_enabled", "chat_id", chatID)
	return func(reason string) error {
		queued, _ := db.CountPendingEvents()
		text := fmt.Sprintf(
			"⚠️ admiral OAuth has stopped working: %s\n\nRun `admiral-oauth` to re-authorize. %d webhook event(s) queued waiting.",
			reason, queued)
		_, sendErr := bot.Send(chatID, text)
		return sendErr
	}
}

// seedRepos writes repos from config into the SQLite store only when the DB
// repos table is empty (DB takes priority over config once repos have been
// stored, to avoid overwriting user changes made via future admin APIs).
func seedRepos(db *store.Store, apCfg *config.Autopilot, logger *slog.Logger) error {
	existing, err := db.ListRepos()
	if err != nil {
		return fmt.Errorf("list repos: %w", err)
	}
	if len(existing) > 0 {
		return nil // DB already has repos — don't overwrite
	}
	if len(apCfg.Repos) == 0 {
		return nil // nothing to seed (validation already enforces non-empty)
	}

	logger.Info("repos_seeding_from_config", "count", len(apCfg.Repos))
	for _, r := range apCfg.Repos {
		repo := store.Repo{
			ProjectID:   r.ProjectID,
			ProjectName: r.ProjectName,
			RepoDir:     r.RepoDir,
			BaseBranch:  r.BaseBranch,
			Enabled:     true,
		}
		if err := db.UpsertRepo(repo); err != nil {
			return fmt.Errorf("upsert repo %s: %w", r.ProjectID, err)
		}
	}
	return nil
}
