// Package booktape records the public WebSocket frames of the combos'
// perpetual contracts on Bitget and Binance verbatim, in gzip files per
// stream and hour, and derives from them one row per contract and second in
// etfpair.perp_second: the executable book (5 levels a side; a contract whose
// price tick is finer than 0.01 gets the levels spanning the same prices,
// Bitget books50's 50 at a 0.001 tick, or Binance depth10) and the last trade
// as of each whole second, for the Combo Spread board and backtests. It only
// reads public market data and only writes its own files and that table.
package booktape

import (
	"encoding/json"
	"log/slog"
	"time"

	"etf-pair-arbitrage/dashboard/internal/scan"
)

// --- configuration ---

// Config is the part of dashboard/config.yaml the recorder reads; the other
// keys belong to pairscan.
type Config struct {
	DSNEnv string       `yaml:"dsn_env"` // environment variable holding the PostgreSQL DSN
	Combos []scan.Combo `yaml:"combos"`  // every market's bull, bear and ref contracts are recorded
}

// --- feeds ---

// endpoints are the venues' public market-data URLs: the WebSocket roots and
// the REST hosts whose instrument lists give the price ticks.
type endpoints struct {
	bitgetWS, bitgetREST, binanceWS, binanceREST string
}

// feed is one WebSocket connection to a venue and the streams it carries,
// dialled again whenever the connection ends. The fields after streams are
// owned by the feed's goroutine (run) alone.
type feed struct {
	name    string   // bitget-<combo id in lower case>, binance-public or binance-market
	venue   string   // bitget or binance
	url     string   // the endpoint; Binance's names the streams
	request []byte   // subscribe frame sent after each dial; nil when the URL names the streams
	ping    []byte   // text heartbeat (Bitget "ping"); nil for a protocol ping
	streams []string // "<SYMBOL>.<topic>", in subscription order

	output string // root of the raw files
	log    *slog.Logger
	rows   *writer          // receives the per-second rows
	tapes  map[string]*tape // every stream of streams
	line   []byte           // the data line being built
	out    []row            // rows emitted while processing one frame

	// The current connection.
	connFrames int  // data frames delivered
	warned     bool // an unreadable frame or unreadable row fields were logged

	// Counted since the last status line.
	frames, bytes, late, queued int
}

// tape is the recording state of one stream of a feed.
type tape struct {
	file    *tapeFile // the open data file; nil until the stream's first record
	tracker *tracker  // its kind of the per-second rows; nil for a stream without one
	seen    bool      // a frame arrived on the current connection
}

// frame is one WebSocket message as the reader received it.
type frame struct {
	receivedMs int64 // local receive time
	binary     bool
	payload    []byte // the bytes as received
}

// routed is what the recorder reads of a text frame.
type routed struct {
	stream  string  // "<SYMBOL>.<topic>" of a data frame; empty for a control frame
	payload []byte  // the data frame to store, as received
	refused bool    // a control frame reporting an error: Bitget refused a subscription
	pong    bool    // Bitget's answer to the text heartbeat, not stored
	events  []event // the frame's events of its per-second kind, ascending by (exchange time, id)
}

// --- per-second rows ---

// kind is one part of a per-second row: the book columns or the trade columns.
type kind int8

const (
	kindNone  kind = iota
	kindBook       // bid_px, bid_sz, ask_px, ask_sz, book_ms: the API-executable levels (RPI excluded)
	kindTrade      // price, trade_ms: the newest trade
)

// level is one price level of a book: price and size, decimal strings as
// sent; "" is NULL.
type level [2]string

// event is one book state or one trade as the per-second rows use it.
// Events are immutable once decoded: rows share their slices.
type event struct {
	exchangeMs int64   // book_ms or trade_ms: the venue's stamp
	id         int64   // trade id, which breaks ties between trades of one millisecond; 0 for a book event
	price      string  // trade: the price as sent
	bids, asks []level // book: the frame's levels, best first
}

// row is one kind's columns of the per-second row (venue, symbol, second_ms).
type row struct {
	venue, symbol string
	secondMs      int64
	kind          kind
	event         event
}

// rowKey identifies what one upsert of a row sets.
type rowKey struct {
	venue, symbol string
	secondMs      int64
	kind          kind
}

func (r row) key() rowKey { return rowKey{r.venue, r.symbol, r.secondMs, r.kind} }

// --- events.jsonl lines ---

// eventHead is the start of every line of events.jsonl.
type eventHead struct {
	ReceivedMs int64  `json:"received_ms"`
	Feed       string `json:"feed"`
	Event      string `json:"event"` // connected, disconnected, control or unreadable
}

// connectedEvent follows each dial; request is null when the URL names the streams.
type connectedEvent struct {
	eventHead
	URL     string          `json:"url"`
	Request json.RawMessage `json:"request"`
}

// disconnectedEvent ends a connection: error is "shutdown" on a clean stop;
// frames counts its data frames.
type disconnectedEvent struct {
	eventHead
	Error  string `json:"error"`
	Frames int    `json:"frames"`
}

// controlEvent is a venue's control frame (an acknowledgement or an error).
type controlEvent struct {
	eventHead
	Payload json.RawMessage `json:"payload"`
}

// unreadableEvent is a text frame that is neither JSON nor Bitget's pong, or a binary frame.
type unreadableEvent struct {
	eventHead
	Text string `json:"text"` // its first bytes
}

// --- constants ---

const (
	secondMs     = int64(1000)
	hourMs       = int64(3_600_000)
	finalAfterMs = int64(2000) // a second's row is final once the clock passes it by more than this
	holdRowsMs   = hourMs      // rows a failing database keeps waiting are dropped after this long
	unreadableN  = 200         // bytes of an unreadable frame kept in its event
	shutdown     = "shutdown"  // the disconnected error of a clean stop
	writeWait    = 10 * time.Second

	// Book depth per contract, chosen by its price tick at start. Every
	// recorded book spans the same prices: 5 levels at a 0.01 tick, so 50 at
	// 0.001 (the owner, 2026-11-22: Bitget books50 whole). Binance has no
	// contract with such a tick now; depth10 would be its deepest choice there.
	levelsCoarse      = 5                // levels a side of a contract whose tick is 0.01 or coarser
	levelsFineBitget  = 50               // books50
	levelsFineBinance = 10               // depth10
	restTimeout       = 15 * time.Second // one instrument request at start
)

// utc8 names the day directories; stored times are Unix milliseconds.
var utc8 = time.FixedZone("UTC+8", 8*3600)

// Timings of the feeds and the writer. They are variables only so that the
// tests can shorten them.
var (
	// A connection from which nothing arrives for readTimeout, not even the answer to a heartbeat, is dead and its
	// values stop being known (one silent for 27 s once kept writing 27-second-old books, 2026-11-21 05:13 UTC+8).
	pingEvery   = 5 * time.Second
	readTimeout = 10 * time.Second
	redialAfter = time.Second     // pause before dialling again
	redialIdle  = 5 * time.Second // pause after a failed dial or a connection without data frames
	flushEvery  = 5 * time.Second // open gzip files are flushed this often
	statusEvery = 5 * time.Minute // status lines
	writeEvery  = 5 * time.Second // per-second rows are written this often
)
