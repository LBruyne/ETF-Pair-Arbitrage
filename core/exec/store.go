package exec

import (
	"encoding/json"
	"math"
	"time"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/inventory"
	"etf-pair-arbitrage/core/logger"
	"etf-pair-arbitrage/core/store"
	"etf-pair-arbitrage/core/strategy"
)

// UseStore sets where the manager records its rows. Call it before Run.
func (m *ExecManager) UseStore(s store.Sink) { m.store = s }

// record queues rows; without a store nothing is recorded.
func (m *ExecManager) record(rows ...store.Row) {
	if m.store != nil {
		m.store.Record(rows...)
	}
}

// event records an alarm or a state change of the loop. An Error-level
// event goes with an Error line, which is the caller's.
func (m *ExecManager) event(now time.Time, level, kind, message string, detail map[string]any) {
	m.record(store.EventRow{TsMs: now.UnixMilli(), Module: "exec", Level: level, Kind: kind, Message: message, Detail: marshalDetail(detail)})
}

// recordDecision records one decision of the loop: an entry, an exit, a
// forced exit or an entry refused at the line. detail carries the numbers a
// replay needs; statistics in it go through finite.
func (m *ExecManager) recordDecision(now time.Time, kind string, c *inventory.Cycle, lot inventory.LotID, dir strategy.Direction, reason strategy.Reason, detail map[string]any) {
	row := store.DecisionRow{TsMs: now.UnixMilli(), LotID: string(lot), Kind: kind, Direction: string(dir), Reason: string(reason), Detail: marshalDetail(detail)}
	if c != nil {
		row.CycleID = string(c.ID)
	}
	m.record(row)
}

// minuteBarRow is the minute_bar row of one finished minute: its raw inputs
// only, which do not depend on the parameters.
func minuteBarRow(sessionID string, b strategy.MinuteBar) store.MinuteBarRow {
	return store.MinuteBarRow{MinuteMs: b.Minute.UnixMilli(), SessionID: sessionID, MidBull: b.MidBull, MidBear: b.MidBear, F: b.F, Valid: b.Valid, Source: "own"}
}

// view computes the round's statistics under the current anchors.
func (m *ExecManager) view(r *round) view {
	v := view{midD: math.NaN(), mu: math.NaN(), sigma: math.NaN(), xLong: math.NaN(), xShort: math.NaN()}
	a := m.anchors
	if !a.Bull.IsPositive() || !a.Bear.IsPositive() {
		return v
	}
	f := r.fx.Price.InexactFloat64()
	if bull, bear := r.books.ByLeg[cex.Bull], r.books.ByLeg[cex.Bear]; f > 0 && hasMid(bull) && hasMid(bear) {
		v.midD = strategy.Spread(f, mid(bull), a.Bull.InexactFloat64(), mid(bear), a.Bear.InexactFloat64())
	}
	if len(m.stats.Window) == 0 {
		return v
	}
	v.mu, v.sigma = m.stats.Mu(a), m.stats.Sigma(a)
	if f > 0 {
		v.xLong, _ = m.strat.Deviation(r.books.ByLeg, strategy.Long, f, a, m.stats)
		v.xShort, _ = m.strat.Deviation(r.books.ByLeg, strategy.Short, f, a, m.stats)
	}
	return v
}

// report writes the round's signal_sample row once a second and the status
// line every statusLog. It runs at the end of every round, with the idle
// reason the round left with.
func (m *ExecManager) report(r *round) {
	if r.now.Before(m.nextSample) && r.now.Before(m.nextStatus) {
		return
	}
	v := m.view(r)
	if !r.now.Before(m.nextSample) {
		m.record(m.sampleRow(r, v))
		m.nextSample = r.now.Truncate(m.timing.sample).Add(m.timing.sample)
	}
	if !r.now.Before(m.nextStatus) {
		m.logStatus(r, v)
		m.nextStatus = r.now.Add(m.timing.statusLog)
	}
}

// sampleRow is the signal_sample row of the round.
func (m *ExecManager) sampleRow(r *round, v view) store.SignalSampleRow {
	f := math.NaN()
	if !r.fx.ReceivedAt.IsZero() {
		f = r.fx.Price.InexactFloat64()
	}
	row := store.SignalSampleRow{TsMs: r.now.UnixMilli(), SessionID: m.session.ID, SessionEndMs: m.session.End.UnixMilli(), Phase: m.phase(r.now),
		AnchorKind: string(m.anchors.Kind), AnchorBull: m.anchors.Bull, AnchorBear: m.anchors.Bear, F: f,
		MidD: v.midD, MuD: v.mu, SigmaD: v.sigma, XLongD: v.xLong, XShortD: v.xShort, StatsReady: m.stats.Ready, IdleReason: string(m.idleReason),
		DepthOKBull: r.books.DepthUsable(cex.Bull, r.now), DepthOKBear: r.books.DepthUsable(cex.Bear, r.now),
		ConflictBull: r.books.ByLeg[cex.Bull].Conflict, ConflictBear: r.books.ByLeg[cex.Bear].Conflict}
	if c := m.inv.Cycle(); c != nil {
		hold := c.Hold()
		row.CycleID, row.Direction, row.HoldBull, row.HoldBear = string(c.ID), string(c.Direction), hold[cex.Bull], hold[cex.Bear]
	}
	_, _, row.QuotaLeft = m.inv.Quota()
	return row
}

// finite is v for a JSON detail, or nil when v is not a finite number: JSON
// has no NaN, and a statistic that could not be computed is stored as null.
func finite(v float64) any {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return v
}

// marshalDetail encodes an event's or a decision's detail. The values are
// the loop's own and always encode; a failure is logged and stores null.
func marshalDetail(detail map[string]any) json.RawMessage {
	if detail == nil {
		return nil
	}
	b, err := json.Marshal(detail)
	if err != nil {
		logger.GlobalLogger.Error("exec: detail not encoded", "err", err)
		return nil
	}
	return b
}
