package cex

import (
	"context"
	"fmt"
	"math"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/logger"
	"etf-pair-arbitrage/core/store"
)

// CexManager owns the exchange side and the reference data: the venue and
// the read-only sources, the two legs' books, F, the calendar, the anchors,
// the market rules, the account's settings, positions and funding records,
// and the orders. The other managers read its published snapshots and place
// orders through it.
//
// Run starts five loops. Four own a part of the mutable state each and
// publish immutable snapshots by pointer swap, closing a channel on every
// publication, so readers take no lock:
//   - the book loop (Run's own goroutine): the two leg books;
//   - the F loop: the USDT-USD rate;
//   - the anchor loop: the Session's anchor state;
//   - the reference loop: rules, settings, positions, funding, the calendar check.
//
// The fifth, the order loop, follows the order table (orders.go), which has
// a lock of its own because exec's loop, the send goroutines and the private
// stream change it too.
type CexManager struct {
	// bound by NewManager
	cfg      Config
	venue    Venue
	index    IndexSource
	fxSource FXSource
	issuer   IssuerSource
	calendar *Calendar
	timing   timings
	symbols  [2]Symbol   // by leg
	tickers  [2]string   // by leg
	splits   []splitBan  // the configured splits: entries stop at each one's start
	payouts  []payoutDay // the configured dividends

	// bound by UseStore and Restore, before Run
	store    store.Sink   // nil records nothing
	restored []AnchorFact // the anchor facts of the Session the process restarted in

	// inboxes read by Run
	bookInputs    chan bookInput // frames and connection losses, from the venue's read goroutines
	droppedFrames atomic.Uint64  // inputs bookInputs could not take

	// published snapshots, each replaced as a whole
	books       atomic.Pointer[booksView]     // written by the book loop alone
	reference   atomic.Pointer[referenceView] // written under referenceMu
	referenceMu sync.Mutex
	account     atomic.Pointer[accountView] // written under accountMu
	accountMu   sync.Mutex
	assets      atomic.Pointer[AccountAssets] // the last account read, written by the reference loop alone; nil before one succeeded. It wakes no loop

	// owned by the book loop
	legBooks [2]legBook
	streams  [2][]bookStream // by leg: its connections

	// owned by the F loop
	fxFailing bool // the last read failed or was invalid
	fxStale   bool // F was stale at the last poll

	// owned by the anchor loop
	anchor                anchorState
	issuerOff             bool      // the issuer's key is missing or refused: no NAV and no distributions until a restart
	nextDistributionCheck time.Time // the next daily comparison of the funds' ex-dates with the configuration

	// owned by the reference loop
	recordsSince time.Time // the funding records are read from here on; zero before the first read
	scheduleDiff string    // the calendar's differences from the venue's schedule as last compared; reported when it changes

	// the order table, with a lock of its own (orders.go)
	orders *orderStore

	// the last result SettingsOK logged, so that a state is logged once
	settingsLogMu sync.Mutex
	settingsLog   string
}

// bookStream is one of a leg's connections: a subscription of the leg's
// depth topic and books1. A leg has several, all feeding the one leg book:
// the venue's connections differ in delay by tens of milliseconds for as
// long as they live, and the book takes each sequence number from whichever
// delivers it first. The counters are of the frames since the last status
// line.
type bookStream struct {
	cancel    context.CancelFunc
	gen       uint64 // counts the connection's subscriptions; an input of an older one is ignored
	connected bool   // the current subscription has delivered a frame since it last connected

	frames uint64          // frames delivered
	first  uint64          // of them, those that brought a sequence number the book did not have yet
	delays []time.Duration // of each frame, local arrival minus the venue's time of the book state: the two clocks' offset is in it, the same for every connection
}

// bookInput is one thing a leg's connection reports: a frame, or the loss
// of the connection.
type bookInput struct {
	leg  Leg
	conn int // which of the leg's connections
	gen  uint64
	down bool // the connection was lost; ev is unset
	ev   BookEvent
}

// booksView is the published books of both legs.
type booksView struct {
	books   [2]*Book
	changed chan struct{} // closed when the next view is published
}

// referenceView is the published reference data.
type referenceView struct {
	fx        FX
	fxMinutes map[int64]decimal.Decimal // unix ms of a minute's start -> the last valid F read in it
	anchors   Anchors
	rules     map[Symbol]MarketRule        // nil until loaded
	settings  *AccountSettings             // nil until read
	funding   [2]map[int64]decimal.Decimal // by leg: unix ms of a settlement -> its funding rate
	changed   chan struct{}                // closed when the next view is published
}

// accountView is the published account data of the two legs.
type accountView struct {
	positions      PositionSet
	positionPushes uint64            // position sets the private stream pushed; a REST read publishes only when none came while it was out
	records        []FinancialRecord // the funding records read so far, oldest first
	changed        chan struct{}     // closed when the next view is published
}

// NewManager validates cfg and builds the manager on the venue and the three
// read-only sources. Nothing is connected until Run.
func NewManager(cfg Config, venue Venue, index IndexSource, fx FXSource, issuer IssuerSource) (*CexManager, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	calendar, err := NewCalendar(cfg.CalendarOverrides)
	if err != nil {
		return nil, fmt.Errorf("cex.calendar_overrides: %w", err)
	}
	m := &CexManager{cfg: cfg, venue: venue, index: index, fxSource: fx, issuer: issuer, calendar: calendar, timing: defaultTimings,
		symbols: [2]Symbol{cfg.Pair.Bull.Symbol, cfg.Pair.Bear.Symbol}, tickers: [2]string{cfg.Pair.Bull.Ticker, cfg.Pair.Bear.Ticker}, orders: newOrderStore()}
	m.bookInputs = make(chan bookInput, m.timing.bookInbox)
	for _, leg := range Legs {
		m.streams[leg] = make([]bookStream, cfg.BookConnections)
	}
	m.bindCorporateActions()
	m.books.Store(&booksView{books: [2]*Book{{Symbol: m.symbols[Bull]}, {Symbol: m.symbols[Bear]}}, changed: make(chan struct{})})
	m.reference.Store(&referenceView{changed: make(chan struct{})})
	m.account.Store(&accountView{changed: make(chan struct{})})
	return m, nil
}

// Run connects the books and the private stream, reads the reference data
// and follows the orders until ctx ends.
func (m *CexManager) Run(ctx context.Context) {
	log := logger.GlobalLogger
	session := m.Session(m.timing.clock())
	log.Info("cex: running", "venue", m.venue.Name(), "bull", m.symbols[Bull], "bear", m.symbols[Bear],
		"bull_topic", m.cfg.Pair.Bull.BookTopic, "bear_topic", m.cfg.Pair.Bear.BookTopic, "book_connections", m.cfg.BookConnections, "order_transport", string(m.cfg.OrderTransport),
		"session", session.ID, "session_end", showTime(session.End))
	var loops sync.WaitGroup
	loops.Go(func() { m.runFX(ctx) })
	loops.Go(func() { m.runAnchors(ctx) })
	loops.Go(func() { m.runReference(ctx) })
	loops.Go(func() { m.runOrders(ctx) })
	m.runBooks(ctx)
	loops.Wait()
	log.Info("cex: stopped")
}

// --- the book loop ---

// runBooks opens every leg's connections and applies their frames in
// arrival order until ctx ends.
func (m *CexManager) runBooks(ctx context.Context) {
	for _, leg := range Legs {
		for conn := range m.streams[leg] {
			m.openBookStream(ctx, leg, conn)
		}
	}
	status := time.NewTicker(m.timing.statusLog)
	defer status.Stop()
	for {
		select {
		case <-ctx.Done():
			return // the streams end with ctx
		case in := <-m.bookInputs:
			m.handleBookInput(ctx, in)
		case <-status.C:
			m.logStatus(m.timing.clock())
		}
	}
}

// openBookStream subscribes the leg's depth topic and books1 on one more
// connection of the venue, as the leg's conn-th. The venue keeps it up; its
// inputs carry the subscription's number so that those of a replaced
// subscription are told apart.
func (m *CexManager) openBookStream(ctx context.Context, leg Leg, conn int) {
	st := &m.streams[leg][conn]
	if st.cancel != nil {
		st.cancel()
	}
	st.gen++
	st.connected = false
	gen, symbol := st.gen, m.symbols[leg]
	var streamCtx context.Context
	streamCtx, st.cancel = context.WithCancel(ctx)
	subs := []BookSubscription{{Symbol: symbol, Topic: m.legConfig(leg).BookTopic}, {Symbol: symbol, Topic: TopicBooks1}}
	err := m.venue.SubscribeBooks(streamCtx, subs,
		func(ev BookEvent) { m.enqueueBookInput(bookInput{leg: leg, conn: conn, gen: gen, ev: ev}) },
		func(error) { m.enqueueBookInput(bookInput{leg: leg, conn: conn, gen: gen, down: true}) })
	if err != nil {
		logger.GlobalLogger.Error("cex: book subscription refused", "symbol", symbol, "connection", conn, "err", err)
	}
}

// enqueueBookInput hands an input to the book loop without blocking the
// venue's read goroutine. A full inbox drops it: a dropped snapshot is
// replaced by the next one, a dropped update breaks the chain and the
// connection is subscribed again, a dropped loss leaves the depth to age
// out.
func (m *CexManager) enqueueBookInput(in bookInput) {
	select {
	case m.bookInputs <- in:
	default:
		m.droppedFrames.Add(1)
	}
}

// handleBookInput applies one input to its leg's book and publishes the
// result. Every connection of a leg delivers the same frames; the leg book
// orders them by sequence number, so the copy that arrives first is applied
// and the later ones change nothing: they publish nothing and wake no
// reader. The leg's depth is dropped only when the last of its connections
// is lost.
func (m *CexManager) handleBookInput(ctx context.Context, in bookInput) {
	log := logger.GlobalLogger
	st, book, symbol := &m.streams[in.leg][in.conn], &m.legBooks[in.leg], m.symbols[in.leg]
	if in.gen != st.gen {
		return // from a subscription that was replaced
	}
	wasUp := m.connectedStreams(in.leg) > 0
	switch {
	case in.down:
		// The venue reports the loss in its own log, reconnects and
		// subscribes again by itself.
		st.connected = false
		if up := m.connectedStreams(in.leg); up > 0 {
			log.Info("cex: book connection down", "symbol", symbol, "connection", in.conn, "connections_up", up)
			return // the others keep the book current
		}
		// The last connection is gone: the depth with it.
		book.dropDepth()
		log.Info("cex: book down", "symbol", symbol, "topic", m.legConfig(in.leg).BookTopic)
	case in.ev.Topic == TopicBooks1:
		held := book.best
		st.count(in.ev, held == nil || in.ev.Seq > held.seq)
		book.applyBest(in.ev)
		log.Debug("cex: books1 frame", "symbol", symbol, "connection", in.conn, "seq", in.ev.Seq)
		if book.best == held && wasUp {
			return // not newer than the frame held
		}
	default:
		held, heldSeq := book.depth, uint64(0)
		if held != nil {
			heldSeq = held.seq
		}
		st.count(in.ev, held == nil || in.ev.Seq > heldSeq)
		log.Debug("cex: depth frame", "symbol", symbol, "connection", in.conn, "topic", in.ev.Topic, "action", in.ev.Action, "seq", in.ev.Seq, "prev_seq", in.ev.PrevSeq)
		switch {
		case book.applyDepth(in.ev):
			// The update does not follow the depth held: the depth is
			// dropped and this connection subscribed again for a new
			// snapshot.
			log.Warn("cex: depth chain broken; resubscribing", "symbol", symbol, "connection", in.conn, "topic", in.ev.Topic, "seq", in.ev.Seq, "prev_seq", in.ev.PrevSeq)
			m.openBookStream(ctx, in.leg, in.conn)
		case held == nil && book.depth != nil:
			log.Info("cex: book live", "symbol", symbol, "connection", in.conn, "topic", in.ev.Topic, "seq", in.ev.Seq)
		case book.depth == held && (held == nil || held.seq == heldSeq) && wasUp:
			return // not newer than the depth held, or an update while the depth waits for a snapshot
		}
	}
	m.publishBook(in.leg)
}

// count notes one frame of the connection: that it is up, whether the frame
// was the first to bring its sequence number, and how old the book state was
// on arrival.
func (st *bookStream) count(ev BookEvent, first bool) {
	st.connected = true
	st.frames++
	if first {
		st.first++
	}
	if !ev.ExchangeTime.IsZero() {
		st.delays = append(st.delays, ev.ReceivedAt.Sub(ev.ExchangeTime))
	}
}

// connectedStreams counts the leg's connections that are up.
func (m *CexManager) connectedStreams(leg Leg) (up int) {
	for i := range m.streams[leg] {
		if m.streams[leg][i].connected {
			up++
		}
	}
	return up
}

// publishBook replaces the published view with the leg's book as it is now.
// Only the book loop publishes books.
func (m *CexManager) publishBook(leg Leg) {
	old := m.books.Load()
	next := &booksView{books: old.books, changed: make(chan struct{})}
	next.books[leg] = m.legBooks[leg].publish(m.symbols[leg], m.connectedStreams(leg) > 0)
	m.books.Store(next)
	close(old.changed)
}

// logStatus writes the periodic status line. For each leg it shows, per
// connection and over the time since the last line: the frames delivered,
// the share of the leg's new sequence numbers that this connection brought
// first, and the median delay of its frames in milliseconds (local arrival
// minus the venue's time of the book state; -1 without a frame). A
// connection that is slow shows a small share and a large delay. A venue
// that reports on its own connections adds its values.
func (m *CexManager) logStatus(now time.Time) {
	ref := m.reference.Load()
	age := func(t time.Time) string {
		if t.IsZero() {
			return "none"
		}
		return now.Sub(t).Round(time.Millisecond).String()
	}
	books := m.Books()
	bull, bear := books.ByLeg[Bull], books.ByLeg[Bear]
	attrs := []any{
		"bull_depth_age", age(bull.DepthReceivedAt), "bull_usable", books.DepthUsable(Bull, now), "bull_conflict", bull.Conflict,
		"bear_depth_age", age(bear.DepthReceivedAt), "bear_usable", books.DepthUsable(Bear, now), "bear_conflict", bear.Conflict}
	for _, leg := range Legs {
		frames, share, delay := m.streamStats(leg)
		attrs = append(attrs, leg.String()+"_conns_up", m.connectedStreams(leg), leg.String()+"_conn_frames", frames, leg.String()+"_conn_first_pct", share, leg.String()+"_conn_delay_ms", delay)
	}
	attrs = append(attrs, "dropped_frames", m.droppedFrames.Load(), "fx", ref.fx.Price, "fx_age", age(ref.fx.ReceivedAt),
		"session", ref.anchors.SessionID, "anchor_kind", string(ref.anchors.Kind), "anchor_bull", ref.anchors.Bull, "anchor_bear", ref.anchors.Bear, "entry_ban", ref.anchors.EntryBan,
		"trading_ready", m.TradingReady(), "unresolved_orders", len(m.UnresolvedOrders()))
	if venue, reports := m.venue.(VenueStatus); reports {
		attrs = append(attrs, venue.StatusAttrs()...)
	}
	logger.GlobalLogger.Info("cex: status", attrs...)
}

// streamStats returns, per connection of the leg, the frames it delivered
// since the last call, its share in percent of the frames that came first,
// and its median frame delay in milliseconds, and starts the next period.
func (m *CexManager) streamStats(leg Leg) (frames []uint64, firstPct []int, delayMs []float64) {
	var firsts uint64
	for i := range m.streams[leg] {
		firsts += m.streams[leg][i].first
	}
	for i := range m.streams[leg] {
		st := &m.streams[leg][i]
		pct, delay := 0, -1.0
		if firsts > 0 {
			pct = int(math.Round(100 * float64(st.first) / float64(firsts)))
		}
		if n := len(st.delays); n > 0 {
			slices.Sort(st.delays)
			delay = math.Round(st.delays[n/2].Seconds()*1e4) / 10
		}
		frames, firstPct, delayMs = append(frames, st.frames), append(firstPct, pct), append(delayMs, delay)
		st.frames, st.first, st.delays = 0, 0, st.delays[:0]
	}
	return frames, firstPct, delayMs
}

// --- market data, for readers ---

// Books returns both legs' latest published books, taken in one load of the
// published view, with depth_max_age bound for its validity questions. A
// reader calls it once per decision and uses nothing but the value returned.
func (m *CexManager) Books() Books {
	return Books{ByLeg: m.books.Load().books, MaxAge: m.cfg.DepthMaxAge}
}

// BookChanged returns a channel closed at the next book publication. A
// reader takes the channel first, then reads the books, then waits on the
// channel: no publication is missed.
func (m *CexManager) BookChanged() <-chan struct{} { return m.books.Load().changed }

// --- reference data, for readers ---

// Session returns the Session that contains now.
func (m *CexManager) Session(now time.Time) Session { return m.calendar.SessionAt(now) }

// ReferenceChanged returns a channel closed at the next publication of F,
// the anchors, the rules, the settings or the funding rates.
func (m *CexManager) ReferenceChanged() <-chan struct{} { return m.reference.Load().changed }

// AccountChanged returns a channel closed at the next publication of the
// positions or the funding records.
func (m *CexManager) AccountChanged() <-chan struct{} { return m.account.Load().changed }

// editReference publishes a new reference view: a copy of the current one
// changed by edit. The loops that publish take turns under referenceMu.
// edit must replace the maps it changes, never write into them.
func (m *CexManager) editReference(edit func(v *referenceView)) {
	m.referenceMu.Lock()
	defer m.referenceMu.Unlock()
	old := m.reference.Load()
	next := *old
	next.changed = make(chan struct{})
	edit(&next)
	m.reference.Store(&next)
	close(old.changed)
}

// editAccount publishes a new account view the same way, unless edit
// reports that it changed nothing.
func (m *CexManager) editAccount(edit func(v *accountView) bool) {
	m.accountMu.Lock()
	defer m.accountMu.Unlock()
	old := m.account.Load()
	next := *old
	next.changed = make(chan struct{})
	if !edit(&next) {
		return
	}
	m.account.Store(&next)
	close(old.changed)
}

// legConfig returns the leg's configuration.
func (m *CexManager) legConfig(leg Leg) LegConfig {
	if leg == Bull {
		return m.cfg.Pair.Bull
	}
	return m.cfg.Pair.Bear
}

// legOf returns the leg that trades symbol.
func (m *CexManager) legOf(symbol Symbol) (Leg, bool) {
	for _, leg := range Legs {
		if m.symbols[leg] == symbol {
			return leg, true
		}
	}
	return 0, false
}
