package inventory

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/logger"
	"etf-pair-arbitrage/core/store"
	"etf-pair-arbitrage/core/strategy"
)

// InventoryManager owns the ledger: the open cycle and its lots, the realised
// total R, the quota and the dust, the comparison of the ledger with the
// positions on the exchange, and the funding records. It books what exec
// hands it and sends no order.
//
// The ledger methods (OpenCycle, OpenLot, Sync, SettleLot, CloseCycle,
// MarkTakeover, Reconcile) are called by exec's loop only. Run books the
// funding records, retries void rows and samples. Both sides share mu, and
// every method judges and changes within one hold of it; read methods
// return copies.
type InventoryManager struct {
	// bound by NewManager
	cfg     Config
	symbols [2]cex.Symbol // by leg
	cex     cexSource     // positions, funding records and settled funding rates are read from here
	timing  timings

	// bound by UseStore, before Restore and Run
	store store.Sink // nil records nothing

	mu sync.Mutex // guards everything below, except what Run owns

	// the ledger
	cycle        *Cycle                                // the unfinished cycle; nil when flat
	orders       map[cex.ClientOrderID]cex.OrderRecord // the newest record of each order of the open cycle
	assigned     map[cex.ClientOrderID]LotID           // the lot each of its entry, completion and trim orders is booked on, fixed when first seen
	reserved     decimal.Decimal                       // what its entry and completion orders without a final state may still fill, in USDT
	capital      decimal.Decimal                       // M the ledger runs on: the configured capital, or the stored one while a changed capital waits for the open cycle to close
	rClosed      decimal.Decimal                       // net of every finished cycle plus every funding record booked under that capital, in USDT
	since        time.Time                             // when this ledger started: its first start, or the last change of the capital (PRD 6.3)
	dust         [2]Dust                               // by leg: the net of the remainders the finished cycles left; recorded, never traded
	lotWritten   map[LotID]string                      // each open-cycle lot's row as last written, to write only changes
	dropped      map[cex.ClientOrderID]decimal.Decimal // the exit fill of each order beyond what the lots held, as last warned of
	cycleWritten string                                // the same for the open cycle's row
	voids        []store.Row                           // rows that void a cycle or a lot whose first write failed; no entries until they are confirmed

	// synchronous writes that are failing, for their logs
	voidFailure, closeFailure, fundingFailure writeFailure

	// reconciliation
	mismatch    bool               // the venue's positions were found to differ from the ledger; never cleared
	differSince time.Time          // the first of the reads that differ in a row; zero when the last read agreed
	differBy    [2]decimal.Decimal // by leg: the difference no open order explains, at differSince
	differAt    [2]decimal.Decimal // by leg: the ledger's position at differSince
	unread      bool               // the last read of the positions failed

	// funding
	fundingBooked map[string]bool     // record ids booked into R, this run's and earlier ones'
	fundingSeen   map[fundingKey]bool // the settlements and legs a record was booked for to the open cycle
	pending       []*pendingCycle     // finished cycles whose funding is not known yet

	// owned by Run
	nextSample, nextStatus time.Time
}

// NewManager validates cfg and builds the manager for the two legs of pair
// on the exchange facts of c.
func NewManager(cfg Config, pair cex.Pair, c cexSource) (*InventoryManager, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &InventoryManager{cfg: cfg, symbols: [2]cex.Symbol{pair.Bull.Symbol, pair.Bear.Symbol}, cex: c, timing: defaultTimings,
		capital: cfg.Capital, fundingBooked: map[string]bool{}}, nil
}

// Restore takes what the previous run stored, before Run: R and the dust,
// the funding records already booked, the finished cycles still waiting for
// funding, and the unfinished cycle with its lots. The ledger starts now
// when nothing was stored, and starts again, with R at zero, when the
// capital differs from the stored one (PRD 6.3): funding settled before its
// start is never booked. With an unfinished cycle a changed capital does
// not take effect yet: that cycle's results belong to the old capital and
// cannot be split between two baselines, so the ledger goes on with the
// stored capital, R and start, opens no lot (ErrCapitalPending), and
// switches when the cycle closes. The lots come back with their
// identity, status and stored amounts; orders, the cycle's order records,
// are then booked like any snapshot, which recomputes the amounts. Without
// records the stored amounts stand until the first Sync.
func (m *InventoryManager) Restore(state store.State, orders []cex.OrderRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.timing.clock()
	log := logger.GlobalLogger

	// R and the dust between cycles.
	if inv := state.Inventory; inv != nil {
		dust, err := decodeDust(inv.Dust)
		if err != nil {
			return fmt.Errorf("inventory: inventory_state.dust: %w", err)
		}
		m.rClosed, m.dust, m.since = inv.RClosed, dust, fromUnixMs(inv.SinceMs)
		switch {
		case inv.Capital.Equal(m.cfg.Capital):
		case state.Cycle != nil:
			m.capital = inv.Capital
			log.Warn("inventory: capital changed with a cycle unfinished; the configured capital waits for that cycle to close, no lot is opened meanwhile",
				"capital_in_effect_usdt", inv.Capital, "configured_usdt", m.cfg.Capital, "cycle", state.Cycle.CycleID, "r_closed_usdt", inv.RClosed)
		default:
			log.Warn("inventory: capital changed; R restarts from zero", "from_usdt", inv.Capital, "to_usdt", m.cfg.Capital, "r_closed_was_usdt", inv.RClosed)
			m.rClosed, m.since = decimal.Zero, now
			m.record(m.stateRow(m.rClosed, dust))
			m.record(capitalChanged(now, inv.Capital, m.cfg.Capital, inv.RClosed))
		}
	} else {
		m.since = now
		m.record(m.stateRow(m.rClosed, m.dust))
		log.Info("inventory: new ledger; funding settled before now is not booked", "capital_usdt", m.cfg.Capital, "since", showTime(now))
	}

	// Funding: what is booked, and the finished cycles still waiting.
	for _, f := range state.Funding {
		m.fundingBooked[f.RecordID] = true
	}
	for _, row := range state.PendingFunding {
		awaited, err := decodeAwaited(row.FundingAwaited)
		if err != nil {
			return fmt.Errorf("inventory: cycle %s funding_awaited: %w", row.CycleID, err)
		}
		m.pending = append(m.pending, &pendingCycle{row: row, awaited: awaited})
	}

	// The unfinished cycle.
	if state.Cycle != nil {
		m.cycle, m.cycleWritten = cycleFromRow(*state.Cycle, state.Lots), fmt.Sprint(*state.Cycle)
		m.orders, m.assigned, m.dropped = map[cex.ClientOrderID]cex.OrderRecord{}, map[cex.ClientOrderID]LotID{}, map[cex.ClientOrderID]decimal.Decimal{}
		m.lotWritten = map[LotID]string{}
		for _, row := range state.Lots {
			m.lotWritten[LotID(row.LotID)] = fmt.Sprint(row)
		}
		m.fundingSeen = m.fundingSeenOf(state.Funding, state.Cycle.CycleID)
		m.sync(orders, now)
	}

	total, used, _ := m.quota()
	attrs := []any{"capital_usdt", m.capital, "capital_pending", m.capitalPending(), "since", showTime(m.since), "r_usdt", m.realised(), "r_closed_usdt", m.rClosed, "quota_total_usdt", total, "quota_used_usdt", used,
		"dust_bull", m.dust[cex.Bull].Qty, "dust_bear", m.dust[cex.Bear].Qty, "funding_records", len(m.fundingBooked), "cycles_awaiting_funding", len(m.pending)}
	if c := m.cycle; c != nil {
		hold := c.Hold()
		attrs = append(attrs, "cycle", c.ID, "status", c.Status, "direction", c.Direction, "lots", len(c.Lots), "orders", len(m.orders), "hold_bull", hold[cex.Bull], "hold_bear", hold[cex.Bear])
	}
	log.Info("inventory: restored", attrs...)
	return nil
}

// Run retries unconfirmed void rows, books new funding records, and writes
// the samples and the status line, until ctx ends. It wakes on every account
// publication of cex and on its own tick. It never reconciles: exec's loop
// does, when the ledger is in step with the orders.
func (m *InventoryManager) Run(ctx context.Context) {
	log := logger.GlobalLogger
	log.Info("inventory: running", "configured_capital_usdt", m.cfg.Capital, "leverage", m.cfg.Leverage, "bull", m.symbols[cex.Bull], "bear", m.symbols[cex.Bear],
		"sample_interval", m.timing.sampleEvery, "status_interval", m.timing.statusLog)
	ticker := time.NewTicker(m.timing.tick)
	defer ticker.Stop()
	for {
		// Taken before the step, so a publication during it still wakes the
		// next round.
		account := m.cex.AccountChanged()
		m.step(m.timing.clock())
		select {
		case <-ctx.Done():
			log.Info("inventory: stopped")
			return
		case <-account:
		case <-ticker.C:
		}
	}
}

// step is one round of Run. Nothing is written synchronously while the
// store reports that the database cannot be written: such a write fails at
// once anyway.
func (m *InventoryManager) step(now time.Time) {
	if m.store == nil || m.store.Healthy() {
		m.retryVoids(now)
		m.bookFunding(now)
	}
	if !now.Before(m.nextSample) {
		m.sample(now)
		m.nextSample = now.Add(m.timing.sampleEvery)
	}
	if !now.Before(m.nextStatus) {
		m.logStatus()
		m.nextStatus = now.Add(m.timing.statusLog)
	}
}

// --- readers ---

// Cycle returns a deep copy of the unfinished cycle, nil when there is none.
func (m *InventoryManager) Cycle() *Cycle {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cycle == nil {
		return nil
	}
	return m.cycle.clone()
}

// Holdings returns the open cycle's direction and the quantity each leg
// holds, as the ledger stands. The dust is not part of it. Without a cycle
// dir is empty and both quantities are zero.
func (m *InventoryManager) Holdings() (dir strategy.Direction, hold [2]decimal.Decimal) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cycle == nil {
		return "", hold
	}
	return m.cycle.Direction, m.cycle.Hold()
}

// LedgerPositions returns the signed position the ledger expects on each
// leg: the open cycle's holdings by its direction - a short cycle holds both
// legs short - plus the dust. It is what Reconcile compares the venue's
// positions with.
func (m *InventoryManager) LedgerPositions() [2]decimal.Decimal {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ledgerQty()
}

// Dust returns the signed quantity of each leg's dust: what the finished
// cycles left on the venue outside any cycle.
func (m *InventoryManager) Dust() (qty [2]decimal.Decimal) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, leg := range cex.Legs {
		qty[leg] = m.dust[leg].Qty
	}
	return qty
}

// Settled reports whether the open cycle's orders allow it to close: each
// has a final state and each with a fill has its fee confirmed. It is true
// without a cycle.
func (m *InventoryManager) Settled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.settled()
}

// CapitalPending reports whether the configured capital differs from the
// one the ledger runs on and waits for the unfinished cycle to close. exec
// opens no lot meanwhile; reducing and closing are not affected.
func (m *InventoryManager) CapitalPending() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.capitalPending()
}

// Leverage is L, the leverage the quota is computed with and the two
// contracts must be set to on the account.
func (m *InventoryManager) Leverage() int { return int(m.cfg.Leverage.IntPart()) }

// capitalPending is CapitalPending; the caller holds mu.
func (m *InventoryManager) capitalPending() bool { return !m.capital.Equal(m.cfg.Capital) }

// capitalChanged is the event of a new capital taking effect.
func capitalChanged(now time.Time, from, to, rClosedWas decimal.Decimal) store.EventRow {
	return eventRow(now, "warn", "capital_changed", "inventory.capital differs from the stored one: R restarts from zero",
		map[string]any{"from": from, "to": to, "r_closed_was": rClosedWas})
}

// StoreBlocked reports whether a row that voids a cycle or a lot is still
// not confirmed written. exec opens nothing meanwhile (store_error); orders
// that only reduce are not affected.
func (m *InventoryManager) StoreBlocked() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.voids) > 0
}

// --- void rows ---

// retryVoids writes the rows that void a cycle or a lot whose first write
// failed, in one WriteSync, until the store confirms them. They never go
// through Record, which may drop a row.
func (m *InventoryManager) retryVoids(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.voids) == 0 {
		return
	}
	if err := m.writeSync(m.voids...); err != nil {
		if m.voidFailure.failed(now, m.timing.failLogEvery) {
			logger.GlobalLogger.Warn("inventory: void rows still not confirmed written; no entries meanwhile", "rows", len(m.voids), "err", err)
		}
		return
	}
	since, _ := m.voidFailure.ended()
	logger.GlobalLogger.Info("inventory: void rows confirmed written; entries may resume", "rows", len(m.voids), "failing_for", now.Sub(since).Round(time.Millisecond))
	m.voids = nil
}

// --- funding ---

// bookFunding books every funding record cex has read that is not booked
// yet (PRD 7.6) and completes the finished cycles whose awaited funding has
// turned out to be zero. Everything of one round goes into one WriteSync:
// the funding rows, the rows of the cycles concerned and inventory_state
// with r_closed. When it is not confirmed nothing is booked and the next
// round tries again.
//
// A record's settlement is its time floored to the funding grid, since the
// venue books a record a few seconds late. Two questions are answered
// apart. Which cycle it belongs to: the open cycle when that had opened by
// its settlement, else a finished cycle still waiting whose interval holds
// the settlement, else none. Whether it enters R: only when it settled at
// or after the ledger's start (PRD 6.3). So a record of an older cycle that
// arrives after the capital changed still completes that cycle and leaves
// the new R alone; one settled before the start that belongs to no cycle
// is not this ledger's and is left out altogether.
func (m *InventoryManager) bookFunding(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rClosed := m.rClosed
	var booked []store.FundingRow
	var open *Cycle                              // the open cycle with the new records added; nil when none is for it
	var seen map[fundingKey]bool                 // and what was booked to it
	changed := map[*pendingCycle]*pendingCycle{} // the waiting cycles a record or a zero rate changes
	edit := func(p *pendingCycle) *pendingCycle {
		if changed[p] == nil {
			changed[p] = &pendingCycle{row: p.row, awaited: slices.Clone(p.awaited)}
		}
		return changed[p]
	}

	for _, rec := range m.cex.FinancialRecords() {
		leg, ours := m.legOf(rec.Symbol)
		settlement := settlementOf(rec.At)
		if !ours || m.fundingBooked[rec.ID] {
			continue
		}
		key := fundingKey{settlementMs: settlement.UnixMilli(), leg: leg}
		row := store.FundingRow{RecordID: rec.ID, Symbol: string(rec.Symbol), Type: rec.Type, Amount: rec.Amount, TsMs: rec.At.UnixMilli(), SettlementMs: key.settlementMs}
		ofOpen := m.cycle != nil && !settlement.Before(m.cycle.OpenedAt)
		ofPending := m.pendingAt(key.settlementMs)
		if settlement.Before(m.since) {
			if !ofOpen && ofPending == nil {
				continue
			}
		} else {
			rClosed = rClosed.Add(rec.Amount)
		}
		if c := m.cycle; ofOpen {
			if open == nil {
				next := *c // the lots are shared and not changed
				open, seen = &next, maps.Clone(m.fundingSeen)
			}
			open.Funding, seen[key], row.CycleID = open.Funding.Add(rec.Amount), true, string(c.ID)
		} else if p := ofPending; p != nil {
			next := edit(p)
			next.row.Funding, next.row.Net = next.row.Funding.Add(rec.Amount), next.row.Net.Add(rec.Amount)
			next.awaited = slices.DeleteFunc(next.awaited, func(k fundingKey) bool { return k == key })
			row.CycleID = p.row.CycleID
		}
		booked = append(booked, row)
	}
	// A waiting cycle also completes when the rate of what it awaits is
	// published as zero.
	for _, p := range m.pending {
		for _, key := range p.awaited {
			if rate, ok := m.cex.FundingRateAt(key.leg, time.UnixMilli(key.settlementMs)); ok && rate.IsZero() {
				next := edit(p)
				next.awaited = slices.DeleteFunc(next.awaited, func(k fundingKey) bool { return k == key })
			}
		}
	}
	if len(booked) == 0 && len(changed) == 0 {
		return
	}

	var rows []store.Row
	for _, row := range booked {
		rows = append(rows, row)
	}
	if open != nil {
		open.Net = open.Gross.Sub(open.Fees).Add(open.Funding)
		open.FundingKnown = len(m.fundingAwaited(open.OpenedAt, now, seen)) == 0
		rows = append(rows, cycleRow(open))
	}
	for _, p := range m.pending { // in their order, not the map's
		if next := changed[p]; next != nil {
			next.row.FundingKnown, next.row.FundingAwaited = len(next.awaited) == 0, encodeAwaited(next.awaited)
			rows = append(rows, next.row)
		}
	}
	if len(booked) > 0 {
		rows = append(rows, m.stateRow(rClosed, m.dust))
	}
	log := logger.GlobalLogger
	if err := m.writeSync(rows...); err != nil {
		if m.fundingFailure.failed(now, m.timing.failLogEvery) {
			log.Warn("inventory: funding not confirmed written; nothing is booked and it is tried again", "records", len(booked), "cycles", len(changed), "err", err)
		}
		return
	}
	if since, was := m.fundingFailure.ended(); was {
		log.Info("inventory: funding written again", "failing_for", now.Sub(since).Round(time.Millisecond))
	}

	// Confirmed: the ledger takes it.
	m.rClosed = rClosed
	for _, row := range booked {
		m.fundingBooked[row.RecordID] = true
		log.Info("inventory: funding booked", "id", row.RecordID, "symbol", row.Symbol, "type", row.Type, "amount_usdt", row.Amount,
			"at", showTime(time.UnixMilli(row.TsMs)), "settlement", showTime(time.UnixMilli(row.SettlementMs)), "cycle", row.CycleID, "r_closed_usdt", rClosed)
	}
	if open != nil {
		c := m.cycle
		c.Funding, c.Net, c.FundingKnown = open.Funding, open.Net, open.FundingKnown
		m.fundingSeen, m.cycleWritten = seen, fmt.Sprint(cycleRow(open))
	}
	for p, next := range changed {
		*p = *next
		if p.row.FundingKnown {
			log.Info("inventory: cycle funding known", "cycle", p.row.CycleID, "funding_usdt", p.row.Funding, "net_usdt", p.row.Net)
		}
	}
	m.pending = slices.DeleteFunc(m.pending, func(p *pendingCycle) bool { return p.row.FundingKnown })
}

// pendingAt finds the finished cycle still waiting for funding whose
// interval holds the settlement; nil when there is none.
func (m *InventoryManager) pendingAt(settlementMs int64) *pendingCycle {
	for _, p := range m.pending {
		if p.row.OpenedMs <= settlementMs && settlementMs <= p.row.ClosedMs {
			return p
		}
	}
	return nil
}

// fundingAwaited lists what of the open cycle's funding is not known yet,
// for a cycle that ran from from to until (PRD 7.6): every settlement in
// that interval and leg the cycle held a position on at that moment, unless
// a record was booked to the cycle for it (seen) or its settled rate is
// published as zero, which is funding of zero. A rate that is not zero, or
// not published yet, without a record is awaited. A leg that held nothing
// at a settlement gets no record and awaits none. The caller holds mu.
func (m *InventoryManager) fundingAwaited(from, until time.Time, seen map[fundingKey]bool) (awaited []fundingKey) {
	first := settlementOf(from)
	if first.Before(from) {
		first = first.Add(fundingInterval)
	}
	for t := first; !t.After(until); t = t.Add(fundingInterval) {
		for _, leg := range cex.Legs {
			key := fundingKey{settlementMs: t.UnixMilli(), leg: leg}
			if seen[key] || !m.heldAt(leg, t) {
				continue
			}
			if rate, ok := m.cex.FundingRateAt(leg, t); !ok || !rate.IsZero() {
				awaited = append(awaited, key)
			}
		}
	}
	return awaited
}

// heldAt reports whether the open cycle held a position on the leg at t:
// the net of its orders' fills up to then is not zero. An order's fills
// are dated by the venue's time of its last change, which for an IOC order
// is the moment it traded. The caller holds mu.
func (m *InventoryManager) heldAt(leg cex.Leg, t time.Time) bool {
	net := decimal.Zero
	for _, o := range m.orders {
		at := o.ExchangeUpdatedAt
		if at.IsZero() {
			at = o.UpdateReceivedAt
		}
		if o.Request.Leg != leg || at.After(t) {
			continue
		}
		if o.Request.Side == cex.SideBuy {
			net = net.Add(o.CumExecQty)
		} else {
			net = net.Sub(o.CumExecQty)
		}
	}
	return !net.IsZero()
}

// fundingSeenOf collects, from the stored funding rows, the settlements
// and legs booked to one cycle.
func (m *InventoryManager) fundingSeenOf(rows []store.FundingRow, cycleID string) map[fundingKey]bool {
	seen := map[fundingKey]bool{}
	for _, f := range rows {
		if leg, ours := m.legOf(cex.Symbol(f.Symbol)); ours && f.CycleID == cycleID {
			seen[fundingKey{settlementMs: f.SettlementMs, leg: leg}] = true
		}
	}
	return seen
}

// settlementOf floors a time to the funding grid.
func settlementOf(t time.Time) time.Time {
	ms := t.UnixMilli()
	return time.UnixMilli(ms - ms%fundingInterval.Milliseconds())
}

// legOf returns the leg a contract is; ok is false for any other contract.
func (m *InventoryManager) legOf(symbol cex.Symbol) (cex.Leg, bool) {
	for _, leg := range cex.Legs {
		if m.symbols[leg] == symbol {
			return leg, true
		}
	}
	return 0, false
}

// --- samples and status ---

// sample records one inventory_sample row: the venue's positions as cex
// last published them (NULL while none was read) against the ledger. For
// the board it adds what the venue reported with each held position and the
// account's last equity and margin ratio; nothing is decided from them.
func (m *InventoryManager) sample(now time.Time) {
	positions := m.cex.Positions()
	assets, assetsRead := m.cex.AccountAssets()
	m.mu.Lock()
	defer m.mu.Unlock()
	ledger := m.ledgerQty()
	total, used, _ := m.quota()
	row := store.InventorySampleRow{TsMs: now.UnixMilli(), LedgerBull: ledger[cex.Bull], LedgerBear: ledger[cex.Bear],
		R: m.realised(), QuotaTotal: total, QuotaUsed: used, Mismatch: m.mismatch}
	if positions.Loaded {
		venue := m.legQty(positions)
		row.PosBull = decimal.NullDecimal{Decimal: venue[cex.Bull], Valid: true}
		row.PosBear = decimal.NullDecimal{Decimal: venue[cex.Bear], Valid: true}
	}
	bull, bear := positions.BySymbol[m.symbols[cex.Bull]], positions.BySymbol[m.symbols[cex.Bear]]
	row.MarkBull, row.UplBull, row.LiqBull = bull.MarkPrice, bull.UnrealisedPnL, bull.LiqPrice
	row.MarkBear, row.UplBear, row.LiqBear = bear.MarkPrice, bear.UnrealisedPnL, bear.LiqPrice
	if assetsRead {
		row.Equity, row.MarginRatio = decimal.NewNullDecimal(assets.Equity), decimal.NewNullDecimal(assets.MarginRatio)
	}
	m.record(row)
}

// logStatus writes the periodic one-line status.
func (m *InventoryManager) logStatus() {
	m.mu.Lock()
	defer m.mu.Unlock()
	ledger := m.ledgerQty()
	total, used, left := m.quota()
	attrs := []any{"ledger_bull", ledger[cex.Bull], "ledger_bear", ledger[cex.Bear], "dust_bull", m.dust[cex.Bull].Qty, "dust_bear", m.dust[cex.Bear].Qty,
		"capital_usdt", m.capital, "capital_pending", m.capitalPending(), "r_usdt", m.realised(), "quota_total_usdt", total, "quota_used_usdt", used, "quota_left_usdt", left,
		"mismatch", m.mismatch, "store_blocked", len(m.voids) > 0, "cycles_awaiting_funding", len(m.pending)}
	if c := m.cycle; c != nil {
		attrs = append(attrs, "cycle", c.ID, "status", c.Status, "direction", c.Direction, "lots", len(c.Lots),
			"gross_usdt", c.Gross, "fees_usdt", c.Fees, "funding_usdt", c.Funding, "funding_known", c.FundingKnown)
	}
	logger.GlobalLogger.Info("inventory: status", attrs...)
}

// utc8 is the zone log lines show times in.
var utc8 = time.FixedZone("UTC+8", 8*60*60)

// showTime writes an instant for a log line, in UTC+8.
func showTime(t time.Time) string { return t.In(utc8).Format(time.DateTime) }
