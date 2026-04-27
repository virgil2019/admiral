package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/georgehuang/admiral/internal/config"
	"github.com/georgehuang/admiral/internal/store"
	"github.com/georgehuang/admiral/internal/teamcli"
	"github.com/georgehuang/admiral/internal/tg"
)

const (
	FromWorker = "tg-bridge"

	kvLastPoll         = "last_successful_poll_at"
	kvLastWakeWarnedAt = "last_wake_warned_at"
)

type Bridge struct {
	cfg          *config.Config
	logger       *slog.Logger
	bot          *tg.Bot
	omx          teamcli.Provider
	caps         teamcli.Capabilities
	leaderWorker string
	db           *store.Store
	wl           *Whitelist

	eventMu      sync.Mutex
	eventRunning bool
	eventCancel  context.CancelFunc
	lastIdlePush time.Time

	wrongChatReported map[int64]bool
}

func New(cfg *config.Config, bot *tg.Bot, provider teamcli.Provider, db *store.Store, logger *slog.Logger) *Bridge {
	return &Bridge{
		cfg:               cfg,
		logger:            logger,
		bot:               bot,
		omx:               provider,
		caps:              provider.Caps(),
		leaderWorker:      cfg.Session.LeaderWorker,
		db:                db,
		wl:                NewWhitelist(cfg.AllowedTGUserIDs, cfg.Session.TGChatID),
		wrongChatReported: map[int64]bool{},
	}
}

func (b *Bridge) Run(ctx context.Context) error {
	// G6: wall-clock wake check before we start polling.
	b.maybeWarnAfterWake(ctx)

	// G1: process any unprocessed TG updates persisted before the previous crash.
	b.drainPending(ctx)

	// If a cursor already exists AND the team is still up, resume event loop.
	// If cursor exists but team is gone, do NOT hammer a dead team.
	if cur, err := b.db.GetCursor(b.cfg.Session.TeamName); err == nil && cur != "" {
		if env, err := b.omx.GetSummary(ctx); err == nil && env.OK {
			b.startEventLoop(ctx)
		} else {
			b.logger.Info("cursor present but team not running — waiting for /start",
				"team", b.cfg.Session.TeamName)
		}
	}

	// N4: heartbeat writer so kvLastPoll reflects long-poll health regardless
	// of incoming message volume. Without this, an idle whitelisted user +
	// restart after 24h would falsely trigger the wake-warn. Writes every 5m.
	heartbeatStop := make(chan struct{})
	defer close(heartbeatStop)
	go b.heartbeat(heartbeatStop)

	// Initial heartbeat so a fresh daemon doesn't appear "stale" to itself.
	_ = b.db.KVSet(kvLastPoll, time.Now().UTC().Format(time.RFC3339))

	updates := b.bot.Updates()

	for {
		select {
		case <-ctx.Done():
			b.stopEventLoop()
			return ctx.Err()
		case upd, ok := <-updates:
			if !ok {
				return fmt.Errorf("tg updates channel closed")
			}
			_ = b.db.KVSet(kvLastPoll, time.Now().UTC().Format(time.RFC3339))
			if upd.Message == nil {
				continue
			}
			b.ingestUpdate(ctx, upd)
		}
	}
}

func (b *Bridge) heartbeat(stop <-chan struct{}) {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			_ = b.db.KVSet(kvLastPoll, time.Now().UTC().Format(time.RFC3339))
		}
	}
}

// ingestUpdate implements G1: persist the update before processing so a
// crash mid-send-message doesn't lose the inbound.
func (b *Bridge) ingestUpdate(ctx context.Context, upd tgbotapi.Update) {
	msg := upd.Message
	gate := b.wl.Check(msg.From.ID, msg.Chat.ID)
	switch gate {
	case GateRejectUser:
		b.logger.Warn("whitelist_reject", "user_id", msg.From.ID, "chat_id", msg.Chat.ID)
		return
	case GateRejectChat:
		if !b.wrongChatReported[msg.Chat.ID] {
			_, _ = b.bot.Send(msg.Chat.ID, "This bridge is bound to a single chat. Not this one.")
			b.wrongChatReported[msg.Chat.ID] = true
		}
		return
	}

	fresh, err := b.db.InsertTGUpdate(int64(upd.UpdateID), msg.From.ID, msg.Chat.ID, msg.Text)
	if err != nil {
		b.logger.Error("tg_update_persist_failed", "err", err)
		return
	}
	if !fresh {
		b.logger.Info("tg_update_dedupe", "update_id", upd.UpdateID)
		return
	}
	// Inbound is persisted via tg_updates; commands land in `commands`;
	// plain-text forwards land in `messages` at send-time (tx with
	// mark-processed). We don't double-write here.
	b.processUpdate(ctx, int64(upd.UpdateID), msg.From.ID, msg.Chat.ID, msg.Text, int64(msg.MessageID))
}

// drainPending processes rows written by an earlier daemon run that died
// before calling send-message.
func (b *Bridge) drainPending(ctx context.Context) {
	rows, err := b.db.UnprocessedTGUpdates()
	if err != nil {
		b.logger.Error("drain_pending_query_failed", "err", err)
		return
	}
	if len(rows) == 0 {
		return
	}
	b.logger.Info("draining_pending_tg_updates", "count", len(rows))
	for _, r := range rows {
		b.processUpdate(ctx, r.UpdateID, r.UserID, r.ChatID, r.Body, 0)
	}
}

// processUpdate dispatches an already-persisted update. On success (or
// terminal failure the user was told about), marks the row processed.
func (b *Bridge) processUpdate(ctx context.Context, updateID, userID, chatID int64, text string, tgMessageID int64) {
	text = strings.TrimRight(text, "\r\n")
	b.logger.Info("tg_in", "user_id", userID, "update_id", updateID, "len", len(text))

	if strings.HasPrefix(text, "/") {
		parts := strings.SplitN(text, " ", 2)
		cmd := strings.ToLower(strings.TrimSpace(parts[0]))
		args := ""
		if len(parts) > 1 {
			args = parts[1]
		}
		b.handleCommand(ctx, userID, chatID, cmd, args)
		_ = b.db.RecordCommand(userID, cmd, args, "done", "")
		_ = b.db.MarkTGUpdateDone(updateID)
		return
	}
	b.handlePlainText(ctx, updateID, userID, tgMessageID, text)
}

func (b *Bridge) handleCommand(ctx context.Context, userID, chatID int64, cmd, args string) {
	switch cmd {
	case "/start":
		b.cmdStart(ctx)
	case "/status":
		b.cmdStatus(ctx)
	case "/stop":
		b.cmdStop(ctx)
	case "/help":
		_, _ = b.bot.SendToSession(helpText())
	case "/whoami":
		_, _ = b.bot.SendToSession(fmt.Sprintf(
			"tg_user_id=%d chat_id=%d team=%s cwd=%s",
			userID, chatID, b.cfg.Session.TeamName, b.cfg.Session.CWD,
		))
	case "/cancel":
		_, _ = b.bot.SendToSession("No long-running bridge operation to cancel.")
	default:
		_, _ = b.bot.SendToSession(fmt.Sprintf("Unknown command: %s. Try /help.", cmd))
	}
	_ = args
}

func helpText() string {
	return strings.Join([]string{
		"admiral commands:",
		"/start  — launch the omx team",
		"/status — show team summary",
		"/stop   — shut down the team",
		"/whoami — print identity",
		"/help   — this message",
		"plain text → forwarded to team leader",
	}, "\n")
}

func (b *Bridge) cmdStart(ctx context.Context) {
	env, err := b.omx.GetSummary(ctx)
	if err == nil && env.OK {
		_, _ = b.bot.SendToSession(fmt.Sprintf("Team %s is already running.", b.cfg.Session.TeamName))
		b.startEventLoop(ctx)
		return
	}
	_, _ = b.bot.SendToSession(fmt.Sprintf("Launching omx team %s...\n(will report when leader is up)", b.cfg.Session.TeamName))

	mode := teamcli.LaunchMode(b.cfg.Launch.Mode)
	if mode == teamcli.LaunchPty {
		b.logger.Warn("launch.mode=pty using_script_wrapper",
			"pty_command", b.cfg.Launch.PtyCommand,
			"team", b.cfg.Session.TeamName)
	}
	spec := teamcli.LaunchSpec{
		WorkerCount: b.cfg.Launch.WorkerCount,
		AgentType:   b.cfg.Launch.AgentType,
		Task:        b.cfg.Launch.BootstrapTask,
	}
	err = b.omx.Launch(ctx, mode, b.cfg.Launch.PtyCommand, spec)
	if err != nil {
		if errors.Is(err, teamcli.ErrLaunchNeedsTTY) {
			_, _ = b.bot.SendToSession(fmt.Sprintf(
				"Launch failed: omx requires a TTY. Set launch.mode: pty in config, "+
					"or run `omx team %d:%s %q` from a terminal on the Mac yourself, then use /status here.",
				spec.WorkerCount, spec.AgentType, spec.Task))
			b.logger.Error("launch_needs_tty_direct_mode")
			return
		}
		b.logger.Error("launch failed", "err", err)
		_, _ = b.bot.SendToSession(fmt.Sprintf("Launch failed: %s", shortErr(err)))
		return
	}

	// Poll get-summary until team is up (bounded 60s). omx team-launch
	// returns once tmux panes are spawned; state may lag a second or two.
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		env, err := b.omx.GetSummary(ctx)
		if err == nil && env.OK {
			_, _ = b.bot.SendToSession(fmt.Sprintf("Team %s is up.", b.cfg.Session.TeamName))
			b.startEventLoop(ctx)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
	_, _ = b.bot.SendToSession(fmt.Sprintf(
		"Team %s launch returned OK but get-summary did not confirm within 60s — check `omx team status %s` on the Mac.",
		b.cfg.Session.TeamName, b.cfg.Session.TeamName))
	b.startEventLoop(ctx)
}

func (b *Bridge) cmdStatus(ctx context.Context) {
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

	// Swap to read-stall-state per product ruling G4(2). Capability-gated:
	// omc does not expose this endpoint and the line is omitted.
	if !b.caps.SupportsStallState {
		_, _ = b.bot.SendToSession(reply)
		return
	}
	stall, err := b.omx.ReadStallState(ctx)
	if err == nil && stall.OK {
		var stallData struct {
			TeamStalled bool     `json:"team_stalled"`
			Reasons     []string `json:"reasons"`
		}
		_ = json.Unmarshal(stall.Data, &stallData)
		line := fmt.Sprintf("\n  stalled: %s", yesNo(stallData.TeamStalled))
		if stallData.TeamStalled && len(stallData.Reasons) > 0 {
			joined := strings.Join(stallData.Reasons, ",")
			if len(joined) > 120 {
				joined = joined[:117] + "..."
			}
			line += " (" + joined + ")"
		}
		reply += line
	}
	_, _ = b.bot.SendToSession(reply)
}

func (b *Bridge) cmdStop(ctx context.Context) {
	env, err := b.omx.WriteShutdownRequest(ctx, b.leaderWorker, FromWorker)
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
		ack, err := b.omx.ReadShutdownAck(ctx, b.leaderWorker)
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
	// Per G7: keep event loop running so any late shutdown_ack event still pushes.
	_, _ = b.bot.SendToSession("Shutdown request sent; ack not seen in 30s — check the Mac.")
}

func (b *Bridge) handlePlainText(ctx context.Context, updateID, userID, tgMessageID int64, text string) {
	body := text
	if b.cfg.Session.Provider == "omc" {
		body = b.wrapForOmcReply(text)
	}
	env, err := b.omx.SendMessage(ctx, teamcli.SendMessageInput{
		FromWorker: FromWorker,
		ToWorker:   b.leaderWorker,
		Body:       body,
	})
	if err != nil {
		// N1: omx CLI exec / transport failure is transient. Tell the user
		// we couldn't deliver right now, but leave the tg_updates row
		// unprocessed so the next boot's drainPending retries it. Avoids
		// losing inbound when the bridge crashes mid-send.
		_, _ = b.bot.SendToSession(fmt.Sprintf("Could not deliver: %s", shortErr(err)))
		b.logger.Warn("send_message_transient_leaving_unprocessed",
			"update_id", updateID, "err", err)
		return
	}
	if !env.OK {
		// Non-OK envelope from omx is terminal for this message — mark done
		// so we don't loop retrying a team-not-found / malformed input / etc.
		// User gets a single notification; manual /start or retry is on them.
		if env.Error != nil && env.Error.Code == "team_not_found" {
			_, _ = b.bot.SendToSession(fmt.Sprintf("Team %s is not running. Use /start.", b.cfg.Session.TeamName))
		} else {
			_, _ = b.bot.SendToSession(fmt.Sprintf("Could not deliver: %s", env.Error.ErrorString()))
		}
		_ = b.db.MarkTGUpdateDone(updateID)
		return
	}
	var data struct {
		Message struct {
			MessageID string `json:"message_id"`
		} `json:"message"`
	}
	_ = json.Unmarshal(env.Data, &data)
	// G1 transactional: mark processed + record outbound message in same tx.
	if err := b.db.MarkTGUpdateProcessed(updateID, data.Message.MessageID, text, userID); err != nil {
		b.logger.Error("mark_processed_failed", "err", err, "update_id", updateID)
	}
	b.startEventLoop(ctx)
}

// maybeWarnAfterWake implements G6: on boot, if last successful poll was
// more than 23h ago, push a one-time "Bridge woke after <d>" notice, then
// suppress re-push for 1h.
func (b *Bridge) maybeWarnAfterWake(ctx context.Context) {
	_ = ctx
	last, _ := b.db.KVGet(kvLastPoll)
	if last == "" {
		return
	}
	lastT, err := time.Parse(time.RFC3339, last)
	if err != nil {
		return
	}
	gap := time.Since(lastT)
	if gap < 23*time.Hour {
		return
	}
	lastWarn, _ := b.db.KVGet(kvLastWakeWarnedAt)
	if lastWarn != "" {
		if wt, err := time.Parse(time.RFC3339, lastWarn); err == nil && time.Since(wt) < time.Hour {
			return
		}
	}
	_, _ = b.bot.SendToSession(fmt.Sprintf("Bridge woke after %s; some older updates may have been dropped by Telegram.", gap.Round(time.Minute)))
	_ = b.db.KVSet(kvLastWakeWarnedAt, time.Now().UTC().Format(time.RFC3339))
}

// wrapForOmcReply prepends explicit reply-via-API instructions to a plain-text
// message routed at omc. Without this, claude's runtime decision to reply via
// the send-message API vs printing in-pane is prompt-dependent — short or terse
// user texts often get answered in the pane only and the reply never reaches
// the tg-bridge mailbox. omx does not need this; its workers are launched with
// dispatch hooks that handle the round-trip implicitly.
func (b *Bridge) wrapForOmcReply(userText string) string {
	return fmt.Sprintf(
		"Telegram user message routed via admiral. To reply, run:\n\n"+
			"omc team api send-message --json --input '{\"team_name\":%q,\"from_worker\":%q,\"to_worker\":%q,\"body\":\"<your reply text>\"}'\n\n"+
			"Do not just print the answer in the pane; the user will not see it. Call the API.\n\n"+
			"User: %s",
		b.cfg.Session.TeamName,
		b.leaderWorker,
		FromWorker,
		userText,
	)
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
