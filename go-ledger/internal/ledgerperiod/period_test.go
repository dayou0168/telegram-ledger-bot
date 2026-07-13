package ledgerperiod

import (
	"testing"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
)

func TestCurrentDayKeyUsesContinuousActivePeriodWhenCutoffDisabled(t *testing.T) {
	loc := time.FixedZone("Asia/Shanghai", 8*3600)
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, loc)
	group := storage.Group{Active: true, ActiveDayKey: "2026-07-01", CutoffHour: CutoffDisabledHour}
	if !AccountingActive(group, now) {
		t.Fatal("disabled cutoff active period should survive date changes")
	}
	if got := CurrentDayKey(group, now); got != "2026-07-01" {
		t.Fatalf("CurrentDayKey = %q, want continuous period key", got)
	}
}

func TestAccountingActiveExpiresAtConfiguredCutoff(t *testing.T) {
	loc := time.FixedZone("Asia/Shanghai", 8*3600)
	group := storage.Group{Active: true, ActiveDayKey: "2026-07-12", ActiveExpiresDayKey: "2026-07-12", CutoffHour: 4}
	if !AccountingActive(group, time.Date(2026, 7, 13, 3, 59, 59, 0, loc)) {
		t.Fatal("period should remain active before cutoff")
	}
	if AccountingActive(group, time.Date(2026, 7, 13, 4, 0, 0, 0, loc)) {
		t.Fatal("period should expire at cutoff")
	}
}

func TestHistoricalCutoffZeroStillMeansMidnight(t *testing.T) {
	loc := time.FixedZone("Asia/Shanghai", 8*3600)
	before := time.Date(2026, 7, 12, 23, 59, 0, 0, loc)
	after := time.Date(2026, 7, 13, 0, 0, 0, 0, loc)
	group := storage.Group{Active: true, ActiveDayKey: "2026-07-12", ActiveExpiresDayKey: "2026-07-12", CutoffHour: 0}
	if !AccountingActive(group, before) || AccountingActive(group, after) {
		t.Fatal("cutoff_hour=0 must retain midnight-cutoff semantics")
	}
}
