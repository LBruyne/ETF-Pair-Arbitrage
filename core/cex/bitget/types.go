// Package bitget is the Bitget implementation of cex.Venue (UTA v3
// interface, USDT futures). The exchange's endpoints, field names and JSON
// shapes stay in this package: everything it hands out is a cex type.
package bitget

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// --- endpoints ---

const (
	restURL     = "https://api.bitget.com"
	wsPublicURL = "wss://ws.bitget.com/v3/ws/public"

	pathInstruments    = "/api/v3/market/instruments"       // public: the contract rules
	pathFundingHistory = "/api/v3/market/history-fund-rate" // public: the settled funding rates

	restCategory = "USDT-FUTURES" // REST spells the product line in upper case
	wsInstType   = "usdt-futures" // the WebSocket in lower case

	codeSuccess = "00000" // the envelope's code of a request that was carried out

	// fundingHistoryPage is the most settlements one request returns.
	fundingHistoryPage = 100
)

// --- timings ---

// wsTimings are one connection's clocks. The exchange answers a text "ping"
// with "pong" and may send nothing else for a quiet book, so a read that
// waits longer than readTimeout means the connection is dead.
type wsTimings struct {
	dialTimeout  time.Duration
	pingInterval time.Duration // the exchange closes a connection that sent no ping for 30 s
	readTimeout  time.Duration
	writeTimeout time.Duration
	reconnectMin time.Duration // the first wait before a reconnect; it doubles up to reconnectMax
	reconnectMax time.Duration // a connection that lived this long resets the wait
}

// defaultWSTimings are the clocks used against the real exchange.
var defaultWSTimings = wsTimings{dialTimeout: 10 * time.Second, pingInterval: 5 * time.Second, readTimeout: 15 * time.Second, writeTimeout: 5 * time.Second,
	reconnectMin: 200 * time.Millisecond, reconnectMax: 5 * time.Second}

// restTimeout is the limit of one REST request.
const restTimeout = 10 * time.Second

// --- errors ---

// errNotImplemented is what the account-side methods return: this client
// has no signed REST and no private stream, so it reads no order, fill,
// position, setting or financial record and places no order.
var errNotImplemented = errors.New("bitget: the account side (signed REST, private stream) is not implemented")

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

// --- wire: WebSocket ---

// wsRequest is a subscribe request.
type wsRequest struct {
	Op   string       `json:"op"`
	Args []wsTopicArg `json:"args"`
}

// wsTopicArg names one topic of one contract.
type wsTopicArg struct {
	InstType string `json:"instType"`
	Topic    string `json:"topic"`
	Symbol   string `json:"symbol"`
}

// wsFrame is any JSON frame of the public stream: a data frame has Action
// ("snapshot" or "update"), a control frame has Event ("subscribe", "error").
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
