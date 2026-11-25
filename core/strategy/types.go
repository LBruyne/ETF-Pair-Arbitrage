// Package strategy is the pure computation of the pair strategy: the pricing
// levels, the spread, the mean and sigma, the entry and exit decisions, and
// what to do when the two legs fill unevenly. It reads no clock, holds no
// state, opens no connection and writes no log; its inputs and outputs are
// values.
package strategy

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
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

	PricingLevel LegLevels       `yaml:"pricing_level"` // per leg: the number of the raw level on the side to take a line order is priced on
	TakeRatio    decimal.Decimal `yaml:"take_ratio"`    // the share of the quantity down to that level one batch takes
	// MinBatchNotional is the least an entry batch may be worth, both legs together at their limit prices, in USDT.
	MinBatchNotional decimal.Decimal `yaml:"min_batch_notional"`

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
	PricingLevel:          LegLevels{Bull: 2, Bear: 5},
	TakeRatio:             decimal.RequireFromString("0.5"),
	MinBatchNotional:      decimal.RequireFromString("100"),
	EntrySpacing:          60 * time.Second,
	EntryStepD:            1,
	CompletionWindow:      5 * time.Second,
	ForcedSlipBps:         10,
	ForcedExitBeforeClose: time.Minute,
}

// LegLevels is a level number per leg, 1 for the best level.
type LegLevels struct {
	Bull int `yaml:"bull"`
	Bear int `yaml:"bear"`
}

// of returns the level number of the given leg.
func (l LegLevels) of(leg cex.Leg) int {
	if leg == cex.Bull {
		return l.Bull
	}
	return l.Bear
}

// maxPricingLevel is the deepest pricing level: both legs subscribe books5,
// which carries five levels a side.
const maxPricingLevel = 5

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
	for _, leg := range cex.Legs {
		if n := c.PricingLevel.of(leg); n < 1 || n > maxPricingLevel {
			errs = append(errs, fmt.Errorf("strategy.pricing_level.%s must be 1 to %d, got %d", leg, maxPricingLevel, n))
		}
	}
	if c.MinBatchNotional.IsNegative() {
		errs = append(errs, fmt.Errorf("strategy.min_batch_notional must not be negative, got %s", c.MinBatchNotional))
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

// --- identifiers ---

// Direction is the way a cycle trades the pair: both legs are bought or both
// are sold.
type Direction string

const (
	Long  Direction = "long"  // both legs bought; closed by selling both
	Short Direction = "short" // both legs sold; closed by buying both
)

// Opposite returns the other direction: the one whose orders close d.
func (d Direction) Opposite() Direction {
	if d == Long {
		return Short
	}
	return Long
}

// Side is the side of the orders that open a position in direction d.
func (d Direction) Side() cex.Side {
	if d == Long {
		return cex.SideBuy
	}
	return cex.SideSell
}

// Reason says why no order was decided at one moment. The values are the
// strings stored and logged.
type Reason string

// The entry checks run in this order and the first one that fails is the
// reason. Steps 1 to 3 are facts the caller gathers and passes in
// EntryInput.Blocked; the later steps are found by EvaluateEntry.
const (
	// Step 1: the program and the account.
	ReasonStopping         Reason = "stopping"          // a stop signal was received
	ReasonNoNewEntries     Reason = "no_new_entries"    // the signal that stops new entries was received
	ReasonNotReady         Reason = "not_ready"         // contract rules, connection or positions are not read yet
	ReasonSettings         Reason = "settings"          // the account settings differ from what the strategy needs
	ReasonStoreError       Reason = "store_error"       // records cannot be written
	ReasonCapitalPending   Reason = "capital_pending"   // a changed capital waits for the open cycle to close: no new lot until then
	ReasonPositionMismatch Reason = "position_mismatch" // the venue's positions differ from the ledger

	// Step 2: the trading hours and the anchors.
	ReasonRegularHours Reason = "regular_hours" // entries are made off-hours only
	ReasonNoAnchor     Reason = "no_anchor"     // the Session has no anchor yet
	ReasonActionWindow Reason = "action_window" // inside a dividend or split window

	// Step 3: the data.
	ReasonDepthBull       Reason = "depth_bull"       // the bull leg's depth is not usable
	ReasonDepthBear       Reason = "depth_bear"       // the bear leg's depth is not usable
	ReasonConflictBull    Reason = "conflict_bull"    // the bull leg's best price contradicts its depth
	ReasonConflictBear    Reason = "conflict_bear"    // the bear leg's best price contradicts its depth
	ReasonFXStale         Reason = "fx_stale"         // F is too old
	ReasonAwaitDepth      Reason = "await_depth"      // no depth has arrived yet after the last batch's results
	ReasonAwaitOrders     Reason = "await_orders"     // an order is still in flight
	ReasonOrderUnknown    Reason = "order_unknown"    // the outcome of an order is not known
	ReasonRejectedBackoff Reason = "rejected_backoff" // the venue refused an order of the last batch a moment ago
	ReasonPositionsRead   Reason = "positions_read"   // the round read the venue's positions, which takes up to seconds: the next round decides on facts read after it

	// Step 4: the mean and sigma.
	ReasonWarmup    Reason = "warmup"     // the run of valid minutes is shorter than max(X, Y)
	ReasonZeroSigma Reason = "zero_sigma" // sigma is zero: no line to compare with

	// Step 5: the book.
	ReasonNoLevels Reason = "no_levels" // a leg has no pricing level: fewer raw levels on the side to take than its pricing_level

	// Step 6: condition 1, the signal.
	ReasonNoSignal         Reason = "no_signal"          // the deviation has not reached the line
	ReasonHoldingOtherSide Reason = "holding_other_side" // the signal is against the open cycle: reverse only after it is flat

	// Step 7: condition 2, the cost cover.
	ReasonCostNotCovered Reason = "cost_not_covered" // the distance between the two lines does not pay the round-trip fee and margin

	// Step 8: the spacing between two entries of one direction.
	ReasonSpacing Reason = "spacing"

	// Step 9: the size.
	ReasonQuotaExhausted Reason = "quota_exhausted" // the quota left does not hold a sendable batch
	ReasonTooSmall       Reason = "too_small"       // the book gives less than the venue's minimum order, or the batch less than min_batch_notional

	// Exits and forced exits.
	ReasonDepth      Reason = "depth"       // a leg's depth is not usable or in conflict: no exit at the line
	ReasonNoPrice    Reason = "no_price"    // a leg to reduce has no best price: no reducing order can be priced
	ReasonAwaitClose Reason = "await_close" // nothing is left to trade; the cycle waits for the venue's positions to confirm
)

// --- mean and sigma ---

// MinuteBar is one finished minute: the last observation in it with both
// legs' depth usable and F fresh. A minute without such an observation is
// invalid and carries no values.
type MinuteBar struct {
	Minute           time.Time // the start of the minute
	MidBull, MidBear float64   // the raw best bid and ask midpoint of each leg, in USDT
	F                float64   // USD per USDT at that observation
	Valid            bool
}

// Stats is the mean and the sigma window of one run of valid minutes, kept
// as per-leg USD prices (F x mid). It holds no anchor: Mu and Sigma express
// it under whichever pair of anchors is current, so an anchor change needs
// no second set of numbers.
type Stats struct {
	Ready                    bool         // the run holds at least max(X, Y) minutes
	AsOf                     time.Time    // the start of the last minute in the run; zero for an empty run
	MeanBullUSD, MeanBearUSD float64      // the EMA or SMA of F x mid, per leg
	Window                   [][2]float64 // F x mid of the last Y minutes of the run, per leg, oldest first
}

// --- entry ---

// EntryInput is every fact one entry decision reads.
type EntryInput struct {
	Blocked         Reason            // the first failing check of steps 1 to 3, gathered by the caller; "" when none
	Books           [2]*cex.Book      // both legs' books, depth usable and without conflict
	Rules           [2]cex.MarketRule // both legs' contract rules
	F               float64           // USD per USDT, fresh
	Anchors         cex.Anchors       // the current anchors: the signal and the cost cover
	PairAnchors     cex.Anchors       // the cycle's pairing anchors: the sizes; equal to Anchors when flat
	Stats           Stats             // of the current run of minutes
	Holding         Direction         // the open cycle's direction; "" when flat
	QuotaLeft       decimal.Decimal   // the notional both legs together may still add, in USDT
	Last            *LastEntry        // the last built entry batch; nil when there has been none
	AnchorChangedAt time.Time         // when the anchors last changed; zero when they have not
	Now             time.Time
}

// LastEntry is the last entry batch that was built, kept across cycles for
// the spacing rule.
type LastEntry struct {
	At        time.Time // when the batch's orders were sent
	E         float64   // the executable deviation x at that decision, in spread units
	Direction Direction
}

// EntryPlan is one entry batch: an IOC limit order per leg, sent together.
type EntryPlan struct {
	Direction    Direction
	Price, Qty   [2]decimal.Decimal // per leg: the limit (the price of the leg's pricing level) and the size
	QuotaPrice   [2]decimal.Decimal // per leg: the price the order's notional counts against the quota at (see quotaPrice)
	X, Mu, Sigma float64            // the executable deviation, the mean and sigma, in spread units under the current anchors
	FeeD         float64            // the round-trip fee at these limit prices, in spread units
	FeeUSDT      decimal.Decimal    // the round-trip fee of the planned sizes at these limit prices, in USDT
	K            decimal.Decimal    // the anchored amount per leg before rounding to the quantity step, in USD
}

// --- exit ---

// ExitInput is every fact one exit decision at the line reads.
type ExitInput struct {
	Direction   Direction          // of the open cycle
	Hold        [2]decimal.Decimal // the quantity left per leg
	Books       [2]*cex.Book
	DepthOK     [2]bool // per leg: the depth is usable and has no conflict
	Rules       [2]cex.MarketRule
	F           float64     // USD per USDT; the last valid value while F is stale
	Anchors     cex.Anchors // the current anchors: the exit line
	PairAnchors cex.Anchors // the cycle's pairing anchors: the sizes
	Stats       Stats       // of the current run; the last ready one while the mean warms up again
}

// ExitPlan is one pair of reduce-only exit orders, one per leg. A leg whose
// quantity is zero gets no order: it holds nothing, or less than the venue
// takes.
type ExitPlan struct {
	Side         cex.Side           // of both orders
	Price, Qty   [2]decimal.Decimal // per leg: the limit (the price of the leg's pricing level) and the size
	X, Mu, Sigma float64            // the executable deviation, the mean and sigma, in spread units under the current anchors
}

// LegOrder is one reduce-only order of a forced exit.
type LegOrder struct {
	Side       cex.Side
	Price, Qty decimal.Decimal
}

// --- uneven fills ---

// GapInput is every fact one decision about the cycle's gap reads. The gap
// is the difference between the two legs' anchored amounts (quantity x
// pairing anchor).
type GapInput struct {
	Direction   Direction          // of the cycle
	Hold        [2]decimal.Decimal // the cycle's quantity per leg, from the ledger synced with the order snapshot
	PairAnchors cex.Anchors        // the cycle's pairing anchors
	Books       [2]*cex.Book       // read level by level, as received
	HasPrice    [2]bool            // per leg: a best price recent enough to price a reducing order
	DepthOK     [2]bool            // per leg: the depth is usable and agrees with the best price (no conflict); otherwise only the best price is read
	Rules       [2]cex.MarketRule
	Batch       *GapBatch       // the entry batch the gap came from; nil when it came from an exit, which only reduces
	CanComplete [2]bool         // per leg: adding to it is allowed (off-hours, no stop signal, settings fine, depth usable, no conflict)
	BookNew     [2]bool         // per leg: a depth or best-price frame arrived after the batch's results
	QuotaLeft   decimal.Decimal // the notional a completion may still add, in USDT
	Now         time.Time
}

// GapBatch is what the comparison of the two losses needs from the entry
// batch.
type GapBatch struct {
	PlanPrice [2]decimal.Decimal // the batch's limit prices
	AvgFill   [2]decimal.Decimal // the average price each leg filled at
	SentAt    time.Time          // when the batch's orders were first sent: the completion window starts here
}

// GapKind is what to do about the gap now.
type GapKind string

const (
	GapNone     GapKind = "none"     // no gap an order could be sent for: the batch can be settled
	GapWait     GapKind = "wait"     // a completion is allowed and only the short leg's new frame is missing
	GapBlocked  GapKind = "blocked"  // the larger leg must be reduced and has no price: look again next round
	GapComplete GapKind = "complete" // add to the leg that filled less
	GapTrim     GapKind = "trim"     // reduce the leg that filled more
)

// GapAction is the decision about the gap. Leg, Side, Price and Qty describe
// the one order of a completion or a trim and are zero otherwise.
type GapAction struct {
	Kind       GapKind
	Leg        cex.Leg
	Side       cex.Side
	Price, Qty decimal.Decimal
	QuotaPrice decimal.Decimal // a completion: the price its notional counts against the quota at (see quotaPrice); zero for a trim, which takes no quota
	// The two expected losses in USDT, for the same share of the gap. They
	// are set only when both orders could be sent and were compared.
	LossComplete, LossTrim decimal.Decimal
}
