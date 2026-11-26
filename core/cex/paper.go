package cex

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/logger"
	"etf-pair-arbitrage/core/store"
)

// PaperConfig is what the paper venue needs beyond the venue it wraps.
type PaperConfig struct {
	Pair        Pair             // the two contracts
	Latency     [2]time.Duration // by leg: an order meets its leg's book this long after it is sent
	TakerFeeBps float64          // the fee of a fill, in basis points of its value
	Leverage    int              // the leverage the account settings are reported with
	DepthMaxAge time.Duration    // cex.depth_max_age: a depth whose best price arrived later than this after it is stale at an order's arrival
	Clock       Clock            // nil: the wall clock and real timers; a replay's virtual clock otherwise
}

// paperTimings are the paper venue's clock and timer; tests replace them.
type paperTimings struct {
	clock func() time.Time
	after func(d time.Duration, f func()) // runs f once, d from now, on a goroutine of its own
}

// paperFeeDecimals is the scale a paper fill's fee is cut to, toward zero:
// the exchange reports fills' and orders' fees cut to 8 decimals (the
// adapter's feeDecimals). An order's fee is the sum of its fills' fees;
// whether the exchange cuts per fill or the order's total once is to be
// confirmed on a real order with several fills.
const paperFeeDecimals = 8

// fundingInterval is the grid of the contracts' funding settlements: every
// eight hours from midnight UTC.
const fundingInterval = 8 * time.Hour

// PaperVenue is a Venue for paper mode. Public data comes from the real
// venue it wraps: the rules, the settled funding rates, the book streams.
// The account side is local: orders are matched against the venue's own
// copy of the two books, positions and funding records are kept here, and
// the wrapped venue never receives an order or any account request.
type PaperVenue struct {
	real    Venue
	cfg     PaperConfig
	fee     decimal.Decimal // the taker fee as a fraction of a fill's value
	symbols [2]Symbol
	timing  paperTimings
	store   store.Sink // where the matches are recorded; nil records nothing

	// emit makes the private events of one match reach the subscriber
	// before those of the next: each order is matched on a timer goroutine
	// of its own, while a venue delivers its events one at a time, in the
	// order it produced them. It is held over a match and the delivery of
	// its events, and taken before mu.
	emit sync.Mutex

	mu        sync.Mutex
	books     [2]legBook  // each leg's raw book, built from the forwarded frames
	bookConns [2]int      // by leg: the book connections that are up
	taken     [2]LegTaken // by leg: what the paper orders took from the book they met
	positions [2]Position
	orders    map[ClientOrderID]*paperOrder
	fills     []Fill
	nextOrder int
	onEvent   func(PrivateEvent) // the private stream's subscriber; nil before SubscribePrivate

	nextSettle [2]time.Time      // by leg: the next funding settlement to capture
	settling   []paperSettlement // settlements captured, waiting for the venue to publish their rate
	records    []FinancialRecord // the paper funding records, oldest first
}

var _ Venue = (*PaperVenue)(nil)

// paperOrder is one paper order: its record, and the book it was sent on.
type paperOrder struct {
	rec    OrderRecord
	atSend *Book // the leg's book when the order was sent: what the decision saw, or the nearest thing to it
}

// paperSettlement is one leg's position and mid price at a funding
// settlement.
type paperSettlement struct {
	leg  Leg
	at   time.Time
	qty  decimal.Decimal // signed
	mid  decimal.Decimal
	seen time.Time // when it was captured
}

// NewPaperVenue wraps the real venue. Only its public methods are ever
// called.
func NewPaperVenue(real Venue, cfg PaperConfig) *PaperVenue {
	p := &PaperVenue{real: real, cfg: cfg, fee: decimal.NewFromFloat(cfg.TakerFeeBps).Div(decimal.NewFromInt(10000)),
		symbols: [2]Symbol{cfg.Pair.Bull.Symbol, cfg.Pair.Bear.Symbol}, orders: map[ClientOrderID]*paperOrder{},
		timing: paperTimings{clock: time.Now, after: func(d time.Duration, f func()) { time.AfterFunc(d, f) }}}
	if cfg.Clock != nil {
		p.timing = paperTimings{clock: cfg.Clock.Now, after: cfg.Clock.AfterFunc}
	}
	for _, leg := range Legs {
		p.positions[leg].Symbol = p.symbols[leg]
	}
	return p
}

// UseStore sets where the venue records each match. Call it before any
// order is placed.
func (p *PaperVenue) UseStore(s store.Sink) { p.store = s }

// SeedPositions sets the paper positions, signed per leg, at a restart: the
// ledger's holdings stand in for the account the process no longer
// remembers. A paper position has a quantity and no average price: nothing
// reads one.
func (p *PaperVenue) SeedPositions(qty [2]decimal.Decimal) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.timing.clock()
	for _, leg := range Legs {
		p.positions[leg].Qty, p.positions[leg].UpdatedAt = qty[leg], now
	}
	logger.GlobalLogger.Info("cex: paper positions seeded", "bull", qty[Bull], "bear", qty[Bear])
}

// Name is the wrapped venue's name, marked as paper.
func (p *PaperVenue) Name() string { return p.real.Name() + "-paper" }

// --- public data: the wrapped venue ---

func (p *PaperVenue) MarketRules(ctx context.Context, symbols []Symbol) (map[Symbol]MarketRule, error) {
	return p.real.MarketRules(ctx, symbols)
}

func (p *PaperVenue) FundingHistory(ctx context.Context, symbol Symbol, limit int) ([]FundingSettlement, error) {
	return p.real.FundingHistory(ctx, symbol, limit)
}

// SubscribeBooks subscribes at the wrapped venue and forwards every frame
// and loss, after applying it to the venue's own book of that leg: orders
// are matched on these books alone. A leg may be subscribed on several
// connections; its own book takes every connection's frames and loses its
// depth, like the manager's, when the last of them is lost.
func (p *PaperVenue) SubscribeBooks(ctx context.Context, subs []BookSubscription, onEvent func(BookEvent), onDown func(error)) error {
	leg, _ := p.legOf(subs[0].Symbol)
	up := false // this connection has delivered a frame since it last connected; under mu
	return p.real.SubscribeBooks(ctx, subs,
		func(ev BookEvent) {
			p.mu.Lock()
			if !up {
				up = true
				p.bookConns[leg]++
			}
			p.mu.Unlock()
			p.applyBookEvent(ev)
			onEvent(ev)
		},
		func(err error) {
			p.mu.Lock()
			if up {
				up = false
				p.bookConns[leg]--
			}
			if p.bookConns[leg] == 0 {
				p.books[leg].dropDepth()
			}
			p.mu.Unlock()
			onDown(err)
		})
}

// applyBookEvent takes one forwarded frame into the leg's own book, and at
// the first frame after a funding settlement captures the leg's position
// and mid price for the paper funding record. A depth frame that leaves the
// leg with the depth of its own seq, and no newer books1 contradicting it,
// also forgets the earlier paper takes it shows gone (observeDepth). The
// same frame again from another connection is checked again, which changes
// nothing; an older frame is not.
func (p *PaperVenue) applyBookEvent(ev BookEvent) {
	leg, ok := p.legOf(ev.Symbol)
	if !ok {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	b := &p.books[leg]
	if ev.Topic == TopicBooks1 {
		b.applyBest(ev)
	} else {
		b.applyDepth(ev) // an update that does not chain drops the depth; the manager subscribes the leg again
		if b.depth != nil && b.depth.seq == ev.Seq && b.conflictSeq == 0 {
			p.taken[leg].observeDepth(b.depth)
		}
	}
	p.captureSettlement(leg, ev.ReceivedAt)
}

// --- the account side: local ---

// AccountSettings reports the settings the strategy expects: paper mode has
// no account to check.
func (p *PaperVenue) AccountSettings(ctx context.Context, symbols []Symbol) (AccountSettings, error) {
	settings := AccountSettings{OneWay: true, BySymbol: map[Symbol]SymbolSetting{}}
	for _, s := range symbols {
		settings.BySymbol[s] = SymbolSetting{Crossed: true, Leverage: p.cfg.Leverage}
	}
	return settings, nil
}

// Positions returns the paper positions.
func (p *PaperVenue) Positions(ctx context.Context, symbols []Symbol) (PositionSet, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.positionSet(), nil
}

// positionSet is the paper positions as a set; a flat leg has no entry. The
// caller holds mu.
func (p *PaperVenue) positionSet() PositionSet {
	set := PositionSet{BySymbol: map[Symbol]Position{}, ReceivedAt: p.timing.clock(), Loaded: true}
	for _, pos := range p.positions {
		if !pos.Qty.IsZero() {
			set.BySymbol[pos.Symbol] = pos
		}
	}
	return set
}

// OrderByClientID answers from the paper orders of this run.
func (p *PaperVenue) OrderByClientID(ctx context.Context, symbol Symbol, id ClientOrderID) (OrderRecord, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	o, ok := p.orders[id]
	if !ok {
		return OrderRecord{}, ErrUnknownOrder
	}
	return o.rec, nil
}

// RecentOrders lists the paper orders of the contract sent since then.
func (p *PaperVenue) RecentOrders(ctx context.Context, symbol Symbol, since time.Time) ([]OrderRecord, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []OrderRecord
	for _, o := range p.orders {
		if o.rec.Request.Symbol == symbol && !o.rec.SubmittedAt.Before(since) {
			out = append(out, o.rec)
		}
	}
	return out, nil
}

// Fills lists the paper fills executed since then.
func (p *PaperVenue) Fills(ctx context.Context, since time.Time) ([]Fill, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []Fill
	for _, f := range p.fills {
		if !f.ExecTime.Before(since) {
			out = append(out, f)
		}
	}
	return out, nil
}

// SubscribePrivate registers the subscriber of the paper orders' reports,
// fills and position changes.
func (p *PaperVenue) SubscribePrivate(ctx context.Context, symbols []Symbol, onEvent func(PrivateEvent), onDown func(error)) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.onEvent = onEvent
	return nil
}

// PlaceOrder accepts a paper order at once and matches it after its leg's
// latency, on the leg's book as it is then. Nothing is sent to the wrapped
// venue.
func (p *PaperVenue) PlaceOrder(ctx context.Context, req OrderRequest) (OrderAck, error) {
	p.mu.Lock()
	now := p.timing.clock()
	p.nextOrder++
	orderID := OrderID(fmt.Sprintf("paper-%d-%d", now.UnixMilli(), p.nextOrder))
	p.orders[req.ClientOrderID] = &paperOrder{rec: OrderRecord{Request: req, OrderID: orderID, Status: StatusAcked, SubmittedAt: now, AckedAt: now},
		atSend: p.books[req.Leg].publish(req.Symbol, p.bookConns[req.Leg] > 0)}
	p.mu.Unlock()
	p.timing.after(p.cfg.Latency[req.Leg], func() { p.match(req.ClientOrderID) })
	return OrderAck{OrderID: orderID, ReceivedAt: now, AcceptedAt: now, CreatedAt: now}, nil
}

// match fills a paper order on the book it meets after its latency, by the
// arrival rule (ArrivalLevels, MatchIOC): the raw levels of a current depth
// that agrees with books1, the best level alone when the depth is
// contradicted, stale or missing, nothing when no connection is up; each
// level net of what earlier paper orders took from it. A reduce-only order
// takes no more than the paper position it reduces. Filled in full it is filled; otherwise the IOC
// remainder is cancelled, with whatever was taken. The result goes to the
// private subscriber as fills, the order's report and the new positions of
// both legs, and is recorded with the books. The events are delivered
// outside mu and under emit, so a later match's positions never reach the
// subscriber before this one's.
func (p *PaperVenue) match(id ClientOrderID) {
	p.emit.Lock()
	defer p.emit.Unlock()
	p.mu.Lock()
	o := p.orders[id]
	req, now := o.rec.Request, p.timing.clock()
	leg := req.Leg
	book := p.books[leg].publish(req.Symbol, p.bookConns[leg] > 0)
	arrival := ArrivalLevels(book, req.Side, p.cfg.DepthMaxAge)

	// The quantity to take.
	want := req.Qty
	signed := func(qty decimal.Decimal) decimal.Decimal { return qty }
	if req.Side == SideSell {
		signed = func(qty decimal.Decimal) decimal.Decimal { return qty.Neg() }
	}
	if req.ReduceOnly {
		// What the position holds against the order's side.
		reducible := decimal.Max(signed(p.positions[leg].Qty).Neg(), decimal.Zero)
		want = decimal.Min(want, reducible)
	}
	takes, available, remaining := MatchIOC(arrival, req.Side, req.Price, want, p.taken[leg].Of(req.Side))

	var events []PrivateEvent
	r := &o.rec
	for _, level := range takes {
		value := level.Qty.Mul(level.Price)
		fill := Fill{ExecID: fmt.Sprintf("%s-%d", r.OrderID, len(events)+1), OrderID: r.OrderID, ClientOrderID: id, Symbol: req.Symbol, Side: req.Side,
			Price: level.Price, Qty: level.Qty, Value: value, Fee: value.Mul(p.fee).Truncate(paperFeeDecimals), ExecTime: now, ReceivedAt: now}
		p.fills = append(p.fills, fill)
		events = append(events, PrivateEvent{Fill: &fill, ReceivedAt: now})
		r.CumExecQty, r.CumExecValue, r.Fee = r.CumExecQty.Add(level.Qty), r.CumExecValue.Add(value), r.Fee.Add(fill.Fee)
		p.positions[leg].Qty, p.positions[leg].UpdatedAt = p.positions[leg].Qty.Add(signed(level.Qty)), now
	}
	r.Status, r.ExchangeUpdatedAt = StatusFilled, now
	switch {
	case arrival.Source == ArrivalUnobservable:
		r.Status, r.CancelReason = StatusCancelled, CancelReasonUnobservable
	case r.CumExecQty.LessThan(req.Qty):
		r.Status, r.CancelReason = StatusCancelled, "ioc"
	}
	if r.CumExecQty.IsPositive() {
		r.AvgPrice, r.FeeKnown = r.CumExecValue.Div(r.CumExecQty), true
	}
	report, positions := *r, p.positionSet()
	events = append(events, PrivateEvent{Order: &report, ReceivedAt: now}, PrivateEvent{Positions: &positions, ReceivedAt: now})
	onEvent, atSend := p.onEvent, o.atSend
	p.mu.Unlock()

	p.recordMatch(report, atSend, book, arrival, available, remaining, now)
	if onEvent != nil {
		for _, ev := range events {
			onEvent(ev)
		}
	}
}

// recordMatch writes one decision row (kind paper_match) for a matched
// paper order, beside the decision that sent it: the order, its result, the
// leg's book when the venue took it and the book it was matched on, and
// what the arrival rule made of the latter - the source used, its frame
// and books1's, the levels available net of the earlier paper orders'
// takes and those left after this order, each list cut at its first
// paperBookLevels - so that the fill can be computed again from the
// record. The book at the send is the paper venue's own at that moment,
// which may be a frame ahead of the one the decision read.
func (p *PaperVenue) recordMatch(r OrderRecord, atSend, atMatch *Book, arrival ArrivalBook, available, remaining []BookLevel, now time.Time) {
	log := logger.GlobalLogger.Info
	if arrival.Source == ArrivalUnobservable {
		log = logger.GlobalLogger.Warn
	}
	log("cex: paper order matched", "client_order_id", r.Request.ClientOrderID, "symbol", r.Request.Symbol, "side", string(r.Request.Side),
		"price", r.Request.Price, "qty", r.Request.Qty, "status", string(r.Status), "cancel_reason", r.CancelReason, "cum_exec_qty", r.CumExecQty, "cum_exec_value_usdt", r.CumExecValue, "fee_usdt", r.Fee,
		"latency", now.Sub(r.SubmittedAt), "arrival_source", string(arrival.Source), "arrival_seq", arrival.Seq, "depth_seq_at_send", atSend.DepthSeq, "depth_seq_at_match", atMatch.DepthSeq, "conflict_at_match", atMatch.Conflict)
	if p.store == nil {
		return
	}
	p.store.Record(store.DecisionRow{TsMs: now.UnixMilli(), CycleID: r.Request.CycleID, LotID: r.Request.LotID, Kind: "paper_match",
		Detail: marshalDetail(map[string]any{
			"client_order_id": r.Request.ClientOrderID, "leg": r.Request.Leg.String(), "side": r.Request.Side,
			"price": r.Request.Price, "qty": r.Request.Qty, "reduce_only": r.Request.ReduceOnly, "status": r.Status, "cancel_reason": r.CancelReason,
			"cum_exec_qty": r.CumExecQty, "cum_exec_value": r.CumExecValue, "fee": r.Fee,
			"sent_ms": r.SubmittedAt.UnixMilli(), "matched_ms": now.UnixMilli(),
			"book_at_send": bookDetail(atSend, r.Request.Side), "book_at_match": bookDetail(atMatch, r.Request.Side),
			"arrival": map[string]any{"source": arrival.Source, "seq": arrival.Seq, "best_seq": atMatch.BestSeq, "received_ms": unixMs(arrival.ReceivedAt),
				"available": levelsDetail(available), "remaining": levelsDetail(remaining)}})})
}

// paperBookLevels is how many levels of the taken side a recorded book
// keeps.
const paperBookLevels = 5

// bookDetail is a book as recorded with a paper match: its sequence number
// and arrival, the best bid and ask, and the first levels of the side an
// order of the given side takes.
func bookDetail(b *Book, side Side) map[string]any {
	levels := b.Asks
	if side == SideSell {
		levels = b.Bids
	}
	return map[string]any{"depth_seq": b.DepthSeq, "depth_received_ms": unixMs(b.DepthReceivedAt), "conflict": b.Conflict,
		"best_bid": b.BestBid.Price, "best_ask": b.BestAsk.Price, "levels": levelsDetail(levels)}
}

// levelsDetail is the first levels of a side as [price, quantity] pairs.
func levelsDetail(levels []BookLevel) [][2]decimal.Decimal {
	out := make([][2]decimal.Decimal, 0, paperBookLevels)
	for _, l := range levels[:min(len(levels), paperBookLevels)] {
		out = append(out, [2]decimal.Decimal{l.Price, l.Qty})
	}
	return out
}

// --- paper funding ---

// captureSettlement notes, at the leg's first book frame at or after a
// funding settlement, the position and the mid price the settlement applies
// to. The first settlement followed is the next one after the venue's first
// frame. The caller holds mu.
func (p *PaperVenue) captureSettlement(leg Leg, now time.Time) {
	if p.nextSettle[leg].IsZero() {
		p.nextSettle[leg] = now.Truncate(fundingInterval).Add(fundingInterval)
	}
	if now.Before(p.nextSettle[leg]) {
		return
	}
	book := p.books[leg].publish(p.symbols[leg], true)
	if book.BestReceivedAt.IsZero() {
		return // no price yet: captured at the next frame that has one
	}
	at := p.nextSettle[leg]
	p.nextSettle[leg] = now.Truncate(fundingInterval).Add(fundingInterval)
	if qty := p.positions[leg].Qty; !qty.IsZero() {
		mid := book.BestBid.Price.Add(book.BestAsk.Price).Div(decimal.NewFromInt(2))
		p.settling = append(p.settling, paperSettlement{leg: leg, at: at, qty: qty, mid: mid, seen: now})
	}
}

// FinancialRecords returns every paper funding record of this run; since is
// not applied. A record is booked when the wrapped venue publishes the
// settlement's rate, which can be minutes after the settlement its time
// names, so a reader that asks "since my last read" would never see it. The
// manager keeps the records it has not seen by their id, and there are at
// most two per settlement.
//
// Before answering, every captured settlement whose rate is published by
// now is booked: a long position pays a positive rate, so the amount is
// -quantity (signed) x mid price x rate; a rate of zero books an amount of
// zero. A leg without a position at a settlement gets no record.
func (p *PaperVenue) FinancialRecords(ctx context.Context, since time.Time) ([]FinancialRecord, error) {
	p.mu.Lock()
	waiting := append([]paperSettlement(nil), p.settling...)
	p.mu.Unlock()

	// The settled rates of the legs that wait for one.
	rates := map[Leg]map[int64]decimal.Decimal{}
	for _, s := range waiting {
		if _, read := rates[s.leg]; read {
			continue
		}
		history, err := p.real.FundingHistory(ctx, p.symbols[s.leg], 10)
		if err != nil {
			return nil, fmt.Errorf("cex: paper funding: settled rates of %s: %w", p.symbols[s.leg], err)
		}
		rates[s.leg] = map[int64]decimal.Decimal{}
		for _, h := range history {
			rates[s.leg][h.At.UnixMilli()] = h.Rate
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	var still []paperSettlement
	for _, s := range p.settling {
		rate, published := rates[s.leg][s.at.UnixMilli()]
		if !published {
			still = append(still, s)
			continue
		}
		rec := FinancialRecord{ID: fmt.Sprintf("paper-funding-%s-%d", p.symbols[s.leg], s.at.UnixMilli()), Symbol: p.symbols[s.leg], Type: "paper_funding",
			Amount: s.qty.Neg().Mul(s.mid).Mul(rate), At: s.at}
		p.records = append(p.records, rec)
		logger.GlobalLogger.Info("cex: paper funding booked", "symbol", rec.Symbol, "settlement", showTime(s.at), "position", s.qty, "mid", s.mid, "rate", rate, "amount_usdt", rec.Amount)
	}
	p.settling = still
	return append([]FinancialRecord(nil), p.records...), nil
}

// legOf returns the leg that trades symbol.
func (p *PaperVenue) legOf(symbol Symbol) (Leg, bool) {
	for _, leg := range Legs {
		if p.symbols[leg] == symbol {
			return leg, true
		}
	}
	return 0, false
}
