package scan

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// httpClient is shared by every source; each request also carries the
// caller's context.
var httpClient = &http.Client{Timeout: 15 * time.Second}

// getJSON reads one public endpoint into out. Numbers decode as json.Number
// so prices keep the digits the source sent.
func getJSON(ctx context.Context, rawURL string, query url.Values, out any) error {
	if len(query) > 0 {
		rawURL += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: http %d", req.URL.Host+req.URL.Path, resp.StatusCode)
	}
	dec := json.NewDecoder(resp.Body)
	dec.UseNumber()
	return dec.Decode(out)
}

// getBody reads one public page or file.
func getBody(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: http %d", req.URL.Host+req.URL.Path, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// --- Binance USD-M futures ---

// binanceFAPI is the host of Binance's USD-M futures market data.
var binanceFAPI = "https://fapi.binance.com"

// Binance reads fapi.binance.com public market data.
type Binance struct{}

// Candles reads GET /fapi/v1/klines: rows of [openTime, open, high, low, close, ...].
func (Binance) Candles(ctx context.Context, symbol string, startMs, endMs int64) ([]Candle, error) {
	var rows [][]any
	q := url.Values{"symbol": {symbol}, "interval": {"1m"}, "limit": {strconv.Itoa(candlePage)},
		"startTime": {strconv.FormatInt(startMs, 10)}, "endTime": {strconv.FormatInt(endMs-1, 10)}}
	if err := getJSON(ctx, binanceFAPI+"/fapi/v1/klines", q, &rows); err != nil {
		return nil, err
	}
	return candlesOf(rows, 0, 4, 1, startMs, endMs)
}

// IndexKline reads GET /fapi/v1/indexPriceKlines, the one-minute klines of
// a contract's index price (USDT; Binance's TradFi contracts have one too):
// rows of [open time, open, high, low, close, ...], a kline identified by its
// open time. limit=1 with startTime gives the first kline opening at or after
// it, however far after.
func (Binance) IndexKline(ctx context.Context, symbol string, minuteMs int64) (*Candle, error) {
	var rows [][]any
	q := url.Values{"pair": {symbol}, "interval": {"1m"}, "startTime": {strconv.FormatInt(minuteMs, 10)}, "limit": {"1"}}
	if err := getJSON(ctx, binanceFAPI+"/fapi/v1/indexPriceKlines", q, &rows); err != nil {
		return nil, err
	}
	klines, err := candlesOf(rows, 0, 4, 1, minuteMs, math.MaxInt64)
	if err != nil || len(klines) == 0 {
		return nil, err
	}
	return &klines[0], nil
}

// TradingSchedule reads GET /fapi/v1/tradingSchedule, the sessions of the
// markets under Binance's TradFi contracts from about a week back to a week
// ahead, and returns those of the US equity market (marketSchedules.EQUITY).
func (Binance) TradingSchedule(ctx context.Context) ([]ScheduleSession, error) {
	body, err := getBody(ctx, binanceFAPI+"/fapi/v1/tradingSchedule")
	if err != nil {
		return nil, err
	}
	return parseTradingSchedule(body)
}

func parseTradingSchedule(body []byte) ([]ScheduleSession, error) {
	var resp struct {
		MarketSchedules map[string]struct {
			Sessions []ScheduleSession `json:"sessions"`
		} `json:"marketSchedules"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("binance tradingSchedule: %w", err)
	}
	return resp.MarketSchedules["EQUITY"].Sessions, nil
}

// SpecialFunding reads GET /fapi/v1/fundingRate of [startMs, endMs) and
// returns the rate and the mark price of its special entry, the dividend
// adjustment of an equity contract (rateType Special); ok is false without one.
func (Binance) SpecialFunding(ctx context.Context, symbol string, startMs, endMs int64) (rate, mark string, ok bool, err error) {
	q := url.Values{"symbol": {symbol}, "startTime": {strconv.FormatInt(startMs, 10)}, "endTime": {strconv.FormatInt(endMs-1, 10)}, "limit": {"100"}}
	body, err := getBody(ctx, binanceFAPI+"/fapi/v1/fundingRate?"+q.Encode())
	if err != nil {
		return "", "", false, err
	}
	return parseSpecialFunding(body)
}

func parseSpecialFunding(body []byte) (rate, mark string, ok bool, err error) {
	var rows []struct {
		Rate     string `json:"fundingRate"`
		Mark     string `json:"markPrice"`
		RateType string `json:"rateType"`
	}
	if err := json.Unmarshal(body, &rows); err != nil {
		return "", "", false, fmt.Errorf("binance fundingRate: %w", err)
	}
	for _, r := range rows {
		if r.RateType == "Special" {
			return r.Rate, r.Mark, true, nil
		}
	}
	return "", "", false, nil
}

// --- Bitget UTA v3 ---

// bitgetAPI is the host of Bitget's public market data.
var bitgetAPI = "https://api.bitget.com"

// Bitget reads api.bitget.com public market data of USDT futures.
type Bitget struct{}

// bitgetRecent is how far back the candles endpoint is asked; it serves
// about 31 days, the history endpoint everything older.
const bitgetRecent = 25 * 24 * time.Hour

// bitgetGet unwraps the {code, msg, data} envelope.
func bitgetGet(ctx context.Context, path string, q url.Values, data any) error {
	var env struct {
		Code string          `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	q.Set("category", "USDT-FUTURES")
	if err := getJSON(ctx, bitgetAPI+path, q, &env); err != nil {
		return err
	}
	if env.Code != "00000" {
		return fmt.Errorf("bitget %s: %s %s", path, env.Code, env.Msg)
	}
	dec := json.NewDecoder(strings.NewReader(string(env.Data)))
	dec.UseNumber()
	return dec.Decode(data)
}

// Candles reads GET /api/v3/market/candles: rows of [ts, open, high, low, close, ...].
// The endpoint returns the newest ceil((endTime-startTime)/1m) candles that
// start at or before endTime, so endTime one millisecond before the range's
// end gives exactly the candles starting in [startMs, endMs).
//
// That endpoint keeps about a month. Older minutes come from the history
// endpoint (bitgetHistory), a page of 100 minutes at a time.
func (Bitget) Candles(ctx context.Context, symbol string, startMs, endMs int64) ([]Candle, error) {
	if startMs >= time.Now().Add(-bitgetRecent).UnixMilli() {
		var rows [][]any
		q := url.Values{"symbol": {symbol}, "interval": {"1m"}, "limit": {strconv.Itoa(candlePage)},
			"startTime": {strconv.FormatInt(startMs, 10)}, "endTime": {strconv.FormatInt(endMs-1, 10)}}
		if err := bitgetGet(ctx, "/api/v3/market/candles", q, &rows); err != nil {
			return nil, err
		}
		return candlesOf(rows, 0, 4, 1, startMs, endMs)
	}
	var out []Candle
	for from := startMs; from < endMs; from += bitgetHistoryPage * minuteMs {
		to := min(from+bitgetHistoryPage*minuteMs, endMs)
		// The history endpoint answers 429 well below its documented rate; a
		// refused page is asked for again after a pause, or the backfill
		// would leave a hole there.
		page, err := bitgetHistory(ctx, symbol, "", from, to)
		for try := 0; err != nil && try < 5 && ctx.Err() == nil; try++ {
			time.Sleep(3 * time.Second)
			page, err = bitgetHistory(ctx, symbol, "", from, to)
		}
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		time.Sleep(2 * historyPause)
	}
	return out, nil
}

// IndexKline reads the contract's index price candles (USDT, the quote
// currency of Bitget's TradFi contracts) from the history endpoint, which
// serves them back to the listing: the first one-minute index kline opening
// in the 100 minutes from fromMs, one page, nil when there is none.
func (Bitget) IndexKline(ctx context.Context, symbol string, fromMs int64) (*Candle, error) {
	page, err := bitgetHistory(ctx, symbol, "index", fromMs, fromMs+bitgetHistoryPage*minuteMs)
	if err != nil || len(page) == 0 {
		return nil, err
	}
	return &page[0], nil
}

// bitgetHistoryPage is the most candles GET /api/v3/market/history-candles returns.
const bitgetHistoryPage = 100

// bitgetHistory reads GET /api/v3/market/history-candles for the one-minute
// candles starting in [startMs, endMs), at most bitgetHistoryPage minutes of
// whole-minute bounds: rows of [ts, open, high, low, close, ...], those that
// have ended by endTime. kind is the candle type, the trades' when empty or
// "index" for the index price.
func bitgetHistory(ctx context.Context, symbol, kind string, startMs, endMs int64) ([]Candle, error) {
	var rows [][]any
	q := url.Values{"symbol": {symbol}, "interval": {"1m"}, "limit": {strconv.Itoa(bitgetHistoryPage)},
		"startTime": {strconv.FormatInt(startMs, 10)}, "endTime": {strconv.FormatInt(endMs, 10)}}
	if kind != "" {
		q.Set("type", kind)
	}
	if err := bitgetGet(ctx, "/api/v3/market/history-candles", q, &rows); err != nil {
		return nil, err
	}
	return candlesOf(rows, 0, 4, 1, startMs, endMs)
}

// SplitRecords reads GET /api/v3/market/split-records: the splits of
// Bitget's stock and ETF contracts (pending, ongoing or completed) of a
// recent window, about ten rows. It returns those of the given contracts
// (symbol -> ticker), the ratio as new shares per old share (adjustmentRatio:
// 3 for 3-for-1, 0.1 for 1-for-10) and the ex-date in New York.
func (Bitget) SplitRecords(ctx context.Context, tickers map[string]string) ([]Action, error) {
	const at = "/api/v3/market/split-records"
	body, err := getBody(ctx, bitgetAPI+at)
	if err != nil {
		return nil, err
	}
	return parseBitgetSplits(body, tickers, "https://api.bitget.com"+at, time.Now().UnixMilli())
}

func parseBitgetSplits(body []byte, tickers map[string]string, source string, readMs int64) ([]Action, error) {
	var env struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
		Data []struct {
			Symbol string `json:"symbol"`
			Ratio  string `json:"adjustmentRatio"`
			ExDate string `json:"exDividendDate"`
			Zone   string `json:"exDividendDateTimezone"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("bitget split-records: %w", err)
	}
	if env.Code != "00000" {
		return nil, fmt.Errorf("bitget split-records: %s %s", env.Code, env.Msg)
	}
	var out []Action
	for _, r := range env.Data {
		ticker, ours := tickers[r.Symbol]
		if !ours {
			continue
		}
		day, err := time.Parse("20060102", r.ExDate)
		if err != nil || r.Zone != "ET" {
			return nil, fmt.Errorf("bitget split-records %s: ex-date %q in %q", r.Symbol, r.ExDate, r.Zone)
		}
		out = append(out, Action{Ticker: ticker, ExDate: day.Format("2006-01-02"), Kind: "split", Value: r.Ratio, Status: "Known",
			Source: "bitget-split-records", URL: source, ReadMs: readMs})
	}
	return out, nil
}

// --- Coinbase Exchange USDT-USD ---

// Coinbase reads USD per USDT from api.exchange.coinbase.com.
type Coinbase struct{}

func (Coinbase) Pair() string { return "USDT-USD" }

// Quote reads GET /products/USDT-USD/ticker.
func (Coinbase) Quote(ctx context.Context) (Quote, error) {
	var row struct {
		Bid string `json:"bid"`
		Ask string `json:"ask"`
	}
	err := getJSON(ctx, "https://api.exchange.coinbase.com/products/USDT-USD/ticker", nil, &row)
	return Quote{Bid: row.Bid, Ask: row.Ask}, err
}

// coinbasePage is the most candles one request returns.
const coinbasePage = 300

// Candles reads GET /products/USDT-USD/candles: rows of [time s, low, high,
// open, close, volume], newest first, without the minutes that had no trade.
func (Coinbase) Candles(ctx context.Context, startMs, endMs int64) ([]Candle, error) {
	var out []Candle
	for from := startMs; from < endMs; from += coinbasePage * minuteMs {
		to := min(from+coinbasePage*minuteMs, endMs)
		var rows [][]any
		q := url.Values{"granularity": {"60"},
			"start": {time.UnixMilli(from).UTC().Format(time.RFC3339)},
			"end":   {time.UnixMilli(to - minuteMs).UTC().Format(time.RFC3339)}}
		if err := getJSON(ctx, "https://api.exchange.coinbase.com/products/USDT-USD/candles", q, &rows); err != nil {
			return nil, err
		}
		page, err := candlesOf(rows, 0, 4, 1000, from, to)
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
	}
	return out, nil
}

// candlesOf turns rows of mixed JSON values into candles inside [startMs,
// endMs). timeScale converts the row's time to milliseconds.
func candlesOf(rows [][]any, timeCol, closeCol int, timeScale int64, startMs, endMs int64) ([]Candle, error) {
	out := make([]Candle, 0, len(rows))
	for _, row := range rows {
		if len(row) <= closeCol {
			return nil, fmt.Errorf("candle row has %d columns", len(row))
		}
		ts, err := strconv.ParseInt(fmt.Sprint(row[timeCol]), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("candle time %v: %w", row[timeCol], err)
		}
		ts *= timeScale
		if ts < startMs || ts >= endMs {
			continue
		}
		out = append(out, Candle{MinuteMs: ts, Close: fmt.Sprint(row[closeCol])})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].MinuteMs < out[j].MinuteMs })
	return out, nil
}

// cents rounds a published NAV to the cent, the precision the issuers'
// pages show.
func cents(value string) (string, error) {
	d, err := decimal.NewFromString(strings.TrimSpace(value))
	if err != nil {
		return "", err
	}
	return d.Round(2).StringFixed(2), nil
}

// --- ProShares ---

// ProShares reads a ProShares ETF's product page, which shows the NAV of the
// latest trading day and that day's date, and the issuer's historical NAV
// file for past days.
type ProShares struct{}

var (
	proSharesDate = regexp.MustCompile(`id="price-asOfDate"[^>]*>\s*as of (\d{2}/\d{2}/\d{4})`)
	proSharesNAV  = regexp.MustCompile(`id="price-nav"[^>]*>\s*\$?([0-9.,]+)`)
)

// Latest reads each ticker's product page once: the NAV of the newest day
// it shows, whichever session is asked for.
func (ProShares) Latest(ctx context.Context, tickers []string, _ string) ([]NAV, error) {
	var navs []NAV
	for i, ticker := range tickers {
		if i > 0 {
			time.Sleep(sourcePause)
		}
		page, err := getBody(ctx, "https://www.proshares.com/our-etfs/leveraged-and-inverse/"+strings.ToLower(ticker))
		if err != nil {
			return nil, err
		}
		nav, err := proSharesNAVOf(page, ticker)
		if err != nil {
			return nil, err
		}
		nav.ReadMs = time.Now().UnixMilli()
		navs = append(navs, nav)
	}
	return navs, nil
}

// proSharesNAVOf reads a product page's NAV, shown to the cent, and the date it is for.
func proSharesNAVOf(page []byte, ticker string) (NAV, error) {
	date, nav := proSharesDate.FindSubmatch(page), proSharesNAV.FindSubmatch(page)
	if date == nil || nav == nil {
		return NAV{}, fmt.Errorf("proshares %s: the page has no dated NAV", ticker)
	}
	day, err := time.Parse("01/02/2006", string(date[1]))
	if err != nil {
		return NAV{}, fmt.Errorf("proshares date %q: %w", date[1], err)
	}
	value, err := cents(strings.ReplaceAll(string(nav[1]), ",", ""))
	if err != nil {
		return NAV{}, fmt.Errorf("proshares nav %q: %w", nav[1], err)
	}
	return NAV{Issuer: "proshares", Ticker: ticker, SessionDate: day.Format("2006-01-02"), Price: value, Via: "page"}, nil
}

// Past reads the ticker's historical NAV file, which holds every trading
// day since the fund started, and returns the days asked for. It waits
// sourcePause first: the files of both legs are read one after the other.
func (ProShares) Past(ctx context.Context, ticker string, sessionDates []string) ([]NAV, error) {
	time.Sleep(sourcePause)
	file, err := getBody(ctx, "https://accounts.profunds.com/etfdata/ByFund/"+ticker+"-historical_nav.csv")
	if err != nil {
		return nil, err
	}
	readMs := time.Now().UnixMilli()
	byDate, err := proSharesHistory(file)
	if err != nil {
		return nil, fmt.Errorf("proshares %s history: %w", ticker, err)
	}
	var out []NAV
	for _, date := range sessionDates {
		if price, ok := byDate[date]; ok {
			out = append(out, NAV{Issuer: "proshares", Ticker: ticker, SessionDate: date, Price: price, Via: "history-csv", ReadMs: readMs})
		}
	}
	return out, nil
}

// proSharesHistory reads a historical NAV file: a header naming the columns
// (Date, ProShares Name, Ticker, NAV, ...), then a row a day with the date
// as 01/02/2006 and the NAV to four places. It returns each day's NAV
// rounded to the cent, as the product page shows it.
func proSharesHistory(file []byte) (map[string]string, error) {
	rows, err := csv.NewReader(bytes.NewReader(file)).ReadAll()
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, errors.New("the file is empty")
	}
	dateCol, navCol := slices.Index(rows[0], "Date"), slices.Index(rows[0], "NAV")
	if dateCol < 0 || navCol < 0 {
		return nil, fmt.Errorf("header %q has no Date and NAV", rows[0])
	}
	out := make(map[string]string, len(rows)-1)
	for _, row := range rows[1:] {
		day, err := time.Parse("01/02/2006", row[dateCol])
		if err != nil {
			return nil, fmt.Errorf("date %q: %w", row[dateCol], err)
		}
		price, err := cents(row[navCol])
		if err != nil {
			return nil, fmt.Errorf("nav %q: %w", row[navCol], err)
		}
		out[day.Format("2006-01-02")] = price
	}
	return out, nil
}

// Distributions reads the product page's own request for a fund's
// distributions of a year, GET /api/distributionsummary?fund=T&year=Y: one
// row per distribution declared, on its declaration day (EffectiveDate, the
// day before ExDate), with CashDividendPerShare. Past rows are restated for
// later splits, so only an amount read before its ex-date passes is kept.
func (ProShares) Distributions(ctx context.Context, ticker string, year int) ([]Action, error) {
	at := proSharesDistributionsURL(ticker, year)
	body, err := getBody(ctx, at)
	if err != nil {
		return nil, err
	}
	return parseProSharesDistributions(body, ticker, at, time.Now().UnixMilli())
}

// proSharesDistributionsURL is the request of a fund's distributions of a
// year, as the product page sends it (with a slash before "?" the host
// answers with a redirect to this).
func proSharesDistributionsURL(ticker string, year int) string {
	return fmt.Sprintf("https://www.proshares.com/api/distributionsummary?fund=%s&year=%d", ticker, year)
}

func parseProSharesDistributions(body []byte, ticker, source string, readMs int64) ([]Action, error) {
	var rows []struct {
		ExDate string       `json:"ExDate"`
		Cash   *json.Number `json:"CashDividendPerShare"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&rows); err != nil {
		return nil, fmt.Errorf("proshares distributions %s: %w", ticker, err)
	}
	var out []Action
	for _, r := range rows {
		day, err := time.Parse("2006-01-02T15:04:05", r.ExDate)
		if err != nil || r.Cash == nil {
			return nil, fmt.Errorf("proshares distributions %s: ex-date %q, amount %v", ticker, r.ExDate, r.Cash)
		}
		out = append(out, Action{Ticker: ticker, ExDate: day.Format("2006-01-02"), Kind: "dividend", Value: r.Cash.String(), Status: "Known",
			Source: "proshares-api", URL: source, ReadMs: readMs})
	}
	return out, nil
}

// proSharesScheduleURL is the issuer's page of the expected distribution
// dates of its quarterly, monthly and weekly payers.
const proSharesScheduleURL = "https://www.proshares.com/resources/distributions/distribution-schedule"

// ScheduleDates reads the expected ex-dates of the quarterly payers (TQQQ
// and SQQQ among them) from the distribution schedule page.
func (ProShares) ScheduleDates(ctx context.Context) ([]string, error) {
	page, err := getBody(ctx, proSharesScheduleURL)
	if err != nil {
		return nil, err
	}
	return parseProSharesSchedule(page)
}

var (
	proSharesQuarterly = regexp.MustCompile(`(?s)What are the \d{4} Quarterly Dates\?.*?</table>`)
	proSharesRow       = regexp.MustCompile(`(?s)<tr[^>]*>\s*<td[^>]*>.*?</td>\s*<td[^>]*>\s*(\d{1,2}/\d{1,2}/\d{4})\s*</td>`)
)

// parseProSharesSchedule reads every "What are the YYYY Quarterly Dates?"
// table: a row per round (First Quarter ... and the potential distribution
// for excise tax purposes) whose first date is the ex-date.
func parseProSharesSchedule(page []byte) ([]string, error) {
	var out []string
	for _, table := range proSharesQuarterly.FindAll(page, -1) {
		for _, row := range proSharesRow.FindAllSubmatch(table, -1) {
			day, err := time.Parse("1/2/2006", string(row[1]))
			if err != nil {
				return nil, err
			}
			out = append(out, day.Format("2006-01-02"))
		}
	}
	if len(out) == 0 {
		return nil, errors.New("proshares distribution schedule: no quarterly dates on the page")
	}
	return out, nil
}

// --- Nasdaq ---

// NasdaqDividends reads GET api.nasdaq.com/api/quote/T/dividends?assetclass=etf,
// the dividends of a Nasdaq-listed ETF as declared (QQQ, SOXX, TQQQ, SQQQ):
// ex-date and cash amount, from the declaration. For a fund listed elsewhere
// (SOXL, SOXS on NYSE Arca) it answers without rows; they are not asked.
func NasdaqDividends(ctx context.Context, ticker string) ([]Action, error) {
	at := "https://api.nasdaq.com/api/quote/" + ticker + "/dividends?assetclass=etf"
	body, err := getBody(ctx, at)
	if err != nil {
		return nil, err
	}
	return parseNasdaqDividends(body, ticker, at, time.Now().UnixMilli())
}

// parseNasdaqDividends sums the rows of one ex-date: one distribution of
// several kinds is one adjustment.
func parseNasdaqDividends(body []byte, ticker, source string, readMs int64) ([]Action, error) {
	var resp struct {
		Data struct {
			Dividends struct {
				Rows []struct {
					ExDate string `json:"exOrEffDate"`
					Amount string `json:"amount"`
				} `json:"rows"`
			} `json:"dividends"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("nasdaq dividends %s: %w", ticker, err)
	}
	sums, order := map[string]decimal.Decimal{}, []string{}
	for _, r := range resp.Data.Dividends.Rows {
		day, dateErr := time.Parse("01/02/2006", r.ExDate)
		amount, amountErr := decimal.NewFromString(strings.TrimPrefix(r.Amount, "$"))
		if dateErr != nil || amountErr != nil {
			return nil, fmt.Errorf("nasdaq dividends %s: ex-date %q, amount %q", ticker, r.ExDate, r.Amount)
		}
		date := day.Format("2006-01-02")
		if _, seen := sums[date]; !seen {
			order = append(order, date)
		}
		sums[date] = sums[date].Add(amount)
	}
	var out []Action
	for _, date := range order {
		out = append(out, Action{Ticker: ticker, ExDate: date, Kind: "dividend", Value: sums[date].String(), Status: "Known",
			Source: "nasdaq", URL: source, ReadMs: readMs})
	}
	return out, nil
}

// --- Direxion ---

// Direxion reads a Direxion fund's pricing and distributions through the
// data API that direxion.com itself calls: an AWS AppSync GraphQL endpoint
// named in the site's scripts, which takes the site's API key in the
// X-Api-Key header. The owner authorised two read-only queries of SOXL and
// SOXS (OWNER.md items 4 and 11). getThirtyDayPrice, the site's own query,
// gives the NAV of one of the fund's last thirty trading days (null for a
// day it does not have: an older one, or the newest before it is
// published); the site's getPricing gives the newest day too but switches
// to a new day hours later (2026-11-20: SOXL 11-19 at 04:57 New York here,
// still 11-18 there at 06:42), so it is not used. getKurtosysFunds, the
// product page's query, gives the fund's distributions as declared. No
// answer states when a figure was published.
type Direxion struct {
	endpoint string
	key      string // from the environment; it goes in the request header only
	cal      Calendar
}

// newDirexion binds the site's API endpoint with the key.
func newDirexion(key string, cal Calendar) Direxion {
	return Direxion{endpoint: "https://p6vg2dlkpverjbr2afvpbqupky.appsync-api.us-east-1.amazonaws.com/graphql", key: key, cal: cal}
}

// The query text is the site's own.
const (
	direxionGetThirtyDayPrice = `query GetThirtyDayPrice($Ticker: String!, $TradeDate: String!) {
  getThirtyDayPrice(Ticker: $Ticker, TradeDate: $TradeDate) {
    ItemKey
    TradeDate
    Ticker
    Nav
    MarketClosingPrice
  }
}
`
	// direxionGetKurtosysFunds is the product page's query of a fund's data,
	// asked for its distributions and the dates the data are of (the page's
	// selection also carries its pricing and performance tables).
	direxionGetKurtosysFunds = `query GetKurtosysFunds($Ticker: String!) {
  getKurtosysFunds(Ticker: $Ticker) {
    Distributions {
      IncomeDividend
      ShortTermCapitalGain
      LongTermCapitalGain
      ReturnOfCapital
      ExDate
    }
    TimeStamp
    TradeDate
  }
}
`
)

const (
	// direxionDays is how many trading days the site's thirty-day pricing
	// lists; older days are not asked for.
	direxionDays = 30
	// direxionPause spaces the requests to Direxion's API: every query waits
	// it first.
	direxionPause = time.Second
)

// Latest asks getThirtyDayPrice once per ticker for the session; a ticker
// whose NAV of the session the API does not have yet is left out.
func (d Direxion) Latest(ctx context.Context, tickers []string, session string) ([]NAV, error) {
	var out []NAV
	for _, ticker := range tickers {
		nav, err := d.day(ctx, ticker, session)
		if err != nil {
			return nil, err
		}
		if nav != nil {
			out = append(out, *nav)
		}
	}
	return out, nil
}

// Past asks getThirtyDayPrice for each of the days that is among the last
// thirty trading days.
func (d Direxion) Past(ctx context.Context, ticker string, sessionDates []string) ([]NAV, error) {
	now := time.Now()
	recent := d.cal.tradingDays(now.AddDate(0, 0, -2*direxionDays), now)
	oldest := recent[max(0, len(recent)-direxionDays)]
	var out []NAV
	for _, date := range sessionDates {
		if date < oldest {
			continue
		}
		nav, err := d.day(ctx, ticker, date)
		if err != nil {
			return nil, err
		}
		if nav != nil {
			out = append(out, *nav)
		}
	}
	return out, nil
}

// day asks getThirtyDayPrice for the NAV of one ticker and trading day: nil
// when the API does not have it.
func (d Direxion) day(ctx context.Context, ticker, date string) (*NAV, error) {
	day, err := time.Parse("2006-01-02", date)
	if err != nil {
		return nil, err
	}
	body, err := d.query(ctx, direxionGetThirtyDayPrice, map[string]string{"Ticker": ticker, "TradeDate": day.Format("01022006")})
	if err != nil {
		return nil, err
	}
	readMs := time.Now().UnixMilli()
	nav, err := direxionNAV(body, "getThirtyDayPrice", ticker)
	if nav != nil {
		nav.ReadMs = readMs
	}
	return nav, err
}

// Distributions asks getKurtosysFunds once for a fund's distributions as
// declared (Direxion keeps the declared amount after a later split): one
// reading per ex-date, Known with the cash per share of income, capital gains
// and return of capital summed, None for a row of zeros. A row without any
// amount is no reading: its ex-date is in unfilled, a round the fund lists
// before its amounts. The list holds the rounds declared and nothing ahead of
// a declaration. timeStamp and tradeDate (MMDDYYYY) are the dates the answer
// gives its data as of.
func (d Direxion) Distributions(ctx context.Context, ticker string) (rows []Action, unfilled []string, timeStamp, tradeDate string, err error) {
	body, err := d.query(ctx, direxionGetKurtosysFunds, map[string]string{"Ticker": ticker})
	if err != nil {
		return nil, nil, "", "", err
	}
	return parseDirexionDistributions(body, ticker, d.distributionsURL(ticker), time.Now().UnixMilli())
}

// distributionsURL names the request of a fund's distributions as it is
// stored with a reading: the endpoint and the query.
func (d Direxion) distributionsURL(ticker string) string {
	return d.endpoint + " getKurtosysFunds(Ticker: " + ticker + ").Distributions"
}

func parseDirexionDistributions(body []byte, ticker, source string, readMs int64) (rows []Action, unfilled []string, timeStamp, tradeDate string, err error) {
	var resp struct {
		Data struct {
			Fund *struct {
				Distributions []struct {
					Income    *json.Number `json:"IncomeDividend"`
					ShortTerm *json.Number `json:"ShortTermCapitalGain"`
					LongTerm  *json.Number `json:"LongTermCapitalGain"`
					Capital   *json.Number `json:"ReturnOfCapital"`
					ExDate    string       `json:"ExDate"`
				} `json:"Distributions"`
				TimeStamp string `json:"TimeStamp"`
				TradeDate string `json:"TradeDate"`
			} `json:"getKurtosysFunds"`
		} `json:"data"`
		Errors []struct {
			ErrorType string `json:"errorType"`
			Message   string `json:"message"`
		} `json:"errors"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&resp); err != nil {
		return nil, nil, "", "", fmt.Errorf("direxion distributions %s: %w", ticker, err)
	}
	if len(resp.Errors) > 0 {
		return nil, nil, "", "", fmt.Errorf("direxion distributions %s: %s %s", ticker, resp.Errors[0].ErrorType, resp.Errors[0].Message)
	}
	fund := resp.Data.Fund
	if fund == nil {
		return nil, nil, "", "", fmt.Errorf("direxion distributions %s: no fund data", ticker)
	}
	sums, order := map[string]decimal.Decimal{}, []string{}
	for _, r := range fund.Distributions {
		parts := []*json.Number{r.Income, r.ShortTerm, r.LongTerm, r.Capital}
		day, err := time.Parse("01022006", r.ExDate)
		if !slices.ContainsFunc(parts, func(p *json.Number) bool { return p != nil }) {
			if err == nil { // a round listed before its amounts: still unknown, not None
				unfilled = append(unfilled, day.Format("2006-01-02"))
			}
			continue
		}
		if err != nil {
			return nil, nil, "", "", fmt.Errorf("direxion distributions %s: ex-date %q", ticker, r.ExDate)
		}
		date := day.Format("2006-01-02")
		if _, seen := sums[date]; !seen {
			order = append(order, date)
		}
		for _, part := range parts {
			if part == nil {
				continue
			}
			amount, err := decimal.NewFromString(part.String())
			if err != nil {
				return nil, nil, "", "", fmt.Errorf("direxion distributions %s %s: amount %q", ticker, date, part.String())
			}
			sums[date] = sums[date].Add(amount)
		}
	}
	for _, date := range order {
		a := Action{Ticker: ticker, ExDate: date, Kind: "dividend", Value: sums[date].String(), Status: "Known", Source: "direxion-api", URL: source,
			ReadMs: readMs}
		if !sums[date].IsPositive() {
			a.Value, a.Status = "", "None"
		}
		rows = append(rows, a)
	}
	return rows, unfilled, fund.TimeStamp, fund.TradeDate, nil
}

// query posts one GraphQL query, direxionPause after the one before, and
// returns the response body. A refused key is errKeyRejected; no error text
// carries the key.
func (d Direxion) query(ctx context.Context, text string, variables map[string]string) ([]byte, error) {
	time.Sleep(direxionPause)
	payload, err := json.Marshal(map[string]any{"query": text, "variables": variables})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", d.key)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("%s: http %d: %w", req.URL.Host, resp.StatusCode, errKeyRejected)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("%s: http %d", req.URL.Host, resp.StatusCode)
	}
	return body, nil
}

// direxionNAV reads a response whose data.<field> is a ThirtyDayPricing
// object: Nav a number, TradeDate as 01022006. A null field is a day the
// API does not have: no NAV and no error.
func direxionNAV(body []byte, field, ticker string) (*NAV, error) {
	var resp struct {
		Data map[string]*struct {
			Nav       *json.Number `json:"Nav"`
			TradeDate string       `json:"TradeDate"`
		} `json:"data"`
		Errors []struct {
			ErrorType string `json:"errorType"`
			Message   string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("direxion %s: %w", ticker, err)
	}
	if len(resp.Errors) > 0 {
		return nil, fmt.Errorf("direxion %s: %s %s", ticker, resp.Errors[0].ErrorType, resp.Errors[0].Message)
	}
	p := resp.Data[field]
	if p == nil {
		return nil, nil
	}
	if p.Nav == nil {
		return nil, fmt.Errorf("direxion %s %s: no NAV", ticker, p.TradeDate)
	}
	day, err := time.Parse("01022006", p.TradeDate)
	if err != nil {
		return nil, fmt.Errorf("direxion %s trade date %q: %w", ticker, p.TradeDate, err)
	}
	price, err := cents(p.Nav.String())
	if err != nil {
		return nil, fmt.Errorf("direxion %s nav %q: %w", ticker, p.Nav.String(), err)
	}
	return &NAV{Issuer: "direxion", Ticker: ticker, SessionDate: day.Format("2006-01-02"), Price: price, Via: "api"}, nil
}
