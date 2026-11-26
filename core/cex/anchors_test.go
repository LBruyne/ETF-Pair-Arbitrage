package cex

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"testing"
	"time"

	"etf-pair-arbitrage/core/store"
)

// The Session the anchor tests run in: Monday 2026-12-21 closes at 16:00 New
// York and starts a Session that ends at Tuesday's close.
var (
	monClose = ny(2026, 12, 21, 16, 0)
	bellMin  = ny(2026, 12, 21, 15, 59) // the minute whose index close is the bell anchor
)

// anchorRig is a rig whose sources hold the bell inputs of the Session that
// starts at monClose: index closes 40 and 50 USDT and a candle F of 0.9990.
func anchorRig(t *testing.T, cfg Config, now time.Time) *testRig {
	t.Helper()
	r := newRig(t, cfg, now)
	r.index.setClose("SOXLUSDT", bellMin, "40")
	r.index.setClose("SOXSUSDT", bellMin, "50")
	r.fx.candles[bellMin.UnixMilli()] = dec("0.9990")
	return r
}

// stepAt moves the clock to at and advances the anchor state once.
func (r *testRig) stepAt(at time.Time) Anchors {
	r.clock.Set(at)
	r.m.stepAnchors(context.Background(), at)
	return r.m.Anchors()
}

// wantAnchors fails unless a has the given kind and values.
func wantAnchors(t *testing.T, a Anchors, kind AnchorKind, bull, bear string) {
	t.Helper()
	if a.Kind != kind || !a.Bull.Equal(dec(bull)) || !a.Bear.Equal(dec(bear)) {
		t.Fatalf("anchors = %s %s / %s, want %s %s / %s", a.Kind, a.Bull, a.Bear, kind, bull, bear)
	}
}

// withDividends is testConfig with a dividend of both funds going ex on
// Tuesday 2026-12-22: Monday carries it, and it takes effect on Monday 20:00
// New York.
func withDividends() Config {
	cfg := testConfig()
	cfg.CorporateActions = []CorporateAction{{Ticker: "SOXL", ExDate: "2026-12-22", Kind: ActionDividend}, {Ticker: "SOXS", ExDate: "2026-12-22", Kind: ActionDividend}}
	return cfg
}

// A Session starts without an anchor, and has none for as long as the index
// kline of the closing minute is not there: the read starts a minute after
// the close and is repeated every five minutes.
func TestAnchorNoneUntilTheBellIsRead(t *testing.T) {
	r := newRig(t, testConfig(), monClose)
	a := r.stepAt(monClose)
	if a.SessionID != "2026-12-21" || a.Kind != AnchorNone || !a.Bull.IsZero() || !a.Bear.IsZero() || !a.KnownAt.IsZero() || a.EntryBan {
		t.Fatalf("at the close: %+v, want the new Session without an anchor", a)
	}
	if r.index.calls != 0 {
		t.Fatal("the index was read before the bell delay")
	}
	// A minute on the read happens and finds F's candle but no kline.
	r.fx.candles[bellMin.UnixMilli()] = dec("0.9990")
	if a := r.stepAt(monClose.Add(time.Minute)); a.Kind != AnchorNone {
		t.Fatalf("without a kline: %+v", a)
	}
	calls := r.index.calls
	if calls == 0 {
		t.Fatal("the index was not read at the bell delay")
	}
	if r.stepAt(monClose.Add(5 * time.Minute)); r.index.calls != calls {
		t.Error("the index was read again before the retry interval")
	}
	// One leg's kline alone is not a bell anchor.
	r.index.setClose("SOXLUSDT", bellMin, "40")
	if a := r.stepAt(monClose.Add(6 * time.Minute)); a.Kind != AnchorNone || r.index.calls == calls {
		t.Fatalf("with one leg's kline: %+v after %d reads, want a retry and still no anchor", a, r.index.calls-calls)
	}
	r.index.setClose("SOXSUSDT", bellMin, "50")
	a = r.stepAt(monClose.Add(11 * time.Minute))
	wantAnchors(t, a, AnchorBell, "39.96", "49.95")
	if !a.KnownAt.Equal(monClose.Add(11 * time.Minute)) {
		t.Errorf("known at %s, want the moment it was read", showTime(a.KnownAt))
	}
	if len(r.facts()) != 4 {
		t.Errorf("%d anchor facts, want the fixed F and the bell anchor of each leg and nothing else", len(r.facts()))
	}
}

// Each reason the bell anchor cannot be read yet is logged as what it is:
// an input the source does not have is an Info line naming it, a read that
// failed is a Warn.
func TestAnchorBellRetryLogsSayWhatIsMissing(t *testing.T) {
	r := newRig(t, testConfig(), monClose)
	r.issuer.err = ErrIssuerKey // the issuer is not part of this
	r.stepAt(monClose)
	logs := captureLogs(t)
	at := monClose.Add(time.Minute)
	step := func() []string {
		t.Helper()
		r.stepAt(at)
		at = at.Add(5 * time.Minute)
		if a := r.m.Anchors(); a.Kind != AnchorNone {
			t.Fatalf("an anchor without its inputs: %+v", a)
		}
		return logs.take()
	}
	want := func(got []string, line string) {
		t.Helper()
		if len(got) != 1 || got[0] != line {
			t.Errorf("logged %q, want %q", got, line)
		}
	}
	// No candle of the minute.
	want(step(), "INFO cex: bell F of the minute missing; retrying")
	// The candle's source fails.
	r.fx.candleErr = errors.New("coinbase: http 503")
	want(step(), "WARN cex: bell anchor read failed; retrying")
	// F is there and is fixed; the index kline of the minute is not there.
	r.fx.candleErr = nil
	r.fx.candles[bellMin.UnixMilli()] = dec("0.9990")
	if got := step(); len(got) != 2 || got[0] != "INFO cex: bell F fixed" || got[1] != "INFO cex: bell index not ready; retrying" {
		t.Errorf("logged %q, want F fixed and then the index not ready", got)
	}
	// The index source fails.
	r.index.err = errors.New("binance: http 502")
	want(step(), "WARN cex: bell anchor read failed; retrying")
}

// The bell anchor is F x the index close, with F the close of the source's
// 1-minute candle of the closing minute (owner item 69): the F the process
// itself read in that minute is not used.
func TestAnchorBellUsesTheCandleCloseOfTheMinute(t *testing.T) {
	r := anchorRig(t, testConfig(), bellMin.Add(10*time.Second)) // candle 0.9990
	ctx := context.Background()
	r.clock.Set(bellMin.Add(50 * time.Second))
	r.fx.set("0.9984", "0.9988", nil) // mid 0.9986: the process's own last F of the minute
	r.m.pollFX(ctx)

	changed := r.m.ReferenceChanged()
	a := r.stepAt(monClose.Add(time.Minute))
	wantAnchors(t, a, AnchorBell, "39.96", "49.95") // 0.9990 x 40, 0.9990 x 50
	select {
	case <-changed:
	default:
		t.Error("ReferenceChanged was not closed by the anchor")
	}
	if r.fx.candleCalls != 1 {
		t.Errorf("%d candle reads, want 1", r.fx.candleCalls)
	}
	// The fixed F of each leg, then the bell anchor of each leg.
	facts := r.facts()
	if len(facts) != 4 || facts[0].Kind != FactBellF || facts[0].Leg != "bull" || !facts[0].Value.Equal(dec("0.999")) || facts[0].Source != "candle" || facts[1].Kind != FactBellF || facts[1].Leg != "bear" {
		t.Fatalf("anchor facts = %+v, want the fixed F of both legs first", facts)
	}
	facts = facts[2:]
	if facts[0].Kind != FactBell || facts[0].Leg != "bull" || !facts[0].Value.Equal(dec("39.96")) || !facts[0].F.Equal(dec("0.999")) ||
		facts[0].SessionID != "2026-12-21" || facts[0].KnownMs != monClose.Add(time.Minute).UnixMilli() || facts[1].Leg != "bear" || !facts[1].Value.Equal(dec("49.95")) {
		t.Errorf("bell facts = %+v", facts)
	}
}

// A restart while the index is still awaited takes the fixed F over from
// the stored fact: the candle is not read again, even when the source has
// revised it meanwhile.
func TestAnchorBellKeepsItsFAcrossARestart(t *testing.T) {
	r := newRig(t, testConfig(), monClose)
	r.issuer.err = ErrIssuerKey // the issuer is not part of this
	r.fx.candles[bellMin.UnixMilli()] = dec("0.999")
	r.stepAt(monClose.Add(time.Minute)) // F fixed, the index missing
	facts := AnchorFactsFromRows(r.facts())
	if len(facts) != 2 || facts[0].Kind != FactBellF {
		t.Fatalf("facts before the restart = %+v, want the fixed F of both legs", facts)
	}

	restart := monClose.Add(6 * time.Minute)
	r2 := newRig(t, testConfig(), restart)
	r2.issuer.err = ErrIssuerKey
	r2.fx.candles[bellMin.UnixMilli()] = dec("0.998") // revised by the source
	r2.m.Restore(facts, nil)
	if a := r2.stepAt(restart); a.Kind != AnchorNone {
		t.Fatalf("an anchor without the index: %+v", a)
	}
	r2.index.setClose("SOXLUSDT", bellMin, "40")
	r2.index.setClose("SOXSUSDT", bellMin, "50")
	a := r2.stepAt(monClose.Add(11 * time.Minute))
	wantAnchors(t, a, AnchorBell, "39.96", "49.95")
	if r2.fx.candleCalls != 0 {
		t.Errorf("%d candle reads after the restart, want none: F was restored", r2.fx.candleCalls)
	}
	// The restarted process stores the bell anchor and not the F again.
	if facts := r2.facts(); len(facts) != 2 || facts[0].Kind != FactBell || !facts[0].F.Equal(dec("0.999")) || string(facts[0].Detail) != `{"f_source":"candle","index_usdt":"40","minute_ms":`+itoa(bellMin.UnixMilli())+`}` {
		t.Errorf("facts after the restart = %+v detail %s", facts, facts[0].Detail)
	}
}

// The candle's close is fixed at the first read, whatever the source answers
// later: an index that only arrives eleven minutes on is multiplied by it.
func TestAnchorBellFixesTheCandleFWhenTheIndexIsLate(t *testing.T) {
	start := monClose.Add(3 * time.Hour)
	r := newRig(t, testConfig(), start)
	r.issuer.err = ErrIssuerKey
	r.fx.candles[bellMin.UnixMilli()] = dec("0.999")
	if a := r.stepAt(start); a.Kind != AnchorNone {
		t.Fatalf("an anchor without the index: %+v", a)
	}
	// The source revises its candle; the index arrives eleven minutes on.
	r.fx.candles[bellMin.UnixMilli()] = dec("0.998")
	r.stepAt(start.Add(5 * time.Minute))
	r.index.setClose("SOXLUSDT", bellMin, "40")
	r.index.setClose("SOXSUSDT", bellMin, "50")
	a := r.stepAt(start.Add(11 * time.Minute))
	wantAnchors(t, a, AnchorBell, "39.96", "49.95") // 0.999 x 40, 0.999 x 50
	if r.fx.candleCalls != 1 {
		t.Errorf("%d candle reads, want the one that fixed F", r.fx.candleCalls)
	}
	if facts := r.facts(); len(facts) != 4 || facts[0].Kind != FactBellF || facts[0].Source != "candle" || !facts[0].Value.Equal(dec("0.999")) || facts[0].KnownMs != start.UnixMilli() {
		t.Errorf("anchor facts = %+v", facts)
	}
}

// itoa writes an integer for an expected JSON text.
func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// The NAV of the Session's trading day replaces the bell anchor at the first
// round that reads both legs' NAV, without a settle delay (owner items
// 96-97). For nav_watch after that switch the NAV is read on: a new value
// of either leg switches the anchor in that round, with the other leg's
// value kept. After the watch nothing more is read until the next Session.
func TestAnchorNAVAtTheFirstRoundReadingBothLegs(t *testing.T) {
	r := anchorRig(t, testConfig(), monClose) // nav_watch 1 h, anchor_poll 5 min
	wantAnchors(t, r.stepAt(monClose.Add(time.Minute)), AnchorBell, "39.96", "49.95")
	if a := r.m.Anchors(); !a.SwitchedAt.Equal(monClose.Add(time.Minute)) {
		t.Errorf("bell anchor switched at %s, want when it was read", showTime(a.SwitchedAt))
	}

	// Thirteen hours on the issuer publishes both NAVs: the next round takes
	// them at once.
	seen := monClose.Add(13 * time.Hour)
	wantAnchors(t, r.stepAt(seen.Add(-5*time.Minute)), AnchorBell, "39.96", "49.95")
	r.issuer.setNAV("SOXL", "40.10")
	r.issuer.setNAV("SOXS", "49.80")
	changed := r.m.ReferenceChanged()
	a := r.stepAt(seen)
	wantAnchors(t, a, AnchorNAV, "40.10", "49.80")
	if !a.KnownAt.Equal(seen) || !a.SwitchedAt.Equal(seen) {
		t.Errorf("NAV known at %s, switched at %s, want both at the round that read both legs", showTime(a.KnownAt), showTime(a.SwitchedAt))
	}
	if got := r.issuer.navDates[len(r.issuer.navDates)-1]; got != "2026-12-21" {
		t.Errorf("the NAV was asked for trade date %s, want the day that started the Session", got)
	}
	select {
	case <-changed:
	default:
		t.Error("ReferenceChanged was not closed by the switch to NAV")
	}

	// Unchanged reads in the watch change nothing.
	wantAnchors(t, r.stepAt(seen.Add(5*time.Minute)), AnchorNAV, "40.10", "49.80")

	// Ten minutes on the bull fund's NAV is revised while the bear fund's
	// read fails: the anchor switches in that round, the bear leg keeps its
	// value.
	revised := seen.Add(10 * time.Minute)
	r.issuer.setNAV("SOXL", "40.12")
	r.issuer.navErr = map[string]error{"SOXS": errors.New("direxion: http 502")}
	a = r.stepAt(revised)
	wantAnchors(t, a, AnchorNAV, "40.12", "49.80")
	if !a.KnownAt.Equal(revised) || !a.SwitchedAt.Equal(revised) {
		t.Errorf("revised NAV known at %s, switched at %s, want the round that read it", showTime(a.KnownAt), showTime(a.SwitchedAt))
	}
	r.issuer.navErr = nil

	// The facts: a nav_seen per value seen, the two nav rows of each switch.
	var kinds []string
	for _, f := range r.facts() {
		kinds = append(kinds, f.Kind+"/"+f.Leg+"/"+f.Value.String())
	}
	want := []string{"bell_f/bull/0.999", "bell_f/bear/0.999", "bell/bull/39.96", "bell/bear/49.95",
		"nav_seen/bull/40.1", "nav_seen/bear/49.8", "nav/bull/40.1", "nav/bear/49.8",
		"nav_seen/bull/40.12", "nav/bull/40.12", "nav/bear/49.8"}
	if !slices.Equal(kinds, want) {
		t.Errorf("anchor facts = %v\nwant %v", kinds, want)
	}

	// The watch runs an hour from the first switch, not from the revision:
	// the round at seen + 55 min is the last one that reads.
	r.stepAt(seen.Add(55 * time.Minute))
	calls := r.issuer.navCalls
	r.issuer.setNAV("SOXL", "41")
	for at := seen.Add(time.Hour); at.Before(seen.Add(3 * time.Hour)); at = at.Add(5 * time.Minute) {
		wantAnchors(t, r.stepAt(at), AnchorNAV, "40.12", "49.80")
	}
	if r.issuer.navCalls != calls {
		t.Errorf("%d NAV reads after the watch", r.issuer.navCalls-calls)
	}

	// The next close starts a new Session without an anchor, and the reads
	// start over; the issuer has no NAV of the new day yet.
	r.issuer.navDate = "2026-12-21"
	next := ny(2026, 12, 22, 16, 0)
	if a := r.stepAt(next); a.SessionID != "2026-12-22" || a.Kind != AnchorNone || !a.Bull.IsZero() {
		t.Errorf("at the next close: %+v, want a new Session without an anchor", a)
	}
	if r.issuer.navCalls == calls {
		t.Error("the new Session did not read the NAV")
	}
}

// Both legs always use the same kind, and the NAV replaces the bell anchor
// only at a round that read both legs: one leg read on one round and the
// other on the next is not yet a switch.
func TestAnchorNAVNeedsBothLegsInOneRound(t *testing.T) {
	r := anchorRig(t, testConfig(), monClose)
	r.stepAt(monClose.Add(time.Minute))
	r.issuer.setNAV("SOXL", "40.10")
	for at := monClose.Add(5 * time.Minute); at.Before(monClose.Add(4 * time.Hour)); at = at.Add(5 * time.Minute) {
		wantAnchors(t, r.stepAt(at), AnchorBell, "39.96", "49.95")
	}
	// The bear fund's NAV appears in a round whose bull read fails.
	late := monClose.Add(4 * time.Hour)
	r.issuer.setNAV("SOXS", "49.80")
	r.issuer.navErr = map[string]error{"SOXL": errors.New("direxion: timeout")}
	wantAnchors(t, r.stepAt(late), AnchorBell, "39.96", "49.95")
	// The next round reads both.
	r.issuer.navErr = nil
	a := r.stepAt(late.Add(5 * time.Minute))
	wantAnchors(t, a, AnchorNAV, "40.10", "49.80")
	if !a.KnownAt.Equal(late.Add(5 * time.Minute)) {
		t.Errorf("NAV known at %s, want the round that read both legs", showTime(a.KnownAt))
	}
}

// Without a usable key the issuer is not read again: the Session stays on
// the bell anchor and one alarm says so.
func TestAnchorIssuerKeyMissingStaysOnBell(t *testing.T) {
	r := anchorRig(t, testConfig(), monClose)
	r.issuer.err = errors.Join(errors.New("direxion: http 403"), ErrIssuerKey)
	for at := monClose; at.Before(monClose.Add(2 * time.Hour)); at = at.Add(5 * time.Minute) {
		r.stepAt(at)
	}
	wantAnchors(t, r.m.Anchors(), AnchorBell, "39.96", "49.95")
	if r.issuer.navCalls != 1 || r.issuer.distCalls != 0 {
		t.Errorf("the issuer was asked %d NAVs and %d distributions after refusing the key, want 1 and 0", r.issuer.navCalls, r.issuer.distCalls)
	}
	if got := r.events(); !slices.Equal(got, []string{"issuer_key"}) {
		t.Errorf("events = %v, want one issuer_key alarm", got)
	}
}

// A dividend bans entries from 04:00 New York of its carrier day to its
// effective time T. With the amount read by T, the anchor drops by it at T,
// the mean restarts at T and entries may go on.
func TestAnchorDividendWindowAndAdjustmentAtT(t *testing.T) {
	// The Session before the carrier Session: Friday's close to Monday's.
	friClose := ny(2026, 12, 18, 16, 0)
	r := newRig(t, withDividends(), friClose)
	r.index.setClose("SOXLUSDT", ny(2026, 12, 18, 15, 59), "41")
	r.index.setClose("SOXSUSDT", ny(2026, 12, 18, 15, 59), "51")
	r.fx.candles[ny(2026, 12, 18, 15, 59).UnixMilli()] = dec("1")
	r.index.setClose("SOXLUSDT", bellMin, "40")
	r.index.setClose("SOXSUSDT", bellMin, "50")
	r.fx.candles[bellMin.UnixMilli()] = dec("0.9990")
	r.issuer.distributions["SOXL"] = []Distribution{{ExDate: "2026-09-22", Amount: dec("0.05"), Declared: true}, {ExDate: "2026-12-22"}}
	r.issuer.distributions["SOXS"] = []Distribution{{ExDate: "2026-12-22"}}

	// Over the weekend there is no ban; it begins at Monday 04:00 New York.
	if a := r.stepAt(friClose.Add(time.Minute)); a.EntryBan || a.Kind != AnchorBell {
		t.Fatalf("Friday after the close: %+v", a)
	}
	if a := r.stepAt(ny(2026, 12, 21, 3, 59)); a.EntryBan {
		t.Fatal("banned before the carrier day's pre-market")
	}
	changed := r.m.ReferenceChanged()
	a := r.stepAt(ny(2026, 12, 21, 4, 0))
	if !a.EntryBan || a.Kind != AnchorBell || !a.RestartAt.IsZero() {
		t.Fatalf("at Monday 04:00: %+v, want the ban and the anchor unchanged", a)
	}
	wantAnchors(t, a, AnchorBell, "41", "51")
	select {
	case <-changed:
	default:
		t.Fatal("the start of the ban was not published")
	}
	if a := r.stepAt(monClose.Add(-time.Minute)); !a.EntryBan {
		t.Fatal("the ban ended before the carrier day's close")
	}
	// So far only the daily check has asked the issuer, on two days for both
	// funds: the amount is not read before the carrier day's close.
	if r.issuer.distCalls != 4 {
		t.Fatalf("%d distribution reads before the carrier day's close, want the two daily checks of both funds", r.issuer.distCalls)
	}

	// The carrier Session: banned from its start, the amounts read every
	// five minutes from the close until they are there.
	if a := r.stepAt(monClose); !a.EntryBan || a.Kind != AnchorNone || a.SessionID != "2026-12-21" {
		t.Fatalf("at Monday's close: %+v", a)
	}
	a = r.stepAt(monClose.Add(time.Minute))
	wantAnchors(t, a, AnchorBell, "39.96", "49.95")
	if !a.EntryBan {
		t.Fatal("no ban in the carrier Session before T")
	}
	r.stepAt(monClose.Add(4 * time.Minute))
	if r.issuer.distCalls != 6 {
		t.Fatalf("%d distribution reads, want one more per fund at the close and none within five minutes", r.issuer.distCalls)
	}
	declared := monClose.Add(2 * time.Hour)
	for at := monClose.Add(5 * time.Minute); at.Before(declared); at = at.Add(5 * time.Minute) {
		r.stepAt(at)
	}
	r.issuer.distributions["SOXL"] = []Distribution{{ExDate: "2026-09-22", Amount: dec("0.05"), Declared: true}, {ExDate: "2026-12-22", Amount: dec("0.11"), Declared: true}}
	r.issuer.distributions["SOXS"] = []Distribution{{ExDate: "2026-12-22", Amount: dec("0.21"), Declared: true}}
	a = r.stepAt(declared)
	// Known before T: nothing changes yet.
	wantAnchors(t, a, AnchorBell, "39.96", "49.95")
	if !a.EntryBan || !a.RestartAt.IsZero() {
		t.Fatalf("with the amount known before T: %+v, want the ban and no restart yet", a)
	}
	reads := r.issuer.distCalls
	effective := ny(2026, 12, 21, 20, 0)
	a = r.stepAt(effective.Add(-time.Second))
	if !a.EntryBan || !a.RestartAt.IsZero() {
		t.Fatalf("a second before T: %+v", a)
	}
	if r.issuer.distCalls != reads {
		t.Error("the amount was read again after it was known")
	}

	// At T: A - Div on both legs, the mean restarts, entries may go on.
	a = r.stepAt(effective)
	wantAnchors(t, a, AnchorBell, "39.85", "49.74")
	if a.EntryBan || !a.RestartAt.Equal(effective) || !a.KnownAt.Equal(effective) {
		t.Fatalf("at T: ban %t restart %s known %s, want no ban, both at T", a.EntryBan, showTime(a.RestartAt), showTime(a.KnownAt))
	}

	// A NAV that replaces the bell anchor after T is reduced too; the
	// restart time stays T, and the switch time is the NAV's.
	r.issuer.setNAV("SOXL", "40.10")
	r.issuer.setNAV("SOXS", "49.80")
	a = r.stepAt(effective.Add(5 * time.Minute))
	wantAnchors(t, a, AnchorNAV, "39.99", "49.59")
	if a.EntryBan || !a.RestartAt.Equal(effective) || !a.KnownAt.Equal(effective.Add(5*time.Minute)) || !a.SwitchedAt.Equal(effective.Add(5*time.Minute)) {
		t.Fatalf("NAV after T: %+v", a)
	}

	// The dividend facts carry the amounts and when they were read.
	var dividends []store.AnchorFactRow
	for _, f := range r.facts() {
		if f.Kind == FactDividend {
			dividends = append(dividends, f)
		}
	}
	if len(dividends) != 2 || !dividends[0].Value.Equal(dec("0.11")) || dividends[0].Leg != "bull" || dividends[0].KnownMs != declared.UnixMilli() || !dividends[1].Value.Equal(dec("0.21")) {
		t.Errorf("dividend facts = %+v", dividends)
	}

	// The Session after it carries nothing of the dividend; the issuer has
	// no NAV of its day yet.
	r.issuer.navDate = "2026-12-21"
	tueClose := ny(2026, 12, 22, 16, 0)
	if a := r.stepAt(tueClose); a.EntryBan || !a.RestartAt.IsZero() || a.Kind != AnchorNone {
		t.Fatalf("the next Session: %+v", a)
	}
}

// At T without the amount the mean still restarts at T, but the anchor stays
// and entries stay banned; when the amount arrives the anchor drops and the
// ban ends, without a second restart.
func TestAnchorDividendAmountArrivingLate(t *testing.T) {
	cfg := testConfig()
	cfg.CorporateActions = []CorporateAction{{Ticker: "SOXS", ExDate: "2026-12-22", Kind: ActionDividend}} // one leg only
	r := anchorRig(t, cfg, monClose)
	r.issuer.distributions["SOXS"] = []Distribution{{ExDate: "2026-12-22"}} // listed, no amount yet
	r.stepAt(monClose.Add(time.Minute))
	effective := ny(2026, 12, 21, 20, 0)
	for at := monClose.Add(5 * time.Minute); at.Before(effective); at = at.Add(5 * time.Minute) {
		r.stepAt(at)
	}
	a := r.stepAt(effective)
	wantAnchors(t, a, AnchorBell, "39.96", "49.95")
	if !a.EntryBan || !a.RestartAt.Equal(effective) || !a.KnownAt.Equal(monClose.Add(time.Minute)) {
		t.Fatalf("at T without the amount: %+v, want the ban, the restart at T and the anchor as it was", a)
	}

	// Ten minutes after T the issuer has the amount.
	arrived := effective.Add(10 * time.Minute)
	r.stepAt(effective.Add(5 * time.Minute))
	r.issuer.distributions["SOXS"] = []Distribution{{ExDate: "2026-12-22", Amount: dec("0.21"), Declared: true}}
	a = r.stepAt(arrived)
	wantAnchors(t, a, AnchorBell, "39.96", "49.74") // only the bear leg pays
	if a.EntryBan || !a.RestartAt.Equal(effective) || !a.KnownAt.Equal(arrived) {
		t.Fatalf("after the amount arrived: ban %t restart %s known %s; want no ban, the restart still at T, known at the arrival",
			a.EntryBan, showTime(a.RestartAt), showTime(a.KnownAt))
	}
	// A declared amount of zero is an amount too: the ban ends, the anchor stays.
	cfg.CorporateActions[0].Ticker = "SOXL"
	r2 := anchorRig(t, cfg, monClose)
	r2.issuer.distributions["SOXL"] = []Distribution{{ExDate: "2026-12-22", Declared: true}}
	r2.stepAt(monClose.Add(time.Minute))
	a = r2.stepAt(effective)
	wantAnchors(t, a, AnchorBell, "39.96", "49.95")
	if a.EntryBan || !a.RestartAt.Equal(effective) {
		t.Fatalf("a declared zero dividend at T: %+v", a)
	}
}

// Once a day the funds' ex-dates are compared with corporate_actions: one
// that is still ahead and not configured is an alarm; past ones and
// configured ones are not.
func TestAnchorDistributionsMissingFromTheConfiguration(t *testing.T) {
	cfg := testConfig()
	cfg.CorporateActions = []CorporateAction{{Ticker: "SOXL", ExDate: "2026-12-22", Kind: ActionDividend}}
	r := anchorRig(t, cfg, monClose)
	r.issuer.distributions["SOXL"] = []Distribution{{ExDate: "2026-09-22", Amount: dec("0.05"), Declared: true}, {ExDate: "2026-12-22"}}
	r.issuer.distributions["SOXS"] = []Distribution{{ExDate: "2026-12-21", Amount: dec("0.1"), Declared: true}, {ExDate: "2026-12-22"}}
	r.stepAt(monClose)
	rows := r.sink.Rows("event")
	if len(rows) != 1 {
		t.Fatalf("events = %v, want one alarm for SOXS 2026-12-22", r.events())
	}
	if ev := rows[0].(store.EventRow); ev.Kind != "ex_date_not_configured" || ev.Level != "error" || string(ev.Detail) != `{"amount":"0","declared":false,"ex_date":"2026-12-22","ticker":"SOXS"}` {
		t.Errorf("event = %+v detail %s", ev, ev.Detail)
	}
	if a := r.m.Anchors(); a.EntryBan != true {
		t.Errorf("the configured SOXL dividend must ban entries in its carrier Session: %+v", a)
	}
	// Not again within the day; again a day later while it is still missing.
	r.stepAt(monClose.Add(12 * time.Hour))
	if n := len(r.events()); n != 1 {
		t.Fatalf("%d events within the day, want 1", n)
	}
	r.stepAt(monClose.Add(24 * time.Hour))
	if got := r.events(); len(got) != 1 {
		// By then the Session is 2026-12-22 and the ex-date no longer ahead.
		t.Fatalf("events a day later = %v: an ex-date that has passed is not alarmed", got)
	}
}

// A configured split stops entries from 04:00 New York of the last trading
// day before its ex-date and keeps them stopped, Session after Session with
// a reminder at each start, until the entry is removed and the process
// restarted. The anchors are not adjusted.
func TestAnchorSplitBansEntriesUntilRemoved(t *testing.T) {
	cfg := testConfig()
	cfg.CorporateActions = []CorporateAction{{Ticker: "SOXL", ExDate: "2026-12-22", Kind: ActionSplit}}
	r := anchorRig(t, cfg, ny(2026, 12, 21, 3, 0))
	r.index.setClose("SOXLUSDT", ny(2026, 12, 18, 15, 59), "41")
	r.index.setClose("SOXSUSDT", ny(2026, 12, 18, 15, 59), "51")
	r.fx.candles[ny(2026, 12, 18, 15, 59).UnixMilli()] = dec("1")

	if a := r.stepAt(ny(2026, 12, 21, 3, 59)); a.EntryBan {
		t.Fatal("banned before the split's window")
	}
	a := r.stepAt(ny(2026, 12, 21, 4, 0))
	wantAnchors(t, a, AnchorBell, "41", "51")
	if !a.EntryBan || !a.RestartAt.IsZero() {
		t.Fatalf("at 04:00 of the day before the ex-date: %+v, want the ban and nothing else", a)
	}
	if n := len(r.events()); n != 0 {
		t.Fatalf("%d reminders before a Session started inside the window", n)
	}
	// Through the ex-date and the days after it: still banned, one reminder
	// per Session start, the anchors those of each Session unadjusted.
	a = r.stepAt(monClose.Add(time.Minute))
	wantAnchors(t, a, AnchorBell, "39.96", "49.95")
	for _, at := range []time.Time{ny(2026, 12, 22, 12, 0), ny(2026, 12, 22, 16, 0), ny(2026, 12, 23, 16, 0), ny(2026, 12, 28, 10, 0)} {
		if a := r.stepAt(at); !a.EntryBan {
			t.Fatalf("at %s the ban had ended: %+v", showTime(at), a)
		}
	}
	if got := r.events(); !slices.Equal(got, []string{"split_entry_ban", "split_entry_ban", "split_entry_ban", "split_entry_ban"}) {
		t.Errorf("events = %v, want one reminder for each of the four Sessions started inside the window", got)
	}

	// Restarted without the entry: no ban.
	r2 := anchorRig(t, testConfig(), ny(2026, 12, 28, 10, 0))
	if a := r2.stepAt(ny(2026, 12, 28, 10, 0)); a.EntryBan {
		t.Errorf("without the split entry: %+v", a)
	}
}

// A restart inside a Session continues from the stored facts: the bell
// anchor with its known-at time and without a new read, the latest NAV of
// the Session and the watch that runs from its first switch.
func TestAnchorRestoreContinuesTheSession(t *testing.T) {
	// The first run reads the bell anchor, switches to the NAV and takes a
	// revision of it.
	r := anchorRig(t, withDividends(), monClose)
	r.issuer.distributions["SOXL"] = []Distribution{{ExDate: "2026-12-22", Amount: dec("0.11"), Declared: true}}
	r.issuer.distributions["SOXS"] = []Distribution{{ExDate: "2026-12-22", Amount: dec("0.21"), Declared: true}}
	bellAt, effective := monClose.Add(time.Minute), ny(2026, 12, 21, 20, 0)
	before := r.stepAt(bellAt)
	wantAnchors(t, before, AnchorBell, "39.96", "49.95")
	seen := monClose.Add(13 * time.Hour)
	r.issuer.setNAV("SOXL", "40.10")
	r.issuer.setNAV("SOXS", "49.80")
	wantAnchors(t, r.stepAt(seen), AnchorNAV, "39.99", "49.59") // past T: the NAV less the dividends
	revised := seen.Add(10 * time.Minute)
	r.issuer.setNAV("SOXL", "40.12")
	before = r.stepAt(revised)
	wantAnchors(t, before, AnchorNAV, "40.01", "49.59")
	facts := AnchorFactsFromRows(r.facts())

	// The second run starts ten minutes later, inside the watch, with those
	// facts. Its sources could not give the bell anchor again.
	restart := revised.Add(10 * time.Minute)
	r2 := newRig(t, withDividends(), restart)
	r2.issuer.setNAV("SOXL", "40.12")
	r2.issuer.setNAV("SOXS", "49.80")
	r2.m.Restore(facts, nil)
	a := r2.stepAt(restart)
	wantAnchors(t, a, AnchorNAV, "40.01", "49.59")
	if !a.KnownAt.Equal(revised) || !a.SwitchedAt.Equal(revised) || !a.RestartAt.Equal(effective) || a.EntryBan {
		t.Fatalf("after the restart: known %s switched %s restart %s ban %t; want the revision's time, T, no ban",
			showTime(a.KnownAt), showTime(a.SwitchedAt), showTime(a.RestartAt), a.EntryBan)
	}
	if !sameAnchors(a, before) {
		t.Errorf("the restart changed the anchors:\n before %+v\n after  %+v", before, a)
	}
	if r2.index.calls != 0 || r2.fx.candleCalls != 0 || r2.issuer.navCalls != 2 {
		t.Errorf("after the restart: %d index reads, %d candle reads, %d NAV reads; want 0, 0 and the watch's read of both legs", r2.index.calls, r2.fx.candleCalls, r2.issuer.navCalls)
	}
	if n := len(r2.facts()); n != 0 {
		t.Errorf("the restart recorded %d facts it already had", n)
	}
	// The watch goes on: a revision switches again ...
	r2.issuer.setNAV("SOXS", "49.70")
	a = r2.stepAt(restart.Add(5 * time.Minute))
	wantAnchors(t, a, AnchorNAV, "40.01", "49.49")
	// ... and ends an hour after the first switch, not after the restart.
	r2.stepAt(seen.Add(55 * time.Minute))
	calls := r2.issuer.navCalls
	r2.issuer.setNAV("SOXS", "49")
	for at := seen.Add(time.Hour); at.Before(seen.Add(2 * time.Hour)); at = at.Add(5 * time.Minute) {
		r2.stepAt(at)
	}
	if r2.issuer.navCalls != calls {
		t.Errorf("%d NAV reads after the watch of the restored Session", r2.issuer.navCalls-calls)
	}

	// A third run after the watch has the latest NAV at once and reads
	// nothing; facts of another Session are ignored.
	facts = append(facts, AnchorFactsFromRows(r2.facts())...)
	facts = append(facts, AnchorFact{SessionID: "2026-12-18", Kind: FactNAV, Leg: Bull, Value: dec("1"), KnownAt: monClose}, AnchorFact{SessionID: "2026-12-18", Kind: FactNAV, Leg: Bear, Value: dec("1"), KnownAt: monClose})
	r3 := newRig(t, withDividends(), seen.Add(2*time.Hour))
	r3.m.Restore(facts, nil)
	a = r3.stepAt(seen.Add(2 * time.Hour))
	wantAnchors(t, a, AnchorNAV, "40.01", "49.49")
	if !a.KnownAt.Equal(restart.Add(5*time.Minute)) || r3.issuer.navCalls != 0 {
		t.Errorf("restored NAV: known %s, %d NAV reads; want the last switch's time and no read", showTime(a.KnownAt), r3.issuer.navCalls)
	}

	// Without stored facts a restart starts the Session's reads over.
	r4 := anchorRig(t, testConfig(), restart)
	a = r4.stepAt(restart)
	wantAnchors(t, a, AnchorBell, "39.96", "49.95")
	if !a.KnownAt.Equal(restart) {
		t.Errorf("without facts the bell anchor is known at %s, want the restart", showTime(a.KnownAt))
	}
}

// A restart that stored a NAV seen for one leg only, or for both legs on
// different rounds, has no NAV yet: the next round that reads both legs
// switches, as an uninterrupted run would.
func TestAnchorRestoreBeforeTheFirstSwitch(t *testing.T) {
	seen := monClose.Add(13 * time.Hour)
	facts := []AnchorFact{
		{SessionID: "2026-12-21", Kind: FactBell, Leg: Bull, Value: dec("39.96"), F: dec("0.999"), Source: "binance", KnownAt: monClose.Add(time.Minute)},
		{SessionID: "2026-12-21", Kind: FactBell, Leg: Bear, Value: dec("49.95"), F: dec("0.999"), Source: "binance", KnownAt: monClose.Add(time.Minute)},
		{SessionID: "2026-12-21", Kind: FactNAVSeen, Leg: Bull, Value: dec("40.10"), Source: "direxion", KnownAt: seen},
	}
	restart := seen.Add(2 * time.Minute)
	r := newRig(t, testConfig(), restart)
	r.m.Restore(facts, nil)
	wantAnchors(t, r.stepAt(restart), AnchorBell, "39.96", "49.95") // the issuer has nothing now
	r.issuer.setNAV("SOXL", "40.10")
	r.issuer.setNAV("SOXS", "49.80")
	a := r.stepAt(restart.Add(5 * time.Minute))
	wantAnchors(t, a, AnchorNAV, "40.10", "49.80")
	var kinds []string
	for _, f := range r.facts() {
		kinds = append(kinds, f.Kind+"/"+f.Leg+"/"+f.Value.String())
	}
	if want := []string{"nav_seen/bear/49.8", "nav/bull/40.1", "nav/bear/49.8"}; !slices.Equal(kinds, want) {
		t.Errorf("facts after the restart = %v, want %v (the bull value was seen before)", kinds, want)
	}
}

// A stored bell anchor of one leg only is not a bell anchor: the restarted
// process reads both legs again, and the anchor is known from that read.
func TestAnchorRestoreOfOneLegReadsTheBellAgain(t *testing.T) {
	restart := monClose.Add(3 * time.Hour)
	r := anchorRig(t, testConfig(), restart)
	r.m.Restore([]AnchorFact{{SessionID: "2026-12-21", Kind: FactBell, Leg: Bull, Value: dec("12.34"), F: dec("1"), Source: "binance", KnownAt: monClose.Add(time.Minute)}}, nil)
	a := r.stepAt(restart)
	wantAnchors(t, a, AnchorBell, "39.96", "49.95") // from the sources, not the 12.34 stored for one leg
	if !a.KnownAt.Equal(restart) || r.index.calls != 2 {
		t.Errorf("known at %s after %d index reads, want the restart and a read of both legs", showTime(a.KnownAt), r.index.calls)
	}
	// While the index is not there the half-stored anchor is not used either.
	r2 := newRig(t, testConfig(), restart)
	r2.m.Restore([]AnchorFact{{SessionID: "2026-12-21", Kind: FactBell, Leg: Bear, Value: dec("12.34"), F: dec("1"), Source: "binance", KnownAt: monClose.Add(time.Minute)}}, nil)
	if a := r2.stepAt(restart); a.Kind != AnchorNone || !a.Bull.IsZero() || !a.Bear.IsZero() {
		t.Errorf("one stored leg and no index: %+v, want no anchor", a)
	}
}

// The rows read back at start convert to facts; a row of an unknown kind or
// leg is left out.
func TestAnchorFactsFromRows(t *testing.T) {
	rows := []store.AnchorFactRow{
		{SessionID: "2026-12-21", Kind: "bell", Leg: "bear", KnownMs: 1798000000000, Value: dec("49.95"), F: dec("0.999"), Source: "binance"},
		{SessionID: "2026-12-21", Kind: "split", Leg: "bull", KnownMs: 1, Value: dec("2")},
		{SessionID: "2026-12-21", Kind: "nav", Leg: "middle", KnownMs: 1, Value: dec("2")},
	}
	got := AnchorFactsFromRows(rows)
	if len(got) != 1 || got[0].Leg != Bear || got[0].Kind != FactBell || !got[0].Value.Equal(dec("49.95")) || !got[0].F.Equal(dec("0.999")) || got[0].KnownAt.UnixMilli() != 1798000000000 {
		t.Errorf("facts = %+v", got)
	}
}

// A bell F the source does not have yet, or a read of it that failed, is
// read again after referenceRetry, not after anchorPoll: one failed read does
// not hold the bell anchor up for five minutes. Without the candle there is
// still no bell anchor.
func TestAnchorBellFIsReadAgainSoon(t *testing.T) {
	r := newRig(t, testConfig(), monClose)
	r.issuer.err = ErrIssuerKey
	r.index.setClose("SOXLUSDT", bellMin, "40")
	r.index.setClose("SOXSUSDT", bellMin, "50")
	r.fx.candleErr = errors.New("coinbase: timeout")
	if a := r.stepAt(monClose.Add(time.Minute)); a.Kind != AnchorNone {
		t.Fatalf("an anchor without F: %+v", a)
	}
	r.fx.candleErr = nil
	if a := r.stepAt(monClose.Add(time.Minute + 3*time.Second)); a.Kind != AnchorNone || r.fx.candleCalls != 1 {
		t.Fatalf("read again before referenceRetry: %+v, %d candle reads", a, r.fx.candleCalls)
	}
	// The candle is still missing at the retry: no bell anchor yet.
	if a := r.stepAt(monClose.Add(time.Minute + 5*time.Second)); a.Kind != AnchorNone || r.fx.candleCalls != 2 {
		t.Fatalf("at the retry without a candle: %+v, %d candle reads", a, r.fx.candleCalls)
	}
	r.fx.candles[bellMin.UnixMilli()] = dec("0.999")
	a := r.stepAt(monClose.Add(time.Minute + 10*time.Second))
	wantAnchors(t, a, AnchorBell, "39.96", "49.95")
}
