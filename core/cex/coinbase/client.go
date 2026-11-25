// Package coinbase reads USDT-USD from the public market data of Coinbase
// Exchange: the best bid and ask behind F, and the one-minute candles that
// stand in for the minutes the process did not record itself. It sends no
// key.
package coinbase

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"time"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
)

const (
	baseURL = "https://api.exchange.coinbase.com"

	pathTicker  = "/products/USDT-USD/ticker"
	pathCandles = "/products/USDT-USD/candles"

	// candlePage is the most candles one request returns.
	candlePage = 300

	requestTimeout = 10 * time.Second
)

// Client reads Coinbase Exchange's public market data of USDT-USD.
type Client struct {
	baseURL string
	http    *http.Client
}

var _ cex.FXSource = (*Client)(nil)

// NewClient builds the client.
func NewClient() *Client {
	return &Client{baseURL: baseURL, http: &http.Client{Timeout: requestTimeout}}
}

// Quote reads the ticker's best bid and ask, USD per USDT.
func (c *Client) Quote(ctx context.Context) (bid, ask decimal.Decimal, err error) {
	body, err := c.get(ctx, pathTicker, nil)
	if err != nil {
		return decimal.Decimal{}, decimal.Decimal{}, err
	}
	var row struct {
		Bid string `json:"bid"`
		Ask string `json:"ask"`
	}
	if err := json.Unmarshal(body, &row); err != nil {
		return decimal.Decimal{}, decimal.Decimal{}, fmt.Errorf("coinbase: ticker: %w", err)
	}
	if bid, err = decimal.NewFromString(row.Bid); err != nil {
		return decimal.Decimal{}, decimal.Decimal{}, fmt.Errorf("coinbase: ticker: bid %q", row.Bid)
	}
	if ask, err = decimal.NewFromString(row.Ask); err != nil {
		return decimal.Decimal{}, decimal.Decimal{}, fmt.Errorf("coinbase: ticker: ask %q", row.Ask)
	}
	return bid, ask, nil
}

// MinuteClose returns the close of the one-minute candle that starts at
// minute. ok is false when that minute has no candle: it had no trade.
func (c *Client) MinuteClose(ctx context.Context, minute time.Time) (price decimal.Decimal, ok bool, err error) {
	candles, err := c.MinuteCloses(ctx, minute, minute.Add(time.Minute))
	if err != nil || len(candles) == 0 {
		return decimal.Decimal{}, false, err
	}
	return candles[0].Close, true, nil
}

// MinuteCloses returns the closes of the one-minute candles that start in
// [from, to), oldest first, a page of candlePage minutes per request.
// Minutes without a trade have no candle and are left out.
func (c *Client) MinuteCloses(ctx context.Context, from, to time.Time) ([]cex.FXMinute, error) {
	from, to = from.Truncate(time.Minute), to.Truncate(time.Minute)
	var out []cex.FXMinute
	for start := from; start.Before(to); start = start.Add(candlePage * time.Minute) {
		end := start.Add(candlePage * time.Minute)
		if end.After(to) {
			end = to
		}
		// The request's end is inclusive: the last minute asked for.
		query := url.Values{"granularity": {"60"}, "start": {start.UTC().Format(time.RFC3339)}, "end": {end.Add(-time.Minute).UTC().Format(time.RFC3339)}}
		body, err := c.get(ctx, pathCandles, query)
		if err != nil {
			return nil, err
		}
		// Rows of [time in seconds, low, high, open, close, volume], all
		// numbers, newest first.
		var rows [][]json.Number
		if err := json.Unmarshal(body, &rows); err != nil {
			return nil, fmt.Errorf("coinbase: candles: %w", err)
		}
		for _, row := range rows {
			if len(row) < 5 {
				return nil, fmt.Errorf("coinbase: candle has %d columns", len(row))
			}
			seconds, err := row[0].Int64()
			if err != nil {
				return nil, fmt.Errorf("coinbase: candle time %s", row[0])
			}
			minute := time.Unix(seconds, 0)
			if minute.Before(start) || !minute.Before(end) {
				continue
			}
			closePrice, err := decimal.NewFromString(row[4].String())
			if err != nil || !closePrice.IsPositive() {
				return nil, fmt.Errorf("coinbase: candle close %s", row[4])
			}
			out = append(out, cex.FXMinute{Minute: minute, Close: closePrice})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Minute.Before(out[j].Minute) })
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
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("coinbase: GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("coinbase: GET %s: reading the body: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("coinbase: GET %s: http %d", path, resp.StatusCode)
	}
	return body, nil
}
