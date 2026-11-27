package exec

import (
	"context"
	"syscall"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/strategy"
)

// The tests of this file are the reproductions of the external audit of
// step 6 (experiments/core_step6_audit_20261125), turned into acceptance
// tests: a read of the venue's positions is a network call of up to two
// seconds, and nothing the round read before it may be decided on after it.

// A3, the audit's case: the routine comparison is due at 05:03:02 with the
// entry book (a short signal) in place. The read takes 120 ms; meanwhile the
// book withdraws the signal and the entry cutoff (set 80 ms after the round
// began) passes. The round ends with the read and sends nothing; the next
// round sees the time and the book after it and sends nothing either.
func TestNoEntryOnFactsOlderThanAPositionRead(t *testing.T) {
	r := newRig(t, utc8Time(11, 25, 5, 1, 0), rigOptions{})
	r.warmUp()
	tick := utc8Time(11, 25, 5, 3, 2)
	r.at(tick)
	r.books(entryBook())
	r.m.session.End = tick.Add(80*time.Millisecond + 2*time.Hour) // entries stop 80 ms after tick
	r.m.nextReconcile = tick
	r.gate.duringRead(func() { r.at(tick.Add(120 * time.Millisecond)); r.books(calibration(1)) })
	r.stepOnce()
	r.results()
	if n := len(r.orders()); n != 0 {
		t.Fatalf("%d orders sent on the facts read before the positions, want none; idle reason %q", n, r.m.idleReason)
	}
	if r.m.idleReason != strategy.ReasonPositionsRead {
		t.Errorf("idle reason %q, want positions_read", r.m.idleReason)
	}
	r.idleAt(r.clock(), strategy.ReasonCloseNear)
}

// The same across the real cutoff, two hours before the close of 11-26
// 05:00 (owner item 108): the comparison is due at 02:59:59.950 with the
// short signal in place; the read returns at 03:00:00.070 with the signal
// withdrawn. No entry.
func TestNoEntryAcrossTheEntryCutoffDuringARead(t *testing.T) {
	r := newRig(t, utc8Time(11, 26, 2, 57, 0), rigOptions{})
	r.warmUp()
	tick := utc8Time(11, 26, 2, 59, 59).Add(950 * time.Millisecond)
	if cut := r.m.session.End.Add(-r.m.scfg.EntryStopBeforeClose); !cut.Equal(utc8Time(11, 26, 3, 0, 0)) {
		t.Fatalf("entries stop at %s, want 03:00", showTime(cut))
	}
	r.at(tick)
	r.books(entryBook())
	r.m.nextReconcile = tick
	r.gate.duringRead(func() { r.at(tick.Add(120 * time.Millisecond)); r.books(calibration(1)) })
	r.stepOnce()
	r.results()
	r.stepOnce()
	if n := len(r.orders()); n != 0 {
		t.Fatalf("%d entry orders across 03:00, want none; idle reason %q", n, r.m.idleReason)
	}
	if r.m.idleReason != strategy.ReasonCloseNear {
		t.Errorf("idle reason %q, want close_near", r.m.idleReason)
	}
}

// A completion and the forced exit go in the round that finds them due,
// before the routine comparison: no read of the positions delays them.
func TestReducingOrdersGoBeforeTheRoutineComparison(t *testing.T) {
	t.Run("completion", func(t *testing.T) {
		r := gapRig(t, rigOptions{})
		t0 := utc8Time(11, 25, 5, 3, 5)
		r.enterMatchedOn(t0, entryBook(), thinBear())
		r.at(t0.Add(time.Second))
		r.books(gapBook(side{lv("40.47", "300"), lv("40.48", "300")}, appendix3Bids("59.418")))
		r.m.nextReconcile = t0 // overdue
		reads := r.gate.reads.Load()
		r.stepAt(t0.Add(time.Second))
		if d := r.decisions()[len(r.decisions())-1]; d.Kind != "complete" {
			t.Fatalf("decision %s, want the completion", d.Kind)
		}
		if n := r.gate.reads.Load() - reads; n != 0 {
			t.Errorf("%d reads of the positions before the completion, want none", n)
		}
	})
	t.Run("forced exit", func(t *testing.T) {
		r := newRig(t, utc8Time(11, 25, 5, 1, 0), rigOptions{})
		r.warmUp()
		t0 := utc8Time(11, 25, 5, 3, 5)
		r.at(t0)
		r.books(entryBook())
		r.trade(t0)
		r.step() // the comparison after the batch
		t1 := utc8Time(11, 26, 4, 59, 0)
		r.at(t1)
		r.books(exitBook())
		if t1.Before(r.m.nextReconcile) {
			t.Fatalf("the routine comparison is not due at %s", showTime(t1))
		}
		reads := r.gate.reads.Load()
		r.stepAt(t1)
		if d := r.decisions()[len(r.decisions())-1]; d.Kind != "forced" {
			t.Fatalf("decision %s, want the forced exit", d.Kind)
		}
		if n := r.gate.reads.Load() - reads; n != 0 {
			t.Errorf("%d reads of the positions before the forced exit, want none", n)
		}
	})
}

// A round reads the positions at most once and ends with the read. The exit
// pair fills both legs: the round of its results compares the positions and
// ends there; the next round closes the cycle on its own read.
func TestARoundEndsWithItsRead(t *testing.T) {
	r := newRig(t, utc8Time(11, 25, 5, 1, 0), rigOptions{})
	r.warmUp()
	t0 := utc8Time(11, 25, 5, 3, 5)
	r.at(t0)
	r.books(entryBook())
	r.trade(t0)
	r.step()
	t1 := t0.Add(5 * time.Second)
	r.at(t1)
	r.books(exitBook())
	r.stepAt(t1)
	r.results()
	reads := r.gate.reads.Load()
	r.stepOnce() // the round of the results
	if n := r.gate.reads.Load() - reads; n != 1 || r.inv.Cycle() == nil || r.m.idleReason != strategy.ReasonPositionsRead {
		t.Fatalf("round of the results: %d reads, cycle open %t, idle reason %q; want 1, open, positions_read", n, r.inv.Cycle() != nil, r.m.idleReason)
	}
	r.stepOnce()
	if n := r.gate.reads.Load() - reads; n != 2 || r.inv.Cycle() != nil {
		t.Fatalf("next round: %d reads in all, cycle open %t; want 2, closed", n, r.inv.Cycle() != nil)
	}
}

// A3/A4: the routine comparison cannot read the positions. No entry while
// the last read failed (not_ready), on the same signal; the read is tried
// again 5 s later, and once it agrees the entry goes.
func TestNoEntryWhileThePositionsCannotBeRead(t *testing.T) {
	r := newRig(t, utc8Time(11, 25, 5, 1, 0), rigOptions{})
	r.warmUp()
	at := utc8Time(11, 25, 5, 3, 5)
	r.at(at)
	r.books(entryBook())
	r.gate.unread.Store(true)
	r.m.nextReconcile = at
	r.stepOnce()
	r.results()
	if len(r.orders()) != 0 {
		t.Fatalf("entry sent in the round whose read failed; idle reason %q", r.m.idleReason)
	}
	t1 := at.Add(300 * time.Millisecond)
	r.at(t1)
	r.books(entryBook())
	r.idleAt(t1, strategy.ReasonNotReady)
	r.gate.unread.Store(false)
	t2 := at.Add(5 * time.Second) // a failed read is tried again recoverRetry after it
	r.at(t2)
	r.books(entryBook())
	r.stepOnce() // read again, and it agrees
	if r.m.idleReason != strategy.ReasonPositionsRead || len(r.orders()) != 0 {
		t.Fatalf("idle reason %q with %d orders, want positions_read and none", r.m.idleReason, len(r.orders()))
	}
	r.trade(t2)
}

// A5: the verdict "never arrived" needs a read of the positions that
// succeeds after cex's NotFound. The positions were read when SOXL was 0;
// SOXL then sold 12 at the venue, the reads fail, and cex reports the order
// NotFound: with only the older read to go on, the order stays unknown and
// is not sent again. When a read succeeds it shows -12, which is not the
// ledger's 0: still no verdict.
func TestNeverArrivedNeedsAReadAfterNotFound(t *testing.T) {
	r := newRig(t, utc8Time(11, 25, 5, 1, 0), rigOptions{})
	r.warmUp()
	at := utc8Time(11, 25, 5, 3, 5)
	id := r.entryWithUnknownBull(at)
	if _, err := r.cex.ReadPositions(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.paper.SeedPositions([2]decimal.Decimal{dec("-12"), dec("-70.20")})
	r.gate.unread.Store(true)
	r.src.setNotFound(id)
	r.stepAt(at.Add(31 * time.Second))
	if r.eventCount("order_never_arrived") != 0 || len(r.cex.UnresolvedOrders()) != 1 {
		t.Fatalf("taken as never arrived on a read older than NotFound; events %v", r.eventKinds())
	}
	r.gate.unread.Store(false)
	r.stepAt(at.Add(33 * time.Second))
	if r.eventCount("order_never_arrived") != 0 || len(r.cex.UnresolvedOrders()) != 1 {
		t.Fatalf("taken as never arrived with SOXL -12 at the venue; events %v", r.eventKinds())
	}
	if n := r.gate.placeCalls(id); n != 1 {
		t.Errorf("SOXL's order handed to the venue %d times, want 1", n)
	}
}

// A2: a stop leaves normally only on a read of the positions that agrees
// with the ledger, the recorded dust included. With 12 SOXL at the venue
// that the ledger does not hold, the recovery after a stop signal does not
// report the process flat: the difference is confirmed a second later, and
// the loop goes on with the mismatch (only a takeover leaves). Without a
// difference the stop leaves after one read.
func TestStopLeavesOnlyOnAgreeingPositions(t *testing.T) {
	t.Run("venue holds more", func(t *testing.T) {
		r := newRig(t, utc8Time(11, 25, 5, 1, 0), rigOptions{})
		r.paper.SeedPositions([2]decimal.Decimal{dec("12"), decimal.Zero})
		r.m.onSignal(syscall.SIGTERM, r.clock())
		leave, err := r.m.recover(context.Background())
		if leave || err != nil {
			t.Fatalf("recovery left = %t, err %v; want it to go on into the loop", leave, err)
		}
		if !r.inv.Mismatch() {
			t.Error("the same 12 SOXL a second apart did not make a mismatch")
		}
		for s := 1; s <= 3; s++ {
			if r.stepAt(r.clock().Add(time.Second)) {
				t.Fatalf("left %d s after the stop with 12 SOXL at the venue", s)
			}
		}
	})
	t.Run("flat", func(t *testing.T) {
		r := newRig(t, utc8Time(11, 25, 5, 1, 0), rigOptions{})
		if _, err := r.m.recover(context.Background()); err != nil {
			t.Fatal(err)
		}
		t1 := r.clock().Add(5 * time.Second)
		r.m.onSignal(syscall.SIGTERM, t1)
		reads := r.gate.reads.Load()
		r.at(t1)
		if r.stepOnce() {
			t.Fatal("left without a read after the stop")
		}
		if !r.stepOnce() {
			t.Fatalf("did not leave after the read agreed; idle reason %q", r.m.idleReason)
		}
		if n := r.gate.reads.Load() - reads; n != 1 {
			t.Errorf("%d reads, want 1", n)
		}
	})
}

// A2: in the recovery a difference is read again a second later, as in the
// loop, not after the 5 s of a failed read: with the production retry the
// same 12 SOXL are confirmed as a mismatch and recovery goes on into the
// loop, which then sends nothing.
func TestRecoveryConfirmsAPersistentDifference(t *testing.T) {
	r := newRig(t, utc8Time(11, 25, 5, 1, 0), rigOptions{})
	r.paper.SeedPositions([2]decimal.Decimal{dec("12"), decimal.Zero})
	done := make(chan error, 1)
	go func() { _, err := r.m.recover(context.Background()); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("recovery did not confirm the difference within 3 s")
	}
	if !r.inv.Mismatch() {
		t.Fatal("no mismatch after recovery")
	}
	r.idleOnMismatch()
}

// idleOnMismatch runs a round on the entry signal that must send nothing
// for the position mismatch.
func (r *rig) idleOnMismatch() {
	r.t.Helper()
	at := r.clock().Add(time.Second)
	r.at(at)
	r.books(entryBook())
	r.idleAt(at, strategy.ReasonPositionMismatch)
}

// A reduction sized by the venue's positions (an outcome unknown) reads the
// quantity first; the round of that read sends nothing, and the next round
// prices the order on the book it reads. SOXS's best ask moves from 59.445
// to 59.50 while the read is on its way: SOXS's 70.20 are bought back
// through 59.50 (x 1.001 = 59.5595, rounded up 59.560), not through the
// book older than the read.
func TestReductionByPositionsPricedAfterTheRead(t *testing.T) {
	moved := entryBook()
	moved.bearAsks = side{lv("59.50", "100"), lv("59.51", "50"), lv("59.52", "50"), lv("59.53", "50"), lv("59.54", "50")}
	check := func(t *testing.T, r *rig, at time.Time, kind string) {
		t.Helper()
		before := len(r.orders())
		r.at(at)
		r.books(entryBook()) // fresh: SOXS's best ask 59.445
		r.gate.duringRead(func() { r.books(moved) })
		r.stepOnce()
		if n := len(r.orders()) - before; n != 0 || r.m.idleReason != strategy.ReasonOrderUnknown {
			t.Fatalf("round of the read: %d orders, idle reason %q; want none, order_unknown", n, r.m.idleReason)
		}
		r.stepOnce()
		d := r.decisions()[len(r.decisions())-1]
		o := r.lastOrder()
		if d.Kind != kind || o.Request.Leg != cex.Bear || !o.Request.Price.Equal(dec("59.56")) || !o.Request.Qty.Equal(dec("70.2")) {
			t.Fatalf("decision %s, last order %s %s x %s; want %s of SOXS 70.20 at 59.560", d.Kind, o.Request.Leg, o.Request.Price, o.Request.Qty, kind)
		}
	}
	t.Run("forced exit", func(t *testing.T) {
		r := newRig(t, utc8Time(11, 25, 5, 1, 0), rigOptions{})
		r.warmUp()
		t0 := utc8Time(11, 25, 5, 3, 5)
		r.entryWithUnknownBull(t0)
		r.m.onSignal(syscall.SIGTERM, t0)
		check(t, r, t0.Add(time.Second), "forced")
	})
	t.Run("trim at 30 s", func(t *testing.T) {
		r := newRig(t, utc8Time(11, 25, 5, 1, 0), rigOptions{})
		r.warmUp()
		t0 := utc8Time(11, 25, 5, 3, 5)
		r.entryWithUnknownBull(t0)
		r.idleAt(t0.Add(time.Second), strategy.ReasonOrderUnknown)
		check(t, r, t0.Add(31*time.Second), "trim")
	})
}

// No entry while the last comparison differed from the ledger, before the
// difference is confirmed or gone (review of the audit fixes): the venue
// shows 12 SOXL the ledger does not hold; once they are gone the read a
// second later agrees and the entry goes.
func TestNoEntryWhileThePositionsDiffer(t *testing.T) {
	r := newRig(t, utc8Time(11, 25, 5, 1, 0), rigOptions{})
	r.warmUp()
	at := utc8Time(11, 25, 5, 3, 5)
	r.paper.SeedPositions([2]decimal.Decimal{dec("12"), decimal.Zero})
	r.at(at)
	r.books(entryBook())
	r.m.nextReconcile = at
	r.step()
	if len(r.orders()) != 0 || r.m.idleReason != strategy.ReasonNotReady {
		t.Fatalf("%d orders, idle reason %q after a differing read; want none, not_ready", len(r.orders()), r.m.idleReason)
	}
	r.paper.SeedPositions([2]decimal.Decimal{decimal.Zero, decimal.Zero})
	t1 := at.Add(1500 * time.Millisecond)
	r.at(t1)
	r.books(entryBook())
	r.trade(t1)
}

// Reads that keep failing by timing out (2 s each) leave the loop most of
// its time to decide: a failed read is tried again recoverRetry after it
// returned, not every second from the round's start.
func TestFailingSlowReadsLeaveTimeToDecide(t *testing.T) {
	r := newRig(t, utc8Time(11, 25, 5, 1, 0), rigOptions{})
	r.warmUp()
	start := utc8Time(11, 25, 5, 3, 5)
	r.at(start)
	r.books(entryBook())
	r.gate.unread.Store(true)
	r.m.nextReconcile = start
	var slow func()
	slow = func() { r.at(r.clock().Add(2 * time.Second)); r.gate.duringRead(slow) }
	r.gate.duringRead(slow)
	reads := r.gate.reads.Load()
	for r.clock().Before(start.Add(time.Minute)) {
		r.stepOnce()
		r.at(r.clock().Add(10 * time.Millisecond)) // the loop wakes again at once
	}
	if n := r.gate.reads.Load() - reads; n*2 > 20 {
		t.Errorf("%d failing 2 s reads in 60 s: %d s inside reads, want at most a third of the time", n, n*2)
	}
}

// A flat cycle waiting for its close while the reads time out (2 s each)
// tries the close again as the comparison's backoff says, not every second
// (review): the reads take at most a third of the time.
func TestCloseRetryOnFailingSlowReads(t *testing.T) {
	r := newRig(t, utc8Time(11, 25, 5, 1, 0), rigOptions{})
	r.warmUp()
	t0 := utc8Time(11, 25, 5, 3, 5)
	r.at(t0)
	r.books(entryBook())
	r.trade(t0)
	t1 := t0.Add(15 * time.Second)
	r.at(t1)
	r.books(exitBook())
	r.stepAt(t1)
	r.results()
	r.gate.unread.Store(true)
	var slow func()
	slow = func() { r.at(r.clock().Add(2 * time.Second)); r.gate.duringRead(slow) }
	r.gate.duringRead(slow)
	reads, start := r.gate.reads.Load(), r.clock()
	for r.clock().Before(start.Add(time.Minute)) {
		r.stepOnce()
		r.at(r.clock().Add(10 * time.Millisecond))
	}
	if r.inv.Cycle() == nil {
		t.Fatal("the cycle closed without a read")
	}
	if n := r.gate.reads.Load() - reads; n*2 > 20 {
		t.Errorf("%d failing 2 s reads in 60 s: %d s inside reads, want at most a third of the time", n, n*2)
	}
}
