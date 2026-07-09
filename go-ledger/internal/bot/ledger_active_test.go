package bot

import (
	"testing"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
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
