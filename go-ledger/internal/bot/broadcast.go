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
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/worker"
)

type privateState struct {
	Mode                   string
	TargetName             string
	ChatIDs                []int64
	NotifyAll              bool
	ControlMessageID       int64
	WatchAddress           string
	QuickReplyTargetChat   int64
	QuickReplyMessageID    int64
	ReturnMode             string
	ReturnTargetName       string
	ReturnChatIDs          []int64
	ReturnNotifyAll        bool
	ReturnControlMessageID int64
	CreatedAt              time.Time
}

const broadcastPickerPageSize = 12

type broadcastGroupOption struct {
	Name    string
	ChatIDs []int64
}

func isBroadcastMenuText(text string) bool {
	switch strings.TrimSpace(text) {
	case "📡群发广播", "群发广播", "📣分组广播", "分组广播", "🗂群列表", "群列表", "单群发送", "切换群", "切换目标":
		return true
	default:
		return false
	}
}

func (b *Bot) sendBroadcastMenu(ctx context.Context, msg telegram.Message, user storage.User, text string) error {
	if !b.canUseBroadcast(ctx, user.ID) {
		return b.enqueueReplyText(ctx, sendPriorityNormal, "broadcast_denied", msg.Chat.ID, msg.MessageID, "没有广播权限。", nil, time.Now().In(b.loc))
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
		return b.enqueueReliableText(ctx, sendPriorityNormal, "broadcast_menu", messageScopedDedupe("broadcast_menu", msg.Chat.ID, msg.MessageID), msg.Chat.ID, "请选择广播目标。选定后，直接发送文字、图片、图片+文字或文件即可广播；可连续发送，发“返回”结束。", map[string]any{
			"reply_to_message_id": msg.MessageID,
			"reply_markup":        keyboard,
		}, reliableMessageRef{}, time.Now().In(b.loc))
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
		return b.enqueueReplyText(ctx, sendPriorityNormal, "broadcast_cancel", chatID, replyTo, "已取消广播。", nil, time.Now().In(b.loc))
	case cb.Data == "bc:all":
		targets, err := b.store.ListAllowedBroadcastChats(ctx, cb.From.ID, b.perms.HasGlobalBroadcastAccess(cb.From.ID))
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
	case strings.HasPrefix(cb.Data, "bc:gp:"):
		page, _ := strconv.Atoi(strings.TrimPrefix(cb.Data, "bc:gp:"))
		return b.editBroadcastGroupsPage(ctx, cb, page)
	case strings.HasPrefix(cb.Data, "bc:gi:"):
		parts := strings.Split(strings.TrimPrefix(cb.Data, "bc:gi:"), ":")
		if len(parts) != 2 {
			return b.tg.AnswerCallback(ctx, cb.ID, "分组无效")
		}
		page, pageErr := strconv.Atoi(parts[0])
		index, indexErr := strconv.Atoi(parts[1])
		options, err := b.allowedBroadcastGroupOptions(ctx, cb.From.ID)
		if err != nil {
			return err
		}
		absolute := page*broadcastPickerPageSize + index
		if pageErr != nil || indexErr != nil || absolute < 0 || absolute >= len(options) {
			return b.tg.AnswerCallback(ctx, cb.ID, "分组已变化，请重新选择")
		}
		option := options[absolute]
		return b.setBroadcastTargets(ctx, cb, "group", option.Name, option.ChatIDs)
	case cb.Data == "bc:chats":
		if err := b.tg.AnswerCallback(ctx, cb.ID, "请选择群"); err != nil {
			return err
		}
		return b.sendBroadcastChats(ctx, chatID, cb.From.ID, replyTo)
	case strings.HasPrefix(cb.Data, "bc:cp:"):
		page, _ := strconv.Atoi(strings.TrimPrefix(cb.Data, "bc:cp:"))
		return b.editBroadcastChatsPage(ctx, cb, page)
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
		targets, err := b.store.ListAllowedBroadcastChats(ctx, cb.From.ID, b.perms.HasGlobalBroadcastAccess(cb.From.ID))
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
	options, err := b.allowedBroadcastGroupOptions(ctx, userID)
	if err != nil {
		return err
	}
	text, keyboard := renderBroadcastGroupPage(options, 0)
	return b.enqueueReliableText(ctx, sendPriorityNormal, "broadcast_groups", messageScopedDedupe("broadcast_groups", privateChatID, replyTo), privateChatID, text, map[string]any{
		"reply_to_message_id": replyTo,
		"reply_markup":        keyboard,
	}, reliableMessageRef{}, time.Now().In(b.loc))
}

func (b *Bot) sendBroadcastChats(ctx context.Context, privateChatID, userID, replyTo int64) error {
	targets, err := b.store.ListAllowedBroadcastChats(ctx, userID, b.perms.HasGlobalBroadcastAccess(userID))
	if err != nil {
		return err
	}
	text, keyboard := renderBroadcastChatPage(targets, 0)
	return b.enqueueReliableText(ctx, sendPriorityNormal, "broadcast_chats", messageScopedDedupe("broadcast_chats", privateChatID, replyTo), privateChatID, text, map[string]any{
		"reply_to_message_id": replyTo,
		"reply_markup":        keyboard,
	}, reliableMessageRef{}, time.Now().In(b.loc))
}

func (b *Bot) allowedBroadcastGroupOptions(ctx context.Context, userID int64) ([]broadcastGroupOption, error) {
	all := b.perms.HasGlobalBroadcastAccess(userID)
	var groups []storage.BroadcastGroup
	var err error
	if all {
		groups, err = b.store.ListBroadcastGroups(ctx)
	} else {
		groups, err = b.store.ListVisibleBroadcastGroups(ctx, userID)
	}
	if err != nil {
		return nil, err
	}
	allowed, err := b.store.ListAllowedBroadcastChats(ctx, userID, all)
	if err != nil {
		return nil, err
	}
	allowedSet := make(map[int64]struct{}, len(allowed))
	for _, group := range allowed {
		allowedSet[group.ChatID] = struct{}{}
	}
	options := make([]broadcastGroupOption, 0, len(groups))
	for _, group := range groups {
		chatIDs := make([]int64, 0, len(group.ChatIDs))
		for _, chatID := range group.ChatIDs {
			if _, ok := allowedSet[chatID]; ok {
				chatIDs = append(chatIDs, chatID)
			}
		}
		if len(chatIDs) > 0 {
			options = append(options, broadcastGroupOption{Name: group.Name, ChatIDs: chatIDs})
		}
	}
	return options, nil
}

func renderBroadcastGroupPage(options []broadcastGroupOption, page int) (string, telegram.InlineKeyboardMarkup) {
	page, start, end, pages := pickerBounds(len(options), page)
	rows := make([][]telegram.InlineKeyboardButton, 0, end-start+2)
	for index, option := range options[start:end] {
		rows = append(rows, []telegram.InlineKeyboardButton{{Text: fmt.Sprintf("%s（%d个群）", option.Name, len(option.ChatIDs)), CallbackData: fmt.Sprintf("bc:gi:%d:%d", page, index)}})
	}
	rows = appendPickerNavigation(rows, "bc:gp:", page, pages)
	rows = append(rows, []telegram.InlineKeyboardButton{{Text: "返回", CallbackData: "bc:cancel"}})
	if len(options) == 0 {
		return "暂无可用广播分组，请先在后台创建分组并添加群。", telegram.InlineKeyboardMarkup{InlineKeyboard: rows}
	}
	return fmt.Sprintf("请选择分组广播目标（第 %d/%d 页）。", page+1, pages), telegram.InlineKeyboardMarkup{InlineKeyboard: rows}
}

func renderBroadcastChatPage(targets []storage.Group, page int) (string, telegram.InlineKeyboardMarkup) {
	page, start, end, pages := pickerBounds(len(targets), page)
	rows := make([][]telegram.InlineKeyboardButton, 0, end-start+2)
	for _, target := range targets[start:end] {
		label := strings.TrimSpace(target.Title)
		if label == "" {
			label = formatID(target.ChatID)
		}
		rows = append(rows, []telegram.InlineKeyboardButton{{Text: label, CallbackData: "bc:c:" + formatID(target.ChatID)}})
	}
	rows = appendPickerNavigation(rows, "bc:cp:", page, pages)
	rows = append(rows, []telegram.InlineKeyboardButton{{Text: "返回", CallbackData: "bc:cancel"}})
	if len(targets) == 0 {
		return "暂无可用群。机器人进群或群内有人发言后，会自动保存群组。", telegram.InlineKeyboardMarkup{InlineKeyboard: rows}
	}
	return fmt.Sprintf("请选择单群发送目标（第 %d/%d 页）。", page+1, pages), telegram.InlineKeyboardMarkup{InlineKeyboard: rows}
}

func pickerBounds(total, page int) (int, int, int, int) {
	pages := (total + broadcastPickerPageSize - 1) / broadcastPickerPageSize
	if pages < 1 {
		pages = 1
	}
	if page < 0 {
		page = 0
	}
	if page >= pages {
		page = pages - 1
	}
	start := page * broadcastPickerPageSize
	end := start + broadcastPickerPageSize
	if end > total {
		end = total
	}
	return page, start, end, pages
}

func appendPickerNavigation(rows [][]telegram.InlineKeyboardButton, prefix string, page, pages int) [][]telegram.InlineKeyboardButton {
	if pages <= 1 {
		return rows
	}
	var nav []telegram.InlineKeyboardButton
	if page > 0 {
		nav = append(nav, telegram.InlineKeyboardButton{Text: "上一页", CallbackData: prefix + strconv.Itoa(page-1)})
	}
	if page+1 < pages {
		nav = append(nav, telegram.InlineKeyboardButton{Text: "下一页", CallbackData: prefix + strconv.Itoa(page+1)})
	}
	return append(rows, nav)
}

func (b *Bot) editBroadcastGroupsPage(ctx context.Context, cb telegram.CallbackQuery, page int) error {
	options, err := b.allowedBroadcastGroupOptions(ctx, cb.From.ID)
	if err != nil {
		return err
	}
	text, keyboard := renderBroadcastGroupPage(options, page)
	if err := b.tg.AnswerCallback(ctx, cb.ID, ""); err != nil {
		return err
	}
	_, err = b.editText(ctx, cb.Message.Chat.ID, cb.Message.MessageID, text, map[string]any{"reply_markup": keyboard})
	return err
}

func (b *Bot) editBroadcastChatsPage(ctx context.Context, cb telegram.CallbackQuery, page int) error {
	targets, err := b.store.ListAllowedBroadcastChats(ctx, cb.From.ID, b.perms.HasGlobalBroadcastAccess(cb.From.ID))
	if err != nil {
		return err
	}
	text, keyboard := renderBroadcastChatPage(targets, page)
	if err := b.tg.AnswerCallback(ctx, cb.ID, ""); err != nil {
		return err
	}
	_, err = b.editText(ctx, cb.Message.Chat.ID, cb.Message.MessageID, text, map[string]any{"reply_markup": keyboard})
	return err
}

func (b *Bot) setBroadcastTargets(ctx context.Context, cb telegram.CallbackQuery, mode, name string, chatIDs []int64) error {
	ctx = withPrivateCleanupCategory(ctx, "broadcast")
	if len(chatIDs) == 0 {
		return b.tg.AnswerCallback(ctx, cb.ID, "没有可发送的目标群")
	}
	state := privateState{
		Mode:       mode,
		TargetName: name,
		ChatIDs:    chatIDs,
		CreatedAt:  time.Now().In(b.loc),
	}
	if err := b.tg.AnswerCallback(ctx, cb.ID, "已选择"); err != nil {
		return err
	}
	ready, err := b.sendText(ctx, sendPriorityNormal, cb.Message.Chat.ID, formatBroadcastReadyText(name, len(chatIDs), false), map[string]any{
		"reply_to_message_id": cb.Message.MessageID,
		"reply_markup":        broadcastSessionKeyboard(name, false),
	})
	if err == nil {
		state.ControlMessageID = ready.MessageID
		b.privateStates.Set(formatID(cb.From.ID), state)
	}
	return err
}

func (b *Bot) toggleBroadcastNotifyAll(ctx context.Context, cb telegram.CallbackQuery) error {
	ctx = withPrivateCleanupCategory(ctx, "broadcast")
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
	_, err := b.editText(ctx, cb.Message.Chat.ID, cb.Message.MessageID, formatBroadcastReadyText(state.TargetName, len(state.ChatIDs), state.NotifyAll), map[string]any{
		"reply_markup": broadcastReadyKeyboard(state.NotifyAll),
	})
	return err
}

func (b *Bot) handleBroadcastMaterial(ctx context.Context, msg telegram.Message, user storage.User, state privateState, now time.Time) error {
	ctx = withPrivateCleanupCategory(ctx, "broadcast")
	if !b.canUseBroadcast(ctx, user.ID) {
		b.privateStates.Delete(formatID(user.ID))
		return b.enqueueReplyText(ctx, sendPriorityNormal, "broadcast_denied", msg.Chat.ID, msg.MessageID, "没有广播权限。", nil, now)
	}
	if len(state.ChatIDs) == 0 {
		b.privateStates.Delete(formatID(user.ID))
		return b.enqueueReplyText(ctx, sendPriorityNormal, "broadcast_target_lost", msg.Chat.ID, msg.MessageID, "广播目标已失效，请重新选择。", nil, now)
	}
	if msg.Text == "菜单" || msg.Text == "/start" {
		b.privateStates.Delete(formatID(user.ID))
		return b.sendPrivateMenu(ctx, msg.Chat.ID, msg.MessageID)
	}
	if isBroadcastEndText(msg.Text) {
		b.privateStates.Delete(formatID(user.ID))
		return b.sendPrivateMenu(ctx, msg.Chat.ID, msg.MessageID)
	}
	if isBroadcastSwitchTargetText(msg.Text) {
		b.privateStates.Delete(formatID(user.ID))
		removed, removeErr := b.sendText(ctx, sendPriorityNormal, msg.Chat.ID, "请选择新的广播目标。", map[string]any{
			"reply_markup": telegram.ReplyKeyboardRemove{RemoveKeyboard: true},
		})
		if removeErr != nil {
			return removeErr
		}
		err := b.sendBroadcastMenu(ctx, msg, user, "群发广播")
		b.deleteMessageBestEffort(ctx, msg.Chat.ID, msg.MessageID)
		b.deleteMessageBestEffort(ctx, msg.Chat.ID, state.ControlMessageID)
		b.deleteMessageBestEffort(ctx, msg.Chat.ID, removed.MessageID)
		return err
	}
	if isBroadcastTargetLabelText(msg.Text) {
		b.deleteMessageBestEffort(ctx, msg.Chat.ID, msg.MessageID)
		return nil
	}
	if isBroadcastNotifyToggleText(msg.Text) {
		state.NotifyAll = !state.NotifyAll
		b.privateStates.Set(formatID(user.ID), state)
		return b.refreshBroadcastSessionKeyboard(ctx, msg, state)
	}
	targets, err := b.currentBroadcastTargets(ctx, user.ID, state)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		b.privateStates.Delete(formatID(user.ID))
		return b.enqueueReplyText(ctx, sendPriorityNormal, "broadcast_target_lost", msg.Chat.ID, msg.MessageID, "广播目标已失效，请重新选择。", nil, now)
	}
	sourceChatID := msg.Chat.ID
	sourceMessageID := msg.MessageID
	targetName := state.TargetName
	mode := state.Mode
	notifyAll := state.NotifyAll
	operatorID := user.ID
	sessionKeyboard := broadcastSessionKeyboard(targetName, notifyAll)
	status, err := b.sendText(ctx, sendPriorityNormal, msg.Chat.ID, formatBroadcastProgressText(mode, len(targets)), map[string]any{
		"reply_markup": sessionKeyboard,
	})
	if err != nil {
		return err
	}
	jobDone := make(chan error, 1)
	job := func(jobCtx context.Context) {
		success, failed := b.copyBroadcast(jobCtx, operatorID, sourceChatID, sourceMessageID, targets, mode, targetName, notifyAll)
		text := formatBroadcastResultText(mode, success, failed, notifyAll)
		if _, err := b.editText(jobCtx, sourceChatID, status.MessageID, text, nil); err != nil {
			log.Printf("edit broadcast result: %v", err)
			b.deleteMessageBestEffort(jobCtx, sourceChatID, status.MessageID)
			if err := b.enqueueReliableText(jobCtx, sendPriorityLow, "broadcast_result", fmt.Sprintf("broadcast_result:%d:%d", sourceChatID, sourceMessageID), sourceChatID, text, map[string]any{"reply_markup": sessionKeyboard}, reliableMessageRef{}, time.Now().In(b.loc)); err != nil {
				log.Printf("enqueue broadcast result fallback: %v", err)
				jobDone <- err
				return
			}
		}
		jobDone <- nil
	}
	submitted, err := submitBroadcastJob(b.broadcastPool, job, func() error {
		text := "发送失败：广播队列繁忙，请稍后重试。"
		if _, err := b.editText(ctx, sourceChatID, status.MessageID, text, nil); err != nil {
			b.deleteMessageBestEffort(ctx, sourceChatID, status.MessageID)
			return b.enqueueReliableText(ctx, sendPriorityNormal, "broadcast_queue_full", fmt.Sprintf("broadcast_queue_full:%d:%d", sourceChatID, sourceMessageID), sourceChatID, text, map[string]any{"reply_markup": sessionKeyboard}, reliableMessageRef{}, now)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !submitted {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-jobDone:
		return err
	}
}

func submitBroadcastJob(pool *worker.Pool, job worker.Job, onFull func() error) (bool, error) {
	if pool != nil && pool.Submit(job) {
		return true, nil
	}
	if onFull != nil {
		return false, onFull()
	}
	return false, nil
}

func (b *Bot) currentBroadcastTargets(ctx context.Context, userID int64, state privateState) ([]storage.Group, error) {
	var allowed []storage.Group
	var err error
	if state.Mode == "group" {
		allowed, err = b.allowedChatsForBroadcastGroup(ctx, userID, state.TargetName)
	} else {
		allowed, err = b.store.ListAllowedBroadcastChats(ctx, userID, b.perms.HasGlobalBroadcastAccess(userID))
	}
	if err != nil {
		return nil, err
	}
	return intersectBroadcastTargets(state.ChatIDs, allowed), nil
}

func intersectBroadcastTargets(original []int64, allowed []storage.Group) []storage.Group {
	allowedByID := make(map[int64]storage.Group, len(allowed))
	for _, group := range allowed {
		allowedByID[group.ChatID] = group
	}
	filtered := make([]storage.Group, 0, len(original))
	for _, chatID := range original {
		if group, ok := allowedByID[chatID]; ok {
			filtered = append(filtered, group)
		}
	}
	return filtered
}

func intersectChatIDs(original []int64, allowed []int64) []int64 {
	allowedSet := make(map[int64]struct{}, len(allowed))
	for _, chatID := range allowed {
		allowedSet[chatID] = struct{}{}
	}
	filtered := make([]int64, 0, len(original))
	for _, chatID := range original {
		if _, ok := allowedSet[chatID]; ok {
			filtered = append(filtered, chatID)
		}
	}
	return filtered
}

func (b *Bot) copyBroadcast(ctx context.Context, operatorID, fromChatID, messageID int64, targets []storage.Group, mode, targetName string, notifyAll bool) (int, int) {
	success := 0
	failed := 0
	now := time.Now().In(b.loc)
	for _, target := range targets {
		if _, delivered, err := b.store.FindBroadcastDeliveryBySourceTarget(ctx, fromChatID, messageID, target.ChatID); err != nil {
			failed++
			log.Printf("check replayed broadcast delivery to %d: %v", target.ChatID, err)
			continue
		} else if delivered {
			success++
			continue
		}
		targetMsg, err := b.copyMessageWithPriority(ctx, sendPriorityBulk, target.ChatID, fromChatID, messageID, nil)
		if err != nil {
			failed++
			log.Printf("copy broadcast to %d: %v", target.ChatID, err)
		} else {
			success++
			if _, err := b.store.InsertBroadcastDelivery(ctx, storage.BroadcastDelivery{
				OperatorUserID:  operatorID,
				SourceChatID:    fromChatID,
				SourceMessageID: messageID,
				TargetChatID:    target.ChatID,
				TargetTitle:     target.Title,
				TargetMessageID: targetMsg.MessageID,
				Mode:            mode,
				TargetName:      targetName,
				CreatedAt:       now,
			}); err != nil {
				log.Printf("record broadcast delivery: %v", err)
			}
			if notifyAll {
				if !b.notifyAllInChatAsync(ctx, target.ChatID, targetMsg.MessageID) {
					if err := b.enqueueReliableText(ctx, sendPriorityLow, "notify_all_queue_full", fmt.Sprintf("notify_all_queue_full:%d:%d", target.ChatID, targetMsg.MessageID), target.ChatID, "通知所有人未发送：发送队列繁忙，请稍后重试。", map[string]any{"reply_to_message_id": targetMsg.MessageID}, reliableMessageRef{}, time.Now().In(b.loc)); err != nil {
						log.Printf("enqueue notify-all queue failure: %v", err)
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return success, failed + len(targets) - success - failed
		case <-time.After(60 * time.Millisecond):
		}
	}
	return success, failed
}

func formatBroadcastProgressText(mode string, count int) string {
	if mode == "chat" && count == 1 {
		return "发送中..."
	}
	return fmt.Sprintf("广播发送中：目标 %d 个。", count)
}

func formatBroadcastResultText(mode string, success, failed int, notifyAll bool) string {
	var text string
	if mode == "chat" && success+failed == 1 {
		if success == 1 {
			text = "发送完成。"
		} else {
			text = "发送失败。"
		}
	} else {
		text = fmt.Sprintf("广播完成：成功 %d 个，失败 %d 个。", success, failed)
	}
	if notifyAll {
		text += "\n通知所有人：开启"
	}
	return text
}

func formatBroadcastReadyText(name string, count int, notifyAll bool) string {
	status := "关闭"
	if notifyAll {
		status = "开启"
	}
	return fmt.Sprintf("已选择：%s\n目标群：%d个\n通知所有人：%s\n\n请直接发送广播内容；文字、图片、图片+文字、文件都会原样复制。底部按钮可切换通知、切换群或结束广播。", name, count, status)
}

func broadcastSessionKeyboard(targetName string, notifyAll bool) telegram.ReplyKeyboardMarkup {
	statusLabel := "通知所有人：关"
	if notifyAll {
		statusLabel = "通知所有人：开"
	}
	targetLabel := "当前目标：" + strings.TrimSpace(targetName)
	if strings.TrimSpace(targetName) == "" {
		targetLabel = "当前目标：未命名群"
	}
	return telegram.ReplyKeyboardMarkup{
		Keyboard: [][]telegram.KeyboardButton{
			{{Text: targetLabel}},
			{{Text: statusLabel}, {Text: "切换群"}, {Text: "结束广播"}},
		},
		IsPersistent:   true,
		ResizeKeyboard: true,
	}
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

func isBroadcastNotifyToggleText(text string) bool {
	switch strings.TrimSpace(text) {
	case "通知所有人", "切换通知", "开启通知", "关闭通知", "开启通知所有人", "关闭通知所有人",
		"通知所有人：关", "通知所有人：开", "通知所有人:关", "通知所有人:开",
		"通知所有人：关闭", "通知所有人：开启", "通知所有人:关闭", "通知所有人:开启":
		return true
	default:
		return false
	}
}

func isBroadcastEndText(text string) bool {
	switch strings.TrimSpace(text) {
	case "结束广播", "取消广播", "返回", "取消":
		return true
	default:
		return false
	}
}

func isBroadcastSwitchTargetText(text string) bool {
	switch strings.TrimSpace(text) {
	case "切换群", "切换目标":
		return true
	default:
		return false
	}
}

func isBroadcastTargetLabelText(text string) bool {
	return strings.HasPrefix(strings.TrimSpace(text), "当前目标：")
}

func (b *Bot) refreshBroadcastSessionKeyboard(ctx context.Context, msg telegram.Message, state privateState) error {
	ctx = withPrivateCleanupCategory(ctx, "broadcast")
	oldControlMessageID := state.ControlMessageID
	refresh, err := b.sendText(ctx, sendPriorityNormal, msg.Chat.ID, formatBroadcastReadyText(state.TargetName, len(state.ChatIDs), state.NotifyAll), map[string]any{
		"reply_markup": broadcastSessionKeyboard(state.TargetName, state.NotifyAll),
	})
	if err != nil {
		return err
	}
	state.ControlMessageID = refresh.MessageID
	if msg.From != nil {
		b.privateStates.Set(formatID(msg.From.ID), state)
	}
	b.deleteMessageBestEffort(ctx, msg.Chat.ID, msg.MessageID)
	b.deleteMessageBestEffort(ctx, msg.Chat.ID, oldControlMessageID)
	return nil
}

func (b *Bot) deleteMessageBestEffort(ctx context.Context, chatID, messageID int64) {
	if messageID <= 0 {
		return
	}
	if err := b.tg.DeleteMessage(ctx, chatID, messageID); err != nil {
		log.Printf("delete private control message %d/%d: %v", chatID, messageID, err)
	}
}

func (b *Bot) canUseBroadcast(ctx context.Context, userID int64) bool {
	if b.perms.HasGlobalBroadcastAccess(userID) {
		return true
	}
	caps, ok, err := b.globalOperatorCapabilities(ctx, userID)
	if err != nil {
		log.Printf("check global operator for broadcast %d: %v", userID, err)
		return false
	}
	return ok && b.perms.CanUsePrivateGlobalFeatures(userID, caps)
}

func (b *Bot) canUseBroadcastFresh(ctx context.Context, userID int64) (bool, error) {
	if b.perms.HasGlobalBroadcastAccess(userID) {
		return true, nil
	}
	caps, ok, err := b.globalOperatorCapabilitiesFresh(ctx, userID)
	if err != nil || !ok {
		return false, err
	}
	return b.perms.CanUsePrivateGlobalFeatures(userID, caps), nil
}

func (b *Bot) allowedChatsForBroadcastGroup(ctx context.Context, userID int64, name string) ([]storage.Group, error) {
	groupChats, err := b.store.ListBroadcastGroupChats(ctx, name)
	if err != nil {
		return nil, err
	}
	if b.perms.HasGlobalBroadcastAccess(userID) {
		return groupChats, nil
	}
	groupAllowed, err := b.store.HasBroadcastGroupUse(ctx, userID, name)
	if err != nil || !groupAllowed {
		return nil, err
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
