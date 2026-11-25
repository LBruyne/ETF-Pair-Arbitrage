// Package binance reads two public facts from Binance's USD-M futures market
// data: the one-minute index price of a contract, which is the index behind
// the bell anchor, and the venue's schedule of the US equity market, which
// the calendar is compared with. It sends no key and places no order.
package binance

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
)

const (
	baseURL = "https://fapi.binance.com"

	pathIndexKlines     = "/fapi/v1/indexPriceKlines"
	pathTradingSchedule = "/fapi/v1/tradingSchedule"

	// sessionRegular is the schedule's type of a regular session.
	sessionRegular = "REGULAR"

	requestTimeout = 10 * time.Second
)

// Client reads Binance's public futures market data.
type Client struct {
	baseURL string
	http    *http.Client
}

var _ cex.IndexSource = (*Client)(nil)

// NewClient builds the client.
func NewClient() *Client {
	return &Client{baseURL: baseURL, http: &http.Client{Timeout: requestTimeout}}
}

// IndexClose reads the one-minute index price kline of symbol that opens at
// minute and returns its close. The request asks for the first kline at or
// after minute, which may be a later one when the minute has no kline: ok is
// true only when the kline returned opens at exactly that minute.
func (c *Client) IndexClose(ctx context.Context, symbol cex.Symbol, minute time.Time) (price decimal.Decimal, ok bool, err error) {
	query := url.Values{"pair": {string(symbol)}, "interval": {"1m"}, "startTime": {strconv.FormatInt(minute.UnixMilli(), 10)}, "limit": {"1"}}
	body, err := c.get(ctx, pathIndexKlines, query)
	if err != nil {
		return decimal.Decimal{}, false, err
	}
	// Rows of [open time, open, high, low, close, ...]: the time a number,
	// the prices strings.
	var rows [][]json.RawMessage
	if err := json.Unmarshal(body, &rows); err != nil {
		return decimal.Decimal{}, false, fmt.Errorf("binance: index klines of %s: %w", symbol, err)
	}
	if len(rows) == 0 {
		return decimal.Decimal{}, false, nil
	}
	if len(rows[0]) < 5 {
		return decimal.Decimal{}, false, fmt.Errorf("binance: index kline of %s has %d columns", symbol, len(rows[0]))
	}
	openMs, err := strconv.ParseInt(string(rows[0][0]), 10, 64)
	if err != nil {
		return decimal.Decimal{}, false, fmt.Errorf("binance: index kline of %s: open time %s", symbol, rows[0][0])
	}
	if openMs != minute.UnixMilli() {
		return decimal.Decimal{}, false, nil
	}
	price, err = decimal.NewFromString(strings.Trim(string(rows[0][4]), `"`))
	if err != nil || !price.IsPositive() {
		return decimal.Decimal{}, false, fmt.Errorf("binance: index kline of %s: close %s", symbol, rows[0][4])
	}
	return price, true, nil
}

// TradingSchedule reads the venue's schedule of the markets under its TradFi
// contracts, about a week back and a week ahead, and returns the regular
// sessions of the US equity market, one per trading day, oldest first.
func (c *Client) TradingSchedule(ctx context.Context) ([]cex.ScheduleDay, error) {
	body, err := c.get(ctx, pathTradingSchedule, nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		MarketSchedules map[string]struct {
			Sessions []struct {
				StartMs int64  `json:"startTime"`
				EndMs   int64  `json:"endTime"`
				Type    string `json:"type"`
			} `json:"sessions"`
		} `json:"marketSchedules"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("binance: trading schedule: %w", err)
	}
	var out []cex.ScheduleDay
	for _, s := range resp.MarketSchedules["EQUITY"].Sessions {
		if s.Type != sessionRegular {
			continue
		}
		open := time.UnixMilli(s.StartMs)
		out = append(out, cex.ScheduleDay{Date: open.In(cex.NewYork).Format("2006-01-02"), RegularOpen: open, RegularClose: time.UnixMilli(s.EndMs)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RegularOpen.Before(out[j].RegularOpen) })
	return out, nil
}

// get reads one public endpoint and returns its body.
func (c *Client) get(ctx context.Context, path string, query url.Values) ([]byte, error) {
	target := c.baseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("binance: GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("binance: GET %s: reading the body: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("binance: GET %s: http %d", path, resp.StatusCode)
	}
	return body, nil
}
