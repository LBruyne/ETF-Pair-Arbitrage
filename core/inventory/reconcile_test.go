package inventory

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/store"
	"etf-pair-arbitrage/core/strategy"
)

// reconcile runs one Reconcile and fails unless it returns want (nil for
// agreement) and Mismatch is as given.
func reconcile(t *testing.T, r *testRig, when string, want error, mismatch bool) {
	t.Helper()
	err := r.m.Reconcile(t.Context())
	if (want == nil && err != nil) || (want != nil && !errors.Is(err, want)) {
		t.Errorf("%s: Reconcile = %v, want %v", when, err, want)
	}
	if got := r.m.Mismatch(); got != mismatch {
		t.Errorf("%s: Mismatch = %t, want %t", when, got, mismatch)
	}
}

// Reconciliation (PRD 13.4, 13.7): equal positions are fine; a difference an
// order without a final state explains is fine; a real difference is a
// mismatch only when a second read, a second later, still differs;
// positions that cannot be read are neither flat nor a mismatch.
func TestReconcile(t *testing.T) {
	r := newRig(t, "1000", "10")
	reconcile(t, r, "flat on both sides", nil, false)

	// A long cycle: the bull entry filled 10, the bear entry of 5 has no
	// final state.
	c := r.openCycle(t, strategy.Long)
	lot := r.openLot(t)
	bull := fill(ord(c.ID, 1, cex.Bull, cex.PurposeEntry, cex.SideBuy, lot.ID, "10", "10"), "10", "100", "0.01")
	bear := ord(c.ID, 2, cex.Bear, cex.PurposeEntry, cex.SideBuy, lot.ID, "20", "5")
	r.m.Sync([]cex.OrderRecord{bull, bear})

	r.cex.setPositions("10", "0")
	reconcile(t, r, "venue equals the ledger", nil, false)
	r.cex.setPositions("10", "3")
	reconcile(t, r, "3 bear the pending buy of 5 explains", nil, false)
	r.cex.setPositions("10", "5")
	reconcile(t, r, "the whole pending buy filled on the venue", nil, false)

	// The positions cannot be read: an error, and not a mismatch however
	// long it lasts.
	r.cex.failReads(errTest)
	reconcile(t, r, "read failed", ErrPositionsUnread, false)
	r.clock.Add(5 * time.Second)
	reconcile(t, r, "read failed again", ErrPositionsUnread, false)
	r.cex.failReads(nil)

	// More than the order could have filled: a difference, a mismatch only
	// when it lasts.
	r.cex.setPositions("10", "6")
	reconcile(t, r, "first differing read", ErrPositionsDiffer, false)
	r.clock.Add(500 * time.Millisecond)
	reconcile(t, r, "differs half a second later", ErrPositionsDiffer, false)
	r.cex.setPositions("10", "3")
	reconcile(t, r, "agrees again", nil, false)
	r.clock.Add(time.Second)

	// A difference against the side of the pending order is not explained.
	// A failed read in between is no second reading.
	r.cex.setPositions("10", "-1")
	reconcile(t, r, "wrong side, first read", ErrPositionsDiffer, false)
	r.clock.Add(time.Second)
	r.cex.failReads(errTest)
	reconcile(t, r, "the second read failed", ErrPositionsUnread, false)
	r.cex.failReads(nil)
	reconcile(t, r, "the second read differs too", ErrPositionsDiffer, true)

	// The alarm is one Error event.
	if kinds := eventKinds(r.sink); len(kinds) != 1 || kinds[0] != "position_mismatch" {
		t.Errorf("events = %v, want one position_mismatch", kinds)
	}
	event := rowsOf[store.EventRow](r.sink)[0]
	if event.Level != "error" || event.Module != "inventory" {
		t.Errorf("mismatch event = %+v, want level error of module inventory", event)
	}

	// The mismatch is never cleared, also when the positions agree again.
	r.cex.setPositions("10", "0")
	reconcile(t, r, "agrees after the mismatch", nil, true)
	reconcile(t, r, "and again", nil, true)
	if kinds := eventKinds(r.sink); len(kinds) != 1 {
		t.Errorf("events = %v, want the one mismatch event only", kinds)
	}
}

// A final order explains nothing: the same difference that a pending order
// covered is a mismatch once that order has its final state.
func TestReconcileAfterFinalState(t *testing.T) {
	r := newRig(t, "1000", "10")
	c := r.openCycle(t, strategy.Short)
	lot := r.openLot(t)
	bull := fill(ord(c.ID, 1, cex.Bull, cex.PurposeEntry, cex.SideSell, lot.ID, "10", "10"), "10", "100", "0.01")
	bear := ord(c.ID, 2, cex.Bear, cex.PurposeEntry, cex.SideSell, lot.ID, "20", "5")
	r.m.Sync([]cex.OrderRecord{bull, bear})
	r.cex.setPositions("-10", "-2")
	reconcile(t, r, "2 sold of the pending sell of 5", nil, false)

	// The order ends with nothing filled, the venue still shows -2.
	r.m.Sync([]cex.OrderRecord{bull, fill(bear, "0", "0", "0")})
	reconcile(t, r, "first read after the final state", ErrPositionsDiffer, false)
	r.clock.Add(time.Second)
	reconcile(t, r, "second read", ErrPositionsDiffer, true)

	// The samples show both sides, signed.
	r.m.sample(r.clock.Now())
	s := lastOf(t, r.sink, func(store.InventorySampleRow) bool { return true })
	if !s.PosBull.Valid || !s.PosBull.Decimal.Equal(dec("-10")) || !s.PosBear.Decimal.Equal(dec("-2")) || !s.LedgerBull.Equal(dec("-10")) || !s.LedgerBear.IsZero() || !s.Mismatch {
		t.Errorf("sample = %+v, want venue -10 / -2, ledger -10 / 0, mismatch", s)
	}
}

// The sample row has no venue position until one was read: NULL, not zero.
func TestSampleBeforePositionsAreRead(t *testing.T) {
	r := newRig(t, "1000", "10")
	r.m.sample(r.clock.Now())
	s := lastOf(t, r.sink, func(store.InventorySampleRow) bool { return true })
	if s.PosBull.Valid || s.PosBear.Valid || s.TsMs != t0.UnixMilli() || !s.QuotaTotal.Equal(dec("10000")) || s.Mismatch {
		t.Errorf("sample before any read = %+v, want no venue positions and quota 10000", s)
	}
	r.cex.setPositions("0", "0")
	reconcile(t, r, "flat", nil, false)
	r.m.sample(r.clock.Now())
	if s = lastOf(t, r.sink, func(store.InventorySampleRow) bool { return true }); !s.PosBull.Valid || !s.PosBull.Decimal.IsZero() {
		t.Errorf("sample after a read = %+v, want venue positions of zero", s)
	}
}

// The sample also carries, for the board, what the venue reports with each
// held position - mark, unrealised PnL, liquidation price - and the
// account's equity and margin ratio. What was not reported is NULL: all of
// it before the first read, a leg's fields the venue left out, and the
// account's values on the paper venue, which has no account read.
func TestSampleCarriesTheVenueFigures(t *testing.T) {
	r := newRig(t, "1000", "10")
	r.m.sample(r.clock.Now())
	s := lastOf(t, r.sink, func(store.InventorySampleRow) bool { return true })
	if s.MarkBull.Valid || s.UplBear.Valid || s.LiqBull.Valid || s.Equity.Valid || s.MarginRatio.Valid {
		t.Errorf("sample before any read = %+v, want the venue's figures NULL", s)
	}

	r.cex.setPositions("10", "-5")
	r.cex.mu.Lock()
	r.cex.board[testPair.Bull.Symbol] = cex.Position{MarkPrice: decimal.NewNullDecimal(dec("43.02")), UnrealisedPnL: decimal.NewNullDecimal(dec("1.02")), LiqPrice: decimal.NewNullDecimal(dec("0"))}
	r.cex.mu.Unlock()
	if _, err := r.cex.ReadPositions(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.m.sample(r.clock.Now())
	s = lastOf(t, r.sink, func(store.InventorySampleRow) bool { return true })
	if !s.MarkBull.Decimal.Equal(dec("43.02")) || !s.UplBull.Decimal.Equal(dec("1.02")) || !s.LiqBull.Valid || !s.LiqBull.Decimal.IsZero() ||
		s.MarkBear.Valid || s.UplBear.Valid || s.LiqBear.Valid || s.Equity.Valid || s.MarginRatio.Valid {
		t.Errorf("sample = %+v, want the bull leg's mark 43.02, PnL 1.02, liquidation 0 and nothing else", s)
	}

	r.cex.mu.Lock()
	r.cex.assets = &cex.AccountAssets{Equity: dec("1234.5"), MarginRatio: dec("0.0123")}
	r.cex.mu.Unlock()
	r.m.sample(r.clock.Now())
	s = lastOf(t, r.sink, func(store.InventorySampleRow) bool { return true })
	if !s.Equity.Valid || !s.Equity.Decimal.Equal(dec("1234.5")) || !s.MarginRatio.Valid || !s.MarginRatio.Decimal.Equal(dec("0.0123")) {
		t.Errorf("sample = %+v, want equity 1234.5 and margin ratio 0.0123", s)
	}
}

// One differing read starts the confirmation for that difference only. A
// later read that differs by other amounts - another passing lag - starts
// it again instead of confirming it; a read that agrees ends it. The same
// difference read again at least reconcileConfirm later confirms, however
// long after: a slow read does not keep a lasting difference from being
// confirmed (audit of step 6).
func TestMismatchConfirmsOnlyTheSameDifference(t *testing.T) {
	r := newRig(t, "1000", "10")
	c := r.openCycle(t, strategy.Long)
	lot := r.openLot(t)
	bull := fill(ord(c.ID, 1, cex.Bull, cex.PurposeEntry, cex.SideBuy, lot.ID, "10", "10"), "10", "100", "0.01")
	bear := fill(ord(c.ID, 2, cex.Bear, cex.PurposeEntry, cex.SideBuy, lot.ID, "20", "5"), "5", "100", "0.01")
	r.m.Sync([]cex.OrderRecord{bull, bear})
	r.cex.setPositions("10", "4") // the venue's position lags the bear fill
	reconcile(t, r, "first: SOXS 1 short", ErrPositionsDiffer, false)
	r.clock.Add(1500 * time.Millisecond)
	exit := fill(ord(c.ID, 3, cex.Bull, cex.PurposeExit, cex.SideSell, "", "10", "3"), "3", "30", "0.01")
	r.m.Sync([]cex.OrderRecord{bull, bear, exit})
	r.cex.setPositions("10", "5") // now the bull exit lags; the bear lag is gone
	reconcile(t, r, "another difference: SOXL 3 long", ErrPositionsDiffer, false)

	r.clock.Add(500 * time.Millisecond)
	r.cex.setPositions("7", "5")
	reconcile(t, r, "agreeing", nil, false)
	r.cex.setPositions("10", "5")
	reconcile(t, r, "SOXL 3 long again, after an agreeing read", ErrPositionsDiffer, false)
	r.clock.Add(30 * time.Second) // no read meanwhile
	reconcile(t, r, "the same difference, 30 s later", ErrPositionsDiffer, true)
}

// The audit's case: in recovery the reads come 5 s apart (the retry of a
// failed read). The same difference on ten of them is a mismatch.
func TestPersistentMismatchInRecovery(t *testing.T) {
	r := newRig(t, "1000", "10")
	r.cex.setPositions("1", "0")
	for i := 0; i < 10; i++ {
		if e := r.m.Reconcile(context.Background()); !errors.Is(e, ErrPositionsDiffer) {
			t.Fatal(e)
		}
		r.clock.Add(5 * time.Second)
	}
	if !r.m.Mismatch() {
		t.Fatal("after 10 mismatching position reads spaced by the production recovery retry, mismatch is still false")
	}
}

// Two unrelated lags of the same size are no mismatch (review of the audit
// fixes): lot 1's SOXS lags by -1, the reads then fail for 20 s, and lot 2
// of the same size lags by -1 again. The ledger changed between the two
// reads, so the second is a first differing read.
func TestUnrelatedEqualDifferencesDoNotConfirm(t *testing.T) {
	r := newRig(t, "1000", "10")
	c := r.openCycle(t, strategy.Long)
	lot := r.openLot(t)
	bull := fill(ord(c.ID, 1, cex.Bull, cex.PurposeEntry, cex.SideBuy, lot.ID, "10", "10"), "10", "100", "0.01")
	bear := fill(ord(c.ID, 2, cex.Bear, cex.PurposeEntry, cex.SideBuy, lot.ID, "20", "5"), "5", "100", "0.01")
	r.m.Sync([]cex.OrderRecord{bull, bear})
	r.cex.setPositions("10", "4") // lot 1's bear lags
	reconcile(t, r, "first lag", ErrPositionsDiffer, false)
	r.cex.failReads(errTest)
	for range 20 {
		r.clock.Add(time.Second)
		_ = r.m.Reconcile(t.Context())
	}
	r.cex.failReads(nil)
	lot2 := r.openLot(t)
	bull2 := fill(ord(c.ID, 3, cex.Bull, cex.PurposeEntry, cex.SideBuy, lot2.ID, "10", "10"), "10", "100", "0.01")
	bear2 := fill(ord(c.ID, 4, cex.Bear, cex.PurposeEntry, cex.SideBuy, lot2.ID, "20", "5"), "5", "100", "0.01")
	r.m.Sync([]cex.OrderRecord{bull, bear, bull2, bear2})
	r.cex.setPositions("20", "9") // lot 2's bear lags by the same amount
	reconcile(t, r, "second, unrelated lag of the same size", ErrPositionsDiffer, false)
}

// PositionsInDoubt is the result of the last read: true after one that
// failed or differed from the ledger, false after one that agreed.
func TestPositionsInDoubt(t *testing.T) {
	r := newRig(t, "1000", "10")
	if r.m.PositionsInDoubt() {
		t.Fatal("in doubt before any read")
	}
	r.cex.setPositions("0", "0")
	r.cex.failReads(errTest)
	if e := r.m.Reconcile(context.Background()); !errors.Is(e, ErrPositionsUnread) || !r.m.PositionsInDoubt() {
		t.Fatalf("failed read: %v, in doubt %t", e, r.m.PositionsInDoubt())
	}
	r.cex.failReads(nil)
	r.cex.setPositions("1", "0")
	if e := r.m.Reconcile(context.Background()); !errors.Is(e, ErrPositionsDiffer) || !r.m.PositionsInDoubt() {
		t.Fatalf("read that differs: %v, in doubt %t", e, r.m.PositionsInDoubt())
	}
	r.cex.setPositions("0", "0")
	if e := r.m.Reconcile(context.Background()); e != nil || r.m.PositionsInDoubt() {
		t.Fatalf("read that agrees: %v, in doubt %t", e, r.m.PositionsInDoubt())
	}
}
