package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/store"
)

// summary is a run's results as PRD 16.5 lists them, for one Session or
// the whole window: the orders and fills, the entry batches by how their two
// legs filled, the trims and completions, the fees, the cycles by end
// reason, the round-trip net (the closed cycles' net: closed lots only)
// and the full cash account (every fill: sells - buys - fees) apart, the
// peak notional held and the returns.
type summary struct {
	Session string `json:"session,omitempty"`

	Orders          int            `json:"orders"`
	OrdersByPurpose map[string]int `json:"orders_by_purpose"`
	Fills           int            `json:"fills"`

	EntryBatches  int     `json:"entry_batches"`   // lots, each one entry batch
	BothLegs      int     `json:"both_legs"`       // batches whose two legs both filled something
	OneLeg        int     `json:"one_leg"`         // batches of which one leg alone filled
	NoFill        int     `json:"no_fill"`         // batches of which neither leg filled
	OneLegRate    float64 `json:"one_leg_rate"`    // one_leg / (both_legs + one_leg)
	EntryFillRate float64 `json:"entry_fill_rate"` // quantity filled / quantity asked, over the entry orders
	Completions   int     `json:"completions"`
	Trims         int     `json:"trims"`

	Fees         decimal.Decimal    `json:"fees"`
	RoundTripNet decimal.Decimal    `json:"round_trip_net"` // the closed cycles' net
	CashNet      decimal.Decimal    `json:"cash_net"`       // every fill: sells - buys - fees
	OpenQty      [2]decimal.Decimal `json:"open_qty"`       // by leg, signed: what the fills leave held (dust)

	Cycles     int            `json:"cycles"`
	EndReasons map[string]int `json:"end_reasons"`

	Notional     decimal.Decimal `json:"notional"`      // N = M x L
	PeakNotional decimal.Decimal `json:"peak_notional"` // the most held at once, both legs at their entry prices
	UsedNotional decimal.Decimal `json:"used_notional"` // min(N, peak): what the return is taken on (owner item 69)
	ReturnOnN    decimal.Decimal `json:"return_on_n"`
	ReturnOnUsed decimal.Decimal `json:"return_on_used"`
}

// summarize computes the summary of the sink's records of one Session ("":
// all), with N the run's total notional.
func summarize(s *memSink, session string, notional decimal.Decimal) summary {
	out := summary{Session: session, OrdersByPurpose: map[string]int{}, EndReasons: map[string]int{}, Notional: notional}
	inSession := map[string]bool{}
	for _, id := range s.cycleSeq {
		c := s.cycles[id]
		if session != "" && c.SessionID != session {
			continue
		}
		inSession[id] = true
		out.Cycles++
		if c.Status == "closed" {
			out.RoundTripNet = out.RoundTripNet.Add(c.Net)
			out.EndReasons[c.EndReason]++
		} else {
			out.EndReasons["open:"+c.Status]++
		}
	}

	// The orders, and the entry batches by lot.
	type legs struct{ asked, filled [2]bool }
	batches := map[string]*legs{}
	var batchOrder []string
	sides := map[string]string{} // client id -> side
	var asked, filled decimal.Decimal
	for _, id := range s.orderSeq {
		o := s.orders[id]
		if !inSession[o.CycleID] {
			continue
		}
		out.Orders++
		out.OrdersByPurpose[o.Purpose]++
		sides[id] = o.Side
		switch o.Purpose {
		case "trim":
			out.Trims++
		case "complete":
			out.Completions++
		case "entry":
			asked, filled = asked.Add(o.Qty), filled.Add(o.CumExecQty)
			b := batches[o.LotID]
			if b == nil {
				b = &legs{}
				batches[o.LotID] = b
				batchOrder = append(batchOrder, o.LotID)
			}
			leg := legIndex(o.Leg)
			b.asked[leg] = true
			b.filled[leg] = b.filled[leg] || o.CumExecQty.IsPositive()
		}
	}
	for _, lot := range batchOrder {
		b := batches[lot]
		out.EntryBatches++
		switch {
		case b.filled[0] && b.filled[1]:
			out.BothLegs++
		case b.filled[0] || b.filled[1]:
			out.OneLeg++
		default:
			out.NoFill++
		}
	}
	if n := out.BothLegs + out.OneLeg; n > 0 {
		out.OneLegRate = float64(out.OneLeg) / float64(n)
	}
	if asked.IsPositive() {
		out.EntryFillRate = filled.Div(asked).InexactFloat64()
	}

	// The fills: the cash account and the notional held.
	var fills []store.FillRow
	legOf := map[string]int{}
	for _, id := range s.orderSeq {
		legOf[id] = legIndex(s.orders[id].Leg)
	}
	for _, id := range s.fillSeq {
		f := s.fills[id]
		if _, ok := sides[f.ClientOrderID]; ok {
			fills = append(fills, f)
		}
	}
	sort.SliceStable(fills, func(i, j int) bool { return fills[i].ExecMs < fills[j].ExecMs })
	var qty, cost [2]decimal.Decimal // by leg: signed quantity held, its entry cost
	for _, f := range fills {
		out.Fills++
		out.Fees = out.Fees.Add(f.Fee)
		signed := f.Qty
		if f.Side == "sell" {
			out.CashNet = out.CashNet.Add(f.Value)
			signed = signed.Neg()
		} else {
			out.CashNet = out.CashNet.Sub(f.Value)
		}
		leg := legOf[f.ClientOrderID]
		switch {
		case qty[leg].IsZero() || qty[leg].Sign() == signed.Sign():
			qty[leg], cost[leg] = qty[leg].Add(signed), cost[leg].Add(f.Value)
		case signed.Abs().LessThanOrEqual(qty[leg].Abs()):
			cost[leg] = cost[leg].Sub(cost[leg].Mul(signed.Abs()).Div(qty[leg].Abs()))
			qty[leg] = qty[leg].Add(signed)
		default: // through flat: the rest opens the other side at this price
			qty[leg] = qty[leg].Add(signed)
			cost[leg] = qty[leg].Abs().Mul(f.Price)
		}
		if held := cost[0].Add(cost[1]); held.GreaterThan(out.PeakNotional) {
			out.PeakNotional = held
		}
	}
	out.CashNet = out.CashNet.Sub(out.Fees)
	out.OpenQty = qty

	out.UsedNotional = decimal.Min(out.PeakNotional, notional)
	if notional.IsPositive() {
		out.ReturnOnN = out.RoundTripNet.Div(notional)
	}
	if out.UsedNotional.IsPositive() {
		out.ReturnOnUsed = out.RoundTripNet.Div(out.UsedNotional)
	}
	return out
}

// legIndex is 0 for the bull leg and 1 for the bear leg.
func legIndex(leg string) int {
	if leg == "bear" {
		return 1
	}
	return 0
}

// sessionsOf lists the Sessions the sink's cycles belong to, in order.
func sessionsOf(s *memSink) []string {
	seen := map[string]bool{}
	var out []string
	for _, id := range s.cycleSeq {
		if c := s.cycles[id]; !seen[c.SessionID] {
			seen[c.SessionID] = true
			out = append(out, c.SessionID)
		}
	}
	return out
}

// fingerprint is a hash of everything the run decided and booked: every
// order, fill, cycle and lot as last recorded and every decision, in the
// order they were first recorded, written with fmt (NaN included). Two runs of the same input are the same
// run when their fingerprints are equal.
func fingerprint(s *memSink) string {
	h := sha256.New()
	for _, id := range s.orderSeq {
		fmt.Fprintf(h, "%+v\n", s.orders[id])
	}
	for _, id := range s.fillSeq {
		fmt.Fprintf(h, "%+v\n", s.fills[id])
	}
	for _, id := range s.cycleSeq {
		fmt.Fprintf(h, "%+v\n", s.cycles[id])
	}
	for _, id := range s.lotSeq {
		fmt.Fprintf(h, "%+v\n", s.lots[id])
	}
	for _, d := range s.decisions {
		fmt.Fprintf(h, "%d %s %s %s %s %s %s %s\n", d.TsMs, d.CycleID, d.LotID, d.Kind, d.Direction, d.Reason, d.Detail, "")
	}
	return hex.EncodeToString(h.Sum(nil))
}
