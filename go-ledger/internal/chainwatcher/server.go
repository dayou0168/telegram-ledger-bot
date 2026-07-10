package chainwatcher

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
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
}

func NewServer(cfg config.ChainWatcherConfig, store *storage.Store, tronClient *tron.Client) *Server {
	s := &Server{cfg: cfg, store: store, tron: tronClient}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
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
		s.pollGlobalSafely(ctx)
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
			s.pollAddressSafely(ctx)
		}
	}
}

func (s *Server) cleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		if err := s.store.CleanupChainWatcherRetention(ctx, s.cfg.Lookback, time.Now()); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("chain watcher cleanup: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Server) pollGlobalSafely(ctx context.Context) {
	if !s.globalBackoff.ready(time.Now()) {
		return
	}
	if err := s.pollGlobalOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
		if s.globalBackoff.record(err, time.Now()) {
			log.Printf("chain watcher global scan rate limited: %v", err)
			return
		}
		log.Printf("chain watcher global scan: %v", err)
		return
	}
	s.globalBackoff.reset()
}

func (s *Server) pollAddressSafely(ctx context.Context) {
	if !s.addressBackoff.ready(time.Now()) {
		return
	}
	if err := s.pollAddressOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
		if s.addressBackoff.record(err, time.Now()) {
			log.Printf("chain watcher address scan rate limited: %v", err)
			return
		}
		log.Printf("chain watcher address scan: %v", err)
		return
	}
	s.addressBackoff.reset()
}

func (s *Server) pollGlobalOnce(ctx context.Context) error {
	subs, err := s.store.ListChainWatcherSubscriptions(ctx)
	if err != nil {
		return err
	}
	if len(subs) == 0 {
		return nil
	}
	byAddress := make(map[string][]storage.ChainWatcherSubscription)
	for _, sub := range subs {
		byAddress[sub.Address] = append(byAddress[sub.Address], sub)
	}
	minTimestamp := time.Now().Add(-s.cfg.Lookback).UnixMilli()
	transfers, err := s.tron.FetchGlobalUSDTTransfers(ctx, s.cfg.USDTContract, minTimestamp, s.cfg.GlobalPages)
	if err != nil {
		return err
	}
	for _, transfer := range transfers {
		if err := s.recordTransferMatches(ctx, transfer, byAddress); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) pollAddressOnce(ctx context.Context) error {
	subs, err := s.store.ListChainWatcherSubscriptions(ctx)
	if err != nil {
		return err
	}
	if len(subs) == 0 {
		return nil
	}
	byAddress := make(map[string][]storage.ChainWatcherSubscription)
	for _, sub := range subs {
		byAddress[sub.Address] = append(byAddress[sub.Address], sub)
	}
	minTimestamp := time.Now().Add(-s.cfg.Lookback).UnixMilli()
	return s.pollAddressTransfers(ctx, byAddress, minTimestamp)
}

func (s *Server) pollAddressTransfers(ctx context.Context, byAddress map[string][]storage.ChainWatcherSubscription, minTimestamp int64) error {
	if len(byAddress) == 0 {
		return nil
	}
	concurrency := s.cfg.AddressConcurrency
	if concurrency < 1 {
		concurrency = 1
	}
	sem := make(chan struct{}, concurrency)
	errCh := make(chan error, 1)
	var wg sync.WaitGroup
	for address := range byAddress {
		address := address
		select {
		case <-ctx.Done():
			wg.Wait()
			return ctx.Err()
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			transfers, err := s.tron.FetchAddressUSDTTransfersSincePages(ctx, address, s.cfg.USDTContract, 50, s.cfg.AddressPages, minTimestamp)
			if err != nil {
				select {
				case errCh <- err:
				default:
				}
				return
			}
			for _, transfer := range transfers {
				if err := s.recordTransferMatches(ctx, transfer, byAddress); err != nil {
					select {
					case errCh <- err:
					default:
					}
					return
				}
			}
		}()
	}
	wg.Wait()
	select {
	case err := <-errCh:
		return err
	default:
		return nil
	}
}

func (s *Server) recordTransferMatches(ctx context.Context, transfer tron.Transfer, byAddress map[string][]storage.ChainWatcherSubscription) error {
	candidates := append([]storage.ChainWatcherSubscription{}, byAddress[transfer.From]...)
	candidates = append(candidates, byAddress[transfer.To]...)
	if len(candidates) == 0 {
		return nil
	}
	event := TransferEvent(transfer, "tronscan")
	deliveries := MatchTransfer(transfer, candidates)
	if len(deliveries) == 0 {
		return nil
	}
	_, err := s.store.RecordChainWatcherMatches(ctx, event, deliveries, time.Now())
	return err
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

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
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
