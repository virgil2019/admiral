package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// startEventLoop launches a goroutine that long-polls `await-event` and pushes
// filtered events to TG. Idempotent — if already running, returns quickly.
func (b *Bridge) startEventLoop(parent context.Context) {
	b.eventMu.Lock()
	defer b.eventMu.Unlock()
	if b.eventRunning {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	b.eventCancel = cancel
	b.eventRunning = true
	go b.runEventLoop(ctx)
}

func (b *Bridge) stopEventLoop() {
	b.eventMu.Lock()
	defer b.eventMu.Unlock()
	if b.eventCancel != nil {
		b.eventCancel()
	}
	b.eventRunning = false
	b.eventCancel = nil
}

func (b *Bridge) runEventLoop(ctx context.Context) {
	b.logger.Info("event_loop_start", "team", b.cfg.Session.TeamName)
	defer b.logger.Info("event_loop_stop")

	var missing int
	for {
		if ctx.Err() != nil {
			return
		}
		cursor, _ := b.db.GetCursor(b.cfg.Session.TeamName)
		env, err := b.omx.AwaitEvent(ctx, cursor, b.cfg.EventStream.AwaitTimeoutMs)
		if err != nil {
			b.logger.Warn("await_event_err", "err", err)
			if b.backoff(ctx) {
				return
			}
			continue
		}
		if !env.OK {
			if env.Error != nil && env.Error.Code == "team_not_found" {
				missing++
				if missing >= 3 {
					_, _ = b.bot.SendToSession(fmt.Sprintf("Team %s went down unexpectedly. Use /start to relaunch.", b.cfg.Session.TeamName))
					b.stopEventLoop()
					return
				}
			}
			if b.backoff(ctx) {
				return
			}
			continue
		}
		missing = 0

		var payload struct {
			Status string          `json:"status"`
			Cursor string          `json:"cursor"`
			Event  json.RawMessage `json:"event"`
		}
		_ = json.Unmarshal(env.Data, &payload)

		switch payload.Status {
		case "timeout":
			continue
		case "event":
			if pushed := b.routeEvent(ctx, payload.Event); pushed {
				if payload.Cursor != "" {
					_ = b.db.SetCursor(b.cfg.Session.TeamName, payload.Cursor)
				}
			} else if payload.Cursor != "" {
				// suppressed events still advance cursor
				_ = b.db.SetCursor(b.cfg.Session.TeamName, payload.Cursor)
			}
		default:
			// unknown status — do not advance cursor
		}
	}
}

func (b *Bridge) backoff(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	case <-time.After(time.Duration(b.cfg.EventStream.IdleBackoffMs) * time.Millisecond):
		return false
	}
}

// routeEvent filters & pushes. Returns true if something was delivered (or
// an irrecoverable push-failure occurred where the cursor should still advance).
// Returns false for pure suppressed events (cursor should still advance).
func (b *Bridge) routeEvent(ctx context.Context, raw json.RawMessage) bool {
	var ev struct {
		EventID   string `json:"event_id"`
		Type      string `json:"type"`
		Worker    string `json:"worker"`
		ToWorker  string `json:"to_worker"`
		TaskID    string `json:"task_id"`
		Reason    string `json:"reason"`
		Body      string `json:"body"`
		MessageID string `json:"message_id"`
		State     string `json:"state"`
	}
	_ = json.Unmarshal(raw, &ev)

	switch ev.Type {
	case "message_received":
		if ev.ToWorker == FromWorker || ev.Worker == LeaderWorker {
			return b.pushLeaderReply(ctx, ev.MessageID)
		}
		return true
	case "task_completed":
		_, _ = b.bot.SendToSession(fmt.Sprintf("Task done: %s — %s", ev.TaskID, ev.Reason))
		return true
	case "task_failed":
		_, _ = b.bot.SendToSession(fmt.Sprintf("Task failed: %s — %s", ev.TaskID, ev.Reason))
		return true
	case "approval_decision":
		_, _ = b.bot.SendToSession(fmt.Sprintf("Approval: %s → %s", ev.TaskID, ev.State))
		return true
	case "all_workers_idle":
		if time.Since(b.lastIdlePush) > 60*time.Second {
			_, _ = b.bot.SendToSession("Team idle.")
			b.lastIdlePush = time.Now()
		}
		return true
	case "shutdown_ack":
		_, _ = b.bot.SendToSession(fmt.Sprintf("Team %s shut down.", b.cfg.Session.TeamName))
		return true
	}
	// default-deny: suppress
	return true
}

func (b *Bridge) pushLeaderReply(ctx context.Context, messageID string) bool {
	env, err := b.omx.MailboxList(ctx, FromWorker, false)
	if err != nil || !env.OK {
		b.logger.Warn("mailbox_list_fail", "err", err)
		return false
	}
	var list struct {
		Messages []struct {
			MessageID  string `json:"message_id"`
			FromWorker string `json:"from_worker"`
			Body       string `json:"body"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(env.Data, &list)
	for _, m := range list.Messages {
		if messageID != "" && m.MessageID != messageID {
			continue
		}
		text := fmt.Sprintf("%s\n%s", m.FromWorker, m.Body)
		if _, err := b.bot.SendToSession(text); err != nil {
			b.logger.Error("tg_send_fail", "err", err)
			return false
		}
		_, _ = b.omx.MailboxMarkDelivered(ctx, FromWorker, m.MessageID)
		return true
	}
	return false
}
