package inventory

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/strategy"
)

// The tests of SPEC 12.2, 12.4 and 12.9 on the ledger: CloseNet, the quota
// without the unpaired thin leg, what the top-up quota reads, and the end
// states of a thin-leg-first batch.

// takerRate is the account's taker fee, 0.65 bps, as CloseNet takes it.
var takerRate = dec("0.000065")

// fee is value x 0.65 bps: the fee the tests' fills carry, so that a real
// fill and CloseNet's estimate of it agree.
func fee(value string) string { return dec(value).Mul(takerRate).String() }

// exampleAnchors are the anchors of SPEC 12.5: A_L 32.0287, A_S 62.1464 USD.
var exampleAnchors = cex.Anchors{SessionID: "2026-11-23", Kind: cex.AnchorBell, Bull: dec("32.0287"), Bear: dec("62.1464")}

// openExampleCycle opens a cycle on the anchors of SPEC 12.5.
func (r *testRig) openExampleCycle(t *testing.T, dir strategy.Direction) *Cycle {
	t.Helper()
	c, err := r.m.OpenCycle("2026-11-23", dir, exampleAnchors, r.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// closeNet calls CloseNet and fails the test on an error.
func closeNet(t *testing.T, m *InventoryManager, purpose cex.OrderPurpose, lot LotID, bull, bear CloseLeg) decimal.Decimal {
	t.Helper()
	net, err := m.CloseNet(purpose, lot, [2]CloseLeg{cex.Bull: bull, cex.Bear: bear}, takerRate)
	if err != nil {
		t.Fatal(err)
	}
	return net
}

func leg(qty, price string) CloseLeg { return CloseLeg{Qty: dec(qty), Price: dec(price)} }

// ledgerRows counts the lot and cycle rows the sink received.
func ledgerRows(r *testRig) int { return len(r.sink.Rows("lot")) + len(r.sink.Rows("cycle")) }

// roundNet is what the round has realised: the cycle's net, funding included.
func roundNet(r *testRig) decimal.Decimal { return r.m.Cycle().Net }

// realisedBeforeFunding is the cycle's gross minus its fees.
func realisedBeforeFunding(r *testRig) decimal.Decimal {
	c := r.m.Cycle()
	return c.Gross.Sub(c.Fees)
}

// SPEC 12.7 example 7: a long position SOXL 15.60 @ 33.26, SOXS 8.04 @
// 59.869, R = 0. Closing all of it at SOXL bid 2 33.24 and SOXS bid 5 59.96
// is the exit1 profit test: +0.2896. CloseNet(exit) gives the round's net,
// and booking exactly those fills gives the same number, digit for digit.
// CloseNet changes nothing.
func TestCloseNetExitIsTheRoundNetOfTheBooking(t *testing.T) {
	r := newRig(t, "1000", "10")
	c := r.openExampleCycle(t, strategy.Long)
	lot := r.openLot(t)
	orders := []cex.OrderRecord{
		fill(ord(c.ID, 1, cex.Bull, cex.PurposeEntry, cex.SideBuy, lot.ID, "33.26", "15.60"), "15.60", "518.856", fee("518.856")),
		fill(ord(c.ID, 2, cex.Bear, cex.PurposeEntry, cex.SideBuy, lot.ID, "59.869", "8.04"), "8.04", "481.34676", fee("481.34676")),
	}
	r.m.Sync(orders)
	r.m.SettleLot(lot.ID, true, r.clock.Now())

	before, rows := r.m.Cycle(), ledgerRows(r)
	_, usedBefore, _ := r.m.Quota()
	net := closeNet(t, r.m, cex.PurposeExit, "", leg("15.60", "33.24"), leg("8.04", "59.96"))
	if got := net.Round(4); !got.Equal(dec("0.2896")) {
		t.Errorf("exit1 profit test = %s (%s), want +0.2896 (SPEC 12.7 example 7)", got, net)
	}
	if after := r.m.Cycle(); !reflect.DeepEqual(before, after) {
		t.Errorf("CloseNet changed the cycle:\nbefore %+v\nafter  %+v", before, after)
	}
	if _, used, _ := r.m.Quota(); !used.Equal(usedBefore) || ledgerRows(r) != rows {
		t.Errorf("CloseNet changed the quota (%s -> %s) or wrote rows (%d -> %d)", usedBefore, used, rows, ledgerRows(r))
	}

	orders = append(orders,
		fill(ord(c.ID, 3, cex.Bull, cex.PurposeExit, cex.SideSell, "", "33.24", "15.60"), "15.60", "518.544", fee("518.544")),
		fill(ord(c.ID, 4, cex.Bear, cex.PurposeExit, cex.SideSell, "", "59.96", "8.04"), "8.04", "482.0784", fee("482.0784")))
	r.m.Sync(orders)
	wantDec(t, "the round's net after the booked exit", roundNet(r), net.String())
}

// FIFO across two lots, a close of part of the position: CloseNet(exit)
// allocates as the ledger does, an order split over two lots by quantity at
// its average price, and returns the round's net including what earlier
// exits realised.
func TestCloseNetExitFollowsFirstInFirstOut(t *testing.T) {
	r := newRig(t, "1000", "10")
	c := r.openExampleCycle(t, strategy.Long)
	lot1 := r.openLot(t)
	orders := []cex.OrderRecord{
		fill(ord(c.ID, 1, cex.Bull, cex.PurposeEntry, cex.SideBuy, lot1.ID, "33.25", "7.77"), "7.77", "258.3525", fee("258.3525")),
		fill(ord(c.ID, 2, cex.Bear, cex.PurposeEntry, cex.SideBuy, lot1.ID, "59.86", "4.01"), "4.01", "240.0386", fee("240.0386")),
	}
	r.m.Sync(orders)
	r.m.SettleLot(lot1.ID, true, r.clock.Now())
	r.clock.Add(time.Minute)
	lot2 := r.openLot(t)
	orders = append(orders,
		fill(ord(c.ID, 3, cex.Bull, cex.PurposeEntry, cex.SideBuy, lot2.ID, "33.27", "7.83"), "7.83", "260.5041", fee("260.5041")),
		fill(ord(c.ID, 4, cex.Bear, cex.PurposeEntry, cex.SideBuy, lot2.ID, "59.878", "4.03"), "4.03", "241.30834", fee("241.30834")))
	r.m.Sync(orders)
	r.m.SettleLot(lot2.ID, true, r.clock.Now())

	// A first exit batch realises something; the second batch closes all of
	// lot 1's rest and part of lot 2.
	orders = append(orders,
		fill(ord(c.ID, 5, cex.Bear, cex.PurposeExit, cex.SideSell, "", "59.95", "2.00"), "2.00", "119.9", fee("119.9")),
		fill(ord(c.ID, 6, cex.Bull, cex.PurposeExit, cex.SideSell, "", "33.24", "3.88"), "3.88", "128.9712", fee("128.9712")))
	r.m.Sync(orders)
	net := closeNet(t, r.m, cex.PurposeExit, "", leg("7.80", "33.23"), leg("4.02", "59.97"))

	orders = append(orders,
		fill(ord(c.ID, 7, cex.Bear, cex.PurposeExit, cex.SideSell, "", "59.97", "4.02"), "4.02", "241.0794", fee("241.0794")),
		fill(ord(c.ID, 8, cex.Bull, cex.PurposeExit, cex.SideSell, "", "33.23", "7.80"), "7.80", "259.194", fee("259.194")))
	r.m.Sync(orders)
	wantDec(t, "the round's net after the booked exit", roundNet(r), net.String())
	if got := r.lot(t, 2).Legs[cex.Bull]; !got.ExitQty.Equal(dec("3.91")) {
		t.Errorf("lot 2 bull closed %s, want 3.91: the exit crossed into the second lot", got.ExitQty)
	}
}

// SPEC 12.7 example 8: SOXS 8.04 @ 60.02 and SOXL 6.00 @ 33.22 of the exit
// are booked; the rest, 9.60, is valued on top of them, the booked fills
// counted once: +0.268 at 33.20, +0.076 at 33.18. Booking the rest at 33.20
// gives the same number.
func TestCloseNetAfterAPartialExitCountsBookedFillsOnce(t *testing.T) {
	r := newRig(t, "1000", "10")
	c := r.openExampleCycle(t, strategy.Long)
	lot := r.openLot(t)
	orders := []cex.OrderRecord{
		fill(ord(c.ID, 1, cex.Bull, cex.PurposeEntry, cex.SideBuy, lot.ID, "33.26", "15.60"), "15.60", "518.856", fee("518.856")),
		fill(ord(c.ID, 2, cex.Bear, cex.PurposeEntry, cex.SideBuy, lot.ID, "59.869", "8.04"), "8.04", "481.34676", fee("481.34676")),
	}
	r.m.Sync(orders)
	r.m.SettleLot(lot.ID, true, r.clock.Now())
	orders = append(orders,
		fill(ord(c.ID, 3, cex.Bear, cex.PurposeExit, cex.SideSell, "", "60.02", "8.04"), "8.04", "482.5608", fee("482.5608")),
		fill(ord(c.ID, 4, cex.Bull, cex.PurposeExit, cex.SideSell, "", "33.22", "15.60"), "6.00", "199.32", fee("199.32")))
	r.m.Sync(orders)

	at20 := closeNet(t, r.m, cex.PurposeExit, "", leg("9.60", "33.20"), CloseLeg{})
	at18 := closeNet(t, r.m, cex.PurposeExit, "", leg("9.60", "33.18"), CloseLeg{})
	if !at20.Round(3).Equal(dec("0.268")) || !at18.Round(3).Equal(dec("0.076")) {
		t.Errorf("rest at 33.20 / 33.18 = %s / %s, want +0.268 / +0.076 (SPEC 12.7 example 8)", at20, at18)
	}
	orders = append(orders, fill(ord(c.ID, 5, cex.Bull, cex.PurposeExit, cex.SideSell, "", "33.20", "9.60"), "9.60", "318.72", fee("318.72")))
	r.m.Sync(orders)
	wantDec(t, "the round's net after booking the rest", roundNet(r), at20.String())
}

// exampleEleven builds SPEC 12.5 example 11 on the rig's anchors 40 / 60: an
// old lot long SOXS 5 @ 59.00, paired with SOXL 7.50 @ 39.90, and a new lot
// long SOXS 5 @ 60.00 that is not paired yet (still building).
func exampleEleven(t *testing.T) (r *testRig, c *Cycle, old, recent *Lot, orders []cex.OrderRecord) {
	t.Helper()
	r = newRig(t, "1000", "10")
	c = r.openCycle(t, strategy.Long)
	old = r.openLot(t)
	orders = []cex.OrderRecord{
		fill(ord(c.ID, 1, cex.Bear, cex.PurposeEntry, cex.SideBuy, old.ID, "59.00", "5"), "5", "295", fee("295")),
		fill(ord(c.ID, 2, cex.Bull, cex.PurposeEntry, cex.SideBuy, old.ID, "39.90", "7.50"), "7.50", "299.25", fee("299.25")),
	}
	r.m.Sync(orders)
	r.m.SettleLot(old.ID, true, r.clock.Now())
	r.clock.Add(time.Minute)
	recent = r.openLot(t)
	orders = append(orders, fill(ord(c.ID, 3, cex.Bear, cex.PurposeEntry, cex.SideBuy, recent.ID, "60.00", "5"), "5", "300", fee("300")))
	r.m.Sync(orders)
	return r, c, old, recent, orders
}

// SPEC 12.5 example 11: with SOXS bid 59.80, first in, first out would
// close the old lot at +0.80 a share and show a profit; a trim of the new
// lot closes the new lot, -0.20 a share: 5 shares lose 1.00 U plus two fees,
// -1.038935 U. CloseNet(trim, new lot) says so, and booking the trim gives
// the same increment of the round and of the lot, digit for digit.
func TestCloseNetTrimValuesTheNamedLot(t *testing.T) {
	r, c, _, recent, orders := exampleEleven(t)

	fifo := closeNet(t, r.m, cex.PurposeExit, "", CloseLeg{}, leg("5", "59.80"))
	wantDec(t, "first in, first out (the old lot)", fifo, "3.96139")
	trim := closeNet(t, r.m, cex.PurposeTrim, recent.ID, CloseLeg{}, leg("5", "59.80"))
	wantDec(t, "trim of the new lot", trim, "-1.038935")
	if !trim.IsNegative() {
		t.Error("the old lot's profit must not let the new lot's profit close through")
	}

	before, lotBefore := realisedBeforeFunding(r), r.lot(t, 2).Net
	orders = append(orders, fill(ord(c.ID, 4, cex.Bear, cex.PurposeTrim, cex.SideSell, recent.ID, "59.80", "5"), "5", "299", fee("299")))
	r.m.Sync(orders)
	wantDec(t, "the round's increment after the booked trim", realisedBeforeFunding(r).Sub(before), trim.String())
	wantDec(t, "the new lot's increment after the booked trim", r.lot(t, 2).Net.Sub(lotBefore), trim.String())
}

// The same book after an exit of 5: the old lot closes, digit for digit
// what CloseNet(exit) said.
func TestCloseNetExitOfExampleElevenIsTheBooking(t *testing.T) {
	r, c, _, _, orders := exampleEleven(t)
	fifo := closeNet(t, r.m, cex.PurposeExit, "", CloseLeg{}, leg("5", "59.80"))
	orders = append(orders, fill(ord(c.ID, 4, cex.Bear, cex.PurposeExit, cex.SideSell, "", "59.80", "5"), "5", "299", fee("299")))
	r.m.Sync(orders)
	wantDec(t, "the round's net after the booked exit", roundNet(r), fifo.String())
}

// A trim larger than what its lot holds closes the rest first in, first out,
// as the ledger books it (PRD 7.4): 6 of the new lot's 5 take 1 from the old
// lot. CloseNet returns the increment of the whole order.
func TestCloseNetTrimBeyondItsLot(t *testing.T) {
	r, c, _, recent, orders := exampleEleven(t)
	trim := closeNet(t, r.m, cex.PurposeTrim, recent.ID, CloseLeg{}, leg("6", "59.80"))
	before := realisedBeforeFunding(r)
	orders = append(orders, fill(ord(c.ID, 4, cex.Bear, cex.PurposeTrim, cex.SideSell, recent.ID, "59.80", "6"), "6", "358.8", fee("358.8")))
	r.m.Sync(orders)
	wantDec(t, "the round's increment after the booked trim", realisedBeforeFunding(r).Sub(before), trim.String())
	if got := r.lot(t, 1).Legs[cex.Bear].ExitQty; !got.Equal(dec("1")) {
		t.Errorf("old lot bear closed %s, want 1", got)
	}
}

// CloseNet refuses what it cannot value: no cycle, an opening purpose, a
// trim without its lot, and a close of more than the cycle holds (the ledger
// would not book the excess).
func TestCloseNetRefuses(t *testing.T) {
	r := newRig(t, "1000", "10")
	if _, err := r.m.CloseNet(cex.PurposeExit, "", [2]CloseLeg{cex.Bull: leg("1", "40")}, takerRate); !errors.Is(err, errNoCycle) {
		t.Errorf("without a cycle: %v, want errNoCycle", err)
	}
	r, _, _, recent, _ := exampleEleven(t)
	cases := []struct {
		name    string
		purpose cex.OrderPurpose
		lot     LotID
		close   [2]CloseLeg
	}{
		{"an entry is not a close", cex.PurposeEntry, recent.ID, [2]CloseLeg{cex.Bear: leg("1", "59.80")}},
		{"a completion is not a close", cex.PurposeComplete, recent.ID, [2]CloseLeg{cex.Bear: leg("1", "59.80")}},
		{"a trim names its lot", cex.PurposeTrim, "", [2]CloseLeg{cex.Bear: leg("1", "59.80")}},
		{"a trim of a lot not in the cycle", cex.PurposeTrim, "c1-9", [2]CloseLeg{cex.Bear: leg("1", "59.80")}},
		{"more than the cycle holds", cex.PurposeExit, "", [2]CloseLeg{cex.Bear: leg("10.01", "59.80")}},
	}
	for _, tc := range cases {
		if _, err := r.m.CloseNet(tc.purpose, tc.lot, tc.close, takerRate); err == nil {
			t.Errorf("%s: no error", tc.name)
		}
	}
}

// --- the quota ---

// thinRig is a rig whose manager pairs with the thin leg first, SOXS.
func thinRig(t *testing.T, capital string) *testRig {
	t.Helper()
	r := newRig(t, capital, "10")
	WithThinLeg(cex.Bear)(r.m)
	return r
}

// SPEC 12.4 "额度": with the thin leg first, the used quota counts what is
// paired and the reservations of pending orders, not the unpaired thin leg
// of a batch in pairing. HeldNotional, the replay's peak, still holds it.
// A pending order of the other leg reserves its own notional at its quota
// price, and the thin quantity it would pair counts from then on.
func TestQuotaLeavesOutTheUnpairedThinLeg(t *testing.T) {
	r := thinRig(t, "1000")
	c := r.openCycle(t, strategy.Long) // anchors 40 / 60: 1.5 bull per bear
	lot := r.openLot(t)
	orders := []cex.OrderRecord{fill(ord(c.ID, 1, cex.Bear, cex.PurposeEntry, cex.SideBuy, lot.ID, "60", "5"), "5", "300", "0")}
	r.m.Sync(orders)
	if _, used, _ := r.m.Quota(); !used.IsZero() {
		t.Errorf("used with only the thin leg filled = %s, want 0", used)
	}
	wantDec(t, "held", r.m.HeldNotional(), "300")
	if u := r.m.Unpaired(lot.ID); !u.Qty.Equal(dec("5")) || !u.Cost.Equal(dec("300")) {
		t.Errorf("unpaired = %s / %s, want 5 / 300", u.Qty, u.Cost)
	}

	// A pending bull order of 3 at 40.50 pairs 2 of the 5 once it fills.
	orders = append(orders, ord(c.ID, 2, cex.Bull, cex.PurposeEntry, cex.SideBuy, lot.ID, "40.50", "3"))
	r.m.Sync(orders)
	if _, used, _ := r.m.Quota(); !used.Equal(dec("241.5")) {
		t.Errorf("used with the pair order pending = %s, want 241.5: 3 x 40.50 reserved and 2 x 60 of the thin leg", used)
	}
	orders[1] = fill(orders[1], "3", "121.5", "0")
	r.m.Sync(orders)
	if _, used, _ := r.m.Quota(); !used.Equal(dec("241.5")) {
		t.Errorf("used with 3 paired = %s, want 241.5", used)
	}
	wantDec(t, "held", r.m.HeldNotional(), "421.5")
	if u := r.m.Unpaired(lot.ID); !u.Qty.Equal(dec("3")) || !u.Cost.Equal(dec("180")) {
		t.Errorf("unpaired = %s / %s, want 3 / 180", u.Qty, u.Cost)
	}

	// The batch ends: nothing of it is left out any more.
	r.m.SettlePairedLot(lot.ID, LotEndBuiltPartial, r.clock.Now())
	if _, used, _ := r.m.Quota(); !used.Equal(dec("421.5")) {
		t.Errorf("used after the batch ended = %s, want 421.5", used)
	}
	if u := r.m.Unpaired(lot.ID); !u.Qty.IsZero() || !u.Cost.IsZero() {
		t.Errorf("unpaired after the batch ended = %s / %s, want zero", u.Qty, u.Cost)
	}
}

// With both legs together (thin_leg "") nothing changes: a building lot's
// one filled leg counts in full, as before section 12.
func TestQuotaWithBothLegsTogetherCountsEveryFill(t *testing.T) {
	r := newRig(t, "1000", "10")
	c := r.openCycle(t, strategy.Long)
	lot := r.openLot(t)
	r.m.Sync([]cex.OrderRecord{fill(ord(c.ID, 1, cex.Bear, cex.PurposeEntry, cex.SideBuy, lot.ID, "60", "5"), "5", "300", "0")})
	if _, used, _ := r.m.Quota(); !used.Equal(dec("300")) {
		t.Errorf("used = %s, want 300", used)
	}
	if u := r.m.Unpaired(lot.ID); !u.Qty.IsZero() {
		t.Errorf("unpaired without a thin leg = %s, want zero", u.Qty)
	}
}

// SPEC 12.4 step 6 and example 6, on the anchors of 12.5: the thin leg
// filled 5.00 @ 59.874 and nothing is paired. What the top-up quota reads
// from the ledger is the quota left without that thin leg, and the thin
// leg's actual cost, 299.37, not 5.00 at the new quote 59.870. With the
// capital set so that 300 U remain once the existing part is paired at
// SOXL 33.27, a top-up of at most 300 / 124.425 = 2.41 SOXS fits.
func TestTopUpQuotaReadsTheActualCost(t *testing.T) {
	r := thinRig(t, "92.2089") // L x M = 922.089
	c := r.openExampleCycle(t, strategy.Long)
	lot := r.openLot(t)
	r.m.Sync([]cex.OrderRecord{fill(ord(c.ID, 1, cex.Bear, cex.PurposeEntry, cex.SideBuy, lot.ID, "59.880", "5"), "5", "299.37", "0")})

	_, _, left := r.m.Quota()
	wantDec(t, "quota left", left, "922.089")
	u := r.m.Unpaired(lot.ID)
	wantDec(t, "unpaired qty", u.Qty, "5")
	wantDec(t, "unpaired cost", u.Cost, "299.37")

	// The formula of 12.4 step 6, as strategy applies it (step 10.4).
	thickPerThin := exampleAnchors.Bear.Div(exampleAnchors.Bull)
	pS, pL := dec("59.870"), dec("33.27")
	existing := u.Cost.Add(u.Qty.Mul(thickPerThin).RoundFloor(2).Mul(pL))
	wantDec(t, "the existing part paired", existing, "622.089")
	qMax := left.Sub(existing).Div(pS.Add(thickPerThin.Mul(pL))).RoundFloor(2)
	wantDec(t, "top-up at most", qMax, "2.41")
}

// --- end states ---

// SPEC 12.4: every thin-leg-first batch ends in one of five states. built
// and built_partial are positions; entry_failed, unwound_profit and
// unwound_stop are void, their state the void reason the lot row carries.
func TestSettlePairedLotEndStates(t *testing.T) {
	cases := []struct {
		end        LotEnd
		status     LotStatus
		voidReason string
	}{
		{LotEndBuilt, LotOpen, ""},
		{LotEndBuiltPartial, LotOpen, ""},
		{LotEndEntryFailed, LotVoid, "entry_failed"},
		{LotEndUnwoundProfit, LotVoid, "unwound_profit"},
		{LotEndUnwoundStop, LotVoid, "unwound_stop"},
	}
	for _, tc := range cases {
		t.Run(string(tc.end), func(t *testing.T) {
			r := thinRig(t, "1000")
			c := r.openCycle(t, strategy.Long)
			lot := r.openLot(t)
			if tc.status == LotOpen { // a built batch holds both legs; one holding nothing would close at once
				r.m.Sync([]cex.OrderRecord{
					fill(ord(c.ID, 1, cex.Bear, cex.PurposeEntry, cex.SideBuy, lot.ID, "60", "5"), "5", "300", "0"),
					fill(ord(c.ID, 2, cex.Bull, cex.PurposeEntry, cex.SideBuy, lot.ID, "40", "7.50"), "7.50", "300", "0"),
				})
			}
			r.m.SettlePairedLot(lot.ID, tc.end, r.clock.Now())
			got := r.lot(t, 1)
			if got.Status != tc.status || got.VoidReason != tc.voidReason || got.End != tc.end {
				t.Errorf("lot = %s %q %s, want %s %q %s", got.Status, got.VoidReason, got.End, tc.status, tc.voidReason, tc.end)
			}
			if row := lastLotRow(t, r.sink, lot.ID); row.Status != string(tc.status) || row.VoidReason != tc.voidReason || row.EndState != string(tc.end) {
				t.Errorf("lot row = %s %q %q, want %s %q %s", row.Status, row.VoidReason, row.EndState, tc.status, tc.voidReason, tc.end)
			}
			// A restart reads the end state back from the lot's row (10.7).
			if back := r.restart(t, "1000", "10").lot(t, 1); back.End != tc.end || back.Status != tc.status {
				t.Errorf("after a restart the lot is %s %s, want %s %s", back.Status, back.End, tc.status, tc.end)
			}
			if tc.status == LotVoid && got.ClosedAt.IsZero() {
				t.Error("a void lot has its closing time")
			}
		})
	}
}
