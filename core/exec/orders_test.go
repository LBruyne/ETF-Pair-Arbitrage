package exec

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/inventory"
	"etf-pair-arbitrage/core/strategy"
)

// roundCounter counts the loop's rounds: Run takes AccountChanged once
// before each.
type roundCounter struct {
	*testCex
	rounds atomic.Int64
}

func (c *roundCounter) AccountChanged() <-chan struct{} {
	c.rounds.Add(1)
	return c.testCex.AccountChanged()
}

// A flat cycle whose close cannot be written is tried again a second later,
// not at once: each try reads the venue's positions, and that read wakes the
// loop. With the clock standing still and no book frame, Run makes only the
// rounds of its 200 ms tick in 500 ms (without the wait: about 25,000, each
// with a positions read). A second later, with the database back, it closes.
func TestCloseRetryWaits(t *testing.T) {
	r := newRig(t, utc8Time(11, 25, 5, 1, 0), rigOptions{})
	r.warmUp()
	r.at(utc8Time(11, 25, 5, 3, 5))
	r.books(entryBook())
	r.trade(utc8Time(11, 25, 5, 3, 5))
	t1 := utc8Time(11, 25, 5, 3, 30)
	r.at(t1)
	r.books(exitBook())
	r.stepAt(t1) // the exit closes all of 105.30 / 70.20
	r.results()
	r.sink.Fail(errors.New("database down"))

	counter := &roundCounter{testCex: r.src}
	r.m.cex = counter
	ctx, cancel := context.WithCancel(context.Background())
	done := runLoop(r, ctx)
	time.Sleep(500 * time.Millisecond)
	cancel()
	<-done
	if n := counter.rounds.Load(); n > 10 {
		t.Errorf("%d rounds in 500 ms, want the tick's few", n)
	}
	if r.inv.Cycle() == nil || r.m.idleReason != strategy.ReasonAwaitClose {
		t.Fatalf("cycle closed %t, idle reason %q; want it open, await_close", r.inv.Cycle() == nil, r.m.idleReason)
	}

	r.sink.Fail(nil)
	r.stepAt(t1.Add(500 * time.Millisecond))
	if r.inv.Cycle() == nil {
		t.Error("the close was tried again within the second")
	}
	r.stepAt(t1.Add(time.Second))
	if c := r.lastCycle(); r.inv.Cycle() != nil || c.EndReason != inventory.EndExitLine {
		t.Errorf("cycle open %t, end reason %q; want closed exit_line", r.inv.Cycle() != nil, c.EndReason)
	}
}

// A loop that takes over a cycle it did not open (a new ExecManager on the
// same cex order table and ledger, as after a restart) numbers its orders
// after the cycle's highest sequence: the re-entry gets ids 3 and 4, lot 1
// keeps its 105.30 / 70.20 and lot 2 is 60 / 40 (with ids 1 and 2 again the
// new orders would replace lot 1's).
func TestOrderIDsContinueInATakenOverCycle(t *testing.T) {
	r := newRig(t, utc8Time(11, 25, 5, 1, 0), rigOptions{})
	r.warmUp()
	r.at(utc8Time(11, 25, 5, 3, 5))
	r.books(entryBook())
	r.trade(utc8Time(11, 25, 5, 3, 5))

	stats, bars, lastReady := r.m.stats, r.m.bars, r.m.lastReady
	m2, err := NewManager(r.m.cfg, r.src, r.inv, r.m.strat, r.signals)
	if err != nil {
		t.Fatal(err)
	}
	m2.UseStore(r.sink)
	m2.timing.clock, m2.ctx = r.clock, r.m.ctx
	m2.start(r.clock())
	m2.stats, m2.bars, m2.lastReady = stats, bars, lastReady
	r.m = m2

	t1 := utc8Time(11, 25, 5, 3, 20)
	r.at(t1)
	r.books(pairBook{
		bullBids: side{lv("40.51", "20"), lv("40.50", "100")}, bullAsks: side{lv("40.53", "100"), lv("40.54", "100")},
		bearBids: side{lv("59.47", "10"), lv("59.465", "10"), lv("59.46", "20"), lv("59.455", "20"), lv("59.45", "20")},
		bearAsks: side{lv("59.48", "50"), lv("59.485", "50"), lv("59.49", "50"), lv("59.495", "50"), lv("59.50", "50")},
	})
	r.trade(t1)
	var seqs []int
	for _, o := range r.orders() {
		seq, _ := cex.ClientOrderSeq(o.Request.ClientOrderID)
		seqs = append(seqs, seq)
	}
	if len(seqs) != 4 || seqs[2] != 3 || seqs[3] != 4 {
		t.Errorf("order sequences %v, want 1 2 3 4", seqs)
	}
	c := r.inv.Cycle()
	if c == nil || len(c.Lots) != 2 {
		t.Fatalf("cycle %+v, want two lots", c)
	}
	for i, want := range [][2]string{{"105.3", "70.2"}, {"60", "40"}} {
		if l := c.Lots[i]; !l.Legs[cex.Bull].EntryQty.Equal(dec(want[0])) || !l.Legs[cex.Bear].EntryQty.Equal(dec(want[1])) || l.Status != inventory.LotOpen {
			t.Errorf("lot %d %s %s / %s, want open %s / %s", i+1, l.Status, l.Legs[cex.Bull].EntryQty, l.Legs[cex.Bear].EntryQty, want[0], want[1])
		}
	}
}

// V6: an entry batch that fills nothing is void and does not reset the
// entry spacing; a built one does. The paper venue matches 100 ms after the
// send, on a book that has moved away by then.
func TestVoidEntryDoesNotResetSpacing(t *testing.T) {
	r := newRig(t, utc8Time(11, 25, 5, 1, 0), rigOptions{latency: 100 * time.Millisecond})
	r.warmUp()
	t0 := utc8Time(11, 25, 5, 3, 5)
	r.at(t0)
	r.books(entryBook())
	r.stepAt(t0)
	away := entryBook()
	away.bullBids = side{lv("40.30", "300"), lv("40.29", "500")}
	away.bearBids = side{lv("59.30", "50"), lv("59.29", "50"), lv("59.28", "50"), lv("59.27", "50"), lv("59.26", "50")}
	r.books(away)
	r.results()
	r.step()
	if c := r.lastCycle(); r.inv.Cycle() != nil || c.EndReason != inventory.EndNoFill {
		t.Fatalf("cycle open %t, end reason %q; want closed no_fill", r.inv.Cycle() != nil, c.EndReason)
	}
	if r.m.lastEntry != nil {
		t.Errorf("the void batch reset the spacing: %+v", r.m.lastEntry)
	}

	// 5 s later the same signal, x = 10 < e + 1: allowed, no built entry
	// came before it.
	r.at(t0.Add(5 * time.Second))
	r.books(entryBook())
	r.trade(t0.Add(5 * time.Second))
	if r.m.lastEntry == nil || !r.m.lastEntry.At.Equal(t0.Add(5*time.Second)) {
		t.Fatalf("the built batch did not reset the spacing: %+v", r.m.lastEntry)
	}
	// And after the built one the same signal waits.
	r.at(t0.Add(10 * time.Second))
	r.books(entryBook())
	r.idleAt(t0.Add(10*time.Second), strategy.ReasonSpacing)
}

// A cycle closed at the line just before the close window keeps exit_line
// as its end reason when its close is confirmed inside the window: a flat
// cycle begins no forced exit.
func TestFlatCycleInTheCloseWindowKeepsItsEndReason(t *testing.T) {
	r := newRig(t, utc8Time(11, 25, 5, 1, 0), rigOptions{})
	r.warmUp()
	r.at(utc8Time(11, 25, 5, 3, 5))
	r.books(entryBook())
	r.trade(utc8Time(11, 25, 5, 3, 5))

	t1 := utc8Time(11, 26, 4, 58, 59)
	r.at(t1)
	r.books(exitBook())
	r.stepAt(t1) // the exit at the line, on the last ready mean
	r.results()
	r.stepAt(utc8Time(11, 26, 4, 59, 0).Add(50 * time.Millisecond))
	if c := r.lastCycle(); r.inv.Cycle() != nil || c.EndReason != inventory.EndExitLine {
		t.Errorf("cycle open %t, end reason %q; want closed exit_line", r.inv.Cycle() != nil, c.EndReason)
	}
	if r.m.forced != "" {
		t.Errorf("forced exit %q begun for a flat cycle", r.m.forced)
	}
}
