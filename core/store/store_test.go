package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"etf-pair-arbitrage/core/internal/dbtest"
	"fmt"
	"log/slog"
	"math"
	"net"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/logger"
)

// rowTypes is one zero row of every table.
var rowTypes = []Row{ProcessRunRow{}, MinuteBarRow{}, SignalSampleRow{}, AnchorFactRow{}, CycleRow{}, LotRow{}, OrderRow{},
	FillRow{}, DecisionRow{}, FundingRow{}, InventorySampleRow{}, InventoryStateRow{}, EventRow{}}

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

// --- tests that need no database ---

// Every table of the schema has exactly one row type, and every row type
// encodes: its fields have supported types and it carries its table's key.
func TestRowTypesCoverTables(t *testing.T) {
	seen := map[string]bool{}
	for _, row := range rowTypes {
		p, err := planFor(row)
		if err != nil {
			t.Errorf("%T: %v", row, err)
			continue
		}
		if seen[p.table] {
			t.Errorf("table %s has two row types", p.table)
		}
		seen[p.table] = true
	}
	for table := range tables {
		if !seen[table] {
			t.Errorf("table %s has no row type", table)
		}
	}
}

// The NULL rules of a row: an `omitempty` field at its zero value, a float
// that is not finite and an empty JSON value are written as NULL; everything
// else is written as it is.
func TestRowValuesAndNulls(t *testing.T) {
	row := LotRow{LotID: "l1", CycleID: "c1", Seq: 2, Status: "building", Direction: "long",
		XD: -3.5, MuD: math.NaN(), SigmaD: math.Inf(1), EntryQtyBull: dec("12.50"), OpenedMs: 1000}
	p, err := planFor(row)
	if err != nil {
		t.Fatal(err)
	}
	args := p.args(row, "run-1", true, 0)
	got := map[string]any{"run_id": args[0], "live": args[1]}
	for i, c := range p.cols {
		got[c.name] = args[2+i]
	}
	want := map[string]any{"run_id": "run-1", "live": true, "lot_id": "l1", "seq": int64(2), "x_d": -3.5,
		"mu_d": nil, "sigma_d": nil, // not finite
		"void_reason": nil, "closed_ms": nil, // omitempty and zero
		"entry_qty_bull": "12.5", "exit_qty_bull": "0", "opened_ms": int64(1000)}
	for name, w := range want {
		if !reflect.DeepEqual(got[name], w) {
			t.Errorf("%s = %#v, want %#v", name, got[name], w)
		}
	}
	if args := mustPlan(t, EventRow{}).args(EventRow{}, "run-1", false, 7); args[2] != int64(7) || args[len(args)-1] != nil {
		t.Errorf("event row: seq = %#v (want 7), empty detail = %#v (want NULL)", args[2], args[len(args)-1])
	}
}

func mustPlan(t *testing.T, row Row) *rowPlan {
	t.Helper()
	p, err := planFor(row)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// A full queue drops new rows and counts them; Record never blocks. A
// WriteSync that finds the queue full fails at once.
func TestQueueFullDropsAndCounts(t *testing.T) {
	tm := defaultTimings
	tm.queueSize = 3
	s := newStore(Config{}, "run-queue", nil, tm) // no writer runs: nothing leaves the queue
	for i := range 5 {
		s.Record(EventRow{TsMs: int64(i), Module: "test", Level: "info", Kind: "k", Message: "m"})
	}
	if got := s.Dropped(); got != 2 {
		t.Errorf("dropped = %d, want 2", got)
	}
	if got := len(s.queue); got != 3 {
		t.Errorf("queued = %d, want 3", got)
	}
	if err := s.WriteSync(EventRow{TsMs: 9}); !errors.Is(err, errQueueFull) {
		t.Errorf("WriteSync on a full queue = %v, want errQueueFull", err)
	}
}

// badRow has a field the store cannot write.
type badRow struct {
	C chan int `json:"c"`
}

func (badRow) Table() string { return "event" }

// A row the store cannot encode is a programming error: Record drops it and
// WriteSync returns the error; neither reaches the queue.
func TestUnencodableRowIsRefused(t *testing.T) {
	s := newStore(Config{}, "run-bad", nil, defaultTimings)
	s.Record(badRow{})
	if err := s.WriteSync(badRow{}); err == nil {
		t.Error("WriteSync of an unencodable row must fail")
	}
	if got := len(s.queue); got != 0 {
		t.Errorf("queued = %d, want 0", got)
	}
}

// The Fake keeps rows per table in order, and fails WriteSync while told to.
func TestFake(t *testing.T) {
	var f Fake
	var _ Sink = &f
	f.Record(EventRow{Kind: "a"}, OrderRow{ClientOrderID: "o1"})
	if err := f.WriteSync(EventRow{Kind: "b"}); err != nil {
		t.Fatal(err)
	}
	if got := f.Rows("event"); len(got) != 2 || got[0].(EventRow).Kind != "a" || got[1].(EventRow).Kind != "b" {
		t.Errorf("event rows = %+v", got)
	}
	if got := f.Last("cex_order"); got.(OrderRow).ClientOrderID != "o1" {
		t.Errorf("last cex_order = %+v", got)
	}
	if f.Last("cycle") != nil || !f.Healthy() {
		t.Error("an empty table must have no last row, and a new Fake must be healthy")
	}

	failure := errors.New("database down")
	f.Fail(failure)
	if err := f.WriteSync(EventRow{Kind: "c"}); !errors.Is(err, failure) || f.Healthy() {
		t.Errorf("while failing: WriteSync = %v, healthy = %t", err, f.Healthy())
	}
	f.Record(EventRow{Kind: "d"}) // queued rows are kept through a failure
	f.Fail(nil)
	if got := f.Rows("event"); len(got) != 3 || got[2].(EventRow).Kind != "d" || !f.Healthy() {
		t.Errorf("after the failure: event rows = %+v, healthy = %t", got, f.Healthy())
	}

	// The other meaning of a WriteSync error: the rows were written.
	f.FailAfterWrite(failure)
	if err := f.WriteSync(EventRow{Kind: "e"}); !errors.Is(err, failure) || f.Healthy() {
		t.Errorf("failing after the write: WriteSync = %v, healthy = %t", err, f.Healthy())
	}
	f.Fail(nil)
	if got := f.Rows("event"); len(got) != 4 || got[3].(EventRow).Kind != "e" || !f.Healthy() {
		t.Errorf("after a failure that kept its rows: event rows = %+v, healthy = %t", got, f.Healthy())
	}
}

// A database that cannot be reached, or a dsn that cannot be parsed, refuses
// the start with an error that names the failure and carries nothing of the
// dsn: no user, password, database, host or port. Nothing of it is logged.
func TestOpenErrorsCarryNothingOfTheDSN(t *testing.T) {
	logs := captureLogs(t)
	// A local port nothing listens on.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)
	ln.Close()
	parts := []string{"SECRETUSER", "SECRETPW", "SECRETDB", "127.0.0.1", ":" + port, "=" + port}

	check := func(what string, s *Store, err error, want string) {
		t.Helper()
		if s != nil || err == nil {
			t.Fatalf("%s: New must fail, got store %v, err %v", what, s != nil, err)
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error %q, want it to say %q", what, err, want)
		}
		for _, part := range parts {
			if strings.Contains(err.Error(), part) || strings.Contains(logs.String(), part) {
				t.Errorf("%s: %q of the dsn is in the error or the log:\nerror: %v\nlog: %s", what, part, err, logs.String())
			}
		}
	}
	s, err := New(Config{}, "run-unreachable", "postgres://SECRETUSER:SECRETPW@127.0.0.1:"+port+"/SECRETDB")
	check("unreachable port", s, err, "store: cannot reach the database: connection refused")
	s, err = New(Config{}, "run-bad-dsn", "postgres://SECRETUSER:SECRETPW@127.0.0.1:"+port+"x/SECRETDB")
	check("unparsable dsn", s, err, "store: the dsn cannot be parsed")
}

// --- tests against a database ---

// testConfig returns the pool configuration of the test database, or skips.
// The tests delete rows, so the production port is refused, and they hold
// the database (dbtest.Lock) against the other packages' tests.
func testConfig(t testing.TB) *pgxpool.Config {
	t.Helper()
	dbtest.Lock(t)
	dsn := os.Getenv("ETFPAIR_TRADE_TEST_DSN")
	pc, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("ETFPAIR_TRADE_TEST_DSN cannot be parsed")
	}
	if pc.ConnConfig.Port == 5432 {
		t.Fatal("ETFPAIR_TRADE_TEST_DSN points at port 5432: these tests delete rows and run only against a development database")
	}
	return pc
}

// testTimings keep the production sizes with a short retry, so a recovery
// test does not wait a second per attempt.
func testTimings() timings {
	tm := defaultTimings
	tm.retryEvery = 50 * time.Millisecond
	return tm
}

// openStore opens a store on the test database without starting its writer,
// and a second pool to look at the tables with. Each test gets its own run
// id. The store is closed when the test ends.
func openStore(t testing.TB, cfg Config, pc *pgxpool.Config, tm timings) (*Store, *pgxpool.Pool) {
	t.Helper()
	run := RunID(fmt.Sprintf("test-%d-%s", time.Now().UnixNano(), t.Name()))
	s, err := open(cfg, run, pc, tm)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	check, err := pgxpool.NewWithConfig(context.Background(), testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(check.Close)
	return s, check
}

// startStore is openStore with the writer running.
func startStore(t testing.TB, cfg Config, pc *pgxpool.Config, tm timings) (*Store, *pgxpool.Pool) {
	t.Helper()
	s, check := openStore(t, cfg, pc, tm)
	go s.Run(context.Background())
	return s, check
}

// gate makes the database of pc unreachable on demand: while down is set no
// connection can be made. cut also drops the store's open connections.
type gate struct{ down atomic.Bool }

func gated(pc *pgxpool.Config) *gate {
	g := &gate{}
	pc.ConnConfig.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if g.down.Load() {
			return nil, errors.New("test: database unreachable")
		}
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	return g
}

func (g *gate) cut(s *Store) {
	g.down.Store(true)
	s.pool.Reset()
}

func (g *gate) restore() { g.down.Store(false) }

// syncBuffer is a log sink the writer goroutine and the test both use.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLogs routes the package's logger to a buffer until the test ends.
// Call it before opening a store, so the swap happens before any writer runs
// and is undone after every store of the test has closed.
func captureLogs(t testing.TB) *syncBuffer {
	t.Helper()
	old, buf := logger.GlobalLogger, &syncBuffer{}
	logger.GlobalLogger = slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Cleanup(func() { logger.GlobalLogger = old })
	return buf
}

// waitFor polls cond for up to five seconds.
func waitFor(t testing.TB, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// testEvent is an event row named by kind; countEvents counts this store's
// event rows of a kind.
func testEvent(kind string) EventRow {
	return EventRow{TsMs: 1, Module: "test", Level: "info", Kind: kind, Message: "m"}
}

func countEvents(t testing.TB, check *pgxpool.Pool, s *Store, kind string) int64 {
	t.Helper()
	return query1[int64](t, check, "SELECT count(*) FROM trade.event WHERE run_id = $1 AND kind = $2", string(s.run), kind)
}

// noFlush is a flush interval that never fires in a test, so only a full
// batch, a WriteSync or Close makes the writer write.
const noFlush = time.Hour

// clearTables empties every table but process_run, which another package's
// test writes at the same time.
func clearTables(t testing.TB, check *pgxpool.Pool) {
	t.Helper()
	for table := range tables {
		if table == "process_run" {
			continue
		}
		if _, err := check.Exec(context.Background(), "DELETE FROM trade."+table); err != nil {
			t.Fatal(err)
		}
	}
}

// query1 returns the single value of a one-row query.
func query1[T any](t testing.TB, check *pgxpool.Pool, sql string, args ...any) T {
	t.Helper()
	var v T
	if err := check.QueryRow(context.Background(), sql, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return v
}

// flushed waits until everything recorded so far is committed: an empty
// WriteSync commits after all rows queued before it.
func flushed(t testing.TB, s *Store) {
	t.Helper()
	if err := s.WriteSync(); err != nil {
		t.Fatalf("flush: %v", err)
	}
}

// asMap is a row as plain JSON values, so two rows compare by content:
// decimals by their text, JSON columns by their meaning.
func asMap(t testing.TB, row any) map[string]any {
	t.Helper()
	b, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// Every row type fits its table: a zero row, with every `omitempty` and JSON
// column NULL, is accepted by each of the 13 tables.
func TestEveryTableTakesItsRow(t *testing.T) {
	s, check := startStore(t, Config{}, testConfig(t), testTimings())
	clearTables(t, check)
	for _, row := range rowTypes {
		if err := s.WriteSync(row); err != nil {
			t.Errorf("%s: %v", row.Table(), err)
			continue
		}
		if n := query1[int64](t, check, "SELECT count(*) FROM trade."+row.Table()+" WHERE run_id = $1 AND NOT live", string(s.run)); n != 1 {
			t.Errorf("%s: %d rows of this run, want 1", row.Table(), n)
		}
	}
}

// The three write rules. State: the last write of a key wins. Fact: the first
// write of a key stays. Sample: every time gets its row, and a repeat is
// ignored. Decision and event rows are numbered by the store in one sequence.
func TestWriteRules(t *testing.T) {
	s, check := startStore(t, Config{}, testConfig(t), testTimings())
	clearTables(t, check)

	// State: an order registered as pending, then filled.
	order := OrderRow{ClientOrderID: "etfp-rules-1", CycleID: "c-rules", Leg: "bull", Symbol: "SOXLUSDT", Purpose: "entry", Side: "buy",
		Price: dec("72.01"), Qty: dec("10"), Status: "pending", SubmittedMs: 1000}
	s.Record(order)
	order.Status, order.OrderID, order.CumExecQty, order.AvgPrice = "filled", "v-1", dec("10"), dec("72.005")
	s.Record(order)

	// Fact: the same execution reported twice.
	fill := FillRow{ExecID: "x-rules-1", ClientOrderID: "etfp-rules-1", OrderID: "v-1", Symbol: "SOXLUSDT", Side: "buy",
		Price: dec("72.005"), Qty: dec("10"), Value: dec("720.05"), Fee: dec("0.0468"), ExecMs: 1001, ReceivedMs: 1002}
	s.Record(fill)
	fill.Price = dec("99")
	s.Record(fill)

	// Sample: two times, and the first one again.
	// The first has read a flat bull position and no bear position yet.
	s.Record(InventorySampleRow{TsMs: 10_000, R: dec("1"), PosBull: decimal.NewNullDecimal(decimal.Zero)},
		InventorySampleRow{TsMs: 20_000, R: dec("2")}, InventorySampleRow{TsMs: 10_000, R: dec("3")})

	// Numbered rows: decision, event, decision.
	s.Record(DecisionRow{TsMs: 1, Kind: "entry"}, EventRow{TsMs: 2, Module: "exec", Level: "error", Kind: "stop_not_flat", Message: "m"}, DecisionRow{TsMs: 3, Kind: "exit"})
	flushed(t, s)

	if n := query1[int64](t, check, "SELECT count(*) FROM trade.cex_order WHERE client_order_id = 'etfp-rules-1'"); n != 1 {
		t.Errorf("state: %d order rows, want 1", n)
	}
	if got := query1[string](t, check, "SELECT status || '/' || order_id || '/' || avg_price::text FROM trade.cex_order WHERE client_order_id = 'etfp-rules-1'"); got != "filled/v-1/72.005" {
		t.Errorf("state: the order is %s, want the last write filled/v-1/72.005", got)
	}
	if got := query1[string](t, check, "SELECT price::text FROM trade.cex_fill WHERE exec_id = 'x-rules-1'"); got != "72.005" {
		t.Errorf("fact: the fill's price is %s, want the first write 72.005", got)
	}
	if got := query1[string](t, check, "SELECT string_agg(ts_ms::text || '=' || r::text, ',' ORDER BY ts_ms) FROM trade.inventory_sample WHERE run_id = $1", string(s.run)); got != "10000=1,20000=2" {
		t.Errorf("sample: rows are %s, want 10000=1,20000=2", got)
	}
	if n := query1[int64](t, check, "SELECT count(*) FROM trade.inventory_sample WHERE run_id = $1 AND ts_ms = 10000 AND pos_bull = 0 AND pos_bear IS NULL", string(s.run)); n != 1 {
		t.Error("sample: a read position of zero must be 0 and a position not read yet NULL")
	}
	if rows, err := load[InventorySampleRow](context.Background(), check, "run_id = $1 AND ts_ms = 10000", string(s.run)); err != nil || len(rows) != 1 ||
		!rows[0].PosBull.Valid || !rows[0].PosBull.Decimal.IsZero() || rows[0].PosBear.Valid {
		t.Errorf("sample read back = %+v (err %v), want pos_bull a valid 0 and pos_bear not valid", rows, err)
	}
	numbered := "SELECT string_agg(seq::text || ':' || kind, ',' ORDER BY seq) FROM (SELECT seq, kind FROM trade.decision WHERE run_id = $1 UNION ALL SELECT seq, kind FROM trade.event WHERE run_id = $1) AS n"
	if got := query1[string](t, check, numbered, string(s.run)); got != "1:entry,2:stop_not_flat,3:exit" {
		t.Errorf("numbered rows are %s, want 1:entry,2:stop_not_flat,3:exit", got)
	}
	if !s.Healthy() || s.Dropped() != 0 {
		t.Errorf("healthy = %t, dropped = %d after plain writes", s.Healthy(), s.Dropped())
	}
}

// WriteSync returns after the commit: the rows are there for another
// connection at once, and read back equal to what was written. NULL columns
// come back as zero values, a NULL statistic as NaN.
func TestWriteSyncReadBack(t *testing.T) {
	s, check := startStore(t, Config{}, testConfig(t), testTimings())
	clearTables(t, check)
	ctx := context.Background()

	cycle := CycleRow{CycleID: "c-sync", SessionID: "2026-11-23", Direction: "long", Status: "open",
		PairAnchorKind: "bell", PairAnchorBull: dec("72.1234"), PairAnchorBear: dec("59.5010"), OpenedMs: 1_795_000_000_000,
		Gross: dec("-0.25"), Fees: dec("0.0936"), Funding: dec("0"), FundingKnown: true, Net: dec("-0.3436")}
	lot := LotRow{LotID: "c-sync-1", CycleID: "c-sync", Seq: 1, Status: "building", Direction: "long", XD: -4.25, MuD: 1.5, SigmaD: 2.75, FeeD: 2.6,
		FeeUSDTEst: dec("1.3"), EntryQtyBull: dec("10"), EntryValueBull: dec("720.05"), EntryFeeBull: dec("0.0468"), OpenedMs: 1_795_000_000_100}
	order := OrderRow{ClientOrderID: "etfp-sync-1", CycleID: "c-sync", LotID: "c-sync-1", Leg: "bull", Symbol: "SOXLUSDT", Purpose: "entry", Side: "buy",
		Price: dec("72.01"), Qty: dec("10.00"), QuotaPrice: dec("72.01"), Status: "pending", FeeKnown: true, SubmittedMs: 1_795_000_000_100}
	if err := s.WriteSync(cycle, lot, order); err != nil {
		t.Fatal(err)
	}

	cycles, err := load[CycleRow](ctx, check, "cycle_id = $1", "c-sync")
	if err != nil || len(cycles) != 1 {
		t.Fatalf("cycle read back: %d rows, err %v", len(cycles), err)
	}
	if got, want := asMap(t, cycles[0]), asMap(t, cycle); !reflect.DeepEqual(got, want) {
		t.Errorf("cycle read back\n got %v\nwant %v", got, want)
	}
	lots, err := load[LotRow](ctx, check, "lot_id = $1", "c-sync-1")
	if err != nil || len(lots) != 1 {
		t.Fatalf("lot read back: %d rows, err %v", len(lots), err)
	}
	if got, want := asMap(t, lots[0]), asMap(t, lot); !reflect.DeepEqual(got, want) {
		t.Errorf("lot read back\n got %v\nwant %v", got, want)
	}
	orders, err := load[OrderRow](ctx, check, "client_order_id = $1", "etfp-sync-1")
	if err != nil || len(orders) != 1 {
		t.Fatalf("order read back: %d rows, err %v", len(orders), err)
	}
	if got, want := asMap(t, orders[0]), asMap(t, order); !reflect.DeepEqual(got, want) {
		t.Errorf("order read back\n got %v\nwant %v", got, want)
	}
	if !lots[0].EntryValueBull.Equal(dec("720.05")) || !cycles[0].PairAnchorBull.Equal(dec("72.1234")) {
		t.Errorf("decimals read back as %s and %s, want exactly 720.05 and 72.1234", lots[0].EntryValueBull, cycles[0].PairAnchorBull)
	}

	// What the columns hold for the empty fields.
	if n := query1[int64](t, check, "SELECT count(*) FROM trade.cex_order WHERE client_order_id = 'etfp-sync-1' AND order_id IS NULL AND avg_price IS NULL AND sent_ms IS NULL AND fee_known AND NOT not_found AND NOT live AND run_id = $1", string(s.run)); n != 1 {
		t.Error("the pending order's empty order_id, avg_price and sent_ms must be NULL, with this run's run_id and live = false")
	}

	// A statistic that is not finite is NULL in the table and NaN when read.
	lot.XD = math.NaN()
	if err := s.WriteSync(lot); err != nil {
		t.Fatal(err)
	}
	if n := query1[int64](t, check, "SELECT count(*) FROM trade.lot WHERE lot_id = 'c-sync-1' AND x_d IS NULL AND mu_d = 1.5"); n != 1 {
		t.Error("a NaN statistic must be written as NULL")
	}
	if lots, _ = load[LotRow](ctx, check, "lot_id = $1", "c-sync-1"); len(lots) != 1 || !math.IsNaN(lots[0].XD) {
		t.Errorf("a NULL statistic must read back as NaN, got %+v", lots)
	}
}

// A state row written by WriteSync is not overwritten by an older state of
// the same key that was recorded just before and had not been written yet.
func TestWriteSyncKeepsOrderWithRecordedRows(t *testing.T) {
	s, check := startStore(t, Config{}, testConfig(t), testTimings())
	clearTables(t, check)
	cycle := CycleRow{CycleID: "c-order", SessionID: "2026-11-23", Direction: "short", Status: "open", PairAnchorKind: "bell", OpenedMs: 1000}
	for i := range 20 {
		cycle.Status, cycle.ClosedMs = "open", 0
		cycle.Net = decimal.NewFromInt(int64(i))
		s.Record(cycle) // the booking of this step, still in the queue
		cycle.Status, cycle.ClosedMs = "closed", 2000
		if err := s.WriteSync(cycle); err != nil { // the close, in the same step
			t.Fatal(err)
		}
		if got := query1[string](t, check, "SELECT status FROM trade.cycle WHERE cycle_id = 'c-order'"); got != "closed" {
			t.Fatalf("round %d: status right after the synchronous close = %s, want closed", i, got)
		}
	}
	time.Sleep(2 * s.timings.flushEvery) // nothing older may arrive later
	if got := query1[string](t, check, "SELECT status FROM trade.cycle WHERE cycle_id = 'c-order'"); got != "closed" {
		t.Errorf("status after the queue drained = %s, want closed", got)
	}
}

// While the database cannot be reached: Healthy is false, recorded rows are
// kept and retried, the failure is logged again at the log interval, and a
// WriteSync fails at once instead of waiting out its timeout. After it is
// back: the kept rows are written, Healthy is true, and the rows of the
// refused WriteSync are not written. No log line carries anything of the
// connection string.
func TestWriteFailureAndRecovery(t *testing.T) {
	logs := captureLogs(t)
	pc := testConfig(t)
	db := gated(pc)
	tm := testTimings()
	tm.writeTimeout = 2 * time.Second
	tm.failLogEvery = 100 * time.Millisecond
	s, check := startStore(t, Config{}, pc, tm)
	clearTables(t, check)

	if err := s.WriteSync(testEvent("before")); err != nil || !s.Healthy() {
		t.Fatalf("before the failure: err = %v, healthy = %t", err, s.Healthy())
	}

	// The database goes away.
	db.cut(s)
	s.Record(testEvent("during"))
	waitFor(t, "the writer to start retrying", s.retrying.Load)
	if s.Healthy() {
		t.Error("Healthy() must be false while a batch is being retried")
	}
	start := time.Now()
	err := s.WriteSync(testEvent("sync-during"))
	if took := time.Since(start); !errors.Is(err, errUnhealthy) || took > 200*time.Millisecond {
		t.Errorf("WriteSync during the failure = %v after %s, want errUnhealthy at once", err, took)
	}
	if n := countEvents(t, check, s, "during"); n != 0 {
		t.Errorf("%d rows written while the database was unreachable", n)
	}
	waitFor(t, "the failure to be logged a second time", func() bool {
		return strings.Count(logs.String(), "store: write failing; the batch is kept and retried") >= 2
	})

	// It comes back: the writer's next retry gets through.
	db.restore()
	waitFor(t, "Healthy() to turn true", s.Healthy)
	flushed(t, s)
	if n := countEvents(t, check, s, "during"); n != 1 {
		t.Errorf("the row recorded during the failure is there %d times, want 1", n)
	}
	if n := countEvents(t, check, s, "sync-during"); n != 0 {
		t.Errorf("the refused WriteSync's row was written %d times, want 0", n)
	}
	if n := countEvents(t, check, s, "before"); n != 1 {
		t.Errorf("the row from before the failure is there %d times, want 1", n)
	}
	if !strings.Contains(logs.String(), "store: write recovered") {
		t.Error("the recovery must be logged")
	}
	assertNoConnectionDetails(t, "the log", logs.String(), pc)
}

// assertNoConnectionDetails fails when text carries the user, the database,
// the host or the port of pc.
func assertNoConnectionDetails(t testing.TB, what, text string, pc *pgxpool.Config) {
	t.Helper()
	c := pc.ConnConfig
	for _, part := range []string{c.User, c.Database, c.Host, fmt.Sprintf(":%d", c.Port), fmt.Sprintf("=%d", c.Port)} {
		if strings.Contains(text, part) {
			t.Errorf("%s carries %q of the connection string:\n%s", what, part, text)
		}
	}
}

// The writer has taken a WriteSync together with rows recorded before it, and
// the transaction fails because the database is gone: the caller gets the
// error, its rows are dropped for good, and the recorded rows are kept and
// written once the database is back.
func TestFailedSyncWriteKeepsRecordedRows(t *testing.T) {
	logs := captureLogs(t)
	pc := testConfig(t)
	db := gated(pc)
	tm := testTimings()
	tm.flushEvery = noFlush // the recorded row waits for the WriteSync behind it
	s, check := startStore(t, Config{}, pc, tm)
	clearTables(t, check)
	flushed(t, s)

	db.cut(s)
	s.Record(testEvent("recorded"))
	err := s.WriteSync(testEvent("sync"))
	if err == nil || errors.Is(err, errTimeout) || errors.Is(err, errUnhealthy) || errors.Is(err, errQueueFull) {
		t.Fatalf("WriteSync = %v, want the writer's own failure", err)
	}
	if !strings.Contains(logs.String(), "store: synchronous write failed; its rows are not retried") {
		t.Error("the failed synchronous write must be logged as such")
	}
	assertNoConnectionDetails(t, "the WriteSync error", err.Error(), pc)

	db.restore()
	waitFor(t, "the recorded row to be written", func() bool { return countEvents(t, check, s, "recorded") == 1 })
	waitFor(t, "Healthy() to turn true", s.Healthy)
	flushed(t, s)
	if n := countEvents(t, check, s, "sync"); n != 0 {
		t.Errorf("the failed WriteSync's row was written %d times, want 0", n)
	}
	if n := countEvents(t, check, s, "recorded"); n != 1 {
		t.Errorf("the recorded row is there %d times, want 1", n)
	}
	assertNoConnectionDetails(t, "the log", logs.String(), pc)
}

// unknownColumnRow names a column the event table does not have, as a row
// struct does after a field was added without the matching schema change.
type unknownColumnRow struct {
	TsMs    int64  `json:"ts_ms"`
	Module  string `json:"module"`
	Level   string `json:"level"`
	Kind    string `json:"kind"`
	Message string `json:"message"`
	Extra   string `json:"no_such_column"`
}

func (unknownColumnRow) Table() string { return "event" }

// A row the database refuses for its content does not stop the writer: the
// row is dropped and counted, the rows around it are written, the store stays
// healthy and takes a synchronous write at once. The log names the table and
// the SQLSTATE and never a value of the row.
func TestRejectedRowDoesNotBlockTheWriter(t *testing.T) {
	logs := captureLogs(t)
	tm := testTimings()
	tm.flushEvery = noFlush // all four rows go in the WriteSync's transaction
	s, check := startStore(t, Config{}, testConfig(t), tm)
	clearTables(t, check)
	withDetail := func(kind, detail string) EventRow {
		e := testEvent(kind)
		e.Detail = json.RawMessage(detail)
		return e
	}

	// A good row, a row whose detail is not JSON (a data exception, found
	// when the statement runs), a good row.
	s.Record(withDetail("before", `{"a":1}`), withDetail("poison", `{not json SECRET-VALUE`), withDetail("after", `{"a":2}`))
	start := time.Now()
	if err := s.WriteSync(testEvent("sync")); err != nil {
		t.Fatalf("WriteSync behind a rejected row: %v", err)
	}
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Errorf("WriteSync behind a rejected row took %s", took)
	}
	for kind, want := range map[string]int64{"before": 1, "poison": 0, "after": 1, "sync": 1} {
		if n := countEvents(t, check, s, kind); n != want {
			t.Errorf("%s rows written: %d, want %d", kind, n, want)
		}
	}
	if got := s.Rejected(); got != 1 {
		t.Errorf("rejected = %d, want 1", got)
	}

	// A row naming a column the table lacks (refused when the statement is
	// prepared, before any row of the batch is sent), between two good rows.
	s.Record(testEvent("before-2"), unknownColumnRow{TsMs: 1, Module: "test", Level: "info", Kind: "unknown-column", Message: "SECRET-VALUE"}, testEvent("after-2"))
	flushed(t, s)
	if b, a := countEvents(t, check, s, "before-2"), countEvents(t, check, s, "after-2"); b != 1 || a != 1 {
		t.Errorf("rows around the unknown column: before=%d after=%d, want 1 and 1", b, a)
	}
	if got := s.Rejected(); got != 2 {
		t.Errorf("rejected = %d, want 2", got)
	}
	if !s.Healthy() {
		t.Error("a rejected row must not make the store unhealthy")
	}

	// A synchronous row of its own that is refused: the caller gets the
	// error with the SQLSTATE and the store goes on.
	err := s.WriteSync(withDetail("sync-poison", `{not json SECRET-VALUE`))
	if err == nil || !strings.Contains(err.Error(), "SQLSTATE 22P02") || strings.Contains(err.Error(), "SECRET-VALUE") {
		t.Errorf("WriteSync of a refused row = %v, want an error with SQLSTATE 22P02 and without the value", err)
	}
	if err := s.WriteSync(testEvent("sync-2")); err != nil || !s.Healthy() {
		t.Errorf("after a refused synchronous row: err = %v, healthy = %t", err, s.Healthy())
	}

	out := logs.String()
	for _, want := range []string{"store: rows rejected by the database and dropped", "event: SQLSTATE 22P02", "event: SQLSTATE 42703", "no_such_column"} {
		if !strings.Contains(out, want) {
			t.Errorf("the log lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "SECRET-VALUE") || strings.Contains(out, "not json") {
		t.Errorf("the log carries a value of a rejected row:\n%s", out)
	}
}

// A batch is written as soon as it holds batchRows rows, without waiting for
// the flush interval.
func TestFullBatchIsWrittenAtOnce(t *testing.T) {
	tm := testTimings()
	tm.flushEvery = noFlush
	s, check := startStore(t, Config{}, testConfig(t), tm)
	clearTables(t, check)
	for range tm.batchRows - 1 {
		s.Record(testEvent("batch"))
	}
	time.Sleep(150 * time.Millisecond)
	if n := countEvents(t, check, s, "batch"); n != 0 {
		t.Fatalf("%d rows written before the batch was full", n)
	}
	s.Record(testEvent("batch"))
	waitFor(t, "the full batch to be written", func() bool { return countEvents(t, check, s, "batch") == int64(tm.batchRows) })
}

// Close writes everything still queued: with the writer running, when the
// writer was never started, and when Close and Run start at the same moment.
// A Run started after Close does nothing.
func TestCloseWritesQueuedRows(t *testing.T) {
	tm := testTimings()
	tm.flushEvery = noFlush
	const rows = 1000 // several batches

	t.Run("writer running", func(t *testing.T) {
		s, check := startStore(t, Config{}, testConfig(t), tm)
		clearTables(t, check)
		flushed(t, s)
		for range rows {
			s.Record(testEvent("close"))
		}
		s.Close()
		if n := countEvents(t, check, s, "close"); n != rows {
			t.Errorf("%d rows written at Close, want %d", n, rows)
		}
	})
	t.Run("writer never started", func(t *testing.T) {
		s, check := openStore(t, Config{}, testConfig(t), tm)
		for range rows {
			s.Record(testEvent("close"))
		}
		s.Close()
		if n := countEvents(t, check, s, "close"); n != rows {
			t.Errorf("%d rows written at Close, want %d", n, rows)
		}
		finished := make(chan struct{})
		go func() { s.Run(context.Background()); close(finished) }()
		select {
		case <-finished:
		case <-time.After(2 * time.Second):
			t.Error("Run after Close must return at once")
		}
	})
	t.Run("writer starting", func(t *testing.T) {
		for round := range 20 {
			s, check := openStore(t, Config{}, testConfig(t), tm)
			s.Record(testEvent("close"), testEvent("close"), testEvent("close"))
			go s.Run(context.Background())
			s.Close()
			if n := countEvents(t, check, s, "close"); n != 3 {
				t.Fatalf("round %d: %d rows written at Close, want 3", round, n)
			}
		}
	})
}

// Two unfinished cycles of one mode are a broken record: LoadState refuses
// to pick one.
func TestLoadStateRefusesTwoUnfinishedCycles(t *testing.T) {
	s, check := startStore(t, Config{}, testConfig(t), testTimings())
	clearTables(t, check)
	cycle := func(id, status string) CycleRow {
		return CycleRow{CycleID: id, SessionID: "2026-11-23", Direction: "long", Status: status, PairAnchorKind: "bell", OpenedMs: 1000}
	}
	if err := s.WriteSync(cycle("c-one", "open"), cycle("c-two", "takeover")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadState(context.Background(), "2026-11-23"); err == nil || !strings.Contains(err.Error(), "2 unfinished cycles") {
		t.Errorf("LoadState = %v, want the two unfinished cycles refused", err)
	}
}

// LoadState returns the unfinished cycle with its lots and orders and leaves
// a finished cycle and the other mode's rows alone; a cycle taken over by a
// person is still unfinished. The session's minutes and anchor facts come
// back, another session's do not.
func TestLoadState(t *testing.T) {
	pc := testConfig(t)
	s, check := startStore(t, Config{Live: false}, pc, testTimings())
	clearTables(t, check)
	live, _ := startStore(t, Config{Live: true}, testConfig(t), testTimings())
	ctx := context.Background()
	const session, earlier = "2026-11-23", "2026-11-20"

	cycle := func(id, status string, openedMs int64) CycleRow {
		return CycleRow{CycleID: id, SessionID: session, Direction: "long", Status: status, PairAnchorKind: "bell",
			PairAnchorBull: dec("72"), PairAnchorBear: dec("59.5"), OpenedMs: openedMs}
	}
	lot := func(id, cycleID string, seq int, status string, openedMs int64) LotRow {
		return LotRow{LotID: id, CycleID: cycleID, Seq: seq, Status: status, Direction: "long", XD: -4, OpenedMs: openedMs}
	}
	order := func(id, cycleID, lotID, status string, submittedMs int64) OrderRow {
		return OrderRow{ClientOrderID: id, CycleID: cycleID, LotID: lotID, Leg: "bull", Symbol: "SOXLUSDT", Purpose: "entry", Side: "buy",
			Price: dec("72"), Qty: dec("5"), Status: status, SubmittedMs: submittedMs}
	}

	// Paper: a finished cycle, then the unfinished one with a built lot and a
	// lot still building.
	done := cycle("c-done", "closed", 1000)
	done.ClosedMs, done.EndReason = 2000, "exit_line"
	if err := s.WriteSync(
		done, lot("c-done-1", "c-done", 1, "closed", 1000), order("etfp-done-1", "c-done", "c-done-1", "filled", 1000),
		cycle("c-open", "open", 5000),
		lot("c-open-2", "c-open", 2, "building", 7000), lot("c-open-1", "c-open", 1, "open", 5000),
		order("etfp-open-3", "c-open", "c-open-2", "pending", 7000), order("etfp-open-1", "c-open", "c-open-1", "filled", 5000), order("etfp-open-2", "c-open", "c-open-1", "cancelled", 5001),
		InventoryStateRow{Capital: dec("1000"), RClosed: dec("-12.5"), Dust: json.RawMessage(`{"bear":{"qty":"-0.02","avg_price":"59.42"}}`)},
		MinuteBarRow{MinuteMs: 120_000, SessionID: session, MidBull: 72.5, MidBear: 59.25, F: 0.9995, Valid: true, Source: "own"},
		MinuteBarRow{MinuteMs: 60_000, SessionID: session, Valid: false, Source: "perp_second", MidBull: math.NaN(), MidBear: math.NaN(), F: math.NaN()},
		MinuteBarRow{MinuteMs: 30_000, SessionID: earlier, MidBull: 70, MidBear: 61, F: 1, Valid: true, Source: "own"},
		AnchorFactRow{SessionID: session, Kind: "bell", Leg: "bull", KnownMs: 100, Value: dec("72.11"), F: dec("0.9996"), Source: "binance"},
		AnchorFactRow{SessionID: session, Kind: "bell", Leg: "bear", KnownMs: 100, Value: dec("59.48"), F: dec("0.9996"), Source: "binance"},
		AnchorFactRow{SessionID: earlier, Kind: "nav", Leg: "bull", KnownMs: 50, Value: dec("70.02"), Source: "direxion"},
	); err != nil {
		t.Fatal(err)
	}
	// Live: its own unfinished cycle and state, which paper must not see.
	if err := live.WriteSync(cycle("c-live", "open", 9000), lot("c-live-1", "c-live", 1, "open", 9000),
		InventoryStateRow{Capital: dec("5000"), RClosed: dec("3")},
		MinuteBarRow{MinuteMs: 120_000, SessionID: session, MidBull: 1, MidBear: 1, F: 1, Valid: true, Source: "own"},
		AnchorFactRow{SessionID: session, Kind: "bell", Leg: "bull", KnownMs: 100, Value: dec("1"), F: dec("1"), Source: "binance"}); err != nil {
		t.Fatal(err)
	}

	st, err := s.LoadState(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	if st.Cycle == nil || st.Cycle.CycleID != "c-open" || !st.Cycle.PairAnchorBear.Equal(dec("59.5")) {
		t.Fatalf("cycle = %+v, want c-open", st.Cycle)
	}
	ids := func(rows any) []string {
		var out []string
		v := reflect.ValueOf(rows)
		for i := range v.Len() {
			out = append(out, v.Index(i).Field(0).String())
		}
		return out
	}
	if got := ids(st.Lots); !reflect.DeepEqual(got, []string{"c-open-1", "c-open-2"}) {
		t.Errorf("lots = %v, want c-open-1, c-open-2 in their order within the cycle", got)
	}
	if got := ids(st.Orders); !reflect.DeepEqual(got, []string{"etfp-open-1", "etfp-open-2", "etfp-open-3"}) {
		t.Errorf("orders = %v, want the three of c-open, oldest first", got)
	}
	if st.Inventory == nil || !st.Inventory.RClosed.Equal(dec("-12.5")) || !st.Inventory.Capital.Equal(dec("1000")) {
		t.Errorf("inventory state = %+v, want paper's: capital 1000, r_closed -12.5", st.Inventory)
	}
	if len(st.MinuteBars) != 2 || st.MinuteBars[0].MinuteMs != 60_000 || st.MinuteBars[0].Valid || st.MinuteBars[1].MidBull != 72.5 || st.MinuteBars[1].F != 0.9995 {
		t.Errorf("minute bars = %+v, want this session's two of paper, oldest first", st.MinuteBars)
	}
	if len(st.AnchorFacts) != 2 || st.AnchorFacts[0].Leg != "bear" || !st.AnchorFacts[1].Value.Equal(dec("72.11")) {
		t.Errorf("anchor facts = %+v, want this session's two of paper", st.AnchorFacts)
	}
	if st.LastEntry == nil || st.LastEntry.LotID != "c-open-1" || st.LastEntry.XD != -4 {
		t.Errorf("last entry = %+v, want c-open-1: the newest lot that was built", st.LastEntry)
	}

	// Live sees only its own.
	if lst, err := live.LoadState(ctx, session); err != nil || lst.Cycle == nil || lst.Cycle.CycleID != "c-live" || len(lst.Lots) != 1 ||
		lst.Inventory == nil || !lst.Inventory.Capital.Equal(dec("5000")) || len(lst.MinuteBars) != 1 || len(lst.AnchorFacts) != 1 ||
		lst.LastEntry == nil || lst.LastEntry.LotID != "c-live-1" {
		t.Errorf("live state = %+v (err %v), want only live's rows", lst, err)
	}

	// A cycle a person took over is still read back.
	if err := s.WriteSync(cycle("c-open", "takeover", 5000)); err != nil {
		t.Fatal(err)
	}
	if st, err = s.LoadState(ctx, session); err != nil || st.Cycle == nil || st.Cycle.Status != "takeover" || len(st.Orders) != 3 {
		t.Errorf("after takeover: cycle = %+v, %d orders, err %v", st.Cycle, len(st.Orders), err)
	}

	// With every cycle finished there is none to continue, and the last
	// entry is still known.
	closed := cycle("c-open", "closed", 5000)
	closed.ClosedMs = 8000
	if err := s.WriteSync(closed, lot("c-open-2", "c-open", 2, "void", 7000)); err != nil {
		t.Fatal(err)
	}
	if st, err = s.LoadState(ctx, session); err != nil || st.Cycle != nil || len(st.Lots) != 0 || len(st.Orders) != 0 || st.LastEntry == nil || st.LastEntry.LotID != "c-open-1" {
		t.Errorf("after the close: cycle = %+v, lots %d, orders %d, last entry %+v, err %v", st.Cycle, len(st.Lots), len(st.Orders), st.LastEntry, err)
	}
}

// What the ledger needs from LoadState beside the unfinished cycle: every
// funding record booked, oldest first, so a record is booked once; the
// finished cycles whose funding is not known yet, so a late record completes
// them; and the dust as the last cycle recorded it. The other mode's rows
// stay out.
func TestLoadStateForTheLedger(t *testing.T) {
	s, check := startStore(t, Config{Live: false}, testConfig(t), testTimings())
	clearTables(t, check)
	live, _ := startStore(t, Config{Live: true}, testConfig(t), testTimings())
	ctx := context.Background()
	const session = "2026-11-23"

	awaited := json.RawMessage(`[{"settlement_ms":3500,"leg":"bull"}]`)
	cycle := func(id, status string, openedMs, closedMs int64, fundingKnown bool) CycleRow {
		row := CycleRow{CycleID: id, SessionID: session, Direction: "short", Status: status, PairAnchorKind: "bell",
			PairAnchorBull: dec("40"), PairAnchorBear: dec("60"), OpenedMs: openedMs, ClosedMs: closedMs, FundingKnown: fundingKnown,
			DustBear: dec("-0.02")}
		if status == "closed" && !fundingKnown {
			row.FundingAwaited = awaited
		}
		return row
	}
	funding := func(id, symbol, amount string, tsMs, settlementMs int64, cycleID string) FundingRow {
		return FundingRow{RecordID: id, Symbol: symbol, Type: "funding", Amount: dec(amount), TsMs: tsMs, SettlementMs: settlementMs, CycleID: cycleID}
	}
	dust := json.RawMessage(`{"bear":{"qty":"-0.02","avg_price":"60"}}`)
	if err := s.WriteSync(
		cycle("c-known", "closed", 1000, 2000, true),
		cycle("c-wait-2", "closed", 5000, 6000, false),
		cycle("c-wait-1", "closed", 3000, 4000, false),
		cycle("c-open", "open", 7000, 0, false), // unfinished: it is State.Cycle, not a pending one
		funding("f-2", "SOXSUSDT", "0.05", 3600, 3500, "c-wait-1"),
		funding("f-1", "SOXLUSDT", "-0.12", 1600, 1500, "c-known"),
		funding("f-3", "SOXLUSDT", "-0.01", 9000, 8900, ""),
		InventoryStateRow{Capital: dec("1000"), SinceMs: 900, RClosed: dec("15.62529116"), Dust: dust},
	); err != nil {
		t.Fatal(err)
	}
	// A funding record is a fact: a second write of its id changes nothing.
	if err := s.WriteSync(funding("f-1", "SOXLUSDT", "-99", 1600, 1500, "c-known")); err != nil {
		t.Fatal(err)
	}
	if err := live.WriteSync(cycle("c-live-wait", "closed", 3000, 4000, false), funding("f-live", "SOXLUSDT", "1", 1600, 1500, "")); err != nil {
		t.Fatal(err)
	}

	st, err := s.LoadState(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	if st.Cycle == nil || st.Cycle.CycleID != "c-open" {
		t.Errorf("cycle = %+v, want c-open", st.Cycle)
	}
	var ids []string
	for _, f := range st.Funding {
		ids = append(ids, f.RecordID)
	}
	if !reflect.DeepEqual(ids, []string{"f-1", "f-2", "f-3"}) {
		t.Errorf("funding = %v, want paper's three, oldest first", ids)
	}
	if f := st.Funding[0]; !f.Amount.Equal(dec("-0.12")) || f.SettlementMs != 1500 || f.CycleID != "c-known" || f.Symbol != "SOXLUSDT" {
		t.Errorf("funding f-1 = %+v, want the first write: -0.12 of settlement 1500 on c-known", f)
	}
	if st.Funding[2].CycleID != "" {
		t.Errorf("funding f-3 cycle = %q, want none", st.Funding[2].CycleID)
	}
	ids = nil
	for _, c := range st.PendingFunding {
		ids = append(ids, c.CycleID)
	}
	if !reflect.DeepEqual(ids, []string{"c-wait-1", "c-wait-2"}) {
		t.Errorf("cycles awaiting funding = %v, want c-wait-1 and c-wait-2, oldest first", ids)
	}
	if c := st.PendingFunding[0]; c.OpenedMs != 3000 || c.ClosedMs != 4000 || !c.DustBear.Equal(dec("-0.02")) || !c.PairAnchorBear.Equal(dec("60")) ||
		!reflect.DeepEqual(asMap(t, c)["funding_awaited"], asMap(t, CycleRow{FundingAwaited: awaited})["funding_awaited"]) {
		t.Errorf("pending cycle = %+v, want the whole row with what it awaits", c)
	}
	if len(st.Cycle.FundingAwaited) != 0 {
		t.Errorf("the open cycle's funding_awaited = %s, want NULL read back as empty", st.Cycle.FundingAwaited)
	}
	if st.Inventory == nil || !st.Inventory.RClosed.Equal(dec("15.62529116")) || st.Inventory.SinceMs != 900 ||
		!reflect.DeepEqual(asMap(t, st.Inventory)["dust"], asMap(t, InventoryStateRow{Dust: dust})["dust"]) {
		t.Errorf("inventory state = %+v, want since 900, r_closed 15.62529116 and the dust as written", st.Inventory)
	}

	// Live sees its own only.
	lst, err := live.LoadState(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	if len(lst.Funding) != 1 || lst.Funding[0].RecordID != "f-live" || len(lst.PendingFunding) != 1 || lst.PendingFunding[0].CycleID != "c-live-wait" || lst.Inventory != nil {
		t.Errorf("live state = %+v, want only live's rows", lst)
	}
}

// The board's role reads schema trade and cannot write it: its sessions are
// read-only by default, and it holds no write privilege when a session turns
// that off. The role is reached on the test database by swapping the user of
// the test connection string (the development cluster trusts local users).
func TestReaderRoleIsReadOnly(t *testing.T) {
	s, _ := startStore(t, Config{}, testConfig(t), testTimings())
	if err := s.WriteSync(EventRow{TsMs: 1, Module: "test", Level: "info", Kind: "reader", Message: "m"}); err != nil {
		t.Fatal(err)
	}
	pc := testConfig(t)
	pc.ConnConfig.User = "etfpair_trade_reader"
	reader, err := pgxpool.NewWithConfig(context.Background(), pc)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	ctx := context.Background()

	if n := query1[int64](t, reader, "SELECT count(*) FROM trade.event WHERE run_id = $1", string(s.run)); n != 1 {
		t.Errorf("the reader sees %d rows of this run, want 1", n)
	}
	const insert = "INSERT INTO trade.event (run_id, seq, live, ts_ms, module, level, kind, message) VALUES ('reader', 1, false, 1, 'm', 'l', 'k', 'x')"
	sqlState := func(err error) string {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			return pgErr.Code
		}
		return fmt.Sprint(err)
	}
	if _, err := reader.Exec(ctx, insert); sqlState(err) != "25006" {
		t.Errorf("insert in the reader's default session: %v, want SQLSTATE 25006 (read-only transaction)", err)
	}
	tx, err := reader.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(tx)
	if _, err := tx.Exec(ctx, "SET TRANSACTION READ WRITE"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, insert); sqlState(err) != "42501" {
		t.Errorf("insert in a read-write transaction of the reader: %v, want SQLSTATE 42501 (insufficient privilege)", err)
	}
}

// BenchmarkWriteSync measures one synchronous commit as a caller sees it:
// WriteSync of a single cex_order row, as written before an order is sent.
// Run it with a fixed count, for example:
//
//	go test ./store -run '^$' -bench WriteSync -benchtime 200x
func BenchmarkWriteSync(b *testing.B) {
	s, check := startStore(b, Config{}, testConfig(b), defaultTimings)
	clearTables(b, check)
	order := OrderRow{CycleID: "c-bench", LotID: "c-bench-1", Leg: "bull", Symbol: "SOXLUSDT", Purpose: "entry", Side: "buy",
		Price: dec("72.01"), Qty: dec("10"), Status: "pending", SubmittedMs: 1}
	if err := s.WriteSync(order); err != nil { // connect and prepare outside the measurement
		b.Fatal(err)
	}
	took := make([]time.Duration, 0, b.N)
	b.ResetTimer()
	for i := range b.N {
		order.ClientOrderID = fmt.Sprintf("etfp-bench-%d", i)
		start := time.Now()
		if err := s.WriteSync(order); err != nil {
			b.Fatal(err)
		}
		took = append(took, time.Since(start))
	}
	b.StopTimer()
	sort.Slice(took, func(i, j int) bool { return took[i] < took[j] })
	ms := func(q float64) float64 {
		return float64(took[int(q*float64(len(took)-1))].Microseconds()) / 1000
	}
	b.ReportMetric(ms(0.5), "p50-ms")
	b.ReportMetric(ms(0.95), "p95-ms")
	b.ReportMetric(ms(1), "max-ms")
}

// Paper and live keep separate books even when both use the same ids: cycle
// ids are creation milliseconds and both modes follow the same market, so
// equal ids must be expected. Each mode writes a cycle, a lot, an order, a
// fill and a funding record under the same ids; each then starts again and
// finds its own quantities and status, untouched by the other.
func TestPaperAndLiveDoNotShareKeys(t *testing.T) {
	paper, check := startStore(t, Config{Live: false}, testConfig(t), testTimings())
	clearTables(t, check)
	live, _ := startStore(t, Config{Live: true}, testConfig(t), testTimings())
	ctx := context.Background()
	const session = "2026-11-23"

	write := func(s *Store, status, qty string) {
		t.Helper()
		if err := s.WriteSync(
			CycleRow{CycleID: "c1795000000000", SessionID: session, Direction: "long", Status: status, PairAnchorKind: "bell", OpenedMs: 1000, Net: dec(qty)},
			LotRow{LotID: "c1795000000000-1", CycleID: "c1795000000000", Seq: 1, Status: "open", Direction: "long", EntryQtyBull: dec(qty), OpenedMs: 1000},
			OrderRow{ClientOrderID: "etfp-same-1", CycleID: "c1795000000000", LotID: "c1795000000000-1", Leg: "bull", Symbol: "SOXLUSDT", Purpose: "entry", Side: "buy",
				Price: dec("72"), Qty: dec(qty), Status: "filled", CumExecQty: dec(qty), SubmittedMs: 1000},
			FillRow{ExecID: "x-same-1", ClientOrderID: "etfp-same-1", OrderID: "v1", Symbol: "SOXLUSDT", Side: "buy", Price: dec("72"), Qty: dec(qty), Value: dec("1"), ExecMs: 1, ReceivedMs: 2},
			FundingRow{RecordID: "f-same-1", Symbol: "SOXLUSDT", Type: "t", Amount: dec(qty), TsMs: 1, SettlementMs: 1, CycleID: "c1795000000000"},
		); err != nil {
			t.Fatal(err)
		}
	}
	// Live first, then paper with the same ids, then live once more after
	// paper: neither order of writing lets one mode touch the other.
	write(live, "open", "11")
	write(paper, "takeover", "22")
	expect := func(what string, s *Store, status, qty string) {
		t.Helper()
		st, err := s.LoadState(ctx, session)
		if err != nil || st.Cycle == nil || len(st.Lots) != 1 || len(st.Orders) != 1 {
			t.Fatalf("%s: state after restart = cycle %v, %d lots, %d orders, err %v", what, st.Cycle != nil, len(st.Lots), len(st.Orders), err)
		}
		if st.Cycle.Status != status || !st.Lots[0].EntryQtyBull.Equal(dec(qty)) || !st.Orders[0].CumExecQty.Equal(dec(qty)) {
			t.Errorf("%s: status %s, lot qty %s, order qty %s; want %s and %s", what, st.Cycle.Status, st.Lots[0].EntryQtyBull, st.Orders[0].CumExecQty, status, qty)
		}
		for _, table := range []string{"cex_fill", "funding"} {
			column := map[string]string{"cex_fill": "qty", "funding": "amount"}[table]
			if got := query1[string](t, check, "SELECT "+column+"::text FROM trade."+table+" WHERE live = $1", s.cfg.Live); got != qty {
				t.Errorf("%s: %s.%s = %s, want %s", what, table, column, got, qty)
			}
		}
	}
	expect("live after paper wrote", live, "open", "11")
	expect("paper", paper, "takeover", "22")
	write(live, "open", "33")
	expect("paper after live wrote again", paper, "takeover", "22")
}

// slowEvents makes every event row of kind "slow" take 40 ms to insert, until
// the test ends.
func slowEvents(t testing.TB, check *pgxpool.Pool) {
	t.Helper()
	run := func(sql string) {
		t.Helper()
		if _, err := check.Exec(context.Background(), sql); err != nil {
			t.Fatal(err)
		}
	}
	run(`CREATE OR REPLACE FUNCTION trade.test_slow_event() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.kind = 'slow' THEN PERFORM pg_sleep(0.04); END IF; RETURN NEW; END $$`)
	run(`DROP TRIGGER IF EXISTS test_slow_event ON trade.event`)
	run(`CREATE TRIGGER test_slow_event BEFORE INSERT ON trade.event FOR EACH ROW EXECUTE FUNCTION trade.test_slow_event()`)
	t.Cleanup(func() {
		run(`DROP TRIGGER IF EXISTS test_slow_event ON trade.event`)
		run(`DROP FUNCTION IF EXISTS trade.test_slow_event()`)
	})
}

// WriteSync keeps its caller's bound when the writer is busy: with a rejected
// row and twelve slow rows queued ahead (about half a second of work) and a
// limit of 100 ms, the caller gets "not confirmed" at its deadline. The
// writer goes on by itself: the rows ahead are all written, and the row with
// which the caller then voids its order is the last state of that order.
func TestWriteSyncReturnsAtItsDeadline(t *testing.T) {
	tm := testTimings()
	tm.flushEvery = noFlush
	tm.writeTimeout = 100 * time.Millisecond
	s, check := startStore(t, Config{}, testConfig(t), tm)
	clearTables(t, check)
	slowEvents(t, check)

	poison := testEvent("poison")
	poison.Detail = json.RawMessage(`{not json`)
	s.Record(poison)
	for range 12 {
		s.Record(testEvent("slow"))
	}
	order := OrderRow{ClientOrderID: "etfp-deadline-1", CycleID: "c-deadline", Leg: "bull", Symbol: "SOXLUSDT", Purpose: "entry", Side: "buy",
		Price: dec("72"), Qty: dec("5"), Status: "pending", SubmittedMs: 1}
	start := time.Now()
	err := s.WriteSync(order)
	took := time.Since(start)
	if !errors.Is(err, errTimeout) {
		t.Errorf("WriteSync = %v, want errTimeout", err)
	}
	if took < 90*time.Millisecond || took > 200*time.Millisecond {
		t.Errorf("WriteSync returned after %s, want its 100 ms limit", took)
	}

	// Told that the commit is not confirmed, the caller does not send the
	// order and records it as not sent.
	order.Status = "not_sent"
	s.Record(order)
	waitFor(t, "the writer to finish the rows ahead and take a new request", func() bool { return s.WriteSync() == nil })
	if n := countEvents(t, check, s, "slow"); n != 12 {
		t.Errorf("%d of the 12 slow rows written", n)
	}
	if got := query1[string](t, check, "SELECT status FROM trade.cex_order WHERE client_order_id = 'etfp-deadline-1'"); got != "not_sent" {
		t.Errorf("the order's last state is %s, want not_sent", got)
	}
	if !s.Healthy() || s.Rejected() != 1 {
		t.Errorf("healthy = %t, rejected = %d; want true and 1", s.Healthy(), s.Rejected())
	}
}

// The recorder's minutes for a restart: per minute (M, M + 60 s] the last
// second in which both legs have a best bid and ask on Bitget. A second at
// the minute's start closes the minute before; a second with one leg only,
// or with an empty side, or of another venue, is not used.
func TestLoadPerpMinutes(t *testing.T) {
	s, check := openStore(t, Config{}, testConfig(t), testTimings())
	ctx := context.Background()
	if _, err := check.Exec(ctx, "DELETE FROM etfpair.perp_second"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { check.Exec(context.Background(), "DELETE FROM etfpair.perp_second") })
	m := time.Date(2026, 11, 24, 21, 0, 0, 0, time.UTC) // 11-25 05:00 UTC+8
	put := func(venue, symbol string, after time.Duration, bid, ask string) {
		t.Helper()
		if _, err := check.Exec(ctx, `INSERT INTO etfpair.perp_second (venue, symbol, second_ms, bid_px, ask_px) VALUES ($1, $2, $3, $4::numeric[], $5::numeric[])`,
			venue, symbol, m.Add(after).UnixMilli(), bid, ask); err != nil {
			t.Fatal(err)
		}
	}
	both := func(after time.Duration, bull, bear [2]string) {
		put("bitget", "SOXLUSDT", after, "{"+bull[0]+"}", "{"+bull[1]+"}")
		put("bitget", "SOXSUSDT", after, "{"+bear[0]+"}", "{"+bear[1]+"}")
	}
	both(0, [2]string{"1", "2"}, [2]string{"3", "4"})                              // closes the minute before
	both(30*time.Second, [2]string{"40.1", "40.2"}, [2]string{"59.1", "59.2"})     // an earlier second
	both(60*time.Second, [2]string{"40.41", "40.42"}, [2]string{"59.41", "59.43"}) // the close of 05:00
	put("bitget", "SOXLUSDT", 90*time.Second, "{41}", "{42}")                      // SOXL only
	put("bitget", "SOXLUSDT", 100*time.Second, "{41}", "{42}")
	put("bitget", "SOXSUSDT", 100*time.Second, "{}", "{60}")                      // SOXS without a bid
	both(110*time.Second, [2]string{"40.5", "40.52"}, [2]string{"59.3", "59.31"}) // the close of 05:01
	put("binance", "SOXLUSDT", 115*time.Second, "{1}", "{2}")
	put("binance", "SOXSUSDT", 115*time.Second, "{1}", "{2}")

	got, err := s.LoadPerpMinutes(ctx, "SOXLUSDT", "SOXSUSDT", m, m.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	want := []PerpMinuteRow{
		{MinuteMs: m.UnixMilli(), SecondMs: m.Add(time.Minute).UnixMilli(), BidBull: 40.41, AskBull: 40.42, BidBear: 59.41, AskBear: 59.43},
		{MinuteMs: m.Add(time.Minute).UnixMilli(), SecondMs: m.Add(110 * time.Second).UnixMilli(), BidBull: 40.5, AskBull: 40.52, BidBear: 59.3, AskBear: 59.31},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("minutes\n got %+v\nwant %+v", got, want)
	}
}
