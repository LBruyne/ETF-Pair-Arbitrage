package coinbase

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"etf-pair-arbitrage/core/cex/internal/livetest"
)

// server answers GET requests and records them.
func server(t *testing.T, answer func(path, query string) (int, string)) (*Client, *[]string) {
	t.Helper()
	var seen []string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		status, body := answer(r.URL.Path, r.URL.RawQuery)
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	c := NewClient()
	c.baseURL = s.URL
	return c, &seen
}

func TestQuote(t *testing.T) {
	body := `{"ask":"0.99962","bid":"0.99961","volume":"150331284.07","trade_id":88313463,"price":"0.99962","size":"25.5","time":"2026-11-24T09:12:30.117000Z"}`
	client, seen := server(t, func(string, string) (int, string) { return http.StatusOK, body })
	ctx := context.Background()
	bid, ask, err := client.Quote(ctx)
	if err != nil || bid.String() != "0.99961" || ask.String() != "0.99962" {
		t.Fatalf("Quote = %s, %s, %v", bid, ask, err)
	}
	if (*seen)[0] != "GET /products/USDT-USD/ticker?" {
		t.Errorf("request = %s", (*seen)[0])
	}
	body = `{"message":"NotFound"}`
	if _, _, err := client.Quote(ctx); err == nil {
		t.Error("a ticker without bid and ask was read")
	}
	client, _ = server(t, func(string, string) (int, string) { return http.StatusTooManyRequests, `{"message":"rate limited"}` })
	if _, _, err := client.Quote(ctx); err == nil || !strings.Contains(err.Error(), "http 429") {
		t.Errorf("http 429: %v", err)
	}
}

// One minute's candle is asked for by its own minute and read by its time;
// a minute without a trade has no candle.
func TestMinuteClose(t *testing.T) {
	minute := time.Date(2026, 11, 23, 20, 59, 0, 0, time.UTC)
	body := `[[1795467540,0.9995,0.9996,0.99955,0.99958,81234.5]]` // [time, low, high, open, close, volume]
	if minute.Unix() != 1795467540 {
		t.Fatal("the fixture's time is not the minute")
	}
	client, seen := server(t, func(string, string) (int, string) { return http.StatusOK, body })
	ctx := context.Background()
	price, ok, err := client.MinuteClose(ctx, minute)
	if err != nil || !ok || price.String() != "0.99958" {
		t.Fatalf("MinuteClose = %s, %t, %v; want the candle's close 0.99958", price, ok, err)
	}
	if want := "GET /products/USDT-USD/candles?end=2026-11-23T20%3A59%3A00Z&granularity=60&start=2026-11-23T20%3A59%3A00Z"; (*seen)[0] != want {
		t.Errorf("request = %s\n   want %s", (*seen)[0], want)
	}
	// The source answers with a neighbouring minute only: not this one's.
	body = `[[1795467600,0.9995,0.9996,0.99955,0.9997,10]]`
	if _, ok, err := client.MinuteClose(ctx, minute); ok || err != nil {
		t.Errorf("another minute's candle: ok %t, err %v", ok, err)
	}
	body = `[]`
	if _, ok, err := client.MinuteClose(ctx, minute); ok || err != nil {
		t.Errorf("no candle: ok %t, err %v", ok, err)
	}
}

// A range is read in pages of 300 minutes and returned oldest first, without
// the minutes that had no trade.
func TestMinuteClosesPagesARange(t *testing.T) {
	from := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	to := from.Add(450 * time.Minute)
	client, seen := server(t, func(_ string, query string) (int, string) {
		if strings.Contains(query, "start=2026-11-23T12%3A00%3A00Z") {
			// newest first, as the source sends; minute 1 had no trade
			return http.StatusOK, `[[` + itoa(from.Add(299*time.Minute).Unix()) + `,1,1,1,0.9993,5],[` + itoa(from.Add(2*time.Minute).Unix()) + `,1,1,1,0.9992,5],[` + itoa(from.Unix()) + `,1,1,1,0.9991,5]]`
		}
		return http.StatusOK, `[[` + itoa(from.Add(449*time.Minute).Unix()) + `,1,1,1,0.9995,5],[` + itoa(from.Add(300*time.Minute).Unix()) + `,1,1,1,0.9994,5]]`
	})
	got, err := client.MinuteCloses(context.Background(), from, to)
	if err != nil {
		t.Fatal(err)
	}
	var closes []string
	for _, c := range got {
		closes = append(closes, c.Close.String())
	}
	if strings.Join(closes, " ") != "0.9991 0.9992 0.9993 0.9994 0.9995" || !got[1].Minute.Equal(from.Add(2*time.Minute)) {
		t.Errorf("closes = %v", got)
	}
	want := []string{
		"GET /products/USDT-USD/candles?end=2026-11-23T16%3A59%3A00Z&granularity=60&start=2026-11-23T12%3A00%3A00Z",
		"GET /products/USDT-USD/candles?end=2026-11-23T19%3A29%3A00Z&granularity=60&start=2026-11-23T17%3A00%3A00Z",
	}
	if len(*seen) != 2 || (*seen)[0] != want[0] || (*seen)[1] != want[1] {
		t.Errorf("requests = %q\n    want %q", *seen, want)
	}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// Against the real endpoints (public, read-only), gated by
// COINBASE_PUBLIC_TEST=1: the ticker, a short range of candles and one
// minute's candle. Every response is saved as received under
// experiments/core_step2/.
func TestLiveCoinbasePublic(t *testing.T) {
	if os.Getenv("COINBASE_PUBLIC_TEST") != "1" {
		t.Skip("COINBASE_PUBLIC_TEST is not 1: the live read of Coinbase's public endpoints is skipped")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := NewClient()
	recorder := &livetest.Recorder{}
	client.http.Transport = recorder
	defer func() {
		path, err := recorder.Save("coinbase_public_raw")
		if err != nil {
			t.Error(err)
		}
		t.Logf("saved %d raw responses to %s", len(recorder.Responses()), path)
	}()
	now := time.Now()
	bid, ask, err := client.Quote(ctx)
	if err != nil || !bid.IsPositive() || bid.GreaterThan(ask) {
		t.Fatalf("Quote = %s, %s, %v", bid, ask, err)
	}
	t.Logf("USDT-USD bid %s ask %s", bid, ask)
	to := now.Truncate(time.Minute).Add(-time.Minute)
	from := to.Add(-30 * time.Minute)
	closes, err := client.MinuteCloses(ctx, from, to)
	if err != nil || len(closes) == 0 {
		t.Fatalf("MinuteCloses = %d candles, %v", len(closes), err)
	}
	for i, c := range closes {
		if c.Minute.Before(from) || !c.Minute.Before(to) || !c.Close.IsPositive() || (i > 0 && !c.Minute.After(closes[i-1].Minute)) {
			t.Errorf("candle %d = %+v is outside [%s, %s), not positive or out of order", i, c, from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339))
		}
	}
	last := closes[len(closes)-1]
	price, ok, err := client.MinuteClose(ctx, last.Minute)
	if err != nil || !ok || !price.Equal(last.Close) {
		t.Errorf("MinuteClose(%s) = %s, %t, %v; the range read gave %s", last.Minute.UTC().Format(time.RFC3339), price, ok, err, last.Close)
	}
	t.Logf("%d of 30 minutes have a candle; the last, %s, closed at %s", len(closes), last.Minute.UTC().Format(time.RFC3339), last.Close)
}
