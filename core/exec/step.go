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

// step is one round of the loop at now (PRD 13.2): the minutes and the
// anchors, the order snapshot booked into the ledger, then at most one
// decision - wait, the forced exit, the cycle's gap, the exit at the line or
// an entry. It reports whether the process may leave. Every exit that sends
// nothing goes through idle, which records why.
func (m *ExecManager) step(now time.Time) (done bool) {
	r := m.newRound(now)

	// The minute that ended is closed on the observations taken inside it
	// before this moment's observation is taken.
	m.closeMinutes(now)
	m.observe(r)
	m.idleReason = ""
	defer m.report(r)

	// One snapshot of the cycle's orders, booked; every check below reads it.
	m.syncOrders(r)
	if m.stopCheck(r, now) {
		return true
	}
	if m.inv.Mismatch() {
		return m.mismatched(r) // no order at all until a person checks
	}
	if anyInFlight(r.orders) {
		return m.idle(strategy.ReasonAwaitOrders) // neither final nor unknown: its result is due within about two seconds
	}
	if u := unknownOf(r.orders); len(u) > 0 {
		return m.whileUnknown(r, u)
	}
	clear(m.unknown)
	if !now.Before(m.nextReconcile) && m.reconcile(r) {
		return m.mismatched(r)
	}
	m.noteBatchDone(r)

	// Holding: the forced exit, the gap, the line exit, then a new entry.
	if c := r.cycle; c != nil {
		if m.forcedDue(c, now, !c.Flat()) {
			return m.forcedExit(r, false)
		}
		if m.workGap(r) {
			return false
		}
		// The batch's results are in and its gap sends nothing more: one
		// comparison of the positions before it is settled, so that no
		// completion or trim waits for a read.
		if b := m.batch; b != nil && !b.doneAt.IsZero() && !b.compared {
			b.compared = true
			if m.reconcile(r) {
				return m.mismatched(r)
			}
		}
		m.settle(r)
		if !m.closeIfFlat(r) {
			if r.cycle.Flat() {
				return m.idle(strategy.ReasonAwaitClose) // nothing left to trade; the venue's positions have not confirmed it yet
			}
			if !m.freshDepthSince(r) {
				return m.idle(strategy.ReasonAwaitDepth)
			}
			if m.exitAtLine(r) {
				return false
			}
		}
	}
	plan, reason := m.strat.EvaluateEntry(m.entryInput(r))
	if plan == nil {
		m.noteAtLine(r, reason)
		return m.idle(reason)
	}
	m.openBatch(r, plan)
	return false
}

// newRound reads the facts this step decides on: both books in one read, F
// and the rules.
func (m *ExecManager) newRound(now time.Time) *round {
	r := &round{now: now, books: m.cex.Books()}
	r.fx, r.fxFresh = m.cex.FX(now)
	okBull, okBear := false, false
	r.rules[cex.Bull], okBull = m.cex.Rule(cex.Bull)
	r.rules[cex.Bear], okBear = m.cex.Rule(cex.Bear)
	r.rulesOK = okBull && okBear
	return r
}

// idle records why this step sends nothing and reports that the loop goes
// on.
func (m *ExecManager) idle(reason strategy.Reason) (done bool) {
	m.idleReason = reason
	return false
}

// syncOrders takes one snapshot of the open cycle's orders from cex and
// books it into the ledger; the round keeps the snapshot and the cycle as
// booked. Without an open cycle there is nothing to take.
func (m *ExecManager) syncOrders(r *round) {
	c := m.inv.Cycle()
	if c == nil {
		return
	}
	r.orders = m.cex.CycleOrders(string(c.ID))
	m.inv.Sync(r.orders)
	r.cycle = m.inv.Cycle()
}

// anyInFlight reports whether an order has neither a final state nor an
// unknown outcome: cex has it on its way, and its result comes soon.
func anyInFlight(orders []cex.OrderRecord) bool {
	for _, o := range orders {
		if !o.Status.Terminal() && !o.Unknown {
			return true
		}
	}
	return false
}

// unknownOf returns the orders whose outcome is not known: cex queries them
// and never sends them again.
func unknownOf(orders []cex.OrderRecord) (unknown []cex.OrderRecord) {
	for _, o := range orders {
		if !o.Status.Terminal() && o.Unknown {
			unknown = append(unknown, o)
		}
	}
	return unknown
}

// noteBatchDone marks the batch done in the first round whose snapshot holds
// every one of its orders with a final state, and notes whether the venue
// refused one. The next entry and line exit wait for depth that arrived
// after this round's time (PRD 5.9), on the loop's clock. That time is read
// before the snapshot, so a result may have arrived up to the length of the
// round's reads after it: a depth frame of that instant counts as new.
func (m *ExecManager) noteBatchDone(r *round) {
	b := m.batch
	if b == nil || !b.doneAt.IsZero() || len(b.orders) == 0 {
		return
	}
	rejected := false
	for _, id := range b.orders {
		o, found := orderIn(r.orders, id)
		if !found || !o.Status.Terminal() {
			return
		}
		rejected = rejected || o.Status == cex.StatusRejected
	}
	b.doneAt, b.rejected = r.now, rejected
	logger.GlobalLogger.Debug("exec: batch results in", "kind", string(b.kind), "orders", len(b.orders), "rejected", rejected, "took", r.now.Sub(b.sentAt).Round(time.Millisecond))
}

// reconcile compares the venue's positions with the ledger (PRD 13.4,
// 13.7), once after each batch's results when its gap sends nothing more,
// and otherwise every reconcileEvery while nothing is in flight. A
// comparison that differs is repeated reconcileAgain later: inventory takes
// two that differ, that far apart, for a mismatch. It reports whether the
// positions do not match.
func (m *ExecManager) reconcile(r *round) (mismatch bool) {
	ctx, cancel := context.WithTimeout(m.ctx, m.timing.readTimeout)
	err := m.inv.Reconcile(ctx)
	cancel()
	m.nextReconcile = r.now.Add(m.timing.reconcileEvery)
	if errors.Is(err, inventory.ErrPositionsDiffer) && !m.inv.Mismatch() {
		m.nextReconcile = r.now.Add(m.timing.reconcileAgain)
	}
	if err != nil {
		logger.GlobalLogger.Debug("exec: positions compared", "err", err, "next", showTime(m.nextReconcile))
	}
	return m.inv.Mismatch()
}

// mismatched is the round while the venue's positions do not match the
// ledger: no order of any kind until a person has checked and restarted
// the process (PRD 13.7); the alarm is repeated every mismatchAlarm.
func (m *ExecManager) mismatched(r *round) bool {
	if m.nextMismatchAlarm.IsZero() {
		m.nextMismatchAlarm = r.now.Add(m.timing.mismatchAlarm) // inventory gave the first alarm
	}
	if !r.now.Before(m.nextMismatchAlarm) {
		m.nextMismatchAlarm = r.now.Add(m.timing.mismatchAlarm)
		pos := m.cex.Positions()
		ledger := m.inv.LedgerPositions()
		logger.GlobalLogger.Error("exec: positions do not match the ledger; no orders until a person checks",
			"venue_bull", pos.BySymbol[r.rules[cex.Bull].Symbol].Qty, "venue_bear", pos.BySymbol[r.rules[cex.Bear].Symbol].Qty, "ledger_bull", ledger[cex.Bull], "ledger_bear", ledger[cex.Bear])
	}
	return m.idle(strategy.ReasonPositionMismatch)
}

// orderIn finds an order of the snapshot by its id.
func orderIn(orders []cex.OrderRecord, id cex.ClientOrderID) (cex.OrderRecord, bool) {
	for _, o := range orders {
		if o.Request.ClientOrderID == id {
			return o, true
		}
	}
	return cex.OrderRecord{}, false
}

// freshDepthSince reports whether both legs' depth arrived after the last
// batch's results (PRD 5.9): the book the loop holds then is no longer the
// one the batch traded on. It holds before the first batch.
func (m *ExecManager) freshDepthSince(r *round) bool {
	if m.batch == nil || m.batch.doneAt.IsZero() {
		return true
	}
	for _, leg := range cex.Legs {
		if !r.books.ByLeg[leg].DepthReceivedAt.After(m.batch.doneAt) {
			return false
		}
	}
	return true
}

// --- the gap ---

// workGap deals with the cycle's gap (PRD 8): the two legs' anchored
// amounts differing by more than an order the venue takes. strategy chooses
// one order at a time (PRD 8.8): a completion of the leg that has less
// inside the entry batch's window, or a trim of the leg that has more. It
// reports whether the round ends here: an order was sent, or the gap waits
// - a completion for the short leg's own new frame, a trim for a price or
// for the resend interval. When it does not, no order can be sent for the
// gap: what is left is dust, and the batch can be settled.
func (m *ExecManager) workGap(r *round) (decided bool) {
	if !r.rulesOK {
		m.idle(strategy.ReasonNotReady)
		return true
	}
	a := m.strat.EvaluateGap(m.gapInput(r))
	switch a.Kind {
	case strategy.GapNone:
		return false
	case strategy.GapWait:
		m.idle(strategy.ReasonAwaitDepth)
		return true
	case strategy.GapBlocked:
		m.idle(strategy.ReasonNoPrice)
		return true
	}
	if m.maySend(r, a.Leg, a.Price) {
		m.sendGap(r, a, false)
	}
	return true
}

// maySend reports whether a reducing or completion order of leg at price may
// go now; otherwise it records why not. Two of one leg are resend apart
// (PRD 8.4, 10.4). One the venue refused for its price is not sent again at
// the same price, only once the book gives another; a refusal of another
// kind, such as a rate limit, does not hold the price back.
func (m *ExecManager) maySend(r *round, leg cex.Leg, price decimal.Decimal) bool {
	if r.now.Sub(m.lastSentAt[leg]) < m.timing.resend {
		m.idle(strategy.ReasonAwaitOrders)
		return false
	}
	if refusedAt(r.orders, leg, price) {
		m.idle(strategy.ReasonAwaitDepth) // a new book gives a new price
		return false
	}
	return true
}

// refusedAt reports whether the last reducing or completion order of leg in
// the snapshot was refused by the venue for its price, and at this price:
// rejected at once, or accepted and then cancelled for it.
func refusedAt(orders []cex.OrderRecord, leg cex.Leg, price decimal.Decimal) bool {
	for i := len(orders) - 1; i >= 0; i-- {
		if o := orders[i]; o.Request.Leg == leg && (o.Request.ReduceOnly || o.Request.Purpose == cex.PurposeComplete) {
			return o.ErrorKind == cex.ErrorPriceLimit && o.Request.Price.Equal(price)
		}
	}
	return false
}

// gapInput gathers the facts of one gap decision. The entry batch that is
// not settled yet is the batch the gap came from; otherwise the gap can
// only be trimmed.
func (m *ExecManager) gapInput(r *round) strategy.GapInput {
	c := r.cycle
	in := strategy.GapInput{Direction: c.Direction, Hold: c.Hold(), PairAnchors: c.PairAnchors, Books: r.books.ByLeg, Rules: r.rules, Now: r.now}
	_, _, in.QuotaLeft = m.inv.Quota()
	settingsOK, _ := m.cex.SettingsOK(m.leverage)
	mayAdd := r.now.Before(m.session.RegularOpen) && !m.stop.stopping && !m.stop.noEntries && settingsOK // PRD 8.5
	for _, leg := range cex.Legs {
		in.HasPrice[leg] = r.books.HasPrice(leg, r.now)
		in.DepthOK[leg] = r.books.DepthOK(leg, r.now)
		in.CanComplete[leg] = mayAdd && in.DepthOK[leg]
	}
	if b := m.batch; b != nil && b.kind == cex.PurposeEntry && !b.settled {
		in.Batch = &strategy.GapBatch{PlanPrice: b.planPrice, SentAt: b.sentAt}
		for _, id := range b.orders {
			if o, found := orderIn(r.orders, id); found && o.Request.Purpose == cex.PurposeEntry {
				in.Batch.AvgFill[o.Request.Leg] = o.AvgPrice
			}
		}
		for _, leg := range cex.Legs {
			book := r.books.ByLeg[leg]
			in.BookNew[leg] = book.DepthReceivedAt.After(b.doneAt) || book.BestReceivedAt.After(b.doneAt)
		}
	}
	return in
}

// --- the exit at the line ---

// exitAtLine sends the exit pair when the open cycle is at the exit line;
// it reports whether it sent one.
func (m *ExecManager) exitAtLine(r *round) bool {
	if !r.rulesOK {
		return false
	}
	in := m.exitInput(r)
	plan, reason := m.strat.EvaluateExit(in)
	if plan == nil {
		logger.GlobalLogger.Debug("exec: no exit at the line", "cycle", r.cycle.ID, "reason", string(reason))
		return false
	}
	m.sendExit(r, plan, in)
	return true
}

// exitInput gathers the facts of one exit decision (PRD 9.7): while the
// mean warms up again the last ready mean and sigma stand in, and while F is
// stale its last valid value does; both are expressed under the current
// anchors.
func (m *ExecManager) exitInput(r *round) strategy.ExitInput {
	c := r.cycle
	stats := m.stats
	if !stats.Ready {
		stats = m.lastReady
	}
	in := strategy.ExitInput{Direction: c.Direction, Hold: c.Hold(), Books: r.books.ByLeg, Rules: r.rules, F: r.fx.Price.InexactFloat64(),
		Anchors: m.anchors, PairAnchors: c.PairAnchors, Stats: stats}
	for _, leg := range cex.Legs {
		in.DepthOK[leg] = r.books.DepthOK(leg, r.now)
	}
	return in
}

// --- the entry ---

// entryInput gathers the facts of one entry decision. Steps 1 to 3 of PRD
// 5.13 that the round has not left on already are in Blocked.
func (m *ExecManager) entryInput(r *round) strategy.EntryInput {
	in := strategy.EntryInput{Blocked: m.entryBlocked(r), Books: r.books.ByLeg, Rules: r.rules, F: r.fx.Price.InexactFloat64(),
		Anchors: m.anchors, PairAnchors: m.anchors, Stats: m.stats, Last: m.lastEntry, AnchorChangedAt: m.anchorChangedAt, Now: r.now}
	if c := r.cycle; c != nil {
		in.Holding, in.PairAnchors = c.Direction, c.PairAnchors
	}
	_, _, in.QuotaLeft = m.inv.Quota()
	return in
}

// entryBlocked is the first failing check of steps 1 to 3 of PRD 5.13, in
// their order; empty when all pass. The position mismatch and the orders
// without a final state are left on earlier in the round, and so is a cycle
// of an ended Session: the forced exit takes the round until it is closed.
func (m *ExecManager) entryBlocked(r *round) strategy.Reason {
	now, books := r.now, r.books

	// Step 1: the program and the account.
	switch {
	case m.stop.stopping:
		return strategy.ReasonStopping
	case m.stop.noEntries:
		return strategy.ReasonNoNewEntries
	case !r.rulesOK || !m.cex.TradingReady() || !m.cex.Positions().Loaded:
		return strategy.ReasonNotReady
	}
	if ok, _ := m.cex.SettingsOK(m.leverage); !ok {
		return strategy.ReasonSettings
	}
	if (m.store != nil && !m.store.Healthy()) || m.inv.StoreBlocked() {
		return strategy.ReasonStoreError
	}
	if m.inv.CapitalPending() {
		return strategy.ReasonCapitalPending
	}

	// Step 2: the trading hours and the anchors.
	switch {
	case !now.Before(m.session.RegularOpen):
		return strategy.ReasonRegularHours
	case m.anchors.Kind == cex.AnchorNone:
		return strategy.ReasonNoAnchor
	case m.anchors.EntryBan:
		return strategy.ReasonActionWindow
	}

	// Step 3: the data.
	switch {
	case !books.DepthUsable(cex.Bull, now):
		return strategy.ReasonDepthBull
	case !books.DepthUsable(cex.Bear, now):
		return strategy.ReasonDepthBear
	case books.ByLeg[cex.Bull].Conflict:
		return strategy.ReasonConflictBull
	case books.ByLeg[cex.Bear].Conflict:
		return strategy.ReasonConflictBear
	case !r.fxFresh:
		return strategy.ReasonFXStale
	case !m.freshDepthSince(r):
		return strategy.ReasonAwaitDepth
	case m.batch != nil && m.batch.rejected && now.Sub(m.batch.doneAt) < m.timing.rejectBackoff:
		return strategy.ReasonRejectedBackoff
	}
	return ""
}

// noteAtLine records an at_line_blocked decision when the entry signal held
// and a later check refused the entry (PRD 5.13), once while the same reason
// lasts.
func (m *ExecManager) noteAtLine(r *round, reason strategy.Reason) {
	if !atLineReasons[reason] {
		m.atLine = ""
		return
	}
	if reason == m.atLine {
		return
	}
	m.atLine = reason
	v := m.view(r)
	dir := strategy.Short // the direction whose signal held
	if m.strat.AtEntryLine(strategy.Long, v.xLong, v.sigma) {
		dir = strategy.Long
	}
	logger.GlobalLogger.Info("exec: at the line, no entry", "reason", string(reason), "direction", dir, "x_long_d", v.xLong, "x_short_d", v.xShort, "mu_d", v.mu, "sigma_d", v.sigma)
	m.recordDecision(r.now, "at_line_blocked", r.cycle, "", dir, reason, map[string]any{
		"x_long_d": finite(v.xLong), "x_short_d": finite(v.xShort), "mu_d": finite(v.mu), "sigma_d": finite(v.sigma),
		"anchor_kind": m.anchors.Kind, "f": r.fx.Price, "depth_seq_bull": r.books.ByLeg[cex.Bull].DepthSeq, "depth_seq_bear": r.books.ByLeg[cex.Bear].DepthSeq})
}

// phase is the part of the Session now is in.
func (m *ExecManager) phase(now time.Time) string {
	switch {
	case now.Before(m.session.RegularOpen):
		return phaseOffHours
	case now.Before(m.session.End.Add(-m.scfg.ForcedExitBeforeClose)):
		return phaseRegular
	}
	return phaseCloseWindow
}

// lotOf finds a lot of the cycle by its id.
func lotOf(c *inventory.Cycle, id inventory.LotID) *inventory.Lot {
	for _, lot := range c.Lots {
		if lot.ID == id {
			return lot
		}
	}
	return nil
}
