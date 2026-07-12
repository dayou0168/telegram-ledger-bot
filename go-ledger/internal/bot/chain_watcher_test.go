package bot

import (
	"errors"
	"testing"
	"time"
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
	controller.recordFailure("ready", now.Add(3*time.Second))
	if mode := controller.snapshot(now.Add(time.Hour)).Mode; mode != fallbackModeActive {
		t.Fatalf("long-running fallback mode = %s, want active", mode)
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
