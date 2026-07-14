package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/telegram"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/worker"
)

const (
	telegramInboxClaimLimit  = 32
	telegramInboxMaxAttempts = 8
	telegramInboxLease       = 2 * time.Minute
	telegramInboxDoneKeep    = 72 * time.Hour
)

type durableTelegramPayload struct {
	Version      int             `json:"version"`
	Update       telegram.Update `json:"update"`
	PrivateState *privateState   `json:"private_state,omitempty"`
}

type updateAdmissionLane int

const (
	updateAdmissionLedger updateAdmissionLane = iota
	updateAdmissionBypass
)

type updateAdmissionJob struct {
	key      string
	executor worker.Executor
	job      worker.Job
}

type UpdateAdmissionStats struct {
	Ledger            worker.DispatcherStats `json:"ledger"`
	Bypass            worker.DispatcherStats `json:"bypass"`
	BypassOverflow    int                    `json:"bypass_overflow"`
	OverflowCapacity  int                    `json:"overflow_capacity"`
	BackpressureCount uint64                 `json:"backpressure_count"`
}

type updateAdmission struct {
	ledger *worker.Dispatcher
	bypass *worker.Dispatcher

	mu               sync.Mutex
	bypassOverflow   []updateAdmissionJob
	overflowCapacity int
	wake             chan struct{}
	space            chan struct{}
	wg               sync.WaitGroup
	backpressure     atomic.Uint64
}

func newUpdateAdmission(queueSize int) *updateAdmission {
	if queueSize < 1 {
		queueSize = 1
	}
	return &updateAdmission{
		ledger:           worker.NewDispatcher(queueSize),
		bypass:           worker.NewDispatcher(queueSize),
		overflowCapacity: queueSize,
		wake:             make(chan struct{}, 1),
		space:            make(chan struct{}, 1),
	}
}

func (a *updateAdmission) Start(ctx context.Context) {
	if a == nil {
		return
	}
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.runBypassOverflow(ctx)
	}()
}

func (a *updateAdmission) Wait() {
	if a != nil {
		a.wg.Wait()
	}
}

func (a *updateAdmission) Submit(ctx context.Context, lane updateAdmissionLane, item updateAdmissionJob) bool {
	if a == nil || item.executor == nil || item.job == nil {
		return false
	}
	if lane == updateAdmissionLedger {
		return a.ledger.Submit(ctx, item.key, item.executor, item.job)
	}
	if a.bypass.TrySubmit(ctx, item.key, item.executor, item.job) {
		return true
	}
	for {
		a.mu.Lock()
		if len(a.bypassOverflow) < a.overflowCapacity {
			a.bypassOverflow = append(a.bypassOverflow, item)
			a.mu.Unlock()
			a.signal(a.wake)
			return true
		}
		a.mu.Unlock()
		a.backpressure.Add(1)
		select {
		case <-ctx.Done():
			return false
		case <-a.space:
			if a.bypass.TrySubmit(ctx, item.key, item.executor, item.job) {
				return true
			}
		}
	}
}

func (a *updateAdmission) Stats() UpdateAdmissionStats {
	if a == nil {
		return UpdateAdmissionStats{}
	}
	a.mu.Lock()
	overflow := len(a.bypassOverflow)
	a.mu.Unlock()
	return UpdateAdmissionStats{
		Ledger:            a.ledger.Stats(),
		Bypass:            a.bypass.Stats(),
		BypassOverflow:    overflow,
		OverflowCapacity:  a.overflowCapacity,
		BackpressureCount: a.backpressure.Load(),
	}
}

func (a *updateAdmission) runBypassOverflow(ctx context.Context) {
	for {
		item, ok := a.peekBypassOverflow()
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-a.wake:
			}
			continue
		}
		if a.bypass.TrySubmit(ctx, item.key, item.executor, item.job) {
			a.popBypassOverflow()
			continue
		}
		timer := time.NewTimer(2 * time.Millisecond)
		select {
		case <-ctx.Done():
			stopAdmissionTimer(timer)
			return
		case <-a.bypass.Ready():
			stopAdmissionTimer(timer)
		case <-a.wake:
			stopAdmissionTimer(timer)
		case <-timer.C:
		}
	}
}

func stopAdmissionTimer(timer *time.Timer) {
	if timer == nil || timer.Stop() {
		return
	}
	select {
	case <-timer.C:
	default:
	}
}

func (a *updateAdmission) peekBypassOverflow() (updateAdmissionJob, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.bypassOverflow) == 0 {
		return updateAdmissionJob{}, false
	}
	return a.bypassOverflow[0], true
}

func (a *updateAdmission) popBypassOverflow() {
	a.mu.Lock()
	if len(a.bypassOverflow) > 0 {
		copy(a.bypassOverflow, a.bypassOverflow[1:])
		a.bypassOverflow = a.bypassOverflow[:len(a.bypassOverflow)-1]
	}
	a.mu.Unlock()
	a.signal(a.space)
}

func (a *updateAdmission) signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (b *Bot) persistTelegramUpdateBatch(ctx context.Context, updates []telegram.Update) (int64, error) {
	now := time.Now().In(b.loc)
	items := make([]storage.TelegramInboxUpdate, 0, len(updates))
	for _, update := range updates {
		durable := durableTelegramPayload{Version: 1, Update: update}
		if userID := telegramUpdatePrivateUserID(update); userID > 0 {
			if state, ok := b.privateStates.Get(formatID(userID)); ok {
				stateCopy := state
				durable.PrivateState = &stateCopy
			}
		}
		payload, err := json.Marshal(durable)
		if err != nil {
			return 0, fmt.Errorf("marshal update %d: %w", update.UpdateID, err)
		}
		key, pool := b.updateRoute(update)
		lane := "ledger"
		if pool == b.queryPool {
			lane = "bypass"
		}
		items = append(items, storage.TelegramInboxUpdate{
			UpdateID: update.UpdateID,
			Payload:  payload,
			Lane:     lane,
			RouteKey: key,
		})
	}
	return b.store.PersistTelegramUpdateBatch(ctx, b.telegramInboxStreamKey(), items, now)
}

func (b *Bot) startTelegramInbox(ctx context.Context) {
	owner := fmt.Sprintf("%s:%d", b.telegramInboxStreamKey(), time.Now().UnixNano())
	go b.runTelegramInboxLane(ctx, "ledger", owner, b.telegramLedgerWake)
	go b.runTelegramInboxLane(ctx, "bypass", owner, b.telegramBypassWake)
	go b.telegramInboxMaintenance(ctx)
}

func (b *Bot) wakeTelegramInbox() {
	for _, wake := range []chan struct{}{b.telegramLedgerWake, b.telegramBypassWake} {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}

func (b *Bot) runTelegramInboxLane(ctx context.Context, lane, owner string, wake <-chan struct{}) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for ctx.Err() == nil {
		claimed, err := b.store.ClaimTelegramUpdates(ctx, b.telegramInboxStreamKey(), lane, owner,
			telegramInboxClaimLimit, telegramInboxMaxAttempts, telegramInboxLease, time.Now().In(b.loc))
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("claim telegram inbox lane=%s: %v", lane, err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}
		queueStops := make([]chan struct{}, len(claimed))
		for index, item := range claimed {
			queueStops[index] = make(chan struct{})
			go b.renewTelegramInboxLease(ctx, item, owner, queueStops[index])
		}
		for index, item := range claimed {
			if !b.submitTelegramInboxItem(ctx, item, owner, queueStops[index]) {
				close(queueStops[index])
				for remaining := index + 1; remaining < len(queueStops); remaining++ {
					close(queueStops[remaining])
				}
				return
			}
		}
		if len(claimed) == telegramInboxClaimLimit {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-ticker.C:
		}
	}
}

func (b *Bot) submitTelegramInboxItem(ctx context.Context, item storage.TelegramInboxUpdate, owner string, queueStop chan struct{}) bool {
	var durable durableTelegramPayload
	if err := json.Unmarshal(item.Payload, &durable); err != nil {
		_, retryErr := b.store.RetryTelegramUpdate(ctx, item.StreamKey, item.UpdateID, owner,
			telegramInboxMaxAttempts, time.Now().In(b.loc), time.Now().In(b.loc), fmt.Errorf("decode inbox payload: %w", err))
		if retryErr != nil {
			log.Printf("dead-letter malformed telegram update %d: %v", item.UpdateID, retryErr)
		}
		close(queueStop)
		return true
	}
	update := durable.Update
	if durable.Version == 0 {
		if err := json.Unmarshal(item.Payload, &update); err != nil {
			_, _ = b.store.RetryTelegramUpdate(ctx, item.StreamKey, item.UpdateID, owner,
				telegramInboxMaxAttempts, time.Now().In(b.loc), time.Now().In(b.loc), fmt.Errorf("decode legacy inbox payload: %w", err))
			close(queueStop)
			return true
		}
	}
	lane := updateAdmissionLedger
	executor := worker.Executor(b.ledgerPool)
	if item.Lane == "bypass" {
		lane = updateAdmissionBypass
		executor = b.queryPool
	} else if len(item.RouteKey) >= len("private:") && item.RouteKey[:len("private:")] == "private:" {
		executor = b.controlPool
	} else if len(item.RouteKey) >= len("update:") && item.RouteKey[:len("update:")] == "update:" {
		executor = b.controlPool
	}
	queuedAt := time.Now()
	return b.updateAdmission.Submit(ctx, lane, updateAdmissionJob{
		key:      item.RouteKey,
		executor: executor,
		job: func(jobCtx context.Context) {
			close(queueStop)
			b.handleAdmittedUpdateWithState(jobCtx, item, update, durable.PrivateState, owner, lane, queuedAt)
		},
	})
}

func telegramUpdatePrivateUserID(update telegram.Update) int64 {
	if update.Message != nil && update.Message.Chat.Type == "private" && update.Message.From != nil {
		return update.Message.From.ID
	}
	if update.CallbackQuery != nil && (update.CallbackQuery.Message == nil || update.CallbackQuery.Message.Chat.Type == "private") {
		return update.CallbackQuery.From.ID
	}
	return 0
}

func (b *Bot) handleAdmittedUpdateWithState(ctx context.Context, item storage.TelegramInboxUpdate, update telegram.Update, state *privateState, owner string, lane updateAdmissionLane, queuedAt time.Time) {
	if state != nil {
		if userID := telegramUpdatePrivateUserID(update); userID > 0 {
			b.privateStates.Set(formatID(userID), *state)
		}
	}
	b.handleAdmittedUpdate(ctx, item, update, owner, lane, queuedAt)
}

func (b *Bot) handleAdmittedUpdate(ctx context.Context, item storage.TelegramInboxUpdate, update telegram.Update, owner string, lane updateAdmissionLane, queuedAt time.Time) {
	trace := newPerfTrace(update.UpdateID, updateChatID(update))
	traceCtx := contextWithPerfTrace(ctx, trace)
	addPerfStage(traceCtx, "queue_wait", time.Since(queuedAt))
	dispatcher := b.updateAdmission.ledger
	if lane == updateAdmissionBypass {
		dispatcher = b.updateAdmission.bypass
	}
	markPerfQueue(traceCtx, item.RouteKey, dispatcher.Depth(item.RouteKey))

	if item.HandledAt == nil {
		stopRenew := make(chan struct{})
		go b.renewTelegramInboxLease(traceCtx, item, owner, stopRenew)
		handleCtx := context.WithValue(traceCtx, telegramUpdateReceivedAtContextKey{}, item.CreatedAt.In(b.loc))
		handleErr := func() error {
			defer close(stopRenew)
			return b.executeUpdate(handleCtx, update)
		}()
		if handleErr != nil {
			retryAt := time.Now().In(b.loc).Add(telegramInboxRetryDelay(item.Attempts))
			status, retryErr := b.store.RetryTelegramUpdate(traceCtx, item.StreamKey, item.UpdateID, owner,
				telegramInboxMaxAttempts, retryAt, time.Now().In(b.loc), handleErr)
			if retryErr != nil {
				log.Printf("retry telegram update %d after handler error %v: %v", item.UpdateID, handleErr, retryErr)
			} else {
				log.Printf("telegram update %d handler failed status=%s attempt=%d: %v", item.UpdateID, status, item.Attempts, handleErr)
			}
			finishPerfTrace(trace, b.cfg.SlowUpdateThreshold)
			return
		}
		if !b.persistTelegramHandled(traceCtx, item, owner) {
			finishPerfTrace(trace, b.cfg.SlowUpdateThreshold)
			return
		}
	}
	b.persistTelegramDone(traceCtx, item, owner)
	finishPerfTrace(trace, b.cfg.SlowUpdateThreshold)
}

func (b *Bot) renewTelegramInboxLease(ctx context.Context, item storage.TelegramInboxUpdate, owner string, stop <-chan struct{}) {
	ticker := time.NewTicker(telegramInboxLease / 3)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case now := <-ticker.C:
			ok, err := b.store.RenewTelegramUpdateLease(ctx, item.StreamKey, item.UpdateID, owner, telegramInboxLease, now.In(b.loc))
			if err != nil {
				log.Printf("renew telegram update %d lease: %v", item.UpdateID, err)
			} else if !ok {
				return
			}
		}
	}
}

func (b *Bot) persistTelegramHandled(ctx context.Context, item storage.TelegramInboxUpdate, owner string) bool {
	for attempt := 1; ctx.Err() == nil; attempt++ {
		ok, err := b.store.MarkTelegramUpdateHandled(ctx, item.StreamKey, item.UpdateID, owner, time.Now().In(b.loc))
		if err == nil {
			return ok
		}
		if attempt == 1 || attempt%10 == 0 {
			log.Printf("mark telegram update %d handled attempt=%d: %v", item.UpdateID, attempt, err)
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(100 * time.Millisecond):
		}
	}
	return false
}

func (b *Bot) persistTelegramDone(ctx context.Context, item storage.TelegramInboxUpdate, owner string) {
	for attempt := 1; ctx.Err() == nil; attempt++ {
		ok, err := b.store.CompleteTelegramUpdate(ctx, item.StreamKey, item.UpdateID, owner, time.Now().In(b.loc))
		if err == nil {
			if !ok {
				log.Printf("telegram update %d completion lost lease", item.UpdateID)
			}
			return
		}
		if attempt == 1 || attempt%10 == 0 {
			log.Printf("complete telegram update %d attempt=%d: %v", item.UpdateID, attempt, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func telegramInboxRetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 8 {
		attempt = 8
	}
	delay := 250 * time.Millisecond * time.Duration(1<<uint(attempt-1))
	if delay > 30*time.Second {
		return 30 * time.Second
	}
	return delay
}

func (b *Bot) telegramInboxMaintenance(ctx context.Context) {
	statsTicker := time.NewTicker(5 * time.Second)
	cleanupTicker := time.NewTicker(time.Hour)
	defer statsTicker.Stop()
	defer cleanupTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-statsTicker.C:
			stats, err := b.store.TelegramInboxStats(ctx, b.telegramInboxStreamKey(), time.Now().In(b.loc))
			if err != nil {
				log.Printf("telegram inbox stats: %v", err)
			} else {
				b.telegramInboxStats.Store(stats)
			}
		case now := <-cleanupTicker.C:
			for {
				removed, err := b.store.CleanupDoneTelegramUpdates(ctx, b.telegramInboxStreamKey(), now.Add(-telegramInboxDoneKeep), 2000)
				if err != nil {
					log.Printf("cleanup telegram inbox: %v", err)
					break
				}
				if removed < 2000 {
					break
				}
			}
		}
	}
}

func (b *Bot) UpdateAdmissionStats() any {
	if b == nil || b.updateAdmission == nil {
		return nil
	}
	return struct {
		Memory UpdateAdmissionStats `json:"memory"`
		Inbox  any                  `json:"inbox,omitempty"`
	}{Memory: b.updateAdmission.Stats(), Inbox: b.telegramInboxStats.Load()}
}
