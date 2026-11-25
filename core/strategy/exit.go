package strategy

import (
	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
)

// EvaluateExit decides whether the open cycle exits at the line now. It
// returns one pair of orders for all positions together (one order when the
// other leg has nothing the venue would take), or nil and the reason.
//
// The exit line sits on the entry's axis: a short opened at +d_in x sigma
// closes when x <= d_out x sigma, a long opened at -d_in x sigma closes when
// x >= -d_out x sigma, so a negative d_out is past the mean. x is taken at
// the second effective level of the side the exit takes. There is one line
// for all positions, from the current mean and sigma, and no fee or entry
// price in the test: it exits at a loss too.
//
// Numbers that cannot be compared never yield a plan: an anchor that is not
// positive is ReasonNoAnchor, an F that is not a positive finite number
// ReasonFXStale (a stale F still exits, with its last valid value; one that
// was never read does not), a mean or sigma that is not finite ReasonWarmup.
// The cycle then leaves by the forced exit, which needs none of them.
//
// Preconditions, which the caller guarantees and which are not checked: the
// contract rules are loaded (a positive quantity step), the pairing anchors
// are positive, and the levels' prices are positive.
func (s *Strategy) EvaluateExit(in ExitInput) (*ExitPlan, Reason) {
	if !in.DepthOK[cex.Bull] || !in.DepthOK[cex.Bear] {
		return nil, ReasonDepth
	}
	if !in.Anchors.Bull.IsPositive() || !in.Anchors.Bear.IsPositive() {
		return nil, ReasonNoAnchor
	}
	if !finite(in.F) || in.F <= 0 {
		return nil, ReasonFXStale
	}
	if !in.Stats.Ready { // there has never been a ready mean to exit against
		return nil, ReasonWarmup
	}
	mu, sigma := in.Stats.Mu(in.Anchors), in.Stats.Sigma(in.Anchors)
	if !finite(mu, sigma) {
		return nil, ReasonWarmup
	}

	closing := in.Direction.Opposite() // a short cycle buys back: it takes the asks
	levels := s.takerLevels(in.Books, closing)
	if len(levels[cex.Bull]) < 2 || len(levels[cex.Bear]) < 2 {
		return nil, ReasonNoLevels
	}
	price := [2]decimal.Decimal{levels[cex.Bull][1].Price, levels[cex.Bear][1].Price}
	x := Spread(in.F, price[cex.Bull].InexactFloat64(), in.Anchors.Bull.InexactFloat64(),
		price[cex.Bear].InexactFloat64(), in.Anchors.Bear.InexactFloat64()) - mu
	if (in.Direction == Short && x > s.cfg.DOut*sigma) || (in.Direction == Long && x < -s.cfg.DOut*sigma) {
		return nil, ReasonNoSignal
	}

	qty, ok := s.exitSize(levels, price, in)
	if !ok { // neither leg has an order the venue would take
		return nil, ReasonTooSmall
	}
	return &ExitPlan{Side: closing.Side(), Price: price, Qty: qty, X: x, Mu: mu, Sigma: sigma}, ""
}

// exitSize sizes one pair of exit orders. When take_ratio of the first two
// effective levels covers what is left on both legs, both legs close all of
// it. Otherwise the batch is sized like an entry: the smaller anchored
// amount of the two legs' books, not above what is held. A leg whose order
// the venue would not take (below its minimum quantity or minimum value)
// gets none: its quantity is zero. ok is false when neither leg has an
// order. A remainder too small to send is left where it is: it is dust when
// the cycle closes (PRD 8.1).
func (s *Strategy) exitSize(levels [2][]cex.BookLevel, price [2]decimal.Decimal, in ExitInput) (qty [2]decimal.Decimal, ok bool) {
	anchor := [2]decimal.Decimal{in.PairAnchors.Bull, in.PairAnchors.Bear}
	var offered [2]decimal.Decimal // quantity each leg's book offers
	for l := range offered {
		offered[l] = levels[l][0].Qty.Add(levels[l][1].Qty).Mul(s.cfg.TakeRatio)
	}
	qty = in.Hold
	if offered[cex.Bull].LessThan(in.Hold[cex.Bull]) || offered[cex.Bear].LessThan(in.Hold[cex.Bear]) {
		// Both legs are sized from one anchored amount k, so the pair stays
		// matched.
		k := decimal.Min(offered[cex.Bull].Mul(anchor[cex.Bull]), offered[cex.Bear].Mul(anchor[cex.Bear]),
			in.Hold[cex.Bull].Mul(anchor[cex.Bull]), in.Hold[cex.Bear].Mul(anchor[cex.Bear]))
		for l := range qty {
			qty[l] = floorToStep(k.Div(anchor[l]), in.Rules[l].QtyStep)
		}
	}
	for l := range qty {
		if in.Rules[l].Sendable(qty[l], price[l]) {
			ok = true
		} else {
			qty[l] = decimal.Zero
		}
	}
	return qty, ok
}

// ForcedOrders closes everything that is left of a cycle in direction dir:
// one reduce-only order per leg for the whole remainder, priced through the
// raw best price by forced_slip_bps. It looks at no line and no level size.
// A leg whose remainder the venue would not take (nothing, or dust) gets
// nil; its book is not read.
//
// Preconditions, which the caller guarantees and which are not checked:
// every leg that holds a quantity has a positive best price on the side the
// order takes, and its contract rule is loaded (a positive price tick).
// Without a price the order would be priced at zero and the leg taken for
// dust.
func (s *Strategy) ForcedOrders(dir Direction, hold [2]decimal.Decimal, books [2]*cex.Book, rules [2]cex.MarketRule) [2]*LegOrder {
	closing := dir.Opposite()
	var orders [2]*LegOrder
	for l := range orders {
		if !hold[l].IsPositive() {
			continue
		}
		price := throughBest(books[l], closing, s.cfg.ForcedSlipBps, rules[l].PriceTick)
		if rules[l].Sendable(hold[l], price) {
			orders[l] = &LegOrder{Side: closing.Side(), Price: price, Qty: hold[l]}
		}
	}
	return orders
}
