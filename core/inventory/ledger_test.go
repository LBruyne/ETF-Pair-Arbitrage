package inventory

import (
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/store"
	"etf-pair-arbitrage/core/strategy"
)

// PRD appendix A, example 2 (V8): two lots of a short cycle and one pair of
// exit orders. The first lot is closed in full, the second in part, first
// in, first out; an exit order across both lots is split by quantity at the
// order's average price. Fees are 0.65 bps of each order's value.
//
//	lot 1  sold 105.30 @ 40.44 = 4,258.332 and 70.20 @ 59.42 = 4,171.284
//	lot 2  sold  60.00 @ 40.50 = 2,430.000 and 40.00 @ 59.45 = 2,378.000
//	exit   bought 150.00 @ 40.38 = 6,057.00 and 100.00 @ 59.40 = 5,940.00
func TestTwoLotsOneExitPair(t *testing.T) {
	r := newRig(t, "2000", "10")
	c := r.openCycle(t, strategy.Short)

	lot1 := r.openLot(t)
	orders := []cex.OrderRecord{
		fill(ord(c.ID, 1, cex.Bull, cex.PurposeEntry, cex.SideSell, lot1.ID, "40.44", "105.30"), "105.30", "4258.332", "0.27679158"),
		fill(ord(c.ID, 2, cex.Bear, cex.PurposeEntry, cex.SideSell, lot1.ID, "59.42", "70.20"), "70.20", "4171.284", "0.27113346"),
	}
	r.m.Sync(orders)
	r.m.SettleLot(lot1.ID, true, r.clock.Now())
	if _, used, _ := r.m.Quota(); !used.Equal(dec("8429.616")) {
		t.Errorf("quota used after lot 1 = %s, want 8429.616: both legs at their entry prices", used)
	}

	r.clock.Add(time.Minute)
	lot2 := r.openLot(t)
	orders = append(orders,
		fill(ord(c.ID, 3, cex.Bull, cex.PurposeEntry, cex.SideSell, lot2.ID, "40.50", "60.00"), "60.00", "2430", "0.15795"),
		fill(ord(c.ID, 4, cex.Bear, cex.PurposeEntry, cex.SideSell, lot2.ID, "59.45", "40.00"), "40.00", "2378", "0.15457"))
	r.m.Sync(orders)
	r.m.SettleLot(lot2.ID, true, r.clock.Now())
	if _, used, _ := r.m.Quota(); !used.Equal(dec("13237.616")) {
		t.Errorf("quota used after lot 2 = %s, want 13237.616", used)
	}
	if dir, hold := r.m.Holdings(); dir != strategy.Short || !hold[cex.Bull].Equal(dec("165.30")) || !hold[cex.Bear].Equal(dec("110.20")) {
		t.Errorf("holdings = %s %s / %s, want short 165.30 / 110.20", dir, hold[cex.Bull], hold[cex.Bear])
	}

	// The exit pair. The bull order is final first; the bear order has
	// filled in full but has no final state yet: lot 1 holds nothing and is
	// still not closed.
	r.clock.Add(time.Minute)
	exitBull := fill(ord(c.ID, 5, cex.Bull, cex.PurposeExit, cex.SideBuy, "", "40.38", "150.00"), "150.00", "6057", "0.393705")
	exitBear := ord(c.ID, 6, cex.Bear, cex.PurposeExit, cex.SideBuy, "", "59.40", "100.00")
	r.m.Sync(append(orders[:4:4], exitBull, working(exitBear, "100.00", "5940", "0.3861")))
	if l := r.lot(t, 1); l.Status != LotOpen || !l.flat() {
		t.Errorf("lot 1 while an exit order has no final state: status %s, flat %t; want open and flat", l.Status, l.flat())
	}
	r.m.Sync(append(orders[:4:4], exitBull, fill(exitBear, "100.00", "5940", "0.3861")))

	// Lot 1: gross 105.30 x 0.06 + 70.20 x 0.02 = 7.722; fees 0.65 bps of
	// 4,258.332 + 4,171.284 + 4,252.014 + 4,169.880 = 1.09534815.
	l1 := r.lot(t, 1)
	if l1.Status != LotClosed || l1.ClosedAt.IsZero() {
		t.Errorf("lot 1: status %s, closed at %v; want closed", l1.Status, l1.ClosedAt)
	}
	wantLeg(t, "lot 1 bull", l1.Legs[cex.Bull], "105.30", "4258.332", "0.27679158", "105.30", "4252.014", "0.27638091")
	wantLeg(t, "lot 1 bear", l1.Legs[cex.Bear], "70.20", "4171.284", "0.27113346", "70.20", "4169.88", "0.2710422")
	wantDec(t, "lot 1 gross", l1.Gross, "7.722")
	wantDec(t, "lot 1 fees", l1.Fees, "1.09534815")
	wantDec(t, "lot 1 net", l1.Net, "6.62665185")
	wantDec(t, "lot 1 fees, as the PRD prints them", l1.Fees.Round(3), "1.095")
	wantDec(t, "lot 1 net, as the PRD prints it", l1.Net.Round(3), "6.627")

	// Lot 2: 44.70 / 29.80 of 60.00 / 40.00 closed, 74.5% of each leg.
	// Gross 44.70 x 0.12 + 29.80 x 0.05 = 6.854; fees: the exit parts
	// 0.11732409 + 0.1150578 and 74.5% of the entry fees, 0.11767275 +
	// 0.11515465.
	l2 := r.lot(t, 2)
	if l2.Status != LotOpen {
		t.Errorf("lot 2: status %s, want open: 15.30 / 10.20 are left", l2.Status)
	}
	wantLeg(t, "lot 2 bull", l2.Legs[cex.Bull], "60", "2430", "0.15795", "44.70", "1804.986", "0.11732409")
	wantLeg(t, "lot 2 bear", l2.Legs[cex.Bear], "40", "2378", "0.15457", "29.80", "1770.12", "0.1150578")
	wantDec(t, "lot 2 gross", l2.Gross, "6.854")
	wantDec(t, "lot 2 fees", l2.Fees, "0.46520929")
	wantDec(t, "lot 2 net", l2.Net, "6.38879071")
	wantDec(t, "lot 2 fees, as the PRD prints them", l2.Fees.Round(3), "0.465")
	wantDec(t, "lot 2 net, as the PRD prints it", l2.Net.Round(3), "6.389")
	wantDec(t, "lot 2 bull left", l2.Legs[cex.Bull].Open(), "15.30")
	wantDec(t, "lot 2 bear left", l2.Legs[cex.Bear].Open(), "10.20")

	// The parts of each exit order add up to the order, and the lots add up
	// to the cycle.
	wantDec(t, "bull exit value over both lots", l1.Legs[cex.Bull].ExitValue.Add(l2.Legs[cex.Bull].ExitValue), "6057")
	wantDec(t, "bull exit fee over both lots", l1.Legs[cex.Bull].ExitFee.Add(l2.Legs[cex.Bull].ExitFee), "0.393705")
	wantDec(t, "bear exit value over both lots", l1.Legs[cex.Bear].ExitValue.Add(l2.Legs[cex.Bear].ExitValue), "5940")
	wantDec(t, "bear exit fee over both lots", l1.Legs[cex.Bear].ExitFee.Add(l2.Legs[cex.Bear].ExitFee), "0.3861")
	cycle := r.m.Cycle()
	wantDec(t, "cycle gross", cycle.Gross, "14.576")
	wantDec(t, "cycle fees", cycle.Fees, "1.56055744")
	wantDec(t, "cycle net", cycle.Net, "13.01544256")
	if _, hold := r.m.Holdings(); !hold[cex.Bull].Equal(dec("15.30")) || !hold[cex.Bear].Equal(dec("10.20")) {
		t.Errorf("holdings after the exit = %s / %s, want 15.30 / 10.20", hold[cex.Bull], hold[cex.Bear])
	}

	// The rows carry the same results.
	if row := lastLotRow(t, r.sink, lot1.ID); row.Status != "closed" || !row.Net.Equal(dec("6.62665185")) || row.ClosedMs == 0 {
		t.Errorf("lot 1 row: status %s, net %s, closed_ms %d; want closed, 6.62665185", row.Status, row.Net, row.ClosedMs)
	}
	if row := lastLotRow(t, r.sink, lot2.ID); row.Status != "open" || !row.ExitQtyBear.Equal(dec("29.80")) || !row.Net.Equal(dec("6.38879071")) {
		t.Errorf("lot 2 row: status %s, exit_qty_bear %s, net %s; want open, 29.80, 6.38879071", row.Status, row.ExitQtyBear, row.Net)
	}
	if row := lastCycleRow(t, r.sink, c.ID); row.Status != "open" || !row.Gross.Equal(dec("14.576")) || !row.Fees.Equal(dec("1.56055744")) {
		t.Errorf("cycle row: status %s, gross %s, fees %s; want open, 14.576, 1.56055744", row.Status, row.Gross, row.Fees)
	}
}

// The same fills booked as per-fill updates, as final order records only,
// and as a replay after a restart give the same lots to the last digit, and the parts of every order add up to the order
// exactly. The bull exit is 3 shares with a fee of 0.01 across two lots,
// which no rounded per-share fee could split evenly.
//
// A long cycle. Lot 1 bought 2 @ 10 and 2 @ 20, lot 2 bought 1 @ 10.3 and
// 1 @ 20.1. The exits sold 3 bull for 31 (fee 0.01) and 2.5 bear for 50.3
// (fee 0.007).
func TestSameFillsThreeWays(t *testing.T) {
	type orderSet struct{ e1Bull, e1Bear, e2Bull, e2Bear, xBull, xBear cex.OrderRecord }
	// The orders, unfilled; every rig starts at the same clock, so the
	// cycle and lot ids are the same in all three.
	makeOrders := func(c CycleID, l1, l2 LotID) orderSet {
		return orderSet{
			e1Bull: ord(c, 1, cex.Bull, cex.PurposeEntry, cex.SideBuy, l1, "10", "2"),
			e1Bear: ord(c, 2, cex.Bear, cex.PurposeEntry, cex.SideBuy, l1, "20", "2"),
			e2Bull: ord(c, 3, cex.Bull, cex.PurposeEntry, cex.SideBuy, l2, "10.3", "1"),
			e2Bear: ord(c, 4, cex.Bear, cex.PurposeEntry, cex.SideBuy, l2, "20.1", "1"),
			xBull:  ord(c, 5, cex.Bull, cex.PurposeExit, cex.SideSell, "", "10.3", "3"),
			xBear:  ord(c, 6, cex.Bear, cex.PurposeExit, cex.SideSell, "", "20.1", "2.5"),
		}
	}
	final := func(o orderSet) []cex.OrderRecord {
		return []cex.OrderRecord{
			at(fill(o.e1Bull, "2", "20", "0.004"), 9), at(fill(o.e1Bear, "2", "40", "0.008"), 9),
			at(fill(o.e2Bull, "1", "10.3", "0.002"), 9), at(fill(o.e2Bear, "1", "20.1", "0.004"), 9),
			at(fill(o.xBull, "3", "31", "0.01"), 9), at(fill(o.xBear, "2.5", "50.3", "0.007"), 9),
		}
	}
	// open runs the part all three share: the cycle and the two lots, each
	// lot settled as built once its entries are booked by book.
	open := func(r *testRig, book func(stage int, o orderSet)) orderSet {
		c := r.openCycle(t, strategy.Long)
		l1 := r.openLot(t)
		o := makeOrders(c.ID, l1.ID, LotID(string(c.ID)+"-2"))
		book(1, o)
		r.m.SettleLot(l1.ID, true, r.clock.Now())
		if l2 := r.openLot(t); l2.ID != LotID(string(c.ID)+"-2") {
			t.Fatalf("lot 2 id = %s", l2.ID)
		}
		book(2, o)
		r.m.SettleLot(LotID(string(c.ID)+"-2"), true, r.clock.Now())
		book(3, o)
		return o
	}

	// (a) Per-fill updates: every order grows fill by fill, with running
	// averages that differ from the final ones along the way.
	perFill := newRig(t, "1000", "10")
	oa := open(perFill, func(stage int, o orderSet) {
		f := final(o)
		switch stage {
		case 1:
			perFill.m.Sync([]cex.OrderRecord{at(working(o.e1Bull, "1", "9.9", "0.002"), 1), at(o.e1Bear, 1)})
			perFill.m.Sync([]cex.OrderRecord{f[0], at(working(o.e1Bear, "0.5", "10.1", "0.002"), 2)})
			perFill.m.Sync(f[:2])
		case 2:
			perFill.m.Sync(append(f[:2:2], at(working(o.e2Bull, "0.4", "4.1", "0.001"), 1), at(o.e2Bear, 1)))
			perFill.m.Sync(f[:4])
		case 3:
			perFill.m.Sync(append(f[:4:4], at(working(o.xBull, "1", "10.4", "0.003"), 1), at(o.xBear, 1)))
			perFill.m.Sync(append(f[:4:4], at(working(o.xBull, "2", "20.7", "0.006"), 2), at(working(o.xBear, "2.5", "50.3", "0.007"), 2)))
			perFill.m.Sync(f)
		}
	})

	// (b) Final order records only.
	finalOnly := newRig(t, "1000", "10")
	open(finalOnly, func(stage int, o orderSet) { finalOnly.m.Sync(final(o)[:2*stage]) })

	// (c) A restart: the first process booked the entries only; the second
	// is restored from the stored rows and replays the final records.
	before := newRig(t, "1000", "10")
	ob := open(before, func(stage int, o orderSet) {
		if stage < 3 {
			before.m.Sync(final(o)[:2*stage])
		}
	})
	replay := before.restart(t, "1000", "10", final(ob)...)

	for name, r := range map[string]*testRig{"per-fill updates": perFill, "final records only": finalOnly, "restart replay": replay} {
		l1, l2 := r.lot(t, 1), r.lot(t, 2)
		// Lot 1 takes 2 of the 3 bull shares: 31 x 2/3 = 20.66666667 and
		// 0.01 x 2/3 = 0.00666667; and 2 of the 2.5 bear shares: 40.24 and
		// 0.0056.
		wantLeg(t, name+": lot 1 bull", l1.Legs[cex.Bull], "2", "20", "0.004", "2", "20.66666667", "0.00666667")
		wantLeg(t, name+": lot 1 bear", l1.Legs[cex.Bear], "2", "40", "0.008", "2", "40.24", "0.0056")
		wantDec(t, name+": lot 1 gross", l1.Gross, "0.90666667")
		wantDec(t, name+": lot 1 fees", l1.Fees, "0.02426667")
		wantDec(t, name+": lot 1 net", l1.Net, "0.8824")
		// Lot 2 takes what is left of each order: 10.33333333 and
		// 0.00333333; 10.06 and 0.0014. Half its bear leg stays open, so
		// half the bear entry fee is not charged yet.
		wantLeg(t, name+": lot 2 bull", l2.Legs[cex.Bull], "1", "10.3", "0.002", "1", "10.33333333", "0.00333333")
		wantLeg(t, name+": lot 2 bear", l2.Legs[cex.Bear], "1", "20.1", "0.004", "0.5", "10.06", "0.0014")
		wantDec(t, name+": lot 2 gross", l2.Gross, "0.04333333")
		wantDec(t, name+": lot 2 fees", l2.Fees, "0.00873333")
		wantDec(t, name+": lot 2 net", l2.Net, "0.0346")
		if l1.Status != LotClosed || l2.Status != LotOpen {
			t.Errorf("%s: statuses %s / %s, want closed / open", name, l1.Status, l2.Status)
		}
		// The parts add up to the orders exactly.
		wantDec(t, name+": bull exit value", l1.Legs[cex.Bull].ExitValue.Add(l2.Legs[cex.Bull].ExitValue), "31")
		wantDec(t, name+": bull exit fee", l1.Legs[cex.Bull].ExitFee.Add(l2.Legs[cex.Bull].ExitFee), "0.01")
		wantDec(t, name+": bear exit value", l1.Legs[cex.Bear].ExitValue.Add(l2.Legs[cex.Bear].ExitValue), "50.3")
		wantDec(t, name+": bear exit fee", l1.Legs[cex.Bear].ExitFee.Add(l2.Legs[cex.Bear].ExitFee), "0.007")
	}

	// A stale snapshot, older than what is booked, moves nothing backwards,
	// and writes no row.
	rowsBefore := len(perFill.sink.Rows("lot"))
	perFill.m.Sync([]cex.OrderRecord{at(working(oa.xBull, "1", "10.4", "0.003"), 1), at(oa.xBear, 1)})
	if l1, l2 := perFill.lot(t, 1), perFill.lot(t, 2); !l1.Legs[cex.Bull].ExitQty.Equal(dec("2")) || !l2.Legs[cex.Bull].ExitQty.Equal(dec("1")) || !l2.Legs[cex.Bear].ExitQty.Equal(dec("0.5")) || l1.Status != LotClosed {
		t.Errorf("after a stale snapshot: lot 1 bull exit %s (%s), lot 2 exits %s / %s; want 2 (closed), 1 / 0.5", l1.Legs[cex.Bull].ExitQty, l1.Status, l2.Legs[cex.Bull].ExitQty, l2.Legs[cex.Bear].ExitQty)
	}
	if got := len(perFill.sink.Rows("lot")); got != rowsBefore {
		t.Errorf("a stale snapshot wrote %d lot rows, want none", got-rowsBefore)
	}
	// The same snapshot again writes nothing either: rows follow changes.
	lotRows, cycleRows := len(finalOnly.sink.Rows("lot")), len(finalOnly.sink.Rows("cycle"))
	finalOnly.m.Sync(final(makeOrders(finalOnly.m.Cycle().ID, finalOnly.lot(t, 1).ID, finalOnly.lot(t, 2).ID)))
	if a, b := len(finalOnly.sink.Rows("lot")), len(finalOnly.sink.Rows("cycle")); a != lotRows || b != cycleRows {
		t.Errorf("a repeated snapshot wrote %d lot rows and %d cycle rows, want none", a-lotRows, b-cycleRows)
	}
}

// A lot's states: building until it is settled, open once built, closed
// when both legs are zero and no order of the cycle is without a final
// state; void when it was not built, as no_fill or one_leg. The loss of a
// void lot is part of the cycle and of R.
func TestLotStates(t *testing.T) {
	r := newRig(t, "1000", "10")
	c := r.openCycle(t, strategy.Long)

	// Lot 1: built, then closed by an exit pair.
	lot1 := r.openLot(t)
	if lot1.Status != LotBuilding || lot1.Seq != 1 || lot1.ID != LotID(string(c.ID)+"-1") {
		t.Fatalf("new lot: %+v, want building, seq 1", lot1)
	}
	orders := []cex.OrderRecord{
		fill(ord(c.ID, 1, cex.Bull, cex.PurposeEntry, cex.SideBuy, lot1.ID, "10", "10"), "10", "100", "0.01"),
		fill(ord(c.ID, 2, cex.Bear, cex.PurposeEntry, cex.SideBuy, lot1.ID, "20", "5"), "5", "100", "0.01"),
	}
	r.m.Sync(orders)
	if l := r.lot(t, 1); l.Status != LotBuilding || !l.Legs[cex.Bull].EntryQty.Equal(dec("10")) {
		t.Errorf("filled and not settled: status %s, bull %s; want building with 10 booked", l.Status, l.Legs[cex.Bull].EntryQty)
	}
	r.m.SettleLot(lot1.ID, true, r.clock.Now())
	if l := r.lot(t, 1); l.Status != LotOpen || !l.ClosedAt.IsZero() {
		t.Errorf("settled as built: status %s, want open", l.Status)
	}

	r.clock.Add(time.Minute)
	exitBull := fill(ord(c.ID, 3, cex.Bull, cex.PurposeExit, cex.SideSell, "", "10.1", "10"), "10", "101", "0.01")
	exitBear := ord(c.ID, 4, cex.Bear, cex.PurposeExit, cex.SideSell, "", "20.1", "5")
	r.m.Sync(append(orders[:2:2], exitBull, working(exitBear, "5", "100.5", "0.01")))
	if l := r.lot(t, 1); l.Status != LotOpen {
		t.Errorf("both legs zero, one exit order without a final state: status %s, want open", l.Status)
	}
	orders = append(orders, exitBull, fill(exitBear, "5", "100.5", "0.01"))
	r.m.Sync(orders)
	l1 := r.lot(t, 1)
	if l1.Status != LotClosed || !l1.ClosedAt.Equal(r.clock.Now()) {
		t.Errorf("after the last final state: status %s closed at %v, want closed at %v", l1.Status, l1.ClosedAt, r.clock.Now())
	}
	wantDec(t, "lot 1 net", l1.Net, "1.46") // 1 + 0.5 - 4 x 0.01

	// Lot 2: neither leg fills.
	r.clock.Add(time.Minute)
	lot2 := r.openLot(t)
	orders = append(orders,
		fill(ord(c.ID, 5, cex.Bull, cex.PurposeEntry, cex.SideBuy, lot2.ID, "10", "10"), "0", "0", "0"),
		fill(ord(c.ID, 6, cex.Bear, cex.PurposeEntry, cex.SideBuy, lot2.ID, "20", "5"), "0", "0", "0"))
	r.m.Sync(orders)
	r.m.SettleLot(lot2.ID, false, r.clock.Now())
	if l := r.lot(t, 2); l.Status != LotVoid || l.VoidReason != VoidNoFill || !l.Net.IsZero() || l.ClosedAt.IsZero() {
		t.Errorf("lot 2: status %s, reason %q, net %s; want void, no_fill, 0", l.Status, l.VoidReason, l.Net)
	}

	// Lot 3: only the bull leg fills and is trimmed back at a loss.
	r.clock.Add(time.Minute)
	lot3 := r.openLot(t)
	orders = append(orders,
		fill(ord(c.ID, 7, cex.Bull, cex.PurposeEntry, cex.SideBuy, lot3.ID, "10", "10"), "10", "100", "0.01"),
		fill(ord(c.ID, 8, cex.Bear, cex.PurposeEntry, cex.SideBuy, lot3.ID, "20", "5"), "0", "0", "0"),
		fill(ord(c.ID, 9, cex.Bull, cex.PurposeTrim, cex.SideSell, lot3.ID, "9.9", "10"), "10", "99", "0.01"))
	r.m.Sync(orders)
	r.m.SettleLot(lot3.ID, false, r.clock.Now())
	l3 := r.lot(t, 3)
	if l3.Status != LotVoid || l3.VoidReason != VoidOneLeg {
		t.Errorf("lot 3: status %s, reason %q; want void, one_leg", l3.Status, l3.VoidReason)
	}
	wantDec(t, "lot 3 gross", l3.Gross, "-1")
	wantDec(t, "lot 3 net", l3.Net, "-1.02")
	if l := r.lot(t, 1); !l.Legs[cex.Bull].ExitQty.Equal(dec("10")) || l.Status != LotClosed {
		t.Errorf("the trim of lot 3 touched lot 1: bull exit %s, status %s", l.Legs[cex.Bull].ExitQty, l.Status)
	}

	// The lots, void ones included, add up to the cycle; the void lot's
	// loss is in R when the cycle closes.
	cycle := r.m.Cycle()
	wantDec(t, "cycle gross", cycle.Gross, "0.5")
	wantDec(t, "cycle fees", cycle.Fees, "0.06")
	r.closeCycle(t, EndExitLine)
	if r.m.Cycle() != nil {
		t.Error("the cycle is still open after CloseCycle")
	}
	wantDec(t, "r_closed", rClosed(t, r.sink), "0.44")
	row := lastCycleRow(t, r.sink, c.ID)
	if row.Status != "closed" || row.EndReason != EndExitLine || row.ClosedMs != r.clock.Now().UnixMilli() || !row.Net.Equal(dec("0.44")) || !row.FundingKnown {
		t.Errorf("closed cycle row = %+v, want closed, exit_line, net 0.44, funding known", row)
	}
	if got := lastLotRow(t, r.sink, lot3.ID); got.Status != "void" || got.VoidReason != VoidOneLeg || !got.Net.Equal(dec("-1.02")) {
		t.Errorf("lot 3 row: status %s, reason %s, net %s", got.Status, got.VoidReason, got.Net)
	}
}

// CloseCycle settles a lot still building by its orders: built and closed
// when both legs filled, void otherwise. The cycle's row and
// inventory_state, with r_closed, are written in one transaction.
func TestCloseCycleWithLotStillBuilding(t *testing.T) {
	// Both legs filled and were closed by forced exits before exec settled
	// the batch.
	r := newRig(t, "1000", "10")
	c := r.openCycle(t, strategy.Long)
	lot := r.openLot(t)
	r.m.Sync([]cex.OrderRecord{
		fill(ord(c.ID, 1, cex.Bull, cex.PurposeEntry, cex.SideBuy, lot.ID, "10", "10"), "10", "100", "0.01"),
		fill(ord(c.ID, 2, cex.Bear, cex.PurposeEntry, cex.SideBuy, lot.ID, "20", "5"), "5", "100", "0.01"),
		fill(ord(c.ID, 3, cex.Bull, cex.PurposeForced, cex.SideSell, "", "9.9", "10"), "10", "99", "0.01"),
		fill(ord(c.ID, 4, cex.Bear, cex.PurposeForced, cex.SideSell, "", "19.9", "5"), "5", "99.5", "0.01"),
	})
	if l := r.lot(t, 1); l.Status != LotBuilding {
		t.Fatalf("before the close: status %s, want building", l.Status)
	}
	r.clock.Add(time.Minute)
	r.closeCycle(t, EndCloseWindow)

	if row := lastLotRow(t, r.sink, lot.ID); row.Status != "closed" || !row.Net.Equal(dec("-1.54")) || row.ClosedMs != r.clock.Now().UnixMilli() {
		t.Errorf("lot row = status %s, net %s, closed_ms %d; want closed, -1.54", row.Status, row.Net, row.ClosedMs)
	}
	var tables []string
	for _, row := range r.sink.lastSync() {
		tables = append(tables, row.Table())
	}
	if len(tables) != 3 || tables[0] != "cycle" || tables[1] != "lot" || tables[2] != "inventory_state" {
		t.Errorf("the close was written as %v, want cycle, lot and inventory_state in one WriteSync", tables)
	}
	wantDec(t, "r_closed", rClosed(t, r.sink), "-1.54")
	if row := lastCycleRow(t, r.sink, c.ID); row.Status != "closed" || row.EndReason != EndCloseWindow || !row.Gross.Equal(dec("-1.5")) || !row.Fees.Equal(dec("0.04")) {
		t.Errorf("cycle row = %+v", row)
	}

	// Nothing filled: the lot is void and the cycle ends as no_fill.
	r.clock.Add(time.Minute)
	c = r.openCycle(t, strategy.Long)
	lot = r.openLot(t)
	r.m.Sync([]cex.OrderRecord{
		fill(ord(c.ID, 1, cex.Bull, cex.PurposeEntry, cex.SideBuy, lot.ID, "10", "10"), "0", "0", "0"),
		fill(ord(c.ID, 2, cex.Bear, cex.PurposeEntry, cex.SideBuy, lot.ID, "20", "5"), "0", "0", "0"),
	})
	r.closeCycle(t, EndNoFill)
	if row := lastLotRow(t, r.sink, lot.ID); row.Status != "void" || row.VoidReason != VoidNoFill {
		t.Errorf("lot row = status %s, reason %s; want void, no_fill", row.Status, row.VoidReason)
	}
	wantDec(t, "r_closed after an empty cycle", rClosed(t, r.sink), "-1.54")

	// A failing store leaves the cycle open and the ledger as it was; the
	// close is repeated.
	r.clock.Add(time.Minute)
	c = r.openCycle(t, strategy.Long)
	r.sink.Fail(errTest)
	if err := r.m.CloseCycle(EndNoFill, r.clock.Now()); err == nil {
		t.Fatal("CloseCycle with a failing store returned nil")
	}
	if got := r.m.Cycle(); got == nil || got.Status != CycleOpen || got.ID != c.ID {
		t.Errorf("after a close that was not written: cycle %+v, want %s still open", got, c.ID)
	}
	r.sink.Fail(nil)
	r.closeCycle(t, EndNoFill)
	if r.m.Cycle() != nil {
		t.Error("the cycle is still open after the repeated close")
	}
}

// A remainder no order can close is only recorded (PRD 8.1). Cycle A, short,
// closes with 0.02 of its bear leg still held: the event and
// inventory_state.dust hold the leg, the signed quantity and the lot's entry
// price, the lot's and the cycle's results cover only what was closed, and
// the venue's -0.02 agrees with the ledger. The dust survives a restart.
// Cycle B, long, is booked by the normal rules alone: the venue nets its
// 33.34 bought against the -0.02, so B's reduce-only exit fills 33.32 and
// B leaves +0.02, which nets the recorded dust to zero.
//
//	A  sold 100 @ 40.00 and 66.66 @ 60.00; bought 100 @ 39.90 and 66.64 @ 59.90
//	B  bought 50 @ 40.10 and 33.34 @ 59.80; sold 50 @ 40.20 and 33.32 @ 59.75
func TestDustIsRecordedOnly(t *testing.T) {
	r := newRig(t, "1000", "10")
	a := r.openCycle(t, strategy.Short)
	la := r.openLot(t)
	r.m.Sync([]cex.OrderRecord{
		fill(ord(a.ID, 1, cex.Bull, cex.PurposeEntry, cex.SideSell, la.ID, "40", "100"), "100", "4000", "0.26"),
		fill(ord(a.ID, 2, cex.Bear, cex.PurposeEntry, cex.SideSell, la.ID, "60", "66.66"), "66.66", "3999.6", "0.259974"),
		fill(ord(a.ID, 3, cex.Bull, cex.PurposeExit, cex.SideBuy, "", "39.9", "100"), "100", "3990", "0.25935"),
		fill(ord(a.ID, 4, cex.Bear, cex.PurposeExit, cex.SideBuy, "", "59.9", "66.64"), "66.64", "3991.736", "0.25946284"),
	})
	// What the ledger expects at the venue: the short cycle's 0.02 of the
	// bear leg, negative.
	if got := r.m.LedgerPositions(); !got[cex.Bull].IsZero() || !got[cex.Bear].Equal(dec("-0.02")) {
		t.Errorf("ledger positions before A closes = %s / %s, want 0 / -0.02", got[cex.Bull], got[cex.Bear])
	}
	r.m.SettleLot(la.ID, true, r.clock.Now())
	r.clock.Add(10 * time.Minute)
	r.cex.setPositions("0", "-0.02")
	if err := r.m.Reconcile(t.Context()); err != nil {
		t.Fatalf("reconcile before A closes: %v", err)
	}
	r.closeCycle(t, EndExitLine)

	// A's results cover the 66.64 it closed: gross 100 x 0.10 + 66.64 x 0.10,
	// and of the bear entry fee the share of 66.64 in 66.66, 0.259896.
	rowA := lastCycleRow(t, r.sink, a.ID)
	wantDec(t, "A gross", rowA.Gross, "16.664")
	wantDec(t, "A fees", rowA.Fees, "1.03870884")
	wantDec(t, "A net", rowA.Net, "15.62529116")
	wantDec(t, "A dust_bear", rowA.DustBear, "-0.02")
	wantDec(t, "A dust_bull", rowA.DustBull, "0")
	lotA := lastLotRow(t, r.sink, la.ID)
	if lotA.Status != "closed" || !lotA.EntryQtyBear.Equal(dec("66.66")) || !lotA.ExitQtyBear.Equal(dec("66.64")) || !lotA.Gross.Equal(dec("16.664")) || !lotA.Fees.Equal(dec("1.03870884")) {
		t.Errorf("A lot row = status %s, bear %s in / %s out, gross %s, fees %s; want closed, 66.66 / 66.64, 16.664, 1.03870884", lotA.Status, lotA.EntryQtyBear, lotA.ExitQtyBear, lotA.Gross, lotA.Fees)
	}
	wantDec(t, "r_closed after A", rClosed(t, r.sink), "15.62529116")

	// The dust: one event, and inventory_state in the transaction of the
	// cycle's close.
	state := lastOf(t, r.sink, func(store.InventoryStateRow) bool { return true })
	if string(state.Dust) != `{"bear":{"qty":"-0.02","avg_price":"60"}}` {
		t.Errorf("inventory_state.dust = %s, want the bear leg's -0.02 at 60", state.Dust)
	}
	var tables []string
	for _, row := range r.sink.lastSync() {
		tables = append(tables, row.Table())
	}
	if !slices.Equal(tables, []string{"cycle", "lot", "inventory_state"}) {
		t.Errorf("the close was written as %v, want cycle, lot and inventory_state in one WriteSync", tables)
	}
	events := rowsOf[store.EventRow](r.sink)
	if len(events) != 1 || events[0].Kind != "dust_left" || events[0].Level != "warn" || events[0].Module != "inventory" {
		t.Fatalf("events after A = %+v, want one dust_left warning", events)
	}
	var detail map[string]string
	if err := json.Unmarshal(events[0].Detail, &detail); err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"leg": "bear", "qty": "-0.02", "avg_price": "60", "cycle_id": string(a.ID), "lot_id": string(la.ID)}; !maps.Equal(detail, want) {
		t.Errorf("dust_left detail = %v, want %v", detail, want)
	}

	// Flat with dust: the venue's -0.02 is what the ledger expects, and a
	// venue without it would differ. The dust takes no quota.
	if err := r.m.Reconcile(t.Context()); err != nil || r.m.Mismatch() {
		t.Errorf("reconcile with only the dust held: %v, mismatch %t", err, r.m.Mismatch())
	}
	r.cex.setPositions("0", "0")
	if err := r.m.Reconcile(t.Context()); !errors.Is(err, ErrPositionsDiffer) {
		t.Errorf("reconcile with the dust gone from the venue = %v, want ErrPositionsDiffer", err)
	}
	r.cex.setPositions("0", "-0.02")
	wantQuota(t, r, "with dust", "10000", "1.2") // the dust is still held: 0.02 x 60

	// A restart restores the dust from inventory_state.
	r = r.restart(t, "1000", "10")
	if err := r.m.Reconcile(t.Context()); err != nil || r.m.Mismatch() {
		t.Errorf("reconcile after the restart: %v, mismatch %t", err, r.m.Mismatch())
	}
	// The same after the restart, now as dust: what a paper account is
	// seeded with.
	if got := r.m.LedgerPositions(); !got[cex.Bull].IsZero() || !got[cex.Bear].Equal(dec("-0.02")) {
		t.Errorf("ledger positions after the restart = %s / %s, want the dust: 0 / -0.02", got[cex.Bull], got[cex.Bear])
	}
	r.m.sample(r.clock.Now())
	if s := lastOf(t, r.sink, func(store.InventorySampleRow) bool { return true }); !s.LedgerBear.Equal(dec("-0.02")) || !s.LedgerBull.IsZero() || !s.PosBear.Decimal.Equal(dec("-0.02")) {
		t.Errorf("sample after the restart = ledger %s / %s, venue bear %s; want 0 / -0.02 and -0.02", s.LedgerBull, s.LedgerBear, s.PosBear.Decimal)
	}

	// B: every fill goes to its lot; nothing is booked against the dust.
	r.clock.Add(10 * time.Minute)
	b := r.openCycle(t, strategy.Long)
	lb := r.openLot(t)
	entries := []cex.OrderRecord{
		fill(ord(b.ID, 1, cex.Bull, cex.PurposeEntry, cex.SideBuy, lb.ID, "40.1", "50"), "50", "2005", "0.130325"),
		fill(ord(b.ID, 2, cex.Bear, cex.PurposeEntry, cex.SideBuy, lb.ID, "59.8", "33.34"), "33.34", "1993.732", "0.12959258"),
	}
	r.m.Sync(entries)
	r.m.SettleLot(lb.ID, true, r.clock.Now())
	wantLeg(t, "B lot bear", r.lot(t, 1).Legs[cex.Bear], "33.34", "1993.732", "0.12959258", "0", "0", "0")
	r.cex.setPositions("50", "33.32") // the venue netted the 33.34 against its -0.02
	if got := r.m.LedgerPositions(); !got[cex.Bull].Equal(dec("50")) || !got[cex.Bear].Equal(dec("33.32")) {
		t.Errorf("ledger positions while B holds = %s / %s, want the long holdings plus the dust: 50 / 33.32", got[cex.Bull], got[cex.Bear])
	}
	if err := r.m.Reconcile(t.Context()); err != nil {
		t.Errorf("reconcile while B holds: %v", err)
	}

	r.clock.Add(10 * time.Minute)
	r.m.Sync(append(entries[:2:2],
		fill(ord(b.ID, 3, cex.Bull, cex.PurposeExit, cex.SideSell, "", "40.2", "50"), "50", "2010", "0.13065"),
		fill(ord(b.ID, 4, cex.Bear, cex.PurposeExit, cex.SideSell, "", "59.75", "33.34"), "33.32", "1990.87", "0.12940655")))
	if _, hold := r.m.Holdings(); !hold[cex.Bull].IsZero() || !hold[cex.Bear].Equal(dec("0.02")) {
		t.Errorf("B holdings after its exits = %s / %s, want 0 / 0.02", hold[cex.Bull], hold[cex.Bear])
	}
	r.cex.setPositions("0", "0")
	if err := r.m.Reconcile(t.Context()); err != nil {
		t.Errorf("reconcile before B closes: %v", err)
	}
	lotRowsOfA := len(rowsOf[store.LotRow](r.sink))
	r.closeCycle(t, EndExitLine)

	// B: gross 50 x 0.10 + (1,990.87 - 33.32 x 59.80) = 3.334; of its bear
	// entry fee the share of 33.32 in 33.34, 0.12951484.
	rowB := lastCycleRow(t, r.sink, b.ID)
	wantDec(t, "B gross", rowB.Gross, "3.334")
	wantDec(t, "B fees", rowB.Fees, "0.51989639")
	wantDec(t, "B net", rowB.Net, "2.81410361")
	wantDec(t, "B dust_bear", rowB.DustBear, "0.02")
	wantDec(t, "r_closed after B", rClosed(t, r.sink), "18.43939477")
	if state = lastOf(t, r.sink, func(store.InventoryStateRow) bool { return true }); string(state.Dust) != "{}" {
		t.Errorf("inventory_state.dust after B = %s, want none: +0.02 nets the -0.02", state.Dust)
	}
	if kinds := eventKinds(r.sink); !slices.Equal(kinds, []string{"dust_left", "dust_left"}) {
		t.Errorf("events = %v, want one dust_left per cycle", kinds)
	}
	if err := r.m.Reconcile(t.Context()); err != nil || r.m.Mismatch() {
		t.Errorf("reconcile after B: %v, mismatch %t", err, r.m.Mismatch())
	}
	// A's rows were not touched again: B's close wrote B's lot only.
	if got := len(rowsOf[store.LotRow](r.sink)); got != lotRowsOfA+1 || lastLotRow(t, r.sink, la.ID) != lotA {
		t.Errorf("B's close wrote %d lot rows, want 1, and A's lot row must be as A closed", got-lotRowsOfA)
	}
}

// Recorded dust occupies quota (PRD 6.1, 6.4): it is still held on the
// venue. A long cycle buys 100.05 bull @ 40 and sells 100: the 0.05 left,
// 2 USDT, is below the minimum order. It counts 2 while it is in its lot,
// 2 as dust after the close and after a restart, and once beside the next
// cycle's holdings.
func TestDustOccupiesQuota(t *testing.T) {
	r := newRig(t, "1000", "10")
	c := r.openCycle(t, strategy.Long)
	lot := r.openLot(t)
	r.m.Sync([]cex.OrderRecord{
		fill(ord(c.ID, 1, cex.Bull, cex.PurposeEntry, cex.SideBuy, lot.ID, "40", "100.05"), "100.05", "4002", "0"),
		fill(ord(c.ID, 2, cex.Bear, cex.PurposeEntry, cex.SideBuy, lot.ID, "60", "66.7"), "66.7", "4002", "0"),
		fill(ord(c.ID, 3, cex.Bull, cex.PurposeExit, cex.SideSell, "", "40.1", "100"), "100", "4010", "0"),
		fill(ord(c.ID, 4, cex.Bear, cex.PurposeExit, cex.SideSell, "", "60", "66.7"), "66.7", "4002", "0"),
	})
	r.m.SettleLot(lot.ID, true, r.clock.Now())
	wantQuota(t, r, "before the close, the remainder in its lot", "10000", "2")
	r.clock.Add(time.Minute)
	r.closeCycle(t, EndExitLine)
	wantQuota(t, r, "after the close, as dust", "10000", "2")
	if state := lastOf(t, r.sink, func(store.InventoryStateRow) bool { return true }); string(state.Dust) != `{"bull":{"qty":"0.05","avg_price":"40"}}` {
		t.Errorf("inventory_state.dust = %s, want 0.05 bull at 40", state.Dust)
	}
	r = r.restart(t, "1000", "10")
	wantQuota(t, r, "after a restart", "10000", "2")

	// The next cycle's entries come on top of it.
	r.clock.Add(time.Minute)
	c = r.openCycle(t, strategy.Long)
	lot = r.openLot(t)
	r.m.Sync([]cex.OrderRecord{
		fill(ord(c.ID, 1, cex.Bull, cex.PurposeEntry, cex.SideBuy, lot.ID, "40", "100"), "100", "4000", "0"),
		ord(c.ID, 2, cex.Bear, cex.PurposeEntry, cex.SideBuy, lot.ID, "60", "50"),
	})
	wantQuota(t, r, "the next cycle holding and reserving", "10000", "7002") // 2 + 4,000 + 50 x 60
}

// An entry or completion order without a final state reserves its unfilled
// quantity at its quota price: the limit of a buy, the decision's raw best
// bid of a sell, which fills at or above its limit. An order that carries
// no quota price reserves at its limit.
func TestReservationAtTheQuotaPrice(t *testing.T) {
	r := newRig(t, "1000", "10")
	c := r.openCycle(t, strategy.Short)
	lot := r.openLot(t)
	bull := ord(c.ID, 1, cex.Bull, cex.PurposeEntry, cex.SideSell, lot.ID, "40.44", "62.44")
	bull.Request.QuotaPrice = dec("40.45")
	bear := ord(c.ID, 2, cex.Bear, cex.PurposeEntry, cex.SideSell, lot.ID, "59.42", "41.62")
	bear.Request.QuotaPrice = dec("59.437")
	r.m.Sync([]cex.OrderRecord{bull, bear})
	wantQuota(t, r, "two pending sells at their best bids", "10000", "4999.46594") // 62.44 x 40.45 + 41.62 x 59.437

	// The bull order fills in part: what filled counts at its value, the
	// rest still at the quota price.
	r.m.Sync([]cex.OrderRecord{working(bull, "20", "809", "0"), bear})
	wantQuota(t, r, "one sell part filled", "10000", "4999.46594") // 809 + 42.44 x 40.45 + 2,473.76794

	// Without a quota price the limit is used.
	bear.Request.QuotaPrice, bear.UpdateSeq = decimal.Zero, recordSeq.Add(1)
	r.m.Sync([]cex.OrderRecord{fill(bull, "62.44", "2525.698", "0"), bear})
	wantQuota(t, r, "a pending sell without a quota price", "10000", "4998.7584") // 2,525.698 + 41.62 x 59.42
}

// An order recognised from the venue after a restart carries no lot id; its
// lot is decided once and never again (PRD 7.4). Batch A of a long cycle
// bought 10 bull and 4 of 5 bear before the crash; the venue also holds a
// completion of 1 bear share the process had not recorded. Restored, the
// completion belongs to A. It stays there when batch B opens and the same
// snapshot is booked again, and after another restart with the lot stamped
// on the record: A and B hold the same quantities and costs at every stage.
func TestRecognisedOrderKeepsItsLot(t *testing.T) {
	first := newRig(t, "1000", "10")
	c := first.openCycle(t, strategy.Long)
	lotA := first.openLot(t)
	orders := []cex.OrderRecord{
		fill(ord(c.ID, 1, cex.Bull, cex.PurposeEntry, cex.SideBuy, lotA.ID, "10", "10"), "10", "100", "0.01"),
		fill(ord(c.ID, 2, cex.Bear, cex.PurposeEntry, cex.SideBuy, lotA.ID, "20", "5"), "4", "80", "0.008"),
	}
	first.m.Sync(orders)
	recognised := fill(ord(c.ID, 3, cex.Bear, cex.PurposeComplete, cex.SideBuy, "", "20", "1"), "1", "20", "0.002")
	orders = append(orders, recognised)

	wantA := func(r *testRig, stage string) {
		t.Helper()
		wantLeg(t, stage+": A bull", r.lot(t, 1).Legs[cex.Bull], "10", "100", "0.01", "0", "0", "0")
		wantLeg(t, stage+": A bear", r.lot(t, 1).Legs[cex.Bear], "5", "100", "0.01", "0", "0", "0")
		if got := r.m.LotOf(recognised.Request.ClientOrderID); got != lotA.ID {
			t.Errorf("%s: the recognised order is booked on %q, want %s", stage, got, lotA.ID)
		}
	}
	wantB := func(r *testRig, stage, bullQty, bullValue, bearQty, bearValue string) {
		t.Helper()
		wantLeg(t, stage+": B bull", r.lot(t, 2).Legs[cex.Bull], bullQty, bullValue, "0", "0", "0", "0")
		wantLeg(t, stage+": B bear", r.lot(t, 2).Legs[cex.Bear], bearQty, bearValue, "0", "0", "0", "0")
	}

	// Stage 1: restored with batch A still building.
	r := first.restart(t, "1000", "10", orders...)
	wantA(r, "restored")

	// Stage 2: A is built and batch B opens; the same snapshot, with B's
	// two orders not filled yet, is booked.
	r.m.SettleLot(lotA.ID, true, r.clock.Now())
	r.clock.Add(time.Minute)
	lotB := r.openLot(t)
	bBull := ord(c.ID, 4, cex.Bull, cex.PurposeEntry, cex.SideBuy, lotB.ID, "10.1", "10")
	bBear := ord(c.ID, 5, cex.Bear, cex.PurposeEntry, cex.SideBuy, lotB.ID, "20.1", "5")
	r.m.Sync(append(orders[:3:3], bBull, bBear))
	wantA(r, "B opened")
	wantB(r, "B opened", "0", "0", "0", "0")

	// Stage 3: the snapshot again, now with B's fills.
	orders = append(orders, fill(bBull, "10", "101", "0"), fill(bBear, "5", "100.5", "0"))
	r.m.Sync(orders)
	r.m.Sync(orders)
	wantA(r, "snapshot booked again")
	wantB(r, "snapshot booked again", "10", "101", "5", "100.5")

	// Stage 4: another restart while B is still building. The recovery
	// stamped the lot on the record, which is what cex_order.lot_id holds.
	stamped := slices.Clone(orders)
	stamped[2].Request.LotID = string(r.m.LotOf(recognised.Request.ClientOrderID))
	again := r.restart(t, "1000", "10", stamped...)
	wantA(again, "restarted with the lot stamped")
	wantB(again, "restarted with the lot stamped", "10", "101", "5", "100.5")
	if dir, hold := again.m.Holdings(); dir != strategy.Long || !hold[cex.Bull].Equal(dec("20")) || !hold[cex.Bear].Equal(dec("10")) {
		t.Errorf("holdings after the second restart = %s / %s, want 20 / 10", hold[cex.Bull], hold[cex.Bear])
	}
}

// The recorded dust of a leg is a running net: the same side adds at the
// quantity-weighted price, the other side reduces and what remains keeps
// its price, and past zero what remains takes the new price.
func TestDustNet(t *testing.T) {
	steps := []struct{ qty, price, wantQty, wantPrice string }{
		{"-0.02", "60", "-0.02", "60"},
		{"-0.03", "61", "-0.05", "60.6"}, // (0.02 x 60 + 0.03 x 61) / 0.05
		{"0.01", "59", "-0.04", "60.6"},
		{"0.06", "58", "0.02", "58"},
		{"-0.02", "57", "0", "0"},
	}
	var d Dust
	for i, s := range steps {
		d = d.add(dec(s.qty), dec(s.price))
		if !d.Qty.Equal(dec(s.wantQty)) || !d.AvgPrice.Equal(dec(s.wantPrice)) {
			t.Errorf("step %d (%s @ %s): dust = %s @ %s, want %s @ %s", i+1, s.qty, s.price, d.Qty, d.AvgPrice, s.wantQty, s.wantPrice)
		}
	}
}

// The last order reports its fill before its fee. Quantities are booked at
// once, but the cycle cannot close until the fee is confirmed, and the
// final net and R hold it. A fee confirmed as zero is not a missing fee.
func TestFeeArrivingAfterTheLastFill(t *testing.T) {
	logs := captureLogs(t)
	r := newRig(t, "1000", "10")
	c := r.openCycle(t, strategy.Long)
	lot := r.openLot(t)
	orders := []cex.OrderRecord{
		fill(ord(c.ID, 1, cex.Bull, cex.PurposeEntry, cex.SideBuy, lot.ID, "10", "10"), "10", "100", "0.0065"),
		fill(ord(c.ID, 2, cex.Bear, cex.PurposeEntry, cex.SideBuy, lot.ID, "20", "5"), "5", "100", "0.0065"),
		fill(ord(c.ID, 3, cex.Bull, cex.PurposeExit, cex.SideSell, "", "10.1", "10"), "10", "101", "0.006565"),
	}
	exitBear := ord(c.ID, 4, cex.Bear, cex.PurposeExit, cex.SideSell, "", "20.1", "5")
	early := fill(exitBear, "5", "100.5", "0")
	early.FeeKnown = false
	r.m.Sync(append(orders[:3:3], early))
	r.m.SettleLot(lot.ID, true, r.clock.Now())

	if _, hold := r.m.Holdings(); !hold[cex.Bull].IsZero() || !hold[cex.Bear].IsZero() {
		t.Errorf("holdings = %s / %s, want 0 / 0: the fill counts before its fee", hold[cex.Bull], hold[cex.Bear])
	}
	// The lot holds nothing and is not closed yet: its result is not final.
	if l := r.lot(t, 1); l.Status != LotOpen || len(logs.lines("inventory: lot closed")) != 0 {
		t.Errorf("lot before its last fee: status %s, %d closed lines; want open and none", l.Status, len(logs.lines("inventory: lot closed")))
	}
	if r.m.Settled() {
		t.Error("Settled() = true while a filled order's fee is not confirmed")
	}
	if err := r.m.CloseCycle(EndExitLine, r.clock.Now()); !errors.Is(err, ErrNotSettled) {
		t.Fatalf("CloseCycle before the fee = %v, want ErrNotSettled", err)
	}
	if got := r.m.Cycle(); got == nil || got.Status != CycleOpen {
		t.Fatal("the cycle did not stay open")
	}
	wantDec(t, "lot fees before the late fee", r.lot(t, 1).Fees, "0.019565")

	r.m.Sync(append(orders[:3:3], fill(exitBear, "5", "100.5", "0.0065325")))
	if !r.m.Settled() {
		t.Error("Settled() = false after the fee was confirmed")
	}
	// Closed now, logged once, with the late fee in its net.
	if l := r.lot(t, 1); l.Status != LotClosed {
		t.Errorf("lot after its last fee: status %s, want closed", l.Status)
	}
	if lines := logs.lines("inventory: lot closed"); len(lines) != 1 || !strings.Contains(lines[0], "net_usdt=1.4739025") || !strings.Contains(lines[0], "from=open to=closed") {
		t.Errorf("lot closed lines = %q, want one, from open to closed with net 1.4739025", lines)
	}
	for _, row := range rowsOf[store.LotRow](r.sink) {
		if row.Status == "closed" && !row.Net.Equal(dec("1.4739025")) {
			t.Errorf("a lot row was written closed with net %s, want only the final 1.4739025", row.Net)
		}
	}
	r.closeCycle(t, EndExitLine)
	if lines := logs.lines("inventory: lot closed"); len(lines) != 1 {
		t.Errorf("lot closed was logged %d times in all, want once", len(lines))
	}
	row := lastCycleRow(t, r.sink, c.ID)
	wantDec(t, "cycle fees", row.Fees, "0.0260975")
	wantDec(t, "cycle net", row.Net, "1.4739025")
	wantDec(t, "r_closed", rClosed(t, r.sink), "1.4739025")
	wantDec(t, "lot net row", lastLotRow(t, r.sink, lot.ID).Net, "1.4739025")

	// The same cycle with the last fee confirmed as zero closes at once.
	z := newRig(t, "1000", "10")
	c = z.openCycle(t, strategy.Long)
	lot = z.openLot(t)
	z.m.Sync([]cex.OrderRecord{
		fill(ord(c.ID, 1, cex.Bull, cex.PurposeEntry, cex.SideBuy, lot.ID, "10", "10"), "10", "100", "0.0065"),
		fill(ord(c.ID, 2, cex.Bear, cex.PurposeEntry, cex.SideBuy, lot.ID, "20", "5"), "5", "100", "0.0065"),
		fill(ord(c.ID, 3, cex.Bull, cex.PurposeExit, cex.SideSell, "", "10.1", "10"), "10", "101", "0.006565"),
		fill(ord(c.ID, 4, cex.Bear, cex.PurposeExit, cex.SideSell, "", "20.1", "5"), "5", "100.5", "0")})
	if !z.m.Settled() {
		t.Error("Settled() = false with a fee confirmed as zero")
	}
	z.closeCycle(t, EndExitLine)
	wantDec(t, "r_closed with a zero fee", rClosed(t, z.sink), "1.480435")

	// An order without a final state keeps the cycle open as well.
	p := newRig(t, "1000", "10")
	c = p.openCycle(t, strategy.Long)
	lot = p.openLot(t)
	p.m.Sync([]cex.OrderRecord{ord(c.ID, 1, cex.Bull, cex.PurposeEntry, cex.SideBuy, lot.ID, "10", "10")})
	if err := p.m.CloseCycle(EndNoFill, p.clock.Now()); !errors.Is(err, ErrNotSettled) || p.m.Settled() {
		t.Errorf("CloseCycle with a pending order = %v (settled %t), want ErrNotSettled", err, p.m.Settled())
	}
}

// A trim larger than what its batch holds on that leg (PRD 7.4). The gap is
// the whole cycle's: lot 1, short, was built with 100.01 bull against 66.66
// bear, 0.8 USD of anchored amount too little to send an order for. Lot 2
// fills only its bull leg, 50 @ 41, and the trim buys back the whole gap,
// 50.02 @ 41.1. 50 of it close lot 2; the other 0.02 close lot 1, first in,
// first out. Anchors 40 / 60, no fees.
func TestTrimLargerThanItsBatch(t *testing.T) {
	r := newRig(t, "1000", "10")
	c := r.openCycle(t, strategy.Short)
	lot1 := r.openLot(t)
	orders := []cex.OrderRecord{
		fill(ord(c.ID, 1, cex.Bull, cex.PurposeEntry, cex.SideSell, lot1.ID, "40", "100.01"), "100.01", "4000.4", "0"),
		fill(ord(c.ID, 2, cex.Bear, cex.PurposeEntry, cex.SideSell, lot1.ID, "60", "66.66"), "66.66", "3999.6", "0"),
	}
	r.m.Sync(orders)
	r.m.SettleLot(lot1.ID, true, r.clock.Now())

	r.clock.Add(time.Minute)
	lot2 := r.openLot(t)
	orders = append(orders,
		fill(ord(c.ID, 3, cex.Bull, cex.PurposeEntry, cex.SideSell, lot2.ID, "41", "50"), "50", "2050", "0"),
		fill(ord(c.ID, 4, cex.Bear, cex.PurposeEntry, cex.SideSell, lot2.ID, "60", "33.33"), "0", "0", "0"),
		fill(ord(c.ID, 5, cex.Bull, cex.PurposeTrim, cex.SideBuy, lot2.ID, "41.1", "50.02"), "50.02", "2055.822", "0"))
	r.m.Sync(orders)
	r.m.SettleLot(lot2.ID, false, r.clock.Now())

	// Lot 2 is closed exactly: 50 x 41.1 = 2,055 of the trim. Lot 1 takes
	// the other 0.02 for 0.822.
	l1, l2 := r.lot(t, 1), r.lot(t, 2)
	wantLeg(t, "lot 2 bull", l2.Legs[cex.Bull], "50", "2050", "0", "50", "2055", "0")
	wantLeg(t, "lot 1 bull", l1.Legs[cex.Bull], "100.01", "4000.4", "0", "0.02", "0.822", "0")
	if l2.Status != LotVoid || l2.VoidReason != VoidOneLeg || !l2.flat() {
		t.Errorf("lot 2: status %s, reason %s, flat %t; want void, one_leg, flat", l2.Status, l2.VoidReason, l2.flat())
	}
	wantDec(t, "lot 2 gross", l2.Gross, "-5") // sold 50 at 41, bought back at 41.1
	if dir, hold := r.m.Holdings(); dir != strategy.Short || !hold[cex.Bull].Equal(dec("99.99")) || !hold[cex.Bear].Equal(dec("66.66")) {
		t.Errorf("holdings after the trim = %s / %s, want 99.99 / 66.66", hold[cex.Bull], hold[cex.Bear])
	}
	wantQuota(t, r, "after the trim", "9949.78", "7999.2") // R = -5 - 0.022; 99.99 x 40 + 66.66 x 60

	// The exit closes what is held, 99.99 @ 39.9 and 66.66 @ 59.9.
	r.clock.Add(time.Minute)
	orders = append(orders,
		fill(ord(c.ID, 6, cex.Bull, cex.PurposeExit, cex.SideBuy, "", "39.9", "99.99"), "99.99", "3989.601", "0"),
		fill(ord(c.ID, 7, cex.Bear, cex.PurposeExit, cex.SideBuy, "", "59.9", "66.66"), "66.66", "3992.934", "0"))
	r.m.Sync(orders)
	if l := r.lot(t, 1); l.Status != LotClosed || !l.flat() {
		t.Errorf("lot 1 after the exit: status %s, flat %t; want closed and flat", l.Status, l.flat())
	}
	wantDec(t, "lot 1 gross", r.lot(t, 1).Gross, "16.643") // 4,000.4 - 0.822 - 3,989.601 and 3,999.6 - 3,992.934
	r.cex.setPositions("0", "0")
	if err := r.m.Reconcile(t.Context()); err != nil {
		t.Errorf("reconcile before the close: %v", err)
	}
	r.closeCycle(t, EndExitLine)

	// The cycle is what its orders are: sold 10,050, bought back
	// 10,038.357. Nothing is left, so there is no dust.
	row := lastCycleRow(t, r.sink, c.ID)
	wantDec(t, "cycle gross", row.Gross, "11.643")
	wantDec(t, "r_closed", rClosed(t, r.sink), "11.643")
	if !row.DustBull.IsZero() || !row.DustBear.IsZero() || len(eventKinds(r.sink)) != 0 {
		t.Errorf("dust after the close: %s / %s, events %v; want none", row.DustBull, row.DustBear, eventKinds(r.sink))
	}
	if state := lastOf(t, r.sink, func(store.InventoryStateRow) bool { return true }); string(state.Dust) != "{}" {
		t.Errorf("inventory_state.dust = %s, want none", state.Dust)
	}
}

// An order recognised from the venue after a restart carries no lot id. A
// completion or a trim without one is booked on the last lot still
// building, the only one that can be completed or trimmed at any moment;
// with no lot building, on the newest lot.
func TestOrderWithoutLotID(t *testing.T) {
	r := newRig(t, "1000", "10")
	c := r.openCycle(t, strategy.Long)
	lot1 := r.openLot(t)
	orders := []cex.OrderRecord{
		fill(ord(c.ID, 1, cex.Bull, cex.PurposeEntry, cex.SideBuy, lot1.ID, "10", "10"), "10", "100", "0"),
		fill(ord(c.ID, 2, cex.Bear, cex.PurposeEntry, cex.SideBuy, lot1.ID, "20", "5"), "5", "100", "0"),
	}
	r.m.Sync(orders)
	r.m.SettleLot(lot1.ID, true, r.clock.Now())
	r.clock.Add(time.Minute)
	lot2 := r.openLot(t)
	orders = append(orders,
		fill(ord(c.ID, 3, cex.Bull, cex.PurposeEntry, cex.SideBuy, lot2.ID, "10", "4"), "4", "40", "0"),
		fill(ord(c.ID, 4, cex.Bear, cex.PurposeEntry, cex.SideBuy, lot2.ID, "20", "2"), "0", "0", "0"),
		fill(ord(c.ID, 5, cex.Bear, cex.PurposeComplete, cex.SideBuy, "", "20", "1"), "1", "20", "0"),
		fill(ord(c.ID, 6, cex.Bull, cex.PurposeTrim, cex.SideSell, "", "10", "2"), "2", "20", "0"))
	r.m.Sync(orders)
	wantLeg(t, "lot 2 bull, trimmed while building", r.lot(t, 2).Legs[cex.Bull], "4", "40", "0", "2", "20", "0")
	wantLeg(t, "lot 2 bear, completed while building", r.lot(t, 2).Legs[cex.Bear], "1", "20", "0", "0", "0", "0")
	wantLeg(t, "lot 1 bull, untouched", r.lot(t, 1).Legs[cex.Bull], "10", "100", "0", "0", "0", "0")
	wantLeg(t, "lot 1 bear, untouched", r.lot(t, 1).Legs[cex.Bear], "5", "100", "0", "0", "0", "0")

	// No lot is building any more: the newest lot takes it.
	r.m.SettleLot(lot2.ID, true, r.clock.Now())
	orders = append(orders, fill(ord(c.ID, 7, cex.Bull, cex.PurposeTrim, cex.SideSell, "", "10", "1"), "1", "10", "0"))
	r.m.Sync(orders)
	wantLeg(t, "lot 2 bull, trimmed once built", r.lot(t, 2).Legs[cex.Bull], "4", "40", "0", "3", "30", "0")
	wantLeg(t, "lot 1 bull, still untouched", r.lot(t, 1).Legs[cex.Bull], "10", "100", "0", "0", "0", "0")
}

// The close writes every lot row of the cycle in its transaction, so a lot
// row the asynchronous queue dropped earlier is repaired when the cycle
// ends.
func TestCloseCycleWritesEveryLotRow(t *testing.T) {
	r := newRig(t, "1000", "10")
	c := r.openCycle(t, strategy.Long)
	lot1 := r.openLot(t)
	orders := []cex.OrderRecord{
		fill(ord(c.ID, 1, cex.Bull, cex.PurposeEntry, cex.SideBuy, lot1.ID, "10", "10"), "10", "100", "0.01"),
		fill(ord(c.ID, 2, cex.Bear, cex.PurposeEntry, cex.SideBuy, lot1.ID, "20", "5"), "5", "100", "0.01"),
	}
	r.m.Sync(orders)
	r.m.SettleLot(lot1.ID, true, r.clock.Now())
	r.clock.Add(time.Minute)
	lot2 := r.openLot(t)
	orders = append(orders,
		fill(ord(c.ID, 3, cex.Bull, cex.PurposeEntry, cex.SideBuy, lot2.ID, "10", "10"), "10", "100", "0.01"),
		fill(ord(c.ID, 4, cex.Bear, cex.PurposeEntry, cex.SideBuy, lot2.ID, "20", "5"), "5", "100", "0.01"))
	r.m.Sync(orders)
	r.m.SettleLot(lot2.ID, true, r.clock.Now())

	// The queue is full while the exits are booked: the rows that say both
	// lots are closed never reach the database.
	r.sink.dropRecords(true)
	orders = append(orders,
		fill(ord(c.ID, 5, cex.Bull, cex.PurposeExit, cex.SideSell, "", "10.1", "20"), "20", "202", "0.02"),
		fill(ord(c.ID, 6, cex.Bear, cex.PurposeExit, cex.SideSell, "", "20", "10"), "10", "200", "0.02"))
	r.m.Sync(orders)
	r.sink.dropRecords(false)
	if row := lastLotRow(t, r.sink, lot1.ID); row.Status != "open" || !row.ExitQtyBull.IsZero() {
		t.Fatalf("the stale lot row is not stale: %+v", row)
	}
	r.m.Sync(orders) // the same snapshot: nothing changed, nothing is recorded again
	if row := lastLotRow(t, r.sink, lot1.ID); row.Status != "open" {
		t.Fatalf("an unchanged snapshot rewrote the lot row: %+v", row)
	}

	r.closeCycle(t, EndExitLine)
	var lots []string
	for _, row := range r.sink.lastSync() {
		if l, ok := row.(store.LotRow); ok {
			lots = append(lots, l.LotID)
		}
	}
	if !slices.Equal(lots, []string{string(lot1.ID), string(lot2.ID)}) {
		t.Errorf("the close wrote lot rows %v, want both lots", lots)
	}
	sum := decimal.Zero
	for _, id := range []LotID{lot1.ID, lot2.ID} {
		row := lastLotRow(t, r.sink, id)
		if row.Status != "closed" || !row.ExitQtyBull.Equal(dec("10")) || !row.Net.Equal(dec("0.96")) { // 1 - 4 x 0.01
			t.Errorf("lot %s after the close: status %s, exit %s, net %s; want closed, 10, 0.96", id, row.Status, row.ExitQtyBull, row.Net)
		}
		sum = sum.Add(row.Net)
	}
	if row := lastCycleRow(t, r.sink, c.ID); !row.Net.Equal(sum) || row.Status != "closed" {
		t.Errorf("stored cycle net %s, stored lots add up to %s", row.Net, sum)
	}
}

// share.take gives every part its proportion and the last part the exact
// remainder.
func TestShareTake(t *testing.T) {
	s := share{qty: dec("3"), value: dec("100"), fee: dec("0.01")}
	a := s.take(dec("1"))
	b := s.take(dec("1"))
	c := s.take(dec("1"))
	for i, got := range []share{a, b, c} {
		want := [][2]string{{"33.33333333", "0.00333333"}, {"33.33333334", "0.00333334"}, {"33.33333333", "0.00333333"}}[i]
		if !got.value.Equal(dec(want[0])) || !got.fee.Equal(dec(want[1])) {
			t.Errorf("part %d = value %s fee %s, want %s and %s", i+1, got.value, got.fee, want[0], want[1])
		}
	}
	if sum := a.value.Add(b.value).Add(c.value); !sum.Equal(dec("100")) {
		t.Errorf("the values add up to %s, want 100", sum)
	}
	if sum := decimal.Sum(a.fee, b.fee, c.fee); !sum.Equal(dec("0.01")) {
		t.Errorf("the fees add up to %s, want 0.01", sum)
	}
	if !s.qty.IsZero() || !s.value.IsZero() || !s.fee.IsZero() {
		t.Errorf("left after the last part: %+v, want nothing", s)
	}
}

// An exit fill beyond what the lots hold (an earlier fill of the leg not
// known yet) is not booked: a warning once per quantity, not per sync.
func TestExitBeyondTheHoldingIsWarned(t *testing.T) {
	r := newRig(t, "1000", "10")
	c := r.openCycle(t, strategy.Long)
	lot := r.openLot(t)
	bull := fill(ord(c.ID, 1, cex.Bull, cex.PurposeEntry, cex.SideBuy, lot.ID, "10", "10"), "10", "100", "0.01")
	exit := fill(ord(c.ID, 2, cex.Bull, cex.PurposeExit, cex.SideSell, "", "10", "12"), "12", "120", "0.01")
	logs := captureLogs(t)
	r.m.Sync([]cex.OrderRecord{bull, exit})
	r.m.Sync([]cex.OrderRecord{bull, exit})
	if got := logs.lines("exit fill beyond"); len(got) != 1 || !strings.Contains(got[0], "qty=2") {
		t.Errorf("warnings %q, want one for the 2 not booked", got)
	}
	if _, hold := r.m.Holdings(); !hold[cex.Bull].IsZero() {
		t.Errorf("SOXL held %s, want 0", hold[cex.Bull])
	}
}
