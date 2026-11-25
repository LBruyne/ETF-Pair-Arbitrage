package main

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"etf-pair-arbitrage/core/store"
)

// flakyLoader fails until ok is set.
type flakyLoader struct{ ok atomic.Bool }

func (l *flakyLoader) LoadState(context.Context, string) (store.State, error) {
	if l.ok.Load() {
		return store.State{Inventory: &store.InventoryStateRow{}}, nil
	}
	return store.State{}, errors.New("the database is down")
}

// While the stored state cannot be read, a stop signal waits for exec; a
// repeat before the stop alarm changes nothing, one after it is a takeover
// and the process leaves. When the state is read, the stop signal is back
// in the channel for exec.
func TestLoadStateWithAStop(t *testing.T) {
	const alarm, retry = 150 * time.Millisecond, 10 * time.Millisecond
	t.Run("takeover after the alarm", func(t *testing.T) {
		signals, done := make(chan os.Signal, 8), make(chan error, 1)
		go func() {
			_, err := loadState(&flakyLoader{}, "s", signals, alarm, retry)
			done <- err
		}()
		signals <- syscall.SIGTERM
		time.Sleep(30 * time.Millisecond)
		signals <- syscall.SIGTERM // before the alarm: no takeover
		select {
		case err := <-done:
			t.Fatalf("left before the alarm: %v", err)
		case <-time.After(alarm):
		}
		signals <- syscall.SIGTERM
		select {
		case err := <-done:
			if !errors.Is(err, errTakenOver) {
				t.Errorf("loadState = %v, want errTakenOver", err)
			}
		case <-time.After(time.Second):
			t.Fatal("the stop after the alarm was not a takeover")
		}
	})
	t.Run("read after a stop", func(t *testing.T) {
		signals, done := make(chan os.Signal, 8), make(chan error, 1)
		l := &flakyLoader{}
		go func() {
			state, err := loadState(l, "s", signals, alarm, retry)
			if err == nil && state.Inventory == nil {
				err = errors.New("not the state read")
			}
			done <- err
		}()
		signals <- syscall.SIGTERM
		time.Sleep(30 * time.Millisecond)
		l.ok.Store(true)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		select {
		case sig := <-signals:
			if sig != syscall.SIGTERM {
				t.Errorf("signal %v, want SIGTERM", sig)
			}
		default:
			t.Error("the stop signal was not kept for exec")
		}
	})
}
