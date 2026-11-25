"""ETF-Pair Live Monitor: the trading program (core/) as it trades, from its own records in schema trade of the etfpair
database (core/store/schema.sql, core/SPEC.md 5.6), read by the read-only role etfpair_trade_reader through the data source
etfpair-trade (dashboard/SPEC.md section 7). Paper and live are never merged: every query keeps the rows whose live column
is the Mode picked (LIVE), and the top of the page says which mode it shows.

The rows, after the sibling repository's Live Monitor: Status of the running session; Live Execution, the spread with the
program's entry and exit lines and a marker at every entry, exit, completion, trim and forced exit (owner item 61); Summary
of the cycles closed in the picked scope (time range, newest run or session); Inventory & Account; Funding; Cycles, one row a
cycle, a click picks it for Cycle Detail; Execution Statistics and System, collapsed. Every time-series chart marks each
change of the anchors in use (owner item 62).

The spread unit is D (1 D = 0.5 bps of the two legs' total notional), never bps; money is USDT. The program's lines are those
of the run that wrote each sample (process_run.config: strategy.d_in, strategy.d_out): a Long enters at x <= -d_in sigma on
the buying side (Buy Both) and exits at x >= -d_out sigma on the selling side (Sell Both); a Short enters at x >= d_in sigma
on Sell Both and exits at x <= d_out sigma on Buy Both (core/SPEC.md 5.2). Every picked setting reaches SQL inside quotes
('$mode', '$cycle', '$nofill', '$scope'); the reader role is the boundary (dashboard/SPEC.md section 6).

Grafana's tooltip shows a nearby point's value in place of a NULL, so on the two execution charts every tooltip row has a
value at every point: a reason code where there is none (NO_VALUE), and a 0 on the rows without a decision for the marker
rows, which the tooltip hides (hideZeros); a real zero there is a code (ZERO) shown as 0."""
from boardkit import (BAD, BLUE, GOOD, GREY, LIGHT_GREY, NO_FIGURE, ORANGE, PURPLE, SIGNED, TEXT, TRADE, WARN, bands, board_slug, board_uid,
                      code_texts, col, custom_var, keep_fields, marks, named, organize, panel, query_var, reset_ids, row, series, sql, stat_strip,
                      table, timeseries, tooltip_only)

NAME = "live-monitor"   # boardkit.BOARDS: uid arb-etf-pair-live-monitor, address /dashboards/arbitrage/etf-pair/live-monitor
URL = "/d/%s/%s" % (board_uid(NAME), board_slug(NAME))
# The Mode picked, as a boolean: every table's live column is compared with it.
LIVE = "('$mode' = 'Live')"
NOW_MS = "(extract(epoch FROM now()) * 1000)::bigint"
# The end of the time range, or now when the range runs into the future.
END_MS = "least($__to, %s)" % NOW_MS
# How far back "the newest sample" is looked for: a bound on the primary key's time keeps the lookup an index range scan
# even when the picked mode has no recent row (the per-second table grows by 86,400 rows a day).
RECENT_MS = "%s - 172800000" % NOW_MS
NBSP = "\u00a0"
NBHY = "\u2011"   # a non-breaking hyphen: a browser may break a line after a plain one
UTC8 = "'Etc/GMT-8'"


def clock(ms, fmt="MM-DD HH24:MI:SS"):
    """A Unix millisecond time as UTC+8 text; no-break spaces and hyphens, so a narrow figure keeps it on one line."""
    return "to_char(to_timestamp((%s) / 1000.0) AT TIME ZONE %s, '%s')" % (ms, UTC8, fmt.replace(" ", NBSP).replace("-", NBHY))


def span(ms):
    """A duration in milliseconds as short text: 45s, 12m 05s, 3h 07m, 2d 04h (no-break spaces)."""
    x = "(%s)::bigint" % ms
    return ("CASE WHEN {x} IS NULL THEN NULL WHEN {x} < 60000 THEN ({x} / 1000) || 's' "
            "WHEN {x} < 3600000 THEN ({x} / 60000) || 'm{s}' || lpad((({x} / 1000) % 60)::text, 2, '0') || 's' "
            "WHEN {x} < 86400000 THEN ({x} / 3600000) || 'h{s}' || lpad((({x} / 60000) % 60)::text, 2, '0') || 'm' "
            "ELSE ({x} / 86400000) || 'd{s}' || lpad((({x} / 3600000) % 24)::text, 2, '0') || 'h' END").format(x=x, s=NBSP)


def shown(x, decimals):
    """x rounded as Grafana rounds it for display (Math.round of x * 10^d, halves up), once in SQL (dashboard/SPEC.md
    section 6): the frame is smaller and reads exactly as unrounded."""
    return "(floor((%s)::float8 * 1e%d::float8 + 0.5) / 1e%d::float8)" % (x, decimals, decimals)


def words(code, space=" "):
    """A program's code as words: no_signal -> No Signal, conflict_bear -> Conflict Bear."""
    return "initcap(replace(%s, '_', '%s'))" % (code, space)


# The step of the drawn points: the finest of 1, 5, 10, 30 seconds and whole minutes that keeps a line under about 1500
# points, about the plot's width in pixels at 1680 px. Each point is a bucket of the per-second samples: its last spread, mean and sigma, the lowest
# buying deviation and the highest selling deviation in it (the side each one triggers on), so a crossing between two drawn
# points is never lost.
STEP_MS = ("(SELECT CASE WHEN r <= 1000 THEN 1000 WHEN r <= 5000 THEN 5000 WHEN r <= 10000 THEN 10000 "
           "WHEN r <= 30000 THEN 30000 ELSE ceil(r / 60000) * 60000 END::bigint FROM (SELECT ($__to - $__from) / 1500.0 AS r) x)")

# Reason codes where a point has no value (code_texts maps them to text).
NO_VALUE = {-2000000000: "No Anchor", -3000000000: "No Book", -4000000000: "Warmup", -5000000000: "No Levels"}
CODE = {text: code for code, text in NO_VALUE.items()}
# A cycle's funding or net while a settlement it held through has no record yet (funding_known false): Pending, not 0.
PENDING = -9000000000
# A fee not reported yet for an order with fills (cum_exec_qty > 0 AND NOT fee_known); an order without a fill needs none.
FEE_PENDING = -9100000000
OPEN = -9200000000       # an open cycle's net in a marker's tooltip
NO_FILL = -9300000000    # a marker's average price without a fill
ZERO = -9400000000       # a real zero in a tooltip row of the execution charts (a 0 is hidden there)
NO_SAMPLE = -9500000000  # a completion's, trim's or forced exit's place: no sample with a spread in the minute before it


def nonzero(x, decimals):
    """A tooltip row's value as shown(), a real zero as ZERO (the tooltip hides a 0)."""
    return "CASE WHEN %s = 0 THEN %d ELSE %s END" % (shown(x, decimals), ZERO, shown(x, decimals))


def coded(value, decimals, why):
    """value shown() (a zero as ZERO), else the code of the first reason in why ((condition, text), ...) that holds, the last
    one standing for any other."""
    whens = ["WHEN %s IS NOT NULL THEN %s" % (value, nonzero(value, decimals))] + ["WHEN %s THEN %d" % (c, CODE[t]) for c, t in why[:-1]]
    return "CASE %s ELSE %d END" % (" ".join(whens), CODE[why[-1][1]])


# The program's settings of each run (process_run.config, the effective config.yaml).
D_IN = "(r.config -> 'strategy' ->> 'd_in')::float8"
D_OUT = "(r.config -> 'strategy' ->> 'd_out')::float8"


def latest_config(path, fallback):
    """A setting of the newest run of the Mode, as text, for titles."""
    return ("SELECT coalesce((SELECT r.config #>> '{%s}' FROM trade.process_run r WHERE r.live = %s ORDER BY r.started_ms DESC LIMIT 1), '%s')"
            % (path, LIVE, fallback))


# ---------------------------------------------------------------------------------------------------------------- Status
# The newest run, signal sample and inventory sample of the Mode, whatever the time range (samples of the last two days).
LATEST = sql("""r AS (SELECT * FROM trade.process_run WHERE live = @LIVE@ ORDER BY started_ms DESC LIMIT 1),
     s AS (SELECT * FROM trade.signal_sample WHERE live = @LIVE@ AND ts_ms > @RECENT@ ORDER BY ts_ms DESC LIMIT 1),
     i AS (SELECT * FROM trade.inventory_sample WHERE live = @LIVE@ AND ts_ms > @RECENT@ ORDER BY ts_ms DESC LIMIT 1)""", LIVE=LIVE, RECENT=RECENT_MS)
SAMPLE_AGE_S = 10   # a running process writes a sample every second; past this it is Lost
# What the open cycle's lots hold per leg (contracts left and their entry cost, USDT) and its direction.
HELD = sql("""h AS (SELECT sum(l.entry_qty_bull - l.exit_qty_bull) AS q_bull,
             sum(CASE WHEN l.entry_qty_bull > 0 THEN l.entry_value_bull * (l.entry_qty_bull - l.exit_qty_bull) / l.entry_qty_bull END) AS v_bull,
             sum(l.entry_qty_bear - l.exit_qty_bear) AS q_bear,
             sum(CASE WHEN l.entry_qty_bear > 0 THEN l.entry_value_bear * (l.entry_qty_bear - l.exit_qty_bear) / l.entry_qty_bear END) AS v_bear,
             max(c.direction) AS dir
      FROM trade.cycle c JOIN trade.lot l ON l.live = c.live AND l.cycle_id = c.cycle_id
      WHERE c.live = @LIVE@ AND c.status = 'open' AND l.status IN ('building', 'open'))""", LIVE=LIVE)
# The open cycle's unrealised PnL at the newest second's mids, before exit fees: what selling (a Long) or buying back (a
# Short) both legs at their mids gives against their entry cost.
UNREALISED = ("CASE WHEN h.dir IS NOT NULL THEN (CASE h.dir WHEN 'short' THEN -1 ELSE 1 END) * "
              "(s.mid_bull * h.q_bull - h.v_bull + s.mid_bear * h.q_bear - h.v_bear) END")
# An idle reason that needs a person (red) or a look (amber); the others are the strategy's ordinary answers.
ENTRY_BAD = ("position_mismatch", "store_error", "settings", "not_ready", "order_unknown")
ENTRY_WARN = ("fx_stale", "conflict_bull", "conflict_bear", "depth_bull", "depth_bear", "await_depth", "await_orders", "rejected_backoff",
              "no_anchor", "action_window", "quota_exhausted", "stopping", "no_new_entries", "capital_pending", "depth", "no_price")
Q_STATUS = sql("""
WITH @LATEST@, @HELD@
SELECT CASE WHEN r.run_id IS NULL THEN 'No Run' WHEN r.stopped_ms IS NOT NULL THEN 'Stopped'
            WHEN s.run_id IS DISTINCT FROM r.run_id AND @NOW@ - r.started_ms < 60000 THEN 'Starting'
            WHEN s.ts_ms IS NULL OR @NOW@ - s.ts_ms > @AGE@ * 1000 THEN 'Lost'
            ELSE 'Up@S@' || @UP@ END AS "Process",
       round((@NOW@ - s.ts_ms) / 1000.0) AS "Sample Age",
       CASE s.phase WHEN 'off_hours' THEN 'Off@H@Hours' WHEN 'regular' THEN 'Regular' WHEN 'close_window' THEN 'Close@S@Window' ELSE s.phase END AS "Phase",
       CASE WHEN s.ts_ms IS NULL THEN NULL WHEN s.anchor_kind = 'nav' THEN 'NAV' WHEN s.anchor_kind = 'bell' THEN 'Bell' ELSE 'Missing' END AS "Anchor",
       CASE WHEN s.ts_ms IS NULL THEN NULL WHEN s.stats_ready THEN 'Ready' ELSE 'Warmup' END AS "Mean · σ",
       @CLOSE_AT@ AS "Close At",
       CASE WHEN s.session_end_ms > @NOW@ THEN @CLOSE_IN@ WHEN s.session_end_ms IS NOT NULL THEN 'Passed' END AS "Close In",
       CASE WHEN s.ts_ms IS NULL THEN NULL WHEN s.idle_reason IS NULL THEN 'Order@S@Sent' ELSE @IDLE@ END AS "Entry",
       CASE WHEN s.ts_ms IS NULL THEN NULL WHEN s.direction IS NULL THEN 'Flat' ELSE initcap(s.direction) END AS "Position",
       s.hold_bull AS "Bull Held", s.hold_bear AS "Bear Held", @UNREALISED@ AS "Unrealised",
       (SELECT sum(c.net) FROM trade.cycle c WHERE c.live = @LIVE@ AND c.session_id = s.session_id AND c.status <> 'open' AND c.funding_known)
         AS "Session Net",
       CASE WHEN i.ts_ms IS NULL THEN NULL WHEN i.mismatch THEN 'Mismatch' ELSE 'None' END AS "Mismatch"
FROM (SELECT 1) one LEFT JOIN r ON true LEFT JOIN s ON true LEFT JOIN i ON true LEFT JOIN h ON true""", LATEST=LATEST, HELD=HELD, LIVE=LIVE,
               NOW=NOW_MS, AGE=str(SAMPLE_AGE_S), UP=span("%s - r.started_ms" % NOW_MS), CLOSE_AT=clock("s.session_end_ms", "MM-DD HH24:MI"),
               CLOSE_IN=span("s.session_end_ms - %s" % NOW_MS), IDLE=words("s.idle_reason", NBSP), UNREALISED=shown(UNREALISED, 2), S=NBSP, H=NBHY)

# The note at the top: which mode the page shows; paper sends no order to the venue.
Q_MODE = sql("""
SELECT CASE WHEN @LIVE@ THEN 'Live Mode · Orders Go to Bitget' ELSE 'Paper Mode · No Orders Leave the Process' END AS "Mode" """, LIVE=LIVE)

# Warnings and errors of every module in the time range, newest first; a row that says so when there is none.
Q_ALERTS = sql("""
SELECT * FROM (
  SELECT to_timestamp(e.ts_ms / 1000.0) AS "Time", initcap(e.level) AS "Level", e.module AS "Module", e.kind AS "Kind", e.message AS "Message",
         e.detail::text AS "Detail"
  FROM trade.event e
  WHERE e.live = @LIVE@ AND e.level IN ('warn', 'error') AND e.ts_ms >= $__from AND e.ts_ms <= $__to
  ORDER BY e.ts_ms DESC, e.seq DESC LIMIT 200) a
UNION ALL
SELECT NULL, NULL, NULL, NULL, 'No warning or error in the time range', NULL
WHERE NOT EXISTS (SELECT 1 FROM trade.event e WHERE e.live = @LIVE@ AND e.level IN ('warn', 'error') AND e.ts_ms >= $__from AND e.ts_ms <= $__to)""",
               LIVE=LIVE)

# ------------------------------------------------------------------------------------------------------- Live Execution
# The decisions drawn as markers: every entry, exit, completion, trim and forced exit, at its instant; an entry whose lot
# became void (no fill, one leg) is a Void Entry. Each with its orders' fills: both legs' filled contracts and average price,
# the notional (USDT, both legs) and the fees (a cost, negative), and its cycle's net (Pending, or Open while open). y on the
# Spread chart: an entry's or exit's executable spread at the decision (its mean + x); a completion's, trim's or forced exit's
# the spread of the newest sample before it, as is its x on the deviation chart (spread - mean).
DECISION_KINDS = ("entry", "exit", "complete", "trim", "forced")
ACTIONS = {1: "Long Entry", 2: "Short Entry", 3: "Long Exit", 4: "Short Exit", 5: "Forced Exit", 6: "Trim", 7: "Complete", 8: "Void Entry"}
ACTION_COLORS = {"Long Entry": BLUE, "Short Entry": ORANGE, "Long Exit": BLUE, "Short Exit": ORANGE, "Forced Exit": WARN, "Trim": GREY,
                 "Complete": GREY, "Void Entry": GREY}
MARKERS = sql("""
m0 AS (
  SELECT d.ts_ms, d.run_id, d.kind, d.direction, (d.detail ->> 'x_d')::float8 AS x, (d.detail ->> 'mu_d')::float8 AS dmu,
         CASE WHEN d.kind = 'entry' AND l.status = 'void' THEN 8 WHEN d.kind = 'entry' THEN CASE d.direction WHEN 'short' THEN 2 ELSE 1 END
              WHEN d.kind = 'exit' THEN CASE d.direction WHEN 'short' THEN 4 ELSE 3 END WHEN d.kind = 'forced' THEN 5 WHEN d.kind = 'trim' THEN 6
              ELSE 7 END AS action,
         CASE WHEN c.status = 'open' THEN @OPEN@ WHEN NOT c.funding_known THEN @PENDING@ ELSE c.net END AS cycle_net,
         coalesce(d.detail -> 'client_order_ids', jsonb_build_array(d.detail ->> 'client_order_id')) AS ids
  FROM trade.decision d
  LEFT JOIN trade.lot l ON l.live = d.live AND l.lot_id = d.lot_id
  LEFT JOIN trade.cycle c ON c.live = d.live AND c.cycle_id = d.cycle_id
  WHERE d.live = @LIVE@ AND d.kind IN @KINDS@ AND d.ts_ms >= $__from AND d.ts_ms <= $__to),
m AS (
  SELECT m0.ts_ms, m0.run_id, m0.action, m0.cycle_net, coalesce(m0.dmu + m0.x, n.mid_d) AS y, coalesce(m0.x, n.mid_d - n.mu_d) AS dev, o.*
  FROM m0
  LEFT JOIN LATERAL (SELECT s.mid_d, s.mu_d FROM trade.signal_sample s WHERE s.live = @LIVE@ AND s.ts_ms <= m0.ts_ms AND s.ts_ms > m0.ts_ms - 60000
                     ORDER BY s.ts_ms DESC LIMIT 1) n ON m0.x IS NULL
  CROSS JOIN LATERAL (
    SELECT sum(o.cum_exec_qty) FILTER (WHERE o.leg = 'bull') AS q_bull, sum(o.cum_exec_value) FILTER (WHERE o.leg = 'bull') AS v_bull,
           sum(o.cum_exec_qty) FILTER (WHERE o.leg = 'bear') AS q_bear, sum(o.cum_exec_value) FILTER (WHERE o.leg = 'bear') AS v_bear,
           sum(o.fee) AS fee, bool_or(o.cum_exec_qty > 0 AND NOT o.fee_known) AS fee_pending
    FROM trade.cex_order o WHERE o.live = @LIVE@ AND o.client_order_id IN (SELECT jsonb_array_elements_text(m0.ids))) o)""",
              LIVE=LIVE, KINDS="(%s)" % ", ".join("'%s'" % k for k in DECISION_KINDS), OPEN=str(OPEN), PENDING=str(PENDING))


# The marker columns: the drawn dots, on a decision's own row only (NULL elsewhere), and the tooltip rows. A dot is a few
# pixels wide, so the tooltip rows hold the decision c nearest a row within about 8 px of the plot (WINDOW_MS) on every row
# there, the decision's own row included, and 0 elsewhere, which the tooltip hides: hovering near a dot shows it, and
# nowhere else does a decision show. Decided is its instant.
def near(x):
    """A tooltip row of the decision near a row: 0 (hidden) where there is none."""
    return "CASE WHEN c.n IS NULL THEN 0 ELSE %s END" % x


MARKER_COLUMNS = [(name, "CASE WHEN p.mark AND c.action = %d THEN %s END" % (a, shown("c.y", 2))) for a, name in ACTIONS.items()] + [
    ("%s · Deviation" % name, "CASE WHEN p.mark AND c.action = %d THEN %s END" % (a, shown("c.dev", 2))) for a, name in ACTIONS.items()] + [
    ("Action", near("c.action")), ("Decided", near("c.ts_ms")),
    ("Bull Filled", near(nonzero("coalesce(c.q_bull, 0)", 2))),
    ("Bull Price", near("CASE WHEN c.q_bull > 0 THEN %s ELSE %d END" % (shown("c.v_bull / c.q_bull", 4), NO_FILL))),
    ("Bear Filled", near(nonzero("coalesce(c.q_bear, 0)", 2))),
    ("Bear Price", near("CASE WHEN c.q_bear > 0 THEN %s ELSE %d END" % (shown("c.v_bear / c.q_bear", 4), NO_FILL))),
    ("Notional", near(nonzero("coalesce(c.v_bull, 0) + coalesce(c.v_bear, 0)", 2))),
    ("Fees", near("CASE WHEN c.fee_pending THEN %d ELSE %s END" % (FEE_PENDING, nonzero("-coalesce(c.fee, 0)", 4)))),
    ("Cycle Net", near("CASE WHEN c.cycle_net <= %d THEN c.cycle_net ELSE %s END" % (PENDING, nonzero("c.cycle_net", 2)))),
    ("At", near("CASE WHEN c.y IS NULL THEN %d ELSE %s END" % (NO_SAMPLE, nonzero("c.y", 2)))),
    ("At · Deviation", near("CASE WHEN c.dev IS NULL THEN %d ELSE %s END" % (NO_SAMPLE, nonzero("c.dev", 2))))]
WINDOW_MS = "greatest(x.step, ($__to - $__from) / 150)"

# The columns of a drawn point: the lines (Spread, Mean, the program's four lines, the executable spreads), the deviations
# (zero at the Mean) and, for the tooltips, each value or the reason it has none. The program's lines and the deviations need
# the mean and sigma; the executable ones the levels.
MID_WHY = (("NOT p.anchored", "No Anchor"), ("true", "No Book"))
MU_WHY = (("NOT p.anchored", "No Anchor"), ("true", "Warmup"))
X_WHY = (("NOT p.anchored", "No Anchor"), ("p.mu IS NULL", "Warmup"), ("true", "No Levels"))
LINES = {"Long Entry · Buy Both": ("-p.d_in", BLUE, [6, 4]), "Short Entry · Sell Both": ("p.d_in", ORANGE, [6, 4]),
         "Long Exit · Sell Both": ("-p.d_out", ORANGE, [2, 3]), "Short Exit · Buy Both": ("p.d_out", BLUE, [2, 3])}
# The drawn lines, computed here: Grafana's binary calculation reads a NULL operand as 0, which would draw a break row of
# the lines at zero. Spread chart: the spread, the mean, the executable spreads, the program's four lines; deviation chart:
# the same less the mean.
LINE_COLUMNS = ([("Spread", shown("p.mid", 2)), ("Mean", shown("p.mu", 2)), ("Buy Both", shown("p.mu + p.xl", 2)), ("Sell Both", shown("p.mu + p.xs", 2))] +
                [(name, shown("p.mu + %s * p.sigma" % k, 2)) for name, (k, _, _) in LINES.items()] +
                [("Deviation", shown("p.mid - p.mu", 2)), ("Buy Both x", shown("p.xl", 2)), ("Sell Both x", shown("p.xs", 2))] +
                [(name + " · x", shown("%s * p.sigma" % k, 2)) for name, (k, _, _) in LINES.items()])
TOOLTIP_COLUMNS = [
    ("Spread · Tooltip", coded("p.mid", 2, MID_WHY)), ("Mean · Tooltip", coded("p.mu", 2, MU_WHY)), ("Sigma · Tooltip", coded("p.sigma", 2, MU_WHY)),
    ("Buy Both · Tooltip", coded("p.mu + p.xl", 2, X_WHY)), ("Sell Both · Tooltip", coded("p.mu + p.xs", 2, X_WHY)),
    ("Deviation · Tooltip", coded("p.mid - p.mu", 2, MU_WHY)), ("Buy Both x · Tooltip", coded("p.xl", 2, X_WHY)),
    ("Sell Both x · Tooltip", coded("p.xs", 2, X_WHY)), ("d_in σ · Tooltip", coded("p.d_in * p.sigma", 2, MU_WHY))]
# The program's answer in the bucket's last second and the position then, as codes for the tooltip (never 0: hidden there).
IDLE_REASONS = ["no_signal", "regular_hours", "spacing", "quota_exhausted", "cost_not_covered", "holding_other_side", "warmup", "zero_sigma",
                "no_levels", "too_small", "conflict_bull", "conflict_bear", "depth_bull", "depth_bear", "fx_stale", "await_depth", "await_orders",
                "order_unknown", "rejected_backoff", "no_anchor", "action_window", "stopping", "no_new_entries", "not_ready", "settings",
                "store_error", "position_mismatch", "capital_pending", "positions_read", "depth", "no_price", "await_close"]
IDLE_CODES = {100: "Order Sent", **{i + 1: r.replace("_", " ").title() for i, r in enumerate(IDLE_REASONS)}, 99: "Other"}
IDLE_COLORS = dict({"Order Sent": TEXT}, **{r.replace("_", " ").title(): BAD for r in ENTRY_BAD}, **{r.replace("_", " ").title(): WARN for r in ENTRY_WARN})
STATE_COLUMNS = [
    ("Entry", "CASE WHEN p.idle IS NULL THEN 100 %s ELSE 99 END" % " ".join("WHEN p.idle = '%s' THEN %d" % (r, i + 1) for i, r in enumerate(IDLE_REASONS))),
    ("Position", "CASE p.dir WHEN 'long' THEN 1 WHEN 'short' THEN 2 ELSE 3 END")]
POSITION_CODES = {3: "Flat", 1: "Long", 2: "Short"}
# A decision row that carries no bucket's values (before the range's first point, or of another run than the point before
# it) has no state: its line, tooltip and state columns are hidden there (NULL breaks the lines, 0 hides the tooltip rows).
ALL_COLUMNS = LINE_COLUMNS + [(n, "CASE WHEN p.anchored IS NULL THEN 0 ELSE %s END" % q) for n, q in TOOLTIP_COLUMNS + STATE_COLUMNS] + MARKER_COLUMNS
RAW = ["mid", "mu", "sigma", "xl", "xs", "d_in", "d_out", "anchored", "idle", "dir"]
# The Spread chart's one query (the deviation chart shows its result): per step bucket of one run and one session, at its last
# sample; each decision as a row of its own at its instant, with the values of the bucket before it when that is of the same
# run (so it does not break the lines), and on every row the decision nearest it within WINDOW_MS, for the tooltip; a row
# with only its time 1 ms before a point that is more than two steps (at least 3 s) after the one before it, or of another
# session or run (the mean restarts), breaks the lines there, after the decisions of a run's last moments; rows of the same
# millisecond come break row, point, decision.
Q_SPREAD = sql("""
WITH x AS MATERIALIZED (SELECT @STEP@ AS step),
b AS (
  SELECT max(s.ts_ms) AS ts, s.run_id, s.session_id,
         (array_agg(s.mid_d ORDER BY s.ts_ms DESC))[1] AS mid, (array_agg(s.mu_d ORDER BY s.ts_ms DESC))[1] AS mu,
         (array_agg(s.sigma_d ORDER BY s.ts_ms DESC))[1] AS sigma, min(s.x_long_d) AS xl, max(s.x_short_d) AS xs,
         (array_agg(s.anchor_kind IS NOT NULL ORDER BY s.ts_ms DESC))[1] AS anchored, (array_agg(s.idle_reason ORDER BY s.ts_ms DESC))[1] AS idle,
         (array_agg(s.direction ORDER BY s.ts_ms DESC))[1] AS dir
  FROM trade.signal_sample s CROSS JOIN x
  WHERE s.live = @LIVE@ AND s.ts_ms >= $__from AND s.ts_ms <= $__to
  GROUP BY s.ts_ms / x.step, s.run_id, s.session_id),
k AS (
  SELECT b.*, @D_IN@ AS d_in, @D_OUT@ AS d_out,
         lead(b.ts) OVER w AS next_ts, lead(b.run_id) OVER w AS next_run, lead(b.session_id) OVER w AS next_session
  FROM b LEFT JOIN trade.process_run r ON r.run_id = b.run_id AND r.live = @LIVE@
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
ORDER BY t.ts, t.ord""", STEP=STEP_MS, LIVE=LIVE, D_IN=D_IN, D_OUT=D_OUT, MARKERS=MARKERS,
               RAW_K=", ".join("k.%s" % c for c in RAW), NULL_RAW=", ".join("NULL" for _ in RAW), WINDOW=WINDOW_MS,
               WIN_RAW=", ".join("CASE WHEN u.run IS DISTINCT FROM u.brun THEN NULL ELSE %s(u.%s) OVER v END AS %s"
                                 % ("bool_or" if c == "anchored" else "max", c, c) for c in RAW),
               NAMES=", ".join('t."%s"' % n for n, _ in ALL_COLUMNS),
               COLUMNS=", ".join('%s AS "%s"' % (q, n) for n, q in ALL_COLUMNS), NULLS=", ".join('NULL AS "%s"' % n for n, _ in ALL_COLUMNS))


# The cycles as shaded spans on both execution charts, from their opening to their close (now while open), one query per
# direction; the text is what the span's marker under the chart shows.
def cycle_spans(direction):
    return sql("""
SELECT c.opened_ms AS time, coalesce(c.closed_ms, @NOW@) AS time_end,
       initcap(c.direction) || ' cycle ' || @OPENED@ || ' – ' || coalesce(@CLOSED@, 'open') || ' · ' || coalesce(@REASON@, 'Open') || ' · '
       || (SELECT count(*) FROM trade.lot l WHERE l.live = @LIVE@ AND l.cycle_id = c.cycle_id AND l.status <> 'void') || ' lots · Net '
       || CASE WHEN c.status = 'open' THEN 'open' WHEN NOT c.funding_known THEN 'Pending' ELSE to_char(c.net, 'FM999,990.00') || ' USDT' END AS text
FROM trade.cycle c
WHERE c.live = @LIVE@ AND c.direction = '@DIR@' AND c.opened_ms <= $__to AND coalesce(c.closed_ms, @NOW@) >= $__from""",
               LIVE=LIVE, NOW=NOW_MS, DIR=direction, OPENED=clock("c.opened_ms", "MM-DD HH24:MI:SS"), CLOSED=clock("c.closed_ms", "HH24:MI:SS"),
               REASON=words("c.end_reason"))


# Owner item 62: a mark on every time-series chart at each instant the anchors in use change, worded as Combo Spread's
# (3.5): "Anchor change · reason", then each leg's anchor before -> after in USD and in USDT (USD ÷ F), then F. The instants
# are the program's own: a sample whose anchor kind or either leg's value differs from the sample before (up to 10 min before
# the range). Reasons: a new session (its close), on the bell; the NAV confirmed (both legs' NAV unchanged for 1 h; when first
# seen and confirmed, from anchor_fact), replacing the bell; the bell again (a restart before the NAV is confirmed again);
# within its kind: a dividend when one was read for the session by then, else a value read again at a restart, else a split
# or a corrected reading; no anchor. F: each side's own sample's, the rate the program used.
USD2 = "coalesce(to_char(%s, 'FM999,990.00'), '–')"
ANCHOR_PAIR = sql("""'<br>' || '$bull ' || @B0@ || ' → ' || @B1@ || ' USD · ' || @U0@ || ' → ' || @U1@ || ' USDT<br>'
       || '$bear ' || @E0@ || ' → ' || @E1@ || ' USD · ' || @V0@ || ' → ' || @V1@ || ' USDT<br>USDT = USD ÷ F, F = '
       || CASE WHEN x.pf = x.f THEN x.f::text ELSE coalesce(x.pf::text, '–') || ' → ' || coalesce(x.f::text, '–') END || ' USD/USDT (the program''s)'""",
                  B0=USD2 % "x.p_bull", B1=USD2 % "x.anchor_bull", U0=USD2 % "x.p_bull / x.pf", U1=USD2 % "x.anchor_bull / x.f",
                  E0=USD2 % "x.p_bear", E1=USD2 % "x.anchor_bear", V0=USD2 % "x.p_bear / x.pf", V1=USD2 % "x.anchor_bear / x.f")


def fact_time(kind, agg):
    """When the program read a fact of the session at or before the change, UTC+8."""
    return ("(SELECT %s FROM trade.anchor_fact a WHERE a.live = %s AND a.session_id = x.session_id AND a.kind = '%s' AND a.known_ms <= x.ts_ms)"
            % (clock("%s(a.known_ms)" % agg, "HH24:MI"), LIVE, kind))


DIVIDEND = ("(SELECT string_agg(CASE a.leg WHEN 'bull' THEN '$bull' ELSE '$bear' END || ' ' || trim_scale(a.value), ', ') FROM trade.anchor_fact a "
            "WHERE a.live = %s AND a.session_id = x.session_id AND a.kind = 'dividend' AND a.known_ms <= x.ts_ms)" % LIVE)
Q_ANCHOR_CHANGES = sql("""
SELECT x.ts_ms AS time,
       'Anchor change · ' ||
       CASE WHEN x.anchor_kind IS NULL THEN 'no anchor (Missing): no entries until one is read'
            WHEN x.ps IS DISTINCT FROM x.session_id THEN 'new session ' || x.session_id || ' (the close)<br>bell anchor (F × Binance''s index at the close; the NAV to follow)'
            WHEN x.pk IS NULL THEN 'an anchor read again'
            WHEN x.pk = 'bell' AND x.anchor_kind = 'nav' THEN 'NAV confirmed for session ' || x.session_id
                 || '<br>both legs'' NAV unchanged for 1 h (Direxion' || coalesce(': first seen ' || @SEEN@ || ', confirmed ' || @KNOWN@, '') || ')'
            WHEN x.pk = 'nav' AND x.anchor_kind = 'bell' THEN 'the bell again: the NAV not yet confirmed in this run'
            WHEN @DIVIDEND@ IS NOT NULL THEN 'a dividend takes effect (anchor − D; read for the session: ' || @DIVIDEND@ || ' USD)'
            WHEN x.prun IS DISTINCT FROM x.run_id THEN 'the anchor read again'
            ELSE 'a split, or a corrected reading, of the same kind'
       END
       || CASE WHEN x.prun IS DISTINCT FROM x.run_id THEN ' · at a restart of the program' ELSE '' END || @PAIR@ AS text
FROM (SELECT s.ts_ms, s.run_id, s.session_id, s.anchor_kind, s.anchor_bull, s.anchor_bear, s.f,
             lag(s.ts_ms) OVER w AS pts, lag(s.run_id) OVER w AS prun, lag(s.session_id) OVER w AS ps, lag(s.anchor_kind) OVER w AS pk,
             lag(s.anchor_bull) OVER w AS p_bull, lag(s.anchor_bear) OVER w AS p_bear, lag(s.f) OVER w AS pf
      FROM trade.signal_sample s WHERE s.live = @LIVE@ AND s.ts_ms >= $__from - 600000 AND s.ts_ms <= $__to
      WINDOW w AS (ORDER BY s.ts_ms)) x
WHERE x.ts_ms >= $__from AND x.pts IS NOT NULL
  AND (x.anchor_kind, x.anchor_bull, x.anchor_bear) IS DISTINCT FROM (x.pk, x.p_bull, x.p_bear)""".replace("@DIVIDEND@", DIVIDEND),
                       LIVE=LIVE, SEEN=fact_time("nav_seen", "min"), KNOWN=fact_time("nav", "max"), PAIR=ANCHOR_PAIR)

# ---------------------------------------------------------------------------------------------------------------- Summary
# The scope the Summary row covers (Scope): the cycles that closed in the time range; since the newest run started; or the
# cycles of the newest sample's session. A cycle whose funding is not known yet (funding_known false) is Pending: left out of
# every money figure (Net, Gross, Fees, Funding, Return, Win Rate) until its funding is known; Pending counts them.
SCOPE_LO = ("CASE '$scope' WHEN 'run' THEN coalesce((SELECT r.started_ms FROM trade.process_run r WHERE r.live = %s ORDER BY r.started_ms DESC LIMIT 1), 0) "
            "WHEN 'session' THEN 0 ELSE $__from END" % LIVE)
SCOPE_HI = "CASE '$scope' WHEN 'range' THEN $__to ELSE %s END" % NOW_MS
SESSION = ("(SELECT s.session_id FROM trade.signal_sample s WHERE s.live = %s AND s.ts_ms > %s ORDER BY s.ts_ms DESC LIMIT 1)" % (LIVE, RECENT_MS))
CLOSED = ("trade.cycle c WHERE c.live = %s AND c.status <> 'open' AND c.closed_ms >= %s AND c.closed_ms <= %s "
          "AND ('$scope' <> 'session' OR c.session_id = %s)" % (LIVE, SCOPE_LO, SCOPE_HI, SESSION))
Q_SUMMARY = sql("""
WITH c AS MATERIALIZED (SELECT c.* FROM @CLOSED@),
k AS (SELECT * FROM c WHERE c.funding_known AND c.end_reason IS DISTINCT FROM 'no_fill'),
l AS (SELECT l.* FROM trade.lot l JOIN c ON c.cycle_id = l.cycle_id WHERE l.live = @LIVE@),
st AS (SELECT capital FROM trade.inventory_state WHERE live = @LIVE@)
SELECT (SELECT sum(k.net) FROM k) AS "Net", (SELECT sum(k.gross) FROM k) AS "Gross", (SELECT -sum(k.fees) FROM k) AS "Fees",
       (SELECT sum(k.funding) FROM k) AS "Funding",
       (SELECT 100 * sum(k.net) / nullif((SELECT capital FROM st), 0) FROM k) AS "Return",
       (SELECT sum(o.cum_exec_value) FROM trade.cex_order o JOIN c ON c.cycle_id = o.cycle_id WHERE o.live = @LIVE@) AS "Notional",
       (SELECT count(*) FILTER (WHERE c.end_reason IS DISTINCT FROM 'no_fill') FROM c) AS "Cycles",
       (SELECT 100.0 * count(*) FILTER (WHERE k.net > 0) / nullif(count(*), 0) FROM k) AS "Win Rate",
       (SELECT count(*) FILTER (WHERE l.status <> 'void') FROM l) AS "Lots",
       (SELECT count(*) FILTER (WHERE l.status = 'void') FROM l) AS "Void",
       (SELECT count(*) FILTER (WHERE NOT c.funding_known) FROM c) AS "Pending" """, CLOSED=CLOSED, LIVE=LIVE)

# The running net and gross of the scope's known cycles, a step at each close: from the start of the scope (or of the time
# range, the sum before it carried in) to the end.
Q_CUMULATIVE = sql("""
WITH c AS MATERIALIZED (SELECT c.* FROM @CLOSED@ AND c.funding_known),
a AS (SELECT greatest(@LO@, $__from) AS t0)
SELECT to_timestamp(t.ts / 1000.0) AS time, sum(t.net) OVER w AS "Net", sum(t.gross) OVER w AS "Gross"
FROM (SELECT a.t0 AS ts, coalesce((SELECT sum(c.net) FROM c WHERE c.closed_ms < a.t0), 0) AS net,
             coalesce((SELECT sum(c.gross) FROM c WHERE c.closed_ms < a.t0), 0) AS gross FROM a
      UNION ALL SELECT c.closed_ms, c.net, c.gross FROM c, a WHERE c.closed_ms >= a.t0
      UNION ALL SELECT least(@HI@, @END@), 0, 0) t
WHERE EXISTS (SELECT 1 FROM c)
WINDOW w AS (ORDER BY t.ts ROWS UNBOUNDED PRECEDING)
ORDER BY t.ts""", CLOSED=CLOSED, LO=SCOPE_LO, HI=SCOPE_HI, END=END_MS)

# Each part's contribution to the scope's net: a gain above zero, a loss below.
Q_COMPOSITION = sql("""
WITH k AS MATERIALIZED (SELECT c.* FROM @CLOSED@ AND c.funding_known)
SELECT x.part AS "Part", CASE WHEN x.v >= 0 THEN x.v END AS "Gain", CASE WHEN x.v < 0 THEN x.v END AS "Loss"
FROM (SELECT 1 AS i, 'Gross' AS part, (SELECT sum(k.gross) FROM k) AS v
      UNION ALL SELECT 2, 'Fees', (SELECT -sum(k.fees) FROM k)
      UNION ALL SELECT 3, 'Funding', (SELECT sum(k.funding) FROM k)
      UNION ALL SELECT 4, 'Net', (SELECT sum(k.net) FROM k)) x
WHERE EXISTS (SELECT 1 FROM k)
ORDER BY x.i""", CLOSED=CLOSED)

# Each closed cycle's net (USDT) in the colour of its direction, under its opening time (UTC+8); Pending cycles left out, as
# from every money figure of the row.
Q_NET_PER_CYCLE = sql("""
SELECT @OPENED@ || CASE WHEN count(*) OVER (PARTITION BY c.opened_ms / 1000) > 1 THEN ' #' || row_number() OVER (PARTITION BY c.opened_ms / 1000 ORDER BY c.opened_ms, c.cycle_id) ELSE '' END
         AS "Cycle", CASE WHEN c.direction = 'long' THEN c.net END AS "Long", CASE WHEN c.direction = 'short' THEN c.net END AS "Short"
FROM @CLOSED@ AND c.end_reason IS DISTINCT FROM 'no_fill' AND c.funding_known
ORDER BY c.opened_ms""", CLOSED=CLOSED, OPENED=clock("c.opened_ms", "MM-DD HH24:MI:SS"))

# ------------------------------------------------------------------------------------------------------ Inventory & Account
# Per leg now: the venue's position and the ledger's (dust included), their difference; what the open cycle holds, its
# average entry price, the newest second's mid and the unrealised PnL at that mid (before exit fees); the dust recorded; the
# venue's mark, unrealised PnL and liquidation price (live only).
Q_POSITIONS = sql("""
WITH @LATEST@, @HELD@,
st AS (SELECT * FROM trade.inventory_state WHERE live = @LIVE@)
SELECT k.leg AS "Leg", k.pos AS "Exchange", k.ledger AS "Ledger", k.pos - k.ledger AS "Difference", k.held AS "Cycle Holds",
       CASE WHEN k.held <> 0 THEN k.cost / abs(k.held) END AS "Entry Price", k.mid AS "Mid",
       CASE WHEN k.held <> 0 THEN @UPL@ END AS "Unrealised at Mid", k.dust AS "Dust", k.mark AS "Mark", k.upl AS "Venue Unrealised",
       k.liq AS "Liquidation", @AGE@ AS "Age"
FROM (SELECT 1) one LEFT JOIN r ON true LEFT JOIN s ON true LEFT JOIN i ON true LEFT JOIN st ON true LEFT JOIN h ON true
CROSS JOIN LATERAL (VALUES
  (1, '$bull', i.pos_bull, i.ledger_bull, (st.dust -> 'bull' ->> 'qty')::numeric, CASE h.dir WHEN 'short' THEN -h.q_bull ELSE h.q_bull END, h.v_bull,
   s.mid_bull, i.mark_bull, i.upl_bull, i.liq_bull),
  (2, '$bear', i.pos_bear, i.ledger_bear, (st.dust -> 'bear' ->> 'qty')::numeric, CASE h.dir WHEN 'short' THEN -h.q_bear ELSE h.q_bear END, h.v_bear,
   s.mid_bear, i.mark_bear, i.upl_bear, i.liq_bear)) k(n, leg, pos, ledger, dust, held, cost, mid, mark, upl, liq)
WHERE i.ts_ms IS NOT NULL
ORDER BY k.n""", LATEST=LATEST, HELD=HELD, LIVE=LIVE, UPL=shown("(k.mid - k.cost / abs(k.held)) * k.held", 2),
                 AGE="round((%s - i.ts_ms) / 1000.0)" % NOW_MS)

# The ledger now: R, the quota (total, used, left, used share), the capital and leverage of the newest run, a mismatch.
Q_LEDGER = sql("""
WITH @LATEST@, st AS (SELECT * FROM trade.inventory_state WHERE live = @LIVE@)
SELECT i.r AS "R", i.quota_total AS "Quota Total", i.quota_used AS "Quota Used", s.quota_left AS "Quota Left",
       CASE WHEN i.quota_total > 0 THEN 100 * i.quota_used / i.quota_total END AS "Used",
       st.capital AS "Capital", (r.config -> 'inventory' ->> 'leverage')::numeric AS "Leverage",
       CASE WHEN i.ts_ms IS NULL THEN NULL WHEN i.mismatch THEN 'Mismatch' ELSE 'None' END AS "Mismatch"
FROM (SELECT 1) one LEFT JOIN r ON true LEFT JOIN s ON true LEFT JOIN i ON true LEFT JOIN st ON true""", LATEST=LATEST, LIVE=LIVE)

# The inventory samples of the range (every 10 s), at most about 1500: the newest of each range / 1500 (DISTINCT ON, one sort).
INVENTORY = """
SELECT to_timestamp(i.ts_ms / 1000.0) AS time, @COLUMNS@
FROM (SELECT DISTINCT ON (i.ts_ms / greatest(1, ($__to - $__from) / 1500)) i.*
      FROM trade.inventory_sample i
      WHERE i.live = @LIVE@ AND i.ts_ms >= $__from AND i.ts_ms <= $__to @WHERE@
      ORDER BY i.ts_ms / greatest(1, ($__to - $__from) / 1500), i.ts_ms DESC) i
ORDER BY i.ts_ms""".replace("@LIVE@", LIVE)
Q_POSITION_HISTORY = sql(INVENTORY, WHERE="", COLUMNS='i.pos_bull AS "Bull Exchange", i.ledger_bull AS "Bull Ledger", i.pos_bear AS "Bear Exchange", '
                                                    'i.ledger_bear AS "Bear Ledger"')
Q_EQUITY = sql(INVENTORY, WHERE="AND i.equity IS NOT NULL", COLUMNS='i.equity AS "Equity"')
Q_MARGIN = sql(INVENTORY, WHERE="AND i.margin_ratio IS NOT NULL", COLUMNS='i.margin_ratio AS "Margin Ratio"')
Q_QUOTA = sql(INVENTORY, WHERE="", COLUMNS='i.quota_used AS "Quota Used", i.quota_total AS "Quota Total"')

# ------------------------------------------------------------------------------------------------------------------ Funding
# The settled rate of each settlement in the range, both legs, in bps per settlement (rate x 10^4).
RATES = ("SELECT f.settlement_ms, max(f.rate) FILTER (WHERE f.leg = 'bull') AS bull, max(f.rate) FILTER (WHERE f.leg = 'bear') AS bear "
         "FROM trade.funding_rate f WHERE f.live = %s GROUP BY f.settlement_ms" % LIVE)
Q_FUNDING_RATE = sql("""
WITH f AS (@RATES@)
SELECT to_timestamp(f.settlement_ms / 1000.0) AS time, @BULL@ AS "Bull Rate", @BEAR@ AS "Bear Rate"
FROM f WHERE f.settlement_ms >= $__from AND f.settlement_ms <= $__to
ORDER BY f.settlement_ms""", RATES=RATES, BULL=shown("10000 * f.bull", 2), BEAR=shown("10000 * f.bear", 2))
# The newest settlement read, whatever the range, and the next one (the newest plus the newest interval).
Q_FUNDING_LATEST = sql("""
WITH f AS (@RATES@), n AS (SELECT * FROM f ORDER BY f.settlement_ms DESC LIMIT 2)
SELECT @AT@ AS "Settled At", @BULL@ AS "Bull Rate", @BEAR@ AS "Bear Rate",
       @NEXT@ AS "Next Settlement"
FROM (SELECT max(n.settlement_ms) AS t, max(n.settlement_ms) - min(n.settlement_ms) AS gap FROM n) x
LEFT JOIN f ON f.settlement_ms = x.t""", RATES=RATES, AT=clock("x.t", "MM-DD HH24:MI"), BULL=shown("10000 * f.bull", 2), BEAR=shown("10000 * f.bear", 2),
                         NEXT="CASE WHEN x.gap > 0 THEN %s END" % clock("x.t + x.gap", "MM-DD HH24:MI"))
# The funding records of the range (booked to the cycle whose interval holds the settlement), and the settlements a finished
# cycle still waits for (Awaited); newest first; a row that says so when there is neither.
Q_FUNDING = sql("""
SELECT * FROM (
  SELECT to_timestamp(f.settlement_ms / 1000.0) AS "Settlement", f.symbol AS "Contract", 'Booked' AS "Status", f.amount AS "Amount",
         f.cycle_id AS "Cycle", to_timestamp(f.ts_ms / 1000.0) AS "Record Time", f.type AS "Type"
  FROM trade.funding f WHERE f.live = @LIVE@ AND f.settlement_ms >= $__from AND f.settlement_ms <= $__to
  UNION ALL
  SELECT to_timestamp((a ->> 'settlement_ms')::bigint / 1000.0), CASE a ->> 'leg' WHEN 'bull' THEN '$bull' ELSE '$bear' END, 'Awaited', NULL,
         c.cycle_id, NULL, 'its record has not arrived: the cycle''s Funding and Net are Pending'
  FROM trade.cycle c CROSS JOIN LATERAL jsonb_array_elements(c.funding_awaited) a
  WHERE c.live = @LIVE@ AND jsonb_typeof(c.funding_awaited) = 'array') t
UNION ALL
SELECT NULL, NULL, NULL, NULL, NULL, NULL, 'None in the range, none awaited'
WHERE NOT EXISTS (SELECT 1 FROM trade.funding f WHERE f.live = @LIVE@ AND f.settlement_ms >= $__from AND f.settlement_ms <= $__to)
  AND NOT EXISTS (SELECT 1 FROM trade.cycle c WHERE c.live = @LIVE@ AND jsonb_typeof(c.funding_awaited) = 'array'
                  AND jsonb_array_length(c.funding_awaited) > 0)
ORDER BY 1 DESC NULLS LAST""", LIVE=LIVE)

# ------------------------------------------------------------------------------------------------------------------- Cycles
# The cycles that were open at any time in the range, newest first; no_fill cycles (neither leg filled) only with No-Fill
# Cycles = Shown. Per cycle: its lots and void lots, the notional its orders traded, its money (Fees as a cost: negative;
# Funding and Net Pending while a settlement has no record), its dust and pairing anchors.
IN_RANGE = "c.live = %s AND c.opened_ms <= $__to AND coalesce(c.closed_ms, %s) >= $__from" % (LIVE, NOW_MS)
Q_CYCLES = sql("""
SELECT c.cycle_id, to_timestamp(c.opened_ms / 1000.0) AS "Opened", to_timestamp(c.closed_ms / 1000.0) AS "Closed",
       @HELD@ AS "Held", initcap(c.direction) AS "Direction", initcap(c.status) AS "Status",
       coalesce(@REASON@, 'Open') AS "End Reason", l.lots AS "Lots", l.void AS "Void", o.notional AS "Notional",
       c.gross AS "Gross", -c.fees AS "Fees", CASE WHEN c.funding_known THEN c.funding ELSE @PENDING@ END AS "Funding",
       CASE WHEN c.funding_known THEN c.net ELSE @PENDING@ END AS "Net",
       CASE WHEN c.dust_bull <> 0 OR c.dust_bear <> 0 THEN trim_scale(c.dust_bull) || ' / ' || trim_scale(c.dust_bear) END AS "Dust",
       CASE c.pair_anchor_kind WHEN 'nav' THEN 'NAV' ELSE initcap(c.pair_anchor_kind) END || ' ' || trim_scale(round(c.pair_anchor_bull, 4)) || ' / '
         || trim_scale(round(c.pair_anchor_bear, 4)) AS "Pair Anchors"
FROM trade.cycle c
CROSS JOIN LATERAL (SELECT count(*) FILTER (WHERE x.status <> 'void') AS lots, count(*) FILTER (WHERE x.status = 'void') AS void
                    FROM trade.lot x WHERE x.live = @LIVE@ AND x.cycle_id = c.cycle_id) l
CROSS JOIN LATERAL (SELECT sum(x.cum_exec_value) AS notional FROM trade.cex_order x WHERE x.live = @LIVE@ AND x.cycle_id = c.cycle_id) o
WHERE @IN_RANGE@ AND ('$nofill' = 'Show' OR c.end_reason IS DISTINCT FROM 'no_fill')
ORDER BY c.opened_ms DESC LIMIT 500""", LIVE=LIVE, IN_RANGE=IN_RANGE, PENDING=str(PENDING), HELD=span("coalesce(c.closed_ms, %s) - c.opened_ms" % NOW_MS),
               REASON=words("c.end_reason"))
# The cycles of the range for the Cycle picker, newest first (the default); one option that says so when there is none.
Q_CYCLE_OPTIONS = sql("""
SELECT * FROM (
  SELECT @OPENED@ || ' · ' || initcap(c.direction) || ' · ' || coalesce(@REASON@, 'Open') AS __text, c.cycle_id AS __value
  FROM trade.cycle c WHERE @IN_RANGE@ ORDER BY c.opened_ms DESC LIMIT 500) x
UNION ALL SELECT 'No Cycle in the Time Range', 'none'
WHERE NOT EXISTS (SELECT 1 FROM trade.cycle c WHERE @IN_RANGE@)""", OPENED=clock("c.opened_ms"),
                      REASON=words("c.end_reason"), IN_RANGE=IN_RANGE)
Q_NOFILL_COUNT = sql("SELECT count(*)::text FROM trade.cycle c WHERE @IN_RANGE@ AND c.end_reason = 'no_fill'", IN_RANGE=IN_RANGE)

# -------------------------------------------------------------------------------------------------------------- Cycle Detail
CYCLE = "'$cycle'"
Q_CYCLE = sql("""
SELECT initcap(c.direction) AS "Direction", initcap(c.status) AS "Status", coalesce(@REASON@, 'Open') AS "End Reason",
       @OPENED@ AS "Opened", @HELD@ AS "Held", (SELECT count(*) FROM trade.lot l WHERE l.live = c.live AND l.cycle_id = c.cycle_id AND l.status <> 'void') AS "Lots",
       c.gross AS "Gross", -c.fees AS "Fees", CASE WHEN c.funding_known THEN c.funding ELSE @PENDING@ END AS "Funding",
       CASE WHEN c.funding_known THEN c.net ELSE @PENDING@ END AS "Net"
FROM trade.cycle c WHERE c.live = @LIVE@ AND c.cycle_id = @CYCLE@""", REASON=words("c.end_reason", NBSP), OPENED=clock("c.opened_ms", "MM-DD HH24:MI"),
              HELD=span("coalesce(c.closed_ms, %s) - c.opened_ms" % NOW_MS), PENDING=str(PENDING), LIVE=LIVE, CYCLE=CYCLE)


def avg(value, qty):
    return "CASE WHEN %s > 0 THEN %s / %s END" % (qty, value, qty)


Q_LOTS = sql("""
SELECT l.seq AS "Lot", initcap(l.status) AS "Status", @VOID@ AS "Void Reason", to_timestamp(l.opened_ms / 1000.0) AS "Opened",
       to_timestamp(l.closed_ms / 1000.0) AS "Closed", l.x_d AS "x", l.mu_d AS "μ", l.sigma_d AS "σ", l.fee_d AS "Fee D",
       l.entry_qty_bull AS "Bull In", @E_BULL@ AS "Bull In Price", l.entry_qty_bear AS "Bear In", @E_BEAR@ AS "Bear In Price",
       l.exit_qty_bull AS "Bull Out", @X_BULL@ AS "Bull Out Price", l.exit_qty_bear AS "Bear Out", @X_BEAR@ AS "Bear Out Price",
       l.gross AS "Gross", -l.fees AS "Fees", l.net AS "Net"
FROM trade.lot l WHERE l.live = @LIVE@ AND l.cycle_id = @CYCLE@ ORDER BY l.seq""", LIVE=LIVE, CYCLE=CYCLE, VOID=words("l.void_reason"),
             E_BULL=avg("l.entry_value_bull", "l.entry_qty_bull"), E_BEAR=avg("l.entry_value_bear", "l.entry_qty_bear"),
             X_BULL=avg("l.exit_value_bull", "l.exit_qty_bull"), X_BEAR=avg("l.exit_value_bear", "l.exit_qty_bear"))
# An order's way, local clock: submitted (registered) -> sent -> acked -> final state in hand; the venue's clock: its gateway
# took it (accepted) -> it created the order. A difference is only taken on one clock.
Q_ORDERS = sql("""
SELECT to_timestamp(o.submitted_ms / 1000.0) AS "Submitted", initcap(o.purpose) AS "Purpose", CASE o.leg WHEN 'bull' THEN '$bull' ELSE '$bear' END AS "Leg",
       initcap(o.side) || CASE WHEN o.reduce_only THEN ' · RO' ELSE '' END AS "Side", o.price AS "Limit", o.qty AS "Qty", o.cum_exec_qty AS "Filled",
       o.avg_price AS "Avg Price", o.cum_exec_value AS "Value", CASE WHEN o.cum_exec_qty > 0 AND NOT o.fee_known THEN @FEE_PENDING@ ELSE -o.fee END AS "Fee",
       initcap(replace(o.status, '_', ' ')) || CASE WHEN o.unknown THEN ' · Unknown' ELSE '' END AS "Status",
       o.sent_ms - o.submitted_ms AS "Submit → Sent", o.acked_ms - o.sent_ms AS "Sent → Ack", o.final_ms - o.sent_ms AS "Sent → Final",
       o.exchange_created_ms - o.accepted_ms AS "Accepted → Created",
       nullif(concat_ws(' · ', o.cancel_reason, o.error_code, o.error_message), '') AS "Note", o.client_order_id AS "Client Order ID", o.lot_id AS "Lot ID"
FROM trade.cex_order o WHERE o.live = @LIVE@ AND o.cycle_id = @CYCLE@ ORDER BY o.submitted_ms, o.client_order_id""",
               LIVE=LIVE, CYCLE=CYCLE, FEE_PENDING=str(FEE_PENDING))
Q_FILLS = sql("""
SELECT to_timestamp(f.exec_ms / 1000.0) AS "Exec Time", CASE o.leg WHEN 'bull' THEN '$bull' ELSE '$bear' END AS "Leg", initcap(o.purpose) AS "Purpose",
       initcap(f.side) AS "Side", f.price AS "Price", f.qty AS "Qty", f.value AS "Value", -f.fee AS "Fee", f.received_ms - f.exec_ms AS "Exec → Received",
       f.client_order_id AS "Client Order ID", f.exec_id AS "Exec ID"
FROM trade.cex_fill f JOIN trade.cex_order o ON o.live = f.live AND o.client_order_id = f.client_order_id
WHERE f.live = @LIVE@ AND o.cycle_id = @CYCLE@ ORDER BY f.exec_ms, f.exec_id""", LIVE=LIVE, CYCLE=CYCLE)
# The cycle's decisions (paper_match rows left out: one per paper order, the order table has them), each with its numbers;
# the at-line-blocked seconds are folded into one row per reason with their count and first and last time.
Q_DECISIONS = sql("""
SELECT * FROM (
  SELECT to_timestamp(d.ts_ms / 1000.0) AS "Time", initcap(d.kind) AS "Kind", initcap(d.direction) AS "Direction", d.lot_id AS "Lot",
         (d.detail ->> 'x_d')::float8 AS "x", (d.detail ->> 'mu_d')::float8 AS "μ", (d.detail ->> 'sigma_d')::float8 AS "σ",
         coalesce(d.detail ->> 'cause', d.reason) AS "Reason", 1 AS "Count",
         (SELECT string_agg(k || ' ' || v, ' · ' ORDER BY k) FROM jsonb_each_text(d.detail) e(k, v)
          WHERE k NOT IN ('x_d', 'mu_d', 'sigma_d', 'cause', 'client_order_ids', 'client_order_id', 'depth_seq_bull', 'depth_seq_bear')) AS "Detail"
  FROM trade.decision d WHERE d.live = @LIVE@ AND d.cycle_id = @CYCLE@ AND d.kind NOT IN ('paper_match', 'at_line_blocked')
  UNION ALL
  SELECT to_timestamp(min(d.ts_ms) / 1000.0), 'At Line Blocked', initcap(max(d.direction)), NULL, NULL, NULL, NULL, d.reason, count(*),
         'last ' || @LAST@
  FROM trade.decision d WHERE d.live = @LIVE@ AND d.cycle_id = @CYCLE@ AND d.kind = 'at_line_blocked' GROUP BY d.reason) t
ORDER BY 1""", LIVE=LIVE, CYCLE=CYCLE, LAST=clock("max(d.ts_ms)"))

# ---------------------------------------------------------------------------------------------------- Execution Statistics
Q_END_REASONS = sql("""
SELECT coalesce(@REASON@, 'Open') AS "End Reason", count(*) AS "Cycles", sum(c.net) FILTER (WHERE c.funding_known) AS "Net",
       count(*) FILTER (WHERE NOT c.funding_known) AS "Pending"
FROM @CLOSED@ GROUP BY c.end_reason ORDER BY count(*) DESC, 1""", CLOSED=CLOSED, REASON=words("c.end_reason"))
Q_IDLE = sql("""
SELECT coalesce(@IDLE@, 'Order Sent') AS "Reason", count(*) AS "Seconds", 100.0 * count(*) / sum(count(*)) OVER () AS "Share"
FROM trade.signal_sample s WHERE s.live = @LIVE@ AND s.ts_ms >= $__from AND s.ts_ms <= $__to
GROUP BY s.idle_reason ORDER BY count(*) DESC""", LIVE=LIVE, IDLE=words("s.idle_reason"))
Q_BLOCKED = sql("""
SELECT @REASON@ AS "Reason", initcap(d.direction) AS "Direction", count(*) AS "Seconds", to_timestamp(max(d.ts_ms) / 1000.0) AS "Last"
FROM trade.decision d WHERE d.live = @LIVE@ AND d.kind = 'at_line_blocked' AND d.ts_ms >= $__from AND d.ts_ms <= $__to
GROUP BY d.reason, d.direction ORDER BY count(*) DESC""", LIVE=LIVE, REASON=words("d.reason"))
LATENCY_STAGES = [("Submit → Sent", "local", "o.sent_ms - o.submitted_ms"), ("Sent → Ack", "local", "o.acked_ms - o.sent_ms"),
                  ("Sent → Final", "local", "o.final_ms - o.sent_ms"), ("Accepted → Created", "venue", "o.exchange_created_ms - o.accepted_ms"),
                  ("Created → Last Update", "venue", "o.exchange_updated_ms - o.exchange_created_ms")]
Q_LATENCY = sql("""
SELECT k.stage AS "Stage", k.clock AS "Clock", count(k.ms) AS "Orders", percentile_cont(0.5) WITHIN GROUP (ORDER BY k.ms) AS "Median",
       percentile_cont(0.9) WITHIN GROUP (ORDER BY k.ms) AS "P90", max(k.ms) AS "Max"
FROM trade.cex_order o
CROSS JOIN LATERAL (VALUES @STAGES@) k(i, stage, clock, ms)
WHERE o.live = @LIVE@ AND o.submitted_ms >= $__from AND o.submitted_ms <= $__to AND k.ms IS NOT NULL
GROUP BY k.i, k.stage, k.clock ORDER BY k.i""", LIVE=LIVE,
                STAGES=", ".join("(%d, '%s', '%s', %s)" % (i, s, c, q) for i, (s, c, q) in enumerate(LATENCY_STAGES)))

# ------------------------------------------------------------------------------------------------------------------ System
LAST_SAMPLE = "(SELECT max(s.ts_ms) FROM trade.signal_sample s WHERE s.run_id = r.run_id AND s.live = r.live AND s.ts_ms >= r.started_ms)"
Q_RUNS = sql("""
SELECT to_timestamp(r.started_ms / 1000.0) AS "Started", to_timestamp(r.stopped_ms / 1000.0) AS "Stopped",
       CASE WHEN r.stopped_ms IS NOT NULL THEN 'Stopped' WHEN @LAST@ > @NOW@ - @AGE@ * 1000 THEN 'Running' ELSE 'Lost' END AS "State",
       @RAN@ AS "Ran", CASE WHEN r.live THEN 'Live' ELSE 'Paper' END AS "Mode", r.host AS "Host",
       r.config -> 'strategy' ->> 'd_in' AS "d_in", r.config -> 'strategy' ->> 'd_out' AS "d_out",
       upper(r.config -> 'strategy' ->> 'mean_model') || ' ' || (r.config -> 'strategy' ->> 'mean_minutes') || ' · σ ' || (r.config -> 'strategy' ->> 'sigma_minutes')
         AS "Mean · σ (min)",
       r.config -> 'inventory' ->> 'capital' AS "Capital", r.config -> 'inventory' ->> 'leverage' AS "Leverage",
       r.config -> 'cex' ->> 'order_transport' AS "Transport", r.run_id AS "Run ID"
FROM trade.process_run r
WHERE r.live = @LIVE@ AND r.started_ms <= $__to AND coalesce(r.stopped_ms, @NOW@) >= $__from
ORDER BY r.started_ms DESC""", LIVE=LIVE, NOW=NOW_MS, AGE=str(SAMPLE_AGE_S), LAST=LAST_SAMPLE,
             RAN=span("coalesce(r.stopped_ms, %s, %s) - r.started_ms" % (LAST_SAMPLE, NOW_MS)))
Q_ANCHORS = sql("""
SELECT a.session_id AS "Session", a.kind AS "Kind", CASE a.leg WHEN 'bull' THEN '$bull' ELSE '$bear' END AS "Leg",
       to_timestamp(a.known_ms / 1000.0) AS "Known", a.value AS "Value", a.f AS "F", a.source AS "Source", a.detail::text AS "Detail"
FROM trade.anchor_fact a
WHERE a.live = @LIVE@ AND a.session_id IN (SELECT DISTINCT s.session_id FROM trade.signal_sample s
                                           WHERE s.live = @LIVE@ AND s.ts_ms >= $__from AND s.ts_ms <= $__to)
ORDER BY a.known_ms DESC, a.leg LIMIT 200""", LIVE=LIVE)


# ------------------------------------------------------------------------------------------------------------- the board
def collapsed_row(title, y, panels):
    """A row shown folded: its panels are queried only when it is opened. Their places count from the row's own."""
    r = row(title, y)
    r["collapsed"] = True
    r["panels"] = panels
    return r


MONEY = {"unit": "none", "decimals": 2, "thresholds": SIGNED}
PENDING_TEXT = code_texts({PENDING: "Pending", FEE_PENDING: "Pending"}, {"Pending": WARN})
STATE_WORDS = {"type": "value", "options": {
    "Lost": {"color": BAD, "index": 0}, "Off" + NBHY + "Hours": {"color": "text", "index": 13}, "Stopped": {"color": GREY, "index": 1}, "No Run": {"color": GREY, "index": 2},
    "Close" + NBSP + "Window": {"color": WARN, "index": 3}, "Missing": {"color": WARN, "index": 4}, "Bell": {"color": GREY, "index": 5},
    "NAV": {"color": "text", "index": 6}, "Warmup": {"color": WARN, "index": 7}, "Mismatch": {"color": BAD, "index": 8},
    "Long": {"color": BLUE, "index": 9}, "Short": {"color": ORANGE, "index": 10}, "Passed": {"color": GREY, "index": 11},
    "Starting": {"color": WARN, "index": 12}}}
UP = {"type": "regex", "options": {"pattern": "^Up.*", "result": {"color": GOOD, "index": 20}}}
ENTRY_WORDS = {"type": "value", "options": {text.replace(" ", NBSP): {"color": color, "index": i} for i, (text, color) in enumerate(IDLE_COLORS.items())}}
AGE = {"mode": "absolute", "steps": [{"color": "text", "value": None}, {"color": BAD, "value": SAMPLE_AGE_S + 0.5}]}
ANY = {"mode": "absolute", "steps": [{"color": "text", "value": None}, {"color": WARN, "value": 1}]}
DIRECTION = {"type": "value", "options": {"Long": {"color": BLUE, "index": 0}, "Short": {"color": ORANGE, "index": 1}}}
SCOPES = [("Time Range", "range"), ("Newest Run", "run"), ("Session", "session")]


def build():
    reset_ids(TRADE)
    templating = [
        query_var("mode", "Mode", "SELECT m AS __text, m AS __value FROM (VALUES ('Paper', false), ('Live', true)) v(m, live) "
                  "ORDER BY live IS DISTINCT FROM (SELECT r.live FROM trade.process_run r ORDER BY r.started_ms DESC LIMIT 1), live",
                  "Paper: the paper runs (exec.live false; no order leaves the process) · Live: the live runs · never merged · "
                  "the default is the mode of the newest run."),
        custom_var("scope", "Summary Scope", SCOPES, "range", "What the Summary row covers: the cycles closed in the time range, since "
                   "the newest run started, or in the newest sample's session."),
        query_var("cycle", "Cycle", Q_CYCLE_OPTIONS, "The cycle Cycle Detail shows: the cycles open at any time in the range, newest first "
                  "(the default) · a click on Opened in the Cycles table picks one."),
        custom_var("nofill", "No-Fill Cycles", [("Hidden", "Hide"), ("Shown", "Show")], "Hide",
                   "Cycles neither leg of which filled (end reason no_fill): hidden from the Cycles table unless Shown; the row title counts them."),
        query_var("bull", "Bull", latest_config("cex,pair,bull,ticker", "SOXL"), "", hide=2),
        query_var("bear", "Bear", latest_config("cex,pair,bear,ticker", "SOXS"), "", hide=2),
        query_var("d_in", "d_in", latest_config("strategy,d_in", "?"), "", hide=2),
        query_var("d_out", "d_out", latest_config("strategy,d_out", "?"), "", hide=2),
        query_var("nofill_n", "No-Fill Cycles in the Range", Q_NOFILL_COUNT, "", hide=2),
    ]
    for v in templating:
        if v["name"] in ("cycle", "nofill_n"):
            v["refresh"] = 2   # again on every change of the time range
        if v["name"] == "cycle":
            v["allowCustomValue"] = True
    panels, y = [], 0
    words_map = {"mappings": [NO_FIGURE, STATE_WORDS, UP]}
    money = dict(MONEY, mappings=[NO_FIGURE, PENDING_TEXT])
    count = {"unit": "locale", "decimals": 0}

    # ------------------------------------------------------------------ Status
    panels.append(row("Status · Running Session · ${mode}", y)); y += 1
    mode = stat_strip("Mode", Q_MODE, [("Mode", "", None)], 0, y, 24, 2, intro=(
        "Paper: the program trades against its own copy of the books; no order leaves the process (exec.live false) · Live: orders go to "
        "Bitget · every panel shows the picked mode only, paper and live never merged."), value_size=16)
    mode["options"].update(textMode="value", colorMode="background")
    mode["fieldConfig"]["defaults"]["mappings"] = [{"type": "value", "options": {
        "Paper Mode · No Orders Leave the Process": {"color": "#5a5953", "index": 0}, "Live Mode · Orders Go to Bitget": {"color": "#8a1f1f", "index": 1}}}]
    panels.append(mode); y += 2
    process = stat_strip("Process · Newest Sample", Q_STATUS, [
        ("Process", "Up and for how long since the run started (green) · Starting: a new run not sampling yet · Stopped: the run stopped in order "
                    "· Lost (red): no sample for more than %d s and no stop recorded (a crash, or the process hung) · No Run." % SAMPLE_AGE_S, words_map),
        ("Sample Age", "Seconds since the newest per-second sample (signal_sample); red past %d s." % SAMPLE_AGE_S,
         {"unit": "suffix: s", "decimals": 0, "thresholds": AGE}),
        ("Phase", "Off-Hours: entries allowed · Regular: US regular hours, exits only · Close Window: before the close, forced exits (amber).", words_map),
        ("Anchor", "NAV: both legs on the issuer's NAV · Bell: on F × Binance's index at the close (grey, provisional) · Missing: no anchor, no trading (amber).",
         words_map),
        ("Mean · σ", "Ready: the mean and sigma have their full windows · Warmup: not yet, no entries (amber).", words_map),
        ("Close At", "The regular close that ends the session, UTC+8: the flatten deadline.", None),
        ("Close In", "Time to that close.", None),
    ], 0, y, 24, 3, intro="The newest sample of the picked mode (the last two days), whatever the time range.", columns=[
        "Process", "Sample Age", "Phase", "Anchor", "Mean · σ", "Close At", "Close In"], value_size=18)
    panels.append(process); y += 3
    panels.append(stat_strip("Trading · Newest Sample · USDT", None, [
        ("Entry", "Order Sent: an order went out that second · else why none did, the program's idle reason in words: No Signal, Spacing, "
                  "Regular Hours… · amber: a book, F, orders or quota hold entries back · red: a person is needed (position mismatch, store "
                  "error, settings, not ready, order unknown).", {"mappings": [NO_FIGURE, ENTRY_WORDS]}),
        ("Position", "Flat, or the open cycle's direction: Long (both legs bought, blue), Short (both sold, orange).", words_map),
        ("Bull Held", "Contracts held on the leg, as the program counts them.", {"unit": "none", "decimals": 2, "displayName": "${bull} Held"}),
        ("Bear Held", "", {"unit": "none", "decimals": 2, "displayName": "${bear} Held"}),
        ("Unrealised", "USDT, the open cycle's lots at the newest second's mids against their entry cost, before exit fees (Positions Now has "
                       "it per leg); – when flat.", dict(MONEY, mappings=[NO_FIGURE])),
        ("Session Net", "USDT, the net of the cycles of the newest sample's session that closed, Pending ones left out.", dict(MONEY, mappings=[NO_FIGURE])),
        ("Mismatch", "None · Mismatch (red): the venue's positions and the ledger differ; no orders until a person checks.", words_map),
    ], 0, y, 24, 3, intro="The same newest sample: what the program would do, what it holds and what it made this session.", source=process,
        value_size=18)); y += 3
    panels.append(table("Alerts · Warnings and Errors · Time Range", Q_ALERTS, 0, y, 24, 4, description=(
        "Every warning and error a module of the program recorded (trade.event) in the time range, newest first\n\n"
        "Level: Warn (handled or retried) · Error (a person is needed) · Module: cex, strategy, inventory, exec, store · Kind and Message: "
        "what happened · Detail: its numbers (JSON) · at most 200 rows."), overrides=[
        col("Time", 136, unit="time:MM-DD HH:mm:ss"),
        col("Level", 64, custom_cellOptions={"type": "color-text"}, mappings=[{"type": "value", "options": {
            "Error": {"color": BAD, "index": 0}, "Warn": {"color": WARN, "index": 1}}}]),
        col("Module", 80), col("Kind", 160), col("Message", custom_minWidth=240, custom_wrapText=True), col("Detail", custom_minWidth=160)])); y += 4

    # ------------------------------------------------------------------ Live Execution
    panels.append(row("Live Execution · D · d_in ${d_in} σ · d_out ${d_out} σ (Newest Run)", y)); y += 1
    action = code_texts(ACTIONS, ACTION_COLORS)
    no_value = code_texts({**NO_VALUE, ZERO: "0.00"}, {"0.00": TEXT})
    idle = code_texts(IDLE_CODES, IDLE_COLORS)
    position = code_texts(POSITION_CODES, {"Long": BLUE, "Short": ORANGE, "Flat": TEXT})
    qty_text = code_texts({ZERO: "0"}, {"0": TEXT})
    price_text = code_texts({NO_FILL: "No Fill"})
    fee_text = code_texts({FEE_PENDING: "Pending", ZERO: "0"}, {"Pending": WARN, "0": TEXT})
    net_text = code_texts({PENDING: "Pending", OPEN: "Open", ZERO: "0"}, {"Pending": WARN, "Open": TEXT, "0": TEXT})

    def dots(name, color, size, legend=True):
        return named(name, {"color": {"mode": "fixed", "fixedColor": color}, "custom.drawStyle": "points", "custom.pointSize": size,
                            "custom.hideFrom": {"legend": not legend, "tooltip": True, "viz": False}})

    def twin(name, color, shown_as):
        return tooltip_only(name, color, no_value, {"decimals": 2}, shown=shown_as)

    def line(name):
        _, color, dash = LINES[name]
        return series(name, color, width=1.5, dash=dash, tooltip=False)
    marker_tooltip = [tooltip_only("Action", TEXT, action), tooltip_only("Decided", TEXT, props={"unit": "time:HH:mm:ss.SSS"}),
                      tooltip_only("Bull Filled", BLUE, qty_text, {"unit": "none", "decimals": 2}, shown="${bull} Filled ct"),
                      tooltip_only("Bull Price", BLUE, price_text, {"unit": "none", "decimals": 4}, shown="${bull} Avg Price"),
                      tooltip_only("Bear Filled", ORANGE, qty_text, {"unit": "none", "decimals": 2}, shown="${bear} Filled ct"),
                      tooltip_only("Bear Price", ORANGE, price_text, {"unit": "none", "decimals": 4}, shown="${bear} Avg Price"),
                      tooltip_only("Notional", TEXT, qty_text, {"unit": "locale", "decimals": 2}, shown="Notional USDT"),
                      tooltip_only("Fees", TEXT, fee_text, {"unit": "none", "decimals": 4}, shown="Fees USDT"),
                      tooltip_only("Cycle Net", TEXT, net_text, {"unit": "none", "decimals": 2}, shown="Cycle Net USDT"),
                      tooltip_only("At", TEXT, code_texts({ZERO: "0.00", NO_SAMPLE: "No Sample"}, {"0.00": TEXT}), {"decimals": 2}, shown="At D")]
    marker_names = list(ACTIONS.values())
    marker_dots = [dots(n, c, s, legend) for n, c, s, legend in (
        ("Long Entry", BLUE, 13, True), ("Short Entry", ORANGE, 13, True), ("Long Exit", BLUE + "b3", 9, False), ("Short Exit", ORANGE + "b3", 9, False),
        ("Forced Exit", WARN, 10, False), ("Trim", GREY, 7, False), ("Complete", GREY, 7, False), ("Void Entry", GREY, 11, False))]
    state_tooltip = [tooltip_only("Entry", TEXT, idle), tooltip_only("Position", TEXT, position)]
    lines = [series("Spread", PURPLE, width=1.5, tooltip=False), series("Mean", TEXT, width=1, tooltip=False),
             series("Buy Both", BLUE + "66", width=1, tooltip=False), series("Sell Both", ORANGE + "66", width=1, tooltip=False)] + [
             line(n) for n in LINES] + [
             twin("Spread · Tooltip", PURPLE, "Spread​"), twin("Mean · Tooltip", TEXT, "Mean​"), twin("Sigma · Tooltip", LIGHT_GREY, "σ"),
             twin("Buy Both · Tooltip", BLUE, "Buy Both​"), twin("Sell Both · Tooltip", ORANGE, "Sell Both​")] + state_tooltip + marker_dots + marker_tooltip
    marker_info = ["Action", "Decided", "Bull Filled", "Bull Price", "Bear Filled", "Bear Price", "Notional", "Fees", "Cycle Net"]
    markers_about = (
        "dots, at the instant of the decision: Long Entry (blue, large), Short Entry (orange, large), and not in the legend: Long Exit and "
        "Short Exit (the cycle's colour, smaller), Forced Exit (amber), Trim and Complete (grey, small), Void Entry (grey: an entry that "
        "filled one leg or none) · shaded: each cycle from its entry to its exit, blue Long, orange Short (its marker under the chart: "
        "times, end reason, lots, net) · purple marks under the chart: a change of the anchors in use, with its reason and both legs' "
        "anchors before → after in USD and USDT (owner item 62) · tooltip at a dot's instant: the action, both legs' filled contracts and "
        "average prices, the notional and fees of its orders (fees as a cost), its cycle's net (Open, Pending)")
    spread = timeseries("Spread · D", Q_SPREAD, 0, y, 24, 13, lines, decimals=1, calcs=("lastNotNull", "min", "max"), description=(
        "Spread = 10⁴ × (F × (P_bull ÷ A_bull + P_bear ÷ A_bear) − 2) on both legs' mids · 1 D = 0.5 bps of the two legs' total notional · "
        "Buy Both, Sell Both = Mean + x, the spread at the prices buying, selling both legs fills at (each leg's pricing level) · a Long "
        "enters at Buy Both ≤ Mean − d_in σ and exits at Sell Both ≥ Mean − d_out σ · a Short enters at Sell Both ≥ Mean + d_in σ and "
        "exits at Buy Both ≤ Mean + d_out σ\n\n"
        "The program's own per-second samples (signal_sample) of the picked mode · dashed: entry lines (blue Long, orange Short), dotted: exit "
        "lines (orange: a Long's, reached by Sell Both; blue: a Short's, reached by Buy Both), each sample on the d_in, d_out of the run that "
        "wrote it · " + markers_about + " · points: every second up to 25 min, then 5 s (to about 2 h), 10 s (4 h), 30 s (12 h), whole minutes; Buy Both is a "
        "bucket's lowest, Sell Both its highest, the rest its last second · a gap: no sample (process down), or a new session or run."))
    spread["options"]["tooltip"]["hideZeros"] = True
    spread["fieldConfig"]["defaults"]["noValue"] = "No sample of this mode in the time range"
    tips = [n for n, _ in TOOLTIP_COLUMNS[:5]] + [n for n, _ in STATE_COLUMNS]
    # The legend lists the fields in this order and Grafana draws them in it: Spread and Mean first, under the faint executable
    # spreads and the program's lines; the dots last, over everything.
    spread_order = ["time", "Spread", "Mean", "Buy Both", "Sell Both"] + list(LINES) + tips + marker_names + marker_info + ["At"]
    spread["transformations"] = [keep_fields(spread_order), organize(spread_order)]
    panels.append(spread); y += 13
    dev_lines = [series("Deviation", PURPLE + "80", width=1, tooltip=False), series("Buy Both x", BLUE, width=1.5, tooltip=False),
                 series("Sell Both x", ORANGE, width=1.5, tooltip=False)] + [line(n) for n in LINES] + [
                 twin("Deviation · Tooltip", PURPLE, "Deviation​"), twin("Buy Both x · Tooltip", BLUE, "Buy Both x​"),
                 twin("Sell Both x · Tooltip", ORANGE, "Sell Both x​"), twin("d_in σ · Tooltip", LIGHT_GREY, "d_in σ")
                 ] + state_tooltip + marker_dots + marker_tooltip
    dev_tips = [n for n, _ in TOOLTIP_COLUMNS[5:]] + [n for n, _ in STATE_COLUMNS]
    dev_order = (["time", "Deviation", "Buy Both x", "Sell Both x"] + [n + " · x" for n in LINES] + dev_tips + [n + " · Deviation" for n in marker_names] +
                 marker_info + ["At · Deviation"])
    deviation = timeseries("Executable Deviation · D", None, 0, y, 24, 11, dev_lines, decimals=1, zero_line=True, source=spread, description=(
        "x = executable spread − Mean · Buy Both x: buying both legs at their pricing levels' asks (a Long's entry, a Short's exit) · Sell "
        "Both x: selling both at the bids (a Short's entry, a Long's exit) · a Long enters at Buy Both x ≤ −d_in σ, exits at Sell Both x ≥ "
        "−d_out σ · a Short enters at Sell Both x ≥ +d_in σ, exits at Buy Both x ≤ +d_out σ\n\n"
        "The Spread chart with its Mean at zero: the lines the program compares, with signs · blue dashed −d_in σ (a Long needs blue at or "
        "under it), orange dashed +d_in σ (a Short needs orange at or over it), dotted the exit lines · an entry also needs the lines' "
        "distance to cover the round-trip fee, the spacing and the quota (the tooltip's Entry gives that second's answer) · purple, faint: "
        "the deviation on the mids · dots, shading and marks as on Spread · D, each dot at its x."))
    deviation["options"]["tooltip"]["hideZeros"] = True
    deviation["fieldConfig"]["defaults"]["noValue"] = "No sample of this mode in the time range"
    deviation["transformations"] = [keep_fields(dev_order), organize(dev_order, rename=dict(
        {n + " · Deviation": n for n in marker_names}, **{"At · Deviation": "At"}, **{n + " · x": n for n in LINES}))]
    deviation["fieldConfig"]["defaults"]["custom"]["axisCenteredZero"] = True
    panels.append(deviation); y += 11

    # ------------------------------------------------------------------ Summary
    panels.append(row("Summary · ${scope:text} · Cycles Closed · USDT", y)); y += 1
    results = stat_strip("Results · USDT", Q_SUMMARY, [
        ("Net", "Gross + Fees + Funding of the cycles closed in the scope whose funding is known (Pending ones left out of every money figure).",
         money),
        ("Gross", "Realised before fees, at the fill prices, void entries included.", money),
        ("Fees", "Trading fees paid, as a cost (negative).", money),
        ("Funding", "Funding booked to those cycles; + received.", money),
        ("Return", "Net ÷ the ledger's capital M, in %.", {"unit": "percent", "decimals": 2, "thresholds": SIGNED, "mappings": [NO_FIGURE]}),
        ("Notional", "USDT both legs' orders of every cycle closed in the scope traded, entries and exits.", {"unit": "locale", "decimals": 0}),
    ], 0, y, 13, 3, intro="The cycles closed in the scope picked (Summary Scope).", columns=["Net", "Gross", "Fees", "Funding", "Return", "Notional"],
        value_size=18)
    panels.append(results)
    panels.append(stat_strip("Counts", None, [
        ("Cycles", "Cycles closed in the scope, no-fill ones left out.", count),
        ("Win Rate", "Known cycles with a positive net.", {"unit": "percent", "decimals": 0, "mappings": [NO_FIGURE]}),
        ("Lots", "Entry batches of those cycles that filled.", count),
        ("Void", "Entry batches voided: no fill, one leg only, or a row not confirmed written (no-fill cycles' included).", count),
        ("Pending", "Cycles whose funding record has not arrived (amber): out of the money figures until it does.", dict(count, thresholds=ANY)),
    ], 13, y, 11, 3, source=results, value_size=18, title_size=12)); y += 3
    cum = timeseries("Cumulative Net · USDT", Q_CUMULATIVE, 0, y, 24, 8, [series("Net", TEXT, width=2), series("Gross", LIGHT_GREY, width=1)],
                     decimals=2, zero_line=True, calcs=("lastNotNull", "min", "max"), description=(
        "Net = Gross + Fees + Funding\n\n"
        "The running sum over the scope, a step at each cycle's close; from the scope's start (or the range's, with the sum before it carried "
        "in) to its end · Net (bold): what the cycles made after fees and funding · Gross (thin, light): before them · cycles with Pending "
        "funding count once it is known."))
    cum["fieldConfig"]["defaults"]["custom"]["lineInterpolation"] = "stepAfter"
    cum["fieldConfig"]["defaults"]["noValue"] = "No cycle with known funding closed in the scope"
    panels.append(cum); y += 8
    panels.append(panel("barchart", "PnL Composition · USDT", Q_COMPOSITION, 0, y, 10, 8, description=(
        "Net = Gross + Fees + Funding\n\nEach part's contribution in the scope: green adds to Net, red takes from it · Fees as a cost · the "
        "cycles with known funding, as the Results strip."), unit="none", decimals=2, no_value="No cycle closed in the scope",
        options={"orientation": "horizontal", "xField": "Part", "stacking": "normal", "showValue": "always", "barWidth": 0.6, "groupWidth": 0.7,
                 "legend": {"showLegend": False, "displayMode": "list", "placement": "bottom", "calcs": []}, "tooltip": {"mode": "single", "sort": "none"},
                 "text": {"valueSize": 13}, "xTickLabelSpacing": 0},
        overrides=[named("Gain", {"color": {"mode": "fixed", "fixedColor": GOOD}}), named("Loss", {"color": {"mode": "fixed", "fixedColor": BAD}})],
        custom={"fillOpacity": 85, "lineWidth": 0, "gradientMode": "none", "axisBorderShow": False, "axisCenteredZero": False}))
    panels.append(panel("barchart", "Net per Cycle · USDT", Q_NET_PER_CYCLE, 10, y, 14, 8, description=(
        "Each cycle closed in the scope, oldest first, under its opening time (UTC+8): its net, blue a Long cycle, orange a Short · "
        "Pending cycles (funding not known yet) and no-fill cycles left out."), unit="none", decimals=2,
        no_value="No cycle closed in the scope",
        options={"orientation": "vertical", "xField": "Cycle", "stacking": "normal", "showValue": "auto", "barWidth": 0.7, "groupWidth": 0.7,
                 "legend": {"showLegend": True, "displayMode": "list", "placement": "bottom", "calcs": []}, "tooltip": {"mode": "single", "sort": "none"},
                 "xTickLabelRotation": -45, "xTickLabelSpacing": 0},
        overrides=[named("Long", {"color": {"mode": "fixed", "fixedColor": BLUE}}), named("Short", {"color": {"mode": "fixed", "fixedColor": ORANGE}})],
        custom={"fillOpacity": 85, "lineWidth": 0, "gradientMode": "none", "axisBorderShow": False, "axisCenteredZero": False,
                "thresholdsStyle": {"mode": "dashed"}},
        thresholds={"mode": "absolute", "steps": [{"color": "transparent", "value": None}, {"color": GREY, "value": 0}]})); y += 8

    # ------------------------------------------------------------------ Inventory & Account
    panels.append(row("Inventory & Account", y)); y += 1
    ct = dict(custom_align="right", unit="none", decimals=2, mappings=[NO_FIGURE])
    px = dict(custom_align="right", unit="none", decimals=4, mappings=[NO_FIGURE])
    signed_col = dict(custom_cellOptions={"type": "color-text"}, color={"mode": "thresholds"}, thresholds=SIGNED, custom_align="right", unit="none",
                      decimals=2, mappings=[NO_FIGURE])
    panels.append(table("Positions Now · ct and USDT", Q_POSITIONS, 0, y, 24, 5, description=(
        "Difference = Exchange − Ledger · Unrealised at Mid = (Mid − Entry Price) × Cycle Holds, USDT, before exit fees\n\n"
        "The newest inventory sample (every 10 s) of the picked mode · Exchange: the venue's position, signed (paper: the paper venue's) · "
        "Ledger: the program's, dust included · Cycle Holds: what the open cycle's lots hold, signed · Entry Price: their average entry "
        "price · Mid: the leg's bid1/ask1 midpoint in the newest second · Dust: what cycles left too small for an order, recorded, not "
        "traded · Mark, Venue Unrealised, Liquidation: as Bitget reports them with the position, live only · Age: seconds since the sample "
        "· ct: contracts, one share of the ETF each."),
        overrides=[col("Leg", 64), col("Exchange", 88, **ct), col("Ledger", 88, **ct),
                   col("Difference", 96, custom_cellOptions={"type": "color-text"}, color={"mode": "thresholds"}, thresholds={"mode": "absolute", "steps": [
                       {"color": BAD, "value": None}, {"color": "text", "value": -0.000001}, {"color": BAD, "value": 0.000001}]}, **ct),
                   col("Cycle Holds", 100, **ct), col("Entry Price", 100, **px), col("Mid", 88, **px), col("Unrealised at Mid", 112, **signed_col),
                   col("Dust", 72, **ct), col("Mark", 88, **px), col("Venue Unrealised", 112, **signed_col), col("Liquidation", 96, **px),
                   col("Age", None, custom_align="right", unit="suffix: s", decimals=0, custom_minWidth=56)])); y += 5
    panels.append(stat_strip("Ledger · USDT", Q_LEDGER, [
        ("R", "Realised since the ledger started: finished cycles' net plus booked funding.", MONEY),
        ("Quota Total", "L × (M + min(0, R)): the notional the program may hold.", {"unit": "locale", "decimals": 0}),
        ("Quota Used", "Held at entry prices, plus open entry and completion orders at their limits, plus dust.", {"unit": "locale", "decimals": 0}),
        ("Quota Left", "From the newest per-second sample.", {"unit": "locale", "decimals": 0}),
        ("Used", "Quota Used ÷ Quota Total.", {"unit": "percent", "decimals": 1}),
        ("Capital", "M, the ledger's capital.", {"unit": "locale", "decimals": 0}),
        ("Leverage", "L, of the newest run.", {"unit": "none", "decimals": 0}),
        ("Mismatch", "Red when the venue's positions and the ledger differ.", {"mappings": [NO_FIGURE, STATE_WORDS]}),
    ], 0, y, 24, 3, intro="The newest inventory sample and ledger state of the picked mode.", value_size=18)); y += 3
    pos_lines = [series("Bull Exchange", BLUE, width=2), series("Bull Ledger", BLUE, width=1, dash=[6, 4]),
                 series("Bear Exchange", ORANGE, width=2), series("Bear Ledger", ORANGE, width=1, dash=[6, 4])]
    pos = timeseries("Positions · ${bull} and ${bear} · ct", Q_POSITION_HISTORY, 0, y, 24, 8, pos_lines, decimals=2, zero_line=True, description=(
        "Signed contracts: + long, − short · solid: the venue's position · dashed: the ledger's (dust included); apart means a mismatch\n\n"
        "Inventory samples every 10 s, the newest of each 1/1500 of the range past about 4 h; a gap: no sample, or the position not read yet."))
    for o in pos["fieldConfig"]["overrides"]:
        o["properties"].append({"id": "displayName", "value": o["matcher"]["options"].replace("Bull", "${bull}").replace("Bear", "${bear}")})
    pos["fieldConfig"]["defaults"]["custom"]["lineInterpolation"] = "stepAfter"
    panels.append(pos); y += 8
    account = ("the whole Bitget account: it also holds the sibling repository's positions, so this is not this program's alone · live only: "
               "paper reads no private endpoint · NULL when the newest reading is older than 30 s")
    for i, (title, q, lines_, decimals, about) in enumerate((
            ("Account Equity · USDT", Q_EQUITY, [series("Equity", TEXT, width=2)], 2, "Bitget usdtEquity, read every 10 s\n\n" + account + "."),
            ("Margin Ratio · Raw", Q_MARGIN, [series("Margin Ratio", TEXT, width=2)], 4,
             "Bitget mgnRatio, cross margin: maintenance margin ÷ equity, shown exactly as the venue reports it (its unit, fraction or "
             "percent, is not confirmed)\n\n" + account + " · the program takes no decision from it (PRD 6.6)."),
            ("Quota · USDT", Q_QUOTA, [series("Quota Used", TEXT, width=2), series("Quota Total", GREY, width=1, dash=[6, 4])], 0,
             "Quota Total = L × (M + min(0, R))\n\nUsed: held at entry prices, plus open entry and completion orders at their limits, plus "
             "dust · inventory samples every 10 s."))):
        p = timeseries(title, q, 8 * i, y, 8, 7, lines_, decimals=decimals, calcs=("lastNotNull",), description=about)
        p["options"]["legend"].update(placement="bottom", displayMode="list")
        p["options"]["legend"].pop("width", None)
        if i < 2:
            p["fieldConfig"]["defaults"]["noValue"] = "No account reading: live mode only"
        else:
            p["fieldConfig"]["defaults"]["custom"]["lineInterpolation"] = "stepAfter"
        panels.append(p)
    y += 7

    # ------------------------------------------------------------------ Funding
    panels.append(row("Funding · Time Range", y)); y += 1
    bps = {"unit": "none", "decimals": 2, "thresholds": SIGNED, "mappings": [NO_FIGURE]}
    panels.append(stat_strip("Newest Settlement · Bps", Q_FUNDING_LATEST, [
        ("Settled At", "The newest settlement the program read, UTC+8, whatever the range.", None),
        ("Bull Rate", "Rate × 10⁴ as Bitget publishes it, signed: + longs pay shorts.", dict(bps, displayName="${bull} Rate")),
        ("Bear Rate", "", dict(bps, displayName="${bear} Rate")),
        ("Next Settlement", "The newest settlement plus the newest interval between two (8 h so far), UTC+8.", None),
    ], 0, y, 24, 3, value_size=18)); y += 3
    rate = timeseries("Settled Funding Rate · Bps per Settlement", Q_FUNDING_RATE, 0, y, 24, 7,
                      [series("Bull Rate", BLUE, width=1.5), series("Bear Rate", ORANGE, width=1.5)], decimals=2, zero_line=True, description=(
        "Rate × 10⁴ of each settlement of the two contracts in the range (every 8 h so far), as Bitget publishes it, signed\n\n"
        "A positive rate: longs pay shorts · public data, the same in paper and live · bps of the position's value per settlement, not D."))
    for o in rate["fieldConfig"]["overrides"]:
        o["properties"] += [{"id": "custom.showPoints", "value": "always"}, {"id": "custom.pointSize", "value": 6},
                            {"id": "displayName", "value": "${bull}" if o["matcher"]["options"].startswith("Bull") else "${bear}"}]
    rate["fieldConfig"]["defaults"]["noValue"] = "No settlement in the time range (the newest is above)"
    panels.append(rate); y += 7
    panels.append(table("Funding Booked and Awaited · USDT", Q_FUNDING, 0, y, 24, 5, description=(
        "Each funding record of the two contracts settled in the range, booked to the cycle whose interval holds the settlement, and each "
        "settlement a finished cycle still waits for\n\n"
        "Amount: as the venue reports it, + received · Awaited (amber): the cycle's Funding and Net stay Pending until it arrives · "
        "Record Time: the record's own time · Type: the venue's record type."), overrides=[
        col("Settlement", 120, unit="time:MM-DD HH:mm"), col("Contract", 96),
        col("Status", 88, custom_cellOptions={"type": "color-text"}, mappings=[{"type": "value", "options": {"Awaited": {"color": WARN, "index": 0}}}]),
        col("Amount", 96, custom_align="right", unit="none", decimals=4, custom_cellOptions={"type": "color-text"}, color={"mode": "thresholds"},
            thresholds=SIGNED, mappings=[NO_FIGURE]),
        col("Cycle", 140), col("Record Time", 136, unit="time:MM-DD HH:mm:ss"), col("Type", custom_minWidth=200)])); y += 5

    # ------------------------------------------------------------------ Cycles
    panels.append(row("Cycles · Time Range · ${nofill_n} No-Fill ${nofill:text}", y)); y += 1
    link = (URL + "?orgId=1&${__url_time_range}&var-mode=${mode}&var-scope=${scope}&var-nofill=${nofill}"
            "&var-cycle=${__data.fields.cycle_id}")
    money_col = dict(custom_align="right", unit="none", decimals=2, custom_cellOptions={"type": "color-text"}, color={"mode": "thresholds"},
                     thresholds=SIGNED, mappings=[NO_FIGURE, PENDING_TEXT])
    panels.append(table("Cycles · Newest First", Q_CYCLES, 0, y, 24, 10, description=(
        "Net = Gross + Fees + Funding · a click on Opened shows the cycle in Cycle Detail\n\n"
        "Every cycle open at any time in the range, newest first, at most 500 · Held: open to close (to now while open) · Lots: entry batches "
        "that filled · Void: voided ones · Notional: USDT both legs' orders traded · Fees as a cost · Funding, Net: Pending (amber) while a "
        "settlement it held through has no record; while open, so far · Dust: contracts left on ${bull} / ${bear} too small for an order · "
        "Pair Anchors: the anchors (kind, USD per share) that size all its lots · no-fill cycles (neither leg filled) only with No-Fill "
        "Cycles = Shown."), overrides=[
        col("cycle_id", custom_hidden=True),
        col("Opened", 132, unit="time:MM-DD HH:mm:ss", links=[{"title": "Show this cycle in Cycle Detail", "url": link, "targetBlank": False}]),
        col("Closed", 132, unit="time:MM-DD HH:mm:ss"), col("Held", 84),
        col("Direction", 80, custom_cellOptions={"type": "color-text"}, mappings=[DIRECTION]), col("Status", 72), col("End Reason", 112),
        col("Lots", 56, custom_align="right"), col("Void", 56, custom_align="right"), col("Notional", 96, custom_align="right", unit="locale", decimals=0),
        col("Gross", 80, **money_col), col("Fees", 80, **money_col), col("Funding", 80, **money_col), col("Net", 80, **money_col),
        col("Dust", 96), col("Pair Anchors", custom_minWidth=160)])); y += 10

    # ------------------------------------------------------------------ Cycle Detail
    panels.append(row("Cycle Detail · ${cycle:text}", y)); y += 1
    panels.append(stat_strip("Cycle · USDT", Q_CYCLE, [
        ("Direction", "Long: both legs bought · Short: both sold.", {"mappings": [NO_FIGURE, DIRECTION]}),
        ("Status", "Open, Closed, or Takeover (a person took the positions over).", None),
        ("End Reason", "Exit Line, Close Window, Stop, Session Changed, No Fill, One Leg, Store Error; Open while open.", None),
        ("Opened", "UTC+8.", None), ("Held", "Open to close, to now while open.", None),
        ("Lots", "Entry batches that filled.", count),
        ("Gross", "", money), ("Fees", "As a cost.", money), ("Funding", "Pending while a settlement has no record.", money),
        ("Net", "Gross + Fees + Funding; so far while open.", money)], 0, y, 24, 3, intro="The cycle picked (Cycle, or a click in the Cycles table).",
        value_size=16)); y += 3

    def num(d):
        return dict(custom_align="right", unit="none", decimals=d, mappings=[NO_FIGURE])
    panels.append(table("Lots · ct, USDT and D", Q_LOTS, 0, y, 24, 6, description=(
        "Net = Gross + Fees · In / Out Price = traded value ÷ contracts\n\n"
        "Each entry batch of the cycle · x, μ, σ: the executable deviation, mean and sigma at its entry decision, D · Fee D: the round-trip fee at "
        "its planned prices, D · In: filled on entry and completion · Out: closed by trims and exits, first in first out (an exit across two "
        "lots split by quantity) · Void Reason: No Fill, One Leg, Store Error."), overrides=[
        col("Lot", 40, custom_align="right"), col("Status", 64), col("Void Reason", 88), col("Opened", 120, unit="time:MM-DD HH:mm:ss"),
        col("Closed", 120, unit="time:MM-DD HH:mm:ss"), col("x", 56, **num(2)), col("μ", 56, **num(2)), col("σ", 56, **num(2)), col("Fee D", 56, **num(2)),
        col("Bull In", 64, displayName="${bull} In", **num(2)), col("Bull In Price", 80, displayName="${bull} In Price", **num(4)),
        col("Bear In", 64, displayName="${bear} In", **num(2)), col("Bear In Price", 80, displayName="${bear} In Price", **num(4)),
        col("Bull Out", 64, displayName="${bull} Out", **num(2)), col("Bull Out Price", 80, displayName="${bull} Out Price", **num(4)),
        col("Bear Out", 64, displayName="${bear} Out", **num(2)), col("Bear Out Price", 80, displayName="${bear} Out Price", **num(4)),
        col("Gross", 64, **money_col), col("Fees", 64, **money_col), col("Net", 64, **money_col)])); y += 6
    ms = num(0)
    panels.append(table("Orders · ct, USDT and ms", Q_ORDERS, 0, y, 24, 8, description=(
        "Sent → Ack, Sent → Final: this host's clock · Accepted → Created: Bitget's clock · Fee as a cost\n\n"
        "Every order of the cycle as the program knows it, in the order it registered them · Side: Buy or Sell, RO a reduce-only order · "
        "Limit, Qty, Filled, Avg Price: the leg's price and contracts · Value: USDT filled · Fee: Pending (amber) while the venue has not "
        "reported the fee of what filled (an order without a fill needs none) · Status: Pending, Acked, Filled, Cancelled, Rejected, Not "
        "Sent, Never Arrived; · Unknown while queried · Submit → Sent: registered to handed to the venue · Note: cancel reason, error code "
        "and message · paper: latencies are the configured simulation."), overrides=[
        col("Submitted", 132, unit="time:MM-DD HH:mm:ss.SSS"), col("Purpose", 72), col("Leg", 56), col("Side", 72),
        col("Limit", 80, **num(4)), col("Qty", 64, **num(2)), col("Filled", 64, **num(2)), col("Avg Price", 80, **num(4)), col("Value", 80, **num(2)),
        col("Fee", 72, custom_align="right", unit="none", decimals=4, mappings=[NO_FIGURE, PENDING_TEXT]), col("Status", 80),
        col("Submit → Sent", 72, **ms), col("Sent → Ack", 64, **ms), col("Sent → Final", 64, **ms), col("Accepted → Created", 80, **ms),
        col("Note", 120), col("Client Order ID", 200), col("Lot ID", custom_minWidth=140)])); y += 8
    panels.append(table("Fills · ct, USDT and ms", Q_FILLS, 0, y, 24, 7, description=(
        "Exec → Received = this host's receipt − Bitget's execution time (two clocks)\n\n"
        "Each execution of the cycle's orders as the venue reported it · Fee as a cost · Value: price × contracts as reported."), overrides=[
        col("Exec Time", 132, unit="time:MM-DD HH:mm:ss.SSS"), col("Leg", 56), col("Purpose", 72), col("Side", 48), col("Price", 80, **num(4)),
        col("Qty", 64, **num(2)), col("Value", 80, **num(2)), col("Fee", 80, **num(5)), col("Exec → Received", 80, **ms), col("Client Order ID", 200),
        col("Exec ID", custom_minWidth=160)])); y += 7
    panels.append(table("Decisions · D", Q_DECISIONS, 0, y, 24, 8, description=(
        "x = executable spread − Mean at the decision, D\n\n"
        "Every decision of the loop for the cycle, oldest first: Entry, Complete, Trim, Exit, Forced (Reason: its cause) · the seconds the "
        "spread was at a line but no entry went out (At Line Blocked) folded into one row per reason, with their count and the last time · "
        "Detail: the decision's other numbers (prices, contracts, anchors, quota) as the program recorded them · paper_match rows (one per "
        "paper order) are in Orders."), overrides=[
        col("Time", 132, unit="time:MM-DD HH:mm:ss.SSS"), col("Kind", 104), col("Direction", 72, custom_cellOptions={"type": "color-text"}, mappings=[DIRECTION]),
        col("Lot", 150), col("x", 64, **num(2)), col("μ", 64, **num(2)), col("σ", 64, **num(2)), col("Reason", 120),
        col("Count", 64, custom_align="right", unit="locale", decimals=0), col("Detail", custom_minWidth=240, custom_inspect=True)])); y += 8

    # ------------------------------------------------------------------ Execution Statistics (collapsed)
    sy = y + 1
    stats = [
        table("End Reasons · ${scope:text}", Q_END_REASONS, 0, sy, 8, 7, description=(
            "Why each cycle closed in the Summary scope ended, how many, and their net (Pending cycles counted, out of Net)."), overrides=[
            col("End Reason", 130), col("Cycles", 64, custom_align="right"), col("Net", 80, **money_col), col("Pending", 72, custom_align="right")]),
        table("Idle Reasons · Share of Seconds", Q_IDLE, 8, sy, 8, 7, description=(
            "Each second's answer in the time range (signal_sample.idle_reason): Order Sent, or why no order went out, its seconds and share."),
            overrides=[col("Reason", 140), col("Seconds", 80, custom_align="right", unit="locale", decimals=0),
                       col("Share", None, unit="percent", decimals=1, custom_cellOptions={"type": "gauge", "mode": "basic", "valueDisplayMode": "text"},
                           color={"mode": "fixed", "fixedColor": LIGHT_GREY}, min=0, max=100)]),
        table("At Line, Not Entered · Seconds", Q_BLOCKED, 16, sy, 8, 7, description=(
            "Seconds in the time range the executable deviation was past an entry line but no entry went out (decision at_line_blocked), by "
            "reason and side."),
            overrides=[col("Reason", 130), col("Direction", 72, custom_cellOptions={"type": "color-text"}, mappings=[DIRECTION]),
                       col("Seconds", 72, custom_align="right", unit="locale", decimals=0), col("Last", 120, unit="time:MM-DD HH:mm:ss")]),
        table("Order Latency by Stage · ms", Q_LATENCY, 0, sy + 7, 24, 6, description=(
            "Median, 90th percentile and largest of each stage over the orders submitted in the time range · local: this host's clock; venue: "
            "Bitget's · paper: the configured simulated latency, not a measurement."), overrides=[
            col("Stage", 180), col("Clock", 72), col("Orders", 80, custom_align="right"), col("Median", 96, **num(1)), col("P90", 96, **num(1)),
            col("Max", None, **num(0))]),
    ]
    panels.append(collapsed_row("Execution Statistics", y, stats)); y += 1
    sy = y + 1
    system = [
        table("Runs · Time Range", Q_RUNS, 0, sy, 24, 7, description=(
            "Every start of the program in the picked mode that ran in the range, newest first, and its settings (process_run.config)\n\n"
            "State: Running (a sample in the last %d s), Stopped (in order), Lost (neither) · Ran: start to stop, or to its last sample · "
            "d_in, d_out: entry and exit lines, σ · Mean · σ: the mean model and its minutes, sigma's minutes." % SAMPLE_AGE_S), overrides=[
            col("Started", 132, unit="time:MM-DD HH:mm:ss"), col("Stopped", 132, unit="time:MM-DD HH:mm:ss"),
            col("State", 80, custom_cellOptions={"type": "color-text"}, mappings=[{"type": "value", "options": {
                "Running": {"color": GOOD, "index": 0}, "Lost": {"color": BAD, "index": 1}, "Stopped": {"color": GREY, "index": 2}}}]),
            col("Ran", 84), col("Mode", 64), col("Host", 140), col("d_in", 56), col("d_out", 56), col("Mean · σ (min)", 120), col("Capital", 72),
            col("Leverage", 72), col("Transport", 80), col("Run ID", custom_minWidth=200)]),
        table("Anchors Read · Sessions in the Time Range", Q_ANCHORS, 0, sy + 7, 24, 8, description=(
            "Every anchor, NAV and dividend the program read for the sessions in the range, newest first: bell_f (the F inside the bell), "
            "bell (F × Binance's index at the close), nav_seen (a NAV first seen), nav (confirmed after 1 h unchanged), dividend · Value: USD "
            "per share · its own reading, apart from Combo Spread's Anchors table (compare the two)."), overrides=[
            col("Session", 96), col("Kind", 80), col("Leg", 56), col("Known", 132, unit="time:MM-DD HH:mm:ss"), col("Value", 120, **num(6)),
            col("F", 80, **num(6)), col("Source", 80), col("Detail", custom_minWidth=200)]),
    ]
    panels.append(collapsed_row("System · Runs and Anchors", y, system)); y += 1

    spans = [spread["id"], deviation["id"]]
    annotations = [bands("Long Cycles", cycle_spans("long"), BLUE, spans), bands("Short Cycles", cycle_spans("short"), ORANGE, spans),
                   marks("Anchor Changes", Q_ANCHOR_CHANGES, PURPLE, [p["id"] for p in panels if p["type"] == "timeseries"])]
    board = {"panels": panels, "templating": {"list": templating}, "annotations": {"list": annotations},
             "links": [{"title": "Reset", "type": "link", "icon": "bolt", "targetBlank": False, "keepTime": False, "includeVars": False,
                        "tooltip": "The time range, refresh and settings back to their defaults; the mode stays.",
                        "url": URL + "?orgId=1&from=now-24h&to=now&refresh=10s&var-mode=${mode}&var-scope=range"
                               "&var-nofill=Hide"}],
             "time": {"from": "now-24h", "to": "now"}, "refresh": "10s", "timepicker": {"refresh_intervals": ["10s", "30s", "1m", "5m"]}, "tags": [],
             "description": "The ETF-pair trading program as it trades: status, the spread with every entry and exit, results, inventory, "
                            "funding, cycles and their orders, from its own records; paper and live apart."}
    return board
