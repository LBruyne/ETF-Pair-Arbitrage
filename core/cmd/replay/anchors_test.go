package main

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/store"
)

// showPairs renders anchor pairs as kind/bull/bear@known_ms.
func showPairs(pairs []pairAnchor) []string {
	var out []string
	for _, p := range pairs {
		out = append(out, fmt.Sprintf("%s %s/%s/%s@%d", p.session, p.kind, p.bull, p.bear, p.known))
	}
	return out
}

// A paper run's anchor facts give one publication per switch: the bell
// anchor, and each nav switch (both legs with one known time) of the watch
// as its own pair, not merged into one (owner items 96-97). nav_seen,
// bell_f and dividend rows are not anchors; a switch with one leg stored is
// left out.
func TestFactPairsFollowTheSwitches(t *testing.T) {
	d := decimal.RequireFromString
	row := func(kind, leg string, known int64, value string) store.AnchorFactRow {
		return store.AnchorFactRow{SessionID: "2026-11-25", Kind: kind, Leg: leg, KnownMs: known, Value: d(value)}
	}
	rows := []store.AnchorFactRow{
		row(cex.FactBellF, "bull", 60, "0.999"), row(cex.FactBellF, "bear", 60, "0.999"),
		row(cex.FactBell, "bull", 61, "39.96"), row(cex.FactBell, "bear", 61, "49.95"),
		row(cex.FactNAVSeen, "bull", 100, "40.1"), row(cex.FactNAVSeen, "bear", 100, "49.8"),
		row(cex.FactNAV, "bull", 100, "40.1"), row(cex.FactNAV, "bear", 100, "49.8"),
		row(cex.FactNAVSeen, "bull", 200, "40.12"),
		row(cex.FactNAV, "bull", 200, "40.12"), row(cex.FactNAV, "bear", 200, "49.8"),
		row(cex.FactNAV, "bull", 300, "41"), // one leg only
		row(cex.FactDividend, "bull", 400, "0.11"),
	}
	got := showPairs(factPairs(rows))
	want := []string{"2026-11-25 bell/39.96/49.95@61", "2026-11-25 nav/40.1/49.8@100", "2026-11-25 nav/40.12/49.8@200"}
	if !slices.Equal(got, want) {
		t.Errorf("pairs = %v\nwant %v", got, want)
	}
	for _, e := range anchorRefs(factPairs(rows)) {
		if !e.anchors.SwitchedAt.Equal(e.anchors.KnownAt) || e.anchors.KnownAt.UnixMilli() != e.at {
			t.Errorf("publication at %d: known %s switched %s, want both at its time", e.at, e.anchors.KnownAt, e.anchors.SwitchedAt)
		}
	}
}

// The board's NAV observations (each distinct value of a fund with when the
// collector first read it) give the program's switches: the NAV replaces
// the bell anchor once both legs have a value, and in the watch after that
// each new value switches again, the other leg keeping its value; a value
// first read after the watch is not taken.
func TestBoardNAVFollowsTheWatch(t *testing.T) {
	d := decimal.RequireFromString
	watch := time.Hour
	h := int64(time.Hour / time.Millisecond)
	obs := []navObs{
		{ticker: "SOXL", nav: d("40.08"), firstMs: 10}, // revised before the other leg appears
		{ticker: "SOXL", nav: d("40.10"), firstMs: 20},
		{ticker: "SOXS", nav: d("49.80"), firstMs: 30},       // both read: the first switch
		{ticker: "SOXS", nav: d("49.70"), firstMs: 30 + h/2}, // in the watch
		{ticker: "SOXL", nav: d("40.12"), firstMs: 30 + h/2}, // in the same round
		{ticker: "SOXL", nav: d("40.20"), firstMs: 30 + h},   // after the watch
		{ticker: "TQQQ", nav: d("70"), firstMs: 40},          // another fund
	}
	got := showPairs(navWatchPairs("2026-11-25", obs, "SOXL", "SOXS", watch))
	want := []string{"2026-11-25 nav/40.1/49.8@30", fmt.Sprintf("2026-11-25 nav/40.12/49.7@%d", 30+h/2)}
	if !slices.Equal(got, want) {
		t.Errorf("pairs = %v\nwant %v", got, want)
	}
	if got := navWatchPairs("2026-11-25", obs[:2], "SOXL", "SOXS", watch); len(got) != 0 {
		t.Errorf("one leg only: %v, want no NAV", showPairs(got))
	}
}
