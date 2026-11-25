// core is the trading process of the ETF pair arbitrage: it reads the two
// legs' books, computes the spread against the anchors and trades the pair.
// See SPEC.md for the design and README.md for how to run it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/cex/binance"
	"etf-pair-arbitrage/core/cex/bitget"
	"etf-pair-arbitrage/core/cex/coinbase"
	"etf-pair-arbitrage/core/cex/direxion"
	"etf-pair-arbitrage/core/config"
	"etf-pair-arbitrage/core/exec"
	"etf-pair-arbitrage/core/inventory"
	"etf-pair-arbitrage/core/logger"
	"etf-pair-arbitrage/core/store"
	"etf-pair-arbitrage/core/strategy"
)

// main loads the configuration, starts the logger and runs the process until
// the execution manager lets it exit: SIGINT or SIGTERM asks it to stop,
// SIGUSR1 stops new entries.
func main() {
	// The process signals are taken over before anything can block. A stop
	// request that arrives during start-up stays in the channel: the process
	// finishes starting, and the execution manager, which reads the channel,
	// then lets it leave in order.
	signals := make(chan os.Signal, 8)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGUSR1)

	// Logs and their file names are in UTC+8.
	time.Local = time.FixedZone("UTC+8", 8*60*60)

	cfg, configPath, envPath := initConfig()
	defer logger.Close()
	log := logger.GlobalLogger

	log.Info("ETF-arbitrage core started", "live", cfg.Exec.Live, "bull", cfg.Cex.Pair.Bull.Symbol, "bear", cfg.Cex.Pair.Bear.Symbol,
		"config_path", configPath, "env_path", envPath, "log_level", cfg.LogLevel)
	if err := run(cfg, signals); err != nil {
		log.Error("ETF-arbitrage core exiting", "err", err)
		logger.Close()
		os.Exit(1)
	}
	log.Info("ETF-arbitrage core stopped")
}

// initConfig parses the flags, loads the configuration and starts the
// logger. Any failure is fatal: nothing can run without them. With
// -check-config it prints one line about the loaded configuration and exits.
// The default of -env serves paper mode only: a live run must name its
// credentials file on the command line.
func initConfig() (cfg *config.Config, configPath, envPath string) {
	flag.StringVar(&configPath, "config", "config.yaml", "path to the configuration (yaml)")
	flag.StringVar(&envPath, "env", config.DefaultEnvPath, "path to the secrets file (JSON); must be given when exec.live is true")
	checkConfig := flag.Bool("check-config", false, "load and validate the configuration and the secrets file, print one line and exit")
	flag.Parse()
	envGiven := false
	flag.Visit(func(f *flag.Flag) { envGiven = envGiven || f.Name == "env" })
	if !envGiven {
		envPath = "" // config.Load reads the default in paper mode and refuses to in live mode
	}

	cfg, err := config.Load(configPath, envPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}
	if !envGiven {
		envPath = config.DefaultEnvPath
	}
	if *checkConfig {
		fmt.Println(configCheckLine(configPath, envPath, cfg))
		os.Exit(0)
	}
	if err := logger.Init(config.LogDir, cfg.LogLevel, true); err != nil {
		fmt.Fprintln(os.Stderr, "logger:", err)
		os.Exit(1)
	}
	return cfg, configPath, envPath
}

// configCheckLine is what -check-config prints for a configuration that
// loaded: the values a start should be checked against, and no secret.
func configCheckLine(configPath, envPath string, cfg *config.Config) string {
	s := cfg.Strategy
	return fmt.Sprintf("config %s ok (secrets file %s): live=%t pair=%s/%s mean_model=%s mean_minutes=%d sigma_minutes=%d d_in=%v d_out=%v taker_fee_bps=%v capital=%s leverage=%s",
		configPath, envPath, cfg.Exec.Live, cfg.Cex.Pair.Bull.Symbol, cfg.Cex.Pair.Bear.Symbol,
		s.MeanModel, s.MeanMinutes, s.SigmaMinutes, s.DIn, s.DOut, s.TakerFeeBps, cfg.Inventory.Capital, cfg.Inventory.Leverage)
}

// run wires the store and the managers and blocks on the execution manager.
// signals, registered by main before anything else, is read by the execution
// manager alone, which returns when the process may exit; then the other
// managers are stopped and waited for, the stop time is written and the store
// closed.
func run(cfg *config.Config, signals chan os.Signal) error {
	log := logger.GlobalLogger

	// The store first: the run's row must be in the database before anything
	// else writes.
	host, _ := os.Hostname()
	started := time.Now()
	runID := store.RunID(fmt.Sprintf("%d-%s", started.UnixMilli(), host))
	st, err := store.New(cfg.Store, runID, cfg.Env.Store.DSN)
	if err != nil {
		return err
	}
	defer st.Close()
	go st.Run(context.Background()) // ends with st.Close
	processRun := processRunRow(cfg, host, started)
	if err := st.WriteSync(processRun); err != nil {
		return fmt.Errorf("the start of the run was not written: %w", err)
	}

	// The managers, in dependency order: cex holds the facts, inventory the
	// ledger on top of them, strategy the pure computation, exec the loop
	// that drives all three. The exchange client holds the account's key in
	// live mode only: in paper mode cfg.Env.Bitget is empty and the client
	// can read public data and nothing else. Paper mode wraps it in the
	// paper venue, which keeps the whole account side local: no order and
	// no account request reaches the exchange.
	exchange := bitget.NewClient(cfg.Env.Bitget)
	exchange.UseOrderTransport(cfg.Cex.OrderTransport)
	var venue cex.Venue = exchange
	var paper *cex.PaperVenue
	if !cfg.Exec.Live {
		paper = cex.NewPaperVenue(venue, cex.PaperConfig{Pair: cfg.Cex.Pair,
			Latency:     [2]time.Duration{cex.Bull: cfg.Exec.SimOrderLatency.Bull, cex.Bear: cfg.Exec.SimOrderLatency.Bear},
			TakerFeeBps: cfg.Strategy.TakerFeeBps, Leverage: int(cfg.Inventory.Leverage.IntPart()), DepthMaxAge: cfg.Cex.DepthMaxAge})
		paper.UseStore(st)
		venue = paper
	}
	cmgr, err := cex.NewManager(cfg.Cex, venue, binance.NewClient(), coinbase.NewClient(), direxion.NewClient(cfg.Env.Direxion.SiteAPIKey))
	if err != nil {
		return err
	}
	imgr, err := inventory.NewManager(cfg.Inventory, cfg.Cex.Pair, cmgr)
	if err != nil {
		return err
	}
	strat, err := strategy.New(cfg.Strategy)
	if err != nil {
		return err
	}
	emgr, err := exec.NewManager(cfg.Exec, cmgr, imgr, strat, signals)
	if err != nil {
		return err
	}
	cmgr.UseStore(st)
	imgr.UseStore(st)
	emgr.UseStore(st)

	// What the previous run stored is read back before any manager runs:
	// the current Session's anchor facts, so the bell anchor and the NAV's
	// settle delay continue instead of starting over, and the ledger (R,
	// the dust, the booked funding and the unfinished cycle with its lots
	// and orders).
	sessionID := cmgr.Session(started).ID
	state, err := loadState(st, sessionID, signals, cfg.Exec.StopAlarmAfter, stateLoadRetry)
	if errors.Is(err, errTakenOver) {
		processRun.StoppedMs = time.Now().UnixMilli()
		if werr := st.WriteSync(processRun); werr != nil {
			log.Error("ETF-arbitrage core: the stop time of the run was not written", "run_id", runID, "err", werr)
		}
		return nil
	}
	// The unfinished cycle's orders go into cex's order table first, which
	// gives each record this process's UpdateSeq and turns those without a
	// final state into unknown ones; the ledger is then rebuilt from the
	// table's snapshot, so both hold the same records.
	orders := cex.OrderRecordsFromRows(state.Orders)
	cmgr.Restore(cex.AnchorFactsFromRows(state.AnchorFacts), orders)
	if state.Cycle != nil {
		orders = cmgr.CycleOrders(state.Cycle.CycleID)
	}
	if err := imgr.Restore(state, orders); err != nil {
		return fmt.Errorf("the stored ledger was not restored: %w", err)
	}
	// The paper account starts with what the ledger holds, the dust
	// included: the position check then starts in agreement.
	if paper != nil {
		paper.SeedPositions(imgr.LedgerPositions())
	}
	// The loop's own: the Session's minutes, the last built entry, and the
	// recorder's per-second rows for the minutes it did not record. Its
	// recovery (reading the venue's orders and positions) runs in Run.
	emgr.Restore(state.MinuteBars, state.AnchorFacts, state.LastEntry, st)
	log.Info("ETF-arbitrage core state loaded", "session", sessionID, "anchor_facts", len(state.AnchorFacts),
		"unfinished_cycle", state.Cycle != nil, "orders", len(orders), "funding_records", len(state.Funding), "minute_bars", len(state.MinuteBars))

	mgrCtx, stopManagers := context.WithCancel(context.Background())
	var managers sync.WaitGroup
	managers.Go(func() { cmgr.Run(mgrCtx) })
	managers.Go(func() { imgr.Run(mgrCtx) })
	if cfg.Exec.Live {
		// Orders go through the exchange client: it keeps its REST
		// connection warm and, with the ws transport, its trade
		// connection up. A paper run sends it no order.
		managers.Go(func() { exchange.Run(mgrCtx) })
	}

	// Signals do not cancel anything here: the loop reads them, including
	// any that arrived since the process started, and decides when the
	// process may exit.
	err = emgr.Run(context.Background())

	// Every producer stopped, then the stop time.
	log.Info("ETF-arbitrage core stopping")
	stopManagers()
	managers.Wait()
	processRun.StoppedMs = time.Now().UnixMilli()
	if werr := st.WriteSync(processRun); werr != nil {
		log.Error("ETF-arbitrage core: the stop time of the run was not written", "run_id", runID, "err", werr)
	}
	return err
}

// stateLoadTimeout is the limit of reading the stored state at start, and
// stateLoadRetry the wait before a failed read is tried again.
const (
	stateLoadTimeout = 10 * time.Second
	stateLoadRetry   = 5 * time.Second
)

// stateLoader reads what the previous run stored (store.Store).
type stateLoader interface {
	LoadState(ctx context.Context, sessionID string) (store.State, error)
}

// errTakenOver is loadState's answer to a person taking over while the
// state cannot be read.
var errTakenOver = errors.New("taken over while the stored state could not be read")

// loadState reads the stored state, trying a failed read again every retry:
// nothing stored is taken for nothing held. The signals that arrive
// meanwhile are put back for exec, which handles them once the state is in.
// While the reads fail after a stop signal, the stop alarm is given every
// alarmAfter (PRD 10.3), and a stop signal after the alarm is a person
// taking over: the process leaves at once (errTakenOver), the stored cycle
// as it is.
func loadState(st stateLoader, sessionID string, signals chan os.Signal, alarmAfter, retry time.Duration) (store.State, error) {
	log := logger.GlobalLogger
	var kept []os.Signal
	defer func() {
		for _, sig := range kept {
			select {
			case signals <- sig:
			default: // the channel is full of signals already
			}
		}
	}()
	var stopAt, nextAlarm time.Time
	alarmed := false
	for attempt := 1; ; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), stateLoadTimeout)
		state, err := st.LoadState(ctx, sessionID)
		cancel()
		if err == nil {
			return state, nil
		}
		log.Warn("ETF-arbitrage core: the stored state was not read; retrying", "attempt", attempt, "err", err, "retry_in", retry)
		wait := time.After(retry)
	waiting:
		for {
			var alarm <-chan time.Time // nil before a stop signal: no alarm
			if !stopAt.IsZero() {
				if now := time.Now(); !now.Before(nextAlarm) {
					alarmed, nextAlarm = true, now.Add(alarmAfter)
					log.Error("ETF-arbitrage core: stopped but the stored state is not read; a further stop signal takes over", "since", now.Sub(stopAt).Round(time.Second))
				}
				alarm = time.After(time.Until(nextAlarm))
			}
			select {
			case sig := <-signals:
				switch {
				case sig == syscall.SIGUSR1 || stopAt.IsZero():
					kept = append(kept, sig)
					if sig != syscall.SIGUSR1 {
						stopAt, nextAlarm = time.Now(), time.Now().Add(alarmAfter)
					}
				case alarmed:
					kept = nil
					log.Info("ETF-arbitrage core: taken over before the stored state was read", "signal", sig.String())
					return store.State{}, errTakenOver
				}
			case <-alarm:
			case <-wait:
				break waiting
			}
		}
	}
}

// processRunRow is the process_run row of this start: the effective
// configuration, with nothing from the secrets file.
func processRunRow(cfg *config.Config, host string, started time.Time) store.ProcessRunRow {
	return store.ProcessRunRow{StartedMs: started.UnixMilli(), Host: host, Config: cfg.Effective()}
}
