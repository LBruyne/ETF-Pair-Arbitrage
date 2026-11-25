package bitget

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
)

// THIS TEST SENDS REAL ORDERS AND HOLDS A REAL POSITION FOR A FEW SECONDS.
// It runs only with BITGET_ORDER_LATENCY_TEST=1 and
// BITGET_ORDER_LATENCY_AUTHORISED=owner-authorised, which nobody sets
// without the account owner's word for this run.
//
// It verifies order entry over the WebSocket with one round trip on one
// contract (BITGET_ORDER_LATENCY_SYMBOL, SOXLUSDT by default or SOXSUSDT):
// a position of the minimum size is opened and closed, every order on the
// trade connection unless said otherwise. Nothing is sent unless the
// contract is flat by REST, the account is in one-way mode, the trade
// connection and the private stream are up and the best bid and ask are
// younger than two seconds.
//
//   - open: one IOC limit buy for a little over the minimum notional,
//     priced 0.3 % above the best ask, and never for more than three times
//     the minimum notional;
//   - close: one reduce-only IOC limit sell of exactly the filled quantity,
//     0.3 % below the best bid, with the flag spelled as the trade
//     connection spells it (YES). Should the exchange refuse it or should
//     it not leave the position flat, its answer is kept and what is left
//     is sold, each time at a fresh price, until the position is flat: by
//     REST reduce-only, then by REST without reduce-only (in one-way mode a
//     plain sell of what is held closes the long; it is sent only for what
//     a REST read shows held and the orders say is left);
//   - end: the position by REST, which must be flat, and every order by
//     query.
//
// At most six orders, more than a second apart, with client ids that start
// with "lat". What was sent and received for them is saved under
// experiments/core_step5/latency/: each order frame and request body
// (never a header, the login or a signature), the trade connection's
// frames, the private pushes of these orders and of this contract's
// position, the REST requests and answers, all with local times. The log
// says which close worked and, per order, the times, the fill against the
// best price at sending and the fee against 0.65 bps of the order's value.
//
// TestWSRoundTripRehearsal runs the same code against a local exchange.
func TestLiveWSOrderRoundTrip(t *testing.T) {
	if os.Getenv("BITGET_ORDER_LATENCY_TEST") != "1" || os.Getenv("BITGET_ORDER_LATENCY_AUTHORISED") != orderLatencyAuthorised {
		t.Skip("this test sends real orders and opens a position: it needs BITGET_ORDER_LATENCY_TEST=1 and BITGET_ORDER_LATENCY_AUTHORISED=" + orderLatencyAuthorised + ", set only with the account owner's authorisation")
	}
	latencyDir(t)
	symbol, leg := cex.Symbol("SOXLUSDT"), cex.Bull
	switch os.Getenv("BITGET_ORDER_LATENCY_SYMBOL") {
	case "", "SOXLUSDT":
	case "SOXSUSDT":
		symbol, leg = "SOXSUSDT", cex.Bear
	default:
		t.Fatal("BITGET_ORDER_LATENCY_SYMBOL must be SOXLUSDT or SOXSUSDT: no order was sent")
	}
	runWSRoundTrip(t, t, NewClient(liveCredentials(t)), symbol, leg, defaultRoundTripTimings)
}

// --- the round trip ---

// roundTripLog is where the round trip reports: the test itself, or a
// recorder in the rehearsal.
type roundTripLog interface {
	Logf(format string, args ...any)
	Errorf(format string, args ...any)
}

// roundTripTimings are the round trip's clocks.
type roundTripTimings struct {
	gap        time.Duration // two orders are at least this far apart
	finalWait  time.Duration // an order's final state is awaited by push this long, then queried
	upWait     time.Duration // the streams and a fresh best price are awaited this long before anything is sent; the trade connection as long before an order on it
	quoteWait  time.Duration // a close waits this long for a fresh best bid, then is priced on the last one
	reread     time.Duration // a position by REST that disagrees with the orders is read again after this, twice at most
	lastPushes time.Duration // the wait for the last pushes before the end
}

// defaultRoundTripTimings are the clocks against the exchange.
var defaultRoundTripTimings = roundTripTimings{gap: 1100 * time.Millisecond, finalWait: 5 * time.Second, upWait: 30 * time.Second,
	quoteWait: 5 * time.Second, reread: 500 * time.Millisecond, lastPushes: time.Second}

const (
	roundTripMaxOrders = 6               // orders that may reach the exchange in one run
	roundTripQuoteAge  = 2 * time.Second // an order is priced on a best bid and ask younger than this
)

var (
	roundTripThrough = decimal.RequireFromString("0.003") // a limit is this far through the best price: 0.3 %
	roundTripFeeBps  = decimal.RequireFromString("0.65")  // the account's taker fee, the reported fee is set against it
)

// roundTripCloses are the ways the position is closed, in the order they
// are tried: the first that leaves it flat ends the round trip.
var roundTripCloses = []struct {
	tag        string
	ws         bool
	reduceOnly bool
}{
	{tag: "CWU", ws: true, reduceOnly: true},
	{tag: "CRY", reduceOnly: true},
	{tag: "CRP"},
}

// closeName names a close: its transport and how the flag is spelled.
func closeName(ws, reduceOnly bool) string {
	switch {
	case ws:
		return "ws-" + wsReduceOnlyYes
	case reduceOnly:
		return "rest-yes"
	}
	return "rest-plain"
}

// How an order's final state became known.
const (
	finalPush    = "push"
	finalQuery   = "query"
	finalRefused = "refused"
	finalUnknown = "unknown"
)

// wsRoundTrip is one run of the round trip.
type wsRoundTrip struct {
	ctx    context.Context
	log    roundTripLog
	client *Client
	symbol cex.Symbol
	leg    cex.Leg
	timing roundTripTimings
	prefix string // every client id of the run starts with it: "lat" and the run's start in unix milliseconds

	// Only the round trip's goroutine touches these.
	rule     cex.MarketRule
	sent     int       // orders that may have reached the exchange
	made     int       // client ids made
	lastSent time.Time // when the last of them was sent

	mu     sync.Mutex
	best   roundTripQuote
	finals map[string]roundTripFinal // by client id: the first push of the order's final state
	out    roundTripRecord           // what is saved
}

// roundTripQuote is the best bid and ask of the contract, from books1.
type roundTripQuote struct {
	bid, ask cex.BookLevel
	at       time.Time // local arrival
}

func (q roundTripQuote) age() time.Duration {
	if q.at.IsZero() {
		return time.Duration(1<<63 - 1)
	}
	return time.Since(q.at)
}

// roundTripFinal is a push of an order's final state.
type roundTripFinal struct {
	at  time.Time
	row orderRow
}

// roundTripRecord is the file the round trip saves.
type roundTripRecord struct {
	Symbol            string            `json:"symbol"`
	ClientIDPrefix    string            `json:"client_id_prefix"`
	Rule              map[string]string `json:"rule"`
	OneWay            bool              `json:"one_way_mode"`
	Orders            []*roundTripOrder `json:"orders"`
	Positions         []positionRead    `json:"positions_by_rest"`
	CloseThatWorked   string            `json:"close_that_left_the_position_flat"`
	ReduceOnlyVerdict string            `json:"reduce_only_verdict"`
	TradeFrames       []receivedFrame   `json:"trade_connection_frames"` // every frame the trade connection received, but pongs
	Pushes            []pushRow         `json:"private_pushes"`          // the rows of this run's orders and fills and of this contract's position
	REST              []restExchange    `json:"rest_exchanges"`          // every REST request but the keep-warm one
}

// roundTripOrder is one order of the round trip and what became of it.
type roundTripOrder struct {
	ID                 string  `json:"client_order_id"`
	Role               string  `json:"role"`    // open or close
	Variant            string  `json:"variant"` // ws, ws-YES, rest-yes or rest-plain
	Transport          string  `json:"transport"`
	ReduceOnlySent     string  `json:"reduce_only_sent"` // the flag's text in the frame or the body
	Side               string  `json:"side"`
	Price              string  `json:"price"`
	Qty                string  `json:"qty"`
	BestBid            string  `json:"best_bid_at_sending"`
	BestAsk            string  `json:"best_ask_at_sending"`
	BestAgeMs          float64 `json:"best_age_ms"`
	Sent               string  `json:"sent,omitempty"` // the frame or the body as sent
	NotSent            string  `json:"not_sent,omitempty"`
	SentNs             int64   `json:"sent_unix_ns,omitempty"`
	AckNs              int64   `json:"ack_unix_ns,omitempty"`
	SentToAckMs        float64 `json:"sent_to_ack_ms,omitempty"`
	ErrorCode          string  `json:"error_code,omitempty"`
	ErrorMessage       string  `json:"error_message,omitempty"`
	Error              string  `json:"error,omitempty"`
	OrderID            string  `json:"order_id,omitempty"`
	AcceptedUs         int64   `json:"exchange_accepted_unix_us,omitempty"`
	CreatedMs          int64   `json:"exchange_created_ms,omitempty"`
	FinalNs            int64   `json:"final_push_unix_ns,omitempty"`
	SentToFinalMs      float64 `json:"sent_to_final_push_ms,omitempty"`
	FinalBy            string  `json:"final_by,omitempty"` // push, query, refused or unknown
	Status             string  `json:"status,omitempty"`
	CumExecQty         string  `json:"cum_exec_qty,omitempty"`
	CumExecValue       string  `json:"cum_exec_value,omitempty"`
	AvgPrice           string  `json:"avg_price,omitempty"`
	Fee                string  `json:"fee,omitempty"`
	ReduceOnlyReported string  `json:"reduce_only_reported,omitempty"` // as the final push or the query reports it
	OrderInfo          string  `json:"order_info_afterwards,omitempty"`

	filled         decimal.Decimal
	infoReduceOnly *bool // the order-info's reduce-only flag, once read
}

// positionRead is one read of the contract's position by REST.
type positionRead struct {
	AtNs  int64  `json:"at_unix_ns"`
	When  string `json:"when"`
	Qty   string `json:"qty,omitempty"` // signed, 0 when flat
	Error string `json:"error,omitempty"`
}

// receivedFrame is a frame as received, with its local arrival.
type receivedFrame struct {
	ReceivedNs int64  `json:"received_unix_ns"`
	Frame      string `json:"frame"`
}

// pushRow is one row of a private push, as received.
type pushRow struct {
	ReceivedNs int64  `json:"received_unix_ns"`
	Topic      string `json:"topic"`
	Action     string `json:"action"`
	Row        string `json:"row,omitempty"` // none: a position snapshot without this contract, which is flat
}

// restExchange is one REST request and its answer as sent and received:
// never a header, so never the key or a signature.
type restExchange struct {
	SentNs     int64  `json:"sent_unix_ns"`
	ReceivedNs int64  `json:"received_unix_ns"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	Query      string `json:"query,omitempty"`
	Body       string `json:"body,omitempty"`
	Status     int    `json:"status,omitempty"`
	Response   string `json:"response,omitempty"`
	Error      string `json:"error,omitempty"`
}

// recordingTransport hands every request to the next transport and keeps
// its line, body and answer, not its headers. The keep-warm request is
// left out.
type recordingTransport struct {
	next http.RoundTripper
	mu   *sync.Mutex
	into *[]restExchange
}

func (r recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Path == pathServerTime {
		return r.next.RoundTrip(req)
	}
	x := restExchange{SentNs: time.Now().UnixNano(), Method: req.Method, Path: req.URL.Path, Query: req.URL.RawQuery}
	if req.GetBody != nil {
		if body, err := req.GetBody(); err == nil {
			raw, _ := io.ReadAll(body)
			body.Close()
			x.Body = string(raw)
		}
	}
	resp, err := r.next.RoundTrip(req)
	x.ReceivedNs = time.Now().UnixNano()
	if err != nil {
		x.Error = err.Error()
	} else {
		raw, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(raw))
		x.Status, x.Response = resp.StatusCode, string(raw)
		if readErr != nil {
			x.Error = readErr.Error()
		}
	}
	r.mu.Lock()
	*r.into = append(*r.into, x)
	r.mu.Unlock()
	return resp, err
}

// runWSRoundTrip is the whole round trip, start to end, and saves what it
// recorded: the live test runs it against the exchange, the rehearsal
// against a local one.
func runWSRoundTrip(t *testing.T, log roundTripLog, client *Client, symbol cex.Symbol, leg cex.Leg, timing roundTripTimings) *wsRoundTrip {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt := newWSRoundTrip(ctx, log, client, symbol, leg, timing)
	defer rt.save(t)
	if err := rt.start(); err != nil {
		log.Errorf("%v: no order was sent", err)
		return rt
	}
	if err := rt.ready(); err != nil {
		log.Errorf("%v", err)
		return rt
	}
	rt.roundTrip()
	rt.finish()
	return rt
}

// newWSRoundTrip sets the client up for the round trip: the ws order
// transport, and its REST requests, its trade connection's frames and its
// private pushes recorded.
func newWSRoundTrip(ctx context.Context, log roundTripLog, client *Client, symbol cex.Symbol, leg cex.Leg, timing roundTripTimings) *wsRoundTrip {
	rt := &wsRoundTrip{ctx: ctx, log: log, client: client, symbol: symbol, leg: leg, timing: timing,
		prefix: fmt.Sprintf("lat%d-", time.Now().UnixMilli()), finals: map[string]roundTripFinal{}}
	rt.out.Symbol, rt.out.ClientIDPrefix = string(symbol), rt.prefix
	client.http.Transport = recordingTransport{next: client.http.Transport, mu: &rt.mu, into: &rt.out.REST}
	client.onRawTradeFrame, client.onRawPrivateFrame = rt.tradeFrame, rt.privateFrame
	client.UseOrderTransport(cex.OrderTransportWS)
	return rt
}

// start runs the client - the REST connection kept warm and the trade
// connection - and opens the contract's books1 and the private stream.
func (rt *wsRoundTrip) start() error {
	go rt.client.Run(rt.ctx)
	if err := rt.client.SubscribeBooks(rt.ctx, []cex.BookSubscription{{Symbol: rt.symbol, Topic: cex.TopicBooks1}}, rt.book, func(error) {}); err != nil {
		return err
	}
	return rt.client.SubscribePrivate(rt.ctx, []cex.Symbol{rt.symbol}, func(cex.PrivateEvent) {}, func(error) {})
}

// ready checks everything that must hold before the first order. An error
// means that nothing was sent.
func (rt *wsRoundTrip) ready() error {
	rules, err := rt.client.MarketRules(rt.ctx, []cex.Symbol{rt.symbol})
	if err != nil {
		return fmt.Errorf("the contract's rules: %w: no order was sent", err)
	}
	rt.rule = rules[rt.symbol]
	rt.out.Rule = map[string]string{"price_tick": rt.rule.PriceTick.String(), "qty_step": rt.rule.QtyStep.String(), "min_qty": rt.rule.MinQty.String(), "min_notional": rt.rule.MinNotional.String()}
	if !rt.rule.Online {
		return fmt.Errorf("%s is not trading: no order was sent", rt.symbol)
	}
	settings, err := rt.client.AccountSettings(rt.ctx, []cex.Symbol{rt.symbol})
	if err != nil {
		return fmt.Errorf("the account settings: %w: no order was sent", err)
	}
	if rt.out.OneWay = settings.OneWay; !settings.OneWay {
		return errors.New("the account is not in one-way mode, which the orders assume: no order was sent")
	}
	for deadline := time.Now().Add(rt.timing.upWait); ; time.Sleep(20 * time.Millisecond) {
		_, fresh := rt.quote()
		privateUp, tradeUp := rt.client.PrivateUp(), rt.client.trade.isUp()
		if fresh && privateUp && tradeUp {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("after %s: a best bid and ask younger than %s %t, private stream up %t, trade connection up %t: no order was sent", rt.timing.upWait, roundTripQuoteAge, fresh, privateUp, tradeUp)
		}
	}
	held, err := rt.position("before")
	if err != nil {
		return fmt.Errorf("the position before: %w: no order was sent", err)
	}
	if !held.IsZero() {
		return fmt.Errorf("%s holds %s: the round trip starts only from flat; no order was sent", rt.symbol, held)
	}
	return nil
}

// roundTrip opens the position and closes it.
func (rt *wsRoundTrip) roundTrip() {
	rule, one := rt.rule, decimal.NewFromInt(1)
	q, fresh := rt.quote()
	if !fresh {
		rt.log.Errorf("the best bid and ask are %s old: no order was sent", q.age().Round(time.Millisecond))
		return
	}
	price := q.ask.Price.Mul(one.Add(roundTripThrough)).Div(rule.PriceTick).Ceil().Mul(rule.PriceTick)
	qty := decimal.Max(rule.MinNotional.Mul(decimal.RequireFromString("1.1")).Div(price).Div(rule.QtyStep).Ceil().Mul(rule.QtyStep), rule.MinQty)
	if limit := rule.MinNotional.Mul(decimal.NewFromInt(3)); !price.IsPositive() || price.Mul(qty).GreaterThan(limit) {
		rt.log.Errorf("the open would be %s at %s, %s USDT, above three times the minimum notional (%s): no order was sent", qty, price, price.Mul(qty), limit)
		return
	}
	open := rt.send(cex.OrderRequest{ClientOrderID: rt.id("OW"), Symbol: rt.symbol, Leg: rt.leg, Side: cex.SideBuy, Price: price, Qty: qty}, q, "open", "ws", true)
	switch {
	case open.NotSent != "":
		rt.log.Errorf("the open was not sent: %s", open.NotSent)
		return
	case open.FinalBy == finalRefused:
		rt.log.Errorf("the open was refused: code %s, %q", open.ErrorCode, open.ErrorMessage)
		return
	}
	rt.pace()
	held, err := rt.positionAgreeing("after the open", open.filled)
	filled := open.filled
	if open.FinalBy == finalUnknown && err == nil && held.IsPositive() {
		filled = held // the open's outcome is known from the position only
	}
	if !filled.IsPositive() {
		rt.log.Logf("the open filled nothing (%s): there is nothing to close", open.Status)
		return
	}
	if err == nil && !held.Equal(filled) {
		rt.log.Errorf("the open filled %s but the position by REST is %s", filled, held)
	}

	// The closes. The first sells exactly the filled quantity; each later
	// one what is left, by the position or, while it cannot be read, by the
	// orders. A reduce-only order cannot sell more than is held; a plain
	// sell could, and is sent only for what both say is left.
	closed, left, heldKnown := decimal.Zero, filled, false
	for _, c := range roundTripCloses {
		if !left.IsPositive() {
			break
		}
		name, qty := closeName(c.ws, c.reduceOnly), left
		if !c.reduceOnly {
			byOrders := filled.Sub(closed)
			if !heldKnown || !byOrders.IsPositive() {
				rt.log.Errorf("the close %s was not sent: the position by REST is known %t, left by the orders %s", name, heldKnown, byOrders)
				break
			}
			qty = decimal.Min(left, byOrders)
		}
		rt.pace()
		q, fresh := rt.waitQuote(rt.timing.quoteWait)
		if !fresh {
			rt.log.Logf("the close %s is priced on a best bid %s old", name, q.age().Round(time.Millisecond))
		}
		price := q.bid.Price.Mul(one.Sub(roundTripThrough)).Div(rule.PriceTick).Floor().Mul(rule.PriceTick)
		if !price.IsPositive() || !qty.IsPositive() || qty.GreaterThan(filled) {
			rt.log.Errorf("the close %s was not sent: price %s, quantity %s, filled %s", name, price, qty, filled)
			break
		}
		req := cex.OrderRequest{ClientOrderID: rt.id(c.tag), Symbol: rt.symbol, Leg: rt.leg, Side: cex.SideSell, Price: price, Qty: qty, ReduceOnly: c.reduceOnly}
		o := rt.send(req, q, "close", name, c.ws)
		if o.NotSent != "" {
			continue
		}
		closed = closed.Add(o.filled)
		rt.pace()
		held, err := rt.positionAgreeing("after "+o.ID, filled.Sub(closed))
		switch {
		case err != nil:
			heldKnown, left = false, filled.Sub(closed)
			rt.log.Logf("after the close %s the position cannot be read (%v); %s left by the orders", name, err, left)
			continue
		case held.IsZero():
			rt.out.CloseThatWorked = name
			rt.log.Logf("the close %s left the position flat", name)
			return
		case held.IsNegative():
			rt.log.Errorf("%s is short %s after the close %s: nothing more is sent", rt.symbol, held.Neg(), name)
			return
		}
		heldKnown, left = true, decimal.Min(held, filled)
		rt.log.Logf("after the close %s the position is %s: %s left to close", name, held, left)
	}
	if !heldKnown && !left.IsPositive() {
		rt.log.Logf("the orders say the position is closed and it could not be read: the read at the end decides")
		return
	}
	rt.log.Errorf("no close left the position flat")
}

// finish reads the position and every order after the round trip, logs
// what each order did and judges the trade connection's reduce-only flag.
func (rt *wsRoundTrip) finish() {
	time.Sleep(rt.timing.lastPushes)
	held, err := rt.position("at the end")
	for i := 0; i < 2 && err != nil; i++ {
		time.Sleep(rt.timing.reread)
		held, err = rt.position("at the end, again")
	}
	for _, o := range rt.out.Orders {
		if o.SentNs == 0 {
			continue
		}
		rec, err := rt.client.OrderByClientID(rt.ctx, rt.symbol, cex.ClientOrderID(o.ID))
		if err != nil {
			o.OrderInfo = "query: " + err.Error()
			continue
		}
		reduceOnly := rec.Request.ReduceOnly
		o.infoReduceOnly = &reduceOnly
		o.OrderInfo = fmt.Sprintf("%s, executed %s for %s at %s, fee %s, reduce-only %t", rec.Status, rec.CumExecQty, rec.CumExecValue, rec.AvgPrice, rec.Fee, reduceOnly)
	}
	for _, o := range rt.out.Orders {
		o.report(rt.log)
	}
	rt.verdict()
	switch {
	case err != nil:
		rt.log.Errorf("THE POSITION OF %s COULD NOT BE READ AT THE END (%v): check it by hand", rt.symbol, err)
	case !held.IsZero():
		rt.log.Errorf("THE POSITION OF %s IS NOT FLAT AT THE END: %s left; close it by hand", rt.symbol, held)
	default:
		rt.log.Logf("the position of %s is flat at the end; the close that left it flat: %s", rt.symbol, rt.out.CloseThatWorked)
	}
}

// verdict judges the trade connection's spelling of the reduce-only flag
// by the first close, which used it.
func (rt *wsRoundTrip) verdict() {
	name := closeName(true, true)
	var first *roundTripOrder
	for _, o := range rt.out.Orders {
		if o.Variant == name && o.SentNs != 0 {
			first = o
			break
		}
	}
	var text string
	var failed bool
	switch {
	case first == nil:
		text, failed = fmt.Sprintf("no reduce-only close was sent on the trade connection: the flag %q is not verified", wsReduceOnlyYes), true
	case first.FinalBy == finalRefused:
		text, failed = fmt.Sprintf("the trade connection refused the reduce-only flag %q: code %s, %q", wsReduceOnlyYes, first.ErrorCode, first.ErrorMessage), true
	case first.FinalBy == finalUnknown:
		text, failed = fmt.Sprintf("the outcome of the close with the reduce-only flag %q is not known (%s; %s): the flag is not verified", wsReduceOnlyYes, first.Error, first.Status), true
	case !first.exchangeReduceOnly():
		text, failed = fmt.Sprintf("the trade connection took the close with the reduce-only flag %q, and the exchange reports it as not reduce-only (%q; order-info: %s)", wsReduceOnlyYes, first.ReduceOnlyReported, first.OrderInfo), true
	case !first.filled.IsPositive():
		text = fmt.Sprintf("the trade connection took the close with the reduce-only flag %q as reduce-only; it did not fill", wsReduceOnlyYes)
	default:
		text = fmt.Sprintf("the trade connection's reduce-only flag %q works: the close was taken as reduce-only and filled %s of %s", wsReduceOnlyYes, first.filled, first.Qty)
	}
	rt.out.ReduceOnlyVerdict = text
	if failed {
		rt.log.Errorf("%s", text)
	} else {
		rt.log.Logf("%s", text)
	}
}

// exchangeReduceOnly reports whether the exchange took the order as
// reduce-only: by the order-info afterwards, else by its final push.
func (o *roundTripOrder) exchangeReduceOnly() bool {
	if o.infoReduceOnly != nil {
		return *o.infoReduceOnly
	}
	return strings.EqualFold(o.ReduceOnlyReported, "yes")
}

// report logs what one order did.
func (o *roundTripOrder) report(log roundTripLog) {
	if o.SentNs == 0 {
		log.Logf("%s %s (%s): not sent: %s", o.ID, o.Role, o.Variant, o.NotSent)
		return
	}
	head := fmt.Sprintf("%s %s (%s by %s, reduce-only sent as %q): %s %s @ %s", o.ID, o.Role, o.Variant, o.Transport, o.ReduceOnlySent, o.Side, o.Qty, o.Price)
	switch {
	case o.FinalBy == finalRefused:
		log.Logf("%s: refused after %.1f ms, code %s, %q", head, o.SentToAckMs, o.ErrorCode, o.ErrorMessage)
		return
	case o.Error != "":
		log.Logf("%s: no acceptance (%s); final %s by %s", head, o.Error, o.Status, o.FinalBy)
	default:
		log.Logf("%s: accepted; final %s by %s", head, o.Status, o.FinalBy)
	}
	created, final := "-", "-"
	if o.CreatedMs > 0 {
		created = fmt.Sprintf("%.1f ms", float64(o.CreatedMs*int64(time.Millisecond)-o.SentNs)/1e6)
	}
	if o.FinalNs > 0 {
		final = fmt.Sprintf("%.1f ms", o.SentToFinalMs)
	}
	log.Logf("%s: sent -> acceptance in hand %.1f ms; sent -> created %s (exchange clock, with the clocks' offset); sent -> final state by push %s", o.ID, o.SentToAckMs, created, final)
	if o.filled.IsPositive() {
		avg, _ := decimal.NewFromString(o.AvgPrice)
		value, _ := decimal.NewFromString(o.CumExecValue)
		fee, _ := decimal.NewFromString(o.Fee)
		best, _ := decimal.NewFromString(o.BestAsk)
		worse := avg.Sub(best) // a buy pays more than the ask
		if o.Side == string(cex.SideSell) {
			best, _ = decimal.NewFromString(o.BestBid)
			worse = best.Sub(avg) // a sell gets less than the bid
		}
		bps := 0.0
		if best.IsPositive() {
			bps = worse.Div(best).Mul(decimal.NewFromInt(10000)).InexactFloat64()
		}
		expected := value.Mul(roundTripFeeBps).Div(decimal.NewFromInt(10000))
		log.Logf("%s: filled %s at %s against the best %s at sending (%.0f ms old): %.2f bps worse; fee %s against %s = 0.65 bps of %s", o.ID, o.CumExecQty, o.AvgPrice, best,
			o.BestAgeMs, bps, fee, expected.Round(8), value)
	}
	log.Logf("%s: reduce-only as the exchange reports it: %q by %s; order-info afterwards: %s", o.ID, o.ReduceOnlyReported, o.FinalBy, o.OrderInfo)
}

// send sends one order, by the trade connection or by REST, and follows it
// to its final state: by push for up to finalWait, then by query. It never
// stops the test - a position may be open - and says what went wrong.
func (rt *wsRoundTrip) send(req cex.OrderRequest, q roundTripQuote, role, variant string, ws bool) *roundTripOrder {
	o := &roundTripOrder{ID: string(req.ClientOrderID), Role: role, Variant: variant, Transport: "rest", ReduceOnlySent: reduceOnlyText(req, "yes", "no"),
		Side: string(req.Side), Price: req.Price.String(), Qty: req.Qty.String(), BestBid: q.bid.Price.String(), BestAsk: q.ask.Price.String(), BestAgeMs: msOf(q.age())}
	if ws {
		o.Transport, o.ReduceOnlySent = "ws", reduceOnlyText(req, wsReduceOnlyYes, wsReduceOnlyNo)
	}
	rt.out.Orders = append(rt.out.Orders, o)
	if rt.sent >= roundTripMaxOrders {
		o.NotSent = fmt.Sprintf("the run's limit of %d orders was reached", roundTripMaxOrders)
		rt.log.Errorf("%s was not sent: %s", o.ID, o.NotSent)
		return o
	}
	rt.pace()
	if ws {
		for deadline := time.Now().Add(rt.timing.upWait); !rt.client.trade.isUp() && time.Now().Before(deadline); {
			time.Sleep(20 * time.Millisecond)
		}
		frame, err := orderFrame(req)
		if err != nil {
			o.NotSent = err.Error()
			return o
		}
		o.Sent = string(frame)
	}

	orderCtx, done := context.WithTimeout(rt.ctx, 5*time.Second)
	defer done()
	var ack cex.OrderAck
	var err error
	sent := time.Now()
	if ws {
		var written bool
		if ack, err, written = rt.client.trade.place(orderCtx, req); !written {
			o.NotSent = "the trade connection was not up"
			rt.log.Logf("%s was not sent: %s", o.ID, o.NotSent)
			return o
		}
	} else if ack, err = rt.client.placeOrderREST(orderCtx, req); errors.Is(err, cex.ErrNotSent) {
		o.NotSent = err.Error()
		rt.log.Logf("%s was not sent: %s", o.ID, o.NotSent)
		return o
	}
	acked := time.Now()
	rt.sent, rt.lastSent = rt.sent+1, sent
	o.SentNs, o.AckNs, o.SentToAckMs = sent.UnixNano(), acked.UnixNano(), msOf(acked.Sub(sent))
	if !ws {
		o.Sent = rt.restBody(o.ID)
	}
	var refusal *cex.VenueError
	switch {
	case err == nil:
		o.OrderID = string(ack.OrderID)
		if !ack.AcceptedAt.IsZero() {
			o.AcceptedUs = ack.AcceptedAt.UnixMicro()
		}
		if !ack.CreatedAt.IsZero() {
			o.CreatedMs = ack.CreatedAt.UnixMilli()
		}
	case !errors.Is(err, cex.ErrOrderStateUnknown) && errors.As(err, &refusal):
		o.ErrorCode, o.ErrorMessage, o.Status, o.FinalBy = refusal.Code, refusal.Message, "refused", finalRefused
		rt.log.Logf("%s (%s) was refused: code %s, %q", o.ID, variant, refusal.Code, refusal.Message)
		return o
	default:
		o.Error = err.Error()
		rt.log.Errorf("%s (%s): %v; its outcome is taken from the push or the query", o.ID, variant, err)
	}
	rt.awaitFinal(o, sent)
	return o
}

// awaitFinal waits for the order's final state by push, then asks for it.
func (rt *wsRoundTrip) awaitFinal(o *roundTripOrder, sent time.Time) {
	for deadline := sent.Add(rt.timing.finalWait); ; time.Sleep(5 * time.Millisecond) {
		rt.mu.Lock()
		p, pushed := rt.finals[o.ID]
		rt.mu.Unlock()
		if pushed {
			o.FinalBy, o.FinalNs, o.SentToFinalMs = finalPush, p.at.UnixNano(), msOf(p.at.Sub(sent))
			o.Status, o.CumExecQty, o.CumExecValue, o.AvgPrice, o.ReduceOnlyReported = p.row.OrderStatus, p.row.CumExecQty, p.row.CumExecValue, p.row.AvgPrice, p.row.ReduceOnly
			if fee, err := feeOf(p.row.FeeDetail); err == nil {
				o.Fee = fee.String()
			}
			if o.OrderID == "" {
				o.OrderID = p.row.OrderID
			}
			if created := wireTime(p.row.CreatedTime); o.CreatedMs == 0 && !created.IsZero() {
				o.CreatedMs = created.UnixMilli()
			}
			o.filled, _ = decimal.NewFromString(p.row.CumExecQty)
			return
		}
		if time.Now().After(deadline) {
			break
		}
	}
	rec, err := rt.client.OrderByClientID(rt.ctx, rt.symbol, cex.ClientOrderID(o.ID))
	if err != nil {
		o.FinalBy, o.Status = finalUnknown, "no final push in time; query: "+err.Error()
		rt.log.Errorf("%s: no final state by push within %s, and the query failed: %v", o.ID, rt.timing.finalWait, err)
		return
	}
	o.FinalBy, o.Status, o.CumExecQty, o.CumExecValue, o.AvgPrice, o.Fee = finalQuery, string(rec.Status), rec.CumExecQty.String(), rec.CumExecValue.String(), rec.AvgPrice.String(), rec.Fee.String()
	o.ReduceOnlyReported, o.filled = reduceOnlyText(rec.Request, "yes", "no"), rec.CumExecQty
	if o.CreatedMs == 0 && !rec.ExchangeCreatedAt.IsZero() {
		o.CreatedMs = rec.ExchangeCreatedAt.UnixMilli()
	}
	rt.log.Logf("%s: no final state by push within %s; by query %s, executed %s", o.ID, rt.timing.finalWait, rec.Status, rec.CumExecQty)
}

// pace waits until the next order may be sent: gap after the last one.
func (rt *wsRoundTrip) pace() {
	if wait := time.Until(rt.lastSent.Add(rt.timing.gap)); wait > 0 {
		time.Sleep(wait)
	}
}

// id makes the next client id of the run.
func (rt *wsRoundTrip) id(tag string) cex.ClientOrderID {
	rt.made++
	return cex.ClientOrderID(fmt.Sprintf("%s%d%s", rt.prefix, rt.made, tag))
}

// position reads the contract's position by REST: signed, zero when flat.
func (rt *wsRoundTrip) position(when string) (decimal.Decimal, error) {
	set, err := rt.client.Positions(rt.ctx, []cex.Symbol{rt.symbol})
	read := positionRead{AtNs: time.Now().UnixNano(), When: when}
	held := set.BySymbol[rt.symbol].Qty
	if err != nil {
		read.Error = err.Error()
	} else {
		read.Qty = held.String()
	}
	rt.out.Positions = append(rt.out.Positions, read)
	return held, err
}

// positionAgreeing reads the position, and reads it again, twice at most,
// while it differs from what the orders say: REST may lag a fill.
func (rt *wsRoundTrip) positionAgreeing(when string, byOrders decimal.Decimal) (decimal.Decimal, error) {
	held, err := rt.position(when)
	for i := 0; i < 2 && (err != nil || !held.Equal(byOrders)); i++ {
		time.Sleep(rt.timing.reread)
		held, err = rt.position(when + ", again")
	}
	return held, err
}

// restBody is the body of the REST order with the client id, as recorded.
func (rt *wsRoundTrip) restBody(id string) string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for i := len(rt.out.REST) - 1; i >= 0; i-- {
		if x := rt.out.REST[i]; x.Method == http.MethodPost && strings.Contains(x.Body, `"clientOid":"`+id+`"`) {
			return x.Body
		}
	}
	return ""
}

// book keeps the contract's best bid and ask.
func (rt *wsRoundTrip) book(ev cex.BookEvent) {
	if len(ev.Bids) == 0 || len(ev.Asks) == 0 {
		return
	}
	rt.mu.Lock()
	rt.best = roundTripQuote{bid: ev.Bids[0], ask: ev.Asks[0], at: ev.ReceivedAt}
	rt.mu.Unlock()
}

// quote returns the last best bid and ask and whether an order may be
// priced on them: young enough and not crossed.
func (rt *wsRoundTrip) quote() (roundTripQuote, bool) {
	rt.mu.Lock()
	q := rt.best
	rt.mu.Unlock()
	return q, q.age() < roundTripQuoteAge && q.bid.Price.IsPositive() && q.ask.Price.GreaterThan(q.bid.Price)
}

// waitQuote waits up to within for a best bid and ask an order may be
// priced on, and returns the last ones either way.
func (rt *wsRoundTrip) waitQuote(within time.Duration) (roundTripQuote, bool) {
	for deadline := time.Now().Add(within); ; time.Sleep(10 * time.Millisecond) {
		if q, fresh := rt.quote(); fresh || time.Now().After(deadline) {
			return q, fresh
		}
	}
}

// tradeFrame records a frame of the trade connection. Nothing but this
// run's orders, their answers and the login's answer travel on it.
func (rt *wsRoundTrip) tradeFrame(raw []byte, at time.Time) {
	rt.mu.Lock()
	rt.out.TradeFrames = append(rt.out.TradeFrames, receivedFrame{ReceivedNs: at.UnixNano(), Frame: string(raw)})
	rt.mu.Unlock()
}

// privateFrame records the rows of a private push that belong to the run:
// the orders and fills of its client ids, and the contract's position, and
// keeps the first push of each order's final state.
func (rt *wsRoundTrip) privateFrame(raw []byte, at time.Time) {
	var f wsFrame
	if json.Unmarshal(raw, &f) != nil || f.Action == "" {
		return // a control frame: the login's and the subscriptions' answers
	}
	var rows []json.RawMessage
	if json.Unmarshal(f.Data, &rows) != nil {
		return
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	kept := 0
	for _, row := range rows {
		var id struct {
			Symbol    string `json:"symbol"`
			ClientOid string `json:"clientOid"`
		}
		if json.Unmarshal(row, &id) != nil || id.Symbol != string(rt.symbol) {
			continue
		}
		switch f.Arg.Topic {
		case topicOrder, topicFill:
			if !strings.HasPrefix(id.ClientOid, rt.prefix) {
				continue
			}
		case topicPosition:
		default:
			continue
		}
		kept++
		rt.out.Pushes = append(rt.out.Pushes, pushRow{ReceivedNs: at.UnixNano(), Topic: f.Arg.Topic, Action: f.Action, Row: string(row)})
		var o orderRow
		if _, seen := rt.finals[id.ClientOid]; f.Arg.Topic != topicOrder || seen || json.Unmarshal(row, &o) != nil {
			continue
		}
		if o.OrderStatus == "filled" || o.OrderStatus == "cancelled" || o.OrderStatus == "canceled" {
			rt.finals[id.ClientOid] = roundTripFinal{at: at, row: o}
		}
	}
	if kept == 0 && f.Arg.Topic == topicPosition && f.Action == "snapshot" {
		rt.out.Pushes = append(rt.out.Pushes, pushRow{ReceivedNs: at.UnixNano(), Topic: f.Arg.Topic, Action: f.Action})
	}
}

// save writes what the run recorded to the evidence directory.
func (rt *wsRoundTrip) save(t *testing.T) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	saveResult(t, "ws_round_trip_"+strings.ToLower(string(rt.symbol)), &rt.out)
}
