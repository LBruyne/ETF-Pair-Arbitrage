package exec

import (
	"context"
	"errors"
	"math"
	"strings"
	"syscall"
	"testing"
	"time"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/inventory"
	"etf-pair-arbitrage/core/store"
)

// NewManager refuses a configuration that does not validate: the defaults
// alone lack sim_order_latency.
func TestNewManagerValidates(t *testing.T) {
	if _, err := NewManager(DefaultConfig, nil, nil, nil, nil); err == nil || !strings.Contains(err.Error(), "exec.sim_order_latency.bull") {
		t.Errorf("NewManager(DefaultConfig) = %v, want the missing sim_order_latency reported", err)
	}
}

// runLoop starts Run on its own goroutine and returns what it returns.
func runLoop(r *rig, ctx context.Context) <-chan error {
	done := make(chan error, 1)
	go func() { done <- r.m.Run(ctx) }()
	return done
}

// SIGUSR1 (no new entries) leaves the loop running; SIGTERM while flat ends
// it with nil.
func TestRunStopsOnStopSignalOnly(t *testing.T) {
	r := newRig(t, utc8Time(11, 25, 5, 1, 0), rigOptions{})
	done := runLoop(r, context.Background())

	r.signals <- syscall.SIGUSR1
	select {
	case err := <-done:
		t.Fatalf("Run returned on SIGUSR1: %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	r.signals <- syscall.SIGTERM
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run after SIGTERM = %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return on SIGTERM")
	}
}

// Signals that arrived while the process was starting, before Run, are
// handled when Run begins: a stop request among them ends Run at once when
// nothing is held.
func TestStopSignalQueuedBeforeRunIsHonoured(t *testing.T) {
	r := newRig(t, utc8Time(11, 25, 5, 1, 0), rigOptions{})
	r.signals <- syscall.SIGUSR1
	r.signals <- syscall.SIGTERM
	select {
	case err := <-runLoop(r, context.Background()):
		if err != nil {
			t.Errorf("Run = %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not honour the stop signal that was queued before it started")
	}
}

// A cancelled context ends Run with the context's error.
func TestRunEndsWithContext(t *testing.T) {
	r := newRig(t, utc8Time(11, 25, 5, 1, 0), rigOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := <-runLoop(r, ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Run = %v, want context.Canceled", err)
	}
}

// A stop with a cycle open: Run closes the cycle with the forced exit and
// leaves only once it is closed, with stop as the end reason.
func TestStopClosesTheCycleThenLeaves(t *testing.T) {
	r := newRig(t, utc8Time(11, 25, 5, 1, 0), rigOptions{})
	r.warmUp()
	r.at(utc8Time(11, 25, 5, 3, 5))
	r.books(entryBook())
	r.trade(utc8Time(11, 25, 5, 3, 5))
	r.books(exitBook()) // fresh prices for the forced exit; the clock stays at 05:03:05

	r.signals <- syscall.SIGTERM
	select {
	case err := <-runLoop(r, context.Background()):
		if err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not leave after closing the cycle")
	}
	if c := r.lastCycle(); c.Status != string(inventory.CycleClosed) || c.EndReason != inventory.EndStop {
		t.Errorf("cycle %s %s, want closed stop", c.Status, c.EndReason)
	}
	if d := r.decisions(); d[len(d)-1].Kind != "forced" {
		t.Errorf("last decision %s, want forced", d[len(d)-1].Kind)
	}
}

// Signals that came before Run count before its first decision: SIGUSR1
// queued with the mean ready and example 1's short signal on the book, and
// no entry is sent.
func TestSignalsQueuedBeforeTheFirstRound(t *testing.T) {
	r := newRig(t, utc8Time(11, 25, 5, 1, 0), rigOptions{})
	r.warmUp()
	r.at(utc8Time(11, 25, 5, 3, 5))
	r.books(entryBook())
	r.signals <- syscall.SIGUSR1
	ctx, cancel := context.WithCancel(context.Background())
	done := runLoop(r, ctx)
	time.Sleep(300 * time.Millisecond)
	cancel()
	<-done
	if n := len(r.orders()); n != 0 {
		t.Errorf("%d orders sent with no new entries asked for before Run", n)
	}
}

// Each second's signal_sample carries both legs' bid1/ask1 midpoints, for
// the board; a leg without a best bid and ask has none (NULL).
func TestSignalSampleCarriesTheMidpoints(t *testing.T) {
	start := utc8Time(11, 25, 5, 1, 0)
	r := newRig(t, start, rigOptions{})
	r.stepAt(start)
	first := r.sink.Last("signal_sample").(store.SignalSampleRow)
	if !math.IsNaN(first.MidBull) || !math.IsNaN(first.MidBear) {
		t.Errorf("mids before any book %v / %v, want NaN", first.MidBull, first.MidBear)
	}
	r.best(cex.Bull, lv("40.38", "10"), lv("40.39", "10"))
	r.best(cex.Bear, lv("59.41", "10"), lv("59.43", "10"))
	r.stepAt(start.Add(time.Second))
	got := r.sink.Last("signal_sample").(store.SignalSampleRow)
	if got.TsMs != start.Add(time.Second).UnixMilli() || math.Abs(got.MidBull-40.385) > 1e-9 || math.Abs(got.MidBear-59.42) > 1e-9 {
		t.Errorf("sample at %d mids %v / %v, want 40.385 / 59.42 a second later", got.TsMs, got.MidBull, got.MidBear)
	}
}
