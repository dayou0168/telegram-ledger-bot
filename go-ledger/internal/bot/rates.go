package bot

import (
	"context"
	"fmt"
	"log"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/p2p"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/telegram"
)

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
	replyTo := msg.MessageID
	b.ratePool.Submit(func(jobCtx context.Context) {
		entries, err := b.rateBook(jobCtx)
		text := ""
		if err != nil {
			text = "Z0 查询失败：" + err.Error()
		} else {
			text = formatZ0(entries)
		}
		if _, err := b.tg.SendMessage(jobCtx, chatID, text, map[string]any{"reply_to_message_id": replyTo}); err != nil {
			log.Printf("send z0 result: %v", err)
		}
	})
	return nil
}

func (b *Bot) handleZRateSetting(ctx context.Context, msg telegram.Message, user storage.User, cmd zRateCommand, now time.Time) error {
	if ok, err := b.canUseLedger(ctx, msg.Chat.ID, user.ID); err != nil {
		return err
	} else if !ok {
		_, err := b.tg.SendMessage(ctx, msg.Chat.ID, "没有设置汇率权限。", map[string]any{"reply_to_message_id": msg.MessageID})
		return err
	}
	chatID := msg.Chat.ID
	replyTo := msg.MessageID
	b.ratePool.Submit(func(jobCtx context.Context) {
		entries, err := b.rateBook(jobCtx)
		if err != nil {
			_, _ = b.tg.SendMessage(jobCtx, chatID, "设置失败：实时汇率暂不可用："+err.Error(), map[string]any{"reply_to_message_id": replyTo})
			return
		}
		if cmd.Rank < 1 || cmd.Rank > len(entries) {
			_, _ = b.tg.SendMessage(jobCtx, chatID, "设置失败：没有这个 Z 档位。", map[string]any{"reply_to_message_id": replyTo})
			return
		}
		base := parseRat(entries[cmd.Rank-1].Price)
		if base == nil {
			_, _ = b.tg.SendMessage(jobCtx, chatID, "设置失败：档位价格格式异常。", map[string]any{"reply_to_message_id": replyTo})
			return
		}
		rate := new(big.Rat).Add(base, cmd.Offset)
		if rate.Sign() <= 0 {
			_, _ = b.tg.SendMessage(jobCtx, chatID, "设置失败：偏移后的汇率必须大于0。", map[string]any{"reply_to_message_id": replyTo})
			return
		}
		rateText := formatRat(rate, 8)
		if err := b.store.SetGroupExchangeRate(jobCtx, chatID, rateText, now); err != nil {
			log.Printf("set z rate: %v", err)
			_, _ = b.tg.SendMessage(jobCtx, chatID, "设置失败：数据库写入失败。", map[string]any{"reply_to_message_id": replyTo})
			return
		}
		text := fmt.Sprintf("操作成功：Z%d 基准%s，偏移%s，当前汇率%s",
			cmd.Rank,
			formatRat(base, 8),
			formatSigned(cmd.Offset),
			rateText,
		)
		if _, err := b.tg.SendMessage(jobCtx, chatID, text, map[string]any{"reply_to_message_id": replyTo}); err != nil {
			log.Printf("send z rate result: %v", err)
		}
	})
	return nil
}

func (b *Bot) rateBook(ctx context.Context) ([]p2p.OrderBookEntry, error) {
	if cached, ok := b.rateBookCache.Get("top10"); ok {
		return cached, nil
	}
	return b.refreshRateBook(ctx)
}

func (b *Bot) refreshRateBook(ctx context.Context) ([]p2p.OrderBookEntry, error) {
	entries, err := b.p2p.FetchOrderBookTop(ctx, b.cfg.P2PMarket, b.cfg.P2PFiatUnit, b.cfg.P2PAsset, b.cfg.P2PTradeMethods, 10)
	if err != nil {
		return nil, err
	}
	b.rateBookCache.Set("top10", entries)
	return entries, nil
}

func formatZ0(entries []p2p.OrderBookEntry) string {
	var out strings.Builder
	out.WriteString("OKX OTC商家所有实时汇率 TOP 10\n\n")
	for i, entry := range entries {
		rank := i + 1
		out.WriteString("Z")
		out.WriteString(strconv.Itoa(rank))
		out.WriteByte(' ')
		out.WriteString(entry.Price)
		out.WriteByte(' ')
		out.WriteString(entry.MerchantName)
		out.WriteByte('\n')
	}
	out.WriteString("\n发送 Z1 -0.1 或 设置汇率 Z1 -0.1 可按第1档偏移后设置汇率。")
	return strings.TrimSpace(out.String())
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
