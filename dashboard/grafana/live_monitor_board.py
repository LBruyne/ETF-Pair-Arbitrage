"""ETF-Pair Live Monitor: the trading program (core/) as it trades, from its own records in schema trade of the etfpair
database (core/store/schema.sql, core/SPEC.md 5.6), read by the read-only role etfpair_trade_reader through the data source
etfpair-trade (dashboard/SPEC.md section 7).

Version 3 (owner item 71) picks, in this order, a Run (one start of the program, numbered and named like the sibling
dex-cex board's Runs: Run 07 · Live · 11-26 11:36 – now · running), Since and its hour, Until and its hour (the Range: the
figures cover the Run's records from Since to Until) and the Combo (the Run's pair). A Run is one mode, paper or live, so
paper and live are never merged: every query keeps the picked Run's rows (SEL: run_id and live of the Run) and a slim banner
at the top says which mode and Run the page shows. It shows the Run's effective settings (process_run.config) and its
latest second (the real-time figures) near the top.

Version 2 (owner item 68) laid the page out to a trader's questions, in this order: is it running and healthy (Health),
what do I hold (Position), how am I doing (Net, P&L), what is the market now (Legs, Spread), can it enter now and why not
(Entry); then the Live Execution chart of the deviation in sigma with the program's lines and a marker at every entry, exit
and adjustment (owner item 61), the spread behind it in D, the Run's settings, the round trips (one row each, a click opens
its orders), risk and account in one strip, and the diagnostics folded at the bottom. Every time-series chart marks each
change of the anchors in use (owner item 62).

The spread unit is D (1 D = 0.5 bps of the two legs' total notional), never bps; money is USDT. The program's lines are those
of the Run (process_run.config: strategy.d_in, strategy.d_out): a Long enters at x <= -d_in sigma on the buying side (Buy
Both) and exits at x >= -d_out sigma on the selling side (Sell Both); a Short enters at x >= d_in sigma on Sell Both and
exits at x <= d_out sigma on Buy Both (core/SPEC.md 5.2). Every picked setting reaches SQL inside quotes ('$run', '$combo',
'${since}', '$cycle', ...) and is parsed there; the reader role is the boundary (dashboard/SPEC.md section 6).

Grafana's tooltip shows a nearby point's value in place of a NULL, so on the execution charts every tooltip row has a
value at every point: a reason code where there is none (NO_VALUE), and a 0 on the rows without a decision for the marker
rows, which the tooltip hides (hideZeros); a real zero there is a code (ZERO) shown as 0."""
from boardkit import (BAD, BLUE, GOOD, GREY, LIGHT_GREY, NO_FIGURE, ORANGE, PURPLE, SIGNED, TEXT, TRADE, WARN, bands, board_slug, board_uid,
                      code_texts, col, keep_fields, marks, named, organize, panel, query_var, reset_ids, row, series, sql, stat_strip,
                      table, timeseries, tooltip_only)

NAME = "live-monitor"   # boardkit.BOARDS: uid arb-etf-pair-live-monitor, address /dashboards/arbitrage/etf-pair/live-monitor
URL = "/d/%s/%s" % (board_uid(NAME), board_slug(NAME))
NOW_MS = "(extract(epoch FROM now()) * 1000)::bigint"
NBSP = " "
NBHY = "‑"   # a non-breaking hyphen: a browser may break a line after a plain one
UTC8 = "'Etc/GMT-8'"
# The price precision of the two contracts (Bitget SOXLUSDT 0.01, SOXSUSDT 0.001): prices are shown to their tick.
TICK_DECIMALS = {"bull": 2, "bear": 3}
SAMPLE_AGE_S = 10   # a running process writes a sample every second; past this it is Stale


def clock(ms, fmt="MM-DD HH24:MI:SS"):
    """A Unix millisecond time as UTC+8 text; no-break spaces and hyphens, so a narrow figure keeps it on one line."""
    return "to_char(to_timestamp((%s) / 1000.0) AT TIME ZONE %s, '%s')" % (ms, UTC8, fmt.replace(" ", NBSP).replace("-", NBHY))


def span(ms, seconds=True):
    """A duration in milliseconds as short text: 45s, 12m 05s (12m without seconds), 3h 07m, 2d 04h (no-break spaces)."""
    x = "(%s)::bigint" % ms
    minutes = "({x} / 60000) || 'm{s}' || lpad((({x} / 1000) % 60)::text, 2, '0') || 's'" if seconds else "({x} / 60000) || 'm'"
    return ("CASE WHEN {x} IS NULL THEN NULL WHEN {x} < 60000 THEN ({x} / 1000) || 's' "
            "WHEN {x} < 3600000 THEN " + minutes + " "
            "WHEN {x} < 86400000 THEN ({x} / 3600000) || 'h{s}' || lpad((({x} / 60000) % 60)::text, 2, '0') || 'm' "
            "ELSE ({x} / 86400000) || 'd{s}' || lpad((({x} / 3600000) % 24)::text, 2, '0') || 'h' END").format(x=x, s=NBSP)


def shown(x, decimals):
    """x rounded as Grafana rounds it for display (Math.round of x * 10^d, halves up), once in SQL (dashboard/SPEC.md
    section 6): the frame is smaller and reads exactly as unrounded."""
    return "(floor((%s)::float8 * 1e%d::float8 + 0.5) / 1e%d::float8)" % (x, decimals, decimals)


# ---------------------------------------------------------------------------------------------------- the program's words
# The program's reason codes in plain words: the idle reason of a second (core/strategy/types.go), as what the entry logic
# is (State) and why (the words). Ready and Order Sent are green; Blocked is the strategy's ordinary answer (text); Held
# Back (amber) waits on data, the venue or a stop; Needs a Person (red). A code not listed reads as its own words, Blocked.
IDLE = [("no_signal", "Ready", "No Signal Yet"),
        ("regular_hours", "Blocked", "Regular Hours"), ("spacing", "Blocked", "Spacing After Entry"),
        ("holding_other_side", "Blocked", "Holding Other Side"), ("cost_not_covered", "Blocked", "Fees Not Covered"),
        ("quota_exhausted", "Blocked", "Quota Used Up"), ("too_small", "Blocked", "Below Minimum Size"),
        ("warmup", "Blocked", "Mean/σ Warming Up"), ("zero_sigma", "Blocked", "σ Is Zero"), ("positions_read", "Blocked", "Reading Positions"),
        ("await_orders", "Blocked", "Orders in Flight"), ("await_depth", "Blocked", "Awaiting Book"),
        ("await_close", "Blocked", "Awaiting Flat Confirm"), ("action_window", "Blocked", "Dividend/Split Window"),
        ("capital_pending", "Blocked", "Capital Change Pending"),
        ("no_levels", "Held Back", "Book Too Thin"), ("depth_bull", "Held Back", "Bull Book Unusable"),
        ("depth_bear", "Held Back", "Bear Book Unusable"), ("conflict_bull", "Held Back", "Bull Book Conflict"),
        ("conflict_bear", "Held Back", "Bear Book Conflict"), ("fx_stale", "Held Back", "USD/USDT Stale"),
        ("rejected_backoff", "Held Back", "Rejected, Backing Off"), ("no_anchor", "Held Back", "No Anchor Yet"),
        ("depth", "Held Back", "Exit Book Unusable"), ("no_price", "Held Back", "No Price to Reduce"),
        ("order_unknown", "Held Back", "Order Outcome Unknown"), ("not_ready", "Held Back", "Starting Up"),
        ("stopping", "Held Back", "Stopping"), ("no_new_entries", "Held Back", "Stop Pending"),
        ("settings", "Needs a Person", "Account Settings"), ("store_error", "Needs a Person", "Records Not Written"),
        ("position_mismatch", "Needs a Person", "Position Mismatch")]
STATE_COLORS = {"Ready": GOOD, "Order Sent": GOOD, "Blocked": TEXT, "Held Back": WARN, "Needs a Person": BAD}
# Why a cycle ended (core/inventory/types.go); a code not listed reads as its own words.
END_REASONS = {"exit_line": "Exit Line", "close_window": "Flattened Before Close", "stop": "Program Stopped", "session_changed": "Session Ended",
               "no_fill": "No Fill", "one_leg": "One Leg Only", "dust_only": "Trimmed to Dust", "store_error": "Not Recorded"}


def plain(code, table_):
    """SQL: a code in plain words from table_ ({code: words}); any other code as its own words (dust_only -> Dust Only)."""
    whens = " ".join("WHEN '%s' THEN '%s'" % (k, v) for k, v in table_.items())
    return "CASE %s %s ELSE initcap(replace(%s, '_', ' ')) END" % (code, whens, code)


def idle_state(code):
    return "CASE WHEN %s IS NULL THEN 'Order Sent' %s ELSE 'Blocked' END" % (code, " ".join(
        "WHEN %s = '%s' THEN '%s'" % (code, c, s) for c, s, _ in IDLE))


def idle_why(code):
    """The words of an idle reason; the legs' tickers in place of Bull, Bear."""
    words = {c: w.replace("Bull ", "$bull ").replace("Bear ", "$bear ") for c, _, w in IDLE}
    return "CASE WHEN %s IS NULL THEN 'This Second' ELSE %s END" % (code, plain(code, words))


def end_reason(code):
    return "coalesce(%s, 'Open')" % plain(code, END_REASONS)


# Reason codes where a point has no value (code_texts maps them to text).
NO_VALUE = {-2000000000: "No Anchor", -3000000000: "No Book", -4000000000: "Warmup", -5000000000: "No Levels",
            -6000000000: "Book Not OK"}
CODE = {text: code for code, text in NO_VALUE.items()}
# A cycle's funding or net while a settlement it held through has no record yet (funding_known false): Pending, not 0.
PENDING = -9000000000
# A fee not reported yet for an order with fills (cum_exec_qty > 0 AND NOT fee_known); an order without a fill needs none.
FEE_PENDING = -9100000000
OPEN = -9200000000       # an open cycle's net
NO_FILL = -9300000000    # a marker's average price without a fill
ZERO = -9400000000       # a real zero in a tooltip row of the execution charts (a 0 is hidden there)
NO_SAMPLE = -9500000000  # a completion's, trim's or forced exit's place: no sample with sigma in the minute before it


def nonzero(x, decimals):
    """A tooltip row's value as shown(), a real zero as ZERO (the tooltip hides a 0)."""
    return "CASE WHEN %s = 0 THEN %d ELSE %s END" % (shown(x, decimals), ZERO, shown(x, decimals))


def coded(value, decimals, why):
    """value shown() (a zero as ZERO), else the code of the first reason in why ((condition, text), ...) that holds, the last
    one standing for any other."""
    whens = ["WHEN %s IS NOT NULL THEN %s" % (value, nonzero(value, decimals))] + ["WHEN %s THEN %d" % (c, CODE[t]) for c, t in why[:-1]]
    return "CASE %s ELSE %d END" % (" ".join(whens), CODE[why[-1][1]])


# ------------------------------------------------------------------------------------------------- the Run and the Range
# The Run (owner item 71, numbered and named like the sibling dex-cex board's Runs): one start of the program, one row of
# process_run with its mode (live), its effective settings (config) and its pair. Picked as Newest Live Run (__live, the
# default: the live run started last, whatever runs beside it), Newest Paper Run (__paper) or one Run by its run_id; both
# newest are worked out by every query, so a new start is followed without reloading the page. Run nn: the Runs of both modes
# numbered by start, from 01.
RUN_ID = ("(SELECT CASE '$run' WHEN '__live' THEN (SELECT x.run_id FROM trade.process_run x WHERE x.live ORDER BY x.started_ms DESC LIMIT 1) "
          "WHEN '__paper' THEN (SELECT x.run_id FROM trade.process_run x WHERE NOT x.live ORDER BY x.started_ms DESC LIMIT 1) ELSE '$run' END)")
RUN_NO = "(SELECT count(*) FROM trade.process_run n WHERE (n.started_ms, n.run_id) <= ({r}.started_ms, {r}.run_id))"
RUN_NUMBER = "'Run ' || lpad(%s::text, 2, '0')" % RUN_NO
RUN_NAME = RUN_NUMBER + " || ' · ' || CASE WHEN {r}.live THEN 'Live' ELSE 'Paper' END"
# The Combo of a Run: its pair's tickers (cex.pair), Bitget's perpetuals; a Run trades one. The pick reaches SEL: a Combo the
# Run does not trade leaves the page empty.
PAIR = ("coalesce({r}.config #>> '{{cex,pair,bull,ticker}}', 'SOXL') || ' / ' || coalesce({r}.config #>> '{{cex,pair,bear,ticker}}', 'SOXS')")
# Since and Until (with their hours, owner item 71 as the sibling board's): Since is Run start (0) or a UTC+8 date the Run ran
# on (its midnight in ms) at an hour 00-23; Until is Run end (NO_END_MS) or a date at an hour 01-24, that moment excluded.
# Parsed from quoted text (a typed value that is not a number counts as Run start / Run end).
NO_END_MS = "99999999999999"
SINCE_MS = ("(CASE WHEN '${since}' ~ '^[0-9]{1,14}$' AND '${since}' <> '0' THEN '${since}'::bigint + "
            "CASE WHEN '${since_hour}' ~ '^[0-9]{1,2}$' THEN '${since_hour}'::int ELSE 0 END * 3600000 ELSE 0 END)")
UNTIL_MS = ("(CASE WHEN '${until}' ~ '^[0-9]{1,14}$' AND '${until}'::bigint < %s THEN '${until}'::bigint + "
            "CASE WHEN '${until_hour}' ~ '^[0-9]{1,2}$' THEN '${until_hour}'::int ELSE 24 END * 3600000 ELSE %s END)" % (NO_END_MS, NO_END_MS))
# Every query starts from these: sel, the picked Run (no row: no Run, or a Combo it does not trade), its end (its stop, now
# while it has none) and rg, the Range: from the later of its start and Since to the earlier of its end and Until, excluded.
SEL = sql("""sel AS MATERIALIZED (SELECT r.run_id, r.live, r.started_ms, r.stopped_ms, r.host, r.config, coalesce(r.stopped_ms, @NOW@) AS end_ms
                         FROM trade.process_run r WHERE r.run_id = @RUN_ID@ AND @PAIR@ = '$combo'),
rg AS MATERIALIZED (SELECT greatest(sel.started_ms, @SINCE@) AS from_ms, least(sel.end_ms + 1, @UNTIL@) AS to_ms FROM sel)""",
          NOW=NOW_MS, RUN_ID=RUN_ID, PAIR=PAIR.format(r="r"), SINCE=SINCE_MS, UNTIL=UNTIL_MS)
RID, RLIVE = "(SELECT sel.run_id FROM sel)", "(SELECT sel.live FROM sel)"
FROM_MS, TO_MS = "(SELECT rg.from_ms FROM rg)", "(SELECT rg.to_ms FROM rg)"


def of_run(alias):
    """The rows of a table of the Run's records (every table but process_run, funding_rate and anchor_fact has run_id)."""
    return "%s.run_id = %s AND %s.live = %s" % (alias, RID, alias, RLIVE)


def cycle_of_run(alias):
    """The cycles of the Run: written last by it, or one it decided on (a new start takes over an open cycle and writes it
    from then on, so its run_id is the newer Run's; the decisions keep each Run's part)."""
    return ("{a}.live = {live} AND ({a}.run_id = {rid} OR {a}.cycle_id IN (SELECT dr.cycle_id FROM trade.decision dr "
            "WHERE dr.run_id = {rid} AND dr.live = {live} AND dr.cycle_id IS NOT NULL))").format(a=alias, rid=RID, live=RLIVE)


def of_mode(alias):
    """The rows of the Run's mode: the market facts it read (funding rates; anchors, which a later start may reuse), and the
    orders of its decisions and cycles (an order is written last by the Run that saw its end)."""
    return "%s.live = %s" % (alias, RLIVE)


def in_range(ms):
    return "%s >= %s AND %s < %s" % (ms, FROM_MS, ms, TO_MS)


# The charts: drawn on the time picker's axis (so a drag zooms them), with the Run's seconds inside the Range only.
CHART_FROM, CHART_TO = "greatest($__from, %s)" % FROM_MS, "least($__to, %s - 1)" % TO_MS


# The Run picker: Newest Live Run, Newest Paper Run (each when one exists), then every Run, newest first, as
# Run nn · Mode · MM-DD HH:MI – HH:MI (the date again when it ends on another day; now while it runs) · running / stopped /
# starting (its first minute) / lost (no stop recorded and no sample in the last minute: crashed or killed); the two Newest
# name the Run they stand for now.
RUN_STATE = ("CASE WHEN r.stopped_ms IS NOT NULL THEN 'stopped' WHEN EXISTS (SELECT 1 FROM trade.signal_sample s WHERE s.ts_ms > %s - 60000 "
             "AND s.run_id = r.run_id AND s.live = r.live) THEN 'running' WHEN %s - r.started_ms < 60000 THEN 'starting' ELSE 'lost' END" % (NOW_MS, NOW_MS))
Q_RUN_OPTIONS = sql("""
SELECT x.__text, x.__value FROM (
  SELECT 'Newest Live Run · ' || @NUMBER@ AS __text, '__live' AS __value, 2 AS g, 0::bigint AS o
  FROM (SELECT * FROM trade.process_run x WHERE x.live ORDER BY x.started_ms DESC LIMIT 1) r
  UNION ALL
  SELECT 'Newest Paper Run · ' || @NUMBER@, '__paper', 1, 0 FROM (SELECT * FROM trade.process_run x WHERE NOT x.live ORDER BY x.started_ms DESC LIMIT 1) r
  UNION ALL
  SELECT @NAME@ || ' · ' || @START@ || ' – ' || CASE WHEN q.state <> 'stopped' THEN 'now'
              WHEN @DAY@ = @STOP_DAY@ THEN @STOP_HM@ ELSE @STOP@ END || ' · ' || q.state, r.run_id, 0, r.started_ms
  FROM trade.process_run r CROSS JOIN LATERAL (SELECT @STATE@ AS state) q
  UNION ALL SELECT 'No Run Yet', '__none', 0, 0 WHERE NOT EXISTS (SELECT 1 FROM trade.process_run x)) x
ORDER BY x.g DESC, x.o DESC""", NAME=RUN_NAME.format(r="r"), NUMBER=RUN_NUMBER.format(r="r"), START=clock("r.started_ms", "MM-DD HH24:MI"), STATE=RUN_STATE,
                    DAY=clock("r.started_ms", "MM-DD"), STOP_DAY=clock("r.stopped_ms", "MM-DD"), STOP_HM=clock("r.stopped_ms", "HH24:MI"),
                    STOP=clock("r.stopped_ms", "MM-DD HH24:MI"))
# The UTC+8 dates the picked Run ran on (start to end, today while it runs), newest first, after Run start / Run end.
RUN_DAYS = sql("""
SELECT g::bigint AS day_ms FROM trade.process_run r,
  generate_series((floor((r.started_ms + 28800000) / 86400000.0) * 86400000 - 28800000)::bigint,
                  (floor((coalesce(r.stopped_ms, @NOW@) + 28800000) / 86400000.0) * 86400000 - 28800000)::bigint, 86400000) g
WHERE r.run_id = @RUN_ID@""", NOW=NOW_MS, RUN_ID=RUN_ID)
DAY_TEXT = clock("d.day_ms", "YYYY-MM-DD")
Q_SINCE = sql("""SELECT x.__value, x.__text FROM (SELECT '0' AS __value, 'Run start' AS __text, 9e15 AS o
  UNION ALL SELECT d.day_ms::text, @DAY@, d.day_ms FROM (@DAYS@) d) x ORDER BY x.o DESC""", DAY=DAY_TEXT, DAYS=RUN_DAYS)
Q_UNTIL = sql("""SELECT x.__value, x.__text FROM (SELECT '@NO_END@' AS __value, 'Run end' AS __text, 9e15 AS o
  UNION ALL SELECT d.day_ms::text, @DAY@, d.day_ms FROM (@DAYS@) d) x ORDER BY x.o DESC""", DAY=DAY_TEXT, DAYS=RUN_DAYS, NO_END=NO_END_MS)
# The hours: Since 00:00-23:00, Until 01:00-24:00 (24: through the date); a dash while the date is Run start / Run end.
Q_SINCE_HOUR = """SELECT x.__value, x.__text FROM (SELECT '00' AS __value, '—' AS __text, -1 AS o WHERE '${since}' = '0'
  UNION ALL SELECT lpad(h::text, 2, '0'), lpad(h::text, 2, '0') || ':00', h FROM generate_series(0, 23) h WHERE '${since}' <> '0') x ORDER BY x.o"""
Q_UNTIL_HOUR = """SELECT x.__value, x.__text FROM (SELECT '24' AS __value, '—' AS __text, -1 AS o WHERE '${until}' = '%s'
  UNION ALL SELECT lpad(h::text, 2, '0'), lpad(h::text, 2, '0') || ':00', h FROM generate_series(1, 24) h WHERE '${until}' <> '%s') x ORDER BY x.o""" % (
    NO_END_MS, NO_END_MS)
# The Combo picker: the pair of the picked Run.
Q_COMBO = sql("""
SELECT @PAIR@ AS __text, @PAIR@ AS __value FROM trade.process_run r WHERE r.run_id = @RUN_ID@
UNION ALL SELECT 'No Combo', '__none' WHERE NOT EXISTS (SELECT 1 FROM trade.process_run r WHERE r.run_id = @RUN_ID@)""",
              PAIR=PAIR.format(r="r"), RUN_ID=RUN_ID)
# For titles: the Run (Run 07 · Live), the Range in words (MM-DD HH:MI – MM-DD HH:MI, now while it reaches the present; an
# empty one says why), and the legs' tickers.
Q_RUN_TEXT = sql("""WITH @SEL@
SELECT coalesce((SELECT @NAME@ FROM sel), 'No Run') AS __text""", SEL=SEL, NAME=RUN_NAME.format(r="sel"))
Q_SPAN = sql("""WITH @SEL@
SELECT CASE WHEN NOT EXISTS (SELECT 1 FROM sel) THEN 'no Run' WHEN rg.from_ms >= rg.to_ms THEN 'empty: Until not after Since'
            ELSE @FROM@ || ' – ' || CASE WHEN rg.to_ms > @NOW@ THEN 'now' ELSE @TO@ END END AS __text
FROM (SELECT 1) one LEFT JOIN rg ON true""", SEL=SEL, NOW=NOW_MS, FROM=clock("rg.from_ms", "MM-DD HH24:MI"), TO=clock("rg.to_ms", "MM-DD HH24:MI"))
# The Range as the time picker's from and to (ms; now while it reaches the present), for the link that fits the charts to it.
Q_RANGE_FROM = sql("""WITH @SEL@ SELECT coalesce((SELECT rg.from_ms::text FROM rg), 'now-6h') AS __text""", SEL=SEL)
Q_RANGE_TO = sql("""WITH @SEL@ SELECT coalesce((SELECT CASE WHEN rg.to_ms > @NOW@ THEN 'now' ELSE rg.to_ms::text END FROM rg), 'now') AS __text""",
                 SEL=SEL, NOW=NOW_MS)


def run_config(path, fallback):
    """A setting of the picked Run as text, for titles."""
    return "SELECT coalesce((SELECT r.config #>> '{%s}' FROM trade.process_run r WHERE r.run_id = %s), '%s')" % (path, RUN_ID, fallback)


# The program's settings of each run (process_run.config, the effective config.yaml).
D_IN = "(r.config -> 'strategy' ->> 'd_in')::float8"
D_OUT = "(r.config -> 'strategy' ->> 'd_out')::float8"
# The lines of the Run in words, for the Live Execution row's title: Long in −2σ / out +0.5σ · Short in +2σ / out −0.5σ.
SIGNED_SIGMA = "CASE WHEN {x} < 0 THEN '−' ELSE '+' END || trim_scale(abs({x})::numeric)::text || 'σ'"
LINES_TEXT = sql("""'Long in ' || @L_IN@ || ' / out ' || @L_OUT@ || ' · Short in ' || @S_IN@ || ' / out ' || @S_OUT@""",
                 L_IN=SIGNED_SIGMA.format(x="-" + D_IN), L_OUT=SIGNED_SIGMA.format(x="-" + D_OUT),
                 S_IN=SIGNED_SIGMA.format(x=D_IN), S_OUT=SIGNED_SIGMA.format(x=D_OUT))
Q_LINES = sql("""SELECT coalesce((SELECT @LINES@ FROM trade.process_run r WHERE r.run_id = @RUN_ID@), 'no Run')""", LINES=LINES_TEXT, RUN_ID=RUN_ID)

# --------------------------------------------------------------------------------------------------------------- the top
# The Run's newest signal and inventory samples (the last two days before its end), whatever the Range: what it saw last.
LATEST = sql("""@SEL@,
     r AS (SELECT * FROM sel), sr AS (SELECT * FROM sel),
     s AS (SELECT * FROM trade.signal_sample s WHERE @RUN_S@ AND s.ts_ms <= (SELECT sel.end_ms FROM sel)
           AND s.ts_ms > (SELECT sel.end_ms FROM sel) - 172800000 ORDER BY s.ts_ms DESC LIMIT 1),
     i AS (SELECT * FROM trade.inventory_sample i WHERE @RUN_I@ AND i.ts_ms <= (SELECT sel.end_ms FROM sel)
           AND i.ts_ms > (SELECT sel.end_ms FROM sel) - 172800000 ORDER BY i.ts_ms DESC LIMIT 1)""",
             SEL=SEL, RUN_S=of_run("s"), RUN_I=of_run("i"))
# What the open cycle's lots hold per leg (contracts left and their entry cost, USDT), the entry fees of that quantity, its
# direction and opening.
HELD = sql("""h AS (SELECT sum(l.entry_qty_bull - l.exit_qty_bull) AS q_bull,
             sum(CASE WHEN l.entry_qty_bull > 0 THEN l.entry_value_bull * (l.entry_qty_bull - l.exit_qty_bull) / l.entry_qty_bull END) AS v_bull,
             sum(l.entry_qty_bear - l.exit_qty_bear) AS q_bear,
             sum(CASE WHEN l.entry_qty_bear > 0 THEN l.entry_value_bear * (l.entry_qty_bear - l.exit_qty_bear) / l.entry_qty_bear END) AS v_bear,
             sum(CASE WHEN l.entry_qty_bull > 0 THEN l.entry_fee_bull * (l.entry_qty_bull - l.exit_qty_bull) / l.entry_qty_bull ELSE 0 END
                 + CASE WHEN l.entry_qty_bear > 0 THEN l.entry_fee_bear * (l.entry_qty_bear - l.exit_qty_bear) / l.entry_qty_bear ELSE 0 END) AS fee,
             max(c.direction) AS dir, min(c.opened_ms) AS opened_ms
      FROM trade.cycle c JOIN trade.lot l ON l.live = c.live AND l.cycle_id = c.cycle_id
      WHERE @RUN_C@ AND c.status = 'open' AND l.status IN ('building', 'open'))""", RUN_C=cycle_of_run("c"))
# The open cycle's unrealised PnL at the newest second's mids after the entry fees of what is held, before exit fees and
# the half spread an exit pays: what selling (a Long) or buying back (a Short) both legs at their mids gives against their
# entry cost.
UNREALISED = ("CASE WHEN h.dir IS NOT NULL THEN (CASE h.dir WHEN 'short' THEN -1 ELSE 1 END) * "
              "(s.mid_bull * h.q_bull - h.v_bull + s.mid_bear * h.q_bear - h.v_bear) - h.fee END")
SIGMA = "nullif(s.sigma_d, 0)"
# A sample whose books the executor would trade on (owner item 70, the auditor's review: core/exec/step.go blocks an entry on a
# leg's conflicting book or unusable depth): sigma ready, both legs' depth usable, neither book in conflict. The executable
# figures (the distances to the entry lines, the Buy Both / Sell Both curves of Live Execution) are shown only then; the raw
# Buy Both / Sell Both of the Entry strip stay, for diagnosis, and Why names the leg.
EXECUTABLE = "({s}.stats_ready AND {s}.depth_ok_bull AND {s}.depth_ok_bear AND NOT ({s}.conflict_bull OR {s}.conflict_bear))"
# A leg's book in words: what keeps the executor from trading on it, or OK.
BOOK_ISSUES = [("conflict_bull", "$bull Book Conflict"), ("conflict_bear", "$bear Book Conflict"),
               ("NOT s.depth_ok_bull", "$bull Book Unusable"), ("NOT s.depth_ok_bear", "$bear Book Unusable")]
BOOK_NOTE = "concat_ws(', ', %s)" % ", ".join("CASE WHEN %s THEN '%s' END" % (c if c.startswith("NOT") else "s." + c, w) for c, w in BOOK_ISSUES)
# Distances in sigma, positive while the line is still ahead: to a Long's entry (Buy Both down to -d_in) and a Short's
# (Sell Both up to +d_in); to the open cycle's exit (a Long: Sell Both up to -d_out; a Short: Buy Both down to +d_out).
TO_LONG = "(s.x_long_d / %s + (sr.config -> 'strategy' ->> 'd_in')::float8)" % SIGMA
TO_SHORT = "((sr.config -> 'strategy' ->> 'd_in')::float8 - s.x_short_d / %s)" % SIGMA
TO_EXIT = ("CASE s.direction WHEN 'long' THEN -(sr.config -> 'strategy' ->> 'd_out')::float8 - s.x_short_d / {s} "
           "WHEN 'short' THEN s.x_long_d / {s} - (sr.config -> 'strategy' ->> 'd_out')::float8 END").format(s=SIGMA)


def distance(x):
    """A distance to a line as text: 0.77σ · 3.9 D, or At the Line once crossed; – without sigma, — while a leg's book is
    not one the executor trades on (EXECUTABLE; Why names the leg)."""
    return ("CASE WHEN NOT s.stats_ready OR {x} IS NULL THEN NULL WHEN NOT {e} THEN '—' WHEN {x} <= 0 THEN 'At the Line' "
            "ELSE to_char({x}, 'FM990.00') || 'σ · ' || to_char({x} * s.sigma_d, 'FM9,990.0') || ' D' END").format(x=x, e=EXECUTABLE.format(s="s"))


def why(code):
    """The program's reason in words, and the leg whose book keeps an entry from going out when that is not the reason itself
    (Regular Hours · SOXS Book Conflict)."""
    return ("%s || CASE WHEN s.stats_ready AND NOT %s AND %s NOT IN ('conflict_bull', 'conflict_bear', 'depth_bull', 'depth_bear') "
            "THEN ' · ' || %s ELSE '' END") % (idle_why(code), EXECUTABLE.format(s="s"), "coalesce(%s, '')" % code, BOOK_NOTE)


ERRORS_1H = "(SELECT count(*) FROM trade.event e WHERE %s AND e.level = '%%s' AND e.ts_ms > %s - 3600000)" % (of_run("e"), NOW_MS)
STALE = "(r.stopped_ms IS NULL AND (s.ts_ms IS NULL OR %s - s.ts_ms > %d * 1000) AND %s - r.started_ms >= 60000)" % (NOW_MS, SAMPLE_AGE_S, NOW_MS)
Q_NOW = sql("""
WITH @LATEST@, @HELD@
SELECT CASE WHEN r.run_id IS NULL THEN 'No Run' WHEN r.stopped_ms IS NOT NULL THEN 'Stopped@S@·@S@' || @STOPPED@
            WHEN s.ts_ms IS NULL AND @NOW@ - r.started_ms < 60000 THEN 'Starting'
            WHEN @STALE@ THEN 'Stale@S@·@S@' || coalesce(@AGE@, 'no sample')
            ELSE 'Running@S@·@S@up@S@' || @UP@ END AS "Status",
       CASE s.phase WHEN 'off_hours' THEN 'Off@H@Hours' WHEN 'regular' THEN 'US Regular' WHEN 'close_window' THEN 'Close Window' ELSE s.phase END AS "Phase",
       CASE WHEN s.ts_ms IS NULL THEN NULL WHEN s.anchor_kind = 'nav' THEN 'NAV' WHEN s.anchor_kind = 'bell' THEN 'Bell' ELSE 'Missing' END AS "Anchor",
       CASE WHEN s.session_end_ms > @NOW@ THEN @CLOSE_AT@ || '@S@·@S@in@S@' || @CLOSE_IN@ WHEN s.session_end_ms IS NOT NULL THEN 'Passed' END AS "Session Close",
       CASE WHEN i.mismatch THEN 'Position Mismatch' WHEN r.run_id IS NOT NULL AND @STALE@ THEN 'Stale Data'
            WHEN s.idle_reason = 'store_error' THEN 'Records Not Written' WHEN s.idle_reason = 'settings' THEN 'Account Settings'
            WHEN @ERRORS@ > 0 THEN 'Errors@S@·@S@' || @ERRORS@ || ' in 1 h'
            WHEN s.idle_reason IN ('stopping', 'no_new_entries') THEN 'Stop Pending'
            WHEN s.idle_reason = 'order_unknown' THEN 'Order Unknown'
            WHEN @WARNINGS@ > 0 THEN 'Warnings@S@·@S@' || @WARNINGS@ || ' in 1 h'
            ELSE 'None' END AS "Attention",
       CASE WHEN s.ts_ms IS NULL THEN NULL WHEN s.direction IS NULL THEN 'Flat' ELSE initcap(s.direction) END AS "Side",
       CASE WHEN s.direction IS NOT NULL THEN CASE s.direction WHEN 'short' THEN -s.hold_bull ELSE s.hold_bull END END AS "Bull",
       CASE WHEN s.direction IS NOT NULL THEN CASE s.direction WHEN 'short' THEN -s.hold_bear ELSE s.hold_bear END END AS "Bear",
       h.v_bull + h.v_bear AS "Cost", @HELD_FOR@ AS "Held", @TO_EXIT@ AS "To Exit",
       @SPREAD@ AS "Spread", @MEAN@ AS "Mean", @SIGMA_D@ AS "σ", @DEVIATION@ AS "Deviation", s.f AS "F", @AS_OF@ AS "As Of",
       CASE WHEN s.ts_ms IS NOT NULL THEN @STATE@ END AS "Entry", CASE WHEN s.ts_ms IS NOT NULL THEN @WHY@ END AS "Why",
       CASE WHEN s.stats_ready THEN @BUY@ END AS "Buy Both", CASE WHEN s.stats_ready THEN @SELL@ END AS "Sell Both",
       @TO_LONG@ AS "To Long Entry", @TO_SHORT@ AS "To Short Entry"
FROM (SELECT 1) one LEFT JOIN r ON true LEFT JOIN s ON true LEFT JOIN i ON true LEFT JOIN h ON true LEFT JOIN sr ON true""",
            LATEST=LATEST, HELD=HELD, NOW=NOW_MS, STALE=STALE, AGE=span("%s - s.ts_ms" % NOW_MS), UP=span("%s - r.started_ms" % NOW_MS, seconds=False),
            STOPPED=clock("r.stopped_ms", "MM-DD HH24:MI"), CLOSE_AT=clock("s.session_end_ms", "HH24:MI"),
            CLOSE_IN=span("s.session_end_ms - %s" % NOW_MS), ERRORS=ERRORS_1H % "error", WARNINGS=ERRORS_1H % "warn",
            HELD_FOR=span("%s - h.opened_ms" % NOW_MS),
            TO_EXIT="CASE WHEN s.stats_ready AND s.depth_ok_bull AND s.depth_ok_bear THEN %s END" % shown(TO_EXIT, 2),
            SPREAD=shown("s.mid_d", 1), MEAN=shown("s.mu_d", 1), SIGMA_D=shown("s.sigma_d", 1),
            DEVIATION="CASE WHEN s.stats_ready THEN %s END" % shown("(s.mid_d - s.mu_d) / %s" % SIGMA, 2), AS_OF=clock("s.ts_ms", "HH24:MI:SS"),
            STATE=idle_state("s.idle_reason"), WHY=why("s.idle_reason"), BUY=shown("s.x_long_d / %s" % SIGMA, 2),
            SELL=shown("s.x_short_d / %s" % SIGMA, 2),
            TO_LONG=distance(TO_LONG), TO_SHORT=distance(TO_SHORT), S=NBSP, H=NBHY)
# The legs at the Run's newest second, one row each: the mid (USDT, to 0.001: SOXL's half tick, SOXS's tick), the anchor in USD (per share, the issuer's
# NAV or the bell's) and in USDT (USD ÷ F), and the book as the executor judges it (Conflict: the snapshot and the ticks
# disagree; Unusable: too old or too thin). The program records no bid / ask or pricing-level price per second.
Q_LEGS = sql("""
WITH @LATEST@
SELECT l.leg AS "Leg", l.mid AS "Mid", l.anchor AS "USD", l.anchor / nullif(s.f, 0) AS "USDT", l.book AS "Book"
FROM s CROSS JOIN LATERAL (VALUES
  (1, '$bull', @MID_BULL@, s.anchor_bull, CASE WHEN s.conflict_bull THEN 'Conflict' WHEN NOT s.depth_ok_bull THEN 'Unusable' ELSE 'OK' END),
  (2, '$bear', @MID_BEAR@, s.anchor_bear, CASE WHEN s.conflict_bear THEN 'Conflict' WHEN NOT s.depth_ok_bear THEN 'Unusable' ELSE 'OK' END)) l(o, leg, mid, anchor, book)
ORDER BY l.o""", LATEST=LATEST, MID_BULL=shown("s.mid_bull", 3), MID_BEAR=shown("s.mid_bear", 3))

# The banner: which mode and Run the page shows; paper sends no order to the venue.
Q_MODE = sql("""
WITH @SEL@
SELECT coalesce((SELECT CASE WHEN sel.live THEN 'LIVE · ' || @NUMBER@ || ' · real orders on Bitget'
                             ELSE 'PAPER · ' || @NUMBER@ || ' · simulated fills, nothing is sent to Bitget' END FROM sel),
                'NO RUN · pick a Run (or the Combo it trades)') AS "Mode" """, SEL=SEL, NUMBER=RUN_NUMBER.format(r="sel"))

# ------------------------------------------------------------------------------------------------------------------- P&L
# The round trips of the Run closed in the Range. A cycle whose funding is not known yet (funding_known false) is left out of
# every money figure (Net, Fees, Funding, Win Rate) until it is known; Trips counts it.
CLOSED = "trade.cycle c WHERE %s AND c.status <> 'open' AND %s" % (cycle_of_run("c"), in_range("c.closed_ms"))
# The Range's net as its running sum (the stat's sparkline, a step at each close) from its start to its end (now while it
# reaches the present); the last value is its net (0 when nothing closed).
Q_NET = sql("""
WITH @SEL@, k AS MATERIALIZED (SELECT c.closed_ms, c.net FROM @CLOSED@ AND c.funding_known AND c.end_reason IS DISTINCT FROM 'no_fill')
SELECT to_timestamp(t.ts / 1000.0) AS time, sum(t.net) OVER (ORDER BY t.ts ROWS UNBOUNDED PRECEDING) AS "Net"
FROM (SELECT rg.from_ms AS ts, 0 AS net FROM rg
      UNION ALL SELECT k.closed_ms, k.net FROM k
      UNION ALL SELECT least(rg.to_ms - 1, @NOW@), 0 FROM rg) t
ORDER BY t.ts""", SEL=SEL, CLOSED=CLOSED, NOW=NOW_MS)
Q_PNL = sql("""
WITH @LATEST@, c AS MATERIALIZED (SELECT c.* FROM @CLOSED@ AND c.end_reason IS DISTINCT FROM 'no_fill'),
k AS (SELECT * FROM c WHERE c.funding_known), @HELD@
SELECT (SELECT @UNREALISED@ FROM (SELECT 1) one LEFT JOIN s ON true LEFT JOIN h ON true) AS "Open P&L",
       (SELECT -sum(k.fees) FROM k) AS "Fees", (SELECT sum(k.funding) FROM k) AS "Funding",
       (SELECT count(*) FROM c) AS "Trips",
       (SELECT 100.0 * count(*) FILTER (WHERE k.net > 0) / nullif(count(*), 0) FROM k) AS "Won" """,
            CLOSED=CLOSED, LATEST=LATEST, HELD=HELD, UNREALISED=shown(UNREALISED, 2))

# ------------------------------------------------------------------------------------------------------- Live Execution
# The step of the drawn points: the finest of 1, 5, 10, 30 seconds and whole minutes that keeps a line under about 1500
# points, about the plot's width in pixels at 1680 px. Each point is a bucket of the per-second samples: its last spread,
# mean and sigma, the lowest buying deviation and the highest selling deviation in it (in sigma, the side each one triggers
# on), so a crossing between two drawn points is never lost.
STEP_MS = ("(SELECT CASE WHEN r <= 1000 THEN 1000 WHEN r <= 5000 THEN 5000 WHEN r <= 10000 THEN 10000 "
           "WHEN r <= 30000 THEN 30000 ELSE ceil(r / 60000) * 60000 END::bigint FROM (SELECT ($__to - $__from) / 1500.0 AS r) x)")

# The decisions drawn as markers: every entry, exit, completion, trim and forced exit, at its instant; an entry whose lot
# became void (no fill, one leg) is a Void Entry. Each with its orders' fills: both legs' filled contracts and average price,
# the notional (USDT, both legs) and the fees (a cost, negative), and its cycle's net (Pending, or Open while open). y: an
# entry's or exit's executable deviation at the decision in sigma (x_d / sigma_d); a completion's, trim's or forced exit's
# the deviation on the mids of the newest sample in the minute before it.
DECISION_KINDS = ("entry", "exit", "complete", "trim", "forced")
ACTIONS = {1: "Long Entry", 2: "Short Entry", 3: "Long Exit", 4: "Short Exit", 5: "Forced Exit", 6: "Trim", 7: "Complete", 8: "Void Entry"}
ACTION_COLORS = {"Long Entry": BLUE, "Short Entry": ORANGE, "Long Exit": BLUE, "Short Exit": ORANGE, "Forced Exit": BAD}
# The drawn dots: (name, the actions it draws, colour, size, in the legend).
DOTS = [("Long Entry", (1,), BLUE, 13, True), ("Short Entry", (2,), ORANGE, 13, True), ("Long Exit", (3,), BLUE + "b3", 9, False),
        ("Short Exit", (4,), ORANGE + "b3", 9, False), ("Forced Exit", (5,), BAD, 11, True), ("Other", (6, 7, 8), GREY, 8, True)]
MARKERS = sql("""
m0 AS (
  SELECT d.ts_ms, d.run_id, (d.detail ->> 'x_d')::float8 / nullif((d.detail ->> 'sigma_d')::float8, 0) AS x,
         CASE WHEN d.kind = 'entry' AND l.status = 'void' THEN 8 WHEN d.kind = 'entry' THEN CASE d.direction WHEN 'short' THEN 2 ELSE 1 END
              WHEN d.kind = 'exit' THEN CASE d.direction WHEN 'short' THEN 4 ELSE 3 END WHEN d.kind = 'forced' THEN 5 WHEN d.kind = 'trim' THEN 6
              ELSE 7 END AS action,
         CASE WHEN c.status = 'open' THEN @OPEN@ WHEN NOT c.funding_known THEN @PENDING@ ELSE c.net END AS cycle_net,
         coalesce(d.detail -> 'client_order_ids', jsonb_build_array(d.detail ->> 'client_order_id')) AS ids
  FROM trade.decision d
  LEFT JOIN trade.lot l ON l.live = d.live AND l.lot_id = d.lot_id
  LEFT JOIN trade.cycle c ON c.live = d.live AND c.cycle_id = d.cycle_id
  WHERE @RUN_D@ AND d.kind IN @KINDS@ AND d.ts_ms >= @CHART_FROM@ AND d.ts_ms <= @CHART_TO@),
m AS (
  SELECT m0.ts_ms, m0.run_id, m0.action, m0.cycle_net, coalesce(m0.x, n.dev) AS y, o.*
  FROM m0
  LEFT JOIN LATERAL (SELECT (s.mid_d - s.mu_d) / nullif(s.sigma_d, 0) AS dev FROM trade.signal_sample s
                     WHERE s.ts_ms <= m0.ts_ms AND s.ts_ms > m0.ts_ms - 60000 AND @RUN_S@ ORDER BY s.ts_ms DESC LIMIT 1) n ON m0.x IS NULL
  CROSS JOIN LATERAL (
    SELECT sum(o.cum_exec_qty) FILTER (WHERE o.leg = 'bull') AS q_bull, sum(o.cum_exec_value) FILTER (WHERE o.leg = 'bull') AS v_bull,
           sum(o.cum_exec_qty) FILTER (WHERE o.leg = 'bear') AS q_bear, sum(o.cum_exec_value) FILTER (WHERE o.leg = 'bear') AS v_bear,
           sum(o.fee) AS fee, bool_or(o.cum_exec_qty > 0 AND NOT o.fee_known) AS fee_pending
    FROM trade.cex_order o WHERE @RUN_O@ AND o.client_order_id IN (SELECT jsonb_array_elements_text(m0.ids))) o)""",
              RUN_D=of_run("d"), RUN_S=of_run("s"), RUN_O=of_mode("o"), CHART_FROM=CHART_FROM, CHART_TO=CHART_TO, KINDS="(%s)" % ", ".join("'%s'" % k for k in DECISION_KINDS), OPEN=str(OPEN), PENDING=str(PENDING))


# The marker columns: the drawn dots, on a decision's own row only (NULL elsewhere), and the tooltip rows. A dot is a few
# pixels wide, so the tooltip rows hold the decision c nearest a row within about 8 px of the plot (WINDOW_MS) on every row
# there, the decision's own row included, and 0 elsewhere, which the tooltip hides: hovering near a dot shows it, and
# nowhere else does a decision show. Decided is its instant. A dot without a y (no sigma) is not drawn; its tooltip shows.
def near(x):
    """A tooltip row of the decision near a row: 0 (hidden) where there is none."""
    return "CASE WHEN c.n IS NULL THEN 0 ELSE %s END" % x


def avg_price(leg):
    return near("CASE WHEN c.q_{l} > 0 THEN {p} ELSE {no} END".format(l=leg, p=shown("c.v_%s / c.q_%s" % (leg, leg), TICK_DECIMALS[leg]), no=NO_FILL))


MARKER_INFO = [("Action", near("c.action")), ("Decided", near("c.ts_ms")),
               ("Bull Filled", near(nonzero("coalesce(c.q_bull, 0)", 2))), ("Bull Avg", avg_price("bull")),
               ("Bear Filled", near(nonzero("coalesce(c.q_bear, 0)", 2))), ("Bear Avg", avg_price("bear")),
               ("Notional", near(nonzero("coalesce(c.v_bull, 0) + coalesce(c.v_bear, 0)", 2))),
               ("Fees", near("CASE WHEN c.fee_pending THEN %d ELSE %s END" % (FEE_PENDING, nonzero("-coalesce(c.fee, 0)", 2)))),
               ("Cycle Net", near("CASE WHEN c.cycle_net <= %d THEN c.cycle_net ELSE %s END" % (PENDING, nonzero("c.cycle_net", 2)))),
               ("At", near("CASE WHEN c.y IS NULL THEN %d ELSE %s END" % (NO_SAMPLE, nonzero("c.y", 2))))]
MARKER_COLUMNS = [(name, "CASE WHEN p.mark AND c.action IN (%s) THEN %s END" % (", ".join(map(str, acts)), shown("c.y", 2)))
                  for name, acts, _, _, _ in DOTS] + MARKER_INFO
WINDOW_MS = "greatest(x.step, ($__to - $__from) / 150)"

# The columns of a drawn point. Deviation chart (sigma): the deviation on the mids, the executable band (Buy Both above,
# Sell Both below), the program's four lines; Spread chart (D): the spread, its mean and the two entry lines. The lines need
# sigma; they are computed here, not in the browser (Grafana's binary calculation reads a NULL as 0).
SIG = "nullif(p.sigma, 0)"
AXIS_SIGMA = 8   # the Deviation chart's axis: soft ±3 σ, never past ±8 σ
LINES = {"Long In": ("-p.d_in", BLUE, [6, 4]), "Long Out": ("-p.d_out", BLUE, [2, 3]),
         "Short In": ("p.d_in", ORANGE, [6, 4]), "Short Out": ("p.d_out", ORANGE, [2, 3])}
LINE_COLUMNS = ([("Deviation", shown("(p.mid - p.mu) / %s" % SIG, 2)), ("Buy Both", shown("p.xl", 2)), ("Sell Both", shown("p.xs", 2))] +
                [(name, "CASE WHEN %s IS NOT NULL THEN %s END" % (SIG, shown(k, 2))) for name, (k, _, _) in LINES.items()] +
                [("Spread", shown("p.mid", 1)), ("Mean", shown("p.mu", 1))])
MID_WHY = (("NOT p.anchored", "No Anchor"), ("true", "No Book"))
MU_WHY = (("NOT p.anchored", "No Anchor"), ("true", "Warmup"))
X_WHY = (("NOT p.anchored", "No Anchor"), (SIG + " IS NULL", "Warmup"), ("NOT p.booked", "Book Not OK"), ("true", "No Levels"))
TOOLTIP_COLUMNS = [
    ("Deviation · Tooltip", coded("(p.mid - p.mu) / %s" % SIG, 2, MU_WHY)), ("Buy Both · Tooltip", coded("p.xl", 2, X_WHY)),
    ("Sell Both · Tooltip", coded("p.xs", 2, X_WHY)), ("Spread · Tooltip", coded("p.mid", 1, MID_WHY)), ("Mean · Tooltip", coded("p.mu", 1, MU_WHY)),
    ("Sigma · Tooltip", coded("p.sigma", 1, MU_WHY))]
# The program's answer in the bucket's last second as a code for the tooltip (never 0: hidden there).
# (Grafana does not fill variables in a mapping's text: the legs are Bull and Bear here, their tickers in the Entry strip.)
IDLE_CODES = {100: "Order Sent", **{i + 1: "%s · %s" % (s, w) for i, (_, s, w) in enumerate(IDLE)}, 99: "Blocked · Other"}
STATE_COLUMNS = [("Entry", "CASE WHEN p.idle IS NULL THEN 100 %s ELSE 99 END" % " ".join(
    "WHEN p.idle = '%s' THEN %d" % (c, i + 1) for i, (c, _, _) in enumerate(IDLE)))]
# A decision row that carries no bucket's values (before the range's first point, or of another run than the point before
# it) has no state: its line, tooltip and state columns are hidden there (NULL breaks the lines, 0 hides the tooltip rows).
# Near a decision the tooltip shows the decision only (its own deviation in At): the point's rows are hidden there too.
ALL_COLUMNS = LINE_COLUMNS + [(n, "CASE WHEN p.anchored IS NULL OR c.n IS NOT NULL THEN 0 ELSE %s END" % q)
                              for n, q in TOOLTIP_COLUMNS + STATE_COLUMNS] + MARKER_COLUMNS
RAW = ["mid", "mu", "sigma", "xl", "xs", "d_in", "d_out", "anchored", "booked", "idle"]
# The Deviation chart's one query (the Spread chart shows its result): per step bucket of one run and one session, at its
# last sample; each decision as a row of its own at its instant, with the values of the bucket before it when that is of
# the same run (so it does not break the lines), and on every row the decision nearest it within WINDOW_MS, for the
# tooltip; a row with only its time 1 ms before a point that is more than two steps (at least 3 s) after the one before
# it, or of another session or run (the mean restarts), breaks the lines there, after the decisions of a run's last
# moments; rows of the same millisecond come break row, point, decision.
Q_EXECUTION = sql("""
WITH @SEL@, x AS MATERIALIZED (SELECT @STEP@ AS step),
b AS (
  SELECT max(s.ts_ms) AS ts, s.run_id, s.session_id,
         (array_agg(s.mid_d ORDER BY s.ts_ms DESC))[1] AS mid, (array_agg(s.mu_d ORDER BY s.ts_ms DESC))[1] AS mu,
         (array_agg(s.sigma_d ORDER BY s.ts_ms DESC))[1] AS sigma,
         min(CASE WHEN @EXECUTABLE@ THEN s.x_long_d / nullif(s.sigma_d, 0) END) AS xl,
         max(CASE WHEN @EXECUTABLE@ THEN s.x_short_d / nullif(s.sigma_d, 0) END) AS xs, coalesce(bool_or(@EXECUTABLE@), false) AS booked,
         (array_agg(s.anchor_kind IS NOT NULL ORDER BY s.ts_ms DESC))[1] AS anchored, (array_agg(s.idle_reason ORDER BY s.ts_ms DESC))[1] AS idle
  FROM trade.signal_sample s CROSS JOIN x
  WHERE s.ts_ms >= @CHART_FROM@ AND s.ts_ms <= @CHART_TO@ AND @RUN_S@
  GROUP BY s.ts_ms / x.step, s.run_id, s.session_id),
k AS (
  SELECT b.*, @D_IN@ AS d_in, @D_OUT@ AS d_out,
         lead(b.ts) OVER w AS next_ts, lead(b.run_id) OVER w AS next_run, lead(b.session_id) OVER w AS next_session
  FROM b CROSS JOIN sel r
  WINDOW w AS (ORDER BY b.ts)),
@MARKERS@,
mm AS (SELECT m.*, row_number() OVER (ORDER BY m.ts_ms) AS n FROM m),
u AS (
  SELECT k.ts, false AS mark, NULL::bigint AS n, k.run_id AS run, @RAW_K@ FROM k
  UNION ALL
  SELECT mm.ts_ms, true, mm.n, mm.run_id, @NULL_RAW@ FROM mm),
g AS (SELECT u.ts, u.mark, u.n, @WIN_RAW@, max(u.n) OVER (ORDER BY u.ts, u.mark ROWS UNBOUNDED PRECEDING) AS pn
      FROM (SELECT u.*, max(u.run) FILTER (WHERE NOT u.mark) OVER v AS brun FROM
              (SELECT u.*, count(*) FILTER (WHERE NOT u.mark) OVER (ORDER BY u.ts, u.mark) AS grp FROM u) u
            WINDOW v AS (PARTITION BY u.grp)) u
      WINDOW v AS (PARTITION BY u.grp)),
h AS (SELECT g.*, CASE WHEN g.mark THEN g.n
                       WHEN a.ts_ms IS NOT NULL AND g.ts - a.ts_ms <= @WINDOW@ AND (b.ts_ms IS NULL OR g.ts - a.ts_ms <= b.ts_ms - g.ts) THEN a.n
                       WHEN b.ts_ms IS NOT NULL AND b.ts_ms - g.ts <= @WINDOW@ THEN b.n END AS cn
      FROM g CROSS JOIN x LEFT JOIN mm a ON a.n = g.pn LEFT JOIN mm b ON b.n = coalesce(g.pn, 0) + 1)
SELECT to_timestamp(t.ts / 1000.0) AS time, @NAMES@ FROM (
  SELECT p.ts, CASE WHEN p.mark THEN 2 ELSE 1 END AS ord, @COLUMNS@ FROM h p LEFT JOIN mm c ON c.n = p.cn
  UNION ALL
  SELECT greatest(k.ts + 1, k.next_ts - 1), 0, @NULLS@ FROM k CROSS JOIN x
  WHERE k.next_ts IS NOT NULL AND (k.next_ts - k.ts > greatest(2 * x.step, 3000) OR k.next_run <> k.run_id OR k.next_session <> k.session_id)) t
ORDER BY t.ts, t.ord""", SEL=SEL, STEP=STEP_MS, RUN_S=of_run("s"), CHART_FROM=CHART_FROM, CHART_TO=CHART_TO, D_IN=D_IN, D_OUT=D_OUT, MARKERS=MARKERS, EXECUTABLE=EXECUTABLE.format(s="s"),
                  RAW_K=", ".join("k.%s" % c for c in RAW), NULL_RAW=", ".join("NULL" for _ in RAW), WINDOW=WINDOW_MS,
                  WIN_RAW=", ".join("CASE WHEN u.run IS DISTINCT FROM u.brun THEN NULL ELSE %s(u.%s) OVER v END AS %s"
                                    % ("bool_or" if c in ("anchored", "booked") else "max", c, c) for c in RAW),
                  NAMES=", ".join('t."%s"' % n for n, _ in ALL_COLUMNS),
                  COLUMNS=", ".join('%s AS "%s"' % (q, n) for n, q in ALL_COLUMNS), NULLS=", ".join('NULL AS "%s"' % n for n, _ in ALL_COLUMNS))


# The cycles as shaded spans on both execution charts, from their opening to their close (now while open), one query per
# direction; the text is what the span's marker under the chart shows.
def cycle_spans(direction):
    return sql("""
WITH @SEL@
SELECT c.opened_ms AS time, coalesce(c.closed_ms, @NOW@) AS time_end,
       initcap(c.direction) || ' ' || @OPENED@ || ' – ' || coalesce(@CLOSED@, 'open') || ' · ' || @REASON@ || ' · Net '
       || CASE WHEN c.status = 'open' THEN 'open' WHEN NOT c.funding_known THEN 'Pending' ELSE to_char(c.net, 'FM999,990.00') || ' USDT' END AS text
FROM trade.cycle c
WHERE @RUN_C@ AND c.direction = '@DIR@' AND c.end_reason IS DISTINCT FROM 'no_fill' AND c.opened_ms <= @CHART_TO@
  AND coalesce(c.closed_ms, @NOW@) >= @CHART_FROM@""",
               SEL=SEL, RUN_C=cycle_of_run("c"), CHART_FROM=CHART_FROM, CHART_TO=CHART_TO, NOW=NOW_MS, DIR=direction, OPENED=clock("c.opened_ms", "HH24:MI:SS"), CLOSED=clock("c.closed_ms", "HH24:MI:SS"),
               REASON=end_reason("c.end_reason"))


# Owner item 62: a mark on every time-series chart at each instant the anchors in use change, worded as Combo Spread's
# (3.5): "Anchor change · reason", then each leg's anchor before -> after in USD and in USDT (USD ÷ F), then F. The instants
# are the Run's own: a sample whose anchor kind or either leg's value differs from the Run's sample before (up to 10 min before
# the chart). Reasons: a new session (its close), on the bell; the NAV confirmed (both legs' NAV unchanged for 1 h; when first
# seen and confirmed, from anchor_fact), replacing the bell; the bell again (the NAV no longer confirmed); within its kind: a
# dividend when one was read for the session by then, else a split or a corrected reading; no anchor; an anchor read again
# after none. F: each side's own sample's, the rate the program used.
USD2 = "coalesce(to_char(%s, 'FM999,990.00'), '–')"
ANCHOR_PAIR = sql("""'<br>' || '$bull ' || @B0@ || ' → ' || @B1@ || ' USD · ' || @U0@ || ' → ' || @U1@ || ' USDT<br>'
       || '$bear ' || @E0@ || ' → ' || @E1@ || ' USD · ' || @V0@ || ' → ' || @V1@ || ' USDT<br>USDT = USD ÷ F, F = '
       || CASE WHEN x.pf = x.f THEN x.f::text ELSE coalesce(x.pf::text, '–') || ' → ' || coalesce(x.f::text, '–') END || ' USD/USDT (the program''s)'""",
                  B0=USD2 % "x.p_bull", B1=USD2 % "x.anchor_bull", U0=USD2 % "x.p_bull / x.pf", U1=USD2 % "x.anchor_bull / x.f",
                  E0=USD2 % "x.p_bear", E1=USD2 % "x.anchor_bear", V0=USD2 % "x.p_bear / x.pf", V1=USD2 % "x.anchor_bear / x.f")


def fact_time(kind, agg):
    """When the program read a fact of the session at or before the change, UTC+8."""
    return ("(SELECT %s FROM trade.anchor_fact a WHERE %s AND a.session_id = x.session_id AND a.kind = '%s' AND a.known_ms <= x.ts_ms)"
            % (clock("%s(a.known_ms)" % agg, "HH24:MI"), of_mode("a"), kind))


DIVIDEND = ("(SELECT string_agg(CASE a.leg WHEN 'bull' THEN '$bull' ELSE '$bear' END || ' ' || trim_scale(a.value), ', ') FROM trade.anchor_fact a "
            "WHERE %s AND a.session_id = x.session_id AND a.kind = 'dividend' AND a.known_ms <= x.ts_ms)" % of_mode("a"))
Q_ANCHOR_CHANGES = sql("""
WITH @SEL@
SELECT x.ts_ms AS time,
       'Anchor change · ' ||
       CASE WHEN x.anchor_kind IS NULL THEN 'no anchor (Missing): no entries until one is read'
            WHEN x.ps IS DISTINCT FROM x.session_id THEN 'new session ' || x.session_id || ' (the close)<br>bell anchor (F × Binance''s index at the close; the NAV to follow)'
            WHEN x.pk IS NULL THEN 'an anchor read again'
            WHEN x.pk = 'bell' AND x.anchor_kind = 'nav' THEN 'NAV confirmed for session ' || x.session_id
                 || '<br>both legs'' NAV unchanged for 1 h (Direxion' || coalesce(': first seen ' || @SEEN@ || ', confirmed ' || @KNOWN@, '') || ')'
            WHEN x.pk = 'nav' AND x.anchor_kind = 'bell' THEN 'the bell again: the NAV no longer confirmed'
            WHEN @DIVIDEND@ IS NOT NULL THEN 'a dividend takes effect (anchor − D; read for the session: ' || @DIVIDEND@ || ' USD)'
            ELSE 'a split, or a corrected reading, of the same kind'
       END
       || @PAIR@ AS text
FROM (SELECT s.ts_ms, s.session_id, s.anchor_kind, s.anchor_bull, s.anchor_bear, s.f,
             lag(s.ts_ms) OVER w AS pts, lag(s.session_id) OVER w AS ps, lag(s.anchor_kind) OVER w AS pk,
             lag(s.anchor_bull) OVER w AS p_bull, lag(s.anchor_bear) OVER w AS p_bear, lag(s.f) OVER w AS pf
      FROM trade.signal_sample s WHERE s.ts_ms >= @CHART_FROM@ - 600000 AND s.ts_ms <= @CHART_TO@ AND @RUN_S@
      WINDOW w AS (ORDER BY s.ts_ms)) x
WHERE x.ts_ms >= @CHART_FROM@ AND x.pts IS NOT NULL
  AND (x.anchor_kind, x.anchor_bull, x.anchor_bear) IS DISTINCT FROM (x.pk, x.p_bull, x.p_bear)""".replace("@DIVIDEND@", DIVIDEND),
                       SEL=SEL, RUN_S=of_run("s"), CHART_FROM=CHART_FROM, CHART_TO=CHART_TO, SEEN=fact_time("nav_seen", "min"), KNOWN=fact_time("nav", "max"), PAIR=ANCHOR_PAIR)

# -------------------------------------------------------------------------------------------------------------- Settings
# The Run's effective settings (process_run.config, owner item 71), five short lists a trader reads across: Signal, Sizing,
# Execution, Risk & Session, Run. Durations as Go writes them, shortened (1m0s -> 1m, 1h0m0s -> 1h).
def cfg(path):
    return "(r.config #>> '{%s}')" % path


def dur(path):
    return "regexp_replace(regexp_replace(%s, '([hm])0s$', '\\1'), 'h0m$', 'h')" % cfg(path)


def cfg_num(path, suffix=""):
    return "trim_scale((%s)::numeric)::text || '%s'" % (cfg(path), suffix)


def ordinal(path):
    return ("%s || CASE WHEN %s IN ('11', '12', '13') THEN 'th' WHEN right(%s, 1) = '1' THEN 'st' WHEN right(%s, 1) = '2' THEN 'nd' "
            "WHEN right(%s, 1) = '3' THEN 'rd' ELSE 'th' END") % ((cfg(path),) * 5)


SIGMA_LINE = "CASE WHEN {x} < 0 THEN '−' ELSE '+' END || trim_scale(abs({x}))::text || 'σ'"
D_IN_N, D_OUT_N = "(%s)::numeric" % cfg("strategy,d_in"), "(%s)::numeric" % cfg("strategy,d_out")
SETTINGS = [
    ("Signal", [
        ("Mean", "upper(%s) || ' ' || %s || 'm'" % (cfg("strategy,mean_model"), cfg("strategy,mean_minutes"))),
        ("σ Window", "%s || 'm'" % cfg("strategy,sigma_minutes")),
        ("Long In → Out", "%s || ' → ' || %s" % (SIGMA_LINE.format(x="-" + D_IN_N), SIGMA_LINE.format(x="-" + D_OUT_N))),
        ("Short In → Out", "%s || ' → ' || %s" % (SIGMA_LINE.format(x=D_IN_N), SIGMA_LINE.format(x=D_OUT_N))),
        ("Taker Fee", cfg_num("strategy,taker_fee_bps", " bps")),
        ("Cost Margin", cfg_num("strategy,cost_margin_d", " D")),
        ("Rewarm", "CASE WHEN %s = 'true' THEN 'Yes' ELSE 'No' END" % cfg("strategy,rewarm_on_anchor_change"))]),
    ("Sizing · USDT", [
        ("Capital M", "to_char(%s::numeric, 'FM999,999,990')" % cfg("inventory,capital")),
        ("Leverage L", cfg_num("inventory,leverage", "×")),
        ("Notional N", "to_char(%s::numeric * %s::numeric, 'FM999,999,990')" % (cfg("inventory,capital"), cfg("inventory,leverage"))),
        ("Min Batch", cfg_num("strategy,min_batch_notional")),
        ("Take Ratio", cfg_num("strategy,take_ratio")),
        ("Entry Spacing", dur("strategy,entry_spacing")),
        ("Entry Step", cfg_num("strategy,entry_step_d", " D"))]),
    ("Execution", [
        ("Mode", "CASE WHEN r.live THEN 'Live' ELSE 'Paper' END"),
        ("Pricing Level", "%s || ' / ' || %s" % (ordinal("strategy,pricing_level,bull"), ordinal("strategy,pricing_level,bear"))),
        ("Order Channel", "upper(%s)" % cfg("cex,order_transport")),
        ("Book Feed", "CASE WHEN %s = %s THEN %s ELSE %s || ' / ' || %s END || ' × ' || %s" % (
            cfg("cex,pair,bull,book_topic"), cfg("cex,pair,bear,book_topic"), cfg("cex,pair,bull,book_topic"), cfg("cex,pair,bull,book_topic"),
            cfg("cex,pair,bear,book_topic"), cfg("cex,book_connections"))),
        ("Complete Within", dur("strategy,completion_window")),
        ("Forced Slip", cfg_num("strategy,forced_slip_bps", " bps")),
        ("Paper Latency", "CASE WHEN NOT r.live THEN %s || ' / ' || %s END" % (
            dur("exec,sim_order_latency,bull"), dur("exec,sim_order_latency,bear")))]),
    ("Risk & Session", [
        ("Flatten Before", dur("strategy,forced_exit_before_close")),
        ("Book Max Age", dur("cex,depth_max_age")),
        ("F Max Age", dur("cex,fx_max_age")),
        ("NAV Settles", dur("cex,nav_settle_delay")),
        ("Stop Alarm", dur("exec,stop_alarm_after")),
        ("Ex-Dates", "coalesce((SELECT string_agg(a.d, ', ' ORDER BY a.d) FROM (SELECT DISTINCT replace(substr(x ->> 'ex_date', 6), '-', '%s') "
                               "|| CASE WHEN x ->> 'kind' = 'dividend' THEN '' ELSE ' ' || (x ->> 'kind') END AS d "
                               "FROM jsonb_array_elements(r.config #> '{cex,corporate_actions}') x) a), 'None')" % NBHY),
        ("Calendar Edits", "CASE WHEN coalesce(jsonb_array_length(r.config #> '{cex,calendar_overrides}'), 0) = 0 THEN 'None' "
                           "ELSE jsonb_array_length(r.config #> '{cex,calendar_overrides}')::text END")]),
    ("Run", [
        ("Started", clock("r.started_ms")),
        ("Stopped", "coalesce(%s, 'running')" % clock("r.stopped_ms")),
        ("Host", "r.host"),
        ("Run ID", "r.run_id")]),
]
# The panels' widths (grid units) and the width of their name column (px): the Run panel holds the run_id.
SETTINGS_WIDTHS = {"Signal": 8, "Sizing · USDT": 8, "Execution": 8, "Risk & Session": 12, "Run": 12}   # two rows, readable at 1280 / 880


def q_settings(group):
    rows = dict(SETTINGS)[group]
    return sql("""WITH @SEL@
SELECT v.setting AS "Setting", v.value AS "Value" FROM sel r CROSS JOIN LATERAL (VALUES @ROWS@) v(o, setting, value)
WHERE v.value IS NOT NULL ORDER BY v.o""",
               SEL=SEL, ROWS=", ".join("(%d, '%s', %s)" % (i, name, q) for i, (name, q) in enumerate(rows)))


# ----------------------------------------------------------------------------------------------------------- Round Trips
# One row per cycle of the Run open at any time in the Range, newest first, at most 500; no-fill cycles (nothing traded)
# left out. Notional: both legs' entry value (entries and completions); In: the lots' executable deviation at their entry
# decisions, in sigma, weighted by their entry value (a void lot that filled one leg counts); Out: the last exit decision's;
# Net in USDT (Pending, Open) and in bps of the notional.
IN_RANGE = "%s AND c.opened_ms < %s AND coalesce(c.closed_ms, %s) >= %s" % (cycle_of_run("c"), TO_MS, NOW_MS, FROM_MS)
TRADED = "%s AND c.end_reason IS DISTINCT FROM 'no_fill'" % IN_RANGE
NET_CODED = "CASE WHEN c.status = 'open' THEN %d WHEN NOT c.funding_known THEN %d ELSE c.net END" % (OPEN, PENDING)
Q_BLOTTER = sql("""
WITH @SEL@
SELECT c.cycle_id, to_timestamp(c.opened_ms / 1000.0) AS "Opened", to_timestamp(c.closed_ms / 1000.0) AS "Closed", @HELD@ AS "Held",
       initcap(c.direction) AS "Side", l.notional AS "Notional", @ENTRY@ AS "In σ", @EXIT@ AS "Out σ", @NET@ AS "Net",
       CASE WHEN c.status <> 'open' AND c.funding_known AND l.notional > 0 THEN 10000 * c.net / l.notional END AS "Net bps",
       @REASON@ AS "End Reason"
FROM trade.cycle c
CROSS JOIN LATERAL (SELECT sum(x.entry_value_bull + x.entry_value_bear) AS notional,
                           sum(x.x_d / nullif(x.sigma_d, 0) * (x.entry_value_bull + x.entry_value_bear))
                             / nullif(sum(x.entry_value_bull + x.entry_value_bear) FILTER (WHERE x.sigma_d <> 0), 0) AS entry
                    FROM trade.lot x WHERE x.live = c.live AND x.cycle_id = c.cycle_id) l
LEFT JOIN LATERAL (SELECT (d.detail ->> 'x_d')::float8 / nullif((d.detail ->> 'sigma_d')::float8, 0) AS x FROM trade.decision d
                   WHERE d.live = c.live AND d.cycle_id = c.cycle_id AND d.kind = 'exit' ORDER BY d.ts_ms DESC LIMIT 1) e ON true
WHERE @TRADED@
ORDER BY c.opened_ms DESC LIMIT 500""", SEL=SEL, TRADED=TRADED, HELD=span("coalesce(c.closed_ms, %s) - c.opened_ms" % NOW_MS),
                ENTRY=shown("l.entry", 2), EXIT=shown("e.x", 2), NET=NET_CODED, REASON=end_reason("c.end_reason"))
# The round trips of the Range for the (hidden) Round Trip setting, newest first (the default); no-fill ones too, so that
# one can be looked at by its address; one option that says so when there is none.
Q_CYCLE_OPTIONS = sql("""
WITH @SEL@
SELECT * FROM (
  SELECT @OPENED@ || ' · ' || initcap(c.direction) || ' · ' || @REASON@ AS __text, c.cycle_id AS __value
  FROM trade.cycle c WHERE @TRADED@ ORDER BY c.opened_ms DESC LIMIT 500) x
UNION ALL SELECT 'No Round Trip in the Range', 'none'
WHERE NOT EXISTS (SELECT 1 FROM trade.cycle c WHERE @TRADED@)""", SEL=SEL, OPENED=clock("c.opened_ms"), REASON=end_reason("c.end_reason"), TRADED=TRADED)

# The picked round trip's orders, in the order the program registered them: fills folded into Filled, Avg and Fee (a cost,
# negative; Pending while the venue has not reported it); Latency: sent to the final state in hand, this host's clock.
CYCLE = "'$cycle'"


def leg_price(x):
    return "CASE o.leg WHEN 'bull' THEN to_char(%s, 'FM9990.%s') ELSE to_char(%s, 'FM9990.%s') END" % (
        x, "0" * TICK_DECIMALS["bull"], x, "0" * TICK_DECIMALS["bear"])


Q_ORDERS = sql("""
WITH @SEL@
SELECT to_timestamp(o.submitted_ms / 1000.0) AS "Time", initcap(o.purpose) AS "Purpose", CASE o.leg WHEN 'bull' THEN '$bull' ELSE '$bear' END AS "Leg",
       initcap(o.side) || CASE WHEN o.reduce_only THEN ' · RO' ELSE '' END AS "Side", o.qty AS "Qty", o.cum_exec_qty AS "Filled",
       @LIMIT@ AS "Limit", @AVG@ AS "Avg", CASE WHEN o.cum_exec_qty = 0 THEN NULL WHEN NOT o.fee_known THEN @FEE_PENDING@ ELSE -o.fee END AS "Fee",
       o.final_ms - o.sent_ms AS "Latency",
       CASE WHEN o.unknown THEN 'Unknown' WHEN o.status = 'filled' THEN 'Filled' WHEN o.status = 'cancelled' AND o.cum_exec_qty > 0 THEN 'Partial'
            WHEN o.status = 'cancelled' THEN 'No Fill' WHEN o.status IN ('pending', 'acked') THEN 'Working'
            ELSE initcap(replace(o.status, '_', ' ')) END || coalesce(' · ' || nullif(concat_ws(' ', o.error_code, o.error_message), ''), '') AS "Status"
FROM trade.cex_order o WHERE @RUN_O@ AND o.cycle_id = @CYCLE@ ORDER BY o.submitted_ms, o.client_order_id""",
               SEL=SEL, RUN_O=of_mode("o"), CYCLE=CYCLE, FEE_PENDING=str(FEE_PENDING), LIMIT=leg_price("o.price"), AVG=leg_price("o.avg_price"))

# ---------------------------------------------------------------------------------------------------------------- Risk
# The Run's newest inventory sample: each leg's exposure (the venue's position at the newest second's mid, USDT, signed), the
# quota used of its total, the account's equity and margin ratio (live only; the whole account); the newest settled funding
# rate of each leg (the mode's) and the settlements the Run's finished cycles still wait for.
RATES = ("SELECT f.settlement_ms, max(f.rate) FILTER (WHERE f.leg = 'bull') AS bull, max(f.rate) FILTER (WHERE f.leg = 'bear') AS bear "
         "FROM trade.funding_rate f WHERE %s GROUP BY f.settlement_ms" % of_mode("f"))
Q_RISK = sql("""
WITH @LATEST@, f AS (@RATES@), n AS (SELECT * FROM f ORDER BY f.settlement_ms DESC LIMIT 1)
SELECT @EXP_BULL@ AS "Bull Exposure", @EXP_BEAR@ AS "Bear Exposure",
       CASE WHEN i.quota_total > 0 THEN 100 * i.quota_used / i.quota_total END AS "Quota Used", i.quota_total AS "Quota Total",
       i.equity AS "Equity", 100 * i.margin_ratio AS "Margin Ratio", @RATE_BULL@ AS "Bull Funding", @RATE_BEAR@ AS "Bear Funding",
       (SELECT count(*) FROM trade.cycle c CROSS JOIN LATERAL jsonb_array_elements(c.funding_awaited) a
        WHERE @RUN_C@ AND jsonb_typeof(c.funding_awaited) = 'array') AS "Funding Awaited"
FROM (SELECT 1) one LEFT JOIN r ON true LEFT JOIN s ON true LEFT JOIN i ON true LEFT JOIN sr ON true LEFT JOIN n ON true""",
             LATEST=LATEST, RATES=RATES, RUN_C=cycle_of_run("c"), EXP_BULL=shown("i.pos_bull * s.mid_bull", 2), EXP_BEAR=shown("i.pos_bear * s.mid_bear", 2),
             RATE_BULL=shown("10000 * n.bull", 2), RATE_BEAR=shown("10000 * n.bear", 2))

# ---------------------------------------------------------------------------------------------- Diagnostics · Execution
Q_END_REASONS = sql("""
WITH @SEL@
SELECT @REASON@ AS "End Reason", count(*) AS "Cycles", sum(c.net) FILTER (WHERE c.funding_known) AS "Net",
       count(*) FILTER (WHERE NOT c.funding_known) AS "Pending"
FROM trade.cycle c WHERE @IN_RANGE@ AND c.status <> 'open' GROUP BY c.end_reason ORDER BY count(*) DESC, 1""",
                    SEL=SEL, IN_RANGE=IN_RANGE, REASON=end_reason("c.end_reason"))
Q_IDLE = sql("""
WITH @SEL@
SELECT @STATE@ || ' · ' || @WHY@ AS "Answer", count(*) AS "Seconds", 100.0 * count(*) / sum(count(*)) OVER () AS "Share"
FROM trade.signal_sample s WHERE @RANGE_S@ AND @RUN_S@
GROUP BY s.idle_reason ORDER BY count(*) DESC""", SEL=SEL, RANGE_S=in_range("s.ts_ms"), RUN_S=of_run("s"),
             STATE=idle_state("s.idle_reason"), WHY=idle_why("s.idle_reason"))
Q_BLOCKED = sql("""
WITH @SEL@
SELECT @WHY@ AS "Reason", initcap(d.direction) AS "Side", count(*) AS "Seconds", to_timestamp(max(d.ts_ms) / 1000.0) AS "Last"
FROM trade.decision d WHERE @RUN_D@ AND d.kind = 'at_line_blocked' AND @RANGE_D@
GROUP BY d.reason, d.direction ORDER BY count(*) DESC""", SEL=SEL, RUN_D=of_run("d"), RANGE_D=in_range("d.ts_ms"), WHY=idle_why("d.reason"))
LATENCY_STAGES = [("Submit → Sent", "local", "o.sent_ms - o.submitted_ms"), ("Sent → Ack", "local", "o.acked_ms - o.sent_ms"),
                  ("Sent → Final", "local", "o.final_ms - o.sent_ms"), ("Accepted → Created", "venue", "o.exchange_created_ms - o.accepted_ms"),
                  ("Created → Last Update", "venue", "o.exchange_updated_ms - o.exchange_created_ms")]
Q_LATENCY = sql("""
WITH @SEL@
SELECT k.stage AS "Stage", k.clock AS "Clock", count(k.ms) AS "Orders", percentile_cont(0.5) WITHIN GROUP (ORDER BY k.ms) AS "Median",
       percentile_cont(0.9) WITHIN GROUP (ORDER BY k.ms) AS "P90", max(k.ms) AS "Max"
FROM trade.cex_order o
CROSS JOIN LATERAL (VALUES @STAGES@) k(i, stage, clock, ms)
WHERE @RUN_O@ AND @RANGE_O@ AND k.ms IS NOT NULL
GROUP BY k.i, k.stage, k.clock ORDER BY k.i""", SEL=SEL, RUN_O=of_run("o"), RANGE_O=in_range("o.submitted_ms"),
                STAGES=", ".join("(%d, '%s', '%s', %s)" % (i, s, c, q) for i, (s, c, q) in enumerate(LATENCY_STAGES)))

# ------------------------------------------------------------------------------------------------- Diagnostics · System
# The Run's warnings and errors of every module in the Range, newest first; a row that says so when there is none.
ALERT_ROWS = "trade.event e WHERE %s AND e.level IN ('warn', 'error') AND %s" % (of_run("e"), in_range("e.ts_ms"))
Q_ALERTS = sql("""
WITH @SEL@
SELECT * FROM (
  SELECT to_timestamp(e.ts_ms / 1000.0) AS "Time", initcap(e.level) AS "Level", e.module AS "Module", e.kind AS "Kind", e.message AS "Message",
         e.detail::text AS "Detail"
  FROM @ROWS@
  ORDER BY e.ts_ms DESC, e.seq DESC LIMIT 200) a
UNION ALL
SELECT NULL, NULL, NULL, NULL, 'No warning or error in the Range', NULL
WHERE NOT EXISTS (SELECT 1 FROM @ROWS@)""", SEL=SEL, ROWS=ALERT_ROWS)
# Every Run of both modes, newest first (the list the Run picker offers): its number and mode, start, stop, state, how long
# it ran, its notional N = M × L (USDT) and where it ran; the one the page shows is marked.
Q_RUNS = sql("""
WITH @SEL@
SELECT @NAME@ || CASE WHEN r.run_id = @RID@ THEN ' · shown' ELSE '' END AS "Run", to_timestamp(r.started_ms / 1000.0) AS "Started",
       to_timestamp(r.stopped_ms / 1000.0) AS "Stopped", initcap(@STATE@) AS "State", @RAN@ AS "Ran",
       (r.config #>> '{inventory,capital}')::numeric * (r.config #>> '{inventory,leverage}')::numeric AS "N",
       r.host AS "Host", r.run_id AS "Run ID", r.started_ms::text AS from_ms, coalesce(r.stopped_ms::text, 'now') AS to_ms
FROM trade.process_run r
ORDER BY r.started_ms DESC LIMIT 100""", SEL=SEL, NAME=RUN_NAME.format(r="r"), RID=RID, STATE=RUN_STATE,
             RAN=span("coalesce(r.stopped_ms, %s) - r.started_ms" % NOW_MS))
Q_ANCHORS = sql("""
WITH @SEL@
SELECT a.session_id AS "Session", a.kind AS "Kind", CASE a.leg WHEN 'bull' THEN '$bull' ELSE '$bear' END AS "Leg",
       to_timestamp(a.known_ms / 1000.0) AS "Known", a.value AS "Value", a.f AS "F", a.source AS "Source", a.detail::text AS "Detail"
FROM trade.anchor_fact a
WHERE @MODE_A@ AND a.session_id IN (SELECT DISTINCT s.session_id FROM trade.signal_sample s WHERE @RANGE_S@ AND @RUN_S@)
ORDER BY a.known_ms DESC, a.leg LIMIT 200""", SEL=SEL, MODE_A=of_mode("a"), RANGE_S=in_range("s.ts_ms"), RUN_S=of_run("s"))
# The Run's funding records of the Range (booked to the cycle whose interval holds the settlement), and the settlements a
# finished cycle still waits for (Awaited); newest first; a row that says so when there is neither.
FUNDING_ROWS = "trade.funding f WHERE %s AND %s" % (of_run("f"), in_range("f.settlement_ms"))
AWAITED = "trade.cycle c WHERE %s AND jsonb_typeof(c.funding_awaited) = 'array'" % cycle_of_run("c")
Q_FUNDING = sql("""
WITH @SEL@
SELECT * FROM (
  SELECT to_timestamp(f.settlement_ms / 1000.0) AS "Settlement", f.symbol AS "Contract", 'Booked' AS "Status", f.amount AS "Amount",
         f.cycle_id AS "Cycle"
  FROM @FUNDING@
  UNION ALL
  SELECT to_timestamp((a ->> 'settlement_ms')::bigint / 1000.0), CASE a ->> 'leg' WHEN 'bull' THEN '$bull' ELSE '$bear' END, 'Awaited', NULL,
         c.cycle_id
  FROM trade.cycle c CROSS JOIN LATERAL jsonb_array_elements(c.funding_awaited) a
  WHERE @RUN_C@ AND jsonb_typeof(c.funding_awaited) = 'array') t
UNION ALL
SELECT NULL, NULL, 'None in the Range, none awaited', NULL, NULL
WHERE NOT EXISTS (SELECT 1 FROM @FUNDING@)
  AND NOT EXISTS (SELECT 1 FROM @AWAITED@ AND jsonb_array_length(c.funding_awaited) > 0)
ORDER BY 1 DESC NULLS LAST""", SEL=SEL, FUNDING=FUNDING_ROWS, AWAITED=AWAITED, RUN_C=cycle_of_run("c"))


# ------------------------------------------------------------------------------------------------------------- the board
def collapsed_row(title, y, panels):
    """A row shown folded: its panels are queried only when it is opened. Their places count from the row's own."""
    r = row(title, y)
    r["collapsed"] = True
    r["panels"] = panels
    return r


def regex_colors(pairs):
    """Value mappings that colour a text by its start: [(prefix, colour)]."""
    return [{"type": "regex", "options": {"pattern": "^%s.*" % p, "result": {"color": c, "index": i}}} for i, (p, c) in enumerate(pairs)]


MONEY = {"unit": "none", "decimals": 2, "thresholds": SIGNED, "mappings": [NO_FIGURE]}
EXPOSURE = {"unit": "none", "decimals": 2, "mappings": [NO_FIGURE]}   # a position, not a gain: no red / green
PENDING_TEXT = code_texts({PENDING: "Pending", FEE_PENDING: "Pending", OPEN: "Open"}, {"Pending": WARN, "Open": TEXT})
STATUS_COLORS = regex_colors([("Running", GOOD), ("Stale", BAD), ("Starting", WARN), ("Stopped", GREY), ("No Run", GREY), ("Close Window", WARN),
                              ("Missing", WARN), ("Bell", GREY), ("Passed", GREY), ("Position Mismatch", BAD), ("Stale Data", BAD),
                              ("Records Not Written", BAD), ("Account Settings", BAD), ("Errors", BAD), ("Stop Pending", WARN),
                              ("Order Unknown", WARN), ("Warnings", WARN), ("None", GREY)])
SIDE_COLORS = regex_colors([("Long", BLUE), ("Short", ORANGE)])


def build():
    reset_ids(TRADE)
    hint = ("The Run first: Newest Live Run (the default: the live run started last; a new start is followed by itself), Newest Paper "
            "Run, or one Run: Run nn (numbered by start, both modes) · Live or Paper · start – end (now while it runs) · running, stopped "
            "or lost (no stop recorded and no sample for a minute). One Run is one start of the program: one mode, one set of settings.")
    templating = [
        query_var("run", "Run", Q_RUN_OPTIONS, hint),
        query_var("since", "Since", Q_SINCE, "Where the figures start: Run start, or a UTC+8 date the Run ran on, at the hour on its right."),
        query_var("since_hour", "At", Q_SINCE_HOUR, "The hour (UTC+8) of Since: 00:00 to 23:00; a dash while Since is Run start."),
        query_var("until", "Until", Q_UNTIL, "Where the figures end: Run end (now while it runs), or a UTC+8 date at the hour on its right, "
                  "that moment excluded."),
        query_var("until_hour", "At", Q_UNTIL_HOUR, "The hour (UTC+8) of Until: 01:00 to 24:00 (24:00: through the whole date); a dash "
                  "while Until is Run end."),
        query_var("combo", "Combo", Q_COMBO, "The pair the Run trades (today one: SOXL / SOXS, Bitget's perpetuals)."),
        query_var("cycle", "Round Trip", Q_CYCLE_OPTIONS, "The round trip whose orders are shown: the Range's newest (the default); a click "
                  "on Opened in Round Trips picks one.", hide=2),
        query_var("span", "Range", Q_SPAN, "The Range in words, for titles.", hide=2),
        query_var("runtext", "Run Name", Q_RUN_TEXT, "", hide=2),
        query_var("range_from", "Range From", Q_RANGE_FROM, "", hide=2),
        query_var("range_to", "Range To", Q_RANGE_TO, "", hide=2),
        query_var("bull", "Bull", run_config("cex,pair,bull,ticker", "SOXL"), "", hide=2),
        query_var("bear", "Bear", run_config("cex,pair,bear,ticker", "SOXS"), "", hide=2),
        query_var("lines", "Lines", Q_LINES, "", hide=2),
    ]
    var = {v["name"]: v for v in templating}
    var["run"]["current"] = {"text": "Newest Live Run", "value": "__live"}
    var["since"]["current"] = {"text": "Run start", "value": "0"}
    var["since_hour"]["current"] = {"text": "—", "value": "00"}
    var["until"]["current"] = {"text": "Run end", "value": NO_END_MS}
    var["until_hour"]["current"] = {"text": "—", "value": "24"}
    for v in templating:
        v["refresh"] = 2   # again on every change of the time range: the Run list and the Range follow a new start or stop
    var["cycle"]["allowCustomValue"] = True
    panels, y = [], 0
    words = {"mappings": [NO_FIGURE] + STATUS_COLORS}

    # ------------------------------------------------------------------ the top: the trader's questions without scrolling
    mode = stat_strip("", Q_MODE, [("Mode", "", None)], 0, y, 24, 1, intro=(
        "Which book and Run the page shows. Paper: the program trades against its own copy of the books; no order leaves the process "
        "(exec.live false) · Live: orders go to Bitget · every panel shows the picked Run only, so paper and live are never merged."),
        value_size=14)
    mode["options"].update(textMode="value", colorMode="background")
    mode["fieldConfig"]["defaults"]["mappings"] = regex_colors([("PAPER", "#5a5953"), ("LIVE", "#8a1f1f"), ("NO RUN", "#5a5953")])
    panels.append(mode); y += 1
    health = stat_strip("Health", Q_NOW, [
        ("Status", "Running and for how long (green) · Starting: a new run, no sample yet · Stale (red): no sample for more than %d s "
                   "and no stop recorded — the process crashed or hung · Stopped (grey): stopped in order, and when · No Run." % SAMPLE_AGE_S, words),
        ("Phase", "Off-Hours: entries allowed · US Regular: exits only · Close Window (amber): flattening before the close.", words),
        ("Anchor", "NAV: both legs on the issuer's NAV · Bell (grey): F × Binance's index at the close, provisional until the NAV is "
                   "confirmed · Missing (amber): no anchor, no entries.", words),
        ("Session Close", "The regular close that ends the session (UTC+8) and the time left: everything is flat by then.", words),
        ("Attention", "None, or the first thing a person must look at: Position Mismatch, Stale Data, Records Not Written, Account Settings, "
                      "Errors in the last hour (red); Stop Pending, Order Unknown, Warnings in the last hour (amber) · details in "
                      "Diagnostics · System.", words),
    ], 0, y, 24, 3, intro="The Run's newest sample (its last second; a stopped Run's last), whatever the Range.", columns=[
        "Status", "Phase", "Anchor", "Session Close", "Attention"], value_size=18)
    panels.append(health); y += 3
    side = {"mappings": [NO_FIGURE] + SIDE_COLORS + regex_colors([("Flat", GREY)])}
    panels.append(stat_strip("Position · ct and USDT", None, [
        ("Side", "Flat, or the open round trip's side: Long (both legs bought, blue), Short (both sold, orange).", side),
        ("Bull", "Contracts held on the leg as the program counts them, signed (+ long, − short); – when flat.",
         {"unit": "none", "decimals": 2, "displayName": "${bull}", "mappings": [NO_FIGURE]}),
        ("Bear", "", {"unit": "none", "decimals": 2, "displayName": "${bear}", "mappings": [NO_FIGURE]}),
        ("Cost", "USDT: what the held contracts cost at entry, both legs (the notional held).", {"unit": "locale", "decimals": 0, "mappings": [NO_FIGURE]}),
        ("Held", "Time since the round trip opened.", None),
        ("To Exit", "σ still to go to the exit line (a Long: Sell Both up to −d_out σ; a Short: Buy Both down to +d_out σ); 0 or less: "
                    "at the line, the exit is being sent · –: a leg's depth is unusable (the program sends no exit then; a book "
                    "conflict does not stop an exit).", {"unit": "suffix:σ", "decimals": 2, "mappings": [NO_FIGURE], "displayName": "To" + NBSP + "Exit"}),
    ], 0, y, 10, 3, source=health, columns=["Side", "Bull", "Bear", "Cost", "Held", "To Exit"], value_size=16, title_size=12))
    net = panel("stat", "Net", Q_NET, 10, y, 4, 3, description=(
        "USDT, realised: the net of the Run's round trips closed in the Range (P&L names it): Gross + Fees + Funding, round trips whose "
        "funding is not known yet left out until it is · the line: its running sum, a step at each close."), unit="none", decimals=2,
        thresholds=SIGNED, color={"mode": "thresholds"},
        options={"reduceOptions": {"calcs": ["lastNotNull"], "fields": "/^Net$/", "values": False}, "colorMode": "value", "graphMode": "area",
                 "textMode": "value", "justifyMode": "center", "wideLayout": True, "text": {"valueSize": 34}, "showPercentChange": False,
                 "orientation": "auto"})
    panels.append(net)
    panels.append(stat_strip("P&L · ${span} · USDT", Q_PNL, [
        ("Open P&L", "Unrealised: the open round trip at the newest second's mids against its entry cost, after the entry fees of what is "
                     "held, before exit fees and the half spread an exit pays (the band's distance to the mid); – when flat; not in Net.", dict(MONEY, displayName="Open" + NBSP + "P&L")),
        ("Fees", "Trading fees of the closed round trips, as a cost (negative).", MONEY),
        ("Funding", "Funding booked to them; + received.", MONEY),
        ("Trips", "Round trips closed in the Range (those with funding still awaited included; no-fill ones left out).", {"unit": "locale", "decimals": 0}),
        ("Won", "Closed round trips with a positive net.", {"unit": "percent", "decimals": 0, "mappings": [NO_FIGURE]}),
    ], 14, y, 10, 3, intro="The Run's round trips closed in the Range (title) · Net = Gross + Fees + Funding (left).", value_size=18,
        title_size=12)); y += 3

    # ------------------------------------------------------------------ the market now: the Run's last second
    book = regex_colors([("Conflict", WARN), ("Unusable", WARN), ("OK", GREY)])
    legs = table("Legs · Mid · Anchor USD / USDT", Q_LEGS, 0, y, 10, 4, description=(
        "Each leg at the Run's newest second: Mid (USDT, the program's book), the Anchor the spread is measured against in USD "
        "(per share: the issuer's NAV, or the bell's F × Binance's index; column USD) and in USDT (USD ÷ F; column USDT), to 0.001, and the Book as the executor judges "
        "it: OK, Conflict (amber: the book snapshot and the tick stream disagree) or Unusable (amber: too old or too thin): no entry "
        "goes out while either leg's is not OK · the program records no bid / ask, pricing-level price or depth per second (only the "
        "mids and the executable deviations), so the board has none."), overrides=[
        col("Leg", 52), col("Mid", None, custom_align="right", unit="none", decimals=3),
        col("USD", None, custom_align="right", unit="none", decimals=3), col("USDT", None, custom_align="right", unit="none", decimals=3),
        col("Book", 76, custom_cellOptions={"type": "color-text"}, mappings=book)])
    legs["fieldConfig"]["defaults"]["custom"]["wrapHeaderText"] = False   # one header line: both legs fit 4 units at 880 px
    panels.append(legs)
    sig = {"unit": "suffix:σ", "decimals": 2, "mappings": [NO_FIGURE]}
    panels.append(stat_strip("Spread · D and σ · Last Second", None, [
        ("Spread", "D, on both legs' mids: 10⁴ × (F × (P_bull ÷ A_bull + P_bear ÷ A_bear) − 2); 1 D = 0.5 bps of the two legs' notional.",
         {"unit": "suffix: D", "decimals": 1, "mappings": [NO_FIGURE]}),
        ("Mean", "D: the program's mean of the spread (the Run's mean model).", {"unit": "suffix: D", "decimals": 1, "mappings": [NO_FIGURE]}),
        ("σ", "D: its standard deviation (the Run's σ window).", {"unit": "suffix: D", "decimals": 1, "mappings": [NO_FIGURE]}),
        ("Deviation", "(Spread − Mean) ÷ σ, on the mids; the executable sides are in Entry below.", sig),
        ("F", "USD per USDT the program converts the anchors with.", {"unit": "none", "decimals": 5, "mappings": [NO_FIGURE]}),
        ("As Of", "The second these figures are of (UTC+8).", None),
    ], 10, y, 14, 4, source=health, columns=["Spread", "Mean", "σ", "Deviation", "F", "As Of"], value_size=15, title_size=12)); y += 4

    entry_state = {"mappings": [NO_FIGURE] + regex_colors(list(STATE_COLORS.items()))}
    panels.append(stat_strip("Entry · σ and D", None, [
        ("Entry", "Can it open now: Ready (green, no signal yet) · Order Sent (green) · Blocked: the strategy's ordinary answer · Held Back "
                  "(amber): waiting on the book, F, the venue or a stop · Needs a Person (red).", entry_state),
        ("Why", "The reason in words (the program's idle reason this second), and after it a leg whose book keeps an entry from going out "
                "when that is not the reason itself (Regular Hours · SOXS Book Conflict).", None),
        ("Buy Both", "Now: the deviation buying both legs at their pricing levels, in σ (the band's top on the chart): a Long enters at "
                     "−d_in σ · shown even while a book is not OK, for diagnosis.", sig),
        ("Sell Both", "Selling both, in σ (the band's bottom): a Short enters at +d_in σ.", sig),
        ("To Long Entry", "How far Buy Both still has to fall to the Long entry line (−d_in σ), in σ and in D; At the Line once there · —: "
         "a leg's book conflicts or its depth is unusable, so the program enters on none (Why names the leg).", None),
        ("To Short Entry", "How far Sell Both still has to rise to the Short entry line (+d_in σ) · — as for To Long Entry.", None),
    ], 0, y, 24, 3, source=health, columns=["Entry", "Why", "Buy Both", "Sell Both", "To Long Entry", "To Short Entry"], value_size=16)); y += 3
    # ------------------------------------------------------------------ Live Execution
    panels.append(row("Live Execution · ${lines}", y)); y += 1
    action = code_texts(ACTIONS, ACTION_COLORS)
    no_value = code_texts({**NO_VALUE, ZERO: "0.00"}, {"0.00": TEXT})
    no_value1 = code_texts({**NO_VALUE, ZERO: "0.0"}, {"0.0": TEXT})
    entry_text = code_texts(IDLE_CODES, {t: STATE_COLORS[t.split(" · ")[0]] for t in IDLE_CODES.values()})
    qty_text = code_texts({ZERO: "0"}, {"0": TEXT})
    price_text = code_texts({NO_FILL: "No Fill"})
    money_text = code_texts({FEE_PENDING: "Pending", PENDING: "Pending", OPEN: "Open", ZERO: "0.00"}, {"Pending": WARN, "Open": TEXT, "0.00": TEXT})

    def dot(name, color, size, legend):
        return named(name, {"color": {"mode": "fixed", "fixedColor": color}, "custom.drawStyle": "points", "custom.pointSize": size,
                            "custom.hideFrom": {"legend": not legend, "tooltip": True, "viz": False}})

    def line(name, legend=True):
        _, color, dash = LINES[name]
        return series(name, color, width=1.5, dash=dash, legend=legend, tooltip=False)
    marker_tooltip = [tooltip_only("Action", TEXT, action), tooltip_only("Decided", TEXT, props={"unit": "time:HH:mm:ss.SSS"}),
                      tooltip_only("Bull Filled", TEXT, qty_text, {"unit": "none", "decimals": 2}, shown="${bull} ct"),
                      tooltip_only("Bull Avg", TEXT, price_text, {"unit": "none", "decimals": TICK_DECIMALS["bull"]}, shown="${bull} Avg Price"),
                      tooltip_only("Bear Filled", TEXT, qty_text, {"unit": "none", "decimals": 2}, shown="${bear} ct"),
                      tooltip_only("Bear Avg", TEXT, price_text, {"unit": "none", "decimals": TICK_DECIMALS["bear"]}, shown="${bear} Avg Price"),
                      tooltip_only("Notional", TEXT, qty_text, {"unit": "locale", "decimals": 0}, shown="Notional USDT"),
                      tooltip_only("Fees", TEXT, money_text, {"unit": "none", "decimals": 2}, shown="Fees USDT"),
                      tooltip_only("Cycle Net", TEXT, money_text, {"unit": "none", "decimals": 2}, shown="Round Trip Net USDT"),
                      tooltip_only("At", PURPLE, code_texts({ZERO: "0.00", NO_SAMPLE: "No Sample"}, {"0.00": TEXT}), {"decimals": 2},
                                   shown="At σ (its x, or the mid's)")]
    deviation_lines = ([series("Buy Both", LIGHT_GREY, width=0, fill_to="Sell Both", fill=22, tooltip=False),
                        series("Sell Both", LIGHT_GREY, width=0, legend=False, tooltip=False),
                        series("Deviation", PURPLE, width=1.5, tooltip=False),
                        line("Long In"), line("Long Out", legend=False), line("Short In"), line("Short Out", legend=False),
                        tooltip_only("Deviation · Tooltip", PURPLE, no_value, {"decimals": 2}, shown="Deviation σ"),
                        tooltip_only("Buy Both · Tooltip", BLUE, no_value, {"decimals": 2}, shown="Buy Both σ"),
                        tooltip_only("Sell Both · Tooltip", ORANGE, no_value, {"decimals": 2}, shown="Sell Both σ"),
                        tooltip_only("Spread · Tooltip", LIGHT_GREY, no_value1, {"decimals": 1}, shown="Spread D"),
                        tooltip_only("Sigma · Tooltip", LIGHT_GREY, no_value1, {"decimals": 1}, shown="σ D"),
                        tooltip_only("Entry", TEXT, entry_text)] +
                       [dot(n, c, s, legend) for n, _, c, s, legend in DOTS] + marker_tooltip)
    for o in deviation_lines:   # the band's swatch in the legend, the lines' names with their side
        name = o["matcher"]["options"]
        if name == "Buy Both":
            o["properties"].append({"id": "displayName", "value": "Executable Band"})
        if name in ("Long In", "Short In"):
            o["properties"].append({"id": "displayName", "value": name.split()[0] + " Lines"})
    dev_tips = ["Deviation · Tooltip", "Buy Both · Tooltip", "Sell Both · Tooltip", "Spread · Tooltip", "Sigma · Tooltip", "Entry"]
    dot_names = [n for n, _, _, _, _ in DOTS]
    info_names = [n for n, _ in MARKER_INFO]
    # The tooltip lists a decision first (its rows are hidden away from one), then the point's values.
    dev_order = ["time", "Buy Both", "Sell Both", "Deviation"] + list(LINES) + dot_names + info_names + dev_tips
    deviation = timeseries("Deviation from the Mean · σ", Q_EXECUTION, 0, y, 24, 12, deviation_lines, decimals=0, zero_line=True, calcs=(),
                           soft_min=-3, description=(
        "Deviation = (Spread − Mean) ÷ σ · Executable Band: Buy Both (buying both legs at their pricing levels, top) to Sell Both "
        "(selling both, bottom), in σ · a Long enters when the whole band is under the blue dashed line (Buy Both ≤ −d_in σ) and exits "
        "when it is over the blue dotted one (Sell Both ≥ −d_out σ); a Short enters over the orange dashed line (Sell Both ≥ +d_in σ) "
        "and exits under the orange dotted one (Buy Both ≤ +d_out σ) · each sample on the lines of the run that wrote it\n\n"
        "Dots at each decision: Long Entry (blue, large), Short Entry (orange, large), the exit in the round trip's colour (smaller, not "
        "in the legend), Forced Exit (red), Other (grey: a trim, a completion, or a void entry that filled one leg or none) · shaded: each round trip "
        "from entry to exit (blue Long, orange Short; its mark under the chart: times, end reason, net) · purple marks under the "
        "chart: a change of the anchors in use, with reason and both legs' anchors before → after in USD and USDT (owner item 62) · "
        "hover a dot: the action, both legs' contracts and average prices, notional, fees and the round trip's net · points: every "
        "second up to 25 min, then 5, 10, 30 s and whole minutes (a band edge is its bucket's extreme, of the seconds whose books the program enters on: none, and tooltip Book Not OK, "
        "where a leg's book conflicts or its depth is unusable) · a gap: no sample (process "
        "down), or a new session or run · the axis stops at ±%g σ: a spike beyond it is drawn to the edge, the tooltip gives its value." % AXIS_SIGMA))
    deviation["options"]["tooltip"]["hideZeros"] = True
    deviation["options"]["legend"].update(displayMode="list", placement="bottom")
    deviation["options"]["legend"].pop("width", None)
    deviation["fieldConfig"]["defaults"]["custom"]["axisSoftMax"] = 3
    deviation["fieldConfig"]["defaults"].update(min=-AXIS_SIGMA, max=AXIS_SIGMA)
    deviation["fieldConfig"]["defaults"]["noValue"] = "No second of the Run on this axis: Charts to the Range (top) moves it to the Run"
    deviation["transformations"] = [keep_fields(dev_order), organize(dev_order)]
    panels.append(deviation); y += 12
    spread_lines = [series("Spread", PURPLE, width=1.5, tooltip=False), series("Mean", TEXT, width=1, tooltip=False),
                    tooltip_only("Spread · Tooltip", PURPLE, no_value1, {"decimals": 1}, shown="Spread​"),
                    tooltip_only("Mean · Tooltip", TEXT, no_value1, {"decimals": 1}, shown="Mean​"),
                    tooltip_only("Sigma · Tooltip", LIGHT_GREY, no_value1, {"decimals": 1}, shown="σ")]
    spread_order = ["time", "Spread", "Mean", "Spread · Tooltip", "Mean · Tooltip", "Sigma · Tooltip"]
    spread = timeseries("Spread · D", None, 0, y, 24, 6, spread_lines, decimals=0, calcs=(), source=deviation, description=(
        "Spread = 10⁴ × (F × (P_bull ÷ A_bull + P_bear ÷ A_bear) − 2) on both legs' mids · 1 D = 0.5 bps of the two legs' total "
        "notional\n\nThe level behind the deviation chart, on the mids: the spread and its Mean (the program trades on the executable "
        "band above, not on these) · the program's own per-second samples · shading and anchor marks as above."))
    spread["options"]["legend"].update(displayMode="list", placement="bottom")
    spread["options"]["legend"].pop("width", None)
    spread["fieldConfig"]["defaults"]["noValue"] = "No second of the Run on this axis: Charts to the Range (top) moves it to the Run"
    spread["transformations"] = [keep_fields(spread_order), organize(spread_order)]
    panels.append(spread); y += 6

    # ------------------------------------------------------------------ Round Trips
    panels.append(row("Round Trips · ${span}", y)); y += 1
    link = (URL + "?orgId=1&${__url_time_range}&${run:queryparam}&${since:queryparam}&${since_hour:queryparam}&${until:queryparam}"
            "&${until_hour:queryparam}&${combo:queryparam}&var-cycle=${__data.fields.cycle_id}")
    money_col = dict(custom_align="right", unit="none", decimals=2, custom_cellOptions={"type": "color-text"}, color={"mode": "thresholds"},
                     thresholds=SIGNED, mappings=[NO_FIGURE, PENDING_TEXT])
    sigma_col = dict(custom_align="right", unit="none", decimals=2, mappings=[NO_FIGURE])
    panels.append(table("Round Trips · Newest First", Q_BLOTTER, 0, y, 24, 9, description=(
        "Net = Gross + Fees + Funding · Net bps = Net ÷ Notional × 10⁴ (bps of the notional, not D) · a click on Opened shows its orders "
        "below\n\nEvery round trip of the Run open at any time in the Range, newest first, at most 500; no-fill ones (nothing traded) left out · "
        "Held: open to close (to now while open) · Notional: both legs' entry value, USDT (a One Leg round trip: that leg's) · In σ: "
        "the executable deviation at its entries, weighted by their value · Out σ: at the exit decision (– for a forced exit or none) · Net: Open while open, Pending (amber) "
        "while a funding settlement it held through has no record."), overrides=[
        col("cycle_id", custom_hidden=True),
        col("Opened", 120, unit="time:MM-DD HH:mm:ss", links=[{"title": "Show this round trip's orders", "url": link, "targetBlank": False}]),
        col("Closed", 72, unit="time:HH:mm:ss"), col("Held", 64),
        col("Side", 52, custom_cellOptions={"type": "color-text"}, mappings=SIDE_COLORS),
        col("Notional", 72, custom_align="right", unit="locale", decimals=0), col("In σ", 56, **sigma_col), col("Out σ", 56, **sigma_col),
        col("Net", 64, **money_col), col("Net bps", 64, **dict(money_col, decimals=1)), col("End Reason", custom_minWidth=120)])); y += 9

    # ------------------------------------------------------------------ Round Trip (detail)
    panels.append(row("Round Trip · ${cycle:text}", y)); y += 1

    def num(d):
        return dict(custom_align="right", unit="none", decimals=d, mappings=[NO_FIGURE])
    panels.append(table("Orders · ct, USDT and ms", Q_ORDERS, 0, y, 24, 9, description=(
        "Every order of the picked round trip, in the order the program sent them; its fills folded in: Filled (contracts), Avg (their "
        "average price), Fee (USDT, a cost, negative; Pending while the venue has not reported it)\n\n"
        "Purpose: Entry, Complete (the leg that filled less), Trim (the leg that filled more), Exit, Forced · Side: RO a reduce-only "
        "order · Limit and Avg to the leg's tick · Latency: sent → final state in hand, ms, this host's clock (paper: the configured "
        "simulation) · Status: Filled, Partial, No Fill (an IOC that met nothing), Rejected (with the venue's code), Working, Unknown "
        "(being queried)."), overrides=[
        col("Time", 92, unit="time:HH:mm:ss.SSS"), col("Purpose", 80), col("Leg", 52), col("Side", 64), col("Qty", 60, **num(2)),
        col("Filled", 60, **num(2)), col("Limit", 64, custom_align="right"), col("Avg", 64, custom_align="right"),
        col("Fee", 56, custom_align="right", unit="none", decimals=2, mappings=[NO_FIGURE, PENDING_TEXT]), col("Latency", 68, **num(0)),
        col("Status", custom_minWidth=96, custom_cellOptions={"type": "color-text"},
            mappings=regex_colors([("Rejected", BAD), ("Unknown", WARN), ("Partial", WARN), ("No Fill", GREY)]))])); y += 9

    # ------------------------------------------------------------------ Risk & Account
    panels.append(row("Risk & Account · ${runtext}", y)); y += 1
    bps = {"unit": "none", "decimals": 2, "mappings": [NO_FIGURE]}   # the unit in the strip's title
    risk = stat_strip("Exposure · USDT", Q_RISK, [
        ("Bull Exposure", "The venue's position at the newest second's mid, USDT, signed (paper: the paper venue's).",
         dict(EXPOSURE, displayName="${bull} Exposure")),
        ("Bear Exposure", "", dict(EXPOSURE, displayName="${bear} Exposure")),
        ("Quota Used", "Held at entry prices, plus open orders at their limits, plus dust, as a share of the quota.",
         {"unit": "percent", "decimals": 1, "mappings": [NO_FIGURE]}),
        ("Quota Total", "L × (M + min(0, R)): the notional the program may hold.", {"unit": "locale", "decimals": 0, "mappings": [NO_FIGURE]}),
    ], 0, y, 12, 3, intro="The Run's newest inventory sample, whatever the Range.", columns=["Bull Exposure", "Bear Exposure", "Quota Used",
                                                                                              "Quota Total"], value_size=18, title_size=12)
    panels.append(risk)
    panels.append(stat_strip("Account · Funding Rate bps", None, [
        ("Equity", "Bitget usdtEquity of the WHOLE account (it also holds the sibling repository's positions); live only.",
         {"unit": "locale", "decimals": 2, "mappings": [NO_FIGURE]}),
        ("Margin Ratio", "Bitget mgnRatio of the whole account: maintenance margin ÷ equity (a fraction, shown in %; 13.01 ÷ 5,579.30 "
                         "= 0.23 % on 2026-11-26 12:16 UTC+8); live only; the program takes no decision from it (PRD 6.6).",
         {"unit": "percent", "decimals": 2, "mappings": [NO_FIGURE]}),
        ("Bull Funding", "The newest settled funding rate × 10⁴ (bps of the position's value per settlement, not D), as Bitget publishes it: "
                         "+ longs pay shorts (a Long round trip pays, a Short receives; P&L's Funding is signed + received).",
         dict(bps, displayName="${bull} Rate")),
        ("Bear Funding", "", dict(bps, displayName="${bear} Rate")),
        ("Funding Awaited", "Settlements finished round trips still wait for (amber): their Net stays Pending until the record arrives.",
         {"unit": "locale", "decimals": 0, "thresholds": {"mode": "absolute", "steps": [{"color": "text", "value": None}, {"color": WARN, "value": 1}]}}),
    ], 12, y, 12, 3, intro="The Run's newest inventory sample (Equity, Margin Ratio: USDT and %) and its mode's newest funding rate "
                           "(bps), whatever the Range.", source=risk, columns=["Equity", "Margin Ratio", "Bull Funding", "Bear Funding",
                                                                               "Funding Awaited"], value_size=18, title_size=12)); y += 3

    # ------------------------------------------------------------------ Settings
    panels.append(row("Settings · ${runtext}", y)); y += 1
    about = {"Signal": "What the program trades on: the mean model and σ window of the spread (m: minutes), the entry → exit lines in σ "
                       "(a Long enters with Buy Both at the first and exits with Sell Both at the second; a Short the mirror), the taker "
                       "fee it counts, the margin over costs an entry needs (D), whether the mean warms up again when the anchor changes.",
             "Sizing · USDT": "How much: capital M and leverage L give the notional N = M × L the program may hold (USDT); the smallest "
                              "batch (USDT); take ratio: the share of the depth up to the pricing level an order takes; the time and the "
                              "D between entries of a cycle.",
             "Execution": "How it trades: Live (real orders on Bitget) or Paper; the book level each leg's limit sits at ($bull / $bear); "
                          "the order channel (WS or REST) and the book feed × its connections; how long it waits to complete the other "
                          "leg; the slip a forced exit may pay; the order latency paper simulates ($bull / $bear; a paper Run only).",
             "Risk & Session": "When it stops and what it trusts: how long before the regular close it flattens; how old a book or the "
                               "USD/USDT rate F may be; how long a NAV must stay unchanged to be confirmed; how long a stop may take "
                               "before an alarm; the ex-dates of the dividends (and splits) it knows; edits to the market calendar.",
             "Run": "Which start this is (its number, mode and pair are in the banner and the pickers): start and stop (UTC+8), "
                    "host and run_id."}
    x = 0
    for group, _ in SETTINGS:
        w = SETTINGS_WIDTHS[group]
        if x + w > 24:
            x, y = 0, y + 8
        t = table(group, q_settings(group), x, y, w, 8, description=about[group] + " · process_run.config of the Run.", overrides=[
            col("Setting", 140, custom_cellOptions={"type": "color-text"}, color={"mode": "fixed", "fixedColor": GREY}),
            col("Value", None, custom_align="right")])
        t["options"]["showHeader"] = False
        panels.append(t)
        x += w
    y += 8

    # ------------------------------------------------------------------ Diagnostics (collapsed)
    sy = y + 1
    execution = [
        table("Entry Answers · Share of Seconds", Q_IDLE, 0, sy, 9, 8, description=(
            "Each second's answer in the Range (signal_sample.idle_reason) in words, its seconds and share."),
            overrides=[col("Answer", 220), col("Seconds", 80, custom_align="right", unit="locale", decimals=0),
                       col("Share", None, unit="percent", decimals=1, custom_cellOptions={"type": "gauge", "mode": "basic", "valueDisplayMode": "text"},
                           color={"mode": "fixed", "fixedColor": LIGHT_GREY}, min=0, max=100)]),
        table("At the Line, Not Entered · Seconds", Q_BLOCKED, 9, sy, 8, 8, description=(
            "Seconds in the Range the executable deviation was past an entry line but no entry went out (decision at_line_blocked), "
            "by reason and side."),
            overrides=[col("Reason", 150), col("Side", 64, custom_cellOptions={"type": "color-text"}, mappings=SIDE_COLORS),
                       col("Seconds", 72, custom_align="right", unit="locale", decimals=0), col("Last", None, unit="time:MM-DD HH:mm:ss")]),
        table("End Reasons", Q_END_REASONS, 17, sy, 7, 8, description=(
            "Why the Run's round trips of the Range that closed ended (no-fill ones included), how many, and their net (Pending ones counted, "
            "out of Net)."), overrides=[col("End Reason", None), col("Cycles", 64, custom_align="right"), col("Net", 72, **money_col),
                                        col("Pending", 72, custom_align="right")]),
        table("Order Latency by Stage · ms", Q_LATENCY, 0, sy + 8, 24, 6, description=(
            "Median, 90th percentile and largest of each stage over the Run's orders submitted in the Range · local: this host's clock; venue: "
            "Bitget's · paper: the configured simulated latency, not a measurement."), overrides=[
            col("Stage", 180), col("Clock", 72), col("Orders", 80, custom_align="right"), col("Median", 96, **num(1)), col("P90", 96, **num(1)),
            col("Max", None, **num(0))]),
    ]
    panels.append(collapsed_row("Diagnostics · Execution · ${span}", y, execution)); y += 1
    sy = y + 1
    system = [
        table("Alerts · Warnings and Errors", Q_ALERTS, 0, sy, 24, 4, description=(
            "Every warning and error a module of the Run recorded (trade.event) in the Range, newest first, at most 200 · Level: "
            "Warn (handled or retried), Error (a person is needed)."), overrides=[
            col("Time", 136, unit="time:MM-DD HH:mm:ss"),
            col("Level", 64, custom_cellOptions={"type": "color-text"}, mappings=regex_colors([("Error", BAD), ("Warn", WARN)])),
            col("Module", 80), col("Kind", 160), col("Message", custom_minWidth=240, custom_wrapText=True), col("Detail", custom_minWidth=160)]),
        table("Runs · Both Modes", Q_RUNS, 0, sy + 4, 24, 6, description=(
            "Every start of the program, both modes, newest first, as the Run picker lists them (the one shown marked) · State: Running "
            "(a sample in the last minute), Stopped (in order), Lost (neither: crashed or killed) · Ran: start to stop, or to now · N: the "
            "notional M × L it may hold, USDT · a click on Run shows that Run, its whole life on the charts · its settings: Settings "
            "above, for the picked Run."), overrides=[
            col("Run", 190, links=[{"title": "Show this Run, its whole life on the charts", "targetBlank": False, "url": URL + (
                "?orgId=1&from=${__data.fields.from_ms}&to=${__data.fields.to_ms}&refresh=10s&var-run=${__data.fields[\"Run ID\"]}"
                "&var-since=0&var-since_hour=00&var-until=%s&var-until_hour=24" % NO_END_MS)}]),
            col("from_ms", custom_hidden=True), col("to_ms", custom_hidden=True), col("Started", 132, unit="time:MM-DD HH:mm:ss"), col("Stopped", 132, unit="time:MM-DD HH:mm:ss"),
            col("State", 80, custom_cellOptions={"type": "color-text"}, mappings=regex_colors([("Running", GOOD), ("Lost", BAD), ("Stopped", GREY)])),
            col("Ran", 84), col("N", 72, custom_align="right", unit="locale", decimals=0), col("Host", 140), col("Run ID", custom_minWidth=200)]),
        table("Anchors Read · Sessions in the Range", Q_ANCHORS, 0, sy + 10, 14, 7, description=(
            "Every anchor, NAV and dividend the program read in the Run's mode for the sessions of the Run in the Range, newest first: bell_f (the F inside the bell), "
            "bell (F × Binance's index at the close), nav_seen (a NAV first seen), nav (confirmed after 1 h unchanged), dividend · Value: USD "
            "per share · its own reading, apart from Combo Spread's Anchors table."), overrides=[
            col("Session", 96), col("Kind", 72), col("Leg", 56), col("Known", 120, unit="time:MM-DD HH:mm:ss"), col("Value", 88, **num(4)),
            col("F", 80, **num(6)), col("Source", 72), col("Detail", custom_minWidth=160)]),
        table("Funding Records · USDT", Q_FUNDING, 14, sy + 10, 10, 7, description=(
            "Each funding record of the Run's two contracts settled in the Range, booked to the round trip whose interval holds the settlement, "
            "and each settlement a finished round trip still waits for (Awaited, amber) · Amount: + received."), overrides=[
            col("Settlement", 112, unit="time:MM-DD HH:mm"), col("Contract", 88),
            col("Status", 80, custom_cellOptions={"type": "color-text"}, mappings=regex_colors([("Awaited", WARN)])),
            col("Amount", 80, custom_align="right", unit="none", decimals=4, custom_cellOptions={"type": "color-text"}, color={"mode": "thresholds"},
                thresholds=SIGNED, mappings=[NO_FIGURE]), col("Cycle", None)]),
    ]
    panels.append(collapsed_row("Diagnostics · System · ${span}", y, system)); y += 1

    spans = [deviation["id"], spread["id"]]
    annotations = [bands("Long Round Trips", cycle_spans("long"), BLUE, spans), bands("Short Round Trips", cycle_spans("short"), ORANGE, spans),
                   marks("Anchor Changes", Q_ANCHOR_CHANGES, PURPLE, [p["id"] for p in panels if p["type"] == "timeseries"])]
    board = {"panels": panels, "templating": {"list": templating}, "annotations": {"list": annotations},
             "links": [{"title": "Charts to the Range", "type": "link", "icon": "dashboard", "targetBlank": False, "keepTime": False,
                        "includeVars": False, "tooltip": "The time range of the charts set to the Range (Run, Since, Until): the whole Run by default.",
                        "url": URL + "?orgId=1&from=${range_from}&to=${range_to}&refresh=10s&${run:queryparam}&${since:queryparam}"
                               "&${since_hour:queryparam}&${until:queryparam}&${until_hour:queryparam}&${combo:queryparam}"},
                       {"title": "Reset", "type": "link", "icon": "bolt", "targetBlank": False, "keepTime": False, "includeVars": False,
                        "tooltip": "Newest Live Run, Run start to Run end, the last 3 hours, refresh 10 s.",
                        "url": URL + "?orgId=1&from=now-3h&to=now&refresh=10s&var-run=__live&var-since=0&var-since_hour=00&var-until=%s"
                               "&var-until_hour=24" % NO_END_MS}],
             "time": {"from": "now-3h", "to": "now"}, "refresh": "10s", "timepicker": {"refresh_intervals": ["10s", "30s", "1m", "5m"]}, "tags": [],
             "description": "The ETF-pair trading program as a trader reads it, one Run at a time: health, position, P&L, the market and "
                            "entry now, the deviation with every entry and exit, the Run's settings, round trips and their orders, risk; "
                            "diagnostics folded; paper and live apart."}
    return board
