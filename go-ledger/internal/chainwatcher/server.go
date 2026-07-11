package chainwatcher

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/config"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/tron"
)

type Server struct {
	cfg            config.ChainWatcherConfig
	store          *storage.Store
	tron           *tron.Client
	http           *http.Server
	globalBackoff  apiBackoff
	addressBackoff apiBackoff
	status         watcherStatus
	addressMu      sync.Mutex
	addressCursor  int
	scanMu         sync.Mutex
	globalRunning  bool
	addressRunning bool
	watermarkMu    sync.Mutex
	watermarks     map[string]int64
}

func NewServer(cfg config.ChainWatcherConfig, store *storage.Store, tronClient *tron.Client) *Server {
	s := &Server{cfg: cfg, store: store, tron: tronClient, watermarks: make(map[string]int64)}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/readyz", s.handleReady)
	mux.HandleFunc("/status", s.handleStatus)
	mux.HandleFunc("/v1/subscriptions/upsert", s.handleUpsertSubscription)
	mux.HandleFunc("/v1/subscriptions/delete", s.handleDeleteSubscription)
	mux.HandleFunc("/v1/subscriptions/sync", s.handleSyncSubscriptions)
	mux.HandleFunc("/v1/events/claim", s.handleClaimEvents)
	mux.HandleFunc("/v1/events/ack", s.handleAckEvents)
	s.http = &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s
}

func (s *Server) Run(ctx context.Context) error {
	for botID, secret := range s.cfg.BotCredentials {
		if err := s.store.UpsertChainWatcherBot(ctx, botID, secret, time.Now()); err != nil {
			return err
		}
	}
	go s.globalLoop(ctx)
	go s.addressLoop(ctx)
	go s.cleanupLoop(ctx)
	errCh := make(chan error, 1)
	go func() {
		log.Printf("chain watcher listening on %s", s.cfg.ListenAddr)
		if err := s.http.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.http.Shutdown(shutdownCtx)
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}

func (s *Server) globalLoop(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.PollInterval)
	defer ticker.Stop()
	for {
		s.startGlobalScan(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Server) addressLoop(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.AddressInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.startAddressScan(ctx)
		}
	}
}

func (s *Server) cleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		stats, err := s.store.CleanupChainWatcherRetention(ctx, s.cfg.Lookback, time.Now())
		if err != nil && !errors.Is(err, context.Canceled) {
			s.status.recordCleanupError(err, time.Now())
			log.Printf("chain watcher cleanup: %v", err)
		} else if err == nil {
			s.status.recordCleanup(stats, time.Now())
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Server) startGlobalScan(ctx context.Context) {
	now := time.Now()
	if !s.globalBackoff.ready(now) {
		return
	}
	if !s.tryStartScan("global") {
		s.status.recordScanOverlap("global")
		return
	}
	go func() {
		defer s.finishScan("global")
		s.pollGlobalSafely(ctx)
	}()
}

func (s *Server) pollGlobalSafely(ctx context.Context) {
	started := time.Now()
	result, err := s.pollGlobalOnce(ctx)
	duration := time.Since(started)
	if err != nil && !errors.Is(err, context.Canceled) {
		if s.globalBackoff.record(err, time.Now()) {
			s.status.recordScanError("global", err, s.globalBackoff.untilTime(), result, duration, time.Now())
			log.Printf("chain watcher global scan rate limited: %v", err)
			return
		}
		s.status.recordScanError("global", err, time.Time{}, result, duration, time.Now())
		log.Printf("chain watcher global scan: %v", err)
		return
	}
	s.globalBackoff.reset()
	s.status.recordScanSuccess("global", result, duration, time.Now())
}

func (s *Server) startAddressScan(ctx context.Context) {
	now := time.Now()
	if !s.addressBackoff.ready(now) {
		return
	}
	if s.shouldSkipAddressScan(now) {
		s.status.recordAddressSkippedNearGlobal()
		return
	}
	if !s.tryStartScan("address") {
		s.status.recordScanOverlap("address")
		return
	}
	go func() {
		defer s.finishScan("address")
		s.pollAddressSafely(ctx)
	}()
}

func (s *Server) pollAddressSafely(ctx context.Context) {
	started := time.Now()
	result, err := s.pollAddressOnce(ctx)
	duration := time.Since(started)
	if err != nil && !errors.Is(err, context.Canceled) {
		if s.addressBackoff.record(err, time.Now()) {
			s.status.recordScanError("address", err, s.addressBackoff.untilTime(), result, duration, time.Now())
			log.Printf("chain watcher address scan rate limited: %v", err)
			return
		}
		s.status.recordScanError("address", err, time.Time{}, result, duration, time.Now())
		log.Printf("chain watcher address scan: %v", err)
		return
	}
	s.addressBackoff.reset()
	s.status.recordScanSuccess("address", result, duration, time.Now())
}

func (s *Server) pollGlobalOnce(ctx context.Context) (scanResult, error) {
	var result scanResult
	subs, err := s.store.ListChainWatcherSubscriptions(ctx)
	if err != nil {
		return result, err
	}
	result.SubscriptionCount = len(subs)
	if len(subs) == 0 {
		return result, nil
	}
	byAddress := make(map[string][]storage.ChainWatcherSubscription)
	for _, sub := range subs {
		byAddress[sub.Address] = append(byAddress[sub.Address], sub)
	}
	result.AddressCount = len(byAddress)
	minTimestamp := time.Now().Add(-s.cfg.Lookback).UnixMilli()
	fetch, err := s.tron.FetchGlobalUSDTTransfersWithMetrics(ctx, s.cfg.USDTContract, minTimestamp, s.cfg.GlobalPages)
	if err != nil {
		return result, err
	}
	result.observeFetch(fetch.Metrics)
	transfers := fetch.Transfers
	result.TransferCount = len(transfers)
	for _, transfer := range transfers {
		result.observeTransfer(transfer)
		matches, timings, err := s.recordTransferMatches(ctx, transfer, byAddress)
		if err != nil {
			return result, err
		}
		result.MatchCount += matches
		result.MatchDuration += timings.MatchDuration
		result.WriteDuration += timings.WriteDuration
	}
	return result, nil
}

func (s *Server) pollAddressOnce(ctx context.Context) (scanResult, error) {
	var result scanResult
	subs, err := s.store.ListChainWatcherSubscriptions(ctx)
	if err != nil {
		return result, err
	}
	result.SubscriptionCount = len(subs)
	if len(subs) == 0 {
		return result, nil
	}
	byAddress := make(map[string][]storage.ChainWatcherSubscription)
	for _, sub := range subs {
		byAddress[sub.Address] = append(byAddress[sub.Address], sub)
	}
	result.AddressCount = len(byAddress)
	defaultMinTimestamp := time.Now().Add(-s.cfg.Lookback).UnixMilli()
	addresses := s.selectAddressBatch(byAddress)
	result.AddressCount = len(addresses)
	addressResult, err := s.pollAddressTransfers(ctx, byAddress, addresses, defaultMinTimestamp)
	result.merge(addressResult)
	return result, err
}

func (s *Server) pollAddressTransfers(ctx context.Context, byAddress map[string][]storage.ChainWatcherSubscription, addresses []string, minTimestamp int64) (scanResult, error) {
	var result scanResult
	if len(byAddress) == 0 || len(addresses) == 0 {
		return result, nil
	}
	concurrency := s.cfg.AddressConcurrency
	if concurrency < 1 {
		concurrency = 1
	}
	sem := make(chan struct{}, concurrency)
	errCh := make(chan error, 1)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, address := range addresses {
		address := address
		select {
		case <-ctx.Done():
			wg.Wait()
			return result, ctx.Err()
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			addressMinTimestamp := s.addressMinTimestamp(address, minTimestamp)
			fetch, err := s.tron.FetchAddressUSDTTransfersSincePagesWithMetrics(ctx, address, s.cfg.USDTContract, 50, s.cfg.AddressPages, addressMinTimestamp)
			if err != nil {
				select {
				case errCh <- err:
				default:
				}
				return
			}
			transfers := fetch.Transfers
			local := scanResult{TransferCount: len(transfers)}
			local.observeFetch(fetch.Metrics)
			for _, transfer := range transfers {
				local.observeTransfer(transfer)
				matches, timings, err := s.recordTransferMatches(ctx, transfer, byAddress)
				if err != nil {
					select {
					case errCh <- err:
					default:
					}
					return
				}
				local.MatchCount += matches
				local.MatchDuration += timings.MatchDuration
				local.WriteDuration += timings.WriteDuration
			}
			s.updateAddressWatermark(address, local.MaxBlockTimestamp)
			mu.Lock()
			result.merge(local)
			mu.Unlock()
		}()
	}
	wg.Wait()
	select {
	case err := <-errCh:
		return result, err
	default:
		return result, nil
	}
}

func (s *Server) recordTransferMatches(ctx context.Context, transfer tron.Transfer, byAddress map[string][]storage.ChainWatcherSubscription) (int, recordTimings, error) {
	started := time.Now()
	candidates := append([]storage.ChainWatcherSubscription{}, byAddress[transfer.From]...)
	candidates = append(candidates, byAddress[transfer.To]...)
	if len(candidates) == 0 {
		return 0, recordTimings{MatchDuration: time.Since(started)}, nil
	}
	event := TransferEvent(transfer, "tronscan")
	deliveries := MatchTransfer(transfer, candidates)
	matchDuration := time.Since(started)
	if len(deliveries) == 0 {
		return 0, recordTimings{MatchDuration: matchDuration}, nil
	}
	writeStarted := time.Now()
	inserted, err := s.store.RecordChainWatcherMatches(ctx, event, deliveries, time.Now())
	return inserted, recordTimings{MatchDuration: matchDuration, WriteDuration: time.Since(writeStarted)}, err
}

type scanResult struct {
	TransferCount     int
	MatchCount        int
	SubscriptionCount int
	AddressCount      int
	MaxBlockTimestamp int64
	APICallCount      int
	PageCount         int
	APIWaitDuration   time.Duration
	APIFetchDuration  time.Duration
	ParseDuration     time.Duration
	MatchDuration     time.Duration
	WriteDuration     time.Duration
}

type recordTimings struct {
	MatchDuration time.Duration
	WriteDuration time.Duration
}

func (r *scanResult) observeTransfer(transfer tron.Transfer) {
	if transfer.BlockTimestamp > r.MaxBlockTimestamp {
		r.MaxBlockTimestamp = transfer.BlockTimestamp
	}
}

func (r *scanResult) merge(other scanResult) {
	r.TransferCount += other.TransferCount
	r.MatchCount += other.MatchCount
	if other.SubscriptionCount > r.SubscriptionCount {
		r.SubscriptionCount = other.SubscriptionCount
	}
	if other.AddressCount > r.AddressCount {
		r.AddressCount = other.AddressCount
	}
	if other.MaxBlockTimestamp > r.MaxBlockTimestamp {
		r.MaxBlockTimestamp = other.MaxBlockTimestamp
	}
	r.APICallCount += other.APICallCount
	r.PageCount += other.PageCount
	r.APIWaitDuration += other.APIWaitDuration
	r.APIFetchDuration += other.APIFetchDuration
	r.ParseDuration += other.ParseDuration
	r.MatchDuration += other.MatchDuration
	r.WriteDuration += other.WriteDuration
}

func (r *scanResult) observeFetch(metrics tron.FetchMetrics) {
	r.APICallCount += metrics.Calls
	r.PageCount += metrics.Pages
	r.APIWaitDuration += metrics.WaitDuration
	r.APIFetchDuration += metrics.APIDuration
	r.ParseDuration += metrics.ParseDuration
}

func (s *Server) selectAddressBatch(byAddress map[string][]storage.ChainWatcherSubscription) []string {
	if len(byAddress) == 0 {
		return nil
	}
	addresses := make([]string, 0, len(byAddress))
	for address := range byAddress {
		addresses = append(addresses, address)
	}
	sort.Strings(addresses)
	limit := s.cfg.AddressMaxPerTick
	if limit < 1 {
		limit = 1
	}
	if limit >= len(addresses) {
		s.addressMu.Lock()
		s.addressCursor = 0
		s.addressMu.Unlock()
		return addresses
	}
	s.addressMu.Lock()
	start := s.addressCursor % len(addresses)
	batch := make([]string, 0, limit)
	for i := 0; i < limit; i++ {
		batch = append(batch, addresses[(start+i)%len(addresses)])
	}
	s.addressCursor = (start + limit) % len(addresses)
	s.addressMu.Unlock()
	return batch
}

func (s *Server) shouldSkipAddressScan(now time.Time) bool {
	if s.cfg.PollInterval <= 0 {
		return false
	}
	global := s.status.snapshotScan("global", now)
	if global.LastStartedAt == nil {
		return false
	}
	nextGlobal := global.LastStartedAt.Add(s.cfg.PollInterval)
	guard := s.cfg.RequestInterval
	if guard <= 0 {
		guard = 250 * time.Millisecond
	}
	guard *= time.Duration(maxInt(1, s.cfg.AddressMaxPerTick))
	if guard < s.cfg.PollInterval/2 {
		guard = s.cfg.PollInterval / 2
	}
	return now.Add(guard).After(nextGlobal)
}

func (s *Server) addressMinTimestamp(address string, defaultMinTimestamp int64) int64 {
	s.watermarkMu.Lock()
	defer s.watermarkMu.Unlock()
	watermark := s.watermarks[address]
	if watermark <= 0 {
		return defaultMinTimestamp
	}
	minTimestamp := watermark - 30000
	if minTimestamp < defaultMinTimestamp {
		return defaultMinTimestamp
	}
	return minTimestamp
}

func (s *Server) updateAddressWatermark(address string, timestamp int64) {
	if timestamp <= 0 {
		return
	}
	s.watermarkMu.Lock()
	defer s.watermarkMu.Unlock()
	if timestamp > s.watermarks[address] {
		s.watermarks[address] = timestamp
	}
}

func (s *Server) tryStartScan(kind string) bool {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()
	if kind == "address" {
		if s.addressRunning {
			return false
		}
		s.addressRunning = true
		return true
	}
	if s.globalRunning {
		return false
	}
	s.globalRunning = true
	return true
}

func (s *Server) finishScan(kind string) {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()
	if kind == "address" {
		s.addressRunning = false
		return
	}
	s.globalRunning = false
}

type apiBackoff struct {
	mu       sync.Mutex
	until    time.Time
	failures int
}

func (b *apiBackoff) ready(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.until.IsZero() || !now.Before(b.until)
}

func (b *apiBackoff) record(err error, now time.Time) bool {
	httpErr, ok := tron.IsRateLimited(err)
	if !ok {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures++
	delay := httpErr.RetryAfter
	if delay <= 0 {
		delay = time.Duration(5*(1<<minInt(b.failures-1, 2))) * time.Second
		delay += time.Duration(now.UnixNano() % 1_000_000_000)
	}
	if delay > 30*time.Second {
		delay = 30 * time.Second
	}
	b.until = now.Add(delay)
	return true
}

func (b *apiBackoff) reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.until = time.Time{}
	b.failures = 0
}

func (b *apiBackoff) untilTime() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.until
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

type watcherStatus struct {
	mu                       sync.Mutex
	global                   scanStatus
	address                  scanStatus
	cleanup                  cleanupStatus
	addressSkippedNearGlobal int64
}

type scanStatus struct {
	lastStartedAt      time.Time
	lastSuccessAt      time.Time
	lastErrorAt        time.Time
	lastError          string
	lastDuration       time.Duration
	backoffUntil       time.Time
	lastBlockTimestamp int64
	lag                time.Duration
	scanCount          int64
	errorCount         int64
	overlapSkipped     int64
	transferCount      int
	matchCount         int
	subscriptionCount  int
	addressCount       int
	apiCallCount       int
	pageCount          int
	apiWaitDuration    time.Duration
	apiFetchDuration   time.Duration
	parseDuration      time.Duration
	matchDuration      time.Duration
	writeDuration      time.Duration
	recent             []scanRound
}

type scanRound struct {
	startedAt        time.Time
	success          bool
	err              string
	duration         time.Duration
	apiWaitDuration  time.Duration
	apiFetchDuration time.Duration
	parseDuration    time.Duration
	matchDuration    time.Duration
	writeDuration    time.Duration
	transferCount    int
	matchCount       int
	addressCount     int
	apiCallCount     int
	pageCount        int
}

type cleanupStatus struct {
	lastRunAt      time.Time
	matchedDeleted int64
	eventsDeleted  int64
	err            string
}

func (s *watcherStatus) recordScanSuccess(kind string, result scanResult, duration time.Duration, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	target := s.scan(kind)
	target.lastStartedAt = now.Add(-duration)
	target.lastSuccessAt = now
	target.lastError = ""
	target.lastDuration = duration
	target.backoffUntil = time.Time{}
	target.scanCount++
	target.transferCount = result.TransferCount
	target.matchCount = result.MatchCount
	target.subscriptionCount = result.SubscriptionCount
	target.addressCount = result.AddressCount
	target.apiCallCount = result.APICallCount
	target.pageCount = result.PageCount
	target.apiWaitDuration = result.APIWaitDuration
	target.apiFetchDuration = result.APIFetchDuration
	target.parseDuration = result.ParseDuration
	target.matchDuration = result.MatchDuration
	target.writeDuration = result.WriteDuration
	target.lastBlockTimestamp = result.MaxBlockTimestamp
	if result.MaxBlockTimestamp > 0 {
		lag := now.Sub(time.UnixMilli(result.MaxBlockTimestamp))
		if lag < 0 {
			lag = 0
		}
		target.lag = lag
	}
	target.appendRound(scanRound{
		startedAt:        target.lastStartedAt,
		success:          true,
		duration:         duration,
		apiWaitDuration:  result.APIWaitDuration,
		apiFetchDuration: result.APIFetchDuration,
		parseDuration:    result.ParseDuration,
		matchDuration:    result.MatchDuration,
		writeDuration:    result.WriteDuration,
		transferCount:    result.TransferCount,
		matchCount:       result.MatchCount,
		addressCount:     result.AddressCount,
		apiCallCount:     result.APICallCount,
		pageCount:        result.PageCount,
	})
}

func (s *watcherStatus) recordScanError(kind string, err error, backoffUntil time.Time, result scanResult, duration time.Duration, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	target := s.scan(kind)
	target.lastStartedAt = now.Add(-duration)
	target.lastErrorAt = now
	target.lastError = err.Error()
	target.lastDuration = duration
	target.backoffUntil = backoffUntil
	target.errorCount++
	target.apiCallCount = result.APICallCount
	target.pageCount = result.PageCount
	target.apiWaitDuration = result.APIWaitDuration
	target.apiFetchDuration = result.APIFetchDuration
	target.parseDuration = result.ParseDuration
	target.matchDuration = result.MatchDuration
	target.writeDuration = result.WriteDuration
	target.appendRound(scanRound{
		startedAt:        target.lastStartedAt,
		success:          false,
		err:              err.Error(),
		duration:         duration,
		apiWaitDuration:  result.APIWaitDuration,
		apiFetchDuration: result.APIFetchDuration,
		parseDuration:    result.ParseDuration,
		matchDuration:    result.MatchDuration,
		writeDuration:    result.WriteDuration,
		transferCount:    result.TransferCount,
		matchCount:       result.MatchCount,
		addressCount:     result.AddressCount,
		apiCallCount:     result.APICallCount,
		pageCount:        result.PageCount,
	})
}

func (s *watcherStatus) recordScanOverlap(kind string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scan(kind).overlapSkipped++
}

func (s *watcherStatus) recordCleanup(stats storage.ChainWatcherCleanupStats, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanup.lastRunAt = now
	s.cleanup.matchedDeleted = stats.MatchedDeleted
	s.cleanup.eventsDeleted = stats.EventsDeleted
	s.cleanup.err = ""
}

func (s *watcherStatus) recordCleanupError(err error, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanup.lastRunAt = now
	s.cleanup.err = err.Error()
}

func (s *watcherStatus) recordAddressSkippedNearGlobal() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addressSkippedNearGlobal++
}

func (s *watcherStatus) snapshotScan(kind string, now time.Time) ScanStatusResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scan(kind).response(now)
}

func (s *watcherStatus) response(now time.Time, staleAfter time.Duration, delivery storage.ChainWatcherDeliveryStats, addressCursor int, addressMaxPerTick int) StatusResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	global := s.global.response(now)
	address := s.address.response(now)
	ready := scanReady(s.global, staleAfter, now)
	status := "ready"
	if !ready {
		status = "degraded"
	}
	return StatusResponse{
		Status:                 status,
		Ready:                  ready,
		Now:                    now,
		StaleAfterMS:           staleAfter.Milliseconds(),
		Global:                 global,
		Address:                address,
		Deliveries:             deliveryResponse(delivery),
		RetentionCleanup:       s.cleanup.response(),
		AddressCursor:          addressCursor,
		AddressScanMaxPerTick:  addressMaxPerTick,
		AddressScanSkippedNear: s.addressSkippedNearGlobal,
	}
}

func (s *watcherStatus) scan(kind string) *scanStatus {
	if kind == "address" {
		return &s.address
	}
	return &s.global
}

func (s scanStatus) response(now time.Time) ScanStatusResponse {
	backoffRemaining := int64(0)
	if !s.backoffUntil.IsZero() && s.backoffUntil.After(now) {
		backoffRemaining = s.backoffUntil.Sub(now).Milliseconds()
	}
	return ScanStatusResponse{
		LastStartedAt:      timePtr(s.lastStartedAt),
		LastSuccessAt:      timePtr(s.lastSuccessAt),
		LastErrorAt:        timePtr(s.lastErrorAt),
		LastError:          s.lastError,
		LastDurationMS:     s.lastDuration.Milliseconds(),
		BackoffUntil:       timePtr(s.backoffUntil),
		BackoffRemainingMS: backoffRemaining,
		LastBlockTimestamp: s.lastBlockTimestamp,
		LagMS:              s.lag.Milliseconds(),
		ScanCount:          s.scanCount,
		ErrorCount:         s.errorCount,
		OverlapSkipped:     s.overlapSkipped,
		TransferCount:      s.transferCount,
		MatchCount:         s.matchCount,
		SubscriptionCount:  s.subscriptionCount,
		AddressCount:       s.addressCount,
		APICallCount:       s.apiCallCount,
		PageCount:          s.pageCount,
		APIWaitMS:          s.apiWaitDuration.Milliseconds(),
		APIFetchMS:         s.apiFetchDuration.Milliseconds(),
		ParseMS:            s.parseDuration.Milliseconds(),
		MatchMS:            s.matchDuration.Milliseconds(),
		WriteMS:            s.writeDuration.Milliseconds(),
		Recent:             scanRoundResponses(s.recent),
	}
}

func (s *scanStatus) appendRound(round scanRound) {
	s.recent = append(s.recent, round)
	if len(s.recent) > 5 {
		copy(s.recent, s.recent[len(s.recent)-5:])
		s.recent = s.recent[:5]
	}
}

func scanRoundResponses(rounds []scanRound) []ScanRoundResponse {
	if len(rounds) == 0 {
		return nil
	}
	out := make([]ScanRoundResponse, 0, len(rounds))
	for i := len(rounds) - 1; i >= 0; i-- {
		round := rounds[i]
		out = append(out, ScanRoundResponse{
			StartedAt:     timePtr(round.startedAt),
			Success:       round.success,
			Error:         round.err,
			DurationMS:    round.duration.Milliseconds(),
			APIWaitMS:     round.apiWaitDuration.Milliseconds(),
			APIFetchMS:    round.apiFetchDuration.Milliseconds(),
			ParseMS:       round.parseDuration.Milliseconds(),
			MatchMS:       round.matchDuration.Milliseconds(),
			WriteMS:       round.writeDuration.Milliseconds(),
			TransferCount: round.transferCount,
			MatchCount:    round.matchCount,
			AddressCount:  round.addressCount,
			APICallCount:  round.apiCallCount,
			PageCount:     round.pageCount,
		})
	}
	return out
}

func (s cleanupStatus) response() CleanupStatusResponse {
	return CleanupStatusResponse{
		LastRunAt:      timePtr(s.lastRunAt),
		MatchedDeleted: s.matchedDeleted,
		EventsDeleted:  s.eventsDeleted,
		Error:          s.err,
	}
}

func scanReady(status scanStatus, staleAfter time.Duration, now time.Time) bool {
	if status.lastSuccessAt.IsZero() {
		return false
	}
	if staleAfter <= 0 {
		staleAfter = 5 * time.Second
	}
	if now.Sub(status.lastSuccessAt) > staleAfter {
		return false
	}
	return status.backoffUntil.IsZero() || !status.backoffUntil.After(now)
}

func deliveryResponse(stats storage.ChainWatcherDeliveryStats) DeliveryStatusResponse {
	return DeliveryStatusResponse{
		PendingCount:       stats.PendingCount,
		DeliveringCount:    stats.DeliveringCount,
		OldestPendingAt:    stats.OldestPendingAt,
		OldestPendingAgeMS: stats.OldestPendingAgeMS,
	}
}

func timePtr(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	return &value
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "process": "alive"})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	status := s.statusResponse(r.Context(), time.Now())
	if !status.Ready {
		writeJSON(w, http.StatusServiceUnavailable, status)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.statusResponse(r.Context(), time.Now()))
}

func (s *Server) statusResponse(ctx context.Context, now time.Time) StatusResponse {
	var delivery storage.ChainWatcherDeliveryStats
	if s.store != nil {
		stats, err := s.store.ChainWatcherDeliveryStats(ctx, s.cfg.Lookback, now)
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("chain watcher delivery stats: %v", err)
		} else {
			delivery = stats
		}
	}
	s.addressMu.Lock()
	cursor := s.addressCursor
	s.addressMu.Unlock()
	return s.status.response(now, s.sourceStaleAfter(), delivery, cursor, s.cfg.AddressMaxPerTick)
}

func (s *Server) sourceStaleAfter() time.Duration {
	interval := s.cfg.PollInterval
	if interval <= 0 {
		interval = time.Second
	}
	staleAfter := interval * 5
	if staleAfter < 5*time.Second {
		staleAfter = 5 * time.Second
	}
	if s.cfg.Lookback > 0 && staleAfter > s.cfg.Lookback {
		staleAfter = s.cfg.Lookback
	}
	return staleAfter
}

func (s *Server) handleUpsertSubscription(w http.ResponseWriter, r *http.Request) {
	botID, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	var req SubscriptionRequest
	if !readJSON(w, r, &req) {
		return
	}
	if err := s.store.UpsertChainWatcherSubscription(r.Context(), ToSubscription(botID, req), time.Now()); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleDeleteSubscription(w http.ResponseWriter, r *http.Request) {
	botID, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	var req DeleteSubscriptionRequest
	if !readJSON(w, r, &req) {
		return
	}
	if err := s.store.RemoveChainWatcherSubscription(r.Context(), botID, req.ChatID, req.OwnerUserID, req.Address, time.Now()); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleSyncSubscriptions(w http.ResponseWriter, r *http.Request) {
	botID, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	var req SyncRequest
	if !readJSON(w, r, &req) {
		return
	}
	subs := make([]storage.ChainWatcherSubscription, 0, len(req.Subscriptions))
	for _, sub := range req.Subscriptions {
		subs = append(subs, ToSubscription(botID, sub))
	}
	if err := s.store.ReplaceChainWatcherSubscriptions(r.Context(), botID, subs, time.Now()); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleClaimEvents(w http.ResponseWriter, r *http.Request) {
	botID, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	var req ClaimRequest
	if !readJSON(w, r, &req) {
		return
	}
	events, err := s.store.ClaimChainWatcherMatchedEvents(r.Context(), botID, req.Limit, s.cfg.ClaimLease, s.cfg.Lookback, time.Now())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp := ClaimResponse{Events: make([]MatchedEvent, 0, len(events))}
	for _, event := range events {
		resp.Events = append(resp.Events, FromMatchedStorage(event))
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleAckEvents(w http.ResponseWriter, r *http.Request) {
	botID, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	var req AckRequest
	if !readJSON(w, r, &req) {
		return
	}
	if err := s.store.AckChainWatcherMatchedEvents(r.Context(), botID, req.DeliveryIDs, time.Now()); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (string, bool) {
	botID := strings.TrimSpace(r.Header.Get("X-Bot-ID"))
	token := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if botID == "" || token == "" {
		writeError(w, http.StatusUnauthorized, "missing chain watcher credentials")
		return "", false
	}
	ok, err := s.store.AuthenticateChainWatcherBot(r.Context(), botID, token)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return "", false
	}
	if !ok {
		writeError(w, http.StatusUnauthorized, "invalid chain watcher credentials")
		return "", false
	}
	return botID, true
}

func readJSON(w http.ResponseWriter, r *http.Request, out any) bool {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return false
	}
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
