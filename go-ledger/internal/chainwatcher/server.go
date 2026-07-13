package chainwatcher

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/config"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/tron"
)

type Server struct {
	cfg                 config.ChainWatcherConfig
	store               *storage.Store
	tron                *tron.Client
	http                *http.Server
	globalBackoff       apiBackoff
	status              watcherStatus
	scanMu              sync.Mutex
	globalRunning       int
	catchupRunning      bool
	globalRoundSeq      int64
	lastGlobalScheduled time.Time
	subMu               sync.RWMutex
	subscriptions       []storage.ChainWatcherSubscription
	subByAddress        map[string][]storage.ChainWatcherSubscription
	subDirty            bool
	gapWake             chan struct{}
	gapOwner            string
}

type expandTask struct {
	AnchorID     string
	Cutoff       int64
	MinTimestamp int64
	StartPage    int
}

func NewServer(cfg config.ChainWatcherConfig, store *storage.Store, tronClient *tron.Client) *Server {
	s := &Server{
		cfg: cfg, store: store, tron: tronClient, subDirty: true,
		gapWake: make(chan struct{}, 1), gapOwner: fmt.Sprintf("watcher-%d", time.Now().UnixNano()),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/readyz", s.handleReady)
	mux.HandleFunc("/status", s.handleStatus)
	mux.HandleFunc("/v1/subscriptions/upsert", s.handleUpsertSubscription)
	mux.HandleFunc("/v1/subscriptions/delete", s.handleDeleteSubscription)
	mux.HandleFunc("/v1/subscriptions/sync", s.handleSyncSubscriptions)
	mux.HandleFunc("/v1/events/claim", s.handleClaimEvents)
	mux.HandleFunc("/v1/events/ack", s.handleAckEvents)
	mux.HandleFunc("/v1/admin/keys", s.handleAdminKeys)
	mux.HandleFunc("/v1/admin/keys/upsert", s.handleAdminKeyUpsert)
	mux.HandleFunc("/v1/admin/keys/delete", s.handleAdminKeyDelete)
	mux.HandleFunc("/v1/admin/keys/probe", s.handleAdminKeyProbe)
	s.http = &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s
}

type adminKeyRequest struct {
	APIKey      string `json:"api_key"`
	Fingerprint string `json:"fingerprint"`
	Enabled     *bool  `json:"enabled,omitempty"`
}

func (s *Server) authorizeAdmin(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	expected := strings.TrimSpace(s.cfg.AdminToken)
	provided := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if expected == "" {
		http.Error(w, `{"error":"chain watcher admin API is disabled"}`, http.StatusServiceUnavailable)
		return false
	}
	if len(expected) != len(provided) || subtle.ConstantTimeCompare([]byte(expected), []byte(provided)) != 1 {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return false
	}
	return true
}

func (s *Server) handleAdminKeys(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	_ = json.NewEncoder(w).Encode(s.tron.KeyPoolStatus(time.Now()))
}

func (s *Server) handleAdminKeyUpsert(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	var request adminKeyRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&request); err != nil || strings.TrimSpace(request.APIKey) == "" {
		http.Error(w, `{"error":"api_key is required"}`, http.StatusBadRequest)
		return
	}
	enabled := true
	if request.Enabled != nil {
		enabled = *request.Enabled
	}
	fingerprint := tron.FingerprintAPIKey(request.APIKey)
	if err := s.store.UpsertTronscanAPIKey(r.Context(), fingerprint, strings.TrimSpace(request.APIKey), enabled, time.Now()); err != nil {
		http.Error(w, `{"error":`+strconv.Quote(err.Error())+`}`, http.StatusConflict)
		return
	}
	if err := s.tron.RefreshKeyRegistry(r.Context()); err != nil {
		http.Error(w, `{"error":"key saved but registry refresh failed"}`, http.StatusInternalServerError)
		return
	}
	_ = s.tron.RequestKeyProbe(r.Context(), fingerprint, enabled)
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"fingerprint": fingerprint, "enabled": enabled, "probe_queued": enabled})
}

func (s *Server) handleAdminKeyDelete(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	var request adminKeyRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&request); err != nil || strings.TrimSpace(request.Fingerprint) == "" {
		http.Error(w, `{"error":"fingerprint is required"}`, http.StatusBadRequest)
		return
	}
	if err := s.store.DeleteTronscanAPIKey(r.Context(), strings.TrimSpace(request.Fingerprint)); err != nil {
		http.Error(w, `{"error":"delete failed"}`, http.StatusInternalServerError)
		return
	}
	if err := s.tron.RefreshKeyRegistry(r.Context()); err != nil {
		http.Error(w, `{"error":"key deleted but registry refresh failed"}`, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAdminKeyProbe(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	var request adminKeyRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&request); err != nil || strings.TrimSpace(request.Fingerprint) == "" {
		http.Error(w, `{"error":"fingerprint is required"}`, http.StatusBadRequest)
		return
	}
	enable := request.Enabled != nil && *request.Enabled
	if err := s.tron.RequestKeyProbe(r.Context(), request.Fingerprint, enable); err != nil {
		http.Error(w, `{"error":`+strconv.Quote(err.Error())+`}`, http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"fingerprint": request.Fingerprint, "probe_queued": true})
}

func (s *Server) Run(ctx context.Context) error {
	for botID, secret := range s.cfg.BotCredentials {
		if err := s.store.UpsertChainWatcherBot(ctx, botID, secret, time.Now()); err != nil {
			return err
		}
	}
	go s.globalLoop(ctx)
	if s.cfg.CatchupEnabled {
		go s.gapLoop(ctx)
	}
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
		s.recordOverlapMetric("global")
		s.markCatchup(ctx, "realtime_overlap_skipped")
		return
	}
	roundID := s.allocateGlobalRound(time.Now())
	go func() {
		defer s.finishScan("global")
		s.pollGlobalSafely(ctx, roundID)
	}()
}

func (s *Server) pollGlobalSafely(ctx context.Context, roundID int64) {
	started := time.Now()
	result, err := s.pollGlobalOnce(ctx, roundID)
	duration := time.Since(started)
	if err != nil && !errors.Is(err, context.Canceled) {
		s.enqueueFailedPageGaps(result)
		s.markCatchup(ctx, "realtime_scan_failed")
		s.recordMetric("global", false, result)
		if s.status.isStaleOutcome("global", result.RoundID) {
			s.status.recordHistoricalError("global", err, result, duration, time.Now())
			return
		}
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
	if result.CutoffTimestamp > 0 && len(result.FailedPages) == 0 {
		anchor := result.HeadEventID
		if anchor == "" {
			anchor = result.PreviousAnchorID
		}
		dbCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := s.store.AdvanceChainWatcherRealtimeWatermark(dbCtx, result.CutoffTimestamp, anchor, time.Now()); err != nil {
			s.status.recordScanError("global", err, time.Time{}, result, duration, time.Now())
			log.Printf("chain watcher advance realtime watermark: %v", err)
			return
		}
	}
	s.status.recordScanSuccess("global", result, duration, time.Now())
	watermarkCtx, watermarkCancel := context.WithTimeout(context.Background(), 2*time.Second)
	watermark, watermarkErr := s.store.GetChainWatcherWatermark(watermarkCtx)
	watermarkCancel()
	if watermarkErr == nil && watermark.Timestamp == 0 {
		s.markCatchup(ctx, "startup_continuity_baseline")
	}
	if result.PreviousAnchorID != "" && !result.AnchorFound {
		s.markCatchup(ctx, "realtime_anchor_missing")
		s.enqueueExpandGap(result)
	}
	s.recordMetric("global", true, result)
}

func (s *Server) markCatchup(ctx context.Context, reason string) {
	if s.store == nil {
		return
	}
	markCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	now := time.Now()
	if err := s.store.MarkChainWatcherCatchupRequired(markCtx, reason, now); err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("mark chain watcher catchup required: %v", err)
	}
	from := now.Add(-s.cfg.Lookback).UnixMilli()
	if watermark, err := s.store.GetChainWatcherWatermark(markCtx); err == nil && watermark.Timestamp > 0 {
		from = watermark.Timestamp
	}
	to := now.Add(-s.cfg.CatchupOverlap).UnixMilli()
	if realtime, err := s.store.GetChainWatcherRealtimeWatermark(markCtx); err == nil && realtime.Timestamp > 0 {
		to = realtime.Timestamp - s.cfg.CatchupOverlap.Milliseconds()
	}
	if to > from {
		_, _ = s.store.EnqueueChainWatcherGap(markCtx, storage.ChainWatcherGapTask{
			Kind: "window", Source: "watcher", Priority: 10, Reason: reason,
			FromTimestamp: from, ToTimestamp: to,
		}, now)
	}
	select {
	case s.gapWake <- struct{}{}:
	default:
	}
}

func (s *Server) enqueueFailedPageGaps(result scanResult) {
	if s.store == nil || len(result.FailedPages) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for _, task := range failedPageGapTasks(result) {
		_, _ = s.store.EnqueueChainWatcherGap(ctx, task, time.Now())
	}
	select {
	case s.gapWake <- struct{}{}:
	default:
	}
}

func failedPageGapTasks(result scanResult) []storage.ChainWatcherGapTask {
	tasks := make([]storage.ChainWatcherGapTask, 0, len(result.FailedPages))
	for _, failure := range result.FailedPages {
		tasks = append(tasks, storage.ChainWatcherGapTask{
			Kind: "page", Source: "watcher", Priority: 0, Reason: failure.Error,
			FromTimestamp: result.MinTimestamp, ToTimestamp: result.CutoffTimestamp,
			StartPage: failure.Page, EndPage: failure.Page + 1, NextPage: failure.Page,
		})
	}
	return tasks
}

func (s *Server) enqueueExpandGap(result scanResult) {
	if s.store == nil || result.CutoffTimestamp <= result.MinTimestamp {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = s.store.EnqueueChainWatcherGap(ctx, storage.ChainWatcherGapTask{
		Kind: "expand", Source: "watcher", Priority: 1, Reason: "anchor_missing",
		FromTimestamp: result.MinTimestamp, ToTimestamp: result.CutoffTimestamp,
		StartPage: s.cfg.GlobalPages, EndPage: s.cfg.GlobalExpandPageLimit,
		NextPage: s.cfg.GlobalPages, AnchorEventID: result.PreviousAnchorID,
	}, time.Now())
	select {
	case s.gapWake <- struct{}{}:
	default:
	}
}

func (s *Server) gapLoop(ctx context.Context) {
	interval := s.cfg.CatchupInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		s.processOneGap(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.gapWake:
		}
	}
}

func (s *Server) processOneGap(ctx context.Context) {
	if !s.tryStartScan("catchup") {
		s.status.recordScanOverlap("catchup")
		s.recordOverlapMetric("catchup")
		return
	}
	defer s.finishScan("catchup")
	claimCtx, claimCancel := context.WithTimeout(context.Background(), 2*time.Second)
	task, ok, err := s.store.ClaimChainWatcherGap(claimCtx, s.gapOwner, "watcher", 15*time.Second, time.Now())
	claimCancel()
	if err != nil || !ok {
		return
	}
	if task.Priority > 1 && s.shouldSkipCatchup(time.Now()) {
		dbCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, _ = s.store.ReleaseChainWatcherGap(dbCtx, task.ID, task.LeaseGeneration, task.LeaseOwner, "realtime_deadline_priority", time.Now())
		cancel()
		s.status.recordCatchupDeferred("realtime_deadline_priority")
		s.wakeGapAfter(s.cfg.PollInterval)
		return
	}
	started := time.Now()
	result, processErr := s.processGapTask(ctx, task)
	duration := time.Since(started)
	lane := "catchup"
	if task.Kind == "expand" || task.Kind == "page" {
		lane = "expand"
	}
	if processErr != nil {
		dbCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, _ = s.store.ReleaseChainWatcherGap(dbCtx, task.ID, task.LeaseGeneration, task.LeaseOwner, processErr.Error(), time.Now())
		cancel()
		s.status.recordScanError(lane, processErr, time.Time{}, result, duration, time.Now())
		s.recordMetric(lane, false, result)
		s.wakeGapAfter(time.Second)
		return
	}
	s.status.recordScanSuccess(lane, result, duration, time.Now())
	s.recordMetric(lane, true, result)
	clearCtx, clearCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer clearCancel()
	if gaps, statsErr := s.store.ChainWatcherGapStats(clearCtx, time.Now()); statsErr == nil && gaps.PendingCount == 0 && gaps.LeasedCount == 0 {
		_ = s.store.ClearChainWatcherCatchupRequired(clearCtx, time.Now())
	} else if statsErr == nil && gaps.PendingCount > 0 {
		s.wakeGap()
	}
}

func (s *Server) wakeGap() {
	select {
	case s.gapWake <- struct{}{}:
	default:
	}
}

func (s *Server) wakeGapAfter(delay time.Duration) {
	if delay <= 0 {
		delay = time.Second
	}
	time.AfterFunc(delay, s.wakeGap)
}

func (s *Server) processGapTask(ctx context.Context, task storage.ChainWatcherGapTask) (scanResult, error) {
	var result scanResult
	dbReadCtx, dbReadCancel := context.WithTimeout(context.Background(), 2*time.Second)
	_, byAddress, err := s.loadSubscriptions(dbReadCtx)
	dbReadCancel()
	if err != nil {
		return result, err
	}
	page := task.NextPage
	if page < task.StartPage {
		page = task.StartPage
	}
	apiCtx, cancel := context.WithTimeout(ctx, s.cfg.MainScanTimeout)
	fetch, fetchErr := s.tron.FetchGlobalUSDTTransfersRangeWithMetrics(apiCtx, s.cfg.USDTContract, task.FromTimestamp, task.ToTimestamp, page, 1)
	cancel()
	result.observeFetch(fetch.Metrics)
	result.FailedPages = append(result.FailedPages, fetch.FailedPages...)
	if fetchErr != nil {
		return result, fetchErr
	}
	for _, transfer := range fetch.Transfers {
		result.observeTransfer(transfer)
		dbCtx, dbCancel := context.WithTimeout(context.Background(), 2*time.Second)
		matches, timings, matchErr := s.recordTransferMatchesSource(dbCtx, transfer, byAddress, task.Kind)
		dbCancel()
		if matchErr != nil {
			return result, matchErr
		}
		result.TransferCount++
		result.MatchCount += matches
		result.MatchDuration += timings.MatchDuration
		result.WriteDuration += timings.WriteDuration
		if task.AnchorEventID != "" && EventID(transfer) == task.AnchorEventID {
			result.AnchorFound = true
		}
	}
	dbCtx, dbCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer dbCancel()
	if task.Kind == "page" || result.AnchorFound {
		_, err = s.store.CompleteChainWatcherGap(dbCtx, task.ID, task.LeaseGeneration, task.LeaseOwner, time.Now())
		return result, err
	}
	fullPage := len(fetch.Transfers) >= 50
	if task.Kind == "expand" {
		next := page + 1
		if fullPage && next < task.EndPage {
			_, err = s.store.YieldChainWatcherGap(dbCtx, task.ID, task.LeaseGeneration, task.LeaseOwner, next, "", time.Now())
			return result, err
		}
		_, err = s.store.CompleteChainWatcherGap(dbCtx, task.ID, task.LeaseGeneration, task.LeaseOwner, time.Now())
		if err == nil {
			_, err = s.store.EnqueueChainWatcherGap(dbCtx, storage.ChainWatcherGapTask{
				Kind: "window", Source: "watcher", Priority: 2, Reason: "expand_anchor_not_found",
				FromTimestamp: task.FromTimestamp, ToTimestamp: task.ToTimestamp,
			}, time.Now())
		}
		return result, err
	}
	if fullPage {
		next := page + 1
		if next >= s.cfg.GlobalExpandPageLimit && task.ToTimestamp-task.FromTimestamp > 1 {
			middle := task.FromTimestamp + (task.ToTimestamp-task.FromTimestamp)/2
			_, err = s.store.SplitChainWatcherGapWindow(dbCtx, task, middle, time.Now())
			return result, err
		}
		if next >= s.cfg.GlobalExpandPageLimit {
			_, err = s.store.ReleaseChainWatcherGap(dbCtx, task.ID, task.LeaseGeneration, task.LeaseOwner, "page_limit_at_minimum_window", time.Now())
			if err == nil {
				err = errors.New("catchup page limit reached at minimum time window")
			}
			return result, err
		}
		_, err = s.store.YieldChainWatcherGap(dbCtx, task.ID, task.LeaseGeneration, task.LeaseOwner, next, "", time.Now())
		return result, err
	}
	completed, err := s.store.CompleteChainWatcherGap(dbCtx, task.ID, task.LeaseGeneration, task.LeaseOwner, time.Now())
	if err == nil && completed {
		err = s.store.AdvanceChainWatcherWatermark(dbCtx, task.ToTimestamp, result.MaxEventID, task.Source, time.Now())
		if err == nil && task.Source == "fallback" {
			if open, countErr := s.store.CountOpenChainWatcherGaps(dbCtx, "fallback", time.Now()); countErr != nil {
				err = countErr
			} else if open == 0 {
				err = s.store.AdvanceChainWatcherFallbackHead(dbCtx, task.ToTimestamp, task.HeadEventID, time.Now())
			}
		}
	}
	return result, err
}

func (s *Server) recordMetric(lane string, success bool, result scanResult) {
	if s.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = s.store.RecordChainWatcherMetricMinute(ctx, lane, success, result.APICallCount,
		result.APIFetchDuration, result.ParseDuration, result.MatchDuration, result.WriteDuration, 0, time.Now())
}

func (s *Server) recordOverlapMetric(lane string) {
	if s.store == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.store.RecordChainWatcherOverlapMinute(ctx, lane, time.Now())
	}()
}

func (s *Server) scanCatchupWindow(ctx context.Context, from, to int64, byAddress map[string][]storage.ChainWatcherSubscription, budget *int) (int64, scanResult, error) {
	var result scanResult
	if from >= to {
		return to, result, nil
	}
	if *budget < 1 {
		return from, result, nil
	}
	pageLimit := s.cfg.CatchupPages
	if pageLimit > *budget {
		pageLimit = *budget
	}
	fetch, err := s.tron.FetchGlobalUSDTTransfersWindowWithMetrics(ctx, s.cfg.USDTContract, from, to, pageLimit)
	*budget -= fetch.Metrics.Calls
	result.observeFetch(fetch.Metrics)
	result.observePageLimit(fetch.Metrics, pageLimit, 50)
	if err != nil {
		return from, result, err
	}
	if result.PageLimitReached && to-from > 1000 {
		middle := from + (to-from)/2
		leftAdvanced, left, leftErr := s.scanCatchupWindow(ctx, from, middle, byAddress, budget)
		result.merge(left)
		if leftErr != nil || leftAdvanced < middle {
			return leftAdvanced, result, leftErr
		}
		rightAdvanced, right, rightErr := s.scanCatchupWindow(ctx, middle, to, byAddress, budget)
		result.merge(right)
		return rightAdvanced, result, rightErr
	}
	if result.PageLimitReached {
		return from, result, nil
	}
	for _, transfer := range fetch.Transfers {
		result.observeTransfer(transfer)
		matches, timings, matchErr := s.recordTransferMatchesSource(ctx, transfer, byAddress, "catchup")
		if matchErr != nil {
			return from, result, matchErr
		}
		result.TransferCount++
		result.MatchCount += matches
		result.MatchDuration += timings.MatchDuration
		result.WriteDuration += timings.WriteDuration
	}
	return to, result, nil
}

func (s *Server) pollGlobalOnce(ctx context.Context, roundID int64) (scanResult, error) {
	result := scanResult{RoundID: roundID}
	dbReadCtx, dbReadCancel := context.WithTimeout(context.Background(), 2*time.Second)
	subs, byAddress, err := s.loadSubscriptions(dbReadCtx)
	if err != nil {
		dbReadCancel()
		return result, err
	}
	result.SubscriptionCount = len(subs)
	if len(subs) == 0 {
		dbReadCancel()
		return result, nil
	}
	result.AddressCount = len(byAddress)
	previous, err := s.store.GetChainWatcherRealtimeWatermark(dbReadCtx)
	dbReadCancel()
	if err != nil {
		return result, err
	}
	result.PreviousAnchorID = previous.TxHash
	result.AnchorFound = result.PreviousAnchorID == ""
	cutoff := time.Now()
	result.CutoffTimestamp = cutoff.UnixMilli()
	minTimestamp := cutoff.Add(-s.cfg.Lookback).UnixMilli()
	result.MinTimestamp = minTimestamp
	apiCtx, cancel := context.WithTimeout(ctx, s.cfg.MainScanTimeout)
	fetch, fetchErr := s.tron.FetchGlobalUSDTTransfersAtWithMetrics(apiCtx, s.cfg.USDTContract, minTimestamp, result.CutoffTimestamp, s.cfg.GlobalPages)
	cancel()
	result.observeFetch(fetch.Metrics)
	result.FailedPages = append(result.FailedPages, fetch.FailedPages...)
	result.observePageLimit(fetch.Metrics, s.cfg.GlobalPages, 50)
	transfers := fetch.Transfers
	result.TransferCount = len(transfers)
	result.HeadEventID, result.AnchorFound = AnchorCoverage(transfers, result.PreviousAnchorID)
	for _, transfer := range transfers {
		result.observeTransfer(transfer)
		dbCtx, dbCancel := context.WithTimeout(context.Background(), 2*time.Second)
		matches, timings, err := s.recordTransferMatches(dbCtx, transfer, byAddress)
		dbCancel()
		if err != nil {
			return result, err
		}
		result.MatchCount += matches
		result.MatchDuration += timings.MatchDuration
		result.WriteDuration += timings.WriteDuration
	}
	return result, fetchErr
}

func (s *Server) pollExpandOnce(ctx context.Context, task expandTask) (scanResult, error) {
	var result scanResult
	result.PreviousAnchorID = task.AnchorID
	_, byAddress, err := s.loadSubscriptions(ctx)
	if err != nil {
		return result, err
	}
	for page := task.StartPage; page < s.cfg.GlobalExpandPageLimit; page++ {
		fetch, fetchErr := s.tron.FetchGlobalUSDTTransfersRangeWithMetrics(ctx, s.cfg.USDTContract, task.MinTimestamp, task.Cutoff, page, 1)
		result.observeFetch(fetch.Metrics)
		if fetchErr != nil {
			return result, fetchErr
		}
		for _, transfer := range fetch.Transfers {
			if _, found := AnchorCoverage([]tron.Transfer{transfer}, task.AnchorID); found {
				result.AnchorFound = true
			}
			result.observeTransfer(transfer)
			matches, timings, matchErr := s.recordTransferMatchesSource(ctx, transfer, byAddress, "expand")
			if matchErr != nil {
				return result, matchErr
			}
			result.TransferCount++
			result.MatchCount += matches
			result.MatchDuration += timings.MatchDuration
			result.WriteDuration += timings.WriteDuration
		}
		if result.AnchorFound || len(fetch.Transfers) < 50 {
			return result, nil
		}
	}
	result.PageLimitReached = true
	return result, nil
}

func (s *Server) recordTransferMatches(ctx context.Context, transfer tron.Transfer, byAddress map[string][]storage.ChainWatcherSubscription) (int, recordTimings, error) {
	return s.recordTransferMatchesSource(ctx, transfer, byAddress, "tronscan")
}

func (s *Server) recordTransferMatchesSource(ctx context.Context, transfer tron.Transfer, byAddress map[string][]storage.ChainWatcherSubscription, source string) (int, recordTimings, error) {
	started := time.Now()
	candidates := append([]storage.ChainWatcherSubscription{}, byAddress[transfer.From]...)
	candidates = append(candidates, byAddress[transfer.To]...)
	if len(candidates) == 0 {
		return 0, recordTimings{MatchDuration: time.Since(started)}, nil
	}
	event := TransferEvent(transfer, source)
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
	RoundID           int64
	TransferCount     int
	MatchCount        int
	SubscriptionCount int
	AddressCount      int
	MaxBlockTimestamp int64
	MaxTxHash         string
	MaxEventID        string
	CutoffTimestamp   int64
	MinTimestamp      int64
	PreviousAnchorID  string
	HeadEventID       string
	AnchorFound       bool
	APICallCount      int
	PageCount         int
	PageLimitReached  bool
	APIWaitDuration   time.Duration
	APIFetchDuration  time.Duration
	ParseDuration     time.Duration
	MatchDuration     time.Duration
	WriteDuration     time.Duration
	FailedPages       []tron.PageFailure
}

type recordTimings struct {
	MatchDuration time.Duration
	WriteDuration time.Duration
}

func (r *scanResult) observeTransfer(transfer tron.Transfer) {
	if transfer.BlockTimestamp > r.MaxBlockTimestamp {
		r.MaxBlockTimestamp = transfer.BlockTimestamp
		r.MaxTxHash = transfer.Hash
		r.MaxEventID = EventID(transfer)
	} else if transfer.BlockTimestamp == r.MaxBlockTimestamp && EventID(transfer) > r.MaxEventID {
		r.MaxEventID = EventID(transfer)
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
		r.MaxTxHash = other.MaxTxHash
		r.MaxEventID = other.MaxEventID
	} else if other.MaxBlockTimestamp == r.MaxBlockTimestamp && other.MaxEventID > r.MaxEventID {
		r.MaxEventID = other.MaxEventID
	}
	if other.CutoffTimestamp > r.CutoffTimestamp {
		r.CutoffTimestamp = other.CutoffTimestamp
	}
	if r.MinTimestamp == 0 || (other.MinTimestamp > 0 && other.MinTimestamp < r.MinTimestamp) {
		r.MinTimestamp = other.MinTimestamp
	}
	r.APICallCount += other.APICallCount
	r.PageCount += other.PageCount
	r.PageLimitReached = r.PageLimitReached || other.PageLimitReached
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

func (r *scanResult) observePageLimit(metrics tron.FetchMetrics, configuredPages int, pageLimit int) {
	if configuredPages < 1 || pageLimit < 1 {
		return
	}
	if metrics.Pages >= configuredPages && !metrics.ReachedWindow && metrics.LastPageRows >= pageLimit {
		r.PageLimitReached = true
	}
}

func (s *Server) loadSubscriptions(ctx context.Context) ([]storage.ChainWatcherSubscription, map[string][]storage.ChainWatcherSubscription, error) {
	s.subMu.RLock()
	if !s.subDirty {
		subs := s.subscriptions
		byAddress := s.subByAddress
		s.subMu.RUnlock()
		return subs, byAddress, nil
	}
	s.subMu.RUnlock()

	subs, err := s.store.ListChainWatcherSubscriptions(ctx)
	if err != nil {
		return nil, nil, err
	}
	byAddress := make(map[string][]storage.ChainWatcherSubscription, len(subs))
	for _, sub := range subs {
		byAddress[sub.Address] = append(byAddress[sub.Address], sub)
	}
	s.subMu.Lock()
	s.subscriptions = subs
	s.subByAddress = byAddress
	s.subDirty = false
	s.subMu.Unlock()
	return subs, byAddress, nil
}

func (s *Server) invalidateSubscriptions() {
	s.subMu.Lock()
	s.subDirty = true
	s.subMu.Unlock()
}

func (s *Server) shouldSkipCatchup(now time.Time) bool {
	if s.cfg.PollInterval <= 0 {
		return false
	}
	s.scanMu.Lock()
	lastScheduled := s.lastGlobalScheduled
	s.scanMu.Unlock()
	if lastScheduled.IsZero() {
		return false
	}
	nextGlobal := lastScheduled.Add(s.cfg.PollInterval)
	guard := s.cfg.RequestInterval
	if guard < 100*time.Millisecond {
		guard = 100 * time.Millisecond
	}
	return now.Add(guard).After(nextGlobal)
}

func (s *Server) tryStartScan(kind string) bool {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()
	if kind == "catchup" {
		if s.catchupRunning {
			return false
		}
		s.catchupRunning = true
		return true
	}
	limit := s.cfg.MainMaxInflight
	if limit < 1 {
		limit = 1
	}
	if s.globalRunning >= limit {
		return false
	}
	s.globalRunning++
	return true
}

func (s *Server) allocateGlobalRound(now time.Time) int64 {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()
	s.globalRoundSeq++
	s.lastGlobalScheduled = now
	return s.globalRoundSeq
}

func (s *Server) finishScan(kind string) {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()
	if kind == "catchup" {
		s.catchupRunning = false
		return
	}
	if s.globalRunning > 0 {
		s.globalRunning--
	}
}

func (s *Server) globalInflight() int {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()
	return s.globalRunning
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
		if delay > 30*time.Second {
			delay = 30 * time.Second
		}
	} else if delay > 24*time.Hour {
		delay = 24 * time.Hour
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
	mu                    sync.Mutex
	global                scanStatus
	catchup               scanStatus
	expand                scanStatus
	cleanup               cleanupStatus
	catchupDeferredReason string
	catchupDeferredCount  int64
}

type scanStatus struct {
	roundID            int64
	latestOutcomeRound int64
	latestOutcomeOK    bool
	latestOutcomeAt    time.Time
	lastStartedAt      time.Time
	lastSuccessAt      time.Time
	lastErrorAt        time.Time
	lastError          string
	lastErrorClass     string
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
	pageLimitReached   bool
	cutoffTimestamp    int64
	anchorFound        bool
	previousAnchorID   string
	headEventID        string
	apiWaitDuration    time.Duration
	apiFetchDuration   time.Duration
	parseDuration      time.Duration
	matchDuration      time.Duration
	writeDuration      time.Duration
	recent             []scanRound
}

type scanRound struct {
	roundID          int64
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
	pageLimitReached bool
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
	target.recordOutcome(result.RoundID, true, now)
	target.lastStartedAt = now.Add(-duration)
	target.roundID = result.RoundID
	target.lastSuccessAt = now
	target.lastError = ""
	target.lastErrorClass = ""
	target.lastDuration = duration
	target.backoffUntil = time.Time{}
	target.scanCount++
	target.transferCount = result.TransferCount
	target.matchCount = result.MatchCount
	target.subscriptionCount = result.SubscriptionCount
	target.addressCount = result.AddressCount
	target.apiCallCount = result.APICallCount
	target.pageCount = result.PageCount
	target.pageLimitReached = result.PageLimitReached
	target.cutoffTimestamp = result.CutoffTimestamp
	target.anchorFound = result.AnchorFound
	target.previousAnchorID = result.PreviousAnchorID
	target.headEventID = result.HeadEventID
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
		roundID:          result.RoundID,
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
		pageLimitReached: result.PageLimitReached,
	})
}

func (s *watcherStatus) recordScanError(kind string, err error, backoffUntil time.Time, result scanResult, duration time.Duration, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	target := s.scan(kind)
	target.recordOutcome(result.RoundID, false, now)
	target.lastStartedAt = now.Add(-duration)
	target.roundID = result.RoundID
	target.lastErrorAt = now
	target.lastError = err.Error()
	target.lastErrorClass = classifyScanError(err)
	target.lastDuration = duration
	target.backoffUntil = backoffUntil
	target.errorCount++
	target.apiCallCount = result.APICallCount
	target.pageCount = result.PageCount
	target.pageLimitReached = result.PageLimitReached
	target.apiWaitDuration = result.APIWaitDuration
	target.apiFetchDuration = result.APIFetchDuration
	target.parseDuration = result.ParseDuration
	target.matchDuration = result.MatchDuration
	target.writeDuration = result.WriteDuration
	target.appendRound(scanRound{
		roundID:          result.RoundID,
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
		pageLimitReached: result.PageLimitReached,
	})
}

func (s *watcherStatus) recordScanOverlap(kind string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scan(kind).overlapSkipped++
}

func (s *watcherStatus) isStaleOutcome(kind string, roundID int64) bool {
	if roundID <= 0 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scan(kind).latestOutcomeRound > roundID
}

func (s *watcherStatus) recordHistoricalError(kind string, err error, result scanResult, duration time.Duration, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	target := s.scan(kind)
	target.errorCount++
	target.appendRound(scanRound{
		roundID: result.RoundID, startedAt: now.Add(-duration), success: false, err: err.Error(), duration: duration,
		apiWaitDuration: result.APIWaitDuration, apiFetchDuration: result.APIFetchDuration,
		parseDuration: result.ParseDuration, matchDuration: result.MatchDuration, writeDuration: result.WriteDuration,
		transferCount: result.TransferCount, matchCount: result.MatchCount, addressCount: result.AddressCount,
		apiCallCount: result.APICallCount, pageCount: result.PageCount, pageLimitReached: result.PageLimitReached,
	})
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

func (s *watcherStatus) recordCatchupDeferred(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.catchupDeferredReason = reason
	s.catchupDeferredCount++
}

func (s *watcherStatus) snapshotScan(kind string, now time.Time) ScanStatusResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scan(kind).response(now)
}

func (s *watcherStatus) response(now time.Time, staleAfter time.Duration, delivery storage.ChainWatcherDeliveryStats) StatusResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	global := s.global.response(now)
	catchup := s.catchup.response(now)
	expand := s.expand.response(now)
	ready := scanReady(s.global, staleAfter, now)
	status := "ready"
	if !ready {
		status = "degraded"
	}
	return StatusResponse{
		Status:                status,
		Ready:                 ready,
		Now:                   now,
		StaleAfterMS:          staleAfter.Milliseconds(),
		Global:                global,
		Catchup:               catchup,
		Expand:                expand,
		Deliveries:            deliveryResponse(delivery),
		RetentionCleanup:      s.cleanup.response(),
		CatchupDeferredReason: s.catchupDeferredReason,
		CatchupDeferredCount:  s.catchupDeferredCount,
	}
}

func (s *watcherStatus) scan(kind string) *scanStatus {
	if kind == "catchup" {
		return &s.catchup
	}
	if kind == "expand" {
		return &s.expand
	}
	return &s.global
}

func (s scanStatus) response(now time.Time) ScanStatusResponse {
	backoffRemaining := int64(0)
	if !s.backoffUntil.IsZero() && s.backoffUntil.After(now) {
		backoffRemaining = s.backoffUntil.Sub(now).Milliseconds()
	}
	return ScanStatusResponse{
		RoundID:            s.roundID,
		LastStartedAt:      timePtr(s.lastStartedAt),
		LastSuccessAt:      timePtr(s.lastSuccessAt),
		LastErrorAt:        timePtr(s.lastErrorAt),
		LastError:          s.lastError,
		LastErrorClass:     s.lastErrorClass,
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
		PageLimitReached:   s.pageLimitReached,
		CutoffTimestamp:    s.cutoffTimestamp,
		AnchorFound:        s.anchorFound,
		PreviousAnchorID:   shortHash(s.previousAnchorID),
		HeadEventID:        shortHash(s.headEventID),
		APIWaitMS:          s.apiWaitDuration.Milliseconds(),
		APIFetchMS:         s.apiFetchDuration.Milliseconds(),
		ParseMS:            s.parseDuration.Milliseconds(),
		MatchMS:            s.matchDuration.Milliseconds(),
		WriteMS:            s.writeDuration.Milliseconds(),
		Recent:             scanRoundResponses(s.recent),
	}
}

func classifyScanError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var unavailable *tron.KeyPoolUnavailableError
	if errors.As(err, &unavailable) {
		return "all_keys_unavailable"
	}
	var httpErr *tron.HTTPError
	if errors.As(err, &httpErr) {
		switch {
		case httpErr.StatusCode == http.StatusTooManyRequests:
			return "upstream_429"
		case httpErr.StatusCode == http.StatusUnauthorized || httpErr.StatusCode == http.StatusForbidden:
			return "key_auth"
		case httpErr.StatusCode >= 500:
			return "upstream_5xx"
		}
	}
	return "other"
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
			RoundID:          round.roundID,
			StartedAt:        timePtr(round.startedAt),
			Success:          round.success,
			Error:            round.err,
			DurationMS:       round.duration.Milliseconds(),
			APIWaitMS:        round.apiWaitDuration.Milliseconds(),
			APIFetchMS:       round.apiFetchDuration.Milliseconds(),
			ParseMS:          round.parseDuration.Milliseconds(),
			MatchMS:          round.matchDuration.Milliseconds(),
			WriteMS:          round.writeDuration.Milliseconds(),
			TransferCount:    round.transferCount,
			MatchCount:       round.matchCount,
			AddressCount:     round.addressCount,
			APICallCount:     round.apiCallCount,
			PageCount:        round.pageCount,
			PageLimitReached: round.pageLimitReached,
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
	if !status.latestOutcomeAt.IsZero() {
		return status.latestOutcomeOK
	}
	return status.backoffUntil.IsZero() || !status.backoffUntil.After(now)
}

func (s *scanStatus) recordOutcome(roundID int64, success bool, now time.Time) {
	if roundID > 0 && s.latestOutcomeRound > roundID {
		return
	}
	if roundID > 0 {
		s.latestOutcomeRound = roundID
	}
	s.latestOutcomeOK = success
	s.latestOutcomeAt = now
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
	response := s.readinessResponse(r.Context(), time.Now())
	if !response.Ready {
		writeJSON(w, http.StatusServiceUnavailable, response)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) readinessResponse(ctx context.Context, now time.Time) ReadyStatusResponse {
	base := s.status.response(now, s.sourceStaleAfter(), storage.ChainWatcherDeliveryStats{})
	response := ReadyStatusResponse{Status: base.Status, Ready: base.Ready, Now: now}
	if s.tron != nil && s.tron.KeyPoolStatus(now).AvailableCount == 0 {
		response.Ready = false
		response.Status = "DEGRADED/NO_KEYS"
	}
	if s.store == nil {
		return response
	}
	dbCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	state, err := s.store.GetChainWatcherReadiness(dbCtx, now)
	cancel()
	if err != nil {
		response.Ready = false
		response.Status = "degraded/readiness_db"
		response.CatchupLagUnknown = true
		return response
	}
	response.OpenGapCount = state.OpenGapCount
	response.LeasedGapCount = state.LeasedGapCount
	response.WatchAddressCount = state.WatchAddressCount
	safeEnd := state.RealtimeTimestamp - s.cfg.CatchupOverlap.Milliseconds()
	if state.CursorTimestamp == 0 {
		response.CatchupLagUnknown = true
	} else if safeEnd > state.CursorTimestamp {
		response.CatchupLagSeconds = (safeEnd - state.CursorTimestamp) / 1000
	}
	response.ContinuityReady = state.WatchAddressCount == 0 || (state.CursorTimestamp > 0 && !state.CatchupRequired && state.OpenGapCount == 0 && state.LeasedGapCount == 0)
	if !response.ContinuityReady {
		response.Ready = false
		if response.Status == "ready" {
			response.Status = "degraded/continuity"
		}
	}
	return response
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeAdmin(w, r) {
		return
	}
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
	response := s.status.response(now, s.sourceStaleAfter(), delivery)
	response.MainInflightRounds = s.globalInflight()
	response.MainInflightLimit = s.cfg.MainMaxInflight
	if s.tron != nil {
		response.TronscanKeys = s.tron.KeyPoolStatus(now)
		if response.TronscanKeys.AvailableCount == 0 {
			response.Ready = false
			response.Status = "DEGRADED/NO_KEYS"
		}
	}
	if s.store != nil {
		if aggregates, err := s.store.ChainWatcherMetricAggregates(ctx, now.Add(-72*time.Hour)); err == nil {
			response.Metrics72H = make([]MetricAggregateResponse, 0, len(aggregates))
			for _, item := range aggregates {
				response.Metrics72H = append(response.Metrics72H, MetricAggregateResponse{
					Lane: item.Lane, SuccessCount: item.SuccessCount, ErrorCount: item.ErrorCount,
					RequestCount: item.RequestCount, APIMS: item.APIMS, ParseMS: item.ParseMS,
					MatchMS: item.MatchMS, WriteMS: item.WriteMS, OverlapCount: item.OverlapCount,
				})
			}
		}
		var cursorTimestamp, realtimeTimestamp int64
		if watermark, err := s.store.GetChainWatcherWatermark(ctx); err == nil {
			cursorTimestamp = watermark.Timestamp
			lag := int64(0)
			if watermark.Timestamp > 0 {
				lag = (now.UnixMilli() - watermark.Timestamp) / 1000
				if lag < 0 {
					lag = 0
				}
			}
			response.GlobalWatermark = WatermarkStatusResponse{Timestamp: watermark.Timestamp, EventID: shortHash(watermark.TxHash), Source: watermark.Source, UpdatedAt: timePtr(watermark.UpdatedAt), LagSeconds: lag}
			response.CatchupLagSeconds = lag
			if watermark.Timestamp == 0 {
				response.CatchupLagUnknown = true
			}
		}
		if realtime, err := s.store.GetChainWatcherRealtimeWatermark(ctx); err == nil {
			realtimeTimestamp = realtime.Timestamp
			lag := int64(0)
			if realtime.Timestamp > 0 {
				lag = (now.UnixMilli() - realtime.Timestamp) / 1000
				if lag < 0 {
					lag = 0
				}
			}
			response.RealtimeWatermark = WatermarkStatusResponse{Timestamp: realtime.Timestamp, EventID: shortHash(realtime.TxHash), Source: "realtime", UpdatedAt: timePtr(realtime.UpdatedAt), LagSeconds: lag}
		}
		response.CatchupSafeEnd = realtimeTimestamp - s.cfg.CatchupOverlap.Milliseconds()
		if response.CatchupSafeEnd < 0 {
			response.CatchupSafeEnd = 0
		}
		if cursorTimestamp > 0 && response.CatchupSafeEnd > cursorTimestamp {
			response.CatchupLagSeconds = (response.CatchupSafeEnd - cursorTimestamp) / 1000
		}
		if state, err := s.store.GetChainWatcherCatchupState(ctx); err == nil {
			response.CatchupRequired, response.CatchupReason = state.Required, state.Reason
		}
		if gaps, err := s.store.ChainWatcherGapStats(ctx, now); err == nil {
			response.OpenGapCount = gaps.PendingCount
			response.LeasedGapCount = gaps.LeasedCount
		}
		subscriptionsKnown := false
		if _, byAddress, err := s.loadSubscriptions(ctx); err == nil {
			response.WatchAddressCount = len(byAddress)
			subscriptionsKnown = true
		}
		response.ContinuityReady = subscriptionsKnown && (response.WatchAddressCount == 0 || (cursorTimestamp > 0 && response.OpenGapCount == 0 && response.LeasedGapCount == 0 && !response.CatchupRequired))
		if !response.ContinuityReady {
			response.Ready = false
			if response.Status == "ready" {
				response.Status = "degraded/continuity"
			}
		}
		if response.TronscanKeys.CompensationBudgetRPS > 0 {
			response.CatchupETASeconds = int64(float64(response.CatchupLagSeconds) / response.TronscanKeys.CompensationBudgetRPS)
		}
		if lease, err := s.store.GetChainWatcherFallbackLease(ctx, "public-no-key"); err == nil {
			catchupLag := int64(0)
			if lease.CatchupTo > lease.CatchupFrom {
				catchupLag = (lease.CatchupTo - lease.CatchupFrom) / 1000
			}
			leader := ""
			if lease.LeaseUntil.After(now) {
				leader = lease.HolderID
			}
			response.Fallback = FallbackStatusResponse{
				Mode: lease.Mode, LastWatcherSuccess: lease.LastWatcherSuccess, FallbackLeader: leader,
				FallbackStartedAt: lease.StartedAt, FallbackRequests: lease.FallbackRequests,
				Fallback429: lease.Fallback429, CatchupFrom: lease.CatchupFrom, CatchupTo: lease.CatchupTo,
				CatchupLagSeconds: catchupLag, CatchupPages: lease.CatchupPages,
				CatchupRequests: lease.FallbackRequests, CatchupBudgetUsed: lease.CatchupBudgetUsed,
				Recovering: lease.Mode == "RECOVERING", LeaseUntil: timePtr(lease.LeaseUntil),
			}
		}
	}
	return response
}

func shortHash(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 12 {
		return value
	}
	return value[:6] + "..." + value[len(value)-6:]
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
	s.invalidateSubscriptions()
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
	s.invalidateSubscriptions()
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
	s.invalidateSubscriptions()
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
