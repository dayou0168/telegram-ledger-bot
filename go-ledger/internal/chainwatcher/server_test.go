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
}

func TestReadyzDegradedWhenAllKeysRateLimited(t *testing.T) {
	now := time.Now()
	server := NewServer(config.ChainWatcherConfig{PollInterval: time.Second, Lookback: 10 * time.Minute}, nil, nil)
	server.status.recordScanSuccess("global", scanResult{}, 10*time.Millisecond, now.Add(-time.Second))
	server.status.recordScanError("global", &tron.HTTPError{StatusCode: http.StatusTooManyRequests, Body: "all keys unavailable"}, now.Add(30*time.Second), scanResult{}, 10*time.Millisecond, now)
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
	if ready := server.status.response(now.Add(time.Second), 5*time.Second, storage.ChainWatcherDeliveryStats{}).Ready; ready {
		t.Fatal("newer failed round should degrade readiness immediately")
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
	server := NewServer(config.ChainWatcherConfig{PollInterval: time.Second}, nil, nil)
	server.status.recordScanSuccess("global", scanResult{
		TransferCount:    3,
		MatchCount:       2,
		AddressCount:     2,
		APICallCount:     3,
		PageCount:        3,
		APIWaitDuration:  5 * time.Millisecond,
		APIFetchDuration: 20 * time.Millisecond,
		ParseDuration:    2 * time.Millisecond,
		MatchDuration:    1 * time.Millisecond,
		WriteDuration:    4 * time.Millisecond,
	}, 40*time.Millisecond, time.Now())
	status := server.statusResponse(contextWithoutCancel{}, time.Now())
	if status.Global.APICallCount != 3 || status.Global.PageCount != 3 {
		t.Fatalf("api counts = %d/%d, want 3/3", status.Global.APICallCount, status.Global.PageCount)
	}
	if status.Global.APIFetchMS != 20 || status.Global.ParseMS != 2 || status.Global.WriteMS != 4 {
		t.Fatalf("timings = api:%d parse:%d write:%d", status.Global.APIFetchMS, status.Global.ParseMS, status.Global.WriteMS)
	}
	if len(status.Global.Recent) != 1 || status.Global.Recent[0].APICallCount != 3 {
		t.Fatalf("recent status missing metrics: %#v", status.Global.Recent)
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
	for _, count := range []int{10, 1000} {
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

	client := tron.NewClientWithKeys(api.URL, []string{"k1", "k2", "k3"}, 3*time.Second, tron.KeyPoolOptions{CompensationMaxRPS: 12})
	server := NewServer(config.ChainWatcherConfig{CatchupPages: 1, USDTContract: "TR7"}, nil, client)
	budget := 20
	advanced, result, err := server.scanCatchupWindow(context.Background(), 1000, 5000, map[string][]storage.ChainWatcherSubscription{}, &budget)
	if err != nil {
		t.Fatal(err)
	}
	if advanced != 5000 {
		t.Fatalf("advanced = %d, want 5000", advanced)
	}
	if result.APICallCount != 7 {
		t.Fatalf("api calls = %d, want 7 split-window calls", result.APICallCount)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(windows) != 7 || windows[0] != [2]int64{1000, 5000} {
		t.Fatalf("windows = %#v", windows)
	}
}

func TestCatchupOverlapIsCountedSeparately(t *testing.T) {
	server := NewServer(config.ChainWatcherConfig{}, nil, nil)
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

func TestExpandStartsAtPageFourAndFindsAnchor(t *testing.T) {
	anchorTransfer := tron.Transfer{Hash: "anchor", EventIndex: "7", TokenAddress: "TR7", BlockTimestamp: 1000}
	anchor := EventID(anchorTransfer)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("start") != "150" {
			t.Errorf("start = %s, want 150", r.URL.Query().Get("start"))
		}
		fmt.Fprint(w, `{"token_transfers":[{"transaction_id":"anchor","event_index":7,"from_address":"A","to_address":"B","quant":"1","block_ts":1000,"tokenInfo":{"tokenId":"TR7","tokenAbbr":"USDT"}}]}`)
	}))
	defer api.Close()
	client := tron.NewClientWithKeys(api.URL, []string{"k1", "k2", "k3", "k4"}, time.Second, tron.KeyPoolOptions{})
	server := NewServer(config.ChainWatcherConfig{GlobalExpandPageLimit: 20, USDTContract: "TR7"}, nil, client)
	server.subDirty = false
	server.subByAddress = map[string][]storage.ChainWatcherSubscription{}
	result, err := server.pollExpandOnce(context.Background(), expandTask{AnchorID: anchor, Cutoff: 2000, MinTimestamp: 1, StartPage: 3})
	if err != nil || !result.AnchorFound || result.PageCount != 1 {
		t.Fatalf("expand = %+v/%v", result, err)
	}
}

func TestExpandTwentyPageLimitBecomesObservable(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(25 * time.Millisecond)
		writeFullTransferPageForExpand(w, r.URL.Query().Get("start"))
	}))
	defer api.Close()
	keys := []string{"k1", "k2", "k3", "k4", "k5", "k6", "k7", "k8", "k9", "k10"}
	client := tron.NewClientWithKeys(api.URL, keys, 2*time.Second, tron.KeyPoolOptions{CompensationMaxRPS: 50})
	server := NewServer(config.ChainWatcherConfig{GlobalExpandPageLimit: 20, USDTContract: "TR7"}, nil, client)
	server.subDirty = false
	server.subByAddress = map[string][]storage.ChainWatcherSubscription{}
	result, err := server.pollExpandOnce(context.Background(), expandTask{AnchorID: "missing", Cutoff: 2000, MinTimestamp: 1, StartPage: 3})
	if err != nil || result.AnchorFound || !result.PageLimitReached || result.PageCount != 17 {
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
