// Package store writes what the process does into schema trade of the
// PostgreSQL database and reads the last state back at start. A module hands
// each record to the store as a typed row; one writer goroutine sends the
// rows to the database in the order they were handed over. There is no local
// journal: a row that must be on disk before an order is sent goes through
// WriteSync, which returns after the commit.
//
// The store imports no other package of the process. Rows are its own
// structs; each module converts between them and its domain types.
package store

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/shopspring/decimal"
)

// --- configuration ---

// Config is the store's configuration. config.yaml has no `store:` section;
// the one field is copied from exec.live by config.Load.
type Config struct {
	Live bool `yaml:"-"` // stamped on every row and used to filter what is read back: paper and live rows never mix
}

// timings are the store's fixed intervals and sizes. Tests shorten them.
type timings struct {
	queueSize    int           // rows and sync requests that may wait for the writer; a row arriving beyond this is dropped
	batchRows    int           // a batch is written as soon as it holds this many rows
	flushEvery   time.Duration // and otherwise this often
	retryEvery   time.Duration // a batch that could not be sent is tried again this often
	failLogEvery time.Duration // and reported again this often while it keeps failing
	writeTimeout time.Duration // the limit of one transaction, and of a WriteSync as its caller sees it
}

var defaultTimings = timings{queueSize: 4096, batchRows: 256, flushEvery: 200 * time.Millisecond, retryEvery: time.Second, failLogEvery: time.Minute, writeTimeout: time.Second}

// --- identifiers ---

// RunID names one start of the process: its start in unix milliseconds with
// the host name. Every row carries the run that wrote it.
type RunID string

// --- interfaces ---

// Row is one record for one table. A row is a struct value whose fields carry
// the column names in their json tags. `omitempty` marks a column written as
// NULL when the field is zero; a float64 that is not finite, an empty
// json.RawMessage and a decimal.NullDecimal that is not valid are always
// written as NULL. run_id and live are not fields:
// the store adds them, as it adds seq to decision and event rows.
type Row interface{ Table() string }

// Sink is what a module records into: the Store in the process, a Fake in a
// test.
type Sink interface {
	// Record queues rows for the writer and never blocks. When the queue is
	// full the rows are dropped and counted.
	Record(rows ...Row)
	// WriteSync returns nil once the rows are committed, in one transaction,
	// after everything recorded before them, and returns an error at the
	// latest when the write timeout ends, however busy the writer is. An
	// error means they are not confirmed written: they are never retried,
	// but the writer may have committed them, or may still commit them, when
	// the timeout ended first. A caller that gets an error and so does
	// not act on the rows records a row of the same key that voids them (a
	// cycle closed as store_error, a lot void, an order not_sent); it follows in
	// the same queue and is the last write either way. While a batch is
	// being retried WriteSync fails at once.
	WriteSync(rows ...Row) error
	// Healthy is false while the last write did not reach the database or
	// was not answered in time.
	Healthy() bool
}

// --- tables ---

// tableKind is how a table takes a row whose key is already there.
type tableKind string

const (
	kindState  tableKind = "state"  // the newest state of a business object: the row is replaced as a whole, last write wins
	kindFact   tableKind = "fact"   // written once: a repeat with the same key is ignored
	kindSample tableKind = "sample" // a periodic measurement keyed by its time; a repeat (a batch sent twice) is ignored
)

// tableSpec is what the writer knows about a table: its kind, the columns
// that name a row, and whether the store numbers its rows.
type tableSpec struct {
	kind tableKind
	key  []string
	seq  bool // the store adds a seq column, one sequence per run
}

// tables lists every table of schema.sql. Each table is written by one
// module, so rows of one key reach the writer in the order they were made
// and a state table needs no version column. Every key holds live or run_id:
// paper and live make their ids from the same clock and the same market, so
// equal ids must be expected and must not meet in one row.
var tables = map[string]tableSpec{
	"process_run":      {kind: kindState, key: []string{"run_id"}},
	"minute_bar":       {kind: kindFact, key: []string{"live", "minute_ms"}},
	"signal_sample":    {kind: kindSample, key: []string{"ts_ms", "run_id"}},
	"anchor_fact":      {kind: kindFact, key: []string{"live", "session_id", "kind", "leg", "known_ms"}},
	"cycle":            {kind: kindState, key: []string{"live", "cycle_id"}},
	"lot":              {kind: kindState, key: []string{"live", "lot_id"}},
	"cex_order":        {kind: kindState, key: []string{"live", "client_order_id"}},
	"cex_fill":         {kind: kindFact, key: []string{"live", "exec_id"}},
	"decision":         {kind: kindFact, key: []string{"run_id", "seq"}, seq: true},
	"funding":          {kind: kindFact, key: []string{"live", "record_id"}},
	"funding_rate":     {kind: kindFact, key: []string{"live", "symbol", "settlement_ms"}},
	"inventory_sample": {kind: kindSample, key: []string{"ts_ms", "run_id"}},
	"inventory_state":  {kind: kindState, key: []string{"live"}},
	"event":            {kind: kindFact, key: []string{"run_id", "seq"}, seq: true},
}

// --- rows ---
//
// One struct per table; schema.sql documents every column. Money, prices and
// quantities are decimals, times are unix milliseconds, statistics of the
// spread are float64.

// ProcessRunRow is one start of the process (process_run, written by main).
type ProcessRunRow struct {
	StartedMs int64           `json:"started_ms"`
	StoppedMs int64           `json:"stopped_ms,omitempty"`
	Host      string          `json:"host"`
	Config    json.RawMessage `json:"config"` // the effective config.yaml; nothing from the secrets file
}

func (ProcessRunRow) Table() string { return "process_run" }

// MinuteBarRow is the raw inputs of one finished minute (minute_bar, written
// by exec).
type MinuteBarRow struct {
	MinuteMs  int64   `json:"minute_ms"`
	SessionID string  `json:"session_id"`
	MidBull   float64 `json:"mid_bull"`
	MidBear   float64 `json:"mid_bear"`
	F         float64 `json:"f"`
	Valid     bool    `json:"valid"`
	Source    string  `json:"source"`
}

func (MinuteBarRow) Table() string { return "minute_bar" }

// SignalSampleRow is what the loop saw in one second (signal_sample, written
// by exec).
type SignalSampleRow struct {
	TsMs         int64           `json:"ts_ms"`
	SessionID    string          `json:"session_id"`
	SessionEndMs int64           `json:"session_end_ms,omitempty"`
	Phase        string          `json:"phase"`
	AnchorKind   string          `json:"anchor_kind,omitempty"`
	AnchorBull   decimal.Decimal `json:"anchor_bull,omitempty"`
	AnchorBear   decimal.Decimal `json:"anchor_bear,omitempty"`
	F            float64         `json:"f"`
	MidD         float64         `json:"mid_d"`
	MuD          float64         `json:"mu_d"`
	SigmaD       float64         `json:"sigma_d"`
	XLongD       float64         `json:"x_long_d"`
	XShortD      float64         `json:"x_short_d"`
	StatsReady   bool            `json:"stats_ready"`
	IdleReason   string          `json:"idle_reason,omitempty"`
	DepthOKBull  bool            `json:"depth_ok_bull"`
	DepthOKBear  bool            `json:"depth_ok_bear"`
	ConflictBull bool            `json:"conflict_bull"`
	ConflictBear bool            `json:"conflict_bear"`
	CycleID      string          `json:"cycle_id,omitempty"`
	Direction    string          `json:"direction,omitempty"`
	HoldBull     decimal.Decimal `json:"hold_bull"`
	HoldBear     decimal.Decimal `json:"hold_bear"`
	QuotaLeft    decimal.Decimal `json:"quota_left"`
	MidBull      float64         `json:"mid_bull"` // NaN (NULL) while the leg has no best bid and ask
	MidBear      float64         `json:"mid_bear"`
}

func (SignalSampleRow) Table() string { return "signal_sample" }

// AnchorFactRow is one anchor, NAV or dividend as the process learned it
// (anchor_fact, written by cex).
type AnchorFactRow struct {
	SessionID string          `json:"session_id"`
	Kind      string          `json:"kind"`
	Leg       string          `json:"leg"`
	KnownMs   int64           `json:"known_ms"`
	Value     decimal.Decimal `json:"value"`
	F         decimal.Decimal `json:"f,omitempty"`
	Source    string          `json:"source"`
	Detail    json.RawMessage `json:"detail"`
}

func (AnchorFactRow) Table() string { return "anchor_fact" }

// CycleRow is one round from flat to flat (cycle, written by inventory).
type CycleRow struct {
	CycleID        string          `json:"cycle_id"`
	SessionID      string          `json:"session_id"`
	Direction      string          `json:"direction"`
	Status         string          `json:"status"`
	EndReason      string          `json:"end_reason,omitempty"`
	PairAnchorKind string          `json:"pair_anchor_kind"`
	PairAnchorBull decimal.Decimal `json:"pair_anchor_bull"`
	PairAnchorBear decimal.Decimal `json:"pair_anchor_bear"`
	OpenedMs       int64           `json:"opened_ms"`
	ClosedMs       int64           `json:"closed_ms,omitempty"`
	Gross          decimal.Decimal `json:"gross"`
	Fees           decimal.Decimal `json:"fees"`
	Funding        decimal.Decimal `json:"funding"`
	FundingKnown   bool            `json:"funding_known"`
	FundingAwaited json.RawMessage `json:"funding_awaited"`
	Net            decimal.Decimal `json:"net"`
	DustBull       decimal.Decimal `json:"dust_bull"`
	DustBear       decimal.Decimal `json:"dust_bear"`
}

func (CycleRow) Table() string { return "cycle" }

// LotRow is one entry batch and what has closed it (lot, written by
// inventory).
type LotRow struct {
	LotID          string          `json:"lot_id"`
	CycleID        string          `json:"cycle_id"`
	Seq            int             `json:"seq"`
	Status         string          `json:"status"`
	VoidReason     string          `json:"void_reason,omitempty"`
	Direction      string          `json:"direction"`
	XD             float64         `json:"x_d"`
	MuD            float64         `json:"mu_d"`
	SigmaD         float64         `json:"sigma_d"`
	FeeD           float64         `json:"fee_d"`
	FeeUSDTEst     decimal.Decimal `json:"fee_usdt_est"`
	EntryQtyBull   decimal.Decimal `json:"entry_qty_bull"`
	EntryValueBull decimal.Decimal `json:"entry_value_bull"`
	EntryFeeBull   decimal.Decimal `json:"entry_fee_bull"`
	ExitQtyBull    decimal.Decimal `json:"exit_qty_bull"`
	ExitValueBull  decimal.Decimal `json:"exit_value_bull"`
	ExitFeeBull    decimal.Decimal `json:"exit_fee_bull"`
	EntryQtyBear   decimal.Decimal `json:"entry_qty_bear"`
	EntryValueBear decimal.Decimal `json:"entry_value_bear"`
	EntryFeeBear   decimal.Decimal `json:"entry_fee_bear"`
	ExitQtyBear    decimal.Decimal `json:"exit_qty_bear"`
	ExitValueBear  decimal.Decimal `json:"exit_value_bear"`
	ExitFeeBear    decimal.Decimal `json:"exit_fee_bear"`
	OpenedMs       int64           `json:"opened_ms"`
	ClosedMs       int64           `json:"closed_ms,omitempty"`
	Gross          decimal.Decimal `json:"gross"`
	Fees           decimal.Decimal `json:"fees"`
	Net            decimal.Decimal `json:"net"`
}

func (LotRow) Table() string { return "lot" }

// OrderRow is one order as the process knows it (cex_order, written by cex).
type OrderRow struct {
	ClientOrderID     string          `json:"client_order_id"`
	CycleID           string          `json:"cycle_id"`
	LotID             string          `json:"lot_id,omitempty"`
	Leg               string          `json:"leg"`
	Symbol            string          `json:"symbol"`
	Purpose           string          `json:"purpose"`
	Side              string          `json:"side"`
	ReduceOnly        bool            `json:"reduce_only"`
	Price             decimal.Decimal `json:"price"`
	Qty               decimal.Decimal `json:"qty"`
	QuotaPrice        decimal.Decimal `json:"quota_price,omitempty"`
	Status            string          `json:"status"`
	Unknown           bool            `json:"unknown"`
	OrderID           string          `json:"order_id,omitempty"`
	CumExecQty        decimal.Decimal `json:"cum_exec_qty"`
	CumExecValue      decimal.Decimal `json:"cum_exec_value"`
	AvgPrice          decimal.Decimal `json:"avg_price,omitempty"`
	Fee               decimal.Decimal `json:"fee"`
	FeeKnown          bool            `json:"fee_known"`
	NotFound          bool            `json:"not_found"`
	CancelReason      string          `json:"cancel_reason,omitempty"`
	ErrorCode         string          `json:"error_code,omitempty"`
	ErrorMessage      string          `json:"error_message,omitempty"`
	UnknownSinceMs    int64           `json:"unknown_since_ms,omitempty"`
	SubmittedMs       int64           `json:"submitted_ms"`
	SentMs            int64           `json:"sent_ms,omitempty"`
	AckedMs           int64           `json:"acked_ms,omitempty"`
	UpdateReceivedMs  int64           `json:"update_received_ms,omitempty"`
	ExchangeUpdatedMs int64           `json:"exchange_updated_ms,omitempty"`
	AcceptedMs        int64           `json:"accepted_ms,omitempty"`
	ExchangeCreatedMs int64           `json:"exchange_created_ms,omitempty"`
	FinalMs           int64           `json:"final_ms,omitempty"`
}

func (OrderRow) Table() string { return "cex_order" }

// FillRow is one execution as the venue reported it (cex_fill, written by
// cex).
type FillRow struct {
	ExecID        string          `json:"exec_id"`
	ClientOrderID string          `json:"client_order_id"`
	OrderID       string          `json:"order_id"`
	Symbol        string          `json:"symbol"`
	Side          string          `json:"side"`
	Price         decimal.Decimal `json:"price"`
	Qty           decimal.Decimal `json:"qty"`
	Value         decimal.Decimal `json:"value"`
	Fee           decimal.Decimal `json:"fee"`
	ExecMs        int64           `json:"exec_ms"`
	ReceivedMs    int64           `json:"received_ms"`
}

func (FillRow) Table() string { return "cex_fill" }

// DecisionRow is one decision of the loop (decision, written by exec), or
// one matched paper order (kind paper_match, written by cex's paper venue).
// The store numbers it.
type DecisionRow struct {
	TsMs      int64           `json:"ts_ms"`
	CycleID   string          `json:"cycle_id,omitempty"`
	LotID     string          `json:"lot_id,omitempty"`
	Kind      string          `json:"kind"`
	Direction string          `json:"direction,omitempty"`
	Reason    string          `json:"reason,omitempty"`
	Detail    json.RawMessage `json:"detail"`
}

func (DecisionRow) Table() string { return "decision" }

// FundingRow is one funding record (funding, written by inventory).
type FundingRow struct {
	RecordID     string          `json:"record_id"`
	Symbol       string          `json:"symbol"`
	Type         string          `json:"type"`
	Amount       decimal.Decimal `json:"amount"`
	TsMs         int64           `json:"ts_ms"`
	SettlementMs int64           `json:"settlement_ms"`
	CycleID      string          `json:"cycle_id,omitempty"`
}

func (FundingRow) Table() string { return "funding" }

// FundingRateRow is one settled funding rate of a contract (funding_rate,
// written by cex). It is recorded for the board; nothing reads it back.
type FundingRateRow struct {
	Leg          string          `json:"leg"`
	Symbol       string          `json:"symbol"`
	SettlementMs int64           `json:"settlement_ms"`
	Rate         decimal.Decimal `json:"rate"`
}

func (FundingRateRow) Table() string { return "funding_rate" }

// InventorySampleRow is the exchange's positions against the ledger at one
// moment (inventory_sample, written by inventory). PosBull and PosBear are
// not valid (NULL) while the positions have not been read; a read position
// of zero is a valid 0. The venue's mark, unrealised PnL and liquidation
// price of each leg and the account's equity and margin ratio are recorded
// for the board only; each is not valid when the venue did not report it.
type InventorySampleRow struct {
	TsMs        int64               `json:"ts_ms"`
	PosBull     decimal.NullDecimal `json:"pos_bull"`
	PosBear     decimal.NullDecimal `json:"pos_bear"`
	LedgerBull  decimal.Decimal     `json:"ledger_bull"`
	LedgerBear  decimal.Decimal     `json:"ledger_bear"`
	R           decimal.Decimal     `json:"r"`
	QuotaTotal  decimal.Decimal     `json:"quota_total"`
	QuotaUsed   decimal.Decimal     `json:"quota_used"`
	Mismatch    bool                `json:"mismatch"`
	MarkBull    decimal.NullDecimal `json:"mark_bull"`
	MarkBear    decimal.NullDecimal `json:"mark_bear"`
	UplBull     decimal.NullDecimal `json:"upl_bull"`
	UplBear     decimal.NullDecimal `json:"upl_bear"`
	LiqBull     decimal.NullDecimal `json:"liq_bull"`
	LiqBear     decimal.NullDecimal `json:"liq_bear"`
	Equity      decimal.NullDecimal `json:"equity"`
	MarginRatio decimal.NullDecimal `json:"margin_ratio"`
}

func (InventorySampleRow) Table() string { return "inventory_sample" }

// InventoryStateRow is what the ledger carries from one cycle to the next
// (inventory_state, written by inventory). There is one row per mode.
type InventoryStateRow struct {
	Capital decimal.Decimal `json:"capital"`
	SinceMs int64           `json:"since_ms"`
	RClosed decimal.Decimal `json:"r_closed"`
	Dust    json.RawMessage `json:"dust"`
}

func (InventoryStateRow) Table() string { return "inventory_state" }

// EventRow is an alarm or a state change of a module (event, written by any
// module). The store numbers it.
type EventRow struct {
	TsMs    int64           `json:"ts_ms"`
	Module  string          `json:"module"`
	Level   string          `json:"level"`
	Kind    string          `json:"kind"`
	Message string          `json:"message"`
	Detail  json.RawMessage `json:"detail"`
}

func (EventRow) Table() string { return "event" }

// --- state read back at start ---

// State is what LoadState returns: the rows a restarted process continues
// from, of the store's own mode (paper or live).
type State struct {
	Cycle       *CycleRow          // the unfinished cycle (status open or takeover) of any session; nil when there is none
	Lots        []LotRow           // that cycle's lots, in their order within the cycle
	Orders      []OrderRow         // that cycle's orders, oldest first
	Inventory   *InventoryStateRow // nil when it was never written
	MinuteBars  []MinuteBarRow     // the given session's minutes, oldest first
	AnchorFacts []AnchorFactRow    // the given session's anchor facts, in the order they were learned
	LastEntry   *LotRow            // the most recently opened lot that was built (status open or closed); nil when there is none

	// What the ledger needs beyond the unfinished cycle.
	Funding        []FundingRow // every funding record booked, oldest first: a record is booked once, by its record_id
	PendingFunding []CycleRow   // the finished cycles whose funding is not known yet (funding_known false), oldest first
}

// PerpMinuteRow is the last second of one minute at which the recorder's
// etfpair.perp_second held a best bid and ask on both legs: what a restart
// fills a minute it did not record itself with (PRD 3.7). It is read, never
// written.
type PerpMinuteRow struct {
	MinuteMs         int64 // the start of the minute
	SecondMs         int64 // the second taken, in (MinuteMs, MinuteMs + 60 s]
	BidBull, AskBull float64
	BidBear, AskBear float64
}

// --- test double ---

// Fake is a Sink for other packages' tests: it keeps every row in memory, in
// the order received.
type Fake struct {
	mu   sync.Mutex
	rows []Row
	err  error
	keep bool // WriteSync keeps its rows although it returns err
}

// Record keeps the rows.
func (f *Fake) Record(rows ...Row) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = append(f.rows, rows...)
}

// WriteSync keeps the rows and returns nil. While an error is set it returns
// that error: after Fail it keeps nothing, after FailAfterWrite it keeps the
// rows all the same. Those are the two things an error of the Store's
// WriteSync can mean: not written, or written without the answer arriving in
// time.
func (f *Fake) WriteSync(rows ...Row) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err == nil || f.keep {
		f.rows = append(f.rows, rows...)
	}
	return f.err
}

// Healthy is false while an error is set by Fail or FailAfterWrite.
func (f *Fake) Healthy() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err == nil
}

// Fail makes WriteSync return err and Healthy report false, as the Store
// does while the database cannot be written; nil ends the failure. Record
// keeps rows throughout, as the Store's queue does.
func (f *Fake) Fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err, f.keep = err, false
}

// FailAfterWrite makes WriteSync keep its rows and return err all the same,
// as the Store does when the commit succeeded and its answer came too late.
// Fail(nil) ends it.
func (f *Fake) FailAfterWrite(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err, f.keep = err, true
}

// Rows returns the rows received for table, oldest first.
func (f *Fake) Rows(table string) []Row {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Row
	for _, r := range f.rows {
		if r.Table() == table {
			out = append(out, r)
		}
	}
	return out
}

// Last returns the newest row received for table, nil when there is none.
func (f *Fake) Last(table string) Row {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.rows) - 1; i >= 0; i-- {
		if f.rows[i].Table() == table {
			return f.rows[i]
		}
	}
	return nil
}
