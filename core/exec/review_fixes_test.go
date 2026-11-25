package exec

import (
	"context"
	"encoding/json"
	"errors"
	"syscall"
	"testing"
	"time"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/inventory"
	"etf-pair-arbitrage/core/store"
	"etf-pair-arbitrage/core/strategy"
)

// The tests of this file hold the fixes of the review of steps 6.2 to 6.4.

// failingPerp is a recorder whose rows cannot be read.
type failingPerp struct{}

func (failingPerp) LoadPerpMinutes(context.Context, string, string, time.Time, time.Time) ([]store.PerpMinuteRow, error) {
	return nil, errors.New("permission denied for table perp_second")
}

// The minutes only warm the mean up: a restart in the close window whose
// recorder rows or minute closes of F cannot be read still recovers, with
// the minutes it did not record invalid, and sends the forced exit.
func TestRecoveryGoesOnWithoutTheMinutes(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(r *rig)
	}{
		{"recorder unreadable", func(r *rig) { r.m.perp = failingPerp{} }},
		{"F unreadable", func(r *rig) {
			r.m.perp = &perpRows{}
			r.src.mu.Lock()
			r.src.fxErr = errors.New("coinbase 503")
			r.src.mu.Unlock()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, utc8Time(11, 25, 5, 1, 0), rigOptions{})
			r.warmUp()
			t0 := utc8Time(11, 25, 5, 3, 5)
			r.at(t0)
			r.books(entryBook())
			r.trade(t0)
			sent := r.gate.placedTotal()

			// As at a restart at 04:59:10 the next morning: the minutes are to
			// be restored and their sources cannot be read.
			tc0 := utc8Time(11, 26, 4, 59, 10)
			r.at(tc0)
			r.books(exitBook())
			r.m.recovered.minutes, r.m.restoredBars = false, nil
			tc.set(r)
			ctx, cancel := context.WithCancel(context.Background())
			done := runLoop(r, ctx)
			waitFor(t, "the forced exit", func() bool { return r.gate.placedTotal() == sent+2 })
			cancel()
			<-done
			if r.m.stats.Ready {
				t.Error("the mean is ready on minutes that were not read")
			}
		})
	}
}

// V14 (I2): the trim by positions of the partly filled SOXL (50 of 105.30
// at the venue, its reports lost and held) sells back 36.86 SOXS. The
// trim's result arrives, the position push does not: the snapshot still
// shows SOXS's 70.20. The next rounds read the positions again before
// sizing anything on them, and no second trim goes.
func TestNoSecondTrimOnAStaleSnapshot(t *testing.T) {
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

	t30 := t0.Add(31 * time.Second)
	r.at(t30)
	r.books(match)
	r.tradeOn(t30)
	for _, ms := range []int{300, 600, 900} {
		r.at(t30.Add(time.Duration(ms) * time.Millisecond))
		r.books(match)
		r.step()
	}
	trims := 0
	for _, d := range r.decisions() {
		if d.Kind == "trim" {
			trims++
		}
	}
	pos, _ := r.paper.Positions(context.Background(), nil)
	if trims != 1 || !pos.BySymbol["SOXSUSDT"].Qty.Equal(dec("-33.34")) {
		t.Errorf("%d trims, SOXS at the venue %s; want one trim, -33.34", trims, pos.BySymbol["SOXSUSDT"].Qty)
	}
}

// I3: an earlier cycle left -0.05 SOXL of dust, on the side of the new
// short cycle. With SOXL's entry outcome unknown a stop forces the exit by
// the venue's positions: it buys back the cycle's 105.30, not the dust with
// it, so that once the outcome is known the venue and the ledger agree and
// the stop completes.
func TestForcedByPositionsLeavesTheDust(t *testing.T) {
	state := &store.State{Inventory: &store.InventoryStateRow{Capital: dec("2000"), SinceMs: utc8Time(11, 25, 5, 0, 0).UnixMilli(),
		Dust: json.RawMessage(`{"bull":{"qty":"-0.05","avg_price":"40"}}`)}}
	r := newRig(t, utc8Time(11, 25, 5, 1, 0), rigOptions{state: state})
	r.warmUp()
	t0 := utc8Time(11, 25, 5, 3, 5)
	r.at(t0)
	r.books(entryBook())
	r.gate.lose("SOXLUSDT")
	r.gate.hold("SOXLUSDT")
	r.stepAt(t0)
	r.waitUnknownBull()
	r.m.onSignal(syscall.SIGTERM, t0)
	r.at(t0.Add(time.Second))
	r.books(entryBook())
	r.stepAt(t0.Add(time.Second))
	if o := r.lastOrder(); o.Request.Purpose != cex.PurposeForced || o.Request.Leg != cex.Bear {
		t.Fatalf("last order %s %s", o.Request.Purpose, o.Request.Leg)
	}
	forced := r.orders()[len(r.orders())-2]
	if forced.Request.Purpose != cex.PurposeForced || forced.Request.Leg != cex.Bull || !forced.Request.Qty.Equal(dec("105.3")) {
		t.Fatalf("SOXL forced order %s %s %s, want forced 105.30", forced.Request.Purpose, forced.Request.Leg, forced.Request.Qty)
	}
	r.gate.release()
	waitFor(t, "every outcome", func() bool { return len(r.cex.UnresolvedOrders()) == 0 })
	left := false
	for s := 2; s <= 6 && !left; s++ {
		r.at(t0.Add(time.Duration(s) * time.Second))
		r.books(entryBook())
		left = r.step()
		time.Sleep(300 * time.Millisecond) // a mismatch is confirmed on wall time
	}
	if !left || r.inv.Mismatch() {
		t.Errorf("left %t, mismatch %t; want the stop completed in agreement", left, r.inv.Mismatch())
	}
}

// I4: in the round an uneven batch's results come in, the gap is decided
// without a read of the positions; they are compared once, when the gap
// sends nothing more, before the lot is settled.
func TestBatchResultsComparedAfterTheGap(t *testing.T) {
	r := gapRig(t, rigOptions{})
	t0 := utc8Time(11, 25, 5, 3, 5)
	r.m.nextReconcile = t0.Add(time.Hour) // no comparison by the interval
	reads := r.gate.reads.Load()
	r.enterMatchedOn(t0, entryBook(), thinBear())
	if n := r.gate.reads.Load() - reads; n != 0 || r.m.idleReason != strategy.ReasonAwaitDepth {
		t.Fatalf("%d reads of the positions in the results' round (idle %q), want none", n, r.m.idleReason)
	}
	r.at(t0.Add(time.Second))
	r.books(gapBook(side{lv("40.47", "300"), lv("40.48", "300")}, appendix3Bids("59.418")))
	r.trade(t0.Add(time.Second)) // the completion, its result, the settling
	if n := r.gate.reads.Load() - reads; n != 1 {
		t.Errorf("%d reads of the positions for the batch, want 1", n)
	}
	if l := r.lastLot(r.decisions()[0].LotID); l.Status != string(inventory.LotOpen) {
		t.Errorf("lot %s, want open (built)", l.Status)
	}
}

// Minor 2: an IOC the venue accepted and then cancelled for its price is
// refused for its price as much as one rejected at once.
func TestRefusedAtCancelledForItsPrice(t *testing.T) {
	o := cex.OrderRecord{Request: cex.OrderRequest{Leg: cex.Bear, ReduceOnly: true, Price: dec("59.445")}, Status: cex.StatusCancelled, ErrorKind: cex.ErrorPriceLimit}
	if !refusedAt([]cex.OrderRecord{o}, cex.Bear, dec("59.445")) {
		t.Error("a cancel for the price is not taken as a refusal")
	}
	if refusedAt([]cex.OrderRecord{o}, cex.Bear, dec("59.45")) {
		t.Error("another price is held back")
	}
}

// Minor 3: example 3's completion of SOXS at 59.418 is refused for its
// price. On SOXS's next frame with the same price it is not sent again.
func TestRefusedCompletionNotResentAtTheSamePrice(t *testing.T) {
	r := gapRig(t, rigOptions{})
	t0 := utc8Time(11, 25, 5, 3, 5)
	r.enterMatchedOn(t0, entryBook(), thinBear())
	r.gate.setRefuse(func(req cex.OrderRequest) error {
		if req.Purpose == cex.PurposeComplete {
			return &cex.VenueError{Code: "40915", Message: "price out of the band", Kind: cex.ErrorPriceLimit}
		}
		return nil
	})
	book := gapBook(side{lv("40.47", "300"), lv("40.48", "300")}, appendix3Bids("59.418"))
	r.at(t0.Add(time.Second))
	r.books(book)
	r.trade(t0.Add(time.Second))
	for _, ms := range []int{1500, 2000} {
		r.at(t0.Add(time.Duration(ms) * time.Millisecond))
		r.books(book)
		r.idleAt(t0.Add(time.Duration(ms)*time.Millisecond), strategy.ReasonAwaitDepth)
	}
	n := 0
	for _, o := range r.orders() {
		if o.Request.Purpose == cex.PurposeComplete {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d completions at 59.418, want 1", n)
	}
}

// Minor 4: the verdict "never arrived" that did not take effect - the
// order's result came after the round's snapshot - writes no
// order_never_arrived.
func TestLateVerdictWritesNothing(t *testing.T) {
	r := newRig(t, utc8Time(11, 25, 5, 1, 0), rigOptions{})
	r.warmUp()
	t0 := utc8Time(11, 25, 5, 3, 5)
	bull := r.entryWithUnknownBull(t0)
	r.src.setNotFound(bull)
	r.src.mu.Lock()
	r.src.lateVerdict = true
	r.src.mu.Unlock()
	r.at(t0.Add(time.Second))
	r.books(entryBook())
	r.stepAt(t0.Add(time.Second))
	if n := r.eventCount("order_never_arrived"); n != 0 {
		t.Errorf("%d order_never_arrived events for a verdict that did not take effect", n)
	}
}

// Minor 5: while the recovery cannot read the positions, a stop gets its
// alarm after stop_alarm_after, and a stop signal after the alarm is a
// takeover: the process leaves with the cycle marked takeover.
func TestStopAlarmAndTakeoverDuringRecovery(t *testing.T) {
	r := newRig(t, utc8Time(11, 25, 5, 1, 0), rigOptions{})
	r.warmUp()
	t0 := utc8Time(11, 25, 5, 3, 5)
	r.at(t0)
	r.books(entryBook())
	r.trade(t0)
	r.m.recovered.orders = false
	r.gate.unread.Store(true)
	done := runLoop(r, context.Background())
	r.signals <- syscall.SIGTERM
	time.Sleep(100 * time.Millisecond)
	r.at(t0.Add(61 * time.Second))
	waitFor(t, "the stop alarm", func() bool { return r.eventCount("stop_not_flat") == 1 })
	r.signals <- syscall.SIGTERM
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the takeover did not end the recovery")
	}
	if c := r.lastCycle(); c.Status != string(inventory.CycleTakeover) {
		t.Errorf("cycle %s, want takeover", c.Status)
	}
}
