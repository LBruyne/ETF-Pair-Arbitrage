package cex

import (
	"context"

	"etf-pair-arbitrage/core/logger"
	"etf-pair-arbitrage/core/store"
)

// CexManager owns the exchange side and the reference data: the venue and
// the read-only sources, the two legs' books, the anchors, the account's
// positions and the orders. The other managers read its published snapshots
// and place orders through it.
type CexManager struct {
	// bound by NewManager
	cfg Config

	// bound by UseStore, before Run
	store store.Sink
}

// NewManager validates cfg and builds the manager. Nothing is connected
// until Run.
func NewManager(cfg Config) (*CexManager, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &CexManager{cfg: cfg}, nil
}

// UseStore sets where the manager records its rows. Call it before Run.
func (m *CexManager) UseStore(s store.Sink) { m.store = s }

// Run blocks until ctx ends.
func (m *CexManager) Run(ctx context.Context) {
	log := logger.GlobalLogger
	log.Info("cex: running", "bull", m.cfg.Pair.Bull.Symbol, "bear", m.cfg.Pair.Bear.Symbol)
	<-ctx.Done()
	log.Info("cex: stopped")
}
