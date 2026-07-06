package bot

import (
	"context"
	"fmt"
	"log"
	"math/big"
	"strings"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/telegram"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/tron"
)

func (b *Bot) handleTRXAddressQuery(ctx context.Context, msg telegram.Message, address string) error {
	chatID := msg.Chat.ID
	replyTo := msg.MessageID
	b.queryPool.Submit(func(jobCtx context.Context) {
		text := b.queryTRXAddressText(jobCtx, address)
		if _, err := b.tg.SendMessage(jobCtx, chatID, text, map[string]any{
			"reply_to_message_id": replyTo,
		}); err != nil {
			log.Printf("send trx address query: %v", err)
		}
	})
	return nil
}

func (b *Bot) queryTRXAddressText(ctx context.Context, address string) string {
	account, accountErr := b.tron.FetchAccount(ctx, address, b.cfg.USDTContract)
	transfers, transferErr := b.tron.FetchAddressUSDTTransfers(ctx, address, b.cfg.USDTContract, 5)
	if accountErr != nil && transferErr != nil {
		return "TRX 地址查询失败：" + accountErr.Error()
	}
	var out strings.Builder
	out.WriteString("TRX 地址查询\n")
	out.WriteString("地址：")
	out.WriteString(address)
	out.WriteByte('\n')
	if accountErr == nil {
		out.WriteString("USDT：")
		out.WriteString(formatTokenAmount(account.USDTBalance, firstPositive(account.USDTDecimals, 6), 2))
		out.WriteString("\nTRX：")
		out.WriteString(formatTokenAmount(account.BalanceSun, 6, 6))
		if account.CreatedAt > 0 {
			out.WriteString("\n创建时间：")
			out.WriteString(formatMilliTime(account.CreatedAt, b.loc))
		}
		latest := account.LatestOperationAt
		if latest == 0 && len(transfers) > 0 {
			latest = transfers[0].BlockTimestamp
		}
		if latest > 0 {
			out.WriteString("\n活跃时间：")
			out.WriteString(formatMilliTime(latest, b.loc))
		}
		out.WriteString("\n交易统计：入 ")
		out.WriteString(formatInt(int(account.TransactionsIn)))
		out.WriteString(" / 出 ")
		out.WriteString(formatInt(int(account.TransactionsOut)))
		out.WriteString(" / 总 ")
		out.WriteString(formatInt(int(account.TotalTransactionCount)))
	} else {
		out.WriteString("账户详情：暂不可用，")
		out.WriteString(accountErr.Error())
	}
	if transferErr == nil && len(transfers) > 0 {
		out.WriteString("\n\n最近 USDT 流水：")
		for i, transfer := range transfers {
			out.WriteByte('\n')
			out.WriteString(formatInt(i + 1))
			out.WriteString(". ")
			out.WriteString(formatTransferLine(address, transfer, b.loc))
		}
	} else if transferErr != nil {
		out.WriteString("\n\n最近 USDT 流水：暂不可用，")
		out.WriteString(transferErr.Error())
	}
	return out.String()
}

func formatTransferLine(address string, transfer tron.Transfer, loc *time.Location) string {
	direction := "收入"
	peer := transfer.From
	sign := "+"
	if strings.EqualFold(transfer.From, address) {
		direction = "支出"
		peer = transfer.To
		sign = "-"
	}
	amount := formatTokenAmount(transfer.Value, firstPositive(transfer.TokenDecimals, 6), 2)
	return fmt.Sprintf("%s %s%sU  对方:%s  %s  %s",
		direction,
		sign,
		amount,
		shortAddress(peer),
		formatMilliTime(transfer.BlockTimestamp, loc),
		shortHash(transfer.Hash),
	)
}

func formatTokenAmount(raw string, decimals int, precision int) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "0"
	}
	value := new(big.Int)
	if _, ok := value.SetString(raw, 10); !ok {
		if rat, ok := new(big.Rat).SetString(raw); ok {
			return formatRat(rat, precision)
		}
		return raw
	}
	denominator := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	rat := new(big.Rat).SetFrac(value, denominator)
	return formatRat(rat, precision)
}

func formatMilliTime(ms int64, loc *time.Location) string {
	if ms <= 0 {
		return "-"
	}
	return time.UnixMilli(ms).In(loc).Format("2006-01-02 15:04:05")
}

func firstPositive(values ...int) int {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

func shortAddress(address string) string {
	if len(address) <= 12 {
		return address
	}
	return address[:6] + "..." + address[len(address)-6:]
}
