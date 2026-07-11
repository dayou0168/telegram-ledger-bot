package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
)

type reliableMessageRef struct {
	Kind string
	ID   int64
}

func (b *Bot) enqueueReliableText(ctx context.Context, priority sendPriority, kind, dedupeKey string, chatID int64, text string, opts map[string]any, ref reliableMessageRef, now time.Time) error {
	done := measurePerfStage(ctx, "send_enqueue")
	defer done()
	item, err := reliableTextOutboxItem(priority, kind, dedupeKey, chatID, text, opts, ref)
	if err != nil {
		return err
	}
	inserted, err := b.store.EnqueueNotification(ctx, item, now)
	if err != nil {
		return err
	}
	if inserted {
		b.kickNotificationOutbox()
	}
	return nil
}

func (b *Bot) enqueueReplyText(ctx context.Context, priority sendPriority, kind string, chatID, replyTo int64, text string, opts map[string]any, now time.Time) error {
	if opts == nil {
		opts = map[string]any{}
	}
	if replyTo > 0 {
		opts["reply_to_message_id"] = replyTo
	}
	return b.enqueueReliableText(ctx, priority, kind, messageScopedDedupe(kind, chatID, replyTo), chatID, text, opts, reliableMessageRef{}, now)
}

func (b *Bot) enqueueLedgerSuccessText(ctx context.Context, priority sendPriority, kind string, chatID, sourceMessageID int64, text string, opts map[string]any, now time.Time) error {
	return b.enqueueReliableText(ctx, priority, kind, messageScopedDedupe(kind, chatID, sourceMessageID), chatID, text, withoutReplyOptions(opts), reliableMessageRef{}, now)
}

func (b *Bot) enqueueLedgerTraceText(ctx context.Context, priority sendPriority, kind string, chatID, sourceMessageID int64, text string, opts map[string]any, now time.Time) error {
	return b.enqueueReplyText(ctx, priority, kind, chatID, sourceMessageID, text, opts, now)
}

func withoutReplyOptions(opts map[string]any) map[string]any {
	if len(opts) == 0 {
		return nil
	}
	cleaned := make(map[string]any, len(opts))
	for key, value := range opts {
		if key == "reply_to_message_id" || key == "reply_parameters" {
			continue
		}
		cleaned[key] = value
	}
	return cleaned
}

func (b *Bot) sendReliableTextAsync(ctx context.Context, priority sendPriority, kind, dedupeKey string, chatID int64, text string, opts map[string]any, ref reliableMessageRef) {
	now := time.Now().In(b.loc)
	key := "outbox:" + strconv.FormatInt(chatID, 10)
	b.dispatcher.Submit(ctx, key, b.notifyPool, func(jobCtx context.Context) {
		if err := b.enqueueReliableText(jobCtx, priority, kind, dedupeKey, chatID, text, opts, ref, now); err != nil {
			log.Printf("enqueue reliable text %s: %v", dedupeKey, err)
		}
	})
}

func reliableTextOutboxItem(priority sendPriority, kind, dedupeKey string, chatID int64, text string, opts map[string]any, ref reliableMessageRef) (storage.NotificationOutbox, error) {
	parseMode, disablePreview, replyTo, replyMarkup, err := extractReliableTextOptions(opts)
	if err != nil {
		return storage.NotificationOutbox{}, err
	}
	return storage.NotificationOutbox{
		Kind:             kind,
		DedupeKey:        dedupeKey,
		ChatID:           chatID,
		Text:             text,
		ParseMode:        parseMode,
		DisablePreview:   disablePreview,
		ReplyToMessageID: replyTo,
		ReplyMarkupJSON:  replyMarkup,
		ReferenceKind:    ref.Kind,
		ReferenceID:      ref.ID,
		Priority:         int(priority),
	}, nil
}

func extractReliableTextOptions(opts map[string]any) (string, bool, int64, string, error) {
	var parseMode string
	var disablePreview bool
	var replyTo int64
	var replyMarkup string
	for key, value := range opts {
		switch key {
		case "parse_mode":
			parseMode = fmt.Sprint(value)
		case "disable_web_page_preview":
			disablePreview = boolOption(value)
		case "link_preview_options":
			if isDisabledLinkPreview(value) {
				disablePreview = true
			}
		case "reply_to_message_id":
			replyTo = int64Option(value)
		case "reply_markup":
			raw, err := json.Marshal(value)
			if err != nil {
				return "", false, 0, "", err
			}
			replyMarkup = string(raw)
		}
	}
	return parseMode, disablePreview, replyTo, replyMarkup, nil
}

func boolOption(value any) bool {
	switch v := value.(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(v, "true") || v == "1"
	default:
		return false
	}
}

func int64Option(value any) int64 {
	switch v := value.(type) {
	case int:
		return int64(v)
	case int64:
		return v
	case int32:
		return int64(v)
	case float64:
		return int64(v)
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		return n
	default:
		return 0
	}
}

func isDisabledLinkPreview(value any) bool {
	options, ok := value.(map[string]any)
	if !ok {
		return false
	}
	return boolOption(options["is_disabled"])
}

func messageScopedDedupe(kind string, chatID, messageID int64) string {
	return fmt.Sprintf("%s:%d:%d", kind, chatID, messageID)
}
