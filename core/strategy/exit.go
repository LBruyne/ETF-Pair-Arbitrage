package strategy

import (
	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
)

// EvaluateExit decides whether the open cycle exits at the line now. It
// returns one pair of orders for all positions together (one order when the
// other leg has nothing the venue would take), or nil and the reason. It is
// DecideExit without the half target it may set.
//
// The exit line sits on the entry's axis: a short opened at +entry_in x
// sigma closes when x <= entry_out x sigma, a long opened at -entry_in x
// sigma closes when x >= -entry_out x sigma, so a negative entry_out is past
// the mean. x is taken at the two legs' pricing levels on the side the exit takes (pricingLevel).
// There is one line for all positions, from the current mean and sigma, and
// no fee or entry price in the test: it exits at a loss too.
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
	d := s.DecideExit(in)
	return d.Plan, d.Reason
}

// DecideExit is the exit decision at the line (EvaluateExit) with the two
// exit lines and the thin leg first (SPEC 12.6, 12.7, PRD 9.11-9.12).
// Without exit2 and without a thin leg it is PRD 3.10's one line and values
// nothing. With exit2, x past exit2 (in the same frame as exit1 too) closes
// all that is left; x past exit1 first sets the half target H when closing
// the whole round now makes a profit (CloseNet), then closes in batches
// until H is reached, each batch pre-checked; it never closes a second half,
// and a batch built after H was set voids it. With a thin leg first every
// line batch is pre-checked; at exit2 or the one line a failing pre-check
// exits all the same (D1) and the plan carries the thick leg's protection.
//
// Preconditions as EvaluateExit's; in.Value is set when exit2 is set or the
// thin leg goes first.
func (s *Strategy) DecideExit(in ExitInput) ExitDecision {
	if !in.DepthOK[cex.Bull] || !in.DepthOK[cex.Bear] {
		return ExitDecision{Reason: ReasonDepth}
	}
	if !in.Anchors.Bull.IsPositive() || !in.Anchors.Bear.IsPositive() {
		return ExitDecision{Reason: ReasonNoAnchor}
	}
	if !finite(in.F) || in.F <= 0 {
		return ExitDecision{Reason: ReasonFXStale}
	}
	if !in.Stats.Ready { // there has never been a ready mean to exit against
		return ExitDecision{Reason: ReasonWarmup}
	}
	mu, sigma := in.Stats.Mu(in.Anchors), in.Stats.Sigma(in.Anchors)
	if !finite(mu, sigma) {
		return ExitDecision{Reason: ReasonWarmup}
	}

	closing := in.Direction.Opposite() // a short cycle buys back: it takes the asks
	levels, ok := s.pricingLevels(in.Books, closing)
	if !ok {
		return ExitDecision{Reason: ReasonNoLevels}
	}
	price := [2]decimal.Decimal{levels[cex.Bull].Price, levels[cex.Bear].Price}
	x := levelSpread(levels, in.F, in.Anchors) - mu
	line := s.exitLine(in.Direction, x, sigma)
	if line == "" {
		return ExitDecision{Reason: ReasonNoSignal}
	}

	// exit1: the half target, set once while the round is in profit, and
	// what is left of it is this batch's most.
	sized, stage := in, (*ExitStage)(nil)
	if line == ExitLine1 {
		h := in.Stage
		if !h.Set || h.BuiltLots != in.BuiltLots {
			net, err := in.Value(cex.PurposeExit, closeAt(in.Hold, price), s.feeRate())
			if err != nil {
				return ExitDecision{Reason: ReasonValuation}
			}
			if !net.IsPositive() {
				return ExitDecision{Reason: ReasonNoProfit}
			}
			h = s.halfStage(in)
			stage = &h
		}
		if base := baseLeg(in.Pairing); !h.Closed[base].LessThan(h.Target[base]) {
			return ExitDecision{Reason: ReasonHalfDone, Stage: stage}
		}
		for l := range sized.Hold {
			sized.Hold[l] = decimal.Min(in.Hold[l], decimal.Max(decimal.Zero, h.Target[l].Sub(h.Closed[l])))
		}
	}

	qty, ok := s.exitSize(levels, price, sized)
	if !ok { // neither leg has an order the venue would take
		return ExitDecision{Reason: ReasonTooSmall, Stage: stage}
	}
	plan := &ExitPlan{Side: closing.Side(), Price: price, Qty: qty, X: x, Mu: mu, Sigma: sigma, Line: line, D: x + mu}

	// The batch's pre-check (SPEC 12.7 step 0): at exit1, and for every
	// line batch with the thin leg first.
	if line == ExitLine1 || in.Pairing.ThinFirst {
		net, pass, err := s.ExitPrecheck(in, qty, price)
		if err != nil {
			return ExitDecision{Reason: ReasonValuation, Stage: stage}
		}
		plan.ExpectedNet = net
		if !pass {
			if line == ExitLine1 {
				return ExitDecision{Reason: ReasonPrecheck, Stage: stage}
			}
			plan.D1, plan.Protect = true, price[in.Pairing.thick()]
		}
	}
	return ExitDecision{Plan: plan, Stage: stage}
}

// exitLine is the line x has reached for a cycle in direction dir: exit2
// before exit1 when exit2 is set (one frame past both is exit2), else the
// one line; "" when x is short of every line.
func (s *Strategy) exitLine(dir Direction, x, sigma float64) ExitLine {
	past := func(line float64) bool {
		if dir == Short {
			return x <= line*sigma
		}
		return x >= -line*sigma
	}
	switch {
	case s.cfg.Exit2 == nil && past(s.cfg.EntryOut):
		return ExitLineOne
	case s.cfg.Exit2 == nil:
		return ""
	case past(*s.cfg.Exit2):
		return ExitLine2
	case past(s.cfg.EntryOut):
		return ExitLine1
	}
	return ""
}

// halfStage is the half target H (SPEC 12.12 item 6): half the base leg's
// holding floored to the step, the other leg by the pairing anchors, floored
// and within its holding.
func (s *Strategy) halfStage(in ExitInput) ExitStage {
	base := baseLeg(in.Pairing)
	other := otherLeg(base)
	var h ExitStage
	h.Set, h.BuiltLots = true, in.BuiltLots
	h.Closed = [2]decimal.Decimal{decimal.Zero, decimal.Zero}
	h.Target[base] = floorToStep(in.Hold[base].Div(decimal.NewFromInt(2)), in.Rules[base].QtyStep)
	paired := h.Target[base].Mul(anchorOf(in.PairAnchors, base)).Div(anchorOf(in.PairAnchors, other))
	h.Target[other] = decimal.Min(in.Hold[other], floorToStep(paired, in.Rules[other].QtyStep))
	return h
}

// baseLeg is the leg the half target and the margin are counted on: the
// thin leg, or SOXS when both legs go together.
func baseLeg(p Pairing) cex.Leg {
	if p.ThinFirst {
		return p.Thin
	}
	return cex.Bear
}

// closeAt is a close of qty at price on both legs.
func closeAt(qty, price [2]decimal.Decimal) [2]CloseLeg {
	return [2]CloseLeg{{Qty: qty[cex.Bull], Price: price[cex.Bull]}, {Qty: qty[cex.Bear], Price: price[cex.Bear]}}
}

// exitSize sizes one pair of exit orders. When take_ratio of the quantity
// the pricing levels carry covers what is left on both legs, both legs close
// all of it. Otherwise the batch is sized like an entry: the smaller anchored
// amount of the two legs' books, not above what is held. A leg whose order
// the venue would not take (below its minimum quantity or minimum value)
// gets none: its quantity is zero. ok is false when neither leg has an
// order. A remainder too small to send is left where it is: it is dust when
// the cycle closes (PRD 8.1).
func (s *Strategy) exitSize(levels [2]cex.BookLevel, price [2]decimal.Decimal, in ExitInput) (qty [2]decimal.Decimal, ok bool) {
	anchor := [2]decimal.Decimal{in.PairAnchors.Bull, in.PairAnchors.Bear}
	var offered [2]decimal.Decimal // quantity each leg's book offers
	for l := range offered {
		offered[l] = levels[l].Qty.Mul(s.cfg.TakeRatio)
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
