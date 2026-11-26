package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"etf-pair-arbitrage/core/cex"
	"etf-pair-arbitrage/core/config"
	"etf-pair-arbitrage/core/store"
)

// The replay reads the history read-only: the board's database (F as the
// board's collector sampled it, its bell anchors and NAVs by known time)
// and, to reproduce a paper run, that run's own rows in schema trade (its
// configuration, the F it used each second, its anchor facts, and what it
// restored at its start). The DSNs are built from the env files inside the
// program and never printed.

// boardDSN is the board's read-only role on the local database; the
// password is ETFPAIR_DB_READER_PASSWORD in dashboard/.env (KEY=VALUE lines).
func boardDSN(envPath string) (string, error) {
	f, err := os.Open(envPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, value, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if ok && key == "ETFPAIR_DB_READER_PASSWORD" {
			u := url.URL{Scheme: "postgres", User: url.UserPassword("etfpair_reader", strings.Trim(value, `"'`)), Host: "127.0.0.1:5432", Path: "etfpair"}
			return u.String(), nil
		}
	}
	return "", fmt.Errorf("replay: %s has no ETFPAIR_DB_READER_PASSWORD", envPath)
}

// tradeDSN is the trade schema's read-only role: store.dsn of the JSON file
// core/.env.reader.
func tradeDSN(envPath string) (string, error) {
	b, err := os.ReadFile(envPath)
	if err != nil {
		return "", err
	}
	var env struct {
		Store struct {
			DSN string `json:"dsn"`
		} `json:"store"`
	}
	if err := json.Unmarshal(b, &env); err != nil || env.Store.DSN == "" {
		return "", fmt.Errorf("replay: %s: not JSON with store.dsn", envPath)
	}
	return env.Store.DSN, nil
}

// sessionStarts are the empty anchors each Session starts with, as the
// anchor loop publishes them at its close: no anchor until its bell.
func sessionStarts(cal *cex.Calendar, from, to time.Time) []refEvent {
	var out []refEvent
	for s := cal.SessionAt(from); s.Start.Before(to); s = cal.SessionAt(s.End) {
		if !s.Start.Before(from) {
			out = append(out, refEvent{at: s.Start.UnixMilli(), anchors: &cex.Anchors{SessionID: s.ID}})
		}
	}
	return out
}

// pairAnchor is one leg pair of anchor values, usable from known.
type pairAnchor struct {
	session    string
	kind       cex.AnchorKind
	bull, bear decimal.Decimal
	known      int64
}

// anchorRefs turns the pairs into anchor publications.
func anchorRefs(pairs []pairAnchor) []refEvent {
	var out []refEvent
	for _, p := range pairs {
		a := cex.Anchors{SessionID: p.session, Kind: p.kind, Bull: p.bull, Bear: p.bear, KnownAt: time.UnixMilli(p.known)}
		out = append(out, refEvent{at: p.known, anchors: &a})
	}
	return out
}

// sortRefs orders the changes by time; at the same time the anchors before F.
func sortRefs(refs []refEvent) {
	sort.SliceStable(refs, func(i, j int) bool {
		if refs[i].at != refs[j].at {
			return refs[i].at < refs[j].at
		}
		return refs[i].anchors != nil && refs[j].anchors == nil
	})
}

// --- the board ---

// boardRefs reads F and the anchors of [from, to) from the board's
// database: F from fx_minute (the collector's sample of each minute, at its
// sample time), the bell anchor from etf_bell and the NAV from etf_nav, each
// pair known when its later leg was.
func boardRefs(ctx context.Context, dsn string, bull, bear string, cal *cex.Calendar, from, to time.Time) ([]refEvent, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("replay: board database: %w", err)
	}
	defer conn.Close(ctx)
	refs := sessionStarts(cal, from, to)

	rows, err := conn.Query(ctx, `select sampled_ms, price, coalesce(bid, price), coalesce(ask, price) from etfpair.fx_minute
		where pair = 'USDT-USD' and sampled_ms >= $1 and sampled_ms < $2 order by sampled_ms`, from.Add(-2*time.Minute).UnixMilli(), to.UnixMilli())
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var at int64
		var price, bid, ask decimal.Decimal
		if err := rows.Scan(&at, &price, &bid, &ask); err != nil {
			return nil, err
		}
		fx := cex.FX{Price: price, Bid: bid, Ask: ask, ReceivedAt: time.UnixMilli(at)}
		refs = append(refs, refEvent{at: at, fx: &fx})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var sessions []string
	for s := cal.SessionAt(from); s.Start.Before(to); s = cal.SessionAt(s.End) {
		sessions = append(sessions, s.ID)
	}
	var pairs []pairAnchor
	for _, q := range []struct {
		kind cex.AnchorKind
		sql  string
	}{
		{cex.AnchorBell, `select ticker, price, known_ms from etfpair.etf_bell where session_date = $1::date and ticker = any($2)`},
		// The first confirmation of each fund's NAV of the day.
		{cex.AnchorNAV, `select distinct on (ticker) ticker, nav, confirmed_ms from etfpair.etf_nav where session_date = $1::date and ticker = any($2) order by ticker, confirmed_ms`},
	} {
		for _, id := range sessions {
			p, ok, err := readPair(ctx, conn, q.sql, id, bull, bear)
			if err != nil {
				return nil, err
			}
			if ok {
				p.kind = q.kind
				pairs = append(pairs, p)
			}
		}
	}
	refs = append(refs, anchorRefs(pairs)...)
	sortRefs(refs)
	return refs, nil
}

// readPair reads one value per fund of a Session; ok when both are there.
func readPair(ctx context.Context, conn *pgx.Conn, sql, session, bull, bear string) (pairAnchor, bool, error) {
	rows, err := conn.Query(ctx, sql, session, []string{bull, bear})
	if err != nil {
		return pairAnchor{}, false, err
	}
	p := pairAnchor{session: session}
	got := 0
	for rows.Next() {
		var ticker string
		var value decimal.Decimal
		var known int64
		if err := rows.Scan(&ticker, &value, &known); err != nil {
			return pairAnchor{}, false, err
		}
		if ticker == bull {
			p.bull = value
		} else {
			p.bear = value
		}
		p.known = max(p.known, known)
		got++
	}
	return p, got == 2, rows.Err()
}

// --- a paper run ---

// paperRun is what a paper run had and knew, from its rows in schema trade.
type paperRun struct {
	runID     string
	startedMs int64
	cfg       *config.Config // process_run.config: the effective configuration
	cal       *cex.Calendar  // the program's calendar with that configuration's overrides
	refs      []refEvent
	state     store.State // the ledger it restored at its start: R and the dust
	bars      []store.MinuteBarRow
	facts     []store.AnchorFactRow
	lastEntry *store.LotRow
}

// readPaperRun reads the run's configuration and start, the F it used (each
// second's signal_sample, as a reading at that second), its anchor facts by
// known time, and what it restored at its start: the Session's minute bars
// before it, the anchor facts known by then, the last entry lot, and R as
// the closed cycles of its ledger before its start.
//
// It refuses a run whose start the replay cannot reproduce (checkRestart).
func readPaperRun(ctx context.Context, dsn, runID string, to time.Time) (*paperRun, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("replay: trade database: %w", err)
	}
	defer conn.Close(ctx)
	run := &paperRun{runID: runID}
	var live bool
	var stored []byte
	if err := conn.QueryRow(ctx, `select started_ms, live, config::text from trade.process_run where run_id = $1`, runID).Scan(&run.startedMs, &live, &stored); err != nil {
		return nil, fmt.Errorf("replay: process_run %s: %w", runID, err)
	}
	if live {
		return nil, fmt.Errorf("replay: %s is a live run", runID)
	}
	if run.cfg, err = config.Parse(stored); err != nil {
		return nil, fmt.Errorf("replay: the run's stored configuration: %w", err)
	}
	if run.cal, err = cex.NewCalendar(run.cfg.Cex.CalendarOverrides); err != nil {
		return nil, err
	}
	cal := run.cal
	start := time.UnixMilli(run.startedMs)
	session := cal.SessionAt(start)
	run.refs = sessionStarts(cal, start, to)

	// F as the loop used it, each second.
	rows, err := conn.Query(ctx, `select ts_ms, f from trade.signal_sample where run_id = $1 and not live and f is not null and ts_ms < $2 order by ts_ms`, runID, to.UnixMilli())
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var at int64
		var f float64
		if err := rows.Scan(&at, &f); err != nil {
			return nil, err
		}
		price := decimal.NewFromFloat(f)
		fx := cex.FX{Price: price, Bid: price, Ask: price, ReceivedAt: time.UnixMilli(at)}
		run.refs = append(run.refs, refEvent{at: at, fx: &fx})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// The anchor facts of the Sessions from the start on, by known time.
	rows, err = conn.Query(ctx, `select session_id, kind, leg, known_ms, value, coalesce(f, 0), source, coalesce(detail::text, 'null') from trade.anchor_fact
		where not live and session_id >= $1 and known_ms < $2 order by known_ms`, session.ID, to.UnixMilli())
	if err != nil {
		return nil, err
	}
	pairs := map[string]*pairAnchor{}
	var order []string
	for rows.Next() {
		var f store.AnchorFactRow
		var detail string
		if err := rows.Scan(&f.SessionID, &f.Kind, &f.Leg, &f.KnownMs, &f.Value, &f.F, &f.Source, &detail); err != nil {
			return nil, err
		}
		f.Detail = json.RawMessage(detail)
		if f.KnownMs <= run.startedMs && f.SessionID == session.ID {
			run.facts = append(run.facts, f)
		}
		if f.Kind != string(cex.AnchorBell) && f.Kind != string(cex.AnchorNAV) {
			continue
		}
		key := f.SessionID + "/" + f.Kind
		p := pairs[key]
		if p == nil {
			p = &pairAnchor{session: f.SessionID, kind: cex.AnchorKind(f.Kind)}
			pairs[key] = p
			order = append(order, key)
		}
		if f.Leg == cex.Bull.String() {
			p.bull = f.Value
		} else {
			p.bear = f.Value
		}
		p.known = max(p.known, f.KnownMs)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var list []pairAnchor
	for _, key := range order {
		if p := pairs[key]; !p.bull.IsZero() && !p.bear.IsZero() {
			list = append(list, *p)
		}
	}
	run.refs = append(run.refs, anchorRefs(list)...)
	sortRefs(run.refs)

	// The minutes of its Session it had recorded before it started.
	rows, err = conn.Query(ctx, `select minute_ms, session_id, coalesce(mid_bull, 0), coalesce(mid_bear, 0), coalesce(f, 0), valid, source from trade.minute_bar
		where not live and session_id = $1 and minute_ms < $2 order by minute_ms`, session.ID, run.startedMs)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var b store.MinuteBarRow
		if err := rows.Scan(&b.MinuteMs, &b.SessionID, &b.MidBull, &b.MidBear, &b.F, &b.Valid, &b.Source); err != nil {
			return nil, err
		}
		run.bars = append(run.bars, b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// The last entry lot before the start, for the spacing rule.
	var last store.LotRow
	err = conn.QueryRow(ctx, `select lot_id, opened_ms, coalesce(x_d, 0), direction from trade.lot where not live and opened_ms < $1 order by opened_ms desc limit 1`, run.startedMs).
		Scan(&last.LotID, &last.OpenedMs, &last.XD, &last.Direction)
	switch {
	case err == nil:
		run.lastEntry = &last
	case err != pgx.ErrNoRows:
		return nil, err
	}

	// R at the start: the ledger's start and capital as stored, and the net
	// of its cycles closed before the start (no funding was booked).
	var inv store.InventoryStateRow
	var dust string
	if err := conn.QueryRow(ctx, `select capital, since_ms, coalesce(dust::text, '{}') from trade.inventory_state where not live`).Scan(&inv.Capital, &inv.SinceMs, &dust); err != nil {
		return nil, fmt.Errorf("replay: inventory_state: %w", err)
	}
	var open []string
	rows, err = conn.Query(ctx, `select cycle_id from trade.cycle where not live and opened_ms < $1 and (closed_ms is null or closed_ms > $1) order by opened_ms`, run.startedMs)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		open = append(open, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := checkRestart(inv.SinceMs, run.startedMs, open); err != nil {
		return nil, fmt.Errorf("replay: %s: %w", runID, err)
	}
	inv.Dust = json.RawMessage(dust)
	if err := conn.QueryRow(ctx, `select coalesce(sum(net), 0) from trade.cycle where not live and closed_ms >= $1 and closed_ms < $2`, inv.SinceMs, run.startedMs).Scan(&inv.RClosed); err != nil {
		return nil, err
	}
	run.state.Inventory = &inv
	return run, nil
}

// checkRestart refuses a paper run's start the replay cannot reproduce:
// the stored ledger (inventory_state, a state row) must have started before
// the run did - a later start means it was replaced since and R at the run's
// start is not known - and no cycle may be open across the start, because
// the replay restores no open cycle.
func checkRestart(sinceMs, startedMs int64, openAcross []string) error {
	if sinceMs > startedMs {
		return fmt.Errorf("the stored inventory_state started at %s, after the run's start %s: R at the start is not known", showMs(sinceMs), showMs(startedMs))
	}
	if len(openAcross) > 0 {
		return fmt.Errorf("cycles %v were open across the run's start %s; the replay restores no open cycle", openAcross, showMs(startedMs))
	}
	return nil
}
