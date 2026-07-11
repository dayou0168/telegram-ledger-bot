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
	wait := l.reserveWait(chatID)
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
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
	slotAt := now
	if readyAt.After(now) {
		slotAt = readyAt
	}
	l.nextGlobal = slotAt.Add(l.globalInterval)
	l.nextByChat[chatID] = slotAt.Add(l.chatInterval)
	return slotAt.Sub(now)
}
