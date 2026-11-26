package exec

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/inventory"
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

// gateVenue is the paper venue with a gate on its private stream: while a
// contract is held, the reports and fills of its orders, and the position
// sets, wait until the test releases them - the venue has the outcome, the
// process does not know it yet.
//
// Orders can be made unknown too: a parked contract's orders do not reach
// the paper venue until releaseParked (the venue has never seen them), a
// lost contract's orders reach it while the answer is lost; both answer
// ErrOrderStateUnknown. refuse, when set, answers an order with its error
// instead. Queries of a held contract's orders fail.
type gateVenue struct {
	*cex.PaperVenue
	mu     sync.Mutex
	held   map[cex.Symbol]bool
	queue  []cex.PrivateEvent
	on     func(cex.PrivateEvent)
	parked map[cex.Symbol]bool
	waits  []cex.OrderRequest
	lost   cex.Symbol
	unread atomic.Bool  // the positions cannot be read
	reads  atomic.Int64 // reads of the positions
	onRead func()       // run once inside the next read of the positions, as what happens while a slow read is on its way
	refuse func(req cex.OrderRequest) error
	placed map[cex.ClientOrderID]int // PlaceOrder calls per order
}

func (g *gateVenue) PlaceOrder(ctx context.Context, req cex.OrderRequest) (cex.OrderAck, error) {
	g.mu.Lock()
	if g.placed == nil {
		g.placed = map[cex.ClientOrderID]int{}
	}
	g.placed[req.ClientOrderID]++
	refuse, park, lose := g.refuse, g.parked[req.Symbol], g.lost == req.Symbol
	if park {
		g.waits = append(g.waits, req)
	}
	g.mu.Unlock()
	if refuse != nil {
		if err := refuse(req); err != nil {
			return cex.OrderAck{}, err
		}
	}
	if park {
		return cex.OrderAck{}, cex.ErrOrderStateUnknown
	}
	ack, err := g.PaperVenue.PlaceOrder(ctx, req)
	if lose {
		return cex.OrderAck{}, cex.ErrOrderStateUnknown
	}
	return ack, err
}

func (g *gateVenue) Positions(ctx context.Context, symbols []cex.Symbol) (cex.PositionSet, error) {
	g.reads.Add(1)
	g.mu.Lock()
	during := g.onRead
	g.onRead = nil
	g.mu.Unlock()
	if during != nil {
		during()
	}
	if g.unread.Load() {
		return cex.PositionSet{}, errors.New("the test makes the positions unreadable")
	}
	return g.PaperVenue.Positions(ctx, symbols)
}

func (g *gateVenue) OrderByClientID(ctx context.Context, symbol cex.Symbol, id cex.ClientOrderID) (cex.OrderRecord, error) {
	g.mu.Lock()
	held := g.held[symbol]
	g.mu.Unlock()
	if held {
		return cex.OrderRecord{}, errors.New("the test holds this contract's queries")
	}
	return g.PaperVenue.OrderByClientID(ctx, symbol, id)
}

// park keeps the symbols' next orders from the venue; lose loses the
// answer to them.
func (g *gateVenue) park(symbols ...cex.Symbol) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.parked = map[cex.Symbol]bool{}
	for _, s := range symbols {
		g.parked[s] = true
	}
}
func (g *gateVenue) lose(symbol cex.Symbol) { g.mu.Lock(); g.lost = symbol; g.mu.Unlock() }

// setRefuse sets what answers the orders instead of the venue; nil lets
// them through.
func (g *gateVenue) setRefuse(f func(req cex.OrderRequest) error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.refuse = f
}

// releaseParked hands the parked orders to the venue at last.
func (g *gateVenue) releaseParked() {
	g.mu.Lock()
	waits := g.waits
	g.waits, g.parked = nil, nil
	g.mu.Unlock()
	for _, req := range waits {
		g.PaperVenue.PlaceOrder(context.Background(), req)
	}
}

// duringRead sets what happens while the next read of the positions is on
// its way.
func (g *gateVenue) duringRead(f func()) { g.mu.Lock(); g.onRead = f; g.mu.Unlock() }

// placeCalls is how often the order was handed to the venue.
func (g *gateVenue) placeCalls(id cex.ClientOrderID) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.placed[id]
}

// RecentOrders gives the paper venue's orders as a real venue would: it
// knows nothing of the lots they were sent for.
func (g *gateVenue) RecentOrders(ctx context.Context, symbol cex.Symbol, since time.Time) ([]cex.OrderRecord, error) {
	recs, err := g.PaperVenue.RecentOrders(ctx, symbol, since)
	for i := range recs {
		recs[i].Request.LotID = ""
	}
	return recs, err
}

// placedTotal is the number of PlaceOrder calls of every order.
func (g *gateVenue) placedTotal() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, c := range g.placed {
		n += c
	}
	return n
}

func (g *gateVenue) SubscribePrivate(ctx context.Context, symbols []cex.Symbol, onEvent func(cex.PrivateEvent), onDown func(error)) error {
	g.mu.Lock()
	g.on = onEvent
	g.mu.Unlock()
	return g.PaperVenue.SubscribePrivate(ctx, symbols, g.deliver, onDown)
}

// deliver passes an event on, or keeps it while its contract is held.
func (g *gateVenue) deliver(ev cex.PrivateEvent) {
	g.mu.Lock()
	held := len(g.held) > 0 && (ev.Positions != nil || (ev.Order != nil && g.held[ev.Order.Request.Symbol]) || (ev.Fill != nil && g.held[ev.Fill.Symbol]))
	if held {
		g.queue = append(g.queue, ev)
	}
	on := g.on
	g.mu.Unlock()
	if !held {
		on(ev)
	}
}

// hold starts keeping the events of the symbols' orders.
func (g *gateVenue) hold(symbols ...cex.Symbol) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.held = map[cex.Symbol]bool{}
	for _, s := range symbols {
		g.held[s] = true
	}
}

// release delivers what was kept, in order, and opens the gate.
func (g *gateVenue) release() {
	g.mu.Lock()
	queue, on := g.queue, g.on
	g.queue, g.held = nil, nil
	g.mu.Unlock()
	for _, ev := range queue {
		on(ev)
	}
}

// heldEvents is how many events wait.
func (g *gateVenue) heldEvents() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.queue)
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
	notFound    map[cex.ClientOrderID]bool // orders cex reports as NotFound: its own verdict takes 30 s of wall time
	fxCloses    []cex.FXMinute             // the source's minute closes of F; cex's source has none in tests
	fxErr       error                      // when set, the minute closes of F cannot be read
	lateVerdict bool                       // MarkNeverArrived finds the order final already, as when its result came after the snapshot
}

func (c *testCex) MarkNeverArrived(id cex.ClientOrderID) bool {
	c.mu.Lock()
	late := c.lateVerdict
	c.mu.Unlock()
	if late {
		return false
	}
	return c.CexManager.MarkNeverArrived(id)
}

func (c *testCex) FXMinuteCloses(_ context.Context, from, to time.Time) ([]cex.FXMinute, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fxErr != nil {
		return nil, c.fxErr
	}
	var out []cex.FXMinute
	for _, m := range c.fxCloses {
		if !m.Minute.Before(from) && m.Minute.Before(to) {
			out = append(out, m)
		}
	}
	return out, nil
}

// CycleOrders is cex's snapshot with the orders the test names marked
// NotFound while their outcome is unknown.
func (c *testCex) CycleOrders(cycleID string) []cex.OrderRecord {
	orders := c.CexManager.CycleOrders(cycleID)
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range orders {
		if orders[i].Unknown && c.notFound[orders[i].Request.ClientOrderID] {
			orders[i].NotFound = true
		}
	}
	return orders
}

func (c *testCex) setNotFound(id cex.ClientOrderID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.notFound == nil {
		c.notFound = map[cex.ClientOrderID]bool{}
	}
	c.notFound[id] = true
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
	capital  string        // M, USDT; default 2000 (PRD appendix A)
	latency  time.Duration // the paper venue's order latency on both legs; default 1 ms
	state    *store.State  // what a restart read back: the ledger, the cycle's orders, the minutes and the last entry; nil is a first start
	exchange *exchange     // the exchange of an earlier process, which keeps its orders and positions; nil makes a new one, whose positions are seeded from the ledger
	sink     store.Sink    // where the rows go besides the rig's fake; nil: the fake alone
	perp     minuteSource  // the recorder's minutes for the loop's recovery
	thin     string        // exec.thin_leg: "" sends both legs together (the default), bear takes SOXS first
	exit2    *float64      // strategy.exit2; nil is one exit line
	stopD    float64       // exec.pair_stop_d; 0 keeps the default 10
}

// exchange is what outlives a process: the venue behind the paper venue,
// the paper venue with its orders and positions, and the gate on it.
type exchange struct {
	venue *fakeVenue
	paper *cex.PaperVenue
	gate  *gateVenue
	seq   [2]uint64 // the last book sequence fed per leg: the venue's books go on
}

// teeSink records into the rig's fake and a real store: the helpers read
// the fake, a restart reads the database.
type teeSink struct {
	fake *store.Fake
	real store.Sink
}

func (s teeSink) Record(rows ...store.Row) { s.real.Record(rows...); s.fake.Record(rows...) }
func (s teeSink) Healthy() bool            { return s.real.Healthy() }
func (s teeSink) WriteSync(rows ...store.Row) error {
	if err := s.real.WriteSync(rows...); err != nil {
		return err
	}
	return s.fake.WriteSync(rows...)
}

// rig is one loop on the real components, with its clock in the test's
// hands.
type rig struct {
	t       *testing.T
	mu      sync.Mutex
	now     time.Time
	fxFixed bool // F is not refreshed when the clock moves: it goes stale

	exchange *exchange
	venue    *fakeVenue
	paper    *cex.PaperVenue
	gate     *gateVenue // the paper venue as cex sees it
	stopCex  func()     // ends cex's loops, once
	cex      *cex.CexManager
	src      *testCex
	inv      *inventory.InventoryManager
	m        *ExecManager
	sink     *store.Fake
	signals  chan os.Signal
	cal      int // calibration books pushed so far, to alternate them
}

// testStrategy is the strategy of the tests: PRD appendix A's lines and fee,
// with an SMA of two minutes for the mean and sigma so that two minutes of
// calibrated books set mu and sigma exactly.
func testStrategy() strategy.Config {
	cfg := strategy.DefaultConfig
	cfg.MeanModel, cfg.MeanMinutes, cfg.SigmaMinutes = strategy.MeanSMA, 2, 2
	cfg.EntryIn, cfg.EntryOut, cfg.TakerFeeBps = 2, -0.5, 0.65
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
	r := &rig{t: t, now: now, sink: &store.Fake{}, signals: make(chan os.Signal, 8)}
	var sink store.Sink = r.sink
	if opts.sink != nil {
		sink = teeSink{fake: r.sink, real: opts.sink}
	}

	pair := cex.Pair{Bull: cex.LegConfig{Symbol: "SOXLUSDT", Ticker: "SOXL", BookTopic: cex.TopicBooks5}, Bear: cex.LegConfig{Symbol: "SOXSUSDT", Ticker: "SOXS", BookTopic: cex.TopicBooks5}}
	cexCfg := cex.DefaultConfig
	cexCfg.Pair, cexCfg.BookConnections = pair, 1
	shared := opts.exchange != nil
	if !shared {
		ex := &exchange{venue: &fakeVenue{t: t, stream: map[cex.Symbol]func(cex.BookEvent){}}}
		ex.paper = cex.NewPaperVenue(ex.venue, cex.PaperConfig{Pair: pair, Latency: [2]time.Duration{opts.latency, opts.latency}, TakerFeeBps: 0.65, Leverage: 10, DepthMaxAge: cex.DefaultConfig.DepthMaxAge})
		ex.paper.UseStore(sink)
		ex.gate = &gateVenue{PaperVenue: ex.paper}
		opts.exchange = ex
	}
	r.exchange = opts.exchange
	r.venue, r.paper, r.gate = r.exchange.venue, r.exchange.paper, r.exchange.gate
	var err error
	if r.cex, err = cex.NewManager(cexCfg, r.gate, silentSources{}, silentSources{}, silentSources{}); err != nil {
		t.Fatal(err)
	}
	r.cex.UseStore(sink)
	cfg := DefaultConfig
	cfg.SimOrderLatency = LegLatency{Bull: time.Millisecond, Bear: time.Millisecond}
	cfg.ThinLeg = opts.thin
	if opts.stopD != 0 {
		cfg.PairStopD = opts.stopD
	}
	var invOpts []inventory.Option
	if leg, ok := cfg.Thin(); ok {
		invOpts = append(invOpts, inventory.WithThinLeg(leg))
	}
	if r.inv, err = inventory.NewManager(inventory.Config{Capital: dec(opts.capital), Leverage: dec("10")}, pair, r.cex, invOpts...); err != nil {
		t.Fatal(err)
	}
	r.inv.UseStore(sink)
	state := store.State{}
	if opts.state != nil {
		state = *opts.state
	}
	// As main reads back: the cycle's orders into cex, the ledger on them;
	// a new paper account starts with the ledger's positions.
	orders := cex.OrderRecordsFromRows(state.Orders)
	r.cex.Restore(nil, orders)
	if state.Cycle != nil {
		orders = r.cex.CycleOrders(state.Cycle.CycleID)
	}
	if err := r.inv.Restore(state, orders); err != nil {
		t.Fatal(err)
	}
	if !shared {
		r.paper.SeedPositions(r.inv.LedgerPositions())
	}
	scfg := testStrategy()
	scfg.Exit2 = opts.exit2
	strat, err := strategy.New(scfg)
	if err != nil {
		t.Fatal(err)
	}
	r.src = &testCex{CexManager: r.cex}
	session := r.cex.Session(now)
	r.src.setAnchors(cex.Anchors{SessionID: session.ID, Kind: cex.AnchorBell, Bull: dec("40"), Bear: dec("60"), KnownAt: now})
	r.src.setFX("1", now)
	if r.m, err = NewManager(cfg, r.src, r.inv, strat, r.signals); err != nil {
		t.Fatal(err)
	}
	r.m.UseStore(sink)
	r.m.Restore(state.MinuteBars, state.AnchorFacts, state.LastEntry, opts.perp)
	r.m.RestoreExitStage(state.ExitStage)
	r.m.timing.clock = r.clock

	// cex runs its own loops; its first reference pass reads the rules, the
	// paper account's settings and positions.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.cex.Run(ctx); close(done) }()
	r.stopCex = func() { cancel(); <-done }
	t.Cleanup(r.crash)
	waitFor(t, "cex ready", func() bool {
		return r.cex.TradingReady() && r.cex.Positions().Loaded && r.venue.feedable("SOXLUSDT") && r.venue.feedable("SOXSUSDT")
	})
	if ok, detail := r.cex.SettingsOK(10); !ok {
		t.Fatalf("paper settings: %s", detail)
	}
	r.m.ctx = ctx
	r.m.start(now)
	r.m.nextReconcile = now.Add(r.m.timing.reconcileEvery) // as the recovery leaves it
	return r
}

// crash ends this process's cex loops, as a crash would: the exchange and
// the database stay.
func (r *rig) crash() {
	if r.stopCex != nil {
		r.stopCex()
		r.stopCex = nil
	}
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

// step runs one round of the loop at the test's time, and the rounds that
// follow a read of the positions in the process: a read ends its round,
// and the loop runs again at once when the read published the positions,
// or at the next tick (200 ms) when it failed; the rig runs those rounds at
// the same time, at most three.
func (r *rig) step() (done bool) {
	for i := 0; ; i++ {
		reads := r.gate.reads.Load()
		if done = r.stepOnce(); done || i == 3 || r.gate.reads.Load() == reads {
			return done
		}
	}
}

// stepOnce runs exactly one round of the loop at the test's time.
func (r *rig) stepOnce() (done bool) { return r.m.step(r.clock()) }

// stepAt moves the clock and runs one round.
func (r *rig) stepAt(t time.Time) bool {
	r.at(t)
	return r.step()
}

// depth feeds one depth snapshot of a leg, arriving now, and waits until cex
// publishes it.
func (r *rig) depth(leg cex.Leg, bids, asks side) {
	r.t.Helper()
	r.exchange.seq[leg]++
	seq, symbol := r.exchange.seq[leg], legSymbol(leg)
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
	r.exchange.seq[leg]++
	ev := cex.BookEvent{Symbol: legSymbol(leg), Topic: cex.TopicBooks1, Action: cex.ActionSnapshot, Bids: side{bid}, Asks: side{ask}, Seq: r.exchange.seq[leg], ExchangeTime: r.clock(), ReceivedAt: r.clock()}
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

// orders lists every order cex registered, of any cycle, in the order they
// were registered, each as its latest row says.
func (r *rig) orders() []cex.OrderRecord {
	var out []cex.OrderRecord
	at := map[string]int{}
	for _, row := range r.sink.Rows("cex_order") {
		o := row.(store.OrderRow)
		rec := cex.OrderRecordsFromRows([]store.OrderRow{o})[0]
		if i, held := at[o.ClientOrderID]; held {
			out[i] = rec
			continue
		}
		at[o.ClientOrderID] = len(out)
		out = append(out, rec)
	}
	return out
}

// tradeOn runs one round at t that must send orders and waits for the
// results of those orders alone (an order of unknown outcome may stay), then
// runs the round their arrival wakes.
func (r *rig) tradeOn(t time.Time) {
	r.t.Helper()
	before := len(r.orders())
	r.stepAt(t)
	sent := r.orders()[before:]
	if len(sent) == 0 {
		r.t.Fatalf("no order sent at %s; idle reason %q", showTime(t), r.m.idleReason)
	}
	waitFor(r.t, "the results of the orders just sent", func() bool {
		for _, o := range r.cex.UnresolvedOrders() {
			for _, s := range sent {
				if o.Request.ClientOrderID == s.Request.ClientOrderID {
					return false
				}
			}
		}
		return true
	})
	r.step()
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
