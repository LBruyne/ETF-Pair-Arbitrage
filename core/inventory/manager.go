package inventory

import (
	"context"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/logger"
	"etf-pair-arbitrage/core/store"
)

// InventoryManager owns the ledger: the open cycle and its lots, the realised
// total R, the quota and the dust, and the comparison of the ledger with the
// positions on the exchange. It books what exec hands it and sends no order.
type InventoryManager struct {
	// bound by NewManager
	cfg Config
	cex *cex.CexManager // positions and funding records are read from here

	// bound by UseStore, before Run
	store store.Sink
}

// NewManager validates cfg and builds the manager on the exchange facts of c.
func NewManager(cfg Config, c *cex.CexManager) (*InventoryManager, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &InventoryManager{cfg: cfg, cex: c}, nil
}

// UseStore sets where the manager records its rows. Call it before Run.
func (m *InventoryManager) UseStore(s store.Sink) { m.store = s }

// Run blocks until ctx ends.
func (m *InventoryManager) Run(ctx context.Context) {
	log := logger.GlobalLogger
	log.Info("inventory: running", "capital_usdt", m.cfg.Capital, "leverage", m.cfg.Leverage)
	<-ctx.Done()
	log.Info("inventory: stopped")
}
