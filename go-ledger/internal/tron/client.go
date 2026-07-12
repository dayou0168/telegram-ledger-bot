package tron

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	baseURL string
	http    *http.Client
	keys    *keyPool
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
	EventIndex     string
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

type FetchMetrics struct {
	Calls         int
	Pages         int
	WaitDuration  time.Duration
	APIDuration   time.Duration
	ParseDuration time.Duration
	LastPageRows  int
	ReachedWindow bool
}

type TransferFetchResult struct {
	Transfers []Transfer
	Metrics   FetchMetrics
}

type RequestMetrics struct {
	Calls         int
	WaitDuration  time.Duration
	APIDuration   time.Duration
	ParseDuration time.Duration
}

func (m *FetchMetrics) addRequest(req RequestMetrics) {
	m.Calls += req.Calls
	m.WaitDuration += req.WaitDuration
	m.APIDuration += req.APIDuration
	m.ParseDuration += req.ParseDuration
}

func (m *FetchMetrics) merge(other FetchMetrics) {
	m.Calls += other.Calls
	m.Pages += other.Pages
	m.WaitDuration += other.WaitDuration
	m.APIDuration += other.APIDuration
	m.ParseDuration += other.ParseDuration
	m.LastPageRows = other.LastPageRows
	m.ReachedWindow = m.ReachedWindow || other.ReachedWindow
}

func NewClient(baseURL, apiKey string, timeout time.Duration) *Client {
	return NewClientWithKeys(baseURL, []string{apiKey}, timeout, KeyPoolOptions{AllowAnonymous: true})
}

func NewPublicFallbackClient(baseURL string, timeout time.Duration) *Client {
	return NewClientWithKeys(baseURL, nil, timeout, KeyPoolOptions{AllowAnonymous: true, PublicFallback: true})
}

func NewClientWithKeys(baseURL string, apiKeys []string, timeout time.Duration, opts KeyPoolOptions) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: timeout},
		keys:    newKeyPool(apiKeys, opts),
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
	c.keys.setMinInterval(interval)
}

func (c *Client) KeyPoolStatus(now time.Time) KeyPoolStatus {
	if c == nil {
		return KeyPoolStatus{}
	}
	return c.keys.status(now)
}

func (c *Client) ConfigureMainBudget(pages int, interval time.Duration) {
	if c != nil {
		c.keys.configureMainBudget(pages, interval)
	}
}

func (c *Client) SetCompensationPressure(lag, target, maximum time.Duration) {
	if c != nil {
		c.keys.setCompensationPressure(lag, target, maximum)
	}
}

func (c *Client) RestoreKeyPool(ctx context.Context) error {
	if c == nil {
		return nil
	}
	return c.keys.restore(ctx)
}

func (c *Client) SeedAndRefreshKeyRegistry(ctx context.Context, configured []string) error {
	if c == nil {
		return nil
	}
	return c.keys.seedRegistry(ctx, configured)
}

func (c *Client) RefreshKeyRegistry(ctx context.Context) error {
	if c == nil {
		return nil
	}
	return c.keys.refreshRegistry(ctx)
}

func (c *Client) RequestKeyProbe(ctx context.Context, fingerprint string, enable bool) error {
	if c == nil {
		return fmt.Errorf("tronscan client is not configured")
	}
	return c.keys.requestProbe(ctx, fingerprint, enable)
}

// ProbeDueKeys performs isolated half-open checks. Probe failures never consume
// a main-scan page job and never fail over to another key.
func (c *Client) ProbeDueKeys(ctx context.Context, contract string) int {
	if c == nil {
		return 0
	}
	leases := c.keys.dueProbeLeases(time.Now())
	for _, lease := range leases {
		values := url.Values{"contract_address": []string{contract}, "limit": []string{"1"}, "start": []string{"0"}, "sort": []string{"-timestamp"}}
		var response tronscanTransferResponse
		_, _ = c.getTimedWithLeaseLimit(ctx, "/token_trc20/transfers", values, &response, RequestSourceOther, &lease, 0)
	}
	return len(leases)
}

func (c *Client) FetchGlobalUSDTTransfers(ctx context.Context, contract string, minTimestamp int64, pages int) ([]Transfer, error) {
	result, err := c.FetchGlobalUSDTTransfersWithMetrics(ctx, contract, minTimestamp, pages)
	return result.Transfers, err
}

func (c *Client) FetchGlobalUSDTTransfersWithMetrics(ctx context.Context, contract string, minTimestamp int64, pages int) (TransferFetchResult, error) {
	return c.fetchGlobalUSDTTransfersWindowWithMetrics(ctx, contract, minTimestamp, 0, 0, pages, RequestSourceMain)
}

func (c *Client) FetchGlobalUSDTTransfersAtWithMetrics(ctx context.Context, contract string, minTimestamp, cutoff int64, pages int) (TransferFetchResult, error) {
	return c.fetchGlobalUSDTTransfersWindowWithMetrics(ctx, contract, minTimestamp, cutoff, 0, pages, RequestSourceMain)
}

func (c *Client) FetchGlobalUSDTTransfersWindowWithMetrics(ctx context.Context, contract string, startTimestamp, endTimestamp int64, pages int) (TransferFetchResult, error) {
	return c.fetchGlobalUSDTTransfersWindowWithMetrics(ctx, contract, startTimestamp, endTimestamp, 0, pages, RequestSourceCompensation)
}

func (c *Client) FetchGlobalUSDTTransfersRangeWithMetrics(ctx context.Context, contract string, startTimestamp, endTimestamp int64, startPage, pages int) (TransferFetchResult, error) {
	return c.fetchGlobalUSDTTransfersWindowWithMetrics(ctx, contract, startTimestamp, endTimestamp, startPage, pages, RequestSourceExpand)
}

func (c *Client) fetchGlobalUSDTTransfersWindowWithMetrics(ctx context.Context, contract string, minTimestamp, endTimestamp int64, startPage, pages int, source RequestSource) (TransferFetchResult, error) {
	if pages < 1 {
		pages = 1
	}
	var result TransferFetchResult
	leases := make([]apiKeyLease, pages)
	var err error
	if source == RequestSourceMain {
		leases, err = c.keys.mainLeases(ctx, pages)
	} else {
		for page := 0; page < pages; page++ {
			leases[page], err = c.keys.lease(ctx, source, nil, false)
			if err != nil {
				return result, err
			}
		}
	}
	if err != nil {
		return result, err
	}
	type pageResult struct {
		page      int
		transfers []Transfer
		metrics   RequestMetrics
		rows      int
		err       error
	}
	results := make(chan pageResult, pages)
	for page := 0; page < pages; page++ {
		page := page
		lease := leases[page]
		go func() {
			values := url.Values{}
			values.Set("contract_address", contract)
			values.Set("limit", "50")
			values.Set("start", strconv.Itoa((startPage+page)*50))
			values.Set("sort", "-timestamp")
			if minTimestamp > 0 {
				values.Set("start_timestamp", strconv.FormatInt(minTimestamp, 10))
			}
			if endTimestamp > 0 {
				values.Set("end_timestamp", strconv.FormatInt(endTimestamp, 10))
			}
			var response tronscanTransferResponse
			reqMetrics, fetchErr := c.getTimedWithLease(ctx, "/token_trc20/transfers", values, &response, source, &lease)
			transfers := make([]Transfer, 0, len(response.TokenTransfers))
			for _, row := range response.TokenTransfers {
				transfer := row.toTransfer()
				transfers = append(transfers, transfer)
			}
			results <- pageResult{page: page, transfers: transfers, metrics: reqMetrics, rows: len(response.TokenTransfers), err: fetchErr}
		}()
	}
	pageResults := make([]pageResult, pages)
	for i := 0; i < pages; i++ {
		pageResult := <-results
		pageResults[pageResult.page] = pageResult
	}
	seen := make(map[string]struct{})
	for _, pageResult := range pageResults {
		result.Metrics.addRequest(pageResult.metrics)
		result.Metrics.Pages++
		result.Metrics.LastPageRows = pageResult.rows
		if pageResult.err != nil && err == nil {
			err = pageResult.err
		}
		for _, transfer := range pageResult.transfers {
			key := TransferIdentity(transfer)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			result.Transfers = append(result.Transfers, transfer)
		}
	}
	sort.SliceStable(result.Transfers, func(i, j int) bool {
		if result.Transfers[i].BlockTimestamp == result.Transfers[j].BlockTimestamp {
			return result.Transfers[i].Hash < result.Transfers[j].Hash
		}
		return result.Transfers[i].BlockTimestamp > result.Transfers[j].BlockTimestamp
	})
	if err != nil {
		return result, err
	}
	return result, nil
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
	result, err := c.FetchAddressUSDTTransfersSincePagesWithMetrics(ctx, address, contract, limit, pages, minTimestamp)
	return result.Transfers, err
}

func (c *Client) FetchAddressUSDTTransfersSincePagesWithMetrics(ctx context.Context, address, contract string, limit int, pages int, minTimestamp int64) (TransferFetchResult, error) {
	if limit < 1 {
		limit = 20
	}
	if pages < 1 {
		pages = 1
	}
	var result TransferFetchResult
	lease, err := c.keys.lease(ctx, RequestSourceCompensation, nil, false)
	if err != nil {
		return result, err
	}
	for page := 0; page < pages; page++ {
		transfers, metrics, err := c.fetchAddressUSDTTransfersPage(ctx, address, contract, limit, page*limit, minTimestamp, &lease)
		if err != nil {
			result.Metrics.addRequest(metrics)
			result.Metrics.Pages++
			return result, err
		}
		result.Metrics.addRequest(metrics)
		result.Metrics.Pages++
		result.Metrics.LastPageRows = len(transfers)
		result.Transfers = append(result.Transfers, transfers...)
		if len(transfers) < limit || reachedTransferWindow(transfers, minTimestamp) {
			result.Metrics.ReachedWindow = reachedTransferWindow(transfers, minTimestamp)
			break
		}
	}
	return result, nil
}

func (c *Client) fetchAddressUSDTTransfersPage(ctx context.Context, address, contract string, limit int, start int, minTimestamp int64, lease *apiKeyLease) ([]Transfer, RequestMetrics, error) {
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
	metrics, err := c.getTimedWithLease(ctx, "/transfer/trc20", values, &result, RequestSourceCompensation, lease)
	if err != nil {
		return nil, metrics, err
	}
	transfers := make([]Transfer, 0, len(result.TokenTransfers))
	for _, row := range result.TokenTransfers {
		transfer := row.toTransfer()
		if minTimestamp > 0 && transfer.BlockTimestamp < minTimestamp {
			continue
		}
		transfers = append(transfers, transfer)
	}
	return transfers, metrics, nil
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
	_, err := c.getTimed(ctx, path, values, out)
	return err
}

func (c *Client) getTimed(ctx context.Context, path string, values url.Values, out any) (RequestMetrics, error) {
	lease, err := c.keys.lease(ctx, RequestSourceOther, nil, false)
	if err != nil {
		return RequestMetrics{}, err
	}
	return c.getTimedWithLease(ctx, path, values, out, RequestSourceOther, &lease)
}

func (c *Client) getTimedWithLease(ctx context.Context, path string, values url.Values, out any, source RequestSource, lease *apiKeyLease) (RequestMetrics, error) {
	return c.getTimedWithLeaseLimit(ctx, path, values, out, source, lease, 2)
}

func (c *Client) getTimedWithLeaseLimit(ctx context.Context, path string, values url.Values, out any, source RequestSource, lease *apiKeyLease, maxFailovers int) (RequestMetrics, error) {
	var metrics RequestMetrics
	apiURL := c.baseURL + path
	if len(values) > 0 {
		apiURL += "?" + values.Encode()
	}
	attempted := make(map[string]struct{})
	var lastKeyError error
	failover := false
	failoverCount := 0
	for {
		if lease == nil {
			return metrics, fmt.Errorf("tronscan request lease is nil")
		}
		waited, err := c.keys.reserve(ctx, *lease, source, failover)
		metrics.WaitDuration += waited
		if err != nil {
			if failoverCount >= maxFailovers {
				return metrics, err
			}
			attempted[lease.fingerprint] = struct{}{}
			next, leaseErr := c.keys.lease(ctx, source, attempted, true)
			if leaseErr != nil {
				if lastKeyError != nil {
					return metrics, lastKeyError
				}
				return metrics, err
			}
			*lease = next
			failover = true
			failoverCount++
			continue
		}
		metrics.Calls++

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
		if err != nil {
			return metrics, err
		}
		req.Header.Set("Accept", "application/json")
		if lease.key != "" {
			req.Header.Set("TRON-PRO-API-KEY", lease.key)
		}
		apiStarted := time.Now()
		c.keys.beginRequest(*lease, source)
		resp, err := c.http.Do(req)
		c.keys.endRequest(*lease, source)
		metrics.APIDuration += time.Since(apiStarted)
		if err != nil {
			_ = c.keys.report(ctx, *lease, 0, err.Error(), 0, time.Now())
			lastKeyError = err
			if failoverCount >= maxFailovers {
				return metrics, err
			}
			attempted[lease.fingerprint] = struct{}{}
			next, leaseErr := c.keys.lease(ctx, source, attempted, true)
			if leaseErr != nil {
				return metrics, err
			}
			*lease = next
			failover = true
			failoverCount++
			continue
		}
		raw, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			_ = c.keys.report(ctx, *lease, 0, readErr.Error(), 0, time.Now())
			return metrics, readErr
		}
		retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
		_ = c.keys.report(ctx, *lease, resp.StatusCode, string(raw), retryAfter, time.Now())
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			httpErr := &HTTPError{StatusCode: resp.StatusCode, Body: string(raw), RetryAfter: retryAfter}
			lastKeyError = httpErr
			if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode >= 500 {
				if failoverCount >= maxFailovers {
					return metrics, httpErr
				}
				attempted[lease.fingerprint] = struct{}{}
				next, leaseErr := c.keys.lease(ctx, source, attempted, true)
				if leaseErr == nil {
					*lease = next
					failover = true
					failoverCount++
					continue
				}
				return metrics, leaseErr
			}
			return metrics, httpErr
		}
		parseStarted := time.Now()
		if err := json.Unmarshal(raw, out); err != nil {
			metrics.ParseDuration += time.Since(parseStarted)
			return metrics, err
		}
		metrics.ParseDuration += time.Since(parseStarted)
		failover = false
		return metrics, nil
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
	EventIndex     stringish     `json:"event_index"`
	EventIndexAlt  stringish     `json:"eventIndex"`
	LogIndex       stringish     `json:"log_index"`
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
	if token.empty() {
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
		EventIndex:     first(string(t.EventIndex), string(t.EventIndexAlt), string(t.LogIndex)),
	}
}

func TransferIdentity(t Transfer) string {
	index := strings.TrimSpace(t.EventIndex)
	if index != "" {
		return strings.ToLower(strings.Join([]string{"tron-mainnet", t.Hash, index, t.TokenAddress}, "|"))
	}
	return strings.ToLower(strings.Join([]string{"tron-mainnet", t.Hash, t.TokenAddress, t.From, t.To, t.Value, strconv.FormatInt(t.BlockTimestamp, 10)}, "|"))
}

type stringish string

func (s *stringish) UnmarshalJSON(raw []byte) error {
	value := strings.TrimSpace(string(raw))
	if value == "" || value == "null" {
		*s = ""
		return nil
	}
	if value[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return err
		}
		*s = stringish(text)
		return nil
	}
	*s = stringish(value)
	return nil
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
