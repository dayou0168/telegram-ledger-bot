package bot

import (
	"strings"
	"testing"
	"time"
)

func TestLedgerClearTicketCallbackFitsTelegramLimit(t *testing.T) {
	data := ledgerClearCallbackData(strings.Repeat("a", 43))
	if len(data) > 64 {
		t.Fatalf("callback data is %d bytes, Telegram limit is 64", len(data))
	}
	if ledgerClearTicketTTL != 60*time.Second {
		t.Fatalf("ticket TTL = %s, want 60s", ledgerClearTicketTTL)
	}
}
