package tron

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNormalizeAPIKeysDeduplicatesCommaAndNewline(t *testing.T) {
	keys := normalizeAPIKeys([]string{" key1, key2\nkey1\r\nkey3 "})
	if got := strings.Join(keys, ","); got != "key1,key2,key3" {
		t.Fatalf("keys = %q, want key1,key2,key3", got)
	}
}

func TestMainScanPagesRotateAcrossDynamicKeyPool(t *testing.T) {
	var mu sync.Mutex
	round := 0
	rounds := make([]map[string]string, 3)
	for i := range rounds {
		rounds[i] = make(map[string]string)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		rounds[round][r.URL.Query().Get("start")] = r.Header.Get("TRON-PRO-API-KEY")
		mu.Unlock()
		writeFullTransferPage(w)
	}))
	defer server.Close()

	client := NewClientWithKeys(server.URL, []string{"key1", "key2", "key3", "key4", "key5"}, time.Second, KeyPoolOptions{})
	for round = 0; round < 3; round++ {
		if _, err := client.FetchGlobalUSDTTransfersWithMetrics(context.Background(), "TR7", 1, 3); err != nil {
			t.Fatalf("round %d: %v", round+1, err)
		}
	}
	want := []map[string]string{
		{"0": "key1", "50": "key2", "100": "key3"},
		{"0": "key4", "50": "key5", "100": "key1"},
		{"0": "key2", "50": "key3", "100": "key4"},
	}
	for i := range want {
		for page, key := range want[i] {
			if rounds[i][page] != key {
				t.Fatalf("round %d page %s key = %q, want %q; all=%v", i+1, page, rounds[i][page], key, rounds)
			}
		}
	}
}

func TestSingleKey429FailsOverWithinRound(t *testing.T) {
	var mu sync.Mutex
	var headers []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("TRON-PRO-API-KEY")
		mu.Lock()
		headers = append(headers, key)
		mu.Unlock()
		if key == "key1" {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"Error":"frequency limit"}`)
			return
		}
		writeShortTransferPage(w)
	}))
	defer server.Close()

	client := NewClientWithKeys(server.URL, []string{"key1", "key2"}, time.Second, KeyPoolOptions{})
	result, err := client.FetchGlobalUSDTTransfersWithMetrics(context.Background(), "TR7", 1, 1)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if result.Metrics.Calls != 2 {
		t.Fatalf("calls = %d, want 2", result.Metrics.Calls)
	}
	if strings.Join(headers, ",") != "key1,key2" {
		t.Fatalf("headers = %v, want key1,key2", headers)
	}
	status := client.KeyPoolStatus(time.Now())
	if status.Keys[0].RateLimitCount != 1 || status.Keys[0].CooldownUntil == nil {
		t.Fatalf("key1 status = %+v", status.Keys[0])
	}
	if status.TodayMainRequests != 2 || status.TodayFailoverRequests != 1 {
		t.Fatalf("request sources = main:%d failover:%d, want 2/1", status.TodayMainRequests, status.TodayFailoverRequests)
	}
	if status.Keys[0].CooldownUntil.Sub(time.Now()) < 5*time.Second {
		t.Fatalf("Retry-After not preserved: %+v", status.Keys[0])
	}
}

func TestAllKeys429MarksPossibleSharedLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"Error":"frequency limit"}`)
	}))
	defer server.Close()

	client := NewClientWithKeys(server.URL, []string{"secret-key-1", "secret-key-2"}, time.Second, KeyPoolOptions{})
	_, err := client.FetchGlobalUSDTTransfersWithMetrics(context.Background(), "TR7", 1, 1)
	if err == nil {
		t.Fatal("expected all-key 429 failure")
	}
	status := client.KeyPoolStatus(time.Now())
	if !status.PossibleSharedLimit || status.RateLimitedKeys != 2 || status.AvailableCount != 0 {
		t.Fatalf("status = %+v", status)
	}
	raw, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "secret-key") {
		t.Fatalf("status leaked key: %s", raw)
	}
}

func TestAuthFailureDisablesKeyAndFailsOver(t *testing.T) {
	for _, statusCode := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(fmt.Sprint(statusCode), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("TRON-PRO-API-KEY") == "bad" {
					w.WriteHeader(statusCode)
					return
				}
				writeShortTransferPage(w)
			}))
			defer server.Close()

			client := NewClientWithKeys(server.URL, []string{"bad", "good"}, time.Second, KeyPoolOptions{})
			if _, err := client.FetchGlobalUSDTTransfersWithMetrics(context.Background(), "TR7", 1, 1); err != nil {
				t.Fatalf("fetch: %v", err)
			}
			status := client.KeyPoolStatus(time.Now())
			if status.Keys[0].AuthErrorCount != 1 || status.Keys[0].Health != "suspect" || status.Keys[0].NextProbeAt == nil {
				t.Fatalf("bad key status = %+v", status.Keys[0])
			}
		})
	}
}

func TestMainRoundIntervalCompletesBeforeNextSecond(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeShortTransferPage(w)
	}))
	defer server.Close()
	client := NewClientWithKeys(server.URL, []string{"key1"}, time.Second, KeyPoolOptions{MinInterval: 20 * time.Millisecond})
	started := time.Now()
	result, err := client.FetchGlobalUSDTTransfersWithMetrics(context.Background(), "TR7", 1, 3)
	if err != nil {
		t.Fatal(err)
	}
	duration := time.Since(started)
	if result.Metrics.Calls != 3 || duration < 390*time.Millisecond || duration >= time.Second {
		t.Fatalf("round calls/duration = %d/%v, want 3 and 390ms..1s", result.Metrics.Calls, duration)
	}
}

func TestDailyQuotaDisablesUntilReset(t *testing.T) {
	now := time.Date(2026, 7, 12, 3, 0, 0, 0, time.UTC)
	pool := newKeyPool([]string{"key1"}, KeyPoolOptions{BudgetZone: time.UTC})
	pool.now = func() time.Time { return now }
	lease, err := pool.lease(context.Background(), RequestSourceMain, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.reserve(context.Background(), lease, RequestSourceMain, false); err != nil {
		t.Fatal(err)
	}
	if err := pool.report(context.Background(), lease, 429, `Exceed the user daily usage (100000)`, 0, now); err != nil {
		t.Fatal(err)
	}
	status := pool.status(now)
	if status.Keys[0].UnavailableFor != "exhausted" || status.Keys[0].CooldownUntil == nil {
		t.Fatalf("status = %+v", status.Keys[0])
	}
	reset := nextBudgetReset(now, time.UTC)
	if status.Keys[0].CooldownUntil.Before(reset.Add(30*time.Second)) || status.Keys[0].CooldownUntil.After(reset.Add(120*time.Second)) {
		t.Fatalf("cooldown_until = %v, want reset plus 30s..120s", status.Keys[0].CooldownUntil)
	}
}

func TestDailyUsagePersistsAcrossClientRestartWithoutLocalStop(t *testing.T) {
	store := newMemoryUsageStore()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeShortTransferPage(w)
	}))
	defer server.Close()
	opts := KeyPoolOptions{BudgetZone: time.UTC, UsageStore: store}

	first := NewClientWithKeys(server.URL, []string{"key1"}, time.Second, opts)
	if err := first.RestoreKeyPool(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := first.FetchGlobalUSDTTransfersWithMetrics(context.Background(), "TR7", 1, 1); err != nil {
			t.Fatal(err)
		}
	}
	if err := first.FlushKeyUsage(context.Background()); err != nil {
		t.Fatal(err)
	}
	second := NewClientWithKeys(server.URL, []string{"key1"}, time.Second, opts)
	if err := second.RestoreKeyPool(context.Background()); err != nil {
		t.Fatal(err)
	}
	status := second.KeyPoolStatus(time.Now())
	if status.Keys[0].TodayRequests != 2 {
		t.Fatalf("restored status = %+v", status.Keys[0])
	}
	if _, err := second.FetchGlobalUSDTTransfersWithMetrics(context.Background(), "TR7", 1, 1); err != nil {
		t.Fatalf("local usage counter stopped requests: %v", err)
	}
}

func TestPlanningDayResetsAtUTCZero(t *testing.T) {
	now := time.Date(2026, 7, 12, 23, 59, 59, 0, time.UTC)
	pool := newKeyPool([]string{"key1"}, KeyPoolOptions{BudgetZone: time.UTC})
	pool.now = func() time.Time { return now }
	lease, err := pool.lease(context.Background(), RequestSourceMain, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.reserve(context.Background(), lease, RequestSourceMain, false); err != nil {
		t.Fatal(err)
	}
	if got := pool.status(now).TodayMainRequests; got != 1 {
		t.Fatalf("requests before reset = %d", got)
	}
	now = now.Add(2 * time.Second)
	status := pool.status(now)
	if status.TodayMainRequests != 0 || status.Keys[0].TodayRequests != 0 {
		t.Fatalf("usage did not reset at UTC 00:00: %+v", status)
	}
	if status.NextBudgetResetAt.Hour() != 0 || status.NextBudgetResetAt.Location() != time.UTC {
		t.Fatalf("next reset = %v, want UTC midnight", status.NextBudgetResetAt)
	}
}

func TestAsyncUsageFlushKeepsBothSidesOfUTCReset(t *testing.T) {
	store := newMemoryUsageStore()
	now := time.Date(2026, 7, 12, 23, 59, 59, 0, time.UTC)
	pool := newKeyPool([]string{"key1"}, KeyPoolOptions{BudgetZone: time.UTC, UsageStore: store})
	pool.now = func() time.Time { return now }
	for request := 0; request < 2; request++ {
		lease, err := pool.lease(context.Background(), RequestSourceMain, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.reserve(context.Background(), lease, RequestSourceMain, false); err != nil {
			t.Fatal(err)
		}
		now = now.Add(2 * time.Second)
	}
	if err := pool.flushUsage(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.records) != 2 {
		t.Fatalf("persisted usage days = %d, want 2: %+v", len(store.records), store.records)
	}
}

func TestKeyPoolConcurrentReservationsAreSafe(t *testing.T) {
	pool := newKeyPool([]string{"key1", "key2", "key3"}, KeyPoolOptions{})
	const requests = 90
	var wg sync.WaitGroup
	errs := make(chan error, requests)
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lease, err := pool.lease(context.Background(), RequestSourceOther, nil, false)
			if err == nil {
				_, err = pool.reserve(context.Background(), lease, RequestSourceOther, false)
			}
			if err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	status := pool.status(time.Now())
	if status.TodayOtherRequests != requests {
		t.Fatalf("other requests = %d, want %d", status.TodayOtherRequests, requests)
	}
	for _, key := range status.Keys {
		if key.TodayRequests < 29 || key.TodayRequests > 31 {
			t.Fatalf("unfair distribution: %+v", status.Keys)
		}
	}
}

func TestMainCapacityReservesOneSustainedRPSPerKey(t *testing.T) {
	now := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	pool := newKeyPool([]string{"key1"}, KeyPoolOptions{MinInterval: 200 * time.Millisecond, BudgetZone: time.UTC})
	pool.now = func() time.Time { return now }
	status := pool.status(now)
	if math.Abs(status.BaseBudgetRPS-1) > 0.0001 || math.Abs(status.CompensationBudgetRPS-(100000.0/86400.0-1)) > 0.0001 {
		t.Fatalf("single-key sustainable budget = base %.6f surplus %.6f", status.BaseBudgetRPS, status.CompensationBudgetRPS)
	}
}

func TestTenKeySustainableBudgetMatchesDailyPlanningMath(t *testing.T) {
	now := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	keys := make([]string, 10)
	for i := range keys {
		keys[i] = fmt.Sprintf("key-%d", i)
	}
	pool := newKeyPool(keys, KeyPoolOptions{BudgetZone: time.UTC, DailyQuotaPerKey: 100000})
	pool.now = func() time.Time { return now }
	status := pool.status(now)
	wantSurplus := 10 * (100000.0/86400.0 - 1)
	if math.Abs(status.BaseBudgetRPS-10) > 0.0001 || math.Abs(status.CompensationBudgetRPS-wantSurplus) > 0.0001 {
		t.Fatalf("ten-key budget = base %.6f surplus %.6f, want 10/%.6f", status.BaseBudgetRPS, status.CompensationBudgetRPS, wantSurplus)
	}
	if status.TodayRemainingEstimate != 1_000_000 {
		t.Fatalf("daily remaining = %d, want 1000000", status.TodayRemainingEstimate)
	}
}

func TestFractionalBaseTokensAccumulateAndSurplusBurstIsCapped(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	pool := newKeyPool([]string{"key1"}, KeyPoolOptions{BudgetZone: time.UTC, DailyQuotaPerKey: 1000, SurplusBurstWindow: 60 * time.Second})
	pool.now = func() time.Time { return now }
	status := pool.status(now)
	if status.BaseBudgetRPS <= 0 || status.BaseBudgetRPS >= 1 {
		t.Fatalf("fractional base rps = %v", status.BaseBudgetRPS)
	}
	if _, err := pool.scheduledMainLeases(context.Background(), 8); err != nil {
		t.Fatalf("emergency head must remain available: %v", err)
	}
	burstPool := newKeyPool([]string{"key2"}, KeyPoolOptions{BudgetZone: time.UTC, DailyQuotaPerKey: 100000, SurplusBurstWindow: 60 * time.Second})
	burstPool.now = func() time.Time { return now }
	_ = burstPool.status(now)
	now = now.Add(10 * time.Minute)
	status = burstPool.status(now)
	if status.CompensationTokens > status.CompensationBudgetRPS*60+0.0001 {
		t.Fatalf("surplus tokens %.6f exceed 60s cap %.6f", status.CompensationTokens, status.CompensationBudgetRPS*60)
	}
}

func TestScheduledMainPersistenceLanesKeepHeadReservedAndBoundNormalConcurrency(t *testing.T) {
	const keyCount = 30
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("start"))
		page /= 50
		if page == 0 {
			time.Sleep(120 * time.Millisecond)
		}
		if page == 7 || page == 19 {
			http.Error(w, "upstream failure", http.StatusInternalServerError)
			return
		}
		writeFullTransferPage(w)
	}))
	defer server.Close()
	keys := make([]string, keyCount)
	for i := range keys {
		keys[i] = fmt.Sprintf("key-%02d", i)
	}
	client := NewClientWithKeys(server.URL, keys, 2*time.Second, KeyPoolOptions{BudgetZone: time.UTC})
	normalRelease := make(chan struct{})
	normalStarted := make(chan int, keyCount)
	headDone := make(chan struct{}, 1)
	var activeNormal atomic.Int32
	var maxNormal atomic.Int32
	var seenMu sync.Mutex
	seen := make(map[int]error, keyCount)
	done := make(chan error, 1)
	go func() {
		result, err := client.FetchScheduledMainPagesAt(context.Background(), "TR7", 1, 1000, keyCount, 4, func(page TransferPageResult) error {
			seenMu.Lock()
			seen[page.Page] = page.Err
			seenMu.Unlock()
			if page.Page == 0 {
				headDone <- struct{}{}
				return nil
			}
			current := activeNormal.Add(1)
			for {
				maximum := maxNormal.Load()
				if current <= maximum || maxNormal.CompareAndSwap(maximum, current) {
					break
				}
			}
			normalStarted <- page.Page
			<-normalRelease
			activeNormal.Add(-1)
			if page.Page == 11 {
				return errors.New("database saturated")
			}
			return nil
		})
		if len(result.FailedPages) != 3 {
			done <- fmt.Errorf("failed pages = %v, want 3", result.FailedPages)
			return
		}
		foundPersistFailure := false
		for _, failure := range result.FailedPages {
			foundPersistFailure = foundPersistFailure || (failure.Page == 11 && strings.Contains(failure.Error, "persist:"))
		}
		if !foundPersistFailure {
			done <- fmt.Errorf("missing exact persistence failure: %v", result.FailedPages)
			return
		}
		done <- err
	}()
	for i := 0; i < 4; i++ {
		select {
		case <-normalStarted:
		case <-time.After(time.Second):
			t.Fatal("normal persistence workers did not saturate")
		}
	}
	select {
	case <-headDone:
	case <-time.After(300 * time.Millisecond):
		t.Fatal("P1 persistence lane was blocked by saturated normal workers")
	}
	if got := maxNormal.Load(); got != 4 {
		t.Fatalf("normal persistence concurrency = %d, want 4", got)
	}
	close(normalRelease)
	if err := <-done; err == nil {
		t.Fatal("partial upstream failures must be returned")
	}
	seenMu.Lock()
	defer seenMu.Unlock()
	if len(seen) != keyCount {
		t.Fatalf("persist callbacks = %d, want %d", len(seen), keyCount)
	}
	if seen[7] == nil || seen[19] == nil {
		t.Fatalf("failed pages were not independently reported: p7=%v p19=%v", seen[7], seen[19])
	}
}

func TestScheduledMainFailedHeadDoesNotBlockReturnedPages(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("start") == "0" {
			time.Sleep(150 * time.Millisecond)
			http.Error(w, "head failed", http.StatusInternalServerError)
			return
		}
		writeShortTransferPage(w)
	}))
	defer server.Close()
	client := NewClientWithKeys(server.URL, []string{"key1", "key2", "key3"}, time.Second, KeyPoolOptions{BudgetZone: time.UTC})
	normalDone := make(chan time.Duration, 1)
	started := time.Now()
	_, err := client.FetchScheduledMainPagesAt(context.Background(), "TR7", 1, 1000, 3, 2, func(page TransferPageResult) error {
		if page.Page == 1 {
			normalDone <- time.Since(started)
		}
		return nil
	})
	if err == nil {
		t.Fatal("expected failed P1")
	}
	select {
	case delay := <-normalDone:
		if delay >= 120*time.Millisecond {
			t.Fatalf("P2 waited for failed P1: %v", delay)
		}
	default:
		t.Fatal("P2 callback was lost")
	}
}

func TestScheduledMainTimedOutHeadDoesNotBlockReturnedPages(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("start") == "0" {
			<-r.Context().Done()
			return
		}
		writeShortTransferPage(w)
	}))
	defer server.Close()
	client := NewClientWithKeys(server.URL, []string{"key1", "key2", "key3"}, time.Second, KeyPoolOptions{BudgetZone: time.UTC})
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	normalDone := make(chan time.Duration, 1)
	started := time.Now()
	done := make(chan error, 1)
	go func() {
		_, err := client.FetchScheduledMainPagesAt(ctx, "TR7", 1, 1000, 3, 2, func(page TransferPageResult) error {
			if page.Page == 2 {
				normalDone <- time.Since(started)
			}
			return nil
		})
		done <- err
	}()
	select {
	case delay := <-normalDone:
		if delay >= 80*time.Millisecond {
			t.Fatalf("PN waited for timed-out P1: %v", delay)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("PN callback was blocked by timed-out P1")
	}
	if err := <-done; err == nil {
		t.Fatal("expected P1 timeout")
	}
}

func TestSharedRoundDeadlineDoesNotCoolElevenHealthyKeys(t *testing.T) {
	const keyCount = 11
	var phase atomic.Int32
	var blockedRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if phase.Load() == 0 {
			blockedRequests.Add(1)
			<-r.Context().Done()
			return
		}
		writeShortTransferPage(w)
	}))
	defer server.Close()

	keys := make([]string, keyCount)
	for index := range keys {
		keys[index] = fmt.Sprintf("key-%02d", index)
	}
	client := NewClientWithKeys(server.URL, keys, 5*time.Second, KeyPoolOptions{
		RealtimeInterval: time.Second,
		BudgetZone:       time.UTC,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	if _, err := client.FetchScheduledMainPagesAt(ctx, "TR7", 1, 1000, keyCount, 4, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shared round error = %v, want deadline exceeded", err)
	}
	if got := blockedRequests.Load(); got != keyCount {
		t.Fatalf("in-flight requests = %d, want %d", got, keyCount)
	}
	status := client.KeyPoolStatus(time.Now())
	if status.HealthyCount != keyCount || status.AvailableCount != keyCount || status.CooldownCount != 0 {
		t.Fatalf("caller deadline changed key health: %+v", status)
	}
	for _, key := range status.Keys {
		if key.LastFailureAt != nil || key.ConsecutiveFailures != 0 {
			t.Fatalf("caller deadline recorded a key failure: %+v", key)
		}
	}

	// Base tokens were consumed by the canceled round, but the emergency head
	// reservation must keep the next P1 alive while a healthy key is available.
	phase.Store(1)
	result, err := client.FetchScheduledMainPagesAt(context.Background(), "TR7", 1, 2000, keyCount, 4, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Metrics.Pages < 1 {
		t.Fatalf("next round pages = %d, want at least the reserved head", result.Metrics.Pages)
	}
}

func TestEqualParentAndRequestTimeoutKeepsPartialElevenKeyRoundHealthy(t *testing.T) {
	const keyCount = 11
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		start, _ := strconv.Atoi(r.URL.Query().Get("start"))
		if start < 4*50 {
			writeShortTransferPage(w)
			return
		}
		<-r.Context().Done()
	}))
	defer server.Close()
	keys := make([]string, keyCount)
	for index := range keys {
		keys[index] = fmt.Sprintf("key-%02d", index)
	}
	const timeout = 120 * time.Millisecond
	client := NewClientWithKeys(server.URL, keys, timeout, KeyPoolOptions{RealtimeInterval: time.Second, BudgetZone: time.UTC})
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	result, err := client.FetchScheduledMainPagesAt(ctx, "TR7", 1, 1000, keyCount, 4, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("round error = %v, want parent deadline", err)
	}
	if got := calls.Load(); got != keyCount {
		t.Fatalf("request count = %d, want %d", got, keyCount)
	}
	if len(result.SuccessfulPages) != 4 || len(result.FailedPages) != keyCount-4 {
		t.Fatalf("partial result = success %v failures %v", result.SuccessfulPages, result.FailedPages)
	}
	status := client.KeyPoolStatus(time.Now())
	if status.HealthyCount != keyCount || status.AvailableCount != keyCount || status.CooldownCount != 0 {
		t.Fatalf("equal deadlines cooled keys: %+v", status)
	}
}

func TestWrappedLateParentDeadlineDoesNotCoolKeys(t *testing.T) {
	const keyCount = 11
	keys := make([]string, keyCount)
	for index := range keys {
		keys[index] = fmt.Sprintf("key-%02d", index)
	}
	client := NewClientWithKeys("https://tronscan.invalid", keys, 100*time.Millisecond, KeyPoolOptions{RealtimeInterval: time.Second})
	client.http.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		// Model a transport that returns after the old round has already ended
		// and wraps the context error in url.Error.
		time.Sleep(20 * time.Millisecond)
		return nil, &url.Error{Op: http.MethodGet, URL: req.URL.String(), Err: req.Context().Err()}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := client.FetchScheduledMainPagesAt(ctx, "TR7", 1, 1000, keyCount, 4, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wrapped late error = %v, want parent deadline", err)
	}
	status := client.KeyPoolStatus(time.Now())
	if status.HealthyCount != keyCount || status.AvailableCount != keyCount || status.CooldownCount != 0 {
		t.Fatalf("wrapped late deadline cooled keys: %+v", status)
	}
}

func TestHTTPClientTimeoutStillCoolsFailingKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
	}))
	defer server.Close()
	client := NewClientWithKeys(server.URL, []string{"key1"}, 20*time.Millisecond, KeyPoolOptions{BudgetZone: time.UTC})

	_, err := client.FetchScheduledMainPagesAt(context.Background(), "TR7", 1, 1000, 1, 1, nil)
	if err == nil {
		t.Fatal("expected HTTP client timeout")
	}
	status := client.KeyPoolStatus(time.Now())
	if status.CooldownCount != 1 || status.AvailableCount != 0 {
		t.Fatalf("real transport timeout did not cool the key: %+v", status)
	}
	if status.Keys[0].LastFailureAt == nil || status.Keys[0].LastErrorClass == "" {
		t.Fatalf("real transport timeout was not classified: %+v", status.Keys[0])
	}
}

func TestSaturatedOldRoundNormalPersistenceDoesNotBlockNextHead(t *testing.T) {
	const keyCount = 30
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeShortTransferPage(w)
	}))
	defer server.Close()
	keys := make([]string, keyCount)
	for index := range keys {
		keys[index] = fmt.Sprintf("key-%02d", index)
	}
	client := NewClientWithKeys(server.URL, keys, 2*time.Second, KeyPoolOptions{RealtimeInterval: time.Second})
	releaseOld := make(chan struct{})
	firstHead := make(chan struct{}, 1)
	firstDone := make(chan error, 1)
	go func() {
		_, err := client.FetchScheduledMainPagesAt(context.Background(), "TR7", 1, 1000, keyCount, 4, func(page TransferPageResult) error {
			if page.Page == 0 {
				firstHead <- struct{}{}
				return nil
			}
			<-releaseOld
			return nil
		})
		firstDone <- err
	}()
	select {
	case <-firstHead:
	case <-time.After(time.Second):
		t.Fatal("first head did not persist")
	}
	// The first round consumed the base tokens. After the per-key 5 RPS
	// interval, the emergency hot-head token must still start a new round.
	time.Sleep(220 * time.Millisecond)
	secondHead := make(chan struct{}, 1)
	started := time.Now()
	_, secondErr := client.FetchScheduledMainPagesAt(context.Background(), "TR7", 1, 2000, keyCount, 4, func(page TransferPageResult) error {
		if page.Page == 0 {
			secondHead <- struct{}{}
		}
		return nil
	})
	if secondErr != nil {
		t.Fatal(secondErr)
	}
	select {
	case <-secondHead:
		if delay := time.Since(started); delay > 500*time.Millisecond {
			t.Fatalf("next head delayed by saturated old normal lane: %v", delay)
		}
	default:
		t.Fatal("next head callback was not executed")
	}
	close(releaseOld)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
}

func TestSustainedMoreThan150TransfersPerSecondKeepsEachHeadOnSchedule(t *testing.T) {
	const (
		keyCount = 6
		rounds   = 3
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("start") != "0" {
			time.Sleep(1200 * time.Millisecond)
		}
		writeFullTransferPage(w)
	}))
	defer server.Close()
	keys := make([]string, keyCount)
	for index := range keys {
		keys[index] = fmt.Sprintf("sustained-key-%d", index)
	}
	client := NewClientWithKeys(server.URL, keys, 2*time.Second, KeyPoolOptions{RealtimeInterval: time.Second})
	started := time.Now()
	roundInterval := 1050 * time.Millisecond
	var persisted atomic.Int64
	headDelays := make(chan time.Duration, rounds)
	errs := make(chan error, rounds)
	for round := 0; round < rounds; round++ {
		due := started.Add(time.Duration(round) * roundInterval)
		if wait := time.Until(due); wait > 0 {
			time.Sleep(wait)
		}
		roundStarted := time.Now()
		go func(cutoff int64) {
			result, err := client.FetchScheduledMainPagesAt(context.Background(), "TR7", 1, cutoff, keyCount, 4, func(page TransferPageResult) error {
				persisted.Add(int64(len(page.Transfers)))
				if page.Page == 0 {
					headDelays <- time.Since(roundStarted)
				}
				return nil
			})
			if err == nil && result.Metrics.Pages != keyCount {
				err = fmt.Errorf("pages = %d, want %d", result.Metrics.Pages, keyCount)
			}
			errs <- err
		}(int64(round + 1))
	}
	for round := 0; round < rounds; round++ {
		select {
		case delay := <-headDelays:
			if delay > 350*time.Millisecond {
				t.Fatalf("round %d head delay = %v, want <=350ms", round+1, delay)
			}
		case <-time.After(time.Second):
			t.Fatalf("round %d head callback missed its one-second window", round+1)
		}
	}
	for round := 0; round < rounds; round++ {
		if err := <-errs; err != nil {
			t.Fatalf("round %d: %v", round+1, err)
		}
	}
	if got, want := persisted.Load(), int64(rounds*keyCount*50); got != want {
		t.Fatalf("persisted transfers = %d, want %d (~286 transfers/second for %d rounds)", got, want, rounds)
	}
}

func TestLocalUsageCountDoesNotExhaustKey(t *testing.T) {
	now := time.Date(2026, 7, 12, 20, 0, 0, 0, time.UTC)
	pool := newKeyPool([]string{"key1"}, KeyPoolOptions{BudgetZone: time.UTC})
	pool.now = func() time.Time { return now }
	lease, err := pool.lease(context.Background(), RequestSourceMain, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.reserve(context.Background(), lease, RequestSourceMain, false); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.lease(context.Background(), RequestSourceMain, nil, false); err != nil {
		t.Fatalf("local count exhausted key: %v", err)
	}
}

func TestCompensationCursorDoesNotChangeMainRoundOrder(t *testing.T) {
	pool := newKeyPool([]string{"key1", "key2", "key3"}, KeyPoolOptions{})
	now := time.Now()
	pool.now = func() time.Time { return now }
	main1, err := pool.lease(context.Background(), RequestSourceMain, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := pool.lease(context.Background(), RequestSourceCompensation, nil, false); err != nil {
			t.Fatal(err)
		}
		now = now.Add(10 * time.Second)
	}
	main2, err := pool.lease(context.Background(), RequestSourceMain, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if main1.key != "key1" || main2.key != "key2" {
		t.Fatalf("main order = %s,%s, want key1,key2", main1.key, main2.key)
	}
}

func TestAuthenticationFailureProbeRecoveryRequiresTwoSuccesses(t *testing.T) {
	now := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	pool := newKeyPool([]string{"key1"}, KeyPoolOptions{})
	pool.now = func() time.Time { return now }
	lease, err := pool.lease(context.Background(), RequestSourceMain, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.report(context.Background(), lease, 401, `{"error":"invalid api key"}`, 0, now); err != nil {
		t.Fatal(err)
	}
	if got := pool.status(now).Keys[0].Health; got != "suspect" {
		t.Fatalf("first auth failure health = %s, want suspect", got)
	}
	now = now.Add(5 * time.Second)
	probes := pool.dueProbeLeases(now)
	if len(probes) != 1 {
		t.Fatalf("suspect probes = %d, want 1", len(probes))
	}
	if err := pool.report(context.Background(), probes[0], 401, `{"error":"invalid api key"}`, 0, now); err != nil {
		t.Fatal(err)
	}
	if got := pool.status(now).Keys[0].Health; got != "invalid" {
		t.Fatalf("second auth failure health = %s, want invalid", got)
	}
	now = now.Add(30 * time.Minute)
	probes = pool.dueProbeLeases(now)
	if len(probes) != 1 {
		t.Fatalf("invalid probes = %d, want 1", len(probes))
	}
	if err := pool.report(context.Background(), probes[0], 200, `{}`, 0, now); err != nil {
		t.Fatal(err)
	}
	if got := pool.status(now).Keys[0].Health; got != "invalid" {
		t.Fatalf("one successful probe health = %s, want invalid", got)
	}
	now = now.Add(5 * time.Second)
	probes = pool.dueProbeLeases(now)
	if len(probes) != 1 {
		t.Fatalf("second recovery probes = %d, want 1", len(probes))
	}
	if err := pool.report(context.Background(), probes[0], 200, `{}`, 0, now); err != nil {
		t.Fatal(err)
	}
	if got := pool.status(now).Keys[0].Health; got != "healthy" {
		t.Fatalf("two successful probes health = %s, want healthy", got)
	}
}

func TestConcurrentInFlightAuthFailuresDoNotReplaceIndependentProbe(t *testing.T) {
	now := time.Now()
	pool := newKeyPool([]string{"key1"}, KeyPoolOptions{})
	lease, _ := pool.lease(context.Background(), RequestSourceMain, nil, false)
	for i := 0; i < 2; i++ {
		if err := pool.report(context.Background(), lease, 401, `invalid api key`, 0, now); err != nil {
			t.Fatal(err)
		}
	}
	status := pool.status(now).Keys[0]
	if status.Health != "suspect" || status.NextProbeAt == nil {
		t.Fatalf("in-flight auth status = %+v, want suspect awaiting probe", status)
	}
}

func TestManualRecheckRequiresTwoSuccessfulProbes(t *testing.T) {
	now := time.Now()
	pool := newKeyPool([]string{"key1"}, KeyPoolOptions{})
	pool.now = func() time.Time { return now }
	fingerprint := keyFingerprint("key1")
	if err := pool.requestProbe(context.Background(), fingerprint, true); err != nil {
		t.Fatal(err)
	}
	probes := pool.dueProbeLeases(now)
	if len(probes) != 1 {
		t.Fatalf("first probes = %d", len(probes))
	}
	if err := pool.report(context.Background(), probes[0], 200, `{}`, 0, now); err != nil {
		t.Fatal(err)
	}
	if got := pool.status(now).Keys[0].Health; got != "suspect" {
		t.Fatalf("one probe health = %s", got)
	}
	now = now.Add(5 * time.Second)
	probes = pool.dueProbeLeases(now)
	if len(probes) != 1 {
		t.Fatalf("second probes = %d", len(probes))
	}
	if err := pool.report(context.Background(), probes[0], 200, `{}`, 0, now); err != nil {
		t.Fatal(err)
	}
	if got := pool.status(now).Keys[0].Health; got != "healthy" {
		t.Fatalf("two probe health = %s", got)
	}
}

func TestForbiddenQuotaIsCooldownNotInvalid(t *testing.T) {
	now := time.Now()
	pool := newKeyPool([]string{"key1"}, KeyPoolOptions{})
	lease, _ := pool.lease(context.Background(), RequestSourceMain, nil, false)
	if err := pool.report(context.Background(), lease, 403, `daily quota exceeded`, 0, now); err != nil {
		t.Fatal(err)
	}
	status := pool.status(now).Keys[0]
	if status.Health != "exhausted" || status.LastErrorClass != "rate_limit" {
		t.Fatalf("quota 403 status = %+v", status)
	}
}

func TestRateLimitWithoutRetryAfterUsesExponentialBackoff(t *testing.T) {
	now := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	pool := newKeyPool([]string{"key1"}, KeyPoolOptions{})
	pool.now = func() time.Time { return now }
	lease, _ := pool.lease(context.Background(), RequestSourceMain, nil, false)
	wants := []time.Duration{60 * time.Second, 120 * time.Second, 300 * time.Second, 900 * time.Second, time.Hour}
	for i, want := range wants {
		if err := pool.report(context.Background(), lease, 429, `frequency limit`, 0, now); err != nil {
			t.Fatal(err)
		}
		status := pool.status(now).Keys[0]
		delay := status.CooldownUntil.Sub(now)
		if delay != want {
			t.Fatalf("429 #%d backoff = %v, want %v", i+1, delay, want)
		}
		now = *status.CooldownUntil
		probes := pool.dueProbeLeases(now)
		if len(probes) != 1 {
			t.Fatalf("429 #%d half-open probes = %d, want 1", i+1, len(probes))
		}
		lease = probes[0]
	}
}

func TestPublicFallback429UsesOneTwoThreeFiveTenSeconds(t *testing.T) {
	now := time.Now()
	pool := newKeyPool(nil, KeyPoolOptions{AllowAnonymous: true, PublicFallback: true})
	pool.now = func() time.Time { return now }
	lease, _ := pool.lease(context.Background(), RequestSourceMain, nil, false)
	for i, want := range []time.Duration{time.Second, 2 * time.Second, 3 * time.Second, 5 * time.Second, 10 * time.Second} {
		if err := pool.report(context.Background(), lease, 429, `frequency limit`, 0, now); err != nil {
			t.Fatal(err)
		}
		status := pool.status(now).Keys[0]
		if got := status.CooldownUntil.Sub(now); got != want {
			t.Fatalf("step %d = %v, want %v", i, got, want)
		}
		now = *status.CooldownUntil
		probes := pool.dueProbeLeases(now)
		if len(probes) != 1 {
			t.Fatalf("step %d probes = %d", i, len(probes))
		}
		lease = probes[0]
	}
}

func TestServerFailureFailsOverAndProbeRestoresKey(t *testing.T) {
	var mu sync.Mutex
	failKey1 := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		fail := failKey1 && r.Header.Get("TRON-PRO-API-KEY") == "key1"
		mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writeShortTransferPage(w)
	}))
	defer server.Close()
	client := NewClientWithKeys(server.URL, []string{"key1", "key2"}, time.Second, KeyPoolOptions{})
	if _, err := client.FetchGlobalUSDTTransfersWithMetrics(context.Background(), "TR7", 1, 1); err != nil {
		t.Fatal(err)
	}
	status := client.KeyPoolStatus(time.Now())
	if status.Keys[0].Health != "cooldown" || status.Keys[1].FailoverRequests != 1 {
		t.Fatalf("post-failover status = %+v", status)
	}
	mu.Lock()
	failKey1 = false
	mu.Unlock()
	client.keys.mu.Lock()
	client.keys.keys[0].nextProbeAt = time.Now()
	client.keys.keys[0].cooldownUntil = time.Now()
	client.keys.mu.Unlock()
	if got := client.ProbeDueKeys(context.Background(), "TR7"); got != 1 {
		t.Fatalf("probe count = %d, want 1", got)
	}
	if got := client.KeyPoolStatus(time.Now()).Keys[0].Health; got != "healthy" {
		t.Fatalf("recovered health = %s, want healthy", got)
	}
}

func TestTenKeyPoolFairlyDistributesExplicitMultiPageRequests(t *testing.T) {
	keys := make([]string, 10)
	for i := range keys {
		keys[i] = fmt.Sprintf("key-%d", i)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeShortTransferPage(w) }))
	defer server.Close()
	client := NewClientWithKeys(server.URL, keys, time.Second, KeyPoolOptions{})
	const rounds = 100
	for i := 0; i < rounds; i++ {
		// This helper call explicitly asks for three pages; realtime main-scan
		// depth is independently derived from accumulated sustainable tokens.
		result, err := client.FetchGlobalUSDTTransfersWithMetrics(context.Background(), "TR7", 1, 3)
		if err != nil {
			t.Fatal(err)
		}
		if result.Metrics.Calls != 3 {
			t.Fatalf("round %d calls = %d, want 3", i, result.Metrics.Calls)
		}
	}
	status := client.KeyPoolStatus(time.Now())
	for _, key := range status.Keys {
		if key.TodayRequests != 30 {
			t.Fatalf("unfair 10-key distribution: %+v", status.Keys)
		}
	}
}

func TestDynamicKeyCountsProduceTokenBasedMainPages(t *testing.T) {
	for _, count := range []int{0, 1, 2, 3, 6, 10, 20, 30} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			keys := make([]string, count)
			for i := range keys {
				keys[i] = fmt.Sprintf("key-%d", i)
			}
			pool := newKeyPool(keys, KeyPoolOptions{})
			now := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
			pool.now = func() time.Time { return now }
			leases, err := pool.scheduledMainLeases(context.Background(), 64)
			if count == 0 {
				if err == nil {
					t.Fatal("zero-key pool returned leases")
				}
				return
			}
			if err != nil || len(leases) != count {
				t.Fatalf("leases = %d/%v", len(leases), err)
			}
			unique := make(map[string]struct{})
			for _, lease := range leases {
				unique[lease.fingerprint] = struct{}{}
			}
			if len(unique) != count {
				t.Fatalf("unique keys = %d, want %d", len(unique), count)
			}
		})
	}
}

func TestSingleKeyRequestStartsStayWithinFiveRPS(t *testing.T) {
	var mu sync.Mutex
	var starts []time.Time
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		starts = append(starts, time.Now())
		mu.Unlock()
		writeShortTransferPage(w)
	}))
	defer server.Close()
	client := NewClientWithKeys(server.URL, []string{"key1"}, 3*time.Second, KeyPoolOptions{})
	for i := 0; i < 2; i++ {
		if _, err := client.FetchGlobalUSDTTransfersWithMetrics(context.Background(), "TR7", 1, 3); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for i := 1; i < len(starts); i++ {
		if gap := starts[i].Sub(starts[i-1]); gap < 180*time.Millisecond {
			t.Fatalf("request gap %d = %v, violates 5 RPS", i, gap)
		}
	}
}

func TestSlowExpandDoesNotBlockNextHotHead(t *testing.T) {
	expandStarted := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("start") == "150" {
			select {
			case expandStarted <- struct{}{}:
			default:
			}
			time.Sleep(800 * time.Millisecond)
		}
		writeShortTransferPage(w)
	}))
	defer server.Close()
	client := NewClientWithKeys(server.URL, []string{"k1", "k2", "k3", "k4"}, 2*time.Second, KeyPoolOptions{RealtimeInterval: time.Second})
	go client.FetchGlobalUSDTTransfersRangeWithMetrics(context.Background(), "TR7", 1, 1000, 3, 1)
	select {
	case <-expandStarted:
	case <-time.After(time.Second):
		t.Fatal("expand did not start")
	}
	started := time.Now()
	result, err := client.FetchGlobalUSDTTransfersAtWithMetrics(context.Background(), "TR7", 1, 1001, 3)
	if err != nil {
		t.Fatal(err)
	}
	if result.Metrics.Calls != 3 || time.Since(started) > 700*time.Millisecond {
		t.Fatalf("hot head calls/duration = %d/%v", result.Metrics.Calls, time.Since(started))
	}
}

func TestRealtimePriorityDefersCompensationWithoutLosingCursor(t *testing.T) {
	now := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	pool := newKeyPool([]string{"key1", "key2", "key3"}, KeyPoolOptions{MinInterval: 100 * time.Millisecond, RealtimeInterval: time.Second})
	pool.now = func() time.Time { return now }
	leases, err := pool.mainLeases(context.Background(), 3)
	if err != nil || len(leases) != 3 {
		t.Fatalf("main leases = %d/%v", len(leases), err)
	}
	if _, err := pool.lease(context.Background(), RequestSourceCompensation, nil, false); !IsCompensationDeferred(err) {
		t.Fatalf("compensation error = %v, want deferred", err)
	}
	for _, lease := range leases {
		if _, err := pool.reserve(context.Background(), lease, RequestSourceMain, false); err != nil {
			t.Fatalf("main reserve delayed by compensation: %v", err)
		}
	}
	now = now.Add(10 * time.Second)
	if _, err := pool.lease(context.Background(), RequestSourceCompensation, nil, false); err != nil {
		t.Fatalf("compensation did not resume between main rounds: %v", err)
	}
}

func TestConfiguredLegacyBudgetDoesNotStopRealtimeOrCompensation(t *testing.T) {
	pool := newKeyPool([]string{"key1"}, KeyPoolOptions{})
	main, err := pool.lease(context.Background(), RequestSourceMain, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.reserve(context.Background(), main, RequestSourceMain, false); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.lease(context.Background(), RequestSourceCompensation, nil, false); err != nil {
		t.Fatalf("legacy budget stopped compensation: %v", err)
	}
	main, err = pool.lease(context.Background(), RequestSourceMain, nil, false)
	if err != nil {
		t.Fatalf("realtime could not use hard reserve: %v", err)
	}
	if _, err := pool.reserve(context.Background(), main, RequestSourceMain, false); err != nil {
		t.Fatalf("realtime hard reserve failed: %v", err)
	}
}

func TestRegistryHotAddDeleteDisableAndInflightLease(t *testing.T) {
	store := newMemoryRegistryStore()
	store.records[keyFingerprint("key1")] = KeyRegistryRecord{Fingerprint: keyFingerprint("key1"), APIKey: "key1", Enabled: true, Health: "healthy"}
	store.records[keyFingerprint("key2")] = KeyRegistryRecord{Fingerprint: keyFingerprint("key2"), APIKey: "key2", Enabled: true, Health: "healthy"}
	client := NewClientWithKeys("http://example.invalid", nil, time.Second, KeyPoolOptions{UsageStore: store})
	if err := client.RefreshKeyRegistry(context.Background()); err != nil {
		t.Fatal(err)
	}
	inflight, err := client.keys.lease(context.Background(), RequestSourceOther, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	delete(store.records, inflight.fingerprint)
	store.records[keyFingerprint("key3")] = KeyRegistryRecord{Fingerprint: keyFingerprint("key3"), APIKey: "key3", Enabled: true, Health: "healthy"}
	store.records[keyFingerprint("key2")] = KeyRegistryRecord{Fingerprint: keyFingerprint("key2"), APIKey: "key2", Enabled: false, Health: "healthy"}
	if err := client.RefreshKeyRegistry(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.keys.reserve(context.Background(), inflight, RequestSourceOther, false); err != nil {
		t.Fatalf("in-flight lease failed after deletion: %v", err)
	}
	status := client.KeyPoolStatus(time.Now())
	foundKey3 := false
	for _, item := range status.Keys {
		foundKey3 = foundKey3 || item.Fingerprint == keyFingerprint("key3")
	}
	if status.KeyCount != 2 || status.EnabledCount != 1 || !foundKey3 {
		t.Fatalf("hot registry status = %+v", status)
	}
	lease, err := client.keys.lease(context.Background(), RequestSourceMain, nil, false)
	if err != nil || lease.key != "key3" {
		t.Fatalf("post-update lease = %q/%v, want key3", lease.key, err)
	}
}

func TestRegistryAcceptsMoreThanTenKeys(t *testing.T) {
	store := newMemoryRegistryStore()
	for i := 0; i < 30; i++ {
		key := fmt.Sprintf("key-%d", i)
		if err := store.UpsertTronscanAPIKey(context.Background(), keyFingerprint(key), key, i%2 == 0, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(store.records); got != 30 {
		t.Fatalf("registry size = %d, want 30", got)
	}
}

func TestCompensationBudgetGrowsWithHealthyKeyCapacity(t *testing.T) {
	statusFor := func(keyCount int, hardCap float64) KeyPoolStatus {
		keys := make([]string, keyCount)
		for index := range keys {
			keys[index] = fmt.Sprintf("key-%d", index)
		}
		client := NewClientWithKeys("http://example.invalid", keys, time.Second, KeyPoolOptions{CompensationMaxRPS: hardCap, RealtimeInterval: time.Second})
		return client.KeyPoolStatus(time.Now())
	}

	six := statusFor(6, 0)
	eight := statusFor(8, 0)
	if six.CompensationBudgetRPS <= 0 || eight.CompensationBudgetRPS <= six.CompensationBudgetRPS {
		t.Fatalf("dynamic compensation budgets = %.3f/%.3f, want positive growth with healthy keys", six.CompensationBudgetRPS, eight.CompensationBudgetRPS)
	}
	capped := statusFor(8, 0.5)
	if capped.CompensationBudgetRPS != 0.5 {
		t.Fatalf("hard-capped compensation budget = %.3f, want 0.5", capped.CompensationBudgetRPS)
	}
}

func TestDailyQuotaIsCapacityEstimateNotLocalHardStop(t *testing.T) {
	store := newMemoryUsageStore()
	pool := newKeyPool([]string{"key1"}, KeyPoolOptions{UsageStore: store, DailyQuotaPerKey: 2})
	for request := 0; request < 2; request++ {
		leases, err := pool.mainLeases(context.Background(), 1)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.reserve(context.Background(), leases[0], RequestSourceMain, false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.mainLeases(context.Background(), 1); err != nil {
		t.Fatalf("quota estimate disabled a key that the upstream still accepts: %v", err)
	}
	status := pool.status(time.Now())
	if status.AvailableCount != 1 || status.TodayRemainingEstimate != 0 || status.ExhaustedCount != 0 {
		t.Fatalf("quota-estimate status = %+v", status)
	}
}

func writeFullTransferPage(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"token_transfers":[`)
	for i := 0; i < 50; i++ {
		if i > 0 {
			fmt.Fprint(w, ",")
		}
		fmt.Fprintf(w, `{"transaction_id":"h%d","from_address":"A","to_address":"B","quant":"1","block_ts":2000000000000,"confirmed":1}`, i)
	}
	fmt.Fprint(w, `]}`)
}

func writeShortTransferPage(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"token_transfers":[]}`)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

type memoryUsageStore struct {
	mu      sync.Mutex
	records map[string]KeyUsageRecord
}

type memoryRegistryStore struct {
	*memoryUsageStore
	muRegistry sync.Mutex
	records    map[string]KeyRegistryRecord
}

func newMemoryRegistryStore() *memoryRegistryStore {
	return &memoryRegistryStore{memoryUsageStore: newMemoryUsageStore(), records: make(map[string]KeyRegistryRecord)}
}

func (s *memoryRegistryStore) ListTronscanAPIKeys(_ context.Context) ([]KeyRegistryRecord, error) {
	s.muRegistry.Lock()
	defer s.muRegistry.Unlock()
	out := make([]KeyRegistryRecord, 0, len(s.records))
	for _, record := range s.records {
		out = append(out, record)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Fingerprint < out[j].Fingerprint })
	return out, nil
}

func (s *memoryRegistryStore) UpsertTronscanAPIKey(_ context.Context, fingerprint, key string, enabled bool, now time.Time) error {
	s.muRegistry.Lock()
	defer s.muRegistry.Unlock()
	s.records[fingerprint] = KeyRegistryRecord{Fingerprint: fingerprint, APIKey: key, Enabled: enabled, Health: "suspect", NextProbeAt: now}
	return nil
}

func (s *memoryRegistryStore) DeleteTronscanAPIKey(_ context.Context, fingerprint string) error {
	s.muRegistry.Lock()
	defer s.muRegistry.Unlock()
	delete(s.records, fingerprint)
	return nil
}

func (s *memoryRegistryStore) UpdateTronscanAPIKeyState(_ context.Context, record KeyRegistryRecord, _ time.Time) error {
	s.muRegistry.Lock()
	defer s.muRegistry.Unlock()
	if _, exists := s.records[record.Fingerprint]; exists {
		s.records[record.Fingerprint] = record
	}
	return nil
}

func newMemoryUsageStore() *memoryUsageStore {
	return &memoryUsageStore{records: make(map[string]KeyUsageRecord)}
}

func (s *memoryUsageStore) id(fingerprint, day string) string { return fingerprint + ":" + day }

func (s *memoryUsageStore) LoadTronscanKeyUsage(_ context.Context, fingerprints []string, day string) (map[string]KeyUsageRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]KeyUsageRecord)
	for _, fingerprint := range fingerprints {
		if record, ok := s.records[s.id(fingerprint, day)]; ok {
			out[fingerprint] = record
		}
	}
	return out, nil
}

func (s *memoryUsageStore) ReserveTronscanKeyRequest(_ context.Context, fingerprint, day string, source RequestSource, failover bool, limit int, _ time.Time) (KeyUsageRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.id(fingerprint, day)
	record := s.records[id]
	record.Fingerprint = fingerprint
	record.BudgetDay = day
	if limit > 0 && record.RequestCount >= limit {
		return record, false, nil
	}
	record.RequestCount++
	switch source {
	case RequestSourceMain:
		record.MainRequestCount++
	case RequestSourceCompensation, RequestSourceExpand:
		record.CompRequestCount++
	default:
		record.OtherRequestCount++
	}
	if failover {
		record.FailoverCount++
	}
	s.records[id] = record
	return record, true, nil
}

func (s *memoryUsageStore) RecordTronscanKeyResult(_ context.Context, fingerprint, day string, status int, last429At, cooldownUntil, disabledUntil time.Time) (KeyUsageRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.id(fingerprint, day)
	record := s.records[id]
	record.Fingerprint = fingerprint
	record.BudgetDay = day
	record.LastHTTPStatus = status
	if status == 429 {
		record.RateLimitCount++
		record.Last429At = last429At
	}
	if status == 401 || status == 403 {
		record.AuthErrorCount++
	}
	record.CooldownUntil = cooldownUntil
	record.DisabledUntil = disabledUntil
	s.records[id] = record
	return record, nil
}

func (s *memoryUsageStore) PersistTronscanKeyUsage(_ context.Context, records []KeyUsageRecord, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, record := range records {
		id := s.id(record.Fingerprint, record.BudgetDay)
		current := s.records[id]
		if current.RequestCount <= record.RequestCount {
			s.records[id] = record
		}
	}
	return nil
}
