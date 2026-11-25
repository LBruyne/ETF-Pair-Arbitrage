package cex

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// lv is one book level.
func lv(price, qty string) BookLevel { return BookLevel{Price: dec(price), Qty: dec(qty)} }

// t0 is the arrival time of a test's first frame.
var t0 = time.Date(2026, 11, 24, 3, 0, 0, 0, time.UTC)

// depthFrame is a depth frame arriving at t0 + offset.
func depthFrame(action BookAction, seq, prev uint64, bids, asks []BookLevel, offset time.Duration) BookEvent {
	return BookEvent{Symbol: "SOXLUSDT", Topic: TopicBooks5, Action: action, Bids: bids, Asks: asks, Seq: seq, PrevSeq: prev, ReceivedAt: t0.Add(offset)}
}

// bestFrame is a books1 frame arriving at t0 + offset.
func bestFrame(seq uint64, bid, ask BookLevel, offset time.Duration) BookEvent {
	return BookEvent{Symbol: "SOXLUSDT", Topic: TopicBooks1, Action: ActionSnapshot, Bids: []BookLevel{bid}, Asks: []BookLevel{ask}, Seq: seq, ReceivedAt: t0.Add(offset)}
}

// wantLevels fails unless got holds exactly the given levels in order.
func wantLevels(t *testing.T, side string, got []BookLevel, want ...BookLevel) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d levels %v, want %d %v", side, len(got), got, len(want), want)
	}
	for i := range want {
		if !got[i].Price.Equal(want[i].Price) || !got[i].Qty.Equal(want[i].Qty) {
			t.Errorf("%s level %d = %s x %s, want %s x %s", side, i, got[i].Price, got[i].Qty, want[i].Price, want[i].Qty)
		}
	}
}

// A stream of snapshots: each replaces the whole book; a frame that is not
// newer than the depth held is dropped whatever its arrival time.
func TestBookSnapshotsReplaceTheBook(t *testing.T) {
	var b legBook
	if b.applyDepth(depthFrame(ActionSnapshot, 100, 0, []BookLevel{lv("40.45", "10"), lv("40.44", "20")}, []BookLevel{lv("40.46", "5"), lv("40.47", "6")}, 0)) {
		t.Fatal("a snapshot asked for a resubscribe")
	}
	b.applyDepth(depthFrame(ActionSnapshot, 105, 0, []BookLevel{lv("40.43", "7")}, []BookLevel{lv("40.48", "8")}, time.Second))
	book := b.publish("SOXLUSDT", true)
	wantLevels(t, "bids", book.Bids, lv("40.43", "7"))
	wantLevels(t, "asks", book.Asks, lv("40.48", "8"))
	if book.DepthSeq != 105 || !book.DepthReceivedAt.Equal(t0.Add(time.Second)) || book.Conflict || !book.Connected {
		t.Errorf("book = seq %d at %s conflict %t connected %t", book.DepthSeq, book.DepthReceivedAt, book.Conflict, book.Connected)
	}
	if !book.BestBid.Price.Equal(dec("40.43")) || !book.BestAsk.Price.Equal(dec("40.48")) || !book.BestReceivedAt.Equal(t0.Add(time.Second)) {
		t.Errorf("best = %s / %s at %s, want the depth's", book.BestBid.Price, book.BestAsk.Price, book.BestReceivedAt)
	}

	// Older and equal sequence numbers arrive later and change nothing: not
	// the levels and not the time the depth counts as received.
	for _, seq := range []uint64{104, 105} {
		if b.applyDepth(depthFrame(ActionSnapshot, seq, 0, []BookLevel{lv("1", "1")}, []BookLevel{lv("2", "1")}, 2*time.Second)) {
			t.Fatalf("a stale snapshot (seq %d) asked for a resubscribe", seq)
		}
	}
	book = b.publish("SOXLUSDT", true)
	wantLevels(t, "bids", book.Bids, lv("40.43", "7"))
	if book.DepthSeq != 105 || !book.DepthReceivedAt.Equal(t0.Add(time.Second)) {
		t.Errorf("after stale frames: seq %d received %s, want 105 and the time of frame 105", book.DepthSeq, book.DepthReceivedAt)
	}
}

// Updates that chain are merged: a level sets its price's quantity, a new
// price is inserted in order, quantity zero deletes the price.
func TestBookChainedUpdatesMerge(t *testing.T) {
	var b legBook
	b.applyDepth(depthFrame(ActionSnapshot, 100, 0,
		[]BookLevel{lv("40.45", "10"), lv("40.44", "20"), lv("40.42", "30")},
		[]BookLevel{lv("40.46", "5"), lv("40.47", "6"), lv("40.49", "7")}, 0))
	steps := []struct {
		ev         BookEvent
		bids, asks []BookLevel
	}{
		{ // a changed quantity, a new price between two levels, on each side
			depthFrame(ActionUpdate, 101, 100, []BookLevel{lv("40.44", "25"), lv("40.43", "3")}, []BookLevel{lv("40.48", "9"), lv("40.46", "4")}, time.Second),
			[]BookLevel{lv("40.45", "10"), lv("40.44", "25"), lv("40.43", "3"), lv("40.42", "30")},
			[]BookLevel{lv("40.46", "4"), lv("40.47", "6"), lv("40.48", "9"), lv("40.49", "7")},
		},
		{ // the best levels deleted, a new best ask, a price deleted that is not there
			depthFrame(ActionUpdate, 102, 101, []BookLevel{lv("40.45", "0"), lv("40.30", "0")}, []BookLevel{lv("40.46", "0"), lv("40.455", "1")}, 2*time.Second),
			[]BookLevel{lv("40.44", "25"), lv("40.43", "3"), lv("40.42", "30")},
			[]BookLevel{lv("40.455", "1"), lv("40.47", "6"), lv("40.48", "9"), lv("40.49", "7")},
		},
		{ // new levels at both ends of each side
			depthFrame(ActionUpdate, 103, 102, []BookLevel{lv("40.445", "2"), lv("40.40", "8")}, []BookLevel{lv("40.50", "11")}, 3*time.Second),
			[]BookLevel{lv("40.445", "2"), lv("40.44", "25"), lv("40.43", "3"), lv("40.42", "30"), lv("40.40", "8")},
			[]BookLevel{lv("40.455", "1"), lv("40.47", "6"), lv("40.48", "9"), lv("40.49", "7"), lv("40.50", "11")},
		},
	}
	for _, s := range steps {
		if b.applyDepth(s.ev) {
			t.Fatalf("update %d, which chains, asked for a resubscribe", s.ev.Seq)
		}
		book := b.publish("SOXLUSDT", true)
		wantLevels(t, "bids after "+strconv.FormatUint(s.ev.Seq, 10), book.Bids, s.bids...)
		wantLevels(t, "asks after "+strconv.FormatUint(s.ev.Seq, 10), book.Asks, s.asks...)
		if book.DepthSeq != s.ev.Seq || !book.DepthReceivedAt.Equal(s.ev.ReceivedAt) {
			t.Errorf("after update %d: seq %d received %s", s.ev.Seq, book.DepthSeq, book.DepthReceivedAt)
		}
	}
	// The published levels are the reader's own: a later update does not
	// reach into a book already published.
	published := b.publish("SOXLUSDT", true)
	b.applyDepth(depthFrame(ActionUpdate, 104, 103, []BookLevel{lv("40.445", "99")}, nil, 4*time.Second))
	wantLevels(t, "the earlier publication", published.Bids[:1], lv("40.445", "2"))
}

// An update that does not chain drops the depth and asks for one
// resubscribe; later updates are ignored until the next snapshot.
func TestBookBrokenChainDropsTheDepth(t *testing.T) {
	var b legBook
	b.applyDepth(depthFrame(ActionSnapshot, 100, 0, []BookLevel{lv("40.45", "10")}, []BookLevel{lv("40.46", "5")}, 0))
	if !b.applyDepth(depthFrame(ActionUpdate, 105, 103, []BookLevel{lv("40.45", "9")}, nil, time.Second)) {
		t.Fatal("an update whose previous seq is not the depth's did not ask for a resubscribe")
	}
	book := b.publish("SOXLUSDT", true)
	if len(book.Bids) != 0 || len(book.Asks) != 0 || book.DepthSeq != 0 || !book.DepthReceivedAt.IsZero() {
		t.Errorf("after the broken chain the book still has a depth: %+v", book)
	}
	// Further updates, even ones that would chain on the dropped frame, are
	// ignored and ask for nothing more.
	for _, ev := range []BookEvent{depthFrame(ActionUpdate, 106, 105, []BookLevel{lv("40.45", "8")}, nil, 2*time.Second), depthFrame(ActionUpdate, 107, 100, nil, nil, 2*time.Second)} {
		if b.applyDepth(ev) {
			t.Errorf("update %d while waiting for the snapshot asked for another resubscribe", ev.Seq)
		}
	}
	if b.depth != nil {
		t.Fatal("an update rebuilt the depth without a snapshot")
	}
	// The snapshot brings the depth back.
	b.applyDepth(depthFrame(ActionSnapshot, 110, 0, []BookLevel{lv("40.44", "4")}, []BookLevel{lv("40.47", "5")}, 3*time.Second))
	book = b.publish("SOXLUSDT", true)
	wantLevels(t, "bids", book.Bids, lv("40.44", "4"))
	if book.DepthSeq != 110 {
		t.Errorf("depth seq = %d, want 110", book.DepthSeq)
	}
}

// books1 newer than the depth at the same prices only refreshes the first
// level's quantities. With a different best price it is a conflict, which
// its return to the depth's price does not end: only a depth at least as new
// as the disagreeing frame does (SPEC 5.1, example 1).
func TestBookBestPriceAwayAndBack(t *testing.T) {
	var b legBook
	b.applyDepth(depthFrame(ActionSnapshot, 100, 0, []BookLevel{lv("40.45", "10"), lv("40.44", "20")}, []BookLevel{lv("40.46", "5"), lv("40.47", "6")}, 0))

	// Same prices, newer quantities.
	b.applyBest(bestFrame(101, lv("40.45", "3"), lv("40.46", "2"), time.Second))
	book := b.publish("SOXLUSDT", true)
	if book.Conflict {
		t.Fatal("books1 at the depth's prices is a conflict")
	}
	wantLevels(t, "bids", book.Bids, lv("40.45", "3"), lv("40.44", "20"))
	wantLevels(t, "asks", book.Asks, lv("40.46", "2"), lv("40.47", "6"))
	if !book.BestReceivedAt.Equal(t0.Add(time.Second)) || !book.DepthReceivedAt.Equal(t0) {
		t.Errorf("times: best %s depth %s; books1 must not refresh the depth's time", book.BestReceivedAt, book.DepthReceivedAt)
	}

	// The best bid moves away: conflict. The best price follows books1, the
	// depth's levels stay as the depth frame gave them.
	b.applyBest(bestFrame(102, lv("40.44", "15"), lv("40.46", "2"), 2*time.Second))
	book = b.publish("SOXLUSDT", true)
	if !book.Conflict || !book.BestBid.Price.Equal(dec("40.44")) || !book.BestBid.Qty.Equal(dec("15")) {
		t.Fatalf("after books1 102: conflict %t best bid %s x %s, want a conflict and 40.44 x 15", book.Conflict, book.BestBid.Price, book.BestBid.Qty)
	}
	wantLevels(t, "bids during the conflict", book.Bids, lv("40.45", "10"), lv("40.44", "20"))

	// It moves back: still a conflict.
	b.applyBest(bestFrame(103, lv("40.45", "1"), lv("40.46", "2"), 3*time.Second))
	book = b.publish("SOXLUSDT", true)
	if !book.Conflict || !book.BestBid.Price.Equal(dec("40.45")) {
		t.Fatalf("after books1 103 returned to 40.45: conflict %t best bid %s, want the conflict to stay", book.Conflict, book.BestBid.Price)
	}
	wantLevels(t, "bids during the conflict", book.Bids[:1], lv("40.45", "10"))

	// An older books1 frame is dropped.
	b.applyBest(bestFrame(102, lv("40.40", "1"), lv("40.60", "1"), 4*time.Second))
	if got := b.publish("SOXLUSDT", true).BestBid.Price; !got.Equal(dec("40.45")) {
		t.Errorf("an older books1 frame replaced the best bid: %s", got)
	}

	// A depth older than the disagreeing frame (102) does not end it; one at
	// least as new does.
	b.applyDepth(depthFrame(ActionSnapshot, 101, 0, []BookLevel{lv("40.45", "10")}, []BookLevel{lv("40.46", "5")}, 5*time.Second))
	if !b.publish("SOXLUSDT", true).Conflict {
		t.Fatal("a depth with seq 101 ended a conflict of seq 102")
	}
	b.applyDepth(depthFrame(ActionSnapshot, 102, 0, []BookLevel{lv("40.44", "15"), lv("40.43", "9")}, []BookLevel{lv("40.46", "2")}, 6*time.Second))
	book = b.publish("SOXLUSDT", true)
	// Depth 102 is as new as the disagreeing frame, but books1 103 is newer
	// still and shows 40.45 against the depth's 40.44: a new conflict.
	if !book.Conflict {
		t.Fatal("depth 102 against books1 103 at another price must be a conflict")
	}
	b.applyDepth(depthFrame(ActionSnapshot, 104, 0, []BookLevel{lv("40.45", "6"), lv("40.44", "15")}, []BookLevel{lv("40.46", "2")}, 7*time.Second))
	book = b.publish("SOXLUSDT", true)
	if book.Conflict || !book.BestBid.Qty.Equal(dec("6")) || !book.BestReceivedAt.Equal(t0.Add(7*time.Second)) {
		t.Errorf("after depth 104: conflict %t best bid qty %s at %s, want no conflict and the depth's best", book.Conflict, book.BestBid.Qty, book.BestReceivedAt)
	}
}

// A depth that arrives after a newer books1 frame and shows another best
// price is a conflict from the moment it is accepted, until a depth at least
// as new as that books1 frame (SPEC 5.1, example 2).
func TestBookLateDepthConflicts(t *testing.T) {
	var b legBook
	b.applyBest(bestFrame(105, lv("40.45", "4"), lv("40.47", "2"), 0))
	book := b.publish("SOXLUSDT", true)
	if book.Conflict || len(book.Bids) != 0 || !book.BestBid.Price.Equal(dec("40.45")) {
		t.Fatalf("books1 alone: conflict %t, %d bids, best bid %s; want a price and no depth", book.Conflict, len(book.Bids), book.BestBid.Price)
	}
	b.applyDepth(depthFrame(ActionSnapshot, 103, 0, []BookLevel{lv("40.46", "9")}, []BookLevel{lv("40.47", "3")}, time.Second))
	book = b.publish("SOXLUSDT", true)
	if !book.Conflict || !book.BestBid.Price.Equal(dec("40.45")) {
		t.Fatalf("late depth 103 with bid 40.46 against books1 105 with 40.45: conflict %t best bid %s", book.Conflict, book.BestBid.Price)
	}
	wantLevels(t, "bids", book.Bids, lv("40.46", "9"))
	b.applyDepth(depthFrame(ActionSnapshot, 104, 0, []BookLevel{lv("40.46", "8")}, []BookLevel{lv("40.47", "3")}, 2*time.Second))
	if !b.publish("SOXLUSDT", true).Conflict {
		t.Fatal("depth 104 ended a conflict of seq 105")
	}
	b.applyDepth(depthFrame(ActionSnapshot, 105, 0, []BookLevel{lv("40.45", "4")}, []BookLevel{lv("40.47", "2")}, 3*time.Second))
	if b.publish("SOXLUSDT", true).Conflict {
		t.Fatal("depth 105 did not end the conflict of seq 105")
	}
}

// A lost connection drops the depth; the best price stays until it ages out.
func TestBookDisconnectKeepsTheBestPrice(t *testing.T) {
	var b legBook
	b.applyDepth(depthFrame(ActionSnapshot, 100, 0, []BookLevel{lv("40.45", "10")}, []BookLevel{lv("40.46", "5")}, 0))
	b.applyBest(bestFrame(101, lv("40.45", "3"), lv("40.46", "2"), time.Second))
	b.dropDepth()
	book := b.publish("SOXLUSDT", false)
	if book.Connected || len(book.Bids) != 0 || !book.DepthReceivedAt.IsZero() {
		t.Errorf("after the disconnect the book still has a depth: %+v", book)
	}
	if !book.BestBid.Price.Equal(dec("40.45")) || !book.BestReceivedAt.Equal(t0.Add(time.Second)) {
		t.Errorf("best = %s at %s, want books1's", book.BestBid.Price, book.BestReceivedAt)
	}
}

// A depth that was newer than books1 leaves its first level behind as the
// best price when it is dropped, with its own sequence number and arrival
// time: by a lost connection and by an update that does not chain alike.
func TestBookDroppedDepthLeavesItsBestPrice(t *testing.T) {
	for name, drop := range map[string]func(b *legBook){
		"lost connection": func(b *legBook) { b.dropDepth() },
		"broken chain": func(b *legBook) {
			if !b.applyDepth(depthFrame(ActionUpdate, 300, 250, []BookLevel{lv("40.40", "1")}, nil, 13*time.Second)) {
				t.Fatal("the update that does not chain was taken")
			}
		},
	} {
		var b legBook
		b.applyBest(bestFrame(100, lv("40.45", "3"), lv("40.46", "2"), 0))
		b.applyDepth(depthFrame(ActionSnapshot, 200, 0, []BookLevel{lv("40.44", "10"), lv("40.43", "20")}, []BookLevel{lv("40.47", "5")}, 12*time.Second))
		drop(&b)
		book := b.publish("SOXLUSDT", false)
		if len(book.Bids) != 0 || book.DepthSeq != 0 || !book.DepthReceivedAt.IsZero() {
			t.Errorf("%s: the depth is still published: %+v", name, book)
		}
		if !book.BestBid.Price.Equal(dec("40.44")) || !book.BestBid.Qty.Equal(dec("10")) || !book.BestAsk.Price.Equal(dec("40.47")) || !book.BestReceivedAt.Equal(t0.Add(12*time.Second)) {
			t.Errorf("%s: best = %s x %s / %s at %s, want the dropped depth's first level and arrival time", name, book.BestBid.Price, book.BestBid.Qty, book.BestAsk.Price, book.BestReceivedAt)
		}
		// A books1 frame older than the dropped depth does not take the
		// price back; a newer one does.
		b.applyBest(bestFrame(150, lv("40.40", "1"), lv("40.50", "1"), 14*time.Second))
		if got := b.publish("SOXLUSDT", false).BestBid.Price; !got.Equal(dec("40.44")) {
			t.Errorf("%s: a books1 frame older than the dropped depth replaced the best bid: %s", name, got)
		}
		b.applyBest(bestFrame(201, lv("40.43", "2"), lv("40.47", "5"), 15*time.Second))
		if got := b.publish("SOXLUSDT", false).BestBid.Price; !got.Equal(dec("40.43")) {
			t.Errorf("%s: a newer books1 frame did not become the best bid: %s", name, got)
		}
		// The next snapshot is a depth like any other: no conflict with the
		// price left behind.
		b.applyDepth(depthFrame(ActionSnapshot, 310, 0, []BookLevel{lv("40.42", "7")}, []BookLevel{lv("40.48", "5")}, 16*time.Second))
		if book := b.publish("SOXLUSDT", true); book.Conflict || !book.BestBid.Price.Equal(dec("40.42")) {
			t.Errorf("%s: after the new snapshot: conflict %t, best bid %s", name, book.Conflict, book.BestBid.Price)
		}
	}
}

// A conflict is held by the newest books1 frame that disagreed, not the
// first: two frames in a row show another price, a third returns to the
// depth's, and a depth as new as the first disagreeing frame but older than
// the second does not end the conflict.
func TestBookConflictTracksTheNewestDisagreeingFrame(t *testing.T) {
	var b legBook
	b.applyDepth(depthFrame(ActionSnapshot, 100, 0, []BookLevel{lv("40.45", "10")}, []BookLevel{lv("40.46", "5")}, 0))
	b.applyBest(bestFrame(101, lv("40.44", "4"), lv("40.46", "5"), time.Second))
	b.applyBest(bestFrame(102, lv("40.43", "4"), lv("40.46", "5"), 2*time.Second))
	b.applyBest(bestFrame(103, lv("40.45", "4"), lv("40.46", "5"), 3*time.Second))
	if !b.publish("SOXLUSDT", true).Conflict {
		t.Fatal("no conflict after books1 moved away twice and back")
	}
	// Depth 101 shows the price books1 has returned to. It covers the first
	// disagreeing frame only: what frame 102 saw is still unaccounted for.
	b.applyDepth(depthFrame(ActionSnapshot, 101, 0, []BookLevel{lv("40.45", "9")}, []BookLevel{lv("40.46", "5")}, 4*time.Second))
	if !b.publish("SOXLUSDT", true).Conflict {
		t.Fatal("a depth with seq 101 ended a conflict whose newest disagreeing books1 frame is 102")
	}
	b.applyDepth(depthFrame(ActionSnapshot, 102, 0, []BookLevel{lv("40.45", "8")}, []BookLevel{lv("40.46", "5")}, 5*time.Second))
	book := b.publish("SOXLUSDT", true)
	if book.Conflict {
		t.Fatal("a depth with seq 102 did not end the conflict")
	}
	// books1 103 is newer at the same prices: its quantity is on the first level.
	wantLevels(t, "bids", book.Bids, lv("40.45", "4"))
}

// --- recorded frames ---

// probeFrame is one line of the recorded probe file: a raw frame of
// Bitget's public stream with its local arrival time.
type probeFrame struct {
	symbol, topic string
	ev            BookEvent
}

// readProbe reads the fixture cut from the probe recording
// experiments/decisions_20261123/book_conflict/probe/: a JSON header line,
// then one line per frame, "<arrival unix ms>\t<raw frame>".
func readProbe(t *testing.T) []probeFrame {
	t.Helper()
	file, err := os.Open("testdata/bitget_probe_20261124T063314Z_head.jsonl.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	zr, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	var out []probeFrame
	sc := bufio.NewScanner(zr)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	sc.Scan() // the header
	for sc.Scan() {
		arrival, raw, _ := strings.Cut(sc.Text(), "\t")
		ms, err := strconv.ParseInt(arrival, 10, 64)
		if err != nil {
			t.Fatalf("arrival time %q", arrival)
		}
		var f struct {
			Action string `json:"action"`
			Arg    struct{ Topic, Symbol string }
			Data   []struct {
				A, B      [][]string
				Seq, Pseq uint64
			}
		}
		if err := json.Unmarshal([]byte(raw), &f); err != nil {
			t.Fatal(err)
		}
		side := func(rows [][]string) []BookLevel {
			levels := make([]BookLevel, len(rows))
			for i, r := range rows {
				levels[i] = BookLevel{Price: decimal.RequireFromString(r[0]), Qty: decimal.RequireFromString(r[1])}
			}
			return levels
		}
		d := f.Data[0]
		out = append(out, probeFrame{symbol: f.Arg.Symbol, topic: f.Arg.Topic, ev: BookEvent{Symbol: Symbol(f.Arg.Symbol), Topic: f.Arg.Topic, Action: BookAction(f.Action),
			Bids: side(d.B), Asks: side(d.A), Seq: d.Seq, PrevSeq: d.Pseq, ReceivedAt: time.UnixMilli(ms)}})
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// The first 2.5 s of a recording of the real stream, replayed in arrival
// order through one leg book per contract (SOXL books5 + books1, SOXS
// books50 + books1). The number of conflict episodes is the count an
// independent script made with the same rule when the fixture was cut.
func TestBookReplayRecordedFrames(t *testing.T) {
	frames := readProbe(t)
	depthTopic := map[string]string{"SOXLUSDT": TopicBooks5, "SOXSUSDT": TopicBooks50}
	wantEpisodes := map[string]int{"SOXLUSDT": 9, "SOXSUSDT": 28}
	wantFrames := map[string]int{"SOXLUSDT.books5": 129, "SOXLUSDT.books1": 201, "SOXSUSDT.books50": 122, "SOXSUSDT.books1": 73}

	books := map[string]*legBook{"SOXLUSDT": {}, "SOXSUSDT": {}}
	episodes, counts := map[string]int{}, map[string]int{}
	inConflict := map[string]bool{}
	lastDepthSeq := map[string]uint64{}
	for _, f := range frames {
		b := books[f.symbol]
		switch f.topic {
		case TopicBooks1:
			b.applyBest(f.ev)
		case depthTopic[f.symbol]:
			if b.applyDepth(f.ev) {
				t.Fatalf("%s %s seq %d asked for a resubscribe: the recording holds snapshots only", f.symbol, f.topic, f.ev.Seq)
			}
			lastDepthSeq[f.symbol] = max(lastDepthSeq[f.symbol], f.ev.Seq)
		default:
			continue // the full-book topic, replayed by the next test
		}
		counts[f.symbol+"."+f.topic]++
		book := b.publish(Symbol(f.symbol), true)

		// Without a conflict the published best price is the depth's first
		// level; with one, books1 is ahead of the depth and differs from it.
		if book.DepthSeq != 0 && !book.Conflict && (!book.BestBid.Price.Equal(book.Bids[0].Price) || !book.BestAsk.Price.Equal(book.Asks[0].Price)) {
			t.Fatalf("%s after seq %d: no conflict, yet best %s/%s and first levels %s/%s differ", f.symbol, f.ev.Seq, book.BestBid.Price, book.BestAsk.Price, book.Bids[0].Price, book.Asks[0].Price)
		}
		if book.Conflict && b.best.seq <= book.DepthSeq {
			t.Fatalf("%s after seq %d: conflict while the depth (seq %d) is as new as books1 (seq %d)", f.symbol, f.ev.Seq, book.DepthSeq, b.best.seq)
		}
		if book.Conflict && !inConflict[f.symbol] {
			episodes[f.symbol]++
		}
		inConflict[f.symbol] = book.Conflict
	}
	for key, want := range wantFrames {
		if counts[key] != want {
			t.Errorf("%s: %d frames replayed, want %d", key, counts[key], want)
		}
	}
	for symbol, want := range wantEpisodes {
		if episodes[symbol] != want {
			t.Errorf("%s: %d conflict episodes, want %d", symbol, episodes[symbol], want)
		}
		if got := books[symbol].depth.seq; got != lastDepthSeq[symbol] {
			t.Errorf("%s: the depth ends at seq %d, the newest depth frame is %d", symbol, got, lastDepthSeq[symbol])
		}
	}
}

// The same recording's full-book topic (`books`: one snapshot of a thousand
// levels a side, then updates) is the one real stream of chained updates
// there is. Every update follows the frame before it, so none may ask for a
// resubscribe, and the merged book must stay a book: prices strictly
// ordered, no empty level, the best bid below the best ask.
func TestBookReplayRecordedUpdateChain(t *testing.T) {
	var b legBook
	updates := 0
	for _, f := range readProbe(t) {
		if f.symbol != "SOXLUSDT" || f.topic != "books" {
			continue
		}
		if b.applyDepth(f.ev) {
			t.Fatalf("update seq %d (prev %d) asked for a resubscribe", f.ev.Seq, f.ev.PrevSeq)
		}
		if f.ev.Action == ActionUpdate {
			updates++
		}
		book := b.publish("SOXLUSDT", true)
		if book.DepthSeq != f.ev.Seq {
			t.Fatalf("frame seq %d was not taken: the depth is at %d", f.ev.Seq, book.DepthSeq)
		}
		for i, l := range book.Bids {
			if !l.Qty.IsPositive() || (i > 0 && !l.Price.LessThan(book.Bids[i-1].Price)) {
				t.Fatalf("after seq %d: bid %d = %s x %s is out of order or empty", f.ev.Seq, i, l.Price, l.Qty)
			}
		}
		for i, l := range book.Asks {
			if !l.Qty.IsPositive() || (i > 0 && !l.Price.GreaterThan(book.Asks[i-1].Price)) {
				t.Fatalf("after seq %d: ask %d = %s x %s is out of order or empty", f.ev.Seq, i, l.Price, l.Qty)
			}
		}
		if !book.Bids[0].Price.LessThan(book.Asks[0].Price) {
			t.Fatalf("after seq %d: best bid %s is not below best ask %s", f.ev.Seq, book.Bids[0].Price, book.Asks[0].Price)
		}
	}
	if updates != 50 {
		t.Errorf("%d updates replayed, want 50", updates)
	}
}
