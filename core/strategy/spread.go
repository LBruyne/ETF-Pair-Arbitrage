package strategy

import "math"

// Spread is D = 10000 x (F x (P_bull / A_bull + P_bear / A_bear) - 2): the
// sum of the two legs' deviations from their anchors, in spread units. f is
// USD per USDT, the prices are in USDT and the anchors in USD per share. One
// unit of D is 0.5 bps of the two legs' combined notional.
func Spread(f, pBull, aBull, pBear, aBear float64) float64 {
	return 10000 * (f*(pBull/aBull+pBear/aBear) - 2)
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
