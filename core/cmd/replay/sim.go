package main

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/config"
	"etf-pair-arbitrage/core/exec"
	"etf-pair-arbitrage/core/inventory"
	"etf-pair-arbitrage/core/store"
	"etf-pair-arbitrage/core/strategy"
)

// --- the virtual clock ---

// vclock is the replay's clock: the time moves only when the driver moves
// it, and a timer fires only when the driver fires it, on the driver's
// goroutine, in the order of its time and then of its registration.
type vclock struct {
	now    time.Time
	timers timerHeap
	seq    uint64
}

func (c *vclock) Now() time.Time { return c.now }

func (c *vclock) AfterFunc(d time.Duration, f func()) {
	c.seq++
	heap.Push(&c.timers, timer{at: c.now.Add(d), seq: c.seq, f: f})
}

// due is the time of the earliest timer; ok is false without one.
func (c *vclock) due() (time.Time, bool) {
	if len(c.timers) == 0 {
		return time.Time{}, false
	}
	return c.timers[0].at, true
}

// fire runs every timer due at or before t, each at its own time.
func (c *vclock) fire(t time.Time) {
	for len(c.timers) > 0 && !c.timers[0].at.After(t) {
		next := heap.Pop(&c.timers).(timer)
		c.now = next.at
		next.f()
	}
}

type timer struct {
	at  time.Time
	seq uint64
	f   func()
}

type timerHeap []timer

func (h timerHeap) Len() int { return len(h) }
func (h timerHeap) Less(i, j int) bool {
	if !h[i].at.Equal(h[j].at) {
		return h[i].at.Before(h[j].at)
	}
	return h[i].seq < h[j].seq
}
func (h timerHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *timerHeap) Push(x any)   { *h = append(*h, x.(timer)) }
func (h *timerHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

// --- the replay venue ---

// replayVenue is the exchange behind the paper venue in a replay: it gives
// the contracts' rules and carries the recorded book frames, which the
// driver delivers by their arrival. It has no account: the paper venue
// keeps that, so none of its account methods is ever called.
type replayVenue struct {
	rules   map[cex.Symbol]cex.MarketRule
	onEvent map[cex.Symbol]func(cex.BookEvent)
	onDown  map[cex.Symbol]func(error)
}

var _ cex.Venue = (*replayVenue)(nil)

var errNoAccount = errors.New("replay: the replay venue has no account")

// bitgetRules are the two contracts' rules as read from the exchange
// (core/SPEC.md 5.1, read 2026-11-24).
func bitgetRules(bull, bear cex.Symbol) map[cex.Symbol]cex.MarketRule {
	d := decimal.RequireFromString
	return map[cex.Symbol]cex.MarketRule{
		bull: {Symbol: bull, PriceTick: d("0.01"), QtyStep: d("0.01"), MinQty: d("0.01"), MinNotional: d("5"), Online: true},
		bear: {Symbol: bear, PriceTick: d("0.001"), QtyStep: d("0.01"), MinQty: d("0.01"), MinNotional: d("5"), Online: true},
	}
}

func (v *replayVenue) Name() string { return "replay" }
func (v *replayVenue) MarketRules(context.Context, []cex.Symbol) (map[cex.Symbol]cex.MarketRule, error) {
	return v.rules, nil
}
func (v *replayVenue) AccountSettings(context.Context, []cex.Symbol) (cex.AccountSettings, error) {
	return cex.AccountSettings{}, errNoAccount
}
func (v *replayVenue) Positions(context.Context, []cex.Symbol) (cex.PositionSet, error) {
	return cex.PositionSet{}, errNoAccount
}
func (v *replayVenue) OrderByClientID(context.Context, cex.Symbol, cex.ClientOrderID) (cex.OrderRecord, error) {
	return cex.OrderRecord{}, errNoAccount
}
func (v *replayVenue) RecentOrders(context.Context, cex.Symbol, time.Time) ([]cex.OrderRecord, error) {
	return nil, errNoAccount
}
func (v *replayVenue) Fills(context.Context, time.Time) ([]cex.Fill, error) { return nil, errNoAccount }
func (v *replayVenue) FinancialRecords(context.Context, time.Time) ([]cex.FinancialRecord, error) {
	return nil, errNoAccount
}

// FundingHistory has no settled rates: the replay books no funding.
func (v *replayVenue) FundingHistory(context.Context, cex.Symbol, int) ([]cex.FundingSettlement, error) {
	return nil, nil
}
func (v *replayVenue) SubscribeBooks(_ context.Context, subs []cex.BookSubscription, onEvent func(cex.BookEvent), onDown func(error)) error {
	v.onEvent[subs[0].Symbol], v.onDown[subs[0].Symbol] = onEvent, onDown
	return nil
}
func (v *replayVenue) SubscribePrivate(context.Context, []cex.Symbol, func(cex.PrivateEvent), func(error)) error {
	return errNoAccount
}
func (v *replayVenue) PlaceOrder(context.Context, cex.OrderRequest) (cex.OrderAck, error) {
	return cex.OrderAck{}, errNoAccount
}

// deliver hands one recorded input to the leg's subscriber.
func (v *replayVenue) deliver(symbol cex.Symbol, in *input) {
	if in.down {
		if f := v.onDown[symbol]; f != nil {
			f(errors.New("replay: the recorder's book feed was lost"))
		}
		return
	}
	if f := v.onEvent[symbol]; f != nil {
		f(in.ev)
	}
}

// noSources stand for Binance, Coinbase and Direxion: a synchronous manager
// reads none of them; F and the anchors come from the history (replayCex).
type noSources struct{}

var errNoSource = errors.New("replay: no live source")

func (noSources) IndexClose(context.Context, cex.Symbol, time.Time) (decimal.Decimal, bool, error) {
	return decimal.Zero, false, errNoSource
}
func (noSources) TradingSchedule(context.Context) ([]cex.ScheduleDay, error) { return nil, errNoSource }
func (noSources) Quote(context.Context) (decimal.Decimal, decimal.Decimal, error) {
	return decimal.Zero, decimal.Zero, errNoSource
}
func (noSources) MinuteClose(context.Context, time.Time) (decimal.Decimal, bool, error) {
	return decimal.Zero, false, errNoSource
}
func (noSources) MinuteCloses(context.Context, time.Time, time.Time) ([]cex.FXMinute, error) {
	return nil, nil
}
func (noSources) NAV(context.Context, string, time.Time) (decimal.Decimal, bool, error) {
	return decimal.Zero, false, cex.ErrIssuerKey
}
func (noSources) Distributions(context.Context, string) ([]cex.Distribution, error) {
	return nil, cex.ErrIssuerKey
}

// --- F and the anchors from the history ---

// refEvent is one change of F or of the anchors, at the moment the process
// knew it.
type refEvent struct {
	at      int64
	fx      *cex.FX
	anchors *cex.Anchors
}

// replayCex is the manager as the loop reads it in a replay: F and the
// anchors are the history's, published at their known times; everything
// else is the manager's.
type replayCex struct {
	*cex.CexManager
	fx       cex.FX
	fxMaxAge time.Duration
	anchors  cex.Anchors
	changed  chan struct{}
}

func (c *replayCex) FX(now time.Time) (cex.FX, bool) {
	return c.fx, !c.fx.ReceivedAt.IsZero() && now.Sub(c.fx.ReceivedAt) <= c.fxMaxAge
}
func (c *replayCex) Anchors() cex.Anchors              { return c.anchors }
func (c *replayCex) ReferenceChanged() <-chan struct{} { return c.changed }

// apply publishes one change and wakes the loop.
func (c *replayCex) apply(e refEvent) {
	if e.fx != nil {
		c.fx = *e.fx
	}
	if e.anchors != nil {
		c.anchors = *e.anchors
	}
	close(c.changed)
	c.changed = make(chan struct{})
}

// --- the in-memory store ---

// memSink keeps what the replay's modules record: the last row of every
// order, cycle and lot, every fill, the decisions, and the per-second
// samples when asked to.
type memSink struct {
	mu          sync.Mutex
	keepSamples bool
	orders      map[string]store.OrderRow
	orderSeq    []string // client ids in the order of their first row
	fills       map[string]store.FillRow
	fillSeq     []string
	cycles      map[string]store.CycleRow
	cycleSeq    []string
	lots        map[string]store.LotRow
	lotSeq      []string
	decisions   []store.DecisionRow // every decision but paper_match, which is kept only with the samples
	matches     []store.DecisionRow
	samples     []store.SignalSampleRow
	minutes     []store.MinuteBarRow
	sampled     int
	idle        map[string]int
}

func newMemSink(keepSamples bool) *memSink {
	return &memSink{keepSamples: keepSamples, orders: map[string]store.OrderRow{}, fills: map[string]store.FillRow{}, cycles: map[string]store.CycleRow{}, lots: map[string]store.LotRow{}, idle: map[string]int{}}
}

func (s *memSink) Record(rows ...store.Row) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, row := range rows {
		switch r := row.(type) {
		case store.OrderRow:
			if _, seen := s.orders[r.ClientOrderID]; !seen {
				s.orderSeq = append(s.orderSeq, r.ClientOrderID)
			}
			s.orders[r.ClientOrderID] = r
		case store.FillRow:
			if _, seen := s.fills[r.ExecID]; !seen {
				s.fillSeq = append(s.fillSeq, r.ExecID)
				s.fills[r.ExecID] = r
			}
		case store.CycleRow:
			if _, seen := s.cycles[r.CycleID]; !seen {
				s.cycleSeq = append(s.cycleSeq, r.CycleID)
			}
			s.cycles[r.CycleID] = r
		case store.LotRow:
			if _, seen := s.lots[r.LotID]; !seen {
				s.lotSeq = append(s.lotSeq, r.LotID)
			}
			s.lots[r.LotID] = r
		case store.DecisionRow:
			switch {
			case r.Kind != "paper_match":
				s.decisions = append(s.decisions, r)
			case s.keepSamples:
				s.matches = append(s.matches, r)
			}
		case store.SignalSampleRow:
			s.sampled++
			s.idle[r.IdleReason]++
			if s.keepSamples {
				s.samples = append(s.samples, r)
			}
		case store.MinuteBarRow:
			s.minutes = append(s.minutes, r)
		}
	}
}

func (s *memSink) WriteSync(rows ...store.Row) error { s.Record(rows...); return nil }
func (s *memSink) Healthy() bool                     { return true }

// --- one simulation ---

// simConfig is one replay run: the configuration, the window and what the
// ledger and the loop start from.
type simConfig struct {
	name        string
	cfg         *config.Config
	start, end  time.Time
	refs        []refEvent // ascending by at
	state       store.State
	bars        []store.MinuteBarRow
	facts       []store.AnchorFactRow
	lastEntry   *store.LotRow
	keepSamples bool
}

// sim is one simulation on the real components: the cex manager on the
// paper venue on the replay venue, inventory, strategy and the exec loop,
// all on one virtual clock and driven from one goroutine.
type sim struct {
	sc    simConfig
	clock *vclock
	venue *replayVenue
	paper *cex.PaperVenue
	cmgr  *cex.CexManager
	view  *replayCex
	inv   *inventory.InventoryManager
	loop  *exec.ExecManager
	sink  *memSink
	cur   time.Time // the moment the inputs delivered last arrived
	ref   int       // refs[:ref] are applied
	steps int64     // Advance calls
}

// newSim builds the components at the window's start and starts them as
// main does: the ledger restored, the paper account seeded from it, the
// loop restored and started.
func newSim(sc simConfig) (*sim, error) {
	cfg := *sc.cfg
	cfg.Cex.BookConnections = 1 // the recording is one stream per topic: since 2026-11-26 the recorder keeps the first of its connections' copies
	s := &sim{sc: sc, clock: &vclock{now: sc.start}, sink: newMemSink(sc.keepSamples), cur: sc.start}
	pair := cfg.Cex.Pair
	s.venue = &replayVenue{rules: bitgetRules(pair.Bull.Symbol, pair.Bear.Symbol), onEvent: map[cex.Symbol]func(cex.BookEvent){}, onDown: map[cex.Symbol]func(error){}}
	s.paper = cex.NewPaperVenue(s.venue, cex.PaperConfig{Pair: pair,
		Latency:     [2]time.Duration{cex.Bull: cfg.Exec.SimOrderLatency.Bull, cex.Bear: cfg.Exec.SimOrderLatency.Bear},
		TakerFeeBps: cfg.Strategy.TakerFeeBps, Leverage: int(cfg.Inventory.Leverage.IntPart()), DepthMaxAge: cfg.Cex.DepthMaxAge, Clock: s.clock})
	s.paper.UseStore(s.sink)
	var err error
	if s.cmgr, err = cex.NewManager(cfg.Cex, s.paper, noSources{}, noSources{}, noSources{}, cex.WithClock(s.clock), cex.Synchronous()); err != nil {
		return nil, err
	}
	s.cmgr.UseStore(s.sink)
	ctx := context.Background()
	if err := s.cmgr.StartSynchronous(ctx); err != nil {
		return nil, err
	}
	s.view = &replayCex{CexManager: s.cmgr, fxMaxAge: cfg.Cex.FXMaxAge, changed: make(chan struct{})}
	if s.inv, err = inventory.NewManager(cfg.Inventory, pair, s.cmgr, inventory.WithClock(s.clock)); err != nil {
		return nil, err
	}
	s.inv.UseStore(s.sink)
	if err := s.inv.Restore(sc.state, nil); err != nil {
		return nil, err
	}
	s.paper.SeedPositions(s.inv.LedgerPositions())
	strat, err := strategy.New(cfg.Strategy)
	if err != nil {
		return nil, err
	}
	if s.loop, err = exec.NewManager(cfg.Exec, s.view, s.inv, strat, nil, exec.WithClock(s.clock)); err != nil {
		return nil, err
	}
	s.loop.UseStore(s.sink)
	s.loop.Restore(sc.bars, sc.facts, sc.lastEntry, nil)
	s.applyRefs(sc.start) // what was known at the start
	if leave, err := startWithin(startTimeout, s.loop.Start); err != nil || leave {
		return nil, fmt.Errorf("replay: the loop did not start: leave %t, %v", leave, err)
	}
	return s, nil
}

// startTimeout bounds the loop's start in wall time: its recovery retries
// a failing step after a wall-clock wait, which in a replay would spin.
const startTimeout = 30 * time.Second

// startWithin runs start under a context that is cancelled when it has not
// returned within d, and left alive when it has: the loop keeps that
// context for its reads.
func startWithin(d time.Duration, start func(ctx context.Context) (bool, error)) (bool, error) {
	ctx, cancel := context.WithCancel(context.Background())
	timer := time.AfterFunc(d, cancel)
	leave, err := start(ctx)
	if !timer.Stop() {
		return leave, fmt.Errorf("replay: the start took longer than %s: %w", d, err)
	}
	return leave, err
}

// applyRefs publishes every change of F and the anchors known by t.
func (s *sim) applyRefs(t time.Time) {
	for s.ref < len(s.sc.refs) && s.sc.refs[s.ref].at <= t.UnixMilli() {
		s.view.apply(s.sc.refs[s.ref])
		s.ref++
	}
}

// feed delivers one recorded input. Inputs come in arrival order; before
// the first one of a new millisecond, the one before is settled (its due
// timers, then the loop) and everything due in between runs at its time.
func (s *sim) feed(in *input) {
	at := time.UnixMilli(in.at)
	if at.After(s.cur) {
		s.settle(s.cur)
		s.runUntil(at)
		s.cur = at
		s.clock.now = at
		s.applyRefs(at)
	}
	symbol := s.sc.cfg.Cex.Pair.Bull.Symbol
	if in.leg == cex.Bear {
		symbol = s.sc.cfg.Cex.Pair.Bear.Symbol
	}
	s.venue.deliver(symbol, in)
}

// settle ends the moment t: the paper orders that meet their books at t,
// then the loop.
func (s *sim) settle(t time.Time) {
	s.clock.fire(t)
	s.clock.now = t
	s.advance(t)
}

// runUntil runs everything due before t, in time order: the changes of F
// and the anchors, the paper venue's matches and the loop's ticks.
func (s *sim) runUntil(t time.Time) {
	for {
		next := s.loop.NextTick()
		if due, has := s.clock.due(); has && due.Before(next) {
			next = due
		}
		if s.ref < len(s.sc.refs) {
			if at := time.UnixMilli(s.sc.refs[s.ref].at); at.Before(next) {
				next = at
			}
		}
		if !next.Before(t) {
			return
		}
		s.clock.now = next
		s.applyRefs(next)
		s.settle(next)
	}
}

// advance runs the loop at t.
func (s *sim) advance(t time.Time) {
	s.steps++
	s.loop.Advance(t)
}

// finish runs the replay to its end.
func (s *sim) finish() {
	s.settle(s.cur)
	s.runUntil(s.sc.end)
}
