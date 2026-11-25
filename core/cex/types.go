// Package cex is the fact layer of the trading process: the two legs' order
// books, the contract rules, the account, orders and fills on the exchange,
// and the reference data behind the spread (the USDT-USD rate F, the US
// equity calendar, the anchors and the planned dividends and splits). It
// computes no spread and decides no order.
package cex

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// --- configuration ---

// Config is the `cex:` section of config.yaml.
type Config struct {
	Pair              Pair               `yaml:"pair"`               // the two legs of the one pair this process trades
	BookConnections   int                `yaml:"book_connections"`   // public connections per leg, each carrying the leg's depth topic and books1; the frame of a sequence number that arrives first is used
	OrderTransport    OrderTransport     `yaml:"order_transport"`    // how an order reaches the venue: ws (the default) or rest
	DepthMaxAge       time.Duration      `yaml:"depth_max_age"`      // a leg's depth, or its best price, older than this is not usable
	FXMaxAge          time.Duration      `yaml:"fx_max_age"`         // F older than this stops entries; exits go on with the last value
	NAVSettleDelay    time.Duration      `yaml:"nav_settle_delay"`   // a NAV unchanged for this long replaces the bell anchor
	CalendarOverrides []CalendarOverride `yaml:"calendar_overrides"` // closures and early closes the built-in calendar does not know
	CorporateActions  []CorporateAction  `yaml:"corporate_actions"`  // planned ex-dates, kept by hand
}

// DefaultConfig holds the values a key left out of config.yaml keeps. The
// pair has no default: it must be written.
var DefaultConfig = Config{BookConnections: 3, OrderTransport: OrderTransportWS, DepthMaxAge: 10 * time.Second, FXMaxAge: 60 * time.Second, NAVSettleDelay: time.Hour}

// OrderTransport names the way orders are sent to the venue. The default
// is ws: on real orders it created the order about 9 ms sooner than rest
// (core/SPEC.md 5.7).
type OrderTransport string

const (
	OrderTransportWS   OrderTransport = "ws"   // a frame on a logged-in WebSocket connection kept for orders; an order goes by REST while that connection is not up
	OrderTransportREST OrderTransport = "rest" // one signed HTTP request per order, on a connection kept warm
)

// maxBookConnections bounds book_connections: the venue limits connections
// per address, and every connection's frames are parsed.
const maxBookConnections = 5

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

// The book topics. A leg subscribes one depth topic (TopicBooks5 or
// TopicBooks50) and always TopicBooks1, the stream of its best bid and ask.
const (
	TopicBooks1  = "books1"
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
	if c.BookConnections < 1 || c.BookConnections > maxBookConnections {
		errs = append(errs, fmt.Errorf("cex.book_connections must be 1 to %d, got %d", maxBookConnections, c.BookConnections))
	}
	if c.OrderTransport != OrderTransportREST && c.OrderTransport != OrderTransportWS {
		errs = append(errs, fmt.Errorf("cex.order_transport must be %s (the default) or %s, got %q", OrderTransportWS, OrderTransportREST, c.OrderTransport))
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
	seen := map[CorporateAction]int{} // ticker, ex-date and kind -> the index that has them
	for i, a := range c.CorporateActions {
		path := fmt.Sprintf("cex.corporate_actions[%d]", i)
		// The same action written twice would be applied twice: a dividend
		// subtracted from the anchor two times.
		key := CorporateAction{Ticker: a.Ticker, ExDate: a.ExDate, Kind: a.Kind}
		if first, dup := seen[key]; dup {
			errs = append(errs, fmt.Errorf("%s repeats cex.corporate_actions[%d] (%s %s %s): each action is written once", path, first, a.Ticker, a.ExDate, a.Kind))
		} else {
			seen[key] = i
		}
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

// Leg is one side of the pair. It indexes the [2]T arrays that hold one value
// per leg.
type Leg int

const (
	Bull Leg = iota // the leveraged long ETF's contract
	Bear            // the leveraged short ETF's contract
)

// Legs lists both legs, for loops.
var Legs = [2]Leg{Bull, Bear}

// String is the leg's name in logs and in the database.
func (l Leg) String() string {
	if l == Bull {
		return "bull"
	}
	return "bear"
}

// Side is the direction of one order.
type Side string

const (
	SideBuy  Side = "buy"
	SideSell Side = "sell"
)

// ClientOrderID is the order id the process makes; it is the idempotency key
// of an order.
type ClientOrderID string

// OwnOrder reports whether a client order id carries this program's prefix.
// On an account that other programs trade too, a venue adapter keeps only
// such orders and fills of the private stream.
func OwnOrder(id ClientOrderID) bool { return strings.HasPrefix(string(id), clientOrderPrefix) }

// OrderID is the id the venue assigns to an order it accepted.
type OrderID string

// OrderPurpose is why an order was sent. It is recorded and never sent to
// the venue.
type OrderPurpose string

const (
	PurposeEntry    OrderPurpose = "entry"    // opens a lot
	PurposeComplete OrderPurpose = "complete" // adds to the leg that filled less
	PurposeTrim     OrderPurpose = "trim"     // closes what the other leg did not match
	PurposeExit     OrderPurpose = "exit"     // closes at the exit line
	PurposeForced   OrderPurpose = "forced"   // closes before the regular close, after a stop signal or for an ended Session
)

// OrderStatus is the state of an order as the process knows it.
type OrderStatus string

const (
	StatusPending      OrderStatus = "pending"       // registered and written, not yet answered by the venue
	StatusAcked        OrderStatus = "acked"         // the venue accepted it; the outcome comes from the private stream
	StatusFilled       OrderStatus = "filled"        // final: filled in full
	StatusCancelled    OrderStatus = "cancelled"     // final: the IOC remainder was cancelled, with or without a partial fill
	StatusRejected     OrderStatus = "rejected"      // final: the venue refused it
	StatusNotSent      OrderStatus = "not_sent"      // final: nothing left the process
	StatusNeverArrived OrderStatus = "never_arrived" // final: exec's verdict that the venue never had it
)

// Terminal reports whether the status is final: no later report changes the
// order's state, only its filled quantity and fee may still be completed.
func (s OrderStatus) Terminal() bool {
	switch s {
	case StatusFilled, StatusCancelled, StatusRejected, StatusNotSent, StatusNeverArrived:
		return true
	}
	return false
}

// --- market data ---

// BookLevel is one price level of a book.
type BookLevel struct{ Price, Qty decimal.Decimal }

// Book is one leg's published book. It is immutable: every change publishes
// a new one. Bids and Asks are the depth levels, with the best level's
// quantity taken from books1 when books1 is newer at the same prices.
type Book struct {
	Symbol           Symbol
	Bids, Asks       []BookLevel // best first; empty while the leg has no depth
	DepthSeq         uint64      // the matching sequence number of the depth held; 0 without depth
	DepthReceivedAt  time.Time   // local arrival of the last accepted depth frame; zero without depth
	Conflict         bool        // books1 is newer than the depth and one best price differs: wait for a newer depth
	BestBid, BestAsk BookLevel   // from the newer of the depth and books1, by sequence number
	BestSeq          uint64      // the sequence number of the frame BestBid and BestAsk come from
	BestReceivedAt   time.Time   // local arrival of the frame BestBid and BestAsk come from
	Connected        bool        // one of the leg's connections is up and has delivered a frame
}

// BookAction is the kind of a depth frame.
type BookAction string

const (
	ActionSnapshot BookAction = "snapshot" // the whole book of the topic
	ActionUpdate   BookAction = "update"   // changed levels on top of the frame PrevSeq names
)

// BookSubscription names one topic of one contract.
type BookSubscription struct {
	Symbol Symbol
	Topic  string // TopicBooks1, TopicBooks5 or TopicBooks50
}

// BookEvent is one book frame as a Venue hands it to the manager. A level
// with quantity zero in an update deletes that price.
type BookEvent struct {
	Symbol       Symbol
	Topic        string
	Action       BookAction
	Bids, Asks   []BookLevel // best first
	Seq, PrevSeq uint64      // the matching sequence number of this frame, and of the frame an update follows
	ExchangeTime time.Time   // the venue's time of the book state
	ReceivedAt   time.Time   // local arrival
}

// --- reference ---

// MarketRule is what the venue demands of an order on one contract.
type MarketRule struct {
	Symbol      Symbol
	PriceTick   decimal.Decimal // prices are multiples of it
	QtyStep     decimal.Decimal // quantities are multiples of it
	MinQty      decimal.Decimal // the smallest order quantity, in shares
	MinNotional decimal.Decimal // the smallest order value, in USDT
	Online      bool            // the contract is trading
}

// Sendable reports whether the venue takes an order of qty at price: at
// least the minimum quantity and the minimum value.
func (r MarketRule) Sendable(qty, price decimal.Decimal) bool {
	return qty.GreaterThanOrEqual(r.MinQty) && qty.Mul(price).GreaterThanOrEqual(r.MinNotional)
}

// AccountSettings is how the account is set up for the two contracts. The
// process reads it and never changes it.
type AccountSettings struct {
	OneWay   bool // one-way position mode (not hedge mode)
	BySymbol map[Symbol]SymbolSetting
}

// SymbolSetting is one contract's margin mode and leverage on the account.
type SymbolSetting struct {
	Crossed  bool // cross margin
	Leverage int
}

// Position is the account's position in one contract. The mark price,
// unrealised PnL and liquidation price are recorded for the board only;
// each is not valid when the venue did not report it (the paper venue
// reports none).
type Position struct {
	Symbol        Symbol
	Qty           decimal.Decimal // signed: a short position is negative
	AvgPrice      decimal.Decimal
	UpdatedAt     time.Time           // the venue's time of the position
	MarkPrice     decimal.NullDecimal // USDT
	UnrealisedPnL decimal.NullDecimal // USDT
	LiqPrice      decimal.NullDecimal // USDT
}

// PositionSet is the account's positions in the two legs at one moment. A
// leg without a position has no entry.
type PositionSet struct {
	BySymbol   map[Symbol]Position
	ReceivedAt time.Time // local arrival of the read or push
	Loaded     bool      // false until the first read or push: nothing is known yet, which is not "flat"
}

// AccountAssets is the whole account's equity and margin ratio as the venue
// reported them at one moment. They are recorded for the board only: the
// process takes no decision from them (PRD 6.6). The account may hold other
// programs' positions too, so both are account-wide.
type AccountAssets struct {
	Equity      decimal.Decimal // USDT
	MarginRatio decimal.Decimal // as the venue reports it
	ReceivedAt  time.Time       // local arrival of the read
}

// FinancialRecord is one funding settlement booked on the account.
type FinancialRecord struct {
	ID     string
	Symbol Symbol
	Type   string          // the venue's record type
	Amount decimal.Decimal // USDT; positive is received
	At     time.Time       // the venue's time of the record, a few seconds after the settlement
}

// FundingSettlement is the funding rate one settlement of a contract used.
type FundingSettlement struct {
	Symbol Symbol
	Rate   decimal.Decimal
	At     time.Time // the settlement
}

// --- F, calendar, anchors ---

// FX is F, the value of one USDT in USD: the mid of the source's best bid
// and ask.
type FX struct {
	Price, Bid, Ask decimal.Decimal
	ReceivedAt      time.Time // local arrival of the read it comes from
}

// FXMinute is the source's last trade price of USDT-USD in one minute.
type FXMinute struct {
	Minute time.Time // the start of the minute
	Close  decimal.Decimal
}

// Session runs from one regular close of the US equity market to the next;
// weekends and holidays belong to the Session before them.
type Session struct {
	ID          string    // New York date of the close that starts it, 2006-01-02
	Start       time.Time // that regular close
	RegularOpen time.Time // 09:30 New York of the trading day that ends it: entries stop here
	End         time.Time // the next regular close (13:00 New York on an early-close day)
}

// ScheduleDay is one trading day of a venue's schedule of the US equity
// market, compared with the calendar.
type ScheduleDay struct {
	Date                      string // New York date, 2006-01-02
	RegularOpen, RegularClose time.Time
}

// AnchorKind is which anchor a Session uses at a moment.
type AnchorKind string

const (
	AnchorNone AnchorKind = ""     // no anchor yet: no entries
	AnchorBell AnchorKind = "bell" // F x the index price at the close
	AnchorNAV  AnchorKind = "nav"  // the issuer's net asset value of the day, once stable
)

// Anchors is the pair of anchors of one Session as published. Both legs
// always use the same kind.
//
// SessionID names the Session the values belong to. It follows the calendar
// with a short lag: for a moment after a regular close, at most the length
// of the anchor loop's reads in flight, the published value is still the
// ended Session's. A reader compares SessionID with the ID of the Session it
// is in and treats the anchors of another Session as no anchor.
type Anchors struct {
	SessionID  string
	Kind       AnchorKind
	Bull, Bear decimal.Decimal // USD per share, with a dividend already subtracted from the moment it took effect
	KnownAt    time.Time       // when this pair of values became usable
	EntryBan   bool            // inside a dividend or split window: no entries
	RestartAt  time.Time       // the mean and sigma restart at this time (a dividend took effect); zero otherwise
}

// Distribution is one declared distribution of a fund.
type Distribution struct {
	ExDate   string          // New York date, 2006-01-02
	Amount   decimal.Decimal // USD per share: income, capital gains and return of capital summed
	Declared bool            // false: the issuer lists the ex-date without its amounts yet
}

// The kinds of AnchorFact, as stored in anchor_fact.kind.
const (
	FactBell     = "bell"     // the bell anchor of one leg
	FactNAVSeen  = "nav_seen" // a NAV value read for the first time; its settle delay starts at KnownAt
	FactNAV      = "nav"      // the NAV confirmed as the anchor
	FactDividend = "dividend" // the amount of a dividend that takes effect in the Session
)

// AnchorFact is one thing the process learned about a Session's anchors, as
// stored and as read back at a restart.
type AnchorFact struct {
	SessionID string
	Kind      string // FactBellF, FactBell, FactNAVSeen, FactNAV or FactDividend
	Leg       Leg
	Value     decimal.Decimal // USD per share
	F         decimal.Decimal // the USDT-USD rate inside a bell anchor; zero for the other kinds
	Source    string
	KnownAt   time.Time
}

// --- orders ---

// OrderRequest is one IOC limit order to send.
type OrderRequest struct {
	ClientOrderID ClientOrderID
	Symbol        Symbol
	Leg           Leg
	Side          Side
	Price, Qty    decimal.Decimal
	QuotaPrice    decimal.Decimal // recorded, not sent to the venue: the price the order's notional is reserved at, its limit for a buy and the decision's raw best bid for a sell; zero reserves at the limit
	ReduceOnly    bool
	Purpose       OrderPurpose // recorded, not sent to the venue
	CycleID       string       // recorded, not sent to the venue
	LotID         string       // recorded, not sent to the venue
}

// OrderRecord is one order with everything known about its outcome. It is
// also the shape of a venue's report of an order, by push or by query; what
// a report must hold is said at Venue.
type OrderRecord struct {
	Request      OrderRequest
	OrderID      OrderID
	Status       OrderStatus
	Unknown      bool      // the outcome is not known yet: being queried, never resent
	UnknownSince time.Time // when Unknown was set
	FeeKnown     bool      // Fee is the fee of the whole of CumExecQty; false while the fee of any part of it is not reported. An order without a fill needs none
	NotFound     bool      // unknown for 30 s, the venue says there is no such order, and no fill carries its id

	CumExecQty, CumExecValue, AvgPrice decimal.Decimal
	Fee                                decimal.Decimal // USDT; positive is paid; what has been reported so far
	CancelReason                       string
	ErrorCode, ErrorMessage            string
	ErrorKind                          ErrorKind // why the venue did not carry the order out, when it said: of a refusal, and ErrorPriceLimit of an order the venue cancelled for its price. Empty otherwise. Kept in memory only: cex_order has no column for it

	// The order's way, for measuring where its time goes. Local clock:
	// registered, handed to the venue, the venue's acceptance in hand, the
	// final state in hand. The venue's clock: its gateway took the request,
	// it created the order, its last change. The two clocks differ by an
	// offset that is not known exactly, so only differences on one clock are
	// exact.
	SubmittedAt, SentAt, AckedAt time.Time // local clock
	FinalAt                      time.Time // local clock: the final state arrived; zero until then
	AcceptedAt                   time.Time // the venue's clock: its gateway took the request; zero when the venue did not say
	ExchangeCreatedAt            time.Time // the venue's clock: it created the order; zero until a report or the acceptance says
	ExchangeUpdatedAt            time.Time // the venue's time of its last change
	UpdateReceivedAt             time.Time // local arrival of the last report
	UpdateSeq                    uint64    // grows with every change of the record
}

// OrderAck is the venue's acceptance of an order. It says nothing about
// fills.
type OrderAck struct {
	OrderID    OrderID
	ReceivedAt time.Time
	AcceptedAt time.Time // the venue's clock: its gateway took the request; zero when the venue does not say
	CreatedAt  time.Time // the venue's clock: it created the order; zero when the acceptance does not say
}

// Fill is one execution as the venue reported it.
type Fill struct {
	ExecID        string
	OrderID       OrderID
	ClientOrderID ClientOrderID
	Symbol        Symbol
	Side          Side
	Price, Qty    decimal.Decimal
	Value         decimal.Decimal // USDT
	Fee           decimal.Decimal // USDT; positive is paid
	ExecTime      time.Time       // the venue's time of the execution
	ReceivedAt    time.Time
}

// PrivateEvent is one push of the private stream. Exactly one of Order, Fill
// and Positions is set. Positions is always the whole set of the two legs:
// it replaces what was published, so a leg it leaves out is flat.
type PrivateEvent struct {
	Order      *OrderRecord
	Fill       *Fill
	Positions  *PositionSet
	ReceivedAt time.Time
}

// --- errors ---

var (
	// ErrNotSent: the order did not leave the process.
	ErrNotSent = errors.New("cex: order not sent")
	// ErrOrderStateUnknown: the venue may or may not have the order.
	ErrOrderStateUnknown = errors.New("cex: order state unknown")
	// ErrUnknownOrder: the venue says there is no such order.
	ErrUnknownOrder = errors.New("cex: the venue has no such order")
	// ErrIssuerKey: the issuer's API key is missing or was refused. Its NAV
	// and distributions cannot be read until the process restarts with a
	// working key.
	ErrIssuerKey = errors.New("cex: the issuer's API key is missing or was refused")
)

// ErrorKind groups the venue's refusals by what the sender does next.
type ErrorKind string

const (
	ErrorRateLimited ErrorKind = "rate_limited" // the same order may be sent again later
	ErrorPriceLimit  ErrorKind = "price_limit"  // the price is outside the venue's band: not resent at the same price
	ErrorOther       ErrorKind = "other"
)

// VenueError is a refusal the venue stated: the request reached it and was
// not carried out.
type VenueError struct {
	Code, Message string
	Kind          ErrorKind
}

func (e *VenueError) Error() string {
	return fmt.Sprintf("venue error %s: %s", e.Code, e.Message)
}

// --- interfaces ---

// Venue is one exchange as cex uses it: reference reads, the market-data and
// private streams, and the one order command. An implementation converts the
// exchange's wire shapes to these types and does nothing else. It has no
// method that cancels or amends an order or changes an account setting: the
// process sends IOC orders only and reads the account's settings without
// changing them.
type Venue interface {
	Name() string

	// --- reference ---

	// MarketRules reads the rules of the given contracts.
	MarketRules(ctx context.Context, symbols []Symbol) (map[Symbol]MarketRule, error)
	// AccountSettings reads the position mode and the given contracts'
	// margin mode and leverage. A contract the account has no setting for
	// is left out of BySymbol.
	AccountSettings(ctx context.Context, symbols []Symbol) (AccountSettings, error)
	// Positions reads the account's positions in the given contracts.
	Positions(ctx context.Context, symbols []Symbol) (PositionSet, error)
	// OrderByClientID reads one order. ErrUnknownOrder: the venue says
	// there is no such order.
	OrderByClientID(ctx context.Context, symbol Symbol, id ClientOrderID) (OrderRecord, error)
	// RecentOrders reads the contract's orders since then, for a restart to
	// recognise its own by their id format.
	RecentOrders(ctx context.Context, symbol Symbol, since time.Time) ([]OrderRecord, error)
	// Fills reads the account's executions since then, of every contract:
	// the venue cannot filter by symbol, the caller does.
	Fills(ctx context.Context, since time.Time) ([]Fill, error)
	// FinancialRecords reads the funding settlements booked on the account
	// since then, of every contract: the caller filters by symbol.
	FinancialRecords(ctx context.Context, since time.Time) ([]FinancialRecord, error)
	// FundingHistory reads the contract's most recent settled funding
	// rates, newest first, at most limit of them.
	FundingHistory(ctx context.Context, symbol Symbol, limit int) ([]FundingSettlement, error)

	// --- streams ---
	//
	// A subscription lives until ctx ends and reconnects by itself. Its
	// handlers run on the connection's read goroutine and must not block.
	// onDown is called after every loss of the connection. The events of
	// one subscription are delivered one at a time, in the order the venue
	// produced them.

	// SubscribeBooks streams the given topics over one connection.
	SubscribeBooks(ctx context.Context, subs []BookSubscription, onEvent func(BookEvent), onDown func(error)) error
	// SubscribePrivate streams the account's orders, fills and positions of
	// the given contracts. What the order table relies on:
	//   - a report of an order (here and from OrderByClientID) is the
	//     order's whole state. Its Status is StatusAcked for every state
	//     that is not final, whatever the venue calls it;
	//   - FeeKnown is true only when Fee covers all of CumExecQty;
	//   - a position event is the whole set of the given contracts, a flat
	//     one left out.
	SubscribePrivate(ctx context.Context, symbols []Symbol, onEvent func(PrivateEvent), onDown func(error)) error

	// --- command ---

	// PlaceOrder sends one IOC limit order. ErrNotSent: nothing left the
	// process. *VenueError: the venue refused it. ErrOrderStateUnknown: the
	// venue may have it.
	PlaceOrder(ctx context.Context, req OrderRequest) (OrderAck, error)
}

// VenueStatus is implemented by a venue that has connections of its own
// worth a place in the status line: StatusAttrs returns their state as log
// keys and values.
type VenueStatus interface {
	StatusAttrs() []any
}

// AccountAssetsReader is implemented by a venue that reads the account's
// equity and margin ratio: the real exchange. The paper venue does not: it
// reads no private endpoint.
type AccountAssetsReader interface {
	AccountAssets(ctx context.Context) (AccountAssets, error)
}

// PrivateStreamStatus is implemented by a venue whose order outcomes and
// position changes arrive over a connection that can be lost: the real
// exchange. The paper venue matches inside the process and does not
// implement it.
type PrivateStreamStatus interface {
	// PrivateUp reports whether the private stream is logged in with its
	// topics subscribed.
	PrivateUp() bool
}

// IndexSource reads the index price behind the bell anchor and the venue's
// schedule of the US equity market (Binance).
type IndexSource interface {
	// IndexClose is the close of the one-minute index kline of symbol that
	// opens at minute. ok is false when the source has no kline of exactly
	// that minute yet.
	IndexClose(ctx context.Context, symbol Symbol, minute time.Time) (price decimal.Decimal, ok bool, err error)
	// TradingSchedule lists the regular sessions the source publishes, about
	// a week back and ahead, oldest first.
	TradingSchedule(ctx context.Context) ([]ScheduleDay, error)
}

// FXSource reads USDT-USD (Coinbase).
type FXSource interface {
	// Quote is the best bid and ask now.
	Quote(ctx context.Context) (bid, ask decimal.Decimal, err error)
	// MinuteClose is the last trade price of the minute that starts at
	// minute. ok is false when the source has no candle of that minute.
	MinuteClose(ctx context.Context, minute time.Time) (price decimal.Decimal, ok bool, err error)
	// MinuteCloses lists the minutes of [from, to) that have a candle,
	// oldest first.
	MinuteCloses(ctx context.Context, from, to time.Time) ([]FXMinute, error)
}

// IssuerSource reads the funds' net asset values and distributions
// (Direxion). Both methods return ErrIssuerKey when the key is missing or
// refused.
type IssuerSource interface {
	// NAV is the fund's net asset value of the trading day tradeDate (a New
	// York date). ok is false while the issuer has not published it.
	NAV(ctx context.Context, ticker string, tradeDate time.Time) (nav decimal.Decimal, ok bool, err error)
	// Distributions lists the fund's distributions as the issuer declares
	// them, one per ex-date.
	Distributions(ctx context.Context, ticker string) ([]Distribution, error)
}

// --- timings ---

// timings are the manager's clock and its fixed intervals and sizes. They
// are not configuration; tests replace the clock and shorten the intervals.
type timings struct {
	clock func() time.Time // every loop reads the time here

	bookInbox int           // book frames that may wait for Run; a frame arriving beyond this is dropped and counted
	statusLog time.Duration // one status line this often

	fxPoll time.Duration // F is read this often
	fxKeep time.Duration // the last F of each minute is kept this long, for the bell anchor

	anchorTick       time.Duration // the anchor state is advanced this often
	bellDelay        time.Duration // the index kline of the closing minute is read this long after the close
	anchorPoll       time.Duration // an anchor source that had nothing is asked again this often
	distributionPoll time.Duration // each fund's distributions are checked against the configuration this often

	referenceTick   time.Duration // the reference reads are checked for being due this often
	referenceRetry  time.Duration // a reference read that failed is tried again after this
	rulesPoll       time.Duration // the market rules are re-read this often
	settingsPoll    time.Duration // the account settings are re-read this often
	recordsPoll     time.Duration // the funding records are read this often
	recordsLookback time.Duration // the first read of the funding records goes back this far
	fundingPoll     time.Duration // the settled funding rates are read this often
	fundingRates    int           // and this many of them per leg
	assetsPoll      time.Duration // the account's equity and margin ratio are read this often, as often as inventory samples
	schedulePoll    time.Duration // the calendar is compared with the venue's schedule this often
	sourceCall      time.Duration // the limit of one read of a venue or a source

	placeTimeout  time.Duration // the limit of one PlaceOrder call; past it the order's outcome is unknown
	ackGrace      time.Duration // an accepted order without a final state for this long is unknown: its push may be lost
	orderTick     time.Duration // the order table is checked for what is due this often
	orderQuery    time.Duration // an order that needs a query is queried this often
	queryTimeout  time.Duration // the limit of one order query or fills read
	notFoundAfter time.Duration // an unknown order the venue says it does not have counts as not found only after this long
}

// defaultTimings are the values used against the real sources.
var defaultTimings = timings{clock: time.Now, bookInbox: 4096, statusLog: 30 * time.Second,
	fxPoll: 5 * time.Second, fxKeep: 10 * time.Minute,
	anchorTick: time.Second, bellDelay: time.Minute, anchorPoll: 5 * time.Minute, distributionPoll: 24 * time.Hour,
	referenceTick: time.Second, referenceRetry: 5 * time.Second, rulesPoll: time.Hour, settingsPoll: time.Minute,
	recordsPoll: time.Minute, recordsLookback: 7 * 24 * time.Hour, fundingPoll: 5 * time.Minute, fundingRates: 100, assetsPoll: 10 * time.Second,
	schedulePoll: time.Hour, sourceCall: 10 * time.Second,
	placeTimeout: time.Second, ackGrace: time.Second, orderTick: 200 * time.Millisecond, orderQuery: time.Second, queryTimeout: 2 * time.Second, notFoundAfter: 30 * time.Second}
