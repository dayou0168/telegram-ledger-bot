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

const (
	ledgerPermissionDeniedText = "没有操作权限。请管理员添加操作员。"
	ledgerInactiveText         = "当前未开始记账，请先发送“开始”。"
)

func (b *Bot) startAccounting(ctx context.Context, msg telegram.Message, user storage.User, now time.Time) error {
	if !b.perms.HasGlobalLedgerAccess(user.ID) {
		ok, err := b.isGroupOperator(ctx, msg.Chat.ID, user.ID)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
	}
	if b.isHost(user.ID) {
		if err := b.store.SetGroupOwner(ctx, msg.Chat.ID, user, now); err == nil {
			b.invalidateGroupCache(msg.Chat.ID)
			b.InvalidateAllPermissionCaches()
		}
	}
	group, err := b.getGroupCached(ctx, msg.Chat.ID)
	if err != nil {
		return err
	}
	activeDayKey := businessDayKey(now, group.CutoffHour)
	if err := b.store.SetGroupActive(ctx, msg.Chat.ID, true, activeDayKey, now); err != nil {
		return err
	}
	b.invalidateGroupCache(msg.Chat.ID)
	return b.enqueueLedgerSuccessText(ctx, sendPriorityNormal, "ledger_start_ok", msg.Chat.ID, msg.MessageID, "机器人已开启，请开始记账", nil, now)
}

func (b *Bot) stopAccounting(ctx context.Context, msg telegram.Message, user storage.User, now time.Time) error {
	if ok, err := b.canUseLedger(ctx, msg.Chat.ID, user.ID); err != nil {
		return err
	} else if !ok {
		return nil
	}
	if err := b.store.SetGroupActive(ctx, msg.Chat.ID, false, "", now); err != nil {
		return err
	}
	b.invalidateGroupCache(msg.Chat.ID)
	return b.enqueueLedgerSuccessText(ctx, sendPriorityNormal, "ledger_stop_ok", msg.Chat.ID, msg.MessageID, "已停止记账。发送“开始”可重新开启。", nil, now)
}

func (b *Bot) handleLedger(ctx context.Context, msg telegram.Message, user storage.User, cmd ledgerCommand, now time.Time) error {
	group, err := b.getGroupCached(ctx, msg.Chat.ID)
	if err != nil {
		return err
	}
	if cmd.Amount.Sign() == 0 {
		ok, err := b.guardAccountingStarted(ctx, msg, user, group, now, "ledger_zero_inactive")
		if err != nil || !ok {
			return err
		}
		return b.sendBill(ctx, msg.Chat.ID, msg.MessageID, now, "")
	}
	if ok, err := b.canUseLedgerWithGroup(ctx, group, user.ID); err != nil {
		return err
	} else if !ok {
		if !groupAccountingActive(group, now) {
			return nil
		}
		return b.enqueueLedgerTraceText(ctx, sendPriorityNormal, "ledger_record_denied", msg.Chat.ID, msg.MessageID, ledgerPermissionDeniedText, nil, now)
	}
	if !groupAccountingActive(group, now) {
		return b.enqueueLedgerTraceText(ctx, sendPriorityNormal, "ledger_inactive", msg.Chat.ID, msg.MessageID, ledgerInactiveText, nil, now)
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
	subject := ledgerSubjectFromMessage(msg, user)
	doneWrite := measurePerfStage(ctx, "db_record_write")
	recordID, err := b.store.InsertRecord(ctx, storage.Record{
		ChatID:          msg.Chat.ID,
		DayKey:          dayKey,
		Kind:            cmd.Kind,
		Currency:        currency,
		Amount:          formatAmount(effectiveAmount),
		Rate:            formatRat(rate, 8),
		FeeRate:         formatRat(feeRate, 4),
		ResultUSDT:      formatAmount(resultUSDT),
		SubjectUserID:   subject.ID,
		SubjectName:     subject.DisplayName,
		ActorUserID:     user.ID,
		ActorName:       user.DisplayName,
		SourceMessageID: msg.MessageID,
		Remark:          cmd.Remark,
		CreatedAt:       now,
	})
	doneWrite()
	if err != nil {
		return err
	}
	b.invalidateBillSummaryCache(msg.Chat.ID, dayKey)
	b.sendBillForRecordAsync(ctx, msg.Chat.ID, recordID, now)
	return nil
}

func ledgerSubjectFromMessage(msg telegram.Message, actor storage.User) storage.User {
	if msg.ReplyTo != nil && msg.ReplyTo.From != nil && msg.ReplyTo.From.ID != 0 {
		return userFromTelegram(*msg.ReplyTo.From)
	}
	return actor
}

func groupAccountingActive(group storage.Group, now time.Time) bool {
	if !group.Active {
		return false
	}
	return group.ActiveDayKey == businessDayKey(now, group.CutoffHour)
}

func (b *Bot) guardAccountingStarted(ctx context.Context, msg telegram.Message, user storage.User, group storage.Group, now time.Time, dedupeKind string) (bool, error) {
	if groupAccountingActive(group, now) {
		return true, nil
	}
	ok, err := b.canUseLedgerWithGroup(ctx, group, user.ID)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	return false, b.enqueueLedgerTraceText(ctx, sendPriorityNormal, dedupeKind, msg.Chat.ID, msg.MessageID, ledgerInactiveText, nil, now)
}

func (b *Bot) handleSetting(ctx context.Context, msg telegram.Message, user storage.User, cmd settingCommand, now time.Time) error {
	if ok, err := b.canUseLedger(ctx, msg.Chat.ID, user.ID); err != nil {
		return err
	} else if !ok {
		return b.enqueueLedgerTraceText(ctx, sendPriorityNormal, "ledger_setting_denied", msg.Chat.ID, msg.MessageID, ledgerPermissionDeniedText, nil, now)
	}
	switch cmd.Kind {
	case "fee":
		rate := formatRat(cmd.Value, 4)
		if err := b.store.SetGroupFeeRate(ctx, msg.Chat.ID, rate, now); err != nil {
			return err
		}
		b.invalidateGroupCache(msg.Chat.ID)
		return b.enqueueLedgerSuccessText(ctx, sendPriorityNormal, "ledger_setting_fee", msg.Chat.ID, msg.MessageID, "操作成功：设置费率="+rate+"%", nil, now)
	case "exchange_rate":
		if cmd.Value == nil || cmd.Value.Sign() <= 0 {
			return b.enqueueLedgerTraceText(ctx, sendPriorityNormal, "ledger_setting_rate_invalid", msg.Chat.ID, msg.MessageID, "汇率必须大于0。", nil, now)
		}
		rate := formatRat(cmd.Value, 8)
		if err := b.store.SetGroupExchangeRate(ctx, msg.Chat.ID, rate, now); err != nil {
			return err
		}
		b.invalidateGroupCache(msg.Chat.ID)
		return b.enqueueLedgerSuccessText(ctx, sendPriorityNormal, "ledger_setting_rate", msg.Chat.ID, msg.MessageID, "操作成功：设置汇率="+rate, nil, now)
	case "cutoff":
		if err := b.store.SetGroupCutoffHour(ctx, msg.Chat.ID, cmd.CutoffHour, now); err != nil {
			return err
		}
		b.invalidateGroupCache(msg.Chat.ID)
		return b.enqueueLedgerSuccessText(ctx, sendPriorityNormal, "ledger_setting_cutoff", msg.Chat.ID, msg.MessageID, fmt.Sprintf("操作成功：日切时间=%02d:00", cmd.CutoffHour), nil, now)
	default:
		return nil
	}
}

func (b *Bot) handleUndo(ctx context.Context, msg telegram.Message, user storage.User, kind string, now time.Time) error {
	if msg.ReplyTo == nil {
		return b.enqueueLedgerTraceText(ctx, sendPriorityNormal, "ledger_undo_no_reply", msg.Chat.ID, msg.MessageID, "请回复要撤销的原始加账消息或机器人账单回执。", nil, now)
	}
	doneFind := measurePerfStage(ctx, "db_record_find")
	record, ok, err := b.store.FindRecordByMessage(ctx, msg.Chat.ID, msg.ReplyTo.MessageID)
	doneFind()
	if err != nil {
		return err
	}
	if !ok {
		return b.enqueueLedgerTraceText(ctx, sendPriorityNormal, "ledger_undo_not_found", msg.Chat.ID, msg.MessageID, "没有找到可撤销的记录。", nil, now)
	}
	if record.ActorUserID != user.ID {
		manager, err := b.canManageGroup(ctx, msg.Chat.ID, user.ID)
		if err != nil {
			return err
		}
		if !manager {
			return b.enqueueLedgerTraceText(ctx, sendPriorityNormal, "ledger_undo_denied", msg.Chat.ID, msg.MessageID, "只能撤销自己录入的记录。", nil, now)
		}
	}
	doneDelete := measurePerfStage(ctx, "db_record_delete")
	deleted, err := b.store.SoftDeleteRecord(ctx, msg.Chat.ID, record.ID, now, kind)
	doneDelete()
	if err != nil {
		return err
	}
	if !deleted {
		return b.enqueueLedgerTraceText(ctx, sendPriorityNormal, "ledger_undo_failed", msg.Chat.ID, msg.MessageID, "撤销失败，记录类型不匹配或已撤销。", nil, now)
	}
	b.invalidateBillSummaryCache(msg.Chat.ID, record.DayKey)
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
		return b.enqueueLedgerTraceText(ctx, sendPriorityNormal, "ledger_operator_denied", msg.Chat.ID, msg.MessageID, "只有宿主或本群最高权限可以管理操作员。", nil, now)
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
		return b.enqueueLedgerTraceText(ctx, sendPriorityNormal, "ledger_operator_no_target", msg.Chat.ID, msg.MessageID, "请回复对方消息，或输入 @用户名。", nil, now)
	}
	var changed []string
	for _, target := range targets {
		if add {
			doneTouch := measurePerfStage(ctx, "db_user_touch")
			if err := b.store.TouchUser(ctx, msg.Chat.ID, target, now); err != nil {
				doneTouch()
				return err
			}
			doneTouch()
			if err := b.store.AddOperator(ctx, msg.Chat.ID, target, user.ID, now); err != nil {
				return err
			}
			b.InvalidateLedgerPermission(msg.Chat.ID, target.ID)
			b.InvalidateLedgerPermission(msg.Chat.ID, user.ID)
			changed = append(changed, target.DisplayName)
			continue
		}
		removed, err := b.store.RemoveOperator(ctx, msg.Chat.ID, target.ID)
		if err != nil {
			return err
		}
		if removed {
			b.InvalidateLedgerPermission(msg.Chat.ID, target.ID)
			b.InvalidateLedgerPermission(msg.Chat.ID, user.ID)
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
	return b.enqueueLedgerSuccessText(ctx, sendPriorityNormal, "ledger_operator_result", msg.Chat.ID, msg.MessageID, textOut, nil, now)
}

func (b *Bot) handleListOperators(ctx context.Context, msg telegram.Message) error {
	operators, err := b.store.ListOperators(ctx, msg.Chat.ID)
	if err != nil {
		return err
	}
	if len(operators) == 0 {
		return b.enqueueLedgerSuccessText(ctx, sendPriorityNormal, "ledger_operator_list_empty", msg.Chat.ID, msg.MessageID, "当前没有操作员。", nil, time.Now().In(b.loc))
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
	return b.enqueueLedgerSuccessText(ctx, sendPriorityNormal, "ledger_operator_list", msg.Chat.ID, msg.MessageID, strings.TrimSpace(out.String()), nil, time.Now().In(b.loc))
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

func (b *Bot) sendBillForRecordAsync(ctx context.Context, chatID, recordID int64, now time.Time) {
	key := "send:" + strconv.FormatInt(chatID, 10)
	b.dispatcher.Submit(ctx, key, b.notifyPool, func(sendCtx context.Context) {
		item := storage.NotificationOutbox{
			Kind:           "ledger_bill",
			DedupeKey:      fmt.Sprintf("ledger_bill_record:%d", recordID),
			ChatID:         chatID,
			ParseMode:      "HTML",
			DisablePreview: true,
			ReferenceKind:  "ledger_record",
			ReferenceID:    recordID,
			Priority:       int(sendPriorityHigh),
		}
		inserted, err := b.store.EnqueueNotification(sendCtx, item, now)
		if err != nil {
			log.Printf("enqueue bill for record %d: %v", recordID, err)
			return
		}
		if inserted {
			b.kickNotificationOutbox()
		}
	})
}

func (b *Bot) sendBill(ctx context.Context, chatID, replyTo int64, now time.Time, prefix string) error {
	text, opts, err := b.renderBillMessageForTime(ctx, chatID, now, prefix)
	if err != nil {
		return err
	}
	return b.enqueueReliableText(ctx, sendPriorityHigh, "ledger_bill", messageScopedDedupe("ledger_bill", chatID, replyTo), chatID, text, withoutReplyOptions(opts), reliableMessageRef{}, now)
}

func (b *Bot) sendBillMessage(ctx context.Context, chatID, replyTo int64, now time.Time, prefix string) (telegram.Message, error) {
	text, opts, err := b.renderBillMessageForTime(ctx, chatID, now, prefix)
	if err != nil {
		return telegram.Message{}, err
	}
	return b.sendText(ctx, sendPriorityHigh, chatID, text, withoutReplyOptions(opts))
}

func (b *Bot) renderBillMessageForTime(ctx context.Context, chatID int64, now time.Time, prefix string) (string, map[string]any, error) {
	group, err := b.getGroupCached(ctx, chatID)
	if err != nil {
		return "", nil, err
	}
	dayKey := businessDayKey(now, group.CutoffHour)
	return b.renderBillMessageWithGroup(ctx, group, dayKey, prefix)
}

func (b *Bot) renderBillMessage(ctx context.Context, chatID int64, dayKey string, prefix string) (string, map[string]any, error) {
	group, err := b.getGroupCached(ctx, chatID)
	if err != nil {
		return "", nil, err
	}
	return b.renderBillMessageWithGroup(ctx, group, dayKey, prefix)
}

func (b *Bot) renderBillMessageWithGroup(ctx context.Context, group storage.Group, dayKey string, prefix string) (string, map[string]any, error) {
	chatID := group.ChatID
	doneDB := measurePerfStage(ctx, "db_bill_summary")
	data, err := b.getBillSummaryCached(ctx, chatID, dayKey, groupBillDefaultRecordLimit)
	doneDB()
	if err != nil {
		return "", nil, err
	}
	doneRender := measurePerfStage(ctx, "bill_render")
	text := buildBillTextFromSummary(group, data, b.loc, prefix)
	doneRender()
	opts := map[string]any{
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
	}
	if url := b.publicBillURL(chatID, dayKey); url != "" {
		opts["reply_markup"] = telegram.InlineKeyboardMarkup{InlineKeyboard: [][]telegram.InlineKeyboardButton{
			{{Text: "🌍 完整账单", URL: url}},
		}}
	}
	return text, opts, nil
}

func (b *Bot) publicBillURL(chatID int64, dayKey string) string {
	if b.cfg.PublicBillBaseURL == "" || dayKey == "" {
		return ""
	}
	shortDay := strings.ReplaceAll(dayKey, "-", "")
	return fmt.Sprintf("%s/b/%d/%s", b.cfg.PublicBillBaseURL, chatID, shortDay)
}

func (b *Bot) isGroupOperator(ctx context.Context, chatID, userID int64) (bool, error) {
	key := ledgerPermissionCacheKey(chatID, userID)
	if value, ok := b.operatorCache.Get(key); ok {
		markPerfCache(ctx, "operator", true)
		return value, nil
	}
	markPerfCache(ctx, "operator", false)
	done := measurePerfStage(ctx, "permission")
	defer done()
	value, err := b.store.IsOperator(ctx, chatID, userID)
	if err != nil {
		return false, err
	}
	b.operatorCache.Set(key, value)
	return value, nil
}

func (b *Bot) canUseLedger(ctx context.Context, chatID, userID int64) (bool, error) {
	if b.perms.HasGlobalLedgerAccess(userID) {
		return true, nil
	}
	return b.isGroupOperator(ctx, chatID, userID)
}

func (b *Bot) canUseLedgerWithGroup(ctx context.Context, group storage.Group, userID int64) (bool, error) {
	if group.AllMembersCanRecord || b.perms.HasGlobalLedgerAccess(userID) {
		return true, nil
	}
	return b.isGroupOperator(ctx, group.ChatID, userID)
}

func (b *Bot) canManageGroup(ctx context.Context, chatID, userID int64) (bool, error) {
	if b.perms.CanManageAnyGroup(userID) {
		return true, nil
	}
	done := measurePerfStage(ctx, "permission")
	defer done()
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
