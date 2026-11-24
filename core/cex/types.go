// Package cex is the fact layer of the trading process: the two legs' order
// books, the contract rules, the account, orders and fills on the exchange,
// and the reference data behind the spread (the USDT-USD rate F, the US
// equity calendar, the anchors and the planned dividends and splits). It
// computes no spread and decides no order.
package cex

import (
	"errors"
	"fmt"
	"time"
)

// --- configuration ---

// Config is the `cex:` section of config.yaml.
type Config struct {
	Pair              Pair               `yaml:"pair"`               // the two legs of the one pair this process trades
	DepthMaxAge       time.Duration      `yaml:"depth_max_age"`      // a leg's depth, or its best price, older than this is not usable
	FXMaxAge          time.Duration      `yaml:"fx_max_age"`         // F older than this stops entries; exits go on with the last value
	NAVSettleDelay    time.Duration      `yaml:"nav_settle_delay"`   // a NAV unchanged for this long replaces the bell anchor
	CalendarOverrides []CalendarOverride `yaml:"calendar_overrides"` // closures and early closes the built-in calendar does not know
	CorporateActions  []CorporateAction  `yaml:"corporate_actions"`  // planned ex-dates, kept by hand
}

// DefaultConfig holds the values a key left out of config.yaml keeps. The
// pair has no default: it must be written.
var DefaultConfig = Config{DepthMaxAge: 10 * time.Second, FXMaxAge: 60 * time.Second, NAVSettleDelay: time.Hour}

// Pair names the two legs: bull is the leveraged long ETF's contract, bear
// the leveraged short ETF's.
type Pair struct {
	Bull LegConfig `yaml:"bull"`
	Bear LegConfig `yaml:"bear"`
}

// LegConfig is one leg: its contract on the exchange, the ETF ticker the
// anchors are read for, and the depth topic subscribed for it. The two legs
// use the topics that cover the same price range, since their ticks differ.
type LegConfig struct {
	Symbol    Symbol `yaml:"symbol"`
	Ticker    string `yaml:"ticker"`
	BookTopic string `yaml:"book_topic"`
}

// The depth topics a leg may subscribe.
const (
	TopicBooks5  = "books5"
	TopicBooks50 = "books50"
)

// CalendarOverride changes one New York trading day by hand. Close is
// CloseClosed for a day without a regular session, or the early close as
// "HH:MM" New York time.
type CalendarOverride struct {
	Date   string `yaml:"date"` // 2006-01-02, New York
	Close  string `yaml:"close"`
	Source string `yaml:"source"` // where the change was published
}

// CloseClosed is the CalendarOverride.Close of a day with no regular session.
const CloseClosed = "closed"

// CorporateAction is a planned dividend or split of one leg's ETF. Entries
// are banned around its ex-date and the anchor is adjusted from the moment it
// takes effect.
type CorporateAction struct {
	Ticker string `yaml:"ticker"`
	ExDate string `yaml:"ex_date"` // 2006-01-02, New York
	Kind   string `yaml:"kind"`    // ActionDividend or ActionSplit
	Source string `yaml:"source"`  // where the date was published
}

// The kinds of CorporateAction.
const (
	ActionDividend = "dividend"
	ActionSplit    = "split"
)

// dateLayout is how config.yaml writes a New York calendar day.
const dateLayout = "2006-01-02"

// Validate rejects a configuration the manager could not run with. Every
// problem is reported, each starting with its yaml path.
func (c Config) Validate() error {
	var errs []error
	errs = append(errs, c.Pair.Bull.validate("cex.pair.bull"), c.Pair.Bear.validate("cex.pair.bear"))
	if c.Pair.Bull.Symbol != "" && c.Pair.Bull.Symbol == c.Pair.Bear.Symbol {
		errs = append(errs, fmt.Errorf("cex.pair.bear.symbol must differ from cex.pair.bull.symbol, both are %s", c.Pair.Bull.Symbol))
	}
	if c.DepthMaxAge <= 0 {
		errs = append(errs, fmt.Errorf("cex.depth_max_age must be positive, got %s", c.DepthMaxAge))
	}
	if c.FXMaxAge <= 0 {
		errs = append(errs, fmt.Errorf("cex.fx_max_age must be positive, got %s", c.FXMaxAge))
	}
	if c.NAVSettleDelay <= 0 {
		errs = append(errs, fmt.Errorf("cex.nav_settle_delay must be positive, got %s", c.NAVSettleDelay))
	}
	for i, o := range c.CalendarOverrides {
		path := fmt.Sprintf("cex.calendar_overrides[%d]", i)
		if _, err := time.Parse(dateLayout, o.Date); err != nil {
			errs = append(errs, fmt.Errorf("%s.date must be a day as 2006-01-02, got %q", path, o.Date))
		}
		if _, err := time.Parse("15:04", o.Close); o.Close != CloseClosed && err != nil {
			errs = append(errs, fmt.Errorf("%s.close must be %q or a New York time as HH:MM, got %q", path, CloseClosed, o.Close))
		}
	}
	for i, a := range c.CorporateActions {
		path := fmt.Sprintf("cex.corporate_actions[%d]", i)
		if a.Ticker == "" || (a.Ticker != c.Pair.Bull.Ticker && a.Ticker != c.Pair.Bear.Ticker) {
			errs = append(errs, fmt.Errorf("%s.ticker must be one of the pair's tickers, got %q", path, a.Ticker))
		}
		if _, err := time.Parse(dateLayout, a.ExDate); err != nil {
			errs = append(errs, fmt.Errorf("%s.ex_date must be a day as 2006-01-02, got %q", path, a.ExDate))
		}
		if a.Kind != ActionDividend && a.Kind != ActionSplit {
			errs = append(errs, fmt.Errorf("%s.kind must be %s or %s, got %q", path, ActionDividend, ActionSplit, a.Kind))
		}
	}
	return errors.Join(errs...)
}

// validate checks one leg; path is its yaml path.
func (l LegConfig) validate(path string) error {
	var errs []error
	if l.Symbol == "" {
		errs = append(errs, fmt.Errorf("%s.symbol is required", path))
	}
	if l.Ticker == "" {
		errs = append(errs, fmt.Errorf("%s.ticker is required", path))
	}
	if l.BookTopic != TopicBooks5 && l.BookTopic != TopicBooks50 {
		errs = append(errs, fmt.Errorf("%s.book_topic must be %s or %s, got %q", path, TopicBooks5, TopicBooks50, l.BookTopic))
	}
	return errors.Join(errs...)
}

// --- secrets ---

// Credentials is one exchange API key. It comes from the secrets file and is
// never logged or stored.
type Credentials struct {
	Key        string `json:"key"`
	Secret     string `json:"secret"`
	Passphrase string `json:"passphrase"`
}

// Env is the secrets file (JSON, see .env.example). Nothing in it is logged
// or written to the database. The file's `bitget` section is not decoded
// with the rest: config.Load decodes it in live mode only, so a paper run
// never holds an exchange key.
type Env struct {
	Bitget   Credentials `json:"-"` // set in live mode only
	Direxion struct {
		SiteAPIKey string `json:"site_api_key"` // empty: the issuer's NAV is not read and the bell anchor is used throughout
	} `json:"direxion"`
	Store struct {
		DSN string `json:"dsn"` // the writer role's connection string
	} `json:"store"`
}

// Validate checks the secrets a run needs: the database always, the exchange
// key only in live mode. The messages name the missing fields and never
// carry a value.
func (e Env) Validate(live bool) error {
	var errs []error
	if e.Store.DSN == "" {
		errs = append(errs, errors.New(".env store.dsn is required"))
	}
	if live {
		for _, f := range []struct{ name, value string }{{"key", e.Bitget.Key}, {"secret", e.Bitget.Secret}, {"passphrase", e.Bitget.Passphrase}} {
			if f.value == "" {
				errs = append(errs, fmt.Errorf(".env bitget.%s is required when exec.live is true", f.name))
			}
		}
	}
	return errors.Join(errs...)
}

// --- identifiers ---

// Symbol is a contract on the exchange, such as SOXLUSDT.
type Symbol string
