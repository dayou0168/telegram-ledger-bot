package chainwatcher

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/config"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/tron"
)

func TestReadyzReflectsSourceFailureAndEmptySuccess(t *testing.T) {
	now := time.Now()
	server := NewServer(config.ChainWatcherConfig{
		PollInterval:      time.Second,
		Lookback:          10 * time.Minute,
		AddressMaxPerTick: 1,
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

func TestAddressBatchUsesCursorAndLimit(t *testing.T) {
	server := NewServer(config.ChainWatcherConfig{AddressMaxPerTick: 2}, nil, nil)
	subs := map[string][]storage.ChainWatcherSubscription{
		"T3": {{Address: "T3"}},
		"T1": {{Address: "T1"}},
		"T2": {{Address: "T2"}},
	}

	first := server.selectAddressBatch(subs)
	second := server.selectAddressBatch(subs)
	if got := joinStrings(first); got != "T1,T2" {
		t.Fatalf("first batch = %q, want T1,T2", got)
	}
	if got := joinStrings(second); got != "T3,T1" {
		t.Fatalf("second batch = %q, want T3,T1", got)
	}
}

func TestAddressScanSkipsNearNextGlobalPoll(t *testing.T) {
	now := time.Unix(2000, 0)
	server := NewServer(config.ChainWatcherConfig{
		PollInterval:      time.Second,
		RequestInterval:   250 * time.Millisecond,
		AddressMaxPerTick: 1,
	}, nil, nil)
	server.status.recordScanSuccess("global", scanResult{}, 400*time.Millisecond, now.Add(-600*time.Millisecond))

	if !server.shouldSkipAddressScan(now) {
		t.Fatal("address scan should skip when next global poll is near")
	}
}

func TestGlobalScanOverlapIsCounted(t *testing.T) {
	server := NewServer(config.ChainWatcherConfig{PollInterval: time.Second, AddressMaxPerTick: 1}, nil, nil)
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

func TestStatusIncludesSegmentTimingsAndAPICounts(t *testing.T) {
	server := NewServer(config.ChainWatcherConfig{PollInterval: time.Second, AddressMaxPerTick: 1}, nil, nil)
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
	server := NewServer(config.ChainWatcherConfig{PollInterval: time.Second, AddressMaxPerTick: 1}, nil, nil)
	result := scanResult{}
	result.observeFetch(tron.FetchMetrics{Calls: 2, Pages: 2, LastPageRows: 50, ReachedWindow: false})
	result.observePageLimit(tron.FetchMetrics{Pages: 2, LastPageRows: 50, ReachedWindow: false}, 2, 50)
	server.status.recordScanSuccess("address", result, 20*time.Millisecond, time.Now())

	status := server.statusResponse(contextWithoutCancel{}, time.Now())
	if !status.Address.PageLimitReached {
		t.Fatal("address page limit should be marked reached")
	}
	if len(status.Address.Recent) != 1 || !status.Address.Recent[0].PageLimitReached {
		t.Fatalf("recent page limit flag missing: %#v", status.Address.Recent)
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

func TestAddressWatermarkMinTimestamp(t *testing.T) {
	server := NewServer(config.ChainWatcherConfig{}, nil, nil)
	if got := server.addressMinTimestamp("T1", 1000); got != 1000 {
		t.Fatalf("empty watermark min = %d, want 1000", got)
	}
	server.updateAddressWatermark("T1", 50000)
	if got := server.addressMinTimestamp("T1", 1000); got != 20000 {
		t.Fatalf("watermark min = %d, want 20000", got)
	}
	if got := server.addressMinTimestamp("T1", 25000); got != 25000 {
		t.Fatalf("watermark should not go before default min, got %d", got)
	}
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
