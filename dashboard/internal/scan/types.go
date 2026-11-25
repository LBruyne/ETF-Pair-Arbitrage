// Package scan collects what the Combo Spread board draws besides the
// recorder's per-second rows (etfpair.perp_second, dashboard/cmd/booktape):
// each perpetual contract's one-minute candle closes on each venue, USD per
// USDT per minute, each ETF's bell anchor (USD per USDT times
// Binance's index price of its perpetual at the regular close, Bitget's
// before Binance lists the contract), the issuers' official NAVs, the ETFs'
// dividends and splits and their checks, and the sessions and phases of the
// US equity calendar with its check against Binance's schedule; and it keeps
// the per-minute summary of the recorder's rows that the board's Data
// Coverage and minute bars read up to date
// (etfpair.refresh_perp_second_minute). It only reads public data and only
// writes the board's own tables.
package scan

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/shopspring/decimal"
)

// --- configuration ---

// Config is dashboard/config.yaml.
type Config struct {
	DSNEnv         string        `yaml:"dsn_env"`          // environment variable holding the PostgreSQL DSN
	Combos         []Combo       `yaml:"combos"`           // the baskets the board offers
	FeeLevels      []FeeLevel    `yaml:"fee_levels"`       // the fee levels the board offers per venue
	HistoryStart   time.Time     `yaml:"history_start"`    // the first session, and the day the one-minute candle history goes back to
	FXPoll         time.Duration `yaml:"fx_poll"`          // how often USD per USDT is sampled
	AnchorPoll     time.Duration `yaml:"anchor_poll"`      // how often a missing bell anchor or a NAV not yet confirmed is asked for again
	NAVSettleDelay time.Duration `yaml:"nav_settle_delay"` // how long the issuer must have given the same NAV of both legs before it replaces the bell anchor
	// CorporateActions are dividends and splits entered by hand: the expected
	// dates of the coming dividends that their sources list only once
	// declared (SOXL's and SOXS's at Direxion, SOXX's and QQQ's at Nasdaq) or
	// not yet (TQQQ's and SQQQ's of a year ProShares' schedule does not list
	// yet), and a figure or a None the operator sets.
	CorporateActions []ActionEntry `yaml:"corporate_actions"`
	// CalendarOverrides close a trading day or move its close: an unscheduled
	// closure or early close the operator has learnt of.
	CalendarOverrides []CalendarOverride `yaml:"calendar_overrides"`
}

// ActionEntry is a corporate action entered in the configuration.
type ActionEntry struct {
	Ticker string `yaml:"ticker"`
	ExDate string `yaml:"ex_date"` // New York, 2006-01-02
	Kind   string `yaml:"kind"`    // dividend or split
	// Value is the dividend in USD per share as declared (per pre-split
	// share), or the split's new shares per old share; without it the
	// action is Scheduled: its date is known, its amount not yet.
	Value  string `yaml:"value"`
	Status string `yaml:"status"` // None: no distribution; otherwise left out (Known with a value, Scheduled without)
	Source string `yaml:"source"` // where the figure or the date comes from
}

// CalendarOverride closes a New York trading day (Close "closed") or gives
// its regular close another time (Close "HH:MM", New York).
type CalendarOverride struct {
	Date   string `yaml:"date"`
	Close  string `yaml:"close"`
	Source string `yaml:"source"`
}

// Combo is a bull and a bear ETF and the contracts that trade them per venue.
type Combo struct {
	ID         string `yaml:"id"`
	Title      string `yaml:"title"`
	BullTicker string `yaml:"bull_ticker"`
	BearTicker string `yaml:"bear_ticker"`
	RefTicker  string `yaml:"ref_ticker"` // the unleveraged ETF of the same index, shown for reference
	// NAVSource names the issuer whose official NAVs become the two legs'
	// anchors once they are stable: "direxion" or "proshares", empty for
	// none. The reference ETF keeps its bell anchor.
	NAVSource string   `yaml:"nav_source"`
	Markets   []Market `yaml:"markets"`
}

// Market is one venue's perpetual contracts of a combo: the two legs and the reference.
type Market struct {
	Venue      string `yaml:"venue"`
	BullSymbol string `yaml:"bull_symbol"`
	BearSymbol string `yaml:"bear_symbol"`
	RefSymbol  string `yaml:"ref_symbol"`
	// BullTick and BearTick are the legs' price ticks on the venue, as its
	// instrument list gives them (the recorder logs them at its start). The
	// board's pricing levels follow from them (etfpair.pricing_levels: 2 at
	// 0.01, 5 at 0.001), and its Book strip shows them.
	BullTick string `yaml:"bull_tick"`
	BearTick string `yaml:"bear_tick"`
}

// symbols lists the market's contracts: the two legs and the reference.
func (m Market) symbols() []string {
	return []string{m.BullSymbol, m.BearSymbol, m.RefSymbol}
}

// FeeLevel is one fee level of a venue. Rates are bps of notional; Source
// names the evidence so the board can show where a rate comes from.
type FeeLevel struct {
	Venue    string  `yaml:"venue"`
	Level    string  `yaml:"level"`
	Label    string  `yaml:"label"`
	TakerBps string  `yaml:"taker_bps"`
	MakerBps *string `yaml:"maker_bps"`
	Source   string  `yaml:"source"`
}

// Validate rejects a configuration the collector could not run with.
func (c Config) Validate() error {
	var errs []error
	if c.DSNEnv == "" {
		errs = append(errs, errors.New("dsn_env is required"))
	}
	if len(c.Combos) == 0 {
		errs = append(errs, errors.New("combos must name at least one combo"))
	}
	for _, combo := range c.Combos {
		if combo.ID == "" || combo.BullTicker == "" || combo.BearTicker == "" || len(combo.Markets) == 0 {
			errs = append(errs, fmt.Errorf("combo %q needs id, bull_ticker, bear_ticker and markets", combo.ID))
		}
		// Binance's index price of each contract gives the bell anchor of its ETF.
		if !slices.ContainsFunc(combo.Markets, func(m Market) bool { return m.Venue == "binance" }) {
			errs = append(errs, fmt.Errorf("combo %q needs a binance market: its index prices give the bell anchors", combo.ID))
		}
		for _, m := range combo.Markets {
			for name, tick := range map[string]string{"bull_tick": m.BullTick, "bear_tick": m.BearTick} {
				if v, err := strconv.ParseFloat(tick, 64); err != nil || !(v > 0) {
					errs = append(errs, fmt.Errorf("combo %q, %s market: %s %q is not a positive price tick", combo.ID, m.Venue, name, tick))
				}
			}
		}
		if !slices.Contains([]string{"", "direxion", "proshares"}, combo.NAVSource) {
			errs = append(errs, fmt.Errorf("combo %q: nav_source %q is not direxion, proshares or empty", combo.ID, combo.NAVSource))
		}
	}
	if c.HistoryStart.IsZero() {
		errs = append(errs, errors.New("history_start is required"))
	}
	for name, d := range map[string]time.Duration{"fx_poll": c.FXPoll, "anchor_poll": c.AnchorPoll, "nav_settle_delay": c.NAVSettleDelay} {
		if d <= 0 {
			errs = append(errs, fmt.Errorf("%s must be positive", name))
		}
	}
	entered := map[string]bool{}
	for _, a := range c.CorporateActions {
		action, err := a.action()
		if err != nil {
			errs = append(errs, err)
		}
		if entered[action.key()] {
			errs = append(errs, fmt.Errorf("corporate action %s is entered twice", action.key()))
		}
		entered[action.key()] = true
	}
	overridden := map[string]bool{}
	for _, o := range c.CalendarOverrides {
		day, dateErr := time.Parse("2006-01-02", o.Date)
		at, closeErr := time.Parse("15:04", o.Close)
		if overridden[o.Date] {
			errs = append(errs, fmt.Errorf("calendar override %s is entered twice", o.Date))
		}
		overridden[o.Date] = true
		switch {
		case dateErr != nil || o.Close != "closed" && closeErr != nil || o.Source == "":
			errs = append(errs, fmt.Errorf("calendar override %+v: needs date (2006-01-02), close (closed or HH:MM) and source", o))
		case o.Close != "closed" && (!scheduledTradingDay(day) || at.Hour()*60+at.Minute() <= 9*60+30 || at.Hour()*60+at.Minute() > 16*60):
			// A moved close keeps the day's phases apart: after 09:30, and at 16:00 at the latest, so that After-Hours,
			// four hours from the close, ends by the next Overnight at 20:00; and only a trading day has a close.
			errs = append(errs, fmt.Errorf("calendar override %+v: HH:MM moves the close of a trading day to after 09:30 and at most 16:00", o))
		}
	}
	return errors.Join(errs...)
}

// action is the entry as a reading of the configuration.
func (a ActionEntry) action() (Action, error) {
	out := Action{Ticker: a.Ticker, ExDate: a.ExDate, Kind: a.Kind, Value: a.Value, Status: a.Status, Source: "config", URL: a.Source}
	if out.Status == "" {
		out.Status = map[bool]string{true: "Known", false: "Scheduled"}[a.Value != ""]
	}
	_, dateErr := time.Parse("2006-01-02", a.ExDate)
	value, valueErr := decimal.NewFromString(a.Value)
	switch {
	case a.Ticker == "" || dateErr != nil || !slices.Contains([]string{"dividend", "split"}, a.Kind) || a.Source == "":
		return out, fmt.Errorf("corporate action %+v: needs ticker, ex_date (2006-01-02), kind (dividend or split) and source", a)
	case out.Status == "Known" && (valueErr != nil || !value.IsPositive()):
		return out, fmt.Errorf("corporate action %+v: value must be a positive number", a)
	case out.Status != "Known" && (a.Value != "" || !slices.Contains([]string{"Scheduled", "None"}, out.Status)):
		return out, fmt.Errorf("corporate action %+v: status is None, or left out (Known with a value, Scheduled without)", a)
	}
	return out, nil
}

// historyDay is the first session: the New York trading day of history_start.
func (c Config) historyDay() string {
	return c.HistoryStart.Format("2006-01-02")
}

// bellSymbols maps each ETF of the combos (bull, bear and reference) to its
// perpetual by venue, whose index price gives the ETF's bell anchor:
// Binance's, or Bitget's for a session before Binance lists the contract.
func (c Config) bellSymbols() map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, combo := range c.Combos {
		for _, m := range combo.Markets {
			for ticker, symbol := range map[string]string{combo.BullTicker: m.BullSymbol, combo.BearTicker: m.BearSymbol, combo.RefTicker: m.RefSymbol} {
				if ticker == "" || symbol == "" {
					continue // a combo without a reference
				}
				if out[ticker] == nil {
					out[ticker] = map[string]string{}
				}
				out[ticker][m.Venue] = symbol
			}
		}
	}
	return out
}

// --- values ---

// Prices stay decimal strings from the wire to the database's NUMERIC columns.

// Quote is a best bid and ask as the source reported them.
type Quote struct {
	Bid, Ask string
}

// Candle is a one-minute candle: its start and its close.
type Candle struct {
	MinuteMs int64
	Close    string
}

// NAV is an issuer's official net asset value per share of a ticker on a
// trading day: once it is stable it anchors the session that starts at that
// day's close.
type NAV struct {
	Issuer      string
	Ticker      string
	SessionDate string // New York trading day, 2006-01-02
	Price       string // rounded to the cent, the precision the issuers' pages show
	Via         string // how it was read: page, api, history-csv or wayback
	ReadMs      int64  // when the response that gave it arrived
}

// navObs is one distinct NAV figure on record for an issuer, ticker and day:
// when it was first and last read, and how it was first read.
type navObs struct {
	Price              string
	ReceivedMs, SeenMs int64
	Via                string
}

// session is a trading day and its regular close.
type session struct {
	Date    string
	CloseMs int64
}

// Action is one reading of a corporate action of an ETF: a source's figure,
// or its statement that there is none or that only the date is known.
type Action struct {
	Ticker string
	ExDate string // New York, 2006-01-02
	Kind   string // dividend or split
	Value  string // decimal, Known only: USD per share as declared, or new shares per old share
	Status string // Scheduled (date known, amount not yet), Known, or None (no distribution)
	Source string // config, proshares-schedule, proshares-api, nasdaq, direxion-api or bitget-split-records
	URL    string // the request, or the configuration entry's source
	ReadMs int64  // when the reading arrived; on record, when it became known
}

// key names the action a reading is of.
func (a Action) key() string {
	return a.Ticker + " " + a.ExDate + " " + a.Kind
}

// --- sources ---

// PerpSource reads one venue's public contract prices.
type PerpSource interface {
	// Candles returns the one-minute candles starting in [startMs, endMs), oldest first.
	Candles(ctx context.Context, symbol string, startMs, endMs int64) ([]Candle, error)
}

// IndexSource reads the index price of a perpetual contract on one venue.
type IndexSource interface {
	// IndexKline returns the first one-minute index kline of the contract
	// that opens at or after minuteMs (on Bitget, within the 100 minutes
	// from it), nil when there is none.
	IndexKline(ctx context.Context, symbol string, minuteMs int64) (*Candle, error)
}

// NAVSource reads one issuer's official NAVs.
type NAVSource interface {
	// Latest reads, one request per ticker, the NAV of each ticker the
	// issuer shows for the session (the newest closed one), or the newest it
	// shows and the trading day that is for; a ticker without one is left out.
	Latest(ctx context.Context, tickers []string, session string) ([]NAV, error)
	// Past returns the issuer's NAVs of a ticker on the given trading days
	// that it still serves.
	Past(ctx context.Context, ticker string, sessionDates []string) ([]NAV, error)
}

// errKeyRejected is a data API refusing the key a request carried.
var errKeyRejected = errors.New("the API key was rejected")

const (
	minuteMs = int64(60_000)
	// historyPause paces the backfill: about six candle pages a second, under
	// Binance's request weight a minute and Bitget's requests a second.
	historyPause = 150 * time.Millisecond
	// candlePage is how many minutes one candle request covers; every
	// source here serves at least this many per request except Coinbase.
	candlePage = 1000
	// candleRecheck is how long after its first read, a second after the
	// minute, the newest candle is read again: the exchanges still revise it
	// for a few seconds.
	candleRecheck = 5 * time.Second
	// bellPause spaces the index requests of the bell backfill: one a second.
	bellPause = time.Second
	// bellDelay is how long after a close its bell anchor is read: the index
	// kline that ends at the close and USD per USDT of its minute are final then.
	bellDelay = time.Minute
	// bellBackfillKnown is when a bell anchor read after the collector
	// started, for a session that closed before, counts as known: the close
	// plus this, about when a running collector stores it.
	bellBackfillKnown = 2 * time.Minute
	// direxionKeyEnv names the environment variable holding the key of the
	// data API of Direxion's website. The key goes in requests only: it is
	// never logged, stored or written to the configuration.
	direxionKeyEnv = "DIREXION_SITE_API_KEY"
	// coverageWindow is how far back each refresh of the per-minute summary
	// (etfpair.perp_second_minute) reaches: the recorder writes the rows of a
	// failed write up to an hour late. The board reads a minute from the
	// summary only from 66 minutes after it began, past its last refresh
	// (schema.sql, range_points_sql and perp_coverage): the two move
	// together. coverageLag is how far before now it ends, coverageAt when in
	// each minute it runs.
	coverageWindow = 65 * time.Minute
	coverageLag    = 10 * time.Second
	coverageAt     = 20 * time.Second
)
