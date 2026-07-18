package bot

import (
	"context"
	"encoding/json"
	"errors"
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
	Version            int             `json:"version"`
	Update             telegram.Update `json:"update"`
	LegacyPrivateState *privateState   `json:"private_state,omitempty"`
}

type telegramPrivateStateRevision struct {
	userID          int64
	expectedVersion int64
}

type telegramUpdateCommitSignal struct {
	done       chan struct{}
	committed  atomic.Bool
	once       sync.Once
	mu         sync.Mutex
	quickReply *storage.QuickReplyOutboxInsert
}

type telegramUpdateCommitSignalContextKey struct{}

type telegramInboxLeaseGuard struct {
	b                  *Bot
	item               storage.TelegramInboxUpdate
	owner              string
	lease              time.Duration
	legacyPrivateState *privateState
	stop               chan struct{}
	lost               chan struct{}
	stopOnce           sync.Once
	lostOnce           sync.Once
	lostFlag           atomic.Bool

	deadlineMu sync.Mutex
	deadline   time.Time
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
		if err := b.observeTelegramUpdateIdentityAtIngress(ctx, update, now); err != nil {
			return 0, fmt.Errorf("observe update %d identity: %w", update.UpdateID, err)
		}
		durable := durableTelegramPayload{Version: 1, Update: update}
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
		lease := b.telegramInboxLeaseDuration()
		claimed, err := b.store.ClaimTelegramUpdates(ctx, b.telegramInboxStreamKey(), lane, owner,
			telegramInboxClaimLimit, telegramInboxMaxAttempts, lease, time.Now().In(b.loc))
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
		guards := make([]*telegramInboxLeaseGuard, len(claimed))
		for index, item := range claimed {
			guards[index] = b.startTelegramInboxLeaseGuard(ctx, item, owner, lease)
		}
		for index, item := range claimed {
			if !b.submitTelegramInboxItem(ctx, item, owner, guards[index]) {
				guards[index].Stop()
				for remaining := index + 1; remaining < len(guards); remaining++ {
					guards[remaining].Stop()
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

func (b *Bot) submitTelegramInboxItem(ctx context.Context, item storage.TelegramInboxUpdate, owner string, guard *telegramInboxLeaseGuard) bool {
	var durable durableTelegramPayload
	if err := json.Unmarshal(item.Payload, &durable); err != nil {
		b.persistTelegramRetry(ctx, item, owner, guard, fmt.Errorf("decode inbox payload: %w", err))
		guard.Stop()
		return true
	}
	update := durable.Update
	if durable.Version == 0 {
		if err := json.Unmarshal(item.Payload, &update); err != nil {
			b.persistTelegramRetry(ctx, item, owner, guard, fmt.Errorf("decode legacy inbox payload: %w", err))
			guard.Stop()
			return true
		}
	}
	guard.legacyPrivateState = durable.LegacyPrivateState
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
			b.handleAdmittedUpdate(jobCtx, item, update, owner, lane, queuedAt, guard)
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

func (b *Bot) handleAdmittedUpdate(ctx context.Context, item storage.TelegramInboxUpdate, update telegram.Update, owner string, lane updateAdmissionLane, queuedAt time.Time, guards ...*telegramInboxLeaseGuard) {
	var guard *telegramInboxLeaseGuard
	if len(guards) > 0 {
		guard = guards[0]
	}
	if guard == nil {
		guard = b.startTelegramInboxLeaseGuard(ctx, item, owner, b.telegramInboxLeaseDuration())
	}
	defer guard.Stop()
	trace := newPerfTrace(update.UpdateID, updateChatID(update))
	traceCtx := contextWithPerfTrace(ctx, trace)
	addPerfStage(traceCtx, "queue_wait", time.Since(queuedAt))
	dispatcher := b.updateAdmission.ledger
	if lane == updateAdmissionBypass {
		dispatcher = b.updateAdmission.bypass
	}
	markPerfQueue(traceCtx, item.RouteKey, dispatcher.Depth(item.RouteKey))

	if guard.Lost() {
		finishPerfTrace(trace, b.cfg.SlowUpdateThreshold)
		return
	}
	if item.HandledAt == nil {
		revision, err := b.restoreTelegramPrivateState(traceCtx, item, update, guard.legacyPrivateState)
		if err != nil {
			b.persistTelegramRetry(traceCtx, item, owner, guard, err)
			finishPerfTrace(trace, b.cfg.SlowUpdateThreshold)
			return
		}
		commitSignal := newTelegramUpdateCommitSignal()
		handleCtx := context.WithValue(traceCtx, telegramUpdateReceivedAtContextKey{}, item.CreatedAt.In(b.loc))
		handleCtx = context.WithValue(handleCtx, telegramUpdateCommitSignalContextKey{}, commitSignal)
		handleCtx, cancelHandle := context.WithCancel(handleCtx)
		cancelWatchDone := make(chan struct{})
		go func() {
			select {
			case <-guard.LostChannel():
				cancelHandle()
			case <-cancelWatchDone:
			}
		}()
		handleErr := b.executeUpdate(handleCtx, update)
		close(cancelWatchDone)
		cancelHandle()
		if handleErr != nil {
			commitSignal.Abort()
			b.persistTelegramRetry(traceCtx, item, owner, guard, handleErr)
			finishPerfTrace(trace, b.cfg.SlowUpdateThreshold)
			return
		}
		if guard.Lost() || !b.persistTelegramHandled(traceCtx, item, owner, guard, revision, commitSignal) {
			commitSignal.Abort()
			finishPerfTrace(trace, b.cfg.SlowUpdateThreshold)
			return
		}
		commitSignal.Commit()
		if commitSignal.HasQuickReply() {
			b.kickQuickReplyOutbox()
		}
	}
	b.persistTelegramDone(traceCtx, item, owner, guard)
	finishPerfTrace(trace, b.cfg.SlowUpdateThreshold)
}

func newTelegramUpdateCommitSignal() *telegramUpdateCommitSignal {
	return &telegramUpdateCommitSignal{done: make(chan struct{})}
}

func (s *telegramUpdateCommitSignal) Commit() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		s.committed.Store(true)
		close(s.done)
	})
}

func (s *telegramUpdateCommitSignal) Abort() {
	if s == nil {
		return
	}
	s.once.Do(func() { close(s.done) })
}

func (s *telegramUpdateCommitSignal) Wait(ctx context.Context) bool {
	if s == nil {
		return true
	}
	select {
	case <-ctx.Done():
		return false
	case <-s.done:
		return s.committed.Load()
	}
}

func (s *telegramUpdateCommitSignal) StageQuickReply(input storage.QuickReplyOutboxInsert) error {
	if s == nil {
		return errors.New("telegram update commit signal is unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.quickReply != nil {
		if *s.quickReply == input {
			return nil
		}
		return errors.New("telegram update already staged a different quick reply")
	}
	value := input
	s.quickReply = &value
	return nil
}

func (s *telegramUpdateCommitSignal) QuickReply() *storage.QuickReplyOutboxInsert {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.quickReply == nil {
		return nil
	}
	value := *s.quickReply
	return &value
}

func (s *telegramUpdateCommitSignal) HasQuickReply() bool {
	return s.QuickReply() != nil
}

func telegramUpdateCommitSignalFromContext(ctx context.Context) *telegramUpdateCommitSignal {
	signal, _ := ctx.Value(telegramUpdateCommitSignalContextKey{}).(*telegramUpdateCommitSignal)
	return signal
}

func (b *Bot) telegramInboxLeaseDuration() time.Duration {
	if b.telegramInboxLease > 0 {
		return b.telegramInboxLease
	}
	return telegramInboxLease
}

func (b *Bot) startTelegramInboxLeaseGuard(ctx context.Context, item storage.TelegramInboxUpdate, owner string, lease time.Duration) *telegramInboxLeaseGuard {
	if lease <= 0 {
		lease = telegramInboxLease
	}
	deadline := time.Now().Add(lease)
	if item.LeaseUntil != nil {
		deadline = *item.LeaseUntil
	}
	guard := &telegramInboxLeaseGuard{
		b: b, item: item, owner: owner, lease: lease,
		stop: make(chan struct{}), lost: make(chan struct{}), deadline: deadline,
	}
	go guard.run(ctx)
	return guard
}

func (g *telegramInboxLeaseGuard) run(ctx context.Context) {
	nextDelay := g.lease / 5
	if nextDelay < 5*time.Millisecond {
		nextDelay = 5 * time.Millisecond
	}
	retryDelay := g.lease / 10
	if retryDelay < 2*time.Millisecond {
		retryDelay = 2 * time.Millisecond
	}
	for {
		deadline := g.Deadline()
		now := time.Now()
		if !deadline.After(now) {
			g.MarkLost()
			return
		}
		wait := nextDelay
		if remaining := time.Until(deadline); wait > remaining {
			wait = remaining
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			stopAdmissionTimer(timer)
			return
		case <-g.stop:
			stopAdmissionTimer(timer)
			return
		case <-timer.C:
		}
		deadline = g.Deadline()
		remaining := time.Until(deadline)
		if remaining <= 0 {
			g.MarkLost()
			return
		}
		callTimeout := g.lease / 2
		if callTimeout < 5*time.Millisecond {
			callTimeout = 5 * time.Millisecond
		}
		safetyMargin := g.lease / 10
		if safetyMargin < 2*time.Millisecond {
			safetyMargin = 2 * time.Millisecond
		}
		if available := remaining - safetyMargin; callTimeout > available {
			callTimeout = available
		}
		if callTimeout <= 0 {
			g.MarkLost()
			return
		}
		renewCtx, cancel := context.WithTimeout(ctx, callTimeout)
		renewedAt := time.Now().In(g.b.loc)
		ok, err := g.b.store.RenewTelegramUpdateLease(renewCtx, g.item.StreamKey, g.item.UpdateID, g.owner, g.lease, renewedAt)
		cancel()
		if err != nil {
			if time.Now().After(g.Deadline()) {
				g.MarkLost()
				return
			}
			log.Printf("renew telegram update %d lease: %v", g.item.UpdateID, err)
			nextDelay = retryDelay
			continue
		}
		if !ok {
			g.MarkLost()
			return
		}
		g.SetDeadline(renewedAt.Add(g.lease))
		nextDelay = g.lease / 5
		if nextDelay < 5*time.Millisecond {
			nextDelay = 5 * time.Millisecond
		}
	}
}

func (g *telegramInboxLeaseGuard) Stop() {
	g.stopOnce.Do(func() { close(g.stop) })
}

func (g *telegramInboxLeaseGuard) MarkLost() {
	g.lostFlag.Store(true)
	g.lostOnce.Do(func() { close(g.lost) })
}

func (g *telegramInboxLeaseGuard) Lost() bool {
	return g == nil || g.lostFlag.Load()
}

func (g *telegramInboxLeaseGuard) LostChannel() <-chan struct{} {
	return g.lost
}

func (g *telegramInboxLeaseGuard) Deadline() time.Time {
	g.deadlineMu.Lock()
	defer g.deadlineMu.Unlock()
	return g.deadline
}

func (g *telegramInboxLeaseGuard) SetDeadline(value time.Time) {
	g.deadlineMu.Lock()
	g.deadline = value
	g.deadlineMu.Unlock()
}

func (b *Bot) restoreTelegramPrivateState(ctx context.Context, item storage.TelegramInboxUpdate, update telegram.Update, legacyState *privateState) (*telegramPrivateStateRevision, error) {
	userID := telegramUpdatePrivateUserID(update)
	if userID <= 0 {
		return nil, nil
	}
	state, found, err := b.store.GetTelegramPrivateRouteState(ctx, item.StreamKey, userID)
	if err != nil {
		return nil, err
	}
	key := formatID(userID)
	stateFresh := found && state.HasState && privateStateIsFresh(state.UpdatedAt, time.Now())
	if stateFresh {
		var value privateState
		if err := json.Unmarshal(state.StateJSON, &value); err != nil {
			return nil, fmt.Errorf("decode private route state user=%d: %w", userID, err)
		}
		value, err = b.upgradeLegacyBroadcastState(ctx, userID, value)
		if err != nil {
			return nil, err
		}
		b.privateStates.Set(key, value)
	} else if !found && legacyState != nil {
		b.privateStates.Set(key, *legacyState)
	} else {
		broadcastState, targetFound, targetErr := b.loadBroadcastTargetState(ctx, userID)
		if targetErr != nil {
			return nil, targetErr
		}
		if targetFound {
			b.privateStates.Set(key, broadcastState)
		} else {
			b.privateStates.Delete(key)
		}
	}
	expected := int64(-1)
	if found {
		expected = state.VersionUpdateID
	}
	return &telegramPrivateStateRevision{userID: userID, expectedVersion: expected}, nil
}

func privateStateIsFresh(updatedAt, now time.Time) bool {
	return updatedAt.IsZero() || now.Sub(updatedAt) < privateStateTTL
}

func (b *Bot) persistTelegramHandled(ctx context.Context, item storage.TelegramInboxUpdate, owner string, guard *telegramInboxLeaseGuard, revision *telegramPrivateStateRevision, commitSignal *telegramUpdateCommitSignal) bool {
	for attempt := 1; ctx.Err() == nil && !guard.Lost(); attempt++ {
		now := time.Now().In(b.loc)
		var ok bool
		var err error
		if revision == nil {
			if b.inboxMarkHandled != nil {
				ok, err = b.inboxMarkHandled(ctx, item, owner, now)
			} else {
				ok, err = b.store.MarkTelegramUpdateHandled(ctx, item.StreamKey, item.UpdateID, owner, now)
			}
		} else {
			stateJSON := []byte(`{}`)
			hasState := false
			if state, exists := b.privateStates.Get(formatID(revision.userID)); exists {
				stateJSON, err = json.Marshal(state)
				hasState = true
			}
			if err == nil {
				ok, err = b.store.CommitTelegramPrivateStateHandledAndQuickReply(ctx, item, owner, revision.userID,
					revision.expectedVersion, stateJSON, hasState, commitSignal.QuickReply(), now)
			}
		}
		if err == nil {
			if !ok {
				guard.MarkLost()
			}
			return ok
		}
		if errors.Is(err, storage.ErrTelegramInboxLeaseLost) {
			guard.MarkLost()
			return false
		}
		if attempt == 1 || attempt%10 == 0 {
			log.Printf("mark telegram update %d handled attempt=%d: %v", item.UpdateID, attempt, err)
		}
		if !waitTelegramInboxRetry(ctx, guard, 100*time.Millisecond) {
			return false
		}
	}
	return false
}

func (b *Bot) persistTelegramDone(ctx context.Context, item storage.TelegramInboxUpdate, owner string, guard *telegramInboxLeaseGuard) bool {
	for attempt := 1; ctx.Err() == nil && !guard.Lost(); attempt++ {
		now := time.Now().In(b.loc)
		var ok bool
		var err error
		if b.inboxComplete != nil {
			ok, err = b.inboxComplete(ctx, item, owner, now)
		} else {
			ok, err = b.store.CompleteTelegramUpdate(ctx, item.StreamKey, item.UpdateID, owner, now)
		}
		if err == nil {
			if !ok {
				guard.MarkLost()
				log.Printf("telegram update %d completion lost lease", item.UpdateID)
			}
			return ok
		}
		if attempt == 1 || attempt%10 == 0 {
			log.Printf("complete telegram update %d attempt=%d: %v", item.UpdateID, attempt, err)
		}
		if !waitTelegramInboxRetry(ctx, guard, 100*time.Millisecond) {
			return false
		}
	}
	return false
}

func (b *Bot) persistTelegramRetry(ctx context.Context, item storage.TelegramInboxUpdate, owner string, guard *telegramInboxLeaseGuard, cause error) {
	for attempt := 1; ctx.Err() == nil && !guard.Lost(); attempt++ {
		now := time.Now().In(b.loc)
		status, err := b.store.RetryTelegramUpdate(ctx, item.StreamKey, item.UpdateID, owner,
			telegramInboxMaxAttempts, now.Add(telegramInboxRetryDelay(item.Attempts)), now, cause)
		if err == nil {
			if status == "" {
				guard.MarkLost()
			} else {
				log.Printf("telegram update %d handler failed status=%s attempt=%d: %v", item.UpdateID, status, item.Attempts, cause)
			}
			return
		}
		if attempt == 1 || attempt%10 == 0 {
			log.Printf("retry telegram update %d after handler error %v attempt=%d: %v", item.UpdateID, cause, attempt, err)
		}
		if !waitTelegramInboxRetry(ctx, guard, 100*time.Millisecond) {
			return
		}
	}
}

func waitTelegramInboxRetry(ctx context.Context, guard *telegramInboxLeaseGuard, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer stopAdmissionTimer(timer)
	select {
	case <-ctx.Done():
		return false
	case <-guard.LostChannel():
		return false
	case <-timer.C:
		return true
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
