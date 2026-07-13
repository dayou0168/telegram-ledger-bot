package bot

import (
	"errors"
	"testing"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/config"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
)

func TestWatcherFallbackControllerStateMachineAndRecovery(t *testing.T) {
	now := time.Unix(1000, 0)
	controller := newWatcherFallbackControllerWithRecovery(3, 2, 5*time.Second)

	controller.recordFailure("ready", now)
	if mode := controller.snapshot(now).Mode; mode != fallbackModePending {
		t.Fatalf("first failure mode = %s, want pending", mode)
	}
	controller.recordFailure("claim", now.Add(time.Second))
	if mode := controller.snapshot(now.Add(time.Second)).Mode; mode != fallbackModePending {
		t.Fatalf("second failure mode = %s, want pending", mode)
	}
	controller.recordFailure("ready", now.Add(2*time.Second))
	controller.recordFailure("ready", now.Add(3*time.Second))
	state := controller.snapshot(now.Add(3 * time.Second))
	if state.Mode != fallbackModePending || !state.LeaseRequested {
		t.Fatalf("fallback before lease = %+v, want pending lease request", state)
	}
	controller.activateLease(now.Add(3 * time.Second))
	if mode := controller.snapshot(now.Add(time.Hour)).Mode; mode != fallbackModeActive {
		t.Fatalf("fallback after lease = %s, want active", mode)
	}

	controller.recordSuccess("ready", now.Add(time.Hour+time.Second), time.Second)
	if mode := controller.snapshot(now.Add(12 * time.Second)).Mode; mode != fallbackModeRecovery {
		t.Fatalf("first recovery mode = %s, want recovering", mode)
	}
	controller.recordSuccess("ready", now.Add(time.Hour+2*time.Second), time.Second)
	controller.recordSuccess("claim", now.Add(time.Hour+3*time.Second), time.Second)
	controller.recordSuccess("ready", now.Add(time.Hour+4*time.Second), 10*time.Second)
	if controller.recordSuccess("claim", now.Add(time.Hour+4*time.Second), 0) {
		t.Fatal("high watcher lag completed recovery")
	}
	if !controller.recordSuccess("ready", now.Add(time.Hour+5*time.Second), time.Second) {
		t.Fatal("ready/claim successes with low lag did not complete recovery")
	}
	if mode := controller.snapshot(now.Add(time.Hour + 5*time.Second)).Mode; mode != fallbackModePrimary {
		t.Fatalf("recovered mode = %s, want primary", mode)
	}
}

func TestSharedSubscriptionPreservesBaselineAndDisablesUnsupportedTRX(t *testing.T) {
	bot := &Bot{cfg: config.Config{ChainWatcherBotID: "bot-a"}}
	sub := bot.sharedSubscription(storage.WatchTarget{
		OwnerUserID: 10, Address: "TAddress", WatchIncome: true, NotifyTRX: true, BaselineTimestamp: 1234,
	})
	if sub.BotID != "bot-a" || sub.ChatID != 10 || sub.BaselineTimestamp != 1234 || sub.NotifyTRX {
		t.Fatalf("shared subscription = %+v", sub)
	}
}

func TestFallbackPollBackoffStepsAndRecovers(t *testing.T) {
	bot := &Bot{}
	var previous time.Duration
	for i, want := range []time.Duration{time.Second, 2 * time.Second, 3 * time.Second, 5 * time.Second, 10 * time.Second} {
		before := time.Now()
		bot.recordFallbackPollResult(errors.New("429"))
		delay := time.Until(time.Unix(0, bot.fallbackNextPoll.Load()))
		if delay < want-100*time.Millisecond || delay > want+100*time.Millisecond {
			t.Fatalf("step %d delay = %v, want %v", i, delay, want)
		}
		if time.Since(before) > 100*time.Millisecond {
			t.Fatal("backoff calculation blocked")
		}
		previous = delay
	}
	bot.recordFallbackPollResult(nil)
	delay := time.Until(time.Unix(0, bot.fallbackNextPoll.Load()))
	if delay >= previous {
		t.Fatalf("successful fallback poll did not reduce backoff: %v >= %v", delay, previous)
	}
}

func TestWatcherFallbackControllerEmptySuccessfulClaimsDoNotFail(t *testing.T) {
	now := time.Unix(2000, 0)
	controller := newWatcherFallbackControllerWithRecovery(3, 2, 5*time.Second)
	for i := 0; i < 10; i++ {
		controller.recordSuccess("claim", now.Add(time.Duration(i)*time.Second), 0)
	}
	if mode := controller.snapshot(now.Add(10 * time.Second)).Mode; mode != fallbackModePrimary {
		t.Fatalf("empty successful claims mode = %s, want primary", mode)
	}
}

func TestWatcherFallbackReadyAndClaimFailuresRecoverIndependently(t *testing.T) {
	now := time.Unix(3000, 0)
	controller := newWatcherFallbackControllerWithRecovery(3, 2, 5*time.Second)
	controller.recordFailure("claim", now)
	controller.recordFailure("claim", now.Add(2*time.Second))
	controller.recordFailure("claim", now.Add(3*time.Second))
	if !controller.snapshot(now.Add(3 * time.Second)).LeaseRequested {
		t.Fatal("claim failures did not request fallback lease")
	}
	controller.recordSuccess("ready", now.Add(4*time.Second), 0)
	state := controller.snapshot(now.Add(4 * time.Second))
	if state.Mode != fallbackModePending || !state.LeaseRequested {
		t.Fatalf("ready success incorrectly cleared claim failure: %+v", state)
	}
	controller.recordSuccess("claim", now.Add(5*time.Second), 0)
	if state := controller.snapshot(now.Add(5 * time.Second)); state.Mode != fallbackModePrimary {
		t.Fatalf("both sources recovered state = %+v", state)
	}
}
