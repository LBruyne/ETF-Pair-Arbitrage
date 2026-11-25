package booktape

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Manager runs the feeds and the database writer of the per-second rows.
type Manager struct {
	feeds  []*feed
	rows   *writer
	output string
	log    *slog.Logger
}

// NewManager reads the venues' price ticks and builds the feeds of the
// configured contracts and the database writer. It does not connect to the
// database: the writer applies the schema before its first write, so the
// recording starts while the database is down.
func NewManager(ctx context.Context, cfg Config, dsn, output string, log *slog.Logger) (*Manager, error) {
	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("the DSN cannot be parsed") // the parser's message may quote it
	}
	feeds, err := feedsOf(ctx, cfg.Combos, venueEndpoints, log)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, err
	}
	return newManager(feeds, pool, output, log), nil
}

// newManager binds the feeds to the output, the logger and one writer. A
// contract's rows of one kind come from one stream, the feed's first of that
// kind in subscription order; a later one (Bitget books5 after books50) is
// kept in the files only.
func newManager(feeds []*feed, pool *pgxpool.Pool, output string, log *slog.Logger) *Manager {
	w := &writer{pool: pool, log: log, pending: map[rowKey]row{}}
	for _, f := range feeds {
		f.output, f.log, f.rows = output, log, w
		f.tapes = map[string]*tape{}
		for _, stream := range f.streams {
			s := &tape{}
			symbol, topic, _ := strings.Cut(stream, ".")
			if k := rowKind(f.venue, topic); k != kindNone && !f.tracks(symbol, k) {
				s.tracker = &tracker{venue: f.venue, symbol: symbol, kind: k}
			}
			f.tapes[stream] = s
		}
	}
	return &Manager{feeds: feeds, rows: w, output: output, log: log}
}

// tracks reports whether a stream of the feed already derives the
// contract's rows of kind k.
func (f *feed) tracks(symbol string, k kind) bool {
	for _, s := range f.tapes {
		if s.tracker != nil && s.tracker.symbol == symbol && s.tracker.kind == k {
			return true
		}
	}
	return false
}

// Run records until ctx ends or a file cannot be written (the error). At the
// end every feed closes its connection and its files, and the rows still
// queued are written once more; rows that last write cannot store are
// dropped with an Error log.
func (m *Manager) Run(ctx context.Context) error {
	defer m.rows.pool.Close()
	var names, contracts []string
	for _, f := range m.feeds {
		names = append(names, f.name)
		for _, stream := range f.streams {
			if c := f.venue + ":" + strings.Split(stream, ".")[0]; !slices.Contains(contracts, c) {
				contracts = append(contracts, c)
			}
		}
	}
	m.log.Info("booktape: recording", "feeds", strings.Join(names, ","), "contracts", strings.Join(contracts, ","), "output", m.output)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		m.rows.run(ctx)
	}()
	var wg sync.WaitGroup
	errs := make(chan error, len(m.feeds))
	for _, f := range m.feeds {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := f.run(ctx); err != nil {
				m.log.Error("booktape: a file cannot be written; stopping", "feed", f.name, "err", err)
				errs <- err
				cancel()
			}
		}()
	}
	wg.Wait()
	<-writerDone
	final, stop := context.WithTimeout(context.Background(), writeWait)
	defer stop()
	if err := m.rows.write(final, time.Now()); err != nil {
		m.rows.drop(math.MaxInt64, err, "booktape: rows dropped at shutdown, the last write failed; rebuild them from the raw files with dashboard/backfill/perp_second_recordings.py")
	}
	select {
	case err := <-errs:
		return err
	default:
		return nil
	}
}

// --- feeds ---

var dialer = websocket.Dialer{Proxy: http.ProxyFromEnvironment, HandshakeTimeout: writeWait}

// run dials the feed again whenever its connection ends, until ctx ends. It
// returns an error only for a file that cannot be written.
func (f *feed) run(ctx context.Context) (err error) {
	defer func() {
		if cerr := f.closeFiles(); err == nil {
			err = cerr
		}
	}()
	flush, status := time.NewTicker(flushEvery), time.NewTicker(statusEvery)
	defer flush.Stop()
	defer status.Stop()
	for {
		conn, _, dialErr := dialer.DialContext(ctx, f.url, nil)
		wait := redialIdle
		if dialErr == nil {
			cause, err := f.serve(ctx, conn, flush.C, status.C)
			if err != nil || cause == shutdown {
				return err
			}
			if f.connFrames > 0 {
				wait = redialAfter
			}
			f.log.Warn("booktape: disconnected", "feed", f.name, "err", cause, "frames", f.connFrames, "redial_in", wait)
		} else if ctx.Err() == nil {
			f.log.Warn("booktape: dial failed", "feed", f.name, "err", dialErr, "redial_in", wait)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}

// serve records one connection until it ends and returns why it ended
// ("shutdown" when ctx ended), and an error for a file that cannot be
// written. It subscribes, then reads on one goroutine and writes the
// heartbeats on another, while the frames are processed here.
func (f *feed) serve(ctx context.Context, conn *websocket.Conn, flush, status <-chan time.Time) (string, error) {
	startMs := time.Now().UnixMilli()
	if err := f.beginConnection(startMs); err != nil {
		conn.Close()
		return "", err
	}
	var cause error
	if f.request != nil {
		conn.SetWriteDeadline(time.Now().Add(writeWait))
		cause = conn.WriteMessage(websocket.TextMessage, f.request)
	}
	in := make(chan frame, 4096)
	var endMs int64
	var readErr error
	go func() {
		defer close(in)
		endMs, readErr = read(conn, in)
	}()
	stop, stopped, failed := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		defer close(stopped)
		f.keepAlive(conn, stop, failed)
	}()
	if cause != nil {
		conn.Close()
	}

	done := ctx.Done()
	var fileErr error
	for open := true; open; {
		select {
		case fr, ok := <-in:
			if !ok {
				open = false
			} else if fileErr == nil {
				if fileErr = f.process(fr); fileErr != nil {
					conn.Close()
				}
			}
		case <-flush:
			if fileErr == nil {
				if fileErr = f.flushFiles(); fileErr != nil {
					conn.Close()
				}
			}
		case <-status:
			f.status()
		case <-done:
			done = nil
			cause = errors.New(shutdown)
			conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
			conn.Close()
		}
	}
	close(stop)
	<-stopped
	if cause == nil {
		select {
		case cause = <-failed:
		default:
			cause = readErr
		}
	}
	if fileErr != nil {
		return "", fileErr
	}
	return cause.Error(), f.endConnection(endMs, cause.Error())
}

// read forwards every frame of conn with its local receive time until a read
// fails or times out, and returns when that happened and why. Anything that
// arrives (a frame, a ping, a pong) pushes the read deadline forward.
func read(conn *websocket.Conn, in chan<- frame) (int64, error) {
	extend := func() { conn.SetReadDeadline(time.Now().Add(readTimeout)) }
	extend()
	answer := conn.PingHandler()
	conn.SetPingHandler(func(data string) error { extend(); return answer(data) })
	conn.SetPongHandler(func(string) error { extend(); return nil })
	for {
		typ, payload, err := conn.ReadMessage()
		receivedMs := time.Now().UnixMilli()
		if err != nil {
			return receivedMs, err
		}
		extend()
		in <- frame{receivedMs: receivedMs, binary: typ == websocket.BinaryMessage, payload: payload}
	}
}

// keepAlive sends the heartbeat every pingEvery until stop is closed. A
// failed write closes the connection and is reported on failed.
func (f *feed) keepAlive(conn *websocket.Conn, stop <-chan struct{}, failed chan<- error) {
	ping := time.NewTicker(pingEvery)
	defer ping.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ping.C:
			if err := f.heartbeat(conn); err != nil {
				failed <- err
				conn.Close()
				return
			}
		}
	}
}

// heartbeat writes the text heartbeat (Bitget) or a protocol ping (Binance,
// where any text that is not JSON closes the connection).
func (f *feed) heartbeat(conn *websocket.Conn) error {
	if f.ping == nil {
		return conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeWait))
	}
	conn.SetWriteDeadline(time.Now().Add(writeWait))
	return conn.WriteMessage(websocket.TextMessage, f.ping)
}

// beginConnection writes the connected event and starts the connection's
// counts; the per-second state is already reset.
func (f *feed) beginConnection(startMs int64) error {
	request := []byte("null")
	if f.request != nil {
		request = f.request
	}
	f.connFrames, f.warned = 0, false
	for _, s := range f.tapes {
		s.seen = false
	}
	f.log.Info("booktape: connected", "feed", f.name, "streams", len(f.streams))
	return appendEvent(f.output, f.venue, startMs, connectedEvent{eventHead{startMs, f.name, "connected"}, f.url, request})
}

// endConnection makes the pending seconds before endMs final with the
// current values, forgets the per-second state (nothing is carried across a
// disconnection), writes the disconnected event and flushes the files.
func (f *feed) endConnection(endMs int64, cause string) error {
	for _, s := range f.tapes {
		if s.tracker != nil {
			f.out = s.tracker.finalize(endMs, f.out)
			s.tracker.reset()
		}
	}
	f.queueRows()
	if err := appendEvent(f.output, f.venue, endMs, disconnectedEvent{eventHead{endMs, f.name, "disconnected"}, cause, f.connFrames}); err != nil {
		return err
	}
	return f.flushFiles()
}

// process stores one frame and updates the per-second rows with it.
func (f *feed) process(fr frame) error {
	if fr.binary {
		return f.unreadable(fr)
	}
	r, err := route(f.venue, fr.payload)
	switch {
	case r.pong:
		return nil
	case r.stream == "" && err != nil:
		return f.unreadable(fr)
	}
	s := f.tapes[r.stream]
	if s == nil { // a control frame, or a stream the feed did not subscribe
		if r.refused {
			f.log.Error("booktape: the venue refused a request", "feed", f.name, "frame", string(fr.payload))
		}
		return appendEvent(f.output, f.venue, fr.receivedMs, controlEvent{eventHead{fr.receivedMs, f.name, "control"}, fr.payload})
	}
	f.connFrames++
	f.frames++
	s.seen = true
	if err := f.record(r.stream, s, fr.receivedMs, r.payload); err != nil {
		return err
	}
	// The clock, rebuilt from receive times, passed T + 2 s for every second
	// T < receivedMs - 2 s. Every data frame of the feed advances it.
	for _, other := range f.tapes {
		if other.tracker != nil {
			f.out = other.tracker.finalize(fr.receivedMs-finalAfterMs, f.out)
		}
	}
	if s.tracker != nil {
		if err != nil && !f.warned {
			f.warned = true
			f.log.Warn("booktape: the per-second fields of a frame cannot be read", "feed", f.name, "stream", r.stream, "err", err)
		}
		var late int
		f.out, late = s.tracker.observe(fr.receivedMs, r.events, f.out)
		f.late += late
	}
	f.queueRows()
	return nil
}

// unreadable records a frame that is neither JSON nor a heartbeat answer;
// it is logged once per connection.
func (f *feed) unreadable(fr frame) error {
	text := string(fr.payload[:min(len(fr.payload), unreadableN)])
	if !f.warned {
		f.warned = true
		f.log.Warn("booktape: unreadable frame", "feed", f.name, "binary", fr.binary, "text", text)
	}
	return appendEvent(f.output, f.venue, fr.receivedMs, unreadableEvent{eventHead{fr.receivedMs, f.name, "unreadable"}, text})
}

func (f *feed) queueRows() {
	if len(f.out) > 0 {
		f.rows.queue(f.out)
		f.queued += len(f.out)
		f.out = f.out[:0]
	}
}

// status logs the feed's counts since the last status line and the streams
// that have delivered no frame on the current connection: a wrong Binance
// stream name is accepted silently and never pushes.
func (f *feed) status() {
	var silent []string
	for _, stream := range f.streams {
		if !f.tapes[stream].seen {
			silent = append(silent, stream)
		}
	}
	f.log.Info("booktape: status", "feed", f.name, "frames", f.frames, "bytes", f.bytes,
		"silent_streams", strings.Join(silent, ","), "late_events", f.late, "rows_queued", f.queued)
	f.frames, f.bytes, f.late, f.queued = 0, 0, 0, 0
}

// --- per-second rows ---

// tracker derives one kind of the per-second rows of one contract on the
// current connection: the newest event by exchange time (ties: the larger
// trade id, or the later book event) and the next second to emit. A second
// T is emitted once, with the value then current, when an event stamped at
// or after T arrives, when the clock passes T + 2 s, or when the connection
// ends. Owned by its feed's goroutine.
type tracker struct {
	venue, symbol string
	kind          kind
	value         *event // nil before the first event on the connection
	next          int64  // the next second to emit; 0 while the kind is unknown
	emitted       int64  // the newest second emitted; 0 for none
}

// observe applies one frame's events received at receivedMs and appends the
// rows they make final to out. The kind is known from the receive time of
// its first frame on the connection, so its first second is the next whole
// second. It also returns how many events were late: stamped before a
// second already emitted, they update the value but no emitted row.
func (t *tracker) observe(receivedMs int64, events []event, out []row) ([]row, int) {
	if t.next == 0 {
		t.next = (receivedMs + secondMs - 1) / secondMs * secondMs
	}
	late := 0
	for _, e := range events {
		out = t.finalize(e.exchangeMs+1, out)
		if t.emitted != 0 && e.exchangeMs < t.emitted {
			late++
		}
		if t.value == nil || e.exchangeMs > t.value.exchangeMs || e.exchangeMs == t.value.exchangeMs && e.id >= t.value.id {
			v := e
			t.value = &v
		}
	}
	return out, late
}

// finalize emits every pending second before `before`. A second without a
// value yet is passed with no row: its columns stay NULL.
func (t *tracker) finalize(before int64, out []row) []row {
	for t.next != 0 && t.next < before {
		if t.value != nil {
			out = append(out, row{venue: t.venue, symbol: t.symbol, secondMs: t.next, kind: t.kind, event: *t.value})
		}
		t.emitted = t.next
		t.next += secondMs
	}
	return out
}

func (t *tracker) reset() {
	t.value, t.next, t.emitted = nil, 0, 0
}

// --- database writer ---

// writer stores the per-second rows. The feeds queue them; its goroutine
// writes them every writeEvery and keeps what a failed write could not
// store, until those rows are an hour old.
type writer struct {
	pool *pgxpool.Pool
	log  *slog.Logger

	mu     sync.Mutex
	queued []row // emitted since the last write; guarded by mu

	// Owned by the writer goroutine (and Run's final write after it).
	pending       map[rowKey]row
	schemaApplied bool
	failing       bool // the last write failed
	written       int  // since the last status line
	dropped       int  // since the last status line
}

// queue takes a copy of rows; it never waits for the database.
func (w *writer) queue(rows []row) {
	w.mu.Lock()
	w.queued = append(w.queued, rows...)
	w.mu.Unlock()
}

// run writes the queued rows every writeEvery until ctx ends.
func (w *writer) run(ctx context.Context) {
	tick, status := time.NewTicker(writeEvery), time.NewTicker(statusEvery)
	defer tick.Stop()
	defer status.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			w.write(ctx, now)
		case <-status.C:
			w.log.Info("booktape: database", "rows_written", w.written, "rows_pending", len(w.pending), "rows_dropped", w.dropped)
			w.written, w.dropped = 0, 0
		}
	}
}

// write stores the queued rows and those kept from failed writes, and
// returns the error of a failed write. After a failure the rows stay
// pending; those whose second is more than an hour before now are dropped
// (the raw files still hold them).
func (w *writer) write(ctx context.Context, now time.Time) error {
	w.mu.Lock()
	queued := w.queued
	w.queued = nil
	w.mu.Unlock()
	for _, r := range queued {
		w.pending[r.key()] = r
	}
	if len(w.pending) == 0 {
		return nil
	}
	err := w.storeRows(ctx, w.pending)
	if err == nil {
		if w.failing {
			w.log.Info("booktape: database writes resumed", "rows", len(w.pending))
		}
		w.failing = false
		w.written += len(w.pending)
		clear(w.pending)
		return nil
	}
	if !w.failing {
		w.log.Warn("booktape: database write failed; rows kept for the next attempt", "err", err, "rows", len(w.pending))
	}
	w.failing = true
	w.drop(now.UnixMilli()-holdRowsMs, err, "booktape: rows dropped after an hour of failed writes; rebuild them from the raw files with dashboard/backfill/perp_second_recordings.py")
	return err
}

// drop forgets the pending rows whose second is before cutoff and logs them
// at Error with their count and seconds, and the error of the failed write.
func (w *writer) drop(cutoff int64, err error, msg string) {
	var n int
	var first, last int64
	for k := range w.pending {
		if k.secondMs < cutoff {
			delete(w.pending, k)
			if n == 0 || k.secondMs < first {
				first = k.secondMs
			}
			last = max(last, k.secondMs)
			n++
		}
	}
	if n > 0 {
		w.dropped += n
		w.log.Error(msg, "rows", n, "from", time.UnixMilli(first), "to", time.UnixMilli(last), "err", err)
	}
}
