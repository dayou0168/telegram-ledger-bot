package tron

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestTronscanAddressTransferResponse(t *testing.T) {
	raw := []byte(`{
		"tokenInfo":{"tokenId":"TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t","tokenAbbr":"USDT","tokenDecimal":6},
		"data":[{
			"amount":"500900000",
			"block_timestamp":1783266231000,
			"from":"TCYugQbJeHtUZF9vNmFExXMnCPNgN7kPPV",
			"to":"TWqcMjV7Wq2RHe2CSiKQHpkn6A7B2AWUPe",
			"hash":"242a3a490a7a96b43bd4ec14b739c8cde8128d3371910ac3465d085f9a5fe02f",
			"confirmed":1,
			"decimals":6,
			"id":"TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"
		}]
	}`)
	var result tronscanTransferResponse
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(result.TokenTransfers) != 1 {
		t.Fatalf("transfers = %d, want 1", len(result.TokenTransfers))
	}
	transfer := result.TokenTransfers[0].toTransfer()
	if transfer.Value != "500900000" {
		t.Fatalf("value = %q", transfer.Value)
	}
	if transfer.TokenDecimals != 6 {
		t.Fatalf("decimals = %d", transfer.TokenDecimals)
	}
	if !transfer.Confirmed {
		t.Fatal("confirmed should parse numeric 1")
	}
	if transfer.From != "TCYugQbJeHtUZF9vNmFExXMnCPNgN7kPPV" || transfer.To != "TWqcMjV7Wq2RHe2CSiKQHpkn6A7B2AWUPe" {
		t.Fatalf("unexpected addresses: %+v", transfer)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Unix(1000, 0).UTC()
	if got := parseRetryAfter("7", now); got != 7*time.Second {
		t.Fatalf("numeric Retry-After = %s, want 7s", got)
	}
	when := now.Add(9 * time.Second).Format(http.TimeFormat)
	if got := parseRetryAfter(when, now); got != 9*time.Second {
		t.Fatalf("date Retry-After = %s, want 9s", got)
	}
	if got := parseRetryAfter("", now); got != 0 {
		t.Fatalf("empty Retry-After = %s, want 0", got)
	}
}

func TestIsRateLimitedUnwrapsHTTPError(t *testing.T) {
	err := fmt.Errorf("wrapped: %w", &HTTPError{StatusCode: http.StatusTooManyRequests})
	httpErr, ok := IsRateLimited(err)
	if !ok || httpErr == nil || httpErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("IsRateLimited() = %#v %v, want wrapped 429", httpErr, ok)
	}
}

func TestAddressFetchMetricsCountsPages(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		start := r.URL.Query().Get("start")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"token_transfers":[`)
		if start == "0" {
			for i := 0; i < 50; i++ {
				if i > 0 {
					fmt.Fprint(w, ",")
				}
				fmt.Fprintf(w, `{"transaction_id":"hash%s","from_address":"TFrom","to_address":"TTo","quant":"1","block_ts":%s,"confirmed":1,"tokenInfo":{"tokenId":"TR7","tokenAbbr":"USDT","tokenDecimal":6}}`, strconv.Itoa(i), strconv.FormatInt(200000-int64(i), 10))
			}
		}
		fmt.Fprint(w, `]}`)
	}))
	defer server.Close()

	client := NewClient(server.URL, "", time.Second)
	result, err := client.FetchAddressUSDTTransfersSincePagesWithMetrics(context.Background(), "TTo", "TR7", 50, 3, 1000)
	if err != nil {
		t.Fatalf("FetchAddressUSDTTransfersSincePagesWithMetrics() error = %v", err)
	}
	if calls != 2 || result.Metrics.Calls != 2 || result.Metrics.Pages != 2 {
		t.Fatalf("calls/pages = handler:%d metrics:%d/%d, want 2/2/2", calls, result.Metrics.Calls, result.Metrics.Pages)
	}
	if len(result.Transfers) != 50 {
		t.Fatalf("transfers = %d, want 50", len(result.Transfers))
	}
}

func TestNormalizeTimestampMillis(t *testing.T) {
	tests := []struct {
		name string
		in   int64
		want int64
	}{
		{name: "zero", in: 0, want: 0},
		{name: "seconds", in: 1783266231, want: 1783266231000},
		{name: "millis", in: 1783266231000, want: 1783266231000},
		{name: "micros", in: 1783266231000000, want: 1783266231000},
		{name: "nanos", in: 1783266231000000000, want: 1783266231000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeTimestampMillis(tt.in); got != tt.want {
				t.Fatalf("normalizeTimestampMillis(%d) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestFetchKeepsMultipleEventsFromSameTransaction(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"token_transfers":[{"transaction_id":"same","event_index":0,"from_address":"A","to_address":"B","quant":"1","block_ts":2000000000000},{"transaction_id":"same","event_index":1,"from_address":"A","to_address":"C","quant":"2","block_ts":2000000000000}]}`)
	}))
	defer server.Close()
	client := NewClient(server.URL, "", time.Second)
	result, err := client.FetchGlobalUSDTTransfersWithMetrics(context.Background(), "TR7", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Transfers) != 2 {
		t.Fatalf("transfers = %d, want 2", len(result.Transfers))
	}
	if result.Transfers[0].EventIndex == result.Transfers[1].EventIndex {
		t.Fatalf("event indexes collapsed: %+v", result.Transfers)
	}
}

func TestThreeHeadPagesShareOneCutoff(t *testing.T) {
	var mu sync.Mutex
	var cutoffs []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		cutoffs = append(cutoffs, r.URL.Query().Get("end_timestamp"))
		mu.Unlock()
		fmt.Fprint(w, `{"token_transfers":[]}`)
	}))
	defer server.Close()
	client := NewClientWithKeys(server.URL, []string{"k1", "k2", "k3"}, time.Second, KeyPoolOptions{})
	if _, err := client.FetchGlobalUSDTTransfersAtWithMetrics(context.Background(), "TR7", 1, 123456, 3); err != nil {
		t.Fatal(err)
	}
	if len(cutoffs) != 3 {
		t.Fatalf("calls = %d", len(cutoffs))
	}
	for _, cutoff := range cutoffs {
		if cutoff != "123456" {
			t.Fatalf("cutoffs = %v", cutoffs)
		}
	}
}

func TestGlobalFetchReturnsSuccessfulPagesWhenOnePageFails(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := r.URL.Query().Get("start")
		if start == "50" {
			http.Error(w, "temporary upstream failure", http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, `{"token_transfers":[{"transaction_id":"tx-%s","event_index":0,"from_address":"A","to_address":"B","quant":"1","block_ts":1000,"tokenInfo":{"tokenId":"TR7"}}]}`, start)
	}))
	defer api.Close()

	client := NewClientWithKeys(api.URL, []string{"k1", "k2", "k3", "k4", "k5"}, 2*time.Second, KeyPoolOptions{})
	result, err := client.FetchGlobalUSDTTransfersAtWithMetrics(context.Background(), "TR7", 1, 2_000, 3)
	if err == nil {
		t.Fatal("page failure should be returned")
	}
	if len(result.Transfers) != 2 || len(result.SuccessfulPages) != 2 || len(result.FailedPages) != 1 {
		t.Fatalf("partial result = transfers:%d successful:%v failed:%v", len(result.Transfers), result.SuccessfulPages, result.FailedPages)
	}
	if result.FailedPages[0].Page != 1 {
		t.Fatalf("failed page = %d, want 1", result.FailedPages[0].Page)
	}
}
