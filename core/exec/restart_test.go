package exec

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/internal/dbtest"
	"etf-pair-arbitrage/core/inventory"
	"etf-pair-arbitrage/core/store"
	"etf-pair-arbitrage/core/strategy"
)

// The restart tests run two processes one after the other on the
// development database and one exchange: the first trades and "crashes"
// (its loops end, its store has written what it had), the second reads the
// database back as main does and recovers. The exchange - the paper venue
// with its orders and positions - outlives the first process like a real
// one; its positions are not seeded from the ledger.

// process is one process of a restart test: the loop on its rig and its
// store.
type process struct {
	*rig
	st    *store.Store
	close func()
}

// dbProcess starts a process at now on its own store, restored from what
// the database holds; ex is the exchange of the process before it, nil for
// the first.
func dbProcess(t *testing.T, now time.Time, ex *exchange) *process {
	t.Helper()
	st, err := store.New(store.Config{}, store.RunID(fmt.Sprintf("test-%d", time.Now().UnixNano())), dbtest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go st.Run(ctx)
	var once sync.Once
	closeStore := func() { once.Do(func() { st.Close(); cancel() }) }
	t.Cleanup(closeStore)
	opts := rigOptions{exchange: ex, sink: st, perp: st}
	if ex != nil {
		calendar, _ := cex.NewCalendar(nil)
		state, err := st.LoadState(ctx, calendar.SessionAt(now).ID)
		if err != nil {
			t.Fatal(err)
		}
		opts.state = &state
	}
	return &process{rig: newRig(t, now, opts), st: st, close: closeStore}
}

// restartTest holds the database for the test and clears it.
func restartTest(t *testing.T) {
	t.Helper()
	dbtest.Lock(t)
	dbtest.Clear(t)
}

// down ends the process: its cex loops stop and its store writes what it
// queued, as a crash after the last write would leave it.
func (p *process) down() {
	p.crash()
	p.close()
}

// exec runs SQL on the test database.
func execSQL(t *testing.T, sql string, args ...any) {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), dbtest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

// querySQL reads one value from the test database.
func querySQL[T any](t *testing.T, sql string, args ...any) T {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), dbtest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	var v T
	if err := conn.QueryRow(context.Background(), sql, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return v
}

// holdingProcess is a first process that sold example 1's 105.30 / 70.20
// at 05:03:05 (fills 4,259.385 / 4,172.0292).
func holdingProcess(t *testing.T) *process {
	t.Helper()
	a := dbProcess(t, utc8Time(11, 25, 5, 1, 0), nil)
	a.warmUp()
	a.at(utc8Time(11, 25, 5, 3, 5))
	a.books(entryBook())
	a.trade(utc8Time(11, 25, 5, 3, 5))
	return a
}

// SPEC row 6.4: the old process holds a position; the new one cannot read
// the venue's positions at its recovery; a stop arrives; the reads come
// back. It reconciles, reduces both legs, settles the cycle and only then
// leaves - not before the positions were read, and without an entry.
func TestRecoveryReadsBeforeItStops(t *testing.T) {
	restartTest(t)
	a := holdingProcess(t)
	a.down()
	sent := a.gate.placedTotal()

	t1 := utc8Time(11, 25, 5, 3, 10)
	b := dbProcess(t, t1, a.exchange)
	b.at(t1)
	b.books(entryBook())
	b.gate.unread.Store(true)
	done := runLoop(b.rig, context.Background())
	time.Sleep(100 * time.Millisecond) // recovery retries every 20 ms here
	b.signals <- syscall.SIGTERM
	time.Sleep(200 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("left while the positions were unread: %v", err)
	default:
	}
	if n := b.gate.placedTotal() - sent; n != 0 {
		t.Fatalf("%d orders sent before the positions were read", n)
	}

	b.gate.unread.Store(false)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not leave after the reads came back")
	}
	if got := strings.Join(b.purposes(), " "); got != "forced forced" {
		t.Errorf("orders of the new process %q, want two forced orders", got)
	}
	if c := b.lastCycle(); c.Status != string(inventory.CycleClosed) || c.EndReason != inventory.EndStop {
		t.Errorf("cycle %s %s, want closed stop", c.Status, c.EndReason)
	}
}

// V15 and SPEC row 6.4, a crash after the send: SOXL's result was held from
// the old process, which ended before it heard of it. The new process
// takes SOXL's final state from the venue's orders, books both legs once,
// settles the lot built and sends nothing more; the held notification,
// arriving now, changes nothing.
func TestRestartAfterTheSend(t *testing.T) {
	restartTest(t)
	a := dbProcess(t, utc8Time(11, 25, 5, 1, 0), nil)
	a.warmUp()
	t0 := utc8Time(11, 25, 5, 3, 5)
	a.at(t0)
	a.books(entryBook())
	a.gate.hold("SOXLUSDT")
	a.stepAt(t0)
	waitFor(t, "SOXS's result and SOXL's held", func() bool { return len(a.cex.UnresolvedOrders()) == 1 && a.gate.heldEvents() > 0 })
	ids := orderIDs(a.cex.CycleOrders(string(a.inv.Cycle().ID)))
	a.down()
	sent := a.gate.placedTotal()

	b := dbProcess(t, t0.Add(time.Second), a.exchange)
	if _, err := b.m.recover(b.m.ctx); err != nil {
		t.Fatal(err)
	}
	b.stepAt(t0.Add(time.Second))
	lot := b.lastLot(string(b.inv.Cycle().Lots[0].ID))
	if lot.Status != string(inventory.LotOpen) {
		t.Errorf("lot %s, want open (built)", lot.Status)
	}
	eqDec(t, "SOXL", lot.EntryValueBull, "4259.385")
	eqDec(t, "SOXS", lot.EntryValueBear, "4172.0292")
	b.gate.release()
	b.stepAt(t0.Add(2 * time.Second))
	if c := b.inv.Cycle(); !c.Lots[0].Legs[cex.Bull].EntryQty.Equal(dec("105.3")) || !c.Lots[0].Legs[cex.Bear].EntryQty.Equal(dec("70.2")) {
		t.Errorf("lot %s / %s after the held reports, want 105.30 / 70.20", c.Lots[0].Legs[cex.Bull].EntryQty, c.Lots[0].Legs[cex.Bear].EntryQty)
	}
	for _, id := range ids {
		if n := b.gate.placeCalls(id); n != 1 {
			t.Errorf("%s handed to the venue %d times, want 1", id, n)
		}
	}
	if n := b.gate.placedTotal() - sent; n != 0 {
		t.Errorf("%d orders from the new process, want none", n)
	}
}

// V15 and SPEC row 6.4, a crash before the send: the entry's rows were
// written and its orders never reached the venue. The new process queries
// them; once the venue has no such order, no fill carries the ids and the
// positions did not move, they are never_arrived, and the batch is void
// (no_fill). Nothing is sent again.
func TestRestartBeforeTheSend(t *testing.T) {
	restartTest(t)
	a := dbProcess(t, utc8Time(11, 25, 5, 1, 0), nil)
	a.warmUp()
	t0 := utc8Time(11, 25, 5, 3, 5)
	a.at(t0)
	a.books(entryBook())
	a.gate.park("SOXLUSDT", "SOXSUSDT")
	a.stepAt(t0)
	waitFor(t, "both outcomes unknown", func() bool { return len(a.cex.UnresolvedOrders()) == 2 })
	ids := orderIDs(a.cex.UnresolvedOrders())
	a.down()

	b := dbProcess(t, t0.Add(time.Second), a.exchange)
	if _, err := b.m.recover(b.m.ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		b.src.setNotFound(id)
	}
	b.stepAt(t0.Add(2 * time.Second)) // the verdicts
	b.stepAt(t0.Add(3 * time.Second)) // the void batch and the cycle's close
	if c := b.lastCycle(); b.inv.Cycle() != nil || c.EndReason != inventory.EndNoFill {
		t.Errorf("cycle open %t, end %q; want closed no_fill", b.inv.Cycle() != nil, c.EndReason)
	}
	for _, id := range ids {
		if n := b.gate.placeCalls(id); n != 1 {
			t.Errorf("%s handed to the venue %d times, want 1 (before the crash)", id, n)
		}
	}
}

// V15: an order the local table never recorded is recognised at the venue
// by its client order id. SOXL's entry row is deleted after the old process
// ended; the new one finds the order among the venue's, books it on the lot
// (105.30 for 4,259.385) and stores that lot on its row.
func TestRestartRecognisesAnUnrecordedOrder(t *testing.T) {
	restartTest(t)
	a := holdingProcess(t)
	bull := a.cex.CycleOrders(string(a.inv.Cycle().ID))[0].Request.ClientOrderID
	lotID := string(a.inv.Cycle().Lots[0].ID)
	a.down()
	execSQL(t, "DELETE FROM trade.cex_order WHERE client_order_id = $1", string(bull))

	b := dbProcess(t, utc8Time(11, 25, 5, 3, 10), a.exchange)
	if _, err := b.m.recover(b.m.ctx); err != nil {
		t.Fatal(err)
	}
	l := b.inv.Cycle().Lots[0]
	if !l.Legs[cex.Bull].EntryQty.Equal(dec("105.3")) || !l.Legs[cex.Bull].EntryValue.Equal(dec("4259.385")) {
		t.Errorf("lot SOXL %s for %s, want 105.30 for 4,259.385", l.Legs[cex.Bull].EntryQty, l.Legs[cex.Bull].EntryValue)
	}
	waitFor(t, "the lot stored on the recognised order's row", func() bool {
		return querySQL[int](t, "SELECT count(*) FROM trade.cex_order WHERE client_order_id = $1 AND lot_id = $2", string(bull), lotID) == 1
	})
}

// V16's last sentence: the exit at the line filled at the venue and the old
// process ended before it heard of it. The new process books both exits
// from the venue's orders, sends nothing, and closes the cycle at the line:
// gross (4,259.385 - 4,250.961) + (4,172.0292 - 4,168.827) = 11.6262.
func TestRestartAfterAnUnrecordedExit(t *testing.T) {
	restartTest(t)
	a := holdingProcess(t)
	t1 := utc8Time(11, 25, 5, 3, 30)
	a.at(t1)
	a.books(exitBook())
	a.gate.hold("SOXLUSDT", "SOXSUSDT")
	a.stepAt(t1)
	waitFor(t, "the exits filled at the venue", func() bool {
		pos, _ := a.paper.Positions(context.Background(), nil)
		return len(pos.BySymbol) == 0
	})
	a.down()
	sent := a.gate.placedTotal()

	b := dbProcess(t, t1.Add(time.Second), a.exchange)
	if _, err := b.m.recover(b.m.ctx); err != nil {
		t.Fatal(err)
	}
	b.stepAt(t1.Add(time.Second))
	if c := b.lastCycle(); b.inv.Cycle() != nil || c.EndReason != inventory.EndExitLine {
		t.Fatalf("cycle open %t, end %q; want closed exit_line", b.inv.Cycle() != nil, c.EndReason)
	}
	eqDec(t, "gross", b.lastCycle().Gross, "11.6262")
	if n := b.gate.placedTotal() - sent; n != 0 {
		t.Errorf("%d orders from the new process, want none", n)
	}
	b.gate.release()
}

// SPEC row 6.4: a restart after a takeover takes the position back: the
// cycle the person took over (stop, alarm, a second stop) is read back with
// status takeover and managed again; at the close window it is closed.
func TestRestartAfterATakeover(t *testing.T) {
	restartTest(t)
	a := holdingProcess(t)
	ts := utc8Time(11, 25, 5, 3, 25)
	a.at(ts)
	b0 := entryBook()
	a.depth(cex.Bull, b0.bullBids, b0.bullAsks) // SOXS without a price for 20 s
	a.m.onSignal(syscall.SIGTERM, ts)
	a.stepAt(ts)
	a.stepAt(ts.Add(60 * time.Second)) // the alarm
	a.m.onSignal(syscall.SIGTERM, ts.Add(61*time.Second))
	if !a.stepAt(ts.Add(61 * time.Second)) {
		t.Fatal("the takeover did not end the old process")
	}
	a.down()
	if s := querySQL[string](t, "SELECT status FROM trade.cycle WHERE NOT live"); s != string(inventory.CycleTakeover) {
		t.Fatalf("stored cycle %s, want takeover", s)
	}

	t1 := utc8Time(11, 26, 4, 59, 10)
	b := dbProcess(t, t1, a.exchange)
	if _, err := b.m.recover(b.m.ctx); err != nil {
		t.Fatal(err)
	}
	b.at(t1)
	b.books(exitBook())
	b.trade(t1)
	if c := b.lastCycle(); b.inv.Cycle() != nil || c.Status != string(inventory.CycleClosed) || c.EndReason != inventory.EndCloseWindow {
		t.Errorf("cycle open %t, %s %q; want closed close_window", b.inv.Cycle() != nil, c.Status, c.EndReason)
	}
}

// V15: a restart in the next Session closes the old Session's cycle at
// once (session_changed), with the late exit alarm; and a restart whose
// positions do not match the ledger sends nothing.
func TestRestartAcrossTheClose(t *testing.T) {
	t.Run("next session", func(t *testing.T) {
		restartTest(t)
		a := holdingProcess(t)
		a.down()
		t1 := utc8Time(11, 26, 5, 5, 0)
		b := dbProcess(t, t1, a.exchange)
		if _, err := b.m.recover(b.m.ctx); err != nil {
			t.Fatal(err)
		}
		b.at(t1)
		b.books(exitBook())
		b.trade(t1)
		b.stepAt(t1.Add(time.Second)) // the cycle closes on the round after the fills
		if c := b.lastCycle(); b.inv.Cycle() != nil || c.EndReason != inventory.EndSessionChanged {
			t.Errorf("cycle open %t, end %q; want closed session_changed", b.inv.Cycle() != nil, c.EndReason)
		}
		if b.eventCount("late_exit") != 1 {
			t.Errorf("events %v, want late_exit", b.eventKinds())
		}
	})
	t.Run("positions differ", func(t *testing.T) {
		restartTest(t)
		a := holdingProcess(t)
		a.down()
		a.paper.SeedPositions([2]decimal.Decimal{dec("-200"), dec("-70.2")})
		sent := a.gate.placedTotal()
		t1 := utc8Time(11, 25, 5, 3, 10)
		b := dbProcess(t, t1, a.exchange)
		if _, err := b.m.recover(b.m.ctx); err != nil {
			t.Fatal(err)
		}
		if !b.inv.Mismatch() {
			t.Fatal("recovered without the mismatch")
		}
		b.at(t1)
		b.books(exitBook())
		b.idleAt(t1, strategy.ReasonPositionMismatch)
		if n := b.gate.placedTotal() - sent; n != 0 {
			t.Errorf("%d orders, want none", n)
		}
	})
}

// perpRows is the recorder's minutes as a minuteSource; it notes what it
// was asked.
type perpRows struct {
	rows     []store.PerpMinuteRow
	from, to time.Time
}

func (p *perpRows) LoadPerpMinutes(_ context.Context, bull, bear string, from, to time.Time) ([]store.PerpMinuteRow, error) {
	p.from, p.to = from, to
	return p.rows, nil
}

// V15: the minutes of the Session are resumed preferring the process's own
// rows; a minute it did not record comes from the recorder's last second
// with both legs' best prices, with F the source's close of that minute; a
// minute without such a second or without F is invalid. At 05:05:30, after
// a start at 05:00: 05:00 from the recorder (mids 40.416 / 59.42), 05:01 own
// (40.384, the recorder's 50.00 is not used), 05:02 from the recorder
// (40.384), 05:03 own and invalid (the recorder's row is not used), 05:04
// without F: invalid. The mean restarts after 05:04, the last ready stats
// are of 05:01 and 05:02; the last built entry comes back for the spacing.
func TestMinutesResumedPreferringOwnRows(t *testing.T) {
	start := utc8Time(11, 25, 5, 0, 0)
	minute := func(i int) time.Time { return start.Add(time.Duration(i) * time.Minute) }
	perp := &perpRows{rows: []store.PerpMinuteRow{
		{MinuteMs: minute(0).UnixMilli(), BidBull: 40.41, AskBull: 40.422, BidBear: 59.415, AskBear: 59.425},
		{MinuteMs: minute(1).UnixMilli(), BidBull: 50, AskBull: 50, BidBear: 50, AskBear: 50},
		{MinuteMs: minute(2).UnixMilli(), BidBull: 40.379, AskBull: 40.389, BidBear: 59.415, AskBear: 59.425},
		{MinuteMs: minute(3).UnixMilli(), BidBull: 50, AskBull: 50, BidBear: 50, AskBear: 50},
		{MinuteMs: minute(4).UnixMilli(), BidBull: 40.379, AskBull: 40.389, BidBear: 59.415, AskBear: 59.425},
	}}
	state := &store.State{
		MinuteBars: []store.MinuteBarRow{
			{MinuteMs: minute(1).UnixMilli(), SessionID: "2026-11-24", MidBull: 40.384, MidBear: 59.42, F: 1, Valid: true, Source: "own"},
			{MinuteMs: minute(3).UnixMilli(), SessionID: "2026-11-24", Valid: false, Source: "own"},
		},
		LastEntry: &store.LotRow{OpenedMs: utc8Time(11, 24, 23, 0, 0).UnixMilli(), XD: 10, Direction: "short"},
	}
	r := newRig(t, utc8Time(11, 25, 5, 5, 30), rigOptions{state: state, perp: perp})
	for i := range 4 {
		r.src.fxCloses = append(r.src.fxCloses, cex.FXMinute{Minute: minute(i), Close: decimal.NewFromInt(1)})
	}
	if _, err := r.m.recover(r.m.ctx); err != nil {
		t.Fatal(err)
	}
	if !perp.from.Equal(start) || !perp.to.Equal(minute(5)) {
		t.Errorf("recorder asked for %s to %s, want 05:00 to 05:05", showTime(perp.from), showTime(perp.to))
	}
	want := []struct {
		valid bool
		bull  float64
	}{{true, 40.416}, {true, 40.384}, {true, 40.384}, {false, 0}, {false, 0}}
	if len(r.m.bars) != len(want) {
		t.Fatalf("%d minutes, want %d", len(r.m.bars), len(want))
	}
	for i, w := range want {
		b := r.m.bars[i]
		if b.Valid != w.valid || (w.valid && (!near1e9(b.MidBull, w.bull) || !near1e9(b.MidBear, 59.42) || b.F != 1)) {
			t.Errorf("minute %d = %+v, want valid %t mid %v", i, b, w.valid, w.bull)
		}
	}
	if r.m.stats.Ready || !r.m.lastReady.Ready || !r.m.lastReady.AsOf.Equal(minute(2)) {
		t.Errorf("stats ready %t, last ready as of %s; want not ready, last ready as of 05:02", r.m.stats.Ready, showTime(r.m.lastReady.AsOf))
	}
	var sources []string
	for _, row := range r.sink.Rows("minute_bar") {
		b := row.(store.MinuteBarRow)
		sources = append(sources, fmt.Sprintf("%d:%s", (b.MinuteMs-start.UnixMilli())/60000, b.Source))
	}
	if got := strings.Join(sources, " "); got != "0:perp_second 2:perp_second" {
		t.Errorf("minute rows written %q, want the two from the recorder", got)
	}
	if l := r.m.lastEntry; l == nil || l.E != 10 || l.Direction != strategy.Short || !l.At.Equal(utc8Time(11, 24, 23, 0, 0)) {
		t.Errorf("last entry %+v", l)
	}
	if !r.m.minute.Equal(minute(5)) {
		t.Errorf("open minute %s, want 05:05", showTime(r.m.minute))
	}
}

// near1e9 compares two prices of a test.
func near1e9(a, b float64) bool { d := a - b; return d < 1e-9 && d > -1e-9 }

// A crash after the lot's row and before its orders' rows: the restart
// finds a lot being built with no order, in the table or at the venue. It
// is settled void at once, and the cycle ends as no_fill.
func TestRestartWithALotWithoutOrders(t *testing.T) {
	restartTest(t)
	a := dbProcess(t, utc8Time(11, 25, 5, 1, 0), nil)
	a.warmUp()
	t0 := utc8Time(11, 25, 5, 3, 5)
	a.at(t0)
	a.books(entryBook())
	a.gate.park("SOXLUSDT", "SOXSUSDT") // the orders never reach the venue
	a.stepAt(t0)
	waitFor(t, "both outcomes unknown", func() bool { return len(a.cex.UnresolvedOrders()) == 2 })
	cycle := string(a.inv.Cycle().ID)
	a.down()
	execSQL(t, "DELETE FROM trade.cex_order WHERE cycle_id = $1", cycle)

	t1 := t0.Add(time.Second)
	b := dbProcess(t, t1, a.exchange)
	if _, err := b.m.recover(b.m.ctx); err != nil {
		t.Fatal(err)
	}
	b.at(t1)
	b.books(entryBook())
	b.stepAt(t1)
	waitFor(t, "the cycle closed as no_fill", func() bool {
		return querySQL[int](t, "SELECT count(*) FROM trade.cycle WHERE cycle_id = $1 AND status = 'closed' AND end_reason = 'no_fill'", cycle) == 1
	})
}
