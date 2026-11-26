package inventory

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/store"
	"etf-pair-arbitrage/core/strategy"
)

// The funding grid around the tests' clock: every eight hours from midnight
// UTC.
var (
	settle08 = time.Date(2026, 11, 24, 8, 0, 0, 0, time.UTC)
	settle16 = time.Date(2026, 11, 24, 16, 0, 0, 0, time.UTC)
	settle00 = time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
)

// anchors is the pair of anchors the tests open cycles with.
var anchors = cex.Anchors{SessionID: "2026-11-23", Kind: cex.AnchorBell, Bull: dec("40"), Bear: dec("60")}

// NewManager refuses a configuration that does not validate.
func TestNewManagerValidates(t *testing.T) {
	if _, err := NewManager(Config{Capital: dec("1000")}, testPair, newFakeCex()); err == nil {
		t.Error("NewManager without a leverage returned nil")
	}
}

// OpenCycle and OpenLot with a store that cannot confirm the write: nothing
// is opened in memory, the row that voids the same key is retried until a
// WriteSync confirms it, and nothing new is opened meanwhile.
func TestOpenWithFailingStore(t *testing.T) {
	r := newRig(t, "1000", "10")

	// The cycle row cannot be written.
	r.sink.Fail(errTest)
	if c, err := r.m.OpenCycle("2026-11-23", strategy.Long, anchors, r.clock.Now()); err == nil || !errors.Is(err, errTest) || c != nil {
		t.Fatalf("OpenCycle with a failing store = %v, %v; want the store's error and no cycle", c, err)
	}
	failedID := fmt.Sprintf("c%d", r.clock.Now().UnixMilli())
	if r.m.Cycle() != nil || !r.m.StoreBlocked() {
		t.Fatalf("after the failed open: cycle %v, blocked %t; want none, blocked", r.m.Cycle(), r.m.StoreBlocked())
	}
	r.m.step(r.clock.Add(200 * time.Millisecond))
	if _, err := r.m.OpenCycle("2026-11-23", strategy.Long, anchors, r.clock.Now()); !errors.Is(err, ErrStoreBlocked) {
		t.Errorf("OpenCycle while the void row is pending = %v, want ErrStoreBlocked", err)
	}
	if rows := r.sink.Rows("cycle"); len(rows) != 0 {
		t.Fatalf("cycle rows while the store fails: %d, want none", len(rows))
	}

	// The store is back: the first round of Run confirms the void row, and
	// only then may a cycle open.
	r.sink.Fail(nil)
	if !r.m.StoreBlocked() {
		t.Error("the block ended before the void row was written")
	}
	r.m.step(r.clock.Add(200 * time.Millisecond))
	if r.m.StoreBlocked() {
		t.Fatal("still blocked after the void row was written")
	}
	void := lastCycleRow(t, r.sink, CycleID(failedID))
	if void.Status != "closed" || void.EndReason != EndStoreError || void.ClosedMs == 0 || !void.FundingKnown {
		t.Errorf("void cycle row = %+v, want closed as store_error with funding known", void)
	}
	if last := r.sink.lastSync(); len(last) != 1 || last[0].Table() != "cycle" {
		t.Errorf("the void row was not written by its own WriteSync: %v", last)
	}
	c := r.openCycle(t, strategy.Long)
	if string(c.ID) == failedID {
		t.Fatalf("the new cycle reused the voided id %s", failedID)
	}

	// The lot row cannot be written.
	r.sink.Fail(errTest)
	if lot, err := r.m.OpenLot(&strategy.EntryPlan{Direction: strategy.Long}, r.clock.Now()); err == nil || lot != nil {
		t.Fatalf("OpenLot with a failing store = %v, %v; want an error and no lot", lot, err)
	}
	if got := r.m.Cycle(); got == nil || len(got.Lots) != 0 || !r.m.StoreBlocked() {
		t.Fatalf("after the failed lot: cycle %+v, blocked %t; want the cycle without a lot, blocked", got, r.m.StoreBlocked())
	}
	if _, err := r.m.OpenLot(&strategy.EntryPlan{Direction: strategy.Long}, r.clock.Now()); !errors.Is(err, ErrStoreBlocked) {
		t.Errorf("OpenLot while the void row is pending = %v, want ErrStoreBlocked", err)
	}
	r.m.step(r.clock.Add(200 * time.Millisecond)) // the store still fails: nothing is confirmed
	if !r.m.StoreBlocked() {
		t.Error("the block ended while the store was still failing")
	}
	r.sink.Fail(nil)
	r.m.step(r.clock.Add(200 * time.Millisecond))
	if r.m.StoreBlocked() {
		t.Fatal("still blocked after the lot's void row was written")
	}
	lotID := LotID(string(c.ID) + "-1")
	if row := lastLotRow(t, r.sink, lotID); row.Status != "void" || row.VoidReason != VoidStoreError || row.ClosedMs == 0 {
		t.Errorf("void lot row = %+v, want void as store_error", row)
	}

	// The next lot takes the same number: the voided one never existed.
	lot := r.openLot(t)
	if lot.ID != lotID || lot.Seq != 1 || lot.Status != LotBuilding {
		t.Errorf("lot after the void = %+v, want %s building", lot, lotID)
	}
	if row := lastLotRow(t, r.sink, lotID); row.Status != "building" || row.VoidReason != "" {
		t.Errorf("lot row after the reopen = status %s, reason %q; want building", row.Status, row.VoidReason)
	}
}

// A funding record (PRD 7.6) is booked once, to the cycle that held through
// its settlement: a funding row, r_closed and the cycle's row in one
// transaction. The settlement is the record's time floored to the grid.
// Records already booked are recognised after a restart.
func TestFundingBookedOnceToTheHoldingCycle(t *testing.T) {
	r := newRig(t, "1000", "10")
	c := r.openCycle(t, strategy.Long) // 01:00 UTC
	lot := r.openLot(t)
	orders := []cex.OrderRecord{
		fill(ord(c.ID, 1, cex.Bull, cex.PurposeEntry, cex.SideBuy, lot.ID, "40", "100"), "100", "4000", "0"),
		fill(ord(c.ID, 2, cex.Bear, cex.PurposeEntry, cex.SideBuy, lot.ID, "60", "50"), "50", "3000", "0"),
	}
	r.m.Sync(orders)
	r.m.SettleLot(lot.ID, true, r.clock.Now())

	// 08:00:05 UTC: the bull leg's record of the 08:00 settlement arrives.
	r.clock.Set(settle08.Add(5 * time.Second))
	r.cex.addRecord("f1", cex.Bull, "-0.12", settle08.Add(3*time.Second))
	r.m.step(r.clock.Now())
	funding := rowsOf[store.FundingRow](r.sink)
	if len(funding) != 1 || funding[0].RecordID != "f1" || funding[0].Symbol != "SOXLUSDT" || !funding[0].Amount.Equal(dec("-0.12")) ||
		funding[0].SettlementMs != settle08.UnixMilli() || funding[0].TsMs != settle08.Add(3*time.Second).UnixMilli() || funding[0].CycleID != string(c.ID) {
		t.Fatalf("funding rows = %+v, want f1 of the 08:00 settlement on cycle %s", funding, c.ID)
	}
	var tables []string
	for _, row := range r.sink.lastSync() {
		tables = append(tables, row.Table())
	}
	if !slices.Equal(tables, []string{"funding", "cycle", "inventory_state"}) {
		t.Errorf("the record was written as %v, want funding, cycle and inventory_state in one WriteSync", tables)
	}
	wantDec(t, "r_closed", rClosed(t, r.sink), "-0.12")
	wantQuota(t, r, "after funding paid", "9998.8", "7000")
	// The bear leg was held through that settlement too; its rate is not
	// published and it has no record: the cycle's funding is not known yet.
	if got := r.m.Cycle(); !got.Funding.Equal(dec("-0.12")) || got.FundingKnown || !got.Net.Equal(dec("-0.12")) {
		t.Errorf("cycle = funding %s known %t net %s; want -0.12, not known, -0.12", got.Funding, got.FundingKnown, got.Net)
	}
	if row := lastCycleRow(t, r.sink, c.ID); !row.Funding.Equal(dec("-0.12")) || row.FundingKnown {
		t.Errorf("cycle row = funding %s known %t; want -0.12, not known", row.Funding, row.FundingKnown)
	}

	// The bear rate is published as zero: nothing more is due.
	r.cex.setRate(cex.Bear, settle08, "0")
	r.m.Sync(orders)
	if got := r.m.Cycle(); !got.FundingKnown {
		t.Error("cycle funding still unknown with the bull record booked and the bear rate zero")
	}
	if row := lastCycleRow(t, r.sink, c.ID); !row.FundingKnown || !row.Funding.Equal(dec("-0.12")) {
		t.Errorf("cycle row = funding %s known %t; want -0.12, known", row.Funding, row.FundingKnown)
	}

	// Another round, and a restart: the record is not booked again.
	r.m.step(r.clock.Add(time.Second))
	r = r.restart(t, "1000", "10", orders...)
	r.m.step(r.clock.Add(time.Second))
	if n := len(rowsOf[store.FundingRow](r.sink)); n != 1 {
		t.Errorf("funding rows after another round and a restart = %d, want 1", n)
	}
	wantDec(t, "r_closed after the restart", rClosed(t, r.sink), "-0.12")
	if got := r.m.Cycle(); !got.Funding.Equal(dec("-0.12")) || !got.FundingKnown {
		t.Errorf("restored cycle = funding %s known %t; want -0.12, known", got.Funding, got.FundingKnown)
	}

	// The cycle closes at 09:00; its net holds the funding, and R does not
	// count it twice.
	r.clock.Set(settle08.Add(time.Hour))
	r.m.Sync(append(orders[:2:2],
		fill(ord(c.ID, 3, cex.Bull, cex.PurposeExit, cex.SideSell, "", "40", "100"), "100", "4010", "0"),
		fill(ord(c.ID, 4, cex.Bear, cex.PurposeExit, cex.SideSell, "", "60", "50"), "50", "3000", "0")))
	r.closeCycle(t, EndExitLine)
	row := lastCycleRow(t, r.sink, c.ID)
	wantDec(t, "closed cycle net", row.Net, "9.88")
	wantDec(t, "r_closed after the close", rClosed(t, r.sink), "9.88")
	if !row.FundingKnown {
		t.Error("closed cycle row has funding unknown")
	}

	// A record of a settlement no cycle held through is booked to R with no
	// cycle.
	r.clock.Set(settle16.Add(10 * time.Second))
	r.cex.addRecord("f2", cex.Bear, "0.05", settle16.Add(4*time.Second))
	r.m.step(r.clock.Now())
	funding = rowsOf[store.FundingRow](r.sink)
	if len(funding) != 2 || funding[1].RecordID != "f2" || funding[1].CycleID != "" || funding[1].SettlementMs != settle16.UnixMilli() {
		t.Errorf("funding rows = %+v, want f2 of the 16:00 settlement without a cycle", funding)
	}
	wantDec(t, "r_closed with f2", rClosed(t, r.sink), "9.93")
}

// heldCycle plays one long cycle that holds 100 / 50 from the rig's clock
// until closeAt, trades flat and closes. The orders carry the venue's times,
// which say what was held at a settlement.
func heldCycle(t *testing.T, r *testRig, closeAt time.Time) CycleID {
	t.Helper()
	opened := r.clock.Now()
	c := r.openCycle(t, strategy.Long)
	lot := r.openLot(t)
	orders := []cex.OrderRecord{
		timed(fill(ord(c.ID, 1, cex.Bull, cex.PurposeEntry, cex.SideBuy, lot.ID, "40", "100"), "100", "4000", "0"), opened),
		timed(fill(ord(c.ID, 2, cex.Bear, cex.PurposeEntry, cex.SideBuy, lot.ID, "60", "50"), "50", "3000", "0"), opened),
	}
	r.m.Sync(orders)
	r.m.SettleLot(lot.ID, true, opened)
	r.clock.Set(closeAt)
	r.m.Sync(append(orders,
		timed(fill(ord(c.ID, 3, cex.Bull, cex.PurposeExit, cex.SideSell, "", "40", "100"), "100", "4000", "0"), closeAt),
		timed(fill(ord(c.ID, 4, cex.Bear, cex.PurposeExit, cex.SideSell, "", "60", "50"), "50", "3000", "0"), closeAt)))
	r.closeCycle(t, EndExitLine)
	return c.ID
}

// A cycle that held through a settlement whose rate is zero for both legs
// has funding of zero, known. With a rate that is not zero and no record
// yet its funding is unknown: the cycle closes as pending and is completed
// when the record arrives, also after a restart, or when the rates turn out
// to be zero.
func TestFundingKnownAndPending(t *testing.T) {
	r := newRig(t, "1000", "10")

	// Cycle 1, 01:00 to 08:00:10: both rates of 08:00 are zero.
	r.cex.setRate(cex.Bull, settle08, "0")
	r.cex.setRate(cex.Bear, settle08, "0")
	c1 := heldCycle(t, r, settle08.Add(10*time.Second))
	if row := lastCycleRow(t, r.sink, c1); !row.FundingKnown || !row.Funding.IsZero() || len(row.FundingAwaited) != 0 {
		t.Errorf("cycle 1 row = funding %s known %t awaited %s; want 0, known, none", row.Funding, row.FundingKnown, row.FundingAwaited)
	}

	// Cycle 2, 09:00 to 16:00:02: the bull rate of 16:00 is not zero and no
	// record has arrived when it closes.
	r.clock.Set(settle08.Add(time.Hour))
	r.cex.setRate(cex.Bull, settle16, "0.0001")
	r.cex.setRate(cex.Bear, settle16, "0")
	c2 := heldCycle(t, r, settle16.Add(2*time.Second))
	row := lastCycleRow(t, r.sink, c2)
	if row.FundingKnown || row.Status != "closed" {
		t.Errorf("cycle 2 row = status %s, funding known %t; want closed with funding unknown", row.Status, row.FundingKnown)
	}
	if want := fmt.Sprintf(`[{"settlement_ms":%d,"leg":"bull"}]`, settle16.UnixMilli()); string(row.FundingAwaited) != want {
		t.Errorf("cycle 2 funding_awaited = %s, want %s", row.FundingAwaited, want)
	}
	r.m.step(r.clock.Add(time.Second))
	if row := lastCycleRow(t, r.sink, c2); row.FundingKnown {
		t.Error("cycle 2 funding became known without a record")
	}

	// The process restarts; then the record arrives and completes cycle 2.
	r = r.restart(t, "1000", "10")
	r.cex.addRecord("f1", cex.Bull, "-0.3", settle16.Add(5*time.Second))
	r.m.step(r.clock.Add(10 * time.Second))
	row = lastCycleRow(t, r.sink, c2)
	if !row.FundingKnown || !row.Funding.Equal(dec("-0.3")) || !row.Net.Equal(dec("-0.3")) || row.Status != "closed" || row.EndReason != EndExitLine || len(row.FundingAwaited) != 0 {
		t.Errorf("cycle 2 row after the record = %+v, want funding -0.3 known, net -0.3, nothing awaited", row)
	}
	if f := rowsOf[store.FundingRow](r.sink); len(f) != 1 || f[0].CycleID != string(c2) {
		t.Errorf("funding rows = %+v, want f1 on cycle %s", f, c2)
	}
	wantDec(t, "r_closed", rClosed(t, r.sink), "-0.3")
	cycleRows := len(r.sink.Rows("cycle"))
	r.m.step(r.clock.Add(time.Second))
	if got := len(r.sink.Rows("cycle")); got != cycleRows {
		t.Errorf("a completed cycle was written %d more times", got-cycleRows)
	}

	// Cycle 3, 17:00 to 00:00:02: the rates of 00:00 are not published when
	// it closes, and turn out to be zero.
	r.clock.Set(settle16.Add(time.Hour))
	c3 := heldCycle(t, r, settle00.Add(2*time.Second))
	if row := lastCycleRow(t, r.sink, c3); row.FundingKnown {
		t.Error("cycle 3 funding known before the rates were published")
	}
	r.cex.setRate(cex.Bull, settle00, "0")
	r.m.step(r.clock.Add(time.Second))
	if row := lastCycleRow(t, r.sink, c3); row.FundingKnown {
		t.Error("cycle 3 funding known with one rate still unpublished")
	}
	r.cex.setRate(cex.Bear, settle00, "0")
	r.m.step(r.clock.Add(time.Second))
	if row := lastCycleRow(t, r.sink, c3); !row.FundingKnown || !row.Funding.IsZero() || row.Status != "closed" {
		t.Errorf("cycle 3 row = %+v, want funding 0 known", row)
	}
	wantDec(t, "r_closed, unchanged", rClosed(t, r.sink), "-0.3")
}

// Funding is awaited only for a leg that held a position at the settlement:
// the venue books no record for a leg that held nothing. A short cycle
// opens at 07:59:58; only its bull leg fills, and the trim is done at
// 08:00:04, so the cycle held the bull leg alone through 08:00. Both rates
// of 08:00 are 0.0001. With the bull leg's record its funding is known,
// whether the record arrives while it is open or after it closed as
// one_leg, also across a restart.
func TestFundingOnlyForLegsHeld(t *testing.T) {
	for _, recordAfterClose := range []bool{false, true} {
		r := newRig(t, "1000", "10")
		r.cex.setRate(cex.Bull, settle08, "0.0001")
		r.cex.setRate(cex.Bear, settle08, "0.0001")
		r.clock.Set(settle08.Add(-2 * time.Second))
		c := r.openCycle(t, strategy.Short)
		lot := r.openLot(t)
		r.m.Sync([]cex.OrderRecord{
			timed(fill(ord(c.ID, 1, cex.Bull, cex.PurposeEntry, cex.SideSell, lot.ID, "40.44", "105.3"), "105.3", "4258.332", "0"), settle08.Add(-time.Second)),
			timed(fill(ord(c.ID, 2, cex.Bear, cex.PurposeEntry, cex.SideSell, lot.ID, "59.42", "70.2"), "0", "0", "0"), settle08.Add(-time.Second)),
			timed(fill(ord(c.ID, 3, cex.Bull, cex.PurposeTrim, cex.SideBuy, lot.ID, "40.47", "105.3"), "105.3", "4261.491", "0"), settle08.Add(4*time.Second)),
		})
		r.m.SettleLot(lot.ID, false, r.clock.Set(settle08.Add(4*time.Second)))
		if got := r.m.Cycle(); got.FundingKnown {
			t.Fatalf("record after close %t: the open cycle's funding is known before the bull record", recordAfterClose)
		}

		if !recordAfterClose {
			r.cex.addRecord("f1", cex.Bull, "0.42", settle08.Add(3*time.Second))
			r.m.step(r.clock.Now())
			if got := r.m.Cycle(); !got.FundingKnown || !got.Funding.Equal(dec("0.42")) {
				t.Errorf("open cycle after the bull record = funding %s known %t; want 0.42, known: the bear leg held nothing", got.Funding, got.FundingKnown)
			}
		}
		r.clock.Set(settle08.Add(5 * time.Second))
		r.closeCycle(t, EndOneLeg)
		row := lastCycleRow(t, r.sink, c.ID)
		if recordAfterClose {
			if want := fmt.Sprintf(`[{"settlement_ms":%d,"leg":"bull"}]`, settle08.UnixMilli()); row.FundingKnown || string(row.FundingAwaited) != want {
				t.Errorf("closed before the record: funding known %t, awaited %s; want unknown, %s", row.FundingKnown, row.FundingAwaited, want)
			}
			r = r.restart(t, "1000", "10")
			r.cex.addRecord("f1", cex.Bull, "0.42", settle08.Add(3*time.Second))
			r.m.step(r.clock.Add(15 * time.Second))
			row = lastCycleRow(t, r.sink, c.ID)
		}
		// 4,258.332 sold, 4,261.491 bought back: -3.159, plus 0.42 received.
		if !row.FundingKnown || !row.Funding.Equal(dec("0.42")) || !row.Net.Equal(dec("-2.739")) || len(row.FundingAwaited) != 0 || row.EndReason != EndOneLeg {
			t.Errorf("record after close %t: cycle row = %+v, want funding 0.42 known, net -2.739, nothing awaited", recordAfterClose, row)
		}
		wantDec(t, "r_closed", rClosed(t, r.sink), "-2.739")

		// Five hours later nothing is waiting and nothing more is written.
		rows := len(r.sink.Rows("cycle"))
		r.m.step(r.clock.Add(5 * time.Hour))
		if got := len(r.sink.Rows("cycle")); got != rows || lastCycleRow(t, r.sink, c.ID).FundingKnown != true {
			t.Errorf("record after close %t: %d more cycle rows five hours later, want none", recordAfterClose, got-rows)
		}
		if r = r.restart(t, "1000", "10"); len(stateOf(t, r.sink).PendingFunding) != 0 {
			t.Errorf("record after close %t: a cycle is still awaiting funding after a restart", recordAfterClose)
		}
	}
}

// A new ledger does not take the account's older funding into R (PRD 6.3: R
// accumulates from the moment this capital takes effect): records settled
// before the ledger started are neither booked nor written. The start is
// kept in inventory_state and moves when the capital changes.
func TestLedgerStartBoundsFunding(t *testing.T) {
	r := newRig(t, "1000", "10")
	day := 24 * time.Hour
	for i, at := range []time.Time{settle08.Add(-2 * day), settle16.Add(-2 * day), settle00.Add(-2 * day), settle08.Add(-day), settle16.Add(-day), settle00.Add(-day)} {
		r.cex.addRecord(fmt.Sprintf("old-%d", i), cex.Leg(i%2), "-1.5", at.Add(3*time.Second))
	}
	r = r.restart(t, "1000", "10") // nothing stored: the ledger starts at 01:00
	r.m.step(r.clock.Add(time.Second))
	if n := len(r.sink.Rows("funding")); n != 0 {
		t.Errorf("funding rows of a new ledger = %d, want 0: all six records are older than it", n)
	}
	wantQuota(t, r, "new ledger with six old records", "10000", "0")
	state := lastOf(t, r.sink, func(store.InventoryStateRow) bool { return true })
	if state.SinceMs != t0.UnixMilli() || !state.RClosed.IsZero() || !state.Capital.Equal(dec("1000")) {
		t.Errorf("inventory_state of a new ledger = %+v, want since %d, r_closed 0", state, t0.UnixMilli())
	}

	// A record settled after the start is booked.
	r.cex.addRecord("new-1", cex.Bull, "-0.5", settle08.Add(3*time.Second))
	r.m.step(r.clock.Set(settle08.Add(10 * time.Second)))
	wantQuota(t, r, "one record of this ledger", "9995", "0")

	// A restart keeps the start; the old records stay out.
	r = r.restart(t, "1000", "10")
	r.m.step(r.clock.Add(time.Second))
	if f := rowsOf[store.FundingRow](r.sink); len(f) != 1 || f[0].RecordID != "new-1" {
		t.Errorf("funding rows after a restart = %+v, want new-1 alone", f)
	}
	wantQuota(t, r, "after a restart", "9995", "0")

	// The capital changes at 09:00: the ledger starts again there, and a
	// record of the 08:00 settlement read only now belongs to the old one.
	r.clock.Set(settle08.Add(time.Hour))
	r = r.restart(t, "2000", "10")
	r.cex.addRecord("late-08", cex.Bear, "-0.7", settle08.Add(4*time.Second))
	r.cex.addRecord("at-16", cex.Bear, "-0.2", settle16.Add(4*time.Second))
	r.m.step(r.clock.Set(settle16.Add(10 * time.Second)))
	if f := rowsOf[store.FundingRow](r.sink); len(f) != 2 || f[1].RecordID != "at-16" {
		t.Errorf("funding rows after the capital change = %+v, want new-1 and at-16", f)
	}
	wantQuota(t, r, "new capital, one record since", "19998", "0")
	if state = lastOf(t, r.sink, func(store.InventoryStateRow) bool { return true }); state.SinceMs != settle08.Add(time.Hour).UnixMilli() {
		t.Errorf("since after the capital change = %d, want 09:00 (%d)", state.SinceMs, settle08.Add(time.Hour).UnixMilli())
	}
}

// A funding record of a cycle that finished under the old capital arrives
// after the capital changed. Which cycle it belongs to and whether it enters
// the new R are separate questions: the old cycle gets its -0.12 and stops
// waiting, the funding row is written, and the new ledger's R stays zero.
func TestFundingOfAnOldCycleAfterCapitalChange(t *testing.T) {
	r := newRig(t, "1000", "10")
	r.cex.setRate(cex.Bull, settle08, "0.0001")
	r.cex.setRate(cex.Bear, settle08, "0")
	old := heldCycle(t, r, settle08.Add(2*time.Second))
	if row := lastCycleRow(t, r.sink, old); row.FundingKnown {
		t.Fatal("the old cycle is not waiting for funding")
	}

	r.clock.Set(settle08.Add(time.Hour))
	r = r.restart(t, "2000", "10") // the ledger starts again at 09:00
	r.cex.addRecord("f1", cex.Bull, "-0.12", settle08.Add(3*time.Second))
	r.cex.addRecord("f0", cex.Bear, "-0.5", settle00.Add(-24*time.Hour).Add(3*time.Second)) // older, of no cycle
	r.m.step(r.clock.Add(time.Second))

	row := lastCycleRow(t, r.sink, old)
	if !row.FundingKnown || !row.Funding.Equal(dec("-0.12")) || !row.Net.Equal(dec("-0.12")) || len(row.FundingAwaited) != 0 {
		t.Errorf("old cycle row = funding %s known %t net %s awaited %s; want -0.12, known, -0.12, none", row.Funding, row.FundingKnown, row.Net, row.FundingAwaited)
	}
	if f := rowsOf[store.FundingRow](r.sink); len(f) != 1 || f[0].RecordID != "f1" || f[0].CycleID != string(old) {
		t.Errorf("funding rows = %+v, want f1 on cycle %s and nothing of f0", f, old)
	}
	wantDec(t, "r_closed of the new ledger", rClosed(t, r.sink), "0")
	wantQuota(t, r, "new ledger", "20000", "0")
	r.m.step(r.clock.Add(time.Second))
	if n := len(r.sink.Rows("funding")); n != 1 {
		t.Errorf("funding rows after another round = %d, want 1", n)
	}
}

// All new funding records of one round are written in one WriteSync, with
// r_closed at its final value, so a reader waits for one write and not for
// one per record. Six records, every write taking 60 ms: Holdings, called
// while the write is under way, returns well before the 360 ms that six
// writes would take.
func TestFundingBookedInOneWrite(t *testing.T) {
	r := newRig(t, "1000", "10")
	r.clock.Set(settle08.Add(10 * time.Second))
	for i := range 6 {
		r.cex.addRecord(fmt.Sprintf("f%d", i), cex.Leg(i%2), "-0.5", settle08.Add(3*time.Second))
	}
	const delay = 60 * time.Millisecond
	r.sink.mu.Lock()
	r.sink.delay, r.sink.entered = delay, make(chan struct{}, 16)
	r.sink.mu.Unlock()

	done := make(chan struct{})
	go func() { defer close(done); r.m.bookFunding(r.clock.Now()) }()
	<-r.sink.entered // the write has begun and the ledger's lock is held
	start := time.Now()
	r.m.Holdings()
	waited := time.Since(start)
	<-done
	if waited >= 3*delay {
		t.Errorf("Holdings waited %s for the funding writes; one write takes %s", waited, delay)
	}
	if n := r.sink.syncCount(); n != 1 {
		t.Fatalf("the six records took %d WriteSync calls, want 1", n)
	}
	tables := map[string]int{}
	for _, row := range r.sink.lastSync() {
		tables[row.Table()]++
	}
	if tables["funding"] != 6 || tables["inventory_state"] != 1 || len(tables) != 2 {
		t.Errorf("the one write held %v, want six funding rows and inventory_state", tables)
	}
	wantDec(t, "r_closed", rClosed(t, r.sink), "-3")
	r.m.bookFunding(r.clock.Now())
	if n := r.sink.syncCount(); n != 1 {
		t.Errorf("a round without new records wrote again: %d WriteSync calls", n)
	}
}

// While the database cannot confirm a write that is tried every round, the
// failure is logged when it starts, once a minute while it lasts and when
// it ends, not every round.
func TestFailingWritesAreNotLoggedEveryRound(t *testing.T) {
	logs := captureLogs(t)
	r := newRig(t, "1000", "10")
	r.openCycle(t, strategy.Long)
	const closeFailed, closeBack = "cycle close not confirmed written", "cycle close written after failing"

	r.sink.Fail(errTest)
	for range 10 { // two seconds of the loop's 200 ms rounds
		if err := r.m.CloseCycle(EndNoFill, r.clock.Add(200*time.Millisecond)); err == nil {
			t.Fatal("CloseCycle with a failing store returned nil")
		}
	}
	if n := len(logs.lines(closeFailed)); n != 1 {
		t.Errorf("ten failed closes in two seconds logged %d warnings, want 1", n)
	}
	r.clock.Add(time.Minute)
	_ = r.m.CloseCycle(EndNoFill, r.clock.Now())
	_ = r.m.CloseCycle(EndNoFill, r.clock.Add(200*time.Millisecond))
	if n := len(logs.lines(closeFailed)); n != 2 {
		t.Errorf("after another minute: %d warnings, want 2", n)
	}
	r.sink.Fail(nil)
	r.closeCycle(t, EndNoFill)
	if n := len(logs.lines(closeBack, "failing_for=1m2")); n != 1 {
		t.Errorf("the recovery was logged %d times with its duration, want once: %v", n, logs.lines(closeBack))
	}

	// The same for the rows that void a cycle: one warning when the open
	// fails, none for the retries of the next minute, one line when the
	// rows are written.
	r.clock.Add(time.Minute)
	r.sink.Fail(errTest)
	if _, err := r.m.OpenCycle("2026-11-23", strategy.Long, anchors, r.clock.Now()); err == nil {
		t.Fatal("OpenCycle with a failing store returned nil")
	}
	for range 10 {
		r.m.retryVoids(r.clock.Add(200 * time.Millisecond))
	}
	if a, b := len(logs.lines("cycle row not confirmed written")), len(logs.lines("void rows still not confirmed")); a != 1 || b != 0 {
		t.Errorf("failed open and ten retries logged %d and %d warnings, want 1 and 0", a, b)
	}
	r.m.retryVoids(r.clock.Add(time.Minute))
	if n := len(logs.lines("void rows still not confirmed")); n != 1 {
		t.Errorf("after a minute of retries: %d warnings, want 1", n)
	}
	r.sink.Fail(nil)
	r.m.retryVoids(r.clock.Add(200 * time.Millisecond))
	if n := len(logs.lines("void rows confirmed written", "failing_for=1m2")); n != 1 || r.m.StoreBlocked() {
		t.Errorf("the void rows' recovery was logged %d times, blocked %t; want once, not blocked", n, r.m.StoreBlocked())
	}
}

// A WriteSync error can mean "written, the answer came too late". OpenCycle
// and OpenLot then leave a row in the database for something that does not
// exist in memory: the void row, once confirmed, must be the final state of
// that key. A close that was written despite its error is repeated and
// counts once.
func TestWrittenDespiteTheError(t *testing.T) {
	r := newRig(t, "1000", "10")

	// The cycle's open row is in the database; the process has no cycle.
	r.sink.FailAfterWrite(errTest)
	if c, err := r.m.OpenCycle("2026-11-23", strategy.Long, anchors, r.clock.Now()); err == nil || c != nil || r.m.Cycle() != nil {
		t.Fatalf("OpenCycle = %v, %v; want an error and no cycle", c, err)
	}
	ghost := CycleID(fmt.Sprintf("c%d", r.clock.Now().UnixMilli()))
	if row := lastCycleRow(t, r.sink, ghost); row.Status != "open" {
		t.Fatalf("the sink did not keep the open row: %+v", row)
	}
	r.sink.Fail(nil)
	r.m.step(r.clock.Add(200 * time.Millisecond))
	if row := lastCycleRow(t, r.sink, ghost); row.Status != "closed" || row.EndReason != EndStoreError || r.m.StoreBlocked() {
		t.Errorf("final row of the cycle = status %s, reason %s, blocked %t; want closed as store_error", row.Status, row.EndReason, r.m.StoreBlocked())
	}
	if st := stateOf(t, r.sink); st.Cycle != nil {
		t.Errorf("a restart would read back cycle %s, which never existed", st.Cycle.CycleID)
	}

	// The same for a lot.
	c := r.openCycle(t, strategy.Long)
	r.sink.FailAfterWrite(errTest)
	if lot, err := r.m.OpenLot(&strategy.EntryPlan{Direction: strategy.Long}, r.clock.Now()); err == nil || lot != nil || len(r.m.Cycle().Lots) != 0 {
		t.Fatalf("OpenLot = %v, %v; want an error and no lot", lot, err)
	}
	lotID := LotID(string(c.ID) + "-1")
	if row := lastLotRow(t, r.sink, lotID); row.Status != "building" {
		t.Fatalf("the sink did not keep the building row: %+v", row)
	}
	r.sink.Fail(nil)
	r.m.step(r.clock.Add(200 * time.Millisecond))
	if row := lastLotRow(t, r.sink, lotID); row.Status != "void" || row.VoidReason != VoidStoreError {
		t.Errorf("final row of the lot = status %s, reason %s; want void as store_error", row.Status, row.VoidReason)
	}
	if st := stateOf(t, r.sink); len(st.Lots) != 1 || st.Lots[0].Status != "void" {
		t.Errorf("a restart would read back lots %+v, want the one void lot", st.Lots)
	}

	// A close written despite its error: the cycle stays open, the close is
	// repeated, and its net is in r_closed once.
	lot := r.openLot(t)
	r.m.Sync([]cex.OrderRecord{
		fill(ord(c.ID, 1, cex.Bull, cex.PurposeEntry, cex.SideBuy, lot.ID, "40", "100"), "100", "4000", "0"),
		fill(ord(c.ID, 2, cex.Bear, cex.PurposeEntry, cex.SideBuy, lot.ID, "60", "50"), "50", "3000", "0"),
		fill(ord(c.ID, 3, cex.Bull, cex.PurposeExit, cex.SideSell, "", "40.1", "100"), "100", "4010", "0"),
		fill(ord(c.ID, 4, cex.Bear, cex.PurposeExit, cex.SideSell, "", "60", "50"), "50", "3000", "0"),
	})
	r.sink.FailAfterWrite(errTest)
	if err := r.m.CloseCycle(EndExitLine, r.clock.Now()); err == nil || r.m.Cycle() == nil {
		t.Fatalf("CloseCycle = %v, cycle %v; want an error and the cycle still open", err, r.m.Cycle())
	}
	wantDec(t, "r_closed the first close wrote", rClosed(t, r.sink), "10")
	r.sink.Fail(nil)
	r.closeCycle(t, EndExitLine)
	wantDec(t, "r_closed after the repeated close", rClosed(t, r.sink), "10")
	wantQuota(t, r, "after the repeated close", "10000", "0")
	if row := lastCycleRow(t, r.sink, c.ID); row.Status != "closed" || !row.Net.Equal(dec("10")) {
		t.Errorf("cycle row = status %s, net %s; want closed, 10", row.Status, row.Net)
	}
}

// A funding record that cannot be written stays unbooked and is booked when
// the store is back, once.
func TestFundingWithFailingStore(t *testing.T) {
	r := newRig(t, "1000", "10")
	r.clock.Set(settle08.Add(5 * time.Second))
	r.cex.addRecord("f1", cex.Bull, "0.2", settle08.Add(3*time.Second))
	r.sink.Fail(errTest)
	r.m.step(r.clock.Now())
	r.m.bookFunding(r.clock.Now()) // also when Run did not hold back
	if n := len(r.sink.Rows("funding")); n != 0 {
		t.Fatalf("funding rows while the store fails = %d, want 0", n)
	}
	wantQuota(t, r, "record not booked", "10000", "0")
	r.sink.Fail(nil)
	r.m.step(r.clock.Add(time.Second))
	r.m.step(r.clock.Add(time.Second))
	if n := len(r.sink.Rows("funding")); n != 1 {
		t.Errorf("funding rows after the store is back = %d, want 1", n)
	}
	wantDec(t, "r_closed", rClosed(t, r.sink), "0.2")
}

// MarkTakeover leaves the cycle in the ledger with status takeover, and a
// restart reads it back with its lots.
func TestTakeover(t *testing.T) {
	r := newRig(t, "1000", "10")
	c := r.openCycle(t, strategy.Long)
	lot := r.openLot(t)
	orders := []cex.OrderRecord{
		fill(ord(c.ID, 1, cex.Bull, cex.PurposeEntry, cex.SideBuy, lot.ID, "40", "100"), "100", "4000", "0.26"),
		fill(ord(c.ID, 2, cex.Bear, cex.PurposeEntry, cex.SideBuy, lot.ID, "60", "50"), "50", "3000", "0.195"),
	}
	r.m.Sync(orders)
	r.m.SettleLot(lot.ID, true, r.clock.Now())
	r.m.MarkTakeover(r.clock.Add(time.Minute))
	if got := r.m.Cycle(); got == nil || got.Status != CycleTakeover {
		t.Fatalf("cycle after the takeover = %+v, want status takeover", got)
	}
	if row := lastCycleRow(t, r.sink, c.ID); row.Status != "takeover" || row.ClosedMs != 0 {
		t.Errorf("cycle row = status %s, closed_ms %d; want takeover, not closed", row.Status, row.ClosedMs)
	}

	// Restored without order records: the lots keep their stored amounts
	// until the first snapshot recomputes them, to the same numbers.
	r = r.restart(t, "1000", "10")
	dir, hold := r.m.Holdings()
	if got := r.m.Cycle(); got == nil || got.Status != CycleTakeover || got.ID != c.ID || len(got.Lots) != 1 || got.Lots[0].Status != LotOpen ||
		dir != strategy.Long || !hold[cex.Bull].Equal(dec("100")) || !hold[cex.Bear].Equal(dec("50")) || !got.PairAnchors.Bear.Equal(dec("60")) {
		t.Fatalf("restored: cycle %+v, holdings %s %v; want the takeover cycle holding 100 / 50", got, dir, hold)
	}
	wantQuota(t, r, "restored", "10000", "7000")
	lotRows := len(r.sink.Rows("lot"))
	r.m.Sync(orders)
	if got := len(r.sink.Rows("lot")); got != lotRows {
		t.Errorf("recomputing the restored lot wrote %d rows, want none: the amounts are the same", got-lotRows)
	}
	wantLeg(t, "restored lot bull", r.lot(t, 1).Legs[cex.Bull], "100", "4000", "0.26", "0", "0", "0")
}

// Run books funding and samples on its own, while the ledger methods are
// called from another goroutine.
func TestRun(t *testing.T) {
	r := newRig(t, "1000", "10")
	r.m.timing.tick, r.m.timing.sampleEvery, r.m.timing.statusLog = 2*time.Millisecond, 5*time.Millisecond, 5*time.Millisecond
	r.m.timing.clock = time.Now
	c, err := r.m.OpenCycle("2026-11-23", strategy.Long, anchors, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	lot, err := r.m.OpenLot(&strategy.EntryPlan{Direction: strategy.Long}, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	r.cex.setPositions("1", "0") // what the loop below books
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Go(func() { r.m.Run(ctx) })
	wg.Go(func() { // exec's loop
		for i := uint64(1); ctx.Err() == nil; i++ {
			r.m.Sync([]cex.OrderRecord{at(working(ord(c.ID, 1, cex.Bull, cex.PurposeEntry, cex.SideBuy, lot.ID, "40", "100"), "1", "40", "0"), i)})
			r.m.Quota()
			r.m.Holdings()
			r.m.Cycle()
			_ = r.m.Reconcile(ctx)
			time.Sleep(time.Millisecond)
		}
	})
	recordAt := time.Now()
	r.cex.addRecord("f1", cex.Bull, "-0.1", recordAt)
	deadline := time.Now().Add(5 * time.Second)
	for len(r.sink.Rows("funding")) == 0 || len(r.sink.Rows("inventory_sample")) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("Run wrote %d funding rows and %d samples in 5 s", len(r.sink.Rows("funding")), len(r.sink.Rows("inventory_sample")))
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	wg.Wait()

	// The record was booked once, to the cycle that had opened before its
	// settlement or to none, and R holds it once.
	funding := rowsOf[store.FundingRow](r.sink)
	wantCycle := ""
	if !settlementOf(recordAt).Before(time.UnixMilli(lastCycleRow(t, r.sink, c.ID).OpenedMs)) {
		wantCycle = string(c.ID)
	}
	if len(funding) != 1 || funding[0].RecordID != "f1" || !funding[0].Amount.Equal(dec("-0.1")) || funding[0].SettlementMs != settlementOf(recordAt).UnixMilli() || funding[0].CycleID != wantCycle {
		t.Errorf("funding rows = %+v, want f1 once, -0.1, on cycle %q", funding, wantCycle)
	}
	wantDec(t, "r_closed", rClosed(t, r.sink), "-0.1")
	// Every sample is one consistent view: the ledger holds nothing or the
	// one share the loop booked, R is 0 or -0.1 with the quota that follows
	// from it, and the venue's position is unread or 1.
	for _, s := range rowsOf[store.InventorySampleRow](r.sink) {
		ledgerOK := s.LedgerBear.IsZero() && (s.LedgerBull.IsZero() || s.LedgerBull.Equal(dec("1")))
		rOK := (s.R.IsZero() && s.QuotaTotal.Equal(dec("10000"))) || (s.R.Equal(dec("-0.1")) && s.QuotaTotal.Equal(dec("9999")))
		posOK := !s.PosBull.Valid || (s.PosBull.Decimal.Equal(dec("1")) && s.PosBear.Decimal.IsZero())
		usedOK := s.QuotaUsed.IsZero() == s.LedgerBull.IsZero() && (s.QuotaUsed.IsZero() || s.QuotaUsed.Equal(dec("4000"))) // 1 x 40 filled + 99 x 40 pending
		if !ledgerOK || !rOK || !posOK || !usedOK || s.Mismatch {
			t.Errorf("sample = %+v: not a consistent view of the ledger", s)
		}
	}
	if got := r.m.Cycle(); got == nil || len(got.Lots) != 1 || !got.Lots[0].Legs[cex.Bull].EntryQty.Equal(dec("1")) || !got.Lots[0].Legs[cex.Bull].EntryValue.Equal(dec("40")) {
		t.Errorf("the ledger after the run = %+v, want one lot with 1 share bought for 40", got)
	}
}

// replayClock is a fixed clock in the shape a replay passes (cex.Clock).
type replayClock struct{ now time.Time }

func (c replayClock) Now() time.Time                  { return c.now }
func (c replayClock) AfterFunc(time.Duration, func()) {}

// WithClock makes the ledger read the time from the given clock: a new
// ledger starts at the clock's time, not the wall clock's.
func TestManagerWithClockStartsTheLedgerOnIt(t *testing.T) {
	at := time.Date(2026, 11, 26, 5, 0, 0, 0, time.UTC)
	sink := &store.Fake{}
	m, err := NewManager(Config{Capital: dec("1000"), Leverage: dec("10")}, testPair, newFakeCex(), WithClock(replayClock{at}))
	if err != nil {
		t.Fatal(err)
	}
	m.UseStore(sink)
	if err := m.Restore(store.State{}, nil); err != nil {
		t.Fatal(err)
	}
	rows := sink.Rows("inventory_state")
	if len(rows) != 1 || rows[0].(store.InventoryStateRow).SinceMs != at.UnixMilli() {
		t.Fatalf("inventory_state rows %+v, want one starting at the clock's %d", rows, at.UnixMilli())
	}
}
