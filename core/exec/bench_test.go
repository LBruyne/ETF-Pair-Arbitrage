package exec

import (
	"fmt"
	"testing"
	"time"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/strategy"
)

// BenchmarkRound measures one decision round of the loop on a cycle of n
// orders (audit 四, last bullet): the long c…904692 of the incident with
// its exit batch taken up after a restart, SOXS 4.02 closed and SOXL's
// exits refused n-5 times, as 05:06:52-11:43 left it. SOXL's bids are
// below its break-even bound, so every round decides on the whole snapshot
// and waits (exit_bound): the order snapshot, its booking, the run of
// refusals, the batch's fills and the exit step.
func BenchmarkRound(b *testing.B) {
	for _, n := range []int{20, 20000} {
		b.Run(fmt.Sprintf("orders=%d", n), func(b *testing.B) {
			start := utc8Time(11, 25, 5, 6, 53)
			state := incidentState(start)
			id, opened := state.Cycle.CycleID, time.UnixMilli(state.Cycle.OpenedMs)
			state.Orders[4].FinalMs = start.Add(-time.Second).UnixMilli() // the thin close: exit_pair_window runs
			for seq := 6; seq <= n; seq++ {
				row := rejectedRow(longRow(filledRow(id, opened, seq, cex.Bull, cex.PurposeExit, "33.92", "7.8", "0"), cex.SideSell))
				row.SubmittedMs, row.FinalMs = start.Add(-time.Second).UnixMilli(), start.Add(-time.Second).UnixMilli()
				state.Orders = append(state.Orders, row)
			}
			r := newRig(b, start, rigOptions{state: state, thin: "bear", perp: &perpRows{}})
			if _, err := r.m.recover(r.m.ctx); err != nil {
				b.Fatal(err)
			}
			if e := r.m.activeExit(); e == nil || len(r.m.batch.orders) != n-4 {
				b.Fatalf("exit batch %+v, want the thin order and its %d refused SOXL orders", r.m.batch, n-5)
			}
			book := exitBook()
			book.bullBids = side{lv("30", "300"), lv("29.99", "300")}
			r.at(start)
			r.books(book)
			r.stepOnce()
			if len(r.orders()) != 0 || r.m.idleReason != strategy.ReasonExitBound {
				b.Fatalf("%d orders, idle %q; want the round to wait for SOXL's bound", len(r.orders()), r.m.idleReason)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				r.m.step(start)
			}
		})
	}
}
