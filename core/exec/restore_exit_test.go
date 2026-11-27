package exec

import (
	"fmt"
	"testing"
	"time"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/store"
	"etf-pair-arbitrage/core/strategy"
)

// Audit of the first thin-first live run, item 一.4: an exit batch taken up
// after a restart keeps its exit's rules. They are not in the order
// records; the batch's exit decision (kind exit, matched by the thin
// order's client id) holds them: the line, D1 and the protection, the
// decision's spread with its anchors and F, and with exit1 the half
// target's closed quantities at the batch's start.

// restartedExit is state with the thin exit order thin appended and the
// exit decision whose detail is detail (the thin order's id is added to
// it as client_order_ids).
func restartedExit(state *store.State, thin store.OrderRow, reason, detail string) *store.State {
	state.Orders = append(state.Orders, thin)
	state.ExitDecision = &store.DecisionRow{TsMs: thin.SubmittedMs, CycleID: state.Cycle.CycleID, Kind: "exit", Direction: state.Cycle.Direction, Reason: reason,
		Detail: []byte(fmt.Sprintf(`{%s,"anchor_kind":"bell","anchor_bull":"40","anchor_bear":"60","f":"1","thin_leg":"bear","client_order_ids":[%q]}`, detail, thin.ClientOrderID))}
	return state
}

// thinExitRow is the short cycle's thin exit order: SOXS bought back, qty
// at price (value), its result at final.
func thinExitRow(state *store.State, seq int, price, qty, value string, final time.Time) store.OrderRow {
	opened := time.UnixMilli(state.Cycle.OpenedMs)
	row := longRow(filledRow(state.Cycle.CycleID, opened, seq, cex.Bear, cex.PurposeExit, price, qty, value), cex.SideBuy)
	row.SubmittedMs, row.FinalMs = final.Add(-20*time.Millisecond).UnixMilli(), final.UnixMilli()
	return row
}

// A D1 batch (the pre-check failed, it exits all the same with SOXL held to
// its pricing level at the decision, 40.38) restarted 10 s after its SOXS
// 70.20 filled @ 59.385: within exit_pair_window SOXL still goes within the
// protection, 105.30 @ 40.38, and does not wait for its break-even bound,
// which a buy at 40.38 does not reach.
func TestRestoredD1ExitKeepsTheProtection(t *testing.T) {
	start := utc8Time(11, 25, 5, 3, 0)
	state := lossState(utc8Time(11, 25, 5, 1, 0))
	thin := thinExitRow(state, 3, "59.385", "70.2", "4168.827", start.Add(-10*time.Second))
	restartedExit(state, thin, "d1_loss", `"line":"exit","d_dec":-5,"expected_net":"-1.1","d1":true,"protect":"40.38"`)
	r := newRig(t, start, rigOptions{state: state, thin: "bear", perp: &perpRows{}})
	if _, err := r.m.recover(r.m.ctx); err != nil {
		t.Fatal(err)
	}
	e := r.m.activeExit()
	if e == nil || !e.started || !e.batch.D1 || !e.batch.Protect.Equal(dec("40.38")) || e.batch.DecisionD != -5 || e.batch.Line != strategy.ExitLineOne {
		t.Fatalf("exit batch taken up %+v, want the D1 batch with its protection 40.38, d_dec -5 and its line", e)
	}
	b := exitBook()
	b.bullAsks = side{lv("40.38", "400"), lv("40.39", "400")}
	r.at(start)
	r.books(b)
	r.stepOnce()
	ex := r.exitOrdersSince(0)
	if len(ex) != 1 {
		t.Fatalf("%d exit orders, want SOXL within the protection; idle %q", len(ex), r.m.idleReason)
	}
	wantOrder(t, "thick", ex[0], cex.Bull, cex.PurposeExit, cex.SideBuy, "105.3", "40.38")
	if rows := r.pairRows("exit_pair"); len(rows) == 0 || detailOf(t, rows[len(rows)-1])["d1"] != true || rows[len(rows)-1].Reason != string(strategy.ReasonD1Loss) {
		t.Errorf("exit_pair rows %v, want the thick order recorded under D1", rows)
	}
}

// An exit1 batch restarted after its SOXS 35.10 filled and was counted
// towards H (stored: SOXS 35.10, SOXL 0 closed): its SOXL 52.65 after the
// restart counts once, and SOXS's fill is not counted again - H closed
// 35.10 / 52.65, not 70.20.
func TestRestoredExit1BatchCountsTowardsHOnce(t *testing.T) {
	start := utc8Time(11, 25, 5, 3, 0)
	state := lossState(utc8Time(11, 25, 5, 1, 0))
	state.Lots[0].EntryValueBull, state.Lots[0].EntryValueBear = dec("4259.385"), dec("4172.0292")
	state.Orders[0] = filledRow(state.Cycle.CycleID, time.UnixMilli(state.Cycle.OpenedMs), 1, cex.Bear, cex.PurposeEntry, "59.4306", "70.2", "4172.0292")
	state.Orders[1] = filledRow(state.Cycle.CycleID, time.UnixMilli(state.Cycle.OpenedMs), 2, cex.Bull, cex.PurposeComplete, "40.45", "105.3", "4259.385")
	fill := start.Add(-5 * time.Second)
	thin := thinExitRow(state, 3, "59.403", "35.1", "2085.0453", fill)
	restartedExit(state, thin, "", `"line":"exit1","d_dec":-0.5,"expected_net":"1.5","d1":false,"protect":"0","stage_closed_bull":"0","stage_closed_bear":"0"`)
	state.ExitStage = &store.ExitStageRow{CycleID: state.Cycle.CycleID, SetMs: fill.Add(-time.Second).UnixMilli(), BuiltLots: 1,
		TargetBull: dec("52.65"), TargetBear: dec("35.1"), ClosedBull: dec("0"), ClosedBear: dec("35.1"), DoneMs: fill.UnixMilli()}
	exit2 := -1.0
	r := newRig(t, start, rigOptions{state: state, thin: "bear", perp: &perpRows{}, exit2: &exit2})
	if _, err := r.m.recover(r.m.ctx); err != nil {
		t.Fatal(err)
	}
	if e := r.m.activeExit(); e == nil || !e.started || e.batch.Line != strategy.ExitLine1 {
		t.Fatalf("exit batch taken up %+v, want the exit1 batch", e)
	}
	r.at(start)
	r.books(exit1Book())
	r.stepOnce()
	r.results()
	r.stepAt(start.Add(10 * time.Millisecond))
	ex := r.exitOrdersSince(0)
	if len(ex) != 1 {
		t.Fatalf("%d exit orders, want SOXL's half; idle %q", len(ex), r.m.idleReason)
	}
	wantOrder(t, "thick half", ex[0], cex.Bull, cex.PurposeExit, cex.SideBuy, "52.65", "40.39")
	row, ok := r.sink.Last("exit_stage").(store.ExitStageRow)
	if !ok || !row.ClosedBear.Equal(dec("35.1")) || !row.ClosedBull.Equal(dec("52.65")) {
		t.Fatalf("exit_stage %+v, want H closed 35.10 / 52.65: the SOXS fill counted once, SOXL's after the restart counted", r.sink.Last("exit_stage"))
	}
}
