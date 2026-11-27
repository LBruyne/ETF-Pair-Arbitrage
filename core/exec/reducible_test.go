package exec

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/inventory"
	"etf-pair-arbitrage/core/store"
	"etf-pair-arbitrage/core/strategy"
)

// A reducing order may not exceed the venue's net position on its leg. The
// cycle's holding is what its lots hold; the dust of earlier cycles sits on
// the same venue position. When the dust has the other sign (a long cycle
// holding 7.80 next to dust of -0.01) the venue nets 7.79, and a reduce-only
// sell of 7.80 is refused (Bitget 40804, live 2026-11-27 05:06-11:38).
func TestReducibleCapsAtTheVenueNet(t *testing.T) {
	hold := [2]decimal.Decimal{dec("7.80"), dec("4.02")}
	cases := []struct {
		name string
		dir  strategy.Direction
		dust [2]decimal.Decimal
		want [2]decimal.Decimal
	}{
		{"long, dust against", strategy.Long, [2]decimal.Decimal{dec("-0.01"), dec("0")}, [2]decimal.Decimal{dec("7.79"), dec("4.02")}},
		{"long, dust with", strategy.Long, [2]decimal.Decimal{dec("0.01"), dec("0.02")}, [2]decimal.Decimal{dec("7.80"), dec("4.02")}},
		{"short, dust against", strategy.Short, [2]decimal.Decimal{dec("0"), dec("0.03")}, [2]decimal.Decimal{dec("7.80"), dec("3.99")}},
		{"short, dust with", strategy.Short, [2]decimal.Decimal{dec("-0.05"), dec("0")}, [2]decimal.Decimal{dec("7.80"), dec("4.02")}},
		{"dust bigger than the holding", strategy.Long, [2]decimal.Decimal{dec("-9"), dec("0")}, [2]decimal.Decimal{dec("0"), dec("4.02")}},
	}
	for _, c := range cases {
		got := reducible(hold, c.dir, c.dust)
		for _, leg := range cex.Legs {
			if !got[leg].Equal(c.want[leg]) {
				t.Errorf("%s: %s reducible %s, want %s", c.name, leg, got[leg], c.want[leg])
			}
		}
	}
}

// The restart of 2026-11-27 11:43:02 (build befe3cb): the cycle was taken
// back after a takeover holding SOXL 7.80 of its lot and no SOXS, next to
// -0.01 SOXL of dust, so the venue netted 7.79. No exit batch was taken up
// (the stop's forced orders came last), and the gap's trim of SOXL sold the
// ledger's 7.80 (Bitget 40804, 25231 by REST) until the forced exit sold
// 7.79. The trim is sized within the venue's net; the 0.01 left is dust
// and the cycle closes.
// takenBackState is the ledger the restart of 11:43:02 read back: the long
// c…904692 taken over, SOXL 7.80 of 15.58 left and SOXS closed, -0.01 SOXL
// of dust, and the two refused orders of the earlier process last.
func takenBackState(start time.Time) *store.State {
	opened := start.Add(-time.Hour)
	cycleID := fmt.Sprintf("c%d", opened.UnixMilli())
	return &store.State{
		Inventory: &store.InventoryStateRow{Capital: dec("2000"), SinceMs: opened.Add(-time.Hour).UnixMilli(),
			Dust: json.RawMessage(`{"bull":{"qty":"-0.01","avg_price":"33.39"}}`)},
		Cycle: &store.CycleRow{CycleID: cycleID, SessionID: "2026-11-24", Direction: "long", Status: string(inventory.CycleTakeover),
			PairAnchorKind: "bell", PairAnchorBull: dec("40"), PairAnchorBear: dec("60"), OpenedMs: opened.UnixMilli(), FundingKnown: true},
		Lots: []store.LotRow{{LotID: cycleID + "-1", CycleID: cycleID, Seq: 1, Status: string(inventory.LotOpen), Direction: "long",
			EntryQtyBull: dec("15.58"), EntryValueBull: dec("522.0858"), ExitQtyBull: dec("7.78"), ExitValueBull: dec("262.1082"),
			EntryQtyBear: dec("8.03"), EntryValueBear: dec("476.5958"), ExitQtyBear: dec("8.03"), ExitValueBear: dec("472.2"), OpenedMs: opened.UnixMilli()}},
		Orders: []store.OrderRow{
			longRow(filledRow(cycleID, opened, 1, cex.Bear, cex.PurposeEntry, "59.3519", "8.03", "476.5958"), cex.SideBuy),
			longRow(filledRow(cycleID, opened, 2, cex.Bull, cex.PurposeComplete, "33.51", "15.58", "522.0858"), cex.SideBuy),
			longRow(filledRow(cycleID, opened, 3, cex.Bear, cex.PurposeExit, "59.0381", "4.01", "236.7428"), cex.SideSell),
			longRow(filledRow(cycleID, opened, 4, cex.Bull, cex.PurposeExit, "33.69", "7.78", "262.1082"), cex.SideSell),
			longRow(filledRow(cycleID, opened, 5, cex.Bear, cex.PurposeExit, "58.58", "4.02", "235.4916"), cex.SideSell),
			rejectedRow(longRow(filledRow(cycleID, opened, 6, cex.Bull, cex.PurposeExit, "33.92", "7.8", "0"), cex.SideSell)),
			rejectedRow(longRow(filledRow(cycleID, opened, 7, cex.Bull, cex.PurposeForced, "36.4", "7.8", "0"), cex.SideSell)),
		},
	}
}

func TestRestartTrimsWithinTheVenueNet(t *testing.T) {
	start := utc8Time(11, 25, 5, 1, 0)
	state := takenBackState(start)
	r := newRig(t, start, rigOptions{state: state, thin: "bear"})
	if pos := r.inv.LedgerPositions(); !pos[cex.Bull].Equal(dec("7.79")) {
		t.Fatalf("ledger SOXL %s, want 7.79 (7.80 held, -0.01 dust)", pos[cex.Bull])
	}
	r.at(start)
	r.books(exitBook())
	r.stepAt(start)
	o := r.orders()
	if len(o) != 1 {
		t.Fatalf("%d orders, want the trim of SOXL; idle %q", len(o), r.m.idleReason)
	}
	if q := o[0].Request; q.Leg != cex.Bull || q.Purpose != cex.PurposeExit || !q.ReduceOnly || !q.Qty.Equal(dec("7.79")) {
		t.Fatalf("trim %s %s reduce-only %t qty %s, want an exit of SOXL 7.79", q.Leg, q.Purpose, q.ReduceOnly, q.Qty)
	}
	r.results()
	for s := 1; s <= 3 && r.inv.Cycle() != nil; s++ {
		r.at(start.Add(time.Duration(s) * time.Second))
		r.books(exitBook())
		r.step()
	}
	if n := len(r.orders()); n != 1 {
		t.Errorf("%d orders, want the one trim: 0.01 is dust", n)
	}
	if c := r.lastCycle(); r.inv.Cycle() != nil || !c.DustBull.Equal(dec("0.01")) {
		t.Errorf("cycle open %t, SOXL dust left %s; want closed with 0.01", r.inv.Cycle() != nil, c.DustBull)
	}
}

// longRow is row as an order of a long cycle: side, reduce-only for a
// closing order, and no lot for an exit (the ledger books it first in,
// first out).
func longRow(row store.OrderRow, side cex.Side) store.OrderRow {
	row.Side = string(side)
	if p := cex.OrderPurpose(row.Purpose); p == cex.PurposeExit || p == cex.PurposeForced {
		row.ReduceOnly, row.LotID = true, ""
	}
	return row
}

// rejectedRow is row refused by the venue with 40804, nothing filled.
func rejectedRow(row store.OrderRow) store.OrderRow {
	row.Status, row.ErrorCode, row.ErrorMessage = string(cex.StatusRejected), "40804", "The number of closed positions cannot exceed the number of positions held"
	row.CumExecQty, row.CumExecValue, row.AvgPrice, row.Fee, row.FeeKnown = decimal.Zero, decimal.Zero, decimal.Zero, decimal.Zero, false
	return row
}

// The thick target of an exit batch taken up after a restart is the round's
// cumulative one: the live short c…154775 (SOXS 8.03 / SOXL 15.58 at the
// bell anchors) closed SOXS 0.17 and SOXL 0.32 in its first exit1 batch;
// the second batch's SOXS 3.84 filled, its SOXL not yet. Past
// exit_pair_window SOXL closes at once: floor(4.01 x A_S / A_L) - 0.32 =
// 7.46, where a floor of the batch alone gave 7.45 and left 0.01 as dust.
func TestRestoredExitBatchClosesTheCumulativeTarget(t *testing.T) {
	start := utc8Time(11, 25, 5, 1, 0)
	opened := start.Add(-time.Hour)
	cycleID := fmt.Sprintf("c%d", opened.UnixMilli())
	short := func(row store.OrderRow, side cex.Side, final time.Time) store.OrderRow {
		row = longRow(row, side)
		row.SubmittedMs, row.FinalMs = final.UnixMilli(), final.UnixMilli()
		return row
	}
	thin1 := short(filledRow(cycleID, opened, 3, cex.Bear, cex.PurposeExit, "59.6424", "0.17", "10.139208"), cex.SideBuy, opened.Add(time.Minute))
	thin1.Qty, thin1.Status = dec("0.18"), string(cex.StatusCancelled)
	state := &store.State{
		Inventory: &store.InventoryStateRow{Capital: dec("2000"), SinceMs: opened.Add(-time.Hour).UnixMilli()},
		Cycle: &store.CycleRow{CycleID: cycleID, SessionID: "2026-11-24", Direction: "short", Status: string(inventory.CycleOpen),
			PairAnchorKind: "bell", PairAnchorBull: dec("32.0287420270484"), PairAnchorBear: dec("62.1463560859516"), OpenedMs: opened.UnixMilli(), FundingKnown: true},
		Lots: []store.LotRow{{LotID: cycleID + "-1", CycleID: cycleID, Seq: 1, Status: string(inventory.LotOpen), EndState: "built", Direction: "short",
			EntryQtyBull: dec("15.58"), EntryValueBull: dec("520.2162"), EntryQtyBear: dec("8.03"), EntryValueBear: dec("478.6683"), OpenedMs: opened.UnixMilli()}},
		Orders: []store.OrderRow{
			short(filledRow(cycleID, opened, 1, cex.Bear, cex.PurposeEntry, "59.61", "8.03", "478.6683"), cex.SideSell, opened),
			short(filledRow(cycleID, opened, 2, cex.Bull, cex.PurposeComplete, "33.39", "15.58", "520.2162"), cex.SideSell, opened),
			thin1,
			short(filledRow(cycleID, opened, 4, cex.Bull, cex.PurposeExit, "33.35", "0.32", "10.672"), cex.SideBuy, opened.Add(time.Minute)),
			short(filledRow(cycleID, opened, 5, cex.Bear, cex.PurposeExit, "59.614", "3.84", "228.91776"), cex.SideBuy, start.Add(-40*time.Second)),
		},
	}
	r := newRig(t, start, rigOptions{state: state, thin: "bear", perp: &perpRows{}})
	if _, err := r.m.recover(r.m.ctx); err != nil {
		t.Fatal(err)
	}
	if e := r.m.activeExit(); e == nil || !e.started {
		t.Fatalf("exit batch taken up %+v, want the one of SOXS 3.84", r.m.batch)
	}
	r.at(start)
	r.books(exitBook())
	r.stepAt(start)
	o := r.orders()
	if len(o) != 1 {
		t.Fatalf("%d orders, want SOXL's hard close; idle %q", len(o), r.m.idleReason)
	}
	if q := o[0].Request; q.Leg != cex.Bull || q.Purpose != cex.PurposeExit || q.Side != cex.SideBuy || !q.Qty.Equal(dec("7.46")) {
		t.Errorf("SOXL %s %s %s %s, want an exit buy of 7.46", q.Leg, q.Purpose, q.Side, q.Qty)
	}
}

// Owner item 99: orders the venue refuses in a row raise an alarm, for
// every purpose. Live, SOXL's exits were refused 21,787 times in 6.5 h
// without one. After rejectAlarmAfter refusals of one leg in a row: an
// Error line and an error event (orders_rejected) with the leg, the
// purposes, the codes, the order's quantity beside the ledger's and the
// venue's position and how long it has lasted; again every
// stop_alarm_after while the refusals go on, not on every refusal.
func TestRefusalsInARowRaiseTheAlarm(t *testing.T) {
	start := utc8Time(11, 25, 5, 1, 0)
	state := takenBackState(start)
	state.Orders = state.Orders[:5] // no refusal of the earlier process
	r := newRig(t, start, rigOptions{state: state, thin: "bear"})
	r.gate.setRefuse(func(req cex.OrderRequest) error {
		return &cex.VenueError{Code: "40804", Message: "The number of closed positions cannot exceed the number of positions held"}
	})
	alarms := func() (rows []store.EventRow) {
		for _, row := range r.sink.Rows("event") {
			if e := row.(store.EventRow); e.Kind == "orders_rejected" {
				rows = append(rows, e)
			}
		}
		return rows
	}
	refuse := func(at time.Time) {
		r.at(at)
		r.books(exitBook())
		r.stepAt(at)
		r.results()
	}
	at := start
	for i := 0; i < 2; i++ {
		refuse(at)
		at = at.Add(250 * time.Millisecond)
	}
	r.stepAt(at)
	if n := len(alarms()); n != 0 {
		t.Fatalf("%d alarms after two refusals, want none", n)
	}
	refuse(at)
	at = at.Add(250 * time.Millisecond)
	r.stepAt(at)
	got := alarms()
	if len(got) != 1 || got[0].Level != "error" {
		t.Fatalf("alarms after three refusals %+v, want one error", got)
	}
	d := decodeDetail(t, got[0].Detail)
	if d["leg"] != "bull" || d["count"] != float64(3) || d["qty"] != "7.79" || d["ledger"] != "7.79" || d["venue"] != "7.79" || d["hold"] != "7.8" {
		t.Errorf("alarm detail %v", d)
	}
	if codes, _ := d["codes"].(map[string]any); codes["40804"] != float64(3) {
		t.Errorf("codes %v, want 40804 x 3", d["codes"])
	}
	if purposes, _ := d["purposes"].(map[string]any); purposes["exit"] != float64(3) {
		t.Errorf("purposes %v, want exit x 3", d["purposes"])
	}

	// Refused on: no new alarm before stop_alarm_after, one after it.
	for i := 0; i < 4; i++ {
		refuse(at)
		at = at.Add(250 * time.Millisecond)
	}
	if n := len(alarms()); n != 1 {
		t.Fatalf("%d alarms within stop_alarm_after, want still one", n)
	}
	at = start.Add(r.m.cfg.StopAlarmAfter + time.Second)
	refuse(at)
	r.stepAt(at.Add(250 * time.Millisecond))
	if got := alarms(); len(got) != 2 {
		t.Fatalf("%d alarms after stop_alarm_after, want 2", len(got))
	} else if d := decodeDetail(t, got[1].Detail); d["count"] != float64(7) || d["lasted_s"] != float64(60.75) {
		t.Errorf("repeated alarm detail %v, want count 7 (the 8th has no result yet) lasting 60.75 s from the round that saw the first refusal", d)
	}
}
