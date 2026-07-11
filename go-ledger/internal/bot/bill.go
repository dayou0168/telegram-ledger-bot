package bot

import (
	"html"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
)

const groupBillDefaultRecordLimit = 5

func buildBillText(group storage.Group, records []storage.Record, loc *time.Location, prefix string) string {
	return buildBillTextFromSummary(group, storage.BillSummaryData{
		Records: records,
		Summary: summarizeRecordsForBill(records),
	}, loc, prefix)
}

func summarizeRecordsForBill(records []storage.Record) storage.RecordDaySummary {
	var summary storage.RecordDaySummary
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
			summary.DepositCount++
			totalDepositUSDT.Add(totalDepositUSDT, result)
			if strings.EqualFold(record.Currency, "CNY") {
				if amount := parseRat(record.Amount); amount != nil {
					totalDepositCNY.Add(totalDepositCNY, amount)
				}
			}
		case "payout":
			summary.PayoutCount++
			totalPayoutUSDT.Add(totalPayoutUSDT, result)
		}
	}
	summary.TotalDepositCNY = formatAmount(totalDepositCNY)
	summary.TotalDepositUSDT = formatAmount(totalDepositUSDT)
	summary.TotalPayoutUSDT = formatAmount(totalPayoutUSDT)
	return summary
}

func buildBillTextFromSummary(group storage.Group, data storage.BillSummaryData, loc *time.Location, prefix string) string {
	var deposits []storage.Record
	var payouts []storage.Record

	for _, record := range data.Records {
		switch record.Kind {
		case "deposit":
			deposits = append(deposits, record)
		case "payout":
			payouts = append(payouts, record)
		}
	}
	totalDepositCNY := ratFromSummary(data.Summary.TotalDepositCNY)
	totalDepositUSDT := ratFromSummary(data.Summary.TotalDepositUSDT)
	totalPayoutUSDT := ratFromSummary(data.Summary.TotalPayoutUSDT)
	balance := new(big.Rat).Sub(totalDepositUSDT, totalPayoutUSDT)

	var out strings.Builder
	if prefix != "" {
		out.WriteString(html.EscapeString(prefix))
		out.WriteString("\n\n")
	}
	out.WriteString("<b>今日入款（")
	out.WriteString(formatInt64(data.Summary.DepositCount))
	out.WriteString("笔）</b>\n")
	for _, record := range latestRecords(deposits, groupBillDefaultRecordLimit) {
		out.WriteString(recordLine(record, loc))
		out.WriteByte('\n')
	}
	out.WriteString("\n<b>今日下发（")
	out.WriteString(formatInt64(data.Summary.PayoutCount))
	out.WriteString("笔）</b>\n")
	for _, record := range latestRecords(payouts, groupBillDefaultRecordLimit) {
		out.WriteString(recordLine(record, loc))
		out.WriteByte('\n')
	}
	out.WriteString("\n总入款：")
	out.WriteString(formatAmount(totalDepositCNY))
	out.WriteString("（")
	out.WriteString(formatAmount(totalDepositUSDT))
	out.WriteString("U）\n")
	if label := exchangeRateDisplay(group); label != "" {
		out.WriteString("实时汇率：\n")
		out.WriteString(html.EscapeString(label))
	} else {
		out.WriteString("汇率：")
		out.WriteString(html.EscapeString(formatRecordRate(group.DepositExchangeRate)))
	}
	out.WriteString("\n交易费率：")
	out.WriteString(html.EscapeString(formatRecordAmount(group.FeeRate)))
	out.WriteString("%\n\n应下发：")
	out.WriteString(formatAmount(totalDepositUSDT))
	out.WriteString("U\n已下发：")
	out.WriteString(formatAmount(totalPayoutUSDT))
	out.WriteString("U\n余额：")
	out.WriteString(formatAmount(balance))
	out.WriteString("U")
	return out.String()
}

func ratFromSummary(raw string) *big.Rat {
	if value := parseRat(raw); value != nil {
		return value
	}
	return big.NewRat(0, 1)
}

func latestRecords(records []storage.Record, limit int) []storage.Record {
	if limit <= 0 || len(records) <= limit {
		return records
	}
	return records[len(records)-limit:]
}

func recordLine(record storage.Record, loc *time.Location) string {
	createdAt := record.CreatedAt
	if loc != nil {
		createdAt = createdAt.In(loc)
	}
	var out strings.Builder
	out.WriteString(createdAt.Format("15:04:05"))
	out.WriteByte(' ')
	link := recordMessageURL(record.ChatID, record.SourceMessageID)
	out.WriteString(recordAmountExprHTML(record, link))
	if name := recordSubjectName(record); name != "" {
		out.WriteByte(' ')
		out.WriteString(linkedRecordText(name, link))
	}
	if record.Remark != "" {
		out.WriteByte(' ')
		out.WriteString(html.EscapeString(record.Remark))
	}
	return out.String()
}

func recordSubjectName(record storage.Record) string {
	if strings.TrimSpace(record.SubjectName) != "" {
		return strings.TrimSpace(record.SubjectName)
	}
	return strings.TrimSpace(record.ActorName)
}

func linkedRecordText(text, link string) string {
	escaped := html.EscapeString(text)
	if link == "" {
		return escaped
	}
	return `<a href="` + html.EscapeString(link) + `">` + escaped + `</a>`
}

func recordMessageURL(chatID, messageID int64) string {
	if messageID <= 0 {
		return ""
	}
	raw := strconv.FormatInt(chatID, 10)
	if strings.HasPrefix(raw, "-100") && len(raw) > 4 {
		return "https://t.me/c/" + strings.TrimPrefix(raw, "-100") + "/" + strconv.FormatInt(messageID, 10)
	}
	return ""
}

func recordAmountExpr(record storage.Record) string {
	if strings.EqualFold(record.Currency, "USDT") {
		return formatRecordAmount(record.Amount) + "U"
	}
	expr := formatRecordAmount(record.Amount)
	rate := formatRecordRate(record.Rate)
	if rate == "" {
		rate = "1"
	}
	expr += " / " + rate
	if record.Kind == "deposit" {
		if factor := feeFactorText(record.FeeRate); factor != "" {
			expr += "*" + factor
		}
	}
	expr += "=" + formatRecordAmount(record.ResultUSDT) + "U"
	return expr
}

func recordAmountExprHTML(record storage.Record, link string) string {
	amount := formatRecordAmount(record.Amount)
	if strings.EqualFold(record.Currency, "USDT") {
		return linkedRecordText(amount+"U", link)
	}
	rate := formatRecordRate(record.Rate)
	if rate == "" {
		rate = "1"
	}
	var out strings.Builder
	out.WriteString(linkedRecordText(amount, link))
	out.WriteString(" / ")
	out.WriteString(html.EscapeString(rate))
	if record.Kind == "deposit" {
		if factor := feeFactorText(record.FeeRate); factor != "" {
			out.WriteByte('*')
			out.WriteString(html.EscapeString(factor))
		}
	}
	out.WriteByte('=')
	out.WriteString(html.EscapeString(formatRecordAmount(record.ResultUSDT)))
	out.WriteByte('U')
	return out.String()
}

func formatRecordAmount(raw string) string {
	value := parseRat(raw)
	if value == nil {
		return strings.TrimSpace(raw)
	}
	return formatAmount(value)
}

func formatRecordRate(raw string) string {
	value := parseRat(raw)
	if value == nil {
		return strings.TrimSpace(raw)
	}
	return formatRat(value, 8)
}

func feeFactorText(raw string) string {
	fee := parseRat(raw)
	if fee == nil || fee.Sign() == 0 {
		return ""
	}
	return formatRat(feeFactor(fee), 4)
}

func formatInt(value int) string {
	return formatInt64(int64(value))
}

func formatInt64(value int64) string {
	return new(big.Int).SetInt64(value).String()
}

func exchangeRateDisplay(group storage.Group) string {
	if group.ExchangeRateSource == "" || group.ExchangeRateRank <= 0 {
		return ""
	}
	source := strings.TrimSpace(group.ExchangeRateSource)
	if source == "" {
		source = "支付宝"
	}
	label := source + formatInt(group.ExchangeRateRank) + "档"
	offset := parseRat(group.ExchangeRateOffset)
	if offset == nil || offset.Sign() == 0 {
		return label
	}
	if offset.Sign() > 0 {
		return label + " 上浮" + formatRat(offset, 8)
	}
	abs := new(big.Rat).Neg(offset)
	return label + " 下浮" + formatRat(abs, 8)
}
