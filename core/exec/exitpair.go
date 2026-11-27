package exec

import (
	"math"
	"time"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/inventory"
	"etf-pair-arbitrage/core/logger"
	"etf-pair-arbitrage/core/store"
	"etf-pair-arbitrage/core/strategy"
)

// The thin leg first, exit side (SPEC 12.7, PRD 9.12), and the half target
// of the two-stage exit (SPEC 12.6, PRD 9.11). A line exit batch sends the
// thin leg's reduce-only order alone; once it filled, every round runs
// ExitPairStep and carries it out until the thick leg matches the thin
// leg's closing fills, counted over the round (strategy.ExitThickTarget),
// or only dust is left. No exit decision is taken while the batch is open.

// exitState is what the loop keeps of a thin-leg-first line exit batch
// beside its orders.
type exitState struct {
	thinID  cex.ClientOrderID // the batch's thin order
	started bool              // the thin order filled: its thick leg follows
	batch   strategy.ExitBatch
	sent    [2]bool            // per leg: the batch sent an order of that leg, so the next one is a repeat
	base    [2]decimal.Decimal // the half target's closed quantities when the batch began (exit1)
	lastWhy strategy.Reason    // the reason of the last wait recorded
	ended   bool
}

// activeExit is the thin-leg-first line exit batch still open; nil when
// there is none.
func (m *ExecManager) activeExit() *exitState {
	if b := m.batch; b != nil && b.exit != nil && !b.exit.ended {
		return b.exit
	}
	return nil
}

// sendThinExit sends the thin leg's reduce-only order of plan alone (SPEC
// 12.7 step 1), at its pricing level for the batch's thin quantity; the
// batch keeps the decision's spread, the pre-check and, with D1, the thick
// leg's protection price of this round.
func (m *ExecManager) sendThinExit(r *round, plan *strategy.ExitPlan, in strategy.ExitInput) {
	c, thin := r.cycle, m.pairing.Thin
	e := &exitState{batch: strategy.NewExitBatch(c.Direction, plan), base: m.stageOf(c).Closed}
	e.sent[thin] = true
	m.batch = &batchState{kind: cex.PurposeExit, sentAt: r.now, line: plan.Line, exit: e}
	req := m.orderRequest(c, r.orders, thin, cex.PurposeExit, plan.Side, plan.Price[thin], plan.Qty[thin], r.rules[thin].Symbol)
	req.ReduceOnly = true
	e.thinID = req.ClientOrderID
	m.lastSentAt[thin] = r.now
	m.logExit(r, plan, in)
	detail := m.exitDetail(r, plan, in, []cex.OrderRequest{req})
	detail["thin_leg"] = thin.String()
	m.recordDecision(r.now, "exit", c, "", c.Direction, exitReason(plan), detail)
	m.send(r, req)
}

// exitReason is the reason an exit decision is recorded with: d1_loss for a
// batch whose pre-check failed and that exits all the same (D1).
func exitReason(plan *strategy.ExitPlan) strategy.Reason {
	if plan.D1 {
		return strategy.ReasonD1Loss
	}
	return ""
}

// workExitPair runs one round of the line exit batch: the thin order's
// outcome first (nothing filled: the batch is over and the next frame
// decides again; a fill: exit_pair_window starts at this round, not reset by
// later fills), then ExitPairStep and its action. It reports whether the
// round ends here; it does not once the batch ended.
func (m *ExecManager) workExitPair(r *round, e *exitState) (ended bool) {
	if !r.rulesOK {
		m.idle(strategy.ReasonNotReady)
		return true
	}
	m.refreshExit(r, e)
	if !e.started {
		if !e.batch.ThinQty.IsPositive() {
			m.endExit(r, e, "", "no_fill")
			return false
		}
		e.started, e.batch.FirstFill = true, r.now
		thinRec, _ := orderIn(r.orders, e.thinID)
		detail := m.exitPairDetail(r, e, nil)
		detail["action"], detail["thin_final_ms"], detail["client_order_id"] = "fill", thinRec.FinalAt.UnixMilli(), e.thinID
		m.recordDecision(r.now, "exit_pair", r.cycle, "", r.cycle.Direction, "", detail)
	}
	m.countExitStage(r, e)

	in := strategy.ExitPairInput{Batch: e.batch, Pairing: m.pairing, Hold: m.reducibleHold(r.cycle), Before: closedBefore(r.orders, e.thinID), Books: r.books.ByLeg, Rules: r.rules, F: r.fx.Price.InexactFloat64(),
		Anchors: m.anchors, PairAnchors: r.cycle.PairAnchors, Value: m.closeValuer(""), Stopping: m.stop.stopping, Forced: m.forced != "", Now: r.now}
	for _, leg := range cex.Legs {
		in.DepthOK[leg] = r.books.DepthOK(leg, r.now)
	}
	a := m.strat.ExitPairStep(in)
	thick := otherLeg(m.pairing.Thin)
	switch a.Kind {
	case strategy.ExitPairWait:
		if a.Why != e.lastWhy {
			e.lastWhy = a.Why
			detail := m.exitPairDetail(r, e, &a)
			detail["action"] = "wait"
			if a.Err != nil {
				detail["err"] = a.Err.Error()
			}
			m.recordDecision(r.now, "exit_pair", r.cycle, "", r.cycle.Direction, a.Why, detail)
		}
		m.idle(a.Why)
		return true
	case strategy.ExitPairHandOver:
		m.handOver(r)
		m.forcedExit(r, false)
		return true
	case strategy.ExitPairDone:
		m.endExit(r, e, e.batch.HardEnd, "done")
		return false
	case strategy.ExitPairHardClose:
		if e.batch.HardEnd == "" { // D2 begins: recorded once
			e.batch.HardEnd = a.Why
			detail := m.exitPairDetail(r, e, &a)
			detail["action"] = "risk_exit"
			m.recordDecision(r.now, "exit_pair", r.cycle, "", r.cycle.Direction, a.Why, detail)
			logger.GlobalLogger.Info("exec: the exit's thick leg closes whatever its bound (D2)", "cycle", r.cycle.ID, "reason", string(a.Why), "d_ideal", a.DIdeal, "d_dec", e.batch.DecisionD)
		}
		if !a.Qty.IsPositive() || !r.books.HasPrice(thick, r.now) { // no order without a recent price (PRD 13.6)
			m.idle(strategy.ReasonNoPrice)
			return true
		}
		if !m.maySend(r, thick, a.Price) {
			return true
		}
	case strategy.ExitPairSendThick:
		if e.sent[thick] && !m.mayRepeat(r, thick, a.Price) {
			return true
		}
	}
	req := m.orderRequest(r.cycle, r.orders, thick, cex.PurposeExit, a.Side, a.Price, a.Qty, r.rules[thick].Symbol)
	req.ReduceOnly = true
	e.sent[thick], e.lastWhy = true, ""
	m.lastSentAt[thick] = r.now
	detail := m.exitPairDetail(r, e, &a)
	detail["action"], detail["side"], detail["price"], detail["qty"], detail["client_order_id"] = string(a.Kind), a.Side, a.Price, a.Qty, req.ClientOrderID
	m.recordDecision(r.now, "exit_pair", r.cycle, "", r.cycle.Direction, a.Why, detail)
	logger.GlobalLogger.Info("exec: exit thick leg", "cycle", r.cycle.ID, "action", string(a.Kind), "reason", string(a.Why), "price", a.Price, "qty", a.Qty,
		"bound", a.Bound, "d_ideal", a.DIdeal, "since_first_fill", r.now.Sub(e.batch.FirstFill).Round(time.Millisecond))
	m.send(r, req)
	return true
}

// refreshExit reads the batch's closing fills from its orders: the thin
// leg's order and the thick leg's orders sent since.
func (m *ExecManager) refreshExit(r *round, e *exitState) {
	thin := m.pairing.Thin
	var q, v [2]decimal.Decimal
	for _, id := range m.batch.orders {
		if o, found := orderIn(r.orders, id); found {
			leg := o.Request.Leg
			q[leg], v[leg] = q[leg].Add(o.CumExecQty), v[leg].Add(o.CumExecValue)
		}
	}
	thick := otherLeg(thin)
	e.batch.Fills = strategy.Fills{ThinQty: q[thin], ThinVWAP: vwap(v[thin], q[thin]), ThickQty: q[thick], ThickVWAP: vwap(v[thick], q[thick])}
}

// closedBefore is what the cycle's exit and forced orders sent before the
// batch's thin order (thin) closed, per leg: the round's closing so far,
// from which the batch's thick target is counted (strategy.ExitThickTarget).
// The batch's orders are the thin one and those after it (ids grow).
func closedBefore(orders []cex.OrderRecord, thin cex.ClientOrderID) (q [2]decimal.Decimal) {
	for _, o := range orders {
		if p := o.Request.Purpose; (p == cex.PurposeExit || p == cex.PurposeForced) && o.Request.ClientOrderID < thin {
			q[o.Request.Leg] = q[o.Request.Leg].Add(o.CumExecQty)
		}
	}
	return q
}

// endExit ends the line exit batch: its fills count towards the half target
// (exit1), and the decision carries the pre-check's expected net beside the
// round's actual net after the batch (SPEC 12.7: D1's accepted loss and the
// execution's own are told apart), and the reason of a D2 hard close.
func (m *ExecManager) endExit(r *round, e *exitState, why strategy.Reason, action string) {
	if e.started {
		m.countExitStage(r, e)
	}
	e.ended = true
	detail := m.exitPairDetail(r, e, nil)
	detail["action"] = action
	if c := m.inv.Cycle(); c != nil {
		detail["actual_net"] = c.Net
	}
	m.recordDecision(r.now, "exit_pair", r.cycle, "", r.cycle.Direction, why, detail)
	logger.GlobalLogger.Info("exec: exit batch ended", "cycle", r.cycle.ID, "action", action, "reason", string(why), "thin_closed", e.batch.ThinQty, "thick_closed", e.batch.ThickQty,
		"expected_net", e.batch.ExpectedNet, "d1", e.batch.D1)
}

// exitPairDetail is the decision detail of a step of the exit batch: the
// line, the decision's spread, the pre-check's expected net, D1 and its
// protection, the thick leg's bound and the ideal closing spread, and the
// batch's fills.
func (m *ExecManager) exitPairDetail(r *round, e *exitState, a *strategy.ExitPairAction) map[string]any {
	b := e.batch
	detail := map[string]any{"thin_leg": m.pairing.Thin.String(), "line": string(b.Line), "d_dec": finite(b.DecisionD), "expected_net": b.ExpectedNet,
		"d1": b.D1, "protect": b.Protect, "hard_end": string(b.HardEnd), "thin_qty": b.ThinQty, "thin_vwap": b.ThinVWAP, "thick_qty": b.ThickQty, "thick_vwap": b.ThickVWAP,
		"depth_seq_bull": r.books.ByLeg[cex.Bull].DepthSeq, "depth_seq_bear": r.books.ByLeg[cex.Bear].DepthSeq}
	if !b.FirstFill.IsZero() {
		detail["first_fill_ms"] = b.FirstFill.UnixMilli()
	}
	if a != nil {
		detail["bound"], detail["d_ideal"] = a.Bound, finite(a.DIdeal)
	}
	return detail
}

// --- the half target of exit1 (SPEC 12.6) ---

// stageOf is the half target of cycle c; the zero value when none is set
// for it.
func (m *ExecManager) stageOf(c *inventory.Cycle) strategy.ExitStage {
	if c == nil || m.stageCycle != c.ID {
		return strategy.ExitStage{Closed: [2]decimal.Decimal{decimal.Zero, decimal.Zero}}
	}
	return m.stage
}

// setStage takes a half target DecideExit set, and stores it.
func (m *ExecManager) setStage(r *round, h strategy.ExitStage) {
	m.stage, m.stageCycle, m.stageSetAt, m.stageDoneAt = h, r.cycle.ID, r.now, time.Time{}
	logger.GlobalLogger.Info("exec: half target set at exit1", "cycle", r.cycle.ID, "target_bull", h.Target[cex.Bull], "target_bear", h.Target[cex.Bear], "built_lots", h.BuiltLots)
	m.recordStage(r.now)
}

// addStageClosed sets what the exit1 batches closed towards the half target
// to closed and stores it when it changed; H is done once its base leg is
// closed in full.
func (m *ExecManager) addStageClosed(r *round, closed [2]decimal.Decimal) {
	if !m.stage.Set || m.stageCycle != r.cycle.ID || (closed[cex.Bull].Equal(m.stage.Closed[cex.Bull]) && closed[cex.Bear].Equal(m.stage.Closed[cex.Bear])) {
		return
	}
	m.stage.Closed = closed
	if base := m.baseLeg(); m.stageDoneAt.IsZero() && !m.stage.Closed[base].LessThan(m.stage.Target[base]) {
		m.stageDoneAt = r.now
	}
	m.recordStage(r.now)
}

// countExitStage counts a thin-leg-first exit1 batch's fills towards the
// half target as they come in.
func (m *ExecManager) countExitStage(r *round, e *exitState) {
	if e.batch.Line != strategy.ExitLine1 {
		return
	}
	thin := m.pairing.Thin
	var closed [2]decimal.Decimal
	closed[thin], closed[otherLeg(thin)] = e.base[thin].Add(e.batch.ThinQty), e.base[otherLeg(thin)].Add(e.batch.ThickQty)
	m.addStageClosed(r, closed)
}

// countExit counts the fills of an exit1 batch of both legs together
// towards the half target, once its results are in.
func (m *ExecManager) countExit(r *round) {
	b := m.batch
	if b == nil || b.kind != cex.PurposeExit || b.exit != nil || b.line != strategy.ExitLine1 || b.counted || b.doneAt.IsZero() {
		return
	}
	b.counted = true
	closed := m.stage.Closed
	for _, id := range b.orders {
		if o, found := orderIn(r.orders, id); found {
			closed[o.Request.Leg] = closed[o.Request.Leg].Add(o.CumExecQty)
		}
	}
	m.addStageClosed(r, closed)
}

// baseLeg is the leg the half target is counted on: the thin leg, or SOXS
// when both legs go together (strategy's choice).
func (m *ExecManager) baseLeg() cex.Leg {
	if m.pairing.ThinFirst {
		return m.pairing.Thin
	}
	return cex.Bear
}

// recordStage writes the half target's row (trade.exit_stage).
func (m *ExecManager) recordStage(now time.Time) {
	h := m.stage
	m.record(store.ExitStageRow{CycleID: string(m.stageCycle), SetMs: m.stageSetAt.UnixMilli(), BuiltLots: h.BuiltLots,
		TargetBull: h.Target[cex.Bull], TargetBear: h.Target[cex.Bear], ClosedBull: h.Closed[cex.Bull], ClosedBear: h.Closed[cex.Bear], DoneMs: unixMsOf(m.stageDoneAt)})
}

// RestoreExitStage takes the unfinished cycle's half target as the previous
// run stored it, before Run; nil when none was set.
func (m *ExecManager) RestoreExitStage(row *store.ExitStageRow) {
	if row == nil {
		return
	}
	m.stage = strategy.ExitStage{Set: true, BuiltLots: row.BuiltLots,
		Target: [2]decimal.Decimal{row.TargetBull, row.TargetBear}, Closed: [2]decimal.Decimal{row.ClosedBull, row.ClosedBear}}
	m.stageCycle, m.stageSetAt, m.stageDoneAt = inventory.CycleID(row.CycleID), time.UnixMilli(row.SetMs), time.Time{}
	if row.DoneMs != 0 {
		m.stageDoneAt = time.UnixMilli(row.DoneMs)
	}
}

// builtLots is how many batches of the cycle are built: open or closed.
func builtLots(c *inventory.Cycle) (n int) {
	for _, lot := range c.Lots {
		if lot.Status == inventory.LotOpen || lot.Status == inventory.LotClosed {
			n++
		}
	}
	return n
}

// unixMsOf is t in unix milliseconds, 0 for the zero time.
func unixMsOf(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// nan is the decision spread of an exit batch restored from its orders: the
// decision is not in them, so its spread stop does not apply.
var nan = math.NaN()
