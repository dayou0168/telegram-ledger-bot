package tron

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Client struct {
	baseURL     string
	apiKey      string
	http        *http.Client
	throttle    sync.Mutex
	minInterval time.Duration
	nextRequest time.Time
}

type Transfer struct {
	Hash           string
	From           string
	To             string
	Value          string
	TokenSymbol    string
	TokenAddress   string
	TokenDecimals  int
	BlockTimestamp int64
	Confirmed      bool
}

type Account struct {
	Address               string
	BalanceSun            string
	USDTBalance           string
	USDTDecimals          int
	TransactionsIn        int64
	TransactionsOut       int64
	TotalTransactionCount int64
	CreatedAt             int64
	LatestOperationAt     int64
}

func NewClient(baseURL, apiKey string, timeout time.Duration) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  strings.TrimSpace(apiKey),
		http:    &http.Client{Timeout: timeout},
	}
}

type HTTPError struct {
	StatusCode int
	Body       string
	RetryAfter time.Duration
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("tronscan http %d: %s", e.StatusCode, e.Body)
}

func (e *HTTPError) RateLimited() bool {
	return e != nil && e.StatusCode == http.StatusTooManyRequests
}

func IsRateLimited(err error) (*HTTPError, bool) {
	var httpErr *HTTPError
	ok := errors.As(err, &httpErr)
	return httpErr, ok && httpErr.RateLimited()
}

func (c *Client) SetMinRequestInterval(interval time.Duration) {
	c.throttle.Lock()
	defer c.throttle.Unlock()
	if interval < 0 {
		interval = 0
	}
	c.minInterval = interval
}

func (c *Client) FetchGlobalUSDTTransfers(ctx context.Context, contract string, minTimestamp int64, pages int) ([]Transfer, error) {
	if pages < 1 {
		pages = 1
	}
	var all []Transfer
	for page := 0; page < pages; page++ {
		values := url.Values{}
		values.Set("contract_address", contract)
		values.Set("limit", "50")
		values.Set("start", strconv.Itoa(page*50))
		values.Set("sort", "-timestamp")
		if minTimestamp > 0 {
			values.Set("start_timestamp", strconv.FormatInt(minTimestamp, 10))
		}
		var result tronscanTransferResponse
		if err := c.get(ctx, "/token_trc20/transfers", values, &result); err != nil {
			return nil, err
		}
		for _, row := range result.TokenTransfers {
			all = append(all, row.toTransfer())
		}
		if len(result.TokenTransfers) < 50 {
			break
		}
	}
	return all, nil
}

func (c *Client) FetchAddressUSDTTransfers(ctx context.Context, address, contract string, limit int) ([]Transfer, error) {
	if limit < 1 {
		limit = 5
	}
	return c.FetchAddressUSDTTransfersSince(ctx, address, contract, limit, 0)
}

func (c *Client) FetchAddressUSDTTransfersSince(ctx context.Context, address, contract string, limit int, minTimestamp int64) ([]Transfer, error) {
	return c.FetchAddressUSDTTransfersSincePages(ctx, address, contract, limit, 1, minTimestamp)
}

func (c *Client) FetchAddressUSDTTransfersSincePages(ctx context.Context, address, contract string, limit int, pages int, minTimestamp int64) ([]Transfer, error) {
	if limit < 1 {
		limit = 20
	}
	if pages < 1 {
		pages = 1
	}
	var all []Transfer
	for page := 0; page < pages; page++ {
		transfers, err := c.fetchAddressUSDTTransfersPage(ctx, address, contract, limit, page*limit, minTimestamp)
		if err != nil {
			return nil, err
		}
		all = append(all, transfers...)
		if len(transfers) < limit || reachedTransferWindow(transfers, minTimestamp) {
			break
		}
	}
	return all, nil
}

func (c *Client) fetchAddressUSDTTransfersPage(ctx context.Context, address, contract string, limit int, start int, minTimestamp int64) ([]Transfer, error) {
	values := url.Values{}
	values.Set("address", address)
	values.Set("trc20Id", contract)
	values.Set("limit", strconv.Itoa(limit))
	values.Set("start", strconv.Itoa(start))
	values.Set("direction", "0")
	values.Set("reverse", "true")
	if minTimestamp > 0 {
		values.Set("start_timestamp", strconv.FormatInt(minTimestamp, 10))
	}
	var result tronscanTransferResponse
	if err := c.get(ctx, "/transfer/trc20", values, &result); err != nil {
		return nil, err
	}
	transfers := make([]Transfer, 0, len(result.TokenTransfers))
	for _, row := range result.TokenTransfers {
		transfer := row.toTransfer()
		if minTimestamp > 0 && transfer.BlockTimestamp < minTimestamp {
			continue
		}
		transfers = append(transfers, transfer)
	}
	return transfers, nil
}

func reachedTransferWindow(transfers []Transfer, minTimestamp int64) bool {
	if minTimestamp <= 0 || len(transfers) == 0 {
		return false
	}
	oldest := transfers[len(transfers)-1].BlockTimestamp
	return oldest <= minTimestamp
}

func (c *Client) FetchAccount(ctx context.Context, address, usdtContract string) (Account, error) {
	values := url.Values{}
	values.Set("address", address)
	var result tronscanAccountResponse
	if err := c.get(ctx, "/account", values, &result); err != nil {
		return Account{}, err
	}
	account := Account{
		Address:               first(result.Address, address),
		BalanceSun:            result.Balance.String(),
		TransactionsIn:        result.TransactionsIn,
		TransactionsOut:       result.TransactionsOut,
		TotalTransactionCount: result.TotalTransactionCount,
		CreatedAt:             normalizeTimestampMillis(result.DateCreated),
		LatestOperationAt:     normalizeTimestampMillis(result.LatestOperationAt),
	}
	for _, token := range result.TRC20TokenBalances {
		if strings.EqualFold(token.TokenID, usdtContract) || strings.EqualFold(token.TokenAbbr, "USDT") {
			account.USDTBalance = token.Balance
			account.USDTDecimals = firstInt(token.TokenDecimal, 6)
			break
		}
	}
	return account, nil
}

func (c *Client) get(ctx context.Context, path string, values url.Values, out any) error {
	if err := c.waitThrottle(ctx); err != nil {
		return err
	}
	apiURL := c.baseURL + path
	if len(values) > 0 {
		apiURL += "?" + values.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if c.apiKey != "" {
		req.Header.Set("TRON-PRO-API-KEY", c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &HTTPError{
			StatusCode: resp.StatusCode,
			Body:       string(raw),
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()),
		}
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return err
	}
	return nil
}

func (c *Client) waitThrottle(ctx context.Context) error {
	c.throttle.Lock()
	interval := c.minInterval
	if interval <= 0 {
		c.throttle.Unlock()
		return nil
	}
	now := time.Now()
	wait := c.nextRequest.Sub(now)
	if wait <= 0 {
		c.nextRequest = now.Add(interval)
		c.throttle.Unlock()
		return nil
	}
	c.nextRequest = c.nextRequest.Add(interval)
	c.throttle.Unlock()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func parseRetryAfter(raw string, now time.Time) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(raw); err == nil {
		if seconds < 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(raw); err == nil {
		if at.Before(now) {
			return 0
		}
		return at.Sub(now)
	}
	return 0
}

type tronscanTransferResponse struct {
	TokenTransfers []tronscanTransfer `json:"token_transfers"`
	Data           []tronscanTransfer `json:"data"`
	TokenInfo      tronscanToken      `json:"tokenInfo"`
}

type tronscanAccountResponse struct {
	Address               string                 `json:"address"`
	Balance               json.Number            `json:"balance"`
	TransactionsIn        int64                  `json:"transactions_in"`
	TransactionsOut       int64                  `json:"transactions_out"`
	TotalTransactionCount int64                  `json:"totalTransactionCount"`
	DateCreated           int64                  `json:"date_created"`
	LatestOperationAt     int64                  `json:"latest_operation_time"`
	TRC20TokenBalances    []tronscanTokenBalance `json:"trc20token_balances"`
}

type tronscanTokenBalance struct {
	TokenID      string `json:"tokenId"`
	Balance      string `json:"balance"`
	TokenAbbr    string `json:"tokenAbbr"`
	TokenDecimal int    `json:"tokenDecimal"`
}

func (r *tronscanTransferResponse) UnmarshalJSON(raw []byte) error {
	type alias tronscanTransferResponse
	var a alias
	if err := json.Unmarshal(raw, &a); err != nil {
		return err
	}
	if len(a.TokenTransfers) == 0 && len(a.Data) > 0 {
		a.TokenTransfers = a.Data
	}
	for i := range a.TokenTransfers {
		if a.TokenTransfers[i].TokenInfo.empty() && a.TokenTransfers[i].TokenInfoAlt.empty() {
			a.TokenTransfers[i].TokenInfo = a.TokenInfo
		}
	}
	*r = tronscanTransferResponse(a)
	return nil
}

type tronscanTransfer struct {
	Hash           string        `json:"transaction_id"`
	TransactionID  string        `json:"hash"`
	From           string        `json:"from_address"`
	FromAlt        string        `json:"from"`
	To             string        `json:"to_address"`
	ToAlt          string        `json:"to"`
	Quant          string        `json:"quant"`
	Value          string        `json:"value"`
	Amount         string        `json:"amount"`
	BlockTimestamp int64         `json:"block_ts"`
	Timestamp      int64         `json:"block_timestamp"`
	Confirmed      boolish       `json:"confirmed"`
	TokenInfo      tronscanToken `json:"tokenInfo"`
	TokenInfoAlt   tronscanToken `json:"token_info"`
	ID             string        `json:"id"`
	Decimals       int           `json:"decimals"`
}

type tronscanToken struct {
	Symbol    string `json:"tokenAbbr"`
	Symbol2   string `json:"symbol"`
	Address   string `json:"tokenId"`
	Address2  string `json:"address"`
	Decimals  int    `json:"tokenDecimal"`
	Decimals2 int    `json:"decimals"`
}

func (t tronscanToken) empty() bool {
	return t.Symbol == "" && t.Symbol2 == "" && t.Address == "" && t.Address2 == "" && t.Decimals == 0 && t.Decimals2 == 0
}

type boolish bool

func (b *boolish) UnmarshalJSON(raw []byte) error {
	rawText := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	switch strings.ToLower(rawText) {
	case "", "null", "false", "0":
		*b = false
	case "true", "1":
		*b = true
	default:
		var n int
		if _, err := fmt.Sscanf(rawText, "%d", &n); err == nil {
			*b = n != 0
			return nil
		}
		*b = false
	}
	return nil
}

func (t tronscanTransfer) toTransfer() Transfer {
	token := t.TokenInfo
	if token.Symbol == "" && token.Symbol2 == "" {
		token = t.TokenInfoAlt
	}
	hash := first(t.Hash, t.TransactionID)
	value := first(t.Quant, t.Value, t.Amount)
	ts := t.BlockTimestamp
	if ts == 0 {
		ts = t.Timestamp
	}
	ts = normalizeTimestampMillis(ts)
	return Transfer{
		Hash:           hash,
		From:           first(t.From, t.FromAlt),
		To:             first(t.To, t.ToAlt),
		Value:          value,
		TokenSymbol:    first(token.Symbol, token.Symbol2),
		TokenAddress:   first(token.Address, token.Address2, t.ID),
		TokenDecimals:  firstInt(token.Decimals, token.Decimals2, t.Decimals, 6),
		BlockTimestamp: ts,
		Confirmed:      bool(t.Confirmed),
	}
}

func normalizeTimestampMillis(ts int64) int64 {
	switch {
	case ts <= 0:
		return 0
	case ts < 1_000_000_000_000:
		return ts * 1000
	case ts > 10_000_000_000_000_000:
		return ts / 1_000_000
	case ts > 10_000_000_000_000:
		return ts / 1000
	default:
		return ts
	}
}

func first(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func firstInt(values ...int) int {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}
