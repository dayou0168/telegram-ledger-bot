package bot

import (
	"strings"
	"testing"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/telegram"
)

func TestBuildBillText(t *testing.T) {
	loc := time.FixedZone("Asia/Shanghai", 8*3600)
	createdAt := time.Date(2026, 7, 6, 1, 2, 3, 0, loc)
	text := buildBillText(storage.Group{
		DepositExchangeRate: "10",
		FeeRate:             "3",
	}, []storage.Record{
		{
			Kind:       "deposit",
			Currency:   "CNY",
			Amount:     "100.00",
			Rate:       "10",
			FeeRate:    "3",
			ResultUSDT: "9.70",
			ActorName:  "阿泽",
			Remark:     "测试",
			CreatedAt:  createdAt,
		},
		{
			Kind:       "payout",
			Currency:   "USDT",
			Amount:     "2.00",
			Rate:       "1",
			FeeRate:    "0",
			ResultUSDT: "2.00",
			ActorName:  "阿泽",
			CreatedAt:  createdAt.Add(time.Second),
		},
	}, loc, "")

	wants := []string{
		"<b>今日入款（1笔）</b>",
		"01:02:03 100 / 10*0.97=9.7U 阿泽 测试",
		"<b>今日下发（1笔）</b>",
		"01:02:04 2U 阿泽",
		"总入款：100（9.7U）",
		"交易费率：3%",
		"已下发：2U",
		"余额：7.7U",
	}
	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Fatalf("bill text missing %q:\n%s", want, text)
		}
	}
}

func TestRecordLineUsesSubjectNameAndMessageLink(t *testing.T) {
	loc := time.FixedZone("Asia/Shanghai", 8*3600)
	record := storage.Record{
		ChatID:          -1003720457420,
		SourceMessageID: 1234,
		Kind:            "deposit",
		Currency:        "CNY",
		Amount:          "666",
		Rate:            "10",
		ResultUSDT:      "66.6",
		SubjectName:     "新一",
		ActorName:       "阿泽",
		CreatedAt:       time.Date(2026, 7, 6, 21, 33, 2, 0, loc),
	}
	line := recordLine(record, loc)
	if !strings.Contains(line, `href="https://t.me/c/3720457420/1234"`) {
		t.Fatalf("record line missing message link: %s", line)
	}
	if !strings.Contains(line, `>666</a> / 10=66.6U`) {
		t.Fatalf("record line should only link the original amount in formula: %s", line)
	}
	if strings.Contains(line, "阿泽") || !strings.Contains(line, ">新一</a>") {
		t.Fatalf("record line should display subject name only: %s", line)
	}
}

func TestRecordLineShowsDefaultRateFormula(t *testing.T) {
	loc := time.FixedZone("Asia/Shanghai", 8*3600)
	record := storage.Record{
		Kind:        "deposit",
		Currency:    "CNY",
		Amount:      "888.00",
		Rate:        "1",
		ResultUSDT:  "888.00",
		SubjectName: "阿泽",
		CreatedAt:   time.Date(2026, 7, 6, 19, 45, 52, 0, loc),
	}
	line := recordLine(record, loc)
	if !strings.Contains(line, "19:45:52 888 / 1=888U 阿泽") {
		t.Fatalf("record line should show default rate formula: %s", line)
	}
}

func TestLedgerSubjectFromReplyMessage(t *testing.T) {
	msg := telegram.Message{
		From: &telegram.User{ID: 1, FirstName: "阿泽", Username: "aze89"},
		ReplyTo: &telegram.Message{
			From: &telegram.User{ID: 2, FirstName: "新一", Username: "newone"},
		},
	}
	subject := ledgerSubjectFromMessage(msg, userFromTelegram(*msg.From))
	if subject.ID != 2 || subject.DisplayName != "新一" {
		t.Fatalf("subject = %+v, want replied user 新一", subject)
	}
}

func TestBuildBillTextRealtimeRateLabel(t *testing.T) {
	loc := time.FixedZone("Asia/Shanghai", 8*3600)
	text := buildBillText(storage.Group{
		DepositExchangeRate: "6.63",
		ExchangeRateSource:  "支付宝",
		ExchangeRateRank:    1,
		ExchangeRateOffset:  "-0.1",
		FeeRate:             "0",
	}, nil, loc, "")
	if !strings.Contains(text, "实时汇率：\n支付宝1档 下浮0.1") {
		t.Fatalf("bill text missing realtime rate label:\n%s", text)
	}
}

func TestBuildBillTextShowsLatestFiveRecords(t *testing.T) {
	loc := time.FixedZone("Asia/Shanghai", 8*3600)
	createdAt := time.Date(2026, 7, 6, 1, 0, 0, 0, loc)
	records := make([]storage.Record, 0, 7)
	for i := 1; i <= 7; i++ {
		records = append(records, storage.Record{
			Kind:        "deposit",
			Currency:    "CNY",
			Amount:      formatInt(i),
			Rate:        "1",
			ResultUSDT:  formatInt(i),
			SubjectName: "阿泽",
			CreatedAt:   createdAt.Add(time.Duration(i) * time.Minute),
		})
	}
	text := buildBillText(storage.Group{DepositExchangeRate: "1"}, records, loc, "")
	if !strings.Contains(text, "<b>今日入款（7笔）</b>") {
		t.Fatalf("bill text should keep total count:\n%s", text)
	}
	if strings.Contains(text, "01:01:00 1 / 1=1U") || strings.Contains(text, "01:02:00 2 / 1=2U") {
		t.Fatalf("bill text should hide older records:\n%s", text)
	}
	for _, want := range []string{
		"01:03:00 3 / 1=3U",
		"01:07:00 7 / 1=7U",
		"总入款：28（28U）",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("bill text missing %q:\n%s", want, text)
		}
	}
}
