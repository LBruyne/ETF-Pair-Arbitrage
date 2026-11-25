package inventory

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/strategy"
)

// holdingRig opens a long cycle with n built lots, each of two filled entry
// orders, and returns the rig and the snapshot of its 2n orders as cex
// gives it: every record with its UpdateSeq.
func holdingRig(t testing.TB, n int) (*testRig, []cex.OrderRecord) {
	r := newRig(t, "20000", "10")
	c := r.openCycle(t, strategy.Long)
	var orders []cex.OrderRecord
	seq := uint64(0)
	for i := 0; i < n; i++ {
		l := r.openLot(t)
		for j := 0; j < 2; j++ {
			seq++
			orders = append(orders, at(fill(ord(c.ID, 2*i+j+1, cex.Leg(j), cex.PurposeEntry, cex.SideBuy, l.ID, "40", "10"), "10", "400", "0.026"), seq))
		}
		r.m.Sync(orders)
		r.m.SettleLot(l.ID, true, r.clock.Now())
	}
	return r, orders
}

// A wake-up of the loop with no order changed books the same snapshot
// again: it is taken as booked already, without recomputing the lots
// (audit of step 6: 8 / 51 / 272 µs per Sync for 2 / 20 / 100 orders).
func TestUnchangedSnapshotIsNotRecomputed(t *testing.T) {
	r, orders := holdingRig(t, 10)
	if allocs := testing.AllocsPerRun(100, func() { r.m.Sync(orders) }); allocs > 2 {
		t.Errorf("%v allocations per Sync of an unchanged snapshot of %d orders, want at most 2", allocs, len(orders))
	}
}

// The ledger and every row written are the same whether the unchanged
// snapshots are taken as booked or recomputed in full: run B gets every
// snapshot with a new UpdateSeq on each record, so that nothing is ever
// taken as booked. Between the reports the same snapshot comes again and
// again, also across a funding settlement, which a position held over it
// makes awaited (FundingKnown false) without any order changing.
func TestSnapshotReuseChangesNothing(t *testing.T) {
	type step struct {
		orders  []cex.OrderRecord
		advance time.Duration
	}
	a, b := newRig(t, "20000", "10"), newRig(t, "20000", "10")
	a.clock.Set(t0.Add(6*time.Hour + 59*time.Minute)) // 07:59 UTC, a minute before a settlement
	b.clock.Set(a.clock.Now())
	ca, cb := a.openCycle(t, strategy.Long), b.openCycle(t, strategy.Long)
	la, lb := a.openLot(t), b.openLot(t)
	if ca.ID != cb.ID || la.ID != lb.ID {
		t.Fatal("the two runs differ in their ids")
	}
	o := func(seq int, leg cex.Leg, purpose cex.OrderPurpose, side cex.Side, lot LotID) cex.OrderRecord {
		return ord(ca.ID, seq, leg, purpose, side, lot, "40", "10")
	}
	e1, e2 := o(1, cex.Bull, cex.PurposeEntry, cex.SideBuy, la.ID), o(2, cex.Bear, cex.PurposeEntry, cex.SideBuy, la.ID)
	x1 := o(3, cex.Bull, cex.PurposeExit, cex.SideSell, "")
	steps := []step{
		{orders: []cex.OrderRecord{at(e1, 1), at(e2, 2)}},
		{orders: []cex.OrderRecord{at(working(e1, "4", "160", "0.01"), 3), at(e2, 2)}},
		{orders: []cex.OrderRecord{at(working(e1, "4", "160", "0.01"), 3), at(e2, 2)}},
		{orders: []cex.OrderRecord{at(timed(fill(e1, "10", "400", "0.026"), a.clock.Now()), 4), at(timed(fill(e2, "10", "400", "0.026"), a.clock.Now()), 5)}},
		{orders: []cex.OrderRecord{at(timed(fill(e1, "10", "400", "0.026"), a.clock.Now()), 4), at(timed(fill(e2, "10", "400", "0.026"), a.clock.Now()), 5)}, advance: 2 * time.Minute}, // across 08:00
		{orders: []cex.OrderRecord{at(timed(fill(e1, "10", "400", "0.026"), a.clock.Now()), 4), at(timed(fill(e2, "10", "400", "0.026"), a.clock.Now()), 5)}},
		{orders: []cex.OrderRecord{at(timed(fill(e1, "10", "400", "0.026"), a.clock.Now()), 4), at(timed(fill(e2, "10", "400", "0.026"), a.clock.Now()), 5), at(working(x1, "6", "246", "0.016"), 6)}},
		{orders: []cex.OrderRecord{at(timed(fill(e1, "10", "400", "0.026"), a.clock.Now()), 4), at(timed(fill(e2, "10", "400", "0.026"), a.clock.Now()), 5), at(fill(x1, "10", "410", "0.027"), 7)}},
		{orders: []cex.OrderRecord{at(timed(fill(e1, "10", "400", "0.026"), a.clock.Now()), 4), at(timed(fill(e2, "10", "400", "0.026"), a.clock.Now()), 5), at(fill(x1, "10", "410", "0.027"), 7)}},
	}
	bump := uint64(100)
	for i, s := range steps {
		a.clock.Add(s.advance)
		b.clock.Add(s.advance)
		a.m.Sync(s.orders)
		fresh := make([]cex.OrderRecord, len(s.orders))
		for j, rec := range s.orders {
			bump++
			fresh[j] = at(rec, bump)
		}
		b.m.Sync(fresh)
		if i == 3 {
			a.m.SettleLot(la.ID, true, a.clock.Now())
			b.m.SettleLot(lb.ID, true, b.clock.Now())
		}
		if ga, gb := a.m.Cycle(), b.m.Cycle(); !reflect.DeepEqual(ga, gb) {
			t.Fatalf("step %d: ledgers differ:\n%+v\n%+v", i, ga, gb)
		}
	}
	if c := a.m.Cycle(); c.FundingKnown {
		t.Error("the settlement passed with a position held: the funding should be awaited")
	}
	for _, table := range []string{"cycle", "lot"} {
		ra, rb := a.sink.Rows(table), b.sink.Rows(table)
		if fmt.Sprint(ra) != fmt.Sprint(rb) {
			t.Errorf("%s rows differ:\n%v\n%v", table, ra, rb)
		}
	}
}

// BenchmarkSyncUnchanged books an unchanged snapshot of 2, 20 and 100
// orders, as a wake-up of the loop does while a cycle is held.
func BenchmarkSyncUnchanged(b *testing.B) {
	for _, lots := range []int{1, 10, 50} {
		b.Run(fmt.Sprintf("orders=%d", 2*lots), func(b *testing.B) {
			r, orders := holdingRig(b, lots)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				r.m.Sync(orders)
			}
		})
	}
}
