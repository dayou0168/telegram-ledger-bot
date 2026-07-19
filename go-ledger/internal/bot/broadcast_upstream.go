package bot

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/telegram"
)

const (
	telegramTextLimitUnits    = 4096
	telegramCaptionLimitUnits = 1024
)

type broadcastDispatchContext struct {
	SenderLabel   string
	TargetDisplay string
}

func (b *Bot) broadcastUpstreamRecipients(ctx context.Context, sourceUserID int64) ([]int64, error) {
	ceilings, err := b.broadcastRecipientCeilings(ctx, sourceUserID)
	if err != nil {
		return nil, err
	}
	recipients := append([]int64(nil), ceilings.Broadcast...)
	preferences, err := b.store.BroadcastMessagePreferenceOverridesForSource(ctx, sourceUserID, recipients)
	if err != nil {
		return nil, err
	}
	filtered := recipients[:0]
	for _, recipientID := range recipients {
		if preference, exists := preferences[recipientID]; !exists || preference.ReceiveBroadcast {
			filtered = append(filtered, recipientID)
		}
	}
	return filtered, nil
}

func (b *Bot) broadcastRecipientCeilings(ctx context.Context, sourceUserID int64) (storage.OperatorMessageRecipients, error) {
	hostID := b.perms.HostUserID()
	if sourceUserID == hostID {
		return storage.OperatorMessageRecipients{}, nil
	}
	operator, ok, err := b.store.GetGlobalOperator(ctx, sourceUserID)
	if err != nil {
		return storage.OperatorMessageRecipients{}, err
	}
	if !ok || operator.Status != "active" {
		return storage.OperatorMessageRecipients{}, nil
	}
	switch operator.Level {
	case "primary":
		if hostID > 0 {
			return storage.OperatorMessageRecipients{Broadcast: []int64{hostID}, Reply: []int64{hostID}}, nil
		}
	case "secondary":
		return b.store.ResolveOperatorMessageRecipients(ctx, sourceUserID, hostID)
	}
	return storage.OperatorMessageRecipients{}, nil
}

func (b *Bot) enqueueBroadcastUpstreamCopies(ctx context.Context, msg telegram.Message, source storage.User, dispatch broadcastDispatchContext, now time.Time) error {
	dispatch.SenderLabel = b.broadcastSenderLabel(ctx, source)
	payload, companion, ok := broadcastUpstreamPayloads(msg, dispatch)
	if !ok {
		return nil
	}
	recipients, err := b.broadcastUpstreamRecipients(ctx, source.ID)
	if err != nil {
		return err
	}
	for _, recipientID := range recipients {
		dedupeKey := fmt.Sprintf("broadcast_upstream:%d:%d:%d", msg.Chat.ID, msg.MessageID, recipientID)
		item, itemErr := reliablePayloadOutboxItem(sendPriorityNormal, "broadcast_upstream_copy", dedupeKey, recipientID, payload, nil, reliableMessageRef{})
		if itemErr != nil {
			return itemErr
		}
		var companions []storage.NotificationOutbox
		if companion != nil {
			companionKey := fmt.Sprintf("broadcast_upstream_context:%d:%d:%d", msg.Chat.ID, msg.MessageID, recipientID)
			companionItem, companionErr := reliablePayloadOutboxItem(sendPriorityNormal, "broadcast_upstream_context", companionKey, recipientID, *companion, nil, reliableMessageRef{})
			if companionErr != nil {
				return companionErr
			}
			companions = append(companions, companionItem)
		}
		inserted, _, enqueueErr := b.store.EnqueueBroadcastUpstreamMessage(ctx, item, source.ID, msg.Chat.ID, msg.MessageID, recipientID, now, companions...)
		if enqueueErr != nil {
			return enqueueErr
		}
		if inserted {
			b.kickNotificationOutbox()
		}
	}
	return nil
}

func (b *Bot) broadcastSenderLabel(ctx context.Context, source storage.User) string {
	if username := strings.TrimPrefix(strings.TrimSpace(source.Username), "@"); username != "" {
		return trimSingleLine("@"+username, 80)
	}
	if displayName := trimSingleLine(source.DisplayName, 80); displayName != "" {
		return displayName
	}
	if operator, ok, err := b.store.GetGlobalOperator(ctx, source.ID); err == nil && ok {
		if username := strings.TrimPrefix(strings.TrimSpace(operator.Username), "@"); username != "" {
			return trimSingleLine("@"+username, 80)
		}
		if displayName := trimSingleLine(operator.DisplayName, 80); displayName != "" {
			return displayName
		}
		if remark := trimSingleLine(operator.Remark, 80); remark != "" {
			return remark
		}
	}
	return "未命名操作人"
}

func broadcastUpstreamPayloads(msg telegram.Message, dispatch broadcastDispatchContext) (reliablePayload, *reliablePayload, bool) {
	original, ok := reliablePayloadFromMessage(msg)
	if !ok {
		return reliablePayload{}, nil, false
	}
	senderLabel := trimSingleLine(dispatch.SenderLabel, 80)
	if senderLabel == "" {
		senderLabel = "未命名操作人"
	}
	targetDisplay := trimSingleLine(dispatch.TargetDisplay, 160)
	if targetDisplay == "" {
		targetDisplay = "未命名目标"
	}
	header := fmt.Sprintf("发送人：%s\n发送目标：%s", senderLabel, targetDisplay)
	companionText := header + "\n\n原始内容保留在所回复的消息中。"
	switch original.Type {
	case "text":
		combined := header + "\n\n原始内容：\n" + original.Text
		if telegramTextUnits(combined) <= telegramTextLimitUnits {
			return reliablePayload{Type: "text", Text: combined}, nil, true
		}
		companion := reliablePayload{Type: "text", Text: companionText}
		return original, &companion, true
	case "photo":
		combinedCaption := header
		if original.Caption != "" {
			combinedCaption += "\n\n原始说明：\n" + original.Caption
		}
		if telegramTextUnits(combinedCaption) <= telegramCaptionLimitUnits {
			original.Caption = combinedCaption
			return original, nil, true
		}
		companion := reliablePayload{Type: "text", Text: companionText}
		return original, &companion, true
	default:
		return reliablePayload{}, nil, false
	}
}

func telegramTextUnits(value string) int {
	return len(utf16.Encode([]rune(value)))
}

func trimSingleLine(value string, limit int) string {
	value = strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
	if limit > 0 {
		value = trimRunes(value, limit)
	}
	return value
}
