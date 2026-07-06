package bot

import (
	"context"
	"sync"
	"time"
)

type telegramRateLimiter struct {
	mu             sync.Mutex
	nextGlobal     time.Time
	nextByChat     map[int64]time.Time
	globalInterval time.Duration
	chatInterval   time.Duration
}

func newTelegramRateLimiter() *telegramRateLimiter {
	return &telegramRateLimiter{
		nextByChat:     make(map[int64]time.Time),
		globalInterval: 35 * time.Millisecond,
		chatInterval:   300 * time.Millisecond,
	}
}

func (l *telegramRateLimiter) Wait(ctx context.Context, chatID int64) error {
	for {
		wait := l.reserveWait(chatID)
		if wait <= 0 {
			return nil
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (l *telegramRateLimiter) reserveWait(chatID int64) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	readyAt := l.nextGlobal
	if chatReadyAt := l.nextByChat[chatID]; chatReadyAt.After(readyAt) {
		readyAt = chatReadyAt
	}
	if readyAt.After(now) {
		return readyAt.Sub(now)
	}
	l.nextGlobal = now.Add(l.globalInterval)
	l.nextByChat[chatID] = now.Add(l.chatInterval)
	return 0
}
