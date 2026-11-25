-- Tables of the Combo Spread board, filled by dashboard/cmd/pairscan.
-- Every statement is idempotent: the collector applies the file at each start.
-- The functions of board 3.2 (from book_usable on) read
-- etfpair.perp_second, the recorder's table
-- (dashboard/internal/booktape/schema.sql): that file is applied first.
CREATE SCHEMA IF NOT EXISTS etfpair;

-- A combo is a bull and a bear ETF traded as one basket; ref_ticker is the
-- unleveraged ETF of the same index, shown for reference.
CREATE TABLE IF NOT EXISTS etfpair.combo (
    combo_id    text PRIMARY KEY,
    title       text NOT NULL,
    bull_ticker text NOT NULL,
    bear_ticker text NOT NULL,
    ref_ticker  text NOT NULL DEFAULT ''
);
ALTER TABLE etfpair.combo ADD COLUMN IF NOT EXISTS ref_ticker text NOT NULL DEFAULT '';

-- The perpetual contracts of a combo on one venue: the two legs and the reference.
CREATE TABLE IF NOT EXISTS etfpair.combo_market (
    combo_id    text NOT NULL REFERENCES etfpair.combo,
    venue       text NOT NULL,
    bull_symbol text NOT NULL,
    bear_symbol text NOT NULL,
    ref_symbol  text NOT NULL DEFAULT '',
    PRIMARY KEY (combo_id, venue)
);
ALTER TABLE etfpair.combo_market ADD COLUMN IF NOT EXISTS ref_symbol text NOT NULL DEFAULT '';
-- Board 3.4: each leg's price tick on the venue (config, from the venue's instrument list), which sets its pricing levels
-- (pricing_levels); NULL until the collector has written the configuration.
ALTER TABLE etfpair.combo_market ADD COLUMN IF NOT EXISTS bull_tick numeric, ADD COLUMN IF NOT EXISTS bear_tick numeric;

-- Fee levels a viewer can price a round trip with. Rates are bps of notional;
-- a negative maker rate is a rebate. source says where the rate comes from.
CREATE TABLE IF NOT EXISTS etfpair.fee_level (
    venue     text    NOT NULL,
    level     text    NOT NULL,
    label     text    NOT NULL,
    taker_bps numeric NOT NULL,
    maker_bps numeric,
    source    text    NOT NULL,
    sort      integer NOT NULL,
    PRIMARY KEY (venue, level)
);

-- The close of each contract's one-minute candle, the last trade of the
-- minute; minute_ms is the minute's start.
CREATE TABLE IF NOT EXISTS etfpair.perp_minute (
    venue      text    NOT NULL,
    symbol     text    NOT NULL,
    minute_ms  bigint  NOT NULL,
    price      numeric NOT NULL,
    sampled_ms bigint  NOT NULL,
    PRIMARY KEY (venue, symbol, minute_ms)
);

-- USD per USDT, one price per minute. source 'ticker': mid of the sampled
-- bid/ask; 'candle': the venue's one-minute candle close.
CREATE TABLE IF NOT EXISTS etfpair.fx_minute (
    pair       text    NOT NULL,
    minute_ms  bigint  NOT NULL,
    price      numeric NOT NULL,
    bid        numeric,
    ask        numeric,
    source     text    NOT NULL CHECK (source IN ('ticker', 'candle')),
    sampled_ms bigint  NOT NULL,
    PRIMARY KEY (pair, minute_ms)
);

-- A session is the day that starts at a trading day's regular close and runs
-- to the next close; session_date is that trading day in New York. The
-- collector writes the closes of its calendar from history_start, the coming
-- week included, so a minute belongs to the session of the newest close at or
-- before it, and a minute before the first close belongs to none.
CREATE TABLE IF NOT EXISTS etfpair.etf_session (
    session_date date   PRIMARY KEY,
    close_ms     bigint NOT NULL
);

-- The phases of the US equity day from the collector's calendar, one row per
-- stretch and no hole between rows: Overnight (20:00 the evening before a
-- trading day to 04:00 New York), Pre-Market (to 09:30), Regular (to the
-- close), After-Hours (four hours after the close), and Closed for the time
-- between one day's after-hours and the next day's overnight session.
CREATE TABLE IF NOT EXISTS etfpair.etf_phase (
    start_ms bigint PRIMARY KEY,
    end_ms   bigint NOT NULL,
    phase    text   NOT NULL
);

-- The bell anchor of an ETF for a session, the provisional anchor from its
-- close: price = fx * index_usdt in USD, where index_usdt is the index price
-- of the ETF's perpetual on venue at the close (the close of the one-minute
-- index kline that opens at kline_ms, one minute before the close; the index
-- is quoted in USDT) and fx is USD per USDT of that minute as the board's
-- functions take it (fx_minute: the minute's own or the newest of the five before it).
-- venue is binance, or bitget for a session before the contract's first
-- Binance index kline; a session before it without a Bitget contract has no
-- row. known_ms is when it became known: when the collector stored it, or,
-- for a session that closed before the collector started, the close plus two
-- minutes (about when a running collector stores it); a row replaced because
-- the close moved after it was read is known when the replacement is stored.
CREATE TABLE IF NOT EXISTS etfpair.etf_bell (
    ticker       text    NOT NULL,
    session_date date    NOT NULL,
    price        numeric NOT NULL,
    index_usdt   numeric NOT NULL,
    venue        text    NOT NULL CHECK (venue IN ('binance', 'bitget')),
    fx           numeric NOT NULL,
    kline_ms     bigint  NOT NULL,
    known_ms     bigint  NOT NULL,
    PRIMARY KEY (ticker, session_date)
);

-- Every distinct official NAV per share an issuer gave for a ticker and
-- trading day, to the cent, with the times it was first and last read: the
-- row read last is the issuer's current figure. via says how it was first
-- read: page (the issuer's product page), api (the data API the issuer's
-- website calls), history-csv (the issuer's historical NAV file), wayback (a
-- Wayback Machine capture of the issuer's page). A row loaded from a history
-- (history-csv, wayback, or the past days the api serves) has its load time
-- as both times. No source states when the issuer published a figure.
CREATE TABLE IF NOT EXISTS etfpair.etf_nav_obs (
    issuer       text    NOT NULL,
    ticker       text    NOT NULL,
    session_date date    NOT NULL,
    nav          numeric NOT NULL,
    received_ms  bigint  NOT NULL,
    seen_ms      bigint  NOT NULL,
    via          text    NOT NULL CHECK (via IN ('page', 'api', 'history-csv', 'wayback')),
    PRIMARY KEY (issuer, ticker, session_date, nav)
);

-- The confirmed NAV anchors: a ticker's NAV of a trading day once the issuer
-- has given the same figure of both legs for the NAV settle delay (the first
-- read of the current figure that long ago, every read since returning it).
-- From then on it is the anchor of the session that starts at that day's
-- close, in place of the bell anchor. confirmed_ms is when the collector
-- wrote the confirmation; for a row loaded from a history it is the load
-- time. A later, different NAV of the day, once stable in the same way, is a
-- further row; the row confirmed last is the one in use, the others stay on
-- record.
CREATE TABLE IF NOT EXISTS etfpair.etf_nav (
    ticker       text    NOT NULL,
    session_date date    NOT NULL,
    nav          numeric NOT NULL,
    confirmed_ms bigint  NOT NULL,
    issuer       text    NOT NULL,
    via          text    NOT NULL CHECK (via IN ('page', 'api', 'history-csv', 'wayback')),
    PRIMARY KEY (ticker, session_date, nav)
);

-- The corporate actions of the combos' ETFs, one row per reading that
-- changed what a source says (a later different reading of the same source
-- is a new row; the newest row of the action, by known_ms, is the one in
-- use). kind: dividend, value USD per share as declared (per pre-split
-- share); split, value new shares per old share (3 for 3-for-1, 0.1 for
-- 1-for-10). status: Scheduled (the date is known, the amount not yet),
-- Known (value set) or None (no distribution). source: config (entered in
-- the configuration; url holds the entry's source), proshares-schedule,
-- proshares-api, nasdaq, direxion-api, bitget-split-records (url: the
-- request). A None of proshares-api or direxion-api is derived: the issuer's
-- list has nothing for an expected round once it is over. known_ms is
-- when the collector first read it (for an action already past at the first
-- reading, its load time).
CREATE TABLE IF NOT EXISTS etfpair.etf_action (
    ticker   text    NOT NULL,
    ex_date  date    NOT NULL,
    kind     text    NOT NULL CHECK (kind IN ('dividend', 'split')),
    value    numeric CHECK (value > 0),
    status   text    NOT NULL CHECK (status IN ('Scheduled', 'Known', 'None')),
    source   text    NOT NULL,
    url      text    NOT NULL,
    known_ms bigint  NOT NULL,
    PRIMARY KEY (ticker, ex_date, kind, known_ms),
    CHECK ((status = 'Known') = (value IS NOT NULL))
);

-- The checks of an action once it has taken effect, information only (they
-- never change an anchor): one row per action and venue of the observation,
-- binance or bitget for an exchange's data, issuer for the issuer's NAVs.
-- effective_ms is the instant the action took effect there (combo_actions).
-- observed, by check: dividend at binance, a leg: the first-minute step at
-- the instant of the basket of the two legs' Binance indices (their market
-- moves cancel), bps; a reference ETF: Binance's special funding rate x mark
-- over the anchor, bps;
-- dividend at issuer: the pair's total return from the carrier day's NAV to
-- the ex-date's with the dividends added back, bps; split at a venue: the log
-- ratio of the first trade after the halt to the last before it, net of the
-- other leg's move. status: Applied (in effect, not checked yet or not
-- checkable), Verified or Mismatch.
CREATE TABLE IF NOT EXISTS etfpair.etf_action_check (
    ticker       text    NOT NULL,
    ex_date      date    NOT NULL,
    kind         text    NOT NULL CHECK (kind IN ('dividend', 'split')),
    venue        text    NOT NULL CHECK (venue IN ('binance', 'bitget', 'issuer')),
    effective_ms bigint  NOT NULL,
    status       text    NOT NULL CHECK (status IN ('Applied', 'Verified', 'Mismatch')),
    observed     numeric,
    checked_ms   bigint  NOT NULL,
    PRIMARY KEY (ticker, ex_date, kind, venue)
);

-- The latest comparison of the calendar with Binance's schedule of the US
-- equity market, one row: when it was made, the window the schedule covered,
-- whether the trading days and their closes agree there (dates with an
-- override aside), and the differences, or OK; a schedule that could not be
-- read or compared has no window, agree false and detail 'not compared: ...'.
CREATE TABLE IF NOT EXISTS etfpair.calendar_check (
    checked_ms     bigint  NOT NULL,
    window_from_ms bigint,
    window_to_ms   bigint,
    agree          boolean NOT NULL,
    detail         text    NOT NULL
);

-- combo_sessions lists every session of the calendar with its anchors for
-- the combo:
--   bell_bull, bell_bear  the two legs' bell anchors (etf_bell), valid only when
--                         read at this close (kline_ms one minute before close_ms)
--   nav_bull, nav_bear    the issuer's current NAV of each leg for the day (the
--                         figure read last), NULL while it has given none
--   a_bull, a_bear        the anchor in use: both legs' confirmed NAVs (the row
--                         of each ticker confirmed last) once both have one,
--                         otherwise both bell anchors once both exist
--   a_ref                 the reference ETF's bell anchor; it has no NAV stage
-- These are the anchors at the session's start: from the instant a dividend
-- or a split takes effect they are adjusted (combo_anchors).
--   basis                 NAV or Bell, the kind of the anchor in use; NULL for none
--   known_ms              when the anchor in use became known: the later of the
--                         two legs' NAV confirmations, or of their bells' known_ms
--   status                NAV (final), Bell (provisional) or Missing (no anchor)
CREATE OR REPLACE FUNCTION etfpair.combo_sessions(p_combo text)
RETURNS TABLE (session_date date, close_ms bigint, a_bull numeric, a_bear numeric, a_ref numeric, basis text,
               bell_bull numeric, bell_bear numeric, nav_bull numeric, nav_bear numeric, known_ms bigint, status text)
LANGUAGE sql STABLE AS $$
SELECT ss.session_date, ss.close_ms,
       CASE WHEN v.on_nav THEN nb.nav WHEN v.on_bell THEN bb.price END,
       CASE WHEN v.on_nav THEN nr.nav WHEN v.on_bell THEN br.price END,
       bf.price,
       CASE WHEN v.on_nav THEN 'NAV' WHEN v.on_bell THEN 'Bell' END,
       bb.price, br.price,
       (SELECT o.nav FROM etfpair.etf_nav_obs o WHERE o.ticker = c.bull_ticker AND o.session_date = ss.session_date ORDER BY o.seen_ms DESC LIMIT 1),
       (SELECT o.nav FROM etfpair.etf_nav_obs o WHERE o.ticker = c.bear_ticker AND o.session_date = ss.session_date ORDER BY o.seen_ms DESC LIMIT 1),
       CASE WHEN v.on_nav THEN greatest(nb.confirmed_ms, nr.confirmed_ms) WHEN v.on_bell THEN greatest(bb.known_ms, br.known_ms) END,
       CASE WHEN v.on_nav THEN 'NAV' WHEN v.on_bell THEN 'Bell' ELSE 'Missing' END
FROM etfpair.etf_session ss
CROSS JOIN (SELECT bull_ticker, bear_ticker, ref_ticker FROM etfpair.combo WHERE combo_id = p_combo) c
LEFT JOIN etfpair.etf_bell bb ON bb.ticker = c.bull_ticker AND bb.session_date = ss.session_date AND bb.kline_ms = ss.close_ms - 60000
LEFT JOIN etfpair.etf_bell br ON br.ticker = c.bear_ticker AND br.session_date = ss.session_date AND br.kline_ms = ss.close_ms - 60000
LEFT JOIN etfpair.etf_bell bf ON bf.ticker = c.ref_ticker AND bf.session_date = ss.session_date AND bf.kline_ms = ss.close_ms - 60000
LEFT JOIN LATERAL (SELECT n.nav, n.confirmed_ms FROM etfpair.etf_nav n
                   WHERE n.ticker = c.bull_ticker AND n.session_date = ss.session_date ORDER BY n.confirmed_ms DESC LIMIT 1) nb ON true
LEFT JOIN LATERAL (SELECT n.nav, n.confirmed_ms FROM etfpair.etf_nav n
                   WHERE n.ticker = c.bear_ticker AND n.session_date = ss.session_date ORDER BY n.confirmed_ms DESC LIMIT 1) nr ON true
CROSS JOIN LATERAL (SELECT nb.nav IS NOT NULL AND nr.nav IS NOT NULL AS on_nav, bb.price IS NOT NULL AND br.price IS NOT NULL AS on_bell) v
$$;

-- action_start is the one definition of T, the start of an action of an
-- ex-date: the start of the ex-date's Overnight phase, 20:00 New York of the
-- calendar day before the ex-date (a Sunday 20:00 for a Monday ex-date; the
-- holiday's 20:00 for an ex-date after a holiday). past_t: an action still
-- Scheduled (its amount not known) once T has passed. combo_actions and the
-- board's Next Actions read it.
CREATE OR REPLACE FUNCTION etfpair.action_start(p_ex_date date, p_status text, OUT start_ms bigint, OUT past_t boolean)
LANGUAGE sql STABLE AS $$
SELECT t, p_status = 'Scheduled' AND (extract(epoch FROM now()) * 1000)::bigint >= t
FROM (SELECT (extract(epoch FROM ((p_ex_date - 1) + time '20:00') AT TIME ZONE 'America/New_York') * 1000)::bigint AS t) x
$$;

-- combo_actions is the one definition of when a corporate action of the
-- combo's ETFs takes effect on a venue. One row per action in use (the newest
-- row of etf_action) of the bull, bear or reference ETF (leg) whose carrier
-- session is in the calendar with its end: the session that holds the
-- action's T (action_start). effective_ms:
--   dividend  T, on both venues
--   split     T_v, the first minute on the venue from the start of the
--             ex-date's Overnight phase to the carrier session's end whose
--             close is on the new basis (nearer the minute before's close / r
--             than to it): the first trade after the venue's halt; NULL until
--             there is one
-- halt_ms (split): the start of the run of unchanged closes just before T_v,
-- the venue's halt: the ETF's contract has no points on the venue from
-- halt_ms to T_v. anchor: the anchor of the ETF in use at the carrier
-- session's start (combo_sessions); expected: the step the action makes at
-- its instant, for a leg's dividend that of the basket of the two legs,
-- -10000 * sum(value / anchor) over the legs going ex that day (bps), for a
-- reference's dividend its own, -10000 * value / anchor, for a split its log
-- ratio, -ln(value), NULL without a value (Scheduled, None) or an anchor;
-- observed: the check at the instant (etf_action_check:
-- Binance for a dividend, this venue for a split); nav_bps: the issuer's
-- next-day check.
-- shown: the status before the instant (Scheduled, Known, None), from it
-- Applied, Verified or Mismatch for a Known action (a split's own venue, a
-- dividend's every check), and 'Scheduled · Past T' for an action whose
-- amount is still not known past T (action_start); warn marks Mismatch and that.
CREATE OR REPLACE FUNCTION etfpair.combo_actions(p_combo text, p_venue text)
RETURNS TABLE (session_date date, leg text, ticker text, ex_date date, kind text, value numeric, status text, known_ms bigint, source text,
               url text, effective_ms bigint, halt_ms bigint, anchor numeric, expected numeric, observed numeric, nav_bps numeric, shown text,
               warn boolean)
LANGUAGE sql STABLE AS $$
WITH legs AS (
    SELECT l.leg, l.ticker, l.symbol
    FROM etfpair.combo c JOIN etfpair.combo_market m ON m.combo_id = c.combo_id AND m.venue = p_venue
    CROSS JOIN LATERAL (VALUES ('bull', c.bull_ticker, m.bull_symbol), ('bear', c.bear_ticker, m.bear_symbol), ('ref', c.ref_ticker, m.ref_symbol))
        AS l(leg, ticker, symbol)
    WHERE c.combo_id = p_combo AND l.ticker <> ''
), sess AS (
    SELECT cs.session_date, cs.close_ms, lead(cs.close_ms) OVER (ORDER BY cs.close_ms) AS next_ms, cs.a_bull, cs.a_bear, cs.a_ref
    FROM etfpair.combo_sessions(p_combo) cs
), act AS (
    SELECT DISTINCT ON (a.ticker, a.ex_date, a.kind) a.*, l.leg, l.symbol, i.start_ms, i.past_t
    FROM etfpair.etf_action a JOIN legs l ON l.ticker = a.ticker CROSS JOIN LATERAL etfpair.action_start(a.ex_date, a.status) i
    ORDER BY a.ticker, a.ex_date, a.kind, a.known_ms DESC
), carried AS (
    SELECT act.*, s.session_date, s.close_ms, s.next_ms, CASE act.leg WHEN 'bull' THEN s.a_bull WHEN 'bear' THEN s.a_bear ELSE s.a_ref END AS anchor
    FROM act JOIN sess s ON act.start_ms >= s.close_ms AND act.start_ms < s.next_ms
), timed AS (
    SELECT c.*, CASE WHEN c.kind = 'dividend' THEN c.start_ms ELSE sp.tv END AS effective_ms, sp.hv AS halt_ms
    FROM carried c
    LEFT JOIN LATERAL (
        WITH m AS (SELECT p.minute_ms, p.price, lag(p.price) OVER (ORDER BY p.minute_ms) AS prev FROM etfpair.perp_minute p
                   WHERE c.kind = 'split' AND c.status = 'Known' AND p.venue = p_venue AND p.symbol = c.symbol
                     AND p.minute_ms >= c.close_ms AND (c.next_ms IS NULL OR p.minute_ms < c.next_ms)),
             t AS (SELECT min(minute_ms) AS tv FROM m WHERE m.minute_ms >= c.start_ms AND abs(ln(price * c.value / prev)) < abs(ln(price / prev)))
        SELECT t.tv, coalesce((SELECT max(m.minute_ms) + 60000 FROM m WHERE m.minute_ms < t.tv AND m.price <> m.prev), c.close_ms) AS hv
        FROM t WHERE t.tv IS NOT NULL) sp ON true
)
SELECT t.session_date, t.leg, t.ticker, t.ex_date, t.kind, t.value, t.status, t.known_ms, t.source, t.url, t.effective_ms, t.halt_ms, t.anchor,
       CASE WHEN t.value IS NULL THEN NULL WHEN t.kind = 'split' THEN -ln(t.value) WHEN t.leg = 'ref' THEN -10000 * t.value / t.anchor
            ELSE -10000 * sum(t.value / t.anchor) FILTER (WHERE t.leg <> 'ref') OVER (PARTITION BY t.session_date, t.ex_date, t.kind) END,
       (SELECT k.observed FROM etfpair.etf_action_check k WHERE k.ticker = t.ticker AND k.ex_date = t.ex_date AND k.kind = t.kind
          AND k.venue = CASE t.kind WHEN 'dividend' THEN 'binance' ELSE p_venue END),
       (SELECT k.observed FROM etfpair.etf_action_check k WHERE k.ticker = t.ticker AND k.ex_date = t.ex_date AND k.kind = t.kind AND k.venue = 'issuer'),
       v.shown, v.shown IN ('Mismatch', 'Scheduled · Past T')
FROM timed t
CROSS JOIN LATERAL (SELECT CASE
    WHEN t.past_t THEN 'Scheduled · Past T'
    WHEN t.status <> 'Known' OR t.effective_ms IS NULL OR (extract(epoch FROM now()) * 1000)::bigint < t.effective_ms THEN t.status
    ELSE coalesce((SELECT CASE WHEN bool_or(k.status = 'Mismatch') THEN 'Mismatch' WHEN bool_or(k.status = 'Verified') THEN 'Verified' END
                   FROM etfpair.etf_action_check k WHERE k.ticker = t.ticker AND k.ex_date = t.ex_date AND k.kind = t.kind
                     AND (t.kind = 'dividend' OR k.venue = p_venue)), 'Applied') END AS shown) v
$$;

-- combo_anchors is the one definition of the anchors in use, piecewise: each
-- session from its close, cut at every instant at which an action of the
-- combo's ETFs takes effect on the venue (combo_actions: a Known one, and a
-- leg's dividend still Scheduled, which changes no anchor) and at a split's
-- halt. In a stretch [from_ms, to_ms), per ETF x with the session's anchor A
-- (combo_sessions):
--   A' = A - D  from a dividend's T          A' = A / r  from a split's T_v
--   both        (A - D) from T, then / r from T_v (D per pre-split share)
-- gap_x marks the stretch from a split's halt to T_v, where x has no points.
-- past_t marks the stretch from T of a leg's dividend whose amount is still
-- unknown (Scheduled): the anchors stay unadjusted, so the spread has stepped
-- there by about the dividend; the mean restarts at T as for a Known one,
-- and the board shows Past T instead of a signal until the amount is known
-- (the whole stretch is then redrawn on A - D).
-- The next session's anchors are ex already: nothing carries over. known_ms
-- is when the two legs' anchors of the stretch became known: the later of
-- the session's anchor and of each Known action of a leg in effect.
CREATE OR REPLACE FUNCTION etfpair.combo_anchors(p_combo text, p_venue text)
RETURNS TABLE (session_date date, from_ms bigint, to_ms bigint, a_bull numeric, a_bear numeric, a_ref numeric,
               gap_bull boolean, gap_bear boolean, gap_ref boolean, known_ms bigint, past_t boolean)
LANGUAGE sql STABLE AS $$
WITH sess AS (
    SELECT cs.session_date, cs.close_ms, lead(cs.close_ms) OVER (ORDER BY cs.close_ms) AS next_ms, cs.a_bull, cs.a_bear, cs.a_ref, cs.known_ms
    FROM etfpair.combo_sessions(p_combo) cs
), acts AS MATERIALIZED (
    SELECT * FROM etfpair.combo_actions(p_combo, p_venue) a
    WHERE a.status = 'Known' AND a.effective_ms IS NOT NULL OR a.status = 'Scheduled' AND a.kind = 'dividend' AND a.leg <> 'ref'
), cuts AS (
    SELECT s.session_date, s.close_ms AS at FROM sess s
    UNION SELECT a.session_date, a.effective_ms FROM acts a
    UNION SELECT a.session_date, a.halt_ms FROM acts a WHERE a.halt_ms IS NOT NULL
), seg AS (
    SELECT c.session_date, c.at AS from_ms, coalesce(lead(c.at) OVER (PARTITION BY c.session_date ORDER BY c.at), s.next_ms) AS to_ms,
           s.a_bull, s.a_bear, s.a_ref, s.known_ms
    FROM cuts c JOIN sess s USING (session_date)
)
SELECT g.session_date, g.from_ms, g.to_ms,
       (g.a_bull - coalesce(x.d_bull, 0)) / coalesce(x.r_bull, 1), (g.a_bear - coalesce(x.d_bear, 0)) / coalesce(x.r_bear, 1),
       (g.a_ref - coalesce(x.d_ref, 0)) / coalesce(x.r_ref, 1),
       coalesce(x.gap_bull, false), coalesce(x.gap_bear, false), coalesce(x.gap_ref, false),
       CASE WHEN g.known_ms IS NOT NULL THEN greatest(g.known_ms, x.known_ms) END, coalesce(x.past_t, false)
FROM seg g
CROSS JOIN LATERAL (
    SELECT max(a.value) FILTER (WHERE a.leg = 'bull' AND a.kind = 'dividend' AND a.effective_ms <= g.from_ms) AS d_bull,
           max(a.value) FILTER (WHERE a.leg = 'bull' AND a.kind = 'split' AND a.effective_ms <= g.from_ms) AS r_bull,
           max(a.value) FILTER (WHERE a.leg = 'bear' AND a.kind = 'dividend' AND a.effective_ms <= g.from_ms) AS d_bear,
           max(a.value) FILTER (WHERE a.leg = 'bear' AND a.kind = 'split' AND a.effective_ms <= g.from_ms) AS r_bear,
           max(a.value) FILTER (WHERE a.leg = 'ref' AND a.kind = 'dividend' AND a.effective_ms <= g.from_ms) AS d_ref,
           max(a.value) FILTER (WHERE a.leg = 'ref' AND a.kind = 'split' AND a.effective_ms <= g.from_ms) AS r_ref,
           bool_or(a.leg = 'bull' AND a.kind = 'split' AND g.from_ms >= a.halt_ms AND g.from_ms < a.effective_ms) AS gap_bull,
           bool_or(a.leg = 'bear' AND a.kind = 'split' AND g.from_ms >= a.halt_ms AND g.from_ms < a.effective_ms) AS gap_bear,
           bool_or(a.leg = 'ref' AND a.kind = 'split' AND g.from_ms >= a.halt_ms AND g.from_ms < a.effective_ms) AS gap_ref,
           max(a.known_ms) FILTER (WHERE a.leg <> 'ref' AND a.status = 'Known' AND a.effective_ms <= g.from_ms) AS known_ms,
           bool_or(a.status = 'Scheduled' AND a.effective_ms <= g.from_ms) AS past_t
    FROM acts a WHERE a.session_date = g.session_date) x
WHERE g.to_ms IS NULL OR g.from_ms < g.to_ms
$$;

-- Board 3.1's objects, which only the 3.1 board read (combo_series, the eight-argument combo_points and their helpers
-- sweep, real_spread, real_depth and the aggregate ema on ema_step), are dropped now that no page uses them
-- (OWNER.md item 24, 2026-11-23); combo_sessions, combo_actions, combo_anchors and action_start above stay, as 3.2
-- calls them. Idempotent: the collector applies this file at every start.
DROP FUNCTION IF EXISTS etfpair.combo_points(text, text, text, integer, bigint, bigint, bigint, numeric);
DROP FUNCTION IF EXISTS etfpair.combo_points(text, text, text, integer, bigint, bigint, bigint);  -- board 3.0's, gone from production since 2026-11-22
DROP FUNCTION IF EXISTS etfpair.combo_series(text, text, text, integer, bigint, bigint);
DROP FUNCTION IF EXISTS etfpair.real_spread(numeric[], numeric[], numeric[], numeric[], numeric, numeric, numeric, numeric);
DROP FUNCTION IF EXISTS etfpair.real_depth(numeric[], numeric[], numeric[], numeric[], numeric, numeric);
DROP FUNCTION IF EXISTS etfpair.sweep(numeric[], numeric[], numeric);
DROP AGGREGATE IF EXISTS etfpair.ema(numeric, numeric);
DROP FUNCTION IF EXISTS etfpair.ema_step(numeric, numeric, numeric);

-- ---------------------------------------------------------------------------------------------------------------
-- Board 3.2 (experiments/board32_20261122/DESIGN.md, third version; dashboard/SPEC.md). Everything below has a new
-- name. Of the objects above it calls combo_anchors, combo_actions, combo_sessions and action_start (range_stretches
-- and combo_actions call it); board 3.1's others were dropped on 2026-11-23 (OWNER.md item 24, the drops above). The
-- functions return signed figures; the board shows the three deviations as absolute values on the Spread chart's
-- tooltip, the strips and the Entry Points chart, signed on the Deviation chart (board 3.3), and keeps the sign for
-- the Signal.
-- ---------------------------------------------------------------------------------------------------------------

-- book_usable is the one definition of a leg's usable book in a second (DESIGN 1.0): both sides have a level, every
-- level has a price and a size above zero (a size array as long as its price array), the best bid is below the best
-- ask (a crossed or locked book is no book), and from a split's T_v to the end of its carrier session (p_tv_ms, NULL
-- elsewhere) the frame is not older than T_v: a book from before it is the frozen book of the old basis. Without a
-- usable book a leg has no mid, no levels and no 1+2 depth in that second. perp_second_minute.end_mid stores this
-- rule's verdict (without T_v) for every aggregated minute: a change to the rule needs the summary refilled (the
-- day-by-day fill of its history, dashboard/SPEC.md section 9).
CREATE OR REPLACE FUNCTION etfpair.book_usable(p_bid_px numeric[], p_bid_sz numeric[], p_ask_px numeric[], p_ask_sz numeric[],
                                               p_book_ms bigint, p_tv_ms bigint)
RETURNS boolean LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
SELECT coalesce(p_bid_px[1] < p_ask_px[1]
                AND cardinality(p_bid_sz) = cardinality(p_bid_px) AND cardinality(p_ask_sz) = cardinality(p_ask_px)
                AND array_position(p_bid_px, NULL) IS NULL AND array_position(p_ask_px, NULL) IS NULL
                AND 0 < ALL (p_bid_sz) AND 0 < ALL (p_ask_sz), false)
       AND NOT coalesce(p_book_ms < p_tv_ms, false)
$$;

-- trade_usable is the one definition of a leg's usable trade price in a second (DESIGN 1.0): there is one, and from a
-- split's T_v to the end of its carrier session it is not from before T_v (the old basis).
CREATE OR REPLACE FUNCTION etfpair.trade_usable(p_price numeric, p_trade_ms bigint, p_tv_ms bigint)
RETURNS boolean LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
SELECT p_price IS NOT NULL AND NOT coalesce(p_trade_ms < p_tv_ms, false)
$$;

-- top2_size is the size, in contracts (shares of the ETF), of one side of a leg's book in its first two 0.01 buckets
-- (DESIGN 1.7): a bid falls in the bucket floor(px * 100) / 100, an ask in ceil(px * 100) / 100 (the identity at a
-- 0.01 tick; Bitget's own 0.01 merged depth), the buckets counted from the best price; NULL (Not Recorded) unless a
-- recorded level lies beyond the second bucket. It reads up to the third bucket. book_pair applies the same rule to
-- both legs' usable books inline (a test compares the two on every recorded row); this one is for a leg with a
-- usable book on a point that is on trade prices.
CREATE OR REPLACE FUNCTION etfpair.top2_size(p_px numeric[], p_sz numeric[], p_bids boolean)
RETURNS numeric LANGUAGE plpgsql IMMUTABLE STRICT PARALLEL SAFE AS $$
DECLARE
    edge numeric;                -- the far edge of the bucket being read: its lowest bid, its highest ask
    in_second boolean := false;  -- reading the second bucket
    total numeric := 0;
BEGIN
    IF cardinality(p_px) = 0 THEN
        RETURN NULL;
    END IF;
    IF p_bids THEN
        edge := floor(p_px[1] * 100) / 100;
        FOR i IN 1 .. cardinality(p_px) LOOP
            IF p_px[i] < edge THEN
                IF in_second THEN
                    RETURN total;
                END IF;
                in_second := true;
                edge := floor(p_px[i] * 100) / 100;
            END IF;
            total := total + p_sz[i];
        END LOOP;
    ELSE
        edge := ceil(p_px[1] * 100) / 100;
        FOR i IN 1 .. cardinality(p_px) LOOP
            IF p_px[i] > edge THEN
                IF in_second THEN
                    RETURN total;
                END IF;
                in_second := true;
                edge := ceil(p_px[i] * 100) / 100;
            END IF;
            total := total + p_sz[i];
        END LOOP;
    END IF;
    RETURN NULL;
END
$$;

-- book_pair is the one definition of what both legs' books give a point (DESIGN 1.4, 1.7), in one call (each array
-- is expanded once). The caller passes usable books only (book_usable), the two legs' of one second, with the
-- point's anchors A, USD per USDT F and the notional N (USDT, both legs together, PRD's N); call it through
-- CROSS JOIN LATERAL, not as (etfpair.book_pair(...)).*, which runs it once per output column.
--   Real spreads of N, Long on both legs' asks, Short on both legs' bids, capped at what the recorded levels hold:
--     S_i   the sizes of leg i's levels on that side, K_max = min(A_bull * S_bull, A_bear * S_bear)
--     v_i   the best price; twice: K = min(N / (v_bull / A_bull + v_bear / A_bear), K_max), q_i = K / A_i (so
--           q_bull * A_bull = q_bear * A_bear), v_i the average price of taking q_i from leg i's levels, best first
--     s     = 10^4 * (F * v_bull / A_bull + F * v_bear / A_bear - 2) on the last prices
--     partial: K was capped in either pass; filled: q_bull * v_bull + q_bear * v_bear of the last pass (USDT, <= N)
--   Where neither pass caps (the levels fill N) the spread is board 3.1's real_spread's (dropped 2026-11-23), digit for
--   digit: the same operations in the same order. The side totals are summed only once a sweep runs out of levels; a pass whose
--   capped K is that of the pass before reuses its prices; a capped leg whose quantity is all its levels takes their
--   sum without a second sweep. No rounding to the venues' quantity steps.
--   NULL real spreads, partial and filled without a positive N, A or F (the board's No Notional, No Anchor).
--   1+2 depths (top2_size's rule): each leg's size in its first two 0.01 buckets of bids and of asks, contracts,
--   NULL when its levels end within the second bucket (Not Recorded); they do not depend on A, F or N.
CREATE OR REPLACE FUNCTION etfpair.book_pair(p_bid_px_bull numeric[], p_bid_sz_bull numeric[], p_ask_px_bull numeric[], p_ask_sz_bull numeric[],
                                             p_bid_px_bear numeric[], p_bid_sz_bear numeric[], p_ask_px_bear numeric[], p_ask_sz_bear numeric[],
                                             p_a_bull numeric, p_a_bear numeric, p_fx numeric, p_notional numeric,
                                             OUT s_long_bps numeric, OUT s_short_bps numeric, OUT partial_long boolean, OUT partial_short boolean,
                                             OUT filled_long numeric, OUT filled_short numeric,
                                             OUT bid12_bull numeric, OUT bid12_bear numeric, OUT ask12_bull numeric, OUT ask12_bear numeric)
LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE AS $$
DECLARE
    n_bull integer;
    n_bear integer;
    edge numeric;          -- 1+2 depth: the far edge of the bucket being read
    in_second boolean;
    total numeric;
    v_bull numeric;        -- real spreads: the prices of the pass
    v_bear numeric;
    k numeric;
    k_max numeric;
    capped boolean;
    q_bull numeric;
    q_bear numeric;
    w_bull numeric;        -- the average prices of a sweep
    w_bear numeric;
    paid numeric;
    rest numeric;
    out_bull boolean;      -- the sweep ran out of levels
    out_bear boolean;
    t_bull numeric;        -- all levels of the side: their size and what they cost
    t_bear numeric;
    all_bull numeric;
    all_bear numeric;
BEGIN
    -- Bid 1+2 Depth, then Ask 1+2 Depth, of each leg.
    n_bull := coalesce(cardinality(p_bid_px_bull), 0);
    IF n_bull > 0 THEN
        edge := floor(p_bid_px_bull[1] * 100) / 100; in_second := false; total := 0;
        FOR i IN 1 .. n_bull LOOP
            IF p_bid_px_bull[i] < edge THEN
                IF in_second THEN bid12_bull := total; EXIT; END IF;
                in_second := true; edge := floor(p_bid_px_bull[i] * 100) / 100;
            END IF;
            total := total + p_bid_sz_bull[i];
        END LOOP;
    END IF;
    n_bear := coalesce(cardinality(p_bid_px_bear), 0);
    IF n_bear > 0 THEN
        edge := floor(p_bid_px_bear[1] * 100) / 100; in_second := false; total := 0;
        FOR i IN 1 .. n_bear LOOP
            IF p_bid_px_bear[i] < edge THEN
                IF in_second THEN bid12_bear := total; EXIT; END IF;
                in_second := true; edge := floor(p_bid_px_bear[i] * 100) / 100;
            END IF;
            total := total + p_bid_sz_bear[i];
        END LOOP;
    END IF;
    n_bull := coalesce(cardinality(p_ask_px_bull), 0);
    IF n_bull > 0 THEN
        edge := ceil(p_ask_px_bull[1] * 100) / 100; in_second := false; total := 0;
        FOR i IN 1 .. n_bull LOOP
            IF p_ask_px_bull[i] > edge THEN
                IF in_second THEN ask12_bull := total; EXIT; END IF;
                in_second := true; edge := ceil(p_ask_px_bull[i] * 100) / 100;
            END IF;
            total := total + p_ask_sz_bull[i];
        END LOOP;
    END IF;
    n_bear := coalesce(cardinality(p_ask_px_bear), 0);
    IF n_bear > 0 THEN
        edge := ceil(p_ask_px_bear[1] * 100) / 100; in_second := false; total := 0;
        FOR i IN 1 .. n_bear LOOP
            IF p_ask_px_bear[i] > edge THEN
                IF in_second THEN ask12_bear := total; EXIT; END IF;
                in_second := true; edge := ceil(p_ask_px_bear[i] * 100) / 100;
            END IF;
            total := total + p_ask_sz_bear[i];
        END LOOP;
    END IF;

    IF NOT coalesce(p_notional > 0 AND p_a_bull > 0 AND p_a_bear > 0 AND p_fx > 0, false) THEN
        RETURN;
    END IF;

    -- Real Long Spread: both legs' asks (n_bull, n_bear are their levels).
    IF n_bull > 0 AND n_bear > 0 THEN
        v_bull := p_ask_px_bull[1]; v_bear := p_ask_px_bear[1]; k_max := NULL; capped := false;
        FOR pass IN 1 .. 2 LOOP
            k := p_notional / (v_bull / p_a_bull + v_bear / p_a_bear);
            IF capped AND k >= k_max THEN
                EXIT;                                     -- capped again: the quantities and prices of the pass before
            END IF;
            q_bull := k / p_a_bull; q_bear := k / p_a_bear;
            paid := 0; rest := q_bull;
            FOR i IN 1 .. n_bull LOOP
                IF rest > p_ask_sz_bull[i] THEN
                    paid := paid + p_ask_sz_bull[i] * p_ask_px_bull[i]; rest := rest - p_ask_sz_bull[i];
                ELSE
                    paid := paid + rest * p_ask_px_bull[i]; rest := 0; EXIT;
                END IF;
            END LOOP;
            out_bull := rest > 0; w_bull := paid / q_bull;
            IF out_bull THEN t_bull := q_bull - rest; all_bull := paid; END IF;
            paid := 0; rest := q_bear;
            FOR i IN 1 .. n_bear LOOP
                IF rest > p_ask_sz_bear[i] THEN
                    paid := paid + p_ask_sz_bear[i] * p_ask_px_bear[i]; rest := rest - p_ask_sz_bear[i];
                ELSE
                    paid := paid + rest * p_ask_px_bear[i]; rest := 0; EXIT;
                END IF;
            END LOOP;
            out_bear := rest > 0; w_bear := paid / q_bear;
            IF out_bear THEN t_bear := q_bear - rest; all_bear := paid; END IF;
            IF out_bull OR out_bear THEN
                -- The levels hold less than K: cap K at what both legs' levels hold.
                IF NOT out_bull THEN
                    t_bull := 0; all_bull := 0;
                    FOR i IN 1 .. n_bull LOOP
                        t_bull := t_bull + p_ask_sz_bull[i]; all_bull := all_bull + p_ask_sz_bull[i] * p_ask_px_bull[i];
                    END LOOP;
                END IF;
                IF NOT out_bear THEN
                    t_bear := 0; all_bear := 0;
                    FOR i IN 1 .. n_bear LOOP
                        t_bear := t_bear + p_ask_sz_bear[i]; all_bear := all_bear + p_ask_sz_bear[i] * p_ask_px_bear[i];
                    END LOOP;
                END IF;
                k_max := least(p_a_bull * t_bull, p_a_bear * t_bear); capped := true;
                -- K_max / A_i <= S_i; least() only absorbs the rounding of the division.
                q_bull := least(k_max / p_a_bull, t_bull); q_bear := least(k_max / p_a_bear, t_bear);
                IF q_bull = t_bull THEN
                    w_bull := all_bull / q_bull;
                ELSE
                    paid := 0; rest := q_bull;
                    FOR i IN 1 .. n_bull LOOP
                        IF rest > p_ask_sz_bull[i] THEN
                            paid := paid + p_ask_sz_bull[i] * p_ask_px_bull[i]; rest := rest - p_ask_sz_bull[i];
                        ELSE
                            paid := paid + rest * p_ask_px_bull[i]; EXIT;
                        END IF;
                    END LOOP;
                    w_bull := paid / q_bull;
                END IF;
                IF q_bear = t_bear THEN
                    w_bear := all_bear / q_bear;
                ELSE
                    paid := 0; rest := q_bear;
                    FOR i IN 1 .. n_bear LOOP
                        IF rest > p_ask_sz_bear[i] THEN
                            paid := paid + p_ask_sz_bear[i] * p_ask_px_bear[i]; rest := rest - p_ask_sz_bear[i];
                        ELSE
                            paid := paid + rest * p_ask_px_bear[i]; EXIT;
                        END IF;
                    END LOOP;
                    w_bear := paid / q_bear;
                END IF;
            END IF;
            v_bull := w_bull; v_bear := w_bear;
        END LOOP;
        s_long_bps := 10000 * (p_fx * v_bull / p_a_bull + p_fx * v_bear / p_a_bear - 2);
        partial_long := capped;
        filled_long := q_bull * v_bull + q_bear * v_bear;
    END IF;

    -- Real Short Spread: both legs' bids, the same steps.
    n_bull := coalesce(cardinality(p_bid_px_bull), 0);
    n_bear := coalesce(cardinality(p_bid_px_bear), 0);
    IF n_bull > 0 AND n_bear > 0 THEN
        v_bull := p_bid_px_bull[1]; v_bear := p_bid_px_bear[1]; k_max := NULL; capped := false;
        FOR pass IN 1 .. 2 LOOP
            k := p_notional / (v_bull / p_a_bull + v_bear / p_a_bear);
            IF capped AND k >= k_max THEN
                EXIT;
            END IF;
            q_bull := k / p_a_bull; q_bear := k / p_a_bear;
            paid := 0; rest := q_bull;
            FOR i IN 1 .. n_bull LOOP
                IF rest > p_bid_sz_bull[i] THEN
                    paid := paid + p_bid_sz_bull[i] * p_bid_px_bull[i]; rest := rest - p_bid_sz_bull[i];
                ELSE
                    paid := paid + rest * p_bid_px_bull[i]; rest := 0; EXIT;
                END IF;
            END LOOP;
            out_bull := rest > 0; w_bull := paid / q_bull;
            IF out_bull THEN t_bull := q_bull - rest; all_bull := paid; END IF;
            paid := 0; rest := q_bear;
            FOR i IN 1 .. n_bear LOOP
                IF rest > p_bid_sz_bear[i] THEN
                    paid := paid + p_bid_sz_bear[i] * p_bid_px_bear[i]; rest := rest - p_bid_sz_bear[i];
                ELSE
                    paid := paid + rest * p_bid_px_bear[i]; rest := 0; EXIT;
                END IF;
            END LOOP;
            out_bear := rest > 0; w_bear := paid / q_bear;
            IF out_bear THEN t_bear := q_bear - rest; all_bear := paid; END IF;
            IF out_bull OR out_bear THEN
                IF NOT out_bull THEN
                    t_bull := 0; all_bull := 0;
                    FOR i IN 1 .. n_bull LOOP
                        t_bull := t_bull + p_bid_sz_bull[i]; all_bull := all_bull + p_bid_sz_bull[i] * p_bid_px_bull[i];
                    END LOOP;
                END IF;
                IF NOT out_bear THEN
                    t_bear := 0; all_bear := 0;
                    FOR i IN 1 .. n_bear LOOP
                        t_bear := t_bear + p_bid_sz_bear[i]; all_bear := all_bear + p_bid_sz_bear[i] * p_bid_px_bear[i];
                    END LOOP;
                END IF;
                k_max := least(p_a_bull * t_bull, p_a_bear * t_bear); capped := true;
                q_bull := least(k_max / p_a_bull, t_bull); q_bear := least(k_max / p_a_bear, t_bear);
                IF q_bull = t_bull THEN
                    w_bull := all_bull / q_bull;
                ELSE
                    paid := 0; rest := q_bull;
                    FOR i IN 1 .. n_bull LOOP
                        IF rest > p_bid_sz_bull[i] THEN
                            paid := paid + p_bid_sz_bull[i] * p_bid_px_bull[i]; rest := rest - p_bid_sz_bull[i];
                        ELSE
                            paid := paid + rest * p_bid_px_bull[i]; EXIT;
                        END IF;
                    END LOOP;
                    w_bull := paid / q_bull;
                END IF;
                IF q_bear = t_bear THEN
                    w_bear := all_bear / q_bear;
                ELSE
                    paid := 0; rest := q_bear;
                    FOR i IN 1 .. n_bear LOOP
                        IF rest > p_bid_sz_bear[i] THEN
                            paid := paid + p_bid_sz_bear[i] * p_bid_px_bear[i]; rest := rest - p_bid_sz_bear[i];
                        ELSE
                            paid := paid + rest * p_bid_px_bear[i]; EXIT;
                        END IF;
                    END LOOP;
                    w_bear := paid / q_bear;
                END IF;
            END IF;
            v_bull := w_bull; v_bear := w_bear;
        END LOOP;
        s_short_bps := 10000 * (p_fx * v_bull / p_a_bull + p_fx * v_bear / p_a_bear - 2);
        partial_short := capped;
        filled_short := q_bull * v_bull + q_bear * v_bear;
    END IF;
END
$$;

-- combo_signal is the one definition of a point's Signal (DESIGN 1.6), in this order: No Anchor (no spread: no
-- anchor or no USD per USDT), Past T (past T of a leg's dividend without its amount), No Mean, No Notional (N is not
-- positive), No Entry (Entry is not a positive finite number), No Book (the point is not on both legs' mid prices), Long when the
-- Real Long Spread is at least Entry below the Mean, Short when the Real Short Spread is at least Entry above it,
-- None between; Long · Partial and Short · Partial when that side's notional was capped at the recorded levels. The
-- comparisons are on the signed differences; the board shows the deviations as absolute values.
CREATE OR REPLACE FUNCTION etfpair.combo_signal(p_s_bps numeric, p_past_t boolean, p_mu_bps numeric, p_notional numeric, p_mid boolean,
                                                p_s_long_bps numeric, p_s_short_bps numeric, p_partial_long boolean, p_partial_short boolean,
                                                p_entry numeric)
RETURNS text LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
SELECT CASE WHEN p_s_bps IS NULL THEN 'No Anchor' WHEN p_past_t THEN 'Past T' WHEN p_mu_bps IS NULL THEN 'No Mean'
            WHEN NOT coalesce(p_notional > 0, false) THEN 'No Notional' WHEN NOT coalesce(p_entry > 0 AND p_entry < 'Infinity', false) THEN 'No Entry'
            WHEN NOT coalesce(p_mid, false) THEN 'No Book'
            WHEN p_s_long_bps - p_mu_bps <= -p_entry THEN CASE WHEN p_partial_long THEN 'Long · Partial' ELSE 'Long' END
            WHEN p_s_short_bps - p_mu_bps >= p_entry THEN CASE WHEN p_partial_short THEN 'Short · Partial' ELSE 'Short' END
            ELSE 'None' END
$$;

-- mean_ema(value, alpha, restart): exponential mean as a window aggregate over minutes ordered by time, seeded with
-- the first value and again at each row with restart: ema = alpha * value + (1 - alpha) * ema, rounded to 20 decimals
-- (as board 3.1's ema_step did), or every step would add digits up to NUMERIC's limit. One ordered pass gives the EMA of
-- every run (a run restarts where restart is true).
CREATE OR REPLACE FUNCTION etfpair.mean_ema_step(state numeric, value numeric, alpha numeric, restart boolean)
RETURNS numeric LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE AS $$
BEGIN
    RETURN CASE WHEN restart OR state IS NULL THEN value ELSE round(alpha * value + (1 - alpha) * state, 20) END;
END
$$;
CREATE OR REPLACE AGGREGATE etfpair.mean_ema(numeric, numeric, boolean) (SFUNC = etfpair.mean_ema_step, STYPE = numeric);

-- range_stretches gives the queries of combo_range, combo_prices and combo_last the combo's anchor stretches on the
-- venue (combo_anchors) that hold a minute of [p_from_ms, p_to_ms) (whole minutes; every cut of combo_anchors is a
-- whole minute), cut to it, as literal rows: (VALUES ...) AS seg(session_date, from_ms, to_ms, a_bull, a_bear,
-- a_ref, gap_bull, gap_bear, gap_ref, past_t, tv_bull, tv_bear, tv_ref). A minute j is in the stretch with from_ms
-- <= j < to_ms, the second T in the stretch that holds T - 1 ms. The minutes before the first session's close are
-- one more stretch without a session or anchors, from the minute that holds the later of the two legs' first
-- per-second rows (two primary-key lookups; before that minute there is no point, which takes both legs' rows, and
-- in this stretch no mean), and none when a leg has no row: a range that starts years before the data costs
-- what one that starts at the data costs. tv_x is T_v of a split of x in effect in the stretch (the venue's first
-- minute on the new basis, combo_actions), from which to the end of its carrier session x's books and trades from
-- before T_v do not count (book_usable, trade_usable); combo_actions is read only when a split of the combo's ETFs
-- has its instant (action_start) within five days before the range's end and after its start minus five days.
CREATE OR REPLACE FUNCTION etfpair.range_stretches(p_combo text, p_venue text, p_from_ms bigint, p_to_ms bigint)
RETURNS text LANGUAGE plpgsql STABLE AS $$
DECLARE
    v_splits boolean;
    v_rows text;
BEGIN
    SELECT EXISTS (SELECT 1 FROM etfpair.combo c JOIN etfpair.etf_action a ON a.ticker IN (c.bull_ticker, c.bear_ticker, c.ref_ticker)
                   CROSS JOIN LATERAL etfpair.action_start(a.ex_date, a.status) i
                   WHERE c.combo_id = p_combo AND a.kind = 'split' AND i.start_ms < p_to_ms AND i.start_ms + 5 * 86400000 > p_from_ms)
    INTO v_splits;
    WITH sp AS MATERIALIZED (
        SELECT a.session_date, a.leg, a.effective_ms FROM etfpair.combo_actions(p_combo, p_venue) a
        WHERE v_splits AND a.kind = 'split' AND a.status = 'Known' AND a.effective_ms IS NOT NULL
    ), g AS (
        SELECT s.session_date, s.from_ms, s.to_ms, s.a_bull, s.a_bear, s.a_ref, s.gap_bull, s.gap_bear, s.gap_ref, s.past_t
        FROM etfpair.combo_anchors(p_combo, p_venue) s
        UNION ALL
        SELECT NULL, (f.first_ms - 1) / 60000 * 60000, (SELECT min(e.close_ms) FROM etfpair.etf_session e), NULL, NULL, NULL, false, false, false, false
        FROM (SELECT greatest(b.first_ms, s.first_ms) AS first_ms
              FROM etfpair.combo_market m
              CROSS JOIN LATERAL (SELECT min(r.second_ms) AS first_ms FROM etfpair.perp_second r WHERE r.venue = m.venue AND r.symbol = m.bull_symbol) b
              CROSS JOIN LATERAL (SELECT min(r.second_ms) AS first_ms FROM etfpair.perp_second r WHERE r.venue = m.venue AND r.symbol = m.bear_symbol) s
              WHERE m.combo_id = p_combo AND m.venue = p_venue AND b.first_ms IS NOT NULL AND s.first_ms IS NOT NULL) f
    )
    SELECT string_agg(format('(%L::date, %s::bigint, %s::bigint, %L::numeric, %L::numeric, %L::numeric, %L::boolean, %L::boolean, %L::boolean, '
                             '%L::boolean, %L::bigint, %L::bigint, %L::bigint)',
                             g.session_date, greatest(g.from_ms, p_from_ms), least(g.to_ms, p_to_ms), g.a_bull, g.a_bear, g.a_ref,
                             g.gap_bull, g.gap_bear, g.gap_ref, g.past_t, t.tv_bull, t.tv_bear, t.tv_ref), ', ' ORDER BY g.from_ms)
    INTO v_rows
    FROM g
    CROSS JOIN LATERAL (SELECT max(x.effective_ms) FILTER (WHERE x.leg = 'bull') AS tv_bull, max(x.effective_ms) FILTER (WHERE x.leg = 'bear') AS tv_bear,
                               max(x.effective_ms) FILTER (WHERE x.leg = 'ref') AS tv_ref
                        FROM sp x WHERE x.session_date = g.session_date AND x.effective_ms <= g.from_ms) t
    WHERE greatest(g.from_ms, p_from_ms) < least(g.to_ms, p_to_ms);
    RETURN '(' || coalesce('VALUES ' || v_rows,
                           'SELECT NULL::date, NULL::bigint, NULL::bigint, NULL::numeric, NULL::numeric, NULL::numeric, NULL::boolean, NULL::boolean, '
                           'NULL::boolean, NULL::boolean, NULL::bigint, NULL::bigint, NULL::bigint WHERE false')
           || ') AS seg(session_date, from_ms, to_ms, a_bull, a_bear, a_ref, gap_bull, gap_bear, gap_ref, past_t, tv_bull, tv_bear, tv_ref)';
END
$$;

-- range_points_sql is the one definition of the points of the Spread row (DESIGN 1.0-1.7), as the text of a query
-- for combo_range and combo_last: every point at the multiples of p_step_ms from p_first_ms to p_last_ms (both
-- multiples of it) with these columns:
--   ts_ms, session_date, basis ('mid' or 'trade'), past_t, fx, a_bull, a_bear, p_bull, p_bear (the basis prices),
--   s_bps, mu_bps, s_long_bps, s_short_bps, partial_long, partial_short, filled_long, filled_short, book_bull,
--   book_bear, bid12_bull, bid12_bear, ask12_bull, ask12_bear, a_ok, signal.
-- p_stretches is range_stretches' text for at least [p_bar_ms, minute of p_last_ms + 1 min); p_bar_ms the first
-- minute whose bar the means need (combo_range, combo_last); p_ma 'ema' or else the SMA, p_n its minutes (positive),
-- or NULL for no mean: the points, real spreads and depths as ever, mu_bps NULL (combo_range and combo_last pass NULL
-- for a p_n that is not positive).
--   Points: the seconds T of a stretch that is no leg's split halt (gap) at which both legs have a usable book
--   (book_usable: the basis is mid, P_i = (best bid + best ask) / 2) or else both a usable trade (trade_usable: the
--   basis is trade, P_i = the last price); no point otherwise. s = 10^4 * (F * P_bull / A_bull + F * P_bear / A_bear
--   - 2) with the anchors A of the stretch and F, USD per USDT, of the minute that holds T - 1 ms (fx_minute: that
--   minute's or the newest of the five before it). On mid points book_pair gives the real spreads of p_notional and
--   the 1+2 depths, on trade points top2_size gives the 1+2 depths of a leg that has a usable book.
--   Bars: minute j's bar is the row at j + 60 s: both legs' mid prices when both have a usable book, else both trade
--   prices when both have a usable trade, else both legs' candle closes (perp_minute), else none. A run is a stretch
--   of consecutive minutes with bars and F, of one session, on the same legs' anchors and on one side of a T past T
--   (restart at a session's start, an action's instant, T of a dividend still without its amount, a minute without a
--   bar or F). Per leg the mean of F * bar over the run: the SMA of the last p_n bars, or the EMA with alpha = 2 /
--   (p_n + 1) seeded with the run's first bar, defined once the run has p_n bars. A point of minute k takes the mean
--   through minute k - 1 when that minute is of the same session, anchors and side of T as k: mu = 10^4 *
--   (m_bull / A_bull + m_bear / A_bear - 2). The bar of minute k itself is not in it.
--   Where a bar's row is read: from perp_second_minute's end-row columns when the minute's row there is final
--   (perp_minute_final) and the minute began at least 66 minutes before now, from perp_second otherwise. The
--   collector refreshes the last 65 minutes every minute, a minute for the last time 65 min 20 s after it began, and
--   the recorder writes the rows a failed write held back at most an hour late, so such a row has every late write
--   unless the collector made no refresh between that write and then (it was stopped; the backfill scripts refresh
--   what they write).
--   a_ok: the point has both anchors and F. signal: combo_signal. Nothing is rounded.
CREATE OR REPLACE FUNCTION etfpair.range_points_sql(p_venue text, p_bull text, p_bear text, p_ma text, p_n integer, p_first_ms bigint,
                                                    p_last_ms bigint, p_step_ms bigint, p_notional numeric, p_entry numeric, p_bar_ms bigint,
                                                    p_stretches text)
RETURNS text LANGUAGE plpgsql IMMUTABLE AS $$
DECLARE
    v_mean text;
BEGIN
    -- The mean of each leg's F * bar over the run through the minute before: an SMA or an EMA, none without p_n. p_ma
    -- only chooses the text.
    IF p_n IS NULL THEN
        v_mean := 'NULL::numeric AS m_bull, NULL::numeric AS m_bear';
    ELSIF p_ma = 'ema' THEN
        v_mean := format('etfpair.mean_ema(f.fx * f.x_bull, 2.0 / (%1$s::numeric + 1), f.restart) OVER (w ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING) AS m_bull, '
                         'etfpair.mean_ema(f.fx * f.x_bear, 2.0 / (%1$s::numeric + 1), f.restart) OVER (w ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING) AS m_bear', p_n);
    ELSE
        v_mean := format('avg(f.fx * f.x_bull) OVER (w ROWS BETWEEN %1$s PRECEDING AND 1 PRECEDING) AS m_bull, '
                         'avg(f.fx * f.x_bear) OVER (w ROWS BETWEEN %1$s PRECEDING AND 1 PRECEDING) AS m_bear', p_n);
    END IF;
    RETURN format($q$
WITH seg AS MATERIALIZED (
    SELECT * FROM %1$s WHERE NOT (gap_bull OR gap_bear)
), mm AS MATERIALIZED (
    -- Every minute from the first bar to the last point's, with its stretch.
    SELECT g.session_date, g.a_bull, g.a_bear, g.past_t, g.tv_bull, g.tv_bear, m.j
    FROM seg g
    CROSS JOIN LATERAL generate_series(greatest(g.from_ms, %8$s::bigint), least(g.to_ms, %9$s::bigint + 60000) - 60000, 60000) m(j)
), fxm AS MATERIALIZED (
    SELECT q.minute_ms, q.price FROM etfpair.fx_minute q WHERE q.pair = 'USDT-USD' AND q.minute_ms >= %8$s::bigint AND q.minute_ms <= %9$s::bigint
), ends AS MATERIALIZED (
    -- Both legs' final minute rows that have the end row's price or mid (perp_second_minute), of the minutes that began
    -- at least 66 minutes ago: a newer minute may still take a row the recorder held back, after its last refresh.
    SELECT t.symbol, t.minute_ms, t.end_price, t.end_trade_ms, t.end_mid, t.end_book_ms FROM etfpair.perp_second_minute t
    WHERE t.venue = %2$L AND t.symbol IN (%3$L, %4$L) AND t.minute_ms >= %8$s::bigint AND t.minute_ms <= %9$s::bigint
      AND t.minute_ms <= floor(extract(epoch FROM now()) * 1000)::bigint - 3960000
      AND etfpair.perp_minute_final(t.minute_ms, t.refreshed_ms) AND (t.end_price IS NOT NULL OR t.end_mid IS NOT NULL)
), minutes AS MATERIALIZED (
    -- Each minute's F (fx_minute: that minute's, else the newest of the five before it) and what both legs' rows at
    -- its end hold: the price, its trade_ms, the mid of a book usable leaving the split rule aside and its book_ms,
    -- from ends where the minute is there, else from the row in perp_second; book_usable's split rule (a book from
    -- before T_v) and trade_usable are applied here, so the bar is what the row itself gives. Set joins, with a
    -- lookup only for the minutes they miss.
    SELECT mm.session_date, mm.a_bull, mm.a_bear, mm.past_t, mm.tv_bull, mm.tv_bear, mm.j,
           CASE WHEN fe.minute_ms IS NOT NULL THEN fe.price
                ELSE (SELECT q.price FROM etfpair.fx_minute q WHERE q.pair = 'USDT-USD' AND q.minute_ms < mm.j AND q.minute_ms >= mm.j - 300000
                      ORDER BY q.minute_ms DESC LIMIT 1) END AS fx,
           b.price AS price_bull, b.mid IS NOT NULL AND NOT coalesce(b.book_ms < mm.tv_bull, false) AS book_bull,
           etfpair.trade_usable(b.price, b.trade_ms, mm.tv_bull) AS trade_bull, b.mid AS mid_bull,
           s.price AS price_bear, s.mid IS NOT NULL AND NOT coalesce(s.book_ms < mm.tv_bear, false) AS book_bear,
           etfpair.trade_usable(s.price, s.trade_ms, mm.tv_bear) AS trade_bear, s.mid AS mid_bear
    FROM mm
    LEFT JOIN fxm fe ON fe.minute_ms = mm.j
    LEFT JOIN ends eb ON eb.symbol = %3$L AND eb.minute_ms = mm.j
    LEFT JOIN ends es ON es.symbol = %4$L AND es.minute_ms = mm.j
    LEFT JOIN LATERAL (SELECT eb.end_price AS price, eb.end_trade_ms AS trade_ms, eb.end_mid AS mid, eb.end_book_ms AS book_ms
                       WHERE eb.minute_ms IS NOT NULL
                       UNION ALL
                       SELECT r.price, r.trade_ms, CASE WHEN etfpair.book_usable(r.bid_px, r.bid_sz, r.ask_px, r.ask_sz, NULL, NULL)
                                                        THEN (r.bid_px[1] + r.ask_px[1]) / 2 END, r.book_ms
                       FROM etfpair.perp_second r WHERE eb.minute_ms IS NULL AND r.venue = %2$L AND r.symbol = %3$L AND r.second_ms = mm.j + 60000) b ON true
    LEFT JOIN LATERAL (SELECT es.end_price AS price, es.end_trade_ms AS trade_ms, es.end_mid AS mid, es.end_book_ms AS book_ms
                       WHERE es.minute_ms IS NOT NULL
                       UNION ALL
                       SELECT r.price, r.trade_ms, CASE WHEN etfpair.book_usable(r.bid_px, r.bid_sz, r.ask_px, r.ask_sz, NULL, NULL)
                                                        THEN (r.bid_px[1] + r.ask_px[1]) / 2 END, r.book_ms
                       FROM etfpair.perp_second r WHERE es.minute_ms IS NULL AND r.venue = %2$L AND r.symbol = %4$L AND r.second_ms = mm.j + 60000) s ON true
), bars AS (
    -- Minute j's bar: both legs' mid prices, else both trade prices, else both candle closes, else none.
    SELECT mi.session_date, mi.a_bull, mi.a_bear, mi.past_t, mi.tv_bull, mi.tv_bear, mi.j, mi.fx,
           CASE WHEN mi.book_bull AND mi.book_bear THEN mi.mid_bull WHEN mi.trade_bull AND mi.trade_bear THEN mi.price_bull
                ELSE (SELECT c.price FROM etfpair.perp_minute c WHERE c.venue = %2$L AND c.symbol = %3$L AND c.minute_ms = mi.j) END AS x_bull,
           CASE WHEN mi.book_bull AND mi.book_bear THEN mi.mid_bear WHEN mi.trade_bull AND mi.trade_bear THEN mi.price_bear
                ELSE (SELECT c.price FROM etfpair.perp_minute c WHERE c.venue = %2$L AND c.symbol = %4$L AND c.minute_ms = mi.j) END AS x_bear
    FROM minutes mi OFFSET 0
), flagged AS (
    -- A run restarts at a minute without a bar or F, after one, after a missing minute, and where the session, an anchor
    -- or the side of a T past T changes.
    SELECT l.*, NOT coalesce(l.has_bar AND l.prev_has_bar AND l.j - l.prev_j = 60000 AND l.session_date IS NOT DISTINCT FROM l.prev_session
                             AND l.a_bull IS NOT DISTINCT FROM l.prev_a_bull AND l.a_bear IS NOT DISTINCT FROM l.prev_a_bear
                             AND l.past_t = l.prev_past_t, false) AS restart
    FROM (SELECT b.*, b.x_bull IS NOT NULL AND b.x_bear IS NOT NULL AND b.fx IS NOT NULL AS has_bar,
                 lag(b.x_bull IS NOT NULL AND b.x_bear IS NOT NULL AND b.fx IS NOT NULL) OVER w AS prev_has_bar, lag(b.j) OVER w AS prev_j,
                 lag(b.session_date) OVER w AS prev_session, lag(b.a_bull) OVER w AS prev_a_bull, lag(b.a_bear) OVER w AS prev_a_bear,
                 lag(b.past_t) OVER w AS prev_past_t
          FROM bars b WINDOW w AS (ORDER BY b.j)) l
), means AS (
    -- Over the minutes before this one: where the minute before's run began, and the mean through the minute before.
    SELECT f.*, f.prev_j - max(CASE WHEN f.restart THEN f.j END) OVER (w ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING) AS prev_run_ms, %12$s
    FROM flagged f WINDOW w AS (ORDER BY f.j)
), mins AS MATERIALIZED (
    -- The minutes that hold a point, with the mean through the minute before when that minute is of the same session,
    -- anchors and side of T and ends a run of at least p_n bars.
    SELECT q.j, q.session_date, q.a_bull, q.a_bear, q.past_t, q.tv_bull, q.tv_bear, q.fx,
           CASE WHEN q.prev_j = q.j - 60000 AND q.prev_has_bar AND q.prev_run_ms >= (%6$L::bigint - 1) * 60000
                     AND q.session_date IS NOT DISTINCT FROM q.prev_session AND q.a_bull IS NOT DISTINCT FROM q.prev_a_bull
                     AND q.a_bear IS NOT DISTINCT FROM q.prev_a_bear AND q.past_t = q.prev_past_t
                THEN 10000 * (q.m_bull / q.a_bull + q.m_bear / q.a_bear - 2) END AS mu_bps
    FROM means q
    WHERE greatest((q.j / %7$s::bigint + 1) * %7$s::bigint, %10$s::bigint) <= least(q.j + 60000, %11$s::bigint)
), secs AS (
    SELECT mi.session_date, mi.a_bull, mi.a_bear, mi.past_t, mi.fx, mi.mu_bps, t.ts,
           b.price AS price_bull, b.bid_px AS bid_px_bull, b.bid_sz AS bid_sz_bull, b.ask_px AS ask_px_bull, b.ask_sz AS ask_sz_bull,
           s.price AS price_bear, s.bid_px AS bid_px_bear, s.bid_sz AS bid_sz_bear, s.ask_px AS ask_px_bear, s.ask_sz AS ask_sz_bear,
           u.book_bull, u.book_bear, u.trade_bull, u.trade_bear
    FROM mins mi
    CROSS JOIN LATERAL generate_series(greatest((mi.j / %7$s::bigint + 1) * %7$s::bigint, %10$s::bigint), least(mi.j + 60000, %11$s::bigint), %7$s::bigint) t(ts)
    CROSS JOIN LATERAL (SELECT r.* FROM etfpair.perp_second r WHERE r.venue = %2$L AND r.symbol = %3$L AND r.second_ms = t.ts OFFSET 0) b
    CROSS JOIN LATERAL (SELECT r.* FROM etfpair.perp_second r WHERE r.venue = %2$L AND r.symbol = %4$L AND r.second_ms = t.ts OFFSET 0) s
    CROSS JOIN LATERAL (SELECT etfpair.book_usable(b.bid_px, b.bid_sz, b.ask_px, b.ask_sz, b.book_ms, mi.tv_bull) AS book_bull,
                               etfpair.book_usable(s.bid_px, s.bid_sz, s.ask_px, s.ask_sz, s.book_ms, mi.tv_bear) AS book_bear,
                               etfpair.trade_usable(b.price, b.trade_ms, mi.tv_bull) AS trade_bull,
                               etfpair.trade_usable(s.price, s.trade_ms, mi.tv_bear) AS trade_bear OFFSET 0) u
    WHERE u.book_bull AND u.book_bear OR u.trade_bull AND u.trade_bear
), points AS (
    SELECT c.ts, c.session_date, c.basis, c.past_t, c.fx, c.a_bull, c.a_bear, c.p_bull, c.p_bear,
           10000 * (c.fx * c.p_bull / c.a_bull + c.fx * c.p_bear / c.a_bear - 2) AS s_bps, c.mu_bps,
           bp.s_long_bps, bp.s_short_bps, bp.partial_long, bp.partial_short, bp.filled_long, bp.filled_short, c.book_bull, c.book_bear,
           CASE WHEN c.basis = 'mid' THEN bp.bid12_bull WHEN c.book_bull THEN etfpair.top2_size(c.bid_px_bull, c.bid_sz_bull, true) END AS bid12_bull,
           CASE WHEN c.basis = 'mid' THEN bp.bid12_bear WHEN c.book_bear THEN etfpair.top2_size(c.bid_px_bear, c.bid_sz_bear, true) END AS bid12_bear,
           CASE WHEN c.basis = 'mid' THEN bp.ask12_bull WHEN c.book_bull THEN etfpair.top2_size(c.ask_px_bull, c.ask_sz_bull, false) END AS ask12_bull,
           CASE WHEN c.basis = 'mid' THEN bp.ask12_bear WHEN c.book_bear THEN etfpair.top2_size(c.ask_px_bear, c.ask_sz_bear, false) END AS ask12_bear,
           c.a_bull IS NOT NULL AND c.a_bear IS NOT NULL AND c.fx IS NOT NULL AS a_ok
    FROM (SELECT w.*, CASE WHEN w.book_bull AND w.book_bear THEN 'mid' ELSE 'trade' END AS basis,
                 CASE WHEN w.book_bull AND w.book_bear THEN (w.bid_px_bull[1] + w.ask_px_bull[1]) / 2 ELSE w.price_bull END AS p_bull,
                 CASE WHEN w.book_bull AND w.book_bear THEN (w.bid_px_bear[1] + w.ask_px_bear[1]) / 2 ELSE w.price_bear END AS p_bear
          FROM secs w OFFSET 0) c
    LEFT JOIN LATERAL (SELECT * FROM etfpair.book_pair(c.bid_px_bull, c.bid_sz_bull, c.ask_px_bull, c.ask_sz_bull, c.bid_px_bear, c.bid_sz_bear,
                                                       c.ask_px_bear, c.ask_sz_bear, c.a_bull, c.a_bear, c.fx, %5$L::numeric)
                       WHERE c.basis = 'mid') bp ON true
)
SELECT p.ts AS ts_ms, p.session_date, p.basis, p.past_t, p.fx, p.a_bull, p.a_bear, p.p_bull, p.p_bear, p.s_bps, p.mu_bps,
       p.s_long_bps, p.s_short_bps, p.partial_long, p.partial_short, p.filled_long, p.filled_short, p.book_bull, p.book_bear,
       p.bid12_bull, p.bid12_bear, p.ask12_bull, p.ask12_bear, p.a_ok,
       etfpair.combo_signal(p.s_bps, p.past_t, p.mu_bps, %5$L::numeric, p.basis = 'mid', p.s_long_bps, p.s_short_bps, p.partial_long,
                            p.partial_short, %13$L::numeric) AS signal
FROM (SELECT * FROM points OFFSET 0) p
$q$, p_stretches, p_venue, p_bull, p_bear, p_notional, p_n, p_step_ms, p_bar_ms, (p_last_ms - 1) / 60000 * 60000, p_first_ms, p_last_ms,
       v_mean, p_entry);
END
$$;

-- combo_range is what the Spread row draws (the Spread and Deviation charts, Entry Points by Phase; DESIGN 1.1-1.11,
-- 3.1): the points of range_points_sql at the multiples of p_step_ms in (p_from_ms, p_to_ms], and a row with only
-- ts_ms one millisecond after the point before where a point is farther from it than p_step_ms or starts a new
-- session (none before the first point). basis is 'mid' or 'trade'; book_bull and book_bear say whether the leg has a
-- usable book in that second; the 1+2 depths are contracts; a_ok says the point has both anchors and F (the reasons
-- of the board's codes). phase and phase_sort: the etf_phase that holds the point (start_ms < ts_ms <= end_ms) and
-- its place in Entry Points by Phase (Pre-Market 1, Regular 2, After-Hours 3, Overnight 4, Closed 5), given only on
-- points with a Deviation (a spread and a mean). The figures are signed: the board shows the deviations as absolute
-- values. A p_n that is not positive gives the points, real spreads and depths without a mean (mu_bps NULL), a
-- p_step_ms that is not positive no rows. PL/pgSQL so that each call plans its query on the literal values (a SQL
-- function's body takes a generic plan from its sixth call on one connection).
CREATE OR REPLACE FUNCTION etfpair.combo_range(p_combo text, p_venue text, p_ma text, p_n integer, p_from_ms bigint, p_to_ms bigint,
                                               p_step_ms bigint, p_notional numeric, p_entry numeric)
RETURNS TABLE (ts_ms bigint, session_date date, phase text, phase_sort integer, basis text, past_t boolean, s_bps numeric, mu_bps numeric,
               s_long_bps numeric, s_short_bps numeric, partial_long boolean, partial_short boolean, book_bull boolean, book_bear boolean,
               bid12_bull numeric, bid12_bear numeric, ask12_bull numeric, ask12_bear numeric, a_ok boolean, signal text)
LANGUAGE plpgsql STABLE AS $$
DECLARE
    v_bull text;
    v_bear text;
    v_n integer := CASE WHEN p_n > 0 THEN p_n END;   -- the mean's minutes, NULL for no mean
    v_first bigint;          -- the first and the last point
    v_last bigint;
    v_bar bigint;            -- the first minute whose bar the means need
BEGIN
    IF NOT coalesce(p_step_ms > 0, false) THEN
        RETURN;
    END IF;
    v_first := (p_from_ms / p_step_ms + 1) * p_step_ms;
    v_last := p_to_ms / p_step_ms * p_step_ms;
    SELECT m.bull_symbol, m.bear_symbol INTO v_bull, v_bear FROM etfpair.combo_market m WHERE m.combo_id = p_combo AND m.venue = p_venue;
    IF v_bull IS NULL OR NOT coalesce(v_first <= v_last, false) THEN
        RETURN;
    END IF;
    -- The EMA from the start of the session of the first point's minute, the SMA from p_n minutes before it (not
    -- before that session's start: a run never crosses it); from that minute itself before the first close (no
    -- session, no mean) and without a mean.
    SELECT coalesce(max(s.close_ms), (v_first - 1) / 60000 * 60000) INTO v_bar FROM etfpair.etf_session s WHERE s.close_ms <= (v_first - 1) / 60000 * 60000;
    IF v_n IS NULL THEN
        v_bar := (v_first - 1) / 60000 * 60000;
    ELSIF p_ma = 'ema' THEN
        NULL;
    ELSE
        v_bar := greatest(v_bar, (v_first - 1) / 60000 * 60000 - v_n * 60000::bigint);
    END IF;
    RETURN QUERY EXECUTE format($q$
WITH p AS MATERIALIZED (%1$s), marked AS MATERIALIZED (
    SELECT p.*, lag(p.ts_ms) OVER w AS prev_ts, lag(p.session_date) OVER w AS prev_session
    FROM p WINDOW w AS (ORDER BY p.ts_ms)
)
SELECT m.ts_ms, m.session_date, ph.phase, array_position(ARRAY['Pre-Market', 'Regular', 'After-Hours', 'Overnight', 'Closed'], ph.phase),
       m.basis, m.past_t, m.s_bps, m.mu_bps, m.s_long_bps, m.s_short_bps, m.partial_long, m.partial_short, m.book_bull, m.book_bear,
       m.bid12_bull, m.bid12_bear, m.ask12_bull, m.ask12_bear, m.a_ok, m.signal
FROM marked m
LEFT JOIN LATERAL (SELECT e.phase FROM etfpair.etf_phase e WHERE e.start_ms < m.ts_ms AND e.end_ms >= m.ts_ms
                   AND m.s_bps IS NOT NULL AND m.mu_bps IS NOT NULL ORDER BY e.start_ms DESC LIMIT 1) ph ON true
UNION ALL
SELECT m.prev_ts + 1, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL
FROM marked m
WHERE m.prev_ts IS NOT NULL AND (m.ts_ms - m.prev_ts > %2$s::bigint OR m.session_date IS DISTINCT FROM m.prev_session)
ORDER BY 1
$q$, etfpair.range_points_sql(p_venue, v_bull, v_bear, p_ma, v_n, v_first, v_last, p_step_ms, p_notional, p_entry, v_bar,
                              etfpair.range_stretches(p_combo, p_venue, v_bar, (v_last - 1) / 60000 * 60000 + 60000)),
       p_step_ms);
END
$$;

-- combo_prices is what the Prices row draws (the three price charts and Implied Return; DESIGN 1.8): trade prices
-- only. A point is a multiple of p_step_ms in (p_from_ms, p_to_ms], in a stretch that is no leg's split halt, at
-- which both legs have a usable trade (trade_usable); p_ref is the reference's usable trade price, NULL without one
-- and in its own halt. fx is USD per USDT of the minute that holds T - 1 ms, the anchors those of the stretch. A row
-- with only ts_ms breaks the lines as in combo_range. No means, no books.
CREATE OR REPLACE FUNCTION etfpair.combo_prices(p_combo text, p_venue text, p_from_ms bigint, p_to_ms bigint, p_step_ms bigint)
RETURNS TABLE (ts_ms bigint, session_date date, fx numeric, p_bull numeric, p_bear numeric, p_ref numeric,
               a_bull numeric, a_bear numeric, a_ref numeric)
LANGUAGE plpgsql STABLE AS $$
DECLARE
    v_bull text;
    v_bear text;
    v_ref text;
    v_first bigint;
    v_last bigint;
BEGIN
    IF NOT coalesce(p_step_ms > 0, false) THEN
        RETURN;
    END IF;
    v_first := (p_from_ms / p_step_ms + 1) * p_step_ms;
    v_last := p_to_ms / p_step_ms * p_step_ms;
    SELECT m.bull_symbol, m.bear_symbol, m.ref_symbol INTO v_bull, v_bear, v_ref FROM etfpair.combo_market m
    WHERE m.combo_id = p_combo AND m.venue = p_venue;
    IF v_bull IS NULL OR NOT coalesce(v_first <= v_last, false) THEN
        RETURN;
    END IF;
    RETURN QUERY EXECUTE format($q$
WITH seg AS MATERIALIZED (
    SELECT * FROM %1$s WHERE NOT (gap_bull OR gap_bear)
), mins AS (
    -- The minutes that hold a point, with their stretch and F.
    SELECT g.*, m.j, f.price AS fx
    FROM seg g
    CROSS JOIN LATERAL generate_series(g.from_ms, g.to_ms - 60000, 60000) m(j)
    LEFT JOIN LATERAL (SELECT q.price FROM etfpair.fx_minute q WHERE q.pair = 'USDT-USD' AND q.minute_ms <= m.j AND q.minute_ms >= m.j - 300000
                       ORDER BY q.minute_ms DESC LIMIT 1) f ON true
    WHERE greatest((m.j / %6$s::bigint + 1) * %6$s::bigint, %7$s::bigint) <= least(m.j + 60000, %8$s::bigint)
), pts AS MATERIALIZED (
    SELECT t.ts, mi.session_date, mi.fx, b.price AS p_bull, s.price AS p_bear,
           CASE WHEN NOT mi.gap_ref AND etfpair.trade_usable(r.price, r.trade_ms, mi.tv_ref) THEN r.price END AS p_ref,
           mi.a_bull, mi.a_bear, mi.a_ref
    FROM mins mi
    CROSS JOIN LATERAL generate_series(greatest((mi.j / %6$s::bigint + 1) * %6$s::bigint, %7$s::bigint), least(mi.j + 60000, %8$s::bigint), %6$s::bigint) t(ts)
    CROSS JOIN LATERAL (SELECT x.price, x.trade_ms FROM etfpair.perp_second x WHERE x.venue = %2$L AND x.symbol = %3$L AND x.second_ms = t.ts OFFSET 0) b
    CROSS JOIN LATERAL (SELECT x.price, x.trade_ms FROM etfpair.perp_second x WHERE x.venue = %2$L AND x.symbol = %4$L AND x.second_ms = t.ts OFFSET 0) s
    LEFT JOIN LATERAL (SELECT x.price, x.trade_ms FROM etfpair.perp_second x WHERE x.venue = %2$L AND x.symbol = %5$L AND x.second_ms = t.ts OFFSET 0) r ON true
    WHERE etfpair.trade_usable(b.price, b.trade_ms, mi.tv_bull) AND etfpair.trade_usable(s.price, s.trade_ms, mi.tv_bear)
), marked AS MATERIALIZED (
    SELECT p.*, lag(p.ts) OVER w AS prev_ts, lag(p.session_date) OVER w AS prev_session FROM pts p WINDOW w AS (ORDER BY p.ts)
)
SELECT m.ts, m.session_date, m.fx, m.p_bull, m.p_bear, m.p_ref, m.a_bull, m.a_bear, m.a_ref FROM marked m
UNION ALL
SELECT m.prev_ts + 1, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL FROM marked m
WHERE m.prev_ts IS NOT NULL AND (m.ts - m.prev_ts > %6$s::bigint OR m.session_date IS DISTINCT FROM m.prev_session)
ORDER BY 1
$q$, etfpair.range_stretches(p_combo, p_venue, (v_first - 1) / 60000 * 60000, (v_last - 1) / 60000 * 60000 + 60000), p_venue, v_bull, v_bear,
       v_ref, p_step_ms, v_first, v_last);
END
$$;

-- combo_last gives the snapshot strips their two newest seconds as of p_now_ms (the board passes now; DESIGN 1.12),
-- each in (p_now_ms - 10 min, p_now_ms] and in a stretch that is no leg's split halt:
--   kind 'prices': the newest second at which both legs and the reference have a usable trade: ts_ms,
--     session_date, fx, the anchors a_* and the trade prices p_* (the other columns NULL);
--   kind 'basis': the newest second at which both legs have a basis price, the point of range_points_sql there
--     (p_bull, p_bear its basis prices, p_ref the reference's usable trade price or NULL, a_ref its anchor) with the
--     filled USDT of each side and the anchor status of its session (combo_sessions: NAV, Bell or Missing).
-- A kind without such a second has no row; a p_n that is not positive gives the basis row without a mean.
CREATE OR REPLACE FUNCTION etfpair.combo_last(p_combo text, p_venue text, p_ma text, p_n integer, p_notional numeric, p_entry numeric,
                                              p_now_ms bigint)
RETURNS TABLE (kind text, ts_ms bigint, session_date date, basis text, past_t boolean, fx numeric,
               a_bull numeric, a_bear numeric, a_ref numeric, p_bull numeric, p_bear numeric, p_ref numeric,
               s_bps numeric, mu_bps numeric, s_long_bps numeric, s_short_bps numeric, partial_long boolean,
               partial_short boolean, filled_long numeric, filled_short numeric, book_bull boolean, book_bear boolean,
               bid12_bull numeric, bid12_bear numeric, ask12_bull numeric, ask12_bear numeric,
               anchor_status text, signal text)
LANGUAGE plpgsql STABLE AS $$
DECLARE
    v_bull text;
    v_bear text;
    v_ref text;
    v_n integer := CASE WHEN p_n > 0 THEN p_n END;                 -- the mean's minutes, NULL for no mean
    v_kmin bigint := (p_now_ms - 600000) / 60000 * 60000;          -- the minutes of the ten
    v_kmax bigint := (p_now_ms - 1) / 60000 * 60000;
    v_bar bigint;
    v_seg text;
    v_basis bigint;
    v_prices bigint;
BEGIN
    SELECT m.bull_symbol, m.bear_symbol, m.ref_symbol INTO v_bull, v_bear, v_ref FROM etfpair.combo_market m
    WHERE m.combo_id = p_combo AND m.venue = p_venue;
    IF v_bull IS NULL OR p_now_ms IS NULL THEN
        RETURN;
    END IF;
    -- The stretches from the first bar any of the ten minutes needs (as combo_range) to the last.
    SELECT coalesce(max(s.close_ms), v_kmin) INTO v_bar FROM etfpair.etf_session s WHERE s.close_ms <= v_kmin;
    IF v_n IS NULL THEN
        v_bar := v_kmin;
    ELSIF p_ma = 'ema' THEN
        NULL;
    ELSE
        v_bar := greatest(v_bar, v_kmin - v_n * 60000::bigint);
    END IF;
    v_seg := etfpair.range_stretches(p_combo, p_venue, v_bar, v_kmax + 60000);
    EXECUTE format($q$
WITH seg AS MATERIALIZED (SELECT * FROM %1$s WHERE NOT (gap_bull OR gap_bear))
SELECT (SELECT b.second_ms FROM etfpair.perp_second b
        CROSS JOIN LATERAL (SELECT g.tv_bull, g.tv_bear FROM seg g WHERE g.from_ms <= (b.second_ms - 1) / 60000 * 60000
                            AND g.to_ms > (b.second_ms - 1) / 60000 * 60000 LIMIT 1) g
        CROSS JOIN LATERAL (SELECT x.* FROM etfpair.perp_second x WHERE x.venue = %2$L AND x.symbol = %4$L AND x.second_ms = b.second_ms OFFSET 0) s
        WHERE b.venue = %2$L AND b.symbol = %3$L AND b.second_ms > %6$s::bigint - 600000 AND b.second_ms <= %6$s::bigint
          AND (etfpair.book_usable(b.bid_px, b.bid_sz, b.ask_px, b.ask_sz, b.book_ms, g.tv_bull)
               AND etfpair.book_usable(s.bid_px, s.bid_sz, s.ask_px, s.ask_sz, s.book_ms, g.tv_bear)
               OR etfpair.trade_usable(b.price, b.trade_ms, g.tv_bull) AND etfpair.trade_usable(s.price, s.trade_ms, g.tv_bear))
        ORDER BY b.second_ms DESC LIMIT 1),
       (SELECT b.second_ms FROM etfpair.perp_second b
        CROSS JOIN LATERAL (SELECT g.tv_bull, g.tv_bear, g.tv_ref, g.gap_ref FROM seg g WHERE g.from_ms <= (b.second_ms - 1) / 60000 * 60000
                            AND g.to_ms > (b.second_ms - 1) / 60000 * 60000 LIMIT 1) g
        CROSS JOIN LATERAL (SELECT x.price, x.trade_ms FROM etfpair.perp_second x WHERE x.venue = %2$L AND x.symbol = %4$L AND x.second_ms = b.second_ms OFFSET 0) s
        CROSS JOIN LATERAL (SELECT x.price, x.trade_ms FROM etfpair.perp_second x WHERE x.venue = %2$L AND x.symbol = %5$L AND x.second_ms = b.second_ms OFFSET 0) r
        WHERE b.venue = %2$L AND b.symbol = %3$L AND b.second_ms > %6$s::bigint - 600000 AND b.second_ms <= %6$s::bigint
          AND etfpair.trade_usable(b.price, b.trade_ms, g.tv_bull) AND etfpair.trade_usable(s.price, s.trade_ms, g.tv_bear)
          AND NOT g.gap_ref AND etfpair.trade_usable(r.price, r.trade_ms, g.tv_ref)
        ORDER BY b.second_ms DESC LIMIT 1)
$q$, v_seg, p_venue, v_bull, v_bear, v_ref, p_now_ms) INTO v_basis, v_prices;
    IF v_basis IS NOT NULL THEN
        RETURN QUERY EXECUTE format($q$
SELECT 'basis'::text, p.ts_ms, p.session_date, p.basis, p.past_t, p.fx, p.a_bull, p.a_bear, g.a_ref, p.p_bull, p.p_bear,
       CASE WHEN NOT g.gap_ref AND etfpair.trade_usable(r.price, r.trade_ms, g.tv_ref) THEN r.price END,
       p.s_bps, p.mu_bps, p.s_long_bps, p.s_short_bps, p.partial_long, p.partial_short, p.filled_long, p.filled_short, p.book_bull, p.book_bear,
       p.bid12_bull, p.bid12_bear, p.ask12_bull, p.ask12_bear,
       (SELECT cs.status FROM etfpair.combo_sessions(%3$L) cs WHERE cs.session_date = p.session_date), p.signal
FROM (%1$s) p
CROSS JOIN LATERAL (SELECT g.a_ref, g.gap_ref, g.tv_ref FROM (SELECT * FROM %2$s) g
                    WHERE g.from_ms <= (p.ts_ms - 1) / 60000 * 60000 AND g.to_ms > (p.ts_ms - 1) / 60000 * 60000) g
LEFT JOIN LATERAL (SELECT x.price, x.trade_ms FROM etfpair.perp_second x WHERE x.venue = %4$L AND x.symbol = %5$L AND x.second_ms = p.ts_ms OFFSET 0) r ON true
$q$, etfpair.range_points_sql(p_venue, v_bull, v_bear, p_ma, v_n, v_basis, v_basis, 1000, p_notional, p_entry, v_bar, v_seg),
             v_seg, p_combo, p_venue, v_ref);
    END IF;
    IF v_prices IS NOT NULL THEN
        RETURN QUERY EXECUTE format($q$
SELECT 'prices'::text, %5$s::bigint, g.session_date, NULL::text, NULL::boolean, f.price, g.a_bull, g.a_bear, g.a_ref, b.price, s.price, r.price,
       NULL::numeric, NULL::numeric, NULL::numeric, NULL::numeric, NULL::boolean, NULL::boolean, NULL::numeric, NULL::numeric, NULL::boolean,
       NULL::boolean, NULL::numeric, NULL::numeric, NULL::numeric, NULL::numeric, NULL::text, NULL::text
FROM (SELECT * FROM %1$s) g
JOIN etfpair.perp_second b ON b.venue = %2$L AND b.symbol = %3$L AND b.second_ms = %5$s::bigint
JOIN etfpair.perp_second s ON s.venue = %2$L AND s.symbol = %4$L AND s.second_ms = %5$s::bigint
JOIN etfpair.perp_second r ON r.venue = %2$L AND r.symbol = %6$L AND r.second_ms = %5$s::bigint
LEFT JOIN LATERAL (SELECT q.price FROM etfpair.fx_minute q WHERE q.pair = 'USDT-USD' AND q.minute_ms <= (%5$s::bigint - 1) / 60000 * 60000
                   AND q.minute_ms >= (%5$s::bigint - 1) / 60000 * 60000 - 300000 ORDER BY q.minute_ms DESC LIMIT 1) f ON true
WHERE g.from_ms <= (%5$s::bigint - 1) / 60000 * 60000 AND g.to_ms > (%5$s::bigint - 1) / 60000 * 60000
$q$, v_seg, p_venue, v_bull, v_bear, v_prices, v_ref);
    END IF;
END
$$;

-- The per-second rows minute by minute, per contract and minute (the seconds T with T - 1 ms in [minute_ms,
-- minute_ms + 60 s)): for Data Coverage (DESIGN 1.13, 3.3) the rows with a price (seconds), with a best bid and a
-- best ask (books), the fewest levels the thinner side and the most the deeper side of those books held; for the
-- minute bars of the means (range_points_sql) what the minute's last row, the one at T = minute_ms + 60 s, holds:
-- its price and trade_ms (end_price, end_trade_ms), the mid (best bid + best ask) / 2 of its book when the book is
-- usable leaving the split rule aside (book_usable with no T_v; end_mid, else NULL) and its book_ms (end_book_ms),
-- all NULL without that row; and when the minute was aggregated (refreshed_ms: the aggregating transaction's start,
-- so every row committed before it was seen). The collector refreshes the last 65 minutes every minute; a whole
-- minute is read from here only once it was aggregated at least 10 s after its end (perp_minute_final) and it began
-- at least 66 minutes ago, after the collector's last refresh of it (range_points_sql, perp_coverage). end_mid holds
-- book_usable's rule as it was when the minute was aggregated: a change to the rule needs the summary refilled.
CREATE TABLE IF NOT EXISTS etfpair.perp_second_minute (
    venue        text    NOT NULL,
    symbol       text    NOT NULL,
    minute_ms    bigint  NOT NULL,
    seconds      integer NOT NULL,
    books        integer NOT NULL,
    least_levels integer,
    most_levels  integer,
    refreshed_ms bigint  NOT NULL,
    end_price    numeric,
    end_trade_ms bigint,
    end_mid      numeric,
    end_book_ms  bigint,
    PRIMARY KEY (venue, symbol, minute_ms)
);
-- A table from before the end-row columns gets them, NULL in its rows until a refresh rewrites their minute; a minute
-- without them takes its bar from perp_second.
ALTER TABLE etfpair.perp_second_minute ADD COLUMN IF NOT EXISTS end_price numeric, ADD COLUMN IF NOT EXISTS end_trade_ms bigint,
                                       ADD COLUMN IF NOT EXISTS end_mid numeric, ADD COLUMN IF NOT EXISTS end_book_ms bigint;

-- perp_minute_final: a minute's row of perp_second_minute was aggregated at least 10 s after the minute ended (the
-- recorder's rows of a second are in the table by T + 7.4 s; a row the recorder held back after a failed write
-- arrives later and the next refresh rewrites the minute).
CREATE OR REPLACE FUNCTION etfpair.perp_minute_final(p_minute_ms bigint, p_refreshed_ms bigint)
RETURNS boolean LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
SELECT p_minute_ms + 70000 <= p_refreshed_ms
$$;

-- refresh_perp_second_minute aggregates again every minute that holds a second T in (p_from_ms, p_to_ms] of the
-- combos' contracts (combo_market), writes a minute's row when its figures changed, or when the row on record was
-- not final and this one is (perp_minute_final), in key order, and deletes the row of a minute in the range that has
-- no per-second row any more; it returns the rows written and deleted. One refresh at a time: the second statement
-- locks the table in SHARE ROW EXCLUSIVE mode, which waits for any other refresh's transaction to end (a backfill
-- file's, a day of the history fill, the collector's), and the aggregation, a statement of its own, then sees every
-- row that transaction wrote; otherwise the one with the older snapshot could write a minute's older figures over
-- newer ones, final and never corrected. The mode conflicts with itself and with writes, not with reads, so the
-- board's queries never wait; it also conflicts with VACUUM, ANALYZE and index builds on the table, so a manual
-- VACUUM or ANALYZE of it makes refreshes wait until it ends. A role that may only read cannot take it (it needs
-- UPDATE, DELETE or TRUNCATE on the table), so the board's reader cannot hold refreshes up (it could an advisory
-- lock). The first statement locks combo_market, which the aggregation reads, before the summary table: the same
-- order as this file, which the collector applies in one transaction at its start (combo_market's ALTER near the
-- top, perp_second_minute's at the end); the other order deadlocks with a refresh already in a backfill file's
-- transaction when the collector starts. The collector calls it
-- every minute for the last 65 minutes (the recorder holds back rows of a failed write for up to an hour), the
-- backfill scripts for the range they wrote. Not SECURITY DEFINER, and only its owner (the writer role) may run it:
-- the board's SQL takes typed text, so a reader must not be able to start an aggregation.
CREATE OR REPLACE FUNCTION etfpair.refresh_perp_second_minute(p_from_ms bigint, p_to_ms bigint)
RETURNS integer LANGUAGE sql VOLATILE AS $$
LOCK TABLE etfpair.combo_market IN ACCESS SHARE MODE;
LOCK TABLE etfpair.perp_second_minute IN SHARE ROW EXCLUSIVE MODE;
WITH k AS (
    SELECT DISTINCT m.venue, u.symbol FROM etfpair.combo_market m CROSS JOIN LATERAL unnest(ARRAY[m.bull_symbol, m.bear_symbol, m.ref_symbol]) u(symbol)
    WHERE u.symbol <> ''
), a AS MATERIALIZED (
    SELECT k.venue, k.symbol, x.*
    FROM k
    CROSS JOIN LATERAL (
        SELECT (r.second_ms - 1) / 60000 * 60000 AS minute_ms, count(r.price)::integer AS seconds,
               count(*) FILTER (WHERE r.bid_px[1] IS NOT NULL AND r.ask_px[1] IS NOT NULL)::integer AS books,
               min(least(cardinality(r.bid_px), cardinality(r.ask_px))) FILTER (WHERE r.bid_px[1] IS NOT NULL AND r.ask_px[1] IS NOT NULL) AS least_levels,
               max(greatest(cardinality(r.bid_px), cardinality(r.ask_px))) FILTER (WHERE r.bid_px[1] IS NOT NULL AND r.ask_px[1] IS NOT NULL) AS most_levels,
               max(CASE WHEN r.second_ms % 60000 = 0 THEN r.price END) AS end_price,
               max(CASE WHEN r.second_ms % 60000 = 0 THEN r.trade_ms END) AS end_trade_ms,
               max(CASE WHEN r.second_ms % 60000 <> 0 THEN NULL
                        WHEN etfpair.book_usable(r.bid_px, r.bid_sz, r.ask_px, r.ask_sz, NULL, NULL) THEN (r.bid_px[1] + r.ask_px[1]) / 2 END) AS end_mid,
               max(CASE WHEN r.second_ms % 60000 = 0 THEN r.book_ms END) AS end_book_ms
        FROM etfpair.perp_second r
        WHERE r.venue = k.venue AND r.symbol = k.symbol AND r.second_ms > p_from_ms / 60000 * 60000 AND r.second_ms <= (p_to_ms - 1) / 60000 * 60000 + 60000
        GROUP BY 1) x
), gone AS (
    DELETE FROM etfpair.perp_second_minute t USING k
    WHERE t.venue = k.venue AND t.symbol = k.symbol AND t.minute_ms >= p_from_ms / 60000 * 60000 AND t.minute_ms <= (p_to_ms - 1) / 60000 * 60000
      AND NOT EXISTS (SELECT 1 FROM a WHERE a.venue = t.venue AND a.symbol = t.symbol AND a.minute_ms = t.minute_ms)
    RETURNING 1
), written AS (
    INSERT INTO etfpair.perp_second_minute AS t (venue, symbol, minute_ms, seconds, books, least_levels, most_levels, refreshed_ms,
                                                 end_price, end_trade_ms, end_mid, end_book_ms)
    SELECT a.venue, a.symbol, a.minute_ms, a.seconds, a.books, a.least_levels, a.most_levels, floor(extract(epoch FROM now()) * 1000)::bigint,
           a.end_price, a.end_trade_ms, a.end_mid, a.end_book_ms
    FROM a
    ORDER BY 1, 2, 3
    ON CONFLICT (venue, symbol, minute_ms) DO UPDATE
    SET seconds = EXCLUDED.seconds, books = EXCLUDED.books, least_levels = EXCLUDED.least_levels, most_levels = EXCLUDED.most_levels,
        refreshed_ms = EXCLUDED.refreshed_ms, end_price = EXCLUDED.end_price, end_trade_ms = EXCLUDED.end_trade_ms, end_mid = EXCLUDED.end_mid,
        end_book_ms = EXCLUDED.end_book_ms
    WHERE (t.seconds, t.books, t.least_levels, t.most_levels, t.end_price, t.end_trade_ms, t.end_mid, t.end_book_ms)
          IS DISTINCT FROM (EXCLUDED.seconds, EXCLUDED.books, EXCLUDED.least_levels, EXCLUDED.most_levels, EXCLUDED.end_price,
                            EXCLUDED.end_trade_ms, EXCLUDED.end_mid, EXCLUDED.end_book_ms)
       OR NOT etfpair.perp_minute_final(t.minute_ms, t.refreshed_ms) AND etfpair.perp_minute_final(EXCLUDED.minute_ms, EXCLUDED.refreshed_ms)
    RETURNING 1
)
SELECT ((SELECT count(*) FROM written) + (SELECT count(*) FROM gone))::integer
$$;
REVOKE EXECUTE ON FUNCTION etfpair.refresh_perp_second_minute(bigint, bigint) FROM PUBLIC;

-- perp_coverage gives Data Coverage the counts of one contract's per-second rows over the seconds T in (p_from_ms,
-- p_to_ms], exactly those of counting perp_second (3.1's query): the rows with a price, the rows with a best bid and
-- a best ask, and over those the fewest levels the thinner side and the most the deeper side held. A whole minute of
-- the range is read from perp_second_minute when its row there is final (perp_minute_final) and the minute began at
-- least 66 minutes ago (after the collector's last refresh of it, as range_points_sql reads its bars); every other
-- second (the range's ends, the last 66 minutes, a minute not aggregated yet, or not since it ended, or without
-- rows) is counted in perp_second.
CREATE OR REPLACE FUNCTION etfpair.perp_coverage(p_venue text, p_symbol text, p_from_ms bigint, p_to_ms bigint)
RETURNS TABLE (seconds bigint, books bigint, least_levels integer, most_levels integer)
LANGUAGE sql STABLE AS $$
WITH tm AS MATERIALIZED (
    SELECT t.minute_ms, t.seconds, t.books, t.least_levels, t.most_levels FROM etfpair.perp_second_minute t
    WHERE t.venue = p_venue AND t.symbol = p_symbol AND t.minute_ms >= p_from_ms AND t.minute_ms + 60000 <= p_to_ms
      AND t.minute_ms <= floor(extract(epoch FROM now()) * 1000)::bigint - 3960000
      AND etfpair.perp_minute_final(t.minute_ms, t.refreshed_ms)
), rest AS (
    -- The seconds no final minute covers, as ranges (lo, hi]: before, between and after them.
    SELECT g.lo, g.hi FROM (
        SELECT coalesce(lag(tm.minute_ms + 60000) OVER (ORDER BY tm.minute_ms), p_from_ms) AS lo, tm.minute_ms AS hi FROM tm
        UNION ALL
        SELECT coalesce(max(tm.minute_ms) + 60000, p_from_ms), p_to_ms FROM tm) g
    WHERE g.hi > g.lo
), raw AS (
    SELECT count(r.price) AS seconds, count(*) FILTER (WHERE r.bid_px[1] IS NOT NULL AND r.ask_px[1] IS NOT NULL) AS books,
           min(least(cardinality(r.bid_px), cardinality(r.ask_px))) FILTER (WHERE r.bid_px[1] IS NOT NULL AND r.ask_px[1] IS NOT NULL) AS least_levels,
           max(greatest(cardinality(r.bid_px), cardinality(r.ask_px))) FILTER (WHERE r.bid_px[1] IS NOT NULL AND r.ask_px[1] IS NOT NULL) AS most_levels
    FROM rest g
    CROSS JOIN LATERAL (SELECT x.price, x.bid_px, x.ask_px FROM etfpair.perp_second x
                        WHERE x.venue = p_venue AND x.symbol = p_symbol AND x.second_ms > g.lo AND x.second_ms <= g.hi OFFSET 0) r
)
SELECT coalesce((SELECT sum(tm.seconds) FROM tm), 0) + raw.seconds, coalesce((SELECT sum(tm.books) FROM tm), 0) + raw.books,
       least((SELECT min(tm.least_levels) FROM tm), raw.least_levels), greatest((SELECT max(tm.most_levels) FROM tm), raw.most_levels)
FROM raw
$$;

-- ---------------------------------------------------------------------------------------------------------------
-- Board 3.4 (OWNER.md item 61; dashboard/SPEC.md section 4, "3.4 的函数"): the trading program's pricing levels and
-- sigma. New overloads of range_points_sql, combo_range and combo_last (one more argument, p_sigma_n, and the new
-- columns); the 3.2 versions above stay, unchanged, for a page opened before 3.4 was published, until a later
-- collector drops them. Same rules as 3.2 except:
--   Pricing levels (PRD 3.2, item 57): a leg's first L raw levels of a side, L = pricing_levels(its tick): 2 at a
--   0.01 tick (SOXL), 5 at a 0.001 tick (Bitget SOXS), the levels as recorded, never folded to 0.01 and without a
--   dust threshold. The real spreads sweep N through those levels only (book_pair on them: the volume-weighted price,
--   both legs' q * A equal, capped at their totals, then Partial); a side with fewer than L levels has none. The
--   depths are each side's total of those levels, contracts and USDT (sum of price * size), not the program's
--   takeable half.
--   Sigma (PRD 4.4): the population standard deviation of the minute bars' spread, 10^4 * (F * bar_bull / A_bull + F *
--   bar_bear / A_bear - 2), over the last p_sigma_n bars of the run through the minute before the point, defined once
--   the run has p_sigma_n bars: the mean's runs, restarts and bars.
-- ---------------------------------------------------------------------------------------------------------------

-- pricing_levels is the one definition of a leg's pricing levels by its tick: 5 below a 0.01 tick (Bitget SOXS's
-- 0.001: five levels span about the prices of two at 0.01, item 15's alignment), else 2; NULL without a tick.
CREATE OR REPLACE FUNCTION etfpair.pricing_levels(p_tick numeric)
RETURNS integer LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
SELECT CASE WHEN p_tick < 0.01 THEN 5 WHEN p_tick >= 0.01 THEN 2 END
$$;

-- first_levels: a side's first p_levels levels, NULL when it has fewer (no pricing level) or no levels count.
CREATE OR REPLACE FUNCTION etfpair.first_levels(p_px numeric[], p_levels integer)
RETURNS numeric[] LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
SELECT CASE WHEN cardinality(p_px) >= p_levels THEN p_px[1:p_levels] END
$$;

-- level_depth is the one definition of a side's depth: the total size of its first p_levels levels (contracts) and
-- what they cost (USDT, sum of price * size); NULL for a side with fewer levels or none (the caller passes NULL for a
-- leg without a usable book).
CREATE OR REPLACE FUNCTION etfpair.level_depth(p_px numeric[], p_sz numeric[], p_levels integer, OUT ct numeric, OUT usdt numeric)
LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE AS $$
BEGIN
    IF NOT coalesce(cardinality(p_px) >= p_levels AND p_levels > 0, false) THEN
        RETURN;
    END IF;
    ct := 0; usdt := 0;
    FOR i IN 1 .. p_levels LOOP
        ct := ct + p_sz[i]; usdt := usdt + p_px[i] * p_sz[i];
    END LOOP;
END
$$;

-- range_points_sql (3.4): the points of the Spread row as in the 3.2 version, with sigma_bps and the pricing levels
-- (this section's head): p_sigma_n sigma's minutes (positive) or NULL for none; p_levels_bull, p_levels_bear the legs'
-- pricing levels. Columns: ts_ms, session_date, basis, past_t, fx, a_bull, a_bear, p_bull, p_bear, s_bps, mu_bps,
-- sigma_bps, s_long_bps, s_short_bps, partial_long, partial_short, filled_long, filled_short, book_bull, book_bear,
-- bid_ct_bull, bid_ct_bear, ask_ct_bull, ask_ct_bear, bid_usdt_bull, bid_usdt_bear, ask_usdt_bull, ask_usdt_bear, a_ok,
-- signal. The real spreads only on mid points (both legs' usable books); a leg's depths wherever it has a usable book.
-- p_bar_ms must reach the more of p_n and p_sigma_n minutes back (SMA) or the session's start (EMA).
CREATE OR REPLACE FUNCTION etfpair.range_points_sql(p_venue text, p_bull text, p_bear text, p_ma text, p_n integer, p_sigma_n integer,
                                                    p_first_ms bigint, p_last_ms bigint, p_step_ms bigint, p_notional numeric, p_entry numeric,
                                                    p_bar_ms bigint, p_stretches text, p_levels_bull integer, p_levels_bear integer)
RETURNS text LANGUAGE plpgsql IMMUTABLE AS $$
DECLARE
    v_mean text;
    v_sigma text;
BEGIN
    -- The mean of each leg's F * bar over the run through the minute before: an SMA or an EMA, none without p_n. p_ma
    -- only chooses the text.
    IF p_n IS NULL THEN
        v_mean := 'NULL::numeric AS m_bull, NULL::numeric AS m_bear';
    ELSIF p_ma = 'ema' THEN
        v_mean := format('etfpair.mean_ema(f.fx * f.x_bull, 2.0 / (%1$s::numeric + 1), f.restart) OVER (w ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING) AS m_bull, '
                         'etfpair.mean_ema(f.fx * f.x_bear, 2.0 / (%1$s::numeric + 1), f.restart) OVER (w ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING) AS m_bear', p_n);
    ELSE
        v_mean := format('avg(f.fx * f.x_bull) OVER (w ROWS BETWEEN %1$s PRECEDING AND 1 PRECEDING) AS m_bull, '
                         'avg(f.fx * f.x_bear) OVER (w ROWS BETWEEN %1$s PRECEDING AND 1 PRECEDING) AS m_bear', p_n);
    END IF;
    -- sigma: the population standard deviation of the minute bars' spread over the last p_sigma_n bars of the run through
    -- the minute before (the anchors are those of the run's minutes), none without p_sigma_n.
    IF p_sigma_n IS NULL THEN
        v_sigma := 'NULL::numeric AS sd';
    ELSE
        v_sigma := format('stddev_pop(10000 * (f.fx * f.x_bull / f.a_bull + f.fx * f.x_bear / f.a_bear - 2)) OVER (w ROWS BETWEEN %1$s PRECEDING AND 1 PRECEDING) AS sd',
                          p_sigma_n);
    END IF;
    RETURN format($q$
WITH seg AS MATERIALIZED (
    SELECT * FROM %1$s WHERE NOT (gap_bull OR gap_bear)
), mm AS MATERIALIZED (
    -- Every minute from the first bar to the last point's, with its stretch.
    SELECT g.session_date, g.a_bull, g.a_bear, g.past_t, g.tv_bull, g.tv_bear, m.j
    FROM seg g
    CROSS JOIN LATERAL generate_series(greatest(g.from_ms, %8$s::bigint), least(g.to_ms, %9$s::bigint + 60000) - 60000, 60000) m(j)
), fxm AS MATERIALIZED (
    SELECT q.minute_ms, q.price FROM etfpair.fx_minute q WHERE q.pair = 'USDT-USD' AND q.minute_ms >= %8$s::bigint AND q.minute_ms <= %9$s::bigint
), ends AS MATERIALIZED (
    -- Both legs' final minute rows that have the end row's price or mid (perp_second_minute), of the minutes that began
    -- at least 66 minutes ago: a newer minute may still take a row the recorder held back, after its last refresh.
    SELECT t.symbol, t.minute_ms, t.end_price, t.end_trade_ms, t.end_mid, t.end_book_ms FROM etfpair.perp_second_minute t
    WHERE t.venue = %2$L AND t.symbol IN (%3$L, %4$L) AND t.minute_ms >= %8$s::bigint AND t.minute_ms <= %9$s::bigint
      AND t.minute_ms <= floor(extract(epoch FROM now()) * 1000)::bigint - 3960000
      AND etfpair.perp_minute_final(t.minute_ms, t.refreshed_ms) AND (t.end_price IS NOT NULL OR t.end_mid IS NOT NULL)
), minutes AS MATERIALIZED (
    -- Each minute's F (fx_minute: that minute's, else the newest of the five before it) and what both legs' rows at
    -- its end hold: the price, its trade_ms, the mid of a book usable leaving the split rule aside and its book_ms,
    -- from ends where the minute is there, else from the row in perp_second; book_usable's split rule (a book from
    -- before T_v) and trade_usable are applied here, so the bar is what the row itself gives. Set joins, with a
    -- lookup only for the minutes they miss.
    SELECT mm.session_date, mm.a_bull, mm.a_bear, mm.past_t, mm.tv_bull, mm.tv_bear, mm.j,
           CASE WHEN fe.minute_ms IS NOT NULL THEN fe.price
                ELSE (SELECT q.price FROM etfpair.fx_minute q WHERE q.pair = 'USDT-USD' AND q.minute_ms < mm.j AND q.minute_ms >= mm.j - 300000
                      ORDER BY q.minute_ms DESC LIMIT 1) END AS fx,
           b.price AS price_bull, b.mid IS NOT NULL AND NOT coalesce(b.book_ms < mm.tv_bull, false) AS book_bull,
           etfpair.trade_usable(b.price, b.trade_ms, mm.tv_bull) AS trade_bull, b.mid AS mid_bull,
           s.price AS price_bear, s.mid IS NOT NULL AND NOT coalesce(s.book_ms < mm.tv_bear, false) AS book_bear,
           etfpair.trade_usable(s.price, s.trade_ms, mm.tv_bear) AS trade_bear, s.mid AS mid_bear
    FROM mm
    LEFT JOIN fxm fe ON fe.minute_ms = mm.j
    LEFT JOIN ends eb ON eb.symbol = %3$L AND eb.minute_ms = mm.j
    LEFT JOIN ends es ON es.symbol = %4$L AND es.minute_ms = mm.j
    LEFT JOIN LATERAL (SELECT eb.end_price AS price, eb.end_trade_ms AS trade_ms, eb.end_mid AS mid, eb.end_book_ms AS book_ms
                       WHERE eb.minute_ms IS NOT NULL
                       UNION ALL
                       SELECT r.price, r.trade_ms, CASE WHEN etfpair.book_usable(r.bid_px, r.bid_sz, r.ask_px, r.ask_sz, NULL, NULL)
                                                        THEN (r.bid_px[1] + r.ask_px[1]) / 2 END, r.book_ms
                       FROM etfpair.perp_second r WHERE eb.minute_ms IS NULL AND r.venue = %2$L AND r.symbol = %3$L AND r.second_ms = mm.j + 60000) b ON true
    LEFT JOIN LATERAL (SELECT es.end_price AS price, es.end_trade_ms AS trade_ms, es.end_mid AS mid, es.end_book_ms AS book_ms
                       WHERE es.minute_ms IS NOT NULL
                       UNION ALL
                       SELECT r.price, r.trade_ms, CASE WHEN etfpair.book_usable(r.bid_px, r.bid_sz, r.ask_px, r.ask_sz, NULL, NULL)
                                                        THEN (r.bid_px[1] + r.ask_px[1]) / 2 END, r.book_ms
                       FROM etfpair.perp_second r WHERE es.minute_ms IS NULL AND r.venue = %2$L AND r.symbol = %4$L AND r.second_ms = mm.j + 60000) s ON true
), bars AS (
    -- Minute j's bar: both legs' mid prices, else both trade prices, else both candle closes, else none.
    SELECT mi.session_date, mi.a_bull, mi.a_bear, mi.past_t, mi.tv_bull, mi.tv_bear, mi.j, mi.fx,
           CASE WHEN mi.book_bull AND mi.book_bear THEN mi.mid_bull WHEN mi.trade_bull AND mi.trade_bear THEN mi.price_bull
                ELSE (SELECT c.price FROM etfpair.perp_minute c WHERE c.venue = %2$L AND c.symbol = %3$L AND c.minute_ms = mi.j) END AS x_bull,
           CASE WHEN mi.book_bull AND mi.book_bear THEN mi.mid_bear WHEN mi.trade_bull AND mi.trade_bear THEN mi.price_bear
                ELSE (SELECT c.price FROM etfpair.perp_minute c WHERE c.venue = %2$L AND c.symbol = %4$L AND c.minute_ms = mi.j) END AS x_bear
    FROM minutes mi OFFSET 0
), flagged AS (
    -- A run restarts at a minute without a bar or F, after one, after a missing minute, and where the session, an anchor
    -- or the side of a T past T changes.
    SELECT l.*, NOT coalesce(l.has_bar AND l.prev_has_bar AND l.j - l.prev_j = 60000 AND l.session_date IS NOT DISTINCT FROM l.prev_session
                             AND l.a_bull IS NOT DISTINCT FROM l.prev_a_bull AND l.a_bear IS NOT DISTINCT FROM l.prev_a_bear
                             AND l.past_t = l.prev_past_t, false) AS restart
    FROM (SELECT b.*, b.x_bull IS NOT NULL AND b.x_bear IS NOT NULL AND b.fx IS NOT NULL AS has_bar,
                 lag(b.x_bull IS NOT NULL AND b.x_bear IS NOT NULL AND b.fx IS NOT NULL) OVER w AS prev_has_bar, lag(b.j) OVER w AS prev_j,
                 lag(b.session_date) OVER w AS prev_session, lag(b.a_bull) OVER w AS prev_a_bull, lag(b.a_bear) OVER w AS prev_a_bear,
                 lag(b.past_t) OVER w AS prev_past_t
          FROM bars b WINDOW w AS (ORDER BY b.j)) l
), means AS (
    -- Over the minutes before this one: where the minute before's run began, and the mean through the minute before.
    SELECT f.*, f.prev_j - max(CASE WHEN f.restart THEN f.j END) OVER (w ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING) AS prev_run_ms, %12$s
    FROM flagged f WINDOW w AS (ORDER BY f.j)
), mins AS MATERIALIZED (
    -- The minutes that hold a point, with the mean through the minute before when that minute is of the same session,
    -- anchors and side of T and ends a run of at least p_n bars.
    SELECT q.j, q.session_date, q.a_bull, q.a_bear, q.past_t, q.tv_bull, q.tv_bear, q.fx,
           CASE WHEN q.prev_j = q.j - 60000 AND q.prev_has_bar AND q.prev_run_ms >= (%6$L::bigint - 1) * 60000
                     AND q.session_date IS NOT DISTINCT FROM q.prev_session AND q.a_bull IS NOT DISTINCT FROM q.prev_a_bull
                     AND q.a_bear IS NOT DISTINCT FROM q.prev_a_bear AND q.past_t = q.prev_past_t
                THEN 10000 * (q.m_bull / q.a_bull + q.m_bear / q.a_bear - 2) END AS mu_bps,
           CASE WHEN q.prev_j = q.j - 60000 AND q.prev_has_bar AND q.prev_run_ms >= (%14$L::bigint - 1) * 60000
                     AND q.session_date IS NOT DISTINCT FROM q.prev_session AND q.a_bull IS NOT DISTINCT FROM q.prev_a_bull
                     AND q.a_bear IS NOT DISTINCT FROM q.prev_a_bear AND q.past_t = q.prev_past_t
                THEN q.sd END AS sigma_bps
    FROM means q
    WHERE greatest((q.j / %7$s::bigint + 1) * %7$s::bigint, %10$s::bigint) <= least(q.j + 60000, %11$s::bigint)
), secs AS (
    SELECT mi.session_date, mi.a_bull, mi.a_bear, mi.past_t, mi.fx, mi.mu_bps, mi.sigma_bps, t.ts,
           b.price AS price_bull, b.bid_px AS bid_px_bull, b.bid_sz AS bid_sz_bull, b.ask_px AS ask_px_bull, b.ask_sz AS ask_sz_bull,
           s.price AS price_bear, s.bid_px AS bid_px_bear, s.bid_sz AS bid_sz_bear, s.ask_px AS ask_px_bear, s.ask_sz AS ask_sz_bear,
           u.book_bull, u.book_bear, u.trade_bull, u.trade_bear
    FROM mins mi
    CROSS JOIN LATERAL generate_series(greatest((mi.j / %7$s::bigint + 1) * %7$s::bigint, %10$s::bigint), least(mi.j + 60000, %11$s::bigint), %7$s::bigint) t(ts)
    CROSS JOIN LATERAL (SELECT r.* FROM etfpair.perp_second r WHERE r.venue = %2$L AND r.symbol = %3$L AND r.second_ms = t.ts OFFSET 0) b
    CROSS JOIN LATERAL (SELECT r.* FROM etfpair.perp_second r WHERE r.venue = %2$L AND r.symbol = %4$L AND r.second_ms = t.ts OFFSET 0) s
    CROSS JOIN LATERAL (SELECT etfpair.book_usable(b.bid_px, b.bid_sz, b.ask_px, b.ask_sz, b.book_ms, mi.tv_bull) AS book_bull,
                               etfpair.book_usable(s.bid_px, s.bid_sz, s.ask_px, s.ask_sz, s.book_ms, mi.tv_bear) AS book_bear,
                               etfpair.trade_usable(b.price, b.trade_ms, mi.tv_bull) AS trade_bull,
                               etfpair.trade_usable(s.price, s.trade_ms, mi.tv_bear) AS trade_bear OFFSET 0) u
    WHERE u.book_bull AND u.book_bear OR u.trade_bull AND u.trade_bear
), points AS (
    SELECT c.ts, c.session_date, c.basis, c.past_t, c.fx, c.a_bull, c.a_bear, c.p_bull, c.p_bear,
           10000 * (c.fx * c.p_bull / c.a_bull + c.fx * c.p_bear / c.a_bear - 2) AS s_bps, c.mu_bps, c.sigma_bps,
           bp.s_long_bps, bp.s_short_bps, bp.partial_long, bp.partial_short, bp.filled_long, bp.filled_short, c.book_bull, c.book_bear,
           db.ct AS bid_ct_bull, ds.ct AS bid_ct_bear, ab.ct AS ask_ct_bull, as_.ct AS ask_ct_bear,
           db.usdt AS bid_usdt_bull, ds.usdt AS bid_usdt_bear, ab.usdt AS ask_usdt_bull, as_.usdt AS ask_usdt_bear,
           c.a_bull IS NOT NULL AND c.a_bear IS NOT NULL AND c.fx IS NOT NULL AS a_ok
    FROM (SELECT w.*, CASE WHEN w.book_bull AND w.book_bear THEN 'mid' ELSE 'trade' END AS basis,
                 CASE WHEN w.book_bull AND w.book_bear THEN (w.bid_px_bull[1] + w.ask_px_bull[1]) / 2 ELSE w.price_bull END AS p_bull,
                 CASE WHEN w.book_bull AND w.book_bear THEN (w.bid_px_bear[1] + w.ask_px_bear[1]) / 2 ELSE w.price_bear END AS p_bear
          FROM secs w OFFSET 0) c
    -- The real spreads on each leg's pricing levels only (a side with fewer has none: no pricing level).
    LEFT JOIN LATERAL (SELECT * FROM etfpair.book_pair(etfpair.first_levels(c.bid_px_bull, %15$L::integer), c.bid_sz_bull[1:%15$L::integer],
                                                       etfpair.first_levels(c.ask_px_bull, %15$L::integer), c.ask_sz_bull[1:%15$L::integer],
                                                       etfpair.first_levels(c.bid_px_bear, %16$L::integer), c.bid_sz_bear[1:%16$L::integer],
                                                       etfpair.first_levels(c.ask_px_bear, %16$L::integer), c.ask_sz_bear[1:%16$L::integer],
                                                       c.a_bull, c.a_bear, c.fx, %5$L::numeric)
                       WHERE c.basis = 'mid') bp ON true
    LEFT JOIN LATERAL etfpair.level_depth(CASE WHEN c.book_bull THEN c.bid_px_bull END, c.bid_sz_bull, %15$L::integer) db ON true
    LEFT JOIN LATERAL etfpair.level_depth(CASE WHEN c.book_bear THEN c.bid_px_bear END, c.bid_sz_bear, %16$L::integer) ds ON true
    LEFT JOIN LATERAL etfpair.level_depth(CASE WHEN c.book_bull THEN c.ask_px_bull END, c.ask_sz_bull, %15$L::integer) ab ON true
    LEFT JOIN LATERAL etfpair.level_depth(CASE WHEN c.book_bear THEN c.ask_px_bear END, c.ask_sz_bear, %16$L::integer) as_ ON true
)
SELECT p.ts AS ts_ms, p.session_date, p.basis, p.past_t, p.fx, p.a_bull, p.a_bear, p.p_bull, p.p_bear, p.s_bps, p.mu_bps, p.sigma_bps,
       p.s_long_bps, p.s_short_bps, p.partial_long, p.partial_short, p.filled_long, p.filled_short, p.book_bull, p.book_bear,
       p.bid_ct_bull, p.bid_ct_bear, p.ask_ct_bull, p.ask_ct_bear, p.bid_usdt_bull, p.bid_usdt_bear, p.ask_usdt_bull, p.ask_usdt_bear, p.a_ok,
       etfpair.combo_signal(p.s_bps, p.past_t, p.mu_bps, %5$L::numeric, p.basis = 'mid', p.s_long_bps, p.s_short_bps, p.partial_long,
                            p.partial_short, %13$L::numeric) AS signal
FROM (SELECT * FROM points OFFSET 0) p
$q$, p_stretches, p_venue, p_bull, p_bear, p_notional, p_n, p_step_ms, p_bar_ms, (p_last_ms - 1) / 60000 * 60000, p_first_ms, p_last_ms,
       v_mean || ', ' || v_sigma, p_entry, p_sigma_n, p_levels_bull, p_levels_bear);
END
$$;


-- combo_range (3.4): what the Spread row draws, as the 3.2 version, with sigma_bps and the depths of the pricing levels
-- in place of the 1+2 depths (contracts and USDT); p_sigma_n not positive: no sigma.
CREATE OR REPLACE FUNCTION etfpair.combo_range(p_combo text, p_venue text, p_ma text, p_n integer, p_sigma_n integer, p_from_ms bigint,
                                               p_to_ms bigint, p_step_ms bigint, p_notional numeric, p_entry numeric)
RETURNS TABLE (ts_ms bigint, session_date date, phase text, phase_sort integer, basis text, past_t boolean, s_bps numeric, mu_bps numeric,
               sigma_bps numeric, s_long_bps numeric, s_short_bps numeric, partial_long boolean, partial_short boolean, book_bull boolean,
               book_bear boolean, bid_ct_bull numeric, bid_ct_bear numeric, ask_ct_bull numeric, ask_ct_bear numeric, bid_usdt_bull numeric,
               bid_usdt_bear numeric, ask_usdt_bull numeric, ask_usdt_bear numeric, a_ok boolean, signal text)
LANGUAGE plpgsql STABLE AS $$
DECLARE
    v_bull text;
    v_bear text;
    v_n integer := CASE WHEN p_n > 0 THEN p_n END;   -- the mean's minutes, NULL for no mean
    v_y integer := CASE WHEN p_sigma_n > 0 THEN p_sigma_n END;   -- sigma's minutes, NULL for no sigma
    v_lb integer;            -- the legs' pricing levels
    v_le integer;
    v_first bigint;          -- the first and the last point
    v_last bigint;
    v_bar bigint;            -- the first minute whose bar the means need
BEGIN
    IF NOT coalesce(p_step_ms > 0, false) THEN
        RETURN;
    END IF;
    v_first := (p_from_ms / p_step_ms + 1) * p_step_ms;
    v_last := p_to_ms / p_step_ms * p_step_ms;
    SELECT m.bull_symbol, m.bear_symbol, etfpair.pricing_levels(m.bull_tick), etfpair.pricing_levels(m.bear_tick) INTO v_bull, v_bear, v_lb, v_le
    FROM etfpair.combo_market m WHERE m.combo_id = p_combo AND m.venue = p_venue;
    IF v_bull IS NULL OR NOT coalesce(v_first <= v_last, false) THEN
        RETURN;
    END IF;
    -- The EMA from the start of the session of the first point's minute; the SMA and sigma from p_n, p_sigma_n minutes
    -- before it, the more of the two (not before that session's start: a run never crosses it); from that minute itself
    -- before the first close (no session, no mean) and without a mean and a sigma.
    SELECT coalesce(max(s.close_ms), (v_first - 1) / 60000 * 60000) INTO v_bar FROM etfpair.etf_session s WHERE s.close_ms <= (v_first - 1) / 60000 * 60000;
    IF v_n IS NOT NULL AND p_ma = 'ema' THEN
        NULL;
    ELSIF coalesce(v_n, v_y) IS NULL THEN
        v_bar := (v_first - 1) / 60000 * 60000;
    ELSE
        v_bar := greatest(v_bar, (v_first - 1) / 60000 * 60000 - greatest(v_n, v_y) * 60000::bigint);
    END IF;
    RETURN QUERY EXECUTE format($q$
WITH p AS MATERIALIZED (%1$s), marked AS MATERIALIZED (
    SELECT p.*, lag(p.ts_ms) OVER w AS prev_ts, lag(p.session_date) OVER w AS prev_session
    FROM p WINDOW w AS (ORDER BY p.ts_ms)
)
SELECT m.ts_ms, m.session_date, ph.phase, array_position(ARRAY['Pre-Market', 'Regular', 'After-Hours', 'Overnight', 'Closed'], ph.phase),
       m.basis, m.past_t, m.s_bps, m.mu_bps, m.sigma_bps, m.s_long_bps, m.s_short_bps, m.partial_long, m.partial_short, m.book_bull,
       m.book_bear, m.bid_ct_bull, m.bid_ct_bear, m.ask_ct_bull, m.ask_ct_bear, m.bid_usdt_bull, m.bid_usdt_bear, m.ask_usdt_bull,
       m.ask_usdt_bear, m.a_ok, m.signal
FROM marked m
LEFT JOIN LATERAL (SELECT e.phase FROM etfpair.etf_phase e WHERE e.start_ms < m.ts_ms AND e.end_ms >= m.ts_ms
                   AND m.s_bps IS NOT NULL AND m.mu_bps IS NOT NULL ORDER BY e.start_ms DESC LIMIT 1) ph ON true
UNION ALL
SELECT m.prev_ts + 1, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL,
       NULL, NULL, NULL, NULL
FROM marked m
WHERE m.prev_ts IS NOT NULL AND (m.ts_ms - m.prev_ts > %2$s::bigint OR m.session_date IS DISTINCT FROM m.prev_session)
ORDER BY 1
$q$, etfpair.range_points_sql(p_venue, v_bull, v_bear, p_ma, v_n, v_y, v_first, v_last, p_step_ms, p_notional, p_entry, v_bar,
                              etfpair.range_stretches(p_combo, p_venue, v_bar, (v_last - 1) / 60000 * 60000 + 60000), v_lb, v_le),
       p_step_ms);
END
$$;


-- combo_last (3.4): the snapshot strips' two newest seconds, as the 3.2 version, with sigma_bps and the depths of the
-- pricing levels; p_sigma_n not positive: no sigma.
CREATE OR REPLACE FUNCTION etfpair.combo_last(p_combo text, p_venue text, p_ma text, p_n integer, p_sigma_n integer, p_notional numeric,
                                              p_entry numeric, p_now_ms bigint)
RETURNS TABLE (kind text, ts_ms bigint, session_date date, basis text, past_t boolean, fx numeric,
               a_bull numeric, a_bear numeric, a_ref numeric, p_bull numeric, p_bear numeric, p_ref numeric,
               s_bps numeric, mu_bps numeric, sigma_bps numeric, s_long_bps numeric, s_short_bps numeric, partial_long boolean,
               partial_short boolean, filled_long numeric, filled_short numeric, book_bull boolean, book_bear boolean,
               bid_ct_bull numeric, bid_ct_bear numeric, ask_ct_bull numeric, ask_ct_bear numeric,
               bid_usdt_bull numeric, bid_usdt_bear numeric, ask_usdt_bull numeric, ask_usdt_bear numeric,
               anchor_status text, signal text)
LANGUAGE plpgsql STABLE AS $$
DECLARE
    v_bull text;
    v_bear text;
    v_ref text;
    v_n integer := CASE WHEN p_n > 0 THEN p_n END;                 -- the mean's minutes, NULL for no mean
    v_y integer := CASE WHEN p_sigma_n > 0 THEN p_sigma_n END;     -- sigma's minutes, NULL for no sigma
    v_lb integer;                                                  -- the legs' pricing levels
    v_le integer;
    v_kmin bigint := (p_now_ms - 600000) / 60000 * 60000;          -- the minutes of the ten
    v_kmax bigint := (p_now_ms - 1) / 60000 * 60000;
    v_bar bigint;
    v_seg text;
    v_basis bigint;
    v_prices bigint;
BEGIN
    SELECT m.bull_symbol, m.bear_symbol, m.ref_symbol, etfpair.pricing_levels(m.bull_tick), etfpair.pricing_levels(m.bear_tick)
    INTO v_bull, v_bear, v_ref, v_lb, v_le FROM etfpair.combo_market m
    WHERE m.combo_id = p_combo AND m.venue = p_venue;
    IF v_bull IS NULL OR p_now_ms IS NULL THEN
        RETURN;
    END IF;
    -- The stretches from the first bar any of the ten minutes needs (as combo_range) to the last.
    SELECT coalesce(max(s.close_ms), v_kmin) INTO v_bar FROM etfpair.etf_session s WHERE s.close_ms <= v_kmin;
    IF v_n IS NOT NULL AND p_ma = 'ema' THEN
        NULL;
    ELSIF coalesce(v_n, v_y) IS NULL THEN
        v_bar := v_kmin;
    ELSE
        v_bar := greatest(v_bar, v_kmin - greatest(v_n, v_y) * 60000::bigint);
    END IF;
    v_seg := etfpair.range_stretches(p_combo, p_venue, v_bar, v_kmax + 60000);
    EXECUTE format($q$
WITH seg AS MATERIALIZED (SELECT * FROM %1$s WHERE NOT (gap_bull OR gap_bear))
SELECT (SELECT b.second_ms FROM etfpair.perp_second b
        CROSS JOIN LATERAL (SELECT g.tv_bull, g.tv_bear FROM seg g WHERE g.from_ms <= (b.second_ms - 1) / 60000 * 60000
                            AND g.to_ms > (b.second_ms - 1) / 60000 * 60000 LIMIT 1) g
        CROSS JOIN LATERAL (SELECT x.* FROM etfpair.perp_second x WHERE x.venue = %2$L AND x.symbol = %4$L AND x.second_ms = b.second_ms OFFSET 0) s
        WHERE b.venue = %2$L AND b.symbol = %3$L AND b.second_ms > %6$s::bigint - 600000 AND b.second_ms <= %6$s::bigint
          AND (etfpair.book_usable(b.bid_px, b.bid_sz, b.ask_px, b.ask_sz, b.book_ms, g.tv_bull)
               AND etfpair.book_usable(s.bid_px, s.bid_sz, s.ask_px, s.ask_sz, s.book_ms, g.tv_bear)
               OR etfpair.trade_usable(b.price, b.trade_ms, g.tv_bull) AND etfpair.trade_usable(s.price, s.trade_ms, g.tv_bear))
        ORDER BY b.second_ms DESC LIMIT 1),
       (SELECT b.second_ms FROM etfpair.perp_second b
        CROSS JOIN LATERAL (SELECT g.tv_bull, g.tv_bear, g.tv_ref, g.gap_ref FROM seg g WHERE g.from_ms <= (b.second_ms - 1) / 60000 * 60000
                            AND g.to_ms > (b.second_ms - 1) / 60000 * 60000 LIMIT 1) g
        CROSS JOIN LATERAL (SELECT x.price, x.trade_ms FROM etfpair.perp_second x WHERE x.venue = %2$L AND x.symbol = %4$L AND x.second_ms = b.second_ms OFFSET 0) s
        CROSS JOIN LATERAL (SELECT x.price, x.trade_ms FROM etfpair.perp_second x WHERE x.venue = %2$L AND x.symbol = %5$L AND x.second_ms = b.second_ms OFFSET 0) r
        WHERE b.venue = %2$L AND b.symbol = %3$L AND b.second_ms > %6$s::bigint - 600000 AND b.second_ms <= %6$s::bigint
          AND etfpair.trade_usable(b.price, b.trade_ms, g.tv_bull) AND etfpair.trade_usable(s.price, s.trade_ms, g.tv_bear)
          AND NOT g.gap_ref AND etfpair.trade_usable(r.price, r.trade_ms, g.tv_ref)
        ORDER BY b.second_ms DESC LIMIT 1)
$q$, v_seg, p_venue, v_bull, v_bear, v_ref, p_now_ms) INTO v_basis, v_prices;
    IF v_basis IS NOT NULL THEN
        RETURN QUERY EXECUTE format($q$
SELECT 'basis'::text, p.ts_ms, p.session_date, p.basis, p.past_t, p.fx, p.a_bull, p.a_bear, g.a_ref, p.p_bull, p.p_bear,
       CASE WHEN NOT g.gap_ref AND etfpair.trade_usable(r.price, r.trade_ms, g.tv_ref) THEN r.price END,
       p.s_bps, p.mu_bps, p.sigma_bps, p.s_long_bps, p.s_short_bps, p.partial_long, p.partial_short, p.filled_long, p.filled_short,
       p.book_bull, p.book_bear, p.bid_ct_bull, p.bid_ct_bear, p.ask_ct_bull, p.ask_ct_bear, p.bid_usdt_bull, p.bid_usdt_bear, p.ask_usdt_bull,
       p.ask_usdt_bear,
       (SELECT cs.status FROM etfpair.combo_sessions(%3$L) cs WHERE cs.session_date = p.session_date), p.signal
FROM (%1$s) p
CROSS JOIN LATERAL (SELECT g.a_ref, g.gap_ref, g.tv_ref FROM (SELECT * FROM %2$s) g
                    WHERE g.from_ms <= (p.ts_ms - 1) / 60000 * 60000 AND g.to_ms > (p.ts_ms - 1) / 60000 * 60000) g
LEFT JOIN LATERAL (SELECT x.price, x.trade_ms FROM etfpair.perp_second x WHERE x.venue = %4$L AND x.symbol = %5$L AND x.second_ms = p.ts_ms OFFSET 0) r ON true
$q$, etfpair.range_points_sql(p_venue, v_bull, v_bear, p_ma, v_n, v_y, v_basis, v_basis, 1000, p_notional, p_entry, v_bar, v_seg, v_lb, v_le),
             v_seg, p_combo, p_venue, v_ref);
    END IF;
    IF v_prices IS NOT NULL THEN
        RETURN QUERY EXECUTE format($q$
SELECT 'prices'::text, %5$s::bigint, g.session_date, NULL::text, NULL::boolean, f.price, g.a_bull, g.a_bear, g.a_ref, b.price, s.price, r.price,
       NULL::numeric, NULL::numeric, NULL::numeric, NULL::numeric, NULL::numeric, NULL::boolean, NULL::boolean, NULL::numeric, NULL::numeric,
       NULL::boolean, NULL::boolean, NULL::numeric, NULL::numeric, NULL::numeric, NULL::numeric, NULL::numeric, NULL::numeric, NULL::numeric,
       NULL::numeric, NULL::text, NULL::text
FROM (SELECT * FROM %1$s) g
JOIN etfpair.perp_second b ON b.venue = %2$L AND b.symbol = %3$L AND b.second_ms = %5$s::bigint
JOIN etfpair.perp_second s ON s.venue = %2$L AND s.symbol = %4$L AND s.second_ms = %5$s::bigint
JOIN etfpair.perp_second r ON r.venue = %2$L AND r.symbol = %6$L AND r.second_ms = %5$s::bigint
LEFT JOIN LATERAL (SELECT q.price FROM etfpair.fx_minute q WHERE q.pair = 'USDT-USD' AND q.minute_ms <= (%5$s::bigint - 1) / 60000 * 60000
                   AND q.minute_ms >= (%5$s::bigint - 1) / 60000 * 60000 - 300000 ORDER BY q.minute_ms DESC LIMIT 1) f ON true
WHERE g.from_ms <= (%5$s::bigint - 1) / 60000 * 60000 AND g.to_ms > (%5$s::bigint - 1) / 60000 * 60000
$q$, v_seg, p_venue, v_bull, v_bear, v_prices, v_ref);
    END IF;
END
$$;
