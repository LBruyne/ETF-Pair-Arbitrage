package cex

import (
	"context"
	"errors"
	"time"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/logger"
)

// anchorState is the anchor state of one Session, owned by the anchor loop.
// A Session starts without an anchor, fixes the F of its closing minute and
// gets the bell anchor a minute or two after its opening close, and switches
// to the issuer's NAV at the first read that finds both legs' NAV of the
// Session's trading day (owner items 96-97). For nav_watch after that the
// NAV is read on, and a new value switches the anchor again; after the
// watch nothing is read until the next Session.
type anchorState struct {
	session Session

	// the bell anchor: F x the index close of the minute before the close
	bellF       decimal.Decimal    // F of that minute, fixed for the Session the first time it is available
	bellFSource string             // where it came from, as stored (fSourceCandle; a fact restored from an older run may say "own")
	bellFAt     time.Time          // when F was fixed; zero until then
	bell        [2]decimal.Decimal // by leg; zero until read
	bellAt      time.Time          // when both were read; zero until then
	nextBell    time.Time          // the next read of what the bell anchor still lacks

	// the issuer's NAV of the trading day that starts the Session
	navSeen     [2]decimal.Decimal // by leg: the value last read; zero until the first
	nav         [2]decimal.Decimal // by leg: the NAV the anchor uses; valid once navAt is set
	navAt       time.Time          // when the NAV first replaced the bell anchor, the start of the watch; zero until then
	navSwitchAt time.Time          // when the anchor last switched to a NAV: navAt, or a new value in the watch
	nextNAV     time.Time          // the next read of the issuer

	// the dividends whose entry ban reaches into this Session
	dividends []*dividend
}

// FactBellF is the kind of the anchor fact that keeps the F fixed for a
// Session's bell anchor, one row per leg with the same value, so that a
// restart while the index is still awaited uses the same F.
const FactBellF = "bell_f"

// fSourceCandle is the source of a bell anchor's F as a bell_f fact stores
// it: the close of the source's 1-minute candle of the closing minute
// (owner item 69).
const fSourceCandle = "candle"

// payoutDay is one configured dividend with the times that follow from its
// ex-date.
type payoutDay struct {
	leg       Leg
	ticker    string
	exDate    string
	banFrom   time.Time // 04:00 New York of the carrier day: entries stop
	effective time.Time // T, 20:00 New York of the calendar day before the ex-date: the anchor drops by the amount
}

// dividend is a payoutDay within one Session.
type dividend struct {
	payoutDay
	inSession bool            // T lies in this Session: the amount is read and the anchor adjusted here
	amount    decimal.Decimal // USD per share; valid once amountAt is set
	amountAt  time.Time       // when the amount was read; zero until then
	nextRead  time.Time       // the next read of the issuer's distributions
}

// splitBan is one configured split: entries stop at from and stay stopped
// until the entry is removed from the configuration and the process
// restarted.
type splitBan struct {
	ticker, exDate string
	from           time.Time // 04:00 New York of the last trading day before the ex-date
}

// bindCorporateActions turns the configured corporate actions into their
// times. NewManager calls it once; the configuration was validated.
func (m *CexManager) bindCorporateActions() {
	for _, action := range m.cfg.CorporateActions {
		leg := Bull
		if action.Ticker == m.tickers[Bear] {
			leg = Bear
		}
		banFrom := m.calendar.Carrier(m.calendar.CarrierDay(action.ExDate))
		if action.Kind == ActionSplit {
			m.splits = append(m.splits, splitBan{ticker: action.Ticker, exDate: action.ExDate, from: banFrom})
			continue
		}
		eve := parseDay(action.ExDate).AddDate(0, 0, -1)
		m.payouts = append(m.payouts, payoutDay{leg: leg, ticker: action.Ticker, exDate: action.ExDate, banFrom: banFrom,
			effective: time.Date(eve.Year(), eve.Month(), eve.Day(), 20, 0, 0, 0, NewYork)})
	}
}

// Restore hands the manager what the previous run stored, before Run: the
// anchor facts of the Session the process restarts in, so the bell anchor,
// the NAV in use, its watch and every known-at time continue instead of
// starting over, and the unfinished cycle's order records, which go into the
// order table. An order read back without a final state becomes unknown and
// is queried at the venue.
func (m *CexManager) Restore(facts []AnchorFact, orders []OrderRecord) {
	m.restored = facts
	m.restoreOrders(orders)
}

// Anchors returns the anchors as last published. They may still be those of
// the Session that just ended: the anchor loop publishes the new Session at
// its next beat, which a read in flight can delay by seconds, while
// Session(now) follows the calendar at once. A reader compares
// Anchors.SessionID with the ID of the Session it is in and treats the
// anchors of another Session as no anchor.
func (m *CexManager) Anchors() Anchors { return m.reference.Load().anchors }

// NAVHotSwitchD is the configured nav_hot_switch_d, in spread units: exec
// keeps the mean and sigma through a NAV switch whose spread jump is below
// it.
func (m *CexManager) NAVHotSwitchD() float64 { return m.cfg.NAVHotSwitchD }

// --- the anchor loop ---

// runAnchors advances the anchor state every anchorTick until ctx ends.
func (m *CexManager) runAnchors(ctx context.Context) {
	ticker := time.NewTicker(m.timing.anchorTick)
	defer ticker.Stop()
	for {
		m.stepAnchors(ctx, m.timing.clock())
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// stepAnchors advances the anchor state to now: a new Session when a regular
// close has passed, then the reads that are due. The anchors are published
// after each stage if anything about them changed, so a new Session or a
// bell anchor does not wait for the issuer's slower reads. The reads block
// this loop only, each for at most sourceCall.
func (m *CexManager) stepAnchors(ctx context.Context, now time.Time) {
	if s := m.calendar.SessionAt(now); s.ID != m.anchor.session.ID {
		m.startSession(s, now)
	}
	m.publishAnchors(now) // a new Session, or a ban that begins or ends with the clock
	m.readBell(ctx, now)
	m.publishAnchors(now)
	m.readNAV(ctx, now)
	m.readDividends(ctx, now)
	m.publishAnchors(now)
	m.checkDistributions(ctx, now)
}

// startSession resets the anchor state for the Session s. At the first
// Session after a restart it takes over what the previous run stored for it.
func (m *CexManager) startSession(s Session, now time.Time) {
	log := logger.GlobalLogger
	a := anchorState{session: s, nextBell: s.Start.Add(m.timing.bellDelay), nextNAV: s.Start}
	for _, p := range m.payouts {
		// The ban of a dividend runs from the pre-market of its carrier day
		// to T, so it touches the Session that ends at the carrier day's
		// close and the Session after it, in which T lies.
		if p.banFrom.Before(s.End) && !p.effective.Before(s.Start) {
			a.dividends = append(a.dividends, &dividend{payoutDay: p, inSession: p.effective.Before(s.End), nextRead: s.Start})
		}
	}
	a.restore(m.restored)
	m.restored = nil
	m.anchor = a
	log.Info("cex: session", "session", s.ID, "start", showTime(s.Start), "regular_open", showTime(s.RegularOpen),
		"end", showTime(s.End), "restored_bell", !a.bellAt.IsZero(), "restored_nav", !a.navAt.IsZero(), "dividends", len(a.dividends))

	// A configured split keeps entries stopped until a person has checked
	// the anchors and the positions in the new units and removed it: say so
	// at every Session start.
	for _, split := range m.splits {
		if !now.Before(split.from) {
			log.Error("cex: split configured: no entries until it is checked and removed from corporate_actions", "ticker", split.ticker, "ex_date", split.exDate)
			m.recordEvent("error", "split_entry_ban", "a configured split stops entries until it is checked by hand and removed from corporate_actions",
				map[string]any{"ticker": split.ticker, "ex_date": split.exDate, "session": s.ID})
		}
	}
}

// restore takes over the stored facts of this Session: the F fixed for the
// bell anchor, the bell anchor, the NAV values last seen, the NAV of the
// latest switch with the time of the first one (the watch runs from there)
// and the dividend amounts. Facts of another Session are ignored; of each
// kind and leg the latest counts.
func (a *anchorState) restore(facts []AnchorFact) {
	var bellLegs, navLegs [2]bool
	var bellAt, navFirst, navLast time.Time
	var nav [2]decimal.Decimal
	var seenAt, navLegAt [2]time.Time
	for _, f := range facts {
		if f.SessionID != a.session.ID {
			continue
		}
		switch f.Kind {
		case FactBellF:
			a.bellF, a.bellFSource, a.bellFAt = f.Value, f.Source, f.KnownAt
		case FactBell:
			a.bell[f.Leg], bellLegs[f.Leg], bellAt = f.Value, true, latest(bellAt, f.KnownAt)
		case FactNAVSeen:
			if !f.KnownAt.Before(seenAt[f.Leg]) {
				a.navSeen[f.Leg], seenAt[f.Leg] = f.Value, f.KnownAt
			}
		case FactNAV:
			if !f.KnownAt.Before(navLegAt[f.Leg]) {
				nav[f.Leg], navLegAt[f.Leg] = f.Value, f.KnownAt
			}
			if navFirst.IsZero() || f.KnownAt.Before(navFirst) {
				navFirst = f.KnownAt
			}
			navLegs[f.Leg], navLast = true, latest(navLast, f.KnownAt)
		case FactDividend:
			for _, d := range a.dividends {
				if d.inSession && d.leg == f.Leg {
					d.amount, d.amountAt = f.Value, f.KnownAt
				}
			}
		}
	}
	if bellLegs[Bull] && bellLegs[Bear] {
		a.bellAt = bellAt
	} else {
		a.bell = [2]decimal.Decimal{}
	}
	if navLegs[Bull] && navLegs[Bear] {
		// The NAV in use is what a later read is compared with.
		a.nav, a.navSeen, a.navAt, a.navSwitchAt = nav, nav, navFirst, navLast
	}
}

// readBell reads the bell anchor once it is due: for each leg the close of
// the index kline of the minute before the Session's opening close, times F
// of that minute. F is the close of the source's 1-minute candle of that
// minute (owner item 69), whether or not the process was running then. It
// is fixed for the Session the first time it is read and stored, so an
// index that arrives late, or a restart, is still multiplied by the same F
// even if the source revises its candle meanwhile. Until
// F and both index closes are there the Session has no bell anchor; a
// missing or failed F is read again after referenceRetry, a missing index
// every anchorPoll. What is missing is
// logged as it is: an input the source does not have yet is expected and an
// Info line, a read that failed is a Warn with its reason.
func (m *CexManager) readBell(ctx context.Context, now time.Time) {
	a := &m.anchor
	if !a.bellAt.IsZero() || now.Before(a.nextBell) {
		return
	}
	log := logger.GlobalLogger
	minute := a.session.Start.Add(-time.Minute)
	// retry puts the read off by wait and says why: message for an input
	// that is not there yet, err for a read that failed.
	retry := func(message, input string, err error, wait time.Duration) {
		a.nextBell = now.Add(wait)
		if err != nil {
			log.Warn("cex: bell anchor read failed; retrying", "session", a.session.ID, "read", input, "minute", showTime(minute), "err", err, "retry_in", wait)
			return
		}
		log.Info(message, "session", a.session.ID, "missing", input, "minute", showTime(minute), "retry_in", wait)
	}

	// F of the minute, unless it is fixed already.
	if a.bellFAt.IsZero() {
		callCtx, cancel := context.WithTimeout(ctx, m.timing.sourceCall)
		f, ok, err := m.fxSource.MinuteClose(callCtx, minute)
		cancel()
		if err != nil || !ok {
			// The candle is read again soon: without it there is no bell
			// anchor, and one failed read must not hold it up for an
			// anchorPoll.
			retry("cex: bell F of the minute missing; retrying", "F candle of the minute", err, m.timing.referenceRetry)
			return
		}
		a.bellF, a.bellFSource, a.bellFAt = f, fSourceCandle, m.timing.clock()
		for _, leg := range Legs {
			m.recordFact(AnchorFact{SessionID: a.session.ID, Kind: FactBellF, Leg: leg, Value: f, F: f, Source: fSourceCandle, KnownAt: a.bellFAt}, map[string]any{"minute_ms": minute.UnixMilli()})
		}
		log.Info("cex: bell F fixed", "session", a.session.ID, "f", f, "f_source", fSourceCandle, "minute", showTime(minute))
	}

	// The index close of each leg.
	var index [2]decimal.Decimal
	for _, leg := range Legs {
		callCtx, cancel := context.WithTimeout(ctx, m.timing.sourceCall)
		price, ok, err := m.index.IndexClose(callCtx, m.symbols[leg], minute)
		cancel()
		if err != nil || !ok {
			retry("cex: bell index not ready; retrying", "index kline of "+string(m.symbols[leg]), err, m.timing.anchorPoll)
			return
		}
		index[leg] = price
	}

	a.bellAt = m.timing.clock()
	for _, leg := range Legs {
		a.bell[leg] = a.bellF.Mul(index[leg])
		m.recordFact(AnchorFact{SessionID: a.session.ID, Kind: FactBell, Leg: leg, Value: a.bell[leg], F: a.bellF, Source: "binance", KnownAt: a.bellAt},
			map[string]any{"index_usdt": index[leg], "minute_ms": minute.UnixMilli(), "f_source": a.bellFSource})
	}
	log.Info("cex: bell anchor read", "session", a.session.ID, "bull_usd", a.bell[Bull], "bear_usd", a.bell[Bear], "f", a.bellF, "f_source", a.bellFSource,
		"index_bull_usdt", index[Bull], "index_bear_usdt", index[Bear])
}

// readNAV asks the issuer for both funds' NAV of the Session's trading day,
// every anchorPoll from the Session's start (owner items 96-97). A value
// read for the first time, or different from the one before, is a nav_seen
// fact. The first round that reads both legs switches the anchor from the
// bell anchor to the NAV, without a settle delay. For nav_watch after that
// switch the reads go on: a round that finds a new value of either leg
// switches the anchor to it at once, the other leg keeping its value; exec
// decides whether that warms the mean up again. Each switch is a nav fact
// of both legs. After the watch the NAV is not read again in the Session.
func (m *CexManager) readNAV(ctx context.Context, now time.Time) {
	a := &m.anchor
	watching := !a.navAt.IsZero()
	if m.issuerOff || now.Before(a.nextNAV) || watching && !now.Before(a.navAt.Add(m.cfg.NAVWatch)) {
		return
	}
	log := logger.GlobalLogger
	a.nextNAV = now.Add(m.timing.anchorPoll)
	tradeDate := parseDay(a.session.ID)
	read, changed := 0, false
	for _, leg := range Legs {
		callCtx, cancel := context.WithTimeout(ctx, m.timing.sourceCall)
		nav, ok, err := m.issuer.NAV(callCtx, m.tickers[leg], tradeDate)
		cancel()
		switch {
		case errors.Is(err, ErrIssuerKey):
			m.stopIssuer(err)
			return
		case err != nil:
			log.Warn("cex: nav read failed; retrying", "ticker", m.tickers[leg], "session", a.session.ID, "err", err, "retry_in", m.timing.anchorPoll)
			continue
		case !ok:
			log.Debug("cex: nav not published yet", "ticker", m.tickers[leg], "session", a.session.ID)
			continue
		}
		read++
		if !nav.Equal(a.navSeen[leg]) {
			seenAt := m.timing.clock()
			log.Info("cex: nav seen", "ticker", m.tickers[leg], "session", a.session.ID, "from", a.navSeen[leg], "to", nav)
			a.navSeen[leg], changed = nav, true
			m.recordFact(AnchorFact{SessionID: a.session.ID, Kind: FactNAVSeen, Leg: leg, Value: nav, Source: "direxion", KnownAt: seenAt}, map[string]any{"trade_date": a.session.ID})
		}
	}
	// The first switch needs a round that read both legs; in the watch a
	// new value of either leg is a switch.
	if watching && !changed || !watching && read < len(Legs) {
		return
	}
	at := m.timing.clock()
	if !watching {
		a.navAt = at
	}
	from := a.nav
	a.nav, a.navSwitchAt = a.navSeen, at
	for _, leg := range Legs {
		m.recordFact(AnchorFact{SessionID: a.session.ID, Kind: FactNAV, Leg: leg, Value: a.nav[leg], Source: "direxion", KnownAt: at},
			map[string]any{"trade_date": a.session.ID, "watch_until_ms": a.navAt.Add(m.cfg.NAVWatch).UnixMilli()})
	}
	log.Info("cex: nav switched", "session", a.session.ID, "first", !watching, "from_bull_usd", from[Bull], "from_bear_usd", from[Bear],
		"bull_usd", a.nav[Bull], "bear_usd", a.nav[Bear], "watch_until", showTime(a.navAt.Add(m.cfg.NAVWatch)))
}

// readDividends reads the amount of each dividend that takes effect in this
// Session, every anchorPoll from the Session's start (the carrier day's
// close) until it is read.
func (m *CexManager) readDividends(ctx context.Context, now time.Time) {
	log := logger.GlobalLogger
	for _, d := range m.anchor.dividends {
		if m.issuerOff || !d.inSession || !d.amountAt.IsZero() || now.Before(d.nextRead) {
			continue
		}
		d.nextRead = now.Add(m.timing.anchorPoll)
		callCtx, cancel := context.WithTimeout(ctx, m.timing.sourceCall)
		distributions, err := m.issuer.Distributions(callCtx, d.ticker)
		cancel()
		if errors.Is(err, ErrIssuerKey) {
			m.stopIssuer(err)
			return
		}
		if err != nil {
			log.Warn("cex: dividend read failed; retrying", "ticker", d.ticker, "ex_date", d.exDate, "err", err, "retry_in", m.timing.anchorPoll)
			continue
		}
		for _, dist := range distributions {
			if dist.ExDate != d.exDate || !dist.Declared {
				continue
			}
			d.amount, d.amountAt = dist.Amount, m.timing.clock()
			log.Info("cex: dividend amount read", "ticker", d.ticker, "ex_date", d.exDate, "amount_usd", d.amount, "effective", showTime(d.effective))
			m.recordFact(AnchorFact{SessionID: m.anchor.session.ID, Kind: FactDividend, Leg: d.leg, Value: d.amount, Source: "direxion", KnownAt: d.amountAt},
				map[string]any{"ex_date": d.exDate, "effective_ms": d.effective.UnixMilli()})
		}
	}
}

// checkDistributions compares, once a day, the ex-dates the issuer lists for
// the two funds with the configuration. An ex-date still ahead (after the
// trading day that started this Session) that corporate_actions does not
// hold is an alarm: a person adds it; the process does not.
func (m *CexManager) checkDistributions(ctx context.Context, now time.Time) {
	if m.issuerOff || now.Before(m.nextDistributionCheck) {
		return
	}
	log := logger.GlobalLogger
	m.nextDistributionCheck = now.Add(m.timing.distributionPoll)
	for _, leg := range Legs {
		callCtx, cancel := context.WithTimeout(ctx, m.timing.sourceCall)
		distributions, err := m.issuer.Distributions(callCtx, m.tickers[leg])
		cancel()
		if errors.Is(err, ErrIssuerKey) {
			m.stopIssuer(err)
			return
		}
		if err != nil {
			log.Warn("cex: distributions read failed; retrying", "ticker", m.tickers[leg], "err", err, "retry_in", m.timing.anchorPoll)
			m.nextDistributionCheck = now.Add(m.timing.anchorPoll)
			continue
		}
		for _, dist := range distributions {
			if dist.ExDate <= m.anchor.session.ID || m.dividendConfigured(leg, dist.ExDate) {
				continue
			}
			log.Error("cex: ex-date missing from corporate_actions", "ticker", m.tickers[leg], "ex_date", dist.ExDate, "declared", dist.Declared, "amount_usd", dist.Amount)
			m.recordEvent("error", "ex_date_not_configured", "the issuer lists an ex-date that corporate_actions does not hold: add it by hand",
				map[string]any{"ticker": m.tickers[leg], "ex_date": dist.ExDate, "declared": dist.Declared, "amount": dist.Amount})
		}
	}
}

// dividendConfigured reports whether corporate_actions holds a dividend of
// the leg's fund with this ex-date.
func (m *CexManager) dividendConfigured(leg Leg, exDate string) bool {
	for _, p := range m.payouts {
		if p.leg == leg && p.exDate == exDate {
			return true
		}
	}
	return false
}

// stopIssuer ends the reads of the issuer for this run: its key is missing
// or was refused, which a retry does not cure. The Session stays on the bell
// anchor and no dividend amount is read.
func (m *CexManager) stopIssuer(err error) {
	m.issuerOff = true
	logger.GlobalLogger.Error("cex: issuer not readable; the bell anchor is used throughout and dividend amounts are not read", "err", err)
	m.recordEvent("error", "issuer_key", "the issuer's API key is missing or was refused: no NAV and no dividend amounts until a restart with a working key", nil)
}

// publishAnchors publishes the anchors as they are at now, when they differ
// from what is published.
func (m *CexManager) publishAnchors(now time.Time) {
	next := m.anchor.anchors(now, m.splitActive(now))
	old := m.reference.Load().anchors
	if sameAnchors(old, next) {
		return
	}
	m.editReference(func(v *referenceView) { v.anchors = next })
	logger.GlobalLogger.Info("cex: anchor changed", "session", next.SessionID, "from", string(old.Kind), "to", string(next.Kind), "bull_usd", next.Bull, "bear_usd", next.Bear,
		"known_at", showTime(next.KnownAt), "entry_ban", next.EntryBan, "restart_at", showTime(next.RestartAt))
}

// splitActive reports whether a configured split's entry ban has begun.
func (m *CexManager) splitActive(now time.Time) bool {
	for _, split := range m.splits {
		if !now.Before(split.from) {
			return true
		}
	}
	return false
}

// anchors builds the published value from the state at now. Both legs use
// the NAV once it has replaced the bell anchor, else the bell anchor once it
// is read, else nothing. A dividend bans entries from the pre-market of its carrier day to
// its effective time T. From T on the mean and sigma restart at T and the
// leg's anchor is lowered by the amount, whichever kind the anchor is and
// whenever it became known; while the amount is not read the anchor stays as
// it was and the ban goes on.
func (a *anchorState) anchors(now time.Time, splitBan bool) Anchors {
	out := Anchors{SessionID: a.session.ID, EntryBan: splitBan}
	var value [2]decimal.Decimal
	switch {
	case !a.navAt.IsZero():
		out.Kind, value, out.KnownAt = AnchorNAV, a.nav, a.navSwitchAt
	case !a.bellAt.IsZero():
		out.Kind, value, out.KnownAt = AnchorBell, a.bell, a.bellAt
	}
	out.SwitchedAt = out.KnownAt
	for _, d := range a.dividends {
		switch {
		case now.Before(d.banFrom):
			// not yet in its window
		case now.Before(d.effective):
			out.EntryBan = true
		case d.amountAt.IsZero():
			out.EntryBan = true
			out.RestartAt = latest(out.RestartAt, d.effective)
		default:
			out.RestartAt = latest(out.RestartAt, d.effective)
			value[d.leg] = value[d.leg].Sub(d.amount)
			if out.Kind != AnchorNone {
				out.KnownAt = latest(out.KnownAt, d.effective, d.amountAt)
			}
		}
	}
	if out.Kind != AnchorNone {
		out.Bull, out.Bear = value[Bull], value[Bear]
	}
	return out
}

// sameAnchors compares two published values.
func sameAnchors(a, b Anchors) bool {
	return a.SessionID == b.SessionID && a.Kind == b.Kind && a.Bull.Equal(b.Bull) && a.Bear.Equal(b.Bear) &&
		a.KnownAt.Equal(b.KnownAt) && a.SwitchedAt.Equal(b.SwitchedAt) && a.EntryBan == b.EntryBan && a.RestartAt.Equal(b.RestartAt)
}

// latest returns the latest of the given times.
func latest(times ...time.Time) time.Time {
	var out time.Time
	for _, t := range times {
		if t.After(out) {
			out = t
		}
	}
	return out
}
