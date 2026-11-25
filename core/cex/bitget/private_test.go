package bitget

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"etf-pair-arbitrage/core/cex"
)

// Frames of the private stream, written by hand in the exchange's shape:
// the control frames as observed, the data rows with made-up ids,
// quantities and amounts.
const (
	frameLoginOK      = `{"event":"login","code":0,"connId":"0a0b0c0d0e0f0001"}`
	frameLoginRefused = `{"event":"error","code":"30005","msg":"Invalid ACCESS_KEY"}`
	wantSubscribe     = `{"op":"subscribe","args":[{"instType":"UTA","topic":"order"},{"instType":"UTA","topic":"fill"},{"instType":"UTA","topic":"position"}]}`
)

func privateAck(topic string) string {
	return fmt.Sprintf(`{"event":"subscribe","arg":{"instType":"UTA","topic":%q},"connId":"0a0b0c0d0e0f0001"}`, topic)
}

func privatePush(action, topic string, rows ...string) string {
	return fmt.Sprintf(`{"action":%q,"arg":{"instType":"UTA","topic":%q},"data":[%s],"ts":1795580000321}`, action, topic, strings.Join(rows, ","))
}

// positionPush is one row of the position topic: the quantity is size, the
// direction posSide, and there is no category.
func positionPush(symbol, side, size, avg, status string) string {
	return fmt.Sprintf(`{"symbol":%q,"marginCoin":"USDT","marginSize":"53.6","marginMode":"crossed","posSide":%q,"holdMode":"one_way_mode","positionStatus":%q,"size":%q,"available":%q,"frozen":"0","avgPrice":%q,"leverage":"10","curRealisedPnl":"0","unrealisedPnl":"0.4","liqPrice":"0","mmr":"0.01","markPrice":"42.9","openFeeTotal":"-0.3","closeFeeTotal":"0","breakEvenPrice":"42.99","profitRate":"0.007","totalFundingFee":"0","cashDividend":"0","createdTime":"1795579990150","updatedTime":"1795579990170"}`,
		symbol, side, status, size, size, avg)
}

// orderPush is one row of the order topic: lower-case category and
// reduceOnly, holdSide for the position side.
func orderPush(clientOid, symbol, status, execQty, execValue, fee string) string {
	return fmt.Sprintf(`{"category":"usdt-futures","symbol":%q,"orderId":"1380000000000000017","clientOid":%q,"price":"42.95","qty":"11.68","amount":"0","holdMode":"one_way_mode","holdSide":"long","tradeSide":"open","orderType":"limit","timeInForce":"ioc","side":"buy","marginMode":"crossed","marginCoin":"USDT","reduceOnly":"no","cumExecQty":%q,"cumExecValue":%q,"avgPrice":"42.93","totalProfit":"0","orderStatus":%q,"cancelReason":"","leverage":"10","feeDetail":[{"feeCoin":"USDT","fee":%q}],"createdTime":"1795579990100","updatedTime":"1795579990160","stpMode":"none","delegateType":"normal"}`,
		symbol, clientOid, execQty, execValue, status, fee)
}

// fillPush is one row of the fill topic: the execution's time is execTime.
func fillPush(execID, clientOid, symbol string) string {
	return fmt.Sprintf(`{"category":"usdt-futures","symbol":%q,"orderId":"1380000000000000017","clientOid":%q,"execId":%q,"execLinkId":"","orderType":"limit","side":"buy","holdSide":"long","tradeSide":"open","execPrice":"42.93","execQty":"11.68","execValue":"501.4224","execPnl":"0","tradeScope":"taker","feeDetail":[{"feeCoin":"USDT","fee":"0.30085344"}],"execTime":"1795579990160","updatedTime":"1795579990161","isRPI":"no"}`,
		symbol, clientOid, execID)
}

// privateServer is a local stand-in for the exchange's private stream. Each
// connection runs the script of its turn on the handler's goroutine: read
// returns the client's next text frame other than "ping", which is answered
// with "pong" - after pingDelay, when set - and counted. Every frame read is
// recorded with its arrival. A request that is not a WebSocket upgrade goes
// to rest, when set: the same address then serves the REST API too.
type privateServer struct {
	*httptest.Server
	mu        sync.Mutex
	frames    [][]string    // per connection, the client's frames in order
	times     [][]time.Time // their arrivals
	pings     int
	pingsBy   []int                        // per connection, the pings it got
	pingAt    [][]time.Time                // per connection, when each ping arrived
	pingDelay func(conn int) time.Duration // how long connection conn (from 1) takes to answer a ping
	rest      http.HandlerFunc
}

func newPrivateServer(t *testing.T, script func(conn int, read func() (string, bool), send func(string))) (*Client, *privateServer) {
	t.Helper()
	s := &privateServer{}
	upgrader := websocket.Upgrader{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		rest := s.rest
		s.mu.Unlock()
		if rest != nil && !websocket.IsWebSocketUpgrade(r) {
			rest(w, r)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		s.mu.Lock()
		s.frames, s.times, s.pingsBy, s.pingAt = append(s.frames, nil), append(s.times, nil), append(s.pingsBy, 0), append(s.pingAt, nil)
		n := len(s.frames)
		s.mu.Unlock()
		send := func(msg string) { conn.WriteMessage(websocket.TextMessage, []byte(msg)) }
		read := func() (string, bool) {
			for {
				_, msg, err := conn.ReadMessage()
				if err != nil {
					return "", false
				}
				s.mu.Lock()
				if string(msg) == "ping" {
					s.pings++
					s.pingsBy[n-1]++
					s.pingAt[n-1] = append(s.pingAt[n-1], time.Now())
					delay := s.pingDelay
					s.mu.Unlock()
					if delay != nil {
						time.Sleep(delay(n))
					}
					send("pong")
					continue
				}
				s.frames[n-1], s.times[n-1] = append(s.frames[n-1], string(msg)), append(s.times[n-1], time.Now())
				s.mu.Unlock()
				return string(msg), true
			}
		}
		script(n, read, send)
	}))
	t.Cleanup(s.Close)
	client := testClient(s.URL)
	client.wsPrivateURL = "ws" + strings.TrimPrefix(s.URL, "http")
	client.creds, client.now = fakeCreds, func() time.Time { return fixedNow }
	return client, s
}

func (s *privateServer) seen() (frames [][]string, times [][]time.Time, pings int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.frames {
		frames, times = append(frames, append([]string(nil), s.frames[i]...)), append(times, append([]time.Time(nil), s.times[i]...))
	}
	return frames, times, s.pings
}

// privateCollector gathers what a private subscription reports, each event
// as one line of text.
type privateCollector struct {
	mu     sync.Mutex
	events []string
	downs  int
}

func (c *privateCollector) onEvent(ev cex.PrivateEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case ev.Order != nil:
		o := ev.Order
		c.events = append(c.events, fmt.Sprintf("order %s %s %s reduce=%t %s exec %s value %s fee %s known=%t", o.Request.ClientOrderID, o.Request.Symbol, o.Request.Side, o.Request.ReduceOnly, o.Status, o.CumExecQty, o.CumExecValue, o.Fee, o.FeeKnown))
	case ev.Fill != nil:
		f := ev.Fill
		c.events = append(c.events, fmt.Sprintf("fill %s %s %s %s %s@%s fee %s at %d", f.ExecID, f.ClientOrderID, f.Symbol, f.Side, f.Qty, f.Price, f.Fee, f.ExecTime.UnixMilli()))
	case ev.Positions != nil:
		line := fmt.Sprintf("positions loaded=%t", ev.Positions.Loaded)
		for _, symbol := range []cex.Symbol{"SOXLUSDT", "SOXSUSDT", "BTCUSDT", "ETHUSDT"} {
			if p, held := ev.Positions.BySymbol[symbol]; held {
				line += fmt.Sprintf(" %s=%s@%s", symbol, p.Qty, p.AvgPrice)
			}
		}
		c.events = append(c.events, line)
	}
}

func (c *privateCollector) onDown(error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.downs++
}

func (c *privateCollector) snapshot() ([]string, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.events...), c.downs
}

var pairSymbols = []cex.Symbol{"SOXLUSDT", "SOXSUSDT"}

// The private stream logs in with a signed frame, subscribes the three
// account-wide topics only after the exchange accepted the login, and is up
// once all three are acknowledged. Of what the account pushes it hands over
// the pair's contracts only, and of orders and fills only those with this
// program's client id; every position event is the whole set of the pair,
// in which a contract the exchange does not list is flat.
func TestPrivateStreamLoginSubscribeAndFilter(t *testing.T) {
	var loginAnswered time.Time
	twoAcks, proceed := make(chan struct{}), make(chan struct{})
	client, server := newPrivateServer(t, func(_ int, read func() (string, bool), send func(string)) {
		if _, ok := read(); !ok { // the login
			return
		}
		time.Sleep(60 * time.Millisecond) // a subscribe sent without waiting would arrive in here
		loginAnswered = time.Now()
		send(frameLoginOK)
		if _, ok := read(); !ok { // the subscribe
			return
		}
		send(privateAck("order"))
		send(privateAck("fill"))
		close(twoAcks)
		<-proceed
		send(privateAck("position"))
		// The first position frame: the account's positions that are not
		// zero, two of them another program's.
		send(privatePush("snapshot", "position", positionPush("BTCUSDT", "long", "0.004", "91234.5", "opening"), positionPush("SOXLUSDT", "long", "11.68", "42.93", "opening"),
			positionPush("ETHUSDT", "short", "0.5", "3050.1", "opening")))
		// Orders, then fills: ours; another program's on the same contract;
		// one with our prefix on a contract that is not the pair's.
		send(privatePush("snapshot", "order", orderPush(ourOrder, "SOXLUSDT", "filled", "11.68", "501.4224", "0.300853440000000017")))
		send(privatePush("snapshot", "order", orderPush(otherOrder, "SOXLUSDT", "filled", "3", "128.79", "0.077274"), orderPush("etfp1795579990000-0001BE", "BTCUSDT", "filled", "1", "1", "0.1")))
		send(privatePush("snapshot", "fill", fillPush("1380000000000000301", otherOrder, "SOXLUSDT"), fillPush("1380000000000000302", ourOrder, "SOXLUSDT"),
			fillPush("1380000000000000303", "etfp1795579990000-0001BE", "BTCUSDT")))
		// Position updates: the bear leg opens short; another contract
		// changes; the bull leg is closed.
		send(privatePush("update", "position", positionPush("SOXSUSDT", "short", "10.1", "49.406", "opening")))
		send(privatePush("update", "position", positionPush("BTCUSDT", "long", "0.001", "91234.5", "opening")))
		send(privatePush("update", "position", positionPush("SOXLUSDT", "long", "0", "42.93", "ended")))
		read() // serve pings until the client leaves
	})
	var got privateCollector
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := client.SubscribePrivate(ctx, pairSymbols, got.onEvent, got.onDown); err != nil {
		t.Fatal(err)
	}

	// Two of three acknowledgements are not a working stream.
	<-twoAcks
	time.Sleep(30 * time.Millisecond)
	if client.PrivateUp() {
		t.Error("the stream reads as up with one topic unacknowledged")
	}
	close(proceed)
	want := []string{
		"positions loaded=true SOXLUSDT=11.68@42.93",
		"order " + ourOrder + " SOXLUSDT buy reduce=false filled exec 11.68 value 501.4224 fee 0.30085344 known=true",
		"fill 1380000000000000302 " + ourOrder + " SOXLUSDT buy 11.68@42.93 fee 0.30085344 at 1795579990160",
		"positions loaded=true SOXLUSDT=11.68@42.93 SOXSUSDT=-10.1@49.406",
		"positions loaded=true SOXSUSDT=-10.1@49.406",
	}
	waitFor(t, "the five events", func() bool { events, _ := got.snapshot(); return len(events) >= len(want) })
	time.Sleep(60 * time.Millisecond) // anything wrongly handed over would arrive by now, and pings are due
	events, downs := got.snapshot()
	if strings.Join(events, "\n") != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\nwant:\n%s", strings.Join(events, "\n"), strings.Join(want, "\n"))
	}
	if !client.PrivateUp() || downs != 0 {
		t.Errorf("up %t, %d losses; want the stream up without a loss", client.PrivateUp(), downs)
	}

	// The frames the client sent: a login signed over the client's clock,
	// then, after the exchange's answer, one subscribe of the three topics.
	frames, times, pings := server.seen()
	if len(frames) != 1 || len(frames[0]) != 2 {
		t.Fatalf("client frames = %v, want a login and a subscribe on one connection", frames)
	}
	wantLogin := `{"op":"login","args":[{"apiKey":"FAKE-KEY-111","passphrase":"FAKE-PASS-333","timestamp":"1795580000000","sign":"zAcRo/gzljl+Rm8PBOWJK6AmnUZ34b1tcoEBHjSAcgM="}]}`
	if frames[0][0] != wantLogin {
		t.Errorf("login = %s\nwant    %s", frames[0][0], wantLogin)
	}
	if frames[0][1] != wantSubscribe {
		t.Errorf("subscribe = %s\nwant        %s", frames[0][1], wantSubscribe)
	}
	if times[0][1].Before(loginAnswered) {
		t.Errorf("the subscribe arrived %s before the login was answered", loginAnswered.Sub(times[0][1]))
	}
	if pings < 2 {
		t.Errorf("the server got %d pings", pings)
	}

	// Ending the context closes the stream: it is no longer up, and the
	// closing is not a loss.
	cancel()
	waitFor(t, "the stream to read as down", func() bool { return !client.PrivateUp() })
	if _, downs := got.snapshot(); downs != 0 {
		t.Errorf("%d losses reported for a closed stream", downs)
	}
}

// After a lost connection the client reports the loss, is not up, and on
// the next connection logs in and subscribes again; the positions start
// from that connection's first frame, not from what the last one held.
func TestPrivateStreamReconnects(t *testing.T) {
	client, server := newPrivateServer(t, func(conn int, read func() (string, bool), send func(string)) {
		if _, ok := read(); !ok {
			return
		}
		send(frameLoginOK)
		if _, ok := read(); !ok {
			return
		}
		for _, topic := range []string{"order", "fill", "position"} {
			send(privateAck(topic))
		}
		if conn == 1 {
			send(privatePush("snapshot", "position", positionPush("SOXLUSDT", "long", "11.68", "42.93", "opening")))
			time.Sleep(40 * time.Millisecond)
			return // the handler's return drops the connection
		}
		// An update ahead of the snapshot says nothing about the other leg
		// and is not handed over; the snapshot lists another program's
		// position only: both legs are flat.
		send(privatePush("update", "position", positionPush("SOXSUSDT", "short", "10.1", "49.406", "opening")))
		send(privatePush("snapshot", "position", positionPush("BTCUSDT", "long", "0.004", "91234.5", "opening")))
		read()
	})
	var got privateCollector
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := client.SubscribePrivate(ctx, pairSymbols, got.onEvent, got.onDown); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the first connection's positions", func() bool { events, _ := got.snapshot(); return len(events) == 1 })
	waitFor(t, "the loss", func() bool { _, downs := got.snapshot(); return downs == 1 })
	waitFor(t, "the second connection's positions", func() bool { events, _ := got.snapshot(); return len(events) == 2 })
	waitFor(t, "the stream up again", client.PrivateUp)
	events, downs := got.snapshot()
	if events[0] != "positions loaded=true SOXLUSDT=11.68@42.93" || events[1] != "positions loaded=true" || downs != 1 {
		t.Errorf("events %q, %d losses; want the first connection's position, one loss, then both legs flat", events, downs)
	}
	frames, _, _ := server.seen()
	if len(frames) != 2 {
		t.Fatalf("%d connections, want two", len(frames))
	}
	for i, sent := range frames {
		if len(sent) != 2 || !strings.HasPrefix(sent[0], `{"op":"login",`) || sent[1] != wantSubscribe {
			t.Errorf("connection %d sent %v, want a login and the subscribe", i+1, sent)
		}
	}
}

// While the connection is lost the stream is not up, and the next
// connection is up only with its own three acknowledgements: those of the
// last connection do not count.
func TestPrivateStreamIsDownBetweenConnections(t *testing.T) {
	hold := make(chan struct{})
	oneAck := make(chan struct{}, 8)
	client, _ := newPrivateServer(t, func(conn int, read func() (string, bool), send func(string)) {
		read()
		send(frameLoginOK)
		read()
		send(privateAck("order"))
		if conn > 1 {
			oneAck <- struct{}{}
			<-hold // the other two topics are never acknowledged
			return
		}
		send(privateAck("fill"))
		send(privateAck("position"))
		time.Sleep(40 * time.Millisecond)
	})
	defer close(hold)
	var got privateCollector
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := client.SubscribePrivate(ctx, pairSymbols, got.onEvent, got.onDown); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the stream up", client.PrivateUp)
	waitFor(t, "the loss", func() bool { _, downs := got.snapshot(); return downs == 1 })
	if client.PrivateUp() {
		t.Error("the stream reads as up after its connection was lost")
	}
	<-oneAck
	time.Sleep(40 * time.Millisecond) // the client has read the one acknowledgement by now
	if client.PrivateUp() {
		t.Error("the stream reads as up on a connection with one of three topics acknowledged")
	}
}

// A refused login is not followed by a subscription, and the stream is not
// up.
func TestPrivateStreamLoginRefused(t *testing.T) {
	client, server := newPrivateServer(t, func(_ int, read func() (string, bool), send func(string)) {
		if _, ok := read(); !ok {
			return
		}
		send(frameLoginRefused)
		read()
	})
	var got privateCollector
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := client.SubscribePrivate(ctx, pairSymbols, got.onEvent, got.onDown); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the login", func() bool { frames, _, _ := server.seen(); return len(frames) == 1 && len(frames[0]) == 1 })
	time.Sleep(80 * time.Millisecond)
	frames, _, _ := server.seen()
	if len(frames[0]) != 1 {
		t.Errorf("after a refused login the client sent %v", frames[0][1:])
	}
	if events, _ := got.snapshot(); client.PrivateUp() || len(events) != 0 {
		t.Errorf("up %t, events %v; want neither", client.PrivateUp(), events)
	}
}
