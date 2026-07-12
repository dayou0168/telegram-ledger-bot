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
	} else {
		b.fallbackSubCache.Delete("all")
	}
}

func (b *Bot) syncChainWatcherTargetAsync(ctx context.Context, target storage.WatchTarget) {
	if !b.cfg.ChainWatcherEnabled() {
		return
	}
	b.dispatcher.Submit(ctx, "chain-watcher-sync", b.chainPool, func(jobCtx context.Context) {
		if err := b.watcher.UpsertSubscription(jobCtx, target); err != nil {
			log.Printf("sync chain watcher target %s: %v", target.Address, err)
		} else {
			b.fallbackSubCache.Delete("all")
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
		} else {
			b.fallbackSubCache.Delete("all")
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
		if fallbackErr := b.pollSharedFallbackEvents(ctx); fallbackErr != nil {
			log.Printf("claim shared fallback events: %v", fallbackErr)
		}
		return
	}
	b.watcherTiming.record(timing)
	if timing.EventCount > 0 {
		log.Printf("chain watcher timing: events=%d acked=%d claim_ms=%d notify_ms=%d outbox_ms=%d gateway_ms=%d ack_ms=%d",
			timing.EventCount, timing.AckedCount, timing.ClaimDuration.Milliseconds(), timing.NotifyDuration.Milliseconds(),
			timing.OutboxDuration.Milliseconds(), timing.GatewayDuration.Milliseconds(), timing.AckDuration.Milliseconds())
	}
	b.recordChainWatcherSuccess("claim", 0)
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
	b.recordChainWatcherSuccess("ready", time.Duration(status.CatchupLagSeconds)*time.Second)
}

func (b *Bot) recordChainWatcherFailure(source string) {
	if b.watcherFallback.recordFailure(source, time.Now()) {
		log.Printf("chain watcher %s failed repeatedly; enabling temporary no-key fallback", source)
	}
}

func (b *Bot) recordChainWatcherSuccess(source string, lag time.Duration) {
	if b.watcherFallback.recordSuccess(source, time.Now(), lag) {
		log.Printf("chain watcher %s state changed after success", source)
	}
}

func (b *Bot) pollFallbackIfActive(ctx context.Context) {
	if next := b.fallbackNextPoll.Load(); next > 0 && time.Now().UnixNano() < next {
		return
	}
	state := b.watcherFallback.snapshot(time.Now())
	if state.Mode == fallbackModePrimary {
		if b.fallbackStore != nil {
			_ = b.fallbackStore.ReleaseChainWatcherFallbackLease(ctx, "public-no-key", b.cfg.BotFallbackInstanceID, string(state.Mode), time.Now())
		}
		return
	}
	if state.Mode != fallbackModeActive && state.Mode != fallbackModeRecovery {
		return
	}
	if b.fallbackStore == nil || b.cfg.BotFallbackInstanceID == "" {
		b.watcherFallback.setDegraded()
		log.Printf("DEGRADED: shared fallback requires BOT_FALLBACK_SHARED_DATABASE_URL and BOT_FALLBACK_INSTANCE_ID")
		return
	}
	if !b.watchRunning.CompareAndSwap(false, true) {
		return
	}
	if !b.chainPool.Submit(func(jobCtx context.Context) {
		defer b.watchRunning.Store(false)
		lease, leader, err := b.fallbackStore.AcquireChainWatcherFallbackLease(jobCtx, "public-no-key", b.cfg.BotFallbackInstanceID, string(state.Mode), b.cfg.BotFallbackLeaseTTL, time.Now())
		if err != nil {
			log.Printf("acquire shared fallback lease: %v", err)
			return
		}
		if !leader {
			_ = lease
			return
		}
		b.fallbackTron.ProbeDueKeys(jobCtx, b.cfg.USDTContract)
		stats, err := b.pollSharedGlobalFallback(jobCtx)
		if updateErr := b.fallbackStore.UpdateChainWatcherFallbackLease(jobCtx, "public-no-key", b.cfg.BotFallbackInstanceID, string(state.Mode), state.LastWatcherSuccess,
			stats.Requests, stats.RateLimits, stats.From, stats.To, stats.Pages, stats.BudgetUsed, b.cfg.BotFallbackLeaseTTL, time.Now()); updateErr != nil {
			log.Printf("update shared fallback lease: %v", updateErr)
		}
		if err != nil {
			log.Printf("poll shared global fallback: %v", err)
			b.recordFallbackPollResult(err)
		} else {
			b.recordFallbackPollResult(nil)
		}
	}) {
		b.watchRunning.Store(false)
		log.Printf("poll fallback address watches: chain queue is full")
	}
}

func (b *Bot) pollSharedFallbackEvents(ctx context.Context) error {
	if b.fallbackStore == nil || !b.watcherFallback.active(time.Now()) {
		return nil
	}
	events, err := b.fallbackStore.ClaimChainWatcherMatchedEvents(ctx, b.cfg.ChainWatcherBotID, b.cfg.ChainWatcherBatchSize, 30*time.Second, 10*time.Minute, time.Now())
	if err != nil {
		return err
	}
	acked := make([]string, 0, len(events))
	for _, item := range events {
		event := chainwatcher.FromMatchedStorage(item)
		if _, err := b.processChainWatcherEvent(ctx, event); err != nil {
			continue
		}
		acked = append(acked, item.DeliveryID)
	}
	if len(acked) == 0 {
		return nil
	}
	return b.fallbackStore.AckChainWatcherMatchedEvents(ctx, b.cfg.ChainWatcherBotID, acked, time.Now())
}

type sharedFallbackScanStats struct {
	From       int64
	To         int64
	Requests   int64
	RateLimits int64
	Pages      int64
	BudgetUsed int64
}

func (b *Bot) pollSharedGlobalFallback(ctx context.Context) (sharedFallbackScanStats, error) {
	var stats sharedFallbackScanStats
	subs, err := b.fallbackSubscriptions(ctx)
	if err != nil {
		return stats, err
	}
	if len(subs) == 0 {
		return stats, nil
	}
	byAddress := make(map[string][]storage.ChainWatcherSubscription, len(subs))
	for _, sub := range subs {
		byAddress[sub.Address] = append(byAddress[sub.Address], sub)
	}
	head, err := b.fallbackStore.GetChainWatcherFallbackHead(ctx)
	if err != nil {
		return stats, err
	}
	now := time.Now()
	cutoff := now.UnixMilli()
	minTimestamp := now.Add(-10 * time.Minute).UnixMilli()
	fetch, err := b.fallbackTron.FetchGlobalUSDTTransfersAtWithMetrics(ctx, b.cfg.USDTContract, minTimestamp, cutoff, 3)
	stats.From, stats.To = head.Timestamp, cutoff
	stats.Requests, stats.Pages = int64(fetch.Metrics.Calls), int64(fetch.Metrics.Pages)
	if _, limited := tron.IsRateLimited(err); limited {
		stats.RateLimits = 1
	}
	if err != nil {
		return stats, err
	}
	if err := b.processSharedFallbackTransfers(ctx, fetch.Transfers, byAddress); err != nil {
		return stats, err
	}
	newAnchor, anchorFound := chainwatcher.AnchorCoverage(fetch.Transfers, head.TxHash)
	if err := b.fallbackStore.AdvanceChainWatcherFallbackHead(ctx, cutoff, newAnchor, now); err != nil {
		return stats, err
	}
	if !anchorFound && head.TxHash != "" && b.fallbackExpandRunning.CompareAndSwap(false, true) {
		go func() {
			defer b.fallbackExpandRunning.Store(false)
			expandCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if !b.pollFallbackExpand(expandCtx, head.TxHash, minTimestamp, cutoff, byAddress) {
				watermark, err := b.fallbackStore.GetChainWatcherWatermark(expandCtx)
				if watermark.Timestamp <= 0 {
					watermark.Timestamp = minTimestamp
				}
				if err == nil && watermark.Timestamp < cutoff-2000 {
					budget := 3
					advanced, _, _, _ := b.scanSharedFallbackWindow(expandCtx, watermark.Timestamp, cutoff-2000, byAddress, &budget)
					if advanced > watermark.Timestamp {
						_ = b.fallbackStore.AdvanceChainWatcherWatermark(expandCtx, advanced, "", "fallback", time.Now())
					}
				}
			}
		}()
	}
	return stats, nil
}

func (b *Bot) pollFallbackExpand(ctx context.Context, anchor string, minTimestamp, cutoff int64, byAddress map[string][]storage.ChainWatcherSubscription) bool {
	for page := 3; page < 20; page++ {
		fetch, err := b.fallbackTron.FetchGlobalUSDTTransfersRangeWithMetrics(ctx, b.cfg.USDTContract, minTimestamp, cutoff, page, 1)
		if err != nil {
			return false
		}
		if err := b.processSharedFallbackTransfers(ctx, fetch.Transfers, byAddress); err != nil {
			return false
		}
		for _, transfer := range fetch.Transfers {
			if chainwatcher.EventID(transfer) == anchor {
				return true
			}
		}
		if len(fetch.Transfers) < 50 {
			return false
		}
	}
	return false
}

func (b *Bot) recordFallbackPollResult(err error) {
	steps := []time.Duration{time.Second, 2 * time.Second, 3 * time.Second, 5 * time.Second, 10 * time.Second}
	step := int(b.fallbackBackoff.Load())
	delay := steps[step]
	if err != nil {
		if step < len(steps)-1 {
			b.fallbackBackoff.Store(int32(step + 1))
		}
	} else if step > 0 {
		step--
		b.fallbackBackoff.Store(int32(step))
		delay = steps[step]
	}
	b.fallbackNextPoll.Store(time.Now().Add(delay).UnixNano())
}

func (b *Bot) scanSharedFallbackWindow(ctx context.Context, from, to int64, byAddress map[string][]storage.ChainWatcherSubscription, budget *int) (int64, int64, int64, error) {
	if to <= from || budget == nil || *budget <= 0 {
		return from, 0, 0, nil
	}
	pages := b.cfg.BotFallbackGlobalPages
	if pages > *budget {
		pages = *budget
	}
	fetch, err := b.fallbackTron.FetchGlobalUSDTTransfersWindowWithMetrics(ctx, b.cfg.USDTContract, from, to, pages)
	requests := int64(fetch.Metrics.Calls)
	*budget -= fetch.Metrics.Calls
	rateLimits := int64(0)
	if _, limited := tron.IsRateLimited(err); limited {
		rateLimits = 1
	}
	if err != nil {
		return from, requests, rateLimits, err
	}
	if processErr := b.processSharedFallbackTransfers(ctx, fetch.Transfers, byAddress); processErr != nil {
		return from, requests, rateLimits, processErr
	}
	saturated := fetch.Metrics.Pages >= pages && fetch.Metrics.LastPageRows >= 50
	if !saturated {
		return to, requests, rateLimits, nil
	}
	if to-from <= 1000 || *budget <= 0 {
		return from, requests, rateLimits, nil
	}
	middle := from + (to-from)/2
	leftTo, leftRequests, left429, leftErr := b.scanSharedFallbackWindow(ctx, from, middle, byAddress, budget)
	requests += leftRequests
	rateLimits += left429
	if leftErr != nil || leftTo < middle || *budget <= 0 {
		return leftTo, requests, rateLimits, leftErr
	}
	rightTo, rightRequests, right429, rightErr := b.scanSharedFallbackWindow(ctx, middle, to, byAddress, budget)
	return rightTo, requests + rightRequests, rateLimits + right429, rightErr
}

func (b *Bot) processSharedFallbackTransfers(ctx context.Context, transfers []tron.Transfer, byAddress map[string][]storage.ChainWatcherSubscription) error {
	for _, transfer := range transfers {
		candidates := append([]storage.ChainWatcherSubscription{}, byAddress[transfer.From]...)
		candidates = append(candidates, byAddress[transfer.To]...)
		if len(candidates) == 0 {
			continue
		}
		deliveries := chainwatcher.MatchTransfer(transfer, candidates)
		if len(deliveries) == 0 {
			continue
		}
		if _, err := b.fallbackStore.RecordChainWatcherMatches(ctx, chainwatcher.TransferEvent(transfer, "fallback"), deliveries, time.Now()); err != nil {
			return err
		}
	}
	return nil
}

func (b *Bot) fallbackSubscriptions(ctx context.Context) ([]storage.ChainWatcherSubscription, error) {
	if cached, ok := b.fallbackSubCache.Get("all"); ok {
		return cached, nil
	}
	subs, err := b.fallbackStore.ListChainWatcherSubscriptions(ctx)
	if err != nil {
		return nil, err
	}
	b.fallbackSubCache.Set("all", subs)
	return subs, nil
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
	inserted, err := b.store.RecordChainNotificationOutboxEvent(ctx, event.OwnerUserID, event.WatchAddress, event.TxHash, event.EventID, event.Direction, event.BlockTimestamp, chatID, text, "HTML", true, time.Now().In(b.loc))
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

type fallbackMode string

const (
	fallbackModePrimary  fallbackMode = "PRIMARY"
	fallbackModePending  fallbackMode = "FAILOVER_PENDING"
	fallbackModeActive   fallbackMode = "FALLBACK_ACTIVE"
	fallbackModeRecovery fallbackMode = "RECOVERING"
	fallbackModeDegraded fallbackMode = "DEGRADED"
)

type watcherFallbackController struct {
	mu                 sync.Mutex
	failThreshold      int
	recoveryThreshold  int
	lagThreshold       time.Duration
	mode               fallbackMode
	failures           int
	firstFailureAt     time.Time
	readySuccesses     int
	claimSuccesses     int
	watcherLag         time.Duration
	startedAt          time.Time
	lastWatcherSuccess time.Time
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

func newWatcherFallbackController(failThreshold int) *watcherFallbackController {
	return newWatcherFallbackControllerWithRecovery(failThreshold, 3, 5*time.Second)
}

func newWatcherFallbackControllerWithRecovery(failThreshold int, recoveryThreshold int, lagThreshold time.Duration) *watcherFallbackController {
	if failThreshold < 1 {
		failThreshold = 1
	}
	if recoveryThreshold < 2 {
		recoveryThreshold = 2
	}
	if lagThreshold <= 0 {
		lagThreshold = 5 * time.Second
	}
	return &watcherFallbackController{failThreshold: failThreshold, recoveryThreshold: recoveryThreshold, lagThreshold: lagThreshold, mode: fallbackModePrimary}
}

func (c *watcherFallbackController) recordFailure(source string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.mode == fallbackModeDegraded {
		return false
	}
	c.readySuccesses = 0
	c.claimSuccesses = 0
	c.failures++
	if c.firstFailureAt.IsZero() {
		c.firstFailureAt = now
	}
	if c.mode == fallbackModePrimary {
		c.mode = fallbackModePending
		return true
	}
	if c.mode == fallbackModeRecovery {
		c.mode = fallbackModeActive
		return true
	}
	if c.mode == fallbackModeActive || c.failures < c.failThreshold || now.Sub(c.firstFailureAt) < 3*time.Second {
		return false
	}
	c.mode = fallbackModeActive
	c.startedAt = now
	return true
}

func (c *watcherFallbackController) recordSuccess(source string, now time.Time, lag time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastWatcherSuccess = now
	c.failures = 0
	c.firstFailureAt = time.Time{}
	if c.mode == fallbackModePending {
		c.mode = fallbackModePrimary
		return true
	}
	if c.mode == fallbackModeActive || c.mode == fallbackModeDegraded {
		c.mode = fallbackModeRecovery
		c.readySuccesses = 0
		c.claimSuccesses = 0
	}
	if c.mode != fallbackModeRecovery {
		return false
	}
	if source == "ready" {
		c.readySuccesses++
		c.watcherLag = lag
	}
	if source == "claim" {
		c.claimSuccesses++
	}
	if c.readySuccesses < c.recoveryThreshold || c.claimSuccesses < c.recoveryThreshold || c.watcherLag > c.lagThreshold {
		return false
	}
	c.mode = fallbackModePrimary
	c.startedAt = time.Time{}
	return true
}

func (c *watcherFallbackController) active(now time.Time) bool {
	mode := c.snapshot(now).Mode
	return mode == fallbackModeActive || mode == fallbackModeRecovery
}

func (c *watcherFallbackController) setDegraded() {
	c.mu.Lock()
	c.mode = fallbackModeDegraded
	c.mu.Unlock()
}

type fallbackStateSnapshot struct {
	Mode               fallbackMode
	StartedAt          time.Time
	LastWatcherSuccess time.Time
	Exhausted          bool
}

func (c *watcherFallbackController) snapshot(now time.Time) fallbackStateSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return fallbackStateSnapshot{Mode: c.mode, StartedAt: c.startedAt, LastWatcherSuccess: c.lastWatcherSuccess, Exhausted: c.mode == fallbackModeDegraded}
}
