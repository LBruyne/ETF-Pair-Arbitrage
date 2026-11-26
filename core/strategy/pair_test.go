package strategy

import (
	"math"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
)

// seqFeeD is C of SPEC 12.5: the round-trip fee at the decision's limits
// 33.26 / 59.880, in spread units (2.5991).
var seqFeeD = 2 * 0.65 * seqF * (33.26/32.0287 + 59.880/62.1464)

// seqDecisionD is D_dec of SPEC 12.5: -6.58 at 33.26 / 59.880.
var seqDecisionD = seqD(33.26, 59.880)

// ex1Batch is SPEC 12.5 example 1: a long batch whose thin leg filled 5.00
// @ 59.874 of the planned 8.05, nothing paired yet.
func ex1Batch() PairBatch {
	return PairBatch{
		Dir:        Long,
		DecisionD:  seqDecisionD,
		XRef:       seqDecisionD + 2, // x_dec = -4.58
		FeeD:       seqFeeD,
		PlanThin:   dec("8.05"),
		Fills:      Fills{ThinQty: dec("5.00"), ThinVWAP: dec("59.874"), ThickQty: decimal.Zero, ThickVWAP: decimal.Zero},
		ThinClosed: decimal.Zero,
		FirstFill:  t0,
	}
}

// soxsShallow are SOXS books whose fifth ask is the decision's 59.880 (no
// top-up signal) and whose bids are below every break-even sell.
func soxsShallow() *cex.Book {
	return book(lv("59.870x5", "59.868x5", "59.866x5", "59.864x5", "59.862x5"),
		lv("59.876x1", "59.877x1", "59.878x1", "59.879x1", "59.880x1"))
}

// soxsDeep are SOXS books whose fifth ask 59.820 puts x below x_dec: with
// SOXL's second ask 33.29 x = -4.86, with 33.28 x = -7.98. The five asks
// offer 6.08 x 1/2 = 3.04 shares.
func soxsDeep() *cex.Book {
	return book(lv("59.810x5", "59.808x5", "59.806x5", "59.804x5", "59.802x5"),
		lv("59.816x1", "59.817x1", "59.818x1", "59.819x1", "59.820x2.08"))
}

// pairInput is a step of the batch b on the given books, with the whole
// quota and nothing in the way.
func pairInput(b PairBatch, soxl, soxs *cex.Book) PairInput {
	return PairInput{
		Batch:       b,
		Pairing:     seqPairing,
		Books:       [2]*cex.Book{soxl, soxs},
		DepthOK:     [2]bool{true, true},
		Rules:       testRules,
		F:           seqF,
		Anchors:     seqAnchors,
		PairAnchors: seqAnchors,
		Stats:       statsFor(seqAnchors, -2, 3),
		QuotaLeft:   dec("100000"),
		Value:       (&fakeLedger{dir: Long, fee: seqFee}).lot("", "5.00@59.874").valuer(0),
		Now:         t0.Add(time.Second),
	}
}

// The thin order of a batch is the plan's thin leg alone, at its pricing
// level for qS; the batch starts from the decision's spread and x.
func TestThinOrderAndNewPairBatch(t *testing.T) {
	plan, reason := newStrategy(t, nil).EvaluateEntry(entryInput(longBooks()))
	if plan == nil {
		t.Fatalf("no plan: %s", reason)
	}
	o := plan.ThinOrder(cex.Bear)
	if o.Side != cex.SideBuy || !o.Price.Equal(plan.Price[cex.Bear]) || !o.Qty.Equal(plan.Qty[cex.Bear]) {
		t.Errorf("thin order %+v, want buy %s @ %s", o, plan.Qty[cex.Bear], plan.Price[cex.Bear])
	}
	b := NewPairBatch(plan, cex.Bear, dec("30"), dec("59.41"), t0)
	wantFloat(t, "D_dec", b.DecisionD, plan.X+plan.Mu)
	wantFloat(t, "x_ref", b.XRef, plan.X)
	wantFloat(t, "C", b.FeeD, plan.FeeD)
	if b.Dir != Long || !b.PlanThin.Equal(plan.Qty[cex.Bear]) || !b.ThinQty.Equal(dec("30")) || !b.ThinVWAP.Equal(dec("59.41")) ||
		!b.ThickQty.IsZero() || !b.ThinClosed.IsZero() || !b.FirstFill.Equal(t0) || b.Unwind != "" {
		t.Errorf("batch %+v", b)
	}
}

// SPEC 12.5 example 1: P* = 33.2742; the first order's bound is 33.27 and,
// after 6.00 @ 33.27, the bound of the 3.70 left is 33.2811 -> 33.28.
func TestPairBound_Example1(t *testing.T) {
	s := seqStrategy(t, nil)
	in := pairInput(ex1Batch(), book(nil, lv("33.27x6", "33.28x20")), soxsShallow())
	vwapCap, limit, ok := s.PairBound(in, dec("9.70"))
	if !ok {
		t.Fatal("no bound")
	}
	wantNear(t, "P*", vwapCap.InexactFloat64(), 33.274245285, 1e-6)
	wantDec(t, "first bound", limit, "33.27")

	in.Batch.ThickQty, in.Batch.ThickVWAP = dec("6.00"), dec("33.27")
	_, limit, _ = s.PairBound(in, dec("3.70"))
	wantDec(t, "second bound", limit, "33.28") // 33.281129530 rounded down

	// A short batch's bound is a floor: P* mirrors and rounds up. With the
	// same fills sold, D_exit1 = mu + entry_out x sigma = -3.5 and
	// D_need = -3.5 + C.
	in.Batch.Dir = Short
	in.Batch.ThickQty, in.Batch.ThickVWAP = decimal.Zero, decimal.Zero
	vwapCap, limit, _ = s.PairBound(in, dec("9.70"))
	aL, aS := 32.0287/seqF, 62.1464/seqF
	want := aL * (2 + (-2-0.5*3+seqFeeD)/1e4 - 59.874/aS)
	wantNear(t, "short P*", vwapCap.InexactFloat64(), want, 1e-6)
	if !limit.Equal(dec("33.29")) { // 33.28130 rounded up
		t.Errorf("short bound = %s, want 33.29 (P* %.5f rounded up)", limit, want)
	}

	// No mean: no bound.
	in.Stats = Stats{}
	if _, _, ok := s.PairBound(in, dec("9.70")); ok {
		t.Error("a bound without a mean")
	}
}

// thickOrder walks the levels: below the cap any quantity, above it only
// the room cheaper fills left (SPEC 12.5 example 10), floored, at least the
// venue's minimums, ties to the better price, within the quota.
func TestThickOrder(t *testing.T) {
	rule := testRules[cex.Bull]
	none := Fills{ThinQty: dec("5"), ThinVWAP: dec("59.874"), ThickQty: decimal.Zero, ThickVWAP: decimal.Zero}
	big := dec("1000000")
	thinPerThick := dec("30.857561882908747") // (A_L / A_S) x 59.874

	cases := []struct {
		name    string
		dir     Direction
		book    *cex.Book
		vwapCap string
		fills   Fills
		need    string
		quota   decimal.Decimal
		qty, px string // "" for no order
	}{
		{"ex1 first: ask1 within P*", Long, book(nil, lv("33.27x6", "33.28x20")), "33.274245285", none, "9.70", big, "6", "33.27"},
		{"ex1 second: ask 33.28 takes the room 6.00 @ 33.27 left", Long, book(nil, lv("33.28x20", "33.29x20")), "33.274245285",
			Fills{ThinQty: dec("5"), ThinVWAP: dec("59.874"), ThickQty: dec("6"), ThickVWAP: dec("33.27")}, "3.70", big, "3.70", "33.28"},
		{"ex10: 6 @ 33.26 leave room for 6 @ 33.28 under 33.27", Long, book(nil, lv("33.28x20")), "33.27",
			Fills{ThinQty: dec("10"), ThinVWAP: dec("59"), ThickQty: dec("6"), ThickVWAP: dec("33.26")}, "10", big, "6", "33.28"},
		{"ex10: the 4 left wait for an ask within 33.27", Long, book(nil, lv("33.28x20")), "33.27",
			Fills{ThinQty: dec("10"), ThinVWAP: dec("59"), ThickQty: dec("12"), ThickVWAP: dec("33.27")}, "4", big, "", ""},
		{"nothing filled: no room above the cap", Long, book(nil, lv("33.28x20")), "33.274245285", none, "9.70", big, "", ""},
		{"the room is below 5 USDT", Long, book(nil, lv("33.28x20")), "33.27",
			Fills{ThinQty: dec("1"), ThinVWAP: dec("59"), ThickQty: dec("0.10"), ThickVWAP: dec("33.26")}, "1", big, "", ""},
		{"equal quantities: the better price", Long, book(nil, lv("33.25x3", "33.26x5")), "33.30", none, "3", big, "3", "33.25"},
		{"short mirror: bids below the floor take the room", Short, book(lv("33.28x20"), nil), "33.29",
			Fills{ThinQty: dec("10"), ThinVWAP: dec("59"), ThickQty: dec("6"), ThickVWAP: dec("33.30")}, "10", big, "6", "33.28"},
		{"short: the better price on a tie is the higher bid", Short, book(lv("33.31x3", "33.30x5"), nil), "33.20", none, "3", big, "3", "33.31"},
		{"the quota holds 4.99 pairs at 33.27 + 30.8576", Long, book(nil, lv("33.27x20")), "33.30", none, "9.70", dec("320"), "4.99", "33.27"},
	}
	for _, c := range cases {
		qty, limit, quotaAt, ok := thickOrder(c.book, c.dir, dec(c.vwapCap), c.fills, dec(c.need), rule, c.quota, thinPerThick)
		if c.qty == "" {
			if ok {
				t.Errorf("%s: order %s @ %s, want none", c.name, qty, limit)
			}
			continue
		}
		if !ok || !qty.Equal(dec(c.qty)) || !limit.Equal(dec(c.px)) {
			t.Errorf("%s: order %s @ %s (ok %v), want %s @ %s", c.name, qty, limit, ok, c.qty, c.px)
		}
		if c.dir == Long && !quotaAt.Equal(limit) {
			t.Errorf("%s: quota price %s, want the limit %s", c.name, quotaAt, limit)
		}
	}
}

// SPEC 12.5 example 1's stop line: with nothing paired D_ideal reaches
// D_dec + 10 = 3.42 at a second ask of 33.2952; after 6.00 @ 33.27 the rest
// at 33.28 gives -3.23.
func TestIdealSpread(t *testing.T) {
	s := seqStrategy(t, nil)
	fills := Fills{ThinQty: dec("5"), ThinVWAP: dec("59.874"), ThickQty: decimal.Zero, ThickVWAP: decimal.Zero}
	for _, c := range []struct {
		ask2 string
		want float64
	}{{"33.28", -1.304770711}, {"33.29", 1.813323373}, {"33.30", 4.931417458}} {
		d, ok := s.IdealSpread(Long, cex.Bear, fills, book(nil, lv("33.27x1", c.ask2+"x1")), seqF, seqAnchors, seqAnchors)
		if !ok {
			t.Fatalf("ask2 %s: no ideal spread", c.ask2)
		}
		wantNear(t, "D_ideal at ask2 "+c.ask2, d, c.want, 1e-6)
	}
	fills.ThickQty, fills.ThickVWAP = dec("6"), dec("33.27")
	d, _ := s.IdealSpread(Long, cex.Bear, fills, book(nil, lv("33.27x1", "33.28x1")), seqF, seqAnchors, seqAnchors)
	wantNear(t, "D_ideal after 6.00 @ 33.27", d, -3.233155943, 1e-6)

	// The exit side (SPEC 12.12 item 1): SOXS 8.04 closed @ 60.02, SOXL 6.00
	// closed @ 33.22, the rest at the second bid 33.20.
	exit := Fills{ThinQty: dec("8.04"), ThinVWAP: dec("60.02"), ThickQty: dec("6"), ThickVWAP: dec("33.22")}
	d, _ = s.IdealSpread(Short, cex.Bear, exit, book(lv("33.21x1", "33.20x1"), nil), seqF, seqAnchors, seqAnchors)
	wantNear(t, "ideal closing spread", d, -0.389013965, 1e-6)

	// No pricing level on the thick leg, or no thin fill: no number.
	if _, ok := s.IdealSpread(Long, cex.Bear, fills, book(nil, lv("33.27x1")), seqF, seqAnchors, seqAnchors); ok {
		t.Error("an ideal spread without the thick leg's pricing level")
	}
	if _, ok := s.IdealSpread(Long, cex.Bear, Fills{ThinQty: decimal.Zero, ThinVWAP: decimal.Zero, ThickQty: decimal.Zero, ThickVWAP: decimal.Zero},
		book(nil, lv("33.27x1", "33.28x1")), seqF, seqAnchors, seqAnchors); ok {
		t.Error("an ideal spread without a thin fill")
	}
}

// PairStep's checks in the order of SPEC 12.4 version 4: each row makes the
// earlier checks pass and shows the next one.
func TestPairStep_Order(t *testing.T) {
	s := seqStrategy(t, nil)
	ex1 := func() PairInput {
		return pairInput(ex1Batch(), book(lv("33.25x20", "33.24x20"), lv("33.27x6", "33.28x20")), soxsShallow())
	}

	// 1. In flight: wait, whatever else holds.
	in := ex1()
	in.OrderPending, in.Stopping, in.Now = true, true, t0.Add(time.Minute)
	if a := s.PairStep(in); a.Kind != PairWait || a.Why != ReasonAwaitOrders {
		t.Errorf("pending: %s %s, want wait await_orders", a.Kind, a.Why)
	}

	// 2. Stop and forced exit take the round over.
	in.OrderPending = false
	if a := s.PairStep(in); a.Kind != PairHandOver || a.Why != ReasonStopping {
		t.Errorf("stopping: %s %s, want hand_over stopping", a.Kind, a.Why)
	}
	in.Stopping, in.Forced = false, true
	if a := s.PairStep(in); a.Kind != PairHandOver || a.Why != ReasonForced {
		t.Errorf("forced: %s %s, want hand_over forced", a.Kind, a.Why)
	}

	// 3. Paired: done as built, although the window has passed.
	in.Forced = false
	in.Batch.ThickQty, in.Batch.ThickVWAP = dec("9.70"), dec("33.27")
	if a := s.PairStep(in); a.Kind != PairDone || a.End != PairEndBuilt {
		t.Errorf("paired: %s %s, want done built", a.Kind, a.End)
	}

	// 4. The window: unwind S_open at PRD 10.4, through the best bid 59.870
	// by 10 bps, rounded down.
	in.Batch.ThickQty, in.Batch.ThickVWAP = decimal.Zero, decimal.Zero
	in.Now = t0.Add(30 * time.Second)
	a := s.PairStep(in)
	if a.Kind != PairUnwind || a.Why != ReasonWindow || a.Leg != cex.Bear || a.Side != cex.SideSell || !a.Qty.Equal(dec("5")) || !a.Price.Equal(dec("59.810")) {
		t.Errorf("window: %+v, want unwind window sell 5 @ 59.810", a)
	}
	in.Now = t0.Add(30*time.Second - time.Millisecond)
	if a := s.PairStep(in); a.Kind != PairSendThick {
		t.Errorf("just inside the window: %s, want send_thick", a.Kind)
	}

	// 5. The spread stop: a second ask of 33.30 puts D_ideal at 4.93, 11.5
	// above D_dec; 33.29 (1.81, 8.4 above) does not.
	in.Books[cex.Bull] = book(lv("33.25x20"), lv("33.29x20", "33.30x20"))
	a = s.PairStep(in)
	if a.Kind != PairUnwind || a.Why != ReasonStopD {
		t.Errorf("stop: %s %s, want unwind stop_d", a.Kind, a.Why)
	}
	wantNear(t, "D_ideal", a.DIdeal, 4.931417458, 1e-6)
	in.Books[cex.Bull] = book(lv("33.25x20"), lv("33.28x20", "33.29x20"))
	if a := s.PairStep(in); a.Kind != PairWait || a.Why != ReasonPairBound {
		t.Errorf("below the stop, beyond P*: %s %s, want wait pair_bound", a.Kind, a.Why)
	}

	// 6. The thick order of example 1: 6.00 @ 33.27, with its detail.
	in = ex1()
	a = s.PairStep(in)
	if a.Kind != PairSendThick || a.Leg != cex.Bull || a.Side != cex.SideBuy || !a.Qty.Equal(dec("6")) || !a.Price.Equal(dec("33.27")) || !a.QuotaPrice.Equal(dec("33.27")) {
		t.Errorf("ex1: %+v, want send_thick buy 6 @ 33.27", a)
	}
	wantNear(t, "P*", a.VWAPCap.InexactFloat64(), 33.274245285, 1e-6)
	wantDec(t, "p*", a.PStar, "33.27")
	wantNear(t, "x", a.X, 1.659421905, 1e-6)
	wantNear(t, "x_ref", a.XRef, seqDecisionD+2, 1e-9)
	wantNear(t, "D_ideal", a.DIdeal, -1.304770711, 1e-6)

	// Example 1's second order, after 6.00 @ 33.27: 3.70 @ 33.28.
	in.Batch.ThickQty, in.Batch.ThickVWAP = dec("6"), dec("33.27")
	in.Books[cex.Bull] = book(lv("33.25x20"), lv("33.28x20", "33.29x20"))
	if a := s.PairStep(in); a.Kind != PairSendThick || !a.Qty.Equal(dec("3.70")) || !a.Price.Equal(dec("33.28")) {
		t.Errorf("ex1 second: %s %s @ %s, want send_thick 3.70 @ 33.28", a.Kind, a.Qty, a.Price)
	}
}

// Counterexample 1 of the second audit: paired and a deeper deviation in
// the same frame end the batch as built; no top-up.
func TestPairStep_PairedBeforeTopUp(t *testing.T) {
	s := seqStrategy(t, nil)
	b := ex1Batch()
	b.ThickQty, b.ThickVWAP = dec("9.70"), dec("33.27")
	a := s.PairStep(pairInput(b, book(lv("33.25x20"), lv("33.28x20", "33.29x20")), soxsDeep()))
	if a.Kind != PairDone || a.End != PairEndBuilt {
		t.Errorf("%s %s, want done built", a.Kind, a.End)
	}
}

// SPEC 12.5 example 2: SOXL beyond P*, x deeper than x_ref and at the entry
// line: a top-up of what the book offers, 3.04 @ 59.820; x_ref then moves to
// its x and an equal x does not top up again. Example 2's P* after the
// top-up: 33.277.
func TestPairStep_TopUp(t *testing.T) {
	s := seqStrategy(t, nil)
	soxl := book(lv("33.25x20"), lv("33.28x20", "33.29x20")) // x = -4.86 with the deep SOXS books
	in := pairInput(ex1Batch(), soxl, soxsDeep())
	a := s.PairStep(in)
	if a.Kind != PairSendThin || a.Leg != cex.Bear || a.Side != cex.SideBuy || !a.Qty.Equal(dec("3.04")) || !a.Price.Equal(dec("59.820")) || !a.QuotaPrice.Equal(dec("59.820")) {
		t.Fatalf("top-up: %+v, want send_thin buy 3.04 @ 59.820", a)
	}
	wantNear(t, "x", a.X, -4.864410172, 1e-6)

	// x_ref moves to the top-up's x: the same x does not top up again.
	in.Batch.XRef = a.X
	if a := s.PairStep(in); a.Kind != PairWait {
		t.Errorf("x equal to x_ref: %s, want wait", a.Kind)
	}

	// Example 2's batch after the top-up: S 8.04 @ 59.869, P* 33.277.
	in.Batch.ThinQty, in.Batch.ThinVWAP = dec("8.04"), dec("59.868706467661696")
	vwapCap, _, _ := s.PairBound(in, dec("15.60"))
	wantNear(t, "P* after the top-up", vwapCap.InexactFloat64(), 33.276973439, 1e-6)
}

// The top-up's caps: 2 x qS, the signal that stops new entries, and the
// quota of the full pairing with the existing unpaired part at its actual
// cost (SPEC 12.5 example 6).
func TestPairStep_TopUpCaps(t *testing.T) {
	s := seqStrategy(t, nil)
	soxl := book(lv("33.25x20"), lv("33.28x20", "33.29x20"))

	in := pairInput(ex1Batch(), soxl, soxsDeep())
	in.Batch.ThinQty = dec("15.00") // 16.10 - 15.00 = 1.10 left of 2 x qS
	if a := s.PairStep(in); a.Kind != PairSendThin || !a.Qty.Equal(dec("1.10")) {
		t.Errorf("near 2 x qS: %s %s, want send_thin 1.10", a.Kind, a.Qty)
	}
	in.Batch.ThinQty = dec("16.10")
	if a := s.PairStep(in); a.Kind != PairWait {
		t.Errorf("at 2 x qS: %s, want wait", a.Kind)
	}
	in.Batch.ThinQty = dec("5.00")
	in.NoNewEntries = true
	if a := s.PairStep(in); a.Kind != PairWait {
		t.Errorf("no new entries: %s, want wait", a.Kind)
	}

	// The quota: S_open 5.00 needs 5 x 59.874 + 9.70 x 33.29 = 622.283 to be
	// paired; with 300 more left one more SOXS share takes 59.820 +
	// 1.94033 x 33.29 = 124.414, so 2.41 shares, not the book's 3.04.
	in.NoNewEntries = false
	in.QuotaLeft = dec("922.283")
	if a := s.PairStep(in); a.Kind != PairSendThin || !a.Qty.Equal(dec("2.41")) {
		t.Errorf("quota: %s %s, want send_thin 2.41", a.Kind, a.Qty)
	}

	// Example 6 to the digit: 1,000 left, 700 for the existing part,
	// n1 = 59.870 + 1.94032 x 33.27 = 124.425: 2.41.
	ratio := dec("62.1464").Div(dec("32.0287"))
	wantDec(t, "example 6", topUpQuotaCap(dec("1000"), dec("700"), dec("59.870"), dec("33.27"), ratio, dec("0.01")), "2.41")
	wantDec(t, "no quota left", topUpQuotaCap(dec("600"), dec("700"), dec("59.870"), dec("33.27"), ratio, dec("0.01")), "0")
}

// SPEC 12.5 example 3: x back inside the entry line, nothing paired. The
// break-even sell 59.8818 rounds up to 59.882, above the fifth bid 59.870,
// so the limit is 59.882 and only the 2.00 bid at or above it are sold. With
// a margin of 0.5 units the limit is 59.885 and 1.20 are sold.
func TestPairStep_ProfitClose(t *testing.T) {
	soxl := book(lv("33.25x20"), lv("33.28x20", "33.29x20")) // beyond P*, below the stop
	soxs := book(lv("59.885x1.20", "59.883x0.80", "59.881x1.50", "59.876x2.0", "59.870x3.0"),
		lv("59.876x1", "59.877x1", "59.878x1", "59.879x1", "59.880x1")) // x = +4.77
	for _, c := range []struct {
		margin          float64
		qty, price, beP string
	}{{0, "2", "59.882", "59.882"}, {0.5, "1.2", "59.885", "59.885"}} {
		s := seqStrategy(t, func(cfg *Config) { cfg.CostMarginD = c.margin })
		a := s.PairStep(pairInput(ex1Batch(), soxl, soxs))
		if a.Kind != PairCloseThin || a.Why != ReasonProfit || a.Leg != cex.Bear || a.Side != cex.SideSell || !a.Qty.Equal(dec(c.qty)) || !a.Price.Equal(dec(c.price)) {
			t.Errorf("margin %v: %+v, want close_thin profit sell %s @ %s", c.margin, a, c.qty, c.price)
		}
		wantDec(t, "b*", a.BreakEven, c.beP)
	}
}

// SPEC 12.5 example 11: an older lot of 5 SOXS @ 59.00 is paired, the new
// batch's 5 @ 60.00 is not, the best bid is 59.80. Round FIFO would close
// the old lot at a profit; the trim of this lot loses 1.039, so its
// break-even sell is 60.0078 -> 60.008 and no bid reaches it.
func TestPairStep_ProfitCloseThisLotOnly(t *testing.T) {
	s := seqStrategy(t, nil)
	b := ex1Batch()
	b.ThinVWAP = dec("60.00")
	b.DecisionD, b.XRef = seqDecisionD, seqDecisionD+2
	ledger := (&fakeLedger{dir: Long, fee: seqFee}).lot("9.70@33.0", "5@59.00").lot("", "5@60.00")
	soxl := book(lv("33.20x20"), lv("33.21x10", "33.22x10")) // beyond P* 33.2093; D_ideal 6.8 above D_dec
	soxs := book(lv("59.80x10", "59.79x10", "59.78x10", "59.77x10", "59.76x10"),
		lv("60.006x1", "60.007x1", "60.008x1", "60.009x1", "60.010x1")) // x = +3.84: inside the entry line
	in := pairInput(b, soxl, soxs)
	in.Value = ledger.valuer(1)

	// The valuation itself, as the inventory test pins it.
	net, _ := in.Value(cex.PurposeTrim, [2]CloseLeg{{Qty: decimal.Zero, Price: decimal.Zero}, {Qty: dec("5"), Price: dec("59.80")}}, seqFee)
	wantRound(t, "CloseNet(trim, new lot)", net, "-1.039", 3)

	a := s.PairStep(in)
	if a.Kind == PairCloseThin {
		t.Fatalf("profit close %s @ %s of a losing lot", a.Qty, a.Price)
	}
	if a.Kind != PairWait {
		t.Errorf("%s %s, want wait", a.Kind, a.Why)
	}
	wantDec(t, "b*", a.BreakEven, "60.008")
}

// Counterexample 2 of the second audit and SPEC 12.5 example 4: SOXS filled
// 0.07; its 0.13 SOXL (4.33 U) and its own close (4.19 U) are below 5 U. It
// waits while a later frame may still top up, tops up when x deepens, and
// ends as dust only once the batch can top up no more or the window passes.
func TestPairStep_TooSmall(t *testing.T) {
	s := seqStrategy(t, nil)
	b := ex1Batch()
	b.ThinQty = dec("0.07")
	soxl := book(lv("33.25x20"), lv("33.27x6", "33.28x20"))

	// x not deeper: wait, not done.
	in := pairInput(b, soxl, soxsShallow())
	if a := s.PairStep(in); a.Kind != PairWait || a.Why != ReasonTooSmall {
		t.Errorf("too small, x not deeper: %s %s, want wait too_small", a.Kind, a.Why)
	}
	// x deeper (-7.98): top up.
	in.Books[cex.Bear] = soxsDeep()
	if a := s.PairStep(in); a.Kind != PairSendThin || !a.Qty.Equal(dec("3.04")) {
		t.Errorf("too small, x deeper: %s %s, want send_thin 3.04", a.Kind, a.Qty)
	}
	// No more top-ups: dust that can never pair ends the batch.
	in.NoNewEntries = true
	if a := s.PairStep(in); a.Kind != PairDone || a.End != PairEndBuilt {
		t.Errorf("too small, no more top-ups: %s %s, want done built", a.Kind, a.End)
	}
	// The window passes: the unwind cannot be sent either, so done.
	in.NoNewEntries = false
	in.Now = t0.Add(31 * time.Second)
	if a := s.PairStep(in); a.Kind != PairDone || a.End != PairEndBuilt {
		t.Errorf("too small, window: %s %s, want done built", a.Kind, a.End)
	}

	// A sendable thin remainder whose thick target is too small (SOXL's
	// minimum notional raised to 10 U: 0.19 x 33.27 = 6.32 U) is unwound as
	// too small once the batch can top up no more.
	in = pairInput(b, soxl, soxsShallow())
	in.Batch.ThinQty = dec("0.10")
	in.Rules[cex.Bull].MinNotional = dec("10")
	if a := s.PairStep(in); a.Kind != PairWait || a.Why != ReasonTooSmall {
		t.Errorf("sendable thin, may top up: %s %s, want wait too_small", a.Kind, a.Why)
	}
	in.NoNewEntries = true
	if a := s.PairStep(in); a.Kind != PairUnwind || a.Why != ReasonTooSmall || !a.Qty.Equal(dec("0.10")) || !a.Price.Equal(dec("59.810")) {
		t.Errorf("sendable thin, no more top-ups: %+v, want unwind too_small 0.10 @ 59.810", a)
	}
}

// An unwind in progress goes on with what is left unpaired, whatever the
// book; the batch ends when the rest is flat or dust, in the state its fills
// say.
func TestPairStep_UnwindAndEnd(t *testing.T) {
	s := seqStrategy(t, nil)
	in := pairInput(ex1Batch(), book(lv("33.25x20"), lv("33.27x6", "33.28x20")), soxsShallow())
	in.Batch.Unwind, in.Batch.ThinClosed = ReasonStopD, dec("2")
	if a := s.PairStep(in); a.Kind != PairUnwind || a.Why != ReasonStopD || !a.Qty.Equal(dec("3")) {
		t.Errorf("unwinding: %s %s %s, want unwind stop_d 3", a.Kind, a.Why, a.Qty)
	}

	for _, c := range []struct {
		name          string
		thick, closed string
		unwind        Reason
		end           PairEnd
	}{
		{"nothing paired, unwound", "0", "5", ReasonWindow, PairEndUnwoundStop},
		{"nothing paired, closed at a profit", "0", "5", "", PairEndUnwoundProfit},
		{"part paired, the rest unwound", "4.85", "2.50", ReasonStopD, PairEndBuiltPartial},
		{"part paired, the rest closed at a profit", "4.85", "2.50", "", PairEndBuiltPartial},
		{"paired whole", "9.70", "0", "", PairEndBuilt},
	} {
		in.Batch.ThickQty, in.Batch.ThickVWAP, in.Batch.ThinClosed, in.Batch.Unwind = dec(c.thick), dec("33.27"), dec(c.closed), c.unwind
		if a := s.PairStep(in); a.Kind != PairDone || a.End != c.end {
			t.Errorf("%s: %s %s, want done %s", c.name, a.Kind, a.End, c.end)
		}
	}
}

// Without a mean the thick order and the top-up have no bound: the batch
// waits, and the window still ends it.
func TestPairStep_NoMean(t *testing.T) {
	s := seqStrategy(t, nil)
	in := pairInput(ex1Batch(), book(lv("33.25x20"), lv("33.27x6", "33.28x20")), soxsDeep())
	in.Stats = Stats{Ready: true, MeanBullUSD: math.NaN()}
	if a := s.PairStep(in); a.Kind != PairWait {
		t.Errorf("no mean: %s, want wait", a.Kind)
	}
	in.Now = t0.Add(time.Minute)
	if a := s.PairStep(in); a.Kind != PairUnwind || a.Why != ReasonWindow {
		t.Errorf("no mean, window: %s %s, want unwind window", a.Kind, a.Why)
	}
}

// SPEC 12.5 example 5 and PRD 5.8: a batch that ended entry_failed is the
// last entry (its x_dec -4.58 here as -10 on the appendix books): the next
// one in its direction waits 60 s or 1 unit more.
func TestEvaluateEntry_SpacingAfterEntryFailed(t *testing.T) {
	s := newStrategy(t, nil)
	in := entryInput(longBooks()) // x = -10
	in.Last = &LastEntry{At: t0, E: -9.5, Direction: Long}
	in.Now = t0.Add(30 * time.Second)
	if _, reason := s.EvaluateEntry(in); reason != ReasonSpacing {
		t.Errorf("0.5 deeper after 30 s: %q, want spacing", reason)
	}
	in.Last.E = -8.9
	if plan, reason := s.EvaluateEntry(in); plan == nil {
		t.Errorf("1.1 deeper: %q, want a plan", reason)
	}
	in.Last.E, in.Now = -9.5, t0.Add(60*time.Second)
	if plan, reason := s.EvaluateEntry(in); plan == nil {
		t.Errorf("after 60 s: %q, want a plan", reason)
	}
}
