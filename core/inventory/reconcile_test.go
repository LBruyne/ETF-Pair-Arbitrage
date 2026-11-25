package inventory

import (
	"errors"
	"testing"
	"time"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/store"
	"etf-pair-arbitrage/core/strategy"
)

// reconcile runs one Reconcile and fails unless it returns want (nil for
// agreement) and Mismatch is as given.
func reconcile(t *testing.T, r *testRig, when string, want error, mismatch bool) {
	t.Helper()
	err := r.m.Reconcile(t.Context())
	if (want == nil && err != nil) || (want != nil && !errors.Is(err, want)) {
		t.Errorf("%s: Reconcile = %v, want %v", when, err, want)
	}
	if got := r.m.Mismatch(); got != mismatch {
		t.Errorf("%s: Mismatch = %t, want %t", when, got, mismatch)
	}
}

// Reconciliation (PRD 13.4, 13.7): equal positions are fine; a difference an
// order without a final state explains is fine; a real difference is a
// mismatch only when a second read, a second later, still differs;
// positions that cannot be read are neither flat nor a mismatch.
func TestReconcile(t *testing.T) {
	r := newRig(t, "1000", "10")
	reconcile(t, r, "flat on both sides", nil, false)

	// A long cycle: the bull entry filled 10, the bear entry of 5 has no
	// final state.
	c := r.openCycle(t, strategy.Long)
	lot := r.openLot(t)
	bull := fill(ord(c.ID, 1, cex.Bull, cex.PurposeEntry, cex.SideBuy, lot.ID, "10", "10"), "10", "100", "0.01")
	bear := ord(c.ID, 2, cex.Bear, cex.PurposeEntry, cex.SideBuy, lot.ID, "20", "5")
	r.m.Sync([]cex.OrderRecord{bull, bear})

	r.cex.setPositions("10", "0")
	reconcile(t, r, "venue equals the ledger", nil, false)
	r.cex.setPositions("10", "3")
	reconcile(t, r, "3 bear the pending buy of 5 explains", nil, false)
	r.cex.setPositions("10", "5")
	reconcile(t, r, "the whole pending buy filled on the venue", nil, false)

	// The positions cannot be read: an error, and not a mismatch however
	// long it lasts.
	r.cex.failReads(errTest)
	reconcile(t, r, "read failed", ErrPositionsUnread, false)
	r.clock.Add(5 * time.Second)
	reconcile(t, r, "read failed again", ErrPositionsUnread, false)
	r.cex.failReads(nil)

	// More than the order could have filled: a difference, a mismatch only
	// when it lasts.
	r.cex.setPositions("10", "6")
	reconcile(t, r, "first differing read", ErrPositionsDiffer, false)
	r.clock.Add(500 * time.Millisecond)
	reconcile(t, r, "differs half a second later", ErrPositionsDiffer, false)
	r.cex.setPositions("10", "3")
	reconcile(t, r, "agrees again", nil, false)
	r.clock.Add(time.Second)

	// A difference against the side of the pending order is not explained.
	// A failed read in between is no second reading.
	r.cex.setPositions("10", "-1")
	reconcile(t, r, "wrong side, first read", ErrPositionsDiffer, false)
	r.clock.Add(time.Second)
	r.cex.failReads(errTest)
	reconcile(t, r, "the second read failed", ErrPositionsUnread, false)
	r.cex.failReads(nil)
	reconcile(t, r, "the second read differs too", ErrPositionsDiffer, true)

	// The alarm is one Error event.
	if kinds := eventKinds(r.sink); len(kinds) != 1 || kinds[0] != "position_mismatch" {
		t.Errorf("events = %v, want one position_mismatch", kinds)
	}
	event := rowsOf[store.EventRow](r.sink)[0]
	if event.Level != "error" || event.Module != "inventory" {
		t.Errorf("mismatch event = %+v, want level error of module inventory", event)
	}

	// The mismatch is never cleared, also when the positions agree again.
	r.cex.setPositions("10", "0")
	reconcile(t, r, "agrees after the mismatch", nil, true)
	reconcile(t, r, "and again", nil, true)
	if kinds := eventKinds(r.sink); len(kinds) != 1 {
		t.Errorf("events = %v, want the one mismatch event only", kinds)
	}
}

// A final order explains nothing: the same difference that a pending order
// covered is a mismatch once that order has its final state.
func TestReconcileAfterFinalState(t *testing.T) {
	r := newRig(t, "1000", "10")
	c := r.openCycle(t, strategy.Short)
	lot := r.openLot(t)
	bull := fill(ord(c.ID, 1, cex.Bull, cex.PurposeEntry, cex.SideSell, lot.ID, "10", "10"), "10", "100", "0.01")
	bear := ord(c.ID, 2, cex.Bear, cex.PurposeEntry, cex.SideSell, lot.ID, "20", "5")
	r.m.Sync([]cex.OrderRecord{bull, bear})
	r.cex.setPositions("-10", "-2")
	reconcile(t, r, "2 sold of the pending sell of 5", nil, false)

	// The order ends with nothing filled, the venue still shows -2.
	r.m.Sync([]cex.OrderRecord{bull, fill(bear, "0", "0", "0")})
	reconcile(t, r, "first read after the final state", ErrPositionsDiffer, false)
	r.clock.Add(time.Second)
	reconcile(t, r, "second read", ErrPositionsDiffer, true)

	// The samples show both sides, signed.
	r.m.sample(r.clock.Now())
	s := lastOf(t, r.sink, func(store.InventorySampleRow) bool { return true })
	if !s.PosBull.Valid || !s.PosBull.Decimal.Equal(dec("-10")) || !s.PosBear.Decimal.Equal(dec("-2")) || !s.LedgerBull.Equal(dec("-10")) || !s.LedgerBear.IsZero() || !s.Mismatch {
		t.Errorf("sample = %+v, want venue -10 / -2, ledger -10 / 0, mismatch", s)
	}
}

// The sample row has no venue position until one was read: NULL, not zero.
func TestSampleBeforePositionsAreRead(t *testing.T) {
	r := newRig(t, "1000", "10")
	r.m.sample(r.clock.Now())
	s := lastOf(t, r.sink, func(store.InventorySampleRow) bool { return true })
	if s.PosBull.Valid || s.PosBear.Valid || s.TsMs != t0.UnixMilli() || !s.QuotaTotal.Equal(dec("10000")) || s.Mismatch {
		t.Errorf("sample before any read = %+v, want no venue positions and quota 10000", s)
	}
	r.cex.setPositions("0", "0")
	reconcile(t, r, "flat", nil, false)
	r.m.sample(r.clock.Now())
	if s = lastOf(t, r.sink, func(store.InventorySampleRow) bool { return true }); !s.PosBull.Valid || !s.PosBull.Decimal.IsZero() {
		t.Errorf("sample after a read = %+v, want venue positions of zero", s)
	}
}
