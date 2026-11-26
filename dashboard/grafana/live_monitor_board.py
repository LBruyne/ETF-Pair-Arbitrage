"""ETF-Pair Live Monitor: the trading program (core/) as it trades, from its own records in schema trade of the etfpair
database (core/store/schema.sql, core/SPEC.md 5.6), read by the read-only role etfpair_trade_reader through the data source
etfpair-trade (dashboard/SPEC.md section 7).

Version 4.4 (core/SPEC.md 12, step 10.9: the thin leg first): the lines read strategy.entry_in / entry_out (d_in / d_out of
the Runs before) and exit2; Parameters adds Exit 2 and a Thin Leg First strip (thin leg, pair window, pair stop, top-up max,
exit pair window); a batch's marker reads its end state (built and built partial an entry, entry failed the grey triangle,
unwound at a profit or at the stop an amber one) and its pairing; an exit's its stage, pre-check, how its thick leg closed and
the expected against the actual net; an entry's step counts every order of its lot, an exit's the thick leg's exit_pair
orders; Round Trips counts the batches, Orders names each order's step.

Version 4.1 (owner items 76-78): a Run is named start–end · Bitget · pair · mode, without a number; Status holds Execution
(no Close; the Phase in Combo Spread's words and colours), Account (4.0's Margin: position or residual, ledger, equity,
maintenance margin, margin ratio, liquidation, funding) and Market (two strips after Combo Spread's latest second, with the
Anchor); the Summary reads PnL, Gross, Fee, Funding, Open PnL, Notional, Peak Used, Win Rate, Return, with cumulative Gross
and PnL under it; an empty table says why in one row.

Version 4 (owner items 73-75) takes the sibling dex-cex Live Monitor's design: its grouping into rows named "Section ·
scope", in its order (Status · Running Session with Execution, Margin and Alerts first; Execution Summary; the position and
market at the last second; the charts; Round Trips and their detail; Inventory; Settings; Execution Statistics and System
folded), its status colours and words, column widths, Title Case titles "Noun · Unit" and bullet descriptions. The Mode
word (LIVE / PAPER) in Execution and the row titles (Run 07 · Live) replace 3.0's banner.

Version 3 (owner item 71) picks, in this order, a Run (one start of the program, numbered and named like the sibling
dex-cex board's Runs: Run 07 · Live · 11-26 11:36 – now · running), Since and its hour, Until and its hour (the Range: the
figures cover the Run's records from Since to Until) and the Combo (the Run's pair). A Run is one mode, paper or live, so
paper and live are never merged: every query keeps the picked Run's rows (SEL: run_id and live of the Run) and a slim banner
at the top said which mode and Run the page shows (the Mode word since 4.0). It shows the Run's effective settings
(process_run.config) and its latest second (the real-time figures).

Version 2 (owner item 68) laid the page out to a trader's questions, in this order: is it running and healthy (Health),
what do I hold (Position), how am I doing (Net, P&L), what is the market now (Legs, Spread), can it enter now and why not
(Entry); then the Live Execution chart of the deviation in sigma with the program's lines and a marker at every entry, exit
and adjustment (owner item 61), the spread behind it, the Run's settings, the round trips (one row each, a click opens
its orders), risk and account in one strip, and the diagnostics folded at the bottom. Every time-series chart marks each
change of the anchors in use (owner item 62).

Owner item 80 (4.2): every spread figure is in bps as Combo Spread writes it (the program's spread unit, 10⁴ × the two legs'
deviations added; 1 bps of spread is 0.5 bps of the two legs' total notional), no D; money is USDT. The program's lines are those
of the Run (process_run.config: strategy.entry_in, strategy.entry_out; d_in, d_out before PRD 4.0): a Long enters at x <=
-entry_in sigma on the buying side (Buy Both) and exits at x >= -entry_out sigma on the selling side (Sell Both); a Short
enters at x >= entry_in sigma on Sell Both and exits at x <= entry_out sigma on Buy Both (core/SPEC.md 5.2); exit2 the same. Every picked setting reaches SQL inside quotes ('$run', '$combo',
'${since}', '$cycle', ...) and is parsed there; the reader role is the boundary (dashboard/SPEC.md section 6).

Grafana's tooltip shows a nearby point's value in place of a NULL, so on the execution charts every tooltip row has a
value at every point: a reason code where there is none (NO_VALUE), and a 0 on the rows without a decision for the marker
rows, which the tooltip hides (hideZeros); a real zero there is a code (ZERO) shown as 0."""
import re

from boardkit import (BAD, BLUE, GOOD, GREY, LIGHT_GREY, NO_FIGURE, ORANGE, PURPLE, SIGNED, TEXT, TRADE, WARN, bands, board_slug, board_uid,
                      code_texts, col, keep_fields, marks, named, organize, panel, query_var, reset_ids, row, series, sql, stat_strip,
                      table, target, timeseries, tooltip_only)

NAME = "live-monitor"   # boardkit.BOARDS: uid arb-etf-pair-live-monitor, address /dashboards/arbitrage/etf-pair/live-monitor
URL = "/d/%s/%s" % (board_uid(NAME), board_slug(NAME))
NOW_MS = "(extract(epoch FROM now()) * 1000)::bigint"
NBSP = " "
NBHY = "‑"   # a non-breaking hyphen: a browser may break a line after a plain one
UTC8 = "'Etc/GMT-8'"
# The price precision of the two contracts (Bitget SOXLUSDT 0.01, SOXSUSDT 0.001): prices are shown to their tick.
TICK_DECIMALS = {"bull": 2, "bear": 3}
SAMPLE_AGE_S = 10   # a running process writes a sample every second; past this it is Stale
# The sibling dex-cex board's status colours (its SPEC 15.5): a part that is well on a muted green, so amber and red stand
# out; serious between amber and red.
WELL, SERIOUS = "#1f5c34", "#ec835a"


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
        ("position_mismatch", "Needs a Person", "Position Mismatch"),
        # 4.4, the thin leg first (core/strategy/types.go, SPEC 12.4-12.7): Pairing (amber) while a batch holds a leg not
        # paired yet or an exit's thick leg is still open; the two-stage exit's ordinary answers Blocked.
        ("pair_bound", "Pairing", "Thick Leg Beyond Its Bound"), ("window", "Pairing", "Pair Window Over"),
        ("stop_d", "Pairing", "Ideal Spread Stop"), ("profit", "Pairing", "Thin Leg Profit Close"),
        ("exit_bound", "Pairing", "Exit: Thick Leg Beyond Break-Even"), ("d1_loss", "Pairing", "Exit Below Break-Even"),
        ("exit_window", "Pairing", "Exit: Thick Leg Hard Close"), ("exit_stop_d", "Pairing", "Exit: Thick Leg Hard Close"),
        ("valuation", "Held Back", "Close Not Valued"), ("forced", "Held Back", "Forced Exit"),
        ("no_profit", "Blocked", "Exit 1: Round Not in Profit"), ("precheck", "Blocked", "Exit 1: Batch Short of Break-Even"),
        ("half_done", "Blocked", "Half Closed, Rest at Exit 2")]
STATE_COLORS = {"Ready": GOOD, "Order Sent": GOOD, "Blocked": TEXT, "Pairing": WARN, "Held Back": WARN, "Needs a Person": BAD}
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
NOT_IN = -9600000000     # a marker's leg that its step sent no order on (a one-leg trim, completion or exit of the remaining leg)


def nonzero(x, decimals):
    """A tooltip row's value as shown(), a real zero as ZERO (the tooltip hides a 0)."""
    return "CASE WHEN %s = 0 THEN %d ELSE %s END" % (shown(x, decimals), ZERO, shown(x, decimals))


def coded(value, decimals, why):
    """value shown() (a zero as ZERO), else the code of the first reason in why ((condition, text), ...) that holds, the last
    one standing for any other."""
    whens = ["WHEN %s IS NOT NULL THEN %s" % (value, nonzero(value, decimals))] + ["WHEN %s THEN %d" % (c, CODE[t]) for c, t in why[:-1]]
    return "CASE %s ELSE %d END" % (" ".join(whens), CODE[why[-1][1]])


# ------------------------------------------------------------------------------------------------- the Run and the Range
# The Run (owner item 71): one start of the program, one row of process_run with its mode (live), its effective settings
# (config) and its pair. Picked as Newest Live Run (__live, the default: the live run started last, whatever runs beside it),
# Newest Paper Run (__paper) or one Run by its run_id; both newest are worked out by every query, so a new start is followed
# without reloading the page. Owner item 76: a Run is named by its start–end, venue and pair, without a number:
# 11‑26 12:57–now · Bitget · SOXL/SOXS · Live (the end's date only when it is another day; now while it runs; the mode last,
# so paper and live stay told apart). The program trades Bitget's perpetuals only (its config names no venue).
VENUE = "Bitget"
RUN_ID = ("(SELECT CASE '$run' WHEN '__live' THEN (SELECT x.run_id FROM trade.process_run x WHERE x.live ORDER BY x.started_ms DESC LIMIT 1) "
          "WHEN '__paper' THEN (SELECT x.run_id FROM trade.process_run x WHERE NOT x.live ORDER BY x.started_ms DESC LIMIT 1) ELSE '$run' END)")
# The Combo of a Run: its pair's tickers (cex.pair), Bitget's perpetuals; a Run trades one. The pick reaches SEL: a Combo the
# Run does not trade leaves the page empty.
PAIR = ("coalesce({r}.config #>> '{{cex,pair,bull,ticker}}', 'SOXL') || ' / ' || coalesce({r}.config #>> '{{cex,pair,bear,ticker}}', 'SOXS')")


def run_label(r):
    """SQL: a Run's name, start–end · venue · pair · mode (owner item 76), UTC+8."""
    return ("{start} || '–' || CASE WHEN {r}.stopped_ms IS NULL THEN 'now' WHEN {day} = {stop_day} THEN {stop_hm} ELSE {stop} END "
            "|| ' · {venue} · ' || replace({pair}, ' / ', '/') || ' · ' || CASE WHEN {r}.live THEN 'Live' ELSE 'Paper' END").format(
        r=r, start=clock("%s.started_ms" % r, "MM-DD HH24:MI"), day=clock("%s.started_ms" % r, "MM-DD"), stop_day=clock("%s.stopped_ms" % r, "MM-DD"),
        stop_hm=clock("%s.stopped_ms" % r, "HH24:MI"), stop=clock("%s.stopped_ms" % r, "MM-DD HH24:MI"), venue=VENUE, pair=PAIR.format(r=r))
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


def or_say(q, names, column, message):
    """q, or when it returns no row one row that says so in its column (owner item 78: no empty panel that only shows –; a
    table's no-value text would also fill its empty cells, so the row carries it)."""
    cells = ", ".join(("'%s'" % message if n == column else "NULL") + ' AS "%s"' % n for n in names)
    return "SELECT * FROM (%s) z\nUNION ALL SELECT %s WHERE NOT EXISTS (%s)" % (q, cells, q)


# The charts: drawn on the time picker's axis (so a drag zooms them), with the Run's seconds inside the Range only.
CHART_FROM, CHART_TO = "greatest($__from, %s)" % FROM_MS, "least($__to, %s - 1)" % TO_MS


# The Run picker: Newest Live Run, Newest Paper Run (each when one exists), then every Run, newest first, by its name
# (run_label), and · starting (its first minute) or · lost (no stop recorded and no sample in the last minute: crashed or
# killed) when it is neither running nor stopped; the two Newest name the Run they stand for now.
RUN_STATE = ("CASE WHEN r.stopped_ms IS NOT NULL THEN 'stopped' WHEN EXISTS (SELECT 1 FROM trade.signal_sample s WHERE s.ts_ms > %s - 60000 "
             "AND s.run_id = r.run_id AND s.live = r.live) THEN 'running' WHEN %s - r.started_ms < 60000 THEN 'starting' ELSE 'lost' END" % (NOW_MS, NOW_MS))
Q_RUN_OPTIONS = sql("""
SELECT x.__text, x.__value FROM (
  SELECT 'Newest Live Run · ' || @NAME@ AS __text, '__live' AS __value, 2 AS g, 0::bigint AS o
  FROM (SELECT * FROM trade.process_run x WHERE x.live ORDER BY x.started_ms DESC LIMIT 1) r
  UNION ALL
  SELECT 'Newest Paper Run · ' || @NAME@, '__paper', 1, 0 FROM (SELECT * FROM trade.process_run x WHERE NOT x.live ORDER BY x.started_ms DESC LIMIT 1) r
  UNION ALL
  SELECT @NAME@ || CASE WHEN q.state IN ('starting', 'lost') THEN ' · ' || q.state ELSE '' END, r.run_id, 0, r.started_ms
  FROM trade.process_run r CROSS JOIN LATERAL (SELECT @STATE@ AS state) q
  UNION ALL SELECT 'No Run Yet', '__none', 0, 0 WHERE NOT EXISTS (SELECT 1 FROM trade.process_run x)) x
ORDER BY x.g DESC, x.o DESC""", NAME=run_label("r"), STATE=RUN_STATE)
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
# For titles: the Run (its name), the Range in words (MM-DD HH:MI – MM-DD HH:MI, now while it reaches the present; an
# empty one says why), and the legs' tickers.
Q_RUN_TEXT = sql("""WITH @SEL@
SELECT coalesce((SELECT @NAME@ FROM sel), 'No Run') AS __text""", SEL=SEL, NAME=run_label("sel"))
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


# The program's settings of each run (process_run.config, the effective config.yaml). PRD 4.0 (core/SPEC.md 12.8) renamed
# strategy.d_in / d_out to entry_in / entry_out (entry_out is exit1 when exit2 is set); the Runs before keep the old keys, so
# every reader takes the new key and falls back to the old one. exit2 is NULL without a second exit line.
def entry_in(r):
    return "coalesce(%s.config #>> '{strategy,entry_in}', %s.config #>> '{strategy,d_in}')::float8" % (r, r)


def entry_out(r):
    return "coalesce(%s.config #>> '{strategy,entry_out}', %s.config #>> '{strategy,d_out}')::float8" % (r, r)


def exit2(r):
    return "(%s.config #>> '{strategy,exit2}')::float8" % r


D_IN, D_OUT, EXIT2 = entry_in("r"), entry_out("r"), exit2("r")
# The lines of the Run in words, for the Live Execution row's title: Long in −1σ / out +0.5σ, +1σ · Short in +1σ / out −0.5σ,
# −1σ (the second exit only when exit2 is set).
SIGNED_SIGMA = "CASE WHEN {x} < 0 THEN '−' ELSE '+' END || trim_scale(abs({x})::numeric)::text || 'σ'"
LINES_TEXT = sql("""'Long in ' || @L_IN@ || ' / out ' || @L_OUT@ || coalesce(', ' || @L_OUT2@, '') || ' · Short in ' || @S_IN@ || ' / out ' || @S_OUT@
                    || coalesce(', ' || @S_OUT2@, '')""",
                 L_IN=SIGNED_SIGMA.format(x="-" + D_IN), L_OUT=SIGNED_SIGMA.format(x="-" + D_OUT), L_OUT2=SIGNED_SIGMA.format(x="-" + EXIT2),
                 S_IN=SIGNED_SIGMA.format(x=D_IN), S_OUT=SIGNED_SIGMA.format(x=D_OUT), S_OUT2=SIGNED_SIGMA.format(x=EXIT2))
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
# figures (the Buy Both / Sell Both curves of Live Execution, Market's Signal) are shown only then (Signal: No Book).
EXECUTABLE = "({s}.stats_ready AND {s}.depth_ok_bull AND {s}.depth_ok_bear AND NOT ({s}.conflict_bull OR {s}.conflict_bear))"
# The distance in sigma, positive while the line is still ahead, to the open cycle's exit line (a Long: Sell Both up to
# -entry_out, or -exit2 for the second line; a Short: Buy Both down to +entry_out, +exit2).
def to_exit(line):
    return ("CASE s.direction WHEN 'long' THEN -{d} - s.x_short_d / {s} WHEN 'short' THEN s.x_long_d / {s} - {d} END").format(s=SIGMA, d=line)


TO_EXIT, TO_EXIT2 = to_exit(entry_out("sr")), to_exit(exit2("sr"))


def market_signal(s, d_in):
    """SQL: the Signal of sample s in Combo Spread's words (owner item 76): Long when the executable Buy Both is at or past
    −entry_in σ, Short when Sell Both is at or past +entry_in σ, None inside; No Anchor, No Mean (σ not ready) and No Book (a leg's
    book conflicts or its depth is unusable: the executor enters on none, owner item 70)."""
    sig = "nullif(%s.sigma_d, 0)" % s
    return ("CASE WHEN {s}.ts_ms IS NULL THEN NULL WHEN {s}.anchor_kind IS NULL THEN 'No Anchor' WHEN NOT {s}.stats_ready OR {sig} IS NULL THEN 'No Mean' "
            "WHEN NOT {e} THEN 'No Book' WHEN {s}.x_long_d / {sig} <= -{d} THEN 'Long' WHEN {s}.x_short_d / {sig} >= {d} THEN 'Short' ELSE 'None' END"
            ).format(s=s, sig=sig, e=EXECUTABLE.format(s=s), d=d_in)


Q_NOW = sql("""
WITH @LATEST@, @HELD@
SELECT CASE WHEN s.ts_ms IS NULL THEN NULL WHEN s.direction IS NULL THEN 'Flat' ELSE initcap(s.direction) END AS "Side",
       CASE WHEN s.direction IS NOT NULL THEN CASE s.direction WHEN 'short' THEN -s.hold_bull ELSE s.hold_bull END END AS "Bull",
       CASE WHEN s.direction IS NOT NULL THEN CASE s.direction WHEN 'short' THEN -s.hold_bear ELSE s.hold_bear END END AS "Bear",
       h.v_bull + h.v_bear AS "Cost", @HELD_FOR@ AS "Held", @TO_EXIT@ AS "To Exit", @TO_EXIT2@ AS "To Exit 2"
FROM (SELECT 1) one LEFT JOIN r ON true LEFT JOIN s ON true LEFT JOIN h ON true LEFT JOIN sr ON true""",
            LATEST=LATEST, HELD=HELD, HELD_FOR=span("%s - h.opened_ms" % NOW_MS),
            TO_EXIT="CASE WHEN s.stats_ready AND s.depth_ok_bull AND s.depth_ok_bear THEN %s END" % shown(TO_EXIT, 2),
            TO_EXIT2="CASE WHEN s.stats_ready AND s.depth_ok_bull AND s.depth_ok_bear THEN %s END" % shown(TO_EXIT2, 2))

# ------------------------------------------------------------------------------------------- Status · Running Session
# Owner items 73-76: the board starts, as the sibling dex-cex board does, with Status · Running Session: Execution and
# Account as one word per part (green well, amber needs a look, red at risk, grey nothing to judge; an amount or a time
# plain), Market, then Alerts. The picked Run (Newest Live Run by default), whatever the Range.
# The Entry word: what the program answered in its last second, short enough for a tile; the words of the strategy's
# ordinary answers (Blocked) are listed, so their colour (grey) is matched by name.
ENTRY_WORDS = {"no_signal": "ready", "regular_hours": "regular hours", "spacing": "spacing", "holding_other_side": "other side held",
               "cost_not_covered": "fees not covered", "quota_exhausted": "quota used up", "too_small": "below minimum", "warmup": "warming up",
               "zero_sigma": "σ zero", "positions_read": "reading positions", "await_orders": "orders in flight", "await_depth": "awaiting book",
               "await_close": "awaiting flat", "action_window": "dividend window", "capital_pending": "capital pending",
               "no_levels": "held · book thin", "depth_bull": "held · $bull depth", "depth_bear": "held · $bear depth",
               "conflict_bull": "held · $bull conflict", "conflict_bear": "held · $bear conflict", "fx_stale": "held · F stale",
               "rejected_backoff": "held · backing off", "no_anchor": "held · no anchor", "depth": "held · exit depth", "no_price": "held · no price",
               "order_unknown": "held · order unknown", "not_ready": "starting", "stopping": "stopping", "no_new_entries": "stop pending",
               "settings": "stop · settings", "store_error": "stop · records", "position_mismatch": "stop · mismatch",
               "pair_bound": "pairing · beyond bound", "window": "pairing · window over", "stop_d": "pairing · spread stop",
               "profit": "pairing · profit close", "exit_bound": "exit · beyond break-even", "d1_loss": "exit · below break-even",
               "exit_window": "exit · hard close", "exit_stop_d": "exit · hard close", "valuation": "held · not valued", "forced": "held · forced exit",
               "no_profit": "exit 1 · no profit", "precheck": "exit 1 · short of break-even", "half_done": "half closed"}
BLOCKED_WORDS = [w for c, s, _ in IDLE for k, w in ENTRY_WORDS.items() if k == c and s == "Blocked"]


def entry_word(code):
    return "CASE WHEN %s IS NULL THEN 'order sent' %s ELSE replace(%s, '_', ' ') END" % (code, " ".join(
        "WHEN %s = '%s' THEN '%s'" % (code, c, w) for c, w in ENTRY_WORDS.items()), code)


def money_word(x):
    """An amount in USDT as a word: +0.22 U, −1.05 U (plain on a status tile)."""
    return "CASE WHEN {x} < 0 THEN '−' ELSE '+' END || to_char(abs({x}), 'FM999,999,990.00') || ' U'".format(x=x)


LIVE_RUN = "(SELECT sel.stopped_ms IS NULL FROM sel)"
# The US market phase as Combo Spread words and colours it (owner item 76; dashboard/SPEC.md section 3, "交易时段", and Combo
# Spread's Phase figure): Pre‑Mkt, Regular, After‑Hrs, Overnight, Closed, each in its band's colour. Combo Spread reads them
# from etfpair.etf_phase, which this board's reader cannot see; the same rule is worked out here from the program's own
# second: Regular while the program is in US regular hours (its phase regular or close_window: holidays and early closes as
# the program's calendar has them); otherwise, New York time, After-Hours for 4 h from the close that started the session
# (that close: the bell's minute, the minute before the close, plus one; 16:00 before a bell is read), Overnight from 20:00
# the evening before the day whose close ends the session (session_end_ms) to 04:00 that day, Pre-Market 04:00 to 09:30
# that day, Closed otherwise (weekends, holidays, the evening after an early close).
PHASE_COLORS = {"Pre-Market": "#b88a0a", "Regular": GREY, "After-Hours": "#d55181", "Overnight": "#4f8fe6", "Closed": GREY}
PHASE_WORDS = {"Pre-Market": "Pre‑Mkt", "Regular": "Regular", "After-Hours": "After‑Hrs", "Overnight": "Overnight", "Closed": "Closed"}
NY = "'America/New_York'"


def us_phase(x):
    """SQL: the US market phase (a PHASE_COLORS key) of the program's sample x (signal_sample's columns, live among them)."""
    return sql("""CASE WHEN @X@.phase IN ('regular', 'close_window') THEN 'Regular' WHEN @X@.session_end_ms IS NULL THEN NULL ELSE (
  SELECT CASE WHEN @X@.ts_ms >= q.sc AND @X@.ts_ms < q.sc + 14400000 THEN 'After-Hours'
              WHEN q.t >= q.d - 1 + time '20:00' AND q.t < q.d + time '04:00' THEN 'Overnight'
              WHEN q.t >= q.d + time '04:00' AND q.t < q.d + time '09:30' THEN 'Pre-Market' ELSE 'Closed' END
  FROM (SELECT coalesce((SELECT min((a.detail ->> 'minute_ms')::bigint) + 60000 FROM trade.anchor_fact a
                         WHERE a.live = @X@.live AND a.session_id = @X@.session_id AND a.kind = 'bell'),
                        (extract(epoch FROM (@X@.session_id::date + time '16:00') AT TIME ZONE @NY@) * 1000)::bigint) AS sc,
               (to_timestamp(@X@.session_end_ms / 1000.0) AT TIME ZONE @NY@)::date AS d,
               to_timestamp(@X@.ts_ms / 1000.0) AT TIME ZONE @NY@ AS t) q) END""", X=x, NY=NY)


Q_STATUS_EXEC = sql("""
WITH @LATEST@
SELECT CASE WHEN r.run_id IS NULL THEN 'no run' WHEN r.live THEN 'LIVE' ELSE 'PAPER' END AS "Mode",
       CASE WHEN r.run_id IS NULL THEN 'no run' WHEN r.stopped_ms IS NOT NULL THEN 'stopped'
            WHEN s.ts_ms IS NULL AND @NOW@ - r.started_ms < 60000 THEN 'starting'
            WHEN s.ts_ms IS NULL OR @NOW@ - s.ts_ms > 60000 THEN 'lost'
            WHEN @NOW@ - s.ts_ms > @STALE_MS@ THEN 'stale · ' || @AGE@
            ELSE 'up ' || @UP@ END AS "Run",
       CASE WHEN r.stopped_ms IS NOT NULL THEN @STOPPED@ ELSE @STARTED@ END AS "Start / End",
       CASE WHEN r.stopped_ms IS NOT NULL THEN 'stopped' WHEN s.ts_ms IS NULL THEN 'no data'
            ELSE CASE @PHASE@ @PHASE_WORDS@ ELSE 'no data' END END AS "Phase",
       CASE WHEN r.stopped_ms IS NOT NULL THEN 'stopped' WHEN s.ts_ms IS NULL THEN 'no data' ELSE coalesce(nullif(concat_ws(' · ',
            CASE WHEN s.conflict_bull THEN '$bull conflict' WHEN NOT s.depth_ok_bull THEN '$bull unusable' END,
            CASE WHEN s.conflict_bear THEN '$bear conflict' WHEN NOT s.depth_ok_bear THEN '$bear unusable' END), ''), 'ok') END AS "Books",
       (SELECT CASE WHEN count(*) = 0 THEN 'none open' WHEN NOT @LIVE_RUN@ THEN count(*) || ' left open'
                    WHEN count(*) FILTER (WHERE o.unknown) > 0 THEN count(*) FILTER (WHERE o.unknown) || ' unknown'
                    ELSE count(*) || ' working' END
        FROM trade.cex_order o WHERE @RUN_O@ AND (o.unknown OR o.status IN ('pending', 'acked'))) AS "Orders",
       CASE WHEN r.stopped_ms IS NOT NULL THEN 'stopped' WHEN s.ts_ms IS NULL THEN 'no data' ELSE @ENTRY@ END AS "Entry"
FROM (SELECT 1) one LEFT JOIN r ON true LEFT JOIN s ON true""",
                    LATEST=LATEST, NOW=NOW_MS, STALE_MS=str(SAMPLE_AGE_S * 1000), AGE=span("%s - s.ts_ms" % NOW_MS),
                    UP=span("%s - r.started_ms" % NOW_MS, seconds=False), STOPPED=clock("r.stopped_ms", "MM-DD HH24:MI"),
                    STARTED=clock("r.started_ms", "MM-DD HH24:MI"), PHASE=us_phase("s"),
                    PHASE_WORDS=" ".join("WHEN '%s' THEN '%s'" % kv for kv in PHASE_WORDS.items()), LIVE_RUN=LIVE_RUN, RUN_O=of_run("o"),
                    ENTRY=entry_word("s.idle_reason"))
# Account (owner item 76; 4.0's Margin): the venue's side of the Run, from its newest inventory sample (every 10 s). Position:
# the open round trip's side and age, flat (the venue holds nothing on either leg), or the residual the venue still holds
# per leg with no round trip open (dust); Ledger: the venue's positions against the program's ledger; the whole account's
# equity, maintenance margin (mgnRatio × equity) and margin ratio (Bitget usdtEquity and mgnRatio; live only, the account
# also holds the sibling repository's positions); the nearer leg's distance to its liquidation price (live only: the paper
# venue reports none); the funding booked to the Run's round trips, or the settlements they still wait for. The board's own
# lines (the program takes no decision from these, PRD 6.6): margin ratio ok below 30 %, warn below 60 %, high from there;
# liquidation ok beyond 25 % of the mark, warn beyond 10 %, near within.
MARGIN_WARN, MARGIN_HIGH, LIQ_WARN, LIQ_NEAR = 0.30, 0.60, 0.25, 0.10
LIQ_LEGS = sql("""lq AS (SELECT x.leg, x.d FROM i CROSS JOIN LATERAL (VALUES
  ('$bull', CASE WHEN i.pos_bull < 0 THEN (i.liq_bull - i.mark_bull) / nullif(i.mark_bull, 0) WHEN i.pos_bull > 0 THEN (i.mark_bull - i.liq_bull) / nullif(i.mark_bull, 0) END),
  ('$bear', CASE WHEN i.pos_bear < 0 THEN (i.liq_bear - i.mark_bear) / nullif(i.mark_bear, 0) WHEN i.pos_bear > 0 THEN (i.mark_bear - i.liq_bear) / nullif(i.mark_bear, 0) END)) x(leg, d)
  WHERE x.d IS NOT NULL ORDER BY x.d LIMIT 1)""")
SIGNED_CT = "CASE WHEN {x} > 0 THEN '+' WHEN {x} < 0 THEN '−' ELSE '' END || trim_scale(abs({x}))::text"
Q_STATUS_ACCOUNT = sql("""
WITH @LATEST@, @HELD@, @LIQ@
SELECT CASE WHEN r.run_id IS NULL THEN 'no data' WHEN h.dir IS NOT NULL AND r.stopped_ms IS NOT NULL THEN h.dir || ' left open'
            WHEN h.dir IS NOT NULL THEN h.dir || ' · ' || @HELD_FOR@
            WHEN i.ts_ms IS NULL OR i.pos_bull IS NULL OR i.pos_bear IS NULL THEN 'no data'
            WHEN i.pos_bull = 0 AND i.pos_bear = 0 THEN 'flat'
            ELSE 'residual · ' || concat_ws(' ', CASE WHEN i.pos_bull <> 0 THEN '$bull ' || @RB@ END, CASE WHEN i.pos_bear <> 0 THEN '$bear ' || @RE@ END) END AS "Position",
       CASE WHEN i.ts_ms IS NULL THEN 'no data' WHEN i.mismatch THEN 'mismatch' WHEN i.pos_bull IS NULL OR i.pos_bear IS NULL THEN 'not read'
            ELSE 'match' END AS "Ledger",
       CASE WHEN r.run_id IS NULL OR i.ts_ms IS NULL THEN 'no data' WHEN NOT r.live THEN 'paper' WHEN r.stopped_ms IS NOT NULL THEN 'stopped'
            WHEN i.equity IS NULL THEN 'no data' ELSE to_char(i.equity, 'FM999,999,990.00') || ' U' END AS "Equity",
       CASE WHEN r.run_id IS NULL OR i.ts_ms IS NULL THEN 'no data' WHEN NOT r.live THEN 'paper' WHEN r.stopped_ms IS NOT NULL THEN 'stopped'
            WHEN i.equity IS NULL OR i.margin_ratio IS NULL THEN 'no data' ELSE to_char(i.margin_ratio * i.equity, 'FM999,999,990.00') || ' U' END AS "Maint. Margin",
       CASE WHEN r.run_id IS NULL OR i.ts_ms IS NULL THEN 'no data' WHEN NOT r.live THEN 'paper' WHEN r.stopped_ms IS NOT NULL THEN 'stopped'
            WHEN i.margin_ratio IS NULL THEN 'no data'
            ELSE CASE WHEN i.margin_ratio < @M_WARN@ THEN 'ok' WHEN i.margin_ratio < @M_HIGH@ THEN 'warn' ELSE 'high' END
                 || ' · ' || to_char(100 * i.margin_ratio, 'FM990.00') || ' %' END AS "Margin Ratio",
       CASE WHEN r.run_id IS NULL OR i.ts_ms IS NULL THEN 'no data' WHEN NOT r.live THEN 'paper' WHEN r.stopped_ms IS NOT NULL THEN 'stopped'
            WHEN coalesce(i.pos_bull, 0) = 0 AND coalesce(i.pos_bear, 0) = 0 THEN 'flat' WHEN lq.d IS NULL THEN 'no data'
            ELSE CASE WHEN lq.d < @L_NEAR@ THEN 'near' WHEN lq.d < @L_WARN@ THEN 'warn' ELSE 'ok' END
                 || ' · ' || lq.leg || ' ' || to_char(100 * lq.d, 'FM999,990') || ' %' END AS "Liquidation",
       (SELECT CASE WHEN count(*) FILTER (WHERE jsonb_typeof(c.funding_awaited) = 'array' AND jsonb_array_length(c.funding_awaited) > 0) > 0
                    THEN sum(CASE WHEN jsonb_typeof(c.funding_awaited) = 'array' THEN jsonb_array_length(c.funding_awaited) ELSE 0 END) || ' awaited'
                    ELSE @FUNDING@ END
        FROM trade.cycle c WHERE @RUN_C@) AS "Funding"
FROM (SELECT 1) one LEFT JOIN r ON true LEFT JOIN s ON true LEFT JOIN i ON true LEFT JOIN h ON true LEFT JOIN lq ON true""",
                       LATEST=LATEST, HELD=HELD, LIQ=LIQ_LEGS, M_WARN=str(MARGIN_WARN), M_HIGH=str(MARGIN_HIGH), L_NEAR=str(LIQ_NEAR),
                       L_WARN=str(LIQ_WARN), RUN_C=cycle_of_run("c"), RB=SIGNED_CT.format(x="i.pos_bull"), RE=SIGNED_CT.format(x="i.pos_bear"),
                       HELD_FOR=span("least(%s, r.end_ms) - h.opened_ms" % NOW_MS, seconds=False),
                       FUNDING=money_word("coalesce(sum(c.funding) FILTER (WHERE c.funding_known), 0)"))
# Market (owner items 76, 80): the Run's newest second as Combo Spread's latest-second strips show a pair: both legs' price
# (the mid on the program's book) and anchor (USD), the anchor's kind (Combo Spread's Anchor Status: NAV, Bell, Missing), F
# (USD per USDT); the Real Spread (owner item 80: the spread at both legs' pricing levels, buying both, Long, and selling
# both, Short: mean + the executable deviation x_long_d / x_short_d, as core/strategy Deviation defines x), the mean, the
# volatility σ, the deviation on the mids in bps and in σ, and the Signal (market_signal). Every spread figure in bps, as
# Combo Spread writes it: 10⁴ × (F × (P_bull ÷ A_bull + P_bear ÷ A_bear) − 2), the two legs' deviations added (the program's
# spread unit; 1 bps of spread is 0.5 bps of the two legs' notional).
SIGNAL_COLORS = {"Long": BLUE, "Short": ORANGE, "None": TEXT, "No Anchor": GREY, "No Mean": GREY, "No Book": GREY}
ANCHOR_COLORS = {"NAV": TEXT, "Bell": GREY, "Missing": WARN}


def bps_text(x):
    """x as spread bps text, a minus as − (as the figures Grafana writes)."""
    return "replace(to_char(round((%s)::numeric, 1), 'FM9,990.0'), '-', '−') || '%sbps'" % (x, NBSP)   # a no-break space: a line breaks at · or /, not in a figure


Q_MARKET = sql("""
WITH @LATEST@
SELECT @MID_BULL@ AS "Bull Price", s.anchor_bull AS "Bull Anchor", @MID_BEAR@ AS "Bear Price", s.anchor_bear AS "Bear Anchor",
       CASE WHEN s.ts_ms IS NULL THEN NULL WHEN s.anchor_kind = 'nav' THEN 'NAV' WHEN s.anchor_kind = 'bell' THEN 'Bell' ELSE 'Missing' END AS "Anchor",
       s.f AS "F",
       CASE WHEN NOT s.stats_ready THEN NULL WHEN NOT @EXEC@ THEN '—' ELSE coalesce(@LONG@, '–') ||
            CASE WHEN s.x_long_d / @SIGMA@ <= -@D_IN@ THEN ' · Entry' ELSE '' END END AS "Real Long",
       CASE WHEN NOT s.stats_ready THEN NULL WHEN NOT @EXEC@ THEN '—' ELSE coalesce(@SHORT@, '–') ||
            CASE WHEN s.x_short_d / @SIGMA@ >= @D_IN@ THEN ' · Entry' ELSE '' END END AS "Real Short",
       @MEAN@ AS "Mean", @SIGMA_BPS@ AS "Volatility",
       CASE WHEN s.stats_ready AND @SIGMA@ IS NOT NULL THEN @DEV_BPS@ || ' · ' || replace(to_char(round(((s.mid_d - s.mu_d) / @SIGMA@)::numeric, 2), 'FM990.00'), '-', '−') || '@S@σ' END AS "Deviation",
       @SIGNAL@ AS "Signal"
FROM (SELECT 1) one LEFT JOIN r ON true LEFT JOIN s ON true LEFT JOIN sr ON true""",
               LATEST=LATEST, MID_BULL=shown("s.mid_bull", 3), MID_BEAR=shown("s.mid_bear", 3), MEAN=bps_text("s.mu_d"), SIGMA_BPS=shown("s.sigma_d", 1),
               SIGMA=SIGMA, LONG=bps_text("s.mu_d + s.x_long_d"), SHORT=bps_text("s.mu_d + s.x_short_d"), DEV_BPS=bps_text("s.mid_d - s.mu_d"),
               SIGNAL=market_signal("s", entry_in("sr")), S=NBSP, EXEC=EXECUTABLE.format(s="s"), D_IN=entry_in("sr"))
# Under the Summary (owner item 80): the Run's parameters a trader reads with its results, from process_run.config: the mean
# model and its scale, the volatility's scale, the entry point and the exit lines in σ and in spread bps at the newest
# second's σ (4.4: Exit 1 is entry_out, the line of the half close when exit2 is set; Exit 2 none without it), the real
# capital M, the leverage L and the notional N = M × L (USDT). The Settings row keeps the rest.
def sigma_bps(x):
    """A setting in σ and in spread bps at the newest second's σ: 1σ · 3.1 bps, −0.5σ · −1.5 bps (a minus as −)."""
    return "replace(trim_scale((%s)::numeric)::text || 'σ · ' || coalesce(%s, '–%sbps'), '-', '−')" % (x, bps_text("%s * s.sigma_d" % x), NBSP)


def go_dur(x):
    """A Go duration as config.yaml's effective form writes it, shortened: 1m0s -> 1m, 1h0m0s -> 1h, 30s stays."""
    return "regexp_replace(regexp_replace(%s, '([hm])0s$', '\\1'), 'h0m$', 'h')" % x


# 4.4 (core/SPEC.md 12.8, step 10.9): the thin-leg-first execution's settings beside the size (a second strip under the
# signal's: eight short figures stay on one line at 880 px, thirteen did not): which leg goes first (exec.thin_leg: bull, bear,
# or "" for both legs together, the way before), how long the other leg is chased after the thin leg's first fill
# (pair_window), the ideally paired spread worse than at the decision that ends the chase (pair_stop_d, spread bps), how far
# top-ups may take the thin leg (thin_topup_max × its planned quantity) and how long an exit's other leg waits for its
# break-even bound (exit_pair_window). A Run before it: both together, the rest not used (—). One query, in the strips' order
# (a strip fed by -- Dashboard -- shows the frame's order).
Q_PARAMS = sql("""
WITH @LATEST@
SELECT upper(r.config #>> '{strategy,mean_model}') || ' · ' || (r.config #>> '{strategy,mean_minutes}') || '@S@min' AS "Mean Model",
       (r.config #>> '{strategy,sigma_minutes}') || '@S@min' AS "Volatility Scale",
       @IN@ AS "Entry", @OUT@ AS "Exit 1", coalesce(@OUT2@, 'none') AS "Exit 2",
       to_char(round((r.config #>> '{inventory,capital}')::numeric), 'FM999,999,990') || ' U' AS "Capital M Text", (r.config #>> '{inventory,leverage}')::numeric AS "Leverage L",
       to_char(round((r.config #>> '{inventory,capital}')::numeric * (r.config #>> '{inventory,leverage}')::numeric), 'FM999,999,990') || ' U' AS "Notional N Text",
       CASE WHEN NOT @THIN@ THEN 'both@S@together'
            ELSE coalesce(r.config #>> ARRAY['cex', 'pair', r.config #>> '{exec,thin_leg}', 'ticker'], r.config #>> '{exec,thin_leg}') || '@S@first' END AS "Thin Leg",
       CASE WHEN @THIN@ THEN @WINDOW@ ELSE '—' END AS "Pair Window",
       CASE WHEN @THIN@ THEN trim_scale((r.config #>> '{exec,pair_stop_d}')::numeric)::text || '@S@bps' ELSE '—' END AS "Pair Stop",
       CASE WHEN @THIN@ THEN trim_scale((r.config #>> '{exec,thin_topup_max}')::numeric)::text || '@S@×' ELSE '—' END AS "Top-Up Max",
       CASE WHEN @THIN@ THEN @EXIT_WINDOW@ ELSE '—' END AS "Exit Pair Window",
       (r.config #>> '{inventory,capital}')::numeric AS "Capital M",
       (r.config #>> '{inventory,capital}')::numeric * (r.config #>> '{inventory,leverage}')::numeric AS "Notional N"
FROM r LEFT JOIN s ON true""", LATEST=LATEST, IN=sigma_bps(D_IN), OUT=sigma_bps(D_OUT), OUT2="CASE WHEN %s IS NOT NULL THEN %s END" % (EXIT2, sigma_bps(EXIT2)),
               S=NBSP, THIN="coalesce(r.config #>> '{exec,thin_leg}', '') <> ''", WINDOW=go_dur("(r.config #>> '{exec,pair_window}')"),
               EXIT_WINDOW=go_dur("(r.config #>> '{exec,exit_pair_window}')"))
# The colours of the words (Grafana takes the first mapping that matches): the mode first, then red, amber, green, grey;
# an amount, a time or a share plain.
STATUS_WORDS = [
    ("^LIVE$", "#8a1f1f"), ("^PAPER$", "#5a5953")] + [("^%s$" % word, PHASE_COLORS[name]) for name, word in PHASE_WORDS.items()] + [
    ("^(lost|stale · .*|mismatch|\\d+ unknown|.* left open|near · .*|high · .*|stop · .*)$", BAD),
    ("^(starting|.* conflict.*|.* unusable.*|not read|held · .*|pairing · .*|exit · .*|stopping|stop pending|warn · .*|residual · .*|\\d+ awaited)$", WARN),
    ("^(up .*|ok|ok · .*|match|none open|\\d+ working|ready|order sent|(long|short)( · .*)?)$", WELL),
    ("^(no data|no run|stopped|flat|paper|%s)$" % "|".join(re.escape(w) for w in BLOCKED_WORDS), GREY),
    ("^([+−].* U|[0-9,.]+ U|\\d\\d‑\\d\\d\u00a0\\d\\d:\\d\\d)$", "transparent")]

# Alerts: what stopped or held trading in the picked Run and what became of it, the unfinished first, each group newest
# first, at most 200 (the sibling board's Alerts, mapped onto what schema trade records; the program writes no alert of
# its own beyond trade.event, which holds only its errors):
#   held entries   the program's idle reasons of Held Back and Needs a Person (IDLE), one row per reason over the Run: how
#                  often, how long in all and at most, the seconds an executable deviation was past an entry line meanwhile;
#   sample gaps    more than GAP_MS without a sample while the Run ran (the loop stalled or the process was down);
#   stale          a running Run without a sample for SAMPLE_AGE_S (lost after a minute);
#   mismatch       an episode of inventory_sample.mismatch (the venue's positions and the ledger disagree);
#   round trips    ended on one leg, on a stop or a session change while holding, a forced exit, dust left (recorded only,
#                  owner item 47), funding awaited;
#   orders         rejected, not sent, never arrived, not found, or with an unknown outcome;
#   events         trade.event of level error or warn.
# Status: Open (a person is needed), In Progress (the program is still on it, while the Run runs), Recovered (it ended by
# itself), Recorded (dust: recorded, not processed).
GAP_MS = 5000
SEVERITY = {"critical": BAD, "serious": SERIOUS, "warning": WARN}
ALERT_STATUS = {"Open": BAD, "In Progress": WARN, "Recovered": GOOD, "Recorded": TEXT}
HELD_CODES = [c for c, s, _ in IDLE if s in ("Held Back", "Needs a Person") and c not in ("not_ready", "forced")]   # a forced exit has its own alert
CRITICAL_CODES = [c for c, s, _ in IDLE if s == "Needs a Person"]
D_IN_R = entry_in("sr")


def quoted(codes):
    return "(%s)" % ", ".join("'%s'" % c for c in codes)


def ct(x):
    return "trim_scale((%s)::numeric)::text" % x


Q_ALERTS = sql("""
WITH @LATEST@,
x AS MATERIALIZED (
  SELECT q.ts_ms, q.idle_reason, q.ts_ms - lag(q.ts_ms) OVER w AS gap,
         q.stats_ready AND (q.x_long_d / nullif(q.sigma_d, 0) <= -@D_IN@ OR q.x_short_d / nullif(q.sigma_d, 0) >= @D_IN@) AS at_line,
         CASE WHEN q.idle_reason IS DISTINCT FROM lag(q.idle_reason) OVER w OR q.ts_ms - lag(q.ts_ms) OVER w > 3000 THEN 1 ELSE 0 END AS new_ep
  FROM trade.signal_sample q CROSS JOIN sr WHERE @RUN_Q@ WINDOW w AS (ORDER BY q.ts_ms)),
ep AS (SELECT x.idle_reason AS code, min(x.ts_ms) AS t0, count(*) AS n, count(*) FILTER (WHERE x.at_line) AS at_line
       FROM (SELECT x.*, sum(x.new_ep) OVER (ORDER BY x.ts_ms) AS k FROM x WHERE x.idle_reason IN @HELD_CODES@) x GROUP BY x.idle_reason, x.k),
held AS (SELECT ep.code, min(ep.t0) AS first_ms, max(ep.t0) AS last_ms, count(*) AS times, sum(ep.n) AS seconds, max(ep.n) AS longest,
                sum(ep.at_line) AS at_line FROM ep GROUP BY ep.code),
mi AS (SELECT y.*, sum(y.st) OVER (ORDER BY y.ts_ms) AS k FROM (
         SELECT m.ts_ms, m.mismatch, m.pos_bull, m.pos_bear, m.ledger_bull, m.ledger_bear,
                CASE WHEN m.mismatch AND NOT coalesce(lag(m.mismatch) OVER (ORDER BY m.ts_ms), false) THEN 1 ELSE 0 END AS st
         FROM trade.inventory_sample m WHERE @RUN_M@) y),
mis AS (SELECT min(mi.ts_ms) AS t0, max(mi.ts_ms) AS t1, (array_agg(mi.pos_bull ORDER BY mi.ts_ms))[1] AS pb, (array_agg(mi.ledger_bull ORDER BY mi.ts_ms))[1] AS lb,
               (array_agg(mi.pos_bear ORDER BY mi.ts_ms))[1] AS pe, (array_agg(mi.ledger_bear ORDER BY mi.ts_ms))[1] AS le
        FROM mi WHERE mi.mismatch GROUP BY mi.k),
al AS (
  SELECT h.last_ms AS ts_ms, CASE WHEN h.code IN @CRITICAL@ THEN 'critical' ELSE 'warning' END AS sev,
         CASE WHEN @LIVE_RUN@ AND h.code = (SELECT s.idle_reason FROM s) THEN CASE WHEN h.code IN @CRITICAL@ THEN 'Open' ELSE 'In Progress' END
              ELSE 'Recovered' END AS status,
         'Entry' AS source, @HELD_WHY@ AS what,
         h.times || CASE WHEN h.times = 1 THEN ' time' ELSE ' times' END || ' since ' || @FIRST@ || ', ' || h.seconds || ' s in all, longest ' || h.longest || ' s'
           || CASE WHEN h.at_line > 0 THEN '; ' || h.at_line || ' s of them past an entry line' ELSE '' END AS detail, NULL::text AS cycle_id
  FROM held h
  UNION ALL
  SELECT x.ts_ms - x.gap, 'serious', 'Recovered', 'Run', 'Sample Gap', 'no sample for ' || @GAP@ || ': the loop stalled or the process was down', NULL
  FROM x WHERE x.gap > @GAP_MS@
  UNION ALL
  SELECT coalesce(s.ts_ms, r.started_ms), 'critical', 'Open', 'Run', CASE WHEN s.ts_ms IS NULL OR @NOW@ - s.ts_ms > 60000 THEN 'Run Lost' ELSE 'Samples Stale' END,
         'no stop recorded and ' || CASE WHEN s.ts_ms IS NULL THEN 'no sample yet' ELSE 'the newest sample ' || @AGE@ || ' old' END
         || ': the process crashed, hung or cannot write', NULL
  FROM r LEFT JOIN s ON true WHERE r.stopped_ms IS NULL AND @NOW@ - r.started_ms >= 60000 AND (s.ts_ms IS NULL OR @NOW@ - s.ts_ms > @STALE_MS@)
  UNION ALL
  SELECT mis.t0, 'critical', CASE WHEN mis.t1 = (SELECT i.ts_ms FROM i) AND @LIVE_RUN@ THEN 'Open' ELSE 'Recovered' END, 'Positions', 'Position Mismatch',
         'venue / ledger: $bull ' || coalesce(@PB@, '–') || ' / ' || @LB@ || ', $bear ' || coalesce(@PE@, '–') || ' / ' || @LE@ || ' ct; no orders until a person checks', NULL
  FROM mis
  UNION ALL
  SELECT coalesce(c.closed_ms, c.opened_ms), CASE WHEN c.end_reason = 'one_leg' AND NOT @THIN_C@ THEN 'serious' ELSE 'warning' END,
         CASE WHEN c.end_reason = 'dust_only' OR c.dust_bull <> 0 OR c.dust_bear <> 0 THEN 'Recorded' ELSE 'Recovered' END,
         'Execution ' || @OPENED@,
         CASE WHEN c.end_reason = 'one_leg' AND @THIN_C@ THEN 'Nothing Paired' ELSE CASE c.end_reason WHEN 'one_leg' THEN 'One Leg Only' WHEN 'stop' THEN 'Stopped While Holding' WHEN 'session_changed' THEN 'Session Ended While Holding'
              WHEN 'store_error' THEN 'Not Recorded' ELSE 'Dust Left' END END,
         initcap(c.direction) || ', ' || CASE WHEN c.end_reason = 'one_leg' AND @THIN_C@ THEN 'every batch unwound' ELSE @REASON@ END || ', net ' || CASE WHEN c.funding_known THEN replace(to_char(c.net, 'FM999,990.0000'), '-', '−') || ' USDT' ELSE 'Pending' END
           || CASE WHEN c.dust_bull <> 0 OR c.dust_bear <> 0 THEN '; dust left (recorded only): $bull ' || @DB@ || ', $bear ' || @DE@ || ' ct' ELSE '' END, c.cycle_id
  FROM trade.cycle c WHERE @RUN_C@ AND (c.end_reason IN ('one_leg', 'stop', 'session_changed', 'store_error', 'dust_only') OR c.dust_bull <> 0 OR c.dust_bear <> 0)
  UNION ALL
  SELECT d.ts_ms, 'serious', CASE WHEN c.status = 'open' AND @LIVE_RUN@ THEN 'In Progress' ELSE 'Recovered' END, 'Execution ' || coalesce(@OPENED@, '–'),
         'Forced Exit', CASE d.detail ->> 'cause' WHEN 'stop' THEN 'on the stop' WHEN 'close_window' THEN 'before the close' ELSE coalesce(d.detail ->> 'cause', 'forced') END
           || ', IOC with ' || coalesce(d.detail ->> 'slip_bps', '–') || ' bps slip', d.cycle_id
  FROM trade.decision d LEFT JOIN trade.cycle c ON c.live = d.live AND c.cycle_id = d.cycle_id WHERE @RUN_D@ AND d.kind = 'forced'
  UNION ALL
  SELECT d.ts_ms, 'warning', CASE WHEN c.status = 'open' AND @LIVE_RUN@ THEN 'In Progress' ELSE 'Recovered' END, 'Execution ' || coalesce(@OPENED@, '–'),
         CASE d.kind WHEN 'pair' THEN 'Thin Leg Unwound' ELSE 'Exit Hard Close' END,
         CASE d.reason WHEN 'window' THEN 'the pair window ran out with ' || coalesce(@UNPAIRED@, '–') || ' ct of $bear not paired: unwound'
                       WHEN 'stop_d' THEN 'the ideally paired spread ' || coalesce(@IDEAL@, '–') || ' bps against ' || coalesce(@DEC@, '–') || ' bps at the decision: unwound'
                       WHEN 'too_small' THEN 'what was not paired is too small to pair: unwound'
                       WHEN 'exit_window' THEN 'the exit''s thick leg found no price within break-even in the exit pair window: closed whatever the price'
                       WHEN 'exit_stop_d' THEN 'the ideal closing spread ' || coalesce(@IDEAL@, '–') || ' bps against ' || coalesce(@DEC@, '–') || ' bps at the decision: closed whatever the price'
                       ELSE coalesce(d.reason, '–') END, d.cycle_id
  FROM trade.decision d LEFT JOIN trade.cycle c ON c.live = d.live AND c.cycle_id = d.cycle_id
  WHERE @RUN_D@ AND d.kind IN ('pair', 'exit_pair') AND d.detail ->> 'action' = 'risk_exit'
  UNION ALL
  SELECT d.ts_ms, 'warning', CASE WHEN c.status = 'open' AND @LIVE_RUN@ THEN 'In Progress' ELSE 'Recovered' END, 'Execution ' || coalesce(@OPENED@, '–'),
         'Exit Below Break-Even', 'the batch''s pre-check expected a round net of ' || coalesce(@EXPECTED@, '–') || ' USDT; it exits all the same at '
           || CASE d.detail ->> 'line' WHEN 'exit2' THEN 'exit 2' ELSE 'the exit line' END || ' (PRD 9.2)', d.cycle_id
  FROM trade.decision d LEFT JOIN trade.cycle c ON c.live = d.live AND c.cycle_id = d.cycle_id WHERE @RUN_D@ AND d.kind = 'exit' AND d.reason = 'd1_loss'
  UNION ALL
  SELECT c.closed_ms, 'warning', CASE WHEN @LIVE_RUN@ THEN 'In Progress' ELSE 'Open' END, 'Execution ' || @OPENED@, 'Funding Awaited',
         jsonb_array_length(c.funding_awaited) || ' settlement(s) without a record: its net stays Pending', c.cycle_id
  FROM trade.cycle c WHERE @RUN_C@ AND jsonb_typeof(c.funding_awaited) = 'array' AND jsonb_array_length(c.funding_awaited) > 0
  UNION ALL
  SELECT o.submitted_ms, CASE WHEN o.status = 'not_sent' THEN 'warning' ELSE 'serious' END,
         CASE WHEN o.unknown THEN CASE WHEN @LIVE_RUN@ THEN 'In Progress' ELSE 'Open' END ELSE 'Recovered' END,
         'Order · ' || CASE o.leg WHEN 'bull' THEN '$bull' ELSE '$bear' END,
         CASE WHEN o.unknown THEN 'Order Outcome Unknown' WHEN o.not_found THEN 'Order Not Found' WHEN o.status = 'rejected' THEN 'Order Rejected'
              WHEN o.status = 'not_sent' THEN 'Order Not Sent' ELSE 'Order Never Arrived' END,
         initcap(o.purpose) || ' ' || o.side || ' ' || @QTY@ || ' @ ' || @PRICE@ || coalesce(' · ' || nullif(concat_ws(' ', o.error_code, o.error_message), ''), ''), o.cycle_id
  FROM trade.cex_order o WHERE @RUN_O@ AND (o.unknown OR o.not_found OR o.status IN ('rejected', 'not_sent', 'never_arrived'))
  UNION ALL
  SELECT e.ts_ms, CASE e.level WHEN 'error' THEN 'critical' ELSE 'warning' END, 'Open', 'Program · ' || e.module, initcap(replace(e.kind, '_', ' ')), e.message, NULL
  FROM trade.event e WHERE @RUN_E@ AND e.level IN ('error', 'warn'))
SELECT t.time, t."Severity", t."Status", t."Source", t."What", t."Detail", t.cycle_id FROM (
  SELECT to_timestamp(a.ts_ms / 1000.0) AS time, a.sev AS "Severity", a.status AS "Status", a.source AS "Source", a.what AS "What", a.detail AS "Detail",
         a.cycle_id, CASE a.status WHEN 'Open' THEN 0 WHEN 'In Progress' THEN 1 ELSE 2 END AS k, a.ts_ms
  FROM al a ORDER BY 8, a.ts_ms DESC LIMIT 200) t
UNION ALL
SELECT NULL, NULL, NULL, NULL, NULL, 'No alert in this Run: nothing stopped or held trading', NULL WHERE NOT EXISTS (SELECT 1 FROM al)""",
               LATEST=LATEST, D_IN=D_IN_R, RUN_Q=of_run("q"), RUN_M=of_run("m"), RUN_C=cycle_of_run("c"), RUN_D=of_run("d"), RUN_O=of_run("o"),
               RUN_E=of_run("e"), HELD_CODES=quoted(HELD_CODES), CRITICAL=quoted(CRITICAL_CODES), LIVE_RUN=LIVE_RUN, HELD_WHY=idle_why("h.code"),
               FIRST=clock("h.first_ms", "HH24:MI:SS"), GAP=span("x.gap"), GAP_MS=str(GAP_MS), NOW=NOW_MS, AGE=span("%s - s.ts_ms" % NOW_MS),
               STALE_MS=str(SAMPLE_AGE_S * 1000), PB=ct("mis.pb"), LB=ct("mis.lb"), PE=ct("mis.pe"), LE=ct("mis.le"),
               OPENED=clock("c.opened_ms", "MM-DD HH24:MI:SS"), REASON="lower(%s)" % end_reason("c.end_reason"), DB=ct("c.dust_bull"),
               DE=ct("c.dust_bear"), QTY=ct("o.qty"), PRICE=ct("o.price"),
               THIN_C="EXISTS (SELECT 1 FROM trade.lot x WHERE x.live = c.live AND x.cycle_id = c.cycle_id AND to_jsonb(x) ->> 'end_state' IS NOT NULL)",
               UNPAIRED="trim_scale(round(((d.detail ->> 'thin_qty')::numeric - coalesce((d.detail ->> 'thick_qty')::numeric, 0) * c.pair_anchor_bull / c.pair_anchor_bear), 2))::text",
               IDEAL="replace(to_char(round((d.detail ->> 'd_ideal')::numeric, 1), 'FM9990.0'), '-', '−')",
               DEC="replace(to_char(round((d.detail ->> 'd_dec')::numeric, 1), 'FM9990.0'), '-', '−')",
               EXPECTED="replace(to_char((d.detail ->> 'expected_net')::numeric, 'FM9990.00'), '-', '−')")


# ------------------------------------------------------------------------------------------------------------------- P&L
# The round trips of the Run closed in the Range. A cycle whose funding is not known yet (funding_known false) is left out of
# every money figure (Net, Fees, Funding, Win Rate) until it is known; Trips counts it.
CLOSED = "trade.cycle c WHERE %s AND c.status <> 'open' AND %s" % (cycle_of_run("c"), in_range("c.closed_ms"))
# The Execution Summary of the Range (owner item 76, in this order): the round trips closed in it (no-fill ones left out;
# one whose funding is not known yet counted in Notional only): PnL (net), Gross, Fee, Funding, the open round trip's P&L, the
# Notional traded (both legs, every fill of those round trips), the Peak Used (the largest quota use, of N = M × L, the Run's
# inventory samples in the Range show: held at entry prices, open orders, dust), the Win Rate and the Return: PnL over Peak
# Used (owner item 69: a return on the largest notional actually used), bps.
Q_PNL = sql("""
WITH @LATEST@, c AS MATERIALIZED (
  SELECT c.*, (SELECT sum(x.entry_value_bull + x.entry_value_bear + x.exit_value_bull + x.exit_value_bear) FROM trade.lot x
               WHERE x.live = c.live AND x.cycle_id = c.cycle_id) AS traded
  FROM @CLOSED@ AND c.end_reason IS DISTINCT FROM 'no_fill'),
k AS (SELECT * FROM c WHERE c.funding_known), @HELD@,
pk AS (SELECT max(p.quota_used) AS used FROM trade.inventory_sample p WHERE @RUN_P@ AND @RANGE_P@)
SELECT (SELECT coalesce(sum(k.net), 0) FROM k) AS "PnL", (SELECT coalesce(sum(k.gross), 0) FROM k) AS "Gross",
       (SELECT coalesce(sum(k.fees), 0) FROM k) AS "Fee", (SELECT coalesce(sum(k.funding), 0) FROM k) AS "Funding",
       (SELECT @UNREALISED@ FROM (SELECT 1) one LEFT JOIN s ON true LEFT JOIN h ON true) AS "Open PnL",
       (SELECT coalesce(sum(c.traded), 0) FROM c) AS "Notional", (SELECT pk.used FROM pk) AS "Peak Used",
       (SELECT to_char(round(coalesce(sum(c.traded), 0)), 'FM999,999,990') || ' U' FROM c) AS "Notional Text", (SELECT to_char(round(pk.used), 'FM999,999,990') || ' U' FROM pk) AS "Peak Used Text",
       (SELECT 100.0 * count(*) FILTER (WHERE k.net > 0) / nullif(count(*), 0) FROM k) AS "Win Rate",
       (SELECT 10000 * coalesce(sum(k.net), 0) / nullif((SELECT pk.used FROM pk), 0) FROM k) AS "Return" """,
            CLOSED=CLOSED, LATEST=LATEST, HELD=HELD, UNREALISED=shown(UNREALISED, 2), RUN_P=of_run("p"), RANGE_P=in_range("p.ts_ms"))
# The Execution Summary's charts, as the sibling board's, on the Range's own axis (an xy chart: Time in ms; Q_PERIOD gives the
# axis its two ends however few points there are): the running Net as steps from 0 at the Range's start to its end (now
# while it reaches the present), and each round trip's Net at its close, gains and losses apart (an xy chart draws a
# missing value as zero), its cycle_id for the link to its orders.
Q_PERIOD = sql("""WITH @SEL@, e AS (SELECT rg.from_ms AS t0, least(rg.to_ms - 1, @NOW@) AS t1 FROM rg)
SELECT e.t0 AS "Time", 0 AS "Period", e.t0 AS "From", e.t1 AS "To" FROM e UNION ALL SELECT e.t1, 0, e.t0, e.t1 FROM e""", SEL=SEL, NOW=NOW_MS)
KNOWN = sql("""k AS MATERIALIZED (SELECT c.cycle_id, c.closed_ms, c.gross, c.net FROM @CLOSED@ AND c.funding_known AND c.end_reason IS DISTINCT FROM 'no_fill')""",
            CLOSED=CLOSED)
# Owner item 77: under the Summary, cumulative Gross and PnL over the Range, as the Summary counts them, a step at each close,
# from 0 at the Range's start to its end (now while it reaches the present).
def cumulative(column, name):
    return sql("""
WITH @SEL@, @KNOWN@,
cum AS (SELECT k.closed_ms AS t, k.@COL@ AS v, row_number() OVER w AS n, sum(k.@COL@) OVER w AS c
        FROM k WINDOW w AS (ORDER BY k.closed_ms, k.cycle_id ROWS UNBOUNDED PRECEDING))
SELECT x.t AS "Time", x.c AS "@NAME@" FROM (
  SELECT rg.from_ms AS t, 0::numeric AS c, 0 AS n, 0 AS o FROM rg
  UNION ALL SELECT cum.t, cum.c - cum.v, cum.n, 1 FROM cum
  UNION ALL SELECT cum.t, cum.c, cum.n, 2 FROM cum
  UNION ALL SELECT least(rg.to_ms - 1, @NOW@), coalesce((SELECT cum.c FROM cum ORDER BY cum.n DESC LIMIT 1), 0), 1e9, 3 FROM rg) x
ORDER BY x.t, x.n, x.o""", SEL=SEL, KNOWN=KNOWN, NOW=NOW_MS, COL=column, NAME=name)


# Owner items 77, 80: under the Summary, the cumulative Gross and the cumulative PnL over the Range, two charts, as the Summary
# counts them, a step at each close, from 0 at the Range's start to its end (now while it reaches the present).
Q_CUM_GROSS, Q_CUM_PNL = cumulative("gross", "Gross"), cumulative("net", "PnL")

# ------------------------------------------------------------------------------------------------------- Live Execution
# The step of the drawn points: the finest of 1, 5, 10, 30 seconds and whole minutes that keeps a line under about 1500
# points, about the plot's width in pixels at 1680 px. Each point is a bucket of the per-second samples: its last spread on
# the mids, mean and sigma, and the extremes on the side each real spread triggers on (the lowest Real Long Spread, where a
# Long enters; the highest Real Short Spread, where a Short enters), so a crossing between two drawn points is never lost.
STEP_MS = ("(SELECT CASE WHEN r <= 1000 THEN 1000 WHEN r <= 5000 THEN 5000 WHEN r <= 10000 THEN 10000 "
           "WHEN r <= 30000 THEN 30000 ELSE ceil(r / 60000) * 60000 END::bigint FROM (SELECT ($__to - $__from) / 1500.0 AS r) x)")

# The decisions drawn as markers (owner item 81): every entry, exit and forced exit, at its instant. Each with the fills of its
# step (4.2.1): its own orders and those sent for it, so an exit whose two legs were closed by separate orders shows both:
#   an entry    every order booked on its lot (cex_order.lot_id): with both legs together the completions and trims sent for
#               the batch; with the thin leg first (4.4, core/SPEC.md 12.4) the thin order, its top-ups (purpose entry), the
#               thick leg's pairing orders (purpose complete) and the unwinds and profit closes of the unpaired thin leg (trim);
#   an exit     its orders, the exits of the remaining leg sent for it (decisions whose detail.batch_sent_ms is its instant)
#               and, with the thin leg first (12.7), the thick leg's orders of its exit_pair steps up to the next exit or forced
#               exit of the round trip;
#   forced      its orders.
# Trims count apart (Trimmed Back), never in the fills. Both legs' filled contracts and average price, the notional (USDT,
# both legs), the fees paid (a cost, positive, as in Summary and Orders), and its round trip's net (Pending, or Open while
# open); a leg the step sent no order on is "—", never 0 / No Fill. Its place: on the Spread chart its executable spread at
# the decision (mu_d + x_d), on the Deviation chart x_d; a forced exit's the spread and deviation on the mids of the newest
# sample in the minute before it.
# 4.4: an entry's batch reads its end state from its lot (lot.end_state of a thin-leg-first batch, read through to_jsonb so
# that a database whose program has not added the column yet still answers; for a batch of both legs together the void
# reason): built and built partial are entries; entry_failed (the thin leg filled nothing) and a void no_fill are Failed
# Entries (the grey triangle of 4.3's undone entry); unwound at a profit or at the stop, and 4.3's undone entry (one_leg: a
# leg filled and was trimmed back), are Unwound Entries. Its pairing facts come from its lot's pair decisions: the end and
# why (the risk exit's reason first: window, stop_d, too_small; else profit, stopping, forced), the ideally paired spread
# that ended it (or at the end), the thick leg's VWAP cap P* and how long the pairing took from the thin leg's first fill. An
# exit's stage (detail.line: exit1 the half, exit2 the rest, exit the one line), whether its pre-check passed (detail.d1:
# false; true = it exits below break-even all the same, D1 of core/SPEC.md 12.7), how its thick leg closed (the end of its
# exit_pair steps: within the bound, or a hard close at exit_pair_window or the spread stop, D2), and the round's expected
# net at the decision against the actual one after the batch: the first is the loss the program accepted, their difference
# what the execution cost.
DECISION_KINDS = ("entry", "exit", "forced")   # 4.3: completions, trims and remaining-leg exits are in their step's marker
ACTIONS = {1: "Long Entry", 2: "Short Entry", 3: "Long Exit", 4: "Short Exit", 5: "Forced Long Exit", 11: "Forced Short Exit",
           6: "Failed Long Entry", 7: "Failed Short Entry", 12: "Unwound Long Entry", 13: "Unwound Short Entry"}
UNWOUND = WARN   # 4.4: an unwound batch, amber, apart from the grey failed entry
ACTION_COLORS = {"Long Entry": GOOD, "Short Entry": BAD, "Long Exit": GOOD, "Short Exit": BAD, "Forced Long Exit": GOOD, "Forced Short Exit": BAD,
                 "Failed Long Entry": GREY, "Failed Short Entry": GREY, "Unwound Long Entry": UNWOUND, "Unwound Short Entry": UNWOUND}
UNWOUND_ENDS = ("unwound_profit", "unwound_stop", "one_leg")
BATCH_ENDS = {"built": "Built", "built_partial": "Built Partial", "entry_failed": "Failed · No Thin Fill", "unwound_profit": "Unwound at a Profit",
              "unwound_stop": "Unwound at the Stop", "no_fill": "No Fill", "one_leg": "One Leg, Trimmed Back", "store_error": "Not Recorded",
              "pairing": "Pairing Now"}
BATCH_WHY = {"window": "Pair Window Over", "stop_d": "Ideal Spread Stop", "too_small": "Too Small to Pair", "profit": "Thin Leg Closed at a Profit",
             "stopping": "Program Stopping", "forced": "Forced Exit"}
EXIT_STAGES = {"exit1": "Exit 1 · Half", "exit2": "Exit 2 · The Rest", "exit": "One Line · All"}
PRECHECKS = {"passed": "Passed", "failed": "Failed · Exits Anyway"}
THICK_CLOSES = {"bound": "Within Break-Even", "exit_window": "Hard Close · Window", "exit_stop_d": "Hard Close · Stop",
                "stopping": "Handed to the Stop", "forced": "Handed to Forced Exit", "no_fill": "No Thin Fill"}


def coded_word(x, words):
    """SQL: the code (1, 2, ...) of the word x in words (dict order), 0 (hidden in the tooltip) for none."""
    return "CASE %s %s ELSE 0 END" % (x, " ".join("WHEN '%s' THEN %d" % (k, i + 1) for i, k in enumerate(words)))


def word_codes(words, colors=None):
    return code_texts({i + 1: w for i, w in enumerate(words.values())}, colors)


MARKERS = sql("""
m0 AS (
  SELECT d.ts_ms, d.run_id, (d.detail ->> 'x_d')::float8 AS x, (d.detail ->> 'mu_d')::float8 AS mu,
         CASE WHEN d.kind = 'entry' AND l.status = 'void' AND l.es IN @UNWOUND@ THEN CASE d.direction WHEN 'short' THEN 13 ELSE 12 END
              WHEN d.kind = 'entry' AND l.status = 'void' THEN CASE d.direction WHEN 'short' THEN 7 ELSE 6 END
              WHEN d.kind = 'entry' THEN CASE d.direction WHEN 'short' THEN 2 ELSE 1 END
              WHEN d.kind = 'exit' THEN CASE d.direction WHEN 'short' THEN 4 ELSE 3 END
              ELSE CASE d.direction WHEN 'short' THEN 11 ELSE 5 END END AS action,
         CASE WHEN c.status = 'open' THEN @OPEN@ WHEN NOT c.funding_known THEN @PENDING@ ELSE c.net END AS cycle_net, c.opened_ms AS exec_ms,
         coalesce(d.detail -> 'client_order_ids', jsonb_build_array(d.detail ->> 'client_order_id')) AS ids,
         d.live, d.cycle_id, d.lot_id, d.kind, d.detail,
         CASE WHEN d.kind = 'entry' THEN coalesce(l.es, CASE WHEN d.detail ? 'thin_leg' AND l.status = 'building' THEN 'pairing' END) END AS batch_end,
         (SELECT min(n.ts_ms) FROM trade.decision n WHERE n.live = d.live AND n.cycle_id = d.cycle_id AND n.kind IN ('exit', 'forced') AND n.ts_ms > d.ts_ms) AS next_ms
  FROM trade.decision d
  LEFT JOIN LATERAL (SELECT x.status, coalesce(to_jsonb(x) ->> 'end_state', CASE WHEN x.status = 'void' THEN x.void_reason END) AS es
                     FROM trade.lot x WHERE x.live = d.live AND x.lot_id = d.lot_id) l ON true
  LEFT JOIN trade.cycle c ON c.live = d.live AND c.cycle_id = d.cycle_id
  WHERE @RUN_D@ AND d.kind IN @KINDS@ AND d.ts_ms >= @CHART_FROM@ AND d.ts_ms <= @CHART_TO@),
m AS (
  SELECT m0.ts_ms, m0.run_id, m0.action, m0.cycle_net, m0.exec_ms, coalesce(m0.mu + m0.x, n.mid) AS ys, coalesce(m0.x, n.dev) AS yd, o.*,
         @BATCH_END@ AS batch_end, @BATCH_WHY@ AS batch_why, pf.d_ideal, pf.cap, pf.paired_ms,
         CASE WHEN m0.kind = 'exit' THEN @STAGE@ ELSE 0 END AS stage,
         CASE WHEN m0.kind = 'exit' AND m0.detail ? 'd1' THEN CASE WHEN (m0.detail ->> 'd1')::boolean THEN 2 ELSE 1 END ELSE 0 END AS precheck,
         CASE WHEN ef.end_action IS NULL THEN 0 WHEN ef.end_action = 'no_fill' THEN @NO_FILL_CLOSE@ ELSE @THICK@ END AS thick_close,
         CASE WHEN m0.kind = 'exit' THEN (m0.detail ->> 'expected_net')::numeric END AS expected, ef.actual
  FROM m0
  LEFT JOIN LATERAL (SELECT s.mid_d AS mid, s.mid_d - s.mu_d AS dev FROM trade.signal_sample s
                     WHERE s.ts_ms <= m0.ts_ms AND s.ts_ms > m0.ts_ms - 60000 AND @RUN_S@ ORDER BY s.ts_ms DESC LIMIT 1) n ON m0.x IS NULL OR m0.mu IS NULL
  LEFT JOIN LATERAL (
    SELECT (array_agg(nullif(f.reason, '') ORDER BY f.detail ->> 'action' = 'risk_exit' DESC, f.ts_ms DESC)
              FILTER (WHERE f.detail ->> 'action' IN ('risk_exit', 'done') AND nullif(f.reason, '') IS NOT NULL))[1] AS why,
           (array_agg((f.detail ->> 'd_ideal')::float8 ORDER BY f.detail ->> 'action' = 'risk_exit' DESC, f.ts_ms DESC)
              FILTER (WHERE f.detail ->> 'action' IN ('risk_exit', 'done') AND f.detail ? 'd_ideal'))[1] AS d_ideal,
           (array_agg((f.detail ->> 'vwap_cap')::numeric ORDER BY f.ts_ms DESC) FILTER (WHERE (f.detail ->> 'vwap_cap')::numeric > 0))[1] AS cap,
           max(f.ts_ms) FILTER (WHERE f.detail ->> 'action' = 'done') - min((f.detail ->> 'first_fill_ms')::bigint) AS paired_ms
    FROM trade.decision f WHERE f.live = m0.live AND f.lot_id = m0.lot_id AND f.kind = 'pair') pf ON m0.kind = 'entry'
  LEFT JOIN LATERAL (
    SELECT (array_agg(f.detail ->> 'action' ORDER BY f.ts_ms DESC) FILTER (WHERE f.detail ->> 'action' IN ('done', 'no_fill', 'hand_over')))[1] AS end_action,
           (array_agg(nullif(f.reason, '') ORDER BY f.ts_ms DESC) FILTER (WHERE f.detail ->> 'action' IN ('done', 'no_fill', 'hand_over')))[1] AS hard,
           (array_agg((f.detail ->> 'actual_net')::numeric ORDER BY f.ts_ms DESC) FILTER (WHERE f.detail ? 'actual_net'))[1] AS actual
    FROM trade.decision f WHERE f.live = m0.live AND f.cycle_id = m0.cycle_id AND f.kind = 'exit_pair' AND f.ts_ms >= m0.ts_ms
      AND f.ts_ms < coalesce(m0.next_ms, @NO_END@)) ef ON m0.kind = 'exit'
  CROSS JOIN LATERAL (
    SELECT sum(o.cum_exec_qty) FILTER (WHERE o.leg = 'bull' AND NOT o.undo) AS q_bull, sum(o.cum_exec_value) FILTER (WHERE o.leg = 'bull' AND NOT o.undo) AS v_bull,
           sum(o.cum_exec_qty) FILTER (WHERE o.leg = 'bear' AND NOT o.undo) AS q_bear, sum(o.cum_exec_value) FILTER (WHERE o.leg = 'bear' AND NOT o.undo) AS v_bear,
           count(*) FILTER (WHERE o.leg = 'bull' AND NOT o.undo) AS n_bull, count(*) FILTER (WHERE o.leg = 'bear' AND NOT o.undo) AS n_bear,
           sum(o.cum_exec_qty) FILTER (WHERE o.leg = 'bull' AND o.undo) AS t_bull, sum(o.cum_exec_qty) FILTER (WHERE o.leg = 'bear' AND o.undo) AS t_bear,
           sum(o.fee) AS fee, bool_or(o.cum_exec_qty > 0 AND NOT o.fee_known) AS fee_pending
    FROM (SELECT o.*, o.purpose = 'trim' AS undo FROM trade.cex_order o WHERE @RUN_O@ AND (o.client_order_id IN (SELECT jsonb_array_elements_text(m0.ids))
      OR m0.kind = 'entry' AND o.lot_id = m0.lot_id
      OR o.client_order_id IN (
        SELECT f.detail ->> 'client_order_id' FROM trade.decision f
        WHERE f.live = m0.live AND f.cycle_id = m0.cycle_id AND (f.kind IN ('trim', 'complete') AND (f.detail ->> 'batch_sent_ms')::bigint = m0.ts_ms
          OR m0.kind = 'exit' AND f.kind = 'exit_pair' AND f.ts_ms >= m0.ts_ms AND f.ts_ms < coalesce(m0.next_ms, @NO_END@))))) o) o)""",
              RUN_D=of_run("d"), RUN_S=of_run("s"), RUN_O=of_mode("o"), CHART_FROM=CHART_FROM, CHART_TO=CHART_TO, KINDS="(%s)" % ", ".join("'%s'" % k for k in DECISION_KINDS),
              OPEN=str(OPEN), PENDING=str(PENDING), UNWOUND="(%s)" % ", ".join("'%s'" % k for k in UNWOUND_ENDS), NO_END=NO_END_MS,
              BATCH_END=coded_word("m0.batch_end", BATCH_ENDS), BATCH_WHY=coded_word("pf.why", BATCH_WHY), STAGE=coded_word("m0.detail ->> 'line'", EXIT_STAGES),
              NO_FILL_CLOSE=str(list(THICK_CLOSES).index("no_fill") + 1),
              THICK=coded_word("coalesce(CASE WHEN ef.end_action = 'hand_over' THEN coalesce(ef.hard, 'stopping') ELSE ef.hard END, 'bound')", THICK_CLOSES))


# The tooltip rows of a decision: on every row within about 13 px of it (WINDOW_MS) the decision c nearest the row, 0 elsewhere,
# which the tooltip hides: hovering near a marker shows it, and nowhere else does a decision show. Decided is its instant.
def near(x):
    """A tooltip row of the decision near a row: 0 (hidden) where there is none."""
    return "CASE WHEN c.n IS NULL THEN 0 ELSE %s END" % x


def filled(leg):
    """A leg's filled contracts in the marker's step; NOT_IN where the step sent no order on that leg (a one-leg trim, completion
    or exit of the remaining leg): never a 0 for a leg another order filled."""
    return near("CASE WHEN c.n_{l} = 0 THEN {no} ELSE {q} END".format(l=leg, no=NOT_IN, q=nonzero("coalesce(c.q_%s, 0)" % leg, 2)))


def avg_price(leg):
    return near("CASE WHEN c.n_{l} = 0 THEN {ni} WHEN c.q_{l} > 0 THEN {p} ELSE {no} END".format(
        l=leg, p=shown("c.v_%s / c.q_%s" % (leg, leg), TICK_DECIMALS[leg]), no=NO_FILL, ni=NOT_IN))


def maybe(x, decimals):
    """A figure a decision may not have: hidden (0) without it, a real zero as ZERO."""
    return near("CASE WHEN %s IS NULL THEN 0 ELSE %s END" % (x, nonzero(x, decimals)))


MARKER_INFO = [("Execution", near("coalesce(c.exec_ms, %d)" % NO_SAMPLE)), ("Action", near("c.action")), ("Decided", near("c.ts_ms")),
               ("Batch End", near("c.batch_end")), ("Batch Why", near("c.batch_why")),
               ("Exit Stage", near("c.stage")), ("Pre-Check", near("c.precheck")), ("Thick Leg Close", near("c.thick_close")),
               ("Bull Filled", filled("bull")), ("Bull Avg", avg_price("bull")), ("Bear Filled", filled("bear")), ("Bear Avg", avg_price("bear")),
               ("Bull Trimmed", near(shown("coalesce(c.t_bull, 0)", 2))), ("Bear Trimmed", near(shown("coalesce(c.t_bear, 0)", 2))),   # 0: hidden
               ("Bull Cap", maybe("c.cap", 3)), ("Ideal Spread", maybe("c.d_ideal", 1)), ("Paired In", maybe("c.paired_ms / 1000.0", 2)),
               ("Notional", near(nonzero("coalesce(c.v_bull, 0) + coalesce(c.v_bear, 0)", 2))),
               ("Fees", near("CASE WHEN c.fee_pending THEN %d ELSE %s END" % (FEE_PENDING, nonzero("coalesce(c.fee, 0)", 2)))),
               ("Expected Net", maybe("c.expected", 2)), ("Actual Net", maybe("c.actual", 2)), ("Execution Cost", maybe("c.actual - c.expected", 2)),
               ("Cycle Net", near("CASE WHEN c.cycle_net <= %d THEN c.cycle_net ELSE %s END" % (PENDING, nonzero("c.cycle_net", 2)))),
               ("At Spread", near("CASE WHEN c.ys IS NULL THEN %d ELSE %s END" % (NO_SAMPLE, nonzero("c.ys", 1)))),
               ("At Deviation", near("CASE WHEN c.yd IS NULL THEN %d ELSE %s END" % (NO_SAMPLE, nonzero("c.yd", 1))))]
WINDOW_MS = "greatest(x.step, ($__to - $__from) / 90)"

# The markers (owner items 81, 84; 4.4): a long entry a green triangle pointing up, a short entry a red one pointing down, a
# failed entry grey and an unwound one amber (the same direction); an exit a circle in the colour of the position it closes.
# Grafana draws points as circles only, so a triangle is drawn as a filled band (fillBelowTo) between two series around the
# decision's place: a flat side (Base) and a peak that rises from it to the apex at the decision's instant, half a width
# (x.half, about 9 px) either side; its height an 18th of the chart's span in the window (x.hs on Spread, x.hd on Deviation),
# so it reads alike at every zoom. The rows at the decision's instant ± half a width carry its corners. Triangles never
# overlap (one series cannot draw two at once): an entry within a triangle's width (2 × x.half) after the start of the one
# before it is drawn by that one (tri), its own tooltip there as for every decision; zooming in separates them. Entries come
# first: a failed or unwound entry is drawn only where no entry's triangle is within a width of it (4.3: an undone one hid the
# real entry 5 s after it).
def triangle(y, h, up):
    """The peak of the triangle e at y (its height h) on a row p: y ∓ h/2 at its corners, y ± h/2 at its apex."""
    sign = "+" if up else "-"
    return ("CASE WHEN abs(p.ts - e.ts_ms) <= x.half THEN {y} {o} {h} / 2 {s} {h} * (1 - abs(p.ts - e.ts_ms)::float8 / x.half) END"
            ).format(y=y, h=h, s=sign, o="-" if up else "+")


def base(y, h, up):
    return "CASE WHEN abs(p.ts - e.ts_ms) <= x.half THEN %s %s %s / 2 END" % (y, "-" if up else "+", h)


def glyphs(chart, y, h):
    """The marker columns of one chart (owner items 81, 84): six triangles (peak and base) and two circles."""
    out = []
    for name, act, up in TRIANGLES:
        out += [("%s%s" % (name, chart), "CASE WHEN e.action = %d THEN %s END" % (act, shown(triangle(y, h, up), 2))),
                ("%s · Base%s" % (name, chart), "CASE WHEN e.action = %d THEN %s END" % (act, shown(base(y, h, up), 2)))]
    for name, acts in CIRCLES:
        out.append(("%s%s" % (name, chart), "CASE WHEN p.mark AND p.kk = 0 AND c.action IN (%s) THEN %s END" % (", ".join(map(str, acts)), shown(y.replace("e.", "c."), 2))))
    return out


# The triangles: entries (green ▲ Long, red ▼ Short), failed entries (grey: the thin leg filled nothing; with both legs
# together, a void no-fill lot) and unwound entries (amber: a leg filled and was closed back, at a profit or at the stop; with
# both legs together 4.3's undone entry), both in the entry's direction; the circles: exits in the colour of the position they
# close (owner item 84: closing a Short red, closing a Long green; a forced exit too, its tooltip says Forced).
TRIANGLES = (("Long Entry", 1, True), ("Short Entry", 2, False), ("Failed Long Entry", 6, True), ("Failed Short Entry", 7, False),
             ("Unwound Long Entry", 12, True), ("Unwound Short Entry", 13, False))
SECOND_TRIANGLES = (6, 7, 12, 13)   # drawn only where no entry's triangle is
CIRCLES = (("Long Exit", (3, 5)), ("Short Exit", (4, 11)))
SPREAD_GLYPHS, DEVIATION_GLYPHS = glyphs("", "e.ys", "x.hs"), glyphs(" · Dev", "e.yd", "x.hd")
# The drawn lines, in bps (owner items 80, 81; after Combo Spread's Spread and Deviation charts). Spread: the Real Long Spread
# (buying both at their pricing levels, blue) and the Real Short Spread (selling both, orange), the Mean, and the program's
# lines around it: a Long enters when Real Long Spread falls to Mean − entry_in σ (blue dashed) and exits when Real Short Spread
# rises to Mean − entry_out σ (blue dotted); a Short the mirror (orange). Deviation: the same less the Mean, the deviation on the
# mids (purple), ±1 σ and ±3 σ (grey). The lines need sigma; they are computed here, not in the browser (Grafana's binary
# calculation reads a NULL as 0). Real spreads only where the executor trades (EXECUTABLE, owner item 70).
SIGMAS = (0.5, 1, 1.5, 2, 2.5, 3)   # owner item 84: the volatility lines as Combo Spread draws them
# 4.4: the second exit line (exit2, NULL without it) dash-dotted beside the first's dots.
LINES = {"Long In": ("-p.d_in", BLUE, [6, 4]), "Long Out": ("-p.d_out", BLUE, [2, 3]), "Long Out 2": ("-p.exit2", BLUE, [8, 3, 2, 3]),
         "Short In": ("p.d_in", ORANGE, [6, 4]), "Short Out": ("p.d_out", ORANGE, [2, 3]), "Short Out 2": ("p.exit2", ORANGE, [8, 3, 2, 3])}
SIG = "nullif(p.sigma, 0)"
spread_sigma = lambda sg, k: "Mean %s %gσ" % (sg, k)
dev_sigma = lambda sg, k: "%s%gσ" % (sg, k)
LINE_COLUMNS = ([("Real Long Spread", shown("p.rl", 1)), ("Real Short Spread", shown("p.rs", 1)), ("Mean", shown("p.mu", 1))] +
                [(name, "CASE WHEN %s IS NOT NULL THEN %s END" % (SIG, shown("p.mu + %s * p.sigma" % k, 1))) for name, (k, _, _) in LINES.items()] +
                [("Real Long Deviation", shown("p.xl", 1)), ("Real Short Deviation", shown("p.xs", 1))] +
                [(name + " · Dev", "CASE WHEN %s IS NOT NULL THEN %s END" % (SIG, shown("%s * p.sigma" % k, 1))) for name, (k, _, _) in LINES.items()] +
                # owner item 84: the stretch of a real spread at or past its entry line, drawn over it brighter
                [("Real Long at Entry", "CASE WHEN p.xl <= -p.d_in * %s THEN %s END" % (SIG, shown("p.rl", 1))),
                 ("Real Short at Entry", "CASE WHEN p.xs >= p.d_in * %s THEN %s END" % (SIG, shown("p.rs", 1))),
                 ("Real Long Deviation at Entry", "CASE WHEN p.xl <= -p.d_in * %s THEN %s END" % (SIG, shown("p.xl", 1))),
                 ("Real Short Deviation at Entry", "CASE WHEN p.xs >= p.d_in * %s THEN %s END" % (SIG, shown("p.xs", 1)))] +
                [(spread_sigma(sg, k), "CASE WHEN %s IS NOT NULL THEN %s END" % (SIG, shown("p.mu %s %g * p.sigma" % ("+" if sg == "+" else "-", k), 1)))
                 for k in SIGMAS for sg in ("+", "−")] +
                [(dev_sigma(sg, k), "CASE WHEN %s IS NOT NULL THEN %s END" % (SIG, shown("%s%g * p.sigma" % ("" if sg == "+" else "-", k), 1)))
                 for k in SIGMAS for sg in ("+", "−")])
MU_WHY = (("NOT p.anchored", "No Anchor"), ("true", "Warmup"))
X_WHY = (("NOT p.anchored", "No Anchor"), (SIG + " IS NULL", "Warmup"), ("NOT p.booked", "Book Not OK"), ("true", "No Levels"))
TOOLTIP_COLUMNS = [
    ("Real Long Spread · Tooltip", coded("p.rl", 1, X_WHY)), ("Real Short Spread · Tooltip", coded("p.rs", 1, X_WHY)),
    ("Mean · Tooltip", coded("p.mu", 1, MU_WHY)), ("Sigma · Tooltip", coded("p.sigma", 1, MU_WHY)),
    ("Real Long Deviation · Tooltip", coded("p.xl", 1, X_WHY)),
    ("Real Short Deviation · Tooltip", coded("p.xs", 1, X_WHY))]
# The program's answer in the bucket's last second as a code for the tooltip (never 0: hidden there).
# (Grafana does not fill variables in a mapping's text: the legs are Bull and Bear here.)
IDLE_CODES = {100: "Order Sent", **{i + 1: "%s · %s" % (s, w) for i, (_, s, w) in enumerate(IDLE)}, 99: "Blocked · Other"}
STATE_COLUMNS = [("Entry", "CASE WHEN p.idle IS NULL THEN 100 %s ELSE 99 END" % " ".join(
    "WHEN p.idle = '%s' THEN %d" % (c, i + 1) for i, (c, _, _) in enumerate(IDLE)))]
# A decision row that carries no bucket's values (before the range's first point, or of another run than the point before
# it) has no state: its line, tooltip and state columns are hidden there (NULL breaks the lines, 0 hides the tooltip rows).
# Near a decision the tooltip shows the decision only: the point's rows are hidden there too.
ALL_COLUMNS = LINE_COLUMNS + [(n, "CASE WHEN p.anchored IS NULL OR c.n IS NOT NULL THEN 0 ELSE %s END" % q)
                              for n, q in TOOLTIP_COLUMNS + STATE_COLUMNS] + SPREAD_GLYPHS + DEVIATION_GLYPHS + MARKER_INFO
RAW = ["mid", "mu", "sigma", "rl", "rs", "xl", "xs", "d_in", "d_out", "exit2", "anchored", "booked", "idle"]
# The two charts' one query (the Deviation chart shows the Spread chart's result): per step bucket of one run and one session,
# at its last sample; each decision as three rows (its instant and its triangle's two corners) with the values of the
# bucket before it when that is of the same run (so they do not break the lines), and on every row the decision nearest it
# within WINDOW_MS, for the tooltip and the triangles; a row with only its time 1 ms before a point that is more than two
# steps (at least 3 s) after the one before it, or of another session or run (the mean restarts), breaks the lines there.
Q_EXECUTION = sql("""
WITH RECURSIVE @SEL@, x0 AS MATERIALIZED (SELECT @STEP@ AS step),
b AS (
  SELECT max(s.ts_ms) AS ts, s.run_id, s.session_id,
         (array_agg(s.mid_d ORDER BY s.ts_ms DESC))[1] AS mid, (array_agg(s.mu_d ORDER BY s.ts_ms DESC))[1] AS mu,
         (array_agg(s.sigma_d ORDER BY s.ts_ms DESC))[1] AS sigma,
         min(CASE WHEN @EXECUTABLE@ THEN s.mu_d + s.x_long_d END) AS rl, max(CASE WHEN @EXECUTABLE@ THEN s.mu_d + s.x_short_d END) AS rs,
         min(CASE WHEN @EXECUTABLE@ THEN s.x_long_d END) AS xl, max(CASE WHEN @EXECUTABLE@ THEN s.x_short_d END) AS xs,
         coalesce(bool_or(@EXECUTABLE@), false) AS booked,
         (array_agg(s.anchor_kind IS NOT NULL ORDER BY s.ts_ms DESC))[1] AS anchored, (array_agg(s.idle_reason ORDER BY s.ts_ms DESC))[1] AS idle
  FROM trade.signal_sample s CROSS JOIN x0
  WHERE s.ts_ms >= @CHART_FROM@ AND s.ts_ms <= @CHART_TO@ AND @RUN_S@
  GROUP BY s.ts_ms / x0.step, s.run_id, s.session_id),
x AS MATERIALIZED (
  SELECT x0.step, greatest(x0.step, ($__to - $__from) / 130)::bigint AS half,
         greatest(coalesce(max(b.rl) - min(b.rs), 0), 2) / 18 AS hs,
         greatest(coalesce(greatest(max(b.xl), 3 * max(b.sigma)) - least(min(b.xs), -3 * max(b.sigma)), 0), 2) / 18 AS hd
  FROM x0 LEFT JOIN b ON true GROUP BY x0.step),
k AS (
  SELECT b.*, @D_IN@ AS d_in, @D_OUT@ AS d_out, @EXIT2@ AS exit2,
         lead(b.ts) OVER w AS next_ts, lead(b.run_id) OVER w AS next_run, lead(b.session_id) OVER w AS next_session
  FROM b CROSS JOIN sel r
  WINDOW w AS (ORDER BY b.ts)),
@MARKERS@,
mm AS (SELECT m.*, row_number() OVER (ORDER BY m.ts_ms) AS n FROM m),
t0 AS (SELECT mm.ts_ms, mm.action, mm.ys, mm.yd, row_number() OVER (ORDER BY mm.ts_ms, mm.n) AS i FROM mm WHERE mm.action IN (1, 2)),
gl AS (
  SELECT t0.i, t0.ts_ms AS start FROM t0 WHERE t0.i = 1
  UNION ALL
  SELECT t0.i, CASE WHEN t0.ts_ms > gl.start + 2 * x.half THEN t0.ts_ms ELSE gl.start END FROM gl JOIN t0 ON t0.i = gl.i + 1 CROSS JOIN x),
tr AS (SELECT DISTINCT ON (gl.start) t0.ts_ms, t0.action, t0.ys, t0.yd FROM gl JOIN t0 ON t0.i = gl.i AND t0.ts_ms = gl.start ORDER BY gl.start, t0.i),
u0 AS (SELECT mm.ts_ms, mm.action, mm.ys, mm.yd, row_number() OVER (ORDER BY mm.ts_ms, mm.n) AS i FROM mm CROSS JOIN x
       WHERE mm.action IN @SECOND@ AND NOT EXISTS (SELECT 1 FROM tr WHERE abs(tr.ts_ms - mm.ts_ms) <= 2 * x.half)),
ul AS (
  SELECT u0.i, u0.ts_ms AS start FROM u0 WHERE u0.i = 1
  UNION ALL
  SELECT u0.i, CASE WHEN u0.ts_ms > ul.start + 2 * x.half THEN u0.ts_ms ELSE ul.start END FROM ul JOIN u0 ON u0.i = ul.i + 1 CROSS JOIN x),
tri AS (SELECT * FROM tr UNION ALL
        SELECT * FROM (SELECT DISTINCT ON (ul.start) u0.ts_ms, u0.action, u0.ys, u0.yd FROM ul JOIN u0 ON u0.i = ul.i AND u0.ts_ms = ul.start
                       ORDER BY ul.start, u0.i) ug),
u AS (
  SELECT k.ts, false AS mark, 0 AS kk, NULL::bigint AS n, k.run_id AS run, @RAW_K@ FROM k
  UNION ALL
  SELECT mm.ts_ms + o.kk * x.half, true, o.kk, mm.n, mm.run_id, @NULL_RAW@ FROM mm CROSS JOIN x CROSS JOIN (VALUES (-1), (0), (1)) o(kk)),
g AS (SELECT u.ts, u.mark, u.kk, u.n, @WIN_RAW@, max(u.n) FILTER (WHERE u.kk = 0) OVER (ORDER BY u.ts, u.mark ROWS UNBOUNDED PRECEDING) AS pn
      FROM (SELECT u.*, max(u.run) FILTER (WHERE NOT u.mark) OVER v AS brun FROM
              (SELECT u.*, count(*) FILTER (WHERE NOT u.mark) OVER (ORDER BY u.ts, u.mark) AS grp FROM u) u
            WINDOW v AS (PARTITION BY u.grp)) u
      WINDOW v AS (PARTITION BY u.grp)),
h AS (SELECT g.*, CASE WHEN g.mark THEN g.n
                       WHEN a.ts_ms IS NOT NULL AND g.ts - a.ts_ms <= @WINDOW@ AND (b.ts_ms IS NULL OR g.ts - a.ts_ms <= b.ts_ms - g.ts) THEN a.n
                       WHEN b.ts_ms IS NOT NULL AND b.ts_ms - g.ts <= @WINDOW@ THEN b.n END AS cn
      FROM g CROSS JOIN x LEFT JOIN mm a ON a.n = g.pn LEFT JOIN mm b ON b.n = coalesce(g.pn, 0) + 1)
SELECT to_timestamp(t.ts / 1000.0) AS time, @NAMES@ FROM (
  SELECT p.ts, CASE WHEN p.mark THEN 2 ELSE 1 END AS ord, @COLUMNS@ FROM h p CROSS JOIN x LEFT JOIN mm c ON c.n = p.cn LEFT JOIN tri e ON abs(p.ts - e.ts_ms) <= x.half
  UNION ALL
  SELECT greatest(k.ts + 1, k.next_ts - 1), 0, @NULLS@ FROM k CROSS JOIN x
  WHERE k.next_ts IS NOT NULL AND (k.next_ts - k.ts > greatest(2 * x.step, 3000) OR k.next_run <> k.run_id OR k.next_session <> k.session_id)) t
ORDER BY t.ts, t.ord""", SEL=SEL, SECOND="(%s)" % ", ".join(map(str, SECOND_TRIANGLES)), STEP=STEP_MS, RUN_S=of_run("s"), CHART_FROM=CHART_FROM, CHART_TO=CHART_TO, D_IN=D_IN, D_OUT=D_OUT, EXIT2=EXIT2, MARKERS=MARKERS, EXECUTABLE=EXECUTABLE.format(s="s"),
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
            WHEN @DIVIDEND@ IS NOT NULL THEN 'a dividend takes effect (anchor less the dividend; read for the session: ' || @DIVIDEND@ || ' USD)'
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
    return go_dur(cfg(path))


def cfg_num(path, suffix=""):
    return "trim_scale((%s)::numeric)::text || '%s'" % (cfg(path), suffix)


def ordinal(path):
    return ("%s || CASE WHEN %s IN ('11', '12', '13') THEN 'th' WHEN right(%s, 1) = '1' THEN 'st' WHEN right(%s, 1) = '2' THEN 'nd' "
            "WHEN right(%s, 1) = '3' THEN 'rd' ELSE 'th' END") % ((cfg(path),) * 5)


SETTINGS = [
    ("Signal and Sizing", [
        ("Taker Fee", cfg_num("strategy,taker_fee_bps", " bps")),
        ("Cost Margin", cfg_num("strategy,cost_margin_d", " bps")),
        ("Rewarm", "CASE WHEN %s = 'true' THEN 'Yes' ELSE 'No' END" % cfg("strategy,rewarm_on_anchor_change")),
        ("Min Batch", cfg_num("strategy,min_batch_notional", " U")),
        ("Take Ratio", cfg_num("strategy,take_ratio")),
        ("Entry Spacing", dur("strategy,entry_spacing")),
        ("Entry Step", cfg_num("strategy,entry_step_d", " bps"))]),
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
        ("NAV Watch", dur("cex,nav_watch")),
        ("NAV Hot Switch", cfg_num("cex,nav_hot_switch_d", " bps")),
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
SETTINGS_WIDTHS = {"Signal and Sizing": 6, "Execution": 6, "Risk & Session": 6, "Run": 6}   # one line of four (4.2: Parameters took the rest)


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
# 4.4: a lot's end: lot.end_state of a thin-leg-first batch (through to_jsonb: a database without the column yet answers too),
# else its void reason, building while it is built, built otherwise (both legs together).
LOT_END = ("coalesce(to_jsonb({x}) ->> 'end_state', CASE {x}.status WHEN 'void' THEN coalesce({x}.void_reason, 'no_fill') WHEN 'building' THEN 'building' "
           "ELSE 'built' END)")
# How a lot ended, in words (Batch Ends, the Batches of a round trip).
BATCH_WORDS = {"built": "Built", "built_partial": "Built Partial", "unwound_profit": "Unwound at a Profit", "unwound_stop": "Unwound at the Stop",
               "one_leg": "Unwound: One Leg Trimmed Back", "entry_failed": "Entry Failed", "no_fill": "No Fill", "building": "Pairing Now",
               "store_error": "Not Recorded"}
Q_BLOTTER = or_say(sql("""
WITH @SEL@
SELECT c.cycle_id, to_timestamp(c.opened_ms / 1000.0) AS "Opened", to_timestamp(c.closed_ms / 1000.0) AS "Closed", @HELD@ AS "Held",
       initcap(c.direction) AS "Side", l.notional AS "Notional", @ENTRY@ AS "In σ", @EXIT@ AS "Out σ", @NET@ AS "Net",
       CASE WHEN c.status <> 'open' AND c.funding_known AND l.notional > 0 THEN 10000 * c.net / l.notional END AS "Net bps",
       CASE WHEN c.end_reason = 'one_leg' AND bt.thin THEN 'Nothing Paired' ELSE @REASON@ END AS "End Reason"
FROM trade.cycle c
CROSS JOIN LATERAL (SELECT bool_or(to_jsonb(x) ->> 'end_state' IS NOT NULL) AS thin FROM trade.lot x WHERE x.live = c.live AND x.cycle_id = c.cycle_id) bt
CROSS JOIN LATERAL (SELECT sum(x.entry_value_bull + x.entry_value_bear) AS notional,
                           sum(x.x_d / nullif(x.sigma_d, 0) * (x.entry_value_bull + x.entry_value_bear))
                             / nullif(sum(x.entry_value_bull + x.entry_value_bear) FILTER (WHERE x.sigma_d <> 0), 0) AS entry
                    FROM trade.lot x WHERE x.live = c.live AND x.cycle_id = c.cycle_id) l
LEFT JOIN LATERAL (SELECT (d.detail ->> 'x_d')::float8 / nullif((d.detail ->> 'sigma_d')::float8, 0) AS x FROM trade.decision d
                   WHERE d.live = c.live AND d.cycle_id = c.cycle_id AND d.kind = 'exit' ORDER BY d.ts_ms DESC LIMIT 1) e ON true
WHERE @TRADED@
ORDER BY c.opened_ms DESC LIMIT 500""", SEL=SEL, TRADED=TRADED, HELD=span("coalesce(c.closed_ms, %s) - c.opened_ms" % NOW_MS),
                ENTRY=shown("l.entry", 2), EXIT=shown("e.x", 2), NET=NET_CODED, REASON=end_reason("c.end_reason")),
                   ['cycle_id', 'Opened', 'Closed', 'Held', 'Side', 'Notional', 'In σ', 'Out σ', 'Net', 'Net bps', 'End Reason'], 'End Reason', 'No round trip in the Range')
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


# 4.4: the entry batches of the picked round trip, one row per lot in its order: when it opened, how it ended (BATCH_WORDS)
# and why (its pair decisions: the risk exit's reason first), each leg's contracts it opened (entries, top-ups, pairing
# orders) and those closed back (trims: unwinds and profit closes), how long the pairing took and the lot's net (USDT,
# Open while it holds a position).
Q_BATCH_ROWS = or_say(sql("""
WITH @SEL@
SELECT 'Batch ' || l.seq AS "Batch", to_timestamp(l.opened_ms / 1000.0) AS "Opened", @END@ AS "End", @WHY@ AS "Why",
       o.q_bull AS "Bull", o.q_bear AS "Bear", o.back AS "Back", p.ms / 1000.0 AS "Pairing",
       CASE WHEN l.status IN ('closed', 'void') THEN l.net ELSE @OPEN@ END AS "Net"
FROM trade.cycle c JOIN trade.lot l ON l.live = c.live AND l.cycle_id = c.cycle_id
CROSS JOIN LATERAL (SELECT sum(o.cum_exec_qty) FILTER (WHERE o.leg = 'bull' AND o.purpose IN ('entry', 'complete')) AS q_bull,
                           sum(o.cum_exec_qty) FILTER (WHERE o.leg = 'bear' AND o.purpose IN ('entry', 'complete')) AS q_bear,
                           sum(o.cum_exec_qty) FILTER (WHERE o.purpose = 'trim') AS back
                    FROM trade.cex_order o WHERE o.live = l.live AND o.lot_id = l.lot_id) o
LEFT JOIN LATERAL (SELECT max(f.ts_ms) FILTER (WHERE f.detail ->> 'action' = 'done') - min((f.detail ->> 'first_fill_ms')::bigint) AS ms,
                          (array_agg(nullif(f.reason, '') ORDER BY f.detail ->> 'action' = 'risk_exit' DESC, f.ts_ms DESC)
                             FILTER (WHERE f.detail ->> 'action' IN ('risk_exit', 'done') AND nullif(f.reason, '') IS NOT NULL))[1] AS why
                   FROM trade.decision f WHERE f.live = l.live AND f.lot_id = l.lot_id AND f.kind = 'pair') p ON true
WHERE @RUN_C@ AND c.cycle_id = @CYCLE@ ORDER BY l.seq""", SEL=SEL, CYCLE="'$cycle'", OPEN=str(OPEN), RUN_C=cycle_of_run("c"),
                          END=plain(LOT_END.format(x="l"), BATCH_WORDS), WHY="CASE p.why %s ELSE initcap(replace(p.why, '_', ' ')) END" % " ".join(
                              "WHEN '%s' THEN '%s'" % kv for kv in BATCH_WHY.items())),
                      ['Batch', 'Opened', 'End', 'Why', 'Bull', 'Bear', 'Back', 'Pairing', 'Net'], 'Why', 'No batch in this round trip')


# 4.4: the step each order was sent for (item 5 of step 10.9: with the thin leg first the thick leg's pairing orders are
# purpose complete and the unwinds and profit closes of the unpaired thin leg trim): the decision that names it (its first),
# the batch (lot seq) of an entry's orders: Batch 2 · Thin Leg, · Top-Up, · Pair, · Unwind, · Profit Close; Exit 1 · Thin,
# Exit 1 · Thick, Exit 2 · Hard Close; with both legs together Entry, Complete, Trim, Exit, Forced.
LINE_WORD = "CASE k.line WHEN 'exit1' THEN 'Exit 1' WHEN 'exit2' THEN 'Exit 2' ELSE 'Exit' END"
ORDER_STEP = sql("""coalesce('Batch ' || l.seq || ' · ', '') || CASE
  WHEN k.kind = 'entry' AND k.thin THEN 'Thin Leg' WHEN k.kind = 'entry' THEN 'Entry'
  WHEN k.kind = 'pair' THEN CASE k.action WHEN 'send_thick' THEN 'Pair' WHEN 'send_thin' THEN 'Top-Up' WHEN 'close_thin' THEN 'Profit Close'
                                          WHEN 'unwind' THEN 'Unwind' ELSE initcap(replace(k.action, '_', ' ')) END
  WHEN k.kind = 'exit' THEN @LINE@ || CASE WHEN k.thin THEN ' · Thin' ELSE '' END
  WHEN k.kind = 'exit_pair' THEN @LINE@ || CASE k.action WHEN 'hard_close' THEN ' · Hard Close' ELSE ' · Thick' END
  ELSE initcap(replace(coalesce(k.kind, o.purpose), '_', ' ')) END""", LINE=LINE_WORD)
Q_ORDERS = or_say(sql("""
WITH @SEL@
SELECT to_timestamp(o.submitted_ms / 1000.0) AS "Time", initcap(o.purpose) AS "Purpose", @STEP@ AS "Step", CASE o.leg WHEN 'bull' THEN '$bull' ELSE '$bear' END AS "Leg",
       initcap(o.side) || CASE WHEN o.reduce_only THEN ' · RO' ELSE '' END AS "Side", o.qty AS "Qty", o.cum_exec_qty AS "Filled",
       @LIMIT@ AS "Limit", @AVG@ AS "Avg", CASE WHEN o.cum_exec_qty = 0 THEN NULL WHEN NOT o.fee_known THEN @FEE_PENDING@ ELSE o.fee END AS "Fee",
       o.final_ms - o.sent_ms AS "Latency",
       CASE WHEN o.unknown THEN 'Unknown' WHEN o.status = 'filled' THEN 'Filled' WHEN o.status = 'cancelled' AND o.cum_exec_qty > 0 THEN 'Partial'
            WHEN o.status = 'cancelled' THEN 'No Fill' WHEN o.status IN ('pending', 'acked') THEN 'Working'
            ELSE initcap(replace(o.status, '_', ' ')) END || coalesce(' · ' || nullif(concat_ws(' ', o.error_code, o.error_message), ''), '') AS "Status"
FROM trade.cex_order o
LEFT JOIN LATERAL (SELECT k.kind, k.detail ->> 'action' AS action, k.detail ->> 'line' AS line, k.detail ? 'thin_leg' AS thin FROM trade.decision k
                   WHERE k.live = o.live AND k.cycle_id = o.cycle_id
                     AND (k.detail ->> 'client_order_id' = o.client_order_id OR k.detail -> 'client_order_ids' ? o.client_order_id)
                   ORDER BY k.ts_ms LIMIT 1) k ON true
LEFT JOIN trade.lot l ON l.live = o.live AND l.lot_id = o.lot_id
WHERE @RUN_O@ AND o.cycle_id = @CYCLE@ ORDER BY o.submitted_ms, o.client_order_id""",
               SEL=SEL, RUN_O=of_mode("o"), CYCLE=CYCLE, FEE_PENDING=str(FEE_PENDING), LIMIT=leg_price("o.price"), AVG=leg_price("o.avg_price"), STEP=ORDER_STEP),
                  ['Time', 'Purpose', 'Step', 'Leg', 'Side', 'Qty', 'Filled', 'Limit', 'Avg', 'Fee', 'Latency', 'Status'], 'Status', 'No order: no round trip in the Range')

# -------------------------------------------------------------------------------------------------------------- Inventory
# Each leg at the Run's newest inventory sample (every 10 s), as the sibling board's Positions Now: the venue's position and
# the ledger's (dust included), the venue's mark and liquidation price (live only), the distance to it in % of the mark,
# the venue's unrealised P&L, the exposure at the newest second's mid (USDT, signed), the newest settled funding rate of the
# mode (bps per settlement), and the sample's age while the Run runs.
RATES = ("SELECT f.settlement_ms, max(f.rate) FILTER (WHERE f.leg = 'bull') AS bull, max(f.rate) FILTER (WHERE f.leg = 'bear') AS bear "
         "FROM trade.funding_rate f WHERE %s GROUP BY f.settlement_ms" % of_mode("f"))
Q_POSITIONS = sql("""
WITH @LATEST@, f AS (@RATES@), n AS (SELECT * FROM f ORDER BY f.settlement_ms DESC LIMIT 1)
SELECT l.leg AS "Leg", l.pos AS "Venue", l.ledger AS "Ledger", l.mark AS "Mark", l.liq AS "Liq Price",
       CASE WHEN l.pos < 0 THEN 100 * (l.liq - l.mark) / nullif(l.mark, 0) WHEN l.pos > 0 THEN 100 * (l.mark - l.liq) / nullif(l.mark, 0) END AS "Liq Distance",
       l.upl AS "Venue UPL", @EXPOSURE@ AS "Exposure", 10000 * l.rate AS "Funding",
       CASE WHEN r.stopped_ms IS NULL THEN (@NOW@ - i.ts_ms) / 1000 END AS "Age"
FROM i JOIN r ON true LEFT JOIN s ON true LEFT JOIN n ON true CROSS JOIN LATERAL (VALUES
  (1, '$bull', i.pos_bull, i.ledger_bull, i.mark_bull, i.liq_bull, i.upl_bull, s.mid_bull, n.bull),
  (2, '$bear', i.pos_bear, i.ledger_bear, i.mark_bear, i.liq_bear, i.upl_bear, s.mid_bear, n.bear)) l(o, leg, pos, ledger, mark, liq, upl, mid, rate)
ORDER BY l.o""", LATEST=LATEST, RATES=RATES, NOW=NOW_MS, EXPOSURE=shown("l.pos * l.mid", 2))

# ---------------------------------------------------------------------------------------------- Diagnostics · Execution
Q_END_REASONS = or_say(sql("""
WITH @SEL@
SELECT @REASON@ AS "End Reason", count(*) AS "Cycles", sum(c.net) FILTER (WHERE c.funding_known) AS "Net",
       count(*) FILTER (WHERE NOT c.funding_known) AS "Pending"
FROM trade.cycle c WHERE @IN_RANGE@ AND c.status <> 'open' GROUP BY c.end_reason ORDER BY count(*) DESC, 1""",
                    SEL=SEL, IN_RANGE=IN_RANGE, REASON=end_reason("c.end_reason")), ['End Reason', 'Cycles', 'Net', 'Pending'], 'End Reason', 'None in the Range')
Q_IDLE = sql("""
WITH @SEL@
SELECT @STATE@ || ' · ' || @WHY@ AS "Answer", count(*) AS "Seconds", 100.0 * count(*) / sum(count(*)) OVER () AS "Share"
FROM trade.signal_sample s WHERE @RANGE_S@ AND @RUN_S@
GROUP BY s.idle_reason ORDER BY count(*) DESC""", SEL=SEL, RANGE_S=in_range("s.ts_ms"), RUN_S=of_run("s"),
             STATE=idle_state("s.idle_reason"), WHY=idle_why("s.idle_reason"))
Q_BLOCKED = or_say(sql("""
WITH @SEL@
SELECT @WHY@ AS "Reason", initcap(d.direction) AS "Side", count(*) AS "Seconds", to_timestamp(max(d.ts_ms) / 1000.0) AS "Last"
FROM trade.decision d WHERE @RUN_D@ AND d.kind = 'at_line_blocked' AND @RANGE_D@
GROUP BY d.reason, d.direction ORDER BY count(*) DESC""", SEL=SEL, RUN_D=of_run("d"), RANGE_D=in_range("d.ts_ms"), WHY=idle_why("d.reason")), ['Reason', 'Side', 'Seconds', 'Last'], 'Reason', 'None in the Range')
LATENCY_STAGES = [("Submit → Sent", "local", "o.sent_ms - o.submitted_ms"), ("Sent → Ack", "local", "o.acked_ms - o.sent_ms"),
                  ("Sent → Final", "local", "o.final_ms - o.sent_ms"), ("Accepted → Created", "venue", "o.exchange_created_ms - o.accepted_ms"),
                  ("Created → Last Update", "venue", "o.exchange_updated_ms - o.exchange_created_ms")]
Q_LATENCY = or_say(sql("""
WITH @SEL@
SELECT k.stage AS "Stage", k.clock AS "Clock", count(k.ms) AS "Orders", percentile_cont(0.5) WITHIN GROUP (ORDER BY k.ms) AS "Median",
       percentile_cont(0.9) WITHIN GROUP (ORDER BY k.ms) AS "P90", max(k.ms) AS "Max"
FROM trade.cex_order o
CROSS JOIN LATERAL (VALUES @STAGES@) k(i, stage, clock, ms)
WHERE @RUN_O@ AND @RANGE_O@ AND k.ms IS NOT NULL
GROUP BY k.i, k.stage, k.clock ORDER BY k.i""", SEL=SEL, RUN_O=of_run("o"), RANGE_O=in_range("o.submitted_ms"),
                STAGES=", ".join("(%d, '%s', '%s', %s)" % (i, s, c, q) for i, (s, c, q) in enumerate(LATENCY_STAGES))), ['Stage', 'Clock', 'Orders', 'Median', 'P90', 'Max'], 'Stage', 'No order in the Range')

# 4.4: how the Run's entry batches opened in the Range ended (the replay's count of end states, core/SPEC.md 12.10): their
# number, their net (the lots' own, closed or void: a void lot's is what its unwind or profit close made), and the median time
# from the thin leg's first fill to the end of the pairing.
Q_BATCHES = or_say(sql("""
WITH @SEL@
SELECT @WORD@ AS "Batch End", count(*) AS "Batches", sum(l.net) FILTER (WHERE l.status IN ('closed', 'void')) AS "Net",
       percentile_cont(0.5) WITHIN GROUP (ORDER BY p.ms) / 1000.0 AS "Pairing s"
FROM trade.lot l
LEFT JOIN LATERAL (SELECT max(f.ts_ms) FILTER (WHERE f.detail ->> 'action' = 'done') - min((f.detail ->> 'first_fill_ms')::bigint) AS ms
                   FROM trade.decision f WHERE f.live = l.live AND f.lot_id = l.lot_id AND f.kind = 'pair') p ON true
WHERE @RUN_L@ AND @RANGE_L@
GROUP BY 1 ORDER BY min(@ORDER@), 1""", SEL=SEL, RUN_L=of_run("l"), RANGE_L=in_range("l.opened_ms"), WORD=plain(LOT_END.format(x="l"), BATCH_WORDS),
                       ORDER="CASE %s %s ELSE 99 END" % (LOT_END.format(x="l"), " ".join("WHEN '%s' THEN %d" % (k, i) for i, k in enumerate(BATCH_WORDS)))),
                   ['Batch End', 'Batches', 'Net', 'Pairing s'], 'Batch End', 'No entry batch in the Range')

# ------------------------------------------------------------------------------------------------- Diagnostics · System
# The Run's events of every module in the Range (trade.event: today the program writes only its errors there), newest first;
# a row that says so when there is none.
EVENT_ROWS = "trade.event e WHERE %s AND %s" % (of_run("e"), in_range("e.ts_ms"))
Q_EVENTS = sql("""
WITH @SEL@
SELECT * FROM (
  SELECT to_timestamp(e.ts_ms / 1000.0) AS "Time", initcap(e.level) AS "Level", e.module AS "Module", e.kind AS "Kind", e.message AS "Message",
         e.detail::text AS "Detail"
  FROM @ROWS@
  ORDER BY e.ts_ms DESC, e.seq DESC LIMIT 200) a
UNION ALL
SELECT NULL, NULL, NULL, NULL, 'No event in the Range', NULL
WHERE NOT EXISTS (SELECT 1 FROM @ROWS@)""", SEL=SEL, ROWS=EVENT_ROWS)
# Every Run of both modes, newest first (the list the Run picker offers): its number and mode, start, stop, state, how long
# it ran, its notional N = M × L (USDT) and where it ran; the one the page shows is marked.
Q_RUNS = sql("""
WITH @SEL@
SELECT @NAME@ || CASE WHEN r.run_id = @RID@ THEN ' · shown' ELSE '' END AS "Run", to_timestamp(r.started_ms / 1000.0) AS "Started",
       to_timestamp(r.stopped_ms / 1000.0) AS "Stopped", initcap(@STATE@) AS "State", @RAN@ AS "Ran",
       (r.config #>> '{inventory,capital}')::numeric * (r.config #>> '{inventory,leverage}')::numeric AS "N",
       r.host AS "Host", r.run_id AS "Run ID", r.started_ms::text AS from_ms, coalesce(r.stopped_ms::text, 'now') AS to_ms
FROM trade.process_run r
ORDER BY r.started_ms DESC LIMIT 100""", SEL=SEL, NAME=run_label("r"), RID=RID, STATE=RUN_STATE,
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
         (SELECT @OPENED@ FROM trade.cycle x WHERE x.live = f.live AND x.cycle_id = f.cycle_id) AS "Execution ID"
  FROM @FUNDING@
  UNION ALL
  SELECT to_timestamp((a ->> 'settlement_ms')::bigint / 1000.0), CASE a ->> 'leg' WHEN 'bull' THEN '$bull' ELSE '$bear' END, 'Awaited', NULL,
         @OPENED_C@
  FROM trade.cycle c CROSS JOIN LATERAL jsonb_array_elements(c.funding_awaited) a
  WHERE @RUN_C@ AND jsonb_typeof(c.funding_awaited) = 'array') t
UNION ALL
SELECT NULL, NULL, NULL, NULL, 'None in the Range, none awaited'
WHERE NOT EXISTS (SELECT 1 FROM @FUNDING@)
  AND NOT EXISTS (SELECT 1 FROM @AWAITED@ AND jsonb_array_length(c.funding_awaited) > 0)
ORDER BY 1 DESC NULLS LAST""", SEL=SEL, FUNDING=FUNDING_ROWS, AWAITED=AWAITED, RUN_C=cycle_of_run("c"),
                OPENED=clock("x.opened_ms", "MM-DD HH24:MI:SS"), OPENED_C=clock("c.opened_ms", "MM-DD HH24:MI:SS"))


# ------------------------------------------------------------------------------------------------------------- the board
# The look of the sibling dex-cex Live Monitor (owner item 73; its SPEC 15.4-15.5 and live_board.py): rows named
# "Section · scope", titles "Noun · Unit" in Title Case, a description of one sentence then one line per figure or column
# (bullets), lowercase words on the status tiles, a status colour only for a status, money green above zero and red below,
# times MM-DD HH:mm:ss in UTC+8, column widths to the longest header word plus its room.
def bullets(intro, items):
    """A panel description in markdown: one sentence, then one line per term (the sibling board's bullets)."""
    return intro + "\n\n" + "\n".join("- **%s**: %s" % (name, text) for name, text in items)


def collapsed_row(title, y, panels):
    """A row shown folded: its panels are queried only when it is opened. Their places count from the row's own."""
    r = row(title, y)
    r["collapsed"] = True
    r["panels"] = panels
    return r


def regex_colors(pairs):
    """Value mappings that colour a text by its start: [(prefix, colour)]."""
    return [{"type": "regex", "options": {"pattern": "^%s.*" % p, "result": {"color": c, "index": i}}} for i, (p, c) in enumerate(pairs)]


def by_regex(pattern, props):
    return {"matcher": {"id": "byRegexp", "options": pattern}, "properties": [{"id": k, "value": v} for k, v in props.items()]}


def word_cells(values, px=None, align="center"):
    """A text column coloured by its value (values: {text: colour}), as the sibling board's mapped()."""
    return dict(custom_cellOptions={"type": "color-text"}, custom_align=align, color={"mode": "fixed", "fixedColor": "text"},
                mappings=[{"type": "value", "options": {v: {"color": c, "index": i} for i, (v, c) in enumerate(values.items())}}],
                **({"custom_width": px} if px else {}))


def status_words(title, q, x, y, w, description):
    """One word per part on a coloured tile (STATUS_WORDS), as the sibling board's status_words."""
    opts = {"reduceOptions": {"calcs": ["lastNotNull"], "fields": "/.*/", "values": False}, "colorMode": "background", "graphMode": "none",
            "textMode": "value_and_name", "justifyMode": "center", "wideLayout": True, "text": {"titleSize": 12, "valueSize": 14},
            "showPercentChange": False, "orientation": "auto"}
    maps = [{"type": "regex", "options": {"pattern": p, "result": {"color": c, "index": i}}} for i, (p, c) in enumerate(STATUS_WORDS)]
    return panel("stat", title, q, x, y, w, 3, description=description, unit="string", options=opts, color={"mode": "thresholds"},
                 thresholds={"mode": "absolute", "steps": [{"color": GREY, "value": None}]}, mappings=maps, no_value="no data")


# The xy charts of the Execution Summary (the sibling board's xy()): the Range's ends set the time axis (configFromData from
# the Period query), so the axis spans the Range however few points there are.
XY_VERSION = "13.2.2"   # an xy chart without a plugin version is read as the pre-11.1 layout, which keeps one y series only
PERIOD_AXIS = [{"id": "configFromData", "options": {"configRefId": "Period", "applyTo": {"id": "byName", "options": "Time"}, "mappings": [
    {"fieldName": "From", "handlerKey": "min", "reducerId": "min"}, {"fieldName": "To", "handlerKey": "max", "reducerId": "max"},
    {"fieldName": "Time", "handlerKey": "__ignore"}, {"fieldName": "Period", "handlerKey": "__ignore"}]}}]
HIDE = {"custom.hideFrom": {"legend": True, "tooltip": True, "viz": True}}


def xy(title, queries, x, y, w, h, description, show, overrides, link=None, no_data="No round trip closed in the Range"):
    p = panel("xychart", title, queries[0][1], x, y, w, h, description=description, unit="none", no_value=no_data,
              custom={"show": show, "pointShape": "circle", "pointSize": {"fixed": 7}, "pointStrokeWidth": 1, "fillOpacity": 70, "lineWidth": 2,
                      "axisPlacement": "auto", "axisBorderShow": False, "hideFrom": {"legend": False, "tooltip": False, "viz": False},
                      "thresholdsStyle": {"mode": "dashed"}},
              options={"mapping": "auto", "series": [{}], "legend": {"displayMode": "list", "placement": "bottom", "showLegend": True, "calcs": []},
                       "tooltip": {"mode": "multi" if show == "lines" else "single", "sort": "desc"}},
              thresholds={"mode": "absolute", "steps": [{"color": "transparent", "value": None}, {"color": GREY, "value": 0}]},
              overrides=list(overrides) + [by_regex("^Time$", {"unit": "time:MM-DD HH:mm", "custom.axisLabel": " "}), by_regex("^(cycle_id|Period)$", HIDE)],
              transformations=PERIOD_AXIS)
    if link:
        p["fieldConfig"]["defaults"]["links"] = [{"title": "Show this round trip's orders", "url": link, "targetBlank": False}]
    p["targets"] = [target(q, name) for name, q in queries]
    p["pluginVersion"] = XY_VERSION
    return p


MONEY = {"unit": "none", "decimals": 2, "thresholds": SIGNED, "mappings": [NO_FIGURE]}
PENDING_TEXT = code_texts({PENDING: "Pending", FEE_PENDING: "Pending", OPEN: "Open"}, {"Pending": WARN, "Open": TEXT})
SIDE_COLORS = regex_colors([("Long", BLUE), ("Short", ORANGE)])
SIGN_BAND = dict(custom_cellOptions={"type": "color-text"}, color={"mode": "thresholds"}, thresholds=SIGNED)


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
                  "on Opened in Round Trips, on a point of Net per Round Trip or on an alert's Source picks one.", hide=2),
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
    link = (URL + "?orgId=1&${__url_time_range}&${run:queryparam}&${since:queryparam}&${since_hour:queryparam}&${until:queryparam}"
            "&${until_hour:queryparam}&${combo:queryparam}&var-cycle=${__data.fields.cycle_id}")

    # ------------------------------------------------------------------ Status · Running Session (owner items 74, 75)
    panels.append(row("Status · Running Session · ${runtext}", y)); y += 1
    panels.append(status_words("Execution", Q_STATUS_EXEC, 0, y, 24, bullets(
        "The picked Run (Newest Live Run by default), at its newest second, whatever the Range. Green well, amber needs a look, red trading "
        "at risk, grey nothing to judge; a time plain.",
        [("Mode", "LIVE (real orders on Bitget) or PAPER (the program trades against its own copy of the books; nothing is sent): a Run is one "
                  "mode, and every panel shows the picked Run only, so paper and live are never merged."),
         ("Run", "up for how long; starting (no sample yet); stale (no sample for more than %d s and no stop recorded); lost (none for a minute: "
                 "crashed or killed); stopped (in order)." % SAMPLE_AGE_S),
         ("Start / End", "when the Run started; for a stopped one, when it stopped; UTC+8."),
         ("Phase", "the US market phase, worded and coloured as on Combo Spread: Pre‑Mkt, Regular, After‑Hrs, Overnight, Closed (weekend or "
                   "holiday); the program enters outside Regular only."),
         ("Books", "ok, or a leg whose book conflicts (snapshot and ticks disagree) or is unusable (too old or thin): no entry goes out on it."),
         ("Orders", "the Run's orders not final: n working, n unknown (being queried), or n left open by a stopped Run."),
         ("Entry", "the program's answer this second: ready (no signal yet) or order sent; the strategy's ordinary answers in grey (regular "
                   "hours, spacing, quota used up, ...); held (waits on a book, F, the venue or a stop); stop (a person is needed).")])))
    y += 3
    panels.append(status_words("Account", Q_STATUS_ACCOUNT, 0, y, 24, bullets(
        "The venue's side of the Run at its newest inventory sample (every 10 s). The program takes no decision from these (PRD 6.6); "
        "the board's own lines colour them.",
        [("Position", "the open round trip's side and how long it has been open; flat (the venue holds nothing on either leg); residual "
                      "(amber): what the venue still holds per leg with no round trip open (dust); left open (red): a stopped Run's."),
         ("Ledger", "match (the venue's positions equal the program's ledger), mismatch (no orders until a person checks) or not read."),
         ("Equity", "Bitget usdtEquity of the WHOLE account, USDT (it also holds the sibling repository's positions); paper: none."),
         ("Maint. Margin", "the account's maintenance margin, USDT: margin ratio × equity."),
         ("Margin Ratio", "Bitget mgnRatio of the whole account (maintenance margin ÷ equity): ok below %d %%, warn below %d %%, high from there."
          % (MARGIN_WARN * 100, MARGIN_HIGH * 100)),
         ("Liquidation", "the nearer leg's liquidation price from its mark, in %% of the mark: ok beyond %d %%, warn beyond %d %%, near within; "
                         "flat without a position; prices in Inventory." % (LIQ_WARN * 100, LIQ_NEAR * 100)),
         ("Funding", "booked to the Run's round trips, USDT (+ received), or the settlements still without a record (n awaited).")])))
    y += 3
    price = {"unit": "none", "decimals": 3, "mappings": [NO_FIGURE]}
    usd = {"unit": "currencyUSD", "decimals": 2, "mappings": [NO_FIGURE]}   # as Combo Spread's anchors: $32.03
    bps1 = {"unit": "suffix: bps", "decimals": 1, "mappings": [NO_FIGURE]}
    hit = lambda c: {"mappings": [NO_FIGURE, {"type": "regex", "options": {"pattern": ".* · Entry$", "result": {"color": c, "index": 0}}}]}
    ENTRY_HIT, ENTRY_HIT_SHORT = hit(BLUE), hit(ORANGE)   # owner item 84: the side at or past its entry line lit
    value_map = lambda colors: {"mappings": [NO_FIGURE, {"type": "value", "options": {k: {"color": c, "index": i} for i, (k, c) in enumerate(colors.items())}}]}
    market = stat_strip("Market · Prices", Q_MARKET, [
        ("Bull Price", "Each leg's mid on the program's book, USDT.", dict(price, displayName="${bull} Price · USDT")),
        ("Bull Anchor", "Each leg's anchor, USD per share: the bell (F × Binance's index at the close), then the issuer's NAV once confirmed.",
         dict(usd, displayName="${bull} Anchor · USD")),
        ("Bear Price", "", dict(price, displayName="${bear} Price · USDT")),
        ("Bear Anchor", "", dict(usd, displayName="${bear} Anchor · USD")),
        ("Anchor", "As Combo Spread's Anchor Status: NAV, Bell (provisional, grey) or Missing (amber: no entries).", value_map(ANCHOR_COLORS)),
        ("F", "USD per USDT (Coinbase USDT-USD), the rate the program converts with: a price in USDT × F is in USD; an anchor in USD ÷ F "
              "is in USDT (owner item 80; Combo Spread's USD/USDT).", {"unit": "none", "decimals": 5, "mappings": [NO_FIGURE], "displayName": "F · USD/USDT"}),
    ], 0, y, 24, 3, intro="The Run's newest second (its age: the Run tile above), as Combo Spread's Prices · Latest Second; whatever the Range.",
        columns=["Bull Price", "Bull Anchor", "Bear Price", "Bear Anchor", "Anchor", "F"], title_size=12, value_size=18)
    panels.append(market)
    y += 3
    panels.append(stat_strip("Market · Real Spread and Signal", None, [
        ("Real Long", "Owner item 80: the spread at both legs' pricing levels buying both (where a Long enters and a Short exits), in "
                      "spread bps: 10⁴ × (F × (P_bull ÷ A_bull + P_bear ÷ A_bear) − 2), the two legs' deviations added, as Combo Spread "
                      "writes it (1 bps of spread is 0.5 bps of the two legs' notional); owner item 84: blue with · Entry when it is at or "
                      "past its entry line (Mean − entry_in σ); — while a leg's book conflicts or its depth is unusable (owner item 70).",
         dict(ENTRY_HIT, displayName="Real Spread · Long")),
        ("Real Short", "Selling both (where a Short enters and a Long exits): orange with · Entry at or past Mean + entry_in σ.",
         dict(ENTRY_HIT_SHORT, displayName="Real Spread · Short")),
        ("Mean", "The program's mean of the spread (on the mids), bps (a minus sign, as in the real spreads).", None),
        ("Volatility", "Volatility (σ): the standard deviation of the spread, bps.", dict(bps1, displayName="Volatility (σ)")),
        ("Deviation", "Spread on the mids − Mean, in bps and in σ.", None),
        ("Signal", "In Combo Spread's words, on the program's executable deviations: Long (Buy Both at or past −entry_in σ), Short (Sell Both at "
                   "or past +entry_in σ), None inside; No Anchor, No Mean (σ not ready), No Book (a leg's book conflicts or its depth is "
                   "unusable: the program enters on none).", value_map(SIGNAL_COLORS)),
    ], 0, y, 24, 3, intro="The same second, as Combo Spread's Spread and Signal · Latest Second; every spread figure in bps.", source=market,
        columns=["Real Long", "Real Short", "Mean", "Volatility", "Deviation", "Signal"], title_size=12, value_size=16))
    y += 3
    panels.append(table("Alerts", Q_ALERTS, 0, y, 24, 8, description=bullets(
        "What stopped or held trading in the picked Run and what became of it: the unfinished first, each group newest first, at most 200.",
        [("Severity", "critical: position mismatch, a lost or stale Run, an entry held for a person (settings, records, mismatch), a program "
                      "error; serious: a sample gap, one leg only, a forced exit, an order rejected, unknown, never arrived or not found; "
                      "warning: an entry held back (a book, F, no anchor, backing off, a stop pending), a stop or session end while holding, "
                      "dust, funding awaited, an order not sent."),
         ("Status", "Open: a person is needed; In Progress: the program is still on it (only while the Run runs, else Open); Recovered: it "
                    "ended by itself; Recorded: dust, recorded and not processed (owner item 47)."),
         ("Time", "when it happened, UTC+8; for an entry held back, the start of its newest episode."),
         ("Source", "the part it belongs to: the Run, the positions, the entry, an order or a round trip (click a round trip's Source for its "
                    "orders below)."),
         ("What, Detail", "what happened, and its figures: for an entry held back, how often, how long in all and at most, and the "
                          "seconds an executable deviation was past an entry line meanwhile (the entries it cost).")]), overrides=[
        col("time", 116, displayName="Time", unit="time:MM-DD HH:mm:ss"), col("Severity", **word_cells(SEVERITY, 66)),
        col("Status", **word_cells(ALERT_STATUS, 90)),
        col("Source", 184, links=[{"title": "Show this round trip's orders", "url": link, "targetBlank": False}]),
        col("What", 146, custom_inspect=True), col("Detail", custom_minWidth=130, custom_inspect=True, custom_cellOptions={"type": "auto", "wrapText": True}),
        col("cycle_id", custom_hidden=True)]))
    y += 8

    # ------------------------------------------------------------------ Execution Summary (owner items 76, 77)
    panels.append(row("Execution Summary · ${span}", y)); y += 1
    money = dict(MONEY, unit="suffix: U")
    panels.append(stat_strip("Summary", Q_PNL, [
        ("PnL", "Gross − Fee + Funding of the round trips closed in the Range, USDT: what the strategy earned (one whose funding is not known "
                "yet left out until it is).", money),
        ("Gross", "Both legs' fills, sells less buys, before costs.", money),
        ("Fee", "Trading fees paid, a cost shown as a positive amount (as everywhere on this board: Orders, the chart markers).",
         {"unit": "suffix: U", "decimals": 2, "color": {"mode": "fixed", "fixedColor": "text"}, "mappings": [NO_FIGURE]}),
        ("Funding", "Funding booked to them; + received.", money),
        ("Open PnL", "The open round trip at the newest second's mids after its entry fees, before exit fees and the half spread an exit "
                     "pays; green above zero, red below; not in PnL.", money),
        ("Notional Text", "Traded: both legs' fills (entries, completions, trims, exits) of those round trips, USDT.",
         {"displayName": "Notional"}),
        ("Peak Used Text", "The most of N = M × L in use in the Range (held at entry prices, open orders, dust), USDT, from the Run's inventory "
                      "samples.", {"displayName": "Peak Used"}),
        ("Win Rate", "Closed round trips with a positive PnL.", {"unit": "percent", "decimals": 0, "mappings": [NO_FIGURE]}),
        ("Return", "PnL over Peak Used, bps (owner item 69: a return on the largest notional actually used).", dict(MONEY, unit="suffix: bps", decimals=1)),
    ], 0, y, 24, 4, intro="The Run's round trips closed in the Range (row title), in the owner's order (item 76); money in USDT (U), "
                         "Notional and Peak Used in USDT.", title_size=12, value_size=18,
        columns=["PnL", "Gross", "Fee", "Funding", "Open PnL", "Notional Text", "Peak Used Text", "Win Rate", "Return"]))
    y += 4
    usdt0 = {"unit": "locale", "decimals": 0, "mappings": [NO_FIGURE]}
    params = stat_strip("Parameters", Q_PARAMS, [
        ("Mean Model", "The mean of the spread: EMA or SMA, over its scale in minutes.", None),
        ("Volatility Scale", "The window of Volatility (σ), the spread's standard deviation, in minutes.", None),
        ("Entry", "entry_in (d_in before PRD 4.0): a Long enters when Buy Both is entry_in σ below the mean, a Short when Sell Both is "
                  "entry_in σ above it; in spread bps at the newest second's σ.", None),
        ("Exit 1", "entry_out (d_out before): a Long exits when Sell Both reaches −entry_out σ from the mean (past it, on the other side, "
                   "when it is negative), a Short the mirror; with Exit 2 set this line closes half the position (the round must be in "
                   "profit), else all of it; in spread bps at the newest second's σ.", {"displayName": "Exit" + NBSP + "1"}),
        ("Exit 2", "exit2: the line that closes the rest, even below break-even (PRD 9.2); none: one exit line only.", {"displayName": "Exit" + NBSP + "2"}),
    ], 0, y, 24, 3, intro="The Run's settings (process_run.config) that its results are read with (owner item 80): its signal; its size and "
                          "execution in the strip below, the rest in Settings.",
        title_size=12, value_size=16, columns=["Mean Model", "Volatility Scale", "Entry", "Exit 1", "Exit 2"])
    panels.append(params)
    y += 3
    panels.append(stat_strip("Size and Execution", None, [
        ("Capital M Text", "The real capital the Run may use, USDT.", {"displayName": "Capital M"}),
        ("Leverage L", "The total leverage.", {"unit": "suffix:×", "decimals": 0, "mappings": [NO_FIGURE]}),
        ("Notional N Text", "M × L: the notional the Run may hold, USDT.", {"displayName": "Notional N"}),
        ("Thin Leg", "exec.thin_leg (4.4): the leg an entry batch and a line exit batch send first, alone (core/SPEC.md 12.4, 12.7); the "
                     "other leg follows by the first one's fills; both together: the way before (a Run before PRD 4.0).", None),
        ("Pair Window", "pair_window: how long the other leg is chased after the thin leg's first fill; then the part not paired is "
                        "unwound.", None),
        ("Pair Stop", "pair_stop_d, spread bps: the ideally paired spread this much worse than at the decision ends the chase (entry) "
                      "or closes the exit's other leg whatever its break-even (exit).", None),
        ("Top-Up Max", "thin_topup_max: top-ups of the thin leg, while the spread goes on widening, keep its fills within this many "
                       "times its planned quantity.", {"displayName": "Top" + NBHY + "Up" + NBSP + "Max"}),
        ("Exit Pair Window", "exit_pair_window: how long an exit's other leg waits for its break-even bound before it closes whatever "
                             "the price.", {"displayName": "Exit" + NBSP + "Window"}),
    ], 0, y, 24, 3, intro="How much the Run may hold and how it executes a batch (process_run.config inventory and exec); — where it sends "
                          "both legs together.", source=params,
        title_size=12, value_size=16, columns=["Capital M Text", "Leverage L", "Notional N Text", "Thin Leg", "Pair Window", "Pair Stop",
                                               "Top-Up Max", "Exit Pair Window"]))
    y += 3
    panels.append(xy("Cumulative Gross · USDT", [("A", Q_CUM_GROSS), ("Period", Q_PERIOD)], 0, y, 12, 8,
                     "The running Gross over the Range (both legs' fills, sells less buys), as the Summary counts it, stepping at each "
                     "round trip's close (its funding known), from 0 at the Range's start to its end.",
                     "lines", [by_regex("^Gross$", {"color": {"mode": "fixed", "fixedColor": "#4fc3d9"}, "custom.axisLabel": " ", "decimals": 2,
                                                    "custom.axisSoftMin": -0.1, "custom.axisSoftMax": 0.1})]))
    panels.append(xy("Cumulative PnL · USDT", [("A", Q_CUM_PNL), ("Period", Q_PERIOD)], 12, y, 12, 8,
                     "The running PnL (Gross + Fee + Funding) over the Range, as the Summary counts it, stepping at each round trip's close.",
                     "lines", [by_regex("^PnL$", {"color": {"mode": "fixed", "fixedColor": PURPLE}, "custom.axisLabel": " ", "decimals": 2,
                                                  "custom.axisSoftMin": -0.1, "custom.axisSoftMax": 0.1})]))
    y += 8

    # ------------------------------------------------------------------ Position · Last Second (owner item 80: no entry figures)
    panels.append(row("Position · Last Second · ${combo}", y)); y += 1
    side = {"mappings": [NO_FIGURE] + SIDE_COLORS + regex_colors([("Flat", GREY)])}
    position = stat_strip("Position · ct and USDT", Q_NOW, [
        ("Side", "Flat, or the open round trip's side: Long (both legs bought, blue), Short (both sold, orange).", side),
        ("Bull", "Contracts held on the leg as the program counts them, signed (+ long, − short); – when flat.",
         {"unit": "none", "decimals": 2, "displayName": "${bull} · ct", "mappings": [NO_FIGURE]}),
        ("Bear", "", {"unit": "none", "decimals": 2, "displayName": "${bear} · ct", "mappings": [NO_FIGURE]}),
        ("Cost", "USDT: what the held contracts cost at entry, both legs (the notional held).",
         {"unit": "locale", "decimals": 0, "mappings": [NO_FIGURE], "displayName": "Cost · USDT"}),
        ("Held", "Time since the round trip opened.", None),
        ("To Exit", "σ still to go to the (first) exit line (a Long: Sell Both up to −entry_out σ; a Short: Buy Both down to +entry_out σ); "
                    "0 or less: at the line · –: a leg's depth is unusable (the program sends no exit then; a book conflict does not stop "
                    "an exit).", {"unit": "suffix:σ", "decimals": 2, "mappings": [NO_FIGURE], "displayName": "To" + NBSP + "Exit" + NBSP + "1"}),
        ("To Exit 2", "The same to the second line (exit2), which closes the rest; – without it.",
         {"unit": "suffix:σ", "decimals": 2, "mappings": [NO_FIGURE], "displayName": "To" + NBSP + "Exit" + NBSP + "2"}),
    ], 0, y, 24, 3, intro="The Run's newest second (a stopped Run's last), whatever the Range.",
        columns=["Side", "Bull", "Bear", "Cost", "Held", "To Exit", "To Exit 2"], value_size=16, title_size=12)
    panels.append(position); y += 3

    # ------------------------------------------------------------------ Live Execution
    panels.append(row("Live Execution · ${lines}", y)); y += 1
    action = code_texts(ACTIONS, ACTION_COLORS)
    no_value1 = code_texts({**NO_VALUE, ZERO: "0.0"}, {"0.0": TEXT})
    entry_text = code_texts(IDLE_CODES, {t: STATE_COLORS[t.split(" · ")[0]] for t in IDLE_CODES.values()})
    qty_text = code_texts({ZERO: "0", NOT_IN: "— (not in this order)"}, {"0": TEXT})
    price_text = code_texts({NO_FILL: "No Fill", NOT_IN: "—"})
    money_text = code_texts({FEE_PENDING: "Pending", PENDING: "Pending", OPEN: "Open", ZERO: "0.00"}, {"Pending": WARN, "Open": TEXT, "0.00": TEXT})
    at_text = code_texts({ZERO: "0.0", NO_SAMPLE: "No Sample"}, {"0.0": TEXT})
    hidden = {"legend": True, "tooltip": True, "viz": False}

    def line(name, legend=True, shown_as=None):
        _, color, dash = LINES[name.replace(" · Dev", "")]
        o = series(name, color, width=1.5, dash=dash, legend=legend, tooltip=False)
        if shown_as:
            o["properties"].append({"id": "displayName", "value": shown_as})
        return o

    def decision_marks(chart):
        """The markers of one chart (owner items 81, 84): triangles as filled bands between their peak and base, circles as points."""
        out = []
        # Grafana fills between two series only where the one with fillBelowTo is above the other: a ▲'s peak fills down to
        # its base, a ▼'s base down to its peak. The two undone entries share one legend item (grey).
        looks = {"Long Entry": (GOOD, "Long Entry ▲", True), "Short Entry": (BAD, "Short Entry ▼", True),
                 "Failed Long Entry": (GREY, "Failed ▲▼", True), "Failed Short Entry": (GREY, "Failed ▼", False),
                 "Unwound Long Entry": (UNWOUND, "Unwound ▲▼", True), "Unwound Short Entry": (UNWOUND, "Unwound ▼", False)}
        for name, _, up in TRIANGLES:
            color, label, legend = looks[name]
            peak_name, base_name = name + chart, "%s · Base%s" % (name, chart)
            peak = series(peak_name, color, width=1, fill_to=base_name if up else None, fill=100 if up else 0, legend=legend, tooltip=False)
            peak["properties"] += [{"id": "displayName", "value": label}, {"id": "custom.showPoints", "value": "never"}]
            base_ = named(base_name, {"color": {"mode": "fixed", "fixedColor": color}, "custom.lineWidth": 1, "custom.showPoints": "never",
                                      "custom.hideFrom": hidden, **({} if up else {"custom.fillBelowTo": peak_name, "custom.fillOpacity": 100})})
            out += [peak, base_]
        # Exits: a circle in the colour of the position it closes (owner item 84); larger than the line points.
        for name, color, label in (("Long Exit", GOOD, "Long Exit ●"), ("Short Exit", BAD, "Short Exit ●")):
            out.append(named(name + chart, {"color": {"mode": "fixed", "fixedColor": color}, "custom.drawStyle": "points", "custom.pointSize": 10,
                                            "custom.hideFrom": {"legend": False, "tooltip": True, "viz": False}, "displayName": label}))
        return out

    def at_entry(name, color):
        """Owner item 84: the stretch of a real line at or past its entry line, drawn over it thick."""
        return named(name, {"color": {"mode": "fixed", "fixedColor": color}, "custom.lineWidth": 4, "custom.showPoints": "always",
                            "custom.pointSize": 5, "custom.hideFrom": {"legend": True, "tooltip": True, "viz": False}})

    def sigma_line(name):
        """As Combo Spread's: grey, whole multiples dashed, halves dotted, stronger further out; neither legend nor tooltip."""
        k = float(name.rstrip("σ").split(" ")[-1].lstrip("+−"))
        return named(name, {"color": {"mode": "fixed", "fixedColor": GREY + "%02X" % round(255 * (0.35 + 0.13 * k))}, "custom.lineWidth": 1,
                            "custom.lineStyle": {"fill": "dash", "dash": [6, 4] if k == int(k) else [2, 3]}, "custom.hideFrom": hidden})
    trimmed_text = code_texts({ZERO: "0"}, {"0": TEXT})
    zero1 = code_texts({ZERO: "0.0"}, {"0.0": TEXT})
    zero2 = code_texts({ZERO: "0.00"}, {"0.00": TEXT})
    marker_tooltip = [tooltip_only("Execution", TEXT, code_texts({NO_SAMPLE: "–"}), {"unit": "time:MM-DD HH:mm:ss"}, shown="Execution ID"),
                      tooltip_only("Action", TEXT, action), tooltip_only("Decided", TEXT, props={"unit": "time:HH:mm:ss.SSS"}),
                      tooltip_only("Batch End", TEXT, word_codes(BATCH_ENDS, {"Built": TEXT, "Built Partial": TEXT, "Unwound at a Profit": UNWOUND,
                                                                             "Unwound at the Stop": UNWOUND, "One Leg, Trimmed Back": UNWOUND}), shown="Batch"),
                      tooltip_only("Batch Why", TEXT, word_codes(BATCH_WHY, {w: TEXT for w in BATCH_WHY.values()}), shown="Batch Ended By"),
                      tooltip_only("Exit Stage", TEXT, word_codes(EXIT_STAGES, {w: TEXT for w in EXIT_STAGES.values()}), shown="Exit Stage"),
                      tooltip_only("Pre-Check", TEXT, word_codes(PRECHECKS, {"Passed": TEXT, "Failed · Exits Anyway": WARN}), shown="Exit Pre-Check"),
                      tooltip_only("Thick Leg Close", TEXT, word_codes(THICK_CLOSES, {"Within Break-Even": TEXT, "Hard Close · Window": WARN,
                                                                                     "Hard Close · Stop": WARN}), shown="${bull} Exit Closed"),
                      tooltip_only("Bull Filled", TEXT, qty_text, {"unit": "none", "decimals": 2}, shown="${bull} ct"),
                      tooltip_only("Bull Avg", TEXT, price_text, {"unit": "none", "decimals": TICK_DECIMALS["bull"]}, shown="${bull} Avg Price USDT"),
                      tooltip_only("Bear Filled", TEXT, qty_text, {"unit": "none", "decimals": 2}, shown="${bear} ct"),
                      tooltip_only("Bear Avg", TEXT, price_text, {"unit": "none", "decimals": TICK_DECIMALS["bear"]}, shown="${bear} Avg Price USDT"),
                      tooltip_only("Bull Trimmed", GREY, trimmed_text, {"unit": "none", "decimals": 2}, shown="${bull} Trimmed Back ct"),
                      tooltip_only("Bear Trimmed", GREY, trimmed_text, {"unit": "none", "decimals": 2}, shown="${bear} Trimmed Back ct"),
                      tooltip_only("Bull Cap", TEXT, qty_text, {"unit": "none", "decimals": 3}, shown="${bull} Pair Cap P* USDT"),
                      tooltip_only("Ideal Spread", TEXT, zero1, {"unit": "none", "decimals": 1}, shown="Ideal Paired Spread bps"),
                      tooltip_only("Paired In", TEXT, zero2, {"unit": "none", "decimals": 2}, shown="Pairing Took s"),
                      tooltip_only("Notional", TEXT, qty_text, {"unit": "locale", "decimals": 0}, shown="Notional USDT"),
                      tooltip_only("Fees", TEXT, money_text, {"unit": "none", "decimals": 2}, shown="Fees Paid USDT"),
                      tooltip_only("Expected Net", TEXT, money_text, {"unit": "none", "decimals": 2}, shown="Expected Round Net USDT"),
                      tooltip_only("Actual Net", TEXT, money_text, {"unit": "none", "decimals": 2}, shown="Round Net After USDT"),
                      tooltip_only("Execution Cost", TEXT, money_text, {"unit": "none", "decimals": 2}, shown="Execution vs Expected USDT"),
                      tooltip_only("Cycle Net", TEXT, money_text, {"unit": "none", "decimals": 2}, shown="Round Trip PnL USDT")]
    common_tips = [tooltip_only("Sigma · Tooltip", LIGHT_GREY, no_value1, {"decimals": 1}, shown="Volatility (σ) bps"),
                   tooltip_only("Entry", TEXT, entry_text)]
    spread_sigmas = [spread_sigma(sg, k) for k in SIGMAS for sg in ("+", "−")]
    dev_sigmas = [dev_sigma(sg, k) for k in SIGMAS for sg in ("+", "−")]
    spread_lines = ([sigma_line(n) for n in spread_sigmas] +
                    [series("Real Long Spread", BLUE, width=1.5, tooltip=False), series("Real Short Spread", ORANGE, width=1.5, tooltip=False),
                     at_entry("Real Long at Entry", BLUE), at_entry("Real Short at Entry", ORANGE),
                     series("Mean", TEXT, width=1, tooltip=False),
                     line("Long In", shown_as="Long In / Out"), line("Long Out", legend=False), line("Long Out 2", legend=False),
                     line("Short In", shown_as="Short In / Out"), line("Short Out", legend=False), line("Short Out 2", legend=False)] + decision_marks("") +
                    [tooltip_only("Real Long Spread · Tooltip", BLUE, no_value1, {"decimals": 1}, shown="Real Long Spread bps"),
                     tooltip_only("Real Short Spread · Tooltip", ORANGE, no_value1, {"decimals": 1}, shown="Real Short Spread bps"),
                     tooltip_only("Mean · Tooltip", TEXT, no_value1, {"decimals": 1}, shown="Mean bps")] + common_tips + marker_tooltip +
                    [tooltip_only("At Spread", PURPLE, at_text, {"decimals": 1}, shown="At Spread bps")])
    spread_tips = ["Real Long Spread · Tooltip", "Real Short Spread · Tooltip", "Mean · Tooltip", "Sigma · Tooltip", "Entry"]
    info = [n for n, _ in MARKER_INFO if not n.startswith("At ")]
    # The tooltip lists a decision first (its rows are hidden away from one), then the point's values. The σ lines first (under
    # everything), the highlights over their lines.
    spread_order = (["time"] + spread_sigmas + ["Real Long Spread", "Real Short Spread", "Real Long at Entry", "Real Short at Entry", "Mean"] +
                    list(LINES) + [n for n, _ in SPREAD_GLYPHS] + info + ["At Spread"] + spread_tips)
    chart_note = ("Points: every second up to 25 min, then 5, 10, 30 s and whole minutes (a real spread is its bucket's extreme on its "
                  "entry side, of the seconds whose books the program enters on: none, and tooltip Book Not OK, where a leg's book "
                  "conflicts or its depth is unusable, owner item 70); a gap: no sample (process down), or a new session or run; shaded: "
                  "each round trip from entry to exit (blue Long, orange Short; its mark under the chart: times, end reason, PnL); purple "
                  "marks: a change of the anchors in use, with reason and both legs' anchors before → after (owner item 62).")
    marker_note = ("Markers (owner items 81, 84; 4.4): a long entry a green triangle ▲, a short entry a red triangle ▼; a failed entry "
                   "(the thin leg filled nothing; with both legs together no fill) a grey triangle; an unwound entry (a leg filled and "
                   "was closed back: at a profit or at the stop) an amber triangle; an exit a circle in the colour of the position it "
                   "closes (closing a Long green, closing a Short red; a forced exit too, its tooltip says Forced); at the decision's "
                   "executable spread. Hover one for its Execution ID (the round trip's opening, UTC+8, owner item 83: every marker of "
                   "one round trip has the same, as its row in Round Trips), the action, each leg's filled contracts and average price, "
                   "what was trimmed back or unwound, the notional, the fees paid (a cost, positive) and the round trip's PnL. A marker "
                   "counts its whole step: an entry every order of its batch (with the thin leg first the thin order, its top-ups, the "
                   "other leg's pairing orders and the unwinds), an exit its orders and those of its other leg, so an exit whose legs "
                   "were closed by separate orders shows both; a leg it sent no order on shows —. With the thin leg first an entry also "
                   "shows how its batch ended and why, the other leg's pair cap P*, the ideally paired spread and how long the pairing "
                   "took; an exit its stage (Exit 1 the half, Exit 2 the rest), its pre-check (failed: it exits below break-even all "
                   "the same, a loss accepted), how its other leg closed (within break-even, or a hard close at the window or the "
                   "spread stop) and the round's net expected at the decision against the net after the batch: their difference is "
                   "what the execution cost.")
    spread = timeseries("Spread · bps", Q_EXECUTION, 0, y, 24, 13, spread_lines, decimals=0, calcs=(), description=bullets(
        "After Combo Spread's Spread chart (owner item 81), in spread bps: 10⁴ × (F × (P_bull ÷ A_bull + P_bear ÷ A_bear) − 2), the "
        "two legs' deviations added (1 bps of spread is 0.5 bps of the two legs' notional), on the time picker's axis.",
        [("Real Long Spread", "buying both legs at their pricing levels (blue): a Long enters when it falls to Mean − entry_in σ (blue dashed), "
                              "a Short exits when it falls to Mean + entry_out σ (orange dotted); thick where it is at or past its entry line "
                              "(owner item 84)."),
         ("Real Short Spread", "selling both (orange): a Short enters when it rises to Mean + entry_in σ (orange dashed), a Long exits when it "
                               "rises to Mean − entry_out σ (blue dotted); thick at or past its entry line."),
         ("Mean", "the program's mean of the spread on the mids."),
         ("Volatility lines", "Mean ± 0.5, 1, 1.5, 2, 2.5 and 3 σ, grey, whole multiples dashed and halves dotted, stronger further out, "
                              "as Combo Spread draws them (owner item 84); the Run's entry and exit lines in colour over them."),
         ("Markers", marker_note), ("Points and marks", chart_note)]))
    spread["options"]["tooltip"]["hideZeros"] = True
    spread["options"]["legend"].update(displayMode="list", placement="bottom")
    spread["options"]["legend"].pop("width", None)
    spread["fieldConfig"]["defaults"]["noValue"] = "No second of the Run on this axis: Charts to the Range (top) moves it to the Run"
    spread["transformations"] = [keep_fields(spread_order), organize(spread_order)]
    panels.append(spread); y += 13
    dev_lines = ([sigma_line(n) for n in dev_sigmas] +
                 [series("Real Long Deviation", BLUE, width=1.5, tooltip=False), series("Real Short Deviation", ORANGE, width=1.5, tooltip=False),
                  at_entry("Real Long Deviation at Entry", BLUE), at_entry("Real Short Deviation at Entry", ORANGE),
                  line("Long In · Dev", shown_as="Long In / Out"), line("Long Out · Dev", legend=False),
                  line("Long Out 2 · Dev", legend=False), line("Short In · Dev", shown_as="Short In / Out"), line("Short Out · Dev", legend=False),
                  line("Short Out 2 · Dev", legend=False)] + decision_marks(" · Dev") +
                 [tooltip_only("Real Long Deviation · Tooltip", BLUE, no_value1, {"decimals": 1}, shown="Real Long Deviation bps"),
                  tooltip_only("Real Short Deviation · Tooltip", ORANGE, no_value1, {"decimals": 1}, shown="Real Short Deviation bps")] +
                 common_tips + marker_tooltip + [tooltip_only("At Deviation", PURPLE, at_text, {"decimals": 1}, shown="At Deviation bps")])
    dev_tips = ["Real Long Deviation · Tooltip", "Real Short Deviation · Tooltip", "Sigma · Tooltip", "Entry"]
    dev_order = (["time"] + dev_sigmas + ["Real Long Deviation", "Real Short Deviation", "Real Long Deviation at Entry",
                                          "Real Short Deviation at Entry"] +
                 [n + " · Dev" for n in LINES] + [n for n, _ in DEVIATION_GLYPHS] + info + ["At Deviation"] + dev_tips)
    deviation = timeseries("Deviation · bps", None, 0, y, 24, 13, dev_lines, decimals=0, zero_line=True, calcs=(), source=spread,
                           description=bullets(
        "After Combo Spread's Deviation chart (owner item 81): the Spread chart with its Mean at zero, in spread bps; no line on the "
        "mids (owner item 84).",
        [("Real Long Deviation, Real Short Deviation", "the real spreads − Mean (blue, orange): a Long enters when blue falls to −entry_in σ "
                                                       "(blue dashed) and exits when orange rises to −entry_out σ (blue dotted); a Short the "
                                                       "mirror (orange lines); thick at or past the entry line."),
         ("Volatility lines", "± 0.5, 1, 1.5, 2, 2.5 and 3 σ, grey, as on the Spread chart."),
         ("Markers", marker_note.replace("executable spread", "executable deviation")),
         ("Points and marks", chart_note)]))
    deviation["options"]["tooltip"]["hideZeros"] = True
    deviation["options"]["legend"].update(displayMode="list", placement="bottom")
    deviation["options"]["legend"].pop("width", None)
    deviation["fieldConfig"]["defaults"]["custom"]["axisCenteredZero"] = True
    deviation["fieldConfig"]["defaults"]["noValue"] = "No second of the Run on this axis: Charts to the Range (top) moves it to the Run"
    deviation["transformations"] = [keep_fields(dev_order), organize(dev_order)]
    panels.append(deviation); y += 13

    # ------------------------------------------------------------------ Round Trips
    panels.append(row("Round Trips · ${combo} · ${span}", y)); y += 1
    money_col = dict(custom_align="right", unit="none", decimals=4, mappings=[NO_FIGURE, PENDING_TEXT], **SIGN_BAND)
    sigma_col = dict(custom_align="right", unit="none", decimals=2, mappings=[NO_FIGURE])
    panels.append(table("Round Trips · Newest First", Q_BLOTTER, 0, y, 24, 7, description=bullets(
        "Every round trip of the Run open at any time in the Range, newest first, at most 500; no-fill ones (nothing traded) left out.",
        [("Execution ID", "the round trip's opening (its first entry), UTC+8 (owner item 83): every marker of the round trip on the Spread "
                          "and Deviation charts (entries, completions, trims, exits) shows the same ID; click for its orders below."), ("Closed, Held", "its close, and open to close (to now while open)."),
         ("Side", "Long: both legs bought; Short: both sold."),
         ("Notional", "both legs' entry value, USDT (a One Leg round trip: that leg's)."),
         ("In σ", "the executable deviation at its entries, weighted by their value."), ("Out σ", "at the exit decision (– for a forced exit or none)."),
         ("Net", "Gross − Fees + Funding, USDT; Open while open, Pending (amber) while a funding settlement it held through has no record."),
         ("Net bps", "Net ÷ Notional × 10⁴ (bps of the notional, not of the spread)."),
         ("End Reason", "why it closed; Nothing Paired (4.4): the thin leg first and every batch unwound (the program's one_leg).")]), overrides=[
        col("cycle_id", custom_hidden=True),
        col("Opened", 116, displayName="Execution ID", unit="time:MM-DD HH:mm:ss", links=[{"title": "Show this round trip's orders", "url": link, "targetBlank": False}]),
        col("Closed", 80, unit="time:HH:mm:ss"), col("Held", 68),
        col("Side", 56, custom_cellOptions={"type": "color-text"}, mappings=SIDE_COLORS),
        col("Notional", 88, displayName="Notional USDT", custom_align="right", unit="none", decimals=2), col("In σ", 56, **sigma_col), col("Out σ", 56, **sigma_col),
        col("Net", 84, displayName="Net USDT", **money_col), col("Net bps", 64, displayName="Net bps", **dict(money_col, decimals=1)), col("End Reason", custom_minWidth=120)]))
    y += 7

    # ------------------------------------------------------------------ Round Trip Detail
    panels.append(row("Round Trip Detail · ${cycle:text}", y)); y += 1

    def num(d):
        return dict(custom_align="right", unit="none", decimals=d, mappings=[NO_FIGURE])
    panels.append(table("Batches · ct, USDT and s", Q_BATCH_ROWS, 0, y, 24, 6, description=bullets(
        "The entry batches of the picked round trip, in their order (4.4, core/SPEC.md 12.4: with the thin leg first one batch sends the "
        "thin leg alone, pairs it with the other leg, and ends in one of five states).",
        [("End", "Built, Built Partial (part paired, the rest of the thin leg unwound or closed at a profit), Unwound at a Profit or at "
                 "the Stop (nothing paired), Entry Failed (the thin leg filled nothing), Pairing Now; with both legs together Built, "
                 "Unwound: One Leg Trimmed Back, No Fill."),
         ("Why", "what ended the pairing: Pair Window Over, Ideal Spread Stop, Too Small to Pair, Thin Leg Closed at a Profit, Program "
                 "Stopping, Forced Exit; empty when the other leg paired it all."),
         ("$bull ct, $bear ct", "the contracts the batch opened on each leg (the thin order, its top-ups, the other leg's pairing orders)."),
         ("Back ct", "what was closed back (unwinds and profit closes of the thin leg's unpaired part; with both legs together trims)."),
         ("Pairing s", "from the thin leg's first fill to the end of the pairing, seconds."),
         ("Net USDT", "the batch's own net (its lot's): of an unwound batch what the unwind or profit close made; Open while it holds a "
                      "position (its exits are the round trip's).")]), overrides=[
        col("Batch", 68), col("Opened", 96, unit="time:HH:mm:ss.SSS"), col("End", 140), col("Why", custom_minWidth=156),
        col("Bull", 68, displayName="${bull} ct", **num(2)), col("Bear", 68, displayName="${bear} ct", **num(2)), col("Back", 64, displayName="Back ct", **num(2)),
        col("Pairing", 64, displayName="Pairing s", **num(2)), col("Net", custom_minWidth=84, displayName="Net USDT", **dict(money_col, mappings=[NO_FIGURE, PENDING_TEXT]))]))
    y += 6
    panels.append(table("Orders · ct, USDT and ms", Q_ORDERS, 0, y, 24, 7, description=bullets(
        "Every order of the picked round trip (the Range's newest by default), in the order the program sent them, its fills folded in.",
        [("Step", "what the order was for, from the decision that sent it (4.4; it replaces 4.3's Purpose column, which stays in the "
                  "panel's data): Batch n · Thin Leg or Top-Up (purpose entry), Pair (complete: the other leg pairing the thin leg's "
                  "fills), Unwind or Profit Close (trim: the thin leg's unpaired part closed back), n the entry batch of the round "
                  "trip; Exit 1 · Thin, Exit 1 · Thick, Exit 2 · Hard Close (the thick leg closed whatever its break-even); with both "
                  "legs together Batch n · Entry, Complete (the leg that filled less), Trim (the leg that filled more), Exit, Forced."),
         ("Side", "Buy or Sell; RO: a reduce-only order."), ("Qty, Filled", "contracts ordered and filled."),
         ("Limit, Avg", "the limit and the fills' average price, USDT, to the leg's tick."),
         ("Fee", "USDT paid, a cost shown as a positive amount (as in Summary); Pending while the venue has not reported it."),
         ("Latency", "sent → final state in hand, ms, this host's clock (paper: the configured simulation)."),
         ("Status", "Filled, Partial, No Fill (an IOC that met nothing), Rejected (with the venue's code), Working, Unknown (being queried).")]),
        overrides=[
        col("Time", 96, unit="time:HH:mm:ss.SSS"), col("Purpose", custom_hidden=True), col("Step", 148), col("Leg", 52), col("Side", 64),
        col("Qty", 56, displayName="Qty ct", **num(2)), col("Filled", 56, displayName="Filled ct", **num(2)),
        col("Limit", 72, displayName="Limit USDT", custom_align="right"), col("Avg", 72, displayName="Avg USDT", custom_align="right"),
        col("Fee", 76, displayName="Fee USDT", custom_align="right", unit="none", decimals=5, mappings=[NO_FIGURE, PENDING_TEXT]), col("Latency", 64, displayName="Latency ms", **num(0)),
        col("Status", custom_minWidth=100, custom_cellOptions={"type": "color-text"},
            mappings=regex_colors([("Rejected", BAD), ("Unknown", WARN), ("Partial", WARN), ("No Fill", GREY)]))])); y += 7

    # ------------------------------------------------------------------ Inventory
    panels.append(row("Inventory · ${runtext}", y)); y += 1
    positions = table("Positions Now · ct, USDT and Bps", Q_POSITIONS, 0, y, 24, 5, description=bullets(
        "Each leg at the Run's newest inventory sample (every 10 s), whatever the Range; the venue's figures live only (the paper venue reports "
        "positions alone).",
        [("Venue, Ledger", "the venue's position and the program's ledger (dust included), contracts, signed (+ long, − short); they must match."),
         ("Mark, Liq Price", "the venue's mark and liquidation price, USDT."),
         ("Liq Distance", "the liquidation price from the mark, in %% of the mark: amber within %d %%, red within %d %%." % (LIQ_WARN * 100, LIQ_NEAR * 100)),
         ("Venue UPL", "the venue's unrealised P&L of the leg, USDT."), ("Exposure", "the position at the newest second's mid, USDT, signed."),
         ("Funding", "the newest settled funding rate × 10⁴ (bps of the position's value per settlement), as Bitget publishes it: "
                     "+ longs pay shorts."),
         ("Age", "seconds since the sample while the Run runs; amber past a minute.")]), overrides=[
        col("Leg", 64), col("Venue", displayName="Venue ct", **num(2)), col("Ledger", displayName="Ledger ct", **num(2)),
        col("Mark", displayName="Mark USDT", **num(3)), col("Liq Price", displayName="Liq Price USDT", **num(3)),
        col("Liq Distance", custom_align="right", unit="percent", decimals=0, custom_cellOptions={"type": "color-text"}, color={"mode": "thresholds"},
            thresholds={"mode": "absolute", "steps": [{"color": BAD, "value": None}, {"color": WARN, "value": LIQ_NEAR * 100},
                                                      {"color": "text", "value": LIQ_WARN * 100}]}, mappings=[NO_FIGURE]),
        col("Venue UPL", displayName="Venue UPL USDT", custom_align="right", unit="none", decimals=4, mappings=[NO_FIGURE], **SIGN_BAND),
        col("Exposure", displayName="Exposure USDT", **num(2)), col("Funding", displayName="Funding bps", **num(2)),
        col("Age", custom_align="right", unit="suffix: s", decimals=0, custom_cellOptions={"type": "color-text"},
            color={"mode": "thresholds"}, thresholds={"mode": "absolute", "steps": [{"color": "text", "value": None}, {"color": WARN, "value": 60}]},
            mappings=[NO_FIGURE])])
    panels.append(positions); y += 5   # 4.3: headers with units wrap to two lines, one unit taller

    # ------------------------------------------------------------------ Settings
    panels.append(row("Settings · ${runtext}", y)); y += 1
    about = {"Signal and Sizing": "What else the program trades on (the mean model, its scale, the entry and exit points, M, L and N are "
                                  "in Parameters above): the taker fee it counts, the margin over costs an entry needs (spread bps), whether the "
                                  "mean warms up again when the anchor changes; the smallest batch (USDT); take ratio: the share of the depth up to the pricing level an order takes; the time and the "
                              "spread bps between entries of a cycle.",
             "Execution": "How it trades: Live (real orders on Bitget) or Paper; the book level each leg's limit sits at ($bull / $bear); "
                          "the order channel (WS or REST) and the book feed × its connections; how long it waits to complete the other "
                          "leg; the slip a forced exit may pay; the order latency paper simulates ($bull / $bear; a paper Run only).",
             "Risk & Session": "When it stops and what it trusts: how long before the regular close it flattens; how old a book or the "
                               "USD/USDT rate F may be; how long a NAV must stay unchanged to be confirmed; how long a stop may take "
                               "before an alarm; the ex-dates of the dividends (and splits) it knows; edits to the market calendar.",
             "Run": "Which start this is (its name and mode are in the row titles and the Execution tiles): start and stop (UTC+8), "
                    "host and run_id."}
    x = 0
    for group, _ in SETTINGS:
        w = SETTINGS_WIDTHS[group]
        if x + w > 24:
            x, y = 0, y + 8
        t = table(group, q_settings(group), x, y, w, 8, description=about[group] + " · process_run.config of the Run.", overrides=[
            col("Setting", 118, custom_cellOptions={"type": "color-text"}, color={"mode": "fixed", "fixedColor": GREY}),
            col("Value", None, custom_align="right")])
        t["options"]["showHeader"] = False
        panels.append(t)
        x += w
    y += 8

    # ------------------------------------------------------------------ Execution Statistics (collapsed)
    sy = y + 1
    statistics = [
        table("Entry Answers · Share of Seconds", Q_IDLE, 0, sy, 9, 8, description=bullets(
            "Each second's answer in the Range (signal_sample.idle_reason) in words.",
            [("Answer", "the state and the reason."), ("Seconds, Share", "how many seconds, and their share of the Range's.")]),
            overrides=[col("Answer", 220), col("Seconds", 80, custom_align="right", unit="locale", decimals=0),
                       col("Share", None, unit="percent", decimals=1, custom_cellOptions={"type": "gauge", "mode": "basic", "valueDisplayMode": "text"},
                           color={"mode": "fixed", "fixedColor": LIGHT_GREY}, min=0, max=100)]),
        table("At the Line, Not Entered · Seconds", Q_BLOCKED, 9, sy, 8, 8, description=bullets(
            "Seconds in the Range the executable deviation was past an entry line but no entry went out (decision at_line_blocked).",
            [("Reason, Side", "why, and the side it would have entered."), ("Last", "the newest such second, UTC+8.")]),
            overrides=[col("Reason", 150), col("Side", 64, custom_cellOptions={"type": "color-text"}, mappings=SIDE_COLORS),
                       col("Seconds", 72, custom_align="right", unit="locale", decimals=0), col("Last", None, unit="time:MM-DD HH:mm:ss")]),
        table("End Reasons · USDT", Q_END_REASONS, 17, sy, 7, 8, description=bullets(
            "Why the Run's round trips of the Range that closed ended (no-fill ones included).",
            [("Cycles", "how many."), ("Net", "their net, USDT (Pending ones out of it)."), ("Pending", "those whose funding is not known yet.")]),
            overrides=[col("End Reason", None), col("Cycles", 64, custom_align="right"), col("Net", 92, displayName="Net USDT", **money_col), col("Pending", 72, custom_align="right")]),
        table("Batch Ends · USDT and s", Q_BATCHES, 14, sy + 8, 10, 6, description=bullets(
            "How the Run's entry batches opened in the Range ended (4.4, core/SPEC.md 12.4; one batch is one lot).",
            [("Batch End", "Built, Built Partial, Unwound at a Profit or at the Stop, Entry Failed (the thin leg filled nothing); a Run "
                           "trading both legs together: Built, Unwound (a leg filled and was trimmed back), No Fill."),
             ("Net", "the lots' own net, USDT: of a void lot what its unwind or profit close made; an open lot's is not in it."),
             ("Pairing s", "the median time from the thin leg's first fill to the end of the pairing, seconds (thin leg first only).")]),
            overrides=[col("Batch End", None), col("Batches", 64, custom_align="right"), col("Net", 92, displayName="Net USDT", **money_col),
                       col("Pairing s", 72, **num(2))]),
        table("Order Latency by Stage · ms", Q_LATENCY, 0, sy + 8, 14, 6, description=bullets(
            "Each stage over the Run's orders submitted in the Range; paper: the configured simulated latency, not a measurement.",
            [("Clock", "local: this host's clock; venue: Bitget's."), ("Orders", "orders measured."),
             ("Median, P90, Max", "the median, 90th percentile and largest, ms.")]), overrides=[
            col("Stage", 180), col("Clock", 72), col("Orders", 80, custom_align="right"), col("Median", 96, displayName="Median ms", **num(1)), col("P90", 96, displayName="P90 ms", **num(1)),
            col("Max", None, displayName="Max ms", **num(0))]),
    ]
    panels.append(collapsed_row("Execution Statistics · ${span}", y, statistics)); y += 1

    # ------------------------------------------------------------------ System (collapsed)
    sy = y + 1
    system = [
        table("Runs · Both Modes", Q_RUNS, 0, sy, 24, 6, description=bullets(
            "Every start of the program, both modes, newest first, as the Run picker lists them (the one shown marked), whatever the filters.",
            [("Run", "click to show that Run, its whole life on the charts."),
             ("State", "Running (a sample in the last minute), Stopped (in order), Lost (neither: crashed or killed)."),
             ("Ran", "start to stop, or to now."), ("N", "the notional M × L it may hold, USDT."), ("Run ID", "process_run.run_id.")]), overrides=[
            col("Run", 380, links=[{"title": "Show this Run, its whole life on the charts", "targetBlank": False, "url": URL + (
                "?orgId=1&from=${__data.fields.from_ms}&to=${__data.fields.to_ms}&refresh=10s&var-run=${__data.fields[\"Run ID\"]}"
                "&var-since=0&var-since_hour=00&var-until=%s&var-until_hour=24" % NO_END_MS)}]),
            col("from_ms", custom_hidden=True), col("to_ms", custom_hidden=True), col("Started", 116, unit="time:MM-DD HH:mm:ss"),
            col("Stopped", 116, unit="time:MM-DD HH:mm:ss"),
            col("State", 80, custom_cellOptions={"type": "color-text"}, mappings=regex_colors([("Running", GOOD), ("Lost", BAD), ("Stopped", GREY)])),
            col("Ran", 84), col("N", 88, displayName="N USDT", custom_align="right", unit="locale", decimals=0), col("Host", 140), col("Run ID", custom_minWidth=200)]),
        table("Program Events", Q_EVENTS, 0, sy + 6, 24, 5, description=bullets(
            "Every event a module of the Run recorded (trade.event) in the Range, newest first, at most 200; the errors are also in Alerts.",
            [("Level", "Error (a person is needed), Warn, or another level the program writes."), ("Module, Kind", "who wrote it and what it is."),
             ("Message, Detail", "its words and its figures.")]), overrides=[
            col("Time", 116, unit="time:MM-DD HH:mm:ss"),
            col("Level", 64, custom_cellOptions={"type": "color-text"}, mappings=regex_colors([("Error", BAD), ("Warn", WARN)])),
            col("Module", 80), col("Kind", 160), col("Message", custom_minWidth=240, custom_inspect=True), col("Detail", custom_minWidth=160, custom_inspect=True)]),
        table("Anchors Read · Sessions in the Range", Q_ANCHORS, 0, sy + 11, 14, 7, description=bullets(
            "Every anchor, NAV and dividend the program read in the Run's mode for the sessions of the Run in the Range, newest first; its own "
            "reading, apart from Combo Spread's Anchors table.",
            [("Kind", "bell_f (the F inside the bell), bell (F × Binance's index at the close), nav_seen (a NAV first seen), nav (confirmed "
                      "after 1 h unchanged), dividend."), ("Value", "USD per share."), ("Known", "when the program learned it, UTC+8.")]), overrides=[
            col("Session", 96), col("Kind", 72), col("Leg", 56), col("Known", 116, unit="time:MM-DD HH:mm:ss"), col("Value", 96, displayName="Value USD", **num(4)),
            col("F", 112, displayName="F USD/USDT", **num(6)), col("Source", 72), col("Detail", custom_minWidth=160, custom_inspect=True)]),
        table("Funding Records · USDT", Q_FUNDING, 14, sy + 11, 10, 7, description=bullets(
            "Each funding record of the Run's two contracts settled in the Range, and each settlement a finished round trip still waits for.",
            [("Status", "Booked (to the round trip whose interval holds the settlement) or Awaited (amber)."), ("Amount", "+ received."),
             ("Execution ID", "the round trip it is booked to, by its opening time (UTC+8).")]),
            overrides=[
            col("Settlement", 112, unit="time:MM-DD HH:mm"), col("Contract", 88),
            col("Status", 80, custom_cellOptions={"type": "color-text"}, mappings=regex_colors([("Awaited", WARN)])),
            col("Amount", 104, displayName="Amount USDT", custom_align="right", unit="none", decimals=4, mappings=[NO_FIGURE], **SIGN_BAND), col("Execution ID", None)]),
    ]
    panels.append(collapsed_row("System · ${span}", y, system)); y += 1

    spans = [spread["id"], deviation["id"]]
    annotations = [bands("Long Round Trips", cycle_spans("long"), BLUE, spans), bands("Short Round Trips", cycle_spans("short"), ORANGE, spans),
                   marks("Anchor Changes", Q_ANCHOR_CHANGES, PURPLE, [p["id"] for p in panels if p["type"] == "timeseries"])]
    board = {"panels": panels, "templating": {"list": templating}, "annotations": {"list": annotations},
             "links": [{"title": "Charts to the Range", "type": "link", "icon": "dashboard", "targetBlank": False, "keepTime": False,
                        "includeVars": False, "tooltip": "The time range of the charts set to the Range (Run, Since, Until): the whole Run by default.",
                        "url": URL + "?orgId=1&from=${range_from}&to=${range_to}&refresh=10s&${run:queryparam}&${since:queryparam}"
                               "&${since_hour:queryparam}&${until:queryparam}&${until_hour:queryparam}&${combo:queryparam}"},
                       {"title": "Reset", "type": "link", "icon": "bolt", "targetBlank": False, "keepTime": False, "includeVars": False,
                        "tooltip": "Every filter and the time range back to their defaults: Newest Live Run, Run start to Run end, the last 3 hours, "
                                   "refresh 10 s.",
                        "url": URL + "?orgId=1&from=now-3h&to=now&refresh=10s&var-run=__live&var-since=0&var-since_hour=00&var-until=%s"
                               "&var-until_hour=24" % NO_END_MS}],
             "time": {"from": "now-3h", "to": "now"}, "refresh": "10s", "timepicker": {"refresh_intervals": ["10s", "30s", "1m", "5m"]}, "tags": [],
             "description": "The ETF-pair trading program as a trader reads it, one Run at a time, laid out as the sibling dex-cex Live Monitor: "
                            "Status · Running Session (Execution, Margin, Alerts), Execution Summary, the position and market at the last "
                            "second, the deviation with every entry and exit, round trips and their orders, inventory, the Run's settings; "
                            "statistics and system folded; paper and live apart."}
    return board
