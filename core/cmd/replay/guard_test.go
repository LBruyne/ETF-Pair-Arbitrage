package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A paper run is reproduced only from a ledger as it was at its start: one
// whose stored start lies after the run's start was replaced since, and a
// cycle open across the start is not restored by the replay. Both are
// refused with the reason.
func TestPaperRunStartMustBeReproducible(t *testing.T) {
	if err := checkRestart(1000, 2000, nil); err != nil {
		t.Errorf("a ledger from before the start was refused: %v", err)
	}
	if err := checkRestart(3000, 2000, nil); err == nil || !strings.Contains(err.Error(), "inventory_state") {
		t.Errorf("a ledger started after the run was accepted: %v", err)
	}
	if err := checkRestart(1000, 2000, []string{"c1500"}); err == nil || !strings.Contains(err.Error(), "c1500") {
		t.Errorf("a cycle open across the start was accepted: %v", err)
	}
}

// The loop's start is given a bounded time: a recovery that keeps failing
// (its retries wait on the wall clock) ends with an error instead of
// spinning; a start that returns in time leaves its context alive for the
// rest of the replay.
func TestStartIsBounded(t *testing.T) {
	_, err := startWithin(20*time.Millisecond, func(ctx context.Context) (bool, error) {
		<-ctx.Done()
		return false, ctx.Err()
	})
	if err == nil {
		t.Fatal("a start that never ends returned no error")
	}
	var kept context.Context
	if _, err := startWithin(20*time.Millisecond, func(ctx context.Context) (bool, error) { kept = ctx; return false, nil }); err != nil {
		t.Fatal(err)
	}
	time.Sleep(40 * time.Millisecond)
	if kept.Err() != nil {
		t.Error("the context of a start that returned in time was cancelled")
	}
}
