package bot

import (
	"context"
	"log"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/telegram"
)

const (
	privateCleanupCheckEvery = 30 * time.Second
	privateCleanupRetention  = 72 * time.Hour
	privateCleanupBatchSize  = 500
)

func (b *Bot) recordIncomingPrivateChatMessage(ctx context.Context, msg telegram.Message, user storage.User, now time.Time) {
	if msg.Chat.Type != "private" || msg.Chat.ID <= 0 || msg.MessageID <= 0 || user.ID <= 0 {
		return
	}
	b.recordPrivateChatMessage(ctx, storage.PrivateChatMessage{
		OperatorUserID: user.ID,
		ChatID:         msg.Chat.ID,
		MessageID:      msg.MessageID,
		Direction:      "incoming",
		CreatedAt:      now,
	})
}

func (b *Bot) recordOutgoingPrivateChatMessage(ctx context.Context, msg telegram.Message, direction string) {
	if msg.Chat.Type != "private" || msg.Chat.ID <= 0 || msg.MessageID <= 0 {
		return
	}
	if direction == "" {
		direction = "outgoing"
	}
	createdAt := time.Now()
	if b.loc != nil {
		createdAt = createdAt.In(b.loc)
	}
	b.recordPrivateChatMessage(ctx, storage.PrivateChatMessage{
		OperatorUserID: msg.Chat.ID,
		ChatID:         msg.Chat.ID,
		MessageID:      msg.MessageID,
		Direction:      direction,
		CreatedAt:      createdAt,
	})
}

func (b *Bot) recordPrivateChatMessage(ctx context.Context, msg storage.PrivateChatMessage) {
	if b.store == nil {
		return
	}
	enabled, err := b.store.IsPrivateCleanupEnabled(ctx, msg.OperatorUserID)
	if err != nil {
		log.Printf("check private cleanup setting %d: %v", msg.OperatorUserID, err)
		return
	}
	if !enabled {
		return
	}
	if err := b.store.RecordPrivateChatMessage(ctx, msg); err != nil {
		log.Printf("record private chat message %d/%d: %v", msg.ChatID, msg.MessageID, err)
	}
}

func (b *Bot) privateCleanupScheduler(ctx context.Context) {
	ticker := time.NewTicker(privateCleanupCheckEvery)
	defer ticker.Stop()
	b.runPrivateCleanupOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.runPrivateCleanupOnce(ctx)
		}
	}
}

func (b *Bot) runPrivateCleanupOnce(ctx context.Context) {
	now := time.Now().In(privateCleanupLocation())
	if _, err := b.store.PurgePrivateChatMessages(ctx, now.Add(-privateCleanupRetention)); err != nil {
		log.Printf("purge private cleanup records: %v", err)
	}
	targets, err := b.store.ListDuePrivateCleanupTargets(ctx, now.Hour()*60+now.Minute(), now.Format("2006-01-02"))
	if err != nil {
		log.Printf("list private cleanup targets: %v", err)
		return
	}
	for _, target := range targets {
		if err := b.runPrivateCleanupForOperator(ctx, target.UserID, now.Format("2006-01-02")); err != nil {
			log.Printf("private cleanup for %d: %v", target.UserID, err)
		}
	}
}

func (b *Bot) runPrivateCleanupForOperator(ctx context.Context, operatorUserID int64, runDate string) error {
	now := time.Now().In(privateCleanupLocation())
	total := 0
	for {
		messages, err := b.store.ListPrivateChatMessagesForCleanup(ctx, operatorUserID, privateCleanupBatchSize)
		if err != nil {
			return err
		}
		if len(messages) == 0 {
			break
		}
		total += len(messages)
		for _, msg := range messages {
			lastError := ""
			if err := b.deletePrivateCleanupMessage(ctx, msg.ChatID, msg.MessageID); err != nil {
				lastError = err.Error()
			}
			if err := b.store.MarkPrivateChatMessageCleanup(ctx, msg.ID, lastError, now); err != nil {
				return err
			}
		}
		if len(messages) < privateCleanupBatchSize {
			break
		}
	}
	if err := b.store.MarkPrivateCleanupRun(ctx, operatorUserID, runDate, now); err != nil {
		return err
	}
	if total > 0 {
		log.Printf("private cleanup operator %d attempted %d messages", operatorUserID, total)
	}
	return nil
}

func (b *Bot) deletePrivateCleanupMessage(ctx context.Context, chatID, messageID int64) error {
	if err := b.waitTelegramSlot(ctx, chatID); err != nil {
		return err
	}
	err := b.tg.DeleteMessage(ctx, chatID, messageID)
	if retry, waitErr := waitTelegramRetry(ctx, err); waitErr != nil {
		return waitErr
	} else if retry {
		if err := b.waitTelegramSlot(ctx, chatID); err != nil {
			return err
		}
		return b.tg.DeleteMessage(ctx, chatID, messageID)
	}
	return err
}

func privateCleanupLocation() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("Asia/Shanghai", 8*3600)
	}
	return loc
}
