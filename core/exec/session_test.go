package exec

import (
	"math"
	"testing"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/inventory"
	"etf-pair-arbitrage/core/store"
	"etf-pair-arbitrage/core/strategy"
)

// lastLot returns the newest lot row written for the lot.
func (r *rig) lastLot(id string) store.LotRow {
	r.t.Helper()
	var out *store.LotRow
	for _, row := range r.sink.Rows("lot") {
		if l := row.(store.LotRow); l.LotID == id {
			out = &l
		}
	}
	if out == nil {
		r.t.Fatalf("no row of lot %s", id)
	}
	return *out
}

// lastCycle returns the newest cycle row.
func (r *rig) lastCycle() store.CycleRow {
	r.t.Helper()
	row := r.sink.Last("cycle")
	if row == nil {
		r.t.Fatal("no cycle row")
	}
	return row.(store.CycleRow)
}

// eqDec fails when got is not want, exactly.
func eqDec(t *testing.T, what string, got decimal.Decimal, want string) {
	t.Helper()
	if !got.Equal(dec(want)) {
		t.Errorf("%s = %s, want %s", what, got, want)
	}
}

// near fails when got is not want within 1e-9.
func near(t *testing.T, what string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s = %.12f, want %.12f", what, got, want)
	}
}

// quotaLeft fails when the quota left is not want.
func (r *rig) quotaLeft(want string) {
	r.t.Helper()
	total, used, left := r.inv.Quota()
	if !left.Equal(dec(want)) {
		r.t.Errorf("quota left = %s (total %s, used %s), want %s", left, total, used, want)
	}
}

// One whole Session on the real cex, paper venue, inventory and strategy:
// the warm-up, an entry, a re-entry refused for its spacing and then made,
// a line exit that closes part of the cycle, the regular open, and the
// forced exit at the close - 1 minute. The Session is the one of the close
// of Tue 2026-11-24 (New York): it starts 11-25 05:00, the regular open is
// 11-25 22:30, the close 11-26 05:00 (UTC+8). F = 1, the bell anchors are
// 40 / 60, M = 2,000 USDT, L = 10, so the quota is 20,000 while R >= 0. The
// fee is 0.65 bps of each fill's value (0.000065).
func TestScriptedSession(t *testing.T) {
	r := newRig(t, utc8Time(11, 25, 5, 1, 0), rigOptions{})

	// Warm-up: 05:01 is book A (spread -0.6667), 05:02 book B (+7.3333).
	// Until both minutes are done the reason is the warm-up; at 05:03:00
	// the SMA of two minutes is mu = 3.3333 and sigma = 4.
	r.idleAt(utc8Time(11, 25, 5, 1, 30), strategy.ReasonDepthBull)
	r.endMinute()
	if r.m.idleReason != strategy.ReasonWarmup {
		t.Fatalf("idle reason during the warm-up %q, want warmup", r.m.idleReason)
	}
	r.endMinute()
	r.stepAt(utc8Time(11, 25, 5, 3, 0))
	near(t, "mu", r.m.stats.Mu(r.m.anchors), 10000*(40.40/40+59.42/60-2))
	near(t, "sigma", r.m.stats.Sigma(r.m.anchors), 4)

	// 05:03:05, PRD appendix A example 1: a short at x = +10.00 >= 8; the
	// round-trip fee 2 x 0.65 x (40.44/40 + 59.420/60) = 2.6017 spread units
	// is covered by (2 + 0.5) x 4 = 10. Each leg offers half of its levels
	// up to the pricing level: 400 SOXL (16,000 USD anchored), 70.2 SOXS
	// (4,212 USD), so K = 4,212: sell 105.30 / 70.20. The quota at the
	// decision's best bids, 20,000 / (40.45/40 + 59.437/60) = 9,990.7, does
	// not cap it. Notional 105.30 x 40.44 + 70.20 x 59.42 = 8,429.616; fee
	// estimate 2 x 0.000065 x 8,429.616 = 1.09585008.
	r.at(utc8Time(11, 25, 5, 3, 5))
	r.books(entryBook())
	r.trade(utc8Time(11, 25, 5, 3, 5))
	entry := r.decisions()[0]
	if entry.Kind != "entry" || entry.Direction != "short" {
		t.Fatalf("first decision %s %s, want a short entry", entry.Kind, entry.Direction)
	}
	lot1 := entry.LotID
	cycleID := entry.CycleID
	if detail := decodeDetail(t, entry.Detail); detail["qty_bull"] != "105.3" || detail["qty_bear"] != "70.2" || detail["price_bull"] != "40.44" ||
		detail["price_bear"] != "59.42" || detail["fee_usdt"] != "1.09585008" || detail["k_usd"] != "4212" {
		t.Errorf("entry detail %v", detail)
	} else {
		near(t, "entry x", detail["x_d"].(float64), 10)
		near(t, "entry fee_d", detail["fee_d"].(float64), 1.3*(40.44/40+59.42/60))
	}
	// The paper venue fills at the bids inside the limits: SOXL 105.30 @
	// 40.45 = 4,259.385 (fee 0.276860025); SOXS 0.4 @ 59.437 + 60 @ 59.431
	// + 9.8 @ 59.428 = 4,172.0292 (fee 0.271181898). The quota used is the
	// entry value: 20,000 - 8,431.4142 = 11,568.5858.
	r.stepAt(utc8Time(11, 25, 5, 3, 6))
	if r.m.idleReason != strategy.ReasonAwaitDepth { // V6: the next decision waits for depth newer than these results
		t.Errorf("idle reason after the entry's results %q, want await_depth", r.m.idleReason)
	}
	l1 := r.lastLot(lot1)
	if l1.Status != string(inventory.LotOpen) {
		t.Errorf("lot 1 status %s, want open (built)", l1.Status)
	}
	eqDec(t, "lot 1 entry value bull", l1.EntryValueBull, "4259.385")
	eqDec(t, "lot 1 entry value bear", l1.EntryValueBear, "4172.0292")
	eqDec(t, "lot 1 entry fee bear", l1.EntryFeeBear, "0.271181898")
	r.quotaLeft("11568.5858")

	// 05:03:10: SOXS's pricing level moves to 59.423, x = 13.8333 - 3.3333
	// = +10.5: on the line, but within 60 s of the last entry (e = 10.00) a
	// same-side entry needs e + 1 = 11: spacing. One at_line_blocked row
	// for the two rounds it lasts.
	r.at(utc8Time(11, 25, 5, 3, 10))
	spacing := entryBook()
	spacing.bearBids = side{lv("59.44", "0.4"), lv("59.434", "60"), lv("59.431", "30"), lv("59.428", "30"), lv("59.423", "20")}
	r.books(spacing)
	r.idleAt(utc8Time(11, 25, 5, 3, 10), strategy.ReasonSpacing)
	r.idleAt(utc8Time(11, 25, 5, 3, 11), strategy.ReasonSpacing)

	// 05:03:20, the re-entry: the bids SOXL 40.51 x 20, 40.50 x 100 and SOXS
	// 59.47 / 59.465 x 10, 59.46 / 59.455 / 59.45 x 20 give x =
	// D(40.50, 59.45) - 3.3333 = 33.3333 - 3.3333 = 30 >= 11. Offered 60 /
	// 40 shares, 2,400 USD both: sell 60.00 / 40.00, sized with the cycle's
	// pairing anchors. Fills: SOXL 20 @ 40.51 + 40 @ 40.50 = 2,430.2 (fee
	// 0.157963); SOXS 10 @ 59.47 + 10 @ 59.465 + 20 @ 59.46 = 2,378.55 (fee
	// 0.15460575). Quota left 11,568.5858 - 4,808.75 = 6,759.8358.
	r.at(utc8Time(11, 25, 5, 3, 20))
	r.books(pairBook{
		bullBids: side{lv("40.51", "20"), lv("40.50", "100")}, bullAsks: side{lv("40.53", "100"), lv("40.54", "100")},
		bearBids: side{lv("59.47", "10"), lv("59.465", "10"), lv("59.46", "20"), lv("59.455", "20"), lv("59.45", "20")},
		bearAsks: side{lv("59.48", "50"), lv("59.485", "50"), lv("59.49", "50"), lv("59.495", "50"), lv("59.50", "50")},
	})
	r.trade(utc8Time(11, 25, 5, 3, 20))
	lot2 := r.decisions()[2].LotID
	l2 := r.lastLot(lot2)
	if l2.Status != string(inventory.LotOpen) || !l2.EntryQtyBull.Equal(dec("60")) || !l2.EntryQtyBear.Equal(dec("40")) {
		t.Errorf("lot 2 %s %s / %s, want open 60 / 40", l2.Status, l2.EntryQtyBull, l2.EntryQtyBear)
	}
	eqDec(t, "lot 2 entry value bull", l2.EntryValueBull, "2430.2")
	eqDec(t, "lot 2 entry value bear", l2.EntryValueBear, "2378.55")
	r.quotaLeft("6759.8358")

	// 05:03:30, PRD appendix A example 2: x = -8.33 <= -2, the exit line.
	// Held 165.30 / 110.20; offered 350 / 100 shares, 14,000 / 6,000 USD
	// anchored: buy back 150.00 / 100.00 at the limits 40.38 / 59.400. They
	// fill at 40.37 (6,055.5, fee 0.3936075) and 59.385 (5,938.5, fee
	// 0.3860025). First in, first out:
	//   lot 1 closes: SOXL exit 105.30 of the order: 4,250.961, fee
	//   0.27631247 (0.276312465 rounded to 8 places); SOXS 70.20: 4,168.827,
	//   fee 0.27097376. Gross (4,259.385 - 4,250.961) + (4,172.0292 -
	//   4,168.827) = 8.424 + 3.2022 = 11.6262; fees 0.276860025 +
	//   0.271181898 + 0.27631247 + 0.27097376 = 1.095328153; net
	//   10.530871847.
	//   lot 2 closes 44.70 / 29.80: exits 1,804.539 / 1,769.673, fees
	//   0.11729503 / 0.11502874; its entry cost of that part 2,430.2 x
	//   44.7/60 = 1,810.499 and 2,378.55 x 29.8/40 = 1,772.01975, the entry
	//   fees 0.11768244 / 0.11518128. Gross 5.96 + 2.34675 = 8.30675; fees
	//   0.46518749; net 7.84156251. 15.30 / 10.20 are left.
	// R = 18.372434357 >= 0: the quota stays 20,000; used is what is left
	// at its entry value, (2,430.2 - 1,810.499) + (2,378.55 - 1,772.01975)
	// = 1,226.23125; left 18,773.76875.
	r.at(utc8Time(11, 25, 5, 3, 30))
	r.books(exitBook())
	r.trade(utc8Time(11, 25, 5, 3, 30))
	exit := r.decisions()[3]
	if exit.Kind != "exit" {
		t.Fatalf("fourth decision %s, want exit", exit.Kind)
	}
	if detail := decodeDetail(t, exit.Detail); detail["qty_bull"] != "150" || detail["qty_bear"] != "100" || detail["stats_stale"] != false {
		t.Errorf("exit detail %v", detail)
	}
	r.stepAt(utc8Time(11, 25, 5, 3, 31))
	if r.m.idleReason != strategy.ReasonAwaitDepth { // V6 after an exit too
		t.Errorf("idle reason after the exit's results %q, want await_depth", r.m.idleReason)
	}
	l1 = r.lastLot(lot1)
	if l1.Status != string(inventory.LotClosed) {
		t.Errorf("lot 1 status %s, want closed", l1.Status)
	}
	eqDec(t, "lot 1 gross", l1.Gross, "11.6262")
	eqDec(t, "lot 1 fees", l1.Fees, "1.095328153")
	eqDec(t, "lot 1 net", l1.Net, "10.530871847")
	l2 = r.lastLot(lot2)
	eqDec(t, "lot 2 gross so far", l2.Gross, "8.30675")
	eqDec(t, "lot 2 fees so far", l2.Fees, "0.46518749")
	eqDec(t, "lot 2 net so far", l2.Net, "7.84156251")
	if _, hold := r.inv.Holdings(); !hold[cex.Bull].Equal(dec("15.3")) || !hold[cex.Bear].Equal(dec("10.2")) {
		t.Errorf("held %v, want 15.30 / 10.20", hold)
	}
	r.quotaLeft("18773.76875")
	r.endMinute() // 05:03 ends on book A: neutral, the cycle is kept

	// 22:30:05, the regular open has passed: the entry book gives the short
	// signal again, but entries stop (regular_hours); the exit line is not
	// reached (x at the asks under the last ready mean is +27.5), and there
	// is no forced exit at the open.
	r.at(utc8Time(11, 25, 22, 30, 5))
	r.books(entryBook())
	r.idleAt(utc8Time(11, 25, 22, 30, 5), strategy.ReasonRegularHours)
	if r.m.forced != "" {
		t.Errorf("forced exit %q at the regular open", r.m.forced)
	}

	// 04:59:00 next day, close - 1 minute: the forced exit begins. The books
	// are stale (no price on either leg): nothing is sent.
	r.idleAt(utc8Time(11, 26, 4, 59, 0), strategy.ReasonNoPrice)
	if r.m.forced != inventory.EndCloseWindow {
		t.Errorf("forced cause %q, want close_window", r.m.forced)
	}

	// 04:59:01, prices again: buy back 15.30 SOXL at the best ask 40.30
	// through 10 bps, 40.3403 rounded up to the tick: 40.35; and 10.20 SOXS
	// at 59.30 x 1.001 = 59.3593, rounded up: 59.360. They fill at 40.30
	// (616.59, fee 0.04007835) and 59.30 (604.86, fee 0.0393159).
	r.at(utc8Time(11, 26, 4, 59, 1))
	r.books(pairBook{
		bullBids: side{lv("40.28", "100"), lv("40.27", "100")}, bullAsks: side{lv("40.30", "100"), lv("40.31", "100")},
		bearBids: side{lv("59.29", "50"), lv("59.289", "50"), lv("59.288", "50"), lv("59.287", "50"), lv("59.286", "50")},
		bearAsks: side{lv("59.30", "50"), lv("59.301", "50"), lv("59.302", "50"), lv("59.303", "50"), lv("59.304", "50")},
	})
	r.trade(utc8Time(11, 26, 4, 59, 1))
	if r.m.idleReason != strategy.ReasonAwaitClose { // the round of the results closes the cycle and sends nothing
		t.Errorf("idle reason of the round that closed the cycle %q, want await_close", r.m.idleReason)
	}
	forced := r.decisions()[4]
	if detail := decodeDetail(t, forced.Detail); forced.Kind != "forced" || detail["price_bull"] != "40.35" || detail["price_bear"] != "59.36" ||
		detail["qty_bull"] != "15.3" || detail["qty_bear"] != "10.2" {
		t.Errorf("forced decision %s %v", forced.Kind, detail)
	}

	// 04:59:02: flat, the venue's positions agree, the cycle closes.
	//   lot 2: exits 60 / 40 for 2,421.129 / 2,374.533; gross (2,430.2 -
	//   2,421.129) + (2,378.55 - 2,374.533) = 9.071 + 4.017 = 13.088; fees
	//   0.157963 + 0.15460575 + 0.11729503 + 0.04007835 + 0.11502874 +
	//   0.0393159 = 0.62428677; net 12.46371323.
	//   cycle: gross 24.7142, fees 1.719614923, net 22.994585077 (no
	//   funding was booked). R is that net and the quota is 20,000 again.
	r.stepAt(utc8Time(11, 26, 4, 59, 2))
	if r.inv.Cycle() != nil {
		t.Fatalf("the cycle is still open; idle reason %q", r.m.idleReason)
	}
	l2 = r.lastLot(lot2)
	if l2.Status != string(inventory.LotClosed) {
		t.Errorf("lot 2 status %s, want closed", l2.Status)
	}
	eqDec(t, "lot 2 gross", l2.Gross, "13.088")
	eqDec(t, "lot 2 fees", l2.Fees, "0.62428677")
	eqDec(t, "lot 2 net", l2.Net, "12.46371323")
	c := r.lastCycle()
	if c.CycleID != cycleID || c.Status != string(inventory.CycleClosed) || c.EndReason != inventory.EndCloseWindow {
		t.Errorf("cycle %s %s %s, want %s closed close_window", c.CycleID, c.Status, c.EndReason, cycleID)
	}
	eqDec(t, "cycle gross", c.Gross, "24.7142")
	eqDec(t, "cycle fees", c.Fees, "1.719614923")
	eqDec(t, "cycle net", c.Net, "22.994585077")
	r.quotaLeft("20000")
	if total, _, _ := r.inv.Quota(); !total.Equal(dec("20000")) {
		t.Errorf("quota total %s, want 20000 (a profit does not raise it)", total)
	}

	// The rows: five decisions in order, eight orders (one pair per batch
	// and no more), the first three minutes with their raw mids, and the
	// samples carry the reasons.
	var kinds []string
	for _, d := range r.decisions() {
		kinds = append(kinds, d.Kind+"/"+d.Reason)
	}
	if d := r.decisions()[1]; d.Direction != "short" {
		t.Errorf("the at_line_blocked row's direction %q, want short", d.Direction)
	}
	if want := []string{"entry/", "at_line_blocked/spacing", "entry/", "exit/", "forced/"}; !equalStrings(kinds, want) {
		t.Errorf("decisions %v, want %v", kinds, want)
	}
	if n := len(r.orders()); n != 8 {
		t.Errorf("%d orders, want 8", n)
	}
	bars := r.sink.Rows("minute_bar")
	if len(bars) < 3 {
		t.Fatalf("%d minute bars", len(bars))
	}
	for i, want := range []float64{40.384, 40.416, 40.384} {
		b := bars[i].(store.MinuteBarRow)
		if !b.Valid || b.Source != "own" || b.SessionID != "2026-11-24" || math.Abs(b.MidBull-want) > 1e-9 || math.Abs(b.MidBear-59.42) > 1e-9 || b.F != 1 {
			t.Errorf("minute bar %d = %+v, want valid mids %v / 59.42 with F 1", i, b, want)
		}
	}
	reasons := map[string]bool{}
	for _, row := range r.sink.Rows("signal_sample") {
		reasons[row.(store.SignalSampleRow).IdleReason] = true
	}
	for _, want := range []string{"depth_bull", "warmup", "spacing", "regular_hours", "no_price", "await_depth"} {
		if !reasons[want] {
			t.Errorf("no signal_sample with idle_reason %s; got %v", want, reasons)
		}
	}
	if kinds := r.eventKinds(); len(kinds) != 0 {
		t.Errorf("events %v, want none", kinds)
	}
}

// equalStrings compares two lists.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
