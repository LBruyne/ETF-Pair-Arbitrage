// Package strategy is the pure computation of the pair strategy: effective
// levels, the spread, the mean and sigma, the entry and exit decisions. It
// reads no clock, holds no state, opens no connection and writes no log; its
// inputs and outputs are values.
package strategy

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/shopspring/decimal"
)

// --- configuration ---

// Config is the `strategy:` section of config.yaml. Keys ending in _d are in
// spread units (one unit is 0.5 bps of the two legs' combined notional); keys
// ending in _bps are basis points of a notional or a price.
type Config struct {
	MeanModel    MeanModel `yaml:"mean_model"`    // how the mean of the spread is taken
	MeanMinutes  int       `yaml:"mean_minutes"`  // X: the span of the mean, in minutes
	SigmaMinutes int       `yaml:"sigma_minutes"` // Y: the window of the standard deviation, in minutes
	DIn          float64   `yaml:"d_in"`          // entry line: this many sigmas from the mean
	DOut         float64   `yaml:"d_out"`         // exit line on the entry's axis, in sigmas; negative is past the mean

	TakerFeeBps float64 `yaml:"taker_fee_bps"` // the trading account's taker fee per leg, of the traded notional
	CostMarginD float64 `yaml:"cost_margin_d"` // what an entry must clear on top of the round-trip fee, in spread units

	LevelBucket decimal.Decimal `yaml:"level_bucket"`  // raw book levels fold into price buckets of this size, in USDT
	LevelMinQty decimal.Decimal `yaml:"level_min_qty"` // a bucket holding fewer shares than this is skipped
	TakeRatio   decimal.Decimal `yaml:"take_ratio"`    // the share of the first two effective levels one batch takes

	EntrySpacing     time.Duration `yaml:"entry_spacing"`     // a same-side entry sooner than this after the last one needs EntryStepD more
	EntryStepD       float64       `yaml:"entry_step_d"`      // that extra deviation, in spread units
	CompletionWindow time.Duration `yaml:"completion_window"` // how long after a batch is sent its short leg may still be completed

	ForcedSlipBps         float64       `yaml:"forced_slip_bps"`          // forced exits and trims price this far past the best price, of each leg's price
	ForcedExitBeforeClose time.Duration `yaml:"forced_exit_before_close"` // positions are closed this long before the regular close
	RewarmOnAnchorChange  bool          `yaml:"rewarm_on_anchor_change"`  // true: entries wait for a new warm-up after the anchor changes from bell to NAV
}

// DefaultConfig holds the values a key left out of config.yaml keeps. The
// model, the two windows, d_in, d_out and the taker fee have no default and
// must be written; DOut and TakerFeeBps start as NaN so that a missing key
// differs from a written 0.
var DefaultConfig = Config{
	DOut:                  math.NaN(),
	TakerFeeBps:           math.NaN(),
	LevelBucket:           decimal.RequireFromString("0.01"),
	LevelMinQty:           decimal.NewFromInt(1),
	TakeRatio:             decimal.RequireFromString("0.5"),
	EntrySpacing:          60 * time.Second,
	EntryStepD:            1,
	CompletionWindow:      5 * time.Second,
	ForcedSlipBps:         10,
	ForcedExitBeforeClose: time.Minute,
}

// MeanModel names how the mean of the spread is taken.
type MeanModel string

const (
	MeanEMA MeanModel = "ema" // exponential, seeded with the first minute of the run
	MeanSMA MeanModel = "sma" // arithmetic mean of the last MeanMinutes minutes
)

// Validate rejects parameters the strategy could not run with. Every problem
// is reported, each starting with its yaml path.
func (c Config) Validate() error {
	var errs []error
	if c.MeanModel != MeanEMA && c.MeanModel != MeanSMA {
		errs = append(errs, fmt.Errorf("strategy.mean_model is required: %s or %s, got %q", MeanEMA, MeanSMA, c.MeanModel))
	}
	if c.MeanMinutes < 1 {
		errs = append(errs, fmt.Errorf("strategy.mean_minutes is required and must be positive, got %d", c.MeanMinutes))
	}
	if c.SigmaMinutes < 1 {
		errs = append(errs, fmt.Errorf("strategy.sigma_minutes is required and must be positive, got %d", c.SigmaMinutes))
	}

	// The two lines: -d_in <= d_out < d_in, so the exit is never beyond the
	// opposite entry line and never at or outside the entry line itself.
	dInOK := c.DIn > 0 && !math.IsInf(c.DIn, 0)
	if !dInOK {
		errs = append(errs, fmt.Errorf("strategy.d_in is required and must be positive, got %v", c.DIn))
	}
	switch {
	case math.IsNaN(c.DOut):
		errs = append(errs, errors.New("strategy.d_out is required"))
	case dInOK && (c.DOut < -c.DIn || c.DOut >= c.DIn):
		errs = append(errs, fmt.Errorf("strategy.d_out must satisfy -d_in <= d_out < d_in, got d_out %v with d_in %v", c.DOut, c.DIn))
	}

	// The fee is the trading account's own rate: it enters the entry
	// condition and every fee estimate, so it is never assumed.
	switch {
	case math.IsNaN(c.TakerFeeBps):
		errs = append(errs, errors.New("strategy.taker_fee_bps is required"))
	case c.TakerFeeBps < 0 || math.IsInf(c.TakerFeeBps, 0):
		errs = append(errs, fmt.Errorf("strategy.taker_fee_bps must not be negative, got %v", c.TakerFeeBps))
	}
	if !(c.CostMarginD >= 0) {
		errs = append(errs, fmt.Errorf("strategy.cost_margin_d must not be negative, got %v", c.CostMarginD))
	}
	if !c.LevelBucket.IsPositive() {
		errs = append(errs, fmt.Errorf("strategy.level_bucket must be positive, got %s", c.LevelBucket))
	}
	if c.LevelMinQty.IsNegative() {
		errs = append(errs, fmt.Errorf("strategy.level_min_qty must not be negative, got %s", c.LevelMinQty))
	}
	if !c.TakeRatio.IsPositive() || c.TakeRatio.GreaterThan(decimal.NewFromInt(1)) {
		errs = append(errs, fmt.Errorf("strategy.take_ratio must be above 0 and at most 1, got %s", c.TakeRatio))
	}
	if c.EntrySpacing < 0 {
		errs = append(errs, fmt.Errorf("strategy.entry_spacing must not be negative, got %s", c.EntrySpacing))
	}
	if !(c.EntryStepD >= 0) {
		errs = append(errs, fmt.Errorf("strategy.entry_step_d must not be negative, got %v", c.EntryStepD))
	}
	if c.CompletionWindow <= 0 {
		errs = append(errs, fmt.Errorf("strategy.completion_window must be positive, got %s", c.CompletionWindow))
	}
	if !(c.ForcedSlipBps >= 0) {
		errs = append(errs, fmt.Errorf("strategy.forced_slip_bps must not be negative, got %v", c.ForcedSlipBps))
	}
	if c.ForcedExitBeforeClose <= 0 {
		errs = append(errs, fmt.Errorf("strategy.forced_exit_before_close must be positive, got %s", c.ForcedExitBeforeClose))
	}
	return errors.Join(errs...)
}
