package cex

import (
	"time"

	"github.com/shopspring/decimal"
)

// Books is both legs' published books as one read: the two books come from
// a single load of the published view, and every question about them is
// answered on these same books. A reader takes one Books per decision and
// derives everything from it - freshness, conflict, levels, prices - so a
// frame published meanwhile cannot make a check pass on one version of a
// book and the computation use another.
type Books struct {
	ByLeg  [2]*Book      // by leg, never nil; immutable
	MaxAge time.Duration // depth_max_age: how old a depth, or a best price, may be
}

// DepthUsable reports whether the leg's depth is current: its connection is
// up, it has a depth, and the last depth frame arrived within MaxAge of now.
// A conflict is not part of it; DepthOK includes it.
func (b Books) DepthUsable(leg Leg, now time.Time) bool {
	book := b.ByLeg[leg]
	return book.Connected && !book.DepthReceivedAt.IsZero() && now.Sub(book.DepthReceivedAt) <= b.MaxAge
}

// DepthOK reports whether the leg's depth may be used for an entry, an exit
// at the line or a completion: it is usable and not in conflict with a newer
// best price.
func (b Books) DepthOK(leg Leg, now time.Time) bool {
	return b.DepthUsable(leg, now) && !b.ByLeg[leg].Conflict
}

// HasPrice reports whether the leg has a best bid and ask, from either
// stream, that arrived within MaxAge of now: what a forced exit or a trim
// needs.
func (b Books) HasPrice(leg Leg, now time.Time) bool {
	book := b.ByLeg[leg]
	return !book.BestReceivedAt.IsZero() && now.Sub(book.BestReceivedAt) <= b.MaxAge
}

// legBook is one leg's working book: its depth (books5 or books50) and its
// best bid and ask (books1), merged by the matching sequence number the two
// topics share. It is owned by one goroutine; publish builds the immutable
// Book the readers see.
type legBook struct {
	depth       *depthState // nil until the first snapshot, after a broken update chain and after a disconnect
	best        *bestState  // the newest books1 frame, or the first level of a newer depth that was dropped; nil until the first
	conflictSeq uint64      // seq of the newest books1 frame that disagreed with the depth; 0 when there is no conflict
}

// depthState is the depth held: its levels, best first, as of seq.
type depthState struct {
	bids, asks []BookLevel
	seq        uint64
	receivedAt time.Time // local arrival of the frame that brought seq
}

// bestState is the best bid and ask of one books1 frame.
type bestState struct {
	bid, ask   BookLevel
	seq        uint64
	receivedAt time.Time
}

// applyDepth takes a depth frame. Frames are ordered by seq only, never by
// arrival time. It reports true when an update did not follow the depth
// held: the depth is dropped and the caller subscribes the leg again, once,
// because every later update is ignored until the next snapshot.
func (b *legBook) applyDepth(ev BookEvent) (resubscribe bool) {
	switch {
	case b.depth != nil && ev.Seq <= b.depth.seq:
		return false // not newer than the depth held: dropped
	case ev.Action == ActionSnapshot:
		b.depth = newDepth(ev) // the whole book is replaced
	case b.depth == nil:
		return false // an update while waiting for a snapshot: ignored
	case ev.PrevSeq == b.depth.seq:
		b.depth.merge(ev) // an update that chains
	default:
		b.dropDepth() // an update that does not chain
		return true
	}
	b.recheck()
	return false
}

// applyBest takes a books1 frame; one that is not newer than the frame held
// is dropped. A frame without both a bid and an ask carries no best price
// and is dropped too.
func (b *legBook) applyBest(ev BookEvent) {
	if len(ev.Bids) == 0 || len(ev.Asks) == 0 {
		return
	}
	if b.best == nil || ev.Seq > b.best.seq {
		b.best = &bestState{bid: ev.Bids[0], ask: ev.Asks[0], seq: ev.Seq, receivedAt: ev.ReceivedAt}
		b.recheck()
	}
}

// dropDepth forgets the depth: after a lost connection, because a depth is
// only as good as the connection that keeps it current, and after an update
// that does not chain. The leg keeps a price all the same: when the depth
// was newer than books1 its first level becomes the best price, with the
// depth's sequence number and arrival time, so that the best bid and ask
// last received from either stream stay published until they age out. A
// conflict stays until a depth at least as new as the disagreeing books1
// frame arrives.
func (b *legBook) dropDepth() {
	if d := b.depth; d != nil && len(d.bids) > 0 && len(d.asks) > 0 && (b.best == nil || d.seq > b.best.seq) {
		b.best = &bestState{bid: d.bids[0], ask: d.asks[0], seq: d.seq, receivedAt: d.receivedAt}
	}
	b.depth = nil
}

// recheck runs after every accepted frame. A depth at least as new as the
// disagreeing books1 frame ends a conflict; a books1 frame newer than the
// depth with a different best price starts one. The conflict remembers that
// frame's seq, so books1 returning to the depth's prices does not end it:
// the levels behind the best price may have traded meanwhile.
func (b *legBook) recheck() {
	if b.depth == nil {
		return
	}
	if b.depth.seq >= b.conflictSeq {
		b.conflictSeq = 0
	}
	if b.best != nil && b.best.seq > b.depth.seq && !b.depth.sameBestPrices(b.best) {
		b.conflictSeq = max(b.conflictSeq, b.best.seq)
	}
}

// publish builds the immutable Book of the leg as it is now. The best bid
// and ask come from the newer of the depth and books1 by seq, also during a
// conflict. books1's quantities replace the depth's first level only when
// there is no conflict, that is when its prices are the depth's.
func (b *legBook) publish(symbol Symbol, connected bool) *Book {
	out := &Book{Symbol: symbol, Connected: connected, Conflict: b.conflictSeq != 0}
	if b.depth != nil {
		out.Bids, out.Asks = b.depth.copyLevels()
		out.DepthSeq, out.DepthReceivedAt = b.depth.seq, b.depth.receivedAt
		if len(out.Bids) > 0 && len(out.Asks) > 0 {
			out.BestBid, out.BestAsk, out.BestSeq, out.BestReceivedAt = out.Bids[0], out.Asks[0], b.depth.seq, b.depth.receivedAt
		}
	}
	if b.best != nil && (b.depth == nil || b.best.seq > b.depth.seq) {
		out.BestBid, out.BestAsk, out.BestSeq, out.BestReceivedAt = b.best.bid, b.best.ask, b.best.seq, b.best.receivedAt
		if b.depth != nil && !out.Conflict {
			out.Bids[0].Qty, out.Asks[0].Qty = b.best.bid.Qty, b.best.ask.Qty // same prices, newer quantities
		}
	}
	return out
}

// newDepth is the depth of a snapshot frame.
func newDepth(ev BookEvent) *depthState {
	return &depthState{bids: append([]BookLevel(nil), ev.Bids...), asks: append([]BookLevel(nil), ev.Asks...), seq: ev.Seq, receivedAt: ev.ReceivedAt}
}

// merge applies an update frame: each level sets its price's quantity, and a
// quantity of zero deletes the price.
func (d *depthState) merge(ev BookEvent) {
	d.bids = mergeSide(d.bids, ev.Bids, func(a, b decimal.Decimal) bool { return a.GreaterThan(b) })
	d.asks = mergeSide(d.asks, ev.Asks, func(a, b decimal.Decimal) bool { return a.LessThan(b) })
	d.seq, d.receivedAt = ev.Seq, ev.ReceivedAt
}

// mergeSide applies changes to one side kept best first. better reports
// whether the first price is nearer the top of that side than the second.
func mergeSide(levels, changes []BookLevel, better func(a, b decimal.Decimal) bool) []BookLevel {
	for _, c := range changes {
		// The place of the price: the first level that is not better than it.
		i := 0
		for i < len(levels) && better(levels[i].Price, c.Price) {
			i++
		}
		present := i < len(levels) && levels[i].Price.Equal(c.Price)
		switch {
		case c.Qty.IsZero() && present:
			levels = append(levels[:i], levels[i+1:]...)
		case c.Qty.IsZero():
			// deleting a price that is not there changes nothing
		case present:
			levels[i].Qty = c.Qty
		default:
			levels = append(levels, BookLevel{})
			copy(levels[i+1:], levels[i:])
			levels[i] = c
		}
	}
	return levels
}

// sameBestPrices reports whether the depth's best bid and ask prices are
// those of a books1 frame. A depth without one side has none to compare and
// does not agree.
func (d *depthState) sameBestPrices(best *bestState) bool {
	return len(d.bids) > 0 && len(d.asks) > 0 && d.bids[0].Price.Equal(best.bid.Price) && d.asks[0].Price.Equal(best.ask.Price)
}

// copyLevels returns the levels in slices of their own, for publication.
func (d *depthState) copyLevels() (bids, asks []BookLevel) {
	return append([]BookLevel(nil), d.bids...), append([]BookLevel(nil), d.asks...)
}
