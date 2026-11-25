package strategy

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
)

// The tests use the setting of the PRD's appendix A: anchors 40 / 60 USD,
// F = 1, the mean at leg prices 40.40 / 59.42 USDT (mu = 3.3333), d_in = 2,
// d_out = -0.5, effective levels of at least 1 share, take ratio 1/2,
// quantity step 0.01, taker fee 0.65 bps, no margin. One cent on the bull
// leg moves the spread by 2.5 units, one cent on the bear leg by 1.6667.
// The expected numbers come from the appendix, its script and hand
// arithmetic, never from the code under test.

// t0 is the fixed start of every test's clock.
var t0 = time.Date(2026, 11, 20, 2, 0, 0, 0, time.UTC)

// testRules are the two contracts' rules: the bull leg prices in cents, the
// bear leg in tenths of a cent; both trade in 0.01 share from 5 USDT.
var testRules = [2]cex.MarketRule{
	{Symbol: "SOXLUSDT", PriceTick: dec("0.01"), QtyStep: dec("0.01"), MinQty: dec("0.01"), MinNotional: dec("5"), Online: true},
	{Symbol: "SOXSUSDT", PriceTick: dec("0.001"), QtyStep: dec("0.01"), MinQty: dec("0.01"), MinNotional: dec("5"), Online: true},
}

// testAnchors are the appendix's anchors.
var testAnchors = anchors("40", "60")

// newStrategy builds a strategy with the appendix's parameters, changed by
// edit when it is not nil.
func newStrategy(t *testing.T, edit func(*Config)) *Strategy {
	t.Helper()
	cfg := DefaultConfig
	cfg.MeanModel, cfg.MeanMinutes, cfg.SigmaMinutes = MeanEMA, 30, 30
	cfg.DIn, cfg.DOut, cfg.TakerFeeBps = 2, -0.5, 0.65
	if edit != nil {
		edit(&cfg)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func anchors(bull, bear string) cex.Anchors {
	return cex.Anchors{Kind: cex.AnchorBell, Bull: dec(bull), Bear: dec(bear)}
}

// lv parses levels written as "price x quantity", best first.
func lv(levels ...string) []cex.BookLevel {
	out := make([]cex.BookLevel, 0, len(levels))
	for _, s := range levels {
		price, qty, ok := strings.Cut(s, "x")
		if !ok {
			panic("level must be written as price x quantity: " + s)
		}
		out = append(out, cex.BookLevel{Price: dec(strings.TrimSpace(price)), Qty: dec(strings.TrimSpace(qty))})
	}
	return out
}

// book builds a leg's book from its depth; the best prices are the depth's
// first levels.
func book(bids, asks []cex.BookLevel) *cex.Book {
	b := &cex.Book{Bids: bids, Asks: asks, Connected: true}
	if len(bids) > 0 {
		b.BestBid = bids[0]
	}
	if len(asks) > 0 {
		b.BestAsk = asks[0]
	}
	return b
}

// statsAt returns ready statistics whose mean is the appendix's (40.40 /
// 59.42 USD, mu = 3.3333 under the test anchors) and whose sigma under the
// test anchors is sigma: a window of two minutes with the bull leg sigma
// spread units below and above the mean (0.004 USD per unit at anchor 40).
func statsAt(sigma float64) Stats {
	return Stats{
		Ready:       true,
		MeanBullUSD: 40.40,
		MeanBearUSD: 59.42,
		Window:      [][2]float64{{40.40 - 0.004*sigma, 59.42}, {40.40 + 0.004*sigma, 59.42}},
	}
}

func near(got, want float64) bool { return math.Abs(got-want) < 1e-9 }

func wantFloat(t *testing.T, name string, got, want float64) {
	t.Helper()
	if !near(got, want) {
		t.Errorf("%s = %.12f, want %.12f", name, got, want)
	}
}

func wantDec(t *testing.T, name string, got decimal.Decimal, want string) {
	t.Helper()
	if !got.Equal(dec(want)) {
		t.Errorf("%s = %s, want %s", name, got, want)
	}
}

func wantLevels(t *testing.T, name string, got []cex.BookLevel, want []cex.BookLevel) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s = %v, want %v", name, got, want)
		return
	}
	for i := range got {
		if !got[i].Price.Equal(want[i].Price) || !got[i].Qty.Equal(want[i].Qty) {
			t.Errorf("%s = %v, want %v", name, got, want)
			return
		}
	}
}
