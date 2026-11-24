package scan

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Candle rows arrive as mixed JSON values in three shapes; each must yield
// the minute start in milliseconds and the close, inside the asked range.
func TestCandlesOfReadsEveryVenueShape(t *testing.T) {
	binance := [][]any{{json.Number("120000"), "1", "2", "0", "51.51000"}, {json.Number("60000"), "1", "2", "0", "51.50000"}}
	got, err := candlesOf(binance, 0, 4, 1, 60000, 180000)
	if err != nil || len(got) != 2 || got[0] != (Candle{MinuteMs: 60000, Close: "51.50000"}) || got[1].MinuteMs != 120000 {
		t.Fatalf("binance rows: %v %v", got, err)
	}
	bitget := [][]any{{"60000", "1", "2", "0", "51.5"}, {"180000", "1", "2", "0", "9"}}
	if got, _ = candlesOf(bitget, 0, 4, 1, 60000, 180000); len(got) != 1 || got[0].Close != "51.5" {
		t.Fatalf("bitget rows outside [start, end) must be dropped: %v", got)
	}
	coinbase := [][]any{{json.Number("120"), json.Number("0.9988"), json.Number("0.9989"), json.Number("0.9988"), json.Number("0.99881")}}
	if got, _ = candlesOf(coinbase, 0, 4, 1000, 0, 180000); len(got) != 1 || got[0] != (Candle{MinuteMs: 120000, Close: "0.99881"}) {
		t.Fatalf("coinbase seconds must become milliseconds: %v", got)
	}
	if _, err = candlesOf([][]any{{"60000", "1"}}, 0, 4, 1, 0, 180000); err == nil {
		t.Fatal("a short row must be an error, not a silent skip")
	}
}

// The index kline is asked for by pair, one-minute interval, the minute as
// startTime and limit 1; the answer is the first kline opening at or after
// it (a row as the documentation gives it), or none.
func TestBinanceIndexKline(t *testing.T) {
	var asked url.Values
	answer := `[[1763585940000,"49.10","49.20","49.00","49.12345678","0",1763585999999,"0",60,"0","0","0"]]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fapi/v1/indexPriceKlines" {
			http.NotFound(w, r)
			return
		}
		asked = r.URL.Query()
		fmt.Fprint(w, answer)
	}))
	defer srv.Close()
	defer func(was string) { binanceFAPI = was }(binanceFAPI)
	binanceFAPI = srv.URL

	k, err := Binance{}.IndexKline(context.Background(), "SOXLUSDT", 1763585940000)
	if err != nil || k == nil || *k != (Candle{MinuteMs: 1763585940000, Close: "49.12345678"}) {
		t.Fatalf("got %+v %v", k, err)
	}
	want := url.Values{"pair": {"SOXLUSDT"}, "interval": {"1m"}, "startTime": {"1763585940000"}, "limit": {"1"}}
	if fmt.Sprint(asked) != fmt.Sprint(want) {
		t.Fatalf("query %v, want %v", asked, want)
	}
	answer = `[]`
	if k, err = (Binance{}).IndexKline(context.Background(), "SOXLUSDT", 1763585940000); err != nil || k != nil {
		t.Fatalf("no kline: got %+v %v, want none", k, err)
	}
}

// Bitget's index kline is asked for on the history endpoint with type index,
// one page of 100 minutes from the minute; the answer's first row is the
// kline (rows as answered on 2026-11-20 for SOXXUSDT), an empty page none,
// and an error code an error. Old market candles come from the same
// endpoint without a type, a page per 100 minutes.
func TestBitgetIndexKline(t *testing.T) {
	var asked url.Values
	var pages []string
	answer := `{"code":"00000","msg":"success","requestTime":1795159546668,"data":[` +
		`["1793826000000","451.1","451.3","451.0","451.2","0","0"],["1793825940000","451.3","451.4","451.1","451.1829549182907088","0","0"]]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/market/history-candles" {
			http.NotFound(w, r)
			return
		}
		asked = r.URL.Query()
		pages = append(pages, r.URL.RawQuery)
		fmt.Fprint(w, answer)
	}))
	defer srv.Close()
	defer func(was string) { bitgetAPI = was }(bitgetAPI)
	bitgetAPI = srv.URL

	k, err := Bitget{}.IndexKline(context.Background(), "SOXXUSDT", 1793825940000)
	if err != nil || k == nil || *k != (Candle{MinuteMs: 1793825940000, Close: "451.1829549182907088"}) {
		t.Fatalf("got %+v %v", k, err)
	}
	want := url.Values{"category": {"USDT-FUTURES"}, "symbol": {"SOXXUSDT"}, "interval": {"1m"}, "type": {"index"}, "limit": {"100"},
		"startTime": {"1793825940000"}, "endTime": {"1793831940000"}}
	if fmt.Sprint(asked) != fmt.Sprint(want) {
		t.Fatalf("query %v, want %v", asked, want)
	}
	answer = `{"code":"00000","msg":"success","data":[]}`
	if k, err = (Bitget{}).IndexKline(context.Background(), "SOXXUSDT", 1793825940000); err != nil || k != nil {
		t.Fatalf("no kline: got %+v %v, want none", k, err)
	}
	answer = `{"code":"40034","msg":"Parameter does not exist","data":null}`
	if _, err = (Bitget{}).IndexKline(context.Background(), "SOXXUSDT", 1793825940000); err == nil {
		t.Fatal("an error code must be an error")
	}

	answer = `{"code":"00000","msg":"success","data":[]}`
	old := time.Now().AddDate(0, 0, -40).UnixMilli() / minuteMs * minuteMs
	pages = nil
	if _, err = (Bitget{}).Candles(context.Background(), "SOXLUSDT", old, old+150*minuteMs); err != nil {
		t.Fatal(err)
	}
	page := func(from, to int64) string {
		return url.Values{"category": {"USDT-FUTURES"}, "symbol": {"SOXLUSDT"}, "interval": {"1m"}, "limit": {"100"},
			"startTime": {fmt.Sprint(from)}, "endTime": {fmt.Sprint(to)}}.Encode()
	}
	if want := []string{page(old, old+100*minuteMs), page(old+100*minuteMs, old+150*minuteMs)}; !slices.Equal(pages, want) {
		t.Fatalf("market history pages\n%v\nwant\n%v", pages, want)
	}
}

// The regular close is 16:00 New York: 05:00 UTC+8 in winter, 04:00 in
// summer, and 13:00 New York on an early-close day.
func TestCloseOf(t *testing.T) {
	utc8 := time.FixedZone("UTC+8", 8*3600)
	for day, want := range map[string]string{
		"2026-11-17": "2026-11-18 05:00", // standard time
		"2027-07-01": "2027-07-02 04:00", // daylight time
		"2026-11-27": "2026-11-28 02:00", // 13:00 early close
		"2026-12-24": "2026-12-25 02:00", // 13:00 early close
	} {
		got, err := Calendar{}.closeOf(day)
		if err != nil || got.In(utc8).Format("2006-01-02 15:04") != want {
			t.Fatalf("%s: got %v %v, want %s UTC+8", day, got.In(utc8), err, want)
		}
	}
	if _, err := (Calendar{}).closeOf("2029-01-02"); err == nil {
		t.Fatal("a day past the calendar table must be an error")
	}
}

func nyTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.ParseInLocation("2006-01-02 15:04:05", s, newYork)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// The newest closed session skips weekends and holidays.
func TestLastClosedSession(t *testing.T) {
	for now, want := range map[string]string{
		"2026-11-17 15:59:00": "2026-11-16", // Tuesday, one minute before the close
		"2026-11-17 16:00:00": "2026-11-17",
		"2026-11-22 12:00:00": "2026-11-20", // Sunday: Friday's session
		"2026-11-26 18:00:00": "2026-11-25", // Thanksgiving is closed
		"2026-11-27 13:00:00": "2026-11-27", // early close at 13:00
	} {
		got, err := Calendar{}.lastClosedSession(nyTime(t, now))
		if err != nil || got != want {
			t.Fatalf("now %s: got %s %v, want %s", now, got, err, want)
		}
	}
	// The days whose closes bound the sessions around Thanksgiving week.
	got := Calendar{}.tradingDays(nyTime(t, "2026-11-25 09:00:00"), nyTime(t, "2026-11-30 09:00:00"))
	if want := []string{"2026-11-25", "2026-11-27", "2026-11-30"}; !slices.Equal(got, want) {
		t.Fatalf("trading days: got %v, want %v", got, want)
	}
}

// The bell is read one minute after each close: 16:01, or 13:01 after an
// early close, skipping the holiday and the weekend.
func TestNextBell(t *testing.T) {
	for now, want := range map[string]string{
		"2026-11-19 15:00:00": "2026-11-19 16:01:00",
		"2026-11-19 16:00:30": "2026-11-19 16:01:00", // inside the minute after the close
		"2026-11-19 16:01:00": "2026-11-20 16:01:00", // strictly after now
		"2026-11-25 17:00:00": "2026-11-27 13:01:00", // Thanksgiving, then the early close
		"2026-11-27 14:00:00": "2026-11-30 16:01:00", // the weekend
	} {
		got, err := Calendar{}.nextBell(nyTime(t, now))
		if err != nil || !got.Equal(nyTime(t, want)) {
			t.Fatalf("now %s: got %v %v, want %s New York", now, got.In(newYork), err, want)
		}
	}
}

// The phases cover the days without a hole: a normal day, the closed
// holiday after it and the early close that follows.
func TestPhasesOf(t *testing.T) {
	phases, err := Calendar{}.phasesOf([]string{"2026-11-25", "2026-11-27", "2026-11-30"})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for i, p := range phases {
		if i > 0 && !p.Start.Equal(phases[i-1].End) {
			t.Fatalf("hole before %s at %v", p.Name, p.Start)
		}
		got = append(got, p.Start.In(newYork).Format("01-02 15:04")+" "+p.Name)
	}
	want := []string{
		"11-24 20:00 Overnight", "11-25 04:00 Pre-Market", "11-25 09:30 Regular", "11-25 16:00 After-Hours",
		"11-25 20:00 Closed", // Thanksgiving
		"11-26 20:00 Overnight", "11-27 04:00 Pre-Market", "11-27 09:30 Regular", "11-27 13:00 After-Hours",
		"11-27 17:00 Closed", // the early close's after-hours ends at 17:00, then the weekend
		"11-29 20:00 Overnight", "11-30 04:00 Pre-Market", "11-30 09:30 Regular", "11-30 16:00 After-Hours",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got\n%v\nwant\n%v", got, want)
	}
}

// An issuer's float or decorated NAV becomes a decimal string to the cent.
func TestCents(t *testing.T) {
	for in, want := range map[string]string{"59.18000030517578": "59.18", "49.07": "49.07", " 58.770 ": "58.77", "48.759999": "48.76"} {
		if got, err := cents(in); err != nil || got != want {
			t.Fatalf("%q: got %q %v, want %q", in, got, err, want)
		}
	}
}

// A NAV is stable once the same figure has been read alone for the settle
// delay; a new figure restarts the clock, a returning one is counted from
// the last read of the figure it replaced, and a correction after a
// confirmation needs the full delay of its own.
func TestSettledNAV(t *testing.T) {
	t0 := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	ms := func(minutes int) int64 { return t0.Add(time.Duration(minutes) * time.Minute).UnixMilli() }
	at := func(minutes int) time.Time { return t0.Add(time.Duration(minutes) * time.Minute) }
	check := func(name string, obs []navObs, now int, wantPrice string, wantStable bool) {
		t.Helper()
		got, stable := settledNAV(obs, at(now), time.Hour)
		if stable != wantStable || (wantPrice != "" && got.Price != wantPrice) {
			t.Fatalf("%s at +%d min: got %s stable %v, want %s stable %v", name, now, got.Price, stable, wantPrice, wantStable)
		}
	}
	check("nothing read", nil, 0, "", false)

	one := []navObs{{Price: "49.33", ReceivedMs: ms(0), SeenMs: ms(55), Via: "api"}}
	check("one figure", one, 59, "49.33", false)
	check("one figure", one, 60, "49.33", true)

	// 49.33 from +0 to +30, then 49.35 from +35: its clock starts at +35.
	changed := []navObs{{Price: "49.33", ReceivedMs: ms(0), SeenMs: ms(30)}, {Price: "49.35", ReceivedMs: ms(35), SeenMs: ms(90)}}
	check("changed figure", changed, 90, "49.35", false)
	check("changed figure", changed, 95, "49.35", true)

	// 49.33 returns at +45 after 49.35 was read from +35 to +40: counted from +40.
	back := []navObs{{Price: "49.33", ReceivedMs: ms(0), SeenMs: ms(95)}, {Price: "49.35", ReceivedMs: ms(35), SeenMs: ms(40)}}
	check("returning figure", back, 99, "49.33", false)
	check("returning figure", back, 100, "49.33", true)

	// Confirmed at +60; the issuer corrects the figure at +185.
	corrected := []navObs{{Price: "49.33", ReceivedMs: ms(0), SeenMs: ms(180)}, {Price: "49.30", ReceivedMs: ms(185), SeenMs: ms(185)}}
	check("correction", corrected, 185, "49.30", false)
	check("correction", corrected, 244, "49.30", false)
	check("correction", corrected, 245, "49.30", true)
}

// The product page carries the NAV and its date in elements with fixed ids
// (markup as served on 2026-11-19).
func TestProSharesNAVOf(t *testing.T) {
	page := []byte(`<h5 class="about-fund__list-header">Price</h5>
        <span id="price-asOfDate" class="about-fund__list-subheader">as of 11/18/2026</span>
    </div>
    <li class="about-fund__list-item mb-3"><span class="about-fund__list-label d-inline-block">NAV</span> <div><span id="price-nav" class="about-fund__list-value d-inline-block">$52.49</span></div></li>
    <li class="about-fund__list-item mb-3"><span class="about-fund__list-label d-inline-block">NAV Change</span> <div><span id="price-navChange" class="about-fund__list-value d-inline-block">$+0.64</span><span class="about-fund__list-image"></span></div></li>
    <li class="about-fund__list-item mb-3"><span class="about-fund__list-label d-inline-block">Market Price</span> <div><span id="price-marketPrice" class="about-fund__list-value d-inline-block">$52.44</span></div></li>`)
	nav, err := proSharesNAVOf(page, "TQQQ")
	if err != nil {
		t.Fatal(err)
	}
	if want := (NAV{Issuer: "proshares", Ticker: "TQQQ", SessionDate: "2026-11-18", Price: "52.49", Via: "page"}); nav != want {
		t.Fatalf("nav: got %+v, want %+v", nav, want)
	}
	if _, err := proSharesNAVOf(bytes.ReplaceAll(page, []byte(`id="price-nav"`), []byte(`id="price-x"`)), "TQQQ"); err == nil {
		t.Fatal("a page without the NAV must be an error")
	}
}

// The historical NAV file gives four places; the anchor takes the cent, as
// the product page shows it.
func TestProSharesHistory(t *testing.T) {
	file := []byte("Date,ProShares Name,Ticker,NAV,Prior NAV,NAV Change (%),NAV Change ($),Shares Outstanding (000),Assets Under Management\n" +
		"11/18/2026,ProShares UltraPro Short QQQ,SQQQ,46.338,46.9226,-1.24588,-0.5846,37151.2,1721512444.614\n" +
		"11/17/2026,ProShares UltraPro Short QQQ,SQQQ,46.9226,45.9076,2.21096,1.015,38001.2,1783115247.8878\n")
	got, err := proSharesHistory(file)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["2026-11-18"] != "46.34" || got["2026-11-17"] != "46.92" {
		t.Fatalf("got %v", got)
	}
	if _, err := proSharesHistory([]byte("Day,Value\n11/18/2026,1\n")); err == nil {
		t.Fatal("a file without Date and NAV columns must be an error")
	}
}

// Responses of the site's query carry the ThirtyDayPricing object the site
// renders (values as answered on 2026-11-20).
func TestDirexionNAV(t *testing.T) {
	thirty := []byte(`{"data":{"getThirtyDayPrice":{"ItemKey":"SOXS-11182026","TradeDate":"11182026","Ticker":"SOXS","Nav":58.38,"MarketClosingPrice":58.4}}}`)
	nav, err := direxionNAV(thirty, "getThirtyDayPrice", "SOXS")
	if err != nil || nav == nil || *nav != (NAV{Issuer: "direxion", Ticker: "SOXS", SessionDate: "2026-11-18", Price: "58.38", Via: "api"}) {
		t.Fatalf("thirty-day: got %+v %v", nav, err)
	}
	if nav, err = direxionNAV([]byte(`{"data":{"getThirtyDayPrice":null}}`), "getThirtyDayPrice", "SOXS"); err != nil || nav != nil {
		t.Fatalf("a day the API does not have: got %+v %v, want none", nav, err)
	}
	if _, err = direxionNAV([]byte(`{"data":null,"errors":[{"errorType":"MappingTemplate","message":"bad"}]}`), "getThirtyDayPrice", "SOXL"); err == nil {
		t.Fatal("a GraphQL error must be an error")
	}
}

// The live read is the site's getThirtyDayPrice for the newest closed
// session, POSTed with the key in X-Api-Key, one request per fund: each NAV
// carries the time its answer arrived, a fund whose NAV of the session is
// not out yet (null, as SOXS 11-19 at 04:57 New York on 2026-11-20 while
// SOXL's was) is left out; a refused key is errKeyRejected and its text does
// not carry the key.
func TestDirexionLatestReadsThirtyDayPrice(t *testing.T) {
	const key = "test-key-not-a-real-one"
	status := http.StatusOK
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query     string            `json:"query"`
			Variables map[string]string `json:"variables"`
		}
		raw, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost || r.Header.Get("X-Api-Key") != key || r.Header.Get("Content-Type") != "application/json" ||
			json.Unmarshal(raw, &body) != nil || body.Query != direxionGetThirtyDayPrice {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		asked = append(asked, body.Variables["Ticker"]+"@"+body.Variables["TradeDate"])
		w.WriteHeader(status)
		if body.Variables["Ticker"] == "SOXS" {
			fmt.Fprint(w, `{"data":{"getThirtyDayPrice":null}}`)
			return
		}
		fmt.Fprintf(w, `{"data":{"getThirtyDayPrice":{"ItemKey":"SOXL-11192026","TradeDate":%q,"Ticker":"SOXL","Nav":45.32,"MarketClosingPrice":45.3}}}`,
			body.Variables["TradeDate"])
	}))
	defer srv.Close()
	d := Direxion{endpoint: srv.URL, key: key}
	before := time.Now().UnixMilli()
	navs, err := d.Latest(context.Background(), []string{"SOXL", "SOXS"}, "2026-11-19")
	after := time.Now().UnixMilli()
	if err != nil || len(navs) != 1 || navs[0].ReadMs < before || navs[0].ReadMs > after {
		t.Fatalf("got %+v %v", navs, err)
	}
	navs[0].ReadMs = 0
	if navs[0] != (NAV{Issuer: "direxion", Ticker: "SOXL", SessionDate: "2026-11-19", Price: "45.32", Via: "api"}) {
		t.Fatalf("got %+v", navs[0])
	}
	if want := []string{"SOXL@11192026", "SOXS@11192026"}; !slices.Equal(asked, want) {
		t.Fatalf("asked %v, want %v", asked, want)
	}
	status = http.StatusUnauthorized
	_, err = d.Latest(context.Background(), []string{"SOXL"}, "2026-11-19")
	if !errors.Is(err, errKeyRejected) || strings.Contains(err.Error(), key) {
		t.Fatalf("refused key: got %v", err)
	}
}

// Without a key Direxion is not asked at all; a refused key is logged at
// Error once and the source is dropped for the rest of the run.
func TestDirexionKeyMissingOrRejected(t *testing.T) {
	if _, ok := navSources("", Calendar{})["direxion"]; ok {
		t.Fatal("no key: Direxion must not be a source")
	}
	if _, ok := navSources("", Calendar{})["proshares"]; !ok {
		t.Fatal("ProShares needs no key")
	}
	var logged bytes.Buffer
	m := &Manager{log: slog.New(slog.NewTextHandler(&logged, nil)), navs: navSources("a-key", Calendar{})}
	if m.keyRejected("direxion", errors.New("timeout")) {
		t.Fatal("another error is not a refused key")
	}
	if !m.keyRejected("direxion", fmt.Errorf("http 401: %w", errKeyRejected)) {
		t.Fatal("a refused key must be reported")
	}
	if _, ok := m.navs["direxion"]; ok || strings.Count(logged.String(), "level=ERROR") != 1 || strings.Contains(logged.String(), "a-key") {
		t.Fatalf("after a refusal: sources %v, log %q", m.navs, logged.String())
	}
}

// A request cancelled by the shutdown is not logged as a failing read; a
// real failure is logged once and its recovery once.
func TestNoteIgnoresTheShutdown(t *testing.T) {
	var logged bytes.Buffer
	m := &Manager{log: slog.New(slog.NewTextHandler(&logged, nil)), failing: map[string]bool{}}
	m.note("candles binance SOXLUSDT", &url.Error{Op: "Get", URL: "https://fapi.binance.com", Err: context.Canceled})
	if logged.Len() != 0 || len(m.failing) != 0 {
		t.Fatalf("a cancelled request was noted: %q", logged.String())
	}
	m.note("candles binance SOXLUSDT", errors.New("http 502"))
	m.note("candles binance SOXLUSDT", errors.New("http 502"))
	m.note("candles binance SOXLUSDT", nil)
	if strings.Count(logged.String(), "level=WARN") != 1 || strings.Count(logged.String(), "read recovered") != 1 {
		t.Fatalf("log %q", logged.String())
	}
}

func validConfig() Config {
	return Config{DSNEnv: "X", HistoryStart: time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC), FXPoll: time.Second,
		AnchorPoll: time.Minute, NAVSettleDelay: time.Hour,
		Combos: []Combo{{ID: "A", BullTicker: "TQQQ", BearTicker: "SQQQ", RefTicker: "QQQ", NAVSource: "proshares",
			Markets: []Market{{Venue: "bitget", BullSymbol: "TQQQUSDT", BearSymbol: "SQQQUSDT", RefSymbol: "QQQUSDT"},
				{Venue: "binance", BullSymbol: "TQQQUSDT", BearSymbol: "SQQQUSDT", RefSymbol: "QQQUSDT"}}}}}
}

// The settings are checked at start: a combo needs a Binance market for its
// bell anchors, a known NAV source and the NAV settle delay; an action entry
// a ticker, an ex-date, a kind, a source and a positive value or none (then
// Scheduled, or None); an override a date, closed or HH:MM, and a source.
func TestValidate(t *testing.T) {
	ok := validConfig()
	ok.CorporateActions = []ActionEntry{{Ticker: "SOXS", ExDate: "2026-09-22", Kind: "dividend", Value: "0.43641", Source: "x"},
		{Ticker: "SOXL", ExDate: "2026-12-22", Kind: "dividend", Source: "x"}, {Ticker: "SOXL", ExDate: "2027-03-23", Kind: "dividend", Status: "None", Source: "x"}}
	ok.CalendarOverrides = []CalendarOverride{{Date: "2027-01-08", Close: "closed", Source: "x"}, {Date: "2026-11-27", Close: "12:30", Source: "x"},
		{Date: "2026-11-18", Close: "16:00", Source: "x"}, {Date: "2026-11-19", Close: "09:31", Source: "x"}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	if a, _ := ok.CorporateActions[1].action(); a.Status != "Scheduled" || a.Source != "config" || a.URL != "x" {
		t.Fatalf("an entry without a value: %+v", a)
	}
	for name, edit := range map[string]func(*Config){
		"action without source": func(c *Config) {
			c.CorporateActions = []ActionEntry{{Ticker: "SOXL", ExDate: "2026-12-22", Kind: "dividend"}}
		},
		"action of an unknown kind": func(c *Config) {
			c.CorporateActions = []ActionEntry{{Ticker: "SOXL", ExDate: "2026-12-22", Kind: "merger", Source: "x"}}
		},
		"action with a bad value": func(c *Config) {
			c.CorporateActions = []ActionEntry{{Ticker: "SOXL", ExDate: "2026-12-22", Kind: "dividend", Value: "-1", Source: "x"}}
		},
		"None with a value": func(c *Config) {
			c.CorporateActions = []ActionEntry{{Ticker: "SOXL", ExDate: "2026-12-22", Kind: "dividend", Value: "1", Status: "None", Source: "x"}}
		},
		"override at 25:00": func(c *Config) {
			c.CalendarOverrides = []CalendarOverride{{Date: "2026-11-27", Close: "25:00", Source: "x"}}
		},
		// After-Hours of a 16:30 close would run into the Overnight from 20:00, a 09:00 close end before the open.
		"override at 16:30": func(c *Config) {
			c.CalendarOverrides = []CalendarOverride{{Date: "2026-11-18", Close: "16:30", Source: "x"}}
		},
		"override at 09:00": func(c *Config) {
			c.CalendarOverrides = []CalendarOverride{{Date: "2026-11-18", Close: "09:00", Source: "x"}}
		},
		"close on a Saturday": func(c *Config) {
			c.CalendarOverrides = []CalendarOverride{{Date: "2026-11-21", Close: "13:00", Source: "x"}}
		},
		"close on a holiday": func(c *Config) {
			c.CalendarOverrides = []CalendarOverride{{Date: "2026-11-26", Close: "13:00", Source: "x"}}
		},
		"override entered twice": func(c *Config) {
			c.CalendarOverrides = []CalendarOverride{{Date: "2027-01-08", Close: "closed", Source: "x"}, {Date: "2027-01-08", Close: "13:00", Source: "x"}}
		},
		"action entered twice": func(c *Config) {
			c.CorporateActions = []ActionEntry{{Ticker: "SOXL", ExDate: "2026-12-22", Kind: "dividend", Source: "x"},
				{Ticker: "SOXL", ExDate: "2026-12-22", Kind: "dividend", Value: "0.1", Source: "y"}}
		},
		"override of no date": func(c *Config) {
			c.CalendarOverrides = []CalendarOverride{{Date: "11/27/2026", Close: "closed", Source: "x"}}
		},
		"override, no source": func(c *Config) { c.CalendarOverrides = []CalendarOverride{{Date: "2026-11-27", Close: "closed"}} },
		"no nav_settle_delay": func(c *Config) { c.NAVSettleDelay = 0 },
		"unknown nav_source":  func(c *Config) { c.Combos[0].NAVSource = "direxon" },
		"no binance market":   func(c *Config) { c.Combos[0].Markets = c.Combos[0].Markets[:1] },
		"no history_start":    func(c *Config) { c.HistoryStart = time.Time{} },
		"no anchor_poll":      func(c *Config) { c.AnchorPoll = 0 },
	} {
		c := validConfig()
		edit(&c)
		if c.Validate() == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

// Every ETF of a combo, the reference too, takes its bell anchor from its
// contract on Binance, or on Bitget before Binance lists it; a combo without
// a reference has none. The first session is history_start's day.
func TestBellSymbols(t *testing.T) {
	c := validConfig()
	c.Combos = append(c.Combos, Combo{ID: "B", BullTicker: "SOXL", BearTicker: "SOXS",
		Markets: []Market{{Venue: "binance", BullSymbol: "SOXLUSDT", BearSymbol: "SOXSUSDT"}}})
	want := "map[QQQ:map[binance:QQQUSDT bitget:QQQUSDT] SOXL:map[binance:SOXLUSDT] SOXS:map[binance:SOXSUSDT] " +
		"SQQQ:map[binance:SQQQUSDT bitget:SQQQUSDT] TQQQ:map[binance:TQQQUSDT bitget:TQQQUSDT]]"
	if got := c.bellSymbols(); fmt.Sprint(got) != want {
		t.Fatalf("got %v", got)
	}
	if c.historyDay() != "2026-07-16" {
		t.Fatalf("history day %s", c.historyDay())
	}
}

// fakeIndex serves index klines from a table and records what was asked.
type fakeIndex struct {
	klines map[string]map[int64]string // symbol -> open time -> close
	asked  []string
}

func (f *fakeIndex) IndexKline(_ context.Context, symbol string, minuteMs int64) (*Candle, error) {
	f.asked = append(f.asked, fmt.Sprintf("%s@%d", symbol, minuteMs))
	var opens []int64
	for open := range f.klines[symbol] {
		if open >= minuteMs {
			opens = append(opens, open)
		}
	}
	if len(opens) == 0 {
		return nil, nil
	}
	sort.Slice(opens, func(i, j int) bool { return opens[i] < opens[j] })
	return &Candle{MinuteMs: opens[0], Close: f.klines[symbol][opens[0]]}, nil
}

// fakeNAVs answers Latest with the figures it holds.
type fakeNAVs struct{ latest []NAV }

func (f *fakeNAVs) Latest(context.Context, []string, string) ([]NAV, error) {
	out := slices.Clone(f.latest)
	for i := range out {
		out[i].ReadMs = time.Now().UnixMilli()
	}
	return out, nil
}

func (f *fakeNAVs) Past(context.Context, string, []string) ([]NAV, error) { return nil, nil }

// scratchStore opens a database of its own on the server ETFPAIR_TEST_DSN
// names (a postgres:// URL) with the schema applied, the recorder's
// (perp_second, which combo_range reads) first, and drops it at the end;
// without the variable the test is skipped.
func scratchStore(t *testing.T) *store {
	base := os.Getenv("ETFPAIR_TEST_DSN")
	if base == "" {
		t.Skip("ETFPAIR_TEST_DSN is not set")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("scan_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	t.Cleanup(func() {
		admin.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)")
		admin.Close(ctx)
	})
	recorder, err := os.ReadFile("../booktape/schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	db, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(ctx, string(recorder), pgx.QueryExecModeSimpleProtocol)
	db.Close(ctx)
	if err != nil {
		t.Fatal(err)
	}
	st, err := openStore(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.pool.Close)
	return st
}

// The anchors on a scratch database: the bell anchors (the minute before
// each close, 16:00 and 13:00, USD per USDT of that minute, a missing
// kline, a contract Binance lists later: Bitget's index before it, none
// without a Bitget contract; the time a row became known), then the
// precedence of combo_sessions (both legs' confirmed NAVs over both bells,
// over nothing), the NAV reads and their confirmation, the history load and
// the calendar from history_start.
func TestAnchorsOnDatabase(t *testing.T) {
	st := scratchStore(t)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := st.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	query := func(sql string, args ...any) []string {
		t.Helper()
		rows, err := st.pool.Query(ctx, sql, args...)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return lines
	}
	expect := func(name string, got []string, want ...string) {
		t.Helper()
		if !slices.Equal(got, want) {
			t.Fatalf("%s:\ngot\n%s\nwant\n%s", name, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}
	testStart := time.Now().UnixMilli()
	var logged bytes.Buffer
	cfg := validConfig()
	cfg.HistoryStart = time.Date(2001, 1, 2, 0, 0, 0, 0, time.UTC)
	cfg.NAVSettleDelay = time.Second
	cfg.Combos = []Combo{{ID: "T", BullTicker: "BULL", BearTicker: "BEAR", RefTicker: "REF", NAVSource: "direxion",
		Markets: []Market{{Venue: "binance", BullSymbol: "BULLUSDT", BearSymbol: "BEARUSDT", RefSymbol: "REFUSDT"}}}}
	if err := st.seed(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	idx, bg := &fakeIndex{klines: map[string]map[int64]string{}}, &fakeIndex{klines: map[string]map[int64]string{}}
	m := &Manager{cfg: cfg, store: st, index: map[string]IndexSource{"binance": idx, "bitget": bg}, log: slog.New(slog.NewTextHandler(&logged, nil)),
		failing: map[string]bool{},
		navs:    map[string]NAVSource{}, navRead: map[string]time.Time{}, indexFrom: map[string]int64{}, told: map[string]bool{}}
	// The collector started between the fourth session's close and the fifth's.
	m.started = time.Date(2001, 6, 1, 0, 0, 0, 0, time.UTC)

	// Five sessions: four 16:00 closes and a 13:00 early close.
	closeAt := map[string]int64{}
	for date, hourUTC := range map[string]int{"2001-01-02": 21, "2001-01-03": 21, "2001-01-04": 21, "2001-01-05": 21, "2001-11-23": 18} {
		day, _ := time.Parse("2006-01-02", date)
		closeAt[date] = day.Add(time.Duration(hourUTC) * time.Hour).UnixMilli()
		exec(`INSERT INTO etfpair.etf_session (session_date, close_ms) VALUES ($1::date, $2)`, date, closeAt[date])
	}
	bell := func(date string) int64 { return closeAt[date] - minuteMs } // the minute before the close
	fx := func(minute int64, price string) {
		exec(`INSERT INTO etfpair.fx_minute (pair, minute_ms, price, source, sampled_ms) VALUES ('USDT-USD', $1, $2::numeric, 'candle', 0)`, minute, price)
	}
	fx(bell("2001-01-02")-minuteMs, "0.9980") // the minute before is not the bell's
	fx(bell("2001-01-02"), "0.9990")
	fx(bell("2001-01-03"), "1.0001")
	fx(bell("2001-01-04"), "0.9995")
	fx(bell("2001-01-05")-6*minuteMs, "0.9999") // too old: 01-05 has no USD per USDT and waits for it
	fx(bell("2001-11-23")-3*minuteMs, "0.9985") // the newest of the five minutes before
	idx.klines["BULLUSDT"] = map[int64]string{}
	idx.klines["BEARUSDT"] = map[int64]string{}
	for i, date := range []string{"2001-01-02", "2001-01-03", "2001-01-04", "2001-01-05", "2001-11-23"} {
		idx.klines["BULLUSDT"][bell(date)] = fmt.Sprintf("10.0%d", i+1)
		idx.klines["BEARUSDT"][bell(date)] = fmt.Sprintf("20.0%d", i+1)
	}
	delete(idx.klines["BEARUSDT"], bell("2001-01-04")) // a missing kline: the next one exists
	idx.klines["BEARUSDT"][closeAt["2001-01-04"]] = "20.99"
	// Binance's index of the reference starts after 01-03. Bitget has the legs' indices too (never asked for: Binance
	// lists them) but no reference contract yet: the sessions before 01-04 have no bell and are not asked for.
	idx.klines["REFUSDT"] = map[int64]string{bell("2001-01-04"): "30.03", bell("2001-11-23"): "30.05"}
	bg.klines["BULLUSDT"] = map[int64]string{bell("2001-01-04"): "99"}
	bg.klines["BEARUSDT"] = map[int64]string{bell("2001-01-04"): "99"}
	bg.klines["REFUSDT"] = map[int64]string{bell("2001-01-02"): "29.01", bell("2001-01-04"): "99"}

	m.loadBells(ctx)
	bells := `SELECT b.ticker || ' ' || b.session_date || ' ' || b.venue || ' ' || b.price || ' = ' || b.fx || ' x ' || b.index_usdt || ' kline close-' ||
		(s.close_ms - b.kline_ms) / 1000 || 's known ' ||
		CASE WHEN b.known_ms = s.close_ms + 120000 THEN 'close+2min' WHEN b.known_ms >= $1 THEN 'at store' ELSE b.known_ms::text END
		FROM etfpair.etf_bell b JOIN etfpair.etf_session s USING (session_date) ORDER BY b.session_date, b.ticker`
	binanceBells := []string{
		"BEAR 2001-01-02 binance 19.98999 = 0.999 x 20.01 kline close-60s known close+2min",
		"BULL 2001-01-02 binance 9.99999 = 0.999 x 10.01 kline close-60s known close+2min",
		"BEAR 2001-01-03 binance 20.022002 = 1.0001 x 20.02 kline close-60s known close+2min",
		"BULL 2001-01-03 binance 10.021002 = 1.0001 x 10.02 kline close-60s known close+2min",
		"BULL 2001-01-04 binance 10.024985 = 0.9995 x 10.03 kline close-60s known close+2min", // BEAR's kline is missing
		"REF 2001-01-04 binance 30.014985 = 0.9995 x 30.03 kline close-60s known close+2min",
		"BEAR 2001-11-23 binance 20.019925 = 0.9985 x 20.05 kline close-60s known at store", // closed after the collector started
		"BULL 2001-11-23 binance 10.034925 = 0.9985 x 10.05 kline close-60s known at store",
		"REF 2001-11-23 binance 30.004925 = 0.9985 x 30.05 kline close-60s known at store"}
	expect("bell anchors", query(bells, testStart), binanceBells...)
	at := func(minute int64) string { return fmt.Sprintf("@%d", minute) }
	sort.Strings(idx.asked)
	var wantAsked []string
	for _, symbol := range []string{"BEARUSDT", "BULLUSDT"} {
		wantAsked = append(wantAsked, symbol+at(cfg.HistoryStart.UnixMilli())) // where its index starts
		for _, date := range []string{"2001-01-02", "2001-01-03", "2001-01-04", "2001-11-23"} {
			wantAsked = append(wantAsked, symbol+at(bell(date)))
		}
	}
	wantAsked = append(wantAsked, "REFUSDT"+at(cfg.HistoryStart.UnixMilli()), "REFUSDT"+at(bell("2001-01-04")), "REFUSDT"+at(bell("2001-11-23")))
	sort.Strings(wantAsked)
	expect("index requests", idx.asked, wantAsked...)
	expect("Bitget index requests", bg.asked)

	// Again: the missing kline is asked for again but logged once; nothing else is asked.
	idx.asked = nil
	m.loadBells(ctx)
	expect("index requests of the second round", idx.asked, "BEARUSDT"+at(bell("2001-01-04")))
	if n := strings.Count(logged.String(), "no index kline at the close"); n != 1 {
		t.Fatalf("missing kline logged %d times:\n%s", n, logged.String())
	}
	if n := strings.Count(logged.String(), "sessions before Binance lists the contract have no bell anchor"); n != 1 || !strings.Contains(logged.String(), "sessions=2") {
		t.Fatalf("sessions before the reference's Binance index logged %d times:\n%s", n, logged.String())
	}
	// 01-05 without USD per USDT before its close: logged once per ETF, not skipped silently.
	if n := strings.Count(logged.String(), "no USD per USDT in the five minutes before the close"); n != 3 || !strings.Contains(logged.String(), "ticker=BULL session=2001-01-05") {
		t.Fatalf("a session without USD per USDT logged %d times:\n%s", n, logged.String())
	}

	// Bitget lists the reference: the sessions before Binance's first index kline take Bitget's index of
	// the bell minute, the venue on record. 01-03's Bitget kline is missing: no bell, logged once.
	m.cfg.Combos[0].Markets = append(m.cfg.Combos[0].Markets, Market{Venue: "bitget", BullSymbol: "BULLUSDT", BearSymbol: "BEARUSDT", RefSymbol: "REFUSDT"})
	idx.asked = nil
	m.loadBells(ctx)
	m.loadBells(ctx)
	expect("Binance index requests with Bitget listed", idx.asked, "BEARUSDT"+at(bell("2001-01-04")), "BEARUSDT"+at(bell("2001-01-04")))
	sort.Strings(bg.asked)
	expect("Bitget index requests", bg.asked, "REFUSDT"+at(bell("2001-01-02")), "REFUSDT"+at(bell("2001-01-03")), "REFUSDT"+at(bell("2001-01-03")))
	expect("bell anchors with Bitget listed", query(bells, testStart), slices.Insert(binanceBells, 2,
		"REF 2001-01-02 bitget 28.98099 = 0.999 x 29.01 kline close-60s known close+2min")...)
	if n := strings.Count(logged.String(), "no index kline at the close"); n != 2 || !strings.Contains(logged.String(), "venue=bitget symbol=REFUSDT session=2001-01-03") {
		t.Fatalf("missing Bitget kline logged %d times:\n%s", n, logged.String())
	}

	// 2001-01-02: the issuer's NAVs, read twice a settle delay apart, are confirmed at the second read.
	src := &fakeNAVs{latest: []NAV{{Issuer: "direxion", Ticker: "BULL", SessionDate: "2001-01-02", Price: "10.00", Via: "api"},
		{Issuer: "direxion", Ticker: "BEAR", SessionDate: "2001-01-02", Price: "20.00", Via: "api"}}}
	firstRead := time.Now().UnixMilli()
	m.readNAVs(ctx, cfg.Combos[0], src, "2001-01-03")
	if ok, _ := st.hasNAV(ctx, "BULL", "BEAR", "2001-01-02"); ok {
		t.Fatal("a NAV read once is not stable yet")
	}
	time.Sleep(1100 * time.Millisecond)
	secondRead := time.Now().UnixMilli()
	m.readNAVs(ctx, cfg.Combos[0], src, "2001-01-03")
	var received, confirmed int64
	if err := st.pool.QueryRow(ctx, `SELECT o.received_ms, n.confirmed_ms FROM etfpair.etf_nav_obs o JOIN etfpair.etf_nav n USING (ticker, session_date, nav)
		WHERE o.ticker = 'BEAR' AND o.session_date = '2001-01-02'`).Scan(&received, &confirmed); err != nil {
		t.Fatal(err)
	}
	if received < firstRead || received >= secondRead || confirmed < secondRead {
		t.Fatalf("first read %d, second %d: observation received at %d, confirmed at %d", firstRead, secondRead, received, confirmed)
	}
	if n := strings.Count(logged.String(), "the issuer shows no NAV of the session yet"); n != 1 {
		t.Fatalf("the issuer's older day logged %d times:\n%s", n, logged.String())
	}

	nav := func(day, ticker, price string, readMs int64) NAV {
		n := NAV{Issuer: "direxion", Ticker: ticker, SessionDate: day, Price: price, Via: "api", ReadMs: readMs}
		if err := st.insertNAVObs(ctx, []NAV{n}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	confirm := func(n NAV, nowMs int64, wantChanged bool) {
		t.Helper()
		if changed, err := st.confirmNAV(ctx, n, nowMs); err != nil || changed != wantChanged {
			t.Fatalf("confirm %+v at %d: changed %v %v, want %v", n, nowMs, changed, err, wantChanged)
		}
	}
	// 2001-01-03: one leg's NAV only. 2001-11-23: 10.09 / 19.91 confirmed at 5000, the bull leg
	// corrected to 10.10 at 9000, then back to 10.09 at 12000; the issuer's current bull figure is
	// 10.11, not yet confirmed.
	confirm(nav("2001-01-03", "BULL", "10.02", 1000), 5000, true)
	first := nav("2001-11-23", "BULL", "10.09", 1000)
	confirm(first, 5000, true)
	confirm(nav("2001-11-23", "BEAR", "19.91", 1000), 5000, true)
	confirm(first, 6000, false) // already in use
	confirm(nav("2001-11-23", "BULL", "10.10", 8000), 9000, true)
	confirm(first, 12000, true)
	nav("2001-11-23", "BULL", "10.11", 13000)

	sessions := `SELECT session_date || ' ' || coalesce(a_bull::text, '') || '/' || coalesce(a_bear::text, '') || ' ref ' || coalesce(a_ref::text, '') ||
		' ' || coalesce(basis, '') || ' bell ' || coalesce(bell_bull::text, '') || '/' || coalesce(bell_bear::text, '') ||
		' nav ' || coalesce(nav_bull::text, '') || '/' || coalesce(nav_bear::text, '') || ' known ' ||
		CASE WHEN known_ms = close_ms + 120000 THEN 'close+2min' WHEN known_ms >= $1 THEN 'at confirmation' ELSE coalesce(known_ms::text, '') END ||
		' ' || status FROM etfpair.combo_sessions('T') ORDER BY session_date`
	expect("combo_sessions", query(sessions, testStart),
		"2001-01-02 10.00/20.00 ref 28.98099 NAV bell 9.99999/19.98999 nav 10.00/20.00 known at confirmation NAV", // both NAVs over both bells
		"2001-01-03 10.021002/20.022002 ref  Bell bell 10.021002/20.022002 nav 10.02/ known close+2min Bell",      // one leg's NAV changes nothing
		"2001-01-04 / ref 30.014985  bell 10.024985/ nav / known  Missing",                                        // one leg without a bell
		"2001-01-05 / ref   bell / nav / known  Missing",                                                          // nothing yet
		"2001-11-23 10.09/19.91 ref 30.004925 NAV bell 10.034925/20.019925 nav 10.11/19.91 known 12000 NAV")       // the row confirmed last is in use

	// The history load asks for the sessions without a NAV of the ticker that closed before the
	// newest, the newest first, and confirms what it loads; a session that has one keeps it.
	due, err := st.sessionsWithoutNAV(ctx, "BEAR", closeAt["2001-11-23"])
	expect("sessions without a BEAR NAV", due, "2001-01-05", "2001-01-04", "2001-01-03")
	if err != nil {
		t.Fatal(err)
	}
	past := []NAV{{Issuer: "direxion", Ticker: "BEAR", SessionDate: "2001-01-03", Price: "20.03", Via: "api", ReadMs: 20000},
		{Issuer: "direxion", Ticker: "BEAR", SessionDate: "2001-01-02", Price: "19.00", Via: "api", ReadMs: 20000}}
	if err := st.insertPastNAVs(ctx, past, 21000); err != nil {
		t.Fatal(err)
	}
	expect("2001-01-03 after the history load", query(`SELECT status || ' ' || a_bull || '/' || a_bear || ' known ' || known_ms
		FROM etfpair.combo_sessions('T') WHERE session_date = '2001-01-03'`), "NAV 10.02/20.03 known 21000")
	expect("a session with a NAV keeps its one row", query(`SELECT nav::text FROM etfpair.etf_nav WHERE ticker = 'BEAR' AND session_date = '2001-01-02'`), "20.00")

	// The candle backfill resumes after the first unbroken run of a series, also for a contract
	// listed after the start, and starts at the start only for a series it has none of.
	start := cfg.HistoryStart.UnixMilli()
	minutes := func(symbol string, from, to int64) {
		exec(`INSERT INTO etfpair.perp_minute (venue, symbol, minute_ms, price, sampled_ms)
			SELECT 'v', $1, m, 1, 0 FROM generate_series($2::bigint, $3::bigint, 60000) m`, symbol, from, to)
	}
	minutes("LATE", start+600*minuteMs, start+699*minuteMs)
	minutes("HOLE", start, start+50*minuteMs)
	minutes("HOLE", start+60*minuteMs, start+100*minuteMs)
	for symbol, want := range map[string]int64{"LATE": start + 684*minuteMs, "HOLE": start + 35*minuteMs, "NONE": start} {
		if got := st.historyFrom(ctx, "perp_minute", "venue = $2 AND symbol = $3 AND minute_ms >= $4", true, start, "v", symbol); got != want {
			t.Fatalf("%s: backfill from %d min, want %d min", symbol, (got-start)/minuteMs, (want-start)/minuteMs)
		}
	}

	// The calendar holds the sessions from history_start only: a minute before its first close has
	// no session and no spread.
	m.cfg.HistoryStart = time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	m.writeCalendar(ctx)
	expect("first session and phase", query(`SELECT (SELECT min(session_date)::text FROM etfpair.etf_session) || ' ' ||
		(SELECT to_char(to_timestamp(min(start_ms) / 1000) AT TIME ZONE 'America/New_York', 'YYYY-MM-DD HH24:MI') FROM etfpair.etf_phase)`),
		"2026-11-16 2026-11-15 20:00")
	first16 := time.Date(2026, 11, 16, 21, 0, 0, 0, time.UTC).UnixMilli()
	for _, minute := range []int64{first16 - 2*minuteMs, first16 + minuteMs} {
		minutes("BULLUSDT", minute, minute)
		exec(`UPDATE etfpair.perp_minute SET venue = 'binance' WHERE venue = 'v' AND symbol = 'BULLUSDT'`)
		minutes("BEARUSDT", minute, minute)
		exec(`UPDATE etfpair.perp_minute SET venue = 'binance' WHERE venue = 'v' AND symbol = 'BEARUSDT'`)
		fx(minute, "1")
	}
	exec(`INSERT INTO etfpair.etf_bell VALUES ('BULL', '2026-11-16', 1, 1, 'binance', 1, $1, $1), ('BEAR', '2026-11-16', 1, 1, 'binance', 1, $1, $1)`, first16-minuteMs)
	// Around the first close of the calendar: a recorder row in the middle of each of the two minutes with its candle close; the
	// one before the first close has no session and no spread (board 3.1's combo_series gave the same for those minutes).
	for _, minute := range []int64{first16 - 2*minuteMs, first16 + minuteMs} {
		exec(`INSERT INTO etfpair.perp_second (venue, symbol, second_ms, price, trade_ms) SELECT venue, symbol, minute_ms + 30000, price, minute_ms + 29900
			FROM etfpair.perp_minute WHERE venue = 'binance' AND symbol IN ('BULLUSDT', 'BEARUSDT') AND minute_ms = $1`, minute)
	}
	expect("points around the first close", query(`SELECT ts_ms - $1 || ' ' || coalesce(session_date::text, 'no session') || ' ' ||
		coalesce(round(s_bps, 2)::text, 'no spread') FROM etfpair.combo_range('T', 'binance', 'sma', 1, $2, $3, 1000, NULL, NULL)
		WHERE basis IS NOT NULL ORDER BY ts_ms`, first16, first16-10*minuteMs, first16+10*minuteMs),
		"-90000 no session no spread", "90000 2026-11-16 0.00")
}

// The drawn points around a dividend's T at every step (combo_range, board 3.2; this test checked board 3.1's combo_points
// until it was dropped, OWNER.md item 24): the recorder's rows at the multiples of the step, on both legs' mids where both
// have a usable book and else on both legs' trades, with the real spreads of a notional the best levels fill only where both
// books are usable (an empty side is no book); a row at T holds the trade before T and is on the anchor before it (A 100),
// one after T on A - D (99) with the mean restarting; a row with books but no trade yet is drawn on the mids; no point stands
// for a minute's candle close (3.1 drew one); a gap breaks the line with a row 1 ms after the point before it (+1), none
// before the first point. combo_prices gives the same seconds' trade prices and anchors.
func TestPerSecondPointsOnDatabase(t *testing.T) {
	st := scratchStore(t)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := st.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	cfg := validConfig()
	cfg.Combos = []Combo{{ID: "P", BullTicker: "BULL", BearTicker: "BEAR", RefTicker: "REF",
		Markets: []Market{{Venue: "binance", BullSymbol: "BULLUSDT", BearSymbol: "BEARUSDT", RefSymbol: "REFUSDT"}}}}
	if err := st.seed(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	var sessions []session
	for _, date := range []string{"2026-11-19", "2026-11-20"} {
		closeAt, _ := Calendar{}.closeOf(date)
		sessions = append(sessions, session{date, closeAt.UnixMilli()})
	}
	if err := st.writeCalendar(ctx, sessions, nil); err != nil {
		t.Fatal(err)
	}
	for ticker, price := range map[string]string{"BULL": "100", "BEAR": "50", "REF": "300"} {
		exec(`INSERT INTO etfpair.etf_bell VALUES ($1, '2026-11-19', $2::numeric, $2::numeric, 'binance', 1, $3, 1000)`, ticker, price, sessions[0].CloseMs-minuteMs)
	}
	// BULL goes ex 1 at T, 20:00 New York of 11-19: its anchor is 99 from T.
	exec(`INSERT INTO etfpair.etf_action VALUES ('BULL', '2026-11-20', 'dividend', 1, 'Known', 'test', 'u', 1)`)
	T := nyTime(t, "2026-11-19 20:00:00").UnixMilli()
	for i, bull := range []string{"100", "100.05", "100.1", "99", "99.1", "99.2"} {
		at := T + int64(i-3)*minuteMs
		exec(`INSERT INTO etfpair.fx_minute (pair, minute_ms, price, source, sampled_ms) VALUES ('USDT-USD', $1, 1, 'candle', 0)`, at)
		for symbol, price := range map[string]string{"BULLUSDT": bull, "BEARUSDT": "50", "REFUSDT": "300"} {
			exec(`INSERT INTO etfpair.perp_minute VALUES ('binance', $1, $2, $3::numeric, 0)`, symbol, at, price)
		}
	}
	// Rows of the recorder: price, best bids, best asks, 1000 contracts at each level; an array NULL is no book, '{}' an empty side.
	second := func(symbol string, at int64, price, bids, asks any) {
		exec(`INSERT INTO etfpair.perp_second (venue, symbol, second_ms, price, bid_px, bid_sz, ask_px, ask_sz) VALUES ('binance', $1, $2, $3::numeric,
			$4::numeric[], CASE WHEN $4::numeric[] IS NOT NULL THEN array_fill(1000::numeric, ARRAY[cardinality($4::numeric[])]) END,
			$5::numeric[], CASE WHEN $5::numeric[] IS NOT NULL THEN array_fill(1000::numeric, ARRAY[cardinality($5::numeric[])]) END)`,
			symbol, at, price, bids, asks)
	}
	second("BULLUSDT", T-59_000, "100.1", "{100.09,100.08}", "{100.11}")
	second("BEARUSDT", T-59_000, "50", "{49.99}", "{50.02}")
	second("REFUSDT", T-59_000, "300", "{299.9}", "{300.1}")
	second("BULLUSDT", T, "100.2", "{100.19}", "{100.21}")
	second("BEARUSDT", T, "50.1", nil, nil)
	second("BULLUSDT", T+1_000, "99.3", "{}", "{99.31}")
	second("BEARUSDT", T+1_000, "50", "{49.98}", "{50.01}")
	second("BULLUSDT", T+2_000, nil, "{99.29}", "{99.31}") // a book before any trade: no point
	second("BEARUSDT", T+2_000, "50", "{49.98}", "{50.01}")
	second("BULLUSDT", T+5_000, "99", "{98.99}", "{99.01}")
	second("BEARUSDT", T+5_000, "50", "{49.99}", "{50.01}")
	lines := func(sql string, step int64) []string {
		t.Helper()
		rows, err := st.pool.Query(ctx, sql, T-2*minuteMs, T+2*minuteMs, step)
		if err != nil {
			t.Fatal(err)
		}
		out, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	points := `SELECT CASE WHEN basis IS NULL THEN 'break +' || (ts_ms - lag(ts_ms) OVER (ORDER BY ts_ms)) ELSE
		to_char(to_timestamp(ts_ms / 1000.0) AT TIME ZONE 'America/New_York', 'HH24:MI:SS') || ' ' || basis || ' s ' || round(s_bps, 1) ||
		' mu ' || coalesce(round(mu_bps, 1)::text, '-') || ' long ' || coalesce(round(s_long_bps, 1)::text, '-') || ' short ' ||
		coalesce(round(s_short_bps, 1)::text, '-') || ' books ' || book_bull || '/' || book_bear || ' ' || coalesce(signal, '-') END
		FROM etfpair.combo_range('P', 'binance', 'sma', 1, $1, $2, $3, 1000, 10) ORDER BY ts_ms`
	prices := `SELECT CASE WHEN p_bull IS NULL THEN 'break' ELSE to_char(to_timestamp(ts_ms / 1000.0) AT TIME ZONE 'America/New_York', 'HH24:MI:SS') ||
		' ' || trim_scale(p_bull) || ' ' || trim_scale(p_bear) || ' ref ' || coalesce(trim_scale(p_ref)::text, '-') || ' a ' || trim_scale(a_bull) END
		FROM etfpair.combo_prices('P', 'binance', $1, $2, $3) ORDER BY ts_ms`
	want := map[int64][2][]string{
		1000: {{ // s = 10^4 (p_bull / A_bull + p_bear / A_bear - 2); long on the asks, short on the bids; mu: the minute before's close
			"19:59:01 mid s 11.0 mu 5.0 long 15.0 short 7.0 books true/true None", // mids 100.1, 50.005
			"break +1",
			"20:00:00 trade s 40.0 mu 5.0 long - short - books true/false No Book",  // the trade before T, on A; BEAR without a book
			"20:00:01 trade s 30.3 mu - long - short - books false/true No Mean",    // on A - D, the mean restarts; BULL's bids empty
			"20:00:02 mid s 29.3 mu - long 33.3 short 25.3 books true/true No Mean", // BULL's book before its next trade
			"break +1",
			"20:00:05 mid s 0.0 mu - long 3.0 short -3.0 books true/true No Mean"}, {
			"19:59:01 100.1 50 ref 300 a 100", "break", "20:00:00 100.2 50.1 ref - a 100", "20:00:01 99.3 50 ref - a 99", "break", "20:00:05 99 50 ref - a 99"}},
		5000: {{
			"20:00:00 trade s 40.0 mu 5.0 long - short - books true/false No Book",
			"20:00:05 mid s 0.0 mu - long 3.0 short -3.0 books true/true No Mean"}, {
			"20:00:00 100.2 50.1 ref - a 100", "20:00:05 99 50 ref - a 99"}},
		60000: {{"20:00:00 trade s 40.0 mu 5.0 long - short - books true/false No Book"}, {"20:00:00 100.2 50.1 ref - a 100"}},
	}
	for _, step := range []int64{1000, 5000, 60000} {
		if got := lines(points, step); !slices.Equal(got, want[step][0]) {
			t.Fatalf("points at step %d:\ngot\n%s\nwant\n%s", step, strings.Join(got, "\n"), strings.Join(want[step][0], "\n"))
		}
		if got := lines(prices, step); !slices.Equal(got, want[step][1]) {
			t.Fatalf("prices at step %d:\ngot\n%s\nwant\n%s", step, strings.Join(got, "\n"), strings.Join(want[step][1], "\n"))
		}
	}
}

// SOXX's 3-for-1 split on Bitget, 2026-11-05 (the carrier session 11-04, bell 450.70): the last trade before the halt was
// 440.58 at 02:00:13 New York, the first minute on the new basis 04:35 (T_v), and the recorder's rows of 04:35:01 to 04:35:05
// still held that old trade. From T_v a row whose last trade is from before T_v has no price on A / r: the reference's price
// is NULL there (it would read +193 % against 150.2333), and from its first new trade (146.93) it is on A / r again. Checked on
// combo_prices (board 3.2; board 3.1's combo_points gave the same rows until it was dropped, OWNER.md item 24).
func TestSplitSecondsOnDatabase(t *testing.T) {
	st := scratchStore(t)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := st.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	cfg := validConfig()
	cfg.Combos = []Combo{{ID: "S", BullTicker: "SOXL", BearTicker: "SOXS", RefTicker: "SOXX",
		Markets: []Market{{Venue: "bitget", BullSymbol: "SOXLUSDT", BearSymbol: "SOXSUSDT", RefSymbol: "SOXXUSDT"}}}}
	if err := st.seed(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	var sessions []session
	for _, date := range []string{"2026-11-04", "2026-11-05"} {
		closeAt, _ := Calendar{}.closeOf(date)
		sessions = append(sessions, session{date, closeAt.UnixMilli()})
	}
	if err := st.writeCalendar(ctx, sessions, nil); err != nil {
		t.Fatal(err)
	}
	for ticker, price := range map[string]string{"SOXL": "100", "SOXS": "50", "SOXX": "450.70"} {
		exec(`INSERT INTO etfpair.etf_bell VALUES ($1, '2026-11-04', $2::numeric, $2::numeric, 'binance', 1, $3, 1000)`, ticker, price, sessions[0].CloseMs-minuteMs)
	}
	exec(`INSERT INTO etfpair.etf_action VALUES ('SOXX', '2026-11-05', 'split', 3, 'Known', 'test', 'u', 1)`)
	ny := func(s string) int64 { return nyTime(t, s).UnixMilli() }
	// Minutes 01:55 to 04:40: SOXX trades to 02:00 (440.58), is frozen from 02:01 (the halt) and resumes at 04:35 on the new basis.
	for at := ny("2026-11-05 01:55:00"); at <= ny("2026-11-05 04:40:00"); at += minuteMs {
		soxx := "440.58"
		switch {
		case at < ny("2026-11-05 02:00:00"):
			soxx = fmt.Sprint(440 + 0.1*float64((at-ny("2026-11-05 01:55:00"))/minuteMs))
		case at >= ny("2026-11-05 04:35:00"):
			soxx = "146.73"
		}
		exec(`INSERT INTO etfpair.fx_minute (pair, minute_ms, price, source, sampled_ms) VALUES ('USDT-USD', $1, 1, 'candle', 0)`, at)
		for symbol, price := range map[string]string{"SOXLUSDT": "100", "SOXSUSDT": "50", "SOXXUSDT": soxx} {
			exec(`INSERT INTO etfpair.perp_minute VALUES ('bitget', $1, $2, $3::numeric, 0)`, symbol, at, price)
		}
	}
	tv, old := ny("2026-11-05 04:35:00"), ny("2026-11-05 02:00:13")
	for i := int64(0); i <= 6; i++ {
		at := tv + i*1000
		soxx, traded := "440.58", old
		if i == 6 {
			soxx, traded = "146.93", tv+5_500
		}
		for symbol, price := range map[string]string{"SOXLUSDT": "100", "SOXSUSDT": "50", "SOXXUSDT": soxx} {
			trade := at - 300
			if symbol == "SOXXUSDT" {
				trade = traded
			}
			exec(`INSERT INTO etfpair.perp_second (venue, symbol, second_ms, price, trade_ms) VALUES ('bitget', $1, $2, $3::numeric, $4)`, symbol, at, price, trade)
		}
	}
	rows, err := st.pool.Query(ctx, `SELECT CASE WHEN p_bull IS NULL THEN 'break' ELSE
		to_char(to_timestamp(ts_ms / 1000.0) AT TIME ZONE 'America/New_York', 'HH24:MI:SS') || ' ref ' || coalesce(trim_scale(p_ref)::text, '-') ||
		' a_ref ' || trim_scale(round(a_ref, 4)) || ' return ' || coalesce(round(100 * (fx * p_ref / a_ref - 1), 2)::text, '-') END
		FROM etfpair.combo_prices('S', 'bitget', $1, $2, 1000) ORDER BY ts_ms`, tv-1000, tv+6000)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"04:35:00 ref - a_ref 450.7 return -", // the trade before 04:35 belongs to 04:34, in the halt
		"04:35:01 ref - a_ref 150.2333 return -", "04:35:02 ref - a_ref 150.2333 return -", "04:35:03 ref - a_ref 150.2333 return -",
		"04:35:04 ref - a_ref 150.2333 return -", "04:35:05 ref - a_ref 150.2333 return -", // 440.58 of 02:00:13: none on A / r
		"04:35:06 ref 146.93 a_ref 150.2333 return -2.20"}
	if !slices.Equal(got, want) {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// The real spreads of a notional on the worked example of SPEC.md 7.9 (A 40 / 60, the books of the example) as the board
// draws them (combo_range, board 3.2; board 3.1's combo_points until it was dropped, OWNER.md item 24), the spread on the
// mid prices (21.6667 bps, SPEC's s = 21.67 on the mids 40.46 / 59.44), at F from the minute's fx_minute; the largest notional
// each side fills (asks: min(40 x 300, 60 x 300) x (40.47 / 40 + 59.45 / 60) = 24,031; bids: min(40 x 800, 60 x 260) x
// (40.45 / 40 + 59.43 / 60) = 31,227.3, which 30,000 still fits and 100,000 does not): at each
// notional N (USDT, both legs) the quantities are q = K / A with K = N / (p_bull / A_bull + p_bear / A_bear), first on the
// best prices and then once more on the average prices, each leg swept through its levels; the spread is on the last average
// prices and the point's F. N = 10,000 sells at 17.0344 bps (SPEC: s_exec = 17.03, SOXS 59.4272 over two levels); a tiny N
// is the best levels' spread; a side whose levels cannot fill N is computed on all its recorded levels and is Partial (3.1
// had none there), the values of TestBookPairOnDatabase. The values were computed by hand in Python's Decimal
// (experiments/real_spread_20261122/DESIGN.md section 2; experiments/board32_20261122/build/sql_go/scripts/expected_book.py).
func TestRealSpreadsOnDatabase(t *testing.T) {
	st := scratchStore(t)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := st.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	cfg := validConfig()
	cfg.Combos = []Combo{{ID: "R", BullTicker: "BULL", BearTicker: "BEAR", RefTicker: "REF",
		Markets: []Market{{Venue: "binance", BullSymbol: "BULLUSDT", BearSymbol: "BEARUSDT", RefSymbol: "REFUSDT"}}}}
	if err := st.seed(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	var sessions []session
	for _, date := range []string{"2026-11-19", "2026-11-20"} {
		closeAt, _ := Calendar{}.closeOf(date)
		sessions = append(sessions, session{date, closeAt.UnixMilli()})
	}
	if err := st.writeCalendar(ctx, sessions, nil); err != nil {
		t.Fatal(err)
	}
	for ticker, price := range map[string]string{"BULL": "40", "BEAR": "60", "REF": "300"} {
		exec(`INSERT INTO etfpair.etf_bell VALUES ($1, '2026-11-19', $2::numeric, $2::numeric, 'binance', 1, $3, 1000)`, ticker, price, sessions[0].CloseMs-minuteMs)
	}
	at := nyTime(t, "2026-11-19 16:10:05").UnixMilli()
	later := at + minuteMs // the same books a minute later, with F = 0.999
	for when, fx := range map[int64]string{at: "1", later: "0.999"} {
		exec(`INSERT INTO etfpair.fx_minute (pair, minute_ms, price, source, sampled_ms) VALUES ('USDT-USD', $1, $2::numeric, 'candle', 0)`, (when-1)/minuteMs*minuteMs, fx)
		exec(`INSERT INTO etfpair.perp_second (venue, symbol, second_ms, price, bid_px, bid_sz, ask_px, ask_sz) VALUES
			('binance', 'BULLUSDT', $1, 40.46, '{40.45,40.44}', '{300,500}', '{40.47}', '{300}'),
			('binance', 'BEARUSDT', $1, 59.44, '{59.43,59.42}', '{60,200}', '{59.45,59.46}', '{100,200}')`, when)
	}
	rows, err := st.pool.Query(ctx, `SELECT trim_scale(f.price) || ' ' || n || ' long ' || round(r.s_long_bps, 4) || ' ' || r.partial_long ||
		' short ' || round(r.s_short_bps, 4) || ' ' || r.partial_short || ' mid ' || round(r.s_bps, 4)
		FROM unnest(ARRAY[1, 10000, 20000, 30000, 100000]::numeric[]) n, LATERAL etfpair.combo_range('R', 'binance', 'sma', 1, $1, $2, 1000, n, 10) r
		JOIN etfpair.fx_minute f ON f.minute_ms = (r.ts_ms - 1) / 60000 * 60000
		WHERE r.basis = 'mid' ORDER BY r.ts_ms, n`, at-1000, later)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"1 1 long 25.8333 false short 17.5000 false mid 21.6667", // the best levels: 10^4 (40.47 / 40 + 59.45 / 60 - 2), (40.45 / 40 + 59.43 / 60 - 2)
		"1 10000 long 25.8333 false short 17.0344 false mid 21.6667",
		"1 20000 long 26.4987 false short 16.4338 false mid 21.6667",
		"1 30000 long 26.6667 true short 15.7352 false mid 21.6667", // BULL's 300 asks hold less than its 374.5: all of them
		"1 100000 long 26.6667 true short 15.6410 true mid 21.6667", // neither side fills
		"0.999 1 long 5.8075 false short -2.5175 false mid 1.6450",  // F scales both prices
		"0.999 10000 long 5.8075 false short -2.9827 false mid 1.6450",
		"0.999 20000 long 6.4722 false short -3.5826 false mid 1.6450",
		"0.999 30000 long 6.6400 true short -4.2805 false mid 1.6450",
		"0.999 100000 long 6.6400 true short -4.3746 true mid 1.6450"}
	if !slices.Equal(got, want) {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// --- board 3.2 (experiments/board32_20261122/DESIGN.md, third version) ---

// dbLines runs sql on the scratch database and returns its one text column, row by row.
func dbLines(t *testing.T, st *store, sql string, args ...any) []string {
	t.Helper()
	rows, err := st.pool.Query(context.Background(), sql, args...)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return lines
}

func dbExec(t *testing.T, st *store, sql string, args ...any) {
	t.Helper()
	if _, err := st.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func expectLines(t *testing.T, name string, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("%s:\ngot\n%s\nwant\n%s", name, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// The collector applies the schema at every start: a second application changes nothing and fails nothing. Board 3.1's
// objects, which only the 3.1 board read, are gone after it (OWNER.md item 24): on a database that still has them (as
// production had until then) and on a new one; the four 3.2 calls stay.
func TestSchemaAppliesTwice(t *testing.T) {
	st := scratchStore(t)
	ctx := context.Background()
	if _, err := st.pool.Exec(ctx, schemaSQL, pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatal(err)
	}
	board31 := `SELECT p.oid::regprocedure::text FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = 'etfpair' AND p.proname IN ('combo_series', 'combo_points', 'ema', 'ema_step', 'real_spread', 'real_depth', 'sweep') ORDER BY 1`
	expectLines(t, "board 3.1's objects", dbLines(t, st, board31))
	// Put back what production held until item 24 (stand-ins of the same signatures; the aggregate on its step function, a
	// caller on each of them) and apply the schema once more: all of them go.
	dbExec(t, st, `CREATE FUNCTION etfpair.ema_step(numeric, numeric, numeric) RETURNS numeric LANGUAGE sql AS 'SELECT $2';
		CREATE AGGREGATE etfpair.ema(numeric, numeric) (SFUNC = etfpair.ema_step, STYPE = numeric);
		CREATE FUNCTION etfpair.sweep(numeric[], numeric[], numeric) RETURNS numeric LANGUAGE sql AS 'SELECT $3';
		CREATE FUNCTION etfpair.real_spread(numeric[], numeric[], numeric[], numeric[], numeric, numeric, numeric, numeric) RETURNS numeric
			LANGUAGE sql AS 'SELECT etfpair.sweep($1, $2, $8)';
		CREATE FUNCTION etfpair.real_depth(numeric[], numeric[], numeric[], numeric[], numeric, numeric) RETURNS numeric LANGUAGE sql AS 'SELECT $5';
		CREATE FUNCTION etfpair.combo_series(text, text, text, integer, bigint, bigint) RETURNS SETOF numeric LANGUAGE sql AS 'SELECT 1::numeric';
		CREATE FUNCTION etfpair.combo_points(text, text, text, integer, bigint, bigint, bigint, numeric) RETURNS SETOF numeric LANGUAGE sql
			AS 'SELECT etfpair.ema(1, 0.5) OVER ()'`)
	expectLines(t, "stand-ins", dbLines(t, st, board31), "etfpair.combo_points(text,text,text,integer,bigint,bigint,bigint,numeric)",
		"etfpair.combo_series(text,text,text,integer,bigint,bigint)", "etfpair.ema(numeric,numeric)", "etfpair.ema_step(numeric,numeric,numeric)",
		"etfpair.real_depth(numeric[],numeric[],numeric[],numeric[],numeric,numeric)",
		"etfpair.real_spread(numeric[],numeric[],numeric[],numeric[],numeric,numeric,numeric,numeric)", "etfpair.sweep(numeric[],numeric[],numeric)")
	if _, err := st.pool.Exec(ctx, schemaSQL, pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatal(err)
	}
	expectLines(t, "board 3.1's objects after the schema", dbLines(t, st, board31))
	expectLines(t, "what 3.2 calls", dbLines(t, st, `SELECT p.proname::text FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = 'etfpair' AND p.proname IN ('combo_anchors', 'combo_actions', 'combo_sessions', 'action_start') ORDER BY 1`),
		"action_start", "combo_actions", "combo_anchors", "combo_sessions")
}

// book_pair on the worked example of SPEC.md 7.9 (A 40 / 60, the books of TestRealSpreadsOnDatabase): where the
// recorded levels fill N in both passes the real spreads are the hand-computed ones (board 3.1's real_spread gave the same
// digit for digit until it was dropped, OWNER.md item 24) and not partial; where they
// cannot, K is capped at what both legs' levels hold, min(40 x 300, 60 x 300) = 12,000 on the asks (all of BULL's 300
// asks, 200 of BEAR's), min(40 x 800, 60 x 260) = 15,600 on the bids: in both passes (N 100,000), or in the second only
// (N 31,225 on the bids: the first pass's K 15,598.9 fits, the second's 15,600.3 does not). The filled USDT never exceed
// N (but for the rounding of the divisions
// that give K and the quantities: 2e-19 USDT over N 10,000 here, so the check allows 1e-15 of N). The values were computed from
// DESIGN 1.4 in Python's Decimal (experiments/board32_20261122/build/sql_go/scripts/expected_book.py). Without a
// positive N, an anchor or F there are no real spreads; a side whose levels are empty on a leg has none.
func TestBookPairOnDatabase(t *testing.T) {
	st := scratchStore(t)
	got := dbLines(t, st, `SELECT trim_scale(fx) || ' ' || n || ' long ' || round(x.s_long_bps, 4) || ' ' || x.partial_long || ' ' || round(x.filled_long, 4) ||
		' short ' || round(x.s_short_bps, 4) || ' ' || x.partial_short || ' ' || round(x.filled_short, 4) ||
		CASE WHEN x.filled_long > n * (1 + 1e-15) OR x.filled_short > n * (1 + 1e-15) THEN ' FILLED MORE THAN N' ELSE '' END
		FROM unnest('{1, 0.999}'::numeric[]) fx, unnest('{1, 10000, 20000, 30000, 31225, 100000}'::numeric[]) n,
		     LATERAL etfpair.book_pair('{40.45,40.44}', '{300,500}', '{40.47}', '{300}', '{59.43,59.42}', '{60,200}', '{59.45,59.46}', '{100,200}',
		                               40, 60, fx, n) x
		ORDER BY fx DESC, n`)
	expectLines(t, "real spreads", got,
		"1 1 long 25.8333 false 1.0000 short 17.5000 false 1.0000",
		"1 10000 long 25.8333 false 10000.0000 short 17.0344 false 10000.0000",
		"1 20000 long 26.4987 false 20000.0000 short 16.4338 false 20000.0000",
		"1 30000 long 26.6667 true 24032.0000 short 15.7352 false 29999.9997",
		"1 31225 long 26.6667 true 24032.0000 short 15.6410 true 31224.4000",
		"1 100000 long 26.6667 true 24032.0000 short 15.6410 true 31224.4000",
		"0.999 1 long 5.8075 false 1.0000 short -2.5175 false 1.0000",
		"0.999 10000 long 5.8075 false 10000.0000 short -2.9827 false 10000.0000",
		"0.999 20000 long 6.4722 false 20000.0000 short -3.5826 false 20000.0000",
		"0.999 30000 long 6.6400 true 24032.0000 short -4.2805 false 29999.9997",
		"0.999 31225 long 6.6400 true 24032.0000 short -4.3746 true 31224.4000",
		"0.999 100000 long 6.6400 true 24032.0000 short -4.3746 true 31224.4000")
	got = dbLines(t, st, `SELECT c.name || ': ' || coalesce(round(x.s_long_bps, 4)::text, '-') || ' ' || coalesce(x.partial_long::text, '-') || ' ' ||
		coalesce(round(x.filled_long, 4)::text, '-') || ' / ' || coalesce(round(x.s_short_bps, 4)::text, '-') || ' ' || coalesce(x.partial_short::text, '-') || ' ' ||
		coalesce(round(x.filled_short, 4)::text, '-')
		FROM (VALUES (1, 'no notional', NULL::numeric, 40::numeric, 1::numeric, '{40.47}'::numeric[], '{300}'::numeric[]),
		             (2, 'zero notional', 0, 40, 1, '{40.47}', '{300}'), (3, 'negative notional', -1, 40, 1, '{40.47}', '{300}'),
		             (4, 'no anchor', 10000, NULL, 1, '{40.47}', '{300}'), (5, 'no F', 10000, 40, NULL, '{40.47}', '{300}'),
		             (6, 'bull asks empty', 10000, 40, 1, '{}', '{}')) c(i, name, n, a, fx, asks, ask_sz)
		CROSS JOIN LATERAL etfpair.book_pair('{40.45,40.44}', '{300,500}', c.asks, c.ask_sz, '{59.43,59.42}', '{60,200}', '{59.45,59.46}', '{100,200}', c.a, 60,
		                                     c.fx, c.n) x
		ORDER BY c.i`)
	expectLines(t, "no real spreads", got,
		"no notional: - - - / - - -", "zero notional: - - - / - - -", "negative notional: - - - / - - -", "no anchor: - - - / - - -", "no F: - - - / - - -",
		"bull asks empty: - - - / 17.0344 false 10000.0000")
}

// The 1+2 depths (DESIGN 1.7): each leg's size in its first two 0.01 buckets of each side, bids floored, asks ceiled
// (a bid of 10.050 is in 10.05, an ask of 10.070 in 10.07), reading up to the first level of the third bucket and no
// further; a leg whose levels end within its second bucket has none (Not Recorded), the other leg keeps its own; an
// empty side has none; they do not depend on N, A or F. top2_size gives the same for every side.
func TestOneTwoDepthOnDatabase(t *testing.T) {
	st := scratchStore(t)
	got := dbLines(t, st, `SELECT c.name || ': bids ' || coalesce(trim_scale(x.bid12_bull)::text, '-') || '/' || coalesce(trim_scale(x.bid12_bear)::text, '-') ||
		' asks ' || coalesce(trim_scale(x.ask12_bull)::text, '-') || '/' || coalesce(trim_scale(x.ask12_bear)::text, '-') ||
		CASE WHEN (x.bid12_bull, x.bid12_bear, x.ask12_bull, x.ask12_bear) IS DISTINCT FROM
		          (etfpair.top2_size(c.bbp, c.bbs, true), etfpair.top2_size(c.sbp, c.sbs, true), etfpair.top2_size(c.bap, c.bas, false), etfpair.top2_size(c.sap, c.sas, false))
		     THEN ' NOT top2_size' ELSE '' END
		FROM (VALUES (1, '0.01 and 0.001 ticks', '{10.05,10.04,10.03,10.02,10.01}'::numeric[], '{1,2,3,4,5}'::numeric[], '{10.06,10.07,10.08,10.09,10.10}'::numeric[],
		              '{1,2,3,4,5}'::numeric[], '{10.059,10.055,10.050,10.049,10.041,10.040,10.039}'::numeric[], '{1,2,3,4,5,6,7}'::numeric[],
		              '{10.061,10.065,10.070,10.071,10.080,10.081}'::numeric[], '{1,2,3,4,5,6}'::numeric[], 10000::numeric),
		             (2, 'no notional', '{10.05,10.04,10.03}', '{1,2,3}', '{10.06,10.07,10.08}', '{1,2,3}', '{10.059,10.055,10.050,10.049,10.041,10.040,10.039}', '{1,2,3,4,5,6,7}',
		              '{10.061,10.065,10.070,10.071,10.080,10.081}', '{1,2,3,4,5,6}', NULL),
		             (3, 'bear bids in two buckets', '{10.05,10.04,10.03}', '{1,2,3}', '{10.06,10.07,10.08}', '{1,2,3}', '{10.059,10.051,10.049}', '{1,2,3}',
		              '{10.061,10.065,10.070,10.071,10.080,10.081}', '{1,2,3,4,5,6}', 10000),
		             (4, 'bull asks of two levels', '{10.05,10.04,10.03}', '{1,2,3}', '{10.06,10.07}', '{1,2}', '{10.059,10.055,10.050,10.049,10.041,10.040,10.039}',
		              '{1,2,3,4,5,6,7}', '{10.061,10.065,10.070,10.071,10.080,10.081}', '{1,2,3,4,5,6}', 10000),
		             (5, 'bull asks empty', '{10.05,10.04,10.03}', '{1,2,3}', '{}', '{}', '{10.059,10.055,10.050,10.049,10.041,10.040,10.039}', '{1,2,3,4,5,6,7}',
		              '{10.061,10.065,10.070,10.071,10.080,10.081}', '{1,2,3,4,5,6}', 10000))
		     c(i, name, bbp, bbs, bap, bas, sbp, sbs, sap, sas, n)
		CROSS JOIN LATERAL etfpair.book_pair(c.bbp, c.bbs, c.bap, c.bas, c.sbp, c.sbs, c.sap, c.sas, 10, 10, 1, c.n) x
		ORDER BY c.i`)
	expectLines(t, "1+2 depths", got,
		"0.01 and 0.001 ticks: bids 3/21 asks 3/15", "no notional: bids 3/21 asks 3/15", "bear bids in two buckets: bids 3/- asks 3/15",
		"bull asks of two levels: bids 3/21 asks -/15", "bull asks empty: bids 3/21 asks -/15")
}

// The usable book and trade of a leg in a second (DESIGN 1.0): both sides with levels, prices and sizes above zero, the
// best bid below the best ask (crossed and locked books are none), and from a split's T_v not older than T_v; a trade
// from before T_v does not count either (a trade without its time does, as in 3.1).
func TestUsableBookAndTradeOnDatabase(t *testing.T) {
	st := scratchStore(t)
	got := dbLines(t, st, `SELECT c.name || ' ' || etfpair.book_usable(c.bp, c.bs, c.ap, c.az, c.bm, c.tv)
		FROM (VALUES (1, 'usable', '{10.00,9.99}'::numeric[], '{5,6}'::numeric[], '{10.01}'::numeric[], '{5}'::numeric[], 1000::bigint, NULL::bigint),
		             (2, 'crossed', '{10.02}', '{5}', '{10.01}', '{5}', 1000, NULL), (3, 'locked', '{10.01}', '{5}', '{10.01}', '{5}', 1000, NULL),
		             (4, 'none', NULL, NULL, NULL, NULL, NULL, NULL), (5, 'no bids', '{}', '{}', '{10.01}', '{5}', 1000, NULL),
		             (6, 'zero size', '{10.00,9.99}', '{5,0}', '{10.01}', '{5}', 1000, NULL), (7, 'size missing', '{10.00,9.99}', '{5}', '{10.01}', '{5}', 1000, NULL),
		             (8, 'size NULL', '{10.00,9.99}', '{5,NULL}', '{10.01}', '{5}', 1000, NULL), (9, 'price NULL', '{10.00,NULL}', '{5,5}', '{10.01}', '{5}', 1000, NULL),
		             (10, 'before T_v', '{10.00}', '{5}', '{10.01}', '{5}', 1999, 2000), (11, 'from T_v', '{10.00}', '{5}', '{10.01}', '{5}', 2000, 2000))
		     c(i, name, bp, bs, ap, az, bm, tv)
		ORDER BY c.i`)
	expectLines(t, "books", got, "usable true", "crossed false", "locked false", "none false", "no bids false", "zero size false", "size missing false",
		"size NULL false", "price NULL false", "before T_v false", "from T_v true")
	got = dbLines(t, st, `SELECT c.name || ' ' || etfpair.trade_usable(c.price, c.trade_ms, c.tv)
		FROM (VALUES (1, 'trade', 10::numeric, 1500::bigint, NULL::bigint), (2, 'none', NULL, NULL, NULL), (3, 'before T_v', 10, 1999, 2000),
		             (4, 'from T_v', 10, 2000, 2000), (5, 'no time', 10, NULL, 2000)) c(i, name, price, trade_ms, tv)
		ORDER BY c.i`)
	expectLines(t, "trades", got, "trade true", "none false", "before T_v false", "from T_v true", "no time true")
}

// The Signal's order (DESIGN 1.6): No Anchor, Past T, No Mean, No Notional, No Entry (Entry missing, zero, negative, NaN or infinite:
// with -5 every point would be a Long), No Book, then Long on the Real Long Spread at least Entry below the Mean (the
// boundary included), Short on the Real Short Spread at least Entry above it, None between; Partial when that side was
// capped.
func TestComboSignalOnDatabase(t *testing.T) {
	st := scratchStore(t)
	got := dbLines(t, st, `SELECT c.i || ' ' || etfpair.combo_signal(c.s, c.past_t, c.mu, c.n, c.mid, c.long, c.short, c.pl, c.ps, c.d)
		FROM (VALUES (1, NULL::numeric, true, NULL::numeric, NULL::numeric, false, NULL::numeric, NULL::numeric, NULL::boolean, NULL::boolean, 10::numeric),
		             (2, 5, true, NULL, NULL, false, NULL, NULL, NULL, NULL, 10), (3, 5, false, NULL, NULL, false, NULL, NULL, NULL, NULL, 10),
		             (4, 5, false, 1, NULL, true, -20, 20, false, false, 10), (5, 5, false, 1, 0, true, -20, 20, false, false, 10),
		             (6, 5, false, 1, 10000, false, NULL, NULL, NULL, NULL, 10), (7, 5, false, 1, 10000, true, -9, 0, false, false, 10),
		             (8, 5, false, 1, 10000, true, -9, 0, true, false, 10), (9, 5, false, 1, 10000, true, 0, 11, false, false, 10),
		             (10, 5, false, 1, 10000, true, 0, 11, false, true, 10), (11, 5, false, 1, 10000, true, -9, 11, false, false, 10),
		             (12, 5, false, 1, 10000, true, -8.99, 10.99, true, true, 10), (13, 5, false, 1, 10000, true, -8.99, 10.99, true, true, 7.5),
		             (14, 5, false, 1, 10000, true, -20, 20, false, false, NULL), (15, 5, false, 1, 10000, true, -20, 20, false, false, 0),
		             (16, 5, false, 1, 10000, true, 0.5, 1.5, false, false, -5), (17, 5, false, 1, 10000, false, NULL, NULL, NULL, NULL, 0),
		             (18, 5, false, 1, 0, true, -20, 20, false, false, 0), (19, 5, false, NULL, 10000, true, -20, 20, false, false, NULL),
		             (20, 5, true, 1, 10000, true, -20, 20, false, false, NULL), (21, NULL, false, NULL, 10000, false, NULL, NULL, NULL, NULL, -5),
		             (22, 5, false, 1, 10000, true, -20, 20, false, false, 'NaN'), (23, 5, false, 1, 10000, true, -20, 20, false, false, 'Infinity'))
		     c(i, s, past_t, mu, n, mid, long, short, pl, ps, d)
		ORDER BY c.i`)
	expectLines(t, "signals", got, "1 No Anchor", "2 Past T", "3 No Mean", "4 No Notional", "5 No Notional", "6 No Book", "7 Long", "8 Long · Partial",
		"9 Short", "10 Short · Partial", "11 Long", "12 None", "13 Long · Partial", "14 No Entry", "15 No Entry", "16 No Entry", "17 No Entry",
		"18 No Notional", "19 No Mean", "20 Past T", "21 No Anchor", "22 No Entry", "23 No Entry")
}

// threeTwoBoard writes a calendar (sessions 11-19, 11-20, 11-23 and their phases), the combo cfg and USD per USDT 1
// for every minute of [from, to) on a scratch database.
func threeTwoBoard(t *testing.T, st *store, combo Combo, from, to int64) {
	t.Helper()
	ctx := context.Background()
	cfg := validConfig()
	cfg.Combos = []Combo{combo}
	if err := st.seed(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	days := []string{"2026-11-19", "2026-11-20", "2026-11-23"}
	var sessions []session
	for _, date := range days {
		closeAt, err := Calendar{}.closeOf(date)
		if err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, session{date, closeAt.UnixMilli()})
	}
	phases, err := Calendar{}.phasesOf(days)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.writeCalendar(ctx, sessions, phases); err != nil {
		t.Fatal(err)
	}
	dbExec(t, st, `INSERT INTO etfpair.fx_minute (pair, minute_ms, price, source, sampled_ms)
		SELECT 'USDT-USD', m, 1, 'candle', 0 FROM generate_series($1::bigint, $2::bigint - 60000, 60000) m`, from, to)
}

// perpSecond inserts a recorder row: the trade 300 ms and the book 100 ms before the second unless times are given; a nil
// price or book leaves those columns NULL. Sizes are given per level.
func perpSecond(t *testing.T, st *store, venue, symbol string, at int64, price any, bids, bidSz, asks, askSz any, times ...int64) {
	t.Helper()
	tradeMs, bookMs := at-300, at-100
	if len(times) == 2 {
		tradeMs, bookMs = times[0], times[1]
	}
	dbExec(t, st, `INSERT INTO etfpair.perp_second (venue, symbol, second_ms, price, trade_ms, bid_px, bid_sz, ask_px, ask_sz, book_ms)
		VALUES ($1, $2, $3, $4::numeric, CASE WHEN $4::numeric IS NOT NULL THEN $5::bigint END, $6::numeric[], $7::numeric[], $8::numeric[], $9::numeric[],
		        CASE WHEN $6::numeric[] IS NOT NULL THEN $10::bigint END)`, venue, symbol, at, price, tradeMs, bids, bidSz, asks, askSz, bookMs)
}

// The points of the Spread row around the close of 2026-11-20 (16:00 New York), SMA of 2 minutes (and EMA), N 10,000,
// Entry 10, on BULL 40 / BEAR 60 / REF 300 of session 11-19 and no anchors for session 11-20:
//   - 15:57:00 both legs' books: a mid point; its minute is 15:56, the mean through 15:55 has no bar: No Mean;
//   - 15:57:30 BULL's book crossed, 15:58:00 BULL without a book: points on trade prices with BEAR's 1+2 depths only;
//   - bars: 15:56 mid (the row at 15:57:00), 15:57 trade (15:58:00), 15:58 the candle closes (no row at 15:59:00), so
//     the SMA through 15:58 is (40.10 + 40.20) / 2, (59.90 + 59.80) / 2: mu = 12.5 bps for the minute 15:59;
//   - 15:59:30 Long (the asks 116.7 bps under the mean); 15:59:45 BEAR without a book: No Book; 16:00:00, the last
//     second of session 11-19, BEAR's one bid of 50 caps the short side at K = 60 x 50 (Short · Partial) and leaves
//     BEAR's Bid 1+2 Depth Not Recorded;
//   - session 11-20 without anchors: No Anchor, no phase; a row of NULLs 1 ms after every point (all are farther
//     apart than the step; 16:00:00 to 16:01:00 is also a new session).
//
// combo_prices takes the trade prices of the same seconds with the reference where it traded; combo_last the newest
// basis second and the newest second of both legs' and the reference's trades.
func TestComboRangeOnDatabase(t *testing.T) {
	st := scratchStore(t)
	closeAt, _ := Calendar{}.closeOf("2026-11-20")
	c := closeAt.UnixMilli()
	threeTwoBoard(t, st, Combo{ID: "Q", BullTicker: "BULL", BearTicker: "BEAR", RefTicker: "REF",
		Markets: []Market{{Venue: "binance", BullSymbol: "BULLUSDT", BearSymbol: "BEARUSDT", RefSymbol: "REFUSDT"}}}, c-24*60*minuteMs, c+10*minuteMs)
	close19, _ := Calendar{}.closeOf("2026-11-19")
	for ticker, price := range map[string]string{"BULL": "40", "BEAR": "60", "REF": "300"} {
		dbExec(t, st, `INSERT INTO etfpair.etf_bell VALUES ($1, '2026-11-19', $2::numeric, $2::numeric, 'binance', 1, $3, 1000)`, ticker, price, close19.UnixMilli()-minuteMs)
	}
	dbExec(t, st, `INSERT INTO etfpair.perp_minute VALUES ('binance', 'BULLUSDT', $1, 40.20, 0), ('binance', 'BEARUSDT', $1, 59.80, 0),
		('binance', 'BULLUSDT', $2, 41.05, 0)`, c-2*minuteMs, c+minuteMs)
	row := func(symbol string, at int64, price any, bids, bidSz, asks, askSz any) {
		perpSecond(t, st, "binance", symbol, at, price, bids, bidSz, asks, askSz)
	}
	k3 := "{1000,1000,1000}"
	row("BULLUSDT", c-180_000, "40.01", "{40.00,39.99,39.98}", k3, "{40.02,40.03,40.04}", k3)
	row("BEARUSDT", c-180_000, "60.00", "{59.98,59.97,59.96}", k3, "{60.02,60.03,60.04}", k3)
	row("REFUSDT", c-180_000, "300.5", nil, nil, nil, nil)
	row("BULLUSDT", c-150_000, "40.05", "{40.05}", "{10}", "{40.04}", "{10}") // crossed
	row("BEARUSDT", c-150_000, "59.95", "{59.94,59.93,59.92}", "{5,6,7}", "{59.96,59.97,59.98}", "{8,9,10}")
	row("BULLUSDT", c-120_000, "40.10", nil, nil, nil, nil)
	row("BEARUSDT", c-120_000, "59.90", "{59.89,59.88,59.87}", "{1,2,3}", "{59.91,59.92,59.93}", "{4,5,6}")
	row("REFUSDT", c-120_000, "300.6", nil, nil, nil, nil)
	row("BULLUSDT", c-30_000, "39.79", "{39.78,39.77,39.76,39.75,39.74}", "{1000,20,30,40,50}", "{39.80,39.81,39.82,39.83,39.84}", "{1000,1000,1000,1000,1000}")
	row("BEARUSDT", c-30_000, "59.59", "{59.58,59.57,59.56,59.55,59.54}", "{1000,22,33,44,55}", "{59.60,59.61,59.62,59.63,59.64}", "{1000,1000,1000,1000,1000}")
	row("BULLUSDT", c-15_000, "39.90", "{39.89,39.88,39.87}", "{7,8,9}", "{39.91,39.92,39.93}", "{7,8,9}")
	row("BEARUSDT", c-15_000, "59.70", nil, nil, nil, nil)
	row("BULLUSDT", c, "40.40", "{40.39,40.38,40.37}", k3, "{40.41,40.42,40.43}", k3)
	row("BEARUSDT", c, "59.80", "{59.79}", "{50}", "{59.81,59.82,59.83}", k3)
	row("REFUSDT", c, "300.9", nil, nil, nil, nil)
	row("REFUSDT", c+60_000, nil, "{301}", "{1}", "{300}", "{1}") // a crossed book and no trade: no mid in perp_second_minute
	for _, at := range []int64{c + 60_000, c + 150_000, c + 180_000} {
		row("BULLUSDT", at, "41.01", "{41.00,40.99,40.98}", "{100,100,100}", "{41.02,41.03,41.04}", "{100,100,100}")
		row("BEARUSDT", at, "58.99", "{58.98,58.97,58.96}", "{100,100,100}", "{59.00,59.01,59.02}", "{100,100,100}")
	}
	const ny = "to_char(to_timestamp(ts_ms / 1000.0) AT TIME ZONE 'America/New_York', 'HH24:MI:SS')"
	num := func(x string) string { return "coalesce(round(" + x + ", 4)::text, '-')" }
	size := func(x string) string { return "coalesce(trim_scale(" + x + ")::text, '-')" }
	got := dbLines(t, st, `SELECT CASE WHEN basis IS NULL THEN 'break ' || to_char(to_timestamp(ts_ms / 1000.0) AT TIME ZONE 'America/New_York', 'HH24:MI:SS.MS') ELSE
		`+ny+` || ' ' || basis || ' s ' || `+num("s_bps")+` || ' mu ' || `+num("mu_bps")+` || ' long ' || `+num("s_long_bps")+` || ' short ' || `+num("s_short_bps")+` ||
		' partial ' || coalesce(partial_long::text, '-') || '/' || coalesce(partial_short::text, '-') || ' books ' || book_bull || '/' || book_bear ||
		' bids ' || `+size("bid12_bull")+` || '/' || `+size("bid12_bear")+` || ' asks ' || `+size("ask12_bull")+` || '/' || `+size("ask12_bear")+` ||
		' a_ok ' || a_ok || ' ' || coalesce(phase || ' ' || phase_sort, '-') || ' ' || signal END
		FROM etfpair.combo_range('Q', 'binance', 'sma', 2, $1, $2, 1000, 10000, 10) ORDER BY ts_ms`, c-181_000, c+180_000)
	expectLines(t, "combo_range", got,
		"15:57:00 mid s 2.5000 mu - long 8.3333 short -3.3333 partial false/false books true/true bids 2000/2000 asks 2000/2000 a_ok true - No Mean",
		"break 15:57:00.001",
		"15:57:30 trade s 4.1667 mu - long - short - partial -/- books false/true bids -/11 asks -/17 a_ok true - No Mean",
		"break 15:57:30.001",
		"15:58:00 trade s 8.3333 mu - long - short - partial -/- books false/true bids -/3 asks -/9 a_ok true - No Mean",
		"break 15:58:00.001",
		"15:59:30 mid s -120.8333 mu 12.5000 long -116.6667 short -125.0000 partial false/false books true/true bids 1020/1022 asks 2000/2000 a_ok true Regular 2 Long",
		"break 15:59:30.001",
		"15:59:45 trade s -75.0000 mu 12.5000 long - short - partial -/- books true/false bids 15/- asks 15/- a_ok true Regular 2 No Book",
		"break 15:59:45.001",
		"16:00:00 mid s 66.6667 mu 12.5000 long 70.8333 short 62.5000 partial false/true books true/true bids 2000/- asks 2000/2000 a_ok true Regular 2 Short · Partial",
		"break 16:00:00.001",
		"16:01:00 mid s - mu - long - short - partial -/- books true/true bids 200/200 asks 200/200 a_ok false - No Anchor",
		"break 16:01:00.001",
		"16:02:30 mid s - mu - long - short - partial -/- books true/true bids 200/200 asks 200/200 a_ok false - No Anchor",
		"break 16:02:30.001",
		"16:03:00 mid s - mu - long - short - partial -/- books true/true bids 200/200 asks 200/200 a_ok false - No Anchor")
	// The EMA of 2 minutes (alpha 2/3) of the same run from its first bar: 40.01, then 40.07 and 40.156667; 60, 59.933333,
	// 59.844444: mu = 10^4 (40.156667 / 40 + 59.844444 / 60 - 2).
	got = dbLines(t, st, `SELECT `+ny+` || ' ' || round(mu_bps, 4) FROM etfpair.combo_range('Q', 'binance', 'ema', 2, $1, $2, 1000, 10000, 10)
		WHERE mu_bps IS NOT NULL ORDER BY ts_ms`, c-181_000, c+180_000)
	expectLines(t, "EMA", got, "15:59:30 13.2407", "15:59:45 13.2407", "16:00:00 13.2407")
	// Without a notional the points keep their depths; the Signal is No Notional once there is a mean (before No Book).
	got = dbLines(t, st, `SELECT `+ny+` || ' ' || coalesce(round(s_long_bps, 4)::text, '-') || ' ' || `+size("bid12_bull")+` || ' ' || signal
		FROM etfpair.combo_range('Q', 'binance', 'sma', 2, $1, $2, 1000, NULL, 10) WHERE ts_ms > $1 + 120000 AND ts_ms <= $1 + 181000 AND basis IS NOT NULL ORDER BY ts_ms`,
		c-181_000, c+180_000)
	expectLines(t, "no notional", got, "15:59:30 - 1020 No Notional", "15:59:45 - 15 No Notional", "16:00:00 - 2000 No Notional")
	// Without a mean length that is a positive number (NULL, 0, -5) the points, their real spreads, partial flags and depths,
	// and the breaks are those of SMA 2, with no mean: No Mean where there is a spread, No Anchor in session 11-20.
	points := `SELECT CASE WHEN basis IS NULL THEN 'break' ELSE ` + ny + ` || ' ' || basis || ' s ' || ` + num("s_bps") + ` || ' mu ' || ` + num("mu_bps") +
		` || ' long ' || ` + num("s_long_bps") + ` || ' short ' || ` + num("s_short_bps") + ` || ' partial ' || coalesce(partial_long::text, '-') || '/' ||
		coalesce(partial_short::text, '-') || ' bids ' || ` + size("bid12_bull") + ` || '/' || ` + size("bid12_bear") + ` || ' asks ' || ` + size("ask12_bull") +
		` || '/' || ` + size("ask12_bear") + ` || ' ' || coalesce(phase, '-') || ' ' || signal END
		FROM etfpair.combo_range('Q', 'binance', 'sma', %s, $1, $2, 1000, 10000, %s) ORDER BY ts_ms`
	for _, n := range []string{"NULL", "0", "-5"} {
		expectLines(t, "mean length "+n, dbLines(t, st, fmt.Sprintf(points, n, "10"), c-181_000, c+180_000),
			"15:57:00 mid s 2.5000 mu - long 8.3333 short -3.3333 partial false/false bids 2000/2000 asks 2000/2000 - No Mean", "break",
			"15:57:30 trade s 4.1667 mu - long - short - partial -/- bids -/11 asks -/17 - No Mean", "break",
			"15:58:00 trade s 8.3333 mu - long - short - partial -/- bids -/3 asks -/9 - No Mean", "break",
			"15:59:30 mid s -120.8333 mu - long -116.6667 short -125.0000 partial false/false bids 1020/1022 asks 2000/2000 - No Mean", "break",
			"15:59:45 trade s -75.0000 mu - long - short - partial -/- bids 15/- asks 15/- - No Mean", "break",
			"16:00:00 mid s 66.6667 mu - long 70.8333 short 62.5000 partial false/true bids 2000/- asks 2000/2000 - No Mean", "break",
			"16:01:00 mid s - mu - long - short - partial -/- bids 200/200 asks 200/200 - No Anchor", "break",
			"16:02:30 mid s - mu - long - short - partial -/- bids 200/200 asks 200/200 - No Anchor", "break",
			"16:03:00 mid s - mu - long - short - partial -/- bids 200/200 asks 200/200 - No Anchor")
	}
	// Without an Entry that is a positive number (NULL, 0, -5) the points with a mean are No Entry, also the one on trade
	// prices (No Entry comes before No Book); the rest is as with Entry 10.
	for _, d := range []string{"NULL", "0", "-5"} {
		got = dbLines(t, st, fmt.Sprintf(points, "2", d), c-181_000, c+180_000)
		want := dbLines(t, st, fmt.Sprintf(points, "2", "10"), c-181_000, c+180_000)
		for i, line := range want {
			want[i] = strings.NewReplacer(" Long", " No Entry", " No Book", " No Entry", " Short · Partial", " No Entry").Replace(line)
		}
		expectLines(t, "Entry "+d, got, want...)
		expectLines(t, "Entry "+d+" signals", dbLines(t, st, `SELECT `+ny+` || ' ' || signal FROM etfpair.combo_range('Q', 'binance', 'sma', 2, $1, $2, 1000, 10000, `+d+`)
			WHERE mu_bps IS NOT NULL ORDER BY ts_ms`, c-181_000, c+180_000), "15:59:30 No Entry", "15:59:45 No Entry", "16:00:00 No Entry")
	}
	got = dbLines(t, st, `SELECT CASE WHEN p_bull IS NULL THEN 'break' ELSE `+ny+` || ' ' || session_date || ' ' || trim_scale(fx) || ' ' || trim_scale(p_bull) || ' ' ||
		trim_scale(p_bear) || ' ' || coalesce(trim_scale(p_ref)::text, '-') || ' ' || coalesce(trim_scale(a_bull)::text, '-') || ' ' || coalesce(trim_scale(a_bear)::text, '-') ||
		' ' || coalesce(trim_scale(a_ref)::text, '-') END
		FROM etfpair.combo_prices('Q', 'binance', $1, $2, 1000) ORDER BY ts_ms`, c-181_000, c+180_000)
	expectLines(t, "combo_prices", got,
		"15:57:00 2026-11-19 1 40.01 60 300.5 40 60 300", "break", "15:57:30 2026-11-19 1 40.05 59.95 - 40 60 300", "break",
		"15:58:00 2026-11-19 1 40.1 59.9 300.6 40 60 300", "break", "15:59:30 2026-11-19 1 39.79 59.59 - 40 60 300", "break",
		"15:59:45 2026-11-19 1 39.9 59.7 - 40 60 300", "break", "16:00:00 2026-11-19 1 40.4 59.8 300.9 40 60 300", "break",
		"16:01:00 2026-11-20 1 41.01 58.99 - - - -", "break", "16:02:30 2026-11-20 1 41.01 58.99 - - - -", "break", "16:03:00 2026-11-20 1 41.01 58.99 - - - -")
	lastOf := func(n, d string) string {
		return `SELECT kind || ' ' || ` + ny + ` || ' ' || coalesce(session_date::text, '-') || ' ' || coalesce(basis, '-') || ' ' || coalesce(trim_scale(fx)::text, '-') || ' ' ||
			coalesce(trim_scale(p_bull)::text, '-') || '/' || coalesce(trim_scale(p_bear)::text, '-') || '/' || coalesce(trim_scale(p_ref)::text, '-') || ' a ' ||
			coalesce(trim_scale(a_bull)::text, '-') || '/' || coalesce(trim_scale(a_bear)::text, '-') || '/' || coalesce(trim_scale(a_ref)::text, '-') || ' s ' || ` + num("s_bps") +
			` || ' mu ' || ` + num("mu_bps") + ` || ' filled ' || coalesce(round(filled_long, 2)::text, '-') || '/' || coalesce(round(filled_short, 2)::text, '-') ||
			' partial ' || coalesce(partial_long::text, '-') || '/' || coalesce(partial_short::text, '-') || ' bids ' || ` + size("bid12_bull") + ` || '/' || ` + size("bid12_bear") +
			` || ' ' || coalesce(anchor_status, '-') || ' ' || coalesce(signal, '-')
			FROM etfpair.combo_last('Q', 'binance', 'sma', ` + n + `, 10000, ` + d + `, $1) ORDER BY kind`
	}
	last := lastOf("2", "10")
	expectLines(t, "combo_last at 16:03:00", dbLines(t, st, last, c+180_000),
		"basis 16:03:00 2026-11-20 mid 1 41.01/58.99/- a -/-/- s - mu - filled -/- partial -/- bids 200/200 Missing No Anchor",
		"prices 16:00:00 2026-11-19 - 1 40.4/59.8/300.9 a 40/60/300 s - mu - filled -/- partial -/- bids -/- - -")
	expectLines(t, "combo_last at 16:00:00", dbLines(t, st, last, c),
		"basis 16:00:00 2026-11-19 mid 1 40.4/59.8/300.9 a 40/60/300 s 66.6667 mu 12.5000 filled 10000.00/6018.75 partial false/true bids 2000/- Bell Short · Partial",
		"prices 16:00:00 2026-11-19 - 1 40.4/59.8/300.9 a 40/60/300 s - mu - filled -/- partial -/- bids -/- - -")
	expectLines(t, "combo_last before any row", dbLines(t, st, last, c-200_000))
	// The newest seconds without a mean length (No Mean, the fills and depths kept) and without an Entry (No Entry).
	for _, n := range []string{"NULL", "0", "-5"} {
		expectLines(t, "combo_last mean length "+n, dbLines(t, st, lastOf(n, "10"), c),
			"basis 16:00:00 2026-11-19 mid 1 40.4/59.8/300.9 a 40/60/300 s 66.6667 mu - filled 10000.00/6018.75 partial false/true bids 2000/- Bell No Mean",
			"prices 16:00:00 2026-11-19 - 1 40.4/59.8/300.9 a 40/60/300 s - mu - filled -/- partial -/- bids -/- - -")
	}
	for _, d := range []string{"NULL", "0", "-5"} {
		expectLines(t, "combo_last Entry "+d, dbLines(t, st, lastOf("2", d), c),
			"basis 16:00:00 2026-11-19 mid 1 40.4/59.8/300.9 a 40/60/300 s 66.6667 mu 12.5000 filled 10000.00/6018.75 partial false/true bids 2000/- Bell No Entry",
			"prices 16:00:00 2026-11-19 - 1 40.4/59.8/300.9 a 40/60/300 s - mu - filled -/- partial -/- bids -/- - -")
	}
	// Once the minutes are aggregated, the bars come from perp_second_minute's end-row columns: the same figures.
	sameAfterRefresh(t, st, c-24*60*minuteMs, c+10*minuteMs,
		func() []string {
			return dbLines(t, st, `SELECT ts_ms || ' ' || coalesce(basis, '-') || ' ' || coalesce(s_bps::text, '-') || ' ' || coalesce(mu_bps::text, '-') || ' ' ||
				coalesce(signal, '-') FROM etfpair.combo_range('Q', 'binance', 'sma', 2, $1, $2, 1000, 10000, 10) ORDER BY ts_ms`, c-181_000, c+180_000)
		},
		func() []string {
			return dbLines(t, st, `SELECT ts_ms || ' ' || coalesce(mu_bps::text, '-') FROM etfpair.combo_range('Q', 'binance', 'ema', 2, $1, $2, 1000, 10000, 10)
				ORDER BY ts_ms`, c-181_000, c+180_000)
		},
		func() []string { return dbLines(t, st, last, c+180_000) },
		func() []string { return dbLines(t, st, last, c) })
	// The end-row columns: the price and trade_ms of the row at the minute's end, the mid of a usable book (BULL has none
	// at 15:58:00) and its book_ms; REF never has a book.
	expectLines(t, "end rows", dbLines(t, st, `SELECT symbol || ' ' || to_char(to_timestamp(minute_ms / 1000.0) AT TIME ZONE 'America/New_York', 'HH24:MI') || ' ' ||
		coalesce(trim_scale(end_price)::text, '-') || ' ' || coalesce(trim_scale(end_mid)::text, '-') || ' ' || coalesce((minute_ms + 60000 - end_trade_ms)::text, '-') || ' ' ||
		coalesce((minute_ms + 60000 - end_book_ms)::text, '-') FROM etfpair.perp_second_minute WHERE venue = 'binance' AND minute_ms BETWEEN $1 AND $2 ORDER BY symbol, minute_ms`,
		c-240_000, c),
		"BEARUSDT 15:56 60 60 300 100", "BEARUSDT 15:57 59.9 59.9 300 100", "BEARUSDT 15:59 59.8 59.8 300 100", "BEARUSDT 16:00 58.99 58.99 300 100",
		"BULLUSDT 15:56 40.01 40.01 300 100", "BULLUSDT 15:57 40.1 - 300 -", "BULLUSDT 15:59 40.4 40.4 300 100", "BULLUSDT 16:00 41.01 41.01 300 100",
		"REFUSDT 15:56 300.5 - 300 -", "REFUSDT 15:57 300.6 - 300 -", "REFUSDT 15:59 300.9 - 300 -", "REFUSDT 16:00 - - - 100")
}

// timedLines runs sql as dbLines does and fails when it takes a second or more; the query is cancelled after 10 s.
func timedLines(t *testing.T, st *store, sql string, args ...any) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	rows, err := st.pool.Query(ctx, sql, args...)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("%s: %v after %v", sql, err, time.Since(start))
	}
	if took := time.Since(start); took >= time.Second {
		t.Fatalf("%s took %v", sql, took)
	}
	return lines
}

// The minutes before the calendar's first close (16:00 New York of 2026-11-19 here) belong to no session and have no
// mean; their stretch starts at the minute of the earlier of the two legs' first per-second rows, and a market without
// rows has none. Both legs have books from 15:57:00 to 16:04:00 on Bitget and on Binance (15:57:00 is a second of the
// minute 15:56); on Bitget BULL also has one row a year before (one leg: no point):
//   - 15:57:00, 15:59:00 and 16:00:00 (the last second of the minute before the close) are No Anchor; 16:01:00 (minute
//     16:00, the session's first) and 16:02:00 (one bar) No Mean;
//   - the EMA of 2 minutes (alpha 2/3) of the session's mids 40.00, 40.30, 40.30 and 60, 60, 59.70 is 40.20 / 60 through
//     16:01 (mu 50 at 16:02:30 and 16:03:00) and 40.266667 / 59.80 through 16:02 (mu 33.3333 at 16:04:00).
//
// An EMA range from three hours before the close and the newest seconds at the close come within 1 s on Bitget: they
// need no bar before their first minute (from 1970 on before the first close, they enumerated every minute since
// BULL's early row). On Binance ranges from two years before the close give exactly what the ranges from the first row
// give, within 1 s, SMA and EMA, and so does combo_prices; on the market Z without rows a range from two years before
// is empty within 1 s.
func TestRangeBeforeTheFirstCloseOnDatabase(t *testing.T) {
	st := scratchStore(t)
	close19, _ := Calendar{}.closeOf("2026-11-19")
	c := close19.UnixMilli()
	p := Combo{ID: "P", BullTicker: "BULL", BearTicker: "BEAR", RefTicker: "REF",
		Markets: []Market{{Venue: "bitget", BullSymbol: "BULLUSDT", BearSymbol: "BEARUSDT", RefSymbol: "REFUSDT"},
			{Venue: "binance", BullSymbol: "BULLUSDT", BearSymbol: "BEARUSDT", RefSymbol: "REFUSDT"}}}
	threeTwoBoard(t, st, p, c-60*minuteMs, c+30*minuteMs)
	cfg := validConfig()
	cfg.Combos = []Combo{p, {ID: "Z", BullTicker: "ZB", BearTicker: "ZS", RefTicker: "ZR",
		Markets: []Market{{Venue: "bitget", BullSymbol: "ZBUSDT", BearSymbol: "ZSUSDT", RefSymbol: "ZRUSDT"}}}}
	if err := st.seed(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	for ticker, price := range map[string]string{"BULL": "40", "BEAR": "60", "REF": "300"} {
		dbExec(t, st, `INSERT INTO etfpair.etf_bell VALUES ($1, '2026-11-19', $2::numeric, $2::numeric, 'binance', 1, $3, 1000)`, ticker, price, c-minuteMs)
	}
	mids := []struct {
		at         int64
		bull, bear float64
	}{{c - 180_000, 40, 60}, {c - 60_000, 40, 60}, {c, 40, 60}, {c + 60_000, 40, 60}, {c + 120_000, 40.30, 60}, {c + 150_000, 40.10, 60},
		{c + 180_000, 40.30, 59.70}, {c + 240_000, 40, 60}}
	for _, venue := range []string{"bitget", "binance"} {
		for _, r := range mids {
			for symbol, mid := range map[string]float64{"BULLUSDT": r.bull, "BEARUSDT": r.bear} {
				perpSecond(t, st, venue, symbol, r.at, fmt.Sprintf("%.2f", mid), fmt.Sprintf("{%.2f}", mid-0.01), "{1000}", fmt.Sprintf("{%.2f}", mid+0.01), "{1000}")
			}
		}
	}
	perpSecond(t, st, "bitget", "BULLUSDT", c-365*24*60*minuteMs, "40", nil, nil, nil, nil)
	const ny = "to_char(to_timestamp(ts_ms / 1000.0) AT TIME ZONE 'America/New_York', 'HH24:MI:SS')"
	points := `SELECT CASE WHEN basis IS NULL THEN 'break' ELSE ` + ny + ` || ' ' || coalesce(session_date::text, '-') || ' ' || basis || ' s ' ||
		coalesce(round(s_bps, 4)::text, '-') || ' mu ' || coalesce(round(mu_bps, 4)::text, '-') || ' ' || signal END
		FROM etfpair.combo_range('P', $3, $4, 2, $1, $2, 1000, 10000, 10) ORDER BY ts_ms`
	want := []string{"15:57:00 - mid s - mu - No Anchor", "break", "15:59:00 - mid s - mu - No Anchor", "break", "16:00:00 - mid s - mu - No Anchor", "break",
		"16:01:00 2026-11-19 mid s 0.0000 mu - No Mean", "break", "16:02:00 2026-11-19 mid s 75.0000 mu - No Mean", "break",
		"16:02:30 2026-11-19 mid s 25.0000 mu 50.0000 Long", "break", "16:03:00 2026-11-19 mid s 25.0000 mu 50.0000 Long", "break",
		"16:04:00 2026-11-19 mid s 0.0000 mu 33.3333 Long"}
	expectLines(t, "EMA from three hours before the close", timedLines(t, st, points, c-180*minuteMs, c+5*minuteMs, "bitget", "ema"), want...)
	expectLines(t, "the newest seconds at the close", timedLines(t, st, `SELECT kind || ' ' || `+ny+` || ' ' || coalesce(session_date::text, '-') || ' ' ||
		basis || ' ' || signal FROM etfpair.combo_last('P', 'bitget', 'ema', 2, 10000, 10, $1)`, c), "basis 16:00:00 - mid No Anchor")
	first := c - 180_000 - 1 // the Binance rows' first second is the first multiple of the step after it
	for _, ma := range []string{"sma", "ema"} {
		from := timedLines(t, st, points, first, c+5*minuteMs, "binance", ma)
		expectLines(t, ma+" from two years before the close", timedLines(t, st, points, c-730*24*60*minuteMs, c+5*minuteMs, "binance", ma), from...)
	}
	expectLines(t, "EMA on Binance", timedLines(t, st, points, first, c+5*minuteMs, "binance", "ema"), want...)
	prices := `SELECT ts_ms || ' ' || coalesce(trim_scale(p_bull)::text, '-') || ' ' || coalesce(trim_scale(p_bear)::text, '-') || ' ' || coalesce(trim_scale(a_bull)::text, '-')
		FROM etfpair.combo_prices('P', 'binance', $1, $2, 1000) ORDER BY ts_ms`
	if from := timedLines(t, st, prices, first, c+5*minuteMs); len(from) != 15 {
		t.Fatalf("combo_prices from the first row: %q", from)
	} else {
		expectLines(t, "combo_prices from two years before the close", timedLines(t, st, prices, c-730*24*60*minuteMs, c+5*minuteMs), from...)
	}
	expectLines(t, "a market without rows", timedLines(t, st, `SELECT ts_ms::text FROM etfpair.combo_range('Z', 'bitget', 'sma', 2, $1, $2, 1000, 10000, 10)`,
		c-730*24*60*minuteMs, c+5*minuteMs))
}

// sameAfterRefresh runs the queries, aggregates perp_second_minute over (from, to] (the rows are long past, so every
// minute's row is final) and runs them again: the minute bars then come from the rows' end-row columns instead of
// perp_second, and every figure must stay the same.
func sameAfterRefresh(t *testing.T, st *store, from, to int64, queries ...func() []string) {
	t.Helper()
	var before [][]string
	for _, q := range queries {
		before = append(before, q())
	}
	if n, err := st.refreshCoverage(context.Background(), from, to); err != nil || n == 0 {
		t.Fatalf("refresh (%d, %d]: wrote %d %v", from, to, n, err)
	}
	for i, q := range queries {
		if after := q(); !slices.Equal(before[i], after) {
			t.Fatalf("query %d changed once the minutes were aggregated:\nbefore %q\nafter  %q", i, before[i], after)
		}
	}
}

// A split of a leg (BEAR, 1-for-10, ex 2026-11-20 on Bitget): its candle closes stop changing after 20:00 New York of
// 11-19 (the halt from 20:01) and are on the new basis from 20:10 (T_v); BEAR's anchor is 5 before and 50 from T_v.
// The recorder's rows from T_v still hold the book frozen before the halt and the old trade: such a book and such a
// trade do not count, so 20:10:01 has no point, 20:10:05 and 20:11:00 (a new trade, the frozen book) are points on trade
// prices, 20:11:06 (a new book) on mid prices; the halt's rows have no point; and minute 20:10's bar is the trade prices
// at 20:11:00, not the frozen book's mid (which would put the mean of 20:11 near -8,990 bps).
func TestLegSplitOnDatabase(t *testing.T) {
	st := scratchStore(t)
	ny := func(s string) int64 { return nyTime(t, "2026-11-19 "+s).UnixMilli() }
	threeTwoBoard(t, st, Combo{ID: "S", BullTicker: "BULL", BearTicker: "BEAR", RefTicker: "REF",
		Markets: []Market{{Venue: "bitget", BullSymbol: "BULLUSDT", BearSymbol: "BEARUSDT", RefSymbol: "REFUSDT"}}}, ny("19:50:00"), ny("20:20:00"))
	close19, _ := Calendar{}.closeOf("2026-11-19")
	for ticker, price := range map[string]string{"BULL": "40", "BEAR": "5", "REF": "300"} {
		dbExec(t, st, `INSERT INTO etfpair.etf_bell VALUES ($1, '2026-11-19', $2::numeric, $2::numeric, 'binance', 1, $3, 1000)`, ticker, price, close19.UnixMilli()-minuteMs)
	}
	dbExec(t, st, `INSERT INTO etfpair.etf_action VALUES ('BEAR', '2026-11-20', 'split', 0.1, 'Known', 'test', 'u', 1)`)
	for at := ny("19:55:00"); at <= ny("20:15:00"); at += minuteMs {
		bear := "5.05"
		switch {
		case at < ny("20:00:00"):
			bear = fmt.Sprintf("%.2f", 5+0.01*float64((at-ny("19:55:00"))/minuteMs))
		case at >= ny("20:10:00"):
			bear = "50.6"
		}
		dbExec(t, st, `INSERT INTO etfpair.perp_minute VALUES ('bitget', 'BULLUSDT', $1, 40, 0), ('bitget', 'BEARUSDT', $1, $2::numeric, 0)`, at, bear)
	}
	k1 := "{1000}"
	for _, s := range []string{"20:00:30", "20:05:00", "20:10:01", "20:10:05", "20:11:00", "20:11:06"} {
		perpSecond(t, st, "bitget", "BULLUSDT", ny(s), "40.00", "{39.99}", k1, "{40.01}", k1)
	}
	frozenTrade, frozenBook := ny("20:00:29")+700, ny("20:00:29")+900
	perpSecond(t, st, "bitget", "BEARUSDT", ny("20:00:30"), "5.05", "{5.04}", k1, "{5.06}", k1, frozenTrade, frozenBook)
	perpSecond(t, st, "bitget", "BEARUSDT", ny("20:05:00"), "5.05", "{5.04}", k1, "{5.06}", k1, frozenTrade, frozenBook)
	perpSecond(t, st, "bitget", "BEARUSDT", ny("20:10:01"), "5.05", "{5.04}", k1, "{5.06}", k1, frozenTrade, frozenBook)
	perpSecond(t, st, "bitget", "BEARUSDT", ny("20:10:05"), "50.7", "{5.04}", k1, "{5.06}", k1, ny("20:10:04"), frozenBook)
	perpSecond(t, st, "bitget", "BEARUSDT", ny("20:11:00"), "50.8", "{5.04}", k1, "{5.06}", k1, ny("20:10:50"), frozenBook)
	perpSecond(t, st, "bitget", "BEARUSDT", ny("20:11:06"), "50.8", "{50.70}", k1, "{50.90}", k1, ny("20:10:50"), ny("20:11:05"))
	const when = "to_char(to_timestamp(ts_ms / 1000.0) AT TIME ZONE 'America/New_York', 'HH24:MI:SS')"
	got := dbLines(t, st, `SELECT CASE WHEN basis IS NULL THEN 'break' ELSE `+when+` || ' ' || basis || ' s ' || round(s_bps, 4) || ' mu ' ||
		coalesce(round(mu_bps, 4)::text, '-') || ' long ' || coalesce(round(s_long_bps, 4)::text, '-') || ' short ' || coalesce(round(s_short_bps, 4)::text, '-') ||
		' books ' || book_bull || '/' || book_bear || ' ' || signal END
		FROM etfpair.combo_range('S', 'bitget', 'sma', 1, $1, $2, 1000, 10000, 10) ORDER BY ts_ms`, ny("20:00:00"), ny("20:11:10"))
	expectLines(t, "combo_range", got,
		"20:00:30 mid s 100.0000 mu 80.0000 long 122.5000 short 77.5000 books true/true None", "break",
		"20:10:05 trade s 140.0000 mu - long - short - books true/false No Mean", "break",
		"20:11:00 trade s 160.0000 mu - long - short - books true/false No Mean", "break",
		"20:11:06 mid s 160.0000 mu 160.0000 long 182.5000 short 137.5000 books true/true None")
	got = dbLines(t, st, `SELECT CASE WHEN p_bull IS NULL THEN 'break' ELSE `+when+` || ' ' || trim_scale(p_bear) || ' a ' || trim_scale(a_bear) END
		FROM etfpair.combo_prices('S', 'bitget', $1, $2, 1000) ORDER BY ts_ms`, ny("20:00:00"), ny("20:11:10"))
	expectLines(t, "combo_prices", got, "20:00:30 5.05 a 5", "break", "20:10:05 50.7 a 50", "break", "20:11:00 50.8 a 50", "break", "20:11:06 50.8 a 50")
	// Minute 20:10's row in perp_second_minute keeps the frozen book's mid at 20:11:00 (5.05, a usable book leaving the split
	// aside) with its book_ms; the bar still takes the trade prices, because that book_ms is before T_v.
	sameAfterRefresh(t, st, ny("19:50:00"), ny("20:20:00"), func() []string {
		return dbLines(t, st, `SELECT ts_ms || ' ' || coalesce(basis, '-') || ' ' || coalesce(s_bps::text, '-') || ' ' || coalesce(mu_bps::text, '-') || ' ' ||
			coalesce(signal, '-') FROM etfpair.combo_range('S', 'bitget', 'sma', 1, $1, $2, 1000, 10000, 10) ORDER BY ts_ms`, ny("20:00:00"), ny("20:11:10"))
	})
	expectLines(t, "minute 20:10's end row", dbLines(t, st, `SELECT trim_scale(end_price) || ' ' || trim_scale(end_mid) || ' ' || (end_book_ms = $2)::text
		FROM etfpair.perp_second_minute WHERE symbol = 'BEARUSDT' AND minute_ms = $1`, ny("20:10:00"), frozenBook), "50.8 5.05 true")
}

// A minute's bars and Data Coverage counts come from perp_second_minute only once the minute began at least 66 minutes
// ago, when the collector's last refresh of it (it refreshes the last 65 minutes every minute) has every row the
// recorder wrote late; a newer minute is read from perp_second. A session from three hours ago (anchors 40 / 60), SMA of
// one minute; for the minute that began at least 66.5 minutes ago and the one that began at most 65.5 minutes ago (30 s
// on each side of the boundary, so the test pins it) both legs' rows at the minute's end hold trades only (40 and 60:
// mu 0 bps) when the minutes are aggregated (final rows); then their books arrive (mids 40.40 and 60.30: mu 150 bps)
// and a row at second 30 of BULL. The recent minute reads them at once (mu 150, 2 seconds) although its row in the
// summary is final and has no mid; the old one reads its row in the summary (mu 0, 1 second) until the next refresh.
// The collector's refresh window ends inside the 66 minutes, so its last refresh of a minute comes before the minute is
// read from the summary.
func TestRecentMinutesFromPerSecondRowsOnDatabase(t *testing.T) {
	if coverageWindow+coverageAt >= 66*time.Minute {
		t.Fatalf("the collector refreshes %v back at %v into a minute: not inside the 66 minutes the board reads from perp_second", coverageWindow, coverageAt)
	}
	st := scratchStore(t)
	ctx := context.Background()
	cfg := validConfig()
	cfg.Combos = []Combo{{ID: "R", BullTicker: "BULL", BearTicker: "BEAR", RefTicker: "REF",
		Markets: []Market{{Venue: "bitget", BullSymbol: "BULLUSDT", BearSymbol: "BEARUSDT", RefSymbol: "REFUSDT"}}}}
	if err := st.seed(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	closeMs := (now - 180*minuteMs) / minuteMs * minuteMs
	dbExec(t, st, `INSERT INTO etfpair.etf_session VALUES ('2026-01-05', $1)`, closeMs)
	for ticker, price := range map[string]string{"BULL": "40", "BEAR": "60", "REF": "300"} {
		dbExec(t, st, `INSERT INTO etfpair.etf_bell VALUES ($1, '2026-01-05', $2::numeric, $2::numeric, 'binance', 1, $3, 1000)`, ticker, price, closeMs-minuteMs)
	}
	dbExec(t, st, `INSERT INTO etfpair.fx_minute (pair, minute_ms, price, source, sampled_ms)
		SELECT 'USDT-USD', m, 1, 'candle', 0 FROM generate_series($1::bigint, $2::bigint, 60000) m`, closeMs, now/minuteMs*minuteMs)
	old := (now - 66*minuteMs - 30_000) / minuteMs * minuteMs                   // began at least 66.5 minutes ago
	recent := (now - 66*minuteMs + 30_000 + minuteMs - 1) / minuteMs * minuteMs // began at most 65.5 minutes ago
	for _, j := range []int64{old, recent} {
		perpSecond(t, st, "bitget", "BULLUSDT", j+minuteMs, "40", nil, nil, nil, nil)
		perpSecond(t, st, "bitget", "BEARUSDT", j+minuteMs, "60", nil, nil, nil, nil)
		perpSecond(t, st, "bitget", "BULLUSDT", j+90_000, "40.01", "{40.00}", "{1000}", "{40.02}", "{1000}")
		perpSecond(t, st, "bitget", "BEARUSDT", j+90_000, "60", "{59.99}", "{1000}", "{60.01}", "{1000}")
	}
	if _, err := st.refreshCoverage(ctx, old-minuteMs, now); err != nil {
		t.Fatal(err)
	}
	for _, j := range []int64{old, recent} {
		for symbol, book := range map[string][2]string{"BULLUSDT": {"{40.39}", "{40.41}"}, "BEARUSDT": {"{60.29}", "{60.31}"}} {
			dbExec(t, st, `UPDATE etfpair.perp_second SET bid_px = $3::numeric[], bid_sz = '{10}', ask_px = $4::numeric[], ask_sz = '{10}', book_ms = second_ms - 100
				WHERE venue = 'bitget' AND symbol = $1 AND second_ms = $2`, symbol, j+minuteMs, book[0], book[1])
		}
		perpSecond(t, st, "bitget", "BULLUSDT", j+30_000, "40", nil, nil, nil, nil)
	}
	board := func(j int64) string {
		t.Helper()
		mean := dbLines(t, st, `SELECT coalesce(round(mu_bps, 4)::text, '-') FROM etfpair.combo_range('R', 'bitget', 'sma', 1, $1, $2, 1000, 10000, 10)
			WHERE basis IS NOT NULL`, j+minuteMs, j+2*minuteMs)
		seconds := dbLines(t, st, `SELECT seconds::text FROM etfpair.perp_coverage('bitget', 'BULLUSDT', $1, $2)`, j, j+minuteMs)
		summary := dbLines(t, st, `SELECT coalesce(trim_scale(end_mid)::text, '-') || ' ' || seconds || ' ' || etfpair.perp_minute_final(minute_ms, refreshed_ms)
			FROM etfpair.perp_second_minute WHERE venue = 'bitget' AND symbol = 'BULLUSDT' AND minute_ms = $1`, j)
		return "mu " + strings.Join(mean, ",") + " seconds " + strings.Join(seconds, ",") + " summary " + strings.Join(summary, ",")
	}
	expectLines(t, "after the late rows", []string{board(old), board(recent)},
		"mu 0.0000 seconds 1 summary - 1 true", "mu 150.0000 seconds 2 summary - 1 true")
	if _, err := st.refreshCoverage(ctx, old-minuteMs, now); err != nil {
		t.Fatal(err)
	}
	expectLines(t, "after the next refresh", []string{board(old), board(recent)},
		"mu 150.0000 seconds 2 summary 40.4 2 true", "mu 150.0000 seconds 2 summary 40.4 2 true")
}

// A database whose perp_second_minute is the table from before its end-row columns (eight columns) takes the schema: the
// columns are added, NULL in the rows on record, and the next refresh of their minute fills them.
func TestSchemaAddsTheEndRowColumns(t *testing.T) {
	st := scratchStore(t)
	ctx := context.Background()
	if err := st.seed(ctx, validConfig()); err != nil {
		t.Fatal(err)
	}
	dbExec(t, st, `ALTER TABLE etfpair.perp_second_minute DROP COLUMN end_price, DROP COLUMN end_trade_ms, DROP COLUMN end_mid, DROP COLUMN end_book_ms`)
	m := time.Now().Add(-2*time.Hour).UnixMilli() / minuteMs * minuteMs
	perpSecond(t, st, "bitget", "TQQQUSDT", m+minuteMs, "5", nil, nil, nil, nil)
	dbExec(t, st, `INSERT INTO etfpair.perp_second_minute VALUES ('bitget', 'TQQQUSDT', $1, 1, 0, NULL, NULL, $1::bigint + 70000)`, m)
	if _, err := st.pool.Exec(ctx, schemaSQL, pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatal(err)
	}
	row := `SELECT coalesce(trim_scale(end_price)::text, '-') || ' ' || coalesce((minute_ms + 60000 - end_trade_ms)::text, '-')
		FROM etfpair.perp_second_minute WHERE symbol = 'TQQQUSDT' AND minute_ms = $1`
	expectLines(t, "the row on record", dbLines(t, st, row, m), "- -")
	if n, err := st.refreshCoverage(ctx, m, m+minuteMs); err != nil || n != 1 {
		t.Fatalf("refresh: wrote %d %v", n, err)
	}
	expectLines(t, "the row refreshed", dbLines(t, st, row, m), "5 300")
}

// Two refreshes of one minute run one after the other: a refresh inside a backfill file's open transaction, which wrote a
// row of the minute, holds the table lock until that transaction ends, and a second refresh then aggregates with a
// snapshot that has the row. Without the lock the second would write the minute's older count over the newer one, and
// the row would be final with it for good. A session holding the advisory lock the refresh took before (any reader may
// take one) does not hold a refresh up: the table lock in its mode needs a write privilege on the table.
func TestRefreshOneAtATime(t *testing.T) {
	st := scratchStore(t)
	ctx := context.Background()
	if err := st.seed(ctx, validConfig()); err != nil {
		t.Fatal(err)
	}
	m := time.Now().Add(-2*time.Hour).UnixMilli() / minuteMs * minuteMs
	perpSecond(t, st, "bitget", "TQQQUSDT", m+1000, "5", nil, nil, nil, nil)
	held, err := st.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	if _, err := held.Exec(ctx, `SELECT pg_advisory_lock(hashtext('etfpair.perp_second_minute'))`); err != nil {
		t.Fatal(err)
	}
	quick, cancel := context.WithTimeout(ctx, 2*time.Second)
	n, err := st.refreshCoverage(quick, m, m+minuteMs)
	cancel()
	if _, err := held.Exec(ctx, `SELECT pg_advisory_unlock_all()`); err != nil {
		t.Fatal(err)
	}
	if err != nil || n != 1 {
		t.Fatalf("first refresh, with the advisory lock held elsewhere: wrote %d %v", n, err)
	}
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `INSERT INTO etfpair.perp_second (venue, symbol, second_ms, price, trade_ms) VALUES ('bitget', 'TQQQUSDT', $1, 5, $2)`, m+2000, m+1700); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT etfpair.refresh_perp_second_minute($1, $2)`, m, m+minuteMs); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := st.refreshCoverage(ctx, m, m+minuteMs)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("the second refresh did not wait for the first one's transaction (%v)", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	expectLines(t, "the minute's count", dbLines(t, st, `SELECT seconds::text FROM etfpair.perp_second_minute WHERE symbol = 'TQQQUSDT' AND minute_ms = $1`, m), "2")
}

// The collector applies the schema in one transaction at its start (combo_market's ALTER near the top, the summary's at
// the end) while a backfill file's transaction may be inside a refresh. A reader holds combo_market, so the schema waits
// for it there; a refresh starts meanwhile, then the reader ends: the refresh locks combo_market before the summary,
// the order of the schema, so both complete. With the summary locked first the refresh would hold it and wait for
// combo_market behind the schema, which then waits for the summary: deadlock.
func TestRefreshAndSchemaLockOrder(t *testing.T) {
	st := scratchStore(t)
	ctx := context.Background()
	if err := st.seed(ctx, validConfig()); err != nil {
		t.Fatal(err)
	}
	m := time.Now().Add(-2*time.Hour).UnixMilli() / minuteMs * minuteMs
	perpSecond(t, st, "bitget", "TQQQUSDT", m+1000, "5", nil, nil, nil, nil)
	reader, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Rollback(ctx)
	if _, err := reader.Exec(ctx, `SELECT count(*) FROM etfpair.combo_market`); err != nil {
		t.Fatal(err)
	}
	schema, refresh := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := st.pool.Exec(ctx, schemaSQL, pgx.QueryExecModeSimpleProtocol)
		schema <- err
	}()
	time.Sleep(300 * time.Millisecond)
	go func() {
		tx, err := st.pool.Begin(ctx)
		if err == nil {
			defer tx.Rollback(ctx)
			if _, err = tx.Exec(ctx, `SELECT etfpair.refresh_perp_second_minute($1, $2)`, m, m+minuteMs); err == nil {
				err = tx.Commit(ctx)
			}
		}
		refresh <- err
	}()
	time.Sleep(300 * time.Millisecond)
	if err := reader.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for name, ch := range map[string]chan error{"schema": schema, "refresh": refresh} {
		select {
		case err := <-ch:
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		case <-time.After(15 * time.Second):
			t.Fatalf("%s did not finish", name)
		}
	}
	expectLines(t, "the minute", dbLines(t, st, `SELECT seconds::text FROM etfpair.perp_second_minute WHERE symbol = 'TQQQUSDT' AND minute_ms = $1`, m), "1")
}

// A minute whose rows are all gone (an undone backfill) loses its row in perp_second_minute at the next refresh of a range
// that holds it; rows outside the range stay.
func TestRefreshDropsEmptyMinutes(t *testing.T) {
	st := scratchStore(t)
	ctx := context.Background()
	if err := st.seed(ctx, validConfig()); err != nil {
		t.Fatal(err)
	}
	m := time.Now().Add(-2*time.Hour).UnixMilli() / minuteMs * minuteMs
	for i := range int64(3) {
		perpSecond(t, st, "bitget", "TQQQUSDT", m+i*minuteMs+1000, "5", nil, nil, nil, nil)
	}
	refresh := func(from, to int64, want int) {
		t.Helper()
		if n, err := st.refreshCoverage(ctx, from, to); err != nil || n != want {
			t.Fatalf("refresh (%d, %d]: wrote or deleted %d %v, want %d", from, to, n, err, want)
		}
	}
	minutes := `SELECT string_agg(((minute_ms - $1) / 60000)::text, ' ' ORDER BY minute_ms) FROM etfpair.perp_second_minute WHERE symbol = 'TQQQUSDT'`
	refresh(m, m+3*minuteMs, 3)
	dbExec(t, st, `DELETE FROM etfpair.perp_second WHERE symbol = 'TQQQUSDT' AND second_ms IN ($1, $2)`, m+1000, m+2*minuteMs+1000)
	refresh(m+minuteMs, m+3*minuteMs, 1) // minute 2 goes; minute 0 is outside the range and stays for now
	expectLines(t, "after the first refresh", dbLines(t, st, minutes, m), "0 1")
	refresh(m-minuteMs, m+minuteMs, 1)
	expectLines(t, "after the second refresh", dbLines(t, st, minutes, m), "1")
}

// Data Coverage's counts (DESIGN 3.3) equal those of counting perp_second (3.1's query) over (from, to]: with an empty
// minute table, with whole minutes read from it, at partial minutes at both ends and inside one minute, and for an empty
// range. A minute's row is final once aggregated at least 10 s after the minute ended: a row that arrives later (the
// recorder's held-back rows) is counted once the next refresh has rewritten the minute (the minute here is two hours
// old; the last 66 minutes are always counted in perp_second, TestRecentMinutesFromPerSecondRowsOnDatabase); a minute
// aggregated before it ended, and one whose row is not final, are counted in perp_second; the refresh writes only what
// changed or became final, in key order, and returns how many rows it wrote.
func TestCoverageOnDatabase(t *testing.T) {
	st := scratchStore(t)
	ctx := context.Background()
	if err := st.seed(ctx, validConfig()); err != nil { // TQQQ, SQQQ, QQQ on bitget and binance
		t.Fatal(err)
	}
	m := time.Now().Add(-2*time.Hour).UnixMilli() / minuteMs * minuteMs
	// Minute m: 59 seconds with a price (m + 30 s is missing), books of 5 levels a side on the seconds 1 to 40 (39 of
	// them), 3 bids and 5 asks on the 41st, and 4 seconds of the next minute.
	for i := int64(1); i <= 60; i++ {
		if i == 30 {
			continue
		}
		var bids, asks any
		switch {
		case i <= 40:
			bids, asks = "{5,4,3,2,1}", "{6,7,8,9,10}"
		case i == 41:
			bids, asks = "{5,4,3}", "{6,7,8,9,10}"
		}
		perpSecond(t, st, "bitget", "TQQQUSDT", m+i*1000, "5", bids, bids, asks, asks)
	}
	for i := int64(1); i <= 4; i++ {
		perpSecond(t, st, "bitget", "TQQQUSDT", m+minuteMs+i*1000, "5", nil, nil, nil, nil)
	}
	ranges := [][2]int64{{m, m + minuteMs}, {m + 500, m + 90_000}, {m + 1000, m + 59_000}, {m, m}, {m - minuteMs, m + 3*minuteMs}, {m + 60_000, m + 61_000}}
	counts := func(name string, want ...string) {
		t.Helper()
		var got []string
		for _, r := range ranges {
			got = append(got, dbLines(t, st, `SELECT c.seconds || ' ' || c.books || ' ' || coalesce(c.least_levels::text, '-') || ' ' || coalesce(c.most_levels::text, '-') ||
				CASE WHEN (c.seconds, c.books, c.least_levels, c.most_levels) IS DISTINCT FROM (d.seconds, d.books, d.least_levels, d.most_levels) THEN ' NOT perp_second' ELSE '' END
				FROM etfpair.perp_coverage('bitget', 'TQQQUSDT', $1, $2) c,
				LATERAL (SELECT count(t.price) AS seconds, count(*) FILTER (WHERE t.bid_px[1] IS NOT NULL AND t.ask_px[1] IS NOT NULL) AS books,
				                min(least(cardinality(t.bid_px), cardinality(t.ask_px))) FILTER (WHERE t.bid_px[1] IS NOT NULL AND t.ask_px[1] IS NOT NULL) AS least_levels,
				                max(greatest(cardinality(t.bid_px), cardinality(t.ask_px))) FILTER (WHERE t.bid_px[1] IS NOT NULL AND t.ask_px[1] IS NOT NULL) AS most_levels
				         FROM etfpair.perp_second t WHERE t.venue = 'bitget' AND t.symbol = 'TQQQUSDT' AND t.second_ms > $1 AND t.second_ms <= $2) d`, r[0], r[1])...)
		}
		expectLines(t, name, got, want...)
	}
	all := []string{"59 40 3 5", "63 40 3 5", "57 39 3 5", "0 0 - -", "63 40 3 5", "1 0 - -"}
	counts("an empty table", all...)
	refresh := func(from, to int64, want int) {
		t.Helper()
		if n, err := st.refreshCoverage(ctx, from, to); err != nil || n != want {
			t.Fatalf("refresh (%d, %d]: wrote %d %v, want %d", from, to, n, err, want)
		}
	}
	refresh(m-minuteMs, m+2*minuteMs, 2) // the minutes m and m + 1 min of the one contract with rows
	counts("whole minutes from the table", all...)
	refresh(m-minuteMs, m+2*minuteMs, 0)
	expectLines(t, "the minute rows", dbLines(t, st, `SELECT symbol || ' ' || (minute_ms - $1) / 60000 || ' ' || seconds || ' ' || books || ' ' || least_levels || ' ' ||
		most_levels FROM etfpair.perp_second_minute WHERE books > 0 ORDER BY venue, symbol, minute_ms`, m), "TQQQUSDT 0 59 40 3 5")
	// The late second m + 30 s: the minute's final row does not have it until the next refresh rewrites the minute.
	perpSecond(t, st, "bitget", "TQQQUSDT", m+30_000, "5", nil, nil, nil, nil)
	got := dbLines(t, st, `SELECT seconds::text FROM etfpair.perp_coverage('bitget', 'TQQQUSDT', $1, $2)`, m, m+minuteMs)
	expectLines(t, "a late row before the next refresh", got, "59")
	refresh(m-minuteMs, m+2*minuteMs, 1)
	counts("after the late row's refresh", "60 40 3 5", "64 40 3 5", "58 39 3 5", "0 0 - -", "64 40 3 5", "1 0 - -")
	// A row that is not final (aggregated 30 s after the minute began) is not read; a refresh makes it final although its
	// counts are the same.
	dbExec(t, st, `UPDATE etfpair.perp_second_minute SET refreshed_ms = minute_ms + 30000 WHERE symbol = 'TQQQUSDT' AND minute_ms = $1`, m)
	dbExec(t, st, `DELETE FROM etfpair.perp_second WHERE symbol = 'TQQQUSDT' AND second_ms = $1`, m+30_000)
	counts("a row not final", all...)
	refresh(m-minuteMs, m+2*minuteMs, 1)
	if got := dbLines(t, st, `SELECT (refreshed_ms >= minute_ms + 70000)::text FROM etfpair.perp_second_minute WHERE symbol = 'TQQQUSDT' AND minute_ms = $1`, m); !slices.Equal(got, []string{"true"}) {
		t.Fatalf("the minute's row is not final after the refresh: %v", got)
	}
	// The minute now running is aggregated before it ends: its row is not final and the counts come from perp_second.
	now := time.Now().UnixMilli()
	run := now / minuteMs * minuteMs
	perpSecond(t, st, "bitget", "SQQQUSDT", run+1000, "7", nil, nil, nil, nil)
	refresh(now-coverageWindow.Milliseconds(), now, 1)
	perpSecond(t, st, "bitget", "SQQQUSDT", run+2000, "7", nil, nil, nil, nil)
	expectLines(t, "the running minute", dbLines(t, st, `SELECT seconds::text FROM etfpair.perp_coverage('bitget', 'SQQQUSDT', $1, $2)`, run, run+minuteMs), "2")
}

// The collector refreshes the minute counts at start for the last 65 minutes, not further back, and then 20 s into every
// minute; it stops with its context.
func TestRefreshCoverageLoop(t *testing.T) {
	for at, want := range map[string]time.Duration{"12:00:00.000": 20 * time.Second, "12:00:19.999": time.Millisecond, "12:00:20.000": time.Minute,
		"12:00:30.000": 50 * time.Second, "12:00:59.999": 20*time.Second + time.Millisecond} {
		now, err := time.Parse("2006-01-02 15:04:05.000", "2026-11-23 "+at)
		if err != nil {
			t.Fatal(err)
		}
		if got := untilCoverage(now); got != want {
			t.Fatalf("at %s: wait %v, want %v", at, got, want)
		}
	}
	st := scratchStore(t)
	if err := st.seed(context.Background(), validConfig()); err != nil {
		t.Fatal(err)
	}
	recent := time.Now().Add(-20*time.Minute).UnixMilli() / minuteMs * minuteMs
	old := time.Now().Add(-70*time.Minute).UnixMilli() / minuteMs * minuteMs
	perpSecond(t, st, "binance", "QQQUSDT", recent+1000, "500", nil, nil, nil, nil)
	perpSecond(t, st, "binance", "QQQUSDT", old+1000, "500", nil, nil, nil, nil)
	m := &Manager{store: st, log: slog.New(slog.NewTextHandler(io.Discard, nil)), failing: map[string]bool{}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.refreshCoverage(ctx)
		close(done)
	}()
	query := `SELECT (minute_ms - $1) / 60000 || ' ' || seconds FROM etfpair.perp_second_minute WHERE symbol = 'QQQUSDT' ORDER BY minute_ms`
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if len(dbLines(t, st, query, recent)) > 0 {
			break
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the refresh loop did not stop with its context")
	}
	expectLines(t, "minutes refreshed at start", dbLines(t, st, query, recent), "0 1")
}
