// Package direxion reads a Direxion fund's net asset value and distributions
// through the data API that direxion.com itself calls: an AWS AppSync
// GraphQL endpoint that takes the site's API key in the X-Api-Key header.
// The owner authorised two read-only queries of the pair's funds, at the
// pace cex reads them. The key comes from the secrets file and appears in
// the request header only: never in code, logs, errors or the database.
package direxion

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
)

const (
	endpoint = "https://p6vg2dlkpverjbr2afvpbqupky.appsync-api.us-east-1.amazonaws.com/graphql"

	// The query text is the site's own. getThirtyDayPrice gives the NAV of
	// one of the fund's last thirty trading days, null for a day it does not
	// have (the newest one before it is published); getKurtosysFunds gives
	// the fund's distributions as declared.
	queryThirtyDayPrice = `query GetThirtyDayPrice($Ticker: String!, $TradeDate: String!) {
  getThirtyDayPrice(Ticker: $Ticker, TradeDate: $TradeDate) {
    ItemKey
    TradeDate
    Ticker
    Nav
    MarketClosingPrice
  }
}
`
	queryKurtosysFunds = `query GetKurtosysFunds($Ticker: String!) {
  getKurtosysFunds(Ticker: $Ticker) {
    Distributions {
      IncomeDividend
      ShortTermCapitalGain
      LongTermCapitalGain
      ReturnOfCapital
      ExDate
    }
    TimeStamp
    TradeDate
  }
}
`

	// wireDate is how the API writes a date: MMDDYYYY.
	wireDate = "01022006"

	// requestPause spaces the requests: every query waits it first.
	requestPause = time.Second

	requestTimeout = 10 * time.Second
)

// Client reads Direxion's site API.
type Client struct {
	endpoint string
	key      string // goes in the request header only
	pause    time.Duration
	http     *http.Client
}

var _ cex.IssuerSource = (*Client)(nil)

// NewClient binds the site's API key. With an empty key the client makes no
// request and every read returns cex.ErrIssuerKey.
func NewClient(siteAPIKey string) *Client {
	return &Client{endpoint: endpoint, key: siteAPIKey, pause: requestPause, http: &http.Client{Timeout: requestTimeout}}
}

// NAV asks getThirtyDayPrice for the fund's NAV of one trading day, rounded
// to the cent, the precision the issuer publishes. ok is false when the API
// does not have that day: before it is published, or older than thirty
// trading days.
func (c *Client) NAV(ctx context.Context, ticker string, tradeDate time.Time) (nav decimal.Decimal, ok bool, err error) {
	want := tradeDate.Format(wireDate)
	body, err := c.query(ctx, queryThirtyDayPrice, map[string]string{"Ticker": ticker, "TradeDate": want})
	if err != nil {
		return decimal.Decimal{}, false, err
	}
	var resp struct {
		Data struct {
			Price *struct {
				Nav       *json.Number `json:"Nav"`
				TradeDate string       `json:"TradeDate"`
			} `json:"getThirtyDayPrice"`
		} `json:"data"`
		Errors []apiError `json:"errors"`
	}
	if err := decodeNumbers(body, &resp); err != nil {
		return decimal.Decimal{}, false, fmt.Errorf("direxion: nav of %s: %w", ticker, err)
	}
	if len(resp.Errors) > 0 {
		return decimal.Decimal{}, false, fmt.Errorf("direxion: nav of %s: %s %s", ticker, resp.Errors[0].ErrorType, resp.Errors[0].Message)
	}
	p := resp.Data.Price
	if p == nil {
		return decimal.Decimal{}, false, nil
	}
	if p.TradeDate != want {
		return decimal.Decimal{}, false, fmt.Errorf("direxion: nav of %s: asked for trade date %s, got %q", ticker, want, p.TradeDate)
	}
	if p.Nav == nil {
		return decimal.Decimal{}, false, fmt.Errorf("direxion: nav of %s on %s: no value", ticker, want)
	}
	nav, err = decimal.NewFromString(p.Nav.String())
	if err != nil || !nav.IsPositive() {
		return decimal.Decimal{}, false, fmt.Errorf("direxion: nav of %s on %s: %q", ticker, want, p.Nav.String())
	}
	return nav.Round(2), true, nil
}

// Distributions asks getKurtosysFunds for the fund's distributions as
// declared, one per ex-date in the API's order: the cash per share of
// income, short- and long-term capital gains and return of capital summed. A
// row without any amount is a round the fund lists before its amounts: its
// ex-date is returned with Declared false.
func (c *Client) Distributions(ctx context.Context, ticker string) ([]cex.Distribution, error) {
	body, err := c.query(ctx, queryKurtosysFunds, map[string]string{"Ticker": ticker})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Data struct {
			Fund *struct {
				Distributions []struct {
					Income    *json.Number `json:"IncomeDividend"`
					ShortTerm *json.Number `json:"ShortTermCapitalGain"`
					LongTerm  *json.Number `json:"LongTermCapitalGain"`
					Capital   *json.Number `json:"ReturnOfCapital"`
					ExDate    string       `json:"ExDate"`
				} `json:"Distributions"`
			} `json:"getKurtosysFunds"`
		} `json:"data"`
		Errors []apiError `json:"errors"`
	}
	if err := decodeNumbers(body, &resp); err != nil {
		return nil, fmt.Errorf("direxion: distributions of %s: %w", ticker, err)
	}
	if len(resp.Errors) > 0 {
		return nil, fmt.Errorf("direxion: distributions of %s: %s %s", ticker, resp.Errors[0].ErrorType, resp.Errors[0].Message)
	}
	if resp.Data.Fund == nil {
		return nil, fmt.Errorf("direxion: distributions of %s: no fund data", ticker)
	}
	var out []cex.Distribution
	byDate := map[string]int{} // ex-date -> its index in out
	for _, row := range resp.Data.Fund.Distributions {
		day, err := time.Parse(wireDate, row.ExDate)
		if err != nil {
			return nil, fmt.Errorf("direxion: distributions of %s: ex-date %q", ticker, row.ExDate)
		}
		date := day.Format("2006-01-02")
		i, seen := byDate[date]
		if !seen {
			i = len(out)
			byDate[date] = i
			out = append(out, cex.Distribution{ExDate: date})
		}
		for _, part := range []*json.Number{row.Income, row.ShortTerm, row.LongTerm, row.Capital} {
			if part == nil {
				continue
			}
			amount, err := decimal.NewFromString(part.String())
			if err != nil {
				return nil, fmt.Errorf("direxion: distributions of %s on %s: amount %q", ticker, date, part.String())
			}
			out[i].Amount, out[i].Declared = out[i].Amount.Add(amount), true
		}
	}
	return out, nil
}

// apiError is one entry of a GraphQL response's errors.
type apiError struct {
	ErrorType string `json:"errorType"`
	Message   string `json:"message"`
}

// query posts one GraphQL query, after the pause, and returns the response
// body. Without a key, or when the API refuses the key, it returns
// cex.ErrIssuerKey; no error text carries the key.
func (c *Client) query(ctx context.Context, text string, variables map[string]string) ([]byte, error) {
	if c.key == "" {
		return nil, fmt.Errorf("direxion: no site API key configured: %w", cex.ErrIssuerKey)
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(c.pause):
	}
	payload, err := json.Marshal(map[string]any{"query": text, "variables": variables})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", c.key)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("direxion: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("direxion: reading the body: %w", err)
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("direxion: http %d: %w", resp.StatusCode, cex.ErrIssuerKey)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("direxion: http %d", resp.StatusCode)
	}
	return body, nil
}

// decodeNumbers decodes JSON keeping every number's digits as sent.
func decodeNumbers(body []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	return dec.Decode(out)
}
