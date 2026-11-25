package inventory

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/logger"
	"etf-pair-arbitrage/core/strategy"
)

// Reconcile reads the two legs' positions from the venue once and compares
// them with the ledger (PRD 13.4, 13.7). Only exec's loop calls it, right
// after a Sync of the order snapshot. It returns nil when they agree,
// ErrPositionsUnread when the read failed, which is neither "flat" nor a
// mismatch, and ErrPositionsDiffer when they differ.
//
// A difference on a leg that the cycle's orders without a final state can
// explain (same leg, same side, not more than their unfilled quantity) is
// agreement. Order reports and position updates arrive in no fixed order,
// so one differing read proves nothing: Mismatch latches when a later read,
// at least reconcileConfirm after the first differing one, still differs.
func (m *InventoryManager) Reconcile(ctx context.Context) error {
	read, err := m.cex.ReadPositions(ctx) // outside mu: it is a network call
	if err == nil && !read.Loaded {
		err = errors.New("the positions are not loaded")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	log := logger.GlobalLogger
	if err != nil {
		if !m.unread {
			log.Warn("inventory: positions could not be read; not taken as flat", "err", err)
		}
		m.unread = true
		return fmt.Errorf("%w: %v", ErrPositionsUnread, err)
	}
	if m.unread {
		log.Info("inventory: positions read again")
	}
	m.unread = false

	venue, ledger := m.legQty(read), m.ledgerQty()
	differs := false
	for _, leg := range cex.Legs {
		if diff := venue[leg].Sub(ledger[leg]); !diff.IsZero() && !m.explained(leg, diff) {
			differs = true
		}
	}
	now := m.timing.clock()
	switch {
	case !differs:
		m.differSince = time.Time{}
		return nil
	case m.differSince.IsZero(): // the first differing read
		m.differSince = now
		log.Warn("inventory: positions differ from the ledger; reading again", "venue_bull", venue[cex.Bull], "venue_bear", venue[cex.Bear],
			"ledger_bull", ledger[cex.Bull], "ledger_bear", ledger[cex.Bear], "confirm_after", m.timing.reconcileConfirm)
	case !m.mismatch && now.Sub(m.differSince) >= m.timing.reconcileConfirm:
		m.mismatch = true
		log.Error("inventory: positions do not match the ledger; no orders until a person checks", "venue_bull", venue[cex.Bull], "venue_bear", venue[cex.Bear],
			"ledger_bull", ledger[cex.Bull], "ledger_bear", ledger[cex.Bear], "dust_bull", m.dust[cex.Bull].Qty, "dust_bear", m.dust[cex.Bear].Qty)
		m.event(now, "error", "position_mismatch", "the venue's positions do not match the ledger; no orders until a person checks and the process is restarted",
			map[string]any{"venue_bull": venue[cex.Bull], "venue_bear": venue[cex.Bear], "ledger_bull": ledger[cex.Bull], "ledger_bear": ledger[cex.Bear]})
	}
	return ErrPositionsDiffer
}

// Mismatch reports whether the venue's positions were found to differ from
// the ledger. It is never cleared: a person checks and restarts the
// process.
func (m *InventoryManager) Mismatch() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mismatch
}

// legQty is the signed position of each leg in a position set; a leg
// without an entry holds nothing.
func (m *InventoryManager) legQty(set cex.PositionSet) (qty [2]decimal.Decimal) {
	for _, leg := range cex.Legs {
		qty[leg] = set.BySymbol[m.symbols[leg]].Qty
	}
	return qty
}

// ledgerQty is the signed position the ledger expects on each leg: the open
// cycle's holdings by its direction, plus the dust. The caller holds mu.
func (m *InventoryManager) ledgerQty() (qty [2]decimal.Decimal) {
	for _, leg := range cex.Legs {
		qty[leg] = m.dust[leg].Qty
	}
	if c := m.cycle; c != nil {
		for leg, held := range c.Hold() {
			if c.Direction == strategy.Short {
				held = held.Neg()
			}
			qty[leg] = qty[leg].Add(held)
		}
	}
	return qty
}

// explained reports whether diff, the venue's position less the ledger's
// on one leg, can be a fill the ledger has not seen yet: orders of that leg
// without a final state, on the side of diff, with at least that much
// unfilled. The caller holds mu.
func (m *InventoryManager) explained(leg cex.Leg, diff decimal.Decimal) bool {
	side := cex.SideBuy
	if diff.IsNegative() {
		side = cex.SideSell
	}
	room := decimal.Zero
	for _, o := range m.orders {
		if o.Request.Leg == leg && o.Request.Side == side && !o.Status.Terminal() {
			room = room.Add(o.Request.Qty.Sub(o.CumExecQty))
		}
	}
	return diff.Abs().LessThanOrEqual(room)
}
