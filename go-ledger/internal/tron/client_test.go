package tron

import (
	"encoding/json"
	"fmt"
	"net/http"
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
