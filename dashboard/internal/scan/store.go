package scan

import (
	"context"
	_ "embed"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schemaSQL string

// store writes the board's tables. Prices travel as decimal strings and are
// cast to NUMERIC by the statements.
type store struct {
	pool *pgxpool.Pool
}

// openStore connects and applies the schema, which is idempotent.
func openStore(ctx context.Context, dsn string) (*store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if _, err := pool.Exec(ctx, schemaSQL, pgx.QueryExecModeSimpleProtocol); err != nil {
		pool.Close()
		return nil, err
	}
	return &store{pool: pool}, nil
}

// seed writes the configured combos, their markets and the fee levels, so
// the board's pickers follow the configuration.
func (s *store) seed(ctx context.Context, cfg Config) error {
	batch := &pgx.Batch{}
	for _, c := range cfg.Combos {
		batch.Queue(`INSERT INTO etfpair.combo (combo_id, title, bull_ticker, bear_ticker, ref_ticker) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (combo_id) DO UPDATE SET title = EXCLUDED.title, bull_ticker = EXCLUDED.bull_ticker, bear_ticker = EXCLUDED.bear_ticker,
			ref_ticker = EXCLUDED.ref_ticker`, c.ID, c.Title, c.BullTicker, c.BearTicker, c.RefTicker)
		for _, m := range c.Markets {
			batch.Queue(`INSERT INTO etfpair.combo_market (combo_id, venue, bull_symbol, bear_symbol, ref_symbol, bull_tick, bear_tick)
				VALUES ($1, $2, $3, $4, $5, NULLIF($6, '')::numeric, NULLIF($7, '')::numeric)
				ON CONFLICT (combo_id, venue) DO UPDATE SET bull_symbol = EXCLUDED.bull_symbol, bear_symbol = EXCLUDED.bear_symbol,
				ref_symbol = EXCLUDED.ref_symbol, bull_tick = EXCLUDED.bull_tick, bear_tick = EXCLUDED.bear_tick`,
				c.ID, m.Venue, m.BullSymbol, m.BearSymbol, m.RefSymbol, m.BullTick, m.BearTick)
		}
	}
	batch.Queue(`DELETE FROM etfpair.fee_level`)
	for i, f := range cfg.FeeLevels {
		batch.Queue(`INSERT INTO etfpair.fee_level (venue, level, label, taker_bps, maker_bps, source, sort)
			VALUES ($1, $2, $3, $4::numeric, $5::numeric, $6, $7)`,
			f.Venue, f.Level, f.Label, f.TakerBps, f.MakerBps, f.Source, i)
	}
	return s.pool.SendBatch(ctx, batch).Close()
}

// historyFrom is where a series' backfill has to start: at start when the
// table holds none of it from start on, otherwise a quarter of an hour before
// the end of its first unbroken run of minutes (contracts) or before its
// newest minute (USD per USDT, which has minutes without a trade). A series
// whose oldest minute is later than start is a contract listed after start:
// the venue has nothing before that minute, so it is not asked for again.
// where names the series; its last placeholder is start.
func (s *store) historyFrom(ctx context.Context, table, where string, unbroken bool, start int64, keys ...any) int64 {
	var last *int64
	err := s.pool.QueryRow(ctx, `WITH m AS (SELECT minute_ms, lead(minute_ms) OVER (ORDER BY minute_ms) AS next_ms FROM etfpair.`+table+` WHERE `+where+`)
		SELECT min(minute_ms) FROM m WHERE next_ms IS NULL OR ($1 AND next_ms - minute_ms > 60000)`,
		append([]any{unbroken}, append(keys, start)...)...).Scan(&last)
	if err != nil || last == nil {
		return start
	}
	return max(start, *last-15*minuteMs)
}

// insertCandles stores candle closes; a minute whose stored close differs,
// read while the exchange was still settling the candle, takes the new one.
func (s *store) insertCandles(ctx context.Context, venue, symbol string, candles []Candle, sampledMs int64) error {
	batch := &pgx.Batch{}
	for _, c := range candles {
		batch.Queue(`INSERT INTO etfpair.perp_minute AS m (venue, symbol, minute_ms, price, sampled_ms)
			VALUES ($1, $2, $3, $4::numeric, $5)
			ON CONFLICT (venue, symbol, minute_ms) DO UPDATE SET price = EXCLUDED.price, sampled_ms = EXCLUDED.sampled_ms
			WHERE m.price <> EXCLUDED.price`, venue, symbol, c.MinuteMs, c.Close, sampledMs)
	}
	return s.pool.SendBatch(ctx, batch).Close()
}

// insertFXCandles adds candle closes of USD per USDT for minutes without a
// row; upsertFXQuote stores the newest bid and ask sampled inside a minute,
// whose mid stays as the minute's price.
func (s *store) insertFXCandles(ctx context.Context, pair string, candles []Candle, sampledMs int64) error {
	batch := &pgx.Batch{}
	for _, c := range candles {
		batch.Queue(`INSERT INTO etfpair.fx_minute (pair, minute_ms, price, source, sampled_ms)
			VALUES ($1, $2, $3::numeric, 'candle', $4) ON CONFLICT DO NOTHING`, pair, c.MinuteMs, c.Close, sampledMs)
	}
	return s.pool.SendBatch(ctx, batch).Close()
}

func (s *store) upsertFXQuote(ctx context.Context, pair string, minuteMs int64, q Quote, sampledMs int64) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO etfpair.fx_minute (pair, minute_ms, price, bid, ask, source, sampled_ms)
		VALUES ($1, $2, ($3::numeric + $4::numeric) / 2, $3::numeric, $4::numeric, 'ticker', $5)
		ON CONFLICT (pair, minute_ms) DO UPDATE
		SET price = EXCLUDED.price, bid = EXCLUDED.bid, ask = EXCLUDED.ask, source = 'ticker', sampled_ms = EXCLUDED.sampled_ms`,
		pair, minuteMs, q.Bid, q.Ask, sampledMs)
	return err
}

// writeCalendar replaces the sessions and the phases with the calendar's.
func (s *store) writeCalendar(ctx context.Context, sessions []session, phases []Phase) error {
	batch := &pgx.Batch{}
	batch.Queue(`DELETE FROM etfpair.etf_session`)
	for _, ss := range sessions {
		batch.Queue(`INSERT INTO etfpair.etf_session (session_date, close_ms) VALUES ($1::date, $2)`, ss.Date, ss.CloseMs)
	}
	batch.Queue(`DELETE FROM etfpair.etf_phase`)
	for _, p := range phases {
		batch.Queue(`INSERT INTO etfpair.etf_phase (start_ms, end_ms, phase) VALUES ($1, $2, $3)`, p.Start.UnixMilli(), p.End.UnixMilli(), p.Name)
	}
	return s.pool.SendBatch(ctx, batch).Close()
}

// bellsDue lists the sessions that closed at least bellDelay before nowMs
// and have no valid bell anchor of the ticker yet, the newest first: no row,
// or a row read at another close (its kline is not the minute before this
// close: the close moved after it was read).
func (s *store) bellsDue(ctx context.Context, ticker string, nowMs int64) ([]session, error) {
	rows, err := s.pool.Query(ctx, `SELECT to_char(ss.session_date, 'YYYY-MM-DD'), ss.close_ms
		FROM etfpair.etf_session ss
		WHERE ss.close_ms + $3 <= $2
		  AND NOT EXISTS (SELECT 1 FROM etfpair.etf_bell b WHERE b.ticker = $1 AND b.session_date = ss.session_date AND b.kline_ms = ss.close_ms - 60000)
		ORDER BY ss.close_ms DESC`, ticker, nowMs, bellDelay.Milliseconds())
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (session, error) {
		var b session
		err := row.Scan(&b.Date, &b.CloseMs)
		return b, err
	})
}

// insertBell stores a session's bell anchor of a ticker: the close of the
// venue's index kline k times F, the close of Coinbase's candle of the same
// minute, known at knownMs; it replaces a row read at another close (the
// close moved after it was read), and the replacement is known at nowMs,
// when it is written. It returns the anchor, or "" when the row was there.
func (s *store) insertBell(ctx context.Context, ticker, sessionDate, venue string, k Candle, fx string, knownMs, nowMs int64) (string, error) {
	var price string
	err := s.pool.QueryRow(ctx, `INSERT INTO etfpair.etf_bell AS b (ticker, session_date, price, index_usdt, venue, fx, kline_ms, known_ms)
		VALUES ($1, $2::date, trim_scale($8::numeric * $3::numeric), $3::numeric, $6, trim_scale($8::numeric), $4, $5)
		ON CONFLICT (ticker, session_date) DO UPDATE SET price = EXCLUDED.price, index_usdt = EXCLUDED.index_usdt, venue = EXCLUDED.venue,
		  fx = EXCLUDED.fx, kline_ms = EXCLUDED.kline_ms, known_ms = $7
		WHERE b.kline_ms <> EXCLUDED.kline_ms
		RETURNING price::text`, ticker, sessionDate, k.Close, k.MinuteMs, knownMs, venue, nowMs, fx).Scan(&price)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return price, err
}

// insertNAVObs records each NAV figure at the time it was read: a new
// figure with that time as its first and last read, a known one with a new
// last read.
func (s *store) insertNAVObs(ctx context.Context, navs []NAV) error {
	batch := &pgx.Batch{}
	for _, n := range navs {
		batch.Queue(`INSERT INTO etfpair.etf_nav_obs (issuer, ticker, session_date, nav, received_ms, seen_ms, via)
			VALUES ($1, $2, $3::date, $4::numeric, $5, $5, $6)
			ON CONFLICT (issuer, ticker, session_date, nav) DO UPDATE SET seen_ms = EXCLUDED.seen_ms`,
			n.Issuer, n.Ticker, n.SessionDate, n.Price, n.ReadMs, n.Via)
	}
	return s.pool.SendBatch(ctx, batch).Close()
}

// navObs lists the NAV figures on record of an issuer, ticker and day.
func (s *store) navObs(ctx context.Context, issuer, ticker, sessionDate string) ([]navObs, error) {
	rows, err := s.pool.Query(ctx, `SELECT nav::text, received_ms, seen_ms, via FROM etfpair.etf_nav_obs
		WHERE issuer = $1 AND ticker = $2 AND session_date = $3::date`, issuer, ticker, sessionDate)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[navObs])
}

// confirmNAV makes a stable NAV the anchor of its ticker's session at nowMs:
// a new row, or a row confirmed before and since replaced by a correction,
// confirmed again. It reports whether the anchor in use changed.
func (s *store) confirmNAV(ctx context.Context, n NAV, nowMs int64) (bool, error) {
	tag, err := s.pool.Exec(ctx, `INSERT INTO etfpair.etf_nav AS a (ticker, session_date, nav, confirmed_ms, issuer, via)
		VALUES ($1, $2::date, $3::numeric, $4, $5, $6)
		ON CONFLICT (ticker, session_date, nav) DO UPDATE SET confirmed_ms = EXCLUDED.confirmed_ms
		WHERE a.confirmed_ms < (SELECT max(e.confirmed_ms) FROM etfpair.etf_nav e WHERE e.ticker = $1 AND e.session_date = $2::date)`,
		n.Ticker, n.SessionDate, n.Price, nowMs, n.Issuer, n.Via)
	return tag.RowsAffected() == 1, err
}

// insertPastNAVs records NAVs of past days read from an issuer's history
// and confirms each at once, at loadMs, for a session that has no NAV
// anchor of that ticker: their publication time is not known.
func (s *store) insertPastNAVs(ctx context.Context, navs []NAV, loadMs int64) error {
	if err := s.insertNAVObs(ctx, navs); err != nil {
		return err
	}
	batch := &pgx.Batch{}
	for _, n := range navs {
		batch.Queue(`INSERT INTO etfpair.etf_nav (ticker, session_date, nav, confirmed_ms, issuer, via)
			SELECT $1, $2::date, $3::numeric, $4, $5, $6
			WHERE NOT EXISTS (SELECT 1 FROM etfpair.etf_nav e WHERE e.ticker = $1 AND e.session_date = $2::date)`,
			n.Ticker, n.SessionDate, n.Price, loadMs, n.Issuer, n.Via)
	}
	return s.pool.SendBatch(ctx, batch).Close()
}

// sessionsWithoutNAV lists the sessions that closed before beforeMs and
// have no NAV anchor of the ticker, the newest first.
func (s *store) sessionsWithoutNAV(ctx context.Context, ticker string, beforeMs int64) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT to_char(ss.session_date, 'YYYY-MM-DD') FROM etfpair.etf_session ss
		WHERE ss.close_ms < $2
		  AND NOT EXISTS (SELECT 1 FROM etfpair.etf_nav n WHERE n.ticker = $1 AND n.session_date = ss.session_date)
		ORDER BY ss.close_ms DESC`, ticker, beforeMs)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// hasNAV reports whether both tickers have a NAV anchor for the session.
func (s *store) hasNAV(ctx context.Context, bull, bear, sessionDate string) (bool, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(DISTINCT ticker) FROM etfpair.etf_nav WHERE session_date = $1::date AND ticker IN ($2, $3)`,
		sessionDate, bull, bear).Scan(&n)
	return n == 2, err
}

// actions lists every corporate action reading on record.
func (s *store) actions(ctx context.Context) ([]Action, error) {
	rows, err := s.pool.Query(ctx, `SELECT ticker, to_char(ex_date, 'YYYY-MM-DD'), kind, coalesce(value::text, ''), status, source, url, known_ms
		FROM etfpair.etf_action`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Action])
}

// insertActions puts readings on record, each known at the time it was read.
func (s *store) insertActions(ctx context.Context, actions []Action) error {
	batch := &pgx.Batch{}
	for _, a := range actions {
		batch.Queue(`INSERT INTO etfpair.etf_action (ticker, ex_date, kind, value, status, source, url, known_ms)
			VALUES ($1, $2::date, $3, nullif($4, '')::numeric, $5, $6, $7, $8) ON CONFLICT DO NOTHING`,
			a.Ticker, a.ExDate, a.Kind, a.Value, a.Status, a.Source, a.URL, a.ReadMs)
	}
	return s.pool.SendBatch(ctx, batch).Close()
}

// scheduledActions lists the actions in use (the newest row of each) that
// are Scheduled: their date known, their amount not yet.
func (s *store) scheduledActions(ctx context.Context) ([]Action, error) {
	rows, err := s.pool.Query(ctx, `SELECT ticker, to_char(ex_date, 'YYYY-MM-DD'), kind, '', status, source, url, known_ms FROM (
		  SELECT DISTINCT ON (ticker, ex_date, kind) * FROM etfpair.etf_action ORDER BY ticker, ex_date, kind, known_ms DESC) a
		WHERE status = 'Scheduled'`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Action])
}

// actionsInEffect lists the Known actions of a combo's ETFs that took effect
// on the venue before beforeMs (combo_actions).
func (s *store) actionsInEffect(ctx context.Context, combo Combo, venue string, beforeMs int64) ([]actionCheck, error) {
	rows, err := s.pool.Query(ctx, `SELECT leg, ticker, to_char(session_date, 'YYYY-MM-DD'), to_char(ex_date, 'YYYY-MM-DD'), kind, value::text,
		  effective_ms, halt_ms, anchor::text, expected::text
		FROM etfpair.combo_actions($1, $2) WHERE status = 'Known' AND effective_ms <= $3 ORDER BY effective_ms`, combo.ID, venue, beforeMs)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (actionCheck, error) {
		a := actionCheck{Combo: combo, Venue: venue}
		err := row.Scan(&a.Leg, &a.Ticker, &a.Session, &a.ExDate, &a.Kind, &a.Value, &a.EffectiveMs, &a.HaltMs, &a.Anchor, &a.Expected)
		return a, err
	})
}

// finalChecks names the checks on record that are done: Verified or Mismatch.
func (s *store) finalChecks(ctx context.Context) (map[string]bool, error) {
	rows, err := s.pool.Query(ctx, `SELECT ticker || ' ' || to_char(ex_date, 'YYYY-MM-DD') || ' ' || kind || ' ' || venue FROM etfpair.etf_action_check
		WHERE status <> 'Applied'`)
	if err != nil {
		return nil, err
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	done := map[string]bool{}
	for _, k := range keys {
		done[k] = true
	}
	return done, err
}

// upsertCheck stores the outcome of a check, replacing an earlier Applied.
func (s *store) upsertCheck(ctx context.Context, a actionCheck, venue, status, observed string, checkedMs int64) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO etfpair.etf_action_check (ticker, ex_date, kind, venue, effective_ms, status, observed, checked_ms)
		VALUES ($1, $2::date, $3, $4, $5, $6, nullif($7, '')::numeric, $8)
		ON CONFLICT (ticker, ex_date, kind, venue) DO UPDATE SET effective_ms = EXCLUDED.effective_ms, status = EXCLUDED.status,
		  observed = EXCLUDED.observed, checked_ms = EXCLUDED.checked_ms`,
		a.Ticker, a.ExDate, a.Kind, venue, a.EffectiveMs, status, observed, checkedMs)
	return err
}

// pairReturn is a combo's total return in bps from the NAVs of the carrier
// session's day to those of the ex-date, the dividends of the ex-date in use
// added back: sum over the two legs of (NAV_ex + D) / NAV_carrier - 1. ok is
// false until the four NAVs are confirmed.
func (s *store) pairReturn(ctx context.Context, combo Combo, carrier, exDate string) (float64, bool, error) {
	var tr *float64
	err := s.pool.QueryRow(ctx, `WITH nav AS (
		  SELECT DISTINCT ON (ticker, session_date) ticker, session_date, nav FROM etfpair.etf_nav
		  WHERE ticker IN ($1, $2) AND session_date IN ($3::date, $4::date) ORDER BY ticker, session_date, confirmed_ms DESC
		), div AS (
		  SELECT DISTINCT ON (ticker) ticker, value FROM etfpair.etf_action
		  WHERE ticker IN ($1, $2) AND ex_date = $4::date AND kind = 'dividend' ORDER BY ticker, known_ms DESC
		)
		SELECT (10000 * sum((x.nav + coalesce(d.value, 0)) / c.nav - 1))::float8
		FROM nav c JOIN nav x ON x.ticker = c.ticker AND x.session_date = $4::date LEFT JOIN div d ON d.ticker = c.ticker
		WHERE c.session_date = $3::date HAVING count(*) = 2`, combo.BullTicker, combo.BearTicker, carrier, exDate).Scan(&tr)
	if errors.Is(err, pgx.ErrNoRows) || tr == nil {
		return 0, false, nil
	}
	return *tr, err == nil, err
}

// logStep is ln(close at afterMs / close at beforeMs) of a contract's minutes
// on a venue; ok is false without both.
func (s *store) logStep(ctx context.Context, venue, symbol string, beforeMs, afterMs int64) (float64, bool, error) {
	var step *float64
	err := s.pool.QueryRow(ctx, `SELECT ln(a.price / b.price)::float8 FROM etfpair.perp_minute a JOIN etfpair.perp_minute b ON b.venue = a.venue AND b.symbol = a.symbol
		WHERE a.venue = $1 AND a.symbol = $2 AND a.minute_ms = $4 AND b.minute_ms = $3`, venue, symbol, beforeMs, afterMs).Scan(&step)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	return deref64(step), step != nil && err == nil, err
}

func deref64(f *float64) float64 {
	if f == nil {
		return 0
	}
	return *f
}

// refreshCoverage re-aggregates the recorder's rows minute by minute over the
// seconds in (fromMs, toMs]: Data Coverage's counts and the minute bars' end
// rows (etfpair.refresh_perp_second_minute, the one definition of both), and
// returns the minute rows it wrote or deleted. It waits while another
// refresh's transaction is open (the function locks the table first).
func (s *store) refreshCoverage(ctx context.Context, fromMs, toMs int64) (int, error) {
	var written int
	err := s.pool.QueryRow(ctx, `SELECT etfpair.refresh_perp_second_minute($1, $2)`, fromMs, toMs).Scan(&written)
	return written, err
}

// writeCalendarCheck keeps the latest comparison with Binance's schedule;
// the window is nil when the schedule was not compared.
func (s *store) writeCalendarCheck(ctx context.Context, checkedMs int64, fromMs, toMs *int64, agree bool, detail string) error {
	batch := &pgx.Batch{}
	batch.Queue(`DELETE FROM etfpair.calendar_check`)
	batch.Queue(`INSERT INTO etfpair.calendar_check (checked_ms, window_from_ms, window_to_ms, agree, detail) VALUES ($1, $2, $3, $4, $5)`,
		checkedMs, fromMs, toMs, agree, detail)
	return s.pool.SendBatch(ctx, batch).Close()
}
