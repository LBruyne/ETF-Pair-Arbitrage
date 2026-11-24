package scan

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"os"
	"sync"
	"time"
)

// Manager fills the board's tables: a candle backfill at start, then each
// contract's candle close as each minute ends and USD per USDT on one
// goroutine; the calendar and its check against Binance's schedule, the bell
// anchors, the issuers' NAVs and the ETFs' corporate actions and their checks
// on another; the per-minute summary of the recorder's rows (Data Coverage's
// counts, the minute bars' end rows) on a third.
// A failed read is logged and tried again at the next tick.
type Manager struct {
	cfg     Config
	cal     Calendar // calendar.go's table with the configuration's overrides
	store   *store
	perps   map[string]PerpSource
	index   map[string]IndexSource // by venue: binance, bitget
	fx      Coinbase
	log     *slog.Logger
	started time.Time
	failing map[string]bool // reads that failed last time, so a failure is logged once
	noting  sync.Mutex      // the minute loop and the anchor loop both note reads
	// The rest belongs to the anchor goroutine. navs holds the NAV sources
	// by the name a combo's nav_source gives, Direxion only while its key is
	// set and accepted; navRead is when each combo's issuer was last read;
	// indexFrom is, per Binance contract, the open time of its first index
	// kline from history_start; told holds what was logged once;
	// actionsRead, direxionRead and calendarChecked are when the action
	// sources, Direxion's distributions (the daily read) and Binance's
	// schedule were last read.
	navs            map[string]NAVSource
	navRead         map[string]time.Time
	indexFrom       map[string]int64
	told            map[string]bool
	actionsRead     time.Time
	direxionRead    time.Time
	calendarChecked time.Time
}

// NewManager connects to the database, applies the schema and binds the
// public sources. Without the Direxion key its funds keep their bell anchors.
func NewManager(ctx context.Context, cfg Config, dsn string, log *slog.Logger) (*Manager, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	st, err := openStore(ctx, dsn)
	if err != nil {
		return nil, err
	}
	cal := newCalendar(cfg.CalendarOverrides)
	navs := navSources(os.Getenv(direxionKeyEnv), cal)
	for _, combo := range cfg.Combos {
		if _, ok := navs[combo.NAVSource]; combo.NAVSource != "" && !ok {
			log.Error("scan: no key for the NAV source: its legs stay on their bell anchors", "combo", combo.ID, "nav_source", combo.NAVSource, "env", direxionKeyEnv)
		}
	}
	return &Manager{cfg: cfg, cal: cal, store: st, log: log, failing: map[string]bool{},
		perps: map[string]PerpSource{"binance": Binance{}, "bitget": Bitget{}}, index: map[string]IndexSource{"binance": Binance{}, "bitget": Bitget{}},
		navs: navs, navRead: map[string]time.Time{}, indexFrom: map[string]int64{}, told: map[string]bool{}}, nil
}

// navSources binds the issuers' NAV sources: Direxion only with a key.
func navSources(direxionKey string, cal Calendar) map[string]NAVSource {
	navs := map[string]NAVSource{"proshares": ProShares{}}
	if direxionKey != "" {
		navs["direxion"] = newDirexion(direxionKey, cal)
	}
	return navs
}

// Run seeds the pickers, backfills the minute history and then reads each
// minute's candle closes and USD per USDT until ctx ends.
func (m *Manager) Run(ctx context.Context) error {
	defer m.store.pool.Close()
	m.started = time.Now()
	if err := m.store.seed(ctx, m.cfg); err != nil {
		return err
	}
	end := time.Now().UnixMilli() / minuteMs * minuteMs
	m.sampleFX(ctx)
	m.log.Info("scan: reading", "fx_poll", m.cfg.FXPoll, "anchor_poll", m.cfg.AnchorPoll)

	// The candle history and the anchors are read on their own goroutines:
	// they take seconds to minutes and must not hold the minutes back.
	go m.loadCandles(ctx, m.cfg.HistoryStart.UnixMilli(), end, true)
	go m.watchAnchors(ctx)
	go m.refreshCoverage(ctx)

	// The closes of the minutes just ended are read a second after each
	// minute, and the newest again candleRecheck later: an exchange can
	// still revise it for a few seconds.
	candle, fx := time.NewTimer(untilCandleRead(time.Now())), time.NewTicker(m.cfg.FXPoll)
	defer candle.Stop()
	defer fx.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case at := <-candle.C:
			minute := at.UnixMilli() / minuteMs * minuteMs
			if minute > end {
				m.loadCandles(ctx, minute-15*minuteMs, minute, false)
				end = minute
				candle.Reset(candleRecheck)
			} else {
				m.loadCandles(ctx, minute-2*minuteMs, minute, false)
				candle.Reset(untilCandleRead(time.Now()))
			}
		case <-fx.C:
			m.sampleFX(ctx)
		}
	}
}

// untilCandleRead is the wait from now to one second after the next minute.
func untilCandleRead(now time.Time) time.Duration {
	return time.Duration(minuteMs-now.UnixMilli()%minuteMs+1000) * time.Millisecond
}

// refreshCoverage keeps etfpair.perp_second_minute, the per-minute summary of
// the recorder's rows that the board's Data Coverage and minute bars read, up
// to date: at start and then coverageAt into every minute it re-aggregates
// the minutes of the last coverageWindow up to coverageLag before now, so a
// minute is aggregated for the last time 65 min 20 s after it began; the board
// reads it from the summary only from 66 minutes on. The older minutes are not
// read at start (the deployment fills the history). Only the SQL function
// defines the summary, and it waits for any other refresh's transaction (a
// backfill's); a failure is noted and the next minute tries again.
func (m *Manager) refreshCoverage(ctx context.Context) {
	for {
		now := time.Now()
		_, err := m.store.refreshCoverage(ctx, now.Add(-coverageWindow).UnixMilli(), now.Add(-coverageLag).UnixMilli())
		m.note("coverage refresh", err)
		next := time.NewTimer(untilCoverage(time.Now()))
		select {
		case <-ctx.Done():
			next.Stop()
			return
		case <-next.C:
		}
	}
}

// untilCoverage is the wait from now to coverageAt into the next minute, or
// into this one when that is still ahead.
func untilCoverage(now time.Time) time.Duration {
	wait := (coverageAt.Milliseconds() - now.UnixMilli()%minuteMs + minuteMs) % minuteMs
	if wait == 0 {
		wait = minuteMs
	}
	return time.Duration(wait) * time.Millisecond
}

// watchAnchors writes the calendar, checks it against Binance's schedule,
// reads the corporate actions and backfills the bell anchors and the NAVs of
// past sessions at start, then checks the actions in effect; then it reads
// each session's bell anchors bellDelay after its close, and at every tick
// the bell anchors still missing, the issuers' NAVs (verifyNAVs), the action
// sources and Direxion's distributions when due (actionsDue, direxionDue),
// Binance's schedule hourly, and the checks of the actions that have taken
// effect.
func (m *Manager) watchAnchors(ctx context.Context) {
	m.writeCalendar(ctx)
	m.checkCalendar(ctx)
	m.readActions(ctx)
	m.readDirexionActions(ctx, m.direxionTickers(), true)
	m.loadBells(ctx)
	m.loadPastNAVs(ctx)
	m.verifyNAVs(ctx)
	m.checkActions(ctx)
	tick := time.NewTicker(m.cfg.AnchorPoll)
	defer tick.Stop()
	for {
		next, err := m.cal.nextBell(time.Now())
		if err != nil {
			m.log.Error("scan: calendar", "err", err)
			next = time.Now().Add(24 * time.Hour)
		}
		bell := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			bell.Stop()
			return
		case <-bell.C:
			m.writeCalendar(ctx)
			m.loadBells(ctx)
		case now := <-tick.C:
			bell.Stop()
			m.loadBells(ctx)
			m.verifyNAVs(ctx)
			if m.actionsDue(ctx, now) {
				m.readActions(ctx)
			}
			if due, daily := m.direxionDue(ctx, now); len(due) > 0 {
				m.readDirexionActions(ctx, due, daily)
			}
			if now.Sub(m.calendarChecked) >= time.Hour {
				m.checkCalendar(ctx)
			}
			m.checkActions(ctx)
		}
	}
}

// loadCandles stores the candle closes of [start, end) for every contract,
// replacing a close the exchange has since revised, and for USD per USDT.
// With history it is the backfill: of each series it loads only what the
// table lacks (historyFrom), paced to stay inside the exchanges' request
// limits, and reports what it loaded.
func (m *Manager) loadCandles(ctx context.Context, start, end int64, history bool) {
	for _, combo := range m.cfg.Combos {
		for _, market := range combo.Markets {
			src, ok := m.perps[market.Venue]
			if !ok {
				m.log.Error("scan: no source for venue", "venue", market.Venue)
				continue
			}
			for _, symbol := range market.symbols() {
				rows, first := 0, start
				if history {
					first = m.store.historyFrom(ctx, "perp_minute", "venue = $2 AND symbol = $3 AND minute_ms >= $4", true, start, market.Venue, symbol)
				}
				for from := first; from < end && ctx.Err() == nil; from += candlePage * minuteMs {
					if history {
						time.Sleep(historyPause)
					}
					candles, err := src.Candles(ctx, symbol, from, min(from+candlePage*minuteMs, end))
					if err == nil {
						err = m.store.insertCandles(ctx, market.Venue, symbol, candles, time.Now().UnixMilli())
					}
					m.note("candles "+market.Venue+" "+symbol, err)
					if err == nil {
						rows += len(candles)
					}
				}
				if history {
					m.log.Info("scan: candles loaded", "venue", market.Venue, "symbol", symbol, "from", time.UnixMilli(first).Format(time.DateTime), "minutes", rows)
				}
			}
		}
	}
	if history {
		start = m.store.historyFrom(ctx, "fx_minute", "pair = $2 AND minute_ms >= $3", false, start, m.fx.Pair())
	}
	candles, err := m.fx.Candles(ctx, start, end)
	if err == nil {
		err = m.store.insertFXCandles(ctx, m.fx.Pair(), candles, time.Now().UnixMilli())
	}
	m.note("candles "+m.fx.Pair(), err)
	if history && err == nil {
		m.log.Info("scan: fx candles loaded", "pair", m.fx.Pair(), "minutes", len(candles))
	}
}

// sampleFX stores Coinbase's best bid and ask of USD per USDT as the
// current minute's price.
func (m *Manager) sampleFX(ctx context.Context) {
	q, err := m.fx.Quote(ctx)
	now := time.Now().UnixMilli()
	if err == nil {
		err = m.store.upsertFXQuote(ctx, m.fx.Pair(), now/minuteMs*minuteMs, q, now)
	}
	m.note("fx "+m.fx.Pair(), err)
}

// note logs a read's first failure and its recovery, not every repeat. A
// request cancelled by the shutdown is not a failure.
func (m *Manager) note(what string, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	m.noting.Lock()
	defer m.noting.Unlock()
	switch {
	case err != nil && !m.failing[what]:
		m.failing[what] = true
		m.log.Warn("scan: read failing", "read", what, "err", err)
	case err == nil && m.failing[what]:
		delete(m.failing, what)
		m.log.Info("scan: read recovered", "read", what)
	}
}

// once reports whether key is new to this run, for what is logged once.
func (m *Manager) once(key string) bool {
	if m.told[key] {
		return false
	}
	m.told[key] = true
	return true
}

// writeCalendar stores the sessions from history_start to a week ahead and
// their phases: a minute belongs to the session of the newest close at or
// before it, so nothing before the first close has a session or a spread.
func (m *Manager) writeCalendar(ctx context.Context) {
	from, err := time.ParseInLocation("2006-01-02", m.cfg.historyDay(), newYork)
	if err != nil {
		m.log.Error("scan: calendar", "err", err)
		return
	}
	days := m.cal.tradingDays(from, time.Now().AddDate(0, 0, 7))
	sessions := make([]session, len(days))
	for i, date := range days {
		closeAt, err := m.cal.closeOf(date)
		if err != nil {
			m.log.Error("scan: calendar", "err", err)
			return
		}
		sessions[i] = session{Date: date, CloseMs: closeAt.UnixMilli()}
	}
	phases, err := m.cal.phasesOf(days)
	if err != nil {
		m.log.Error("scan: calendar", "err", err)
		return
	}
	if err := m.store.writeCalendar(ctx, sessions, phases); err != nil {
		m.log.Warn("scan: calendar not stored", "err", err)
	}
}

// loadBells stores the bell anchor of every ETF of the combos for every
// session that has closed and lacks a valid one (none, or one read at a
// close that has moved since): USD per USDT times the index price of the
// ETF's perpetual at the close, the close of the index kline that opens one
// minute before it. The index is Binance's; a session before the contract's
// first Binance index kline takes Bitget's, and without a Bitget contract it
// has none and is not asked for. The newest session goes first. A session
// whose kline at the close is missing, or without USD per USDT in the five
// minutes before the close, has no bell anchor (no other substitute): it is
// logged once and asked again at the next tick.
func (m *Manager) loadBells(ctx context.Context) {
	for ticker, symbols := range m.cfg.bellSymbols() {
		due, err := m.store.bellsDue(ctx, ticker, time.Now().UnixMilli())
		if err != nil {
			m.log.Warn("scan: bell sessions lookup failed", "err", err)
			return
		}
		if len(due) == 0 {
			continue
		}
		// from: where Binance's index of the contract starts. Without a Binance
		// kline from history_start, every session is before it.
		from, ok := m.indexFrom[symbols["binance"]]
		if !ok {
			from = math.MaxInt64
			if symbol, listed := symbols["binance"]; listed {
				first, err := m.index["binance"].IndexKline(ctx, symbol, m.cfg.HistoryStart.UnixMilli())
				m.note("index binance "+symbol, err)
				time.Sleep(bellPause)
				if err != nil {
					continue
				}
				if first != nil {
					from, m.indexFrom[symbol] = first.MinuteMs, first.MinuteMs
				}
			}
		}
		stored, before := map[string]int{}, 0
		var newest, newestUSD string
		for _, s := range due {
			if ctx.Err() != nil {
				return
			}
			if !s.HasFX {
				if m.once("bell fx " + ticker + " " + s.Date) {
					m.log.Info("scan: no USD per USDT in the five minutes before the close: no bell anchor yet", "ticker", ticker, "session", s.Date)
				}
				continue
			}
			kline, venue := s.CloseMs-minuteMs, "binance"
			if kline < from {
				venue = "bitget"
			}
			symbol, listed := symbols[venue]
			if !listed {
				before++
				continue
			}
			k, err := m.index[venue].IndexKline(ctx, symbol, kline)
			m.note("index "+venue+" "+symbol, err)
			time.Sleep(bellPause)
			switch {
			case err != nil:
				continue // asked again at the next tick
			case k == nil || k.MinuteMs != kline:
				if m.once("bell " + ticker + " " + s.Date) {
					next := "none"
					if k != nil {
						next = time.UnixMilli(k.MinuteMs).Format(time.DateTime)
					}
					m.log.Info("scan: no index kline at the close: no bell anchor", "ticker", ticker, "venue", venue, "symbol", symbol,
						"session", s.Date, "next_kline", next)
				}
				continue
			}
			known := time.Now()
			if !time.UnixMilli(s.CloseMs).After(m.started) {
				known = time.UnixMilli(s.CloseMs).Add(bellBackfillKnown)
			}
			price, err := m.store.insertBell(ctx, ticker, s.Date, venue, *k, known.UnixMilli(), time.Now().UnixMilli())
			if err != nil {
				m.log.Warn("scan: bell anchor not stored", "ticker", ticker, "session", s.Date, "err", err)
				continue
			}
			if price != "" {
				if stored[venue]++; newest == "" {
					newest, newestUSD = s.Date, price
				}
			}
		}
		if before > 0 && m.once("bell before "+ticker) {
			first := "none"
			if from != math.MaxInt64 {
				first = time.UnixMilli(from).Format(time.DateTime)
			}
			m.log.Info("scan: sessions before Binance lists the contract have no bell anchor: no Bitget contract", "ticker", ticker,
				"first_binance_kline", first, "sessions", before)
		}
		if stored["binance"]+stored["bitget"] > 0 {
			m.log.Info("scan: bell anchors stored", "ticker", ticker, "binance", stored["binance"], "bitget", stored["bitget"],
				"newest", newest, "newest_usd", newestUSD)
		}
	}
}

// loadPastNAVs fills in, once at start, the NAVs of the closed sessions that
// have none, the newest too, from the issuer's records of past days:
// ProShares' NAV files, the recent days Direxion's API serves. They are
// confirmed as they are loaded.
func (m *Manager) loadPastNAVs(ctx context.Context) {
	now := time.Now()
	for _, combo := range m.cfg.Combos {
		for _, ticker := range []string{combo.BullTicker, combo.BearTicker} {
			src, ok := m.navs[combo.NAVSource]
			if !ok {
				break
			}
			due, err := m.store.sessionsWithoutNAV(ctx, ticker, now.UnixMilli()+1)
			if err != nil {
				m.log.Warn("scan: NAV sessions lookup failed", "err", err)
				continue
			}
			if len(due) == 0 {
				continue
			}
			navs, err := src.Past(ctx, ticker, due)
			if m.keyRejected(combo.NAVSource, err) {
				break
			}
			m.note("past navs "+combo.NAVSource+" "+ticker, err)
			if err != nil {
				continue
			}
			if err := m.store.insertPastNAVs(ctx, navs, time.Now().UnixMilli()); err != nil {
				m.log.Warn("scan: past NAVs not stored", "ticker", ticker, "err", err)
				continue
			}
			m.log.Info("scan: past NAVs loaded", "ticker", ticker, "nav_source", combo.NAVSource, "sessions_without", len(due), "loaded", len(navs))
		}
	}
}

// verifyNAVs reads each combo's issuer at every tick from a session's close
// until both legs of that session have a NAV anchor, and hourly afterwards
// to catch a correction; Direxion only until then (the owner authorised its
// site API "直到 NAV 稳定", OWNER.md item 4).
func (m *Manager) verifyNAVs(ctx context.Context) {
	now := time.Now()
	session, err := m.cal.lastClosedSession(now)
	if err != nil {
		m.log.Error("scan: calendar", "err", err)
		return
	}
	for _, combo := range m.cfg.Combos {
		src, ok := m.navs[combo.NAVSource]
		if !ok {
			continue
		}
		if read, ok := m.navRead[combo.ID]; combo.NAVSource == "direxion" || ok && now.Sub(read) < time.Hour {
			done, err := m.store.hasNAV(ctx, combo.BullTicker, combo.BearTicker, session)
			if err != nil {
				m.log.Warn("scan: NAV lookup failed", "err", err)
				continue
			}
			if done {
				continue
			}
		}
		m.navRead[combo.ID] = now
		m.readNAVs(ctx, combo, src, session)
	}
}

// readNAVs reads the issuer's figures of the combo's two legs for the newest
// closed session and records every figure, logging once per session when
// the issuer does not show that session's NAV of a leg yet. When both legs
// show the same day, that day's NAVs are confirmed once they are stable.
func (m *Manager) readNAVs(ctx context.Context, combo Combo, src NAVSource, newest string) {
	legs := []string{combo.BullTicker, combo.BearTicker}
	navs, err := src.Latest(ctx, legs, newest)
	if m.keyRejected(combo.NAVSource, err) {
		return
	}
	m.note("navs "+combo.NAVSource+" "+combo.ID, err)
	if err != nil {
		return
	}
	if err := m.store.insertNAVObs(ctx, navs); err != nil {
		m.log.Warn("scan: NAV observations not stored", "err", err)
		return
	}
	for _, ticker := range legs {
		shown := "none"
		for _, n := range navs {
			if n.Ticker == ticker {
				shown = n.SessionDate
			}
		}
		if shown != newest && m.once("nav "+combo.NAVSource+" "+combo.ID+" "+newest) {
			m.log.Info("scan: the issuer shows no NAV of the session yet", "nav_source", combo.NAVSource, "combo", combo.ID, "session", newest,
				"ticker", ticker, "shown", shown)
		}
	}
	if len(navs) == 2 && navs[0].SessionDate == navs[1].SessionDate {
		m.confirmNAV(ctx, combo, navs[0].SessionDate)
	}
}

// confirmNAV makes the issuer's NAVs of a day the anchors of its session
// once the figure of each leg is stable (settledNAV), confirmed at the time
// of writing, and logs each anchor that changes: the first one, or a
// correction.
func (m *Manager) confirmNAV(ctx context.Context, combo Combo, sessionDate string) {
	legs := []string{combo.BullTicker, combo.BearTicker}
	settled := make([]navObs, len(legs))
	for i, ticker := range legs {
		obs, err := m.store.navObs(ctx, combo.NAVSource, ticker, sessionDate)
		if err != nil {
			m.log.Warn("scan: NAV observations lookup failed", "err", err)
			return
		}
		var stable bool
		if settled[i], stable = settledNAV(obs, time.Now(), m.cfg.NAVSettleDelay); !stable {
			return
		}
	}
	for i, ticker := range legs {
		nav := NAV{Issuer: combo.NAVSource, Ticker: ticker, SessionDate: sessionDate, Price: settled[i].Price, Via: settled[i].Via}
		changed, err := m.store.confirmNAV(ctx, nav, time.Now().UnixMilli())
		switch {
		case err != nil:
			m.log.Warn("scan: NAV anchor not stored", "ticker", ticker, "session", sessionDate, "err", err)
		case changed:
			m.log.Info("scan: NAV anchor stored", "ticker", ticker, "session", sessionDate, "nav_usd", nav.Price, "nav_source", nav.Issuer, "via", nav.Via)
		}
	}
}

// keyRejected reports whether err is a NAV source refusing its key. The
// first refusal is logged at Error and the source is not asked again in
// this run: its funds stay on their bell anchors.
func (m *Manager) keyRejected(source string, err error) bool {
	if !errors.Is(err, errKeyRejected) {
		return false
	}
	m.log.Error("scan: the NAV source rejected its key: its legs stay on their bell anchors", "nav_source", source, "env", direxionKeyEnv, "err", err)
	delete(m.navs, source)
	return true
}

// settledNAV returns the figure of one ticker and day that is stable at now:
// the figure read last, once delay has passed since the first read of its
// current run with no other figure read since. The run starts at the
// figure's first read, or, when another figure was read after that, at the
// other figure's last read: a changed figure restarts the clock, and a
// figure that comes back is counted from the read before its return (at
// most one poll early). ok is false while no figure is stable.
func settledNAV(obs []navObs, now time.Time, delay time.Duration) (navObs, bool) {
	if len(obs) == 0 {
		return navObs{}, false
	}
	current := obs[0]
	for _, o := range obs[1:] {
		if o.SeenMs > current.SeenMs {
			current = o
		}
	}
	start := current.ReceivedMs
	for _, o := range obs {
		if o.Price != current.Price && o.SeenMs > start {
			start = o.SeenMs
		}
	}
	return current, now.UnixMilli()-start >= delay.Milliseconds()
}
