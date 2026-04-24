package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
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

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: parseLevel(cfg.Logging.Level)}))

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
