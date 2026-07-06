package tron

import (
	"encoding/json"
	"testing"
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
