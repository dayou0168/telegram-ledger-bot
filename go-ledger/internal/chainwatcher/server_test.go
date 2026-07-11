package chainwatcher

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/config"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
)

func TestReadyzReflectsSourceFailureAndEmptySuccess(t *testing.T) {
	now := time.Now()
	server := NewServer(config.ChainWatcherConfig{
		PollInterval:      time.Second,
		Lookback:          10 * time.Minute,
		AddressMaxPerTick: 1,
	}, nil, nil)

	server.status.recordScanError("global", errTest("tronscan unavailable"), now.Add(5*time.Second), 10*time.Millisecond, now)
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

type errTest string

func (e errTest) Error() string { return string(e) }

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
