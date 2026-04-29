// admiral-autopilot is the v0.3 Linear-driven autopilot daemon. It runs an
// HTTP webhook receiver, picks up Linear issue assignments, and drives a
// `claude -p` run inside a per-issue worktree to completion.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/georgehuang/admiral/internal/autopilot"
	"github.com/georgehuang/admiral/internal/config"
	"github.com/georgehuang/admiral/internal/linear"
	"github.com/georgehuang/admiral/internal/store"
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

	// Build Linear client and optionally wire token refresh.
	lc := linear.NewClient(cfg.Linear.APIBase, cfg.Linear.APIToken)
	tr, refreshAvailable := linear.NewTokenRefresher(
		cfg.Linear.ClientID, cfg.Linear.ClientSecret, db, logger, "")
	if refreshAvailable {
		lc.SetTokenRefresher(tr)
		logger.Info("linear_token_refresh_enabled")
	}
	orch := autopilot.New(&cfg.Autopilot, lc, db, logger)
	wh := linear.NewWebhook(cfg.Linear.WebhookSecret, orch.HandleAgentEvent, logger)

	mux := http.NewServeMux()
	// /webhook matches Linear's typical agent webhook URL convention
	// (the path used in the existing oauth-callback.ts demo). /linear/webhook
	// is kept as an alias.
	mux.Handle("/webhook", wh.Handler())
	mux.Handle("/linear/webhook", wh.Handler())
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

	logger.Info("admiral-autopilot starting",
		"listen", cfg.Autopilot.ListenAddr,
		"repo", cfg.Autopilot.RepoDir,
		"base_branch", cfg.Autopilot.BaseBranch,
		"claude_bin", cfg.Autopilot.ClaudeBin,
		"sqlite", cfg.Storage.SQLitePath,
	)

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
