package bot

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/telegram"
)

func TestSendGatewayBulkDoesNotBlockCritical(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gateway := newTelegramSendGateway(nil, nil, 3, 8)
	gateway.retryBaseDelay = time.Millisecond
	gateway.Start(ctx)

	bulkStarted := make(chan struct{})
	releaseBulk := make(chan struct{})
	bulkDone := make(chan struct{})
	go func() {
		_, _ = gateway.Do(ctx, sendPriorityBulk, -1001, func(context.Context) (telegram.Message, error) {
			close(bulkStarted)
			<-releaseBulk
			return telegram.Message{MessageID: 1}, nil
		})
		close(bulkDone)
	}()

	select {
	case <-bulkStarted:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("bulk request did not start")
	}

	criticalDone := make(chan error, 1)
	go func() {
		msg, err := gateway.Do(ctx, sendPriorityCritical, -1002, func(context.Context) (telegram.Message, error) {
			return telegram.Message{MessageID: 2}, nil
		})
		if err == nil && msg.MessageID != 2 {
			err = errors.New("unexpected critical message id")
		}
		criticalDone <- err
	}()

	select {
	case err := <-criticalDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("critical request was blocked by bulk lane")
	}

	normalDone := make(chan error, 1)
	go func() {
		msg, err := gateway.Do(ctx, sendPriorityNormal, -1003, func(context.Context) (telegram.Message, error) {
			return telegram.Message{MessageID: 3}, nil
		})
		if err == nil && msg.MessageID != 3 {
			err = errors.New("unexpected normal message id")
		}
		normalDone <- err
	}()

	select {
	case err := <-normalDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("normal request was blocked by bulk lane")
	}

	close(releaseBulk)
	select {
	case <-bulkDone:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("bulk request did not finish")
	}
}

func TestSendGatewayRetries5xxAndSucceeds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gateway := newTelegramSendGateway(nil, nil, 3, 8)
	gateway.retryBaseDelay = time.Millisecond
	gateway.retryMaxDelay = time.Millisecond
	gateway.Start(ctx)

	attempts := 0
	msg, err := gateway.Do(ctx, sendPriorityCritical, -1001, func(context.Context) (telegram.Message, error) {
		attempts++
		if attempts == 1 {
			return telegram.Message{}, &telegram.Error{Endpoint: "sendMessage", ErrorCode: 500, Description: "server error"}
		}
		return telegram.Message{MessageID: 9}, nil
	})
	if err != nil {
		t.Fatalf("gateway send failed: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if msg.MessageID != 9 {
		t.Fatalf("message id = %d, want 9", msg.MessageID)
	}
}

func TestSendGatewayRetriesNetworkErrorAndSucceeds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gateway := newTelegramSendGateway(nil, nil, 3, 8)
	gateway.retryBaseDelay = time.Millisecond
	gateway.retryMaxDelay = time.Millisecond
	gateway.Start(ctx)
	attempts := 0
	msg, err := gateway.Do(ctx, sendPriorityCritical, -1001, func(context.Context) (telegram.Message, error) {
		attempts++
		if attempts == 1 {
			return telegram.Message{}, errors.New("connection reset by peer")
		}
		return telegram.Message{MessageID: 10}, nil
	})
	if err != nil || attempts != 2 || msg.MessageID != 10 {
		t.Fatalf("network retry = message %d attempts %d error %v", msg.MessageID, attempts, err)
	}
}

func TestSendGatewayReportsMetrics(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gateway := newTelegramSendGateway(nil, nil, 3, 8)
	gateway.Start(ctx)

	msg, err, metrics := gateway.DoWithMetrics(ctx, sendPriorityCritical, -1001, func(context.Context) (telegram.Message, error) {
		time.Sleep(time.Millisecond)
		return telegram.Message{MessageID: 12}, nil
	})
	if err != nil {
		t.Fatalf("gateway send failed: %v", err)
	}
	if msg.MessageID != 12 {
		t.Fatalf("message id = %d, want 12", msg.MessageID)
	}
	if metrics.Priority != sendPriorityCritical {
		t.Fatalf("priority = %s, want critical", metrics.Priority)
	}
	if metrics.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", metrics.Attempts)
	}
	if metrics.OperationDuration <= 0 {
		t.Fatalf("operation duration = %v, want >0", metrics.OperationDuration)
	}
	if metrics.TotalDuration <= 0 {
		t.Fatalf("total duration = %v, want >0", metrics.TotalDuration)
	}
	if metrics.CompletedAt.Before(metrics.StartedAt) {
		t.Fatalf("completed_at %v before started_at %v", metrics.CompletedAt, metrics.StartedAt)
	}
}

func TestSendGatewayStatsTracksQueuedBulk(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gateway := newTelegramSendGateway(nil, nil, 3, 8)
	gateway.Start(ctx)

	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan struct{})
	go func() {
		_, _ = gateway.Do(ctx, sendPriorityBulk, -1001, func(context.Context) (telegram.Message, error) {
			close(firstStarted)
			<-releaseFirst
			return telegram.Message{MessageID: 1}, nil
		})
		close(firstDone)
	}()
	select {
	case <-firstStarted:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("first bulk request did not start")
	}

	secondStarted := make(chan struct{})
	secondDone := make(chan struct{})
	go func() {
		_, _ = gateway.Do(ctx, sendPriorityBulk, -1002, func(context.Context) (telegram.Message, error) {
			close(secondStarted)
			return telegram.Message{MessageID: 2}, nil
		})
		close(secondDone)
	}()

	deadline := time.After(100 * time.Millisecond)
	for {
		stats := gateway.Stats(time.Now().Add(10 * time.Millisecond))
		if stats.Bulk.Queued == 1 && stats.Bulk.OldestQueuedAgeMS > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("bulk stats did not show queued request: %+v", gateway.Stats(time.Now()))
		default:
			time.Sleep(time.Millisecond)
		}
	}

	close(releaseFirst)
	select {
	case <-secondStarted:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("second bulk request did not start after first release")
	}
	select {
	case <-firstDone:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("first bulk request did not finish")
	}
	select {
	case <-secondDone:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("second bulk request did not finish")
	}
}

func TestSendGatewayRetries5xxAndFailsAfterLimit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gateway := newTelegramSendGateway(nil, nil, 3, 8)
	gateway.maxAttempts = 2
	gateway.retryBaseDelay = time.Millisecond
	gateway.retryMaxDelay = time.Millisecond
	gateway.Start(ctx)

	attempts := 0
	_, err := gateway.Do(ctx, sendPriorityCritical, -1001, func(context.Context) (telegram.Message, error) {
		attempts++
		return telegram.Message{}, &telegram.Error{Endpoint: "sendMessage", ErrorCode: 502, Description: "bad gateway"}
	})
	if err == nil {
		t.Fatal("gateway send should fail after retry limit")
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func TestSendGatewayRetryAfterDelay(t *testing.T) {
	delay := telegramGatewayRetryDelay(1, &telegram.Error{Endpoint: "sendMessage", ErrorCode: 429, RetryAfter: 4}, time.Millisecond, time.Second)
	if delay != 5*time.Second {
		t.Fatalf("retry_after delay = %v, want 5s", delay)
	}
}

func TestTelegramRateLimiterPerChat(t *testing.T) {
	limiter := newTelegramRateLimiter()
	limiter.globalInterval = 0
	limiter.chatInterval = 30 * time.Millisecond
	limiter.bulkInterval = 0

	if err := limiter.Wait(context.Background(), sendPriorityNormal, 1001); err != nil {
		t.Fatalf("first reservation: %v", err)
	}
	started := time.Now()
	if err := limiter.Wait(context.Background(), sendPriorityCritical, 1001); err != nil {
		t.Fatalf("second reservation: %v", err)
	}
	if elapsed := time.Since(started); elapsed < 20*time.Millisecond {
		t.Fatalf("same-chat reservation elapsed = %v, want >=20ms", elapsed)
	}
}

func TestSendGatewayRealLimiterBulkCannotReserveAheadOfCritical(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	limiter := newTelegramRateLimiter()
	limiter.globalInterval = 20 * time.Millisecond
	limiter.chatInterval = 0
	limiter.bulkInterval = 30 * time.Millisecond
	gateway := newTelegramSendGateway(nil, limiter, 3, 32)
	gateway.Start(ctx)

	if _, err := gateway.Do(ctx, sendPriorityNormal, 1, func(context.Context) (telegram.Message, error) {
		return telegram.Message{MessageID: 1}, nil
	}); err != nil {
		t.Fatalf("seed global limiter: %v", err)
	}

	var mu sync.Mutex
	order := make([]string, 0, 8)
	record := func(value string) telegramSendOperation {
		return func(context.Context) (telegram.Message, error) {
			mu.Lock()
			order = append(order, value)
			mu.Unlock()
			return telegram.Message{MessageID: 2}, nil
		}
	}
	bulkDone := make(chan error, 6)
	for i := 0; i < 6; i++ {
		value := i
		go func() {
			_, err := gateway.Do(ctx, sendPriorityBulk, int64(100+value), record("bulk"))
			bulkDone <- err
		}()
	}
	waitForLimiterWaiters(t, limiter, sendPriorityBulk, 1)

	criticalStarted := time.Now()
	criticalDone := make(chan error, 1)
	go func() {
		_, err := gateway.Do(ctx, sendPriorityCritical, 999, record("critical"))
		criticalDone <- err
	}()
	waitForLimiterWaiters(t, limiter, sendPriorityCritical, 1)
	normalDone := make(chan error, 1)
	go func() {
		_, err := gateway.Do(ctx, sendPriorityNormal, 998, record("normal"))
		normalDone <- err
	}()

	if err := <-criticalDone; err != nil {
		t.Fatalf("critical send: %v", err)
	}
	if elapsed := time.Since(criticalStarted); elapsed > 100*time.Millisecond {
		t.Fatalf("critical limiter latency = %v, want <=100ms", elapsed)
	}
	if err := <-normalDone; err != nil {
		t.Fatalf("normal send: %v", err)
	}
	for i := 0; i < 6; i++ {
		if err := <-bulkDone; err != nil {
			t.Fatalf("bulk send %d: %v", i, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 8 || order[0] != "critical" {
		t.Fatalf("limiter order = %v, want critical first and all lanes complete", order)
	}
}

func TestTelegramRateLimiterCancellationRemovesWaiter(t *testing.T) {
	limiter := newTelegramRateLimiter()
	limiter.mu.Lock()
	limiter.nextGlobal = time.Now().Add(time.Second)
	limiter.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- limiter.Wait(ctx, sendPriorityBulk, 42)
	}()
	waitForLimiterWaiters(t, limiter, sendPriorityBulk, 1)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled limiter wait = %v", err)
	}
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	if len(limiter.waiters) != 0 {
		t.Fatalf("limiter retained %d canceled waiters", len(limiter.waiters))
	}
}

func TestTelegramRateLimiterFairnessAfterCriticalBurst(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	limiter := newTelegramRateLimiter()
	limiter.globalInterval = time.Millisecond
	limiter.chatInterval = 0
	limiter.bulkInterval = 0
	limiter.mu.Lock()
	limiter.nextGlobal = time.Now().Add(30 * time.Millisecond)
	limiter.criticalBurst = telegramRateFairnessBurst
	limiter.mu.Unlock()
	order := make(chan string, 2)
	go func() {
		if err := limiter.Wait(ctx, sendPriorityBulk, 1); err == nil {
			order <- "bulk"
		}
	}()
	waitForLimiterWaiters(t, limiter, sendPriorityBulk, 1)
	go func() {
		if err := limiter.Wait(ctx, sendPriorityCritical, 2); err == nil {
			order <- "critical"
		}
	}()
	waitForLimiterWaiters(t, limiter, sendPriorityCritical, 1)
	if first := <-order; first != "bulk" {
		t.Fatalf("fairness slot = %s, want bulk after critical burst", first)
	}
	if second := <-order; second != "critical" {
		t.Fatalf("post-fairness slot = %s, want critical", second)
	}
}

func TestTelegramRateLimiterSustainedMixMakesEveryLaneProgress(t *testing.T) {
	limiter := newTelegramRateLimiter()
	limiter.globalInterval = 0
	limiter.chatInterval = 0
	limiter.bulkInterval = 0
	sequence := uint64(0)
	add := func(priority sendPriority, count int, chatBase int64) {
		for i := 0; i < count; i++ {
			sequence++
			limiter.waiters = append(limiter.waiters, &telegramRateWaiter{
				ctx: context.Background(), priority: priority, chatID: chatBase + int64(i), sequence: sequence,
			})
		}
	}
	add(sendPriorityCritical, 64, 1000)
	add(sendPriorityNormal, 16, 2000)
	add(sendPriorityBulk, 16, 3000)

	seen := map[sendPriority]int{}
	firstNormal := -1
	firstBulk := -1
	for slot := 0; len(limiter.waiters) > 0; slot++ {
		selected, readyAt := limiter.selectWaiterLocked(time.Now())
		if selected == nil || readyAt.After(time.Now().Add(time.Millisecond)) {
			t.Fatalf("slot %d selected=%v ready_at=%v", slot, selected, readyAt)
		}
		seen[selected.priority]++
		if selected.priority == sendPriorityNormal && firstNormal < 0 {
			firstNormal = slot
		}
		if selected.priority == sendPriorityBulk && firstBulk < 0 {
			firstBulk = slot
		}
		limiter.removeWaiterLocked(selected)
		if selected.priority == sendPriorityCritical {
			limiter.criticalBurst++
		} else {
			limiter.criticalBurst = 0
		}
		if selected.priority == sendPriorityNormal {
			limiter.normalBurst++
		} else if selected.priority == sendPriorityBulk {
			limiter.normalBurst = 0
		}
	}
	if firstNormal > telegramRateFairnessBurst || firstNormal < 0 {
		t.Fatalf("normal first slot = %d, bound=%d", firstNormal, telegramRateFairnessBurst)
	}
	if firstBulk < 0 || seen[sendPriorityBulk] != 16 || seen[sendPriorityNormal] != 16 || seen[sendPriorityCritical] != 64 {
		t.Fatalf("sustained limiter progress=%v first_bulk=%d", seen, firstBulk)
	}
	bulkBound := (telegramRateFairnessBurst + 1) * (telegramRateNormalFairnessBurst + 1)
	if firstBulk > bulkBound {
		t.Fatalf("bulk first slot=%d bound=%d", firstBulk, bulkBound)
	}
}

func waitForLimiterWaiters(t *testing.T, limiter *telegramRateLimiter, priority sendPriority, count int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		limiter.mu.Lock()
		found := 0
		for _, waiter := range limiter.waiters {
			if waiter.priority == priority {
				found++
			}
		}
		limiter.mu.Unlock()
		if found >= count {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("limiter did not queue %d %s waiters", count, priority)
}
