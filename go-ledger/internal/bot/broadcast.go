package bot

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/telegram"
)

type privateState struct {
	Mode                 string
	TargetName           string
	ChatIDs              []int64
	NotifyAll            bool
	QuickReplyTargetChat int64
	QuickReplyMessageID  int64
	CreatedAt            time.Time
}

func isBroadcastMenuText(text string) bool {
	switch strings.TrimSpace(text) {
	case "📡群发广播", "群发广播", "📣分组广播", "分组广播", "🗂群列表", "群列表", "单群发送":
		return true
	default:
		return false
	}
}

func (b *Bot) sendBroadcastMenu(ctx context.Context, msg telegram.Message, user storage.User, text string) error {
	if !b.canUseBroadcast(ctx, user.ID) {
		_, err := b.tg.SendMessage(ctx, msg.Chat.ID, "没有广播权限。", map[string]any{"reply_to_message_id": msg.MessageID})
		return err
	}
	switch strings.TrimSpace(text) {
	case "📣分组广播", "分组广播":
		return b.sendBroadcastGroups(ctx, msg.Chat.ID, user.ID, msg.MessageID)
	case "🗂群列表", "群列表", "单群发送":
		return b.sendBroadcastChats(ctx, msg.Chat.ID, user.ID, msg.MessageID)
	default:
		keyboard := telegram.InlineKeyboardMarkup{InlineKeyboard: [][]telegram.InlineKeyboardButton{
			{{Text: "群发到全部授权群", CallbackData: "bc:all"}},
			{{Text: "选择分组广播", CallbackData: "bc:groups"}, {Text: "选择单群发送", CallbackData: "bc:chats"}},
			{{Text: "取消", CallbackData: "bc:cancel"}},
		}}
		_, err := b.tg.SendMessage(ctx, msg.Chat.ID, "请选择广播目标。选定后，直接发送文字、图片、图片+文字或文件即可广播；可连续发送，发“返回”结束。", map[string]any{
			"reply_to_message_id": msg.MessageID,
			"reply_markup":        keyboard,
		})
		return err
	}
}

func (b *Bot) handleBroadcastCallback(ctx context.Context, cb telegram.CallbackQuery) error {
	if !b.canUseBroadcast(ctx, cb.From.ID) {
		return b.tg.AnswerCallback(ctx, cb.ID, "没有广播权限。")
	}
	if cb.Message == nil {
		return b.tg.AnswerCallback(ctx, cb.ID, "")
	}
	chatID := cb.Message.Chat.ID
	replyTo := cb.Message.MessageID
	switch {
	case cb.Data == "bc:cancel":
		b.privateStates.Delete(formatID(cb.From.ID))
		if err := b.tg.AnswerCallback(ctx, cb.ID, "已取消"); err != nil {
			return err
		}
		_, err := b.tg.SendMessage(ctx, chatID, "已取消广播。", map[string]any{"reply_to_message_id": replyTo})
		return err
	case cb.Data == "bc:all":
		targets, err := b.store.ListAllowedBroadcastChats(ctx, cb.From.ID, b.isRoot(cb.From.ID))
		if err != nil {
			return err
		}
		return b.setBroadcastTargets(ctx, cb, "all", "全部授权群", groupsToIDs(targets))
	case cb.Data == "bc:notify":
		return b.toggleBroadcastNotifyAll(ctx, cb)
	case cb.Data == "bc:groups":
		if err := b.tg.AnswerCallback(ctx, cb.ID, "请选择分组"); err != nil {
			return err
		}
		return b.sendBroadcastGroups(ctx, chatID, cb.From.ID, replyTo)
	case cb.Data == "bc:chats":
		if err := b.tg.AnswerCallback(ctx, cb.ID, "请选择群"); err != nil {
			return err
		}
		return b.sendBroadcastChats(ctx, chatID, cb.From.ID, replyTo)
	case strings.HasPrefix(cb.Data, "bc:g:"):
		name, err := url.QueryUnescape(strings.TrimPrefix(cb.Data, "bc:g:"))
		if err != nil {
			return b.tg.AnswerCallback(ctx, cb.ID, "分组无效")
		}
		targets, err := b.allowedChatsForBroadcastGroup(ctx, cb.From.ID, name)
		if err != nil {
			return err
		}
		return b.setBroadcastTargets(ctx, cb, "group", name, groupsToIDs(targets))
	case strings.HasPrefix(cb.Data, "bc:c:"):
		raw := strings.TrimPrefix(cb.Data, "bc:c:")
		targetChatID, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return b.tg.AnswerCallback(ctx, cb.ID, "群无效")
		}
		targets, err := b.store.ListAllowedBroadcastChats(ctx, cb.From.ID, b.isRoot(cb.From.ID))
		if err != nil {
			return err
		}
		for _, target := range targets {
			if target.ChatID == targetChatID {
				return b.setBroadcastTargets(ctx, cb, "chat", target.Title, []int64{target.ChatID})
			}
		}
		return b.tg.AnswerCallback(ctx, cb.ID, "没有这个群的广播权限")
	default:
		return b.tg.AnswerCallback(ctx, cb.ID, "")
	}
}

func (b *Bot) sendBroadcastGroups(ctx context.Context, privateChatID, userID, replyTo int64) error {
	groups, err := b.store.ListBroadcastGroups(ctx)
	if err != nil {
		return err
	}
	var rows [][]telegram.InlineKeyboardButton
	for _, group := range groups {
		if !b.isRoot(userID) && !b.broadcastGroupAllowed(ctx, userID, group.Name) {
			continue
		}
		label := fmt.Sprintf("%s（%d个群）", group.Name, len(group.ChatIDs))
		data := "bc:g:" + url.QueryEscape(group.Name)
		if len(data) > 64 {
			label = group.Name
			data = "bc:g:" + truncateCallback(group.Name, 59)
		}
		rows = append(rows, []telegram.InlineKeyboardButton{{Text: label, CallbackData: data}})
	}
	rows = append(rows, []telegram.InlineKeyboardButton{{Text: "返回", CallbackData: "bc:cancel"}})
	text := "请选择分组广播目标。"
	if len(rows) == 1 {
		text = "暂无可用广播分组，请先在后台创建分组并添加群。"
	}
	_, err = b.tg.SendMessage(ctx, privateChatID, text, map[string]any{
		"reply_to_message_id": replyTo,
		"reply_markup":        telegram.InlineKeyboardMarkup{InlineKeyboard: rows},
	})
	return err
}

func (b *Bot) sendBroadcastChats(ctx context.Context, privateChatID, userID, replyTo int64) error {
	targets, err := b.store.ListAllowedBroadcastChats(ctx, userID, b.isRoot(userID))
	if err != nil {
		return err
	}
	var rows [][]telegram.InlineKeyboardButton
	for i, target := range targets {
		if i >= 40 {
			break
		}
		label := target.Title
		if label == "" {
			label = formatID(target.ChatID)
		}
		rows = append(rows, []telegram.InlineKeyboardButton{{Text: label, CallbackData: "bc:c:" + formatID(target.ChatID)}})
	}
	rows = append(rows, []telegram.InlineKeyboardButton{{Text: "返回", CallbackData: "bc:cancel"}})
	text := "请选择单群发送目标。"
	if len(rows) == 1 {
		text = "暂无可用群。机器人进群或群内有人发言后，会自动保存群组。"
	}
	_, err = b.tg.SendMessage(ctx, privateChatID, text, map[string]any{
		"reply_to_message_id": replyTo,
		"reply_markup":        telegram.InlineKeyboardMarkup{InlineKeyboard: rows},
	})
	return err
}

func (b *Bot) setBroadcastTargets(ctx context.Context, cb telegram.CallbackQuery, mode, name string, chatIDs []int64) error {
	if len(chatIDs) == 0 {
		return b.tg.AnswerCallback(ctx, cb.ID, "没有可发送的目标群")
	}
	b.privateStates.Set(formatID(cb.From.ID), privateState{
		Mode:       mode,
		TargetName: name,
		ChatIDs:    chatIDs,
		CreatedAt:  time.Now().In(b.loc),
	})
	if err := b.tg.AnswerCallback(ctx, cb.ID, "已选择"); err != nil {
		return err
	}
	_, err := b.tg.SendMessage(ctx, cb.Message.Chat.ID, formatBroadcastReadyText(name, len(chatIDs), false), map[string]any{
		"reply_to_message_id": cb.Message.MessageID,
		"reply_markup":        broadcastReadyKeyboard(false),
	})
	return err
}

func (b *Bot) toggleBroadcastNotifyAll(ctx context.Context, cb telegram.CallbackQuery) error {
	state, ok := b.privateStates.Get(formatID(cb.From.ID))
	if !ok || len(state.ChatIDs) == 0 || state.Mode == "quick_reply" {
		return b.tg.AnswerCallback(ctx, cb.ID, "请先选择广播目标")
	}
	state.NotifyAll = !state.NotifyAll
	b.privateStates.Set(formatID(cb.From.ID), state)
	if err := b.tg.AnswerCallback(ctx, cb.ID, "通知所有人已切换"); err != nil {
		return err
	}
	if cb.Message == nil {
		return nil
	}
	_, err := b.tg.SendMessage(ctx, cb.Message.Chat.ID, formatBroadcastReadyText(state.TargetName, len(state.ChatIDs), state.NotifyAll), map[string]any{
		"reply_to_message_id": cb.Message.MessageID,
		"reply_markup":        broadcastReadyKeyboard(state.NotifyAll),
	})
	return err
}

func (b *Bot) handleBroadcastMaterial(ctx context.Context, msg telegram.Message, user storage.User, state privateState, now time.Time) error {
	if len(state.ChatIDs) == 0 {
		b.privateStates.Delete(formatID(user.ID))
		_, err := b.tg.SendMessage(ctx, msg.Chat.ID, "广播目标已失效，请重新选择。", map[string]any{"reply_to_message_id": msg.MessageID})
		return err
	}
	if msg.Text == "菜单" || msg.Text == "/start" {
		b.privateStates.Delete(formatID(user.ID))
		return b.sendPrivateMenu(ctx, msg.Chat.ID, msg.MessageID)
	}
	targets := append([]int64(nil), state.ChatIDs...)
	sourceChatID := msg.Chat.ID
	sourceMessageID := msg.MessageID
	targetName := state.TargetName
	mode := state.Mode
	notifyAll := state.NotifyAll
	operatorID := user.ID
	b.broadcastPool.Submit(func(jobCtx context.Context) {
		success, failed := b.copyBroadcast(jobCtx, operatorID, sourceChatID, sourceMessageID, targets, mode, targetName, notifyAll)
		text := fmt.Sprintf("广播完成：成功 %d 个，失败 %d 个。\n目标：%s", success, failed, targetName)
		if notifyAll {
			text += "\n通知所有人：开启"
		}
		if _, err := b.tg.SendMessage(jobCtx, sourceChatID, text, map[string]any{"reply_to_message_id": sourceMessageID}); err != nil {
			log.Printf("send broadcast result: %v", err)
		}
	})
	_, err := b.tg.SendMessage(ctx, msg.Chat.ID, fmt.Sprintf("广播已提交：%s，目标 %d 个。可继续发送下一条。", targetName, len(targets)), map[string]any{"reply_to_message_id": msg.MessageID})
	return err
}

func (b *Bot) copyBroadcast(ctx context.Context, operatorID, fromChatID, messageID int64, targetChatIDs []int64, mode, targetName string, notifyAll bool) (int, int) {
	success := 0
	failed := 0
	now := time.Now().In(b.loc)
	for _, targetChatID := range targetChatIDs {
		targetMsg, err := b.tg.CopyMessage(ctx, targetChatID, fromChatID, messageID, nil)
		if err != nil {
			failed++
			log.Printf("copy broadcast to %d: %v", targetChatID, err)
		} else {
			success++
			title := ""
			if targetMsg.Chat.Title != "" {
				title = targetMsg.Chat.Title
			}
			if _, err := b.store.InsertBroadcastDelivery(ctx, storage.BroadcastDelivery{
				OperatorUserID:  operatorID,
				SourceChatID:    fromChatID,
				SourceMessageID: messageID,
				TargetChatID:    targetChatID,
				TargetTitle:     title,
				TargetMessageID: targetMsg.MessageID,
				Mode:            mode,
				TargetName:      targetName,
				CreatedAt:       now,
			}); err != nil {
				log.Printf("record broadcast delivery: %v", err)
			}
			if notifyAll {
				b.notifyAllInChatAsync(ctx, targetChatID, targetMsg.MessageID)
			}
		}
		select {
		case <-ctx.Done():
			return success, failed + len(targetChatIDs) - success - failed
		case <-time.After(60 * time.Millisecond):
		}
	}
	return success, failed
}

func formatBroadcastReadyText(name string, count int, notifyAll bool) string {
	status := "关闭"
	if notifyAll {
		status = "开启"
	}
	return fmt.Sprintf("已选择：%s\n目标群：%d个\n通知所有人：%s\n\n请直接发送广播内容；文字、图片、图片+文字、文件都会原样复制。可连续发送，发“返回”结束。", name, count, status)
}

func broadcastReadyKeyboard(notifyAll bool) telegram.InlineKeyboardMarkup {
	label := "通知所有人：关闭"
	if notifyAll {
		label = "通知所有人：开启"
	}
	return telegram.InlineKeyboardMarkup{InlineKeyboard: [][]telegram.InlineKeyboardButton{
		{{Text: label, CallbackData: "bc:notify"}},
		{{Text: "取消广播", CallbackData: "bc:cancel"}},
	}}
}

func (b *Bot) canUseBroadcast(ctx context.Context, userID int64) bool {
	if b.isRoot(userID) {
		return true
	}
	key := "broadcast:" + formatID(userID)
	if value, ok := b.operatorCache.Get(key); ok {
		return value
	}
	value, err := b.store.IsBroadcastOperator(ctx, userID)
	if err != nil {
		log.Printf("check broadcast operator %d: %v", userID, err)
		return false
	}
	b.operatorCache.Set(key, value)
	return value
}

func (b *Bot) allowedChatsForBroadcastGroup(ctx context.Context, userID int64, name string) ([]storage.Group, error) {
	groupChats, err := b.store.ListBroadcastGroupChats(ctx, name)
	if err != nil {
		return nil, err
	}
	if b.isRoot(userID) {
		return groupChats, nil
	}
	allowed, err := b.store.ListAllowedBroadcastChats(ctx, userID, false)
	if err != nil {
		return nil, err
	}
	allowedMap := make(map[int64]struct{}, len(allowed))
	for _, group := range allowed {
		allowedMap[group.ChatID] = struct{}{}
	}
	filtered := make([]storage.Group, 0, len(groupChats))
	for _, group := range groupChats {
		if _, ok := allowedMap[group.ChatID]; ok {
			filtered = append(filtered, group)
		}
	}
	return filtered, nil
}

func (b *Bot) broadcastGroupAllowed(ctx context.Context, userID int64, name string) bool {
	targets, err := b.allowedChatsForBroadcastGroup(ctx, userID, name)
	if err != nil {
		log.Printf("check broadcast group allowed %d %s: %v", userID, name, err)
		return false
	}
	return len(targets) > 0
}

func groupsToIDs(groups []storage.Group) []int64 {
	ids := make([]int64, 0, len(groups))
	for _, group := range groups {
		ids = append(ids, group.ChatID)
	}
	return ids
}

func truncateCallback(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[:max]
}
