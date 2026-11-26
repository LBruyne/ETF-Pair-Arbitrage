package main

import (
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"etf-pair-arbitrage/core/cex"
)

// writeFile writes one gzipped raw file of the recorder.
func writeFile(t *testing.T, dir, name string, lines ...[]byte) {
	t.Helper()
	day := filepath.Join(dir, "bitget", "20261125")
	os.MkdirAll(day, 0o755)
	f, err := os.Create(filepath.Join(day, name))
	if err != nil {
		t.Fatal(err)
	}
	w := gzip.NewWriter(f)
	for _, l := range lines {
		w.Write(l)
	}
	w.Close()
	f.Close()
}

// The bear leg's depth is books50 cut to five levels before its books5
// recording starts and books5 from then on; the inputs of all topics come
// in arrival order, a depth frame before a books1 frame of the same
// millisecond and sequence number, and a leg is lost only when the
// recorder's feed carrying its books1 disconnects (the feed's last
// connection ends): not when one of its connections ends, nor when the
// trades' feed or another pair's feed does.
func TestTapeReadsTheDepthOfItsTimeInArrivalOrder(t *testing.T) {
	dir := t.TempDir()
	ten := side(d("60"), "0.001", 10, true)
	asks := side(d("60.001"), "0.001", 10, false)
	writeFile(t, dir, "SOXSUSDT.books50.1000.jsonl.gz", tapeLine(1000, "SOXSUSDT", "books50", 1, ten, asks), tapeLine(3000, "SOXSUSDT", "books50", 5, ten, asks))
	writeFile(t, dir, "SOXSUSDT.books5.2000.jsonl.gz", tapeLine(2000, "SOXSUSDT", "books5", 3, ten[:5], asks[:5]), tapeLine(4000, "SOXSUSDT", "books5", 7, ten[:5], asks[:5]))
	writeFile(t, dir, "SOXSUSDT.books1.1000.jsonl.gz", tapeLine(1000, "SOXSUSDT", "books1", 1, ten[:1], asks[:1]))
	writeFile(t, dir, "SOXLUSDT.books5.1000.jsonl.gz", tapeLine(1500, "SOXLUSDT", "books5", 2, side(d("40"), "0.01", 5, true), side(d("40.01"), "0.01", 5, false)))
	writeFile(t, dir, "SOXLUSDT.books1.1000.jsonl.gz", tapeLine(1500, "SOXLUSDT", "books1", 2, side(d("40"), "0.01", 1, true), side(d("40.01"), "0.01", 1, false)))
	subscribe := func(feed string, args ...string) string {
		var parts []string
		for _, a := range args {
			symbol, topic, _ := strings.Cut(a, ".")
			parts = append(parts, `{"instType":"usdt-futures","topic":"`+topic+`","symbol":"`+symbol+`"}`)
		}
		return `{"received_ms":900,"feed":"` + feed + `","event":"connected","url":"wss://x","request":{"op":"subscribe","args":[` + strings.Join(parts, ",") + `]}}`
	}
	os.WriteFile(filepath.Join(dir, "bitget", "20261125", "events.jsonl"), []byte(strings.Join([]string{
		subscribe("bitget-soxl-soxs-books", "SOXLUSDT.books5", "SOXLUSDT.books1", "SOXSUSDT.books50", "SOXSUSDT.books5", "SOXSUSDT.books1"),
		subscribe("bitget-soxl-soxs", "SOXLUSDT.publicTrade", "SOXSUSDT.publicTrade"),
		subscribe("bitget-tqqq-sqqq-books", "TQQQUSDT.books5", "TQQQUSDT.books1"),
		`{"received_ms":2200,"feed":"bitget-soxl-soxs-books","event":"link_disconnected","conn":1}`, // one connection of three: the books stay
		`{"received_ms":2300,"feed":"bitget-soxl-soxs","event":"disconnected"}`,                     // the trades' connection: not a book loss
		`{"received_ms":2500,"feed":"bitget-soxl-soxs-books","event":"disconnected"}`,
		`{"received_ms":2600,"feed":"bitget-tqqq-sqqq-books","event":"disconnected"}`}, "\n")+"\n"), 0o644)

	streams, downs, err := openTape(tapeSpec{dir: dir, from: 1000, to: 5000, bull: "SOXLUSDT", bear: "SOXSUSDT", bearBooks5From: 2000})
	if err != nil {
		t.Fatal(err)
	}
	m := newMerger(streams, downs)
	type seen struct {
		at     int64
		leg    cex.Leg
		topic  string
		down   bool
		levels int
	}
	var got []seen
	for {
		in, err := m.next()
		if err != nil {
			t.Fatal(err)
		}
		if in == nil {
			break
		}
		got = append(got, seen{in.at, in.leg, in.ev.Topic, in.down, len(in.ev.Bids)})
	}
	want := []seen{
		{1000, cex.Bear, cex.TopicBooks5, false, 5}, // books50, cut to five levels and delivered as the depth
		{1000, cex.Bear, cex.TopicBooks1, false, 1},
		{1500, cex.Bull, cex.TopicBooks5, false, 5},
		{1500, cex.Bull, cex.TopicBooks1, false, 1},
		{2000, cex.Bear, cex.TopicBooks5, false, 5}, // books5 from its start; books50's frame of 3000 is left out
		{2500, cex.Bull, "", true, 0},
		{2500, cex.Bear, "", true, 0},
		{4000, cex.Bear, cex.TopicBooks5, false, 5},
	}
	if len(got) != len(want) {
		t.Fatalf("inputs %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("input %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// The grid expands every combination over the base configuration, N
// becoming the capital M = N / L.
func TestGridExpandsTheCombinations(t *testing.T) {
	base := testSim(t, "base", 15).cfg
	points, err := parseGrid("model=ema,sma;din=1.5,2;lat=5/25;n=1000,100000", base)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 8 {
		t.Fatalf("%d points, want 8", len(points))
	}
	p := points[len(points)-1]
	if p.cfg.Strategy.MeanModel != "sma" || p.cfg.Strategy.EntryIn != 2 || p.cfg.Exec.SimOrderLatency.Bull.Milliseconds() != 5 || p.cfg.Exec.SimOrderLatency.Bear.Milliseconds() != 25 ||
		!p.cfg.Inventory.Capital.Equal(d("10000")) || p.cfg.Strategy.MeanMinutes != base.Strategy.MeanMinutes {
		t.Errorf("last point %s: %+v %+v %s", p.name, p.cfg.Strategy, p.cfg.Exec.SimOrderLatency, p.cfg.Inventory.Capital)
	}
	if !base.Inventory.Capital.Equal(d("2000")) || base.Strategy.EntryIn != 2 {
		t.Error("the grid changed the base configuration")
	}
	if _, err := parseGrid("din=2;dout=3", base); err == nil {
		t.Error("entry_out >= entry_in was accepted")
	}
}

// lastBook gives every level of the newest frame received within maxAge
// before the moment, none when the newest is older or there is no file.
func TestLastBookReadsTheNewestFrameWhole(t *testing.T) {
	dir := t.TempDir()
	bids, asks := side(d("60"), "0.001", 50, true), side(d("60.001"), "0.001", 50, false)
	writeFile(t, dir, "SOXSUSDT.books50.1000.jsonl.gz", tapeLine(1000, "SOXSUSDT", "books50", 1, bids, asks), tapeLine(4000, "SOXSUSDT", "books50", 2, bids[1:], asks[1:]),
		tapeLine(9000, "SOXSUSDT", "books50", 3, bids, asks))
	b, a, ok := lastBook(dir, cex.Bear, "SOXSUSDT", cex.TopicBooks50, time.UnixMilli(5000), 10*time.Second)
	if !ok || len(b) != 49 || len(a) != 49 || !a[0].Price.Equal(d("60.002")) {
		t.Fatalf("at 5000: ok %t, %d bids, %d asks (first %v); want the frame of 4000 whole", ok, len(b), len(a), a)
	}
	if _, _, ok := lastBook(dir, cex.Bear, "SOXSUSDT", cex.TopicBooks50, time.UnixMilli(8500), 2*time.Second); ok {
		t.Error("the newest frame before 8500 is 4.5 s old; with a 2 s max age there is none")
	}
	if _, _, ok := lastBook(dir, cex.Bull, "SOXLUSDT", cex.TopicBooks50, time.UnixMilli(5000), 10*time.Second); ok {
		t.Error("no SOXL books50 files: none")
	}
}
