// Package exec decides and sends every order: one event loop reads the facts
// from cex, computes with strategy, books through inventory and places orders
// through cex. The minute bars, the batch being worked and the stop state
// belong to that loop alone.
package exec

import (
	"errors"
	"fmt"
	"time"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/inventory"
	"etf-pair-arbitrage/core/strategy"
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

// --- the loop's state ---

// batchState is the batch of orders being worked: an entry batch, one pair
// of exit orders, or the forced exit's orders. A batch's orders are read from
// the cycle's order snapshot by their ids; the batch itself keeps only what
// the snapshot does not say.
type batchState struct {
	kind      cex.OrderPurpose    // entry, exit or forced: what opened the batch
	orders    []cex.ClientOrderID // the orders of the batch, in the order they were sent
	sentAt    time.Time           // when its first orders were handed to cex: the completion window and the entry spacing start here
	doneAt    time.Time           // the round that first saw every order of the batch with a final state; zero while one has none
	rejected  bool                // at doneAt, the venue had refused one of its orders
	lotID     inventory.LotID     // entry batches: the lot the batch builds
	direction strategy.Direction  // entry batches: the direction of the lot
	planPrice [2]decimal.Decimal  // entry batches: the two limit prices
	x         float64             // entry batches: the executable deviation at the decision, the e of the spacing rule
	settled   bool                // entry batches: the lot was settled, built or void
}

// observation is the last usable moment of the open minute: both legs'
// depth usable and F fresh (PRD 4.2).
type observation struct {
	midBull, midBear float64 // the raw best bid and ask midpoints, USDT
	f                float64
}

// stopState is what the process signals asked for.
type stopState struct {
	noEntries bool      // SIGUSR1: no new entries, positions are managed as usual
	stopping  bool      // SIGINT or SIGTERM: close everything, then leave
	at        time.Time // when the stop was asked for
}

// round is the facts of one step, read once at its start: every check and
// every computation of the step uses these values, so a publication during
// the step cannot make a check pass on one version and the decision use
// another.
type round struct {
	now     time.Time
	books   cex.Books
	fx      cex.FX // the latest F; its price is the last valid value when it is stale
	fxFresh bool
	rules   [2]cex.MarketRule
	rulesOK bool              // both legs' rules are loaded
	orders  []cex.OrderRecord // the open cycle's order snapshot, as booked by this step; nil when flat
	cycle   *inventory.Cycle  // the open cycle after that booking; nil when flat
}

// atLineReasons are the reasons an entry is refused after its signal held
// (PRD 5.13 steps 6 to 9): each is recorded once as an at_line_blocked
// decision while it lasts.
var atLineReasons = map[strategy.Reason]bool{
	strategy.ReasonHoldingOtherSide: true, strategy.ReasonCostNotCovered: true, strategy.ReasonSpacing: true,
	strategy.ReasonQuotaExhausted: true, strategy.ReasonTooSmall: true,
}

// view is the statistics of one moment, in spread units, for the sample and
// the status line; NaN where there is no number (no anchor, no level, no
// minute in the run).
type view struct {
	midD, mu, sigma, xLong, xShort float64
}

// The phases of a Session as signal_sample stores them.
const (
	phaseOffHours    = "off_hours"    // from the close to the regular open: entries are allowed
	phaseRegular     = "regular"      // the regular session: exits only
	phaseCloseWindow = "close_window" // forced_exit_before_close before the close: everything is closed
)

// --- interfaces ---

// cexSource is what the loop reads from cex and the one command it gives;
// *cex.CexManager satisfies it.
type cexSource interface {
	// Books is both legs' books in one read; BookChanged is closed at the
	// next publication.
	Books() cex.Books
	BookChanged() <-chan struct{}
	// FX is the latest F; fresh is false when it is older than fx_max_age.
	FX(now time.Time) (fx cex.FX, fresh bool)
	// Anchors is the published pair of anchors, of the Session it names.
	Anchors() cex.Anchors
	// Session is the Session that contains now.
	Session(now time.Time) cex.Session
	// ReferenceChanged is closed at the next publication of F, the anchors,
	// the rules or the settings.
	ReferenceChanged() <-chan struct{}
	// Rule is a leg's contract rule; ok is false until the rules are read.
	Rule(leg cex.Leg) (cex.MarketRule, bool)
	// SettingsOK reports whether the account is one-way, crossed and at
	// this leverage on both contracts.
	SettingsOK(leverage int) (ok bool, detail string)
	// TradingReady reports whether the venue side can take orders.
	TradingReady() bool
	// Positions is the latest position snapshot; AccountChanged is closed
	// at the next publication of positions or funding records.
	Positions() cex.PositionSet
	AccountChanged() <-chan struct{}
	// PlaceOrders writes the orders' rows and sends them; OrdersChanged is
	// closed at the next change of any order record.
	PlaceOrders(reqs ...cex.OrderRequest) error
	OrdersChanged() <-chan struct{}
	// CycleOrders is one snapshot of a cycle's orders; UnresolvedOrders the
	// orders of any cycle without a final state.
	CycleOrders(cycleID string) []cex.OrderRecord
	UnresolvedOrders() []cex.OrderRecord
}

// --- timings ---

// timings are the manager's clock and its fixed intervals. They are not
// configuration (PRD 15 fixes the step and the resend interval); tests
// replace the clock.
type timings struct {
	clock func() time.Time // the loop reads the time here

	step          time.Duration // the loop looks at the facts at least this often
	resend        time.Duration // two reducing orders of one leg are at least this far apart
	rejectBackoff time.Duration // after a batch with a refused order, the next entry waits at least this long
	sample        time.Duration // one signal_sample row this often
	statusLog     time.Duration // one status line this often
	readTimeout   time.Duration // the limit of the positions read that confirms a cycle is flat
	closeRetry    time.Duration // a cycle that could not be closed is not tried again sooner than this
}

// defaultTimings are the values the process runs with.
var defaultTimings = timings{clock: time.Now, step: 200 * time.Millisecond, resend: 200 * time.Millisecond, rejectBackoff: time.Second,
	sample: time.Second, statusLog: 30 * time.Second, readTimeout: 2 * time.Second, closeRetry: time.Second}
