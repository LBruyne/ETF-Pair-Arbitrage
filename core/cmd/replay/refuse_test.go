package main

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/logger"
	"etf-pair-arbitrage/core/store"
)

// Bitget refuses a reduce-only order larger than what the position holds
// against its side (40804 by WebSocket, 25231 by REST: live 2026-11-27,
// SOXL 7.80 against a net long of 7.79); the paper venue alone would fill
// it in part. The replay's venue refuses it as Bitget does, so the loop's
// handling of that refusal (SPEC 12.12, item 100 point 1) is replayed too.
func TestReplayVenueRefusesAReduceAboveThePosition(t *testing.T) {
	logger.GlobalLogger = slog.New(slog.DiscardHandler)
	clock := &vclock{now: t0}
	pair := testSim(t, "r", 0).cfg.Cex.Pair
	inner := &replayVenue{rules: bitgetRules(pair.Bull.Symbol, pair.Bear.Symbol), onEvent: map[cex.Symbol]func(cex.BookEvent){}, onDown: map[cex.Symbol]func(error){}}
	paper := cex.NewPaperVenue(inner, cex.PaperConfig{Pair: pair, Clock: clock})
	paper.SeedPositions([2]decimal.Decimal{d("7.79"), d("-3")})
	v := refusingPaper{paper}
	order := func(id string, leg cex.Leg, side cex.Side, qty string, reduce bool) error {
		symbol := pair.Bull.Symbol
		if leg == cex.Bear {
			symbol = pair.Bear.Symbol
		}
		_, err := v.PlaceOrder(context.Background(), cex.OrderRequest{ClientOrderID: cex.ClientOrderID(id), Symbol: symbol, Leg: leg, Side: side, Price: d("36.45"), Qty: d(qty), ReduceOnly: reduce})
		return err
	}
	refused := func(err error) bool {
		var ve *cex.VenueError
		return errors.As(err, &ve) && ve.Kind == cex.ErrorReduceQty && ve.Code == "40804"
	}
	if err := order("a", cex.Bull, cex.SideSell, "7.80", true); !refused(err) {
		t.Errorf("SOXL sell 7.80 reduce-only against 7.79: %v, want the 40804 refusal", err)
	}
	if err := order("b", cex.Bull, cex.SideSell, "7.79", true); err != nil {
		t.Errorf("SOXL sell 7.79 reduce-only against 7.79: %v", err)
	}
	if err := order("c", cex.Bull, cex.SideBuy, "0.01", true); !refused(err) {
		t.Errorf("a reduce-only buy against a long: %v, want refused", err)
	}
	if err := order("d", cex.Bear, cex.SideBuy, "3", true); err != nil {
		t.Errorf("SOXS buy 3 reduce-only against a short of 3: %v", err)
	}
	if err := order("e", cex.Bear, cex.SideBuy, "10", false); err != nil {
		t.Errorf("an order that is not reduce-only: %v", err)
	}
}

// The simulation sends its orders through that venue: a reduce-only order
// on a flat paper account is recorded as refused, with Bitget's code.
func TestSimulationRecordsTheRefusal(t *testing.T) {
	logger.GlobalLogger = slog.New(slog.DiscardHandler)
	s, err := newSim(testSim(t, "r", 15*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	pair := s.sc.cfg.Cex.Pair
	if err := s.cmgr.PlaceOrders(cex.OrderRequest{ClientOrderID: "x-1", Symbol: pair.Bull.Symbol, Leg: cex.Bull, Side: cex.SideSell, Price: d("40"), Qty: d("1"), ReduceOnly: true, Purpose: cex.PurposeExit, CycleID: "c"}); err != nil {
		t.Fatal(err)
	}
	if o := s.sink.orders["x-1"]; o.Status != "rejected" || o.ErrorCode != "40804" {
		t.Errorf("recorded %s %q, want rejected 40804", o.Status, o.ErrorCode)
	}
}

// The report counts the refused orders (those of the reduce quantity
// apart), the closed cycles whose orders left a quantity on a leg (dust a
// per-batch rounding would leave), and the longest exit batch from its
// decision to its last fill.
func TestSeqStatsCountsRefusalsResidueAndExitSpan(t *testing.T) {
	s := seqSink()
	s.Record(store.OrderRow{ClientOrderID: "o11", CycleID: "c1", Leg: "bull", Purpose: "exit", Side: "sell", Qty: d("5.01"), Status: "rejected", ErrorCode: "40804", SubmittedMs: 39000, ReduceOnly: true},
		store.OrderRow{ClientOrderID: "o12", CycleID: "c1", Leg: "bull", Purpose: "exit", Side: "sell", Qty: d("5"), Status: "rejected", ErrorCode: "40762", SubmittedMs: 39050, ReduceOnly: true})
	q := summarize(s, "", d("1000"), d("820.6"), nil).Seq
	if q.Refused != 2 || q.RefusedReduceQty != 1 {
		t.Errorf("refused %d (reduce quantity %d), want 2 (1)", q.Refused, q.RefusedReduceQty)
	}
	if q.ResidualCycles != 0 {
		t.Errorf("residual cycles %d, want 0: c1 ends flat on both legs", q.ResidualCycles)
	}
	// exit1 at 5000, its last fill at 5100; exit2 at 9000, its last fill (the
	// D2 hard close) at 39200.
	if q.ExitBatches != 2 || q.ExitBatchMaxS != 30.2 {
		t.Errorf("exit batches %d, longest %v s; want 2 and 30.2", q.ExitBatches, q.ExitBatchMaxS)
	}

	// A cycle closed with 0.01 SOXL short left (the 11-26 round 2: 15.58
	// opened, 15.57 bought back).
	s = newMemSink(false)
	s.Record(store.OrderRow{ClientOrderID: "p1", CycleID: "c2", LotID: "c2-1", Leg: "bull", Purpose: "complete", Side: "sell", Qty: d("15.58"), CumExecQty: d("15.58"), CumExecValue: d("520.2162"), Status: "filled", SubmittedMs: 1},
		store.OrderRow{ClientOrderID: "p2", CycleID: "c2", Leg: "bull", Purpose: "exit", Side: "buy", Qty: d("15.57"), CumExecQty: d("15.57"), CumExecValue: d("517.9"), Status: "filled", SubmittedMs: 2, ReduceOnly: true},
		store.CycleRow{CycleID: "c2", SessionID: "2026-11-25", Direction: "short", Status: "closed", ClosedMs: 3})
	if q := summarize(s, "", d("1000"), d("520"), nil).Seq; q.ResidualCycles != 1 || !q.ResidualQty[0].Equal(d("-0.01")) || !q.ResidualQty[1].IsZero() {
		t.Errorf("residual cycles %d, quantity %v; want 1 and SOXL -0.01", q.ResidualCycles, q.ResidualQty)
	}
}
