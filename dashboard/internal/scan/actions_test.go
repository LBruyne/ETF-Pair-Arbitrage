package scan

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func testdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func brief(actions []Action) []string {
	var out []string
	for _, a := range actions {
		out = append(out, strings.Join(slices.DeleteFunc([]string{a.Ticker, a.ExDate, a.Kind, a.Status, a.Value, a.Source}, func(f string) bool { return f == "" }), " "))
	}
	return out
}

// The sources' answers as saved on 2026-11-20 (testdata/README.txt): each
// parser gives the readings the collector puts on record.
func TestActionSourceParsers(t *testing.T) {
	got, err := parseProSharesDistributions(testdata(t, "proshares_distributions_TQQQ_2026.json"), "TQQQ", "u", 7)
	want := []string{"TQQQ 2026-09-23 dividend Known 0.156012 proshares-api", "TQQQ 2026-06-24 dividend Known 0.171229 proshares-api",
		"TQQQ 2026-03-25 dividend Known 0.071616 proshares-api"}
	if err != nil || !slices.Equal(brief(got), want) || got[0].ReadMs != 7 || got[0].URL != "u" {
		t.Fatalf("proshares distributions: %v %v", brief(got), err)
	}
	dates, err := parseProSharesSchedule(testdata(t, "proshares_distribution-schedule_excerpt.html"))
	if want := []string{"2026-03-25", "2026-06-24", "2026-09-23", "2026-12-23", "2026-12-31"}; err != nil || !slices.Equal(dates, want) {
		t.Fatalf("proshares schedule: %v %v", dates, err)
	}
	got, err = parseNasdaqDividends(testdata(t, "nasdaq_dividends_TQQQ.json"), "TQQQ", "u", 7)
	if err != nil || len(got) < 4 || brief(got)[0] != "TQQQ 2026-09-23 dividend Known 0.156012 nasdaq" {
		t.Fatalf("nasdaq TQQQ: %v %v", brief(got), err)
	}
	if got, err = parseNasdaqDividends(testdata(t, "nasdaq_dividends_SOXL.json"), "SOXL", "u", 7); err != nil || len(got) != 0 {
		t.Fatalf("nasdaq SOXL (NYSE Arca, no rows): %v %v", brief(got), err)
	}
	got, err = parseBitgetSplits(testdata(t, "bitget_split-records.json"), map[string]string{"SOXSUSDT": "SOXS", "SOXXUSDT": "SOXX", "SOXLUSDT": "SOXL"}, "u", 7)
	if want := []string{"SOXS 2026-07-15 split Known 0.1 bitget-split-records", "SOXX 2026-11-05 split Known 3 bitget-split-records"}; err != nil ||
		!slices.Equal(brief(got), want) {
		t.Fatalf("bitget split-records: %v %v", brief(got), err)
	}
	// Direxion's distributions as declared (SOXS's 06-23 is per pre-split share, the 1-for-10 of 07-15 after it), one reading
	// per ex-date with every part of the cash summed (SOXL 2016-12-20: a short-term gain only); SOXL paid nothing in 2026
	// before September, so its list has no round of 03-24 or 06-23.
	// The answer also gives the dates its data are as of.
	got, unfilled, stamp, trade, err := parseDirexionDistributions(testdata(t, "direxion_getKurtosysFunds_SOXS.json"), "SOXS", "u", 7)
	if want := []string{"SOXS 2026-09-22 dividend Known 0.43641 direxion-api", "SOXS 2026-06-23 dividend Known 0.0375 direxion-api",
		"SOXS 2026-03-24 dividend Known 0.28685 direxion-api"}; err != nil || len(got) < 3 || !slices.Equal(brief(got[:3]), want) || got[0].URL != "u" ||
		stamp != "11202026" || trade != "11192026" || len(unfilled) != 0 {
		t.Fatalf("direxion SOXS: %v %v %s %s %v", brief(got), unfilled, stamp, trade, err)
	}
	got, _, _, _, err = parseDirexionDistributions(testdata(t, "direxion_getKurtosysFunds_SOXL.json"), "SOXL", "u", 7)
	if err != nil || brief(got)[0] != "SOXL 2026-09-22 dividend Known 0.0929 direxion-api" || brief(got)[1] != "SOXL 2025-09-23 dividend Known 0.01008 direxion-api" ||
		!slices.Contains(brief(got), "SOXL 2016-12-20 dividend Known 2.76442 direxion-api") {
		t.Fatalf("direxion SOXL: %v %v", brief(got), err)
	}
	// A row of zeros is None; a row whose four amounts are all null is a round listed before its amounts: no reading, its
	// ex-date unfilled.
	got, unfilled, _, _, err = parseDirexionDistributions([]byte(`{"data":{"getKurtosysFunds":{"Distributions":[{"IncomeDividend":0.0,"ShortTermCapitalGain":null,`+
		`"LongTermCapitalGain":null,"ReturnOfCapital":null,"ExDate":"12222026"},{"IncomeDividend":null,"ShortTermCapitalGain":null,`+
		`"LongTermCapitalGain":null,"ReturnOfCapital":null,"ExDate":"03232027"}]}}}`), "SOXL", "u", 7)
	if err != nil || !slices.Equal(brief(got), []string{"SOXL 2026-12-22 dividend None direxion-api"}) || !slices.Equal(unfilled, []string{"2027-03-23"}) {
		t.Fatalf("direxion, a row of zeros and a row of nulls: %v %v %v", brief(got), unfilled, err)
	}
	for _, bad := range []string{`{"data":{"getKurtosysFunds":null}}`, `{"data":null,"errors":[{"errorType":"UnauthorizedException","message":"x"}]}`} {
		if _, _, _, _, err := parseDirexionDistributions([]byte(bad), "SOXL", "u", 7); err == nil {
			t.Fatalf("direxion %s: no error", bad)
		}
	}
	sessions, err := parseTradingSchedule(testdata(t, "binance_tradingSchedule.json"))
	if err != nil || len(sessions) != 42 || sessions[0] != (ScheduleSession{StartMs: 1794445200000, EndMs: 1794474000000, Type: "OVERNIGHT"}) {
		t.Fatalf("binance schedule: %d %+v %v", len(sessions), sessions[0], err)
	}
	rate, mark, ok, err := parseSpecialFunding(testdata(t, "binance_fundingRate_QQQUSDT.json"))
	if err != nil || !ok || rate != "-0.00103754" || mark != "724.24000000" {
		t.Fatalf("binance special funding: %s %s %v %v", rate, mark, ok, err)
	}
	if _, _, ok, _ = parseSpecialFunding([]byte(`[{"fundingRate":"0.0001","markPrice":"1","rateType":"Regular"}]`)); ok {
		t.Fatal("a regular entry is not the dividend's")
	}
}

// What goes on record: a source's first reading of an action, or one that
// says otherwise than its own newest row; a date-only reading only for an
// action without any row; after the ex-date a source keeps its first
// reading (ProShares restates past amounts after a split), the
// configuration does not.
func TestNewReadings(t *testing.T) {
	at := func(a Action, ms int64) Action { a.ReadMs = ms; return a }
	sqqq := Action{Ticker: "SQQQ", ExDate: "2026-09-23", Kind: "dividend", Value: "0.473413", Status: "Known", Source: "proshares-api"}
	dec := Action{Ticker: "TQQQ", ExDate: "2026-12-23", Kind: "dividend", Status: "Scheduled", Source: "proshares-schedule"}
	soxl := Action{Ticker: "SOXL", ExDate: "2026-09-22", Kind: "dividend", Value: "0.0929", Status: "Known", Source: "config"}
	stored := []Action{at(sqqq, 1), at(dec, 1), at(soxl, 1)}

	nasdaq := sqqq
	nasdaq.Source = "nasdaq"
	restated := sqqq
	restated.Value = "0.2367065"
	declared := Action{Ticker: "TQQQ", ExDate: "2026-12-23", Kind: "dividend", Value: "0.2", Status: "Known", Source: "proshares-api"}
	excise := Action{Ticker: "TQQQ", ExDate: "2026-12-31", Kind: "dividend", Status: "Scheduled", Source: "proshares-schedule"}
	exciseNone := excise
	exciseNone.Status, exciseNone.Source = "None", "proshares-api"
	corrected := soxl
	corrected.Value = "0.09290" // the same figure written otherwise: nothing new
	soxlTypo := soxl
	soxlTypo.Value = "0.0930"
	record, kept := newReadings(stored, []Action{at(excise, 2), at(dec, 2), at(sqqq, 2), at(nasdaq, 2), at(restated, 2), at(declared, 2),
		at(exciseNone, 2), at(corrected, 2), at(soxlTypo, 2)}, "2026-12-22")
	if want := []string{"SQQQ 2026-09-23 dividend Known 0.473413 nasdaq", "TQQQ 2026-12-23 dividend Known 0.2 proshares-api",
		"SOXL 2026-09-22 dividend Known 0.0930 config", "TQQQ 2026-12-31 dividend None proshares-api"}; !slices.Equal(brief(record), want) {
		t.Fatalf("record\n%v\nwant\n%v", brief(record), want)
	}
	if want := []string{"SQQQ 2026-09-23 dividend Known 0.2367065 proshares-api"}; !slices.Equal(brief(kept), want) {
		t.Fatalf("kept %v", brief(kept))
	}
	// A None an issuer derives does not hide another source's amount (Nasdaq's TQQQ 12-31, on record or read in the same
	// pass), and the issuer's own amount after its None is recorded although the ex-date has passed.
	nasdaqExcise := Action{Ticker: "TQQQ", ExDate: "2026-12-31", Kind: "dividend", Value: "0.01", Status: "Known", Source: "nasdaq"}
	if record, _ = newReadings([]Action{at(nasdaqExcise, 1)}, []Action{at(exciseNone, 2)}, "2027-01-01"); len(record) != 0 {
		t.Fatalf("a derived None over an amount on record: %v", brief(record))
	}
	if record, _ = newReadings(nil, []Action{at(exciseNone, 2), at(nasdaqExcise, 2)}, "2027-01-01"); !slices.Equal(brief(record),
		[]string{"TQQQ 2026-12-31 dividend Known 0.01 nasdaq"}) {
		t.Fatalf("a derived None and an amount in one pass: %v", brief(record))
	}
	late := Action{Ticker: "TQQQ", ExDate: "2026-12-31", Kind: "dividend", Value: "0.01", Status: "Known", Source: "proshares-api"}
	if record, kept = newReadings([]Action{at(exciseNone, 1)}, []Action{at(late, 2)}, "2027-01-05"); !slices.Equal(brief(record),
		[]string{"TQQQ 2026-12-31 dividend Known 0.01 proshares-api"}) || len(kept) != 0 {
		t.Fatalf("the amount after the issuer's None: %v %v", brief(record), brief(kept))
	}
	// The operator's None stands over an amount: it is the configuration's word.
	configNone := Action{Ticker: "TQQQ", ExDate: "2026-12-31", Kind: "dividend", Status: "None", Source: "config"}
	if record, _ = newReadings([]Action{at(nasdaqExcise, 1)}, []Action{at(configNone, 2)}, "2027-01-01"); len(record) != 1 {
		t.Fatalf("the configuration's None: %v", brief(record))
	}
	// A date and its figure read in one pass: the figure only.
	record, _ = newReadings(nil, []Action{at(excise, 3), at(Action{Ticker: "TQQQ", ExDate: "2026-12-31", Kind: "dividend", Value: "0.01",
		Status: "Known", Source: "proshares-api"}, 3)}, "2026-12-22")
	if want := []string{"TQQQ 2026-12-31 dividend Known 0.01 proshares-api"}; !slices.Equal(brief(record), want) {
		t.Fatalf("one pass: %v", brief(record))
	}
}

// An issuer says an expected round has no distribution once the round is
// over and its list, read in full, has nothing for it.
func TestNoneReadings(t *testing.T) {
	url := proSharesDistributionsURL("TQQQ", 2026)
	expected := []Action{{Ticker: "TQQQ", ExDate: "2026-12-23", Kind: "dividend", Status: "Scheduled", Source: "proshares-api", URL: url},
		{Ticker: "TQQQ", ExDate: "2026-12-31", Kind: "dividend", Status: "Scheduled", Source: "proshares-api", URL: url}}
	declared := []Action{{Ticker: "TQQQ", ExDate: "2026-12-23", Kind: "dividend", Value: "0.2", Status: "Known"}}
	closed := func(now time.Time) func(string) bool {
		return func(exDate string) bool { c, err := (Calendar{}).closeOf(exDate); return err == nil && !now.Before(c) }
	}
	if got := noneReadings(expected, declared, nyTime(t, "2026-12-31 15:59:00"), closed(nyTime(t, "2026-12-31 15:59:00"))); len(got) != 0 {
		t.Fatalf("before the close: %v", brief(got))
	}
	got := noneReadings(expected, declared, nyTime(t, "2026-12-31 16:00:00"), closed(nyTime(t, "2026-12-31 16:00:00")))
	if want := []string{"TQQQ 2026-12-31 dividend None proshares-api"}; !slices.Equal(brief(got), want) ||
		got[0].URL != "https://www.proshares.com/api/distributionsummary?fund=TQQQ&year=2026" || got[0].ReadMs != nyTime(t, "2026-12-31 16:00:00").UnixMilli() {
		t.Fatalf("after the close: %v", got)
	}
}

// Overrides: a closed day is no trading day (its session folds into the one
// before), an HH:MM close moves the bell and After-Hours, which ends four
// hours after it.
func TestCalendarOverrides(t *testing.T) {
	cal := newCalendar([]CalendarOverride{{Date: "2027-01-08", Close: "closed", Source: "x"}, {Date: "2026-11-27", Close: "12:00", Source: "x"}})
	if got := cal.tradingDays(nyTime(t, "2027-01-07 00:00:00"), nyTime(t, "2027-01-11 00:00:00")); !slices.Equal(got, []string{"2027-01-07", "2027-01-11"}) {
		t.Fatalf("closed day: %v", got)
	}
	if got, err := cal.lastClosedSession(nyTime(t, "2027-01-08 18:00:00")); err != nil || got != "2027-01-07" {
		t.Fatalf("session on the closed day: %s %v", got, err)
	}
	if got, err := cal.closeOf("2026-11-27"); err != nil || !got.Equal(nyTime(t, "2026-11-27 12:00:00")) {
		t.Fatalf("moved close: %v %v", got, err)
	}
	if got, err := cal.nextBell(nyTime(t, "2026-11-25 17:00:00")); err != nil || !got.Equal(nyTime(t, "2026-11-27 12:01:00")) {
		t.Fatalf("bell after the moved close: %v %v", got, err)
	}
	phases, err := cal.phasesOf([]string{"2026-11-27", "2026-11-30"})
	if err != nil || phases[3] != (Phase{nyTime(t, "2026-11-27 12:00:00"), nyTime(t, "2026-11-27 16:00:00"), "After-Hours"}) ||
		phases[4].Name != "Closed" || !phases[4].Start.Equal(nyTime(t, "2026-11-27 16:00:00")) {
		t.Fatalf("phases: %v %v", phases[3:5], err)
	}
	// The carrier window: from the pre-market of the trading day before the ex-date (a holiday and a weekend skipped) to the
	// end of the ex-date.
	for ex, want := range map[string]string{"2026-12-22": "2026-12-21 04:00:00", "2026-11-27": "2026-11-25 04:00:00", "2026-11-30": "2026-11-27 04:00:00"} {
		from, to, err := (Calendar{}).carrierWindow(ex)
		if err != nil || !from.Equal(nyTime(t, want)) || !to.Equal(nyTime(t, ex+" 00:00:00").AddDate(0, 0, 1)) {
			t.Fatalf("carrier window of %s: %v %v %v, want from %s", ex, from.In(newYork), to.In(newYork), err, want)
		}
	}
	// Direxion's declaration window: from the carrier day's regular close (13:00 on 11-27) to the end of the first trading day
	// after the ex-date (12-24 after 12-23; 12-28 after the 12-24 early close and Christmas).
	for ex, want := range map[string][2]string{"2026-12-22": {"2026-12-21 16:00:00", "2026-12-24 00:00:00"}, "2026-11-30": {"2026-11-27 13:00:00", "2026-12-02 00:00:00"},
		"2026-12-24": {"2026-12-23 16:00:00", "2026-12-29 00:00:00"}, "2027-03-23": {"2027-03-22 16:00:00", "2027-03-25 00:00:00"}} {
		from, to, err := (Calendar{}).declarationWindow(ex)
		if err != nil || !from.Equal(nyTime(t, want[0])) || !to.Equal(nyTime(t, want[1])) {
			t.Fatalf("declaration window of %s: %v %v %v, want %v", ex, from.In(newYork), to.In(newYork), err, want)
		}
	}
}

// When the sources are read at every tick: the sources but Direxion in the
// carrier window of a Scheduled action (SOXX's date from the configuration:
// Nasdaq gives its amounts), Direxion for a Scheduled dividend of its funds
// in its declaration window, from the carrier day's close to the end of the
// trading day after the ex-date, one request per fund still without an
// amount; neither once the amount is on record, and both once a day.
func TestActionsDueOnDatabase(t *testing.T) {
	st := scratchStore(t)
	ctx := context.Background()
	cfg := validConfig()
	cfg.Combos = append(cfg.Combos, Combo{ID: "SOXL-SOXS", BullTicker: "SOXL", BearTicker: "SOXS", RefTicker: "SOXX", NAVSource: "direxion",
		Markets: []Market{{Venue: "binance", BullSymbol: "SOXLUSDT", BearSymbol: "SOXSUSDT", RefSymbol: "SOXXUSDT"}}})
	m := &Manager{cfg: cfg, store: st, log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), told: map[string]bool{}, failing: map[string]bool{}}
	insert := func(a Action) {
		t.Helper()
		if err := st.insertActions(ctx, []Action{a}); err != nil {
			t.Fatal(err)
		}
	}
	insert(Action{Ticker: "SOXX", ExDate: "2026-09-15", Kind: "dividend", Value: "0.325046", Status: "Known", Source: "nasdaq", URL: "u", ReadMs: 1})
	insert(Action{Ticker: "SOXX", ExDate: "2026-12-15", Kind: "dividend", Status: "Scheduled", Source: "config", URL: "u", ReadMs: 1})
	insert(Action{Ticker: "SOXL", ExDate: "2026-09-22", Kind: "dividend", Value: "0.0929", Status: "Known", Source: "direxion-api", URL: "u", ReadMs: 1})
	for _, ticker := range []string{"SOXL", "SOXS"} {
		insert(Action{Ticker: ticker, ExDate: "2026-12-22", Kind: "dividend", Status: "Scheduled", Source: "config", URL: "u", ReadMs: 1})
	}
	due := func(at string) string {
		now := nyTime(t, at)
		m.actionsRead, m.direxionRead = now, now // the daily reads are not due
		funds, daily := m.direxionDue(ctx, now)
		return fmt.Sprint(m.actionsDue(ctx, now), funds, daily)
	}
	for _, c := range [][2]string{
		{"2026-12-14 03:59:00", "false [] false"}, // before SOXX's carrier day opens
		{"2026-12-14 04:00:00", "true [] false"},  // SOXX: declared after the close of 12-14, effective at 20:00
		{"2026-12-15 23:59:00", "true [] false"},
		{"2026-12-16 00:00:00", "false [] false"}, // the ex-date is over: the daily read only
		{"2026-12-21 05:00:00", "false [] false"}, // SOXL and SOXS come from Direxion, from the close only
		{"2026-12-21 15:59:00", "false [] false"},
		{"2026-12-21 16:00:00", "false [SOXL SOXS] false"},
		{"2026-12-22 23:59:00", "false [SOXL SOXS] false"},
		{"2026-12-23 23:59:00", "false [SOXL SOXS] false"}, // the API trails the declaration: to the end of the next trading day
		{"2026-12-24 00:00:00", "false [] false"},
	} {
		if got := due(c[0]); got != c[1] {
			t.Fatalf("due at %s: %s, want %s", c[0], got, c[1])
		}
	}
	insert(Action{Ticker: "SOXX", ExDate: "2026-12-15", Kind: "dividend", Value: "0.3", Status: "Known", Source: "nasdaq", URL: "u", ReadMs: 2})
	insert(Action{Ticker: "SOXS", ExDate: "2026-12-22", Kind: "dividend", Value: "0.5", Status: "Known", Source: "direxion-api", URL: "u", ReadMs: 2})
	if got := due("2026-12-14 20:00:00"); got != "false [] false" {
		t.Fatalf("due once SOXX's amount is on record: %s", got)
	}
	if got := due("2026-12-21 18:00:00"); got != "false [SOXL] false" {
		t.Fatalf("due once SOXS's amount is on record: %s", got)
	}
	// Once a day: every source, Direxion for both funds.
	m.actionsRead, m.direxionRead = time.Time{}, time.Time{}
	d, daily := m.direxionDue(ctx, nyTime(t, "2026-11-30 10:00:00"))
	if a := m.actionsDue(ctx, nyTime(t, "2026-11-30 10:00:00")); !a || !slices.Equal(d, []string{"SOXL", "SOXS"}) || !daily {
		t.Fatalf("daily: %v %v %v", a, d, daily)
	}
}

// Direxion's distributions on a scratch database, through a fake site API:
// one POST of getKurtosysFunds per fund with the key in its header, an Info
// line per read with the dates the answer is as of, the rounds since
// history_start on record as Direxion declared them, and nothing new at the
// next read; a read of the declaration window leaves the daily read's time
// alone.
func TestReadDirexionActionsOnDatabase(t *testing.T) {
	st := scratchStore(t)
	ctx := context.Background()
	const key = "test-key-not-a-real-one"
	var asked []string
	soxlWithout := false // SOXL's list without its round of 2026-09-22
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query     string            `json:"query"`
			Variables map[string]string `json:"variables"`
		}
		raw, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost || r.Header.Get("X-Api-Key") != key || json.Unmarshal(raw, &body) != nil || body.Query != direxionGetKurtosysFunds {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		asked = append(asked, body.Variables["Ticker"])
		answer := testdata(t, "direxion_getKurtosysFunds_"+body.Variables["Ticker"]+".json")
		if soxlWithout && body.Variables["Ticker"] == "SOXL" {
			answer = bytes.Replace(answer, []byte(`{"IncomeDividend":0.0929,"IncomeDividendConvertibleToReturnOfCapital":false,"LongTermCapitalGain":null,"ExDate":"09222026",`+
				`"PayDate":"09292026","RecordDate":"09222026","ReturnOfCapital":null,"ShortTermCapitalGain":null},`), nil, 1)
		}
		w.Write(answer)
	}))
	defer srv.Close()
	cfg := validConfig()
	cfg.Combos = []Combo{{ID: "SOXL-SOXS", BullTicker: "SOXL", BearTicker: "SOXS", RefTicker: "SOXX", NAVSource: "direxion",
		Markets: []Market{{Venue: "binance", BullSymbol: "SOXLUSDT", BearSymbol: "SOXSUSDT", RefSymbol: "SOXXUSDT"}}}}
	cfg.CorporateActions = []ActionEntry{{Ticker: "SOXL", ExDate: "2026-12-22", Kind: "dividend", Source: "x"}}
	var logged bytes.Buffer
	m := &Manager{cfg: cfg, cal: Calendar{}, store: st, log: slog.New(slog.NewTextHandler(&logged, nil)), told: map[string]bool{}, failing: map[string]bool{},
		navs: map[string]NAVSource{"direxion": Direxion{endpoint: srv.URL, key: key}}}
	m.readDirexionActions(ctx, m.direxionTickers(), true)
	stored, err := st.actions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"SOXL 2026-09-22 dividend Known 0.0929 direxion-api", "SOXS 2026-09-22 dividend Known 0.43641 direxion-api"}; !slices.Equal(brief(stored), want) ||
		!slices.Equal(asked, []string{"SOXL", "SOXS"}) || m.direxionRead.IsZero() || !strings.HasPrefix(stored[0].URL, srv.URL+" getKurtosysFunds(Ticker: SOXL)") {
		t.Fatalf("on record %v, asked %v, url %s", brief(stored), asked, stored[0].URL)
	}
	daily := m.direxionRead
	m.readDirexionActions(ctx, []string{"SOXS"}, false)
	if again, _ := st.actions(ctx); len(again) != 2 || strings.Count(logged.String(), "on record") != 2 || strings.Contains(logged.String(), key) ||
		strings.Count(logged.String(), "Direxion distributions read") != 3 || !strings.Contains(logged.String(), "ticker=SOXS time_stamp=11202026 trade_date=11192026") ||
		!m.direxionRead.Equal(daily) {
		t.Fatalf("second read: %v, daily read %v then %v, log %s", brief(again), daily, m.direxionRead, logged.String())
	}
	// A round Direxion has declared for SOXS that SOXL's list lacks: SOXL pays nothing in it. An expected round nobody has
	// declared yet stays Scheduled.
	if _, err := st.pool.Exec(ctx, `DELETE FROM etfpair.etf_action WHERE ticker = 'SOXL'`); err != nil {
		t.Fatal(err)
	}
	soxlWithout = true
	m.cfg.CorporateActions = []ActionEntry{{Ticker: "SOXL", ExDate: "2026-09-22", Kind: "dividend", Source: "x"}, {Ticker: "SOXL", ExDate: "2026-12-22", Kind: "dividend", Source: "x"}}
	m.readDirexionActions(ctx, []string{"SOXL"}, false)
	if got, _ := st.actions(ctx); !slices.Contains(brief(got), "SOXL 2026-09-22 dividend None direxion-api") || slices.ContainsFunc(got, func(a Action) bool { return a.ExDate == "2026-12-22" }) {
		t.Fatalf("SOXL without the round: %v", brief(got))
	}
}

// A round Direxion lists for both funds, SOXL's with its amount and SOXS's with its four amounts still null (REVIEW_r4 S1):
// SOXS has listed the round, so it is not inferred None from SOXL's amount; it stays Scheduled and is read again.
func TestDirexionRoundWithoutAmountsOnDatabase(t *testing.T) {
	st := scratchStore(t)
	ctx := context.Background()
	const key = "test-key-not-a-real-one"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Variables map[string]string `json:"variables"`
		}
		raw, _ := io.ReadAll(r.Body)
		if r.Header.Get("X-Api-Key") != key || json.Unmarshal(raw, &body) != nil {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		amount := "null"
		if body.Variables["Ticker"] == "SOXL" {
			amount = "0.05"
		}
		w.Write([]byte(`{"data":{"getKurtosysFunds":{"TimeStamp":"12222026","TradeDate":"12212026","Distributions":[{"IncomeDividend":` + amount +
			`,"ShortTermCapitalGain":null,"LongTermCapitalGain":null,"ReturnOfCapital":null,"ExDate":"12222026"}]}}}`))
	}))
	defer srv.Close()
	cfg := validConfig()
	cfg.Combos = []Combo{{ID: "SOXL-SOXS", BullTicker: "SOXL", BearTicker: "SOXS", RefTicker: "SOXX", NAVSource: "direxion",
		Markets: []Market{{Venue: "binance", BullSymbol: "SOXLUSDT", BearSymbol: "SOXSUSDT", RefSymbol: "SOXXUSDT"}}}}
	cfg.CorporateActions = []ActionEntry{{Ticker: "SOXL", ExDate: "2026-12-22", Kind: "dividend", Source: "x"},
		{Ticker: "SOXS", ExDate: "2026-12-22", Kind: "dividend", Source: "x"}}
	for _, e := range cfg.CorporateActions { // the configuration's dates on record, as readActions puts them
		a, _ := e.action()
		a.ReadMs = 1
		if err := st.insertActions(ctx, []Action{a}); err != nil {
			t.Fatal(err)
		}
	}
	m := &Manager{cfg: cfg, cal: Calendar{}, store: st, log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), told: map[string]bool{}, failing: map[string]bool{},
		navs: map[string]NAVSource{"direxion": Direxion{endpoint: srv.URL, key: key}}}
	m.readDirexionActions(ctx, m.direxionTickers(), false)
	stored, err := st.actions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := st.scheduledActions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(brief(stored), "SOXL 2026-12-22 dividend Known 0.05 direxion-api") || slices.Contains(brief(stored), "SOXS 2026-12-22 dividend None direxion-api") ||
		!slices.Equal(brief(waiting), []string{"SOXS 2026-12-22 dividend Scheduled config"}) {
		t.Fatalf("on record %v, still Scheduled %v", brief(stored), brief(waiting))
	}
}

// cannedTransport answers the collector's requests in place of the network.
type cannedTransport func(*http.Request) (int, []byte)

func (c cannedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	status, body := c(r)
	return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(body)), Header: http.Header{}, Request: r}, nil
}

// ProShares through canned answers (no request leaves the host): after the
// NAV read of a tick (the product pages), readActions asks www.proshares.com
// a second apart at least; it reads the distribution tables through the year
// after a December date of the schedule; a December round that neither its
// year's table nor the next year's lists is None once both were read, and
// not while the next year's could not be (TQQQ's 2026 table fails here).
func TestProSharesReadsOnDatabase(t *testing.T) {
	st := scratchStore(t)
	ctx := context.Background()
	var mu sync.Mutex
	var proshares []time.Time
	var years []string
	schedule := append([]byte(`<h2>What are the 2025 Quarterly Dates?</h2><table><tr><td>Excise</td><td>12/31/2025</td></tr></table>`),
		testdata(t, "proshares_distribution-schedule_excerpt.html")...)
	was := httpClient.Transport
	httpClient.Transport = cannedTransport(func(r *http.Request) (int, []byte) {
		mu.Lock()
		defer mu.Unlock()
		q := r.URL.Query()
		switch {
		case r.URL.Host == "www.proshares.com":
			proshares = append(proshares, time.Now())
			switch {
			case strings.HasPrefix(r.URL.Path, "/our-etfs/"):
				return http.StatusOK, []byte(`<span id="price-asOfDate">as of 11/20/2026</span><span id="price-nav">$50.12</span>`)
			case strings.HasPrefix(r.URL.Path, "/resources/"):
				return http.StatusOK, schedule
			}
			years = append(years, q.Get("fund")+" "+q.Get("year"))
			switch q.Get("fund") + " " + q.Get("year") {
			case "TQQQ 2026":
				return http.StatusInternalServerError, nil
			case "SQQQ 2026":
				return http.StatusOK, testdata(t, "proshares_distributions_TQQQ_2026.json")
			}
			return http.StatusOK, []byte(`[]`)
		case r.URL.Host == "api.nasdaq.com":
			return http.StatusOK, testdata(t, "nasdaq_dividends_SOXL.json")
		case r.URL.Host == "api.bitget.com":
			return http.StatusOK, testdata(t, "bitget_split-records.json")
		}
		return http.StatusNotFound, nil
	})
	defer func() { httpClient.Transport = was }()
	cfg := validConfig()
	cfg.HistoryStart = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	m := &Manager{cfg: cfg, cal: Calendar{}, store: st, log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), told: map[string]bool{}, failing: map[string]bool{}}
	if _, err := (ProShares{}).Latest(ctx, []string{"TQQQ", "SQQQ"}, ""); err != nil {
		t.Fatal(err)
	}
	m.readActions(ctx)
	for i := 1; i < len(proshares); i++ {
		if gap := proshares[i].Sub(proshares[i-1]); gap < sourcePause {
			t.Fatalf("requests %d and %d to www.proshares.com %v apart", i-1, i, gap)
		}
	}
	if want := []string{"TQQQ 2025", "TQQQ 2026", "TQQQ 2027", "SQQQ 2025", "SQQQ 2026", "SQQQ 2027"}; !slices.Equal(years, want) {
		t.Fatalf("tables read %v, want %v", years, want)
	}
	stored, err := st.actions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := slices.DeleteFunc(brief(stored), func(a string) bool { return !strings.Contains(a, "2025-12-31") })
	slices.Sort(got)
	if want := []string{"SQQQ 2025-12-31 dividend None proshares-api", "TQQQ 2025-12-31 dividend Scheduled proshares-schedule"}; !slices.Equal(got, want) {
		t.Fatalf("the round of 2025-12-31: %v, want %v", got, want)
	}
}

// A schedule that cannot be compared (an unknown session type) is stored as
// not compared, with the time of the check and no window, logged once, and
// asked again only after an hour; a readable one replaces it.
func TestCalendarCheckOnDatabase(t *testing.T) {
	st := scratchStore(t)
	ctx := context.Background()
	schedule := `{"marketSchedules":{"EQUITY":{"sessions":[{"startTime":1794445200000,"endTime":1794474000000,"type":"OVERNIGHT_PLUS"}]}}}`
	asked := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked++
		fmt.Fprint(w, schedule)
	}))
	defer srv.Close()
	was := binanceFAPI
	binanceFAPI = srv.URL
	defer func() { binanceFAPI = was }()
	var logged bytes.Buffer
	m := &Manager{cfg: validConfig(), store: st, log: slog.New(slog.NewTextHandler(&logged, nil)), told: map[string]bool{}, failing: map[string]bool{}}
	row := func() string {
		t.Helper()
		var ok bool
		var detail string
		if err := st.pool.QueryRow(ctx, `SELECT checked_ms > 0 AND window_from_ms IS NULL AND window_to_ms IS NULL AND NOT agree, detail
			FROM etfpair.calendar_check`).Scan(&ok, &detail); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprint(ok, " ", detail)
	}
	m.checkCalendar(ctx)
	if got := row(); got != `true not compared: unknown session type "OVERNIGHT_PLUS"` || m.calendarChecked.IsZero() {
		t.Fatalf("not compared: %s, checked %v", got, m.calendarChecked)
	}
	m.checkCalendar(ctx)
	if strings.Count(logged.String(), "level=WARN") != 1 || asked != 2 {
		t.Fatalf("logged once: %s", logged.String())
	}
	schedule = string(testdata(t, "binance_tradingSchedule.json"))
	m.checkCalendar(ctx)
	var agree bool
	var detail string
	var to *int64
	if err := st.pool.QueryRow(ctx, `SELECT agree, detail, window_to_ms FROM etfpair.calendar_check`).Scan(&agree, &detail, &to); err != nil || !agree ||
		detail != "OK" || to == nil || !strings.Contains(logged.String(), "read recovered") {
		t.Fatalf("compared again: %v %s %v %v", agree, detail, to, err)
	}
}

// A bell anchor replaced because the close moved after it was read is
// known when the replacement is stored, not two minutes after the close.
func TestReplacedBellKnownOnDatabase(t *testing.T) {
	st := scratchStore(t)
	ctx := context.Background()
	closeAt, _ := Calendar{}.closeOf("2026-11-19")
	c := closeAt.UnixMilli()
	if err := st.writeCalendar(ctx, []session{{"2026-11-19", c}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `INSERT INTO etfpair.fx_minute (pair, minute_ms, price, source, sampled_ms) VALUES ('USDT-USD', $1, 1, 'candle', 0)`, c-minuteMs); err != nil {
		t.Fatal(err)
	}
	// Read at a close three hours earlier, before an override moved it to 16:00.
	if _, err := st.pool.Exec(ctx, `INSERT INTO etfpair.etf_bell VALUES ('QQQ', '2026-11-19', 600, 600, 'binance', 1, $1, $2)`, c-3*3600_000-minuteMs, c-3*3600_000+2*minuteMs); err != nil {
		t.Fatal(err)
	}
	cfg := validConfig()
	cfg.Combos[0].BullTicker, cfg.Combos[0].BearTicker = "", "" // the reference only
	cfg.Combos[0].Markets = []Market{{Venue: "binance", RefSymbol: "QQQUSDT"}}
	m := &Manager{cfg: cfg, store: st, log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), told: map[string]bool{}, failing: map[string]bool{},
		started: time.UnixMilli(c).Add(24 * time.Hour), indexFrom: map[string]int64{"QQQUSDT": 0},
		index: map[string]IndexSource{"binance": &fakeIndex{klines: map[string]map[int64]string{"QQQUSDT": {c - minuteMs: "601"}}}}}
	before := time.Now().UnixMilli()
	m.loadBells(ctx)
	var price string
	var known, kline int64
	if err := st.pool.QueryRow(ctx, `SELECT price::text, known_ms, kline_ms FROM etfpair.etf_bell WHERE ticker = 'QQQ'`).Scan(&price, &known, &kline); err != nil ||
		price != "601" || kline != c-minuteMs || known < before {
		t.Fatalf("replaced bell: %s kline %d known %d (stored from %d) %v", price, kline, known, before, err)
	}
}

// Binance's schedule of 2026-11-20 equals the calendar. Compared are the
// trading days and their closes (an override turns a difference on its date
// into information) and, as information only, the extended hours.
func TestCompareSchedule(t *testing.T) {
	sessions, err := parseTradingSchedule(testdata(t, "binance_tradingSchedule.json"))
	if err != nil {
		t.Fatal(err)
	}
	from, to, differ, info, err := Calendar{}.compareSchedule(sessions)
	if err != nil || len(differ)+len(info) != 0 || !time.UnixMilli(from).Equal(nyTime(t, "2026-11-11 20:00:00")) ||
		!time.UnixMilli(to).Equal(nyTime(t, "2026-11-25 20:00:00")) {
		t.Fatalf("as saved: %v %v %v %v %v", time.UnixMilli(from), time.UnixMilli(to), differ, info, err)
	}
	edit := func(change func([]ScheduleSession) []ScheduleSession) []ScheduleSession {
		return change(slices.Clone(sessions))
	}
	regular24 := slices.IndexFunc(sessions, func(s ScheduleSession) bool {
		return s.Type == "REGULAR" && time.UnixMilli(s.StartMs).In(newYork).Format("2006-01-02") == "2026-11-24"
	})
	early := edit(func(s []ScheduleSession) []ScheduleSession {
		s[regular24].EndMs, s[regular24+1].StartMs = nyTime(t, "2026-11-24 13:00:00").UnixMilli(), nyTime(t, "2026-11-24 13:00:00").UnixMilli()
		return s
	})
	_, _, differ, info, _ = Calendar{}.compareSchedule(early)
	if want := []string{"2026-11-24: close 11-25 05:00 here, 11-25 02:00 on Binance"}; !slices.Equal(differ, want) || len(info) != 2 {
		t.Fatalf("moved close: %v %v", differ, info)
	}
	// The operator's override agrees on the close; that Binance's after-market still runs to 20:00 (here 17:00) is information.
	_, _, differ, info, _ = newCalendar([]CalendarOverride{{Date: "2026-11-24", Close: "13:00", Source: "x"}}).compareSchedule(early)
	if len(differ) != 0 || len(info) != 3 || slices.ContainsFunc(info, func(d string) bool { return strings.Contains(d, "close") }) {
		t.Fatalf("the operator's override agrees: %v %v", differ, info)
	}
	_, _, differ, info, _ = newCalendar([]CalendarOverride{{Date: "2026-11-24", Close: "14:00", Source: "x"}}).compareSchedule(early)
	if len(differ) != 0 || !strings.HasSuffix(info[0], "(overridden here)") {
		t.Fatalf("an overridden date is information: %v %v", differ, info)
	}
	missing := edit(func(s []ScheduleSession) []ScheduleSession { return slices.Delete(s, regular24, regular24+1) })
	if _, _, differ, _, _ = (Calendar{}).compareSchedule(missing); len(differ) != 1 || !strings.Contains(differ[0], "a trading day here") {
		t.Fatalf("no trading day on Binance: %v", differ)
	}
	night := edit(func(s []ScheduleSession) []ScheduleSession {
		s[4].StartMs += 3600_000 // an overnight session from 21:00, as the exchanges' from 2026-12-06
		return s
	})
	if _, _, differ, info, _ = (Calendar{}).compareSchedule(night); len(differ) != 0 || len(info) != 2 {
		t.Fatalf("extended hours are information only: %v %v", differ, info)
	}
}

// The actions on a scratch database: when each takes effect (a Monday
// ex-date: Sunday 20:00; after a holiday: the holiday's 20:00; across
// daylight saving), the anchors from then (A - D, A / r, (A - D) / r piecewise,
// a split per venue with no points from its halt), the mean restarting at
// the instant, the time the anchors became known, the bell's validity, and
// the statuses the board shows. The anchors are checked on combo_anchors, the drawn points on combo_range (board 3.2's,
// since board 3.1's combo_series and combo_points were dropped, OWNER.md item 24).
func TestActionsOnDatabase(t *testing.T) {
	st := scratchStore(t)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := st.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	lines := func(sql string, args ...any) []string {
		t.Helper()
		rows, err := st.pool.Query(ctx, sql, args...)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		out, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return out
	}
	expect := func(name string, got []string, want ...string) {
		t.Helper()
		if !slices.Equal(got, want) {
			t.Fatalf("%s:\ngot\n%s\nwant\n%s", name, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}
	cfg := validConfig()
	cfg.Combos = []Combo{{ID: "T", BullTicker: "BULL", BearTicker: "BEAR", RefTicker: "REF", NAVSource: "direxion",
		Markets: []Market{{Venue: "binance", BullSymbol: "BULLUSDT", BearSymbol: "BEARUSDT", RefSymbol: "REFUSDT"},
			{Venue: "bitget", BullSymbol: "BULLUSDT", BearSymbol: "BEARUSDT", RefSymbol: "REFUSDT"}}}}
	if err := st.seed(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	var sessions []session
	for _, date := range []string{"2026-09-18", "2026-09-21", "2026-09-22", "2026-11-25", "2026-11-27", "2026-12-14", "2026-12-15", "2026-12-18",
		"2026-12-21", "2026-12-22", "2026-12-23", "2026-12-24", "2026-12-28", "2026-12-29", "2026-12-30", "2026-12-31", "2027-01-04",
		"2027-03-11", "2027-03-12", "2027-03-15", "2027-11-29", "2027-11-30", "2027-12-01"} {
		closeAt, _ := Calendar{}.closeOf(date)
		sessions = append(sessions, session{date, closeAt.UnixMilli()})
	}
	if err := st.writeCalendar(ctx, sessions, nil); err != nil {
		t.Fatal(err)
	}
	ny := func(s string) int64 { return nyTime(t, s).UnixMilli() }
	act := func(ticker, exDate, kind, value, status string, knownMs int64) {
		exec(`INSERT INTO etfpair.etf_action VALUES ($1, $2::date, $3, nullif($4, '')::numeric, $5, 'test', 'u', $6)`, ticker, exDate, kind, value, status, knownMs)
	}
	// The instants: Monday 2026-09-21 (carrier Friday 09-18), Friday 2026-11-27 after Thanksgiving (carrier 11-25), the
	// December rounds (SOXX 12-15, QQQ Monday 12-21, SOXL / SOXS 12-22, TQQQ / SQQQ 12-23, the excise-tax 12-30 and 12-31,
	// around the 12-24 early close and Christmas), 2027-03-12 in standard time and 2027-03-15 after the clocks go forward.
	for _, ex := range []string{"2026-09-21", "2026-11-27", "2026-12-15", "2026-12-21", "2026-12-22", "2026-12-23", "2026-12-30", "2026-12-31",
		"2027-03-12", "2027-03-15"} {
		act("REF", ex, "dividend", "0.5", "Known", 1)
	}
	instants := `SELECT ex_date || ' ' || session_date || ' ' || to_char(to_timestamp(effective_ms / 1000) AT TIME ZONE 'UTC', 'MM-DD HH24:MI') || ' UTC'
		FROM etfpair.combo_actions('T', 'binance') WHERE leg = 'ref' ORDER BY ex_date`
	expect("instants", lines(instants),
		"2026-09-21 2026-09-18 09-21 00:00 UTC", // Sunday 20:00 EDT
		"2026-11-27 2026-11-25 11-27 01:00 UTC", // Thanksgiving 20:00 EST
		"2026-12-15 2026-12-14 12-15 01:00 UTC", // SOXX: Monday 12-14 20:00 EST
		"2026-12-21 2026-12-18 12-21 01:00 UTC", // QQQ: Sunday 12-20 20:00 EST, carrier Friday 12-18
		"2026-12-22 2026-12-21 12-22 01:00 UTC", // SOXL / SOXS
		"2026-12-23 2026-12-22 12-23 01:00 UTC", // TQQQ / SQQQ
		"2026-12-30 2026-12-29 12-30 01:00 UTC",
		"2026-12-31 2026-12-30 12-31 01:00 UTC",
		"2027-03-12 2027-03-11 03-12 01:00 UTC", // 20:00 EST
		"2027-03-15 2027-03-12 03-15 00:00 UTC") // Sunday 20:00 EDT
	exec(`DELETE FROM etfpair.etf_action`)

	// Session 2026-09-21 (16:00 EDT close), the actions of the ex-date 09-22 at 20:00: BULL pays 2 and splits 2-for-1, BEAR pays 1, REF
	// pays nothing it has said (Scheduled). Bells 100 / 50 / 300, F = 1.
	c21 := ny("2026-09-21 16:00:00")
	for ticker, price := range map[string]string{"BULL": "100", "BEAR": "50", "REF": "300"} {
		exec(`INSERT INTO etfpair.etf_bell VALUES ($1, '2026-09-21', $2::numeric, $2::numeric, 'binance', 1, $3, 1000)`, ticker, price, c21-minuteMs)
	}
	act("BULL", "2026-09-22", "dividend", "2", "Known", 5000)
	act("BEAR", "2026-09-22", "dividend", "1", "Known", 6000)
	act("BULL", "2026-09-22", "split", "2", "Known", 7000)
	act("REF", "2026-09-22", "dividend", "", "Scheduled", 1)
	T := ny("2026-09-21 20:00:00")
	minute := func(venue, symbol string, at int64, price string) {
		exec(`INSERT INTO etfpair.perp_minute VALUES ($1, $2, $3, $4::numeric, 0)`, venue, symbol, at, price)
	}
	for i := int64(-5); i <= 12; i++ {
		at := T + i*minuteMs
		exec(`INSERT INTO etfpair.fx_minute (pair, minute_ms, price, source, sampled_ms) VALUES ('USDT-USD', $1, 1, 'candle', 0)`, at)
		bear := "50"
		if i >= 0 {
			bear = "49.05" // 1 down at 20:00, then 0.1 bps... a steady ex price
		}
		for _, venue := range []string{"binance", "bitget"} {
			minute(venue, "BEARUSDT", at, bear)
			minute(venue, "REFUSDT", at, "300")
		}
		// BULL on Binance: trades to 20:02, frozen (the halt) 20:03-20:05, new basis from 20:06; on Bitget frozen from 20:01, new basis 20:08.
		binance, bitget := fmt.Sprint(98+0.01*float64(i)), fmt.Sprint(98+0.01*float64(i))
		if i < 0 {
			binance, bitget = fmt.Sprint(100+0.01*float64(i)), fmt.Sprint(100+0.01*float64(i))
		}
		switch {
		case i >= 6:
			binance = fmt.Sprint(49 + 0.005*float64(i))
		case i >= 3:
			binance = "98.02"
		}
		switch {
		case i >= 8:
			bitget = fmt.Sprint(49 + 0.005*float64(i))
		case i >= 1:
			bitget = "98"
		}
		minute("binance", "BULLUSDT", at, binance)
		minute("bitget", "BULLUSDT", at, bitget)
	}
	// A 31% fall on Bitget in After-Hours, before T, nearer half the price than the price: no split takes effect before T.
	minute("bitget", "BULLUSDT", T-151*minuteMs, "100")
	minute("bitget", "BULLUSDT", T-150*minuteMs, "69")
	clock := func(ms *int64) string {
		if ms == nil {
			return "-"
		}
		return time.UnixMilli(*ms).In(newYork).Format("15:04")
	}
	for venue, want := range map[string][]string{
		"binance": {"bull dividend 20:00 - Applied", "bull split 20:06 20:03 Applied", "bear dividend 20:00 - Applied", "ref dividend 20:00 - Scheduled · Past T warn"},
		"bitget":  {"bull dividend 20:00 - Applied", "bull split 20:08 20:01 Applied", "bear dividend 20:00 - Applied", "ref dividend 20:00 - Scheduled · Past T warn"},
	} {
		rows, err := st.pool.Query(ctx, `SELECT leg, kind, coalesce(effective_ms, start), halt_ms, shown, warn FROM etfpair.combo_actions('T', $1),
			LATERAL (SELECT $2::bigint AS start) s ORDER BY array_position(ARRAY['bull', 'bear', 'ref'], leg), kind`, venue, T)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for rows.Next() {
			var leg, kind, shown string
			var eff int64
			var halt *int64
			var warn bool
			if err := rows.Scan(&leg, &kind, &eff, &halt, &shown, &warn); err != nil {
				t.Fatal(err)
			}
			got = append(got, strings.TrimSpace(fmt.Sprintf("%s %s %s %s %s %s", leg, kind, clock(&eff), clock(halt), shown, map[bool]string{true: "warn"}[warn])))
		}
		expect("actions on "+venue, got, want...)
	}
	// The anchors of session 09-21 on each venue (combo_anchors, which combo_range reads): the bells until T, A - D from T
	// (98, 49), BULL out from its halt to T_v (a gap: no points), (A - D) / r from T_v (49); the anchors' known time is the
	// latest of the session's and the actions'.
	anchors := `SELECT to_char(to_timestamp(from_ms / 1000) AT TIME ZONE 'America/New_York', 'HH24:MI') || ' ' || trim_scale(a_bull) || ' ' ||
		trim_scale(a_bear) || ' ' || coalesce(trim_scale(a_ref)::text, '-') || CASE WHEN gap_bull THEN ' bull gap' ELSE '' END || ' known ' || known_ms ||
		CASE WHEN past_t THEN ' past T' ELSE '' END FROM etfpair.combo_anchors('T', $1) WHERE session_date = $2::date ORDER BY from_ms`
	expect("anchors on binance", lines(anchors, "binance", "2026-09-21"),
		"16:00 100 50 300 known 1000", "20:00 98 49 300 known 6000", "20:03 98 49 300 bull gap known 6000", "20:06 49 49 300 known 7000")
	expect("anchors on bitget", lines(anchors, "bitget", "2026-09-21"),
		"16:00 100 50 300 known 1000", "20:00 98 49 300 known 6000", "20:01 98 49 300 bull gap known 6000", "20:08 49 49 300 known 7000")
	// Each minute as the board draws it (combo_range, SMA of 2 minutes, trade prices): a recorder row in the middle of every
	// minute with that minute's candle close, the bars the candle closes (no row at a minute's end). On Binance: A - D from
	// 20:00, nothing while BULL is halted (20:03-20:05), (A - D) / r from 20:06; the two-minute mean restarts at each change
	// of the legs' anchors. The same figures as board 3.1's per-minute combo_series gave on this fixture (2026-11-23,
	// experiments/board33_20261123/drop31/REPORT.md).
	for _, venue := range []string{"binance", "bitget"} {
		exec(`INSERT INTO etfpair.perp_second (venue, symbol, second_ms, price, trade_ms) SELECT venue, symbol, minute_ms + 30000, price, minute_ms + 29900
			FROM etfpair.perp_minute WHERE venue = $1 AND symbol IN ('BULLUSDT', 'BEARUSDT') AND minute_ms BETWEEN $2 AND $3`, venue, T-3*minuteMs, T+9*minuteMs)
	}
	drawn := `SELECT to_char(to_timestamp(ts_ms / 1000) AT TIME ZONE 'America/New_York', 'HH24:MI:SS') || ' s ' || round(s_bps, 1) || ' mu ' ||
		coalesce(round(mu_bps, 1)::text, '-') || CASE WHEN past_t THEN ' past T' ELSE '' END
		FROM etfpair.combo_range('T', $1, 'sma', 2, $2, $3, 1000, NULL, NULL) WHERE s_bps IS NOT NULL AND ts_ms % $4::bigint = $5::bigint ORDER BY ts_ms`
	expect("minutes on binance", lines(drawn, "binance", T-3*minuteMs, T+9*minuteMs, minuteMs, 30_000),
		// s = 10000 (p_bull / A'_bull + p_bear / A'_bear - 2); mu: the same on the mean of the two minutes before
		"19:57:30 s -3.0 mu -4.5", // 99.97 / 100 + 50 / 50 - 2
		"19:58:30 s -2.0 mu -3.5",
		"19:59:30 s -1.0 mu -2.5",
		"20:00:30 s 10.2 mu -", // 98 / 98 + 49.05 / 49 - 2
		"20:01:30 s 11.2 mu -",
		"20:02:30 s 12.2 mu 10.7", // the mean of 98, 98.01 over 98
		"20:06:30 s 16.3 mu -",    // 49.03 / 49 + 49.05 / 49 - 2
		"20:07:30 s 17.3 mu -",
		"20:08:30 s 18.4 mu 16.8") // the mean of 49.03, 49.035
	expect("minutes on bitget", lines(drawn, "bitget", T-3*minuteMs, T+9*minuteMs, minuteMs, 30_000),
		"19:57:30 s -3.0 mu -4.5", "19:58:30 s -2.0 mu -3.5", "19:59:30 s -1.0 mu -2.5", "20:00:30 s 10.2 mu -", "20:08:30 s 18.4 mu -")
	// The drawn seconds: one in 20:00 is on A - D with no mean (the minute before was on A), one in BULL's halt is not drawn; from
	// T_v (20:06) a row whose last trade is from before T_v (20:06:02, the 98.02 of 20:02) has no price on A / r and is not drawn,
	// one with a trade after T_v (20:06:03) is.
	for _, at := range []int64{T - 55_000, T + 5_000, T + 4*minuteMs + 5_000, T + 7*minuteMs + 5_000} {
		for _, symbol := range []string{"BULLUSDT", "BEARUSDT"} {
			exec(`INSERT INTO etfpair.perp_second (venue, symbol, second_ms, price) SELECT venue, symbol, $3, price FROM etfpair.perp_minute
				WHERE venue = 'binance' AND symbol = $1 AND minute_ms = $2`, symbol, (at-1)/minuteMs*minuteMs, at)
		}
	}
	second := func(symbol string, at int64, price string, tradeMs int64) {
		exec(`INSERT INTO etfpair.perp_second (venue, symbol, second_ms, price, trade_ms) VALUES ('binance', $1, $2, $3::numeric, $4)`, symbol, at, price, tradeMs)
	}
	second("BULLUSDT", T+6*minuteMs+2_000, "98.02", T+2*minuteMs+30_000)
	second("BEARUSDT", T+6*minuteMs+2_000, "49.05", T+6*minuteMs+1_500)
	second("BULLUSDT", T+6*minuteMs+3_000, "49.03", T+6*minuteMs+500)
	second("BEARUSDT", T+6*minuteMs+3_000, "49.05", T+6*minuteMs+1_500)
	expect("seconds", lines(`SELECT to_char(to_timestamp(ts_ms / 1000.0) AT TIME ZONE 'America/New_York', 'HH24:MI:SS') || ' s ' || round(s_bps, 1) ||
		' mu ' || coalesce(round(mu_bps, 1)::text, '-') FROM etfpair.combo_range('T', 'binance', 'sma', 2, $1, $2, 1000, NULL, NULL)
		WHERE s_bps IS NOT NULL AND ts_ms % 30000 <> 0 ORDER BY ts_ms`, T-minuteMs, T+8*minuteMs),
		"19:59:05 s -1.0 mu -2.5", "20:00:05 s 10.2 mu -", "20:06:03 s 16.3 mu -", "20:07:05 s 17.3 mu -")

	// The statuses: a dividend's checks of every venue, a split's of its own venue; Mismatch and an amount still unknown past T warn.
	check := func(ticker, kind, venue, status string) {
		exec(`INSERT INTO etfpair.etf_action_check VALUES ($1, '2026-09-22', $2, $3, 0, $4, NULL, 0)`, ticker, kind, venue, status)
	}
	check("BEAR", "dividend", "binance", "Verified")
	check("BEAR", "dividend", "issuer", "Mismatch")
	check("BULL", "dividend", "binance", "Verified")
	check("BULL", "split", "bitget", "Verified")
	act("REF", "2027-11-30", "dividend", "", "Scheduled", 1)
	act("BULL", "2027-11-30", "dividend", "0.1", "Known", 1)
	act("BEAR", "2027-11-30", "dividend", "", "None", 1)
	expect("statuses on binance", lines(`SELECT ticker || ' ' || ex_date || ' ' || kind || ' ' || shown || ' ' || warn FROM etfpair.combo_actions('T', 'binance')
		ORDER BY ex_date, ticker, kind`),
		"BEAR 2026-09-22 dividend Mismatch true", "BULL 2026-09-22 dividend Verified false", "BULL 2026-09-22 split Applied false",
		"REF 2026-09-22 dividend Scheduled · Past T true",
		"BEAR 2027-11-30 dividend None false", "BULL 2027-11-30 dividend Known false", "REF 2027-11-30 dividend Scheduled false")

	// A bell read at a close that has moved since is not valid: combo_sessions ignores it, bellsDue lists the session again and
	// insertBell replaces it.
	exec(`UPDATE etfpair.etf_bell SET kline_ms = kline_ms - 3 * 3600000 WHERE ticker = 'REF'`)
	expect("ignored bell", lines(`SELECT coalesce(a_ref::text, 'none') FROM etfpair.combo_sessions('T') WHERE session_date = '2026-09-21'`), "none")
	due, err := st.bellsDue(ctx, "REF", c21+time.Hour.Milliseconds())
	if err != nil || len(due) == 0 || due[0].Date != "2026-09-21" || due[0].HasFX {
		t.Fatalf("bells due: %+v %v", due, err)
	}
	exec(`INSERT INTO etfpair.fx_minute (pair, minute_ms, price, source, sampled_ms) VALUES ('USDT-USD', $1, 1, 'candle', 0)`, c21-minuteMs)
	if price, err := st.insertBell(ctx, "REF", "2026-09-21", "binance", Candle{MinuteMs: c21 - minuteMs, Close: "301"}, 9000, 9500); err != nil || price != "301" {
		t.Fatalf("replaced bell: %q %v", price, err)
	}
	expect("replaced bell", lines(`SELECT a_ref::text FROM etfpair.combo_sessions('T') WHERE session_date = '2026-09-21'`), "301")
	expect("replaced bell known", lines(`SELECT known_ms::text FROM etfpair.etf_bell WHERE ticker = 'REF'`), "9500")

	// The checks the collector makes from the database: the split's log ratio net of the other leg, the pair's NAV return with D added back.
	m := &Manager{cfg: cfg, store: st, log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), told: map[string]bool{}, failing: map[string]bool{}}
	due2, err := st.actionsInEffect(ctx, cfg.Combos[0], "binance", time.Now().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	var split actionCheck
	for _, a := range due2 {
		if a.Kind == "split" {
			split = a
		}
	}
	status, observed, final := m.checkSplit(ctx, split)
	if status != "Verified" || !final || observed != "-0.6927" { // ln(49.03 / 98.02) + ln(49.05 / 49.05): BEAR flat over the halt; -ln 2 = -0.6931
		t.Fatalf("split check: %s %s %v", status, observed, final)
	}
	for _, n := range [][3]string{{"BULL", "2026-09-21", "100"}, {"BEAR", "2026-09-21", "50"}, {"BULL", "2026-09-22", "98"}, {"BEAR", "2026-09-22", "48.9"}} {
		exec(`INSERT INTO etfpair.etf_nav VALUES ($1, $2::date, $3::numeric, 1, 'direxion', 'api')`, n[0], n[1], n[2])
	}
	if tr, ok, err := st.pairReturn(ctx, cfg.Combos[0], "2026-09-21", "2026-09-22"); err != nil || !ok || fmt.Sprintf("%.1f", tr) != "-20.0" {
		t.Fatalf("pair return: %v %v %v", tr, ok, err) // (98 + 2) / 100 - 1 + (48.9 + 1) / 50 - 1 = -0.002
	}

	// The expected step: the basket's of the legs with an amount; none for an action without one (BEAR now says None, REF is
	// Scheduled), rather than the other leg's.
	act("BEAR", "2026-09-22", "dividend", "", "None", 9000)
	expect("expected steps", lines(`SELECT ticker || ' ' || kind || ' ' || coalesce(round(expected, 4)::text, '-') FROM etfpair.combo_actions('T', 'binance')
		WHERE ex_date = '2026-09-22' ORDER BY ticker, kind`),
		"BEAR dividend -", "BULL dividend -200.0000", "BULL split -0.6931", "REF dividend -")

	// A leg's dividend still without its amount at T (Monday ex-date 09-21, T Sunday 09-20 20:00, carrier session 09-18): the
	// anchors stay as they are, so s steps down by about the dividend, but the mean restarts at T and every minute and sample from
	// T is past_t; once the amount is known the stretch is on A - D and no longer past_t.
	c18 := ny("2026-09-18 16:00:00")
	for ticker, price := range map[string]string{"BULL": "100", "BEAR": "50"} {
		exec(`INSERT INTO etfpair.etf_bell VALUES ($1, '2026-09-18', $2::numeric, $2::numeric, 'binance', 1, $3, 1000)`, ticker, price, c18-minuteMs)
	}
	act("BEAR", "2026-09-21", "dividend", "", "Scheduled", 1)
	T20 := ny("2026-09-20 20:00:00")
	for i := int64(-5); i <= 3; i++ {
		at := T20 + i*minuteMs
		exec(`INSERT INTO etfpair.fx_minute (pair, minute_ms, price, source, sampled_ms) VALUES ('USDT-USD', $1, 1, 'candle', 0)`, at)
		bear := "50"
		if i >= 0 {
			bear = "49" // 1 down at T: the dividend, not yet known
		}
		minute("binance", "BEARUSDT", at, bear)
		minute("binance", "BULLUSDT", at, fmt.Sprint(100+0.01*float64(i)))
	}
	expect("anchors past T without the amount", lines(anchors, "binance", "2026-09-18"), "16:00 100 50 - known 1000", "20:00 100 50 - known 1000 past T")
	// Minute rows in the middle of each minute (their candle closes) and three seconds with the closes of the minute before.
	exec(`INSERT INTO etfpair.perp_second (venue, symbol, second_ms, price, trade_ms) SELECT venue, symbol, minute_ms + 30000, price, minute_ms + 29900
		FROM etfpair.perp_minute WHERE venue = 'binance' AND symbol IN ('BULLUSDT', 'BEARUSDT') AND minute_ms BETWEEN $1 AND $2`, T20-5*minuteMs, T20+3*minuteMs)
	for _, at := range []int64{T20 - 55_000, T20 + 5_000, T20 + 2*minuteMs + 5_000} {
		for _, symbol := range []string{"BULLUSDT", "BEARUSDT"} {
			exec(`INSERT INTO etfpair.perp_second (venue, symbol, second_ms, price) SELECT venue, symbol, $3, price FROM etfpair.perp_minute
				WHERE venue = 'binance' AND symbol = $1 AND minute_ms = $2`, symbol, (at-1)/minuteMs*minuteMs, at)
		}
	}
	// As board 3.1's combo_series and combo_points gave on this fixture: s -200 at T is 100 / 100 + 49 / 50 - 2 (the mean of
	// 19:58 and 19:59 would be -1.5), the mean restarts at T and every point from T is past T.
	expect("points past T without the amount", lines(drawn, "binance", T20-3*minuteMs, T20+3*minuteMs, 1000, 0),
		"19:57:30 s -3.0 mu -4.5", "19:58:30 s -2.0 mu -3.5", "19:59:05 s -1.0 mu -2.5", "19:59:30 s -1.0 mu -2.5",
		"20:00:05 s -200.0 mu - past T", "20:00:30 s -200.0 mu - past T", "20:01:30 s -199.0 mu - past T",
		"20:02:05 s -198.0 mu -199.5 past T", "20:02:30 s -198.0 mu -199.5 past T")
	act("BEAR", "2026-09-21", "dividend", "1", "Known", 12345)
	// Once the amount is known the stretch from T is on A - D (BEAR 49, known when the amount was) and no longer past T:
	// 20:00 s 0.0 and 20:01 s 1.0, as combo_series gave.
	expect("anchors once the amount is known", lines(anchors, "binance", "2026-09-18"), "16:00 100 50 - known 1000", "20:00 100 49 - known 12345")
	expect("points once the amount is known", lines(drawn, "binance", T20-3*minuteMs, T20+3*minuteMs, 1000, 0),
		"19:57:30 s -3.0 mu -4.5", "19:58:30 s -2.0 mu -3.5", "19:59:05 s -1.0 mu -2.5", "19:59:30 s -1.0 mu -2.5",
		"20:00:05 s 0.0 mu -", "20:00:30 s 0.0 mu -", "20:01:30 s 1.0 mu -", "20:02:05 s 2.0 mu 0.5", "20:02:30 s 2.0 mu 0.5")

}
