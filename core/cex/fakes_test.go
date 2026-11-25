package cex

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/logger"
	"etf-pair-arbitrage/core/store"
)

// dec parses a decimal literal of a test.
func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

// ny is a New York wall-clock time.
func ny(year int, month time.Month, day, hour, minute int) time.Time {
	return time.Date(year, month, day, hour, minute, 0, 0, NewYork)
}

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

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// testConfig is the configuration of the tests: the default with the pair,
// and one book connection per leg - a test of several sets the number.
func testConfig() Config {
	cfg := DefaultConfig
	cfg.BookConnections = 1
	cfg.Pair = Pair{Bull: LegConfig{Symbol: "SOXLUSDT", Ticker: "SOXL", BookTopic: TopicBooks5}, Bear: LegConfig{Symbol: "SOXSUSDT", Ticker: "SOXS", BookTopic: TopicBooks50}}
	return cfg
}

// testRig is a manager on fakes, with the clock and the sink the test reads.
type testRig struct {
	m      *CexManager
	clock  *fakeClock
	venue  *fakeVenue
	index  *fakeIndex
	fx     *fakeFX
	issuer *fakeIssuer
	sink   *store.Fake
}

// newRig builds a manager on fakes with its clock at now. No loop runs: a
// test calls the step functions itself, or starts the loop it needs.
func newRig(t *testing.T, cfg Config, now time.Time) *testRig {
	t.Helper()
	r := &testRig{clock: &fakeClock{now: now}, venue: newFakeVenue(), index: &fakeIndex{closes: map[Symbol]map[int64]decimal.Decimal{}},
		fx: &fakeFX{candles: map[int64]decimal.Decimal{}}, issuer: &fakeIssuer{navs: map[string]decimal.Decimal{}, distributions: map[string][]Distribution{}}, sink: &store.Fake{}}
	m, err := NewManager(cfg, r.venue, r.index, r.fx, r.issuer)
	if err != nil {
		t.Fatal(err)
	}
	m.timing.clock = r.clock.Now
	m.UseStore(r.sink)
	r.m = m
	return r
}

// events returns the kinds of the event rows recorded so far.
func (r *testRig) events() []string {
	var kinds []string
	for _, row := range r.sink.Rows("event") {
		kinds = append(kinds, row.(store.EventRow).Kind)
	}
	return kinds
}

// facts returns the anchor_fact rows recorded so far.
func (r *testRig) facts() []store.AnchorFactRow {
	var out []store.AnchorFactRow
	for _, row := range r.sink.Rows("anchor_fact") {
		out = append(out, row.(store.AnchorFactRow))
	}
	return out
}

// waitFor polls cond until it holds; the loops under test run on their own
// goroutines.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); !cond(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// logCapture is a slog handler that keeps every line's level and message.
type logCapture struct {
	mu    sync.Mutex
	lines []string // "LEVEL message"
}

func (c *logCapture) Enabled(context.Context, slog.Level) bool { return true }
func (c *logCapture) WithAttrs([]slog.Attr) slog.Handler       { return c }
func (c *logCapture) WithGroup(string) slog.Handler            { return c }
func (c *logCapture) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, r.Level.String()+" "+r.Message)
	return nil
}

// take returns the lines captured since the last call that are not Debug.
func (c *logCapture) take() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, line := range c.lines {
		if !strings.HasPrefix(line, "DEBUG") {
			out = append(out, line)
		}
	}
	c.lines = nil
	return out
}

// captureLogs routes the process logger into a logCapture until the test
// ends.
func captureLogs(t *testing.T) *logCapture {
	t.Helper()
	c, old := &logCapture{}, logger.GlobalLogger
	logger.GlobalLogger = slog.New(c)
	t.Cleanup(func() { logger.GlobalLogger = old })
	return c
}

// --- fake venue ---

// fakeSubscription is one SubscribeBooks call as the fake venue keeps it.
type fakeSubscription struct {
	ctx     context.Context
	subs    []BookSubscription
	onEvent func(BookEvent)
	onDown  func(error)
}

// fakeVenue is a Venue whose answers the test sets and whose streams the
// test drives. It counts the calls it gets; it has no method that could
// change the account, like the interface.
type fakeVenue struct {
	mu            sync.Mutex
	subscriptions []*fakeSubscription

	rules     map[Symbol]MarketRule
	settings  AccountSettings
	positions PositionSet
	records   []FinancialRecord
	funding   map[Symbol][]FundingSettlement
	err       error // returned by every reference read while set

	recordsSince []time.Time // the since of each FinancialRecords call
	calls        map[string]int

	// orders: what PlaceOrder answers, what the venue knows of each order,
	// its fills and its recent orders; and what it was asked
	place       func(req OrderRequest) (OrderAck, error) // nil: ErrNotSent
	placed      []OrderRequest
	orderInfo   map[ClientOrderID]OrderRecord // OrderByClientID; an id not held is ErrUnknownOrder
	orderErr    error                         // returned by OrderByClientID while set
	fillsRead   []Fill
	fillsSince  []time.Time // the since of each Fills call
	recent      map[Symbol][]OrderRecord
	recentErr   map[Symbol]error   // returned by RecentOrders of the contract while set
	recentSince []time.Time        // the since of each RecentOrders call
	private     func(PrivateEvent) // the subscriber of the private stream
}

var _ Venue = (*fakeVenue)(nil)

func newFakeVenue() *fakeVenue {
	return &fakeVenue{calls: map[string]int{}, funding: map[Symbol][]FundingSettlement{}, orderInfo: map[ClientOrderID]OrderRecord{}, recent: map[Symbol][]OrderRecord{}}
}

func (v *fakeVenue) called(name string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls[name]++
	return v.err
}

func (v *fakeVenue) callCount(name string) int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.calls[name]
}

func (v *fakeVenue) Name() string { return "fake" }

func (v *fakeVenue) MarketRules(ctx context.Context, symbols []Symbol) (map[Symbol]MarketRule, error) {
	if err := v.called("MarketRules"); err != nil {
		return nil, err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	out := map[Symbol]MarketRule{}
	for _, s := range symbols {
		if r, ok := v.rules[s]; ok {
			out[s] = r
		}
	}
	return out, nil
}

func (v *fakeVenue) AccountSettings(ctx context.Context, symbols []Symbol) (AccountSettings, error) {
	if err := v.called("AccountSettings"); err != nil {
		return AccountSettings{}, err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.settings, nil
}

func (v *fakeVenue) Positions(ctx context.Context, symbols []Symbol) (PositionSet, error) {
	if err := v.called("Positions"); err != nil {
		return PositionSet{}, err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.positions, nil
}

func (v *fakeVenue) OrderByClientID(ctx context.Context, symbol Symbol, id ClientOrderID) (OrderRecord, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls["OrderByClientID"]++
	if v.orderErr != nil {
		return OrderRecord{}, v.orderErr
	}
	rec, ok := v.orderInfo[id]
	if !ok {
		return OrderRecord{}, fmt.Errorf("code 25204: %w", ErrUnknownOrder)
	}
	return rec, nil
}

func (v *fakeVenue) RecentOrders(ctx context.Context, symbol Symbol, since time.Time) ([]OrderRecord, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls["RecentOrders"]++
	v.recentSince = append(v.recentSince, since)
	if err := v.recentErr[symbol]; err != nil {
		return nil, err
	}
	return v.recent[symbol], v.err
}

func (v *fakeVenue) Fills(ctx context.Context, since time.Time) ([]Fill, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls["Fills"]++
	v.fillsSince = append(v.fillsSince, since)
	return v.fillsRead, nil
}

func (v *fakeVenue) FinancialRecords(ctx context.Context, since time.Time) ([]FinancialRecord, error) {
	if err := v.called("FinancialRecords"); err != nil {
		return nil, err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.recordsSince = append(v.recordsSince, since)
	return append([]FinancialRecord(nil), v.records...), nil
}

func (v *fakeVenue) FundingHistory(ctx context.Context, symbol Symbol, limit int) ([]FundingSettlement, error) {
	if err := v.called("FundingHistory"); err != nil {
		return nil, err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.funding[symbol], nil
}

func (v *fakeVenue) SubscribeBooks(ctx context.Context, subs []BookSubscription, onEvent func(BookEvent), onDown func(error)) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.subscriptions = append(v.subscriptions, &fakeSubscription{ctx: ctx, subs: subs, onEvent: onEvent, onDown: onDown})
	return nil
}

func (v *fakeVenue) SubscribePrivate(ctx context.Context, symbols []Symbol, onEvent func(PrivateEvent), onDown func(error)) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls["SubscribePrivate"]++
	v.private = onEvent
	return nil
}

func (v *fakeVenue) PlaceOrder(ctx context.Context, req OrderRequest) (OrderAck, error) {
	v.mu.Lock()
	v.calls["PlaceOrder"]++
	v.placed = append(v.placed, req)
	place := v.place
	v.mu.Unlock()
	if place == nil {
		return OrderAck{}, ErrNotSent
	}
	return place(req)
}

// placedIDs returns the ids of the orders PlaceOrder received, in order.
func (v *fakeVenue) placedIDs() []ClientOrderID {
	v.mu.Lock()
	defer v.mu.Unlock()
	var out []ClientOrderID
	for _, req := range v.placed {
		out = append(out, req.ClientOrderID)
	}
	return out
}

// subscriptionsOf returns the SubscribeBooks calls made for symbol, oldest
// first.
func (v *fakeVenue) subscriptionsOf(symbol Symbol) []*fakeSubscription {
	v.mu.Lock()
	defer v.mu.Unlock()
	var out []*fakeSubscription
	for _, s := range v.subscriptions {
		if s.subs[0].Symbol == symbol {
			out = append(out, s)
		}
	}
	return out
}

// --- fake sources ---

// fakeIndex is an IndexSource: index closes by symbol and minute, and a
// schedule.
type fakeIndex struct {
	mu       sync.Mutex
	closes   map[Symbol]map[int64]decimal.Decimal // symbol -> unix ms of the minute -> close
	schedule []ScheduleDay
	err      error
	calls    int // IndexClose calls
}

func (f *fakeIndex) setClose(symbol Symbol, minute time.Time, price string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closes[symbol] == nil {
		f.closes[symbol] = map[int64]decimal.Decimal{}
	}
	f.closes[symbol][minute.UnixMilli()] = dec(price)
}

func (f *fakeIndex) IndexClose(ctx context.Context, symbol Symbol, minute time.Time) (decimal.Decimal, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return decimal.Decimal{}, false, f.err
	}
	price, ok := f.closes[symbol][minute.UnixMilli()]
	return price, ok, nil
}

func (f *fakeIndex) TradingSchedule(ctx context.Context) ([]ScheduleDay, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.schedule, f.err
}

// fakeFX is an FXSource: the quote the next poll reads, and minute candles.
type fakeFX struct {
	mu          sync.Mutex
	bid, ask    decimal.Decimal
	err         error
	candles     map[int64]decimal.Decimal // unix ms of the minute -> close
	candleErr   error                     // returned by MinuteClose while set
	candleCalls int
}

func (f *fakeFX) set(bid, ask string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bid, f.ask, f.err = dec(bid), dec(ask), err
}

func (f *fakeFX) Quote(ctx context.Context) (decimal.Decimal, decimal.Decimal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bid, f.ask, f.err
}

func (f *fakeFX) MinuteClose(ctx context.Context, minute time.Time) (decimal.Decimal, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.candleCalls++
	if f.candleErr != nil {
		return decimal.Decimal{}, false, f.candleErr
	}
	price, ok := f.candles[minute.UnixMilli()]
	return price, ok, nil
}

func (f *fakeFX) MinuteCloses(ctx context.Context, from, to time.Time) ([]FXMinute, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []FXMinute
	for minute := from; minute.Before(to); minute = minute.Add(time.Minute) {
		if price, ok := f.candles[minute.UnixMilli()]; ok {
			out = append(out, FXMinute{Minute: minute, Close: price})
		}
	}
	return out, nil
}

// fakeIssuer is an IssuerSource: each fund's NAV of the trade date asked for
// (the test changes it as the issuer would publish and revise it) and its
// distributions.
type fakeIssuer struct {
	mu            sync.Mutex
	navs          map[string]decimal.Decimal // ticker -> NAV; missing: not published yet
	navDates      []string                   // the trade date of every NAV call
	distributions map[string][]Distribution
	err           error
	navCalls      int
	distCalls     int
}

func (f *fakeIssuer) setNAV(ticker, nav string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.navs[ticker] = dec(nav)
}

func (f *fakeIssuer) NAV(ctx context.Context, ticker string, tradeDate time.Time) (decimal.Decimal, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.navCalls++
	f.navDates = append(f.navDates, tradeDate.Format(dateLayout))
	if f.err != nil {
		return decimal.Decimal{}, false, f.err
	}
	nav, ok := f.navs[ticker]
	return nav, ok, nil
}

func (f *fakeIssuer) Distributions(ctx context.Context, ticker string) ([]Distribution, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.distCalls++
	if f.err != nil {
		return nil, f.err
	}
	return f.distributions[ticker], nil
}
