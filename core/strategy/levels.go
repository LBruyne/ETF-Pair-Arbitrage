package strategy

import (
	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
)

// --- effective levels ---

// EffectiveLevels folds raw levels (best first) into price buckets of size
// bucket and keeps the buckets that hold at least minQty. Asks round up to
// their bucket and bids round down, so a bucket's price is never better than
// the levels in it. A bucket below minQty is dust: it is skipped and its
// quantity is not added to the next one.
func EffectiveLevels(raw []cex.BookLevel, asks bool, bucket, minQty decimal.Decimal) []cex.BookLevel {
	var folded []cex.BookLevel
	for _, lv := range raw {
		p := lv.Price.Div(bucket)
		if asks {
			p = p.Ceil()
		} else {
			p = p.Floor()
		}
		p = p.Mul(bucket)
		if n := len(folded); n > 0 && folded[n-1].Price.Equal(p) {
			folded[n-1].Qty = folded[n-1].Qty.Add(lv.Qty)
			continue
		}
		folded = append(folded, cex.BookLevel{Price: p, Qty: lv.Qty})
	}
	out := folded[:0]
	for _, lv := range folded {
		if lv.Qty.GreaterThanOrEqual(minQty) {
			out = append(out, lv)
		}
	}
	return out
}

// takerLevels returns both legs' effective levels on the side a batch in
// direction dir takes: the asks when it buys, the bids when it sells.
func (s *Strategy) takerLevels(books [2]*cex.Book, dir Direction) (levels [2][]cex.BookLevel) {
	for l, b := range books {
		levels[l] = EffectiveLevels(takerSide(b, dir), dir == Long, s.cfg.LevelBucket, s.cfg.LevelMinQty)
	}
	return levels
}

// --- raw book ---

// takerSide returns the raw depth levels an order in direction dir takes,
// best first: the asks for a buy, the bids for a sell.
func takerSide(b *cex.Book, dir Direction) []cex.BookLevel {
	if dir == Long {
		return b.Asks
	}
	return b.Bids
}

// bestPrice is the raw best price an order in direction dir takes: the best
// ask for a buy, the best bid for a sell. It comes from the newer of the
// depth and the best-price stream, so it can differ from the depth's first
// level while the two disagree.
func bestPrice(b *cex.Book, dir Direction) decimal.Decimal {
	if dir == Long {
		return b.BestAsk.Price
	}
	return b.BestBid.Price
}

// quotaPrice is the price an order's notional counts against the quota at
// (PRD 6.4): its limit for a buy, which cannot fill above it; the raw best
// bid of the decision's book for a sell, whose limit is the lowest price it
// takes and which fills at the bids above it. A sell on a book without a
// best bid counts at its limit.
func quotaPrice(b *cex.Book, dir Direction, limit decimal.Decimal) decimal.Decimal {
	if dir == Long {
		return limit
	}
	return decimal.Max(limit, b.BestBid.Price)
}

// walkOrLast takes qty from the raw book level by level, as an order in
// direction dir would, and returns what is taken at each price. When the
// levels do not hold qty, the rest is counted at the last level's price: an
// estimate for a reducing order, which is sent whatever the book shows.
func walkOrLast(b *cex.Book, dir Direction, qty decimal.Decimal) []cex.BookLevel {
	levels := takerSide(b, dir)
	var fills []cex.BookLevel
	left := qty
	for i, lv := range levels {
		if !left.IsPositive() {
			break
		}
		take := decimal.Min(left, lv.Qty)
		if i == len(levels)-1 {
			take = left
		}
		fills = append(fills, cex.BookLevel{Price: lv.Price, Qty: take})
		left = left.Sub(take)
	}
	return fills
}

// capByLimit sizes a completion order of up to qty in direction dir. It
// tries the raw levels from the best one down to the one that fills qty; at
// each, the quantity is what the book holds down to that level, capped by
// qty and by quota / the order's quota price (rounded down to the quantity
// step). The order would be sent with that level's price as its limit, and
// its quota price is that limit for a buy and the raw best bid for a sell
// (quotaPrice). The largest quantity wins, the better price on a tie.
//
// It returns that quantity, its limit price and what is taken at each price
// (for the loss estimate only). The quantity is zero when the levels together
// do not hold qty, or when the quota holds nothing: no completion then.
func capByLimit(b *cex.Book, dir Direction, qty, quota decimal.Decimal, rule cex.MarketRule) (capQty, limit decimal.Decimal, fills []cex.BookLevel) {
	held, filled := decimal.Zero, false
	for _, lv := range takerSide(b, dir) {
		held = held.Add(lv.Qty)
		if q := decimal.Min(held, qty, floorToStep(quota.Div(quotaPrice(b, dir, lv.Price)), rule.QtyStep)); q.GreaterThan(capQty) {
			capQty, limit = q, lv.Price
		}
		if held.GreaterThanOrEqual(qty) {
			filled = true
			break
		}
	}
	if !filled || !capQty.IsPositive() {
		return decimal.Zero, decimal.Zero, nil
	}
	return capQty, limit, walkOrLast(b, dir, capQty)
}

// value is the traded amount of fills: the sum of price x quantity, in USDT.
func value(fills []cex.BookLevel) decimal.Decimal {
	sum := decimal.Zero
	for _, f := range fills {
		sum = sum.Add(f.Price.Mul(f.Qty))
	}
	return sum
}

// adverse is how much worse the amount got is than the reference amount ref
// for an order in direction dir: a buy that pays more, a sell that receives
// less. Negative when got is better.
func adverse(got, ref decimal.Decimal, dir Direction) decimal.Decimal {
	if dir == Long {
		return got.Sub(ref)
	}
	return ref.Sub(got)
}

// --- order limits ---

// floorToStep rounds a quantity down to a multiple of the quantity step.
func floorToStep(qty, step decimal.Decimal) decimal.Decimal {
	return qty.Div(step).Floor().Mul(step)
}

// throughBest is the limit price of a reducing order in direction dir: the
// raw best price given up by slipBps (of that price), rounded to the price
// tick against ourselves. A buy rounds up from ask x (1 + slip), a sell
// rounds down from bid x (1 - slip).
func throughBest(b *cex.Book, dir Direction, slipBps float64, tick decimal.Decimal) decimal.Decimal {
	slip := decimal.NewFromFloat(slipBps).Div(bpsPerUnit)
	one, best := decimal.NewFromInt(1), bestPrice(b, dir)
	if dir == Long {
		return best.Mul(one.Add(slip)).Div(tick).Ceil().Mul(tick)
	}
	return best.Mul(one.Sub(slip)).Div(tick).Floor().Mul(tick)
}
