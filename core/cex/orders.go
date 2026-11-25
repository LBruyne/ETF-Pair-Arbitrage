package cex

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"etf-pair-arbitrage/core/logger"
	"etf-pair-arbitrage/core/store"
)

// orderStore is the table of the orders the process placed, read back at a
// restart or recognised at the venue. It has a lock of its own: PlaceOrders
// (exec's loop), each order's send goroutine, the private stream's callback
// and the order loop all change it. Every change gives the record the next
// UpdateSeq, writes its row and closes the changed channel. The notification
// carries no data: a reader takes one snapshot under the lock (CycleOrders)
// and decides on that alone.
type orderStore struct {
	mu      sync.Mutex
	orders  map[ClientOrderID]*orderEntry
	seq     uint64        // the UpdateSeq last given; one counter for the table, so each order's sequence only grows
	changed chan struct{} // closed at the next change of any record

	queryFailing bool // the last order query failed for a reason other than "no such order"; the order loop's own
}

// orderEntry is one order of the table.
type orderEntry struct {
	rec       OrderRecord
	queriedAt time.Time // when the order was last queried at the venue
	fillSeen  bool      // the venue said "no such order" while a fill carries its id: it exists, and the fills are not read for it again
}

func newOrderStore() *orderStore {
	return &orderStore{orders: map[ClientOrderID]*orderEntry{}, changed: make(chan struct{})}
}

// --- client order ids ---

// clientOrderPrefix starts every order id of this program, so that it knows
// its own orders among the account's.
const clientOrderPrefix = "etfp"

// The letters an order id ends with: its leg and its purpose.
var (
	legLetters     = [2]byte{Bull: 'B', Bear: 'R'}
	purposeLetters = map[OrderPurpose]byte{PurposeEntry: 'E', PurposeComplete: 'C', PurposeTrim: 'T', PurposeExit: 'X', PurposeForced: 'F'}
)

// NewClientOrderID makes the id of the seq-th order of the cycle created at
// cycleAt: the prefix, the cycle's creation in unix milliseconds, a dash,
// the sequence number as four base-36 digits, the leg and the purpose, such
// as etfp1795406754673-0007BE. The ids of one cycle sort in the order of
// their sequence numbers, and an id alone says which cycle, leg and purpose
// an order found at the venue belongs to.
func NewClientOrderID(cycleAt time.Time, seq int, leg Leg, purpose OrderPurpose) ClientOrderID {
	digits := strings.ToUpper(strconv.FormatInt(int64(seq), 36))
	return ClientOrderID(fmt.Sprintf("%s%d-%04s%c%c", clientOrderPrefix, cycleAt.UnixMilli(), digits, legLetters[leg], purposeLetters[purpose]))
}

// parseClientOrderID reads an id made by NewClientOrderID: the cycle it
// names ("c" and the cycle's creation in unix milliseconds, as the ledger
// names its cycles), the leg and the purpose. ok is false for any other id.
func parseClientOrderID(id ClientOrderID) (cycleID string, leg Leg, purpose OrderPurpose, ok bool) {
	rest, found := strings.CutPrefix(string(id), clientOrderPrefix)
	created, tail, dash := strings.Cut(rest, "-")
	if _, err := strconv.ParseInt(created, 10, 64); !found || !dash || err != nil || len(tail) != 6 {
		return "", 0, "", false
	}
	switch tail[4] {
	case legLetters[Bull]:
		leg = Bull
	case legLetters[Bear]:
		leg = Bear
	default:
		return "", 0, "", false
	}
	for p, letter := range purposeLetters {
		if letter == tail[5] {
			return "c" + created, leg, p, true
		}
	}
	return "", 0, "", false
}

// --- placing ---

// PlaceOrders registers the orders as pending, commits their cex_order rows,
// and then sends each on a goroutine of its own. When the rows cannot be
// confirmed written, a group that holds an order that is not reduce-only is
// not sent: its orders end as not_sent and the error is returned. A group of
// reduce-only orders is sent all the same, because reducing a position must
// not wait for the database, and nil is returned. The outcome of each order
// is read from the table afterwards.
func (m *CexManager) PlaceOrders(reqs ...OrderRequest) error {
	log := logger.GlobalLogger
	now := m.timing.clock()
	t := m.orders

	// Register.
	t.mu.Lock()
	rows := make([]store.Row, 0, len(reqs))
	reduceOnly := true
	for _, req := range reqs {
		e := &orderEntry{rec: OrderRecord{Request: req, Status: StatusPending, SubmittedAt: now}}
		t.seq++
		e.rec.UpdateSeq = t.seq
		t.orders[req.ClientOrderID] = e
		rows = append(rows, orderRow(e.rec))
		reduceOnly = reduceOnly && req.ReduceOnly
		log.Info("cex: order registered", "client_order_id", req.ClientOrderID, "cycle", req.CycleID, "lot", req.LotID, "purpose", string(req.Purpose),
			"symbol", req.Symbol, "side", string(req.Side), "price", req.Price, "qty", req.Qty, "reduce_only", req.ReduceOnly)
	}
	t.notify()
	t.mu.Unlock()

	// Commit the rows before anything is sent.
	if m.store != nil {
		if err := m.store.WriteSync(rows...); err != nil {
			if !reduceOnly {
				for _, req := range reqs {
					m.editOrder(req.ClientOrderID, func(r *OrderRecord, _ time.Time) bool {
						r.Status, r.ErrorMessage = StatusNotSent, "the order's row was not confirmed written"
						return true
					})
				}
				return fmt.Errorf("cex: %d orders not sent: their rows were not confirmed written: %w", len(reqs), err)
			}
			log.Warn("cex: order rows not confirmed written; reduce-only orders are sent all the same", "orders", len(reqs), "err", err)
		}
	}
	for _, req := range reqs {
		go m.sendOrder(req)
	}
	return nil
}

// sendOrder sends one registered order and books the venue's answer. An
// acceptance only makes it acked: the outcome arrives by the private stream
// or a query. Only a stated answer ends the order here: ErrNotSent (nothing
// left the process) and a refusal by the venue, kept with its code, text and
// kind. A timeout, a lost connection, the adapter's ErrOrderStateUnknown or
// any other failure leaves the outcome unknown, with the error's text on
// the record; the order is then queried and never sent again.
func (m *CexManager) sendOrder(req OrderRequest) {
	ctx, cancel := context.WithTimeout(context.Background(), m.timing.placeTimeout)
	defer cancel()
	sentAt := m.timing.clock()
	ack, err := m.venue.PlaceOrder(ctx, req)
	m.editOrder(req.ClientOrderID, func(r *OrderRecord, now time.Time) bool {
		r.SentAt = sentAt
		if err == nil {
			// The acceptance's times count also when a push came first.
			if r.AckedAt.IsZero() {
				r.AckedAt = now
			}
			if r.AcceptedAt.IsZero() {
				r.AcceptedAt = ack.AcceptedAt
			}
		}
		if r.Status.Terminal() {
			return true // the push with the outcome arrived before this answer
		}
		var refusal *VenueError
		switch {
		case err == nil:
			r.Status = StatusAcked
			if r.OrderID == "" {
				r.OrderID = ack.OrderID
			}
			if r.ExchangeCreatedAt.IsZero() {
				r.ExchangeCreatedAt = ack.CreatedAt
			}
		case errors.Is(err, ErrNotSent):
			r.Status, r.ErrorMessage = StatusNotSent, err.Error()
		case errors.As(err, &refusal) && !errors.Is(err, ErrOrderStateUnknown):
			r.Status, r.ErrorCode, r.ErrorMessage, r.ErrorKind = StatusRejected, refusal.Code, refusal.Message, refusal.Kind
		default:
			r.ErrorMessage = err.Error()
			markUnknown(r, now)
		}
		return true
	})
}

// markUnknown flags an order whose outcome is not known; the time of the
// first flag is kept.
func markUnknown(r *OrderRecord, now time.Time) {
	if !r.Unknown {
		r.Unknown, r.UnknownSince = true, now
	}
}

// --- changes ---

// editOrder applies change to the order's record under the table's lock.
// When change reports that it changed the record, the record gets the next
// UpdateSeq, its row is written and the readers are notified. An id the
// table does not hold is ignored.
func (m *CexManager) editOrder(id ClientOrderID, change func(r *OrderRecord, now time.Time) bool) {
	t := m.orders
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.orders[id]
	if !ok {
		return
	}
	before := e.rec
	if change(&e.rec, m.timing.clock()) {
		m.commitOrder(e, before)
	}
}

// commitOrder finishes a change of e's record: the next UpdateSeq, the time
// its final state arrived, the row, the log lines of a new status or a newly
// unknown outcome, and the notification. The caller holds the table's lock,
// so the rows of one order reach the store in the order of its changes.
func (m *CexManager) commitOrder(e *orderEntry, before OrderRecord) {
	t, r, log := m.orders, &e.rec, logger.GlobalLogger
	t.seq++
	r.UpdateSeq = t.seq
	if r.Status.Terminal() && r.FinalAt.IsZero() {
		r.FinalAt = m.timing.clock()
	}
	if m.store != nil {
		m.store.Record(orderRow(*r))
	}
	if r.Status != before.Status {
		log.Info("cex: order status changed", "client_order_id", r.Request.ClientOrderID, "from", string(before.Status), "to", string(r.Status),
			"cum_exec_qty", r.CumExecQty, "cum_exec_value_usdt", r.CumExecValue, "fee_usdt", r.Fee, "fee_known", r.FeeKnown, "error_code", r.ErrorCode)
	}
	if r.Unknown && !before.Unknown {
		log.Warn("cex: order outcome unknown; querying", "client_order_id", r.Request.ClientOrderID, "status", string(r.Status), "err", r.ErrorMessage)
	}
	t.notify()
}

// notify closes the changed channel and opens the next. The caller holds
// the lock.
func (t *orderStore) notify() {
	close(t.changed)
	t.changed = make(chan struct{})
}

// mergeReport books what the venue says about an order, from a push of the
// private stream or from a query. The filled quantity only grows: a report
// with less is out of date and changes nothing of it. A final state, once
// held, is not replaced; after it only a larger filled quantity and the fee
// are taken. The fee is known only for the quantity it was reported with:
// when the quantity grows, the fee held is no longer the whole fee, so
// FeeKnown falls back to false until a report carries the fee of the new
// quantity - this one, or a later push or query. A report with a fill also
// shows that the venue has the order, which ends NotFound. A report that
// changes nothing is dropped without a new UpdateSeq.
func (m *CexManager) mergeReport(in OrderRecord) {
	m.editOrder(in.Request.ClientOrderID, func(r *OrderRecord, now time.Time) bool {
		changed := false
		if r.OrderID == "" && in.OrderID != "" {
			r.OrderID, changed = in.OrderID, true
		}
		if r.ExchangeCreatedAt.IsZero() && !in.ExchangeCreatedAt.IsZero() {
			r.ExchangeCreatedAt, changed = in.ExchangeCreatedAt, true
		}
		if in.CumExecQty.GreaterThan(r.CumExecQty) {
			r.CumExecQty, r.CumExecValue, r.AvgPrice, changed = in.CumExecQty, in.CumExecValue, in.AvgPrice, true
			r.FeeKnown, r.NotFound = false, false // the fee held covers the smaller quantity; this report's is taken below when it has one
		}
		switch {
		case r.Status.Terminal():
		case in.Status.Terminal():
			r.Status, r.CancelReason, r.ErrorCode, r.ErrorMessage, r.ErrorKind = in.Status, in.CancelReason, in.ErrorCode, in.ErrorMessage, in.ErrorKind
			r.Unknown, r.NotFound, changed = false, false, true
		case r.Status == StatusPending && in.Status == StatusAcked:
			r.Status, r.AckedAt, changed = StatusAcked, now, true
		}
		if in.FeeKnown && in.CumExecQty.Equal(r.CumExecQty) && (!r.FeeKnown || !in.Fee.Equal(r.Fee)) {
			r.Fee, r.FeeKnown, changed = in.Fee, true, true
		}
		if changed {
			r.UpdateReceivedAt = now
			if in.ExchangeUpdatedAt.After(r.ExchangeUpdatedAt) {
				r.ExchangeUpdatedAt = in.ExchangeUpdatedAt
			}
		}
		return changed
	})
}

// SetOrderLot stores the lot an order is booked on, once: on a record that
// carries none. The recovery calls it for every order it recognised at the
// venue, with the ledger's assignment, so that cex_order.lot_id holds it and
// a later start books the order on the same lot.
func (m *CexManager) SetOrderLot(id ClientOrderID, lotID string) {
	m.editOrder(id, func(r *OrderRecord, _ time.Time) bool {
		if r.Request.LotID != "" || lotID == "" {
			return false
		}
		r.Request.LotID = lotID
		logger.GlobalLogger.Info("cex: order lot stored", "client_order_id", id, "lot", lotID)
		return true
	})
}

// MarkNeverArrived ends an order whose outcome is unknown as never_arrived.
// It is exec's verdict, given once the three evidences are in: the venue
// does not have the order, no fill carries its id (NotFound), and the leg's
// position equals the ledger.
func (m *CexManager) MarkNeverArrived(id ClientOrderID) {
	m.editOrder(id, func(r *OrderRecord, _ time.Time) bool {
		if r.Status.Terminal() {
			return false
		}
		r.Status, r.Unknown = StatusNeverArrived, false
		return true
	})
}

// --- readers ---

// OrdersChanged returns a channel closed at the next change of any order
// record. A reader takes the channel first, then the snapshot, then waits on
// the channel.
func (m *CexManager) OrdersChanged() <-chan struct{} {
	m.orders.mu.Lock()
	defer m.orders.mu.Unlock()
	return m.orders.changed
}

// CycleOrders returns one consistent snapshot of the cycle's orders, in the
// order they were sent (their ids sort that way).
func (m *CexManager) CycleOrders(cycleID string) []OrderRecord {
	return m.ordersWhere(func(r *OrderRecord) bool { return r.Request.CycleID == cycleID })
}

// UnresolvedOrders returns the orders without a final state, of any cycle.
func (m *CexManager) UnresolvedOrders() []OrderRecord {
	return m.ordersWhere(func(r *OrderRecord) bool { return !r.Status.Terminal() })
}

// ordersWhere copies the records keep accepts, under the lock, sorted by id.
func (m *CexManager) ordersWhere(keep func(r *OrderRecord) bool) []OrderRecord {
	t := m.orders
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []OrderRecord
	for _, e := range t.orders {
		if keep(&e.rec) {
			out = append(out, e.rec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Request.ClientOrderID < out[j].Request.ClientOrderID })
	return out
}

// --- restart ---

// restoreOrders takes the order records read back at a restart into the
// table, before Run. An order without a final state becomes unknown: the
// process that sent it is gone, so its outcome is asked of the venue.
func (m *CexManager) restoreOrders(orders []OrderRecord) {
	t := m.orders
	now := m.timing.clock()
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, rec := range orders {
		e := &orderEntry{rec: rec}
		t.orders[rec.Request.ClientOrderID] = e
		if !rec.Status.Terminal() && !rec.Unknown {
			markUnknown(&e.rec, now)
			m.commitOrder(e, rec)
			continue
		}
		t.seq++
		e.rec.UpdateSeq = t.seq
	}
}

// RecoverOrders reads the venue's orders of both contracts since the
// unfinished cycle was created and books those of that cycle: an order the
// table holds is merged like any report; an order with this program's id
// that the table does not hold is recognised - its leg and purpose are read
// from its id - added, and returned. A recognised order carries no lot: the
// caller books the table's snapshot in the ledger and stores the ledger's
// assignment with SetOrderLot.
//
// cycleID is the unfinished cycle the table was restored with, and the
// function is for that case alone. Every stored order of that cycle is in
// the table, so an order of it that the table lacks has no row and writing
// one overwrites nothing. Orders of any other cycle are left alone, however
// far back the venue lists them: their cycles are finished, their rows are
// complete in the database and are not in the table, and a row written from
// the venue's report would replace them without the lot, the quota price
// and the local times. Without an unfinished cycle there is nothing to
// recover - a cycle's row is committed before its first order and a cycle
// closes only when each of its orders has a final state - and the function
// is not called.
//
// Both contracts are read before anything is booked: a failed read returns
// the error with the table unchanged, so the next call still returns every
// recognised order.
func (m *CexManager) RecoverOrders(ctx context.Context, cycleID string) ([]OrderRecord, error) {
	ms, err := strconv.ParseInt(strings.TrimPrefix(cycleID, "c"), 10, 64)
	if err != nil || !strings.HasPrefix(cycleID, "c") {
		return nil, fmt.Errorf("cex: recover orders: %q is not a cycle id", cycleID)
	}
	var read [2][]OrderRecord
	for _, leg := range Legs {
		callCtx, cancel := context.WithTimeout(ctx, m.timing.sourceCall)
		read[leg], err = m.venue.RecentOrders(callCtx, m.symbols[leg], time.UnixMilli(ms))
		cancel()
		if err != nil {
			return nil, fmt.Errorf("cex: recent orders of %s: %w", m.symbols[leg], err)
		}
	}
	var recognised []OrderRecord
	for _, leg := range Legs {
		for _, in := range read[leg] {
			if rec, added := m.recogniseOrder(in, cycleID); added {
				recognised = append(recognised, rec)
			}
		}
	}
	return recognised, nil
}

// recogniseOrder books one order read from the venue at a restart, when its
// id names the cycle; added reports that the table did not hold it and now
// does.
func (m *CexManager) recogniseOrder(in OrderRecord, cycleID string) (rec OrderRecord, added bool) {
	id := in.Request.ClientOrderID
	idCycle, leg, purpose, ours := parseClientOrderID(id)
	if !ours || idCycle != cycleID {
		return OrderRecord{}, false
	}
	t := m.orders
	t.mu.Lock()
	_, held := t.orders[id]
	if !held {
		now := m.timing.clock()
		rec = in
		rec.Request.Leg, rec.Request.Purpose, rec.Request.CycleID, rec.Request.Symbol = leg, purpose, cycleID, m.symbols[leg]
		if rec.SubmittedAt.IsZero() {
			rec.SubmittedAt = now
		}
		rec.UpdateReceivedAt = now
		if !rec.Status.Terminal() {
			markUnknown(&rec, now)
		}
		e := &orderEntry{rec: rec}
		t.orders[id] = e
		m.commitOrder(e, OrderRecord{Status: rec.Status, Unknown: rec.Unknown})
		rec = e.rec
		logger.GlobalLogger.Info("cex: order recognised at the venue", "client_order_id", id, "cycle", cycleID, "leg", leg.String(), "purpose", string(purpose),
			"status", string(rec.Status), "cum_exec_qty", rec.CumExecQty, "cum_exec_value_usdt", rec.CumExecValue)
	}
	t.mu.Unlock()
	if held {
		m.mergeReport(in)
	}
	return rec, !held
}

// --- the order loop and the private stream ---

// runOrders subscribes the private stream and, every orderTick, turns
// accepted orders without an outcome into unknown ones and queries what
// needs a query, until ctx ends. Without the stream every order's outcome
// waits for the grace period and a query, and no fill is recorded.
func (m *CexManager) runOrders(ctx context.Context) {
	if err := m.venue.SubscribePrivate(ctx, m.symbols[:], m.onPrivateEvent, func(error) {}); err != nil {
		logger.GlobalLogger.Error("cex: private stream not subscribed; order outcomes come from queries only", "err", err)
	}
	ticker := time.NewTicker(m.timing.orderTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.stepOrders(ctx, m.timing.clock())
		}
	}
}

// onPrivateEvent takes one push of the private stream on the stream's
// goroutine: an order report is merged, a fill is written, a position set
// is published.
func (m *CexManager) onPrivateEvent(ev PrivateEvent) {
	switch {
	case ev.Order != nil:
		m.mergeReport(*ev.Order)
	case ev.Fill != nil:
		m.recordFill(*ev.Fill)
	case ev.Positions != nil:
		m.publishPositions(*ev.Positions)
	}
}

// recordFill writes one fill of the two contracts to cex_fill. A fill that
// arrives twice - by push and by a read, or after a reconnect - is written
// twice: the table is keyed by the exec id and keeps one. Fills are a
// record only: quantities come from the orders' cumulative filled quantity.
func (m *CexManager) recordFill(f Fill) {
	if _, ours := m.legOf(f.Symbol); ours && m.store != nil {
		m.store.Record(fillRow(f))
	}
}

// dueOrder is one order to query: its record, and whether a fill with its
// id was already seen.
type dueOrder struct {
	rec      OrderRecord
	fillSeen bool
}

// stepOrders advances the order table to now. An order accepted ackGrace
// ago and still without a final state becomes unknown. Every unknown order,
// and every finished order with a fill whose fee is not known yet, is
// queried by its client order id, each at most once per orderQuery.
func (m *CexManager) stepOrders(ctx context.Context, now time.Time) {
	t := m.orders
	t.mu.Lock()
	var due []dueOrder
	for _, e := range t.orders {
		r := &e.rec
		if r.Status == StatusAcked && !r.Unknown && now.Sub(r.AckedAt) >= m.timing.ackGrace {
			before := *r
			markUnknown(r, now)
			m.commitOrder(e, before)
		}
		feeOpen := r.Status.Terminal() && r.CumExecQty.IsPositive() && !r.FeeKnown
		if (r.Unknown || feeOpen) && now.Sub(e.queriedAt) >= m.timing.orderQuery {
			e.queriedAt = now
			due = append(due, dueOrder{rec: *r, fillSeen: e.fillSeen})
		}
	}
	t.mu.Unlock()
	sort.Slice(due, func(i, j int) bool { return due[i].rec.Request.ClientOrderID < due[j].rec.Request.ClientOrderID })
	for _, d := range due {
		if ctx.Err() != nil {
			return
		}
		m.queryOrder(ctx, d, now)
	}
}

// queryOrder asks the venue for one order. An answer is merged like a push.
// "No such order" proves nothing at once - the venue may answer so for a
// moment after taking an order - so it counts only once the order has been
// unknown for notFoundAfter and a read of the account's fills since the
// order was registered shows none with its id: then the record is marked
// NotFound. When fills do carry its id the venue executed it after all:
// they are recorded, and the order is queried on without reading the fills
// again. Whether the order never arrived is exec's verdict
// (MarkNeverArrived); the order stays unknown and queried until then.
//
// A query that fails for another reason is tried again at the next turn;
// the first failure and the first answer after failures are logged.
func (m *CexManager) queryOrder(ctx context.Context, d dueOrder, now time.Time) {
	log := logger.GlobalLogger
	rec, t := d.rec, m.orders
	id := rec.Request.ClientOrderID
	callCtx, cancel := context.WithTimeout(ctx, m.timing.queryTimeout)
	got, err := m.venue.OrderByClientID(callCtx, rec.Request.Symbol, id)
	cancel()
	if failed := err != nil && !errors.Is(err, ErrUnknownOrder); failed != t.queryFailing {
		t.queryFailing = failed
		if failed {
			log.Warn("cex: order queries failing; retrying every turn", "client_order_id", id, "err", err)
		} else {
			log.Info("cex: order queries answered again", "client_order_id", id)
		}
	}
	switch {
	case err == nil:
		got.Request.ClientOrderID = id
		m.mergeReport(got)
	case !errors.Is(err, ErrUnknownOrder):
		log.Debug("cex: order query failed; retrying", "client_order_id", id, "err", err)
	case rec.Unknown && !rec.NotFound && !d.fillSeen && now.Sub(rec.UnknownSince) >= m.timing.notFoundAfter:
		callCtx, cancel := context.WithTimeout(ctx, m.timing.queryTimeout)
		fills, err := m.venue.Fills(callCtx, rec.SubmittedAt)
		cancel()
		if err != nil {
			log.Debug("cex: fills read failed; retrying", "client_order_id", id, "err", err)
			return
		}
		executed := 0
		for _, f := range fills {
			if f.ClientOrderID == id {
				executed++
				m.recordFill(f)
			}
		}
		if executed > 0 {
			// The venue executed it after all; a later query shows it.
			log.Warn("cex: the venue has no such order but fills carry its id; querying on", "client_order_id", id, "fills", executed)
			t.mu.Lock()
			if e, held := t.orders[id]; held {
				e.fillSeen = true
			}
			t.mu.Unlock()
			return
		}
		m.editOrder(id, func(r *OrderRecord, _ time.Time) bool {
			if !r.Unknown || r.NotFound {
				return false
			}
			r.NotFound = true
			log.Warn("cex: order not found at the venue and in its fills", "client_order_id", id, "unknown_for", now.Sub(r.UnknownSince).Round(time.Millisecond))
			return true
		})
	}
}
