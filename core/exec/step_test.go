package exec

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/inventory"
	"etf-pair-arbitrage/core/store"
	"etf-pair-arbitrage/core/strategy"
)

// V4: the reasons of PRD 5.13 steps 1 to 3 come in their order. On a book
// that signals a short (x = +10) every blocker is set before the first
// round; each round names the first that is left, the test lifts it, and the
// entry goes once none is. A stop while flat lets the process leave and is
// not among them; capital_pending needs a restored cycle and has its own
// test (TestCapitalPendingStopsEntries).
func TestEntryReasonOrder(t *testing.T) {
	r := newRig(t, utc8Time(11, 25, 5, 1, 0), rigOptions{})
	r.warmUp()

	// 05:03:01: an entry whose cycle row is not written leaves a void row
	// waiting to be confirmed (inv.StoreBlocked). The loop's own store is
	// healthy for that round, so that the entry is tried.
	r.at(utc8Time(11, 25, 5, 3, 1))
	r.books(entryBook())
	r.m.store = &store.Fake{}
	r.sink.Fail(errors.New("database down"))
	r.idleAt(utc8Time(11, 25, 5, 3, 1), strategy.ReasonStoreError)
	r.m.store = r.sink
	if !r.inv.StoreBlocked() {
		t.Fatal("no void row is waiting")
	}

	// The data: SOXL's depth is from 05:03:01, 11 s old at t0; SOXS's from
	// 05:03:08 with a newer books1 ask of 59.44 against the depth's 59.445;
	// F was read 61 s before t0.
	b := entryBook()
	r.at(utc8Time(11, 25, 5, 3, 8))
	r.depth(cex.Bear, b.bearBids, b.bearAsks)
	r.at(utc8Time(11, 25, 5, 3, 9))
	r.best(cex.Bear, lv("59.437", "0.4"), lv("59.44", "50"))
	t0 := utc8Time(11, 25, 5, 3, 12)
	r.fxFixed = true
	r.at(t0)
	r.src.setFX("1", t0.Add(-61*time.Second))

	// The program, the hours and the anchors. The paper venue never refuses
	// an order: the last batch is set as one with a refused order whose
	// results came half a second before t0, after every depth held.
	r.m.stop.noEntries = true
	r.src.set(true, true)
	regularOpen := r.m.session.RegularOpen
	r.m.session.RegularOpen = t0.Add(-time.Hour)
	bell := r.src.Anchors()
	r.src.setAnchors(cex.Anchors{SessionID: bell.SessionID})
	r.m.batch = &batchState{kind: cex.PurposeEntry, settled: true, doneAt: t0.Add(-500 * time.Millisecond), rejected: true}

	steps := []struct {
		want strategy.Reason
		lift func()
	}{
		{strategy.ReasonNoNewEntries, func() { r.m.stop.noEntries = false }},
		{strategy.ReasonNotReady, func() { r.src.set(false, true) }},
		{strategy.ReasonSettings, func() { r.src.set(false, false) }},
		{strategy.ReasonStoreError, func() { r.sink.Fail(nil) }}, // the store is unhealthy
		{strategy.ReasonStoreError, func() { // the void row waits: inventory's Run confirms it
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { r.inv.Run(ctx); close(done) }()
			waitFor(t, "the void row confirmed", func() bool { return !r.inv.StoreBlocked() })
			cancel()
			<-done
		}},
		{strategy.ReasonRegularHours, func() { r.m.session.RegularOpen = regularOpen }},
		{strategy.ReasonNoAnchor, func() { a := bell; a.EntryBan = true; r.src.setAnchors(a) }},
		{strategy.ReasonActionWindow, func() { r.src.setAnchors(bell) }},
		{strategy.ReasonDepthBull, func() { // a SOXL depth older than the batch's results
			r.at(utc8Time(11, 25, 5, 3, 10))
			r.depth(cex.Bull, b.bullBids, b.bullAsks)
			r.at(t0)
		}},
		{strategy.ReasonConflictBear, func() { // a SOXS depth that ends the conflict, also older than the results
			r.at(utc8Time(11, 25, 5, 3, 11))
			r.depth(cex.Bear, b.bearBids, b.bearAsks)
			r.at(t0)
		}},
		{strategy.ReasonFXStale, func() { r.src.setFX("1", t0) }},
		{strategy.ReasonAwaitDepth, func() { r.books(b) }}, // both legs anew, after the results
		{strategy.ReasonRejectedBackoff, func() {}},        // lifted by the clock: 1 s after the results
	}
	for i, s := range steps {
		r.idleAt(t0, s.want)
		if i == 0 && r.m.batch.doneAt.IsZero() {
			t.Fatal("the batch set by the test was replaced")
		}
		s.lift()
	}
	r.trade(t0.Add(600 * time.Millisecond))
}

// capital_pending (PRD 6.3): a capital changed while a cycle was unfinished
// opens no lot until that cycle has closed. It comes after store_error and
// before the trading hours. The ledger is restored with M = 3,000 and the
// configured M is 2,000, with example 1's lot held.
func TestCapitalPendingStopsEntries(t *testing.T) {
	start := utc8Time(11, 25, 5, 1, 0)
	state := heldState(start, "3000", "70.2", "4172.0292")
	r := newRig(t, start, rigOptions{state: state})
	if !r.inv.CapitalPending() {
		t.Fatal("the capital is not pending")
	}
	r.endMinute()
	r.endMinute()
	r.stepAt(utc8Time(11, 25, 5, 3, 0))

	t0 := utc8Time(11, 25, 5, 3, 5)
	r.at(t0)
	r.books(entryBook())
	r.sink.Fail(errors.New("database down"))
	r.idleAt(t0, strategy.ReasonStoreError)
	r.sink.Fail(nil)
	r.idleAt(t0, strategy.ReasonCapitalPending)
	regularOpen := r.m.session.RegularOpen
	r.m.session.RegularOpen = t0.Add(-time.Hour)
	r.idleAt(t0, strategy.ReasonCapitalPending)
	r.m.session.RegularOpen = regularOpen
}

// heldState is a ledger as a restart reads it back: capital M and one open
// short lot of the Session of start, opened an hour earlier, with SOXL
// 105.30 at 4,259.385 and SOXS bearQty at bearValue.
func heldState(start time.Time, capital, bearQty, bearValue string) *store.State {
	opened := start.Add(-time.Hour)
	cycleID := fmt.Sprintf("c%d", opened.UnixMilli())
	return &store.State{
		Inventory: &store.InventoryStateRow{Capital: dec(capital), SinceMs: opened.Add(-time.Hour).UnixMilli()},
		Cycle: &store.CycleRow{CycleID: cycleID, SessionID: "2026-11-24", Direction: "short", Status: string(inventory.CycleOpen),
			PairAnchorKind: "bell", PairAnchorBull: dec("40"), PairAnchorBear: dec("60"), OpenedMs: opened.UnixMilli(), FundingKnown: true},
		Lots: []store.LotRow{{LotID: cycleID + "-1", CycleID: cycleID, Seq: 1, Status: string(inventory.LotOpen), Direction: "short",
			EntryQtyBull: dec("105.3"), EntryValueBull: dec("4259.385"), EntryQtyBear: dec(bearQty), EntryValueBear: dec(bearValue), OpenedMs: opened.UnixMilli()}},
	}
}

// V3, the execution part: while a leg's book is in conflict there is no
// entry and no exit at the line; the forced exit is not held up, and its
// limit comes from the newer best price.
func TestConflictStopsEntryAndLineExitNotForcedExit(t *testing.T) {
	r := newRig(t, utc8Time(11, 25, 5, 1, 0), rigOptions{})
	r.warmUp()
	r.at(utc8Time(11, 25, 5, 3, 5))
	r.books(entryBook())
	r.trade(utc8Time(11, 25, 5, 3, 5))
	orders := len(r.orders())

	// The exit line is reached (x = -8.33), but books1 of SOXS shows an
	// ask the depth does not have: no exit, the reason is the conflict.
	t1 := utc8Time(11, 25, 5, 3, 30)
	r.at(t1)
	r.books(exitBook())
	r.best(cex.Bear, lv("59.37", "50"), lv("59.39", "100"))
	r.idleAt(t1, strategy.ReasonConflictBear)
	r.idleAt(t1.Add(time.Second), strategy.ReasonConflictBear)

	// The close window with the conflict still there: the forced exit
	// sends both legs. SOXS's limit is books1's ask 59.39 through 10 bps,
	// 59.44939 rounded up: 59.450 (the depth's 59.385 would give 59.445).
	t2 := utc8Time(11, 26, 4, 59, 0)
	r.at(t2)
	r.books(exitBook())
	r.best(cex.Bear, lv("59.37", "50"), lv("59.39", "100"))
	if !r.cex.Books().ByLeg[cex.Bear].Conflict {
		t.Fatal("the bear book is not in conflict")
	}
	r.trade(t2)
	if got := len(r.orders()) - orders; got != 2 {
		t.Errorf("%d forced orders, want 2", got)
	}
	forced := r.decisions()[len(r.decisions())-1]
	if detail := decodeDetail(t, forced.Detail); forced.Kind != "forced" || detail["price_bear"] != "59.45" || detail["conflict_bear"] != true {
		t.Errorf("forced decision %s %v", forced.Kind, detail)
	}
	r.stepAt(t2.Add(time.Second))
	if c := r.lastCycle(); c.Status != string(inventory.CycleClosed) || c.EndReason != inventory.EndCloseWindow {
		t.Errorf("cycle %s %s, want closed close_window", c.Status, c.EndReason)
	}
}

// The minimum batch of 100 USDT (owner item 59) stops a small entry, and an
// exit below it is sent all the same.
func TestMinBatchBlocksEntryNotExit(t *testing.T) {
	r := newRig(t, utc8Time(11, 25, 5, 1, 0), rigOptions{})
	r.warmUp()

	// A thin book at the prices of example 1: SOXL offers (1 + 1) / 2 = 1
	// share (40 USD anchored), SOXS (5 x 0.4) / 2 = 1 share (60 USD): K =
	// 40, sell 1.00 / 0.66 for 40.44 + 39.2172 = 79.66 USDT < 100.
	t0 := utc8Time(11, 25, 5, 3, 5)
	r.at(t0)
	thin := entryBook()
	thin.bullBids = side{lv("40.45", "1"), lv("40.44", "1")}
	thin.bearBids = side{lv("59.437", "0.4"), lv("59.431", "0.4"), lv("59.428", "0.4"), lv("59.425", "0.4"), lv("59.420", "0.4")}
	r.books(thin)
	r.idleAt(t0, strategy.ReasonTooSmall)
	if d := r.decisions(); len(d) != 1 || d[0].Kind != "at_line_blocked" || d[0].Reason != "too_small" {
		t.Errorf("decisions %+v, want one at_line_blocked too_small", d)
	}

	// A full entry, then a thin exit book: 1.00 / 0.66 at 40.38 / 59.400,
	// 79.58 USDT, is sent.
	r.at(t0.Add(time.Second))
	r.books(entryBook())
	r.trade(t0.Add(time.Second))
	r.at(t0.Add(5 * time.Second))
	exit := exitBook()
	exit.bullAsks = side{lv("40.37", "1"), lv("40.38", "1")}
	exit.bearAsks = side{lv("59.385", "0.4"), lv("59.391", "0.4"), lv("59.394", "0.4"), lv("59.396", "0.4"), lv("59.400", "0.4")}
	r.books(exit)
	r.trade(t0.Add(5 * time.Second))
	d := r.decisions()[len(r.decisions())-1]
	if detail := decodeDetail(t, d.Detail); d.Kind != "exit" || detail["qty_bull"] != "1" || detail["qty_bear"] != "0.66" {
		t.Errorf("last decision %s %v, want an exit of 1 / 0.66", d.Kind, detail)
	}
	if _, hold := r.inv.Holdings(); !hold[cex.Bull].Equal(dec("104.3")) || !hold[cex.Bear].Equal(dec("69.54")) {
		t.Errorf("held %v, want 104.30 / 69.54", hold)
	}
}

// V10: the exit line sits past the mean on the entry's axis and the cycle
// exits at a loss too. The capital is 850 USDT (quota 8,500): the entry of
// example 1 leaves 68.59, so the market moving away gives the short signal
// again and no batch (quota_exhausted). Two minutes of that market move
// the mean to its mid, the spread 46.8333 (sigma 0), and the exit line is
// x <= -0.5 x 0 = 0.
func TestExitAtTheLineAtALoss(t *testing.T) {
	r := newRig(t, utc8Time(11, 25, 5, 1, 0), rigOptions{capital: "850"})
	r.warmUp()
	r.at(utc8Time(11, 25, 5, 3, 5))
	r.books(entryBook())
	r.trade(utc8Time(11, 25, 5, 3, 5))
	r.quotaLeft("68.5858") // 8,500 - 4,259.385 - 4,172.0292

	// The moved market: SOXL bids 40.55 / 40.54, asks 40.57 / 40.58 (mid
	// 40.56); SOXS as example 1 (mid 59.441). x at the bids is +35: on the
	// line, but the quota left holds 0.85 / 0.57 shares, 68.3 USDT.
	moved := entryBook()
	moved.bullBids, moved.bullAsks = side{lv("40.55", "300"), lv("40.54", "500")}, side{lv("40.57", "300"), lv("40.58", "300")}
	for _, at := range []time.Time{utc8Time(11, 25, 5, 3, 10), utc8Time(11, 25, 5, 3, 59), utc8Time(11, 25, 5, 4, 59)} {
		r.at(at)
		r.books(moved)
		r.step()
	}
	if d := r.decisions(); len(d) != 2 || d[1].Reason != "quota_exhausted" {
		t.Errorf("decisions %+v, want the entry and one at_line_blocked quota_exhausted", d)
	}

	// 05:05:00: mu = D(40.56, 59.441) = 46.8333, sigma = 0. The asks SOXL
	// 40.54 / 40.55 and SOXS 59.436 ... 59.440 give x = 44.1667 - 46.8333 =
	// -2.67 <= 0: buy back all of 105.30 / 70.20, above the entry prices.
	// Fills: SOXL 105.30 @ 40.54 = 4,268.862 (fee 0.27747603); SOXS 50 @
	// 59.436 + 20.2 @ 59.437 = 4,172.4274 (fee 0.271207781). Gross
	// (4,259.385 - 4,268.862) + (4,172.0292 - 4,172.4274) = -9.8752; fees
	// 0.548041923 + 0.548683811 = 1.096725734; net -10.971925734. R is the
	// loss, so the quota falls to 10 x (850 - 10.971925734) = 8,390.28074266.
	t1 := utc8Time(11, 25, 5, 5, 0)
	r.at(t1)
	r.books(pairBook{
		bullBids: side{lv("40.52", "300"), lv("40.51", "300")}, bullAsks: side{lv("40.54", "300"), lv("40.55", "300")},
		bearBids: side{lv("59.43", "50"), lv("59.429", "50"), lv("59.428", "50"), lv("59.427", "50"), lv("59.426", "50")},
		bearAsks: side{lv("59.436", "50"), lv("59.437", "50"), lv("59.438", "50"), lv("59.439", "50"), lv("59.440", "50")},
	})
	r.trade(t1)
	r.stepAt(t1.Add(time.Second))
	c := r.lastCycle()
	if c.Status != string(inventory.CycleClosed) || c.EndReason != inventory.EndExitLine {
		t.Fatalf("cycle %s %s, want closed exit_line", c.Status, c.EndReason)
	}
	eqDec(t, "gross", c.Gross, "-9.8752")
	eqDec(t, "fees", c.Fees, "1.096725734")
	eqDec(t, "net", c.Net, "-10.971925734")
	if total, _, _ := r.inv.Quota(); !total.Equal(dec("8390.28074266")) {
		t.Errorf("quota total %s, want 8390.28074266", total)
	}
}

// V10: after part of the cycle is closed at the line a same-side entry is
// made again, and a reverse entry comes only once the cycle is flat.
func TestReentryAfterPartialExitAndReverseAfterFlat(t *testing.T) {
	r := newRig(t, utc8Time(11, 25, 5, 1, 0), rigOptions{})
	r.warmUp()
	r.at(utc8Time(11, 25, 5, 3, 5))
	r.books(entryBook())
	r.trade(utc8Time(11, 25, 5, 3, 5))

	// SOXS's asks half as deep: offered 350 / 50 shares, 14,000 / 3,000 USD:
	// buy back 75.00 / 50.00 of 105.30 / 70.20.
	r.at(utc8Time(11, 25, 5, 3, 30))
	thin := exitBook()
	thin.bearAsks = side{lv("59.385", "50"), lv("59.391", "20"), lv("59.394", "15"), lv("59.396", "10"), lv("59.400", "5")}
	r.books(thin)
	r.trade(utc8Time(11, 25, 5, 3, 30))
	if _, hold := r.inv.Holdings(); !hold[cex.Bull].Equal(dec("30.3")) || !hold[cex.Bear].Equal(dec("20.2")) {
		t.Fatalf("held %v after the partial exit, want 30.30 / 20.20", hold)
	}
	r.endMinute() // book A: the mean stays 3.3333, sigma 4

	// 05:04:10, past the 60 s spacing: example 1's book again, a second
	// short lot of 105.30 / 70.20 in the same cycle.
	r.at(utc8Time(11, 25, 5, 4, 10))
	r.books(entryBook())
	r.trade(utc8Time(11, 25, 5, 4, 10))
	c := r.inv.Cycle()
	if c == nil || len(c.Lots) != 2 || c.Lots[1].Direction != strategy.Short {
		t.Fatalf("cycle %+v, want two short lots", c)
	}
	shortCycle := c.ID

	// 05:04:15, example 2's prices with 0.1 share on each ask: the long
	// signal (x = -8.33 <= -8) and the short's exit line at once, but the
	// exit sizes to 0.10 / 0.06 shares, 4.04 / 3.56 USDT, below the venue's
	// 5 USDT: no exit, and the long entry is refused while short.
	r.at(utc8Time(11, 25, 5, 4, 15))
	tiny := exitBook()
	tiny.bullAsks = side{lv("40.37", "0.1"), lv("40.38", "0.1")}
	tiny.bearAsks = side{lv("59.385", "0.02"), lv("59.391", "0.02"), lv("59.394", "0.02"), lv("59.396", "0.02"), lv("59.400", "0.02")}
	r.books(tiny)
	r.idleAt(utc8Time(11, 25, 5, 4, 15), strategy.ReasonHoldingOtherSide)
	if d := r.decisions()[len(r.decisions())-1]; d.Kind != "at_line_blocked" || d.Reason != "holding_other_side" || d.Direction != "long" {
		t.Errorf("last decision %s %s %s, want at_line_blocked holding_other_side long", d.Kind, d.Reason, d.Direction)
	}

	// Example 2's book: x = -8.33 at the asks is the short's exit line and
	// a long signal (<= -8) at once. The exit goes first and closes all of
	// 135.60 / 90.40 (14,000 / 6,000 USD offered against 5,424 held).
	r.at(utc8Time(11, 25, 5, 4, 20))
	r.books(exitBook())
	r.trade(utc8Time(11, 25, 5, 4, 20))
	if d := r.decisions()[len(r.decisions())-1]; d.Kind != "exit" {
		t.Fatalf("last decision %s, want the exit", d.Kind)
	}
	r.stepAt(utc8Time(11, 25, 5, 4, 21))
	if r.inv.Cycle() != nil {
		t.Fatal("the short cycle is still open")
	}

	// Flat, on a new frame of the same book: the long entry, in a new
	// cycle; a reverse entry does not wait for the spacing.
	r.at(utc8Time(11, 25, 5, 4, 25))
	r.books(exitBook())
	r.trade(utc8Time(11, 25, 5, 4, 25))
	c = r.inv.Cycle()
	if c == nil || c.ID == shortCycle || c.Direction != strategy.Long {
		t.Fatalf("cycle %+v, want a new long cycle", c)
	}
}

// V12: a change from the bell anchor to the NAV within the Session keeps
// the positions, does not stop entries and does not warm the mean up again;
// the next lot of the cycle is sized with the cycle's pairing anchors, and a
// same-side entry within 60 s of an anchor change waits the whole spacing.
func TestAnchorChangeKeepsEntriesAndPairing(t *testing.T) {
	r := newRig(t, utc8Time(11, 25, 5, 1, 0), rigOptions{})
	r.warmUp()
	r.at(utc8Time(11, 25, 5, 3, 5))
	r.books(entryBook())
	r.trade(utc8Time(11, 25, 5, 3, 5))

	// 05:03:10: NAV 40.10 / 59.90. On the re-entry book of the scripted
	// Session x is (40.50 - 40.40) / 40.10 + (59.45 - 59.42) / 59.90 = 29.95
	// spread units under the NAV, past e + 1 = 11; without the change that
	// entry would go (TestScriptedSession), with it the entry waits the
	// spacing.
	r.at(utc8Time(11, 25, 5, 3, 10))
	nav := cex.Anchors{SessionID: "2026-11-24", Kind: cex.AnchorNAV, Bull: dec("40.10"), Bear: dec("59.90"), KnownAt: r.clock()}
	r.src.setAnchors(nav)
	r.books(pairBook{
		bullBids: side{lv("40.51", "20"), lv("40.50", "100")}, bullAsks: side{lv("40.53", "100"), lv("40.54", "100")},
		bearBids: side{lv("59.47", "10"), lv("59.465", "10"), lv("59.46", "20"), lv("59.455", "20"), lv("59.45", "20")},
		bearAsks: side{lv("59.48", "50"), lv("59.485", "50"), lv("59.49", "50"), lv("59.495", "50"), lv("59.50", "50")},
	})
	r.idleAt(utc8Time(11, 25, 5, 3, 10), strategy.ReasonSpacing)
	if kinds := r.eventKinds(); len(kinds) != 1 || kinds[0] != "anchor_changed" {
		t.Errorf("events %v, want one anchor_changed", kinds)
	}
	ev := r.sink.Last("event").(store.EventRow)
	if detail := decodeDetail(t, ev.Detail); detail["from"] != "bell" || detail["to"] != "nav" || detail["mid_jump_d"] == nil {
		t.Errorf("anchor_changed detail %v", detail)
	}
	if !r.m.stats.Ready {
		t.Error("the mean warms up again after the anchor change")
	}
	r.endMinute()

	// 05:04:10, past the spacing: example 1's book under the NAV gives x =
	// 0.04 / 40.10 x 10^4 = 9.975 >= 2 sigma = 7.98. Sized with the pairing
	// anchors 40 / 60: K = 70.2 x 60 = 4,212, sell 105.30 / 70.20 (under the
	// NAV it would be 104.86 / 70.20).
	r.at(utc8Time(11, 25, 5, 4, 10))
	r.books(entryBook())
	r.trade(utc8Time(11, 25, 5, 4, 10))
	d := r.decisions()[len(r.decisions())-1]
	detail := decodeDetail(t, d.Detail)
	if d.Kind != "entry" || detail["qty_bull"] != "105.3" || detail["qty_bear"] != "70.2" || detail["anchor_kind"] != "nav" || detail["pair_anchor_bull"] != "40" {
		t.Errorf("entry %s %v, want 105.30 / 70.20 on the pairing anchors 40 / 60 under the NAV", d.Kind, detail)
	}
	near(t, "x under the NAV", detail["x_d"].(float64), 10000*0.04/40.10)
	if c := r.inv.Cycle(); c == nil || len(c.Lots) != 2 || c.PairAnchors.Kind != cex.AnchorBell || !c.PairAnchors.Bull.Equal(dec("40")) {
		t.Errorf("cycle %+v, want two lots on the bell pairing anchors", c)
	}
}

// V13: with the mean warming up again after an invalid minute and F stale,
// the exit at the line still goes, on the last ready mean and the last F,
// and says so. A leg without any price holds both legs of the forced exit,
// which goes on when the price is back.
func TestDataGaps(t *testing.T) {
	r := newRig(t, utc8Time(11, 25, 5, 1, 0), rigOptions{})
	r.warmUp()
	r.at(utc8Time(11, 25, 5, 3, 5))
	r.books(entryBook())
	r.trade(utc8Time(11, 25, 5, 3, 5))
	r.endMinute() // 05:03 is book A: the last ready mean is {B, A}

	// F is not read after 05:03:59; at 05:05:10 it is 71 s old. 05:04 had
	// no observation: an invalid minute, the mean starts again. SOXS's asks
	// are half as deep: 75.00 / 50.00 of 105.30 / 70.20 are bought back.
	r.fxFixed = true
	t1 := utc8Time(11, 25, 5, 5, 10)
	r.at(t1)
	thin := exitBook()
	thin.bearAsks = side{lv("59.385", "50"), lv("59.391", "20"), lv("59.394", "15"), lv("59.396", "10"), lv("59.400", "5")}
	r.books(thin)
	r.trade(t1)
	if r.m.stats.Ready {
		t.Error("the mean is still ready after an invalid minute")
	}
	d := r.decisions()[len(r.decisions())-1]
	detail := decodeDetail(t, d.Detail)
	if d.Kind != "exit" || detail["stats_stale"] != true || detail["f_fresh"] != false || detail["f"] != "1" {
		t.Errorf("exit %s %v, want one on the old mean and the last F", d.Kind, detail)
	}
	near(t, "x on the old mean", detail["x_d"].(float64), 10000*(40.38/40+59.400/60-2)-10000*(40.40/40+59.42/60-2))
	bars := r.sink.Rows("minute_bar")
	if b := bars[len(bars)-1].(store.MinuteBarRow); b.Valid || b.MinuteMs != utc8Time(11, 25, 5, 4, 0).UnixMilli() {
		t.Errorf("last minute bar %+v, want 05:04 invalid", b)
	}

	// The close window; SOXS has had no frame for hours, SOXL a fresh one.
	r.fxFixed = false
	t2 := utc8Time(11, 26, 4, 59, 0)
	r.at(t2)
	b := exitBook()
	r.depth(cex.Bull, b.bullBids, b.bullAsks)
	r.idleAt(t2, strategy.ReasonNoPrice)
	r.idleAt(t2.Add(time.Second), strategy.ReasonNoPrice)
	r.at(t2.Add(2 * time.Second))
	r.depth(cex.Bear, b.bearBids, b.bearAsks)
	before := len(r.orders())
	r.trade(t2.Add(2 * time.Second))
	if got := len(r.orders()) - before; got != 2 {
		t.Errorf("%d forced orders once the price is back, want 2", got)
	}
}

// V11: an early-close day closes at 13:00 New York, so the forced exit
// begins at 01:59 UTC+8; a leg without a price past the close keeps the
// exit going into the next Session, which writes late_exit and has no
// anchor until the cycle is closed and its own anchor comes. The weekend
// Session holds positions over Saturday and Sunday, and in the regular
// hours of Monday exits at the line still go.
func TestSessionTimes(t *testing.T) {
	t.Run("early close and a late exit", func(t *testing.T) {
		// Wed 11-25 New York closes; Thu 11-26 is a holiday, Fri 11-27
		// closes at 13:00: the Session ends 11-28 02:00 UTC+8.
		r := newRig(t, utc8Time(11, 26, 5, 1, 0), rigOptions{})
		if s := r.cex.Session(r.clock()); s.ID != "2026-11-25" || !s.End.Equal(utc8Time(11, 28, 2, 0, 0)) {
			t.Fatalf("session %+v", s)
		}
		r.warmUp()
		r.at(utc8Time(11, 26, 5, 3, 5))
		r.books(entryBook())
		r.trade(utc8Time(11, 26, 5, 3, 5))

		r.at(utc8Time(11, 28, 1, 58, 59))
		r.books(calibration(1)) // B: no exit, no entry (regular hours)
		r.idleAt(utc8Time(11, 28, 1, 58, 59), strategy.ReasonRegularHours)
		if r.m.forced != "" {
			t.Fatalf("forced exit %q before 01:59", r.m.forced)
		}
		// 01:59: the forced exit begins; SOXS has no price until after the
		// close.
		r.at(utc8Time(11, 28, 1, 59, 30))
		b := exitBook()
		r.depth(cex.Bull, b.bullBids, b.bullAsks)
		r.idleAt(utc8Time(11, 28, 1, 59, 30), strategy.ReasonNoPrice)
		if r.m.forced != inventory.EndCloseWindow {
			t.Fatalf("forced cause %q, want close_window", r.m.forced)
		}
		// 02:00:30, the next Session (Fri 11-27's close): late_exit, the
		// exit goes on, and the cycle ends as close_window.
		t2 := utc8Time(11, 28, 2, 0, 30)
		r.at(t2)
		r.books(exitBook())
		r.trade(t2)
		if r.m.session.ID != "2026-11-27" {
			t.Errorf("session %s, want 2026-11-27", r.m.session.ID)
		}
		if kinds := r.eventKinds(); len(kinds) != 1 || kinds[0] != "late_exit" {
			t.Errorf("events %v, want late_exit", kinds)
		}
		r.at(t2.Add(time.Second))
		r.books(entryBook())
		r.stepAt(t2.Add(time.Second))
		if c := r.lastCycle(); c.Status != string(inventory.CycleClosed) || c.EndReason != inventory.EndCloseWindow || c.SessionID != "2026-11-25" {
			t.Errorf("cycle %s %s %s, want 2026-11-25 closed close_window", c.SessionID, c.Status, c.EndReason)
		}
		// The published anchors are still the ended Session's: no anchor.
		r.idleAt(t2.Add(2*time.Second), strategy.ReasonNoAnchor)
	})

	t.Run("weekend", func(t *testing.T) {
		// Fri 11-27 closes at 13:00 New York: the Session runs from 11-28
		// 02:00 to Mon 11-30's close, 12-01 05:00 UTC+8; its regular open is
		// 11-30 22:30.
		r := newRig(t, utc8Time(11, 28, 5, 1, 0), rigOptions{})
		if s := r.cex.Session(r.clock()); s.ID != "2026-11-27" || !s.RegularOpen.Equal(utc8Time(11, 30, 22, 30, 0)) {
			t.Fatalf("session %+v", s)
		}
		r.warmUp()
		r.at(utc8Time(11, 28, 5, 3, 5))
		r.books(entryBook())
		r.trade(utc8Time(11, 28, 5, 3, 5))
		for _, at := range []time.Time{utc8Time(11, 28, 12, 0, 0), utc8Time(11, 29, 12, 0, 0), utc8Time(11, 30, 12, 0, 0)} {
			r.at(at)
			r.books(calibration(1))
			r.step()
			if r.m.forced != "" || r.inv.Cycle() == nil {
				t.Fatalf("at %s the cycle is closed or forced (%q)", showTime(at), r.m.forced)
			}
		}
		// Monday 23:00, the regular hours: example 2's book is at the exit
		// line (on the last ready mean), and the exit goes.
		r.at(utc8Time(11, 30, 23, 0, 0))
		r.books(exitBook())
		r.trade(utc8Time(11, 30, 23, 0, 0))
		if d := r.decisions()[len(r.decisions())-1]; d.Kind != "exit" {
			t.Errorf("last decision %s, want an exit in the regular hours", d.Kind)
		}
	})
}
