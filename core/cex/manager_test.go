package cex

import (
	"context"
	"testing"
	"time"
)

// startBooks runs the book loop of a rig until the test ends and returns the
// two legs' subscriptions.
func startBooks(t *testing.T, r *testRig) (bull, bear *fakeSubscription) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.m.runBooks(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	waitFor(t, "both legs' subscriptions", func() bool {
		return len(r.venue.subscriptionsOf("SOXLUSDT")) == 1 && len(r.venue.subscriptionsOf("SOXSUSDT")) == 1
	})
	return r.venue.subscriptionsOf("SOXLUSDT")[0], r.venue.subscriptionsOf("SOXSUSDT")[0]
}

// legFrame is a frame of the given symbol and topic.
func legFrame(symbol Symbol, topic string, action BookAction, seq, prev uint64, bid, ask BookLevel, at time.Time) BookEvent {
	return BookEvent{Symbol: symbol, Topic: topic, Action: action, Bids: []BookLevel{bid}, Asks: []BookLevel{ask}, Seq: seq, PrevSeq: prev, ReceivedAt: at}
}

// Each leg subscribes its configured depth topic and books1 on a
// subscription of its own, and the frames reach the published books.
func TestManagerSubscribesAndPublishesBooks(t *testing.T) {
	r := newRig(t, testConfig(), t0)
	if b := r.m.Books().ByLeg[Bull]; b == nil || b.Symbol != "SOXLUSDT" || r.m.Books().DepthUsable(Bull, t0) || r.m.Books().HasPrice(Bull, t0) {
		t.Fatalf("before any frame: book %+v must be empty and unusable, never nil", b)
	}
	bull, bear := startBooks(t, r)
	wantSubs := func(s *fakeSubscription, symbol Symbol, depthTopic string) {
		t.Helper()
		if len(s.subs) != 2 || s.subs[0] != (BookSubscription{symbol, depthTopic}) || s.subs[1] != (BookSubscription{symbol, TopicBooks1}) {
			t.Errorf("%s subscribed %v, want %s and books1", symbol, s.subs, depthTopic)
		}
	}
	wantSubs(bull, "SOXLUSDT", TopicBooks5)
	wantSubs(bear, "SOXSUSDT", TopicBooks50)

	// A reader takes the channel before the frame: the publication closes it.
	changed := r.m.BookChanged()
	bull.onEvent(legFrame("SOXLUSDT", TopicBooks5, ActionSnapshot, 100, 0, lv("40.45", "10"), lv("40.46", "5"), t0))
	select {
	case <-changed:
	case <-time.After(3 * time.Second):
		t.Fatal("BookChanged was not closed by the publication")
	}
	bear.onEvent(legFrame("SOXSUSDT", TopicBooks50, ActionSnapshot, 101, 0, lv("49.707", "14"), lv("49.718", "13"), t0))
	bear.onEvent(legFrame("SOXSUSDT", TopicBooks1, ActionSnapshot, 102, 0, lv("49.707", "9"), lv("49.718", "13"), t0))
	waitFor(t, "the bear leg's frames", func() bool { return r.m.Books().ByLeg[Bear].BestBid.Qty.Equal(dec("9")) })

	b := r.m.Books().ByLeg[Bull]
	if b.DepthSeq != 100 || !b.Connected || !b.Bids[0].Price.Equal(dec("40.45")) || !r.m.Books().DepthUsable(Bull, t0) || !r.m.Books().HasPrice(Bull, t0) {
		t.Errorf("bull book = %+v, want the frame's depth, connected and usable", b)
	}
	if b := r.m.Books().ByLeg[Bear]; b.DepthSeq != 101 || !b.Bids[0].Qty.Equal(dec("9")) || !r.m.Books().DepthUsable(Bear, t0) {
		t.Errorf("bear book = %+v, want depth 101 with books1's quantity on its first level", b)
	}
}

// One Books value answers every question on the same pair of books. A frame
// published after it was taken changes the manager's next read and nothing
// in the value: its validity answers and the levels it holds cannot come to
// disagree.
func TestBooksIsOneConsistentRead(t *testing.T) {
	r := newRig(t, testConfig(), t0) // depth_max_age 10 s
	bull, bear := startBooks(t, r)
	bull.onEvent(legFrame("SOXLUSDT", TopicBooks5, ActionSnapshot, 100, 0, lv("40.45", "10"), lv("40.46", "5"), t0))
	bear.onEvent(legFrame("SOXSUSDT", TopicBooks50, ActionSnapshot, 101, 0, lv("49.707", "14"), lv("49.718", "13"), t0))
	waitFor(t, "both depths", func() bool { b := r.m.Books(); return b.ByLeg[Bull].DepthSeq == 100 && b.ByLeg[Bear].DepthSeq == 101 })

	taken := r.m.Books()
	now := t0.Add(time.Second)
	if !taken.DepthOK(Bull, now) || !taken.DepthOK(Bear, now) || !taken.HasPrice(Bull, now) || taken.MaxAge != 10*time.Second {
		t.Fatalf("the read taken: %+v, want both depths good", taken)
	}
	// Then the bull leg's best bid moves away (a conflict), a new depth
	// replaces its levels, and the bear leg's connection is lost.
	bull.onEvent(legFrame("SOXLUSDT", TopicBooks1, ActionSnapshot, 102, 0, lv("40.40", "1"), lv("40.46", "5"), now))
	bear.onDown(context.DeadlineExceeded)
	waitFor(t, "the later publications", func() bool { b := r.m.Books(); return b.ByLeg[Bull].Conflict && !b.ByLeg[Bear].Connected })
	latest := r.m.Books()
	if latest.DepthOK(Bull, now) || latest.DepthUsable(Bear, now) || !latest.DepthUsable(Bull, now) || !latest.HasPrice(Bear, now) {
		t.Errorf("the next read: bull ok %t usable %t, bear usable %t priced %t; want the conflict and the loss", latest.DepthOK(Bull, now), latest.DepthUsable(Bull, now), latest.DepthUsable(Bear, now), latest.HasPrice(Bear, now))
	}

	// The value taken before still answers as it did, about the levels it
	// still holds.
	for _, leg := range Legs {
		if !taken.DepthOK(leg, now) || !taken.DepthUsable(leg, now) || !taken.HasPrice(leg, now) {
			t.Errorf("%s: the read taken earlier changed its answers", leg)
		}
	}
	if b := taken.ByLeg[Bull]; b.DepthSeq != 100 || b.Conflict || !b.BestBid.Price.Equal(dec("40.45")) || !b.Bids[0].Price.Equal(dec("40.45")) {
		t.Errorf("the bull book of the read taken earlier changed: %+v", b)
	}
	if b := taken.ByLeg[Bear]; b.DepthSeq != 101 || !b.Connected || len(b.Bids) != 1 {
		t.Errorf("the bear book of the read taken earlier changed: %+v", b)
	}
	// Its answers depend on the time asked about and nothing else.
	if taken.DepthOK(Bull, t0.Add(10*time.Second+time.Millisecond)) || taken.HasPrice(Bull, t0.Add(10*time.Second+time.Millisecond)) {
		t.Error("the read taken earlier is still good past depth_max_age")
	}
}

// A depth is usable for depth_max_age after its last depth frame. books1 and
// the other leg's frames do not extend it, but books1 alone still gives the
// leg a price, until that too is older than depth_max_age.
func TestManagerDepthAgesOutWhileBooks1GivesAPrice(t *testing.T) {
	r := newRig(t, testConfig(), t0) // depth_max_age 10 s
	bull, bear := startBooks(t, r)
	bull.onEvent(legFrame("SOXLUSDT", TopicBooks5, ActionSnapshot, 100, 0, lv("40.45", "10"), lv("40.46", "5"), t0))
	// Nine seconds on, only books1 and the other leg speak.
	bull.onEvent(legFrame("SOXLUSDT", TopicBooks1, ActionSnapshot, 101, 0, lv("40.45", "7"), lv("40.46", "5"), t0.Add(9*time.Second)))
	bear.onEvent(legFrame("SOXSUSDT", TopicBooks50, ActionSnapshot, 102, 0, lv("49.707", "14"), lv("49.718", "13"), t0.Add(9*time.Second)))
	waitFor(t, "the frames", func() bool {
		return r.m.Books().ByLeg[Bull].BestBid.Qty.Equal(dec("7")) && r.m.Books().ByLeg[Bear].DepthSeq == 102
	})

	for _, c := range []struct {
		after              time.Duration
		depthOK, hasPrice  bool
		bearDepth, bearHas bool
	}{
		{10 * time.Second, true, true, true, true},                   // exactly depth_max_age: still usable
		{10*time.Second + time.Millisecond, false, true, true, true}, // the depth is too old, books1 (9 s) still gives a price
		{19 * time.Second, false, true, true, true},
		{19*time.Second + time.Millisecond, false, false, false, false}, // both streams silent for more than 10 s
	} {
		now := t0.Add(c.after)
		if got := r.m.Books().DepthUsable(Bull, now); got != c.depthOK {
			t.Errorf("bull DepthUsable at +%s = %t, want %t", c.after, got, c.depthOK)
		}
		if got := r.m.Books().HasPrice(Bull, now); got != c.hasPrice {
			t.Errorf("bull HasPrice at +%s = %t, want %t", c.after, got, c.hasPrice)
		}
		if got := r.m.Books().DepthUsable(Bear, now); got != c.bearDepth {
			t.Errorf("bear DepthUsable at +%s = %t, want %t", c.after, got, c.bearDepth)
		}
		if got := r.m.Books().HasPrice(Bear, now); got != c.bearHas {
			t.Errorf("bear HasPrice at +%s = %t, want %t", c.after, got, c.bearHas)
		}
	}
	// A new depth frame makes the leg usable again.
	bull.onEvent(legFrame("SOXLUSDT", TopicBooks5, ActionSnapshot, 110, 0, lv("40.45", "10"), lv("40.46", "5"), t0.Add(30*time.Second)))
	waitFor(t, "the new depth", func() bool { return r.m.Books().ByLeg[Bull].DepthSeq == 110 })
	if !r.m.Books().DepthUsable(Bull, t0.Add(31*time.Second)) {
		t.Error("a new depth frame did not make the leg usable again")
	}
}

// A lost connection makes its leg's depth unusable at once and leaves the
// other leg alone; the leg keeps a price while its last best price is young.
func TestManagerConnectionLossDropsTheDepth(t *testing.T) {
	r := newRig(t, testConfig(), t0)
	bull, bear := startBooks(t, r)
	bull.onEvent(legFrame("SOXLUSDT", TopicBooks5, ActionSnapshot, 100, 0, lv("40.45", "10"), lv("40.46", "5"), t0))
	bull.onEvent(legFrame("SOXLUSDT", TopicBooks1, ActionSnapshot, 101, 0, lv("40.45", "7"), lv("40.46", "5"), t0))
	bear.onEvent(legFrame("SOXSUSDT", TopicBooks50, ActionSnapshot, 102, 0, lv("49.707", "14"), lv("49.718", "13"), t0))
	waitFor(t, "the frames", func() bool {
		return r.m.Books().ByLeg[Bull].BestBid.Qty.Equal(dec("7")) && r.m.Books().ByLeg[Bear].DepthSeq == 102
	})

	bull.onDown(context.DeadlineExceeded)
	waitFor(t, "the loss", func() bool { return !r.m.Books().ByLeg[Bull].Connected })
	now := t0.Add(time.Second)
	if b := r.m.Books().ByLeg[Bull]; len(b.Bids) != 0 || r.m.Books().DepthUsable(Bull, now) || !r.m.Books().HasPrice(Bull, now) {
		t.Errorf("after the loss: bull book %+v, DepthUsable %t, HasPrice %t; want no depth, unusable, a price", b, r.m.Books().DepthUsable(Bull, now), r.m.Books().HasPrice(Bull, now))
	}
	if !r.m.Books().DepthUsable(Bear, now) {
		t.Error("the other leg lost its depth too")
	}
	// The venue reconnects by itself: the same subscription delivers again.
	bull.onEvent(legFrame("SOXLUSDT", TopicBooks5, ActionSnapshot, 120, 0, lv("40.44", "3"), lv("40.47", "5"), now))
	waitFor(t, "the depth after the reconnect", func() bool { return r.m.Books().DepthUsable(Bull, now) })
	if len(r.venue.subscriptionsOf("SOXLUSDT")) != 1 {
		t.Error("a connection loss must not make the manager subscribe again: the venue reconnects")
	}
}

// The best price is steady while the levels behind it change: books1 is
// silent and the depth keeps arriving. When the connection is then lost the
// leg still has a price, the depth's first level received half a second
// before, for the rest of depth_max_age (PRD 3.1). The manager reports the
// loss as a state change, not a second warning: the venue's stream already
// warned.
func TestManagerConnectionLossKeepsTheDepthsBestPrice(t *testing.T) {
	r := newRig(t, testConfig(), t0) // depth_max_age 10 s
	logs := captureLogs(t)
	bull, _ := startBooks(t, r)
	bull.onEvent(legFrame("SOXLUSDT", TopicBooks1, ActionSnapshot, 100, 0, lv("40.45", "7"), lv("40.46", "5"), t0))
	for i := 1; i <= 24; i++ { // a depth frame every half second up to 12.0 s
		bull.onEvent(legFrame("SOXLUSDT", TopicBooks5, ActionSnapshot, 100+uint64(i), 0, lv("40.45", "7"), lv("40.46", "5"), t0.Add(time.Duration(i)*500*time.Millisecond)))
	}
	waitFor(t, "the depth frames", func() bool { return r.m.Books().ByLeg[Bull].DepthSeq == 124 })
	lost := t0.Add(12500 * time.Millisecond)
	if !r.m.Books().HasPrice(Bull, lost) {
		t.Fatal("no price before the loss")
	}
	logs.take()
	bull.onDown(context.DeadlineExceeded)
	waitFor(t, "the loss", func() bool { return !r.m.Books().ByLeg[Bull].Connected })
	if got := logs.take(); len(got) != 1 || got[0] != "INFO cex: book down" {
		t.Errorf("the loss was logged as %q, want one Info line", got)
	}
	if r.m.Books().DepthUsable(Bull, lost) {
		t.Error("the depth is usable after the loss")
	}
	b := r.m.Books().ByLeg[Bull]
	if !b.BestReceivedAt.Equal(t0.Add(12*time.Second)) || !b.BestBid.Price.Equal(dec("40.45")) {
		t.Fatalf("after the loss the best price is %s received %s, want the depth's of 12.0 s", b.BestBid.Price, b.BestReceivedAt.Sub(t0))
	}
	for _, c := range []struct {
		at   time.Duration
		want bool
	}{{12500 * time.Millisecond, true}, {22 * time.Second, true}, {22*time.Second + time.Millisecond, false}} {
		if got := r.m.Books().HasPrice(Bull, t0.Add(c.at)); got != c.want {
			t.Errorf("HasPrice at +%s = %t, want %t: the price is good for depth_max_age after the frame that carried it", c.at, got, c.want)
		}
	}
}

// The inbox never blocks the stream's reader. While the book loop does not
// take its inputs, the inbox fills; every input after that is dropped and
// counted, and the reader's callback returns at once. The inputs kept are
// the oldest ones.
func TestManagerFullInboxDropsWithoutBlocking(t *testing.T) {
	r := newRig(t, testConfig(), t0)
	r.m.bookInputs = make(chan bookInput, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.m.openBookStream(ctx, Bull) // subscribed, with no loop reading the inbox
	bull := r.venue.subscriptionsOf("SOXLUSDT")[0]
	delivered := make(chan struct{})
	go func() {
		for seq := uint64(1); seq <= 12; seq++ {
			bull.onEvent(legFrame("SOXLUSDT", TopicBooks5, ActionSnapshot, seq, 0, lv("40.45", "10"), lv("40.46", "5"), t0))
		}
		bull.onDown(context.DeadlineExceeded) // a loss arriving at a full inbox is dropped too
		close(delivered)
	}()
	select {
	case <-delivered:
	case <-time.After(3 * time.Second):
		t.Fatal("the stream's reader blocked on a full inbox")
	}
	if got := r.m.droppedFrames.Load(); got != 5 {
		t.Errorf("%d inputs dropped, want the four frames and the loss beyond the inbox's eight", got)
	}
	// What was kept is frames 1 to 8, applied here as the loop would.
	for len(r.m.bookInputs) > 0 {
		r.m.handleBookInput(ctx, <-r.m.bookInputs)
	}
	if b := r.m.Books().ByLeg[Bull]; b.DepthSeq != 8 || !b.Connected {
		t.Errorf("after the kept inputs the book is at seq %d, connected %t; want frame 8 and no loss", b.DepthSeq, b.Connected)
	}
}

// An update that does not chain drops the leg's depth and subscribes that
// leg again, once: the old subscription is cancelled and whatever it still
// delivers is ignored, the other leg is not touched.
func TestManagerBrokenChainResubscribesTheLegOnce(t *testing.T) {
	r := newRig(t, testConfig(), t0)
	_, bear := startBooks(t, r)
	bear.onEvent(legFrame("SOXSUSDT", TopicBooks50, ActionSnapshot, 100, 0, lv("49.707", "14"), lv("49.718", "13"), t0))
	bear.onEvent(legFrame("SOXSUSDT", TopicBooks50, ActionUpdate, 101, 100, lv("49.707", "12"), lv("49.718", "13"), t0))
	waitFor(t, "the chained update", func() bool { return r.m.Books().ByLeg[Bear].DepthSeq == 101 })

	bear.onEvent(legFrame("SOXSUSDT", TopicBooks50, ActionUpdate, 105, 103, lv("49.707", "11"), lv("49.718", "13"), t0))
	waitFor(t, "the resubscription", func() bool { return len(r.venue.subscriptionsOf("SOXSUSDT")) == 2 })
	if bear.ctx.Err() == nil {
		t.Error("the replaced subscription was not cancelled")
	}
	waitFor(t, "the depth to be dropped", func() bool { return r.m.Books().ByLeg[Bear].DepthSeq == 0 })
	if r.m.Books().DepthUsable(Bear, t0) {
		t.Error("the leg is usable without a depth")
	}

	// More updates and even a snapshot from the replaced subscription change
	// nothing and cause no further subscription.
	bear.onEvent(legFrame("SOXSUSDT", TopicBooks50, ActionUpdate, 106, 105, lv("49.707", "10"), lv("49.718", "13"), t0))
	bear.onEvent(legFrame("SOXSUSDT", TopicBooks50, ActionSnapshot, 107, 0, lv("49.700", "1"), lv("49.720", "1"), t0))
	// The new subscription's snapshot brings the depth back.
	fresh := r.venue.subscriptionsOf("SOXSUSDT")[1]
	fresh.onEvent(legFrame("SOXSUSDT", TopicBooks50, ActionSnapshot, 110, 0, lv("49.706", "4"), lv("49.719", "5"), t0))
	waitFor(t, "the new snapshot", func() bool { return r.m.Books().ByLeg[Bear].DepthSeq == 110 })
	if got := r.m.Books().ByLeg[Bear].Bids[0].Price; !got.Equal(dec("49.706")) {
		t.Errorf("best bid = %s, want the new subscription's 49.706", got)
	}
	// The replaced subscription's connection closing is not this leg's
	// loss: the new subscription's depth stays. The frame after it shows
	// that the loss had been taken from the inbox by then.
	bear.onDown(context.Canceled)
	fresh.onEvent(legFrame("SOXSUSDT", TopicBooks1, ActionSnapshot, 111, 0, lv("49.706", "3"), lv("49.719", "5"), t0))
	waitFor(t, "the frame after the stale loss", func() bool { return r.m.Books().ByLeg[Bear].BestBid.Qty.Equal(dec("3")) })
	if b := r.m.Books().ByLeg[Bear]; b.DepthSeq != 110 || !b.Connected || !r.m.Books().DepthUsable(Bear, t0) {
		t.Errorf("a loss reported by the replaced subscription dropped the new one's depth: %+v", b)
	}
	if n := len(r.venue.subscriptionsOf("SOXSUSDT")); n != 2 {
		t.Errorf("the bear leg was subscribed %d times, want 2", n)
	}
	if n := len(r.venue.subscriptionsOf("SOXLUSDT")); n != 1 {
		t.Errorf("the bull leg was subscribed %d times, want 1", n)
	}
}

// A conflict is published with the book and does not make the depth
// unusable by age: the two are separate facts for the reader.
func TestManagerPublishesConflict(t *testing.T) {
	r := newRig(t, testConfig(), t0)
	bull, _ := startBooks(t, r)
	bull.onEvent(legFrame("SOXLUSDT", TopicBooks5, ActionSnapshot, 100, 0, lv("40.45", "10"), lv("40.46", "5"), t0))
	bull.onEvent(legFrame("SOXLUSDT", TopicBooks1, ActionSnapshot, 101, 0, lv("40.44", "7"), lv("40.46", "5"), t0))
	waitFor(t, "the conflict", func() bool { return r.m.Books().ByLeg[Bull].Conflict })
	if b := r.m.Books().ByLeg[Bull]; !r.m.Books().DepthUsable(Bull, t0) || !b.BestBid.Price.Equal(dec("40.44")) {
		t.Errorf("during the conflict: DepthUsable %t, best bid %s; want true and books1's 40.44", r.m.Books().DepthUsable(Bull, t0), b.BestBid.Price)
	}
	bull.onEvent(legFrame("SOXLUSDT", TopicBooks5, ActionSnapshot, 101, 0, lv("40.44", "7"), lv("40.46", "5"), t0))
	waitFor(t, "the end of the conflict", func() bool { return !r.m.Books().ByLeg[Bull].Conflict })
}

// Run starts the four loops, which publish the books, F, the anchors and the
// reference data, and returns when its context ends. Nothing in it sends an
// order.
func TestManagerRunStartsAndStops(t *testing.T) {
	r := anchorRig(t, testConfig(), monClose.Add(2*time.Minute))
	r.fx.set("0.9994", "0.9996", nil)
	r.venue.rules = map[Symbol]MarketRule{"SOXLUSDT": soxlRule, "SOXSUSDT": soxsRule}
	r.venue.settings = goodSettings()
	r.venue.positions = PositionSet{BySymbol: map[Symbol]Position{}}
	r.m.timing.fxPoll, r.m.timing.anchorTick, r.m.timing.referenceTick = 5*time.Millisecond, 5*time.Millisecond, 5*time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.m.Run(ctx); close(done) }()
	now := r.clock.Now()
	waitFor(t, "the first publications", func() bool {
		_, fresh := r.m.FX(now)
		ok, _ := r.m.SettingsOK(10)
		return fresh && r.m.Anchors().Kind == AnchorBell && r.m.TradingReady() && ok && r.m.Positions().Loaded && len(r.venue.subscriptionsOf("SOXSUSDT")) == 1
	})
	if s := r.m.Session(now); s.ID != "2026-12-21" || r.m.Anchors().SessionID != s.ID {
		t.Errorf("session %s, anchors of %s", s.ID, r.m.Anchors().SessionID)
	}
	bull := r.venue.subscriptionsOf("SOXLUSDT")[0]
	bull.onEvent(legFrame("SOXLUSDT", TopicBooks5, ActionSnapshot, 100, 0, lv("40.45", "10"), lv("40.46", "5"), now))
	waitFor(t, "the book", func() bool { return r.m.Books().DepthUsable(Bull, now) })

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after its context ended")
	}
	if bull.ctx.Err() == nil {
		t.Error("the book subscription outlived Run")
	}
	if n := r.venue.callCount("PlaceOrder"); n != 0 {
		t.Errorf("Run sent %d orders", n)
	}
}
