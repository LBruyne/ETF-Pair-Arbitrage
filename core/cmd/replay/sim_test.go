package main

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/config"
	"etf-pair-arbitrage/core/logger"
	"etf-pair-arbitrage/core/strategy"
)

// t0 is the start of the synthetic tape: Thanksgiving 2026-11-26 06:00
// UTC+8, inside Session 2026-11-25 and outside regular hours all day.
var t0 = time.Date(2026, 11, 26, 6, 0, 0, 0, utc8)

// tapeLine is one line of a raw file as the recorder writes it.
func tapeLine(received int64, symbol, topic string, seq uint64, bids, asks [][2]string) []byte {
	b, _ := json.Marshal(map[string]any{"received_ms": received, "payload": map[string]any{"action": "snapshot",
		"arg":  map[string]any{"instType": "usdt-futures", "topic": topic, "symbol": symbol},
		"data": []any{map[string]any{"a": asks, "b": bids, "seq": seq, "pseq": 0, "ts": fmt.Sprint(received - 3)}}, "ts": received - 2}})
	return append(b, '\n')
}

// side lists n levels from best, tick apart, 50 each.
func side(best decimal.Decimal, tick string, n int, down bool) [][2]string {
	step := decimal.RequireFromString(tick)
	if down {
		step = step.Neg()
	}
	var out [][2]string
	for i := range n {
		out = append(out, [2]string{best.Add(step.Mul(decimal.NewFromInt(int64(i)))).String(), "50"})
	}
	return out
}

// writeTape writes ten minutes of both legs' books5 and books1, a frame of
// each every 500 ms, into a recorder-shaped directory: the spread alternates
// by 10 units from minute to minute, jumps by about 100 in the first half of
// minute 6 and falls 10 below its low in the second half.
func writeTape(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	day := filepath.Join(dir, "bitget", "20261126")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]*gzip.Writer{}
	var closers []func()
	out := func(symbol, topic string) *gzip.Writer {
		key := symbol + "." + topic
		if w := files[key]; w != nil {
			return w
		}
		f, err := os.Create(filepath.Join(day, fmt.Sprintf("%s.%d.jsonl.gz", key, t0.UnixMilli())))
		if err != nil {
			t.Fatal(err)
		}
		w := gzip.NewWriter(f)
		files[key] = w
		closers = append(closers, func() { w.Close(); f.Close() })
		return w
	}
	seq := uint64(1000)
	for ms := int64(0); ms < 10*60*1000; ms += 500 {
		minute := ms / 60000
		bull := decimal.RequireFromString("40.00")
		switch {
		case minute == 6 && ms%60000 < 30000:
			bull = decimal.RequireFromString("40.40")
		case minute == 6:
			bull = decimal.RequireFromString("39.96")
		case minute%2 == 1:
			bull = decimal.RequireFromString("40.04")
		}
		bear := decimal.RequireFromString("59.999")
		at := t0.UnixMilli() + ms
		for _, leg := range []struct {
			symbol, tick string
			bid          decimal.Decimal
		}{{"SOXLUSDT", "0.01", bull}, {"SOXSUSDT", "0.001", bear}} {
			seq++
			ask := leg.bid.Add(decimal.RequireFromString(leg.tick))
			bids, asks := side(leg.bid, leg.tick, 5, true), side(ask, leg.tick, 5, false)
			out(leg.symbol, "books5").Write(tapeLine(at, leg.symbol, "books5", seq, bids, asks))
			out(leg.symbol, "books1").Write(tapeLine(at+1, leg.symbol, "books1", seq+1, bids[:1], asks[:1]))
			seq++
		}
	}
	for _, c := range closers {
		c()
	}
	return dir
}

// testSim is the example configuration with an SMA of two minutes, on the
// synthetic tape, with F = 1 every second and the bell anchors 40 / 60
// known at the start.
func testSim(t *testing.T, name string, latency time.Duration) simConfig {
	t.Helper()
	b, err := os.ReadFile("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Strategy.MeanModel, cfg.Strategy.MeanMinutes, cfg.Strategy.SigmaMinutes = strategy.MeanSMA, 2, 2
	cfg.Exec.SimOrderLatency.Bull, cfg.Exec.SimOrderLatency.Bear = latency, latency
	cfg.Inventory.Capital = decimal.RequireFromString("2000")
	refs := []refEvent{{at: t0.UnixMilli(), anchors: &cex.Anchors{SessionID: "2026-11-25", Kind: cex.AnchorBell, Bull: decimal.NewFromInt(40), Bear: decimal.NewFromInt(60), KnownAt: t0}}}
	for s := int64(0); s <= 600; s++ {
		fx := cex.FX{Price: decimal.NewFromInt(1), Bid: decimal.NewFromInt(1), Ask: decimal.NewFromInt(1), ReceivedAt: t0.Add(time.Duration(s) * time.Second)}
		refs = append(refs, refEvent{at: fx.ReceivedAt.UnixMilli(), fx: &fx})
	}
	return simConfig{name: name, cfg: cfg, start: t0, end: t0.Add(10 * time.Minute), refs: refs}
}

// The same tape replayed twice - in one pass and again in another - gives
// the same orders, fills, ledger and decisions; the scenario trades: an
// entry at the jump, an exit when the spread is back, flat at the end. A
// different latency is a different run.
func TestReplayIsDeterministic(t *testing.T) {
	logger.GlobalLogger = slog.New(slog.DiscardHandler)
	dir := writeTape(t)
	spec := tapeSpec{dir: dir, from: t0.UnixMilli(), to: t0.Add(10 * time.Minute).UnixMilli(), bull: "SOXLUSDT", bear: "SOXSUSDT"}
	first, err := runWindow(spec, []simConfig{testSim(t, "a", 15*time.Millisecond), testSim(t, "b", 15*time.Millisecond), testSim(t, "slow", 300*time.Millisecond)})
	if err != nil {
		t.Fatal(err)
	}
	again, err := runWindow(spec, []simConfig{testSim(t, "c", 15*time.Millisecond)})
	if err != nil {
		t.Fatal(err)
	}
	a := first[0].sink
	sum := summarize(a, "", decimal.NewFromInt(20000))
	if sum.OrdersByPurpose["entry"] == 0 || sum.OrdersByPurpose["exit"] == 0 || sum.EndReasons["exit_line"] == 0 {
		t.Fatalf("the scenario did not trade as designed: %+v", sum)
	}
	if !sum.OpenQty[0].IsZero() || !sum.OpenQty[1].IsZero() || first[0].inv.Cycle() != nil {
		t.Errorf("not flat at the end: open %v, cycle %v", sum.OpenQty, first[0].inv.Cycle())
	}
	print := fingerprint(a)
	for _, other := range []*sim{first[1], again[0]} {
		if fingerprint(other.sink) != print || !slices.Equal(other.sink.orderSeq, a.orderSeq) {
			t.Errorf("%s differs from a: orders %v vs %v", other.sc.name, other.sink.orderSeq, a.orderSeq)
		}
	}
	if fingerprint(first[2].sink) == print {
		t.Error("a 300 ms latency gave the same run as 15 ms: the latency is not applied")
	}
	if first[0].steps == 0 || first[0].sink.sampled < 500 {
		t.Errorf("the loop ran %d times and sampled %d seconds", first[0].steps, first[0].sink.sampled)
	}
}
