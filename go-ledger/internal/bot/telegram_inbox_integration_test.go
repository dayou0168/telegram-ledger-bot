package bot

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/config"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/telegram"
)

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
	cfg := config.Config{
		TelegramBotToken: "integration-inbox-token", Timezone: "Asia/Shanghai", QueueSize: 8,
		LedgerWorkers: 1, ControlWorkers: 1, ChainWorkers: 1, RateWorkers: 1,
		BroadcastWorkers: 1, QueryWorkers: 1, NotifyWorkers: 2,
		GroupCacheTTL: time.Minute, BillSummaryCacheTTL: time.Minute, UserTouchCacheTTL: time.Minute,
		OperatorCacheTTL: time.Minute, WatchCacheTTL: time.Minute, P2PCacheTTL: time.Minute,
	}
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
