package bot

import (
	"context"
	"errors"
	"fmt"
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
		if b.fallbackStore != nil {
			subs := make([]storage.ChainWatcherSubscription, 0, len(targets))
			for _, target := range targets {
				subs = append(subs, b.sharedSubscription(target))
			}
			if mirrorErr := b.fallbackStore.ReplaceChainWatcherSubscriptions(ctx, b.cfg.ChainWatcherBotID, subs, time.Now()); mirrorErr != nil {
				log.Printf("mirror chain watcher subscriptions to shared fallback DB: %v", mirrorErr)
				return
			}
			b.fallbackSubCache.Delete("all")
		}
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
			if b.fallbackStore != nil {
				if mirrorErr := b.fallbackStore.UpsertChainWatcherSubscription(jobCtx, b.sharedSubscription(target), time.Now()); mirrorErr != nil {
					log.Printf("mirror chain watcher target %s to shared fallback DB: %v", target.Address, mirrorErr)
					return
				}
				b.fallbackSubCache.Delete("all")
			}
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
			if b.fallbackStore != nil {
				if mirrorErr := b.fallbackStore.RemoveChainWatcherSubscription(jobCtx, b.cfg.ChainWatcherBotID, owner, owner, address, time.Now()); mirrorErr != nil {
					log.Printf("mirror chain watcher target delete %s to shared fallback DB: %v", address, mirrorErr)
					return
				}
				b.fallbackSubCache.Delete("all")
			}
		} else {
			b.fallbackSubCache.Delete("all")
		}
	})
}

func (b *Bot) sharedSubscription(target storage.WatchTarget) storage.ChainWatcherSubscription {
	return chainwatcher.ToSubscription(b.cfg.ChainWatcherBotID, chainwatcher.SubscriptionRequest{
		ChatID: target.OwnerUserID, OwnerUserID: target.OwnerUserID, Address: target.Address,
		Label: target.Label, WatchIncome: target.WatchIncome, WatchExpense: target.WatchExpense,
		NotifyTRX: false, MinNotifyAmount: target.MinNotifyAmount, BaselineTimestamp: target.BaselineTimestamp,
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
		trace := outboxTrace(fmt.Sprintf("chain:%d:%s:%s:%s", event.OwnerUserID, event.WatchAddress, event.EventID, event.Direction))
		if err != nil {
			log.Printf("chain watcher delivery timing: trace=%s status=failed notify_ms=%d outbox_ms=%d gateway_kick_ms=%d error=%v",
				trace, notificationTiming.NotifyDuration.Milliseconds(), notificationTiming.OutboxDuration.Milliseconds(), notificationTiming.GatewayDuration.Milliseconds(), err)
			continue
		}
		log.Printf("chain watcher delivery timing: trace=%s status=queued notify_ms=%d outbox_ms=%d gateway_kick_ms=%d",
			trace, notificationTiming.NotifyDuration.Milliseconds(), notificationTiming.OutboxDuration.Milliseconds(), notificationTiming.GatewayDuration.Milliseconds())
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
		log.Printf("chain watcher %s failed repeatedly; requesting shared fallback lease", source)
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
		b.fallbackLeaderActive.Store(false)
		if b.fallbackStore != nil {
			_ = b.fallbackStore.ReleaseChainWatcherFallbackLease(ctx, "public-no-key", b.cfg.BotFallbackInstanceID, string(state.Mode), time.Now())
		}
		return
	}
	if state.Mode != fallbackModePending && state.Mode != fallbackModeActive && state.Mode != fallbackModeRecovery {
		return
	}
	if state.Mode == fallbackModePending && !state.LeaseRequested {
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
			b.fallbackLeaderActive.Store(false)
			_ = lease
			return
		}
		b.watcherFallback.activateLease(time.Now())
		state = b.watcherFallback.snapshot(time.Now())
		if b.fallbackLeaderActive.CompareAndSwap(false, true) {
			log.Printf("shared no-key fallback active: lease acquired by this instance")
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
	if handled, err := b.processOneFallbackGap(ctx); err != nil {
		return stats, err
	} else if handled {
		return stats, nil
	}
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
	if head.TxHash == "" || anchorFound {
		if err := b.fallbackStore.AdvanceChainWatcherFallbackHead(ctx, cutoff, newAnchor, now); err != nil {
			return stats, err
		}
	} else {
		_, err := b.fallbackStore.EnqueueChainWatcherGap(ctx, storage.ChainWatcherGapTask{
			Kind: "expand", Source: "fallback", Priority: 0, Reason: "fallback_anchor_missing",
			FromTimestamp: minTimestamp, ToTimestamp: cutoff, StartPage: 3, EndPage: 20,
			NextPage: 3, AnchorEventID: head.TxHash, HeadEventID: newAnchor,
		}, now)
		if err != nil {
			return stats, err
		}
	}
	return stats, nil
}

func (b *Bot) processOneFallbackGap(ctx context.Context) (bool, error) {
	task, ok, err := b.fallbackStore.ClaimChainWatcherGap(ctx, b.cfg.BotFallbackInstanceID, "fallback", b.cfg.BotFallbackLeaseTTL, time.Now())
	if err != nil || !ok {
		return false, err
	}
	subs, err := b.fallbackSubscriptions(ctx)
	if err != nil {
		_, _ = b.fallbackStore.ReleaseChainWatcherGap(ctx, task.ID, task.LeaseGeneration, task.LeaseOwner, err.Error(), time.Now())
		return true, err
	}
	byAddress := make(map[string][]storage.ChainWatcherSubscription, len(subs))
	for _, sub := range subs {
		byAddress[sub.Address] = append(byAddress[sub.Address], sub)
	}
	page := task.NextPage
	if page < task.StartPage {
		page = task.StartPage
	}
	fetch, fetchErr := b.fallbackTron.FetchGlobalUSDTTransfersRangeWithMetrics(ctx, b.cfg.USDTContract, task.FromTimestamp, task.ToTimestamp, page, 1)
	if fetchErr != nil {
		_, _ = b.fallbackStore.ReleaseChainWatcherGap(ctx, task.ID, task.LeaseGeneration, task.LeaseOwner, fetchErr.Error(), time.Now())
		return true, fetchErr
	}
	if err := b.processSharedFallbackTransfers(ctx, fetch.Transfers, byAddress); err != nil {
		_, _ = b.fallbackStore.ReleaseChainWatcherGap(ctx, task.ID, task.LeaseGeneration, task.LeaseOwner, err.Error(), time.Now())
		return true, err
	}
	anchorFound := false
	for _, transfer := range fetch.Transfers {
		if chainwatcher.EventID(transfer) == task.AnchorEventID {
			anchorFound = true
		}
	}
	if task.Kind == "expand" && anchorFound {
		return true, b.completeFallbackGap(ctx, task)
	}
	full := len(fetch.Transfers) >= 50
	next := page + 1
	if full && next < task.EndPage {
		_, err = b.fallbackStore.YieldChainWatcherGap(ctx, task.ID, task.LeaseGeneration, task.LeaseOwner, next, "", time.Now())
		return true, err
	}
	if task.Kind == "expand" {
		completed, completeErr := b.fallbackStore.CompleteChainWatcherGap(ctx, task.ID, task.LeaseGeneration, task.LeaseOwner, time.Now())
		if completeErr != nil || !completed {
			return true, completeErr
		}
		_, err = b.fallbackStore.EnqueueChainWatcherGap(ctx, storage.ChainWatcherGapTask{
			Kind: "window", Source: "fallback", Priority: 1, Reason: "fallback_expand_exhausted",
			FromTimestamp: task.FromTimestamp, ToTimestamp: task.ToTimestamp,
			StartPage: 0, EndPage: 20, NextPage: 0, HeadEventID: task.HeadEventID,
		}, time.Now())
		return true, err
	}
	if full && task.ToTimestamp-task.FromTimestamp > 1 {
		middle := task.FromTimestamp + (task.ToTimestamp-task.FromTimestamp)/2
		_, err = b.fallbackStore.SplitChainWatcherGapWindow(ctx, task, middle, time.Now())
		return true, err
	}
	if full {
		_, err = b.fallbackStore.ReleaseChainWatcherGap(ctx, task.ID, task.LeaseGeneration, task.LeaseOwner, "page_limit_at_minimum_window", time.Now())
		if err == nil {
			err = errors.New("fallback gap page limit reached at minimum time window")
		}
		return true, err
	}
	return true, b.completeFallbackGap(ctx, task)
}

func (b *Bot) completeFallbackGap(ctx context.Context, task storage.ChainWatcherGapTask) error {
	completed, err := b.fallbackStore.CompleteChainWatcherGap(ctx, task.ID, task.LeaseGeneration, task.LeaseOwner, time.Now())
	if err != nil || !completed {
		return err
	}
	open, err := b.fallbackStore.CountOpenChainWatcherGaps(ctx, "fallback", time.Now())
	if err != nil || open != 0 {
		return err
	}
	return b.fallbackStore.AdvanceChainWatcherFallbackHead(ctx, task.ToTimestamp, task.HeadEventID, time.Now())
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
	readyFailures      int
	claimFailures      int
	firstReadyFailure  time.Time
	firstClaimFailure  time.Time
	readySuccesses     int
	claimSuccesses     int
	watcherLag         time.Duration
	startedAt          time.Time
	lastWatcherSuccess time.Time
	leaseRequested     bool
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
	if source == "ready" {
		c.readyFailures++
		c.readySuccesses = 0
		if c.firstReadyFailure.IsZero() {
			c.firstReadyFailure = now
		}
	} else {
		c.claimFailures++
		c.claimSuccesses = 0
		if c.firstClaimFailure.IsZero() {
			c.firstClaimFailure = now
		}
	}
	if c.mode == fallbackModePrimary {
		c.mode = fallbackModePending
		return false
	}
	if c.mode == fallbackModeRecovery {
		c.mode = fallbackModeActive
		return true
	}
	readyFailed := c.readyFailures >= c.failThreshold && !c.firstReadyFailure.IsZero() && now.Sub(c.firstReadyFailure) >= 3*time.Second
	claimFailed := c.claimFailures >= c.failThreshold && !c.firstClaimFailure.IsZero() && now.Sub(c.firstClaimFailure) >= 3*time.Second
	if c.mode == fallbackModeActive || (!readyFailed && !claimFailed) {
		return false
	}
	c.leaseRequested = true
	return true
}

func (c *watcherFallbackController) activateLease(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.mode != fallbackModePending || !c.leaseRequested {
		return
	}
	c.mode = fallbackModeActive
	c.startedAt = now
}

func (c *watcherFallbackController) recordSuccess(source string, now time.Time, lag time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if source == "ready" {
		c.lastWatcherSuccess = now
		c.readyFailures = 0
		c.firstReadyFailure = time.Time{}
	} else {
		c.claimFailures = 0
		c.firstClaimFailure = time.Time{}
	}
	if c.mode == fallbackModePending && c.readyFailures == 0 && c.claimFailures == 0 {
		c.mode = fallbackModePrimary
		c.leaseRequested = false
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
	c.leaseRequested = false
	return true
}

func (c *watcherFallbackController) active(now time.Time) bool {
	state := c.snapshot(now)
	return (state.Mode == fallbackModePending && state.LeaseRequested) || state.Mode == fallbackModeActive || state.Mode == fallbackModeRecovery
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
	LeaseRequested     bool
}

func (c *watcherFallbackController) snapshot(now time.Time) fallbackStateSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return fallbackStateSnapshot{Mode: c.mode, StartedAt: c.startedAt, LastWatcherSuccess: c.lastWatcherSuccess, Exhausted: c.mode == fallbackModeDegraded, LeaseRequested: c.leaseRequested}
}
