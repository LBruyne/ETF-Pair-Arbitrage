package direxion

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"etf-pair-arbitrage/core/cex"
)

const testKey = "FAKE-SITE-KEY-777"

// request is one GraphQL request as the server saw it.
type request struct {
	Key       string
	Query     string            `json:"query"`
	Variables map[string]string `json:"variables"`
}

// server is a local stand-in for the site API: it records every request and
// answers with the status and body the test set.
func server(t *testing.T, status int, body *string) (*Client, *[]request) {
	t.Helper()
	var mu sync.Mutex
	var seen []request
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req request
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &req)
		req.Key = r.Header.Get("X-Api-Key")
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("request %s with content type %q, want a JSON POST", r.Method, r.Header.Get("Content-Type"))
		}
		mu.Lock()
		seen = append(seen, req)
		mu.Unlock()
		w.WriteHeader(status)
		w.Write([]byte(*body))
	}))
	t.Cleanup(s.Close)
	c := NewClient(testKey)
	c.endpoint, c.pause = s.URL, 0
	return c, &seen
}

// The NAV of a trading day is asked for with the date as MMDDYYYY and
// rounded to the cent; null is a day the API does not have yet.
func TestNAV(t *testing.T) {
	body := `{"data":{"getThirtyDayPrice":{"ItemKey":"SOXL#11232026","TradeDate":"11232026","Ticker":"SOXL","Nav":42.4861,"MarketClosingPrice":42.51}}}`
	client, seen := server(t, http.StatusOK, &body)
	ctx := context.Background()
	day := time.Date(2026, 11, 23, 0, 0, 0, 0, cex.NewYork)
	nav, ok, err := client.NAV(ctx, "SOXL", day)
	if err != nil || !ok || nav.String() != "42.49" {
		t.Fatalf("NAV = %s, %t, %v; want 42.49", nav, ok, err)
	}
	req := (*seen)[0]
	if req.Key != testKey || req.Variables["Ticker"] != "SOXL" || req.Variables["TradeDate"] != "11232026" || !strings.Contains(req.Query, "getThirtyDayPrice(Ticker: $Ticker, TradeDate: $TradeDate)") {
		t.Errorf("request = %+v", req)
	}

	body = `{"data":{"getThirtyDayPrice":null}}`
	if _, ok, err := client.NAV(ctx, "SOXL", day); ok || err != nil {
		t.Errorf("a day the API does not have: ok %t, err %v; want not ok and no error", ok, err)
	}
	// Another day's NAV is not this day's.
	body = `{"data":{"getThirtyDayPrice":{"TradeDate":"11202026","Ticker":"SOXL","Nav":41.2}}}`
	if _, ok, err := client.NAV(ctx, "SOXL", day); ok || err == nil {
		t.Errorf("another trade date: ok %t, err %v; want an error", ok, err)
	}
	body = `{"data":{"getThirtyDayPrice":{"TradeDate":"11232026","Ticker":"SOXL","Nav":null}}}`
	if _, ok, err := client.NAV(ctx, "SOXL", day); ok || err == nil {
		t.Errorf("a row without a NAV: ok %t, err %v; want an error", ok, err)
	}
	body = `{"data":null,"errors":[{"errorType":"MalformedHttpRequestException","message":"bad query"}]}`
	if _, ok, err := client.NAV(ctx, "SOXL", day); ok || err == nil || !strings.Contains(err.Error(), "MalformedHttpRequestException") {
		t.Errorf("a GraphQL error: ok %t, err %v", ok, err)
	}
}

// The distributions are one per ex-date with the four kinds of cash summed;
// a round listed without amounts is returned as not declared.
func TestDistributions(t *testing.T) {
	body := `{"data":{"getKurtosysFunds":{"Distributions":[
		{"IncomeDividend":0.01234,"ShortTermCapitalGain":0.1,"LongTermCapitalGain":null,"ReturnOfCapital":0.002,"ExDate":"09222026"},
		{"IncomeDividend":0.05,"ShortTermCapitalGain":null,"LongTermCapitalGain":null,"ReturnOfCapital":null,"ExDate":"06232026"},
		{"IncomeDividend":null,"ShortTermCapitalGain":null,"LongTermCapitalGain":0.3,"ReturnOfCapital":null,"ExDate":"06232026"},
		{"IncomeDividend":0,"ShortTermCapitalGain":0,"LongTermCapitalGain":0,"ReturnOfCapital":0,"ExDate":"03242026"},
		{"IncomeDividend":null,"ShortTermCapitalGain":null,"LongTermCapitalGain":null,"ReturnOfCapital":null,"ExDate":"12222026"}],
		"TimeStamp":"2026-11-23","TradeDate":"11232026"}}}`
	client, seen := server(t, http.StatusOK, &body)
	ctx := context.Background()
	got, err := client.Distributions(ctx, "SOXS")
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		exDate, amount string
		declared       bool
	}{{"2026-09-22", "0.11434", true}, {"2026-06-23", "0.35", true}, {"2026-03-24", "0", true}, {"2026-12-22", "0", false}}
	if len(got) != len(want) {
		t.Fatalf("%d distributions %+v, want %d", len(got), got, len(want))
	}
	for i, w := range want {
		if got[i].ExDate != w.exDate || got[i].Amount.String() != w.amount || got[i].Declared != w.declared {
			t.Errorf("distribution %d = %+v, want %+v", i, got[i], w)
		}
	}
	if req := (*seen)[0]; req.Key != testKey || req.Variables["Ticker"] != "SOXS" || !strings.Contains(req.Query, "getKurtosysFunds(Ticker: $Ticker)") {
		t.Errorf("request = %+v", req)
	}
	body = `{"data":{"getKurtosysFunds":null}}`
	if _, err := client.Distributions(ctx, "NOPE"); err == nil {
		t.Error("an answer without fund data was read")
	}
}

// Without a key nothing is sent; a key the API refuses is the same error.
// Neither error carries the key.
func TestKeyMissingOrRefused(t *testing.T) {
	body := `{"errors":[{"errorType":"UnauthorizedException","message":"You are not authorized to make this call."}]}`
	client, seen := server(t, http.StatusUnauthorized, &body)
	ctx := context.Background()
	day := time.Date(2026, 11, 23, 0, 0, 0, 0, cex.NewYork)
	_, _, errNAV := client.NAV(ctx, "SOXL", day)
	_, errDist := client.Distributions(ctx, "SOXL")
	for name, err := range map[string]error{"NAV": errNAV, "Distributions": errDist} {
		if !errors.Is(err, cex.ErrIssuerKey) || strings.Contains(err.Error(), testKey) {
			t.Errorf("%s with a refused key: %v, want cex.ErrIssuerKey without the key", name, err)
		}
	}
	sent := len(*seen)

	client.key = ""
	if _, _, err := client.NAV(ctx, "SOXL", day); !errors.Is(err, cex.ErrIssuerKey) {
		t.Errorf("NAV without a key: %v", err)
	}
	if _, err := client.Distributions(ctx, "SOXL"); !errors.Is(err, cex.ErrIssuerKey) {
		t.Errorf("Distributions without a key: %v", err)
	}
	if len(*seen) != sent {
		t.Error("a request was sent without a key")
	}
	// Another failure is not a key problem.
	body = `oops`
	client2, _ := server(t, http.StatusInternalServerError, &body)
	if _, _, err := client2.NAV(ctx, "SOXL", day); err == nil || errors.Is(err, cex.ErrIssuerKey) {
		t.Errorf("http 500: %v, want an error that is not the key's", err)
	}
}

// Every query waits the pause first.
func TestQueryPause(t *testing.T) {
	body := `{"data":{"getThirtyDayPrice":null}}`
	client, _ := server(t, http.StatusOK, &body)
	client.pause = 60 * time.Millisecond
	began := time.Now()
	client.NAV(context.Background(), "SOXL", time.Now())
	if elapsed := time.Since(began); elapsed < 60*time.Millisecond {
		t.Errorf("the query went out after %s, before the pause", elapsed)
	}
	// A context that ends during the pause ends the query.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, _, err := client.NAV(ctx, "SOXL", time.Now()); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a query cancelled during the pause: %v", err)
	}
}

// Against the real site API, gated by DIREXION_READ_TEST=1 with the key in
// DIREXION_SITE_API_KEY: one NAV and one distributions read of SOXL, at the
// client's own pace. Reading the issuer needs the owner's key; without it
// the test is skipped.
func TestLiveDirexionRead(t *testing.T) {
	key := os.Getenv("DIREXION_SITE_API_KEY")
	if os.Getenv("DIREXION_READ_TEST") != "1" || key == "" {
		t.Skip("DIREXION_READ_TEST is not 1 or DIREXION_SITE_API_KEY is empty: the live read of the issuer is skipped")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := NewClient(key)
	calendar, err := cex.NewCalendar(nil)
	if err != nil {
		t.Fatal(err)
	}
	session := calendar.SessionAt(time.Now())
	day, _ := time.ParseInLocation("2006-01-02", session.ID, cex.NewYork)
	nav, ok, err := client.NAV(ctx, "SOXL", day)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("SOXL NAV of %s: %s (published: %t)", session.ID, nav, ok)
	distributions, err := client.Distributions(ctx, "SOXL")
	if err != nil || len(distributions) == 0 {
		t.Fatalf("Distributions = %d, %v", len(distributions), err)
	}
	t.Logf("SOXL distributions: %d, the first %+v", len(distributions), distributions[0])
}
