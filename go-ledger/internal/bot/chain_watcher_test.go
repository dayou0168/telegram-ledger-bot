package bot

import (
	"testing"
	"time"
)

func TestWatcherFallbackControllerFailureRecoveryAndMaxActive(t *testing.T) {
	now := time.Unix(1000, 0)
	controller := newWatcherFallbackController(2, 10*time.Second)

	if controller.recordFailure(now) {
		t.Fatal("first failure enabled fallback")
	}
	if !controller.recordFailure(now.Add(time.Second)) {
		t.Fatal("second failure did not enable fallback")
	}
	if !controller.active(now.Add(2 * time.Second)) {
		t.Fatal("fallback should be active before max active duration")
	}
	if controller.active(now.Add(11 * time.Second)) {
		t.Fatal("fallback should stop after max active duration")
	}
	if controller.recordFailure(now.Add(12 * time.Second)) {
		t.Fatal("fallback re-enabled before watcher recovery")
	}
	if controller.recordSuccess(now.Add(13 * time.Second)) {
		t.Fatal("first success should not reset exhausted fallback")
	}
	if controller.recordSuccess(now.Add(14 * time.Second)) {
		t.Fatal("second success should not reset exhausted fallback")
	}
	if !controller.recordSuccess(now.Add(15 * time.Second)) {
		t.Fatal("third success should reset exhausted fallback")
	}
	if controller.recordFailure(now.Add(16 * time.Second)) {
		t.Fatal("first failure after recovery enabled fallback")
	}
	if !controller.recordFailure(now.Add(17 * time.Second)) {
		t.Fatal("second failure after recovery did not enable fallback")
	}
}
