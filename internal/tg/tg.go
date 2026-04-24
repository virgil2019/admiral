package tg

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// ErrPermanent is returned by SendWithRetry when TG replies with a non-retryable
// error (bot blocked, chat not found, etc). Event cursor should still advance
// in this case — the event will never be deliverable to this chat.
var ErrPermanent = errors.New("tg permanent send failure")

type Bot struct {
	api     *tgbotapi.BotAPI
	chatID  int64
	timeout int
}

func New(token string, chatID int64, longPollTimeoutS int) (*Bot, error) {
	api, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return nil, fmt.Errorf("tg bot init: %w", err)
	}
	if longPollTimeoutS <= 0 {
		longPollTimeoutS = 50
	}
	return &Bot{api: api, chatID: chatID, timeout: longPollTimeoutS}, nil
}

func (b *Bot) Updates() tgbotapi.UpdatesChannel {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = b.timeout
	return b.api.GetUpdatesChan(u)
}

// Send posts a plain-text message to the session chat. Returns message_id if sent.
func (b *Bot) Send(chatID int64, text string) (int, error) {
	if len(text) == 0 {
		return 0, nil
	}
	if len(text) > 3500 {
		text = text[:3500] + fmt.Sprintf("\n\n... [truncated — %d bytes total]", len(text))
	}
	msg := tgbotapi.NewMessage(chatID, text)
	sent, err := b.api.Send(msg)
	if err != nil {
		return 0, err
	}
	return sent.MessageID, nil
}

func (b *Bot) SendToSession(text string) (int, error) {
	return b.Send(b.chatID, text)
}

// retryBackoffs is the spec-§4.2 schedule for transient TG send errors.
var retryBackoffs = []time.Duration{1 * time.Second, 3 * time.Second, 10 * time.Second}

// SendWithRetry posts to chat with the spec retry policy. Returns nil on
// successful delivery, ErrPermanent when TG reports a permanent failure
// (caller should log ERROR but still advance the event cursor), or a
// transient error after exhausting retries (caller should hold the cursor).
func (b *Bot) SendWithRetry(ctx context.Context, chatID int64, text string) error {
	attempts := len(retryBackoffs) + 1 // initial try + retries
	var lastErr error
	for i := 0; i < attempts; i++ {
		if _, err := b.Send(chatID, text); err == nil {
			return nil
		} else {
			lastErr = err
			if isPermanentTGError(err) {
				return fmt.Errorf("%w: %v", ErrPermanent, err)
			}
		}
		if i == attempts-1 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retryBackoffs[i]):
		}
	}
	return fmt.Errorf("tg send failed after %d attempts: %w", attempts, lastErr)
}

func (b *Bot) SendToSessionWithRetry(ctx context.Context, text string) error {
	return b.SendWithRetry(ctx, b.chatID, text)
}

// isPermanentTGError matches 4xx TG responses that won't succeed on retry —
// 403 bot blocked by user, 400 chat not found, etc. The go-telegram-bot-api
// library doesn't expose the status code cleanly via typed errors, so we
// match on the common substrings it surfaces.
func isPermanentTGError(err error) bool {
	s := err.Error()
	return strings.Contains(s, "Forbidden") ||
		strings.Contains(s, "bot was blocked") ||
		strings.Contains(s, "chat not found") ||
		strings.Contains(s, "user is deactivated") ||
		strings.Contains(s, "Bad Request")
}
