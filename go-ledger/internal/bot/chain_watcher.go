package bot

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/chainwatcher"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/tron"
)

func (b *Bot) chainWatcherSyncScheduler(ctx context.Context) {
	b.syncAllChainWatcherSubscriptions(ctx)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.syncAllChainWatcherSubscriptions(ctx)
		}
	}
}

func (b *Bot) chainWatcherEventScheduler(ctx context.Context) {
	interval := b.cfg.ChainWatcherPollInterval
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		b.pollChainWatcherEventsWithStatus(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (b *Bot) chainWatcherHealthScheduler(ctx context.Context) {
	interval := b.cfg.BotWatcherHealthInterval
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		b.checkChainWatcherHealth(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (b *Bot) chainWatcherFallbackScheduler(ctx context.Context) {
	if b.cfg.ChainWatcherEmergencyFallback {
		return
	}
	interval := b.cfg.BotFallbackPollInterval
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		b.pollFallbackIfActive(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (b *Bot) syncAllChainWatcherSubscriptions(ctx context.Context) {
	if !b.cfg.ChainWatcherEnabled() {
		return
	}
	targets, err := b.store.ListWatchTargets(ctx)
	if err != nil {
		log.Printf("list watch targets for chain watcher sync: %v", err)
		return
	}
	if err := b.watcher.SyncSubscriptions(ctx, targets); err != nil {
		log.Printf("sync chain watcher subscriptions: %v", err)
	}
}

func (b *Bot) syncChainWatcherTargetAsync(ctx context.Context, target storage.WatchTarget) {
	if !b.cfg.ChainWatcherEnabled() {
		return
	}
	b.dispatcher.Submit(ctx, "chain-watcher-sync", b.chainPool, func(jobCtx context.Context) {
		if err := b.watcher.UpsertSubscription(jobCtx, target); err != nil {
			log.Printf("sync chain watcher target %s: %v", target.Address, err)
		}
	})
}

func (b *Bot) deleteChainWatcherSubscriptionAsync(ctx context.Context, owner int64, address string) {
	if !b.cfg.ChainWatcherEnabled() {
		return
	}
	b.dispatcher.Submit(ctx, "chain-watcher-sync", b.chainPool, func(jobCtx context.Context) {
		if err := b.watcher.DeleteSubscription(jobCtx, owner, address); err != nil {
			log.Printf("delete chain watcher target %s: %v", address, err)
		}
	})
}

func (b *Bot) pollChainWatcherEventsWithStatus(ctx context.Context) {
	timing, err := b.pollChainWatcherEvents(ctx)
	if err != nil {
		timing.Error = err.Error()
		b.watcherTiming.record(timing)
		log.Printf("claim chain watcher events: %v", err)
		b.recordChainWatcherFailure("claim")
		return
	}
	b.watcherTiming.record(timing)
	if timing.EventCount > 0 {
		log.Printf("chain watcher timing: events=%d acked=%d claim_ms=%d notify_ms=%d outbox_ms=%d gateway_ms=%d ack_ms=%d",
			timing.EventCount, timing.AckedCount, timing.ClaimDuration.Milliseconds(), timing.NotifyDuration.Milliseconds(),
			timing.OutboxDuration.Milliseconds(), timing.GatewayDuration.Milliseconds(), timing.AckDuration.Milliseconds())
	}
	b.recordChainWatcherSuccess("claim")
}

func (b *Bot) pollChainWatcherEvents(ctx context.Context) (chainWatcherPollTiming, error) {
	var timing chainWatcherPollTiming
	timing.StartedAt = time.Now()
	if !b.cfg.ChainWatcherEnabled() {
		return timing, nil
	}
	claimCtx, cancel := context.WithTimeout(ctx, b.cfg.BotWatcherClaimTimeout)
	defer cancel()
	claimStarted := time.Now()
	events, err := b.watcher.ClaimEvents(claimCtx, b.cfg.ChainWatcherBatchSize)
	timing.ClaimDuration = time.Since(claimStarted)
	if err != nil {
		timing.Error = err.Error()
		return timing, err
	}
	timing.EventCount = len(events)
	if len(events) == 0 {
		return timing, nil
	}
	acked := make([]string, 0, len(events))
	for _, event := range events {
		notificationTiming, err := b.processChainWatcherEvent(ctx, event)
		timing.NotifyDuration += notificationTiming.NotifyDuration
		timing.OutboxDuration += notificationTiming.OutboxDuration
		timing.GatewayDuration += notificationTiming.GatewayDuration
		if err != nil {
			log.Printf("process chain watcher event %s: %v", event.DeliveryID, err)
			continue
		}
		acked = append(acked, event.DeliveryID)
	}
	timing.AckedCount = len(acked)
	if len(acked) == 0 {
		return timing, nil
	}
	ackStarted := time.Now()
	if err := b.watcher.AckEvents(ctx, acked); err != nil {
		timing.AckDuration = time.Since(ackStarted)
		timing.Error = err.Error()
		return timing, err
	}
	timing.AckDuration = time.Since(ackStarted)
	return timing, nil
}

func (b *Bot) checkChainWatcherHealth(ctx context.Context) {
	if !b.cfg.ChainWatcherEnabled() {
		return
	}
	healthCtx, cancel := context.WithTimeout(ctx, b.cfg.BotWatcherClaimTimeout)
	defer cancel()
	status, err := b.watcher.Ready(healthCtx)
	if err != nil {
		log.Printf("chain watcher readiness: %v", err)
		b.recordChainWatcherFailure("ready")
		return
	}
	if !status.Ready {
		log.Printf("chain watcher source degraded: %s", status.Status)
		b.recordChainWatcherFailure("ready")
		return
	}
	b.recordChainWatcherSuccess("ready")
}

func (b *Bot) recordChainWatcherFailure(source string) {
	if b.watcherFallback.recordFailure(source, time.Now()) {
		log.Printf("chain watcher %s failed repeatedly; enabling temporary no-key fallback", source)
	}
}

func (b *Bot) recordChainWatcherSuccess(source string) {
	if b.watcherFallback.recordSuccess(source, time.Now()) {
		log.Printf("chain watcher %s recovered; disabling temporary fallback", source)
	}
}

func (b *Bot) pollFallbackIfActive(ctx context.Context) {
	if !b.watcherFallback.active(time.Now()) {
		return
	}
	if !b.watchRunning.CompareAndSwap(false, true) {
		return
	}
	if !b.chainPool.Submit(func(jobCtx context.Context) {
		defer b.watchRunning.Store(false)
		if err := b.pollAddressWatchesWithClient(jobCtx, b.fallbackTron); err != nil {
			log.Printf("poll fallback address watches: %v", err)
		}
	}) {
		b.watchRunning.Store(false)
		log.Printf("poll fallback address watches: chain queue is full")
	}
}

func (b *Bot) processChainWatcherEvent(ctx context.Context, event chainwatcher.MatchedEvent) (notificationTiming, error) {
	var timing notificationTiming
	transfer := tron.Transfer{
		Hash:           event.TxHash,
		From:           event.From,
		To:             event.To,
		Value:          event.Value,
		TokenSymbol:    event.TokenSymbol,
		TokenAddress:   event.TokenAddress,
		TokenDecimals:  event.TokenDecimals,
		BlockTimestamp: event.BlockTimestamp,
		Confirmed:      event.Confirmed,
	}
	target := storage.WatchTarget{
		OwnerUserID:     event.OwnerUserID,
		Address:         event.WatchAddress,
		Label:           event.Label,
		WatchIncome:     event.Direction == "income",
		WatchExpense:    event.Direction == "expense",
		MinNotifyAmount: "0",
	}
	notifyStarted := time.Now()
	text := b.formatTransferNotice(transfer, target, event.Direction)
	timing.NotifyDuration = time.Since(notifyStarted)
	chatID := event.ChatID
	if chatID == 0 {
		chatID = event.OwnerUserID
	}
	outboxStarted := time.Now()
	inserted, err := b.store.RecordChainNotificationOutbox(ctx, event.OwnerUserID, event.WatchAddress, event.TxHash, event.Direction, event.BlockTimestamp, chatID, text, "HTML", true, time.Now().In(b.loc))
	timing.OutboxDuration = time.Since(outboxStarted)
	if err != nil {
		return timing, err
	}
	if inserted {
		gatewayStarted := time.Now()
		b.kickNotificationOutbox()
		timing.GatewayDuration = time.Since(gatewayStarted)
	}
	return timing, nil
}

const watcherRecoveryThreshold = 3

type watcherFallbackController struct {
	mu            sync.Mutex
	failThreshold int
	maxActive     time.Duration
	failures      map[string]int
	successes     int
	activeNow     bool
	exhausted     bool
	startedAt     time.Time
}

type notificationTiming struct {
	NotifyDuration  time.Duration
	OutboxDuration  time.Duration
	GatewayDuration time.Duration
}

type chainWatcherPollTiming struct {
	StartedAt       time.Time
	EventCount      int
	AckedCount      int
	ClaimDuration   time.Duration
	NotifyDuration  time.Duration
	OutboxDuration  time.Duration
	GatewayDuration time.Duration
	AckDuration     time.Duration
	Error           string
}

type chainWatcherTimingStatus struct {
	mu     sync.Mutex
	last   chainWatcherPollTiming
	recent []chainWatcherPollTiming
}

func (s *chainWatcherTimingStatus) record(timing chainWatcherPollTiming) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.last = timing
	s.recent = append(s.recent, timing)
	if len(s.recent) > 5 {
		copy(s.recent, s.recent[len(s.recent)-5:])
		s.recent = s.recent[:5]
	}
}

func newWatcherFallbackController(failThreshold int, maxActive time.Duration) *watcherFallbackController {
	if failThreshold < 1 {
		failThreshold = 1
	}
	if maxActive <= 0 {
		maxActive = 10 * time.Minute
	}
	return &watcherFallbackController{failThreshold: failThreshold, maxActive: maxActive, failures: make(map[string]int)}
}

func (c *watcherFallbackController) recordFailure(source string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if source == "" {
		source = "watcher"
	}
	c.successes = 0
	if c.exhausted {
		return false
	}
	c.failures[source]++
	if c.activeNow || c.failures[source] < c.failThreshold {
		return false
	}
	c.activeNow = true
	c.startedAt = now
	return true
}

func (c *watcherFallbackController) recordSuccess(source string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if source == "" {
		source = "watcher"
	}
	delete(c.failures, source)
	if len(c.failures) > 0 {
		c.successes = 0
		return false
	}
	c.successes++
	if c.successes < watcherRecoveryThreshold {
		return false
	}
	changed := c.activeNow || c.exhausted
	c.activeNow = false
	c.exhausted = false
	c.startedAt = time.Time{}
	return changed
}

func (c *watcherFallbackController) active(now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.activeNow {
		return false
	}
	if c.maxActive > 0 && now.Sub(c.startedAt) >= c.maxActive {
		c.activeNow = false
		c.exhausted = true
		c.failures = make(map[string]int)
		return false
	}
	return true
}
