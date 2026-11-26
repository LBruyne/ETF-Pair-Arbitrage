package main

import (
	"bufio"
	"compress/gzip"
	"container/heap"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
)

// The recorder's raw files (dashboard/SPEC.md appendix A):
// <tape>/bitget/<YYYYMMDD in UTC+8>/<SYMBOL>.<topic>.<first received ms>.jsonl.gz,
// one line per frame {"received_ms":..., "payload":<the frame as received>};
// a file never crosses an hour. events.jsonl in the same directory notes
// each feed's connects and disconnects. Since 2026-11-26 a pair's books
// come from a feed of three connections that keeps the copy of each
// sequence number that arrives first, as the trading program does.

// input is one thing the replay delivers at its local arrival: a book frame
// of a leg, or the loss of the recorder's feed that carries the leg's books.
type input struct {
	at   int64 // received_ms
	leg  cex.Leg
	down bool // the recorder's feed of the leg's books was lost; ev is unset
	ev   cex.BookEvent
}

// tapeSpec says what to read: the window, and per leg the depth topic and
// its first moment. The bear leg's books5 was recorded from 2026-11-25
// 18:00:07 UTC+8 on; before bearBooks5From its depth is books50 cut to its
// first 5 levels (labelled as such in the results).
type tapeSpec struct {
	dir            string // the tape's root, holding bitget/
	from, to       int64  // unix ms, [from, to)
	bull, bear     cex.Symbol
	bearBooks5From int64 // unix ms; 0: books5 only
}

// depthLevels is how many levels a depth frame keeps: books5's five, also
// when the frame is a books50 one standing in for it.
const depthLevels = 5

// stream is one recorded topic of one leg as a time-ordered sequence of
// inputs, read lazily file by file.
type stream struct {
	leg      cex.Leg
	symbol   cex.Symbol
	topic    string // the recorded topic
	depth    bool   // a depth topic: its frames go in as depth frames
	asTopic  string // the topic the frames are delivered as
	from, to int64  // the part of the window this stream covers
	files    []string
	sc       *bufio.Scanner
	closer   func()
	numbers  map[string]decimal.Decimal // parsed prices and quantities
	head     *input
	err      error
}

// openTape lists the streams of the window: per leg its depth topic and
// books1, and the recorder's disconnects.
func openTape(spec tapeSpec) ([]*stream, []input, error) {
	var streams []*stream
	add := func(leg cex.Leg, symbol cex.Symbol, topic, as string, depth bool, from, to int64) error {
		if from >= to {
			return nil
		}
		files, err := tapeFiles(spec.dir, symbol, topic, from, to)
		if err != nil {
			return err
		}
		if len(files) == 0 {
			return fmt.Errorf("replay: no %s %s files for %s – %s", symbol, topic, showMs(from), showMs(to))
		}
		streams = append(streams, &stream{leg: leg, symbol: symbol, topic: topic, depth: depth, asTopic: as, from: from, to: to, files: files, numbers: map[string]decimal.Decimal{}})
		return nil
	}
	if err := add(cex.Bull, spec.bull, cex.TopicBooks5, cex.TopicBooks5, true, spec.from, spec.to); err != nil {
		return nil, nil, err
	}
	if err := add(cex.Bull, spec.bull, cex.TopicBooks1, cex.TopicBooks1, false, spec.from, spec.to); err != nil {
		return nil, nil, err
	}
	switchAt := max(spec.from, min(spec.to, spec.bearBooks5From))
	if err := add(cex.Bear, spec.bear, cex.TopicBooks50, cex.TopicBooks5, true, spec.from, switchAt); err != nil {
		return nil, nil, err
	}
	if err := add(cex.Bear, spec.bear, cex.TopicBooks5, cex.TopicBooks5, true, switchAt, spec.to); err != nil {
		return nil, nil, err
	}
	if err := add(cex.Bear, spec.bear, cex.TopicBooks1, cex.TopicBooks1, false, spec.from, spec.to); err != nil {
		return nil, nil, err
	}
	downs, err := tapeDisconnects(spec)
	return streams, downs, err
}

// tapeFiles lists the files of a symbol's topic that may hold frames of
// [from, to), oldest first: those that start before to, from the last one
// starting at or before from on.
func tapeFiles(dir string, symbol cex.Symbol, topic string, from, to int64) ([]string, error) {
	pattern := filepath.Join(dir, "bitget", "*", fmt.Sprintf("%s.%s.*.jsonl.gz", symbol, topic))
	paths, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}
	type file struct {
		path  string
		first int64
	}
	var all []file
	for _, p := range paths {
		parts := strings.Split(filepath.Base(p), ".")
		first, err := strconv.ParseInt(parts[len(parts)-3], 10, 64)
		if err != nil {
			continue
		}
		all = append(all, file{p, first})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].first < all[j].first })
	var out []string
	for i, f := range all {
		if f.first >= to {
			break
		}
		if i+1 < len(all) && all[i+1].first <= from {
			continue // a later file starts at or before from: this one ends before it
		}
		out = append(out, f.path)
	}
	return out, nil
}

// tapeDisconnects reads the recorder's book losses of the two legs within
// the window: a leg is lost when the feed whose connected event subscribes
// its books1 disconnects. That event marks the feed's last connection
// ending; a feed of several connections notes each one's ends as
// link_disconnected, which loses nothing while another is up. Feeds without
// the legs' books (the trades', another pair's) lose nothing.
func tapeDisconnects(spec tapeSpec) ([]input, error) {
	paths, err := filepath.Glob(filepath.Join(spec.dir, "bitget", "*", "events.jsonl"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)               // day directories: oldest first, so that a feed's connected event comes before its disconnects
	carries := map[string][]cex.Leg{} // feed: the legs whose books1 its newest connected event subscribes
	var out []input
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<24)
		for sc.Scan() {
			var e struct {
				ReceivedMs int64  `json:"received_ms"`
				Feed       string `json:"feed"`
				Event      string `json:"event"`
				Request    struct {
					Args []struct {
						Topic  string `json:"topic"`
						Symbol string `json:"symbol"`
					} `json:"args"`
				} `json:"request"`
			}
			if json.Unmarshal(sc.Bytes(), &e) != nil {
				continue
			}
			switch e.Event {
			case "connected":
				var legs []cex.Leg
				for _, a := range e.Request.Args {
					switch {
					case a.Topic != cex.TopicBooks1:
					case cex.Symbol(a.Symbol) == spec.bull:
						legs = append(legs, cex.Bull)
					case cex.Symbol(a.Symbol) == spec.bear:
						legs = append(legs, cex.Bear)
					}
				}
				carries[e.Feed] = legs
			case "disconnected":
				if e.ReceivedMs < spec.from || e.ReceivedMs >= spec.to {
					continue
				}
				for _, leg := range carries[e.Feed] {
					out = append(out, input{at: e.ReceivedMs, leg: leg, down: true})
				}
			}
		}
		f.Close()
		if err := sc.Err(); err != nil {
			return nil, err
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].at < out[j].at })
	return out, nil
}

// next returns the stream's next input in its window, nil at its end.
func (s *stream) next() *input {
	for {
		if s.sc == nil {
			if len(s.files) == 0 {
				return nil
			}
			if err := s.open(s.files[0]); err != nil {
				s.err = err
				return nil
			}
			s.files = s.files[1:]
		}
		if !s.sc.Scan() {
			if err := s.sc.Err(); err != nil && !(errors.Is(err, io.ErrUnexpectedEOF) && len(s.files) == 0) {
				s.err = err // a truncated file is an error, unless it is the last one: the recorder is still writing it
				return nil
			}
			s.closer()
			s.sc = nil
			continue
		}
		in, ok, err := s.decode(s.sc.Bytes())
		if err != nil {
			s.err = err
			return nil
		}
		if !ok || in.at < s.from {
			continue
		}
		if in.at >= s.to {
			s.closer()
			s.sc, s.files = nil, nil
			return nil
		}
		return in
	}
}

func (s *stream) open(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		f.Close()
		return fmt.Errorf("replay: %s: %w", path, err)
	}
	s.sc = bufio.NewScanner(gz)
	s.sc.Buffer(make([]byte, 1<<20), 1<<24)
	s.closer = func() { gz.Close(); f.Close() }
	return nil
}

// rawFrame is one line of a raw file.
type rawFrame struct {
	ReceivedMs int64 `json:"received_ms"`
	Payload    struct {
		Action string `json:"action"`
		Arg    struct {
			Topic  string `json:"topic"`
			Symbol string `json:"symbol"`
		} `json:"arg"`
		Data []struct {
			A    [][2]string `json:"a"`
			B    [][2]string `json:"b"`
			Seq  uint64      `json:"seq"`
			Pseq uint64      `json:"pseq"`
			Ts   string      `json:"ts"`
		} `json:"data"`
	} `json:"payload"`
}

// decode turns one line into an input; ok is false for a line that is not
// a frame of the stream's symbol.
func (s *stream) decode(line []byte) (*input, bool, error) {
	var raw rawFrame
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil, false, fmt.Errorf("replay: %s %s: %w", s.symbol, s.topic, err)
	}
	if raw.Payload.Arg.Symbol != string(s.symbol) || len(raw.Payload.Data) == 0 {
		return nil, false, nil
	}
	d := raw.Payload.Data[0]
	ev := cex.BookEvent{Symbol: s.symbol, Topic: s.asTopic, Action: cex.ActionSnapshot, Seq: d.Seq, PrevSeq: d.Pseq, ReceivedAt: time.UnixMilli(raw.ReceivedMs)}
	if raw.Payload.Action == "update" {
		ev.Action = cex.ActionUpdate
	}
	if ts, err := strconv.ParseInt(d.Ts, 10, 64); err == nil {
		ev.ExchangeTime = time.UnixMilli(ts)
	}
	keep := len(d.B)
	if s.depth {
		keep = depthLevels
	}
	var err error
	if ev.Bids, err = s.levels(d.B, keep); err != nil {
		return nil, false, err
	}
	if ev.Asks, err = s.levels(d.A, keep); err != nil {
		return nil, false, err
	}
	return &input{at: raw.ReceivedMs, leg: s.leg, ev: ev}, true, nil
}

// levels parses up to keep levels of a side, sharing the parsed numbers.
func (s *stream) levels(side [][2]string, keep int) ([]cex.BookLevel, error) {
	n := min(len(side), keep)
	out := make([]cex.BookLevel, n)
	for i := range n {
		p, err := s.number(side[i][0])
		if err != nil {
			return nil, err
		}
		q, err := s.number(side[i][1])
		if err != nil {
			return nil, err
		}
		out[i] = cex.BookLevel{Price: p, Qty: q}
	}
	return out, nil
}

func (s *stream) number(text string) (decimal.Decimal, error) {
	if d, ok := s.numbers[text]; ok {
		return d, nil
	}
	d, err := decimal.NewFromString(text)
	if err != nil {
		return decimal.Decimal{}, fmt.Errorf("replay: %s %s: number %q: %w", s.symbol, s.topic, text, err)
	}
	if len(s.numbers) < 1<<20 {
		s.numbers[text] = d
	}
	return d, nil
}

// merger yields the inputs of all streams and the disconnects in arrival
// order. Frames that arrived in the same millisecond on different topics
// have no recorded order: they go by sequence number, a depth frame before
// a books1 frame of the same number, then bull before bear; a disconnect
// goes after the frames of its millisecond.
type merger struct {
	streams []*stream
	downs   []input
	h       inputHeap
}

func newMerger(streams []*stream, downs []input) *merger {
	m := &merger{streams: streams, downs: downs}
	for i, s := range streams {
		if in := s.next(); in != nil {
			heap.Push(&m.h, headOf{in, i})
		}
	}
	return m
}

// next returns the next input, nil at the end; err reports a stream that
// failed.
func (m *merger) next() (*input, error) {
	if len(m.h) == 0 || (len(m.downs) > 0 && m.downs[0].at < m.h[0].in.at) {
		for _, s := range m.streams {
			if s.err != nil {
				return nil, s.err
			}
		}
		if len(m.downs) == 0 {
			return nil, nil
		}
		in := m.downs[0]
		m.downs = m.downs[1:]
		return &in, nil
	}
	top := heap.Pop(&m.h).(headOf)
	if in := m.streams[top.stream].next(); in != nil {
		heap.Push(&m.h, headOf{in, top.stream})
	} else if err := m.streams[top.stream].err; err != nil {
		return nil, err
	}
	return top.in, nil
}

type headOf struct {
	in     *input
	stream int
}

type inputHeap []headOf

func (h inputHeap) Len() int { return len(h) }
func (h inputHeap) Less(i, j int) bool {
	a, b := h[i].in, h[j].in
	if a.at != b.at {
		return a.at < b.at
	}
	if a.ev.Seq != b.ev.Seq {
		return a.ev.Seq < b.ev.Seq
	}
	ad, bd := a.ev.Topic != cex.TopicBooks1, b.ev.Topic != cex.TopicBooks1
	if ad != bd {
		return ad
	}
	return a.leg < b.leg
}
func (h inputHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *inputHeap) Push(x any)   { *h = append(*h, x.(headOf)) }
func (h *inputHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

// showMs writes unix ms in UTC+8.
func showMs(ms int64) string { return time.UnixMilli(ms).In(utc8).Format("2006-01-02 15:04:05.000") }

var utc8 = time.FixedZone("UTC+8", 8*60*60)
