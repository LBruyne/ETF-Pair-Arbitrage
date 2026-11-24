package store

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/logger"
)

// schemaSQL creates the tables of schema trade; applied at every start.
//
//go:embed schema.sql
var schemaSQL string

// schemaLockKey is the advisory lock taken while the schema is applied, so
// that two processes starting together do not create the same table at once.
const schemaLockKey int64 = 0x65746670_74726164 // "etfptrad"

// openTimeout bounds connecting and applying the schema at start.
const openTimeout = 10 * time.Second

// poolSize is the number of connections kept: one for the writer and one for
// LoadState.
const poolSize = 2

var (
	errQueueFull = errors.New("store: queue full")
	errUnhealthy = errors.New("store: the database cannot be written; a batch is being retried")
	errTimeout   = errors.New("store: synchronous write not confirmed within its time limit")
	errStopped   = errors.New("store: stopped")
)

// Store takes the modules' rows and writes them to the database. One writer
// goroutine (Run) owns the connection's write side: rows are queued by Record
// and WriteSync and sent in the order they were queued, which is what lets a
// state table keep the last write without a version column.
type Store struct {
	// bound by New
	cfg     Config
	run     RunID
	timings timings
	pool    *pgxpool.Pool

	// inboxes read by the writer
	queue   chan queued
	closing chan struct{} // closed by Close: the writer writes what is queued and returns

	// written by the callers of Record and WriteSync, read by anyone
	dropped atomic.Uint64 // rows dropped because the queue was full
	seq     atomic.Int64  // the last number given to a decision or event row

	// written by the writer, read by anyone
	failing  atomic.Bool   // the last write did not reach the database or was not answered
	retrying atomic.Bool   // a batch of recorded rows failed and is being retried
	rejected atomic.Uint64 // recorded rows the database refused, which were dropped

	// lifecycle
	started   atomic.Bool   // the queue has its consumer: Run, or Close when Run never started
	done      chan struct{} // closed when that consumer has finished
	closeOnce sync.Once

	// owned by the writer
	failedAt      time.Time // when the current failure began
	failLoggedAt  time.Time // when the current failure was last logged
	droppedLogged uint64    // the dropped count at the last warning
}

// statement is one row ready to send: its table, its INSERT and the values.
type statement struct {
	table string
	sql   string
	args  []any
}

// queued is one unit of the writer's queue: a recorded row, or a WriteSync
// request.
type queued struct {
	statement
	sync *syncRequest // nil for a recorded row
}

// syncRequest is a WriteSync waiting for the writer. state settles the race
// between the caller giving up and the writer taking the request: exactly one
// of them wins, so a request its caller gave up on while it was still queued
// is never written. A request the writer has taken is finished by the writer
// alone; the caller stops waiting at deadline either way.
type syncRequest struct {
	rows     []statement
	deadline time.Time // the caller returns by then; the writer starts no transaction for these rows after it
	state    atomic.Int32
	done     chan error // buffered, so the writer's answer never blocks it, also when nobody waits any more
}

const (
	syncWaiting   int32 = iota // queued, not yet seen by the writer
	syncTaken                  // the writer has it and will answer on done
	syncAbandoned              // the caller's time ended while it was queued; the writer skips it
)

// New connects to the database named by dsn as the writer role, applies
// schema.sql and returns a store ready for Run. A database that cannot be
// reached refuses the start. The dsn is a secret: nothing of it (user,
// password, host, port, database) is logged or put in an error.
func New(cfg Config, run RunID, dsn string) (*Store, error) {
	pc, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("store: the dsn cannot be parsed")
	}
	return open(cfg, run, pc, defaultTimings)
}

// open is New on a parsed pool configuration; tests pass their own timings
// and dial function.
func open(cfg Config, run RunID, pc *pgxpool.Config, t timings) (*Store, error) {
	ctx, cancel := context.WithTimeout(context.Background(), openTimeout)
	defer cancel()
	pc.MaxConns = poolSize
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("store: connection pool: %s", reason(err))
	}
	// The pool connects on first use: ping, so that an unreachable database
	// is reported as that and not as a schema problem.
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: cannot reach the database: %s", reason(err))
	}
	s := newStore(cfg, run, pool, t)
	if err := s.applySchema(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: schema: %s", reason(err))
	}
	logger.GlobalLogger.Info("store: open", "run_id", run, "live", cfg.Live)
	return s, nil
}

// newStore builds the store around pool, which a test of the queue alone
// leaves nil.
func newStore(cfg Config, run RunID, pool *pgxpool.Pool, t timings) *Store {
	return &Store{cfg: cfg, run: run, timings: t, pool: pool,
		queue: make(chan queued, t.queueSize), closing: make(chan struct{}), done: make(chan struct{})}
}

// applySchema runs schema.sql in one transaction under an advisory lock.
func (s *Store) applySchema(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", schemaLockKey); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, schemaSQL); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// --- recording ---

// Record queues rows for the writer and returns at once. When the queue is
// full the rows are dropped and counted; a row the store cannot encode (a
// programming error) is dropped with an Error log.
func (s *Store) Record(rows ...Row) {
	for _, row := range rows {
		st, err := s.statement(row)
		if err != nil {
			logger.GlobalLogger.Error("store: row dropped", "err", err)
			continue
		}
		select {
		case s.queue <- queued{statement: st}:
		default:
			s.dropped.Add(1)
		}
	}
}

// WriteSync returns nil once rows are committed. They go through the writer's
// queue like recorded rows and are committed in one transaction after
// everything queued before them, so a state row written here is never
// overwritten by an older one still waiting. It returns at the latest when
// the write timeout ends, whatever the writer is doing.
//
// An error means the rows are not confirmed written, and they are never
// retried. They are certainly not written when the queue was full, when a
// batch was being retried (the call fails at once and does not wait) or when
// the store stopped. When the timeout ended the request may still have been
// queued (then it is withdrawn) or already with the writer, which goes on by
// itself: it may have committed the rows without the answer arriving in
// time, or may commit them just after. A transaction the writer reports as
// failed is rolled back.
//
// So a caller that gets an error must not assume the rows are absent. Having
// decided not to act on them, it records a row of the same key that voids
// them (a cycle closed as store_error, a lot void, an order not_sent). That row
// follows in the same queue and is the last write whichever way the first
// went.
func (s *Store) WriteSync(rows ...Row) error {
	if s.retrying.Load() {
		return errUnhealthy
	}
	req := &syncRequest{deadline: time.Now().Add(s.timings.writeTimeout), done: make(chan error, 1)}
	for _, row := range rows {
		st, err := s.statement(row)
		if err != nil {
			return err
		}
		req.rows = append(req.rows, st)
	}
	select {
	case s.queue <- queued{sync: req}:
	default:
		return errQueueFull
	}
	timer := time.NewTimer(time.Until(req.deadline))
	defer timer.Stop()
	select {
	case err := <-req.done:
		return err
	case <-timer.C:
		// Withdrawn when still queued; otherwise the writer has it and
		// finishes on its own, and its answer is dropped.
		req.state.CompareAndSwap(syncWaiting, syncAbandoned)
		return errTimeout
	}
}

// Healthy is false while the last write did not reach the database or was
// not answered in time, whether a batch of recorded rows (which the writer
// retries) or a WriteSync. It turns true again with the next write the
// database answers. A row the database refuses for its content does not make
// the store unhealthy.
func (s *Store) Healthy() bool { return !s.failing.Load() }

// Dropped is the number of rows dropped so far because the queue was full.
func (s *Store) Dropped() uint64 { return s.dropped.Load() }

// Rejected is the number of recorded rows the database has refused so far.
func (s *Store) Rejected() uint64 { return s.rejected.Load() }

// statement encodes row for its table and stamps the run, the mode and, for
// a numbered table, the next sequence number.
func (s *Store) statement(row Row) (statement, error) {
	p, err := planFor(row)
	if err != nil {
		return statement{}, err
	}
	var seq int64
	if p.seq {
		seq = s.seq.Add(1)
	}
	return statement{table: p.table, sql: p.insert, args: p.args(row, s.run, s.cfg.Live, seq)}, nil
}

// --- the writer ---

// Run is the writer: it sends the queued rows in batches, every flush
// interval or as soon as a batch is full, and at once for a WriteSync. It
// returns when ctx ends or Close is called, after one last attempt to write
// what is still queued. It does nothing when Close was called first.
func (s *Store) Run(ctx context.Context) {
	if !s.started.CompareAndSwap(false, true) {
		return
	}
	defer close(s.done)
	log := logger.GlobalLogger
	log.Info("store: running", "queue_size", s.timings.queueSize, "batch_rows", s.timings.batchRows, "flush_every", s.timings.flushEvery)
	defer func() {
		log.Info("store: stopped", "dropped_total", s.dropped.Load(), "rejected_total", s.rejected.Load())
	}()

	flush := time.NewTicker(s.timings.flushEvery)
	defer flush.Stop()
	var batch []statement
	for {
		var req *syncRequest
		select {
		case <-ctx.Done():
			s.drain(batch)
			return
		case <-s.closing:
			s.drain(batch)
			return
		case q := <-s.queue:
			switch {
			case q.sync == nil:
				batch = append(batch, q.statement)
				if len(batch) < s.timings.batchRows {
					continue
				}
			case q.sync.state.CompareAndSwap(syncWaiting, syncTaken):
				req = q.sync
			default: // abandoned by its caller
				continue
			}
		case <-flush.C:
			s.warnDropped()
		}
		batch = s.flush(ctx, batch, req)
	}
}

// flush writes batch, with req's rows after it in the same transaction, and
// returns the recorded rows still unwritten, which is none unless the store
// is stopping.
//
// A synchronous request is answered after one attempt and its rows are never
// retried; its caller may have stopped waiting by then, which changes nothing
// here. Recorded rows are retried every retry interval for as long as the
// database cannot be reached; meanwhile WriteSync fails at once, and the
// queue fills and then drops. Rows the database refuses for their content
// are dropped, so that they do not hold up the rows behind them.
func (s *Store) flush(ctx context.Context, batch []statement, req *syncRequest) []statement {
	defer s.retrying.Store(false)
	if req != nil {
		expired := false
		err := s.write(ctx, append(batch[:len(batch):len(batch)], req.rows...), req.deadline)
		switch {
		case err == nil:
			batch = batch[:0]
		case isRejection(err) && len(batch) > 0:
			// A row was refused, and it may be a recorded one. Those are
			// sorted out one by one; then the synchronous rows are tried on
			// their own, unless their caller's time has ended meanwhile.
			if batch, err = s.writeEach(ctx, batch); err == nil {
				if expired = !time.Now().Before(req.deadline); !expired {
					err = s.write(ctx, req.rows, req.deadline)
				}
			}
		}
		switch {
		case expired:
			logger.GlobalLogger.Warn("store: synchronous rows not written; their time limit ended while the rows before them were written", "rows", len(req.rows), "tables", tablesOf(req.rows))
			err = errTimeout
		default:
			s.noteWrite(err)
			if err != nil {
				logger.GlobalLogger.Error("store: synchronous write failed; its rows are not retried", "rows", len(req.rows), "tables", tablesOf(req.rows), "reason", reason(err))
				err = fmt.Errorf("store: synchronous write failed: %s", reason(err))
			}
		}
		req.done <- err
	}
	for len(batch) > 0 {
		err := s.write(ctx, batch, time.Now().Add(s.timings.writeTimeout))
		switch {
		case err == nil:
			batch = batch[:0]
		case isRejection(err):
			batch, err = s.writeEach(ctx, batch)
		}
		if err == nil {
			s.retrying.Store(false) // before Healthy turns true, so a WriteSync that sees it healthy is taken
		}
		s.noteWrite(err)
		if err == nil {
			break
		}
		s.logFailing(err, len(batch))
		s.retrying.Store(true)
		if !s.waitRetry(ctx) {
			break
		}
	}
	return batch
}

// writeEach writes rows one transaction each, after the database refused them
// as one batch, and drops every row it refuses again. It returns the rows
// still unwritten and the error that stopped it, which is nil unless the
// database could not be reached.
func (s *Store) writeEach(ctx context.Context, rows []statement) ([]statement, error) {
	var refused []string // distinct "table: reason"
	dropped := 0
	defer func() {
		if dropped > 0 {
			logger.GlobalLogger.Error("store: rows rejected by the database and dropped", "rows", dropped, "rejected", strings.Join(refused, "; "), "rejected_total", s.rejected.Add(uint64(dropped)))
		}
	}()
	for i, r := range rows {
		err := s.write(ctx, rows[i:i+1], time.Now().Add(s.timings.writeTimeout))
		switch {
		case err == nil:
		case isRejection(err):
			dropped++
			if what := r.table + ": " + reason(err); !slices.Contains(refused, what) {
				refused = append(refused, what)
			}
		default:
			return rows[i:], err
		}
	}
	return rows[:0], nil
}

// waitRetry waits one retry interval; false means the store is stopping.
func (s *Store) waitRetry(ctx context.Context) bool {
	timer := time.NewTimer(s.timings.retryEvery)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-s.closing:
		return false
	case <-timer.C:
		return true
	}
}

// write sends rows as one pipelined batch in one transaction that must
// commit by deadline. Every statement is idempotent, so a batch whose commit
// outcome was lost is safe to send again.
func (s *Store) write(ctx context.Context, rows []statement, deadline time.Time) error {
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	b := &pgx.Batch{}
	for _, r := range rows {
		b.Queue(r.sql, r.args...)
	}
	if err := tx.SendBatch(ctx, b).Close(); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// noteWrite records the outcome of a write for Healthy: the database
// answered (it committed, or it refused a row), or it did not.
func (s *Store) noteWrite(err error) {
	if err == nil || isRejection(err) {
		if s.failing.CompareAndSwap(true, false) {
			logger.GlobalLogger.Info("store: write recovered", "failed_for_ms", time.Since(s.failedAt).Milliseconds())
			s.failLoggedAt = time.Time{}
		}
		return
	}
	if s.failing.CompareAndSwap(false, true) {
		s.failedAt = time.Now()
	}
}

// logFailing reports a batch that cannot be written: at once when the
// failure begins, then once every failLogEvery while it lasts.
func (s *Store) logFailing(err error, rows int) {
	now := time.Now()
	if now.Sub(s.failLoggedAt) < s.timings.failLogEvery {
		return
	}
	s.failLoggedAt = now
	logger.GlobalLogger.Error("store: write failing; the batch is kept and retried", "rows", rows, "reason", reason(err),
		"failing_for_ms", now.Sub(s.failedAt).Milliseconds(), "retry_every", s.timings.retryEvery, "queued", len(s.queue), "dropped_total", s.dropped.Load())
}

// warnDropped logs once per flush interval while rows are being dropped.
func (s *Store) warnDropped() {
	if total := s.dropped.Load(); total != s.droppedLogged {
		logger.GlobalLogger.Warn("store: queue full; rows dropped", "dropped", total-s.droppedLogged, "dropped_total", total)
		s.droppedLogged = total
	}
}

// drain is the last step of the queue's consumer: everything still queued
// joins batch and is written once. Waiting WriteSync calls are answered with
// errStopped.
func (s *Store) drain(batch []statement) {
	for more := true; more; {
		select {
		case q := <-s.queue:
			if q.sync == nil {
				batch = append(batch, q.statement)
			} else if q.sync.state.CompareAndSwap(syncWaiting, syncTaken) {
				q.sync.done <- errStopped
			}
		default:
			more = false
		}
	}
	if len(batch) == 0 {
		return
	}
	err := s.write(context.Background(), batch, time.Now().Add(s.timings.writeTimeout))
	if isRejection(err) {
		batch, err = s.writeEach(context.Background(), batch)
	}
	if err != nil {
		logger.GlobalLogger.Error("store: rows lost at stop", "rows", len(batch), "reason", reason(err))
	}
}

// Close writes what is still queued, once, and closes the connections. With
// the writer running it stops the writer, which does that last write; when
// Run was never started, Close does it itself.
func (s *Store) Close() {
	s.closeOnce.Do(func() {
		close(s.closing)
		if s.started.CompareAndSwap(false, true) {
			s.drain(nil)
			close(s.done)
		}
		<-s.done
		if s.pool != nil {
			s.pool.Close()
		}
	})
}

// --- errors ---

// isRejection reports whether the database refused a statement for what it
// holds or names: a data exception (SQLSTATE class 22), a constraint
// violation (23), or a syntax, name or privilege error (42). Sending the same
// row again cannot succeed, so such a row is dropped instead of retried.
func isRejection(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || len(pgErr.Code) < 2 {
		return false
	}
	switch pgErr.Code[:2] {
	case "22", "23", "42":
		return true
	}
	return false
}

// reason describes a database error for a log line or a returned error
// without anything of the connection string (pgx's own text for a failed
// connection carries the user, the database, the host and the port) and
// without row values (the server's text for refused data can quote them). A
// server error is its SQLSTATE; a name or syntax error (class 42) outside the
// connection phase also keeps the server's message, which names the table and
// column concerned.
func reason(err error) string {
	var pgErr *pgconn.PgError
	var connErr *pgconn.ConnectError
	var netErr net.Error
	connecting := errors.As(err, &connErr)
	switch {
	case errors.As(err, &pgErr) && !connecting && strings.HasPrefix(pgErr.Code, "42"):
		return "SQLSTATE " + pgErr.Code + ": " + pgErr.Message
	case errors.As(err, &pgErr):
		return "SQLSTATE " + pgErr.Code
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case connecting:
		return "cannot connect"
	}
	return "connection error"
}

// tablesOf lists the distinct tables of rows, in order of first appearance.
func tablesOf(rows []statement) string {
	var tables []string
	for _, r := range rows {
		if !slices.Contains(tables, r.table) {
			tables = append(tables, r.table)
		}
	}
	return strings.Join(tables, ",")
}

// --- reading back ---

// LoadState reads what a restarted process continues from, of the store's
// own mode: the unfinished cycle (status open or takeover, whatever its
// session) with its lots and orders, the inventory state, the minute bars
// and anchor facts of sessionID, and the last lot that was built. main calls
// it before any manager runs.
func (s *Store) LoadState(ctx context.Context, sessionID string) (State, error) {
	var st State
	live := s.cfg.Live

	// The unfinished cycle. The ledger keeps at most one; more is a broken
	// record that a person has to look at.
	cycles, err := load[CycleRow](ctx, s.pool, "live = $1 AND status IN ('open', 'takeover') ORDER BY opened_ms", live)
	if err != nil {
		return st, err
	}
	if len(cycles) > 1 {
		return st, fmt.Errorf("store: %d unfinished cycles (live=%t), at most one is expected", len(cycles), live)
	}
	if len(cycles) == 1 {
		st.Cycle = &cycles[0]
		if st.Lots, err = load[LotRow](ctx, s.pool, "live = $1 AND cycle_id = $2 ORDER BY seq", live, st.Cycle.CycleID); err != nil {
			return st, err
		}
		if st.Orders, err = load[OrderRow](ctx, s.pool, "live = $1 AND cycle_id = $2 ORDER BY submitted_ms, client_order_id", live, st.Cycle.CycleID); err != nil {
			return st, err
		}
	}

	// What the ledger carries between cycles.
	inv, err := load[InventoryStateRow](ctx, s.pool, "live = $1", live)
	if err != nil {
		return st, err
	}
	if len(inv) == 1 {
		st.Inventory = &inv[0]
	}

	// The session's own minutes and anchor facts.
	if st.MinuteBars, err = load[MinuteBarRow](ctx, s.pool, "live = $1 AND session_id = $2 ORDER BY minute_ms", live, sessionID); err != nil {
		return st, err
	}
	if st.AnchorFacts, err = load[AnchorFactRow](ctx, s.pool, "live = $1 AND session_id = $2 ORDER BY known_ms, kind, leg", live, sessionID); err != nil {
		return st, err
	}

	// The last built lot, for the spacing of the next entry.
	last, err := load[LotRow](ctx, s.pool, "live = $1 AND status IN ('open', 'closed') ORDER BY opened_ms DESC LIMIT 1", live)
	if err != nil {
		return st, err
	}
	if len(last) == 1 {
		st.LastEntry = &last[0]
	}
	return st, nil
}

// load reads the rows of T's table that match where, a SQL condition with
// its ORDER BY and $n arguments.
func load[T Row](ctx context.Context, pool *pgxpool.Pool, where string, args ...any) ([]T, error) {
	var zero T
	p, err := planFor(zero)
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, "SELECT "+p.selectList+" FROM trade."+p.table+" WHERE "+where, args...)
	if err != nil {
		return nil, fmt.Errorf("store: read %s: %s", p.table, reason(err))
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			return nil, fmt.Errorf("store: read %s: %s", p.table, reason(err))
		}
		var item T
		if err := p.fill(reflect.ValueOf(&item).Elem(), values); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read %s: %s", p.table, reason(err))
	}
	return out, nil
}

// --- row encoding ---

// colKind is the Go type of a row field, which decides how its column is
// written and read.
type colKind int

const (
	colString      colKind = iota // TEXT
	colBool                       // BOOLEAN
	colInt                        // BIGINT or INTEGER
	colFloat                      // DOUBLE PRECISION; not finite <-> NULL
	colDecimal                    // NUMERIC, sent and read as decimal text, so the value is exact
	colNullDecimal                // NUMERIC that may be NULL: decimal.NullDecimal, not Valid <-> NULL
	colJSON                       // JSONB, sent and read as text; empty <-> NULL
)

// column maps one struct field to its column.
type column struct {
	name     string
	field    int
	kind     colKind
	nullZero bool // `omitempty`: the zero value is written as NULL
}

// rowPlan is how one row type is written and read, built once per type.
type rowPlan struct {
	table      string
	seq        bool
	cols       []column
	insert     string // the INSERT with the table's conflict rule
	selectList string // the columns for reading, in cols order
}

var (
	plans       sync.Map // reflect.Type -> *rowPlan
	decimalType = reflect.TypeFor[decimal.Decimal]()
	nullDecType = reflect.TypeFor[decimal.NullDecimal]()
	jsonType    = reflect.TypeFor[json.RawMessage]()
)

// planFor returns the plan of row's type, building it on first use.
func planFor(row Row) (*rowPlan, error) {
	t := reflect.TypeOf(row)
	if p, ok := plans.Load(t); ok {
		return p.(*rowPlan), nil
	}
	p, err := buildPlan(t, row.Table())
	if err != nil {
		return nil, err
	}
	plans.Store(t, p)
	return p, nil
}

// buildPlan derives the columns from t's json tags and composes the INSERT
// for the table's kind.
func buildPlan(t reflect.Type, table string) (*rowPlan, error) {
	spec, ok := tables[table]
	if !ok {
		return nil, fmt.Errorf("store: unknown table %q", table)
	}
	if t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("store: a %s row must be a struct value, got %s", table, t)
	}
	p := &rowPlan{table: table, seq: spec.seq}

	// The columns the store adds, then the struct's own.
	names := []string{"run_id", "live"}
	if spec.seq {
		names = append(names, "seq")
	}
	var selects []string
	for i := range t.NumField() {
		f := t.Field(i)
		name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
		c := column{name: name, field: i, nullZero: opts == "omitempty"}
		sel := name
		switch {
		case f.Type == decimalType:
			c.kind, sel = colDecimal, name+"::text"
		case f.Type == nullDecType:
			c.kind, sel = colNullDecimal, name+"::text"
		case f.Type == jsonType:
			c.kind, sel = colJSON, name+"::text"
		case f.Type.Kind() == reflect.String:
			c.kind = colString
		case f.Type.Kind() == reflect.Bool:
			c.kind = colBool
		case f.Type.Kind() == reflect.Int, f.Type.Kind() == reflect.Int64:
			c.kind = colInt
		case f.Type.Kind() == reflect.Float64:
			c.kind = colFloat
		default:
			return nil, fmt.Errorf("store: %s.%s has unsupported type %s", table, name, f.Type)
		}
		if name == "" {
			return nil, fmt.Errorf("store: %s field %s has no json tag", table, f.Name)
		}
		p.cols = append(p.cols, c)
		names = append(names, name)
		selects = append(selects, sel)
	}
	p.selectList = strings.Join(selects, ", ")

	// The conflict rule: a state row replaces every column outside the key;
	// a fact or a sample already there is left as it is.
	isKey := map[string]bool{}
	for _, k := range spec.key {
		isKey[k] = true
	}
	var holders, updates []string
	found := 0
	for i, n := range names {
		holders = append(holders, fmt.Sprintf("$%d", i+1))
		if isKey[n] {
			found++
		} else {
			updates = append(updates, n+" = EXCLUDED."+n)
		}
	}
	if found != len(spec.key) {
		return nil, fmt.Errorf("store: %s row lacks a key column of %v", table, spec.key)
	}
	conflict := "DO NOTHING"
	if spec.kind == kindState {
		conflict = "DO UPDATE SET " + strings.Join(updates, ", ")
	}
	p.insert = fmt.Sprintf("INSERT INTO trade.%s (%s) VALUES (%s) ON CONFLICT (%s) %s",
		table, strings.Join(names, ", "), strings.Join(holders, ", "), strings.Join(spec.key, ", "), conflict)
	return p, nil
}

// args returns row's values in the INSERT's order, with NULL (nil) where the
// column's rule says so.
func (p *rowPlan) args(row Row, run RunID, live bool, seq int64) []any {
	v := reflect.ValueOf(row)
	args := make([]any, 0, len(p.cols)+3)
	args = append(args, string(run), live)
	if p.seq {
		args = append(args, seq)
	}
	for _, c := range p.cols {
		f := v.Field(c.field)
		var arg any
		switch c.kind {
		case colString:
			if s := f.String(); s != "" || !c.nullZero {
				arg = s
			}
		case colBool:
			arg = f.Bool()
		case colInt:
			if n := f.Int(); n != 0 || !c.nullZero {
				arg = n
			}
		case colFloat:
			if x := f.Float(); !math.IsNaN(x) && !math.IsInf(x, 0) {
				arg = x
			}
		case colDecimal:
			if d := f.Interface().(decimal.Decimal); !d.IsZero() || !c.nullZero {
				arg = d.String()
			}
		case colNullDecimal:
			if d := f.Interface().(decimal.NullDecimal); d.Valid {
				arg = d.Decimal.String()
			}
		case colJSON:
			if b := f.Bytes(); len(b) > 0 {
				arg = string(b)
			}
		}
		args = append(args, arg)
	}
	return args
}

// fill sets the fields of v, a row struct, from values read with selectList.
// NULL leaves the zero value (for a NullDecimal that is "not valid"), except
// a float64, which becomes NaN.
func (p *rowPlan) fill(v reflect.Value, values []any) error {
	for i, c := range p.cols {
		f, val := v.Field(c.field), values[i]
		if val == nil {
			if c.kind == colFloat {
				f.SetFloat(math.NaN())
			}
			continue
		}
		ok := true
		switch c.kind {
		case colString:
			var s string
			if s, ok = val.(string); ok {
				f.SetString(s)
			}
		case colBool:
			var b bool
			if b, ok = val.(bool); ok {
				f.SetBool(b)
			}
		case colInt:
			switch n := val.(type) {
			case int64:
				f.SetInt(n)
			case int32:
				f.SetInt(int64(n))
			default:
				ok = false
			}
		case colFloat:
			var x float64
			if x, ok = val.(float64); ok {
				f.SetFloat(x)
			}
		case colDecimal, colNullDecimal:
			var s string
			if s, ok = val.(string); ok {
				d, err := decimal.NewFromString(s)
				if err != nil {
					return fmt.Errorf("store: read %s.%s: %w", p.table, c.name, err)
				}
				if c.kind == colDecimal {
					f.Set(reflect.ValueOf(d))
				} else {
					f.Set(reflect.ValueOf(decimal.NullDecimal{Decimal: d, Valid: true}))
				}
			}
		case colJSON:
			var s string
			if s, ok = val.(string); ok {
				f.SetBytes([]byte(s))
			}
		}
		if !ok {
			return fmt.Errorf("store: read %s.%s: unexpected database type %T", p.table, c.name, val)
		}
	}
	return nil
}

// rollback ends tx if it is still open; after a commit it does nothing.
func rollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}
