package exec

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

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
	logger.GlobalLogger.Info("exec: batch taken up", "lot", lot.ID, "orders", len(b.orders), "sent", showTime(b.sentAt))
}
