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
				if markErr := store.MarkNotificationSent(ctx, item.ID, wantMessageID+10000, time.Now()); markErr != nil {
					t.Fatal(markErr)
				}
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

	before, err := store.NotificationOutboxCountForChat(ctx, chatID)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.handleMessage(ctx, message(102, member, "开始")); err != nil {
		t.Fatal(err)
	}
	if err := b.handleMessage(ctx, message(103, member, "停止")); err != nil {
		t.Fatal(err)
	}
	after, err := store.NotificationOutboxCountForChat(ctx, chatID)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
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
	if item := claim(105); item.ReplyToMessageID != 0 || item.Text != "费率设置成功，当前交易费率为：3%" {
		t.Fatalf("successful setting should not reply: %+v", item)
	}
	if err := b.handleMessage(ctx, message(1050, owner, "设置汇率10")); err != nil {
		t.Fatal(err)
	}
	if item := claim(1050); item.ReplyToMessageID != 0 || item.Text != "固定汇率设置成功，当前固定汇率为： 10 。" {
		t.Fatalf("fixed-rate success contract mismatch: %+v", item)
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
	if item := claim(107); item.ReplyToMessageID != 0 ||
		strings.Contains(item.Text, "<pre>") || strings.Contains(item.Text, "</pre>") ||
		strings.Contains(item.Text, "<code>") || strings.Contains(item.Text, "</code>") ||
		!strings.Contains(item.Text, "Z1 :   7.1   cached") {
		t.Fatalf("Z0 cache result should be plain aligned text and not reply: %+v", item)
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
	if err := b.handleMessage(ctx, message(1090, member, "1+2")); err != nil {
		t.Fatal(err)
	}
	if item := claim(1090); item.ReplyToMessageID != 0 || item.Text != "1+2=3" {
		t.Fatalf("calculator success should be a new message: %+v", item)
	}
	original := message(1091, owner, "+100")
	if err := b.handleMessage(ctx, original); err != nil {
		t.Fatal(err)
	}
	if item := claim(1091); item.ReplyToMessageID != 0 || item.ReferenceKind != "ledger_record" {
		t.Fatalf("ledger write receipt contract mismatch: %+v", item)
	}
	undo := message(1092, owner, "撤销")
	undo.ReplyTo = &original
	if err := b.handleMessage(ctx, undo); err != nil {
		t.Fatal(err)
	}
	if item := claim(1092); item.ReplyToMessageID != 0 || item.ReferenceKind != "ledger_record" || item.Text != "已撤销入款" {
		t.Fatalf("undo success should be durable and no-reply: %+v", item)
	}

	if err := b.handleMessage(ctx, message(110, owner, "清除当前账期")); err != nil {
		t.Fatal(err)
	}
	item := claim(110)
	if item.ReplyToMessageID != 0 || !strings.Contains(item.Text, "账期起止") || !strings.Contains(item.Text, "记录数：1 条") || !strings.Contains(item.Text, "不可恢复") {
		t.Fatalf("clear confirmation contract mismatch: %+v", item)
	}
}

func TestPostgresDelayedLedgerReceiptRendersItsOriginalPeriod(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := storage.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	loc := time.FixedZone("Asia/Shanghai", 8*3600)
	now := time.Now().In(loc).Truncate(time.Microsecond)
	chatID := -1009100000000 - now.UnixNano()%1000000
	oldDay, newDay := "2026-07-14", "2026-07-15"
	oldStart := now.Add(-2 * time.Hour)
	newStart := now.Add(-time.Hour)
	if err := store.EnsureGroup(ctx, chatID, "old receipt", oldStart); err != nil {
		t.Fatal(err)
	}
	if err := store.SetGroupActivePeriod(ctx, chatID, true, oldDay, oldDay, oldStart); err != nil {
		t.Fatal(err)
	}
	oldRecordID, _, err := store.InsertLedgerRecordWithOutbox(ctx, storage.Record{
		ChatID: chatID, DayKey: oldDay, PeriodStartedAt: oldStart,
		Kind: "deposit", Currency: "CNY", Amount: "111", Rate: "1", FeeRate: "0", ResultUSDT: "111",
		SubjectUserID: 1, SubjectName: "old-period-marker", ActorUserID: 1, ActorName: "owner",
		SourceMessageID: 9001, CreatedAt: oldStart.Add(time.Minute),
	}, storage.NotificationOutbox{
		Kind: "ledger_bill", DedupeKey: "delayed-old-period:" + formatID(chatID), ChatID: chatID,
		ReferenceKind: "ledger_record", Priority: 0,
	}, now, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetGroupActivePeriod(ctx, chatID, false, "", "", now); err != nil {
		t.Fatal(err)
	}
	if err := store.SetGroupActivePeriod(ctx, chatID, true, newDay, newDay, newStart); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.InsertLedgerRecordWithOutbox(ctx, storage.Record{
		ChatID: chatID, DayKey: newDay, PeriodStartedAt: newStart,
		Kind: "deposit", Currency: "CNY", Amount: "222", Rate: "1", FeeRate: "0", ResultUSDT: "222",
		SubjectUserID: 2, SubjectName: "new-period-marker", ActorUserID: 1, ActorName: "owner",
		SourceMessageID: 9002, CreatedAt: newStart.Add(time.Minute),
	}, storage.NotificationOutbox{
		Kind: "ledger_bill", DedupeKey: "delayed-new-period:" + formatID(chatID), ChatID: chatID,
		ReferenceKind: "ledger_record", Priority: 0,
	}, now, true); err != nil {
		t.Fatal(err)
	}

	b := New(config.Config{
		Timezone: "Asia/Shanghai", QueueSize: 16, GroupCacheTTL: time.Minute,
		BillSummaryCacheTTL: time.Minute, LedgerSummaryWriteMode: "shadow", LedgerSummaryReadMode: "safe",
	}, store, nil, nil, nil)
	text, _, err := b.renderOutboxMessage(ctx, storage.NotificationOutbox{
		Kind: "ledger_bill", ReferenceKind: "ledger_record", ReferenceID: oldRecordID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "old-period-marker") || strings.Contains(text, "new-period-marker") || strings.Contains(text, "222") {
		t.Fatalf("delayed old receipt mixed periods: %q", text)
	}
}

func TestPostgresLedgerStartStopResumePeriodContract(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := storage.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	loc := time.FixedZone("Asia/Shanghai", 8*3600)
	owner := storage.User{ID: 92001, DisplayName: "owner"}
	cfg := config.Config{
		Timezone: "Asia/Shanghai", QueueSize: 16, GroupCacheTTL: time.Minute,
		OperatorCacheTTL: time.Minute, BillSummaryCacheTTL: time.Minute,
	}
	b := New(cfg, store, nil, nil, nil)
	b.globalOperatorLookup = func(context.Context, int64) (permissions.UserCapabilities, bool, error) {
		return permissions.UserCapabilities{}, false, nil
	}
	message := func(chatID, id int64, text string) telegram.Message {
		return telegram.Message{MessageID: id, Chat: telegram.Chat{ID: chatID, Type: "supergroup"},
			From: &telegram.User{ID: owner.ID, FirstName: owner.DisplayName}, Text: text}
	}
	prepare := func(chatID int64, cutoff int, dayKey string, periodStart time.Time) {
		if err := store.EnsureGroup(ctx, chatID, "period contract", periodStart); err != nil {
			t.Fatal(err)
		}
		if err := store.TouchUser(ctx, chatID, owner, periodStart); err != nil {
			t.Fatal(err)
		}
		if err := store.SetGroupOwner(ctx, chatID, owner, periodStart); err != nil {
			t.Fatal(err)
		}
		expires := dayKey
		if cutoff == cutoffDisabledHour {
			expires = ""
		}
		if err := store.SetGroupCutoffState(ctx, chatID, cutoff, true, dayKey, expires, periodStart); err != nil {
			t.Fatal(err)
		}
	}

	base := time.Date(2026, 7, 14, 10, 0, 0, 123000000, loc)
	chatID := int64(-1009200000001)
	prepare(chatID, 0, "2026-07-14", base)
	if err := b.stopAccounting(ctx, message(chatID, 1, "停止"), owner, base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := b.startAccounting(ctx, message(chatID, 2, "开始"), owner, base.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	group, err := store.GetGroup(ctx, chatID)
	if err != nil {
		t.Fatal(err)
	}
	if group.ActiveDayKey != "2026-07-14" || !group.ActivePeriodStartedAt.Equal(base) {
		t.Fatalf("same-day resume changed period: %+v", group)
	}
	beforeCutoff := time.Date(2026, 7, 14, 23, 59, 59, 999000000, loc)
	if err := b.stopAccounting(ctx, message(chatID, 3, "停止"), owner, beforeCutoff); err != nil {
		t.Fatal(err)
	}
	afterCutoff := time.Date(2026, 7, 15, 0, 0, 0, 0, loc)
	if err := b.startAccounting(ctx, message(chatID, 4, "开始"), owner, afterCutoff); err != nil {
		t.Fatal(err)
	}
	group, err = store.GetGroup(ctx, chatID)
	if err != nil {
		t.Fatal(err)
	}
	if group.ActiveDayKey != "2026-07-15" || !group.ActivePeriodStartedAt.Equal(afterCutoff) {
		t.Fatalf("post-cutoff start did not create a new period: %+v", group)
	}

	fourAMChatID := int64(-1009200000003)
	prepare(fourAMChatID, 4, "2026-07-14", base)
	preFourAM := time.Date(2026, 7, 15, 3, 59, 59, 999000000, loc)
	if err := b.stopAccounting(ctx, message(fourAMChatID, 7, "停止"), owner, preFourAM.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := b.startAccounting(ctx, message(fourAMChatID, 8, "开始"), owner, preFourAM); err != nil {
		t.Fatal(err)
	}
	group, err = store.GetGroup(ctx, fourAMChatID)
	if err != nil {
		t.Fatal(err)
	}
	if group.ActiveDayKey != "2026-07-14" || !group.ActivePeriodStartedAt.Equal(base) {
		t.Fatalf("pre-4am resume changed period: %+v", group)
	}
	if err := b.stopAccounting(ctx, message(fourAMChatID, 9, "停止"), owner, preFourAM); err != nil {
		t.Fatal(err)
	}
	atFourAM := time.Date(2026, 7, 15, 4, 0, 0, 0, loc)
	if err := b.startAccounting(ctx, message(fourAMChatID, 10, "开始"), owner, atFourAM); err != nil {
		t.Fatal(err)
	}
	group, err = store.GetGroup(ctx, fourAMChatID)
	if err != nil {
		t.Fatal(err)
	}
	if group.ActiveDayKey != "2026-07-15" || !group.ActivePeriodStartedAt.Equal(atFourAM) {
		t.Fatalf("4am boundary did not create a new period: %+v", group)
	}

	expiredThenDisabledChatID := int64(-1009200000004)
	prepare(expiredThenDisabledChatID, 0, "2026-07-14", base)
	if err := b.stopAccounting(ctx, message(expiredThenDisabledChatID, 11, "停止"), owner, beforeCutoff); err != nil {
		t.Fatal(err)
	}
	disableAfterBoundary := time.Date(2026, 7, 15, 1, 0, 0, 0, loc)
	if err := b.handleSetting(ctx, message(expiredThenDisabledChatID, 12, "关闭日切"), owner,
		settingCommand{Kind: "cutoff", CutoffHour: cutoffDisabledHour}, disableAfterBoundary); err != nil {
		t.Fatal(err)
	}
	group, err = store.GetGroup(ctx, expiredThenDisabledChatID)
	if err != nil {
		t.Fatal(err)
	}
	if group.Active || group.ActiveDayKey != "" || group.ActivePeriodStartedAt.After(time.Unix(0, 0)) {
		t.Fatalf("disabling cutoff revived an already sealed period: %+v", group)
	}
	if err := b.startAccounting(ctx, message(expiredThenDisabledChatID, 13, "开始"), owner, disableAfterBoundary); err != nil {
		t.Fatal(err)
	}
	group, err = store.GetGroup(ctx, expiredThenDisabledChatID)
	if err != nil {
		t.Fatal(err)
	}
	if group.ActiveDayKey != "2026-07-15" || !group.ActivePeriodStartedAt.Equal(disableAfterBoundary) {
		t.Fatalf("post-seal disabled-cutoff start did not create a new period: %+v", group)
	}

	disabledChatID := int64(-1009200000002)
	prepare(disabledChatID, cutoffDisabledHour, "2026-07-01", base)
	if err := b.stopAccounting(ctx, message(disabledChatID, 5, "停止"), owner, base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	resumeLater := base.AddDate(0, 1, 0)
	if err := b.startAccounting(ctx, message(disabledChatID, 6, "开始"), owner, resumeLater); err != nil {
		t.Fatal(err)
	}
	group, err = store.GetGroup(ctx, disabledChatID)
	if err != nil {
		t.Fatal(err)
	}
	if group.ActiveDayKey != "2026-07-01" || !group.ActivePeriodStartedAt.Equal(base) {
		t.Fatalf("disabled-cutoff resume changed continuous period: %+v", group)
	}
}

func TestPostgresDisableCutoffAliasesPreserveStateAcrossMidnightAndRestart(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	loc := time.FixedZone("Asia/Shanghai", 8*3600)
	beforeMidnight := time.Date(2026, 7, 19, 23, 59, 59, 0, loc)
	afterMidnight := time.Date(2026, 7, 20, 0, 0, 1, 0, loc)
	periodStart := time.Date(2026, 7, 19, 10, 0, 0, 0, loc)
	dayKey := "2026-07-19"
	owner := storage.User{ID: 94001, DisplayName: "owner"}
	suffix := time.Now().UnixNano() % 1000000

	newBot := func(store *storage.Store) *Bot {
		b := New(config.Config{
			Timezone: "Asia/Shanghai", QueueSize: 16, GroupCacheTTL: time.Minute,
			OperatorCacheTTL: time.Minute, BillSummaryCacheTTL: time.Minute,
		}, store, nil, nil, nil)
		b.globalOperatorLookup = func(context.Context, int64) (permissions.UserCapabilities, bool, error) {
			return permissions.UserCapabilities{}, false, nil
		}
		return b
	}
	message := func(chatID, id int64, text string) telegram.Message {
		return telegram.Message{
			MessageID: id,
			Chat:      telegram.Chat{ID: chatID, Type: "supergroup", Title: "cutoff alias test"},
			From:      &telegram.User{ID: owner.ID, FirstName: owner.DisplayName},
			Text:      text,
		}
	}
	prepare := func(t *testing.T, store *storage.Store, chatID int64) {
		t.Helper()
		if err := store.EnsureGroup(ctx, chatID, "cutoff alias test", periodStart); err != nil {
			t.Fatal(err)
		}
		if err := store.TouchUser(ctx, chatID, owner, periodStart); err != nil {
			t.Fatal(err)
		}
		if err := store.SetGroupOwner(ctx, chatID, owner, periodStart); err != nil {
			t.Fatal(err)
		}
		if err := store.SetGroupExchangeRate(ctx, chatID, "7.1", periodStart); err != nil {
			t.Fatal(err)
		}
		if err := store.SetGroupFeeRate(ctx, chatID, "3", periodStart); err != nil {
			t.Fatal(err)
		}
		if err := store.SetGroupCutoffState(ctx, chatID, 0, true, dayKey, dayKey, periodStart); err != nil {
			t.Fatal(err)
		}
	}
	assertSettingsPreserved := func(t *testing.T, group storage.Group) {
		t.Helper()
		if group.DepositExchangeRate != "7.1" || group.PayoutExchangeRate != "7.1" || group.FeeRate != "3" {
			t.Fatalf("disabling cutoff changed rate settings: %+v", group)
		}
	}

	for index, alias := range []string{"关闭日切", "设置日切-1"} {
		t.Run(alias, func(t *testing.T) {
			store, err := storage.Open(ctx, dsn)
			if err != nil {
				t.Fatal(err)
			}
			activeChatID := int64(-1009400000000) - int64(index*10) - suffix
			stoppedChatID := activeChatID - 1
			prepare(t, store, activeChatID)
			prepare(t, store, stoppedChatID)
			b := newBot(store)
			cmd, ok := parseSetting(alias)
			if !ok || cmd.Kind != "cutoff" || cmd.CutoffHour != cutoffDisabledHour {
				store.Close()
				t.Fatalf("parseSetting(%q) = %+v, %v", alias, cmd, ok)
			}

			if err := b.handleSetting(ctx, message(activeChatID, 100+int64(index), alias), owner, cmd, beforeMidnight); err != nil {
				store.Close()
				t.Fatal(err)
			}
			activeGroup, err := store.GetGroup(ctx, activeChatID)
			if err != nil {
				store.Close()
				t.Fatal(err)
			}
			if !activeGroup.Active || activeGroup.CutoffHour != cutoffDisabledHour || activeGroup.ActiveDayKey != dayKey ||
				activeGroup.ActiveExpiresDayKey != "" || !activeGroup.ActivePeriodStartedAt.Equal(periodStart) {
				store.Close()
				t.Fatalf("active group changed period while disabling cutoff: %+v", activeGroup)
			}
			assertSettingsPreserved(t, activeGroup)

			if err := b.stopAccounting(ctx, message(stoppedChatID, 200+int64(index), "停止"), owner, beforeMidnight.Add(-time.Hour)); err != nil {
				store.Close()
				t.Fatal(err)
			}
			if err := b.handleSetting(ctx, message(stoppedChatID, 300+int64(index), alias), owner, cmd, beforeMidnight); err != nil {
				store.Close()
				t.Fatal(err)
			}
			stoppedGroup, err := store.GetGroup(ctx, stoppedChatID)
			if err != nil {
				store.Close()
				t.Fatal(err)
			}
			if stoppedGroup.Active || stoppedGroup.CutoffHour != cutoffDisabledHour || stoppedGroup.ActiveDayKey != dayKey ||
				stoppedGroup.ActiveExpiresDayKey != "" || !stoppedGroup.ActivePeriodStartedAt.Equal(periodStart) {
				store.Close()
				t.Fatalf("disabled cutoff changed stopped period: %+v", stoppedGroup)
			}
			assertSettingsPreserved(t, stoppedGroup)
			store.Close()

			reopened, err := storage.Open(ctx, dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			activeGroup, err = reopened.GetGroup(ctx, activeChatID)
			if err != nil {
				t.Fatal(err)
			}
			if !groupAccountingActive(activeGroup, afterMidnight) || currentLedgerDayKey(activeGroup, afterMidnight) != dayKey {
				t.Fatalf("active disabled-cutoff period did not survive midnight/restart: %+v", activeGroup)
			}
			assertSettingsPreserved(t, activeGroup)

			restartedBot := newBot(reopened)
			ledgerCmd, ok := parseLedger("+100")
			if !ok {
				t.Fatal("parseLedger(+100) failed")
			}
			recordMessageID := int64(400 + index)
			if err := restartedBot.handleLedger(ctx, message(activeChatID, recordMessageID, "+100"), owner, ledgerCmd, afterMidnight); err != nil {
				t.Fatal(err)
			}
			record, ok, err := reopened.FindRecordByMessage(ctx, activeChatID, recordMessageID)
			if err != nil || !ok {
				t.Fatalf("find post-midnight record: ok=%v err=%v", ok, err)
			}
			if record.DayKey != dayKey || !record.PeriodStartedAt.Equal(periodStart) {
				t.Fatalf("post-midnight record left continuous period: %+v", record)
			}

			stoppedGroup, err = reopened.GetGroup(ctx, stoppedChatID)
			if err != nil {
				t.Fatal(err)
			}
			if stoppedGroup.Active || groupAccountingActive(stoppedGroup, afterMidnight) {
				t.Fatalf("stopped group was restarted by disabling cutoff: %+v", stoppedGroup)
			}
			assertSettingsPreserved(t, stoppedGroup)
		})
	}
}

func TestPostgresBillRenderSeesCommitFromAnotherProcessCachePath(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := storage.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	loc := time.FixedZone("Asia/Shanghai", 8*3600)
	now := time.Now().In(loc).Truncate(time.Microsecond)
	chatID := -1009300000000 - now.UnixNano()%1000000
	dayKey := businessDayKey(now, 0)
	periodStart := now.Add(-time.Hour)
	if err := store.EnsureGroup(ctx, chatID, "cache visibility", periodStart); err != nil {
		t.Fatal(err)
	}
	if err := store.SetGroupActivePeriod(ctx, chatID, true, dayKey, dayKey, periodStart); err != nil {
		t.Fatal(err)
	}
	insert := func(source int64, marker string) {
		if _, _, insertErr := store.InsertLedgerRecordWithOutbox(ctx, storage.Record{
			ChatID: chatID, DayKey: dayKey, PeriodStartedAt: periodStart,
			Kind: "deposit", Currency: "CNY", Amount: "1", Rate: "1", FeeRate: "0", ResultUSDT: "1",
			SubjectUserID: source, SubjectName: marker, ActorUserID: source, ActorName: marker,
			SourceMessageID: source, CreatedAt: now.Add(time.Duration(source) * time.Microsecond),
		}, storage.NotificationOutbox{
			Kind: "ledger_bill", DedupeKey: "cache-visibility:" + marker + ":" + formatID(chatID),
			ChatID: chatID, ReferenceKind: "ledger_record", Priority: 0,
		}, now, true); insertErr != nil {
			t.Fatal(insertErr)
		}
	}
	insert(1, "first-process-marker")
	b := New(config.Config{
		Timezone: "Asia/Shanghai", QueueSize: 16, GroupCacheTTL: time.Minute,
		BillSummaryCacheTTL: time.Minute, LedgerSummaryWriteMode: "shadow", LedgerSummaryReadMode: "safe",
	}, store, nil, nil, nil)
	first, _, err := b.renderBillMessageForTime(ctx, chatID, now, "")
	if err != nil || !strings.Contains(first, "first-process-marker") {
		t.Fatalf("first render = %q, %v", first, err)
	}
	insert(2, "second-process-marker")
	second, _, err := b.renderBillMessageForTime(ctx, chatID, now, "")
	if err != nil || !strings.Contains(second, "first-process-marker") || !strings.Contains(second, "second-process-marker") {
		t.Fatalf("render after external commit stayed stale = %q, %v", second, err)
	}
}
