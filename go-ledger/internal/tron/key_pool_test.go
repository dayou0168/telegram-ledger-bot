package tron

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
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

func TestMainCapacityUsesPerKeyFiveRPS(t *testing.T) {
	pool := newKeyPool([]string{"key1"}, KeyPoolOptions{MinInterval: 200 * time.Millisecond})
	pool.configureMainBudget(3, time.Second)
	status := pool.status(time.Now())
	if status.RequiredMainKeyCount != 1 || !status.MainCapacitySafe || status.CapacityWarning != "" {
		t.Fatalf("status = %+v", status)
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
		now = now.Add(time.Second)
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

func TestTenKeyPoolFairnessAndFixedThreeRequestsPerRound(t *testing.T) {
	keys := make([]string, 10)
	for i := range keys {
		keys[i] = fmt.Sprintf("key-%d", i)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeShortTransferPage(w) }))
	defer server.Close()
	client := NewClientWithKeys(server.URL, keys, time.Second, KeyPoolOptions{})
	const rounds = 100
	for i := 0; i < rounds; i++ {
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

func TestDynamicKeyCountsKeepExactlyThreePageJobs(t *testing.T) {
	for _, count := range []int{0, 1, 2, 3, 4, 9, 10} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			keys := make([]string, count)
			for i := range keys {
				keys[i] = fmt.Sprintf("key-%d", i)
			}
			pool := newKeyPool(keys, KeyPoolOptions{})
			leases, err := pool.mainLeases(context.Background(), 3)
			if count == 0 {
				if err == nil {
					t.Fatal("zero-key pool returned leases")
				}
				return
			}
			if err != nil || len(leases) != 3 {
				t.Fatalf("leases = %d/%v", len(leases), err)
			}
			unique := make(map[string]struct{})
			for _, lease := range leases {
				unique[lease.fingerprint] = struct{}{}
			}
			wantUnique := count
			if wantUnique > 3 {
				wantUnique = 3
			}
			if len(unique) != wantUnique {
				t.Fatalf("unique keys = %d, want %d", len(unique), wantUnique)
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
	client := NewClientWithKeys(server.URL, []string{"k1", "k2", "k3", "k4"}, 2*time.Second, KeyPoolOptions{})
	client.ConfigureMainBudget(3, time.Second)
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
	pool := newKeyPool([]string{"key1", "key2", "key3"}, KeyPoolOptions{MinInterval: 100 * time.Millisecond})
	pool.now = func() time.Time { return now }
	pool.configureMainBudget(3, time.Second)
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
	now = now.Add(500 * time.Millisecond)
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

func TestRegistryRejectsEleventhKeyAndDisabledConsumesSlot(t *testing.T) {
	store := newMemoryRegistryStore()
	for i := 0; i < MaxConfiguredKeys; i++ {
		key := fmt.Sprintf("key-%d", i)
		if err := store.UpsertTronscanAPIKey(context.Background(), keyFingerprint(key), key, i%2 == 0, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.UpsertTronscanAPIKey(context.Background(), keyFingerprint("key-11"), "key-11", true, time.Now()); err == nil {
		t.Fatal("eleventh key was accepted")
	}
	if err := store.DeleteTronscanAPIKey(context.Background(), keyFingerprint("key-1")); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertTronscanAPIKey(context.Background(), keyFingerprint("replacement"), "replacement", true, time.Now()); err != nil {
		t.Fatalf("replacement after delete failed: %v", err)
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
	if _, exists := s.records[fingerprint]; !exists && len(s.records) >= MaxConfiguredKeys {
		return fmt.Errorf("maximum is %d", MaxConfiguredKeys)
	}
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
	case RequestSourceCompensation:
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
