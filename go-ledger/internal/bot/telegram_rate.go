package bot

import (
	"context"
	"sync"
	"time"
)

const (
	telegramRateFairnessBurst       = 16
	telegramRateNormalFairnessBurst = 4
)

type telegramRateWaiter struct {
	ctx      context.Context
	priority sendPriority
	chatID   int64
	sequence uint64
}

type telegramRateLimiter struct {
	mu sync.Mutex

	nextGlobal     time.Time
	nextBulk       time.Time
	nextByChat     map[int64]time.Time
	globalInterval time.Duration
	chatInterval   time.Duration
	bulkInterval   time.Duration

	waiters       []*telegramRateWaiter
	notify        chan struct{}
	nextSequence  uint64
	criticalBurst int
	normalBurst   int
}

func newTelegramRateLimiter() *telegramRateLimiter {
	return &telegramRateLimiter{
		nextByChat:     make(map[int64]time.Time),
		globalInterval: 35 * time.Millisecond,
		chatInterval:   300 * time.Millisecond,
		bulkInterval:   100 * time.Millisecond,
		notify:         make(chan struct{}),
	}
}

func (l *telegramRateLimiter) Wait(ctx context.Context, priority sendPriority, chatID int64) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	waiter := &telegramRateWaiter{ctx: ctx, priority: priority.normalized(), chatID: chatID}
	l.mu.Lock()
	l.nextSequence++
	waiter.sequence = l.nextSequence
	l.waiters = append(l.waiters, waiter)
	l.signalLocked()
	l.mu.Unlock()

	for {
		l.mu.Lock()
		now := time.Now()
		selected, readyAt := l.selectWaiterLocked(now)
		notify := l.notify
		if selected == waiter && !readyAt.After(now) {
			l.removeWaiterLocked(waiter)
			l.nextGlobal = now.Add(l.globalInterval)
			l.nextByChat[chatID] = now.Add(l.chatInterval)
			if waiter.priority == sendPriorityBulk {
				l.nextBulk = now.Add(l.bulkInterval)
			}
			if waiter.priority == sendPriorityCritical {
				l.criticalBurst++
			} else {
				l.criticalBurst = 0
			}
			if waiter.priority == sendPriorityNormal {
				l.normalBurst++
			} else if waiter.priority == sendPriorityBulk {
				l.normalBurst = 0
			}
			l.signalLocked()
			l.mu.Unlock()
			return nil
		}
		wait := time.Duration(-1)
		if selected == waiter {
			wait = readyAt.Sub(now)
		}
		l.mu.Unlock()

		if wait >= 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				stopRateTimer(timer)
				l.cancelWaiter(waiter)
				return ctx.Err()
			case <-notify:
				stopRateTimer(timer)
			case <-timer.C:
			}
			continue
		}
		select {
		case <-ctx.Done():
			l.cancelWaiter(waiter)
			return ctx.Err()
		case <-notify:
		}
	}
}

func (l *telegramRateLimiter) selectWaiterLocked(now time.Time) (*telegramRateWaiter, time.Time) {
	l.pruneCanceledLocked()
	if len(l.waiters) == 0 {
		return nil, time.Time{}
	}
	globalReady := now
	if l.nextGlobal.After(globalReady) {
		globalReady = l.nextGlobal
	}

	var eligible []*telegramRateWaiter
	for _, waiter := range l.waiters {
		if !l.waiterReadyAtLocked(waiter, globalReady).After(globalReady) {
			eligible = append(eligible, waiter)
		}
	}
	if len(eligible) > 0 {
		selected := l.selectEligibleLocked(eligible)
		return selected, globalReady
	}

	selected := l.waiters[0]
	selectedAt := l.waiterReadyAtLocked(selected, globalReady)
	for _, waiter := range l.waiters[1:] {
		readyAt := l.waiterReadyAtLocked(waiter, globalReady)
		if readyAt.Before(selectedAt) || (readyAt.Equal(selectedAt) && rateWaiterBefore(waiter, selected)) {
			selected = waiter
			selectedAt = readyAt
		}
	}
	return selected, selectedAt
}

func (l *telegramRateLimiter) selectEligibleLocked(waiters []*telegramRateWaiter) *telegramRateWaiter {
	if l.criticalBurst >= telegramRateFairnessBurst {
		fair := l.selectNonCriticalLocked(waiters)
		if fair != nil {
			return fair
		}
	}
	if l.normalBurst >= telegramRateNormalFairnessBurst {
		if bulk := oldestRateWaiter(waiters, sendPriorityBulk); bulk != nil {
			return bulk
		}
	}
	selected := waiters[0]
	for _, waiter := range waiters[1:] {
		if rateWaiterBefore(waiter, selected) {
			selected = waiter
		}
	}
	return selected
}

func (l *telegramRateLimiter) selectNonCriticalLocked(waiters []*telegramRateWaiter) *telegramRateWaiter {
	if l.normalBurst >= telegramRateNormalFairnessBurst {
		if bulk := oldestRateWaiter(waiters, sendPriorityBulk); bulk != nil {
			return bulk
		}
	}
	if normal := oldestRateWaiter(waiters, sendPriorityNormal); normal != nil {
		return normal
	}
	return oldestRateWaiter(waiters, sendPriorityBulk)
}

func oldestRateWaiter(waiters []*telegramRateWaiter, priority sendPriority) *telegramRateWaiter {
	var oldest *telegramRateWaiter
	for _, waiter := range waiters {
		if waiter.priority != priority {
			continue
		}
		if oldest == nil || waiter.sequence < oldest.sequence {
			oldest = waiter
		}
	}
	return oldest
}

func rateWaiterBefore(left, right *telegramRateWaiter) bool {
	if left.priority != right.priority {
		return left.priority < right.priority
	}
	return left.sequence < right.sequence
}

func (l *telegramRateLimiter) waiterReadyAtLocked(waiter *telegramRateWaiter, globalReady time.Time) time.Time {
	readyAt := globalReady
	if chatReady := l.nextByChat[waiter.chatID]; chatReady.After(readyAt) {
		readyAt = chatReady
	}
	if waiter.priority == sendPriorityBulk && l.nextBulk.After(readyAt) {
		readyAt = l.nextBulk
	}
	return readyAt
}

func (l *telegramRateLimiter) cancelWaiter(waiter *telegramRateWaiter) {
	l.mu.Lock()
	l.removeWaiterLocked(waiter)
	l.signalLocked()
	l.mu.Unlock()
}

func (l *telegramRateLimiter) pruneCanceledLocked() {
	kept := l.waiters[:0]
	changed := false
	for _, waiter := range l.waiters {
		if waiter.ctx.Err() != nil {
			changed = true
			continue
		}
		kept = append(kept, waiter)
	}
	l.waiters = kept
	if changed {
		l.signalLocked()
	}
}

func (l *telegramRateLimiter) removeWaiterLocked(target *telegramRateWaiter) {
	for i, waiter := range l.waiters {
		if waiter != target {
			continue
		}
		copy(l.waiters[i:], l.waiters[i+1:])
		l.waiters = l.waiters[:len(l.waiters)-1]
		return
	}
}

func (l *telegramRateLimiter) signalLocked() {
	close(l.notify)
	l.notify = make(chan struct{})
}

func stopRateTimer(timer *time.Timer) {
	if timer == nil || timer.Stop() {
		return
	}
	select {
	case <-timer.C:
	default:
	}
}
