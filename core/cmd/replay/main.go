// replay runs the trading program's own components - the cex manager's
// book merge on the paper venue, strategy, inventory and the exec loop - on
// the recorder's raw book frames, on a virtual clock and from one goroutine
// per simulation: the backtest and parameter selection of core/SPEC.md 5.8
// (step 8 of section 9). It reads the history read-only and writes its
// results as files; it sends nothing anywhere.
//
//	replay -sessions 2026-11-24,2026-11-25 -grid 'n=1000,10000,100000' -out DIR
//	replay -paper-run <run_id> -to 2026-11-26T11:37:00 -keep -out DIR
package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/config"
	"etf-pair-arbitrage/core/logger"
	"etf-pair-arbitrage/core/store"
	"etf-pair-arbitrage/core/strategy"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "replay:", err)
		os.Exit(1)
	}
}

// options are the command line.
type options struct {
	configPath, tape, boardEnv, tradeEnv, out string
	sessions, from, to, paperRun, grid        string
	keep                                      bool
	repeat                                    int
	logPath                                   string
}

func run(args []string) error {
	var o options
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	fs.StringVar(&o.configPath, "config", "config.yaml", "the configuration the simulations start from (yaml); -paper-run uses that run's own")
	fs.StringVar(&o.tape, "tape", "../experiments/book_tape", "the recorder's raw files")
	fs.StringVar(&o.boardEnv, "board-env", "../dashboard/.env", "the board's env file, for its read-only role's password")
	fs.StringVar(&o.tradeEnv, "trade-env", ".env.reader", "the trade schema's read-only DSN file (JSON, store.dsn), for -paper-run")
	fs.StringVar(&o.out, "out", "", "the directory the results are written to (required)")
	fs.StringVar(&o.sessions, "sessions", "", "Sessions to replay one by one, by id (New York date of the close that starts each), comma-separated")
	fs.StringVar(&o.from, "from", "", "the start of one window instead of -sessions, UTC+8 2006-01-02T15:04:05 (a Session ends at its own close)")
	fs.StringVar(&o.to, "to", "", "the end of the window (UTC+8 2006-01-02T15:04:05); with -sessions it cuts a Session short")
	fs.StringVar(&o.paperRun, "paper-run", "", "reproduce this paper run from its start: its configuration, the F it used, its anchor facts and what it restored")
	fs.StringVar(&o.grid, "grid", "", "parameter grid, e.g. 'model=ema,sma;x=30;y=30;din=2;dout=-0.5;thin=none,bear;exit2=none,-1;lat=15/15,5/5;n=1000,10000,100000' (thin: the leg taken first, none for both together; exit2: none for one exit line; lat: bull/bear ms; n: N = M x L in USDT)")
	fs.BoolVar(&o.keep, "keep", false, "also write each second's sample, every decision and every paper match")
	fs.IntVar(&o.repeat, "repeat", 1, "run every simulation this many times and require identical results (determinism)")
	fs.StringVar(&o.logPath, "log", "", "write the components' log lines (info) to this file; default: none")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if o.out == "" {
		return errors.New("-out is required")
	}
	logger.GlobalLogger = slog.New(slog.DiscardHandler)
	if o.logPath != "" {
		f, err := os.Create(o.logPath)
		if err != nil {
			return err
		}
		defer f.Close()
		logger.GlobalLogger = slog.New(slog.NewTextHandler(f, nil))
	}

	ctx := context.Background()
	base, windows, err := plan(ctx, o)
	if err != nil {
		return err
	}
	points, err := parseGrid(o.grid, base)
	if err != nil {
		return err
	}
	var rows [][]string
	for _, w := range windows {
		var sims []simConfig
		for _, p := range points {
			sc := w.sim
			sc.name, sc.cfg, sc.keepSamples = p.name, p.cfg, o.keep
			for r := range o.repeat {
				if o.repeat > 1 {
					sc.name = fmt.Sprintf("%s#%d", p.name, r+1)
				}
				sims = append(sims, sc)
			}
		}
		started := time.Now()
		done, err := runWindow(w.tape, sims)
		if err != nil {
			return fmt.Errorf("%s: %w", w.label, err)
		}
		fmt.Fprintf(os.Stderr, "%s: %d simulations in %s\n", w.label, len(done), time.Since(started).Round(time.Second))
		prints := map[string]string{}
		for _, s := range done {
			print := fingerprint(s.sink)
			name, _, repeated := strings.Cut(s.sc.name, "#")
			if repeated {
				if first, ok := prints[name]; ok && first != print {
					return fmt.Errorf("%s: %s is not deterministic: %s then %s", w.label, name, first, print)
				}
				prints[name] = print
				if !strings.HasSuffix(s.sc.name, "#1") {
					continue // the repeats are only compared
				}
			}
			dir := filepath.Join(o.out, w.label, name)
			sums, err := writeSim(dir, s, w, print)
			if err != nil {
				return err
			}
			for _, sum := range sums {
				rows = append(rows, gridRow(w, name, s, sum, print))
			}
		}
	}
	return writeGrid(filepath.Join(o.out, "grid.csv"), rows)
}

// window is one stretch of the tape and what its simulations start from.
type window struct {
	label string
	tape  tapeSpec
	sim   simConfig // the start, end, references and restored state; name and cfg come from the grid
}

// plan reads the configuration and the history and lists the windows.
func plan(ctx context.Context, o options) (*config.Config, []window, error) {
	var base *config.Config
	var cal *cex.Calendar
	var run *paperRun
	to, err := parseUTC8(o.to)
	if err != nil {
		return nil, nil, err
	}
	if o.paperRun != "" {
		if to.IsZero() {
			to = time.Now()
		}
		dsn, err := tradeDSN(o.tradeEnv)
		if err != nil {
			return nil, nil, err
		}
		if run, err = readPaperRun(ctx, dsn, o.paperRun, to); err != nil {
			return nil, nil, err
		}
		base, cal = run.cfg, run.cal
	} else {
		b, err := os.ReadFile(o.configPath)
		if err != nil {
			return nil, nil, err
		}
		if base, err = config.Parse(b); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", o.configPath, err)
		}
		// The calendar with the configuration's overrides: Session starts
		// and ids follow it.
		if cal, err = cex.NewCalendar(base.Cex.CalendarOverrides); err != nil {
			return nil, nil, err
		}
	}

	type span struct {
		label    string
		from, to time.Time
	}
	var spans []span
	switch {
	case run != nil:
		spans = append(spans, span{"paper-" + run.runID, time.UnixMilli(run.startedMs), to})
	case o.sessions != "":
		for _, id := range strings.Split(o.sessions, ",") {
			day, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(id), cex.NewYork)
			if err != nil {
				return nil, nil, fmt.Errorf("-sessions: %w", err)
			}
			s := cal.SessionAt(day.Add(20 * time.Hour)) // after that day's close
			if s.ID != strings.TrimSpace(id) {
				return nil, nil, fmt.Errorf("-sessions: %s is not a Session (that evening is in %s)", id, s.ID)
			}
			end := s.End
			if !to.IsZero() && to.Before(end) {
				end = to
			}
			spans = append(spans, span{"session-" + s.ID, s.Start, end})
		}
	default:
		from, err := parseUTC8(o.from)
		if err != nil || from.IsZero() || to.IsZero() {
			return nil, nil, errors.New("give -sessions, -paper-run, or -from and -to")
		}
		spans = append(spans, span{fmt.Sprintf("window-%s", from.In(utc8).Format("20060102T150405")), from, to})
	}

	bearBooks5, err := firstFile(o.tape, base.Cex.Pair.Bear.Symbol, cex.TopicBooks5)
	if err != nil {
		return nil, nil, err
	}
	var windows []window
	var dsn string
	if run == nil {
		if dsn, err = boardDSN(o.boardEnv); err != nil {
			return nil, nil, err
		}
	}
	for _, sp := range spans {
		w := window{label: sp.label, tape: tapeSpec{dir: o.tape, from: sp.from.UnixMilli(), to: sp.to.UnixMilli(), bull: base.Cex.Pair.Bull.Symbol, bear: base.Cex.Pair.Bear.Symbol, bearBooks5From: bearBooks5},
			sim: simConfig{start: sp.from, end: sp.to}}
		if run != nil {
			w.sim.refs, w.sim.state, w.sim.bars, w.sim.facts, w.sim.lastEntry = run.refs, run.state, run.bars, run.facts, run.lastEntry
		} else if w.sim.refs, err = boardRefs(ctx, dsn, base.Cex.Pair.Bull.Ticker, base.Cex.Pair.Bear.Ticker, base.Cex.NAVWatch, cal, sp.from, sp.to); err != nil {
			return nil, nil, err
		}
		windows = append(windows, w)
	}
	return base, windows, nil
}

// firstFile is the first received ms of the topic's recording of the
// symbol; MaxInt64 when there is none.
func firstFile(dir string, symbol cex.Symbol, topic string) (int64, error) {
	files, err := tapeFiles(dir, symbol, topic, 0, 1<<62)
	if err != nil || len(files) == 0 {
		return 1 << 62, err
	}
	parts := strings.Split(filepath.Base(files[0]), ".")
	return strconv.ParseInt(parts[len(parts)-3], 10, 64)
}

// parseUTC8 reads a UTC+8 wall-clock time; "" is the zero time.
func parseUTC8(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.ParseInLocation("2006-01-02T15:04:05", s, utc8)
}

// --- the grid ---

// gridPoint is one parameter set.
type gridPoint struct {
	name string
	cfg  *config.Config
}

// parseGrid expands the grid over the base configuration; an empty grid is
// the base alone. Keys: model (ema, sma), x and y (minutes), din and dout
// (sigmas: strategy.entry_in and strategy.entry_out; the grid keeps the short
// names), thin (bear, bull or none: exec.thin_leg, none being both legs
// together), exit2 (sigmas, or none for one exit line: strategy.exit2; SPEC
// 12.9), lat (bull/bear in ms) and n (N in USDT: M = N / L).
func parseGrid(spec string, base *config.Config) ([]gridPoint, error) {
	values := map[string][]string{}
	order := []string{"model", "x", "y", "din", "dout", "thin", "exit2", "lat", "n"}
	for _, part := range strings.Split(spec, ";") {
		if strings.TrimSpace(part) == "" {
			continue
		}
		key, list, ok := strings.Cut(part, "=")
		key = strings.TrimSpace(key)
		if !ok || !contains(order, key) {
			return nil, fmt.Errorf("-grid: %q is not key=v1,v2 with a key of %v", part, order)
		}
		values[key] = strings.Split(list, ",")
	}
	s, e := base.Strategy, base.Exec.SimOrderLatency
	n := base.Inventory.Capital.Mul(base.Inventory.Leverage)
	defaults := map[string][]string{"model": {string(s.MeanModel)}, "x": {strconv.Itoa(s.MeanMinutes)}, "y": {strconv.Itoa(s.SigmaMinutes)},
		"din": {fmtFloat(s.EntryIn)}, "dout": {fmtFloat(s.EntryOut)}, "thin": {thinName(base.Exec.ThinLeg)}, "exit2": {s.Exit2Text()}, "lat": {fmt.Sprintf("%d/%d", e.Bull.Milliseconds(), e.Bear.Milliseconds())}, "n": {n.String()}}
	for k, v := range defaults {
		if _, set := values[k]; !set {
			values[k] = v
		}
	}
	points := []map[string]string{{}}
	for _, k := range order {
		var next []map[string]string
		for _, p := range points {
			for _, v := range values[k] {
				q := map[string]string{}
				for a, b := range p {
					q[a] = b
				}
				q[k] = strings.TrimSpace(v)
				next = append(next, q)
			}
		}
		points = next
	}
	var out []gridPoint
	for _, p := range points {
		cfg := *base
		st := cfg.Strategy
		st.MeanModel = strategy.MeanModel(p["model"])
		var err error
		if st.MeanMinutes, err = strconv.Atoi(p["x"]); err != nil {
			return nil, fmt.Errorf("-grid x: %w", err)
		}
		if st.SigmaMinutes, err = strconv.Atoi(p["y"]); err != nil {
			return nil, fmt.Errorf("-grid y: %w", err)
		}
		if st.EntryIn, err = strconv.ParseFloat(p["din"], 64); err != nil {
			return nil, fmt.Errorf("-grid din: %w", err)
		}
		if st.EntryOut, err = strconv.ParseFloat(p["dout"], 64); err != nil {
			return nil, fmt.Errorf("-grid dout: %w", err)
		}
		st.Exit2 = nil
		if p["exit2"] != "none" {
			x, err := strconv.ParseFloat(p["exit2"], 64)
			if err != nil {
				return nil, fmt.Errorf("-grid exit2: %w", err)
			}
			st.Exit2 = &x
		}
		if err := st.Validate(); err != nil {
			return nil, fmt.Errorf("-grid %v: %w", p, err)
		}
		cfg.Strategy = st
		cfg.Exec.ThinLeg = p["thin"]
		if cfg.Exec.ThinLeg == "none" {
			cfg.Exec.ThinLeg = ""
		}
		if err := cfg.Exec.Validate(); err != nil {
			return nil, fmt.Errorf("-grid %v: %w", p, err)
		}
		bull, bear, ok := strings.Cut(p["lat"], "/")
		lb, err1 := strconv.Atoi(bull)
		lr, err2 := strconv.Atoi(bear)
		if !ok || err1 != nil || err2 != nil || lb <= 0 || lr <= 0 {
			return nil, fmt.Errorf("-grid lat %q: want bull/bear in ms", p["lat"])
		}
		cfg.Exec.SimOrderLatency.Bull, cfg.Exec.SimOrderLatency.Bear = time.Duration(lb)*time.Millisecond, time.Duration(lr)*time.Millisecond
		notional, err := decimal.NewFromString(p["n"])
		if err != nil || !notional.IsPositive() {
			return nil, fmt.Errorf("-grid n %q: want a positive USDT amount", p["n"])
		}
		cfg.Inventory.Capital = notional.Div(cfg.Inventory.Leverage)
		name := fmt.Sprintf("%s%s-%s_din%s_dout%s_thin%s_exit2%s_lat%s_N%s", p["model"], p["x"], p["y"], p["din"], p["dout"], p["thin"], p["exit2"], strings.ReplaceAll(p["lat"], "/", "-"), p["n"])
		out = append(out, gridPoint{name: name, cfg: &cfg})
	}
	return out, nil
}

// thinName is exec.thin_leg as the grid writes it: none for both legs
// together.
func thinName(leg string) string {
	if leg == "" {
		return "none"
	}
	return leg
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func fmtFloat(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// --- running ---

// chunkSize is how many inputs go to the simulations at a time.
const chunkSize = 4096

// runWindow decodes the window's frames once and feeds them to every
// simulation, each on its own goroutine: a simulation sees the inputs in
// arrival order and nothing else, so its results depend on its input alone.
func runWindow(spec tapeSpec, configs []simConfig) ([]*sim, error) {
	sims := make([]*sim, len(configs))
	for i, sc := range configs {
		s, err := newSim(sc)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", sc.name, err)
		}
		sims[i] = s
	}
	streams, downs, err := openTape(spec)
	if err != nil {
		return nil, err
	}
	m := newMerger(streams, downs)
	chans := make([]chan []*input, len(sims))
	var wg sync.WaitGroup
	for i, s := range sims {
		chans[i] = make(chan []*input, 32)
		wg.Go(func() {
			for chunk := range chans[i] {
				for _, in := range chunk {
					s.feed(in)
				}
			}
			s.finish()
		})
	}
	var readErr error
	for {
		chunk := make([]*input, 0, chunkSize)
		for len(chunk) < chunkSize {
			in, err := m.next()
			if err != nil {
				readErr = err
				break
			}
			if in == nil {
				break
			}
			chunk = append(chunk, in)
		}
		if len(chunk) > 0 {
			for _, c := range chans {
				c <- chunk
			}
		}
		if len(chunk) < chunkSize || readErr != nil {
			break
		}
	}
	for _, c := range chans {
		close(c)
	}
	wg.Wait()
	return sims, readErr
}

// --- writing ---

// writeSim writes one simulation's records and summaries; it returns the
// summary of each Session and of the whole window.
func writeSim(dir string, s *sim, w window, print string) ([]summary, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	n := s.sc.cfg.Inventory.Capital.Mul(s.sc.cfg.Inventory.Leverage)
	var sums []summary
	sessions := sessionsOf(s.sink)
	for i, id := range sessions {
		var end *closeout // a Session before the last one ended before the window did
		if i == len(sessions)-1 {
			end = s.closeout(w.tape)
		}
		sums = append(sums, summarize(s.sink, id, n, s.peak(id), end))
	}
	total := summarize(s.sink, "", n, s.peak(""), s.closeout(w.tape))
	total.Session = "all"
	sums = append(sums, total)
	meta := map[string]any{"name": s.sc.name, "funding": "excluded: the replay books no funding", "window": w.label, "from": showMs(w.tape.from), "to": showMs(w.tape.to), "fingerprint": print,
		"bear_depth": bearDepth(w.tape), "advance_calls": s.steps, "samples": s.sink.sampled, "idle_reasons": s.sink.idle,
		"strategy": s.sc.cfg.Strategy, "thin_leg": thinName(s.sc.cfg.Exec.ThinLeg), "exit2": s.sc.cfg.Strategy.Exit2Text(), "latency_ms": [2]int64{s.sc.cfg.Exec.SimOrderLatency.Bull.Milliseconds(), s.sc.cfg.Exec.SimOrderLatency.Bear.Milliseconds()},
		"capital": s.sc.cfg.Inventory.Capital, "leverage": s.sc.cfg.Inventory.Leverage, "summaries": sums}
	if err := writeJSON(filepath.Join(dir, "summary.json"), meta); err != nil {
		return nil, err
	}
	if err := writeRows(filepath.Join(dir, "orders.jsonl"), s.sink.orderSeq, func(id string) any { return s.sink.orders[id] }); err != nil {
		return nil, err
	}
	if err := writeRows(filepath.Join(dir, "fills.jsonl"), s.sink.fillSeq, func(id string) any { return s.sink.fills[id] }); err != nil {
		return nil, err
	}
	if err := writeRows(filepath.Join(dir, "cycles.jsonl"), s.sink.cycleSeq, func(id string) any { return s.sink.cycles[id] }); err != nil {
		return nil, err
	}
	if err := writeRows(filepath.Join(dir, "lots.jsonl"), s.sink.lotSeq, func(id string) any { return s.sink.lots[id] }); err != nil {
		return nil, err
	}
	if err := writeList(filepath.Join(dir, "decisions.jsonl"), s.sink.decisions); err != nil {
		return nil, err
	}
	if err := writeList(filepath.Join(dir, "minutes.jsonl"), s.sink.minutes); err != nil {
		return nil, err
	}
	if s.sc.keepSamples {
		if err := writeSamples(filepath.Join(dir, "samples.csv"), s.sink.samples); err != nil {
			return nil, err
		}
		if err := writeList(filepath.Join(dir, "paper_matches.jsonl"), s.sink.matches); err != nil {
			return nil, err
		}
	}
	return sums, nil
}

// bearDepth says which depth the bear leg was replayed on.
func bearDepth(t tapeSpec) string {
	switch {
	case t.bearBooks5From <= t.from:
		return "books5"
	case t.bearBooks5From >= t.to:
		return "books50 cut to 5 levels"
	default:
		return "books50 cut to 5 levels until " + showMs(t.bearBooks5From) + ", books5 after"
	}
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func writeRows(path string, ids []string, row func(string) any) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	for _, id := range ids {
		if err := enc.Encode(row(id)); err != nil {
			f.Close()
			return err
		}
	}
	return f.Close()
}

func writeList[T any](path string, rows []T) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	for _, r := range rows {
		if err := enc.Encode(r); err != nil {
			f.Close()
			return err
		}
	}
	return f.Close()
}

// writeSamples writes each second's sample as CSV; a number that is not
// there is NaN.
func writeSamples(path string, rows []store.SignalSampleRow) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := csv.NewWriter(f)
	w.Write([]string{"ts_ms", "session_id", "phase", "anchor_kind", "anchor_bull", "anchor_bear", "f", "mid_d", "mu_d", "sigma_d", "x_long_d", "x_short_d", "stats_ready",
		"idle_reason", "depth_ok_bull", "depth_ok_bear", "conflict_bull", "conflict_bear", "cycle_id", "direction", "hold_bull", "hold_bear", "quota_left", "mid_bull", "mid_bear"})
	g := func(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }
	b := strconv.FormatBool
	for _, r := range rows {
		w.Write([]string{strconv.FormatInt(r.TsMs, 10), r.SessionID, r.Phase, r.AnchorKind, r.AnchorBull.String(), r.AnchorBear.String(), g(r.F), g(r.MidD), g(r.MuD), g(r.SigmaD), g(r.XLongD), g(r.XShortD), b(r.StatsReady),
			r.IdleReason, b(r.DepthOKBull), b(r.DepthOKBear), b(r.ConflictBull), b(r.ConflictBear), r.CycleID, r.Direction, r.HoldBull.String(), r.HoldBear.String(), r.QuotaLeft.String(), g(r.MidBull), g(r.MidBear)})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

var gridHeader = []string{"window", "sim", "session", "bear_depth", "model", "x", "y", "d_in", "d_out", "thin_leg", "exit2", "lat_bull_ms", "lat_bear_ms", "n_usdt",
	"round_trip_net", "cash_net", "fees", "orders", "fills", "entry_batches", "both_legs", "one_leg", "no_fill", "one_leg_rate", "entry_fill_rate",
	"completions", "trims", "cycles", "end_reasons", "peak_notional", "used_notional", "return_on_n", "return_on_used", "open_bull", "open_bear",
	"window_value", "window_net", "window_return_on_n", "window_return_on_used",
	// SPEC 12.10 (seqStats)
	"signals", "planned_notional", "built_notional", "built_per_signal", "built_ratio",
	"es_entry_failed", "es_built", "es_built_partial", "es_unwound_profit", "es_unwound_stop", "es_building",
	"exposed_batches", "exposure_median_s", "exposure_max_s", "complete_slip", "unwind_net", "profit_close_net",
	"d1_batches", "d1_net", "d2_batches", "d2_net", "d2_expected",
	"closed_exit", "closed_exit1", "closed_exit2", "closed_forced", "net_exit", "net_exit1", "net_exit2", "net_forced", "stages_set", "stages_done",
	"line_to_flat_cycles", "line_to_flat_median_s", "line_to_flat_max_s", "line_to_flat_usdt_h", "realised_check",
	"exit_batches", "exit_batch_max_s", "refused", "refused_reduce_qty", "residual_cycles", "residual_bull", "residual_bear",
	"fingerprint"}

// gridRow is one summary as a row of grid.csv.
func gridRow(w window, name string, s *sim, sum summary, print string) []string {
	st, lat := s.sc.cfg.Strategy, s.sc.cfg.Exec.SimOrderLatency
	var reasons []string
	for k, v := range sum.EndReasons {
		reasons = append(reasons, fmt.Sprintf("%s:%d", k, v))
	}
	sort.Strings(reasons)
	q := sum.Seq
	perSignal, ratio := decimal.Zero, decimal.Zero
	if q.Signals > 0 {
		perSignal = q.BuiltNotional.Div(decimal.NewFromInt(int64(q.Signals)))
	}
	if q.PlannedNotional.IsPositive() {
		ratio = q.BuiltNotional.Div(q.PlannedNotional)
	}
	seq := []string{strconv.Itoa(q.Signals), q.PlannedNotional.String(), q.BuiltNotional.StringFixed(8), perSignal.StringFixed(8), ratio.StringFixed(6)}
	for _, state := range []string{"entry_failed", "built", "built_partial", "unwound_profit", "unwound_stop", "building"} {
		seq = append(seq, strconv.Itoa(q.EndStates[state]))
	}
	seq = append(seq, strconv.Itoa(q.ExposedBatches), fmtFloat(q.ExposureMedianS), fmtFloat(q.ExposureMaxS), q.CompleteSlip.String(), q.UnwindNet.String(), q.ProfitCloseNet.String(),
		strconv.Itoa(q.D1Batches), q.D1Net.String(), strconv.Itoa(q.D2Batches), q.D2Net.String(), q.D2Expected.String())
	for _, m := range []map[string]decimal.Decimal{q.ClosedByLine, q.NetByLine} {
		for _, line := range []string{lineOne, "exit1", "exit2", lineForced} {
			seq = append(seq, m[line].String())
		}
	}
	seq = append(seq, strconv.Itoa(q.StagesSet), strconv.Itoa(q.StagesDone),
		strconv.Itoa(q.LineToFlatCycles), fmtFloat(q.LineToFlatMedianS), fmtFloat(q.LineToFlatMaxS), strconv.FormatFloat(q.LineToFlatUSDTh, 'f', 4, 64), q.RealisedCheck.String(),
		strconv.Itoa(q.ExitBatches), fmtFloat(q.ExitBatchMaxS), strconv.Itoa(q.Refused), strconv.Itoa(q.RefusedReduceQty),
		strconv.Itoa(q.ResidualCycles), q.ResidualQty[0].String(), q.ResidualQty[1].String())
	row := []string{w.label, name, sum.Session, bearDepth(w.tape), string(st.MeanModel), strconv.Itoa(st.MeanMinutes), strconv.Itoa(st.SigmaMinutes), fmtFloat(st.EntryIn), fmtFloat(st.EntryOut),
		thinName(s.sc.cfg.Exec.ThinLeg), st.Exit2Text(),
		strconv.FormatInt(lat.Bull.Milliseconds(), 10), strconv.FormatInt(lat.Bear.Milliseconds(), 10), sum.Notional.String(),
		sum.RoundTripNet.String(), sum.CashNet.String(), sum.Fees.String(), strconv.Itoa(sum.Orders), strconv.Itoa(sum.Fills),
		strconv.Itoa(sum.EntryBatches), strconv.Itoa(sum.BothLegs), strconv.Itoa(sum.OneLeg), strconv.Itoa(sum.NoFill), fmtFloat(sum.OneLegRate), fmtFloat(sum.EntryFillRate),
		strconv.Itoa(sum.Completions), strconv.Itoa(sum.Trims), strconv.Itoa(sum.Cycles), strings.Join(reasons, " "),
		sum.PeakNotional.String(), sum.UsedNotional.String(), sum.ReturnOnN.StringFixed(8), sum.ReturnOnUsed.StringFixed(8), sum.OpenQty[0].String(), sum.OpenQty[1].String(),
		sum.WindowValue, windowNumber(sum, sum.WindowNet.String()), windowNumber(sum, sum.WindowReturnOnN.StringFixed(8)), windowNumber(sum, sum.WindowReturnOnUsed.StringFixed(8))}
	return append(append(row, seq...), print)
}

// windowNumber is a window result as written in grid.csv: empty when the
// window has no value (a position the books at the end could not close),
// so that it cannot be ranked as a zero.
func windowNumber(sum summary, text string) string {
	if sum.WindowValue == valueNone {
		return ""
	}
	return text
}

func writeGrid(path string, rows [][]string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := csv.NewWriter(f)
	w.Write(gridHeader)
	w.WriteAll(rows)
	if err := w.Error(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
