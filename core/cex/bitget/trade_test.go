package bitget

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/logger"
)

// warnings keeps the Warn and Error lines the package logs, with their
// attributes, and passes every line on. It is installed before any test
// runs, so that no goroutine of an earlier test reads the logger while it
// is replaced.
var warnings = func() *warnCapture {
	c := &warnCapture{next: logger.GlobalLogger.Handler()}
	logger.GlobalLogger = slog.New(c)
	return c
}()

type warnCapture struct {
	next  slog.Handler
	mu    sync.Mutex
	lines []string // "LEVEL message key=value ..."
}

func (c *warnCapture) Enabled(ctx context.Context, l slog.Level) bool { return c.next.Enabled(ctx, l) }
func (c *warnCapture) WithAttrs([]slog.Attr) slog.Handler             { return c }
func (c *warnCapture) WithGroup(string) slog.Handler                  { return c }
func (c *warnCapture) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelWarn {
		line := r.Level.String() + " " + r.Message
		r.Attrs(func(a slog.Attr) bool { line += " " + a.String(); return true })
		c.mu.Lock()
		c.lines = append(c.lines, line)
		c.mu.Unlock()
	}
	return c.next.Handle(ctx, r)
}

// logged reports whether a Warn or Error line holds every one of parts.
func (c *warnCapture) logged(parts ...string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.ContainsFunc(c.lines, func(line string) bool {
		return !slices.ContainsFunc(parts, func(p string) bool { return !strings.Contains(line, p) })
	})
}

// tradeAck is the exchange's acceptance of an order sent over the
// WebSocket, in the shape seen on the wire on 2026-11-25 (receiveTime and
// pushTime beside args, not inside the order as the documentation's example
// has them), with made-up values: the
// gateway took the frame at ...009123 microseconds and the order system
// created the order at ...012 milliseconds.
func tradeAck(id string) string {
	return fmt.Sprintf(`{"event":"trade","id":%q,"category":"usdt-futures","topic":"place-order","args":[{"symbol":"SOXLUSDT","orderId":"1380000000000000021","clientOid":%q,"cTime":"1795580000012"}],"code":"0","msg":"Success","ts":"1795580000013","connId":"0a0b0c0d0e0f0002","rateLimit":[{"limit":"250","remaining":"249"}],"receiveTime":"1795580000009123","pushTime":"1795580000013456"}`, id, id)
}

// tradeRig is a client with the ws order transport against a local server
// that is both the trade connection and the REST API.
type tradeRig struct {
	client *Client
	server *privateServer
	mu     sync.Mutex
	posts  []string // the bodies of the place-order requests REST received
}

// newTradeRig starts the server and the client's trade connection. onOrder
// is called for the n-th order frame of a connection and returns the frames
// to answer with and whether to drop the connection after them; login, when
// set, replaces the answer to the login.
func newTradeRig(t *testing.T, login string, onOrder func(conn, n int, id string) (answers []string, drop bool)) *tradeRig {
	t.Helper()
	r := &tradeRig{}
	if login == "" {
		login = frameLoginOK
	}
	r.client, r.server = newPrivateServer(t, func(conn int, read func() (string, bool), send func(string)) {
		if _, ok := read(); !ok {
			return
		}
		send(login)
		for n := 1; ; n++ {
			frame, ok := read()
			if !ok {
				return
			}
			var req struct {
				ID string `json:"id"`
			}
			json.Unmarshal([]byte(frame), &req)
			answers, drop := onOrder(conn, n, req.ID)
			for _, a := range answers {
				send(a)
			}
			if drop {
				return
			}
		}
	})
	r.server.mu.Lock()
	r.server.rest = func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		if req.URL.Path == pathPlaceOrder {
			r.mu.Lock()
			r.posts = append(r.posts, string(body))
			r.mu.Unlock()
			w.Write([]byte(`{"code":"00000","msg":"success","requestTime":1,"data":{"clientOid":"x","orderId":"1380000000000000099"}}`))
			return
		}
		w.Write([]byte(`{"code":"00000","msg":"success","requestTime":1,"data":{"serverTime":"1"}}`))
	}
	r.server.mu.Unlock()
	choiceTimings(r.client)
	r.client.timing.readTimeout = time.Second
	r.client.UseOrderTransport(cex.OrderTransportWS)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r.client.trade.start(ctx)
	return r
}

// waitUp waits until the trade connection is logged in.
func (r *tradeRig) waitUp(t *testing.T) {
	t.Helper()
	waitFor(t, "the trade connection", r.client.trade.isUp)
}

// orderFrames returns, per connection, the order frames the server got:
// everything after the login.
func (r *tradeRig) orderFrames(t *testing.T) [][]string {
	t.Helper()
	frames, _, _ := r.server.seen()
	out := make([][]string, len(frames))
	for i, sent := range frames {
		if len(sent) == 0 {
			continue // a connection that was pinged and not chosen
		}
		if !strings.HasPrefix(sent[0], `{"op":"login",`) {
			t.Errorf("connection %d began with %s, want the login", i+1, sent[0])
		}
		for _, frame := range sent[1:] {
			if strings.Contains(frame, `"subscribe"`) {
				t.Errorf("the trade connection subscribed: %s", frame)
			}
		}
		out[i] = sent[1:]
	}
	return out
}

func (r *tradeRig) restPosts() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.posts)
}

func orderWithID(seq int, leg string) cex.OrderRequest {
	req := entryOrder
	req.ClientOrderID = cex.ClientOrderID(fmt.Sprintf("etfp1795579990000-%04d%sE", seq, leg))
	return req
}

// An order over the WebSocket is one frame on the logged-in trade
// connection: op trade, topic place-order, the client order id as the
// request id, the category in lower case, and the nine order fields as
// text - no position side, no request time, no receive window. The
// acceptance returns the exchange's order id and its two times. The
// connection logs in and subscribes nothing; REST gets no order.
func TestTradeOrderFrameAndAck(t *testing.T) {
	r := newTradeRig(t, "", func(_, _ int, id string) ([]string, bool) { return []string{tradeAck(id)}, false })
	r.waitUp(t)
	ctx := context.Background()
	ack, err := r.client.PlaceOrder(ctx, entryOrder)
	if err != nil {
		t.Fatal(err)
	}
	if ack.OrderID != "1380000000000000021" || !ack.AcceptedAt.Equal(time.UnixMicro(1795580000009123)) || !ack.CreatedAt.Equal(time.UnixMilli(1795580000012)) || ack.ReceivedAt.IsZero() {
		t.Errorf("ack = %+v, want the order id, the gateway's receive time and the creation time", ack)
	}
	exit := entryOrder
	exit.ClientOrderID, exit.Symbol, exit.Leg, exit.Side, exit.Price, exit.Qty, exit.ReduceOnly = "etfp1795579990000-0009RX", "SOXSUSDT", cex.Bear, cex.SideSell, d("49.406"), d("10.10"), true
	if _, err := r.client.PlaceOrder(ctx, exit); err != nil {
		t.Fatal(err)
	}

	frames := r.orderFrames(t)
	if len(frames) != 1 || len(frames[0]) != 2 {
		t.Fatalf("order frames per connection = %v, want the two orders on the one connection", frames)
	}
	wants := []struct {
		id    string
		order map[string]string
	}{
		{ourOrder, map[string]string{"symbol": "SOXLUSDT", "orderType": "limit", "qty": "11.68", "price": "42.81", "side": "buy", "timeInForce": "ioc", "reduceOnly": "NO", "clientOid": ourOrder, "marginMode": "crossed"}},
		{"etfp1795579990000-0009RX", map[string]string{"symbol": "SOXSUSDT", "orderType": "limit", "qty": "10.1", "price": "49.406", "side": "sell", "timeInForce": "ioc", "reduceOnly": "YES", "clientOid": "etfp1795579990000-0009RX", "marginMode": "crossed"}},
	}
	for i, raw := range frames[0] {
		var frame map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &frame); err != nil {
			t.Fatal(err)
		}
		var top = map[string]string{}
		for _, key := range []string{"op", "id", "category", "topic"} {
			var text string // each must be text
			if err := json.Unmarshal(frame[key], &text); err != nil {
				t.Errorf("order %d: %s is %s, want text", i, key, frame[key])
			}
			top[key] = text
		}
		if top["op"] != "trade" || top["id"] != wants[i].id || top["category"] != "usdt-futures" || top["topic"] != "place-order" || len(frame) != 5 {
			t.Errorf("order %d: frame %s; want op trade, the client order id as id, category usdt-futures, topic place-order and args, nothing else", i, raw)
		}
		var args []map[string]string // every value must be text
		if err := json.Unmarshal(frame["args"], &args); err != nil || len(args) != 1 {
			t.Fatalf("order %d: args %s: %v; want one order", i, frame["args"], err)
		}
		for field, want := range wants[i].order {
			if args[0][field] != want {
				t.Errorf("order %d: %s = %q, want %q", i, field, args[0][field], want)
			}
		}
		if len(args[0]) != len(wants[i].order) {
			t.Errorf("order %d: the order %s has %d fields, want exactly %d", i, frame["args"], len(args[0]), len(wants[i].order))
		}
	}
	if n := r.restPosts(); n != 0 {
		t.Errorf("REST received %d orders while the trade connection was up", n)
	}
	// The status values carry the trade connection.
	attrs := r.client.StatusAttrs()
	if i := slices.Index(attrs, any("trade_up")); i < 0 || attrs[i+1] != true || attrs[i+2] != "trade_ping_ms" || attrs[i+3].(float64) < 0 {
		t.Errorf("the status values %v do not show the trade connection up with its ping", attrs)
	}
}

// The reduce-only flag is spelled YES / NO in an order frame and yes / no
// in a REST body. The frame orderFrame returns is the frame written.
func TestTradeReduceOnlySpelling(t *testing.T) {
	r := newTradeRig(t, "", func(_, _ int, id string) ([]string, bool) { return []string{tradeAck(id)}, false })
	r.waitUp(t)
	ctx := context.Background()
	exit := orderWithID(1, "R")
	exit.Side, exit.ReduceOnly = cex.SideSell, true
	want, err := orderFrame(exit)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.client.PlaceOrder(ctx, exit); err != nil {
		t.Fatal(err)
	}
	if _, err := r.client.PlaceOrder(ctx, orderWithID(2, "B")); err != nil {
		t.Fatal(err)
	}
	frames := r.orderFrames(t)
	if len(frames) != 1 || len(frames[0]) != 2 {
		t.Fatalf("order frames per connection = %v, want two on the one connection", frames)
	}
	for i, wantFlag := range []string{"YES", "NO"} {
		var frame tradeRequest
		if err := json.Unmarshal([]byte(frames[0][i]), &frame); err != nil || len(frame.Args) != 1 {
			t.Fatalf("order %d: %s: %v", i, frames[0][i], err)
		}
		if frame.Args[0].ReduceOnly != wantFlag {
			t.Errorf("order %d: reduceOnly %q, want %q", i, frame.Args[0].ReduceOnly, wantFlag)
		}
	}
	if frames[0][0] != string(want) {
		t.Errorf("orderFrame gave %s, the connection wrote %s", want, frames[0][0])
	}

	exit.ClientOrderID = orderWithID(3, "R").ClientOrderID
	if _, err := r.client.placeOrderREST(ctx, exit); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	posts := slices.Clone(r.posts)
	r.mu.Unlock()
	if len(posts) != 1 || !strings.Contains(posts[0], `"reduceOnly":"yes"`) {
		t.Errorf("REST received %v, want the reduce-only order with reduceOnly yes", posts)
	}
}

// An error frame that carries the request's id is that order's answer, by
// the same table as REST: a refusal with its code and kind, or "may have
// arrived" for the codes the exchange gives when it does not know. The
// exchange writes the code as text or as a number. The connection stays up
// and every order is one frame.
func TestTradeOrderErrors(t *testing.T) {
	cases := []struct {
		answer   string // %q takes the request id
		wantCode string
		wantKind cex.ErrorKind
		unknown  bool
	}{
		{answer: `{"event":"error","id":%q,"code":"25203","msg":"Insufficient margin"}`, wantCode: "25203", wantKind: cex.ErrorOther},
		{answer: `{"event":"error","id":%q,"code":41117,"msg":"The price is too high"}`, wantCode: "41117", wantKind: cex.ErrorPriceLimit},
		{answer: `{"event":"error","id":%q,"code":"25004","msg":"Operation too frequent, please try again later"}`, wantCode: "25004", wantKind: cex.ErrorRateLimited},
		{answer: `{"event":"error","id":%q,"code":30006,"msg":"request too many"}`, wantCode: "30006", wantKind: cex.ErrorRateLimited},
		{answer: `{"event":"trade","id":%q,"code":"25212","msg":"Duplicate clientOid","args":[]}`, wantCode: "25212", wantKind: cex.ErrorOther},
		{answer: `{"event":"error","id":%q,"code":"40010","msg":"Request timed out"}`, unknown: true},
		{answer: `{"event":"error","id":%q,"code":40725,"msg":"service return an error"}`, unknown: true},
		{answer: `{"event":"error","id":%q,"code":"45001","msg":"Unknown error"}`, unknown: true},
		{answer: `{"event":"trade","id":%q,"code":"0","msg":"success","args":[]}`, unknown: true}, // an acceptance without its order
	}
	r := newTradeRig(t, "", func(_, n int, id string) ([]string, bool) { return []string{fmt.Sprintf(cases[n-1].answer, id)}, false })
	r.waitUp(t)
	for i, tc := range cases {
		_, err := r.client.PlaceOrder(context.Background(), orderWithID(i+1, "B"))
		var refusal *cex.VenueError
		switch {
		case err == nil || errors.Is(err, cex.ErrNotSent):
			t.Errorf("case %d: %v, want an error other than not-sent", i, err)
		case tc.unknown:
			if !errors.Is(err, cex.ErrOrderStateUnknown) || errors.As(err, &refusal) {
				t.Errorf("case %d: %v, want cex.ErrOrderStateUnknown and no refusal", i, err)
			}
		default:
			if errors.Is(err, cex.ErrOrderStateUnknown) || !errors.As(err, &refusal) || refusal.Code != tc.wantCode || refusal.Kind != tc.wantKind {
				t.Errorf("case %d: %v, want a refusal with code %s and kind %s", i, err, tc.wantCode, tc.wantKind)
			}
		}
	}
	if frames := r.orderFrames(t); len(frames) != 1 || len(frames[0]) != len(cases) || r.restPosts() != 0 {
		t.Errorf("%v order frames, %d REST orders; want one frame per order on the one connection and nothing by REST", frames, r.restPosts())
	}
}

// No answer within the caller's deadline: the order may have arrived. It
// was written once, is not sent again on any transport, and its late
// acceptance is a warning with the exchange's order id; the connection goes
// on taking orders.
func TestTradeOrderWithoutAnAnswerIsUnknown(t *testing.T) {
	release := make(chan struct{})
	r := newTradeRig(t, "", func(_, n int, id string) ([]string, bool) {
		if n == 1 {
			<-release // the answer comes after the caller gave up
		}
		return []string{tradeAck(id)}, false
	})
	r.waitUp(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	silent := orderWithID(1, "B")
	if _, err := r.client.PlaceOrder(ctx, silent); !errors.Is(err, cex.ErrOrderStateUnknown) || errors.Is(err, cex.ErrNotSent) {
		t.Fatalf("no answer in time: %v, want cex.ErrOrderStateUnknown", err)
	}
	close(release)
	if ack, err := r.client.PlaceOrder(context.Background(), orderWithID(2, "R")); err != nil || ack.OrderID == "" {
		t.Fatalf("the next order after a late answer: %+v, %v", ack, err)
	}
	if !warnings.logged("WARN bitget: trade connection accepted an order nobody waits for", "client_order_id="+string(silent.ClientOrderID), "order_id=1380000000000000021", "code=0") {
		t.Errorf("the late acceptance of %s is not a warning with its order id and code", silent.ClientOrderID)
	}
	frames := r.orderFrames(t)
	sent := 0
	for _, frame := range frames[0] {
		if strings.Contains(frame, string(silent.ClientOrderID)) {
			sent++
		}
	}
	if len(frames) != 1 || sent != 1 || r.restPosts() != 0 {
		t.Errorf("the unanswered order went out %d times on the trade connection (%d connections) and REST got %d orders; want once, one connection, none", sent, len(frames), r.restPosts())
	}
}

// The connection is lost with two orders in flight: both may have arrived.
// The client reconnects and logs in again; nothing of the two is sent
// again, and new orders go out on the new connection.
func TestTradeConnectionLostWithOrdersInFlight(t *testing.T) {
	r := newTradeRig(t, "", func(conn, n int, id string) ([]string, bool) {
		if conn == 1 {
			return nil, n == 2 // the first connection answers nothing and drops after the second order
		}
		return []string{tradeAck(id)}, false
	})
	r.waitUp(t)
	bull, bear := orderWithID(1, "B"), orderWithID(2, "R")
	errs := make(chan error, 2)
	for _, req := range []cex.OrderRequest{bull, bear} {
		go func() {
			_, err := r.client.PlaceOrder(context.Background(), req)
			errs <- err
		}()
	}
	for range 2 {
		select {
		case err := <-errs:
			if !errors.Is(err, cex.ErrOrderStateUnknown) {
				t.Errorf("an order in flight when the connection dropped: %v, want cex.ErrOrderStateUnknown", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("an order in flight was never answered after the connection dropped")
		}
	}
	waitFor(t, "the second connection", func() bool { frames, _, _ := r.server.seen(); return len(frames) == 2 && r.client.trade.isUp() })
	third := orderWithID(3, "B")
	if ack, err := r.client.PlaceOrder(context.Background(), third); err != nil || ack.OrderID == "" {
		t.Fatalf("an order on the new connection: %+v, %v", ack, err)
	}
	frames := r.orderFrames(t)
	if len(frames) != 2 || len(frames[0]) != 2 || len(frames[1]) != 1 || !strings.Contains(frames[1][0], string(third.ClientOrderID)) {
		t.Errorf("order frames per connection = %v; want the two legs as two frames on the first and only the new order on the second", frames)
	}
	if n := r.restPosts(); n != 0 {
		t.Errorf("REST received %d orders: an order in flight must not be sent again", n)
	}
}

// An error frame without a request id cannot be matched to an order: every
// order in flight may be the one it is about, so each ends as "may have
// arrived". The connection is not lost and takes the next order.
func TestTradeErrorFrameThatNamesNoRequest(t *testing.T) {
	r := newTradeRig(t, "", func(_, n int, id string) ([]string, bool) {
		switch n {
		case 1:
			return nil, false
		case 2:
			return []string{`{"event":"error","code":40015,"msg":"System is abnormal, please try again later","ts":1785222459567}`}, false
		}
		return []string{tradeAck(id)}, false
	})
	r.waitUp(t)
	errs := make(chan error, 2)
	first := orderWithID(1, "B")
	go func() { _, err := r.client.PlaceOrder(context.Background(), first); errs <- err }()
	waitFor(t, "the first order's frame", func() bool { frames, _, _ := r.server.seen(); return len(frames[0]) == 2 })
	go func() { _, err := r.client.PlaceOrder(context.Background(), orderWithID(2, "R")); errs <- err }()
	for range 2 {
		select {
		case err := <-errs:
			var refusal *cex.VenueError
			if !errors.Is(err, cex.ErrOrderStateUnknown) || errors.As(err, &refusal) {
				t.Errorf("an order in flight at an unmatched error: %v, want cex.ErrOrderStateUnknown and no refusal", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("an order in flight was never answered after the unmatched error")
		}
	}
	if ack, err := r.client.PlaceOrder(context.Background(), orderWithID(3, "B")); err != nil || ack.OrderID == "" {
		t.Fatalf("the next order: %+v, %v", ack, err)
	}
	if frames := r.orderFrames(t); len(frames) != 1 || len(frames[0]) != 3 || r.restPosts() != 0 {
		t.Errorf("order frames %v, %d REST orders; want three frames on the one connection, which stays up", frames, r.restPosts())
	}
}

// An order gets no answer in time and is given up as unknown. Its late
// refusal arrives while the next order is in flight, just before that
// order's acceptance: it names the order given up, so it is a warning and
// the order in flight is accepted.
func TestTradeLateRefusalOfAnOrderGivenUp(t *testing.T) {
	var first string
	r := newTradeRig(t, "", func(_, n int, id string) ([]string, bool) {
		switch n {
		case 1:
			first = id
			return nil, false // no answer in time
		case 2:
			late := fmt.Sprintf(`{"event":"error","id":%q,"code":"25203","msg":"Insufficient margin"}`, first)
			return []string{late, tradeAck(id)}, false
		}
		return []string{tradeAck(id)}, false
	})
	r.waitUp(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	given := orderWithID(1, "B")
	if _, err := r.client.PlaceOrder(ctx, given); !errors.Is(err, cex.ErrOrderStateUnknown) {
		t.Fatalf("no answer in time: %v, want cex.ErrOrderStateUnknown", err)
	}
	if ack, err := r.client.PlaceOrder(context.Background(), orderWithID(2, "R")); err != nil || ack.OrderID == "" {
		t.Fatalf("the order in flight at the late refusal of another: %+v, %v; want its acceptance", ack, err)
	}
	if !warnings.logged("WARN bitget: trade connection refused an order nobody waits for", "client_order_id="+string(given.ClientOrderID), "code=25203") {
		t.Errorf("the late refusal of %s is not a warning with its code", given.ClientOrderID)
	}
}

// While the trade connection is not up - here its login is refused - an
// order goes by REST, once, and no order frame is written. With the rest
// transport the trade connection is not used at all.
func TestTradeFallsBackToRESTWhileTheConnectionIsNotUp(t *testing.T) {
	r := newTradeRig(t, frameLoginRefused, func(int, int, string) ([]string, bool) { return nil, false })
	waitFor(t, "the refused login", func() bool {
		frames, _, _ := r.server.seen()
		return slices.ContainsFunc(frames, func(f []string) bool { return len(f) == 1 })
	})
	time.Sleep(30 * time.Millisecond)
	if r.client.trade.isUp() {
		t.Fatal("the trade connection is up after its login was refused")
	}
	ack, err := r.client.PlaceOrder(context.Background(), entryOrder)
	if err != nil || ack.OrderID != "1380000000000000099" {
		t.Fatalf("the order while the trade connection is down: %+v, %v; want REST's acceptance", ack, err)
	}
	r.mu.Lock()
	posts := slices.Clone(r.posts)
	r.mu.Unlock()
	if len(posts) != 1 || !strings.Contains(posts[0], `"clientOid":"`+ourOrder+`"`) || !strings.Contains(posts[0], `"category":"USDT-FUTURES"`) {
		t.Errorf("REST received %v, want the one order in the REST spelling", posts)
	}
	for _, frames := range r.orderFrames(t) {
		if len(frames) != 0 {
			t.Errorf("an order frame was written on a connection that is not logged in: %v", frames)
		}
	}
	attrs := r.client.StatusAttrs()
	if i := slices.Index(attrs, any("trade_up")); i < 0 || attrs[i+1] != false {
		t.Errorf("the status values %v do not show the trade connection down", attrs)
	}

	// The rest transport: straight to REST, whatever the connection is.
	r.client.UseOrderTransport(cex.OrderTransportREST)
	if _, err := r.client.PlaceOrder(context.Background(), orderWithID(2, "R")); err != nil || r.restPosts() != 2 {
		t.Errorf("with the rest transport: %v, %d REST orders", err, r.restPosts())
	}
	if attrs := r.client.StatusAttrs(); slices.Index(attrs, any("trade_up")) >= 0 {
		t.Errorf("the status values %v show a trade connection with the rest transport", attrs)
	}
}

// Run starts the trade connection with the ws transport, and only then; it
// ends with the context, and the connection with it.
func TestRunStartsTheTradeConnectionForTheWSTransport(t *testing.T) {
	client, server := newPrivateServer(t, loginThenIdle)
	server.mu.Lock()
	server.rest = func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"code":"00000","msg":"success","requestTime":1,"data":{"serverTime":"1"}}`))
	}
	server.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { client.Run(ctx); close(done) }()
	time.Sleep(60 * time.Millisecond)
	if frames, _, _ := server.seen(); len(frames) != 0 {
		t.Errorf("with the rest transport Run opened %d WebSocket connections", len(frames))
	}
	cancel()
	<-done

	client.UseOrderTransport(cex.OrderTransportWS)
	ctx, cancel = context.WithCancel(context.Background())
	done = make(chan struct{})
	go func() { client.Run(ctx); close(done) }()
	waitFor(t, "the trade connection", client.trade.isUp)
	cancel()
	<-done
	waitFor(t, "the trade connection to close", func() bool { return !client.trade.isUp() })
	if _, _, sent := client.trade.place(context.Background(), entryOrder); sent {
		t.Error("an order frame was written after Run stopped")
	}
}

// The status line reads the trade connection's ping from another goroutine
// while Run starts the connection; run with -race.
func TestStatusAttrsWhileRunStartsTheTradeConnection(t *testing.T) {
	client, server := newPrivateServer(t, loginThenIdle)
	server.mu.Lock()
	server.rest = func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"code":"00000","msg":"success","requestTime":1,"data":{"serverTime":"1"}}`))
	}
	server.mu.Unlock()
	client.UseOrderTransport(cex.OrderTransportWS)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { client.Run(ctx); close(done) }()
	for range 50 {
		client.StatusAttrs()
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
}
