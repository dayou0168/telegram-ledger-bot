package chainclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/chainwatcher"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
)

type Client struct {
	baseURL string
	botID   string
	secret  string
	http    *http.Client
}

func New(baseURL, botID, secret string, timeout time.Duration) *Client {
	return &Client{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		botID:   strings.TrimSpace(botID),
		secret:  strings.TrimSpace(secret),
		http:    &http.Client{Timeout: timeout},
	}
}

func (c *Client) Enabled() bool {
	return c != nil && c.baseURL != "" && c.botID != "" && c.secret != ""
}

func (c *Client) UpsertSubscription(ctx context.Context, target storage.WatchTarget) error {
	req := chainwatcher.SubscriptionRequest{
		ChatID:          target.OwnerUserID,
		OwnerUserID:     target.OwnerUserID,
		Address:         target.Address,
		Label:           target.Label,
		MinNotifyAmount: target.MinNotifyAmount,
		WatchIncome:     target.WatchIncome,
		WatchExpense:    target.WatchExpense,
		NotifyTRX:       target.NotifyTRX,
	}
	return c.post(ctx, "/v1/subscriptions/upsert", req, nil)
}

func (c *Client) DeleteSubscription(ctx context.Context, owner int64, address string) error {
	req := chainwatcher.DeleteSubscriptionRequest{ChatID: owner, OwnerUserID: owner, Address: address}
	return c.post(ctx, "/v1/subscriptions/delete", req, nil)
}

func (c *Client) SyncSubscriptions(ctx context.Context, targets []storage.WatchTarget) error {
	req := chainwatcher.SyncRequest{Subscriptions: make([]chainwatcher.SubscriptionRequest, 0, len(targets))}
	for _, target := range targets {
		req.Subscriptions = append(req.Subscriptions, chainwatcher.SubscriptionRequest{
			ChatID:          target.OwnerUserID,
			OwnerUserID:     target.OwnerUserID,
			Address:         target.Address,
			Label:           target.Label,
			MinNotifyAmount: target.MinNotifyAmount,
			WatchIncome:     target.WatchIncome,
			WatchExpense:    target.WatchExpense,
			NotifyTRX:       target.NotifyTRX,
		})
	}
	return c.post(ctx, "/v1/subscriptions/sync", req, nil)
}

func (c *Client) ClaimEvents(ctx context.Context, limit int) ([]chainwatcher.MatchedEvent, error) {
	var resp chainwatcher.ClaimResponse
	if err := c.post(ctx, "/v1/events/claim", chainwatcher.ClaimRequest{Limit: limit}, &resp); err != nil {
		return nil, err
	}
	return resp.Events, nil
}

func (c *Client) AckEvents(ctx context.Context, ids []string) error {
	return c.post(ctx, "/v1/events/ack", chainwatcher.AckRequest{DeliveryIDs: ids}, nil)
}

func (c *Client) Health(ctx context.Context) error {
	if !c.Enabled() {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/healthz", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("chain watcher health http %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

func (c *Client) post(ctx context.Context, path string, payload any, out any) error {
	if !c.Enabled() {
		return nil
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Bot-ID", c.botID)
	req.Header.Set("Authorization", "Bearer "+c.secret)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("chain watcher http %d: %s", resp.StatusCode, string(body))
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return err
		}
	}
	return nil
}
