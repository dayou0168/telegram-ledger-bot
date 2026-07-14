package chainwatcher

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/tron"
)

type SubscriptionRequest struct {
	BotID             string `json:"bot_id,omitempty"`
	ChatID            int64  `json:"chat_id"`
	OwnerUserID       int64  `json:"owner_user_id"`
	Address           string `json:"address"`
	Label             string `json:"label"`
	MinNotifyAmount   string `json:"min_amount"`
	WatchIncome       bool   `json:"watch_income"`
	WatchExpense      bool   `json:"watch_expense"`
	NotifyTRX         bool   `json:"notify_trx"`
	BaselineTimestamp int64  `json:"baseline_timestamp"`
}

type SyncRequest struct {
	BotID         string                `json:"bot_id,omitempty"`
	Subscriptions []SubscriptionRequest `json:"subscriptions"`
}

type DeleteSubscriptionRequest struct {
	BotID       string `json:"bot_id,omitempty"`
	ChatID      int64  `json:"chat_id"`
	OwnerUserID int64  `json:"owner_user_id"`
	Address     string `json:"address"`
}

type ClaimRequest struct {
	BotID string `json:"bot_id,omitempty"`
	Limit int    `json:"limit"`
}

type ClaimResponse struct {
	Events []MatchedEvent `json:"events"`
}

type StatusResponse struct {
	Status                string                    `json:"status"`
	Ready                 bool                      `json:"ready"`
	SourceReady           bool                      `json:"source_ready"`
	Now                   time.Time                 `json:"now"`
	StaleAfterMS          int64                     `json:"stale_after_ms"`
	Global                ScanStatusResponse        `json:"global"`
	Catchup               ScanStatusResponse        `json:"catchup"`
	Expand                ScanStatusResponse        `json:"expand"`
	Deliveries            DeliveryStatusResponse    `json:"deliveries"`
	RetentionCleanup      CleanupStatusResponse     `json:"retention_cleanup"`
	TronscanKeys          tron.KeyPoolStatus        `json:"tronscan_keys"`
	GlobalWatermark       WatermarkStatusResponse   `json:"global_watermark"`
	RealtimeWatermark     WatermarkStatusResponse   `json:"realtime_watermark"`
	Fallback              FallbackStatusResponse    `json:"fallback"`
	WatchAddressCount     int                       `json:"watch_address_count"`
	CatchupDeferredReason string                    `json:"catchup_deferred_reason,omitempty"`
	CatchupDeferredCount  int64                     `json:"catchup_deferred_count"`
	CatchupLagSeconds     int64                     `json:"catchup_lag_seconds"`
	CatchupRequired       bool                      `json:"catchup_required"`
	CatchupReason         string                    `json:"catchup_reason,omitempty"`
	CatchupSafeEnd        int64                     `json:"catchup_safe_end"`
	CatchupETASeconds     int64                     `json:"catchup_eta_seconds"`
	CatchupLagUnknown     bool                      `json:"catchup_lag_unknown"`
	ContinuityReady       bool                      `json:"continuity_ready"`
	OpenGapCount          int64                     `json:"open_gap_count"`
	LeasedGapCount        int64                     `json:"leased_gap_count"`
	MainInflightRounds    int                       `json:"main_inflight_rounds"`
	MainInflightLimit     int                       `json:"main_inflight_limit"`
	HeadAPIMaxConcurrency int                       `json:"head_api_max_concurrency"`
	HeadPersistWorkers    int                       `json:"head_persist_workers"`
	HeadPriorityDBLanes   int                       `json:"head_priority_db_lanes"`
	HeadOnTime            bool                      `json:"head_on_time"`
	HeadLatenessMS        int64                     `json:"head_lateness_ms"`
	GapScheduler          GapSchedulerStatus        `json:"gap_scheduler"`
	DeprecatedConfig      []string                  `json:"deprecated_config,omitempty"`
	Metrics72H            []MetricAggregateResponse `json:"metrics_72h,omitempty"`
}

type GapSchedulerStatus struct {
	ConfiguredWorkers      int                       `json:"configured_workers"`
	ActiveWorkers          int                       `json:"active_workers"`
	EffectiveConcurrency   int                       `json:"effective_concurrency"`
	ConcurrencyCapReached  bool                      `json:"concurrency_cap_reached"`
	P1ReservationConflicts int64                     `json:"p1_reservation_conflicts"`
	FairnessMaxWaitMS      int64                     `json:"fairness_max_wait_ms"`
	Metrics                []GapMetricStatusResponse `json:"metrics"`
	OpenGroups             []GapGroupStatusResponse  `json:"open_groups"`
}

type GapMetricStatusResponse struct {
	WindowMinutes      int    `json:"window_minutes"`
	Kind               string `json:"kind"`
	Priority           int    `json:"priority"`
	Created            int64  `json:"created"`
	Completed          int64  `json:"completed"`
	NetChange          int64  `json:"net_change"`
	Merged             int64  `json:"merged"`
	Failed             int64  `json:"failed"`
	FairnessSelections int64  `json:"fairness_selections"`
}

type GapGroupStatusResponse struct {
	Kind        string `json:"kind"`
	Priority    int    `json:"priority"`
	Pending     int64  `json:"pending"`
	Leased      int64  `json:"leased"`
	OldestAgeMS int64  `json:"oldest_age_ms"`
}

type MetricAggregateResponse struct {
	Lane         string `json:"lane"`
	SuccessCount int64  `json:"success_count"`
	ErrorCount   int64  `json:"error_count"`
	RequestCount int64  `json:"request_count"`
	APIMS        int64  `json:"api_ms"`
	ParseMS      int64  `json:"parse_ms"`
	MatchMS      int64  `json:"match_ms"`
	WriteMS      int64  `json:"write_ms"`
	OverlapCount int64  `json:"overlap_count"`
}

type ReadyStatusResponse struct {
	Status            string    `json:"status"`
	Ready             bool      `json:"ready"`
	SourceReady       bool      `json:"source_ready"`
	Now               time.Time `json:"now"`
	CatchupLagSeconds int64     `json:"catchup_lag_seconds"`
	CatchupLagUnknown bool      `json:"catchup_lag_unknown"`
	ContinuityReady   bool      `json:"continuity_ready"`
	OpenGapCount      int64     `json:"open_gap_count"`
	LeasedGapCount    int64     `json:"leased_gap_count"`
	WatchAddressCount int       `json:"watch_address_count"`
}

type WatermarkStatusResponse struct {
	Timestamp  int64      `json:"timestamp"`
	TxHash     string     `json:"tx_hash,omitempty"`
	EventID    string     `json:"event_id,omitempty"`
	Source     string     `json:"source,omitempty"`
	UpdatedAt  *time.Time `json:"updated_at,omitempty"`
	LagSeconds int64      `json:"lag_seconds"`
}

type FallbackStatusResponse struct {
	Mode               string     `json:"mode"`
	LastWatcherSuccess *time.Time `json:"last_watcher_success,omitempty"`
	FallbackLeader     string     `json:"fallback_leader,omitempty"`
	FallbackStartedAt  *time.Time `json:"fallback_started_at,omitempty"`
	FallbackRequests   int64      `json:"fallback_requests"`
	Fallback429        int64      `json:"fallback_429"`
	CatchupFrom        int64      `json:"catchup_from"`
	CatchupTo          int64      `json:"catchup_to"`
	CatchupLagSeconds  int64      `json:"catchup_lag_seconds"`
	CatchupPages       int64      `json:"catchup_pages"`
	CatchupRequests    int64      `json:"catchup_requests"`
	CatchupBudgetUsed  int64      `json:"catchup_budget_used"`
	Recovering         bool       `json:"recovering"`
	LeaseUntil         *time.Time `json:"lease_until,omitempty"`
}

type ScanStatusResponse struct {
	RoundID            int64               `json:"round_id"`
	LastStartedAt      *time.Time          `json:"last_started_at,omitempty"`
	LastSuccessAt      *time.Time          `json:"last_success_at,omitempty"`
	LastErrorAt        *time.Time          `json:"last_error_at,omitempty"`
	LastError          string              `json:"last_error,omitempty"`
	LastErrorClass     string              `json:"last_error_class,omitempty"`
	LastDurationMS     int64               `json:"last_duration_ms"`
	APIWaitMS          int64               `json:"api_wait_ms"`
	APIFetchMS         int64               `json:"api_fetch_ms"`
	ParseMS            int64               `json:"parse_ms"`
	MatchMS            int64               `json:"match_ms"`
	WriteMS            int64               `json:"write_ms"`
	BackoffUntil       *time.Time          `json:"backoff_until,omitempty"`
	BackoffRemainingMS int64               `json:"backoff_remaining_ms"`
	LastBlockTimestamp int64               `json:"last_block_timestamp"`
	LagMS              int64               `json:"lag_ms"`
	ScanCount          int64               `json:"scan_count"`
	ErrorCount         int64               `json:"error_count"`
	OverlapSkipped     int64               `json:"overlap_skipped"`
	TransferCount      int                 `json:"transfer_count"`
	MatchCount         int                 `json:"match_count"`
	SubscriptionCount  int                 `json:"subscription_count"`
	AddressCount       int                 `json:"address_count"`
	APICallCount       int                 `json:"api_call_count"`
	PageCount          int                 `json:"page_count"`
	PageLimit          int                 `json:"page_limit"`
	PageLimitReached   bool                `json:"page_limit_reached"`
	BasePageCount      int                 `json:"base_page_count"`
	DynamicPageCount   int                 `json:"dynamic_page_count"`
	ContinuationPage   int                 `json:"continuation_page"`
	YieldReason        string              `json:"yield_reason,omitempty"`
	CutoffTimestamp    int64               `json:"cutoff_timestamp"`
	AnchorFound        bool                `json:"anchor_found"`
	AnchorHitCount     int64               `json:"anchor_hit_count"`
	AnchorMissCount    int64               `json:"anchor_miss_count"`
	AnchorHitRate      float64             `json:"anchor_hit_rate"`
	PreviousAnchorID   string              `json:"previous_anchor_id,omitempty"`
	HeadEventID        string              `json:"head_event_id,omitempty"`
	Recent             []ScanRoundResponse `json:"recent,omitempty"`
}

type ScanRoundResponse struct {
	RoundID          int64      `json:"round_id"`
	StartedAt        *time.Time `json:"started_at,omitempty"`
	Success          bool       `json:"success"`
	Error            string     `json:"error,omitempty"`
	DurationMS       int64      `json:"duration_ms"`
	APIWaitMS        int64      `json:"api_wait_ms"`
	APIFetchMS       int64      `json:"api_fetch_ms"`
	ParseMS          int64      `json:"parse_ms"`
	MatchMS          int64      `json:"match_ms"`
	WriteMS          int64      `json:"write_ms"`
	TransferCount    int        `json:"transfer_count"`
	MatchCount       int        `json:"match_count"`
	AddressCount     int        `json:"address_count"`
	APICallCount     int        `json:"api_call_count"`
	PageCount        int        `json:"page_count"`
	PageLimitReached bool       `json:"page_limit_reached"`
}

type DeliveryStatusResponse struct {
	PendingCount       int64      `json:"pending_count"`
	DeliveringCount    int64      `json:"delivering_count"`
	OldestPendingAt    *time.Time `json:"oldest_pending_at,omitempty"`
	OldestPendingAgeMS int64      `json:"oldest_pending_age_ms"`
}

type CleanupStatusResponse struct {
	LastRunAt      *time.Time `json:"last_run_at,omitempty"`
	MatchedDeleted int64      `json:"matched_deleted"`
	EventsDeleted  int64      `json:"events_deleted"`
	Error          string     `json:"error,omitempty"`
}

type AckRequest struct {
	BotID       string   `json:"bot_id,omitempty"`
	DeliveryIDs []string `json:"delivery_ids"`
}

type MatchedEvent struct {
	DeliveryID     string `json:"delivery_id"`
	EventID        string `json:"event_id"`
	BotID          string `json:"bot_id"`
	ChatID         int64  `json:"chat_id"`
	OwnerUserID    int64  `json:"owner_user_id"`
	WatchAddress   string `json:"watch_address"`
	Label          string `json:"label"`
	Direction      string `json:"direction"`
	TxHash         string `json:"tx_hash"`
	From           string `json:"from"`
	To             string `json:"to"`
	Value          string `json:"value"`
	TokenSymbol    string `json:"token_symbol"`
	TokenAddress   string `json:"token_address"`
	TokenDecimals  int    `json:"token_decimals"`
	BlockTimestamp int64  `json:"block_timestamp"`
	Confirmed      bool   `json:"confirmed"`
}

func EventID(t tron.Transfer) string {
	raw := tron.TransferIdentity(t)
	sum := sha1.Sum([]byte(raw))
	return "tron:trc20:" + strings.ToLower(t.Hash) + ":" + hex.EncodeToString(sum[:])[:12]
}

func AnchorCoverage(transfers []tron.Transfer, previous string) (string, bool) {
	found := previous == ""
	head := previous
	if len(transfers) > 0 {
		head = EventID(transfers[0])
	}
	for _, transfer := range transfers {
		if EventID(transfer) == previous {
			found = true
		}
	}
	return head, found
}

func DeliveryID(sub storage.ChainWatcherSubscription, t tron.Transfer, direction string) string {
	raw := strings.Join([]string{sub.BotID, fmt.Sprint(sub.ChatID), fmt.Sprint(sub.OwnerUserID), sub.Address, EventID(t), direction}, "|")
	sum := sha1.Sum([]byte(raw))
	return "cw:" + hex.EncodeToString(sum[:])
}

func TransferEvent(t tron.Transfer, source string) storage.ChainWatcherEvent {
	return storage.ChainWatcherEvent{
		EventID:        EventID(t),
		TxHash:         t.Hash,
		Contract:       t.TokenAddress,
		From:           t.From,
		To:             t.To,
		Value:          t.Value,
		TokenSymbol:    t.TokenSymbol,
		TokenAddress:   t.TokenAddress,
		TokenDecimals:  t.TokenDecimals,
		BlockTimestamp: t.BlockTimestamp,
		Confirmed:      t.Confirmed,
		Source:         source,
		EventIndex:     t.EventIndex,
	}
}

func MatchTransfer(t tron.Transfer, subs []storage.ChainWatcherSubscription) []storage.ChainWatcherMatchedEvent {
	matches := make([]storage.ChainWatcherMatchedEvent, 0)
	for _, sub := range subs {
		if !sub.Active {
			continue
		}
		if sub.BaselineTimestamp > 0 && t.BlockTimestamp < sub.BaselineTimestamp {
			continue
		}
		direction := ""
		switch sub.Address {
		case t.From:
			if sub.WatchExpense {
				direction = "expense"
			}
		case t.To:
			if sub.WatchIncome {
				direction = "income"
			}
		}
		if direction == "" {
			continue
		}
		if !amountAtLeast(t.Value, t.TokenDecimals, sub.MinNotifyAmount) {
			continue
		}
		matches = append(matches, storage.ChainWatcherMatchedEvent{
			DeliveryID:   DeliveryID(sub, t, direction),
			EventID:      EventID(t),
			BotID:        sub.BotID,
			ChatID:       sub.ChatID,
			OwnerUserID:  sub.OwnerUserID,
			WatchAddress: sub.Address,
			Label:        sub.Label,
			Direction:    direction,
		})
	}
	return matches
}

func amountAtLeast(raw string, decimals int, minRaw string) bool {
	minRaw = strings.TrimSpace(minRaw)
	if minRaw == "" || minRaw == "0" {
		return true
	}
	min, ok := new(big.Rat).SetString(minRaw)
	if !ok {
		return true
	}
	value, ok := new(big.Rat).SetString(raw)
	if !ok {
		return false
	}
	if decimals < 0 || decimals > 30 {
		decimals = 6
	}
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	value.Quo(value, new(big.Rat).SetInt(scale))
	return value.Cmp(min) >= 0
}

func ToSubscription(botID string, req SubscriptionRequest) storage.ChainWatcherSubscription {
	minAmount := strings.TrimSpace(req.MinNotifyAmount)
	if minAmount == "" {
		minAmount = "0"
	}
	chatID := req.ChatID
	if chatID == 0 {
		chatID = req.OwnerUserID
	}
	return storage.ChainWatcherSubscription{
		BotID:             strings.TrimSpace(botID),
		ChatID:            chatID,
		OwnerUserID:       req.OwnerUserID,
		Address:           strings.TrimSpace(req.Address),
		Label:             strings.TrimSpace(req.Label),
		WatchIncome:       req.WatchIncome,
		WatchExpense:      req.WatchExpense,
		NotifyTRX:         req.NotifyTRX,
		BaselineTimestamp: req.BaselineTimestamp,
		MinNotifyAmount:   minAmount,
		Active:            true,
	}
}

func FromMatchedStorage(item storage.ChainWatcherMatchedEvent) MatchedEvent {
	return MatchedEvent{
		DeliveryID:     item.DeliveryID,
		EventID:        item.EventID,
		BotID:          item.BotID,
		ChatID:         item.ChatID,
		OwnerUserID:    item.OwnerUserID,
		WatchAddress:   item.WatchAddress,
		Label:          item.Label,
		Direction:      item.Direction,
		TxHash:         item.TxHash,
		From:           item.From,
		To:             item.To,
		Value:          item.Value,
		TokenSymbol:    item.TokenSymbol,
		TokenAddress:   item.TokenAddress,
		TokenDecimals:  item.TokenDecimals,
		BlockTimestamp: item.BlockTimestamp,
		Confirmed:      item.Confirmed,
	}
}
