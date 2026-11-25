package bitget

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/cex/internal/livetest"
)

// The book of the local exchange: it does not move.
const (
	simBid = "42.8"
	simAsk = "42.81"
)

// simExchange is a local stand-in for the exchange in the rehearsal of the
// round trip. One address serves the REST API, the public stream (books1
// of SOXLUSDT) and the private connections (the stream and the trade
// connection), which it tells apart by what they send. It holds a one-way
// position in SOXLUSDT; an IOC limit order fills in full against the book
// or not at all, and each order is pushed on the private stream - before
// its acceptance is answered - with its fill and the position. The knobs
// say how it treats the reduce-only flag. Codes and messages are made up.
type simExchange struct {
	*httptest.Server

	// The knobs, set before the round trip.
	refuseWS       map[string]bool // reduce-only texts of an order frame that are refused
	refuseWSNoID   map[string]bool // texts refused by an error frame that names no request; the order is not taken
	ignoreWS       map[string]bool // texts that are accepted and not honoured: the order is not reduce-only
	refuseREST     bool            // REST refuses a reduce-only order
	noOpenFill     bool            // a buy finds no ask
	halfFirstClose bool            // the first sell fills half its quantity

	mu          sync.Mutex
	position    decimal.Decimal
	orders      map[string]string // by client id: the order's row, as order-info returns it
	sells       int
	private     func(string) // the private stream's writer, once it subscribed
	tradeFrames int          // order frames received
	restOrders  int          // place-order requests received
}

func newSimExchange(t *testing.T) *simExchange {
	x := &simExchange{refuseWS: map[string]bool{}, refuseWSNoID: map[string]bool{}, ignoreWS: map[string]bool{}, orders: map[string]string{}}
	x.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if websocket.IsWebSocketUpgrade(r) {
			x.serveWS(w, r)
			return
		}
		x.serveREST(w, r)
	}))
	t.Cleanup(x.Close)
	return x
}

// client is a client of the local exchange with the clocks of the unit
// tests and a key.
func (x *simExchange) client() *Client {
	c := testClient(x.URL)
	c.creds, c.wsPrivateURL = fakeCreds, c.wsPublicURL
	choiceTimings(c)
	return c
}

// order takes one order: it refuses it with a code and a message, or fills
// what the book and the flag allow and pushes the order, its fill and the
// position.
func (x *simExchange) order(id, side, price, qty, reduceOnly string, ws bool) (code, msg string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	reduce := strings.EqualFold(reduceOnly, "yes")
	switch {
	case ws && x.refuseWS[reduceOnly], !ws && reduce && x.refuseREST:
		return "40017", "Parameter reduceOnly error"
	case ws && x.ignoreWS[reduceOnly]:
		reduce = false
	}
	limit, want := decimal.RequireFromString(price), decimal.RequireFromString(qty)
	fill, at := decimal.Zero, decimal.RequireFromString(simAsk)
	if side == "buy" {
		if !x.noOpenFill && limit.GreaterThanOrEqual(at) {
			fill = want
		}
		x.position = x.position.Add(fill)
	} else {
		at = decimal.RequireFromString(simBid)
		if limit.LessThanOrEqual(at) {
			fill = want
		}
		if x.sells++; x.sells == 1 && x.halfFirstClose {
			fill = fill.Div(decimal.NewFromInt(2)).RoundFloor(2)
		}
		if reduce {
			fill = decimal.Min(fill, decimal.Max(x.position, decimal.Zero))
		}
		x.position = x.position.Sub(fill)
	}
	status, flag, avg := "cancelled", "no", "0"
	if fill.Equal(want) {
		status = "filled"
	}
	if reduce {
		flag = "yes"
	}
	if fill.IsPositive() {
		avg = at.String()
	}
	value := fill.Mul(at)
	fee := value.Mul(decimal.RequireFromString("0.000065")).Round(8)
	row := fmt.Sprintf(`{"category":"usdt-futures","symbol":"SOXLUSDT","orderId":"13800000000000%05d","clientOid":%q,"price":%q,"qty":%q,"side":%q,"orderType":"limit","timeInForce":"ioc","reduceOnly":%q,"cumExecQty":%q,"cumExecValue":%q,"avgPrice":%q,"orderStatus":%q,"cancelReason":"","feeDetail":[{"feeCoin":"USDT","fee":%q}],"createdTime":"1795580000012","updatedTime":"1795580000014"}`,
		len(x.orders)+1, id, price, qty, side, flag, fill, value, avg, status, fee)
	x.orders[id] = row
	if x.private != nil {
		x.private(privatePush("update", topicOrder, row))
		if fill.IsPositive() {
			x.private(privatePush("update", topicFill, fmt.Sprintf(`{"symbol":"SOXLUSDT","orderId":"1","clientOid":%q,"execId":"%d","side":%q,"execPrice":%q,"execQty":%q,"execValue":%q,"feeDetail":[{"feeCoin":"USDT","fee":%q}],"execTime":"1795580000013"}`,
				id, len(x.orders), side, at, fill, value, fee)))
		}
		x.private(privatePush("update", topicPosition, x.positionRow()))
	}
	return "", ""
}

// positionRow is the position as the private stream pushes it; flat is
// size 0.
func (x *simExchange) positionRow() string {
	side := "long"
	if x.position.IsNegative() {
		side = "short"
	}
	return positionPush("SOXLUSDT", side, x.position.Abs().String(), simAsk, "normal")
}

func (x *simExchange) serveWS(w http.ResponseWriter, r *http.Request) {
	conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	var writes sync.Mutex // the private stream is written from the trade connection's handler and the REST handlers too
	send := func(msg string) {
		writes.Lock()
		defer writes.Unlock()
		conn.WriteMessage(websocket.TextMessage, []byte(msg))
	}
	done := make(chan struct{})
	defer close(done)
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}
		msg := string(raw)
		switch {
		case msg == "ping":
			send("pong")
		case strings.HasPrefix(msg, `{"op":"login"`):
			send(frameLoginOK)
		case strings.Contains(msg, `"topic":"books1"`):
			go func() {
				for seq := 1; ; seq++ {
					now := time.Now().UnixMilli()
					send(fmt.Sprintf(`{"action":"snapshot","arg":{"instType":"usdt-futures","topic":"books1","symbol":"SOXLUSDT"},"data":[{"a":[[%q,"100"]],"b":[[%q,"100"]],"seq":%d,"pseq":0,"ts":"%d"}],"ts":%d}`, simAsk, simBid, seq, now, now))
					select {
					case <-done:
						return
					case <-time.After(20 * time.Millisecond):
					}
				}
			}()
		case strings.HasPrefix(msg, `{"op":"subscribe"`): // the private stream
			for _, topic := range []string{topicOrder, topicFill, topicPosition} {
				send(privateAck(topic))
			}
			x.mu.Lock()
			x.private = send
			if x.position.IsZero() {
				send(privatePush("snapshot", topicPosition))
			} else {
				send(privatePush("snapshot", topicPosition, x.positionRow()))
			}
			x.mu.Unlock()
		case strings.HasPrefix(msg, `{"op":"trade"`):
			var req tradeRequest
			if json.Unmarshal(raw, &req) != nil || len(req.Args) != 1 {
				send(`{"event":"error","code":"30001","msg":"bad request"}`)
				continue
			}
			x.mu.Lock()
			x.tradeFrames++
			anonymous := x.refuseWSNoID[req.Args[0].ReduceOnly]
			x.mu.Unlock()
			a := req.Args[0]
			if anonymous {
				send(`{"event":"error","code":"40017","msg":"Parameter reduceOnly error"}`)
				continue
			}
			if code, text := x.order(a.ClientOid, a.Side, a.Price, a.Qty, a.ReduceOnly, true); code != "" {
				send(fmt.Sprintf(`{"event":"error","id":%q,"code":%q,"msg":%q}`, req.ID, code, text))
			} else {
				send(tradeAck(req.ID))
			}
		}
	}
}

func (x *simExchange) serveREST(w http.ResponseWriter, r *http.Request) {
	data := func(text string) { fmt.Fprintf(w, `{"code":"00000","msg":"success","requestTime":1,"data":%s}`, text) }
	refuse := func(code, msg string) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, `{"code":%q,"msg":%q,"requestTime":1,"data":null}`, code, msg)
	}
	switch r.URL.Path {
	case pathServerTime:
		data(`{"serverTime":"1"}`)
	case pathInstruments:
		data(`[{"symbol":"SOXLUSDT","priceMultiplier":"0.01","quantityMultiplier":"0.01","minOrderQty":"0.01","minOrderAmount":"5","status":"online"}]`)
	case pathSettings:
		data(`{"holdMode":"one_way_mode","symbolConfigList":[{"category":"USDT-FUTURES","symbol":"SOXLUSDT","marginMode":"crossed","leverage":"10"}]}`)
	case pathPositions:
		x.mu.Lock()
		list := ""
		if !x.position.IsZero() {
			side := "long"
			if x.position.IsNegative() {
				side = "short"
			}
			list = fmt.Sprintf(`{"symbol":"SOXLUSDT","posSide":%q,"total":%q,"avgPrice":%q,"updatedTime":"1795580000014"}`, side, x.position.Abs(), simAsk)
		}
		x.mu.Unlock()
		data(`{"list":[` + list + `]}`)
	case pathOrderInfo:
		x.mu.Lock()
		row, known := x.orders[r.URL.Query().Get("clientOid")]
		x.mu.Unlock()
		if !known {
			refuse(codeOrderUnknown, "Order does not exist")
			return
		}
		data(row)
	case pathPlaceOrder:
		var body placeOrderBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			refuse("40017", "bad body")
			return
		}
		x.mu.Lock()
		x.restOrders++
		x.mu.Unlock()
		if code, text := x.order(body.ClientOid, body.Side, body.Price, body.Qty, body.ReduceOnly, false); code != "" {
			refuse(code, text)
			return
		}
		data(fmt.Sprintf(`{"clientOid":%q,"orderId":"1380000000000000099"}`, body.ClientOid))
	default:
		http.NotFound(w, r)
	}
}

// rehearsalLog passes the round trip's log on and keeps what it reports as
// errors, for the rehearsal to compare with what it expects. Only the round
// trip's goroutine calls it.
type rehearsalLog struct {
	t      *testing.T
	errors []string
}

func (l *rehearsalLog) Logf(format string, args ...any) { l.t.Logf(format, args...) }

func (l *rehearsalLog) Errorf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	l.errors = append(l.errors, msg)
	l.t.Logf("reported as an error: %s", msg)
}

// The round trip of TestLiveWSOrderRoundTrip, the same code, against a
// local exchange, in each way the exchange may treat the trade
// connection's reduce-only flag: the orders it sends and how each is
// priced and spelled, the close that leaves the position flat, what it
// reports as an error, and that it ends flat - it never sells more than it
// bought, so the plain sell does not open a short. A contract that is not
// flat before stops it before anything is sent. Every client id starts
// with "lat", orders are at least the gap apart, and the record it saves
// holds the orders' frames and bodies, the trade connection's answers and
// the pushes, and no credential.
func TestWSRoundTripRehearsal(t *testing.T) {
	timing := roundTripTimings{gap: 20 * time.Millisecond, finalWait: time.Second, upWait: 3 * time.Second, quoteWait: 500 * time.Millisecond,
		reread: 20 * time.Millisecond, lastPushes: 30 * time.Millisecond}
	cases := []struct {
		name   string
		setup  func(*simExchange)
		sent   []string // the variants sent, in order
		flatBy string
		errors []string // what each error reported holds, in order
	}{
		{name: "the trade connection's spelling works", sent: []string{"ws", "ws-YES"}, flatBy: "ws-YES"},
		{name: "YES refused, REST reduce-only taken", setup: func(x *simExchange) { x.refuseWS["YES"] = true },
			sent: []string{"ws", "ws-YES", "rest-yes"}, flatBy: "rest-yes", errors: []string{`refused the reduce-only flag "YES": code 40017, "Parameter reduceOnly error"`}},
		{name: "every reduce-only order refused, the plain sell closes", setup: func(x *simExchange) { x.refuseWS["YES"], x.refuseREST = true, true },
			sent: []string{"ws", "ws-YES", "rest-yes", "rest-plain"}, flatBy: "rest-plain", errors: []string{`refused the reduce-only flag "YES"`}},
		{name: "YES answered by an error that names no request", setup: func(x *simExchange) { x.refuseWSNoID["YES"] = true },
			sent: []string{"ws", "ws-YES", "rest-yes"}, flatBy: "rest-yes", errors: []string{"without naming a request in flight", "the query failed",
				`the outcome of the close with the reduce-only flag "YES" is not known`}},
		{name: "YES taken and not honoured", setup: func(x *simExchange) { x.ignoreWS["YES"] = true },
			sent: []string{"ws", "ws-YES"}, flatBy: "ws-YES", errors: []string{`reports it as not reduce-only`}},
		{name: "the first close fills half", setup: func(x *simExchange) { x.halfFirstClose = true },
			sent: []string{"ws", "ws-YES", "rest-yes"}, flatBy: "rest-yes"},
		{name: "the open fills nothing", setup: func(x *simExchange) { x.noOpenFill = true },
			sent: []string{"ws"}, errors: []string{`the flag "YES" is not verified`}},
		{name: "not flat before", setup: func(x *simExchange) { x.position = decimal.RequireFromString("0.5") },
			errors: []string{"SOXLUSDT holds 0.5: the round trip starts only from flat; no order was sent"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			saved := livetest.Dir
			livetest.Dir = t.TempDir()
			t.Cleanup(func() { livetest.Dir = saved })
			x := newSimExchange(t)
			if tc.setup != nil {
				tc.setup(x)
			}
			log := &rehearsalLog{t: t}
			rt := runWSRoundTrip(t, log, x.client(), "SOXLUSDT", cex.Bull, timing)

			var sent []string
			var last int64
			for _, o := range rt.out.Orders {
				if o.SentNs == 0 {
					continue
				}
				sent = append(sent, o.Variant)
				if !strings.HasPrefix(o.ID, "lat") || !strings.HasPrefix(o.ID, rt.prefix) {
					t.Errorf("client id %s does not start with the run's prefix %s", o.ID, rt.prefix)
				}
				if last != 0 && time.Duration(o.SentNs-last) < timing.gap {
					t.Errorf("%s was sent %s after the order before it, less than the gap %s", o.ID, time.Duration(o.SentNs-last), timing.gap)
				}
				last = o.SentNs
				switch {
				case o.Role == "open" && (o.Price != "42.94" || o.Qty != "0.13" || o.Side != "buy" || o.ReduceOnlySent != "NO" || !strings.Contains(o.Sent, `"reduceOnly":"NO"`)):
					t.Errorf("the open %+v; want a buy of 0.13 at 42.94 (0.3 %% above the ask %s, a little over 5 USDT) with reduceOnly NO", o, simAsk)
				case o.Role == "close" && (o.Price != "42.67" || o.Side != "sell"):
					t.Errorf("the close %s: %s %s @ %s; want a sell at 42.67 (0.3 %% below the bid %s)", o.ID, o.Side, o.Qty, o.Price, simBid)
				case o.Role == "close" && !strings.Contains(o.Sent, fmt.Sprintf(`"reduceOnly":%q`, o.ReduceOnlySent)):
					t.Errorf("the close %s was recorded as sent with reduceOnly %q, but its frame or body is %s", o.ID, o.ReduceOnlySent, o.Sent)
				}
			}
			if !slices.Equal(sent, tc.sent) {
				t.Errorf("orders sent %v, want %v", sent, tc.sent)
			}
			if rt.out.CloseThatWorked != tc.flatBy {
				t.Errorf("the close that left the position flat: %q, want %q", rt.out.CloseThatWorked, tc.flatBy)
			}
			if len(log.errors) != len(tc.errors) {
				t.Errorf("errors reported %q, want %d holding %q", log.errors, len(tc.errors), tc.errors)
			} else {
				for i, want := range tc.errors {
					if !strings.Contains(log.errors[i], want) {
						t.Errorf("error %d: %q, want it to hold %q", i, log.errors[i], want)
					}
				}
			}
			x.mu.Lock()
			position, frames, posts := x.position, x.tradeFrames, x.restOrders
			x.mu.Unlock()
			wantPosition := decimal.Zero
			if tc.name == "not flat before" {
				wantPosition = decimal.RequireFromString("0.5")
			}
			if !position.Equal(wantPosition) || frames+posts != len(tc.sent) {
				t.Errorf("the exchange holds %s and got %d order frames and %d REST orders; want %s and %d orders", position, frames, posts, wantPosition, len(tc.sent))
			}

			// The record saved.
			files, _ := filepath.Glob(filepath.Join(livetest.Dir, "ws_round_trip_soxlusdt_*.json"))
			if len(files) != 1 {
				t.Fatalf("records saved %v, want one", files)
			}
			text, err := os.ReadFile(files[0])
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{fakeCreds.Key, fakeCreds.Secret, fakeCreds.Passphrase, `"sign"`, "ACCESS-"} {
				if strings.Contains(string(text), secret) {
					t.Errorf("the record holds %q", secret)
				}
			}
			var record roundTripRecord
			if err := json.Unmarshal(text, &record); err != nil {
				t.Fatal(err)
			}
			for _, o := range record.Orders {
				if o.SentNs == 0 {
					continue
				}
				if o.Sent == "" {
					t.Errorf("%s: no frame or body recorded", o.ID)
				}
				if o.Transport == "ws" && o.FinalBy != finalUnknown && !slices.ContainsFunc(record.TradeFrames, func(f receivedFrame) bool { return strings.Contains(f.Frame, `"id":"`+o.ID+`"`) }) {
					t.Errorf("%s: the trade connection's answer is not recorded", o.ID)
				}
				if o.FinalBy == finalPush && !slices.ContainsFunc(record.Pushes, func(p pushRow) bool { return p.Topic == topicOrder && strings.Contains(p.Row, o.ID) }) {
					t.Errorf("%s: its order push is not recorded", o.ID)
				}
				if o.OrderInfo == "" {
					t.Errorf("%s: no order-info afterwards", o.ID)
				}
			}
			if len(record.Positions) == 0 || slices.ContainsFunc(record.REST, func(x restExchange) bool { return x.Path == pathServerTime }) {
				t.Errorf("positions read %v; the keep-warm request recorded %t", record.Positions, slices.ContainsFunc(record.REST, func(x restExchange) bool { return x.Path == pathServerTime }))
			}
		})
	}
}
