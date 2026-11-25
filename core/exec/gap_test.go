package exec

import (
	"testing"
	"time"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/inventory"
	"etf-pair-arbitrage/core/store"
	"etf-pair-arbitrage/core/strategy"
)

// The tests of this file give the paper venue a latency of 250 ms: the
// entry is decided on one book and matched on another, which the test puts
// in place meanwhile, so that the two legs fill unevenly.

// gapRig is a rig whose orders meet the book 250 ms after they are sent,
// warmed up at 05:03:00 with mu = 3.3333, sigma = 4.
func gapRig(t *testing.T, opts rigOptions) *rig {
	t.Helper()
	opts.latency = 250 * time.Millisecond
	r := newRig(t, utc8Time(11, 25, 5, 1, 0), opts)
	r.warmUp()
	return r
}

// enterMatchedOn sends the entry decide gives at t, has the paper venue
// match it on match, and runs the round the results wake.
func (r *rig) enterMatchedOn(t time.Time, decide, match pairBook) {
	r.t.Helper()
	r.at(t)
	r.books(decide)
	before := len(r.orders())
	r.stepAt(t)
	if len(r.orders()) == before {
		r.t.Fatalf("no entry at %s; idle reason %q", showTime(t), r.m.idleReason)
	}
	r.books(match)
	r.results()
	r.step()
}

// lastOrder is the order sent last.
func (r *rig) lastOrder() cex.OrderRecord {
	r.t.Helper()
	orders := r.orders()
	return orders[len(orders)-1]
}

// thinBear is the match book of PRD appendix A example 3: SOXL's bids
// 40.44 x 500 fill the 105.30 sold at 40.44, SOXS's bids hold 60 shares at
// the limit 59.420 and nothing more above it, so SOXS sells only 60 of
// 70.20 (both at their limits, as the appendix counts them).
func thinBear() pairBook {
	return pairBook{
		bullBids: side{lv("40.44", "500"), lv("40.43", "100")}, bullAsks: side{lv("40.47", "300"), lv("40.48", "300")},
		bearBids: side{lv("59.42", "60"), lv("59.30", "50"), lv("59.29", "50"), lv("59.28", "50"), lv("59.27", "50")},
		bearAsks: side{lv("59.445", "50"), lv("59.45", "50"), lv("59.455", "50"), lv("59.46", "50"), lv("59.465", "50")},
	}
}

// gapBook is a book after the uneven fill: SOXL's asks and SOXS's bids as
// given, the other sides away from them.
func gapBook(bullAsks, bearBids side) pairBook {
	return pairBook{
		bullBids: side{lv("40.45", "300"), lv("40.44", "500")}, bullAsks: bullAsks,
		bearBids: bearBids, bearAsks: side{lv("59.445", "50"), lv("59.45", "50"), lv("59.455", "50"), lv("59.46", "50"), lv("59.465", "50")},
	}
}

// appendix3Bids are SOXS's bids of example 3: 59.418 x 200.
func appendix3Bids(best string) side {
	return side{lv(best, "200"), lv("59.21", "50"), lv("59.20", "50"), lv("59.19", "50"), lv("59.18", "50")}
}

// V9, PRD appendix A example 3 through the whole flow. SOXL sold 105.30 at
// 40.44 (4,258.332), SOXS 60 at 59.42 (3,565.2): the gap is 105.30 x 40 -
// 60 x 60 = 612 USD, 15.30 SOXL to trim or 10.20 SOXS to complete. In the
// round of the results the completion waits for SOXS's own new frame; on it
// (SOXL asks 40.47 x 300, SOXS bids 59.418 x 200):
//   - trim: buy back 15.30 at 40.47 = 619.191 against 15.30 x 40.44 =
//     618.732 sold, 0.459, plus 0.000065 x (618.732 + 619.191) = 0.080464995
//     of fees: 0.539464995;
//   - complete: sell 10.20 at 59.418 = 606.0636 against 10.20 x 59.42 =
//     606.084 planned: 0.0204.
//
// The completion goes; the lot is built with SOXS 70.20 for 3,565.2 +
// 606.0636 = 4,171.2636. With SOXS's bid at 59.30 instead the completion
// would lose 10.20 x 0.12 = 1.224 > 0.539464995: the trim goes, at 40.47 x
// 1.001 = 40.51047 rounded up to 40.52, booked on the lot; its result is
// exactly the trim loss estimated: gross -0.459, fees 0.080464995.
func TestAppendixExample3(t *testing.T) {
	t.Run("complete", func(t *testing.T) {
		r := gapRig(t, rigOptions{})
		t0 := utc8Time(11, 25, 5, 3, 5)
		r.enterMatchedOn(t0, entryBook(), thinBear())
		if r.m.idleReason != strategy.ReasonAwaitDepth {
			t.Fatalf("idle reason after the uneven results %q, want await_depth (the short leg's new frame)", r.m.idleReason)
		}
		r.at(t0.Add(time.Second))
		r.books(gapBook(side{lv("40.47", "300"), lv("40.48", "300")}, appendix3Bids("59.418")))
		r.trade(t0.Add(time.Second))
		d := r.decisions()[len(r.decisions())-1]
		detail := decodeDetail(t, d.Detail)
		if d.Kind != "complete" || detail["leg"] != "bear" || detail["qty"] != "10.2" || detail["price"] != "59.418" ||
			detail["loss_complete_usdt"] != "0.0204" || detail["loss_trim_usdt"] != "0.539464995" || detail["quota_price"] != "59.418" {
			t.Fatalf("decision %s %v", d.Kind, detail)
		}
		o := r.lastOrder()
		if o.Request.Purpose != cex.PurposeComplete || o.Request.ReduceOnly || string(o.Request.LotID) != d.LotID || !o.Request.QuotaPrice.Equal(dec("59.418")) {
			t.Errorf("completion order %+v", o.Request)
		}
		l := r.lastLot(d.LotID)
		if l.Status != string(inventory.LotOpen) {
			t.Errorf("lot %s, want open (built)", l.Status)
		}
		eqDec(t, "bull entry", l.EntryValueBull, "4258.332")
		eqDec(t, "bear entry qty", l.EntryQtyBear, "70.2")
		eqDec(t, "bear entry", l.EntryValueBear, "4171.2636")
		if r.m.lastEntry == nil || !r.m.lastEntry.At.Equal(t0) {
			t.Errorf("the built batch did not reset the spacing at its first send: %+v", r.m.lastEntry)
		}
	})
	t.Run("trim", func(t *testing.T) {
		r := gapRig(t, rigOptions{})
		t0 := utc8Time(11, 25, 5, 3, 5)
		r.enterMatchedOn(t0, entryBook(), thinBear())
		r.at(t0.Add(time.Second))
		r.books(gapBook(side{lv("40.47", "300"), lv("40.48", "300")}, appendix3Bids("59.30")))
		r.trade(t0.Add(time.Second))
		d := r.decisions()[len(r.decisions())-1]
		if detail := decodeDetail(t, d.Detail); d.Kind != "trim" || detail["leg"] != "bull" || detail["qty"] != "15.3" || detail["price"] != "40.52" ||
			detail["loss_complete_usdt"] != "1.224" || detail["loss_trim_usdt"] != "0.539464995" {
			t.Fatalf("decision %s %v", d.Kind, detail)
		}
		if o := r.lastOrder(); o.Request.Purpose != cex.PurposeTrim || !o.Request.ReduceOnly || string(o.Request.LotID) != d.LotID {
			t.Errorf("trim order %+v", o.Request)
		}
		l := r.lastLot(d.LotID)
		if l.Status != string(inventory.LotOpen) || !l.ExitQtyBull.Equal(dec("15.3")) {
			t.Errorf("lot %s, SOXL closed %s; want open with 15.30 trimmed", l.Status, l.ExitQtyBull)
		}
		eqDec(t, "trimmed gross", l.Gross, "-0.459")
		eqDec(t, "trimmed fees", l.Fees, "0.080464995")
		if _, hold := r.inv.Holdings(); !hold[cex.Bull].Equal(dec("90")) || !hold[cex.Bear].Equal(dec("60")) {
			t.Errorf("held %v, want 90 / 60", hold)
		}
	})
}

// V9, PRD appendix A example 4: the best levels are thin. SOXL's asks 40.47
// x 0.4, 40.50 x 300; SOXS's bids 59.418 x 2, 59.36 x 200. Level by level:
//   - trim: 0.4 x 40.47 + 14.9 x 40.50 = 619.638 against 618.732, 0.906,
//     plus 0.000065 x (618.732 + 619.638) = 0.08049405: 0.98649405;
//   - complete: 2 x 59.418 + 8.2 x 59.36 = 605.588 against 606.084: 0.496,
//     at the limit 59.36 (the level that fills 10.20), counted against the
//     quota at the best bid 59.418.
//
// The completion goes and fills on those two levels: SOXS 3,565.2 + 605.588
// = 4,170.788 for 70.20.
func TestAppendixExample4(t *testing.T) {
	r := gapRig(t, rigOptions{})
	t0 := utc8Time(11, 25, 5, 3, 5)
	r.enterMatchedOn(t0, entryBook(), thinBear())
	r.at(t0.Add(time.Second))
	r.books(gapBook(side{lv("40.47", "0.4"), lv("40.50", "300")},
		side{lv("59.418", "2"), lv("59.36", "200"), lv("59.21", "50"), lv("59.20", "50"), lv("59.19", "50")}))
	r.trade(t0.Add(time.Second))
	d := r.decisions()[len(r.decisions())-1]
	if detail := decodeDetail(t, d.Detail); d.Kind != "complete" || detail["qty"] != "10.2" || detail["price"] != "59.36" || detail["quota_price"] != "59.418" ||
		detail["loss_complete_usdt"] != "0.496" || detail["loss_trim_usdt"] != "0.98649405" {
		t.Fatalf("decision %s %v", d.Kind, detail)
	}
	l := r.lastLot(d.LotID)
	if l.Status != string(inventory.LotOpen) {
		t.Errorf("lot %s, want open", l.Status)
	}
	eqDec(t, "bear entry", l.EntryValueBear, "4170.788")
}

// V7: a completion never takes more than the quota left; the part the
// quota does not hold is trimmed after the completion's result (PRD 6.4).
// M = 819, so the quota of 8,190 sizes the entry: K = 8,190 / (40.45/40 +
// 59.437/60) = 4,091.18, sell 102.27 / 68.18. SOXL fills 102.27 at 40.44
// (4,135.7988), SOXS nothing: 8,190 - 4,135.7988 = 4,054.2012 is left. On
// SOXS's bids 59.70 x 200 the gap of 102.27 x 40 = 4,090.8 USD would need
// 68.18 SOXS, but the quota holds 4,054.2012 / 59.70 = 67.909: sell 67.90
// for 4,053.63. The gap left, 4,090.8 - 67.90 x 60 = 16.8 USD, finds no
// quota on SOXS's next frame (0.5712 left): trim 0.42 SOXL. The lot is
// built with 101.85 / 67.90, and the quota used never passes 8,190.
func TestCompletionWithinTheQuota(t *testing.T) {
	r := gapRig(t, rigOptions{capital: "819"})
	t0 := utc8Time(11, 25, 5, 3, 5)
	match := thinBear()
	match.bearBids = side{lv("59.30", "50"), lv("59.29", "50"), lv("59.28", "50"), lv("59.27", "50"), lv("59.26", "50")}
	r.enterMatchedOn(t0, entryBook(), match)
	if _, hold := r.inv.Holdings(); !hold[cex.Bull].Equal(dec("102.27")) || !hold[cex.Bear].IsZero() {
		t.Fatalf("held %v, want 102.27 / 0", hold)
	}
	r.quotaLeft("4054.2012")

	bids := side{lv("59.70", "200"), lv("59.69", "50"), lv("59.68", "50"), lv("59.67", "50"), lv("59.66", "50")}
	book := gapBook(side{lv("40.47", "300"), lv("40.48", "300")}, bids)
	r.at(t0.Add(time.Second))
	r.books(book)
	r.trade(t0.Add(time.Second))
	d := r.decisions()[len(r.decisions())-1]
	if detail := decodeDetail(t, d.Detail); d.Kind != "complete" || detail["qty"] != "67.9" || detail["price"] != "59.7" || detail["quota_left"] != "4054.2012" {
		t.Fatalf("decision %s %v", d.Kind, detail)
	}
	if _, used, _ := r.inv.Quota(); !used.Equal(dec("8189.4288")) {
		t.Errorf("quota used %s, want 4,135.7988 + 4,053.63 = 8,189.4288", used)
	}

	r.at(t0.Add(2 * time.Second))
	r.books(book)
	r.trade(t0.Add(2 * time.Second))
	d = r.decisions()[len(r.decisions())-1]
	if detail := decodeDetail(t, d.Detail); d.Kind != "trim" || detail["leg"] != "bull" || detail["qty"] != "0.42" {
		t.Fatalf("decision %s %v, want a trim of 0.42 SOXL", d.Kind, detail)
	}
	l := r.lastLot(d.LotID)
	if l.Status != string(inventory.LotOpen) || !l.ExitQtyBull.Equal(dec("0.42")) || !l.EntryQtyBear.Equal(dec("67.9")) {
		t.Errorf("lot %s, SOXL trimmed %s, SOXS %s; want open, 0.42, 67.90", l.Status, l.ExitQtyBull, l.EntryQtyBear)
	}
	if total, used, _ := r.inv.Quota(); used.GreaterThan(total) {
		t.Errorf("quota used %s over the total %s", used, total)
	}
}

// Both legs have their final state at the venue while the second leg's
// notification is held back: the loop sends nothing more and waits for it;
// once it arrives, the lot is built and every quantity and amount counted
// once (example 1's fills: 4,259.385 and 4,172.0292).
func TestHeldNotificationSendsNothingExtra(t *testing.T) {
	r := newRig(t, utc8Time(11, 25, 5, 1, 0), rigOptions{})
	r.warmUp()
	t0 := utc8Time(11, 25, 5, 3, 5)
	r.at(t0)
	r.books(entryBook())
	r.gate.hold("SOXLUSDT")
	r.stepAt(t0)
	waitFor(t, "SOXS's result and SOXL's held", func() bool { return len(r.cex.UnresolvedOrders()) == 1 && r.gate.heldEvents() > 0 })
	for i := range 3 {
		r.idleAt(t0.Add(time.Duration(i)*100*time.Millisecond), strategy.ReasonAwaitOrders)
	}
	if n := len(r.orders()); n != 2 {
		t.Fatalf("%d orders while SOXL's result is held, want 2", n)
	}

	r.gate.release()
	r.results()
	for i := range 3 {
		r.stepAt(t0.Add(time.Second + time.Duration(i)*100*time.Millisecond))
	}
	if n := len(r.orders()); n != 2 {
		t.Errorf("%d orders after the release, want 2", n)
	}
	lot := r.decisions()[0].LotID
	l := r.lastLot(lot)
	if l.Status != string(inventory.LotOpen) {
		t.Errorf("lot %s, want open", l.Status)
	}
	eqDec(t, "bull qty", l.EntryQtyBull, "105.3")
	eqDec(t, "bull value", l.EntryValueBull, "4259.385")
	eqDec(t, "bear qty", l.EntryQtyBear, "70.2")
	eqDec(t, "bear value", l.EntryValueBear, "4172.0292")
	r.quotaLeft("11568.5858")
	if pos := r.inv.LedgerPositions(); !pos[cex.Bull].Equal(dec("-105.3")) || !pos[cex.Bear].Equal(dec("-70.2")) {
		t.Errorf("ledger positions %v, want -105.30 / -70.20", pos)
	}
}

// The completion window runs from the batch's first send (PRD 8.2 as
// narrowed): SOXL is sent at second 0 and fills, its final state arrives
// at second 4, and SOXS's book, in conflict meanwhile, recovers at second
// 6. SOXS may not be completed while its book is in conflict, and past 5 s
// not at all: SOXL is trimmed and no completion order is ever sent.
func TestCompletionWindowFromTheFirstSend(t *testing.T) {
	r := gapRig(t, rigOptions{})
	t0 := utc8Time(11, 25, 5, 3, 5)
	r.at(t0)
	r.books(entryBook())
	r.gate.hold("SOXLUSDT")
	r.stepAt(t0)
	r.books(thinBear()) // SOXS sells 60 of 70.20
	waitFor(t, "SOXS's result and SOXL's held", func() bool { return len(r.cex.UnresolvedOrders()) == 1 && r.gate.heldEvents() > 0 })
	r.best(cex.Bear, lv("59.41", "50"), lv("59.445", "50")) // after SOXS's match: its book is in conflict from now on
	for s := 1; s <= 3; s++ {
		r.idleAt(t0.Add(time.Duration(s)*time.Second), strategy.ReasonAwaitOrders)
	}

	t4 := t0.Add(4 * time.Second)
	r.gate.release()
	r.results()
	r.trade(t4) // the trim: SOXS cannot be completed on a book in conflict
	if d := r.decisions()[len(r.decisions())-1]; d.Kind != "trim" {
		t.Fatalf("decision at second 4 %s, want trim", d.Kind)
	}
	r.at(t0.Add(6 * time.Second))
	r.books(gapBook(side{lv("40.47", "300"), lv("40.48", "300")}, appendix3Bids("59.418")))
	r.stepAt(t0.Add(6 * time.Second))
	for _, o := range r.orders() {
		if o.Request.Purpose == cex.PurposeComplete {
			t.Errorf("completion order %s sent", o.Request.ClientOrderID)
		}
	}
	if l := r.lastLot(r.decisions()[0].LotID); l.Status != string(inventory.LotOpen) || !l.ExitQtyBull.Equal(dec("15.3")) {
		t.Errorf("lot %s with %s SOXL trimmed, want open with 15.30", l.Status, l.ExitQtyBull)
	}
}

// V9, PRD 8.6: a second batch that filled one leg only is trimmed back all
// of it once its window has passed, and is void (one_leg); the first lot is
// not touched and the cycle stays open; the spacing keeps the first lot's
// entry. The re-entry of the scripted Session sells 60 SOXL at 40.50 and 20
// at 40.51 (2,430.2) and no SOXS; at second 6 the 60 are bought back at
// 40.53 (2,431.8).
func TestOneLegBatchIsTrimmedAndVoid(t *testing.T) {
	r := gapRig(t, rigOptions{})
	t0 := utc8Time(11, 25, 5, 3, 5)
	r.at(t0)
	r.books(entryBook())
	r.trade(t0)
	lot1 := r.decisions()[0].LotID

	t1 := utc8Time(11, 25, 5, 3, 20)
	reentry := pairBook{
		bullBids: side{lv("40.51", "20"), lv("40.50", "100")}, bullAsks: side{lv("40.53", "100"), lv("40.54", "100")},
		bearBids: side{lv("59.47", "10"), lv("59.465", "10"), lv("59.46", "20"), lv("59.455", "20"), lv("59.45", "20")},
		bearAsks: side{lv("59.48", "50"), lv("59.485", "50"), lv("59.49", "50"), lv("59.495", "50"), lv("59.50", "50")},
	}
	nothing := reentry
	nothing.bearBids = side{lv("59.40", "50"), lv("59.39", "50"), lv("59.38", "50"), lv("59.37", "50"), lv("59.36", "50")}
	r.enterMatchedOn(t1, reentry, nothing)
	lot2 := r.decisions()[1].LotID

	// Past the 5 s window only the trim is left.
	r.at(t1.Add(6 * time.Second))
	r.books(reentry)
	r.trade(t1.Add(6 * time.Second))
	if d := r.decisions()[len(r.decisions())-1]; d.Kind != "trim" || decodeDetail(t, d.Detail)["qty"] != "60" {
		t.Fatalf("decision %s, want a trim of 60 SOXL", d.Kind)
	}
	l2 := r.lastLot(lot2)
	if l2.Status != string(inventory.LotVoid) || l2.VoidReason != inventory.VoidOneLeg {
		t.Errorf("lot 2 %s %s, want void one_leg", l2.Status, l2.VoidReason)
	}
	eqDec(t, "lot 2 gross", l2.Gross, "-1.6") // 2,430.2 - 2,431.8
	l1 := r.lastLot(lot1)
	if l1.Status != string(inventory.LotOpen) || !l1.EntryQtyBull.Equal(dec("105.3")) || !l1.ExitQtyBull.IsZero() {
		t.Errorf("lot 1 %s %s / %s closed, want open and untouched", l1.Status, l1.EntryQtyBull, l1.ExitQtyBull)
	}
	if r.inv.Cycle() == nil {
		t.Error("the cycle closed")
	}
	if r.m.lastEntry == nil || !r.m.lastEntry.At.Equal(t0) {
		t.Errorf("the spacing was reset by the void batch: %+v", r.m.lastEntry)
	}
}

// V9: a cycle whose only batch filled one leg ends as one_leg once the leg
// is trimmed back; and one whose only fill is too small for any order
// keeps it as dust, recorded and not traded (owner item 47).
func TestOneLegCycles(t *testing.T) {
	t.Run("trimmed", func(t *testing.T) {
		r := gapRig(t, rigOptions{})
		t0 := utc8Time(11, 25, 5, 3, 5)
		match := thinBear()
		match.bearBids = side{lv("59.30", "50"), lv("59.29", "50"), lv("59.28", "50"), lv("59.27", "50"), lv("59.26", "50")}
		r.enterMatchedOn(t0, entryBook(), match)
		r.at(t0.Add(6 * time.Second))
		r.books(thinBear())
		r.trade(t0.Add(6 * time.Second)) // the trim of all 105.30 SOXL
		if c := r.lastCycle(); r.inv.Cycle() != nil || c.EndReason != inventory.EndOneLeg {
			t.Errorf("cycle open %t, end %q; want closed one_leg", r.inv.Cycle() != nil, c.EndReason)
		}
	})
	t.Run("dust", func(t *testing.T) {
		// SOXL fills nothing, SOXS 0.05 at 59.42: a gap of 3 USD, 0.05 SOXS
		// to trim (2.97 USDT) or 0.07 SOXL to complete (2.83 USDT), both
		// under the venue's 5 USDT. On SOXL's new frame no order is left:
		// the lot is void (one_leg), the cycle closes with the 0.05 as dust.
		r := gapRig(t, rigOptions{})
		t0 := utc8Time(11, 25, 5, 3, 5)
		match := thinBear()
		match.bullBids = side{lv("40.30", "500"), lv("40.29", "100")}
		match.bearBids = side{lv("59.42", "0.05"), lv("59.30", "50"), lv("59.29", "50"), lv("59.28", "50"), lv("59.27", "50")}
		r.enterMatchedOn(t0, entryBook(), match)
		r.at(t0.Add(time.Second))
		r.books(match)
		r.stepAt(t0.Add(time.Second))
		if n := len(r.orders()); n != 2 {
			t.Errorf("%d orders, want the entry's 2", n)
		}
		c := r.lastCycle()
		if r.inv.Cycle() != nil || c.EndReason != inventory.EndOneLeg || !c.DustBear.Equal(dec("-0.05")) {
			t.Errorf("cycle open %t, end %q, dust %s; want closed one_leg with -0.05 SOXS", r.inv.Cycle() != nil, c.EndReason, c.DustBear)
		}
		dust := false
		for _, row := range r.sink.Rows("event") {
			dust = dust || row.(store.EventRow).Kind == "dust_left"
		}
		if !dust {
			t.Error("no dust_left event")
		}
		if pos := r.inv.LedgerPositions(); !pos[cex.Bear].Equal(dec("-0.05")) {
			t.Errorf("ledger SOXS %s, want the dust -0.05", pos[cex.Bear])
		}
	})
}

// V9, PRD 8.5: from the regular open a gap is only trimmed, completion
// would be an entry. Example 3's books, where completing is cheaper, with
// the results after 22:30.
func TestRegularHoursOnlyTrim(t *testing.T) {
	r := newRig(t, utc8Time(11, 25, 22, 26, 0), rigOptions{latency: 250 * time.Millisecond})
	r.warmUp()
	r.endMinute() // 22:28 ends on book A: the mean of 22:27 and 22:28 is mu = 3.3333, sigma = 4
	t0 := utc8Time(11, 25, 22, 29, 59)
	r.enterMatchedOn(t0, entryBook(), thinBear())
	t1 := utc8Time(11, 25, 22, 30, 0).Add(500 * time.Millisecond)
	r.at(t1)
	r.books(gapBook(side{lv("40.47", "300"), lv("40.48", "300")}, appendix3Bids("59.418")))
	r.trade(t1)
	if d := r.decisions()[len(r.decisions())-1]; d.Kind != "trim" || decodeDetail(t, d.Detail)["qty"] != "15.3" {
		t.Errorf("decision %s, want the trim of 15.30 SOXL", d.Kind)
	}
}

// V9, PRD 8.7: an exit at the line that closes one leg more than the other
// goes on reducing the leg left larger, as an exit, first in first out. The
// exit of example 2 buys back all of 105.30 SOXL but only 30 of 70.20 SOXS;
// the 40.20 left are bought back at once, through the best ask 59.385 by
// 10 bps: 59.445; the cycle closes at the line.
func TestExitGapReducesTheLargerLeg(t *testing.T) {
	r := gapRig(t, rigOptions{})
	t0 := utc8Time(11, 25, 5, 3, 5)
	r.at(t0)
	r.books(entryBook())
	r.trade(t0)

	t1 := utc8Time(11, 25, 5, 3, 30)
	r.at(t1)
	r.books(exitBook())
	r.stepAt(t1)
	match := exitBook()
	match.bearAsks = side{lv("59.385", "30"), lv("59.44", "100"), lv("59.45", "50"), lv("59.46", "50"), lv("59.47", "50")}
	r.books(match)
	r.results()
	r.step() // the round of the results: the trim goes at once
	d := r.decisions()[len(r.decisions())-1]
	if detail := decodeDetail(t, d.Detail); d.Kind != "trim" || detail["leg"] != "bear" || detail["qty"] != "40.2" || detail["price"] != "59.445" || detail["purpose"] != "exit" {
		t.Fatalf("decision %s %v, want an exit of the 40.20 SOXS left", d.Kind, detail)
	}
	if o := r.lastOrder(); o.Request.Purpose != cex.PurposeExit || !o.Request.ReduceOnly || o.Request.LotID != "" {
		t.Errorf("order %+v", o.Request)
	}
	r.results()
	r.stepAt(t1.Add(time.Second))
	if c := r.lastCycle(); r.inv.Cycle() != nil || c.EndReason != inventory.EndExitLine {
		t.Errorf("cycle open %t, end %q; want closed exit_line", r.inv.Cycle() != nil, c.EndReason)
	}
}
