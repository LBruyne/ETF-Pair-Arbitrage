package strategy

import (
	"math"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
)

// ExitPrecheck is a line exit batch's pre-check (SPEC 12.7 step 0): the
// round's expected net in USDT were the batch, qty per leg, closed now at
// price (CloseNet, first in, first out), and whether it reaches the margin
// on the batch's base leg (the thin leg; SOXS when both legs go together).
func (s *Strategy) ExitPrecheck(in ExitInput, qty, price [2]decimal.Decimal) (net decimal.Decimal, pass bool, err error) {
	net, err = in.Value(cex.PurposeExit, closeAt(qty, price), s.feeRate())
	if err != nil {
		return decimal.Zero, false, err
	}
	base := baseLeg(in.Pairing)
	return net, !net.LessThan(s.marginUSDT(qty[base], anchorOf(in.PairAnchors, base), in.F)), nil
}

// NewExitBatch starts the thin-first exit batch of plan for a cycle in
// direction dir: the decision's spread, the pre-check and, with D1, the
// thick leg's protection travel with the batch; exec adds the fills.
func NewExitBatch(dir Direction, plan *ExitPlan) ExitBatch {
	return ExitBatch{
		Dir:             dir,
		Line:            plan.Line,
		DecisionD:       plan.D,
		DecisionAnchors: plan.Anchors,
		DecisionF:       plan.F,
		ExpectedNet:     plan.ExpectedNet,
		D1:              plan.D1,
		Protect:         plan.Protect,
		Fills:           Fills{ThinQty: decimal.Zero, ThinVWAP: decimal.Zero, ThickQty: decimal.Zero, ThickVWAP: decimal.Zero},
	}
}

// ExitPairStep decides one step of the thick leg of a thin-first line exit
// batch (SPEC 12.7 steps 2-3, PRD 9.12). It looks at no exit line: once the
// thin leg closed part of the batch, the matching thick leg is finished
// before the exit pauses. The checks run in this order: an order in flight,
// the round taken over, the thick leg matched (or only dust left), the hard
// end (D2: exit_pair_window from the first thin closing fill, not reset, or
// the ideal closing spread pair_stop_d worse than at the decision; both
// close at PRD 10.4's price), and the thick order within its bound, the
// break-even bound or, with D1, the protection.
//
// Preconditions as PairStep's.
func (s *Strategy) ExitPairStep(in ExitPairInput) ExitPairAction {
	b := in.Batch
	thin, thick := in.Pairing.Thin, in.Pairing.thick()
	closing := b.Dir.Opposite()
	act := ExitPairAction{Leg: thick, DIdeal: math.NaN()}

	switch {
	case in.OrderPending:
		act.Kind, act.Why = ExitPairWait, ReasonAwaitOrders
		return act
	case in.Stopping:
		act.Kind, act.Why = ExitPairHandOver, ReasonStopping
		return act
	case in.Forced:
		act.Kind, act.Why = ExitPairHandOver, ReasonForced
		return act
	}

	target := ExitThickTarget(in.Pairing, in.PairAnchors, in.Rules[thick].QtyStep, in.Before, b.Fills, in.Hold)
	if !target.IsPositive() {
		act.Kind = ExitPairDone
		return act
	}
	if in.DepthOK[thick] {
		a, f := decisionBasis(b.DecisionAnchors, b.DecisionF, in.Anchors, in.F)
		if d, ok := s.IdealSpread(closing, thin, b.Fills, in.Books[thick], f, a, in.PairAnchors); ok {
			act.DIdeal = d
		}
	}

	// D2: the hard end.
	switch {
	case b.HardEnd != "":
		return s.hardClose(act, in, b.HardEnd, target)
	case !in.Now.Before(b.FirstFill.Add(in.Pairing.ExitWindow)):
		return s.hardClose(act, in, ReasonExitWindow, target)
	case !math.IsNaN(act.DIdeal) && adverseD(closing, act.DIdeal, b.DecisionD) >= in.Pairing.StopD:
		return s.hardClose(act, in, ReasonExitStopD, target)
	}

	// The bound, and the thick order within it.
	why := Reason("")
	act.Bound = b.Protect
	if b.D1 {
		why = ReasonD1Loss
	} else {
		bound, err := s.ExitThickBound(in, target)
		if err != nil {
			act.Kind, act.Why, act.Err = ExitPairWait, ReasonValuation, err
			return act
		}
		act.Bound = bound
	}
	if !in.DepthOK[thick] {
		act.Kind, act.Why = ExitPairWait, ReasonDepth
		return act
	}
	if q, p, ok := walkWithin(in.Books[thick], closing, act.Bound, target, in.Rules[thick]); ok {
		act.Kind, act.Why, act.Side, act.Price, act.Qty = ExitPairSendThick, why, closing.Side(), p, q
		return act
	}
	act.Kind, act.Why = ExitPairWait, ReasonExitBound
	return act
}

// ExitThickTarget is what the thick leg of a thin-first exit batch still
// has to close (SPEC 12.7 step 2): the round's cumulative target, floor((the
// thin leg closed before the batch + its thin fills) x A_thin / A_thick) at
// the pairing anchors and the thick leg's step, less the thick leg closed
// before it and by it, within what the cycle holds. Once a batch with a
// thin fill has closed the last of the thin leg, it is everything the thick
// leg still holds. A floor per batch would lose up to one step per batch
// and leave it as dust (live 2026-11-27: SOXL 15.57 of 15.58 closed).
// before and hold are per leg, as ExitPairInput's Before and Hold.
func ExitThickTarget(p Pairing, pair cex.Anchors, step decimal.Decimal, before [2]decimal.Decimal, fills Fills, hold [2]decimal.Decimal) decimal.Decimal {
	thin, thick := p.Thin, p.thick()
	if fills.ThinQty.IsPositive() && !hold[thin].IsPositive() {
		return hold[thick]
	}
	ratio := anchorOf(pair, thin).Div(anchorOf(pair, thick))
	total := floorToStep(before[thin].Add(fills.ThinQty).Mul(ratio), step)
	return decimal.Min(hold[thick], total.Sub(before[thick]).Sub(fills.ThickQty))
}

// ExitThickBound is the break-even bound of an exit batch's thick leg (SPEC
// 12.7 step 2): the worst price at the tick (a sell's lowest, rounded up; a
// buy's highest, rounded down) at which closing rest more of the thick leg
// keeps the round's expected net (CloseNet with cex.PurposeExit) at the
// margin of the batch's thin leg. The batch's fills so far are booked in the
// ledger already and count at their actual value; only rest is priced.
func (s *Strategy) ExitThickBound(in ExitPairInput, rest decimal.Decimal) (decimal.Decimal, error) {
	b, thin, thick := in.Batch, in.Pairing.Thin, in.Pairing.thick()
	closing := b.Dir.Opposite()
	tick := in.Rules[thick].PriceTick
	ref := bestPrice(in.Books[thick], closing)
	if !ref.IsPositive() {
		ref = tick
	}
	need := s.marginUSDT(b.ThinQty, anchorOf(in.PairAnchors, thin), in.F)
	return s.breakEven(in.Value, cex.PurposeExit, thick, rest, ref, tick, need, closing)
}

// hardClose is the D2 order for the thick leg's target at PRD 10.4's
// price. Without a best price its quantity is zero and it goes on next
// round; with only dust left the batch is done.
func (s *Strategy) hardClose(act ExitPairAction, in ExitPairInput, why Reason, target decimal.Decimal) ExitPairAction {
	thick, closing := in.Pairing.thick(), in.Batch.Dir.Opposite()
	act.Kind, act.Why, act.Side = ExitPairHardClose, why, closing.Side()
	if !bestPrice(in.Books[thick], closing).IsPositive() {
		return act
	}
	price := throughBest(in.Books[thick], closing, s.cfg.ForcedSlipBps, in.Rules[thick].PriceTick)
	if !in.Rules[thick].Sendable(target, price) {
		act.Kind, act.Why, act.Side = ExitPairDone, "", ""
		return act
	}
	act.Price, act.Qty = price, target
	return act
}

// walkWithin walks the levels on dir's taking side, best first, down to
// limit and returns the largest order of at most need: at each level the
// quantity down to it, floored to the step, sendable at that level's price,
// which is the order's limit. Ties go to the better price.
func walkWithin(b *cex.Book, dir Direction, limit, need decimal.Decimal, rule cex.MarketRule) (qty, price decimal.Decimal, ok bool) {
	held := decimal.Zero
	for _, lv := range takerSide(b, dir) {
		if worse(dir, lv.Price, limit) {
			break
		}
		held = held.Add(lv.Qty)
		q := floorToStep(decimal.Min(need, held), rule.QtyStep)
		if q.GreaterThan(qty) && rule.Sendable(q, lv.Price) {
			qty, price, ok = q, lv.Price, true
		}
	}
	return qty, price, ok
}
