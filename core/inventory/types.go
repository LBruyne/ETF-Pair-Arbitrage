// Package inventory keeps the books of the strategy: cycles and lots, their
// profit and loss, the realised total R, the quota, and the comparison with
// the positions on the exchange. It sends no order.
package inventory

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/store"
	"etf-pair-arbitrage/core/strategy"
)

// --- configuration ---

// Config is the `inventory:` section of config.yaml. Neither key has a
// default: both must be written.
type Config struct {
	Capital  decimal.Decimal `yaml:"capital"`  // M: the capital given to the strategy, in USDT
	Leverage decimal.Decimal `yaml:"leverage"` // L: the quota is L x (M + min(0, R)); both contracts must be set to it on the account
}

// Validate rejects a configuration the manager could not run with. Every
// problem is reported, each starting with its yaml path.
func (c Config) Validate() error {
	var errs []error
	if !c.Capital.IsPositive() {
		errs = append(errs, fmt.Errorf("inventory.capital is required and must be positive, got %s", c.Capital))
	}
	if !c.Leverage.IsPositive() || !c.Leverage.IsInteger() {
		errs = append(errs, fmt.Errorf("inventory.leverage is required and must be a positive whole number, got %s", c.Leverage))
	}
	return errors.Join(errs...)
}

// --- identifiers ---

// CycleID names one cycle: "c" and the unix milliseconds of its creation.
type CycleID string

// LotID names one entry batch: its cycle's id, "-" and its number within the
// cycle, counted from 1.
type LotID string

// CycleStatus is the state of a cycle, as stored in cycle.status.
type CycleStatus string

const (
	CycleOpen     CycleStatus = "open"     // from its creation until it is flat and confirmed
	CycleClosed   CycleStatus = "closed"   // finished: its totals are final, apart from funding still to arrive
	CycleTakeover CycleStatus = "takeover" // a person took the positions over; the cycle stays as it was
)

// LotStatus is the state of a lot, as stored in lot.status.
type LotStatus string

const (
	LotBuilding LotStatus = "building" // its entry batch is not settled yet
	LotOpen     LotStatus = "open"     // built: both legs filled and the gap between them is dealt with
	LotClosed   LotStatus = "closed"   // built and closed again; its result is final
	LotVoid     LotStatus = "void"     // the batch was not built; what it lost is booked all the same
)

// Why a lot is void, as stored in lot.void_reason.
const (
	VoidNoFill     = "no_fill"     // neither leg filled
	VoidOneLeg     = "one_leg"     // one leg filled and was trimmed back
	VoidStoreError = "store_error" // its row could not be confirmed written, so its orders were never sent
)

// Why a cycle ended, as stored in cycle.end_reason. exec passes one of them
// to CloseCycle; EndStoreError is written by OpenCycle alone.
const (
	EndExitLine       = "exit_line"       // closed at the exit line
	EndCloseWindow    = "close_window"    // closed before the regular close
	EndStop           = "stop"            // closed after a stop signal
	EndSessionChanged = "session_changed" // it belonged to a Session that had ended
	EndNoFill         = "no_fill"         // its only batch filled nothing
	EndOneLeg         = "one_leg"         // its only batch filled one leg and was trimmed back
	EndStoreError     = "store_error"     // its row could not be confirmed written, so it never traded
)

// --- the ledger ---

// LotLeg is one leg of a lot: what entered it and what has closed it.
// Values are traded amounts in USDT, fees are positive when paid.
type LotLeg struct {
	EntryQty, EntryValue, EntryFee decimal.Decimal // filled by entry and completion orders
	ExitQty, ExitValue, ExitFee    decimal.Decimal // closed by trims, exits and forced exits
}

// Open is the quantity of the leg not closed yet.
func (l LotLeg) Open() decimal.Decimal { return l.EntryQty.Sub(l.ExitQty) }

// Lot is one entry batch (PRD 7.1): a position once it is built, void when
// it was not. Its legs, result and status are recomputed from the cycle's
// orders; only its identity and the decision's numbers are fixed.
type Lot struct {
	ID         LotID
	CycleID    CycleID
	Seq        int // its number within the cycle, from 1
	Status     LotStatus
	VoidReason string // VoidNoFill, VoidOneLeg or VoidStoreError; empty unless void
	Direction  strategy.Direction
	Legs       [2]LotLeg // by cex.Leg

	// Realised so far, in USDT. Gross is what the closed part of each leg
	// made at the fill prices; Fees is the exit fees plus the entry fees in
	// proportion to the quantity closed; Net is Gross - Fees.
	Gross, Fees, Net decimal.Decimal

	OpenedAt time.Time // when the batch was opened
	ClosedAt time.Time // when it became closed or void; zero before

	X, Mu, Sigma, FeeD float64         // the entry decision's numbers, in spread units, for the record
	FeeUSDTEst         decimal.Decimal // the round-trip fee estimated at the decision, in USDT
}

// Cycle is one round from flat to flat (PRD section 2). All of its lots are
// sized with PairAnchors.
type Cycle struct {
	ID          CycleID
	Status      CycleStatus
	SessionID   string // the Session it was opened in
	Direction   strategy.Direction
	PairAnchors cex.Anchors // the anchors when it was opened (PRD 11.2)
	Lots        []*Lot      // oldest first

	// Realised so far, in USDT: the sums over the lots, void ones included.
	// Net is Gross - Fees + Funding.
	Gross, Fees, Net decimal.Decimal
	Funding          decimal.Decimal // the funding records booked to it; positive is received
	FundingKnown     bool            // false while a leg it held through a settlement has a non-zero or unpublished rate and no record yet

	DustLeft  [2]decimal.Decimal // by leg: the signed quantity it left unsendable when it closed
	OpenedAt  time.Time
	ClosedAt  time.Time // zero while open
	EndReason string    // one of the End constants; empty while open
}

// Hold is the quantity each leg of the cycle still holds: the open
// quantities of all its lots, building and void ones included. It is never
// negative; Direction says which way it is held.
func (c *Cycle) Hold() (hold [2]decimal.Decimal) {
	for _, lot := range c.Lots {
		for leg := range lot.Legs {
			hold[leg] = hold[leg].Add(lot.Legs[leg].Open())
		}
	}
	return hold
}

// Flat reports whether the cycle holds nothing on either leg.
func (c *Cycle) Flat() bool {
	hold := c.Hold()
	return hold[cex.Bull].IsZero() && hold[cex.Bear].IsZero()
}

// clone returns a deep copy: the lots are copied too.
func (c *Cycle) clone() *Cycle {
	cp := *c
	cp.Lots = make([]*Lot, len(c.Lots))
	for i, lot := range c.Lots {
		l := *lot
		cp.Lots[i] = &l
	}
	return &cp
}

// Dust is what one leg holds outside any cycle: the net of the remainders
// cycles left when they closed because no order could be sent for them
// (below the venue's minimum quantity or minimum value, PRD 8.1). It is only
// recorded: no fill is booked against it and it carries no realised profit
// or loss. The comparison with the venue's positions counts it.
type Dust struct {
	Qty      decimal.Decimal // signed: negative is short; zero is no dust
	AvgPrice decimal.Decimal // the entry average price of what Qty is made of, in USDT
}

// add nets one more remainder, qty (signed) entered at price, into the dust.
// On the dust's own side the quantities add and the price is their
// quantity-weighted average. Against it the quantity shrinks and what
// remains keeps its price; when the net crosses zero what remains is of the
// new remainder and takes its price.
func (d Dust) add(qty, price decimal.Decimal) Dust {
	net := d.Qty.Add(qty)
	switch {
	case net.IsZero():
		return Dust{}
	case d.Qty.Sign() == qty.Sign():
		return Dust{Qty: net, AvgPrice: d.Qty.Mul(d.AvgPrice).Add(qty.Mul(price)).Div(net).Round(allocDecimals)}
	case d.Qty.Sign() == net.Sign():
		return Dust{Qty: net, AvgPrice: d.AvgPrice}
	}
	return Dust{Qty: net, AvgPrice: price}
}

// --- funding ---

// fundingInterval is the grid of the venue's funding settlements: every
// eight hours from midnight UTC.
const fundingInterval = 8 * time.Hour

// fundingKey names one leg's funding at one settlement.
type fundingKey struct {
	settlementMs int64
	leg          cex.Leg
}

// pendingCycle is a finished cycle that held a leg through a settlement
// whose funding record has not arrived. Its whole row is kept: completing
// it rewrites the row.
type pendingCycle struct {
	row     store.CycleRow
	awaited []fundingKey // the settlements and legs still without a record or a zero rate
}

// writeFailure tracks a synchronous write that is tried again every round
// while the database cannot confirm it, so that it is logged when it starts
// failing, at most once per failLogEvery while it lasts, and when it ends.
type writeFailure struct {
	since  time.Time // when it started failing; zero while it does not fail
	logged time.Time // when it was last logged
}

// failed notes one more failure at now and reports whether to log it.
func (f *writeFailure) failed(now time.Time, every time.Duration) bool {
	if f.since.IsZero() {
		f.since, f.logged = now, now
		return true
	}
	if now.Sub(f.logged) < every {
		return false
	}
	f.logged = now
	return true
}

// ended reports whether the write had been failing, and since when, and
// forgets the failure.
func (f *writeFailure) ended() (since time.Time, was bool) {
	since, was = f.since, !f.since.IsZero()
	*f = writeFailure{}
	return since, was
}

// --- interfaces ---

// cexSource is what the manager reads from cex; *cex.CexManager satisfies it
// and the tests use a fake.
type cexSource interface {
	// ReadPositions reads the two legs' positions from the venue now.
	ReadPositions(ctx context.Context) (cex.PositionSet, error)
	// Positions is the latest position snapshot, for the samples; Loaded is
	// false until the first read.
	Positions() cex.PositionSet
	// AccountChanged is closed when the positions or the funding records are
	// next published.
	AccountChanged() <-chan struct{}
	// AccountAssets is the last read of the account's equity and margin
	// ratio, for the samples only; ok is false before one and on the paper
	// venue.
	AccountAssets() (assets cex.AccountAssets, ok bool)
	// FinancialRecords lists the funding records of the two legs read so
	// far, oldest first. The slice is shared and is not changed.
	FinancialRecords() []cex.FinancialRecord
	// FundingRateAt is the rate the leg's contract settled at the given
	// settlement; ok is false while it is not published or not read.
	FundingRateAt(leg cex.Leg, settlement time.Time) (rate decimal.Decimal, ok bool)
}

// --- timings ---

// timings are the manager's clock and its fixed intervals. They are not
// configuration; tests replace the clock and shorten the intervals.
type timings struct {
	clock func() time.Time // every method without a time argument reads the time here

	tick             time.Duration // Run retries unconfirmed void rows and looks for new funding records this often
	sampleEvery      time.Duration // one inventory_sample row this often
	statusLog        time.Duration // one status line this often
	reconcileConfirm time.Duration // positions that differ from the ledger are a mismatch when they still differ this long after the first read
	failLogEvery     time.Duration // a synchronous write that keeps failing is logged again this often
}

// defaultTimings are the values used in the process.
var defaultTimings = timings{clock: time.Now, tick: 200 * time.Millisecond, sampleEvery: 10 * time.Second, statusLog: 30 * time.Second, reconcileConfirm: time.Second, failLogEvery: time.Minute}

// --- errors ---

var (
	// errNoCycle: a ledger method that needs an open cycle found none.
	errNoCycle = errors.New("inventory: no cycle is open")
	// ErrStoreBlocked: a row that voids a cycle or a lot is not confirmed
	// written yet. No cycle and no lot is opened until it is.
	ErrStoreBlocked = errors.New("inventory: a void row is not confirmed written yet")
	// ErrCapitalPending: the configured capital differs from the one the
	// ledger runs on and waits for the unfinished cycle to close. No lot is
	// opened until then; reducing and closing go on.
	ErrCapitalPending = errors.New("inventory: the configured capital takes effect when the open cycle has closed")
	// ErrNotSettled: the cycle cannot close yet. One of its orders has no
	// final state, or the fee of a filled order is not confirmed.
	ErrNotSettled = errors.New("inventory: the cycle's orders are not settled")
	// ErrPositionsUnread: the venue's positions could not be read. That is
	// neither "flat" nor a mismatch.
	ErrPositionsUnread = errors.New("inventory: the venue's positions could not be read")
	// ErrPositionsDiffer: the venue's positions differ from the ledger by
	// more than the orders without a final state explain.
	ErrPositionsDiffer = errors.New("inventory: the venue's positions differ from the ledger")
)
