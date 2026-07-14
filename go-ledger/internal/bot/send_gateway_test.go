package bot

import (
	"context"
	"errors"
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
	limiter.chatInterval = time.Second

	if wait := limiter.reserveWait(1001); wait != 0 {
		t.Fatalf("first reservation wait = %v, want 0", wait)
	}
	if wait := limiter.reserveWait(1001); wait <= 0 {
		t.Fatalf("second same-chat reservation wait = %v, want >0", wait)
	}
}
