package inventory

import (
	"fmt"
	"sort"
	"time"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/logger"
	"etf-pair-arbitrage/core/store"
	"etf-pair-arbitrage/core/strategy"
)

// The ledger is not booked fill by fill. Every Sync recomputes the lots of
// the open cycle from the cycle's whole order snapshot, so per-fill updates,
// final records only and a replay after a restart all give the same ledger,
// and a late, repeated or reordered report changes nothing.

// --- opening ---

// OpenCycle starts a cycle. Its row is committed before it exists in memory
// (PRD 13.5): on an error nothing is open and the caller sends nothing. The
// write may have reached the database all the same, so a row that closes the
// same cycle as EndStoreError is kept until it is confirmed written, and no
// cycle or lot is opened meanwhile (StoreBlocked).
func (m *InventoryManager) OpenCycle(sessionID string, dir strategy.Direction, pair cex.Anchors, now time.Time) (*Cycle, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cycle != nil {
		return nil, fmt.Errorf("inventory: cycle %s is still open", m.cycle.ID)
	}
	if len(m.voids) > 0 {
		return nil, ErrStoreBlocked
	}

	c := &Cycle{ID: CycleID(fmt.Sprintf("c%d", now.UnixMilli())), Status: CycleOpen, SessionID: sessionID, Direction: dir,
		PairAnchors: pair, FundingKnown: true, OpenedAt: now}
	row := cycleRow(c)
	if err := m.writeSync(row); err != nil {
		void := row
		void.Status, void.EndReason, void.ClosedMs = string(CycleClosed), EndStoreError, now.UnixMilli()
		m.voids = append(m.voids, void)
		m.voidFailure.failed(now, m.timing.failLogEvery)
		logger.GlobalLogger.Warn("inventory: cycle row not confirmed written; nothing is opened and the cycle is voided", "cycle", c.ID, "err", err)
		return nil, fmt.Errorf("inventory: cycle %s was not written: %w", c.ID, err)
	}

	m.cycle, m.cycleWritten = c, fmt.Sprint(row)
	m.orders, m.assigned, m.dropped = map[cex.ClientOrderID]cex.OrderRecord{}, map[cex.ClientOrderID]LotID{}, map[cex.ClientOrderID]decimal.Decimal{}
	m.lotWritten, m.fundingSeen = map[LotID]string{}, map[fundingKey]bool{}
	m.reserved = decimal.Zero
	logger.GlobalLogger.Info("inventory: cycle opened", "cycle", c.ID, "session", sessionID, "direction", dir,
		"pair_anchor_kind", pair.Kind, "pair_anchor_bull_usd", pair.Bull, "pair_anchor_bear_usd", pair.Bear,
		"dust_bull", m.dust[cex.Bull].Qty, "dust_bear", m.dust[cex.Bear].Qty)
	return c.clone(), nil
}

// OpenLot starts an entry batch of the open cycle, in status building. Like
// OpenCycle it commits the row first: on an error the lot does not exist,
// the batch is not sent, and a row that voids the same lot as VoidStoreError
// is kept until it is confirmed written. While a changed capital waits for
// the cycle to close it refuses with ErrCapitalPending.
func (m *InventoryManager) OpenLot(plan *strategy.EntryPlan, now time.Time) (*Lot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.cycle
	if c == nil {
		return nil, errNoCycle
	}
	if m.capitalPending() {
		return nil, ErrCapitalPending
	}
	if len(m.voids) > 0 {
		return nil, ErrStoreBlocked
	}

	seq := len(c.Lots) + 1
	lot := &Lot{ID: LotID(fmt.Sprintf("%s-%d", c.ID, seq)), CycleID: c.ID, Seq: seq, Status: LotBuilding, Direction: plan.Direction,
		OpenedAt: now, X: plan.X, Mu: plan.Mu, Sigma: plan.Sigma, FeeD: plan.FeeD, FeeUSDTEst: plan.FeeUSDT}
	row := lotRow(lot)
	if err := m.writeSync(row); err != nil {
		void := row
		void.Status, void.VoidReason, void.ClosedMs = string(LotVoid), VoidStoreError, now.UnixMilli()
		m.voids = append(m.voids, void)
		m.voidFailure.failed(now, m.timing.failLogEvery)
		logger.GlobalLogger.Warn("inventory: lot row not confirmed written; the batch is not sent and the lot is voided", "lot", lot.ID, "err", err)
		return nil, fmt.Errorf("inventory: lot %s was not written: %w", lot.ID, err)
	}

	c.Lots = append(c.Lots, lot)
	m.lotWritten[lot.ID] = fmt.Sprint(row)
	logger.GlobalLogger.Info("inventory: lot opened", "lot", lot.ID, "direction", lot.Direction,
		"plan_qty_bull", plan.Qty[cex.Bull], "plan_price_bull", plan.Price[cex.Bull], "plan_qty_bear", plan.Qty[cex.Bear], "plan_price_bear", plan.Price[cex.Bear],
		"x_d", plan.X, "mu_d", plan.Mu, "sigma_d", plan.Sigma, "fee_d", plan.FeeD, "fee_est_usdt", plan.FeeUSDT)
	cp := *lot
	return &cp, nil
}

// --- booking ---

// Sync books a snapshot of the open cycle's order records: the ledger is
// recomputed from it. Of each order the newest record seen is kept (by
// UpdateSeq), so a snapshot older than one already booked moves nothing
// backwards, and one that holds no newer record than those booked - exec
// books the snapshot at every wake-up of its loop - is not recomputed: only
// whether the cycle's funding is awaited can change with the time alone.
// Records of another cycle are ignored; without an open cycle Sync does
// nothing.
//
// The caller guarantees what that rule rests on: within one process the
// UpdateSeq of an order grows with every change of its record and never
// starts over, also for a record recognised from the venue after a restart.
// A record whose UpdateSeq went back would be taken for an old one and its
// change ignored.
func (m *InventoryManager) Sync(orders []cex.OrderRecord) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sync(orders, m.timing.clock())
}

// sync is Sync at the given time; the caller holds mu.
func (m *InventoryManager) sync(orders []cex.OrderRecord, now time.Time) {
	c := m.cycle
	if c == nil {
		return
	}
	changed := false
	for _, o := range orders {
		if CycleID(o.Request.CycleID) != c.ID {
			continue
		}
		if held, ok := m.orders[o.Request.ClientOrderID]; ok && held.UpdateSeq >= o.UpdateSeq {
			continue
		}
		m.orders[o.Request.ClientOrderID] = o
		m.assign(o)
		changed = true
	}
	if len(m.orders) == 0 {
		// Nothing to recompute from: the lots keep what they have, which is
		// zero in a new cycle and the stored amounts after a restart.
		return
	}
	if !changed && c.FundingKnown == (len(m.fundingAwaited(c.OpenedAt, now, m.fundingSeen)) == 0) {
		return // booked already
	}
	sent := m.ordersAsSent()
	for id, q := range rebuild(c.Lots, sent, m.assigned) {
		if !q.Equal(m.dropped[id]) {
			m.dropped[id] = q
			logger.GlobalLogger.Warn("inventory: an exit fill beyond what the lots hold is not booked; the position check shows it", "cycle", c.ID, "client_order_id", id, "qty", q)
		}
	}
	m.reserved = pendingNotional(sent)
	m.refresh(now)
}

// assign fixes, once, the lot an entry, completion or trim order is booked
// on (PRD 7.4): the lot it was sent with, and for an order recognised from
// the venue after a restart, which carries none, the last lot still building
// at this moment (the only one that can be completed or trimmed at any
// moment), or else the newest lot. The assignment never changes afterwards:
// decided again at a later recomputation, it would move the order's fill to
// whichever batch is building then. The caller holds mu.
func (m *InventoryManager) assign(o cex.OrderRecord) {
	id, purpose := o.Request.ClientOrderID, o.Request.Purpose
	if _, done := m.assigned[id]; done || len(m.cycle.Lots) == 0 ||
		(purpose != cex.PurposeEntry && purpose != cex.PurposeComplete && purpose != cex.PurposeTrim) {
		return
	}
	lots := m.cycle.Lots
	lot, sent := lots[len(lots)-1], false
	for _, l := range lots {
		if l.Status == LotBuilding {
			lot = l
		}
	}
	for _, l := range lots {
		if l.ID == LotID(o.Request.LotID) {
			lot, sent = l, true
		}
	}
	m.assigned[id] = lot.ID
	if !sent {
		logger.GlobalLogger.Info("inventory: order without a lot assigned to one", "order", id, "purpose", purpose, "leg", o.Request.Leg.String(), "lot", lot.ID, "lot_status", lot.Status)
	}
}

// LotOf returns the lot an entry, completion or trim order of the open
// cycle is booked on; empty for an order not booked yet and for exits. The
// order recovery stamps it on a record it recognised without a lot, so that
// it is stored in cex_order.lot_id and a later start books the order on the
// same lot.
func (m *InventoryManager) LotOf(id cex.ClientOrderID) LotID {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.assigned[id]
}

// ordersAsSent returns the kept order records in the order they were sent.
// The ids of one cycle share their prefix and carry a fixed-width sequence
// number, so their string order is that order.
func (m *InventoryManager) ordersAsSent() []cex.OrderRecord {
	out := make([]cex.OrderRecord, 0, len(m.orders))
	for _, o := range m.orders {
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Request.ClientOrderID < out[j].Request.ClientOrderID })
	return out
}

// share is what of one order is not allocated yet.
type share struct{ qty, value, fee decimal.Decimal }

// allocDecimals is the scale a proportional part of a value or a fee is
// rounded to.
const allocDecimals = 8

// take cuts q out of what is left. A part gets value and fee in proportion
// to its quantity, rounded; the part that takes all that is left gets
// exactly the remaining value and fee, so the parts always add up to the
// totals.
func (s *share) take(q decimal.Decimal) share {
	if q.GreaterThanOrEqual(s.qty) {
		part := *s
		*s = share{}
		return part
	}
	part := share{qty: q, value: s.value.Mul(q).Div(s.qty).Round(allocDecimals), fee: s.fee.Mul(q).Div(s.qty).Round(allocDecimals)}
	s.qty, s.value, s.fee = s.qty.Sub(q), s.value.Sub(part.value), s.fee.Sub(part.fee)
	return part
}

// portion is total x part / whole, rounded; exactly total when part is the
// whole, and zero when there is no whole.
func portion(total, part, whole decimal.Decimal) decimal.Decimal {
	switch {
	case whole.IsZero():
		return decimal.Zero
	case part.Equal(whole):
		return total
	}
	return total.Mul(part).Div(whole).Round(allocDecimals)
}

// rebuild recomputes the legs of every lot from the cycle's orders, in the
// order they were sent. An entry, completion or trim order is booked on the
// lot assigned to it; an order without one is left out. An order's value
// and fee are spread over its quantity through share.take, at the order's
// average price (PRD 1.4 narrows 7.4 so). The recorded dust plays no part:
// every fill is booked by its order's purpose alone. A closing fill beyond
// what the lots hold is not booked; it is returned by order. It comes while
// an earlier fill of the leg is not known yet, when the venue's positions
// differ from the ledger anyway.
func rebuild(lots []*Lot, orders []cex.OrderRecord, assigned map[cex.ClientOrderID]LotID) (dropped map[cex.ClientOrderID]decimal.Decimal) {
	byID := make(map[LotID]*Lot, len(lots))
	for _, lot := range lots {
		lot.Legs = [2]LotLeg{}
		byID[lot.ID] = lot
	}
	for _, o := range orders {
		if beyond := book(lots, byID[assigned[o.Request.ClientOrderID]], o); beyond.IsPositive() {
			if dropped == nil {
				dropped = map[cex.ClientOrderID]decimal.Decimal{}
			}
			dropped[o.Request.ClientOrderID] = beyond
		}
	}
	return dropped
}

// book adds one order's fill to the lots, by its purpose: an entry or a
// completion to lot, the lot assigned to it (nil: not booked); a trim first
// to lot and the rest like an exit; an exit or a forced exit first in, first
// out. It returns the closing quantity beyond what the lots hold, which is
// not booked. rebuild books every order through it, and CloseNet a
// hypothetical one, so the two allocate alike.
func book(lots []*Lot, lot *Lot, o cex.OrderRecord) (beyond decimal.Decimal) {
	if !o.CumExecQty.IsPositive() {
		return decimal.Zero
	}
	rest := share{qty: o.CumExecQty, value: o.CumExecValue, fee: o.Fee}
	leg := o.Request.Leg
	switch o.Request.Purpose {
	case cex.PurposeEntry, cex.PurposeComplete:
		if lot != nil {
			lot.Legs[leg].addEntry(rest.take(rest.qty))
		}
		return decimal.Zero
	case cex.PurposeTrim:
		// Booked on the batch it trims (PRD 7.4), up to what that batch's
		// leg still holds. The gap a trim closes is the whole cycle's, so it
		// can be larger than the batch: the rest closes the older lots like
		// an exit.
		if lot != nil {
			if q := decimal.Min(rest.qty, lot.Legs[leg].Open()); q.IsPositive() {
				lot.Legs[leg].addExit(rest.take(q))
			}
		}
	}
	// exit and forced, and what a trim leaves: first in, first out; one
	// order across two lots is split by quantity
	for _, lot := range lots {
		if q := decimal.Min(rest.qty, lot.Legs[leg].Open()); q.IsPositive() {
			lot.Legs[leg].addExit(rest.take(q))
		}
	}
	return rest.qty
}

func (l *LotLeg) addEntry(part share) {
	l.EntryQty, l.EntryValue, l.EntryFee = l.EntryQty.Add(part.qty), l.EntryValue.Add(part.value), l.EntryFee.Add(part.fee)
}

func (l *LotLeg) addExit(part share) {
	l.ExitQty, l.ExitValue, l.ExitFee = l.ExitQty.Add(part.qty), l.ExitValue.Add(part.value), l.ExitFee.Add(part.fee)
}

// --- valuation ---

// CloseNet values a close of the open cycle that is not sent yet: close[leg]
// of each leg at its price, a fee of feeRate (a fraction of the traded
// value, the taker fee) on each, booked as the ledger would book such fills
// now (SPEC 12.2, 12.9). It is the one valuation of a close; it reads the
// ledger and changes nothing. What is booked already counts once.
//
// purpose sets the allocation, as for a real order:
//   - cex.PurposeExit (or cex.PurposeForced): first in, first out over the
//     cycle's lots. The result is the round's expected net: what the cycle
//     has realised, funding included, plus this close.
//   - cex.PurposeTrim: first back into lot, the rest first in, first out (an
//     unwind and a profit close of a thin leg are trims of their batch). The
//     result is the increment of this close alone: an older lot's profit
//     cannot make it pass (SPEC 12.5 example 11).
//
// It refuses another purpose, a trim without a lot of the cycle, and a close
// of more than the cycle holds on a leg.
func (m *InventoryManager) CloseNet(purpose cex.OrderPurpose, lot LotID, close [2]CloseLeg, feeRate decimal.Decimal) (decimal.Decimal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cycle == nil {
		return decimal.Zero, errNoCycle
	}
	c := m.cycle.clone() // booked on a copy: the ledger is not touched
	var into *Lot
	switch purpose {
	case cex.PurposeExit, cex.PurposeForced:
	case cex.PurposeTrim:
		for _, l := range c.Lots {
			if l.ID == lot {
				into = l
			}
		}
		if into == nil {
			return decimal.Zero, fmt.Errorf("inventory: CloseNet: a trim needs a lot of cycle %s, got %q", c.ID, lot)
		}
	default:
		return decimal.Zero, fmt.Errorf("inventory: CloseNet values a close, got purpose %s", purpose)
	}

	c.realise(true, time.Time{})
	before := c.Gross.Sub(c.Fees)
	for _, leg := range cex.Legs {
		q := close[leg]
		if !q.Qty.IsPositive() {
			continue
		}
		value := q.Qty.Mul(q.Price)
		o := cex.OrderRecord{Request: cex.OrderRequest{Leg: leg, Purpose: purpose}, CumExecQty: q.Qty, CumExecValue: value, Fee: value.Mul(feeRate)}
		if beyond := book(c.Lots, into, o); beyond.IsPositive() {
			return decimal.Zero, fmt.Errorf("inventory: CloseNet: closing %s %s is %s more than cycle %s holds", q.Qty, leg, beyond, c.ID)
		}
	}
	c.realise(true, time.Time{})
	if purpose == cex.PurposeTrim {
		return c.Gross.Sub(c.Fees).Sub(before), nil
	}
	return c.Gross.Sub(c.Fees).Add(c.Funding), nil
}

// --- results and statuses ---

// realise sets what the lot has realised from its legs (PRD 7.5). Per leg
// the closed part made the direction's sign times (exit value - its share
// of the entry value), and cost its exit fees plus the entry fees in
// proportion to the quantity closed. What is not closed realises nothing.
func (l *Lot) realise() {
	gross, fees := decimal.Zero, decimal.Zero
	for _, g := range l.Legs {
		cost := portion(g.EntryValue, g.ExitQty, g.EntryQty)
		if l.Direction == strategy.Long {
			gross = gross.Add(g.ExitValue).Sub(cost)
		} else {
			gross = gross.Add(cost).Sub(g.ExitValue)
		}
		fees = fees.Add(g.ExitFee).Add(portion(g.EntryFee, g.ExitQty, g.EntryQty))
	}
	l.Gross, l.Fees, l.Net = gross, fees, gross.Sub(fees)
}

// flat reports whether both legs of the lot are closed.
func (l *Lot) flat() bool {
	return l.Legs[cex.Bull].Open().IsZero() && l.Legs[cex.Bear].Open().IsZero()
}

// settle ends the building of a lot: built makes it a position, otherwise
// it is void, for the reason its fills show.
func (l *Lot) settle(built bool, now time.Time) {
	switch {
	case built:
		l.Status = LotOpen
	case l.Legs[cex.Bull].EntryQty.IsZero() && l.Legs[cex.Bear].EntryQty.IsZero():
		l.Status, l.VoidReason, l.ClosedAt = LotVoid, VoidNoFill, now
	default:
		l.Status, l.VoidReason, l.ClosedAt = LotVoid, VoidOneLeg, now
	}
}

// realise recomputes every lot's result, moves the built lots between open
// and closed, and sums the cycle's totals. A built lot is closed when both
// legs are zero and the cycle's orders are settled at that moment: each has
// a final state and each with a fill has its fee confirmed (unsettled is
// false). Until then an exit order still filling, or a fee still to come,
// would fix the lot at a result that is not its last (PRD 7.4).
func (c *Cycle) realise(unsettled bool, now time.Time) {
	c.Gross, c.Fees = decimal.Zero, decimal.Zero
	for _, lot := range c.Lots {
		lot.realise()
		switch {
		case lot.Status == LotOpen && lot.flat() && !unsettled:
			lot.Status, lot.ClosedAt = LotClosed, now
		case lot.Status == LotClosed && !lot.flat(): // its orders changed after it closed
			lot.Status, lot.ClosedAt = LotOpen, time.Time{}
		}
		c.Gross, c.Fees = c.Gross.Add(lot.Gross), c.Fees.Add(lot.Fees)
	}
	c.Net = c.Gross.Sub(c.Fees).Add(c.Funding)
}

// refresh brings the open cycle's results, statuses and rows in line with
// the legs as rebuild left them. Rows are recorded only when they differ
// from what was last written. The caller holds mu.
func (m *InventoryManager) refresh(now time.Time) {
	c := m.cycle
	before := make([]LotStatus, len(c.Lots))
	for i, lot := range c.Lots {
		before[i] = lot.Status
	}
	c.realise(!m.settled(), now)
	c.FundingKnown = len(m.fundingAwaited(c.OpenedAt, now, m.fundingSeen)) == 0
	for i, lot := range c.Lots {
		if lot.Status != before[i] {
			logLot(lot, before[i])
		}
	}

	var rows []store.Row
	for _, lot := range c.Lots {
		if row := lotRow(lot); fmt.Sprint(row) != m.lotWritten[lot.ID] {
			m.lotWritten[lot.ID] = fmt.Sprint(row)
			rows = append(rows, row)
		}
	}
	if row := cycleRow(c); fmt.Sprint(row) != m.cycleWritten {
		m.cycleWritten = fmt.Sprint(row)
		rows = append(rows, row)
	}
	if len(rows) > 0 {
		m.record(rows...)
		hold := c.Hold()
		logger.GlobalLogger.Debug("inventory: ledger recomputed", "cycle", c.ID, "orders", len(m.orders), "hold_bull", hold[cex.Bull], "hold_bear", hold[cex.Bear],
			"gross_usdt", c.Gross, "fees_usdt", c.Fees, "reserved_usdt", m.reserved, "rows", len(rows))
	}
}

// settled reports whether the open cycle's orders allow it to close: each
// has a final state, and each with a fill has its fee confirmed. A fee not
// reported yet is not a fee of zero.
func (m *InventoryManager) settled() bool {
	for _, o := range m.orders {
		if !o.Status.Terminal() || (o.CumExecQty.IsPositive() && !o.FeeKnown) {
			return false
		}
	}
	return true
}

// logLot writes the line of a lot's change of status, from from to the
// status it has now.
func logLot(l *Lot, from LotStatus) {
	message := "inventory: lot built"
	switch {
	case l.Status == LotClosed:
		message = "inventory: lot closed"
	case l.Status == LotVoid:
		message = "inventory: lot void"
	case from == LotClosed: // its orders changed after it closed
		message = "inventory: lot reopened"
	}
	bull, bear := l.Legs[cex.Bull], l.Legs[cex.Bear]
	attrs := []any{"lot", l.ID, "from", from, "to", l.Status, "direction", l.Direction,
		"entry_qty_bull", bull.EntryQty, "entry_avg_bull", avg(bull.EntryValue, bull.EntryQty), "exit_qty_bull", bull.ExitQty, "exit_avg_bull", avg(bull.ExitValue, bull.ExitQty),
		"entry_qty_bear", bear.EntryQty, "entry_avg_bear", avg(bear.EntryValue, bear.EntryQty), "exit_qty_bear", bear.ExitQty, "exit_avg_bear", avg(bear.ExitValue, bear.ExitQty),
		"gross_usdt", l.Gross, "fees_usdt", l.Fees, "net_usdt", l.Net}
	if l.Status == LotVoid {
		attrs = append(attrs, "void_reason", l.VoidReason)
	}
	if l.End != "" {
		attrs = append(attrs, "end", l.End)
	}
	logger.GlobalLogger.Info(message, attrs...)
}

// avg is value / qty for a log line; zero without a quantity.
func avg(value, qty decimal.Decimal) decimal.Decimal {
	if qty.IsZero() {
		return decimal.Zero
	}
	return value.Div(qty).Round(allocDecimals)
}

// --- settling and closing ---

// SettleLot ends the building of an entry batch, once exec finds no gap an
// order could be sent for: built makes it a position (open, and closed at
// once when nothing of it is left), otherwise it is void, as VoidNoFill or
// VoidOneLeg by what filled.
func (m *InventoryManager) SettleLot(id LotID, built bool, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cycle == nil {
		return
	}
	for _, lot := range m.cycle.Lots {
		if lot.ID != id || lot.Status != LotBuilding {
			continue
		}
		lot.settle(built, now)
		logLot(lot, LotBuilding)
		m.refresh(now)
		return
	}
	logger.GlobalLogger.Warn("inventory: no building lot to settle", "lot", id, "built", built)
}

// SettlePairedLot ends the building of a thin-leg-first entry batch in the
// end state exec found (SPEC 12.4): LotEndBuilt and LotEndBuiltPartial make
// it a position, like SettleLot's built; the three others make it void, the
// end state its void reason. From then on its thin leg takes quota in full.
func (m *InventoryManager) SettlePairedLot(id LotID, end LotEnd, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cycle == nil {
		return
	}
	for _, lot := range m.cycle.Lots {
		if lot.ID != id || lot.Status != LotBuilding {
			continue
		}
		switch end {
		case LotEndBuilt, LotEndBuiltPartial:
			lot.Status = LotOpen
		case LotEndEntryFailed, LotEndUnwoundProfit, LotEndUnwoundStop:
			lot.Status, lot.VoidReason, lot.ClosedAt = LotVoid, string(end), now
		default:
			logger.GlobalLogger.Warn("inventory: unknown end state; the lot stays building", "lot", id, "end", end)
			return
		}
		lot.End = end
		logLot(lot, LotBuilding)
		m.refresh(now)
		return
	}
	logger.GlobalLogger.Warn("inventory: no building lot to settle", "lot", id, "end", end)
}

// CloseCycle ends the open cycle for the given reason (an End constant). It
// refuses with ErrNotSettled while an order has no final state or a filled
// order's fee is not confirmed.
//
// Precondition: the caller has established that what each leg still holds
// cannot be sent as an order (below the venue's minimum quantity or minimum
// value at a price it has), and that the venue's positions agree. CloseCycle
// has no book and no rule to check that with: whatever is left, it records
// as dust.
//
// A lot still building is settled by its orders: built when both legs
// filled, void otherwise. What a leg still holds leaves its lot as dust
// (PRD 8.1): it is netted into the leg's recorded dust at the lot's entry
// average price, with an event, and realises nothing. The cycle's row, the
// rows of all its lots and inventory_state (with r_closed and the dust) are
// committed in one WriteSync, so the stored cycle is whole when it ends,
// also when an earlier lot row was dropped from the asynchronous queue. A
// changed capital that waited for this cycle takes effect in that same
// write: the cycle's result stays with the old capital, and the ledger
// starts again with the configured one, R at zero, from now. On an error
// nothing has changed and the call is repeated.
func (m *InventoryManager) CloseCycle(reason string, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cycle == nil {
		return errNoCycle
	}
	if !m.settled() {
		return ErrNotSettled
	}

	// Everything is prepared on a copy: the ledger changes only when the
	// rows are confirmed written.
	c := m.cycle.clone()
	for _, lot := range c.Lots {
		if lot.Status == LotBuilding {
			lot.settle(lot.Legs[cex.Bull].EntryQty.IsPositive() && lot.Legs[cex.Bear].EntryQty.IsPositive(), now)
		}
	}
	c.realise(false, now)

	// What a leg still holds cannot be traded: it is recorded as dust and
	// the lot ends with the cycle.
	type remainder struct {
		lot        LotID
		leg        cex.Leg
		qty, price decimal.Decimal // signed quantity, the lot's entry average price
	}
	var left []remainder
	dust := m.dust
	for _, lot := range c.Lots {
		for leg, g := range lot.Legs {
			qty := g.Open()
			if qty.IsZero() {
				continue
			}
			if c.Direction == strategy.Short {
				qty = qty.Neg()
			}
			price := avg(g.EntryValue, g.EntryQty)
			dust[leg] = dust[leg].add(qty, price)
			c.DustLeft[leg] = c.DustLeft[leg].Add(qty)
			left = append(left, remainder{lot: lot.ID, leg: cex.Leg(leg), qty: qty, price: price})
		}
		if lot.Status == LotOpen {
			lot.Status, lot.ClosedAt = LotClosed, now
		}
	}
	c.Status, c.EndReason, c.ClosedAt = CycleClosed, reason, now
	awaited := m.fundingAwaited(c.OpenedAt, now, m.fundingSeen)
	c.FundingKnown = len(awaited) == 0

	// R takes the cycle's net before funding: funding enters R when its
	// record is booked. A capital that waited for this close starts a new
	// ledger instead: the old R, this cycle included, ends here.
	capital, since, rClosed := m.capital, m.since, m.rClosed.Add(c.Gross).Sub(c.Fees)
	closedRow := cycleRow(c)
	closedRow.FundingAwaited = encodeAwaited(awaited)
	rows := []store.Row{closedRow}
	for _, lot := range c.Lots {
		rows = append(rows, lotRow(lot))
	}
	switched := m.capitalPending()
	if switched {
		rows = append(rows, capitalChanged(now, m.capital, m.cfg.Capital, rClosed))
		capital, since, rClosed = m.cfg.Capital, now, decimal.Zero
	}
	rows = append(rows, store.InventoryStateRow{Capital: capital, SinceMs: unixMs(since), RClosed: rClosed, Dust: encodeDust(dust)})
	log := logger.GlobalLogger
	if err := m.writeSync(rows...); err != nil {
		if m.closeFailure.failed(now, m.timing.failLogEvery) {
			log.Warn("inventory: cycle close not confirmed written; the cycle stays open and the close is tried again", "cycle", c.ID, "reason", reason, "err", err)
		}
		return fmt.Errorf("inventory: the close of cycle %s was not written: %w", c.ID, err)
	}
	if since, was := m.closeFailure.ended(); was {
		log.Info("inventory: cycle close written after failing", "cycle", c.ID, "failing_for", now.Sub(since).Round(time.Millisecond))
	}

	// Confirmed: the ledger moves on.
	for i, lot := range c.Lots {
		if from := m.cycle.Lots[i].Status; lot.Status != from {
			logLot(lot, from)
		}
	}
	log.Info("inventory: cycle closed", "cycle", c.ID, "reason", reason, "lots", len(c.Lots), "gross_usdt", c.Gross, "fees_usdt", c.Fees,
		"funding_usdt", c.Funding, "funding_known", c.FundingKnown, "net_usdt", c.Net, "r_closed_usdt", rClosed, "held", now.Sub(c.OpenedAt).Round(time.Millisecond))
	if switched {
		log.Warn("inventory: the configured capital takes effect; R restarts from zero", "from_usdt", m.capital, "to_usdt", capital, "since", showTime(since))
	}
	for _, r := range left {
		log.Warn("inventory: dust left", "cycle", c.ID, "lot", r.lot, "leg", r.leg.String(), "qty", r.qty, "avg_price", r.price,
			"dust_qty", dust[r.leg].Qty, "dust_avg_price", dust[r.leg].AvgPrice)
		m.event(now, "warn", "dust_left", "a quantity too small for an order was left on a leg when the cycle closed; it is recorded and not traded",
			map[string]any{"cycle_id": c.ID, "lot_id": r.lot, "leg": r.leg.String(), "qty": r.qty, "avg_price": r.price})
	}
	if !c.FundingKnown {
		m.pending = append(m.pending, &pendingCycle{row: closedRow, awaited: awaited})
	}
	m.capital, m.since, m.rClosed, m.dust, m.reserved = capital, since, rClosed, dust, decimal.Zero
	m.cycle, m.orders, m.assigned, m.lotWritten, m.cycleWritten, m.fundingSeen = nil, nil, nil, nil, "", nil
	return nil
}

// MarkTakeover records that a person took the positions over (PRD 10.3):
// the cycle is not closed and stays in the ledger with status takeover, so
// a later start reads it back.
func (m *InventoryManager) MarkTakeover(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.cycle
	if c == nil {
		return
	}
	from := c.Status
	c.Status = CycleTakeover
	row := cycleRow(c)
	m.cycleWritten = fmt.Sprint(row)
	m.record(row)
	hold := c.Hold()
	logger.GlobalLogger.Info("inventory: cycle taken over", "cycle", c.ID, "from", from, "to", c.Status, "at", showTime(now), "hold_bull", hold[cex.Bull], "hold_bear", hold[cex.Bear])
}
