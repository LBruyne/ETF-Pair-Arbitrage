package exec

import (
	"context"
	"os"
	"syscall"
	"time"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/inventory"
	"etf-pair-arbitrage/core/logger"
	"etf-pair-arbitrage/core/store"
	"etf-pair-arbitrage/core/strategy"
)

// ExecManager owns the trading loop: one goroutine that reads the facts from
// cex, decides with strategy, books through inventory and sends every order.
// The process signals are delivered to it, and it decides when the process
// may exit. Every field below "owned by Run" is read and written by that
// goroutine alone.
type ExecManager struct {
	// bound by NewManager
	cfg      Config
	cex      cexSource
	inv      *inventory.InventoryManager
	strat    *strategy.Strategy
	scfg     strategy.Config // the strategy's parameters, read by the loop
	leverage int             // L, which the account settings must show on both contracts
	timing   timings

	// bound by UseStore, before Run
	store store.Sink // nil records nothing

	// inboxes read by Run
	signals <-chan os.Signal // SIGINT and SIGTERM stop the process; SIGUSR1 stops new entries

	// owned by Run: the Session, the anchors and the minutes
	ctx             context.Context      // Run's; the positions read that confirms a flat cycle runs under it
	session         cex.Session          // the Session the loop is in
	anchors         cex.Anchors          // the current anchors as the loop uses them: none until cex publishes this Session's
	anchorChangedAt time.Time            // when the anchors' values last changed
	restartedAt     time.Time            // the last dividend restart taken (Anchors.RestartAt)
	minute          time.Time            // the start of the open minute
	obs             *observation         // the open minute's last usable observation; nil while it has none
	bars            []strategy.MinuteBar // the Session's finished minutes, oldest first
	runStart        int                  // bars[runStart:] is the run the mean and sigma are taken on
	stats           strategy.Stats       // of that run
	lastReady       strategy.Stats       // the last ready stats, kept across restarts of the mean: line exits use it while the mean warms up (PRD 9.7)

	// owned by Run: orders and decisions
	batch        *batchState         // the batch being worked, or the last one; nil before the first
	lastEntry    *strategy.LastEntry // the last built entry batch, for the spacing rule
	seqCycle     inventory.CycleID   // the cycle the order sequence counts in
	orderSeq     int                 // the sequence number of that cycle's last order
	lastReduceAt [2]time.Time        // by leg: when the last reducing order of a forced exit or a gap was sent
	nextCloseTry time.Time           // a cycle whose close failed is not tried again before this
	forced       string              // the end reason of the forced exit in progress (inventory.End*); empty while none
	lateLogged   bool                // the forced exit of a cycle of an ended Session was reported
	atLine       strategy.Reason     // the at_line_blocked reason recorded last; empty when the last entry check did not stop there
	stop         stopState

	// owned by Run: the record of each step
	idleReason             strategy.Reason // why this step sent nothing; empty when it sent an order
	nextSample, nextStatus time.Time
}

// NewManager validates cfg and builds the manager on the three components it
// drives. signals is the channel main registered for the process signals
// before anything else started: it is buffered, so a signal that arrived
// during start-up is still in it and is handled when Run begins. Signals do
// not cancel Run's context: the loop decides when the process may exit.
func NewManager(cfg Config, c cexSource, inv *inventory.InventoryManager, strat *strategy.Strategy, signals <-chan os.Signal) (*ExecManager, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &ExecManager{cfg: cfg, cex: c, inv: inv, strat: strat, scfg: strat.Config(), leverage: inv.Leverage(), timing: defaultTimings, signals: signals}, nil
}

// Run runs the loop until the process may exit and returns nil, or until
// ctx ends and returns its error. Every round reads the facts and decides
// (step); a round runs at each publication of books, orders, the account
// or the reference data, at each signal, and at least every step interval
// (PRD 13.1). SIGUSR1 stops new entries. SIGINT or SIGTERM closes what is
// held and lets the process leave once it is flat with no order unresolved;
// a signal received before Run started counts like one received now.
func (m *ExecManager) Run(ctx context.Context) error {
	log := logger.GlobalLogger
	m.ctx = ctx
	now := m.timing.clock()
	m.start(now)
	// Signals that came during start-up count before the first decision.
	for drained := false; !drained; {
		select {
		case sig := <-m.signals:
			m.onSignal(sig, now)
		default:
			drained = true
		}
	}
	log.Info("exec: running", "live", m.cfg.Live, "mean_model", m.scfg.MeanModel, "mean_minutes", m.scfg.MeanMinutes, "sigma_minutes", m.scfg.SigmaMinutes,
		"d_in", m.scfg.DIn, "d_out", m.scfg.DOut, "taker_fee_bps", m.scfg.TakerFeeBps, "leverage", m.leverage,
		"session", m.session.ID, "regular_open", showTime(m.session.RegularOpen), "session_end", showTime(m.session.End))
	defer log.Info("exec: stopped")
	tick := time.NewTicker(m.timing.step)
	defer tick.Stop()
	for {
		// The channels are taken before the facts are read, so a
		// publication during the step still wakes the next round.
		books, account, reference, orders := m.cex.BookChanged(), m.cex.AccountChanged(), m.cex.ReferenceChanged(), m.cex.OrdersChanged()
		if m.step(m.timing.clock()) {
			return nil
		}
		select {
		case <-ctx.Done(): // hard cancel: tests only; main stops through signals
			return ctx.Err()
		case sig := <-m.signals:
			m.onSignal(sig, m.timing.clock())
		case <-books:
		case <-orders:
		case <-account:
		case <-reference:
		case <-tick.C:
		}
	}
}

// start sets the loop's state at the first round: the Session, the open
// minute and no anchor yet. The minutes of a previous run are not read back:
// the mean warms up from here.
func (m *ExecManager) start(now time.Time) {
	m.session = m.cex.Session(now)
	m.anchors = cex.Anchors{SessionID: m.session.ID}
	m.minute = now.Truncate(time.Minute)
	m.nextSample, m.nextStatus = now, now
}

// onSignal takes one process signal. SIGUSR1 stops new entries. The first
// SIGINT or SIGTERM starts the stop: the forced exit closes what is held and
// the loop leaves when the cycle is closed (stopCheck). A later one changes
// nothing yet: taking the positions over by hand is not part of the loop.
func (m *ExecManager) onSignal(sig os.Signal, now time.Time) {
	log := logger.GlobalLogger
	switch {
	case sig == syscall.SIGUSR1 && !m.stop.noEntries:
		m.stop.noEntries = true
		log.Info("exec: no new entries", "signal", sig.String())
	case sig == syscall.SIGUSR1:
		log.Info("exec: no new entries already asked for", "signal", sig.String())
	case !m.stop.stopping:
		m.stop.stopping, m.stop.at = true, now
		dir, hold := m.inv.Holdings()
		log.Info("exec: stop requested", "signal", sig.String(), "direction", dir, "hold_bull", hold[cex.Bull], "hold_bear", hold[cex.Bear])
	default:
		log.Info("exec: stop already requested", "signal", sig.String(), "since", showTime(m.stop.at))
	}
}

// stopCheck reports whether the process may leave: a stop was asked for,
// no cycle is open (it closes only once its orders are settled and the
// venue's positions agree, closeIfFlat) and no order of any cycle is without
// a final state.
func (m *ExecManager) stopCheck(c *inventory.Cycle, now time.Time) bool {
	if !m.stop.stopping || c != nil || len(m.cex.UnresolvedOrders()) > 0 {
		return false
	}
	logger.GlobalLogger.Info("exec: flat after the stop; leaving", "stop_asked", showTime(m.stop.at), "took", now.Sub(m.stop.at).Round(time.Millisecond))
	return true
}

// logStatus writes the periodic status line: the phase, the anchors, the
// mean and sigma, the two deviations, why nothing was sent, the holdings and
// the quota.
func (m *ExecManager) logStatus(r *round, v view) {
	dir, hold := m.inv.Holdings()
	total, used, left := m.inv.Quota()
	logger.GlobalLogger.Info("exec: status", "phase", m.phase(r.now), "session", m.session.ID, "anchor_kind", string(m.anchors.Kind),
		"anchor_bull_usd", m.anchors.Bull, "anchor_bear_usd", m.anchors.Bear, "fx", r.fx.Price, "fx_fresh", r.fxFresh,
		"stats_ready", m.stats.Ready, "mu_d", v.mu, "sigma_d", v.sigma, "x_long_d", v.xLong, "x_short_d", v.xShort, "idle_reason", string(m.idleReason),
		"direction", dir, "hold_bull", hold[cex.Bull], "hold_bear", hold[cex.Bear], "quota_total_usdt", total, "quota_used_usdt", used, "quota_left_usdt", left,
		"forced", m.forced, "no_new_entries", m.stop.noEntries, "stopping", m.stop.stopping)
}

// utc8 is the zone times are shown in to people.
var utc8 = time.FixedZone("UTC+8", 8*60*60)

// showTime writes a time for a log line, in UTC+8.
func showTime(t time.Time) string {
	if t.IsZero() {
		return "none"
	}
	return t.In(utc8).Format("2006-01-02 15:04:05.000")
}
