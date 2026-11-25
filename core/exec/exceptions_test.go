package exec

import (
	"context"
	"reflect"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/inventory"
	"etf-pair-arbitrage/core/store"
	"etf-pair-arbitrage/core/strategy"
)

// entryWithUnknownBull sends example 1's entry at t0 with SOXL's order kept
// from the venue (parked): SOXS sells its 70.20, SOXL's outcome is unknown.
// It returns SOXL's order id.
func (r *rig) entryWithUnknownBull(t0 time.Time) cex.ClientOrderID {
	r.t.Helper()
	r.at(t0)
	r.books(entryBook())
	r.gate.park("SOXLUSDT")
	r.stepAt(t0)
	return r.waitUnknownBull()
}

// waitUnknownBull waits until the only order without a final state is
// SOXL's, with its outcome unknown, and returns its id.
func (r *rig) waitUnknownBull() cex.ClientOrderID {
	r.t.Helper()
	var id cex.ClientOrderID
	waitFor(r.t, "SOXS final and SOXL unknown", func() bool {
		u := r.cex.UnresolvedOrders()
		if len(u) != 1 || !u[0].Unknown || u[0].Request.Leg != cex.Bull {
			return false
		}
		id = u[0].Request.ClientOrderID
		return true
	})
	return id
}

// eventCount is how many event rows of kind the loop wrote.
func (r *rig) eventCount(kind string) (n int) {
	for _, k := range r.eventKinds() {
		if k == kind {
			n++
		}
	}
	return n
}

// purposes lists the purposes of every order sent, in order.
func (r *rig) purposes() (out []string) {
	for _, o := range r.orders() {
		out = append(out, string(o.Request.Purpose))
	}
	return out
}

// The stop while an entry order's outcome is unknown (SPEC row 6.3). SOXS
// sold its 70.20 and SOXL's order has not reached the venue. The stop
// forces the exit by the venue's positions: buy back SOXS's 70.20. The
// positions then read zero on both legs, and the process stays: SOXL's
// outcome is still unknown. When that order reaches the venue and sells
// 105.30, the ledger books it and the forced exit buys it back; the cycle
// closes as stop, and only then may the process leave. No entry is sent
// after the first, and SOXL's order is handed to the venue once.
func TestStopWithAnUnknownEntryOrder(t *testing.T) {
	r := newRig(t, utc8Time(11, 25, 5, 1, 0), rigOptions{})
	r.warmUp()
	t0 := utc8Time(11, 25, 5, 3, 5)
	bull := r.entryWithUnknownBull(t0)
	r.m.onSignal(syscall.SIGTERM, t0)

	r.tradeOn(t0.Add(time.Second)) // the forced exit by positions: SOXS only
	d := r.decisions()[len(r.decisions())-1]
	if detail := decodeDetail(t, d.Detail); d.Kind != "forced" || detail["by_positions"] != true || detail["qty_bear"] != "70.2" || detail["qty_bull"] != nil {
		t.Fatalf("decision %s %v, want a forced exit of SOXS's 70.20 by positions", d.Kind, detail)
	}
	for s := 2; s <= 3; s++ {
		if r.stepAt(t0.Add(time.Duration(s) * time.Second)) {
			t.Fatalf("left at second %d with SOXL's outcome unknown", s)
		}
		if pos := r.cex.Positions(); len(pos.BySymbol) != 0 || r.m.idleReason != strategy.ReasonOrderUnknown {
			t.Fatalf("positions %v, idle reason %q; want none held, order_unknown", pos.BySymbol, r.m.idleReason)
		}
	}

	r.gate.releaseParked() // SOXL's order reaches the venue and sells 105.30 at 40.45
	waitFor(t, "SOXL's result", func() bool { return len(r.cex.UnresolvedOrders()) == 0 })
	r.trade(t0.Add(4 * time.Second)) // the forced exit by the ledger: SOXL 105.30
	if !r.stepAt(t0.Add(5 * time.Second)) {
		t.Fatalf("did not leave once flat; idle reason %q, cycle open %t", r.m.idleReason, r.inv.Cycle() != nil)
	}
	if got, want := strings.Join(r.purposes(), " "), "entry entry forced forced"; got != want {
		t.Errorf("orders %s, want %s", got, want)
	}
	if c := r.lastCycle(); c.Status != string(inventory.CycleClosed) || c.EndReason != inventory.EndStop {
		t.Errorf("cycle %s %s, want closed stop", c.Status, c.EndReason)
	}
	if n := r.gate.placeCalls(bull); n != 1 {
		t.Errorf("SOXL's entry order handed to the venue %d times, want 1", n)
	}
}

// V14: an order that never reaches the venue. Meanwhile there is no entry
// and no exit at the line (example 2's book is at the exit line and a long
// signal) and the order is never sent again. At 30 s the alarm, and SOXS's
// 70.20 are trimmed by the venue's positions. The verdict "never arrived"
// needs all three evidences: with cex's NotFound but a SOXL position of
// -0.05 the order stays unknown; with the position back at the ledger's 0
// it is never_arrived. The batch is then void (one_leg) and the cycle ends.
func TestUnknownOrderNeverArrived(t *testing.T) {
	r := newRig(t, utc8Time(11, 25, 5, 1, 0), rigOptions{})
	r.warmUp()
	t0 := utc8Time(11, 25, 5, 3, 5)
	bull := r.entryWithUnknownBull(t0)
	for s := 1; s <= 3; s++ {
		r.at(t0.Add(time.Duration(s) * time.Second))
		r.books(exitBook())
		r.idleAt(t0.Add(time.Duration(s)*time.Second), strategy.ReasonOrderUnknown)
	}

	t30 := t0.Add(31 * time.Second) // 30 s after the first round that saw it unknown
	r.at(t30)
	r.books(entryBook())
	r.tradeOn(t30)
	if r.eventCount("order_unknown_30s") != 1 {
		t.Errorf("events %v, want one order_unknown_30s", r.eventKinds())
	}
	d := r.decisions()[len(r.decisions())-1]
	if detail := decodeDetail(t, d.Detail); d.Kind != "trim" || detail["by_positions"] != true || detail["leg"] != "bear" || detail["qty"] != "70.2" {
		t.Fatalf("decision %s %v, want a trim of SOXS's 70.20 by positions", d.Kind, detail)
	}

	r.src.setNotFound(bull)
	r.paper.SeedPositions([2]decimal.Decimal{dec("-0.05"), decimal.Zero})
	r.stepAt(t30.Add(2 * time.Second))
	if len(r.cex.UnresolvedOrders()) != 1 || r.eventCount("order_never_arrived") != 0 {
		t.Fatal("the order was taken as never arrived while the position may hold its fill")
	}
	r.paper.SeedPositions([2]decimal.Decimal{decimal.Zero, decimal.Zero})
	r.stepAt(t30.Add(4 * time.Second))
	if r.eventCount("order_never_arrived") != 1 || len(r.cex.UnresolvedOrders()) != 0 {
		t.Fatalf("events %v, unresolved %d; want the verdict", r.eventKinds(), len(r.cex.UnresolvedOrders()))
	}
	r.stepAt(t30.Add(5 * time.Second))
	lot := r.lastLot(r.decisions()[0].LotID)
	if lot.Status != string(inventory.LotVoid) || lot.VoidReason != inventory.VoidOneLeg {
		t.Errorf("lot %s %s, want void one_leg", lot.Status, lot.VoidReason)
	}
	if c := r.lastCycle(); r.inv.Cycle() != nil || c.EndReason != inventory.EndOneLeg {
		t.Errorf("cycle open %t, end %q; want closed one_leg", r.inv.Cycle() != nil, c.EndReason)
	}
	if n := r.gate.placeCalls(bull); n != 1 {
		t.Errorf("SOXL's order handed to the venue %d times, want 1", n)
	}
}

// V14: an order that filled in part while its outcome stays unknown (the
// answer and the reports are lost; queries fail). SOXL sold 50 of 105.30
// at the venue, SOXS all 70.20. At 30 s the gap by the venue's positions,
// 70.20 x 60 - 50 x 40 = 2,212 USD, trims SOXS's (2,212 / 60 =) 36.86; the
// ledger never books the 50 the positions show. When the reports arrive the
// 50 are booked once, the gap left (50 x 40 - 33.34 x 60 = -0.4 USD) is
// dust, and the batch is built.
func TestUnknownOrderPartlyFilled(t *testing.T) {
	r := gapRig(t, rigOptions{})
	t0 := utc8Time(11, 25, 5, 3, 5)
	r.at(t0)
	r.books(entryBook())
	r.gate.lose("SOXLUSDT")
	r.gate.hold("SOXLUSDT")
	r.stepAt(t0)
	match := entryBook()
	match.bullBids = side{lv("40.44", "50"), lv("40.30", "500")}
	r.books(match)
	r.waitUnknownBull()
	waitFor(t, "SOXL's 50 at the venue", func() bool {
		pos, _ := r.paper.Positions(context.Background(), nil)
		return pos.BySymbol["SOXLUSDT"].Qty.Equal(dec("-50"))
	})
	r.idleAt(t0.Add(time.Second), strategy.ReasonOrderUnknown)

	t30 := t0.Add(31 * time.Second) // 30 s after the first round that saw it unknown
	r.at(t30)
	r.books(match)
	r.tradeOn(t30)
	d := r.decisions()[len(r.decisions())-1]
	if detail := decodeDetail(t, d.Detail); d.Kind != "trim" || detail["by_positions"] != true || detail["leg"] != "bear" || detail["qty"] != "36.86" {
		t.Fatalf("decision %s %v, want a trim of 36.86 SOXS by positions", d.Kind, detail)
	}
	if _, hold := r.inv.Holdings(); !hold[cex.Bull].IsZero() || !hold[cex.Bear].Equal(dec("33.34")) {
		t.Errorf("ledger %v, want 0 / 33.34: the 50 the positions show are not booked", hold)
	}

	r.gate.release()
	waitFor(t, "SOXL's report", func() bool { return len(r.cex.UnresolvedOrders()) == 0 })
	r.stepAt(t30.Add(time.Second))
	l := r.lastLot(r.decisions()[0].LotID)
	if l.Status != string(inventory.LotOpen) || !l.EntryQtyBull.Equal(dec("50")) || !l.ExitQtyBear.Equal(dec("36.86")) {
		t.Errorf("lot %s, SOXL %s, SOXS trimmed %s; want open, 50, 36.86", l.Status, l.EntryQtyBull, l.ExitQtyBear)
	}
}

// PRD 8.4, 10.4: in the close window SOXS's forced buy at 59.445 (59.385 x
// 1.001 rounded up) is refused for its price. On the same book it is not
// sent again; when the book's ask moves to 59.39 the limit is 59.450 and it
// goes. A refusal for a rate limit holds no price back: the same order goes
// again after the resend interval.
func TestRefusedReducingOrders(t *testing.T) {
	setup := func(t *testing.T) (*rig, time.Time) {
		r := newRig(t, utc8Time(11, 25, 5, 1, 0), rigOptions{})
		r.warmUp()
		r.at(utc8Time(11, 25, 5, 3, 5))
		r.books(entryBook())
		r.trade(utc8Time(11, 25, 5, 3, 5))
		t1 := utc8Time(11, 26, 4, 59, 0)
		r.at(t1)
		r.books(exitBook())
		return r, t1
	}
	sentBear := func(r *rig) (n int) {
		for _, o := range r.orders() {
			if o.Request.Purpose == cex.PurposeForced && o.Request.Leg == cex.Bear {
				n++
			}
		}
		return n
	}

	t.Run("price limit", func(t *testing.T) {
		r, t1 := setup(t)
		r.gate.setRefuse(func(req cex.OrderRequest) error {
			if req.Leg == cex.Bear && req.Price.Equal(dec("59.445")) {
				return &cex.VenueError{Code: "40915", Message: "price out of the band", Kind: cex.ErrorPriceLimit}
			}
			return nil
		})
		r.trade(t1) // SOXL fills, SOXS is refused
		for i := 1; i <= 3; i++ {
			r.idleAt(t1.Add(time.Duration(i)*300*time.Millisecond), strategy.ReasonAwaitDepth)
		}
		if n := sentBear(r); n != 1 {
			t.Fatalf("%d SOXS forced orders on the same book, want 1", n)
		}
		t2 := t1.Add(2 * time.Second)
		r.at(t2)
		moved := exitBook()
		moved.bearAsks[0] = lv("59.39", "100")
		r.books(moved)
		r.trade(t2)
		if o := r.lastOrder(); o.Request.Leg != cex.Bear || !o.Request.Price.Equal(dec("59.45")) {
			t.Errorf("last order %s at %s, want SOXS at 59.450", o.Request.Leg, o.Request.Price)
		}
		if c := r.lastCycle(); r.inv.Cycle() != nil || c.EndReason != inventory.EndCloseWindow {
			t.Errorf("cycle open %t, end %q; want closed close_window", r.inv.Cycle() != nil, c.EndReason)
		}
	})
	t.Run("rate limit", func(t *testing.T) {
		r, t1 := setup(t)
		var refused atomic.Bool
		r.gate.setRefuse(func(req cex.OrderRequest) error {
			if req.Leg == cex.Bear && refused.CompareAndSwap(false, true) {
				return &cex.VenueError{Code: "429", Message: "too many requests", Kind: cex.ErrorRateLimited}
			}
			return nil
		})
		r.trade(t1)
		r.trade(t1.Add(300 * time.Millisecond)) // the same price, once the interval passed
		if n := sentBear(r); n != 2 {
			t.Errorf("%d SOXS forced orders, want 2", n)
		}
		if o := r.lastOrder(); !o.Request.Price.Equal(dec("59.445")) || o.Status != cex.StatusFilled {
			t.Errorf("last order at %s %s, want 59.445 filled", o.Request.Price, o.Status)
		}
	})
}

// V16: after a stop the forced exit cannot price SOXS (no frame for 20 s):
// at 60 s the alarm, not the exit, and again every 60 s; a stop signal
// repeated before the alarm is no takeover. When SOXS's price is back at 121
// s the exit completes and the process leaves; a stop signal after the
// alarm is a takeover instead: the cycle is kept, marked takeover.
func TestStopAlarmAndTakeover(t *testing.T) {
	setup := func(t *testing.T) (*rig, time.Time) {
		r := newRig(t, utc8Time(11, 25, 5, 1, 0), rigOptions{})
		r.warmUp()
		t0 := utc8Time(11, 25, 5, 3, 5)
		r.at(t0)
		r.books(entryBook())
		r.trade(t0)
		ts := t0.Add(20 * time.Second)
		r.at(ts)
		b := entryBook()
		r.depth(cex.Bull, b.bullBids, b.bullAsks) // SOXS's last frame is 20 s old: no price
		r.m.onSignal(syscall.SIGTERM, ts)
		r.idleAt(ts, strategy.ReasonNoPrice)
		r.m.onSignal(syscall.SIGTERM, ts.Add(30*time.Second))
		for _, s := range []int{30, 60, 90, 120} {
			if r.stepAt(ts.Add(time.Duration(s) * time.Second)) {
				t.Fatalf("left at %d s", s)
			}
		}
		if n := r.eventCount("stop_not_flat"); n != 2 {
			t.Fatalf("%d stop_not_flat alarms by 120 s, want 2 (60 s, 120 s)", n)
		}
		if r.lastCycle().Status != string(inventory.CycleOpen) {
			t.Fatal("the signal before the alarm was taken for a takeover")
		}
		return r, ts
	}

	t.Run("price back", func(t *testing.T) {
		r, ts := setup(t)
		t1 := ts.Add(121 * time.Second)
		r.at(t1)
		r.books(entryBook())
		r.trade(t1)
		if !r.stepAt(t1.Add(time.Second)) {
			t.Fatalf("did not leave; idle reason %q", r.m.idleReason)
		}
		if c := r.lastCycle(); c.Status != string(inventory.CycleClosed) || c.EndReason != inventory.EndStop {
			t.Errorf("cycle %s %s, want closed stop", c.Status, c.EndReason)
		}
	})
	t.Run("takeover", func(t *testing.T) {
		r, ts := setup(t)
		r.m.onSignal(syscall.SIGTERM, ts.Add(121*time.Second))
		if !r.stepAt(ts.Add(121 * time.Second)) {
			t.Fatal("did not leave on the takeover")
		}
		if c := r.lastCycle(); c.Status != string(inventory.CycleTakeover) {
			t.Errorf("cycle %s, want takeover", c.Status)
		}
		if r.eventCount("taken_over") != 1 {
			t.Errorf("events %v, want taken_over", r.eventKinds())
		}
	})
}

// V16: "no new entries" stops entries only: the exit at the line goes on.
func TestNoNewEntriesKeepsExits(t *testing.T) {
	r := newRig(t, utc8Time(11, 25, 5, 1, 0), rigOptions{})
	r.warmUp()
	t0 := utc8Time(11, 25, 5, 3, 5)
	r.at(t0)
	r.books(entryBook())
	r.trade(t0)
	r.m.onSignal(syscall.SIGUSR1, t0)
	r.at(t0.Add(5 * time.Second))
	r.books(exitBook())
	r.trade(t0.Add(5 * time.Second))
	if d := r.decisions()[len(r.decisions())-1]; d.Kind != "exit" {
		t.Fatalf("decision %s, want the exit", d.Kind)
	}
	r.at(t0.Add(10 * time.Second))
	r.books(entryBook())
	r.idleAt(t0.Add(10*time.Second), strategy.ReasonNoNewEntries)
}

// PRD 13.4, 13.7: the venue's positions change behind the ledger's back
// (SOXL -200 for -105.30). Two comparisons that differ, at least 1 s apart,
// make a mismatch: then no order of any kind, not even the forced exit at
// the close window.
func TestPositionMismatchStopsAllOrders(t *testing.T) {
	r := newRig(t, utc8Time(11, 25, 5, 1, 0), rigOptions{})
	r.warmUp()
	t0 := utc8Time(11, 25, 5, 3, 5)
	r.at(t0)
	r.books(entryBook())
	r.trade(t0)
	r.paper.SeedPositions([2]decimal.Decimal{dec("-200"), dec("-70.2")})

	r.at(t0.Add(10 * time.Second))
	r.books(calibration(1))
	r.stepAt(t0.Add(10 * time.Second)) // the first comparison that differs
	if r.inv.Mismatch() {
		t.Fatal("one differing read made a mismatch")
	}
	time.Sleep(1100 * time.Millisecond) // inventory confirms on its own clock
	r.idleAt(t0.Add(11*time.Second), strategy.ReasonPositionMismatch)

	t1 := utc8Time(11, 26, 4, 59, 0)
	r.at(t1)
	r.books(exitBook())
	before := len(r.orders())
	r.idleAt(t1, strategy.ReasonPositionMismatch)
	r.idleAt(t1.Add(time.Second), strategy.ReasonPositionMismatch)
	if n := len(r.orders()) - before; n != 0 {
		t.Errorf("%d orders after the mismatch, want none", n)
	}
	mismatch := false
	for _, row := range r.sink.Rows("event") {
		mismatch = mismatch || row.(store.EventRow).Kind == "position_mismatch"
	}
	if !mismatch {
		t.Error("no position_mismatch event")
	}
}

// V17: the venue interface has no call that changes the account, and
// settings that differ allow no completion: the gap of example 3 is trimmed
// although completing would be cheaper.
func TestAccountSettings(t *testing.T) {
	venue := reflect.TypeOf((*cex.Venue)(nil)).Elem()
	for i := range venue.NumMethod() {
		name := venue.Method(i).Name
		for _, verb := range []string{"Set", "Change", "Cancel", "Amend", "Modify", "Adjust"} {
			if strings.HasPrefix(name, verb) || strings.Contains(name, "Leverage") || strings.Contains(name, "Margin") {
				t.Errorf("cex.Venue has %s", name)
			}
		}
	}

	r := gapRig(t, rigOptions{})
	t0 := utc8Time(11, 25, 5, 3, 5)
	r.enterMatchedOn(t0, entryBook(), thinBear())
	r.src.set(false, true) // the account's settings now differ
	r.at(t0.Add(time.Second))
	r.books(gapBook(side{lv("40.47", "300"), lv("40.48", "300")}, appendix3Bids("59.418")))
	r.trade(t0.Add(time.Second))
	if d := r.decisions()[len(r.decisions())-1]; d.Kind != "trim" {
		t.Errorf("decision %s, want trim", d.Kind)
	}
}
