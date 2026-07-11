package bot

import (
	"testing"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/telegram"
)

func TestNotificationRetryDelayUsesTelegramRetryAfter(t *testing.T) {
	delay := notificationRetryDelay(1, &telegram.Error{ErrorCode: 429, RetryAfter: 4})
	if delay != 5*time.Second {
		t.Fatalf("delay = %v, want 5s", delay)
	}
}

func TestNotificationRetryDelayBackoff(t *testing.T) {
	if delay := notificationRetryDelay(3, errTest("network")); delay != 15*time.Second {
		t.Fatalf("delay = %v, want 15s", delay)
	}
}

func TestChainOutboxPriorityUsesCriticalLane(t *testing.T) {
	if priority := outboxSendPriority(0); priority != sendPriorityCritical {
		t.Fatalf("priority = %s, want critical", priority)
	}
}

type errTest string

func (e errTest) Error() string { return string(e) }
