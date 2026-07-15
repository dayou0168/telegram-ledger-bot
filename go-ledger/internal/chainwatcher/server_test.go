package chainwatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/config"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/tron"
)

func TestReadyzReflectsSourceFailureAndEmptySuccess(t *testing.T) {
	now := time.Now()
	server := NewServer(config.ChainWatcherConfig{
		PollInterval: time.Second,
		Lookback:     10 * time.Minute,
	}, nil, nil)

	server.status.recordScanError("global", errTest("tronscan unavailable"), now.Add(5*time.Second), scanResult{}, 10*time.Millisecond, now)
	rec := httptest.NewRecorder()
	server.handleReady(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz after source error = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}

	server.status.recordScanSuccess("global", scanResult{}, 10*time.Millisecond, time.Now())
	rec = httptest.NewRecorder()
	server.handleReady(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("readyz after empty successful scan = %d, want %d", rec.Code, http.StatusOK)
	}
	var ready ReadyStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &ready); err != nil {
		t.Fatal(err)
	}
	if !ready.SourceReady {
		t.Fatalf("successful empty scan must report source_ready: %+v", ready)
	}
}

func TestReadyzDoesNotFlapWhenOlderRoundTimesOutAfterNewerSuccess(t *testing.T) {
	now := time.Now()
	server := NewServer(config.ChainWatcherConfig{
		PollInterval: time.Second,
		Lookback:     10 * time.Minute,
	}, nil, nil)

	server.status.recordScanSuccess("global", scanResult{RoundID: 12}, 20*time.Millisecond, now)
	server.status.recordScanError("global", context.DeadlineExceeded, now.Add(time.Second), scanResult{RoundID: 11}, 3*time.Second, now.Add(10*time.Millisecond))
	rec := httptest.NewRecorder()
	server.handleReady(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("readyz after stale round timeout = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var ready ReadyStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &ready); err != nil {
		t.Fatal(err)
	}
	if !ready.SourceReady {
		t.Fatalf("older failed round overrode newer source success: %+v", ready)
	}
}

func TestReadyzToleratesOneFreshFailureThenDegradesAndRecovers(t *testing.T) {
	now := time.Unix(10_000, 0)
	server := NewServer(config.ChainWatcherConfig{PollInterval: time.Second, Lookback: 10 * time.Minute}, nil, nil)
	server.status.recordScanSuccess("global", scanResult{RoundID: 1}, 20*time.Millisecond, now)
	server.status.recordScanError("global", context.DeadlineExceeded, time.Time{}, scanResult{RoundID: 2}, time.Second, now.Add(time.Second))

	isolated := server.readinessResponse(context.Background(), now.Add(time.Second))
	if !isolated.Ready || !isolated.SourceReady || isolated.Status != "ready" {
		t.Fatalf("isolated fresh failure readiness = %+v, want source ready", isolated)
	}

	server.status.recordScanError("global", context.DeadlineExceeded, time.Time{}, scanResult{RoundID: 3}, time.Second, now.Add(2*time.Second))
	failed := server.readinessResponse(context.Background(), now.Add(2*time.Second))
	if failed.Ready || failed.SourceReady || failed.Status != "degraded" {
		t.Fatalf("consecutive failure readiness = %+v, want source degraded", failed)
	}

	server.status.recordScanSuccess("global", scanResult{RoundID: 4}, 20*time.Millisecond, now.Add(3*time.Second))
	recovered := server.readinessResponse(context.Background(), now.Add(3*time.Second))
	if !recovered.Ready || !recovered.SourceReady {
		t.Fatalf("recovered readiness = %+v, want source ready", recovered)
	}

	stale := server.readinessResponse(context.Background(), now.Add(9*time.Second))
	if stale.Ready || stale.SourceReady {
		t.Fatalf("stale success readiness = %+v, want source degraded", stale)
	}
}

func TestContinuityDegradationPreservesSourceReason(t *testing.T) {
	continuityOnly := ReadyStatusResponse{Status: "ready", Ready: true, SourceReady: true}
	applyContinuityReadiness(&continuityOnly)
	if continuityOnly.Ready || !continuityOnly.SourceReady || continuityOnly.Status != "degraded/continuity" {
		t.Fatalf("continuity-only readiness = %+v", continuityOnly)
	}

	sourceFailure := ReadyStatusResponse{Status: "degraded", Ready: false, SourceReady: false}
	applyContinuityReadiness(&sourceFailure)
	if sourceFailure.Status != "degraded" || sourceFailure.SourceReady {
		t.Fatalf("continuity overwrote source failure = %+v", sourceFailure)
	}
}

func TestGapWorkerPollingUsesTokensWithoutLockingToRealtimeSecond(t *testing.T) {
	if got := gapWorkerPollInterval(30*time.Second, 10*time.Second, 0); got != 250*time.Millisecond {
		t.Fatalf("priority worker interval = %v, want 250ms", got)
	}
	if got := gapWorkerPollInterval(30*time.Second, 10*time.Second, 1); got != 10*time.Second {
		t.Fatalf("secondary worker interval = %v, want fairness bound", got)
	}
	deferred := &tron.CompensationDeferredError{Reason: "compensation_token_bucket_empty"}
	if got := gapRetryDelay(20, deferred, time.Second); got != 250*time.Millisecond {
		t.Fatalf("deferred retry = %v, want 250ms and independent of attempts", got)
	}
}

func TestReadyzDegradedWhenAllKeysRateLimited(t *testing.T) {
	now := time.Now()
	server := NewServer(config.ChainWatcherConfig{PollInterval: time.Second, Lookback: 10 * time.Minute}, nil, nil)
	server.status.recordScanSuccess("global", scanResult{}, 10*time.Millisecond, now.Add(-time.Second))
	server.status.recordScanError("global", &tron.HTTPError{StatusCode: http.StatusTooManyRequests, Body: "all keys unavailable"}, now.Add(30*time.Second), scanResult{}, 10*time.Millisecond, now)
	server.status.recordScanError("global", &tron.HTTPError{StatusCode: http.StatusTooManyRequests, Body: "all keys unavailable"}, now.Add(30*time.Second), scanResult{}, 10*time.Millisecond, now.Add(time.Millisecond))
	rec := httptest.NewRecorder()
	server.handleReady(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestAPIBackoffPreservesExplicitLongRetryAfter(t *testing.T) {
	now := time.Now()
	var backoff apiBackoff
	if !backoff.record(&tron.HTTPError{StatusCode: http.StatusTooManyRequests, RetryAfter: 6 * time.Hour}, now) {
		t.Fatal("429 should enable backoff")
	}
	remaining := backoff.untilTime().Sub(now)
	if remaining != 6*time.Hour {
		t.Fatalf("backoff = %v, want 6h", remaining)
	}
}

func TestGlobalScanOverlapIsCounted(t *testing.T) {
	server := NewServer(config.ChainWatcherConfig{PollInterval: time.Second}, nil, nil)
	if !server.tryStartScan("global") {
		t.Fatal("first global scan should start")
	}
	if server.tryStartScan("global") {
		t.Fatal("overlapping global scan should not start")
	}
	server.status.recordScanOverlap("global")
	status := server.statusResponse(contextWithoutCancel{}, time.Now())
	if status.Global.OverlapSkipped != 1 {
		t.Fatalf("global overlap skipped = %d, want 1", status.Global.OverlapSkipped)
	}
	server.finishScan("global")
	if !server.tryStartScan("global") {
		t.Fatal("global scan should start after finish")
	}
}

func TestGlobalScanAllowsAtMostThreeInflightRounds(t *testing.T) {
	server := NewServer(config.ChainWatcherConfig{PollInterval: time.Second, MainMaxInflight: 3}, nil, nil)
	for i := 0; i < 3; i++ {
		if !server.tryStartScan("global") {
			t.Fatalf("round %d was rejected before limit", i+1)
		}
	}
	if server.tryStartScan("global") {
		t.Fatal("fourth inflight round should be rejected")
	}
	if got := server.globalInflight(); got != 3 {
		t.Fatalf("inflight rounds = %d, want 3", got)
	}
	server.finishScan("global")
	if !server.tryStartScan("global") {
		t.Fatal("new round should start after one slot is released")
	}
}

func TestRoundIDIsVisibleInRecentStatus(t *testing.T) {
	server := NewServer(config.ChainWatcherConfig{PollInterval: time.Second}, nil, nil)
	server.status.recordScanSuccess("global", scanResult{RoundID: 42, APICallCount: 3}, time.Millisecond, time.Now())
	status := server.statusResponse(contextWithoutCancel{}, time.Now())
	if status.Global.RoundID != 42 || len(status.Global.Recent) != 1 || status.Global.Recent[0].RoundID != 42 {
		t.Fatalf("round status = %+v", status.Global)
	}
}

func TestOlderSlowRoundFailureDoesNotOverrideNewerSuccessReadiness(t *testing.T) {
	now := time.Now()
	server := NewServer(config.ChainWatcherConfig{PollInterval: time.Second}, nil, nil)
	server.status.recordScanSuccess("global", scanResult{RoundID: 2}, time.Second, now)
	server.status.recordScanError("global", context.DeadlineExceeded, time.Time{}, scanResult{RoundID: 1}, 3*time.Second, now.Add(time.Millisecond))
	if ready := server.status.response(now.Add(time.Second), 5*time.Second, storage.ChainWatcherDeliveryStats{}).Ready; !ready {
		t.Fatal("older failed round overrode newer successful round")
	}
	server.status.recordScanError("global", context.DeadlineExceeded, time.Time{}, scanResult{RoundID: 3}, 3*time.Second, now.Add(2*time.Millisecond))
	if ready := server.status.response(now.Add(time.Second), 5*time.Second, storage.ChainWatcherDeliveryStats{}).Ready; !ready {
		t.Fatal("one newer failed round should not degrade a fresh source")
	}
	server.status.recordScanError("global", context.DeadlineExceeded, time.Time{}, scanResult{RoundID: 4}, 3*time.Second, now.Add(3*time.Millisecond))
	if ready := server.status.response(now.Add(time.Second), 5*time.Second, storage.ChainWatcherDeliveryStats{}).Ready; ready {
		t.Fatal("consecutive newer failed rounds should degrade readiness")
	}
}

func TestOlderSlowRoundSuccessStaysHistorical(t *testing.T) {
	now := time.Now()
	server := NewServer(config.ChainWatcherConfig{PollInterval: time.Second}, nil, nil)
	server.status.recordScanSuccess("global", scanResult{RoundID: 2, APICallCount: 3}, time.Second, now)
	server.status.recordHistoricalSuccess("global", scanResult{RoundID: 1, APICallCount: 9}, 3*time.Second, now.Add(time.Millisecond))
	status := server.status.response(now.Add(time.Second), 5*time.Second, storage.ChainWatcherDeliveryStats{})
	if status.Global.RoundID != 2 || status.Global.APICallCount != 3 {
		t.Fatalf("historical success overwrote latest status: %+v", status.Global)
	}
	if len(status.Global.Recent) != 2 {
		t.Fatalf("historical success missing from bounded diagnostics: %+v", status.Global.Recent)
	}
}

func TestPartialMainScanCreatesOnlyExactFailedPageGaps(t *testing.T) {
	tasks := failedPageGapTasks(scanResult{
		MinTimestamp: 1000, CutoffTimestamp: 2000,
		FailedPages: []tron.PageFailure{{Page: 1, Error: "timeout"}, {Page: 2, Error: "429"}},
	})
	if len(tasks) != 2 {
		t.Fatalf("gap tasks = %d, want 2", len(tasks))
	}
	for index, task := range tasks {
		wantPage := index + 1
		if task.Kind != "page" || task.Priority != 0 || task.StartPage != wantPage || task.NextPage != wantPage || task.EndPage != wantPage+1 || task.FromTimestamp != 1000 || task.ToTimestamp != 2000 {
			t.Fatalf("task %d = %+v", index, task)
		}
	}
}

func TestStatusRequiresAdminAuthentication(t *testing.T) {
	server := NewServer(config.ChainWatcherConfig{AdminToken: "admin-token"}, nil, nil)
	recorder := httptest.NewRecorder()
	server.handleStatus(recorder, httptest.NewRequest(http.MethodGet, "/status", nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status code = %d, want 401", recorder.Code)
	}
	request := httptest.NewRequest(http.MethodGet, "/status", nil)
	request.Header.Set("Authorization", "Bearer admin-token")
	recorder = httptest.NewRecorder()
	server.handleStatus(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("authenticated status code = %d, want 200", recorder.Code)
	}
}

func TestReadyResponseDoesNotExposeDetailedDiagnostics(t *testing.T) {
	server := NewServer(config.ChainWatcherConfig{PollInterval: time.Second}, nil, nil)
	server.status.recordScanSuccess("global", scanResult{RoundID: 7, PreviousAnchorID: "secret-anchor"}, time.Millisecond, time.Now())
	recorder := httptest.NewRecorder()
	server.handleReady(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	body := recorder.Body.String()
	for _, forbidden := range []string{"tronscan_keys", "previous_anchor_id", "recent", "secret-anchor"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("ready response leaked %q: %s", forbidden, body)
		}
	}
}

func TestStatusIncludesSegmentTimingsAndAPICounts(t *testing.T) {
	server := NewServer(config.ChainWatcherConfig{PollInterval: time.Second, HeadMaxConcurrency: 32, HeadPersistConcurrency: 8}, nil, nil)
	server.status.recordScanSuccess("global", scanResult{
		TransferCount:    3,
		MatchCount:       2,
		AddressCount:     2,
		APICallCount:     3,
		PageCount:        3,
		PageLimit:        6,
		APIWaitDuration:  5 * time.Millisecond,
		APIFetchDuration: 20 * time.Millisecond,
		ParseDuration:    2 * time.Millisecond,
		MatchDuration:    1 * time.Millisecond,
		WriteDuration:    4 * time.Millisecond,
	}, 40*time.Millisecond, time.Now())
	status := server.statusResponse(contextWithoutCancel{}, time.Now())
	if status.Global.APICallCount != 3 || status.Global.PageCount != 3 || status.Global.PageLimit != 6 {
		t.Fatalf("api counts/limit = %d/%d/%d, want 3/3/6", status.Global.APICallCount, status.Global.PageCount, status.Global.PageLimit)
	}
	if status.Global.APIFetchMS != 20 || status.Global.ParseMS != 2 || status.Global.WriteMS != 4 {
		t.Fatalf("timings = api:%d parse:%d write:%d", status.Global.APIFetchMS, status.Global.ParseMS, status.Global.WriteMS)
	}
	if len(status.Global.Recent) != 1 || status.Global.Recent[0].APICallCount != 3 {
		t.Fatalf("recent status missing metrics: %#v", status.Global.Recent)
	}
	if status.HeadAPIMaxConcurrency != 32 || status.HeadPersistWorkers != 8 || status.HeadPriorityDBLanes != 1 {
		t.Fatalf("head lanes = api %d normal %d priority %d", status.HeadAPIMaxConcurrency, status.HeadPersistWorkers, status.HeadPriorityDBLanes)
	}
}

func TestStatusIncludesPageLimitReached(t *testing.T) {
	server := NewServer(config.ChainWatcherConfig{PollInterval: time.Second}, nil, nil)
	result := scanResult{}
	result.observeFetch(tron.FetchMetrics{Calls: 2, Pages: 2, LastPageRows: 50, ReachedWindow: false})
	result.observePageLimit(tron.FetchMetrics{Pages: 2, LastPageRows: 50, ReachedWindow: false}, 2, 50)
	server.status.recordScanSuccess("catchup", result, 20*time.Millisecond, time.Now())

	status := server.statusResponse(contextWithoutCancel{}, time.Now())
	if !status.Catchup.PageLimitReached {
		t.Fatal("catchup page limit should be marked reached")
	}
	if len(status.Catchup.Recent) != 1 || !status.Catchup.Recent[0].PageLimitReached {
		t.Fatalf("recent page limit flag missing: %#v", status.Catchup.Recent)
	}
}

func TestPageLimitReachedIgnoresWindowReached(t *testing.T) {
	var result scanResult
	result.observePageLimit(tron.FetchMetrics{Pages: 2, LastPageRows: 50, ReachedWindow: true}, 2, 50)
	if result.PageLimitReached {
		t.Fatal("page limit should not be marked when scan reached the time window")
	}
	result.observePageLimit(tron.FetchMetrics{Pages: 1, LastPageRows: 10, ReachedWindow: false}, 2, 50)
	if result.PageLimitReached {
		t.Fatal("page limit should not be marked when configured pages were not exhausted")
	}
}

func TestGlobalCatchupRequestCountDoesNotGrowWithWatchAddresses(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"token_transfers":[]}`)
	}))
	defer api.Close()
	for _, count := range []int{10, 1000, 10000} {
		client := tron.NewClient(api.URL, "", time.Second)
		server := NewServer(config.ChainWatcherConfig{CatchupPages: 1, CatchupMaxRequests: 3}, nil, client)
		addresses := make(map[string][]storage.ChainWatcherSubscription, count)
		for i := 0; i < count; i++ {
			address := fmt.Sprintf("T%04d", i)
			addresses[address] = []storage.ChainWatcherSubscription{{Address: address}}
		}
		budget := 3
		advanced, result, err := server.scanCatchupWindow(context.Background(), 1000, 2000, addresses, &budget)
		if err != nil {
			t.Fatal(err)
		}
		if advanced != 2000 || result.APICallCount != 1 {
			t.Fatalf("addresses=%d advanced/calls = %d/%d, want 2000/1", count, advanced, result.APICallCount)
		}
	}
}

func TestCatchupSplitsSaturatedWindowWithoutSkippingGap(t *testing.T) {
	var mu sync.Mutex
	var windows [][2]int64
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		from, _ := strconv.ParseInt(r.URL.Query().Get("start_timestamp"), 10, 64)
		to, _ := strconv.ParseInt(r.URL.Query().Get("end_timestamp"), 10, 64)
		mu.Lock()
		windows = append(windows, [2]int64{from, to})
		mu.Unlock()
		rows := 1
		if to-from > 1000 {
			rows = 50
		}
		fmt.Fprint(w, `{"token_transfers":[`)
		for i := 0; i < rows; i++ {
			if i > 0 {
				fmt.Fprint(w, ",")
			}
			fmt.Fprintf(w, `{"transaction_id":"%d-%d-%d","event_index":%d,"from_address":"A","to_address":"B","quant":"1","block_ts":%d,"tokenInfo":{"tokenId":"TR7"}}`, from, to, i, i, to-1)
		}
		fmt.Fprint(w, `]}`)
	}))
	defer api.Close()

	client := tron.NewClientWithKeys(api.URL, []string{"k1", "k2", "k3"}, 3*time.Second, tron.KeyPoolOptions{CompensationMaxRPS: 0.1})
	server := NewServer(config.ChainWatcherConfig{CatchupPages: 1, USDTContract: "TR7"}, nil, client)
	budget := 20
	advanced, result, err := server.scanCatchupWindow(context.Background(), 1000, 5000, map[string][]storage.ChainWatcherSubscription{}, &budget)
	if !tron.IsCompensationDeferred(err) {
		t.Fatalf("error = %v, want surplus deferral", err)
	}
	if advanced != 1000 || result.APICallCount != 1 {
		t.Fatalf("advanced/calls = %d/%d, want cursor preserved after one request", advanced, result.APICallCount)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(windows) != 1 || windows[0] != [2]int64{1000, 5000} {
		t.Fatalf("windows = %#v", windows)
	}
}

func TestCatchupOverlapIsCountedSeparately(t *testing.T) {
	client := tron.NewClientWithKeys("http://example.invalid", []string{"k1"}, time.Second, tron.KeyPoolOptions{})
	server := NewServer(config.ChainWatcherConfig{CatchupMaxInflight: 1}, nil, client)
	if !server.tryStartScan("catchup") || server.tryStartScan("catchup") {
		t.Fatal("catchup overlap guard failed")
	}
	server.status.recordScanOverlap("catchup")
	server.status.recordScanSuccess("catchup", scanResult{APICallCount: 2, PageCount: 2}, time.Millisecond, time.Now())
	status := server.status.response(time.Now(), time.Second, storage.ChainWatcherDeliveryStats{})
	if status.Catchup.OverlapSkipped != 1 || status.Catchup.APICallCount != 2 {
		t.Fatalf("catchup status = %+v", status.Catchup)
	}
}

func TestCatchupInflightTracksAvailableKeys(t *testing.T) {
	for _, test := range []struct {
		name      string
		available int
		budgetRPS float64
		maximum   int
		want      int
	}{
		{"no keys", 0, 8, 8, 0},
		{"one key", 1, 0.3, 8, 1},
		{"three keys", 3, 2.2, 8, 3},
		{"six keys", 6, 3.8, 8, 4},
		{"ten keys", 10, 8, 8, 8},
		{"twenty keys capped", 20, 18, 8, 8},
	} {
		status := tron.KeyPoolStatus{AvailableCount: test.available, CompensationBudgetRPS: test.budgetRPS}
		if got := effectiveCatchupConcurrency(status, test.maximum); got != test.want {
			t.Fatalf("%s: effective concurrency=%d, want %d", test.name, got, test.want)
		}
	}
}

func TestGapSchedulerGivesFairLaneBoundedClaimOpportunity(t *testing.T) {
	const fairnessEvery = 4
	for round := 1; round <= fairnessEvery; round++ {
		class := gapWorkerClass(1, 0, round, fairnessEvery)
		if round < fairnessEvery && class != "watcher_priority" {
			t.Fatalf("round %d class = %s, want priority", round, class)
		}
		if round == fairnessEvery && class != "watcher_fair" {
			t.Fatalf("round %d class = %s, want fair", round, class)
		}
	}
	if class := gapWorkerClass(6, 5, 1, fairnessEvery); class != "watcher_fair" {
		t.Fatalf("last worker class = %s, want dedicated fair lane", class)
	}
	if class := gapWorkerClass(6, 4, 1, fairnessEvery); class != "watcher_priority" {
		t.Fatalf("priority worker class = %s", class)
	}
}

func TestAdminKeyStatusDoesNotExposeSecret(t *testing.T) {
	client := tron.NewClientWithKeys("http://example.invalid", []string{"super-secret-key"}, time.Second, tron.KeyPoolOptions{})
	server := NewServer(config.ChainWatcherConfig{AdminToken: "admin-token"}, nil, client)
	request := httptest.NewRequest(http.MethodGet, "/v1/admin/keys", nil)
	request.Header.Set("Authorization", "Bearer admin-token")
	recorder := httptest.NewRecorder()
	server.handleAdminKeys(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status code = %d", recorder.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(recorder.Body.String(), "super-secret-key") {
		t.Fatalf("admin status leaked key: %s", recorder.Body.String())
	}
}

func TestAnchorCoverageFindsPreviousHead(t *testing.T) {
	transfers := []tron.Transfer{{Hash: "new", EventIndex: "0", TokenAddress: "TR7"}, {Hash: "old", EventIndex: "1", TokenAddress: "TR7"}}
	previous := EventID(transfers[1])
	head, found := AnchorCoverage(transfers, previous)
	if !found || head != EventID(transfers[0]) {
		t.Fatalf("coverage = %q/%v", head, found)
	}
}

func TestExpandResumesExactContinuationPageAndFindsAnchor(t *testing.T) {
	anchorTransfer := tron.Transfer{Hash: "anchor", EventIndex: "7", TokenAddress: "TR7", BlockTimestamp: 1000}
	anchor := EventID(anchorTransfer)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("start") != "300" {
			t.Errorf("start = %s, want 300", r.URL.Query().Get("start"))
		}
		fmt.Fprint(w, `{"token_transfers":[{"transaction_id":"anchor","event_index":7,"from_address":"A","to_address":"B","quant":"1","block_ts":1000,"tokenInfo":{"tokenId":"TR7","tokenAbbr":"USDT"}}]}`)
	}))
	defer api.Close()
	client := tron.NewClientWithKeys(api.URL, []string{"k1", "k2", "k3", "k4"}, time.Second, tron.KeyPoolOptions{})
	server := NewServer(config.ChainWatcherConfig{RecoverySafetyMaxPages: 256, USDTContract: "TR7"}, nil, client)
	server.subDirty = false
	server.subByAddress = map[string][]storage.ChainWatcherSubscription{}
	result, err := server.pollExpandOnce(context.Background(), expandTask{AnchorID: anchor, Cutoff: 2000, MinTimestamp: 1, StartPage: 6})
	if err != nil || !result.AnchorFound || result.PageCount != 1 {
		t.Fatalf("expand = %+v/%v", result, err)
	}
}

func TestExpandYieldsWhenSurplusBudgetIsExhausted(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(25 * time.Millisecond)
		writeFullTransferPageForExpand(w, r.URL.Query().Get("start"))
	}))
	defer api.Close()
	keys := []string{"k1", "k2", "k3", "k4", "k5", "k6", "k7", "k8", "k9", "k10"}
	client := tron.NewClientWithKeys(api.URL, keys, 2*time.Second, tron.KeyPoolOptions{CompensationMaxRPS: 0.1})
	server := NewServer(config.ChainWatcherConfig{RecoverySafetyMaxPages: 8, USDTContract: "TR7"}, nil, client)
	server.subDirty = false
	server.subByAddress = map[string][]storage.ChainWatcherSubscription{}
	result, err := server.pollExpandOnce(context.Background(), expandTask{AnchorID: "missing", Cutoff: 2000, MinTimestamp: 1, StartPage: 3})
	if !tron.IsCompensationDeferred(err) || result.AnchorFound || result.PageCount != 1 {
		t.Fatalf("expand = %+v/%v", result, err)
	}
}

func writeFullTransferPageForExpand(w http.ResponseWriter, start string) {
	fmt.Fprint(w, `{"token_transfers":[`)
	for i := 0; i < 50; i++ {
		if i > 0 {
			fmt.Fprint(w, ",")
		}
		fmt.Fprintf(w, `{"transaction_id":"h%s-%d","event_index":%d,"from_address":"A","to_address":"B","quant":"1","block_ts":1000,"tokenInfo":{"tokenId":"TR7"}}`, start, i, i)
	}
	fmt.Fprint(w, `]}`)
}

type errTest string

func (e errTest) Error() string { return string(e) }

type contextWithoutCancel struct{}

func (contextWithoutCancel) Deadline() (time.Time, bool) { return time.Time{}, false }
func (contextWithoutCancel) Done() <-chan struct{}       { return nil }
func (contextWithoutCancel) Err() error                  { return nil }
func (contextWithoutCancel) Value(key any) any           { return nil }

func joinStrings(values []string) string {
	if len(values) == 0 {
		return ""
	}
	out := values[0]
	for _, value := range values[1:] {
		out += "," + value
	}
	return out
}
