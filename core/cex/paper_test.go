package cex

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/store"
)

// manualTimer stands in for the paper venue's clock and timer: time moves
// only when the test advances it, and what is due then runs on the test's
// goroutine.
type manualTimer struct {
	mu      sync.Mutex
	now     time.Time
	pending []timedCall
}

type timedCall struct {
	at time.Time
	f  func()
}

func (m *manualTimer) clock() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.now
}

func (m *manualTimer) after(d time.Duration, f func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pending = append(m.pending, timedCall{at: m.now.Add(d), f: f})
}

// advance moves the time by d and runs every call due by then, earliest
// first, each at its own time.
func (m *manualTimer) advance(d time.Duration) {
	m.mu.Lock()
	end := m.now.Add(d)
	sort.SliceStable(m.pending, func(i, j int) bool { return m.pending[i].at.Before(m.pending[j].at) })
	var due []timedCall
	for len(m.pending) > 0 && !m.pending[0].at.After(end) {
		due, m.pending = append(due, m.pending[0]), m.pending[1:]
	}
	m.mu.Unlock()
	for _, call := range due {
		m.mu.Lock()
		m.now = call.at
		m.mu.Unlock()
		call.f()
	}
	m.mu.Lock()
	m.now = end
	m.mu.Unlock()
}

// paperRig is a paper venue on a fake real venue, with its timer, the sink
// its matches go to and the private events it emitted.
type paperRig struct {
	p      *PaperVenue
	real   *fakeVenue
	timer  *manualTimer
	sink   *store.Fake
	mu     sync.Mutex
	events []PrivateEvent
	bull   *fakeSubscription
	bear   *fakeSubscription
}

// newPaperRig builds a paper venue with latencies of 15 ms (bull) and 40 ms
// (bear), a taker fee of 0.65 bps and a depth_max_age of 10 s, subscribed to both legs' books and to
// its own private stream.
func newPaperRig(t *testing.T) *paperRig {
	t.Helper()
	r := &paperRig{real: newFakeVenue(), timer: &manualTimer{now: t0}, sink: &store.Fake{}}
	r.p = NewPaperVenue(r.real, PaperConfig{Pair: testConfig().Pair, Latency: [2]time.Duration{Bull: 15 * time.Millisecond, Bear: 40 * time.Millisecond}, TakerFeeBps: 0.65, Leverage: 10, DepthMaxAge: 10 * time.Second})
	r.p.timing = paperTimings{clock: r.timer.clock, after: r.timer.after}
	r.p.UseStore(r.sink)
	ctx := context.Background()
	for _, symbol := range []Symbol{"SOXLUSDT", "SOXSUSDT"} {
		if err := r.p.SubscribeBooks(ctx, []BookSubscription{{Symbol: symbol, Topic: TopicBooks5}, {Symbol: symbol, Topic: TopicBooks1}}, func(BookEvent) {}, func(error) {}); err != nil {
			t.Fatal(err)
		}
	}
	r.bull, r.bear = r.real.subscriptionsOf("SOXLUSDT")[0], r.real.subscriptionsOf("SOXSUSDT")[0]
	if err := r.p.SubscribePrivate(ctx, []Symbol{"SOXLUSDT", "SOXSUSDT"}, func(ev PrivateEvent) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.events = append(r.events, ev)
	}, func(error) {}); err != nil {
		t.Fatal(err)
	}
	return r
}

// bullBook sends the bull leg a depth frame with the given sequence number:
// bids 40.45 x 10, 40.44 x 20 and the given asks.
func (r *paperRig) bullBook(seq uint64, asks ...BookLevel) {
	r.bull.onEvent(BookEvent{Symbol: "SOXLUSDT", Topic: TopicBooks5, Action: ActionSnapshot, Bids: []BookLevel{lv("40.45", "10"), lv("40.44", "20")}, Asks: asks, Seq: seq, ReceivedAt: r.timer.clock()})
}

// take returns the private events emitted since the last call.
func (r *paperRig) take() (fills []Fill, orders []OrderRecord, positions []PositionSet) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ev := range r.events {
		switch {
		case ev.Fill != nil:
			fills = append(fills, *ev.Fill)
		case ev.Order != nil:
			orders = append(orders, *ev.Order)
		case ev.Positions != nil:
			positions = append(positions, *ev.Positions)
		}
	}
	r.events = nil
	return fills, orders, positions
}

// place sends one paper order and fails the test unless it is accepted.
func (r *paperRig) place(t *testing.T, req OrderRequest) {
	t.Helper()
	ack, err := r.p.PlaceOrder(context.Background(), req)
	if err != nil || ack.OrderID == "" || !ack.ReceivedAt.Equal(r.timer.clock()) {
		t.Fatalf("PlaceOrder = %+v, %v; a paper order is accepted at once", ack, err)
	}
}

// A paper order is accepted at once and matched after its leg's latency:
// level by level inside its limit. Taken in full it is filled; the fee is
// the value times the taker fee; the position follows. The wrapped venue
// never sees an order (V18).
func TestPaperOrderFilledAcrossLevels(t *testing.T) {
	r := newPaperRig(t)
	r.bullBook(100, lv("40.46", "5"), lv("40.47", "6"), lv("40.48", "9"))
	req := orderReq(1, Bull, PurposeEntry, SideBuy, "40.47", "8")
	r.place(t, req)

	// Before the latency has passed nothing has happened; a query shows the
	// order accepted.
	r.timer.advance(14 * time.Millisecond)
	if fills, orders, _ := r.take(); len(fills)+len(orders) != 0 {
		t.Fatalf("matched before its latency: %d fills, %d reports", len(fills), len(orders))
	}
	if rec, err := r.p.OrderByClientID(context.Background(), "SOXLUSDT", req.ClientOrderID); err != nil || rec.Status != StatusAcked {
		t.Fatalf("the order before its match: %+v, %v", rec, err)
	}
	r.timer.advance(time.Millisecond)
	fills, orders, positions := r.take()
	if len(fills) != 2 || len(orders) != 1 || len(positions) != 1 {
		t.Fatalf("%d fills, %d reports, %d position sets; want 2, 1, 1", len(fills), len(orders), len(positions))
	}
	// 5 at 40.46 and 3 at 40.47: 202.30 + 121.41 = 323.71; fee 323.71 x 0.000065.
	matchedAt := t0.Add(15 * time.Millisecond)
	if f := fills[0]; !f.Price.Equal(dec("40.46")) || !f.Qty.Equal(dec("5")) || !f.Value.Equal(dec("202.3")) || !f.Fee.Equal(dec("0.0131495")) || f.ClientOrderID != req.ClientOrderID || f.Side != SideBuy || !f.ExecTime.Equal(matchedAt) {
		t.Errorf("the first fill = %+v", f)
	}
	if f := fills[1]; !f.Price.Equal(dec("40.47")) || !f.Qty.Equal(dec("3")) || !f.Value.Equal(dec("121.41")) || f.ExecID == fills[0].ExecID {
		t.Errorf("the second fill = %+v", f)
	}
	o := orders[0]
	if o.Status != StatusFilled || !o.CumExecQty.Equal(dec("8")) || !o.CumExecValue.Equal(dec("323.71")) || !o.AvgPrice.Equal(dec("40.46375")) || !o.Fee.Equal(dec("0.02104115")) || !o.FeeKnown ||
		!o.ExchangeUpdatedAt.Equal(matchedAt) || o.Request.ClientOrderID != req.ClientOrderID || o.OrderID == "" {
		t.Errorf("the report = %+v", o)
	}
	if p := positions[0].BySymbol["SOXLUSDT"]; !p.Qty.Equal(dec("8")) || !p.UpdatedAt.Equal(matchedAt) || len(positions[0].BySymbol) != 1 || !positions[0].Loaded {
		t.Errorf("positions = %+v", positions[0])
	}
	if got, _ := r.p.Positions(context.Background(), nil); !got.BySymbol["SOXLUSDT"].Qty.Equal(dec("8")) {
		t.Errorf("Positions = %+v", got)
	}
	if n := r.real.callCount("PlaceOrder"); n != 0 {
		t.Errorf("the wrapped venue received %d orders", n)
	}
}

// An order larger than what stands inside its limit is filled for that part
// and its remainder cancelled; one that meets no level inside its limit is
// cancelled with nothing, and has no fee to know.
func TestPaperOrderPartialAndZeroFill(t *testing.T) {
	r := newPaperRig(t)
	r.bullBook(100, lv("40.46", "5"), lv("40.47", "6"), lv("40.48", "9"))
	partial, zero := orderReq(1, Bull, PurposeEntry, SideBuy, "40.47", "20"), orderReq(2, Bull, PurposeEntry, SideBuy, "40.45", "5")
	r.place(t, partial)
	r.place(t, zero)
	r.timer.advance(15 * time.Millisecond)
	fills, orders, _ := r.take()
	if len(fills) != 2 || len(orders) != 2 {
		t.Fatalf("%d fills and %d reports, want 2 and 2", len(fills), len(orders))
	}
	// 5 at 40.46 and 6 at 40.47: 202.30 + 242.82 = 445.12.
	if o := orders[0]; o.Status != StatusCancelled || !o.CumExecQty.Equal(dec("11")) || !o.CumExecValue.Equal(dec("445.12")) || !o.Fee.Equal(dec("0.0289328")) || !o.FeeKnown || o.CancelReason == "" {
		t.Errorf("the partly filled order = %+v", o)
	}
	if o := orders[1]; o.Status != StatusCancelled || !o.CumExecQty.IsZero() || !o.CumExecValue.IsZero() || !o.Fee.IsZero() || o.FeeKnown || !o.AvgPrice.IsZero() {
		t.Errorf("the order that met no level = %+v", o)
	}
	// Without a depth there is nothing to match on.
	r.bull.onDown(errors.New("connection lost"))
	r.place(t, orderReq(3, Bull, PurposeEntry, SideBuy, "40.47", "1"))
	r.timer.advance(15 * time.Millisecond)
	if _, orders, _ := r.take(); len(orders) != 1 || orders[0].Status != StatusCancelled || !orders[0].CumExecQty.IsZero() {
		t.Errorf("an order matched without a depth: %+v", orders)
	}
}

// A leg subscribed on several connections keeps its paper depth while one
// of them is up: only the loss of the last leaves nothing to match on.
func TestPaperDepthSurvivesTheLossOfOneOfItsConnections(t *testing.T) {
	r := newPaperRig(t)
	// A second connection for the bull leg.
	if err := r.p.SubscribeBooks(context.Background(), []BookSubscription{{Symbol: "SOXLUSDT", Topic: TopicBooks5}, {Symbol: "SOXLUSDT", Topic: TopicBooks1}}, func(BookEvent) {}, func(error) {}); err != nil {
		t.Fatal(err)
	}
	second := r.real.subscriptionsOf("SOXLUSDT")[1]
	r.bullBook(100, lv("40.46", "5"))
	second.onEvent(BookEvent{Symbol: "SOXLUSDT", Topic: TopicBooks5, Action: ActionSnapshot, Bids: []BookLevel{lv("40.45", "10")}, Asks: []BookLevel{lv("40.46", "5")}, Seq: 100, ReceivedAt: r.timer.clock()})

	r.bull.onDown(errors.New("connection lost"))
	r.place(t, orderReq(1, Bull, PurposeEntry, SideBuy, "40.47", "2"))
	r.timer.advance(15 * time.Millisecond)
	if _, orders, _ := r.take(); len(orders) != 1 || orders[0].Status != StatusFilled {
		t.Fatalf("with one of two connections lost: %+v, want the order filled on the depth the other keeps", orders)
	}
	second.onDown(errors.New("connection lost"))
	r.place(t, orderReq(2, Bull, PurposeEntry, SideBuy, "40.47", "2"))
	r.timer.advance(15 * time.Millisecond)
	if _, orders, _ := r.take(); len(orders) != 1 || orders[0].Status != StatusCancelled || !orders[0].CumExecQty.IsZero() {
		t.Errorf("with both connections lost: %+v, want nothing to match on", orders)
	}
}

// The order meets the book of the moment it is matched, not of the moment
// it was sent; each leg has its own latency. The match is recorded with
// both books.
func TestPaperOrderMeetsTheBookAfterItsLatency(t *testing.T) {
	r := newPaperRig(t)
	r.bullBook(100, lv("40.46", "5"), lv("40.47", "6"))
	r.bear.onEvent(BookEvent{Symbol: "SOXSUSDT", Topic: TopicBooks5, Action: ActionSnapshot, Bids: []BookLevel{lv("49.707", "14")}, Asks: []BookLevel{lv("49.718", "13")}, Seq: 200, ReceivedAt: t0})
	bull, bear := orderReq(1, Bull, PurposeEntry, SideBuy, "40.47", "8"), orderReq(2, Bear, PurposeEntry, SideBuy, "49.72", "8")
	r.place(t, bull)
	r.place(t, bear)

	// Ten milliseconds on the bull asks move away from the limit.
	r.timer.advance(10 * time.Millisecond)
	r.bullBook(101, lv("40.50", "5"), lv("40.51", "6"))
	r.timer.advance(5 * time.Millisecond) // 15 ms: the bull leg's latency
	_, orders, _ := r.take()
	if len(orders) != 1 || orders[0].Request.ClientOrderID != bull.ClientOrderID || orders[0].Status != StatusCancelled || !orders[0].CumExecQty.IsZero() {
		t.Fatalf("at 15 ms: %+v; want only the bull order, matched on the book that had moved away", orders)
	}
	r.timer.advance(24 * time.Millisecond) // 39 ms
	if _, orders, _ := r.take(); len(orders) != 0 {
		t.Fatalf("the bear order matched at 39 ms, before its 40 ms")
	}
	r.timer.advance(time.Millisecond)
	_, orders, _ = r.take()
	if len(orders) != 1 || orders[0].Status != StatusFilled || !orders[0].CumExecValue.Equal(dec("397.744")) || !orders[0].ExchangeUpdatedAt.Equal(t0.Add(40*time.Millisecond)) {
		t.Fatalf("at 40 ms: %+v; want the bear order filled 8 x 49.718", orders)
	}

	// One decision row per match, with the book at the send and at the
	// match; nothing in the event table, which holds alarms.
	rows := r.sink.Rows("decision")
	if len(rows) != 2 || len(r.sink.Rows("event")) != 0 {
		t.Fatalf("%d decision rows and %d event rows, want one decision row per match and no event", len(rows), len(r.sink.Rows("event")))
	}
	ev := rows[0].(store.DecisionRow)
	var detail struct {
		ClientOrderID string `json:"client_order_id"`
		Status        string `json:"status"`
		SentMs        int64  `json:"sent_ms"`
		MatchedMs     int64  `json:"matched_ms"`
		AtSend        struct {
			DepthSeq uint64      `json:"depth_seq"`
			BestAsk  string      `json:"best_ask"`
			Levels   [][2]string `json:"levels"`
		} `json:"book_at_send"`
		AtMatch struct {
			DepthSeq uint64      `json:"depth_seq"`
			BestAsk  string      `json:"best_ask"`
			Levels   [][2]string `json:"levels"`
		} `json:"book_at_match"`
	}
	if err := json.Unmarshal(ev.Detail, &detail); err != nil {
		t.Fatal(err)
	}
	if ev.Kind != "paper_match" || ev.CycleID != testCycleID || ev.LotID != testCycleID+"-1" || ev.TsMs != t0.Add(15*time.Millisecond).UnixMilli() || detail.ClientOrderID != string(bull.ClientOrderID) || detail.Status != "cancelled" ||
		detail.SentMs != t0.UnixMilli() || detail.MatchedMs != t0.Add(15*time.Millisecond).UnixMilli() {
		t.Errorf("row = %+v detail %s", ev, ev.Detail)
	}
	if detail.AtSend.DepthSeq != 100 || detail.AtSend.BestAsk != "40.46" || !slices.Equal(detail.AtSend.Levels, [][2]string{{"40.46", "5"}, {"40.47", "6"}}) ||
		detail.AtMatch.DepthSeq != 101 || detail.AtMatch.BestAsk != "40.5" || detail.AtMatch.Levels[0] != [2]string{"40.5", "5"} {
		t.Errorf("the recorded books: at send %+v, at match %+v", detail.AtSend, detail.AtMatch)
	}
}

// A reduce-only order takes no more than the position it reduces, and a
// sell is matched on the bids at or above its limit.
func TestPaperReduceOnlyIsCappedByThePosition(t *testing.T) {
	r := newPaperRig(t)
	r.p.SeedPositions([2]decimal.Decimal{Bull: dec("3"), Bear: dec("-4")})
	r.bullBook(100, lv("40.46", "5"))
	r.bear.onEvent(BookEvent{Symbol: "SOXSUSDT", Topic: TopicBooks5, Action: ActionSnapshot, Bids: []BookLevel{lv("49.707", "14")}, Asks: []BookLevel{lv("49.718", "13")}, Seq: 200, ReceivedAt: t0})

	// Long 3 bull: a reduce-only sell of 5 at 40.44 sells 3, at the bid.
	sell := orderReq(1, Bull, PurposeExit, SideSell, "40.44", "5")
	// A reduce-only buy cannot add to a long position.
	buy := orderReq(2, Bull, PurposeExit, SideBuy, "40.47", "5")
	buy.ReduceOnly = true
	// Short 4 bear: a reduce-only buy of 6 buys 4 back.
	cover := orderReq(3, Bear, PurposeExit, SideBuy, "49.72", "6")
	r.place(t, sell)
	r.place(t, buy)
	r.place(t, cover)
	r.timer.advance(40 * time.Millisecond)
	_, orders, positions := r.take()
	if len(orders) != 3 {
		t.Fatalf("%d reports, want 3", len(orders))
	}
	if o := orders[0]; o.Status != StatusCancelled || !o.CumExecQty.Equal(dec("3")) || !o.CumExecValue.Equal(dec("121.35")) { // 3 x 40.45
		t.Errorf("the reduce-only sell = %+v, want 3 sold at the bid 40.45", o)
	}
	if o := orders[1]; o.Status != StatusCancelled || !o.CumExecQty.IsZero() {
		t.Errorf("the reduce-only buy against a flat-by-then long = %+v, want nothing filled", o)
	}
	if o := orders[2]; o.Status != StatusCancelled || !o.CumExecQty.Equal(dec("4")) || !o.CumExecValue.Equal(dec("198.872")) { // 4 x 49.718
		t.Errorf("the reduce-only cover = %+v, want 4 bought back", o)
	}
	if last := positions[len(positions)-1]; len(last.BySymbol) != 0 {
		t.Errorf("positions at the end = %+v, want flat", last)
	}
	// Without reduce-only the same sell opens a short.
	open := orderReq(4, Bull, PurposeEntry, SideSell, "40.44", "5")
	r.place(t, open)
	r.timer.advance(15 * time.Millisecond)
	if _, orders, positions := r.take(); orders[0].Status != StatusFilled || !orders[0].CumExecValue.Equal(dec("202.25")) || !positions[0].BySymbol["SOXLUSDT"].Qty.Equal(dec("-5")) {
		t.Errorf("an opening sell: %+v, positions %+v", orders[0], positions[0])
	}
}

// At a funding settlement the position and the mid price of each leg are
// noted at its first book frame; once the wrapped venue publishes the
// settled rate, a paper funding record is booked: -position x mid x rate.
// A flat leg gets none.
func TestPaperFundingRecord(t *testing.T) {
	r := newPaperRig(t)
	settlement := time.Date(2026, 11, 24, 8, 0, 0, 0, time.UTC)
	r.timer.now = settlement.Add(-10 * time.Second)
	r.p.SeedPositions([2]decimal.Decimal{Bull: dec("10"), Bear: decimal.Zero})
	r.bullBook(100, lv("40.46", "5"))
	r.bear.onEvent(BookEvent{Symbol: "SOXSUSDT", Topic: TopicBooks5, Action: ActionSnapshot, Bids: []BookLevel{lv("49.707", "14")}, Asks: []BookLevel{lv("49.718", "13")}, Seq: 200, ReceivedAt: r.timer.clock()})
	ctx := context.Background()
	if recs, err := r.p.FinancialRecords(ctx, settlement.Add(-time.Hour)); err != nil || len(recs) != 0 {
		t.Fatalf("before the settlement: %+v, %v", recs, err)
	}

	// The first frames after the settlement: bid 40.45, ask 40.46 on the
	// bull leg, mid 40.455.
	r.timer.advance(10*time.Second + 200*time.Millisecond)
	r.bullBook(101, lv("40.46", "5"))
	r.bear.onEvent(BookEvent{Symbol: "SOXSUSDT", Topic: TopicBooks5, Action: ActionSnapshot, Bids: []BookLevel{lv("49.707", "14")}, Asks: []BookLevel{lv("49.718", "13")}, Seq: 201, ReceivedAt: r.timer.clock()})
	// A later frame at another price does not move what was noted.
	r.timer.advance(time.Second)
	r.bull.onEvent(BookEvent{Symbol: "SOXLUSDT", Topic: TopicBooks5, Action: ActionSnapshot, Bids: []BookLevel{lv("41", "10")}, Asks: []BookLevel{lv("41.02", "5")}, Seq: 102, ReceivedAt: r.timer.clock()})

	// The venue has not published the settled rate yet: no record.
	r.real.funding["SOXLUSDT"] = []FundingSettlement{{Symbol: "SOXLUSDT", Rate: dec("0.0003"), At: settlement.Add(-8 * time.Hour)}}
	if recs, err := r.p.FinancialRecords(ctx, settlement.Add(-time.Hour)); err != nil || len(recs) != 0 {
		t.Fatalf("before the rate is published: %+v, %v", recs, err)
	}
	r.real.funding["SOXLUSDT"] = append([]FundingSettlement{{Symbol: "SOXLUSDT", Rate: dec("0.0001"), At: settlement}}, r.real.funding["SOXLUSDT"]...)
	recs, err := r.p.FinancialRecords(ctx, settlement.Add(-time.Hour))
	if err != nil || len(recs) != 1 {
		t.Fatalf("records = %+v, %v; want one, for the leg that held a position", recs, err)
	}
	// A long of 10 at a mid of 40.455 pays 10 x 40.455 x 0.0001.
	if rec := recs[0]; rec.Symbol != "SOXLUSDT" || !rec.Amount.Equal(dec("-0.040455")) || !rec.At.Equal(settlement) || rec.ID != "paper-funding-SOXLUSDT-1795507200000" {
		t.Errorf("record = %+v", rec)
	}
	// Asked again it is the same record, once - also by a reader whose
	// since has passed the settlement: the record was booked after the
	// time it carries.
	for _, since := range []time.Time{settlement.Add(-time.Hour), settlement.Add(time.Minute)} {
		if again, _ := r.p.FinancialRecords(ctx, since); len(again) != 1 || again[0].ID != recs[0].ID {
			t.Errorf("asked again since %s: %+v, want the one record", since, again)
		}
	}
	if n := r.real.callCount("FinancialRecords"); n != 0 {
		t.Errorf("the wrapped venue was asked for the account's records %d times", n)
	}
}

// The rest of the account side is local too: the expected settings, and
// queries answered from this run's paper orders and fills.
func TestPaperAccountSideIsLocal(t *testing.T) {
	r := newPaperRig(t)
	ctx := context.Background()
	symbols := []Symbol{"SOXLUSDT", "SOXSUSDT"}
	settings, err := r.p.AccountSettings(ctx, symbols)
	if err != nil || !settings.OneWay || settings.BySymbol["SOXLUSDT"] != (SymbolSetting{Crossed: true, Leverage: 10}) || settings.BySymbol["SOXSUSDT"] != (SymbolSetting{Crossed: true, Leverage: 10}) {
		t.Errorf("settings = %+v, %v", settings, err)
	}
	if _, err := r.p.OrderByClientID(ctx, "SOXLUSDT", "etfp1-0001BE"); !errors.Is(err, ErrUnknownOrder) {
		t.Errorf("a query for an order never placed here: %v, want ErrUnknownOrder", err)
	}
	r.bullBook(100, lv("40.46", "5"))
	req := orderReq(1, Bull, PurposeEntry, SideBuy, "40.47", "2")
	r.place(t, req)
	r.timer.advance(15 * time.Millisecond)
	if rec, err := r.p.OrderByClientID(ctx, "SOXLUSDT", req.ClientOrderID); err != nil || rec.Status != StatusFilled || !rec.CumExecQty.Equal(dec("2")) {
		t.Errorf("the order by its id: %+v, %v", rec, err)
	}
	if recent, _ := r.p.RecentOrders(ctx, "SOXLUSDT", t0); len(recent) != 1 || recent[0].Request.ClientOrderID != req.ClientOrderID {
		t.Errorf("recent orders = %+v", recent)
	}
	if other, _ := r.p.RecentOrders(ctx, "SOXSUSDT", t0); len(other) != 0 {
		t.Errorf("recent orders of the other contract = %+v", other)
	}
	if fills, _ := r.p.Fills(ctx, t0); len(fills) != 1 || fills[0].ClientOrderID != req.ClientOrderID {
		t.Errorf("fills = %+v", fills)
	}
	if fills, _ := r.p.Fills(ctx, t0.Add(time.Second)); len(fills) != 0 {
		t.Errorf("fills since after the match = %+v", fills)
	}
	for _, method := range []string{"PlaceOrder", "AccountSettings", "Positions", "OrderByClientID", "RecentOrders", "Fills", "FinancialRecords", "SubscribePrivate"} {
		if n := r.real.callCount(method); n != 0 {
			t.Errorf("the wrapped venue's %s was called %d times", method, n)
		}
	}
	if r.p.Name() != "fake-paper" {
		t.Errorf("Name = %q", r.p.Name())
	}
}

// The venue publishes a settlement's rate late - after the manager's
// "since my last read" has moved past the settlement. The paper record is
// booked then, with the settlement's time, and must still reach the
// manager: the ledger waits for it. Reads at 07:59:20, 08:00:20 and
// 08:01:20; the rate is there from 08:01:40; the read at 08:02:20 has it.
func TestPaperFundingReachesTheManagerWhenTheRateIsLate(t *testing.T) {
	r := newPaperRig(t)
	settlement := time.Date(2026, 11, 24, 8, 0, 0, 0, time.UTC)
	r.timer.now = settlement.Add(-time.Minute)
	m, err := NewManager(testConfig(), r.p, &fakeIndex{}, &fakeFX{}, &fakeIssuer{})
	if err != nil {
		t.Fatal(err)
	}
	r.p.SeedPositions([2]decimal.Decimal{Bull: dec("10"), Bear: decimal.Zero})
	r.bullBook(100, lv("40.46", "5"))
	ctx := context.Background()
	read := func(at time.Time) {
		t.Helper()
		r.timer.advance(at.Sub(r.timer.clock()))
		if err := m.readFinancialRecords(ctx, at); err != nil {
			t.Fatal(err)
		}
	}
	read(settlement.Add(-40 * time.Second))
	r.timer.advance(40*time.Second + 200*time.Millisecond)
	r.bullBook(101, lv("40.46", "5")) // the first frame after the settlement: mid 40.455
	read(settlement.Add(20 * time.Second))
	read(settlement.Add(80 * time.Second))
	if got := m.FinancialRecords(); len(got) != 0 {
		t.Fatalf("before the rate is published the manager holds %+v", got)
	}
	r.real.funding["SOXLUSDT"] = []FundingSettlement{{Symbol: "SOXLUSDT", Rate: dec("0.0001"), At: settlement}}
	read(settlement.Add(140 * time.Second))
	got := m.FinancialRecords()
	if len(got) != 1 || got[0].Symbol != "SOXLUSDT" || !got[0].Amount.Equal(dec("-0.040455")) || !got[0].At.Equal(settlement) {
		t.Fatalf("the manager holds %+v; want the paper record of the settlement, -10 x 40.455 x 0.0001", got)
	}
	// Later reads return the record again; the manager keeps it once.
	read(settlement.Add(200 * time.Second))
	if got := m.FinancialRecords(); len(got) != 1 {
		t.Errorf("after another read the manager holds %d records, want the one", len(got))
	}
}

// Each paper order is matched on a timer goroutine of its own, while a
// venue's events arrive one at a time. With real timers and a subscriber
// that is slow on the first event it gets, the two orders of a batch are
// matched at the same moment on two goroutines: the events of the match
// that happened first must still all arrive before the other's, or the
// position set that arrives last is the older one and lacks a leg.
func TestPaperEventsOfConcurrentMatchesArriveInOrder(t *testing.T) {
	for round := 0; round < 20; round++ {
		real := newFakeVenue()
		p := NewPaperVenue(real, PaperConfig{Pair: testConfig().Pair, Latency: [2]time.Duration{Bull: time.Millisecond, Bear: time.Millisecond}, TakerFeeBps: 0.65, Leverage: 10, DepthMaxAge: DefaultConfig.DepthMaxAge})
		ctx := context.Background()
		for _, symbol := range []Symbol{"SOXLUSDT", "SOXSUSDT"} {
			if err := p.SubscribeBooks(ctx, []BookSubscription{{Symbol: symbol, Topic: TopicBooks5}}, func(BookEvent) {}, func(error) {}); err != nil {
				t.Fatal(err)
			}
		}
		now := time.Now()
		real.subscriptionsOf("SOXLUSDT")[0].onEvent(BookEvent{Symbol: "SOXLUSDT", Topic: TopicBooks5, Action: ActionSnapshot, Bids: []BookLevel{lv("40.45", "10")}, Asks: []BookLevel{lv("40.46", "50")}, Seq: 100, ReceivedAt: now})
		real.subscriptionsOf("SOXSUSDT")[0].onEvent(BookEvent{Symbol: "SOXSUSDT", Topic: TopicBooks5, Action: ActionSnapshot, Bids: []BookLevel{lv("49.707", "14")}, Asks: []BookLevel{lv("49.718", "30")}, Seq: 101, ReceivedAt: now})

		var mu sync.Mutex
		var sets []PositionSet
		var orders []ClientOrderID // by event, the order it belongs to; a position set belongs to the report before it
		first := true
		if err := p.SubscribePrivate(ctx, []Symbol{"SOXLUSDT", "SOXSUSDT"}, func(ev PrivateEvent) {
			mu.Lock()
			slow := first
			first = false
			mu.Unlock()
			if slow {
				time.Sleep(20 * time.Millisecond) // the other order is matched meanwhile
			}
			mu.Lock()
			defer mu.Unlock()
			switch {
			case ev.Fill != nil:
				orders = append(orders, ev.Fill.ClientOrderID)
			case ev.Order != nil:
				orders = append(orders, ev.Order.Request.ClientOrderID)
			case ev.Positions != nil:
				orders = append(orders, orders[len(orders)-1])
				sets = append(sets, *ev.Positions)
			}
		}, func(error) {}); err != nil {
			t.Fatal(err)
		}
		bull, bear := orderReq(1, Bull, PurposeEntry, SideBuy, "40.47", "10"), orderReq(2, Bear, PurposeEntry, SideBuy, "49.72", "8")
		for _, req := range []OrderRequest{bull, bear} {
			if _, err := p.PlaceOrder(ctx, req); err != nil {
				t.Fatal(err)
			}
		}
		waitFor(t, "the events of both matches", func() bool { mu.Lock(); defer mu.Unlock(); return len(sets) == 2 })
		mu.Lock()
		last, got := sets[1], slices.Clone(orders)
		mu.Unlock()
		if !last.BySymbol["SOXLUSDT"].Qty.Equal(dec("10")) || !last.BySymbol["SOXSUSDT"].Qty.Equal(dec("8")) {
			t.Fatalf("round %d: the position set delivered last is %+v; the venue holds 10 and 8", round, last.BySymbol)
		}
		// Fill, report, positions of one order, then the same of the other.
		if len(got) != 6 || got[0] != got[1] || got[1] != got[2] || got[3] != got[4] || got[4] != got[5] || got[0] == got[3] {
			t.Fatalf("round %d: the events arrived as %v; want one order's three events, then the other's", round, got)
		}
	}
}

// Paper mode end to end through the manager (V18): orders placed through
// the order table are matched by the paper venue and come back as final
// records, their rows and fills are written, the positions are published,
// and the wrapped real venue receives no order and no account request.
func TestPaperModeThroughTheManager(t *testing.T) {
	real := newFakeVenue()
	paper := NewPaperVenue(real, PaperConfig{Pair: testConfig().Pair, Latency: [2]time.Duration{Bull: 2 * time.Millisecond, Bear: 2 * time.Millisecond}, TakerFeeBps: 0.65, Leverage: 10, DepthMaxAge: DefaultConfig.DepthMaxAge})
	sink := &store.Fake{}
	m, err := NewManager(testConfig(), paper, &fakeIndex{}, &fakeFX{}, &fakeIssuer{})
	if err != nil {
		t.Fatal(err)
	}
	m.UseStore(sink)
	paper.UseStore(sink)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.runOrders(ctx); close(done) }()
	go m.runBooks(ctx)
	t.Cleanup(func() { cancel(); <-done })
	waitFor(t, "the book subscriptions", func() bool {
		return len(real.subscriptionsOf("SOXLUSDT")) == 1 && len(real.subscriptionsOf("SOXSUSDT")) == 1
	})
	now := time.Now()
	real.subscriptionsOf("SOXLUSDT")[0].onEvent(BookEvent{Symbol: "SOXLUSDT", Topic: TopicBooks5, Action: ActionSnapshot, Bids: []BookLevel{lv("40.45", "10")}, Asks: []BookLevel{lv("40.46", "50")}, Seq: 100, ReceivedAt: now})
	real.subscriptionsOf("SOXSUSDT")[0].onEvent(BookEvent{Symbol: "SOXSUSDT", Topic: TopicBooks50, Action: ActionSnapshot, Bids: []BookLevel{lv("49.707", "14")}, Asks: []BookLevel{lv("49.718", "3")}, Seq: 101, ReceivedAt: now})
	waitFor(t, "the books", func() bool { b := m.Books(); return b.ByLeg[Bull].DepthSeq == 100 && b.ByLeg[Bear].DepthSeq == 101 })

	bull, bear := orderReq(1, Bull, PurposeEntry, SideBuy, "40.47", "10"), orderReq(2, Bear, PurposeEntry, SideBuy, "49.72", "8")
	if err := m.PlaceOrders(bull, bear); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "both outcomes", func() bool { return len(m.UnresolvedOrders()) == 0 && len(m.CycleOrders(testCycleID)) == 2 })
	orders := m.CycleOrders(testCycleID)
	if o := orders[0]; o.Status != StatusFilled || !o.CumExecQty.Equal(dec("10")) || !o.CumExecValue.Equal(dec("404.6")) || !o.Fee.Equal(dec("0.026299")) || !o.FeeKnown || o.Unknown {
		t.Errorf("the bull order = %+v", o)
	}
	if o := orders[1]; o.Status != StatusCancelled || !o.CumExecQty.Equal(dec("3")) || !o.CumExecValue.Equal(dec("149.154")) || !o.FeeKnown {
		t.Errorf("the bear order = %+v, want 3 of 8 filled", o)
	}
	waitFor(t, "the positions", func() bool { return m.Positions().BySymbol["SOXSUSDT"].Qty.Equal(dec("3")) })
	if p := m.Positions(); !p.BySymbol["SOXLUSDT"].Qty.Equal(dec("10")) {
		t.Errorf("positions = %+v", p)
	}
	if read, err := m.ReadPositions(ctx); err != nil || !read.BySymbol["SOXLUSDT"].Qty.Equal(dec("10")) {
		t.Errorf("ReadPositions = %+v, %v", read, err)
	}
	if fills := sink.Rows("cex_fill"); len(fills) != 2 {
		t.Errorf("%d fill rows, want one per leg", len(fills))
	}
	var last store.OrderRow
	for _, row := range sink.Rows("cex_order") {
		if o := row.(store.OrderRow); o.ClientOrderID == string(bull.ClientOrderID) {
			last = o
		}
	}
	if last.Status != "filled" || !last.CumExecValue.Equal(dec("404.6")) || !last.QuotaPrice.Equal(dec("40.47")) {
		t.Errorf("the bull order's last row = %+v", last)
	}
	for _, method := range []string{"PlaceOrder", "AccountSettings", "Positions", "OrderByClientID", "RecentOrders", "Fills", "FinancialRecords", "SubscribePrivate"} {
		if n := real.callCount(method); n != 0 {
			t.Errorf("the wrapped real venue's %s was called %d times in paper mode", method, n)
		}
	}
}
