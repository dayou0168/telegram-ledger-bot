package bot

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/telegram"
)

type reliablePayload struct {
	Type    string
	Text    string
	FileID  string
	Caption string
}

func reliablePayloadFromMessage(msg telegram.Message) (reliablePayload, bool) {
	if len(msg.Photo) > 0 {
		fileID := strings.TrimSpace(msg.Photo[len(msg.Photo)-1].FileID)
		if fileID == "" {
			return reliablePayload{}, false
		}
		return reliablePayload{Type: "photo", FileID: fileID, Caption: msg.Caption}, true
	}
	if msg.Text != "" {
		return reliablePayload{Type: "text", Text: msg.Text}, true
	}
	return reliablePayload{}, false
}

func reliablePayloadOutboxItem(priority sendPriority, kind, dedupeKey string, chatID int64, payload reliablePayload, opts map[string]any, ref reliableMessageRef) (storage.NotificationOutbox, error) {
	parseMode, disablePreview, replyTo, replyMarkup, err := extractReliableTextOptions(opts)
	if err != nil {
		return storage.NotificationOutbox{}, err
	}
	payload.Type = strings.TrimSpace(payload.Type)
	switch payload.Type {
	case "text":
		if payload.Text == "" {
			return storage.NotificationOutbox{}, errors.New("reliable text payload is empty")
		}
	case "photo":
		if strings.TrimSpace(payload.FileID) == "" {
			return storage.NotificationOutbox{}, errors.New("reliable photo payload file id is empty")
		}
	default:
		return storage.NotificationOutbox{}, errors.New("reliable payload type is unsupported")
	}
	return storage.NotificationOutbox{
		Kind: kind, DedupeKey: dedupeKey, ChatID: chatID,
		Text: payload.Text, PayloadType: payload.Type, FileID: payload.FileID, Caption: payload.Caption,
		ParseMode: parseMode, DisablePreview: disablePreview, ReplyToMessageID: replyTo,
		ReplyMarkupJSON: replyMarkup, ReferenceKind: ref.Kind, ReferenceID: ref.ID,
		Priority: int(priority),
	}, nil
}

func (b *Bot) enqueueReliablePayload(ctx context.Context, priority sendPriority, kind, dedupeKey string, chatID int64, payload reliablePayload, opts map[string]any, ref reliableMessageRef, replyToUpstreamID int64, now time.Time) error {
	item, err := reliablePayloadOutboxItem(priority, kind, dedupeKey, chatID, payload, opts, ref)
	if err != nil {
		return err
	}
	item.ReplyToUpstreamID = replyToUpstreamID
	inserted, err := b.store.EnqueueNotification(ctx, item, now)
	if err != nil {
		return err
	}
	if inserted {
		b.kickNotificationOutbox()
	}
	return nil
}
