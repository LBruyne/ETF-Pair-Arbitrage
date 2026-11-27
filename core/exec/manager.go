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
	scfg     strategy.Config  // the strategy's parameters, read by the loop
	pairing  strategy.Pairing // exec's thin-leg-first keys as strategy reads them; the zero value sends both legs together
	leverage int              // L, which the account settings must show on both contracts
	timing   timings

	// bound by UseStore, before Run
	store store.Sink // nil records nothing

	// inboxes read by Run
	signals <-chan os.Signal // SIGINT and SIGTERM stop the process; SIGUSR1 stops new entries

	// owned by Run: the Session, the anchors and the minutes
	ctx             context.Context      // Run's; every read of the venue's positions runs under it
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
	lastSentAt   [2]time.Time        // by leg: when the last reducing or completion order was sent
	nextCloseTry time.Time           // a cycle whose close failed is not tried again before this

	// owned by Run: the half target of exit1 (SPEC 12.6), of cycle stageCycle
	stage                   strategy.ExitStage
	stageCycle              inventory.CycleID
	stageSetAt, stageDoneAt time.Time

	// bound by Restore, before Run
	restoredBars  []store.MinuteBarRow  // the Session's minutes the previous run recorded
	restoredFacts []store.AnchorFactRow // the Session's anchor facts the previous run recorded
	restoredEntry *store.DecisionRow    // the entry decision of the lot still building, as the previous run recorded it; nil when none
	restoredExit  *store.DecisionRow    // the cycle's last exit decision, as the previous run recorded it; nil when none
	perp          minuteSource          // the recorder's per-second rows; nil: none

	// owned by Run: recovery
	recovered struct{ orders, minutes bool } // the recovery steps done, so that a retry does not repeat them

	// owned by Run: the exceptions
	unknown           map[cex.ClientOrderID]*unknownOrder // the orders of unknown outcome
	nextPositionsRead time.Time                           // the venue's positions are not read again for an unknown outcome before this
	readAt            time.Time                           // the round of the last of those reads that succeeded
	nextReconcile     time.Time                           // the next comparison of the venue's positions with the ledger
	agreedAt          time.Time                           // the round of the last comparison that agreed: a stop leaves only after one made since the stop
	nextMismatchAlarm time.Time                           // the next repetition of the mismatch alarm
	rejects           [2]*rejectRun                       // by leg: the orders the venue refused in a row; nil while the last result was not a refusal
	rejectSeen        rejectCursor                        // the orders whose results watchRejects has taken
	forced            string                              // the end reason of the forced exit in progress (inventory.End*); empty while none
	lateLogged        bool                                // the forced exit of a cycle of an ended Session was reported
	atLine            strategy.Reason                     // the at_line_blocked reason recorded last; empty when the last entry check did not stop there
	stop              stopState

	// owned by Run: the record of each step
	idleReason             strategy.Reason // why this step sent nothing; empty when it sent an order
	nextSample, nextStatus time.Time

	// owned by Advance: Run's waiting, for a driver that owns the clock
	driven drivenWait
}

// drivenWait is what Run's select waits on, kept between two Advance calls:
// the channels taken before the last round and the next tick.
type drivenWait struct {
	started                           bool
	books, orders, account, reference <-chan struct{}
	nextTick                          time.Time
}

// maxDrivenRounds bounds the rounds one Advance call runs back to back
// while each round's publications wake the next; the next call goes on.
const maxDrivenRounds = 16

// Option changes how a manager runs. The process passes none.
type Option func(*ExecManager)

// WithClock makes the loop read the time from c instead of the wall clock:
// a replay's virtual clock (cmd/replay), with Start and Advance in place of
// Run.
func WithClock(c cex.Clock) Option { return func(m *ExecManager) { m.timing.clock = c.Now } }

// NewManager validates cfg and builds the manager on the three components it
// drives. signals is the channel main registered for the process signals
// before anything else started: it is buffered, so a signal that arrived
// during start-up is still in it and is handled when Run begins. Signals do
// not cancel Run's context: the loop decides when the process may exit.
func NewManager(cfg Config, c cexSource, inv *inventory.InventoryManager, strat *strategy.Strategy, signals <-chan os.Signal, opts ...Option) (*ExecManager, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	m := &ExecManager{cfg: cfg, cex: c, inv: inv, strat: strat, scfg: strat.Config(), leverage: inv.Leverage(), timing: defaultTimings, signals: signals,
		unknown: map[cex.ClientOrderID]*unknownOrder{}}
	if thin, ok := cfg.Thin(); ok {
		m.pairing = strategy.Pairing{ThinFirst: true, Thin: thin, Window: cfg.PairWindow, StopD: cfg.PairStopD, TopupMax: cfg.ThinTopupMax, ExitWindow: cfg.ExitPairWindow}
	}
	for _, opt := range opts {
		opt(m)
	}
	return m, nil
}

// Run runs the loop until the process may exit and returns nil, or until
// ctx ends and returns its error. Every round reads the facts and decides
// (step); a round runs at each publication of books, orders, the account
// or the reference data, at each signal, and at least every step interval
// (PRD 13.1). SIGUSR1 stops new entries. SIGINT or SIGTERM closes what is
// held and lets the process leave once it is flat with no order unresolved;
// a signal received before Run started counts like one received now.
func (m *ExecManager) Run(ctx context.Context) error {
	if leave, err := m.Start(ctx); err != nil || leave {
		return err
	}
	defer logger.GlobalLogger.Info("exec: stopped")
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

// Start is what Run does before its loop: the first round's state, the
// signals that came during start-up, and the recovery (PRD 13.4). leave is
// true when the process may exit already (a takeover or a stop during the
// recovery). Run calls it; a driver that owns the clock (cmd/replay) calls
// it and then Advance, in place of Run.
func (m *ExecManager) Start(ctx context.Context) (leave bool, err error) {
	m.ctx = ctx
	now := m.timing.clock()
	m.start(now)
	m.driven.nextTick = now.Add(m.timing.step)
	// Signals that came during start-up count before the first decision.
	for drained := false; !drained; {
		select {
		case sig := <-m.signals:
			m.onSignal(sig, now)
		default:
			drained = true
		}
	}
	if leave, err := m.recover(ctx); err != nil || leave {
		return leave, err
	}
	logger.GlobalLogger.Info("exec: running", "live", m.cfg.Live, "mean_model", m.scfg.MeanModel, "mean_minutes", m.scfg.MeanMinutes, "sigma_minutes", m.scfg.SigmaMinutes,
		"entry_in", m.scfg.EntryIn, "entry_out", m.scfg.EntryOut, "exit2", m.scfg.Exit2Text(), "taker_fee_bps", m.scfg.TakerFeeBps, "leverage", m.leverage,
		"thin_leg", m.cfg.ThinLeg, "pair_window", m.cfg.PairWindow, "pair_stop_d", m.cfg.PairStopD, "thin_topup_max", m.cfg.ThinTopupMax, "exit_pair_window", m.cfg.ExitPairWindow,
		"session", m.session.ID, "regular_open", showTime(m.session.RegularOpen), "session_end", showTime(m.session.End))
	return false, nil
}

// Advance is Run's loop for a driver that owns the clock (cmd/replay),
// after Start: the driver calls it at each moment it delivered something
// and at each NextTick. A round runs at now when Run's would: at the first
// call, when a publication of the books, the orders, the account or the
// reference data closed a channel taken before the last round, or at a tick
// of the step interval, counted from Start on the manager's clock (a call
// past several ticks runs one round, as a ticker drops the ticks a busy
// loop missed). A round whose own publications close those channels is
// followed at once by another, as in Run. Signals are not read. It reports
// whether the process may exit.
func (m *ExecManager) Advance(now time.Time) (done bool) {
	d := &m.driven
	due := !d.started || !now.Before(d.nextTick)
	for !now.Before(d.nextTick) {
		d.nextTick = d.nextTick.Add(m.timing.step)
	}
	for rounds := 0; rounds < maxDrivenRounds && (due || closedAny(d.books, d.orders, d.account, d.reference)); rounds++ {
		due, d.started = false, true
		d.books, d.account, d.reference, d.orders = m.cex.BookChanged(), m.cex.AccountChanged(), m.cex.ReferenceChanged(), m.cex.OrdersChanged()
		if m.step(now) {
			return true
		}
	}
	return false
}

// NextTick is when Advance's next tick is due.
func (m *ExecManager) NextTick() time.Time { return m.driven.nextTick }

// closedAny reports whether one of the channels is closed.
func closedAny(chans ...<-chan struct{}) bool {
	for _, c := range chans {
		select {
		case <-c:
			return true
		default:
		}
	}
	return false
}

// start sets the loop's state at the first round: the Session, the open
// minute and no anchor yet. The recovery then restores the Session's
// minutes.
func (m *ExecManager) start(now time.Time) {
	m.session = m.cex.Session(now)
	m.anchors = cex.Anchors{SessionID: m.session.ID}
	m.minute = now.Truncate(time.Minute)
	m.nextSample, m.nextStatus = now, now
}

// onSignal takes one process signal (PRD 10.3). SIGUSR1 stops new entries.
// The first SIGINT or SIGTERM starts the stop: the forced exit closes what
// is held and the loop leaves when the cycle is closed (stopCheck). One that
// comes after the not-flat alarm is a person taking over; one before the
// alarm changes nothing.
func (m *ExecManager) onSignal(sig os.Signal, now time.Time) {
	log := logger.GlobalLogger
	switch {
	case sig == syscall.SIGUSR1 && !m.stop.noEntries:
		m.stop.noEntries = true
		log.Info("exec: no new entries", "signal", sig.String())
	case sig == syscall.SIGUSR1:
		log.Info("exec: no new entries already asked for", "signal", sig.String())
	case !m.stop.stopping:
		m.stop.stopping, m.stop.at, m.stop.nextAlarm = true, now, now.Add(m.cfg.StopAlarmAfter)
		m.nextReconcile = now // a stop leaves only on a comparison made after it
		dir, hold := m.inv.Holdings()
		log.Info("exec: stop requested", "signal", sig.String(), "direction", dir, "hold_bull", hold[cex.Bull], "hold_bear", hold[cex.Bear])
	case m.stop.alarmed:
		m.stop.takeover = true
		log.Info("exec: takeover requested", "signal", sig.String(), "stop_asked", showTime(m.stop.at))
	default:
		log.Info("exec: stop already requested; a repeat before the alarm is not a takeover", "signal", sig.String(), "since", showTime(m.stop.at))
	}
}

// stopCheck reports whether the process may leave after a stop signal: no
// cycle is open (it closes only once its orders are settled and the venue's
// positions agree, closeIfFlat), no order of any cycle is without a final
// state - a read of zero positions alone is not enough - and a comparison
// made since the stop found the venue's positions equal to the ledger's,
// its recorded dust included (onSignal makes one due). While that is
// not so, the stop's alarm is given stop_alarm_after after it and again at
// every interval (PRD 10.3); a takeover asked for after the alarm leaves at
// once, with the cycle kept as it is.
func (m *ExecManager) stopCheck(r *round, now time.Time) bool {
	log := logger.GlobalLogger
	if !m.stop.stopping {
		return false
	}
	unresolved := m.cex.UnresolvedOrders()
	if m.stop.takeover {
		m.takeOver(r, unresolved, now)
		return true
	}
	if r.cycle == nil && len(unresolved) == 0 && !m.agreedAt.Before(m.stop.at) {
		log.Info("exec: flat after the stop; leaving", "stop_asked", showTime(m.stop.at), "took", now.Sub(m.stop.at).Round(time.Millisecond))
		return true
	}
	if !now.Before(m.stop.nextAlarm) {
		m.stop.alarmed, m.stop.nextAlarm = true, now.Add(m.cfg.StopAlarmAfter)
		dir, hold := m.inv.Holdings()
		bull, bear := r.books.ByLeg[cex.Bull], r.books.ByLeg[cex.Bear]
		log.Error("exec: not flat 60 s after stop", "since", now.Sub(m.stop.at).Round(time.Second), "direction", dir, "hold_bull", hold[cex.Bull], "hold_bear", hold[cex.Bear],
			"unresolved_orders", len(unresolved), "best_bid_bull", bull.BestBid.Price, "best_ask_bull", bull.BestAsk.Price, "best_bid_bear", bear.BestBid.Price, "best_ask_bear", bear.BestAsk.Price)
		m.event(now, "error", "stop_not_flat", "not flat after the stop signal: still closing; a further stop signal takes the positions over by hand",
			map[string]any{"since_ms": m.stop.at.UnixMilli(), "direction": dir, "hold_bull": hold[cex.Bull], "hold_bear": hold[cex.Bear], "unresolved_orders": orderIDs(unresolved),
				"best_bid_bull": bull.BestBid.Price, "best_ask_bull": bull.BestAsk.Price, "best_bid_bear": bear.BestBid.Price, "best_ask_bear": bear.BestAsk.Price})
	}
	return false
}

// takeOver leaves the cycle to a person (PRD 10.3): it is not closed but
// marked takeover, with what it holds and the orders without an outcome
// recorded; a later start reads it back.
func (m *ExecManager) takeOver(r *round, unresolved []cex.OrderRecord, now time.Time) {
	dir, hold := m.inv.Holdings()
	if r.cycle != nil {
		m.inv.MarkTakeover(now)
	}
	logger.GlobalLogger.Info("exec: taken over", "cycle", cycleOf(r.cycle), "direction", dir, "hold_bull", hold[cex.Bull], "hold_bear", hold[cex.Bear], "unresolved_orders", len(unresolved))
	m.event(now, "warn", "taken_over", "a person took the positions over after the stop alarm; the cycle is kept as it is",
		map[string]any{"cycle_id": cycleOf(r.cycle), "direction": dir, "hold_bull": hold[cex.Bull], "hold_bear": hold[cex.Bear], "unresolved_orders": orderIDs(unresolved)})
}

// cycleOf is the cycle's id, empty without one.
func cycleOf(c *inventory.Cycle) string {
	if c == nil {
		return ""
	}
	return string(c.ID)
}

// orderIDs lists the orders' client ids.
func orderIDs(orders []cex.OrderRecord) []cex.ClientOrderID {
	out := make([]cex.ClientOrderID, len(orders))
	for i, o := range orders {
		out[i] = o.Request.ClientOrderID
	}
	return out
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
