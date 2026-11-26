package booktape

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/pgxpool"

	"etf-pair-arbitrage/dashboard/internal/scan"
)

// midnight is 2026-11-20 00:00 UTC+8, the base of the hand-built recordings.
const midnight = int64(1795104000000)

var testCombos = []scan.Combo{
	{ID: "SOXL-SOXS", Markets: []scan.Market{
		{Venue: "binance", BullSymbol: "SOXLUSDT", BearSymbol: "SOXSUSDT", RefSymbol: "SOXXUSDT"},
		{Venue: "bitget", BullSymbol: "SOXLUSDT", BearSymbol: "SOXSUSDT", RefSymbol: "SOXXUSDT"}}},
	{ID: "TQQQ-SQQQ", Markets: []scan.Market{
		{Venue: "binance", BullSymbol: "TQQQUSDT", BearSymbol: "SQQQUSDT", RefSymbol: "QQQUSDT"},
		{Venue: "bitget", BullSymbol: "TQQQUSDT", BearSymbol: "SQQQUSDT", RefSymbol: "QQQUSDT"}}},
}

// The configured combos give two Bitget feeds per combo, each subscribing
// every contract in one frame: the books (the book topics by the
// contract's price tick, books5 or books50 and then books5, and books1) on
// bookConnections connections, and the other topics on one; and two
// Binance feeds over every contract once, in configuration order. Each
// venue's instrument list is read once.
func TestFeedsOfCombos(t *testing.T) {
	// Bitget lists SOXSUSDT with a 0.001 tick, as it does; the Binance
	// SOXSUSDT tick of 0.001 is made up to show the rule on that venue.
	rest, requests := instrumentServer(t, map[string]string{"bitget:SOXSUSDT": "0.001", "binance:SOXSUSDT": "0.00100"})
	combos := append(slices.Clone(testCombos), scan.Combo{ID: "SOXL-SOXX", Markets: []scan.Market{
		{Venue: "binance", BullSymbol: "SOXLUSDT", BearSymbol: "SOXXUSDT", RefSymbol: "SOXXUSDT"}}})
	log, logs := testLogger()
	feeds, err := feedsOf(context.Background(), combos, endpoints{"wss://b", rest.URL, "wss://f", rest.URL}, log)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range feeds {
		names = append(names, f.name)
	}
	if want := []string{"bitget-soxl-soxs-books", "bitget-soxl-soxs", "bitget-tqqq-sqqq-books", "bitget-tqqq-sqqq", "binance-public", "binance-market"}; !slices.Equal(names, want) {
		t.Fatalf("feeds %v, want %v", names, want)
	}
	if requests["bitget"].Load() != 1 || requests["binance"].Load() != 1 {
		t.Fatalf("instrument requests: bitget %d, binance %d, want one each", requests["bitget"].Load(), requests["binance"].Load())
	}

	soxlBooks, soxl, tqqqBooks := feeds[0], feeds[1], feeds[2]
	args := func(f *feed) []bitgetArg {
		var req struct {
			Op   string      `json:"op"`
			Args []bitgetArg `json:"args"`
		}
		if err := json.Unmarshal(f.request, &req); err != nil || req.Op != "subscribe" {
			t.Fatalf("%s request %s: %v", f.name, f.request, err)
		}
		return req.Args
	}
	var wantBooks, wantOther []bitgetArg
	for _, c := range []struct {
		symbol string
		books  []string
	}{{"SOXLUSDT", []string{"books5"}}, {"SOXSUSDT", []string{"books50", "books5"}}, {"SOXXUSDT", []string{"books5"}}} {
		for _, topic := range append(c.books, "books1") {
			wantBooks = append(wantBooks, bitgetArg{"usdt-futures", topic, c.symbol})
		}
		for _, topic := range []string{"publicTrade", "ticker", "funding-rate"} {
			wantOther = append(wantOther, bitgetArg{"usdt-futures", topic, c.symbol})
		}
	}
	if got := args(soxlBooks); !slices.Equal(got, wantBooks) {
		t.Fatalf("bitget books request %v, want %v", got, wantBooks)
	}
	if got := args(soxl); !slices.Equal(got, wantOther) {
		t.Fatalf("bitget request %v, want %v", got, wantOther)
	}
	if soxlBooks.url != "wss://b" || string(soxlBooks.ping) != "ping" || soxlBooks.conns != bookConnections || bookConnections != 3 ||
		len(soxlBooks.streams) != 7 || soxlBooks.streams[2] != "SOXSUSDT.books50" || soxlBooks.streams[3] != "SOXSUSDT.books5" || soxlBooks.streams[4] != "SOXSUSDT.books1" {
		t.Fatalf("bitget books feed %+v", soxlBooks)
	}
	if soxl.url != "wss://b" || string(soxl.ping) != "ping" || soxl.conns != 1 || len(soxl.streams) != 9 || soxl.streams[0] != "SOXLUSDT.publicTrade" {
		t.Fatalf("bitget feed %+v", soxl)
	}
	if !strings.Contains(string(tqqqBooks.request), `"topic":"books5","symbol":"QQQUSDT"`) || strings.Contains(string(tqqqBooks.request), "books50") {
		t.Fatalf("tqqq request %s", tqqqBooks.request)
	}
	pub, mkt := feeds[4], feeds[5]
	if pub.conns != 1 || mkt.conns != 1 {
		t.Fatalf("binance feeds have one connection each: %d, %d", pub.conns, mkt.conns)
	}
	if pub.url != "wss://f/public/stream?streams=soxlusdt@depth5@100ms/soxlusdt@bookTicker/soxsusdt@depth10@100ms/soxsusdt@bookTicker/"+
		"soxxusdt@depth5@100ms/soxxusdt@bookTicker/tqqqusdt@depth5@100ms/tqqqusdt@bookTicker/"+
		"sqqqusdt@depth5@100ms/sqqqusdt@bookTicker/qqqusdt@depth5@100ms/qqqusdt@bookTicker" || pub.request != nil || pub.ping != nil ||
		!slices.Equal(pub.streams, []string{"SOXLUSDT.depth5", "SOXLUSDT.bookTicker", "SOXSUSDT.depth10", "SOXSUSDT.bookTicker",
			"SOXXUSDT.depth5", "SOXXUSDT.bookTicker", "TQQQUSDT.depth5", "TQQQUSDT.bookTicker", "SQQQUSDT.depth5", "SQQQUSDT.bookTicker",
			"QQQUSDT.depth5", "QQQUSDT.bookTicker"}) {
		t.Fatalf("binance-public %+v", pub)
	}
	if !strings.HasPrefix(mkt.url, "wss://f/market/stream?streams=soxlusdt@aggTrade/soxlusdt@markPrice@1s/") || len(mkt.streams) != 12 ||
		mkt.streams[0] != "SOXLUSDT.aggTrade" || mkt.streams[1] != "SOXLUSDT.markPrice" {
		t.Fatalf("binance-market %+v", mkt)
	}
	if !strings.Contains(logs.String(), "SOXSUSDT=50(tick 0.001)") || !strings.Contains(logs.String(), "SOXSUSDT=10(tick 0.00100)") ||
		!strings.Contains(logs.String(), "SOXLUSDT=5(tick 0.01000)") {
		t.Fatalf("the levels are logged:\n%s", logs)
	}

	// Start errors: an unknown venue, no contract, a contract the venue does
	// not list, an instrument request that fails, a Bitget error code.
	ep := endpoints{"wss://b", rest.URL, "wss://f", rest.URL}
	for name, c := range map[string]struct {
		combos []scan.Combo
		ep     endpoints
	}{
		"unknown venue":     {[]scan.Combo{{ID: "X", Markets: []scan.Market{{Venue: "okx", BullSymbol: "A"}}}}, ep},
		"no contract":       {nil, ep},
		"unlisted":          {[]scan.Combo{{ID: "X", Markets: []scan.Market{{Venue: "binance", BullSymbol: "NOPEUSDT"}}}}, ep},
		"http error":        {testCombos, endpoints{"wss://b", rest.URL + "/fail", "wss://f", rest.URL}},
		"bitget error code": {testCombos, endpoints{"wss://b", rest.URL + "/code", "wss://f", rest.URL}},
	} {
		if _, err := feedsOf(context.Background(), c.combos, c.ep, log); err == nil {
			t.Errorf("%s must be a start error", name)
		}
	}
}

// The depth rule: finer than 0.01 gets the venue's fine depth (Bitget 50,
// the same price span as 5 levels at 0.01), 0.01 and coarser 5, whatever the
// trailing zeros.
func TestLevelsByTick(t *testing.T) {
	for tick, want := range map[string]int{"0.001": 50, "0.00100": 50, "0.0099": 50, "0.01": 5, "0.01000": 5, "0.1": 5, "1": 5} {
		rest, _ := instrumentServer(t, map[string]string{"bitget:SOXLUSDT": tick})
		levels, err := depthOf(context.Background(), "bitget", rest.URL+bitgetInstruments, bitgetTicks, []string{"SOXLUSDT"}, levelsFineBitget,
			slog.New(slog.DiscardHandler))
		if err != nil || levels["SOXLUSDT"] != want {
			t.Errorf("tick %s: levels %v (%v), want %d", tick, levels, err, want)
		}
	}
}

// Every frame kind of both venues is routed to its stream, a control
// frame, Bitget's pong or an error, with the per-second events read and the
// bytes to store.
func TestRouteEveryFrameKind(t *testing.T) {
	five := ladder(52.924, -0.001, 5)
	asks12, bids12 := ladder(52.929, 0.001, 12), ladder(52.924, -0.001, 12)
	books50 := booksFrame("books50", "SOXSUSDT", 1795087336320, bids12, asks12)
	cases := []struct {
		name, venue, payload string
		stream               string
		stored               string // the bytes to store when they differ from the frame
		refused, pong, err   bool
		events               []event
	}{
		{name: "bitget books5", venue: "bitget", payload: string(booksFrame("books5", "SOXLUSDT", 1795087336320, five, lv("52.929/8.95,52.933/11.94"))),
			stream: "SOXLUSDT.books5", events: []event{{exchangeMs: 1795087336320, bids: five, asks: lv("52.929/8.95,52.933/11.94")}}},
		{name: "bitget books5 empty side", venue: "bitget", payload: `{"action":"snapshot","arg":{"topic":"books5","symbol":"A"},"data":[{"a":[],"b":[["1","2"]],"ts":"5"}]}`,
			stream: "A.books5", events: []event{{exchangeMs: 5, bids: lv("1/2"), asks: lv("")}}},
		{name: "bitget books50", venue: "bitget", payload: string(books50), stream: "SOXSUSDT.books50",
			events: []event{{exchangeMs: 1795087336320, bids: bids12, asks: asks12}}},
		{name: "bitget fills newest first", venue: "bitget", payload: string(fills("SOXLUSDT", fill{7, 1003, "11.03"}, fill{6, 998, "10.98"}, fill{8, 998, "10.99"})),
			stream: "SOXLUSDT.publicTrade", events: []event{{exchangeMs: 998, id: 6, price: "10.98"}, {exchangeMs: 998, id: 8, price: "10.99"}, {exchangeMs: 1003, id: 7, price: "11.03"}}},
		{name: "bitget ticker", venue: "bitget", payload: `{"action":"snapshot","arg":{"instType":"usdt-futures","topic":"ticker","symbol":"SOXSUSDT"},"data":[{"markPrice":"52.906","bid1Price":"52.89"}],"ts":1}`,
			stream: "SOXSUSDT.ticker"},
		{name: "bitget funding-rate", venue: "bitget", payload: `{"action":"snapshot","arg":{"instType":"usdt-futures","topic":"funding-rate","symbol":"SOXSUSDT"},"data":[{"fundingRate":"0"}],"ts":1}`,
			stream: "SOXSUSDT.funding-rate"},
		{name: "bitget ack", venue: "bitget", payload: `{"event":"subscribe","arg":{"instType":"usdt-futures","topic":"books5","symbol":"SOXLUSDT"},"connId":"x"}`},
		{name: "bitget error", venue: "bitget", payload: `{"event":"error","code":30001,"msg":"doesn't exist","connId":"x"}`, refused: true},
		{name: "bitget pong", venue: "bitget", payload: "pong", pong: true},
		{name: "bitget not JSON", venue: "bitget", payload: "hello", err: true},
		{name: "bitget other JSON", venue: "bitget", payload: `[1,2]`},
		{name: "bitget bad books5 ts", venue: "bitget", payload: `{"action":"snapshot","arg":{"topic":"books5","symbol":"A"},"data":[{"a":[],"b":[],"ts":"x"}]}`,
			stream: "A.books5", err: true},
		{name: "binance depth5", venue: "binance", payload: string(depthFrame("SOXLUSDT", 5, 1795087397595, lv("54.15000/1707.12,54.14000/2734.17"), lv("54.16000/2144.85"))),
			stream: "SOXLUSDT.depth5", events: []event{{exchangeMs: 1795087397595, bids: lv("54.15000/1707.12,54.14000/2734.17"), asks: lv("54.16000/2144.85")}}},
		{name: "binance depth10", venue: "binance", payload: string(depthFrame("SOXSUSDT", 10, 7, lv("1/2"), lv("3/4"))),
			stream: "SOXSUSDT.depth10", events: []event{{exchangeMs: 7, bids: lv("1/2"), asks: lv("3/4")}}},
		{name: "binance aggTrade", venue: "binance", payload: string(aggTrade("SOXLUSDT", 1795087397382, 92637612, "54.15000")),
			stream: "SOXLUSDT.aggTrade", events: []event{{exchangeMs: 1795087397382, id: 92637612, price: "54.15000"}}},
		{name: "binance markPrice", venue: "binance", payload: `{"stream":"soxlusdt@markPrice@1s","data":{"e":"markPriceUpdate","E":1,"p":"54.1","P":"54.2","i":"54.1","r":"0","T":2}}`,
			stream: "SOXLUSDT.markPrice"},
		{name: "binance control", venue: "binance", payload: `{"result":null,"id":1}`},
		{name: "binance not JSON", venue: "binance", payload: "x", err: true},
	}
	for _, c := range cases {
		r, err := route(c.venue, []byte(c.payload))
		stored := c.stored
		if stored == "" && c.stream != "" {
			stored = c.payload
		}
		if r.stream != c.stream || string(r.payload) != stored || r.refused != c.refused || r.pong != c.pong || (err != nil) != c.err ||
			!reflect.DeepEqual(r.events, c.events) {
			t.Errorf("%s: got %+v err %v", c.name, r, err)
		}
	}
}

// Frames go to <venue>/<UTC+8 day>/<stream>.<first ms>.jsonl.gz, one line
// {"received_ms":..,"payload":<frame verbatim>} each; a record of another
// hour begins a new file; a flush makes the lines readable without the
// trailer; control, unreadable and unsubscribed frames go to events.jsonl.
func TestRawFilesAndRotation(t *testing.T) {
	f, logs := testFeed(t, oneFeed("SOXL-SOXS", []string{"SOXLUSDT"}, map[string]int{"SOXLUSDT": 5}, "wss://b"))
	dir := f.output
	h := midnight - hourMs // 23:00 UTC+8 of 2026-11-19
	a := books5("SOXLUSDT", h+5, "1", "1", "2", "1")
	b := books5("SOXLUSDT", h+hourMs-2, "1.1", "1", "2", "1")
	c := books5("SOXLUSDT", midnight+3, "1.2", "1", "2", "1")
	for _, fr := range []frame{{receivedMs: h + 10, payload: a}, {receivedMs: h + hourMs - 1, payload: b}} {
		if err := f.process(fr); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.flushFiles(); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(dir, "bitget", "20261119", fmt.Sprintf("SOXLUSDT.books5.%d.jsonl.gz", h+10))
	if got := partialLines(t, first); len(got) != 2 || string(got[0]) != fmt.Sprintf(`{"received_ms":%d,"payload":%s}`, h+10, a) {
		t.Fatalf("flushed file before its trailer: %q", got)
	}
	for _, fr := range []frame{
		{receivedMs: midnight + 7, payload: c},
		{receivedMs: midnight + 8, payload: []byte(`{"event":"subscribe","arg":{"topic":"books5"}}`)},
		{receivedMs: midnight + 9, payload: books5("SOXSUSDT", 1, "1", "1", "2", "1")}, // not subscribed by this feed
		{receivedMs: midnight + 10, payload: []byte("hello")},
		{receivedMs: midnight + 11, binary: true, payload: []byte{0xff, 0x00}},
		{receivedMs: midnight + 12, payload: []byte("pong")},
	} {
		if err := f.process(fr); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.closeFiles(); err != nil {
		t.Fatal(err)
	}
	day19, _ := os.ReadDir(filepath.Join(dir, "bitget", "20261119"))
	day20, _ := os.ReadDir(filepath.Join(dir, "bitget", "20261120"))
	if len(day19) != 1 || len(day20) != 2 {
		t.Fatalf("day directories: %v %v", day19, day20)
	}
	second := filepath.Join(dir, "bitget", "20261120", fmt.Sprintf("SOXLUSDT.books5.%d.jsonl.gz", midnight+7))
	if got := fileLines(t, first); len(got) != 2 || string(got[1]) != fmt.Sprintf(`{"received_ms":%d,"payload":%s}`, h+hourMs-1, b) {
		t.Fatalf("first hour %q", got)
	}
	if got := fileLines(t, second); len(got) != 1 || string(got[0]) != fmt.Sprintf(`{"received_ms":%d,"payload":%s}`, midnight+7, c) {
		t.Fatalf("second hour %q", got)
	}
	var kinds []string
	for _, e := range readEvents(t, dir, "bitget") {
		kinds = append(kinds, e.Event)
	}
	if !slices.Equal(kinds, []string{"control", "control", "unreadable", "unreadable"}) {
		t.Fatalf("events %v", kinds)
	}
	if n := strings.Count(logs.String(), "unreadable frame"); n != 1 {
		t.Fatalf("an unreadable frame is logged once per connection, got %d:\n%s", n, logs)
	}
	if _, err := createTape(dir, "bitget", "SOXLUSDT.books5", h+10); err == nil {
		t.Fatal("an existing file must not be opened again")
	}
}

// A fine-tick contract on Bitget records books50 frames whole, and its
// per-second rows carry all 50 levels a side.
func TestBooks50StoredWhole(t *testing.T) {
	f, _ := testFeed(t, oneFeed("SOXL-SOXS", []string{"SOXSUSDT"}, map[string]int{"SOXSUSDT": levelsFineBitget}, "wss://b"))
	bids, asks := ladder(52.924, -0.001, 50), ladder(52.929, 0.001, 50)
	mustBegin(t, f, midnight)
	process(t, f, frame{receivedMs: midnight + 100, payload: booksFrame("books50", "SOXSUSDT", midnight+90, bids, asks)},
		frame{receivedMs: midnight + 3100, payload: booksFrame("books50", "SOXSUSDT", midnight+3090, bids[1:], asks[1:])})
	if err := f.closeFiles(); err != nil {
		t.Fatal(err)
	}
	lines := streamLines(t, f.output, "bitget", "SOXSUSDT.books50")
	want := fmt.Sprintf(`{"received_ms":%d,"payload":%s}`, midnight+100, booksFrame("books50", "SOXSUSDT", midnight+90, bids, asks))
	if len(lines) != 2 || string(lines[0]) != want {
		t.Fatalf("stored %q, want first %s", lines, want)
	}
	rows := f.rows.queued // seconds 1000 to 3000, all of the first frame
	if len(rows) != 3 {
		t.Fatalf("rows %+v", rows)
	}
	for i, r := range rows {
		if r.secondMs != midnight+int64(i+1)*1000 || !reflect.DeepEqual(r.event.bids, bids) || !reflect.DeepEqual(r.event.asks, asks) {
			t.Fatalf("row %d: %+v", i, r)
		}
	}
}

// A fine-tick Bitget contract also records books5, in the files only: its
// frames are stored under SOXSUSDT.books5 and make no row, so the contract's
// book rows keep coming from books50 alone, while a coarse contract's books5
// on the same feed still makes its rows.
func TestFineTickBooks5FilesOnly(t *testing.T) {
	f, _ := testFeed(t, oneFeed("SOXL-SOXS", []string{"SOXLUSDT", "SOXSUSDT"}, map[string]int{"SOXLUSDT": levelsCoarse, "SOXSUSDT": levelsFineBitget}, "wss://b"))
	for stream, tracked := range map[string]bool{"SOXLUSDT.books5": true, "SOXSUSDT.books50": true, "SOXSUSDT.books5": false, "SOXSUSDT.books1": false} {
		if s := f.tapes[stream]; s == nil || (s.tracker != nil) != tracked {
			t.Fatalf("%s: tape %+v, per-second rows %v", stream, s, tracked)
		}
	}
	bids, asks := ladder(52.924, -0.001, 50), ladder(52.929, 0.001, 50)
	// The books5 frames are newer than the first books50 frame and carry other prices.
	soxs5 := []frame{
		{receivedMs: midnight + 1510, payload: booksFrame("books5", "SOXSUSDT", midnight+1500, lv("9.999/1"), lv("10.001/1"))},
		{receivedMs: midnight + 2510, payload: booksFrame("books5", "SOXSUSDT", midnight+2500, lv("9.998/1"), lv("10.002/1"))},
	}
	mustBegin(t, f, midnight)
	process(t, f, frame{receivedMs: midnight + 100, payload: booksFrame("books50", "SOXSUSDT", midnight+90, bids, asks)},
		frame{receivedMs: midnight + 110, payload: books5("SOXLUSDT", midnight+95, "1", "1", "2", "1")})
	process(t, f, soxs5...)
	process(t, f, frame{receivedMs: midnight + 3100, payload: booksFrame("books50", "SOXSUSDT", midnight+3090, bids[1:], asks[1:])},
		frame{receivedMs: midnight + 3110, payload: books5("SOXLUSDT", midnight+3095, "1.1", "1", "2", "1")})
	if err := f.closeFiles(); err != nil {
		t.Fatal(err)
	}

	lines := streamLines(t, f.output, "bitget", "SOXSUSDT.books5")
	if len(lines) != len(soxs5) {
		t.Fatalf("SOXSUSDT.books5 stored %q", lines)
	}
	for i, fr := range soxs5 {
		if want := fmt.Sprintf(`{"received_ms":%d,"payload":%s}`, fr.receivedMs, fr.payload); string(lines[i]) != want {
			t.Fatalf("SOXSUSDT.books5 line %d: %s, want %s", i, lines[i], want)
		}
	}
	var soxs, soxl []row // seconds 1000 to 3000 of each contract, all of its first book frame
	for _, r := range f.rows.queued {
		switch {
		case r.kind != kindBook:
			t.Fatalf("row %+v", r)
		case r.symbol == "SOXSUSDT":
			soxs = append(soxs, r)
		case r.symbol == "SOXLUSDT":
			soxl = append(soxl, r)
		}
	}
	if len(soxs) != 3 || len(soxl) != 3 {
		t.Fatalf("rows: SOXS %+v, SOXL %+v", soxs, soxl)
	}
	for i := range 3 {
		if r := soxs[i]; r.secondMs != midnight+int64(i+1)*1000 || r.event.exchangeMs != midnight+90 ||
			!reflect.DeepEqual(r.event.bids, bids) || !reflect.DeepEqual(r.event.asks, asks) {
			t.Fatalf("SOXS row %d is not of books50: %+v", i, r)
		}
		if r := soxl[i]; r.secondMs != midnight+int64(i+1)*1000 || r.event.exchangeMs != midnight+95 || r.event.bids[0][0] != "1" {
			t.Fatalf("SOXL row %d: %+v", i, r)
		}
	}
}

// The per-second rules on the Bitget recording of research/test_booktape.py
// (Seconds.test_bitget_rules), with the rows seconds() derives from it: the
// newest event stamped before T, ties by the larger trade id, a frame's
// fills applied oldest first, a late event updating the value but no
// emitted row, nothing carried across a disconnection; a book row holds the
// frame's levels.
func TestPerSecondBitgetRules(t *testing.T) {
	f, _ := testFeed(t, &feed{name: "bitget-soxl-soxs", venue: "bitget", streams: []string{"SOXLUSDT.books5", "SOXLUSDT.publicTrade"}})
	m := midnight
	bk := func(at, ts int64, bids, asks string) frame {
		return frame{receivedMs: m + at, payload: booksFrame("books5", "SOXLUSDT", m+ts, lv(bids), lv(asks))}
	}
	tr := func(at int64, fs ...fill) frame {
		for i := range fs {
			fs[i].t += m
		}
		return frame{receivedMs: m + at, payload: fills("SOXLUSDT", fs...)}
	}
	mustBegin(t, f, m+9500)
	process(t, f,
		bk(10050, 10040, "1.0/1", "2.0/2"),
		tr(10500, fill{5, 10400, "10.4"}),
		bk(11005, 10999, "1.1/1", "2.1/2"),
		bk(11010, 11000, "1.2/7,1.19/3", "2.2/8,2.21/4,2.22/5"),
		tr(11020, fill{7, 11003, "11.03"}, fill{6, 10998, "10.98"}),
		tr(12010, fill{9, 11500, "11.5b"}, fill{8, 11500, "11.5a"}),
		bk(14500, 11500, "1.3/1", "2.3/2"))
	if err := f.endConnection(m+16500, "read: timeout"); err != nil {
		t.Fatal(err)
	}
	mustBegin(t, f, m+18000)
	process(t, f, bk(18200, 18150, "1.4/1", "2.4/2"), bk(19100, 19050, "1.5/1", "2.5/2"), bk(23000, 22990, "1.6/1", "2.6/2"))
	want := map[int64]string{
		11000: "10.98@10998 | 1.1/1 2.1/2@10999",
		12000: "11.5b@11500 | 1.2/7,1.19/3 2.2/8,2.21/4,2.22/5@11000",
		19000: "- | 1.4/1 2.4/2@18150",
	}
	for s := int64(13000); s <= 16000; s += 1000 {
		want[s] = "11.5b@11500 | 1.3/1 2.3/2@11500"
	}
	for s := int64(20000); s <= 22000; s += 1000 {
		want[s] = "- | 1.5/1 2.5/2@19050"
	}
	assertSeconds(t, f.rows.queued, m, want)
	if f.late != 1 {
		t.Fatalf("late events %d, want 1 (the book event stamped 11500 after row 12000)", f.late)
	}
}

// "The clock passes T + 2 s" is strictly greater, on receive times
// (Seconds.test_clock_rule_is_strictly_after_two_seconds); a kind known but
// without an event stamped before T leaves T without its columns.
func TestPerSecondClockRule(t *testing.T) {
	for _, c := range []struct {
		received int64
		want     string
	}{{4000, "- | 1.1/1 2/1@1500"}, {4001, "- | 1.0/1 2/1@1040"}} {
		f, _ := testFeed(t, &feed{name: "f", venue: "bitget", streams: []string{"SOXLUSDT.books5"}})
		mustBegin(t, f, midnight)
		process(t, f, frame{receivedMs: midnight + 1050, payload: books5("SOXLUSDT", midnight+1040, "1.0", "1", "2", "1")},
			frame{receivedMs: midnight + c.received, payload: books5("SOXLUSDT", midnight+1500, "1.1", "1", "2", "1")},
			frame{receivedMs: midnight + 9000, payload: books5("SOXLUSDT", midnight+8990, "1.2", "1", "2", "1")})
		got := secondsOf(t, f.rows.queued, midnight)
		if got[2000] != c.want {
			t.Errorf("received %d: row 2000 is %q, want %q", c.received, got[2000], c.want)
		}
	}
	// The first frame (received 500, stamped 1200 by a venue clock ahead)
	// makes the kind known from second 1000, but no event is stamped before it.
	f, _ := testFeed(t, &feed{name: "f", venue: "bitget", streams: []string{"SOXLUSDT.books5"}})
	mustBegin(t, f, midnight)
	process(t, f, frame{receivedMs: midnight + 500, payload: books5("SOXLUSDT", midnight+1200, "1.0", "1", "2", "1")},
		frame{receivedMs: midnight + 5000, payload: books5("SOXLUSDT", midnight+4990, "1.1", "1", "2", "1")})
	assertSeconds(t, f.rows.queued, midnight, map[int64]string{
		2000: "- | 1.0/1 2/1@1200", 3000: "- | 1.0/1 2/1@1200", 4000: "- | 1.0/1 2/1@1200"})
}

// Binance's book and trade kinds live on two feeds and end with their own
// connections (Seconds.test_binance_feeds_end_separately).
func TestPerSecondBinanceFeedsEndSeparately(t *testing.T) {
	pub := &feed{name: "binance-public", venue: "binance", streams: []string{"SOXLUSDT.depth5"}}
	mkt := &feed{name: "binance-market", venue: "binance", streams: []string{"SOXLUSDT.aggTrade", "SOXLUSDT.markPrice"}}
	newManager([]*feed{pub, mkt}, nil, t.TempDir(), slog.New(slog.DiscardHandler))
	m := midnight
	bt := func(at, ts int64, bid, ask string) frame {
		return frame{receivedMs: m + at, payload: depthFrame("SOXLUSDT", 5, m+ts, lv(bid+"/5"), lv(ask+"/6"))}
	}
	ag := func(at, ts, id int64, price string) frame {
		return frame{receivedMs: m + at, payload: aggTrade("SOXLUSDT", m+ts, id, price)}
	}
	mustBegin(t, pub, m+9500)
	mustBegin(t, mkt, m+9600)
	process(t, pub, bt(10010, 10005, "1.10", "1.20"))
	process(t, mkt, ag(10100, 10050, 100, "1.15"))
	process(t, pub, bt(12100, 12090, "1.11", "1.21"))
	process(t, mkt, ag(12500, 12400, 101, "1.16"))
	if err := mkt.endConnection(m+13500, "read: timeout"); err != nil {
		t.Fatal(err)
	}
	mustBegin(t, mkt, m+14000)
	process(t, mkt, ag(15100, 15000, 150, "1.17"),
		frame{receivedMs: m + 19000, payload: []byte(`{"stream":"soxlusdt@markPrice@1s","data":{"E":1795104018685,"p":"1"}}`)})
	process(t, pub, bt(19500, 19490, "1.12", "1.22"))
	got := secondsOf(t, pub.rows.queued, m)
	want := map[int64]string{
		11000: "1.15@10050 | 1.10/5 1.20/6@10005",
		12000: "1.15@10050 | 1.10/5 1.20/6@10005",
		13000: "1.16@12400 | 1.11/5 1.21/6@12090",
		14000: "- | 1.11/5 1.21/6@12090", // the market connection ended at 13500
		15000: "- | 1.11/5 1.21/6@12090", // trades known again from 15100
		16000: "1.17@15000 | 1.11/5 1.21/6@12090",
	}
	for s, w := range want {
		if got[s] != w {
			t.Errorf("row %d: got %q, want %q", s, got[s], w)
		}
	}
}

// A Bitget feed against a local venue: it subscribes after each dial,
// answers with text pings and sends nothing else, routes every frame kind,
// stores books50 frames whole, dials again after the venue drops it, losing
// and repeating no frame, and closes cleanly with complete gzip files.
func TestBitgetFeedReconnectsAndShutsDown(t *testing.T) {
	shortTimings(t)
	got := make(chan string, 64)
	now := time.Now().UnixMilli()
	sent1 := [][]byte{books5("SOXLUSDT", now, "1", "1", "2", "1"), fills("SOXLUSDT", fill{2, now, "1.5"}, fill{1, now, "1.4"}),
		booksFrame("books50", "SOXSUSDT", now, ladder(52.924, -0.001, 50), ladder(52.929, 0.001, 50)),
		booksFrame("books5", "SOXSUSDT", now, ladder(52.924, -0.001, 5), ladder(52.929, 0.001, 5)),
		[]byte(`{"action":"snapshot","arg":{"instType":"usdt-futures","topic":"ticker","symbol":"SOXLUSDT"},"data":[{"markPrice":"1.5"}],"ts":1}`),
		[]byte(`{"action":"snapshot","arg":{"instType":"usdt-futures","topic":"funding-rate","symbol":"SOXLUSDT"},"data":[{"fundingRate":"0"}],"ts":1}`)}
	sent2 := [][]byte{books5("SOXLUSDT", now+1, "1.1", "1", "2", "1"), fills("SOXLUSDT", fill{3, now + 1, "1.6"})}
	srv := venueServer(t,
		func(c *websocket.Conn, r *http.Request) {
			_, sub, _ := c.ReadMessage()
			got <- "subscribe " + string(sub)
			c.WriteMessage(websocket.TextMessage, []byte(`{"event":"subscribe","arg":{"instType":"usdt-futures","topic":"books5","symbol":"SOXLUSDT"},"connId":"a"}`))
			for _, p := range sent1 {
				c.WriteMessage(websocket.TextMessage, p)
			}
			c.WriteMessage(websocket.TextMessage, []byte("hello"))
			c.WriteMessage(websocket.BinaryMessage, []byte{1, 2})
			c.WriteMessage(websocket.TextMessage, []byte(`{"event":"error","code":30001,"msg":"x doesn't exist","connId":"a"}`))
			answerPings(c, got, 1) // one text ping, answered, then the venue drops the connection
			c.NetConn().Close()
		},
		func(c *websocket.Conn, r *http.Request) {
			_, sub, _ := c.ReadMessage()
			got <- "subscribe " + string(sub)
			for _, p := range sent2 {
				c.WriteMessage(websocket.TextMessage, p)
			}
			answerPings(c, got, -1)
			got <- "closed"
		})
	f, logs := testFeed(t, oneFeed("SOXL-SOXS", []string{"SOXLUSDT", "SOXSUSDT"}, map[string]int{"SOXLUSDT": 5, "SOXSUSDT": levelsFineBitget}, wsURL(srv)))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.run(ctx) }()

	expect(t, got, "subscribe "+string(f.request))
	expect(t, got, "ping")
	expect(t, got, "subscribe "+string(f.request))
	expect(t, got, "ping") // the heartbeat goes on after the reconnection and nothing else is sent
	expect(t, got, "ping")
	cancel()
	expect(t, got, "closed")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after the context ended")
	}

	// Every frame once, in order, across the reconnection, as route gives it
	// to store; complete gzip files.
	for _, stream := range f.streams {
		var want []string
		for _, p := range append(slices.Clone(sent1), sent2...) {
			if r, _ := route("bitget", p); r.stream == stream {
				want = append(want, string(r.payload))
			}
		}
		var payloads []string
		for _, l := range streamLines(t, f.output, "bitget", stream) {
			payloads = append(payloads, string(l[bytes.Index(l, []byte(`,"payload":`))+11:len(l)-1]))
		}
		if !slices.Equal(payloads, want) {
			t.Errorf("%s: recorded %q, want %q", stream, payloads, want)
		}
	}
	if l := streamLines(t, f.output, "bitget", "SOXSUSDT.books50"); len(l) != 1 || bytes.Count(l[0], []byte(`["`)) != 100 {
		t.Errorf("books50 is stored with 50 levels a side: %s", l)
	}
	events := readEvents(t, f.output, "bitget")
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e.Event)
	}
	wantKinds := []string{"connected", "control", "unreadable", "unreadable", "control", "disconnected", "connected", "disconnected"}
	if !slices.Equal(kinds, wantKinds) {
		t.Fatalf("events %v", kinds)
	}
	if string(events[0].Request) != string(f.request) || events[0].URL != f.url || events[5].Frames != len(sent1) {
		t.Fatalf("connected %+v, disconnected %+v", events[0], events[5])
	}
	if last := events[len(events)-1]; last.Error != "shutdown" || last.Frames != len(sent2) {
		t.Fatalf("last event %+v", last)
	}
	text := logs.String()
	for _, want := range []string{"refused a request", "unreadable frame", "booktape: disconnected", "redial_in=20ms"} {
		if !strings.Contains(text, want) {
			t.Errorf("log lacks %q:\n%s", want, text)
		}
	}
}

// A feed of several connections records each sequence number of a book
// stream once, from the copy that arrives first: a later copy, or an older
// sequence number, is dropped and counted, and the per-second rows see only
// the recorded frames. Once the feed's last connection has ended the
// numbers start afresh. A feed of one connection records every frame.
func TestBookFeedRecordsTheFirstCopy(t *testing.T) {
	books := func(string) []string { return []string{"books5", "books1"} }
	a := books5("SOXLUSDT", midnight+700, "1", "1", "2", "1")
	older := books5("SOXLUSDT", midnight+70, "0.9", "1", "2", "1")
	b := books5("SOXLUSDT", midnight+1400, "1.1", "1", "2", "1")
	top := booksFrame("books1", "SOXLUSDT", midnight+700, lv("1/1"), lv("2/1"))
	in := []frame{{conn: 0, receivedMs: midnight + 710, payload: a}, {conn: 1, receivedMs: midnight + 712, payload: a},
		{conn: 2, receivedMs: midnight + 713, payload: top}, {conn: 0, receivedMs: midnight + 714, payload: top},
		{conn: 2, receivedMs: midnight + 720, payload: older}, {conn: 2, receivedMs: midnight + 1405, payload: b},
		{conn: 0, receivedMs: midnight + 1430, payload: b}}
	received := func(f *feed, stream string) (out []int64) {
		for _, l := range streamLines(t, f.output, "bitget", stream) {
			ms, err := strconv.ParseInt(string(l[15:bytes.IndexByte(l, ',')]), 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, ms-midnight)
		}
		return out
	}

	group, logs := testFeed(t, bitgetFeed("bitget-soxl-soxs-books", []string{"SOXLUSDT"}, books, "wss://b", 3))
	mustBegin(t, group, midnight+600)
	process(t, group, in...)
	if group.copies != 4 || group.links[0].first != 1 || group.links[1].first != 0 || group.links[2].first != 2 {
		t.Errorf("copies %d, first by connection %d/%d/%d; want 4, 1/0/2", group.copies, group.links[0].first, group.links[1].first, group.links[2].first)
	}
	for _, r := range group.rows.queued {
		if len(r.event.bids) > 0 && r.event.bids[0][0] == "0.9" {
			t.Errorf("a row holds the dropped older frame: %+v", r)
		}
	}
	group.status()
	if !strings.Contains(logs.String(), "copies_dropped=4") || !strings.Contains(logs.String(), "first_by_connection=1/0/2") {
		t.Errorf("the status line shows the copies and the first arrivals:\n%s", logs)
	}
	if err := group.endConnection(midnight+2000, "x"); err != nil { // the last connection ended
		t.Fatal(err)
	}
	mustBegin(t, group, midnight+2100)
	process(t, group, frame{conn: 1, receivedMs: midnight + 2110, payload: b})
	if err := group.closeFiles(); err != nil {
		t.Fatal(err)
	}
	if got := received(group, "SOXLUSDT.books5"); !slices.Equal(got, []int64{710, 1405, 2110}) {
		t.Errorf("books5 recorded at %v, want 710, 1405 and, after the feed came back, 2110", got)
	}
	if got := received(group, "SOXLUSDT.books1"); !slices.Equal(got, []int64{713}) {
		t.Errorf("books1 recorded at %v, want 713", got)
	}

	single, _ := testFeed(t, bitgetFeed("bitget-soxl-soxs", []string{"SOXLUSDT"}, books, "wss://b", 1))
	mustBegin(t, single, midnight+600)
	for _, fr := range in {
		fr.conn = 0
		process(t, single, fr)
	}
	if err := single.closeFiles(); err != nil {
		t.Fatal(err)
	}
	if got := received(single, "SOXLUSDT.books5"); !slices.Equal(got, []int64{710, 712, 720, 1405, 1430}) || single.copies != 0 {
		t.Errorf("one connection records every frame: %v, %d copies dropped", got, single.copies)
	}
}

// The feed's disconnected event is stamped with the latest end of its
// connections, whichever end is handled last: never before a frame it
// recorded.
func TestBookFeedEndsAtItsLatestConnectionEnd(t *testing.T) {
	books := func(string) []string { return []string{"books5"} }
	f, _ := testFeed(t, bitgetFeed("bitget-soxl-soxs-books", []string{"SOXLUSDT"}, books, "wss://b", 2))
	for _, x := range []input{{conn: 0, up: true, atMs: midnight + 100}, {conn: 1, up: true, atMs: midnight + 101},
		{conn: 1, fr: frame{conn: 1, receivedMs: midnight + 201, payload: books5("SOXLUSDT", midnight+190, "1", "1", "2", "1")}},
		{conn: 1, down: true, atMs: midnight + 205, cause: "read: reset"}, // handled first, though it ended last
		{conn: 0, down: true, atMs: midnight + 200, cause: "read: reset"}} {
		if err := f.apply(x); err != nil {
			t.Fatal(err)
		}
	}
	events := readEvents(t, f.output, "bitget")
	if last := events[len(events)-1]; last.Event != "disconnected" || last.ReceivedMs != midnight+205 {
		t.Fatalf("the feed ended %+v, want disconnected at +205", last)
	}
}

// The connections of a book feed dial, subscribe and redial each on its
// own; the feed is connected from its first connection's start to its last
// one's end (connected, disconnected), each connection's start and end are
// noted with its number (link_connected, link_disconnected), and one
// connection dropping while the others are up loses nothing. Every
// sequence number is recorded once.
func TestBookFeedConnections(t *testing.T) {
	shortTimings(t)
	now := time.Now().UnixMilli()
	a, b, c := books5("SOXLUSDT", now, "1", "1", "2", "1"), books5("SOXLUSDT", now+7, "1.1", "1", "2", "1"), books5("SOXLUSDT", now+14, "1.2", "1", "2", "1")
	sent, release := make(chan string, 8), make(chan struct{})
	serve := func(c *websocket.Conn, frames ...[]byte) {
		c.ReadMessage() // the subscribe frame
		for _, p := range frames {
			c.WriteMessage(websocket.TextMessage, p)
		}
	}
	pong := func(c *websocket.Conn) {
		for {
			_, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			if string(msg) == "ping" {
				c.WriteMessage(websocket.TextMessage, []byte("pong"))
			}
		}
	}
	kept := func(c *websocket.Conn, r *http.Request) { serve(c, a, b); sent <- "kept"; pong(c) }
	srv := venueServer(t,
		func(c *websocket.Conn, r *http.Request) {
			serve(c, a, b)
			sent <- "dropped"
			<-release
			c.NetConn().Close()
		},
		kept, kept,
		func(conn *websocket.Conn, r *http.Request) { serve(conn, a, b, c); sent <- "redialled"; pong(conn) })
	books := func(string) []string { return []string{"books5", "books1"} }
	f, _ := testFeed(t, bitgetFeed("bitget-soxl-soxs-books", []string{"SOXLUSDT"}, books, wsURL(srv), 3))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.run(ctx) }()
	for range 3 { // all three connected and delivered a and b
		select {
		case <-sent:
		case <-time.After(5 * time.Second):
			t.Fatal("the three connections were not all served")
		}
	}
	close(release)
	expect(t, sent, "redialled")
	path := ""
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		found, _ := filepath.Glob(filepath.Join(f.output, "bitget", "*", "SOXLUSDT.books5.*.jsonl.gz"))
		if len(found) == 1 {
			path = found[0]
			if len(partialLines(t, path)) == 3 {
				break
			}
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var payloads []string
	for _, l := range streamLines(t, f.output, "bitget", "SOXLUSDT.books5") {
		payloads = append(payloads, string(l[bytes.Index(l, []byte(`,"payload":`))+11:len(l)-1]))
	}
	if want := []string{string(a), string(b), string(c)}; !slices.Equal(payloads, want) {
		t.Errorf("recorded %q, want each frame once %q", payloads, want)
	}
	var kinds []string
	for _, e := range readEvents(t, f.output, "bitget") {
		kinds = append(kinds, e.Event)
	}
	want := []string{"connected", "link_connected", "link_connected", "link_connected", "link_disconnected", "link_connected",
		"link_disconnected", "link_disconnected", "link_disconnected", "disconnected"}
	if !slices.Equal(kinds, want) {
		t.Fatalf("events %v, want %v", kinds, want)
	}
	events := readEvents(t, f.output, "bitget")
	if string(events[0].Request) != string(f.request) || events[4].Frames != 2 || events[4].Error == "" || events[4].Conn != events[5].Conn {
		t.Errorf("connected %+v; the dropped connection %+v and its redial %+v", events[0], events[4], events[5])
	}
	if last := events[len(events)-1]; last.Error != "shutdown" || last.Frames != 3 {
		t.Errorf("the feed ends at shutdown with the 3 frames it recorded: %+v", last)
	}
}

// A Binance feed sends protocol pings only (never text), answers the
// venue's pings, and stays connected while pongs arrive; a venue that goes
// silent is dropped after the read timeout, and one that never delivered a
// frame is dialled again only after redialIdle.
func TestBinanceHeartbeatsAndSilentServer(t *testing.T) {
	shortTimings(t)
	var pings, texts, pongs atomic.Int32
	var silent atomic.Bool
	answer := make(chan struct{}, 1)
	srv := venueServer(t,
		func(c *websocket.Conn, r *http.Request) {
			if r.URL.Path != "/public/stream" || r.URL.Query().Get("streams") != "soxlusdt@depth5@100ms" {
				return
			}
			c.SetPingHandler(func(data string) error {
				if silent.Load() {
					return nil
				}
				pings.Add(1)
				return c.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(time.Second))
			})
			c.SetPongHandler(func(string) error { pongs.Add(1); answer <- struct{}{}; return nil })
			c.WriteMessage(websocket.TextMessage, depthFrame("SOXLUSDT", 5, time.Now().UnixMilli(), lv("1/1"), lv("2/1")))
			c.WriteControl(websocket.PingMessage, []byte("1795087435681"), time.Now().Add(time.Second))
			time.AfterFunc(3*readTimeout, func() { silent.Store(true) }) // pongs for 3 read timeouts, then none
			readAll(c, &texts)
		},
		func(c *websocket.Conn, r *http.Request) { // silent from the start
			c.SetPingHandler(func(string) error { return nil })
			readAll(c, &texts)
		})
	book := func(string) [][2]string { return [][2]string{binanceBook(5)} }
	f, _ := testFeed(t, binanceFeed("binance-public", wsURL(srv)+"/public/stream", []string{"SOXLUSDT"}, book))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.run(ctx) }()
	select {
	case <-answer:
	case <-time.After(5 * time.Second):
		t.Fatal("the feed did not answer the venue's ping")
	}
	for deadline := time.Now().Add(10 * time.Second); (countEvents(f.output, "binance", "disconnected") < 3 ||
		countEvents(f.output, "binance", "connected") < 4) && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	conns := connections(t, f.output, "binance")
	if len(conns) < 4 {
		t.Fatalf("connections %v", conns)
	}
	first, second, third := conns[0], conns[1], conns[2]
	if first.duration() < 3*readTimeout || !strings.Contains(first.err, "timeout") {
		t.Errorf("first connection lasted %v (%s): pongs must keep it alive for 3 read timeouts, then the deadline ends it", first.duration(), first.err)
	}
	if d := second.duration(); d < readTimeout || d > readTimeout+time.Second || !strings.Contains(second.err, "timeout") {
		t.Errorf("silent connection lasted %v (%s), want the read timeout %v", d, second.err, readTimeout)
	}
	if d := time.Duration(second.start-first.end) * time.Millisecond; d < redialAfter || d >= redialIdle {
		t.Errorf("after a connection with data the feed dialled again after %v, want %v", d, redialAfter)
	}
	if d := time.Duration(third.start-second.end) * time.Millisecond; d < redialIdle {
		t.Errorf("after a connection without data the feed dialled again after %v, want at least %v", d, redialIdle)
	}
	if last := conns[len(conns)-1]; last.err != "shutdown" {
		t.Errorf("last connection ended with %q, want shutdown", last.err)
	}
	if pings.Load() < 3 || texts.Load() != 0 || pongs.Load() != 1 {
		t.Errorf("venue saw %d pings, %d text frames, %d pongs", pings.Load(), texts.Load(), pongs.Load())
	}
}

// The table has the columns of the CSV format in its order, the book
// columns as numeric arrays. The writer upserts each kind's columns only,
// writes a side's levels best first ({} for an empty side, NULL for an
// empty field), keeps the rows of a failed write for the next attempt,
// drops those an hour old, and stores the kept rows once the database
// accepts them again.
func TestWriterAgainstDatabase(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	log, logs := testLogger()
	w := &writer{pool: pool, log: log, pending: map[rowKey]row{}}
	now := time.Now()
	s := now.UnixMilli() / 1000 * 1000
	bookRow := func(sym string, sec int64, bids, asks string) row {
		return row{"bitget", sym, sec, kindBook, event{exchangeMs: sec - 5, bids: lv(bids), asks: lv(asks)}}
	}
	tradeRow := func(sym string, sec int64, price string) row {
		return row{"bitget", sym, sec, kindTrade, event{exchangeMs: sec - 7, id: 99, price: price}}
	}
	w.queue([]row{bookRow("SOXLUSDT", s, "54.15000/2477.84,54.14000/1000", "54.16000/2145.27"), tradeRow("SOXLUSDT", s, "54.15"),
		bookRow("SOXLUSDT", s+1000, "", "54.2/1")})
	if err := w.write(ctx, now); err != nil {
		t.Fatal(err)
	}
	var columns string
	if err := pool.QueryRow(ctx, `SELECT string_agg(attname || ' ' || format_type(atttypid, atttypmod), ', ' ORDER BY attnum)
		FROM pg_attribute WHERE attrelid = 'etfpair.perp_second'::regclass AND attnum > 0 AND NOT attisdropped`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if want := "venue text, symbol text, second_ms bigint, price numeric, trade_ms bigint, bid_px numeric[], bid_sz numeric[], " +
		"ask_px numeric[], ask_sz numeric[], book_ms bigint"; columns != want {
		t.Fatalf("columns %s, want %s", columns, want)
	}
	want := map[int64]string{
		s:        fmt.Sprintf("54.15 %d {54.15000,54.14000} {2477.84,1000} {54.16000} {2145.27} %d", s-7, s-5),
		s + 1000: fmt.Sprintf("- - {} {} {54.2} {1} %d", s+995),
	}
	assertTable(t, pool, "SOXLUSDT", want)

	// Each upsert sets its own columns only; the schema applies again.
	w.schemaApplied = false
	w.queue([]row{tradeRow("SOXLUSDT", s+1000, "54.3"), bookRow("SOXLUSDT", s, "54.14/1", "54.17/2,54.18/")})
	if err := w.write(ctx, now); err != nil {
		t.Fatal(err)
	}
	want[s] = fmt.Sprintf("54.15 %d {54.14} {1} {54.17,54.18} {2,NULL} %d", s-7, s-5)
	want[s+1000] = fmt.Sprintf("54.3 %d {} {} {54.2} {1} %d", s+993, s+995)
	assertTable(t, pool, "SOXLUSDT", want)

	// A failed write keeps its rows; rows an hour old are dropped.
	if _, err := pool.Exec(ctx, `ALTER TABLE etfpair.perp_second ADD CONSTRAINT booktape_test_fail CHECK (symbol <> 'FAILUSDT')`); err != nil {
		t.Fatal(err)
	}
	w.queue([]row{tradeRow("FAILUSDT", s+2000, "1"), tradeRow("SOXLUSDT", s+2000, "54.4"), tradeRow("SOXLUSDT", s-holdRowsMs-1000, "50")})
	if w.write(ctx, now) == nil || w.write(ctx, now) == nil {
		t.Fatal("the writes must fail")
	}
	if len(w.pending) != 2 || w.dropped != 1 {
		t.Fatalf("pending %d, dropped %d after the failed writes", len(w.pending), w.dropped)
	}
	assertTable(t, pool, "SOXLUSDT", want)
	if n := strings.Count(logs.String(), "database write failed"); n != 1 || !strings.Contains(logs.String(), "rows dropped after an hour") {
		t.Fatalf("a failing database is logged once, dropped rows always:\n%s", logs)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE etfpair.perp_second DROP CONSTRAINT booktape_test_fail`); err != nil {
		t.Fatal(err)
	}
	if err := w.write(ctx, now); err != nil {
		t.Fatal(err)
	}
	want[s+2000] = fmt.Sprintf("54.4 %d - - - - -", s+1993)
	assertTable(t, pool, "SOXLUSDT", want)
	assertTable(t, pool, "FAILUSDT", map[int64]string{s + 2000: fmt.Sprintf("1 %d - - - - -", s+1993)})
	if len(w.pending) != 0 || !strings.Contains(logs.String(), "database writes resumed") {
		t.Fatalf("pending %d after the database came back:\n%s", len(w.pending), logs)
	}
}

// Run against local venues and a throwaway database: the levels follow the
// instrument lists, every feed records, the rows reach the table with as
// many levels a side as the frames carry, and the seconds made final by the
// shutdown are written by the last write.
func TestManagerRunWritesRows(t *testing.T) {
	pool := testPool(t)
	shortTimings(t)
	bids12, asks12 := ladder(52.924, -0.001, 12), ladder(52.929, 0.001, 12)
	stream := func(c *websocket.Conn, r *http.Request) {
		if r.URL.Path == "/bitget" {
			c.ReadMessage() // the subscribe frame
		}
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for i := int64(1); ; i++ {
			<-tick.C
			now := time.Now().UnixMilli()
			var frames [][]byte
			switch r.URL.Path {
			case "/bitget":
				// SOXS books5, newer and with other levels, must not reach the rows.
				frames = [][]byte{books5("SOXLUSDT", now-3, "1.00", "1", "1.01", "2"), booksFrame("books50", "SOXSUSDT", now-3, bids12, asks12),
					booksFrame("books5", "SOXSUSDT", now-2, ladder(52.900, -0.001, 5), ladder(52.950, 0.001, 5)),
					fills("SOXLUSDT", fill{i, now - 2, "1.005"})}
			case "/public/stream":
				frames = [][]byte{depthFrame("SOXLUSDT", 5, now-3, ladder(2.00, -0.01, 5), ladder(2.01, 0.01, 5))}
			default:
				frames = [][]byte{aggTrade("SOXLUSDT", now-130, i, "2.005")}
			}
			for _, p := range frames {
				if c.WriteMessage(websocket.TextMessage, p) != nil {
					return
				}
			}
		}
	}
	srv := venueServer(t, stream)
	rest, _ := instrumentServer(t, map[string]string{"bitget:SOXSUSDT": "0.001"})
	combos := []scan.Combo{{ID: "SOXL-SOXS", Markets: []scan.Market{
		{Venue: "bitget", BullSymbol: "SOXLUSDT", BearSymbol: "SOXSUSDT"}, {Venue: "binance", BullSymbol: "SOXLUSDT"}}}}
	log, logs := testLogger()
	feeds, err := feedsOf(context.Background(), combos, endpoints{wsURL(srv) + "/bitget", rest.URL, wsURL(srv), rest.URL}, log)
	if err != nil {
		t.Fatal(err)
	}
	own, err := pgxpool.New(context.Background(), os.Getenv("BOOKTAPE_TEST_DSN")) // Run closes it
	if err != nil {
		t.Fatal(err)
	}
	m := newManager(feeds, own, t.TempDir(), log)
	ctx, cancel := context.WithTimeout(context.Background(), 3500*time.Millisecond)
	defer cancel()
	if err := m.Run(ctx); err != nil {
		t.Fatal(err)
	}
	ends := map[string]int64{}
	for _, venue := range []string{"bitget", "binance"} {
		for _, e := range readEvents(t, m.output, venue) {
			if e.Event == "disconnected" && e.Error == "shutdown" {
				ends[e.Feed] = e.ReceivedMs
			}
		}
	}
	if len(ends) != 4 {
		t.Fatalf("shutdown events %v\n%s", ends, logs)
	}
	for _, c := range []struct {
		venue, symbol, bookFeed, tradeFeed, price, bid string
		levels                                         int
	}{
		{"bitget", "SOXLUSDT", "bitget-soxl-soxs-books", "bitget-soxl-soxs", "1.005", "1.00", 1},
		{"bitget", "SOXSUSDT", "bitget-soxl-soxs-books", "", "", bids12[0][0], 12},
		{"binance", "SOXLUSDT", "binance-public", "binance-market", "2.005", "2.000", 5},
	} {
		var rows, prices, bids, books, sized int
		var lastBook, lastTrade int64
		err := pool.QueryRow(context.Background(), `SELECT count(*), count(price) FILTER (WHERE price::text = $3), count(*) FILTER (WHERE bid_px[1]::text = $4),
			count(bid_px), count(*) FILTER (WHERE cardinality(bid_px) = $5 AND cardinality(bid_sz) = $5 AND cardinality(ask_px) = $5 AND cardinality(ask_sz) = $5),
			coalesce(max(second_ms) FILTER (WHERE bid_px IS NOT NULL), 0), coalesce(max(second_ms) FILTER (WHERE price IS NOT NULL), 0)
			FROM etfpair.perp_second WHERE venue = $1 AND symbol = $2`, c.venue, c.symbol, c.price, c.bid, c.levels).
			Scan(&rows, &prices, &bids, &books, &sized, &lastBook, &lastTrade)
		if err != nil {
			t.Fatal(err)
		}
		if rows < 2 || (c.price != "") != (prices > 0) || bids < 1 || bids != books || sized != books {
			t.Errorf("%s %s: %d rows, %d with the price, %d with the bid of %d book rows, %d of %d levels", c.venue, c.symbol, rows, prices, bids, books, sized, c.levels)
		}
		if want := (ends[c.bookFeed] - 1) / 1000 * 1000; lastBook != want {
			t.Errorf("%s %s: newest book row %d, want %d, the last second before the shutdown", c.venue, c.symbol, lastBook, want)
		}
		if want := (ends[c.tradeFeed] - 1) / 1000 * 1000; c.tradeFeed != "" && lastTrade != want {
			t.Errorf("%s %s: newest trade row %d, want %d", c.venue, c.symbol, lastTrade, want)
		}
	}
	for _, venue := range []string{"bitget", "binance"} {
		paths, _ := filepath.Glob(filepath.Join(m.output, venue, "*", "*.jsonl.gz"))
		if len(paths) == 0 {
			t.Errorf("%s: no data file", venue)
		}
		for _, p := range paths {
			fileLines(t, p)
		}
	}
}

// When the last write at shutdown fails, the rows it could not store are
// logged at Error with their count, and none stays pending.
func TestShutdownLogsDroppedRows(t *testing.T) {
	shortTimings(t)
	srv := venueServer(t, func(c *websocket.Conn, r *http.Request) {
		c.ReadMessage() // the subscribe frame
		for i := int64(1); ; i++ {
			now := time.Now().UnixMilli()
			if c.WriteMessage(websocket.TextMessage, books5("SOXLUSDT", now, "1", "1", "2", "1")) != nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := l.Addr().String() // nothing listens here once l is closed
	l.Close()
	pool, err := pgxpool.New(context.Background(), "postgres://nobody@"+closed+"/none?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	log, logs := testLogger()
	m := newManager([]*feed{oneFeed("SOXL-SOXS", []string{"SOXLUSDT"}, map[string]int{"SOXLUSDT": 5}, wsURL(srv))}, pool, t.TempDir(), log)
	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	if err := m.Run(ctx); err != nil {
		t.Fatal(err)
	}
	text := logs.String()
	var line string
	for _, l := range strings.Split(text, "\n") {
		if strings.Contains(l, "rows dropped at shutdown") {
			line = l
		}
	}
	if !strings.Contains(line, "level=ERROR") || !strings.Contains(line, " rows=") || strings.Contains(line, " rows=0 ") || len(m.rows.pending) != 0 {
		t.Fatalf("pending %d after Run:\n%s", len(m.rows.pending), text)
	}
	if n := strings.Count(text, "database write failed"); n != 1 {
		t.Fatalf("the failing writes are logged once at Warn, got %d:\n%s", n, text)
	}
}

// --- helpers ---

type fill struct {
	id, t int64
	price string
}

// lv reads levels written "price/size,price/size"; "" is an empty side.
func lv(text string) []level {
	out := []level{}
	if text == "" {
		return out
	}
	for _, p := range strings.Split(text, ",") {
		price, size, _ := strings.Cut(p, "/")
		out = append(out, level{price, size})
	}
	return out
}

// ladder is n levels from start in steps of step (negative for bids), size 1.25.
func ladder(start, step float64, n int) []level {
	out := make([]level, n)
	for i := range out {
		out[i] = level{strconv.FormatFloat(start+float64(i)*step, 'f', 3, 64), "1.25"}
	}
	return out
}

func levelsJSON(levels []level) string {
	var parts []string
	for _, l := range levels {
		parts = append(parts, fmt.Sprintf(`["%s","%s"]`, l[0], l[1]))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// booksFrame is a Bitget books5 or books50 frame of the given levels.
func booksFrame(topic, symbol string, ts int64, bids, asks []level) []byte {
	return fmt.Appendf(nil, `{"action":"snapshot","arg":{"instType":"usdt-futures","topic":"%s","symbol":"%s"},`+
		`"data":[{"a":%s,"b":%s,"checksum":0,"seq":%d,"pseq":0,"ts":"%d"}],"ts":%d}`, topic, symbol, levelsJSON(asks), levelsJSON(bids), ts/7, ts, ts+2)
}

// books5 is a Bitget books5 frame with one level a side.
func books5(symbol string, ts int64, bid, bidSize, ask, askSize string) []byte {
	return booksFrame("books5", symbol, ts, []level{{bid, bidSize}}, []level{{ask, askSize}})
}

// fills is a publicTrade frame holding the fills as given (the venue sends newest first).
func fills(symbol string, fs ...fill) []byte {
	var data []string
	for _, f := range fs {
		data = append(data, fmt.Sprintf(`{"i":"%d","p":"%s","v":"0.5","S":"buy","T":"%d","L":"%d","isRPI":"no"}`, f.id, f.price, f.t, f.id+1))
	}
	return fmt.Appendf(nil, `{"action":"update","arg":{"instType":"usdt-futures","topic":"publicTrade","symbol":"%s"},"data":[%s],"ts":%d}`,
		symbol, strings.Join(data, ","), fs[0].t+1)
}

// depthFrame is a Binance depth5 or depth10 frame stamped t.
func depthFrame(symbol string, levels int, t int64, bids, asks []level) []byte {
	return fmt.Appendf(nil, `{"stream":"%s@depth%d@100ms","data":{"e":"depthUpdate","E":%d,"T":%d,"s":"%s","ps":"%s","U":%d,"u":%d,"pu":%d,"b":%s,"a":%s,"st":1}}`,
		strings.ToLower(symbol), levels, t+8, t, symbol, symbol, t*10, t*10+5, t*10-3, levelsJSON(bids), levelsJSON(asks))
}

func aggTrade(symbol string, t, id int64, p string) []byte {
	return fmt.Appendf(nil, `{"stream":"%s@aggTrade","data":{"e":"aggTrade","E":%d,"a":%d,"s":"%s","p":"%s","q":"1","nq":"1","f":%d,"l":%d,"T":%d,"m":false,"st":1}}`,
		strings.ToLower(symbol), t+130, id, symbol, p, id*10, id*10+1, t)
}

// instrumentServer plays both venues' instrument lists for the six
// contracts of the test combos, ticks 0.01 (Bitget) and 0.01000 (Binance)
// unless ticks names another for "<venue>:<SYMBOL>". Paths under /fail
// answer HTTP 500 and those under /code a Bitget error code. It counts the
// requests per venue.
func instrumentServer(t *testing.T, ticks map[string]string) (*httptest.Server, map[string]*atomic.Int32) {
	requests := map[string]*atomic.Int32{"bitget": {}, "binance": {}}
	tick := func(venue, symbol, otherwise string) string {
		if v, ok := ticks[venue+":"+symbol]; ok {
			return v
		}
		return otherwise
	}
	symbols := []string{"SOXLUSDT", "SOXSUSDT", "SOXXUSDT", "TQQQUSDT", "SQQQUSDT", "QQQUSDT"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var parts []string
		switch {
		case r.URL.Path == "/code/api/v3/market/instruments":
			fmt.Fprint(w, `{"code":"40034","msg":"Parameter does not exist","requestTime":1,"data":null}`)
		case r.URL.Path == "/api/v3/market/instruments" && r.URL.Query().Get("category") == "USDT-FUTURES":
			requests["bitget"].Add(1)
			for _, s := range symbols {
				parts = append(parts, fmt.Sprintf(`{"symbol":"%s","category":"USDT-FUTURES","pricePrecision":"3","priceMultiplier":"%s","quantityMultiplier":"0.01","status":"online"}`,
					s, tick("bitget", s, "0.01")))
			}
			fmt.Fprintf(w, `{"code":"00000","msg":"success","requestTime":1,"data":[%s]}`, strings.Join(parts, ","))
		case r.URL.Path == "/fapi/v1/exchangeInfo":
			requests["binance"].Add(1)
			for _, s := range symbols {
				parts = append(parts, fmt.Sprintf(`{"symbol":"%s","pair":"%s","filters":[{"minPrice":"0.01000","maxPrice":"20000","filterType":"PRICE_FILTER","tickSize":"%s"},`+
					`{"stepSize":"0.01","filterType":"LOT_SIZE"}]}`, s, s, tick("binance", s, "0.01000")))
			}
			fmt.Fprintf(w, `{"timezone":"UTC","serverTime":1,"symbols":[%s]}`, strings.Join(parts, ","))
		default:
			http.Error(w, "no", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, requests
}

// logBuffer collects the log lines of several goroutines.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func testLogger() (*slog.Logger, *logBuffer) {
	logs := &logBuffer{}
	return slog.New(slog.NewTextHandler(logs, nil)), logs
}

// oneFeed is a Bitget feed of one connection carrying every topic of the
// contracts, the book topics by their depth: the recorder's single feed of a
// combo before 2026-11-26, which the tests of the per-stream rules use.
func oneFeed(comboID string, symbols []string, levels map[string]int, endpoint string) *feed {
	topics := func(symbol string) []string {
		return append(append(bitgetBooks(levels[symbol]), "books1"), bitgetTopics...)
	}
	return bitgetFeed("bitget-"+strings.ToLower(comboID), symbols, topics, endpoint, 1)
}

// testFeed binds one feed to a temporary output and a writer without a database.
func testFeed(t *testing.T, f *feed) (*feed, *logBuffer) {
	log, logs := testLogger()
	newManager([]*feed{f}, nil, t.TempDir(), log)
	return f, logs
}

// shortTimings makes the feeds' timings fit a test.
func shortTimings(t *testing.T) {
	saved := []time.Duration{pingEvery, readTimeout, redialAfter, redialIdle, flushEvery, statusEvery, writeEvery}
	pingEvery, readTimeout, redialAfter, redialIdle = 50*time.Millisecond, 300*time.Millisecond, 20*time.Millisecond, 250*time.Millisecond
	flushEvery, statusEvery, writeEvery = 100*time.Millisecond, 200*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() {
		pingEvery, readTimeout, redialAfter, redialIdle, flushEvery, statusEvery, writeEvery = saved[0], saved[1], saved[2], saved[3], saved[4], saved[5], saved[6]
	})
}

func mustBegin(t *testing.T, f *feed, ms int64) {
	t.Helper()
	if err := f.beginConnection(ms); err != nil {
		t.Fatal(err)
	}
}

func process(t *testing.T, f *feed, frames ...frame) {
	t.Helper()
	for _, fr := range frames {
		if err := f.process(fr); err != nil {
			t.Fatal(err)
		}
	}
}

// secondsOf renders the queued rows per second relative to t0:
// "price@trade_ms | bids asks@book_ms", a side "price/size,price/size" best
// first, "-" for a kind without a row. A kind emitted twice for one second
// is an error.
func secondsOf(t *testing.T, rows []row, t0 int64) map[int64]string {
	t.Helper()
	side := func(levels []level) string {
		var parts []string
		for _, l := range levels {
			parts = append(parts, l[0]+"/"+l[1])
		}
		return strings.Join(parts, ",")
	}
	parts := map[int64][2]string{}
	for _, r := range rows {
		p := parts[r.secondMs-t0]
		i, text := 0, fmt.Sprintf("%s@%d", r.event.price, r.event.exchangeMs-t0)
		if r.kind == kindBook {
			i, text = 1, fmt.Sprintf("%s %s@%d", side(r.event.bids), side(r.event.asks), r.event.exchangeMs-t0)
		}
		if p[i] != "" {
			t.Errorf("second %d emitted twice: %s and %s", r.secondMs-t0, p[i], text)
		}
		p[i] = text
		parts[r.secondMs-t0] = p
	}
	out := map[int64]string{}
	for s, p := range parts {
		for i := range p {
			if p[i] == "" {
				p[i] = "-"
			}
		}
		out[s] = p[0] + " | " + p[1]
	}
	return out
}

func assertSeconds(t *testing.T, rows []row, t0 int64, want map[int64]string) {
	t.Helper()
	got := secondsOf(t, rows, t0)
	for s, w := range want {
		if got[s] != w {
			t.Errorf("row %d: got %q, want %q", s, got[s], w)
		}
	}
	for s, g := range got {
		if _, ok := want[s]; !ok {
			t.Errorf("unexpected row %d: %q", s, g)
		}
	}
}

// venueServer plays a venue: the n-th connection runs scripts[n] (the last
// script for any later one) on the server side of the connection.
func venueServer(t *testing.T, scripts ...func(*websocket.Conn, *http.Request)) *httptest.Server {
	var n atomic.Int32
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		scripts[min(int(n.Add(1))-1, len(scripts)-1)](c, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func wsURL(srv *httptest.Server) string { return "ws" + strings.TrimPrefix(srv.URL, "http") }

// answerPings reads until n text pings arrived (n < 0: until the connection
// ends), answering each with "pong" and reporting it as "ping"; any other
// text is reported as "text <frame>".
func answerPings(c *websocket.Conn, got chan<- string, n int) {
	for n != 0 {
		_, msg, err := c.ReadMessage()
		if err != nil {
			return
		}
		if string(msg) != "ping" {
			got <- "text " + string(msg)
			continue
		}
		got <- "ping"
		c.WriteMessage(websocket.TextMessage, []byte("pong"))
		n--
	}
}

// readAll reads until the connection ends, counting text frames.
func readAll(c *websocket.Conn, texts *atomic.Int32) {
	for {
		typ, _, err := c.ReadMessage()
		if err != nil {
			return
		}
		if typ == websocket.TextMessage {
			texts.Add(1)
		}
	}
}

func expect(t *testing.T, got <-chan string, want string) {
	t.Helper()
	select {
	case g := <-got:
		if g != want {
			t.Fatalf("venue saw %q, want %q", g, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("venue did not see %q", want)
	}
}

type testEvent struct {
	ReceivedMs int64           `json:"received_ms"`
	Feed       string          `json:"feed"`
	Event      string          `json:"event"`
	URL        string          `json:"url"`
	Request    json.RawMessage `json:"request"`
	Error      string          `json:"error"`
	Frames     int             `json:"frames"`
	Conn       int             `json:"conn"`
	Payload    json.RawMessage `json:"payload"`
	Text       string          `json:"text"`
}

func readEvents(t *testing.T, dir, venue string) []testEvent {
	t.Helper()
	paths, _ := filepath.Glob(filepath.Join(dir, venue, "*", "events.jsonl"))
	slices.Sort(paths)
	var out []testEvent
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range bytes.Split(bytes.TrimSuffix(b, []byte("\n")), []byte("\n")) {
			var e testEvent
			if err := json.Unmarshal(l, &e); err != nil {
				t.Fatalf("%s: %q: %v", p, l, err)
			}
			out = append(out, e)
		}
	}
	return out
}

// span is one ended connection of the events: its connected and
// disconnected receive times and why it ended.
type span struct {
	start, end int64
	err        string
}

func (s span) duration() time.Duration { return time.Duration(s.end-s.start) * time.Millisecond }

// connections lists the ended connections of a venue's events.
func connections(t *testing.T, dir, venue string) []span {
	var out []span
	for _, e := range readEvents(t, dir, venue) {
		switch e.Event {
		case "connected":
			out = append(out, span{start: e.ReceivedMs})
		case "disconnected":
			out[len(out)-1].end, out[len(out)-1].err = e.ReceivedMs, e.Error
		}
	}
	if len(out) > 0 && out[len(out)-1].end == 0 {
		out = out[:len(out)-1]
	}
	return out
}

// countEvents counts lines of one event kind while a feed may still be writing.
func countEvents(dir, venue, kind string) int {
	paths, _ := filepath.Glob(filepath.Join(dir, venue, "*", "events.jsonl"))
	n := 0
	for _, p := range paths {
		b, _ := os.ReadFile(p)
		n += bytes.Count(b, []byte(`"event":"`+kind+`"`))
	}
	return n
}

// fileLines reads a whole data file: a missing gzip trailer or a wrong checksum is an error.
func fileLines(t *testing.T, path string) [][]byte {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return bytes.Split(bytes.TrimSuffix(b, []byte("\n")), []byte("\n"))
}

// partialLines reads the complete lines of a data file still being written.
func partialLines(t *testing.T, path string) [][]byte {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	var out [][]byte
	sc := bufio.NewScanner(gz)
	for sc.Scan() {
		out = append(out, slices.Clone(sc.Bytes()))
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("%s: %v", path, err)
	}
	return out
}

// streamLines reads every data file of a stream in time order.
func streamLines(t *testing.T, dir, venue, stream string) [][]byte {
	t.Helper()
	paths, _ := filepath.Glob(filepath.Join(dir, venue, "*", stream+".*.jsonl.gz"))
	first := func(p string) int64 {
		name := strings.TrimSuffix(filepath.Base(p), ".jsonl.gz")
		v, _ := strconv.ParseInt(name[strings.LastIndex(name, ".")+1:], 10, 64)
		return v
	}
	slices.SortFunc(paths, func(a, b string) int { return int(first(a) - first(b)) })
	var out [][]byte
	for _, p := range paths {
		out = append(out, fileLines(t, p)...)
	}
	return out
}

// testPool connects to the throwaway database of BOOKTAPE_TEST_DSN and drops the table.
func testPool(t *testing.T) *pgxpool.Pool {
	dsn := os.Getenv("BOOKTAPE_TEST_DSN")
	if dsn == "" {
		t.Skip("BOOKTAPE_TEST_DSN names no throwaway database")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(context.Background(), `DROP TABLE IF EXISTS etfpair.perp_second`); err != nil {
		t.Fatal(err)
	}
	return pool
}

// assertTable compares the bitget rows of a symbol, rendered
// "price trade_ms bid_px bid_sz ask_px ask_sz book_ms" with "-" for NULL.
func assertTable(t *testing.T, pool *pgxpool.Pool, symbol string, want map[int64]string) {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT second_ms, concat_ws(' ', coalesce(price::text, '-'), coalesce(trade_ms::text, '-'),
		coalesce(bid_px::text, '-'), coalesce(bid_sz::text, '-'), coalesce(ask_px::text, '-'), coalesce(ask_sz::text, '-'), coalesce(book_ms::text, '-'))
		FROM etfpair.perp_second WHERE venue = 'bitget' AND symbol = $1`, symbol)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]string{}
	for rows.Next() {
		var s int64
		var text string
		if err := rows.Scan(&s, &text); err != nil {
			t.Fatal(err)
		}
		got[s] = text
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	if len(got) != len(want) {
		t.Errorf("%s: %d rows, want %d: %v", symbol, len(got), len(want), got)
	}
	for s, w := range want {
		if got[s] != w {
			t.Errorf("%s row %d: %q, want %q", symbol, s, got[s], w)
		}
	}
}

// The top-of-book topics (Bitget books1, Binance bookTicker) are stored as
// received and feed no per-second row.
func TestTopOfBookFilesOnly(t *testing.T) {
	if rowKind("bitget", "books1") != kindNone || rowKind("binance", "bookTicker") != kindNone {
		t.Fatal("books1 and bookTicker must not feed the per-second rows")
	}
	books1 := []byte(`{"action":"snapshot","arg":{"instType":"usdt-futures","topic":"books1","symbol":"SOXLUSDT"},` +
		`"data":[{"a":[["50.01","3"]],"b":[["50.00","2"]],"checksum":0,"pseq":0,"seq":7,"ts":"1795087435681"}],"ts":1795087435682}`)
	ticker := []byte(`{"stream":"soxlusdt@bookTicker","data":{"e":"bookTicker","u":1,"s":"SOXLUSDT","b":"50.00","B":"2",` +
		`"a":"50.01","A":"3","T":1795087435681,"E":1795087435682}}`)
	for venue, frame := range map[string][]byte{"bitget": books1, "binance": ticker} {
		r, err := route(venue, frame)
		if err != nil || len(r.events) != 0 || !bytes.Equal(r.payload, frame) {
			t.Fatalf("%s: routed %+v, %v", venue, r, err)
		}
	}
	if r, _ := route("bitget", books1); r.stream != "SOXLUSDT.books1" {
		t.Fatalf("bitget stream %q", r.stream)
	}
	if r, _ := route("binance", ticker); r.stream != "SOXLUSDT.bookTicker" {
		t.Fatalf("binance stream %q", r.stream)
	}
}
