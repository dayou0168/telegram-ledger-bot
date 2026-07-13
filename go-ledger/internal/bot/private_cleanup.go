package bot

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/telegram"
)

type privateCleanupCategoryKey struct{}

func withPrivateCleanupCategory(ctx context.Context, category string) context.Context {
	return context.WithValue(ctx, privateCleanupCategoryKey{}, normalizePrivateCleanupCategory(category))
}

func privateCleanupCategoryFromContext(ctx context.Context) string {
	if category, ok := ctx.Value(privateCleanupCategoryKey{}).(string); ok {
		return normalizePrivateCleanupCategory(category)
	}
	return "menu"
}

func normalizePrivateCleanupCategory(category string) string {
	switch strings.TrimSpace(category) {
	case "broadcast", "quick_reply", "menu":
		return strings.TrimSpace(category)
	default:
		return "menu"
	}
}

func privateCleanupCategoryForKind(kind string) string {
	kind = strings.ToLower(strings.TrimSpace(kind))
	switch {
	case strings.HasPrefix(kind, "quick_reply"), strings.HasPrefix(kind, "broadcast_reply"):
		return "quick_reply"
	case strings.HasPrefix(kind, "broadcast"):
		return "broadcast"
	default:
		return "menu"
	}
}

const (
	privateCleanupCheckEvery = 30 * time.Second
	privateCleanupRetention  = 72 * time.Hour
	privateCleanupBatchSize  = 500
)

func (b *Bot) recordIncomingPrivateChatMessage(ctx context.Context, msg telegram.Message, user storage.User, now time.Time) {
	if msg.Chat.Type != "private" || msg.Chat.ID <= 0 || msg.MessageID <= 0 || user.ID <= 0 {
		return
	}
	category := "menu"
	if state, ok := b.privateStates.Get(formatID(user.ID)); ok {
		if state.Mode == "quick_reply" {
			category = "quick_reply"
		} else if len(state.ChatIDs) > 0 {
			category = "broadcast"
		}
	}
	b.recordPrivateChatMessage(ctx, storage.PrivateChatMessage{
		OperatorUserID: user.ID,
		ChatID:         msg.Chat.ID,
		MessageID:      msg.MessageID,
		Direction:      "incoming",
		Category:       category,
		CreatedAt:      now,
	})
}

func (b *Bot) recordOutgoingPrivateChatMessage(ctx context.Context, msg telegram.Message, direction string) {
	b.recordOutgoingPrivateChatMessageCategory(ctx, msg, direction, privateCleanupCategoryFromContext(ctx))
}

func (b *Bot) recordOutgoingPrivateChatMessageCategory(ctx context.Context, msg telegram.Message, direction, category string) {
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
		Category:       normalizePrivateCleanupCategory(category),
		CreatedAt:      createdAt,
	})
}

func (b *Bot) recordPrivateChatMessage(ctx context.Context, msg storage.PrivateChatMessage) {
	if b.store == nil {
		return
	}
	settings, ok, err := b.store.GetPrivateCleanupSettings(ctx, msg.OperatorUserID)
	if err != nil {
		log.Printf("check private cleanup setting %d: %v", msg.OperatorUserID, err)
		return
	}
	if !ok || !settings.Enabled {
		return
	}
	if !storage.PrivateCleanupScopeIncludes(settings.Scope, msg.Category) {
		return
	}
	msg.Category, msg.CleanupAfterSeconds, msg.DueAt = privateCleanupMessageSchedule(msg, settings)
	if msg.Direction == "incoming" && !settings.IncomingEnabled {
		return
	}
	if msg.DueAt == nil && settings.DailyTime == "" {
		return
	}
	if err := b.store.RecordPrivateChatMessage(ctx, msg); err != nil {
		log.Printf("record private chat message %d/%d: %v", msg.ChatID, msg.MessageID, err)
	}
}

func privateCleanupMessageSchedule(msg storage.PrivateChatMessage, settings storage.PrivateCleanupSettings) (string, int, *time.Time) {
	category := msg.Category
	if category == "" {
		category = "private"
	}
	seconds := 0
	if msg.Direction == "incoming" {
		seconds = settings.IncomingDeleteAfter
	} else {
		seconds = settings.BotDeleteAfter
	}
	if seconds <= 0 {
		return category, 0, nil
	}
	createdAt := msg.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	dueAt := createdAt.Add(time.Duration(seconds) * time.Second)
	return category, seconds, &dueAt
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
	if err := b.runDuePrivateCleanup(ctx, now); err != nil {
		log.Printf("due private cleanup: %v", err)
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

func (b *Bot) runDuePrivateCleanup(ctx context.Context, now time.Time) error {
	for {
		messages, err := b.store.ListDuePrivateChatMessagesForCleanup(ctx, now, privateCleanupBatchSize)
		if err != nil {
			return err
		}
		if len(messages) == 0 {
			return nil
		}
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
			return nil
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
	if b.sendGateway != nil {
		_, err := b.sendGateway.Do(ctx, sendPriorityBulk, chatID, func(opCtx context.Context) (telegram.Message, error) {
			return telegram.Message{}, b.tg.DeleteMessage(opCtx, chatID, messageID)
		})
		return err
	}
	return errTelegramSendGatewayNotConfigured
}

func privateCleanupLocation() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("Asia/Shanghai", 8*3600)
	}
	return loc
}
