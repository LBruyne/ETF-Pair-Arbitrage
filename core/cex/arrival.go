package cex

import (
	"time"

	"github.com/shopspring/decimal"
)

// The arrival rule of a simulated IOC: which part of its leg's book it may
// take when it arrives, and how orders that arrive on the same book share
// its quantity. The paper venue applies it to its own copy of the books;
// the replay applies it to the books it builds from the recorded frames.

// ArrivalSource names the part of a leg's book an IOC was matched on.
type ArrivalSource string

const (
	ArrivalDepth        ArrivalSource = "depth"        // a current depth no newer books1 contradicts: its raw levels
	ArrivalBestOnly     ArrivalSource = "best_only"    // the depth is contradicted, stale or missing: the best level alone
	ArrivalUnobservable ArrivalSource = "unobservable" // no connection up, or no price: the order takes nothing
)

// CancelReasonUnobservable is the cancel reason of a simulated IOC whose
// leg's book could not be observed at its arrival.
const CancelReasonUnobservable = "book_unobservable"

// ArrivalBook is the side of a leg's book an IOC meets when it arrives.
type ArrivalBook struct {
	Source     ArrivalSource
	Seq        uint64      // the frame the levels come from: the depth's, or the best price's for best_only; 0 when unobservable
	ReceivedAt time.Time   // that frame's local arrival; zero when unobservable
	Levels     []BookLevel // the side the order takes, best first; empty when unobservable
}

// ArrivalLevels picks what an IOC of the given side may take from its
// leg's book when it arrives. A connected leg whose depth no newer books1
// contradicts, and whose best price in hand arrived no more than maxAge
// (depth_max_age) after the depth, gives the depth's raw levels. One whose
// depth is contradicted, stale under a newer best price, or missing gives
// its best level alone, with the quantity of the frame it came from: the
// levels behind it are not known. A leg with no connection up, or no
// price, is not observable. The depth's age is measured against the best
// price's arrival, both stamped by the same receiving clock; that the best
// price itself is current is the sender's check (HasPrice or DepthOK),
// made one order latency before the arrival.
func ArrivalLevels(book *Book, side Side, maxAge time.Duration) ArrivalBook {
	levels, best := book.Asks, book.BestAsk
	if side == SideSell {
		levels, best = book.Bids, book.BestBid
	}
	switch {
	case !book.Connected:
	case len(levels) > 0 && !book.Conflict && book.BestReceivedAt.Sub(book.DepthReceivedAt) <= maxAge:
		return ArrivalBook{Source: ArrivalDepth, Seq: book.DepthSeq, ReceivedAt: book.DepthReceivedAt, Levels: levels}
	case best.Qty.IsPositive():
		return ArrivalBook{Source: ArrivalBestOnly, Seq: book.BestSeq, ReceivedAt: book.BestReceivedAt, Levels: []BookLevel{best}}
	}
	return ArrivalBook{Source: ArrivalUnobservable}
}

// LegTaken is what earlier simulated IOCs took from one leg's book: the
// asks by buys, the bids by sells.
type LegTaken struct{ Asks, Bids TakenLevels }

// Of returns what was taken from the side an order of the given side takes.
func (l *LegTaken) Of(side Side) *TakenLevels {
	if side == SideSell {
		return &l.Bids
	}
	return &l.Asks
}

// TakenLevels is what earlier simulated IOCs took from one side of a leg's
// book, by price. A simulated order leaves the real book as it was, so what
// it took is subtracted from whatever the book later shows at that price.
// A take is forgotten when the book shows the price empty: within its
// visible levels (at or better than the last of them) and not listed. A
// price beyond the last visible level is not observed; its take is
// forgotten at the first depth newer than the take.
type TakenLevels struct{ levels []takenLevel }

// takenLevel is what was taken at one price, and the seq of the book it
// was last taken from.
type takenLevel struct {
	price, taken decimal.Decimal
	seq          uint64
}

// MatchIOC matches an IOC on the book it meets at arrival: level by level
// inside the limit until qty is taken, each level net of what earlier
// orders took from it. The takes are added to taken. It returns the fills
// (price and quantity) and, for the record, the levels available to the
// order and those left after it, both net of the takes.
func MatchIOC(book ArrivalBook, side Side, limit, qty decimal.Decimal, taken *TakenLevels) (fills, available, remaining []BookLevel) {
	inside := func(price decimal.Decimal) bool { return price.LessThanOrEqual(limit) }
	if side == SideSell {
		inside = func(price decimal.Decimal) bool { return price.GreaterThanOrEqual(limit) }
	}
	taken.observe(book, side)
	left := qty
	for _, level := range book.Levels {
		free := level.Qty.Sub(taken.at(level.Price))
		if !free.IsPositive() {
			continue
		}
		available = append(available, BookLevel{Price: level.Price, Qty: free})
		take := decimal.Zero
		if left.IsPositive() && inside(level.Price) {
			take = decimal.Min(left, free)
			fills = append(fills, BookLevel{Price: level.Price, Qty: take})
			taken.add(level.Price, take, book.Seq)
			left = left.Sub(take)
		}
		if rest := free.Sub(take); rest.IsPositive() {
			remaining = append(remaining, BookLevel{Price: level.Price, Qty: rest})
		}
	}
	return fills, available, remaining
}

// observe forgets every take the book no longer supports: its price lies
// within the visible levels and is not listed, or lies beyond them and the
// book is a depth newer than the take.
func (t *TakenLevels) observe(book ArrivalBook, side Side) {
	levels := book.Levels
	if len(levels) == 0 {
		return
	}
	last := levels[len(levels)-1].Price
	visible := func(price decimal.Decimal) bool { return price.LessThanOrEqual(last) }
	if side == SideSell {
		visible = func(price decimal.Decimal) bool { return price.GreaterThanOrEqual(last) }
	}
	kept := t.levels[:0]
	for _, tl := range t.levels {
		switch {
		case levelIndex(levels, tl.price) >= 0:
		case visible(tl.price):
			continue
		case book.Source == ArrivalDepth && book.Seq > tl.seq:
			continue
		}
		kept = append(kept, tl)
	}
	t.levels = kept
}

// at is what was taken at the price.
func (t *TakenLevels) at(price decimal.Decimal) decimal.Decimal {
	for _, tl := range t.levels {
		if tl.price.Equal(price) {
			return tl.taken
		}
	}
	return decimal.Zero
}

// add records a take of qty at the price from the book of seq.
func (t *TakenLevels) add(price, qty decimal.Decimal, seq uint64) {
	for i := range t.levels {
		if t.levels[i].price.Equal(price) {
			t.levels[i].taken, t.levels[i].seq = t.levels[i].taken.Add(qty), seq
			return
		}
	}
	t.levels = append(t.levels, takenLevel{price: price, taken: qty, seq: seq})
}

// levelIndex is the index of the level at the price, or -1.
func levelIndex(levels []BookLevel, price decimal.Decimal) int {
	for i, l := range levels {
		if l.Price.Equal(price) {
			return i
		}
	}
	return -1
}
