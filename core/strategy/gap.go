package strategy

import (
	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
)

// EvaluateGap decides what to do when the cycle's two legs do not carry the
// same anchored amount (quantity x pairing anchor): complete the leg that
// has less, trim the leg that has more, wait, or nothing. It decides one
// order at a time; the caller evaluates again on that order's result.
//
// A trim reduces the larger leg through its raw best price by
// forced_slip_bps and needs only a best price. Its loss is estimated on the
// book view that limit comes from: level by level on the depth while the
// depth is usable and agrees with the best price (DepthOK), and otherwise
// the whole quantity at the best price, which is then the newer fact. A
// completion adds to the
// smaller leg and is opening risk, so it is considered only for a gap that
// came from an entry batch, inside completion_window after the batch was
// sent, while CanComplete allows it on that leg, and on a book frame newer
// than the batch's results. Its limit is the raw level that fills the
// missing quantity and its size is capped by the quota left, counted at the
// order's quota price; what the quota does not hold is trimmed after the
// completion's result.
//
// When both orders can be sent, the one with the smaller expected loss on
// the raw books wins, and a tie goes to the trim. The trim is sized within
// the venue's net position (Reducible with the ledger's dust): what it
// cannot close of the gap is dust.
//
// Preconditions, which the caller guarantees and which are not checked: the
// contract rules are loaded (a positive quantity step and price tick), the
// pairing anchors are positive, and a leg with HasPrice has a positive best
// price. A best price of zero would price the trim at zero and take the gap
// for one no order can be sent for.
func (s *Strategy) EvaluateGap(in GapInput) GapAction {
	// The gap, and what it is worth on each leg. lead holds more anchored
	// amount, lag less.
	anchor := [2]decimal.Decimal{in.PairAnchors.Bull, in.PairAnchors.Bear}
	gap := in.Hold[cex.Bull].Mul(anchor[cex.Bull]).Sub(in.Hold[cex.Bear].Mul(anchor[cex.Bear]))
	lead, lag := cex.Bull, cex.Bear
	if gap.IsNegative() {
		lead, lag = cex.Bear, cex.Bull
	}
	// A trim is reduce-only: within what the venue's net position lets it
	// close (Reducible).
	trimQty := decimal.Min(floorToStep(gap.Abs().Div(anchor[lead]), in.Rules[lead].QtyStep), Reducible(in.Hold, in.Direction, in.Dust)[lead])
	compQty := floorToStep(gap.Abs().Div(anchor[lag]), in.Rules[lag].QtyStep)
	if trimQty.IsZero() && compQty.IsZero() {
		return GapAction{Kind: GapNone}
	}
	opening, closing := in.Direction, in.Direction.Opposite()

	// The trim: it needs a price on the larger leg.
	trim := GapAction{Kind: GapTrim, Leg: lead, Side: closing.Side(), Qty: trimQty}
	canTrim := false
	if in.HasPrice[lead] {
		trim.Price = throughBest(in.Books[lead], closing, s.cfg.ForcedSlipBps, in.Rules[lead].PriceTick)
		canTrim = in.Rules[lead].Sendable(trimQty, trim.Price)
	}

	// The completion: only where the rules above allow it.
	comp := GapAction{Kind: GapComplete, Leg: lag, Side: opening.Side()}
	canComp := false
	var compFills []cex.BookLevel
	if in.Batch != nil && in.CanComplete[lag] && in.Now.Sub(in.Batch.SentAt) <= s.cfg.CompletionWindow {
		if !in.BookNew[lag] {
			// Only the smaller leg's new frame is missing. Waiting does
			// not move the end of the window.
			return GapAction{Kind: GapWait}
		}
		comp.Qty, comp.Price, compFills = capByLimit(in.Books[lag], opening, compQty, in.QuotaLeft, in.Rules[lag])
		comp.QuotaPrice = quotaPrice(in.Books[lag], opening, comp.Price)
		canComp = comp.Qty.IsPositive() && in.Rules[lag].Sendable(comp.Qty, comp.Price)
	}

	// Only one of the two can be sent, or neither.
	switch {
	case !canComp && canTrim:
		return trim
	case !canComp && !in.HasPrice[lead]:
		return GapAction{Kind: GapBlocked}
	case !canComp:
		return GapAction{Kind: GapNone} // dust: no order can be sent for it
	case !canTrim:
		return comp
	}

	// Both can be sent: compare the two losses for the same share of the
	// gap on the raw books. The share the quota cut from the completion is
	// left out of both.
	//
	// The completion loses what its fills are worse than the batch's limit
	// price; its fee would have been paid anyway. The trim loses what
	// closing is worse than that quantity's entry fills, plus the entry and
	// exit fees of that quantity.
	cmpTrimQty := floorToStep(trimQty.Mul(comp.Qty).Div(compQty), in.Rules[lead].QtyStep)
	lossComp := adverse(value(compFills), comp.Qty.Mul(in.Batch.PlanPrice[lag]), opening)
	trimValue := cmpTrimQty.Mul(bestPrice(in.Books[lead], closing))
	if in.DepthOK[lead] {
		trimValue = value(walkOrLast(in.Books[lead], closing, cmpTrimQty))
	}
	entryValue := cmpTrimQty.Mul(in.Batch.AvgFill[lead])
	lossTrim := adverse(trimValue, entryValue, closing).Add(s.feeRate().Mul(entryValue.Add(trimValue)))

	comp.LossComplete, comp.LossTrim = lossComp, lossTrim
	trim.LossComplete, trim.LossTrim = lossComp, lossTrim
	if lossComp.LessThan(lossTrim) {
		return comp
	}
	return trim
}

// Reducible caps a cycle's holding per leg at what a reduce-only order may
// close at the venue: the net position there, which also carries the dust
// of earlier cycles (signed, as the ledger keeps it). Dust of the other
// sign (a long cycle's 7.80 next to dust of -0.01) leaves 7.79 to reduce;
// dust of the same sign changes nothing. A reducing order above the
// venue's net is refused (Bitget 40804, 25231 by REST), so every reducing
// order is sized within this.
func Reducible(hold [2]decimal.Decimal, dir Direction, dust [2]decimal.Decimal) (out [2]decimal.Decimal) {
	for _, leg := range cex.Legs {
		d := dust[leg]
		if dir == Short {
			d = d.Neg()
		}
		out[leg] = decimal.Max(decimal.Zero, decimal.Min(hold[leg], hold[leg].Add(d)))
	}
	return out
}
