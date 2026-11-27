package exec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/inventory"
	"etf-pair-arbitrage/core/logger"
	"etf-pair-arbitrage/core/store"
	"etf-pair-arbitrage/core/strategy"
)

// Restore takes what the previous run stored, before Run: the Session's
// minute bars and anchor facts, the last built entry for the spacing rule,
// and where the minutes the process did not record itself are read from
// (the recorder's per-second rows; nil leaves them invalid).
func (m *ExecManager) Restore(bars []store.MinuteBarRow, facts []store.AnchorFactRow, last *store.LotRow, perp minuteSource) {
	m.restoredBars, m.restoredFacts, m.perp = bars, facts, perp
	if last != nil {
		m.lastEntry = &strategy.LastEntry{At: time.UnixMilli(last.OpenedMs), E: last.XD, Direction: strategy.Direction(last.Direction)}
	}
}

// recover brings the loop up to what the venue and the stored records say
// before the first decision (PRD 13.4). A step that cannot read its facts
// is tried again every recoverRetry, never taken as "flat"; positions that
// differ from the ledger are read again after reconcileAgain, so that
// inventory can confirm the difference. A stop signal
// meanwhile only sets stopping: the previous process may have left a
// position or an order of unknown outcome, so recovery is finished first
// and the loop then closes and leaves as after any stop. While it is not,
// the stop is checked as in a round (stopCheck): its alarm every
// stop_alarm_after, and a stop signal after the alarm is a takeover, which
// leaves at once (leave); so does a stop with no cycle and no order open.
func (m *ExecManager) recover(ctx context.Context) (leave bool, err error) {
	log := logger.GlobalLogger
	for attempt := 1; ; attempt++ {
		err := m.recoverOnce(ctx, m.timing.clock())
		if err == nil {
			return false, nil
		}
		// A difference is read again as soon as inventory can confirm it, as
		// in the loop; facts that could not be read wait longer.
		wait := m.timing.recoverRetry
		if errors.Is(err, inventory.ErrPositionsDiffer) {
			wait = m.timing.reconcileAgain
		}
		log.Warn("exec: recovery not complete; retrying", "attempt", attempt, "err", err, "retry_in", wait)
		retry := time.After(wait)
	wait:
		for {
			now := m.timing.clock()
			r := m.newRound(now)
			r.cycle = m.inv.Cycle()
			if m.stopCheck(r, now) {
				return true, nil
			}
			select {
			case <-ctx.Done():
				return false, ctx.Err()
			case sig := <-m.signals:
				m.onSignal(sig, m.timing.clock())
			case <-retry:
				break wait
			}
		}
	}
}

// recoverOnce runs the steps of recovery that are not done yet: the orders
// of the unfinished cycle the venue has and the table lacks, the minutes,
// the batch being built, and one comparison of the venue's positions with
// the ledger. It returns the first failure; done steps are not repeated.
func (m *ExecManager) recoverOnce(ctx context.Context, now time.Time) error {
	log := logger.GlobalLogger
	c := m.inv.Cycle()

	// The orders. Only an unfinished cycle can have orders the table lacks
	// (cex.RecoverOrders). Each one recognised is booked, and the lot the
	// ledger gives it is stored, so that a later start books it the same.
	if c != nil && !m.recovered.orders {
		found, err := m.cex.RecoverOrders(ctx, string(c.ID))
		if err != nil {
			return fmt.Errorf("the venue's orders of cycle %s: %w", c.ID, err)
		}
		m.inv.Sync(m.cex.CycleOrders(string(c.ID)))
		for _, o := range found {
			id := o.Request.ClientOrderID
			lot := m.inv.LotOf(id)
			if lot != "" {
				m.cex.SetOrderLot(id, string(lot))
			}
			log.Info("exec: order recognised at the venue", "client_order_id", id, "purpose", string(o.Request.Purpose), "leg", o.Request.Leg.String(), "lot", lot,
				"status", string(o.Status), "cum_exec_qty", o.CumExecQty)
		}
		m.recovered.orders = true
	}

	// The minutes, and the batch an unsettled lot was being built by.
	if !m.recovered.minutes {
		if err := m.restoreMinutes(ctx, now); err != nil {
			return err
		}
		m.recovered.minutes = true
	}
	m.restoreBatch(c, now)

	// The venue's positions against the ledger. Unread is not flat; one
	// read that differs proves nothing yet, two make a mismatch, and the
	// loop then sends nothing until a person checks.
	err := m.compare(ctx, now)
	if err != nil && !(errors.Is(err, inventory.ErrPositionsDiffer) && m.inv.Mismatch()) {
		return fmt.Errorf("the venue's positions: %w", err)
	}

	// The anchors as they are, so that the first round sees no change.
	if a := m.cex.Anchors(); a.SessionID == m.session.ID {
		m.anchors, m.anchorChangedAt, m.restartedAt = a, a.KnownAt, a.RestartAt
	}
	attrs := []any{"session", m.session.ID, "minutes", len(m.bars), "stats_ready", m.stats.Ready, "mismatch", m.inv.Mismatch()}
	if c != nil {
		dir, hold := m.inv.Holdings()
		attrs = append(attrs, "cycle", c.ID, "status", c.Status, "direction", dir, "hold_bull", hold[cex.Bull], "hold_bear", hold[cex.Bear])
		if c.Status == inventory.CycleTakeover {
			log.Info("exec: cycle taken back after a takeover", "cycle", c.ID)
		}
	}
	log.Info("exec: recovered", attrs...)
	return nil
}

// restoreMinutes rebuilds the Session's finished minutes up to now (PRD 3.7,
// 4.2): the process's own minute_bar rows first; a minute it did not record
// from the recorder's last second with both legs' best bid and ask in it,
// with F the source's close of that minute; a minute with neither is
// invalid. The mean and sigma then run as if the loop had closed every
// minute itself: they restart after an invalid minute, at a dividend's
// effect and at rewarmMinute of the last switch of the anchor that warmed
// them up again (restoredRewarm), as anchorChanged does; the last ready
// stats are kept for the exits.
func (m *ExecManager) restoreMinutes(ctx context.Context, now time.Time) error {
	s := m.session
	from, to := s.Start.Truncate(time.Minute), now.Truncate(time.Minute)
	own := map[int64]store.MinuteBarRow{}
	for _, row := range m.restoredBars {
		if row.SessionID == s.ID {
			own[row.MinuteMs] = row
		}
	}

	// The recorder's minutes and F, read only when a minute is missing. They
	// only warm the mean up: when either cannot be read, the minutes the
	// process did not record are invalid, and recovery goes on.
	perp, fx := map[int64]store.PerpMinuteRow{}, map[int64]float64{}
	missing := false
	for t := from; t.Before(to) && !missing; t = t.Add(time.Minute) {
		_, held := own[t.UnixMilli()]
		missing = !held
	}
	if missing && m.perp != nil {
		bull, okBull := m.cex.Rule(cex.Bull)
		bear, okBear := m.cex.Rule(cex.Bear)
		if !okBull || !okBear {
			return errors.New("the contract rules are not read yet")
		}
		rows, err := m.perp.LoadPerpMinutes(ctx, string(bull.Symbol), string(bear.Symbol), from, to)
		if err != nil {
			logger.GlobalLogger.Warn("exec: the recorder's minutes not read; the minutes not recorded are invalid", "session", s.ID, "err", err)
		}
		for _, row := range rows {
			perp[row.MinuteMs] = row
		}
		closes, err := m.cex.FXMinuteCloses(ctx, from, to)
		if err != nil {
			logger.GlobalLogger.Warn("exec: the minute closes of F not read; the minutes not recorded are invalid", "session", s.ID, "err", err)
		}
		for _, c := range closes {
			fx[c.Minute.UnixMilli()] = c.Close.InexactFloat64()
		}
	}

	// The minutes as closeMinutes would have closed them.
	var bars []strategy.MinuteBar
	ownN, filled := 0, 0
	for t := from; t.Before(to); t = t.Add(time.Minute) {
		bar := strategy.MinuteBar{Minute: t}
		if row, held := own[t.UnixMilli()]; held {
			bar.Valid, bar.MidBull, bar.MidBear, bar.F = row.Valid, row.MidBull, row.MidBear, row.F
			ownN++
		} else if p, held := perp[t.UnixMilli()]; held {
			if f, known := fx[t.UnixMilli()]; known {
				bar = strategy.MinuteBar{Minute: t, Valid: true, MidBull: (p.BidBull + p.AskBull) / 2, MidBear: (p.BidBear + p.AskBear) / 2, F: f}
				m.record(minuteBarRow(s.ID, bar, sourcePerpSecond))
				filled++
			}
		}
		bars = append(bars, bar)
	}

	// The mean and sigma over them, restarting where the loop would have.
	var restartAt time.Time
	if a := m.cex.Anchors(); a.SessionID == s.ID {
		restartAt = a.RestartAt
	}
	rewarmAt := m.restoredRewarm(bars, anchorSwitches(m.restoredFacts, s.ID))
	m.bars, m.runStart, m.stats = nil, 0, strategy.Stats{}
	restarted := false
	for _, bar := range bars {
		t := bar.Minute
		if !restartAt.IsZero() && !restarted && !t.Before(restartAt) {
			m.runStart, restarted = len(m.bars), true // the dividend took effect: the run starts here
		}
		if t.Equal(rewarmAt) {
			m.runStart = len(m.bars) // the anchor switched in this minute and warmed the mean up again
		}
		m.bars = append(m.bars, bar)
		if !bar.Valid {
			m.runStart = len(m.bars)
		}
		m.stats = strategy.ComputeStats(m.bars[m.runStart:], m.scfg)
		if m.stats.Ready {
			m.lastReady = m.stats
		}
	}
	if rewarmAt.Equal(to) { // in the minute still open
		m.runStart, m.stats = len(m.bars), strategy.Stats{}
	}
	m.minute, m.obs, m.restartedAt = to, nil, restartAt
	logger.GlobalLogger.Info("exec: minutes restored", "session", s.ID, "minutes", len(m.bars), "own", ownN, "from_recorder", filled,
		"invalid", len(m.bars)-ownN-filled, "run_minutes", len(m.bars)-m.runStart, "stats_ready", m.stats.Ready)
	return nil
}

// anchorSwitch is one switch of a Session's anchor as the stored facts say:
// the bell anchor read (cex.FactBell), or the anchor switched to a NAV
// (cex.FactNAV), both legs with one known time.
type anchorSwitch struct {
	at     time.Time
	values cex.Anchors // Kind, Bull and Bear
}

// anchorSwitches lists the Session's switches of the anchor from its stored
// facts, oldest first.
func anchorSwitches(facts []store.AnchorFactRow, sessionID string) []anchorSwitch {
	byTime := map[string]*anchorSwitch{}
	var out []*anchorSwitch
	for _, f := range facts {
		if f.SessionID != sessionID || f.Kind != cex.FactBell && f.Kind != cex.FactNAV {
			continue
		}
		key := fmt.Sprintf("%s/%d", f.Kind, f.KnownMs)
		sw := byTime[key]
		if sw == nil {
			sw = &anchorSwitch{at: time.UnixMilli(f.KnownMs), values: cex.Anchors{Kind: cex.AnchorKind(f.Kind)}}
			byTime[key] = sw
			out = append(out, sw)
		}
		if f.Leg == cex.Bull.String() {
			sw.values.Bull = f.Value
		} else {
			sw.values.Bear = f.Value
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].at.Before(out[j].at) })
	list := make([]anchorSwitch, len(out))
	for i, sw := range out {
		list[i] = *sw
	}
	return list
}

// restoredRewarm is the minute of the last switch that warmed the mean and
// sigma up again, as anchorChanged decided it (warmsUpAgain); zero when
// none did. The first anchor of a Session warms nothing up. The spread's
// jump of a NAV switch is taken at the mids and F of the last valid minute
// before its minute: the restart does not have the mids the loop saw.
func (m *ExecManager) restoredRewarm(bars []strategy.MinuteBar, switches []anchorSwitch) time.Time {
	var at time.Time
	for i := 1; i < len(switches); i++ {
		prev, sw := switches[i-1], switches[i]
		minute, jump := rewarmMinute(sw.at), math.NaN()
		for j := len(bars) - 1; j >= 0; j-- {
			if b := bars[j]; b.Valid && b.Minute.Before(minute) {
				jump = anchorJump(b.F, b.MidBull, b.MidBear, prev.values, sw.values)
				break
			}
		}
		if m.warmsUpAgain(sw.values.Kind != prev.values.Kind, jump) {
			at = minute
		}
	}
	return at
}

// restoreBatch takes up the entry batch whose lot was still being built
// when the previous process ended: its orders (as sent and as recognised)
// and its first send, so that the gap of that lot is worked to its end and
// the lot settled. Without such a lot there is no batch to take up. A lot
// whose orders were never written, nor found at the venue, is settled void
// at once: nothing was sent for it.
func (m *ExecManager) restoreBatch(c *inventory.Cycle, now time.Time) {
	if m.batch != nil || c == nil {
		return
	}
	var lot *inventory.Lot
	for _, l := range c.Lots {
		if l.Status == inventory.LotBuilding {
			lot = l
		}
	}
	if lot == nil {
		if m.pairing.ThinFirst {
			m.restoreExit(c)
		}
		return
	}
	b := &batchState{kind: cex.PurposeEntry, lotID: lot.ID, direction: c.Direction, x: lot.X, sentAt: lot.OpenedAt}
	for _, o := range m.cex.CycleOrders(string(c.ID)) {
		id := o.Request.ClientOrderID
		if inventory.LotID(o.Request.LotID) != lot.ID && m.inv.LotOf(id) != lot.ID {
			continue
		}
		b.orders = append(b.orders, id)
		if o.Request.Purpose == cex.PurposeEntry {
			b.planPrice[o.Request.Leg] = o.Request.Price
			if !o.SubmittedAt.IsZero() && o.SubmittedAt.Before(b.sentAt) {
				b.sentAt = o.SubmittedAt
			}
		}
	}
	if len(b.orders) == 0 {
		m.inv.SettleLot(lot.ID, false, now)
		logger.GlobalLogger.Info("exec: a lot without orders settled void", "lot", lot.ID)
		return
	}
	m.batch = b
	if m.pairing.ThinFirst {
		m.restorePair(b, lot)
	}
	logger.GlobalLogger.Info("exec: batch taken up", "lot", lot.ID, "orders", len(b.orders), "sent", showTime(b.sentAt))
}

// RestoreEntryDecision takes the entry decision of the unfinished cycle's
// lot still building, as the previous run stored it, before Run; nil when
// there is none. A batch in pairing taken up from its orders reads from it
// the anchors and F its D_dec was taken under (restorePair).
func (m *ExecManager) RestoreEntryDecision(row *store.DecisionRow) {
	m.restoredEntry = row
}

// RestoreExitDecision takes the unfinished cycle's last exit decision, as
// the previous run stored it, before Run; nil when there is none. An exit
// batch taken up from its orders reads its rules from it when it is the
// batch's (restoreExit).
func (m *ExecManager) RestoreExitDecision(row *store.DecisionRow) {
	m.restoredExit = row
}

// decisionBasis is the anchors and F the entry decision of lot was taken
// under, from its stored detail (anchor_bull, anchor_bear, f); ok is false
// without that record or with a number missing from it.
func (m *ExecManager) decisionBasis(lot inventory.LotID) (a cex.Anchors, f float64, ok bool) {
	row := m.restoredEntry
	if row == nil || inventory.LotID(row.LotID) != lot {
		return cex.Anchors{}, 0, false
	}
	var d struct {
		Kind       cex.AnchorKind      `json:"anchor_kind"`
		AnchorBull decimal.NullDecimal `json:"anchor_bull"`
		AnchorBear decimal.NullDecimal `json:"anchor_bear"`
		F          decimal.NullDecimal `json:"f"`
	}
	if err := json.Unmarshal(row.Detail, &d); err != nil || !d.AnchorBull.Valid || !d.AnchorBear.Valid || !d.F.Valid ||
		!d.AnchorBull.Decimal.IsPositive() || !d.AnchorBear.Decimal.IsPositive() || !d.F.Decimal.IsPositive() {
		return cex.Anchors{}, 0, false
	}
	return cex.Anchors{SessionID: m.session.ID, Kind: d.Kind, Bull: d.AnchorBull.Decimal, Bear: d.AnchorBear.Decimal}, d.F.Decimal.InexactFloat64(), true
}

// restorePair takes up a thin-leg-first batch in pairing from its order
// records (SPEC 12.9 "重启", PRD 5.19): the decision from the lot (x_dec,
// mu, C) and the thin order (qS), and the anchors and F of D_dec from the
// entry decision's record (without it the spread stop falls back to the
// current ones); S, v_S, L, v_L from the orders each round
// (refreshPair); the first fill's time from the arrival of the thin order's
// final state. Inside the window the pairing goes on, after it the next
// round unwinds S_open at once. The x of a top-up is not in the order
// records: after one filled the batch tops up no more. The batch is the last
// entry for the spacing.
func (m *ExecManager) restorePair(b *batchState, lot *inventory.Lot) {
	thin := m.pairing.Thin
	plan := &strategy.EntryPlan{Direction: lot.Direction, X: lot.X, Mu: lot.Mu, Sigma: lot.Sigma, FeeD: lot.FeeD}
	basis := false
	if a, f, ok := m.decisionBasis(lot.ID); ok {
		plan.Anchors, plan.F, basis = a, f, true
	}
	p := &pairState{plan: plan, thin: thin, thick: otherLeg(thin)}
	thinFills := 0
	for _, o := range m.cex.CycleOrders(string(lot.CycleID)) {
		if !slices.Contains(b.orders, o.Request.ClientOrderID) || o.Request.Purpose == cex.PurposeTrim {
			continue
		}
		p.sent[o.Request.Leg] = true
		if o.Request.Leg != thin {
			continue
		}
		if p.thinID == "" {
			p.thinID = o.Request.ClientOrderID
			plan.Qty[thin], plan.Price[thin] = o.Request.Qty, o.Request.Price
		}
		if o.CumExecQty.IsPositive() {
			thinFills++
		}
		if o.Request.ClientOrderID == p.thinID && o.Status.Terminal() && o.CumExecQty.IsPositive() {
			first := o.FinalAt
			if first.IsZero() {
				first = o.SubmittedAt
			}
			p.batch = strategy.NewPairBatch(plan, thin, o.CumExecQty, vwap(o.CumExecValue, o.CumExecQty), first)
			p.started = true
		}
	}
	p.noTopUps = thinFills > 1
	b.pair = p
	if m.lastEntry == nil || m.lastEntry.At.Before(lot.OpenedAt) {
		m.lastEntry = &strategy.LastEntry{At: lot.OpenedAt, E: lot.X, Direction: lot.Direction}
	}
	logger.GlobalLogger.Info("exec: pairing taken up", "lot", lot.ID, "thin_order", p.thinID, "started", p.started, "first_fill", showTime(p.batch.FirstFill), "top_ups_stopped", p.noTopUps,
		"decision_anchors_read", basis, "decision_bull_usd", plan.Anchors.Bull, "decision_bear_usd", plan.Anchors.Bear, "decision_f", plan.F)
}

// restoreExit takes up a thin-leg-first line exit batch whose thick leg had
// not reached its target (strategy.ExitThickTarget) when the previous process ended
// (PRD 9.13): the last thin exit order with a fill and the thick exit
// orders after it, when nothing but those orders came after it. Its fills
// come from those orders, exit_pair_window runs from the arrival of the thin
// order's final state. The exit's rules are not in the order records: they
// come from the batch's exit decision (restoreExitRules) - the line, D1 and
// the protection, the decision's spread with its anchors and F, and the half
// target's closed quantities at the batch's start, so that its fills count
// towards H once. Without that decision the spread stop does not apply, the
// thick leg is held to its break-even bound until the window ends it, and
// the batch counts towards no half target.
func (m *ExecManager) restoreExit(c *inventory.Cycle) {
	thin := m.pairing.Thin
	orders := m.cex.CycleOrders(string(c.ID))
	slices.SortFunc(orders, func(a, b cex.OrderRecord) int {
		return strings.Compare(string(a.Request.ClientOrderID), string(b.Request.ClientOrderID))
	})
	var b *batchState
	for _, o := range orders {
		switch {
		case o.Request.Purpose == cex.PurposeExit && o.Request.Leg == thin && (o.CumExecQty.IsPositive() || !o.Status.Terminal()):
			e := &exitState{thinID: o.Request.ClientOrderID, base: m.stageOf(c).Closed,
				batch: strategy.ExitBatch{Dir: c.Direction, DecisionD: nan, Fills: strategy.Fills{ThinQty: decimal.Zero, ThinVWAP: decimal.Zero, ThickQty: decimal.Zero, ThickVWAP: decimal.Zero}}}
			e.sent[thin] = true
			if o.Status.Terminal() {
				e.started, e.batch.FirstFill = true, o.FinalAt
				if e.batch.FirstFill.IsZero() {
					e.batch.FirstFill = o.SubmittedAt
				}
			}
			b = &batchState{kind: cex.PurposeExit, sentAt: o.SubmittedAt, orders: []cex.ClientOrderID{o.Request.ClientOrderID}, exit: e}
		case o.Request.Purpose == cex.PurposeExit && o.Request.Leg != thin && b != nil:
			b.orders = append(b.orders, o.Request.ClientOrderID)
			b.exit.sent[o.Request.Leg] = true
		default:
			b = nil
		}
	}
	if b == nil {
		return
	}
	// A batch whose thick leg matched its thin leg already ended: nothing is
	// left of the round's cumulative thick target (strategy.ExitThickTarget).
	var q [2]decimal.Decimal
	for _, o := range orders {
		if o.Request.ClientOrderID >= b.exit.thinID {
			q[o.Request.Leg] = q[o.Request.Leg].Add(o.CumExecQty)
		}
	}
	if rule, ok := m.cex.Rule(otherLeg(thin)); ok && b.exit.started {
		fills := strategy.Fills{ThinQty: q[thin], ThickQty: q[otherLeg(thin)]}
		if !strategy.ExitThickTarget(m.pairing, c.PairAnchors, rule.QtyStep, closedBefore(orders, b.exit.thinID), fills, m.ledgerReducible(c)).IsPositive() {
			return
		}
	}
	read := m.restoreExitRules(b.exit, q)
	b.line = b.exit.batch.Line
	m.batch = b
	logger.GlobalLogger.Info("exec: exit batch taken up", "cycle", c.ID, "orders", len(b.orders), "first_fill", showTime(b.exit.batch.FirstFill),
		"decision_read", read, "line", string(b.line), "d1", b.exit.batch.D1, "protect", b.exit.batch.Protect, "d_dec", b.exit.batch.DecisionD,
		"stage_base_bull", b.exit.base[cex.Bull], "stage_base_bear", b.exit.base[cex.Bear])
}

// restoreExitRules reads the rules of the exit batch e taken up from its
// orders out of the exit decision the previous run stored
// (RestoreExitDecision), when its client_order_ids hold the batch's thin
// order: the line, D1 and the protection, the expected net, the decision's
// spread and the anchors and F it is under. With exit1 the half target's
// closed quantities when the batch began are its base: from the decision
// (stage_closed_bull, stage_closed_bear), or, in a decision written before
// they were recorded, the stored closed quantities less the batch's fills
// (filled, per leg), which the stored ones held already. Its fills then
// count towards H from that base, each once. It reports whether it read
// the decision; e is unchanged otherwise.
func (m *ExecManager) restoreExitRules(e *exitState, filled [2]decimal.Decimal) bool {
	row := m.restoredExit
	if row == nil {
		return false
	}
	var d struct {
		IDs         []cex.ClientOrderID `json:"client_order_ids"`
		Line        strategy.ExitLine   `json:"line"`
		DDec        *float64            `json:"d_dec"`
		ExpectedNet decimal.NullDecimal `json:"expected_net"`
		D1          bool                `json:"d1"`
		Protect     decimal.NullDecimal `json:"protect"`
		Kind        cex.AnchorKind      `json:"anchor_kind"`
		AnchorBull  decimal.NullDecimal `json:"anchor_bull"`
		AnchorBear  decimal.NullDecimal `json:"anchor_bear"`
		F           decimal.NullDecimal `json:"f"`
		BaseBull    decimal.NullDecimal `json:"stage_closed_bull"`
		BaseBear    decimal.NullDecimal `json:"stage_closed_bear"`
	}
	if err := json.Unmarshal(row.Detail, &d); err != nil || !slices.Contains(d.IDs, e.thinID) {
		return false
	}
	b := &e.batch
	b.Line, b.D1, b.ExpectedNet, b.Protect = d.Line, d.D1, d.ExpectedNet.Decimal, d.Protect.Decimal
	if d.DDec != nil {
		b.DecisionD = *d.DDec
	}
	if d.AnchorBull.Decimal.IsPositive() && d.AnchorBear.Decimal.IsPositive() && d.F.Decimal.IsPositive() {
		b.DecisionAnchors = cex.Anchors{SessionID: m.session.ID, Kind: d.Kind, Bull: d.AnchorBull.Decimal, Bear: d.AnchorBear.Decimal}
		b.DecisionF = d.F.Decimal.InexactFloat64()
	}
	if b.Line == strategy.ExitLine1 {
		if d.BaseBull.Valid && d.BaseBear.Valid {
			e.base = [2]decimal.Decimal{d.BaseBull.Decimal, d.BaseBear.Decimal}
		} else {
			for _, leg := range cex.Legs {
				e.base[leg] = decimal.Max(decimal.Zero, e.base[leg].Sub(filled[leg]))
			}
		}
	}
	return true
}
