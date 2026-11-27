// Package bitget is the Bitget implementation of cex.Venue (UTA v3
// interface, USDT futures). The exchange's endpoints, field names and JSON
// shapes stay in this package: everything it hands out is a cex type.
package bitget

import (
	"encoding/json"
	"strings"
	"time"
)

// --- endpoints ---

const (
	restURL      = "https://api.bitget.com"
	wsPublicURL  = "wss://ws.bitget.com/v3/ws/public"
	wsPrivateURL = "wss://ws.bitget.com/v3/ws/private"

	pathInstruments    = "/api/v3/market/instruments"       // public: the contract rules
	pathFundingHistory = "/api/v3/market/history-fund-rate" // public: the settled funding rates
	pathServerTime     = "/api/v3/market/time"              // public: the server's clock; the request that keeps the REST connection warm

	pathSettings         = "/api/v3/account/settings"          // the position mode and each contract's margin mode and leverage
	pathPositions        = "/api/v3/position/current-position" // the positions of one contract
	pathOrderInfo        = "/api/v3/trade/order-info"          // one order by its client id
	pathHistoryOrders    = "/api/v3/trade/history-orders"      // a contract's finished orders, newest first
	pathFills            = "/api/v3/trade/fills"               // the account's executions; it has no symbol filter
	pathFinancialRecords = "/api/v3/account/financial-records" // the account's ledger; it has no symbol filter
	pathAssets           = "/api/v3/account/assets"            // the whole account's equity and margin ratio, for the board
	pathPlaceOrder       = "/api/v3/trade/place-order"         // the one request that changes the account

	wsLoginPath = "/user/verify" // the path signed in a private stream login

	// headerAcceptTime is the response header with the time, in unix
	// milliseconds, at which the exchange's application took the request.
	headerAcceptTime = "X-Bg-Request-Accept-Time"

	restCategory = "USDT-FUTURES" // REST spells the product line in upper case
	wsInstType   = "usdt-futures" // the public WebSocket in lower case
	wsPrivateUTA = "UTA"          // the instType of every private topic: they are account-wide

	codeSuccess      = "00000" // the envelope's code of a request that was carried out
	codeOrderUnknown = "25204" // "Order does not exist"

	// fundingHistoryPage is the most settlements one request returns.
	fundingHistoryPage = 100
	// listPage is the most rows one page of a list endpoint returns.
	listPage = 100
)

// The private topics, and the topic of an order sent over the WebSocket.
const (
	topicOrder      = "order"
	topicFill       = "fill"
	topicPosition   = "position"
	topicPlaceOrder = "place-order"
)

// fundingRecordTypes are the financial-record types of a funding settlement
// on a cross-margin position: received and paid. Their money is in the
// row's amount, signed.
var fundingRecordTypes = []string{"CONTRACT_MAIN_SETTLE_FEE_USER_IN", "CONTRACT_MAIN_SETTLE_FEE_USER_OUT"}

// The values of fields this client reads.
const (
	holdModeOneWay         = "one_way_mode"
	marginModeCrossed      = "crossed"
	posSideShort           = "short"
	cancelReasonPriceLimit = "limit_price_exceed_cancel" // the order's price is outside the allowed band
)

// --- error codes ---

// outcomeUnknownCodes are the answers to a place-order request after which
// the order may or may not exist: the exchange's documentation says to query
// the order by its client id for the first three, and the texts of the
// others (system error, timed out, concurrent operation) do not say whether
// the order was taken. Such an order is queried and never sent again: the
// request is not idempotent on the client id.
var outcomeUnknownCodes = map[string]bool{"40010": true, "40725": true, "45001": true,
	"25000": true, "25001": true, "40015": true, "40910": true, "25003": true, "40908": true}

// priceLimitCodes are the refusals of a price outside the allowed band.
// The documentation lists these pairs without saying which one a futures
// order gets.
var priceLimitCodes = map[string]bool{"41117": true, "41118": true, "25205": true, "25206": true, "22046": true, "22047": true,
	"40815": true, "40816": true, "43008": true, "43009": true, "25654": true, "25655": true}

// reduceQtyCodes are the refusals of a reduce-only order larger than the
// position it closes: 40804 by WebSocket, 25231 by REST (live 2026-11-27,
// SOXL 7.80 against a net of 7.79).
var reduceQtyCodes = map[string]bool{"40804": true, "25231": true}

// rateLimitCodes are the refusals of a request that came too often: by
// REST, and 30006 on a WebSocket connection.
var rateLimitCodes = map[string]bool{"429": true, "25004": true, "40500": true, "30006": true}

// --- timings ---

// wsTimings are one connection's clocks. The exchange answers a text "ping"
// with "pong" and may send nothing else for a quiet book, so a read that
// waits longer than readTimeout means the connection is dead.
type wsTimings struct {
	dialTimeout  time.Duration
	pingInterval time.Duration // the exchange closes a connection that sent no ping for two minutes, and one that sends more than ten in five seconds
	readTimeout  time.Duration
	writeTimeout time.Duration
	reconnectMin time.Duration // the first wait before a reconnect; it doubles up to reconnectMax
	reconnectMax time.Duration // a connection that lived this long resets the wait

	// The choice of a connection by its ping (wsConn.choose). The exchange's
	// connections differ: on some a message and its answer take a few
	// milliseconds, on others tens, for as long as the connection lives;
	// the dial time does not tell them apart, a few pings do. The pings of
	// the sample are sent pingGap apart, each after a silence, as an order
	// is: pings sent back to back come back fast on a slow connection too
	// and do not predict what a later message takes.
	pingSamples   int           // pings sent on a new connection; their median is its measure
	pingGap       time.Duration // the silence before each ping of the sample
	pingThreshold time.Duration // a median above this makes the client dial another connection
	pingAttempts  int           // connections tried at most; the one with the lowest median is kept
	pingWait      time.Duration // the longest wait for one pong
}

// defaultWSTimings are the clocks used against the real exchange. A choice
// takes three seconds a connection and nine at most; its pings, one a
// second, stay far below the exchange's limit of ten in five seconds on one
// connection.
var defaultWSTimings = wsTimings{dialTimeout: 10 * time.Second, pingInterval: 5 * time.Second, readTimeout: 15 * time.Second, writeTimeout: 5 * time.Second,
	reconnectMin: 200 * time.Millisecond, reconnectMax: 5 * time.Second,
	pingSamples: 3, pingGap: time.Second, pingThreshold: 15 * time.Millisecond, pingAttempts: 3, pingWait: time.Second}

// restTimings are the REST client's clocks. The exchange's edge closes a
// connection that carried no request for 400 seconds; Go's default transport
// closes an idle one after 90. An order sent on a connection that has to be
// opened first is about 6 ms later, so the client keeps its connection open:
// it does not close an idle one for idleConn, and Run sends a cheap public
// request every keepWarm, well inside both limits.
type restTimings struct {
	timeout  time.Duration // the limit of one request
	idleConn time.Duration // an idle connection is kept this long by this client
	keepWarm time.Duration // a public request this often keeps the connection in use
}

var defaultRESTTimings = restTimings{timeout: 10 * time.Second, idleConn: 5 * time.Minute, keepWarm: 30 * time.Second}

// restIdleConns is how many idle connections to the exchange the client
// keeps: two orders sent at once find a connection each when the server
// speaks HTTP/1.1; over HTTP/2 one connection carries both.
const restIdleConns = 4

// feeDecimals is the scale an order's fee is cut to. The exchange reports
// the same fee with eight decimals by order query and in the fills, cut,
// not rounded, and with more decimals in the order push; cut, every source
// gives the same number.
const feeDecimals = 8

// --- wire: REST ---

// restEnvelope wraps every REST response.
type restEnvelope struct {
	Code string          `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// instrumentRow is one contract of GET /api/v3/market/instruments. Prices
// are multiples of priceMultiplier, quantities of quantityMultiplier;
// quantities are in the base coin (shares).
type instrumentRow struct {
	Symbol             string `json:"symbol"`
	PriceMultiplier    string `json:"priceMultiplier"`
	QuantityMultiplier string `json:"quantityMultiplier"`
	MinOrderQty        string `json:"minOrderQty"`
	MinOrderAmount     string `json:"minOrderAmount"` // USDT
	Status             string `json:"status"`         // "online" while the contract trades
}

// statusOnline is instrumentRow.Status of a contract that trades.
const statusOnline = "online"

// fundingHistoryData is the data of GET /api/v3/market/history-fund-rate.
type fundingHistoryData struct {
	ResultList []fundingHistoryRow `json:"resultList"` // newest first
}

// fundingHistoryRow is one past settlement.
type fundingHistoryRow struct {
	Symbol               string     `json:"symbol"`
	FundingRate          string     `json:"fundingRate"`
	FundingRateTimestamp wireNumber `json:"fundingRateTimestamp"` // unix milliseconds of the settlement
}

// settingsData is the data of GET /api/v3/account/settings. The list holds
// only the contracts the account was ever configured for.
type settingsData struct {
	HoldMode         string `json:"holdMode"` // one_way_mode or hedge_mode
	SymbolConfigList []struct {
		Category   string `json:"category"`
		Symbol     string `json:"symbol"`
		MarginMode string `json:"marginMode"` // crossed or isolated
		Leverage   string `json:"leverage"`
	} `json:"symbolConfigList"`
}

// listData is the data of a list endpoint: a page of rows, newest first,
// and the cursor of the next page. An empty result has list null or [].
type listData[T any] struct {
	List   []T    `json:"list"`
	Cursor string `json:"cursor"`
}

// positionRow is one position, as the REST endpoint and as the private
// stream report it. The quantity is unsigned, in total (REST) or size
// (stream); the direction is posSide. The mark price, unrealised PnL and
// liquidation price are kept for the board only.
type positionRow struct {
	Symbol           string     `json:"symbol"`
	PosSide          string     `json:"posSide"` // long or short
	Total            string     `json:"total"`   // REST
	Size             string     `json:"size"`    // private stream
	AvgPrice         string     `json:"avgPrice"`
	UpdatedTime      wireNumber `json:"updatedTime"` // unix milliseconds
	MarkPrice        string     `json:"markPrice"`
	UnrealisedPnl    string     `json:"unrealisedPnl"`    // USDT
	LiquidationPrice string     `json:"liquidationPrice"` // REST
	LiqPrice         string     `json:"liqPrice"`         // private stream
}

// assetsData is the data of GET /api/v3/account/assets: the whole account,
// whatever program holds its positions. Only the two fields the board shows
// are read.
type assetsData struct {
	USDTEquity string `json:"usdtEquity"` // "Account equity (USDT)"
	MgnRatio   string `json:"mgnRatio"`   // "Margin ratio": (maintenance margin + partial liquidation fees) / equity, by the UTA glossary
}

// feeDetailRow is one fee of an order or a fill: positive is paid.
type feeDetailRow struct {
	FeeCoin string `json:"feeCoin"`
	Fee     string `json:"fee"`
}

// orderRow is one order, as order-info, history-orders and the private
// stream's order topic report it. Each report is the order's whole state.
type orderRow struct {
	OrderID      string         `json:"orderId"`
	ClientOid    string         `json:"clientOid"`
	Symbol       string         `json:"symbol"`
	Side         string         `json:"side"`
	Price        string         `json:"price"`
	Qty          string         `json:"qty"`        // base coin
	ReduceOnly   string         `json:"reduceOnly"` // YES / NO by REST, yes / no by the stream
	OrderStatus  string         `json:"orderStatus"`
	CumExecQty   string         `json:"cumExecQty"`
	CumExecValue string         `json:"cumExecValue"` // quote coin, exact
	AvgPrice     string         `json:"avgPrice"`
	FeeDetail    []feeDetailRow `json:"feeDetail"`
	CancelReason string         `json:"cancelReason"`
	CreatedTime  wireNumber     `json:"createdTime"` // unix milliseconds
	UpdatedTime  wireNumber     `json:"updatedTime"` // unix milliseconds: of an IOC order, its last fill
}

// fillRow is one execution, as the fills endpoint and the private stream's
// fill topic report it. The execution's time is createdTime by REST and
// execTime by the stream.
type fillRow struct {
	ExecID      string         `json:"execId"`
	OrderID     string         `json:"orderId"`
	ClientOid   string         `json:"clientOid"`
	Symbol      string         `json:"symbol"`
	Side        string         `json:"side"`
	ExecPrice   string         `json:"execPrice"`
	ExecQty     string         `json:"execQty"`
	ExecValue   string         `json:"execValue"` // rounded to four decimals by the exchange
	FeeDetail   []feeDetailRow `json:"feeDetail"`
	CreatedTime wireNumber     `json:"createdTime"` // REST
	ExecTime    wireNumber     `json:"execTime"`    // private stream
}

// financialRecordRow is one row of the account's ledger.
type financialRecordRow struct {
	ID     string     `json:"id"`
	Symbol string     `json:"symbol"`
	Type   string     `json:"type"`
	Amount string     `json:"amount"` // signed; the money of a cross-margin funding row
	Ts     wireNumber `json:"ts"`     // unix milliseconds, a few seconds after the settlement
}

// placeOrderBody is the body of POST /api/v3/trade/place-order: an IOC
// limit order on a cross-margin contract in one-way mode, where a position
// side is not sent and a reducing order says so with reduceOnly.
type placeOrderBody struct {
	Category    string `json:"category"`
	Symbol      string `json:"symbol"`
	Side        string `json:"side"`
	OrderType   string `json:"orderType"`
	TimeInForce string `json:"timeInForce"`
	Price       string `json:"price"`
	Qty         string `json:"qty"`
	ReduceOnly  string `json:"reduceOnly"` // yes or no
	MarginMode  string `json:"marginMode"`
	ClientOid   string `json:"clientOid"`
}

// placeOrderData is the data of an accepted place-order request. It says
// that the exchange took the request, nothing about fills.
type placeOrderData struct {
	OrderID   string `json:"orderId"`
	ClientOid string `json:"clientOid"`
}

// --- wire: WebSocket ---

// wsRequest is a subscribe request.
type wsRequest struct {
	Op   string       `json:"op"`
	Args []wsTopicArg `json:"args"`
}

// wsTopicArg names one topic: of one contract on the public stream, of the
// whole account (no symbol) on the private one.
type wsTopicArg struct {
	InstType string `json:"instType"`
	Topic    string `json:"topic"`
	Symbol   string `json:"symbol,omitempty"`
}

// wsLoginRequest is the login of the private stream. The signature is over
// timestamp + "GET" + wsLoginPath; the timestamp is unix milliseconds.
type wsLoginRequest struct {
	Op   string       `json:"op"`
	Args []wsLoginArg `json:"args"`
}

type wsLoginArg struct {
	APIKey     string `json:"apiKey"`
	Passphrase string `json:"passphrase"`
	Timestamp  string `json:"timestamp"`
	Sign       string `json:"sign"`
}

// tradeRequest is one order sent over the WebSocket: op "trade", topic
// "place-order". The id comes back in the answer; it is the client order
// id. The category is in lower case here and in upper case by REST.
type tradeRequest struct {
	Op       string          `json:"op"`
	ID       string          `json:"id"`
	Category string          `json:"category"`
	Topic    string          `json:"topic"`
	Args     []tradeOrderArg `json:"args"`
}

// tradeOrderArg is the order of a tradeRequest: the fields of the REST
// place-order body without the category. All values are text.
type tradeOrderArg struct {
	Symbol      string `json:"symbol"`
	OrderType   string `json:"orderType"`
	Qty         string `json:"qty"`
	Price       string `json:"price"`
	Side        string `json:"side"`
	TimeInForce string `json:"timeInForce"`
	ReduceOnly  string `json:"reduceOnly"` // wsReduceOnlyYes or wsReduceOnlyNo
	ClientOid   string `json:"clientOid"`
	MarginMode  string `json:"marginMode"`
}

// tradeFrame is a frame of the trade connection: the login's answer, the
// acceptance of an order (event "trade", code 0) or an error (event
// "error"). The id is that of the request; some error frames carry none.
// The exchange writes id and code as text in some frames and as numbers in
// others.
type tradeFrame struct {
	Event string        `json:"event"`
	ID    wireNumber    `json:"id"`
	Code  wireNumber    `json:"code"`
	Msg   string        `json:"msg"`
	Args  []tradeAckArg `json:"args"`
	// When the exchange's gateway took the request, unix microseconds. On
	// the wire it stands beside args (observed 2026-11-25), not inside each
	// order as the documentation's example shows.
	ReceiveTime wireNumber `json:"receiveTime"`
}

// tradeAckArg is the order of an acceptance: the exchange's order id and
// when its order system created the order (unix milliseconds).
type tradeAckArg struct {
	OrderID   string     `json:"orderId"`
	ClientOid string     `json:"clientOid"`
	CTime     wireNumber `json:"cTime"`
}

// wsFrame is any JSON frame of a stream: a data frame has Action
// ("snapshot" or "update"), a control frame has Event ("login", "subscribe",
// "error"). A successful login carries the code 0, as a number on the wire.
type wsFrame struct {
	Action string          `json:"action"`
	Event  string          `json:"event"`
	Code   wireNumber      `json:"code"`
	Msg    string          `json:"msg"`
	Arg    wsTopicArg      `json:"arg"`
	Data   json.RawMessage `json:"data"`
}

// bookRow is one element of a book frame's data: the levels as [price,
// quantity] strings, best first, a quantity of "0" deleting the price in an
// update. seq is the matching sequence number, shared by every book topic of
// the contract; pseq is the seq of the frame an update follows, 0 in a
// snapshot.
type bookRow struct {
	Asks    [][]string `json:"a"`
	Bids    [][]string `json:"b"`
	Seq     wireNumber `json:"seq"`
	PrevSeq wireNumber `json:"pseq"`
	Ts      wireNumber `json:"ts"` // unix milliseconds of the book state
}

// wireNumber is a number the exchange writes sometimes as a JSON number and
// sometimes as a JSON string; it keeps the digits either way.
type wireNumber string

func (n *wireNumber) UnmarshalJSON(b []byte) error {
	*n = wireNumber(strings.Trim(string(b), `"`))
	return nil
}
