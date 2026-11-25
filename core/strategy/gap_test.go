package strategy

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
)

// gapInput is appendix A example 3: the short entry of example 1 sold 105.30
// SOXL and only 60 SOXS. The gap is 105.30 x 40 - 60 x 60 = 612 USD: trim
// 15.30 SOXL or complete 10.20 SOXS. One second after the batch was sent,
// with new frames on both legs and nothing forbidding a completion.
func gapInput() GapInput {
	return GapInput{
		Direction:   Short,
		Hold:        [2]decimal.Decimal{dec("105.30"), dec("60")},
		PairAnchors: testAnchors,
		Books: [2]*cex.Book{
			book(lv("40.45x300"), lv("40.47x300")),
			book(lv("59.418x200"), lv("59.43x200")),
		},
		HasPrice: [2]bool{true, true},
		DepthOK:  [2]bool{true, true},
		Rules:    testRules,
		Batch: &GapBatch{
			PlanPrice: [2]decimal.Decimal{dec("40.44"), dec("59.42")},
			AvgFill:   [2]decimal.Decimal{dec("40.44"), dec("59.42")},
			SentAt:    t0,
		},
		CanComplete: [2]bool{true, true},
		BookNew:     [2]bool{true, true},
		QuotaLeft:   dec("10000"),
		Now:         t0.Add(time.Second),
	}
}

// wantAction is the expected decision. The order's fields are checked for a
// completion or a trim; the losses only when lossComplete is set.
type wantAction struct {
	kind                   GapKind
	leg                    cex.Leg
	side                   cex.Side
	price, qty             string
	lossComplete, lossTrim string
}

func checkAction(t *testing.T, got GapAction, want wantAction) {
	t.Helper()
	if got.Kind != want.kind {
		t.Fatalf("kind = %s, want %s (action %+v)", got.Kind, want.kind, got)
	}
	if want.kind != GapComplete && want.kind != GapTrim {
		return
	}
	if got.Leg != want.leg || got.Side != want.side {
		t.Errorf("order on %s %s, want %s %s", got.Leg, got.Side, want.leg, want.side)
	}
	wantDec(t, "limit", got.Price, want.price)
	wantDec(t, "quantity", got.Qty, want.qty)
	if want.lossComplete != "" {
		wantDec(t, "loss of completing", got.LossComplete, want.lossComplete)
		wantDec(t, "loss of trimming", got.LossTrim, want.lossTrim)
	}
}

// V9, appendix A examples 3 and 4: both orders can be sent and the smaller
// loss wins, each walked level by level on the raw book.
//
// The trim buys 15.30 SOXL back (sold at 40.44, worth 618.732) and pays the
// entry and exit fees of that quantity; the completion sells 10.20 SOXS
// against the plan's 59.42 (606.084). A trim's limit is the best ask 40.47
// through 10 bps: 40.51047 -> 40.52.
func TestEvaluateGap_AppendixExamples(t *testing.T) {
	tests := []struct {
		name     string
		edit     func(*GapInput)
		fee      float64
		want     wantAction
		wantCent [2]string // complete, trim, to the cent as the appendix prints them
	}{
		{
			// Trim: 619.191 - 618.732 = 0.459, fees 0.65 bps x 1,237.923 = 0.0805.
			// Complete: 606.084 - 10.20 x 59.418 = 0.0204.
			name:     "example 3: complete 0.02 against trim 0.54",
			fee:      0.65,
			want:     wantAction{GapComplete, cex.Bear, cex.SideSell, "59.418", "10.20", "0.0204", "0.539464995"},
			wantCent: [2]string{"0.02", "0.54"},
		},
		{
			// Complete: 10.20 x (59.42 - 59.30) = 1.224.
			name:     "example 3 with bids at 59.30: trim",
			edit:     func(in *GapInput) { in.Books[cex.Bear] = book(lv("59.30x200"), lv("59.43x200")) },
			fee:      0.65,
			want:     wantAction{GapTrim, cex.Bull, cex.SideBuy, "40.52", "15.30", "1.224", "0.539464995"},
			wantCent: [2]string{"1.22", "0.54"},
		},
		{
			// Trim: 0.4 x 40.47 + 14.9 x 40.50 = 619.638, 0.906 worse, fees 0.0805.
			// Complete: 2 x 59.418 + 8.2 x 59.36 = 605.588, 0.496 worse.
			// On the best prices alone the trim would look cheaper.
			name: "example 4: a thin best level, complete 0.50 against trim 0.99",
			edit: func(in *GapInput) {
				in.Books[cex.Bull] = book(lv("40.45x300"), lv("40.47x0.4", "40.50x300"))
				in.Books[cex.Bear] = book(lv("59.418x2", "59.36x200"), lv("59.43x200"))
			},
			fee:      0.65,
			want:     wantAction{GapComplete, cex.Bear, cex.SideSell, "59.36", "10.20", "0.496", "0.98649405"},
			wantCent: [2]string{"0.50", "0.99"},
		},
		{
			// No fee: 15.30 x (40.46 - 40.44) = 10.20 x (59.42 - 59.39) = 0.306.
			// 40.46 x 1.001 = 40.50046 -> 40.51.
			name: "equal losses: trim",
			edit: func(in *GapInput) {
				in.Books[cex.Bull] = book(lv("40.45x300"), lv("40.46x300"))
				in.Books[cex.Bear] = book(lv("59.39x200"), lv("59.43x200"))
			},
			fee:      0,
			want:     wantAction{GapTrim, cex.Bull, cex.SideBuy, "40.51", "15.30", "0.306", "0.306"},
			wantCent: [2]string{"0.31", "0.31"},
		},
		{
			// The entry filled better than its limit: the trim is measured
			// from the fills (15.30 x 40.46 = 619.038), the completion from
			// the plan. Trim: 0.153 + 0.65 bps x 1,238.229 = 0.233484885.
			name:     "the trim is measured from the entry's fills",
			edit:     func(in *GapInput) { in.Batch.AvgFill[cex.Bull] = dec("40.46") },
			fee:      0.65,
			want:     wantAction{GapComplete, cex.Bear, cex.SideSell, "59.418", "10.20", "0.0204", "0.233484885"},
			wantCent: [2]string{"0.02", "0.23"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := gapInput()
			if tc.edit != nil {
				tc.edit(&in)
			}
			got := newStrategy(t, func(c *Config) { c.TakerFeeBps = tc.fee }).EvaluateGap(in)
			checkAction(t, got, tc.want)
			wantDec(t, "loss of completing, to the cent", got.LossComplete.Round(2), tc.wantCent[0])
			wantDec(t, "loss of trimming, to the cent", got.LossTrim.Round(2), tc.wantCent[1])
		})
	}
}

// V9: when a completion is allowed, when only a trim is, and when nothing
// can be done. The base is example 3, where the completion is the cheaper
// order whenever it is allowed.
func TestEvaluateGap_WhichOrder(t *testing.T) {
	trim := wantAction{kind: GapTrim, leg: cex.Bull, side: cex.SideBuy, price: "40.52", qty: "15.30"}
	complete := wantAction{kind: GapComplete, leg: cex.Bear, side: cex.SideSell, price: "59.418", qty: "10.20"}
	tests := []struct {
		name string
		edit func(*GapInput)
		want wantAction
	}{
		{"inside the window", nil, complete},
		// The window runs from the moment the batch was sent, whenever its
		// results arrived.
		{"5 s after the batch was sent: still inside", func(in *GapInput) { in.Now = t0.Add(5 * time.Second) }, complete},
		{"5.001 s after the batch was sent: trim", func(in *GapInput) { in.Now = t0.Add(5001 * time.Millisecond) }, trim},
		{"8 s after the batch was sent: trim", func(in *GapInput) { in.Now = t0.Add(8 * time.Second) }, trim},

		// PRD 8.5: regular hours, a stop signal, the settings, an unusable
		// or conflicting depth on the smaller leg all arrive as CanComplete.
		{"completing the smaller leg is not allowed: trim", func(in *GapInput) { in.CanComplete[cex.Bear] = false }, trim},
		{"not allowed and no new frame: trim, no waiting", func(in *GapInput) { in.CanComplete[cex.Bear], in.BookNew[cex.Bear] = false, false }, trim},
		{"only the larger leg may not be added to: complete", func(in *GapInput) { in.CanComplete[cex.Bull] = false }, complete},

		// PRD 5.9: a completion waits for its own leg's new frame only.
		{"the smaller leg's new frame is missing: wait", func(in *GapInput) { in.BookNew[cex.Bear] = false }, wantAction{kind: GapWait}},
		{"the larger leg's new frame is missing: no waiting", func(in *GapInput) { in.BookNew[cex.Bull] = false }, complete},
		{"no new frame, but the window is over: trim", func(in *GapInput) { in.BookNew[cex.Bear], in.Now = false, t0.Add(6*time.Second) }, trim},

		// A gap from an exit has no batch: it only reduces.
		{"a gap after an exit: trim", func(in *GapInput) { in.Batch = nil }, trim},

		// PRD 8.4: the levels must hold the whole missing quantity.
		{"the bids hold less than the missing quantity: trim", func(in *GapInput) {
			in.Books[cex.Bear] = book(lv("59.418x5", "59.41x5"), lv("59.43x200"))
		}, trim},

		// Only one of the two can be sent.
		{"no price on the larger leg: complete", func(in *GapInput) { in.HasPrice[cex.Bull] = false }, complete},
		{"no price on the larger leg and no completion: blocked", func(in *GapInput) {
			in.HasPrice[cex.Bull], in.CanComplete[cex.Bear] = false, false
		}, wantAction{kind: GapBlocked}},
		{"no price on the larger leg after the window: blocked", func(in *GapInput) {
			in.HasPrice[cex.Bull], in.Now = false, t0.Add(6*time.Second)
		}, wantAction{kind: GapBlocked}},
		{"no price on the smaller leg does not stop a trim", func(in *GapInput) {
			in.HasPrice[cex.Bear], in.CanComplete[cex.Bear] = false, false
		}, trim},
		{"no quota left: trim", func(in *GapInput) { in.QuotaLeft = decimal.Zero }, trim},

		// One leg filled and the other did not: the same two orders, for the
		// whole of it. Completing 70.20 SOXS costs 70.20 x 0.002 = 0.14;
		// trimming 105.30 SOXL costs 105.30 x 0.03 + fees = 3.71.
		{"one leg only: complete it", func(in *GapInput) { in.Hold[cex.Bear] = decimal.Zero },
			wantAction{kind: GapComplete, leg: cex.Bear, side: cex.SideSell, price: "59.418", qty: "70.20"}},
		{"one leg only, after the window: trim all of it", func(in *GapInput) {
			in.Hold[cex.Bear], in.Now = decimal.Zero, t0.Add(6*time.Second)
		}, wantAction{kind: GapTrim, leg: cex.Bull, side: cex.SideBuy, price: "40.52", qty: "105.30"}},

		// Dust: no order can be sent for it, whatever is allowed.
		{"both legs match", func(in *GapInput) { in.Hold[cex.Bear] = dec("70.20") }, wantAction{kind: GapNone}},
		// 0.4 USD: 0.01 SOXL to trim, nothing on the step grid to complete.
		{"a gap of one step", func(in *GapInput) { in.Hold = [2]decimal.Decimal{dec("105.31"), dec("70.20")} }, wantAction{kind: GapNone}},
		// 4 USD: 0.10 SOXL (4.05 USDT) or 0.06 SOXS (3.57 USDT), both under 5 USDT.
		{"a gap under the minimum notional", func(in *GapInput) { in.Hold = [2]decimal.Decimal{dec("105.40"), dec("70.20")} }, wantAction{kind: GapNone}},
		{"dust after an exit", func(in *GapInput) {
			in.Hold, in.Batch = [2]decimal.Decimal{dec("15.40"), dec("10.20")}, nil
		}, wantAction{kind: GapNone}},
		// 8 USD: 0.20 SOXL can be trimmed (8.10 USDT); 0.13 SOXS (7.72 USDT) could be completed.
		{"just over the minimum: handled", func(in *GapInput) { in.Hold = [2]decimal.Decimal{dec("105.50"), dec("70.20")} },
			wantAction{kind: GapComplete, leg: cex.Bear, side: cex.SideSell, price: "59.418", qty: "0.13"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := gapInput()
			if tc.edit != nil {
				tc.edit(&in)
			}
			checkAction(t, newStrategy(t, nil).EvaluateGap(in), tc.want)
		})
	}
}

// V9: the directions. A long cycle that bought 60 SOXL and 70.20 SOXS has
// 4,212 - 2,400 = 1,812 USD too much on the bear leg: trim 30.20 SOXS (a
// sell through the best bid) or complete 45.30 SOXL (a buy at the asks).
func TestEvaluateGap_LongCycle(t *testing.T) {
	base := func() GapInput {
		in := gapInput()
		in.Direction = Long
		in.Hold = [2]decimal.Decimal{dec("60"), dec("70.20")}
		in.Batch.PlanPrice = [2]decimal.Decimal{dec("40.36"), dec("59.42")}
		in.Batch.AvgFill = [2]decimal.Decimal{dec("40.36"), dec("59.42")}
		return in
	}

	// Asks at 40.37: completing costs 45.30 x 0.01 = 0.453. Trimming sells
	// 30.20 at 59.418 for 1,794.4236 against 1,794.484 paid: 0.0604 plus
	// fees 0.65 bps x 3,588.9076 = 0.233278994. The trim is cheaper; its
	// limit is 59.418 x 0.999 = 59.358582 -> 59.358.
	in := base()
	in.Books[cex.Bull] = book(lv("40.35x300"), lv("40.37x300"))
	checkAction(t, newStrategy(t, nil).EvaluateGap(in),
		wantAction{GapTrim, cex.Bear, cex.SideSell, "59.358", "30.20", "0.453", "0.293678994"})

	// Asks at the plan's 40.36: completing costs nothing.
	in = base()
	in.Books[cex.Bull] = book(lv("40.35x300"), lv("40.36x300"))
	checkAction(t, newStrategy(t, nil).EvaluateGap(in),
		wantAction{GapComplete, cex.Bull, cex.SideBuy, "40.36", "45.30", "0", "0.293678994"})

	// Asks better than the plan: the completion's loss is negative.
	in = base()
	in.Books[cex.Bull] = book(lv("40.34x300"), lv("40.35x300"))
	checkAction(t, newStrategy(t, nil).EvaluateGap(in),
		wantAction{GapComplete, cex.Bull, cex.SideBuy, "40.35", "45.30", "-0.453", "0.293678994"})
}

// V9 and PRD 6.4: a completion is capped by the quota left, counted as
// quantity x its limit; only that share of the gap is compared, and the rest
// is trimmed after the completion's result.
func TestEvaluateGap_QuotaCap(t *testing.T) {
	// Example 3 with 300 USDT left: 300 / 59.418 = 5.049 -> 5.04 SOXS. The
	// same share of the trim is 15.30 x 5.04 / 10.20 = 7.56 SOXL.
	// Complete: 5.04 x 0.002 = 0.01008. Trim: 7.56 x 0.03 + 0.65 bps x
	// 7.56 x 80.91 = 0.266559174.
	in := gapInput()
	in.QuotaLeft = dec("300")
	got := newStrategy(t, nil).EvaluateGap(in)
	checkAction(t, got, wantAction{GapComplete, cex.Bear, cex.SideSell, "59.418", "5.04", "0.01008", "0.266559174"})
	if notional := got.Qty.Mul(got.Price); notional.GreaterThan(in.QuotaLeft) {
		t.Errorf("the completion reserves %s, over the quota %s", notional, in.QuotaLeft)
	}

	// After that completion the cycle holds 105.30 / 65.04 and the quota is
	// used up: the remaining 309.6 USD of gap is trimmed, 7.74 SOXL.
	in = gapInput()
	in.Hold[cex.Bear] = dec("65.04")
	in.QuotaLeft = dec("0.53")
	checkAction(t, newStrategy(t, nil).EvaluateGap(in),
		wantAction{kind: GapTrim, leg: cex.Bull, side: cex.SideBuy, price: "40.52", qty: "7.74"})

	// The quota holds less than one order: no completion, trim everything.
	in = gapInput()
	in.QuotaLeft = dec("4")
	checkAction(t, newStrategy(t, nil).EvaluateGap(in),
		wantAction{kind: GapTrim, leg: cex.Bull, side: cex.SideBuy, price: "40.52", qty: "15.30"})
}

// The trim's limit comes from the best price of the newer stream, and its
// loss is estimated on that price when the leg's depth was dropped.
func TestEvaluateGap_TrimWithoutDepth(t *testing.T) {
	in := gapInput()
	in.Books[cex.Bull] = &cex.Book{BestBid: lv("40.45x3")[0], BestAsk: lv("40.47x3")[0]}
	in.DepthOK[cex.Bull] = false
	// The same numbers as example 3: 15.30 at 40.47.
	checkAction(t, newStrategy(t, nil).EvaluateGap(in),
		wantAction{GapComplete, cex.Bear, cex.SideSell, "59.418", "10.20", "0.0204", "0.539464995"})

	in.CanComplete[cex.Bear] = false
	checkAction(t, newStrategy(t, nil).EvaluateGap(in),
		wantAction{kind: GapTrim, leg: cex.Bull, side: cex.SideBuy, price: "40.52", qty: "15.30"})
}

// A book in conflict: the bull leg's depth still shows an ask of 40.47, the
// best-price stream already 40.60 x 300. The trim's limit is built from
// 40.60, so its loss is estimated there too: buying 15.30 back costs
// 15.30 x 0.16 = 2.448 more than they were sold for, plus fees 0.65 bps x
// (618.732 + 621.18) = 0.08059428, 2.52859428 in all. On the stale depth it
// would look like 0.539464995 and win against the completion's
// 10.20 x 0.07 = 0.714; on the visible price the completion is right.
func TestEvaluateGap_TrimDuringBookConflict(t *testing.T) {
	in := gapInput()
	in.Books[cex.Bear] = book(lv("59.35x200"), lv("59.43x200"))
	conflict := book(lv("40.45x300"), lv("40.47x300"))
	conflict.BestAsk, conflict.Conflict = lv("40.60x300")[0], true
	in.Books[cex.Bull] = conflict
	in.DepthOK[cex.Bull] = false
	got := newStrategy(t, nil).EvaluateGap(in)
	checkAction(t, got, wantAction{GapComplete, cex.Bear, cex.SideSell, "59.35", "10.20", "0.714", "2.52859428"})
	wantDec(t, "the completion's quota price: the best bid", got.QuotaPrice, "59.35")

	// The same books taken as agreeing would have trimmed: that is the
	// mistake the book view prevents.
	in.DepthOK[cex.Bull] = true
	checkAction(t, newStrategy(t, nil).EvaluateGap(in),
		wantAction{GapTrim, cex.Bull, cex.SideBuy, "40.65", "15.30", "0.714", "0.539464995"}) // limit 40.60 x 1.001 = 40.6406 -> 40.65

	// When only the trim can be sent, its limit is the same 40.65.
	in.DepthOK[cex.Bull], in.CanComplete[cex.Bear] = false, false
	trim := newStrategy(t, nil).EvaluateGap(in)
	checkAction(t, trim, wantAction{kind: GapTrim, leg: cex.Bull, side: cex.SideBuy, price: "40.65", qty: "15.30"})
	if !trim.QuotaPrice.IsZero() {
		t.Errorf("a trim's quota price = %s, want none: it takes no quota", trim.QuotaPrice)
	}
}

// A buy completion counts against the quota at its limit (PRD 6.4): with
// 200 USDT left and asks 9 x 10, 11 x 10, a long cycle short of 20 bull
// shares completes 18.18 at limit 11.
func TestEvaluateGap_BuyCompletionQuotaPrice(t *testing.T) {
	in := gapInput()
	in.Direction = Long
	in.PairAnchors = anchors("10", "10")
	in.Hold = [2]decimal.Decimal{dec("30"), dec("50")}
	in.Books = [2]*cex.Book{book(lv("8x100"), lv("9x10", "11x10")), book(lv("10x100"), lv("10.1x100"))}
	in.Batch.PlanPrice = [2]decimal.Decimal{dec("9"), dec("10")}
	in.Batch.AvgFill = [2]decimal.Decimal{dec("9"), dec("10")}
	in.HasPrice[cex.Bear] = false // only the completion can be sent
	in.QuotaLeft = dec("200")
	got := newStrategy(t, nil).EvaluateGap(in)
	checkAction(t, got, wantAction{kind: GapComplete, leg: cex.Bull, side: cex.SideBuy, price: "11", qty: "18.18"})
	wantDec(t, "quota price", got.QuotaPrice, "11")
	wantDec(t, "reserved", got.Qty.Mul(got.QuotaPrice), "199.98")
}
