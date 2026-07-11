package bot

import (
	"context"
	"testing"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/telegram"
)

func TestGroupAccountingActiveRequiresCurrentBusinessDay(t *testing.T) {
	loc := time.FixedZone("Asia/Shanghai", 8*3600)
	group := storage.Group{
		Active:       true,
		ActiveDayKey: "2026-07-09",
		CutoffHour:   0,
	}
	if !groupAccountingActive(group, time.Date(2026, 7, 9, 23, 59, 59, 0, loc)) {
		t.Fatal("group should stay active before midnight cutoff")
	}
	if groupAccountingActive(group, time.Date(2026, 7, 10, 0, 0, 0, 0, loc)) {
		t.Fatal("group should require a new start after midnight cutoff")
	}
}

func TestGroupAccountingActiveUsesCutoffHour(t *testing.T) {
	loc := time.FixedZone("Asia/Shanghai", 8*3600)
	group := storage.Group{
		Active:       true,
		ActiveDayKey: "2026-07-09",
		CutoffHour:   4,
	}
	if !groupAccountingActive(group, time.Date(2026, 7, 10, 3, 59, 59, 0, loc)) {
		t.Fatal("group should stay active until its configured cutoff hour")
	}
	if groupAccountingActive(group, time.Date(2026, 7, 10, 4, 0, 0, 0, loc)) {
		t.Fatal("group should expire at its configured cutoff hour")
	}
}

func TestGroupAccountingActiveRequiresActiveDayKey(t *testing.T) {
	loc := time.FixedZone("Asia/Shanghai", 8*3600)
	group := storage.Group{
		Active:     true,
		CutoffHour: 0,
	}
	if groupAccountingActive(group, time.Date(2026, 7, 9, 12, 0, 0, 0, loc)) {
		t.Fatal("legacy active groups without active_day_key should require start")
	}
}

func TestOpenBillQueryDoesNotRequireWritePermissionWhenAccountingActive(t *testing.T) {
	loc := time.FixedZone("Asia/Shanghai", 8*3600)
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, loc)
	group := storage.Group{
		ChatID:       -1001,
		Active:       true,
		ActiveDayKey: businessDayKey(now, 0),
		CutoffHour:   0,
	}
	msg := telegram.Message{Chat: telegram.Chat{ID: group.ChatID}, MessageID: 10}
	user := storage.User{ID: 2002}

	ok, err := (&Bot{}).guardAccountingStarted(context.Background(), msg, user, group, now, "ledger_zero_inactive")
	if err != nil {
		t.Fatalf("guardAccountingStarted returned error: %v", err)
	}
	if !ok {
		t.Fatal("open bill query should be allowed without write permission when accounting is active")
	}
}
