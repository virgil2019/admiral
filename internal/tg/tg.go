package tg

import (
	"fmt"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

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
