package exec

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/inventory"
	"etf-pair-arbitrage/core/logger"
	"etf-pair-arbitrage/core/store"
	"etf-pair-arbitrage/core/strategy"
)

// The tests drive the loop through the real cex manager, paper venue,
// inventory and strategy. Only what cex reads on its own clock is replaced:
// F and the anchors (testCex). The books are real cex books, fed frame by
// frame with the test's clock as their arrival time; orders are matched by
// the paper venue on those same frames.

// dec parses a decimal literal of a test.
func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

// utc8Time is a wall-clock time in UTC+8, as the PRD's examples give them.
func utc8Time(month time.Month, day, hour, minute, second int) time.Time {
	return time.Date(2026, month, day, hour, minute, second, 0, utc8)
}

// lv is one book level.
func lv(price, qty string) cex.BookLevel { return cex.BookLevel{Price: dec(price), Qty: dec(qty)} }

// side is one side of a book, best first.
type side []cex.BookLevel

// pairBook is one frame of each leg's depth.
type pairBook struct {
	bullBids, bullAsks, bearBids, bearAsks side
}

// --- the real venue behind the paper venue ---

// fakeVenue is the exchange the paper venue wraps: it publishes the rules
// and carries the book streams the test feeds. It must never receive an
// order or an account request.
type fakeVenue struct {
	t      *testing.T
	mu     sync.Mutex
	stream map[cex.Symbol]func(cex.BookEvent)
}

var _ cex.Venue = (*fakeVenue)(nil)

func (v *fakeVenue) Name() string { return "fake" }

func (v *fakeVenue) MarketRules(context.Context, []cex.Symbol) (map[cex.Symbol]cex.MarketRule, error) {
	return map[cex.Symbol]cex.MarketRule{
		"SOXLUSDT": {Symbol: "SOXLUSDT", PriceTick: dec("0.01"), QtyStep: dec("0.01"), MinQty: dec("0.01"), MinNotional: dec("5"), Online: true},
		"SOXSUSDT": {Symbol: "SOXSUSDT", PriceTick: dec("0.001"), QtyStep: dec("0.01"), MinQty: dec("0.01"), MinNotional: dec("5"), Online: true},
	}, nil
}

func (v *fakeVenue) refused(what string) error {
	v.t.Errorf("the real venue was asked for %s", what)
	return errors.New("refused")
}

func (v *fakeVenue) AccountSettings(context.Context, []cex.Symbol) (cex.AccountSettings, error) {
	return cex.AccountSettings{}, v.refused("account settings")
}
func (v *fakeVenue) Positions(context.Context, []cex.Symbol) (cex.PositionSet, error) {
	return cex.PositionSet{}, v.refused("positions")
}
func (v *fakeVenue) OrderByClientID(context.Context, cex.Symbol, cex.ClientOrderID) (cex.OrderRecord, error) {
	return cex.OrderRecord{}, v.refused("an order")
}
func (v *fakeVenue) RecentOrders(context.Context, cex.Symbol, time.Time) ([]cex.OrderRecord, error) {
	return nil, v.refused("recent orders")
}
func (v *fakeVenue) Fills(context.Context, time.Time) ([]cex.Fill, error) {
	return nil, v.refused("fills")
}
func (v *fakeVenue) FinancialRecords(context.Context, time.Time) ([]cex.FinancialRecord, error) {
	return nil, v.refused("financial records")
}
func (v *fakeVenue) FundingHistory(context.Context, cex.Symbol, int) ([]cex.FundingSettlement, error) {
	return nil, nil
}
func (v *fakeVenue) SubscribeBooks(_ context.Context, subs []cex.BookSubscription, onEvent func(cex.BookEvent), _ func(error)) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.stream[subs[0].Symbol] = onEvent
	return nil
}
func (v *fakeVenue) SubscribePrivate(context.Context, []cex.Symbol, func(cex.PrivateEvent), func(error)) error {
	return v.refused("the private stream")
}
func (v *fakeVenue) PlaceOrder(context.Context, cex.OrderRequest) (cex.OrderAck, error) {
	return cex.OrderAck{}, v.refused("an order")
}

// feed hands one frame to the leg's stream; false while it is not
// subscribed yet.
func (v *fakeVenue) feed(ev cex.BookEvent) bool {
	v.mu.Lock()
	on := v.stream[ev.Symbol]
	v.mu.Unlock()
	if on == nil {
		return false
	}
	on(ev)
	return true
}

// silentSources stand for Binance, Coinbase and Direxion: cex's own loops
// ask them on the wall clock, and the test publishes F and the anchors
// itself.
type silentSources struct{}

func (silentSources) IndexClose(context.Context, cex.Symbol, time.Time) (decimal.Decimal, bool, error) {
	return decimal.Zero, false, nil
}
func (silentSources) TradingSchedule(context.Context) ([]cex.ScheduleDay, error) {
	return nil, errors.New("no schedule in tests")
}
func (silentSources) Quote(context.Context) (decimal.Decimal, decimal.Decimal, error) {
	return decimal.Zero, decimal.Zero, errors.New("no quote in tests")
}
func (silentSources) MinuteClose(context.Context, time.Time) (decimal.Decimal, bool, error) {
	return decimal.Zero, false, nil
}
func (silentSources) MinuteCloses(context.Context, time.Time, time.Time) ([]cex.FXMinute, error) {
	return nil, nil
}
func (silentSources) NAV(context.Context, string, time.Time) (decimal.Decimal, bool, error) {
	return decimal.Zero, false, cex.ErrIssuerKey
}
func (silentSources) Distributions(context.Context, string) ([]cex.Distribution, error) {
	return nil, cex.ErrIssuerKey
}

// testCex is the real cex manager with F and the anchors set by the test.
// notReady and settingsBad make it report the venue side not ready and the
// account's settings wrong, which the paper venue never does.
type testCex struct {
	*cex.CexManager
	mu          sync.Mutex
	fx          cex.FX
	anchors     cex.Anchors
	notReady    bool
	settingsBad bool
}

func (c *testCex) TradingReady() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.notReady && c.CexManager.TradingReady()
}

func (c *testCex) SettingsOK(leverage int) (bool, string) {
	c.mu.Lock()
	bad := c.settingsBad
	c.mu.Unlock()
	if bad {
		return false, "set wrong by the test"
	}
	return c.CexManager.SettingsOK(leverage)
}

func (c *testCex) set(notReady, settingsBad bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.notReady, c.settingsBad = notReady, settingsBad
}

func (c *testCex) FX(now time.Time) (cex.FX, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fx, !c.fx.ReceivedAt.IsZero() && now.Sub(c.fx.ReceivedAt) <= cex.DefaultConfig.FXMaxAge
}

func (c *testCex) Anchors() cex.Anchors {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.anchors
}

func (c *testCex) setAnchors(a cex.Anchors) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.anchors = a
}

func (c *testCex) setFX(price string, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fx = cex.FX{Price: dec(price), Bid: dec(price), Ask: dec(price), ReceivedAt: at}
}

// --- the rig ---

// rigOptions changes the defaults of a rig.
type rigOptions struct {
	capital string        // M, USDT; default 2000 (PRD appendix A)
	latency time.Duration // the paper venue's order latency on both legs; default 1 ms
	state   *store.State  // what the ledger is restored from, as at a restart; nil is an empty ledger. The paper positions are seeded from it
}

// rig is one loop on the real components, with its clock in the test's
// hands.
type rig struct {
	t       *testing.T
	mu      sync.Mutex
	now     time.Time
	fxFixed bool // F is not refreshed when the clock moves: it goes stale

	venue   *fakeVenue
	paper   *cex.PaperVenue
	cex     *cex.CexManager
	src     *testCex
	inv     *inventory.InventoryManager
	m       *ExecManager
	sink    *store.Fake
	signals chan os.Signal
	seq     [2]uint64
	cal     int // calibration books pushed so far, to alternate them
}

// testStrategy is the strategy of the tests: PRD appendix A's lines and fee,
// with an SMA of two minutes for the mean and sigma so that two minutes of
// calibrated books set mu and sigma exactly.
func testStrategy() strategy.Config {
	cfg := strategy.DefaultConfig
	cfg.MeanModel, cfg.MeanMinutes, cfg.SigmaMinutes = strategy.MeanSMA, 2, 2
	cfg.DIn, cfg.DOut, cfg.TakerFeeBps = 2, -0.5, 0.65
	return cfg
}

// newRig starts cex on the paper venue, restores an empty ledger and builds
// the loop at now, with the bell anchors 40 / 60 of the Session now is in.
func newRig(t *testing.T, now time.Time, opts rigOptions) *rig {
	t.Helper()
	if opts.capital == "" {
		opts.capital = "2000"
	}
	if opts.latency == 0 {
		opts.latency = time.Millisecond
	}
	r := &rig{t: t, now: now, venue: &fakeVenue{t: t, stream: map[cex.Symbol]func(cex.BookEvent){}}, sink: &store.Fake{}, signals: make(chan os.Signal, 8)}

	pair := cex.Pair{Bull: cex.LegConfig{Symbol: "SOXLUSDT", Ticker: "SOXL", BookTopic: cex.TopicBooks5}, Bear: cex.LegConfig{Symbol: "SOXSUSDT", Ticker: "SOXS", BookTopic: cex.TopicBooks5}}
	cexCfg := cex.DefaultConfig
	cexCfg.Pair, cexCfg.BookConnections = pair, 1
	r.paper = cex.NewPaperVenue(r.venue, cex.PaperConfig{Pair: pair, Latency: [2]time.Duration{opts.latency, opts.latency}, TakerFeeBps: 0.65, Leverage: 10})
	r.paper.UseStore(r.sink)
	var err error
	if r.cex, err = cex.NewManager(cexCfg, r.paper, silentSources{}, silentSources{}, silentSources{}); err != nil {
		t.Fatal(err)
	}
	r.cex.UseStore(r.sink)
	if r.inv, err = inventory.NewManager(inventory.Config{Capital: dec(opts.capital), Leverage: dec("10")}, pair, r.cex); err != nil {
		t.Fatal(err)
	}
	r.inv.UseStore(r.sink)
	state := store.State{}
	if opts.state != nil {
		state = *opts.state
	}
	if err := r.inv.Restore(state, nil); err != nil {
		t.Fatal(err)
	}
	r.paper.SeedPositions(r.inv.LedgerPositions())
	strat, err := strategy.New(testStrategy())
	if err != nil {
		t.Fatal(err)
	}
	r.src = &testCex{CexManager: r.cex}
	session := r.cex.Session(now)
	r.src.setAnchors(cex.Anchors{SessionID: session.ID, Kind: cex.AnchorBell, Bull: dec("40"), Bear: dec("60"), KnownAt: now})
	r.src.setFX("1", now)
	cfg := DefaultConfig
	cfg.SimOrderLatency = LegLatency{Bull: time.Millisecond, Bear: time.Millisecond}
	if r.m, err = NewManager(cfg, r.src, r.inv, strat, r.signals); err != nil {
		t.Fatal(err)
	}
	r.m.UseStore(r.sink)
	r.m.timing.clock = r.clock

	// cex runs its own loops; its first reference pass reads the rules, the
	// paper account's settings and positions.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.cex.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	waitFor(t, "cex ready", func() bool {
		return r.cex.TradingReady() && r.cex.Positions().Loaded && r.venue.feedable("SOXLUSDT") && r.venue.feedable("SOXSUSDT")
	})
	if ok, detail := r.cex.SettingsOK(10); !ok {
		t.Fatalf("paper settings: %s", detail)
	}
	r.m.ctx = ctx
	r.m.start(now)
	return r
}

// feedable reports whether the leg's book stream is subscribed.
func (v *fakeVenue) feedable(symbol cex.Symbol) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.stream[symbol] != nil
}

// clock is the loop's clock: the test's time.
func (r *rig) clock() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.now
}

// at moves the clock to t; F is read again at t unless it is held stale.
func (r *rig) at(t time.Time) {
	r.mu.Lock()
	r.now = t
	r.mu.Unlock()
	if !r.fxFixed {
		r.src.setFX("1", t)
	}
}

// step runs one round of the loop at the test's time.
func (r *rig) step() (done bool) { return r.m.step(r.clock()) }

// stepAt moves the clock and runs one round.
func (r *rig) stepAt(t time.Time) bool {
	r.at(t)
	return r.step()
}

// depth feeds one depth snapshot of a leg, arriving now, and waits until cex
// publishes it.
func (r *rig) depth(leg cex.Leg, bids, asks side) {
	r.t.Helper()
	r.seq[leg]++
	seq, symbol := r.seq[leg], legSymbol(leg)
	ev := cex.BookEvent{Symbol: symbol, Topic: cex.TopicBooks5, Action: cex.ActionSnapshot, Bids: bids, Asks: asks, Seq: seq, ExchangeTime: r.clock(), ReceivedAt: r.clock()}
	if !r.venue.feed(ev) {
		r.t.Fatalf("%s is not subscribed", symbol)
	}
	waitFor(r.t, "the depth published", func() bool { return r.cex.Books().ByLeg[leg].DepthSeq == seq })
}

// best feeds one books1 frame of a leg, newer than its depth, and waits
// until cex publishes it.
func (r *rig) best(leg cex.Leg, bid, ask cex.BookLevel) {
	r.t.Helper()
	r.seq[leg]++
	ev := cex.BookEvent{Symbol: legSymbol(leg), Topic: cex.TopicBooks1, Action: cex.ActionSnapshot, Bids: side{bid}, Asks: side{ask}, Seq: r.seq[leg], ExchangeTime: r.clock(), ReceivedAt: r.clock()}
	r.venue.feed(ev)
	waitFor(r.t, "the best price published", func() bool {
		b := r.cex.Books().ByLeg[leg]
		return b.BestBid.Price.Equal(bid.Price) && b.BestAsk.Price.Equal(ask.Price) && b.BestBid.Qty.Equal(bid.Qty)
	})
}

// books feeds both legs' depth.
func (r *rig) books(b pairBook) {
	r.t.Helper()
	r.depth(cex.Bull, b.bullBids, b.bullAsks)
	r.depth(cex.Bear, b.bearBids, b.bearAsks)
}

// results waits until every order has a final state: the paper venue
// matches each a millisecond after it is sent.
func (r *rig) results() {
	r.t.Helper()
	waitFor(r.t, "the orders' results", func() bool { return len(r.cex.UnresolvedOrders()) == 0 })
}

// trade runs one round at t that must send orders, waits for their
// results, and runs the round their arrival wakes in the process (cex
// closes OrdersChanged), still at t: it books them and marks the batch done.
func (r *rig) trade(t time.Time) {
	r.t.Helper()
	before := len(r.orders())
	r.stepAt(t)
	if len(r.orders()) == before {
		r.t.Fatalf("no order sent at %s; idle reason %q", showTime(t), r.m.idleReason)
	}
	r.results()
	r.step()
}

// idleAt runs one round at t that must send nothing, for the given reason.
func (r *rig) idleAt(t time.Time, want strategy.Reason) {
	r.t.Helper()
	before := len(r.orders())
	r.stepAt(t)
	if got := len(r.orders()); got != before {
		r.t.Fatalf("at %s: %d orders sent, want none", showTime(t), got-before)
	}
	if r.m.idleReason != want {
		r.t.Fatalf("at %s: idle reason %q, want %q", showTime(t), r.m.idleReason, want)
	}
}

// orders lists every order cex registered, of any cycle, in id order.
func (r *rig) orders() []cex.OrderRecord {
	var out []cex.OrderRecord
	for _, row := range r.sink.Rows("cex_order") {
		o := row.(store.OrderRow)
		if !seen(out, o.ClientOrderID) {
			out = append(out, cex.OrderRecordsFromRows([]store.OrderRow{o})...)
		}
	}
	return out
}

// seen reports whether the list holds the order.
func seen(orders []cex.OrderRecord, id string) bool {
	for _, o := range orders {
		if string(o.Request.ClientOrderID) == id {
			return true
		}
	}
	return false
}

// decisions returns the loop's decision rows, without the paper matches.
func (r *rig) decisions() []store.DecisionRow {
	var out []store.DecisionRow
	for _, row := range r.sink.Rows("decision") {
		if d := row.(store.DecisionRow); d.Kind != "paper_match" {
			out = append(out, d)
		}
	}
	return out
}

// decodeDetail reads a decision's or an event's detail.
func decodeDetail(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// eventKinds returns the kinds of the event rows the loop wrote.
func (r *rig) eventKinds() []string {
	var out []string
	for _, row := range r.sink.Rows("event") {
		if e := row.(store.EventRow); e.Module == "exec" {
			out = append(out, e.Kind)
		}
	}
	return out
}

// endMinute feeds a calibration book shortly before the end of the open
// minute and runs a round on it, so that the minute's bar is that book:
// alternating A and B, every pair of minutes has mu = 3.3333 and sigma = 4
// under the anchors 40 / 60 (see calibration).
func (r *rig) endMinute() {
	r.t.Helper()
	t := r.clock().Truncate(time.Minute).Add(59 * time.Second)
	if !t.After(r.clock()) {
		t = t.Add(time.Minute)
	}
	r.at(t)
	r.books(calibration(r.cal))
	r.cal++
	r.step()
}

// warmUp runs the two minutes after 05:01:00 of the rig's Session, A then
// B, and the first round of 05:03:00, where the mean is ready: mu = 3.3333,
// sigma = 4 under the anchors 40 / 60.
func (r *rig) warmUp() {
	r.t.Helper()
	r.idleAt(r.clock().Add(30*time.Second), strategy.ReasonDepthBull) // no book yet
	r.endMinute()
	r.endMinute()
	r.stepAt(r.clock().Add(time.Second))
	if !r.m.stats.Ready {
		r.t.Fatalf("the mean is not ready after the warm-up")
	}
}

// warnCapture counts the Warn lines of a message while a test runs.
type warnCapture struct {
	mu    sync.Mutex
	inner slog.Handler
	count map[string]int
}

func (c *warnCapture) Enabled(ctx context.Context, l slog.Level) bool { return c.inner.Enabled(ctx, l) }
func (c *warnCapture) WithAttrs(a []slog.Attr) slog.Handler           { return c }
func (c *warnCapture) WithGroup(string) slog.Handler                  { return c }
func (c *warnCapture) Handle(ctx context.Context, r slog.Record) error {
	if r.Level == slog.LevelWarn {
		c.mu.Lock()
		c.count[r.Message]++
		c.mu.Unlock()
	}
	return c.inner.Handle(ctx, r)
}

// snapshot returns the Warn lines logged so far, counted by message.
func (c *warnCapture) snapshot() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int, len(c.count))
	for message, n := range c.count {
		out[message] = n
	}
	return out
}

// captureWarns routes the process logger through a warnCapture until the
// test ends.
func captureWarns(t *testing.T) *warnCapture {
	old := logger.GlobalLogger
	c := &warnCapture{inner: old.Handler(), count: map[string]int{}}
	logger.GlobalLogger = slog.New(c)
	t.Cleanup(func() { logger.GlobalLogger = old })
	return c
}

// legSymbol is the test pair's contract of a leg.
func legSymbol(leg cex.Leg) cex.Symbol {
	if leg == cex.Bull {
		return "SOXLUSDT"
	}
	return "SOXSUSDT"
}

// waitFor polls cond until it holds: cex and the paper venue work on their
// own goroutines.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); !cond(); time.Sleep(200 * time.Microsecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// --- the books of the tests ---
//
// Under the anchors A = 40 / 60 and F = 1 the spread of two prices is
// D = 10000 x (P_bull / 40 + P_bear / 60 - 2). The pricing levels are the
// 2nd raw level of SOXL and the 5th of SOXS.

// calibration is the book of the minutes that set the mean and sigma. Book A
// has the mids 40.384 / 59.42, a minute spread of -0.6667; book B 40.416 /
// 59.42, +7.3333. A pair of them gives mu = 3.3333 (the mids of PRD appendix
// A, 40.40 / 59.42) and sigma = 4 (each is 4 from the mean). Neither gives a
// signal: under that mean A's deviations are -15 (bids) and +6.67 (asks), B's
// -7.5 and +14.17, inside the entry lines +-8 and away from both exit lines.
func calibration(i int) pairBook {
	bearBids := side{lv("59.415", "50"), lv("59.413", "50"), lv("59.41", "50"), lv("59.408", "50"), lv("59.405", "50")}
	bearAsks := side{lv("59.425", "50"), lv("59.427", "50"), lv("59.428", "50"), lv("59.429", "50"), lv("59.43", "50")}
	if i%2 == 0 { // A
		return pairBook{bullBids: side{lv("40.358", "100"), lv("40.35", "100")}, bullAsks: side{lv("40.41", "100"), lv("40.42", "100")}, bearBids: bearBids, bearAsks: bearAsks}
	}
	return pairBook{bullBids: side{lv("40.39", "100"), lv("40.38", "100")}, bullAsks: side{lv("40.442", "100"), lv("40.45", "100")}, bearBids: bearBids, bearAsks: bearAsks}
}

// entryBook is PRD appendix A example 1: the bids SOXL 40.45 x 300, 40.44 x
// 500 and SOXS 59.437 x 0.4, 59.431 x 60, 59.428 x 30, 59.425 x 30, 59.420 x
// 20 give a short at x = D(40.44, 59.420) - 3.3333 = 13.3333 - 3.3333 =
// +10.00. The asks are far from a long signal.
func entryBook() pairBook {
	return pairBook{
		bullBids: side{lv("40.45", "300"), lv("40.44", "500")}, bullAsks: side{lv("40.47", "300"), lv("40.48", "300")},
		bearBids: side{lv("59.437", "0.4"), lv("59.431", "60"), lv("59.428", "30"), lv("59.425", "30"), lv("59.420", "20")},
		bearAsks: side{lv("59.445", "50"), lv("59.45", "50"), lv("59.455", "50"), lv("59.46", "50"), lv("59.465", "50")},
	}
}

// exitBook is PRD appendix A example 2's asks: SOXL 40.37 x 300, 40.38 x
// 400; SOXS 59.385 x 100, 59.391 x 40, 59.394 x 30, 59.396 x 20, 59.400 x
// 10. A short closes at x = D(40.38, 59.400) - 3.3333 = -5 - 3.3333 =
// -8.33 <= -0.5 x 4 = -2.
func exitBook() pairBook {
	return pairBook{
		bullBids: side{lv("40.35", "300"), lv("40.34", "300")}, bullAsks: side{lv("40.37", "300"), lv("40.38", "400")},
		bearBids: side{lv("59.37", "50"), lv("59.365", "50"), lv("59.36", "50"), lv("59.355", "50"), lv("59.35", "50")},
		bearAsks: side{lv("59.385", "100"), lv("59.391", "40"), lv("59.394", "30"), lv("59.396", "20"), lv("59.400", "10")},
	}
}
