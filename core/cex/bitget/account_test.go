package bitget

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"etf-pair-arbitrage/core/cex"
)

// The account of these tests. Nothing here is a real credential, id,
// quantity or amount: the fixtures are written by hand in the shape of the
// exchange's answers.
var (
	fakeCreds = cex.Credentials{Key: "FAKE-KEY-111", Secret: "FAKE-SECRET-222", Passphrase: "FAKE-PASS-333"}
	fixedNow  = time.UnixMilli(1795580000000)
)

const (
	ourOrder   = "etfp1795579990000-0001BE" // an id of this program
	otherOrder = "web-7f3a19c2"             // an order another program placed on the same account
)

// The signature is the base64 HMAC-SHA256 of the prehash under the secret.
// The first two vectors are the exchange's documented examples under this
// test's secret, the others a request without a query and the stream login;
// all four were computed outside this program.
func TestSign(t *testing.T) {
	for prehash, want := range map[string]string{
		"16273667805456GET/api/v3/account/fee-rate?category=SPOT&symbol=BTCUSDT":                                                                                             "K1tgAdxOrA5C+N97r6qQKb0j9eh+U87SUa8nzdpmn1s=",
		`16273667805456POST/api/v3/trade/place-order{"category":"SPOT","symbol":"BTCUSDT","side":"buy","orderType":"limit","qty":"0.1","price":"30000","timeInForce":"gtc"}`: "XURM8BeWEabl+dCibfZw36p3AYJiqG//iyq54MI2Vog=",
		"1795580000000GET/api/v3/account/settings": "4f4rcjW3OB5PU2+hD7zWK95H+v2PJlEyfLvPw89Gq+A=",
		"1795580000000GET/user/verify":             "zAcRo/gzljl+Rm8PBOWJK6AmnUZ34b1tcoEBHjSAcgM=",
	} {
		if got := sign("FAKE-SECRET-222", prehash); got != want {
			t.Errorf("sign(%q) = %s, want %s", prehash, got, want)
		}
	}
}

// --- a local REST server for the account side ---

// restCall is one request as the local server received it.
type restCall struct {
	method, path, query, body string
	header                    http.Header
}

func (c restCall) target() string { return c.method + " " + c.path + "?" + c.query }

// accountServer is a local stand-in for the exchange's signed REST API. It
// records every request and answers with what answer returns: an HTTP
// status and a body.
type accountServer struct {
	*httptest.Server
	mu    sync.Mutex
	calls []restCall
}

func newAccountServer(t *testing.T, answer func(restCall) (int, string)) (*Client, *accountServer) {
	t.Helper()
	s := &accountServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		call := restCall{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, body: string(body), header: r.Header.Clone()}
		s.mu.Lock()
		s.calls = append(s.calls, call)
		s.mu.Unlock()
		status, answer := answer(call)
		w.Header().Set("X-BG-REQUEST-ACCEPT-TIME", "1795580000009") // on every response of the exchange
		w.WriteHeader(status)
		w.Write([]byte(answer))
	}))
	t.Cleanup(s.Close)
	client := testClient(s.URL)
	client.creds, client.now = fakeCreds, func() time.Time { return fixedNow }
	return client, s
}

func (s *accountServer) seen() []restCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]restCall(nil), s.calls...)
}

// assertSigned checks the headers of one signed request: the key, the
// passphrase, the client's clock in unix milliseconds, and a signature that
// is the HMAC of timestamp + METHOD + path [+ "?" + query] + body, computed
// here from what the server received.
func assertSigned(t *testing.T, call restCall) {
	t.Helper()
	prehash := "1795580000000" + call.method + call.path
	if call.query != "" {
		prehash += "?" + call.query
	}
	mac := hmac.New(sha256.New, []byte(fakeCreds.Secret))
	mac.Write([]byte(prehash + call.body))
	for name, want := range map[string]string{"ACCESS-KEY": "FAKE-KEY-111", "ACCESS-PASSPHRASE": "FAKE-PASS-333", "ACCESS-TIMESTAMP": "1795580000000",
		"ACCESS-SIGN": base64.StdEncoding.EncodeToString(mac.Sum(nil)), "Content-Type": "application/json", "locale": "en-US"} {
		if got := call.header.Get(name); got != want {
			t.Errorf("%s: header %s = %q, want %q", call.target(), name, got, want)
		}
	}
}

func ok(data string) (int, string) {
	return http.StatusOK, `{"code":"00000","msg":"success","requestTime":1795580000123,"data":` + data + `}`
}

// refusal is the exchange's error envelope, which comes with HTTP 400.
func refusal(code, msg string) (int, string) {
	return http.StatusBadRequest, fmt.Sprintf(`{"code":%q,"msg":%q,"requestTime":1795580000123,"data":null}`, code, msg)
}

// --- settings ---

const settingsOneWay = `{"uid":"9000000001","accountMode":"unified","assetMode":"multi_assets","holdMode":"one_way_mode","stpMode":"none","accountLevel":"1","deltaSwitch":"off","repayMode":"auto","symbolConfigList":[` +
	`{"category":"USDT-FUTURES","symbol":"BTCUSDT","marginMode":"isolated","leverage":"3","longLeverage":null,"shortLeverage":null},` +
	`{"category":"USDT-FUTURES","symbol":"SOXLUSDT","marginMode":"crossed","leverage":"10","longLeverage":null,"shortLeverage":null},` +
	`{"category":"MARGIN","symbol":"SOXLUSDT","marginMode":"isolated","leverage":"2","longLeverage":null,"shortLeverage":null},` +
	`{"category":"USDT-FUTURES","symbol":"SOXSUSDT","marginMode":"isolated","leverage":"5","longLeverage":null,"shortLeverage":null}],"coinConfigList":[]}`

// The settings are read with one signed GET; the position mode is the
// account's, margin mode and leverage are the futures rows of the asked
// contracts, and a contract without such a row has no entry.
func TestAccountSettings(t *testing.T) {
	data := settingsOneWay
	client, server := newAccountServer(t, func(restCall) (int, string) { return ok(data) })
	ctx := context.Background()
	got, err := client.AccountSettings(ctx, []cex.Symbol{"SOXLUSDT", "SOXSUSDT", "NVDAUSDT"})
	if err != nil {
		t.Fatal(err)
	}
	want := cex.AccountSettings{OneWay: true, BySymbol: map[cex.Symbol]cex.SymbolSetting{"SOXLUSDT": {Crossed: true, Leverage: 10}, "SOXSUSDT": {Crossed: false, Leverage: 5}}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("settings = %+v, want %+v", got, want)
	}
	calls := server.seen()
	if len(calls) != 1 || calls[0].target() != "GET /api/v3/account/settings?" || calls[0].body != "" {
		t.Fatalf("requests = %+v, want one GET of the settings without a query or a body", calls)
	}
	assertSigned(t, calls[0])
	if got := calls[0].header.Get("ACCESS-SIGN"); got != "4f4rcjW3OB5PU2+hD7zWK95H+v2PJlEyfLvPw89Gq+A=" {
		t.Errorf("ACCESS-SIGN = %s, want the vector of 1795580000000GET/api/v3/account/settings", got)
	}

	// Hedge mode is not one-way.
	data = strings.Replace(settingsOneWay, "one_way_mode", "hedge_mode", 1)
	if got, err := client.AccountSettings(ctx, []cex.Symbol{"SOXLUSDT"}); err != nil || got.OneWay || len(got.BySymbol) != 1 {
		t.Errorf("hedge mode: %+v, %v", got, err)
	}
}

// --- positions ---

func positionAnswer(symbol, side, total, avg string) string {
	return fmt.Sprintf(`{"list":[{"category":"USDT-FUTURES","symbol":%q,"marginCoin":"USDT","posSide":%q,"holdMode":"one_way_mode","marginMode":"crossed","positionBalance":"61.7","available":%q,"frozen":"0","total":%q,"leverage":"10","curRealisedPnl":"-0.31","avgPrice":%q,"positionStatus":"opening","unrealisedPnl":"1.02","liquidationPrice":"0","mmr":"0.01","profitRate":"0.016","markPrice":"43.02","breakEvenPrice":"42.99","totalFunding":"0","openFeeTotal":"-0.3","closeFeeTotal":"0","cashDividend":"0","createdTime":"1795570000111","updatedTime":"1795570000222"}]}`,
		symbol, side, total, total, avg)
}

// Positions are read one contract per request; a long position is positive,
// a short one negative, and a contract the exchange lists nothing for is
// flat: it has no entry in a loaded set.
func TestPositions(t *testing.T) {
	client, server := newAccountServer(t, func(c restCall) (int, string) {
		switch c.query {
		case "category=USDT-FUTURES&symbol=SOXLUSDT":
			return ok(positionAnswer("SOXLUSDT", "long", "14.42", "42.93"))
		case "category=USDT-FUTURES&symbol=SOXSUSDT":
			return ok(positionAnswer("SOXSUSDT", "short", "12.5", "49.512"))
		}
		return ok(`{"list":null}`)
	})
	ctx := context.Background()
	got, err := client.Positions(ctx, []cex.Symbol{"SOXLUSDT", "SOXSUSDT", "NVDAUSDT"})
	if err != nil {
		t.Fatal(err)
	}
	bull, bear := got.BySymbol["SOXLUSDT"], got.BySymbol["SOXSUSDT"]
	if !got.Loaded || !got.ReceivedAt.Equal(fixedNow) || len(got.BySymbol) != 2 ||
		bull.Symbol != "SOXLUSDT" || !bull.Qty.Equal(d("14.42")) || !bull.AvgPrice.Equal(d("42.93")) || !bull.UpdatedAt.Equal(time.UnixMilli(1795570000222)) ||
		!bear.Qty.Equal(d("-12.5")) || !bear.AvgPrice.Equal(d("49.512")) {
		t.Errorf("positions = %+v, want +14.42 @ 42.93 and -12.5 @ 49.512, the third contract flat", got)
	}
	if !bull.MarkPrice.Valid || !bull.MarkPrice.Decimal.Equal(d("43.02")) || !bull.UnrealisedPnL.Valid || !bull.UnrealisedPnL.Decimal.Equal(d("1.02")) ||
		!bull.LiqPrice.Valid || !bull.LiqPrice.Decimal.IsZero() {
		t.Errorf("bull mark %v, unrealised PnL %v, liquidation price %v; want 43.02, 1.02 and 0 as the exchange reports them", bull.MarkPrice, bull.UnrealisedPnL, bull.LiqPrice)
	}
	calls := server.seen()
	if len(calls) != 3 {
		t.Fatalf("%d requests, want one per contract", len(calls))
	}
	for i, symbol := range []string{"SOXLUSDT", "SOXSUSDT", "NVDAUSDT"} {
		if want := "GET /api/v3/position/current-position?category=USDT-FUTURES&symbol=" + symbol; calls[i].target() != want {
			t.Errorf("request %d = %s, want %s", i, calls[i].target(), want)
		}
		assertSigned(t, calls[i])
	}
}

// The fields kept for the board: REST names the liquidation price
// liquidationPrice, the private stream liqPrice. A field the row does not
// carry, or carries empty, is not valid and does not fail the position.
func TestPositionBoardFields(t *testing.T) {
	var pushed, bare positionRow
	if err := json.Unmarshal([]byte(positionPush("SOXSUSDT", "short", "12.5", "49.5", "opening")), &pushed); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"symbol":"SOXLUSDT","posSide":"long","total":"3","avgPrice":"42","markPrice":"","updatedTime":"1795570000222"}`), &bare); err != nil {
		t.Fatal(err)
	}
	positions := map[cex.Symbol]cex.Position{}
	for _, row := range []positionRow{pushed, bare} {
		if err := addPosition(positions, row); err != nil {
			t.Fatal(err)
		}
	}
	bear, bull := positions["SOXSUSDT"], positions["SOXLUSDT"]
	if !bear.Qty.Equal(d("-12.5")) || !bear.MarkPrice.Decimal.Equal(d("42.9")) || !bear.UnrealisedPnL.Decimal.Equal(d("0.4")) || !bear.LiqPrice.Valid || !bear.LiqPrice.Decimal.IsZero() {
		t.Errorf("pushed position = %+v, want -12.5 with mark 42.9, unrealised PnL 0.4 and liquidation price 0", bear)
	}
	if !bull.Qty.Equal(d("3")) || bull.MarkPrice.Valid || bull.UnrealisedPnL.Valid || bull.LiqPrice.Valid {
		t.Errorf("position without the fields = %+v, want 3 with none of them valid", bull)
	}
}

// The account's equity and margin ratio are read with one signed GET of
// the account assets: usdtEquity and mgnRatio as the exchange reports
// them, received now.
func TestAccountAssets(t *testing.T) {
	client, server := newAccountServer(t, func(restCall) (int, string) {
		return ok(`{"accountEquity":"1233.9","usdtEquity":"1234.5","btcEquity":"0.0125","unrealisedPnl":"1.4","usdtUnrealisedPnl":"1.4","btcUnrealizedPnl":"0","effEquity":"1100.2","mmr":"15.2","imr":"120.4","mgnRatio":"0.0123","positionMgnRatio":"0.0123","positionValue":"1204","leverage":"1","assets":[{"coin":"USDT","equity":"1234.5","usdValue":"1233.9","balance":"1233.1","available":"1100","debt":"0","locked":"0","bonus":"0","interestBase":"0","balanceOriginal":"1233.1","collateralLimitStatus":"normal"}]}`)
	})
	var reader cex.AccountAssetsReader = client
	got, err := reader.AccountAssets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equity.Equal(d("1234.5")) || !got.MarginRatio.Equal(d("0.0123")) || !got.ReceivedAt.Equal(fixedNow) {
		t.Errorf("account assets = %+v, want equity 1234.5 USDT and margin ratio 0.0123, received now", got)
	}
	calls := server.seen()
	if len(calls) != 1 || calls[0].target() != "GET /api/v3/account/assets?" {
		t.Fatalf("requests %v, want one GET of /api/v3/account/assets", calls)
	}
	assertSigned(t, calls[0])
}

// --- orders ---

// orderAnswer is one order in the shape of order-info and of a
// history-orders row.
func orderAnswer(clientOid, symbol, side, reduceOnly, status, price, qty, execQty, execValue, avg, fee, cancelReason string) string {
	feeDetail := "[]"
	if fee != "" {
		feeDetail = fmt.Sprintf(`[{"feeCoin":"USDT","fee":%q}]`, fee)
	}
	return fmt.Sprintf(`{"orderId":"1380000000000000017","clientOid":%q,"category":"USDT-FUTURES","symbol":%q,"orderType":"limit","side":%q,"price":%q,"qty":%q,"amount":"0","cumExecQty":%q,"cumExecValue":%q,"avgPrice":%q,"timeInForce":"ioc","orderStatus":%q,"posSide":"long","tradeSide":"open","holdMode":"one_way_mode","reduceOnly":%q,"feeDetail":%s,"createdTime":"1795579990100","updatedTime":"1795579990160","cancelReason":%q,"execType":"normal","stpMode":"none","tpTriggerBy":null,"slTriggerBy":null,"takeProfit":null,"stopLoss":null,"tpOrderType":null,"slOrderType":null,"tpLimitPrice":null,"slLimitPrice":null,"marginMode":"crossed","delegateType":"normal","venue":"","rTokenTimeInForce":""}`,
		clientOid, symbol, side, price, qty, execQty, execValue, avg, status, reduceOnly, feeDetail, cancelReason)
}

// An order is read by its client id with one signed GET. The record carries
// what the exchange knows; the fee is the fee detail rounded to eight
// decimals and counts as known once it is not zero beside a fill. The
// exchange's "Order does not exist" is cex.ErrUnknownOrder.
func TestOrderByClientID(t *testing.T) {
	answers := map[string]func() (int, string){
		ourOrder: func() (int, string) {
			return ok(orderAnswer(ourOrder, "SOXLUSDT", "buy", "NO", "filled", "42.95", "11.68", "11.68", "501.4224", "42.93", "0.300853440000000017", ""))
		},
		"etfp1795579990000-0002RE": func() (int, string) {
			return ok(orderAnswer("etfp1795579990000-0002RE", "SOXSUSDT", "sell", "YES", "cancelled", "49.4", "10.1", "4.05", "200.0943", "49.406", "0.12005658", "ioc_cancel"))
		},
		"etfp1795579990000-0003BE": func() (int, string) {
			return ok(orderAnswer("etfp1795579990000-0003BE", "SOXLUSDT", "buy", "NO", "cancelled", "40", "11.68", "0", "0", "0", "", "limit_price_exceed_cancel"))
		},
		"etfp1795579990000-0004BE": func() (int, string) {
			return ok(orderAnswer("etfp1795579990000-0004BE", "SOXLUSDT", "buy", "NO", "partially_filled", "42.95", "11.68", "3", "128.79", "42.93", "0", ""))
		},
		"etfp1795579990000-0005BE": func() (int, string) {
			return ok(orderAnswer("etfp1795579990000-0005BE", "SOXLUSDT", "buy", "NO", "live", "42.95", "11.68", "0", "0", "0", "", ""))
		},
		"etfp1795579990000-0006BE": func() (int, string) {
			return ok(orderAnswer("etfp1795579990000-0006BE", "SOXLUSDT", "buy", "NO", "frozen", "42.95", "11.68", "0", "0", "0", "", ""))
		},
		"etfp1795579990000-0007BE": func() (int, string) { return refusal("25204", "Order does not exist") },
		"etfp1795579990000-0008BE": func() (int, string) { return refusal("40913", "orderId or clientOid must be passed one") },
	}
	client, server := newAccountServer(t, func(c restCall) (int, string) {
		values, _ := url.ParseQuery(c.query)
		return answers[values.Get("clientOid")]()
	})
	ctx := context.Background()

	// A filled order.
	rec, err := client.OrderByClientID(ctx, "SOXLUSDT", ourOrder)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Request.ClientOrderID != ourOrder || rec.Request.Symbol != "SOXLUSDT" || rec.Request.Side != cex.SideBuy || rec.Request.ReduceOnly ||
		!rec.Request.Price.Equal(d("42.95")) || !rec.Request.Qty.Equal(d("11.68")) || rec.OrderID != "1380000000000000017" || rec.Status != cex.StatusFilled ||
		!rec.CumExecQty.Equal(d("11.68")) || !rec.CumExecValue.Equal(d("501.4224")) || !rec.AvgPrice.Equal(d("42.93")) ||
		!rec.Fee.Equal(d("0.30085344")) || !rec.FeeKnown || rec.Unknown || rec.CancelReason != "" ||
		!rec.SubmittedAt.Equal(time.UnixMilli(1795579990100)) || !rec.ExchangeUpdatedAt.Equal(time.UnixMilli(1795579990160)) {
		t.Errorf("filled order = %+v", rec)
	}
	calls := server.seen()
	if len(calls) != 1 || calls[0].target() != "GET /api/v3/trade/order-info?clientOid="+ourOrder {
		t.Fatalf("requests = %+v, want one GET of order-info by the client id", calls)
	}
	assertSigned(t, calls[0])
	if got := calls[0].header.Get("ACCESS-SIGN"); got != "BoikRHnPYvybdKelJ3qCAUBq15iKIJh3CKA+VeYwT7I=" {
		t.Errorf("ACCESS-SIGN = %s, want the vector of the request with its query", got)
	}

	// A reducing sell order, cancelled after a part was filled.
	rec, err = client.OrderByClientID(ctx, "SOXSUSDT", "etfp1795579990000-0002RE")
	if err != nil || rec.Status != cex.StatusCancelled || !rec.Request.ReduceOnly || rec.Request.Side != cex.SideSell || !rec.CumExecQty.Equal(d("4.05")) ||
		!rec.Fee.Equal(d("0.12005658")) || !rec.FeeKnown || rec.CancelReason != "ioc_cancel" || rec.ErrorKind != "" {
		t.Errorf("partly filled cancelled order = %+v, %v", rec, err)
	}
	// Cancelled without a fill because its price was outside the band: no
	// fee to know, the reason kept, and the price-limit kind.
	rec, err = client.OrderByClientID(ctx, "SOXLUSDT", "etfp1795579990000-0003BE")
	if err != nil || rec.Status != cex.StatusCancelled || !rec.CumExecQty.IsZero() || rec.FeeKnown || !rec.Fee.IsZero() || rec.CancelReason != "limit_price_exceed_cancel" || rec.ErrorKind != cex.ErrorPriceLimit {
		t.Errorf("cancelled order without a fill = %+v, %v", rec, err)
	}
	// Not final yet: acked; a fill whose fee still reads zero has no known fee.
	rec, err = client.OrderByClientID(ctx, "SOXLUSDT", "etfp1795579990000-0004BE")
	if err != nil || rec.Status != cex.StatusAcked || !rec.CumExecQty.Equal(d("3")) || rec.FeeKnown {
		t.Errorf("partially filled order = %+v, %v", rec, err)
	}
	if rec, err = client.OrderByClientID(ctx, "SOXLUSDT", "etfp1795579990000-0005BE"); err != nil || rec.Status != cex.StatusAcked {
		t.Errorf("live order = %+v, %v", rec, err)
	}
	// A status this client does not know is an error, not a guess.
	if _, err = client.OrderByClientID(ctx, "SOXLUSDT", "etfp1795579990000-0006BE"); err == nil || !strings.Contains(err.Error(), `status "frozen"`) {
		t.Errorf("unknown status: %v", err)
	}
	// "Order does not exist" (HTTP 400) is the unknown-order answer; any
	// other refusal stays a VenueError with its code.
	if _, err = client.OrderByClientID(ctx, "SOXLUSDT", "etfp1795579990000-0007BE"); !errors.Is(err, cex.ErrUnknownOrder) {
		t.Errorf("25204: %v, want cex.ErrUnknownOrder", err)
	}
	var venueErr *cex.VenueError
	if _, err = client.OrderByClientID(ctx, "SOXLUSDT", "etfp1795579990000-0008BE"); errors.Is(err, cex.ErrUnknownOrder) || !errors.As(err, &venueErr) || venueErr.Code != "40913" || venueErr.Kind != cex.ErrorOther {
		t.Errorf("40913: %v, want a VenueError with the code", err)
	}
}

// The finished orders of a contract are read page by page: every page asks
// for 100 rows of the window from since until now, the next one with the
// cursor of the one before, until a page comes back short.
func TestRecentOrders(t *testing.T) {
	page := func(first, n int, cursor string) string {
		rows := make([]string, n)
		for i := range rows {
			rows[i] = orderAnswer(fmt.Sprintf("etfp1795579990000-%04dBE", first+i), "SOXLUSDT", "buy", "NO", "filled", "42.95", "1", "1", "42.93", "42.93", "0.025758", "")
		}
		return fmt.Sprintf(`{"list":[%s],"cursor":%q}`, strings.Join(rows, ","), cursor)
	}
	client, server := newAccountServer(t, func(c restCall) (int, string) {
		values, _ := url.ParseQuery(c.query)
		switch {
		case values.Get("symbol") == "SOXSUSDT":
			return ok(`{"list":null,"cursor":null}`)
		case values.Get("cursor") == "":
			return ok(page(0, 100, "1379000000000000100"))
		default:
			return ok(page(100, 37, "1379000000000000137"))
		}
	})
	ctx := context.Background()
	since := fixedNow.Add(-time.Hour)
	got, err := client.RecentOrders(ctx, "SOXLUSDT", since)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 137 || got[0].Request.ClientOrderID != "etfp1795579990000-0000BE" || got[136].Request.ClientOrderID != "etfp1795579990000-0136BE" ||
		got[136].Status != cex.StatusFilled || !got[136].Fee.Equal(d("0.025758")) {
		t.Errorf("%d orders, want the 137 of both pages in the exchange's order", len(got))
	}
	calls := server.seen()
	window := "category=USDT-FUTURES&endTime=1795580000000&limit=100&startTime=1795576400000&symbol=SOXLUSDT"
	want := []string{"GET /api/v3/trade/history-orders?" + window,
		"GET /api/v3/trade/history-orders?" + strings.Replace(window, "&endTime", "&cursor=1379000000000000100&endTime", 1)}
	if len(calls) != 2 || calls[0].target() != want[0] || calls[1].target() != want[1] {
		t.Fatalf("requests = %v, want %v", targets(calls), want)
	}
	assertSigned(t, calls[1])

	// A contract without a finished order in the window: an empty list.
	if got, err := client.RecentOrders(ctx, "SOXSUSDT", since); err != nil || len(got) != 0 {
		t.Errorf("empty history: %v, %v", got, err)
	}
}

func targets(calls []restCall) []string {
	out := make([]string, len(calls))
	for i, c := range calls {
		out[i] = c.target()
	}
	return out
}

// --- fills ---

const fillsAnswer = `{"list":[` +
	`{"execId":"1380000000000000301","orderId":"1380000000000000017","clientOid":"etfp1795579990000-0001BE","category":"USDT-FUTURES","symbol":"SOXLUSDT","orderType":"limit","side":"buy","tradeSide":"open","posSide":"long","execPrice":"42.93","execQty":"7.5","execValue":"321.975","tradeScope":"taker","feeDetail":[{"feeCoin":"USDT","fee":"0.193185"}],"createdTime":"1795579990150","updatedTime":"1795579990151","execPnl":"0","execLinkId":"","isRPI":"no","execVenue":""},` +
	`{"execId":"1380000000000000302","orderId":"1380000000000000017","clientOid":"etfp1795579990000-0001BE","category":"USDT-FUTURES","symbol":"SOXLUSDT","orderType":"limit","side":"buy","tradeSide":"open","posSide":"long","execPrice":"42.93","execQty":"4.18","execValue":"179.4474","tradeScope":"taker","feeDetail":[{"feeCoin":"USDT","fee":"0.10766844"}],"createdTime":"1795579990160","updatedTime":"1795579990161","execPnl":"0","execLinkId":"","isRPI":"no","execVenue":""},` +
	`{"execId":"1380000000000000299","orderId":"1380000000000000011","clientOid":"web-7f3a19c2","category":"USDT-FUTURES","symbol":"BTCUSDT","orderType":"market","side":"sell","tradeSide":"close","posSide":"long","execPrice":"91234.5","execQty":"0.002","execValue":"182.469","tradeScope":"taker","feeDetail":[{"feeCoin":"USDT","fee":"0.1094814"}],"createdTime":"1795579000000","updatedTime":"1795579000001","execPnl":"1.25","execLinkId":"","isRPI":"no","execVenue":""}],"cursor":"1380000000000000299"}`

// The fills of the window are read without a symbol: the endpoint has no
// such filter, so the executions of every contract come back, each with its
// client order id for the caller to pick by.
func TestFills(t *testing.T) {
	client, server := newAccountServer(t, func(restCall) (int, string) { return ok(fillsAnswer) })
	got, err := client.Fills(context.Background(), fixedNow.Add(-30*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("%d fills, want all three of the account", len(got))
	}
	f := got[1]
	if f.ExecID != "1380000000000000302" || f.OrderID != "1380000000000000017" || f.ClientOrderID != ourOrder || f.Symbol != "SOXLUSDT" || f.Side != cex.SideBuy ||
		!f.Price.Equal(d("42.93")) || !f.Qty.Equal(d("4.18")) || !f.Value.Equal(d("179.4474")) || !f.Fee.Equal(d("0.10766844")) ||
		!f.ExecTime.Equal(time.UnixMilli(1795579990160)) || !f.ReceivedAt.Equal(fixedNow) {
		t.Errorf("fill = %+v", f)
	}
	if got[2].Symbol != "BTCUSDT" || got[2].ClientOrderID != otherOrder {
		t.Errorf("the other contract's fill = %+v, want it returned as it is", got[2])
	}
	calls := server.seen()
	if want := "GET /api/v3/trade/fills?category=USDT-FUTURES&endTime=1795580000000&limit=100&startTime=1795579970000"; len(calls) != 1 || calls[0].target() != want {
		t.Fatalf("requests = %v, want one: %s", targets(calls), want)
	}
	assertSigned(t, calls[0])
}

// --- financial records ---

// The funding records are read once per funding type, the type filtered by
// the exchange; an amount is signed, positive when received.
func TestFinancialRecords(t *testing.T) {
	row := func(id, symbol, recordType, amount, ts string) string {
		return fmt.Sprintf(`{"category":"USDT-FUTURES","id":%q,"symbol":%q,"coin":"USDT","type":%q,"positionType":"crossed","amount":%q,"fee":"0","positionAmount":"0","positionBalance":"0","balance":"1000.5","ts":%q}`, id, symbol, recordType, amount, ts)
	}
	client, server := newAccountServer(t, func(c restCall) (int, string) {
		values, _ := url.ParseQuery(c.query)
		switch values.Get("type") {
		case "CONTRACT_MAIN_SETTLE_FEE_USER_IN":
			return ok(`{"list":[` + row("1380000000000000901", "SOXSUSDT", "CONTRACT_MAIN_SETTLE_FEE_USER_IN", "0.03391", "1795564802106") + `],"cursor":"1380000000000000901"}`)
		case "CONTRACT_MAIN_SETTLE_FEE_USER_OUT":
			return ok(`{"list":[` + row("1380000000000000902", "SOXLUSDT", "CONTRACT_MAIN_SETTLE_FEE_USER_OUT", "-0.05124", "1795564804873") + `,` +
				row("1380000000000000850", "ETHUSDT", "CONTRACT_MAIN_SETTLE_FEE_USER_OUT", "-0.2", "1795536002500") + `],"cursor":"1380000000000000850"}`)
		}
		return refusal("400172", "Parameter verification failed")
	})
	got, err := client.FinancialRecords(context.Background(), fixedNow.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("%d records %+v, want the three of both types", len(got), got)
	}
	if r := got[0]; r.ID != "1380000000000000901" || r.Symbol != "SOXSUSDT" || r.Type != "CONTRACT_MAIN_SETTLE_FEE_USER_IN" || !r.Amount.Equal(d("0.03391")) || !r.At.Equal(time.UnixMilli(1795564802106)) {
		t.Errorf("received funding = %+v", r)
	}
	if r := got[1]; r.Symbol != "SOXLUSDT" || !r.Amount.Equal(d("-0.05124")) || !r.At.Equal(time.UnixMilli(1795564804873)) {
		t.Errorf("paid funding = %+v, want a negative amount", r)
	}
	calls := server.seen()
	window := "category=USDT-FUTURES&endTime=1795580000000&limit=100&startTime=1795493600000&type="
	want := []string{"GET /api/v3/account/financial-records?" + window + "CONTRACT_MAIN_SETTLE_FEE_USER_IN", "GET /api/v3/account/financial-records?" + window + "CONTRACT_MAIN_SETTLE_FEE_USER_OUT"}
	if len(calls) != 2 || calls[0].target() != want[0] || calls[1].target() != want[1] {
		t.Fatalf("requests = %v, want %v", targets(calls), want)
	}
	assertSigned(t, calls[0])
}

// --- place order ---

var entryOrder = cex.OrderRequest{ClientOrderID: ourOrder, Symbol: "SOXLUSDT", Leg: cex.Bull, Side: cex.SideBuy, Price: d("42.81"), Qty: d("11.68"),
	QuotaPrice: d("42.8"), Purpose: cex.PurposeEntry, CycleID: "c1795579990000"}

// The order goes out as one signed POST whose body holds exactly the ten
// fields of an IOC limit order in one-way mode, all as text and without a
// position side; an acceptance returns the exchange's order id.
func TestPlaceOrderRequest(t *testing.T) {
	client, server := newAccountServer(t, func(restCall) (int, string) {
		return ok(`{"clientOid":"` + ourOrder + `","orderId":"1380000000000000017"}`)
	})
	ctx := context.Background()
	ack, err := client.PlaceOrder(ctx, entryOrder)
	if err != nil || ack.OrderID != "1380000000000000017" || !ack.ReceivedAt.Equal(fixedNow) {
		t.Fatalf("ack = %+v, %v", ack, err)
	}
	// The response's header says when the exchange took the request; REST
	// does not say when the order was created.
	if !ack.AcceptedAt.Equal(time.UnixMilli(1795580000009)) || !ack.CreatedAt.IsZero() {
		t.Errorf("ack times: accepted %s, created %s; want the header's 1795580000009 and none", ack.AcceptedAt, ack.CreatedAt)
	}
	exit := entryOrder
	exit.ClientOrderID, exit.Symbol, exit.Side, exit.Price, exit.Qty, exit.ReduceOnly = "etfp1795579990000-0009RX", "SOXSUSDT", cex.SideSell, d("49.406"), d("10.10"), true
	if _, err := client.PlaceOrder(ctx, exit); err != nil {
		t.Fatal(err)
	}

	calls := server.seen()
	if len(calls) != 2 {
		t.Fatalf("%d requests for two orders", len(calls))
	}
	wants := []map[string]string{
		{"category": "USDT-FUTURES", "symbol": "SOXLUSDT", "side": "buy", "orderType": "limit", "timeInForce": "ioc", "price": "42.81", "qty": "11.68",
			"reduceOnly": "no", "marginMode": "crossed", "clientOid": ourOrder},
		{"category": "USDT-FUTURES", "symbol": "SOXSUSDT", "side": "sell", "orderType": "limit", "timeInForce": "ioc", "price": "49.406", "qty": "10.1",
			"reduceOnly": "yes", "marginMode": "crossed", "clientOid": "etfp1795579990000-0009RX"},
	}
	for i, call := range calls {
		if call.target() != "POST /api/v3/trade/place-order?" {
			t.Errorf("order %d went to %s", i, call.target())
		}
		assertSigned(t, call)
		var body map[string]string // every value must be text
		if err := json.Unmarshal([]byte(call.body), &body); err != nil {
			t.Fatalf("order %d: body %s: %v", i, call.body, err)
		}
		for field, want := range wants[i] {
			if body[field] != want {
				t.Errorf("order %d: %s = %q, want %q", i, field, body[field], want)
			}
		}
		if len(body) != len(wants[i]) {
			t.Errorf("order %d: body %s has %d fields, want exactly %d", i, call.body, len(body), len(wants[i]))
		}
	}
	if got := calls[0].header.Get("ACCESS-SIGN"); got != "3oRol9hC47QGa/sDO1BpOkRuoCA36V1BKjNyFaBvRSE=" {
		t.Errorf("ACCESS-SIGN = %s, want the vector of the request with its body", got)
	}
}

// What PlaceOrder answers says what is known of the order: a refusal with
// its code and kind, "may have arrived" for every answer that does not
// settle it, "not sent" only when nothing left. In every case the request
// is sent at most once.
func TestPlaceOrderOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		answer   func() (int, string)
		wantKind cex.ErrorKind // of a refusal
		wantCode string
		unknown  bool
	}{
		{name: "insufficient margin", answer: func() (int, string) { return refusal("25203", "Insufficient margin") }, wantKind: cex.ErrorOther, wantCode: "25203"},
		{name: "duplicate client id", answer: func() (int, string) { return refusal("25212", "Duplicate clientOid") }, wantKind: cex.ErrorOther, wantCode: "25212"},
		{name: "a refusal answered with http 200", answer: func() (int, string) {
			return http.StatusOK, `{"code":"45110","msg":"Less than the minimum order amount","requestTime":1,"data":null}`
		}, wantKind: cex.ErrorOther, wantCode: "45110"},
		{name: "price above the band", answer: func() (int, string) { return refusal("41117", "The price is too high") }, wantKind: cex.ErrorPriceLimit, wantCode: "41117"},
		{name: "price below the band", answer: func() (int, string) { return refusal("25206", "The price is too low") }, wantKind: cex.ErrorPriceLimit, wantCode: "25206"},
		{name: "close above the position", answer: func() (int, string) {
			return refusal("25231", "The number of closed positions cannot exceed the number of positions held")
		}, wantKind: cex.ErrorReduceQty, wantCode: "25231"},
		{name: "too frequent", answer: func() (int, string) { return refusal("25004", "Operation too frequent, please try again later") }, wantKind: cex.ErrorRateLimited, wantCode: "25004"},
		{name: "http 429 without an envelope", answer: func() (int, string) { return http.StatusTooManyRequests, "Too Many Requests" }, wantKind: cex.ErrorRateLimited, wantCode: "429"},
		{name: "request timed out at the exchange", answer: func() (int, string) { return refusal("40010", "Request timed out") }, unknown: true},
		{name: "server timeout", answer: func() (int, string) { return refusal("40725", "service return an error") }, unknown: true},
		{name: "exchange does not know", answer: func() (int, string) { return refusal("45001", "Unknown error") }, unknown: true},
		{name: "concurrent operation", answer: func() (int, string) { return refusal("25003", "Concurrent operation, please retry") }, unknown: true},
		{name: "http 502 without an envelope", answer: func() (int, string) { return http.StatusBadGateway, "<html>bad gateway</html>" }, unknown: true},
		{name: "a server error with a refusal's envelope", answer: func() (int, string) {
			return http.StatusServiceUnavailable, `{"code":"25203","msg":"Insufficient margin","requestTime":1,"data":null}`
		}, unknown: true},
		{name: "http 200 with an empty body", answer: func() (int, string) { return http.StatusOK, "" }, unknown: true},
		{name: "an acceptance that cannot be read", answer: func() (int, string) { return http.StatusOK, `{"code":"00000","msg":"success","data":"?"}` }, unknown: true},
	} {
		client, server := newAccountServer(t, func(restCall) (int, string) { return tc.answer() })
		_, err := client.PlaceOrder(context.Background(), entryOrder)
		var venueErr *cex.VenueError
		switch {
		case err == nil || errors.Is(err, cex.ErrNotSent):
			t.Errorf("%s: %v, want an error other than not-sent", tc.name, err)
		case tc.unknown:
			if !errors.Is(err, cex.ErrOrderStateUnknown) || errors.As(err, &venueErr) {
				t.Errorf("%s: %v, want cex.ErrOrderStateUnknown and no refusal", tc.name, err)
			}
		default:
			if errors.Is(err, cex.ErrOrderStateUnknown) || !errors.As(err, &venueErr) || venueErr.Code != tc.wantCode || venueErr.Kind != tc.wantKind {
				t.Errorf("%s: %v, want a refusal with code %s and kind %v", tc.name, err, tc.wantCode, tc.wantKind)
			}
		}
		if n := len(server.seen()); n != 1 {
			t.Errorf("%s: the order was sent %d times", tc.name, n)
		}
	}
}

// A request the exchange does not answer in time, or whose connection is
// lost after it was written, may have been carried out: the outcome is
// unknown, and the request is not sent again.
func TestPlaceOrderTimeoutAndLostConnectionAreUnknown(t *testing.T) {
	release := make(chan struct{})
	client, server := newAccountServer(t, func(restCall) (int, string) {
		<-release
		return ok(`{"clientOid":"` + ourOrder + `","orderId":"1380000000000000017"}`)
	})
	defer close(release)
	client.http = &http.Client{Timeout: 50 * time.Millisecond}
	if _, err := client.PlaceOrder(context.Background(), entryOrder); !errors.Is(err, cex.ErrOrderStateUnknown) || errors.Is(err, cex.ErrNotSent) {
		t.Errorf("timeout: %v, want cex.ErrOrderStateUnknown", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	client.http = &http.Client{Timeout: time.Second}
	if _, err := client.PlaceOrder(ctx, entryOrder); !errors.Is(err, cex.ErrOrderStateUnknown) {
		t.Errorf("caller's deadline: %v, want cex.ErrOrderStateUnknown", err)
	}
	if n := len(server.seen()); n != 2 {
		t.Errorf("the two orders were sent %d times", n)
	}

	// The server reads the request and drops the connection without an
	// answer: first by closing it, then by resetting it.
	var received int
	var mu sync.Mutex
	dropping := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		mu.Lock()
		received++
		reset := received == 2
		mu.Unlock()
		conn, _, _ := w.(http.Hijacker).Hijack()
		if reset {
			conn.(*net.TCPConn).SetLinger(0) // closing now sends a reset
		}
		conn.Close()
	}))
	defer dropping.Close()
	lost := testClient(dropping.URL)
	lost.creds = fakeCreds
	for _, how := range []string{"closed", "reset"} {
		if _, err := lost.PlaceOrder(context.Background(), entryOrder); !errors.Is(err, cex.ErrOrderStateUnknown) || errors.Is(err, cex.ErrNotSent) {
			t.Errorf("connection %s after the request: %v, want cex.ErrOrderStateUnknown", how, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if received != 2 {
		t.Errorf("the two orders reached the server %d times", received)
	}
}

// An order that provably never left is "not sent": the connection could
// not be opened, or the client has no API key.
func TestPlaceOrderNotSent(t *testing.T) {
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close() // nothing listens on its address any more
	client := testClient(gone.URL)
	client.creds = fakeCreds
	if _, err := client.PlaceOrder(context.Background(), entryOrder); !errors.Is(err, cex.ErrNotSent) || errors.Is(err, cex.ErrOrderStateUnknown) {
		t.Errorf("refused connection: %v, want cex.ErrNotSent", err)
	}

	keyless, server := newAccountServer(t, func(restCall) (int, string) { return ok(`{}`) })
	keyless.creds = cex.Credentials{}
	if _, err := keyless.PlaceOrder(context.Background(), entryOrder); !errors.Is(err, cex.ErrNotSent) {
		t.Errorf("no API key: %v, want cex.ErrNotSent", err)
	}
	if n := len(server.seen()); n != 0 {
		t.Errorf("a client without a key sent %d requests", n)
	}
}

// A client without an API key - the paper mode's - sends no account
// request at all: every account-side read fails before the network, and the
// private stream is not opened.
func TestAccountSideNeedsCredentials(t *testing.T) {
	client, server := newAccountServer(t, func(restCall) (int, string) { return ok(`{}`) })
	client.creds = cex.Credentials{}
	ctx := context.Background()
	symbols := []cex.Symbol{"SOXLUSDT"}
	_, errSettings := client.AccountSettings(ctx, symbols)
	_, errPositions := client.Positions(ctx, symbols)
	_, errOrder := client.OrderByClientID(ctx, "SOXLUSDT", ourOrder)
	_, errRecent := client.RecentOrders(ctx, "SOXLUSDT", fixedNow)
	_, errFills := client.Fills(ctx, fixedNow)
	_, errRecords := client.FinancialRecords(ctx, fixedNow)
	errPrivate := client.SubscribePrivate(ctx, symbols, func(cex.PrivateEvent) {}, func(error) {})
	for name, err := range map[string]error{"AccountSettings": errSettings, "Positions": errPositions, "OrderByClientID": errOrder, "RecentOrders": errRecent,
		"Fills": errFills, "FinancialRecords": errRecords, "SubscribePrivate": errPrivate} {
		if !errors.Is(err, errNoCredentials) {
			t.Errorf("%s = %v, want the no-API-key error", name, err)
		}
	}
	if n := len(server.seen()); n != 0 {
		t.Errorf("the account-side methods sent %d requests", n)
	}
	if client.PrivateUp() {
		t.Error("the private stream reads as up without a connection")
	}
	if client.Name() != "bitget" {
		t.Errorf("Name = %q", client.Name())
	}
}

// A pushed order carries its fee with ten decimals; order-info and the
// fills give it cut to eight, not rounded. The record keeps the venue's
// eight: 0.0003574896 is 0.00035748 (step 9 check, SOXS entry of
// 2026-11-25), and 0.000357344 stays 0.00035734.
func TestOrderFeeIsCutToTheVenuesEightDecimals(t *testing.T) {
	for raw, want := range map[string]string{"0.0003574896": "0.00035748", "0.000357344": "0.00035734"} {
		rec, err := orderRecord(orderRow{ClientOid: "etfp1795598077400-0001BE", OrderStatus: "filled", CumExecQty: "0.08",
			FeeDetail: []feeDetailRow{{FeeCoin: "USDT", Fee: raw}}})
		if err != nil || !rec.Fee.Equal(d(want)) {
			t.Errorf("fee %s: recorded %s, %v; want %s", raw, rec.Fee, err, want)
		}
	}
}
