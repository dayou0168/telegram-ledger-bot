package bot

import (
	"context"
	"errors"
	"fmt"
	"html"
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
	job := func(jobCtx context.Context) {
		delivery, ok, err := b.store.FindBroadcastDeliveryByTarget(jobCtx, chatID, replyMessageID)
		if err != nil {
			log.Printf("find broadcast delivery: %v", err)
			return
		}
		if !ok || delivery.OperatorUserID == 0 {
			return
		}
		delivery = b.tryReplaceBroadcastDelivery(jobCtx, delivery, *msg.ReplyTo)
		broadcastSender := b.broadcastDeliverySenderLabel(jobCtx, delivery)
		payload, payloadOK := broadcastReplyPayload(msg, user, delivery, broadcastSender)
		if !payloadOK {
			return
		}
		for recipient := range b.broadcastReplyRecipients(jobCtx, delivery.OperatorUserID) {
			keyboard := broadcastReplyKeyboard(msg, delivery, b.canQuickReplyDelivery(jobCtx, recipient, delivery))
			var replyToUpstreamID int64
			if upstream, upstreamOK, upstreamErr := b.store.GetBroadcastUpstreamMessage(jobCtx, delivery.SourceChatID, delivery.SourceMessageID, recipient); upstreamErr != nil {
				log.Printf("load broadcast upstream reply target: %v", upstreamErr)
			} else if upstreamOK {
				replyToUpstreamID = upstream.ID
			}
			if err := b.enqueueReliablePayload(jobCtx, sendPriorityNormal, "broadcast_reply_notice", fmt.Sprintf("broadcast_reply_notice:%d:%d:%d", recipient, chatID, msg.MessageID), recipient, payload, map[string]any{
				"parse_mode":   "HTML",
				"reply_markup": keyboard,
			}, reliableMessageRef{}, replyToUpstreamID, time.Now().In(b.loc)); err != nil {
				log.Printf("enqueue broadcast reply notice: %v", err)
			}
		}
	}
	if !b.notifyPool.Submit(job) {
		job(ctx)
	}
}

func (b *Bot) handleBroadcastReplyCallback(ctx context.Context, cb telegram.CallbackQuery) error {
	ctx = withPrivateCleanupCategory(ctx, "quick_reply")
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
	if !ok || !b.canQuickReplyDelivery(ctx, cb.From.ID, delivery) {
		return b.tg.AnswerCallback(ctx, cb.ID, "当前无快速回复权限")
	}
	quickState := privateState{
		Mode:                 "quick_reply",
		TargetName:           delivery.TargetTitle,
		QuickReplyTargetChat: delivery.TargetChatID,
		QuickReplyMessageID:  delivery.TargetMessageID,
		CreatedAt:            time.Now().In(b.loc),
	}
	if current, ok := b.privateStates.Get(formatID(cb.From.ID)); ok && current.Mode != "quick_reply" && isBroadcastTargetState(current) {
		quickState.ReturnMode = current.Mode
		quickState.ReturnTargetName = current.TargetName
		quickState.ReturnTargetChatID = current.TargetChatID
		quickState.ReturnGroupID = current.GroupID
		quickState.ReturnNotifyAll = current.NotifyAll
		quickState.ReturnControlMessageID = current.ControlMessageID
	}
	b.privateStates.Set(formatID(cb.From.ID), quickState)
	if err := b.tg.AnswerCallback(ctx, cb.ID, "请发送回复内容"); err != nil {
		return err
	}
	if cb.Message == nil {
		return nil
	}
	return b.enqueueReplyText(ctx, sendPriorityNormal, "quick_reply_prompt", cb.Message.Chat.ID, cb.Message.MessageID, "快速回复已开启。请直接发送要回复的内容；文字、图片或文件都会复制到目标群。底部按钮可结束快速回复。", map[string]any{
		"reply_markup": quickReplyKeyboard(delivery.TargetTitle, quickState.ReturnMode != ""),
	}, time.Now().In(b.loc))
}

func (b *Bot) handleQuickReplyMaterial(ctx context.Context, msg telegram.Message, user storage.User, state privateState) error {
	ctx = withPrivateCleanupCategory(ctx, "quick_reply")
	if isQuickReplyStatusText(msg.Text) {
		b.deleteMessageBestEffort(ctx, msg.Chat.ID, msg.MessageID)
		return nil
	}
	if isQuickReplyEndText(msg.Text) || msg.Text == "菜单" || msg.Text == "/start" {
		return b.exitQuickReply(ctx, msg, user, state)
	}
	_, allowed, err := b.quickReplyDeliveryFresh(ctx, user.ID, state)
	if err != nil {
		return err
	}
	if !allowed {
		b.privateStates.Delete(formatID(user.ID))
		return b.enqueueReplyText(ctx, sendPriorityNormal, "quick_reply_lost", msg.Chat.ID, msg.MessageID, "快速回复目标已失效，请重新点回复通知。", nil, time.Now().In(b.loc))
	}
	updateID, _ := ctx.Value(telegramUpdateIDContextKey{}).(int64)
	commitSignal := telegramUpdateCommitSignalFromContext(ctx)
	if updateID <= 0 {
		return errors.New("quick reply update id is unavailable")
	}
	return commitSignal.StageQuickReply(storage.QuickReplyOutboxInsert{
		DedupeKey:       fmt.Sprintf("quick_reply:%s:%d", b.telegramInboxStreamKey(), updateID),
		ActorUserID:     user.ID,
		SourceChatID:    msg.Chat.ID,
		SourceMessageID: msg.MessageID,
		TargetChatID:    state.QuickReplyTargetChat,
		TargetMessageID: state.QuickReplyMessageID,
	})
}

func (b *Bot) quickReplyDeliveryFresh(ctx context.Context, userID int64, state privateState) (storage.BroadcastDelivery, bool, error) {
	if state.QuickReplyTargetChat == 0 || state.QuickReplyMessageID == 0 {
		return storage.BroadcastDelivery{}, false, nil
	}
	delivery, ok, err := b.store.FindBroadcastDeliveryByTarget(ctx, state.QuickReplyTargetChat, state.QuickReplyMessageID)
	if err != nil || !ok {
		return storage.BroadcastDelivery{}, false, err
	}
	allowed, err := b.canQuickReplyDeliveryFresh(ctx, userID, delivery)
	return delivery, allowed, err
}

func (b *Bot) canQuickReplyDeliveryFresh(ctx context.Context, userID int64, delivery storage.BroadcastDelivery) (bool, error) {
	if userID <= 0 || delivery.OperatorUserID <= 0 {
		return false, nil
	}
	recipients, err := b.resolveBroadcastReplyRecipients(ctx, delivery.OperatorUserID)
	if err != nil {
		return false, err
	}
	_, allowed := recipients[userID]
	return allowed, nil
}

func (b *Bot) canQuickReplyDelivery(ctx context.Context, userID int64, delivery storage.BroadcastDelivery) bool {
	allowed, err := b.canQuickReplyDeliveryFresh(ctx, userID, delivery)
	if err != nil {
		log.Printf("check quick reply permission %d: %v", userID, err)
		return false
	}
	return allowed
}

func broadcastReplyKeyboard(msg telegram.Message, delivery storage.BroadcastDelivery, quickReply bool) telegram.InlineKeyboardMarkup {
	rows := make([][]telegram.InlineKeyboardButton, 0, 2)
	if quickReply {
		rows = append(rows, []telegram.InlineKeyboardButton{{Text: "快速回复", CallbackData: "br:q:" + formatID(delivery.ID)}})
	}
	if delivery.Mode != "chat" {
		rows = append(rows, replyLinkButtons(msg, delivery))
	}
	return telegram.InlineKeyboardMarkup{InlineKeyboard: rows}
}

func (b *Bot) exitQuickReply(ctx context.Context, msg telegram.Message, user storage.User, state privateState) error {
	ctx = withPrivateCleanupCategory(ctx, "quick_reply")
	b.deleteMessageBestEffort(ctx, msg.Chat.ID, msg.MessageID)
	if restored, ok, err := b.loadBroadcastTargetState(ctx, user.ID); err != nil {
		return err
	} else if ok {
		b.privateStates.Set(formatID(user.ID), restored)
		targets, err := b.currentBroadcastTargets(ctx, user.ID, restored)
		if err != nil {
			return err
		}
		_, err = b.sendText(ctx, sendPriorityNormal, msg.Chat.ID, formatBroadcastReadyText(restored.TargetName, len(targets), restored.NotifyAll), map[string]any{
			"reply_markup": broadcastSessionKeyboard(restored.TargetName, restored.NotifyAll),
		})
		return err
	}
	b.privateStates.Delete(formatID(user.ID))
	return b.sendPrivateMenu(ctx, msg.Chat.ID, msg.MessageID)
}

func quickReplyReturnState(state privateState) (privateState, bool) {
	if state.ReturnMode == "" {
		return privateState{}, false
	}
	return privateState{
		Mode:             state.ReturnMode,
		TargetName:       state.ReturnTargetName,
		TargetChatID:     state.ReturnTargetChatID,
		GroupID:          state.ReturnGroupID,
		ChatIDs:          append([]int64(nil), state.ReturnChatIDs...),
		NotifyAll:        state.ReturnNotifyAll,
		ControlMessageID: state.ReturnControlMessageID,
		CreatedAt:        time.Now(),
	}, true
}

func quickReplyKeyboard(targetName string, canReturnBroadcast bool) telegram.ReplyKeyboardMarkup {
	targetLabel := "当前快速回复：" + strings.TrimSpace(targetName)
	if strings.TrimSpace(targetName) == "" {
		targetLabel = "当前快速回复：未命名群"
	}
	second := []telegram.KeyboardButton{{Text: "结束快速回复"}, {Text: "取消"}}
	if canReturnBroadcast {
		second = []telegram.KeyboardButton{{Text: "结束快速回复"}, {Text: "返回广播"}, {Text: "取消"}}
	}
	return telegram.ReplyKeyboardMarkup{
		Keyboard: [][]telegram.KeyboardButton{
			{{Text: targetLabel}},
			second,
		},
		IsPersistent:   true,
		ResizeKeyboard: true,
	}
}

func isQuickReplyStatusText(text string) bool {
	return strings.HasPrefix(strings.TrimSpace(text), "当前快速回复：")
}

func isQuickReplyEndText(text string) bool {
	switch strings.TrimSpace(text) {
	case "结束快速回复", "取消快速回复", "退出快速回复", "返回广播", "取消", "返回":
		return true
	default:
		return false
	}
}

func (b *Bot) findBroadcastDeliveryByID(ctx context.Context, id int64) (storage.BroadcastDelivery, bool, error) {
	return b.store.GetBroadcastDelivery(ctx, id)
}

func formatBroadcastReplyNotice(msg telegram.Message, user storage.User, delivery storage.BroadcastDelivery, broadcastSender string) string {
	return formatBroadcastReplyNoticeLimit(msg, user, delivery, broadcastSender, 1200)
}

func formatBroadcastReplyNoticeLimit(msg telegram.Message, user storage.User, delivery storage.BroadcastDelivery, broadcastSender string, contentLimit int) string {
	group := delivery.TargetTitle
	if group == "" {
		group = msg.Chat.Title
	}
	if group == "" {
		group = "未命名群"
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
	groupLabel := html.EscapeString(group)
	if url := telegramMessageURL(msg.Chat, msg.MessageID); url != "" {
		groupLabel = `<a href="` + html.EscapeString(url) + `">` + groupLabel + `</a>`
	}
	sender := html.EscapeString(user.DisplayName)
	if sender == "" {
		sender = "未命名成员"
	}
	sender = `<a href="tg://user?id=` + formatID(user.ID) + `">` + sender + `</a>`
	broadcastSender = html.EscapeString(trimSingleLine(broadcastSender, 80))
	if broadcastSender == "" {
		broadcastSender = "未命名操作人"
	}
	return fmt.Sprintf("来源群：%s\n回复人：%s\n原广播发送人：%s\n\n回复内容：\n\n%s", groupLabel, sender, broadcastSender, html.EscapeString(trimRunes(content, contentLimit)))
}

func broadcastReplyPayload(msg telegram.Message, user storage.User, delivery storage.BroadcastDelivery, broadcastSender string) (reliablePayload, bool) {
	if len(msg.Photo) > 0 {
		fileID := strings.TrimSpace(msg.Photo[len(msg.Photo)-1].FileID)
		if fileID == "" {
			return reliablePayload{}, false
		}
		return reliablePayload{Type: "photo", FileID: fileID, Caption: formatBroadcastReplyNoticeLimit(msg, user, delivery, broadcastSender, 650)}, true
	}
	return reliablePayload{Type: "text", Text: formatBroadcastReplyNotice(msg, user, delivery, broadcastSender)}, true
}

func (b *Bot) broadcastDeliverySenderLabel(ctx context.Context, delivery storage.BroadcastDelivery) string {
	if source, ok, err := b.store.GetUser(ctx, delivery.SourceChatID, delivery.OperatorUserID); err == nil && ok {
		return b.broadcastSenderLabel(ctx, source)
	}
	return b.broadcastSenderLabel(ctx, storage.User{ID: delivery.OperatorUserID})
}

func (b *Bot) broadcastReplyRecipients(ctx context.Context, operatorID int64) map[int64]struct{} {
	recipients, err := b.resolveBroadcastReplyRecipients(ctx, operatorID)
	if err != nil {
		log.Printf("resolve broadcast reply recipients for source %d: %v", operatorID, err)
		return map[int64]struct{}{operatorID: {}}
	}
	return recipients
}

func (b *Bot) resolveBroadcastReplyRecipients(ctx context.Context, operatorID int64) (map[int64]struct{}, error) {
	recipients := map[int64]struct{}{}
	senderActive, err := b.broadcastReplySenderActive(ctx, operatorID)
	if err != nil {
		return nil, err
	}
	if senderActive {
		recipients[operatorID] = struct{}{}
	}
	ceilings, err := b.broadcastRecipientCeilings(ctx, operatorID)
	if err != nil {
		return nil, err
	}
	overrides, err := b.store.BroadcastMessagePreferenceOverridesForSource(ctx, operatorID, ceilings.Reply)
	if err != nil {
		return nil, err
	}
	for _, observerID := range ceilings.Reply {
		if preference, exists := overrides[observerID]; !exists || preference.ReceiveReply {
			recipients[observerID] = struct{}{}
		}
	}
	return recipients, nil
}

func (b *Bot) broadcastReplySenderActive(ctx context.Context, operatorID int64) (bool, error) {
	if operatorID <= 0 {
		return false, nil
	}
	if b.perms.IsPrivileged(operatorID) {
		return true, nil
	}
	operator, ok, err := b.store.GetGlobalOperator(ctx, operatorID)
	if err != nil {
		return false, err
	}
	return ok && operator.Status == "active", nil
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
