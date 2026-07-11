package bot

import (
	"context"
	"fmt"
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
	title := "确认清除今日账单？"
	desc := "只会清除当前群当前业务日的账单，群配置、汇率、费率不变。"
	if scope == "all" {
		title = "确认清除全部账单？"
		desc = "会清除当前群所有账单记录，群配置、汇率、费率不变。"
	}
	keyboard := telegram.InlineKeyboardMarkup{InlineKeyboard: [][]telegram.InlineKeyboardButton{
		{{Text: "确认清除", CallbackData: "clear:" + scope}, {Text: "取消", CallbackData: "clear:cancel"}},
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
	scope := strings.TrimPrefix(cb.Data, "clear:")
	if scope != "today" && scope != "all" {
		return b.tg.AnswerCallback(ctx, cb.ID, "操作无效")
	}
	if ok, err := b.canUseLedger(ctx, cb.Message.Chat.ID, cb.From.ID); err != nil {
		return err
	} else if !ok {
		return b.tg.AnswerCallback(ctx, cb.ID, "没有清除账单权限")
	}
	now := time.Now().In(b.loc)
	var count int64
	var err error
	var clearedDayKey string
	if scope == "today" {
		group, getErr := b.getGroupCached(ctx, cb.Message.Chat.ID)
		if getErr != nil {
			return getErr
		}
		clearedDayKey = businessDayKey(now, group.CutoffHour)
		doneDelete := measurePerfStage(ctx, "db_record_delete")
		count, err = b.store.SoftDeleteRecordsForDay(ctx, cb.Message.Chat.ID, clearedDayKey, now)
		doneDelete()
	} else {
		doneDelete := measurePerfStage(ctx, "db_record_delete")
		count, err = b.store.SoftDeleteAllRecords(ctx, cb.Message.Chat.ID, now)
		doneDelete()
	}
	if err != nil {
		return err
	}
	if scope == "today" {
		b.invalidateBillSummaryCache(cb.Message.Chat.ID, clearedDayKey)
	} else {
		b.clearBillSummaryCache()
	}
	if err := b.tg.AnswerCallback(ctx, cb.ID, "已清除"); err != nil {
		return err
	}
	text := fmt.Sprintf("清除完成：已清除 %d 条记录。", count)
	if scope == "today" {
		return b.sendBill(ctx, cb.Message.Chat.ID, cb.Message.MessageID, now, text)
	}
	return b.enqueueLedgerSuccessText(ctx, sendPriorityNormal, "clear_ledger_done", cb.Message.Chat.ID, cb.Message.MessageID, text, nil, now)
}
