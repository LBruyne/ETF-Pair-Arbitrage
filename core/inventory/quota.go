package inventory

import (
	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
)

// Quota is the notional the strategy may hold and what of it is taken
// (PRD section 6), in USDT. total is L x (M + min(0, R)), with M the capital
// the ledger runs on (the stored one while a changed capital is pending): a loss lowers it,
// a profit does not raise it above L x M. used is the entry value of
// everything still held on both legs, batches not built yet included, plus
// the recorded dust of each leg at its average price, plus what the entry
// and completion orders without a final state may still fill at their quota
// price. With a thin leg first (WithThinLeg), used leaves out the unpaired
// thin leg of each batch still building (SPEC 12.4 "额度"): only what is
// paired, and what pending orders reserve, takes quota. left is total - used
// and may be negative.
func (m *InventoryManager) Quota() (total, used, left decimal.Decimal) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.quota()
}

// quota is Quota; the caller holds mu.
func (m *InventoryManager) quota() (total, used, left decimal.Decimal) {
	total = m.cfg.Leverage.Mul(m.capital.Add(decimal.Min(decimal.Zero, m.realised())))
	used = m.reserved.Add(m.held())
	if m.cycle != nil {
		for _, lot := range m.cycle.Lots {
			used = used.Sub(m.unpaired(lot).Cost)
		}
	}
	return total, used, total.Sub(used)
}

// Unpaired returns the unpaired thin leg of a batch of the open cycle while
// it is building (SPEC 12.2 S_open): zero without a thin leg first, for a
// batch that has ended and for one the cycle does not hold. Its Cost is
// what the quota leaves out and what the top-up quota counts it at.
func (m *InventoryManager) Unpaired(id LotID) Unpaired {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cycle != nil {
		for _, lot := range m.cycle.Lots {
			if lot.ID == id {
				return m.unpaired(lot)
			}
		}
	}
	return Unpaired{Qty: decimal.Zero, Cost: decimal.Zero}
}

// unpaired is Unpaired of one lot: the thin leg's open quantity less what
// the other leg pairs, its open quantity and the unfilled quantity of its
// pending entry and completion orders on this lot, at the cycle's pairing
// anchors (q_thin x A_thin = q_thick x A_thick). A pending order of the
// other leg reserves its own notional, and the thin quantity it would pair
// takes quota from then on. The caller holds mu.
func (m *InventoryManager) unpaired(lot *Lot) Unpaired {
	none := Unpaired{Qty: decimal.Zero, Cost: decimal.Zero}
	if m.thin == nil || lot.Status != LotBuilding {
		return none
	}
	thin, thick := *m.thin, cex.Bull
	if thin == cex.Bull {
		thick = cex.Bear
	}
	pairing := m.cycle.PairAnchors
	aThin, aThick := pairing.Bear, pairing.Bull
	if thin == cex.Bull {
		aThin, aThick = pairing.Bull, pairing.Bear
	}
	t := lot.Legs[thin]
	if !t.EntryQty.IsPositive() || !aThin.IsPositive() {
		return none
	}
	other := lot.Legs[thick].Open()
	for _, o := range m.orders {
		if o.Request.Leg == thick && !o.Status.Terminal() && m.assigned[o.Request.ClientOrderID] == lot.ID &&
			(o.Request.Purpose == cex.PurposeEntry || o.Request.Purpose == cex.PurposeComplete) {
			other = other.Add(o.Request.Qty.Sub(o.CumExecQty))
		}
	}
	qty := t.Open().Sub(other.Mul(aThick).Div(aThin))
	if !qty.IsPositive() {
		return none
	}
	return Unpaired{Qty: qty, Cost: portion(t.EntryValue, qty, t.EntryQty)}
}

// HeldNotional is the entry value of what the ledger holds now: every lot's
// open quantity at its entry price (first in, first out) plus the recorded
// dust at its average price. It is the quota's used without what the
// pending orders reserve; the step 8 replay takes its peak notional from it.
func (m *InventoryManager) HeldNotional() decimal.Decimal {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.held()
}

// held is HeldNotional; the caller holds mu.
func (m *InventoryManager) held() decimal.Decimal {
	held := decimal.Zero
	if m.cycle != nil {
		held = m.cycle.entryNotional()
	}
	// The dust is still held on the venue. A remainder is in its lot until
	// the cycle closes and in the dust from then on, at the same price: it
	// counts once throughout.
	for _, d := range m.dust {
		held = held.Add(d.Qty.Abs().Mul(d.AvgPrice))
	}
	return held
}

// realised is R, the realised total the quota follows (PRD 6.3): the net of
// every finished cycle and every funding record booked (r_closed), plus
// what the open cycle has realised so far. The open cycle's part is
// recomputed from its orders each time, so replaying them after a restart
// never counts it twice. The caller holds mu.
func (m *InventoryManager) realised() decimal.Decimal {
	if m.cycle == nil {
		return m.rClosed
	}
	return m.rClosed.Add(m.cycle.Gross).Sub(m.cycle.Fees)
}

// entryNotional is the open quantity of every lot at its entry average
// price, both legs summed: the part of each leg's entry value not closed.
func (c *Cycle) entryNotional() decimal.Decimal {
	sum := decimal.Zero
	for _, lot := range c.Lots {
		for _, g := range lot.Legs {
			sum = sum.Add(g.EntryValue).Sub(portion(g.EntryValue, g.ExitQty, g.EntryQty))
		}
	}
	return sum
}

// pendingNotional is what the entry and completion orders without a final
// state may still add: their unfilled quantity at their quota price, which
// is the limit of a buy and the decision's raw best bid of a sell (a sell
// fills at or above its limit); an order that carries none counts at its
// limit. What they have filled already is in the lots.
func pendingNotional(orders []cex.OrderRecord) decimal.Decimal {
	sum := decimal.Zero
	for _, o := range orders {
		if o.Status.Terminal() || (o.Request.Purpose != cex.PurposeEntry && o.Request.Purpose != cex.PurposeComplete) {
			continue
		}
		at := o.Request.QuotaPrice
		if at.IsZero() {
			at = o.Request.Price
		}
		sum = sum.Add(o.Request.Qty.Sub(o.CumExecQty).Mul(at))
	}
	return sum
}
