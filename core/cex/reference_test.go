package cex

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"etf-pair-arbitrage/core/store"
)

// soxlRule and soxsRule are the two contracts' rules as the exchange lists
// them.
var (
	soxlRule = MarketRule{Symbol: "SOXLUSDT", PriceTick: dec("0.01"), QtyStep: dec("0.01"), MinQty: dec("0.01"), MinNotional: dec("5"), Online: true}
	soxsRule = MarketRule{Symbol: "SOXSUSDT", PriceTick: dec("0.001"), QtyStep: dec("0.01"), MinQty: dec("0.01"), MinNotional: dec("5"), Online: true}
)

// goodSettings is an account set up as the strategy needs with leverage 10.
func goodSettings() AccountSettings {
	return AccountSettings{OneWay: true, BySymbol: map[Symbol]SymbolSetting{"SOXLUSDT": {Crossed: true, Leverage: 10}, "SOXSUSDT": {Crossed: true, Leverage: 10}}}
}

// An order is sendable when it has the minimum quantity and the minimum
// value.
func TestMarketRuleSendable(t *testing.T) {
	for _, c := range []struct {
		qty, price string
		want       bool
	}{{"0.13", "40", true}, {"0.125", "40", true}, {"0.12", "40", false}, {"0.005", "2000", false}, {"1", "5", true}} {
		if got := soxlRule.Sendable(dec(c.qty), dec(c.price)); got != c.want {
			t.Errorf("Sendable(%s, %s) = %t, want %t", c.qty, c.price, got, c.want)
		}
	}
}

// The rules are published once read; trading is ready when both contracts
// are listed online.
func TestRulesAndTradingReady(t *testing.T) {
	r := newRig(t, testConfig(), t0)
	ctx := context.Background()
	if _, ok := r.m.Rule(Bull); ok || r.m.TradingReady() {
		t.Fatal("a rule or readiness before any read")
	}
	// A read that lacks one contract publishes nothing.
	r.venue.rules = map[Symbol]MarketRule{"SOXLUSDT": soxlRule}
	if err := r.m.readRules(ctx, t0); err == nil {
		t.Fatal("a read without the bear contract's rule succeeded")
	}
	if _, ok := r.m.Rule(Bull); ok {
		t.Fatal("half a read was published")
	}

	r.venue.rules["SOXSUSDT"] = soxsRule
	changed := r.m.ReferenceChanged()
	if err := r.m.readRules(ctx, t0); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
	default:
		t.Fatal("ReferenceChanged was not closed by the rules")
	}
	if rule, ok := r.m.Rule(Bear); !ok || !rule.PriceTick.Equal(dec("0.001")) || !r.m.TradingReady() {
		t.Errorf("bear rule = %+v (%t), ready %t", rule, ok, r.m.TradingReady())
	}
	// An unchanged re-read publishes nothing.
	changed = r.m.ReferenceChanged()
	r.m.readRules(ctx, t0)
	select {
	case <-changed:
		t.Error("unchanged rules were published again")
	default:
	}
	// A contract going offline ends readiness.
	offline := soxsRule
	offline.Online = false
	r.venue.rules["SOXSUSDT"] = offline
	r.m.readRules(ctx, t0)
	if r.m.TradingReady() {
		t.Error("ready with the bear contract offline")
	}
}

// SettingsOK holds only for one-way mode with cross margin and the wanted
// leverage on both contracts; whatever differs is named, raised as an alarm
// once, and the venue is only ever read (PRD 6.7, V17).
func TestSettingsOK(t *testing.T) {
	r := newRig(t, testConfig(), t0)
	ctx := context.Background()
	if ok, detail := r.m.SettingsOK(10); ok || !strings.Contains(detail, "not been read") {
		t.Fatalf("before any read: ok %t, %q", ok, detail)
	}
	read := func(s AccountSettings) (bool, string) {
		t.Helper()
		r.venue.settings = s
		if err := r.m.readSettings(ctx, t0); err != nil {
			t.Fatal(err)
		}
		return r.m.SettingsOK(10)
	}
	if ok, detail := read(goodSettings()); !ok || detail != "" {
		t.Fatalf("good settings: ok %t, %q", ok, detail)
	}
	if n := len(r.events()); n != 0 {
		t.Fatalf("%d events for settings that match", n)
	}

	hedge := goodSettings()
	hedge.OneWay = false
	isolated := goodSettings()
	isolated.BySymbol["SOXSUSDT"] = SymbolSetting{Crossed: false, Leverage: 10}
	leverage := goodSettings()
	leverage.BySymbol["SOXLUSDT"] = SymbolSetting{Crossed: true, Leverage: 20}
	missing := goodSettings()
	delete(missing.BySymbol, "SOXSUSDT")
	for _, c := range []struct {
		name     string
		settings AccountSettings
		want     string
	}{
		{"hedge mode", hedge, "position mode is not one-way"},
		{"isolated margin on one leg", isolated, "SOXSUSDT margin mode is not crossed"},
		{"another leverage on one leg", leverage, "SOXLUSDT leverage is 20, want 10"},
		{"a leg without a setting", missing, "SOXSUSDT has no setting on the account"},
	} {
		before := len(r.events())
		ok, detail := read(c.settings)
		if ok || detail != c.want {
			t.Errorf("%s: ok %t, detail %q, want %q", c.name, ok, detail, c.want)
		}
		// Asked again, as exec does every beat: the same answer, no second alarm.
		r.m.SettingsOK(10)
		if got := r.events()[before:]; !slices.Equal(got, []string{"account_settings_differ"}) {
			t.Errorf("%s: events %v, want one account_settings_differ", c.name, got)
		}
	}
	// The same account against another wanted leverage differs on both legs.
	r.venue.settings = goodSettings()
	r.m.readSettings(ctx, t0)
	if ok, detail := r.m.SettingsOK(5); ok || detail != "SOXLUSDT leverage is 10, want 5; SOXSUSDT leverage is 10, want 5" {
		t.Errorf("leverage 5 wanted: ok %t, %q", ok, detail)
	}
	if n := r.venue.callCount("PlaceOrder"); n != 0 {
		t.Errorf("the venue got %d order calls from reference reads", n)
	}
}

// ReadPositions publishes the two legs' positions and nothing of another
// contract; Positions is not loaded before the first read.
func TestPositions(t *testing.T) {
	r := newRig(t, testConfig(), t0)
	ctx := context.Background()
	if p := r.m.Positions(); p.Loaded {
		t.Fatal("positions are loaded before any read")
	}
	r.venue.err = errors.New("timeout")
	if _, err := r.m.ReadPositions(ctx); err == nil || r.m.Positions().Loaded {
		t.Fatal("a failed read published positions")
	}
	r.venue.err = nil
	r.venue.positions = PositionSet{BySymbol: map[Symbol]Position{
		"SOXLUSDT": {Symbol: "SOXLUSDT", Qty: dec("12.5"), AvgPrice: dec("40.4")},
		"BTCUSDT":  {Symbol: "BTCUSDT", Qty: dec("-1")}}}
	changed := r.m.AccountChanged()
	got, err := r.m.ReadPositions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
	default:
		t.Fatal("AccountChanged was not closed by the positions")
	}
	published := r.m.Positions()
	if !got.Loaded || !published.Loaded || len(published.BySymbol) != 1 || !published.BySymbol["SOXLUSDT"].Qty.Equal(dec("12.5")) || !published.ReceivedAt.Equal(t0) {
		t.Errorf("positions = %+v, want only the bull leg's 12.5, received now", published)
	}
}

// The funding records of the two legs are kept once each, oldest first;
// other contracts' records are left out; only a new record publishes.
func TestFinancialRecords(t *testing.T) {
	r := newRig(t, testConfig(), t0)
	ctx := context.Background()
	rec := func(id string, symbol Symbol, amount string, at time.Time) FinancialRecord {
		return FinancialRecord{ID: id, Symbol: symbol, Type: "funding", Amount: dec(amount), At: at}
	}
	r.venue.records = []FinancialRecord{rec("3", "SOXSUSDT", "0.02", t0.Add(-8*time.Hour)), rec("9", "BTCUSDT", "-1", t0.Add(-8*time.Hour)), rec("1", "SOXLUSDT", "-0.01", t0.Add(-16*time.Hour))}
	if err := r.m.readFinancialRecords(ctx, t0); err != nil {
		t.Fatal(err)
	}
	ids := func() []string {
		var out []string
		for _, rec := range r.m.FinancialRecords() {
			out = append(out, rec.ID)
		}
		return out
	}
	if got := ids(); !slices.Equal(got, []string{"1", "3"}) {
		t.Fatalf("records = %v, want the two legs' records oldest first", got)
	}
	if since := r.venue.recordsSince[0]; !since.Equal(t0.Add(-7 * 24 * time.Hour)) {
		t.Errorf("the first read went back to %s, want the lookback of seven days", since)
	}

	// The next read overlaps the first: the same records again and one new.
	later := t0.Add(time.Minute)
	r.venue.records = append(r.venue.records, rec("5", "SOXLUSDT", "-0.03", t0.Add(30*time.Second)))
	changed := r.m.AccountChanged()
	r.m.readFinancialRecords(ctx, later)
	select {
	case <-changed:
	default:
		t.Fatal("AccountChanged was not closed by a new record")
	}
	if got := ids(); !slices.Equal(got, []string{"1", "3", "5"}) {
		t.Errorf("records = %v, want 1, 3, 5", got)
	}
	if since := r.venue.recordsSince[1]; !since.Equal(t0.Add(-time.Minute)) {
		t.Errorf("the second read asked from %s, want one interval before the first read", since)
	}
	changed = r.m.AccountChanged()
	r.m.readFinancialRecords(ctx, later.Add(time.Minute))
	select {
	case <-changed:
		t.Error("a read without a new record published")
	default:
	}
}

// The settled funding rates are found by leg and settlement time, and stay
// when a later read no longer lists them.
func TestFundingRateAt(t *testing.T) {
	r := newRig(t, testConfig(), t0)
	ctx := context.Background()
	s1, s2 := time.Date(2026, 11, 23, 16, 0, 0, 0, time.UTC), time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)
	r.venue.funding["SOXLUSDT"] = []FundingSettlement{{Symbol: "SOXLUSDT", Rate: dec("0.0001"), At: s1}}
	r.venue.funding["SOXSUSDT"] = []FundingSettlement{{Symbol: "SOXSUSDT", Rate: dec("0"), At: s1}}
	if err := r.m.readFundingRates(ctx, t0); err != nil {
		t.Fatal(err)
	}
	r.venue.funding["SOXLUSDT"] = []FundingSettlement{{Symbol: "SOXLUSDT", Rate: dec("-0.0002"), At: s2}}
	r.m.readFundingRates(ctx, t0)
	if rate, ok := r.m.FundingRateAt(Bull, s1); !ok || !rate.Equal(dec("0.0001")) {
		t.Errorf("bull rate at the first settlement = %s (%t)", rate, ok)
	}
	if rate, ok := r.m.FundingRateAt(Bull, s2); !ok || !rate.Equal(dec("-0.0002")) {
		t.Errorf("bull rate at the second settlement = %s (%t)", rate, ok)
	}
	if rate, ok := r.m.FundingRateAt(Bear, s1); !ok || !rate.IsZero() {
		t.Errorf("bear rate at the first settlement = %s (%t), want a known zero", rate, ok)
	}
	if _, ok := r.m.FundingRateAt(Bear, s2); ok {
		t.Error("a settlement that was never read has a rate")
	}
}

// A reference read that fails is tried again after referenceRetry, one that
// succeeded after its own interval; the positions are read until the first
// success and then no more.
func TestReferenceReadsRetryAndRepeat(t *testing.T) {
	r := newRig(t, testConfig(), t0)
	ctx := context.Background()
	r.venue.rules = map[Symbol]MarketRule{"SOXLUSDT": soxlRule, "SOXSUSDT": soxsRule}
	r.venue.settings = goodSettings()
	r.venue.err = errors.New("http 502")
	reads := r.m.referenceReads()
	step := func(after time.Duration) { r.m.stepReference(ctx, reads, t0.Add(after)) }

	step(0)
	step(4 * time.Second) // before the retry is due
	if n := r.venue.callCount("MarketRules"); n != 1 {
		t.Fatalf("%d rule reads within the retry delay, want 1", n)
	}
	step(5 * time.Second)
	if n := r.venue.callCount("MarketRules"); n != 2 {
		t.Fatalf("%d rule reads after the retry delay, want 2", n)
	}
	r.venue.err = nil
	step(10 * time.Second)
	if !r.m.TradingReady() || !r.m.Positions().Loaded {
		t.Fatal("after the venue recovered the rules or the positions are still missing")
	}
	rules, settings, positions := r.venue.callCount("MarketRules"), r.venue.callCount("AccountSettings"), r.venue.callCount("Positions")

	// Nothing is due a second later; the settings a minute later, the rules
	// an hour later; the positions never again.
	step(11 * time.Second)
	if r.venue.callCount("MarketRules") != rules || r.venue.callCount("AccountSettings") != settings {
		t.Error("a read was repeated before its interval")
	}
	step(10*time.Second + time.Minute)
	if got := r.venue.callCount("AccountSettings"); got != settings+1 {
		t.Errorf("%d settings reads a minute on, want %d", got, settings+1)
	}
	step(10*time.Second + time.Hour)
	if got := r.venue.callCount("MarketRules"); got != rules+1 {
		t.Errorf("%d rule reads an hour on, want %d", got, rules+1)
	}
	if got := r.venue.callCount("Positions"); got != positions {
		t.Errorf("the positions were read %d more times by the loop after the first success", got-positions)
	}
}

// A venue schedule that differs from the calendar is an alarm when the
// difference first appears and when its content changes, not at every hourly
// check; the calendar keeps its own times throughout.
func TestScheduleCheckAlarmsOnChangeAndNeverFollows(t *testing.T) {
	r := newRig(t, testConfig(), ny(2026, 11, 24, 10, 0))
	logs := captureLogs(t)
	ctx := context.Background()
	day := func(date string, closeHour int) ScheduleDay {
		d := parseDay(date)
		return ScheduleDay{Date: date, RegularOpen: time.Date(d.Year(), d.Month(), d.Day(), 9, 30, 0, 0, NewYork), RegularClose: time.Date(d.Year(), d.Month(), d.Day(), closeHour, 0, 0, 0, NewYork)}
	}
	check := func() []string {
		t.Helper()
		if err := r.m.checkSchedule(ctx, r.clock.Now()); err != nil {
			t.Fatal(err)
		}
		return logs.take()
	}
	agreeing := []ScheduleDay{day("2026-11-24", 16), day("2026-11-25", 16), day("2026-11-27", 13)}
	r.index.schedule = agreeing
	if got := check(); len(got) != 0 || len(r.events()) != 0 {
		t.Fatalf("an agreeing schedule: logs %q, events %v", got, r.events())
	}

	// The venue lists a full day on the 27th: one alarm, however often the
	// check runs.
	r.index.schedule = []ScheduleDay{day("2026-11-24", 16), day("2026-11-25", 16), day("2026-11-27", 16)}
	if got := check(); len(got) != 1 || got[0] != "ERROR cex: calendar differs from the venue schedule" {
		t.Fatalf("the first check with a difference logged %q", got)
	}
	for hour := 0; hour < 24; hour++ {
		if got := check(); len(got) != 0 {
			t.Fatalf("check %d with the same difference logged %q", hour+2, got)
		}
	}
	rows := r.sink.Rows("event")
	if len(rows) != 1 {
		t.Fatalf("%d events after 25 checks with one difference, want 1", len(rows))
	}
	ev := rows[0].(store.EventRow)
	if ev.Kind != "calendar_differs" || ev.Level != "error" || ev.Module != "cex" || !strings.Contains(string(ev.Detail), "2026-11-27: close 11-28 02:00 here, 11-28 05:00 on the venue") {
		t.Errorf("event = %+v detail %s", ev, ev.Detail)
	}
	if end := r.m.Session(ny(2026, 11, 27, 10, 0)).End; !end.Equal(ny(2026, 11, 27, 13, 0)) {
		t.Errorf("the calendar followed the venue: the 27th now closes at %s", showTime(end))
	}

	// The difference changes (the venue also drops the 25th): a new alarm.
	r.index.schedule = []ScheduleDay{day("2026-11-24", 16), day("2026-11-27", 16)}
	if got := check(); len(got) != 1 || !strings.HasPrefix(got[0], "ERROR") || len(r.events()) != 2 {
		t.Fatalf("a changed difference: logs %q, %d events; want a second alarm", got, len(r.events()))
	}
	// It goes away: one Info line, no event. It comes back: an alarm again.
	r.index.schedule = agreeing
	if got := check(); len(got) != 1 || got[0] != "INFO cex: calendar agrees with the venue schedule again" || len(r.events()) != 2 {
		t.Fatalf("the difference gone: logs %q, %d events", got, len(r.events()))
	}
	if got := check(); len(got) != 0 {
		t.Fatalf("a second agreeing check logged %q", got)
	}
	r.index.schedule = []ScheduleDay{day("2026-11-24", 16), day("2026-11-27", 16)}
	if check(); len(r.events()) != 3 {
		t.Fatalf("%d events after the difference returned, want 3", len(r.events()))
	}

	// An empty schedule is a failed read, not agreement.
	r.index.schedule = nil
	if err := r.m.checkSchedule(ctx, r.clock.Now()); err == nil {
		t.Error("an empty schedule counted as read")
	}
}
