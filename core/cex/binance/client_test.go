package binance

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/cex/internal/livetest"
)

// server answers GET requests by path and records their queries.
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

// The index close is the close of the kline that opens at exactly the minute
// asked for. The endpoint returns the first kline at or after it, so a later
// one means the minute has none.
func TestIndexClose(t *testing.T) {
	minute := time.Date(2026, 11, 23, 20, 59, 0, 0, time.UTC) // 15:59 New York
	kline := func(openMs int64) string {
		return fmt.Sprintf(`[[%d,"42.50000000","42.61000000","42.48000000","42.57310000","0",%d,"0",60,"0","0","0"]]`, openMs, openMs+59999)
	}
	body := kline(minute.UnixMilli())
	client, seen := server(t, func(path, query string) (int, string) { return http.StatusOK, body })
	ctx := context.Background()

	price, ok, err := client.IndexClose(ctx, "SOXLUSDT", minute)
	if err != nil || !ok || price.String() != "42.5731" {
		t.Fatalf("IndexClose = %s, %t, %v; want 42.5731", price, ok, err)
	}
	want := fmt.Sprintf("GET /fapi/v1/indexPriceKlines?interval=1m&limit=1&pair=SOXLUSDT&startTime=%d", minute.UnixMilli())
	if (*seen)[0] != want {
		t.Errorf("request = %s\n   want %s", (*seen)[0], want)
	}

	// The next kline instead of the one asked for: not read.
	body = kline(minute.Add(time.Minute).UnixMilli())
	if _, ok, err := client.IndexClose(ctx, "SOXLUSDT", minute); ok || err != nil {
		t.Errorf("a later kline: ok %t, err %v; want not ok and no error", ok, err)
	}
	// No kline at all.
	body = `[]`
	if _, ok, err := client.IndexClose(ctx, "SOXLUSDT", minute); ok || err != nil {
		t.Errorf("no kline: ok %t, err %v", ok, err)
	}
	// A body that is not klines, and a refusal, are errors.
	body = `{"code":-1121,"msg":"Invalid symbol."}`
	if _, ok, err := client.IndexClose(ctx, "NOPEUSDT", minute); ok || err == nil {
		t.Errorf("an error body: ok %t, err %v", ok, err)
	}
	client, _ = server(t, func(string, string) (int, string) { return http.StatusTooManyRequests, `{"code":-1003}` })
	if _, ok, err := client.IndexClose(ctx, "SOXLUSDT", minute); ok || err == nil || !strings.Contains(err.Error(), "http 429") {
		t.Errorf("http 429: ok %t, err %v", ok, err)
	}
}

// The schedule's regular sessions of the US equity market become trading
// days with their New York date; other session types and other markets are
// left out.
func TestTradingSchedule(t *testing.T) {
	ms := func(t time.Time) int64 { return t.UnixMilli() }
	ny := func(day, hour, minute int) time.Time {
		return time.Date(2026, 11, day, hour, minute, 0, 0, cex.NewYork)
	}
	body := fmt.Sprintf(`{"updateTime":1,"marketSchedules":{"EQUITY":{"sessions":[
		{"startTime":%d,"endTime":%d,"type":"PRE_MARKET"},
		{"startTime":%d,"endTime":%d,"type":"REGULAR"},
		{"startTime":%d,"endTime":%d,"type":"AFTER_MARKET"},
		{"startTime":%d,"endTime":%d,"type":"NO_TRADING"},
		{"startTime":%d,"endTime":%d,"type":"REGULAR"}]},
		"COMMODITY":{"sessions":[{"startTime":%d,"endTime":%d,"type":"REGULAR"}]}}}`,
		ms(ny(27, 4, 0)), ms(ny(27, 9, 30)), ms(ny(27, 9, 30)), ms(ny(27, 13, 0)), ms(ny(27, 13, 0)), ms(ny(27, 17, 0)),
		ms(ny(26, 0, 0)), ms(ny(26, 20, 0)), ms(ny(25, 9, 30)), ms(ny(25, 16, 0)), ms(ny(26, 9, 30)), ms(ny(26, 16, 0)))
	client, seen := server(t, func(string, string) (int, string) { return http.StatusOK, body })
	days, err := client.TradingSchedule(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 2 || days[0].Date != "2026-11-25" || !days[0].RegularOpen.Equal(ny(25, 9, 30)) || !days[0].RegularClose.Equal(ny(25, 16, 0)) ||
		days[1].Date != "2026-11-27" || !days[1].RegularClose.Equal(ny(27, 13, 0)) {
		t.Errorf("days = %+v, want the 25th and the early close of the 27th, oldest first", days)
	}
	if (*seen)[0] != "GET /fapi/v1/tradingSchedule?" {
		t.Errorf("request = %s", (*seen)[0])
	}
}

// Against the real endpoints (public, read-only), gated by
// BINANCE_PUBLIC_TEST=1: the index close of the minute before the last
// regular close of both contracts, and the schedule against the calendar.
// Every response is saved as received under experiments/core_step2/.
func TestLiveBinancePublic(t *testing.T) {
	if os.Getenv("BINANCE_PUBLIC_TEST") != "1" {
		t.Skip("BINANCE_PUBLIC_TEST is not 1: the live read of Binance's public endpoints is skipped")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := NewClient()
	recorder := &livetest.Recorder{}
	client.http.Transport = recorder
	defer func() {
		path, err := recorder.Save("binance_public_raw")
		if err != nil {
			t.Error(err)
		}
		t.Logf("saved %d raw responses to %s", len(recorder.Responses()), path)
	}()
	calendar, err := cex.NewCalendar(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	session := calendar.SessionAt(now)
	minute := session.Start.Add(-time.Minute)
	for _, symbol := range []cex.Symbol{"SOXLUSDT", "SOXSUSDT"} {
		price, ok, err := client.IndexClose(ctx, symbol, minute)
		if err != nil || !ok || !price.IsPositive() {
			t.Errorf("IndexClose(%s, %s) = %s, %t, %v", symbol, minute.UTC().Format(time.RFC3339), price, ok, err)
		}
		t.Logf("%s index close of %s (the minute before the close of session %s): %s", symbol, minute.UTC().Format(time.RFC3339), session.ID, price)
	}
	// A minute that has not begun has no kline.
	if _, ok, err := client.IndexClose(ctx, "SOXLUSDT", now.Add(time.Hour).Truncate(time.Minute)); ok || err != nil {
		t.Errorf("a future minute: ok %t, err %v; want not ok and no error", ok, err)
	}
	days, err := client.TradingSchedule(ctx)
	if err != nil || len(days) == 0 {
		t.Fatalf("TradingSchedule = %d days, %v", len(days), err)
	}
	differences := calendar.CompareSchedule(days)
	t.Logf("schedule: %d regular sessions from %s to %s; differences from the calendar: %v", len(days), days[0].Date, days[len(days)-1].Date, differences)
	if len(differences) != 0 {
		t.Errorf("the calendar differs from Binance's schedule: %v", differences)
	}
}
