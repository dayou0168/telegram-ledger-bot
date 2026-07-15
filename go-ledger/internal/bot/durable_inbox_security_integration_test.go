package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/adminauth"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/config"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/telegram"
	"github.com/jackc/pgx/v5"
)

func TestPostgresDurableInboxUndoUsesExecutionLedgerPeriod(t *testing.T) {
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
	executionNow := time.Now().In(loc).Truncate(time.Microsecond)
	eventTime := executionNow.Add(-24 * time.Hour)
	oldDay := eventTime.Format("2006-01-02")
	base := executionNow.UnixNano()
	chatID, userID := -base, base%1_000_000_000+100_000
	sourceMessageID, undoMessageID := base+1, base+2
	if err := store.EnsureGroup(ctx, chatID, "delayed undo", eventTime); err != nil {
		t.Fatal(err)
	}
	if err := store.SetGroupCutoffState(ctx, chatID, 0, true, oldDay, oldDay, eventTime); err != nil {
		t.Fatal(err)
	}
	if err := store.AddOperator(ctx, chatID, storage.User{ID: userID, DisplayName: "operator"}, userID, eventTime); err != nil {
		t.Fatal(err)
	}
	recordID, err := store.InsertRecord(ctx, storage.Record{
		ChatID: chatID, DayKey: oldDay, PeriodStartedAt: eventTime,
		Kind: "deposit", Currency: "CNY", Amount: "100", Rate: "1", FeeRate: "0", ResultUSDT: "100",
		ActorUserID: userID, ActorName: "operator", SubjectUserID: userID, SubjectName: "operator",
		SourceMessageID: sourceMessageID, CreatedAt: eventTime.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}

	b := New(telegramInboxIntegrationConfig(fmt.Sprintf("delayed-undo-%d", base)), store, nil, nil, nil)
	update := telegram.Update{UpdateID: base + 10, Message: &telegram.Message{
		MessageID: undoMessageID, Chat: telegram.Chat{ID: chatID, Type: "supergroup", Title: "delayed undo"},
		From: &telegram.User{ID: userID, FirstName: "operator"}, Text: "撤销",
		ReplyTo: &telegram.Message{MessageID: sourceMessageID},
	}}
	persistAndHandleTelegramInboxUpdate(t, ctx, b, store, update, eventTime, "delayed-undo-owner")
	record, ok, err := store.GetRecord(ctx, recordID)
	if err != nil || !ok || record.DeletedAt != nil {
		t.Fatalf("delayed undo crossed cutoff: record=%+v ok=%v err=%v", record, ok, err)
	}
	items, err := store.ClaimDueNotifications(ctx, 100, 5, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	foundClosed := false
	for _, item := range items {
		if item.ChatID == chatID && strings.Contains(item.DedupeKey, "ledger_undo_period_closed") {
			foundClosed = true
			break
		}
	}
	if !foundClosed {
		t.Fatalf("delayed undo did not emit closed-period result: %+v", items)
	}
}

func TestPostgresDurableInboxClearTicketStartsAtExecutionTime(t *testing.T) {
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
	eventTime := now.Add(-5 * time.Minute)
	dayKey := now.Format("2006-01-02")
	base := now.UnixNano()
	chatID, userID := -base-1, base%1_000_000_000+200_000
	if err := store.EnsureGroup(ctx, chatID, "delayed clear", now); err != nil {
		t.Fatal(err)
	}
	if err := store.SetGroupCutoffState(ctx, chatID, cutoffDisabledHour, true, dayKey, "", now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.AddOperator(ctx, chatID, storage.User{ID: userID, DisplayName: "operator"}, userID, now); err != nil {
		t.Fatal(err)
	}
	group, err := store.GetGroup(ctx, chatID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.InsertRecord(ctx, storage.Record{
		ChatID: chatID, DayKey: dayKey, PeriodStartedAt: group.ActivePeriodStartedAt,
		Kind: "deposit", Currency: "CNY", Amount: "1", Rate: "1", FeeRate: "0", ResultUSDT: "1",
		ActorUserID: userID, SubjectUserID: userID, CreatedAt: now.Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	cfg := telegramInboxIntegrationConfig(fmt.Sprintf("delayed-clear-%d", base))
	b := New(cfg, store, nil, nil, nil)
	update := privateLedgerUpdate(base+20, chatID, userID, "清除当前账期")
	beforeExecution := time.Now().In(loc)
	persistAndHandleTelegramInboxUpdate(t, ctx, b, store, update, eventTime, "delayed-clear-owner")
	afterExecution := time.Now().In(loc)
	tokenCtx := context.WithValue(ctx, telegramUpdateIDContextKey{}, update.UpdateID)
	token, err := b.ledgerClearToken(tokenCtx, chatID, userID)
	if err != nil {
		t.Fatal(err)
	}
	ticket, ok, err := store.GetLedgerClearTicket(ctx, adminauth.HashToken(token))
	if err != nil || !ok {
		t.Fatalf("clear ticket found=%v err=%v", ok, err)
	}
	if ticket.CreatedAt.Before(beforeExecution.Add(-time.Second)) || ticket.CreatedAt.Before(eventTime.Add(4*time.Minute)) {
		t.Fatalf("ticket used inbox time: event=%s created=%s", eventTime, ticket.CreatedAt)
	}
	if ttl := ticket.ExpiresAt.Sub(ticket.CreatedAt); ttl != ledgerClearTicketTTL {
		t.Fatalf("ticket ttl=%s want=%s", ttl, ledgerClearTicketTTL)
	}
	if remaining := ticket.ExpiresAt.Sub(afterExecution); remaining < 59*time.Second {
		t.Fatalf("delayed clear did not receive full confirmation window: %s", remaining)
	}
}

func TestPostgresDurableQuickReplyRestartRevocationClearsState(t *testing.T) {
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

	fixture := prepareDurableQuickReplyFixture(t, ctx, store, nil)
	seedDurablePrivateState(t, ctx, fixture.bot, store, fixture.seedUpdateID, fixture.userID, fixture.state)
	if disabled, err := store.DisableGlobalOperator(ctx, fixture.userID, fixture.userID, time.Now()); err != nil || !disabled {
		t.Fatalf("disable global operator=%v err=%v", disabled, err)
	}
	restarted := New(fixture.cfg, store, nil, nil, nil)
	update := privateMessageUpdate(fixture.seedUpdateID+1, fixture.userID, "revoked payload")
	persistAndHandleTelegramInboxUpdate(t, ctx, restarted, store, update, time.Now().Add(-time.Minute), "restart-revoked-owner")
	state, found, err := store.GetTelegramPrivateRouteState(ctx, restarted.telegramInboxStreamKey(), fixture.userID)
	if err != nil || !found || state.HasState || state.VersionUpdateID != update.UpdateID {
		t.Fatalf("revoked restart state=%+v found=%v err=%v", state, found, err)
	}
}

func TestPostgresDurableQuickReplyRechecksPermissionAfterQueueWait(t *testing.T) {
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

	var copyCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/copyMessage") {
			copyCalls.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":9001,"chat":{"id":-1,"type":"supergroup"}}}`))
	}))
	defer server.Close()
	tg := telegram.NewClient(server.URL, "test", 2*time.Second)
	fixture := prepareDurableQuickReplyFixture(t, ctx, store, tg)
	seedDurablePrivateState(t, ctx, fixture.bot, store, fixture.seedUpdateID, fixture.userID, fixture.state)

	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	fixture.bot.quickReplyPool.Start(runCtx)
	fixture.bot.sendGateway.Start(runCtx)
	blockStarted, releaseBlock := make(chan struct{}), make(chan struct{})
	blockDone := make(chan struct{})
	go func() {
		defer close(blockDone)
		_, _ = fixture.bot.sendGateway.Do(runCtx, sendPriorityNormal, fixture.targetChatID, func(jobCtx context.Context) (telegram.Message, error) {
			close(blockStarted)
			select {
			case <-releaseBlock:
			case <-jobCtx.Done():
			}
			return telegram.Message{}, nil
		})
	}()
	waitInboxTestSignal(t, blockStarted, "notify blocker")
	go fixture.bot.quickReplyOutboxScheduler(runCtx)
	update := privateMessageUpdate(fixture.seedUpdateID+1, fixture.userID, "queued payload")
	persistAndHandleTelegramInboxUpdate(t, ctx, fixture.bot, store, update, time.Now(), "queued-revoked-owner")
	if removed, err := store.RemoveBroadcastPermission(ctx, fixture.userID, "chat", fixture.targetChatID, "", fixture.userID, time.Now()); err != nil || !removed {
		t.Fatalf("remove broadcast permission=%v err=%v", removed, err)
	}
	close(releaseBlock)
	deadline := time.Now().Add(5 * time.Second)
	for {
		state, found, err := store.GetTelegramPrivateRouteState(ctx, fixture.bot.telegramInboxStreamKey(), fixture.userID)
		if err != nil {
			t.Fatal(err)
		}
		if found && !state.HasState && state.VersionUpdateID == update.UpdateID {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("queued revocation did not clear state: %+v found=%v", state, found)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := copyCalls.Load(); got != 0 {
		t.Fatalf("revoked queued quick reply copied %d messages", got)
	}
	waitInboxTestSignal(t, blockDone, "gateway blocker")
	outbox, found, err := store.GetQuickReplyOutboxByDedupe(ctx,
		fmt.Sprintf("quick_reply:%s:%d", fixture.bot.telegramInboxStreamKey(), update.UpdateID))
	if err != nil || !found || outbox.Status != "cancelled" {
		t.Fatalf("revoked outbox=%+v found=%v err=%v", outbox, found, err)
	}
}

func TestPostgresDurableQuickReplyRestartsAfterCommitBeforeSend(t *testing.T) {
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

	var copyCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/copyMessage") {
			copyCalls.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":9101,"chat":{"id":-1,"type":"supergroup"}}}`))
	}))
	defer server.Close()
	tg := telegram.NewClient(server.URL, "test", 2*time.Second)
	fixture := prepareDurableQuickReplyFixture(t, ctx, store, tg)
	seedDurablePrivateState(t, ctx, fixture.bot, store, fixture.seedUpdateID, fixture.userID, fixture.state)
	update := privateMessageUpdate(fixture.seedUpdateID+1, fixture.userID, "restart payload")
	persistAndHandleTelegramInboxUpdate(t, ctx, fixture.bot, store, update, time.Now(), "pre-crash-owner")
	dedupe := fmt.Sprintf("quick_reply:%s:%d", fixture.bot.telegramInboxStreamKey(), update.UpdateID)
	outbox, found, err := store.GetQuickReplyOutboxByDedupe(ctx, dedupe)
	if err != nil || !found || outbox.Status != "pending" || copyCalls.Load() != 0 {
		t.Fatalf("pre-restart outbox=%+v found=%v copies=%d err=%v", outbox, found, copyCalls.Load(), err)
	}

	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	restarted := New(fixture.cfg, store, tg, nil, nil)
	restarted.quickReplyPool.Start(runCtx)
	restarted.sendGateway.Start(runCtx)
	go restarted.quickReplyOutboxScheduler(runCtx)
	waitQuickReplyOutboxStatus(t, ctx, store, dedupe, "sent")
	if got := copyCalls.Load(); got != 1 {
		t.Fatalf("restart copies=%d want=1", got)
	}
}

func TestPostgresDurableQuickReplyClassifiesTelegramFailures(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	tests := []struct {
		name      string
		firstBody string
		network   bool
		permanent bool
	}{
		{name: "400", firstBody: `{"ok":false,"error_code":400,"description":"Bad Request"}`, permanent: true},
		{name: "403", firstBody: `{"ok":false,"error_code":403,"description":"Forbidden"}`, permanent: true},
		{name: "500", firstBody: `{"ok":false,"error_code":500,"description":"Server Error"}`},
		{name: "network", network: true},
		{name: "429", firstBody: `{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":1}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			store, err := storage.Open(ctx, dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()

			var copyCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				call := copyCalls.Add(1)
				if call == 1 && tc.network {
					panic(http.ErrAbortHandler)
				}
				w.Header().Set("Content-Type", "application/json")
				if call == 1 {
					_, _ = w.Write([]byte(tc.firstBody))
					return
				}
				_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":9201,"chat":{"id":-1,"type":"supergroup"}}}`))
			}))
			defer server.Close()
			tg := telegram.NewClient(server.URL, "test", 2*time.Second)
			fixture := prepareDurableQuickReplyFixture(t, ctx, store, tg)
			seedDurablePrivateState(t, ctx, fixture.bot, store, fixture.seedUpdateID, fixture.userID, fixture.state)
			update := privateMessageUpdate(fixture.seedUpdateID+1, fixture.userID, "classified payload")
			persistAndHandleTelegramInboxUpdate(t, ctx, fixture.bot, store, update, time.Now(), "classified-owner")
			dedupe := fmt.Sprintf("quick_reply:%s:%d", fixture.bot.telegramInboxStreamKey(), update.UpdateID)

			runCtx, stop := context.WithCancel(ctx)
			defer stop()
			fixture.bot.quickReplyPool.Start(runCtx)
			fixture.bot.sendGateway.Start(runCtx)
			go fixture.bot.quickReplyOutboxScheduler(runCtx)
			wantStatus := "sent"
			if tc.permanent {
				wantStatus = "dead"
			}
			outbox := waitQuickReplyOutboxStatus(t, ctx, store, dedupe, wantStatus)
			wantCalls := int32(2)
			wantAttempts := 2
			if tc.permanent {
				wantCalls = 1
				wantAttempts = 1
			}
			if outbox.Attempts != wantAttempts || copyCalls.Load() != wantCalls {
				t.Fatalf("classified outbox=%+v copies=%d", outbox, copyCalls.Load())
			}
			if tc.permanent {
				successor := privateMessageUpdate(update.UpdateID+1, fixture.userID, "successor payload")
				persistAndHandleTelegramInboxUpdate(t, ctx, fixture.bot, store, successor, time.Now(), "classified-successor")
				successorDedupe := fmt.Sprintf("quick_reply:%s:%d", fixture.bot.telegramInboxStreamKey(), successor.UpdateID)
				waitQuickReplyOutboxStatus(t, ctx, store, successorDedupe, "sent")
				if copyCalls.Load() != 2 {
					t.Fatalf("permanent failure blocked successor, copies=%d", copyCalls.Load())
				}
			}
		})
	}
}

func TestPostgresDurableQuickReplyLeaseReclaimSuppressesStaleQueuedCopy(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	storeA, err := storage.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer storeA.Close()
	storeB, err := storage.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer storeB.Close()

	var copyCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		copyCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":9301,"chat":{"id":-1,"type":"supergroup"}}}`))
	}))
	defer server.Close()
	tg := telegram.NewClient(server.URL, "test", 2*time.Second)
	fixture := prepareDurableQuickReplyFixture(t, ctx, storeA, tg)
	fixture.bot.quickReplyLease = 2 * time.Second
	seedDurablePrivateState(t, ctx, fixture.bot, storeA, fixture.seedUpdateID, fixture.userID, fixture.state)
	update := privateMessageUpdate(fixture.seedUpdateID+1, fixture.userID, "lease payload")
	persistAndHandleTelegramInboxUpdate(t, ctx, fixture.bot, storeA, update, time.Now(), "lease-inbox-owner")

	ownerA, ownerB := "quick-owner-a", "quick-owner-b"
	claimedA, err := storeA.ClaimQuickReplyOutbox(ctx, ownerA, 1, quickReplyOutboxMaxAttempt, 2*time.Second, time.Now())
	if err != nil || len(claimedA) != 1 {
		t.Fatalf("claim owner A=%v err=%v", claimedA, err)
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	fixture.bot.sendGateway.Start(runCtx)
	blockStarted, releaseBlock := make(chan struct{}), make(chan struct{})
	go func() {
		_, _ = fixture.bot.sendGateway.Do(runCtx, sendPriorityNormal, fixture.targetChatID, func(opCtx context.Context) (telegram.Message, error) {
			close(blockStarted)
			select {
			case <-releaseBlock:
			case <-opCtx.Done():
			}
			return telegram.Message{}, nil
		})
	}()
	waitInboxTestSignal(t, blockStarted, "normal gateway blocker")
	guardA := fixture.bot.startQuickReplyOutboxLeaseGuard(runCtx, claimedA[0], ownerA)
	oldDone := make(chan struct{})
	go func() {
		defer close(oldDone)
		defer guardA.Stop()
		fixture.bot.sendQuickReplyOutbox(runCtx, claimedA[0], ownerA, guardA)
	}()
	deadline := time.Now().Add(3 * time.Second)
	for fixture.bot.sendGateway.Stats(time.Now()).Normal.Queued < 1 {
		if time.Now().After(deadline) {
			t.Fatal("stale quick reply did not enter normal queue")
		}
		time.Sleep(10 * time.Millisecond)
	}

	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	if _, err := admin.Exec(ctx, `UPDATE telegram_quick_reply_outbox SET lease_until=$2 WHERE id=$1`,
		claimedA[0].ID, time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	claimedB, err := storeB.ClaimQuickReplyOutbox(ctx, ownerB, 1, quickReplyOutboxMaxAttempt, 2*time.Second, time.Now())
	if err != nil || len(claimedB) != 1 || claimedB[0].ID != claimedA[0].ID {
		t.Fatalf("claim owner B=%v err=%v", claimedB, err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for !guardA.Lost() {
		if time.Now().After(deadline) {
			t.Fatal("stale owner did not observe lease loss")
		}
		time.Sleep(10 * time.Millisecond)
	}

	botB := New(fixture.cfg, storeB, tg, nil, nil)
	botB.quickReplyLease = 2 * time.Second
	botB.sendGateway.Start(runCtx)
	guardB := botB.startQuickReplyOutboxLeaseGuard(runCtx, claimedB[0], ownerB)
	botB.sendQuickReplyOutbox(runCtx, claimedB[0], ownerB, guardB)
	guardB.Stop()
	close(releaseBlock)
	waitInboxTestSignal(t, oldDone, "stale queued sender")
	dedupe := fmt.Sprintf("quick_reply:%s:%d", fixture.bot.telegramInboxStreamKey(), update.UpdateID)
	outbox := waitQuickReplyOutboxStatus(t, ctx, storeB, dedupe, "sent")
	if outbox.LeaseOwner != "" || copyCalls.Load() != 1 {
		t.Fatalf("reclaimed outbox=%+v copies=%d", outbox, copyCalls.Load())
	}
}

func waitQuickReplyOutboxStatus(t *testing.T, ctx context.Context, store *storage.Store, dedupe, want string) storage.QuickReplyOutbox {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for {
		item, found, err := store.GetQuickReplyOutboxByDedupe(ctx, dedupe)
		if err != nil {
			t.Fatal(err)
		}
		if found && item.Status == want {
			return item
		}
		if time.Now().After(deadline) {
			t.Fatalf("quick reply outbox status=%q found=%v want=%q item=%+v", item.Status, found, want, item)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

type durableQuickReplyFixture struct {
	bot          *Bot
	cfg          config.Config
	state        privateState
	userID       int64
	targetChatID int64
	seedUpdateID int64
}

func prepareDurableQuickReplyFixture(t *testing.T, ctx context.Context, store *storage.Store, tg *telegram.Client) durableQuickReplyFixture {
	t.Helper()
	base := time.Now().UnixNano()
	userID := base%1_000_000_000 + 300_000
	targetChatID := -base - 100
	seedUpdateID := base + 100
	now := time.Now()
	if err := store.UpsertGlobalOperator(ctx, userID, "primary", 0, userID, "quick reply fixture", now); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureGroup(ctx, targetChatID, "quick reply target", now); err != nil {
		t.Fatal(err)
	}
	if err := store.AddBroadcastPermission(ctx, userID, "chat", targetChatID, "", userID, now); err != nil {
		t.Fatal(err)
	}
	targetMessageID := base + 101
	if _, err := store.InsertBroadcastDelivery(ctx, storage.BroadcastDelivery{
		OperatorUserID: userID, SourceChatID: userID, SourceMessageID: base + 99,
		TargetChatID: targetChatID, TargetTitle: "quick reply target", TargetMessageID: targetMessageID,
		Mode: "chat", TargetName: "quick reply target", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	cfg := telegramInboxIntegrationConfig(fmt.Sprintf("quick-reply-security-%d", base))
	cfg.NotifyWorkers = 1
	cfg.TelegramAPIBase = ""
	if tg != nil {
		cfg.TelegramAPIBase = "test"
	}
	bot := New(cfg, store, tg, nil, nil)
	return durableQuickReplyFixture{
		bot: bot, cfg: cfg, userID: userID, targetChatID: targetChatID, seedUpdateID: seedUpdateID,
		state: privateState{Mode: "quick_reply", TargetName: "quick reply target", QuickReplyTargetChat: targetChatID,
			QuickReplyMessageID: targetMessageID, CreatedAt: now},
	}
}

func seedDurablePrivateState(t *testing.T, ctx context.Context, b *Bot, store *storage.Store, updateID, userID int64, state privateState) {
	t.Helper()
	b.updateHandler = func(context.Context, telegram.Update) error {
		b.privateStates.Set(formatID(userID), state)
		return nil
	}
	persistAndHandleTelegramInboxUpdate(t, ctx, b, store, privateMessageUpdate(updateID, userID, "seed"), time.Now(), "private-state-seed")
	b.updateHandler = nil
}

func persistAndHandleTelegramInboxUpdate(t *testing.T, ctx context.Context, b *Bot, store *storage.Store, update telegram.Update, persistedAt time.Time, owner string) {
	t.Helper()
	payload, err := json.Marshal(durableTelegramPayload{Version: 1, Update: update})
	if err != nil {
		t.Fatal(err)
	}
	key, pool := b.updateRoute(update)
	lane := "ledger"
	admissionLane := updateAdmissionLedger
	if pool == b.queryPool {
		lane = "bypass"
		admissionLane = updateAdmissionBypass
	}
	if _, err := store.PersistTelegramUpdateBatch(ctx, b.telegramInboxStreamKey(), []storage.TelegramInboxUpdate{{
		UpdateID: update.UpdateID, Payload: payload, Lane: lane, RouteKey: key,
	}}, persistedAt); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimTelegramUpdates(ctx, b.telegramInboxStreamKey(), lane, owner, 1,
		telegramInboxMaxAttempts, time.Minute, time.Now())
	if err != nil || len(claimed) != 1 || claimed[0].UpdateID != update.UpdateID {
		t.Fatalf("claim durable update=%v err=%v", telegramInboxUpdateIDs(claimed), err)
	}
	b.handleAdmittedUpdate(ctx, claimed[0], update, owner, admissionLane, time.Now())
}

func privateLedgerUpdate(updateID, chatID, userID int64, text string) telegram.Update {
	return telegram.Update{UpdateID: updateID, Message: &telegram.Message{
		MessageID: updateID, Chat: telegram.Chat{ID: chatID, Type: "supergroup"},
		From: &telegram.User{ID: userID, FirstName: "operator"}, Text: text,
	}}
}
