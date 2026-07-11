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
	BotID           string `json:"bot_id,omitempty"`
	ChatID          int64  `json:"chat_id"`
	OwnerUserID     int64  `json:"owner_user_id"`
	Address         string `json:"address"`
	Label           string `json:"label"`
	MinNotifyAmount string `json:"min_amount"`
	WatchIncome     bool   `json:"watch_income"`
	WatchExpense    bool   `json:"watch_expense"`
	NotifyTRX       bool   `json:"notify_trx"`
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
	Status                 string                 `json:"status"`
	Ready                  bool                   `json:"ready"`
	Now                    time.Time              `json:"now"`
	StaleAfterMS           int64                  `json:"stale_after_ms"`
	Global                 ScanStatusResponse     `json:"global"`
	Address                ScanStatusResponse     `json:"address"`
	Deliveries             DeliveryStatusResponse `json:"deliveries"`
	RetentionCleanup       CleanupStatusResponse  `json:"retention_cleanup"`
	AddressCursor          int                    `json:"address_cursor"`
	AddressScanMaxPerTick  int                    `json:"address_scan_max_per_tick"`
	AddressScanSkippedNear int64                  `json:"address_scan_skipped_near_global"`
}

type ScanStatusResponse struct {
	LastStartedAt      *time.Time          `json:"last_started_at,omitempty"`
	LastSuccessAt      *time.Time          `json:"last_success_at,omitempty"`
	LastErrorAt        *time.Time          `json:"last_error_at,omitempty"`
	LastError          string              `json:"last_error,omitempty"`
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
	PageLimitReached   bool                `json:"page_limit_reached"`
	Recent             []ScanRoundResponse `json:"recent,omitempty"`
}

type ScanRoundResponse struct {
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
	raw := strings.Join([]string{t.Hash, t.From, t.To, t.Value, t.TokenAddress, fmt.Sprint(t.BlockTimestamp)}, "|")
	sum := sha1.Sum([]byte(raw))
	return "tron:trc20:" + strings.ToLower(t.Hash) + ":" + hex.EncodeToString(sum[:])[:12]
}

func DeliveryID(sub storage.ChainWatcherSubscription, t tron.Transfer, direction string) string {
	raw := strings.Join([]string{sub.BotID, fmt.Sprint(sub.ChatID), fmt.Sprint(sub.OwnerUserID), sub.Address, strings.ToLower(t.Hash), direction}, "|")
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
	}
}

func MatchTransfer(t tron.Transfer, subs []storage.ChainWatcherSubscription) []storage.ChainWatcherMatchedEvent {
	matches := make([]storage.ChainWatcherMatchedEvent, 0)
	for _, sub := range subs {
		if !sub.Active {
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
		BotID:           strings.TrimSpace(botID),
		ChatID:          chatID,
		OwnerUserID:     req.OwnerUserID,
		Address:         strings.TrimSpace(req.Address),
		Label:           strings.TrimSpace(req.Label),
		WatchIncome:     req.WatchIncome,
		WatchExpense:    req.WatchExpense,
		NotifyTRX:       req.NotifyTRX,
		MinNotifyAmount: minAmount,
		Active:          true,
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
