package exec

import (
	"math"
	"time"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/logger"
	"etf-pair-arbitrage/core/strategy"
)

// closeMinutes finishes every minute that has ended by now, each with the
// last usable observation taken inside it (PRD 4.2). A minute without one is
// invalid: it is not filled from its neighbour, and the mean and sigma start
// again after it (PRD 4.6). It runs before the round's observation, so a
// minute never sees a moment of the next one, and the open minute never
// enters the mean (PRD 4.5).
func (m *ExecManager) closeMinutes(now time.Time) {
	log := logger.GlobalLogger
	for !m.minute.Add(time.Minute).After(now) {
		bar := strategy.MinuteBar{Minute: m.minute, Valid: m.obs != nil}
		if bar.Valid {
			bar.MidBull, bar.MidBear, bar.F = m.obs.midBull, m.obs.midBear, m.obs.f
		}
		m.bars = append(m.bars, bar)
		if !bar.Valid {
			m.runStart = len(m.bars)
		}
		wasReady := m.stats.Ready
		m.stats = strategy.ComputeStats(m.bars[m.runStart:], m.scfg)
		if m.stats.Ready {
			m.lastReady = m.stats
			if !wasReady {
				log.Info("exec: mean and sigma ready", "session", m.session.ID, "as_of", showTime(m.stats.AsOf), "minutes", len(m.bars)-m.runStart,
					"mu_d", m.stats.Mu(m.anchors), "sigma_d", m.stats.Sigma(m.anchors))
			}
		}
		m.record(minuteBarRow(m.session.ID, bar, sourceOwn))
		log.Debug("exec: minute closed", "minute", showTime(bar.Minute), "valid", bar.Valid, "mid_bull", bar.MidBull, "mid_bear", bar.MidBear, "f", bar.F,
			"run_minutes", len(m.bars)-m.runStart, "stats_ready", m.stats.Ready)
		m.minute, m.obs = m.minute.Add(time.Minute), nil
	}
}

// observe follows the Session and the anchors, then keeps this moment as the
// open minute's observation when both legs' depth is usable and F is fresh.
// A conflict does not spoil an observation (PRD 3.1): the mid is taken from
// the newer of the two best-price streams.
func (m *ExecManager) observe(r *round) {
	log := logger.GlobalLogger
	now := r.now

	// A regular close passed: a new Session, whose mean starts from nothing.
	// The last ready stats stay, for the exits of a cycle still open.
	if s := m.cex.Session(now); s.ID != m.session.ID {
		log.Info("exec: new session", "from", m.session.ID, "to", s.ID, "regular_open", showTime(s.RegularOpen), "session_end", showTime(s.End))
		m.session, m.bars, m.runStart, m.stats = s, nil, 0, strategy.Stats{}
	}

	// Just after a close cex may still publish the ended Session's anchors:
	// they count as no anchor yet.
	a := m.cex.Anchors()
	if a.SessionID != m.session.ID {
		a = cex.Anchors{SessionID: m.session.ID}
	}
	if a.RestartAt.After(m.restartedAt) { // a dividend took effect
		m.runStart, m.restartedAt, m.stats = len(m.bars), a.RestartAt, strategy.Stats{}
		log.Info("exec: mean and sigma restart: a dividend took effect", "restart_at", showTime(a.RestartAt))
	}
	if a.Kind != m.anchors.Kind || !a.Bull.Equal(m.anchors.Bull) || !a.Bear.Equal(m.anchors.Bear) {
		m.anchorChanged(r, a)
	}
	m.anchors = a

	if r.books.DepthUsable(cex.Bull, now) && r.books.DepthUsable(cex.Bear, now) && r.fxFresh {
		m.obs = &observation{midBull: mid(r.books.ByLeg[cex.Bull]), midBear: mid(r.books.ByLeg[cex.Bear]), f: r.fx.Price.InexactFloat64()}
	}
}

// anchorChanged takes the anchors' new values a. The first anchor of a
// Session is logged. A change from one anchor to another within the Session
// is an event with the spread's jump at the current mids (PRD 11.4).
// Positions and the cycle's pairing anchors are not touched (PRD 11.1,
// 11.2). The mean and sigma hold no anchor and are expressed under the new
// one at once (PRD 11.3), unless the change is a switch of the anchor that
// warms them up again (warmsUpAgain); the new run starts at rewarmMinute. A
// dividend's adjustment keeps SwitchedAt and is not a switch: its restart
// comes with RestartAt (PRD 4.6).
func (m *ExecManager) anchorChanged(r *round, a cex.Anchors) {
	log := logger.GlobalLogger
	from := m.anchors
	m.anchorChangedAt = r.now
	if from.Kind == cex.AnchorNone || a.Kind == cex.AnchorNone {
		log.Info("exec: anchors", "session", a.SessionID, "kind", string(a.Kind), "bull_usd", a.Bull, "bear_usd", a.Bear, "known_at", showTime(a.KnownAt), "entry_ban", a.EntryBan)
		return
	}
	jump := math.NaN()
	if bull, bear := r.books.ByLeg[cex.Bull], r.books.ByLeg[cex.Bear]; hasMid(bull) && hasMid(bear) {
		jump = anchorJump(r.fx.Price.InexactFloat64(), mid(bull), mid(bear), from, a)
	}
	switched := a.Kind != from.Kind || !a.SwitchedAt.Equal(from.SwitchedAt)
	rewarm := switched && m.warmsUpAgain(a.Kind != from.Kind, jump)
	if rewarm {
		// A minute closed already when the loop sees the NAV only in the next
		// one; never before the run's start (an invalid minute, a dividend).
		start, from := rewarmMinute(a.SwitchedAt), len(m.bars)
		for from > m.runStart && !m.bars[from-1].Minute.Before(start) {
			from--
		}
		m.runStart = from
		m.stats = strategy.ComputeStats(m.bars[m.runStart:], m.scfg)
	}
	log.Info("exec: anchor changed", "session", a.SessionID, "from", string(from.Kind), "to", string(a.Kind), "from_bull_usd", from.Bull, "from_bear_usd", from.Bear,
		"to_bull_usd", a.Bull, "to_bear_usd", a.Bear, "known_at", showTime(a.KnownAt), "switched", switched, "mid_jump_d", jump, "rewarm", rewarm)
	m.event(r.now, "info", "anchor_changed", "the anchors changed within the Session; positions and the cycle's pairing anchors are kept",
		map[string]any{"session_id": a.SessionID, "from": from.Kind, "to": a.Kind, "from_bull": from.Bull, "from_bear": from.Bear, "to_bull": a.Bull, "to_bear": a.Bear,
			"known_ms": a.KnownAt.UnixMilli(), "switched": switched, "mid_jump_d": finite(jump), "rewarm": rewarm})
}

// warmsUpAgain reports whether a switch of the anchor within the Session
// starts the mean and sigma over (PRD 11.3, owner items 96-97). Only with
// rewarm_on_anchor_change: the NAV replacing the bell anchor (kindChanged)
// always does; a new NAV in the watch does when the spread's jump at the
// mids is cex.nav_hot_switch_d or more, or unknown, and is a hot switch
// otherwise.
func (m *ExecManager) warmsUpAgain(kindChanged bool, jump float64) bool {
	return m.scfg.RewarmOnAnchorChange && (kindChanged || !(math.Abs(jump) < m.cex.NAVHotSwitchD()))
}

// anchorJump is the spread's jump, in spread units, at the mids from the
// anchors from to the anchors to.
func anchorJump(f, midBull, midBear float64, from, to cex.Anchors) float64 {
	return strategy.Spread(f, midBull, to.Bull.InexactFloat64(), midBear, to.Bear.InexactFloat64()) -
		strategy.Spread(f, midBull, from.Bull.InexactFloat64(), midBear, from.Bear.InexactFloat64())
}

// rewarmMinute is the minute the mean and sigma start again from after a
// switch of the anchor that warms them up again: the minute it became known
// in, whether the loop saw it in that minute or the next, and at a restart.
func rewarmMinute(switchedAt time.Time) time.Time { return switchedAt.Truncate(time.Minute) }

// mid is a book's raw best bid and ask midpoint, in USDT.
func mid(b *cex.Book) float64 {
	return b.BestBid.Price.Add(b.BestAsk.Price).InexactFloat64() / 2
}

// hasMid reports whether a book has both a best bid and a best ask.
func hasMid(b *cex.Book) bool {
	return b.BestBid.Price.IsPositive() && b.BestAsk.Price.IsPositive()
}
