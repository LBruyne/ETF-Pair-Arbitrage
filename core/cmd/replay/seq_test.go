package main

import (
	"encoding/json"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/logger"
	"etf-pair-arbitrage/core/store"
)

// The grid takes the thin leg (bear, bull or none) and the second exit
// line (a number of sigmas or none) as keys of their own (SPEC 12.9): every
// combination is a point, none being both legs together and one exit line.
func TestGridSetsThinLegAndExit2(t *testing.T) {
	base := testSim(t, "base", 15).cfg
	points, err := parseGrid("din=1;dout=-0.5;thin=none,bear;exit2=none,-1;lat=15/15", base)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 4 {
		t.Fatalf("%d points, want 4", len(points))
	}
	seen := map[string]bool{}
	for _, p := range points {
		thin, exit2 := p.cfg.Exec.ThinLeg, p.cfg.Strategy.Exit2Text()
		key := thin + "/" + exit2
		seen[key] = true
		want := "_thin" + map[string]string{"": "none", "bear": "bear"}[thin] + "_exit2" + exit2
		if !strings.Contains(p.name, want) {
			t.Errorf("point %s: name lacks %s", p.name, want)
		}
	}
	for _, k := range []string{"/none", "/-1", "bear/none", "bear/-1"} {
		if !seen[k] {
			t.Errorf("no point with thin/exit2 %s: %v", k, seen)
		}
	}
	if base.Exec.ThinLeg != "" || base.Strategy.Exit2 != nil {
		t.Error("the grid changed the base configuration")
	}
	// Two points never share one exit2 value.
	if points[1].cfg.Strategy.Exit2 != nil && points[3].cfg.Strategy.Exit2 != nil && points[1].cfg.Strategy.Exit2 == points[3].cfg.Strategy.Exit2 {
		t.Error("two points share the exit2 pointer")
	}
	for _, bad := range []string{"thin=soxs;lat=15/15", "exit2=x;lat=15/15", "din=1;dout=-0.5;exit2=0;lat=15/15", "din=1;dout=-0.5;exit2=-2;lat=15/15"} {
		if _, err := parseGrid(bad, base); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

// seqSink is one long cycle of the thin leg first (SPEC 12.4-12.7), with no
// fee but on the thin leg's entries and its profit close:
//   - lot c1-1: SOXS 5 @ 60 (fee 0.03) at 1015, then SOXL 6 @ 40.1 at 1100
//     and 4 @ 40 at 1300 (decision price 40): built;
//   - lot c1-2: SOXS fills nothing: entry_failed;
//   - lot c1-3: SOXS 2 @ 60 (fee 0.012) at 3015, closed at a profit at 60.5
//     (fee 0.0121) at 3500: unwound_profit;
//   - exit1 at 5000 closes half (SOXS 2.5 @ 61, SOXL 5 @ 41), the half
//     target done at 5100;
//   - exit2 at 9000: SOXS 2.5 @ 60.8, the thick leg's hard end (D2,
//     exit_window) at 39100, SOXL 5 @ 40.5 at 39200; the cycle closes there.
//
// Its net: c1-1 6.9 + 4.5 - 0.03 = 11.37, c1-3 1 - 0.0241 = 0.9759.
func seqSink() *memSink {
	s := newMemSink(false)
	detail := func(kv map[string]any) json.RawMessage { b, _ := json.Marshal(kv); return b }
	order := func(id, lot, leg, purpose, side, qty, value, fee string, at int64) {
		s.Record(store.OrderRow{ClientOrderID: id, CycleID: "c1", LotID: lot, Leg: leg, Purpose: purpose, Side: side, Qty: d(qty), CumExecQty: d(qty), CumExecValue: d(value), Fee: d(fee), Status: "filled", SubmittedMs: at})
	}
	fill := func(id, order, side, qty, value, fee string, at int64) {
		s.Record(store.FillRow{ExecID: id, ClientOrderID: order, Side: side, Qty: d(qty), Value: d(value), Price: d(value).Div(d(qty)), Fee: d(fee), ExecMs: at})
	}
	entry := func(lot string, at int64) {
		s.Record(store.DecisionRow{TsMs: at, CycleID: "c1", LotID: lot, Kind: "entry", Direction: "long",
			Detail: detail(map[string]any{"price_bull": d("40"), "qty_bull": d("10"), "price_bear": d("60"), "qty_bear": d("5"), "thin_leg": "bear"})})
	}
	entry("c1-1", 1000)
	order("o01", "c1-1", "bear", "entry", "buy", "5", "300", "0.03", 1000)
	fill("f01", "o01", "buy", "5", "300", "0.03", 1015)
	order("o02", "c1-1", "bull", "complete", "buy", "6", "240.6", "0", 1085)
	fill("f02", "o02", "buy", "6", "240.6", "0", 1100)
	order("o03", "c1-1", "bull", "complete", "buy", "4", "160", "0", 1285)
	fill("f03", "o03", "buy", "4", "160", "0", 1300)
	entry("c1-2", 2000)
	s.Record(store.OrderRow{ClientOrderID: "o04", CycleID: "c1", LotID: "c1-2", Leg: "bear", Purpose: "entry", Side: "buy", Qty: d("5"), Status: "cancelled", SubmittedMs: 2000})
	entry("c1-3", 3000)
	order("o05", "c1-3", "bear", "entry", "buy", "2", "120", "0.012", 3000)
	fill("f05", "o05", "buy", "2", "120", "0.012", 3015)
	s.Record(store.DecisionRow{TsMs: 3485, CycleID: "c1", LotID: "c1-3", Kind: "pair", Direction: "long", Reason: "profit", Detail: detail(map[string]any{"action": "close_thin", "client_order_id": "o06"})})
	order("o06", "c1-3", "bear", "trim", "sell", "2", "121", "0.0121", 3485)
	fill("f06", "o06", "sell", "2", "121", "0.0121", 3500)

	s.Record(store.DecisionRow{TsMs: 5000, CycleID: "c1", Kind: "exit", Direction: "long", Detail: detail(map[string]any{"line": "exit1", "expected_net": d("3")})})
	order("o07", "", "bear", "exit", "sell", "2.5", "152.5", "0", 5000)
	fill("f07", "o07", "sell", "2.5", "152.5", "0", 5015)
	order("o08", "", "bull", "exit", "sell", "5", "205", "0", 5085)
	fill("f08", "o08", "sell", "5", "205", "0", 5100)
	s.Record(store.ExitStageRow{CycleID: "c1", SetMs: 5000, BuiltLots: 1, TargetBull: d("5"), TargetBear: d("2.5"), ClosedBull: d("0"), ClosedBear: d("2.5")})
	s.Record(store.ExitStageRow{CycleID: "c1", SetMs: 5000, BuiltLots: 1, TargetBull: d("5"), TargetBear: d("2.5"), ClosedBull: d("5"), ClosedBear: d("2.5"), DoneMs: 5100})

	s.Record(store.DecisionRow{TsMs: 9000, CycleID: "c1", Kind: "exit", Direction: "long", Detail: detail(map[string]any{"line": "exit2", "expected_net": d("5")})})
	order("o09", "", "bear", "exit", "sell", "2.5", "152", "0", 9000)
	fill("f09", "o09", "sell", "2.5", "152", "0", 9015)
	s.Record(store.DecisionRow{TsMs: 39100, CycleID: "c1", Kind: "exit_pair", Direction: "long", Reason: "exit_window", Detail: detail(map[string]any{"action": "risk_exit"})})
	order("o10", "", "bull", "exit", "sell", "5", "202.5", "0", 39100)
	fill("f10", "o10", "sell", "5", "202.5", "0", 39200)

	s.Record(store.LotRow{LotID: "c1-1", CycleID: "c1", Seq: 1, Status: "closed", EndState: "built", Direction: "long", OpenedMs: 1000},
		store.LotRow{LotID: "c1-2", CycleID: "c1", Seq: 2, Status: "void", VoidReason: "entry_failed", EndState: "entry_failed", Direction: "long", OpenedMs: 2000},
		store.LotRow{LotID: "c1-3", CycleID: "c1", Seq: 3, Status: "void", VoidReason: "unwound_profit", EndState: "unwound_profit", Direction: "long", OpenedMs: 3000})
	s.Record(store.CycleRow{CycleID: "c1", SessionID: "2026-11-25", Direction: "long", Status: "closed", EndReason: "exit_line", Net: d("12.3459"), OpenedMs: 1000, ClosedMs: 39200})
	for _, p := range [][2]string{{"1015", "300"}, {"1300", "700.6"}, {"3015", "820.6"}, {"3500", "700.6"}, {"5100", "350.3"}, {"39200", "0"}} {
		ms, _ := decimal.NewFromString(p[0])
		s.held = append(s.held, heldPoint{ms: ms.IntPart(), value: d(p[1])})
	}
	return s
}

// The report of 12.10 from a run's records: what each entry signal built,
// the batches by end state, how long a leg stood alone, what the trims,
// profit closes and completions cost, the D2 path, what exit1 closed and
// how long the round held quota from its first line exit until flat. The
// orders' realised nets, booked as the ledger books them (an entry on its
// lot, a trim on its lot, an exit first in first out), add up to the
// cycles' round-trip net.
func TestSeqStatsOfAThinFirstRun(t *testing.T) {
	q := summarize(seqSink(), "", d("1000"), d("820.6"), nil).Seq
	if q.Signals != 3 || !q.PlannedNotional.Equal(d("2100")) || !q.BuiltNotional.Equal(d("700.6")) {
		t.Errorf("signals %d, planned %s, built %s; want 3, 2100, 700.6", q.Signals, q.PlannedNotional, q.BuiltNotional)
	}
	want := map[string]int{"built": 1, "entry_failed": 1, "unwound_profit": 1}
	for k, v := range want {
		if q.EndStates[k] != v {
			t.Errorf("end states %v, want %v", q.EndStates, want)
		}
	}
	if len(q.EndStates) != len(want) {
		t.Errorf("end states %v, want %v", q.EndStates, want)
	}
	if q.ExposedBatches != 2 || math.Abs(q.ExposureMedianS-0.385) > 1e-9 || math.Abs(q.ExposureMaxS-0.485) > 1e-9 {
		t.Errorf("exposure: %d batches, median %v s, max %v s; want 2, 0.385, 0.485", q.ExposedBatches, q.ExposureMedianS, q.ExposureMaxS)
	}
	if !q.CompleteSlip.Equal(d("0.6")) || !q.ProfitCloseNet.Equal(d("0.9759")) || !q.UnwindNet.IsZero() {
		t.Errorf("completion slip %s, profit close %s, unwind %s; want 0.6, 0.9759, 0", q.CompleteSlip, q.ProfitCloseNet, q.UnwindNet)
	}
	if q.D2Batches != 1 || !q.D2Net.Equal(d("4.185")) || !q.D2Expected.Equal(d("5")) || q.D1Batches != 0 {
		t.Errorf("D2: %d batches, net %s, expected %s; D1 %d; want 1, 4.185, 5, 0", q.D2Batches, q.D2Net, q.D2Expected, q.D1Batches)
	}
	if !q.ClosedByLine["exit1"].Equal(d("357.5")) || !q.ClosedByLine["exit2"].Equal(d("354.5")) || !q.NetByLine["exit1"].Equal(d("7.185")) || !q.NetByLine["exit2"].Equal(d("4.185")) {
		t.Errorf("closed by line %v, net by line %v", q.ClosedByLine, q.NetByLine)
	}
	if q.StagesSet != 1 || q.StagesDone != 1 {
		t.Errorf("half targets set %d, done %d; want 1 and 1", q.StagesSet, q.StagesDone)
	}
	// From the first line-exit fill at 5015 to the close at 39200: 0.085 s
	// at 700.6 and 34.1 s at 350.3.
	if q.LineToFlatCycles != 1 || math.Abs(q.LineToFlatMedianS-34.185) > 1e-9 || math.Abs(q.LineToFlatUSDTh-(0.085*700.6+34.1*350.3)/3600) > 1e-9 {
		t.Errorf("line to flat: %d cycles, median %v s, %v USDT h", q.LineToFlatCycles, q.LineToFlatMedianS, q.LineToFlatUSDTh)
	}
	if !q.RealisedNet.Equal(d("12.3459")) || !q.RealisedCheck.IsZero() {
		t.Errorf("realised %s, check %s; want the cycle's 12.3459 and 0", q.RealisedNet, q.RealisedCheck)
	}
}

// Both legs together (no thin leg) name the same end states by what the
// batch's fills did: built when both legs filled, built_partial when a trim
// cut the built batch, entry_failed when nothing filled, unwound_stop when
// one leg filled and was trimmed back. The trims are unwinds.
func TestSeqStatsOfBothLegsTogether(t *testing.T) {
	s := exampleSink()
	q := summarize(s, "", d("10000"), d("700"), nil).Seq
	if q.EndStates["built_partial"] != 1 || q.EndStates["unwound_stop"] != 1 || len(q.EndStates) != 2 {
		t.Errorf("end states %v, want built_partial 1 and unwound_stop 1", q.EndStates)
	}
	// cA-1: first fill 1015, its trim 2015; cB-1: 4015 to 4115.
	if q.ExposedBatches != 2 || math.Abs(q.ExposureMaxS-1) > 1e-9 || math.Abs(q.ExposureMedianS-0.55) > 1e-9 {
		t.Errorf("exposure %d, median %v, max %v", q.ExposedBatches, q.ExposureMedianS, q.ExposureMaxS)
	}
	// Trim a3: 205 - 200 - 0.013325 - 0.013; trim b3: 159.6 - 160 - 0.010374 - 0.0104.
	if !q.UnwindNet.Equal(d("4.552901")) || !q.ProfitCloseNet.IsZero() {
		t.Errorf("unwind %s, profit close %s; want 4.552901 and 0", q.UnwindNet, q.ProfitCloseNet)
	}
	// The exit orders without an exit decision count on the one line.
	if !q.ClosedByLine["exit"].Equal(d("515")) {
		t.Errorf("closed by line %v, want exit 515", q.ClosedByLine)
	}
}

// A run with the thin leg first on the synthetic tape: every entry signal's
// batch ends in one of the end states, the ledger's held value over time is
// kept, and the orders' realised nets add up to the cycles' net.
func TestThinFirstReplayReportsEndStates(t *testing.T) {
	logger.GlobalLogger = slog.New(slog.DiscardHandler)
	dir := writeTape(t)
	spec := tapeSpec{dir: dir, from: t0.UnixMilli(), to: t0.Add(10 * time.Minute).UnixMilli(), bull: "SOXLUSDT", bear: "SOXSUSDT"}
	sc := testSim(t, "thin", 15*time.Millisecond)
	cfg := *sc.cfg
	cfg.Exec.ThinLeg = "bear"
	sc.cfg = &cfg
	done, err := runWindow(spec, []simConfig{sc})
	if err != nil {
		t.Fatal(err)
	}
	s := done[0]
	sum := summarize(s.sink, "", d("20000"), s.peak(""), s.closeout(spec))
	q := sum.Seq
	if q.Signals == 0 {
		t.Fatalf("the scenario did not enter: %+v", sum)
	}
	n := 0
	for k, v := range q.EndStates {
		if !map[string]bool{"built": true, "built_partial": true, "entry_failed": true, "unwound_profit": true, "unwound_stop": true}[k] {
			t.Errorf("end state %q", k)
		}
		n += v
	}
	if n != q.Signals {
		t.Errorf("%d end states for %d signals: %v", n, q.Signals, q.EndStates)
	}
	// The last held value is what the ledger holds at the end (here the
	// dust a partly built batch left).
	if len(s.sink.held) == 0 || !s.sink.held[len(s.sink.held)-1].value.Equal(s.inv.HeldNotional()) {
		t.Errorf("held series %v: want the ledger's changes, ending at its %s", s.sink.held, s.inv.HeldNotional())
	}
	if !q.RealisedCheck.Abs().LessThan(d("0.000001")) {
		t.Errorf("the orders' realised net is %s off the cycles' net", q.RealisedCheck)
	}
}

// grid.csv carries the thin leg, exit2 and the columns of 12.10, one cell
// per header.
func TestGridRowCarriesTheSeqColumns(t *testing.T) {
	cfg := *testSim(t, "g", 15*time.Millisecond).cfg
	x := -1.0
	cfg.Exec.ThinLeg, cfg.Strategy.Exit2 = "bear", &x
	s := &sim{sc: simConfig{cfg: &cfg}}
	sum := summarize(seqSink(), "", d("1000"), d("820.6"), nil)
	row := gridRow(window{label: "w"}, "g", s, sum, "print")
	if len(row) != len(gridHeader) {
		t.Fatalf("%d cells for %d columns", len(row), len(gridHeader))
	}
	cell := map[string]string{}
	for i, h := range gridHeader {
		cell[h] = row[i]
	}
	for k, v := range map[string]string{"thin_leg": "bear", "exit2": "-1", "signals": "3", "built_per_signal": "233.53333333", "es_built": "1", "es_entry_failed": "1",
		"es_unwound_profit": "1", "es_unwound_stop": "0", "exposure_median_s": "0.385", "profit_close_net": "0.9759", "d2_batches": "1", "d2_net": "4.185",
		"closed_exit1": "357.5", "closed_exit2": "354.5", "line_to_flat_median_s": "34.185", "realised_check": "0",
		"exit_batches": "2", "exit_batch_max_s": "30.2", "refused": "0", "refused_reduce_qty": "0", "residual_cycles": "0", "residual_bull": "0", "residual_bear": "0", "fingerprint": "print"} {
		if cell[k] != v {
			t.Errorf("%s = %q, want %q", k, cell[k], v)
		}
	}
}
