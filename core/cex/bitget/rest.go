package bitget

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"etf-pair-arbitrage/core/cex"
)

// errNotSent is PlaceOrder's answer: placing is not implemented, so nothing
// left the process.
var errNotSent = fmt.Errorf("%w: %w", cex.ErrNotSent, errNotImplemented)

// MarketRules reads GET /api/v3/market/instruments once per contract.
func (c *Client) MarketRules(ctx context.Context, symbols []cex.Symbol) (map[cex.Symbol]cex.MarketRule, error) {
	out := make(map[cex.Symbol]cex.MarketRule, len(symbols))
	for _, symbol := range symbols {
		var rows []instrumentRow
		if err := c.getPublic(ctx, pathInstruments, url.Values{"category": {restCategory}, "symbol": {string(symbol)}}, &rows); err != nil {
			return nil, err
		}
		if len(rows) != 1 || rows[0].Symbol != string(symbol) {
			return nil, fmt.Errorf("bitget: instruments returned %d rows for %s", len(rows), symbol)
		}
		rule, err := marketRule(rows[0])
		if err != nil {
			return nil, err
		}
		out[symbol] = rule
	}
	return out, nil
}

// FundingHistory reads GET /api/v3/market/history-fund-rate: the contract's
// last settlements, newest first, at most limit of them and at most one page.
func (c *Client) FundingHistory(ctx context.Context, symbol cex.Symbol, limit int) ([]cex.FundingSettlement, error) {
	params := url.Values{"category": {restCategory}, "symbol": {string(symbol)}, "limit": {strconv.Itoa(min(limit, fundingHistoryPage))}}
	var data fundingHistoryData
	if err := c.getPublic(ctx, pathFundingHistory, params, &data); err != nil {
		return nil, err
	}
	out := make([]cex.FundingSettlement, 0, len(data.ResultList))
	for _, row := range data.ResultList {
		s, err := fundingSettlement(symbol, row)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// getPublic calls one public GET endpoint and decodes the envelope's data
// into out. The request carries no key and no signature. A code other than
// success is a *cex.VenueError.
func (c *Client) getPublic(ctx context.Context, path string, params url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.restURL+path+"?"+params.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("bitget: GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("bitget: GET %s: reading the body: %w", path, err)
	}
	var env restEnvelope
	if err := json.Unmarshal(body, &env); err != nil || env.Code == "" {
		return fmt.Errorf("bitget: GET %s: http %d without an envelope", path, resp.StatusCode)
	}
	if env.Code != codeSuccess {
		return fmt.Errorf("bitget: GET %s: %w", path, &cex.VenueError{Code: env.Code, Message: env.Msg, Kind: cex.ErrorOther})
	}
	if len(env.Data) == 0 {
		return errors.New("bitget: GET " + path + ": the envelope has no data")
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("bitget: GET %s: decoding the data: %w", path, err)
	}
	return nil
}
