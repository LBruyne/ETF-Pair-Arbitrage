package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"time"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/store"
)

// summary is a run's results as PRD 16.5 lists them, for one Session or
// the whole window: the orders and fills, the entry batches by how their two
// legs filled, the trims and completions, the fees, the cycles by end
// reason, the round-trip net (the closed cycles' net: closed lots only)
// and the full cash account (every fill: sells - buys - fees) apart, the
// peak notional held and the returns. Parameters are ranked on the
// window's result (WindowNet, WindowReturnOnUsed), which also counts a
// position still held at the window's end; the round-trip net counts the
// closed cycles only.
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

	Notional     decimal.Decimal `json:"notional"`       // N = M x L
	PeakNotional decimal.Decimal `json:"peak_notional"`  // the most the ledger held at once: its lots' open quantities at their entry prices (first in, first out) and the dust, without pending orders
	UsedNotional decimal.Decimal `json:"used_notional"`  // min(N, peak): what the return is taken on (owner item 69)
	ReturnOnN    decimal.Decimal `json:"return_on_n"`    // round-trip net / N
	ReturnOnUsed decimal.Decimal `json:"return_on_used"` // round-trip net / used

	// The window's result, for ranking: the cash net plus, for a position
	// left at the end, what closing it on the books there would bring, less
	// the closing fees. WindowValue says how it was valued; without a value
	// the three numbers are zero.
	WindowValue        string          `json:"window_value"` // valueFlat, valueOnBook, valueOnBooks50 or valueNone
	WindowNet          decimal.Decimal `json:"window_net"`
	WindowReturnOnN    decimal.Decimal `json:"window_return_on_n"`
	WindowReturnOnUsed decimal.Decimal `json:"window_return_on_used"`
}

// How a summary's window result was valued.
const (
	valueFlat      = "flat"              // nothing held at the end: the cash net
	valueOnBook    = "closed_on_book"    // what was held, closed on the books the program had at the end
	valueOnBooks50 = "closed_on_books50" // as valueOnBook, a leg on the deeper books50 recorded at the end
	valueNone      = "no_value"          // held at the end and the books there cannot close it
)

// closeout is what values a position left at the window's end: both legs'
// books at that moment, the taker fee of the closing orders, and the
// deeper book recorded then. The program prices SOXS on books5, whose five
// levels at a 0.001 tick hold a few shares (10.93 asks at 2026-11-26
// 12:00): deep gives a leg's whole books50 received within depth_max_age
// before the end, read only when the program's books cannot take the
// quantity; ok is false when there is none.
type closeout struct {
	books   cex.Books
	at      time.Time
	feeRate decimal.Decimal // of the traded value
	deep    func(leg cex.Leg) (bids, asks []cex.BookLevel, ok bool)
}

// value returns what closing the signed quantities brings, fees deducted
// (negative for buying back a short), and how it was valued: on the
// program's books, or for a leg they cannot take on the deeper books50,
// or valueNone when a leg's depth is not usable at the end, is in conflict
// with a newer best price, or neither book takes the quantity. A fee is cut
// to 8 decimals as the venue and the paper venue cut them.
func (c *closeout) value(open [2]decimal.Decimal) (decimal.Decimal, string) {
	total, how := decimal.Zero, valueOnBook
	for leg, qty := range open {
		if qty.IsZero() {
			continue
		}
		if !c.books.DepthOK(cex.Leg(leg), c.at) {
			return decimal.Zero, valueNone
		}
		sideOf := func(bids, asks []cex.BookLevel) []cex.BookLevel {
			if qty.IsNegative() {
				return asks // a short is bought back at the asks
			}
			return bids // a long is sold at the bids
		}
		value, ok := sweep(sideOf(c.books.ByLeg[leg].Bids, c.books.ByLeg[leg].Asks), qty.Abs())
		if !ok && c.deep != nil {
			if bids, asks, found := c.deep(cex.Leg(leg)); found {
				if value, ok = sweep(sideOf(bids, asks), qty.Abs()); ok {
					how = valueOnBooks50
				}
			}
		}
		if !ok {
			return decimal.Zero, valueNone
		}
		if qty.IsNegative() {
			value = value.Neg()
		}
		total = total.Add(value).Sub(value.Abs().Mul(c.feeRate).Truncate(feeDecimals))
	}
	return total, how
}

// sweep is the value of taking qty from the levels, best first, and false
// when they hold less.
func sweep(levels []cex.BookLevel, qty decimal.Decimal) (decimal.Decimal, bool) {
	value := decimal.Zero
	for _, l := range levels {
		take := decimal.Min(qty, l.Qty)
		value, qty = value.Add(take.Mul(l.Price)), qty.Sub(take)
		if qty.IsZero() {
			return value, true
		}
	}
	return value, false
}

// feeDecimals: fees are cut to 8 decimals (the venue's order-info and fills).
const feeDecimals = 8

// summarize computes the summary of the sink's records of one Session ("":
// all), with N the run's total notional and peak the most the ledger held
// in it. end values a position left at the end; nil when the summary does
// not end at the window's end.
func summarize(s *memSink, session string, notional, peak decimal.Decimal, end *closeout) summary {
	out := summary{Session: session, OrdersByPurpose: map[string]int{}, EndReasons: map[string]int{}, Notional: notional, PeakNotional: peak}
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

	// The fills: the cash account and what they leave held.
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
	var qty [2]decimal.Decimal // by leg: signed quantity held
	for _, f := range fills {
		out.Fills++
		out.Fees = out.Fees.Add(f.Fee)
		leg := legOf[f.ClientOrderID]
		if f.Side == "sell" {
			out.CashNet, qty[leg] = out.CashNet.Add(f.Value), qty[leg].Sub(f.Qty)
		} else {
			out.CashNet, qty[leg] = out.CashNet.Sub(f.Value), qty[leg].Add(f.Qty)
		}
	}
	out.CashNet = out.CashNet.Sub(out.Fees)
	out.OpenQty = qty

	out.WindowValue, out.WindowNet = valueFlat, out.CashNet
	if !qty[0].IsZero() || !qty[1].IsZero() {
		closing, how := decimal.Zero, valueNone
		if end != nil {
			closing, how = end.value(qty)
		}
		out.WindowValue, out.WindowNet = how, out.CashNet.Add(closing)
		if how == valueNone {
			out.WindowNet = decimal.Zero
		}
	}

	out.UsedNotional = decimal.Min(out.PeakNotional, notional)
	if notional.IsPositive() {
		out.ReturnOnN, out.WindowReturnOnN = out.RoundTripNet.Div(notional), out.WindowNet.Div(notional)
	}
	if out.UsedNotional.IsPositive() {
		out.ReturnOnUsed, out.WindowReturnOnUsed = out.RoundTripNet.Div(out.UsedNotional), out.WindowNet.Div(out.UsedNotional)
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
