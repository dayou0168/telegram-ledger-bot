package bot

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
)

func (b *Bot) tryReplaceBroadcastDelivery(ctx context.Context, delivery storage.BroadcastDelivery) {
	if delivery.Mode != "chat" || delivery.ReplacedAt != nil {
		return
	}
	setting, err := b.store.GetBroadcastReplaceSetting(ctx)
	if err != nil {
		log.Printf("get broadcast replace setting: %v", err)
		return
	}
	if !setting.Enabled {
		return
	}
	text := strings.TrimSpace(setting.Text)
	if len(setting.ImageData) > 0 {
		if _, err := b.editPhotoBytes(ctx, delivery.TargetChatID, delivery.TargetMessageID, setting.ImageName, setting.ImageData, text, nil); err != nil {
			log.Printf("replace broadcast media: %v", err)
			return
		}
		b.markBroadcastReplaced(ctx, delivery.ID)
		return
	}
	if text == "" {
		return
	}
	if _, err := b.editText(ctx, delivery.TargetChatID, delivery.TargetMessageID, text, nil); err == nil {
		b.markBroadcastReplaced(ctx, delivery.ID)
		return
	}
	if _, err := b.editCaption(ctx, delivery.TargetChatID, delivery.TargetMessageID, text, nil); err == nil {
		b.markBroadcastReplaced(ctx, delivery.ID)
		return
	}
	log.Printf("replace broadcast text/caption failed for delivery %d", delivery.ID)
}

func (b *Bot) markBroadcastReplaced(ctx context.Context, deliveryID int64) {
	if _, err := b.store.MarkBroadcastDeliveryReplaced(ctx, deliveryID, time.Now().In(b.loc)); err != nil {
		log.Printf("mark broadcast replaced: %v", err)
	}
}
