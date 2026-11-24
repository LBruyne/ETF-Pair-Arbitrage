package exec

import (
	"context"
	"os"
	"syscall"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/inventory"
	"etf-pair-arbitrage/core/logger"
	"etf-pair-arbitrage/core/store"
	"etf-pair-arbitrage/core/strategy"
)

// ExecManager owns the trading loop: one goroutine that reads the facts from
// cex, decides with strategy, books through inventory and sends every order.
// The process signals are delivered to it, and it decides when the process
// may exit.
type ExecManager struct {
	// bound by NewManager
	cfg   Config
	cex   *cex.CexManager
	inv   *inventory.InventoryManager
	strat *strategy.Strategy

	// bound by UseStore, before Run
	store store.Sink

	// inboxes read by Run
	signals <-chan os.Signal // SIGINT and SIGTERM stop the process; SIGUSR1 stops new entries
}

// NewManager validates cfg and builds the manager on the three components it
// drives. signals is the channel main registered for the process signals
// before anything else started: it is buffered, so a signal that arrived
// during start-up is still in it and is handled when Run begins. Signals do
// not cancel Run's context: the loop decides when the process may exit.
func NewManager(cfg Config, c *cex.CexManager, inv *inventory.InventoryManager, strat *strategy.Strategy, signals <-chan os.Signal) (*ExecManager, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &ExecManager{cfg: cfg, cex: c, inv: inv, strat: strat, signals: signals}, nil
}

// UseStore sets where the manager records its rows. Call it before Run.
func (m *ExecManager) UseStore(s store.Sink) { m.store = s }

// Run blocks until a stop signal (SIGINT or SIGTERM) and returns nil, or
// until ctx ends and returns its error. SIGUSR1 stops new entries and leaves
// it running. A signal received before Run started counts like one received
// now.
func (m *ExecManager) Run(ctx context.Context) error {
	log := logger.GlobalLogger
	log.Info("exec: running", "live", m.cfg.Live)
	defer log.Info("exec: stopped")
	for {
		select {
		case <-ctx.Done(): // hard cancel: tests only; main stops through signals
			return ctx.Err()
		case sig := <-m.signals:
			if sig == syscall.SIGUSR1 {
				log.Info("exec: no new entries", "signal", sig.String())
				continue
			}
			log.Info("exec: stop requested", "signal", sig.String())
			return nil
		}
	}
}
