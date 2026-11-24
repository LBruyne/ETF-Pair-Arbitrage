package scan

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// The corporate actions of the combos' ETFs: what each source says is put on
// record (readActions), the anchors follow it in the database (combo_actions,
// combo_anchors), and once an action has taken effect it is checked against
// the exchanges' and the issuers' data (checkActions).

const (
	// actionsEvery is how often the action sources are read outside a
	// carrier window (actionsDue).
	actionsEvery = 24 * time.Hour
	// sourcePause spaces the requests to one host.
	sourcePause = time.Second
	// checkDelay is how long after an instant its first minute is final.
	checkDelay = 2 * time.Minute
)

// The bands of the checks (DESIGN v4.1 B.5).
const (
	stepLow, stepHigh = 0.5, 1.5 // the legs' first-minute basket step over the expected one
	fundingBand       = 0.02     // a reference's special funding against the dividend
	navBandBps        = 5        // the pair's total return with the dividends added back
	splitBand         = 0.1      // the split's log ratio against -ln r
)

// newReadings picks the readings that go on record, given the rows on record
// and today's New York date. A source's reading of an action is recorded
// when that source has no row of it yet, or when its newest row says
// something else (status or value); a date-only reading (Scheduled) only when
// the action has no row at all; a None a source derives (an issuer's list
// without the round) only while no source has given the action an amount.
// Once an action's ex-date has passed, a source other than the configuration
// keeps its first amount: the issuers restate past amounts for later splits
// (ProShares' TQQQ of 2025-09-24, 0.048906 against 0.097812 as declared). Such
// a reading is returned as kept. An amount after a source's None or date is
// recorded at any time.
func newReadings(stored, readings []Action, today string) (record, kept []Action) {
	newest := map[string]Action{} // action and source -> its newest row
	exists := map[string]bool{}   // action -> any row
	known := map[string]bool{}    // action -> a row with an amount
	note := func(a Action) {
		if was, ok := newest[a.key()+" "+a.Source]; !ok || a.ReadMs >= was.ReadMs {
			newest[a.key()+" "+a.Source] = a
		}
		exists[a.key()] = true
		known[a.key()] = known[a.key()] || a.Status == "Known"
	}
	for _, a := range stored {
		note(a)
	}
	// Amounts first, then Nones, then dates: a None or a date of the same pass then finds the amount on record.
	rank := map[string]int{"Known": 0, "None": 1, "Scheduled": 2}
	readings = slices.Clone(readings)
	slices.SortStableFunc(readings, func(a, b Action) int { return rank[a.Status] - rank[b.Status] })
	for _, r := range readings {
		was, had := newest[r.key()+" "+r.Source]
		switch {
		case r.Status == "Scheduled" && exists[r.key()]:
			continue
		case r.Status == "None" && r.Source != "config" && known[r.key()]:
			continue
		case had && was.Status == r.Status && sameValue(was.Value, r.Value):
			continue
		case had && was.Status == "Known" && r.Source != "config" && r.ExDate < today:
			kept = append(kept, r)
			continue
		}
		record = append(record, r)
		note(r)
	}
	return record, kept
}

func sameValue(a, b string) bool {
	if a == "" || b == "" {
		return a == b
	}
	x, errX := decimal.NewFromString(a)
	y, errY := decimal.NewFromString(b)
	return errX == nil && errY == nil && x.Equal(y)
}

// noneReadings derives an issuer's statement of no distribution: an expected
// round that is over (over) while the issuer's list, read in full, has
// nothing for its ex-date. expected carries the source and the request the
// statement is recorded with.
func noneReadings(expected, declared []Action, now time.Time, over func(exDate string) bool) []Action {
	var out []Action
	for _, e := range expected {
		if !over(e.ExDate) || slices.ContainsFunc(declared, func(d Action) bool { return d.key() == e.key() }) {
			continue
		}
		e.Status, e.Value, e.ReadMs = "None", "", now.UnixMilli()
		out = append(out, e)
	}
	return out
}

// readActions reads every action source but Direxion once and puts what is
// new on record: the configuration's entries; for the legs of combos on
// ProShares NAVs the issuer's schedule page and its distributions of each
// year from history_start's (proSharesYears: a date of the schedule whose
// ex-date has closed without a distribution listed in the tables that may
// list it is None); Nasdaq's dividends of every ticker but Direxion's funds
// (NYSE Arca: Nasdaq has none of them); Bitget's split records of every
// ticker it lists.
func (m *Manager) readActions(ctx context.Context) {
	var readings []Action
	for _, e := range m.cfg.CorporateActions {
		a, _ := e.action() // Validate has checked it
		a.ReadMs = time.Now().UnixMilli()
		readings = append(readings, a)
	}
	now := time.Now()
	var proshares []string
	tickers, bitget := []string{}, map[string]string{}
	for _, combo := range m.cfg.Combos {
		if combo.NAVSource == "proshares" {
			proshares = append(proshares, combo.BullTicker, combo.BearTicker)
		}
		for _, t := range []string{combo.BullTicker, combo.BearTicker, combo.RefTicker} {
			if t != "" && !slices.Contains(tickers, t) && !slices.Contains(m.direxionTickers(), t) {
				tickers = append(tickers, t)
			}
		}
		for _, mk := range combo.Markets {
			if mk.Venue == "bitget" {
				for ticker, symbol := range map[string]string{combo.BullTicker: mk.BullSymbol, combo.BearTicker: mk.BearSymbol, combo.RefTicker: mk.RefSymbol} {
					if symbol != "" {
						bitget[symbol] = ticker
					}
				}
			}
		}
	}
	if len(proshares) > 0 {
		time.Sleep(sourcePause) // the same host as the product pages the NAV read of this tick has just read
		dates, err := ProShares{}.ScheduleDates(ctx)
		m.note("actions proshares schedule", err)
		last := now.In(newYork).Year()
		for _, date := range dates {
			last = max(last, slices.Max(proSharesYears(date)))
		}
		var schedule, expected, declared []Action
		for _, ticker := range proshares {
			for _, date := range dates {
				round := Action{Ticker: ticker, ExDate: date, Kind: "dividend", Status: "Scheduled", Source: "proshares-schedule", URL: proSharesScheduleURL,
					ReadMs: now.UnixMilli()}
				schedule = append(schedule, round)
				round.Source, round.URL = "proshares-api", proSharesDistributionsURL(ticker, proSharesYears(date)[0])
				expected = append(expected, round)
			}
			for year := m.cfg.HistoryStart.Year(); year <= last; year++ {
				time.Sleep(sourcePause)
				rows, err := ProShares{}.Distributions(ctx, ticker, year)
				m.note(fmt.Sprintf("actions proshares %s %d", ticker, year), err)
				if err != nil {
					// Without a table that may list a round, the round cannot be said to have no distribution.
					expected = slices.DeleteFunc(expected, func(e Action) bool { return e.Ticker == ticker && slices.Contains(proSharesYears(e.ExDate), year) })
				}
				declared = append(declared, rows...)
			}
		}
		readings = append(readings, declared...)
		readings = append(readings, schedule...)
		// A round is over once its ex-date's regular close has passed: ProShares lists it on its declaration day.
		readings = append(readings, noneReadings(expected, declared, now, func(exDate string) bool {
			closeAt, err := m.cal.closeOf(exDate)
			return err == nil && !now.Before(closeAt)
		})...)
	}
	for i, ticker := range tickers {
		if i > 0 {
			time.Sleep(sourcePause)
		}
		rows, err := NasdaqDividends(ctx, ticker)
		m.note("actions nasdaq "+ticker, err)
		readings = append(readings, rows...)
	}
	if len(bitget) > 0 {
		rows, err := Bitget{}.SplitRecords(ctx, bitget)
		m.note("actions bitget split-records", err)
		readings = append(readings, rows...)
	}
	if m.recordActions(ctx, readings, now) {
		m.actionsRead = now
	}
}

// proSharesYears are the years whose ProShares distribution tables may list
// a round of the ex-date: its own, and for a December ex-date the next one
// too (a round paid in January, such as the potential excise-tax round of
// 12-31, may be listed under the year it is paid in).
func proSharesYears(exDate string) []int {
	year, _ := strconv.Atoi(exDate[:4])
	if exDate[5:7] == "12" {
		return []int{year, year + 1}
	}
	return []int{year}
}

// readDirexionActions asks Direxion's site API for the distributions of the
// given funds, one request each, logs the dates each answer is as of, and
// puts what is new on record. An expected round of the configuration that a
// fund's list lacks while Direxion has declared that round for another of its
// funds is None for that fund (SOXL often pays nothing in a round of SOXS's);
// a round the fund lists without its amounts yet is not lacking: it stays
// Scheduled and is read again.
// daily says that this is the daily read, which is due again a day later;
// the reads of a declaration window leave that time alone. Without the key
// nothing is read.
func (m *Manager) readDirexionActions(ctx context.Context, tickers []string, daily bool) {
	d, ok := m.navs["direxion"].(Direxion)
	if !ok {
		return
	}
	stored, err := m.store.actions(ctx)
	if err != nil {
		m.noteDB("scan: actions lookup failed", err)
		return
	}
	now := time.Now()
	declared := map[string][]Action{} // fund -> its list, read in full in this pass
	unfilled := map[string][]string{} // fund -> the ex-dates its list holds without any amount yet
	for _, ticker := range tickers {
		rows, blank, timeStamp, tradeDate, err := d.Distributions(ctx, ticker)
		if m.keyRejected("direxion", err) {
			return
		}
		m.note("actions direxion "+ticker, err)
		if err == nil {
			declared[ticker], unfilled[ticker] = rows, blank
			m.log.Info("scan: Direxion distributions read", "ticker", ticker, "time_stamp", timeStamp, "trade_date", tradeDate, "rounds", len(rows))
		}
	}
	// The rounds Direxion has declared: an amount of one of its funds, on record or read now.
	round := map[string]bool{}
	for _, a := range slices.Concat(stored, slices.Concat(slices.Collect(maps.Values(declared))...)) {
		if a.Source == "direxion-api" && a.Status == "Known" {
			round[a.ExDate] = true
		}
	}
	var readings []Action
	for _, ticker := range tickers {
		rows, read := declared[ticker]
		if !read {
			continue
		}
		var expected []Action
		for _, e := range m.cfg.CorporateActions {
			if a, _ := e.action(); a.Ticker == ticker && a.Kind == "dividend" && a.Status == "Scheduled" && !slices.Contains(unfilled[ticker], a.ExDate) {
				a.Source, a.URL = "direxion-api", d.distributionsURL(ticker)
				expected = append(expected, a)
			}
		}
		readings = append(readings, rows...)
		readings = append(readings, noneReadings(expected, rows, now, func(exDate string) bool { return round[exDate] })...)
	}
	if m.recordActions(ctx, readings, now) && daily {
		m.direxionRead = now
	}
}

// recordActions puts the new readings on record (newReadings) and logs each;
// only actions with an ex-date after the first session's day are kept:
// earlier ones belong to no session. It reports whether the database answered.
func (m *Manager) recordActions(ctx context.Context, readings []Action, now time.Time) bool {
	first := m.cfg.historyDay()
	readings = slices.DeleteFunc(readings, func(a Action) bool { return a.ExDate <= first })
	stored, err := m.store.actions(ctx)
	if err != nil {
		m.noteDB("scan: actions lookup failed", err)
		return false
	}
	record, kept := newReadings(stored, readings, now.In(newYork).Format("2006-01-02"))
	for _, k := range kept {
		if m.once("kept " + k.key() + " " + k.Source + " " + k.Status + " " + k.Value) {
			m.log.Warn("scan: a source now says otherwise of a past action: its first reading stays", "action", k.key(), "source", k.Source,
				"status", k.Status, "value", k.Value)
		}
	}
	if err := m.store.insertActions(ctx, record); err != nil {
		m.noteDB("scan: actions not stored", err)
		return false
	}
	for _, a := range record {
		m.log.Info("scan: corporate action on record", "ticker", a.Ticker, "ex_date", a.ExDate, "kind", a.Kind, "status", a.Status, "value", a.Value,
			"source", a.Source)
	}
	return true
}

// direxionTickers are the legs of the combos on Direxion NAVs: their
// dividends come from Direxion's site API.
func (m *Manager) direxionTickers() []string {
	var out []string
	for _, combo := range m.cfg.Combos {
		if combo.NAVSource == "direxion" {
			out = append(out, combo.BullTicker, combo.BearTicker)
		}
	}
	return out
}

// actionsDue reports whether the sources but Direxion are read at this
// tick: a day after the last read, or at every tick in the carrier window
// of an action in use that is Scheduled, but a dividend of a Direxion fund:
// the configuration gives SOXX's and QQQ's dates, which Nasdaq lists only
// once declared, ProShares' schedule TQQQ's and SQQQ's.
func (m *Manager) actionsDue(ctx context.Context, now time.Time) bool {
	if now.Sub(m.actionsRead) >= actionsEvery {
		return true
	}
	waiting, err := m.store.scheduledActions(ctx)
	if err != nil {
		m.noteDB("scan: actions lookup failed", err)
		return false
	}
	for _, a := range waiting {
		if a.Kind == "dividend" && slices.Contains(m.direxionTickers(), a.Ticker) {
			continue
		}
		if from, to, err := m.cal.carrierWindow(a.ExDate); err == nil && !now.Before(from) && now.Before(to) {
			return true
		}
	}
	return false
}

// direxionDue lists the Direxion funds whose distributions are read at this
// tick, as the owner authorised: both once a day (daily), and at every tick a
// fund with a Scheduled dividend in use inside its declaration window, from
// the carrier day's close until the amount is on record (at the latest to the
// end of the trading day after the ex-date).
func (m *Manager) direxionDue(ctx context.Context, now time.Time) (due []string, daily bool) {
	if now.Sub(m.direxionRead) >= actionsEvery {
		return m.direxionTickers(), true
	}
	waiting, err := m.store.scheduledActions(ctx)
	if err != nil {
		m.noteDB("scan: actions lookup failed", err)
		return nil, false
	}
	for _, a := range waiting {
		if a.Kind != "dividend" || !slices.Contains(m.direxionTickers(), a.Ticker) || slices.Contains(due, a.Ticker) {
			continue
		}
		if from, to, err := m.cal.declarationWindow(a.ExDate); err == nil && !now.Before(from) && now.Before(to) {
			due = append(due, a.Ticker)
		}
	}
	return due, false
}

// actionCheck is one action in effect on a venue, as combo_actions gives it.
type actionCheck struct {
	Combo                                     Combo
	Venue, Leg, Ticker, Session, ExDate, Kind string
	Value                                     string
	EffectiveMs                               int64
	HaltMs                                    *int64
	Anchor                                    *string // the ETF's anchor at the carrier session's start, USD
	Expected                                  *string // bps for a dividend, the log ratio for a split
}

// checkActions checks every Known action whose instant has passed: once per
// action and observation, Applied until the data for it exist, then Verified
// or Mismatch, which stays. Information only: an anchor never depends on it.
//   - dividend, at Binance: a leg, the first-minute step at T of the basket
//     of the two legs' Binance indices (bull + bear: their 3x market moves
//     cancel), against the basket's expected step, from 50% to 150%
//     (September 2026: 73% and 85%); a reference, Binance's special funding
//     at T, rate x mark over the anchor, within 2% of the expected step
//   - dividend, at the issuer (legs): the pair's total return from the
//     carrier day's NAV to the ex-date's with the dividends added back,
//     within 5 bps
//   - split, at its venue: the log ratio of the first close after the halt
//     to the last before it, net of the other leg's move (bull and bear: -1
//     x the other leg's; reference: 1/3 x the bull's), within 0.1 of -ln r
func (m *Manager) checkActions(ctx context.Context) {
	now := time.Now()
	done, err := m.store.finalChecks(ctx)
	if err != nil {
		m.noteDB("scan: checks lookup failed", err)
		return
	}
	type job struct {
		a  actionCheck
		at string
	}
	var jobs []job
	seen := map[string]bool{}
	for _, combo := range m.cfg.Combos {
		for _, market := range combo.Markets {
			due, err := m.store.actionsInEffect(ctx, combo, market.Venue, now.Add(-checkDelay).UnixMilli())
			if err != nil {
				m.noteDB("scan: actions lookup failed", err)
				return
			}
			for _, a := range due {
				venues := []string{"binance"} // a dividend's instant is the same on both venues: one check of Binance's data
				switch {
				case a.Kind == "split":
					venues = []string{a.Venue}
				case a.Leg != "ref" && combo.NAVSource != "":
					venues = append(venues, "issuer")
				}
				for _, at := range venues {
					key := a.Ticker + " " + a.ExDate + " " + a.Kind + " " + at
					if !done[key] && !m.told["check "+key] && !seen[key] {
						seen[key] = true
						jobs = append(jobs, job{a, at})
					}
				}
			}
		}
	}
	for _, j := range jobs {
		status, observed, final := m.check(ctx, j.a, j.at)
		if ctx.Err() != nil {
			return
		}
		if err := m.store.upsertCheck(ctx, j.a, j.at, status, observed, now.UnixMilli()); err != nil {
			m.noteDB("scan: check not stored", err)
			continue
		}
		if status != "Applied" || final {
			m.told["check "+j.a.Ticker+" "+j.a.ExDate+" "+j.a.Kind+" "+j.at] = true
			m.log.Info("scan: corporate action checked", "action", j.a.Ticker+" "+j.a.ExDate+" "+j.a.Kind, "at", j.at, "status", status,
				"observed", observed, "expected", deref(j.a.Expected))
		}
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// check makes one observation: its status, the observed figure ("" for
// none) and, for Applied, whether it can never be made (final).
func (m *Manager) check(ctx context.Context, a actionCheck, at string) (status, observed string, final bool) {
	switch {
	case a.Kind == "split":
		return m.checkSplit(ctx, a)
	case at == "issuer":
		return m.checkNAV(ctx, a)
	case a.Leg == "ref":
		return m.checkFunding(ctx, a)
	default:
		return m.checkStep(ctx, a)
	}
}

// within returns Verified when lo <= x <= hi, else Mismatch.
func within(x, lo, hi float64) string {
	if x >= lo && x <= hi {
		return "Verified"
	}
	return "Mismatch"
}

func parse(s string) float64 {
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

// indexStep is the step in bps of an index from the minute before at to the
// minute at: ok false when either kline is missing.
func (m *Manager) indexStep(ctx context.Context, venue, symbol string, at int64) (float64, bool, error) {
	var closes [2]float64
	for i, minute := range []int64{at - minuteMs, at} {
		time.Sleep(sourcePause)
		k, err := m.index[venue].IndexKline(ctx, symbol, minute)
		if err != nil {
			return 0, false, err
		}
		if k == nil || k.MinuteMs != minute {
			return 0, false, nil
		}
		closes[i] = parse(k.Close)
	}
	return 10000 * (closes[1]/closes[0] - 1), true, nil
}

func (m *Manager) checkStep(ctx context.Context, a actionCheck) (string, string, bool) {
	symbols := m.cfg.bellSymbols()
	var basket float64
	for _, leg := range []string{a.Combo.BullTicker, a.Combo.BearTicker} {
		step, ok, err := m.indexStep(ctx, "binance", symbols[leg]["binance"], a.EffectiveMs)
		m.note("check index binance "+leg, err)
		if err != nil || !ok || a.Expected == nil {
			return "Applied", "", err == nil // no Binance index at the instant: not listed then
		}
		basket += step
	}
	return within(basket/parse(*a.Expected), stepLow, stepHigh), strconv.FormatFloat(basket, 'f', 2, 64), true
}

func (m *Manager) checkFunding(ctx context.Context, a actionCheck) (string, string, bool) {
	symbol := m.cfg.bellSymbols()[a.Ticker]["binance"]
	if _, ok, err := m.indexStep(ctx, "binance", symbol, a.EffectiveMs); err != nil || !ok || a.Expected == nil {
		m.note("check index binance "+a.Ticker, err)
		return "Applied", "", err == nil // Binance did not list the contract then
	}
	time.Sleep(sourcePause)
	rate, mark, ok, err := Binance{}.SpecialFunding(ctx, symbol, a.EffectiveMs, a.EffectiveMs+minuteMs)
	m.note("check funding "+a.Ticker, err)
	if err != nil {
		return "Applied", "", false
	}
	if !ok {
		return "Mismatch", "", true
	}
	observed := 10000 * parse(rate) * parse(mark) / parse(*a.Anchor)
	return within(observed/parse(*a.Expected), 1-fundingBand, 1+fundingBand), strconv.FormatFloat(observed, 'f', 2, 64), true
}

func (m *Manager) checkNAV(ctx context.Context, a actionCheck) (string, string, bool) {
	tr, ok, err := m.store.pairReturn(ctx, a.Combo, a.Session, a.ExDate)
	if err != nil {
		m.noteDB("scan: NAV check lookup failed", err)
	}
	if err != nil || !ok {
		return "Applied", "", false // the ex-date's NAVs come the next day
	}
	return within(tr, -navBandBps, navBandBps), strconv.FormatFloat(tr, 'f', 2, 64), true
}

func (m *Manager) checkSplit(ctx context.Context, a actionCheck) (string, string, bool) {
	other := map[string]string{"bull": a.Combo.BearTicker, "bear": a.Combo.BullTicker, "ref": a.Combo.BullTicker}[a.Leg]
	ratio := map[string]float64{"bull": -1, "bear": -1, "ref": 1.0 / 3}[a.Leg] // the leg's leverage over the other's
	if a.HaltMs == nil {
		return "Applied", "", false
	}
	before := *a.HaltMs - minuteMs
	own, okOwn, err := m.store.logStep(ctx, a.Venue, m.symbol(a.Combo, a.Venue, a.Ticker), before, a.EffectiveMs)
	if err == nil && okOwn {
		var move float64
		var okOther bool
		move, okOther, err = m.store.logStep(ctx, a.Venue, m.symbol(a.Combo, a.Venue, other), before, a.EffectiveMs)
		if err == nil && okOther {
			net := own - ratio*move
			return within(net+math.Log(parse(a.Value)), -splitBand, splitBand), strconv.FormatFloat(net, 'f', 4, 64), true
		}
	}
	if err != nil {
		m.noteDB("scan: split check lookup failed", err)
	}
	return "Applied", "", false
}

// symbol is a ticker's contract on a venue in a combo.
func (m *Manager) symbol(combo Combo, venue, ticker string) string {
	for _, mk := range combo.Markets {
		if mk.Venue == venue {
			return map[string]string{combo.BullTicker: mk.BullSymbol, combo.BearTicker: mk.BearSymbol, combo.RefTicker: mk.RefSymbol}[ticker]
		}
	}
	return ""
}

// noteDB logs a failed database step, unless the shutdown cancelled it.
func (m *Manager) noteDB(what string, err error) {
	if !errors.Is(err, context.Canceled) {
		m.log.Warn(what, "err", err)
	}
}

// checkCalendar compares the calendar with Binance's schedule of the US
// equity market and stores the outcome: a trading day or a close that
// differs on a date without an override is logged at Error once per
// difference and run, the rest at Info once. A schedule that cannot be read
// or compared is stored as not compared, logged once, and tried again in an
// hour. Nothing switches: the operator adds an override, or leaves it when
// Binance is wrong.
func (m *Manager) checkCalendar(ctx context.Context) {
	now := time.Now()
	sessions, err := Binance{}.TradingSchedule(ctx)
	var from, to int64
	var differ, info []string
	if err == nil {
		from, to, differ, info, err = m.cal.compareSchedule(sessions)
	}
	m.note("binance tradingSchedule", err)
	if err != nil {
		if ctx.Err() == nil {
			m.storeCalendarCheck(ctx, now, nil, nil, false, "not compared: "+err.Error())
		}
		return
	}
	for _, d := range differ {
		if m.once("calendar differ " + d) {
			m.log.Error("scan: Binance's schedule differs from the calendar: check NYSE / Nasdaq and add a calendar override, or leave it", "difference", d)
		}
	}
	for _, d := range info {
		if m.once("calendar info " + d) {
			m.log.Info("scan: Binance's schedule differs from the calendar (extended hours, or an overridden date)", "difference", d)
		}
	}
	detail := "OK"
	if len(differ) > 0 {
		detail = strings.Join(differ, "; ")
	}
	through := time.UnixMilli(to - 1).In(newYork).Format("2006-01-02")
	if m.once(fmt.Sprintf("calendar checked %s %d", through, len(differ))) {
		m.log.Info("scan: calendar checked against Binance's schedule", "agree", len(differ) == 0, "through", through, "differences", len(differ),
			"extended_hours_or_overridden", len(info))
	}
	m.storeCalendarCheck(ctx, now, &from, &to, len(differ) == 0, detail)
}

// storeCalendarCheck keeps the outcome of a check, and its time as the last
// check: the next is due an hour later.
func (m *Manager) storeCalendarCheck(ctx context.Context, now time.Time, from, to *int64, agree bool, detail string) {
	if err := m.store.writeCalendarCheck(ctx, now.UnixMilli(), from, to, agree, detail); err != nil {
		m.noteDB("scan: calendar check not stored", err)
		return
	}
	m.calendarChecked = now
}
