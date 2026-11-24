package exec

import (
	"context"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// testConfig is a configuration NewManager accepts.
func testConfig() Config {
	cfg := DefaultConfig
	cfg.SimOrderLatency = LegLatency{Bull: 15 * time.Millisecond, Bear: 15 * time.Millisecond}
	return cfg
}

// newTestManager builds a manager and the signal channel it reads, as main
// does.
func newTestManager(t *testing.T) (*ExecManager, chan os.Signal) {
	t.Helper()
	signals := make(chan os.Signal, 8)
	m, err := NewManager(testConfig(), nil, nil, nil, signals)
	if err != nil {
		t.Fatal(err)
	}
	return m, signals
}

// NewManager refuses a configuration that does not validate: the defaults
// alone lack sim_order_latency.
func TestNewManagerValidates(t *testing.T) {
	if _, err := NewManager(DefaultConfig, nil, nil, nil, nil); err == nil || !strings.Contains(err.Error(), "exec.sim_order_latency.bull") {
		t.Errorf("NewManager(DefaultConfig) = %v, want the missing sim_order_latency reported", err)
	}
}

// SIGUSR1 (no new entries) leaves the loop running; SIGTERM ends it with nil.
func TestRunStopsOnStopSignalOnly(t *testing.T) {
	m, signals := newTestManager(t)
	done := make(chan error, 1)
	go func() { done <- m.Run(context.Background()) }()

	signals <- syscall.SIGUSR1
	select {
	case err := <-done:
		t.Fatalf("Run returned on SIGUSR1: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	signals <- syscall.SIGTERM
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run after SIGTERM = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return on SIGTERM")
	}
}

// A cancelled context ends Run with the context's error.
// Signals that arrived while the process was starting, before Run, are
// handled when Run begins: a stop request among them ends Run at once.
func TestStopSignalQueuedBeforeRunIsHonoured(t *testing.T) {
	m, signals := newTestManager(t)
	signals <- syscall.SIGUSR1
	signals <- syscall.SIGTERM
	done := make(chan error, 1)
	go func() { done <- m.Run(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not honour the stop signal that was queued before it started")
	}
}

func TestRunEndsWithContext(t *testing.T) {
	m, _ := newTestManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Run = %v, want context.Canceled", err)
	}
}
