package bot

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/adminauth"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/telegram"
)

const ledgerClearTicketTTL = 60 * time.Second

func (b *Bot) handleClearLedgerRequest(ctx context.Context, msg telegram.Message, user storage.User, scope string) error {
	if ok, err := b.canUseLedger(ctx, msg.Chat.ID, user.ID); err != nil {
		return err
	} else if !ok {
		return b.enqueueLedgerTraceText(ctx, sendPriorityNormal, "clear_ledger_denied", msg.Chat.ID, msg.MessageID, "没有清除账单权限。", nil, time.Now().In(b.loc))
	}
	if scope != "current" {
		return nil
	}
	now := telegramExecutionTime(b.loc)
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
	token, err := b.ledgerClearToken(ctx, msg.Chat.ID, user.ID)
	if err != nil {
		return err
	}
	if err := b.store.CreateLedgerClearTicket(ctx, storage.LedgerClearTicket{
		TokenHash:             adminauth.HashToken(token),
		ChatID:                msg.Chat.ID,
		RequestedByUserID:     user.ID,
		DayKey:                group.ActiveDayKey,
		ActivePeriodStartedAt: group.ActivePeriodStartedAt,
		ExpiresAt:             now.Add(ledgerClearTicketTTL),
		CreatedAt:             now,
	}); err != nil {
		return err
	}
	title := "确认清除当前账期？"
	desc := fmt.Sprintf("账期起止：%s 至 %s\n记录数：%d 条\n此操作不可恢复，只清除当前账期，群配置、汇率和费率不变。",
		formatLedgerPeriodTime(group.ActivePeriodStartedAt, b.loc), formatLedgerPeriodTime(now, b.loc), count)
	keyboard := telegram.InlineKeyboardMarkup{InlineKeyboard: [][]telegram.InlineKeyboardButton{
		{{Text: "确认清除当前账期", CallbackData: ledgerClearCallbackData(token)}, {Text: "取消", CallbackData: "clear:cancel"}},
	}}
	return b.enqueueLedgerSuccessText(ctx, sendPriorityNormal, "clear_ledger_confirm", msg.Chat.ID, msg.MessageID, title+"\n"+desc, map[string]any{
		"reply_markup": keyboard,
	}, time.Now().In(b.loc))
}

func (b *Bot) ledgerClearToken(ctx context.Context, chatID, userID int64) (string, error) {
	updateID, _ := ctx.Value(telegramUpdateIDContextKey{}).(int64)
	if updateID <= 0 {
		return adminauth.NewToken()
	}
	mac := hmac.New(sha256.New, []byte(b.cfg.TelegramBotToken))
	_, _ = mac.Write([]byte("ledger-clear:" + strconv.FormatInt(updateID, 10) + ":" +
		strconv.FormatInt(chatID, 10) + ":" + strconv.FormatInt(userID, 10)))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func ledgerClearCallbackData(token string) string {
	return "clear:t:" + strings.TrimSpace(token)
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
	parts := strings.SplitN(cb.Data, ":", 3)
	if len(parts) != 3 || parts[0] != "clear" || parts[1] != "t" || strings.TrimSpace(parts[2]) == "" {
		return b.tg.AnswerCallback(ctx, cb.ID, "操作无效")
	}
	now := time.Now().In(b.loc)
	tokenHash := adminauth.HashToken(parts[2])
	ticket, ok, err := b.store.GetLedgerClearTicket(ctx, tokenHash)
	if err != nil {
		return err
	}
	if !ok || ticket.ConsumedAt != nil {
		return b.tg.AnswerCallback(ctx, cb.ID, "纭宸插け鏁?")
	}
	if ticket.ChatID != cb.Message.Chat.ID || ticket.RequestedByUserID != cb.From.ID {
		return b.tg.AnswerCallback(ctx, cb.ID, "杩欎笉鏄綘鐨勬竻璐︾‘璁?")
	}
	if !ticket.ExpiresAt.After(now) {
		return b.tg.AnswerCallback(ctx, cb.ID, "纭宸茶繃鏈燂紝璇烽噸鏂板彂閫佹竻璐︽寚浠?")
	}
	allowed, err := b.canUseLedgerFresh(ctx, cb.Message.Chat.ID, cb.From.ID)
	if err != nil {
		return err
	}
	if !allowed {
		return b.tg.AnswerCallback(ctx, cb.ID, "娌℃湁娓呴櫎璐﹀崟鏉冮檺")
	}
	group, err := b.store.GetGroup(ctx, cb.Message.Chat.ID)
	if err != nil {
		return err
	}
	if !groupAccountingActive(group, now) || group.ActivePeriodStartedAt.IsZero() ||
		currentLedgerDayKey(group, now) != ticket.DayKey ||
		!group.ActivePeriodStartedAt.Equal(ticket.ActivePeriodStartedAt) {
		return b.tg.AnswerCallback(ctx, cb.ID, "璐︽湡宸插彉鍖栵紝璇烽噸鏂扮‘璁?")
	}
	result, err := b.store.ConsumeLedgerClearTicketAndDelete(ctx, tokenHash, cb.Message.Chat.ID, cb.From.ID,
		b.perms.HasGlobalLedgerAccess(cb.From.ID), group, now)
	if err != nil {
		return err
	}
	switch result.Status {
	case storage.LedgerClearTicketWrongUser:
		return b.tg.AnswerCallback(ctx, cb.ID, "这不是你的清账确认")
	case storage.LedgerClearTicketExpired:
		return b.tg.AnswerCallback(ctx, cb.ID, "确认已过期，请重新发送清账指令")
	case storage.LedgerClearTicketPeriodChanged:
		return b.tg.AnswerCallback(ctx, cb.ID, "账期已变化，请重新确认")
	case storage.LedgerClearTicketPermissionDenied:
		return b.tg.AnswerCallback(ctx, cb.ID, "没有清除账单权限。")
	case storage.LedgerClearTicketApplied:
		// Continue below.
	default:
		return b.tg.AnswerCallback(ctx, cb.ID, "确认已失效")
	}
	b.invalidateGroupCache(cb.Message.Chat.ID)
	b.invalidateBillSummaryCache(cb.Message.Chat.ID, result.DayKey)
	if err := b.tg.AnswerCallback(ctx, cb.ID, "已清除"); err != nil {
		return err
	}
	text := fmt.Sprintf("清除完成：已清除 %d 条记录。", result.DeletedCount)
	return b.sendBill(ctx, cb.Message.Chat.ID, cb.Message.MessageID, now, text)
}

func formatLedgerPeriodTime(value time.Time, loc *time.Location) string {
	if loc != nil {
		value = value.In(loc)
	}
	return value.Format("2006-01-02 15:04:05")
}
