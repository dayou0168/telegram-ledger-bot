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
)

func (b *Bot) startAccounting(ctx context.Context, msg telegram.Message, user storage.User, now time.Time) error {
	if !b.isRoot(user.ID) {
		ok, err := b.isGroupOperator(ctx, msg.Chat.ID, user.ID)
		if err != nil {
			return err
		}
		if !ok {
			_, err := b.tg.SendMessage(ctx, msg.Chat.ID, "没有开启记账权限。", map[string]any{"reply_to_message_id": msg.MessageID})
			return err
		}
	}
	if b.cfg.HostUserID != 0 && user.ID == b.cfg.HostUserID {
		_ = b.store.SetGroupOwner(ctx, msg.Chat.ID, user, now)
	}
	if err := b.store.SetGroupActive(ctx, msg.Chat.ID, true, now); err != nil {
		return err
	}
	_, err := b.tg.SendMessage(ctx, msg.Chat.ID, "机器人已开启，请开始记账", map[string]any{"reply_to_message_id": msg.MessageID})
	return err
}

func (b *Bot) stopAccounting(ctx context.Context, msg telegram.Message, user storage.User, now time.Time) error {
	if ok, err := b.canUseLedger(ctx, msg.Chat.ID, user.ID); err != nil {
		return err
	} else if !ok {
		_, err := b.tg.SendMessage(ctx, msg.Chat.ID, "没有停止记账权限。", map[string]any{"reply_to_message_id": msg.MessageID})
		return err
	}
	if err := b.store.SetGroupActive(ctx, msg.Chat.ID, false, now); err != nil {
		return err
	}
	_, err := b.tg.SendMessage(ctx, msg.Chat.ID, "已停止记账。发送“开始”可重新开启。", map[string]any{"reply_to_message_id": msg.MessageID})
	return err
}

func (b *Bot) handleLedger(ctx context.Context, msg telegram.Message, user storage.User, cmd ledgerCommand, now time.Time) error {
	group, err := b.store.GetGroup(ctx, msg.Chat.ID)
	if err != nil {
		return err
	}
	if cmd.Amount.Sign() == 0 {
		return b.sendBill(ctx, msg.Chat.ID, msg.MessageID, now, "")
	}
	if !group.Active {
		_, err := b.tg.SendMessage(ctx, msg.Chat.ID, "当前未开始记账，请先发送“开始”。", map[string]any{"reply_to_message_id": msg.MessageID})
		return err
	}
	if ok, err := b.canUseLedgerWithGroup(ctx, group, user.ID); err != nil {
		return err
	} else if !ok {
		_, err := b.tg.SendMessage(ctx, msg.Chat.ID, "没有操作权限。请管理员添加操作员。", map[string]any{"reply_to_message_id": msg.MessageID})
		return err
	}

	effectiveAmount := new(big.Rat).Set(cmd.Amount)
	if cmd.Multiplier != nil {
		effectiveAmount.Mul(effectiveAmount, cmd.Multiplier)
	}
	rate := cmd.Rate
	if cmd.IsUSDT {
		rate = big.NewRat(1, 1)
	} else if rate == nil && cmd.Kind == "payout" {
		rate = parseRat(group.PayoutExchangeRate)
	} else if rate == nil {
		rate = parseRat(group.DepositExchangeRate)
	}
	if rate == nil || rate.Sign() == 0 {
		rate = big.NewRat(1, 1)
	}
	resultUSDT := new(big.Rat).Set(effectiveAmount)
	if !cmd.IsUSDT {
		resultUSDT.Quo(resultUSDT, rate)
	}
	feeRate := cmd.FeeRate
	if feeRate == nil {
		feeRate = parseRat(group.FeeRate)
	}
	if cmd.Kind == "deposit" && feeRate != nil && feeRate.Sign() != 0 {
		resultUSDT.Mul(resultUSDT, feeFactor(feeRate))
	}
	currency := "CNY"
	if cmd.IsUSDT {
		currency = "USDT"
	}
	dayKey := businessDayKey(now, group.CutoffHour)
	recordID, err := b.store.InsertRecord(ctx, storage.Record{
		ChatID:          msg.Chat.ID,
		DayKey:          dayKey,
		Kind:            cmd.Kind,
		Currency:        currency,
		Amount:          formatAmount(effectiveAmount),
		Rate:            formatRat(rate, 8),
		FeeRate:         formatRat(feeRate, 4),
		ResultUSDT:      formatAmount(resultUSDT),
		ActorUserID:     user.ID,
		ActorName:       user.DisplayName,
		SourceMessageID: msg.MessageID,
		Remark:          cmd.Remark,
		CreatedAt:       now,
	})
	if err != nil {
		return err
	}
	b.sendBillForRecordAsync(ctx, msg.Chat.ID, msg.MessageID, recordID, now)
	return nil
}

func (b *Bot) handleSetting(ctx context.Context, msg telegram.Message, user storage.User, cmd settingCommand, now time.Time) error {
	if ok, err := b.canUseLedger(ctx, msg.Chat.ID, user.ID); err != nil {
		return err
	} else if !ok {
		_, err := b.tg.SendMessage(ctx, msg.Chat.ID, "没有设置权限。", map[string]any{"reply_to_message_id": msg.MessageID})
		return err
	}
	switch cmd.Kind {
	case "fee":
		rate := formatRat(cmd.Value, 4)
		if err := b.store.SetGroupFeeRate(ctx, msg.Chat.ID, rate, now); err != nil {
			return err
		}
		_, err := b.tg.SendMessage(ctx, msg.Chat.ID, "操作成功：设置费率="+rate+"%", map[string]any{"reply_to_message_id": msg.MessageID})
		return err
	case "exchange_rate":
		if cmd.Value == nil || cmd.Value.Sign() <= 0 {
			_, err := b.tg.SendMessage(ctx, msg.Chat.ID, "汇率必须大于0。", map[string]any{"reply_to_message_id": msg.MessageID})
			return err
		}
		rate := formatRat(cmd.Value, 8)
		if err := b.store.SetGroupExchangeRate(ctx, msg.Chat.ID, rate, now); err != nil {
			return err
		}
		_, err := b.tg.SendMessage(ctx, msg.Chat.ID, "操作成功：设置汇率="+rate, map[string]any{"reply_to_message_id": msg.MessageID})
		return err
	case "cutoff":
		if err := b.store.SetGroupCutoffHour(ctx, msg.Chat.ID, cmd.CutoffHour, now); err != nil {
			return err
		}
		_, err := b.tg.SendMessage(ctx, msg.Chat.ID, fmt.Sprintf("操作成功：日切时间=%02d:00", cmd.CutoffHour), map[string]any{"reply_to_message_id": msg.MessageID})
		return err
	default:
		return nil
	}
}

func (b *Bot) handleUndo(ctx context.Context, msg telegram.Message, user storage.User, kind string, now time.Time) error {
	if msg.ReplyTo == nil {
		_, err := b.tg.SendMessage(ctx, msg.Chat.ID, "请回复要撤销的原始加账消息或机器人账单回执。", map[string]any{"reply_to_message_id": msg.MessageID})
		return err
	}
	record, ok, err := b.store.FindRecordByMessage(ctx, msg.Chat.ID, msg.ReplyTo.MessageID)
	if err != nil {
		return err
	}
	if !ok {
		_, err := b.tg.SendMessage(ctx, msg.Chat.ID, "没有找到可撤销的记录。", map[string]any{"reply_to_message_id": msg.MessageID})
		return err
	}
	if record.ActorUserID != user.ID {
		manager, err := b.canManageGroup(ctx, msg.Chat.ID, user.ID)
		if err != nil {
			return err
		}
		if !manager {
			_, err := b.tg.SendMessage(ctx, msg.Chat.ID, "只能撤销自己录入的记录。", map[string]any{"reply_to_message_id": msg.MessageID})
			return err
		}
	}
	deleted, err := b.store.SoftDeleteRecord(ctx, msg.Chat.ID, record.ID, now, kind)
	if err != nil {
		return err
	}
	if !deleted {
		_, err := b.tg.SendMessage(ctx, msg.Chat.ID, "撤销失败，记录类型不匹配或已撤销。", map[string]any{"reply_to_message_id": msg.MessageID})
		return err
	}
	prefix := "已撤销"
	if record.Kind == "deposit" {
		prefix = "已撤销入款"
	} else if record.Kind == "payout" {
		prefix = "已撤销下发"
	}
	return b.sendBill(ctx, msg.Chat.ID, msg.MessageID, now, prefix)
}

func (b *Bot) handleOperatorCommand(ctx context.Context, msg telegram.Message, user storage.User, text string, now time.Time) error {
	if ok, err := b.canManageGroup(ctx, msg.Chat.ID, user.ID); err != nil {
		return err
	} else if !ok {
		_, err := b.tg.SendMessage(ctx, msg.Chat.ID, "只有宿主或本群最高权限可以管理操作员。", map[string]any{"reply_to_message_id": msg.MessageID})
		return err
	}
	add := strings.HasPrefix(text, "添加操作员") || strings.HasPrefix(text, "设置操作人")
	remove := strings.HasPrefix(text, "删除操作员") || strings.HasPrefix(text, "移除操作人") || strings.HasPrefix(text, "删除操作人")
	if !add && !remove {
		return nil
	}
	targets, missing, err := b.operatorTargets(ctx, msg, text)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		_, err := b.tg.SendMessage(ctx, msg.Chat.ID, "请回复对方消息，或输入 @用户名。", map[string]any{"reply_to_message_id": msg.MessageID})
		return err
	}
	var changed []string
	for _, target := range targets {
		if add {
			if err := b.store.TouchUser(ctx, msg.Chat.ID, target, now); err != nil {
				return err
			}
			if err := b.store.AddOperator(ctx, msg.Chat.ID, target, user.ID, now); err != nil {
				return err
			}
			b.operatorCache.Delete(formatID(msg.Chat.ID) + ":" + formatID(target.ID))
			changed = append(changed, target.DisplayName)
			continue
		}
		removed, err := b.store.RemoveOperator(ctx, msg.Chat.ID, target.ID)
		if err != nil {
			return err
		}
		if removed {
			b.operatorCache.Delete(formatID(msg.Chat.ID) + ":" + formatID(target.ID))
			changed = append(changed, target.DisplayName)
		}
	}
	action := "添加"
	if remove {
		action = "删除"
	}
	textOut := fmt.Sprintf("操作成功：已%s操作员 %s", action, strings.Join(changed, "、"))
	if len(changed) == 0 {
		textOut = "没有操作员被" + action + "。"
	}
	if len(missing) > 0 {
		textOut += "\n未找到：" + strings.Join(missing, "、") + "\n对方需要先在群里发过消息，或直接回复对方消息操作。"
	}
	_, err = b.tg.SendMessage(ctx, msg.Chat.ID, textOut, map[string]any{"reply_to_message_id": msg.MessageID})
	return err
}

func (b *Bot) handleListOperators(ctx context.Context, msg telegram.Message) error {
	operators, err := b.store.ListOperators(ctx, msg.Chat.ID)
	if err != nil {
		return err
	}
	if len(operators) == 0 {
		_, err := b.tg.SendMessage(ctx, msg.Chat.ID, "当前没有操作员。", map[string]any{"reply_to_message_id": msg.MessageID})
		return err
	}
	var out strings.Builder
	out.WriteString("当前操作员：\n")
	for _, op := range operators {
		name := op.DisplayName
		if name == "" && op.Username != "" {
			name = "@" + op.Username
		}
		if name == "" {
			name = formatID(op.UserID)
		}
		role := "操作员"
		if op.Role == "owner" {
			role = "最高权限"
		}
		out.WriteString(role)
		out.WriteString("：")
		out.WriteString(name)
		out.WriteString("（")
		out.WriteString(formatID(op.UserID))
		out.WriteString("）\n")
	}
	_, err = b.tg.SendMessage(ctx, msg.Chat.ID, strings.TrimSpace(out.String()), map[string]any{"reply_to_message_id": msg.MessageID})
	return err
}

func (b *Bot) operatorTargets(ctx context.Context, msg telegram.Message, text string) ([]storage.User, []string, error) {
	seen := make(map[int64]struct{})
	var targets []storage.User
	if msg.ReplyTo != nil && msg.ReplyTo.From != nil {
		user := userFromTelegram(*msg.ReplyTo.From)
		targets = append(targets, user)
		seen[user.ID] = struct{}{}
	}
	var missing []string
	for _, username := range parseMentions(text) {
		user, ok, err := b.store.FindUserByUsername(ctx, msg.Chat.ID, username)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			missing = append(missing, "@"+username)
			continue
		}
		if _, exists := seen[user.ID]; exists {
			continue
		}
		seen[user.ID] = struct{}{}
		targets = append(targets, user)
	}
	return targets, missing, nil
}

func (b *Bot) sendBillForRecordAsync(ctx context.Context, chatID, replyTo, recordID int64, now time.Time) {
	key := "send:" + strconv.FormatInt(chatID, 10)
	b.dispatcher.Submit(ctx, key, b.notifyPool, func(sendCtx context.Context) {
		msg, err := b.sendBillMessage(sendCtx, chatID, replyTo, now, "")
		if err != nil {
			log.Printf("send bill for record %d: %v", recordID, err)
			return
		}
		if err := b.store.SetRecordBotMessage(sendCtx, recordID, msg.MessageID); err != nil {
			log.Printf("set record bot message %d: %v", recordID, err)
		}
	})
}

func (b *Bot) sendBill(ctx context.Context, chatID, replyTo int64, now time.Time, prefix string) error {
	_, err := b.sendBillMessage(ctx, chatID, replyTo, now, prefix)
	return err
}

func (b *Bot) sendBillMessage(ctx context.Context, chatID, replyTo int64, now time.Time, prefix string) (telegram.Message, error) {
	group, err := b.store.GetGroup(ctx, chatID)
	if err != nil {
		return telegram.Message{}, err
	}
	dayKey := businessDayKey(now, group.CutoffHour)
	records, err := b.store.ListRecordsForDay(ctx, chatID, dayKey)
	if err != nil {
		return telegram.Message{}, err
	}
	text := buildBillText(group, records, b.loc, prefix)
	opts := map[string]any{
		"parse_mode": "HTML",
	}
	if url := b.publicBillURL(chatID, dayKey); url != "" {
		opts["reply_markup"] = telegram.InlineKeyboardMarkup{InlineKeyboard: [][]telegram.InlineKeyboardButton{
			{{Text: "🌍 完整账单", URL: url}},
		}}
	}
	return b.tg.SendMessage(ctx, chatID, text, opts)
}

func (b *Bot) publicBillURL(chatID int64, dayKey string) string {
	if b.cfg.PublicBillBaseURL == "" || dayKey == "" {
		return ""
	}
	shortDay := strings.ReplaceAll(dayKey, "-", "")
	return fmt.Sprintf("%s/b/%d/%s", b.cfg.PublicBillBaseURL, chatID, shortDay)
}

func (b *Bot) isGroupOperator(ctx context.Context, chatID, userID int64) (bool, error) {
	key := formatID(chatID) + ":" + formatID(userID)
	if value, ok := b.operatorCache.Get(key); ok {
		return value, nil
	}
	value, err := b.store.IsOperator(ctx, chatID, userID)
	if err != nil {
		return false, err
	}
	b.operatorCache.Set(key, value)
	return value, nil
}

func (b *Bot) canUseLedger(ctx context.Context, chatID, userID int64) (bool, error) {
	if b.isRoot(userID) {
		return true, nil
	}
	return b.isGroupOperator(ctx, chatID, userID)
}

func (b *Bot) canUseLedgerWithGroup(ctx context.Context, group storage.Group, userID int64) (bool, error) {
	if group.AllMembersCanRecord || b.isRoot(userID) {
		return true, nil
	}
	return b.isGroupOperator(ctx, group.ChatID, userID)
}

func (b *Bot) canManageGroup(ctx context.Context, chatID, userID int64) (bool, error) {
	if b.isRoot(userID) {
		return true, nil
	}
	return b.store.IsOwner(ctx, chatID, userID)
}

func feeFactor(feeRate *big.Rat) *big.Rat {
	factor := big.NewRat(100, 1)
	factor.Sub(factor, feeRate)
	factor.Quo(factor, big.NewRat(100, 1))
	return factor
}

func businessDayKey(now time.Time, cutoffHour int) string {
	if cutoffHour < 0 || cutoffHour > 23 {
		cutoffHour = 0
	}
	shifted := now.Add(-time.Duration(cutoffHour) * time.Hour)
	return shifted.Format("2006-01-02")
}
