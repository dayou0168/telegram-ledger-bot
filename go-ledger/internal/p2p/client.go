package p2p

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	baseURL  string
	frontAPI string
	http     *http.Client
}

type OrderBookEntry struct {
	Rank         int
	Price        string
	MerchantName string
}

func NewClient(baseURL, frontAPI string, timeout time.Duration) *Client {
	return &Client{
		baseURL:  strings.TrimRight(baseURL, "/"),
		frontAPI: strings.TrimSpace(frontAPI),
		http:     &http.Client{Timeout: timeout},
	}
}

func (c *Client) FetchOrderBookTop(ctx context.Context, market, fiatUnit, asset string, tradeMethods []string, limit int) ([]OrderBookEntry, error) {
	if limit < 1 {
		limit = 10
	}
	body := map[string]any{
		"fiatUnit":            fiatUnit,
		"market1":             market,
		"market2":             market,
		"asset1":              asset,
		"asset2":              asset,
		"tradeMethods1":       tradeMethods,
		"tradeMethods2":       tradeMethods,
		"tradeType1":          "BUY",
		"tradeType2":          "BUY",
		"amount1":             "",
		"amount2":             "",
		"only_merchants1":     false,
		"only_merchants2":     false,
		"only_merchants_pro1": false,
		"only_merchants_pro2": false,
		"user_orders1from":    "",
		"user_orders1to":      "",
		"user_orders2from":    "",
		"user_orders2to":      "",
		"user_reviews1from":   "",
		"user_reviews1to":     "",
		"user_reviews2from":   "",
		"user_reviews2to":     "",
		"price1from":          "",
		"price1to":            "",
		"price2from":          "",
		"price2to":            "",
		"limit":               limit,
	}
	var payload orderBookResponse
	if err := c.post(ctx, "/p2p/order-book/monitoring", body, &payload); err != nil {
		return nil, err
	}
	if !payload.Success {
		return nil, fmt.Errorf("p2p order book returned success=false")
	}
	entries := make([]OrderBookEntry, 0, limit)
	for _, item := range payload.Data {
		price := strings.TrimSpace(item.Buy.Price.String())
		if price == "" || parsePrice(price) == nil {
			continue
		}
		rank := item.Pos
		if rank == 0 {
			rank = len(entries) + 1
		}
		merchant := item.Buy.User.Nickname
		if merchant == "" {
			merchant = "-"
		}
		entries = append(entries, OrderBookEntry{Rank: rank, Price: price, MerchantName: merchant})
		if len(entries) >= limit {
			break
		}
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("p2p order book has no usable entries")
	}
	return entries, nil
}

func (c *Client) post(ctx context.Context, path string, payload any, out any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("X-Lang", "en")
	if c.frontAPI != "" {
		req.Header.Set("X-FRONT-API", c.frontAPI)
	}
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
		return fmt.Errorf("p2p http %d: %s", resp.StatusCode, string(body))
	}
	return json.Unmarshal(body, out)
}

type orderBookResponse struct {
	Success bool            `json:"success"`
	Data    []orderBookItem `json:"data"`
}

func (r *orderBookResponse) UnmarshalJSON(raw []byte) error {
	type alias orderBookResponse
	var a alias
	a.Success = true
	if err := json.Unmarshal(raw, &a); err != nil {
		return err
	}
	*r = orderBookResponse(a)
	return nil
}

type orderBookItem struct {
	Pos int         `json:"pos"`
	Buy orderBookAd `json:"buy"`
}

type orderBookAd struct {
	Price jsonStringNumber  `json:"price"`
	User  orderBookMerchant `json:"user"`
}

type orderBookMerchant struct {
	Nickname string `json:"nickname"`
}

type jsonStringNumber string

func (v *jsonStringNumber) UnmarshalJSON(raw []byte) error {
	raw = bytes.TrimSpace(raw)
	if bytes.Equal(raw, []byte("null")) || len(raw) == 0 {
		*v = ""
		return nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		*v = jsonStringNumber(strings.TrimSpace(text))
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var number json.Number
	if err := decoder.Decode(&number); err == nil {
		*v = jsonStringNumber(number.String())
		return nil
	}
	return fmt.Errorf("unsupported json string/number: %s", string(raw))
}

func (v jsonStringNumber) String() string {
	return string(v)
}

func parsePrice(raw string) *big.Rat {
	value, ok := new(big.Rat).SetString(strings.TrimSpace(raw))
	if !ok {
		return nil
	}
	return value
}
