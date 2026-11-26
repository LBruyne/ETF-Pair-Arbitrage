package strategy

import (
	"errors"
	"math"
	"time"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
)

// ThinOrder is the first order of a thin-leg-first batch (SPEC 12.4 step
// one, PRD 5.15): the thin leg's IOC alone, at its pricing level, for the
// plan's quantity qS. The plan is sized as any entry batch (EvaluateEntry).
func (p *EntryPlan) ThinOrder(thin cex.Leg) LegOrder {
	return LegOrder{Side: p.Direction.Side(), Price: p.Price[thin], Qty: p.Qty[thin]}
}

// NewPairBatch starts pairing the batch of plan once its thin order filled
// qty at vwap. firstFill is the local arrival of the first fill report; the
// pair window runs from it. The top-up reference starts at the decision's x.
func NewPairBatch(plan *EntryPlan, thin cex.Leg, qty, vwap decimal.Decimal, firstFill time.Time) PairBatch {
	return PairBatch{
		Dir:             plan.Direction,
		DecisionD:       plan.X + plan.Mu,
		DecisionAnchors: plan.Anchors,
		DecisionF:       plan.F,
		XRef:            plan.X,
		FeeD:            plan.FeeD,
		PlanThin:        plan.Qty[thin],
		Fills:           Fills{ThinQty: qty, ThinVWAP: vwap, ThickQty: decimal.Zero, ThickVWAP: decimal.Zero},
		ThinClosed:      decimal.Zero,
		FirstFill:       firstFill,
	}
}

// PairStep decides one step of a batch in pairing (SPEC 12.4, PRD 5.16). It
// runs on every depth frame, every order result and every tick of exec's
// loop, so the window ends without new data. The checks run in the order of
// SPEC 12.4 version 4 and the first that holds wins: an order in flight, the
// round taken over, the batch paired (or dust that can never pair), the
// window, the spread stop, the thick order, a top-up, a profit close of the
// unpaired thin leg, too small with no top-up left, and else wait.
//
// "This frame cannot top up" (x is not deeper than x_ref) only waits; "the
// batch can top up no more" (2 x qS reached, or no new entries) lets a
// remainder too small to pair end the batch.
//
// Preconditions, which the caller guarantees and which are not checked: the
// contract rules are loaded, the anchors are positive, F is a positive
// number, and the batch has a thin fill.
func (s *Strategy) PairStep(in PairInput) PairAction {
	b, p := in.Batch, in.Pairing
	thin, thick := p.Thin, p.thick()
	act := PairAction{X: math.NaN(), XRef: b.XRef, DIdeal: math.NaN()}

	// 1 and 2: an order in flight, and the round taken over.
	switch {
	case in.OrderPending:
		return act.wait(ReasonAwaitOrders)
	case in.Stopping:
		act.Kind, act.Why = PairHandOver, ReasonStopping
		return act
	case in.Forced:
		act.Kind, act.Why = PairHandOver, ReasonForced
		return act
	}

	// What is left to pair, from the batch's fills at the pairing anchors:
	// T = floor(held x A_thin / A_thick), need = T - L, S_open = held - L x
	// A_thick / A_thin.
	ratio := anchorOf(in.PairAnchors, thin).Div(anchorOf(in.PairAnchors, thick))
	held := b.ThinQty.Sub(b.ThinClosed)
	need := b.need(thin, in.PairAnchors, in.Rules[thick].QtyStep)
	open := decimal.Max(decimal.Zero, held.Sub(b.ThickQty.Div(ratio)))
	unwind, priced := s.unwindOrder(in, open)

	// An unwind in progress goes on with what is left unpaired.
	if b.Unwind != "" {
		if unwind == nil && priced {
			return act.done(b)
		}
		return act.unwind(b.Unwind, unwind, thin)
	}

	// 2a: paired, or dust that can never pair (SPEC 12.4 "薄腿太小").
	noMoreTopUps := in.NoNewEntries || !floorToStep(p.TopupMax.Mul(b.PlanThin).Sub(b.ThinQty), in.Rules[thin].QtyStep).IsPositive()
	tooSmall := need.IsPositive() && in.DepthOK[thick] && !in.Rules[thick].Sendable(need, bestPrice(in.Books[thick], b.Dir))
	if !need.IsPositive() || (tooSmall && unwind == nil && priced && noMoreTopUps) {
		return act.done(b)
	}

	// The decision's detail: x now and the ideally paired spread.
	levels, haveLevels := [2]cex.BookLevel{}, false
	mu, sigma, haveStats := lineStats(in.Stats, in.Anchors)
	if in.DepthOK[cex.Bull] && in.DepthOK[cex.Bear] {
		levels, haveLevels = s.pricingLevels(in.Books, b.Dir)
	}
	if haveLevels && haveStats {
		act.X = levelSpread(levels, in.F, in.Anchors) - mu
	}
	if in.DepthOK[thick] {
		fills := b.Fills
		fills.ThinQty = held
		a, f := decisionBasis(b.DecisionAnchors, b.DecisionF, in.Anchors, in.F)
		if d, ok := s.IdealSpread(b.Dir, thin, fills, in.Books[thick], f, a, in.PairAnchors); ok {
			act.DIdeal = d
		}
	}

	// 3 and 4: the window and the spread stop unwind S_open. The stop
	// compares D_ideal with D_dec under the decision's anchors and F; P* and
	// x below stay on the current ones.
	if !in.Now.Before(b.FirstFill.Add(p.Window)) {
		return act.startUnwind(b, ReasonWindow, unwind, priced, thin)
	}
	if !math.IsNaN(act.DIdeal) && adverseD(b.Dir, act.DIdeal, b.DecisionD) >= p.StopD {
		return act.startUnwind(b, ReasonStopD, unwind, priced, thin)
	}

	// 5: the thick order, level by level within the batch's bound P*.
	if vwapCap, ok := s.thickCap(in); ok {
		act.VWAPCap = vwapCap
		act.PStar = pairLimit(vwapCap, b.Fills, need, b.Dir, in.Rules[thick].PriceTick)
		if in.DepthOK[thick] {
			if q, limit, quotaAt, ok := thickOrder(in.Books[thick], b.Dir, vwapCap, b.Fills, need, in.Rules[thick], in.QuotaLeft, b.ThinVWAP.Div(ratio)); ok {
				return act.order(PairSendThick, "", thick, b.Dir.Side(), limit, q, quotaAt)
			}
		}
	}

	// 6: a top-up while x is strictly deeper than x_ref and at the line.
	xOK := !math.IsNaN(act.X) && sigma > 0
	if xOK && !noMoreTopUps && !in.TopUpBlocked && deeper(b.Dir, act.X, b.XRef) && s.AtEntryLine(b.Dir, act.X, sigma) {
		if q, limit, quotaAt, ok := s.topUpOrder(in, levels, open, ratio); ok {
			return act.order(PairSendThin, "", thin, b.Dir.Side(), limit, q, quotaAt)
		}
	}

	// 7: x back inside the entry line: the unpaired part may close on its
	// own at or above break-even.
	if xOK && !s.AtEntryLine(b.Dir, act.X, sigma) && in.DepthOK[thin] {
		q, limit, be, ok, err := s.profitClose(in, open)
		if err != nil {
			act.Err = err
			return act.wait(ReasonValuation)
		}
		act.BreakEven = be
		if ok {
			return act.order(PairCloseThin, ReasonProfit, thin, b.Dir.Opposite().Side(), limit, q, decimal.Zero)
		}
	}

	// 8 and 9: too small with no top-up left ends the batch; the rest waits.
	if tooSmall && noMoreTopUps {
		return act.startUnwind(b, ReasonTooSmall, unwind, priced, thin)
	}
	if tooSmall {
		return act.wait(ReasonTooSmall)
	}
	return act.wait(ReasonPairBound)
}

// PairBound is the bound of the thick leg of a batch in pairing (SPEC 12.4
// step 5, PRD 5.16 item 6). vwapCap is P*, the most (a short: the least)
// the batch's thick VWAP may come to so that the distance from its spread
// to the exit1 line pays the round-trip fee C and the margin. limit is the
// price one more order of qty may have with the batch's VWAP still within
// P*, rounded to the tick against the order (down for a buy, up for a
// sell). ok is false while the mean or sigma give no number.
func (s *Strategy) PairBound(in PairInput, qty decimal.Decimal) (vwapCap, limit decimal.Decimal, ok bool) {
	vwapCap, ok = s.thickCap(in)
	if !ok {
		return decimal.Zero, decimal.Zero, false
	}
	return vwapCap, pairLimit(vwapCap, in.Batch.Fills, qty, in.Batch.Dir, in.Rules[in.Pairing.thick()].PriceTick), true
}

// IdealSpread is the spread of a batch were the rest of its thick target
// filled now at the thick leg's pricing level (SPEC 12.4 step 4, 12.12 item
// 1): T_full = S x A_thin / A_thick unrounded, the thick price is (L x v_L +
// (T_full - L) x p_level) / T_full and the thin price v_S. dir is the
// direction of the batch's orders: the entry's for an entry batch, the
// cycle's opposite for an exit batch, so the thick level is taken on the
// side those orders take. a are the anchors of the spread, pair the pairing
// anchors. ok is false without a thin fill or without the thick leg's
// pricing level.
func (s *Strategy) IdealSpread(dir Direction, thin cex.Leg, fills Fills, thickBook *cex.Book, f float64, a, pair cex.Anchors) (float64, bool) {
	thick := otherLeg(thin)
	if !fills.ThinQty.IsPositive() {
		return math.NaN(), false
	}
	level, ok := s.pricingLevel(thickBook, thick, dir)
	if !ok {
		return math.NaN(), false
	}
	tFull := fills.ThinQty.Mul(anchorOf(pair, thin)).Div(anchorOf(pair, thick))
	ideal := fills.ThickQty.Mul(fills.ThickVWAP).Add(tFull.Sub(fills.ThickQty).Mul(level.Price)).Div(tFull)
	var price [2]float64
	price[thin], price[thick] = fills.ThinVWAP.InexactFloat64(), ideal.InexactFloat64()
	return Spread(f, price[cex.Bull], a.Bull.InexactFloat64(), price[cex.Bear], a.Bear.InexactFloat64()), true
}

// thickCap is P* of the batch (SPEC 12.4 step 5): a_thick x (2 + D_need /
// 10^4 - v_thin / a_thin), unrounded, with D_need = D_exit1 - C - m for a
// long and D_exit1 + C + m for a short, D_exit1 the exit1 line from the
// current mean and sigma, and a = A / F under the current anchors. ok is
// false while the mean or sigma give no number.
func (s *Strategy) thickCap(in PairInput) (decimal.Decimal, bool) {
	b, thin := in.Batch, in.Pairing.Thin
	mu, sigma, ok := lineStats(in.Stats, in.Anchors)
	if !ok {
		return decimal.Zero, false
	}
	dNeed := mu - s.cfg.EntryOut*sigma - b.FeeD - s.cfg.CostMarginD
	if b.Dir == Short {
		dNeed = mu + s.cfg.EntryOut*sigma + b.FeeD + s.cfg.CostMarginD
	}
	f := decimal.NewFromFloat(in.F)
	aThick, aThin := anchorOf(in.Anchors, otherLeg(thin)).Div(f), anchorOf(in.Anchors, thin).Div(f)
	two := decimal.NewFromInt(2)
	return aThick.Mul(two.Add(decimal.NewFromFloat(dNeed).Div(bpsPerUnit)).Sub(b.ThinVWAP.Div(aThin))), true
}

// pairLimit is the limit one thick order of qty may have so that the
// batch's thick VWAP stays within vwapCap: (P* x (L + q) - L x v_L) / q,
// rounded to the tick against the order.
func pairLimit(vwapCap decimal.Decimal, fills Fills, qty decimal.Decimal, dir Direction, tick decimal.Decimal) decimal.Decimal {
	limit := vwapCap
	if qty.IsPositive() {
		limit = vwapCap.Mul(fills.ThickQty.Add(qty)).Sub(fills.ThickQty.Mul(fills.ThickVWAP)).Div(qty)
	}
	return roundToTick(limit, tick, dir == Short)
}

// thickOrder walks the levels received on the thick leg's taking side, best
// first, and returns the largest order the batch's bound allows (SPEC 12.4
// step 5, audit counterexample 10). At level price p the quantity is what
// the book holds down to p, capped by need; when p is beyond vwapCap only
// the room earlier cheaper fills left, L x |P* - v_L| / |p - P*| (none while
// L is zero or v_L is not inside P*); and the whole pair the order adds
// (the order at its quota price plus thinPerThick of the paired thin leg
// per share) stays within quotaLeft. The quantity is floored to the step and
// must pass the venue's minimums at p, which is the order's limit. Ties go
// to the better price.
func thickOrder(b *cex.Book, dir Direction, vwapCap decimal.Decimal, fills Fills, need decimal.Decimal, rule cex.MarketRule, quotaLeft, thinPerThick decimal.Decimal) (qty, limit, quotaAt decimal.Decimal, ok bool) {
	held := decimal.Zero
	for _, lv := range takerSide(b, dir) {
		held = held.Add(lv.Qty)
		q := decimal.Min(need, held)
		if worse(dir, lv.Price, vwapCap) {
			room := decimal.Zero
			if fills.ThickQty.IsPositive() && worse(dir, vwapCap, fills.ThickVWAP) {
				room = fills.ThickQty.Mul(vwapCap.Sub(fills.ThickVWAP).Abs()).Div(lv.Price.Sub(vwapCap).Abs())
			}
			q = decimal.Min(q, room)
		}
		qa := quotaPrice(b, dir, lv.Price)
		q = floorToStep(decimal.Min(q, decimal.Max(decimal.Zero, quotaLeft).Div(qa.Add(thinPerThick))), rule.QtyStep)
		if q.GreaterThan(qty) && rule.Sendable(q, lv.Price) {
			qty, limit, quotaAt, ok = q, lv.Price, qa, true
		}
	}
	return qty, limit, quotaAt, ok
}

// topUpOrder sizes a top-up of the thin leg (SPEC 12.4 step 6): what the
// book gives at both legs' pricing levels as for an entry batch, within
// thin_topup_max x qS less S, and within the quota the full pairing of the
// existing unpaired part and the top-up leaves (topUpQuotaCap). The limit is
// the thin leg's pricing-level price.
func (s *Strategy) topUpOrder(in PairInput, levels [2]cex.BookLevel, open, ratio decimal.Decimal) (qty, limit, quotaAt decimal.Decimal, ok bool) {
	b, thin, thick := in.Batch, in.Pairing.Thin, in.Pairing.thick()
	limit = levels[thin].Price
	offered := decimal.Min(levels[thin].Qty.Mul(s.cfg.TakeRatio).Mul(anchorOf(in.PairAnchors, thin)),
		levels[thick].Qty.Mul(s.cfg.TakeRatio).Mul(anchorOf(in.PairAnchors, thick)))
	room := in.Pairing.TopupMax.Mul(b.PlanThin).Sub(b.ThinQty)

	quotaAt = quotaPrice(in.Books[thin], b.Dir, limit)
	thickQuota := quotaPrice(in.Books[thick], b.Dir, levels[thick].Price)
	existing := open.Mul(b.ThinVWAP).Add(floorToStep(open.Mul(ratio), in.Rules[thick].QtyStep).Mul(thickQuota))
	byQuota := topUpQuotaCap(in.QuotaLeft, existing, quotaAt, thickQuota, ratio, in.Rules[thin].QtyStep)

	qty = floorToStep(decimal.Min(offered.Div(anchorOf(in.PairAnchors, thin)), room, byQuota), in.Rules[thin].QtyStep)
	return qty, limit, quotaAt, in.Rules[thin].Sendable(qty, limit)
}

// topUpQuotaCap is the most the thin leg may add so that it and the
// existing unpaired part, both paired, fit the quota left (SPEC 12.4 step 6,
// PRD 6.4): (quotaLeft - existing) / (thinQuota + ratio x thickQuota),
// floored to the step and never negative. existing is what pairing the
// unpaired part takes, that part at its actual cost; ratio is the thick
// shares per thin share.
func topUpQuotaCap(quotaLeft, existing, thinQuota, thickQuota, ratio, step decimal.Decimal) decimal.Decimal {
	left := quotaLeft.Sub(existing)
	if !left.IsPositive() {
		return decimal.Zero
	}
	return floorToStep(left.Div(thinQuota.Add(ratio.Mul(thickQuota))), step)
}

// profitClose sizes a profit close of the unpaired thin leg (SPEC 12.4 step
// 7, PRD 5.16 item 8). b* is the worst price at the tick at which closing
// the unpaired part of this lot alone (CloseNet with cex.PurposeTrim)
// reaches the margin; the limit is b* or the thin leg's pricing level on
// the closing side, whichever is better for us; the quantity is what the
// book holds at or better than the limit, within S_open, floored and
// sendable at the limit, and its valuation at the limit is checked once
// more. ok is false when nothing passes; be is b* whenever it was found.
func (s *Strategy) profitClose(in PairInput, open decimal.Decimal) (qty, limit, be decimal.Decimal, ok bool, err error) {
	thin, closing := in.Pairing.Thin, in.Batch.Dir.Opposite()
	rule, book := in.Rules[thin], in.Books[thin]
	whole := floorToStep(open, rule.QtyStep)
	level, have := s.pricingLevel(book, thin, closing)
	if !whole.IsPositive() || !have {
		return decimal.Zero, decimal.Zero, decimal.Zero, false, nil
	}
	anchor := anchorOf(in.PairAnchors, thin)
	be, err = s.breakEven(in.Value, cex.PurposeTrim, thin, whole, level.Price, rule.PriceTick, s.marginUSDT(whole, anchor, in.F), closing)
	if err != nil {
		return decimal.Zero, decimal.Zero, decimal.Zero, false, err
	}
	limit = level.Price
	if worse(closing, limit, be) {
		limit = be
	}
	avail := decimal.Zero
	for _, lv := range takerSide(book, closing) {
		if worse(closing, lv.Price, limit) {
			break
		}
		avail = avail.Add(lv.Qty)
	}
	qty = floorToStep(decimal.Min(avail, open), rule.QtyStep)
	if !rule.Sendable(qty, limit) {
		return decimal.Zero, decimal.Zero, be, false, nil
	}
	net, err := in.Value(cex.PurposeTrim, legClose(thin, qty, limit), s.feeRate())
	if err != nil {
		return decimal.Zero, decimal.Zero, be, false, err
	}
	if net.LessThan(s.marginUSDT(qty, anchor, in.F)) {
		return decimal.Zero, decimal.Zero, be, false, nil
	}
	return qty, limit, be, true, nil
}

// unwindOrder is the reduce-only order that unwinds the unpaired thin leg
// (PRD 5.18): S_open floored to the step, through the raw best price by
// forced_slip_bps (PRD 10.4). It is nil when nothing sendable is left: the
// rest is dust (PRD 8.1). priced is false while the thin leg has no best
// price on the closing side; nothing can be told about the order then.
func (s *Strategy) unwindOrder(in PairInput, open decimal.Decimal) (o *LegOrder, priced bool) {
	thin, closing := in.Pairing.Thin, in.Batch.Dir.Opposite()
	if !bestPrice(in.Books[thin], closing).IsPositive() {
		return nil, false
	}
	qty := floorToStep(open, in.Rules[thin].QtyStep)
	price := throughBest(in.Books[thin], closing, s.cfg.ForcedSlipBps, in.Rules[thin].PriceTick)
	if !qty.IsPositive() || !in.Rules[thin].Sendable(qty, price) {
		return nil, true
	}
	return &LegOrder{Side: closing.Side(), Price: price, Qty: qty}, true
}

// HandOverEnd is the end state of a batch the stop or the forced exit takes
// over (SPEC 12.4 step 2), as its fills stand at that moment: with part of
// the thin leg still unpaired (the thick target, floored to thickStep, not
// reached) built_partial when some of it was paired and unwound_stop when
// none was, for the forced exit closes the rest; otherwise End. thin is the
// thin leg and pair the cycle's pairing anchors; without a step (the rules
// not read) the target is not floored.
func (b PairBatch) HandOverEnd(thin cex.Leg, pair cex.Anchors, thickStep decimal.Decimal) PairEnd {
	switch {
	case !b.need(thin, pair, thickStep).IsPositive():
		return b.End()
	case b.ThickQty.IsPositive():
		return PairEndBuiltPartial
	default:
		return PairEndUnwoundStop
	}
}

// need is what the thick leg still has to pair: the target floor(held x
// A_thin / A_thick) at the pairing anchors, less L; unfloored without a
// step.
func (b PairBatch) need(thin cex.Leg, pair cex.Anchors, thickStep decimal.Decimal) decimal.Decimal {
	target := b.ThinQty.Sub(b.ThinClosed).Mul(anchorOf(pair, thin)).Div(anchorOf(pair, otherLeg(thin)))
	if thickStep.IsPositive() {
		target = floorToStep(target, thickStep)
	}
	return target.Sub(b.ThickQty)
}

// End is the end state the batch's fills say (SPEC 12.4 table): built while
// part was paired and nothing of the thin leg was closed again (the unpaired
// rest is dust), built_partial when part was paired and the rest closed, and
// with nothing paired unwound_profit or unwound_stop by whether an unwind
// ran. A batch that paired nothing and closed nothing holds only dust of the
// thin leg: unwound_stop (the lead's override in 10.6 of 10.4 deviation 7).
// exec reads it too when the stop or the forced exit takes a batch over: it
// ends in the state its fills say at that moment.
func (b PairBatch) End() PairEnd {
	switch {
	case b.neverPaired():
		return PairEndUnwoundStop
	case !b.ThinClosed.IsPositive():
		return PairEndBuilt
	case b.ThickQty.IsPositive():
		return PairEndBuiltPartial
	case b.Unwind == "":
		return PairEndUnwoundProfit
	default:
		return PairEndUnwoundStop
	}
}

// --- actions ---

func (a PairAction) wait(why Reason) PairAction {
	a.Kind, a.Why = PairWait, why
	return a
}

// done ends the batch in the state its fills say; a batch left with only
// dust of a thin leg it never paired gives the reason too_small.
func (a PairAction) done(b PairBatch) PairAction {
	a.Kind, a.End = PairDone, b.End()
	if b.neverPaired() {
		a.Why = ReasonTooSmall
	}
	return a
}

// neverPaired reports whether the batch paired nothing and closed nothing:
// all it holds is a thin leg too small to pair or to close.
func (b PairBatch) neverPaired() bool {
	return !b.ThickQty.IsPositive() && !b.ThinClosed.IsPositive()
}

func (a PairAction) order(kind PairKind, why Reason, leg cex.Leg, side cex.Side, price, qty, quotaAt decimal.Decimal) PairAction {
	a.Kind, a.Why, a.Leg, a.Side, a.Price, a.Qty, a.QuotaPrice = kind, why, leg, side, price, qty, quotaAt
	return a
}

// unwind is the next unwind order; without a price for it this round its
// quantity is zero and the unwind goes on next round.
func (a PairAction) unwind(why Reason, o *LegOrder, thin cex.Leg) PairAction {
	a.Kind, a.Why, a.Leg = PairUnwind, why, thin
	if o != nil {
		a.Side, a.Price, a.Qty = o.Side, o.Price, o.Qty
	}
	return a
}

// startUnwind starts an unwind for why; with only dust left the batch ends.
func (a PairAction) startUnwind(b PairBatch, why Reason, o *LegOrder, priced bool, thin cex.Leg) PairAction {
	if o == nil && priced {
		return a.done(b)
	}
	return a.unwind(why, o, thin)
}

// --- helpers ---

// thick is the leg that follows the thin leg's fills.
func (p Pairing) thick() cex.Leg { return otherLeg(p.Thin) }

func otherLeg(l cex.Leg) cex.Leg {
	if l == cex.Bull {
		return cex.Bear
	}
	return cex.Bull
}

func anchorOf(a cex.Anchors, l cex.Leg) decimal.Decimal {
	if l == cex.Bull {
		return a.Bull
	}
	return a.Bear
}

// decisionBasis is the anchors and F a batch's decision spread is under,
// a and f; the current ones, cur and curF, when the batch does not carry
// them (restored without its decision record).
func decisionBasis(a cex.Anchors, f float64, cur cex.Anchors, curF float64) (cex.Anchors, float64) {
	if !a.Bull.IsPositive() || !a.Bear.IsPositive() || !(f > 0) {
		return cur, curF
	}
	return a, f
}

// lineStats is the mean and sigma under the anchors a, and whether they are
// numbers a line can be drawn from.
func lineStats(st Stats, a cex.Anchors) (mu, sigma float64, ok bool) {
	if !st.Ready {
		return math.NaN(), math.NaN(), false
	}
	mu, sigma = st.Mu(a), st.Sigma(a)
	return mu, sigma, finite(mu, sigma)
}

// worse reports whether price p is worse than ref for an order in
// direction dir: higher for a buy, lower for a sell.
func worse(dir Direction, p, ref decimal.Decimal) bool {
	if dir == Long {
		return p.GreaterThan(ref)
	}
	return p.LessThan(ref)
}

// deeper reports whether x is strictly further from the mean than ref on
// the side of an entry in direction dir.
func deeper(dir Direction, x, ref float64) bool {
	if dir == Long {
		return x < ref
	}
	return x > ref
}

// adverseD is how much worse the spread d is than ref for orders in
// direction dir: a buy loses as the spread rises, a sell as it falls.
func adverseD(dir Direction, d, ref float64) float64 {
	if dir == Long {
		return d - ref
	}
	return ref - d
}

// roundToTick rounds a price to the tick, up or down.
func roundToTick(p, tick decimal.Decimal, up bool) decimal.Decimal {
	if up {
		return p.Div(tick).Ceil().Mul(tick)
	}
	return p.Div(tick).Floor().Mul(tick)
}

// legClose is a close of qty at price on one leg alone.
func legClose(leg cex.Leg, qty, price decimal.Decimal) [2]CloseLeg {
	c := [2]CloseLeg{{Qty: decimal.Zero, Price: decimal.Zero}, {Qty: decimal.Zero, Price: decimal.Zero}}
	c[leg] = CloseLeg{Qty: qty, Price: price}
	return c
}

// marginUSDT is the margin m in USDT for a part whose thin leg is qty at
// the pairing anchor anchor: m x qty x anchor / F / 10^4 (PRD 5.20).
func (s *Strategy) marginUSDT(qty, anchor decimal.Decimal, f float64) decimal.Decimal {
	if s.cfg.CostMarginD == 0 {
		return decimal.Zero
	}
	return decimal.NewFromFloat(s.cfg.CostMarginD).Mul(qty).Mul(anchor).Div(decimal.NewFromFloat(f)).Div(bpsPerUnit)
}

var errFlatValuation = errors.New("strategy: the close's valuation does not move with its price")

// breakEven is the worst price at the tick for a close of qty on leg alone,
// an order in direction dir, at which value(purpose, ...) reaches need: a
// sell's lowest price, rounded up, a buy's highest, rounded down. The
// valuation of a fixed quantity is linear in its price, so two valuations
// give the line; the rounded price is valued once more and moved one tick
// if the rounding left it short (SPEC 12.0 "速度 2"). ref is any price near
// the answer.
func (s *Strategy) breakEven(value CloseValuer, purpose cex.OrderPurpose, leg cex.Leg, qty, ref, tick, need decimal.Decimal, dir Direction) (decimal.Decimal, error) {
	fee := s.feeRate()
	n0, err := value(purpose, legClose(leg, qty, ref), fee)
	if err != nil {
		return decimal.Zero, err
	}
	n1, err := value(purpose, legClose(leg, qty, ref.Add(tick)), fee)
	if err != nil {
		return decimal.Zero, err
	}
	slope := n1.Sub(n0).Div(tick)
	if slope.IsZero() {
		return decimal.Zero, errFlatValuation
	}
	sell := dir == Short
	p := roundToTick(ref.Add(need.Sub(n0).Div(slope)), tick, sell)
	n, err := value(purpose, legClose(leg, qty, p), fee)
	if err != nil {
		return decimal.Zero, err
	}
	if n.LessThan(need) {
		if sell {
			p = p.Add(tick)
		} else {
			p = p.Sub(tick)
		}
	}
	return p, nil
}
