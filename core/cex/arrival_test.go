package cex

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/store"
)

// arrivalDetail is the arrival part of a paper_match row.
type arrivalDetail struct {
	ClientOrderID string `json:"client_order_id"`
	CancelReason  string `json:"cancel_reason"`
	Arrival       struct {
		Source     string      `json:"source"`
		Seq        uint64      `json:"seq"`
		BestSeq    uint64      `json:"best_seq"`
		ReceivedMs int64       `json:"received_ms"`
		Available  [][2]string `json:"available"`
		Remaining  [][2]string `json:"remaining"`
	} `json:"arrival"`
}

// matchDetails returns the arrival detail of every paper_match row so far,
// oldest first.
func (r *paperRig) matchDetails(t *testing.T) []arrivalDetail {
	t.Helper()
	var out []arrivalDetail
	for _, row := range r.sink.Rows("decision") {
		var d arrivalDetail
		if err := json.Unmarshal(row.(store.DecisionRow).Detail, &d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

// bullBest sends the bull leg a books1 frame.
func (r *paperRig) bullBest(seq uint64, bid, ask BookLevel) {
	r.bull.onEvent(BookEvent{Symbol: "SOXLUSDT", Topic: TopicBooks1, Action: ActionSnapshot, Bids: []BookLevel{bid}, Asks: []BookLevel{ask}, Seq: seq, ReceivedAt: r.timer.clock()})
}

// A books1 frame newer than the depth moves the best ask from 40.46 to
// 40.50: the depth behind it is contradicted. A buy limited at 40.46 that
// arrives then meets the best ask alone and takes nothing; a buy limited at
// 40.51 takes the 3 the best ask shows and nothing of the depth's levels.
func TestPaperIOCAtConflictMeetsOnlyTheNewerBest(t *testing.T) {
	r := newPaperRig(t)
	r.bullBook(100, lv("40.46", "5"), lv("40.47", "6"), lv("40.50", "9"))
	r.place(t, orderReq(1, Bull, PurposeEntry, SideBuy, "40.46", "2"))
	r.timer.advance(5 * time.Millisecond)
	r.bullBest(101, lv("40.49", "10"), lv("40.50", "3"))
	r.timer.advance(10 * time.Millisecond)
	if _, orders, _ := r.take(); len(orders) != 1 || !orders[0].CumExecQty.IsZero() || orders[0].Status != StatusCancelled {
		t.Fatalf("best ask 40.50, buy limit 40.46: %+v; want nothing filled", orders)
	}

	r.place(t, orderReq(2, Bull, PurposeEntry, SideBuy, "40.51", "8"))
	r.timer.advance(15 * time.Millisecond)
	fills, orders, _ := r.take()
	if len(fills) != 1 || !fills[0].Price.Equal(dec("40.50")) || !fills[0].Qty.Equal(dec("3")) || orders[0].Status != StatusCancelled {
		t.Fatalf("fills %+v, order %+v; want 3 at 40.50, the best ask only", fills, orders)
	}

	details := r.matchDetails(t)
	if len(details) != 2 {
		t.Fatalf("%d paper_match rows, want 2", len(details))
	}
	for i, d := range details {
		a := d.Arrival
		if a.Source != string(ArrivalBestOnly) || a.Seq != 101 || a.ReceivedMs != t0.Add(5*time.Millisecond).UnixMilli() || !slices.Equal(a.Available, [][2]string{{"40.5", "3"}}) {
			t.Errorf("match %d arrival = %+v; want the best ask of books1 seq 101 alone", i+1, a)
		}
	}
	if a := details[1].Arrival; len(a.Remaining) != 0 {
		t.Errorf("the second match: remaining %v; want nothing left", a.Remaining)
	}
}

// Two reduce-only buys 200 ms apart meet the same unchanged book: the
// first takes the 5 at 40.46 and the second finds them gone. A new depth
// that shows the same 5 changes nothing; one that shows 7 leaves 2.
func TestPaperOrdersDoNotTakeTheSameLotsTwice(t *testing.T) {
	r := newPaperRig(t)
	r.bullBook(100, lv("40.46", "5"))
	r.p.SeedPositions([2]decimal.Decimal{dec("-20"), dec("0")})
	buy := func(seq int) {
		req := orderReq(seq, Bull, PurposeExit, SideBuy, "40.46", "5")
		req.ReduceOnly = true
		r.place(t, req)
		r.timer.advance(200 * time.Millisecond)
	}
	filled := func() decimal.Decimal {
		fills, _, _ := r.take()
		total := decimal.Zero
		for _, f := range fills {
			total = total.Add(f.Qty)
		}
		return total
	}
	buy(1)
	buy(2)
	if got := filled(); !got.Equal(dec("5")) {
		t.Fatalf("two buys on one unchanged ask of 5 filled %s, want 5", got)
	}
	r.bullBook(101, lv("40.46", "5"))
	buy(3)
	if got := filled(); !got.IsZero() {
		t.Fatalf("a new depth showing the same 5: filled %s, want 0", got)
	}
	r.bullBook(102, lv("40.46", "7"))
	buy(4)
	if got := filled(); !got.Equal(dec("2")) {
		t.Fatalf("a new depth showing 7 at the price where 5 were taken: filled %s, want 2", got)
	}
	if d := r.matchDetails(t); d[1].Arrival.Source != string(ArrivalDepth) || d[1].Arrival.Seq != 100 || len(d[1].Arrival.Available) != 0 {
		t.Errorf("the second match's arrival = %+v; want the depth of seq 100 with nothing available", d[1].Arrival)
	}
}

// Two orders that arrive together share a level: the first takes 3 of the
// 5 at 40.46, the second the other 2 and then the next level.
func TestPaperOrdersShareALevel(t *testing.T) {
	r := newPaperRig(t)
	r.bullBook(100, lv("40.46", "5"), lv("40.47", "6"))
	r.place(t, orderReq(1, Bull, PurposeEntry, SideBuy, "40.46", "3"))
	r.place(t, orderReq(2, Bull, PurposeEntry, SideBuy, "40.47", "5"))
	r.timer.advance(15 * time.Millisecond)
	fills, orders, _ := r.take()
	if len(orders) != 2 || !orders[0].CumExecQty.Equal(dec("3")) || !orders[1].CumExecQty.Equal(dec("5")) {
		t.Fatalf("orders %+v", orders)
	}
	// 2 x 40.46 + 3 x 40.47 = 80.92 + 121.41.
	if o := orders[1]; !o.CumExecValue.Equal(dec("202.33")) || len(fills) != 3 || !fills[1].Price.Equal(dec("40.46")) || !fills[1].Qty.Equal(dec("2")) {
		t.Errorf("the second order = %+v, fills %+v; want 2 at 40.46 and 3 at 40.47", o, fills)
	}
	d := r.matchDetails(t)[1].Arrival
	if !slices.Equal(d.Available, [][2]string{{"40.46", "2"}, {"40.47", "6"}}) || !slices.Equal(d.Remaining, [][2]string{{"40.47", "3"}}) {
		t.Errorf("the second arrival = %+v", d)
	}
	// The other side of the book is not touched: a sell takes the bids in full.
	r.place(t, orderReq(3, Bull, PurposeEntry, SideSell, "40.45", "10"))
	r.timer.advance(15 * time.Millisecond)
	if _, orders, _ := r.take(); len(orders) != 1 || orders[0].Status != StatusFilled {
		t.Errorf("a sell on the untouched bids: %+v", orders)
	}
}

// A connected leg with books1 and no depth: an order takes the best level
// alone. Once the leg's last connection is lost its book is not observable:
// the order is cancelled with nothing and a reason of its own, although the
// last best price is still held.
func TestPaperArrivalBestOnlyAndUnobservable(t *testing.T) {
	r := newPaperRig(t)
	r.bullBest(100, lv("40.45", "10"), lv("40.46", "4"))
	r.place(t, orderReq(1, Bull, PurposeEntry, SideBuy, "40.47", "8"))
	r.timer.advance(15 * time.Millisecond)
	if fills, _, _ := r.take(); len(fills) != 1 || !fills[0].Qty.Equal(dec("4")) || !fills[0].Price.Equal(dec("40.46")) {
		t.Fatalf("books1 without a depth: fills %+v; want 4 at 40.46", fills)
	}
	if d := r.matchDetails(t)[0].Arrival; d.Source != string(ArrivalBestOnly) || d.Seq != 100 || d.ReceivedMs != t0.UnixMilli() {
		t.Errorf("the arrival = %+v; want the best only, seq 100", d)
	}

	r.bull.onDown(errors.New("connection lost"))
	r.place(t, orderReq(2, Bull, PurposeEntry, SideBuy, "40.47", "8"))
	r.timer.advance(15 * time.Millisecond)
	if _, orders, _ := r.take(); len(orders) != 1 || orders[0].Status != StatusCancelled || !orders[0].CumExecQty.IsZero() || orders[0].CancelReason != CancelReasonUnobservable {
		t.Fatalf("an order on a leg without a connection: %+v; want cancelled, nothing filled, reason %s", orders, CancelReasonUnobservable)
	}
	if d := r.matchDetails(t)[1]; d.Arrival.Source != string(ArrivalUnobservable) || len(d.Arrival.Available) != 0 || d.CancelReason != CancelReasonUnobservable {
		t.Errorf("the arrival without a connection = %+v", d)
	}
}

// MatchIOC on its own, as the replay calls it. A best-only book at 40.50
// shows 40.46 empty (better than its level, not listed): the take there is
// forgotten and the 5 shown later can be taken again. 40.52 lies beyond
// that book's level and is not observed: its take stands.
func TestMatchIOCForgetsATakeTheBookContradicts(t *testing.T) {
	var taken TakenLevels
	depth := func(seq uint64, levels ...BookLevel) ArrivalBook {
		return ArrivalBook{Source: ArrivalDepth, Seq: seq, ReceivedAt: t0, Levels: levels}
	}
	sum := func(fills []BookLevel) decimal.Decimal {
		total := decimal.Zero
		for _, f := range fills {
			total = total.Add(f.Qty)
		}
		return total
	}
	if fills, _, _ := MatchIOC(depth(1, lv("40.46", "5"), lv("40.52", "4")), SideBuy, dec("40.52"), dec("9"), &taken); !sum(fills).Equal(dec("9")) {
		t.Fatalf("the first take: %+v, want 9", fills)
	}
	// A best-only book at 40.50: 40.46 is better than its level and not
	// listed, so it is empty; 40.52 lies beyond and is not observed.
	if fills, _, _ := MatchIOC(ArrivalBook{Source: ArrivalBestOnly, Seq: 2, Levels: []BookLevel{lv("40.50", "3")}}, SideBuy, dec("40.45"), dec("1"), &taken); len(fills) != 0 {
		t.Fatalf("a buy below the best ask took %+v", fills)
	}
	fills, available, _ := MatchIOC(depth(3, lv("40.46", "5"), lv("40.52", "4")), SideBuy, dec("40.52"), dec("9"), &taken)
	if !sum(fills).Equal(dec("5")) || len(fills) != 1 || !fills[0].Price.Equal(dec("40.46")) {
		t.Errorf("after the book showed 40.46 empty: fills %+v, want the 5 at 40.46 again and nothing at 40.52", fills)
	}
	if len(available) != 1 || !available[0].Price.Equal(dec("40.46")) {
		t.Errorf("available = %+v, want 40.46 alone", available)
	}
}

// The quantity at a taken price moves: books1 shows 6 where 5 were taken.
// The second buy gets the 1 the take leaves, not 5 again; the record names
// the depth's seq and books1's.
func TestPaperTakeStandsWhenTheQuantityMoves(t *testing.T) {
	r := newPaperRig(t)
	r.bullBook(100, lv("40.46", "5"))
	r.p.SeedPositions([2]decimal.Decimal{dec("-20"), dec("0")})
	for i, books1 := range []bool{false, true} {
		if books1 {
			r.bullBest(101, lv("40.45", "3"), lv("40.46", "6"))
		}
		req := orderReq(i+1, Bull, PurposeExit, SideBuy, "40.46", "5")
		req.ReduceOnly = true
		r.place(t, req)
		r.timer.advance(20 * time.Millisecond)
	}
	fills, _, _ := r.take()
	if len(fills) != 2 || !fills[0].Qty.Equal(dec("5")) || !fills[1].Qty.Equal(dec("1")) {
		t.Fatalf("fills %+v; want 5, then the 1 left of the 6 shown", fills)
	}
	if a := r.matchDetails(t)[1].Arrival; a.Source != string(ArrivalDepth) || a.Seq != 100 || a.BestSeq != 101 {
		t.Errorf("the second arrival = %+v; want the depth of seq 100 with books1 seq 101", a)
	}
}

// books5 stalls while books1 keeps the same prices: no conflict, but the
// depth arrived more than depth_max_age before the best price in hand. A
// buy through the best takes the best level alone, not the stale levels
// behind it.
func TestPaperStaleDepthUnderAFreshBestGivesTheBestOnly(t *testing.T) {
	r := newPaperRig(t)
	r.p.SeedPositions([2]decimal.Decimal{dec("-20"), dec("0")})
	r.bullBook(100, lv("40.46", "5"), lv("40.47", "6"))
	r.timer.advance(10*time.Second + time.Millisecond)
	r.bullBest(101, lv("40.45", "10"), lv("40.46", "4"))
	r.place(t, orderReq(1, Bull, PurposeExit, SideBuy, "40.47", "8"))
	r.timer.advance(15 * time.Millisecond)
	if fills, _, _ := r.take(); len(fills) != 1 || !fills[0].Price.Equal(dec("40.46")) || !fills[0].Qty.Equal(dec("4")) {
		t.Fatalf("fills %+v; want 4 at 40.46 only", fills)
	}
	if a := r.matchDetails(t)[0].Arrival; a.Source != string(ArrivalBestOnly) || a.Seq != 101 {
		t.Errorf("the arrival = %+v; want the best only, seq 101", a)
	}
}

// Sells share the bids: two sells of 6 on a bid of 10 get 6 and 4.
func TestPaperSellsShareTheBids(t *testing.T) {
	r := newPaperRig(t)
	r.bullBook(100, lv("40.46", "5"))
	r.place(t, orderReq(1, Bull, PurposeEntry, SideSell, "40.45", "6"))
	r.place(t, orderReq(2, Bull, PurposeEntry, SideSell, "40.45", "6"))
	r.timer.advance(15 * time.Millisecond)
	if _, orders, _ := r.take(); len(orders) != 2 || !orders[0].CumExecQty.Equal(dec("6")) || !orders[1].CumExecQty.Equal(dec("4")) {
		t.Fatalf("orders %+v; want 6 and 4 of the bid of 10", orders)
	}
}

// What one leg's orders took does not touch the other leg's book, even at
// the same price.
func TestPaperLegsTakeIndependently(t *testing.T) {
	r := newPaperRig(t)
	r.bullBook(100, lv("40.46", "5"))
	r.bear.onEvent(BookEvent{Symbol: "SOXSUSDT", Topic: TopicBooks5, Action: ActionSnapshot, Bids: []BookLevel{lv("40.45", "10")}, Asks: []BookLevel{lv("40.46", "5")}, Seq: 200, ReceivedAt: t0})
	r.place(t, orderReq(1, Bull, PurposeEntry, SideBuy, "40.46", "5"))
	r.place(t, orderReq(2, Bear, PurposeEntry, SideBuy, "40.46", "5"))
	r.timer.advance(40 * time.Millisecond)
	if _, orders, _ := r.take(); len(orders) != 2 || orders[0].Status != StatusFilled || orders[1].Status != StatusFilled {
		t.Fatalf("orders %+v; want both legs filled 5", orders)
	}
}

// A conflict ends when a depth at least as new as the disagreeing books1
// arrives: orders then take the raw levels again.
func TestPaperConflictClearsBackToTheRawLevels(t *testing.T) {
	r := newPaperRig(t)
	r.bullBook(100, lv("40.46", "5"), lv("40.47", "6"))
	r.bullBest(101, lv("40.49", "10"), lv("40.50", "3"))
	r.bullBook(102, lv("40.50", "3"), lv("40.51", "4"))
	r.place(t, orderReq(1, Bull, PurposeEntry, SideBuy, "40.51", "7"))
	r.timer.advance(15 * time.Millisecond)
	if _, orders, _ := r.take(); len(orders) != 1 || orders[0].Status != StatusFilled || !orders[0].CumExecValue.Equal(dec("283.54")) { // 3 x 40.50 + 4 x 40.51
		t.Fatalf("orders %+v; want 7 filled on the levels of seq 102", orders)
	}
	if a := r.matchDetails(t)[0].Arrival; a.Source != string(ArrivalDepth) || a.Seq != 102 {
		t.Errorf("the arrival = %+v; want the depth of seq 102", a)
	}
}

// A take beyond the visible levels is not observed, until a newer depth
// arrives: the asks fall to 40.10-40.14 and the takes at 40.46 and 40.47
// are dropped, so when the price comes back the same 5 and 5 can be taken.
func TestMatchIOCDropsATakeBeyondTheWindowOnANewerDepth(t *testing.T) {
	var taken TakenLevels
	depth := func(seq uint64, levels ...BookLevel) ArrivalBook {
		return ArrivalBook{Source: ArrivalDepth, Seq: seq, Levels: levels}
	}
	MatchIOC(depth(1, lv("40.46", "5"), lv("40.47", "5")), SideBuy, dec("40.47"), dec("10"), &taken)
	MatchIOC(depth(2, lv("40.10", "5"), lv("40.11", "5"), lv("40.12", "5"), lv("40.13", "5"), lv("40.14", "5")), SideBuy, dec("40.00"), dec("1"), &taken)
	fills, _, _ := MatchIOC(depth(3, lv("40.46", "5"), lv("40.47", "5")), SideBuy, dec("40.47"), dec("10"), &taken)
	total := decimal.Zero
	for _, f := range fills {
		total = total.Add(f.Qty)
	}
	if !total.Equal(dec("10")) {
		t.Fatalf("after the price came back: fills %+v, want 10", fills)
	}
}
