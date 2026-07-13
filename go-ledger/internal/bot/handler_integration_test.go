package bot

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/config"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/p2p"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/permissions"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/telegram"
)

func TestPostgresLedgerHandlerReplyAndPermissionContract(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := storage.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	now := time.Now().In(time.FixedZone("Asia/Shanghai", 8*3600))
	chatID := -1009000000000 - now.UnixNano()%1000000
	owner := storage.User{ID: 91001, DisplayName: "owner"}
	member := storage.User{ID: 91002, DisplayName: "member"}
	if err := store.EnsureGroup(ctx, chatID, "handler test", now); err != nil {
		t.Fatal(err)
	}
	if err := store.TouchUser(ctx, chatID, owner, now); err != nil {
		t.Fatal(err)
	}
	if err := store.SetGroupOwner(ctx, chatID, owner, now); err != nil {
		t.Fatal(err)
	}
	dayKey := businessDayKey(now, 0)
	if err := store.SetGroupActivePeriod(ctx, chatID, true, dayKey, dayKey, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.InsertRecord(ctx, storage.Record{
		ChatID: chatID, DayKey: dayKey, Kind: "deposit", Currency: "CNY", Amount: "10", Rate: "1", FeeRate: "0", ResultUSDT: "10",
		ActorUserID: owner.ID, ActorName: owner.DisplayName, SubjectUserID: owner.ID, SubjectName: owner.DisplayName, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{
		Timezone: "Asia/Shanghai", DefaultOperatorIDs: map[int64]struct{}{}, QueueSize: 16,
		GroupCacheTTL: time.Minute, BillSummaryCacheTTL: time.Minute, UserTouchCacheTTL: time.Minute,
		OperatorCacheTTL: time.Minute, WatchCacheTTL: time.Minute, P2PCacheTTL: time.Minute, P2PRefreshEvery: time.Minute,
	}
	b := New(cfg, store, nil, nil, nil)
	b.globalOperatorLookup = func(context.Context, int64) (permissions.UserCapabilities, bool, error) {
		return permissions.UserCapabilities{}, false, nil
	}

	message := func(id int64, user storage.User, text string) telegram.Message {
		return telegram.Message{
			MessageID: id,
			Chat:      telegram.Chat{ID: chatID, Type: "supergroup", Title: "handler test"},
			From:      &telegram.User{ID: user.ID, FirstName: user.DisplayName},
			Text:      text,
		}
	}
	claim := func(wantMessageID int64) storage.NotificationOutbox {
		items, claimErr := store.ClaimDueNotifications(ctx, 1000, 5, time.Now().Add(time.Minute))
		if claimErr != nil {
			t.Fatal(claimErr)
		}
		for _, item := range items {
			if item.ChatID == chatID && strings.Contains(item.DedupeKey, ":"+formatID(wantMessageID)) {
				return item
			}
		}
		t.Fatalf("no outbox item for source message %d: %+v", wantMessageID, items)
		return storage.NotificationOutbox{}
	}

	if err := b.handleMessage(ctx, message(101, member, "+0")); err != nil {
		t.Fatal(err)
	}
	if item := claim(101); item.ReplyToMessageID != 0 {
		t.Fatalf("+0 success should not reply: %+v", item)
	}

	before, err := store.NotificationOutboxStats(ctx, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := b.handleMessage(ctx, message(102, member, "开始")); err != nil {
		t.Fatal(err)
	}
	if err := b.handleMessage(ctx, message(103, member, "停止")); err != nil {
		t.Fatal(err)
	}
	after, err := store.NotificationOutboxStats(ctx, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if before.Pending != after.Pending || before.Sending != after.Sending {
		t.Fatalf("non-operator start/stop should stay silent: before=%+v after=%+v", before, after)
	}

	if err := b.handleMessage(ctx, message(104, member, "+100")); err != nil {
		t.Fatal(err)
	}
	if item := claim(104); item.ReplyToMessageID != 104 || !strings.Contains(item.Text, "没有操作权限") {
		t.Fatalf("rejected write should reply with error: %+v", item)
	}

	if err := b.handleMessage(ctx, message(105, owner, "设置费率3")); err != nil {
		t.Fatal(err)
	}
	if item := claim(105); item.ReplyToMessageID != 0 {
		t.Fatalf("successful setting should not reply: %+v", item)
	}
	if err := b.handleMessage(ctx, message(106, owner, "设置汇率0")); err != nil {
		t.Fatal(err)
	}
	if item := claim(106); item.ReplyToMessageID != 106 {
		t.Fatalf("setting error should reply: %+v", item)
	}

	b.setRateBookEntries([]p2p.OrderBookEntry{{Rank: 1, Price: "7.1", MerchantName: "cached"}}, now)
	if err := b.handleMessage(ctx, message(107, member, "Z0")); err != nil {
		t.Fatal(err)
	}
	if item := claim(107); item.ReplyToMessageID != 0 {
		t.Fatalf("Z0 cache result should not reply: %+v", item)
	}
	if err := b.handleMessage(ctx, message(108, owner, "设置汇率 Z1 -0.1")); err != nil {
		t.Fatal(err)
	}
	if item := claim(108); item.ReplyToMessageID != 0 {
		t.Fatalf("successful Z setting should not reply: %+v", item)
	}
	if err := b.handleMessage(ctx, message(109, owner, "设置汇率 Z9 -0.1")); err != nil {
		t.Fatal(err)
	}
	if item := claim(109); item.ReplyToMessageID != 109 {
		t.Fatalf("Z setting error should reply: %+v", item)
	}

	if err := b.handleMessage(ctx, message(110, owner, "清除当前账期")); err != nil {
		t.Fatal(err)
	}
	item := claim(110)
	if item.ReplyToMessageID != 0 || !strings.Contains(item.Text, "账期起止") || !strings.Contains(item.Text, "记录数：1 条") || !strings.Contains(item.Text, "不可恢复") {
		t.Fatalf("clear confirmation contract mismatch: %+v", item)
	}
}
