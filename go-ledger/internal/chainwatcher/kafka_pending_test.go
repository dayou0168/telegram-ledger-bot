package chainwatcher

import (
	"testing"
)

func TestParseConfirmedUSDTTransfer(t *testing.T) {
	raw := []byte(`{"schema":"tron.confirmed.v1","txId":"ABCDEF","kind":"usdt_transfer","timestamp":1786916328000,"blockNumber":85415057,"transactionIndex":"0","status":"SUCCESS","fromAddress":"TFrom","toAddress":"TTo","amount":"70000000","tokenSymbol":"USDT","tokenAddress":"TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t","tokenDecimals":6}`)
	transfer, relevant, err := ParseConfirmedTransfer(raw, "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t")
	if err != nil {
		t.Fatal(err)
	}
	if !relevant || transfer.Hash != "abcdef" || transfer.Value != "70000000" || transfer.Result != "SUCCESS" || !transfer.Confirmed {
		t.Fatalf("confirmed transfer = %+v relevant=%v", transfer, relevant)
	}
}

func TestParseConfirmedFailedTRXTransfer(t *testing.T) {
	raw := []byte(`{"schema":"tron.confirmed.v1","txId":"deadbeef","kind":"trx_transfer","timestamp":1786916328000,"blockNumber":85415057,"transactionIndex":"1","status":"REVERT","fromAddress":"TFrom","toAddress":"TTo","amount":1234567,"tokenSymbol":"TRX","tokenAddress":"trx","tokenDecimals":6}`)
	transfer, relevant, err := ParseConfirmedTransfer(raw, "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t")
	if err != nil {
		t.Fatal(err)
	}
	if !relevant || transfer.TokenSymbol != "TRX" || transfer.Value != "1234567" || transfer.Result != "FAILED" || transfer.EventIndex != "1" {
		t.Fatalf("confirmed transfer = %+v relevant=%v", transfer, relevant)
	}
}

func TestMovementKeyIgnoresPendingAndConfirmedTimestamps(t *testing.T) {
	pendingRaw := []byte(`{"schema":"tron.pending.v1","txId":"abc","kind":"trx_transfer","contractType":"TransferContract","timestamp":1000,"contractValue":{"owner_address":"TFrom","to_address":"TTo","amount":1000000}}`)
	pending, relevant, err := ParsePendingTransfer(pendingRaw, "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t")
	if err != nil || !relevant {
		t.Fatalf("pending parse = %+v/%v/%v", pending, relevant, err)
	}
	confirmedRaw := []byte(`{"schema":"tron.confirmed.v1","txId":"abc","kind":"trx_transfer","timestamp":2000,"blockNumber":1,"transactionIndex":"0","status":"SUCCESS","fromAddress":"TFrom","toAddress":"TTo","amount":"1000000","tokenSymbol":"TRX","tokenAddress":"trx","tokenDecimals":6}`)
	confirmed, relevant, err := ParseConfirmedTransfer(confirmedRaw, "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t")
	if err != nil || !relevant {
		t.Fatalf("confirmed parse = %+v/%v/%v", confirmed, relevant, err)
	}
	if MovementKey(pending) != MovementKey(confirmed) {
		t.Fatalf("movement keys differ: %s != %s", MovementKey(pending), MovementKey(confirmed))
	}
}
