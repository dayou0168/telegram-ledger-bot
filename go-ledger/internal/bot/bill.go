package bot

import (
	"math/big"
	"strings"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
)

func buildBillText(group storage.Group, records []storage.Record, loc *time.Location, prefix string) string {
	var deposits []storage.Record
	var payouts []storage.Record
	totalDepositCNY := big.NewRat(0, 1)
	totalDepositUSDT := big.NewRat(0, 1)
	totalPayoutUSDT := big.NewRat(0, 1)

	for _, record := range records {
		result := parseRat(record.ResultUSDT)
		if result == nil {
			result = big.NewRat(0, 1)
		}
		switch record.Kind {
		case "deposit":
			deposits = append(deposits, record)
			totalDepositUSDT.Add(totalDepositUSDT, result)
			if strings.EqualFold(record.Currency, "CNY") {
				if amount := parseRat(record.Amount); amount != nil {
					totalDepositCNY.Add(totalDepositCNY, amount)
				}
			}
		case "payout":
			payouts = append(payouts, record)
			totalPayoutUSDT.Add(totalPayoutUSDT, result)
		}
	}
	balance := new(big.Rat).Sub(totalDepositUSDT, totalPayoutUSDT)

	var out strings.Builder
	if prefix != "" {
		out.WriteString(prefix)
		out.WriteString("\n\n")
	}
	out.WriteString("今日入款（")
	out.WriteString(formatInt(len(deposits)))
	out.WriteString("笔）\n")
	for _, record := range deposits {
		out.WriteString(recordLine(record, loc))
		out.WriteByte('\n')
	}
	out.WriteString("\n今日下发（")
	out.WriteString(formatInt(len(payouts)))
	out.WriteString("笔）\n")
	for _, record := range payouts {
		out.WriteString(recordLine(record, loc))
		out.WriteByte('\n')
	}
	out.WriteString("\n总入款：")
	out.WriteString(formatAmount(totalDepositCNY))
	out.WriteString("（")
	out.WriteString(formatAmount(totalDepositUSDT))
	out.WriteString("U）\n汇率：")
	out.WriteString(group.DepositExchangeRate)
	out.WriteString("\n交易费率：")
	out.WriteString(group.FeeRate)
	out.WriteString("%\n\n应下发：")
	out.WriteString(formatAmount(totalDepositUSDT))
	out.WriteString("U\n已下发：")
	out.WriteString(formatAmount(totalPayoutUSDT))
	out.WriteString("U\n余额：")
	out.WriteString(formatAmount(balance))
	out.WriteString("U")
	return out.String()
}

func recordLine(record storage.Record, loc *time.Location) string {
	createdAt := record.CreatedAt
	if loc != nil {
		createdAt = createdAt.In(loc)
	}
	var out strings.Builder
	out.WriteString(createdAt.Format("15:04:05"))
	out.WriteByte(' ')
	out.WriteString(recordAmountExpr(record))
	if record.ActorName != "" {
		out.WriteByte(' ')
		out.WriteString(record.ActorName)
	}
	if record.Remark != "" {
		out.WriteByte(' ')
		out.WriteString(record.Remark)
	}
	return out.String()
}

func recordAmountExpr(record storage.Record) string {
	if strings.EqualFold(record.Currency, "USDT") {
		return record.Amount + "U"
	}
	expr := record.Amount
	if record.Rate != "" && record.Rate != "1" {
		expr += "/" + record.Rate
		if record.Kind == "deposit" {
			if factor := feeFactorText(record.FeeRate); factor != "" {
				expr += "*" + factor
			}
		}
		expr += "=" + record.ResultUSDT + "U"
	}
	return expr
}

func feeFactorText(raw string) string {
	fee := parseRat(raw)
	if fee == nil || fee.Sign() == 0 {
		return ""
	}
	return formatRat(feeFactor(fee), 4)
}

func formatInt(value int) string {
	return new(big.Int).SetInt64(int64(value)).String()
}
