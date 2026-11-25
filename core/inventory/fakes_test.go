package inventory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/logger"
	"etf-pair-arbitrage/core/store"
	"etf-pair-arbitrage/core/strategy"
)

// dec parses a decimal literal of a test.
func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

// testPair is the example pair.
var testPair = cex.Pair{Bull: cex.LegConfig{Symbol: "SOXLUSDT", Ticker: "SOXL", BookTopic: cex.TopicBooks5}, Bear: cex.LegConfig{Symbol: "SOXSUSDT", Ticker: "SOXS", BookTopic: cex.TopicBooks50}}

// t0 is where the tests' clocks start: 2026-11-24 01:00 UTC, an hour after
// one funding settlement and seven before the next.
var t0 = time.Date(2026, 11, 24, 1, 0, 0, 0, time.UTC)

// fakeClock is the clock of a manager under test: it moves only when the
// test moves it.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Set(t time.Time) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
	return t
}

func (c *fakeClock) Add(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	return c.now
}

// fakeCex is the exchange side as the manager reads it.
type fakeCex struct {
	mu        sync.Mutex
	positions map[cex.Symbol]decimal.Decimal // signed, by contract
	loaded    bool                           // a position snapshot was published
	readErr   error                          // ReadPositions fails with it while set
	reads     int                            // ReadPositions calls that succeeded
	records   []cex.FinancialRecord
	rates     [2]map[int64]decimal.Decimal // by leg: settlement in unix ms -> rate
	board     map[cex.Symbol]cex.Position  // the mark, unrealised PnL and liquidation price the venue reports with a held position
	assets    *cex.AccountAssets           // the account read; nil: none, as on the paper venue
	changed   chan struct{}
}

func newFakeCex() *fakeCex {
	return &fakeCex{positions: map[cex.Symbol]decimal.Decimal{}, rates: [2]map[int64]decimal.Decimal{{}, {}}, board: map[cex.Symbol]cex.Position{}, changed: make(chan struct{})}
}

func (f *fakeCex) set() cex.PositionSet {
	set := cex.PositionSet{BySymbol: map[cex.Symbol]cex.Position{}, Loaded: true}
	for symbol, qty := range f.positions {
		if !qty.IsZero() {
			b := f.board[symbol]
			set.BySymbol[symbol] = cex.Position{Symbol: symbol, Qty: qty, MarkPrice: b.MarkPrice, UnrealisedPnL: b.UnrealisedPnL, LiqPrice: b.LiqPrice}
		}
	}
	return set
}

func (f *fakeCex) ReadPositions(context.Context) (cex.PositionSet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readErr != nil {
		return cex.PositionSet{}, f.readErr
	}
	f.reads++
	f.loaded = true
	return f.set(), nil
}

func (f *fakeCex) Positions() cex.PositionSet {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.loaded {
		return cex.PositionSet{}
	}
	return f.set()
}

func (f *fakeCex) AccountChanged() <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.changed
}

func (f *fakeCex) AccountAssets() (cex.AccountAssets, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.assets == nil {
		return cex.AccountAssets{}, false
	}
	return *f.assets, true
}

func (f *fakeCex) FinancialRecords() []cex.FinancialRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.records
}

func (f *fakeCex) FundingRateAt(leg cex.Leg, settlement time.Time) (decimal.Decimal, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rate, ok := f.rates[leg][settlement.UnixMilli()]
	return rate, ok
}

// setPositions sets the venue's signed positions of the two legs.
func (f *fakeCex) setPositions(bull, bear string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.positions[testPair.Bull.Symbol], f.positions[testPair.Bear.Symbol] = dec(bull), dec(bear)
}

// failReads makes ReadPositions fail with err; nil ends the failure.
func (f *fakeCex) failReads(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readErr = err
}

// addRecord publishes one more funding record, as cex does after a read.
func (f *fakeCex) addRecord(id string, leg cex.Leg, amount string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	symbol := testPair.Bull.Symbol
	if leg == cex.Bear {
		symbol = testPair.Bear.Symbol
	}
	f.records = append(append([]cex.FinancialRecord(nil), f.records...), cex.FinancialRecord{ID: id, Symbol: symbol, Type: "funding", Amount: dec(amount), At: at})
	close(f.changed)
	f.changed = make(chan struct{})
}

// setRate publishes the rate one leg's contract settled at.
func (f *fakeCex) setRate(leg cex.Leg, settlement time.Time, rate string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rates[leg][settlement.UnixMilli()] = dec(rate)
}

// testSink is a store.Fake that also keeps the rows of every WriteSync that
// succeeded, one batch per call: a batch is one transaction. It can drop
// recorded rows, as the Store's full queue does, and make every WriteSync
// slow.
type testSink struct {
	*store.Fake
	mu      sync.Mutex
	syncs   [][]store.Row
	drop    bool          // Record drops its rows
	delay   time.Duration // every WriteSync takes this long
	entered chan struct{} // receives one value when a WriteSync begins; nil sends none
}

func (s *testSink) Record(rows ...store.Row) {
	s.mu.Lock()
	drop := s.drop
	s.mu.Unlock()
	if !drop {
		s.Fake.Record(rows...)
	}
}

func (s *testSink) WriteSync(rows ...store.Row) error {
	s.mu.Lock()
	delay, entered := s.delay, s.entered
	s.mu.Unlock()
	if entered != nil {
		entered <- struct{}{}
	}
	time.Sleep(delay)
	if err := s.Fake.WriteSync(rows...); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncs = append(s.syncs, rows)
	return nil
}

// dropRecords makes Record drop its rows, or keep them again.
func (s *testSink) dropRecords(drop bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.drop = drop
}

// syncCount is the number of WriteSync calls that succeeded.
func (s *testSink) syncCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.syncs)
}

// logBuffer collects log lines written from any goroutine.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// lines returns the log lines that contain every one of the given texts.
func (b *logBuffer) lines(texts ...string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, line := range strings.Split(b.buf.String(), "\n") {
		keep := line != ""
		for _, text := range texts {
			keep = keep && strings.Contains(line, text)
		}
		if keep {
			out = append(out, line)
		}
	}
	return out
}

// captureLogs routes the process logger to a buffer until the test ends.
func captureLogs(t *testing.T) *logBuffer {
	t.Helper()
	old, buf := logger.GlobalLogger, &logBuffer{}
	logger.GlobalLogger = slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Cleanup(func() { logger.GlobalLogger = old })
	return buf
}

// lastSync returns the rows of the newest WriteSync that succeeded.
func (s *testSink) lastSync() []store.Row {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.syncs) == 0 {
		return nil
	}
	return s.syncs[len(s.syncs)-1]
}

// testRig is a manager on fakes, with the clock and the sink the test reads.
type testRig struct {
	m     *InventoryManager
	clock *fakeClock
	cex   *fakeCex
	sink  *testSink
}

// newRig builds a manager with the given capital and leverage on fakes, its
// clock at t0. No loop runs: a test calls the methods and step functions
// itself.
func newRig(t testing.TB, capital, leverage string) *testRig {
	t.Helper()
	return newRigOn(t, capital, leverage, &fakeClock{now: t0}, newFakeCex(), &testSink{Fake: &store.Fake{}})
}

// newRigOn builds a manager on the given clock, exchange and sink: a second
// manager on the first one's sink is a restarted process.
func newRigOn(t testing.TB, capital, leverage string, clock *fakeClock, c *fakeCex, sink *testSink) *testRig {
	t.Helper()
	m, err := NewManager(Config{Capital: dec(capital), Leverage: dec(leverage)}, testPair, c)
	if err != nil {
		t.Fatal(err)
	}
	m.timing.clock = clock.Now
	m.UseStore(sink)
	return &testRig{m: m, clock: clock, cex: c, sink: sink}
}

// restart builds a new manager on the rig's clock, exchange and sink and
// restores it from what the sink holds, with the given order records.
func (r *testRig) restart(t *testing.T, capital, leverage string, orders ...cex.OrderRecord) *testRig {
	t.Helper()
	next := newRigOn(t, capital, leverage, r.clock, r.cex, r.sink)
	if err := next.m.Restore(stateOf(t, r.sink), orders); err != nil {
		t.Fatal(err)
	}
	return next
}

// openCycle opens a cycle at the rig's clock with anchors 40 / 60.
func (r *testRig) openCycle(t testing.TB, dir strategy.Direction) *Cycle {
	t.Helper()
	c, err := r.m.OpenCycle("2026-11-23", dir, cex.Anchors{SessionID: "2026-11-23", Kind: cex.AnchorBell, Bull: dec("40"), Bear: dec("60")}, r.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// openLot opens a lot in the open cycle's direction at the rig's clock.
func (r *testRig) openLot(t testing.TB) *Lot {
	t.Helper()
	lot, err := r.m.OpenLot(&strategy.EntryPlan{Direction: r.m.Cycle().Direction, X: 10, Mu: 3.33, Sigma: 4, FeeD: 2.6, FeeUSDT: dec("1.1")}, r.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	return lot
}

// closeCycle closes the open cycle at the rig's clock.
func (r *testRig) closeCycle(t *testing.T, reason string) {
	t.Helper()
	if err := r.m.CloseCycle(reason, r.clock.Now()); err != nil {
		t.Fatal(err)
	}
}

// lot returns a copy of lot number seq of the open cycle.
func (r *testRig) lot(t *testing.T, seq int) *Lot {
	t.Helper()
	c := r.m.Cycle()
	if c == nil || len(c.Lots) < seq {
		t.Fatalf("the open cycle has no lot %d", seq)
	}
	return c.Lots[seq-1]
}

// --- order records ---

// recordSeq numbers the record versions the helpers below build, as cex's
// order table does: every new version of a record has a larger UpdateSeq
// (Sync relies on it). at sets one explicitly.
var recordSeq atomic.Uint64

// ord builds one order record of cycle c, without a fill and in status
// pending. seq is its number within the cycle: the ids sort by it, as the
// real ids do.
func ord(c CycleID, seq int, leg cex.Leg, purpose cex.OrderPurpose, side cex.Side, lot LotID, price, qty string) cex.OrderRecord {
	letter := map[cex.OrderPurpose]string{cex.PurposeEntry: "E", cex.PurposeComplete: "C", cex.PurposeTrim: "T", cex.PurposeExit: "X", cex.PurposeForced: "F"}[purpose]
	symbol, legLetter := testPair.Bull.Symbol, "B"
	if leg == cex.Bear {
		symbol, legLetter = testPair.Bear.Symbol, "R"
	}
	id := fmt.Sprintf("etfp%s-%04d%s%s", strings.TrimPrefix(string(c), "c"), seq, legLetter, letter)
	return cex.OrderRecord{UpdateSeq: recordSeq.Add(1), Status: cex.StatusPending, Request: cex.OrderRequest{ClientOrderID: cex.ClientOrderID(id), Symbol: symbol, Leg: leg, Side: side,
		Price: dec(price), Qty: dec(qty), ReduceOnly: purpose != cex.PurposeEntry && purpose != cex.PurposeComplete, Purpose: purpose, CycleID: string(c), LotID: string(lot)}}
}

// fill returns o with the given cumulative fill and a confirmed fee: filled
// when the whole quantity traded, cancelled (the IOC's remainder) when less.
func fill(o cex.OrderRecord, qty, value, fee string) cex.OrderRecord {
	o.CumExecQty, o.CumExecValue, o.Fee, o.FeeKnown, o.UpdateSeq = dec(qty), dec(value), dec(fee), true, recordSeq.Add(1)
	o.Status = cex.StatusCancelled
	if o.CumExecQty.Equal(o.Request.Qty) {
		o.Status = cex.StatusFilled
	}
	if o.CumExecQty.IsPositive() {
		o.AvgPrice = o.CumExecValue.Div(o.CumExecQty)
	}
	return o
}

// working returns o with the given cumulative fill and no final state yet.
func working(o cex.OrderRecord, qty, value, fee string) cex.OrderRecord {
	o = fill(o, qty, value, fee)
	o.Status = cex.StatusAcked
	return o
}

// timed returns o with the venue's time of its last change, which dates its
// fills.
func timed(o cex.OrderRecord, t time.Time) cex.OrderRecord {
	o.ExchangeUpdatedAt, o.UpdateSeq = t, recordSeq.Add(1)
	return o
}

// at returns o as its n-th report: UpdateSeq grows with every change.
func at(o cex.OrderRecord, n uint64) cex.OrderRecord {
	o.UpdateSeq = n
	return o
}

// --- reading the sink ---

// rowsOf returns the rows of type T the sink received, oldest first.
func rowsOf[T store.Row](sink *testSink) []T {
	var zero T
	var out []T
	for _, row := range sink.Rows(zero.Table()) {
		out = append(out, row.(T))
	}
	return out
}

// lastOf returns the newest row of type T that keep accepts.
func lastOf[T store.Row](t *testing.T, sink *testSink, keep func(T) bool) T {
	t.Helper()
	rows := rowsOf[T](sink)
	for i := len(rows) - 1; i >= 0; i-- {
		if keep(rows[i]) {
			return rows[i]
		}
	}
	var zero T
	t.Fatalf("the sink holds no %s row that matches", zero.Table())
	return zero
}

// lastLotRow returns the newest row of the lot.
func lastLotRow(t *testing.T, sink *testSink, id LotID) store.LotRow {
	t.Helper()
	return lastOf(t, sink, func(r store.LotRow) bool { return r.LotID == string(id) })
}

// lastCycleRow returns the newest row of the cycle.
func lastCycleRow(t *testing.T, sink *testSink, id CycleID) store.CycleRow {
	t.Helper()
	return lastOf(t, sink, func(r store.CycleRow) bool { return r.CycleID == string(id) })
}

// rClosed returns r_closed of the newest inventory_state row.
func rClosed(t *testing.T, sink *testSink) decimal.Decimal {
	t.Helper()
	return lastOf(t, sink, func(store.InventoryStateRow) bool { return true }).RClosed
}

// eventKinds returns the kinds of the event rows the sink received.
func eventKinds(sink *testSink) []string {
	var kinds []string
	for _, e := range rowsOf[store.EventRow](sink) {
		kinds = append(kinds, e.Kind)
	}
	return kinds
}

// stateOf reads the sink the way store.LoadState reads the database: the
// newest row of every key, the unfinished cycle with its lots, the funding
// rows and the cycles awaiting funding.
func stateOf(t *testing.T, sink *testSink) store.State {
	t.Helper()
	var st store.State
	cycles, lots := map[string]store.CycleRow{}, map[string]store.LotRow{}
	var cycleIDs []string
	for _, c := range rowsOf[store.CycleRow](sink) {
		if _, seen := cycles[c.CycleID]; !seen {
			cycleIDs = append(cycleIDs, c.CycleID)
		}
		cycles[c.CycleID] = c
	}
	for _, l := range rowsOf[store.LotRow](sink) {
		lots[l.LotID] = l
	}
	if inv := rowsOf[store.InventoryStateRow](sink); len(inv) > 0 {
		st.Inventory = &inv[len(inv)-1]
	}
	booked := map[string]bool{}
	for _, f := range rowsOf[store.FundingRow](sink) {
		if !booked[f.RecordID] {
			booked[f.RecordID] = true
			st.Funding = append(st.Funding, f)
		}
	}

	for _, id := range cycleIDs {
		c := cycles[id]
		switch {
		case c.Status == string(CycleOpen) || c.Status == string(CycleTakeover):
			if st.Cycle != nil {
				t.Fatalf("the sink holds two unfinished cycles: %s and %s", st.Cycle.CycleID, c.CycleID)
			}
			st.Cycle = &c
		case !c.FundingKnown:
			st.PendingFunding = append(st.PendingFunding, c)
		}
	}
	if st.Cycle != nil {
		for _, l := range lots {
			if l.CycleID == st.Cycle.CycleID {
				st.Lots = append(st.Lots, l)
			}
		}
		sort.Slice(st.Lots, func(i, j int) bool { return st.Lots[i].Seq < st.Lots[j].Seq })
	}
	return st
}

// --- comparisons ---

// wantDec fails unless got equals the decimal literal want exactly.
func wantDec(t *testing.T, what string, got decimal.Decimal, want string) {
	t.Helper()
	if !got.Equal(dec(want)) {
		t.Errorf("%s = %s, want %s", what, got, want)
	}
}

// wantLeg fails unless one leg of a lot holds exactly these six amounts.
func wantLeg(t *testing.T, what string, got LotLeg, entryQty, entryValue, entryFee, exitQty, exitValue, exitFee string) {
	t.Helper()
	wantDec(t, what+" entry qty", got.EntryQty, entryQty)
	wantDec(t, what+" entry value", got.EntryValue, entryValue)
	wantDec(t, what+" entry fee", got.EntryFee, entryFee)
	wantDec(t, what+" exit qty", got.ExitQty, exitQty)
	wantDec(t, what+" exit value", got.ExitValue, exitValue)
	wantDec(t, what+" exit fee", got.ExitFee, exitFee)
}

// errTest is the error the fakes fail with.
var errTest = errors.New("test: failing on purpose")
