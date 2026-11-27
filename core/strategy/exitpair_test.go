package strategy

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
)

// ex7Ledger is SPEC 12.7 example 7's position: a long of SOXL 15.60 @ 33.26
// and SOXS 8.04 @ 59.869, nothing realised.
func ex7Ledger() *fakeLedger {
	return (&fakeLedger{dir: Long, fee: seqFee}).lot("15.60@33.26", "8.04@59.869")
}

// ex7Books are the bids of example 7 at exit1: SOXL's second bid 33.24 and
// SOXS's fifth 59.96 (D = 0.04, x = 2.04: past exit1 at 1.5, short of exit2
// at 3), deep enough to close the whole position in one batch.
func ex7Books(soxlBid2 string) [2]*cex.Book {
	return [2]*cex.Book{
		book(lv("33.25x20", soxlBid2+"x20"), lv("33.30x20", "33.31x20")),
		book(lv("59.99x5", "59.98x5", "59.97x5", "59.965x5", "59.96x5"), lv("60.05x5", "60.06x5", "60.07x5", "60.08x5", "60.09x5")),
	}
}

func seqExit2(c *Config) { x := -1.0; c.Exit2 = &x }

func ex7ExitInput(ledger *fakeLedger) ExitInput {
	return ExitInput{
		Direction:   Long,
		Hold:        [2]decimal.Decimal{dec("15.60"), dec("8.04")},
		Books:       ex7Books("33.24"),
		DepthOK:     [2]bool{true, true},
		Rules:       testRules,
		F:           seqF,
		Anchors:     seqAnchors,
		PairAnchors: seqAnchors,
		Stats:       statsFor(seqAnchors, -2, 3),
		Pairing:     seqPairing,
		BuiltLots:   1,
		Value:       ledger.valuer(-1),
	}
}

// SPEC 12.7 example 7 at exit1: closing the whole round makes +0.2896, so H
// is set at SOXS 4.02 / SOXL 7.80 (half the thin leg, the other leg by the
// pairing anchors), and the batch of that half pre-checks at +0.1448.
func TestDecideExit_Exit1SetsHalf(t *testing.T) {
	s := seqStrategy(t, seqExit2)
	d := s.DecideExit(ex7ExitInput(ex7Ledger()))
	if d.Plan == nil {
		t.Fatalf("no plan: %s", d.Reason)
	}
	if d.Stage == nil || !d.Stage.Set || d.Stage.BuiltLots != 1 {
		t.Fatalf("stage %+v, want one set at 1 built lot", d.Stage)
	}
	wantDec(t, "H SOXS", d.Stage.Target[cex.Bear], "4.02")
	wantDec(t, "H SOXL", d.Stage.Target[cex.Bull], "7.80")
	if !d.Stage.Closed[cex.Bull].IsZero() || !d.Stage.Closed[cex.Bear].IsZero() {
		t.Errorf("a new stage has closed %v", d.Stage.Closed)
	}
	p := d.Plan
	if p.Line != ExitLine1 || p.Side != cex.SideSell || p.D1 {
		t.Errorf("plan line %s side %s D1 %v, want exit1 sell without D1", p.Line, p.Side, p.D1)
	}
	wantDec(t, "SOXS", p.Qty[cex.Bear], "4.02")
	wantDec(t, "SOXL", p.Qty[cex.Bull], "7.80")
	wantDec(t, "SOXS limit", p.Price[cex.Bear], "59.96")
	wantDec(t, "SOXL limit", p.Price[cex.Bull], "33.24")
	wantRound(t, "expected net", p.ExpectedNet, "0.1448", 4)
	wantNear(t, "D", p.D, seqD(33.24, 59.96), 1e-6)
}

// The half target in batches (SPEC 12.6): what is left of H is the batch;
// H reached is not closed a second time; a batch built after H was set
// voids it and the next exit1 decides again.
func TestDecideExit_HalfStage(t *testing.T) {
	s := seqStrategy(t, seqExit2)
	h := ExitStage{Set: true, Target: [2]decimal.Decimal{dec("7.80"), dec("4.02")}, BuiltLots: 1}

	in := ex7ExitInput(ex7Ledger())
	in.Stage = h
	in.Stage.Closed = [2]decimal.Decimal{dec("3.88"), dec("2.00")}
	d := s.DecideExit(in)
	if d.Plan == nil || d.Stage != nil {
		t.Fatalf("part closed: plan %v stage %v (%s), want a plan and no new stage", d.Plan, d.Stage, d.Reason)
	}
	wantDec(t, "SOXS left of H", d.Plan.Qty[cex.Bear], "2.02")
	wantDec(t, "SOXL left of H", d.Plan.Qty[cex.Bull], "3.92")

	in.Stage.Closed = [2]decimal.Decimal{dec("7.80"), dec("4.02")}
	if d := s.DecideExit(in); d.Plan != nil || d.Reason != ReasonHalfDone {
		t.Errorf("H reached: plan %v reason %q, want none half_done", d.Plan, d.Reason)
	}

	in.BuiltLots = 2 // a new batch was built: H is void, exit1 decides again
	d = s.DecideExit(in)
	if d.Plan == nil || d.Stage == nil || d.Stage.BuiltLots != 2 {
		t.Fatalf("after a new lot: plan %v stage %+v (%s), want a new stage", d.Plan, d.Stage, d.Reason)
	}
	wantDec(t, "new H SOXS", d.Stage.Target[cex.Bear], "4.02")
}

// exit1 without profit sets no target; with the whole round in profit but
// this batch short of the margin, the target is set and the batch waits.
func TestDecideExit_Exit1NoProfitAndPrecheck(t *testing.T) {
	s := seqStrategy(t, seqExit2)
	in := ex7ExitInput(ex7Ledger())
	in.Value = fixedValuer("0")
	if d := s.DecideExit(in); d.Plan != nil || d.Stage != nil || d.Reason != ReasonNoProfit {
		t.Errorf("zero profit: plan %v stage %v reason %q, want no_profit", d.Plan, d.Stage, d.Reason)
	}

	// The whole round (SOXS 8.04) makes 0.1, the half batch -0.01.
	in.Value = func(_ cex.OrderPurpose, c [2]CloseLeg, _ decimal.Decimal) (decimal.Decimal, error) {
		if c[cex.Bear].Qty.Equal(dec("8.04")) {
			return dec("0.1"), nil
		}
		return dec("-0.01"), nil
	}
	d := s.DecideExit(in)
	if d.Plan != nil || d.Reason != ReasonPrecheck || d.Stage == nil {
		t.Errorf("batch short of the margin: plan %v stage %v reason %q, want precheck with a stage", d.Plan, d.Stage, d.Reason)
	}
}

// SPEC 12.7 example 9: the mean moved to -6.5 (exit1 at D -5.0, exit2 at
// -3.5). At exit1 the round loses 0.0751: no target. At exit2 a batch that
// loses 0.0025 exits all the same (D1) with SOXL's pricing-level price as
// its protection; at +0.0201 it exits as usual.
func TestDecideExit_Example9(t *testing.T) {
	s := seqStrategy(t, seqExit2)
	in := ex7ExitInput(ex7Ledger())
	in.Stats = statsFor(seqAnchors, -6.5, 3)
	in.Books = [2]*cex.Book{
		book(lv("33.24x20", "33.23x20"), lv("33.30x20", "33.31x20")),
		book(lv("59.99x5", "59.98x5", "59.97x5", "59.96x5", "59.95x5"), lv("60.05x5", "60.06x5", "60.07x5", "60.08x5", "60.09x5")),
	} // D = -4.68: past exit1, short of exit2

	in.Value = fixedValuer("-0.0751")
	if d := s.DecideExit(in); d.Plan != nil || d.Reason != ReasonNoProfit {
		t.Errorf("exit1: plan %v reason %q, want no_profit", d.Plan, d.Reason)
	}

	in.Books[cex.Bear] = book(lv("60.00x5", "59.99x5", "59.98x5", "59.97x5", "59.96x5"), lv("60.05x5", "60.06x5", "60.07x5", "60.08x5", "60.09x5")) // D = -3.07: past exit2
	in.Value = fixedValuer("-0.0025")
	d := s.DecideExit(in)
	if d.Plan == nil || d.Plan.Line != ExitLine2 || !d.Plan.D1 {
		t.Fatalf("exit2 losing: %+v (%s), want an exit2 plan with D1", d.Plan, d.Reason)
	}
	wantDec(t, "protection", d.Plan.Protect, "33.23")
	wantDec(t, "expected net", d.Plan.ExpectedNet, "-0.0025")
	wantDec(t, "SOXS", d.Plan.Qty[cex.Bear], "8.04") // all that is left

	in.Value = fixedValuer("0.0201")
	if d := s.DecideExit(in); d.Plan == nil || d.Plan.D1 || !d.Plan.ExpectedNet.Equal(dec("0.0201")) {
		t.Errorf("exit2 in profit: %+v, want a plan without D1", d.Plan)
	}
}

// Past both lines in one frame is exit2: all that is left, no half target.
func TestDecideExit_BothLinesInOneFrame(t *testing.T) {
	s := seqStrategy(t, seqExit2)
	in := ex7ExitInput(ex7Ledger())
	in.Books = ex7Books("33.26") // x = 8.28
	d := s.DecideExit(in)
	if d.Plan == nil || d.Plan.Line != ExitLine2 || d.Stage != nil || d.Plan.D1 {
		t.Fatalf("%+v stage %v (%s), want exit2 without a stage", d.Plan, d.Stage, d.Reason)
	}
	wantDec(t, "SOXS", d.Plan.Qty[cex.Bear], "8.04")
	wantDec(t, "SOXL", d.Plan.Qty[cex.Bull], "15.60")
	wantRound(t, "expected net", d.Plan.ExpectedNet, "0.6016", 4)
}

// thin_leg "" with exit2 (replay group 2, SPEC 12.12 item 2): exit1's half
// is pre-checked; exit2 sends at a loss too and values nothing.
func TestDecideExit_BothLegsTogetherWithExit2(t *testing.T) {
	s := seqStrategy(t, seqExit2)
	in := ex7ExitInput(ex7Ledger())
	in.Pairing = Pairing{}
	in.Books = ex7Books("33.26")
	in.Value = failValuer(t)
	d := s.DecideExit(in)
	if d.Plan == nil || d.Plan.Line != ExitLine2 || d.Plan.D1 || !d.Plan.ExpectedNet.IsZero() {
		t.Errorf("exit2: %+v (%s), want a plan, nothing valued", d.Plan, d.Reason)
	}

	in.Books = ex7Books("33.24")
	in.Value = ex7Ledger().valuer(-1)
	d = s.DecideExit(in)
	if d.Plan == nil || d.Plan.Line != ExitLine1 || d.Stage == nil {
		t.Fatalf("exit1: %+v stage %v (%s), want the half", d.Plan, d.Stage, d.Reason)
	}
	wantRound(t, "expected net", d.Plan.ExpectedNet, "0.1448", 4)
}

// Without exit2 and without a thin leg nothing is valued and the plan is
// EvaluateExit's (PRD 3.10's one line).
func TestDecideExit_OneLineBothLegs(t *testing.T) {
	s := seqStrategy(t, nil)
	in := ex7ExitInput(ex7Ledger())
	in.Pairing, in.Value = Pairing{}, failValuer(t)
	d := s.DecideExit(in)
	plan, reason := s.EvaluateExit(in)
	if d.Plan == nil || plan == nil || reason != d.Reason {
		t.Fatalf("DecideExit %v %q, EvaluateExit %v %q", d.Plan, d.Reason, plan, reason)
	}
	if d.Plan.Line != ExitLineOne || d.Plan.D1 || !d.Plan.ExpectedNet.IsZero() || d.Stage != nil {
		t.Errorf("%+v, want the one line, nothing valued", d.Plan)
	}
	wantDec(t, "SOXS", d.Plan.Qty[cex.Bear], "8.04")
	wantDec(t, "SOXL", d.Plan.Qty[cex.Bull], "15.60")
}

// One line with the thin leg first: the batch is pre-checked and a failing
// pre-check exits all the same (D1).
func TestDecideExit_OneLineThinFirst(t *testing.T) {
	s := seqStrategy(t, nil)
	in := ex7ExitInput(ex7Ledger())
	in.Value = fixedValuer("-0.01")
	d := s.DecideExit(in)
	if d.Plan == nil || d.Plan.Line != ExitLineOne || !d.Plan.D1 || d.Stage != nil {
		t.Fatalf("%+v stage %v (%s), want D1 on the one line", d.Plan, d.Stage, d.Reason)
	}
	wantDec(t, "protection", d.Plan.Protect, "33.24")
	in.Value = fixedValuer("0.01")
	if d := s.DecideExit(in); d.Plan == nil || d.Plan.D1 {
		t.Errorf("%+v, want a plan without D1", d.Plan)
	}
}

func TestNewExitBatch(t *testing.T) {
	plan := &ExitPlan{Line: ExitLine2, D: 3.45, ExpectedNet: dec("-0.0025"), D1: true, Protect: dec("33.22")}
	b := NewExitBatch(Long, plan)
	if b.Dir != Long || b.Line != ExitLine2 || b.DecisionD != 3.45 || !b.ExpectedNet.Equal(dec("-0.0025")) || !b.D1 || !b.Protect.Equal(dec("33.22")) ||
		!b.ThinQty.IsZero() || !b.ThickQty.IsZero() || !b.FirstFill.IsZero() || b.HardEnd != "" {
		t.Errorf("%+v", b)
	}
}

// --- the thick leg of an exit batch ---

// exitPairInput is SPEC 12.7 example 7's exit2 after SOXS 8.04 closed @
// 60.02 (booked in ledger): SOXL's bids 33.22 / 33.21.
func exitPairInput(ledger *fakeLedger) ExitPairInput {
	return ExitPairInput{
		Batch: ExitBatch{
			Dir: Long, Line: ExitLine2, DecisionD: seqD(33.22, 60.02),
			Fills:     Fills{ThinQty: dec("8.04"), ThinVWAP: dec("60.02"), ThickQty: decimal.Zero, ThickVWAP: decimal.Zero},
			FirstFill: t0,
		},
		Pairing:     seqPairing,
		Hold:        [2]decimal.Decimal{dec("15.60"), decimal.Zero},
		Books:       [2]*cex.Book{book(lv("33.22x20", "33.21x20"), lv("33.30x20", "33.31x20")), book(lv("59.90x5"), lv("60.10x5"))},
		DepthOK:     [2]bool{true, true},
		Rules:       testRules,
		F:           seqF,
		Anchors:     seqAnchors,
		PairAnchors: seqAnchors,
		Value:       ledger.valuer(-1),
		Now:         t0.Add(time.Second),
	}
}

func booked(l *fakeLedger, leg cex.Leg, qty, price string) *fakeLedger {
	if err := l.book(-1, leg, dec(qty), dec(price)); err != nil {
		panic(err)
	}
	return l
}

// SPEC 12.7 examples 7 and 8: the break-even bound of the thick leg after
// the thin leg closed, rounded to the worst legal tick (a sell rounds up),
// and after part of the thick leg filled, solved on the rest only.
func TestExitThickBound(t *testing.T) {
	s := seqStrategy(t, seqExit2)
	in := exitPairInput(booked(ex7Ledger(), cex.Bear, "8.04", "60.02"))
	if p, err := s.ExitThickBound(in, dec("15.60")); err != nil || !p.Equal(dec("33.20")) {
		t.Errorf("exit2 all: %s %v, want 33.20 (33.1905 rounded up)", p, err)
	}

	half := exitPairInput(booked(ex7Ledger(), cex.Bear, "4.02", "59.96"))
	if p, _ := s.ExitThickBound(half, dec("7.80")); !p.Equal(dec("33.23")) {
		t.Errorf("exit1 half: %s, want 33.23 (33.2214 rounded up)", p)
	}

	part := exitPairInput(booked(booked(ex7Ledger(), cex.Bear, "8.04", "60.02"), cex.Bull, "6.00", "33.22"))
	if p, _ := s.ExitThickBound(part, dec("9.60")); !p.Equal(dec("33.18")) {
		t.Errorf("after 6.00 @ 33.22: %s, want 33.18 (33.1721 rounded up)", p)
	}
	net, _ := part.Value(cex.PurposeExit, legClose(cex.Bull, dec("9.60"), dec("33.20")), seqFee)
	wantRound(t, "the round with the rest at 33.20", net, "0.268", 3)
	net, _ = part.Value(cex.PurposeExit, legClose(cex.Bull, dec("9.60"), dec("33.18")), seqFee)
	wantRound(t, "the round with the rest at 33.18", net, "0.076", 3)

	// A short cycle buys its thick leg back: its bound is the highest price,
	// rounded down. Sold SOXL 10 @ 33.30 and SOXS 5.15 @ 60.00, SOXS bought
	// back @ 59.90: the 9.99 SOXL may cost up to 33.3432 -> 33.34.
	short := (&fakeLedger{dir: Short, fee: seqFee}).lot("10@33.30", "5.15@60.00")
	sin := exitPairInput(booked(short, cex.Bear, "5.15", "59.90"))
	sin.Batch.Dir, sin.Batch.ThinQty, sin.Batch.ThinVWAP = Short, dec("5.15"), dec("59.90")
	if p, _ := s.ExitThickBound(sin, dec("9.99")); !p.Equal(dec("33.34")) {
		t.Errorf("short: %s, want 33.34", p)
	}
}

// ExitPairStep's checks in order: in flight, taken over, matched, the hard
// end (D2), the bound, the level walk.
func TestExitPairStep(t *testing.T) {
	s := seqStrategy(t, seqExit2)
	in := exitPairInput(booked(ex7Ledger(), cex.Bear, "8.04", "60.02"))

	// Example 7's exit2: bound 33.20, the first bid 33.22 takes all 15.60.
	a := s.ExitPairStep(in)
	if a.Kind != ExitPairSendThick || a.Leg != cex.Bull || a.Side != cex.SideSell || !a.Qty.Equal(dec("15.60")) || !a.Price.Equal(dec("33.22")) || a.Why != "" {
		t.Errorf("exit2: %+v, want send_thick sell 15.60 @ 33.22", a)
	}
	wantDec(t, "bound", a.Bound, "33.20")
	wantNear(t, "ideal closing spread", a.DIdeal, seqD(33.21, 60.02), 1e-6)

	pending := in
	pending.OrderPending, pending.Stopping = true, true
	if a := s.ExitPairStep(pending); a.Kind != ExitPairWait || a.Why != ReasonAwaitOrders {
		t.Errorf("pending: %s %s", a.Kind, a.Why)
	}
	pending.OrderPending = false
	if a := s.ExitPairStep(pending); a.Kind != ExitPairHandOver || a.Why != ReasonStopping {
		t.Errorf("stopping: %s %s", a.Kind, a.Why)
	}
	pending.Stopping, pending.Forced = false, true
	if a := s.ExitPairStep(pending); a.Kind != ExitPairHandOver || a.Why != ReasonForced {
		t.Errorf("forced: %s %s", a.Kind, a.Why)
	}

	matched := in
	matched.Batch.ThickQty, matched.Batch.ThickVWAP = dec("15.60"), dec("33.22")
	matched.Hold = [2]decimal.Decimal{decimal.Zero, decimal.Zero} // the ledger has booked the thick fills
	if a := s.ExitPairStep(matched); a.Kind != ExitPairDone {
		t.Errorf("matched: %s, want done", a.Kind)
	}
	nothing := in
	nothing.Batch.ThinQty = decimal.Zero
	if a := s.ExitPairStep(nothing); a.Kind != ExitPairDone {
		t.Errorf("no thin fill: %s, want done", a.Kind)
	}

	// Example 7's half: bound 33.23; bids 33.24 x 5, 33.23 x 5: 7.80 @ 33.23.
	// Only 33.22 and below: wait (33.22 would lose 0.011).
	half := exitPairInput(booked(ex7Ledger(), cex.Bear, "4.02", "59.96"))
	half.Batch.Line, half.Batch.DecisionD = ExitLine1, seqD(33.24, 59.96)
	half.Batch.ThinQty, half.Batch.ThinVWAP = dec("4.02"), dec("59.96")
	half.Hold = [2]decimal.Decimal{dec("15.60"), dec("4.02")}
	half.Books[cex.Bull] = book(lv("33.24x5", "33.23x5", "33.22x20"), lv("33.30x20", "33.31x20"))
	if a := s.ExitPairStep(half); a.Kind != ExitPairSendThick || !a.Qty.Equal(dec("7.80")) || !a.Price.Equal(dec("33.23")) {
		t.Errorf("half: %s %s @ %s, want send_thick 7.80 @ 33.23", a.Kind, a.Qty, a.Price)
	}
	half.Books[cex.Bull] = book(lv("33.22x20", "33.21x20"), lv("33.30x20", "33.31x20"))
	if a := s.ExitPairStep(half); a.Kind != ExitPairWait || a.Why != ReasonExitBound || !a.Bound.Equal(dec("33.23")) {
		t.Errorf("half below the bound: %s %s bound %s, want wait exit_bound 33.23", a.Kind, a.Why, a.Bound)
	}

	// Example 8: after 6.00 @ 33.22 the rest 9.60 goes at 33.20, bound 33.18.
	part := exitPairInput(booked(booked(ex7Ledger(), cex.Bear, "8.04", "60.02"), cex.Bull, "6.00", "33.22"))
	part.Batch.ThickQty, part.Batch.ThickVWAP = dec("6.00"), dec("33.22")
	part.Hold = [2]decimal.Decimal{dec("9.60"), decimal.Zero}
	part.Books[cex.Bull] = book(lv("33.20x20", "33.19x20"), lv("33.30x20", "33.31x20"))
	if a := s.ExitPairStep(part); a.Kind != ExitPairSendThick || !a.Qty.Equal(dec("9.60")) || !a.Price.Equal(dec("33.20")) || !a.Bound.Equal(dec("33.18")) {
		t.Errorf("ex8: %s %s @ %s bound %s, want send_thick 9.60 @ 33.20 bound 33.18", a.Kind, a.Qty, a.Price, a.Bound)
	}
}

// D1: the protection recorded before the batch's SOXS order limits every
// thick order of the batch; its break-even bound is not valued.
func TestExitPairStep_D1(t *testing.T) {
	s := seqStrategy(t, seqExit2)
	in := exitPairInput(ex7Ledger())
	in.Value = failValuer(t)
	in.Batch.D1, in.Batch.Protect = true, dec("33.22")
	in.Books[cex.Bull] = book(lv("33.21x20", "33.20x20"), lv("33.30x20", "33.31x20"))
	if a := s.ExitPairStep(in); a.Kind != ExitPairWait || a.Why != ReasonExitBound || !a.Bound.Equal(dec("33.22")) {
		t.Errorf("below the protection: %s %s bound %s, want wait exit_bound 33.22", a.Kind, a.Why, a.Bound)
	}
	in.Books[cex.Bull] = book(lv("33.22x5", "33.21x20"), lv("33.30x20", "33.31x20"))
	if a := s.ExitPairStep(in); a.Kind != ExitPairSendThick || a.Why != ReasonD1Loss || !a.Qty.Equal(dec("5")) || !a.Price.Equal(dec("33.22")) {
		t.Errorf("at the protection: %+v, want send_thick d1_loss 5 @ 33.22", a)
	}
}

// D2: exit_pair_window from the batch's first thin closing fill, not reset
// by later fills, and the ideal closing spread pair_stop_d worse than at the
// decision, end the batch at PRD 10.4's price whatever the bound.
func TestExitPairStep_D2(t *testing.T) {
	s := seqStrategy(t, seqExit2)
	in := exitPairInput(booked(ex7Ledger(), cex.Bear, "8.04", "60.02"))

	in.Now = t0.Add(30*time.Second - time.Millisecond)
	in.Books[cex.Bull] = book(lv("33.19x20", "33.19x20"), lv("33.30x20", "33.31x20")) // ideal 9.35 worse: no stop
	if a := s.ExitPairStep(in); a.Kind != ExitPairWait {
		t.Errorf("inside the window: %s %s, want wait", a.Kind, a.Why)
	}
	in.Now = t0.Add(30 * time.Second)
	a := s.ExitPairStep(in)
	if a.Kind != ExitPairHardClose || a.Why != ReasonExitWindow || !a.Qty.Equal(dec("15.60")) || !a.Price.Equal(dec("33.15")) {
		t.Errorf("window: %+v, want hard_close exit_window 15.60 @ 33.15 (33.19 less 10 bps, rounded down)", a)
	}

	// The hard close goes on with the rest, whatever the clock.
	in.Now = t0.Add(time.Second)
	in.Batch.HardEnd, in.Batch.ThickQty, in.Batch.ThickVWAP = ReasonExitWindow, dec("10"), dec("33.17")
	in.Hold[cex.Bull] = dec("5.60") // the ledger has booked the 10
	if a := s.ExitPairStep(in); a.Kind != ExitPairHardClose || a.Why != ReasonExitWindow || !a.Qty.Equal(dec("5.60")) {
		t.Errorf("hard close in progress: %s %s %s, want hard_close exit_window 5.60", a.Kind, a.Why, a.Qty)
	}

	// The spread stop: SOXL's second bid 33.18 is 12.47 worse than the
	// decision's 33.22.
	in.Batch.HardEnd, in.Batch.ThickQty, in.Batch.ThickVWAP = "", decimal.Zero, decimal.Zero
	in.Hold[cex.Bull] = dec("15.60")
	in.Books[cex.Bull] = book(lv("33.19x20", "33.18x20"), lv("33.30x20", "33.31x20"))
	if a := s.ExitPairStep(in); a.Kind != ExitPairHardClose || a.Why != ReasonExitStopD {
		t.Errorf("stop: %s %s, want hard_close exit_stop_d", a.Kind, a.Why)
	}
}

// The thick target of an exit batch is the round's cumulative one: floor of
// all the thin leg the round's exits closed x A_thin / A_thick, less all the
// thick leg they closed; the batch that closes the last of the thin leg
// closes everything the thick leg still holds. Live 2026-11-27, cycle
// c…154775 (short SOXS 8.03 / SOXL 15.58, bell anchors): per-batch floors
// closed SOXL 0.32 (0.17 SOXS), 7.45 (3.84) and 7.80 (4.02), 15.57 in all,
// and left 0.01 SOXL short as dust.
func TestExitPairStep_CumulativeThickTarget(t *testing.T) {
	s := seqStrategy(t, seqExit2)
	live := anchors("32.0287420270484", "62.1463560859516")
	legs := func(bull, bear string) [2]decimal.Decimal { return [2]decimal.Decimal{dec(bull), dec(bear)} }
	target := func(before [2]decimal.Decimal, thin string, hold [2]decimal.Decimal) decimal.Decimal {
		in := exitPairInput(ex7Ledger())
		in.Batch.Dir, in.Batch.ThinQty, in.Batch.ThinVWAP = Short, dec(thin), dec("59.62")
		in.Batch.HardEnd = ReasonExitWindow // the hard close sends the whole target, whatever the bound
		in.Anchors, in.PairAnchors, in.Before, in.Hold = live, live, before, hold
		a := s.ExitPairStep(in)
		if a.Kind != ExitPairHardClose {
			t.Fatalf("before %v, thin %s, hold %v: %s %s, want hard_close", before, thin, hold, a.Kind, a.Why)
		}
		return a.Qty
	}
	cases := []struct {
		name   string
		before [2]decimal.Decimal
		thin   string
		hold   [2]decimal.Decimal
		want   string
	}{
		{"exit1, SOXS 0.17", legs("0", "0"), "0.17", legs("15.58", "7.86"), "0.32"},                  // floor(0.3298)
		{"exit1, SOXS 3.84", legs("0.32", "0.17"), "3.84", legs("15.26", "4.02"), "7.46"},            // floor(4.01 x 1.94033) - 0.32, not floor(7.4509)
		{"exit2, the last SOXS 4.02", legs("7.78", "4.01"), "4.02", legs("7.80", "0"), "7.80"},       // all that is left
		{"after the live per-batch floors", legs("7.77", "4.01"), "4.02", legs("7.81", "0"), "7.81"}, // not floor(7.8001): no 0.01 left
	}
	for _, c := range cases {
		if got := target(c.before, c.thin, c.hold); !got.Equal(dec(c.want)) {
			t.Errorf("%s: SOXL %s, want %s", c.name, got, c.want)
		}
	}
}
