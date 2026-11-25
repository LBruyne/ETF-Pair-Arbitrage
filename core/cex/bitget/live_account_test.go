package bitget

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/cex/internal/livetest"
)

// getOnly is the transport of the live account test: it hands a GET to the
// next transport and refuses every other request before it reaches the
// network.
type getOnly struct{ next http.RoundTripper }

func (g getOnly) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet {
		return nil, fmt.Errorf("refused %s %s: this transport only reads", req.Method, req.URL.Path)
	}
	return g.next.RoundTrip(req)
}

// The read-only transport passes a GET and stops every other method before
// the server sees it.
func TestGetOnlyTransportRefusesWrites(t *testing.T) {
	var mu sync.Mutex
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods = append(methods, r.Method)
		mu.Unlock()
	}))
	defer server.Close()
	client := &http.Client{Transport: getOnly{next: http.DefaultTransport}}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch, http.MethodGet} {
		req, _ := http.NewRequest(method, server.URL+"/api/v3/trade/place-order", strings.NewReader("{}"))
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
		}
		if (err == nil) != (method == http.MethodGet) {
			t.Errorf("%s: %v", method, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(methods) != 1 || methods[0] != http.MethodGet {
		t.Errorf("the server saw %v, want the one GET", methods)
	}
}

// liveCredentials returns the exchange key for a gated live test, from the
// secrets file BITGET_ACCOUNT_ENV names: its `bitget` section, decoded into
// the program's own type. config.Load is not used because it validates the
// whole file and so needs a database DSN these tests have no use for. A
// failure is reported without anything of the file, and nothing of it is
// ever printed.
func liveCredentials(t *testing.T) cex.Credentials {
	t.Helper()
	envPath := os.Getenv("BITGET_ACCOUNT_ENV")
	if envPath == "" {
		t.Fatal("BITGET_ACCOUNT_ENV must name the secrets file")
	}
	text, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal("BITGET_ACCOUNT_ENV: the secrets file cannot be read")
	}
	var secrets struct {
		Bitget cex.Credentials `json:"bitget"`
	}
	if json.Unmarshal(text, &secrets) != nil || secrets.Bitget.Key == "" || secrets.Bitget.Secret == "" || secrets.Bitget.Passphrase == "" {
		t.Fatal("BITGET_ACCOUNT_ENV: the secrets file has no complete bitget section (key, secret, passphrase)")
	}
	return secrets.Bitget
}

// livePrivate is how long TestLiveBitgetAccountRead keeps the private
// stream.
const livePrivate = 15 * time.Second

// Against the real exchange with the account's API key, reading only; gated
// by BITGET_ACCOUNT_READ_TEST=1. BITGET_ACCOUNT_ENV names the secrets file;
// its bitget section is decoded and nothing of it is printed. The
// test reads the rules, the account settings, the positions, an order that
// does not exist, the finished orders, the fills and the funding records,
// then logs in on the private stream and holds its three topics for fifteen
// seconds. Its transport refuses every request that is not a GET, and the
// order command is not called.
//
// The REST responses (URL and body, no request header) and the frames the
// private stream delivered are saved as received under
// experiments/core_step5/. The log and the assertions speak of shapes and
// counts only: no balance, size or order detail.
func TestLiveBitgetAccountRead(t *testing.T) {
	if os.Getenv("BITGET_ACCOUNT_READ_TEST") != "1" {
		t.Skip("BITGET_ACCOUNT_READ_TEST is not 1: the read-only test of the Bitget account is skipped")
	}
	creds := liveCredentials(t)

	saved := livetest.Dir
	livetest.Dir = filepath.Join("..", "..", "..", "experiments", "core_step5")
	defer func() { livetest.Dir = saved }()
	client := NewClient(creds)
	recorder := &livetest.Recorder{}
	client.http.Transport = getOnly{next: recorder}
	defer func() {
		path, err := recorder.Save("bitget_account_rest_raw")
		if err != nil {
			t.Error(err)
		}
		responses := recorder.Responses()
		for _, r := range responses {
			if r.Method != http.MethodGet {
				t.Errorf("a %s request reached the network", r.Method)
			}
		}
		t.Logf("saved %d raw REST responses (all GET) to %s", len(responses), path)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	symbols := []cex.Symbol{"SOXLUSDT", "SOXSUSDT"}
	ours := map[cex.Symbol]bool{symbols[0]: true, symbols[1]: true}
	now := time.Now()

	// Rules.
	rules, err := client.MarketRules(ctx, symbols)
	if err != nil || len(rules) != 2 || !rules[symbols[0]].Online || !rules[symbols[1]].Online {
		t.Errorf("rules: %d loaded, %v; want both contracts online", len(rules), err)
	}

	// Settings.
	settings, err := client.AccountSettings(ctx, symbols)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	crossed := 0
	for _, s := range settings.BySymbol {
		if s.Crossed && s.Leverage > 0 {
			crossed++
		}
	}
	t.Logf("settings: one-way %t; %d of 2 contracts have a setting, %d of them crossed with a leverage", settings.OneWay, len(settings.BySymbol), crossed)

	// Positions.
	positions, err := client.Positions(ctx, symbols)
	if err != nil || !positions.Loaded {
		t.Fatalf("positions: loaded %t, %v", positions.Loaded, err)
	}
	t.Logf("positions: read for 2 contracts, %d of them not flat", len(positions.BySymbol))

	// An order that does not exist.
	_, err = client.OrderByClientID(ctx, symbols[0], "etfp1700000000000-ZZZZBE")
	if !errors.Is(err, cex.ErrUnknownOrder) {
		t.Errorf("an order that was never placed: %v, want cex.ErrUnknownOrder", err)
	} else {
		t.Log("order-info of an id never used: the unknown-order answer")
	}

	// Finished orders of the last seven days.
	for _, symbol := range symbols {
		orders, err := client.RecentOrders(ctx, symbol, now.Add(-7*24*time.Hour))
		if err != nil {
			t.Errorf("%s: recent orders: %v", symbol, err)
			continue
		}
		own, byStatus, wrongSymbol, feeKnown, withFill := 0, map[cex.OrderStatus]int{}, 0, 0, 0
		for _, o := range orders {
			byStatus[o.Status]++
			if cex.OwnOrder(o.Request.ClientOrderID) {
				own++
			}
			if o.Request.Symbol != symbol {
				wrongSymbol++
			}
			if o.CumExecQty.IsPositive() {
				withFill++
				if o.FeeKnown {
					feeKnown++
				}
			}
		}
		if wrongSymbol != 0 {
			t.Errorf("%s: %d orders of another contract in its history", symbol, wrongSymbol)
		}
		t.Logf("%s: %d finished orders in 7 days (%d with this program's prefix), by status %v; %d with a fill, %d of them with a known fee", symbol, len(orders), own, byStatus, withFill, feeKnown)
	}

	// Fills of the last seven days, of every contract.
	fills, err := client.Fills(ctx, now.Add(-7*24*time.Hour))
	if err != nil {
		t.Errorf("fills: %v", err)
	}
	pairFills, ownFills, timed := 0, 0, 0
	for _, f := range fills {
		if ours[f.Symbol] {
			pairFills++
		}
		if cex.OwnOrder(f.ClientOrderID) {
			ownFills++
		}
		if !f.ExecTime.IsZero() && f.ExecID != "" && f.Qty.IsPositive() {
			timed++
		}
	}
	if timed != len(fills) {
		t.Errorf("fills: %d of %d have an id, a time and a quantity", timed, len(fills))
	}
	t.Logf("fills: %d in 7 days over all contracts, %d of the pair, %d with this program's prefix", len(fills), pairFills, ownFills)

	// Funding records of the last 29 days.
	records, err := client.FinancialRecords(ctx, now.Add(-29*24*time.Hour))
	if err != nil {
		t.Errorf("financial records: %v", err)
	}
	byType, pairRecords := map[string]int{}, 0
	for _, r := range records {
		byType[r.Type]++
		if ours[r.Symbol] {
			pairRecords++
		}
		if r.ID == "" || r.At.IsZero() || (r.Type != fundingRecordTypes[0] && r.Type != fundingRecordTypes[1]) {
			t.Errorf("financial records: a row without an id or a time, or of a type that was not asked for (%s)", r.Type)
		}
	}
	t.Logf("financial records: %d funding rows in 29 days, by type %v, %d of the pair", len(records), byType, pairRecords)

	// The private stream: every frame it delivers is saved as it arrived.
	// The login frame this client sends is not written anywhere.
	if err := os.MkdirAll(livetest.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	framesPath := filepath.Join(livetest.Dir, "bitget_private_"+started.UTC().Format("20060102T150405Z")+".jsonl")
	file, err := os.Create(framesPath)
	if err != nil {
		t.Fatal(err)
	}
	header, _ := json.Marshal(map[string]any{"url": wsPrivateURL, "started_ms": started.UnixMilli(), "seconds": livePrivate.Seconds(),
		"topics": []string{topicOrder, topicFill, topicPosition}, "note": "frames received only, one per line after their arrival in unix ms; the login request is not recorded"})
	fmt.Fprintf(file, "%s\n", header)
	var mu sync.Mutex
	closed := false
	kinds, firstPosition, firstPositionRows := map[string]int{}, "", 0
	client.onRawPrivateFrame = func(raw []byte, at time.Time) {
		var f wsFrame
		json.Unmarshal(raw, &f)
		mu.Lock()
		defer mu.Unlock()
		if closed {
			return
		}
		fmt.Fprintf(file, "%d\t%s\n", at.UnixMilli(), raw)
		switch {
		case f.Action != "":
			kinds["push "+f.Arg.Topic]++
			if f.Arg.Topic == topicPosition && firstPosition == "" {
				var rows []json.RawMessage
				json.Unmarshal(f.Data, &rows)
				firstPosition, firstPositionRows = f.Action, len(rows)
			}
		case f.Event == "login":
			kinds["login code "+string(f.Code)]++
		default:
			kinds[f.Event+" "+f.Arg.Topic]++
		}
	}
	var events struct {
		orders, fills, positionSets int
		first                       *cex.PositionSet
	}
	downs := 0
	streamCtx, cancelStream := context.WithTimeout(context.Background(), livePrivate)
	defer cancelStream()
	err = client.SubscribePrivate(streamCtx, symbols,
		func(ev cex.PrivateEvent) {
			mu.Lock()
			defer mu.Unlock()
			switch {
			case ev.Order != nil:
				events.orders++
			case ev.Fill != nil:
				events.fills++
			case ev.Positions != nil:
				events.positionSets++
				if events.first == nil {
					events.first = ev.Positions
				}
			}
		},
		func(error) {
			mu.Lock()
			defer mu.Unlock()
			downs++
		})
	if err != nil {
		t.Fatalf("SubscribePrivate: %v", err)
	}
	upAfter := time.Duration(-1)
	for streamCtx.Err() == nil {
		if upAfter < 0 && client.PrivateUp() {
			upAfter = time.Since(started)
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond) // the connection closes
	mu.Lock()
	closed = true
	file.Close()
	upAtEnd := upAfter >= 0 && downs == 0
	mu.Unlock()

	t.Logf("private stream: up %s after the dial; frames %v; first position frame %q with %d rows; handed over %d position sets, %d orders, %d fills; %d losses",
		upAfter.Round(time.Millisecond), kinds, firstPosition, firstPositionRows, events.positionSets, events.orders, events.fills, downs)
	if !upAtEnd || kinds["login code 0"] != 1 || kinds["subscribe order"] != 1 || kinds["subscribe fill"] != 1 || kinds["subscribe position"] != 1 {
		t.Errorf("private stream: up %t, frames %v; want one accepted login and one acknowledgement per topic on a connection that held", upAtEnd, kinds)
	}
	if firstPosition != "snapshot" || events.first == nil || !events.first.Loaded {
		t.Errorf("private stream: first position frame %q, a position set handed over: %t; want a snapshot and its set", firstPosition, events.first != nil)
	} else {
		// The stream's first set against the REST read a few seconds earlier.
		differ := 0
		for _, symbol := range symbols {
			if !events.first.BySymbol[symbol].Qty.Equal(positions.BySymbol[symbol].Qty) {
				differ++
			}
		}
		if differ != 0 || len(events.first.BySymbol) != len(positions.BySymbol) {
			t.Errorf("private stream: its first position set differs from the REST read on %d of 2 contracts", differ)
		} else {
			t.Logf("private stream: its first position set equals the REST read on both contracts (%d not flat)", len(events.first.BySymbol))
		}
	}
	if client.PrivateUp() {
		t.Error("the stream still reads as up after it was closed")
	}
	t.Logf("saved the received frames to %s", framesPath)
}
