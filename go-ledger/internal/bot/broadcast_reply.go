package bot

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/telegram"
)

func (b *Bot) notifyBroadcastReplyAsync(ctx context.Context, msg telegram.Message, user storage.User) {
	if msg.ReplyTo == nil {
		return
	}
	chatID := msg.Chat.ID
	replyMessageID := msg.ReplyTo.MessageID
	b.notifyPool.Submit(func(jobCtx context.Context) {
		delivery, ok, err := b.store.FindBroadcastDeliveryByTarget(jobCtx, chatID, replyMessageID)
		if err != nil {
			log.Printf("find broadcast delivery: %v", err)
			return
		}
		if !ok || delivery.OperatorUserID == 0 {
			return
		}
		b.tryReplaceBroadcastDelivery(jobCtx, delivery)
		text := formatBroadcastReplyNotice(msg, user, delivery)
		keyboard := telegram.InlineKeyboardMarkup{InlineKeyboard: [][]telegram.InlineKeyboardButton{
			{{Text: "快速回复", CallbackData: "br:q:" + formatID(delivery.ID)}},
			replyLinkButtons(msg, delivery),
		}}
		if _, err := b.tg.SendMessage(jobCtx, delivery.OperatorUserID, text, map[string]any{
			"reply_markup": keyboard,
		}); err != nil {
			log.Printf("send broadcast reply notice: %v", err)
		}
	})
}

func (b *Bot) handleBroadcastReplyCallback(ctx context.Context, cb telegram.CallbackQuery) error {
	if !strings.HasPrefix(cb.Data, "br:q:") {
		return b.tg.AnswerCallback(ctx, cb.ID, "")
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(cb.Data, "br:q:"), 10, 64)
	if err != nil {
		return b.tg.AnswerCallback(ctx, cb.ID, "通知无效")
	}
	delivery, ok, err := b.findBroadcastDeliveryByID(ctx, id)
	if err != nil {
		return err
	}
	if !ok || delivery.OperatorUserID != cb.From.ID {
		return b.tg.AnswerCallback(ctx, cb.ID, "通知已失效")
	}
	b.privateStates.Set(formatID(cb.From.ID), privateState{
		Mode:                 "quick_reply",
		TargetName:           delivery.TargetTitle,
		QuickReplyTargetChat: delivery.TargetChatID,
		QuickReplyMessageID:  delivery.TargetMessageID,
		CreatedAt:            time.Now().In(b.loc),
	})
	if err := b.tg.AnswerCallback(ctx, cb.ID, "请发送回复内容"); err != nil {
		return err
	}
	if cb.Message == nil {
		return nil
	}
	_, err = b.tg.SendMessage(ctx, cb.Message.Chat.ID, "请直接发送要回复的内容；文字、图片或文件都会复制到目标群。发“返回”结束快速回复。", map[string]any{
		"reply_to_message_id": cb.Message.MessageID,
	})
	return err
}

func (b *Bot) handleQuickReplyMaterial(ctx context.Context, msg telegram.Message, user storage.User, state privateState) error {
	if msg.Text == "菜单" || msg.Text == "/start" || msg.Text == "返回" || msg.Text == "取消" {
		b.privateStates.Delete(formatID(user.ID))
		return b.sendPrivateMenu(ctx, msg.Chat.ID, msg.MessageID)
	}
	targetChatID := state.QuickReplyTargetChat
	replyTo := state.QuickReplyMessageID
	if targetChatID == 0 || replyTo == 0 {
		b.privateStates.Delete(formatID(user.ID))
		_, err := b.tg.SendMessage(ctx, msg.Chat.ID, "快速回复目标已失效，请重新点回复通知。", map[string]any{"reply_to_message_id": msg.MessageID})
		return err
	}
	b.notifyPool.Submit(func(sendCtx context.Context) {
		if _, err := b.tg.CopyMessage(sendCtx, targetChatID, msg.Chat.ID, msg.MessageID, map[string]any{"reply_to_message_id": replyTo}); err != nil {
			log.Printf("send quick reply: %v", err)
			_, _ = b.tg.SendMessage(sendCtx, msg.Chat.ID, "快速回复发送失败："+err.Error(), map[string]any{"reply_to_message_id": msg.MessageID})
			return
		}
		_, _ = b.tg.SendMessage(sendCtx, msg.Chat.ID, "快速回复已发送。可继续发送下一条，或发“返回”结束。", map[string]any{"reply_to_message_id": msg.MessageID})
	})
	return nil
}

func (b *Bot) findBroadcastDeliveryByID(ctx context.Context, id int64) (storage.BroadcastDelivery, bool, error) {
	return b.store.GetBroadcastDelivery(ctx, id)
}

func formatBroadcastReplyNotice(msg telegram.Message, user storage.User, delivery storage.BroadcastDelivery) string {
	group := delivery.TargetTitle
	if group == "" {
		group = msg.Chat.Title
	}
	if group == "" {
		group = formatID(msg.Chat.ID)
	}
	content := strings.TrimSpace(msg.TextOrCaption())
	if content == "" {
		if len(msg.Photo) > 0 {
			content = "[图片]"
		} else if msg.Document != nil {
			content = "[文件] " + msg.Document.FileName
		} else {
			content = "[消息]"
		}
	}
	return fmt.Sprintf("广播消息收到群内回复\n群组：%s\n回复人：%s\n内容：\n%s", group, user.DisplayName, content)
}

func replyLinkButtons(msg telegram.Message, delivery storage.BroadcastDelivery) []telegram.InlineKeyboardButton {
	var buttons []telegram.InlineKeyboardButton
	if url := telegramMessageURL(msg.Chat, msg.MessageID); url != "" {
		buttons = append(buttons, telegram.InlineKeyboardButton{Text: "定位回复消息", URL: url})
	}
	if url := telegramMessageURL(msg.Chat, delivery.TargetMessageID); url != "" {
		buttons = append(buttons, telegram.InlineKeyboardButton{Text: "定位原投递消息", URL: url})
	}
	if len(buttons) == 0 {
		buttons = append(buttons, telegram.InlineKeyboardButton{Text: "无法生成定位链接", CallbackData: "br:none"})
	}
	return buttons
}

func telegramMessageURL(chat telegram.Chat, messageID int64) string {
	if chat.Username != "" {
		return fmt.Sprintf("https://t.me/%s/%d", chat.Username, messageID)
	}
	raw := strconv.FormatInt(chat.ID, 10)
	if strings.HasPrefix(raw, "-100") {
		return fmt.Sprintf("https://t.me/c/%s/%d", strings.TrimPrefix(raw, "-100"), messageID)
	}
	return ""
}
