package chainwatcher

import (
	"testing"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/tron"
)

func TestMatchTransferFiltersByDirection(t *testing.T) {
	transfer := tron.Transfer{
		Hash:           "abcdef",
		From:           "TFrom",
		To:             "TTo",
		Value:          "10000000",
		TokenAddress:   "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t",
		TokenDecimals:  6,
		BlockTimestamp: 1720000000000,
	}
	subs := []storage.ChainWatcherSubscription{
		{BotID: "bot-a", ChatID: 1, OwnerUserID: 1, Address: "TTo", WatchIncome: true, WatchExpense: false, Active: true},
		{BotID: "bot-b", ChatID: 2, OwnerUserID: 2, Address: "TFrom", WatchIncome: true, WatchExpense: false, Active: true},
		{BotID: "bot-c", ChatID: 3, OwnerUserID: 3, Address: "TFrom", WatchIncome: false, WatchExpense: true, Active: true},
		{BotID: "bot-d", ChatID: 4, OwnerUserID: 4, Address: "TTo", WatchIncome: true, WatchExpense: true, Active: false},
		{BotID: "bot-e", ChatID: 5, OwnerUserID: 5, Address: "TTo", WatchIncome: true, WatchExpense: true, MinNotifyAmount: "11", Active: true},
	}

	matches := MatchTransfer(transfer, subs)
	if len(matches) != 2 {
		t.Fatalf("expected 2 matches, got %d: %#v", len(matches), matches)
	}
	if matches[0].BotID != "bot-a" || matches[0].Direction != "income" {
		t.Fatalf("unexpected first match: %#v", matches[0])
	}
	if matches[1].BotID != "bot-c" || matches[1].Direction != "expense" {
		t.Fatalf("unexpected second match: %#v", matches[1])
	}
}

func TestIDsAreStableAndTenantScoped(t *testing.T) {
	transfer := tron.Transfer{
		Hash:           "ABCDEF",
		From:           "TFrom",
		To:             "TTo",
		Value:          "10000000",
		TokenAddress:   "TR7",
		BlockTimestamp: 1720000000000,
	}
	sub := storage.ChainWatcherSubscription{BotID: "bot-a", ChatID: 1, OwnerUserID: 1, Address: "TTo"}

	eventA := EventID(transfer)
	eventB := EventID(transfer)
	if eventA == "" || eventA != eventB {
		t.Fatalf("event id is not stable: %q %q", eventA, eventB)
	}
	deliveryA := DeliveryID(sub, transfer, "income")
	sub.BotID = "bot-b"
	deliveryB := DeliveryID(sub, transfer, "income")
	if deliveryA == "" || deliveryA == deliveryB {
		t.Fatalf("delivery id should be stable and bot scoped: %q %q", deliveryA, deliveryB)
	}
}

func TestDeliveryIDDedupesSameTransferAcrossCompensationPages(t *testing.T) {
	transfer := tron.Transfer{
		Hash:           "ABCDEF",
		From:           "TFrom",
		To:             "TTo",
		Value:          "10000000",
		TokenAddress:   "TR7",
		BlockTimestamp: 1720000000000,
	}
	sub := storage.ChainWatcherSubscription{BotID: "bot-a", ChatID: 1, OwnerUserID: 1, Address: "TTo"}

	firstPage := DeliveryID(sub, transfer, "income")
	secondPage := DeliveryID(sub, transfer, "income")
	if firstPage == "" || firstPage != secondPage {
		t.Fatalf("same transfer should produce same delivery id: %q %q", firstPage, secondPage)
	}
}
