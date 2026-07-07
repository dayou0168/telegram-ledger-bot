package bot

import (
	"context"
	"log"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/telegram"
)

const notificationOutboxBatchSize = 50

func (b *Bot) notificationOutboxScheduler(ctx context.Context) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	b.drainNotificationOutbox(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-b.notificationWake:
			b.drainNotificationOutbox(ctx)
		case <-ticker.C:
			b.drainNotificationOutbox(ctx)
		}
	}
}

func (b *Bot) kickNotificationOutbox() {
	select {
	case b.notificationWake <- struct{}{}:
	default:
	}
}

func (b *Bot) drainNotificationOutbox(ctx context.Context) {
	for i := 0; i < 3; i++ {
		now := time.Now().In(b.loc)
		items, err := b.store.ClaimDueNotifications(ctx, notificationOutboxBatchSize, now)
		if err != nil {
			log.Printf("claim notification outbox: %v", err)
			return
		}
		if len(items) == 0 {
			return
		}
		for _, item := range items {
			item := item
			if !b.notifyPool.Submit(func(sendCtx context.Context) {
				b.sendOutboxNotification(sendCtx, item)
			}) {
				next := time.Now().In(b.loc).Add(2 * time.Second)
				if err := b.store.MarkNotificationFailed(ctx, item.ID, "notification queue is full", next, time.Now().In(b.loc)); err != nil {
					log.Printf("mark notification queue full: %v", err)
				}
			}
		}
		if len(items) < notificationOutboxBatchSize {
			return
		}
	}
}

func (b *Bot) sendOutboxNotification(ctx context.Context, item storage.NotificationOutbox) {
	opts := map[string]any{}
	if item.ParseMode != "" {
		opts["parse_mode"] = item.ParseMode
	}
	if item.DisablePreview {
		opts["disable_web_page_preview"] = true
		opts["link_preview_options"] = map[string]any{"is_disabled": true}
	}
	_, err := b.sendText(ctx, sendPriorityHigh, item.ChatID, item.Text, opts)
	now := time.Now().In(b.loc)
	if err == nil {
		if err := b.store.MarkNotificationSent(ctx, item.ID, now); err != nil {
			log.Printf("mark notification sent %d: %v", item.ID, err)
		}
		return
	}
	delay := notificationRetryDelay(item.Attempts, err)
	if err := b.store.MarkNotificationFailed(ctx, item.ID, err.Error(), now.Add(delay), now); err != nil {
		log.Printf("mark notification failed %d: %v", item.ID, err)
	}
}

func notificationRetryDelay(attempts int, err error) time.Duration {
	if retryAfter, ok := telegram.RetryAfter(err); ok {
		return retryAfter + time.Second
	}
	switch {
	case attempts <= 1:
		return 2 * time.Second
	case attempts == 2:
		return 5 * time.Second
	case attempts == 3:
		return 15 * time.Second
	case attempts == 4:
		return 30 * time.Second
	case attempts == 5:
		return time.Minute
	default:
		return 5 * time.Minute
	}
}
