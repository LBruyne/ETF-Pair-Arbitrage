package strategy

import (
	"math"
	"testing"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
)

// exitInput is appendix A example 2: a short cycle of 165.30 / 110.20 shares
// and asks whose pricing levels, the second bull ask and the fifth bear ask,
// are 40.38 / 59.40, offering 350 / 100 shares.
func exitInput() ExitInput {
	return ExitInput{
		Direction: Short,
		Hold:      [2]decimal.Decimal{dec("165.30"), dec("110.20")},
		Books: [2]*cex.Book{
			book(lv("40.36x300", "40.35x400"), lv("40.37x300", "40.38x400")),
			book(lv("59.37x100", "59.36x100"), lv("59.385x100", "59.391x40", "59.394x30", "59.396x20", "59.400x10")),
		},
		DepthOK:     [2]bool{true, true},
		Rules:       testRules,
		F:           1,
		Anchors:     testAnchors,
		PairAnchors: testAnchors,
		Stats:       statsAt(4),
	}
}

// V10, appendix A example 2: the book takes part of the position; 150.00 /
// 100.00 are bought back and 15.30 / 10.20 stay.
func TestEvaluateExit_AppendixExample2(t *testing.T) {
	in := exitInput()
	plan, reason := newStrategy(t, nil).EvaluateExit(in)
	if plan == nil {
		t.Fatalf("no plan: %s", reason)
	}
	if reason != "" || plan.Side != cex.SideBuy {
		t.Errorf("reason %q, side %s; want a buy", reason, plan.Side)
	}
	wantDec(t, "bull limit", plan.Price[cex.Bull], "40.38")
	wantDec(t, "bear limit", plan.Price[cex.Bear], "59.40") // the fifth ask
	wantFloat(t, "x", plan.X, -25.0/3)                      // -5 - 3.3333 = -8.3333, past the line at -2
	wantFloat(t, "mu", plan.Mu, 10.0/3)
	wantFloat(t, "sigma", plan.Sigma, 4)
	wantDec(t, "bull quantity", plan.Qty[cex.Bull], "150") // K = min(350 x 40, 100 x 60) = 6,000
	wantDec(t, "bear quantity", plan.Qty[cex.Bear], "100")
	wantDec(t, "bull remainder", in.Hold[cex.Bull].Sub(plan.Qty[cex.Bull]), "15.30")
	wantDec(t, "bear remainder", in.Hold[cex.Bear].Sub(plan.Qty[cex.Bear]), "10.20")
}

// V10: the exit line is on the entry's axis. With entry_out = -0.5 and sigma 4 a
// short exits at x <= -2 and a long at x >= +2: half a sigma past the mean.
// entry_out = +0.5 puts the line half a sigma before the mean.
func TestEvaluateExit_Line(t *testing.T) {
	// The pricing level of the side the exit takes, per leg: the second
	// bull level and the fifth bear level. The levels are deep enough to
	// take a small position whole.
	asks := func(bull, bear string) [2]*cex.Book {
		return [2]*cex.Book{book(nil, lv("40.00x900", bull+"x900")),
			book(nil, lv("59.001x900", "59.002x900", "59.003x900", "59.004x900", bear+"x900"))}
	}
	bids := func(bull, bear string) [2]*cex.Book {
		return [2]*cex.Book{book(lv("41.00x900", bull+"x900"), nil),
			book(lv("60.004x900", "60.003x900", "60.002x900", "60.001x900", bear+"x900"), nil)}
	}
	tests := []struct {
		name     string
		dir      Direction
		dOut     float64
		books    [2]*cex.Book
		want     Reason
		wantSide cex.Side
		wantX    float64
	}{
		// Short, bought back at the asks. 40.38 / 59.44: 95 - 93.3333 - 3.3333.
		{"short, past the mean but not half a sigma", Short, -0.5, asks("40.38", "59.44"), ReasonNoSignal, "", 0},
		{"short, more than half a sigma past the mean", Short, -0.5, asks("40.38", "59.43"), "", cex.SideBuy, -10.0 / 3},
		{"short, at the mean", Short, -0.5, asks("40.40", "59.42"), ReasonNoSignal, "", 0},
		{"short, still on the entry's side", Short, -0.5, asks("40.41", "59.42"), ReasonNoSignal, "", 0},
		// Long, sold at the bids. 40.41 / 59.42: 102.5 - 96.6667 - 3.3333.
		{"long, past the mean but not half a sigma", Long, -0.5, bids("40.40", "59.43"), ReasonNoSignal, "", 0},
		{"long, more than half a sigma past the mean", Long, -0.5, bids("40.41", "59.42"), "", cex.SideSell, 2.5},
		{"long, at the mean", Long, -0.5, bids("40.40", "59.42"), ReasonNoSignal, "", 0},
		{"long, still on the entry's side", Long, -0.5, bids("40.39", "59.42"), ReasonNoSignal, "", 0},
		// entry_out = +0.5: the lines are at +2 for a short and -2 for a long.
		{"short, entry_out +0.5, half a sigma before the mean", Short, 0.5, asks("40.38", "59.44"), "", cex.SideBuy, -5.0 / 3},
		{"short, entry_out +0.5, not yet", Short, 0.5, asks("40.41", "59.42"), ReasonNoSignal, "", 0}, // x = +2.5
		{"long, entry_out +0.5, half a sigma before the mean", Long, 0.5, bids("40.40", "59.43"), "", cex.SideSell, 5.0 / 3},
		{"long, entry_out +0.5, not yet", Long, 0.5, bids("40.39", "59.42"), ReasonNoSignal, "", 0}, // x = -2.5
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := exitInput()
			in.Direction, in.Books = tc.dir, tc.books
			in.Hold = [2]decimal.Decimal{dec("105.30"), dec("70.20")}
			plan, reason := newStrategy(t, func(c *Config) { c.EntryOut = tc.dOut }).EvaluateExit(in)
			if reason != tc.want {
				t.Fatalf("reason = %q, want %q", reason, tc.want)
			}
			if tc.want != "" {
				if plan != nil {
					t.Fatalf("plan = %+v with reason %q", plan, reason)
				}
				return
			}
			if plan.Side != tc.wantSide {
				t.Errorf("side = %s, want %s", plan.Side, tc.wantSide)
			}
			wantFloat(t, "x", plan.X, tc.wantX)
			wantDec(t, "bull quantity: all of it", plan.Qty[cex.Bull], "105.30")
			wantDec(t, "bear quantity: all of it", plan.Qty[cex.Bear], "70.20")
		})
	}
}

// V10: the exit has no entry price and no fee in it. A short sold at 40.44 /
// 59.42 is bought back at 40.60 / 59.60, at a loss, once the mean has moved
// above those prices: mean 40.70 / 59.60 gives mu = 108.33 and x = -25.
func TestEvaluateExit_AtALoss(t *testing.T) {
	in := exitInput()
	in.Hold = [2]decimal.Decimal{dec("105.30"), dec("70.20")}
	in.Books = [2]*cex.Book{book(nil, lv("40.59x900", "40.60x900")), book(nil, lv("59.59x900", "59.592x900", "59.595x900", "59.598x900", "59.60x900"))}
	in.Stats = Stats{Ready: true, MeanBullUSD: 40.70, MeanBearUSD: 59.60, Window: [][2]float64{{40.684, 59.60}, {40.716, 59.60}}}
	plan, reason := newStrategy(t, func(c *Config) { c.TakerFeeBps = 50 }).EvaluateExit(in)
	if plan == nil {
		t.Fatalf("no plan: %s", reason)
	}
	wantFloat(t, "x", plan.X, -25)
	wantDec(t, "bull limit, above the entry's 40.44", plan.Price[cex.Bull], "40.60")
	wantDec(t, "bear limit, above the entry's 59.42", plan.Price[cex.Bear], "59.60")
	wantDec(t, "bull quantity", plan.Qty[cex.Bull], "105.30")
}

// V10 and PRD 9.5, 9.6: the size of one pair of exit orders. The asks are
// example 2's: limits 40.38 / 59.40, 350 / 100 shares offered.
func TestEvaluateExit_Size(t *testing.T) {
	tests := []struct {
		name               string
		holdBull, holdBear string
		pair               [2]string
		rules              func(*[2]cex.MarketRule)
		want               Reason
		wantBull, wantBear string
	}{
		{name: "the book takes everything: all of both legs", holdBull: "105.30", holdBear: "70.20", wantBull: "105.30", wantBear: "70.20"},
		{name: "everything, with a dust gap between the legs", holdBull: "105.31", holdBear: "70.20", wantBull: "105.31", wantBear: "70.20"},
		{name: "the bear book is the smaller side", holdBull: "165.30", holdBear: "110.20", wantBull: "150", wantBear: "100"},
		// Offered 350 x 40 = 14,000 and 100 x 60 = 6,000 USD; the bull leg
		// holds only 100 x 40 = 4,000, which sizes both: 4000 / 60 = 66.66.
		{name: "not above what a leg holds", holdBull: "100", holdBear: "110.20", wantBull: "100", wantBear: "66.66"},
		// K = 6,000 leaves 0.05 / 0.03 shares, about 2 USDT each and too
		// small to send. The batch is not shrunk for them: they are dust
		// when the cycle closes.
		{name: "a remainder too small to send is left", holdBull: "150.05", holdBear: "100.03", wantBull: "150", wantBear: "100"},
		// The book takes all of both legs, but 0.03 bear shares are 1.78
		// USDT, below the minimum order value: the bull leg alone is closed.
		{name: "everything, one leg's remainder cannot be sent", holdBull: "105.30", holdBear: "0.03", wantBull: "105.30", wantBear: "0"},
		{name: "everything, one leg holds nothing", holdBull: "0", holdBear: "70.20", wantBull: "0", wantBear: "70.20"},
		// The sizes follow the pairing anchors 50 / 50: K = min(350, 100, ...) x 50.
		{name: "sized by the pairing anchors", holdBull: "165.30", holdBear: "165.30", pair: [2]string{"50", "50"}, wantBull: "100", wantBear: "100"},
		{name: "one leg of the batch is below the venue's minimum", holdBull: "165.30", holdBear: "110.20",
			rules: func(r *[2]cex.MarketRule) { r[cex.Bear].MinQty = dec("101") }, wantBull: "150", wantBear: "0"},
		{name: "both legs of the batch are below the venue's minimum", holdBull: "165.30", holdBear: "110.20",
			rules: func(r *[2]cex.MarketRule) { r[cex.Bull].MinQty, r[cex.Bear].MinQty = dec("151"), dec("101") }, want: ReasonTooSmall},
		{name: "nothing left that can be sent", holdBull: "0.05", holdBear: "0.03", want: ReasonTooSmall},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := exitInput()
			in.Hold = [2]decimal.Decimal{dec(tc.holdBull), dec(tc.holdBear)}
			if tc.pair[0] != "" {
				in.PairAnchors = anchors(tc.pair[0], tc.pair[1])
			}
			if tc.rules != nil {
				tc.rules(&in.Rules)
			}
			plan, reason := newStrategy(t, nil).EvaluateExit(in)
			if reason != tc.want {
				t.Fatalf("reason = %q, want %q", reason, tc.want)
			}
			if tc.want != "" {
				return
			}
			wantDec(t, "bull quantity", plan.Qty[cex.Bull], tc.wantBull)
			wantDec(t, "bear quantity", plan.Qty[cex.Bear], tc.wantBear)
			for l, rule := range in.Rules {
				if !plan.Qty[l].IsZero() && !rule.Sendable(plan.Qty[l], plan.Price[l]) {
					t.Errorf("leg %s has an order of %s, which cannot be sent", cex.Leg(l), plan.Qty[l])
				}
			}
		})
	}
}

// V10 and PRD 9.7: no exit at the line on a book that is not usable, without
// a pricing level on both legs, or without statistics that were ever ready.
func TestEvaluateExit_NotJudged(t *testing.T) {
	tests := []struct {
		name string
		edit func(*ExitInput)
		want Reason
	}{
		{"bull depth not usable", func(in *ExitInput) { in.DepthOK[cex.Bull] = false }, ReasonDepth},
		{"bear depth not usable", func(in *ExitInput) { in.DepthOK[cex.Bear] = false }, ReasonDepth},
		{"statistics never ready", func(in *ExitInput) { in.Stats = Stats{} }, ReasonWarmup},
		{"statistics not ready, though at the line", func(in *ExitInput) { in.Stats.Ready = false }, ReasonWarmup},
		{"depth is checked before the statistics", func(in *ExitInput) { in.DepthOK[cex.Bear], in.Stats = false, Stats{} }, ReasonDepth},
		{"four ask levels on the bear leg", func(in *ExitInput) {
			in.Books[cex.Bear] = book(nil, lv("59.385x100", "59.391x40", "59.394x30", "59.396x20"))
		}, ReasonNoLevels},
		{"no asks on the bear leg", func(in *ExitInput) { in.Books[cex.Bear] = book(lv("59.37x100"), nil) }, ReasonNoLevels},
		{"one ask level on the bull leg", func(in *ExitInput) { in.Books[cex.Bull] = book(nil, lv("40.37x300")) }, ReasonNoLevels},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := exitInput()
			tc.edit(&in)
			if plan, reason := newStrategy(t, nil).EvaluateExit(in); plan != nil || reason != tc.want {
				t.Errorf("plan %v, reason %q; want no plan and %q", plan, reason, tc.want)
			}
		})
	}
}

// PRD 10.2 and 10.4: a forced exit closes everything left, one order per leg
// through the raw best price; a leg with nothing or with dust gets none.
func TestForcedOrders(t *testing.T) {
	books := [2]*cex.Book{
		book(lv("40.45x300"), lv("40.47x0.4", "40.50x300")),
		book(lv("59.418x2", "59.36x200"), lv("59.385x100")),
	}
	tests := []struct {
		name               string
		dir                Direction
		holdBull, holdBear string
		wantSide           cex.Side
		wantBull, wantBear string // the limit; "" for no order
	}{
		// Buys: 40.47 x 1.001 = 40.51047 -> 40.52; 59.385 x 1.001 = 59.444385 -> 59.445.
		{"short cycle: buy both back, whatever the levels hold", Short, "165.30", "110.20", cex.SideBuy, "40.52", "59.445"},
		// Sells: 40.45 x 0.999 = 40.40955 -> 40.40; 59.418 x 0.999 = 59.358582 -> 59.358.
		{"long cycle: sell both", Long, "165.30", "110.20", cex.SideSell, "40.40", "59.358"},
		{"one leg is flat", Short, "15.30", "0", cex.SideBuy, "40.52", ""},
		{"one leg is dust", Short, "15.30", "0.05", cex.SideBuy, "40.52", ""}, // 0.05 x 59.445 = 2.97 USDT
		{"only dust is left", Short, "0.05", "0.05", cex.SideBuy, "", ""},
		{"nothing is left", Short, "0", "0", cex.SideBuy, "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hold := [2]decimal.Decimal{dec(tc.holdBull), dec(tc.holdBear)}
			orders := newStrategy(t, nil).ForcedOrders(tc.dir, hold, books, testRules)
			for l, want := range [2]string{tc.wantBull, tc.wantBear} {
				order := orders[l]
				if want == "" {
					if order != nil {
						t.Errorf("leg %s: order %+v, want none", cex.Leg(l), order)
					}
					continue
				}
				if order == nil {
					t.Errorf("leg %s: no order, want one at %s", cex.Leg(l), want)
					continue
				}
				if order.Side != tc.wantSide {
					t.Errorf("leg %s: side %s, want %s", cex.Leg(l), order.Side, tc.wantSide)
				}
				wantDec(t, "limit", order.Price, want)
				wantDec(t, "quantity: everything left", order.Qty, hold[l].String())
			}
		})
	}

	// A flat leg's book is not read: it may have no price at all.
	orders := newStrategy(t, nil).ForcedOrders(Short, [2]decimal.Decimal{dec("15.30"), decimal.Zero}, [2]*cex.Book{books[cex.Bull], nil}, testRules)
	if orders[cex.Bull] == nil || orders[cex.Bear] != nil {
		t.Errorf("orders = %v, want the bull leg's only", orders)
	}
}

// Numbers that cannot be compared never yield an exit plan. The input is
// example 2's, which exits; each case breaks one input. A book far from the
// line must not exit either: with an anchor of zero every comparison of the
// deviation is false, which without the check read as "past the line".
func TestEvaluateExit_InvalidNumbers(t *testing.T) {
	tests := []struct {
		name string
		edit func(*ExitInput)
		want Reason
	}{
		{"an anchor of zero", func(in *ExitInput) { in.Anchors = cex.Anchors{Kind: cex.AnchorBell, Bull: dec("40")} }, ReasonNoAnchor},
		{"no anchors at all", func(in *ExitInput) { in.Anchors = cex.Anchors{} }, ReasonNoAnchor},
		{"F is not a number", func(in *ExitInput) { in.F = math.NaN() }, ReasonFXStale},
		// F = 0: a rate never read. The deviation would be far below any
		// short's exit line.
		{"F is zero", func(in *ExitInput) { in.F = 0 }, ReasonFXStale},
		{"F is negative", func(in *ExitInput) { in.F = -0.5 }, ReasonFXStale},
		{"a mean that is not a number", func(in *ExitInput) { in.Stats.MeanBearUSD = math.NaN() }, ReasonWarmup},
	}
	farFromTheLine := [2]*cex.Book{book(nil, lv("40.50x300", "40.51x400")), book(nil, lv("59.50x100", "59.502x20", "59.505x20", "59.508x20", "59.51x100"))}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, books := range [][2]*cex.Book{exitInput().Books, farFromTheLine} {
				in := exitInput()
				in.Books = books
				tc.edit(&in)
				if plan, reason := newStrategy(t, nil).EvaluateExit(in); plan != nil || reason != tc.want {
					t.Errorf("plan %+v, reason %q; want no plan and %q", plan, reason, tc.want)
				}
			}
		})
	}
	// The same far book with valid numbers is simply not at the line.
	in := exitInput()
	in.Books = farFromTheLine
	if plan, reason := newStrategy(t, nil).EvaluateExit(in); plan != nil || reason != ReasonNoSignal {
		t.Errorf("far from the line: plan %+v, reason %q; want no_signal", plan, reason)
	}
}

// A deviation exactly on the exit line, as for the entry: the comparison is
// made on the computed float64 values, which lie about 1e-12 spread units
// off the exact ones. With sigma 10 and entry_out = -0.5 the lines are at -5 and
// +5. A short whose asks are at 40.38 / 59.42 is at exactly -5: computed, x
// is -4.9999999999972 against a line of -4.9999999999994, and it does not
// exit. A long whose bids are at 40.42 / 59.42 is at exactly +5: computed,
// x is 5.0000000000017 against 4.9999999999994, and it exits. One price
// tick settles either case.
func TestEvaluateExit_OnTheLine(t *testing.T) {
	s := newStrategy(t, nil)
	short := exitInput()
	short.Stats = statsAt(10)
	short.Books = [2]*cex.Book{book(nil, lv("40.37x300", "40.38x400")), book(nil, lv("59.41x100", "59.412x20", "59.415x20", "59.418x20", "59.42x100"))}
	long := short
	long.Direction = Long
	long.Books = [2]*cex.Book{book(lv("40.43x300", "40.42x400"), nil), book(lv("59.43x100", "59.428x20", "59.425x20", "59.422x20", "59.42x100"), nil)}
	mu, half := short.Stats.Mu(testAnchors), -s.cfg.EntryOut*short.Stats.Sigma(testAnchors)

	xShort := Spread(1, 40.38, 40, 59.42, 60) - mu
	xLong := Spread(1, 40.42, 40, 59.42, 60) - mu
	if math.Abs(xShort+5) > 1e-9 || math.Abs(xLong-5) > 1e-9 || math.Abs(half-5) > 1e-9 {
		t.Fatalf("x = %.17g / %.17g, lines at -/+ %.17g: the cases are not on the line", xShort, xLong, half)
	}
	planShort, reasonShort := s.EvaluateExit(short)
	if exits := planShort != nil; exits != (xShort <= -half) || exits {
		t.Errorf("short on the line: plan %t (reason %q); the computed x %.17g is above the computed line %.17g, so it must not exit", exits, reasonShort, xShort, -half)
	}
	planLong, reasonLong := s.EvaluateExit(long)
	if exits := planLong != nil; exits != (xLong >= half) || !exits {
		t.Errorf("long on the line: plan %t (reason %q); the computed x %.17g is above the computed line %.17g, so it must exit", exits, reasonLong, xLong, half)
	}

	// One cent further on the bull leg the short exits; one cent short of
	// the line the long does not.
	short.Books[cex.Bull] = book(nil, lv("40.36x300", "40.37x400"))
	if plan, reason := s.EvaluateExit(short); plan == nil {
		t.Errorf("short one tick beyond the line: no plan, reason %q", reason)
	}
	long.Books[cex.Bull] = book(lv("40.42x300", "40.41x400"), nil)
	if plan, reason := s.EvaluateExit(long); plan != nil || reason != ReasonNoSignal {
		t.Errorf("long one tick inside the line: plan %+v, reason %q; want no_signal", plan, reason)
	}
}

// PRD 3.2, 9.5: an exit on the levels as the book holds them. Example 2's
// short buys back at the asks. Bull asks 40.37 x 0.4, 40.38 x 400, 40.39 x
// 300: the tiny best ask is the first level, so the limit is 40.38 with 400.4
// counted, 200.2 offered. Bear asks 59.381 x 40, 59.388 x 30, 59.392 x 50,
// 59.397 x 20, 59.404 x 60 span three 0.01 steps: the limit is the fifth
// price itself, 59.404, with 200 counted, 100 offered.
//
//	x = 10000 x (40.38/40 + 59.404/60 - 2) - 3.3333 = -4.3333 - 3.3333 = -7.6667, past the line at -2
//	K = min(200.2 x 40, 100 x 60, 165.30 x 40, 110.20 x 60) = 6,000: 150 bull, 100 bear
func TestEvaluateExit_RawLevels(t *testing.T) {
	in := exitInput()
	in.Books[cex.Bull] = book(in.Books[cex.Bull].Bids, lv("40.37x0.4", "40.38x400", "40.39x300"))
	in.Books[cex.Bear] = book(in.Books[cex.Bear].Bids, lv("59.381x40", "59.388x30", "59.392x50", "59.397x20", "59.404x60"))
	plan, reason := newStrategy(t, nil).EvaluateExit(in)
	if plan == nil {
		t.Fatalf("no plan: %s", reason)
	}
	wantDec(t, "bull limit", plan.Price[cex.Bull], "40.38")
	wantDec(t, "bear limit", plan.Price[cex.Bear], "59.404")
	wantDec(t, "bull quantity", plan.Qty[cex.Bull], "150")
	wantDec(t, "bear quantity", plan.Qty[cex.Bear], "100")
	wantFloat(t, "x", plan.X, -23.0/3)

	// The five bear asks inside one 0.01 step, 59.391 up to 59.399: the
	// limit is 59.399, not a rounded 59.40, and
	// x = 10000 x (40.38/40 + 59.399/60 - 2) - 3.3333 = -5.1667 - 3.3333 = -8.5.
	in.Books[cex.Bear] = book(in.Books[cex.Bear].Bids, lv("59.391x40", "59.393x30", "59.395x50", "59.397x20", "59.399x60"))
	plan, reason = newStrategy(t, nil).EvaluateExit(in)
	if plan == nil {
		t.Fatalf("one step: no plan: %s", reason)
	}
	wantDec(t, "one step: bear limit", plan.Price[cex.Bear], "59.399")
	wantFloat(t, "one step: x", plan.X, -8.5)

	// The book takes everything that is left when it offers enough: 100
	// offered on the bear leg cover a holding of 70.20.
	in.Hold = [2]decimal.Decimal{dec("105.30"), dec("70.20")}
	plan, reason = newStrategy(t, nil).EvaluateExit(in)
	if plan == nil {
		t.Fatalf("everything: no plan: %s", reason)
	}
	wantDec(t, "everything: bull quantity", plan.Qty[cex.Bull], "105.30")
	wantDec(t, "everything: bear quantity", plan.Qty[cex.Bear], "70.20")
}
