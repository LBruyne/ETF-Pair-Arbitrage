package strategy

import (
	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
)

// EvaluateEntry decides whether to open an entry batch now. It returns the
// plan, or nil and the first check that fails, in the fixed order of the
// Reason constants: an earlier failing check hides the later ones.
//
// The signal (condition 1) compares the executable deviation x with the
// entry line and has no fee in it: x is the spread at the two legs' pricing
// levels on the side to take (pricingLevel), minus the mean. The fee enters
// only the cost cover (condition 2).
//
// Numbers that cannot be compared never yield a plan, whatever the caller
// gathered in Blocked: an anchor that is not positive is ReasonNoAnchor, an
// F that is not a positive finite number ReasonFXStale (the zero value of a
// rate never read is one), a mean or sigma that is not finite ReasonWarmup.
//
// Preconditions, which the caller guarantees and which are not checked: the
// contract rules are loaded (a positive quantity step), the pairing anchors
// are positive, and the levels' prices are positive.
func (s *Strategy) EvaluateEntry(in EntryInput) (*EntryPlan, Reason) {
	// Steps 1 to 3: the program, the trading hours and anchors, the data.
	if in.Blocked != "" {
		return nil, in.Blocked
	}
	if !in.Anchors.Bull.IsPositive() || !in.Anchors.Bear.IsPositive() {
		return nil, ReasonNoAnchor
	}
	if !finite(in.F) || in.F <= 0 {
		return nil, ReasonFXStale
	}

	// Step 4: the mean is ready and sigma is positive.
	if !in.Stats.Ready {
		return nil, ReasonWarmup
	}
	mu, sigma := in.Stats.Mu(in.Anchors), in.Stats.Sigma(in.Anchors)
	if !finite(mu, sigma) {
		return nil, ReasonWarmup
	}
	if sigma <= 0 {
		return nil, ReasonZeroSigma
	}

	// Steps 5 and 6 are looked at for both directions. At most one can
	// signal: the asks are above the bids, so the long deviation is above
	// the short one, and the two lines are on opposite sides of the mean.
	aBull, aBear := in.Anchors.Bull.InexactFloat64(), in.Anchors.Bear.InexactFloat64()
	reason := ReasonNoLevels
	for _, dir := range []Direction{Long, Short} {
		// Step 5: a pricing level on the side to take, on both legs.
		levels, ok := s.pricingLevels(in.Books, dir)
		if !ok {
			continue
		}
		price := [2]decimal.Decimal{levels[cex.Bull].Price, levels[cex.Bear].Price}
		pBull, pBear := price[cex.Bull].InexactFloat64(), price[cex.Bear].InexactFloat64()

		// Step 6: condition 1. A long needs x <= -d_in x sigma, a short
		// x >= +d_in x sigma.
		x := Spread(in.F, pBull, aBull, pBear, aBear) - mu
		if !s.AtEntryLine(dir, x, sigma) {
			reason = ReasonNoSignal
			continue
		}
		if in.Holding != "" && in.Holding != dir {
			return nil, ReasonHoldingOtherSide
		}

		// Step 7: condition 2. The distance from the entry line to the exit
		// line pays the round-trip fee at these limit prices plus the margin,
		// all in spread units. The exit is estimated at the same prices.
		feeD := 2 * s.cfg.TakerFeeBps * in.F * (pBull/aBull + pBear/aBear)
		if (s.cfg.DIn-s.cfg.DOut)*sigma < feeD+s.cfg.CostMarginD {
			return nil, ReasonCostNotCovered
		}

		// Step 8: the spacing after the last built entry.
		if !s.spacingOK(in, dir, x) {
			return nil, ReasonSpacing
		}

		// Step 9: the size, which the venue must take on both legs.
		quotaAt := [2]decimal.Decimal{quotaPrice(in.Books[cex.Bull], dir, price[cex.Bull]), quotaPrice(in.Books[cex.Bear], dir, price[cex.Bear])}
		qty, k, quotaBound := s.entrySize(levels, quotaAt, in)
		if !in.Rules[cex.Bull].Sendable(qty[cex.Bull], price[cex.Bull]) || !in.Rules[cex.Bear].Sendable(qty[cex.Bear], price[cex.Bear]) {
			if quotaBound {
				return nil, ReasonQuotaExhausted
			}
			return nil, ReasonTooSmall
		}
		// Both legs together must reach the minimum batch (owner item 59).
		notional := qty[cex.Bull].Mul(price[cex.Bull]).Add(qty[cex.Bear].Mul(price[cex.Bear]))
		if notional.LessThan(s.cfg.MinBatchNotional) {
			if quotaBound {
				return nil, ReasonQuotaExhausted
			}
			return nil, ReasonTooSmall
		}
		return &EntryPlan{
			Direction:  dir,
			Price:      price,
			Qty:        qty,
			QuotaPrice: quotaAt,
			X:          x,
			Mu:         mu,
			Sigma:      sigma,
			FeeD:       feeD,
			FeeUSDT:    s.feeRate().Mul(notional).Mul(decimal.NewFromInt(2)),
			K:          k,
		}, ""
	}
	return nil, reason
}

// AtEntryLine is condition 1 of PRD 5.2: a long at x <= -d_in x sigma, a
// short at x >= +d_in x sigma, x being the executable deviation of that
// direction. No fee is in it.
func (s *Strategy) AtEntryLine(dir Direction, x, sigma float64) bool {
	if dir == Long {
		return x <= -s.cfg.DIn*sigma
	}
	return x >= s.cfg.DIn*sigma
}

// spacingOK reports whether an entry in direction dir at deviation x may
// follow the last built entry. The first entry, an entry in the other
// direction and one after entry_spacing do not wait. A sooner one in the
// same direction must be entry_step_d further from the mean than the last.
// When the anchors changed since the last entry, the two deviations are not
// comparable and the entry waits the whole spacing.
func (s *Strategy) spacingOK(in EntryInput, dir Direction, x float64) bool {
	if in.Last == nil || in.Last.Direction != dir || in.Now.Sub(in.Last.At) >= s.cfg.EntrySpacing {
		return true
	}
	if in.AnchorChangedAt.After(in.Last.At) {
		return false
	}
	if dir == Long {
		return x <= in.Last.E-s.cfg.EntryStepD
	}
	return x >= in.Last.E+s.cfg.EntryStepD
}

// entrySize sizes one batch. Each leg offers take_ratio of the quantity its
// pricing level carries; that is turned into an anchored amount (quantity x
// pairing anchor) and the smaller of the two legs is K, the anchored amount
// of both. K is capped so that the two orders' notional at their quota
// prices (the limits of a buy, the raw best bids of a sell) stays within the
// quota left. Each leg's quantity is K / its pairing
// anchor, rounded down to the quantity step, so the two legs carry about the
// same anchored amount. quotaBound says that the quota, not the book, set K.
func (s *Strategy) entrySize(levels [2]cex.BookLevel, quotaAt [2]decimal.Decimal, in EntryInput) (qty [2]decimal.Decimal, k decimal.Decimal, quotaBound bool) {
	anchor := [2]decimal.Decimal{in.PairAnchors.Bull, in.PairAnchors.Bear}
	var offered [2]decimal.Decimal // anchored amount each leg's book offers, in USD
	for l := range offered {
		offered[l] = levels[l].Qty.Mul(s.cfg.TakeRatio).Mul(anchor[l])
	}
	k = decimal.Min(offered[cex.Bull], offered[cex.Bear])

	// The notional of an anchored amount K at the quota prices Q is
	// K x (Q_bull / A_bull + Q_bear / A_bear).
	perK := quotaAt[cex.Bull].Div(anchor[cex.Bull]).Add(quotaAt[cex.Bear].Div(anchor[cex.Bear]))
	if byQuota := in.QuotaLeft.Div(perK); byQuota.LessThan(k) {
		k, quotaBound = byQuota, true
	}
	for l := range qty {
		qty[l] = floorToStep(k.Div(anchor[l]), in.Rules[l].QtyStep)
	}
	return qty, k, quotaBound
}
