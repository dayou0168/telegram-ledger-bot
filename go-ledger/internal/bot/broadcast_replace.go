package bot

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/telegram"
)

type broadcastReplacement struct {
	Kind    string
	Text    string
	Caption string
}

func (b *Bot) tryReplaceBroadcastDelivery(ctx context.Context, delivery storage.BroadcastDelivery, original telegram.Message) storage.BroadcastDelivery {
	if delivery.Mode != "chat" || delivery.ReplacedAt != nil {
		return delivery
	}
	setting, err := b.store.GetBroadcastReplaceSetting(ctx)
	if err != nil {
		log.Printf("get broadcast replace setting: %v", err)
		return delivery
	}
	plan := broadcastReplacementPlan(delivery.Mode, delivery.ReplacedAt != nil, original, setting)
	if plan.Kind == "" {
		return delivery
	}
	switch plan.Kind {
	case "photo":
		if _, err := b.editPhotoBytes(ctx, delivery.TargetChatID, delivery.TargetMessageID, setting.ImageName, setting.ImageData, plan.Caption, nil); err != nil {
			log.Printf("replace broadcast media in place: %v", err)
			replacement, sendErr := b.sendPhotoBytes(ctx, delivery.TargetChatID, setting.ImageName, setting.ImageData, plan.Caption, nil)
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
	case "text":
		if _, err := b.editText(ctx, delivery.TargetChatID, delivery.TargetMessageID, plan.Text, nil); err != nil {
			log.Printf("replace broadcast text failed for delivery %d: %v", delivery.ID, err)
			return delivery
		}
	case "caption":
		if _, err := b.editCaption(ctx, delivery.TargetChatID, delivery.TargetMessageID, plan.Caption, nil); err != nil {
			log.Printf("replace broadcast caption failed for delivery %d: %v", delivery.ID, err)
			return delivery
		}
	}
	if b.markBroadcastReplaced(ctx, delivery.ID) {
		now := time.Now().In(b.loc)
		delivery.ReplacedAt = &now
	}
	return delivery
}

func broadcastReplacementPlan(mode string, replaced bool, original telegram.Message, setting storage.BroadcastReplaceSetting) broadcastReplacement {
	if mode != "chat" || replaced || !setting.Enabled {
		return broadcastReplacement{}
	}
	fixedText := strings.TrimSpace(setting.Text)
	hasPhoto := len(original.Photo) > 0
	if !hasPhoto {
		if original.Text != "" && fixedText != "" {
			return broadcastReplacement{Kind: "text", Text: fixedText}
		}
		return broadcastReplacement{}
	}
	hasCaption := strings.TrimSpace(original.Caption) != ""
	if len(setting.ImageData) > 0 {
		caption := ""
		if hasCaption {
			caption = original.Caption
			if fixedText != "" {
				caption = fixedText
			}
		}
		return broadcastReplacement{Kind: "photo", Caption: caption}
	}
	if hasCaption && fixedText != "" {
		return broadcastReplacement{Kind: "caption", Caption: fixedText}
	}
	return broadcastReplacement{}
}

func (b *Bot) markBroadcastReplaced(ctx context.Context, deliveryID int64) bool {
	updated, err := b.store.MarkBroadcastDeliveryReplaced(ctx, deliveryID, time.Now().In(b.loc))
	if err != nil {
		log.Printf("mark broadcast replaced: %v", err)
		return false
	}
	return updated
}
