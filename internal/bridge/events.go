package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/georgehuang/admiral/internal/tg"
)

// routeOutcome describes how the event-loop should treat a single event
// after routeEvent handled it. Separating "advance cursor" from "pushed"
// lets us hold the cursor on transient push failures (per spec §4.2) while
// still advancing past events that were intentionally suppressed.
type routeOutcome int

const (
	// routeAdvance means the event has reached a terminal outcome (pushed
	// successfully, or explicitly suppressed). Cursor should advance.
	routeAdvance routeOutcome = iota
	// routeHold means the event needed a push that failed transiently. The
	// cursor must NOT advance so the next await-event re-delivers it.
	routeHold
)

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
	if b.caps.SupportsAwaitEvent {
		b.runAwaitEventLoop(ctx)
		return
	}
	b.runPollingLoop(ctx)
}

// runAwaitEventLoop is the omx-style long-poll loop that consumes the
// `team api await-event` event stream and routes each event per spec §4.
func (b *Bridge) runAwaitEventLoop(ctx context.Context) {
	b.logger.Info("event_loop_start_await", "team", b.cfg.Session.TeamName)
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
			outcome := b.routeEvent(ctx, payload.Event)
			if outcome == routeAdvance && payload.Cursor != "" {
				_ = b.db.SetCursor(b.cfg.Session.TeamName, payload.Cursor)
			}
			// routeHold: cursor stays; next await-event re-delivers this event.
		default:
			// unknown status — do not advance cursor
		}
	}
}

// runPollingLoop is the omc-style fallback for providers without
// await-event. It periodically (a) confirms team liveness via
// get-summary, and (b) drains undelivered tg-bridge mailbox messages
// to TG. Cursor state is unused — mailbox-mark-delivered handles
// dedupe. omc does not synthesize task_completed / all_workers_idle
// / approval_decision / shutdown_ack events, so those notifications
// are not pushed under this provider.
func (b *Bridge) runPollingLoop(ctx context.Context) {
	b.logger.Info("event_loop_start_polling", "team", b.cfg.Session.TeamName)
	defer b.logger.Info("event_loop_stop")

	interval := time.Duration(b.cfg.EventStream.IdleBackoffMs) * time.Millisecond
	if interval <= 0 {
		interval = 2 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var missing int
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		summary, err := b.omx.GetSummary(ctx)
		if err != nil {
			b.logger.Warn("polling_summary_err", "err", err)
			continue
		}
		if !summary.OK {
			if summary.Error != nil && summary.Error.Code == "team_not_found" {
				missing++
				if missing >= 3 {
					_, _ = b.bot.SendToSession(fmt.Sprintf(
						"Team %s went down unexpectedly. Use /start to relaunch.",
						b.cfg.Session.TeamName))
					b.stopEventLoop()
					return
				}
			}
			continue
		}
		missing = 0

		mb, err := b.omx.MailboxList(ctx, FromWorker, false)
		if err != nil || !mb.OK {
			if err != nil {
				b.logger.Warn("polling_mailbox_err", "err", err)
			}
			continue
		}
		var list struct {
			Messages []struct {
				MessageID  string `json:"message_id"`
				FromWorker string `json:"from_worker"`
				Body       string `json:"body"`
				Delivered  bool   `json:"delivered"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(mb.Data, &list)
		for _, m := range list.Messages {
			if m.Delivered {
				continue
			}
			text := fmt.Sprintf("%s\n%s", m.FromWorker, m.Body)
			if outcome := b.pushWithRetry(ctx, text); outcome == routeAdvance {
				_, _ = b.omx.MailboxMarkDelivered(ctx, FromWorker, m.MessageID)
			}
			// routeHold: leave undelivered, next tick re-fetches.
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

// routeEvent filters an event and, for the push cases, returns routeAdvance
// on success (spec §4 + §4.2) or routeHold on transient push failure. Events
// in the spec's "suppress" set also return routeAdvance — intentionally-
// suppressed events should not wedge the cursor.
func (b *Bridge) routeEvent(ctx context.Context, raw json.RawMessage) routeOutcome {
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
		// B2 fix: only push leader→tg-bridge mails. Spec §4: "message_received
		// with to_worker==tg-bridge". A leader send to some other worker is
		// suppressed, not forwarded to TG.
		if ev.ToWorker != FromWorker {
			return routeAdvance
		}
		return b.pushLeaderReply(ctx, ev.MessageID)
	case "task_completed":
		return b.pushWithRetry(ctx, fmt.Sprintf("Task done: %s — %s", ev.TaskID, ev.Reason))
	case "task_failed":
		return b.pushWithRetry(ctx, fmt.Sprintf("Task failed: %s — %s", ev.TaskID, ev.Reason))
	case "approval_decision":
		return b.pushWithRetry(ctx, fmt.Sprintf("Approval: %s → %s", ev.TaskID, ev.State))
	case "all_workers_idle":
		if time.Since(b.lastIdlePush) > 60*time.Second {
			outcome := b.pushWithRetry(ctx, "Team idle.")
			if outcome == routeAdvance {
				b.lastIdlePush = time.Now()
			}
			return outcome
		}
		// Rate-limited suppression: still advance past this event.
		return routeAdvance
	case "shutdown_ack":
		return b.pushWithRetry(ctx, fmt.Sprintf("Team %s shut down.", b.cfg.Session.TeamName))
	}
	// default-deny: explicitly suppress, advance cursor.
	return routeAdvance
}

// pushWithRetry sends text to TG using the spec §4.2 backoff schedule.
// Permanent TG errors (403/400 class) log ERROR but still advance the
// cursor — retrying them won't help and holding the cursor would wedge
// the stream permanently.
func (b *Bridge) pushWithRetry(ctx context.Context, text string) routeOutcome {
	err := b.bot.SendToSessionWithRetry(ctx, text)
	if err == nil {
		return routeAdvance
	}
	if errors.Is(err, tg.ErrPermanent) {
		b.logger.Error("tg_send_permanent", "err", err)
		return routeAdvance
	}
	b.logger.Warn("tg_send_transient_holding_cursor", "err", err)
	return routeHold
}

// pushLeaderReply fetches the full body for the specific message_id from
// the tg-bridge mailbox and pushes it. Per G3 the event carries message_id.
// If message_id is empty, we log WARN and suppress rather than guess at
// an oldest-undelivered fallback (picking the wrong message is worse than
// missing one — user will see the next one and can ask for context).
func (b *Bridge) pushLeaderReply(ctx context.Context, messageID string) routeOutcome {
	if messageID == "" {
		b.logger.Warn("leader_reply_no_message_id")
		return routeAdvance
	}
	env, err := b.omx.MailboxList(ctx, FromWorker, false)
	if err != nil || !env.OK {
		// Transient: the mailbox op failed, event stays in queue and we retry.
		b.logger.Warn("mailbox_list_fail", "err", err, "message_id", messageID)
		return routeHold
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
		if m.MessageID != messageID {
			continue
		}
		text := fmt.Sprintf("%s\n%s", m.FromWorker, m.Body)
		outcome := b.pushWithRetry(ctx, text)
		if outcome == routeAdvance {
			// Only mark delivered after successful TG push, so a push-failure
			// retry on the next loop can re-fetch the body.
			_, _ = b.omx.MailboxMarkDelivered(ctx, FromWorker, m.MessageID)
		}
		return outcome
	}
	// Body not in mailbox — shouldn't happen in v0.1 since only tg-bridge
	// reads its own mailbox. Push a placeholder so the user sees *something*
	// and advance cursor. We do NOT call MailboxMarkDelivered here: if the
	// message_id was never in our mailbox (e.g. a bug or stale event), marking
	// it delivered would stamp the wrong mailbox entry or be a no-op at best.
	b.logger.Warn("body_fetch_miss", "message_id", messageID)
	return b.pushWithRetry(ctx, fmt.Sprintf("%s\n[body unavailable]", b.leaderWorker))
}
