package storage

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestPostgresStoreBasicFlow(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	store, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	now := time.Now().UTC()
	suffix := now.UnixNano()
	chatID := -900000000000 - suffix%1000000
	userID := int64(700000000000 + suffix%1000000)

	claimed, err := store.ClaimUpdate(ctx, suffix, now)
	if err != nil {
		t.Fatalf("claim update: %v", err)
	}
	if !claimed {
		t.Fatalf("first update claim should be true")
	}
	claimed, err = store.ClaimUpdate(ctx, suffix, now)
	if err != nil {
		t.Fatalf("claim duplicate update: %v", err)
	}
	if claimed {
		t.Fatalf("duplicate update claim should be false")
	}

	if err := store.EnsureGroup(ctx, chatID, "Go v2.2 test group", now); err != nil {
		t.Fatalf("ensure group: %v", err)
	}
	user := User{ID: userID, Username: "go22", DisplayName: "Go 2.2"}
	if err := store.TouchUser(ctx, chatID, user, now); err != nil {
		t.Fatalf("touch user: %v", err)
	}
	if err := store.SetGroupOwner(ctx, chatID, user, now); err != nil {
		t.Fatalf("set owner: %v", err)
	}
	ok, err := store.IsOperator(ctx, chatID, userID)
	if err != nil {
		t.Fatalf("is operator: %v", err)
	}
	if !ok {
		t.Fatalf("owner should also be operator")
	}

	group, err := store.GetGroup(ctx, chatID)
	if err != nil {
		t.Fatalf("get group: %v", err)
	}
	if group.OwnerUserID != userID {
		t.Fatalf("owner mismatch: got %d want %d", group.OwnerUserID, userID)
	}

	recordID, err := store.InsertRecord(ctx, Record{
		ChatID:          chatID,
		DayKey:          "2026-07-06",
		Kind:            "deposit",
		Currency:        "CNY",
		Amount:          "100",
		Rate:            "7",
		FeeRate:         "0",
		ResultUSDT:      "14.29",
		ActorUserID:     userID,
		ActorName:       user.DisplayName,
		SourceMessageID: 1001,
		Remark:          "integration",
		CreatedAt:       now,
	})
	if err != nil {
		t.Fatalf("insert record: %v", err)
	}
	if recordID == 0 {
		t.Fatalf("record id should be non-zero")
	}

	address := "TGhAAySHUUcEGua33pZZ88wP3bA6X5eQuZ"
	if err := store.AddWatch(ctx, userID, address, "watch address", now); err != nil {
		t.Fatalf("add watch: %v", err)
	}
	targets, err := store.ListWatchTargets(ctx)
	if err != nil {
		t.Fatalf("list watch targets: %v", err)
	}
	found := false
	for _, target := range targets {
		if target.OwnerUserID == userID && target.Address == address {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("watch target was not returned")
	}

	inserted, err := store.RecordChainNotification(ctx, userID, address, "txhash-"+time.Now().Format("150405.000000000"), "income", now.UnixMilli(), now)
	if err != nil {
		t.Fatalf("record chain notification: %v", err)
	}
	if !inserted {
		t.Fatalf("first chain notification should insert")
	}
}
