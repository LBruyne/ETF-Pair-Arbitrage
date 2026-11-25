package store_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/inventory"
	"etf-pair-arbitrage/core/store"
	"etf-pair-arbitrage/core/strategy"
)

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }

// restartPair is the pair the restart test trades.
var restartPair = cex.Pair{Bull: cex.LegConfig{Symbol: "SOXLUSDT", Ticker: "SOXL", BookTopic: cex.TopicBooks5}, Bear: cex.LegConfig{Symbol: "SOXSUSDT", Ticker: "SOXS", BookTopic: cex.TopicBooks50}}

// restartVenue is the exchange of the restart test: it accepts every order,
// reports outcomes when the test pushes them through the private stream, and
// lists the orders it holds as its recent orders. It is shared by the
// "processes" of the test, as a real exchange outlives a process.
type restartVenue struct {
	mu      sync.Mutex
	orders  map[cex.Symbol][]cex.OrderRecord // what the venue holds, as RecentOrders returns it
	private func(cex.PrivateEvent)           // the running process's subscriber
	placed  int
}

var errNoAccount = errors.New("restart test venue: not part of this test")

func (v *restartVenue) Name() string { return "restart-test" }
func (v *restartVenue) MarketRules(context.Context, []cex.Symbol) (map[cex.Symbol]cex.MarketRule, error) {
	return nil, errNoAccount
}
func (v *restartVenue) AccountSettings(context.Context, []cex.Symbol) (cex.AccountSettings, error) {
	return cex.AccountSettings{}, errNoAccount
}
func (v *restartVenue) Positions(context.Context, []cex.Symbol) (cex.PositionSet, error) {
	return cex.PositionSet{}, errNoAccount
}
func (v *restartVenue) OrderByClientID(context.Context, cex.Symbol, cex.ClientOrderID) (cex.OrderRecord, error) {
	return cex.OrderRecord{}, errNoAccount
}
func (v *restartVenue) Fills(context.Context, time.Time) ([]cex.Fill, error) {
	return nil, errNoAccount
}
func (v *restartVenue) FinancialRecords(context.Context, time.Time) ([]cex.FinancialRecord, error) {
	return nil, errNoAccount
}
func (v *restartVenue) FundingHistory(context.Context, cex.Symbol, int) ([]cex.FundingSettlement, error) {
	return nil, errNoAccount
}
func (v *restartVenue) SubscribeBooks(context.Context, []cex.BookSubscription, func(cex.BookEvent), func(error)) error {
	return nil
}

func (v *restartVenue) RecentOrders(_ context.Context, symbol cex.Symbol, _ time.Time) ([]cex.OrderRecord, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]cex.OrderRecord(nil), v.orders[symbol]...), nil
}

func (v *restartVenue) SubscribePrivate(_ context.Context, _ []cex.Symbol, onEvent func(cex.PrivateEvent), _ func(error)) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.private = onEvent
	return nil
}

func (v *restartVenue) PlaceOrder(_ context.Context, req cex.OrderRequest) (cex.OrderAck, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.placed++
	return cex.OrderAck{OrderID: cex.OrderID("v-" + string(req.ClientOrderID))}, nil
}

// fill makes the venue hold the order as executed for cumQty at cumValue
// with the given fee - filled when that is its whole quantity, else the
// remainder cancelled - and, when push is set, reports it to the running
// process through the private stream. The venue's own record carries only
// what an exchange knows: no leg, purpose, cycle or lot.
func (v *restartVenue) fill(req cex.OrderRequest, cumQty, cumValue, fee string, push bool) {
	rec := cex.OrderRecord{Request: cex.OrderRequest{ClientOrderID: req.ClientOrderID, Symbol: req.Symbol, Side: req.Side, Price: req.Price, Qty: req.Qty, ReduceOnly: req.ReduceOnly},
		OrderID: cex.OrderID("v-" + string(req.ClientOrderID)), Status: cex.StatusFilled, CumExecQty: d(cumQty), CumExecValue: d(cumValue), AvgPrice: d(cumValue).Div(d(cumQty)),
		Fee: d(fee), FeeKnown: true, SubmittedAt: time.Now(), ExchangeUpdatedAt: time.Now()}
	if rec.CumExecQty.LessThan(req.Qty) {
		rec.Status = cex.StatusCancelled
	}
	v.mu.Lock()
	v.orders[req.Symbol] = append(v.orders[req.Symbol], rec)
	private := v.private
	v.mu.Unlock()
	if push {
		private(cex.PrivateEvent{Order: &rec})
	}
}

// noSource stands in for the read-only sources: this test reads none.
type noSource struct{}

func (noSource) IndexClose(context.Context, cex.Symbol, time.Time) (decimal.Decimal, bool, error) {
	return decimal.Decimal{}, false, errNoAccount
}
func (noSource) TradingSchedule(context.Context) ([]cex.ScheduleDay, error) { return nil, errNoAccount }
func (noSource) Quote(context.Context) (decimal.Decimal, decimal.Decimal, error) {
	return decimal.Decimal{}, decimal.Decimal{}, errNoAccount
}
func (noSource) MinuteClose(context.Context, time.Time) (decimal.Decimal, bool, error) {
	return decimal.Decimal{}, false, errNoAccount
}
func (noSource) MinuteCloses(context.Context, time.Time, time.Time) ([]cex.FXMinute, error) {
	return nil, errNoAccount
}
func (noSource) NAV(context.Context, string, time.Time) (decimal.Decimal, bool, error) {
	return decimal.Decimal{}, false, cex.ErrIssuerKey
}
func (noSource) Distributions(context.Context, string) ([]cex.Distribution, error) {
	return nil, cex.ErrIssuerKey
}

// process is one run of the trading process as far as this test needs it:
// a store on the database, cex's manager with its order table, and the
// ledger, started the way main starts them.
type process struct {
	t      *testing.T
	st     *store.Store
	cex    *cex.CexManager
	inv    *inventory.InventoryManager
	state  store.State
	stop   func()
	venue  *restartVenue
	cycle  *inventory.Cycle
	nextID int
}

// startProcess opens a store, reads the stored state, restores cex and the
// ledger from it as main does, and runs cex so that the private stream is
// subscribed.
func startProcess(t *testing.T, pc *pgxpool.Config, venue *restartVenue) *process {
	t.Helper()
	st, err := store.New(store.Config{}, store.RunID(fmt.Sprintf("test-%d-restart", time.Now().UnixNano())), pc.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	go st.Run(context.Background())
	cfg := cex.DefaultConfig
	cfg.Pair = restartPair
	cm, err := cex.NewManager(cfg, venue, noSource{}, noSource{}, noSource{})
	if err != nil {
		t.Fatal(err)
	}
	im, err := inventory.NewManager(inventory.Config{Capital: d("1000"), Leverage: d("10")}, restartPair, cm)
	if err != nil {
		t.Fatal(err)
	}
	cm.UseStore(st)
	im.UseStore(st)

	// As main does: load, the orders into cex's table, the ledger from the
	// table's snapshot.
	state, err := st.LoadState(context.Background(), "2026-11-24")
	if err != nil {
		t.Fatal(err)
	}
	orders := cex.OrderRecordsFromRows(state.Orders)
	cm.Restore(cex.AnchorFactsFromRows(state.AnchorFacts), orders)
	if state.Cycle != nil {
		orders = cm.CycleOrders(state.Cycle.CycleID)
	}
	if err := im.Restore(state, orders); err != nil {
		t.Fatal(err)
	}

	venue.mu.Lock()
	venue.private = nil
	venue.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { cm.Run(ctx); close(done) }()
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(time.Millisecond) {
		venue.mu.Lock()
		subscribed := venue.private != nil
		venue.mu.Unlock()
		if subscribed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the private stream was never subscribed")
		}
	}
	p := &process{t: t, st: st, cex: cm, inv: im, state: state, venue: venue, cycle: im.Cycle(), nextID: len(orders) + 1}
	stopped := false
	p.stop = func() { // the process ends; everything recorded is committed
		if !stopped {
			stopped = true
			cancel()
			<-done
			st.Close()
		}
	}
	t.Cleanup(p.stop)
	return p
}

// sell builds the next entry order of the cycle: a sell of the leg on the
// given lot, with the quota price a sell carries (the decision's raw best
// bid, above its limit).
func (p *process) sell(leg cex.Leg, lot inventory.LotID, price, quotaPrice, qty string) cex.OrderRequest {
	symbol := restartPair.Bull.Symbol
	if leg == cex.Bear {
		symbol = restartPair.Bear.Symbol
	}
	req := cex.OrderRequest{ClientOrderID: cex.NewClientOrderID(p.cycle.OpenedAt, p.nextID, leg, cex.PurposeEntry), Symbol: symbol, Leg: leg, Side: cex.SideSell,
		Price: d(price), Qty: d(qty), QuotaPrice: d(quotaPrice), Purpose: cex.PurposeEntry, CycleID: string(p.cycle.ID), LotID: string(lot)}
	p.nextID++
	return req
}

// placeAndFill places the orders through cex's order table, lets the venue
// report each outcome, and books the table's snapshot in the ledger once
// every order has its final state.
func (p *process) placeAndFill(outcomes ...func()) {
	p.t.Helper()
	for _, report := range outcomes {
		report()
	}
	for deadline := time.Now().Add(3 * time.Second); len(p.cex.UnresolvedOrders()) > 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			p.t.Fatalf("orders still without an outcome: %+v", p.cex.UnresolvedOrders())
		}
	}
	p.inv.Sync(p.cex.CycleOrders(string(p.cycle.ID)))
}

// wantLot fails unless lot number seq holds the given entry quantities,
// values and fees on its two legs.
func (p *process) wantLot(stage string, seq int, bullQty, bullValue, bullFee, bearQty, bearValue, bearFee string) {
	p.t.Helper()
	c := p.inv.Cycle()
	if c == nil || len(c.Lots) < seq {
		p.t.Fatalf("%s: the cycle has no lot %d", stage, seq)
	}
	lot := c.Lots[seq-1]
	for _, w := range []struct {
		leg             cex.Leg
		qty, value, fee string
	}{{cex.Bull, bullQty, bullValue, bullFee}, {cex.Bear, bearQty, bearValue, bearFee}} {
		got := lot.Legs[w.leg]
		if !got.EntryQty.Equal(d(w.qty)) || !got.EntryValue.Equal(d(w.value)) || !got.EntryFee.Equal(d(w.fee)) || !got.ExitQty.IsZero() {
			p.t.Errorf("%s: lot %d %s leg holds %s for %s (fee %s, exited %s), want %s for %s (fee %s)", stage, seq, w.leg, got.EntryQty, got.EntryValue, got.EntryFee, got.ExitQty, w.qty, w.value, w.fee)
		}
	}
}

// wantQuotaPrices fails unless the two sell orders of batch A carry, in the
// rows read back and in cex's restored records alike, the quota prices they
// were sent with.
func (p *process) wantQuotaPrices(stage string, bull, bear cex.ClientOrderID) {
	p.t.Helper()
	want := map[cex.ClientOrderID]string{bull: "10.02", bear: "20.03"}
	seen := 0
	for _, row := range p.state.Orders {
		if w, ok := want[cex.ClientOrderID(row.ClientOrderID)]; ok {
			seen++
			if !row.QuotaPrice.Equal(d(w)) || row.Side != "sell" {
				p.t.Errorf("%s: the stored %s order %s has quota price %s, want %s", stage, row.Side, row.ClientOrderID, row.QuotaPrice, w)
			}
		}
	}
	for _, rec := range p.cex.CycleOrders(string(p.cycle.ID)) {
		if w, ok := want[rec.Request.ClientOrderID]; ok {
			seen++
			if !rec.Request.QuotaPrice.Equal(d(w)) {
				p.t.Errorf("%s: the restored order %s has quota price %s, want %s", stage, rec.Request.ClientOrderID, rec.Request.QuotaPrice, w)
			}
		}
	}
	if seen != 4 {
		p.t.Errorf("%s: %d of the two sell orders' rows and records were found, want 4", stage, seen)
	}
}

// The restart acceptance, through the database. A short cycle's batch A
// sold 10 bull and 4 of 5 bear; the process also sent a completion of 1
// bear share and ended before recording it. The next process finds the
// completion at the venue, the ledger books it on A, and that lot is stored
// in cex_order.lot_id. Batch B is then opened and filled. A third process,
// started from the database alone, holds the same quantities and costs on A
// and B, also after the snapshot is booked again, and recognises nothing
// anew; the sell orders' quota prices are read back as they were sent.
func TestRestartKeepsRecognisedOrdersOnTheirLot(t *testing.T) {
	pc := store.TestDatabase(t)
	check, err := pgxpool.NewWithConfig(context.Background(), pc)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	// Opening a store applies the schema, on a new database for the first
	// time; then the tables are emptied.
	schema, err := store.New(store.Config{}, "test-restart-schema", pc.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	schema.Close()
	store.ClearTables(t, check)
	venue := &restartVenue{orders: map[cex.Symbol][]cex.OrderRecord{}}
	anchors := cex.Anchors{SessionID: "2026-11-24", Kind: cex.AnchorBell, Bull: d("10"), Bear: d("20")}
	plan := &strategy.EntryPlan{Direction: strategy.Short, X: 10, Mu: 3.33, Sigma: 4, FeeD: 2.6, FeeUSDT: d("1.1")}

	// --- the first process: batch A, and a completion it does not record ---
	p1 := startProcess(t, pc, venue)
	if p1.cycle, err = p1.inv.OpenCycle("2026-11-24", strategy.Short, anchors, time.Now()); err != nil {
		t.Fatal(err)
	}
	lotA, err := p1.inv.OpenLot(plan, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	aBull, aBear := p1.sell(cex.Bull, lotA.ID, "10", "10.02", "10"), p1.sell(cex.Bear, lotA.ID, "20", "20.03", "5")
	if err := p1.cex.PlaceOrders(aBull, aBear); err != nil {
		t.Fatal(err)
	}
	p1.placeAndFill(func() { venue.fill(aBull, "10", "100", "0.01", true) }, func() { venue.fill(aBear, "4", "80", "0.008", true) })
	p1.wantLot("batch A sent", 1, "10", "100", "0.01", "4", "80", "0.008")
	// The completion reaches the venue and is filled; the process is gone
	// before it registers it.
	completion := cex.OrderRequest{ClientOrderID: cex.NewClientOrderID(p1.cycle.OpenedAt, 3, cex.Bear, cex.PurposeComplete), Symbol: restartPair.Bear.Symbol, Side: cex.SideSell, Price: d("20"), Qty: d("1")}
	venue.fill(completion, "1", "20", "0.002", false)
	p1.stop()

	// --- the second process: recognises the completion, stores its lot, opens B ---
	p2 := startProcess(t, pc, venue)
	if p2.cycle == nil || p2.cycle.ID != p1.cycle.ID || len(p2.state.Orders) != 2 {
		t.Fatalf("restored cycle %+v with %d orders, want the open cycle and its two orders", p2.cycle, len(p2.state.Orders))
	}
	p2.wantLot("restarted", 1, "10", "100", "0.01", "4", "80", "0.008")
	p2.wantQuotaPrices("restarted", aBull.ClientOrderID, aBear.ClientOrderID)

	// What the recovery does: read the venue's orders, book the table's
	// snapshot, store the ledger's assignment of what was recognised.
	recognised, err := p2.cex.RecoverOrders(context.Background(), string(p2.cycle.ID))
	if err != nil {
		t.Fatal(err)
	}
	if len(recognised) != 1 || recognised[0].Request.ClientOrderID != completion.ClientOrderID || recognised[0].Request.LotID != "" ||
		recognised[0].Request.Purpose != cex.PurposeComplete || recognised[0].Request.Leg != cex.Bear || recognised[0].Request.CycleID != string(p2.cycle.ID) {
		t.Fatalf("recognised = %+v, want the one completion, read from its id, without a lot", recognised)
	}
	p2.inv.Sync(p2.cex.CycleOrders(string(p2.cycle.ID)))
	for _, rec := range recognised {
		p2.cex.SetOrderLot(rec.Request.ClientOrderID, string(p2.inv.LotOf(rec.Request.ClientOrderID)))
	}
	if got := p2.inv.LotOf(completion.ClientOrderID); got != lotA.ID {
		t.Fatalf("the completion is booked on %q, want batch A %s", got, lotA.ID)
	}
	p2.wantLot("completion recognised", 1, "10", "100", "0.01", "5", "100", "0.01")

	// Batch A is built; batch B opens and fills.
	p2.inv.SettleLot(lotA.ID, true, time.Now())
	lotB, err := p2.inv.OpenLot(plan, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	p2.nextID = 4
	bBull, bBear := p2.sell(cex.Bull, lotB.ID, "10.1", "10.12", "10"), p2.sell(cex.Bear, lotB.ID, "20.1", "20.13", "5")
	if err := p2.cex.PlaceOrders(bBull, bBear); err != nil {
		t.Fatal(err)
	}
	p2.inv.Sync(p2.cex.CycleOrders(string(p2.cycle.ID))) // B's orders in flight: A must not move
	p2.wantLot("B in flight", 1, "10", "100", "0.01", "5", "100", "0.01")
	p2.wantLot("B in flight", 2, "0", "0", "0", "0", "0", "0")
	p2.placeAndFill(func() { venue.fill(bBull, "10", "101", "0.0101", true) }, func() { venue.fill(bBear, "5", "100.5", "0.01005", true) })
	p2.wantLot("B filled", 1, "10", "100", "0.01", "5", "100", "0.01")
	p2.wantLot("B filled", 2, "10", "101", "0.0101", "5", "100.5", "0.01005")
	p2.stop()

	// --- the third process: from the database alone ---
	p3 := startProcess(t, pc, venue)
	if len(p3.state.Orders) != 5 {
		t.Fatalf("%d orders read back, want the five of the cycle", len(p3.state.Orders))
	}
	for _, row := range p3.state.Orders {
		if row.ClientOrderID == string(completion.ClientOrderID) && (row.LotID != string(lotA.ID) || row.Purpose != "complete" || row.Leg != "bear" || !row.CumExecValue.Equal(d("20"))) {
			t.Errorf("the completion's stored row = %+v, want lot %s", row, lotA.ID)
		}
	}
	stages := []func(){
		func() {},
		func() { p3.inv.Sync(p3.cex.CycleOrders(string(p3.cycle.ID))) },
		func() {
			again, err := p3.cex.RecoverOrders(context.Background(), string(p3.cycle.ID))
			if err != nil || len(again) != 0 {
				t.Errorf("the third process recognised %+v, %v; every order is stored", again, err)
			}
			p3.inv.Sync(p3.cex.CycleOrders(string(p3.cycle.ID)))
		},
	}
	for i, replay := range stages {
		replay()
		stage := fmt.Sprintf("restarted from the database, stage %d", i+1)
		p3.wantLot(stage, 1, "10", "100", "0.01", "5", "100", "0.01")
		p3.wantLot(stage, 2, "10", "101", "0.0101", "5", "100.5", "0.01005")
		if got := p3.inv.LotOf(completion.ClientOrderID); got != lotA.ID {
			t.Errorf("%s: the completion is booked on %q, want %s", stage, got, lotA.ID)
		}
	}
	p3.wantQuotaPrices("restarted from the database", aBull.ClientOrderID, aBear.ClientOrderID)
	if dir, hold := p3.inv.Holdings(); dir != strategy.Short || !hold[cex.Bull].Equal(d("20")) || !hold[cex.Bear].Equal(d("10")) {
		t.Errorf("holdings = %s %s / %s, want short 20 / 10", dir, hold[cex.Bull], hold[cex.Bear])
	}
	if venue.placed != 4 {
		t.Errorf("the venue received %d orders, want the four the test placed: a restart sends none again", venue.placed)
	}

	// What main seeds a paper account with after a restart: the restored
	// ledger's positions, signed. The paper venue then reports exactly
	// them.
	paper := cex.NewPaperVenue(venue, cex.PaperConfig{Pair: restartPair, Leverage: 10, DepthMaxAge: cex.DefaultConfig.DepthMaxAge})
	paper.SeedPositions(p3.inv.LedgerPositions())
	seeded, err := paper.Positions(context.Background(), nil)
	if err != nil || !seeded.BySymbol["SOXLUSDT"].Qty.Equal(d("-20")) || !seeded.BySymbol["SOXSUSDT"].Qty.Equal(d("-10")) {
		t.Errorf("the paper account seeded from the restored ledger holds %+v, %v; want -20 / -10", seeded.BySymbol, err)
	}
}
