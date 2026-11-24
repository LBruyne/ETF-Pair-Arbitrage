package booktape

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/dashboard/internal/scan"
)

// venueEndpoints are the public market-data endpoints the recorder uses.
var venueEndpoints = endpoints{
	bitgetWS:    "wss://ws.bitget.com/v3/ws/public",
	bitgetREST:  "https://api.bitget.com",
	binanceWS:   "wss://fstream.binance.com",
	binanceREST: "https://fapi.binance.com",
}

// Instrument lists, read once at start for the price ticks.
const (
	bitgetInstruments  = "/api/v3/market/instruments?category=USDT-FUTURES"
	binanceInstruments = "/fapi/v1/exchangeInfo"
)

// bitgetTopics are recorded for every Bitget contract after its book topic
// (bitgetBook). books1 (every change of the best bid and ask) is kept in the
// files only; the per-second rows take their levels from the book topic.
var bitgetTopics = []string{"books1", "publicTrade", "ticker", "funding-rate"}

// binanceMarket are the channels binance-market carries for every contract:
// the URL name after "<symbol>@" and the topic it is stored as.
// binance-public carries each contract's book channel (binanceBook) and
// binanceTop.
var binanceMarket = [][2]string{{"aggTrade", "aggTrade"}, {"markPrice@1s", "markPrice"}}

// binanceTop is every change of the best bid and ask, kept in the files only
// (as Bitget books1).
var binanceTop = [2]string{"bookTicker", "bookTicker"}

// fineTick: a contract whose price tick is smaller gets the fine depth of its venue.
var fineTick = decimal.New(1, -2)

// --- feeds ---

// feedsOf builds the feeds of the combos' markets: one Bitget feed per combo,
// and binance-public and binance-market carrying every Binance contract.
// Contracts are bull, bear and ref in configuration order, without
// duplicates. Each venue's instrument list is read once first: a contract's
// price tick sets its book depth.
func feedsOf(ctx context.Context, combos []scan.Combo, ep endpoints, log *slog.Logger) ([]*feed, error) {
	type bitgetMarket struct {
		combo   string
		symbols []string
	}
	var onBitget []bitgetMarket
	var onBitgetAll, onBinance []string
	for _, combo := range combos {
		for _, market := range combo.Markets {
			var symbols []string
			for _, s := range []string{market.BullSymbol, market.BearSymbol, market.RefSymbol} {
				if s != "" && !slices.Contains(symbols, s) {
					symbols = append(symbols, s)
				}
			}
			switch market.Venue {
			case "bitget":
				onBitget = append(onBitget, bitgetMarket{combo.ID, symbols})
				onBitgetAll = append(onBitgetAll, symbols...)
			case "binance":
				for _, s := range symbols {
					if !slices.Contains(onBinance, s) {
						onBinance = append(onBinance, s)
					}
				}
			default:
				return nil, fmt.Errorf("combo %q: venue %q is neither bitget nor binance", combo.ID, market.Venue)
			}
		}
	}
	if len(onBitget) == 0 && len(onBinance) == 0 {
		return nil, errors.New("the combos name no contract")
	}

	var feeds []*feed
	if len(onBitget) > 0 {
		levels, err := depthOf(ctx, "bitget", ep.bitgetREST+bitgetInstruments, bitgetTicks, onBitgetAll, levelsFineBitget, log)
		if err != nil {
			return nil, err
		}
		for _, m := range onBitget {
			feeds = append(feeds, bitgetFeed(m.combo, m.symbols, levels, ep.bitgetWS))
		}
	}
	if len(onBinance) > 0 {
		levels, err := depthOf(ctx, "binance", ep.binanceREST+binanceInstruments, binanceTicks, onBinance, levelsFineBinance, log)
		if err != nil {
			return nil, err
		}
		book := func(symbol string) [][2]string { return [][2]string{binanceBook(levels[symbol]), binanceTop} }
		market := func(string) [][2]string { return binanceMarket }
		feeds = append(feeds, binanceFeed("binance-public", ep.binanceWS+"/public/stream", onBinance, book),
			binanceFeed("binance-market", ep.binanceWS+"/market/stream", onBinance, market))
	}
	return feeds, nil
}

// depthOf reads a venue's instrument list and returns the book depth of each
// contract: fine levels a side when its price tick is finer than 0.01,
// otherwise levelsCoarse. A contract without a tick in the list is an error.
func depthOf(ctx context.Context, venue, url string, ticksOf func([]byte) (map[string]string, error), symbols []string, fine int,
	log *slog.Logger) (map[string]int, error) {
	body, err := getBody(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("%s instruments: %w", venue, err)
	}
	ticks, err := ticksOf(body)
	if err != nil {
		return nil, fmt.Errorf("%s instruments: %w", venue, err)
	}
	levels := map[string]int{}
	var text []string
	for _, symbol := range symbols {
		tick, err := decimal.NewFromString(ticks[symbol])
		if err != nil {
			return nil, fmt.Errorf("%s instruments: no price tick of %s (%q)", venue, symbol, ticks[symbol])
		}
		levels[symbol] = levelsCoarse
		if tick.LessThan(fineTick) {
			levels[symbol] = fine
		}
		text = append(text, fmt.Sprintf("%s=%d(tick %s)", symbol, levels[symbol], ticks[symbol]))
	}
	log.Info("booktape: book levels from the price ticks", "venue", venue, "levels", strings.Join(text, ","))
	return levels, nil
}

// getBody reads one public REST endpoint.
func getBody(ctx context.Context, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, restTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// bitgetTicks reads GET /api/v3/market/instruments: a contract's price tick
// is its priceMultiplier (prices are multiples of it).
func bitgetTicks(body []byte) (map[string]string, error) {
	var r struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
		Data []struct {
			Symbol          string `json:"symbol"`
			PriceMultiplier string `json:"priceMultiplier"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	if r.Code != "00000" {
		return nil, fmt.Errorf("code %s: %s", r.Code, r.Msg)
	}
	ticks := map[string]string{}
	for _, d := range r.Data {
		ticks[d.Symbol] = d.PriceMultiplier
	}
	return ticks, nil
}

// binanceTicks reads GET /fapi/v1/exchangeInfo: a contract's price tick is
// the tickSize of its PRICE_FILTER.
func binanceTicks(body []byte) (map[string]string, error) {
	var r struct {
		Symbols []struct {
			Symbol  string `json:"symbol"`
			Filters []struct {
				FilterType string `json:"filterType"`
				TickSize   string `json:"tickSize"`
			} `json:"filters"`
		} `json:"symbols"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	ticks := map[string]string{}
	for _, s := range r.Symbols {
		for _, f := range s.Filters {
			if f.FilterType == "PRICE_FILTER" {
				ticks[s.Symbol] = f.TickSize
			}
		}
	}
	return ticks, nil
}

// bitgetArg is one subscription of a Bitget subscribe frame.
type bitgetArg struct {
	InstType string `json:"instType"`
	Topic    string `json:"topic"`
	Symbol   string `json:"symbol"`
}

// bitgetFeed subscribes every topic of the combo's contracts in one frame:
// the book topic of the contract's depth, then bitgetTopics.
func bitgetFeed(comboID string, symbols []string, levels map[string]int, endpoint string) *feed {
	var args []bitgetArg
	var streams []string
	for _, symbol := range symbols {
		for _, topic := range append([]string{bitgetBook(levels[symbol])}, bitgetTopics...) {
			args = append(args, bitgetArg{InstType: "usdt-futures", Topic: topic, Symbol: symbol})
			streams = append(streams, symbol+"."+topic)
		}
	}
	return &feed{name: "bitget-" + strings.ToLower(comboID), venue: "bitget", url: endpoint,
		request: subscribeFrame(args), ping: []byte("ping"), streams: streams}
}

// bitgetBook is the book topic of a depth: books5, or books50 for
// levelsFineBitget.
func bitgetBook(levels int) string {
	if levels == levelsFineBitget {
		return "books50"
	}
	return "books5"
}

func subscribeFrame(args []bitgetArg) []byte {
	b, _ := json.Marshal(struct {
		Op   string      `json:"op"`
		Args []bitgetArg `json:"args"`
	}{"subscribe", args}) // plain strings: cannot fail
	return b
}

// binanceFeed names its streams in the URL; Binance sends no acknowledgement.
// channels gives a contract's channels: the URL name after "<symbol>@" and
// the topic it is stored as.
func binanceFeed(name, route string, symbols []string, channels func(symbol string) [][2]string) *feed {
	var names, streams []string
	for _, symbol := range symbols {
		for _, c := range channels(symbol) {
			names = append(names, strings.ToLower(symbol)+"@"+c[0])
			streams = append(streams, symbol+"."+c[1])
		}
	}
	return &feed{name: name, venue: "binance", url: route + "?streams=" + strings.Join(names, "/"), streams: streams}
}

// binanceBook is the book channel of a depth (depth5 or depth10, at most
// every 100 ms) and its topic.
func binanceBook(levels int) [2]string {
	topic := "depth" + strconv.Itoa(levels)
	return [2]string{topic + "@100ms", topic}
}

// rowKind is the part of the per-second rows a topic carries: the book
// columns from every book frame (Bitget books5 and books50, Binance depth5
// and depth10), the trade columns from every trade (Bitget publicTrade,
// Binance aggTrade), or none.
func rowKind(venue, topic string) kind {
	switch venue + "." + topic {
	case "bitget.books5", "bitget.books50", "binance.depth5", "binance.depth10":
		return kindBook
	case "bitget.publicTrade", "binance.aggTrade":
		return kindTrade
	}
	return kindNone
}

// --- routing ---

// route reads one text frame. A frame that is not JSON is an error with an
// empty stream; a data frame whose per-second fields cannot be read is an
// error with its stream and payload set, so that it is still stored.
func route(venue string, payload []byte) (routed, error) {
	if venue == "bitget" {
		return routeBitget(payload)
	}
	return routeBinance(payload)
}

// routeBitget: frames with "action" are data frames of arg.symbol and
// arg.topic, the others control frames ("event": subscribe, error).
func routeBitget(payload []byte) (routed, error) {
	if string(payload) == "pong" {
		return routed{pong: true}, nil
	}
	var f struct {
		Action string `json:"action"`
		Event  string `json:"event"`
		Arg    struct {
			Topic  string `json:"topic"`
			Symbol string `json:"symbol"`
		} `json:"arg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(payload, &f); err != nil {
		if json.Valid(payload) {
			return routed{}, nil // JSON of another shape: a control frame
		}
		return routed{}, err
	}
	if f.Action == "" {
		return routed{refused: f.Event == "error"}, nil
	}
	r := routed{stream: f.Arg.Symbol + "." + f.Arg.Topic, payload: payload}
	var err error
	switch f.Arg.Topic {
	case "books5":
		r.events, err = bitgetLevels(f.Data, levelsCoarse)
	case "books50":
		r.events, err = bitgetLevels(f.Data, levelsFineBitget)
	case "publicTrade":
		r.events, err = bitgetFills(f.Data)
	}
	return r, err
}

// bitgetLevels: data[0].ts is the matching time of the book state, b and a
// its levels best first, of which the first n are kept.
func bitgetLevels(data json.RawMessage, n int) ([]event, error) {
	var d []struct {
		Bids []level `json:"b"`
		Asks []level `json:"a"`
		Ts   string  `json:"ts"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("book data: %w", err)
	}
	if len(d) == 0 {
		return nil, errors.New("book data is empty")
	}
	ts, err := strconv.ParseInt(d[0].Ts, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("book ts: %w", err)
	}
	return []event{{exchangeMs: ts, bids: d[0].Bids[:min(len(d[0].Bids), n)], asks: d[0].Asks[:min(len(d[0].Asks), n)]}}, nil
}

// bitgetFills: every fill (RPI fills included), newest first as sent; they
// are applied oldest first, as if they had arrived one by one.
func bitgetFills(data json.RawMessage) ([]event, error) {
	var d []struct {
		ID    string `json:"i"`
		Price string `json:"p"`
		T     string `json:"T"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("publicTrade data: %w", err)
	}
	events := make([]event, 0, len(d))
	for _, fill := range d {
		t, err1 := strconv.ParseInt(fill.T, 10, 64)
		id, err2 := strconv.ParseInt(fill.ID, 10, 64)
		if err := errors.Join(err1, err2); err != nil {
			return nil, fmt.Errorf("publicTrade fill: %w", err)
		}
		events = append(events, event{exchangeMs: t, id: id, price: fill.Price})
	}
	slices.SortFunc(events, func(a, b event) int {
		return cmp.Or(cmp.Compare(a.exchangeMs, b.exchangeMs), cmp.Compare(a.id, b.id))
	})
	return events, nil
}

// routeBinance: frames with "stream" are data frames of the upper-case
// symbol and the channel name up to its first "@", the others control frames.
func routeBinance(payload []byte) (routed, error) {
	var f struct {
		Stream string          `json:"stream"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(payload, &f); err != nil {
		if json.Valid(payload) {
			return routed{}, nil
		}
		return routed{}, err
	}
	if f.Stream == "" {
		return routed{}, nil
	}
	symbol, rest, _ := strings.Cut(f.Stream, "@")
	channel, _, _ := strings.Cut(rest, "@")
	r := routed{stream: strings.ToUpper(symbol) + "." + channel, payload: payload}
	switch channel {
	case "depth5", "depth10": // T is the matching time of the book state; b and a its levels, best first
		var d struct {
			T    int64   `json:"T"`
			Bids []level `json:"b"`
			Asks []level `json:"a"`
		}
		if err := json.Unmarshal(f.Data, &d); err != nil {
			return r, fmt.Errorf("%s data: %w", channel, err)
		}
		r.events = []event{{exchangeMs: d.T, bids: d.Bids, asks: d.Asks}}
	case "aggTrade": // T is the time of the aggregate's first trade, a its id
		var d struct {
			T     int64  `json:"T"`
			ID    int64  `json:"a"`
			Price string `json:"p"`
		}
		if err := json.Unmarshal(f.Data, &d); err != nil {
			return r, fmt.Errorf("aggTrade data: %w", err)
		}
		r.events = []event{{exchangeMs: d.T, id: d.ID, price: d.Price}}
	}
	return r, nil
}
