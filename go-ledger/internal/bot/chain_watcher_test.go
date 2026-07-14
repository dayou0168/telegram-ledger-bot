package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/chainclient"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/chainwatcher"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/config"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
	"github.com/jackc/pgx/v5"
)

func TestChainWatcherClaimWritesCriticalOutboxKicksAndAcknowledges(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	schema := fmt.Sprintf("bot_chain_outbox_%d", time.Now().UnixNano())
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quotedSchema); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = admin.Exec(context.Background(), "DROP SCHEMA "+quotedSchema+" CASCADE") }()
	migrationURL, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := migrationURL.Query()
	query.Set("search_path", schema)
	migrationURL.RawQuery = query.Encode()
	store, err := storage.Open(ctx, migrationURL.String())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	suffix := fmt.Sprint(time.Now().UnixNano())
	event := chainwatcher.MatchedEvent{
		DeliveryID: "delivery-" + suffix, EventID: "event-" + suffix, BotID: "bot-test",
		ChatID: 88001, OwnerUserID: 88001, WatchAddress: "TWatch" + suffix,
		Direction: "income", TxHash: "tx-" + suffix, From: "TFrom", To: "TWatch" + suffix,
		Value: "1000000", TokenSymbol: "USDT", TokenAddress: "TR7", TokenDecimals: 6,
		BlockTimestamp: time.Now().UTC().UnixMilli(),
	}
	acked := make(chan chainwatcher.AckRequest, 1)
	watcherServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/events/claim":
			_ = json.NewEncoder(w).Encode(chainwatcher.ClaimResponse{Events: []chainwatcher.MatchedEvent{event}})
		case "/v1/events/ack":
			var request chainwatcher.AckRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			acked <- request
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer watcherServer.Close()
	cfg := config.Config{
		ChainWatcherURL: watcherServer.URL, ChainWatcherBotID: "bot-test", ChainWatcherSecret: "test-secret",
		ChainWatcherBatchSize: 10, BotWatcherClaimTimeout: time.Second,
	}
	bot := &Bot{
		cfg: cfg, store: store, loc: time.UTC, notificationWake: make(chan struct{}, 1),
		watcher: chainclient.New(cfg.ChainWatcherURL, cfg.ChainWatcherBotID, cfg.ChainWatcherSecret, time.Second),
	}
	timing, err := bot.pollChainWatcherEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if timing.EventCount != 1 || timing.AckedCount != 1 {
		t.Fatalf("claim timing = %+v, want one event acknowledged", timing)
	}
	select {
	case <-bot.notificationWake:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("critical outbox insert did not immediately kick the send scheduler")
	}
	if timing.OutboxDuration <= 0 {
		t.Fatalf("outbox timing was not recorded: %+v", timing)
	}
	select {
	case request := <-acked:
		if len(request.DeliveryIDs) != 1 || request.DeliveryIDs[0] != event.DeliveryID {
			t.Fatalf("ack request = %+v", request)
		}
	case <-time.After(time.Second):
		t.Fatal("watcher delivery was not acknowledged after outbox commit")
	}
	items, err := store.ClaimDueNotifications(ctx, 100, 8, time.Now().UTC().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range items {
		if item.Kind == "chain" && item.DedupeKey == "chain:88001:"+event.WatchAddress+":"+event.EventID+":income" {
			found = true
			if item.Priority != 0 {
				t.Fatalf("chain outbox priority = %d, want critical priority 0", item.Priority)
			}
		}
	}
	if !found {
		t.Fatalf("critical chain outbox row not found in %d claimed notifications", len(items))
	}
}

func TestWatcherFallbackControllerStateMachineAndRecovery(t *testing.T) {
	now := time.Unix(1000, 0)
	controller := newWatcherFallbackControllerWithRecovery(3, 2, 5*time.Second)

	controller.recordFailure("ready", now)
	if mode := controller.snapshot(now).Mode; mode != fallbackModePending {
		t.Fatalf("first failure mode = %s, want pending", mode)
	}
	controller.recordFailure("claim", now.Add(time.Second))
	if mode := controller.snapshot(now.Add(time.Second)).Mode; mode != fallbackModePending {
		t.Fatalf("second failure mode = %s, want pending", mode)
	}
	controller.recordFailure("ready", now.Add(2*time.Second))
	controller.recordFailure("ready", now.Add(3*time.Second))
	state := controller.snapshot(now.Add(3 * time.Second))
	if state.Mode != fallbackModePending || !state.LeaseRequested {
		t.Fatalf("fallback before lease = %+v, want pending lease request", state)
	}
	controller.activateLease(now.Add(3 * time.Second))
	if mode := controller.snapshot(now.Add(time.Hour)).Mode; mode != fallbackModeActive {
		t.Fatalf("fallback after lease = %s, want active", mode)
	}

	controller.recordSuccess("ready", now.Add(time.Hour+time.Second), time.Second)
	if mode := controller.snapshot(now.Add(12 * time.Second)).Mode; mode != fallbackModeRecovery {
		t.Fatalf("first recovery mode = %s, want recovering", mode)
	}
	controller.recordSuccess("ready", now.Add(time.Hour+2*time.Second), time.Second)
	controller.recordSuccess("claim", now.Add(time.Hour+3*time.Second), time.Second)
	controller.recordSuccess("ready", now.Add(time.Hour+4*time.Second), 10*time.Second)
	if controller.recordSuccess("claim", now.Add(time.Hour+4*time.Second), 0) {
		t.Fatal("high watcher lag completed recovery")
	}
	if !controller.recordSuccess("ready", now.Add(time.Hour+5*time.Second), time.Second) {
		t.Fatal("ready/claim successes with low lag did not complete recovery")
	}
	if mode := controller.snapshot(now.Add(time.Hour + 5*time.Second)).Mode; mode != fallbackModePrimary {
		t.Fatalf("recovered mode = %s, want primary", mode)
	}
}

func TestSharedSubscriptionPreservesBaselineAndDisablesUnsupportedTRX(t *testing.T) {
	bot := &Bot{cfg: config.Config{ChainWatcherBotID: "bot-a"}}
	sub := bot.sharedSubscription(storage.WatchTarget{
		OwnerUserID: 10, Address: "TAddress", WatchIncome: true, NotifyTRX: true, BaselineTimestamp: 1234,
	})
	if sub.BotID != "bot-a" || sub.ChatID != 10 || sub.BaselineTimestamp != 1234 || sub.NotifyTRX {
		t.Fatalf("shared subscription = %+v", sub)
	}
}

func TestFallbackPollBackoffStepsAndRecovers(t *testing.T) {
	bot := &Bot{}
	var previous time.Duration
	for i, want := range []time.Duration{time.Second, 2 * time.Second, 3 * time.Second, 5 * time.Second, 10 * time.Second} {
		before := time.Now()
		bot.recordFallbackPollResult(errors.New("429"))
		delay := time.Until(time.Unix(0, bot.fallbackNextPoll.Load()))
		if delay < want-100*time.Millisecond || delay > want+100*time.Millisecond {
			t.Fatalf("step %d delay = %v, want %v", i, delay, want)
		}
		if time.Since(before) > 100*time.Millisecond {
			t.Fatal("backoff calculation blocked")
		}
		previous = delay
	}
	bot.recordFallbackPollResult(nil)
	delay := time.Until(time.Unix(0, bot.fallbackNextPoll.Load()))
	if delay >= previous {
		t.Fatalf("successful fallback poll did not reduce backoff: %v >= %v", delay, previous)
	}
}

func TestWatcherFallbackControllerEmptySuccessfulClaimsDoNotFail(t *testing.T) {
	now := time.Unix(2000, 0)
	controller := newWatcherFallbackControllerWithRecovery(3, 2, 5*time.Second)
	for i := 0; i < 10; i++ {
		controller.recordSuccess("claim", now.Add(time.Duration(i)*time.Second), 0)
	}
	if mode := controller.snapshot(now.Add(10 * time.Second)).Mode; mode != fallbackModePrimary {
		t.Fatalf("empty successful claims mode = %s, want primary", mode)
	}
}

func TestWatcherFallbackReadyAndClaimFailuresRecoverIndependently(t *testing.T) {
	now := time.Unix(3000, 0)
	controller := newWatcherFallbackControllerWithRecovery(3, 2, 5*time.Second)
	controller.recordFailure("claim", now)
	controller.recordFailure("claim", now.Add(2*time.Second))
	controller.recordFailure("claim", now.Add(3*time.Second))
	if !controller.snapshot(now.Add(3 * time.Second)).LeaseRequested {
		t.Fatal("claim failures did not request fallback lease")
	}
	controller.recordSuccess("ready", now.Add(4*time.Second), 0)
	state := controller.snapshot(now.Add(4 * time.Second))
	if state.Mode != fallbackModePending || !state.LeaseRequested {
		t.Fatalf("ready success incorrectly cleared claim failure: %+v", state)
	}
	controller.recordSuccess("claim", now.Add(5*time.Second), 0)
	if state := controller.snapshot(now.Add(5 * time.Second)); state.Mode != fallbackModePrimary {
		t.Fatalf("both sources recovered state = %+v", state)
	}
}

func TestWatcherFallbackLeaseRequestLoggingIsBounded(t *testing.T) {
	now := time.Unix(4000, 0)
	controller := newWatcherFallbackControllerWithRecovery(2, 2, 5*time.Second)
	first := controller.recordFailure("ready", now)
	if !first.ModeChanged || first.Mode != fallbackModePending {
		t.Fatalf("first notice = %+v", first)
	}
	requested := controller.recordFailure("ready", now.Add(3*time.Second))
	if !requested.LeaseRequested {
		t.Fatalf("threshold notice = %+v", requested)
	}
	for second := 4; second < 63; second++ {
		notice := controller.recordFailure("ready", now.Add(time.Duration(second)*time.Second))
		if notice.LeaseRequested || notice.SuppressedFailures != 0 {
			t.Fatalf("unexpected per-second lease log at second %d: %+v", second, notice)
		}
	}
	summary := controller.recordFailure("ready", now.Add(63*time.Second))
	if summary.SuppressedFailures != 60 || summary.LeaseRequested {
		t.Fatalf("periodic summary = %+v, want 60 suppressed failures", summary)
	}
	controller.activateLease(now.Add(63 * time.Second))
	for second := 64; second < 123; second++ {
		notice := controller.recordFailure("ready", now.Add(time.Duration(second)*time.Second))
		if notice.SuppressedFailures != 0 {
			t.Fatalf("active fallback logged before summary interval at second %d: %+v", second, notice)
		}
	}
	activeSummary := controller.recordFailure("ready", now.Add(123*time.Second))
	if activeSummary.SuppressedFailures != 60 {
		t.Fatalf("active fallback summary = %+v, want 60", activeSummary)
	}
}
