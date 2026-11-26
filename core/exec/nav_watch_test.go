package exec

import (
	"testing"
	"time"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/store"
)

// navAnchors is the NAV pair cex publishes after a switch at at.
func navAnchors(r *rig, bull, bear string, at time.Time) cex.Anchors {
	return cex.Anchors{SessionID: r.m.session.ID, Kind: cex.AnchorNAV, Bull: dec(bull), Bear: dec(bear), KnownAt: at, SwitchedAt: at}
}

// lastAnchorEvent is the detail of the last anchor_changed event.
func lastAnchorEvent(t *testing.T, r *rig) map[string]any {
	t.Helper()
	ev := r.sink.Last("event").(store.EventRow)
	if ev.Kind != "anchor_changed" {
		t.Fatalf("last event %s, want anchor_changed", ev.Kind)
	}
	return decodeDetail(t, ev.Detail)
}

// Owner items 96-97 with rewarm_on_anchor_change: the NAV replacing the bell
// anchor warms the mean and sigma up again from that minute; a later NAV
// switch in the watch whose spread jump at the current mids is below
// nav_hot_switch_d (1 spread unit) keeps them (a hot switch); a larger one
// warms them up again from its minute. A dividend's adjustment of the same
// NAV is not a switch. With the default (false) nothing warms up again.
func TestNAVSwitchHotOrRewarm(t *testing.T) {
	r := newRig(t, utc8Time(11, 25, 5, 1, 0), rigOptions{})
	r.m.scfg.RewarmOnAnchorChange = true
	r.warmUp() // ready at 05:03:01 under the bell anchor 40 / 60

	// 05:03:20 bell to NAV: a warm-up from the minute 05:03.
	at := utc8Time(11, 25, 5, 3, 20)
	r.src.setAnchors(navAnchors(r, "40", "60", at))
	r.stepAt(at)
	if r.m.stats.Ready || r.m.runStart != len(r.m.bars) || !r.m.lastReady.Ready {
		t.Fatalf("after bell to NAV: ready %t, run from %d of %d bars, last ready kept %t; want a warm-up from 05:03 with the last ready stats kept",
			r.m.stats.Ready, r.m.runStart, len(r.m.bars), r.m.lastReady.Ready)
	}
	if d := lastAnchorEvent(t, r); d["rewarm"] != true {
		t.Errorf("bell to NAV event %v, want rewarm", d)
	}
	r.endMinute()
	r.endMinute()
	r.stepAt(utc8Time(11, 25, 5, 5, 1))
	if !r.m.stats.Ready {
		t.Fatal("not ready two minutes after the NAV")
	}
	run := r.m.runStart

	// 05:05:10 a new NAV of the bull leg, 40.001: the jump at the mids is
	// 10^4 x 40.416 x (1/40.001 - 1/40) = -0.25 spread units, a hot switch.
	at = utc8Time(11, 25, 5, 5, 10)
	r.src.setAnchors(navAnchors(r, "40.001", "60", at))
	r.stepAt(at)
	if !r.m.stats.Ready || r.m.runStart != run {
		t.Errorf("after a small NAV change: ready %t, run from %d; want the mean kept from %d", r.m.stats.Ready, r.m.runStart, run)
	}
	d := lastAnchorEvent(t, r)
	if d["rewarm"] != false {
		t.Errorf("small NAV change event %v, want no rewarm", d)
	}
	near(t, "small jump", d["mid_jump_d"].(float64), 10000*40.416*(1/40.001-1/40.0))

	// 05:05:20 the bull leg's NAV is lowered by a dividend: the values
	// change, the switch time does not; not a switch.
	adjusted := navAnchors(r, "39.891", "60", at)
	adjusted.RestartAt = utc8Time(11, 25, 5, 5, 0) // as taken already
	r.m.restartedAt = adjusted.RestartAt
	r.src.setAnchors(adjusted)
	r.stepAt(utc8Time(11, 25, 5, 5, 20))
	if !r.m.stats.Ready || r.m.runStart != run {
		t.Errorf("after a dividend adjustment: ready %t, run from %d; want the mean kept from %d", r.m.stats.Ready, r.m.runStart, run)
	}

	// 05:05:30 a new NAV of 40.10 (39.99 after the dividend): about -27
	// spread units, a warm-up again from 05:05.
	at = utc8Time(11, 25, 5, 5, 30)
	large := navAnchors(r, "39.99", "60", at)
	large.RestartAt = adjusted.RestartAt
	r.src.setAnchors(large)
	r.stepAt(at)
	if r.m.stats.Ready || r.m.runStart != len(r.m.bars) {
		t.Errorf("after a large NAV change: ready %t, run from %d of %d bars; want a warm-up from 05:05", r.m.stats.Ready, r.m.runStart, len(r.m.bars))
	}
	if d := lastAnchorEvent(t, r); d["rewarm"] != true {
		t.Errorf("large NAV change event %v, want rewarm", d)
	}

	// The default keeps the mean through every switch.
	b := newRig(t, utc8Time(11, 25, 5, 1, 0), rigOptions{})
	b.warmUp()
	b.src.setAnchors(navAnchors(b, "40", "60", utc8Time(11, 25, 5, 3, 20)))
	b.stepAt(utc8Time(11, 25, 5, 3, 20))
	b.src.setAnchors(navAnchors(b, "40.10", "60", utc8Time(11, 25, 5, 3, 30)))
	b.stepAt(utc8Time(11, 25, 5, 3, 30))
	if !b.m.stats.Ready || b.m.runStart != 0 {
		t.Errorf("default: ready %t, run from %d; want the mean kept", b.m.stats.Ready, b.m.runStart)
	}
}

// A restart after NAV switches in the watch rebuilds the run as the
// uninterrupted loop left it: from the stored nav facts, the bell to NAV
// switch warms up again, a small NAV change does not, a large one does
// (its jump measured on the last minute before it).
func TestRestartAfterNAVSwitchesInTheWatch(t *testing.T) {
	for _, c := range []struct {
		name     string
		bull     string // the second NAV of the bull leg
		wantFrom time.Time
	}{
		{"small change", "40.001", utc8Time(11, 25, 5, 3, 0)},
		{"large change", "40.10", utc8Time(11, 25, 5, 5, 0)},
	} {
		t.Run(c.name, func(t *testing.T) {
			first, second := utc8Time(11, 25, 5, 3, 20), utc8Time(11, 25, 5, 5, 20)
			restart := utc8Time(11, 25, 5, 6, 30)
			a := newRig(t, utc8Time(11, 25, 5, 1, 0), rigOptions{})
			a.m.scfg.RewarmOnAnchorChange = true
			a.warmUp()
			a.src.setAnchors(navAnchors(a, "40", "60", first))
			a.stepAt(first)
			a.endMinute()
			a.endMinute()
			a.src.setAnchors(navAnchors(a, c.bull, "60", second))
			a.stepAt(second)
			a.endMinute()
			a.stepAt(restart.Add(-time.Second))
			if got := a.m.bars[a.m.runStart].Minute; !got.Equal(c.wantFrom) {
				t.Fatalf("the uninterrupted run starts at %s, want %s", showTime(got), showTime(c.wantFrom))
			}

			var bars []store.MinuteBarRow
			for _, row := range a.sink.Rows("minute_bar") {
				bars = append(bars, row.(store.MinuteBarRow))
			}
			var facts []store.AnchorFactRow
			id := a.m.session.ID
			for _, leg := range cex.Legs {
				facts = append(facts,
					store.AnchorFactRow{SessionID: id, Kind: cex.FactBell, Leg: leg.String(), KnownMs: utc8Time(11, 25, 5, 1, 0).UnixMilli(), Value: dec(map[cex.Leg]string{cex.Bull: "40", cex.Bear: "60"}[leg])},
					store.AnchorFactRow{SessionID: id, Kind: cex.FactNAV, Leg: leg.String(), KnownMs: first.UnixMilli(), Value: dec(map[cex.Leg]string{cex.Bull: "40", cex.Bear: "60"}[leg])},
					store.AnchorFactRow{SessionID: id, Kind: cex.FactNAV, Leg: leg.String(), KnownMs: second.UnixMilli(), Value: dec(map[cex.Leg]string{cex.Bull: c.bull, cex.Bear: "60"}[leg])})
			}
			b := newRig(t, restart, rigOptions{state: &store.State{MinuteBars: bars, AnchorFacts: facts}})
			b.m.scfg.RewarmOnAnchorChange = true
			b.src.setAnchors(navAnchors(b, c.bull, "60", second))
			if _, err := b.m.recover(b.m.ctx); err != nil {
				t.Fatal(err)
			}
			if got := b.m.bars[b.m.runStart].Minute; !got.Equal(c.wantFrom) {
				t.Errorf("the restarted run starts at %s, want %s", showTime(got), showTime(c.wantFrom))
			}
			if b.m.stats.Ready != a.m.stats.Ready || !b.m.stats.AsOf.Equal(a.m.stats.AsOf) {
				t.Errorf("restarted: ready %t as of %s; the uninterrupted run ready %t as of %s",
					b.m.stats.Ready, showTime(b.m.stats.AsOf), a.m.stats.Ready, showTime(a.m.stats.AsOf))
			}
		})
	}
}
