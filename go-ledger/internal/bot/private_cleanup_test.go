package bot

import (
	"testing"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
)

func TestPrivateCleanupMessageSchedule(t *testing.T) {
	now := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	category, seconds, dueAt := privateCleanupMessageSchedule(storage.PrivateChatMessage{
		Direction: "outgoing",
		Category:  "bot_prompt",
		CreatedAt: now,
	}, storage.PrivateCleanupSettings{BotDeleteAfter: 300})
	if category != "bot_prompt" || seconds != 300 || dueAt == nil || !dueAt.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("unexpected outgoing schedule: category=%q seconds=%d dueAt=%v", category, seconds, dueAt)
	}

	category, seconds, dueAt = privateCleanupMessageSchedule(storage.PrivateChatMessage{
		Direction: "incoming",
		CreatedAt: now,
	}, storage.PrivateCleanupSettings{IncomingDeleteAfter: 45})
	if category != "private" || seconds != 45 || dueAt == nil || !dueAt.Equal(now.Add(45*time.Second)) {
		t.Fatalf("unexpected incoming schedule: category=%q seconds=%d dueAt=%v", category, seconds, dueAt)
	}

	_, seconds, dueAt = privateCleanupMessageSchedule(storage.PrivateChatMessage{
		Direction: "outgoing",
		CreatedAt: now,
	}, storage.PrivateCleanupSettings{})
	if seconds != 0 || dueAt != nil {
		t.Fatalf("message without delay should wait for daily cleanup only: seconds=%d dueAt=%v", seconds, dueAt)
	}
}
