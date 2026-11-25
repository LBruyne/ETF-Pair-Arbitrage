package strategy

import (
	"math"

	"etf-pair-arbitrage/core/cex"
)

// Spread is D = 10000 x (F x (P_bull / A_bull + P_bear / A_bear) - 2): the
// sum of the two legs' deviations from their anchors, in spread units. f is
// USD per USDT, the prices are in USDT and the anchors in USD per share. One
// unit of D is 0.5 bps of the two legs' combined notional.
func Spread(f, pBull, aBull, pBear, aBear float64) float64 {
	return 10000 * (f*(pBull/aBull+pBear/aBear) - 2)
}

// levelSpread is the spread at the prices of the two legs' pricing levels
// under the anchors a.
func levelSpread(levels [2]cex.BookLevel, f float64, a cex.Anchors) float64 {
	return Spread(f, levels[cex.Bull].Price.InexactFloat64(), a.Bull.InexactFloat64(), levels[cex.Bear].Price.InexactFloat64(), a.Bear.InexactFloat64())
}

// Deviation is the executable deviation x of an order pair in direction dir
// (PRD 4.7), as the entry and exit decisions compute it: the spread at both
// legs' pricing levels on the side dir takes, under the anchors a, minus the
// mean of stats under a. It is for the record of a moment; ok is false when
// a leg has no pricing level. x is not finite when the anchors, F or stats
// give no number.
func (s *Strategy) Deviation(books [2]*cex.Book, dir Direction, f float64, a cex.Anchors, stats Stats) (x float64, ok bool) {
	levels, ok := s.pricingLevels(books, dir)
	if !ok {
		return math.NaN(), false
	}
	return levelSpread(levels, f, a) - stats.Mu(a), true
}

// finite reports whether every value is a finite number. A decision compares
// its numbers as "the check fails when ...": with NaN every such comparison
// is false, so a number that is not finite must stop the decision before
// any comparison.
func finite(values ...float64) bool {
	for _, v := range values {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}
