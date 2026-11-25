package cex

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"etf-pair-arbitrage/core/store"
)

// The cycle the order tests place for: created at this moment, so its id is
// c1795406754673 and its orders' ids start with etfp1795406754673-.
var (
	cycleAt     = time.UnixMilli(1795406754673)
	testCycleID = "c1795406754673"
)

// orderReq is the seq-th order of the test cycle: an IOC limit order of the
// given leg, purpose and side, booked on lot <cycle>-1.
func orderReq(seq int, leg Leg, purpose OrderPurpose, side Side, price, qty string) OrderRequest {
	symbol := Symbol("SOXLUSDT")
	if leg == Bear {
		symbol = "SOXSUSDT"
	}
	return OrderRequest{ClientOrderID: NewClientOrderID(cycleAt, seq, leg, purpose), Symbol: symbol, Leg: leg, Side: side, Price: dec(price), Qty: dec(qty),
		QuotaPrice: dec(price), ReduceOnly: purpose == PurposeExit || purpose == PurposeForced || purpose == PurposeTrim, Purpose: purpose, CycleID: testCycleID, LotID: testCycleID + "-1"}
}

// report is what the venue says about an order: its status and what was
// filled, with the fee when feeKnown.
func report(req OrderRequest, status OrderStatus, cumQty, cumValue, fee string, feeKnown bool, at time.Time) OrderRecord {
	r := OrderRecord{Request: OrderRequest{ClientOrderID: req.ClientOrderID, Symbol: req.Symbol, Side: req.Side, Price: req.Price, Qty: req.Qty},
		OrderID: OrderID("v-" + string(req.ClientOrderID)), Status: status, CumExecQty: dec(cumQty), CumExecValue: dec(cumValue), Fee: dec(fee), FeeKnown: feeKnown, ExchangeUpdatedAt: at}
	if r.CumExecQty.IsPositive() {
		r.AvgPrice = r.CumExecValue.Div(r.CumExecQty)
	}
	if status == StatusCancelled {
		r.CancelReason = "ioc_not_full_cancel"
	}
	return r
}

// accept makes the fake venue accept every order.
func accept(req OrderRequest) (OrderAck, error) {
	return OrderAck{OrderID: OrderID("v-" + string(req.ClientOrderID))}, nil
}

// order returns the table's record of an order.
func (r *testRig) order(t *testing.T, id ClientOrderID) OrderRecord {
	t.Helper()
	for _, rec := range r.m.CycleOrders(testCycleID) {
		if rec.Request.ClientOrderID == id {
			return rec
		}
	}
	t.Fatalf("the table holds no order %s", id)
	return OrderRecord{}
}

// placeAndWait places the orders and waits until each has left pending or
// become unknown: its send goroutine has booked the venue's answer.
func (r *testRig) placeAndWait(t *testing.T, reqs ...OrderRequest) {
	t.Helper()
	if err := r.m.PlaceOrders(reqs...); err != nil {
		t.Fatal(err)
	}
	for _, req := range reqs {
		waitFor(t, "the answer to "+string(req.ClientOrderID), func() bool {
			rec := r.order(t, req.ClientOrderID)
			return rec.Status != StatusPending || rec.Unknown
		})
	}
}

// stepOrdersAt moves the clock and advances the order table once.
func (r *testRig) stepOrdersAt(at time.Time) {
	r.clock.Set(at)
	r.m.stepOrders(context.Background(), at)
}

// orderRows returns the cex_order rows recorded for an order, oldest first.
func (r *testRig) orderRows(id ClientOrderID) []store.OrderRow {
	var out []store.OrderRow
	for _, row := range r.sink.Rows("cex_order") {
		if o := row.(store.OrderRow); o.ClientOrderID == string(id) {
			out = append(out, o)
		}
	}
	return out
}

// changedSince reports whether the channel taken before was closed.
func changedSince(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// An order id carries the cycle's creation, the sequence number, the leg
// and the purpose; ids of one cycle sort by sequence number; anything else
// is not this program's.
func TestClientOrderID(t *testing.T) {
	id := NewClientOrderID(cycleAt, 7, Bull, PurposeEntry)
	if id != "etfp1795406754673-0007BE" {
		t.Fatalf("id = %s, want etfp1795406754673-0007BE", id)
	}
	cycleID, leg, purpose, ok := parseClientOrderID(NewClientOrderID(cycleAt, 371, Bear, PurposeTrim)) // 371 = AB in base 36
	if !ok || cycleID != testCycleID || leg != Bear || purpose != PurposeTrim {
		t.Errorf("parsed %q %s %s %t", cycleID, leg, purpose, ok)
	}
	ids := []ClientOrderID{NewClientOrderID(cycleAt, 36, Bull, PurposeExit), NewClientOrderID(cycleAt, 9, Bear, PurposeForced), NewClientOrderID(cycleAt, 10, Bull, PurposeComplete)}
	slices.Sort(ids)
	if ids[0] != "etfp1795406754673-0009RF" || ids[1] != "etfp1795406754673-000ABC" || ids[2] != "etfp1795406754673-0010BX" {
		t.Errorf("sorted ids = %v, want sequence 9, 10, 36", ids)
	}
	for _, seq := range []int{7, 36, 371} {
		if got, ok := ClientOrderSeq(NewClientOrderID(cycleAt, seq, Bear, PurposeExit)); !ok || got != seq {
			t.Errorf("ClientOrderSeq of the id of %d = %d, %t", seq, got, ok)
		}
	}
	for _, other := range []ClientOrderID{"ep1795406754673-0007BE", "etfp1795406754673-0007BZ", "etfp1795406754673-0007QE", "etfpx-0007BE", "etfp1795406754673-007BE", ""} {
		if _, _, _, ok := parseClientOrderID(other); ok {
			t.Errorf("%q was taken for an id of this program", other)
		}
		if _, ok := ClientOrderSeq(other); ok {
			t.Errorf("ClientOrderSeq read a sequence from %q", other)
		}
	}
}

// The venue accepts an order: it is acked, with its row committed before it
// was sent. A second later, still without an outcome, it is unknown and
// queried, never sent again. The push that was late then ends it, and every
// change carries a larger UpdateSeq and closes the changed channel.
func TestOrderLatePushAfterTheAck(t *testing.T) {
	r := newRig(t, testConfig(), t0)
	r.venue.place = accept
	req := orderReq(1, Bull, PurposeEntry, SideBuy, "40.47", "10")
	changed := r.m.OrdersChanged()
	r.placeAndWait(t, req)
	if !changedSince(changed) {
		t.Fatal("OrdersChanged was not closed by the registration")
	}
	rec := r.order(t, req.ClientOrderID)
	if rec.Status != StatusAcked || rec.Unknown || rec.OrderID != "v-etfp1795406754673-0001BE" || !rec.SubmittedAt.Equal(t0) || !rec.AckedAt.Equal(t0) {
		t.Fatalf("after the ack: %+v", rec)
	}
	rows := r.orderRows(req.ClientOrderID)
	if len(rows) != 2 || rows[0].Status != "pending" || rows[0].SubmittedMs != t0.UnixMilli() || !rows[0].QuotaPrice.Equal(dec("40.47")) || rows[0].LotID != testCycleID+"-1" || rows[1].Status != "acked" {
		t.Fatalf("rows = %+v, want the pending row first and then the acked one", rows)
	}
	seqs := []uint64{rec.UpdateSeq}

	// Within a second of the ack it is in flight, not unknown.
	r.stepOrdersAt(t0.Add(999 * time.Millisecond))
	if rec := r.order(t, req.ClientOrderID); rec.Unknown || r.venue.callCount("OrderByClientID") != 0 {
		t.Fatalf("999 ms after the ack: unknown %t, %d queries", rec.Unknown, r.venue.callCount("OrderByClientID"))
	}
	changed = r.m.OrdersChanged()
	r.stepOrdersAt(t0.Add(time.Second))
	rec = r.order(t, req.ClientOrderID)
	if !rec.Unknown || !rec.UnknownSince.Equal(t0.Add(time.Second)) || rec.Status != StatusAcked || !changedSince(changed) {
		t.Fatalf("a second after the ack: %+v, want unknown since then", rec)
	}
	if n := r.venue.callCount("OrderByClientID"); n != 1 {
		t.Fatalf("%d queries at the moment it became unknown, want 1", n)
	}
	seqs = append(seqs, rec.UpdateSeq)
	if got := r.m.UnresolvedOrders(); len(got) != 1 || got[0].Request.ClientOrderID != req.ClientOrderID {
		t.Errorf("unresolved = %+v", got)
	}

	// The push arrives: filled, no longer unknown, the fee with it.
	changed = r.m.OrdersChanged()
	r.clock.Set(t0.Add(1300 * time.Millisecond))
	r.m.onPrivateEvent(PrivateEvent{Order: ptr(report(req, StatusFilled, "10", "404.4", "0.026286", true, t0.Add(40*time.Millisecond)))})
	rec = r.order(t, req.ClientOrderID)
	if rec.Status != StatusFilled || rec.Unknown || !rec.CumExecQty.Equal(dec("10")) || !rec.CumExecValue.Equal(dec("404.4")) || !rec.AvgPrice.Equal(dec("40.44")) ||
		!rec.Fee.Equal(dec("0.026286")) || !rec.FeeKnown || !rec.ExchangeUpdatedAt.Equal(t0.Add(40*time.Millisecond)) || !rec.UpdateReceivedAt.Equal(t0.Add(1300*time.Millisecond)) || !changedSince(changed) {
		t.Fatalf("after the late push: %+v", rec)
	}
	seqs = append(seqs, rec.UpdateSeq)
	if !slices.IsSorted(seqs) || seqs[0] == seqs[1] || seqs[1] == seqs[2] {
		t.Errorf("UpdateSeq over the changes = %v, want strictly increasing", seqs)
	}
	// Finished: not queried again, not unresolved, and sent exactly once.
	r.stepOrdersAt(t0.Add(5 * time.Second))
	if n := r.venue.callCount("OrderByClientID"); n != 1 || len(r.m.UnresolvedOrders()) != 0 || len(r.venue.placedIDs()) != 1 {
		t.Errorf("after the outcome: %d queries, %d unresolved, sent %d times", n, len(r.m.UnresolvedOrders()), len(r.venue.placedIDs()))
	}
	if last := r.orderRows(req.ClientOrderID); last[len(last)-1].Status != "filled" || last[len(last)-1].Unknown || !last[len(last)-1].CumExecValue.Equal(dec("404.4")) {
		t.Errorf("the last row = %+v", last[len(last)-1])
	}
}

func ptr[T any](v T) *T { return &v }

// A send whose outcome the venue did not state - the adapter's "state
// unknown", or any failure such as a timeout - leaves the order unknown at
// once. It is queried every second and never resent; the query's answer, a
// partially filled and cancelled IOC here, ends it.
func TestOrderTimeoutIsUnknownUntilTheQueryAnswers(t *testing.T) {
	for name, sendErr := range map[string]error{
		"the adapter reports the state unknown": fmt.Errorf("bitget: code 40010 request timeout: %w", ErrOrderStateUnknown),
		"the call timed out":                    context.DeadlineExceeded,
	} {
		r := newRig(t, testConfig(), t0)
		r.venue.place = func(OrderRequest) (OrderAck, error) { return OrderAck{}, sendErr }
		req := orderReq(1, Bear, PurposeEntry, SideBuy, "49.72", "10")
		r.placeAndWait(t, req)
		rec := r.order(t, req.ClientOrderID)
		if !rec.Unknown || rec.Status != StatusPending || !rec.UnknownSince.Equal(t0) || !rec.SentAt.Equal(t0) {
			t.Fatalf("%s: %+v, want pending and unknown since the send", name, rec)
		}
		if rows := r.orderRows(req.ClientOrderID); rec.ErrorMessage != sendErr.Error() || rows[len(rows)-1].ErrorMessage != sendErr.Error() {
			t.Errorf("%s: the record's error text is %q, the row's %q; want why the outcome is unknown: %q", name, rec.ErrorMessage, rows[len(rows)-1].ErrorMessage, sendErr)
		}
		// One query per second while the venue does not know it.
		for _, c := range []struct {
			after   time.Duration
			queries int
		}{{0, 1}, {500 * time.Millisecond, 1}, {time.Second, 2}, {1900 * time.Millisecond, 2}, {2 * time.Second, 3}} {
			r.stepOrdersAt(t0.Add(c.after))
			if n := r.venue.callCount("OrderByClientID"); n != c.queries {
				t.Fatalf("%s: %d queries at +%s, want %d", name, n, c.after, c.queries)
			}
		}
		// The venue had it: 4 of 10 filled, the rest cancelled.
		r.venue.orderInfo[req.ClientOrderID] = report(req, StatusCancelled, "4", "198.88", "0.0129272", true, t0.Add(30*time.Millisecond))
		r.stepOrdersAt(t0.Add(3 * time.Second))
		rec = r.order(t, req.ClientOrderID)
		if rec.Status != StatusCancelled || rec.Unknown || !rec.CumExecQty.Equal(dec("4")) || !rec.CumExecValue.Equal(dec("198.88")) || !rec.Fee.Equal(dec("0.0129272")) || !rec.FeeKnown ||
			rec.CancelReason != "ioc_not_full_cancel" || rec.OrderID == "" {
			t.Fatalf("%s: after the query answered: %+v", name, rec)
		}
		r.stepOrdersAt(t0.Add(10 * time.Second))
		if queries, sent := r.venue.callCount("OrderByClientID"), len(r.venue.placedIDs()); queries != 4 || sent != 1 {
			t.Errorf("%s: %d queries and %d sends in all, want 4 and 1", name, queries, sent)
		}
	}
}

// "No such order" from the venue is not taken at its word: the record is
// marked NotFound only when the order has been unknown for 30 seconds and a
// read of the fills since it was registered shows none with its id. It then
// stays unknown and queried until exec's verdict ends it as never_arrived.
func TestOrderNotFoundOnlyAfterThirtySecondsAndNoFill(t *testing.T) {
	r := newRig(t, testConfig(), t0)
	r.venue.place = func(OrderRequest) (OrderAck, error) { return OrderAck{}, context.DeadlineExceeded }
	req := orderReq(1, Bull, PurposeEntry, SideBuy, "40.47", "10")
	r.placeAndWait(t, req)

	// The venue answers 25204 from the first query on.
	for s := 0; s < 30; s++ {
		r.stepOrdersAt(t0.Add(time.Duration(s) * time.Second))
	}
	if rec := r.order(t, req.ClientOrderID); rec.NotFound || !rec.Unknown || r.venue.callCount("Fills") != 0 {
		t.Fatalf("29 s unknown: not found %t, unknown %t, %d fills reads; want no verdict and no fills read yet", rec.NotFound, rec.Unknown, r.venue.callCount("Fills"))
	}
	// At 30 s the fills are read, from the order's registration on. They
	// hold another program's fill only: NotFound, still unknown.
	r.venue.fillsRead = []Fill{{ExecID: "x1", ClientOrderID: "ep-another-program", Symbol: "SOXLUSDT"}}
	changed := r.m.OrdersChanged()
	r.stepOrdersAt(t0.Add(30 * time.Second))
	rec := r.order(t, req.ClientOrderID)
	if !rec.NotFound || !rec.Unknown || rec.Status.Terminal() || !changedSince(changed) {
		t.Fatalf("30 s unknown with no fill: %+v, want NotFound and still unknown", rec)
	}
	if len(r.venue.fillsSince) != 1 || !r.venue.fillsSince[0].Equal(t0) {
		t.Fatalf("the fills were read since %v, want once since the order's registration", r.venue.fillsSince)
	}
	if row := r.orderRows(req.ClientOrderID); !row[len(row)-1].NotFound || row[len(row)-1].UnknownSinceMs != t0.UnixMilli() {
		t.Errorf("the last row = %+v", row[len(row)-1])
	}
	if rows := r.sink.Rows("cex_fill"); len(rows) != 0 {
		t.Errorf("fill rows = %+v, want none: the fill read is another program's", rows)
	}
	// Still queried each second, the fills not read again.
	queries := r.venue.callCount("OrderByClientID")
	r.stepOrdersAt(t0.Add(31 * time.Second))
	if r.venue.callCount("OrderByClientID") != queries+1 || r.venue.callCount("Fills") != 1 {
		t.Errorf("after NotFound: %d more queries, %d fills reads; want 1 and 1", r.venue.callCount("OrderByClientID")-queries, r.venue.callCount("Fills"))
	}

	// exec's verdict ends it.
	r.m.MarkNeverArrived(req.ClientOrderID)
	rec = r.order(t, req.ClientOrderID)
	if rec.Status != StatusNeverArrived || rec.Unknown || !rec.Status.Terminal() || len(r.m.UnresolvedOrders()) != 0 {
		t.Fatalf("after the verdict: %+v", rec)
	}
	queries = r.venue.callCount("OrderByClientID")
	r.stepOrdersAt(t0.Add(40 * time.Second))
	if r.venue.callCount("OrderByClientID") != queries || len(r.venue.placedIDs()) != 1 {
		t.Error("an order ended as never_arrived was queried or sent again")
	}
	// A verdict on a finished order changes nothing.
	seq := rec.UpdateSeq
	r.m.MarkNeverArrived(req.ClientOrderID)
	if got := r.order(t, req.ClientOrderID).UpdateSeq; got != seq {
		t.Error("a second verdict changed the record")
	}
}

// The venue answers "no such order" while its fills carry the order's id:
// it executed the order after all. Every fill of the order is written, the
// record is not marked NotFound, and the order is queried on without the
// fills being read again, with one warning.
func TestOrderWithFillsIsNotNotFound(t *testing.T) {
	logs := captureLogs(t)
	r := newRig(t, testConfig(), t0)
	r.venue.place = func(OrderRequest) (OrderAck, error) { return OrderAck{}, context.DeadlineExceeded }
	req := orderReq(1, Bull, PurposeEntry, SideBuy, "40.47", "10")
	r.placeAndWait(t, req)
	fill := Fill{ExecID: "x2", OrderID: "v9", ClientOrderID: req.ClientOrderID, Symbol: "SOXLUSDT", Side: SideBuy, Price: dec("40.46"), Qty: dec("6"), Value: dec("242.76"), Fee: dec("0.0157794"), ExecTime: t0}
	second := fill
	second.ExecID, second.Qty, second.Value, second.Fee = "x3", dec("4"), dec("161.84"), dec("0.0105196")
	r.venue.fillsRead = []Fill{{ExecID: "x1", ClientOrderID: "ep-another-program", Symbol: "SOXLUSDT"}, fill, second}
	for s := 0; s <= 33; s++ {
		r.stepOrdersAt(t0.Add(time.Duration(s) * time.Second))
	}
	if rec := r.order(t, req.ClientOrderID); rec.NotFound || !rec.Unknown || rec.Status.Terminal() {
		t.Fatalf("fills carry its id: %+v, want unknown and not NotFound", rec)
	}
	rows := r.sink.Rows("cex_fill")
	if len(rows) != 2 || rows[0].(store.FillRow).ExecID != "x2" || rows[1].(store.FillRow).ExecID != "x3" {
		t.Errorf("fill rows = %+v, want both fills of this order, once", rows)
	}
	if fills, queries := r.venue.callCount("Fills"), r.venue.callCount("OrderByClientID"); fills != 1 || queries != 34 {
		t.Errorf("%d fills reads and %d queries in 34 seconds, want 1 and 34", fills, queries)
	}
	warnings := 0
	for _, line := range logs.take() {
		if line == "WARN cex: the venue has no such order but fills carry its id; querying on" {
			warnings++
		}
	}
	if warnings != 1 {
		t.Errorf("%d warnings about the fills, want one", warnings)
	}
	// The venue's order query catches up: the order is booked from it.
	r.venue.orderInfo[req.ClientOrderID] = report(req, StatusFilled, "10", "404.6", "0.026299", true, t0.Add(30*time.Millisecond))
	r.stepOrdersAt(t0.Add(34 * time.Second))
	if rec := r.order(t, req.ClientOrderID); rec.Status != StatusFilled || rec.Unknown || !rec.CumExecQty.Equal(dec("10")) || !rec.FeeKnown {
		t.Errorf("after the query answered: %+v", rec)
	}
}

// NotFound ends when the venue reports the order with a fill after all: an
// order the venue still works on must not be judged never arrived.
func TestOrderNotFoundEndsWhenTheVenueReportsAFill(t *testing.T) {
	r := newRig(t, testConfig(), t0)
	r.venue.place = func(OrderRequest) (OrderAck, error) { return OrderAck{}, context.DeadlineExceeded }
	req := orderReq(1, Bull, PurposeEntry, SideBuy, "40.47", "10")
	r.placeAndWait(t, req)
	for s := 0; s <= 30; s++ {
		r.stepOrdersAt(t0.Add(time.Duration(s) * time.Second))
	}
	if rec := r.order(t, req.ClientOrderID); !rec.NotFound {
		t.Fatalf("30 s without a trace: %+v, want NotFound", rec)
	}
	r.m.onPrivateEvent(PrivateEvent{Order: ptr(report(req, StatusAcked, "3", "121.38", "0", false, t0.Add(31*time.Second)))})
	rec := r.order(t, req.ClientOrderID)
	if rec.NotFound || !rec.Unknown || !rec.CumExecQty.Equal(dec("3")) || rec.Status.Terminal() {
		t.Fatalf("after a report with a fill: %+v, want the fill booked, NotFound ended, still unknown", rec)
	}
	if rows := r.orderRows(req.ClientOrderID); rows[len(rows)-1].NotFound {
		t.Errorf("the last row still says not found: %+v", rows[len(rows)-1])
	}
}

// Order queries that fail are tried again every second; the first failure
// and the first answer after failures are logged above Debug, once each.
func TestOrderQueryFailuresAreLoggedOnce(t *testing.T) {
	logs := captureLogs(t)
	r := newRig(t, testConfig(), t0)
	r.venue.place = func(OrderRequest) (OrderAck, error) { return OrderAck{}, context.DeadlineExceeded }
	req := orderReq(1, Bull, PurposeEntry, SideBuy, "40.47", "10")
	r.placeAndWait(t, req)
	logs.take()
	r.venue.orderErr = errors.New("http 502")
	for s := 0; s < 5; s++ {
		r.stepOrdersAt(t0.Add(time.Duration(s) * time.Second))
	}
	if got := logs.take(); !slices.Equal(got, []string{"WARN cex: order queries failing; retrying every turn"}) {
		t.Errorf("five failed queries logged %q, want one warning", got)
	}
	r.venue.orderErr = nil // the venue answers again: no such order
	r.stepOrdersAt(t0.Add(5 * time.Second))
	r.stepOrdersAt(t0.Add(6 * time.Second))
	if got := logs.take(); !slices.Equal(got, []string{"INFO cex: order queries answered again"}) {
		t.Errorf("the recovery logged %q, want one line", got)
	}
	if n := r.venue.callCount("OrderByClientID"); n != 7 {
		t.Errorf("%d queries in seven seconds, want one a second through the failures", n)
	}
}

// A refusal the venue states and "nothing left the process" are final at
// once: rejected with the venue's code and kind, not_sent; neither is
// queried.
func TestOrderRejectedAndNotSent(t *testing.T) {
	r := newRig(t, testConfig(), t0)
	rejected, notSent := orderReq(1, Bull, PurposeEntry, SideBuy, "40.47", "10"), orderReq(2, Bear, PurposeEntry, SideBuy, "49.72", "8")
	r.venue.place = func(req OrderRequest) (OrderAck, error) {
		if req.ClientOrderID == rejected.ClientOrderID {
			return OrderAck{}, fmt.Errorf("bitget: place-order: %w", &VenueError{Code: "25213", Message: "price exceeds the limit", Kind: ErrorPriceLimit})
		}
		return OrderAck{}, fmt.Errorf("bitget: the trade connection is down: %w", ErrNotSent)
	}
	r.placeAndWait(t, rejected, notSent)
	if rec := r.order(t, rejected.ClientOrderID); rec.Status != StatusRejected || rec.ErrorCode != "25213" || rec.ErrorMessage != "price exceeds the limit" || rec.ErrorKind != ErrorPriceLimit || rec.Unknown || !rec.CumExecQty.IsZero() {
		t.Errorf("the refused order: %+v", rec)
	}
	if rec := r.order(t, notSent.ClientOrderID); rec.Status != StatusNotSent || rec.Unknown || rec.ErrorMessage == "" || rec.ErrorKind != "" {
		t.Errorf("the order that was not sent: %+v", rec)
	}
	r.stepOrdersAt(t0.Add(5 * time.Second))
	if r.venue.callCount("OrderByClientID") != 0 || len(r.m.UnresolvedOrders()) != 0 {
		t.Errorf("%d queries, %d unresolved for two finished orders", r.venue.callCount("OrderByClientID"), len(r.m.UnresolvedOrders()))
	}
	if rows := r.orderRows(rejected.ClientOrderID); rows[len(rows)-1].Status != "rejected" || rows[len(rows)-1].ErrorCode != "25213" {
		t.Errorf("the refused order's last row = %+v", rows[len(rows)-1])
	}
}

// An IOC the book did not meet is cancelled with nothing filled, as the
// live trial of 11-23 recorded twelve times: a final state without a fill,
// which needs no fee and no query. One that met part of its size is
// cancelled with that part and its fee.
func TestOrderCancelledIOCWithoutAndWithAFill(t *testing.T) {
	r := newRig(t, testConfig(), t0)
	r.venue.place = accept
	zero, part := orderReq(1, Bull, PurposeEntry, SideBuy, "40.47", "10"), orderReq(2, Bear, PurposeEntry, SideBuy, "49.72", "8")
	r.placeAndWait(t, zero, part)
	r.m.onPrivateEvent(PrivateEvent{Order: ptr(report(zero, StatusCancelled, "0", "0", "0", false, t0.Add(20*time.Millisecond)))})
	r.m.onPrivateEvent(PrivateEvent{Order: ptr(report(part, StatusCancelled, "3", "149.13", "0.00969345", true, t0.Add(20*time.Millisecond)))})

	rec := r.order(t, zero.ClientOrderID)
	if rec.Status != StatusCancelled || !rec.CumExecQty.IsZero() || !rec.CumExecValue.IsZero() || !rec.Fee.IsZero() || rec.FeeKnown || rec.Unknown || rec.CancelReason != "ioc_not_full_cancel" {
		t.Errorf("the zero-fill IOC: %+v", rec)
	}
	rec = r.order(t, part.ClientOrderID)
	if rec.Status != StatusCancelled || !rec.CumExecQty.Equal(dec("3")) || !rec.CumExecValue.Equal(dec("149.13")) || !rec.AvgPrice.Equal(dec("49.71")) || !rec.Fee.Equal(dec("0.00969345")) || !rec.FeeKnown {
		t.Errorf("the partly filled IOC: %+v", rec)
	}
	r.stepOrdersAt(t0.Add(5 * time.Second))
	if n := r.venue.callCount("OrderByClientID"); n != 0 {
		t.Errorf("%d queries for two finished orders, the zero-fill one needing no fee", n)
	}

	// The venue took an order and cancelled it for its price: the report's
	// kind says so, without the reader knowing the venue's reason texts.
	band := orderReq(3, Bull, PurposeForced, SideSell, "30", "10")
	r.placeAndWait(t, band)
	cancelled := report(band, StatusCancelled, "0", "0", "0", false, t0.Add(20*time.Millisecond))
	cancelled.CancelReason, cancelled.ErrorKind = "limit_price_exceed_cancel", ErrorPriceLimit
	r.m.onPrivateEvent(PrivateEvent{Order: &cancelled})
	if rec := r.order(t, band.ClientOrderID); rec.Status != StatusCancelled || rec.ErrorKind != ErrorPriceLimit || rec.CancelReason != "limit_price_exceed_cancel" {
		t.Errorf("the order cancelled for its price: %+v, want the price-limit kind", rec)
	}
	if rec := r.order(t, zero.ClientOrderID); rec.ErrorKind != "" {
		t.Errorf("an IOC that met no level has the kind %q", rec.ErrorKind)
	}
}

// The final state can arrive before the fee. The quantity counts at once;
// the order is then queried each second until a report carries the fee of
// the whole filled quantity. "Fee not known" and "fee is zero" differ.
func TestOrderFeeArrivingAfterTheFinalState(t *testing.T) {
	r := newRig(t, testConfig(), t0)
	r.venue.place = accept
	req := orderReq(1, Bull, PurposeExit, SideSell, "40.40", "10")
	r.placeAndWait(t, req)
	r.m.onPrivateEvent(PrivateEvent{Order: ptr(report(req, StatusFilled, "10", "404.5", "0", false, t0.Add(20*time.Millisecond)))})
	rec := r.order(t, req.ClientOrderID)
	if rec.Status != StatusFilled || !rec.CumExecQty.Equal(dec("10")) || rec.FeeKnown || !rec.Fee.IsZero() {
		t.Fatalf("filled, the fee not reported: %+v", rec)
	}
	// The venue's order query does not have the fee yet either.
	r.venue.orderInfo[req.ClientOrderID] = report(req, StatusFilled, "10", "404.5", "0", false, t0.Add(20*time.Millisecond))
	seq := rec.UpdateSeq
	r.stepOrdersAt(t0.Add(time.Second))
	r.stepOrdersAt(t0.Add(2 * time.Second))
	if rec := r.order(t, req.ClientOrderID); rec.FeeKnown || rec.UpdateSeq != seq || r.venue.callCount("OrderByClientID") != 2 {
		t.Fatalf("two queries without the fee: fee known %t, seq %d (was %d), %d queries", rec.FeeKnown, rec.UpdateSeq, seq, r.venue.callCount("OrderByClientID"))
	}
	// Then it has.
	r.venue.orderInfo[req.ClientOrderID] = report(req, StatusFilled, "10", "404.5", "0.0262925", true, t0.Add(20*time.Millisecond))
	r.stepOrdersAt(t0.Add(3 * time.Second))
	rec = r.order(t, req.ClientOrderID)
	if !rec.FeeKnown || !rec.Fee.Equal(dec("0.0262925")) || rec.Status != StatusFilled || rec.UpdateSeq <= seq {
		t.Fatalf("after the fee arrived: %+v", rec)
	}
	r.stepOrdersAt(t0.Add(10 * time.Second))
	if n := r.venue.callCount("OrderByClientID"); n != 3 {
		t.Errorf("%d queries in all, want 3: none once the fee is known", n)
	}
}

// A fee is known for the quantity it was reported with. A partial fill
// arrives with its fee; the final report has the whole quantity and no fee
// yet. The fee held then covers 4 of 10, so it is not known: the order is
// queried, and the cycle cannot settle on 0.010517 instead of 0.0262925.
func TestOrderFeeUnknownAgainWhenTheQuantityGrows(t *testing.T) {
	r := newRig(t, testConfig(), t0)
	r.venue.place = accept
	req := orderReq(1, Bull, PurposeEntry, SideBuy, "40.47", "10")
	r.placeAndWait(t, req)
	r.m.onPrivateEvent(PrivateEvent{Order: ptr(report(req, StatusAcked, "4", "161.8", "0.010517", true, t0.Add(10*time.Millisecond)))})
	if rec := r.order(t, req.ClientOrderID); !rec.FeeKnown || !rec.Fee.Equal(dec("0.010517")) {
		t.Fatalf("after the partial fill with its fee: %+v", rec)
	}
	r.m.onPrivateEvent(PrivateEvent{Order: ptr(report(req, StatusFilled, "10", "404.5", "0", false, t0.Add(20*time.Millisecond)))})
	rec := r.order(t, req.ClientOrderID)
	if rec.Status != StatusFilled || !rec.CumExecQty.Equal(dec("10")) || !rec.CumExecValue.Equal(dec("404.5")) || rec.FeeKnown || !rec.Fee.Equal(dec("0.010517")) {
		t.Fatalf("filled 10 with the fee of 4: %+v, want the fee not known", rec)
	}
	if rows := r.orderRows(req.ClientOrderID); rows[len(rows)-1].FeeKnown || !rows[len(rows)-1].CumExecQty.Equal(dec("10")) {
		t.Errorf("the last row = %+v, want 10 filled and the fee not known", rows[len(rows)-1])
	}
	// So it is queried until the fee of the whole quantity is reported.
	r.stepOrdersAt(t0.Add(time.Second))
	if n := r.venue.callCount("OrderByClientID"); n != 1 {
		t.Fatalf("%d queries for a filled order without its whole fee, want 1", n)
	}
	r.venue.orderInfo[req.ClientOrderID] = report(req, StatusFilled, "10", "404.5", "0.0262925", true, t0.Add(20*time.Millisecond))
	r.stepOrdersAt(t0.Add(2 * time.Second))
	if rec := r.order(t, req.ClientOrderID); !rec.FeeKnown || !rec.Fee.Equal(dec("0.0262925")) {
		t.Fatalf("after the query: %+v, want the fee of the whole quantity", rec)
	}
	// A late copy of the partial report moves nothing back.
	r.m.onPrivateEvent(PrivateEvent{Order: ptr(report(req, StatusAcked, "4", "161.8", "0.010517", true, t0.Add(10*time.Millisecond)))})
	if rec := r.order(t, req.ClientOrderID); !rec.FeeKnown || !rec.Fee.Equal(dec("0.0262925")) || !rec.CumExecQty.Equal(dec("10")) {
		t.Errorf("after a late partial report: %+v", rec)
	}
}

// An order's way is timed on the record and in its row: handed to the venue
// and accepted (local clock), taken by the venue's gateway and created by it
// (the venue's clock, from the acceptance), and the arrival of its final
// state (local clock). An acceptance that does not say when the order was
// created leaves that to the first report that does.
func TestOrderTimestampsOfItsWay(t *testing.T) {
	r := newRig(t, testConfig(), t0)
	ms := time.Millisecond
	venueT := t0.Add(-800 * ms) // the venue's clock runs apart from the local one
	r.venue.place = func(req OrderRequest) (OrderAck, error) {
		r.clock.Set(t0.Add(24 * ms)) // the acceptance is in hand 24 ms after the send
		ack := OrderAck{OrderID: OrderID("v-" + string(req.ClientOrderID)), AcceptedAt: venueT.Add(9 * ms)}
		if req.Leg == Bull {
			ack.CreatedAt = venueT.Add(12 * ms) // an acceptance over the WebSocket says so; one over REST does not
		}
		return ack, nil
	}
	bull := orderReq(1, Bull, PurposeEntry, SideBuy, "40.47", "10")
	r.placeAndWait(t, bull)
	rec := r.order(t, bull.ClientOrderID)
	if !rec.SubmittedAt.Equal(t0) || !rec.SentAt.Equal(t0) || !rec.AckedAt.Equal(t0.Add(24*ms)) || !rec.AcceptedAt.Equal(venueT.Add(9*ms)) || !rec.ExchangeCreatedAt.Equal(venueT.Add(12*ms)) || !rec.FinalAt.IsZero() {
		t.Fatalf("after the acceptance: %+v", rec)
	}
	// The final state arrives by push 31 ms after the send, without the fee.
	r.clock.Set(t0.Add(31 * ms))
	final := report(bull, StatusFilled, "10", "404.5", "0", false, venueT.Add(14*ms))
	final.ExchangeCreatedAt = venueT.Add(13 * ms) // the report's own creation time does not replace the acceptance's
	r.m.onPrivateEvent(PrivateEvent{Order: &final})
	rec = r.order(t, bull.ClientOrderID)
	if !rec.FinalAt.Equal(t0.Add(31*ms)) || !rec.ExchangeCreatedAt.Equal(venueT.Add(12*ms)) || !rec.ExchangeUpdatedAt.Equal(venueT.Add(14*ms)) {
		t.Fatalf("after the final state: final %s, created %s, updated %s", rec.FinalAt.Sub(t0), rec.ExchangeCreatedAt.Sub(venueT), rec.ExchangeUpdatedAt.Sub(venueT))
	}
	// The fee arrives later and changes the record; the final arrival stays.
	r.clock.Set(t0.Add(900 * ms))
	r.m.onPrivateEvent(PrivateEvent{Order: ptr(report(bull, StatusFilled, "10", "404.5", "0.0262925", true, venueT.Add(14*ms)))})
	if rec := r.order(t, bull.ClientOrderID); !rec.FeeKnown || !rec.FinalAt.Equal(t0.Add(31*ms)) {
		t.Errorf("after the fee: fee known %t, the final arrival at +%s; want it left at +31ms", rec.FeeKnown, rec.FinalAt.Sub(t0))
	}
	rows := r.orderRows(bull.ClientOrderID)
	if row := rows[len(rows)-1]; row.SentMs != t0.UnixMilli() || row.AckedMs != t0.Add(24*ms).UnixMilli() || row.AcceptedMs != venueT.Add(9*ms).UnixMilli() ||
		row.ExchangeCreatedMs != venueT.Add(12*ms).UnixMilli() || row.ExchangeUpdatedMs != venueT.Add(14*ms).UnixMilli() || row.FinalMs != t0.Add(31*ms).UnixMilli() {
		t.Errorf("the last row = %+v", row)
	}

	// Over REST the acceptance has no creation time: the first report
	// brings it. A refusal is final when its answer arrives.
	r.clock.Set(t0)
	bear := orderReq(2, Bear, PurposeEntry, SideBuy, "49.72", "8")
	r.placeAndWait(t, bear)
	if rec := r.order(t, bear.ClientOrderID); !rec.ExchangeCreatedAt.IsZero() || !rec.AcceptedAt.Equal(venueT.Add(9*ms)) {
		t.Fatalf("an acceptance without a creation time: %+v", rec)
	}
	cancelled := report(bear, StatusCancelled, "0", "0", "0", false, venueT.Add(15*ms))
	cancelled.ExchangeCreatedAt = venueT.Add(13 * ms)
	r.m.onPrivateEvent(PrivateEvent{Order: &cancelled})
	if rec := r.order(t, bear.ClientOrderID); !rec.ExchangeCreatedAt.Equal(venueT.Add(13*ms)) || !rec.FinalAt.Equal(t0.Add(24*ms)) {
		t.Errorf("after the report: created %s, final %s", rec.ExchangeCreatedAt.Sub(venueT), rec.FinalAt.Sub(t0))
	}
	r.venue.place = func(OrderRequest) (OrderAck, error) {
		r.clock.Set(t0.Add(40 * ms))
		return OrderAck{}, &VenueError{Code: "25203", Message: "Insufficient margin", Kind: ErrorOther}
	}
	refused := orderReq(3, Bull, PurposeEntry, SideBuy, "40.47", "10")
	r.placeAndWait(t, refused)
	if rec := r.order(t, refused.ClientOrderID); rec.Status != StatusRejected || !rec.FinalAt.Equal(t0.Add(40*ms)) || !rec.AckedAt.IsZero() {
		t.Errorf("the refused order: %+v, want final when the refusal arrived", rec)
	}
}

// The push with the final state arrives before the acceptance, as it did
// for three of the four orders of the first live round trip. The order is
// final from the push, and the acceptance still brings its two times: in
// hand locally, and taken by the venue's gateway.
func TestOrderAcceptanceAfterTheFinalPushKeepsItsTimes(t *testing.T) {
	r := newRig(t, testConfig(), t0)
	ms := time.Millisecond
	venueT := t0.Add(-800 * ms)
	req := orderReq(1, Bull, PurposeEntry, SideBuy, "40.47", "10")
	r.venue.place = func(req OrderRequest) (OrderAck, error) {
		r.clock.Set(t0.Add(13 * ms))
		r.m.onPrivateEvent(PrivateEvent{Order: ptr(report(req, StatusFilled, "10", "404.5", "0.0262925", true, venueT.Add(14*ms)))})
		r.clock.Set(t0.Add(27 * ms))
		return OrderAck{OrderID: OrderID("v-" + string(req.ClientOrderID)), AcceptedAt: venueT.Add(9 * ms)}, nil
	}
	if err := r.m.PlaceOrders(req); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the acceptance", func() bool { return !r.order(t, req.ClientOrderID).SentAt.IsZero() })
	rec := r.order(t, req.ClientOrderID)
	if rec.Status != StatusFilled || !rec.FinalAt.Equal(t0.Add(13*ms)) || !rec.AckedAt.Equal(t0.Add(27*ms)) || !rec.AcceptedAt.Equal(venueT.Add(9*ms)) {
		t.Fatalf("status %s, final +%s, acked +%s, accepted %s; want filled, +13ms, +27ms and the gateway's time", rec.Status, rec.FinalAt.Sub(t0), rec.AckedAt.Sub(t0), rec.AcceptedAt)
	}
	rows := r.orderRows(req.ClientOrderID)
	if row := rows[len(rows)-1]; row.AckedMs != t0.Add(27*ms).UnixMilli() || row.AcceptedMs != venueT.Add(9*ms).UnixMilli() {
		t.Errorf("the last row = %+v, want the acceptance's two times", row)
	}
}

// Reports are merged, not copied. A repeated report changes nothing: no new
// UpdateSeq, no row, no notification. A report older than what is held - a
// smaller filled quantity, a state that is not final after a final one, a
// different final state - moves nothing back.
func TestOrderDuplicateAndOutOfDateReports(t *testing.T) {
	r := newRig(t, testConfig(), t0)
	r.venue.place = accept
	req := orderReq(1, Bull, PurposeEntry, SideBuy, "40.47", "10")
	r.placeAndWait(t, req)
	partial := report(req, StatusAcked, "4", "161.8", "0.010517", true, t0.Add(10*time.Millisecond)) // partially filled, still working
	filled := report(req, StatusFilled, "10", "404.5", "0.0262925", true, t0.Add(20*time.Millisecond))

	var seqs []uint64
	push := func(in OrderRecord) (changed bool) {
		ch, rows := r.m.OrdersChanged(), len(r.orderRows(req.ClientOrderID))
		r.m.onPrivateEvent(PrivateEvent{Order: &in})
		rec := r.order(t, req.ClientOrderID)
		changed = changedSince(ch)
		if grew := len(r.orderRows(req.ClientOrderID)) > rows; grew != changed {
			t.Fatalf("a row was written: %t, the readers were notified: %t", grew, changed)
		}
		if changed {
			seqs = append(seqs, rec.UpdateSeq)
		}
		return changed
	}
	if !push(partial) {
		t.Fatal("the first partial fill changed nothing")
	}
	if rec := r.order(t, req.ClientOrderID); rec.Status != StatusAcked || !rec.CumExecQty.Equal(dec("4")) || !rec.Fee.Equal(dec("0.010517")) {
		t.Fatalf("after the partial fill: %+v", rec)
	}
	if push(partial) {
		t.Error("the same partial fill again changed the record")
	}
	if !push(filled) {
		t.Fatal("the final report changed nothing")
	}
	want := r.order(t, req.ClientOrderID)
	for name, stale := range map[string]OrderRecord{
		"the same final report":                filled,
		"the partial fill, late":               partial,
		"a cancelled report with less filled":  report(req, StatusCancelled, "4", "161.8", "0.010517", true, t0.Add(15*time.Millisecond)),
		"a cancelled report with the same qty": report(req, StatusCancelled, "10", "404.5", "0.0262925", true, t0.Add(25*time.Millisecond)),
	} {
		if push(stale) {
			t.Errorf("%s changed the record", name)
		}
		if got := r.order(t, req.ClientOrderID); got.Status != StatusFilled || !got.CumExecQty.Equal(dec("10")) || !got.CumExecValue.Equal(dec("404.5")) ||
			!got.Fee.Equal(dec("0.0262925")) || got.UpdateSeq != want.UpdateSeq || !got.ExchangeUpdatedAt.Equal(want.ExchangeUpdatedAt) {
			t.Errorf("%s: the record is now %+v", name, got)
		}
	}
	if len(seqs) != 2 || seqs[0] >= seqs[1] || seqs[0] <= 2 {
		t.Errorf("UpdateSeq of the two changes = %v, want two growing numbers after the registration's and the ack's", seqs)
	}
	// A report for an order the table does not hold is ignored.
	r.m.onPrivateEvent(PrivateEvent{Order: ptr(report(orderReq(9, Bull, PurposeEntry, SideBuy, "1", "1"), StatusFilled, "1", "1", "0", true, t0))})
	if n := len(r.m.CycleOrders(testCycleID)); n != 1 {
		t.Errorf("%d orders in the table after a report of an unknown one", n)
	}
}

// The rows are committed before anything is sent. When the commit fails, a
// group holding an order that is not reduce-only is not sent at all: its
// orders end as not_sent and the caller gets the error. A group of
// reduce-only orders is sent all the same.
func TestPlaceOrdersWhenTheRowsCannotBeWritten(t *testing.T) {
	r := newRig(t, testConfig(), t0)
	r.venue.place = accept
	r.sink.Fail(errors.New("store: cannot reach the database"))

	entry, trim := orderReq(1, Bull, PurposeEntry, SideBuy, "40.47", "10"), orderReq(2, Bear, PurposeTrim, SideSell, "49.60", "2")
	if err := r.m.PlaceOrders(entry, trim); err == nil {
		t.Fatal("a group with an entry order was placed without its rows")
	}
	time.Sleep(20 * time.Millisecond) // nothing may be on its way
	if sent := r.venue.placedIDs(); len(sent) != 0 {
		t.Fatalf("the venue received %v", sent)
	}
	for _, req := range []OrderRequest{entry, trim} {
		rec := r.order(t, req.ClientOrderID)
		if rec.Status != StatusNotSent || rec.Unknown || !rec.SentAt.IsZero() {
			t.Errorf("%s: %+v, want not_sent", req.ClientOrderID, rec)
		}
		if rows := r.orderRows(req.ClientOrderID); len(rows) != 1 || rows[0].Status != "not_sent" {
			t.Errorf("%s: rows %+v, want the one row that voids it", req.ClientOrderID, rows)
		}
	}

	// Reducing a position does not wait for the database.
	exitBull, exitBear := orderReq(3, Bull, PurposeForced, SideSell, "40.30", "10"), orderReq(4, Bear, PurposeForced, SideSell, "49.50", "8")
	r.placeAndWait(t, exitBull, exitBear)
	sent := r.venue.placedIDs()
	slices.Sort(sent)
	if !slices.Equal(sent, []ClientOrderID{exitBull.ClientOrderID, exitBear.ClientOrderID}) {
		t.Fatalf("the venue received %v, want the two reduce-only orders", sent)
	}
	if rec := r.order(t, exitBull.ClientOrderID); rec.Status != StatusAcked {
		t.Errorf("the reduce-only order: %+v", rec)
	}
}

// Fills of the two contracts are written as they arrive - one that arrives
// twice is written twice, the table's key keeps one - and those of other
// contracts are not; a position push is published.
func TestPrivateFillsAndPositions(t *testing.T) {
	r := newRig(t, testConfig(), t0)
	fill := Fill{ExecID: "e1", OrderID: "v1", ClientOrderID: "etfp1795406754673-0001BE", Symbol: "SOXLUSDT", Side: SideBuy, Price: dec("40.46"), Qty: dec("5"), Value: dec("202.3"), Fee: dec("0.0131495"),
		ExecTime: t0.Add(10 * time.Millisecond), ReceivedAt: t0.Add(15 * time.Millisecond)}
	second, other := fill, fill
	second.ExecID, second.Price = "e2", dec("40.47")
	other.ExecID, other.Symbol = "e3", "BTCUSDT"
	for _, f := range []Fill{fill, fill, second, other} {
		r.m.onPrivateEvent(PrivateEvent{Fill: &f})
	}
	rows := r.sink.Rows("cex_fill")
	if len(rows) != 3 || rows[1].(store.FillRow).ExecID != "e1" || rows[2].(store.FillRow).ExecID != "e2" {
		t.Fatalf("fill rows = %+v, want e1 as often as it arrived, then e2, and nothing of the other contract", rows)
	}
	if row := rows[0].(store.FillRow); row.ExecID != "e1" || row.ClientOrderID != "etfp1795406754673-0001BE" || !row.Value.Equal(dec("202.3")) || !row.Fee.Equal(dec("0.0131495")) ||
		row.ExecMs != t0.Add(10*time.Millisecond).UnixMilli() || row.ReceivedMs != t0.Add(15*time.Millisecond).UnixMilli() {
		t.Errorf("fill row = %+v", row)
	}

	changed := r.m.AccountChanged()
	r.m.onPrivateEvent(PrivateEvent{Positions: &PositionSet{BySymbol: map[Symbol]Position{"SOXLUSDT": {Symbol: "SOXLUSDT", Qty: dec("5")}, "BTCUSDT": {Symbol: "BTCUSDT", Qty: dec("-1")}}}})
	if p := r.m.Positions(); !p.Loaded || len(p.BySymbol) != 1 || !p.BySymbol["SOXLUSDT"].Qty.Equal(dec("5")) || !changedSince(changed) {
		t.Errorf("positions after the push = %+v", p)
	}
}

// At a restart the stored orders go back into the table. One without a
// final state becomes unknown and is queried; finished ones stay as they
// are. Then the venue's orders since the unfinished cycle was created are
// read: a known order is merged, an order of that cycle that the table does
// not hold is recognised from its id, and its lot is stored once. Orders of
// other cycles and of other programs are left alone.
func TestRestoreAndRecoverOrders(t *testing.T) {
	// What the previous run stored: a filled entry and an entry it never
	// heard back about.
	done, open := orderReq(1, Bull, PurposeEntry, SideBuy, "40.47", "10"), orderReq(2, Bear, PurposeEntry, SideBuy, "49.72", "8")
	stored := []store.OrderRow{
		orderRow(OrderRecord{Request: done, OrderID: "v1", Status: StatusFilled, CumExecQty: dec("10"), CumExecValue: dec("404.5"), AvgPrice: dec("40.45"), Fee: dec("0.0262925"), FeeKnown: true,
			SubmittedAt: t0.Add(-time.Minute), SentAt: t0.Add(-time.Minute), AckedAt: t0.Add(-time.Minute), ExchangeUpdatedAt: t0.Add(-time.Minute), UpdateReceivedAt: t0.Add(-time.Minute), UpdateSeq: 77}),
		orderRow(OrderRecord{Request: open, Status: StatusPending, SubmittedAt: t0.Add(-time.Minute), UpdateSeq: 78}),
	}
	restart := t0
	r := newRig(t, testConfig(), restart)
	r.m.Restore(nil, OrderRecordsFromRows(stored))

	got := r.m.CycleOrders(testCycleID)
	if len(got) != 2 || fmt.Sprint(got[0].Request) != fmt.Sprint(done) || fmt.Sprint(got[1].Request) != fmt.Sprint(open) {
		t.Fatalf("restored requests = %+v", got)
	}
	if got[0].Status != StatusFilled || got[0].Unknown || !got[0].Fee.Equal(dec("0.0262925")) || !got[0].FeeKnown || !got[0].ExchangeUpdatedAt.Equal(t0.Add(-time.Minute)) || !got[0].SubmittedAt.Equal(t0.Add(-time.Minute)) {
		t.Errorf("the finished order after the restart: %+v", got[0])
	}
	if got[1].Status != StatusPending || !got[1].Unknown || !got[1].UnknownSince.Equal(restart) {
		t.Errorf("the open order after the restart: %+v, want unknown since the restart", got[1])
	}
	if got[0].UpdateSeq == 0 || got[1].UpdateSeq == 0 || got[0].UpdateSeq == got[1].UpdateSeq {
		t.Errorf("UpdateSeq after the restart = %d and %d, want this process's own", got[0].UpdateSeq, got[1].UpdateSeq)
	}
	if rows := r.orderRows(open.ClientOrderID); len(rows) != 1 || !rows[0].Unknown || len(r.orderRows(done.ClientOrderID)) != 0 {
		t.Errorf("rows written by the restore: %+v; want only the order that became unknown", rows)
	}
	r.stepOrdersAt(restart)
	if n := r.venue.callCount("OrderByClientID"); n != 1 {
		t.Fatalf("%d queries after the restart, want one for the open order", n)
	}

	// The venue's recent orders: the open order, filled; two orders the
	// process had sent and not recorded, a completion on the bear leg and a
	// trim on the bull leg; an order of another program; and orders of an
	// earlier and of a later cycle.
	recognised, recognisedBull := NewClientOrderID(cycleAt, 3, Bear, PurposeComplete), NewClientOrderID(cycleAt, 4, Bull, PurposeTrim)
	earlier, later := NewClientOrderID(cycleAt.Add(-time.Hour), 1, Bear, PurposeEntry), NewClientOrderID(cycleAt.Add(time.Minute), 1, Bear, PurposeEntry)
	venueOrder := func(id ClientOrderID, symbol Symbol, qty, value, fee string) OrderRecord {
		return OrderRecord{Request: OrderRequest{ClientOrderID: id, Symbol: symbol, Side: SideBuy, Price: dec("49.72"), Qty: dec(qty)}, OrderID: OrderID("v-" + string(id)), Status: StatusFilled,
			CumExecQty: dec(qty), CumExecValue: dec(value), AvgPrice: dec(value).Div(dec(qty)), Fee: dec(fee), FeeKnown: true, SubmittedAt: t0.Add(-50 * time.Second), ExchangeUpdatedAt: t0.Add(-50 * time.Second)}
	}
	r.venue.recent["SOXSUSDT"] = []OrderRecord{venueOrder(open.ClientOrderID, "SOXSUSDT", "8", "397.6", "0.025844"), venueOrder(recognised, "SOXSUSDT", "1", "49.7", "0.0032305"),
		venueOrder("ep1795406754673-0001RE", "SOXSUSDT", "1", "49.7", "0"), venueOrder(earlier, "SOXSUSDT", "1", "49.7", "0"), venueOrder(later, "SOXSUSDT", "1", "49.7", "0")}
	r.venue.recent["SOXLUSDT"] = []OrderRecord{venueOrder(recognisedBull, "SOXLUSDT", "2", "80.9", "0.0052585")}

	// The bull contract is read, the bear contract's read fails: an error,
	// and nothing is booked - not the bull order either, or the retry
	// would no longer return it and its lot would never be stored.
	r.venue.recentErr = map[Symbol]error{"SOXSUSDT": errors.New("http 502")}
	if added, err := r.m.RecoverOrders(context.Background(), testCycleID); err == nil || len(added) != 0 {
		t.Fatalf("one contract's read failed: recognised %+v, %v; want the error", added, err)
	}
	if got := r.m.CycleOrders(testCycleID); len(got) != 2 || got[1].Status != StatusPending || len(r.orderRows(recognisedBull)) != 0 {
		t.Fatalf("after the failed read the table holds %+v; want it unchanged", got)
	}
	r.venue.recentErr = nil
	added, err := r.m.RecoverOrders(context.Background(), testCycleID)
	if err != nil {
		t.Fatal(err)
	}
	for _, since := range r.venue.recentSince {
		if !since.Equal(cycleAt) {
			t.Errorf("the venue's orders were read since %s, want since the cycle's creation %s", since, cycleAt)
		}
	}
	if len(added) != 2 || added[0].Request.ClientOrderID != recognisedBull || added[1].Request.ClientOrderID != recognised {
		t.Fatalf("recognised = %+v, want the bull trim and the bear completion", added)
	}
	if q := added[0].Request; q.CycleID != testCycleID || q.Leg != Bull || q.Purpose != PurposeTrim || q.Symbol != "SOXLUSDT" {
		t.Errorf("the recognised bull order = %+v", added[0])
	}
	added = added[1:]
	q := added[0].Request
	if q.CycleID != testCycleID || q.Leg != Bear || q.Purpose != PurposeComplete || q.Symbol != "SOXSUSDT" || q.Side != SideBuy || q.LotID != "" || !q.Qty.Equal(dec("1")) ||
		added[0].Status != StatusFilled || !added[0].CumExecValue.Equal(dec("49.7")) || !added[0].SubmittedAt.Equal(t0.Add(-50*time.Second)) || added[0].UpdateSeq == 0 {
		t.Errorf("the recognised order = %+v", added[0])
	}
	got = r.m.CycleOrders(testCycleID)
	if len(got) != 4 || got[1].Status != StatusFilled || got[1].Unknown || !got[1].CumExecValue.Equal(dec("397.6")) || got[2].Request.ClientOrderID != recognised || got[3].Request.ClientOrderID != recognisedBull {
		t.Fatalf("the table after the recovery = %+v", got)
	}
	if len(r.m.UnresolvedOrders()) != 0 {
		t.Error("an order is still unresolved after the venue reported both")
	}
	for _, id := range []ClientOrderID{earlier, later, "ep1795406754673-0001RE"} {
		if len(r.orderRows(id)) != 0 {
			t.Errorf("a row was written for %s, an order that is not of the unfinished cycle", id)
		}
	}
	// Reading the same orders again recognises nothing new.
	if again, err := r.m.RecoverOrders(context.Background(), testCycleID); err != nil || len(again) != 0 {
		t.Errorf("a second recovery recognised %+v, %v", again, err)
	}

	// The ledger's assignment is stored on the recognised order, once.
	r.m.SetOrderLot(recognised, testCycleID+"-1")
	r.m.SetOrderLot(recognised, testCycleID+"-2")
	r.m.SetOrderLot(done.ClientOrderID, testCycleID+"-2")
	if rec := r.order(t, recognised); rec.Request.LotID != testCycleID+"-1" || rec.UpdateSeq <= added[0].UpdateSeq {
		t.Errorf("the recognised order's lot = %q (seq %d after %d), want the first assignment and a new seq", rec.Request.LotID, rec.UpdateSeq, added[0].UpdateSeq)
	}
	if rec := r.order(t, done.ClientOrderID); rec.Request.LotID != testCycleID+"-1" {
		t.Errorf("an order sent with a lot was moved to %q", rec.Request.LotID)
	}
	rows := r.orderRows(recognised)
	if len(rows) != 2 || rows[0].LotID != "" || rows[1].LotID != testCycleID+"-1" || rows[1].Purpose != "complete" || rows[1].Leg != "bear" || rows[1].CycleID != testCycleID {
		t.Errorf("the recognised order's rows = %+v", rows)
	}
}

// Without an unfinished cycle nothing is recovered. The table is empty
// then, and the venue still lists the orders of the cycles that finished:
// their rows in the database are complete - lot, quota price, the local
// times - and a row written from the venue's report would replace them.
// No call can make that happen: there is no cycle to name.
func TestRecoverOrdersLeavesFinishedCyclesAlone(t *testing.T) {
	r := newRig(t, testConfig(), t0)
	finished := orderReq(1, Bear, PurposeExit, SideSell, "49.60", "8")
	r.venue.recent["SOXSUSDT"] = []OrderRecord{{Request: OrderRequest{ClientOrderID: finished.ClientOrderID, Symbol: "SOXSUSDT", Side: SideSell, Price: dec("49.60"), Qty: dec("8")}, OrderID: "v1",
		Status: StatusFilled, CumExecQty: dec("8"), CumExecValue: dec("397.6"), AvgPrice: dec("49.7"), Fee: dec("0.025844"), FeeKnown: true, SubmittedAt: cycleAt.Add(time.Second)}}
	for _, cycleID := range []string{"", "c", "1795406754673", "c1795403154673"} { // no cycle, malformed ids, an hour before the finished cycle
		added, err := r.m.RecoverOrders(context.Background(), cycleID)
		if len(added) != 0 || (err == nil) != (cycleID == "c1795403154673") {
			t.Errorf("RecoverOrders(%q) = %+v, %v; want nothing recognised, and an error unless the id is one", cycleID, added, err)
		}
	}
	if rows := r.sink.Rows("cex_order"); len(rows) != 0 || len(r.m.CycleOrders(testCycleID)) != 0 {
		t.Errorf("%d rows written, %d orders in the table; want the finished cycle's order left alone", len(rows), len(r.m.CycleOrders(testCycleID)))
	}
}

// An order record survives the trip through its row: everything but the
// UpdateSeq, which is this process's own, and the error kind, for which the
// table has no column.
func TestOrderRowRoundTrip(t *testing.T) {
	req := orderReq(11, Bear, PurposeExit, SideSell, "49.60", "8")
	req.QuotaPrice = dec("49.707")
	rec := OrderRecord{Request: req, OrderID: "v7", Status: StatusCancelled, Unknown: true, UnknownSince: t0.Add(2 * time.Second), FeeKnown: true, NotFound: true,
		CumExecQty: dec("3"), CumExecValue: dec("149.1"), AvgPrice: dec("49.7"), Fee: dec("0.0096915"), CancelReason: "ioc", ErrorCode: "c1", ErrorMessage: "m1",
		SubmittedAt: t0, SentAt: t0.Add(time.Millisecond), AckedAt: t0.Add(2 * time.Millisecond), ExchangeUpdatedAt: t0.Add(3 * time.Millisecond), UpdateReceivedAt: t0.Add(4 * time.Millisecond), UpdateSeq: 9,
		AcceptedAt: t0.Add(5 * time.Millisecond), ExchangeCreatedAt: t0.Add(6 * time.Millisecond), FinalAt: t0.Add(7 * time.Millisecond)}
	rec.ErrorKind = ErrorPriceLimit
	back := OrderRecordsFromRows([]store.OrderRow{orderRow(rec)})[0]
	rec.UpdateSeq, rec.ErrorKind = 0, ""
	if fmt.Sprint(back) != fmt.Sprint(rec) {
		t.Errorf("record after the round trip =\n %+v\nwant\n %+v", back, rec)
	}
}
