package bot

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/telegram"
)

const (
	notificationOutboxBatchSize    = 50
	notificationOutboxMaxAttempt   = 8
	notificationOutboxCleanupEvery = time.Hour
)

func (b *Bot) notificationOutboxScheduler(ctx context.Context) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	cleanupTicker := time.NewTicker(notificationOutboxCleanupEvery)
	defer cleanupTicker.Stop()
	b.drainNotificationOutbox(ctx)
	b.cleanupNotificationOutbox(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-b.notificationWake:
			b.drainNotificationOutbox(ctx)
		case <-ticker.C:
			b.drainNotificationOutbox(ctx)
		case <-cleanupTicker.C:
			b.cleanupNotificationOutbox(ctx)
		}
	}
}

func (b *Bot) kickNotificationOutbox() {
	select {
	case b.notificationWake <- struct{}{}:
	default:
	}
}

func (b *Bot) wakeCriticalOutbox(id int64) {
	if id <= 0 || !b.cfg.CriticalOutboxFastpath {
		b.kickNotificationOutbox()
		return
	}
	select {
	case b.criticalOutboxWake <- id:
	default:
		// The generic scheduler remains the durable lost-wake recovery path.
		b.kickNotificationOutbox()
	}
}

func (b *Bot) criticalOutboxScheduler(ctx context.Context) {
	if !b.cfg.CriticalOutboxFastpath {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-b.criticalOutboxWake:
			item, ok, err := b.store.ClaimNotificationByID(ctx, id, notificationOutboxMaxAttempt, time.Now().In(b.loc))
			if err != nil {
				log.Printf("claim critical outbox %d: %v", id, err)
				b.kickNotificationOutbox()
				continue
			}
			if !ok {
				continue
			}
			if !b.criticalPool.Submit(func(sendCtx context.Context) { b.sendOutboxNotification(sendCtx, item) }) {
				now := time.Now().In(b.loc)
				_ = b.store.MarkNotificationFailed(ctx, item.ID, "critical notification queue is full", now.Add(time.Second), now)
				b.kickNotificationOutbox()
			}
		}
	}
}

func (b *Bot) cleanupNotificationOutbox(ctx context.Context) {
	if b == nil || b.store == nil {
		return
	}
	now := time.Now().In(b.loc)
	sentRetention := b.cfg.OutboxSentRetention
	if sentRetention <= 0 {
		sentRetention = 72 * time.Hour
	}
	failedRetention := b.cfg.OutboxFailedRetention
	if failedRetention <= 0 {
		failedRetention = 14 * 24 * time.Hour
	}
	stats, err := b.store.CleanupNotificationOutbox(ctx, now.Add(-sentRetention), now.Add(-failedRetention))
	if err != nil {
		log.Printf("cleanup notification outbox: %v", err)
	} else if stats.SentDeleted > 0 || stats.FailedDeleted > 0 {
		log.Printf("cleanup notification outbox: sent=%d failed=%d", stats.SentDeleted, stats.FailedDeleted)
	}
	deliveryRetention := b.cfg.BroadcastDeliveryRetention
	if deliveryRetention <= 0 {
		deliveryRetention = 168 * time.Hour
	}
	if deleted, err := b.store.CleanupBroadcastDeliveries(ctx, now.Add(-deliveryRetention)); err != nil {
		log.Printf("cleanup broadcast deliveries: %v", err)
	} else if deleted > 0 {
		log.Printf("cleanup broadcast deliveries: deleted=%d", deleted)
	}
	if deleted, err := b.store.CleanupLedgerClearTickets(ctx, now.Add(-24*time.Hour)); err != nil {
		log.Printf("cleanup ledger clear tickets: %v", err)
	} else if deleted > 0 {
		log.Printf("cleanup ledger clear tickets: deleted=%d", deleted)
	}
}

func (b *Bot) drainNotificationOutbox(ctx context.Context) {
	for i := 0; i < 3; i++ {
		now := time.Now().In(b.loc)
		items, err := b.store.ClaimDueNotifications(ctx, notificationOutboxBatchSize, notificationOutboxMaxAttempt, now)
		if err != nil {
			log.Printf("claim notification outbox: %v", err)
			return
		}
		if len(items) == 0 {
			return
		}
		for _, item := range items {
			item := item
			pool := b.notifyPool
			if outboxSendPriority(item.Priority) == sendPriorityCritical {
				pool = b.criticalPool
			}
			if !pool.Submit(func(sendCtx context.Context) {
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
	renderStarted := time.Now()
	text, opts, err := b.renderOutboxMessage(ctx, item)
	renderDuration := time.Since(renderStarted)
	if err != nil {
		now := time.Now().In(b.loc)
		if markErr := b.store.MarkNotificationFailed(ctx, item.ID, err.Error(), now.Add(notificationRetryDelay(item.Attempts, err)), now); markErr != nil {
			log.Printf("mark notification render failed %d: %v", item.ID, markErr)
		}
		return
	}
	priority := outboxSendPriority(item.Priority)
	message, err, sendMetrics := b.sendOutboxText(ctx, priority, item.Kind, item.ChatID, text, opts)
	now := time.Now().In(b.loc)
	if err == nil {
		if item.Kind == "chain" {
			logChainOutboxSendTiming(item, priority, renderDuration, sendMetrics, 0, nil)
		}
		if err := b.store.MarkNotificationSent(ctx, item.ID, message.MessageID, now); err != nil {
			log.Printf("mark notification sent %d: %v", item.ID, err)
		}
		return
	}
	delay := notificationRetryDelay(item.Attempts, err)
	if item.Kind == "chain" {
		logChainOutboxSendTiming(item, priority, renderDuration, sendMetrics, delay, err)
	}
	if err := b.store.MarkNotificationFailed(ctx, item.ID, err.Error(), now.Add(delay), now); err != nil {
		log.Printf("mark notification failed %d: %v", item.ID, err)
	}
}

func (b *Bot) sendOutboxText(ctx context.Context, priority sendPriority, kind string, chatID int64, text string, opts map[string]any) (telegram.Message, error, telegramSendMetrics) {
	done := measurePerfStage(ctx, "send_enqueue")
	defer done()
	var msg telegram.Message
	var err error
	var metrics telegramSendMetrics
	if b.sendGateway != nil {
		msg, err, metrics = b.sendGateway.SendMessageWithMetrics(ctx, priority, chatID, text, opts)
	} else {
		err = errTelegramSendGatewayNotConfigured
	}
	if err == nil {
		b.recordOutgoingPrivateChatMessageCategory(ctx, msg, "outgoing", privateCleanupCategoryForKind(kind))
	}
	return msg, err, metrics
}

func logChainOutboxSendTiming(item storage.NotificationOutbox, priority sendPriority, renderDuration time.Duration, metrics telegramSendMetrics, outboxRetry time.Duration, err error) {
	status := "sent"
	if err != nil {
		status = "failed"
	}
	log.Printf(
		"chain outbox send timing: id=%d trace=%s status=%s priority=%s render_ms=%d queue_wait_ms=%d rate_limit_ms=%d telegram_ms=%d telegram_retry_ms=%d outbox_retry_ms=%d total_send_ms=%d telegram_attempts=%d outbox_attempts=%d",
		item.ID,
		outboxTrace(item.DedupeKey),
		status,
		priority.String(),
		renderDuration.Milliseconds(),
		metrics.QueueWaitDuration.Milliseconds(),
		metrics.RateLimitDuration.Milliseconds(),
		metrics.OperationDuration.Milliseconds(),
		metrics.RetryDelayDuration.Milliseconds(),
		outboxRetry.Milliseconds(),
		metrics.TotalDuration.Milliseconds(),
		metrics.Attempts,
		item.Attempts,
	)
}

func outboxTrace(dedupeKey string) string {
	sum := sha256.Sum256([]byte(dedupeKey))
	return fmt.Sprintf("%x", sum[:6])
}

func (b *Bot) renderOutboxMessage(ctx context.Context, item storage.NotificationOutbox) (string, map[string]any, error) {
	if item.Kind == "ledger_bill" && item.ReferenceKind == "ledger_record" && item.ReferenceID > 0 {
		record, ok, err := b.store.GetRecord(ctx, item.ReferenceID)
		if err != nil {
			return "", nil, err
		}
		if !ok {
			return "", nil, fmt.Errorf("ledger record %d not found", item.ReferenceID)
		}
		return b.renderBillMessageForRecord(ctx, record, item.Text)
	}
	opts := notificationOptions(item)
	return item.Text, opts, nil
}

func notificationOptions(item storage.NotificationOutbox) map[string]any {
	opts := map[string]any{}
	if item.ParseMode != "" {
		opts["parse_mode"] = item.ParseMode
	}
	if item.DisablePreview {
		opts["disable_web_page_preview"] = true
		opts["link_preview_options"] = map[string]any{"is_disabled": true}
	}
	if item.ReplyToMessageID > 0 {
		opts["reply_to_message_id"] = item.ReplyToMessageID
	}
	if item.ReplyMarkupJSON != "" {
		var markup any
		if err := json.Unmarshal([]byte(item.ReplyMarkupJSON), &markup); err == nil {
			opts["reply_markup"] = markup
		}
	}
	return opts
}

func outboxSendPriority(value int) sendPriority {
	switch sendPriority(value) {
	case sendPriorityHigh:
		return sendPriorityHigh
	case sendPriorityLow:
		return sendPriorityLow
	default:
		return sendPriorityNormal
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
