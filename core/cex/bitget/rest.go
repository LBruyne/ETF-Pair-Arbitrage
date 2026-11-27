package bitget

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/logger"
)

// --- request plumbing ---

// errNoCredentials is the answer of every signed request of a client built
// without an API key: nothing is sent.
var errNoCredentials = errors.New("bitget: no API key: the account side needs credentials")

// httpError is an HTTP answer that is not the exchange's verdict on the
// request: a server error (status 5xx, whatever its body), or a body
// without the exchange's JSON envelope - a gateway's page, an empty body.
// Whether the request was carried out is not known from it.
type httpError struct {
	method, path string
	status       int
	code         string // the envelope's code, when the body of a server error had one
}

func (e *httpError) Error() string {
	if e.code != "" {
		return fmt.Sprintf("bitget: %s %s: http %d with code %s", e.method, e.path, e.status, e.code)
	}
	return fmt.Sprintf("bitget: %s %s: http %d without an envelope", e.method, e.path, e.status)
}

// sign is the request signature: the base64 of the HMAC-SHA256 of prehash
// under the API secret.
func sign(secret, prehash string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(prehash))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// call sends one REST request and decodes the envelope's data into out; see
// do.
func (c *Client) call(ctx context.Context, method, path string, query url.Values, body []byte, signed bool, out any) error {
	_, err := c.do(ctx, method, path, query, body, signed, out)
	return err
}

// do sends one REST request, decodes the envelope's data into out and
// returns the response's headers. A signed request carries the key, the
// passphrase, the timestamp in unix milliseconds and the signature of
// timestamp + METHOD + path [+ "?" + query] + body, over exactly the text
// that is sent. An envelope code other than success is a *cex.VenueError,
// also on a 4xx status: the exchange answers refusals with HTTP 400 and the
// same envelope. A 5xx status is a failure of the server, not a refusal,
// whatever its body says. No error text carries a header.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body []byte, signed bool, out any) (http.Header, error) {
	target, q := c.restURL+path, query.Encode()
	if q != "" {
		target += "?" + q
	}
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if signed {
		if c.creds.Key == "" {
			return nil, errNoCredentials
		}
		ts, prehash := strconv.FormatInt(c.now().UnixMilli(), 10), ""
		prehash = ts + method + path
		if q != "" {
			prehash += "?" + q
		}
		prehash += string(body)
		req.Header.Set("ACCESS-KEY", c.creds.Key)
		req.Header.Set("ACCESS-SIGN", sign(c.creds.Secret, prehash))
		req.Header.Set("ACCESS-TIMESTAMP", ts)
		req.Header.Set("ACCESS-PASSPHRASE", c.creds.Passphrase)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("locale", "en-US")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bitget: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return resp.Header, fmt.Errorf("bitget: %s %s: reading the body: %w", method, path, err)
	}
	var env restEnvelope
	if err := json.Unmarshal(raw, &env); err != nil || env.Code == "" || resp.StatusCode >= http.StatusInternalServerError {
		if resp.StatusCode == http.StatusTooManyRequests {
			return resp.Header, fmt.Errorf("bitget: %s %s: %w", method, path, &cex.VenueError{Code: "429", Message: "Too Many Requests", Kind: cex.ErrorRateLimited})
		}
		return resp.Header, &httpError{method: method, path: path, status: resp.StatusCode, code: env.Code}
	}
	if env.Code != codeSuccess {
		return resp.Header, fmt.Errorf("bitget: %s %s: %w", method, path, &cex.VenueError{Code: env.Code, Message: env.Msg, Kind: errorKind(env.Code)})
	}
	if out == nil {
		return resp.Header, nil
	}
	if len(env.Data) == 0 {
		return resp.Header, fmt.Errorf("bitget: %s %s: the envelope has no data", method, path)
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return resp.Header, fmt.Errorf("bitget: %s %s: decoding the data: %w", method, path, err)
	}
	return resp.Header, nil
}

// errorKind groups an envelope code by what the sender does next.
func errorKind(code string) cex.ErrorKind {
	switch {
	case priceLimitCodes[code]:
		return cex.ErrorPriceLimit
	case rateLimitCodes[code]:
		return cex.ErrorRateLimited
	case reduceQtyCodes[code]:
		return cex.ErrorReduceQty
	}
	return cex.ErrorOther
}

// pages reads a list endpoint from its newest row back to the end of the
// result: page after page, each with the cursor of the one before, until a
// page comes back short. query holds the endpoint's own parameters.
func pages[T any](ctx context.Context, c *Client, path string, query url.Values) ([]T, error) {
	var out []T
	query.Set("limit", strconv.Itoa(listPage))
	for {
		var page listData[T]
		if err := c.call(ctx, http.MethodGet, path, query, nil, true, &page); err != nil {
			return nil, err
		}
		out = append(out, page.List...)
		if len(page.List) < listPage || page.Cursor == "" {
			return out, nil
		}
		query.Set("cursor", page.Cursor)
	}
}

// window is the time parameters of a list read: from since until now, in
// unix milliseconds.
func (c *Client) window(since time.Time) url.Values {
	return url.Values{"startTime": {strconv.FormatInt(since.UnixMilli(), 10)}, "endTime": {strconv.FormatInt(c.now().UnixMilli(), 10)}}
}

// --- public reads ---

// MarketRules reads GET /api/v3/market/instruments once per contract.
func (c *Client) MarketRules(ctx context.Context, symbols []cex.Symbol) (map[cex.Symbol]cex.MarketRule, error) {
	out := make(map[cex.Symbol]cex.MarketRule, len(symbols))
	for _, symbol := range symbols {
		var rows []instrumentRow
		if err := c.call(ctx, http.MethodGet, pathInstruments, url.Values{"category": {restCategory}, "symbol": {string(symbol)}}, nil, false, &rows); err != nil {
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
	if err := c.call(ctx, http.MethodGet, pathFundingHistory, params, nil, false, &data); err != nil {
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

// --- account reads ---

// AccountSettings reads GET /api/v3/account/settings: the account's
// position mode, and the margin mode and leverage of those of the given
// contracts the account has a setting for.
func (c *Client) AccountSettings(ctx context.Context, symbols []cex.Symbol) (cex.AccountSettings, error) {
	var data settingsData
	if err := c.call(ctx, http.MethodGet, pathSettings, nil, nil, true, &data); err != nil {
		return cex.AccountSettings{}, err
	}
	return accountSettings(data, symbols)
}

// Positions reads GET /api/v3/position/current-position once per contract:
// the account holds other contracts' positions too, so each read names its
// contract. A contract without a position has no entry.
func (c *Client) Positions(ctx context.Context, symbols []cex.Symbol) (cex.PositionSet, error) {
	set := cex.PositionSet{BySymbol: map[cex.Symbol]cex.Position{}, Loaded: true}
	for _, symbol := range symbols {
		var data listData[positionRow]
		if err := c.call(ctx, http.MethodGet, pathPositions, url.Values{"category": {restCategory}, "symbol": {string(symbol)}}, nil, true, &data); err != nil {
			return cex.PositionSet{}, err
		}
		for _, row := range data.List {
			if err := addPosition(set.BySymbol, row); err != nil {
				return cex.PositionSet{}, err
			}
		}
	}
	set.ReceivedAt = c.now()
	return set, nil
}

// AccountAssets reads GET /api/v3/account/assets: the whole account's
// equity in USDT and its margin ratio, for the board. The account holds
// other programs' positions too, so both are account-wide.
func (c *Client) AccountAssets(ctx context.Context) (cex.AccountAssets, error) {
	var data assetsData
	if err := c.call(ctx, http.MethodGet, pathAssets, nil, nil, true, &data); err != nil {
		return cex.AccountAssets{}, err
	}
	return accountAssets(data, c.now())
}

// OrderByClientID reads GET /api/v3/trade/order-info by the client id. The
// exchange's "Order does not exist" is cex.ErrUnknownOrder. That answer is
// also given for a cancelled order without a fill once the exchange has
// dropped it, some hours after it ended.
func (c *Client) OrderByClientID(ctx context.Context, symbol cex.Symbol, id cex.ClientOrderID) (cex.OrderRecord, error) {
	var row orderRow
	err := c.call(ctx, http.MethodGet, pathOrderInfo, url.Values{"clientOid": {string(id)}}, nil, true, &row)
	var refusal *cex.VenueError
	if errors.As(err, &refusal) && refusal.Code == codeOrderUnknown {
		return cex.OrderRecord{}, fmt.Errorf("bitget: order-info %s: code %s: %w", id, refusal.Code, cex.ErrUnknownOrder)
	}
	if err != nil {
		return cex.OrderRecord{}, err
	}
	return orderRecord(row)
}

// RecentOrders reads GET /api/v3/trade/history-orders of the contract from
// since until now, every page: the finished orders, whoever placed them.
// Cancelled orders without a fill are kept by the exchange for about two
// hours only.
func (c *Client) RecentOrders(ctx context.Context, symbol cex.Symbol, since time.Time) ([]cex.OrderRecord, error) {
	query := c.window(since)
	query.Set("category", restCategory)
	query.Set("symbol", string(symbol))
	rows, err := pages[orderRow](ctx, c, pathHistoryOrders, query)
	if err != nil {
		return nil, err
	}
	out := make([]cex.OrderRecord, 0, len(rows))
	for _, row := range rows {
		rec, err := orderRecord(row)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}

// Fills reads GET /api/v3/trade/fills from since until now, every page: the
// executions of every futures contract of the account. The endpoint has no
// symbol filter; the caller picks its own.
func (c *Client) Fills(ctx context.Context, since time.Time) ([]cex.Fill, error) {
	query := c.window(since)
	query.Set("category", restCategory)
	rows, err := pages[fillRow](ctx, c, pathFills, query)
	if err != nil {
		return nil, err
	}
	now := c.now()
	out := make([]cex.Fill, 0, len(rows))
	for _, row := range rows {
		f, err := fill(row, now)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

// FinancialRecords reads GET /api/v3/account/financial-records from since
// until now, once per funding type: the funding settlements of the
// account's cross-margin positions, of every contract. The ledger also
// holds a row per trade, so the type is filtered in the request; the
// endpoint has no symbol filter.
func (c *Client) FinancialRecords(ctx context.Context, since time.Time) ([]cex.FinancialRecord, error) {
	var out []cex.FinancialRecord
	for _, recordType := range fundingRecordTypes {
		query := c.window(since)
		query.Set("category", restCategory)
		query.Set("type", recordType)
		rows, err := pages[financialRecordRow](ctx, c, pathFinancialRecords, query)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			rec, err := financialRecord(row)
			if err != nil {
				return nil, err
			}
			out = append(out, rec)
		}
	}
	return out, nil
}

// --- the command ---

// PlaceOrder sends one IOC limit order, by the order transport in use. With
// the ws transport the order goes out on the trade connection; while that
// connection is not up the order goes by REST, so an order never waits for
// a reconnect. An order is sent once, on one transport. An acceptance means
// the exchange took the request, nothing more. The request is not
// idempotent on the client id - the exchange checks it against open orders
// only, and an IOC order is finished within milliseconds - so the error
// says exactly what is known:
//
//   - cex.ErrNotSent: the request provably never left: no credentials, or
//     the REST connection could not be opened;
//   - cex.ErrOrderStateUnknown: the exchange may have the order: a timeout,
//     a connection lost after the request was written, a server error, an
//     answer without an envelope, an error frame of the trade connection
//     that names no request, or one of the codes the exchange gives when it
//     does not know either;
//   - *cex.VenueError: the exchange refused it, with its code and kind.
func (c *Client) PlaceOrder(ctx context.Context, req cex.OrderRequest) (cex.OrderAck, error) {
	if c.orderTransport == cex.OrderTransportWS {
		if ack, err, sent := c.trade.place(ctx, req); sent {
			return ack, err
		}
		logger.GlobalLogger.Warn("bitget: trade connection not up; the order goes by REST", "client_order_id", req.ClientOrderID)
	}
	return c.placeOrderREST(ctx, req)
}

// placeOrderREST sends the order with POST /api/v3/trade/place-order. The
// acceptance carries the time the exchange's application took the request,
// from the response's headers.
func (c *Client) placeOrderREST(ctx context.Context, req cex.OrderRequest) (cex.OrderAck, error) {
	body, err := json.Marshal(placeOrderBody{Category: restCategory, Symbol: string(req.Symbol), Side: string(req.Side), OrderType: "limit", TimeInForce: "ioc",
		Price: req.Price.String(), Qty: req.Qty.String(), ReduceOnly: reduceOnlyText(req, "yes", "no"), MarginMode: marginModeCrossed, ClientOid: string(req.ClientOrderID)})
	if err != nil {
		return cex.OrderAck{}, fmt.Errorf("%w: %v", cex.ErrNotSent, err)
	}
	var data placeOrderData
	header, err := c.do(ctx, http.MethodPost, pathPlaceOrder, nil, body, true, &data)
	var refusal *cex.VenueError
	var dial *net.OpError
	switch {
	case err == nil:
		return cex.OrderAck{OrderID: cex.OrderID(data.OrderID), ReceivedAt: c.now(), AcceptedAt: wireTime(wireNumber(header.Get(headerAcceptTime)))}, nil
	case errors.Is(err, errNoCredentials), errors.As(err, &dial) && dial.Op == "dial":
		return cex.OrderAck{}, fmt.Errorf("%w: %v", cex.ErrNotSent, err)
	case errors.As(err, &refusal):
		return cex.OrderAck{}, refusalOrUnknown(refusal, err)
	default:
		return cex.OrderAck{}, fmt.Errorf("%w: %v", cex.ErrOrderStateUnknown, err)
	}
}

// refusalOrUnknown is the error of an order the exchange answered with a
// code: the refusal itself, or "may have arrived" for the codes the
// exchange gives when it does not know whether it took the order.
func refusalOrUnknown(refusal *cex.VenueError, err error) error {
	if outcomeUnknownCodes[refusal.Code] {
		return fmt.Errorf("%w: %v", cex.ErrOrderStateUnknown, err)
	}
	return err
}

// reduceOnlyText is the order's reduce-only flag in the spelling of the
// transport.
func reduceOnlyText(req cex.OrderRequest, yes, no string) string {
	if req.ReduceOnly {
		return yes
	}
	return no
}
