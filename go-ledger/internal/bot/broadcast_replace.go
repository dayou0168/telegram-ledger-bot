package bot

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
)

func (b *Bot) tryReplaceBroadcastDelivery(ctx context.Context, delivery storage.BroadcastDelivery) storage.BroadcastDelivery {
	if delivery.Mode != "chat" || delivery.ReplacedAt != nil {
		return delivery
	}
	setting, err := b.store.GetBroadcastReplaceSetting(ctx)
	if err != nil {
		log.Printf("get broadcast replace setting: %v", err)
		return delivery
	}
	if !setting.Enabled {
		return delivery
	}
	text := strings.TrimSpace(setting.Text)
	if len(setting.ImageData) > 0 {
		if _, err := b.editPhotoBytes(ctx, delivery.TargetChatID, delivery.TargetMessageID, setting.ImageName, setting.ImageData, text, nil); err != nil {
			log.Printf("replace broadcast media in place: %v", err)
			replacement, sendErr := b.sendPhotoBytes(ctx, delivery.TargetChatID, setting.ImageName, setting.ImageData, text, nil)
			if sendErr != nil {
				log.Printf("send replacement broadcast media: %v", sendErr)
				return delivery
			}
			now := time.Now().In(b.loc)
			updated, updateErr := b.store.ReplaceBroadcastDeliveryMessage(ctx, delivery.ID, replacement.MessageID, now)
			if updateErr != nil || !updated {
				log.Printf("update replacement broadcast delivery %d: updated=%t err=%v", delivery.ID, updated, updateErr)
				if deleteErr := b.tg.DeleteMessage(ctx, delivery.TargetChatID, replacement.MessageID); deleteErr != nil {
					log.Printf("rollback replacement broadcast message %d: %v", delivery.ID, deleteErr)
				}
				return delivery
			}
			if err := b.tg.DeleteMessage(ctx, delivery.TargetChatID, delivery.TargetMessageID); err != nil {
				log.Printf("delete original broadcast after replacement %d: %v", delivery.ID, err)
			}
			delivery.TargetMessageID = replacement.MessageID
			delivery.ReplacedAt = &now
			return delivery
		}
		b.markBroadcastReplaced(ctx, delivery.ID)
		now := time.Now().In(b.loc)
		delivery.ReplacedAt = &now
		return delivery
	}
	if text == "" {
		return delivery
	}
	if _, err := b.editText(ctx, delivery.TargetChatID, delivery.TargetMessageID, text, nil); err == nil {
		b.markBroadcastReplaced(ctx, delivery.ID)
		return delivery
	}
	if _, err := b.editCaption(ctx, delivery.TargetChatID, delivery.TargetMessageID, text, nil); err == nil {
		b.markBroadcastReplaced(ctx, delivery.ID)
		return delivery
	}
	log.Printf("replace broadcast text/caption failed for delivery %d", delivery.ID)
	return delivery
}

func (b *Bot) markBroadcastReplaced(ctx context.Context, deliveryID int64) {
	if _, err := b.store.MarkBroadcastDeliveryReplaced(ctx, deliveryID, time.Now().In(b.loc)); err != nil {
		log.Printf("mark broadcast replaced: %v", err)
	}
}
