package bot

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/telegram"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/worker"
)

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

func (b *Bot) admitUpdateBatch(ctx context.Context, updates []telegram.Update, offset *int64) bool {
	if len(updates) == 0 {
		return true
	}
	startOffset := *offset
	nextOffset := startOffset
	for _, lane := range []updateAdmissionLane{updateAdmissionLedger, updateAdmissionBypass} {
		for _, update := range updates {
			if update.UpdateID < startOffset {
				continue
			}
			key, pool := b.updateRoute(update)
			updateLane := updateAdmissionLedger
			if pool == b.queryPool {
				updateLane = updateAdmissionBypass
			}
			if updateLane != lane {
				continue
			}
			u := update
			queuedAt := time.Now()
			item := updateAdmissionJob{
				key:      key,
				executor: pool,
				job: func(jobCtx context.Context) {
					b.handleAdmittedUpdate(jobCtx, u, key, updateLane, queuedAt)
				},
			}
			if !b.updateAdmission.Submit(ctx, updateLane, item) {
				return false
			}
			if update.UpdateID >= nextOffset {
				nextOffset = update.UpdateID + 1
			}
		}
	}
	*offset = nextOffset
	return true
}

func (b *Bot) handleAdmittedUpdate(ctx context.Context, update telegram.Update, key string, lane updateAdmissionLane, queuedAt time.Time) {
	trace := newPerfTrace(update.UpdateID, updateChatID(update))
	traceCtx := contextWithPerfTrace(ctx, trace)
	addPerfStage(traceCtx, "queue_wait", time.Since(queuedAt))
	dispatcher := b.updateAdmission.ledger
	if lane == updateAdmissionBypass {
		dispatcher = b.updateAdmission.bypass
	}
	markPerfQueue(traceCtx, key, dispatcher.Depth(key))

	for attempt := 1; ; attempt++ {
		done := measurePerfStage(traceCtx, "db_claim_update")
		claimed, err := b.store.ClaimUpdate(traceCtx, update.UpdateID, time.Now().In(b.loc))
		done()
		if err == nil {
			if claimed {
				if err := b.handleUpdate(traceCtx, update); err != nil {
					log.Printf("handle update %d: %v", update.UpdateID, err)
				}
			}
			finishPerfTrace(trace, b.cfg.SlowUpdateThreshold)
			return
		}
		if attempt == 1 || attempt%10 == 0 {
			log.Printf("claim update %d attempt %d: %v", update.UpdateID, attempt, err)
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (b *Bot) UpdateAdmissionStats() any {
	if b == nil || b.updateAdmission == nil {
		return nil
	}
	return b.updateAdmission.Stats()
}
