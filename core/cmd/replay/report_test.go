package main

import (
	"testing"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/store"
)

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }

// exampleSink is two cycles of one Session. Cycle A: entry lot A1 fills
// both legs (bull 10 at 40, bear 5 at 60: 700 USDT held), half of the bull
// leg is trimmed at 41, then the rest of both legs exits at the line. Cycle
// B: entry lot B1 fills the bull leg only and is trimmed back (one_leg).
func exampleSink() *memSink {
	s := newMemSink(false)
	order := func(id, cycle, lot, leg, purpose, side, qty, cum, value, fee string, at int64) {
		s.Record(store.OrderRow{ClientOrderID: id, CycleID: cycle, LotID: lot, Leg: leg, Purpose: purpose, Side: side, Qty: d(qty), CumExecQty: d(cum), CumExecValue: d(value), Fee: d(fee), Status: "filled", SubmittedMs: at})
	}
	fill := func(id, order, side, price, qty, fee string, at int64) {
		p, q := d(price), d(qty)
		s.Record(store.FillRow{ExecID: id, ClientOrderID: order, Side: side, Price: p, Qty: q, Value: p.Mul(q), Fee: d(fee), ExecMs: at})
	}
	order("a1", "cA", "cA-1", "bull", "entry", "buy", "10", "10", "400", "0.026", 1000)
	order("a2", "cA", "cA-1", "bear", "entry", "buy", "5", "5", "300", "0.0195", 1000)
	fill("fa1", "a1", "buy", "40", "10", "0.026", 1015)
	fill("fa2", "a2", "buy", "60", "5", "0.0195", 1015)
	order("a3", "cA", "cA-1", "bull", "trim", "sell", "5", "5", "205", "0.013325", 2000)
	fill("fa3", "a3", "sell", "41", "5", "0.013325", 2015)
	order("a4", "cA", "", "bull", "exit", "sell", "5", "5", "210", "0.01365", 3000)
	order("a5", "cA", "", "bear", "exit", "sell", "5", "5", "305", "0.019825", 3000)
	fill("fa4", "a4", "sell", "42", "5", "0.01365", 3015)
	fill("fa5", "a5", "sell", "61", "5", "0.019825", 3015)
	order("b1", "cB", "cB-1", "bull", "entry", "buy", "10", "4", "160", "0.0104", 4000)
	order("b2", "cB", "cB-1", "bear", "entry", "buy", "5", "0", "0", "0", 4000)
	fill("fb1", "b1", "buy", "40", "4", "0.0104", 4015)
	order("b3", "cB", "cB-1", "bull", "trim", "sell", "4", "4", "159.6", "0.010374", 4100)
	fill("fb3", "b3", "sell", "39.9", "4", "0.010374", 4115)
	s.Record(store.CycleRow{CycleID: "cA", SessionID: "2026-11-25", Status: "closed", EndReason: "exit_line", Net: d("19.8"), Fees: d("0.0923")},
		store.CycleRow{CycleID: "cB", SessionID: "2026-11-25", Status: "closed", EndReason: "one_leg", Net: d("-0.420774"), Fees: d("0.020774")})
	s.Record(store.LotRow{LotID: "cA-1", CycleID: "cA", Status: "closed"}, store.LotRow{LotID: "cB-1", CycleID: "cB", Status: "void", VoidReason: "one_leg"})
	return s
}

// The summary of a run: the round-trip net (the cycles') and the full cash
// account (every fill) kept apart, the peak notional held at entry prices,
// and the return on N and on the peak actually used when it stays below N
// (owner item 69).
func TestSummaryOfARun(t *testing.T) {
	s := summarize(exampleSink(), "", d("10000"))
	if !s.RoundTripNet.Equal(d("19.379226")) {
		t.Errorf("round-trip net %s, want 19.8 - 0.420774", s.RoundTripNet)
	}
	// Sells 205 + 210 + 305 + 159.6, buys 400 + 300 + 160, fees 0.113074.
	if !s.CashNet.Equal(d("19.486926")) || !s.Fees.Equal(d("0.113074")) {
		t.Errorf("cash net %s, fees %s; want 19.486926 and 0.113074", s.CashNet, s.Fees)
	}
	if !s.PeakNotional.Equal(d("700")) {
		t.Errorf("peak notional %s, want 700 (both legs of lot A1)", s.PeakNotional)
	}
	if !s.UsedNotional.Equal(d("700")) || !s.ReturnOnN.Equal(d("0.0019379226")) || !s.ReturnOnUsed.Equal(s.RoundTripNet.Div(d("700"))) {
		t.Errorf("used %s, return on N %s, on used %s", s.UsedNotional, s.ReturnOnN, s.ReturnOnUsed)
	}
	if s.EntryBatches != 2 || s.BothLegs != 1 || s.OneLeg != 1 || s.NoFill != 0 || s.OneLegRate != 0.5 {
		t.Errorf("batches %d, both %d, one leg %d, none %d, one-leg rate %v", s.EntryBatches, s.BothLegs, s.OneLeg, s.NoFill, s.OneLegRate)
	}
	// Entry orders: 30 asked, 19 filled.
	if s.EntryFillRate != 19.0/30.0 || s.Trims != 2 || s.Completions != 0 || s.Orders != 8 || s.Fills != 7 {
		t.Errorf("fill rate %v, trims %d, completions %d, orders %d, fills %d", s.EntryFillRate, s.Trims, s.Completions, s.Orders, s.Fills)
	}
	if s.Cycles != 2 || s.EndReasons["exit_line"] != 1 || s.EndReasons["one_leg"] != 1 {
		t.Errorf("cycles %d, end reasons %v", s.Cycles, s.EndReasons)
	}
	if !s.OpenQty[0].IsZero() || !s.OpenQty[1].IsZero() {
		t.Errorf("open quantity %v, want flat", s.OpenQty)
	}
	// A peak above N is capped at N for the return.
	if big := summarize(exampleSink(), "", d("500")); !big.UsedNotional.Equal(d("500")) || !big.ReturnOnUsed.Equal(big.ReturnOnN) {
		t.Errorf("with N 500: used %s, return on used %s, on N %s", big.UsedNotional, big.ReturnOnUsed, big.ReturnOnN)
	}
	// Another Session has nothing.
	if other := summarize(exampleSink(), "2026-11-26", d("10000")); other.Cycles != 0 || other.Orders != 0 || !other.CashNet.IsZero() {
		t.Errorf("another Session: %+v", other)
	}
}
