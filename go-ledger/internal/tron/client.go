package tron

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
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
	values := url.Values{}
	values.Set("contract_address", contract)
	values.Set("relatedAddress", address)
	values.Set("limit", strconv.Itoa(limit))
	values.Set("start", "0")
	values.Set("sort", "-timestamp")
	var result tronscanTransferResponse
	if err := c.get(ctx, "/token_trc20/transfers", values, &result); err != nil {
		return nil, err
	}
	transfers := make([]Transfer, 0, len(result.TokenTransfers))
	for _, row := range result.TokenTransfers {
		transfers = append(transfers, row.toTransfer())
	}
	return transfers, nil
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
		CreatedAt:             result.DateCreated,
		LatestOperationAt:     result.LatestOperationAt,
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
		return fmt.Errorf("tronscan http %d: %s", resp.StatusCode, string(raw))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return err
	}
	return nil
}

type tronscanTransferResponse struct {
	TokenTransfers []tronscanTransfer `json:"token_transfers"`
	Data           []tronscanTransfer `json:"data"`
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
	BlockTimestamp int64         `json:"block_ts"`
	Timestamp      int64         `json:"block_timestamp"`
	Confirmed      bool          `json:"confirmed"`
	TokenInfo      tronscanToken `json:"tokenInfo"`
	TokenInfoAlt   tronscanToken `json:"token_info"`
}

type tronscanToken struct {
	Symbol    string `json:"tokenAbbr"`
	Symbol2   string `json:"symbol"`
	Address   string `json:"tokenId"`
	Address2  string `json:"address"`
	Decimals  int    `json:"tokenDecimal"`
	Decimals2 int    `json:"decimals"`
}

func (t tronscanTransfer) toTransfer() Transfer {
	token := t.TokenInfo
	if token.Symbol == "" && token.Symbol2 == "" {
		token = t.TokenInfoAlt
	}
	hash := first(t.Hash, t.TransactionID)
	value := first(t.Quant, t.Value)
	ts := t.BlockTimestamp
	if ts == 0 {
		ts = t.Timestamp
	}
	return Transfer{
		Hash:           hash,
		From:           first(t.From, t.FromAlt),
		To:             first(t.To, t.ToAlt),
		Value:          value,
		TokenSymbol:    first(token.Symbol, token.Symbol2),
		TokenAddress:   first(token.Address, token.Address2),
		TokenDecimals:  firstInt(token.Decimals, token.Decimals2, 6),
		BlockTimestamp: ts,
		Confirmed:      t.Confirmed,
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
