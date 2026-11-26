package exec

import (
	"testing"
	"time"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/store"
)

// A restart inside the warm-up that rewarm_on_anchor_change asks for after
// bell to NAV (audit of step 6, and its review). The uninterrupted run is
// ready at 05:03:00 under the bell anchor; when the NAV replaces it, the
// mean warms up again from the minute the NAV became known in (two minutes
// here). The restarted run stops after the NAV and starts again from the
// stored minutes and anchor facts: it must be as warm as the uninterrupted
// run, with the same first ready minute and the same entry at 05:05:05.
// The cases: the restart in the NAV's minute and a minute later; the NAV
// known at 05:03:59.900 and seen by the loop in the next minute (the run
// still starts at 05:03); the NAV as the Session's first anchor, with no
// bell before it (no warm-up again). With the default (false) the NAV never
// changes the run.
func TestRestartInsideTheWarmUpAfterTheNAV(t *testing.T) {
	type event struct {
		at   time.Time
		book func() pairBook
	}
	// The market after the warm-up, the same for every run: the minute 05:03
	// ends on book A, 05:04 on book B, the entry book at 05:05:05.
	events := []event{
		{utc8Time(11, 25, 5, 3, 59), func() pairBook { return calibration(0) }},
		{utc8Time(11, 25, 5, 4, 59), func() pairBook { return calibration(1) }},
		{utc8Time(11, 25, 5, 5, 0), nil},
		{utc8Time(11, 25, 5, 5, 5), entryBook},
	}
	end := utc8Time(11, 25, 5, 6, 0)
	cases := []struct {
		name                   string
		navAt, seenAt, restart time.Time
		bellFirst              bool      // the Session had the bell anchor before the NAV
		wantReady              time.Time // the minute the uninterrupted run with rewarm on is first seen ready as of after the restart
	}{
		{"restart in the NAV's minute", utc8Time(11, 25, 5, 3, 20), utc8Time(11, 25, 5, 3, 20), utc8Time(11, 25, 5, 3, 50), true, utc8Time(11, 25, 5, 4, 0)},
		{"restart a minute later", utc8Time(11, 25, 5, 3, 20), utc8Time(11, 25, 5, 3, 20), utc8Time(11, 25, 5, 4, 30), true, utc8Time(11, 25, 5, 4, 0)},
		{"NAV seen in the next minute", utc8Time(11, 25, 5, 3, 59).Add(900 * time.Millisecond), utc8Time(11, 25, 5, 4, 0).Add(100 * time.Millisecond), utc8Time(11, 25, 5, 4, 30), true, utc8Time(11, 25, 5, 4, 0)},
		{"no bell before the NAV", utc8Time(11, 25, 5, 3, 20), utc8Time(11, 25, 5, 3, 20), utc8Time(11, 25, 5, 3, 50), false, utc8Time(11, 25, 5, 2, 0)},
	}
	for _, c := range cases {
		nav := func(r *rig) {
			r.src.setAnchors(cex.Anchors{SessionID: r.m.session.ID, Kind: cex.AnchorNAV, Bull: dec("40"), Bear: dec("60"), KnownAt: c.navAt, SwitchedAt: c.navAt})
		}
		// play runs the events in [from, until) and returns the first ready
		// minute seen, zero when none.
		play := func(r *rig, from, until time.Time) (firstReady time.Time) {
			for _, e := range events {
				if e.at.Before(from) || !e.at.Before(until) {
					continue
				}
				r.at(e.at)
				if e.book != nil {
					r.books(e.book())
				}
				if e.book != nil && e.at.Equal(events[3].at) {
					r.trade(e.at)
				} else {
					r.step()
				}
				if r.m.stats.Ready && firstReady.IsZero() {
					firstReady = r.m.stats.AsOf
				}
			}
			return firstReady
		}
		for _, rewarm := range []bool{true, false} {
			name := c.name + map[bool]string{true: ", rewarm", false: ", default"}[rewarm]
			t.Run(name, func(t *testing.T) {
				// The uninterrupted run, and what it had stored when the
				// restarted one starts.
				a := newRig(t, utc8Time(11, 25, 5, 1, 0), rigOptions{})
				a.m.scfg.RewarmOnAnchorChange = rewarm
				if c.bellFirst {
					a.warmUp()
				} else { // the minutes need no anchor: they warm the mean up before the first one
					a.src.setAnchors(cex.Anchors{SessionID: a.m.session.ID})
					a.stepAt(a.clock().Add(30 * time.Second))
					a.endMinute()
					a.endMinute()
					a.stepAt(a.clock().Add(time.Second))
				}
				play(a, a.clock(), c.seenAt)
				nav(a)
				a.stepAt(c.seenAt)
				play(a, c.seenAt, c.restart)
				a.stepAt(c.restart.Add(-time.Second)) // its last round, which closed the minutes before it
				var bars []store.MinuteBarRow
				for _, row := range a.sink.Rows("minute_bar") {
					bars = append(bars, row.(store.MinuteBarRow))
				}
				atRestart := a.m.stats
				wantReady := play(a, c.restart, end)
				want := a.decisions()[len(a.decisions())-1]

				// The facts as cex stores them: both legs' NAV with one known
				// time, and the bell anchor before it when there was one.
				var facts []store.AnchorFactRow
				for _, leg := range cex.Legs {
					if c.bellFirst {
						facts = append(facts, store.AnchorFactRow{SessionID: a.m.session.ID, Kind: cex.FactBell, Leg: leg.String(), KnownMs: utc8Time(11, 25, 5, 1, 0).UnixMilli()})
					}
					facts = append(facts, store.AnchorFactRow{SessionID: a.m.session.ID, Kind: cex.FactNAV, Leg: leg.String(), KnownMs: c.navAt.UnixMilli()})
				}
				b := newRig(t, c.restart, rigOptions{state: &store.State{MinuteBars: bars, AnchorFacts: facts}})
				b.m.scfg.RewarmOnAnchorChange = rewarm
				nav(b)
				if _, err := b.m.recover(b.m.ctx); err != nil {
					t.Fatal(err)
				}
				if b.m.stats.Ready != atRestart.Ready || !b.m.stats.AsOf.Equal(atRestart.AsOf) {
					t.Fatalf("right after the restart: ready %t as of %s; the uninterrupted run ready %t as of %s",
						b.m.stats.Ready, showTime(b.m.stats.AsOf), atRestart.Ready, showTime(atRestart.AsOf))
				}
				gotReady := play(b, c.restart, end)
				got := b.decisions()[len(b.decisions())-1]

				if rewarm && !wantReady.Equal(c.wantReady) {
					t.Errorf("uninterrupted run first ready as of %s, want %s", showTime(wantReady), showTime(c.wantReady))
				}
				if !gotReady.Equal(wantReady) {
					t.Errorf("ready as of %s, the uninterrupted run %s", showTime(gotReady), showTime(wantReady))
				}
				if got.Kind != "entry" || got.Kind != want.Kind || got.TsMs != want.TsMs {
					t.Fatalf("decision %s at %d, the uninterrupted run's %s at %d", got.Kind, got.TsMs, want.Kind, want.TsMs)
				}
				gd, wd := decodeDetail(t, got.Detail), decodeDetail(t, want.Detail)
				for _, k := range []string{"x_d", "mu_d", "sigma_d", "price_bull", "price_bear", "qty_bull", "qty_bear"} {
					if gd[k] != wd[k] {
						t.Errorf("entry %s = %v, the uninterrupted run's %v", k, gd[k], wd[k])
					}
				}
			})
		}
	}
}

// The run started after bell to NAV never reaches back over a minute that
// already starts a run (review): the NAV is known in the last second of a
// minute and seen in the next, but that minute closed invalid (PRD 4.6: the
// run starts after it), or a dividend took effect when it ended. The run
// keeps starting after that minute, as the restart does.
func TestRewarmKeepsALaterRunStart(t *testing.T) {
	cases := []struct {
		name string
		last time.Time // the minute the NAV becomes known in, after which the run already starts
	}{{"invalid minute", utc8Time(11, 25, 5, 4, 0)}, {"dividend restart", utc8Time(11, 25, 5, 3, 0)}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t, utc8Time(11, 25, 5, 1, 0), rigOptions{})
			r.m.scfg.RewarmOnAnchorChange = true
			r.warmUp() // the minutes 05:01 and 05:02; 05:03 is observed at 05:03:01
			next := c.last.Add(time.Minute)
			if c.name == "invalid minute" {
				r.stepAt(c.last.Add(50 * time.Millisecond)) // 05:03 closes valid; no usable book in 05:04
			} else {
				dividend := r.m.anchors
				dividend.RestartAt = next
				r.src.setAnchors(dividend)
			}
			r.stepAt(next.Add(50 * time.Millisecond))
			before := r.m.runStart
			if before == 0 || !r.m.bars[before-1].Minute.Equal(c.last) {
				t.Fatalf("the run starts at minute %d, want after %s", before, showTime(c.last))
			}
			a := r.m.anchors
			a.Kind, a.KnownAt, a.SwitchedAt = cex.AnchorNAV, next.Add(-100*time.Millisecond), next.Add(-100*time.Millisecond)
			r.src.setAnchors(a)
			r.stepAt(next.Add(100 * time.Millisecond))
			if r.m.runStart != before {
				t.Errorf("the run starts at minute %d, want %d (after %s)", r.m.runStart, before, showTime(c.last))
			}
		})
	}
}
