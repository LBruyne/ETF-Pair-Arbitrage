package strategy

import (
	"testing"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
)

// The independent review of the thin-leg-first execution (2026-11-26):
// the spread stop under the decision's anchors, the top-ups blocked by what
// blocks new entries, and the end state of a batch taken over.

// An anchor switch inside the pair window moves D by the jump of PRD 11.4
// (about 25 units on 2026-11-20). The stop compares D_ideal with D_dec
// under the decision's anchors and F, so the switch neither fires it
// (SOXL's anchor 32.0287 -> 31.95, +25.6 units) nor masks it (-> 32.11,
// -26.2 units).
func TestPairStep_StopUnderTheDecisionAnchors(t *testing.T) {
	s := seqStrategy(t, nil)
	batch := ex1Batch()
	batch.DecisionAnchors, batch.DecisionF = seqAnchors, seqF

	// SOXL's second ask 33.28: D_ideal -1.30, 5.3 above D_dec: no stop.
	in := pairInput(batch, book(lv("33.25x20"), lv("33.28x20", "33.28x20")), soxsShallow())
	in.Anchors = anchors("31.95", "62.1464")
	a := s.PairStep(in)
	if a.Kind == PairUnwind || a.Why == ReasonStopD {
		t.Errorf("SOXL anchor 32.0287 -> 31.95 mid-batch: %s %s, want no unwind", a.Kind, a.Why)
	}
	wantNear(t, "D_ideal under the decision's anchors", a.DIdeal, -1.304770711, 1e-6)

	// SOXL's second ask 33.30: D_ideal 4.93, 11.5 above D_dec: the stop.
	in.Books[cex.Bull] = book(lv("33.25x20"), lv("33.29x20", "33.30x20"))
	in.Anchors = anchors("32.11", "62.1464")
	if a := s.PairStep(in); a.Kind != PairUnwind || a.Why != ReasonStopD {
		t.Errorf("SOXL anchor 32.0287 -> 32.11 mid-batch at ask2 33.30: %s %s, want unwind stop_d", a.Kind, a.Why)
	}

	// The thick bound P* and x stay on the current anchors.
	in.Books[cex.Bull] = book(lv("33.25x20"), lv("33.28x20", "33.28x20"))
	in.Anchors = anchors("31.95", "62.1464")
	a = s.PairStep(in)
	cur := in
	cur.Batch.DecisionAnchors, cur.Batch.DecisionF = cex.Anchors{}, 0
	if vwapCap, _, _ := s.PairBound(cur, dec("9.70")); !a.VWAPCap.Equal(vwapCap) {
		t.Errorf("P* %s, want %s under the current anchors", a.VWAPCap, vwapCap)
	}
	mu, _, _ := lineStats(in.Stats, in.Anchors)
	wantNear(t, "x under the current anchors", a.X, Spread(seqF, 33.28, 31.95, 59.880, 62.1464)-mu, 1e-6)
}

// The exit's spread stop likewise: SOXL's anchor 32.0287 -> 32.11 mid exit
// batch (-26.2 units) must not hard-close a long cycle's exit; 31.95 must
// not mask a real one (second bid 33.18, 12.47 worse than the decision's).
func TestExitPairStep_StopUnderTheDecisionAnchors(t *testing.T) {
	s := seqStrategy(t, seqExit2)
	in := exitPairInput(booked(ex7Ledger(), cex.Bear, "8.04", "60.02"))
	in.Batch.DecisionAnchors, in.Batch.DecisionF = seqAnchors, seqF
	in.Anchors = anchors("32.11", "62.1464")
	a := s.ExitPairStep(in)
	if a.Kind != ExitPairSendThick || !a.Qty.Equal(dec("15.60")) || !a.Price.Equal(dec("33.22")) {
		t.Errorf("SOXL anchor -> 32.11 mid exit batch: %s %s %s @ %s, want send_thick 15.60 @ 33.22", a.Kind, a.Why, a.Qty, a.Price)
	}
	wantNear(t, "ideal closing spread under the decision's anchors", a.DIdeal, seqD(33.21, 60.02), 1e-6)

	in.Anchors = anchors("31.95", "62.1464")
	in.Books[cex.Bull] = book(lv("33.19x20", "33.18x20"), lv("33.30x20", "33.31x20"))
	if a := s.ExitPairStep(in); a.Kind != ExitPairHardClose || a.Why != ReasonExitStopD {
		t.Errorf("SOXL anchor -> 31.95 at bid2 33.18: %s %s, want hard_close exit_stop_d", a.Kind, a.Why)
	}
}

// The plans carry the anchors and F of their decision, and the batches
// take them over.
func TestBatchesKeepTheDecisionAnchors(t *testing.T) {
	plan, reason := newStrategy(t, nil).EvaluateEntry(entryInput(longBooks()))
	if plan == nil {
		t.Fatalf("no plan: %s", reason)
	}
	in := entryInput(longBooks())
	if !plan.Anchors.Bull.Equal(in.Anchors.Bull) || !plan.Anchors.Bear.Equal(in.Anchors.Bear) || plan.F != in.F {
		t.Errorf("entry plan anchors %+v F %v, want the input's %+v %v", plan.Anchors, plan.F, in.Anchors, in.F)
	}
	b := NewPairBatch(plan, cex.Bear, dec("30"), dec("59.41"), t0)
	if !b.DecisionAnchors.Bull.Equal(plan.Anchors.Bull) || !b.DecisionAnchors.Bear.Equal(plan.Anchors.Bear) || b.DecisionF != plan.F {
		t.Errorf("pair batch anchors %+v F %v, want the plan's", b.DecisionAnchors, b.DecisionF)
	}

	s := seqStrategy(t, seqExit2)
	ein := ex7ExitInput(ex7Ledger())
	d := s.DecideExit(ein)
	if d.Plan == nil {
		t.Fatalf("no exit plan: %s", d.Reason)
	}
	if !d.Plan.Anchors.Bull.Equal(ein.Anchors.Bull) || !d.Plan.Anchors.Bear.Equal(ein.Anchors.Bear) || d.Plan.F != ein.F {
		t.Errorf("exit plan anchors %+v F %v, want the input's", d.Plan.Anchors, d.Plan.F)
	}
	e := NewExitBatch(Long, d.Plan)
	if !e.DecisionAnchors.Bull.Equal(d.Plan.Anchors.Bull) || e.DecisionF != d.Plan.F {
		t.Errorf("exit batch anchors %+v F %v, want the plan's", e.DecisionAnchors, e.DecisionF)
	}
}

// What blocks new entries (PRD 5.10, 5.11) blocks a top-up for the frame:
// the pairing goes on, and a remainder too small to pair waits for a later
// frame instead of ending the batch.
func TestPairStep_TopUpBlocked(t *testing.T) {
	s := seqStrategy(t, nil)
	soxl := book(lv("33.25x20"), lv("33.28x20", "33.29x20"))
	in := pairInput(ex1Batch(), soxl, soxsDeep())
	in.TopUpBlocked = true
	if a := s.PairStep(in); a.Kind != PairWait || a.Why != ReasonPairBound {
		t.Errorf("top-up blocked: %s %s, want wait pair_bound", a.Kind, a.Why)
	}
	in.Books[cex.Bull] = book(lv("33.25x20"), lv("33.27x6", "33.28x20"))
	if a := s.PairStep(in); a.Kind != PairSendThick {
		t.Errorf("top-up blocked, SOXL within P*: %s, want send_thick", a.Kind)
	}

	small := ex1Batch()
	small.ThinQty = dec("0.07")
	in = pairInput(small, soxl, soxsDeep())
	in.TopUpBlocked = true
	if a := s.PairStep(in); a.Kind != PairWait || a.Why != ReasonTooSmall {
		t.Errorf("too small, top-up blocked: %s %s, want wait too_small", a.Kind, a.Why)
	}
}

// SPEC 12.4 step 2: a batch taken over ends as it stands. With part paired
// and S_open left: built_partial; with nothing paired: unwound_stop; paired
// whole: built.
func TestPairBatchHandOverEnd(t *testing.T) {
	step := testRules[cex.Bull].QtyStep
	for _, c := range []struct {
		name          string
		thick, closed string
		want          PairEnd
	}{
		{"part paired, S_open left", "4.85", "0", PairEndBuiltPartial},
		{"part paired, part closed, S_open left", "2.00", "1.00", PairEndBuiltPartial},
		{"nothing paired", "0", "0", PairEndUnwoundStop},
		{"nothing paired, part closed at a profit", "0", "2.00", PairEndUnwoundStop},
		{"paired whole", "9.70", "0", PairEndBuilt},
		{"part paired, the rest closed", "4.85", "2.50", PairEndBuiltPartial},
	} {
		b := ex1Batch()
		b.ThickQty, b.ThickVWAP, b.ThinClosed = dec(c.thick), dec("33.27"), dec(c.closed)
		if got := b.HandOverEnd(cex.Bear, seqAnchors, step); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
	// No step known (the rules not read): S_open is told unfloored.
	b := ex1Batch()
	b.ThickQty, b.ThickVWAP = dec("4.85"), dec("33.27")
	if got := b.HandOverEnd(cex.Bear, seqAnchors, decimal.Zero); got != PairEndBuiltPartial {
		t.Errorf("without a step: %s, want built_partial", got)
	}
}
