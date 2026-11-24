// Package exec decides and sends every order: one event loop reads the facts
// from cex, computes with strategy, books through inventory and places orders
// through cex. The minute bars, the batch being worked and the stop state
// belong to that loop alone.
package exec

import (
	"errors"
	"fmt"
	"time"
)

// --- configuration ---

// Config is the `exec:` section of config.yaml.
type Config struct {
	Live            bool          `yaml:"live"`              // false: paper mode, no order leaves the process
	SimOrderLatency LegLatency    `yaml:"sim_order_latency"` // paper and replay: an order meets its leg's book this long after it is sent
	StopAlarmAfter  time.Duration `yaml:"stop_alarm_after"`  // not flat this long after a stop signal: alarm, and again every interval
}

// LegLatency is one duration per leg.
type LegLatency struct {
	Bull time.Duration `yaml:"bull"`
	Bear time.Duration `yaml:"bear"`
}

// DefaultConfig holds the values a key left out of config.yaml keeps: paper
// mode and the stop alarm. sim_order_latency has no default and must be
// written.
var DefaultConfig = Config{StopAlarmAfter: 60 * time.Second}

// Validate rejects a configuration the manager could not run with. Every
// problem is reported, each starting with its yaml path.
func (c Config) Validate() error {
	var errs []error
	if c.SimOrderLatency.Bull <= 0 {
		errs = append(errs, fmt.Errorf("exec.sim_order_latency.bull is required and must be positive, got %s", c.SimOrderLatency.Bull))
	}
	if c.SimOrderLatency.Bear <= 0 {
		errs = append(errs, fmt.Errorf("exec.sim_order_latency.bear is required and must be positive, got %s", c.SimOrderLatency.Bear))
	}
	if c.StopAlarmAfter <= 0 {
		errs = append(errs, fmt.Errorf("exec.stop_alarm_after must be positive, got %s", c.StopAlarmAfter))
	}
	return errors.Join(errs...)
}
