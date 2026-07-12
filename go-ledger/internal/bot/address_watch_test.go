package bot

import (
	"strings"
	"testing"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/tron"
)

func TestFormatTransferNoticeUsesConfiguredTimezone(t *testing.T) {
	loc := time.FixedZone("Asia/Shanghai", 8*3600)
	b := &Bot{loc: loc}
	text := b.formatTransferNotice(tron.Transfer{
		Hash:           "242a3a490a7a96b43bd4ec14b739c8cde8128d3371910ac3465d085f9a5fe02f",
		From:           "TCYugQbJeHtUZF9vNmFExXMnCPNgN7kPPV",
		To:             "TWqcMjV7Wq2RHe2CSiKQHpkn6A7B2AWUPe",
		Value:          "500900000",
		TokenDecimals:  6,
		BlockTimestamp: time.Date(2026, 7, 6, 15, 43, 51, 0, time.UTC).UnixMilli(),
	}, storage.WatchTarget{
		Address: "TWqcMjV7Wq2RHe2CSiKQHpkn6A7B2AWUPe",
		Label:   "新币",
	}, "income")
	if !strings.Contains(text, "交易时间： 2026-07-06 23:43:51") {
		t.Fatalf("notice should use Beijing time:\n%s", text)
	}
	if !strings.Contains(text, `href="https://tronscan.org/#/transaction/242a3a490a7a96b43bd4ec14b739c8cde8128d3371910ac3465d085f9a5fe02f"`) {
		t.Fatalf("notice should keep hash link:\n%s", text)
	}
}
