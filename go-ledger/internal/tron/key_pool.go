package tron

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	sharedLimitWindow    = time.Minute
	longRateLimitBackoff = 15 * time.Minute
	MaxConfiguredKeys    = 10
)

var ErrCompensationDeferred = errors.New("tronscan compensation deferred")

type CompensationDeferredError struct{ Reason string }

func (e *CompensationDeferredError) Error() string {
	return "tronscan compensation deferred: " + e.Reason
}

func (e *CompensationDeferredError) Unwrap() error { return ErrCompensationDeferred }

func IsCompensationDeferred(err error) bool {
	return errors.Is(err, ErrCompensationDeferred)
}

type KeyRegistryRecord struct {
	Fingerprint               string
	APIKey                    string
	Enabled                   bool
	Health                    string
	Reason                    string
	ConsecutiveFailures       int
	ConsecutiveAuthFailures   int
	ConsecutiveProbeSuccesses int
	CooldownUntil             time.Time
	NextProbeAt               time.Time
	LastUsedAt                time.Time
	LastSuccessAt             time.Time
	LastFailureAt             time.Time
	LastErrorClass            string
}

type KeyRegistryStore interface {
	ListTronscanAPIKeys(context.Context) ([]KeyRegistryRecord, error)
	UpsertTronscanAPIKey(context.Context, string, string, bool, time.Time) error
	DeleteTronscanAPIKey(context.Context, string) error
	UpdateTronscanAPIKeyState(context.Context, KeyRegistryRecord, time.Time) error
}

type RequestSource string

const (
	RequestSourceMain         RequestSource = "main"
	RequestSourceCompensation RequestSource = "compensation"
	RequestSourceExpand       RequestSource = "expand"
	RequestSourceOther        RequestSource = "other"
)

type KeyPoolOptions struct {
	MinInterval          time.Duration
	BudgetZone           *time.Location
	AuthCooldown         time.Duration
	AuthProbeInterval    time.Duration
	InvalidProbeInterval time.Duration
	BlockedProbeInterval time.Duration
	CompensationMaxRPS   float64
	UsageStore           KeyUsageStore
	AllowAnonymous       bool
	PublicFallback       bool
}

type KeyUsageRecord struct {
	Fingerprint       string
	BudgetDay         string
	RequestCount      int
	MainRequestCount  int
	CompRequestCount  int
	OtherRequestCount int
	FailoverCount     int
	RateLimitCount    int
	AuthErrorCount    int
	LastHTTPStatus    int
	Last429At         time.Time
	CooldownUntil     time.Time
	DisabledUntil     time.Time
}

type KeyUsageStore interface {
	LoadTronscanKeyUsage(context.Context, []string, string) (map[string]KeyUsageRecord, error)
	ReserveTronscanKeyRequest(context.Context, string, string, RequestSource, bool, int, time.Time) (KeyUsageRecord, bool, error)
	RecordTronscanKeyResult(context.Context, string, string, int, time.Time, time.Time, time.Time) (KeyUsageRecord, error)
}

type KeyPoolStatus struct {
	KeyCount                   int            `json:"key_count"`
	AvailableCount             int            `json:"available_count"`
	EnabledCount               int            `json:"enabled_count"`
	HealthyCount               int            `json:"healthy_count"`
	CooldownCount              int            `json:"cooldown_count"`
	ExhaustedCount             int            `json:"exhausted_count"`
	InvalidCount               int            `json:"invalid_count"`
	RequiredMainKeyCount       int            `json:"required_main_key_count"`
	MainCapacitySafe           bool           `json:"main_capacity_safe"`
	CapacityWarning            string         `json:"capacity_warning,omitempty"`
	BudgetTimezone             string         `json:"budget_timezone"`
	NextBudgetResetAt          time.Time      `json:"next_budget_reset_at"`
	RateLimitedKeys            int            `json:"rate_limited_keys"`
	PossibleSharedLimit        bool           `json:"possible_shared_limit"`
	TodayMainRequests          int            `json:"today_main_requests"`
	TodayCompRequests          int            `json:"today_compensation_requests"`
	TodayOtherRequests         int            `json:"today_other_requests"`
	TodayFailoverRequests      int            `json:"today_failover_requests"`
	PersistenceError           string         `json:"persistence_error,omitempty"`
	CompensationDeferred       bool           `json:"compensation_deferred"`
	CompensationDeferredReason string         `json:"compensation_deferred_reason,omitempty"`
	RealtimeReservedRPS        float64        `json:"realtime_reserved_rps"`
	CompensationBudgetRPS      float64        `json:"compensation_budget_rps"`
	CompensationTokens         float64        `json:"compensation_tokens"`
	PerKeyLimitRPS             float64        `json:"per_key_limit_rps"`
	Keys                       []APIKeyStatus `json:"keys"`
}

type APIKeyStatus struct {
	Index               int        `json:"index"`
	Fingerprint         string     `json:"fingerprint"`
	TodayRequests       int        `json:"today_requests"`
	MainRequests        int        `json:"main_requests"`
	CompRequests        int        `json:"compensation_requests"`
	OtherRequests       int        `json:"other_requests"`
	FailoverRequests    int        `json:"failover_requests"`
	RateLimitCount      int        `json:"rate_limit_count"`
	AuthErrorCount      int        `json:"auth_error_count"`
	LastHTTPStatus      int        `json:"last_http_status,omitempty"`
	Last429At           *time.Time `json:"last_429_at,omitempty"`
	CooldownUntil       *time.Time `json:"cooldown_until,omitempty"`
	DisabledUntil       *time.Time `json:"disabled_until,omitempty"`
	NextRequestAt       *time.Time `json:"next_request_at,omitempty"`
	Available           bool       `json:"available"`
	UnavailableFor      string     `json:"unavailable_for,omitempty"`
	Enabled             bool       `json:"enabled"`
	Health              string     `json:"health"`
	Reason              string     `json:"reason,omitempty"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	NextProbeAt         *time.Time `json:"next_probe_at,omitempty"`
	LastUsedAt          *time.Time `json:"last_used_at,omitempty"`
	LastSuccessAt       *time.Time `json:"last_success_at,omitempty"`
	LastFailureAt       *time.Time `json:"last_failure_at,omitempty"`
	LastErrorClass      string     `json:"last_error,omitempty"`
}

type apiKeyState struct {
	key                       string
	fingerprint               string
	budgetDay                 string
	todayRequests             int
	mainRequests              int
	compRequests              int
	otherRequests             int
	failoverRequests          int
	rateLimitCount            int
	authErrorCount            int
	consecutive429            int
	lastHTTPStatus            int
	last429At                 time.Time
	cooldownUntil             time.Time
	disabledUntil             time.Time
	nextRequestAt             time.Time
	enabled                   bool
	health                    string
	reason                    string
	consecutiveFailures       int
	consecutiveAuthFailures   int
	consecutiveProbeSuccesses int
	nextProbeAt               time.Time
	lastUsedAt                time.Time
	lastSuccessAt             time.Time
	lastFailureAt             time.Time
	lastErrorClass            string
}

type apiKeyLease struct {
	index       int
	fingerprint string
	key         string
	probe       bool
}

type keyPool struct {
	mu                     sync.Mutex
	keys                   []apiKeyState
	mainNext               int
	compNext               int
	minInterval            time.Duration
	budgetZone             *time.Location
	authCooldown           time.Duration
	authProbeInterval      time.Duration
	invalidProbeInterval   time.Duration
	blockedProbeInterval   time.Duration
	usageStore             KeyUsageStore
	registryStore          KeyRegistryStore
	loadedDay              string
	lastStoreError         string
	mainPages              int
	pollInterval           time.Duration
	nextMainAt             time.Time
	mainPriorityUntil      time.Time
	realtimeReserved       map[string]time.Time
	compTokens             float64
	compLastRefill         time.Time
	compBudgetRPS          float64
	lastCompDeferredReason string
	compBusy               map[string]int
	compMaxRPS             float64
	compPressure           float64
	publicFallback         bool
	now                    func() time.Time
}

func newKeyPool(keys []string, opts KeyPoolOptions) *keyPool {
	zone := opts.BudgetZone
	if zone == nil {
		zone = time.UTC
	}
	if opts.MinInterval < 200*time.Millisecond {
		opts.MinInterval = 200 * time.Millisecond
	}
	if opts.AuthCooldown <= 0 {
		opts.AuthCooldown = 24 * time.Hour
	}
	if opts.AuthProbeInterval <= 0 {
		opts.AuthProbeInterval = 5 * time.Second
	}
	if opts.InvalidProbeInterval <= 0 {
		opts.InvalidProbeInterval = 30 * time.Minute
	}
	if opts.BlockedProbeInterval <= 0 {
		opts.BlockedProbeInterval = time.Hour
	}
	if opts.CompensationMaxRPS <= 0 {
		opts.CompensationMaxRPS = 8
	}
	keys = normalizeAPIKeys(keys)
	if len(keys) == 0 && opts.AllowAnonymous {
		keys = []string{""}
	}
	states := make([]apiKeyState, 0, len(keys))
	for _, key := range keys {
		states = append(states, apiKeyState{key: key, fingerprint: keyFingerprint(key), enabled: true, health: "healthy"})
	}
	registryStore, _ := opts.UsageStore.(KeyRegistryStore)
	return &keyPool{
		keys:                 states,
		minInterval:          opts.MinInterval,
		budgetZone:           zone,
		authCooldown:         opts.AuthCooldown,
		authProbeInterval:    opts.AuthProbeInterval,
		invalidProbeInterval: opts.InvalidProbeInterval,
		blockedProbeInterval: opts.BlockedProbeInterval,
		usageStore:           opts.UsageStore,
		registryStore:        registryStore,
		realtimeReserved:     make(map[string]time.Time),
		compBusy:             make(map[string]int),
		compMaxRPS:           opts.CompensationMaxRPS,
		compPressure:         1,
		publicFallback:       opts.PublicFallback,
		now:                  time.Now,
	}
}

func normalizeAPIKeys(keys []string) []string {
	seen := make(map[string]struct{}, len(keys))
	out := make([]string, 0, len(keys))
	for _, raw := range keys {
		for _, value := range strings.FieldsFunc(raw, func(r rune) bool {
			return r == ',' || r == '\n' || r == '\r'
		}) {
			key := strings.TrimSpace(value)
			if key == "" {
				continue
			}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, key)
		}
	}
	return out
}

func keyFingerprint(key string) string {
	if key == "" {
		return "anonymous"
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:12]
}

func FingerprintAPIKey(key string) string {
	return keyFingerprint(strings.TrimSpace(key))
}

func (p *keyPool) configureMainBudget(pages int, interval time.Duration) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.mainPages = pages
	p.pollInterval = interval
	p.mu.Unlock()
}

func (p *keyPool) setMinInterval(interval time.Duration) {
	if p == nil {
		return
	}
	if interval < 200*time.Millisecond {
		interval = 200 * time.Millisecond
	}
	p.mu.Lock()
	p.minInterval = interval
	p.mu.Unlock()
}

func (p *keyPool) setCompensationPressure(lag, target, maximum time.Duration) {
	if p == nil {
		return
	}
	factor := 1.0
	if lag <= 0 {
		factor = 0
	} else if target > 0 && lag <= target {
		factor = 0.25
	} else if maximum > target && lag < maximum {
		factor = 0.25 + 0.75*float64(lag-target)/float64(maximum-target)
	}
	p.mu.Lock()
	p.compPressure = factor
	p.mu.Unlock()
}

func (p *keyPool) restore(ctx context.Context) error {
	if p == nil || p.usageStore == nil {
		return nil
	}
	now := p.now()
	day := budgetDay(now, p.budgetZone)
	p.mu.Lock()
	fingerprints := make([]string, len(p.keys))
	for i := range p.keys {
		fingerprints[i] = p.keys[i].fingerprint
	}
	p.mu.Unlock()
	records, err := p.usageStore.LoadTronscanKeyUsage(ctx, fingerprints, day)
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		p.lastStoreError = err.Error()
		return err
	}
	p.applyRecordsLocked(records, day)
	p.loadedDay = day
	p.lastStoreError = ""
	return nil
}

func (p *keyPool) refreshRegistry(ctx context.Context) error {
	if p == nil || p.registryStore == nil {
		return nil
	}
	records, err := p.registryStore.ListTronscanAPIKeys(ctx)
	if err != nil {
		return err
	}
	if len(records) > MaxConfiguredKeys {
		return fmt.Errorf("Tronscan API key registry contains %d keys; maximum is %d", len(records), MaxConfiguredKeys)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	existing := make(map[string]apiKeyState, len(p.keys))
	for _, state := range p.keys {
		existing[state.fingerprint] = state
	}
	next := make([]apiKeyState, 0, len(records))
	for _, record := range records {
		state := existing[record.Fingerprint]
		state.key = record.APIKey
		state.fingerprint = record.Fingerprint
		state.enabled = record.Enabled
		state.health = record.Health
		state.reason = record.Reason
		state.consecutiveFailures = record.ConsecutiveFailures
		state.consecutiveAuthFailures = record.ConsecutiveAuthFailures
		state.consecutiveProbeSuccesses = record.ConsecutiveProbeSuccesses
		state.cooldownUntil = record.CooldownUntil
		state.nextProbeAt = record.NextProbeAt
		state.lastUsedAt = record.LastUsedAt
		state.lastSuccessAt = record.LastSuccessAt
		state.lastFailureAt = record.LastFailureAt
		state.lastErrorClass = record.LastErrorClass
		next = append(next, state)
	}
	p.keys = next
	if len(p.keys) > 0 {
		p.mainNext %= len(p.keys)
		p.compNext %= len(p.keys)
	} else {
		p.mainNext = 0
		p.compNext = 0
	}
	return nil
}

func (p *keyPool) seedRegistry(ctx context.Context, configured []string) error {
	if p == nil || p.registryStore == nil {
		return nil
	}
	existing, err := p.registryStore.ListTronscanAPIKeys(ctx)
	if err != nil {
		return err
	}
	known := make(map[string]struct{}, len(existing))
	for _, record := range existing {
		known[record.Fingerprint] = struct{}{}
	}
	for _, key := range normalizeAPIKeys(configured) {
		fingerprint := keyFingerprint(key)
		if _, ok := known[fingerprint]; ok {
			continue
		}
		if err := p.registryStore.UpsertTronscanAPIKey(ctx, fingerprint, key, true, p.now()); err != nil {
			return err
		}
		known[fingerprint] = struct{}{}
	}
	return p.refreshRegistry(ctx)
}

func (p *keyPool) ensureDayLoaded(ctx context.Context, now time.Time) error {
	if p == nil || p.usageStore == nil {
		return nil
	}
	day := budgetDay(now, p.budgetZone)
	p.mu.Lock()
	loaded := p.loadedDay == day
	p.mu.Unlock()
	if loaded {
		return nil
	}
	return p.restore(ctx)
}

func (p *keyPool) lease(ctx context.Context, source RequestSource, excluded map[string]struct{}, failover bool) (apiKeyLease, error) {
	if p == nil {
		return apiKeyLease{}, fmt.Errorf("tronscan key pool is not configured")
	}
	now := p.now()
	if err := p.ensureDayLoaded(ctx, now); err != nil {
		return apiKeyLease{}, fmt.Errorf("load tronscan key usage: %w", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.resetBudgetDaysLocked(now)
	if source == RequestSourceCompensation {
		if reason := p.takeCompensationTokenLocked(now); reason != "" {
			p.lastCompDeferredReason = reason
			return apiKeyLease{}, &CompensationDeferredError{Reason: reason}
		}
	}
	start := p.compNext
	if source == RequestSourceMain {
		start = p.mainNext
	}
	index, reason := p.selectLeaseLocked(now, start, excluded, source, failover)
	if index < 0 {
		if source == RequestSourceCompensation && reason == "realtime key reservation" {
			p.compTokens++
			p.lastCompDeferredReason = "realtime_keys_reserved"
			return apiKeyLease{}, &CompensationDeferredError{Reason: "realtime_keys_reserved"}
		}
		retryAfter := 30 * time.Second
		if reason == "daily budget exhausted" || reason == "keys disabled" {
			retryAfter = nextBudgetReset(now, p.budgetZone).Sub(now)
		}
		return apiKeyLease{}, &HTTPError{
			StatusCode: 429,
			Body:       "all configured API keys unavailable: " + reason,
			RetryAfter: retryAfter,
		}
	}
	if !failover {
		if source == RequestSourceMain {
			p.mainNext = (index + 1) % len(p.keys)
		} else {
			p.compNext = (index + 1) % len(p.keys)
		}
	}
	if source == RequestSourceCompensation {
		p.lastCompDeferredReason = ""
	}
	return p.leaseForStateLocked(index), nil
}

func (p *keyPool) mainLeases(ctx context.Context, count int) ([]apiKeyLease, error) {
	if count < 1 {
		return nil, nil
	}
	now := p.now()
	if err := p.ensureDayLoaded(ctx, now); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.resetBudgetDaysLocked(now)
	eligibleIdle := make([]int, 0, len(p.keys))
	eligibleBusy := make([]int, 0, len(p.keys))
	for offset := 0; offset < len(p.keys); offset++ {
		index := (p.mainNext + offset) % len(p.keys)
		state := &p.keys[index]
		if p.eligibleLocked(state, now, 0) {
			if p.compBusy[state.fingerprint] == 0 {
				eligibleIdle = append(eligibleIdle, index)
			} else {
				eligibleBusy = append(eligibleBusy, index)
			}
		}
	}
	eligible := append(eligibleIdle, eligibleBusy...)
	sort.SliceStable(eligible, func(i, j int) bool {
		return p.keys[eligible[i]].todayRequests < p.keys[eligible[j]].todayRequests
	})
	if len(eligible) == 0 {
		return nil, &HTTPError{StatusCode: 429, Body: "no eligible Tronscan API keys", RetryAfter: 30 * time.Second}
	}
	p.nextMainAt = now.Add(p.pollInterval)
	waves := (count + len(eligible) - 1) / len(eligible)
	priorityWindow := time.Duration(waves)*p.minInterval + 100*time.Millisecond
	if p.pollInterval > 0 && priorityWindow > 3*p.pollInterval/4 {
		priorityWindow = 3 * p.pollInterval / 4
	}
	p.mainPriorityUntil = now.Add(priorityWindow)
	leases := make([]apiKeyLease, count)
	for i := 0; i < count; i++ {
		index := eligible[i%len(eligible)]
		leases[i] = p.leaseForStateLocked(index)
		p.realtimeReserved[leases[i].fingerprint] = p.mainPriorityUntil
	}
	p.mainNext = (leases[count-1].index + 1) % len(p.keys)
	return leases, nil
}

func (p *keyPool) leaseForStateLocked(index int) apiKeyLease {
	state := &p.keys[index]
	return apiKeyLease{index: index, fingerprint: state.fingerprint, key: state.key}
}

func (p *keyPool) stateIndexLocked(lease apiKeyLease) int {
	if lease.fingerprint != "" {
		for index := range p.keys {
			if p.keys[index].fingerprint == lease.fingerprint {
				return index
			}
		}
		return -1
	}
	if lease.index >= 0 && lease.index < len(p.keys) {
		return lease.index
	}
	return -1
}

func (p *keyPool) takeCompensationTokenLocked(now time.Time) string {
	p.updateCompensationBudgetLocked(now)
	if p.compBudgetRPS <= 0 {
		return "no_spare_key_capacity"
	}
	if p.compTokens < 1 {
		return "compensation_token_bucket_empty"
	}
	p.compTokens--
	return ""
}

func (p *keyPool) updateCompensationBudgetLocked(now time.Time) {
	eligible := 0
	for index := range p.keys {
		state := &p.keys[index]
		if !p.eligibleLocked(state, now, 0) {
			continue
		}
		eligible++
	}
	realtimeRPS := float64(0)
	if p.mainPages > 0 && p.pollInterval > 0 {
		realtimeRPS = float64(p.mainPages) / p.pollInterval.Seconds()
	}
	perKeyRPS := 5.0
	if p.minInterval > 0 {
		perKeyRPS = 1 / p.minInterval.Seconds()
	}
	budgetRPS := float64(eligible)*perKeyRPS - realtimeRPS
	if budgetRPS < 0 {
		budgetRPS = 0
	}
	budgetRPS *= p.compPressure
	if p.compMaxRPS > 0 && budgetRPS > p.compMaxRPS {
		budgetRPS = p.compMaxRPS
	}
	if p.compLastRefill.IsZero() {
		p.compLastRefill = now
		if budgetRPS > 0 {
			p.compTokens = 1
		}
	} else if now.After(p.compLastRefill) {
		p.compTokens += now.Sub(p.compLastRefill).Seconds() * budgetRPS
		p.compLastRefill = now
	}
	capacity := budgetRPS * 2
	if capacity < 1 {
		capacity = 1
	}
	if p.compTokens > capacity {
		p.compTokens = capacity
	}
	p.compBudgetRPS = budgetRPS
}

func (p *keyPool) beginRequest(lease apiKeyLease, source RequestSource) {
	if p == nil || (source != RequestSourceCompensation && source != RequestSourceExpand) {
		return
	}
	p.mu.Lock()
	p.compBusy[lease.fingerprint]++
	p.mu.Unlock()
}

func (p *keyPool) endRequest(lease apiKeyLease, source RequestSource) {
	if p == nil || (source != RequestSourceCompensation && source != RequestSourceExpand) {
		return
	}
	p.mu.Lock()
	if p.compBusy[lease.fingerprint] <= 1 {
		delete(p.compBusy, lease.fingerprint)
	} else {
		p.compBusy[lease.fingerprint]--
	}
	p.mu.Unlock()
}

func (p *keyPool) eligibleLocked(state *apiKeyState, now time.Time, limit int) bool {
	if !state.enabled || (state.health != "" && state.health != "healthy") {
		return false
	}
	return !state.disabledUntil.After(now) && !state.cooldownUntil.After(now)
}

func (p *keyPool) selectLeaseLocked(now time.Time, start int, excluded map[string]struct{}, source RequestSource, failover bool) (int, string) {
	reason := "all keys excluded"
	selected := -1
	selectedUsage := int(^uint(0) >> 1)
	for offset := 0; offset < len(p.keys); offset++ {
		index := (start + offset) % len(p.keys)
		if _, skip := excluded[p.keys[index].fingerprint]; skip {
			continue
		}
		state := &p.keys[index]
		if (source == RequestSourceCompensation || source == RequestSourceExpand) && p.realtimeReserved[state.fingerprint].After(now) {
			reason = "realtime key reservation"
			continue
		}
		if !state.enabled {
			reason = "keys disabled"
			continue
		}
		if state.health != "" && state.health != "healthy" {
			reason = "keys unhealthy"
			continue
		}
		if state.disabledUntil.After(now) {
			reason = "keys disabled"
			continue
		}
		if state.cooldownUntil.After(now) {
			reason = "keys cooling down"
			continue
		}
		if source != RequestSourceCompensation {
			return index, ""
		}
		if state.todayRequests < selectedUsage {
			selected = index
			selectedUsage = state.todayRequests
		}
	}
	if selected >= 0 {
		return selected, ""
	}
	return -1, reason
}

func (p *keyPool) reserve(ctx context.Context, lease apiKeyLease, source RequestSource, failover bool) (time.Duration, error) {
	if p == nil {
		return 0, fmt.Errorf("tronscan key pool is not configured")
	}
	waitStarted := time.Now()
	for {
		now := p.now()
		if err := p.ensureDayLoaded(ctx, now); err != nil {
			return time.Since(waitStarted), err
		}
		p.mu.Lock()
		p.resetBudgetDaysLocked(now)
		stateIndex := p.stateIndexLocked(lease)
		if stateIndex < 0 {
			if p.usageStore != nil && lease.fingerprint != "" {
				record, allowed, reserveErr := p.usageStore.ReserveTronscanKeyRequest(ctx, lease.fingerprint, budgetDay(now, p.budgetZone), source, failover, 0, now)
				_ = record
				p.mu.Unlock()
				if reserveErr != nil {
					return time.Since(waitStarted), reserveErr
				}
				if !allowed {
					return time.Since(waitStarted), &HTTPError{StatusCode: 429, Body: "API key daily budget exhausted", RetryAfter: nextBudgetReset(now, p.budgetZone).Sub(now)}
				}
				return time.Since(waitStarted), nil
			}
			p.mu.Unlock()
			return time.Since(waitStarted), fmt.Errorf("tronscan key lease %s is no longer registered", lease.fingerprint)
		}
		state := &p.keys[stateIndex]
		blockedUntil := laterTime(state.cooldownUntil, state.disabledUntil, state.nextRequestAt)
		if blockedUntil.After(now) {
			p.mu.Unlock()
			delay := time.Until(blockedUntil)
			if delay > 2*time.Second {
				return time.Since(waitStarted), &HTTPError{StatusCode: 429, Body: "leased API key unavailable", RetryAfter: delay}
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return time.Since(waitStarted), ctx.Err()
			case <-timer.C:
			}
			continue
		}
		if p.usageStore != nil {
			record, allowed, err := p.usageStore.ReserveTronscanKeyRequest(ctx, state.fingerprint, state.budgetDay, source, failover, 0, now)
			if err != nil {
				p.lastStoreError = err.Error()
				p.mu.Unlock()
				return time.Since(waitStarted), err
			}
			p.applyRecordLocked(state, record)
			p.lastStoreError = ""
			if !allowed {
				p.mu.Unlock()
				return time.Since(waitStarted), &HTTPError{StatusCode: 429, Body: "API key daily budget exhausted", RetryAfter: nextBudgetReset(now, p.budgetZone).Sub(now)}
			}
		} else {
			p.incrementSourceLocked(state, source, failover)
		}
		state.nextRequestAt = now.Add(p.minInterval)
		p.mu.Unlock()
		return time.Since(waitStarted), nil
	}
}

func (p *keyPool) incrementSourceLocked(state *apiKeyState, source RequestSource, failover bool) {
	state.todayRequests++
	switch source {
	case RequestSourceMain:
		state.mainRequests++
	case RequestSourceCompensation:
		state.compRequests++
	default:
		state.otherRequests++
	}
	if failover {
		state.failoverRequests++
	}
}

func (p *keyPool) report(ctx context.Context, lease apiKeyLease, status int, body string, retryAfter time.Duration, now time.Time) error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	stateIndex := p.stateIndexLocked(lease)
	if stateIndex < 0 {
		if p.usageStore != nil && lease.fingerprint != "" && status != 0 {
			_, err := p.usageStore.RecordTronscanKeyResult(ctx, lease.fingerprint, budgetDay(now, p.budgetZone), status, time.Time{}, time.Time{}, time.Time{})
			return err
		}
		return nil
	}
	p.resetBudgetDaysLocked(now)
	state := &p.keys[stateIndex]
	state.lastHTTPStatus = status
	state.lastUsedAt = now
	switch status {
	case 401, 403:
		if quotaOrRateError(body) {
			p.applyRateLimitLocked(state, body, retryAfter, now)
		} else if blockedError(body) {
			state.health = "blocked"
			state.reason = "account_or_ip_blocked"
			state.consecutiveFailures++
			state.nextProbeAt = now.Add(p.blockedProbeInterval)
			state.lastErrorClass = "blocked"
		} else {
			previousHealth := state.health
			state.authErrorCount++
			state.consecutiveFailures++
			if lease.probe || state.health != "suspect" {
				state.consecutiveAuthFailures++
			}
			if previousHealth == "invalid" && lease.probe {
				state.health = "invalid"
				state.reason = "authentication_failed"
				state.nextProbeAt = now.Add(p.invalidProbeInterval)
				state.consecutiveProbeSuccesses = 0
			} else if state.consecutiveAuthFailures >= 2 {
				state.health = "invalid"
				state.reason = "authentication_failed"
				state.nextProbeAt = now.Add(p.invalidProbeInterval)
			} else {
				state.health = "suspect"
				state.reason = "authentication_suspect"
				state.nextProbeAt = now.Add(p.authProbeInterval)
			}
			state.lastErrorClass = "auth"
		}
		state.lastFailureAt = now
		state.consecutive429 = 0
	case 429:
		p.applyRateLimitLocked(state, body, retryAfter, now)
	default:
		if status >= 200 && status < 300 {
			state.lastSuccessAt = now
			if state.health == "healthy" || state.health == "" || lease.probe {
				state.consecutive429 = 0
				state.consecutiveFailures = 0
				state.consecutiveAuthFailures = 0
				state.cooldownUntil = time.Time{}
				state.disabledUntil = time.Time{}
			}
			requiresDoubleProbe := state.health == "invalid" || state.health == "blocked" || state.reason == "new_or_updated" || state.reason == "manual_recheck" || strings.Contains(state.reason, "authentication_failed") || strings.Contains(state.reason, "blocked")
			if requiresDoubleProbe && lease.probe {
				state.consecutiveProbeSuccesses++
				if state.consecutiveProbeSuccesses >= 2 {
					state.health = "healthy"
					state.reason = ""
					state.nextProbeAt = time.Time{}
				} else {
					state.nextProbeAt = now.Add(p.authProbeInterval)
				}
			} else if state.health == "healthy" || state.health == "" || lease.probe {
				state.health = "healthy"
				state.reason = ""
				state.nextProbeAt = time.Time{}
				state.consecutiveProbeSuccesses = 0
			}
			if state.health == "healthy" || state.health == "" {
				state.lastErrorClass = ""
			}
		} else if status == 0 || status >= 500 {
			state.consecutiveFailures++
			state.lastFailureAt = now
			state.health = "cooldown"
			state.reason = "temporary_transport_failure"
			state.lastErrorClass = classifyTransportError(status, body)
			delay := transportBackoff(state.consecutiveFailures)
			state.cooldownUntil = now.Add(jitterDuration(delay, state.fingerprint, now, 10))
			state.nextProbeAt = state.cooldownUntil
		}
	}
	if p.usageStore != nil && status != 0 {
		record, err := p.usageStore.RecordTronscanKeyResult(ctx, state.fingerprint, state.budgetDay, status, state.last429At, state.cooldownUntil, state.disabledUntil)
		if err != nil {
			p.lastStoreError = err.Error()
			return err
		}
		p.applyRecordLocked(state, record)
		p.lastStoreError = ""
	}
	if p.registryStore != nil {
		if err := p.registryStore.UpdateTronscanAPIKeyState(ctx, p.registryRecordLocked(state), now); err != nil {
			p.lastStoreError = err.Error()
			return err
		}
	}
	return nil
}

func (p *keyPool) dueProbeLeases(now time.Time) []apiKeyLease {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	leasingWindow := p.authProbeInterval
	var leases []apiKeyLease
	for index := range p.keys {
		state := &p.keys[index]
		if !state.enabled || state.health == "healthy" || state.health == "" || state.nextProbeAt.IsZero() || state.nextProbeAt.After(now) {
			continue
		}
		leases = append(leases, p.leaseForStateLocked(index))
		leases[len(leases)-1].probe = true
		state.nextProbeAt = now.Add(leasingWindow)
	}
	return leases
}

func (p *keyPool) requestProbe(ctx context.Context, fingerprint string, enable bool) error {
	if p == nil {
		return fmt.Errorf("tronscan key pool is not configured")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for index := range p.keys {
		state := &p.keys[index]
		if state.fingerprint != strings.TrimSpace(fingerprint) {
			continue
		}
		if enable {
			state.enabled = true
		}
		state.health = "suspect"
		state.reason = "manual_recheck"
		state.consecutiveProbeSuccesses = 0
		state.consecutiveAuthFailures = 0
		state.consecutiveFailures = 0
		state.cooldownUntil = time.Time{}
		state.nextProbeAt = p.now()
		if p.registryStore != nil {
			return p.registryStore.UpdateTronscanAPIKeyState(ctx, p.registryRecordLocked(state), p.now())
		}
		return nil
	}
	return fmt.Errorf("tronscan API key %s was not found", strings.TrimSpace(fingerprint))
}

func (p *keyPool) applyRateLimitLocked(state *apiKeyState, body string, retryAfter time.Duration, now time.Time) {
	state.rateLimitCount++
	state.consecutive429++
	state.consecutiveFailures++
	state.last429At = now
	state.lastFailureAt = now
	state.lastErrorClass = "rate_limit"
	if dailyQuotaExceeded(body) {
		state.health = "exhausted"
		state.reason = "daily_quota_exhausted"
		state.cooldownUntil = nextBudgetReset(now, p.budgetZone).Add(deterministicRange(state.fingerprint, now, 30*time.Second, 120*time.Second))
		state.nextProbeAt = state.cooldownUntil
		return
	}
	state.health = "cooldown"
	state.reason = "rate_limited"
	delay := retryAfter
	if delay > 0 {
		delay = jitterDuration(delay, state.fingerprint, now, 10)
	} else if p.publicFallback {
		delay = publicFallbackBackoff(state.consecutive429)
	} else {
		delay = rateLimitBackoff(state.consecutive429)
	}
	state.cooldownUntil = now.Add(delay)
	state.nextProbeAt = state.cooldownUntil
}

func publicFallbackBackoff(failures int) time.Duration {
	steps := []time.Duration{time.Second, 2 * time.Second, 3 * time.Second, 5 * time.Second, 10 * time.Second}
	if failures < 1 {
		failures = 1
	}
	if failures > len(steps) {
		failures = len(steps)
	}
	return steps[failures-1]
}

func quotaOrRateError(body string) bool {
	value := strings.ToLower(body)
	return dailyQuotaExceeded(body) || strings.Contains(value, "rate limit") || strings.Contains(value, "too many") || strings.Contains(value, "quota")
}

func blockedError(body string) bool {
	value := strings.ToLower(body)
	return strings.Contains(value, "blocked") || strings.Contains(value, "banned") || strings.Contains(value, "suspended") || strings.Contains(value, "ip forbidden")
}

func rateLimitBackoff(failures int) time.Duration {
	steps := []time.Duration{60 * time.Second, 120 * time.Second, 300 * time.Second, 900 * time.Second, time.Hour}
	if failures < 1 {
		failures = 1
	}
	if failures > len(steps) {
		failures = len(steps)
	}
	return steps[failures-1]
}

func transportBackoff(failures int) time.Duration {
	steps := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 15 * time.Second, 30 * time.Second}
	if failures < 1 {
		failures = 1
	}
	if failures > len(steps) {
		failures = len(steps)
	}
	return steps[failures-1]
}

func classifyTransportError(status int, body string) string {
	value := strings.ToLower(body)
	switch {
	case strings.Contains(value, "tls"):
		return "tls"
	case strings.Contains(value, "dns") || strings.Contains(value, "no such host"):
		return "dns"
	case strings.Contains(value, "timeout") || strings.Contains(value, "deadline"):
		return "timeout"
	case status >= 500:
		return "5xx"
	default:
		return "network"
	}
}

func jitterDuration(base time.Duration, seed string, now time.Time, maxPercent int) time.Duration {
	if base <= 0 || maxPercent <= 0 {
		return base
	}
	sum := sha256.Sum256([]byte(seed + now.UTC().Format(time.RFC3339Nano)))
	pct := int(sum[0]) % (maxPercent + 1)
	return base + base*time.Duration(pct)/100
}

func deterministicRange(seed string, now time.Time, min, max time.Duration) time.Duration {
	if max <= min {
		return min
	}
	sum := sha256.Sum256([]byte(seed + now.UTC().Format("2006-01-02")))
	span := int64(max - min)
	value := int64(sum[0])<<8 | int64(sum[1])
	return min + time.Duration(value%span)
}

func (p *keyPool) registryRecordLocked(state *apiKeyState) KeyRegistryRecord {
	return KeyRegistryRecord{Fingerprint: state.fingerprint, APIKey: state.key, Enabled: state.enabled,
		Health: state.health, Reason: state.reason, ConsecutiveFailures: state.consecutiveFailures,
		ConsecutiveAuthFailures: state.consecutiveAuthFailures, ConsecutiveProbeSuccesses: state.consecutiveProbeSuccesses,
		CooldownUntil: state.cooldownUntil, NextProbeAt: state.nextProbeAt, LastUsedAt: state.lastUsedAt,
		LastSuccessAt: state.lastSuccessAt, LastFailureAt: state.lastFailureAt, LastErrorClass: state.lastErrorClass}
}

func dailyQuotaExceeded(body string) bool {
	value := strings.ToLower(body)
	return strings.Contains(value, "daily usage") ||
		strings.Contains(value, "daily limit") ||
		strings.Contains(value, "daily quota") ||
		strings.Contains(value, "quota exceeded") ||
		strings.Contains(value, "100000")
}

func (p *keyPool) status(now time.Time) KeyPoolStatus {
	if p == nil {
		return KeyPoolStatus{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.resetBudgetDaysLocked(now)
	required := requiredKeyCount(p.mainPages, p.pollInterval, p.minInterval)
	status := KeyPoolStatus{
		KeyCount:             len(p.keys),
		RequiredMainKeyCount: required,
		MainCapacitySafe:     true,
		BudgetTimezone:       p.budgetZone.String(),
		NextBudgetResetAt:    nextBudgetReset(now, p.budgetZone),
		PersistenceError:     p.lastStoreError,
		Keys:                 make([]APIKeyStatus, 0, len(p.keys)),
	}
	if p.minInterval > 0 {
		status.PerKeyLimitRPS = 1 / p.minInterval.Seconds()
	}
	p.updateCompensationBudgetLocked(now)
	status.RealtimeReservedRPS = 0
	if p.mainPages > 0 && p.pollInterval > 0 {
		status.RealtimeReservedRPS = float64(p.mainPages) / p.pollInterval.Seconds()
	}
	status.CompensationBudgetRPS = p.compBudgetRPS
	status.CompensationTokens = p.compTokens
	status.CompensationDeferredReason = p.lastCompDeferredReason
	status.CompensationDeferred = p.lastCompDeferredReason != ""
	for index := range p.keys {
		state := &p.keys[index]
		if state.enabled {
			status.EnabledCount++
		}
		switch state.health {
		case "healthy", "":
			status.HealthyCount++
		case "cooldown", "half_open", "suspect":
			status.CooldownCount++
		case "exhausted":
			status.ExhaustedCount++
		case "invalid", "blocked":
			status.InvalidCount++
		}
		available, unavailableFor := p.availableLocked(state, now)
		if available {
			status.AvailableCount++
		}
		if !state.last429At.IsZero() && now.Sub(state.last429At) <= sharedLimitWindow {
			status.RateLimitedKeys++
		}
		status.Keys = append(status.Keys, APIKeyStatus{
			Index:               index,
			Fingerprint:         state.fingerprint,
			TodayRequests:       state.todayRequests,
			MainRequests:        state.mainRequests,
			CompRequests:        state.compRequests,
			OtherRequests:       state.otherRequests,
			FailoverRequests:    state.failoverRequests,
			RateLimitCount:      state.rateLimitCount,
			AuthErrorCount:      state.authErrorCount,
			LastHTTPStatus:      state.lastHTTPStatus,
			Last429At:           timePointer(state.last429At),
			CooldownUntil:       timePointer(state.cooldownUntil),
			DisabledUntil:       timePointer(state.disabledUntil),
			NextRequestAt:       timePointer(state.nextRequestAt),
			Available:           available,
			UnavailableFor:      unavailableFor,
			Enabled:             state.enabled,
			Health:              state.health,
			Reason:              state.reason,
			ConsecutiveFailures: state.consecutiveFailures,
			NextProbeAt:         timePointer(state.nextProbeAt),
			LastUsedAt:          timePointer(state.lastUsedAt),
			LastSuccessAt:       timePointer(state.lastSuccessAt),
			LastFailureAt:       timePointer(state.lastFailureAt),
			LastErrorClass:      state.lastErrorClass,
		})
		status.TodayMainRequests += state.mainRequests
		status.TodayCompRequests += state.compRequests
		status.TodayOtherRequests += state.otherRequests
		status.TodayFailoverRequests += state.failoverRequests
	}
	status.PossibleSharedLimit = status.KeyCount > 1 && status.RateLimitedKeys >= 2
	status.MainCapacitySafe = required == 0 || status.AvailableCount >= required
	if !status.MainCapacitySafe {
		status.CapacityWarning = fmt.Sprintf("main scan requires at least %d available keys at the configured per-key rate; available %d", required, status.AvailableCount)
	}
	return status
}

func requiredKeyCount(pages int, interval, minRequestInterval time.Duration) int {
	if pages < 1 || interval <= 0 {
		return 0
	}
	perKeyRPS := 5.0
	if minRequestInterval > 0 {
		perKeyRPS = 1 / minRequestInterval.Seconds()
	}
	requiredRPS := float64(pages) / interval.Seconds()
	return int(math.Ceil(requiredRPS / perKeyRPS))
}

func (p *keyPool) availableLocked(state *apiKeyState, now time.Time) (bool, string) {
	if !state.enabled {
		return false, "disabled"
	}
	if state.health != "" && state.health != "healthy" {
		return false, state.health
	}
	if state.disabledUntil.After(now) {
		if state.lastHTTPStatus == 429 {
			return false, "daily_quota"
		}
		return false, "auth"
	}
	if state.cooldownUntil.After(now) {
		return false, "rate_limit"
	}
	return true, ""
}

func (p *keyPool) resetBudgetDaysLocked(now time.Time) {
	day := budgetDay(now, p.budgetZone)
	for index := range p.keys {
		state := &p.keys[index]
		if state.budgetDay == day {
			continue
		}
		p.clearDailyStateLocked(state, day)
	}
}

func (p *keyPool) applyRecordsLocked(records map[string]KeyUsageRecord, day string) {
	for index := range p.keys {
		state := &p.keys[index]
		p.clearDailyStateLocked(state, day)
		if record, ok := records[state.fingerprint]; ok {
			p.applyRecordLocked(state, record)
		}
	}
}

func (p *keyPool) clearDailyStateLocked(state *apiKeyState, day string) {
	nextRequestAt := state.nextRequestAt
	state.budgetDay = day
	state.todayRequests = 0
	state.mainRequests = 0
	state.compRequests = 0
	state.otherRequests = 0
	state.failoverRequests = 0
	state.rateLimitCount = 0
	state.authErrorCount = 0
	state.consecutive429 = 0
	state.lastHTTPStatus = 0
	state.last429At = time.Time{}
	state.cooldownUntil = time.Time{}
	state.disabledUntil = time.Time{}
	state.nextRequestAt = nextRequestAt
}

func (p *keyPool) applyRecordLocked(state *apiKeyState, record KeyUsageRecord) {
	state.budgetDay = record.BudgetDay
	state.todayRequests = record.RequestCount
	state.mainRequests = record.MainRequestCount
	state.compRequests = record.CompRequestCount
	state.otherRequests = record.OtherRequestCount
	state.failoverRequests = record.FailoverCount
	state.rateLimitCount = record.RateLimitCount
	state.authErrorCount = record.AuthErrorCount
	state.lastHTTPStatus = record.LastHTTPStatus
	state.last429At = record.Last429At
	state.cooldownUntil = record.CooldownUntil
	state.disabledUntil = record.DisabledUntil
}

func budgetDay(now time.Time, zone *time.Location) string {
	if zone == nil {
		zone = time.UTC
	}
	return now.In(zone).Format("2006-01-02")
}

func nextBudgetReset(now time.Time, zone *time.Location) time.Time {
	if zone == nil {
		zone = time.UTC
	}
	local := now.In(zone)
	return time.Date(local.Year(), local.Month(), local.Day()+1, 0, 0, 0, 0, zone)
}

func laterTime(values ...time.Time) time.Time {
	var latest time.Time
	for _, value := range values {
		if value.After(latest) {
			latest = value
		}
	}
	return latest
}

func timePointer(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	copy := value
	return &copy
}

func minKeyInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
