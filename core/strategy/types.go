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
	"strconv"
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
	EntryIn      float64   `yaml:"entry_in"`      // entry line: this many sigmas from the mean
	EntryOut     float64   `yaml:"entry_out"`     // exit line on the entry's axis, in sigmas; negative is past the mean. With Exit2 it is exit1, the line of the half close (SPEC 12.6)
	// Exit2 is the second exit line on the entry's axis, in sigmas, where all
	// that is left closes (SPEC 12.6); nil is one exit line, EntryOut.
	Exit2 *float64 `yaml:"exit2,omitempty"`

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
	// RewarmOnAnchorChange true: when the NAV replaces the bell anchor (as
	// soon as both legs' NAV are read, PRD 11.3) entries wait for a new
	// warm-up. A later NAV-to-NAV change within cex.nav_watch is a hot switch
	// when the spread jump at the current mids is below cex.nav_hot_switch_d,
	// and a warm-up again otherwise.
	RewarmOnAnchorChange bool `yaml:"rewarm_on_anchor_change"`
}

// DefaultConfig holds the values a key left out of config.yaml keeps. The
// model, the two windows, entry_in, entry_out and the taker fee have no
// default and must be written; EntryOut and TakerFeeBps start as NaN so that
// a missing key differs from a written 0. exit2 is absent by default.
var DefaultConfig = Config{
	EntryOut:              math.NaN(),
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

// Exit2Text is the second exit line for a log line: the number as written,
// or "none" for one exit line.
func (c Config) Exit2Text() string {
	if c.Exit2 == nil {
		return "none"
	}
	return strconv.FormatFloat(*c.Exit2, 'f', -1, 64)
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

	// The lines: -entry_in <= exit2 <= entry_out < entry_in, so an exit is
	// never beyond the opposite entry line and never at or outside the entry
	// line itself, and the second exit line is not before the first.
	inOK := c.EntryIn > 0 && !math.IsInf(c.EntryIn, 0)
	if !inOK {
		errs = append(errs, fmt.Errorf("strategy.entry_in is required and must be positive, got %v", c.EntryIn))
	}
	outOK := false
	switch {
	case math.IsNaN(c.EntryOut):
		errs = append(errs, errors.New("strategy.entry_out is required"))
	case inOK && (c.EntryOut < -c.EntryIn || c.EntryOut >= c.EntryIn):
		errs = append(errs, fmt.Errorf("strategy.entry_out must satisfy -entry_in <= entry_out < entry_in, got entry_out %v with entry_in %v", c.EntryOut, c.EntryIn))
	default:
		outOK = inOK
	}
	if x := c.Exit2; x != nil && outOK && !(*x >= -c.EntryIn && *x <= c.EntryOut) {
		errs = append(errs, fmt.Errorf("strategy.exit2 must satisfy -entry_in <= exit2 <= entry_out, got exit2 %v with entry_in %v and entry_out %v", *x, c.EntryIn, c.EntryOut))
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

	// Thin leg first: a batch in pairing (SPEC 12.4) and a line exit batch
	// (SPEC 12.6, 12.7). ReasonStopping and ReasonTooSmall above serve too.
	ReasonForced     Reason = "forced"      // the forced exit before the close takes the round over
	ReasonWindow     Reason = "window"      // pair_window has passed since the thin leg's first fill: the unpaired thin leg is unwound
	ReasonStopD      Reason = "stop_d"      // the ideally paired spread is pair_stop_d worse than at the decision: the unpaired thin leg is unwound
	ReasonProfit     Reason = "profit"      // the unpaired thin leg is closed on its own at or above break-even
	ReasonPairBound  Reason = "pair_bound"  // waits: no level of the thick leg is within the batch's bound p*
	ReasonValuation  Reason = "valuation"   // the ledger could not value the close (CloseNet failed)
	ReasonNoProfit   Reason = "no_profit"   // exit1: closing the whole round now would not make a profit, so no half target is set
	ReasonPrecheck   Reason = "precheck"    // exit1: this batch's pre-check does not reach the margin
	ReasonHalfDone   Reason = "half_done"   // exit1: the half target is closed; the rest waits for exit2
	ReasonD1Loss     Reason = "d1_loss"     // exit2 or the one line: the pre-check failed and the batch exits all the same (D1)
	ReasonExitWindow Reason = "exit_window" // exit_pair_window has passed since the batch's first thin closing fill (D2)
	ReasonExitStopD  Reason = "exit_stop_d" // the ideal closing spread is pair_stop_d worse than at the exit decision (D2)
	ReasonExitBound  Reason = "exit_bound"  // waits: no level of the thick leg is within its break-even bound
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
// the spacing rule. With a thin leg first it is the last batch whose thin
// order was sent, however that batch ended, entry_failed included (PRD 5.8,
// SPEC 12.3); a top-up inside a batch does not change it.
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

	// The two-stage exit and the thin leg first (SPEC 12.6, 12.7). Without
	// exit2 and without a thin leg none of them is read.
	Pairing   Pairing     // the thin-leg-first keys; the zero value sends both legs together
	Stage     ExitStage   // the half target of exit1 as stored; the zero value is none set
	BuiltLots int         // how many batches of the cycle are built now: a stage set at another count is void
	Value     CloseValuer // the open cycle's CloseNet; read with exit2 set or a thin leg first
}

// ExitPlan is one pair of reduce-only exit orders, one per leg. A leg whose
// quantity is zero gets no order: it holds nothing, or less than the venue
// takes. With a thin leg first the thin leg's order goes alone and the
// thick leg follows its fills (ExitPairStep).
type ExitPlan struct {
	Side         cex.Side           // of both orders
	Price, Qty   [2]decimal.Decimal // per leg: the limit (the price of the leg's pricing level) and the size
	X, Mu, Sigma float64            // the executable deviation, the mean and sigma, in spread units under the current anchors

	Line ExitLine // the line reached
	D    float64  // the spread at the decision, X + Mu: the reference of the exit's spread stop
	// ExpectedNet is the batch's pre-check (SPEC 12.7 step 0): the round's
	// expected net in USDT were this batch closed at these limits. It is
	// computed at exit1 and with a thin leg first; zero otherwise.
	ExpectedNet decimal.Decimal
	// D1 says the pre-check failed and the batch exits all the same (exit2
	// or the one line, thin leg first): the thick leg is then limited by
	// Protect, its pricing-level price at this decision, and not by its
	// break-even bound.
	D1      bool
	Protect decimal.Decimal
}

// ExitLine names the exit line a decision reached (SPEC 12.6).
type ExitLine string

const (
	ExitLineOne ExitLine = "exit"  // the one line, entry_out, without exit2
	ExitLine1   ExitLine = "exit1" // entry_out with exit2 set: the half close
	ExitLine2   ExitLine = "exit2" // all that is left closes
)

// ExitStage is the half target of exit1 (SPEC 12.6), as exec stores it in
// trade.exit_stage.
type ExitStage struct {
	Set       bool
	Target    [2]decimal.Decimal // H per leg: half the thin leg's holding, the other leg by the pairing anchors
	Closed    [2]decimal.Decimal // what the line exit batches closed towards H so far
	BuiltLots int                // ExitInput.BuiltLots when H was set: a batch built after it voids H
}

// ExitDecision is the outcome of one exit decision at the line. Stage is a
// half target set at this decision, which exec stores even when Plan is nil
// (the batch's pre-check may fail in the same frame).
type ExitDecision struct {
	Plan   *ExitPlan
	Reason Reason
	Stage  *ExitStage
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

// --- thin leg first (SPEC 12) ---

// CloseLeg is one leg of a close a CloseValuer values: Qty at Price, in
// USDT a share. A zero Qty closes nothing on that leg.
type CloseLeg struct {
	Qty, Price decimal.Decimal
}

// CloseValuer values a close not sent yet as the ledger would book it now
// and returns the net in USDT (SPEC 12.2 CloseNet): inventory's CloseNet with
// the batch's lot bound by the caller. With cex.PurposeExit it is the round's
// expected net (first in, first out over the cycle, what the cycle has
// realised included); with cex.PurposeTrim the increment of this close of
// the lot alone. feeRate is the taker fee as a fraction of the traded value.
// It reads the ledger and changes nothing; fills already booked count once.
type CloseValuer func(purpose cex.OrderPurpose, close [2]CloseLeg, feeRate decimal.Decimal) (decimal.Decimal, error)

// Pairing is what the thin-leg-first decisions read of the exec section
// (SPEC 12.8). The zero value sends both legs together, as before.
type Pairing struct {
	ThinFirst  bool            // exec.thin_leg is set
	Thin       cex.Leg         // the leg taken first: its order goes alone and the other leg follows its fills
	Window     time.Duration   // pair_window: how long the thick leg of an entry batch may be chased
	StopD      float64         // pair_stop_d: the ideally paired spread this many spread units worse than at the decision ends the chase
	TopupMax   decimal.Decimal // thin_topup_max: the thin leg's fills of one batch stay within this multiple of its planned quantity
	ExitWindow time.Duration   // exit_pair_window: how long the thick leg of an exit batch may wait for its break-even bound
}

// Fills is what one batch's two legs have filled so far: an entry batch's
// opening fills, or an exit batch's closing fills.
type Fills struct {
	ThinQty, ThinVWAP   decimal.Decimal // S and v_S
	ThickQty, ThickVWAP decimal.Decimal // L and v_L
}

// --- thin leg first: entry (SPEC 12.4) ---

// PairBatch is the state of one entry batch in pairing, as exec keeps it
// from the batch's orders. It starts at NewPairBatch.
type PairBatch struct {
	Dir       Direction
	DecisionD float64         // D_dec: the spread at both legs' pricing levels at the decision, under the anchors of that moment
	XRef      float64         // the top-up reference: x_dec, then the x of the last top-up that filled
	FeeD      float64         // C: the round-trip fee at the decision's limits, in spread units (EntryPlan.FeeD)
	PlanThin  decimal.Decimal // qS: the planned thin quantity
	Fills                     // the opening fills of both legs so far: S, v_S, L, v_L
	// ThinClosed is the thin quantity the batch closed again: profit closes
	// and unwinds of the unpaired part.
	ThinClosed decimal.Decimal
	FirstFill  time.Time // local arrival of the thin leg's first fill report: the pair window runs from here
	Unwind     Reason    // set by exec when PairStep starts an unwind (window, stop_d, too_small): from then on only the unpaired part is reduced
}

// PairInput is every fact one step of a batch in pairing reads: one
// snapshot of both books and of the ledger per round.
type PairInput struct {
	Batch       PairBatch
	Pairing     Pairing
	Books       [2]*cex.Book
	DepthOK     [2]bool // per leg: the depth is usable and has no conflict
	Rules       [2]cex.MarketRule
	F           float64
	Anchors     cex.Anchors // the current anchors: the spread
	PairAnchors cex.Anchors // the cycle's pairing anchors: the thick target
	Stats       Stats
	// QuotaLeft is the quota left without the unpaired thin leg (inventory
	// Quota with WithThinLeg), in USDT.
	QuotaLeft decimal.Decimal
	Value     CloseValuer // CloseNet bound to this batch's lot; a profit close values cex.PurposeTrim

	OrderPending bool // an order of the batch has no result yet, or its outcome is unknown
	Stopping     bool // a stop signal: the round is taken over
	Forced       bool // inside the forced exit before the close: the round is taken over
	NoNewEntries bool // the signal that stops new entries: no top-up, the pairing goes on
	Now          time.Time
}

// PairKind is what one step of a batch in pairing does.
type PairKind string

const (
	PairWait      PairKind = "wait"       // nothing to send this round
	PairHandOver  PairKind = "hand_over"  // the stop or the forced exit flattens the whole round
	PairDone      PairKind = "done"       // the batch has ended: End is its end state
	PairSendThick PairKind = "send_thick" // an IOC on the thick leg that pairs the thin fills
	PairSendThin  PairKind = "send_thin"  // a top-up IOC on the thin leg
	PairCloseThin PairKind = "close_thin" // a reduce-only IOC closing part of the unpaired thin leg at or above break-even
	PairUnwind    PairKind = "unwind"     // a reduce-only IOC unwinding the unpaired thin leg at PRD 10.4's price
)

// PairEnd is the end state of a batch (SPEC 12.4); the values are
// inventory's LotEnd strings.
type PairEnd string

const (
	PairEndBuilt         PairEnd = "built"          // the unpaired part was paired, or only dust of it is left
	PairEndBuiltPartial  PairEnd = "built_partial"  // part was paired; the rest was unwound or closed at a profit
	PairEndUnwoundProfit PairEnd = "unwound_profit" // nothing was paired; the thin leg was closed at a profit
	PairEndUnwoundStop   PairEnd = "unwound_stop"   // nothing was paired; the thin leg was unwound
)

// PairAction is one step's decision. Leg, Side, Price and Qty describe the
// order to send and are zero without one; the rest is the decision's detail
// for the record.
type PairAction struct {
	Kind       PairKind
	Why        Reason // the reason of an unwind, a hand-over, a profit close or a wait
	Leg        cex.Leg
	Side       cex.Side
	Price, Qty decimal.Decimal
	QuotaPrice decimal.Decimal // a thick order or a top-up: the price its notional counts against the quota at
	End        PairEnd         // PairDone only

	X, XRef   float64         // x at both legs' pricing levels now, and the top-up reference; NaN when x has no number
	DIdeal    float64         // the ideally paired spread; NaN when the thick leg has no pricing level
	VWAPCap   decimal.Decimal // P*: the bound of the batch's thick VWAP; zero without a mean
	PStar     decimal.Decimal // the bound of one thick order for what is left to pair (PairBound); zero without a mean
	BreakEven decimal.Decimal // b*: the break-even price of a profit close, rounded to the tick
	Err       error           // the valuation's error with ReasonValuation
}

// --- thin leg first: exit (SPEC 12.7) ---

// ExitBatch is the state of one line exit batch with a thin leg first, as
// exec keeps it from the batch's orders. It starts at NewExitBatch.
type ExitBatch struct {
	Dir         Direction // of the cycle; the orders close it
	Line        ExitLine
	DecisionD   float64         // the spread at the exit decision: the reference of the spread stop
	ExpectedNet decimal.Decimal // the pre-check at the decision, for the record
	D1          bool            // the pre-check failed and the batch exits all the same
	Protect     decimal.Decimal // with D1: the thick leg's pricing-level price at the decision, the limit of every thick order of the batch
	Fills                       // the batch's closing fills: thin closed and its VWAP, thick closed and its VWAP
	FirstFill   time.Time       // local arrival of the batch's first thin closing fill: exit_pair_window runs from here and is not reset
	HardEnd     Reason          // set by exec when ExitPairStep starts the hard close (exit_window, exit_stop_d)
}

// ExitPairInput is every fact one step of an exit batch's thick leg reads.
type ExitPairInput struct {
	Batch       ExitBatch
	Pairing     Pairing
	Hold        [2]decimal.Decimal // the cycle's quantity left per leg
	Books       [2]*cex.Book
	DepthOK     [2]bool
	Rules       [2]cex.MarketRule
	F           float64
	Anchors     cex.Anchors
	PairAnchors cex.Anchors
	Value       CloseValuer // the open cycle's CloseNet, read with cex.PurposeExit

	OrderPending bool
	Stopping     bool
	Forced       bool
	Now          time.Time
}

// ExitPairKind is what one step of an exit batch's thick leg does.
type ExitPairKind string

const (
	ExitPairWait      ExitPairKind = "wait"       // nothing to send this round
	ExitPairHandOver  ExitPairKind = "hand_over"  // the stop or the forced exit flattens the whole round
	ExitPairDone      ExitPairKind = "done"       // the thick leg matches the thin leg's closing fills, or only dust is left
	ExitPairSendThick ExitPairKind = "send_thick" // a reduce-only IOC on the thick leg within its bound
	ExitPairHardClose ExitPairKind = "hard_close" // a reduce-only IOC at PRD 10.4's price, whatever the bound (D2)
)

// ExitPairAction is one step's decision about an exit batch's thick leg.
type ExitPairAction struct {
	Kind       ExitPairKind
	Why        Reason
	Leg        cex.Leg
	Side       cex.Side
	Price, Qty decimal.Decimal
	Bound      decimal.Decimal // the limit the thick leg is held to: its break-even bound or, with D1, Protect
	DIdeal     float64         // the ideal closing spread; NaN when the thick leg has no pricing level
	Err        error           // the valuation's error with ReasonValuation
}
