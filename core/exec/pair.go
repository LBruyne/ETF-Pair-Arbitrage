package exec

import (
	"time"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/inventory"
	"etf-pair-arbitrage/core/logger"
	"etf-pair-arbitrage/core/strategy"
)

// The thin leg first, entry side (SPEC 12.4, PRD 5.14-5.19). An entry batch
// sends the thin leg's order alone; once it has filled, every round (a depth
// frame, an order result, a tick) runs PairStep on one snapshot of both
// books and the ledger and carries its action out, until the batch reaches
// an end state. One batch is in pairing at a time: no entry and no line
// exit while it is (holding). The stop and the forced exit take it over.

// pairState is what the loop keeps of a thin-leg-first entry batch beside
// its orders. The fills are read again from the batch's orders every round,
// so a restart rebuilds them from the order records (restorePair).
type pairState struct {
	plan        *strategy.EntryPlan // the decision; after a restart rebuilt from the lot and the thin order
	thin, thick cex.Leg
	thinID      cex.ClientOrderID // the batch's first order, the thin leg's
	started     bool              // the thin order filled: the batch is in pairing
	batch       strategy.PairBatch
	sent        [2]bool           // per leg: the batch sent an order of that leg, so the next one is a repeat (200 ms and a new frame)
	topUp       cex.ClientOrderID // the top-up sent last, until its result is in
	topUpX      float64           // x at that top-up's decision: x_ref once it filled
	noTopUps    bool              // restored after a top-up had filled: its x is not in the order records, so no further top-up
	lastWhy     strategy.Reason   // the reason of the last wait recorded: a wait is recorded when it changes
	ended       bool
	end         inventory.LotEnd
}

// activePair is the thin-leg-first entry batch in pairing, or that waits for
// its thin order's result; nil when there is none.
func (m *ExecManager) activePair() *pairState {
	if b := m.batch; b != nil && b.pair != nil && !b.pair.ended {
		return b.pair
	}
	return nil
}

// openThinBatch sends the batch's thin order alone (SPEC 12.4 step one): the
// plan's thin leg at its pricing level for qS. The batch is the last entry
// from now on, however it ends (SPEC 12.3, PRD 5.8).
func (m *ExecManager) openThinBatch(r *round, c *inventory.Cycle, lot *inventory.Lot, plan *strategy.EntryPlan) []cex.OrderRequest {
	thin := m.pairing.Thin
	o := plan.ThinOrder(thin)
	p := &pairState{plan: plan, thin: thin, thick: otherLeg(thin)}
	p.sent[thin] = true
	m.batch.pair = p
	req := m.orderRequest(c, r.orders, thin, cex.PurposeEntry, o.Side, o.Price, o.Qty, r.rules[thin].Symbol)
	req.QuotaPrice, req.LotID = plan.QuotaPrice[thin], string(lot.ID)
	p.thinID = req.ClientOrderID
	m.lastEntry = &strategy.LastEntry{At: r.now, E: plan.X, Direction: plan.Direction}
	m.lastSentAt[thin] = r.now
	return []cex.OrderRequest{req}
}

// workPair runs one round of the batch in pairing: the thin order's outcome
// first (zero or refused: entry_failed; a fill: the pairing starts with the
// round's time as the first fill's arrival), then PairStep and its action.
// It reports whether the round ends here; it does not once the batch ended,
// so that the round goes on as after any batch (the comparison of the
// positions, the close of a flat cycle).
func (m *ExecManager) workPair(r *round, p *pairState) (ended bool) {
	if !r.rulesOK {
		m.idle(strategy.ReasonNotReady)
		return true
	}
	m.refreshPair(r, p)
	if !p.started {
		thinRec, _ := orderIn(r.orders, p.thinID)
		if !p.batch.ThinQty.IsPositive() {
			m.endPair(r, p, inventory.LotEndEntryFailed, "", nil, map[string]any{"status": thinRec.Status, "error_code": thinRec.ErrorCode,
				"error_message": thinRec.ErrorMessage, "cancel_reason": thinRec.CancelReason, "client_order_id": p.thinID})
			return false
		}
		fills := p.batch.Fills
		p.batch = strategy.NewPairBatch(p.plan, p.thin, fills.ThinQty, fills.ThinVWAP, r.now)
		p.batch.Fills, p.started = fills, true
		detail := m.pairDetail(r, p, nil)
		detail["action"], detail["thin_final_ms"], detail["client_order_id"] = "fill", thinRec.FinalAt.UnixMilli(), p.thinID
		m.recordDecision(r.now, "pair", r.cycle, m.batch.lotID, p.batch.Dir, "", detail)
		logger.GlobalLogger.Info("exec: thin leg filled; pairing", "lot", m.batch.lotID, "leg", p.thin.String(), "qty", fills.ThinQty, "vwap", fills.ThinVWAP,
			"plan_qty", p.batch.PlanThin, "result_at", showTime(thinRec.FinalAt))
	}
	return m.doPair(r, p, m.strat.PairStep(m.pairInput(r, p)))
}

// refreshPair reads the batch's fills from its orders in the round's
// snapshot: S and v_S from the thin leg's entry orders (the first and the
// top-ups), L and v_L from the thick leg's, what the thin leg closed again
// from its trims (profit closes and unwinds). A top-up that has its result
// moves x_ref to its x when it filled (SPEC 12.4 step 6).
func (m *ExecManager) refreshPair(r *round, p *pairState) {
	lot := m.batch.lotID
	var thinQ, thinV, thickQ, thickV, closed decimal.Decimal
	for _, o := range r.orders {
		if inventory.LotID(o.Request.LotID) != lot && m.inv.LotOf(o.Request.ClientOrderID) != lot {
			continue
		}
		adds := o.Request.Purpose == cex.PurposeEntry || o.Request.Purpose == cex.PurposeComplete
		switch {
		case o.Request.Leg == p.thin && adds:
			thinQ, thinV = thinQ.Add(o.CumExecQty), thinV.Add(o.CumExecValue)
		case o.Request.Leg == p.thick && adds:
			thickQ, thickV = thickQ.Add(o.CumExecQty), thickV.Add(o.CumExecValue)
		case o.Request.Leg == p.thin && o.Request.Purpose == cex.PurposeTrim:
			closed = closed.Add(o.CumExecQty)
		}
	}
	p.batch.Fills = strategy.Fills{ThinQty: thinQ, ThinVWAP: vwap(thinV, thinQ), ThickQty: thickQ, ThickVWAP: vwap(thickV, thickQ)}
	p.batch.ThinClosed = closed
	if p.topUp != "" {
		if o, found := orderIn(r.orders, p.topUp); found && o.Status.Terminal() {
			if o.CumExecQty.IsPositive() {
				p.batch.XRef = p.topUpX
			}
			p.topUp = ""
		}
	}
}

// vwap is value / qty; zero without a quantity.
func vwap(value, qty decimal.Decimal) decimal.Decimal {
	if !qty.IsPositive() {
		return decimal.Zero
	}
	return value.Div(qty)
}

// pairInput gathers the facts of one PairStep: the round's books, the
// ledger's quota without the unpaired thin leg, and CloseNet bound to the
// batch's lot. While the mean warms up again the last ready mean stands in,
// and while F is stale its last valid value does, as for the exit (PRD 9.7);
// either blocks a top-up (topUpBlocked).
func (m *ExecManager) pairInput(r *round, p *pairState) strategy.PairInput {
	stats := m.stats
	if !stats.Ready {
		stats = m.lastReady
	}
	_, _, left := m.inv.Quota()
	in := strategy.PairInput{Batch: p.batch, Pairing: m.pairing, Books: r.books.ByLeg, Rules: r.rules, F: r.fx.Price.InexactFloat64(),
		Anchors: m.anchors, PairAnchors: r.cycle.PairAnchors, Stats: stats, QuotaLeft: left, Value: m.closeValuer(m.batch.lotID), Dust: m.inv.Dust(),
		Stopping: m.stop.stopping, Forced: m.forced != "", NoNewEntries: m.stop.noEntries || p.noTopUps, TopUpBlocked: m.topUpBlocked(r), Now: r.now}
	for _, leg := range cex.Legs {
		in.DepthOK[leg] = r.books.DepthOK(leg, r.now)
	}
	return in
}

// topUpBlocked reports whether what blocks a new entry now blocks a top-up
// in this round (PRD 5.10, 5.11): the checks of steps 1 to 3 of PRD 5.13
// (entryBlocked) and the warm-up. The pairing goes on. Left out are the new
// depth after the batch's last result, for which a repeat of the thin leg
// waits on its own frame (mayRepeat, SPEC 12.12 item 3), and the backoff
// after a refusal, which is about the next batch.
func (m *ExecManager) topUpBlocked(r *round) bool {
	switch m.entryBlocked(r) {
	case "", strategy.ReasonAwaitDepth, strategy.ReasonRejectedBackoff:
		return !m.stats.Ready
	}
	return true
}

// doPair carries out one PairStep action (SPEC 12.9). The first thick order
// goes in the round the thin fill is booked; any later order of a leg the
// batch has sent before waits 200 ms after it and a new frame of that leg
// after the last result (SPEC 12.12 item 3); an unwind repeats at PRD 10.4's
// price every 200 ms until the unpaired part is flat or dust.
func (m *ExecManager) doPair(r *round, p *pairState, a strategy.PairAction) (ended bool) {
	c, b := r.cycle, m.batch
	switch a.Kind {
	case strategy.PairWait:
		m.notePairWait(r, p, a)
		m.idle(a.Why)
		return true
	case strategy.PairHandOver:
		m.handOver(r)
		m.forcedExit(r, false)
		return true
	case strategy.PairDone:
		m.endPair(r, p, inventory.LotEnd(a.End), a.Why, &a, nil)
		return false
	case strategy.PairUnwind:
		if p.batch.Unwind == "" { // the risk exit begins: recorded once
			p.batch.Unwind = a.Why
			detail := m.pairDetail(r, p, &a)
			detail["action"] = "risk_exit"
			m.recordDecision(r.now, "pair", c, b.lotID, p.batch.Dir, a.Why, detail)
			logger.GlobalLogger.Info("exec: pairing ends; the unpaired thin leg is unwound", "lot", b.lotID, "reason", string(a.Why), "d_ideal", a.DIdeal, "d_dec", p.batch.DecisionD)
		}
		if !a.Qty.IsPositive() || !r.books.HasPrice(p.thin, r.now) { // no order without a recent price (PRD 13.6)
			m.idle(strategy.ReasonNoPrice)
			return true
		}
		if !m.maySend(r, p.thin, a.Price) {
			return true
		}
	case strategy.PairSendThick:
		if p.sent[p.thick] && !m.mayRepeat(r, p.thick, a.Price) {
			return true
		}
	case strategy.PairSendThin, strategy.PairCloseThin:
		if !m.mayRepeat(r, p.thin, a.Price) {
			return true
		}
	}

	purpose := cex.PurposeEntry
	switch a.Kind {
	case strategy.PairSendThick:
		purpose = cex.PurposeComplete
	case strategy.PairCloseThin, strategy.PairUnwind:
		purpose = cex.PurposeTrim
	}
	req := m.orderRequest(c, r.orders, a.Leg, purpose, a.Side, a.Price, a.Qty, r.rules[a.Leg].Symbol)
	req.LotID = string(b.lotID)
	if purpose == cex.PurposeTrim {
		req.ReduceOnly = true
	} else {
		req.QuotaPrice = a.QuotaPrice
	}
	detail := m.pairDetail(r, p, &a)
	detail["action"], detail["leg"], detail["side"], detail["price"], detail["qty"], detail["quota_price"], detail["client_order_id"] =
		string(a.Kind), a.Leg.String(), a.Side, a.Price, a.Qty, a.QuotaPrice, req.ClientOrderID
	if a.Kind == strategy.PairSendThick && !p.sent[p.thick] {
		// For the latency of the first thick order (SPEC 12.10): when the thin
		// leg's result arrived and when this order went.
		thinRec, _ := orderIn(r.orders, p.thinID)
		detail["first_thick"], detail["thin_final_ms"], detail["sent_ms"] = true, thinRec.FinalAt.UnixMilli(), r.now.UnixMilli()
	}
	if a.Kind == strategy.PairSendThin {
		p.topUp, p.topUpX = req.ClientOrderID, a.X
	}
	p.sent[a.Leg], p.lastWhy = true, ""
	m.lastSentAt[a.Leg] = r.now
	m.recordDecision(r.now, "pair", c, b.lotID, p.batch.Dir, a.Why, detail)
	logger.GlobalLogger.Info("exec: pairing", "lot", b.lotID, "action", string(a.Kind), "reason", string(a.Why), "leg", a.Leg.String(), "side", string(a.Side),
		"price", a.Price, "qty", a.Qty, "x_d", a.X, "x_ref_d", a.XRef, "d_ideal", a.DIdeal, "vwap_cap", a.VWAPCap, "since_first_fill", r.now.Sub(p.batch.FirstFill).Round(time.Millisecond))
	m.send(r, req)
	return true
}

// mayRepeat reports whether a further order of a leg the batch has sent
// before may go now (SPEC 12.12 item 3): resend after the last one, a frame
// of the leg (depth or best price) received after the batch's last result,
// for the book is otherwise as it was before that fill, and not at a price
// the venue refused it for. Otherwise it records why not.
func (m *ExecManager) mayRepeat(r *round, leg cex.Leg, price decimal.Decimal) bool {
	if !m.maySend(r, leg, price) {
		return false
	}
	book, done := r.books.ByLeg[leg], m.batch.doneAt
	if !book.DepthReceivedAt.After(done) && !book.BestReceivedAt.After(done) {
		m.idle(strategy.ReasonAwaitDepth)
		return false
	}
	return true
}

// notePairWait records a wait of the batch when its reason changed: repeated
// waits are in the per-second samples (idle_reason), not one row a frame.
func (m *ExecManager) notePairWait(r *round, p *pairState, a strategy.PairAction) {
	if a.Why == p.lastWhy {
		return
	}
	p.lastWhy = a.Why
	detail := m.pairDetail(r, p, &a)
	detail["action"] = "wait"
	if a.Err != nil {
		detail["err"] = a.Err.Error()
	}
	m.recordDecision(r.now, "pair", r.cycle, m.batch.lotID, p.batch.Dir, a.Why, detail)
}

// endPair ends the batch in its end state: the lot is settled (SPEC 12.4,
// inventory SettlePairedLot), and the decision is recorded with the batch's
// fills and, for a thin leg left unpaired and too small to close, its dust
// (owner item 47: the leg, the quantity and the price; it stays in the lot
// and is recorded as dust when the cycle closes).
func (m *ExecManager) endPair(r *round, p *pairState, end inventory.LotEnd, why strategy.Reason, a *strategy.PairAction, extra map[string]any) {
	b := m.batch
	m.inv.SettlePairedLot(b.lotID, end, r.now)
	b.settled, p.ended, p.end = true, true, end
	r.cycle = m.inv.Cycle()
	detail := m.pairDetail(r, p, a)
	detail["action"], detail["end"] = "done", string(end)
	for k, v := range extra {
		detail[k] = v
	}
	if lot := lotOf(r.cycle, b.lotID); lot != nil && p.started {
		ratio := anchorOf(r.cycle.PairAnchors, p.thin).Div(anchorOf(r.cycle.PairAnchors, p.thick))
		if dust := lot.Legs[p.thin].Open().Sub(lot.Legs[p.thick].Open().Div(ratio)); dust.IsPositive() {
			detail["dust_leg"], detail["dust_qty"], detail["dust_price"] = p.thin.String(), dust, p.batch.ThinVWAP
		}
	}
	m.recordDecision(r.now, "pair", r.cycle, b.lotID, p.batch.Dir, why, detail)
	logger.GlobalLogger.Info("exec: batch ended", "lot", b.lotID, "end", string(end), "reason", string(why), "thin_qty", p.batch.ThinQty, "thick_qty", p.batch.ThickQty,
		"thin_closed", p.batch.ThinClosed, "sent", showTime(b.sentAt))
}

// pairDetail is the decision detail of a step of the batch (SPEC 12.9): x,
// x_ref, D_ideal, D_dec, P* (the VWAP cap) and p* (one order's bound), b*,
// and the batch's fills so far.
func (m *ExecManager) pairDetail(r *round, p *pairState, a *strategy.PairAction) map[string]any {
	b := p.batch
	detail := map[string]any{"thin_leg": p.thin.String(), "d_dec": finite(b.DecisionD), "plan_thin": b.PlanThin,
		"thin_qty": b.ThinQty, "thin_vwap": b.ThinVWAP, "thick_qty": b.ThickQty, "thick_vwap": b.ThickVWAP, "thin_closed": b.ThinClosed,
		"unwind": string(b.Unwind), "depth_seq_bull": r.books.ByLeg[cex.Bull].DepthSeq, "depth_seq_bear": r.books.ByLeg[cex.Bear].DepthSeq}
	if !b.FirstFill.IsZero() {
		detail["first_fill_ms"] = b.FirstFill.UnixMilli()
	}
	if a != nil {
		detail["x_d"], detail["x_ref_d"], detail["d_ideal"] = finite(a.X), finite(a.XRef), finite(a.DIdeal)
		detail["vwap_cap"], detail["p_star"], detail["b_star"] = a.VWAPCap, a.PStar, a.BreakEven
	}
	return detail
}

// handOver ends the batch in pairing, or the line exit batch, that the stop
// or the forced exit takes over (SPEC 12.4 step 2, 12.7 step 3): the forced
// exit then flattens both legs' actual holdings. The entry batch ends in the
// state its fills say now (entry_failed when its thin order filled nothing;
// built_partial or unwound_stop while part of its thin leg is unpaired,
// strategy.PairBatch.HandOverEnd), so that its lot is settled before the
// cycle closes.
func (m *ExecManager) handOver(r *round) {
	why := strategy.ReasonForced
	if m.stop.stopping {
		why = strategy.ReasonStopping
	}
	if p := m.activePair(); p != nil && r.cycle != nil {
		m.refreshPair(r, p)
		end := inventory.LotEndEntryFailed
		if p.batch.ThinQty.IsPositive() {
			end = inventory.LotEnd(p.batch.HandOverEnd(p.thin, r.cycle.PairAnchors, r.rules[p.thick].QtyStep))
		}
		m.endPair(r, p, end, why, nil, map[string]any{"hand_over": true})
	}
	if e := m.activeExit(); e != nil && r.cycle != nil {
		m.refreshExit(r, e)
		m.endExit(r, e, why, "hand_over")
	}
}

// closeValuer is CloseNet of the open cycle with lot bound (SPEC 12.2): the
// strategy's valuation of a close not sent yet. An exit's valuation needs no
// lot.
func (m *ExecManager) closeValuer(lot inventory.LotID) strategy.CloseValuer {
	return func(purpose cex.OrderPurpose, close [2]strategy.CloseLeg, feeRate decimal.Decimal) (decimal.Decimal, error) {
		var legs [2]inventory.CloseLeg
		for l := range legs {
			legs[l] = inventory.CloseLeg{Qty: close[l].Qty, Price: close[l].Price}
		}
		return m.inv.CloseNet(purpose, lot, legs, feeRate)
	}
}

// otherLeg is the leg that is not l.
func otherLeg(l cex.Leg) cex.Leg {
	if l == cex.Bull {
		return cex.Bear
	}
	return cex.Bull
}

// anchorOf is a's anchor of leg l.
func anchorOf(a cex.Anchors, l cex.Leg) decimal.Decimal {
	if l == cex.Bull {
		return a.Bull
	}
	return a.Bear
}
