package bot

import (
	"context"
	"fmt"
	"html"
	"log"
	"math/big"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/p2p"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/telegram"
)

type rateBookState struct {
	mu        sync.RWMutex
	entries   []p2p.OrderBookEntry
	updatedAt time.Time
	lastError string
}

type cachedRateBook struct {
	Entries   []p2p.OrderBookEntry
	UpdatedAt time.Time
	LastError string
	Stale     bool
}

func (b *Bot) rateScheduler(ctx context.Context) {
	if b.cfg.P2PRefreshEvery <= 0 {
		return
	}
	b.ratePool.Submit(func(jobCtx context.Context) {
		if _, err := b.refreshRateBook(jobCtx); err != nil {
			log.Printf("refresh p2p rates: %v", err)
		}
	})
	ticker := time.NewTicker(b.cfg.P2PRefreshEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.ratePool.Submit(func(jobCtx context.Context) {
				if _, err := b.refreshRateBook(jobCtx); err != nil {
					log.Printf("refresh p2p rates: %v", err)
				}
			})
		}
	}
}

func (b *Bot) handleZ0(ctx context.Context, msg telegram.Message) error {
	chatID := msg.Chat.ID
	now := time.Now().In(b.loc)
	book, err := b.rateBook(now)
	text := ""
	if err != nil {
		text = "Z0 查询失败：" + err.Error()
	} else {
		text = formatZ0Book(book, b.loc)
	}
	return b.enqueueReliableText(ctx, sendPriorityNormal, "z0_result", messageScopedDedupe("z0_result", chatID, msg.MessageID), chatID, text, map[string]any{"parse_mode": "HTML"}, reliableMessageRef{}, now)
}

func (b *Bot) handleZRateSetting(ctx context.Context, msg telegram.Message, user storage.User, cmd zRateCommand, now time.Time) error {
	if ok, err := b.canUseLedger(ctx, msg.Chat.ID, user.ID); err != nil {
		return err
	} else if !ok {
		return b.enqueueLedgerTraceText(ctx, sendPriorityNormal, "zrate_denied", msg.Chat.ID, msg.MessageID, ledgerPermissionDeniedText, nil, now)
	}
	chatID := msg.Chat.ID
	book, err := b.rateBook(now)
	if err != nil {
		return b.enqueueReplyText(ctx, sendPriorityNormal, "zrate_result", chatID, msg.MessageID, "设置失败：实时汇率暂不可用："+err.Error(), nil, now)
	}
	if cmd.Rank < 1 || cmd.Rank > len(book.Entries) {
		return b.enqueueReplyText(ctx, sendPriorityNormal, "zrate_result", chatID, msg.MessageID, "设置失败：没有这个 Z 档位。", nil, now)
	}
	base := parseRat(book.Entries[cmd.Rank-1].Price)
	if base == nil {
		return b.enqueueReplyText(ctx, sendPriorityNormal, "zrate_result", chatID, msg.MessageID, "设置失败：档位价格格式异常。", nil, now)
	}
	rate := new(big.Rat).Add(base, cmd.Offset)
	if rate.Sign() <= 0 {
		return b.enqueueReplyText(ctx, sendPriorityNormal, "zrate_result", chatID, msg.MessageID, "设置失败：偏移后的汇率必须大于0。", nil, now)
	}
	rateText := formatRat(rate, 8)
	source := p2pMethodLabel(b.cfg.P2PTradeMethods)
	offset := formatSigned(cmd.Offset)
	if err := b.store.SetGroupRealtimeExchangeRate(ctx, chatID, rateText, source, cmd.Rank, offset, now); err != nil {
		log.Printf("set z rate: %v", err)
		return b.enqueueReplyText(ctx, sendPriorityNormal, "zrate_result", chatID, msg.MessageID, "设置失败：数据库写入失败。", nil, now)
	}
	b.invalidateGroupCache(chatID)
	text := fmt.Sprintf("操作成功：Z%d 基准%s，偏移%s，当前汇率%s",
		cmd.Rank,
		formatRat(base, 8),
		formatSigned(cmd.Offset),
		rateText,
	)
	if book.UpdatedAt.IsZero() {
		text += "\n实时汇率缓存：尚未记录更新时间"
	} else {
		text += "\n实时汇率缓存：" + rateBookUpdatedLabel(book.UpdatedAt, b.loc)
	}
	if book.Stale {
		text += "，数据可能陈旧"
	}
	return b.enqueueReliableText(ctx, sendPriorityNormal, "zrate_result", messageScopedDedupe("zrate_result", chatID, msg.MessageID), chatID, text, nil, reliableMessageRef{}, now)
}

func p2pMethodLabel(methods []string) string {
	method := ""
	if len(methods) > 0 {
		method = strings.ToLower(strings.TrimSpace(methods[0]))
	}
	switch method {
	case "alipay", "ali_pay", "ali-pay":
		return "支付宝"
	case "bank", "bankcard", "bank_card":
		return "银行卡"
	case "wechat", "wxpay", "wechatpay", "wx_pay":
		return "微信"
	default:
		if method == "" {
			return "支付宝"
		}
		return method
	}
}

func (b *Bot) rateBook(now time.Time) (cachedRateBook, error) {
	b.rateBookState.mu.RLock()
	entries := cloneOrderBookEntries(b.rateBookState.entries)
	updatedAt := b.rateBookState.updatedAt
	lastError := b.rateBookState.lastError
	b.rateBookState.mu.RUnlock()
	if len(entries) == 0 {
		return cachedRateBook{}, fmt.Errorf("实时汇率缓存尚未就绪，请稍后再试")
	}
	return cachedRateBook{
		Entries:   entries,
		UpdatedAt: updatedAt,
		LastError: lastError,
		Stale:     rateBookStale(now, updatedAt, lastError, b.cfg.P2PRefreshEvery),
	}, nil
}

func (b *Bot) refreshRateBook(ctx context.Context) ([]p2p.OrderBookEntry, error) {
	entries, err := b.p2p.FetchOrderBookTop(ctx, b.cfg.P2PMarket, b.cfg.P2PFiatUnit, b.cfg.P2PAsset, b.cfg.P2PTradeMethods, 10)
	if err != nil {
		b.setRateBookError(err)
		return nil, err
	}
	b.setRateBookEntries(entries, time.Now().In(b.loc))
	b.rateBookCache.Set("top10", entries)
	return entries, nil
}

func (b *Bot) setRateBookEntries(entries []p2p.OrderBookEntry, now time.Time) {
	b.rateBookState.mu.Lock()
	b.rateBookState.entries = cloneOrderBookEntries(entries)
	b.rateBookState.updatedAt = now
	b.rateBookState.lastError = ""
	b.rateBookState.mu.Unlock()
}

func (b *Bot) setRateBookError(err error) {
	if err == nil {
		return
	}
	b.rateBookState.mu.Lock()
	b.rateBookState.lastError = err.Error()
	b.rateBookState.mu.Unlock()
}

func cloneOrderBookEntries(entries []p2p.OrderBookEntry) []p2p.OrderBookEntry {
	if len(entries) == 0 {
		return nil
	}
	clone := make([]p2p.OrderBookEntry, len(entries))
	copy(clone, entries)
	return clone
}

func rateBookStale(now, updatedAt time.Time, lastError string, refreshEvery time.Duration) bool {
	if !updatedAt.IsZero() && refreshEvery > 0 && now.Sub(updatedAt) > refreshEvery*2 {
		return true
	}
	return strings.TrimSpace(lastError) != ""
}

func formatZ0Book(book cachedRateBook, loc *time.Location) string {
	text := formatZ0(book.Entries)
	_ = loc
	if book.Stale {
		text += "\n状态：使用上一版缓存，数据可能陈旧"
		if book.LastError != "" {
			text += "（最近刷新失败：" + html.EscapeString(book.LastError) + "）"
		}
	}
	return strings.TrimSpace(text)
}

func rateBookUpdatedLabel(updatedAt time.Time, loc *time.Location) string {
	if loc != nil {
		updatedAt = updatedAt.In(loc)
	}
	return updatedAt.Format("2006-01-02 15:04:05")
}

func formatZ0(entries []p2p.OrderBookEntry) string {
	var out strings.Builder
	out.WriteString("<b>OKX OTC商家所有实时汇率 TOP 10</b>\n\n")
	out.WriteString("<pre>")
	for i, entry := range entries {
		rank := i + 1
		out.WriteString("Z")
		out.WriteString(strconv.Itoa(rank))
		if rank < 10 {
			out.WriteString(" :   ")
		} else {
			out.WriteString(" : ")
		}
		out.WriteString(html.EscapeString(entry.Price))
		out.WriteString("   ")
		out.WriteString(html.EscapeString(trimRunes(entry.MerchantName, 10)))
		out.WriteByte('\n')
	}
	out.WriteString("</pre>\n发送 Z1 -0.1\n或 设置汇率 Z1 -0.1 可按第1档偏移后设置汇率。")
	return strings.TrimSpace(out.String())
}

func trimRunes(text string, limit int) string {
	text = strings.TrimSpace(text)
	runes := []rune(text)
	if limit <= 0 || len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "..."
}

func formatSigned(value *big.Rat) string {
	if value == nil || value.Sign() == 0 {
		return "0"
	}
	if value.Sign() > 0 {
		return "+" + formatRat(value, 8)
	}
	return formatRat(value, 8)
}
