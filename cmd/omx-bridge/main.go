package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/georgehuang/omx-bridge/internal/bridge"
	"github.com/georgehuang/omx-bridge/internal/config"
	"github.com/georgehuang/omx-bridge/internal/omx"
	"github.com/georgehuang/omx-bridge/internal/store"
	"github.com/georgehuang/omx-bridge/internal/tg"
)

func main() {
	var cfgPath string
	flag.StringVar(&cfgPath, "config", config.DefaultConfigPath(), "path to config.yaml")
	flag.Parse()

	cfg, err := config.Load(cfgPath)
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

	bot, err := tg.New(cfg.BotToken, cfg.Session.TGChatID, cfg.Telegram.LongPollTimeoutS)
	if err != nil {
		logger.Error("tg bot init failed", "err", err)
		os.Exit(1)
	}

	oc := omx.New(cfg.Session.OmxBinPath, cfg.Session.CWD, cfg.Session.TeamName)
	br := bridge.New(cfg, bot, oc, db, logger)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	logger.Info("bridge starting",
		"team", cfg.Session.TeamName,
		"cwd", cfg.Session.CWD,
		"sqlite", cfg.Storage.SQLitePath,
		"launch_mode", cfg.Launch.Mode,
	)

	if err := br.Run(ctx); err != nil && err != context.Canceled {
		logger.Error("bridge exited", "err", err)
		os.Exit(1)
	}
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

// newLogger builds a slog logger writing to stderr and, if logging.file is
// set in config, tees to that file too. File open errors are logged via
// stderr-only and swallowed (we don't want a bad log path to kill the daemon).
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
