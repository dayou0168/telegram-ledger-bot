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
