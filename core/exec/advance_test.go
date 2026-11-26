package exec

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/strategy"
)

// countingCex counts the rounds of a loop: each reads the books once.
type countingCex struct {
	*testCex
	rounds atomic.Int64
}

func (c *countingCex) Books() cex.Books { c.rounds.Add(1); return c.testCex.Books() }

// fixedClock is a clock in the shape a replay passes (cex.Clock).
type fixedClock struct{ now time.Time }

func (c *fixedClock) Now() time.Time                  { return c.now }
func (c *fixedClock) AfterFunc(time.Duration, func()) {}

// A loop driven by Advance runs its rounds when Run's would: at the first
// call; then only when a publication closed a channel taken before the last
// round, or at each tick of the step interval counted from Start on the
// manager's clock; a round that publishes (here: a read of the positions)
// is followed at once by another.
func TestAdvanceRunsRoundsWhenRunWould(t *testing.T) {
	base := utc8Time(11, 24, 23, 0, 0)
	r := newRig(t, base, rigOptions{})
	clk := &fixedClock{now: base}
	src := &countingCex{testCex: r.src}
	strat, err := strategy.New(testStrategy())
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig
	cfg.SimOrderLatency = LegLatency{Bull: time.Millisecond, Bear: time.Millisecond}
	m, err := NewManager(cfg, src, r.inv, strat, nil, WithClock(clk))
	if err != nil {
		t.Fatal(err)
	}
	if !m.timing.clock().Equal(base) {
		t.Fatalf("the manager's clock reads %s, want the given clock's", m.timing.clock())
	}
	if leave, err := m.Start(context.Background()); err != nil || leave {
		t.Fatalf("Start = %t, %v", leave, err)
	}
	if got, want := m.NextTick(), base.Add(200*time.Millisecond); !got.Equal(want) {
		t.Fatalf("NextTick = %s, want %s", got, want)
	}
	rounds := func(at time.Duration) int64 {
		t.Helper()
		before := src.rounds.Load()
		clk.now = base.Add(at)
		if m.Advance(clk.now) {
			t.Fatal("the loop asked to exit")
		}
		return src.rounds.Load() - before
	}
	if n := rounds(0); n < 1 {
		t.Fatalf("the first Advance ran %d rounds, want at least one", n)
	}
	if n := rounds(100 * time.Millisecond); n != 0 {
		t.Fatalf("without a publication or a tick %d rounds ran", n)
	}
	if n := rounds(200 * time.Millisecond); n != 1 {
		t.Fatalf("at the tick %d rounds ran, want 1", n)
	}
	if got, want := m.NextTick(), base.Add(400*time.Millisecond); !got.Equal(want) {
		t.Fatalf("NextTick after the tick = %s, want %s", got, want)
	}
	// A publication: the positions read again by cex.
	if _, err := r.cex.ReadPositions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := rounds(250 * time.Millisecond); n < 1 {
		t.Fatalf("after a publication %d rounds ran, want at least one", n)
	}
	if n := rounds(300 * time.Millisecond); n != 0 {
		t.Fatalf("with nothing new %d rounds ran", n)
	}
	// A late call catches up with the ticks it passed: one round, the next
	// tick on the grid after it.
	if n := rounds(1050 * time.Millisecond); n != 1 {
		t.Fatalf("a call past several ticks ran %d rounds, want 1", n)
	}
	if got, want := m.NextTick(), base.Add(1200*time.Millisecond); !got.Equal(want) {
		t.Fatalf("NextTick = %s, want %s", got, want)
	}
}
