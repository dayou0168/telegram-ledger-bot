package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/config"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/telegram"
)

func telegramInboxIntegrationConfig(token string) config.Config {
	return config.Config{
		TelegramBotToken: token, Timezone: "Asia/Shanghai", QueueSize: 8,
		LedgerWorkers: 1, ControlWorkers: 1, ChainWorkers: 1, RateWorkers: 1,
		BroadcastWorkers: 1, QueryWorkers: 1, NotifyWorkers: 2,
		GroupCacheTTL: time.Minute, BillSummaryCacheTTL: time.Minute, UserTouchCacheTTL: time.Minute,
		OperatorCacheTTL: time.Minute, WatchCacheTTL: time.Minute, P2PCacheTTL: time.Minute,
	}
}

func TestPostgresTelegramInboxHandlerErrorRetriesThenCompletes(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	store, err := storage.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := telegramInboxIntegrationConfig("integration-inbox-token")
	b := New(cfg, store, nil, nil, nil)
	var calls atomic.Int32
	b.updateHandler = func(context.Context, telegram.Update) error {
		if calls.Add(1) == 1 {
			return errors.New("injected handler failure")
		}
		return nil
	}
	update := telegram.Update{UpdateID: time.Now().UnixNano(), Message: &telegram.Message{
		MessageID: 1, Chat: telegram.Chat{ID: -991, Type: "supergroup"}, From: &telegram.User{ID: 1}, Text: "+1",
	}}
	payload, err := json.Marshal(update)
	if err != nil {
		t.Fatal(err)
	}
	stream := b.telegramInboxStreamKey()
	now := time.Now().UTC()
	if _, err := store.PersistTelegramUpdateBatch(ctx, stream, []storage.TelegramInboxUpdate{{
		UpdateID: update.UpdateID, Payload: payload, Lane: "ledger", RouteKey: "ledger:-991",
	}}, now); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimTelegramUpdates(ctx, stream, "ledger", "test-owner", 1, telegramInboxMaxAttempts, time.Minute, now)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("first claim=%d err=%v", len(claimed), err)
	}
	b.handleAdmittedUpdate(ctx, claimed[0], update, "test-owner", updateAdmissionLedger, time.Now())
	time.Sleep(telegramInboxRetryDelay(1) + 50*time.Millisecond)
	claimed, err = store.ClaimTelegramUpdates(ctx, stream, "ledger", "test-owner", 1, telegramInboxMaxAttempts, time.Minute, time.Now().UTC())
	if err != nil || len(claimed) != 1 || claimed[0].Attempts != 2 {
		t.Fatalf("retry claim=%v err=%v", claimed, err)
	}
	b.handleAdmittedUpdate(ctx, claimed[0], update, "test-owner", updateAdmissionLedger, time.Now())
	if calls.Load() != 2 {
		t.Fatalf("handler calls=%d", calls.Load())
	}
	stats, err := store.TelegramInboxStats(ctx, stream, time.Now().UTC())
	if err != nil || stats.Pending != 0 || stats.Processing != 0 || stats.Retry != 0 || stats.Dead != 0 {
		t.Fatalf("final stats=%+v err=%v", stats, err)
	}
}

func TestPostgresTelegramPrivateStateEvolvesInBatchAndSurvivesRestart(t *testing.T) {
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

	base := time.Now().UnixNano()
	userID := base%1_000_000_000 + 10_000
	token := fmt.Sprintf("private-state-%d", base)
	cfg := telegramInboxIntegrationConfig(token)
	bots := []*Bot{New(cfg, storeA, nil, nil, nil), New(cfg, storeB, nil, nil, nil)}
	updates := []telegram.Update{
		privateCallbackUpdate(base+1, userID, "enter-quick-reply"),
		privateMessageUpdate(base+2, userID, "exit-quick-reply"),
		privateMessageUpdate(base+3, userID, "toggle-notify"),
		privateCallbackUpdate(base+4, userID, "switch-target"),
		privateCallbackUpdate(base+5, userID, "enter-cleanup"),
		privateMessageUpdate(base+6, userID, "finish-cleanup"),
	}
	if next, err := bots[0].persistTelegramUpdateBatch(ctx, updates); err != nil || next != base+7 {
		t.Fatalf("persist private batch next=%d err=%v", next, err)
	}

	step := 0
	installHandler := func(b *Bot) {
		b.updateHandler = func(_ context.Context, update telegram.Update) error {
			step++
			key := formatID(userID)
			state, exists := b.privateStates.Get(key)
			switch step {
			case 1:
				if exists {
					t.Fatalf("step 1 restored unexpected state: %+v", state)
				}
				b.privateStates.Set(key, privateState{
					Mode: "quick_reply", TargetName: "source", ChatIDs: []int64{-101, -102}, NotifyAll: false,
					ControlMessageID: 41, WatchAddress: "TSource", QuickReplyTargetChat: -202,
					QuickReplyMessageID: 42, ReturnMode: "group", ReturnTargetName: "saved",
					ReturnChatIDs: []int64{-101, -102}, ReturnNotifyAll: true, ReturnControlMessageID: 43,
					CreatedAt: time.Now().UTC(),
				})
			case 2:
				if !exists || state.Mode != "quick_reply" || state.QuickReplyTargetChat != -202 ||
					state.ReturnMode != "group" || !state.ReturnNotifyAll || len(state.ReturnChatIDs) != 2 {
					t.Fatalf("step 2 did not observe quick-reply predecessor: %+v exists=%v", state, exists)
				}
				b.privateStates.Set(key, privateState{Mode: "group", TargetName: "saved", ChatIDs: []int64{-101, -102}, CreatedAt: time.Now().UTC()})
			case 3:
				if !exists || state.Mode != "group" || state.NotifyAll {
					t.Fatalf("step 3 did not observe restored broadcast state: %+v exists=%v", state, exists)
				}
				state.NotifyAll = true
				b.privateStates.Set(key, state)
			case 4:
				if !exists || state.Mode != "group" || !state.NotifyAll || len(state.ChatIDs) != 2 {
					t.Fatalf("step 4 restart did not restore notify state: %+v exists=%v", state, exists)
				}
				b.privateStates.Set(key, privateState{Mode: "chat", TargetName: "next", ChatIDs: []int64{-303}, NotifyAll: true, CreatedAt: time.Now().UTC()})
			case 5:
				if !exists || state.Mode != "chat" || state.TargetName != "next" || state.ChatIDs[0] != -303 {
					t.Fatalf("step 5 did not observe switched target: %+v exists=%v", state, exists)
				}
				b.privateStates.Set(key, privateState{Mode: "watch_target_label", WatchAddress: "TNext", CreatedAt: time.Now().UTC()})
			case 6:
				if !exists || state.Mode != "watch_target_label" || state.WatchAddress != "TNext" {
					t.Fatalf("step 6 did not restore cleanup session: %+v exists=%v", state, exists)
				}
				b.privateStates.Delete(key)
			default:
				t.Fatalf("unexpected handler step %d update=%d", step, update.UpdateID)
			}
			return nil
		}
	}
	installHandler(bots[0])
	installHandler(bots[1])

	stream := bots[0].telegramInboxStreamKey()
	for index, want := range updates {
		activeBot := bots[0]
		owner := "private-owner-a"
		if index >= 3 {
			activeBot = bots[1]
			owner = "private-owner-b"
		}
		claimed, err := activeBot.store.ClaimTelegramUpdates(ctx, stream, "ledger", owner, 10,
			telegramInboxMaxAttempts, time.Minute, time.Now().UTC())
		if err != nil || len(claimed) != 1 || claimed[0].UpdateID != want.UpdateID {
			t.Fatalf("claim step %d ids=%v err=%v", index+1, telegramInboxUpdateIDs(claimed), err)
		}
		var durable durableTelegramPayload
		if err := json.Unmarshal(claimed[0].Payload, &durable); err != nil {
			t.Fatal(err)
		}
		if durable.LegacyPrivateState != nil {
			t.Fatalf("step %d persisted stale private snapshot", index+1)
		}
		activeBot.handleAdmittedUpdate(ctx, claimed[0], durable.Update, owner, updateAdmissionLedger, time.Now())
	}
	if step != len(updates) {
		t.Fatalf("handler steps=%d want=%d", step, len(updates))
	}
	state, found, err := storeA.GetTelegramPrivateRouteState(ctx, stream, userID)
	if err != nil || !found || state.HasState || state.VersionUpdateID != updates[len(updates)-1].UpdateID {
		t.Fatalf("final private tombstone=%+v found=%v err=%v", state, found, err)
	}
}

func TestPostgresTelegramInboxLeaseCoversHandledAndDoneAcknowledgements(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
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

	base := time.Now().UnixNano()
	b := New(telegramInboxIntegrationConfig(fmt.Sprintf("lease-ack-%d", base)), storeA, nil, nil, nil)
	// One second is short enough to exercise repeated renewals while leaving
	// enough room for Windows scheduler and local PostgreSQL latency variance.
	b.telegramInboxLease = time.Second
	b.updateHandler = func(context.Context, telegram.Update) error { return nil }
	markEntered, completeEntered := make(chan struct{}), make(chan struct{})
	var markOnce, completeOnce sync.Once
	var allowMark, allowComplete atomic.Bool
	b.inboxMarkHandled = func(callCtx context.Context, item storage.TelegramInboxUpdate, owner string, now time.Time) (bool, error) {
		if !allowMark.Load() {
			markOnce.Do(func() { close(markEntered) })
			return false, errors.New("injected mark-handled database failure")
		}
		return storeA.MarkTelegramUpdateHandled(callCtx, item.StreamKey, item.UpdateID, owner, now)
	}
	b.inboxComplete = func(callCtx context.Context, item storage.TelegramInboxUpdate, owner string, now time.Time) (bool, error) {
		if !allowComplete.Load() {
			completeOnce.Do(func() { close(completeEntered) })
			return false, errors.New("injected complete database failure")
		}
		return storeA.CompleteTelegramUpdate(callCtx, item.StreamKey, item.UpdateID, owner, now)
	}
	update := telegram.Update{UpdateID: base + 1, Message: &telegram.Message{
		MessageID: 1, Chat: telegram.Chat{ID: -8801, Type: "supergroup"}, From: &telegram.User{ID: 8801}, Text: "+1",
	}}
	if _, err := b.persistTelegramUpdateBatch(ctx, []telegram.Update{update}); err != nil {
		t.Fatal(err)
	}
	stream := b.telegramInboxStreamKey()
	claimed, err := storeA.ClaimTelegramUpdates(ctx, stream, "ledger", "lease-owner-a", 1,
		telegramInboxMaxAttempts, b.telegramInboxLease, time.Now().UTC())
	if err != nil || len(claimed) != 1 {
		t.Fatalf("initial claim=%v err=%v", telegramInboxUpdateIDs(claimed), err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.handleAdmittedUpdate(ctx, claimed[0], update, "lease-owner-a", updateAdmissionLedger, time.Now())
	}()

	waitInboxTestSignal(t, markEntered, "mark-handled hook")
	time.Sleep(2 * b.telegramInboxLease)
	assertTelegramInboxNotReclaimed(t, ctx, storeB, stream, "lease-owner-b")
	allowMark.Store(true)
	waitInboxTestSignal(t, completeEntered, "complete hook")
	time.Sleep(2 * b.telegramInboxLease)
	assertTelegramInboxNotReclaimed(t, ctx, storeB, stream, "lease-owner-b")
	allowComplete.Store(true)
	waitInboxTestSignal(t, done, "handler completion")
	stats, err := storeB.TelegramInboxStats(ctx, stream, time.Now().UTC())
	if err != nil || stats.Pending != 0 || stats.Processing != 0 || stats.Retry != 0 || stats.Dead != 0 {
		t.Fatalf("final inbox stats=%+v err=%v", stats, err)
	}
}

func TestPostgresTelegramLegacyPrivateSnapshotOnlyBootstrapsFirstUpdate(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	store, err := storage.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	base := time.Now().UnixNano()
	userID := base%1_000_000_000 + 20_000
	b := New(telegramInboxIntegrationConfig(fmt.Sprintf("legacy-private-%d", base)), store, nil, nil, nil)
	updates := []telegram.Update{
		privateMessageUpdate(base+1, userID, "first"),
		privateMessageUpdate(base+2, userID, "second"),
	}
	legacy := privateState{Mode: "quick_reply", TargetName: "legacy", ChatIDs: []int64{-1}, CreatedAt: time.Now().UTC()}
	items := make([]storage.TelegramInboxUpdate, 0, len(updates))
	for _, update := range updates {
		payload, err := json.Marshal(durableTelegramPayload{Version: 1, Update: update, LegacyPrivateState: &legacy})
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, storage.TelegramInboxUpdate{UpdateID: update.UpdateID, Payload: payload, Lane: "ledger", RouteKey: fmt.Sprintf("private:%d", userID)})
	}
	stream := b.telegramInboxStreamKey()
	if _, err := store.PersistTelegramUpdateBatch(ctx, stream, items, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	step := 0
	b.updateHandler = func(context.Context, telegram.Update) error {
		step++
		state, ok := b.privateStates.Get(formatID(userID))
		if !ok {
			t.Fatalf("step %d did not restore private state", step)
		}
		if step == 1 {
			if state.TargetName != "legacy" {
				t.Fatalf("first update did not bootstrap legacy state: %+v", state)
			}
			state.TargetName = "database-wins"
			b.privateStates.Set(formatID(userID), state)
		} else if state.TargetName != "database-wins" {
			t.Fatalf("later legacy snapshot rolled state back: %+v", state)
		}
		return nil
	}
	for _, want := range updates {
		claimed, err := store.ClaimTelegramUpdates(ctx, stream, "ledger", "legacy-owner", 10,
			telegramInboxMaxAttempts, time.Minute, time.Now().UTC())
		if err != nil || len(claimed) != 1 || claimed[0].UpdateID != want.UpdateID {
			t.Fatalf("legacy claim ids=%v err=%v", telegramInboxUpdateIDs(claimed), err)
		}
		var durable durableTelegramPayload
		if err := json.Unmarshal(claimed[0].Payload, &durable); err != nil {
			t.Fatal(err)
		}
		guard := b.startTelegramInboxLeaseGuard(ctx, claimed[0], "legacy-owner", time.Minute)
		guard.legacyPrivateState = durable.LegacyPrivateState
		b.handleAdmittedUpdate(ctx, claimed[0], durable.Update, "legacy-owner", updateAdmissionLedger, time.Now(), guard)
	}
}

func privateMessageUpdate(updateID, userID int64, text string) telegram.Update {
	return telegram.Update{UpdateID: updateID, Message: &telegram.Message{
		MessageID: updateID, Chat: telegram.Chat{ID: userID, Type: "private"}, From: &telegram.User{ID: userID}, Text: text,
	}}
}

func privateCallbackUpdate(updateID, userID int64, data string) telegram.Update {
	return telegram.Update{UpdateID: updateID, CallbackQuery: &telegram.CallbackQuery{
		ID: fmt.Sprintf("callback-%d", updateID), From: telegram.User{ID: userID}, Data: data,
		Message: &telegram.Message{MessageID: updateID, Chat: telegram.Chat{ID: userID, Type: "private"}},
	}}
}

func telegramInboxUpdateIDs(items []storage.TelegramInboxUpdate) []int64 {
	ids := make([]int64, len(items))
	for index := range items {
		ids[index] = items[index].UpdateID
	}
	return ids
}

func waitInboxTestSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func assertTelegramInboxNotReclaimed(t *testing.T, ctx context.Context, store *storage.Store, stream, owner string) {
	t.Helper()
	claimed, err := store.ClaimTelegramUpdates(ctx, stream, "ledger", owner, 1,
		telegramInboxMaxAttempts, time.Minute, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 0 {
		t.Fatalf("second owner reclaimed active lease: %v", telegramInboxUpdateIDs(claimed))
	}
}
