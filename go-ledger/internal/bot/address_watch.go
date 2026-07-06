package bot

import (
	"context"
	"fmt"
	"log"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/telegram"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/tron"
)

const addressWatchDeniedText = "没有地址监听权限。只有宿主、一级操作人和下级操作人可以使用。"

func (b *Bot) canUseAddressWatch(ctx context.Context, userID int64) bool {
	if b.isRoot(userID) {
		return true
	}
	key := "broadcast:" + formatID(userID)
	if value, ok := b.operatorCache.Get(key); ok {
		return value
	}
	value, err := b.store.IsBroadcastOperator(ctx, userID)
	if err != nil {
		log.Printf("check address watch operator %d: %v", userID, err)
		return false
	}
	b.operatorCache.Set(key, value)
	return value
}

func (b *Bot) sendAddressWatchMenu(ctx context.Context, chatID, ownerID, replyTo int64) error {
	settings, err := b.store.GetWatchSettings(ctx, ownerID)
	if err != nil {
		return err
	}
	targets, err := b.store.ListWatchTargetsForOwner(ctx, ownerID)
	if err != nil {
		return err
	}
	text := formatAddressWatchMenuText(settings, targets)
	keyboard := telegram.InlineKeyboardMarkup{InlineKeyboard: addressWatchKeyboard(settings, targets)}
	_, err = b.tg.SendMessage(ctx, chatID, text, map[string]any{
		"reply_to_message_id": replyTo,
		"reply_markup":        keyboard,
	})
	return err
}

func (b *Bot) handleAddressWatchCallback(ctx context.Context, cb telegram.CallbackQuery) error {
	if cb.Message == nil {
		return b.tg.AnswerCallback(ctx, cb.ID, "")
	}
	chatID := cb.Message.Chat.ID
	replyTo := cb.Message.MessageID
	now := time.Now().In(b.loc)
	switch {
	case cb.Data == "watch:menu":
		if err := b.tg.AnswerCallback(ctx, cb.ID, "已刷新"); err != nil {
			return err
		}
		return b.sendAddressWatchMenu(ctx, chatID, cb.From.ID, replyTo)
	case cb.Data == "watch:add":
		b.privateStates.Set(formatID(cb.From.ID), privateState{Mode: "watch_add", CreatedAt: now})
		if err := b.tg.AnswerCallback(ctx, cb.ID, "请输入监听地址"); err != nil {
			return err
		}
		_, err := b.tg.SendMessage(ctx, chatID, "请发送要监听的 TRC20 地址，可在地址后面加备注。\n例如：TGhAAy... 监控地址", map[string]any{"reply_to_message_id": replyTo})
		return err
	case cb.Data == "watch:remove":
		b.privateStates.Set(formatID(cb.From.ID), privateState{Mode: "watch_remove", CreatedAt: now})
		if err := b.tg.AnswerCallback(ctx, cb.ID, "请输入要删除的地址"); err != nil {
			return err
		}
		_, err := b.tg.SendMessage(ctx, chatID, "请发送要删除的 TRC20 监听地址。", map[string]any{"reply_to_message_id": replyTo})
		return err
	case cb.Data == "watch:min":
		b.privateStates.Set(formatID(cb.From.ID), privateState{Mode: "watch_min", CreatedAt: now})
		if err := b.tg.AnswerCallback(ctx, cb.ID, "请输入最小金额"); err != nil {
			return err
		}
		_, err := b.tg.SendMessage(ctx, chatID, "请发送最小提醒金额，低于这个 USDT 金额不提醒。\n例如：10；发送 0 表示全部提醒。", map[string]any{"reply_to_message_id": replyTo})
		return err
	case cb.Data == "watch:income", cb.Data == "watch:expense", cb.Data == "watch:trx":
		return b.toggleAddressWatchSetting(ctx, cb, now)
	case strings.HasPrefix(cb.Data, "watch:del:"):
		address := strings.TrimPrefix(cb.Data, "watch:del:")
		removed, err := b.store.RemoveWatch(ctx, cb.From.ID, address, now)
		if err != nil {
			return err
		}
		if removed {
			b.watchTargetCache.Clear()
			if err := b.tg.AnswerCallback(ctx, cb.ID, "已删除"); err != nil {
				return err
			}
		} else if err := b.tg.AnswerCallback(ctx, cb.ID, "没有找到这个地址"); err != nil {
			return err
		}
		return b.sendAddressWatchMenu(ctx, chatID, cb.From.ID, replyTo)
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
			_, err := b.tg.SendMessage(ctx, msg.Chat.ID, "地址格式不支持，请发送 T 开头的 TRC20 地址。", map[string]any{"reply_to_message_id": msg.MessageID})
			return err
		}
		if err := b.addWatchFromPrivate(ctx, msg, user, address, label, now); err != nil {
			return err
		}
		return b.sendAddressWatchMenu(ctx, msg.Chat.ID, user.ID, msg.MessageID)
	case "watch_remove":
		address := strings.Fields(text)
		if len(address) == 0 || !isTRC20Address(address[0]) {
			_, err := b.tg.SendMessage(ctx, msg.Chat.ID, "地址格式不支持，请发送要删除的 TRC20 地址。", map[string]any{"reply_to_message_id": msg.MessageID})
			return err
		}
		if err := b.removeWatchFromPrivate(ctx, msg, user, address[0], now); err != nil {
			return err
		}
		return b.sendAddressWatchMenu(ctx, msg.Chat.ID, user.ID, msg.MessageID)
	case "watch_min":
		minAmount := formatRat(parseRat(text), 2)
		if parseRat(text) == nil || parseRat(text).Sign() < 0 {
			_, err := b.tg.SendMessage(ctx, msg.Chat.ID, "最小提醒金额格式不正确，请发送大于等于 0 的数字。", map[string]any{"reply_to_message_id": msg.MessageID})
			return err
		}
		settings, err := b.store.GetWatchSettings(ctx, user.ID)
		if err != nil {
			return err
		}
		settings.MinNotifyAmount = minAmount
		if err := b.store.SaveWatchSettings(ctx, settings, now); err != nil {
			return err
		}
		b.watchTargetCache.Clear()
		_, _ = b.tg.SendMessage(ctx, msg.Chat.ID, "最小提醒金额已设置为 "+minAmount+" USDT。", map[string]any{"reply_to_message_id": msg.MessageID})
		return b.sendAddressWatchMenu(ctx, msg.Chat.ID, user.ID, msg.MessageID)
	default:
		return nil
	}
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
	case "watch:trx":
		settings.NotifyTRX = !settings.NotifyTRX
		label = "TRX通知"
	}
	if err := b.store.SaveWatchSettings(ctx, settings, now); err != nil {
		return err
	}
	b.watchTargetCache.Clear()
	if err := b.tg.AnswerCallback(ctx, cb.ID, label+"已切换"); err != nil {
		return err
	}
	return b.sendAddressWatchMenu(ctx, cb.Message.Chat.ID, cb.From.ID, cb.Message.MessageID)
}

func (b *Bot) addWatchFromPrivate(ctx context.Context, msg telegram.Message, user storage.User, address, label string, now time.Time) error {
	if !b.canUseAddressWatch(ctx, user.ID) {
		_, err := b.tg.SendMessage(ctx, msg.Chat.ID, addressWatchDeniedText, map[string]any{"reply_to_message_id": msg.MessageID})
		return err
	}
	if !isTRC20Address(address) {
		_, err := b.tg.SendMessage(ctx, msg.Chat.ID, "地址格式不支持。USDT 监听当前只支持 TRC20 的 T 开头地址。", map[string]any{"reply_to_message_id": msg.MessageID})
		return err
	}
	if err := b.store.AddWatch(ctx, user.ID, address, strings.TrimSpace(label), now); err != nil {
		return err
	}
	b.watchTargetCache.Clear()
	_, err := b.tg.SendMessage(ctx, msg.Chat.ID, "监听地址已保存。", map[string]any{"reply_to_message_id": msg.MessageID})
	return err
}

func formatAddressWatchMenuText(settings storage.WatchSettings, targets []storage.WatchTarget) string {
	var out strings.Builder
	out.WriteString("USDT 地址监听\n\n")
	out.WriteString("收入：")
	out.WriteString(onOff(settings.WatchIncome))
	out.WriteString("  支出：")
	out.WriteString(onOff(settings.WatchExpense))
	out.WriteString("\nTRX通知：")
	out.WriteString(onOff(settings.NotifyTRX))
	out.WriteString("  最小提醒：")
	out.WriteString(settings.MinNotifyAmount)
	out.WriteString(" USDT\n\n当前监听地址：")
	if len(targets) == 0 {
		out.WriteString("\n暂无")
		return out.String()
	}
	for i, target := range targets {
		out.WriteByte('\n')
		out.WriteString(strconv.Itoa(i + 1))
		out.WriteString(". ")
		out.WriteString(target.Address)
		if target.Label != "" {
			out.WriteString("  ")
			out.WriteString(target.Label)
		}
	}
	return out.String()
}

func addressWatchKeyboard(settings storage.WatchSettings, targets []storage.WatchTarget) [][]telegram.InlineKeyboardButton {
	rows := [][]telegram.InlineKeyboardButton{
		{{Text: "添加地址", CallbackData: "watch:add"}, {Text: "删除地址", CallbackData: "watch:remove"}},
		{{Text: "收入 " + onOff(settings.WatchIncome), CallbackData: "watch:income"}, {Text: "支出 " + onOff(settings.WatchExpense), CallbackData: "watch:expense"}},
		{{Text: "TRX通知 " + onOff(settings.NotifyTRX), CallbackData: "watch:trx"}, {Text: "最小金额 " + settings.MinNotifyAmount, CallbackData: "watch:min"}},
	}
	for _, target := range targets {
		rows = append(rows, []telegram.InlineKeyboardButton{{
			Text:         "删除 " + shortAddress(target.Address),
			CallbackData: "watch:del:" + target.Address,
		}})
		if len(rows) >= 12 {
			break
		}
	}
	rows = append(rows, []telegram.InlineKeyboardButton{{Text: "刷新", CallbackData: "watch:menu"}})
	return rows
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

func (b *Bot) removeWatchFromPrivate(ctx context.Context, msg telegram.Message, user storage.User, address string, now time.Time) error {
	if !b.canUseAddressWatch(ctx, user.ID) {
		_, err := b.tg.SendMessage(ctx, msg.Chat.ID, addressWatchDeniedText, map[string]any{"reply_to_message_id": msg.MessageID})
		return err
	}
	removed, err := b.store.RemoveWatch(ctx, user.ID, address, now)
	if err != nil {
		return err
	}
	if removed {
		b.watchTargetCache.Clear()
	}
	text := "监听地址已删除。"
	if !removed {
		text = "没有找到这个监听地址。"
	}
	_, err = b.tg.SendMessage(ctx, msg.Chat.ID, text, map[string]any{"reply_to_message_id": msg.MessageID})
	return err
}

func (b *Bot) addressWatchScheduler(ctx context.Context) {
	if b.cfg.TronPollInterval <= 0 {
		return
	}
	ticker := time.NewTicker(b.cfg.TronPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.chainPool.Submit(func(jobCtx context.Context) {
				if err := b.pollAddressWatches(jobCtx); err != nil {
					log.Printf("poll address watches: %v", err)
				}
			})
		}
	}
}

func (b *Bot) pollAddressWatches(ctx context.Context) error {
	targets, err := b.watchTargets(ctx)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return nil
	}
	minTimestamp := time.Now().Add(-time.Duration(b.cfg.TronLookbackMinutes) * time.Minute).UnixMilli()
	for _, target := range targets {
		if target.LatestTimestamp > 0 && target.LatestTimestamp-30000 > minTimestamp {
			minTimestamp = target.LatestTimestamp - 30000
		}
	}
	transfers, err := b.tron.FetchGlobalUSDTTransfers(ctx, b.cfg.USDTContract, minTimestamp, b.cfg.TronGlobalPages)
	if err != nil {
		return err
	}
	byAddress := make(map[string][]storage.WatchTarget)
	for _, target := range targets {
		byAddress[target.Address] = append(byAddress[target.Address], target)
	}
	now := time.Now().In(b.loc)
	for _, transfer := range transfers {
		matches := append([]storage.WatchTarget{}, byAddress[transfer.From]...)
		matches = append(matches, byAddress[transfer.To]...)
		for _, target := range matches {
			direction := "income"
			if transfer.From == target.Address {
				direction = "expense"
			}
			if direction == "income" && !target.WatchIncome {
				continue
			}
			if direction == "expense" && !target.WatchExpense {
				continue
			}
			if !amountAtLeast(transfer.Value, transfer.TokenDecimals, target.MinNotifyAmount) {
				continue
			}
			inserted, err := b.store.RecordChainNotification(ctx, target.OwnerUserID, target.Address, transfer.Hash, direction, transfer.BlockTimestamp, now)
			if err != nil {
				log.Printf("record chain notification: %v", err)
				continue
			}
			if !inserted {
				continue
			}
			t := transfer
			w := target
			d := direction
			b.notifyPool.Submit(func(sendCtx context.Context) {
				_, err := b.tg.SendMessage(sendCtx, w.OwnerUserID, formatTransferNotice(t, w, d), map[string]any{"parse_mode": "HTML"})
				if err != nil {
					log.Printf("send chain notification: %v", err)
				}
			})
		}
	}
	return nil
}

func (b *Bot) watchTargets(ctx context.Context) ([]storage.WatchTarget, error) {
	if cached, ok := b.watchTargetCache.Get("all"); ok {
		return cached, nil
	}
	targets, err := b.store.ListWatchTargets(ctx)
	if err != nil {
		return nil, err
	}
	b.watchTargetCache.Set("all", targets)
	return targets, nil
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

func formatTransferNotice(t tron.Transfer, w storage.WatchTarget, direction string) string {
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
	return fmt.Sprintf("交易类型： %s\n交易金额： %s USDT\n出账地址： <code>%s</code>\n入账地址： <code>%s</code>\n交易时间： %s\n交易哈希： <a href=\"https://tronscan.org/#/transaction/%s\">%s</a>",
		label,
		signedAmount,
		from,
		to,
		time.UnixMilli(t.BlockTimestamp).Format("2006-01-02 15:04:05"),
		t.Hash,
		shortHash(t.Hash),
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
