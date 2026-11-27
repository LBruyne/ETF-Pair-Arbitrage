package exec

import (
	"fmt"
	"syscall"
	"testing"
	"time"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/inventory"
	"etf-pair-arbitrage/core/store"
	"etf-pair-arbitrage/core/strategy"
)

// The thin leg first (core/SPEC.md 12.4, PRD 5.14-5.19) on the rig: thin_leg
// bear, so SOXS is sent alone and SOXL follows its fills. The tests use the
// rig's books: example 1 of PRD appendix A (entryBook) signals a short at x
// = +10 under mu = 3.3333, sigma = 4; the batch plans SOXS 70.20 @ 59.420
// and SOXL 105.30 @ 40.44. SOXS's sell fills 0.4 @ 59.437 + 60 @ 59.431 +
// 9.8 @ 59.428 = 4,172.0292 (VWAP 59.43061). The batch's bound P* for SOXL
// (a short: the least SOXL's VWAP may come to) is a_L x (2 + D_need / 10^4
// - v_S / a_S) with D_need = mu + entry_out x sigma + C = 1.3333 + 2.6017:
// 40.3953.

// thinRig is a rig with SOXS first, warmed up: mu = 3.3333, sigma = 4.
func thinRig(t *testing.T, opts rigOptions) *rig {
	t.Helper()
	opts.thin = "bear"
	r := newRig(t, utc8Time(11, 25, 5, 1, 0), opts)
	r.warmUp()
	return r
}

// pairRows are the loop's decisions of kind pair, in order.
func (r *rig) pairRows(kind string) []store.DecisionRow {
	var out []store.DecisionRow
	for _, d := range r.decisions() {
		if d.Kind == kind {
			out = append(out, d)
		}
	}
	return out
}

// actions lists the detail's action of the decisions of kind.
func (r *rig) actions(kind string) (out []string) {
	for _, d := range r.pairRows(kind) {
		a, _ := decodeDetail(r.t, d.Detail)["action"].(string)
		out = append(out, a)
	}
	return out
}

// lotNow is the open cycle's lot number seq as the ledger holds it now.
func (r *rig) lotNow(seq int) *inventory.Lot {
	r.t.Helper()
	c := r.inv.Cycle()
	if c == nil || len(c.Lots) < seq {
		r.t.Fatalf("no lot %d in the open cycle", seq)
	}
	return c.Lots[seq-1]
}

// wantOrder fails unless o is on leg for purpose, side, qty at price.
func wantOrder(t *testing.T, what string, o cex.OrderRecord, leg cex.Leg, purpose cex.OrderPurpose, side cex.Side, qty, price string) {
	t.Helper()
	q := o.Request
	if q.Leg != leg || q.Purpose != purpose || q.Side != side || !q.Qty.Equal(dec(qty)) || !q.Price.Equal(dec(price)) {
		t.Errorf("%s: %s %s %s %s @ %s, want %s %s %s %s @ %s", what, q.Leg, q.Purpose, q.Side, q.Qty, q.Price, leg, purpose, side, qty, price)
	}
}

// awayBook is example 1's book with SOXS's bids below the thin order's
// limit 59.420: a sell there fills nothing.
func awayBook() pairBook {
	b := entryBook()
	b.bearBids = side{lv("59.30", "50"), lv("59.29", "50"), lv("59.28", "50"), lv("59.27", "50"), lv("59.26", "50")}
	return b
}

// SPEC 12.4: the thin order goes alone; the round that books its fill sends
// the first thick order at once (no 200 ms wait, speed 1 of version 4); the
// thick order is within P* (40.45 >= 40.3953) and pairs T = 70.20 x 60 / 40
// = 105.30; the batch then ends as built. The spacing is set by the thin
// order's batch.
func TestThinFirstEntryAllFilled(t *testing.T) {
	r := thinRig(t, rigOptions{})
	t0 := utc8Time(11, 25, 5, 3, 5)
	r.at(t0)
	r.books(entryBook())
	r.stepAt(t0)
	if o := r.orders(); len(o) != 1 {
		t.Fatalf("%d orders sent, want SOXS's alone", len(o))
	} else {
		wantOrder(t, "thin", o[0], cex.Bear, cex.PurposeEntry, cex.SideSell, "70.2", "59.42")
	}
	if r.m.lastEntry == nil || !r.m.lastEntry.At.Equal(t0) || r.m.lastEntry.E != detailOf(t, r.decisions()[0])["x_d"].(float64) {
		t.Errorf("last entry %+v, want the thin order's batch at %s", r.m.lastEntry, showTime(t0))
	}

	r.results()
	r.step() // books SOXS's fill: the thick order goes in this round
	o := r.orders()
	if len(o) != 2 {
		t.Fatalf("%d orders after the thin fill's round, want the thick order sent in it", len(o))
	}
	wantOrder(t, "thick", o[1], cex.Bull, cex.PurposeComplete, cex.SideSell, "105.3", "40.45")
	if !o[1].SubmittedAt.After(o[0].FinalAt) && !o[1].SubmittedAt.Equal(o[0].FinalAt) {
		t.Errorf("thick submitted %s before the thin result %s", o[1].SubmittedAt, o[0].FinalAt)
	}
	sends := r.pairRows("pair")
	if len(sends) != 2 {
		t.Fatalf("pair decisions %v, want the fill and the first thick order", r.actions("pair"))
	}
	fill, thick := detailOf(t, sends[0]), detailOf(t, sends[1])
	if fill["action"] != "fill" || fill["thin_qty"] != "70.2" || fill["thin_final_ms"] == nil {
		t.Errorf("fill record %v", fill)
	}
	if thick["action"] != "send_thick" || thick["first_thick"] != true || thick["sent_ms"] == nil || thick["thin_final_ms"] == nil {
		t.Errorf("first thick record %v, want send_thick with its latency times", thick)
	}
	near(t, "P* recorded", dec(thick["vwap_cap"].(string)).InexactFloat64(), 40*(2+(10000*(40.40/40+59.42/60-2)-2+2*0.65*(40.44/40+59.420/60))/1e4-(4172.0292/70.2)/60))

	r.results()
	r.step() // books SOXL's fill: paired, built
	lot := r.lotNow(1)
	if lot.Status != inventory.LotOpen || lot.End != inventory.LotEndBuilt {
		t.Errorf("lot %s %s, want open built", lot.Status, lot.End)
	}
	if _, hold := r.inv.Holdings(); !hold[cex.Bull].Equal(dec("105.3")) || !hold[cex.Bear].Equal(dec("70.2")) {
		t.Errorf("held %v, want 105.30 / 70.20", hold)
	}
	if got := r.actions("pair"); len(got) != 3 || got[2] != "done" {
		t.Errorf("pair actions %v, want fill, send_thick, done", got)
	}
	if done := r.pairRows("pair")[2]; done.Reason != "" || detailOf(t, done)["end"] != "built" {
		t.Errorf("done %s %v", done.Reason, detailOf(t, done))
	}
	if row := r.lastLot(string(lot.ID)); row.EndState != "built" {
		t.Errorf("lot row end_state %q, want built", row.EndState)
	}
}

// detailOf decodes a decision's detail.
func detailOf(t *testing.T, d store.DecisionRow) map[string]any {
	t.Helper()
	return decodeDetail(t, d.Detail)
}

// SPEC 12.4 step 1: the thin order fills in part (the book moved before it
// arrived: SOXS's third bid is gone): S = 30.40 @ 59.43108; T = floor(30.40
// x 1.5) = 45.60 of SOXL, sent in the same round; built.
func TestThinFirstEntryPartlyFilled(t *testing.T) {
	r := thinRig(t, rigOptions{latency: 100 * time.Millisecond})
	t0 := utc8Time(11, 25, 5, 3, 5)
	r.at(t0)
	r.books(entryBook())
	r.stepAt(t0)
	moved := entryBook()
	moved.bearBids = side{lv("59.437", "0.4"), lv("59.431", "30"), lv("59.30", "30"), lv("59.29", "30"), lv("59.28", "20")}
	r.books(moved)
	r.results()
	r.step()
	o := r.orders()
	if len(o) != 2 || !o[0].CumExecQty.Equal(dec("30.4")) {
		t.Fatalf("orders %d, thin filled %s; want 30.40 and the thick order", len(o), o[0].CumExecQty)
	}
	wantOrder(t, "thick", o[1], cex.Bull, cex.PurposeComplete, cex.SideSell, "45.6", "40.45")
	r.results()
	r.step()
	if lot := r.lotNow(1); lot.End != inventory.LotEndBuilt || !lot.Legs[cex.Bull].EntryQty.Equal(dec("45.6")) || !lot.Legs[cex.Bear].EntryQty.Equal(dec("30.4")) {
		t.Errorf("lot %s %s / %s, want built 45.60 / 30.40", lot.End, lot.Legs[cex.Bull].EntryQty, lot.Legs[cex.Bear].EntryQty)
	}
}

// SPEC 12.4 step 1 and 12.3: a thin order that fills nothing ends the batch
// as entry_failed, which is the last entry all the same (owner item 93 point
// 5): the same signal 5 s later waits for the spacing. The next batch waits
// only for the thin leg's new depth (12.12 item 4): one deeper by 1 unit
// goes on a new SOXS depth with SOXL's older one.
func TestThinFirstEntryFailed(t *testing.T) {
	r := thinRig(t, rigOptions{latency: 100 * time.Millisecond})
	t0 := utc8Time(11, 25, 5, 3, 5)
	r.at(t0)
	r.books(entryBook())
	r.stepAt(t0)
	b := awayBook()
	r.depth(cex.Bear, b.bearBids, b.bearAsks) // SOXL's depth stays example 1's
	r.results()
	r.step()
	if len(r.orders()) != 1 {
		t.Fatalf("%d orders, want the thin order alone", len(r.orders()))
	}
	lot := r.lastLot(r.orders()[0].Request.LotID)
	if lot.Status != string(inventory.LotVoid) || lot.EndState != "entry_failed" || lot.VoidReason != "entry_failed" {
		t.Errorf("lot %s %s %s, want void entry_failed", lot.Status, lot.VoidReason, lot.EndState)
	}
	if r.m.lastEntry == nil || !r.m.lastEntry.At.Equal(t0) {
		t.Errorf("last entry %+v, want the failed batch at %s", r.m.lastEntry, showTime(t0))
	}
	r.stepAt(t0.Add(500 * time.Millisecond)) // the cycle closes
	if c := r.lastCycle(); r.inv.Cycle() != nil || c.EndReason != inventory.EndNoFill {
		t.Fatalf("cycle open %t, end %q; want closed no_fill", r.inv.Cycle() != nil, c.EndReason)
	}

	// The same signal on a new SOXS depth: x = 10 is not 1 deeper: spacing.
	t1 := t0.Add(time.Second)
	r.at(t1)
	r.depth(cex.Bear, entryBook().bearBids, entryBook().bearAsks)
	r.idleAt(t1, strategy.ReasonSpacing)

	// SOXS's fifth bid 59.427: x = 11.17 >= 10 + 1. SOXL's depth is from t0,
	// before the batch's result: only the thin leg's new depth is needed.
	t2 := t0.Add(2 * time.Second)
	r.at(t2)
	r.depth(cex.Bear, side{lv("59.44", "0.4"), lv("59.435", "60"), lv("59.43", "30"), lv("59.428", "30"), lv("59.427", "20")}, entryBook().bearAsks)
	before := len(r.orders())
	r.stepAt(t2)
	if got := r.orders(); len(got) != before+1 || got[before].Request.Leg != cex.Bear {
		t.Fatalf("orders %d -> %d (idle %q), want a new thin order", before, len(got), r.m.idleReason)
	}
}

// A refused thin order is entry_failed too, with the venue's reason in its
// record, and the next entry waits rejected_backoff.
func TestThinFirstEntryRejected(t *testing.T) {
	r := thinRig(t, rigOptions{})
	r.gate.setRefuse(func(req cex.OrderRequest) error {
		if req.Leg == cex.Bear {
			return &cex.VenueError{Code: "40762", Message: "the order amount exceeds the balance", Kind: cex.ErrorOther}
		}
		return nil
	})
	t0 := utc8Time(11, 25, 5, 3, 5)
	r.at(t0)
	r.books(entryBook())
	r.stepAt(t0)
	r.results()
	r.step()
	rows := r.pairRows("pair")
	if len(rows) != 1 {
		t.Fatalf("pair decisions %v, want one done", r.actions("pair"))
	}
	if d := detailOf(t, rows[0]); d["action"] != "done" || d["end"] != "entry_failed" || d["error_code"] != "40762" {
		t.Errorf("done %v, want entry_failed with the venue's code", d)
	}
	r.gate.setRefuse(nil)
	t1 := t0.Add(500 * time.Millisecond)
	r.at(t1)
	r.books(entryBook())
	r.idleAt(t1, strategy.ReasonRejectedBackoff)
}

// SPEC 12.4 step 1: while the thin order's outcome is unknown nothing else
// is sent; once it is resolved with a fill the batch pairs it.
func TestThinFirstEntryUnknownThenResolved(t *testing.T) {
	r := thinRig(t, rigOptions{})
	t0 := utc8Time(11, 25, 5, 3, 5)
	r.at(t0)
	r.books(entryBook())
	r.gate.park("SOXSUSDT")
	r.stepAt(t0)
	waitFor(t, "SOXS unknown", func() bool {
		u := r.cex.UnresolvedOrders()
		return len(u) == 1 && u[0].Unknown
	})
	for s := 1; s <= 3; s++ {
		r.at(t0.Add(time.Duration(s) * time.Second))
		r.books(entryBook())
		r.idleAt(t0.Add(time.Duration(s)*time.Second), strategy.ReasonOrderUnknown)
	}
	r.gate.releaseParked()
	waitFor(t, "SOXS's result", func() bool { return len(r.cex.UnresolvedOrders()) == 0 })
	r.stepAt(t0.Add(4 * time.Second))
	o := r.orders()
	if len(o) != 2 {
		t.Fatalf("%d orders, want the thick order once SOXS is resolved", len(o))
	}
	wantOrder(t, "thick", o[1], cex.Bull, cex.PurposeComplete, cex.SideSell, "105.3", "40.45")
}

// withBull is example 1's book with SOXL's bids replaced.
func withBull(bids ...cex.BookLevel) pairBook {
	b := entryBook()
	b.bullBids = side(bids)
	return b
}

// thinFilled sends example 1's thin order at t0 and books its fill (70.20 @
// 59.43061); SOXL's depth is then after's, which the round that books the
// fill runs PairStep on (the decision needs example 1's second bid 40.44).
func (r *rig) thinFilled(t0 time.Time, after pairBook) {
	r.t.Helper()
	r.at(t0)
	r.books(entryBook())
	r.stepAt(t0)
	r.depth(cex.Bull, after.bullBids, after.bullAsks)
	r.results()
	r.step()
}

// SPEC 12.4 step 5, several thick orders and a partial IOC: the first thick
// order pairs 50 of 105.30 at 40.45 (the second bid 40.30 is beyond P* and
// nothing filled earlier leaves room). A repeat on SOXL waits 200 ms and a
// new SOXL frame after the last result (12.12 item 3). The second order, 30
// @ 40.43, meets a book that holds 10 of it: L = 60. The third pairs the
// rest, 45.30 @ 40.42: built. pair_stop_d is 50 here: SOXL's second bid
// 40.30 would put D_ideal 33 below D_dec.
func TestThinFirstSeveralThickFills(t *testing.T) {
	r := thinRig(t, rigOptions{latency: 100 * time.Millisecond, stopD: 50})
	t0 := utc8Time(11, 25, 5, 3, 5)
	r.thinFilled(t0, withBull(lv("40.45", "50"), lv("40.30", "500")))
	r.results()
	r.step()
	o := r.orders()
	wantOrder(t, "first thick", o[1], cex.Bull, cex.PurposeComplete, cex.SideSell, "50", "40.45")
	if got := r.lotNow(1).Legs[cex.Bull].EntryQty; !got.Equal(dec("50")) {
		t.Fatalf("SOXL paired %s, want 50", got)
	}
	if r.m.idleReason != strategy.ReasonAwaitOrders {
		t.Errorf("idle %q right after the result, want await_orders (200 ms)", r.m.idleReason)
	}
	r.idleAt(t0.Add(300*time.Millisecond), strategy.ReasonAwaitDepth) // no SOXL frame since the result

	t1 := t0.Add(400 * time.Millisecond)
	r.at(t1)
	b := withBull(lv("40.43", "30"), lv("40.30", "500"))
	r.depth(cex.Bull, b.bullBids, b.bullAsks)
	r.stepAt(t1)
	thinner := withBull(lv("40.43", "10"), lv("40.30", "500"))
	r.depth(cex.Bull, thinner.bullBids, thinner.bullAsks) // before the order arrives
	r.results()
	r.step()
	o = r.orders()
	wantOrder(t, "second thick", o[2], cex.Bull, cex.PurposeComplete, cex.SideSell, "30", "40.43")
	if !o[2].CumExecQty.Equal(dec("10")) {
		t.Fatalf("second thick filled %s, want 10 of 30", o[2].CumExecQty)
	}

	t2 := t1.Add(300 * time.Millisecond)
	r.at(t2)
	last := withBull(lv("40.42", "100"), lv("40.30", "500"))
	r.depth(cex.Bull, last.bullBids, last.bullAsks)
	r.stepAt(t2)
	r.results()
	r.step()
	o = r.orders()
	wantOrder(t, "third thick", o[3], cex.Bull, cex.PurposeComplete, cex.SideSell, "45.3", "40.42")
	lot := r.lotNow(1)
	if lot.End != inventory.LotEndBuilt || !lot.Legs[cex.Bull].EntryQty.Equal(dec("105.3")) {
		t.Errorf("lot %s with SOXL %s, want built 105.30", lot.End, lot.Legs[cex.Bull].EntryQty)
	}
	firsts := 0
	for _, d := range r.pairRows("pair") {
		if detailOf(t, d)["first_thick"] == true {
			firsts++
		}
	}
	if firsts != 1 {
		t.Errorf("%d thick orders marked first, want 1", firsts)
	}
}

// SPEC 12.4 step 4 and PRD 5.18: SOXL's bids fall before the thin fill is
// booked (second bid 40.39: D_ideal = 2.60, 10.73 below D_dec = 13.33): the
// unpaired thin leg is unwound at PRD 10.4's price (SOXS's best ask 59.445
// through 10 bps, up: 59.505) once 200 ms passed since the thin order (two
// orders of one leg, PRD 5.18), and again 200 ms later for what the first
// unwind left (the book held 30): unwound_stop, and the cycle ends one_leg.
func TestThinFirstUnwindLoop(t *testing.T) {
	r := thinRig(t, rigOptions{latency: 100 * time.Millisecond})
	t0 := utc8Time(11, 25, 5, 3, 5)
	r.at(t0)
	r.books(entryBook())
	r.stepAt(t0)
	fallen := withBull(lv("40.39", "300"), lv("40.39", "500"))
	r.depth(cex.Bull, fallen.bullBids, fallen.bullAsks)
	r.results()
	r.step() // the thin fill and the stop; the unwind waits 200 ms after the thin order
	if o := r.orders(); len(o) != 1 || r.m.idleReason != strategy.ReasonAwaitOrders {
		t.Fatalf("%d orders, idle %q; want the unwind held for 200 ms", len(o), r.m.idleReason)
	}
	t0 = t0.Add(200 * time.Millisecond)
	r.stepAt(t0)
	o := r.orders()
	if len(o) != 2 {
		t.Fatalf("%d orders, want the thin order and the unwind", len(o))
	}
	wantOrder(t, "unwind", o[1], cex.Bear, cex.PurposeTrim, cex.SideBuy, "70.2", "59.505")
	if !o[1].Request.ReduceOnly {
		t.Error("the unwind is not reduce-only")
	}
	shallow := entryBook()
	shallow.bearAsks = side{lv("59.445", "30"), lv("59.60", "50"), lv("59.61", "50"), lv("59.62", "50"), lv("59.63", "50")}
	r.depth(cex.Bear, shallow.bearBids, shallow.bearAsks)
	r.results()
	r.step()
	if got := r.orders()[1].CumExecQty; !got.Equal(dec("30")) {
		t.Fatalf("first unwind filled %s, want 30", got)
	}
	if r.m.idleReason != strategy.ReasonAwaitOrders {
		t.Errorf("idle %q, want await_orders until 200 ms after the unwind", r.m.idleReason)
	}
	t1 := t0.Add(300 * time.Millisecond)
	r.at(t1)
	r.depth(cex.Bear, entryBook().bearBids, entryBook().bearAsks)
	r.stepAt(t1)
	r.results()
	r.step()
	o = r.orders()
	wantOrder(t, "second unwind", o[2], cex.Bear, cex.PurposeTrim, cex.SideBuy, "40.2", "59.505")
	if got := r.actions("pair"); len(got) != 5 || got[1] != "risk_exit" || got[4] != "done" {
		t.Errorf("pair actions %v, want fill, risk_exit, unwind, unwind, done", got)
	}
	done := r.pairRows("pair")[4]
	if detailOf(t, done)["end"] != "unwound_stop" || r.pairRows("pair")[1].Reason != "stop_d" {
		t.Errorf("done %v, risk exit reason %q; want unwound_stop after stop_d", detailOf(t, done), r.pairRows("pair")[1].Reason)
	}
	r.stepAt(t1.Add(time.Second))
	if c := r.lastCycle(); r.inv.Cycle() != nil || c.EndReason != inventory.EndOneLeg {
		t.Errorf("cycle open %t, end %q; want closed one_leg", r.inv.Cycle() != nil, c.EndReason)
	}
}

// SPEC 12.4 step 3: the window is checked on the loop's tick, with no new
// frame. pair_stop_d is 20 here, so SOXL's bids 40.39 / 40.38 (below P*,
// above the stop) leave the batch waiting; the round 30 s after the thin
// fill unwinds it for the window.
func TestThinFirstWindowEndsOnTick(t *testing.T) {
	r := thinRig(t, rigOptions{stopD: 20})
	t0 := utc8Time(11, 25, 5, 3, 5)
	r.thinFilled(t0, withBull(lv("40.39", "300"), lv("40.38", "500")))
	if r.m.idleReason != strategy.ReasonPairBound {
		t.Fatalf("idle %q, want pair_bound", r.m.idleReason)
	}
	r.at(t0.Add(25 * time.Second))
	r.books(withBull(lv("40.39", "300"), lv("40.38", "500"))) // the last frames
	r.idleAt(t0.Add(25*time.Second), strategy.ReasonPairBound)
	r.idleAt(t0.Add(30*time.Second-time.Millisecond), strategy.ReasonPairBound)
	before := len(r.orders())
	r.stepAt(t0.Add(30 * time.Second)) // a tick: nothing new arrived
	o := r.orders()
	if len(o) != before+1 {
		t.Fatalf("no unwind on the tick at the window's end; idle %q", r.m.idleReason)
	}
	wantOrder(t, "unwind", o[before], cex.Bear, cex.PurposeTrim, cex.SideBuy, "70.2", "59.505")
	if rows := r.pairRows("pair"); rows[len(rows)-2].Reason != "window" {
		t.Errorf("risk exit reason %q, want window", rows[len(rows)-2].Reason)
	}
}

// SPEC 12.4 step 2 and step 6: the stop takes the round over during the
// pairing (the forced exit buys SOXS's 70.20 back and the batch ends as it
// stands: unwound_stop, reason stopping); the signal that stops new entries
// only stops top-ups, the pairing goes on; the close window takes the round
// over too (reason forced).
func TestThinFirstTakeovers(t *testing.T) {
	waiting := withBull(lv("40.39", "300"), lv("40.38", "500")) // below P*, above the stop of 20
	t0 := utc8Time(11, 25, 5, 3, 5)

	t.Run("stop", func(t *testing.T) {
		r := thinRig(t, rigOptions{stopD: 20})
		r.thinFilled(t0, waiting)
		r.m.onSignal(syscall.SIGTERM, t0.Add(time.Second))
		r.trade(t0.Add(time.Second))
		o := r.orders()
		wantOrder(t, "forced", o[len(o)-1], cex.Bear, cex.PurposeForced, cex.SideBuy, "70.2", "59.505")
		rows := r.pairRows("pair")
		last := rows[len(rows)-1]
		if d := detailOf(t, last); last.Reason != "stopping" || d["end"] != "unwound_stop" || d["hand_over"] != true {
			t.Errorf("hand-over %s %v, want unwound_stop for stopping", last.Reason, d)
		}
		if !r.stepAt(t0.Add(2 * time.Second)) {
			t.Fatalf("did not leave once flat; idle %q", r.m.idleReason)
		}
		// Its only batch never paired: the cycle ends one_leg, as a void
		// batch of both legs together does whatever closed it.
		if c := r.lastCycle(); c.EndReason != inventory.EndOneLeg {
			t.Errorf("cycle end %q, want one_leg", c.EndReason)
		}
	})

	t.Run("no new entries", func(t *testing.T) {
		r := thinRig(t, rigOptions{stopD: 20})
		r.thinFilled(t0, waiting)
		r.m.onSignal(syscall.SIGUSR1, t0)
		// x deeper than x_dec (SOXS's fifth bid 59.427, x = 11.17): a top-up
		// would go, but not after the signal.
		deeper := waiting
		deeper.bearBids = side{lv("59.44", "0.4"), lv("59.435", "60"), lv("59.43", "30"), lv("59.428", "30"), lv("59.427", "20")}
		t1 := t0.Add(time.Second)
		r.at(t1)
		r.books(deeper)
		r.idleAt(t1, strategy.ReasonPairBound)
		t2 := t0.Add(2 * time.Second)
		r.at(t2)
		r.books(entryBook())
		r.trade(t2)
		if lot := r.lotNow(1); lot.End != inventory.LotEndBuilt {
			t.Errorf("lot %s, want built: the pairing goes on after no_new_entries", lot.End)
		}
	})

	t.Run("close window", func(t *testing.T) {
		r := thinRig(t, rigOptions{stopD: 20})
		r.thinFilled(t0, waiting)
		t1 := t0.Add(time.Second)
		r.m.session.End = t1.Add(30 * time.Second) // inside forced_exit_before_close
		r.at(t1)
		r.books(waiting)
		r.trade(t1)
		rows := r.pairRows("pair")
		if last := rows[len(rows)-1]; last.Reason != "forced" {
			t.Errorf("hand-over reason %q, want forced", last.Reason)
		}
		if o := r.orders(); o[len(o)-1].Request.Purpose != cex.PurposeForced {
			t.Errorf("last order %s, want the forced exit", o[len(o)-1].Request.Purpose)
		}
	})
}

// SPEC 12.4 step 6: with SOXL out of reach (bids 40.39 / 40.38, below P* =
// 40.3953) and x deeper than x_ref (SOXS's bids up to a fifth of 59.52: x =
// 11.67 > 10), a top-up of SOXS goes: 70.20 more (the book's half, within 2
// x qS and the quota). Once it filled x_ref is its x, and v_S (59.483) moves
// P* to 40.3604: the next round pairs all of T = 210.60 at 40.39.
func TestThinFirstTopUp(t *testing.T) {
	r := thinRig(t, rigOptions{stopD: 20})
	t0 := utc8Time(11, 25, 5, 3, 5)
	r.thinFilled(t0, withBull(lv("40.39", "300"), lv("40.38", "500")))
	deeper := withBull(lv("40.39", "300"), lv("40.38", "500"))
	deeper.bearBids = side{lv("59.54", "0.4"), lv("59.535", "60"), lv("59.53", "30"), lv("59.525", "30"), lv("59.52", "20")}
	t1 := t0.Add(time.Second)
	r.at(t1)
	r.books(deeper)
	r.trade(t1)
	o := r.orders()
	wantOrder(t, "top-up", o[1], cex.Bear, cex.PurposeEntry, cex.SideSell, "70.2", "59.52")
	if !o[1].CumExecQty.Equal(dec("70.2")) {
		t.Fatalf("top-up filled %s, want 70.20", o[1].CumExecQty)
	}
	near(t, "x_ref after the top-up", r.m.batch.pair.batch.XRef, 10000*(40.38/40+59.52/60-2)-10000*(40.40/40+59.42/60-2))
	if len(o) != 3 {
		t.Fatalf("%d orders, want the thick order in the round after the top-up's result", len(o))
	}
	wantOrder(t, "thick", o[2], cex.Bull, cex.PurposeComplete, cex.SideSell, "210.6", "40.39")
	if r.m.lastEntry == nil || !r.m.lastEntry.At.Equal(t0) {
		t.Errorf("last entry %+v: a top-up must not move it from %s", r.m.lastEntry, showTime(t0))
	}
}

// --- the exit (SPEC 12.6, 12.7) ---

// builtShort is a rig with SOXS first and example 1's short built by the
// thin leg first: SOXS 70.20 sold @ 59.43061, SOXL 105.30 @ 40.45.
func builtShort(t *testing.T, opts rigOptions) (*rig, time.Time) {
	t.Helper()
	r := thinRig(t, opts)
	t0 := utc8Time(11, 25, 5, 3, 5)
	r.thinFilled(t0, entryBook())
	r.results()
	r.step()
	if lot := r.lotNow(1); lot.End != inventory.LotEndBuilt {
		t.Fatalf("lot %s, want built", lot.End)
	}
	return r, t0
}

// exit1Book has the asks of an exit at exit1 for the short: x = D(40.40,
// 59.403) - 3.3333 = -2.83, past -0.5 sigma = -2 and short of -1 sigma = -4.
func exit1Book() pairBook {
	b := exitBook()
	b.bullAsks = side{lv("40.39", "300"), lv("40.40", "400")}
	b.bearAsks = side{lv("59.39", "100"), lv("59.395", "40"), lv("59.398", "30"), lv("59.40", "20"), lv("59.403", "10")}
	return b
}

// exitOrders are the exit orders sent since n orders, in order.
func (r *rig) exitOrdersSince(n int) []cex.OrderRecord {
	var out []cex.OrderRecord
	for _, o := range r.orders()[n:] {
		if o.Request.Purpose == cex.PurposeExit {
			out = append(out, o)
		}
	}
	return out
}

// SPEC 12.7 with one exit line: SOXS is bought back alone (70.20 @ the
// pricing level 59.400, filled @ 59.385), and in the round that books it
// SOXL follows within its break-even bound: 105.30 @ 40.37. The decision
// carries the pre-check; the cycle ends at the line.
func TestThinFirstExitOneLine(t *testing.T) {
	r, t0 := builtShort(t, rigOptions{})
	n := len(r.orders())
	t1 := t0.Add(5 * time.Second)
	r.at(t1)
	r.books(exitBook())
	r.stepAt(t1)
	ex := r.exitOrdersSince(n)
	if len(ex) != 1 {
		t.Fatalf("%d exit orders, want SOXS's alone", len(ex))
	}
	wantOrder(t, "thin exit", ex[0], cex.Bear, cex.PurposeExit, cex.SideBuy, "70.2", "59.4")
	exit := r.decisions()[len(r.decisions())-1]
	if d := detailOf(t, exit); exit.Kind != "exit" || d["line"] != "exit" || d["d1"] != false || d["expected_net"] == nil || d["thin_leg"] != "bear" {
		t.Errorf("exit decision %s %v", exit.Kind, d)
	}
	r.results()
	r.step()
	ex = r.exitOrdersSince(n)
	if len(ex) != 2 {
		t.Fatalf("%d exit orders after the thin close's round, want SOXL's sent in it", len(ex))
	}
	wantOrder(t, "thick exit", ex[1], cex.Bull, cex.PurposeExit, cex.SideBuy, "105.3", "40.37")
	r.results()
	r.step()
	if got := r.actions("exit_pair"); len(got) != 3 || got[0] != "fill" || got[1] != "send_thick" || got[2] != "done" {
		t.Errorf("exit_pair actions %v, want fill, send_thick, done", got)
	}
	r.stepAt(t1.Add(time.Second))
	if c := r.lastCycle(); r.inv.Cycle() != nil || c.EndReason != inventory.EndExitLine {
		t.Errorf("cycle open %t, end %q; want closed exit_line", r.inv.Cycle() != nil, c.EndReason)
	}
}

// SPEC 12.6 with the thin leg first and exit2 = -1: at exit1 the round is
// in profit, so H is half SOXS's 70.20, 35.10, and SOXL's 52.65 by the
// pairing anchors (12.12 item 6); one batch closes it thin first and H is
// done (stored in exit_stage). exit1 again closes nothing more; exit2 closes
// the rest.
func TestThinFirstTwoStageExit(t *testing.T) {
	exit2 := -1.0
	r, t0 := builtShort(t, rigOptions{exit2: &exit2})
	n := len(r.orders())
	t1 := t0.Add(5 * time.Second)
	r.at(t1)
	r.books(exit1Book())
	r.trade(t1)
	ex := r.exitOrdersSince(n)
	if len(ex) != 2 {
		t.Fatalf("%d exit orders, want the half: SOXS then SOXL", len(ex))
	}
	wantOrder(t, "thin half", ex[0], cex.Bear, cex.PurposeExit, cex.SideBuy, "35.1", "59.403")
	wantOrder(t, "thick half", ex[1], cex.Bull, cex.PurposeExit, cex.SideBuy, "52.65", "40.39")
	r.results()
	r.step()
	row, ok := r.sink.Last("exit_stage").(store.ExitStageRow)
	if !ok || !row.TargetBear.Equal(dec("35.1")) || !row.TargetBull.Equal(dec("52.65")) || !row.ClosedBear.Equal(dec("35.1")) || !row.ClosedBull.Equal(dec("52.65")) || row.DoneMs == 0 || row.BuiltLots != 1 {
		t.Fatalf("exit_stage %+v, want H 52.65 / 35.10 closed and done", row)
	}
	if exit := r.pairRows("exit")[0]; detailOf(t, exit)["line"] != "exit1" {
		t.Errorf("first exit line %v, want exit1", detailOf(t, exit)["line"])
	} else if d := detailOf(t, exit); d["stage_closed_bull"] != "0" || d["stage_closed_bear"] != "0" {
		t.Errorf("exit1 decision's half-target base %v / %v, want 0 / 0: a restart counts the batch's fills from it", d["stage_closed_bull"], d["stage_closed_bear"])
	}

	// exit1 again: H is done, nothing more closes there.
	t2 := t1.Add(time.Second)
	r.at(t2)
	r.books(exit1Book())
	before := len(r.orders())
	r.stepAt(t2)
	if len(r.orders()) != before {
		t.Fatalf("an order at exit1 after H was done")
	}

	// exit2: the rest, thin first.
	t3 := t2.Add(time.Second)
	r.at(t3)
	r.books(exitBook())
	r.trade(t3)
	r.results()
	r.step()
	ex = r.exitOrdersSince(before)
	if len(ex) != 2 {
		t.Fatalf("%d exit orders at exit2, want two", len(ex))
	}
	wantOrder(t, "thin rest", ex[0], cex.Bear, cex.PurposeExit, cex.SideBuy, "35.1", "59.4")
	wantOrder(t, "thick rest", ex[1], cex.Bull, cex.PurposeExit, cex.SideBuy, "52.65", "40.37")
	if exits := r.pairRows("exit"); detailOf(t, exits[len(exits)-1])["line"] != "exit2" {
		t.Errorf("last exit line %v, want exit2", detailOf(t, exits[len(exits)-1])["line"])
	}
	r.stepAt(t3.Add(time.Second))
	if c := r.lastCycle(); r.inv.Cycle() != nil || c.EndReason != inventory.EndExitLine {
		t.Errorf("cycle open %t, end %q; want closed exit_line", r.inv.Cycle() != nil, c.EndReason)
	}
}

// SPEC 12.12 item 2 (replay group 2): both legs together with exit2. The
// half at exit1 goes as one pair after the pre-check; its fills count
// towards H once its results are in.
func TestTwoStageExitBothLegsTogether(t *testing.T) {
	exit2 := -1.0
	r := newRig(t, utc8Time(11, 25, 5, 1, 0), rigOptions{exit2: &exit2})
	r.warmUp()
	t0 := utc8Time(11, 25, 5, 3, 5)
	r.at(t0)
	r.books(entryBook())
	r.trade(t0)
	n := len(r.orders())
	t1 := t0.Add(5 * time.Second)
	r.at(t1)
	r.books(exit1Book())
	r.trade(t1)
	ex := r.exitOrdersSince(n)
	if len(ex) != 2 || !ex[0].Request.Qty.Equal(dec("52.65")) || !ex[1].Request.Qty.Equal(dec("35.1")) {
		t.Fatalf("exit orders %+v, want the half 52.65 / 35.10 together", ex)
	}
	r.stepAt(t1.Add(10 * time.Millisecond))
	if row, ok := r.sink.Last("exit_stage").(store.ExitStageRow); !ok || !row.ClosedBear.Equal(dec("35.1")) || !row.ClosedBull.Equal(dec("52.65")) || row.DoneMs == 0 {
		t.Fatalf("exit_stage %+v, want the half closed and done", r.sink.Last("exit_stage"))
	}
	t2 := t1.Add(time.Second)
	r.at(t2)
	r.books(exit1Book())
	before := len(r.orders())
	r.stepAt(t2)
	if len(r.orders()) != before {
		t.Fatalf("an order at exit1 after H was done")
	}
}

// lossState is a ledger as a restart reads it back: one open short lot,
// thin leg first and built, SOXL 105.30 @ 40.38 and SOXS 70.20 @ 59.40 (D =
// -5 under 40 / 60): at exitBook's levels the round makes nothing before
// fees, so the pre-check fails.
func lossState(start time.Time) *store.State {
	opened := start.Add(-time.Hour)
	id := fmt.Sprintf("c%d", opened.UnixMilli())
	return &store.State{
		Inventory: &store.InventoryStateRow{Capital: dec("2000"), SinceMs: opened.Add(-time.Hour).UnixMilli()},
		Cycle: &store.CycleRow{CycleID: id, SessionID: "2026-11-24", Direction: "short", Status: string(inventory.CycleOpen),
			PairAnchorKind: "bell", PairAnchorBull: dec("40"), PairAnchorBear: dec("60"), OpenedMs: opened.UnixMilli(), FundingKnown: true},
		Lots: []store.LotRow{{LotID: id + "-1", CycleID: id, Seq: 1, Status: string(inventory.LotOpen), EndState: "built", Direction: "short",
			EntryQtyBull: dec("105.3"), EntryValueBull: dec("4252.014"), EntryQtyBear: dec("70.2"), EntryValueBear: dec("4169.88"), OpenedMs: opened.UnixMilli()}},
		Orders: []store.OrderRow{
			filledRow(id, opened, 1, cex.Bear, cex.PurposeEntry, "59.40", "70.2", "4169.88"),
			filledRow(id, opened, 2, cex.Bull, cex.PurposeComplete, "40.38", "105.3", "4252.014"),
		},
	}
}

// filledRow is the stored row of an entry order of the lot cycleID-1 that
// filled qty at price (value), its fee 0.65 bps and known.
func filledRow(cycleID string, opened time.Time, seq int, leg cex.Leg, purpose cex.OrderPurpose, price, qty, value string) store.OrderRow {
	return store.OrderRow{ClientOrderID: string(cex.NewClientOrderID(opened, seq, leg, purpose)), CycleID: cycleID, LotID: cycleID + "-1", Leg: leg.String(),
		Symbol: string(legSymbol(leg)), Purpose: string(purpose), Side: string(cex.SideSell), Price: dec(price), Qty: dec(qty), Status: string(cex.StatusFilled),
		CumExecQty: dec(qty), CumExecValue: dec(value), AvgPrice: dec(price), Fee: dec(value).Mul(dec("0.000065")), FeeKnown: true,
		SubmittedMs: opened.UnixMilli(), FinalMs: opened.UnixMilli()}
}

// lossRig is a thin-first rig restored with lossState, its mean ready.
func lossRig(t *testing.T, opts rigOptions) *rig {
	t.Helper()
	start := utc8Time(11, 25, 5, 1, 0)
	opts.thin, opts.state = "bear", lossState(start)
	r := newRig(t, start, opts)
	r.endMinute()
	r.endMinute()
	r.stepAt(utc8Time(11, 25, 5, 3, 0))
	if !r.m.stats.Ready {
		t.Fatal("the mean is not ready")
	}
	return r
}

// D1 (owner item 95, "我建议照样平"): at the one line the pre-check fails
// and the batch exits all the same, SOXL held to its pricing level at the
// decision (40.38), the protection: SOXS 70.20 @ 59.400, then SOXL 105.30
// @ 40.37 within it. The exit is recorded as d1_loss with the expected net
// below zero and, at its end, the actual net.
func TestThinFirstExitD1(t *testing.T) {
	r := lossRig(t, rigOptions{})
	t1 := utc8Time(11, 25, 5, 3, 5)
	r.at(t1)
	r.books(exitBook())
	r.trade(t1)
	exit := r.pairRows("exit")[0]
	d := detailOf(t, exit)
	if exit.Reason != "d1_loss" || d["d1"] != true || d["protect"] != "40.38" || !dec(d["expected_net"].(string)).IsNegative() {
		t.Fatalf("exit %s %v, want d1_loss with SOXL protected at 40.38 and a negative expected net", exit.Reason, d)
	}
	ex := r.exitOrdersSince(0)
	if len(ex) != 2 {
		t.Fatalf("%d exit orders, want SOXS then SOXL", len(ex))
	}
	wantOrder(t, "thick", ex[1], cex.Bull, cex.PurposeExit, cex.SideBuy, "105.3", "40.37")
	r.results()
	r.step()
	rows := r.pairRows("exit_pair")
	if done := detailOf(t, rows[len(rows)-1]); done["action"] != "done" || done["actual_net"] == nil || done["d1"] != true {
		t.Errorf("exit batch end %v, want done with the actual net", done)
	}
}

// D2 (owner item 95, "接受"): once SOXS is bought back, SOXL above its bound
// (the protection 40.38) waits; 30 s after the first thin closing fill
// (exit_pair_window, on the tick) it closes at PRD 10.4's price whatever
// the bound: best ask 40.42 through 10 bps, up: 40.47 (reason
// exit_window). With pair_stop_d 10 a SOXL ask of 40.44 puts the ideal
// closing spread 12.5 above the decision's -5: it closes at once (reason
// exit_stop_d), 40.43 through 10 bps: 40.48.
func TestThinFirstExitD2(t *testing.T) {
	for _, c := range []struct {
		name    string
		stopD   float64
		asks    side
		wait    time.Duration
		why, px string
	}{
		{"window", 50, side{lv("40.42", "300"), lv("40.43", "400")}, 30 * time.Second, "exit_window", "40.47"},
		{"spread stop", 10, side{lv("40.43", "300"), lv("40.44", "400")}, 0, "exit_stop_d", "40.48"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := lossRig(t, rigOptions{latency: 100 * time.Millisecond, stopD: c.stopD})
			t1 := utc8Time(11, 25, 5, 3, 5)
			r.at(t1)
			r.books(exitBook())
			r.stepAt(t1)
			moved := exitBook()
			moved.bullAsks = c.asks
			r.depth(cex.Bull, moved.bullBids, moved.bullAsks)
			r.results()
			r.step() // the thin close is booked: the window starts here
			if c.wait > 0 {
				if len(r.exitOrdersSince(0)) != 1 || r.m.idleReason != strategy.ReasonExitBound {
					t.Fatalf("%d exit orders, idle %q; want SOXL waiting for its bound", len(r.exitOrdersSince(0)), r.m.idleReason)
				}
				r.at(t1.Add(c.wait - 5*time.Second))
				r.depth(cex.Bull, moved.bullBids, moved.bullAsks) // a recent price for the hard close
				r.idleAt(t1.Add(c.wait-time.Millisecond), strategy.ReasonExitBound)
				r.stepAt(t1.Add(c.wait))
			}
			ex := r.exitOrdersSince(0)
			if len(ex) != 2 {
				t.Fatalf("%d exit orders, want the hard close of SOXL; idle %q", len(ex), r.m.idleReason)
			}
			wantOrder(t, "hard close", ex[1], cex.Bull, cex.PurposeExit, cex.SideBuy, "105.3", c.px)
			rows := r.pairRows("exit_pair")
			if risk := rows[len(rows)-2]; risk.Reason != c.why || detailOf(t, risk)["action"] != "risk_exit" {
				t.Errorf("risk exit %s %v, want %s", risk.Reason, detailOf(t, risk), c.why)
			}
		})
	}
}

// PRD 13.6: an unwind needs a recent price of the thin leg (a frame within
// 10 s). When the window ends with SOXS silent for 30 s the unwind waits for
// SOXS's next frame and goes on it.
func TestThinFirstUnwindNeedsAPrice(t *testing.T) {
	r := thinRig(t, rigOptions{stopD: 20})
	t0 := utc8Time(11, 25, 5, 3, 5)
	r.thinFilled(t0, withBull(lv("40.39", "300"), lv("40.38", "500")))
	r.idleAt(t0.Add(30*time.Second), strategy.ReasonNoPrice)
	t1 := t0.Add(31 * time.Second)
	r.at(t1)
	r.depth(cex.Bear, entryBook().bearBids, entryBook().bearAsks)
	before := len(r.orders())
	r.stepAt(t1)
	if o := r.orders(); len(o) != before+1 {
		t.Fatalf("no unwind on SOXS's new frame; idle %q", r.m.idleReason)
	} else {
		wantOrder(t, "unwind", o[before], cex.Bear, cex.PurposeTrim, cex.SideBuy, "70.2", "59.505")
	}
}

// SPEC 12.4 "薄腿太小" and the lead's override of 10.4 deviation 7: SOXS
// fills 0.07 (4.16 U): its SOXL target 0.10 (4.05 U) and its own close are
// below 5 U. The batch waits in the window (a top-up could still come);
// when the window ends it has paired nothing and holds only dust: it ends
// as unwound_stop, reason too_small, with the dust recorded (owner item
// 47), and the cycle closes with the dust left.
func TestThinFirstDustOnlyEndsUnwoundStop(t *testing.T) {
	r := thinRig(t, rigOptions{latency: 100 * time.Millisecond})
	t0 := utc8Time(11, 25, 5, 3, 5)
	r.at(t0)
	r.books(entryBook())
	r.stepAt(t0)
	b := entryBook()
	b.bearBids = side{lv("59.437", "0.07"), lv("59.41", "60"), lv("59.40", "30"), lv("59.39", "30"), lv("59.38", "20")}
	r.depth(cex.Bear, b.bearBids, b.bearAsks)
	r.results()
	r.step()
	if r.m.idleReason != strategy.ReasonTooSmall {
		t.Fatalf("idle %q, want too_small: a top-up may still come", r.m.idleReason)
	}
	t1 := t0.Add(30 * time.Second)
	r.at(t1.Add(-time.Second))
	r.books(b) // recent prices
	r.stepAt(t1)
	rows := r.pairRows("pair")
	done := rows[len(rows)-1]
	d := detailOf(t, done)
	if d["action"] != "done" || d["end"] != "unwound_stop" || done.Reason != "too_small" || d["dust_qty"] == nil || d["dust_leg"] != "bear" {
		t.Fatalf("end %s %v, want done unwound_stop too_small with the dust", done.Reason, d)
	}
	if got := dec(d["dust_qty"].(string)); !got.Equal(dec("0.07")) {
		t.Errorf("dust %s, want 0.07", got)
	}
	r.stepAt(t1.Add(time.Second))
	if c := r.lastCycle(); r.inv.Cycle() != nil || !c.DustBear.Equal(dec("-0.07")) || c.EndReason != inventory.EndOneLeg {
		t.Errorf("cycle open %t, dust %s, end %q; want closed with -0.07 SOXS left, one_leg", r.inv.Cycle() != nil, c.DustBear, c.EndReason)
	}
}
