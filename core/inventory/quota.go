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
// price. left is total - used and may be negative.
func (m *InventoryManager) Quota() (total, used, left decimal.Decimal) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.quota()
}

// quota is Quota; the caller holds mu.
func (m *InventoryManager) quota() (total, used, left decimal.Decimal) {
	total = m.cfg.Leverage.Mul(m.capital.Add(decimal.Min(decimal.Zero, m.realised())))
	used = m.reserved.Add(m.held())
	return total, used, total.Sub(used)
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
