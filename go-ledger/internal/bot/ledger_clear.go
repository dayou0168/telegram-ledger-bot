package bot

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/telegram"
)

func (b *Bot) handleClearLedgerRequest(ctx context.Context, msg telegram.Message, user storage.User, scope string) error {
	if ok, err := b.canUseLedger(ctx, msg.Chat.ID, user.ID); err != nil {
		return err
	} else if !ok {
		return b.enqueueLedgerTraceText(ctx, sendPriorityNormal, "clear_ledger_denied", msg.Chat.ID, msg.MessageID, "没有清除账单权限。", nil, time.Now().In(b.loc))
	}
	if scope != "current" {
		return nil
	}
	now := time.Now().In(b.loc)
	group, err := b.getGroupCached(ctx, msg.Chat.ID)
	if err != nil {
		return err
	}
	if !groupAccountingActive(group, now) || group.ActivePeriodStartedAt.IsZero() {
		return b.enqueueLedgerTraceText(ctx, sendPriorityNormal, "clear_ledger_inactive", msg.Chat.ID, msg.MessageID, ledgerInactiveText, nil, now)
	}
	count, err := b.store.CountRecordsForPeriod(ctx, msg.Chat.ID, group.ActiveDayKey, group.ActivePeriodStartedAt)
	if err != nil {
		return err
	}
	title := "确认清除当前账期？"
	desc := fmt.Sprintf("账期起止：%s 至 %s\n记录数：%d 条\n此操作不可恢复，只清除当前账期，群配置、汇率和费率不变。",
		formatLedgerPeriodTime(group.ActivePeriodStartedAt, b.loc), formatLedgerPeriodTime(now, b.loc), count)
	keyboard := telegram.InlineKeyboardMarkup{InlineKeyboard: [][]telegram.InlineKeyboardButton{
		{{Text: "确认清除当前账期", CallbackData: fmt.Sprintf("clear:current:%d", group.ActivePeriodStartedAt.Unix())}, {Text: "取消", CallbackData: "clear:cancel"}},
	}}
	return b.enqueueLedgerSuccessText(ctx, sendPriorityNormal, "clear_ledger_confirm", msg.Chat.ID, msg.MessageID, title+"\n"+desc, map[string]any{
		"reply_markup": keyboard,
	}, time.Now().In(b.loc))
}

func (b *Bot) handleClearLedgerCallback(ctx context.Context, cb telegram.CallbackQuery) error {
	if cb.Message == nil {
		return b.tg.AnswerCallback(ctx, cb.ID, "")
	}
	if cb.Data == "clear:cancel" {
		if err := b.tg.AnswerCallback(ctx, cb.ID, "已取消"); err != nil {
			return err
		}
		return b.enqueueLedgerSuccessText(ctx, sendPriorityNormal, "clear_ledger_cancel", cb.Message.Chat.ID, cb.Message.MessageID, "已取消清除账单。", nil, time.Now().In(b.loc))
	}
	parts := strings.Split(cb.Data, ":")
	if len(parts) != 3 || parts[0] != "clear" || parts[1] != "current" {
		return b.tg.AnswerCallback(ctx, cb.ID, "操作无效")
	}
	confirmedStart, parseErr := strconv.ParseInt(parts[2], 10, 64)
	if parseErr != nil || confirmedStart <= 0 {
		return b.tg.AnswerCallback(ctx, cb.ID, "操作无效")
	}
	if ok, err := b.canUseLedger(ctx, cb.Message.Chat.ID, cb.From.ID); err != nil {
		return err
	} else if !ok {
		return b.tg.AnswerCallback(ctx, cb.ID, "没有清除账单权限")
	}
	now := time.Now().In(b.loc)
	group, err := b.getGroupCached(ctx, cb.Message.Chat.ID)
	if err != nil {
		return err
	}
	if !groupAccountingActive(group, now) || group.ActivePeriodStartedAt.IsZero() {
		return b.tg.AnswerCallback(ctx, cb.ID, "当前没有有效账期")
	}
	if group.ActivePeriodStartedAt.Unix() != confirmedStart {
		return b.tg.AnswerCallback(ctx, cb.ID, "账期已变化，请重新确认")
	}
	clearedDayKey := group.ActiveDayKey
	doneDelete := measurePerfStage(ctx, "db_record_delete")
	count, err := b.store.SoftDeleteRecordsForPeriod(ctx, cb.Message.Chat.ID, clearedDayKey, group.ActivePeriodStartedAt, now)
	doneDelete()
	if err != nil {
		return err
	}
	b.invalidateBillSummaryCache(cb.Message.Chat.ID, clearedDayKey)
	if err := b.tg.AnswerCallback(ctx, cb.ID, "已清除"); err != nil {
		return err
	}
	text := fmt.Sprintf("清除完成：已清除 %d 条记录。", count)
	return b.sendBill(ctx, cb.Message.Chat.ID, cb.Message.MessageID, now, text)
}

func formatLedgerPeriodTime(value time.Time, loc *time.Location) string {
	if loc != nil {
		value = value.In(loc)
	}
	return value.Format("2006-01-02 15:04:05")
}
