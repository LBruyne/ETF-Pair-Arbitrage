package bitget

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/cex/internal/livetest"
)

// noSource stands in for the read-only sources a manager needs and this
// test does not exercise: every read fails.
type noSource struct{}

var errNoSource = errors.New("not part of this test")

func (noSource) IndexClose(context.Context, cex.Symbol, time.Time) (decimal.Decimal, bool, error) {
	return decimal.Decimal{}, false, errNoSource
}
func (noSource) TradingSchedule(context.Context) ([]cex.ScheduleDay, error) { return nil, errNoSource }
func (noSource) Quote(context.Context) (decimal.Decimal, decimal.Decimal, error) {
	return decimal.Decimal{}, decimal.Decimal{}, errNoSource
}
func (noSource) MinuteClose(context.Context, time.Time) (decimal.Decimal, bool, error) {
	return decimal.Decimal{}, false, errNoSource
}
func (noSource) MinuteCloses(context.Context, time.Time, time.Time) ([]cex.FXMinute, error) {
	return nil, errNoSource
}
func (noSource) NAV(context.Context, string, time.Time) (decimal.Decimal, bool, error) {
	return decimal.Decimal{}, false, cex.ErrIssuerKey
}
func (noSource) Distributions(context.Context, string) ([]cex.Distribution, error) {
	return nil, cex.ErrIssuerKey
}

// liveRecording is how long TestLiveBitgetPublic listens.
const liveRecording = 60 * time.Second

// Against the real exchange (public, unauthenticated, read-only), gated by
// BITGET_PUBLIC_TEST=1: a manager on the real client, inside the paper
// venue, runs for a minute with the example pair. Both legs must get a usable book and keep it, the rules
// and the settled funding rates must load. Every raw frame is saved under
// experiments/core_step2/ in the format of the earlier probe recordings: a
// JSON header line, then "<arrival unix ms>\t<frame>" per line. The REST
// responses (instruments, history-fund-rate) are saved beside it as
// received.
func TestLiveBitgetPublic(t *testing.T) {
	if os.Getenv("BITGET_PUBLIC_TEST") != "1" {
		t.Skip("BITGET_PUBLIC_TEST is not 1: the live read of Bitget's public stream is skipped")
	}
	cfg := cex.DefaultConfig
	cfg.Pair = cex.Pair{Bull: cex.LegConfig{Symbol: "SOXLUSDT", Ticker: "SOXL", BookTopic: cex.TopicBooks5}, Bear: cex.LegConfig{Symbol: "SOXSUSDT", Ticker: "SOXS", BookTopic: cex.TopicBooks50}}
	symbols := map[cex.Leg]cex.Symbol{cex.Bull: cfg.Pair.Bull.Symbol, cex.Bear: cfg.Pair.Bear.Symbol}

	// The recording file.
	dir := livetest.Dir
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	path := filepath.Join(dir, "bitget_public_"+started.UTC().Format("20060102T150405Z")+".jsonl.gz")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := gzip.NewWriter(file)
	header, _ := json.Marshal(map[string]any{"url": wsPublicURL, "started_ms": started.UnixMilli(), "seconds": liveRecording.Seconds(),
		"request": []string{"SOXLUSDT.books5", "SOXLUSDT.books1", "SOXSUSDT.books50", "SOXSUSDT.books1"}, "note": "one connection per contract; frames of both interleaved by arrival"})
	fmt.Fprintf(zw, "%s\n", header)

	// Every raw frame is written and counted; the gap between a contract's
	// depth frames is the age its book reaches.
	var mu sync.Mutex
	closed := false
	frames := map[string]int{}
	lastDepth, maxDepthGap := map[string]time.Time{}, map[string]time.Duration{}
	client := NewClient(cex.Credentials{})
	recorder := &livetest.Recorder{} // the REST responses, as received
	client.http.Transport = recorder
	defer func() {
		restPath, err := recorder.Save("bitget_public_rest_raw")
		if err != nil {
			t.Error(err)
		}
		t.Logf("saved %d raw REST responses to %s", len(recorder.Responses()), restPath)
	}()
	client.onRawFrame = func(raw []byte, at time.Time) {
		var f wsFrame
		json.Unmarshal(raw, &f)
		mu.Lock()
		defer mu.Unlock()
		if closed {
			return // a frame read while the connections were closing
		}
		fmt.Fprintf(zw, "%d\t%s\n", at.UnixMilli(), raw)
		if f.Action == "" {
			return
		}
		frames[f.Arg.Symbol+"."+f.Arg.Topic]++
		if f.Arg.Topic != cex.TopicBooks1 {
			if last, ok := lastDepth[f.Arg.Symbol]; ok {
				maxDepthGap[f.Arg.Symbol] = max(maxDepthGap[f.Arg.Symbol], at.Sub(last))
			}
			lastDepth[f.Arg.Symbol] = at
		}
	}

	// The client has no API key, so the manager gets it the way a paper run
	// does: inside the paper venue, which uses its public side only.
	mgr, err := cex.NewManager(cfg, cex.NewPaperVenue(client, cex.PaperConfig{Pair: cfg.Pair, Leverage: 10}), noSource{}, noSource{}, noSource{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), liveRecording)
	defer cancel()
	done := make(chan struct{})
	go func() { mgr.Run(ctx); close(done) }()

	// Sample the published books ten times a second.
	samples, usable, conflict, priced := 0, map[cex.Leg]int{}, map[cex.Leg]int{}, map[cex.Leg]int{}
	firstUsable := map[cex.Leg]time.Duration{}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
sampling:
	for {
		select {
		case <-ctx.Done():
			break sampling
		case now := <-ticker.C:
			samples++
			books := mgr.Books()
			for _, leg := range cex.Legs {
				if books.DepthUsable(leg, now) {
					usable[leg]++
					if _, seen := firstUsable[leg]; !seen {
						firstUsable[leg] = now.Sub(started)
					}
				}
				if books.HasPrice(leg, now) {
					priced[leg]++
				}
				if books.ByLeg[leg].Conflict {
					conflict[leg]++
				}
			}
		}
	}
	// The books at the end of the minute, before the streams close.
	end := time.Now()
	final := mgr.Books().ByLeg
	<-done
	mu.Lock()
	closed = true
	zw.Close()
	file.Close()
	mu.Unlock()

	for _, leg := range cex.Legs {
		symbol, book := symbols[leg], final[leg]
		depthTopic := cfg.Pair.Bull.BookTopic
		if leg == cex.Bear {
			depthTopic = cfg.Pair.Bear.BookTopic
		}
		t.Logf("%s: %d %s frames, %d books1 frames; first usable after %s; usable in %d, priced in %d, in conflict in %d of %d samples; longest gap between depth frames %s; at the end %d bids / %d asks, best %s / %s, depth age %s",
			symbol, frames[string(symbol)+"."+depthTopic], depthTopic, frames[string(symbol)+".books1"], firstUsable[leg].Round(time.Millisecond), usable[leg], priced[leg], conflict[leg], samples,
			maxDepthGap[string(symbol)].Round(time.Millisecond), len(book.Bids), len(book.Asks), book.BestBid.Price, book.BestAsk.Price, end.Sub(book.DepthReceivedAt).Round(time.Millisecond))
		if frames[string(symbol)+"."+depthTopic] == 0 || frames[string(symbol)+".books1"] == 0 {
			t.Errorf("%s: a topic delivered no frame", symbol)
		}
		if _, seen := firstUsable[leg]; !seen || usable[leg] < samples*9/10 {
			t.Errorf("%s: usable in %d of %d samples, want a usable book for at least nine tenths of the minute", symbol, usable[leg], samples)
		}
		if len(book.Bids) == 0 || len(book.Asks) == 0 || !book.BestBid.Price.LessThan(book.BestAsk.Price) {
			t.Errorf("%s: the final book is not a book: %d bids, %d asks, best %s / %s", symbol, len(book.Bids), len(book.Asks), book.BestBid.Price, book.BestAsk.Price)
		}
		rule, ok := mgr.Rule(leg)
		if !ok || !rule.Online || !rule.PriceTick.IsPositive() {
			t.Errorf("%s: rule %+v (%t), want it loaded and online", symbol, rule, ok)
		}
		t.Logf("%s: rule tick %s, step %s, min qty %s, min notional %s, online %t", symbol, rule.PriceTick, rule.QtyStep, rule.MinQty, rule.MinNotional, rule.Online)
	}
	if !mgr.TradingReady() {
		t.Error("the manager is not ready with both rules loaded")
	}

	// The settled funding rates, read once more directly: newest first, on
	// the eight-hour grid.
	restCtx, cancelRest := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelRest()
	for _, leg := range cex.Legs {
		settlements, err := client.FundingHistory(restCtx, symbols[leg], 5)
		if err != nil || len(settlements) == 0 {
			t.Errorf("%s: FundingHistory = %d settlements, %v", symbols[leg], len(settlements), err)
			continue
		}
		for i, s := range settlements {
			if s.At.UnixMilli()%(8*time.Hour).Milliseconds() != 0 || (i > 0 && !s.At.Before(settlements[i-1].At)) {
				t.Errorf("%s: settlement %d at %s is off the eight-hour grid or out of order", symbols[leg], i, s.At.UTC().Format(time.RFC3339))
			}
		}
		rate, ok := mgr.FundingRateAt(leg, settlements[0].At)
		if !ok || !rate.Equal(settlements[0].Rate) {
			t.Errorf("%s: the manager has rate %s (%t) for %s, the venue says %s", symbols[leg], rate, ok, settlements[0].At.UTC().Format(time.RFC3339), settlements[0].Rate)
		}
		t.Logf("%s: %d settlements read, the newest at %s with rate %s", symbols[leg], len(settlements), settlements[0].At.UTC().Format(time.RFC3339), settlements[0].Rate)
	}
	t.Logf("saved %s", path)
}
