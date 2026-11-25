package inventory

import (
	"errors"
	"slices"
	"testing"
	"time"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/store"
	"etf-pair-arbitrage/core/strategy"
)

// wantQuota fails unless the quota is exactly total and used.
func wantQuota(t *testing.T, r *testRig, when, total, used string) {
	t.Helper()
	gotTotal, gotUsed, gotLeft := r.m.Quota()
	if !gotTotal.Equal(dec(total)) || !gotUsed.Equal(dec(used)) || !gotLeft.Equal(dec(total).Sub(dec(used))) {
		t.Errorf("%s: quota = %s total, %s used, %s left; want %s total, %s used", when, gotTotal, gotUsed, gotLeft, total, used)
	}
}

// roundTrip plays one whole long cycle without fees: 100 bull bought for
// 4,000 and 50 bear for 3,000, sold again for the two given amounts.
func roundTrip(t *testing.T, r *testRig, bullSold, bearSold string) {
	t.Helper()
	r.clock.Add(time.Minute)
	c := r.openCycle(t, strategy.Long)
	lot := r.openLot(t)
	r.m.Sync([]cex.OrderRecord{
		fill(ord(c.ID, 1, cex.Bull, cex.PurposeEntry, cex.SideBuy, lot.ID, "40", "100"), "100", "4000", "0"),
		fill(ord(c.ID, 2, cex.Bear, cex.PurposeEntry, cex.SideBuy, lot.ID, "60", "50"), "50", "3000", "0"),
		fill(ord(c.ID, 3, cex.Bull, cex.PurposeExit, cex.SideSell, "", "40", "100"), "100", bullSold, "0"),
		fill(ord(c.ID, 4, cex.Bear, cex.PurposeExit, cex.SideSell, "", "60", "50"), "50", bearSold, "0"),
	})
	r.m.SettleLot(lot.ID, true, r.clock.Now())
	r.closeCycle(t, EndExitLine)
}

// The quota (V7, PRD section 6) with M = 1,000 and L = 10. Entry and
// completion orders without a final state reserve their unfilled quantity
// at their limit price and release it at a final state; what is held counts
// at its entry price. A loss lowers the total, earning it back restores it,
// a profit never raises it above L x M. R survives a restart and restarts
// from zero when the capital changes.
func TestQuota(t *testing.T) {
	r := newRig(t, "1000", "10")
	wantQuota(t, r, "at start", "10000", "0")

	// Two entry orders sent, nothing known of them yet.
	c := r.openCycle(t, strategy.Long)
	lot := r.openLot(t)
	wantQuota(t, r, "lot opened, no order snapshot yet", "10000", "0")
	bull := ord(c.ID, 1, cex.Bull, cex.PurposeEntry, cex.SideBuy, lot.ID, "40", "100")
	bear := ord(c.ID, 2, cex.Bear, cex.PurposeEntry, cex.SideBuy, lot.ID, "60", "50")
	r.m.Sync([]cex.OrderRecord{bull, bear})
	wantQuota(t, r, "two pending entry orders", "10000", "7000") // 100 x 40 + 50 x 60

	// The bull order has filled 40 for 1,600 and is still working: what
	// filled counts at its price, the other 60 at the limit.
	r.m.Sync([]cex.OrderRecord{working(bull, "40", "1600", "0"), bear})
	wantQuota(t, r, "bull part filled, still working", "10000", "7000") // 1,600 + 60 x 40 + 3,000

	// Final states release what did not fill.
	bullDone := fill(bull, "40", "1600", "0")
	r.m.Sync([]cex.OrderRecord{bullDone, bear})
	wantQuota(t, r, "bull final with 40 filled", "10000", "4600")
	bearDone := fill(bear, "50", "2995", "0") // filled better than its limit
	r.m.Sync([]cex.OrderRecord{bullDone, bearDone})
	wantQuota(t, r, "both final", "10000", "4595")

	// A completion order reserves while it is pending; a venue's refusal is
	// a final state. A pending exit order reserves nothing.
	complete := ord(c.ID, 3, cex.Bull, cex.PurposeComplete, cex.SideBuy, lot.ID, "40.1", "60")
	r.m.Sync([]cex.OrderRecord{bullDone, bearDone, complete})
	wantQuota(t, r, "pending completion", "10000", "7001") // + 60 x 40.1
	complete.Status, complete.UpdateSeq = cex.StatusRejected, recordSeq.Add(1)
	r.m.Sync([]cex.OrderRecord{bullDone, bearDone, complete})
	wantQuota(t, r, "completion rejected", "10000", "4595")
	r.m.SettleLot(lot.ID, true, r.clock.Now())
	exitBull := ord(c.ID, 4, cex.Bull, cex.PurposeExit, cex.SideSell, "", "39", "40")
	r.m.Sync([]cex.OrderRecord{bullDone, bearDone, complete, exitBull})
	wantQuota(t, r, "pending exit", "10000", "4595")

	// The exits lose 20 and 10: R is -30 as soon as they are booked, and
	// the total falls by L x 30.
	r.m.Sync([]cex.OrderRecord{bullDone, bearDone, complete,
		fill(exitBull, "40", "1580", "0"),
		fill(ord(c.ID, 5, cex.Bear, cex.PurposeExit, cex.SideSell, "", "59", "50"), "50", "2985", "0")})
	wantQuota(t, r, "loss of 30 realised, cycle still open", "9700", "0")
	r.closeCycle(t, EndExitLine)
	wantQuota(t, r, "after the losing cycle", "9700", "0")
	wantDec(t, "r_closed", rClosed(t, r.sink), "-30")

	// A restart with the same capital keeps R.
	r = r.restart(t, "1000", "10")
	wantQuota(t, r, "restarted with the same capital", "9700", "0")

	// Earning back restores the quota, up to L x M and no further.
	roundTrip(t, r, "4020", "3000")
	wantQuota(t, r, "20 earned back", "9900", "0")
	roundTrip(t, r, "4060", "3000")
	wantQuota(t, r, "R at +50", "10000", "0")
	wantDec(t, "r_closed", rClosed(t, r.sink), "50")
	roundTrip(t, r, "3940", "3000")
	wantQuota(t, r, "R at -10 again", "9900", "0")

	// A changed capital restarts R from zero, with an event.
	r = r.restart(t, "2000", "10")
	wantQuota(t, r, "restarted with capital 2000", "20000", "0")
	if kinds := eventKinds(r.sink); !slices.Contains(kinds, "capital_changed") {
		t.Errorf("events = %v, want capital_changed", kinds)
	}
	state := lastOf(t, r.sink, func(store.InventoryStateRow) bool { return true })
	if !state.Capital.Equal(dec("2000")) || !state.RClosed.IsZero() {
		t.Errorf("inventory_state after the change = capital %s, r_closed %s; want 2000, 0", state.Capital, state.RClosed)
	}
	roundTrip(t, r, "3990", "3000")
	wantQuota(t, r, "a loss of 10 under the new capital", "19900", "0")
}

// A capital changed while a cycle is unfinished (PRD 6.3). The cycle's
// results belong to the old capital, so the new one waits: the ledger goes
// on with the stored capital and R, opens no lot, still books reductions,
// and switches when the cycle closes, in the transaction of the close.
//
// M = 1,000, L = 10. A long cycle bought 10 bull for 400 and 5 bear for 300
// and sold half of each for 195 and 145: -10 realised, 350 still held. Then
// M is set to 2,000 and the process restarts.
func TestCapitalChangedWithACycleUnfinished(t *testing.T) {
	r := newRig(t, "1000", "10")
	r = r.restart(t, "1000", "10") // a ledger started at 01:00
	c := r.openCycle(t, strategy.Long)
	lot := r.openLot(t)
	orders := []cex.OrderRecord{
		fill(ord(c.ID, 1, cex.Bull, cex.PurposeEntry, cex.SideBuy, lot.ID, "40", "10"), "10", "400", "0"),
		fill(ord(c.ID, 2, cex.Bear, cex.PurposeEntry, cex.SideBuy, lot.ID, "60", "5"), "5", "300", "0"),
		fill(ord(c.ID, 3, cex.Bull, cex.PurposeExit, cex.SideSell, "", "39", "5"), "5", "195", "0"),
		fill(ord(c.ID, 4, cex.Bear, cex.PurposeExit, cex.SideSell, "", "58", "2.5"), "2.5", "145", "0"),
	}
	r.m.Sync(orders)
	r.m.SettleLot(lot.ID, true, r.clock.Now())
	wantQuota(t, r, "before the change", "9900", "350")

	// (a) Restarted with M = 2,000: the old baseline stands, 10 x (1,000 -
	// 10), entries are blocked and a reduction is booked.
	r.clock.Add(time.Hour)
	r = r.restart(t, "2000", "10", orders...)
	wantQuota(t, r, "restarted with the new capital, cycle unfinished", "9900", "350")
	if !r.m.CapitalPending() {
		t.Error("CapitalPending() = false with the configured capital waiting")
	}
	if l, err := r.m.OpenLot(&strategy.EntryPlan{Direction: strategy.Long}, r.clock.Now()); !errors.Is(err, ErrCapitalPending) || l != nil {
		t.Errorf("OpenLot while the capital is pending = %v, %v; want ErrCapitalPending", l, err)
	}
	if kinds := eventKinds(r.sink); slices.Contains(kinds, "capital_changed") {
		t.Errorf("events = %v: the capital has not changed yet", kinds)
	}
	orders = append(orders, fill(ord(c.ID, 5, cex.Bull, cex.PurposeExit, cex.SideSell, "", "40", "5"), "5", "200", "0"))
	r.m.Sync(orders)
	wantQuota(t, r, "a reduction booked while pending", "9900", "150")
	if state := lastOf(t, r.sink, func(store.InventoryStateRow) bool { return true }); !state.Capital.Equal(dec("1000")) || state.SinceMs != t0.UnixMilli() {
		t.Errorf("inventory_state while pending = capital %s since %d; want the old 1000 and %d", state.Capital, state.SinceMs, t0.UnixMilli())
	}

	// (c) Another restart before the cycle closes: still pending.
	r = r.restart(t, "2000", "10", orders...)
	wantQuota(t, r, "restarted again, still unfinished", "9900", "150")
	if !r.m.CapitalPending() {
		t.Error("CapitalPending() = false after a second restart")
	}

	// (b) The cycle closes: the new capital takes effect there.
	closeAt := r.clock.Add(time.Hour)
	r.m.Sync(append(orders, fill(ord(c.ID, 6, cex.Bear, cex.PurposeExit, cex.SideSell, "", "60", "2.5"), "2.5", "150", "0")))
	r.closeCycle(t, EndExitLine)
	var tables []string
	for _, row := range r.sink.lastSync() {
		tables = append(tables, row.Table())
	}
	if !slices.Equal(tables, []string{"cycle", "lot", "event", "inventory_state"}) {
		t.Errorf("the close was written as %v, want cycle, lot, event and inventory_state in one WriteSync", tables)
	}
	state := lastOf(t, r.sink, func(store.InventoryStateRow) bool { return true })
	if !state.Capital.Equal(dec("2000")) || !state.RClosed.IsZero() || state.SinceMs != closeAt.UnixMilli() {
		t.Errorf("inventory_state after the close = capital %s, r_closed %s, since %d; want 2000, 0, %d", state.Capital, state.RClosed, state.SinceMs, closeAt.UnixMilli())
	}
	wantDec(t, "the old cycle's net", lastCycleRow(t, r.sink, c.ID).Net, "-10")
	wantQuota(t, r, "after the close", "20000", "0")
	if r.m.CapitalPending() {
		t.Error("CapitalPending() = true after the cycle closed")
	}
	events := 0
	for _, kind := range eventKinds(r.sink) {
		if kind == "capital_changed" {
			events++
		}
	}
	if events != 1 {
		t.Errorf("capital_changed events = %d, want 1", events)
	}

	// Entries are allowed again, and a restart finds nothing to change.
	r.clock.Add(time.Minute)
	r.openCycle(t, strategy.Long)
	if _, err := r.m.OpenLot(&strategy.EntryPlan{Direction: strategy.Long}, r.clock.Now()); err != nil {
		t.Errorf("OpenLot under the new capital: %v", err)
	}
	r.closeCycle(t, EndNoFill)
	r = r.restart(t, "2000", "10")
	wantQuota(t, r, "restarted under the new capital", "20000", "0")
	if r.m.CapitalPending() || len(rowsOf[store.EventRow](r.sink)) != 1 {
		t.Errorf("after the last restart: pending %t, %d events; want not pending and the one event", r.m.CapitalPending(), len(rowsOf[store.EventRow](r.sink)))
	}
}
