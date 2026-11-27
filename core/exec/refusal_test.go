package exec

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/inventory"
	"etf-pair-arbitrage/core/logger"
	"etf-pair-arbitrage/core/store"
	"etf-pair-arbitrage/core/strategy"
)

// Audit of the first thin-first live run, item 一.3: a reducing order the
// venue refuses because it is larger than the position it closes (Bitget
// 40804, 25231 by REST) changes the next action. The positions are read
// once, the round ends there, and the next reducing order of that leg is
// sized within what the venue holds in the cycle's direction; the refused
// quantity is not sent again until new evidence comes.

// qtyRefusal is the venue's refusal of a reduce-only order above the
// position, as the Bitget adapter reports it.
func qtyRefusal() error {
	return &cex.VenueError{Code: "40804", Message: "The number of closed positions cannot exceed the number of positions held", Kind: cex.ErrorReduceQty}
}

// refuseBullAbove refuses SOXL's reduce-only orders above limit for their
// quantity.
func refuseBullAbove(limit string) func(req cex.OrderRequest) error {
	return func(req cex.OrderRequest) error {
		if req.Leg == cex.Bull && req.ReduceOnly && req.Qty.GreaterThan(dec(limit)) {
			return qtyRefusal()
		}
		return nil
	}
}

// incidentState is the long c…904692 at 05:06:52 as its ledger reads
// without the dust: SOXS closed (8.03 in two exit batches), SOXL 7.80 of
// 15.58 left after the first batch's 7.78; the second batch's thin SOXS
// 4.02 filled and its SOXL not sent yet. The venue nets SOXL 7.79.
func incidentState(start time.Time) *store.State {
	st := takenBackState(start)
	st.Inventory.Dust = nil
	st.Cycle.Status = string(inventory.CycleOpen)
	st.Orders = st.Orders[:5]
	return st
}

// The acceptance of item 一.3 with the incident's numbers: the exit batch's
// SOXL 7.80 is refused; the next round reads the venue's positions (7.79)
// and sends nothing; the round after it sends 7.79.
func TestQtyRefusalReadsThePositionsAndResizes(t *testing.T) {
	start := utc8Time(11, 25, 5, 6, 53)
	r := newRig(t, start, rigOptions{state: incidentState(start), thin: "bear", perp: &perpRows{}})
	if _, err := r.m.recover(r.m.ctx); err != nil {
		t.Fatal(err)
	}
	if e := r.m.activeExit(); e == nil || !e.started {
		t.Fatalf("exit batch taken up %+v, want the one of SOXS 4.02", r.m.batch)
	}
	r.paper.SeedPositions([2]decimal.Decimal{dec("7.79"), decimal.Zero})
	r.gate.setRefuse(refuseBullAbove("7.79"))

	r.at(start)
	r.books(exitBook())
	r.stepOnce()
	r.results()
	o := r.orders()
	if len(o) != 1 || !o[0].Request.Qty.Equal(dec("7.8")) || o[0].Status != cex.StatusRejected {
		t.Fatalf("orders %+v, want SOXL 7.80 refused", o)
	}

	reads := r.gate.reads.Load()
	t1 := start.Add(250 * time.Millisecond)
	r.at(t1)
	r.books(exitBook())
	r.stepOnce()
	if got := r.gate.reads.Load(); got != reads+1 || len(r.orders()) != 1 || r.m.idleReason != strategy.ReasonPositionsRead {
		t.Fatalf("after the refusal: %d reads, %d orders, idle %q; want one read, no order, positions_read", got-reads, len(r.orders()), r.m.idleReason)
	}
	if q := r.cex.Positions().BySymbol["SOXLUSDT"].Qty; !q.Equal(dec("7.79")) {
		t.Fatalf("venue SOXL read %s, want 7.79", q)
	}

	t2 := t1.Add(250 * time.Millisecond)
	r.at(t2)
	r.books(exitBook())
	r.stepOnce()
	r.results()
	o = r.orders()
	if len(o) != 2 {
		t.Fatalf("%d orders, want the second SOXL; idle %q", len(o), r.m.idleReason)
	}
	if q := o[1].Request; q.Leg != cex.Bull || !q.ReduceOnly || !q.Qty.Equal(dec("7.79")) || o[1].Status != cex.StatusFilled {
		t.Errorf("second order %s reduce-only %t qty %s %s, want SOXL 7.79 filled", q.Leg, q.ReduceOnly, q.Qty, o[1].Status)
	}
}

// When the venue's positions still show the refused quantity, the order is
// not sent again, whatever the book: the round waits (qty_refused) for new
// evidence. The comparison of the positions due every 10 s is such
// evidence: once it reads SOXL 7.79 the trim goes at 7.79.
func TestQtyRefusalIsNotRepeated(t *testing.T) {
	start := utc8Time(11, 25, 5, 1, 0)
	r := newRig(t, start, rigOptions{state: incidentState(start)})
	r.gate.setRefuse(refuseBullAbove("7.79"))
	at := func(t time.Time) {
		r.at(t)
		r.books(exitBook())
		r.stepOnce()
	}
	at(start)
	r.results()
	at(start.Add(250 * time.Millisecond)) // the read
	if r.m.idleReason != strategy.ReasonPositionsRead {
		t.Fatalf("idle %q after the refusal, want positions_read", r.m.idleReason)
	}
	for i := 2; i <= 6; i++ {
		at(start.Add(time.Duration(i) * 250 * time.Millisecond))
		if n := len(r.orders()); n != 1 || r.m.idleReason != strategy.ReasonQtyRefused {
			t.Fatalf("round %d: %d orders, idle %q; want the refused 7.80 not sent again (qty_refused)", i, n, r.m.idleReason)
		}
	}

	r.paper.SeedPositions([2]decimal.Decimal{dec("7.79"), decimal.Zero})
	at(start.Add(11 * time.Second)) // the comparison reads the positions
	if n := len(r.orders()); n != 1 {
		t.Fatalf("%d orders in the comparison's round, want none", n)
	}
	at(start.Add(11*time.Second + 250*time.Millisecond))
	r.results()
	o := r.orders()
	if len(o) != 2 || !o[1].Request.Qty.Equal(dec("7.79")) || o[1].Status != cex.StatusFilled {
		t.Fatalf("orders %+v, want SOXL 7.79 filled after the venue read 7.79", o)
	}
}

// Other legal reducing actions go on while one leg waits: in a forced exit
// SOXL's 105.30 is refused for its quantity while the venue shows 105.30,
// and SOXS's first order fills 50 of 70.20. The positions the venue pushes
// after SOXS's fill arrived after the refusal: no read is needed. SOXL is
// not sent again; SOXS's 20.20 goes on at every new book.
func TestQtyRefusalHoldsOnlyItsLeg(t *testing.T) {
	r := lossRig(t, rigOptions{})
	r.gate.setRefuse(refuseBullAbove("0"))
	t0 := utc8Time(11, 25, 5, 3, 5)
	r.m.onSignal(syscall.SIGTERM, t0)
	thin := exitBook()
	thin.bearAsks = side{lv("59.385", "50"), lv("60.50", "100")}
	for i := 0; i < 4; i++ {
		r.at(t0.Add(time.Duration(i) * 250 * time.Millisecond))
		r.books(thin)
		r.stepOnce()
		r.results()
	}
	o := r.orders()
	if len(o) < 3 {
		t.Fatalf("%d orders, want the two forced orders and SOXS's rest; idle %q", len(o), r.m.idleReason)
	}
	if q := o[0].Request; q.Leg != cex.Bull || !q.Qty.Equal(dec("105.3")) || o[0].Status != cex.StatusRejected {
		t.Fatalf("first order %s %s %s, want SOXL 105.30 refused", q.Leg, q.Qty, o[0].Status)
	}
	for _, x := range o[1:] {
		if q := x.Request; q.Leg != cex.Bear || q.Purpose != cex.PurposeForced {
			t.Errorf("order %s %s %s after SOXL's refusal, want SOXS's forced orders only", q.Leg, q.Purpose, q.Qty)
		}
	}
	if q := o[2].Request; !q.Qty.Equal(dec("20.2")) {
		t.Errorf("SOXS's second order %s, want the rest 20.20", q.Qty)
	}
}

// Audit item 一.3, last part: the alarm of refusals in a row reaches what
// the person on call watches. During the incident trade.event held one
// stop_not_flat error and nothing of the 21,796 refusals. orders_rejected
// is an error-level trade.event row of the run, which the Live Monitor's
// Alerts panel lists (its events branch: trade.event of the picked Run,
// level error or warn, shown critical, Open, source "Program · exec", What
// "Orders Rejected"), and an Error line of the process log.
func TestRefusalAlarmReachesTheAlerts(t *testing.T) {
	restartTest(t)
	logs := captureLogs(t)
	start := utc8Time(11, 25, 5, 1, 0)
	state := takenBackState(start)
	state.Orders = state.Orders[:5]
	p := dbProcessWith(t, start, nil, rigOptions{state: state, thin: "bear"})
	p.gate.setRefuse(func(cex.OrderRequest) error {
		return &cex.VenueError{Code: "25203", Message: "Insufficient margin", Kind: cex.ErrorOther}
	})
	at := start
	for i := 0; i < 3; i++ {
		p.at(at)
		p.books(exitBook())
		p.stepAt(at)
		p.results()
		at = at.Add(250 * time.Millisecond)
	}
	p.stepAt(at)
	p.down()

	rows := `FROM trade.event e WHERE e.run_id = $1 AND e.live = false AND e.level IN ('error', 'warn') AND e.kind = 'orders_rejected'`
	if n := querySQL[int64](t, "SELECT count(*) "+rows, string(p.run)); n != 1 {
		t.Fatalf("%d orders_rejected rows of the run in trade.event, want 1", n)
	}
	what := querySQL[string](t, "SELECT e.level || ' ' || 'Program · ' || e.module || ' ' || initcap(replace(e.kind, '_', ' ')) "+rows, string(p.run))
	if what != "error Program · exec Orders Rejected" {
		t.Errorf("the Alerts row reads %q, want an error of Program · exec, Orders Rejected", what)
	}
	if !logs.has(slog.LevelError, "exec: orders refused in a row") {
		t.Errorf("no Error line for the refusals in a row")
	}
}

// logTee is the process logger of the exec tests, set once in TestMain
// before any goroutine logs: it writes to the logger the package had and,
// while a test captures, to that test's logCapture. Swapping
// logger.GlobalLogger inside a test would race with the goroutines an
// earlier test left (an order's send, the paper venue's matching).
type logTee struct {
	next slog.Handler
	mu   *sync.Mutex
	cap  **logCapture
}

var (
	teeMu      sync.Mutex
	teeCapture *logCapture
)

func TestMain(m *testing.M) {
	logger.GlobalLogger = slog.New(logTee{next: logger.GlobalLogger.Handler(), mu: &teeMu, cap: &teeCapture})
	os.Exit(m.Run())
}

func (h logTee) Enabled(ctx context.Context, l slog.Level) bool { return true }
func (h logTee) WithAttrs(a []slog.Attr) slog.Handler {
	return logTee{next: h.next.WithAttrs(a), mu: h.mu, cap: h.cap}
}
func (h logTee) WithGroup(g string) slog.Handler {
	return logTee{next: h.next.WithGroup(g), mu: h.mu, cap: h.cap}
}
func (h logTee) Handle(ctx context.Context, r slog.Record) error {
	h.mu.Lock()
	c := *h.cap
	h.mu.Unlock()
	if c != nil {
		c.add(r)
	}
	if !h.next.Enabled(ctx, r.Level) {
		return nil
	}
	return h.next.Handle(ctx, r)
}

// logCapture keeps the process log's records of a test.
type logCapture struct {
	mu   sync.Mutex
	recs []slog.Record
}

// captureLogs keeps the process log's records until the test ends.
func captureLogs(t *testing.T) *logCapture {
	c := &logCapture{}
	teeMu.Lock()
	teeCapture = c
	teeMu.Unlock()
	t.Cleanup(func() {
		teeMu.Lock()
		teeCapture = nil
		teeMu.Unlock()
	})
	return c
}

func (c *logCapture) add(r slog.Record) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recs = append(c.recs, r.Clone())
}

// has reports whether a record of level starts with msg.
func (c *logCapture) has(level slog.Level, msg string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.recs {
		if r.Level == level && strings.HasPrefix(r.Message, msg) {
			return true
		}
	}
	return false
}
