package bitget

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/cex/internal/livetest"
)

// The measurements below run against the real exchange and only read: GET
// requests, WebSocket connects, logins, subscriptions and pings. They are
// gated by BITGET_LATENCY_TEST=1; those that log in take the key from the
// file BITGET_ACCOUNT_ENV names. Raw results go to
// experiments/core_step5/latency/: timings and response bodies, never a
// request header.

// latencyGate skips the test unless the measurements are asked for, and
// points the evidence directory at experiments/core_step5/latency.
func latencyGate(t *testing.T) {
	t.Helper()
	if os.Getenv("BITGET_LATENCY_TEST") != "1" {
		t.Skip("BITGET_LATENCY_TEST is not 1: the read-only latency measurement is skipped")
	}
	latencyDir(t)
}

// latencyDir points the evidence directory at
// experiments/core_step5/latency until the test ends.
func latencyDir(t *testing.T) {
	t.Helper()
	saved := livetest.Dir
	livetest.Dir = filepath.Join("..", "..", "..", "experiments", "core_step5", "latency")
	t.Cleanup(func() { livetest.Dir = saved })
	if err := os.MkdirAll(livetest.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

// saveResult writes v as <name>_<UTC time>.json to the evidence directory.
func saveResult(t *testing.T, name string, v any) {
	t.Helper()
	out, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(livetest.Dir, name+"_"+time.Now().UTC().Format("20060102T150405Z")+".json")
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("saved %s", path)
}

// dist is a distribution in milliseconds.
type dist struct {
	N                          int
	Min, Median, P90, P99, Max float64
}

func distOf(ms []float64) dist {
	if len(ms) == 0 {
		return dist{}
	}
	s := slices.Sorted(slices.Values(ms))
	at := func(q float64) float64 { return s[min(len(s)-1, int(q*float64(len(s))))] }
	return dist{N: len(s), Min: s[0], Median: at(0.5), P90: at(0.9), P99: at(0.99), Max: s[len(s)-1]}
}

func (d dist) String() string {
	return fmt.Sprintf("n %d, min %.1f, median %.1f, p90 %.1f, p99 %.1f, max %.1f ms", d.N, d.Min, d.Median, d.P90, d.P99, d.Max)
}

func msOf(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// --- L1: several book connections per leg ---

// liveBookSeconds is how long TestLiveBookConnections listens.
const liveBookSeconds = 300

// Three public connections per leg, each with books5 and books1, for five
// minutes. Every frame is identified by its contract, topic and sequence
// number, so the same frame is compared across the connections: each
// connection's delay (local arrival minus the frame's exchange time, which
// includes the unknown offset between the two clocks, the same for all),
// the merged stream's (the first arrival of each frame, which is what the
// leg book uses), which connection came first how often, and how often and
// by how much the merged stream was ahead of the single best connection on
// the frames that connection delivered.
func TestLiveBookConnections(t *testing.T) {
	latencyGate(t)
	const conns = 3
	symbols := []cex.Symbol{"SOXLUSDT", "SOXSUSDT"}
	type row struct {
		leg, conn int
		topic     string
		seq       uint64
		exchange  time.Time
		received  time.Time
	}
	var mu sync.Mutex
	rows := make([]row, 0, 400_000)
	client := NewClient(cex.Credentials{})
	ctx, cancel := context.WithTimeout(context.Background(), liveBookSeconds*time.Second)
	defer cancel()
	downs := 0
	started := time.Now()
	for leg, symbol := range symbols {
		for conn := 0; conn < conns; conn++ {
			subs := []cex.BookSubscription{{Symbol: symbol, Topic: cex.TopicBooks5}, {Symbol: symbol, Topic: cex.TopicBooks1}}
			err := client.SubscribeBooks(ctx, subs,
				func(ev cex.BookEvent) {
					mu.Lock()
					rows = append(rows, row{leg: leg, conn: conn, topic: ev.Topic, seq: ev.Seq, exchange: ev.ExchangeTime, received: ev.ReceivedAt})
					mu.Unlock()
				},
				func(error) { mu.Lock(); downs++; mu.Unlock() })
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	<-ctx.Done()
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()

	// The raw rows.
	path := filepath.Join(livetest.Dir, "book_connections_"+started.UTC().Format("20060102T150405Z")+".jsonl.gz")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := gzip.NewWriter(file)
	header, _ := json.Marshal(map[string]any{"url": wsPublicURL, "started_ms": started.UnixMilli(), "seconds": liveBookSeconds, "connections_per_leg": conns,
		"topics": []string{cex.TopicBooks5, cex.TopicBooks1}, "columns": []string{"symbol", "connection", "topic", "seq", "exchange_ms", "received_unix_ns"}})
	fmt.Fprintf(zw, "%s\n", header)
	for _, r := range rows {
		fmt.Fprintf(zw, "%s\t%d\t%s\t%d\t%d\t%d\n", symbols[r.leg], r.conn, r.topic, r.seq, r.exchange.UnixMilli(), r.received.UnixNano())
	}
	zw.Close()
	file.Close()
	t.Logf("saved %d frame rows to %s; %d connection losses", len(rows), path, downs)

	// Only the time in which every connection was delivering.
	firstOf := map[[2]int]time.Time{}
	for _, r := range rows {
		if _, seen := firstOf[[2]int{r.leg, r.conn}]; !seen {
			firstOf[[2]int{r.leg, r.conn}] = r.received
		}
	}
	if len(firstOf) != len(symbols)*conns {
		t.Fatalf("%d of %d connections delivered a frame", len(firstOf), len(symbols)*conns)
	}
	var from time.Time
	for _, at := range firstOf {
		if at.After(from) {
			from = at
		}
	}
	from = from.Add(time.Second)

	type key struct {
		topic string
		seq   uint64
	}
	summary := map[string]any{"seconds": liveBookSeconds, "connections_per_leg": conns, "losses": downs,
		"note": "delays are local arrival minus the frame's exchange time in ms and include the clocks' offset; a frame is a (contract, topic, seq)"}
	for leg, symbol := range symbols {
		arrivals := map[key][]time.Time{} // per frame, its arrival on each connection; zero where it did not arrive
		exchange := map[key]time.Time{}
		for _, r := range rows {
			if r.leg != leg || r.received.Before(from) {
				continue
			}
			k := key{r.topic, r.seq}
			if arrivals[k] == nil {
				arrivals[k] = make([]time.Time, conns)
				exchange[k] = r.exchange
			}
			if arrivals[k][r.conn].IsZero() {
				arrivals[k][r.conn] = r.received
			}
		}
		delays := make([][]float64, conns)
		var merged []float64
		mergedByTopic := map[string][]float64{}
		first := make([]int, conns)
		onAll := 0
		for k, at := range arrivals {
			earliest, winner, all := time.Time{}, -1, true
			for conn, a := range at {
				if a.IsZero() {
					all = false
					continue
				}
				delays[conn] = append(delays[conn], msOf(a.Sub(exchange[k])))
				if winner < 0 || a.Before(earliest) {
					earliest, winner = a, conn
				}
			}
			first[winner]++
			merged = append(merged, msOf(earliest.Sub(exchange[k])))
			mergedByTopic[k.topic] = append(mergedByTopic[k.topic], msOf(earliest.Sub(exchange[k])))
			if all {
				onAll++
			}
		}
		// The best single connection: the lowest median delay.
		best, perConn := 0, make([]dist, conns)
		for conn := range delays {
			perConn[conn] = distOf(delays[conn])
			if perConn[conn].Median < perConn[best].Median {
				best = conn
			}
		}
		// Against it: how often another connection brought the frame
		// earlier, and by how much.
		var ahead, gainAll []float64
		for _, at := range arrivals {
			if at[best].IsZero() {
				continue
			}
			earliest := at[best]
			for _, a := range at {
				if !a.IsZero() && a.Before(earliest) {
					earliest = a
				}
			}
			gain := msOf(at[best].Sub(earliest))
			gainAll = append(gainAll, gain)
			if gain > 0 {
				ahead = append(ahead, gain)
			}
		}
		firstPct := make([]float64, conns)
		for conn := range first {
			firstPct[conn] = 100 * float64(first[conn]) / float64(len(arrivals))
		}
		mergedDist, aheadDist, gainDist := distOf(merged), distOf(ahead), distOf(gainAll)
		t.Logf("%s: %d frames (%d on all three connections)", symbol, len(arrivals), onAll)
		for conn := range perConn {
			t.Logf("%s connection %d: delay %s; first to deliver %.1f %% of the frames", symbol, conn, perConn[conn], firstPct[conn])
		}
		t.Logf("%s merged (first arrival of each frame): delay %s; books1 alone: %s; books5 alone: %s", symbol, mergedDist, distOf(mergedByTopic[cex.TopicBooks1]), distOf(mergedByTopic[cex.TopicBooks5]))
		t.Logf("%s merged against the best single connection (%d): ahead on %.1f %% of its frames, by %s; over all its frames the gain is %s",
			symbol, best, 100*float64(len(ahead))/float64(max(len(gainAll), 1)), aheadDist, gainDist)
		summary[string(symbol)] = map[string]any{"frames": len(arrivals), "frames_on_all": onAll, "delay_by_connection": perConn, "first_pct_by_connection": firstPct,
			"delay_merged": mergedDist, "delay_merged_books1": distOf(mergedByTopic[cex.TopicBooks1]), "delay_merged_books5": distOf(mergedByTopic[cex.TopicBooks5]), "best_single_connection": best, "merged_ahead_of_best_pct": 100 * float64(len(ahead)) / float64(max(len(gainAll), 1)),
			"merged_ahead_of_best_when_ahead": aheadDist, "merged_gain_over_best_all_frames": gainDist}
		// The connections do not carry the same frames: each delivers its
		// own subset of the sequence numbers, so the merged stream holds
		// more frames than any one of them and its delay distribution is
		// over a different set. The comparison that holds frame by frame is
		// the gain above.
		if gainDist.Min < 0 {
			t.Errorf("%s: the merged stream was behind the best connection on a frame by %.2f ms", symbol, -gainDist.Min)
		}
	}
	saveResult(t, "book_connections_summary", summary)
}

// --- L2: the REST connection kept warm ---

// The cost of the first request after two minutes without one, with Go's
// default transport (which closes an idle connection after 90 s) and with
// this client's transport and its keep-warm requests. Five clients of each
// kind; each, three times: idle 120 s, then a signed order query for an id
// that does not exist - the path an order takes through the trading gateway
// - and one more straight after as the hot reference. At most two signed
// requests a second, 60 in all; every request is a GET.
func TestLiveRESTWarmth(t *testing.T) {
	latencyGate(t)
	creds := liveCredentials(t)
	const perKind, cycles, idle = 5, 3, 120 * time.Second
	type sample struct {
		Kind    string  `json:"kind"` // default or warm
		Client  int     `json:"client"`
		Cycle   int     `json:"cycle"`
		Request string  `json:"request"` // first_after_idle or hot
		Ms      float64 `json:"ms"`
		Reused  bool    `json:"connection_reused"`
		IdleMs  float64 `json:"connection_idle_ms"`
		Answer  string  `json:"answer"`
		AtMs    int64   `json:"at_unix_ms"`
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type probe struct {
		kind   string
		client *Client
	}
	var probes []probe
	for i := 0; i < perKind; i++ {
		byDefault := NewClient(creds)
		byDefault.http = &http.Client{Timeout: 10 * time.Second, Transport: getOnly{next: http.DefaultTransport.(*http.Transport).Clone()}}
		warm := NewClient(creds)
		warm.http.Transport = getOnly{next: warm.http.Transport}
		go warm.Run(ctx) // opens the connection and keeps it warm
		probes = append(probes, probe{"default", byDefault}, probe{"warm", warm})
	}
	var samples []sample
	query := func(p probe, n, cycle int, request string) {
		var reused bool
		var idleFor time.Duration
		trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused, idleFor = info.Reused, info.IdleTime }}
		began := time.Now()
		err := p.client.call(httptrace.WithClientTrace(ctx, trace), http.MethodGet, pathOrderInfo, url.Values{"clientOid": {"etfp1700000000000-ZZZZBE"}}, nil, true, nil)
		took := time.Since(began)
		answer := "ok"
		var refusal *cex.VenueError
		if errors.As(err, &refusal) {
			answer = refusal.Code
		} else if err != nil {
			answer = "error"
			t.Errorf("%s client %d: %v", p.kind, n, err)
		}
		samples = append(samples, sample{Kind: p.kind, Client: n, Cycle: cycle, Request: request, Ms: msOf(took), Reused: reused, IdleMs: msOf(idleFor), Answer: answer, AtMs: began.UnixMilli()})
	}
	// Every client opens its connection with a public request.
	for _, p := range probes {
		if err := p.client.call(ctx, http.MethodGet, pathServerTime, nil, nil, false, nil); err != nil {
			t.Fatal(err)
		}
	}
	last := make([]time.Time, len(probes))
	for i := range last {
		last[i] = time.Now()
	}
	for cycle := 1; cycle <= cycles; cycle++ {
		for i, p := range probes {
			if wait := time.Until(last[i].Add(idle)); wait > 0 {
				time.Sleep(wait)
			}
			query(p, i/2, cycle, "first_after_idle")
			time.Sleep(500 * time.Millisecond)
			query(p, i/2, cycle, "hot")
			last[i] = time.Now()
			time.Sleep(500 * time.Millisecond)
		}
	}
	saveResult(t, "rest_warmth", samples)
	for _, kind := range []string{"default", "warm"} {
		var first, hot, extra []float64
		reused, n := 0, 0
		hotOf := map[[2]int][]float64{}
		for _, s := range samples {
			if s.Kind == kind && s.Request == "hot" {
				hot = append(hot, s.Ms)
				hotOf[[2]int{s.Client, s.Cycle}] = append(hotOf[[2]int{s.Client, s.Cycle}], s.Ms)
			}
		}
		for _, s := range samples {
			if s.Kind != kind || s.Request != "first_after_idle" {
				continue
			}
			n++
			first = append(first, s.Ms)
			if s.Reused {
				reused++
			}
			extra = append(extra, s.Ms-hotOf[[2]int{s.Client, s.Cycle}][0])
		}
		t.Logf("%s transport: first request after %s idle: %s; the connection was reused in %d of %d; the hot request straight after it: %s; first minus its hot one: %s",
			kind, idle, distOf(first), reused, n, distOf(hot), distOf(extra))
	}
}

// --- L3: the private connection chosen by its ping ---

// The ping of a private connection as dialled against one chosen by the
// client's rule (three pings a second apart, a median above 15 ms dials
// again, at most three connections, the best is kept): ten of each, in
// turn. On every connection: the three pings of the sample, five more a
// second apart as an independent measure, a login, and three pings after
// it. Pings stay under the exchange's ten per five seconds on a connection.
func TestLivePrivateConnectionChoice(t *testing.T) {
	latencyGate(t)
	creds := liveCredentials(t)
	client := NewClient(creds)
	const each = 10
	type result struct {
		Kind         string      `json:"kind"` // dialled or chosen
		Attempts     [][]float64 `json:"attempts_ping_ms"`
		SamplePings  []float64   `json:"sample_ping_ms"`
		LaterPings   []float64   `json:"later_ping_ms"`
		LoginMs      float64     `json:"login_ms"`
		LoginOK      bool        `json:"login_ok"`
		AfterLogin   []float64   `json:"after_login_ping_ms"`
		SampleMedian float64     `json:"sample_median_ms"`
		LaterMedian  float64     `json:"later_median_ms"`
		AtMs         int64       `json:"at_unix_ms"`
	}
	toMs := func(ds []time.Duration) []float64 {
		out := make([]float64, len(ds))
		for i, d := range ds {
			out[i] = msOf(d)
		}
		return out
	}
	ctx := context.Background()
	measure := func(kind string) result {
		t.Helper()
		res := result{Kind: kind, AtMs: time.Now().UnixMilli()}
		w := &wsConn{stream: "probe", url: wsPrivateURL, timing: defaultWSTimings, choose: kind == "chosen",
			onAttempt: func(_ int, pings []time.Duration) { res.Attempts = append(res.Attempts, toMs(pings)) }}
		conn, pings, err := w.dial(ctx)
		if err != nil {
			t.Fatalf("%s: dial: %v", kind, err)
		}
		defer conn.Close()
		if kind == "dialled" { // the same three pings a choice would have taken
			if pings, err = w.samplePings(ctx, conn); err != nil {
				t.Fatalf("%s: pings before the login: %v", kind, err)
			}
		}
		res.SamplePings, res.SampleMedian = toMs(pings), msOf(median(pings))
		later := func(n int) []time.Duration { // n more pings, a second apart
			more := &wsConn{timing: w.timing}
			more.timing.pingSamples = n
			out, err := more.samplePings(ctx, conn)
			if err != nil {
				t.Fatalf("%s: ping: %v", kind, err)
			}
			return out
		}
		more := later(5)
		res.LaterPings, res.LaterMedian = toMs(more), msOf(median(more))
		// The login, then three more pings.
		request, _ := client.loginRequest()
		began := time.Now()
		conn.SetWriteDeadline(began.Add(5 * time.Second))
		if err := conn.WriteMessage(websocket.TextMessage, request); err != nil {
			t.Fatalf("%s: login: %v", kind, err)
		}
		conn.SetReadDeadline(began.Add(5 * time.Second))
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				t.Fatalf("%s: login answer: %v", kind, err)
			}
			var f wsFrame
			if json.Unmarshal(msg, &f) == nil && (f.Event == "login" || f.Event == "error") {
				res.LoginMs, res.LoginOK = msOf(time.Since(began)), f.Event == "login" && f.Code == "0"
				break
			}
		}
		res.AfterLogin = toMs(later(3))
		return res
	}
	var results []result
	for i := 0; i < each; i++ {
		results = append(results, measure("dialled"), measure("chosen"))
	}
	saveResult(t, "private_connection_choice", results)
	for _, kind := range []string{"dialled", "chosen"} {
		var sample, later, after, attempts []float64
		logins := 0
		for _, r := range results {
			if r.Kind != kind {
				continue
			}
			sample, later = append(sample, r.SampleMedian), append(later, r.LaterMedian)
			after = append(after, distOf(r.AfterLogin).Median)
			attempts = append(attempts, float64(max(len(r.Attempts), 1)))
			if r.LoginOK {
				logins++
			}
		}
		sort.Float64s(later)
		t.Logf("%s: per-connection ping medians over %d connections - the three sample pings: %s; five later pings: %s (each: %v); three pings after the login: %s; connections tried per result: %v; %d logins accepted",
			kind, len(sample), distOf(sample), distOf(later), later, distOf(after), attempts, logins)
		if logins != each {
			t.Errorf("%s: %d of %d logins accepted", kind, logins, each)
		}
	}
}

// --- WS: the trade connection, login and ping only ---

// The trade connection of the ws order transport against the real exchange:
// it is chosen by ping, logs in, and is held for twenty seconds with its
// heartbeat. No order is sent: PlaceOrder is not called and no trade frame
// is written. The REST side runs its keep-warm requests behind a transport
// that refuses everything but GET.
func TestLiveTradeConnection(t *testing.T) {
	latencyGate(t)
	client := NewClient(liveCredentials(t))
	client.http.Transport = getOnly{next: client.http.Transport}
	client.UseOrderTransport(cex.OrderTransportWS)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	began := time.Now()
	done := make(chan struct{})
	go func() { client.Run(ctx); close(done) }()
	upAfter := time.Duration(-1)
	var samples []map[string]any
	for ctx.Err() == nil {
		if upAfter < 0 && client.trade.isUp() {
			upAfter = time.Since(began)
		}
		if len(samples) < int(time.Since(began)/(5*time.Second)) {
			attrs := client.StatusAttrs()
			row := map[string]any{"at_ms": msOf(time.Since(began))}
			for i := 0; i+1 < len(attrs); i += 2 {
				row[attrs[i].(string)] = attrs[i+1]
			}
			samples = append(samples, row)
		}
		time.Sleep(20 * time.Millisecond)
	}
	<-done
	ping, pinged := client.trade.ping()
	saveResult(t, "trade_connection", map[string]any{"up_after_ms": msOf(upAfter), "status_every_5s": samples, "note": "login and ping only; no trade frame was sent"})
	t.Logf("trade connection: logged in %s after the start; status values every 5 s: %v", upAfter.Round(time.Millisecond), samples)
	if upAfter < 0 {
		t.Error("the trade connection never logged in")
	}
	if len(samples) == 0 || samples[len(samples)-1]["trade_up"] != true || samples[len(samples)-1]["trade_ping_ms"].(float64) < 0 {
		t.Errorf("the last status values %v do not show the trade connection up with a ping", samples)
	}
	if client.trade.isUp() || pinged {
		t.Errorf("after the stop the trade connection is up %t with ping %s", client.trade.isUp(), ping)
	}
}
