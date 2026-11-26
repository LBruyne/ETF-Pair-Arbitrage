package exec

import (
	"syscall"
	"testing"
	"time"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/strategy"
)

// The independent review of the thin-leg-first execution (2026-11-26), on
// the loop: the spread stops under the decision's anchors (live and after a
// restart), the top-ups blocked by what blocks new entries, the end state
// of a batch taken over, and no routine comparison of the positions while a
// batch is open.

// reasons lists the reasons of the loop's decisions of kind.
func (r *rig) reasons(kind string) (out []strategy.Reason) {
	for _, d := range r.pairRows(kind) {
		out = append(out, strategy.Reason(d.Reason))
	}
	return out
}

func hasReason(rs []strategy.Reason, want strategy.Reason) bool {
	for _, r := range rs {
		if r == want {
			return true
		}
	}
	return false
}

// navAnchors are the rig's anchors switched to a NAV of bull / bear at t.
func (r *rig) navAnchors(bull, bear string, t time.Time) cex.Anchors {
	a := r.src.Anchors()
	a.Kind, a.Bull, a.Bear, a.KnownAt, a.SwitchedAt = cex.AnchorNAV, dec(bull), dec(bear), t, t
	return a
}

// PRD 11.4: the NAV replacing the bell anchor inside the pair window moves
// D by its jump. SOXL's anchor 40 -> 40.2 moves it about -50 units at SOXL
// 40.39: for this short batch (sold, a falling D is adverse) the old code
// unwound for stop_d although nothing moved; 40 -> 39.8 (+50 units) hid a
// real stop (SOXL's second bid 40.35, 20.7 units below D_dec).
func TestThinFirstAnchorSwitchInPairing(t *testing.T) {
	t0 := utc8Time(11, 25, 5, 3, 5)
	waiting := withBull(lv("40.39", "300"), lv("40.38", "500")) // below P*, above the stop of 20

	t.Run("no false stop", func(t *testing.T) {
		r := thinRig(t, rigOptions{stopD: 20})
		r.thinFilled(t0, waiting)
		t1 := t0.Add(time.Second)
		r.src.setAnchors(r.navAnchors("40.2", "60", t1))
		r.at(t1)
		r.books(waiting)
		r.stepAt(t1)
		if len(r.orders()) != 1 || hasReason(r.reasons("pair"), strategy.ReasonStopD) {
			t.Errorf("%d orders, pair reasons %v, idle %q; want no unwind for the anchor switch", len(r.orders()), r.reasons("pair"), r.m.idleReason)
		}
	})
	t.Run("no masked stop", func(t *testing.T) {
		r := thinRig(t, rigOptions{stopD: 20})
		r.thinFilled(t0, waiting)
		t1 := t0.Add(time.Second)
		r.src.setAnchors(r.navAnchors("39.8", "60", t1))
		fallen := withBull(lv("40.36", "300"), lv("40.35", "500"))
		r.at(t1)
		r.books(fallen)
		r.stepAt(t1)
		if !hasReason(r.reasons("pair"), strategy.ReasonStopD) {
			t.Errorf("pair reasons %v, idle %q; want the stop_d the anchor switch must not mask", r.reasons("pair"), r.m.idleReason)
		}
	})
}

// The exit batch likewise: SOXL bought back after SOXS, its asks 40.42 /
// 40.43 (12.5 units worse than the decision's 40.38, within pair_stop_d
// 50), and SOXL's anchor 40 -> 39.5 (+128 units at 40.43): the thick leg
// keeps waiting for its bound, no hard close.
func TestThinFirstAnchorSwitchInExitBatch(t *testing.T) {
	r := lossRig(t, rigOptions{latency: 100 * time.Millisecond, stopD: 50})
	t1 := utc8Time(11, 25, 5, 3, 5)
	r.at(t1)
	r.books(exitBook())
	r.stepAt(t1)
	moved := exitBook()
	moved.bullAsks = side{lv("40.42", "300"), lv("40.43", "400")}
	r.depth(cex.Bull, moved.bullBids, moved.bullAsks)
	r.results()
	r.step()
	if r.m.idleReason != strategy.ReasonExitBound {
		t.Fatalf("idle %q, want SOXL waiting for its bound", r.m.idleReason)
	}
	t2 := t1.Add(time.Second)
	r.src.setAnchors(r.navAnchors("39.5", "60", t2))
	r.idleAt(t2, strategy.ReasonExitBound)
	if hasReason(r.reasons("exit_pair"), strategy.ReasonExitStopD) {
		t.Errorf("exit_pair reasons %v, want no exit_stop_d for the anchor switch", r.reasons("exit_pair"))
	}
}

// SPEC 12.9 "重启": a restart inside the pair window takes the decision's
// anchors and F from its entry decision record, so the anchors the new
// process starts with (SOXL's NAV 40.2: about -50 units at 40.39) do not
// unwind the batch for stop_d.
func TestRestartInPairingKeepsTheDecisionAnchors(t *testing.T) {
	restartTest(t)
	t0 := utc8Time(11, 25, 5, 3, 5)
	a, _ := thinProcess(t, t0)
	t1 := t0.Add(10 * time.Second)
	b := dbProcessWith(t, t1, a.exchange, rigOptions{thin: "bear", stopD: 20})
	b.src.setAnchors(b.navAnchors("40.2", "60", t1))
	if _, err := b.m.recover(b.m.ctx); err != nil {
		t.Fatal(err)
	}
	p := b.m.activePair()
	if p == nil || !p.batch.DecisionAnchors.Bull.Equal(dec("40")) || !p.batch.DecisionAnchors.Bear.Equal(dec("60")) || p.batch.DecisionF != 1 {
		t.Fatalf("pairing taken up %+v, want the decision's anchors 40 / 60 and F 1", p)
	}
	b.at(t1)
	b.books(withBull(lv("40.39", "300"), lv("40.38", "500")))
	b.stepAt(t1)
	if len(b.orders()) != 0 || hasReason(b.reasons("pair"), strategy.ReasonStopD) {
		t.Errorf("%d orders from the new process, pair reasons %v, idle %q; want no unwind after the restart", len(b.orders()), b.reasons("pair"), b.m.idleReason)
	}
}

// PRD 5.10 and 5.11: what blocks a new entry blocks a top-up too, the
// pairing going on (TestThinFirstTopUp's top-up does not go).
func TestThinFirstTopUpBlocked(t *testing.T) {
	t0 := utc8Time(11, 25, 5, 3, 5)
	t1 := t0.Add(time.Second)
	for _, c := range []struct {
		name  string
		block func(r *rig)
	}{
		{"F stale", func(r *rig) { r.fxFixed = true; r.src.setFX("1", t1.Add(-61*time.Second)) }},
		{"action window", func(r *rig) { a := r.src.Anchors(); a.EntryBan = true; r.src.setAnchors(a) }},
		{"regular hours", func(r *rig) { r.m.session.RegularOpen = t1 }},
		{"account settings", func(r *rig) { r.src.set(false, true) }},
		{"warming up", func(r *rig) { r.m.stats = strategy.Stats{} }},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := thinRig(t, rigOptions{stopD: 20})
			r.thinFilled(t0, withBull(lv("40.39", "300"), lv("40.38", "500")))
			deeper := withBull(lv("40.39", "300"), lv("40.38", "500"))
			deeper.bearBids = side{lv("59.54", "0.4"), lv("59.535", "60"), lv("59.53", "30"), lv("59.525", "30"), lv("59.52", "20")}
			c.block(r)
			r.at(t1)
			r.books(deeper)
			r.idleAt(t1, strategy.ReasonPairBound)
			if p := r.m.activePair(); p == nil {
				t.Error("the pairing ended; it goes on without top-ups")
			}
		})
	}
}

// SPEC 12.4 step 2: the stop takes over a batch that paired 50 of SOXL's
// 105.30: it ends built_partial (the forced exit closes the rest), not
// built.
func TestThinFirstHandOverPartlyPaired(t *testing.T) {
	r := thinRig(t, rigOptions{latency: 100 * time.Millisecond, stopD: 50})
	t0 := utc8Time(11, 25, 5, 3, 5)
	r.thinFilled(t0, withBull(lv("40.45", "50"), lv("40.30", "500")))
	r.results()
	r.step()
	if got := r.lotNow(1).Legs[cex.Bull].EntryQty; !got.Equal(dec("50")) {
		t.Fatalf("SOXL paired %s, want 50", got)
	}
	t1 := t0.Add(time.Second)
	r.m.onSignal(syscall.SIGTERM, t1)
	r.trade(t1)
	rows := r.pairRows("pair")
	last := rows[len(rows)-1]
	if d := detailOf(t, last); last.Reason != "stopping" || d["end"] != "built_partial" || d["hand_over"] != true {
		t.Errorf("hand-over %s %v, want built_partial for stopping", last.Reason, d)
	}
}

// The routine comparison of the positions (every 10 s) is a read of up to
// 2 s: it does not run while a batch is pairing or an exit batch is open.
func TestNoRoutineComparisonInABatch(t *testing.T) {
	t.Run("pairing", func(t *testing.T) {
		r := thinRig(t, rigOptions{stopD: 20})
		t0 := utc8Time(11, 25, 5, 3, 5)
		waiting := withBull(lv("40.39", "300"), lv("40.38", "500"))
		r.thinFilled(t0, waiting)
		r.m.nextReconcile = t0 // overdue
		reads := r.gate.reads.Load()
		r.at(t0.Add(time.Second))
		r.books(waiting)
		r.idleAt(t0.Add(time.Second), strategy.ReasonPairBound)
		if n := r.gate.reads.Load() - reads; n != 0 {
			t.Errorf("%d reads of the positions while pairing, want none", n)
		}
	})
	t.Run("exit batch", func(t *testing.T) {
		r := lossRig(t, rigOptions{latency: 100 * time.Millisecond, stopD: 50})
		t1 := utc8Time(11, 25, 5, 3, 5)
		r.at(t1)
		r.books(exitBook())
		r.stepAt(t1)
		moved := exitBook()
		moved.bullAsks = side{lv("40.42", "300"), lv("40.43", "400")}
		r.depth(cex.Bull, moved.bullBids, moved.bullAsks)
		r.results()
		r.step()
		r.m.nextReconcile = t1 // overdue
		reads := r.gate.reads.Load()
		r.idleAt(t1.Add(time.Second), strategy.ReasonExitBound)
		if n := r.gate.reads.Load() - reads; n != 0 {
			t.Errorf("%d reads of the positions in the exit batch, want none", n)
		}
	})
}
