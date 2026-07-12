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
		Active:              true,
		ActiveDayKey:        "2026-07-09",
		ActiveExpiresDayKey: "2026-07-09",
		CutoffHour:          0,
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
		Active:              true,
		ActiveDayKey:        "2026-07-09",
		ActiveExpiresDayKey: "2026-07-09",
		CutoffHour:          4,
	}
	if !groupAccountingActive(group, time.Date(2026, 7, 10, 3, 59, 59, 0, loc)) {
		t.Fatal("group should stay active until its configured cutoff hour")
	}
	if groupAccountingActive(group, time.Date(2026, 7, 10, 4, 0, 0, 0, loc)) {
		t.Fatal("group should expire at its configured cutoff hour")
	}
}

func TestGroupAccountingActiveDisabledCutoffSurvivesMidnight(t *testing.T) {
	loc := time.FixedZone("Asia/Shanghai", 8*3600)
	group := storage.Group{
		Active:       true,
		ActiveDayKey: "2026-07-09",
		CutoffHour:   cutoffDisabledHour,
	}
	nextDay := time.Date(2026, 7, 10, 12, 0, 0, 0, loc)
	if !groupAccountingActive(group, nextDay) {
		t.Fatal("disabled cutoff should keep an active ledger period across midnight")
	}
	if got := currentLedgerDayKey(group, nextDay); got != "2026-07-09" {
		t.Fatalf("currentLedgerDayKey = %q, want original active period", got)
	}
}

func TestDisabledCutoffDoesNotStartInactiveGroup(t *testing.T) {
	loc := time.FixedZone("Asia/Shanghai", 8*3600)
	group := storage.Group{
		Active:     false,
		CutoffHour: cutoffDisabledHour,
	}
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, loc)
	active, activeDayKey, activeExpiresDayKey := cutoffStateAfterSetting(group, cutoffDisabledHour, now)
	if active || activeDayKey != "" || activeExpiresDayKey != "" {
		t.Fatalf("inactive group should stay inactive after disabling cutoff: %v %q %q", active, activeDayKey, activeExpiresDayKey)
	}
	if groupAccountingActive(group, now) {
		t.Fatal("inactive group with disabled cutoff should not become active")
	}
}

func TestDisabledCutoffManualStopStaysStoppedAcrossMidnight(t *testing.T) {
	loc := time.FixedZone("Asia/Shanghai", 8*3600)
	group := storage.Group{
		Active:       false,
		ActiveDayKey: "",
		CutoffHour:   cutoffDisabledHour,
	}
	if groupAccountingActive(group, time.Date(2026, 7, 10, 12, 0, 0, 0, loc)) {
		t.Fatal("manual stop should keep disabled-cutoff group stopped")
	}
}

func TestDisabledCutoffRestartStateRestoresActivePeriod(t *testing.T) {
	loc := time.FixedZone("Asia/Shanghai", 8*3600)
	group := storage.Group{
		Active:       true,
		ActiveDayKey: "2026-07-09",
		CutoffHour:   cutoffDisabledHour,
	}
	now := time.Date(2026, 7, 12, 8, 0, 0, 0, loc)
	if !groupAccountingActive(group, now) {
		t.Fatal("persisted disabled-cutoff active state should restore after restart")
	}
	if got := currentLedgerDayKey(group, now); got != "2026-07-09" {
		t.Fatalf("restored current ledger day = %q", got)
	}
}

func TestReenabledCutoffKeepsCurrentPeriodUntilNextBoundary(t *testing.T) {
	loc := time.FixedZone("Asia/Shanghai", 8*3600)
	now := time.Date(2026, 7, 10, 10, 0, 0, 0, loc)
	group := storage.Group{
		Active:       true,
		ActiveDayKey: "2026-07-09",
		CutoffHour:   cutoffDisabledHour,
	}
	active, activeDayKey, activeExpiresDayKey := cutoffStateAfterSetting(group, 4, now)
	if !active || activeDayKey != "2026-07-09" || activeExpiresDayKey != "2026-07-10" {
		t.Fatalf("reenable cutoff state = %v %q %q", active, activeDayKey, activeExpiresDayKey)
	}
	reenabled := storage.Group{
		Active:              active,
		ActiveDayKey:        activeDayKey,
		ActiveExpiresDayKey: activeExpiresDayKey,
		CutoffHour:          4,
	}
	if !groupAccountingActive(reenabled, time.Date(2026, 7, 11, 3, 59, 59, 0, loc)) {
		t.Fatal("reenabled cutoff should keep current period before next cutoff boundary")
	}
	if groupAccountingActive(reenabled, time.Date(2026, 7, 11, 4, 0, 0, 0, loc)) {
		t.Fatal("reenabled cutoff should expire at the next configured boundary")
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
		ChatID:              -1001,
		Active:              true,
		ActiveDayKey:        businessDayKey(now, 0),
		ActiveExpiresDayKey: businessDayKey(now, 0),
		CutoffHour:          0,
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
