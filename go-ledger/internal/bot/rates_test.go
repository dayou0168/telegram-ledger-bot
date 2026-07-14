package bot

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/config"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/p2p"
)

func TestRateBookUsesCachedSnapshotOnly(t *testing.T) {
	loc := time.FixedZone("Asia/Shanghai", 8*3600)
	b := &Bot{
		cfg: config.Config{P2PRefreshEvery: time.Minute},
		loc: loc,
	}
	updatedAt := time.Date(2026, 7, 11, 12, 0, 0, 0, loc)
	b.setRateBookEntries([]p2p.OrderBookEntry{{Rank: 1, Price: "7.12", MerchantName: "Alpha"}}, updatedAt)

	book, err := b.rateBook(updatedAt.Add(time.Second))
	if err != nil {
		t.Fatalf("rateBook() error = %v", err)
	}
	if len(book.Entries) != 1 || book.Entries[0].Price != "7.12" {
		t.Fatalf("rateBook entries = %+v", book.Entries)
	}
	if book.Stale {
		t.Fatal("fresh cached rate book should not be stale")
	}
}

func TestRateBookRefreshFailureKeepsPreviousSnapshot(t *testing.T) {
	loc := time.FixedZone("Asia/Shanghai", 8*3600)
	b := &Bot{
		cfg: config.Config{P2PRefreshEvery: time.Minute},
		loc: loc,
	}
	updatedAt := time.Date(2026, 7, 11, 12, 0, 0, 0, loc)
	b.setRateBookEntries([]p2p.OrderBookEntry{{Rank: 1, Price: "7.12", MerchantName: "Alpha"}}, updatedAt)
	b.setRateBookError(errors.New("upstream timeout"))

	book, err := b.rateBook(updatedAt.Add(10 * time.Second))
	if err != nil {
		t.Fatalf("rateBook() should keep previous snapshot after refresh failure: %v", err)
	}
	if !book.Stale || book.LastError != "upstream timeout" {
		t.Fatalf("book stale/error = %v/%q", book.Stale, book.LastError)
	}
	text := formatZ0Book(book, loc)
	if !strings.Contains(text, "状态：使用上一版缓存") || !strings.Contains(text, "upstream timeout") {
		t.Fatalf("formatZ0Book should mark stale cache and last error:\n%s", text)
	}
}

func TestRateBookWithoutSnapshotFailsFast(t *testing.T) {
	b := &Bot{cfg: config.Config{P2PRefreshEvery: time.Minute}}
	if _, err := b.rateBook(time.Now()); err == nil {
		t.Fatal("rateBook without snapshot should fail fast instead of cold fetching")
	}
}

func TestFormatZ0ExactAlignedHTMLAndPrompt(t *testing.T) {
	entries := make([]p2p.OrderBookEntry, 10)
	for i := range entries {
		entries[i] = p2p.OrderBookEntry{Rank: i + 1, Price: "6.73", MerchantName: "商户"}
	}
	entries[9] = p2p.OrderBookEntry{Rank: 10, Price: "6.74", MerchantName: "红杉<&贸易"}
	text := formatZ0Book(cachedRateBook{Entries: entries, UpdatedAt: time.Now()}, time.UTC)
	for _, want := range []string{
		"<pre>Z1 :   6.73   商户\n",
		"Z10 : 6.74   红杉&lt;&amp;贸易\n</pre>",
		"发送 Z1 -0.1\n或 设置汇率 Z1 -0.1 可按第1档偏移后设置汇率。",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("Z0 output missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "缓存更新时间") {
		t.Fatalf("Z0 output must not expose cache timestamp:\n%s", text)
	}
}
