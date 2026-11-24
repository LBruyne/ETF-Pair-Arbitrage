package booktape

import (
	"bufio"
	"compress/gzip"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

//go:embed schema.sql
var schemaSQL string

// --- raw files ---

// tapeFile is the open data file of one stream: one gzip member of lines
// {"received_ms":<int>,"payload":<frame verbatim>} received within one
// UTC+8 hour.
type tapeFile struct {
	file  *os.File
	buf   *bufio.Writer
	gz    *gzip.Writer
	hour  int64 // received_ms / hourMs of its first record
	dirty bool  // written since the last flush
}

// record appends one frame to its stream's file. A file never spans an
// hour: the first record of another hour closes it and begins the next one,
// named by that record's receive time.
func (f *feed) record(stream string, s *tape, receivedMs int64, payload []byte) error {
	if s.file != nil && s.file.hour != receivedMs/hourMs {
		err := s.file.close()
		s.file = nil
		if err != nil {
			return err
		}
	}
	if s.file == nil {
		file, err := createTape(f.output, f.venue, stream, receivedMs)
		if err != nil {
			return err
		}
		s.file = file
	}
	f.line = append(f.line[:0], `{"received_ms":`...)
	f.line = strconv.AppendInt(f.line, receivedMs, 10)
	f.line = append(f.line, `,"payload":`...)
	f.line = append(f.line, payload...)
	f.line = append(f.line, "}\n"...)
	f.bytes += len(f.line)
	s.file.dirty = true
	_, err := s.file.gz.Write(f.line)
	return err
}

// createTape creates <output>/<venue>/<YYYYMMDD>/<stream>.<firstMs>.jsonl.gz,
// the day being the UTC+8 day of firstMs. An existing file is an error.
func createTape(output, venue, stream string, firstMs int64) (*tapeFile, error) {
	dir := dayDir(output, venue, firstMs)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(dir, fmt.Sprintf("%s.%d.jsonl.gz", stream, firstMs)), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, err
	}
	buf := bufio.NewWriterSize(file, 64<<10)
	return &tapeFile{file: file, buf: buf, gz: gzip.NewWriter(buf), hour: firstMs / hourMs}, nil
}

// flush makes what was written readable (a gzip sync point); no fsync.
func (t *tapeFile) flush() error {
	if !t.dirty {
		return nil
	}
	t.dirty = false
	if err := t.gz.Flush(); err != nil {
		return err
	}
	return t.buf.Flush()
}

// close writes the gzip trailer and closes the file.
func (t *tapeFile) close() error {
	err := t.gz.Close()
	if err == nil {
		err = t.buf.Flush()
	}
	if cerr := t.file.Close(); err == nil {
		err = cerr
	}
	return err
}

// flushFiles flushes every open data file of the feed.
func (f *feed) flushFiles() error {
	for _, s := range f.tapes {
		if s.file != nil {
			if err := s.file.flush(); err != nil {
				return err
			}
		}
	}
	return nil
}

// closeFiles closes every open data file of the feed; the first error is returned.
func (f *feed) closeFiles() error {
	var first error
	for _, s := range f.tapes {
		if s.file != nil {
			if err := s.file.close(); err != nil && first == nil {
				first = err
			}
			s.file = nil
		}
	}
	return first
}

// appendEvent appends one line to <output>/<venue>/<YYYYMMDD>/events.jsonl,
// the day of receivedMs. Feeds of one venue share the file: each line is one
// write to a file opened for appending.
func appendEvent(output, venue string, receivedMs int64, event any) error {
	line, err := json.Marshal(event)
	if err != nil {
		return err
	}
	dir := dayDir(output, venue, receivedMs)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(dir, "events.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	_, err = file.Write(append(line, '\n'))
	if cerr := file.Close(); err == nil {
		err = cerr
	}
	return err
}

func dayDir(output, venue string, ms int64) string {
	return filepath.Join(output, venue, time.UnixMilli(ms).In(utc8).Format("20060102"))
}

// --- database rows ---

// Each upsert sets only its kind's columns, so the book and the trade part of
// a row are written independently. Prices and sizes travel as the decimal
// strings the venue sent, a book column as the array literal of its levels
// (arrayLiteral); an empty price is NULL.
const (
	upsertBooks = `INSERT INTO etfpair.perp_second (venue, symbol, second_ms, bid_px, bid_sz, ask_px, ask_sz, book_ms)
		SELECT v, s, t, bp::numeric[], bz::numeric[], ap::numeric[], az::numeric[], m
		FROM unnest($1::text[], $2::text[], $3::bigint[], $4::text[], $5::text[], $6::text[], $7::text[], $8::bigint[]) AS r(v, s, t, bp, bz, ap, az, m)
		ON CONFLICT (venue, symbol, second_ms) DO UPDATE SET bid_px = EXCLUDED.bid_px, bid_sz = EXCLUDED.bid_sz,
		ask_px = EXCLUDED.ask_px, ask_sz = EXCLUDED.ask_sz, book_ms = EXCLUDED.book_ms`
	upsertTrades = `INSERT INTO etfpair.perp_second (venue, symbol, second_ms, price, trade_ms)
		SELECT v, s, t, NULLIF(p, '')::numeric, m
		FROM unnest($1::text[], $2::text[], $3::bigint[], $4::text[], $5::bigint[]) AS r(v, s, t, p, m)
		ON CONFLICT (venue, symbol, second_ms) DO UPDATE SET price = EXCLUDED.price, trade_ms = EXCLUDED.trade_ms`
)

// storeRows applies the schema until it has been applied once, then upserts
// the rows in one batch, which PostgreSQL runs as one implicit transaction:
// either every row is stored or none.
func (w *writer) storeRows(ctx context.Context, rows map[rowKey]row) error {
	if !w.schemaApplied {
		if _, err := w.pool.Exec(ctx, schemaSQL, pgx.QueryExecModeSimpleProtocol); err != nil {
			return fmt.Errorf("schema: %w", err)
		}
		w.schemaApplied = true
	}
	var b struct {
		venue, symbol, bidPx, bidSz, askPx, askSz []string
		second, bookMs                            []int64
	}
	var t struct {
		venue, symbol, price []string
		second, tradeMs      []int64
	}
	for _, r := range rows {
		e := r.event
		if r.kind == kindBook {
			b.venue, b.symbol, b.second = append(b.venue, r.venue), append(b.symbol, r.symbol), append(b.second, r.secondMs)
			b.bidPx, b.bidSz = append(b.bidPx, arrayLiteral(e.bids, 0)), append(b.bidSz, arrayLiteral(e.bids, 1))
			b.askPx, b.askSz = append(b.askPx, arrayLiteral(e.asks, 0)), append(b.askSz, arrayLiteral(e.asks, 1))
			b.bookMs = append(b.bookMs, e.exchangeMs)
		} else {
			t.venue, t.symbol, t.second = append(t.venue, r.venue), append(t.symbol, r.symbol), append(t.second, r.secondMs)
			t.price, t.tradeMs = append(t.price, e.price), append(t.tradeMs, e.exchangeMs)
		}
	}
	batch := &pgx.Batch{}
	if len(b.venue) > 0 {
		batch.Queue(upsertBooks, b.venue, b.symbol, b.second, b.bidPx, b.bidSz, b.askPx, b.askSz, b.bookMs)
	}
	if len(t.venue) > 0 {
		batch.Queue(upsertTrades, t.venue, t.symbol, t.second, t.price, t.tradeMs)
	}
	return w.pool.SendBatch(ctx, batch).Close()
}

// arrayLiteral is the PostgreSQL array literal of one field of a book side
// (0 the prices, 1 the sizes), best level first: {54.15000,54.14000}. An
// empty side is {} and an empty field NULL.
func arrayLiteral(levels []level, field int) string {
	var b strings.Builder
	b.WriteByte('{')
	for i, l := range levels {
		if i > 0 {
			b.WriteByte(',')
		}
		if l[field] == "" {
			b.WriteString("NULL")
		} else {
			b.WriteString(l[field])
		}
	}
	b.WriteByte('}')
	return b.String()
}
