package exec

import (
	"testing"
	"time"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/inventory"
	"etf-pair-arbitrage/core/strategy"
)

// Restarts with the thin leg first (SPEC 12.9 "重启", PRD 5.19, 9.13), on
// the development database: the batch in pairing, or the line exit batch,
// is taken up from the order records. The window runs from the thin order's
// result as cex recorded it (final_ms); cex runs on the wall clock in these
// tests, so the tests set final_ms to the time the first process saw the
// result on its own clock.

// setFinal sets an order's final_ms in the database.
func setFinal(t *testing.T, id cex.ClientOrderID, at time.Time) {
	t.Helper()
	execSQL(t, "UPDATE trade.cex_order SET final_ms = $1 WHERE client_order_id = $2", at.UnixMilli(), string(id))
}

// thinProcess is a first process with SOXS first: example 1's thin order
// filled at t0, SOXL out of reach (bids 40.39 / 40.38, pair_stop_d 20).
func thinProcess(t *testing.T, t0 time.Time) (*process, cex.ClientOrderID) {
	t.Helper()
	a := dbProcessWith(t, utc8Time(11, 25, 5, 1, 0), nil, rigOptions{thin: "bear", stopD: 20})
	a.warmUp()
	a.thinFilled(t0, withBull(lv("40.39", "300"), lv("40.38", "500")))
	if a.m.idleReason != strategy.ReasonPairBound {
		t.Fatalf("idle %q, want the batch waiting for SOXL", a.m.idleReason)
	}
	id := a.orders()[0].Request.ClientOrderID
	a.down()
	setFinal(t, id, t0)
	return a, id
}

// SPEC 12.9: a restart inside the pair window goes on pairing from the
// order records (S = 70.20 @ 59.43061, nothing paired): SOXL back at 40.45
// pairs all of it; after the window the first round unwinds S_open.
func TestRestartInPairing(t *testing.T) {
	t0 := utc8Time(11, 25, 5, 3, 5)
	t.Run("inside the window", func(t *testing.T) {
		restartTest(t)
		a, _ := thinProcess(t, t0)
		t1 := t0.Add(10 * time.Second)
		b := dbProcessWith(t, t1, a.exchange, rigOptions{thin: "bear", stopD: 20})
		if _, err := b.m.recover(b.m.ctx); err != nil {
			t.Fatal(err)
		}
		if p := b.m.activePair(); p == nil || !p.started || !p.batch.FirstFill.Equal(t0) || !p.batch.ThinQty.Equal(dec("70.2")) {
			t.Fatalf("pairing taken up %+v, want the batch from %s with S = 70.20", b.m.batch, showTime(t0))
		}
		if b.m.lastEntry == nil || b.m.lastEntry.Direction != strategy.Short {
			t.Errorf("last entry %+v, want the batch in pairing", b.m.lastEntry)
		}
		b.at(t1)
		b.books(entryBook())
		b.trade(t1)
		o := b.orders()
		if len(o) != 1 {
			t.Fatalf("%d orders from the new process, want the thick order", len(o))
		}
		wantOrder(t, "thick", o[0], cex.Bull, cex.PurposeComplete, cex.SideSell, "105.3", "40.45")
		if lot := b.lotNow(1); lot.End != inventory.LotEndBuilt {
			t.Errorf("lot %s, want built", lot.End)
		}
	})
	t.Run("after the window", func(t *testing.T) {
		restartTest(t)
		a, _ := thinProcess(t, t0)
		t1 := t0.Add(40 * time.Second)
		b := dbProcessWith(t, t1, a.exchange, rigOptions{thin: "bear", stopD: 20})
		if _, err := b.m.recover(b.m.ctx); err != nil {
			t.Fatal(err)
		}
		b.at(t1)
		b.books(withBull(lv("40.39", "300"), lv("40.38", "500")))
		b.stepAt(t1)
		o := b.orders()
		if len(o) != 1 {
			t.Fatalf("%d orders, want the unwind at once; idle %q", len(o), b.m.idleReason)
		}
		wantOrder(t, "unwind", o[0], cex.Bear, cex.PurposeTrim, cex.SideBuy, "70.2", "59.505")
		if rows := b.pairRows("pair"); len(rows) == 0 || rows[0].Reason != "window" {
			t.Errorf("first pair decision of the new process %+v, want the window's risk exit", rows)
		}
	})
}

// exitProcess is a first process with SOXS first and pair_stop_d 50 that
// built example 1's short, sent its line exit at t1 (exitBook: SOXS 70.20
// bought back @ 59.385) and, SOXL's asks having moved to 40.50 (above its
// break-even bound 40.47), waits for SOXL.
func exitProcess(t *testing.T, t1 time.Time) *process {
	t.Helper()
	opts := rigOptions{thin: "bear", stopD: 50, latency: 100 * time.Millisecond}
	a := dbProcessWith(t, utc8Time(11, 25, 5, 1, 0), nil, opts)
	a.warmUp()
	a.thinFilled(utc8Time(11, 25, 5, 3, 5), entryBook())
	a.results()
	a.step()
	a.at(t1)
	a.books(exitBook())
	a.stepAt(t1)
	moved := exitBook()
	moved.bullAsks = side{lv("40.50", "300"), lv("40.51", "400")}
	a.depth(cex.Bull, moved.bullBids, moved.bullAsks)
	a.results()
	a.step()
	if a.m.idleReason != strategy.ReasonExitBound {
		t.Fatalf("idle %q, want SOXL waiting for its bound", a.m.idleReason)
	}
	ex := a.exitOrdersSince(0)
	a.down()
	setFinal(t, ex[0].Request.ClientOrderID, t1)
	return a
}

// PRD 9.13: a restart in a line exit batch takes it up from the orders
// (SOXS closed, SOXL not): inside exit_pair_window SOXL closes within its
// bound once the book allows; after it, it closes at once at PRD 10.4's
// price (40.50 through 10 bps: 40.55).
func TestRestartInExitBatch(t *testing.T) {
	t1 := utc8Time(11, 25, 5, 3, 10)
	opts := rigOptions{thin: "bear", stopD: 50}
	t.Run("inside the window", func(t *testing.T) {
		restartTest(t)
		a := exitProcess(t, t1)
		t2 := t1.Add(10 * time.Second)
		b := dbProcessWith(t, t2, a.exchange, opts)
		if _, err := b.m.recover(b.m.ctx); err != nil {
			t.Fatal(err)
		}
		if e := b.m.activeExit(); e == nil || !e.started || !e.batch.FirstFill.Equal(t1) {
			t.Fatalf("exit batch taken up %+v, want the one of %s", b.m.batch, showTime(t1))
		}
		b.at(t2)
		b.books(exitBook())
		b.trade(t2)
		o := b.orders()
		if len(o) != 1 {
			t.Fatalf("%d orders from the new process, want SOXL's close", len(o))
		}
		wantOrder(t, "thick", o[0], cex.Bull, cex.PurposeExit, cex.SideBuy, "105.3", "40.37")
		b.stepAt(t2.Add(time.Second))
		if c := b.lastCycle(); b.inv.Cycle() != nil || c.EndReason != inventory.EndExitLine {
			t.Errorf("cycle open %t, end %q; want closed exit_line", b.inv.Cycle() != nil, c.EndReason)
		}
	})
	t.Run("after the window", func(t *testing.T) {
		restartTest(t)
		a := exitProcess(t, t1)
		t2 := t1.Add(40 * time.Second)
		b := dbProcessWith(t, t2, a.exchange, opts)
		if _, err := b.m.recover(b.m.ctx); err != nil {
			t.Fatal(err)
		}
		moved := exitBook()
		moved.bullAsks = side{lv("40.50", "300"), lv("40.51", "400")}
		b.at(t2)
		b.books(moved)
		b.stepAt(t2)
		o := b.orders()
		if len(o) != 1 {
			t.Fatalf("%d orders, want the hard close at once; idle %q", len(o), b.m.idleReason)
		}
		wantOrder(t, "hard close", o[0], cex.Bull, cex.PurposeExit, cex.SideBuy, "105.3", "40.55")
		if rows := b.pairRows("exit_pair"); len(rows) == 0 || rows[0].Reason != "exit_window" {
			t.Errorf("first exit_pair decision %+v, want exit_window", rows)
		}
	})
}

// SPEC 12.6: the half target is read back from trade.exit_stage. After H
// was closed at exit1, a restart at exit1 closes nothing more.
func TestRestartReadsTheHalfTarget(t *testing.T) {
	restartTest(t)
	exit2 := -1.0
	opts := rigOptions{thin: "bear", exit2: &exit2}
	a := dbProcessWith(t, utc8Time(11, 25, 5, 1, 0), nil, opts)
	a.warmUp()
	a.thinFilled(utc8Time(11, 25, 5, 3, 5), entryBook())
	a.results()
	a.step()
	t1 := utc8Time(11, 25, 5, 3, 10)
	a.at(t1)
	a.books(exit1Book())
	a.trade(t1)
	a.results()
	a.step()
	a.down()

	t2 := t1.Add(5 * time.Second)
	b := dbProcessWith(t, t2, a.exchange, opts)
	if _, err := b.m.recover(b.m.ctx); err != nil {
		t.Fatal(err)
	}
	h := b.m.stageOf(b.inv.Cycle())
	if !h.Set || !h.Target[cex.Bear].Equal(dec("35.1")) || !h.Closed[cex.Bear].Equal(dec("35.1")) || h.BuiltLots != 1 {
		t.Fatalf("half target read back %+v, want H 35.10 closed", h)
	}
	b.at(t2)
	b.books(exit1Book())
	b.stepAt(t2)
	if len(b.orders()) != 0 {
		t.Errorf("%d orders at exit1 after the restart, want none: H was done", len(b.orders()))
	}
}
