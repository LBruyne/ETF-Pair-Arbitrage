package cex

import (
	"context"
	"testing"
	"time"
)

// timerClock is a manualTimer as a Clock, the shape a replay passes.
type timerClock struct{ *manualTimer }

func (c timerClock) Now() time.Time                      { return c.clock() }
func (c timerClock) AfterFunc(d time.Duration, f func()) { c.after(d, f) }

// syncRig is a manager built for a replay: on a manual clock, synchronous,
// started with StartSynchronous on a fake venue that has rules, settings
// and positions.
func syncRig(t *testing.T) (*CexManager, *fakeVenue, *manualTimer) {
	t.Helper()
	timer := &manualTimer{now: t0}
	venue := newFakeVenue()
	venue.rules = map[Symbol]MarketRule{"SOXLUSDT": soxlRule, "SOXSUSDT": soxsRule}
	venue.settings = goodSettings()
	venue.positions = PositionSet{BySymbol: map[Symbol]Position{}}
	m, err := NewManager(testConfig(), venue, &fakeIndex{}, &fakeFX{}, &fakeIssuer{}, WithClock(timerClock{timer}), Synchronous())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.StartSynchronous(context.Background()); err != nil {
		t.Fatal(err)
	}
	return m, venue, timer
}

// A manager started with StartSynchronous reads the rules, the settings and
// the positions once, subscribes the books and the private stream, and
// starts no loop: a frame the venue delivers is in the published books when
// the delivery returns, stamped by nothing but the frame.
func TestSynchronousManagerAppliesFramesWhenDelivered(t *testing.T) {
	m, venue, _ := syncRig(t)
	if ok, detail := m.SettingsOK(10); !m.TradingReady() || !ok || !m.Positions().Loaded {
		t.Fatalf("after StartSynchronous: ready %t, settings %t (%s), positions loaded %t", m.TradingReady(), ok, detail, m.Positions().Loaded)
	}
	if venue.callCount("SubscribePrivate") != 1 || len(venue.subscriptionsOf("SOXLUSDT")) != 1 || len(venue.subscriptionsOf("SOXSUSDT")) != 1 {
		t.Fatalf("subscriptions: private %d, books %d / %d", venue.callCount("SubscribePrivate"), len(venue.subscriptionsOf("SOXLUSDT")), len(venue.subscriptionsOf("SOXSUSDT")))
	}
	changed := m.BookChanged()
	venue.subscriptionsOf("SOXLUSDT")[0].onEvent(legFrame("SOXLUSDT", TopicBooks5, ActionSnapshot, 100, 0, lv("40.45", "10"), lv("40.46", "5"), t0))
	if b := m.Books().ByLeg[Bull]; b.DepthSeq != 100 || !b.DepthReceivedAt.Equal(t0) {
		t.Fatalf("right after the delivery the book is seq %d received %s, want 100 at the frame's arrival", b.DepthSeq, showTime(b.DepthReceivedAt))
	}
	select {
	case <-changed:
	default:
		t.Error("BookChanged was not closed by the delivery")
	}
	// The loss of the leg's only connection drops its depth at once.
	venue.subscriptionsOf("SOXLUSDT")[0].onDown(nil)
	if b := m.Books().ByLeg[Bull]; b.Connected || len(b.Bids) != 0 {
		t.Errorf("after the loss: connected %t, %d bid levels", b.Connected, len(b.Bids))
	}
}

// A synchronous manager hands each order to the venue within PlaceOrders,
// on the caller's goroutine, and stamps it with its clock: when the call
// returns the venue has the order and the record shows its answer.
func TestSynchronousManagerSendsOrdersWithinTheCall(t *testing.T) {
	m, venue, timer := syncRig(t)
	venue.place = func(req OrderRequest) (OrderAck, error) {
		return OrderAck{OrderID: "v-1", ReceivedAt: timer.clock(), AcceptedAt: timer.clock()}, nil
	}
	timer.now = t0.Add(time.Second)
	req := orderReq(1, Bull, PurposeEntry, SideBuy, "40.46", "5")
	if err := m.PlaceOrders(req); err != nil {
		t.Fatal(err)
	}
	if ids := venue.placedIDs(); len(ids) != 1 || ids[0] != req.ClientOrderID {
		t.Fatalf("the venue has %v when PlaceOrders returns, want the order", ids)
	}
	orders := m.CycleOrders(testCycleID)
	if len(orders) != 1 || orders[0].Status != StatusAcked || orders[0].OrderID != "v-1" || !orders[0].SubmittedAt.Equal(t0.Add(time.Second)) || !orders[0].SentAt.Equal(t0.Add(time.Second)) {
		t.Fatalf("the record = %+v, want acked, submitted and sent on the manager's clock", orders)
	}
}

// The paper venue runs on the clock its configuration names: an order is
// accepted at the clock's time and matched only when the clock's timer
// fires, not on a timer of its own.
func TestPaperVenueRunsOnTheConfiguredClock(t *testing.T) {
	timer := &manualTimer{now: t0}
	real := newFakeVenue()
	p := NewPaperVenue(real, PaperConfig{Pair: testConfig().Pair, Latency: [2]time.Duration{Bull: 15 * time.Millisecond, Bear: 15 * time.Millisecond}, TakerFeeBps: 0.65, Leverage: 10,
		DepthMaxAge: 10 * time.Second, Clock: timerClock{timer}})
	ctx := context.Background()
	if err := p.SubscribeBooks(ctx, []BookSubscription{{Symbol: "SOXLUSDT", Topic: TopicBooks5}, {Symbol: "SOXLUSDT", Topic: TopicBooks1}}, func(BookEvent) {}, func(error) {}); err != nil {
		t.Fatal(err)
	}
	var reports []OrderRecord
	p.SubscribePrivate(ctx, nil, func(ev PrivateEvent) {
		if ev.Order != nil {
			reports = append(reports, *ev.Order)
		}
	}, func(error) {})
	real.subscriptionsOf("SOXLUSDT")[0].onEvent(legFrame("SOXLUSDT", TopicBooks5, ActionSnapshot, 100, 0, lv("40.45", "10"), lv("40.46", "5"), t0))
	ack, err := p.PlaceOrder(ctx, orderReq(1, Bull, PurposeEntry, SideBuy, "40.46", "5"))
	if err != nil || !ack.ReceivedAt.Equal(t0) {
		t.Fatalf("PlaceOrder = %+v, %v; want accepted at the clock's time", ack, err)
	}
	time.Sleep(30 * time.Millisecond) // a wall-clock timer would have fired by now
	if len(reports) != 0 {
		t.Fatal("matched on a wall-clock timer")
	}
	timer.advance(15 * time.Millisecond)
	if len(reports) != 1 || reports[0].Status != StatusFilled || !reports[0].ExchangeUpdatedAt.Equal(t0.Add(15*time.Millisecond)) {
		t.Fatalf("reports after the clock's 15 ms = %+v", reports)
	}
}

// A paper fill's fee is cut to the venue's 8 decimals (toward zero), and the
// order's fee is the sum of its fills' fees: 6.22215 x 0.65 bps =
// 0.00040443975 -> 0.00040443, 6.2226 x 0.65 bps = 0.000404469 ->
// 0.00040446; the order 0.00080889 (the total cut once would be 0.00080890).
func TestPaperFeesAreCutToEightDecimalsPerFill(t *testing.T) {
	r := newPaperRig(t)
	r.bear.onEvent(BookEvent{Symbol: "SOXSUSDT", Topic: TopicBooks5, Action: ActionSnapshot, Bids: []BookLevel{lv("69.128", "15.3")},
		Asks: []BookLevel{lv("69.135", "0.09"), lv("69.14", "0.09"), lv("69.142", "0.18")}, Seq: 100, ReceivedAt: r.timer.clock()})
	r.place(t, orderReq(1, Bear, PurposeEntry, SideBuy, "69.14", "0.18"))
	r.timer.advance(40 * time.Millisecond)
	fills, orders, _ := r.take()
	if len(fills) != 2 || len(orders) != 1 {
		t.Fatalf("%d fills and %d reports, want 2 and 1", len(fills), len(orders))
	}
	if !fills[0].Fee.Equal(dec("0.00040443")) || !fills[1].Fee.Equal(dec("0.00040446")) {
		t.Errorf("fill fees %s and %s, want 0.00040443 and 0.00040446", fills[0].Fee, fills[1].Fee)
	}
	if o := orders[0]; o.Status != StatusFilled || !o.Fee.Equal(dec("0.00080889")) || !o.Fee.Equal(fills[0].Fee.Add(fills[1].Fee)) {
		t.Errorf("order fee %s (status %s), want the sum of the fills' 0.00080889", o.Fee, o.Status)
	}
}

// A synchronous manager keeps no per-frame delays: only Run's status line
// reads and resets them, and it never runs.
func TestSynchronousManagerKeepsNoFrameDelays(t *testing.T) {
	m, venue, _ := syncRig(t)
	on := venue.subscriptionsOf("SOXLUSDT")[0].onEvent
	for i := range 1000 {
		ev := legFrame("SOXLUSDT", TopicBooks5, ActionSnapshot, uint64(100+i), 0, lv("40.45", "10"), lv("40.46", "5"), t0)
		ev.ExchangeTime = t0.Add(-3 * time.Millisecond)
		on(ev)
	}
	if n := len(m.streams[Bull][0].delays); n != 0 {
		t.Errorf("%d frame delays kept, want none", n)
	}
	if m.streams[Bull][0].frames != 1000 {
		t.Errorf("%d frames counted, want 1000", m.streams[Bull][0].frames)
	}
}
