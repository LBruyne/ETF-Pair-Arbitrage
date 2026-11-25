package bitget

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
)

// Frames as the exchange sent them (recorded 2026-11-24; the books50 frame
// is cut to three levels a side, the update is one of the full-book topic).
const (
	frameBooks5   = `{"action":"snapshot","arg":{"instType":"usdt-futures","topic":"books5","symbol":"SOXLUSDT"},"data":[{"a":[["42.81","804.24"],["42.82","3006.07"],["42.83","3362.03"],["42.84","2240.88"],["42.85","2504.69"]],"b":[["42.8","266.96"],["42.79","2600.79"],["42.78","2415.07"],["42.77","3141.16"],["42.76","2473.27"]],"seq":702640680409,"pseq":0,"ts":"1795501994624"}],"ts":1795501994628}`
	frameBooks1   = `{"action":"snapshot","arg":{"instType":"usdt-futures","topic":"books1","symbol":"SOXLUSDT"},"data":[{"a":[["42.81","804.24"]],"b":[["42.8","373.98"]],"seq":702640681270,"pseq":0,"ts":"1795501994657"}],"ts":1795501994658}`
	frameBooks50  = `{"action":"snapshot","arg":{"instType":"usdt-futures","topic":"books50","symbol":"SOXSUSDT"},"data":[{"a":[["49.718","13.22"],["49.719","10.06"],["49.721","0.13"]],"b":[["49.708","1.81"],["49.707","14.54"],["49.705","20.15"]],"seq":702640680794,"pseq":0,"ts":"1795501994643"}],"ts":1795501994646}`
	frameUpdate   = `{"action":"update","arg":{"instType":"usdt-futures","topic":"books","symbol":"SOXLUSDT"},"data":[{"a":[["42.84","2240.88"],["42.86","0"]],"b":[["41.73","0.3"]],"seq":702640681249,"pseq":702640679901,"ts":"1795501994656","maxdepth":"1000"}],"ts":1795501994658}`
	frameSubAck   = `{"event":"subscribe","arg":{"instType":"usdt-futures","topic":"books5","symbol":"SOXLUSDT"},"connId":"0affd3fffe28b41f"}`
	frameSubError = `{"event":"error","arg":{"instType":"usdt-futures","topic":"books7","symbol":"SOXLUSDT"},"code":30001,"msg":"topic books7 does not exist"}`
)

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }

// decode runs one raw frame through the frame handler and returns the events
// it produced.
func decode(raw string, at time.Time) []cex.BookEvent {
	var out []cex.BookEvent
	handleBookFrame([]byte(raw), at, func(ev cex.BookEvent) { out = append(out, ev) })
	return out
}

// A recorded snapshot becomes one event with the frame's contract, topic,
// sequence numbers, exchange time and levels, best first.
func TestBookFrameDecoding(t *testing.T) {
	at := time.UnixMilli(1795501994700)
	evs := decode(frameBooks5, at)
	if len(evs) != 1 {
		t.Fatalf("%d events from one snapshot frame", len(evs))
	}
	ev := evs[0]
	if ev.Symbol != "SOXLUSDT" || ev.Topic != cex.TopicBooks5 || ev.Action != cex.ActionSnapshot || ev.Seq != 702640680409 || ev.PrevSeq != 0 ||
		!ev.ExchangeTime.Equal(time.UnixMilli(1795501994624)) || !ev.ReceivedAt.Equal(at) {
		t.Errorf("event = %+v", ev)
	}
	if len(ev.Bids) != 5 || len(ev.Asks) != 5 || !ev.Bids[0].Price.Equal(d("42.8")) || !ev.Bids[0].Qty.Equal(d("266.96")) || !ev.Asks[0].Price.Equal(d("42.81")) || !ev.Asks[4].Qty.Equal(d("2504.69")) {
		t.Errorf("levels = %v / %v", ev.Bids, ev.Asks)
	}

	if evs := decode(frameBooks1, at); len(evs) != 1 || evs[0].Topic != cex.TopicBooks1 || evs[0].Seq != 702640681270 || len(evs[0].Bids) != 1 || !evs[0].Bids[0].Qty.Equal(d("373.98")) {
		t.Errorf("books1 = %+v", evs)
	}
	if evs := decode(frameBooks50, at); len(evs) != 1 || evs[0].Symbol != "SOXSUSDT" || evs[0].Topic != cex.TopicBooks50 || !evs[0].Asks[2].Price.Equal(d("49.721")) {
		t.Errorf("books50 = %+v", evs)
	}

	// An update keeps the sequence number it follows and its zero
	// quantities, which delete a price.
	evs = decode(frameUpdate, at)
	if len(evs) != 1 || evs[0].Action != cex.ActionUpdate || evs[0].Seq != 702640681249 || evs[0].PrevSeq != 702640679901 || !evs[0].Asks[1].Qty.IsZero() {
		t.Errorf("update = %+v", evs)
	}
}

// Control frames and frames that cannot be read produce no event.
func TestBookFrameControlAndMalformed(t *testing.T) {
	at := time.Now()
	for name, raw := range map[string]string{
		"subscribe ack":       frameSubAck,
		"subscription error":  frameSubError,
		"not JSON":            `pong?`,
		"a negative quantity": strings.Replace(frameBooks1, `"373.98"`, `"-1"`, 1),
		"a price of zero":     strings.Replace(frameBooks1, `"42.8"`, `"0"`, 1),
		"a level of one item": strings.Replace(frameBooks1, `["42.8","373.98"]`, `["42.8"]`, 1),
		"an unknown action":   strings.Replace(frameBooks1, `"snapshot"`, `"delta"`, 1),
		"a seq that is text":  strings.Replace(frameBooks1, `702640681270`, `"abc"`, 1),
	} {
		if evs := decode(raw, at); len(evs) != 0 {
			t.Errorf("%s produced %d events", name, len(evs))
		}
	}
}

// --- a local WebSocket server ---

// wsServer is a local stand-in for the exchange's public stream. Each
// connection reads the subscribe request, then runs the script of its turn;
// it answers "ping" with "pong" unless told to stay silent.
type wsServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []wsRequest // the subscribe request of each connection
	opened   []time.Time // when each connection's subscribe request arrived
	closed   []time.Time // when the script closed each connection; zero while it has not
	pings    int
	silent   bool // answer nothing and send nothing after the subscription
	script   func(conn int, send func(string), closeConn func())
}

func newWSServer(t *testing.T, script func(conn int, send func(string), closeConn func())) *wsServer {
	t.Helper()
	s := &wsServer{script: script}
	upgrader := websocket.Upgrader{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var writeMu sync.Mutex
		send := func(msg string) {
			writeMu.Lock()
			defer writeMu.Unlock()
			conn.WriteMessage(websocket.TextMessage, []byte(msg))
		}
		_, first, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var req wsRequest
		json.Unmarshal(first, &req)
		s.mu.Lock()
		s.requests, s.opened, s.closed = append(s.requests, req), append(s.opened, time.Now()), append(s.closed, time.Time{})
		n, silent := len(s.requests), s.silent
		s.mu.Unlock()
		if !silent {
			go s.script(n, send, func() {
				s.mu.Lock()
				s.closed[n-1] = time.Now()
				s.mu.Unlock()
				conn.Close()
			})
		}
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if string(msg) == "ping" {
				s.mu.Lock()
				s.pings++
				silent := s.silent
				s.mu.Unlock()
				if !silent {
					send("pong")
				}
			}
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *wsServer) connections() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

// testClient is a client pointed at the local server with short clocks.
func testClient(url string) *Client {
	c := NewClient(cex.Credentials{})
	c.wsPublicURL = "ws" + strings.TrimPrefix(url, "http")
	c.restURL = url
	c.timing = wsTimings{dialTimeout: time.Second, pingInterval: 20 * time.Millisecond, readTimeout: 150 * time.Millisecond, writeTimeout: time.Second,
		reconnectMin: 10 * time.Millisecond, reconnectMax: 40 * time.Millisecond}
	return c
}

// collector gathers what a subscription reports.
type collector struct {
	mu     sync.Mutex
	events []cex.BookEvent
	downs  int
}

func (c *collector) onEvent(ev cex.BookEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, ev)
}

func (c *collector) onDown(error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.downs++
}

func (c *collector) snapshot() (seqs []uint64, downs int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, ev := range c.events {
		seqs = append(seqs, ev.Seq)
	}
	return seqs, c.downs
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); !cond(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// The client dials, subscribes the topics in one request, delivers the
// frames in order, pings, and after the server drops the connection reports
// it down, reconnects and subscribes again.
func TestSubscribeBooksDeliversAndReconnects(t *testing.T) {
	server := newWSServer(t, func(conn int, send func(string), closeConn func()) {
		send(frameSubAck)
		if conn == 1 {
			send(frameBooks5)
			send(frameBooks1)
			time.Sleep(70 * time.Millisecond) // long enough for pings
			closeConn()
			return
		}
		send(strings.Replace(frameBooks5, "702640680409", "702640690000", 1))
	})
	client := testClient(server.URL)
	var got collector
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	subs := []cex.BookSubscription{{Symbol: "SOXLUSDT", Topic: cex.TopicBooks5}, {Symbol: "SOXLUSDT", Topic: cex.TopicBooks1}}
	if err := client.SubscribeBooks(ctx, subs, got.onEvent, got.onDown); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the frames of both connections", func() bool { seqs, _ := got.snapshot(); return len(seqs) == 3 })
	seqs, downs := got.snapshot()
	if seqs[0] != 702640680409 || seqs[1] != 702640681270 || seqs[2] != 702640690000 || downs != 1 {
		t.Errorf("events %v, %d losses; want the two frames, one loss, the frame of the second connection", seqs, downs)
	}
	server.mu.Lock()
	requests, pings := server.requests, server.pings
	server.mu.Unlock()
	want := wsRequest{Op: "subscribe", Args: []wsTopicArg{{InstType: "usdt-futures", Topic: "books5", Symbol: "SOXLUSDT"}, {InstType: "usdt-futures", Topic: "books1", Symbol: "SOXLUSDT"}}}
	for i, req := range requests {
		if req.Op != want.Op || len(req.Args) != 2 || req.Args[0] != want.Args[0] || req.Args[1] != want.Args[1] {
			t.Errorf("connection %d subscribed %+v, want %+v", i+1, req, want)
		}
	}
	if pings < 2 {
		t.Errorf("the server got %d pings in 70 ms at one per 20 ms", pings)
	}

	// Cancelling the context ends the subscription: no further connection,
	// and the closing of the connection is not reported as a loss.
	cancel()
	time.Sleep(100 * time.Millisecond)
	if n := server.connections(); n != 2 {
		t.Errorf("%d connections after the context ended, want 2", n)
	}
	if _, downs := got.snapshot(); downs != 1 {
		t.Errorf("%d losses reported after the context ended, want still 1: ending a subscription is not a loss", downs)
	}
	if err := client.SubscribeBooks(context.Background(), nil, got.onEvent, got.onDown); err == nil {
		t.Error("a subscription without a topic was accepted")
	}
}

// A connection on which nothing arrives, not even a pong, is dead after the
// read timeout: the client reports it and reconnects, waiting longer each
// time up to the maximum.
func TestSubscribeBooksReadTimeoutAndBackoff(t *testing.T) {
	server := newWSServer(t, func(int, func(string), func()) {})
	server.silent = true
	client := testClient(server.URL)
	var got collector
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	began := time.Now()
	client.SubscribeBooks(ctx, []cex.BookSubscription{{Symbol: "SOXLUSDT", Topic: cex.TopicBooks5}}, got.onEvent, got.onDown)
	waitFor(t, "three connections", func() bool { return server.connections() == 3 })
	// Two read timeouts of 150 ms and the waits of 10 and 20 ms lie behind
	// the third connection.
	if elapsed := time.Since(began); elapsed < 320*time.Millisecond {
		t.Errorf("the third connection came after %s, before two read timeouts and their waits could pass", elapsed)
	}
	if _, downs := got.snapshot(); downs < 2 {
		t.Errorf("%d losses reported for two dead connections", downs)
	}
}

// times returns when each connection opened and when its script closed it.
func (s *wsServer) times() (opened, closed []time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.opened...), append([]time.Time(nil), s.closed...)
}

// After every loss the client waits before it dials again, twice as long
// each time up to the maximum: 40, 80, 160 and again 160 ms here, measured
// from one connection's closing to the next one's opening.
func TestReconnectWaitDoublesUpToTheMaximum(t *testing.T) {
	server := newWSServer(t, func(_ int, _ func(string), closeConn func()) { closeConn() })
	client := testClient(server.URL)
	client.timing.reconnectMin, client.timing.reconnectMax, client.timing.readTimeout = 40*time.Millisecond, 160*time.Millisecond, 5*time.Second
	var got collector
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client.SubscribeBooks(ctx, []cex.BookSubscription{{Symbol: "SOXLUSDT", Topic: cex.TopicBooks5}}, got.onEvent, got.onDown)
	waitFor(t, "five connections", func() bool { return server.connections() == 5 })
	opened, closed := server.times()
	for i, want := range []time.Duration{40 * time.Millisecond, 80 * time.Millisecond, 160 * time.Millisecond, 160 * time.Millisecond} {
		wait := opened[i+1].Sub(closed[i])
		// Never shorter than the wait; the dial and the scheduler add a
		// little, far less than the next doubling would.
		if wait < want-5*time.Millisecond || wait > want+want/2+20*time.Millisecond {
			t.Errorf("wait %d before reconnecting was %s, want about %s", i+1, wait.Round(time.Millisecond), want)
		}
	}
}

// A connection that lived at least as long as the maximum wait resets the
// wait to its minimum: after two short-lived connections (waits of 40 and 80
// ms) and one that held, the client is back within 40 ms, not 160.
func TestReconnectWaitResetsAfterALongLivedConnection(t *testing.T) {
	server := newWSServer(t, func(conn int, _ func(string), closeConn func()) {
		if conn == 3 {
			time.Sleep(250 * time.Millisecond) // longer than the maximum wait
		}
		if conn <= 3 {
			closeConn()
		}
	})
	client := testClient(server.URL)
	client.timing.reconnectMin, client.timing.reconnectMax, client.timing.readTimeout = 40*time.Millisecond, 160*time.Millisecond, 5*time.Second
	var got collector
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client.SubscribeBooks(ctx, []cex.BookSubscription{{Symbol: "SOXLUSDT", Topic: cex.TopicBooks5}}, got.onEvent, got.onDown)
	waitFor(t, "four connections", func() bool { return server.connections() == 4 })
	opened, closed := server.times()
	if wait := opened[2].Sub(closed[1]); wait < 75*time.Millisecond {
		t.Fatalf("the wait before the third connection was %s: it should have doubled to 80 ms by then", wait.Round(time.Millisecond))
	}
	if wait := opened[3].Sub(closed[2]); wait < 35*time.Millisecond || wait > 100*time.Millisecond {
		t.Errorf("the wait after the long-lived connection was %s, want the minimum of 40 ms again", wait.Round(time.Millisecond))
	}
}

// --- REST ---

// instrumentsSOXS is the exchange's answer for SOXSUSDT (recorded
// 2026-11-24, fields this client does not read left out).
const instrumentsSOXS = `{"code":"00000","msg":"success","requestTime":1795511550300,"data":[{"symbol":"SOXSUSDT","category":"USDT-FUTURES","baseCoin":"SOXS","quoteCoin":"USDT","isRwa":"YES","buyLimitPriceRatio":"0.05","sellLimitPriceRatio":"0.05","minOrderQty":"0.01","pricePrecision":"3","quantityPrecision":"2","priceMultiplier":"0.001","quantityMultiplier":"0.01","type":"perpetual","minOrderAmount":"5","status":"online","fundInterval":"8","maxLeverage":"20"}]}`

// restServer answers the two public endpoints and records the requests.
func restServer(t *testing.T, answers map[string]string) (*Client, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		mu.Unlock()
		for _, header := range []string{"ACCESS-KEY", "ACCESS-SIGN", "ACCESS-PASSPHRASE"} {
			if r.Header.Get(header) != "" {
				t.Errorf("a public request carries %s", header)
			}
		}
		body, ok := answers[r.URL.Path+"?"+r.URL.RawQuery]
		if !ok {
			w.WriteHeader(http.StatusBadGateway)
			w.Write([]byte("<html>bad gateway</html>"))
			return
		}
		if strings.Contains(body, `"code":"40034"`) {
			w.WriteHeader(http.StatusBadRequest)
		}
		w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return testClient(server.URL), &seen
}

func TestMarketRules(t *testing.T) {
	soxl := strings.NewReplacer("SOXSUSDT", "SOXLUSDT", `"priceMultiplier":"0.001"`, `"priceMultiplier":"0.01"`).Replace(instrumentsSOXS)
	client, seen := restServer(t, map[string]string{
		"/api/v3/market/instruments?category=USDT-FUTURES&symbol=SOXLUSDT": soxl,
		"/api/v3/market/instruments?category=USDT-FUTURES&symbol=SOXSUSDT": instrumentsSOXS,
		"/api/v3/market/instruments?category=USDT-FUTURES&symbol=NOPEUSDT": `{"code":"40034","msg":"Parameter does not exist","requestTime":1,"data":null}`,
		"/api/v3/market/instruments?category=USDT-FUTURES&symbol=HALTUSDT": strings.NewReplacer("SOXSUSDT", "HALTUSDT", `"status":"online"`, `"status":"offline"`).Replace(instrumentsSOXS),
	})
	ctx := context.Background()
	rules, err := client.MarketRules(ctx, []cex.Symbol{"SOXLUSDT", "SOXSUSDT"})
	if err != nil {
		t.Fatal(err)
	}
	bear := rules["SOXSUSDT"]
	if !bear.PriceTick.Equal(d("0.001")) || !bear.QtyStep.Equal(d("0.01")) || !bear.MinQty.Equal(d("0.01")) || !bear.MinNotional.Equal(d("5")) || !bear.Online || bear.Symbol != "SOXSUSDT" {
		t.Errorf("SOXS rule = %+v", bear)
	}
	if bull := rules["SOXLUSDT"]; !bull.PriceTick.Equal(d("0.01")) || !bull.Online {
		t.Errorf("SOXL rule = %+v", bull)
	}
	if len(*seen) != 2 || !strings.HasPrefix((*seen)[0], "GET ") {
		t.Errorf("requests = %v, want one GET per contract", *seen)
	}

	// A contract that is not trading is listed as not online.
	if rules, err := client.MarketRules(ctx, []cex.Symbol{"HALTUSDT"}); err != nil || rules["HALTUSDT"].Online {
		t.Errorf("offline contract: %+v, %v", rules, err)
	}
	// The exchange's refusal is a VenueError with its code; an answer
	// without an envelope is a plain error.
	var venueErr *cex.VenueError
	if _, err := client.MarketRules(ctx, []cex.Symbol{"NOPEUSDT"}); !errors.As(err, &venueErr) || venueErr.Code != "40034" {
		t.Errorf("refused request: %v, want a VenueError with code 40034", err)
	}
	if _, err := client.MarketRules(ctx, []cex.Symbol{"OTHERUSDT"}); err == nil || errors.As(err, &venueErr) || !strings.Contains(err.Error(), "http 502") {
		t.Errorf("answer without an envelope: %v, want a plain error naming http 502", err)
	}
}

// fundingHistorySOXS is the exchange's answer for SOXSUSDT with limit=5,
// as received on 2026-11-25.
const fundingHistorySOXS = `{"code":"00000","msg":"success","requestTime":1795580226619,"data":{"resultList":[{"symbol":"SOXSUSDT","fundingRate":"-0.000538","fundingRateTimestamp":"1795564800000"},{"symbol":"SOXSUSDT","fundingRate":"0","fundingRateTimestamp":"1795536000000"},{"symbol":"SOXSUSDT","fundingRate":"0.00016","fundingRateTimestamp":"1795507200000"},{"symbol":"SOXSUSDT","fundingRate":"0.000593","fundingRateTimestamp":"1795478400000"},{"symbol":"SOXSUSDT","fundingRate":"0","fundingRateTimestamp":"1795449600000"}]}}`

func TestFundingHistory(t *testing.T) {
	client, _ := restServer(t, map[string]string{
		"/api/v3/market/history-fund-rate?category=USDT-FUTURES&limit=100&symbol=SOXSUSDT": fundingHistorySOXS,
		"/api/v3/market/history-fund-rate?category=USDT-FUTURES&limit=3&symbol=SOXLUSDT":   `{"code":"00000","msg":"success","requestTime":1,"data":{"resultList":[]}}`,
	})
	ctx := context.Background()
	// A limit above the page size is cut to it.
	got, err := client.FundingHistory(ctx, "SOXSUSDT", 500)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		rate string
		ms   int64
	}{{"-0.000538", 1795564800000}, {"0", 1795536000000}, {"0.00016", 1795507200000}, {"0.000593", 1795478400000}, {"0", 1795449600000}}
	if len(got) != len(want) {
		t.Fatalf("%d settlements %+v, want %d", len(got), got, len(want))
	}
	for i, w := range want {
		if got[i].Symbol != "SOXSUSDT" || !got[i].Rate.Equal(d(w.rate)) || !got[i].At.Equal(time.UnixMilli(w.ms)) {
			t.Errorf("settlement %d = %+v, want rate %s at %d", i, got[i], w.rate, w.ms)
		}
	}
	if got, err := client.FundingHistory(ctx, "SOXLUSDT", 3); err != nil || len(got) != 0 {
		t.Errorf("an empty history: %v, %v", got, err)
	}
}

// The account side is not implemented: every such method says so, and
// placing an order answers in the terms of "nothing left the process".
func TestAccountSideIsNotImplemented(t *testing.T) {
	client, seen := restServer(t, nil)
	ctx := context.Background()
	symbols := []cex.Symbol{"SOXLUSDT"}
	_, errSettings := client.AccountSettings(ctx, symbols)
	_, errPositions := client.Positions(ctx, symbols)
	_, errOrder := client.OrderByClientID(ctx, "SOXLUSDT", "etfp-1")
	_, errRecent := client.RecentOrders(ctx, "SOXLUSDT", time.Now())
	_, errFills := client.Fills(ctx, time.Now())
	_, errRecords := client.FinancialRecords(ctx, time.Now())
	errPrivate := client.SubscribePrivate(ctx, symbols, func(cex.PrivateEvent) {}, func(error) {})
	for name, err := range map[string]error{"AccountSettings": errSettings, "Positions": errPositions, "OrderByClientID": errOrder, "RecentOrders": errRecent,
		"Fills": errFills, "FinancialRecords": errRecords, "SubscribePrivate": errPrivate} {
		if !errors.Is(err, errNotImplemented) {
			t.Errorf("%s = %v, want the not-implemented error", name, err)
		}
	}
	if _, err := client.PlaceOrder(ctx, cex.OrderRequest{Symbol: "SOXLUSDT"}); !errors.Is(err, cex.ErrNotSent) {
		t.Errorf("PlaceOrder = %v, want cex.ErrNotSent", err)
	}
	if len(*seen) != 0 {
		t.Errorf("the account-side methods sent requests: %v", *seen)
	}
	if client.Name() != "bitget" {
		t.Errorf("Name = %q", client.Name())
	}
}
