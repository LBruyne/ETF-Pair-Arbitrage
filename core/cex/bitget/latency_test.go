package bitget

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"etf-pair-arbitrage/core/cex"
)

// restCountingServer answers every request like the public server-time
// endpoint and counts the connections opened to it and the requests by path.
type restCountingServer struct {
	*httptest.Server
	mu    sync.Mutex
	conns int
	paths []string
}

// newRESTCountingServer starts the server; idle is how long it keeps an
// idle connection, zero for ever.
func newRESTCountingServer(t *testing.T, idle time.Duration) *restCountingServer {
	t.Helper()
	s := &restCountingServer{}
	s.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.paths = append(s.paths, r.Method+" "+r.URL.Path)
		s.mu.Unlock()
		w.Header().Set(headerAcceptTime, "1795580000009")
		w.Write([]byte(`{"code":"00000","msg":"success","requestTime":1795580000010,"data":{"serverTime":"1795580000010"}}`))
	}))
	s.Config.IdleTimeout = idle
	s.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			s.mu.Lock()
			s.conns++
			s.mu.Unlock()
		}
	}
	s.Start()
	t.Cleanup(s.Close)
	return s
}

func (s *restCountingServer) seen() (conns int, paths []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conns, slices.Clone(s.paths)
}

// The client keeps an idle REST connection for five minutes where Go's
// default transport closes it after 90 seconds. Scaled a thousand times
// down: after 120 ms without a request the default has closed its
// connection and the next request opens a new one; this client's is still
// there.
func TestRESTConnectionOutlivesTheDefaultIdleLimit(t *testing.T) {
	ctx := context.Background()
	request := func(c *Client) {
		t.Helper()
		if err := c.call(ctx, http.MethodGet, pathServerTime, nil, nil, false, nil); err != nil {
			t.Fatal(err)
		}
	}
	// Go's default, scaled: idle connections closed after 90 ms.
	server := newRESTCountingServer(t, 0)
	byDefault := testClient(server.URL)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.IdleConnTimeout = 90 * time.Millisecond
	byDefault.http = &http.Client{Transport: transport}
	request(byDefault)
	time.Sleep(120 * time.Millisecond)
	request(byDefault)
	if conns, _ := server.seen(); conns != 2 {
		t.Fatalf("the default transport used %d connections for two requests 120 ms apart, want 2: this test must show the difference", conns)
	}

	// This client's transport with a limit well above the pause.
	server = newRESTCountingServer(t, 0)
	own := testClient(server.URL)
	own.http = &http.Client{Transport: newTransport(restTimings{idleConn: 3 * time.Second})}
	request(own)
	time.Sleep(120 * time.Millisecond)
	request(own)
	if conns, _ := server.seen(); conns != 1 {
		t.Errorf("this client used %d connections for two requests 120 ms apart, want the one kept open", conns)
	}
	if got := defaultRESTTimings; got.idleConn != 5*time.Minute || got.keepWarm != 30*time.Second {
		t.Errorf("the real timings are %+v, want idle connections kept 5 minutes and a request every 30 s", got)
	}
	if tr := NewClient(cex.Credentials{}).http.Transport.(*http.Transport); tr.IdleConnTimeout != 5*time.Minute || tr.MaxIdleConnsPerHost < 2 || !tr.ForceAttemptHTTP2 {
		t.Errorf("the client's transport: idle %s, %d idle connections per host, HTTP/2 %t; want 5 minutes, room for two orders at once, HTTP/2 attempted", tr.IdleConnTimeout, tr.MaxIdleConnsPerHost, tr.ForceAttemptHTTP2)
	}
}

// The server closes a connection that carried no request for a while (the
// exchange: 400 seconds; here 300 ms). Run opens the connection at once and
// sends the public server-time request at every keep-warm interval, so the
// connection is still open when an order needs it; without Run the next
// request has to open a new one. Run stops with its context.
func TestRunKeepsTheRESTConnectionWarm(t *testing.T) {
	// Without Run: the server has closed the idle connection.
	server := newRESTCountingServer(t, 300*time.Millisecond)
	cold := testClient(server.URL)
	if err := cold.call(context.Background(), http.MethodGet, pathServerTime, nil, nil, false, nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(900 * time.Millisecond)
	if err := cold.call(context.Background(), http.MethodGet, pathInstruments, nil, nil, false, nil); err != nil {
		t.Fatal(err)
	}
	if conns, _ := server.seen(); conns != 2 {
		t.Fatalf("without the keep-warm requests %d connections were used, want 2: the server closes after 300 ms idle", conns)
	}

	server = newRESTCountingServer(t, 300*time.Millisecond)
	warm := testClient(server.URL)
	warm.rest.keepWarm = 30 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { warm.Run(ctx); close(done) }()
	waitFor(t, "the connection opened at the start", func() bool { conns, _ := server.seen(); return conns == 1 })
	time.Sleep(900 * time.Millisecond)
	if err := warm.call(context.Background(), http.MethodGet, pathInstruments, nil, nil, false, nil); err != nil {
		t.Fatal(err)
	}
	conns, paths := server.seen()
	if conns != 1 {
		t.Errorf("%d connections after 900 ms with the keep-warm requests, want the one opened at the start", conns)
	}
	if n := len(paths) - 1; n < 10 || paths[0] != "GET "+pathServerTime || paths[len(paths)-1] != "GET "+pathInstruments {
		t.Errorf("%d keep-warm requests in 900 ms at one per 30 ms, the first %q", n, paths[0])
	}
	if attrs := warm.StatusAttrs(); attrs[4] != "rest_warm_ms" || attrs[5].(float64) <= 0 {
		t.Errorf("the status values %v do not carry the last keep-warm round trip", attrs)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop with its context")
	}
	_, before := server.seen()
	time.Sleep(100 * time.Millisecond)
	if _, after := server.seen(); len(after) != len(before) {
		t.Errorf("%d keep-warm requests after Run stopped", len(after)-len(before))
	}
}

// choiceTimings are short clocks with the connection choice on: three
// pings 5 ms apart, a threshold of 15 ms, at most three connections.
func choiceTimings(c *Client) {
	c.timing.pingSamples, c.timing.pingGap, c.timing.pingThreshold, c.timing.pingAttempts, c.timing.pingWait = 3, 5*time.Millisecond, 15*time.Millisecond, 3, time.Second
}

// loginThenIdle is a private server's script: it answers the login and
// then serves pings until the client leaves.
func loginThenIdle(_ int, read func() (string, bool), send func(string)) {
	if _, ok := read(); !ok {
		return
	}
	send(frameLoginOK)
	for {
		if _, ok := read(); !ok {
			return
		}
	}
}

// The private stream picks its connection by ping. The first connection
// answers a ping after 40 ms - the exchange's slow class - so after three
// pings it is closed without a login and another is dialled; that one
// answers at once and becomes the stream. The status values carry its ping.
func TestPrivateConnectionChosenByPing(t *testing.T) {
	client, server := newPrivateServer(t, loginThenIdle)
	choiceTimings(client)
	server.pingDelay = func(conn int) time.Duration {
		if conn == 1 {
			return 40 * time.Millisecond
		}
		return 0
	}
	var got privateCollector
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := client.SubscribePrivate(ctx, pairSymbols, got.onEvent, got.onDown); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the login on the second connection", func() bool { frames, _, _ := server.seen(); return len(frames) == 2 && len(frames[1]) >= 1 })
	time.Sleep(50 * time.Millisecond)
	frames, _, _ := server.seen()
	if len(frames) != 2 || len(frames[0]) != 0 {
		t.Fatalf("frames per connection = %v; want two connections, the slow first one without a login", frames)
	}
	server.mu.Lock()
	sampled := server.pingsBy[0]
	server.mu.Unlock()
	if sampled != 3 {
		t.Errorf("the rejected connection got %d pings, want the three of its sample", sampled)
	}
	// The pings of a sample are sent apart, each after a silence: 5 ms
	// here, a second against the exchange.
	server.mu.Lock()
	first, third := server.pingAt[0][0], server.pingAt[0][2]
	server.mu.Unlock()
	if gap := third.Sub(first); gap < 2*(40+5)*time.Millisecond {
		t.Errorf("the third ping of the sample came %s after the first; want two round trips of 40 ms and two gaps of 5 ms", gap)
	}
	if gap := defaultWSTimings.pingGap; gap != time.Second {
		t.Errorf("the real gap between the pings of a sample is %s, want 1 s", gap)
	}
	ping, measured := client.privateConn.Load().ping()
	if attrs := client.StatusAttrs(); !measured || ping >= 15*time.Millisecond || attrs[2] != "private_ping_ms" || attrs[3].(float64) != milliseconds(ping) {
		t.Errorf("the chosen connection's ping is %s (%t), the status values %v; want a ping below the threshold, shown in the status", ping, measured, attrs)
	}
	if _, downs := got.snapshot(); downs != 0 {
		t.Errorf("closing the rejected connection was reported as %d losses", downs)
	}
}

// When no connection is under the threshold, three are tried and the one
// with the lowest ping is kept: here the second of 60, 25 and 45 ms. The
// other two never see a login.
func TestConnectionChoiceKeepsTheBestOfThree(t *testing.T) {
	client, server := newPrivateServer(t, loginThenIdle)
	choiceTimings(client)
	delays := []time.Duration{60 * time.Millisecond, 25 * time.Millisecond, 45 * time.Millisecond}
	server.pingDelay = func(conn int) time.Duration { return delays[min(conn, len(delays))-1] }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := client.SubscribePrivate(ctx, pairSymbols, func(cex.PrivateEvent) {}, func(error) {}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a login", func() bool {
		frames, _, _ := server.seen()
		return slices.ContainsFunc(frames, func(f []string) bool { return len(f) > 0 })
	})
	time.Sleep(100 * time.Millisecond)
	frames, _, _ := server.seen()
	if len(frames) != 3 || len(frames[0]) != 0 || len(frames[1]) == 0 || !strings.HasPrefix(frames[1][0], `{"op":"login",`) || len(frames[2]) != 0 {
		t.Fatalf("frames per connection = %v; want three connections tried and the login on the second, the fastest", frames)
	}
	if ping, ok := client.privateConn.Load().ping(); !ok || ping < 25*time.Millisecond || ping >= 45*time.Millisecond {
		t.Errorf("the kept connection's ping is %s (%t), want the second connection's 25 ms", ping, ok)
	}
}

// A connection keeps measuring: the round trip of every heartbeat ping
// joins the ones of the choice, and the median follows.
func TestConnectionPingFollowsTheHeartbeat(t *testing.T) {
	client, server := newPrivateServer(t, loginThenIdle)
	choiceTimings(client)
	var mu sync.Mutex
	slowNow := false
	server.pingDelay = func(int) time.Duration {
		mu.Lock()
		defer mu.Unlock()
		if slowNow {
			return 30 * time.Millisecond
		}
		return 0
	}
	client.timing.pingInterval, client.timing.readTimeout = 60*time.Millisecond, time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := client.SubscribePrivate(ctx, pairSymbols, func(cex.PrivateEvent) {}, func(error) {}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the first ping median", func() bool { _, ok := client.privateConn.Load().ping(); return ok })
	if ping, _ := client.privateConn.Load().ping(); ping >= 15*time.Millisecond {
		t.Fatalf("the chosen connection's ping is %s", ping)
	}
	mu.Lock()
	slowNow = true // the connection turns slow while it lives
	mu.Unlock()
	waitFor(t, "the median to follow the heartbeat pings", func() bool { ping, _ := client.privateConn.Load().ping(); return ping >= 30*time.Millisecond })
}
