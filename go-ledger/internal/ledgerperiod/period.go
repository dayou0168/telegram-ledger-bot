package ledgerperiod

import (
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
)

// CutoffDisabledHour is the explicit disabled sentinel. Existing zero values
// are indistinguishable from intentional midnight cutoffs and must not be
// inferred or bulk-migrated to this value.
const CutoffDisabledHour = -1

func BusinessDayKey(now time.Time, cutoffHour int) string {
	if cutoffHour < 0 || cutoffHour > 23 {
		cutoffHour = 0
	}
	return now.Add(-time.Duration(cutoffHour) * time.Hour).Format("2006-01-02")
}

func AccountingActive(group storage.Group, now time.Time) bool {
	if !group.Active || group.ActiveDayKey == "" {
		return false
	}
	if group.CutoffHour == CutoffDisabledHour {
		return true
	}
	expiresDayKey := group.ActiveExpiresDayKey
	if expiresDayKey == "" {
		expiresDayKey = group.ActiveDayKey
	}
	return expiresDayKey == BusinessDayKey(now, group.CutoffHour)
}

func ExpiresDayKey(now time.Time, cutoffHour int) string {
	if cutoffHour == CutoffDisabledHour {
		return ""
	}
	return BusinessDayKey(now, cutoffHour)
}

func StartDayKey(group storage.Group, now time.Time) string {
	return BusinessDayKey(now, group.CutoffHour)
}

func ResumeDayKey(group storage.Group, now time.Time) string {
	if periodCanResume(group, now) {
		return group.ActiveDayKey
	}
	return StartDayKey(group, now)
}

func periodCanResume(group storage.Group, now time.Time) bool {
	if group.ActiveDayKey == "" || group.ActivePeriodStartedAt.IsZero() || !group.ActivePeriodStartedAt.After(time.Unix(0, 0)) {
		return false
	}
	if group.CutoffHour == CutoffDisabledHour {
		return true
	}
	expiresDayKey := group.ActiveExpiresDayKey
	if expiresDayKey == "" {
		expiresDayKey = group.ActiveDayKey
	}
	return expiresDayKey == BusinessDayKey(now, group.CutoffHour)
}

func CurrentDayKey(group storage.Group, now time.Time) string {
	if AccountingActive(group, now) {
		return group.ActiveDayKey
	}
	return BusinessDayKey(now, group.CutoffHour)
}

func StateAfterCutoffSetting(group storage.Group, cutoffHour int, now time.Time) (bool, string, string) {
	if AccountingActive(group, now) {
		return true, CurrentDayKey(group, now), ExpiresDayKey(now, cutoffHour)
	}
	if !group.Active && periodCanResume(group, now) {
		return false, group.ActiveDayKey, ExpiresDayKey(now, cutoffHour)
	}
	return false, "", ""
}
