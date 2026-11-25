package strategy

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
)

// shortBooks are the books of appendix A example 1 on the bid side: the
// pricing levels are the second bull bid 40.44 (800 counted, 400 offered)
// and the fifth bear bid 59.420 (140.4 counted, 70.2 offered), so a short's
// deviation is 13.3333 - 3.3333 = +10.00. The asks are far enough up that a
// long does not signal (its deviation at 40.47 / 59.45 is +22.50).
func shortBooks() [2]*cex.Book {
	return [2]*cex.Book{
		book(lv("40.45x300", "40.44x500"), lv("40.46x300", "40.47x500")),
		book(lv("59.437x0.4", "59.431x60", "59.428x30", "59.425x30", "59.420x20"),
			lv("59.44x100", "59.442x20", "59.445x20", "59.448x20", "59.45x100")),
	}
}

// longBooks mirror shortBooks: the pricing asks are 40.36 / 59.42, so a
// long's deviation is -6.6667 - 3.3333 = -10.00, and the bids are far enough
// down that a short does not signal (-22.50 at 40.33 / 59.39). The asks offer
// 400 / 70.2 shares like example 1's bids.
func longBooks() [2]*cex.Book {
	return [2]*cex.Book{
		book(lv("40.34x300", "40.33x500"), lv("40.35x300", "40.36x500")),
		book(lv("59.40x100", "59.398x20", "59.395x20", "59.392x20", "59.39x100"),
			lv("59.403x0.4", "59.409x60", "59.412x30", "59.415x30", "59.420x20")),
	}
}

// entryInput is appendix A example 1 with nothing in the way: flat, the
// whole N = 20,000 USDT of quota, no earlier entry.
func entryInput(books [2]*cex.Book) EntryInput {
	return EntryInput{
		Books:       books,
		Rules:       testRules,
		F:           1,
		Anchors:     testAnchors,
		PairAnchors: testAnchors,
		Stats:       statsAt(4),
		QuotaLeft:   dec("20000"),
		Now:         t0,
	}
}

// V5, appendix A example 1's numbers, to the last digit: sell 105.30 / 70.20
// at the pricing levels 40.44 / 59.42.
func TestEvaluateEntry_AppendixExample1(t *testing.T) {
	plan, reason := newStrategy(t, nil).EvaluateEntry(entryInput(shortBooks()))
	if plan == nil {
		t.Fatalf("no plan: %s", reason)
	}
	if reason != "" || plan.Direction != Short {
		t.Errorf("reason %q, direction %s; want a short plan", reason, plan.Direction)
	}
	wantDec(t, "bull limit", plan.Price[cex.Bull], "40.44")
	wantDec(t, "bear limit", plan.Price[cex.Bear], "59.42")
	wantDec(t, "bull quantity", plan.Qty[cex.Bull], "105.30")
	wantDec(t, "bear quantity", plan.Qty[cex.Bear], "70.20")
	wantDec(t, "K", plan.K, "4212")                                    // min(400 x 40, 70.2 x 60), under the quota's 9,990.68
	wantDec(t, "bull quota price", plan.QuotaPrice[cex.Bull], "40.45") // a sell counts at the raw best bid
	wantDec(t, "bear quota price", plan.QuotaPrice[cex.Bear], "59.437")
	wantFloat(t, "x", plan.X, 10)
	wantFloat(t, "mu", plan.Mu, 10000*(0.01-0.58/60)) // 3.3333
	wantFloat(t, "sigma", plan.Sigma, 4)
	wantFloat(t, "round-trip fee in spread units", plan.FeeD, 2.601733333333333) // 2 x 0.65 x (40.44/40 + 59.42/60)
	wantDec(t, "round-trip fee in USDT", plan.FeeUSDT, "1.09585008")             // 2 x 0.65 bps x 8,429.616
}

// V4 and PRD 5.13: the checks run in a fixed order and the first failing one
// is the reason. The input starts with every check failing; each row repairs
// the check that was reported and the next one shows.
func TestEvaluateEntry_ReasonOrder(t *testing.T) {
	s := newStrategy(t, nil)
	in := entryInput([2]*cex.Book{ // one level a side: step 5 fails
		book(lv("40.45x300"), lv("40.46x300")),
		book(lv("59.43x60"), lv("59.44x100")),
	})
	in.Blocked = ReasonFXStale
	in.Stats = Stats{Window: [][2]float64{{40.40, 59.42}, {40.40, 59.42}}} // not ready, and flat
	in.Holding = Long                                                      // the short signal is against it
	in.Last = &LastEntry{At: t0.Add(-10 * time.Second), E: 9.9, Direction: Short}
	in.QuotaLeft = decimal.Zero

	atMean := shortBooks() // the pricing bids are the mean's prices: x = 0
	atMean[cex.Bull] = book(lv("40.41x300", "40.40x500"), atMean[cex.Bull].Asks)
	atMean[cex.Bear] = book(lv("59.43x60", "59.428x20", "59.425x20", "59.422x20", "59.42x80"), atMean[cex.Bear].Asks)
	steps := []struct {
		repair func()
		want   Reason
	}{
		{func() {}, ReasonFXStale},
		{func() { in.Blocked = "" }, ReasonWarmup},
		{func() { in.Stats.Ready = true }, ReasonZeroSigma},
		{func() { in.Stats = statsAt(1) }, ReasonNoLevels},
		{func() { in.Books = atMean }, ReasonNoSignal},
		{func() { in.Books = shortBooks() }, ReasonHoldingOtherSide}, // x = 10 >= 2 x 1
		{func() { in.Holding = Short }, ReasonCostNotCovered},        // 2.5 x 1 < 2.60
		{func() { in.Stats = statsAt(4) }, ReasonSpacing},            // 10 s after an entry at 9.9: needs 10.9
		{func() { in.Last.E = 8 }, ReasonQuotaExhausted},
		{func() { in.QuotaLeft = dec("20000"); in.Rules[cex.Bear].MinQty = dec("100") }, ReasonTooSmall}, // 70.20 < 100
		{func() { in.Rules = testRules }, ""},
	}
	for _, step := range steps {
		step.repair()
		plan, reason := s.EvaluateEntry(in)
		if reason != step.want {
			t.Fatalf("reason = %q, want %q", reason, step.want)
		}
		if (plan != nil) != (step.want == "") {
			t.Fatalf("plan = %v with reason %q", plan, reason)
		}
	}
}

// V4: condition 1 compares the deviation with the entry line and has no fee
// in it; the fee and the margin enter condition 2 only, with the round-trip
// fee computed from the batch's limit prices and reported in both units.
func TestEvaluateEntry_Conditions(t *testing.T) {
	// One cent lower on the bull leg: x = 7.5, short of the line at 8.
	belowLine := shortBooks()
	belowLine[cex.Bull] = book(lv("40.44x300", "40.43x500"), lv("40.46x300", "40.47x500"))

	tests := []struct {
		name        string
		edit        func(*Config)
		books       [2]*cex.Book
		f, sigma    float64
		want        Reason
		wantDir     Direction
		wantX       float64
		wantFeeD    float64
		wantFeeUSDT string
	}{
		{name: "short at the appendix's prices", books: shortBooks(), f: 1, sigma: 4,
			wantDir: Short, wantX: 10, wantFeeD: 2.601733333333333, wantFeeUSDT: "1.09585008"},
		// Long: 2 x 0.65 x (40.36/40 + 59.42/60) = 2.5991; 2 x 0.65 bps x (105.30 x 40.36 + 70.20 x 59.42).
		{name: "long: the fee follows the limit prices", books: longBooks(), f: 1, sigma: 4,
			wantDir: Long, wantX: -10, wantFeeD: 2.599133333333333, wantFeeUSDT: "1.09475496"},
		// F = 1.001: x = 33.3467 - 3.3333, fee 2 x 0.65 x 1.001 x 2.00133333.
		{name: "F scales the fee in spread units", books: shortBooks(), f: 1.001, sigma: 4,
			wantDir: Short, wantX: 30.013333333333333, wantFeeD: 2.604335066666667, wantFeeUSDT: "1.09585008"},
		{name: "no fee: the same signal", edit: func(c *Config) { c.TakerFeeBps = 0 }, books: shortBooks(), f: 1, sigma: 4,
			wantDir: Short, wantX: 10, wantFeeD: 0, wantFeeUSDT: "0"},
		{name: "no fee does not move the line", edit: func(c *Config) { c.TakerFeeBps = 0 }, books: belowLine, f: 1, sigma: 4,
			want: ReasonNoSignal},
		{name: "a large fee fails condition 2, not condition 1", edit: func(c *Config) { c.TakerFeeBps = 50 }, books: shortBooks(), f: 1, sigma: 4,
			want: ReasonCostNotCovered},
		{name: "sigma 1: 2.5 does not cover 2.60", books: shortBooks(), f: 1, sigma: 1, want: ReasonCostNotCovered},
		{name: "sigma 1.1: 2.75 covers 2.60", books: shortBooks(), f: 1, sigma: 1.1,
			wantDir: Short, wantX: 10, wantFeeD: 2.601733333333333, wantFeeUSDT: "1.09585008"},
		{name: "the margin adds to the fee: 10 < 2.60 + 8", edit: func(c *Config) { c.CostMarginD = 8 }, books: shortBooks(), f: 1, sigma: 4,
			want: ReasonCostNotCovered},
		{name: "the margin adds to the fee: 10 >= 2.60 + 7", edit: func(c *Config) { c.CostMarginD = 7 }, books: shortBooks(), f: 1, sigma: 4,
			wantDir: Short, wantX: 10, wantFeeD: 2.601733333333333, wantFeeUSDT: "1.09585008"},
		{name: "a narrower exit line: (2 - 1.5) x 4 < 2.60", edit: func(c *Config) { c.DOut = 1.5 }, books: shortBooks(), f: 1, sigma: 4,
			want: ReasonCostNotCovered},
		{name: "sigma 0", books: shortBooks(), f: 1, sigma: 0, want: ReasonZeroSigma},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := entryInput(tc.books)
			in.F, in.Stats = tc.f, statsAt(tc.sigma)
			plan, reason := newStrategy(t, tc.edit).EvaluateEntry(in)
			if reason != tc.want {
				t.Fatalf("reason = %q, want %q", reason, tc.want)
			}
			if tc.want != "" {
				if plan != nil {
					t.Fatalf("plan = %+v with reason %q", plan, reason)
				}
				return
			}
			if plan.Direction != tc.wantDir {
				t.Errorf("direction = %s, want %s", plan.Direction, tc.wantDir)
			}
			wantFloat(t, "x", plan.X, tc.wantX)
			wantFloat(t, "fee in spread units", plan.FeeD, tc.wantFeeD)
			wantDec(t, "fee in USDT", plan.FeeUSDT, tc.wantFeeUSDT)
		})
	}
}

// V5: the limit is each leg's pricing level (the second bull bid, the fifth
// bear bid); each leg offers the quantity of levels 1 to it x take ratio; the
// smaller anchored amount sizes both legs, capped by the quota and rounded
// down to the step; a batch the venue would not take is not sent.
func TestEvaluateEntry_Size(t *testing.T) {
	bullBids := lv("40.45x300", "40.44x500")                                          // offers 400 shares = 16,000 USD
	bearBids := lv("59.431x60.4", "59.428x30", "59.425x30", "59.423x10", "59.420x10") // offers 70.2 shares = 4,212 USD
	asks := [2][]cex.BookLevel{shortBooks()[cex.Bull].Asks, shortBooks()[cex.Bear].Asks}
	tests := []struct {
		name               string
		edit               func(*Config)
		bull, bear         []cex.BookLevel // bids
		quota              string
		rules              func(*[2]cex.MarketRule)
		want               Reason
		wantBull, wantBear string
		wantK              string // rounded to 6 places
	}{
		// 70.2 x 60 = 4,212 < 16,000.
		{name: "the bear leg is the smaller side", bull: bullBids, bear: shortBooks()[cex.Bear].Bids,
			quota: "20000", wantBull: "105.30", wantBear: "70.20", wantK: "4212"},
		// 40 x 40 = 1,600 < 4,212: 1600 / 60 = 26.666 -> 26.66.
		{name: "the bull leg is the smaller side", bull: lv("40.45x30", "40.44x50"), bear: bearBids,
			quota: "20000", wantBull: "40", wantBear: "26.66", wantK: "1600"},
		// 70.25 x 60 = 4,215: 4215 / 40 = 105.375 -> 105.37.
		{name: "rounded down to the step", bull: bullBids, bear: lv("59.431x60.4", "59.428x30", "59.425x30", "59.423x10", "59.420x10.1"),
			quota: "20000", wantBull: "105.37", wantBear: "70.25", wantK: "4215"},
		// A sell counts at the best bids, 40.45 / 59.431, above its limits:
		// 4000 / (40.45/40 + 59.431/60) = 1,998.234893: 49.955 -> 49.95,
		// 33.303 -> 33.30; 3,999.5298 at the best bids.
		{name: "capped by the quota", bull: bullBids, bear: bearBids,
			quota: "4000", wantBull: "49.95", wantBear: "33.30", wantK: "1998.234893"},
		// Take ratio 1: 140.4 x 60 = 8,424.
		{name: "the take ratio is a parameter", edit: func(c *Config) { c.TakeRatio = dec("1") }, bull: bullBids, bear: bearBids,
			quota: "20000", wantBull: "210.60", wantBear: "140.40", wantK: "8424"},
		// 0.1 share x 40.44 = 4.044 USDT < 5.
		{name: "below the minimum notional", bull: lv("40.45x0.1", "40.44x0.1"), bear: bearBids,
			quota: "20000", want: ReasonTooSmall},
		{name: "below the minimum quantity", bull: bullBids, bear: bearBids, quota: "20000",
			rules: func(r *[2]cex.MarketRule) { r[cex.Bull].MinQty = dec("200") }, want: ReasonTooSmall},
		{name: "no quota left", bull: bullBids, bear: bearBids, quota: "0", want: ReasonQuotaExhausted},
		// 3 USDT of quota: 0.03 / 0.02 shares, about 1.2 USDT a leg.
		{name: "the quota left holds no sendable batch", bull: bullBids, bear: bearBids, quota: "3", want: ReasonQuotaExhausted},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := entryInput([2]*cex.Book{book(tc.bull, asks[cex.Bull]), book(tc.bear, asks[cex.Bear])})
			in.QuotaLeft = dec(tc.quota)
			if tc.rules != nil {
				tc.rules(&in.Rules)
			}
			plan, reason := newStrategy(t, tc.edit).EvaluateEntry(in)
			if reason != tc.want {
				t.Fatalf("reason = %q, want %q", reason, tc.want)
			}
			if tc.want != "" {
				return
			}
			wantDec(t, "bull limit", plan.Price[cex.Bull], "40.44")
			wantDec(t, "bear limit", plan.Price[cex.Bear], "59.42")
			wantDec(t, "bull quantity", plan.Qty[cex.Bull], tc.wantBull)
			wantDec(t, "bear quantity", plan.Qty[cex.Bear], tc.wantBear)
			wantDec(t, "K", plan.K.Round(6), tc.wantK)
			// What the two sells can fill is bounded by their quantities at
			// the best bids, which is what the plan reserves.
			notional := plan.Qty[cex.Bull].Mul(tc.bull[0].Price).Add(plan.Qty[cex.Bear].Mul(tc.bear[0].Price))
			reserved := plan.Qty[cex.Bull].Mul(plan.QuotaPrice[cex.Bull]).Add(plan.Qty[cex.Bear].Mul(plan.QuotaPrice[cex.Bear]))
			if !notional.Equal(reserved) || notional.GreaterThan(in.QuotaLeft) {
				t.Errorf("notional at the best bids %s, reserved %s, quota %s", notional, reserved, in.QuotaLeft)
			}
		})
	}
}

// V5 and PRD 3.2, 4.7: a direction needs a pricing level on the side it
// takes, on both legs: two levels on the bull leg, five on the bear leg.
// Without them its deviation cannot be computed. The two directions are
// judged separately: a thin bid side stops a short, not a long. When neither
// enters, the reason is the furthest check a direction reached.
func TestEvaluateEntry_Levels(t *testing.T) {
	short, long := shortBooks(), longBooks()
	fourBearBids := short[cex.Bear].Bids[:4]
	tests := []struct {
		name       string
		bull, bear *cex.Book
		want       Reason
		wantDir    Direction
	}{
		// One bull bid: no short. The asks give no long signal.
		{"one bull bid level, no long signal", book(lv("40.45x300"), short[cex.Bull].Asks), short[cex.Bear], ReasonNoSignal, ""},
		{"neither side has two levels on the bull leg", book(lv("40.45x300"), lv("40.46x300")), short[cex.Bear], ReasonNoLevels, ""},
		// Four bear bids: no short. The asks give no long signal.
		{"four bear bid levels, no long signal", short[cex.Bull], book(fourBearBids, short[cex.Bear].Asks), ReasonNoSignal, ""},
		{"neither side has five levels on the bear leg", short[cex.Bull], book(fourBearBids, short[cex.Bear].Asks[:4]), ReasonNoLevels, ""},
		{"an empty leg", short[cex.Bull], book(nil, nil), ReasonNoLevels, ""},
		{"a thin bid side does not stop a long", book(lv("40.34x300"), long[cex.Bull].Asks), book(lv("59.40x0.5"), long[cex.Bear].Asks), "", Long},
		{"a thin ask side does not stop a short", book(short[cex.Bull].Bids, lv("40.46x300")), book(short[cex.Bear].Bids, nil), "", Short},
		// A level below one share is a level like any other.
		{"a tiny second bull bid level: a short", book(lv("40.45x300", "40.44x0.9"), short[cex.Bull].Asks), short[cex.Bear], "", Short},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plan, reason := newStrategy(t, nil).EvaluateEntry(entryInput([2]*cex.Book{tc.bull, tc.bear}))
			if reason != tc.want {
				t.Fatalf("reason = %q, want %q", reason, tc.want)
			}
			if tc.want == "" && plan.Direction != tc.wantDir {
				t.Errorf("direction = %s, want %s", plan.Direction, tc.wantDir)
			}
		})
	}
}

// PRD 3.2, 5.4, 5.5: an entry on the levels as the book holds them. A short
// sells at the bids. Bull bids 40.45 x 0.4, 40.44 x 500, 40.43 x 300: the
// tiny best bid is the first level, so the limit is 40.44 with 500.4
// counted, 250.2 offered, 10,008 USD. Bear bids 59.452 x 10, 59.441 x 20,
// 59.438 x 30, 59.431 x 5, 59.427 x 40 span four 0.01 steps: the limit is the
// fifth price itself, 59.427, with 105 counted, 52.5 offered, 3,150 USD.
//
//	x    = 10000 x (40.44/40 + 59.427/60 - 2) - 3.3333 = 14.5 - 3.3333 = 11.1667, past the line at +8
//	K    = min(10,008, 3,150) = 3,150: 78.75 bull, 52.50 bear
//	fee  = 2 x 0.65 x (40.44/40 + 59.427/60) = 2.601885 spread units; 2 x 0.65 bps x (3,184.65 + 3,119.9175) = 0.819593775 USDT
//	the quota counts the sells at the raw best bids 40.45 and 59.452
func TestEvaluateEntry_RawLevels(t *testing.T) {
	in := entryInput(shortBooks())
	in.Books[cex.Bull] = book(lv("40.45x0.4", "40.44x500", "40.43x300"), in.Books[cex.Bull].Asks)
	in.Books[cex.Bear] = book(lv("59.452x10", "59.441x20", "59.438x30", "59.431x5", "59.427x40"), in.Books[cex.Bear].Asks)
	plan, reason := newStrategy(t, nil).EvaluateEntry(in)
	if plan == nil || plan.Direction != Short {
		t.Fatalf("plan %+v, reason %q; want a short", plan, reason)
	}
	wantDec(t, "bull limit", plan.Price[cex.Bull], "40.44")
	wantDec(t, "bear limit", plan.Price[cex.Bear], "59.427")
	wantDec(t, "bull quantity", plan.Qty[cex.Bull], "78.75")
	wantDec(t, "bear quantity", plan.Qty[cex.Bear], "52.50")
	wantDec(t, "K", plan.K, "3150")
	wantFloat(t, "x", plan.X, 33.5/3)
	wantFloat(t, "round-trip fee in spread units", plan.FeeD, 2.601885)
	wantDec(t, "round-trip fee in USDT", plan.FeeUSDT, "0.819593775")
	wantDec(t, "bull quota price: the raw best bid", plan.QuotaPrice[cex.Bull], "40.45")
	wantDec(t, "bear quota price: the raw best bid", plan.QuotaPrice[cex.Bear], "59.452")

	// The five bear bids inside one 0.01 step, 59.438 down to 59.434: the
	// limit is 59.434, not a rounded 59.43. 200 counted, 100 offered, 6,000
	// USD, below the bull leg's 10,008: 150 / 100, and
	// x = 10000 x (40.44/40 + 59.434/60 - 2) - 3.3333 = 15.6667 - 3.3333 = 12.3333.
	in.Books[cex.Bear] = book(lv("59.438x40", "59.437x30", "59.436x50", "59.435x20", "59.434x60"), in.Books[cex.Bear].Asks)
	plan, reason = newStrategy(t, nil).EvaluateEntry(in)
	if plan == nil || plan.Direction != Short {
		t.Fatalf("one step: plan %+v, reason %q; want a short", plan, reason)
	}
	wantDec(t, "one step: bear limit", plan.Price[cex.Bear], "59.434")
	wantDec(t, "one step: bull quantity", plan.Qty[cex.Bull], "150")
	wantDec(t, "one step: bear quantity", plan.Qty[cex.Bear], "100")
	wantFloat(t, "one step: x", plan.X, 37.0/3)
}

// Numbers that cannot be compared never yield an entry plan. The books and
// statistics are example 1's, which signal a short; each case breaks one
// input.
func TestEvaluateEntry_InvalidNumbers(t *testing.T) {
	tests := []struct {
		name string
		edit func(*EntryInput)
		want Reason
	}{
		{"an anchor of zero", func(in *EntryInput) { in.Anchors = cex.Anchors{Kind: cex.AnchorBell, Bear: dec("60")} }, ReasonNoAnchor},
		{"no anchors at all", func(in *EntryInput) { in.Anchors = cex.Anchors{} }, ReasonNoAnchor},
		{"a negative anchor", func(in *EntryInput) { in.Anchors = anchors("40", "-60") }, ReasonNoAnchor},
		{"F is not a number", func(in *EntryInput) { in.F = math.NaN() }, ReasonFXStale},
		{"F is infinite", func(in *EntryInput) { in.F = math.Inf(1) }, ReasonFXStale},
		// F = 0 is what a rate never read looks like. The deviation would be
		// -20,003.33 and the fee threshold 0: a long at any book.
		{"F is zero", func(in *EntryInput) { in.F = 0 }, ReasonFXStale},
		{"F is negative", func(in *EntryInput) { in.F = -1 }, ReasonFXStale},
		{"a mean that is not a number", func(in *EntryInput) { in.Stats.MeanBullUSD = math.NaN() }, ReasonWarmup},
		{"a window minute that is not a number", func(in *EntryInput) { in.Stats.Window[1][1] = math.NaN() }, ReasonWarmup},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, books := range [][2]*cex.Book{shortBooks(), longBooks()} {
				in := entryInput(books)
				tc.edit(&in)
				if plan, reason := newStrategy(t, nil).EvaluateEntry(in); plan != nil || reason != tc.want {
					t.Errorf("plan %+v, reason %q; want no plan and %q", plan, reason, tc.want)
				}
			}
		})
	}
}

// A deviation exactly on the entry line. PRD 5 says "at or beyond the line";
// the comparison is made on float64 values, and a deviation that is on the
// line in exact arithmetic is computed about 1e-12 spread units off it, on
// either side. With sigma 5 the line is at 10.00 and example 1's short is at
// exactly 10.00: computed, x is 9.9999999999989 and the line
// 10.0000000000033, so it does not signal. What the code decides is the
// comparison of those computed numbers; one price tick (2.5 units on the
// bull leg) is far beyond that noise and decides either way without doubt.
func TestEvaluateEntry_OnTheLine(t *testing.T) {
	s := newStrategy(t, nil)
	in := entryInput(shortBooks())
	in.Stats = statsAt(5)
	x := Spread(1, 40.44, 40, 59.42, 60) - in.Stats.Mu(in.Anchors)
	line := s.cfg.DIn * in.Stats.Sigma(in.Anchors)
	if math.Abs(x-line) > 1e-9 || math.Abs(x-10) > 1e-9 {
		t.Fatalf("x = %.17g, line = %.17g: the case is not on the line", x, line)
	}
	plan, reason := s.EvaluateEntry(in)
	if signalled := plan != nil; signalled != (x >= line) {
		t.Errorf("on the line: plan %t (reason %q), but x >= line is %t for x = %.17g, line = %.17g", signalled, reason, x >= line, x, line)
	}
	if plan != nil || reason != ReasonNoSignal {
		t.Errorf("on the line with these numbers: plan %+v, reason %q; the computed x is below the computed line", plan, reason)
	}

	// One cent further on the bull leg: x = 12.5, a short.
	in.Books[cex.Bull] = book(lv("40.46x300", "40.45x500"), lv("40.47x300", "40.48x500"))
	if plan, reason := s.EvaluateEntry(in); plan == nil || plan.Direction != Short {
		t.Errorf("one tick beyond the line: plan %+v, reason %q; want a short", plan, reason)
	}
	// One cent short of it: x = 7.5.
	in.Books[cex.Bull] = book(lv("40.44x300", "40.43x500"), lv("40.47x300", "40.48x500"))
	if plan, reason := s.EvaluateEntry(in); plan != nil || reason != ReasonNoSignal {
		t.Errorf("one tick inside the line: plan %+v, reason %q; want no_signal", plan, reason)
	}
}

// The quota bounds what the orders can fill, not only their notional at the
// limits (PRD 6.1, 6.4). Example 1's short with 5,000 USDT left: sized at
// the limits 40.44 / 59.42 the two sells would be 62.45 / 41.63, 4,999.13 at
// the limits and 5,000.22 at the bids they fill at. Counted at the raw best
// bids 40.45 / 59.437, K = 5000 / (40.45/40 + 59.437/60) = 2,497.668842:
// 62.44 / 41.62, at most 4,999.46594 whatever bids they hit.
func TestEvaluateEntry_SellQuotaAtTheBestBid(t *testing.T) {
	in := entryInput(shortBooks())
	in.QuotaLeft = dec("5000")
	plan, reason := newStrategy(t, nil).EvaluateEntry(in)
	if plan == nil || plan.Direction != Short {
		t.Fatalf("plan %+v, reason %q; want a short", plan, reason)
	}
	wantDec(t, "bull limit", plan.Price[cex.Bull], "40.44")
	wantDec(t, "bear limit", plan.Price[cex.Bear], "59.42")
	wantDec(t, "bull quantity", plan.Qty[cex.Bull], "62.44")
	wantDec(t, "bear quantity", plan.Qty[cex.Bear], "41.62")
	wantDec(t, "K", plan.K.Round(6), "2497.668842")
	wantDec(t, "reserved at the best bids", plan.Qty[cex.Bull].Mul(plan.QuotaPrice[cex.Bull]).Add(plan.Qty[cex.Bear].Mul(plan.QuotaPrice[cex.Bear])), "4999.46594")
	// Walked down the bids of the decision's book the two sells receive
	// 62.44 x 40.45 and 0.4 x 59.437 + 41.22 x 59.431.
	filled := value(walkOrLast(in.Books[cex.Bull], Short, plan.Qty[cex.Bull])).Add(value(walkOrLast(in.Books[cex.Bear], Short, plan.Qty[cex.Bear])))
	wantDec(t, "filled on the decision's book", filled, "4999.21862")
	if filled.GreaterThan(in.QuotaLeft) {
		t.Errorf("the sells can fill %s, over the %s left", filled, in.QuotaLeft)
	}

	// A buy cannot fill above its limit: a long counts at its limits.
	long := entryInput(longBooks())
	long.QuotaLeft = dec("5000")
	plan, reason = newStrategy(t, nil).EvaluateEntry(long)
	if plan == nil || plan.Direction != Long {
		t.Fatalf("plan %+v, reason %q; want a long", plan, reason)
	}
	wantDec(t, "long bull quota price", plan.QuotaPrice[cex.Bull], "40.36")
	wantDec(t, "long bear quota price", plan.QuotaPrice[cex.Bear], "59.42")
	if at := plan.Qty[cex.Bull].Mul(dec("40.36")).Add(plan.Qty[cex.Bear].Mul(dec("59.42"))); at.GreaterThan(dec("5000")) || at.LessThan(dec("4999")) {
		t.Errorf("the long's notional at its limits = %s, want just under 5000", at)
	}
}

// PRD 5.7 and owner item 59 ("最小两腿共100U"): a batch whose two legs come
// to less than strategy.min_batch_notional at their limit prices is not sent.
// The bear bids keep example 1's prices; only their quantities shrink, so the
// signal is the same x = 10 and the size alone decides.
func TestEvaluateEntry_MinBatchNotional(t *testing.T) {
	bull := lv("40.45x300", "40.44x500")
	tests := []struct {
		name     string
		bearBids []cex.BookLevel
		edit     func(*Config)
		want     Reason
		wantBull string
		wantBear string
	}{
		// 1.6 x 1/2 = 0.8 SOXS -> K = 48 USD -> 1.2 SOXL: 1.2 x 40.44 + 0.8 x 59.42 = 96.064 USDT.
		{name: "96.06 USDT is below 100", bearBids: lv("59.437x0.4", "59.431x0.3", "59.428x0.3", "59.425x0.3", "59.420x0.3"), want: ReasonTooSmall},
		// 1.7 x 1/2 = 0.85 SOXS -> K = 51 USD -> 1.275 -> 1.27 SOXL: 51.3588 + 50.507 = 101.8658 USDT.
		{name: "101.87 USDT is enough", bearBids: lv("59.437x0.4", "59.431x0.3", "59.428x0.3", "59.425x0.4", "59.420x0.3"), wantBull: "1.27", wantBear: "0.85"},
		// The same 96.06 USDT with the minimum set to 90.
		{name: "a lower minimum lets it through", bearBids: lv("59.437x0.4", "59.431x0.3", "59.428x0.3", "59.425x0.3", "59.420x0.3"),
			edit: func(c *Config) { c.MinBatchNotional = dec("90") }, wantBull: "1.2", wantBear: "0.8"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			books := shortBooks()
			books[cex.Bull] = book(bull, books[cex.Bull].Asks)
			books[cex.Bear] = book(tc.bearBids, books[cex.Bear].Asks)
			plan, reason := newStrategy(t, tc.edit).EvaluateEntry(entryInput(books))
			if tc.want != "" {
				if plan != nil || reason != tc.want {
					t.Fatalf("plan %v, reason %q; want no plan with %q", plan, reason, tc.want)
				}
				return
			}
			if plan == nil {
				t.Fatalf("no plan: %s", reason)
			}
			wantDec(t, "bull quantity", plan.Qty[cex.Bull], tc.wantBull)
			wantDec(t, "bear quantity", plan.Qty[cex.Bear], tc.wantBear)
		})
	}
}

// The minimum batch defaults to 100 USDT and may not be negative; 0 turns it off.
func TestConfigMinBatchNotional(t *testing.T) {
	if got := DefaultConfig.MinBatchNotional; !got.Equal(dec("100")) {
		t.Errorf("default min_batch_notional = %s, want 100", got)
	}
	cfg := newStrategy(t, nil).cfg
	cfg.MinBatchNotional = dec("-1")
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "strategy.min_batch_notional") {
		t.Errorf("min_batch_notional -1: Validate = %v, want it refused", err)
	}
	cfg.MinBatchNotional = dec("0")
	if err := cfg.Validate(); err != nil {
		t.Errorf("min_batch_notional 0: Validate = %v, want it accepted", err)
	}
}

// Deviation gives the x of the decisions, for the record: example 1's short
// x = +10.00; a side without the pricing level gives none.
func TestDeviation(t *testing.T) {
	s, in := newStrategy(t, nil), entryInput(shortBooks())
	x, ok := s.Deviation(in.Books, Short, in.F, in.Anchors, in.Stats)
	if !ok {
		t.Fatal("no deviation")
	}
	wantFloat(t, "x", x, 10)
	thin := in.Books
	thin[cex.Bear] = book(lv("59.437 x 1"), nil)
	if x, ok := s.Deviation(thin, Short, in.F, in.Anchors, in.Stats); ok || !math.IsNaN(x) {
		t.Errorf("Deviation without a pricing level = %v, %t; want NaN, false", x, ok)
	}
}

// The entry's x is the spread at the pricing levels minus the mean, to the
// last bit the same as Deviation's (which the record and the exit use), for
// both directions and several F.
func TestEntryDeviationBitIdentical(t *testing.T) {
	s := newStrategy(t, nil)
	for _, books := range [][2]*cex.Book{shortBooks(), longBooks()} {
		for _, f := range []float64{1, 1 + 1e-7, 1 - 3e-8, 1.0000123} {
			in := entryInput(books)
			in.F = f
			plan, reason := s.EvaluateEntry(in)
			if plan == nil {
				t.Fatalf("F %v: no plan: %s", f, reason)
			}
			x, _ := s.Deviation(in.Books, plan.Direction, in.F, in.Anchors, in.Stats)
			if math.Float64bits(plan.X) != math.Float64bits(x) {
				t.Errorf("F %v, %s: entry x %v, Deviation %v", f, plan.Direction, plan.X, x)
			}
		}
	}
}

// BenchmarkEvaluateEntryNoSignal is the decision of almost every round:
// both directions priced and neither at the line (d_in = 3: example 1's
// short x = +10 is under 3 x 4).
func BenchmarkEvaluateEntryNoSignal(b *testing.B) {
	cfg := DefaultConfig
	cfg.MeanModel, cfg.MeanMinutes, cfg.SigmaMinutes = MeanEMA, 30, 30
	cfg.DIn, cfg.DOut, cfg.TakerFeeBps = 3, -0.5, 0.65
	s, err := New(cfg)
	if err != nil {
		b.Fatal(err)
	}
	in := entryInput(shortBooks())
	if plan, reason := s.EvaluateEntry(in); plan != nil || reason != ReasonNoSignal {
		b.Fatalf("plan %v, reason %q; want no signal", plan, reason)
	}
	b.ReportAllocs()
	for b.Loop() {
		s.EvaluateEntry(in)
	}
}
