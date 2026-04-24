package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/georgehuang/omx-bridge/internal/config"
	"github.com/georgehuang/omx-bridge/internal/omx"
	"github.com/georgehuang/omx-bridge/internal/store"
	"github.com/georgehuang/omx-bridge/internal/tg"
)

const (
	FromWorker   = "tg-bridge"
	LeaderWorker = "leader-fixed"
)

type Bridge struct {
	cfg    *config.Config
	logger *slog.Logger
	bot    *tg.Bot
	omx    *omx.Client
	db     *store.Store
	wl     *Whitelist

	eventMu      sync.Mutex
	eventRunning bool
	eventCancel  context.CancelFunc
	lastIdlePush time.Time
}

func New(cfg *config.Config, bot *tg.Bot, omxClient *omx.Client, db *store.Store, logger *slog.Logger) *Bridge {
	return &Bridge{
		cfg:    cfg,
		logger: logger,
		bot:    bot,
		omx:    omxClient,
		db:     db,
		wl:     NewWhitelist(cfg.AllowedTGUserIDs, cfg.Session.TGChatID),
	}
}

func (b *Bridge) Run(ctx context.Context) error {
	// If a cursor already exists, the team was previously up — resume event loop.
	cur, err := b.db.GetCursor(b.cfg.Session.TeamName)
	if err != nil {
		b.logger.Error("cursor read failed", "err", err)
	}
	if cur != "" {
		b.startEventLoop(ctx)
	}

	updates := b.bot.Updates()
	wrongChatReported := map[int64]bool{}

	for {
		select {
		case <-ctx.Done():
			b.stopEventLoop()
			return ctx.Err()
		case upd, ok := <-updates:
			if !ok {
				return fmt.Errorf("tg updates channel closed")
			}
			if upd.Message == nil {
				continue
			}
			msg := upd.Message
			gate := b.wl.Check(msg.From.ID, msg.Chat.ID)
			switch gate {
			case GateRejectUser:
				b.logger.Warn("whitelist_reject", "user_id", msg.From.ID, "chat_id", msg.Chat.ID)
				continue
			case GateRejectChat:
				if !wrongChatReported[msg.Chat.ID] {
					_, _ = b.bot.Send(msg.Chat.ID, "This bridge is bound to a single chat. Not this one.")
					wrongChatReported[msg.Chat.ID] = true
				}
				continue
			}
			b.handleMessage(ctx, msg)
		}
	}
}

func (b *Bridge) handleMessage(ctx context.Context, msg *tgbotapi.Message) {
	text := msg.Text
	b.logger.Info("tg_in", "user_id", msg.From.ID, "len", len(text))
	_ = b.db.RecordMessage("in", int64(msg.MessageID), msg.From.ID, "", text)

	if strings.HasPrefix(text, "/") {
		parts := strings.SplitN(text, " ", 2)
		cmd := strings.ToLower(strings.TrimSpace(parts[0]))
		args := ""
		if len(parts) > 1 {
			args = parts[1]
		}
		b.handleCommand(ctx, msg, cmd, args)
		return
	}
	b.handlePlainText(ctx, msg, text)
}

func (b *Bridge) handleCommand(ctx context.Context, msg *tgbotapi.Message, cmd, args string) {
	switch cmd {
	case "/start":
		b.cmdStart(ctx, msg)
	case "/status":
		b.cmdStatus(ctx, msg)
	case "/stop":
		b.cmdStop(ctx, msg)
	case "/help":
		_, _ = b.bot.SendToSession(helpText())
	case "/whoami":
		_, _ = b.bot.SendToSession(fmt.Sprintf(
			"tg_user_id=%d chat_id=%d team=%s cwd=%s",
			msg.From.ID, msg.Chat.ID, b.cfg.Session.TeamName, b.cfg.Session.CWD,
		))
	case "/cancel":
		_, _ = b.bot.SendToSession("No long-running bridge operation to cancel.")
	default:
		_, _ = b.bot.SendToSession(fmt.Sprintf("Unknown command: %s. Try /help.", cmd))
	}
	_ = b.db.RecordCommand(msg.From.ID, cmd, args, "done", "")
}

func helpText() string {
	return strings.Join([]string{
		"omx-bridge commands:",
		"/start  — launch the omx team",
		"/status — show team summary",
		"/stop   — shut down the team",
		"/whoami — print identity",
		"/help   — this message",
		"plain text → forwarded to team leader",
	}, "\n")
}

func (b *Bridge) cmdStart(ctx context.Context, msg *tgbotapi.Message) {
	// Pre-check running state via get-summary.
	env, err := b.omx.GetSummary(ctx)
	if err == nil && env.OK {
		_, _ = b.bot.SendToSession(fmt.Sprintf("Team %s is already running.", b.cfg.Session.TeamName))
		b.startEventLoop(ctx)
		return
	}
	_, _ = b.bot.SendToSession(fmt.Sprintf("Launching omx team %s...\n(will report when leader is up)", b.cfg.Session.TeamName))
	if err := b.omx.Launch(ctx); err != nil {
		b.logger.Error("launch failed", "err", err)
		_, _ = b.bot.SendToSession(fmt.Sprintf("Launch failed: %s", shortErr(err)))
		return
	}
	_, _ = b.bot.SendToSession(fmt.Sprintf("Team %s is up.", b.cfg.Session.TeamName))
	b.startEventLoop(ctx)
}

func (b *Bridge) cmdStatus(ctx context.Context, msg *tgbotapi.Message) {
	env, err := b.omx.GetSummary(ctx)
	if err != nil {
		_, _ = b.bot.SendToSession(fmt.Sprintf("Status error: %s", shortErr(err)))
		return
	}
	if !env.OK {
		_, _ = b.bot.SendToSession(fmt.Sprintf("Team %s is not running. Use /start.", b.cfg.Session.TeamName))
		return
	}
	reply := formatSummary(b.cfg.Session.TeamName, env.Data)

	idle, err := b.omx.ReadIdleState(ctx)
	if err == nil && idle.OK {
		var idleData struct {
			AllWorkersIdle bool `json:"all_workers_idle"`
		}
		_ = json.Unmarshal(idle.Data, &idleData)
		reply += fmt.Sprintf("\n  all_idle: %s", yesNo(idleData.AllWorkersIdle))
	}
	_, _ = b.bot.SendToSession(reply)
}

func (b *Bridge) cmdStop(ctx context.Context, msg *tgbotapi.Message) {
	env, err := b.omx.WriteShutdownRequest(ctx, LeaderWorker, FromWorker)
	if err != nil || !env.OK {
		if env != nil && env.Error != nil && env.Error.Code == "team_not_found" {
			_, _ = b.bot.SendToSession(fmt.Sprintf("Team %s is not running.", b.cfg.Session.TeamName))
			return
		}
		_, _ = b.bot.SendToSession(fmt.Sprintf("Stop error: %s", shortErr(err)))
		return
	}
	_, _ = b.bot.SendToSession(fmt.Sprintf("Requested shutdown of team %s.", b.cfg.Session.TeamName))

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		ack, err := b.omx.ReadShutdownAck(ctx, LeaderWorker)
		if err == nil && ack.OK {
			var ackData struct {
				Ack map[string]any `json:"ack"`
			}
			_ = json.Unmarshal(ack.Data, &ackData)
			if len(ackData.Ack) > 0 {
				_, _ = b.bot.SendToSession(fmt.Sprintf("Team %s shut down.", b.cfg.Session.TeamName))
				b.stopEventLoop()
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(1 * time.Second):
		}
	}
	_, _ = b.bot.SendToSession("Shutdown request sent; ack not seen in 30s — check the Mac.")
}

func (b *Bridge) handlePlainText(ctx context.Context, msg *tgbotapi.Message, text string) {
	env, err := b.omx.SendMessage(ctx, omx.SendMessageInput{
		FromWorker: FromWorker,
		ToWorker:   LeaderWorker,
		Body:       text,
	})
	if err != nil {
		_, _ = b.bot.SendToSession(fmt.Sprintf("Could not deliver: %s", shortErr(err)))
		return
	}
	if !env.OK {
		if env.Error != nil && env.Error.Code == "team_not_found" {
			_, _ = b.bot.SendToSession(fmt.Sprintf("Team %s is not running. Use /start.", b.cfg.Session.TeamName))
			return
		}
		_, _ = b.bot.SendToSession(fmt.Sprintf("Could not deliver: %s", env.Error.ErrorString()))
		return
	}
	var data struct {
		Message struct {
			MessageID string `json:"message_id"`
		} `json:"message"`
	}
	_ = json.Unmarshal(env.Data, &data)
	_ = b.db.RecordMessage("out", int64(msg.MessageID), msg.From.ID, data.Message.MessageID, text)
	b.startEventLoop(ctx)
}

func formatSummary(teamName string, data json.RawMessage) string {
	var d struct {
		WorkerCount int `json:"workerCount"`
		Workers     []struct {
			Name  string `json:"name"`
			State string `json:"state"`
		} `json:"workers"`
		Tasks map[string]int `json:"tasks"`
	}
	_ = json.Unmarshal(data, &d)
	idle := 0
	for _, w := range d.Workers {
		if w.State == "idle" {
			idle++
		}
	}
	if d.WorkerCount == 0 {
		d.WorkerCount = len(d.Workers)
	}
	get := func(k string) int { return d.Tasks[k] }
	return fmt.Sprintf(
		"Team %s\n  workers: %d (idle: %d)\n  tasks:   pending=%d  in_progress=%d  blocked=%d  completed=%d\n  leader:  alive",
		teamName, d.WorkerCount, idle,
		get("pending"), get("in_progress"), get("blocked"), get("completed"),
	)
}

func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

func shortErr(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
