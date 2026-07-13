package bot

import (
	"context"
	"fmt"
	"html"
	"log"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/telegram"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/tron"
)

func (b *Bot) hasUnlimitedAddressWatch(ctx context.Context, userID int64) bool {
	if b.perms.HasGlobalAddressWatchAccess(userID) {
		return true
	}
	caps, ok, err := b.globalOperatorCapabilities(ctx, userID)
	if err != nil {
		log.Printf("check address watch global operator %d: %v", userID, err)
		return false
	}
	return ok && b.perms.CanUsePrivateGlobalFeatures(userID, caps)
}

func (b *Bot) sendAddressWatchMenu(ctx context.Context, chatID, ownerID, replyTo int64) error {
	targets, err := b.store.ListWatchTargetsForOwner(ctx, ownerID)
	if err != nil {
		return err
	}
	text := formatAddressWatchMenuText(targets)
	keyboard := telegram.InlineKeyboardMarkup{InlineKeyboard: addressWatchKeyboard(targets)}
	return b.enqueueReliableText(ctx, sendPriorityNormal, "address_watch_menu", fmt.Sprintf("address_watch_menu:%d:%d:%d", chatID, ownerID, time.Now().UnixNano()), chatID, text, map[string]any{
		"parse_mode":   "HTML",
		"reply_markup": keyboard,
	}, reliableMessageRef{}, time.Now().In(b.loc))
}

func (b *Bot) handleAddressWatchCallback(ctx context.Context, cb telegram.CallbackQuery) error {
	if cb.Message == nil {
		return b.tg.AnswerCallback(ctx, cb.ID, "")
	}
	chatID := cb.Message.Chat.ID
	messageID := cb.Message.MessageID
	now := time.Now().In(b.loc)
	switch {
	case cb.Data == "watch:menu":
		if err := b.tg.AnswerCallback(ctx, cb.ID, "已刷新"); err != nil {
			return err
		}
		return b.editAddressWatchMenu(ctx, chatID, messageID, cb.From.ID)
	case cb.Data == "watch:add":
		b.privateStates.Set(formatID(cb.From.ID), privateState{Mode: "watch_add", CreatedAt: now})
		if err := b.tg.AnswerCallback(ctx, cb.ID, "请输入监听地址"); err != nil {
			return err
		}
		return b.enqueueReliableText(ctx, sendPriorityNormal, "watch_add_prompt", fmt.Sprintf("watch_add_prompt:%d:%d:%d", chatID, cb.From.ID, now.UnixNano()), chatID, "请发送要监听的 TRC20 地址，可在地址后面加备注。\n例如：TGhAAy... 监控地址", nil, reliableMessageRef{}, now)
	case cb.Data == "watch:remove":
		b.privateStates.Set(formatID(cb.From.ID), privateState{Mode: "watch_remove", CreatedAt: now})
		if err := b.tg.AnswerCallback(ctx, cb.ID, "请输入要删除的地址"); err != nil {
			return err
		}
		return b.enqueueReliableText(ctx, sendPriorityNormal, "watch_remove_prompt", fmt.Sprintf("watch_remove_prompt:%d:%d:%d", chatID, cb.From.ID, now.UnixNano()), chatID, "请发送要删除的 TRC20 监听地址。", nil, reliableMessageRef{}, now)
	case cb.Data == "watch:min":
		b.privateStates.Set(formatID(cb.From.ID), privateState{Mode: "watch_min", CreatedAt: now})
		if err := b.tg.AnswerCallback(ctx, cb.ID, "请输入最小金额"); err != nil {
			return err
		}
		return b.enqueueReliableText(ctx, sendPriorityNormal, "watch_min_prompt", fmt.Sprintf("watch_min_prompt:%d:%d:%d", chatID, cb.From.ID, now.UnixNano()), chatID, "请发送最小提醒金额，低于这个 USDT 金额不提醒。\n例如：10；发送 0 表示全部提醒。", nil, reliableMessageRef{}, now)
	case cb.Data == "watch:income", cb.Data == "watch:expense":
		return b.toggleAddressWatchSetting(ctx, cb, now)
	case strings.HasPrefix(cb.Data, "watch:open:"):
		address := strings.TrimPrefix(cb.Data, "watch:open:")
		if err := b.tg.AnswerCallback(ctx, cb.ID, "已打开"); err != nil {
			return err
		}
		return b.editAddressWatchDetail(ctx, chatID, messageID, cb.From.ID, address)
	case strings.HasPrefix(cb.Data, "watch:t:"):
		return b.handleAddressWatchTargetCallback(ctx, cb, now)
	case strings.HasPrefix(cb.Data, "watch:del:"):
		address := strings.TrimPrefix(cb.Data, "watch:del:")
		removed, err := b.store.RemoveWatch(ctx, cb.From.ID, address, now)
		if err != nil {
			return err
		}
		if removed {
			b.InvalidateWatchTargets()
			b.deleteChainWatcherSubscriptionAsync(ctx, cb.From.ID, address)
			if err := b.tg.AnswerCallback(ctx, cb.ID, "已删除"); err != nil {
				return err
			}
		} else if err := b.tg.AnswerCallback(ctx, cb.ID, "没有找到这个地址"); err != nil {
			return err
		}
		return b.editAddressWatchMenu(ctx, chatID, messageID, cb.From.ID)
	default:
		return b.tg.AnswerCallback(ctx, cb.ID, "")
	}
}

func (b *Bot) handleAddressWatchState(ctx context.Context, msg telegram.Message, user storage.User, state privateState, text string, now time.Time) error {
	b.privateStates.Delete(formatID(user.ID))
	switch state.Mode {
	case "watch_add":
		address, label := parseWatchAddressAndLabel(text)
		if address == "" {
			return b.enqueueReplyText(ctx, sendPriorityNormal, "watch_add_invalid", msg.Chat.ID, msg.MessageID, "地址格式不支持，请发送 T 开头的 TRC20 地址。", nil, now)
		}
		if err := b.addWatchFromPrivate(ctx, msg, user, address, label, now); err != nil {
			return err
		}
		return b.sendAddressWatchMenu(ctx, msg.Chat.ID, user.ID, msg.MessageID)
	case "watch_remove":
		address := strings.Fields(text)
		if len(address) == 0 || !isTRC20Address(address[0]) {
			return b.enqueueReplyText(ctx, sendPriorityNormal, "watch_remove_invalid", msg.Chat.ID, msg.MessageID, "地址格式不支持，请发送要删除的 TRC20 地址。", nil, now)
		}
		if err := b.removeWatchFromPrivate(ctx, msg, user, address[0], now); err != nil {
			return err
		}
		return b.sendAddressWatchMenu(ctx, msg.Chat.ID, user.ID, msg.MessageID)
	case "watch_min":
		minAmount := formatRat(parseRat(text), 2)
		if parseRat(text) == nil || parseRat(text).Sign() < 0 {
			return b.enqueueReplyText(ctx, sendPriorityNormal, "watch_min_invalid", msg.Chat.ID, msg.MessageID, "最小提醒金额格式不正确，请发送大于等于 0 的数字。", nil, now)
		}
		settings, err := b.store.GetWatchSettings(ctx, user.ID)
		if err != nil {
			return err
		}
		settings.MinNotifyAmount = minAmount
		if err := b.store.SaveWatchSettings(ctx, settings, now); err != nil {
			return err
		}
		b.InvalidateWatchTargets()
		_ = b.enqueueReplyText(ctx, sendPriorityNormal, "watch_min_ok", msg.Chat.ID, msg.MessageID, "最小提醒金额已设置为 "+minAmount+" USDT。", nil, now)
		return b.sendAddressWatchMenu(ctx, msg.Chat.ID, user.ID, msg.MessageID)
	case "watch_target_min":
		minAmount := formatRat(parseRat(text), 2)
		if parseRat(text) == nil || parseRat(text).Sign() < 0 {
			return b.enqueueReplyText(ctx, sendPriorityNormal, "watch_target_min_invalid", msg.Chat.ID, msg.MessageID, "最小提醒金额格式不正确，请发送大于等于 0 的数字。", nil, now)
		}
		target, ok, err := b.store.GetWatchTarget(ctx, user.ID, state.WatchAddress)
		if err != nil {
			return err
		}
		if !ok {
			return b.enqueueReplyText(ctx, sendPriorityNormal, "watch_target_min_lost", msg.Chat.ID, msg.MessageID, "没有找到这个监听地址。", nil, now)
		}
		target.MinNotifyAmount = minAmount
		if _, err := b.store.UpdateWatchTarget(ctx, target, now); err != nil {
			return err
		}
		b.InvalidateWatchTargets()
		b.syncChainWatcherTargetAsync(ctx, target)
		_ = b.enqueueReplyText(ctx, sendPriorityNormal, "watch_target_min_ok", msg.Chat.ID, msg.MessageID, "最小提醒金额已设置为 "+minAmount+" USDT。", nil, now)
		return b.sendAddressWatchDetail(ctx, msg.Chat.ID, user.ID, target.Address)
	case "watch_target_label":
		target, ok, err := b.store.GetWatchTarget(ctx, user.ID, state.WatchAddress)
		if err != nil {
			return err
		}
		if !ok {
			return b.enqueueReplyText(ctx, sendPriorityNormal, "watch_target_label_lost", msg.Chat.ID, msg.MessageID, "没有找到这个监听地址。", nil, now)
		}
		target.Label = normalizeWatchLabel(text)
		if _, err := b.store.UpdateWatchTarget(ctx, target, now); err != nil {
			return err
		}
		b.InvalidateWatchTargets()
		b.syncChainWatcherTargetAsync(ctx, target)
		_ = b.enqueueReplyText(ctx, sendPriorityNormal, "watch_target_label_ok", msg.Chat.ID, msg.MessageID, "备注已保存。", nil, now)
		return b.sendAddressWatchDetail(ctx, msg.Chat.ID, user.ID, target.Address)
	default:
		return nil
	}
}

func (b *Bot) handleAddressWatchTargetCallback(ctx context.Context, cb telegram.CallbackQuery, now time.Time) error {
	if cb.Message == nil {
		return b.tg.AnswerCallback(ctx, cb.ID, "")
	}
	parts := strings.SplitN(strings.TrimPrefix(cb.Data, "watch:t:"), ":", 2)
	if len(parts) != 2 {
		return b.tg.AnswerCallback(ctx, cb.ID, "")
	}
	action, address := parts[0], parts[1]
	chatID := cb.Message.Chat.ID
	messageID := cb.Message.MessageID
	if action == "back" {
		if err := b.tg.AnswerCallback(ctx, cb.ID, "返回列表"); err != nil {
			return err
		}
		return b.editAddressWatchMenu(ctx, chatID, messageID, cb.From.ID)
	}
	target, ok, err := b.store.GetWatchTarget(ctx, cb.From.ID, address)
	if err != nil {
		return err
	}
	if !ok {
		if err := b.tg.AnswerCallback(ctx, cb.ID, "没有找到这个地址"); err != nil {
			return err
		}
		return b.editAddressWatchMenu(ctx, chatID, messageID, cb.From.ID)
	}
	switch action {
	case "enabled":
		enabled := targetWatchEnabled(target)
		target.WatchIncome = !enabled
		target.WatchExpense = !enabled
		target.NotifyTRX = false
	case "income":
		target.WatchIncome = !target.WatchIncome
	case "expense":
		target.WatchExpense = !target.WatchExpense
	case "min":
		b.privateStates.Set(formatID(cb.From.ID), privateState{Mode: "watch_target_min", WatchAddress: address, CreatedAt: now})
		if err := b.tg.AnswerCallback(ctx, cb.ID, "请输入最小金额"); err != nil {
			return err
		}
		return b.enqueueReliableText(ctx, sendPriorityNormal, "watch_target_min_prompt", fmt.Sprintf("watch_target_min_prompt:%d:%d:%d", chatID, cb.From.ID, now.UnixNano()), chatID, "请发送这个地址的最小提醒金额，低于这个 USDT 金额不提醒。\n例如：10；发送 0 表示全部提醒。", nil, reliableMessageRef{}, now)
	case "label":
		b.privateStates.Set(formatID(cb.From.ID), privateState{Mode: "watch_target_label", WatchAddress: address, CreatedAt: now})
		if err := b.tg.AnswerCallback(ctx, cb.ID, "请输入备注"); err != nil {
			return err
		}
		return b.enqueueReliableText(ctx, sendPriorityNormal, "watch_target_label_prompt", fmt.Sprintf("watch_target_label_prompt:%d:%d:%d", chatID, cb.From.ID, now.UnixNano()), chatID, "请发送这个地址的备注。发送“清空”可删除备注。", nil, reliableMessageRef{}, now)
	case "del":
		removed, err := b.store.RemoveWatch(ctx, cb.From.ID, address, now)
		if err != nil {
			return err
		}
		b.InvalidateWatchTargets()
		if removed {
			b.deleteChainWatcherSubscriptionAsync(ctx, cb.From.ID, address)
		}
		if removed {
			if err := b.tg.AnswerCallback(ctx, cb.ID, "已删除"); err != nil {
				return err
			}
		} else if err := b.tg.AnswerCallback(ctx, cb.ID, "没有找到这个地址"); err != nil {
			return err
		}
		return b.editAddressWatchMenu(ctx, chatID, messageID, cb.From.ID)
	default:
		return b.tg.AnswerCallback(ctx, cb.ID, "")
	}
	if _, err := b.store.UpdateWatchTarget(ctx, target, now); err != nil {
		return err
	}
	b.InvalidateWatchTargets()
	b.syncChainWatcherTargetAsync(ctx, target)
	if err := b.tg.AnswerCallback(ctx, cb.ID, "已更新"); err != nil {
		return err
	}
	return b.editAddressWatchDetail(ctx, chatID, messageID, cb.From.ID, address)
}

func (b *Bot) toggleAddressWatchSetting(ctx context.Context, cb telegram.CallbackQuery, now time.Time) error {
	settings, err := b.store.GetWatchSettings(ctx, cb.From.ID)
	if err != nil {
		return err
	}
	label := ""
	switch cb.Data {
	case "watch:income":
		settings.WatchIncome = !settings.WatchIncome
		label = "收入提醒"
	case "watch:expense":
		settings.WatchExpense = !settings.WatchExpense
		label = "支出提醒"
	}
	if err := b.store.SaveWatchSettings(ctx, settings, now); err != nil {
		return err
	}
	b.InvalidateWatchTargets()
	if err := b.tg.AnswerCallback(ctx, cb.ID, label+"已切换"); err != nil {
		return err
	}
	return b.sendAddressWatchMenu(ctx, cb.Message.Chat.ID, cb.From.ID, cb.Message.MessageID)
}

func (b *Bot) addWatchFromPrivate(ctx context.Context, msg telegram.Message, user storage.User, address, label string, now time.Time) error {
	if !isTRC20Address(address) {
		return b.enqueueReplyText(ctx, sendPriorityNormal, "watch_add_bad_address", msg.Chat.ID, msg.MessageID, "地址格式不支持。USDT 监听当前只支持 TRC20 的 T 开头地址。", nil, now)
	}
	if err := b.guardAddressWatchLimit(ctx, msg, user.ID, address, now); err != nil {
		return err
	}
	if err := b.store.AddWatch(ctx, user.ID, address, strings.TrimSpace(label), now); err != nil {
		return err
	}
	b.InvalidateWatchTargets()
	if target, ok, err := b.store.GetWatchTarget(ctx, user.ID, address); err == nil && ok {
		b.syncChainWatcherTargetAsync(ctx, target)
	}
	return b.enqueueReplyText(ctx, sendPriorityNormal, "watch_add_ok", msg.Chat.ID, msg.MessageID, "监听地址已保存。", nil, now)
}

func (b *Bot) guardAddressWatchLimit(ctx context.Context, msg telegram.Message, userID int64, address string, now time.Time) error {
	if b.hasUnlimitedAddressWatch(ctx, userID) {
		return nil
	}
	if _, ok, err := b.store.GetWatchTarget(ctx, userID, address); err != nil {
		return err
	} else if ok {
		return nil
	}
	limit := b.cfg.AddressWatchFreeLimit
	if limit <= 0 {
		return b.enqueueReplyText(ctx, sendPriorityNormal, "watch_add_limited_closed", msg.Chat.ID, msg.MessageID, "当前暂未开放普通用户添加监听地址。", nil, now)
	}
	count, err := b.store.CountActiveWatchTargetsForOwner(ctx, userID)
	if err != nil {
		return err
	}
	if count >= limit {
		return b.enqueueReplyText(ctx, sendPriorityNormal, "watch_add_limited", msg.Chat.ID, msg.MessageID, fmt.Sprintf("普通用户最多只能监听 %d 个地址。请先删除不需要的监听地址。", limit), nil, now)
	}
	return nil
}

func (b *Bot) editAddressWatchMenu(ctx context.Context, chatID, messageID, ownerID int64) error {
	targets, err := b.store.ListWatchTargetsForOwner(ctx, ownerID)
	if err != nil {
		return err
	}
	_, err = b.editText(ctx, chatID, messageID, formatAddressWatchMenuText(targets), map[string]any{
		"parse_mode": "HTML",
		"reply_markup": telegram.InlineKeyboardMarkup{
			InlineKeyboard: addressWatchKeyboard(targets),
		},
	})
	return err
}

func (b *Bot) sendAddressWatchDetail(ctx context.Context, chatID, ownerID int64, address string) error {
	target, ok, err := b.store.GetWatchTarget(ctx, ownerID, address)
	if err != nil {
		return err
	}
	if !ok {
		return b.enqueueReliableText(ctx, sendPriorityNormal, "watch_detail_lost", fmt.Sprintf("watch_detail_lost:%d:%d:%d", chatID, ownerID, time.Now().UnixNano()), chatID, "没有找到这个监听地址。", nil, reliableMessageRef{}, time.Now().In(b.loc))
	}
	return b.enqueueReliableText(ctx, sendPriorityNormal, "watch_detail", fmt.Sprintf("watch_detail:%d:%d:%s:%d", chatID, ownerID, target.Address, time.Now().UnixNano()), chatID, formatAddressWatchDetailText(target), map[string]any{
		"parse_mode": "HTML",
		"reply_markup": telegram.InlineKeyboardMarkup{
			InlineKeyboard: addressWatchDetailKeyboard(target),
		},
	}, reliableMessageRef{}, time.Now().In(b.loc))
}

func (b *Bot) editAddressWatchDetail(ctx context.Context, chatID, messageID, ownerID int64, address string) error {
	target, ok, err := b.store.GetWatchTarget(ctx, ownerID, address)
	if err != nil {
		return err
	}
	if !ok {
		_, err := b.editText(ctx, chatID, messageID, "没有找到这个监听地址。", map[string]any{
			"reply_markup": telegram.InlineKeyboardMarkup{InlineKeyboard: [][]telegram.InlineKeyboardButton{
				{{Text: "返回列表", CallbackData: "watch:menu"}},
			}},
		})
		return err
	}
	_, err = b.editText(ctx, chatID, messageID, formatAddressWatchDetailText(target), map[string]any{
		"parse_mode": "HTML",
		"reply_markup": telegram.InlineKeyboardMarkup{
			InlineKeyboard: addressWatchDetailKeyboard(target),
		},
	})
	return err
}

func formatAddressWatchMenuText(targets []storage.WatchTarget) string {
	var out strings.Builder
	out.WriteString("<b>USDT 地址监听</b>\n\n")
	out.WriteString("当前监听地址：")
	if len(targets) == 0 {
		out.WriteString("\n暂无")
		return out.String()
	}
	for i, target := range targets {
		out.WriteByte('\n')
		out.WriteString(strconv.Itoa(i + 1))
		out.WriteString(". ")
		out.WriteString(html.EscapeString(target.Address))
		out.WriteString("  ")
		out.WriteString(onOff(targetWatchEnabled(target)))
	}
	return out.String()
}

func formatAddressWatchDetailText(target storage.WatchTarget) string {
	var out strings.Builder
	out.WriteString("<b>监听地址设置</b>\n\n")
	out.WriteString("<code>")
	out.WriteString(html.EscapeString(target.Address))
	out.WriteString("</code>\n")
	out.WriteString("状态：")
	out.WriteString(onOff(targetWatchEnabled(target)))
	out.WriteString("\n备注：")
	if target.Label == "" {
		out.WriteString("无")
	} else {
		out.WriteString(html.EscapeString(target.Label))
	}
	out.WriteString("\n\n收入：")
	out.WriteString(onOff(target.WatchIncome))
	out.WriteString("  支出：")
	out.WriteString(onOff(target.WatchExpense))
	out.WriteString("\n最小提醒：")
	out.WriteString(target.MinNotifyAmount)
	out.WriteString(" USDT")
	return out.String()
}

func addressWatchKeyboard(targets []storage.WatchTarget) [][]telegram.InlineKeyboardButton {
	rows := [][]telegram.InlineKeyboardButton{
		{{Text: "添加地址", CallbackData: "watch:add"}},
	}
	for _, target := range targets {
		rows = append(rows, []telegram.InlineKeyboardButton{{
			Text:         target.Address + "  " + onOff(targetWatchEnabled(target)),
			CallbackData: "watch:open:" + target.Address,
		}})
	}
	rows = append(rows, []telegram.InlineKeyboardButton{{Text: "刷新", CallbackData: "watch:menu"}})
	return rows
}

func addressWatchDetailKeyboard(target storage.WatchTarget) [][]telegram.InlineKeyboardButton {
	enabledAction := "关闭监听"
	if !targetWatchEnabled(target) {
		enabledAction = "开启监听"
	}
	address := target.Address
	return [][]telegram.InlineKeyboardButton{
		{{Text: enabledAction, CallbackData: "watch:t:enabled:" + address}},
		{{Text: "收入 " + onOff(target.WatchIncome), CallbackData: "watch:t:income:" + address}, {Text: "支出 " + onOff(target.WatchExpense), CallbackData: "watch:t:expense:" + address}},
		{{Text: "最小金额 " + target.MinNotifyAmount, CallbackData: "watch:t:min:" + address}},
		{{Text: "设置备注", CallbackData: "watch:t:label:" + address}, {Text: "删除地址", CallbackData: "watch:t:del:" + address}},
		{{Text: "返回列表", CallbackData: "watch:t:back:" + address}},
	}
}

func parseWatchAddressAndLabel(text string) (string, string) {
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) == 0 || !isTRC20Address(fields[0]) {
		return "", ""
	}
	address := fields[0]
	label := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), address))
	return address, label
}

func onOff(value bool) string {
	if value {
		return "开启"
	}
	return "关闭"
}

func targetWatchEnabled(target storage.WatchTarget) bool {
	return target.WatchIncome || target.WatchExpense
}

func normalizeWatchLabel(text string) string {
	text = strings.TrimSpace(text)
	switch text {
	case "清空", "删除", "无", "-":
		return ""
	default:
		return text
	}
}

func (b *Bot) removeWatchFromPrivate(ctx context.Context, msg telegram.Message, user storage.User, address string, now time.Time) error {
	removed, err := b.store.RemoveWatch(ctx, user.ID, address, now)
	if err != nil {
		return err
	}
	if removed {
		b.InvalidateWatchTargets()
		b.deleteChainWatcherSubscriptionAsync(ctx, user.ID, address)
	}
	text := "监听地址已删除。"
	if !removed {
		text = "没有找到这个监听地址。"
	}
	return b.enqueueReplyText(ctx, sendPriorityNormal, "watch_remove_result", msg.Chat.ID, msg.MessageID, text, nil, now)
}

func amountAtLeast(raw string, decimals int, minRaw string) bool {
	value := tokenAmount(raw, decimals)
	min := parseRat(minRaw)
	if min == nil {
		return true
	}
	return value.Cmp(min) >= 0
}

func tokenAmount(raw string, decimals int) *big.Rat {
	if decimals < 0 || decimals > 30 {
		decimals = 6
	}
	value := parseRat(raw)
	if value == nil {
		return big.NewRat(0, 1)
	}
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	return value.Quo(value, new(big.Rat).SetInt(scale))
}

func (b *Bot) formatTransferNotice(t tron.Transfer, w storage.WatchTarget, direction string) string {
	amount := formatAmount(tokenAmount(t.Value, t.TokenDecimals))
	label := "⬇️收入"
	signedAmount := amount
	if direction == "expense" {
		label = "⬆️支出"
		signedAmount = "-" + amount
	}
	from := t.From
	to := t.To
	if t.From == w.Address && w.Label != "" {
		from += " ← " + w.Label
	}
	if t.To == w.Address && w.Label != "" {
		to += " ← " + w.Label
	}
	return fmt.Sprintf("交易类型： %s\n交易金额： %s USDT\n出账地址： %s\n入账地址： %s\n交易时间： %s\n交易哈希： <a href=\"https://tronscan.org/#/transaction/%s\">%s</a>",
		label,
		signedAmount,
		formatCode(from),
		formatCode(to),
		formatMilliTime(t.BlockTimestamp, b.loc),
		html.EscapeString(t.Hash),
		html.EscapeString(shortHash(t.Hash)),
	)
}

func shortHash(hash string) string {
	if len(hash) <= 12 {
		return hash
	}
	return hash[:4] + "..." + hash[len(hash)-4:]
}

func formatID(id int64) string {
	return strconv.FormatInt(id, 10)
}
