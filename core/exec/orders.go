package exec

import (
	"context"
	"errors"
	"time"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/inventory"
	"etf-pair-arbitrage/core/logger"
	"etf-pair-arbitrage/core/strategy"
)

// --- entry ---

// openBatch opens the cycle when there is none, opens the lot, and sends the
// two entry orders. The cycle's and the lot's rows are committed before any
// order leaves, and cex commits the orders' rows before it sends them
// (PRD 13.5): when a row cannot be confirmed nothing is sent.
func (m *ExecManager) openBatch(r *round, plan *strategy.EntryPlan) {
	log := logger.GlobalLogger
	c := r.cycle
	if c == nil {
		opened, err := m.inv.OpenCycle(m.session.ID, plan.Direction, m.anchors, r.now)
		if err != nil {
			log.Warn("exec: cycle not opened; the entry is not sent", "direction", plan.Direction, "err", err)
			m.idle(strategy.ReasonStoreError)
			return
		}
		c = opened
	}
	lot, err := m.inv.OpenLot(plan, r.now)
	if err != nil {
		reason := strategy.ReasonStoreError
		if errors.Is(err, inventory.ErrCapitalPending) {
			reason = strategy.ReasonCapitalPending
		}
		log.Warn("exec: lot not opened; the entry is not sent", "cycle", c.ID, "reason", string(reason), "err", err)
		m.idle(reason)
		return
	}

	m.batch = &batchState{kind: cex.PurposeEntry, sentAt: r.now, lotID: lot.ID, direction: plan.Direction, planPrice: plan.Price, x: plan.X}
	var reqs []cex.OrderRequest
	for _, leg := range cex.Legs {
		req := m.orderRequest(c, r.orders, leg, cex.PurposeEntry, plan.Direction.Side(), plan.Price[leg], plan.Qty[leg], r.rules[leg].Symbol)
		req.QuotaPrice, req.LotID = plan.QuotaPrice[leg], string(lot.ID)
		reqs = append(reqs, req)
	}
	m.atLine = ""
	log.Info("exec: entry", "cycle", c.ID, "lot", lot.ID, "direction", plan.Direction, "x_d", plan.X, "mu_d", plan.Mu, "sigma_d", plan.Sigma,
		"fee_d", plan.FeeD, "fee_est_usdt", plan.FeeUSDT, "k_usd", plan.K, "price_bull", plan.Price[cex.Bull], "qty_bull", plan.Qty[cex.Bull],
		"price_bear", plan.Price[cex.Bear], "qty_bear", plan.Qty[cex.Bear], "anchor_kind", string(m.anchors.Kind))
	m.recordDecision(r.now, "entry", c, lot.ID, plan.Direction, "", map[string]any{
		"x_d": finite(plan.X), "mu_d": finite(plan.Mu), "sigma_d": finite(plan.Sigma), "fee_d": finite(plan.FeeD), "fee_usdt": plan.FeeUSDT, "k_usd": plan.K,
		"price_bull": plan.Price[cex.Bull], "price_bear": plan.Price[cex.Bear], "qty_bull": plan.Qty[cex.Bull], "qty_bear": plan.Qty[cex.Bear],
		"quota_price_bull": plan.QuotaPrice[cex.Bull], "quota_price_bear": plan.QuotaPrice[cex.Bear], "f": r.fx.Price,
		"anchor_kind": m.anchors.Kind, "anchor_bull": m.anchors.Bull, "anchor_bear": m.anchors.Bear, "pair_anchor_bull": c.PairAnchors.Bull, "pair_anchor_bear": c.PairAnchors.Bear,
		"pricing_level_bull": m.scfg.PricingLevel.Bull, "pricing_level_bear": m.scfg.PricingLevel.Bear,
		"depth_seq_bull": r.books.ByLeg[cex.Bull].DepthSeq, "depth_seq_bear": r.books.ByLeg[cex.Bear].DepthSeq, "client_order_ids": ids(reqs)})
	m.send(r, reqs...)
}

// settle ends the building of the entry batch, once, when its results are in
// and no gap order can be sent for the cycle: built when both legs filled
// (PRD 8.1), which resets the entry spacing (PRD 5.8); void otherwise, which
// does not.
func (m *ExecManager) settle(r *round) {
	b := m.batch
	if b == nil || b.kind != cex.PurposeEntry || b.settled || b.doneAt.IsZero() {
		return
	}
	b.settled = true
	lot := lotOf(r.cycle, b.lotID)
	if lot == nil || lot.Status != inventory.LotBuilding {
		return
	}
	built := lot.Legs[cex.Bull].EntryQty.IsPositive() && lot.Legs[cex.Bear].EntryQty.IsPositive()
	m.inv.SettleLot(b.lotID, built, r.now)
	if built {
		m.lastEntry = &strategy.LastEntry{At: b.sentAt, E: b.x, Direction: b.direction}
	}
	logger.GlobalLogger.Info("exec: batch settled", "lot", b.lotID, "built", built, "entry_qty_bull", lot.Legs[cex.Bull].EntryQty, "entry_qty_bear", lot.Legs[cex.Bear].EntryQty,
		"sent", showTime(b.sentAt), "e_d", b.x)
	r.cycle = m.inv.Cycle()
}

// --- the gap ---

// sendGap sends the one order strategy chose for the cycle's gap. While the
// entry batch is being built it is a completion, which opens and counts
// against the quota at its quota price, or a trim, booked on that batch's
// lot (PRD 7.4). After an exit the larger leg is reduced further as an exit,
// first in first out (PRD 8.7). The order joins the batch: the next
// decision waits for its result, and the completion window keeps the
// batch's first send as its start. A trim is reduce-only.
func (m *ExecManager) sendGap(r *round, a strategy.GapAction, byPositions bool) {
	c, b := r.cycle, m.batch
	purpose, lot := cex.PurposeExit, inventory.LotID("")
	if b != nil && b.kind == cex.PurposeEntry && !b.settled {
		purpose, lot = cex.PurposeTrim, b.lotID
		if a.Kind == strategy.GapComplete {
			purpose = cex.PurposeComplete
		}
	}
	if b == nil { // a cycle this loop did not open: its gap is only reduced
		m.batch = &batchState{kind: cex.PurposeExit, sentAt: r.now}
	}
	req := m.orderRequest(c, r.orders, a.Leg, purpose, a.Side, a.Price, a.Qty, r.rules[a.Leg].Symbol)
	req.LotID = string(lot)
	if a.Kind == strategy.GapComplete {
		req.QuotaPrice = a.QuotaPrice
	} else {
		req.ReduceOnly = true
	}
	m.lastSentAt[a.Leg] = r.now
	hold := c.Hold()
	_, _, left := m.inv.Quota()
	logger.GlobalLogger.Info("exec: gap", "by_positions", byPositions, "cycle", c.ID, "lot", lot, "order", string(a.Kind), "purpose", string(purpose), "leg", a.Leg.String(), "side", string(a.Side),
		"price", a.Price, "qty", a.Qty, "loss_complete_usdt", a.LossComplete, "loss_trim_usdt", a.LossTrim, "hold_bull", hold[cex.Bull], "hold_bear", hold[cex.Bear],
		"quota_left_usdt", left, "since_batch_sent", r.now.Sub(m.batch.sentAt).Round(time.Millisecond))
	m.recordDecision(r.now, string(a.Kind), c, lot, c.Direction, "", map[string]any{
		"purpose": purpose, "by_positions": byPositions, "leg": a.Leg.String(), "side": a.Side, "price": a.Price, "qty": a.Qty, "quota_price": a.QuotaPrice,
		"loss_complete_usdt": a.LossComplete, "loss_trim_usdt": a.LossTrim, "hold_bull": hold[cex.Bull], "hold_bear": hold[cex.Bear],
		"quota_left": left, "batch_sent_ms": m.batch.sentAt.UnixMilli(), "pair_anchor_bull": c.PairAnchors.Bull, "pair_anchor_bear": c.PairAnchors.Bear,
		"depth_seq_bull": r.books.ByLeg[cex.Bull].DepthSeq, "depth_seq_bear": r.books.ByLeg[cex.Bear].DepthSeq, "client_order_id": req.ClientOrderID})
	m.send(r, req)
}

// --- exit at the line ---

// sendExit sends the pair of reduce-only exit orders of plan; a leg the plan
// gives nothing gets no order (PRD 9.6 as narrowed: its remainder is dust).
func (m *ExecManager) sendExit(r *round, plan *strategy.ExitPlan, in strategy.ExitInput) {
	c := r.cycle
	m.batch = &batchState{kind: cex.PurposeExit, sentAt: r.now}
	var reqs []cex.OrderRequest
	for _, leg := range cex.Legs {
		if plan.Qty[leg].IsPositive() {
			req := m.orderRequest(c, r.orders, leg, cex.PurposeExit, plan.Side, plan.Price[leg], plan.Qty[leg], r.rules[leg].Symbol)
			req.ReduceOnly = true
			reqs = append(reqs, req)
		}
	}
	statsStale := !m.stats.Ready
	logger.GlobalLogger.Info("exec: exit", "cycle", c.ID, "direction", c.Direction, "x_d", plan.X, "mu_d", plan.Mu, "sigma_d", plan.Sigma,
		"price_bull", plan.Price[cex.Bull], "qty_bull", plan.Qty[cex.Bull], "price_bear", plan.Price[cex.Bear], "qty_bear", plan.Qty[cex.Bear],
		"hold_bull", in.Hold[cex.Bull], "hold_bear", in.Hold[cex.Bear], "stats_stale", statsStale, "stats_as_of", showTime(in.Stats.AsOf), "fx_fresh", r.fxFresh)
	m.recordDecision(r.now, "exit", c, "", c.Direction, "", map[string]any{
		"x_d": finite(plan.X), "mu_d": finite(plan.Mu), "sigma_d": finite(plan.Sigma),
		"price_bull": plan.Price[cex.Bull], "price_bear": plan.Price[cex.Bear], "qty_bull": plan.Qty[cex.Bull], "qty_bear": plan.Qty[cex.Bear],
		"hold_bull": in.Hold[cex.Bull], "hold_bear": in.Hold[cex.Bear],
		// PRD 9.7: an exit on the last ready mean, or on the last valid F, says so.
		"stats_stale": statsStale, "stats_as_of_ms": in.Stats.AsOf.UnixMilli(), "f": r.fx.Price, "f_fresh": r.fxFresh, "f_received_ms": r.fx.ReceivedAt.UnixMilli(),
		"anchor_kind": m.anchors.Kind, "anchor_bull": m.anchors.Bull, "anchor_bear": m.anchors.Bear, "pair_anchor_bull": c.PairAnchors.Bull, "pair_anchor_bear": c.PairAnchors.Bear,
		"depth_seq_bull": r.books.ByLeg[cex.Bull].DepthSeq, "depth_seq_bear": r.books.ByLeg[cex.Bear].DepthSeq, "client_order_ids": ids(reqs)})
	m.send(r, reqs...)
}

// --- forced exit ---

// forcedDue reports whether the open cycle is to be closed whatever the line
// says (PRD 10.1): from forced_exit_before_close before the Session's close,
// when the cycle belongs to a Session that has ended, or after a stop
// signal. Once it holds it holds until the cycle is closed (PRD 10.5); the
// first cause is kept as the cycle's end reason. A cycle that holds nothing
// any more only waits to be confirmed flat (holding is false): it does not
// begin a forced exit and keeps the end reason of what closed it.
func (m *ExecManager) forcedDue(c *inventory.Cycle, now time.Time, holding bool) bool {
	if m.forced != "" {
		return true
	}
	if !holding {
		return false
	}
	switch {
	case m.stop.stopping:
		m.forced = inventory.EndStop
	case c.SessionID != m.session.ID:
		m.forced = inventory.EndSessionChanged
	case !now.Before(m.session.End.Add(-m.scfg.ForcedExitBeforeClose)):
		m.forced = inventory.EndCloseWindow
	default:
		return false
	}
	hold := c.Hold()
	logger.GlobalLogger.Info("exec: forced exit begins", "cycle", c.ID, "cause", m.forced, "cycle_session", c.SessionID, "session", m.session.ID,
		"session_end", showTime(m.session.End), "direction", c.Direction, "hold_bull", hold[cex.Bull], "hold_bear", hold[cex.Bear])
	return true
}

// forcedExit closes everything the cycle holds with one reduce-only order
// per leg, through the best price by forced_slip_bps, regardless of the line
// and of the book's depth (PRD 10.2, 10.4); a conflict does not hold it up.
// While a leg that holds something has no price, neither leg is sent, so the
// pair stays hedged (PRD 10.4, 13.6). A leg is sent again on the new book
// once its last order has a result, at least resend after the last one,
// and not at a price the venue refused it for. What cannot be sent at all
// is dust, and the cycle then closes with it (PRD 8.1). byPositions sizes
// the orders by the venue's positions instead of the ledger: an order's
// outcome is unknown and the ledger may lack its fill (PRD 13.3); when the
// round had to read them it ends there, and the next round prices the orders
// on its own books.
func (m *ExecManager) forcedExit(r *round, byPositions bool) (done bool) {
	log := logger.GlobalLogger
	c := r.cycle
	if c.SessionID != m.session.ID && !m.lateLogged {
		m.lateLogged = true
		hold := c.Hold()
		log.Error("exec: late exit past the close", "cycle", c.ID, "cycle_session", c.SessionID, "session", m.session.ID, "hold_bull", hold[cex.Bull], "hold_bear", hold[cex.Bear])
		m.event(r.now, "error", "late_exit", "the cycle of an ended Session is still open: it is being closed, entries wait until it is",
			map[string]any{"cycle_id": c.ID, "cycle_session": c.SessionID, "session_id": m.session.ID, "hold_bull": hold[cex.Bull], "hold_bear": hold[cex.Bear]})
	}
	if !r.rulesOK {
		return m.idle(strategy.ReasonNotReady)
	}
	hold := c.Hold()
	if byPositions {
		pos := m.venuePositions(r)
		if !pos.Loaded {
			return m.idle(strategy.ReasonNotReady)
		}
		if hold = m.positionHold(r, c.Direction, pos); hold[cex.Bull].IsZero() && hold[cex.Bear].IsZero() {
			return m.idle(strategy.ReasonOrderUnknown) // nothing at the venue to reduce: the outcome decides
		}
		if r.read {
			return m.idle(strategy.ReasonOrderUnknown) // the quantity is known; the prices come from the next round's books
		}
	} else if c.Flat() {
		if !m.closeIfFlat(r) { // closed this round or not, nothing is left to send
			m.idle(strategy.ReasonAwaitClose)
		}
		return false
	}
	for _, leg := range cex.Legs {
		if hold[leg].IsPositive() && !r.books.HasPrice(leg, r.now) {
			return m.idle(strategy.ReasonNoPrice)
		}
	}
	orders := m.strat.ForcedOrders(c.Direction, hold, r.books.ByLeg, r.rules)
	if orders[cex.Bull] == nil && orders[cex.Bear] == nil {
		if !m.closeIfFlat(r) { // only dust is left
			m.idle(strategy.ReasonAwaitClose)
		}
		return false
	}

	var reqs []cex.OrderRequest
	for _, leg := range cex.Legs {
		o := orders[leg]
		if o == nil || !m.maySend(r, leg, o.Price) {
			continue
		}
		req := m.orderRequest(c, r.orders, leg, cex.PurposeForced, o.Side, o.Price, o.Qty, r.rules[leg].Symbol)
		req.ReduceOnly = true
		reqs = append(reqs, req)
		m.lastSentAt[leg] = r.now
	}
	if len(reqs) == 0 {
		return false // maySend recorded why
	}
	m.idleReason = ""
	if m.batch == nil || m.batch.kind != cex.PurposeForced {
		m.batch = &batchState{kind: cex.PurposeForced, sentAt: r.now}
	}
	bull, bear := r.books.ByLeg[cex.Bull], r.books.ByLeg[cex.Bear]
	log.Info("exec: forced exit", "cycle", c.ID, "cause", m.forced, "by_positions", byPositions, "orders", len(reqs), "hold_bull", hold[cex.Bull], "hold_bear", hold[cex.Bear],
		"best_bid_bull", bull.BestBid.Price, "best_ask_bull", bull.BestAsk.Price, "best_bid_bear", bear.BestBid.Price, "best_ask_bear", bear.BestAsk.Price)
	detail := map[string]any{"cause": m.forced, "by_positions": byPositions, "hold_bull": hold[cex.Bull], "hold_bear": hold[cex.Bear], "slip_bps": m.scfg.ForcedSlipBps,
		"best_bid_bull": bull.BestBid.Price, "best_ask_bull": bull.BestAsk.Price, "best_bid_bear": bear.BestBid.Price, "best_ask_bear": bear.BestAsk.Price,
		"conflict_bull": bull.Conflict, "conflict_bear": bear.Conflict, "client_order_ids": ids(reqs)}
	for _, req := range reqs {
		detail["price_"+req.Leg.String()], detail["qty_"+req.Leg.String()] = req.Price, req.Qty
	}
	m.recordDecision(r.now, "forced", c, "", c.Direction, "", detail)
	m.send(r, reqs...)
	return false
}

// --- outcomes unknown ---

// whileUnknown is the round while an order's outcome is unknown (PRD 13.3):
// no entry and no exit at the line. cex queries the order; the loop alarms
// after unknownAlarm and gives the verdict "never arrived" once the venue
// has no such order, no fill carries its id (cex's NotFound) and the leg's
// position at the venue is the ledger's. A forced exit due meanwhile is
// sized by the venue's positions; past unknownAlarm the other leg's gap is
// trimmed by the venue's positions too. What the positions show is never
// booked: the ledger follows the order reports alone. A round that read the
// positions sends nothing after the read: the next one prices on its books.
func (m *ExecManager) whileUnknown(r *round, unknown []cex.OrderRecord) bool {
	m.chase(r, unknown)
	if c := r.cycle; c != nil && m.forcedDue(c, r.now, true) {
		return m.forcedExit(r, true)
	}
	if m.unknownFor(r.now) >= m.timing.unknownAlarm && m.gapByPositions(r) {
		return false
	}
	return m.idle(strategy.ReasonOrderUnknown)
}

// chase follows each order of unknown outcome: the 30 s alarm, once, and
// the verdict of PRD 13.3 when its three evidences are in. The third, the
// leg's position equal to the ledger's, must come from a read of the
// positions that succeeded in or after the round that first saw cex's
// NotFound: a snapshot from before it may predate a fill, and a read that
// failed proves nothing, so the order stays unknown (and is never sent
// again) until such a read agrees.
func (m *ExecManager) chase(r *round, unknown []cex.OrderRecord) {
	log := logger.GlobalLogger
	held := map[cex.ClientOrderID]bool{}
	for _, o := range unknown {
		id := o.Request.ClientOrderID
		held[id] = true
		u := m.unknown[id]
		if u == nil {
			u = &unknownOrder{since: r.now}
			m.unknown[id] = u
		}
		if !u.alarmed && r.now.Sub(u.since) >= m.timing.unknownAlarm {
			u.alarmed = true
			log.Error("exec: order outcome unknown for 30 s; using positions", "client_order_id", id, "leg", o.Request.Leg.String(), "purpose", string(o.Request.Purpose),
				"qty", o.Request.Qty, "price", o.Request.Price, "unknown_since", showTime(u.since), "err", o.ErrorMessage)
			m.event(r.now, "error", "order_unknown_30s", "an order's outcome has been unknown for 30 s: it is queried on and never sent again; the other leg follows the venue's positions",
				map[string]any{"client_order_id": id, "leg": o.Request.Leg.String(), "purpose": o.Request.Purpose, "qty": o.Request.Qty, "price": o.Request.Price})
		}
		if !o.NotFound {
			continue
		}
		if u.notFoundAt.IsZero() {
			u.notFoundAt = r.now
		}
		if !r.now.Before(m.nextPositionsRead) {
			m.readPositions(r)
		}
		if m.readAt.Before(u.notFoundAt) {
			continue // no read since NotFound succeeded: no verdict
		}
		venue, ledger := m.cex.Positions().BySymbol[o.Request.Symbol].Qty, m.inv.LedgerPositions()[o.Request.Leg]
		if !venue.Equal(ledger) {
			continue // the position may hold its fill: no verdict
		}
		if !m.cex.MarkNeverArrived(id) {
			continue // it got a final state since the snapshot
		}
		log.Error("exec: order taken as never arrived", "client_order_id", id, "leg", o.Request.Leg.String(), "position", venue)
		m.event(r.now, "error", "order_never_arrived", "the venue has no such order, no fill carries its id and the position did not move: it never arrived; a fill of it later is a mismatch",
			map[string]any{"client_order_id": id, "leg": o.Request.Leg.String(), "purpose": o.Request.Purpose, "qty": o.Request.Qty, "position": venue})
	}
	for id := range m.unknown {
		if !held[id] {
			delete(m.unknown, id)
		}
	}
}

// unknownFor is how long the oldest unknown outcome has been unknown.
func (m *ExecManager) unknownFor(now time.Time) (longest time.Duration) {
	for _, u := range m.unknown {
		longest = max(longest, now.Sub(u.since))
	}
	return longest
}

// gapByPositions trims the gap the venue's positions show, as strategy
// decides it for a gap that is only reduced; it reports whether an order
// was sent.
func (m *ExecManager) gapByPositions(r *round) bool {
	if !r.rulesOK {
		return false
	}
	pos := m.venuePositions(r)
	if !pos.Loaded || r.read {
		return false // a round that read the positions prices nothing on its older books
	}
	in := m.gapInput(r)
	in.Hold, in.Batch = m.positionHold(r, r.cycle.Direction, pos), nil
	a := m.strat.EvaluateGap(in)
	if a.Kind != strategy.GapTrim || !m.maySend(r, a.Leg, a.Price) {
		return false
	}
	m.sendGap(r, a, true)
	return true
}

// venuePositions is the venue's positions of the two legs, read again when
// the last read is positionsRead old; between reads the published snapshot
// serves (pushes keep it current). A snapshot older than the latest final
// state among the cycle's orders is read again at once: the push of the
// positions can come after the order's report, and a gap sized on the older
// snapshot would be reduced twice. One still older after that read is not
// used this round (Loaded is false). A read ends the round (readPositions).
func (m *ExecManager) venuePositions(r *round) cex.PositionSet {
	last := lastFinal(r.orders)
	if !r.now.Before(m.nextPositionsRead) || m.cex.Positions().ReceivedAt.Before(last) {
		m.readPositions(r)
	}
	pos := m.cex.Positions()
	if pos.ReceivedAt.Before(last) {
		pos.Loaded = false
	}
	return pos
}

// readPositions reads the venue's positions now, which cex publishes; the
// next read for an unknown outcome waits positionsRead after it returned. The round sends
// nothing after it (round.read). The round of a read that succeeds is kept
// for the verdict "never arrived".
func (m *ExecManager) readPositions(r *round) {
	r.read = true
	ctx, cancel := context.WithTimeout(m.ctx, m.timing.readTimeout)
	_, err := m.cex.ReadPositions(ctx)
	cancel()
	m.nextPositionsRead = m.timing.clock().Add(m.timing.positionsRead) // from the read's return: a slow read does not fill the loop's time
	if err != nil {
		logger.GlobalLogger.Debug("exec: positions not read", "err", err)
		return
	}
	m.readAt = r.now
}

// lastFinal is when the latest final state among the orders arrived, on
// cex's clock.
func lastFinal(orders []cex.OrderRecord) (last time.Time) {
	for _, o := range orders {
		if o.FinalAt.After(last) {
			last = o.FinalAt
		}
	}
	return last
}

// positionHold is what the venue's positions hold of a cycle in direction
// dir, per leg: the net position less the ledger's dust, which is not the
// cycle's (both signed), with a short cycle's negative positions counted
// positive. The forced exit and the gap by positions both size on it.
func (m *ExecManager) positionHold(r *round, dir strategy.Direction, pos cex.PositionSet) (hold [2]decimal.Decimal) {
	dust := m.inv.Dust()
	for _, leg := range cex.Legs {
		q := pos.BySymbol[r.rules[leg].Symbol].Qty.Sub(dust[leg])
		if dir == strategy.Short {
			q = q.Neg()
		}
		hold[leg] = decimal.Max(q, decimal.Zero)
	}
	return hold
}

// --- closing the cycle ---

// closeIfFlat closes the open cycle when nothing of it can be traded any
// more: both legs hold nothing, or only what no order could be sent for.
// Its orders must all have a final state and every filled one a confirmed
// fee, and one read of the venue's positions must agree with the ledger.
// An attempt whose read fails or differs is tried again when the next
// comparison is due (compare's backoff); one whose close is not written,
// closeRetry later: every read of the positions publishes them and wakes the
// loop, which would otherwise read them again at once. What is left is recorded as dust by the ledger (PRD 8.1). It reports
// whether it read the positions: the round then ends, with the reason
// positions_read when the cycle closed and await_close when it did not.
func (m *ExecManager) closeIfFlat(r *round) (read bool) {
	log := logger.GlobalLogger
	c := r.cycle
	if c == nil || (!c.Flat() && !m.onlyDust(r)) || !m.inv.Settled() || r.now.Before(m.nextCloseTry) {
		return false
	}
	err := m.compare(m.ctx, r.now)
	r.read = true
	if err != nil {
		m.nextCloseTry = m.nextReconcile // the comparison's own backoff: a difference again soon, a failed read later
		log.Debug("exec: cycle not closed: the venue's positions do not confirm it yet", "cycle", c.ID, "err", err, "retry_at", showTime(m.nextCloseTry))
		m.idle(strategy.ReasonAwaitClose)
		return true
	}
	reason := m.endReason(c)
	if err := m.inv.CloseCycle(reason, r.now); err != nil {
		m.nextCloseTry = r.now.Add(m.timing.closeRetry)
		log.Debug("exec: cycle not closed yet", "cycle", c.ID, "reason", reason, "err", err, "retry_at", showTime(m.nextCloseTry))
		m.idle(strategy.ReasonAwaitClose)
		return true
	}
	m.forced, m.lateLogged, m.nextCloseTry = "", false, time.Time{}
	r.cycle, r.orders = nil, nil
	m.idle(strategy.ReasonPositionsRead)
	return true
}

// onlyDust reports whether what the open cycle still holds is all dust: no
// leg's remainder could be sent as a reducing order at its current price.
// A leg without a price cannot be judged, and is taken as tradable.
func (m *ExecManager) onlyDust(r *round) bool {
	if !r.rulesOK {
		return false
	}
	c, hold := r.cycle, r.cycle.Hold()
	for _, leg := range cex.Legs {
		if hold[leg].IsPositive() && !r.books.HasPrice(leg, r.now) {
			return false
		}
	}
	orders := m.strat.ForcedOrders(c.Direction, hold, r.books.ByLeg, r.rules)
	return orders[cex.Bull] == nil && orders[cex.Bear] == nil
}

// endReason is why the cycle ends: no_fill or one_leg when none of its lots
// was built, else the cause of the forced exit in progress, else the exit
// line.
func (m *ExecManager) endReason(c *inventory.Cycle) string {
	allVoid, oneLeg := true, false
	for _, lot := range c.Lots {
		switch {
		case lot.Status != inventory.LotVoid:
			allVoid = false
		case lot.VoidReason == inventory.VoidOneLeg:
			oneLeg = true
		}
	}
	switch {
	case allVoid && oneLeg:
		return inventory.EndOneLeg
	case allVoid:
		return inventory.EndNoFill
	case m.forced != "":
		return m.forced
	}
	return inventory.EndExitLine
}

// --- sending ---

// orderRequest builds one IOC limit order of the cycle with the next order
// id of the cycle. The first order the loop sends in a cycle continues after
// the highest sequence number among the cycle's orders: the cycle may have
// been opened by an earlier process, and an id used twice would replace the
// earlier order's record. The caller sets what is particular to its purpose:
// the lot and the quota price of an entry, reduce-only of a closing order.
func (m *ExecManager) orderRequest(c *inventory.Cycle, orders []cex.OrderRecord, leg cex.Leg, purpose cex.OrderPurpose, side cex.Side, price, qty decimal.Decimal, symbol cex.Symbol) cex.OrderRequest {
	if m.seqCycle != c.ID {
		m.seqCycle, m.orderSeq = c.ID, 0
		for _, o := range orders {
			if seq, ok := cex.ClientOrderSeq(o.Request.ClientOrderID); ok && seq > m.orderSeq {
				m.orderSeq = seq
			}
		}
	}
	m.orderSeq++
	return cex.OrderRequest{ClientOrderID: cex.NewClientOrderID(c.OpenedAt, m.orderSeq, leg, purpose), Symbol: symbol, Leg: leg, Side: side,
		Price: price, Qty: qty, Purpose: purpose, CycleID: string(c.ID)}
}

// send hands the orders to cex, which commits their pending rows before
// sending each. When the rows cannot be confirmed and the orders may not go
// without them, cex ends them as not_sent; they stay in the batch, whose
// results are then in at once. The quota they take counts from the next
// order snapshot on.
func (m *ExecManager) send(r *round, reqs ...cex.OrderRequest) {
	if err := m.cex.PlaceOrders(reqs...); err != nil {
		logger.GlobalLogger.Warn("exec: orders not sent: their rows were not written", "kind", string(m.batch.kind), "orders", ids(reqs), "err", err)
		m.event(r.now, "warn", "store_error", "the orders' rows were not confirmed written, so the orders were not sent",
			map[string]any{"kind": m.batch.kind, "client_order_ids": ids(reqs)})
	}
	for _, req := range reqs {
		m.batch.orders = append(m.batch.orders, req.ClientOrderID)
	}
	m.batch.doneAt, m.batch.rejected, m.batch.compared = time.Time{}, false, false
}

// ids lists the orders' client ids.
func ids(reqs []cex.OrderRequest) []cex.ClientOrderID {
	out := make([]cex.ClientOrderID, len(reqs))
	for i, req := range reqs {
		out[i] = req.ClientOrderID
	}
	return out
}
