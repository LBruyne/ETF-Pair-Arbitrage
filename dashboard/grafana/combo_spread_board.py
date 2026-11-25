"""Combo Spread: the two perpetual contracts of a combo on one exchange, the
anchor of each session (the bell anchor, then the issuer's NAV, adjusted from
the instant a dividend or a split takes effect), the basket spread on both
legs' mid prices, its moving mean and the entry band around it, and the
spreads at which a taker fills a basket of the chosen notional through both
legs' recorded books in each second. All figures come from etfpair.combo_range,
etfpair.combo_prices, etfpair.combo_last, etfpair.combo_sessions,
etfpair.combo_actions and etfpair.action_start (dashboard/internal/scan/schema.sql),
the one definition of the drawn points, the spread, the real spreads, the
mean, the Signal, a session's anchor and an action's instant; Data Coverage
reads etfpair.perp_coverage and Fee Levels the recorder's per-second rows.

A page computes each range once: the query of Spread · Bps also feeds
Deviation · Bps, Entry Points · Bps and Entry Points by Phase, that of the first price chart the
other price charts and Implied Return, that of Prices · Latest Second the
other Latest Second strips (Grafana's -- Dashboard -- data source). A shared
frame's columns are named without variables (the shown names hold them), and
every figure is rounded once in SQL to the decimals it is shown with
(shown()). The typed settings (Notional, Entry, Mean Scale) reach SQL only
as quoted text, parsed once per query (SETTINGS); one that is not a number
gives a reason (No Notional, No Entry, No Mean), never a query error."""
from boardkit import (BAD, BLUE, BPS, GREY, LIGHT_GREY, NO_FIGURE, ORANGE, PHASES, PURPLE, SIGNED, TEAL, TEXT, WARN, bands, code_texts, col,
                      copy_field, custom_var, group_by, keep_fields, marks, named, organize, query_var, reset_ids, reset_link, row, rows_with, series, sort_by, sql,
                      stat_strip, table, textbox_var, timeseries, tooltip_only)

# The spacing of the drawn points: the finest of 1, 5, 10, 30 seconds and whole minutes that keeps a line under about 2000
# points, about 1.6 a pixel of a full-width chart at 1680 px (3.5, owner item 63: 5000 until 3.4, which made a 6-hour page
# 1.5 MB and slow to draw): every second up to about 33 minutes, 5 s to about 2.8 h, 10 s to about 5.6 h, 30 s to about 16.7 h,
# then every minute or more. Every step reads the recorder's per-second rows at its multiples.
POINTS = 2000
STEP_MS = ("(SELECT CASE WHEN r <= 1000 THEN 1000 WHEN r <= 5000 THEN 5000 WHEN r <= 10000 THEN 10000 "
           "WHEN r <= 30000 THEN 30000 ELSE ceil(r / 60000) * 60000 END::bigint FROM (SELECT ($__to - $__from) / %d.0 AS r) x)" % POINTS)
# The notional N of the real spreads, USDT of both legs together, as typed: full-width characters read as their plain forms
# (NFKC); a number, its thousands grouped by commas (in threes) or not, spaces and underscores ignored, a decimal part, k for
# thousands or w / 万 for ten thousands, a trailing U or USDT; anything else is NULL, for which the Signal reads No Notional.
NOTIONAL = ("(SELECT CASE m[3] WHEN 'k' THEN 1000 WHEN 'w' THEN 10000 WHEN '万' THEN 10000 ELSE 1 END * (replace(m[1], ',', '') || m[2])::numeric "
            "FROM regexp_match(regexp_replace(regexp_replace(lower(normalize('$notional', NFKC)), '[[:space:]_]', '', 'g'), 'u(sdt)?$', ''), "
            "'^([0-9]{1,3}(?:,[0-9]{3})+|[0-9]+)((?:[.][0-9]+)?)(k|w|万)?$') m)")
# N as the titles show it, whatever was typed (10k, 1万, 10000U): with separators of thousands, ? when it is not a number.
N_TEXT = "SELECT coalesce(rtrim(rtrim(to_char(%s, 'FM999,999,999,999,990.999999'), '0'), '.'), '?')" % NOTIONAL


def typed(var):
    """A textbox's text as the queries read it: inside quotes (Grafana doubles a single quote in a single-value variable for an
    SQL data source, so the text stays one literal), full-width characters as their plain forms (NFKC), lower case, spaces and
    underscores taken out."""
    return "regexp_replace(lower(normalize('$%s', NFKC)), '[[:space:]_]', '', 'g')" % var


# Entry d in bps, as typed: a number without a sign, a decimal part allowed, bps after it allowed (10, 7.5, 10bps, １０);
# NULL for anything else and for 0, for which the Signal reads No Entry and Mean ± Entry and +Entry are not drawn.
ENTRY = "(SELECT CASE WHEN m[1]::numeric > 0 THEN m[1]::numeric END FROM regexp_match(%s, '^([0-9]+(?:[.][0-9]+)?)(?:bps)?$') m)" % typed("d")
# Mean Scale n, the mean's minutes, as typed: a whole number of at most six digits, min or m after it allowed (60, 60min,
# 60m); NULL for anything else and for 0, for which the board has no mean (No Mean).
MEAN_SCALE = "(SELECT CASE WHEN m[1]::integer > 0 THEN m[1]::integer END FROM regexp_match(%s, '^([0-9]{1,6})(?:min|m)?$') m)" % typed("ma_minutes")
# Entry and Mean Scale as the titles show them, whatever was typed (10bps, 30min): the value in use, ? when there is none.
# Entry as a multiple k of sigma (3.5, owner item 64): a positive number with σ or sigma after it (2σ, 0.5σ, 2sigma); the Entry
# of each point is then k × that point's sigma (the Sigma Scale's, per minute as the sigma lines), none where sigma is not
# ready. NULL for anything else, when Entry (bps) above reads the typed text.
ENTRY_K = "(SELECT CASE WHEN m[1]::numeric > 0 THEN m[1]::numeric END FROM regexp_match(%s, '^([0-9]+(?:[.][0-9]+)?)(?:σ|sigma)$') m)" % typed("d")
D_TEXT = ("SELECT coalesce(rtrim(rtrim(to_char(%s, 'FM999,999,990.999999'), '0'), '.') || ' bps', "
          "rtrim(rtrim(to_char(%s, 'FM999,990.999999'), '0'), '.') || 'σ', '?')" % (ENTRY, ENTRY_K))
MA_TEXT = "SELECT coalesce((%s)::text, '?')" % MEAN_SCALE
# Sigma Scale, sigma's minutes, as typed: as Mean Scale; NULL for anything else, for which the board has no sigma (No Sigma).
SIGMA_SCALE = "(SELECT CASE WHEN m[1]::integer > 0 THEN m[1]::integer END FROM regexp_match(%s, '^([0-9]{1,6})(?:min|m)?$') m)" % typed("sigma_minutes")
SIGMA_TEXT = "SELECT coalesce((%s)::text, '?')" % SIGMA_SCALE
# The typed settings, read once per query (one row v): N, Entry, Mean Scale and Sigma Scale; every figure of a query takes them
# from it.
SETTINGS = "(SELECT %s AS n, %s AS d, %s AS k, %s AS ma_n, %s AS sig_n) v" % (NOTIONAL, ENTRY, ENTRY_K, MEAN_SCALE, SIGMA_SCALE)
# The sigma multiples drawn around the Mean (Spread chart) and zero (Deviation chart, + only on Entry Points), owner item 61.
SIGMAS = (1, 1.5, 2, 2.5, 3)
# The end of the range, or now when the range runs into the future.
END_MS = "least($__to, (extract(epoch FROM now()) * 1000)::bigint)"
NOW_MS = "(extract(epoch FROM now()) * 1000)::bigint"
# What the Spread row draws (etfpair.combo_range, LATERAL to v): each point at the time its value was known, and a row with
# only its time where the lines break: at missing data and at the start of a session.
# A point's Entry in bps: the typed bps, or k × its sigma; and its Signal on that Entry (combo_signal, the one definition,
# called per point: combo_range's own Signal takes a single Entry), No Sigma where the Entry is k σ and sigma is not ready.
def entry_at(x):
    return "coalesce(v.d, v.k * %s.sigma_bps)" % x


def signal_at(x):
    return ("CASE WHEN v.d IS NULL AND v.k IS NOT NULL AND {x}.sigma_bps IS NULL AND {x}.s_bps IS NOT NULL AND NOT {x}.past_t AND {x}.mu_bps IS NOT NULL "
            "AND coalesce(v.n > 0, false) THEN 'No Sigma' ELSE etfpair.combo_signal({x}.s_bps, {x}.past_t, {x}.mu_bps, v.n, {x}.basis = 'mid', "
            "{x}.s_long_bps, {x}.s_short_bps, {x}.partial_long, {x}.partial_short, %s) END" % entry_at("{x}")).format(x=x)


RANGE_CALL = "etfpair.combo_range('$combo', '$venue', '$ma', v.ma_n, v.sig_n, $__from, $__to, %s, v.n, v.d) x" % STEP_MS
RANGE = "(SELECT x.*, %s AS entry, %s AS sig FROM %s) r" % (entry_at("x"), signal_at("x"), RANGE_CALL)
# What the Prices row draws (etfpair.combo_prices): trade prices only, no mean and no book.
PRICES = "etfpair.combo_prices('$combo', '$venue', $__from, $__to, %s) p" % STEP_MS
# The snapshot strips' two newest seconds, whatever the time range (etfpair.combo_last, LATERAL to v, ten minutes back at
# most): kind 'prices', the newest second at which both legs and the reference have a trade price; kind 'basis', the newest at
# which both legs have a basis price, the point combo_range would draw there.
LAST = "etfpair.combo_last('$combo', '$venue', '$ma', v.ma_n, v.sig_n, v.n, v.d, %s) x" % NOW_MS


def shown(x, decimals):
    """x as the board shows it with that many decimals, the one rounding a figure gets: Grafana's own (a float's
    Math.round(x * 10^d) / 10^d, halves up), so that the figure reads exactly as unrounded and its frame is smaller."""
    return "(floor((%s)::float8 * 1e%d::float8 + 0.5) / 1e%d::float8)" % (x, decimals, decimals)


# What a tooltip or a strip shows where a point has no value: Grafana's tooltip shows a nearby point's value in place of a
# NULL, so each point carries a code that names the reason (code_texts maps it to the text); the rows that break the lines
# stay NULL. FULL is a fill that took the whole notional. No Entry is a Signal only (Entry is not a positive number); Other
# Side is a real deviation in the Signal strip on the side of the mean its real spread cannot fill on.
NO_VALUE = {-2000000000: "No Book", -3000000000: "No Notional", -4000000000: "No Anchor", -5000000000: "Past T", -6000000000: "No Mean",
            -9000000000: "No Entry", -10000000000: "Other Side", -11000000000: "No Sigma", -12000000000: "Few Levels"}
CODE = {text: code for code, text in NO_VALUE.items()}
FULL = -8000000000


def reason(text, x, leg=None):
    """When a point of x (a row of combo_range or combo_last) has a reason: no anchor or F (no spread); past T of a leg's
    dividend without its amount; no mean (not ready, or no valid Mean Scale typed); no valid notional typed (v.n, the
    query's settings); not on both legs' books (No Book; with leg, that leg has no usable book); leg's recorded levels end
    query's settings); not on both legs' books (No Book; with leg, that leg has no usable book); no sigma (not ready, or no
    valid Sigma Scale typed); a side with fewer levels than the leg's pricing levels (Few Levels, the last resort of a real
    spread or a depth)."""
    return {"No Anchor": "{x}.s_bps IS NULL", "Past T": "{x}.past_t", "No Mean": "{x}.mu_bps IS NULL", "No Notional": "NOT coalesce(v.n > 0, false)",
            "No Book": "NOT {x}.book_%s" % leg if leg else "{x}.basis <> 'mid'", "No Sigma": "true", "Few Levels": "true"}[text].format(x=x)


def coded(x, value, decimals, before=(), after=(), leg=None, side=None):
    """value at a point of x, shown(), else the code of the first reason it has none (after, the last one standing for any
    other); before are reasons that hold even with a value (a real deviation past T is not shown); with side, the value
    shows only where side holds and is Other Side elsewhere (a real deviation on the side of the mean its real spread
    cannot fill on). NULL on a break row."""
    whens = ["WHEN %s.basis IS NULL THEN NULL" % x] + ["WHEN %s THEN %d" % (reason(t, x, leg), CODE[t]) for t in before]
    if side:
        whens += ["WHEN %s THEN %s" % (side, shown(value, decimals)), "WHEN %s IS NOT NULL THEN %d" % (value, CODE["Other Side"])]
    else:
        whens += ["WHEN %s IS NOT NULL THEN %s" % (value, shown(value, decimals))]
    whens += ["WHEN %s THEN %d" % (reason(t, x, leg), CODE[t]) for t in after[:-1]]
    return "CASE %s ELSE %d END" % (" ".join(whens), CODE[after[-1]])


# The reasons each figure can lack a value, in order (DESIGN 1.9).
SPREAD_WHY = dict(after=("No Anchor",))
REAL_WHY = dict(after=("No Anchor", "No Notional", "No Book", "Few Levels"))
MEAN_WHY = dict(after=("No Anchor", "Past T", "No Mean"))
SIGMA_WHY = dict(after=("No Anchor", "Past T", "No Sigma"))
REAL_DEV_WHY = dict(before=("No Anchor", "Past T"), after=("No Mean", "No Notional", "No Book", "Few Levels"))


def depth_why(leg):
    return dict(after=("No Book", "Few Levels"), leg=leg)


# The Signal of each point as a number, for the charts' tooltips, and its text and colour there (those of the Signal figure).
SIGNALS = dict({"None": 0, "Long": 1, "Short": 2, "Long · Partial": 3, "Short · Partial": 4}, **{t: CODE[t] for t in
                                                                                               ("No Book", "No Notional", "No Anchor", "Past T", "No Mean",
                                                                                                "No Entry", "No Sigma")})


def signal_code(x):
    return "CASE WHEN {x}.basis IS NOT NULL THEN CASE {x}.sig {whens} END END".format(
        x=x, whens=" ".join("WHEN '%s' THEN %d" % (text, code) for text, code in SIGNALS.items()))


def fill(x, side):
    """The USDT both legs' recorded levels fill of the notional on a side (combo_last's filled_*), FULL when they fill all of
    it, else the reason there is none: no anchor, no notional, not on both legs' books."""
    return ("CASE WHEN {x}.basis IS NULL THEN NULL WHEN {x}.s_bps IS NULL THEN %d WHEN NOT coalesce(v.n > 0, false) THEN %d "
            "WHEN {x}.basis <> 'mid' THEN %d WHEN NOT {x}.partial_{side} THEN %d WHEN {x}.filled_{side} IS NOT NULL THEN %s ELSE %d END"
            % (CODE["No Anchor"], CODE["No Notional"], CODE["No Book"], FULL, shown("{x}.filled_{side}", 0), CODE["No Book"])
            ).format(x=x, side=side)


# The alpha of a faded line's #rrggbbaa colour: a quarter of the opacity.
FADED = "40"
LEGS = (("Bid", "bid", "Bull", "bull"), ("Bid", "bid", "Bear", "bear"), ("Ask", "ask", "Bull", "bull"), ("Ask", "ask", "Bear", "bear"))
# Each leg's depth on a side in the tooltip: the total of its pricing levels (1 to ${bull_levels}: 2 at a 0.01 tick, 5 at
# 0.001), in contracts (ct) and in USDT. (frame column, shown name, combo_range column, leg).
DEPTHS = [("%s %s %s" % (side, leg, unit), "%s 1–${%s_levels} · ${%s} · %s" % (side, low_leg, low_leg, unit), "%s_%s_%s" % (low_side, unit.lower(), low_leg), low_leg)
          for unit in ("ct", "USDT") for side, low_side, leg, low_leg in LEGS]

# The newest seconds (combo_last, called once; p its prices second, b its basis second), in two rows. Row 1 holds the strips'
# figures: the US market phase now; the trade prices with their anchors and the reference's return; the basis second's
# spread, mean, sigma, deviation, anchor status and USD/USDT; its real deviations, each shown on the side of the mean its real
# spread can fill on (Real Long Spread below the Mean, Real Short Spread above it) and Other Side on the other, fills and
# Signal. Both rows hold the Book table, one leg each: its ticker, price tick, pricing levels and the depth of those levels on
# each side in contracts and USDT. A strip reduces each field to its last value, the table keeps the rows with a Leg.
LAST_COLUMNS = [
    ("Phase", "(SELECT phase FROM etfpair.etf_phase WHERE start_ms <= %s AND end_ms > %s)" % (NOW_MS, NOW_MS)),
    ("Bull Price", "p.p_bull"), ("Bull Anchor", shown("p.a_bull", 2)), ("Bear Price", "p.p_bear"), ("Bear Anchor", shown("p.a_bear", 2)),
    ("Ref Price", "p.p_ref"), ("Ref Return", shown("100 * (p.fx * p.p_ref / p.a_ref - 1)", 2)),
    ("Spread", coded("b", "b.s_bps", 2, **SPREAD_WHY)), ("Mean", coded("b", "b.mu_bps", 2, **MEAN_WHY)),
    ("Sigma", coded("b", "b.sigma_bps", 2, **SIGMA_WHY)),
    ("Deviation", coded("b", "abs(b.s_bps - b.mu_bps)", 2, **MEAN_WHY)), ("Anchor Status", "b.anchor_status"), ("USD/USDT", "b.fx"),
    ("Real Long Deviation", coded("b", "abs(b.s_long_bps - b.mu_bps)", 2, side="b.s_long_bps < b.mu_bps", **REAL_DEV_WHY)),
    ("Real Short Deviation", coded("b", "abs(b.s_short_bps - b.mu_bps)", 2, side="b.s_short_bps > b.mu_bps", **REAL_DEV_WHY)),
    ("Long Fill", fill("b", "long")), ("Short Fill", fill("b", "short")),
    ("Signal", "CASE WHEN b.kind IS NOT NULL THEN %s END" % signal_at("b"))]
BOOK_COLUMNS = [("Leg", "k.ticker"), ("Tick", "trim_scale(k.tick)::text"), ("Levels", "'1–' || etfpair.pricing_levels(k.tick)")] + [
    ("%s · %s" % (side, unit), "CASE g.i WHEN 1 THEN %s ELSE %s END" % (
        coded("b", "b.%s_%s_bull" % (low_side, unit.lower()), 2 if unit == "ct" else 0, **depth_why("bull")),
        coded("b", "b.%s_%s_bear" % (low_side, unit.lower()), 2 if unit == "ct" else 0, **depth_why("bear"))))
    for side, low_side in (("Bid", "bid"), ("Ask", "ask")) for unit in ("ct", "USDT")]
Q_LAST = sql("""
WITH v AS MATERIALIZED (SELECT * FROM @SETTINGS@),
     l AS MATERIALIZED (SELECT x.* FROM v CROSS JOIN LATERAL @LAST@)
SELECT @COLUMNS@
FROM v LEFT JOIN l p ON p.kind = 'prices' LEFT JOIN l b ON b.kind = 'basis'
CROSS JOIN (VALUES (1), (2)) g(i)
LEFT JOIN LATERAL (SELECT CASE g.i WHEN 1 THEN c.bull_ticker ELSE c.bear_ticker END AS ticker, CASE g.i WHEN 1 THEN m.bull_tick ELSE m.bear_tick END AS tick
                   FROM etfpair.combo c JOIN etfpair.combo_market m ON m.combo_id = c.combo_id
                   WHERE c.combo_id = '$combo' AND m.venue = '$venue') k ON true
ORDER BY g.i""", SETTINGS=SETTINGS, LAST=LAST,
             COLUMNS=",\n       ".join(['CASE WHEN g.i = 1 THEN %s END AS "%s"' % (q, name) for name, q in LAST_COLUMNS] +
                                        ['%s AS "%s"' % (q, name) for name, q in BOOK_COLUMNS]))


def phase_hours(name):
    """The hours, in UTC+8, of a phase's current or next stretch, as 17:00–22:30: a browser may break a line after a dash, even
    before a no-break space, so a word joiner follows it and a narrow figure keeps the range on one line."""
    return ("(SELECT to_char(to_timestamp(start_ms / 1000.0) AT TIME ZONE 'Etc/GMT-8', 'HH24:MI') || '–⁠' || "
            "to_char(to_timestamp(end_ms / 1000.0) AT TIME ZONE 'Etc/GMT-8', 'HH24:MI') "
            "FROM etfpair.etf_phase WHERE phase = '%s' AND end_ms > %s ORDER BY start_ms LIMIT 1) AS \"%s\"" % (name, NOW_MS, name))


# The key to the bands behind the charts; the next regular close (the
# flatten deadline) and the latest check of the calendar against Binance's
# schedule: OK through the last day Binance's schedule covers, the first date
# that differs and how many more, or not compared (the Calendar Check table
# has the detail). Their spaces are no-break spaces (NBSP): a narrow window
# keeps each figure on one line.
NBSP = "\u00a0"
Q_PHASES = "SELECT " + ",\n       ".join([phase_hours(name) for name in PHASES] + [
    "(SELECT to_char(to_timestamp(min(close_ms) / 1000.0) AT TIME ZONE 'Etc/GMT-8', 'MM-DD%sHH24:MI') FROM etfpair.etf_session "
    "WHERE close_ms > %s) AS \"Next Close\"" % (NBSP, NOW_MS),
    ("(SELECT CASE WHEN agree THEN 'OK@to@' || to_char(to_timestamp((window_to_ms - 1) / 1000.0) AT TIME ZONE 'America/New_York', 'MM-DD') "
     "WHEN window_to_ms IS NULL THEN 'Not@compared' "
     "ELSE (SELECT 'Differ@·@' || substr(min(d[1]), 6) || CASE WHEN count(*) > 1 THEN '@+' || (count(*) - 1) ELSE '' END "
     "FROM regexp_matches(detail, '(\\d{4}-\\d\\d-\\d\\d):', 'g') d) END "
     "FROM etfpair.calendar_check) AS \"Calendar\"").replace("@", NBSP)])

# How an action reads: the dividend as declared (? while Scheduled) or the
# split's ratio; with the ETF in front in the Anchors table and on a mark.
WHAT = ("CASE WHEN a.kind = 'dividend' THEN 'ex ' || coalesce(trim_scale(a.value)::text, '?') "
        "WHEN a.value >= 1 THEN 'split ' || trim_scale(a.value) || ':1' ELSE 'split 1:' || trim_scale(round(1 / a.value, 6)) END")
ACTION = "a.ticker || ' ' || " + WHAT

# The coming action of each ETF of the combo, read from etf_action ahead of
# its carrier session: the newest reading of its next action whose ex-date
# has not passed (New York); T and Past T from etfpair.action_start, as
# combo_actions has them: the instant a dividend takes effect, shown in UTC+8
# (a split's instant is each venue's first trade after its halt), and Past T
# for a date without an amount once T has passed.
Q_NEXT_ACTIONS = sql("""
SELECT l.ticker AS "ETF", to_char(a.ex_date, 'YYYY-MM-DD') AS "Ex-Date", @WHAT@ AS "Action",
       CASE WHEN a.kind = 'split' THEN 'after its halt'
            WHEN a.status <> 'None' THEN to_char(to_timestamp(s.start_ms / 1000.0) AT TIME ZONE 'Etc/GMT-8', 'MM-DD HH24:MI') END AS "Takes Effect",
       a.status || CASE WHEN s.past_t THEN ' · Past T' ELSE '' END AS "Status", a.source AS "Source"
FROM etfpair.combo c
CROSS JOIN LATERAL (VALUES (1, c.bull_ticker), (2, c.bear_ticker), (3, c.ref_ticker)) l(i, ticker)
LEFT JOIN LATERAL (
    SELECT n.* FROM (SELECT DISTINCT ON (x.ex_date, x.kind) x.* FROM etfpair.etf_action x
                     WHERE x.ticker = l.ticker AND x.ex_date >= (now() AT TIME ZONE 'America/New_York')::date ORDER BY x.ex_date, x.kind, x.known_ms DESC) n
    ORDER BY n.ex_date, n.kind LIMIT 1) a ON true
LEFT JOIN LATERAL etfpair.action_start(a.ex_date, a.status) s ON true
WHERE c.combo_id = '$combo' AND l.ticker <> ''
ORDER BY l.i""", WHAT=WHAT)

# The latest check of the calendar against Binance's schedule: when, through
# which New York day the schedule went, the outcome and its detail.
Q_CALENDAR_CHECK = sql("""
SELECT to_timestamp(checked_ms / 1000.0) AS "Checked",
       to_char(to_timestamp((window_to_ms - 1) / 1000.0) AT TIME ZONE 'America/New_York', 'MM-DD') AS "Binance Through",
       CASE WHEN agree THEN 'OK' WHEN window_to_ms IS NULL THEN 'Not compared' ELSE 'Differ' END AS "Result", detail AS "Detail"
FROM etfpair.calendar_check""")


def phase_marks(name):
    """The stretches of one phase inside the time range, as shaded spans."""
    return ("SELECT greatest(start_ms, $__from) AS time, least(end_ms, $__to) AS time_end, '%s' AS text "
            "FROM etfpair.etf_phase WHERE phase = '%s' AND end_ms > $__from AND start_ms < $__to" % (name, name))


# The Spread row's one query: every column of the Spread chart, the Deviation chart, the Entry Points chart and Entry Points by
# Phase.
#   Spread chart: the spread with its mean and band, drawn; then, for the tooltip only and in its order, each second's value or
#   the reason it has none (the twins of the drawn lines are named "· Tooltip" and shown under the line's name). The legend has
#   no Deviation since the Deviation chart below it gives it with its sign (3.3, UI review r1 S2).
#   Deviation chart: the Spread chart with its Mean at zero (OWNER.md item 22): the three deviations with their signs, past T
#   apart (an amber line of its own), and the band ±Entry, drawn; for the tooltip each second's signed value or the reason it
#   has none. Where both legs have books Real Long Deviation is at or above the Deviation and Real Short Deviation at or below.
#   Entry Points chart: the running number of points whose Signal is Long or Short (the last value is the count in the range), for
#   the legend; the three absolute deviations, past T apart, and Entry, drawn: each real deviation twice, faded at every second
#   and bright on the side of the mean its real spread can fill on (Real Long Spread below the Mean, Real Short Spread above
#   it), the bright one drawn over the faded one; a dot at each Long or Short second on its line.
#   Entry Points by Phase: the place in the table (PHASE_PLACES) of the phase of each point with a Deviation, the table showing
#   its name (a number in the frame is a fifth of the bytes of the name); 0 or 1 per kind of point; the absolute real deviation of
#   a point on the favourable side of the mean (not past T), whose largest is the phase's Best.
SPREAD_CHART = [("Spread", shown("r.s_bps", 1)), ("Mean", shown("r.mu_bps", 1)), ("Mean + Entry", shown("r.mu_bps + r.entry", 1)),
                ("Mean − Entry", shown("r.mu_bps - r.entry", 1)), ("Sigma", shown("r.sigma_bps", 2)),
                ("Spread · Tooltip", coded("r", "r.s_bps", 1, **SPREAD_WHY)), ("Real Long Spread", coded("r", "r.s_long_bps", 1, **REAL_WHY)),
                ("Real Short Spread", coded("r", "r.s_short_bps", 1, **REAL_WHY)), ("Mean · Tooltip", coded("r", "r.mu_bps", 1, **MEAN_WHY)),
                ("Sigma · Tooltip", coded("r", "r.sigma_bps", 2, **SIGMA_WHY)),
                ("Deviation · Tooltip", coded("r", "abs(r.s_bps - r.mu_bps)", 1, **MEAN_WHY)),
                ("Real Long Deviation · Tooltip", coded("r", "abs(r.s_long_bps - r.mu_bps)", 1, **REAL_DEV_WHY)),
                ("Real Short Deviation · Tooltip", coded("r", "abs(r.s_short_bps - r.mu_bps)", 1, **REAL_DEV_WHY)),
                ("Signal", signal_code("r"))] + [(name, coded("r", "r." + c, 2 if c.split("_")[1] == "ct" else 0, **depth_why(leg)))
                                                  for name, _, c, leg in DEPTHS]
DEVIATION_CHART = [("Signed Deviation", "CASE WHEN NOT r.past_t THEN %s END" % shown("r.s_bps - r.mu_bps", 1)),
                   ("Signed Real Long Deviation", "CASE WHEN NOT r.past_t THEN %s END" % shown("r.s_long_bps - r.mu_bps", 1)),
                   ("Signed Real Short Deviation", "CASE WHEN NOT r.past_t THEN %s END" % shown("r.s_short_bps - r.mu_bps", 1)),
                   ("Signed Past T", "CASE WHEN r.past_t THEN %s END" % shown("r.s_bps - r.mu_bps", 1)),
                   ("−Entry", "-trim_scale(round(r.entry, 6))"),
                   ("Signed Deviation · Tooltip", coded("r", "r.s_bps - r.mu_bps", 1, **MEAN_WHY)),
                   ("Signed Real Long Deviation · Tooltip", coded("r", "r.s_long_bps - r.mu_bps", 1, **REAL_DEV_WHY)),
                   ("Signed Real Short Deviation · Tooltip", coded("r", "r.s_short_bps - r.mu_bps", 1, **REAL_DEV_WHY))]
ENTRY_CHART = [("Long Points", "count(*) FILTER (WHERE r.sig LIKE 'Long%') OVER w"),
               ("Short Points", "count(*) FILTER (WHERE r.sig LIKE 'Short%') OVER w"),
               ("Deviation · Line", "CASE WHEN NOT r.past_t THEN %s END" % shown("abs(r.s_bps - r.mu_bps)", 1)),
               ("Real Long Deviation · Faded", "CASE WHEN NOT r.past_t THEN %s END" % shown("abs(r.s_long_bps - r.mu_bps)", 1)),
               ("Real Short Deviation · Faded", "CASE WHEN NOT r.past_t THEN %s END" % shown("abs(r.s_short_bps - r.mu_bps)", 1)),
               ("Real Long Deviation", "CASE WHEN NOT r.past_t AND r.s_long_bps < r.mu_bps THEN %s END" % shown("abs(r.s_long_bps - r.mu_bps)", 1)),
               ("Real Short Deviation", "CASE WHEN NOT r.past_t AND r.s_short_bps > r.mu_bps THEN %s END" % shown("abs(r.s_short_bps - r.mu_bps)", 1)),
               ("Past T", "CASE WHEN r.past_t THEN %s END" % shown("abs(r.s_bps - r.mu_bps)", 1)),
               ("+Entry", "trim_scale(round(r.entry, 6))"),
               ("Long Dots", "CASE WHEN r.sig LIKE 'Long%%' THEN %s END" % shown("abs(r.s_long_bps - r.mu_bps)", 1)),
               ("Short Dots", "CASE WHEN r.sig LIKE 'Short%%' THEN %s END" % shown("abs(r.s_short_bps - r.mu_bps)", 1))]
ENTRY_COLUMNS = [("Phase", "r.phase_sort")] + [
    (name, "CASE WHEN r.phase IS NOT NULL THEN (%s)::integer END" % test) for name, test in (
        ("Long Point", "r.sig LIKE 'Long%'"), ("Short Point", "r.sig LIKE 'Short%'"), ("Partial Point", "r.sig LIKE '% · Partial'"),
        ("Past T Point", "r.sig = 'Past T'"), ("No Book Point", "r.sig = 'No Book'"))] + [
    ("Best Long", "CASE WHEN r.phase IS NOT NULL AND NOT r.past_t AND r.s_long_bps < r.mu_bps THEN %s END" % shown("r.mu_bps - r.s_long_bps", 1)),
    ("Best Short", "CASE WHEN r.phase IS NOT NULL AND NOT r.past_t AND r.s_short_bps > r.mu_bps THEN %s END" % shown("r.s_short_bps - r.mu_bps", 1))]
def sigma_text(k):
    return "%gσ" % k


# The sigma lines (owner item 61): on the Spread chart Mean ± k σ, on the Deviation chart ± k σ, on Entry Points + k σ (the
# Deviation chart's columns). Made here, from the Mean and Sigma as shown, NULL where either is (3.5.1: made in the browser
# before, where Grafana's binary calculation reads a NULL operand as 0 and drew the lines at 0 through each session's warm-up,
# the gaps and the break rows, with spikes into them); one decimal, as the Mean: the lines are drawn only (neither legend nor
# tooltip), and a second decimal would add a fifth to the page's bytes.
SPREAD_SIGMAS = [("Mean %s %s" % (sign, sigma_text(k)), k) for k in SIGMAS for sign in ("+", "−")]
DEVIATION_SIGMAS = [("%s%s" % (sign, sigma_text(k)), k) for k in SIGMAS for sign in ("+", "−")]
ENTRY_SIGMAS = [("+" + sigma_text(k), k) for k in SIGMAS]


def k_sigma(k):
    return shown("%g * %s" % (k, shown("r.sigma_bps", 2)), 1)


SIGMA_COLUMNS = ([(name, shown("%s %s %g * %s" % (shown("r.mu_bps", 1), "+" if "+" in name else "-", k, shown("r.sigma_bps", 2)), 1))
                  for name, k in SPREAD_SIGMAS] +
                 [(name, ("" if name.startswith("+") else "-") + k_sigma(k)) for name, k in DEVIATION_SIGMAS])
Q_SPREAD = sql("""
WITH v AS MATERIALIZED (SELECT * FROM @SETTINGS@)
SELECT to_timestamp(r.ts_ms / 1000.0) AS time,
       @COLUMNS@
FROM v CROSS JOIN LATERAL @RANGE@
WINDOW w AS (ORDER BY r.ts_ms)
ORDER BY r.ts_ms""", SETTINGS=SETTINGS, RANGE=RANGE,
               COLUMNS=",\n       ".join('%s AS "%s"' % (q, name) for name, q in SPREAD_CHART + DEVIATION_CHART + ENTRY_CHART + ENTRY_COLUMNS + SIGMA_COLUMNS))


# The Prices row's one query: each leg's price; the session's anchor in USDT at the point's USD/USDT, the price at which the
# return is zero; the anchor itself in USD; the return since the anchor in percent (the reference's is also Implied Return's
# Reference); then each leg's return over its leverage of 3, with its own sign, and their sum, in percent.
def leg_columns(leg, name):
    a, p = "p.a_" + leg, "p.p_" + leg
    return [(name + " Price", p), (name + " Anchor (USDT)", shown("%s / p.fx" % a, 2)), (name + " Anchor (USD)", shown(a, 2)),
            (name + " Return", shown("100 * (p.fx * %s / %s - 1)" % (p, a), 2))]


PRICE_COLUMNS = leg_columns("bull", "Bull") + leg_columns("bear", "Bear") + leg_columns("ref", "Ref") + [
    ("Sum", shown("100 * (p.fx * p.p_bull / p.a_bull + p.fx * p.p_bear / p.a_bear - 2) / 3", 3)),
    ("Bull Leg", shown("100 * (p.fx * p.p_bull / p.a_bull - 1) / 3", 2)), ("Bear Leg", shown("100 * (p.fx * p.p_bear / p.a_bear - 1) / 3", 2))]
Q_PRICES = sql("""
SELECT to_timestamp(p.ts_ms / 1000.0) AS time,
       @COLUMNS@
FROM @PRICES@
ORDER BY p.ts_ms""", PRICES=PRICES, COLUMNS=",\n       ".join('%s AS "%s"' % (q, name) for name, q in PRICE_COLUMNS))

# A round trip from one side of the band to the other, in bps of the notional
# N of both legs: four taker fills. The Signal is on the real spreads of N,
# so crossing the spread and going down the levels are inside it; Half
# Spreads, both legs' half spreads in the newest second with a book of both
# (ten minutes back at most), shows what crossing the best levels costs now.
# Net at Entry on Entry as typed (ENTRY, read once), none when it is not a
# positive number.
Q_FEES = sql("""
SELECT f.label AS "Level", f.taker_bps AS "Taker", c.half_bps AS "Half Spreads",
       2 * f.taker_bps AS "Breakeven Entry", v.d - 2 * f.taker_bps AS "Net at Entry", f.source AS "Source"
FROM etfpair.fee_level f
CROSS JOIN (SELECT @ENTRY@ AS d) v
LEFT JOIN (SELECT 10000 * ((b.ask_px[1] - b.bid_px[1]) / (b.ask_px[1] + b.bid_px[1]) + (s.ask_px[1] - s.bid_px[1]) / (s.ask_px[1] + s.bid_px[1])) AS half_bps
           FROM etfpair.combo_market k
           JOIN etfpair.perp_second b ON b.venue = k.venue AND b.symbol = k.bull_symbol
           JOIN etfpair.perp_second s ON s.venue = k.venue AND s.symbol = k.bear_symbol AND s.second_ms = b.second_ms
           WHERE k.combo_id = '$combo' AND k.venue = '$venue' AND b.second_ms > @NOW@ - 600000
             AND b.bid_px[1] IS NOT NULL AND b.ask_px[1] IS NOT NULL AND s.bid_px[1] IS NOT NULL AND s.ask_px[1] IS NOT NULL
           ORDER BY b.second_ms DESC LIMIT 1) c ON true
WHERE f.venue = '$venue' ORDER BY f.sort""", NOW=NOW_MS, ENTRY=ENTRY)

# Per session the bell anchors of the three ETFs, the issuer's current NAV of
# each leg, and the anchors in use: the bells, or on a NAV basis the confirmed
# NAVs, which differ from the current figures while a correction is not yet
# stable; then the session's corporate actions on this exchange: what, when
# it takes effect (UTC+8), its status, and when its amount became known if
# that was after it took effect.
Q_ANCHORS = sql("""
SELECT to_char(cs.session_date, 'YYYY-MM-DD') AS "Session", to_timestamp(cs.close_ms / 1000.0) AS "Close Time",
       cs.bell_bull AS "${bull} Bell", cs.bell_bear AS "${bear} Bell", cs.a_ref AS "${ref} Bell", cs.nav_bull AS "${bull} NAV", cs.nav_bear AS "${bear} NAV",
       cs.a_bull AS "${bull} Anchor", cs.a_bear AS "${bear} Anchor", to_timestamp(cs.known_ms / 1000.0) AS "Known Time",
       cs.status AS "Status", ac.actions AS "Actions"
FROM etfpair.combo_sessions('$combo') cs
LEFT JOIN (SELECT a.session_date, string_agg(@ACTION@ || coalesce(' · ' || to_char(to_timestamp(CASE WHEN a.status <> 'None' THEN a.effective_ms END / 1000.0)
                                                                                  AT TIME ZONE 'Etc/GMT-8', 'MM-DD HH24:MI'), '') || ' · ' || a.shown
                                             || coalesce(' · known ' || to_char(to_timestamp(CASE WHEN a.status = 'Known' AND a.known_ms > a.effective_ms
                                                                                              THEN a.known_ms END / 1000.0) AT TIME ZONE 'Etc/GMT-8', 'MM-DD HH24:MI'), ''),
                                             '; ' ORDER BY a.ex_date, a.ticker) AS actions
           FROM etfpair.combo_actions('$combo', '$venue') a GROUP BY a.session_date) ac ON ac.session_date = cs.session_date
WHERE cs.close_ms <= extract(epoch FROM now()) * 1000
ORDER BY cs.close_ms DESC LIMIT 6""", ACTION=ACTION)

# One mark on the Spread chart at each instant at which actions take effect
# on this exchange that do not change an anchor (a dividend past T without its amount, a date only; a Known action is an
# Anchor Changes mark on every chart, 3.5), naming each of them: what it is; the step it should make
# (combo_actions: a dividend in bps of its ETF, a split's log ratio), none
# without an amount or an anchor; once it is Known the step measured and the
# issuer's next-day check of a leg's dividend; and its status. An action
# without a distribution (None) makes no step and has no mark.
UNIT = "CASE a.kind WHEN 'dividend' THEN ' bps' ELSE ' log' END"
MARK = ("@ACTION@ || coalesce(' · expected ' || round(a.expected, CASE a.kind WHEN 'dividend' THEN 1 ELSE 4 END) || @UNIT@, '') "
        "|| CASE WHEN a.status = 'Known' THEN ' · measured ' || coalesce(round(a.observed, CASE a.kind WHEN 'dividend' THEN 1 ELSE 4 END) || @UNIT@, '–') "
        "|| coalesce(' · NAV check ' || round(a.nav_bps, 1) || ' bps', '') ELSE '' END || ' · ' || a.shown").replace("@UNIT@", UNIT)
Q_MARKS = sql("""
SELECT a.effective_ms AS time, string_agg(@MARK@, '; ' ORDER BY a.ticker, a.kind) AS text
FROM etfpair.combo_actions('$combo', '$venue') a
WHERE a.status <> 'None' AND a.effective_ms > $__from AND a.effective_ms <= $__to
  AND a.status <> 'Known'
GROUP BY a.effective_ms""", MARK=MARK, ACTION=ACTION)

# Owner item 62: a mark on every chart where an anchor in use changes, with the reason and the anchors before and after, in
# USD and in USDT (A ÷ F, F the USD per USDT of that minute, as the price charts' Anchor (USDT)): both legs', and the
# reference's (its bell) where it changes too. The board draws each session on the anchor it has now, so the moments the
# drawn lines switch are: a session's start (its close), on the anchors in use (the bell, or the NAV once confirmed); a Known
# dividend or split of the combo's ETFs taking effect on this exchange (combo_anchors' cut, A − D or A ÷ r; the Corporate
# Actions words of combo_actions, whose own marks then keep only the dividends past T without their amount and the dates
# only); and the moment both legs' NAV is confirmed (combo_sessions' known_ms, within four days of the close; a NAV loaded
# from the issuers' history, confirmed when first read, says so), from which the whole session is drawn on the NAV in
# place of the bell, the spread moving by about 10⁴ × Σ (bell ÷ NAV − 1).
USD2 = "to_char(%s, 'FM999,990.00')"
# Grafana 13 shows a mark's text as sanitized HTML on one line as wide as it is: <br> keeps each part on a line of its own.
ANCHOR_PAIR = ("'<br>' || @BULL@ || ' ' || @B0@ || ' → ' || @B1@ || ' USD · ' || @U0@ || ' → ' || @U1@ || ' USDT<br>' || "
               "@BEAR@ || ' ' || @E0@ || ' → ' || @E1@ || ' USD · ' || @V0@ || ' → ' || @V1@ || ' USDT' || "
               "CASE WHEN ch.why <> 'nav' THEN '<br>' || @REF@ || ' ' || @R0@ || ' → ' || @R1@ || ' USD · ' || @W0@ || ' → ' || @W1@ || ' USDT (reference, "
               "bell only)' ELSE '' END || '<br>USDT = USD ÷ F, F = ' || coalesce(trim_scale(f.fx)::text, '–') || ' USD/USDT of that minute'")
ANCHOR_PAIR = sql(ANCHOR_PAIR, BULL="k.bull_ticker", BEAR="k.bear_ticker", REF="k.ref_ticker",
                  R0="coalesce(%s, '–')" % (USD2 % "ch.r0"), R1="coalesce(%s, '–')" % (USD2 % "ch.r1"),
                  W0="coalesce(%s, '–')" % (USD2 % "ch.r0 / f.fx"), W1="coalesce(%s, '–')" % (USD2 % "ch.r1 / f.fx"),
                  B0="coalesce(%s, '–')" % (USD2 % "ch.b0"), B1="coalesce(%s, '–')" % (USD2 % "ch.b1"),
                  U0="coalesce(%s, '–')" % (USD2 % "ch.b0 / f.fx"), U1="coalesce(%s, '–')" % (USD2 % "ch.b1 / f.fx"),
                  E0="coalesce(%s, '–')" % (USD2 % "ch.e0"), E1="coalesce(%s, '–')" % (USD2 % "ch.e1"),
                  V0="coalesce(%s, '–')" % (USD2 % "ch.e0 / f.fx"), V1="coalesce(%s, '–')" % (USD2 % "ch.e1 / f.fx"))
Q_ANCHOR_CHANGES = sql("""
WITH k AS (SELECT bull_ticker, bear_ticker, ref_ticker FROM etfpair.combo WHERE combo_id = '$combo'),
cs AS (
    -- Each session's anchors in use; loaded: its NAV came from the issuer's history (confirmed when first read, not after an
    -- hour of the same figure), so its time is the load's, not a publication's.
    SELECT c.*, c.basis = 'NAV' AND EXISTS (
               SELECT 1 FROM etfpair.etf_nav n JOIN etfpair.etf_nav_obs o ON o.ticker = n.ticker AND o.session_date = n.session_date AND o.nav = n.nav
               WHERE n.ticker IN (k.bull_ticker, k.bear_ticker) AND n.session_date = c.session_date AND n.confirmed_ms = c.known_ms
                 AND n.confirmed_ms - o.received_ms < 60000) AS loaded
    FROM etfpair.combo_sessions('$combo') c CROSS JOIN k
), s AS (
    SELECT a.from_ms, a.session_date, a.a_bull, a.a_bear, a.a_ref, lag(a.a_bull) OVER w AS pb, lag(a.a_bear) OVER w AS pe, lag(a.a_ref) OVER w AS pr,
           lag(a.session_date) OVER w AS ps
    FROM etfpair.combo_anchors('$combo', '$venue') a WINDOW w AS (ORDER BY a.from_ms)
), ch AS (
    SELECT s.from_ms AS t, CASE WHEN s.session_date IS DISTINCT FROM s.ps THEN 'session' ELSE 'action' END AS why, s.session_date,
           s.pb AS b0, s.a_bull AS b1, s.pe AS e0, s.a_bear AS e1, s.pr AS r0, s.a_ref AS r1
    FROM s WHERE s.session_date IS DISTINCT FROM s.ps OR s.a_bull IS DISTINCT FROM s.pb OR s.a_bear IS DISTINCT FROM s.pe OR s.a_ref IS DISTINCT FROM s.pr
    UNION ALL
    SELECT cs.known_ms, 'nav', cs.session_date, cs.bell_bull, cs.a_bull, cs.bell_bear, cs.a_bear, NULL, NULL FROM cs
    WHERE cs.basis = 'NAV' AND cs.known_ms > cs.close_ms AND cs.known_ms < cs.close_ms + 4 * 86400000
)
SELECT ch.t AS time,
       CASE ch.why
            WHEN 'session' THEN 'Anchor change · new session ' || to_char(ch.session_date, 'MM-DD') || ' (the close)<br>' ||
                 coalesce(CASE WHEN cs.loaded THEN 'NAV (loaded from the issuer''s history), the whole session drawn on it'
                               WHEN cs.basis = 'NAV' THEN 'NAV (confirmed ' || to_char(to_timestamp(cs.known_ms / 1000.0) AT TIME ZONE 'Etc/GMT-8', 'MM-DD HH24:MI')
                                                          || '), the whole session drawn on it'
                               WHEN cs.basis = 'Bell' THEN 'bell anchor (F × Binance''s index at the close; the NAV to follow)' END,
                          'no anchor yet (Missing)')
            WHEN 'nav' THEN 'Anchor change · NAV ' || CASE WHEN cs.loaded THEN 'of session ' || to_char(ch.session_date, 'MM-DD') || ' loaded from the '
                 || 'issuer''s history<br>(the load time, not a publication): ' ELSE 'confirmed for session ' || to_char(ch.session_date, 'MM-DD')
                 || '<br>both legs'' NAV the same for 1 h: ' END || 'the whole session redrawn on it in place of the bell, spread '
                 || to_char(10000 * (ch.b0 / ch.b1 + ch.e0 / ch.e1 - 2), 'FMSG990.0') || ' bps'
            ELSE 'Anchor change · a dividend or split takes effect (anchor − D, or ÷ r)<br>' ||
                 coalesce((SELECT string_agg(@MARK@, '<br>' ORDER BY a.ticker, a.kind) FROM etfpair.combo_actions('$combo', '$venue') a
                           WHERE a.effective_ms = ch.t AND a.status = 'Known'), 'anchor cut')
       END || @PAIR@ AS text
FROM ch
CROSS JOIN k
LEFT JOIN cs ON cs.session_date = ch.session_date
LEFT JOIN LATERAL (SELECT q.price AS fx FROM etfpair.fx_minute q WHERE q.pair = 'USDT-USD' AND q.minute_ms <= ch.t AND q.minute_ms >= ch.t - 300000
                   ORDER BY q.minute_ms DESC LIMIT 1) f ON true
WHERE ch.t > $__from AND ch.t <= $__to
ORDER BY ch.t""", MARK=MARK, ACTION=ACTION, PAIR=ANCHOR_PAIR)

# Per contract: the finished minutes of the range that have a candle close
# and those that have none; the recorder's seconds in the range that have a
# last price and those that have a book, a best bid and a best ask (a side
# that is {} is none); the fewest levels the thinner side and the most the
# deeper side of those books held (the recorded depth the real spreads take),
# – without a book (etfpair.perp_coverage: the counts of perp_second, whole
# minutes read from its minute table); and how old the newest second with a
# book is.
Q_COVERAGE = sql("""
SELECT k.symbol AS "Contract", m.minutes AS "Candle Minutes",
       @END@ / 60000 - ($__from + 59999) / 60000 - m.minutes AS "Missing Minutes",
       p.seconds AS "Seconds", p.books AS "Book Seconds",
       CASE WHEN p.least_levels = p.most_levels THEN p.least_levels::text ELSE p.least_levels || '–' || p.most_levels END AS "Levels",
       round(extract(epoch FROM now()) - (SELECT t.second_ms FROM etfpair.perp_second t WHERE t.venue = k.venue AND t.symbol = k.symbol
                                          AND t.bid_px[1] IS NOT NULL AND t.ask_px[1] IS NOT NULL ORDER BY t.second_ms DESC LIMIT 1) / 1000.0) AS "Book Age s"
FROM (SELECT m.venue, u.symbol, u.leg FROM etfpair.combo_market m, unnest(ARRAY[m.bull_symbol, m.bear_symbol, m.ref_symbol]) WITH ORDINALITY AS u(symbol, leg)
      WHERE m.combo_id = '$combo' AND m.venue = '$venue') k
CROSS JOIN LATERAL (SELECT count(*) AS minutes FROM etfpair.perp_minute m
                    WHERE m.venue = k.venue AND m.symbol = k.symbol AND m.minute_ms >= $__from AND m.minute_ms + 60000 <= @END@) m
CROSS JOIN LATERAL etfpair.perp_coverage(k.venue, k.symbol, $__from, @END@) p
ORDER BY k.leg""", END=END_MS)

# Shown with a non-breaking hyphen, so that a narrow figure does not split the name over two lines.
# The regular session's band is a light neutral; as text it is the darker grey, which reads on both themes.
PHASE_TEXT = dict(PHASES, Regular=GREY)
# The Phase figure is one of seven in its strip: the two long names are shortened and keep a non-breaking hyphen, so they fit a narrow window on one line.
PHASE_SHORT = {"Pre-Market": "Pre‑Mkt", "After-Hours": "After‑Hrs"}
PHASE = {"type": "value", "options": dict({name: {"text": PHASE_SHORT.get(name, name), "color": color, "index": i}
                                            for i, (name, color) in enumerate(PHASE_TEXT.items())}, Closed={"color": GREY, "index": len(PHASES)})}
# The phases in Entry Points by Phase's order (combo_range's phase_sort: 1 to 5), each name in its band's colour.
PHASE_PLACES = {1: "Pre-Market", 2: "Regular", 3: "After-Hours", 4: "Overnight", 5: "Closed"}
PHASE_NAMES = {"type": "value", "options": {str(place): {"text": name, "color": PHASE_TEXT.get(name, GREY), "index": place - 1}
                                            for place, name in PHASE_PLACES.items()}}
SIGNAL = {"type": "value", "options": {"Long": {"color": BLUE, "index": 0}, "Short": {"color": ORANGE, "index": 1}, "None": {"color": "text", "index": 2},
                                       "No Anchor": {"color": GREY, "index": 3}, "No Mean": {"color": GREY, "index": 4}, "Past T": {"color": WARN, "index": 5},
                                       "No Book": {"color": GREY, "index": 6}, "Long · Partial": {"color": BLUE, "index": 7},
                                       "Short · Partial": {"color": ORANGE, "index": 8}, "No Notional": {"color": WARN, "index": 9},
                                       "No Entry": {"color": WARN, "index": 10}, "No Sigma": {"color": GREY, "index": 11}}}
STATUS = {"type": "value", "options": {"NAV": {"color": "text", "index": 0}, "Bell": {"color": GREY, "index": 1}, "Missing": {"color": WARN, "index": 2}}}
# An action that needs a look: a check that did not agree, or a dividend that took effect before its amount was known.
ACTION_WARN = {"type": "regex", "options": {"pattern": ".*(Mismatch|Past T).*", "result": {"color": WARN, "index": 0}}}
# \s: the Calendar figure's spaces are no-break spaces, the Calendar Check table's plain ones (JavaScript's \s matches both).
CALENDAR = {"type": "regex", "options": {"pattern": r"^(Differ|Not\scompared).*", "result": {"color": WARN, "index": 0}}}
AGE = {"mode": "absolute", "steps": [{"color": "text", "value": None}, {"color": WARN, "value": 60}, {"color": BAD, "value": 300}]}
ANY = {"mode": "absolute", "steps": [{"color": "text", "value": None}, {"color": WARN, "value": 1}]}
# A reason's text where a strip's figure has none (the codes of NO_VALUE: Other Side grey as the reasons), Full for a fill of
# the whole notional; the settings typed wrong in amber.
WHY = code_texts(NO_VALUE, {"Past T": WARN, "No Notional": WARN, "No Entry": WARN})
FILLED = code_texts({**NO_VALUE, FULL: "Full"}, {"Full": "text", "Past T": WARN, "No Notional": WARN, "No Entry": WARN})

# The Deviation chart, the Entry Points chart and Entry Points by Phase take their columns of Spread · Bps's frame; Organize reads
# names before its own renaming. Entry Points by Phase: the points with a phase, grouped by it, counted (Long Point's count is the
# number of points), their Best taken, in the phases' fixed order (the phase's place).
# The Deviation chart: the band's fill first (two copies of ±Entry made in the browser, behind everything), then the Deviation
# and over it the two real lines (their inner edges decide a signal: UI review r1 S1), the band's edges and Past T; the tooltip's
# twins, in its order, and the Signal last.
DEVIATION_FIELDS = ["time", "Signed Deviation", "Signed Real Long Deviation", "Signed Real Short Deviation", "+Entry", "−Entry", "Signed Past T",
                    "Sigma", "Signed Deviation · Tooltip", "Signed Real Long Deviation · Tooltip", "Signed Real Short Deviation · Tooltip",
                    "Sigma · Tooltip", "Signal"]
DEVIATION_BAND = [copy_field("+Entry", "Band Top"), copy_field("−Entry", "Band Bottom")]


# The sigma lines (owner item 61) are drawn behind the data, grey and thin, whole multiples dashed and halves dotted, none in the
# legend (its Sigma row gives σ itself) nor the tooltip.
def sigma_line(name, legend=False):
    k = float(name.split(" ")[-1].lstrip("+−±").rstrip("σ"))
    whole = k == int(k)
    return series(name, GREY + ("C0" if whole else "90"), width=1, dash=[6, 4] if whole else [2, 3], legend=legend, tooltip=False)


DEVIATION_ORDER = (DEVIATION_FIELDS[:1] + ["Band Top", "Band Bottom"] + [name for name, _ in DEVIATION_SIGMAS] + DEVIATION_FIELDS[1:7] +
                   ["Sigma"] + DEVIATION_FIELDS[8:])
DEVIATION_NAMES = {"Signed Deviation": "Deviation", "Signed Real Long Deviation": "Real Long Deviation",
                   "Signed Real Short Deviation": "Real Short Deviation", "Signed Past T": "Past T"}
# The Entry Points chart: the faded real lines come before the bright ones, which Grafana then draws over them.
ENTRY_FIELDS = ["time", "Long Points", "Short Points", "Deviation · Line", "Real Long Deviation · Faded", "Real Short Deviation · Faded",
                "Real Long Deviation", "Real Short Deviation", "Past T", "+Entry", "Long Dots", "Short Dots", "Sigma", "Deviation · Tooltip",
                "Real Long Deviation · Tooltip", "Real Short Deviation · Tooltip", "Signal"]
ENTRY_ORDER = ENTRY_FIELDS[:3] + [name for name, _ in ENTRY_SIGMAS] + ENTRY_FIELDS[3:]
SPREAD_ORDER = ["time"] + [name for name, _ in SPREAD_SIGMAS] + [name for name, _ in SPREAD_CHART]
ENTRY_SUMS = ["Long Point", "Short Point", "Partial Point", "Past T Point", "No Book Point"]
ENTRY_TABLE = [("Phase", "Phase"), ("Long Point (sum)", "Long Points"), ("Short Point (sum)", "Short Points"), ("Partial Point (sum)", "Partial Points"),
               ("Best Long (max)", "Best Long"), ("Best Short (max)", "Best Short"), ("Past T Point (sum)", "Past T Points"),
               ("No Book Point (sum)", "No Book Points"), ("Long Point (count)", "Points")]
ENTRY_TRANSFORMS = [rows_with("Phase"),
                    group_by(["Phase"], dict({f: ["sum"] for f in ENTRY_SUMS}, **{"Long Point": ["sum", "count"], "Best Long": ["max"],
                                                                                    "Best Short": ["max"]})),
                    sort_by("Phase"),
                    organize([name for name, _ in ENTRY_TABLE], rename={a: b for a, b in ENTRY_TABLE if a != b})]


def price_fields(name):
    """The columns of the Prices row's frame one price chart draws, under the names the chart shows."""
    columns = [c for c, _ in leg_columns(name.lower(), name)]
    return [keep_fields(["time"] + columns), organize(["time"] + columns, rename={c: c[len(name) + 1:] for c in columns})]


# Entry's options (3.5, owner item 64): multiples of sigma; a typed number is bps. The halves are written ½ in the picker (their
# values, the URL and the titles keep 0.5σ, 1.5σ, 2.5σ): Enter picks the first option whose text contains what is typed, so a
# typed 5, 0.5, 1.5 or 2.5 would otherwise pick a sigma option instead of bps; only 1, 2 and 3 still do. The default, 10 bps, is
# an option too: Grafana replaces a default that is not one by the first option (3.5 opened on 0.5σ; 3.5.1).
ENTRY_OPTIONS = [("10 bps", "10")] + [(t, v + "σ") for t, v in (("½σ", "0.5"), ("1σ", "1"), ("1½σ", "1.5"), ("2σ", "2"), ("2½σ", "2.5"),
                                                                ("3σ", "3"))]
# The Notional's options, fixed sizes from 100 U (item 61) to 100k: written in k from 1,000 on, so that a typed amount (Grafana
# picks the first option whose text contains what is typed, on Enter) never picks one unless it is 1 to 99.
NOTIONAL_OPTIONS = [("100", "100"), ("500", "500")] + [("%dk" % (v // 1000), str(v)) for v in (1000, 5000, 10000, 50000, 100000)]
# A leg's pricing levels (etfpair.pricing_levels of its tick), for the names of its depths.
LEVELS = "SELECT etfpair.pricing_levels(%s_tick) FROM etfpair.combo_market WHERE combo_id = '$combo' AND venue = '$venue'"


def build():
    reset_ids()
    templating = [
        query_var("combo", "Combo", "SELECT title AS __text, combo_id AS __value FROM etfpair.combo ORDER BY 1",
                  "The bull and bear ETF perps traded as one basket."),
        query_var("venue", "CEX", "SELECT initcap(venue) AS __text, venue AS __value FROM etfpair.combo_market WHERE combo_id = '$combo' ORDER BY 1",
                  "The exchange whose perps price the combo."),
        custom_var("ma", "EMA/SMA", [("EMA", "ema"), ("SMA", "sma")], "sma", "SMA: plain average · EMA: alpha = 2 ÷ (Mean Scale + 1)."),
        textbox_var("ma_minutes", "Mean Scale (min)", "60", "Minutes the mean covers, a whole number from 1 to 999,999 (60, 60min); it restarts at each "
                    "close · anything else: no mean, the Mean, the deviations and the Signal read No Mean."),
        textbox_var("sigma_minutes", "Sigma Scale (min)", "60", "Minutes sigma covers: the population standard deviation of the "
                    "minute bars' spread over that many minutes, a whole number from 1 to 999,999 (60, 60min); it restarts with the mean · "
                    "anything else: no sigma lines, Sigma reads No Sigma."),
        custom_var("d", "Entry", ENTRY_OPTIONS, "10",
                   "Long: Real Long Spread ≤ Mean − Entry · Short: Real Short Spread ≥ Mean + Entry · pick 10 bps or a multiple of sigma (½σ to 3σ: "
                   "each point's Entry is k × its sigma, the Sigma Scale's, as the sigma lines; none until sigma is ready: No Sigma) or type a "
                   "number of bps and press Enter (10, 7.5, 10bps) · Enter picks the first option whose text contains what is typed, so 1, 2 "
                   "and 3 need bps after them (2bps) · anything else: No Entry, no Entry lines.", allow_custom=True),
        custom_var("notional", "Notional (USDT)", NOTIONAL_OPTIONS, "10000",
                   "N, both legs together: the Real spreads and the Signal fill N through both legs' pricing levels (2 at a 0.01 tick, 5 "
                   "at 0.001), on their totals where they hold less (Partial) · pick one or type any amount and press Enter: 2000, "
                   "20,000, 20k, 2万, 20000U · the options are written 1k to 100k, so a typed number of four digits or more never "
                   "picks one; only 1 to 99 (contained in 100 or 500) need a U (50U) · the titles show the N in use.",
                   allow_custom=True),
        query_var("bull", "Bull", "SELECT bull_ticker FROM etfpair.combo WHERE combo_id = '$combo'", "", hide=2),
        query_var("bear", "Bear", "SELECT bear_ticker FROM etfpair.combo WHERE combo_id = '$combo'", "", hide=2),
        query_var("ref", "Reference", "SELECT ref_ticker FROM etfpair.combo WHERE combo_id = '$combo'", "", hide=2),
        query_var("n_text", "Notional in Use", N_TEXT, "", hide=2),
        query_var("d_text", "Entry in Use", D_TEXT, "", hide=2),
        query_var("ma_text", "Mean Scale in Use", MA_TEXT, "", hide=2),
        query_var("sigma_text", "Sigma Scale in Use", SIGMA_TEXT, "", hide=2),
        query_var("bull_levels", "Bull Pricing Levels", LEVELS % "bull", "", hide=2),
        query_var("bear_levels", "Bear Pricing Levels", LEVELS % "bear", "", hide=2),
    ]
    panels, y = [], 0
    bps2 = {"unit": BPS, "decimals": 2}
    price = {"unit": "none", "decimals": 3}
    usd = {"unit": "currencyUSD", "decimals": 2}
    why = {"mappings": [NO_FIGURE, WHY]}

    panels.append(row("Market Snapshot · ${combo:text} · ${venue:text}", y)); y += 1
    # The strips' one query, at the top of the page (a source in a collapsed row would run twice on load).
    prices = stat_strip("Prices · Latest Second", Q_LAST, [
        ("Phase", "US market phase now, in its band colour: Pre-Mkt, Regular, After-Hrs, Overnight · Closed: weekend or holiday.",
         {"mappings": [PHASE]}),
        ("Bull Price", "Perp last trade price before the second, USDT.", dict(price, displayName="${bull} Price")),
        ("Bull Anchor", "The session's anchor, USD: the bell (F × Binance's index of the perp at the close; Bitget's before Binance lists "
         "the perp), then the issuer's NAV of that day once stable; less a dividend, or over a split's ratio, from the instant it takes effect.",
         dict(usd, displayName="${bull} Anchor")),
        ("Bear Price", "", dict(price, displayName="${bear} Price")),
        ("Bear Anchor", "", dict(usd, displayName="${bear} Anchor")),
        ("Ref Price", "Perp of ${ref}, the unleveraged ETF of the same index.", dict(price, displayName="${ref} Price")),
        ("Ref Return", "F·Price ÷ ${ref}'s bell anchor − 1, the bell adjusted from a dividend or split of ${ref}.",
         {"unit": "percent", "decimals": 2, "thresholds": SIGNED, "displayName": "${ref} Return"}),
    ], 0, y, 24, 3, intro="The newest second at which both legs and ${ref} have a trade price, whatever the time range (ten minutes back at most).",
        columns=["Phase", "Bull Price", "Bull Anchor", "Bear Price", "Bear Anchor", "Ref Price", "Ref Return"], title_size=12)
    panels.append(prices); y += 3
    panels.append(stat_strip("Spread · Latest Second", None, [
        ("Spread", "10⁴ × (F·P_bull ÷ A_bull + F·P_bear ÷ A_bear − 2) · P: both legs' mid prices, (best bid + best ask) ÷ 2 of the recorded "
         "book, else both last trades · A: anchor · F: USD/USDT.", dict(bps2, **why)),
        ("Mean", "The same on each leg's ${ma:text} of its minute bars over the ${ma_text} min before, this session only · a bar: both "
         "legs' mids at the minute's end, else their trades, else their candle closes · No Mean: not ready, or Mean Scale is not a whole "
         "number of minutes.", dict(bps2, **why)),
        ("Sigma", "σ: the population standard deviation of the minute bars' spread over the ${sigma_text} min before, this session "
         "only, as the trading program's (PRD 4.4) · No Sigma: not ready, or Sigma Scale is not a whole number of minutes.", dict(bps2, **why)),
        ("Deviation", "|Spread − Mean|, absolute: its side is in the Signal.", dict(bps2, **why)),
        ("Anchor Status", "NAV: both legs on the issuer's NAV, the same figure for 1 h · Bell: both legs on F × the perp's index at the close "
         "(Binance's; Bitget's before Binance lists it), provisional · Missing: no bell anchor yet, or none.", {"mappings": [STATUS]}),
        ("USD/USDT", "F, Coinbase USDT-USD.", {"unit": "none", "decimals": 5}),
    ], 0, y, 24, 3, intro="The newest second at which both legs have a basis price: their books' mids, else their last trades (ten minutes "
                          "back at most).", source=prices)); y += 3
    usdt = {"unit": "locale", "decimals": 0}
    panels.append(stat_strip("Signal · Latest Second · Notional ${n_text} USDT", None, [
        ("Real Long Deviation", "|Real Long Spread − Mean| while Real Long Spread is below the Mean, the side a Long fills on · Other "
         "Side (grey): Real Long Spread at or above the Mean, where a Long cannot come (the tooltips still give |Real Long Spread − "
         "Mean| there) · Real Long Spread: the Spread on the average prices of buying both legs for N through their pricing levels' asks "
         "(open Long, close Short).", dict(bps2, **why)),
        ("Real Short Deviation", "|Real Short Spread − Mean| while Real Short Spread is above the Mean, the side a Short fills on · "
         "Other Side: at or below the Mean · Real Short Spread: selling both legs for N through their pricing levels' bids (open "
         "Short, close Long).", dict(bps2, **why)),
        ("Long Fill", "USDT of N the asks of both legs' pricing levels fill (${bull} 1–${bull_levels}, ${bear} 1–${bear_levels}), the "
         "quantities making q·A equal on both legs · Full: all of N · less where those levels hold less (Partial): the real spread is "
         "then on their totals, as Book · Latest Second shows them.", dict(usdt, mappings=[NO_FIGURE, FILLED])),
        ("Short Fill", "The same on the pricing levels' bids.", dict(usdt, mappings=[NO_FIGURE, FILLED])),
        ("Signal", "Long: Real Long Spread ≤ Mean − Entry · Short: Real Short Spread ≥ Mean + Entry · Partial: that side filled less "
         "than N (its pricing levels hold less) · None: inside · No Anchor: not ready · No Mean: not ready, or Mean Scale is not a whole number of minutes from 1 to 999,999 · No Book: "
         "not on both legs' books (trade prices, as before the recorder) · No Notional: the typed notional is not a positive number "
         "(the title shows ? or 0), amber · No Entry: Entry is not a positive number, amber · No Sigma: Entry is a multiple of sigma and "
         "sigma is not ready (or Sigma Scale is not valid) · Past T: a leg's dividend took effect "
         "without its amount, amber, no signal until the amount is known.", {"mappings": [SIGNAL]}),
    ], 0, y, 24, 3, intro="The same second, at the notional N (USDT, both legs together); the deviations are absolute, each shown on "
                          "the side of the Mean its real spread can fill on, as the bright lines of Entry Points · Bps (Other Side on the "
                          "other), and the Signal gives the side.",
        source=prices)); y += 3
    # The book of each leg in the same second, a row a leg (Q_LAST's two rows): its price tick on this CEX, its pricing levels
    # and their depth on each side, in contracts and USDT (owner item 61, point 4).
    depth_ct = dict(custom_align="right", unit="locale", decimals=2, mappings=[NO_FIGURE, WHY])
    depth_usdt = dict(custom_align="right", unit="locale", decimals=0, mappings=[NO_FIGURE, WHY])
    book_fields = [name for name, _ in BOOK_COLUMNS]
    panels.append(table("Book · Latest Second · Pricing Levels", None, 0, y, 24, 5, source=prices,
                        transformations=[keep_fields(book_fields), rows_with("Leg")], description=(
        "Depth = the total of a leg's pricing levels on a side: ct (contracts, shares of the ETF) and USDT (Σ price × size)\n\n"
        "The same second as the strips above · Tick: the leg's price tick on this CEX · Levels: its pricing levels, the first levels "
        "from the best price on the side a taker takes, as the trading program prices (PRD 3.2): 2 at a 0.01 tick (SOXL), 5 at 0.001 "
        "(Bitget SOXS), the levels as recorded, never merged into 0.01 ones · the Real spreads, the fills and the Signal take N "
        "through these levels only · the total, not the program's takeable half · No Book: no usable book of the leg in that second "
        "· Few Levels: that side holds fewer levels than its pricing levels (no pricing level)."), overrides=[
        col("Leg", 80), col("Tick", 80, custom_align="right"), col("Levels", 80, custom_align="right"),
        col("Bid · ct", **depth_ct), col("Bid · USDT", **depth_usdt), col("Ask · ct", **depth_ct), col("Ask · USDT", **depth_usdt)])); y += 5
    panels.append(stat_strip("Trading Phases · UTC+8", Q_PHASES, [
        (name, about, {"color": {"mode": "fixed", "fixedColor": color}}) for (name, color), about in zip(PHASE_TEXT.items(), (
            "04:00 – 09:30 New York.", "09:30 – 16:00 New York, 13:00 on an early close.", "Four hours from the close.",
            "20:00 – 04:00 New York before a trading day."))
    ] + [
        ("Next Close", "The next regular close, the flatten deadline: 16:00 New York, 13:00 on an early close, as the calendar has it with its overrides.",
         None),
        ("Calendar", "The calendar against Binance's schedule, checked hourly: OK to the last day the schedule covers (New York date) · "
         "Differ: the first date whose trading day or close differs, without an override, and +n more · Not compared: the schedule could not be read or "
         "compared · amber unless OK · detail and check time in Calendar Check; extended hours are not compared here.", {"mappings": [CALENDAR]}),
    ], 0, y, 24, 3, intro="Key to the bands behind every chart: each phase's current or next hours · no band while closed.", value_size=18)); y += 3
    # Six rows high: the three ETFs' rows below the header, none cut.
    panels.append(table("Next Actions · UTC+8", Q_NEXT_ACTIONS, 0, y, 14, 6, description=(
        "Each ETF's coming dividend or split, from the dates known ahead\n\n"
        "Ex-Date: New York · Action: ex D (USD per share as declared, ? until a source gives it) or split r:1 · Takes Effect: a dividend at "
        "20:00 New York of the day before the ex-date, the anchor then less D; a split at each CEX's first trade after its halt · "
        "Status: Scheduled (date only), Known, None (no distribution); Past T: it has taken effect, its amount still unknown, amber · "
        "Source: where the newest reading came from · it stays until the ex-date has passed."), overrides=[
        col("ETF", 64), col("Ex-Date", 96), col("Action", 120), col("Takes Effect", 110),
        col("Status", 150, custom_cellOptions={"type": "color-text"}, mappings=[ACTION_WARN]), col("Source", custom_minWidth=100)]))
    panels.append(table("Calendar Check · UTC+8", Q_CALENDAR_CHECK, 14, y, 10, 6, description=(
        "The latest hourly check of the calendar against Binance's schedule\n\n"
        "Checked: when · Binance Through: the last day its schedule covers, New York · Result: OK, Differ (a trading day or close differs "
        "on a date without an override), Not compared (the schedule could not be read or compared), amber unless OK · Detail: the differences "
        "(here UTC+8 against Binance) or the reason."), overrides=[
        col("Checked", 96, unit="time:MM-DD HH:mm"), col("Binance Through", 80),
        col("Result", 104, custom_cellOptions={"type": "color-text"}, mappings=[CALENDAR]),
        col("Detail", custom_minWidth=120, custom_wrapText=True)])); y += 6

    panels.append(row("Spread · ${ma:text} ${ma_text} min · σ ${sigma_text} min · Entry ${d_text} · Notional ${n_text} USDT (Both Legs)", y)); y += 1
    # The tooltips show only twins, each point's value or the reason it has none (tooltip_only), as Grafana would show a nearby
    # point's value in place of a NULL: the drawn lines and the legend's figures are kept out of them.
    no_value = code_texts(NO_VALUE)
    signal = code_texts({code: text for text, code in SIGNALS.items()}, {text: o["color"] for text, o in SIGNAL["options"].items()})

    def twin(name, color, shown_as=None, props=None):
        return tooltip_only(name, color, no_value, props, shown=shown_as)
    # Sigma: a figure for the legend (Last, Min, Max) without a line, on a hidden axis of its own (the sigma lines are columns of the query).
    sigma = named("Sigma", {"color": {"mode": "fixed", "fixedColor": LIGHT_GREY}, "custom.lineWidth": 0, "custom.axisPlacement": "hidden",
                            "decimals": 2, "custom.hideFrom": {"legend": False, "tooltip": True, "viz": False}})
    band = [sigma] + [sigma_line(name) for name, _ in SPREAD_SIGMAS] + [
            series("Spread", PURPLE, width=2, tooltip=False), series("Mean", TEXT, width=1, tooltip=False),
            series("Mean + Entry", TEAL, dash=[6, 4], fill_to="Mean − Entry", fill=8, tooltip=False),
            series("Mean − Entry", TEAL, dash=[6, 4], legend=False, tooltip=False),
            twin("Spread · Tooltip", PURPLE, "Spread​"), twin("Real Long Spread", BLUE), twin("Real Short Spread", ORANGE),
            twin("Mean · Tooltip", TEXT, "Mean​"), twin("Sigma · Tooltip", LIGHT_GREY, "Sigma​", {"decimals": 2}),
            twin("Deviation · Tooltip", GREY, "Deviation"),
            twin("Real Long Deviation · Tooltip", BLUE, "Real Long Deviation"), twin("Real Short Deviation · Tooltip", ORANGE, "Real Short Deviation"),
            tooltip_only("Signal", TEXT, signal)] + [
        twin(name, ORANGE if name.startswith("Bid") else BLUE, display,
             {"unit": "locale", "decimals": 2 if name.endswith("ct") else 0}) for name, display, _, _ in DEPTHS]
    spread = timeseries("Spread · Bps", Q_SPREAD, 0, y, 24, 13, band, transformations=[keep_fields(SPREAD_ORDER), organize(SPREAD_ORDER)], description=(
        "Spread = 10⁴ × (F·P_bull ÷ A_bull + F·P_bear ÷ A_bear − 2) · P: both legs' mid prices, else both last trades · Real Long, Real "
        "Short Spread: the same on the average prices of buying, selling both legs for the notional N through their pricing levels' "
        "asks, bids · σ: the population standard deviation of the minute bars' spread over the Sigma Scale · Long: Real Long Spread ≤ "
        "Mean − Entry · Short: Real Short Spread ≥ Mean + Entry · Entry: the bps typed, or k × σ (0.5σ … 3σ) at each point\n\n"
        "Drawn: Spread, Mean, the band Mean ± Entry (teal, dashed; not drawn when Entry is not a positive number: the Signal reads No "
        "Entry) and Mean ± 1, 1.5, 2, 2.5, 3 σ (grey: whole σ dashed, half σ dotted; none until sigma is ready); the drawn Spread "
        "crossing a line is not a signal (see Deviation · Bps and Entry Points · Bps) · legend Sigma: σ's last, lowest and highest · "
        "tooltip, in this order: Spread, Real Long Spread, Real Short Spread, Mean, Sigma, Deviation, Real Long Deviation, Real Short "
        "Deviation, Signal, each leg's bid and ask depth of its pricing levels in ct (contracts) and USDT; each row is that second's "
        "value or the reason it has none (No Anchor, Past T, No Mean, No Sigma, No Notional, No Book, Few Levels) · P: (best bid + best "
        "ask) ÷ 2 of both legs' recorded books in that second, else both legs' last trades before it (before the recorder: trades "
        "only, No Book) · Real Long Spread, Real Short Spread: what opening or closing a basket of N USDT (both legs together) as a "
        "taker gives in that second, the quantities making q·A equal on both legs, through each leg's pricing levels only (${bull} 1–"
        "${bull_levels}, ${bear} 1–${bear_levels}: 2 at a 0.01 tick, 5 at 0.001, the trading program's, PRD 3.2); where those levels "
        "hold less than N, on their totals, and the Signal reads Partial · Deviation, Real Long Deviation, Real Short Deviation: |Spread "
        "− Mean|, |Real Long Spread − Mean|, |Real Short Spread − Mean|, absolute (with their signs on Deviation · Bps) · Signal: as in "
        "Signal · Latest Second · depth: as in Book · Latest Second · A: session anchor, less a dividend or over a split's ratio from "
        "the instant it takes effect (a mark there, with the expected and measured step; the mean and σ restart, also when a leg's "
        "dividend takes effect without its amount: A stays, the spread steps, Past T until the amount is known) · F: USD/USDT · Mean: "
        "the same on each leg's mean of its minute bars (both legs' mids at the minute's end, else their trades, else their candle "
        "closes) · each session is drawn on its own · points: the recorder's per-second rows, at most about 2,000 a line: every second "
        "to about 33 min, 5 s to about 2.8 h, 10 s to about 5.6 h, 30 s to about 16.7 h, then every minute or more; a second without "
        "both legs' prices is a gap."))
    panels.append(spread); y += 13
    # The Deviation chart (OWNER.md item 22): the Spread chart with its Mean moved to zero, the zero in the middle of the axis,
    # every figure with its sign. The sigma lines ±k σ behind everything but the band (item 61). Deviation purple and 2 px as the
    # Spread, drawn first; the real deviations over it, curves at every second in their sides' colours. The band ±Entry filled as
    # the Spread chart's Mean ± Entry (on browser copies, Grafana filling a band in its upper line's colour) and each edge dashed in
    # the colour of the line it triggers: −Entry blue, where a Long needs Real Long Deviation; +Entry orange, where a Short needs
    # Real Short Deviation (UI review r1 M1). Neither the band nor Past T (amber) is in the legend, as on Entry Points (item 17);
    # the row's title has Entry.
    hidden = {"legend": True, "tooltip": True, "viz": False}
    signed = [named("Band Top", {"color": {"mode": "fixed", "fixedColor": TEAL}, "custom.lineWidth": 0, "custom.fillBelowTo": "Band Bottom",
                                 "custom.fillOpacity": 8, "custom.hideFrom": hidden}),
              named("Band Bottom", {"color": {"mode": "fixed", "fixedColor": TEAL}, "custom.lineWidth": 0, "custom.hideFrom": hidden})] + [
              sigma_line(name) for name, _ in DEVIATION_SIGMAS] + [
              series("Deviation", PURPLE, width=2, tooltip=False), series("Real Long Deviation", BLUE, width=1, tooltip=False),
              series("Real Short Deviation", ORANGE, width=1, tooltip=False),
              series("+Entry", ORANGE, width=1.5, dash=[6, 4], legend=False, tooltip=False),
              series("−Entry", BLUE, width=1.5, dash=[6, 4], legend=False, tooltip=False),
              series("Past T", WARN, width=2, legend=False, tooltip=False), sigma,
              twin("Signed Deviation · Tooltip", PURPLE, "Deviation​"), twin("Signed Real Long Deviation · Tooltip", BLUE, "Real Long Deviation​"),
              twin("Signed Real Short Deviation · Tooltip", ORANGE, "Real Short Deviation​"),
              twin("Sigma · Tooltip", LIGHT_GREY, "Sigma​", {"decimals": 2}), tooltip_only("Signal", TEXT, signal)]
    # Thirteen rows high, as the Spread chart: the same plot height, so their shapes compare (UI review r1 N2).
    deviation = timeseries("Deviation · Bps", None, 0, y, 24, 13, signed, zero_line=True, source=spread,
                           transformations=[keep_fields(DEVIATION_FIELDS + [name for name, _ in DEVIATION_SIGMAS])] + DEVIATION_BAND +
                           [organize(DEVIATION_ORDER, rename=DEVIATION_NAMES)], description=(
        "Deviation = Spread − Mean · Real Long / Short Deviation = Real Long / Short Spread − Mean · Long: Real Long Deviation ≤ −Entry "
        "· Short: Real Short Deviation ≥ +Entry · ±k σ: k times sigma, k = 1, 1.5, 2, 2.5, 3\n\n"
        "The Spread chart with its Mean at zero, with signs · purple: Deviation on the mids (trades where there is no book; the only line "
        "before the recorder) · blue: Real Long Deviation, buying both legs for N through their pricing levels' asks, never below purple "
        "· orange: Real Short Deviation, selling through the bids, never above it · a Long needs blue at or under −Entry (blue dashed), a "
        "Short orange at or over +Entry (orange dashed), Partial included; blue over +Entry or orange under −Entry is no signal · grey: "
        "±1, 1.5, 2, 2.5, 3 σ (whole σ dashed, half σ dotted), the trading program's lines are d_in·σ: where blue reaches −2σ, x ≤ "
        "−2σ · legend Min of blue, Max of orange: how far each side came toward or past its Entry (Entry Points' Best); Sigma: σ's last, "
        "lowest and highest, k σ is k times it · tooltip: each second's value or why it has none, σ, and the Signal · Past T (amber): no signal until the "
        "dividend's amount is known · no band with No Entry, no σ lines with No Sigma · absolute values and signal dots: Entry Points · Bps."))
    deviation["fieldConfig"]["defaults"]["custom"]["axisCenteredZero"] = True
    panels.append(deviation); y += 13
    # The two counts are for the legend, first so that a narrow window's legend under the plot shows them: no line, a hidden
    # axis of their own, not in the tooltip (a running count says little of a second).
    def count_of(name, color):
        return named(name, {"color": {"mode": "fixed", "fixedColor": color}, "custom.lineWidth": 0, "custom.axisPlacement": "hidden", "unit": "locale",
                            "decimals": 0, "custom.thresholdsStyle": {"mode": "off"}, "custom.hideFrom": {"legend": False, "tooltip": True, "viz": False}})

    # A dot at each Long or Short second, on its line: the lines are absolute, so they do not show the side of a signal.
    def dots(name, color):
        return named(name, {"color": {"mode": "fixed", "fixedColor": color}, "custom.drawStyle": "points", "custom.pointSize": 9,
                            "custom.hideFrom": {"legend": True, "tooltip": True, "viz": False}})

    # A real deviation at every second, faded (a quarter of its colour's opacity), under the bright line of the side its real spread
    # can fill on: neither in the legend nor in the tooltip.
    def faded(name, color):
        return series(name, color + FADED, width=1.5, legend=False, tooltip=False)
    lines_x = [count_of("Long Points", BLUE), count_of("Short Points", ORANGE)] + [sigma_line(name) for name, _ in ENTRY_SIGMAS] + [
               series("Deviation", GREY, width=1, tooltip=False),
               faded("Real Long Deviation · Faded", BLUE), faded("Real Short Deviation · Faded", ORANGE),
               series("Real Long Deviation", BLUE, width=1.5, tooltip=False), series("Real Short Deviation", ORANGE, width=1.5, tooltip=False),
               series("Past T", WARN, width=2, legend=False, tooltip=False),
               series("+Entry", TEAL, width=1.5, dash=[6, 4], legend=False, tooltip=False),
               dots("Long Dots", BLUE), dots("Short Dots", ORANGE),
               twin("Deviation · Tooltip", GREY, "Deviation​"), twin("Real Long Deviation · Tooltip", BLUE, "Real Long Deviation​"),
               twin("Real Short Deviation · Tooltip", ORANGE, "Real Short Deviation​"), tooltip_only("Signal", TEXT, signal)]
    # Thirteen rows high: at 880 px the legend under the plot shows its last row whole (twelve leaves it 4 px short).
    panels.append(timeseries("Entry Points · Bps", None, 0, y, 24, 13, lines_x, zero_line=True, soft_min=0, source=spread, calcs=["max"],
                             transformations=[keep_fields(ENTRY_FIELDS + [name for name, _ in ENTRY_SIGMAS])] +
                             [organize(ENTRY_ORDER, rename={"Deviation · Line": "Deviation"}, exclude=["Sigma"])],
                             description=(
        "Deviation = |Spread − Mean| · Real Long Deviation = |Real Long Spread − Mean| · Real Short Deviation = |Real Short Spread − Mean| · "
        "Long: Real Long Spread ≤ Mean − Entry · Short: Real Short Spread ≥ Mean + Entry\n\n"
        "All three are absolute (with their signs: Deviation · Bps); each real line is bright on the side of the Mean its real spread can fill on and faded on the other: "
        "Real Long Deviation (blue) bright while Real Long Spread is below the Mean, Real Short Deviation (orange) bright while Real "
        "Short Spread is above it, so a bright line above Entry (teal dashed) is a signal of its side and a faded one is none (Signal · "
        "Latest Second shows a faded side as Other Side) · grey: 1, 1.5, 2, 2.5, 3 σ (whole σ dashed, half σ dotted) · Long Points, "
        "Short Points: a dot on the blue, orange line at each drawn second whose Signal is Long, Short (Partial included), counted in "
        "the legend · Real Long, Real Short Deviation: buying, selling both legs for the notional through their pricing levels, the "
        "Signal's spreads; none without both legs' books · Deviation (grey): on the basis prices, the only line before the recorder · "
        "legend Max: the largest value of each bright line (Entry Points' Best of the range), the counts in "
        "the range · tooltip: each row is that second's value, absolute whatever its side, or the reason it has none (No Anchor, Past "
        "T, No Mean, No Notional, No Book, Few Levels), and the Signal · No Entry (Entry not a positive number): no Entry line, no dots · empty "
        "until the session's mean is ready, and with no valid Mean Scale · Past T, amber: the deviation from the instant a leg's "
        "dividend takes effect without its amount until it is known, no signal and not counted · points are coarser on longer ranges, "
        "so counts do not compare across ranges."))); y += 13
    whole = dict(custom_align="right", unit="locale", decimals=0)
    bps = dict(custom_align="right", unit="none", decimals=1)
    # Eight rows high: the header and all five phases without scrolling.
    panels.append(table("Entry Points by Phase · Bps", None, 0, y, 24, 8, source=spread, transformations=ENTRY_TRANSFORMS, description=(
        "Long Points: Real Long Spread ≤ Mean − Entry · Short Points: Real Short Spread ≥ Mean + Entry · Best: the largest absolute "
        "deviation on the favourable side\n\n"
        "The drawn points of the time range with a deviation, by the US market phase they fall in, counted as the Signal reads them, at "
        "the notional (none Long, Short or No Book with No Entry or No Notional) · Partial Points: Long and Short points whose side the "
        "recorded levels filled less than N, already in Long Points and Short Points · Best Long: the largest Real Long Deviation of the "
        "points whose Real Long Spread is below the Mean, Best Short: the largest Real Short Deviation of those whose Real Short Spread "
        "is above it (the legend Max of the bright lines on Entry Points · Bps); how far each side came toward or past its Entry (not past "
        "T; – where none) · Past T Points: points past the instant a leg's dividend took effect without its amount, amber · No Book "
        "Points: points without both legs' books (on trade prices) · these two are in neither Long nor Short Points · Points: every "
        "point with a deviation, these included · counts depend on the range, as in the chart."), overrides=[
        # 736 px in all, the headers wrapped: every column shows in the 740 px an 880 px window leaves the table.
        col("Phase", 96, custom_cellOptions={"type": "color-text"}, mappings=[PHASE_NAMES]),
        col("Long Points", 80, **whole), col("Short Points", 80, **whole), col("Partial Points", 80, **whole), col("Best Long", 80, **bps),
        col("Best Short", 80, **bps),
        col("Past T Points", 80, custom_cellOptions={"type": "color-text"}, color={"mode": "thresholds"}, thresholds=ANY, **whole),
        col("No Book Points", 80, **whole), col("Points", 80, **whole)])); y += 8

    panels.append(row("Prices · ${venue:text}", y)); y += 1
    # Return has its own axis on the right, in percent. Anchor (USD) is there for the tooltip and the legend, as Deviation is on the
    # Spread chart: it has no line and, on a hidden axis of its own, does not stretch the price axis.
    return_line = named("Return", {"color": {"mode": "fixed", "fixedColor": GREY}, "custom.lineWidth": 1, "custom.axisPlacement": "right",
                                   "unit": "percent", "decimals": 2})
    anchor_usd = named("Anchor (USD)", {"color": {"mode": "fixed", "fixedColor": LIGHT_GREY}, "custom.lineWidth": 0, "custom.axisPlacement": "hidden"})
    bell = "the bell, F × Binance's index of the perp at the close (Bitget's before Binance lists the perp)"
    on_nav = bell + ", replaced for the whole session by the issuer's NAV once stable"
    adjusted = "; less a dividend, or over a split's ratio, from the instant it takes effect"
    first = None
    for leg, name, color, anchor, about in (("bull", "Bull", BLUE, on_nav, ""), ("bear", "Bear", ORANGE, on_nav, ""),
                                            ("ref", "Ref", TEXT, bell, " · ${ref}: the unleveraged ETF of the same index")):
        lines = [series("Price", color, width=2), series("Anchor (USDT)", LIGHT_GREY, dash=[8, 4]), anchor_usd, return_line]
        chart = timeseries("${%s} Price · USDT" % leg, Q_PRICES if first is None else None, 0, y, 24, 10, lines, decimals=2, right_axis=True,
                           source=first, transformations=price_fields(name), description=(
            "Return = Price ÷ Anchor (USDT) − 1 · Anchor (USDT) = Anchor (USD) ÷ F\n\n"
            "Price: perp last trade as of each point's time, at the seconds both legs have one, none during a split's halt · Anchor (USD): "
            "the session's anchor, " + anchor + adjusted + "; legend and tooltip only · Anchor (USDT): the price where Return is 0, moves "
            "slightly with F · F: USD/USDT · Return: right axis, %" + about + "."))
        panels.append(chart); y += 10
        first = first or chart
    # Sum is two orders smaller than the legs: it has its own axis on the right. Both axes are centred on zero, so their zeros coincide.
    centred = {"custom.axisCenteredZero": True}
    implied = [named("Sum", dict(centred, **{"color": {"mode": "fixed", "fixedColor": PURPLE}, "custom.lineWidth": 1, "custom.axisPlacement": "right",
                                              "decimals": 3, "custom.thresholdsStyle": {"mode": "off"}})),
               named("Reference", dict(centred, **{"color": {"mode": "fixed", "fixedColor": TEXT}, "custom.lineWidth": 1})),
               named("Bull Leg", dict(centred, **{"color": {"mode": "fixed", "fixedColor": BLUE}, "custom.lineWidth": 1})),
               named("Bear Leg", dict(centred, **{"color": {"mode": "fixed", "fixedColor": ORANGE}, "custom.lineWidth": 1}))]
    implied_fields = ["time", "Sum", "Ref Return", "Bull Leg", "Bear Leg"]
    panels.append(timeseries("Implied Return · %", None, 0, y, 24, 10, implied, decimals=2, zero_line=True, right_axis=True, source=first,
                             transformations=[keep_fields(implied_fields), organize(implied_fields, rename={"Ref Return": "Reference"})], description=(
        "Bull Leg = (F·p_bull ÷ A_bull − 1) ÷ 3 · Bear Leg = (F·p_bear ÷ A_bear − 1) ÷ 3 · Sum = Bull Leg + Bear Leg\n\n"
        "The index return each leg implies since the close, on the perps' last trades · Sum: the spread on those trades ÷ 3 in % (0.01 % "
        "= 1 bps), right axis; the drawn Spread is on mid prices, apart from it by the trades against the mids · Reference: ${ref}'s own "
        "return · each on its anchor less a dividend, or over a split's ratio, from the instant it takes effect."))); y += 10

    panels.append(row("Costs · Entry ${d_text}", y)); y += 1
    num = dict(custom_align="right", unit="none", decimals=2)
    panels.append(table("Fee Levels · ${venue:text} · Bps", Q_FEES, 0, y, 24, 5, description=(
        "Breakeven Entry = 2 × Taker · Net at Entry = Entry − Breakeven Entry\n\n"
        "One round trip across the band, bps of both legs' notional: four taker fills; the Signal is on the real spreads of the notional, so "
        "crossing the spread and going down the levels are inside it · Half Spreads: both legs' half spreads in the newest second with a book "
        "of both, what crossing the best levels costs now · Net at Entry: – when Entry is not a positive number of bps (also when it is a "
        "multiple of sigma: compare k × Sigma of Spread · Latest Second with Breakeven Entry) · funding not included."),
        overrides=[
        col("Level", 150), col("Taker", 76, **num), col("Half Spreads", 116, **num), col("Breakeven Entry", 136, **num),
        col("Net at Entry", 116, custom_cellOptions={"type": "color-text"}, color={"mode": "thresholds"}, thresholds=SIGNED, **num),
        col("Source", custom_minWidth=320)])); y += 5

    panels.append(row("Data Quality", y)); y += 1
    when = dict(unit="time:MM-DD HH:mm")
    usd = dict(custom_align="right", **usd)
    panels.append(table("Anchors · Bell and NAV", Q_ANCHORS, 0, y, 24, 11, description=(
        "Anchor = both legs' NAV once the issuer has given the same figure for 1 h, else both legs' Bell = F × the perp's index at the close\n\n"
        "Session: the New York day whose close starts it · Bell: F × the close of the perp's one-minute index kline that ends at the close, "
        "Binance's, or Bitget's before Binance lists the perp, USD · NAV: the issuer's current figure · Known Time: "
        "when the anchor in use became known (a bell filled in later: close + 2 min; a NAV loaded from the issuer's history: its load time) · "
        "Missing: no bell anchor yet, or none · Actions: the session's dividends (ex, USD as declared) and splits, when each takes effect on "
        "this CEX and its status: Scheduled, Known, None (no instant), then Applied, Verified, Mismatch, and known MM-DD HH:MM when the amount "
        "came after that instant; amber: Mismatch, or Past T without an amount · anchors shown are the session's start, before an action; "
        "coming actions: Next Actions · times UTC+8."),
        overrides=[
        col("Session", 96), col("Close Time", 96, **when), col("${bull} Bell", 84, **usd), col("${bull} NAV", 84, **usd),
        col("${bull} Anchor", 88, **usd), col("Known Time", 96, **when),
        col("Status", 72, custom_cellOptions={"type": "color-text"}, mappings=[STATUS]),
        # The rest of the width, the text wrapped (two actions of a session are about 80 characters: three lines at 1280 px).
        col("Actions", custom_minWidth=240, custom_wrapText=True, custom_cellOptions={"type": "color-text"}, mappings=[ACTION_WARN])])); y += 11
    count = dict(custom_align="right", unit="locale", decimals=0)
    panels.append(table("Data Coverage · Time Range", Q_COVERAGE, 0, y, 24, 6, description=(
        "Missing Minutes = minutes of the range without a candle close · Book Age = seconds since the newest second with a book\n\n"
        "Seconds: the recorder's per-second rows with a last price, which give the points and the mean's minute bars · Book Seconds: those "
        "with a best bid and a best ask · Levels: the fewest levels the thinner side and the most the deeper side of those books held, the "
        "depth the real spreads can take (Bitget SOXS: 5 until 11-20 18:43, 10 until 11-22 12:16, 50 since; Binance SOXL, SOXS: 1 from "
        "11-19 19:15 to 11-20 18:43) · Candle Minutes: the exchanges' one-minute candles, the mean's bar only where a minute's end has "
        "neither both legs' books nor both legs' trades; a minute without any bar restarts the mean · Book Age amber from 60 s, red "
        "from 300 s."), overrides=[
        col("Contract", 104), col("Candle Minutes", 130, **count), col("Missing Minutes", 144, **count), col("Seconds", 104, **count),
        col("Book Seconds", 120, **count), col("Levels", 80, custom_align="right"),
        col("Book Age s", 116, custom_align="right", unit="none", decimals=0, custom_cellOptions={"type": "color-text"}, color={"mode": "thresholds"},
            thresholds=AGE)]))

    return {"panels": panels, "templating": {"list": templating},
            "annotations": {"list": [bands(name, phase_marks(name), color) for name, color in PHASES.items()] +
                                    [marks("Corporate Actions", Q_MARKS, PURPLE, [spread["id"]]),
                                     marks("Anchor Changes", Q_ANCHOR_CHANGES, PURPLE, [p["id"] for p in panels if p["type"] == "timeseries"])]},
            "links": [reset_link("combo-spread", templating, "now-6h", "30s", keep=("combo", "venue"),
                                 tooltip="Settings, time range and refresh back to defaults; Combo and CEX stay.")],
            "time": {"from": "now-6h", "to": "now"}, "refresh": "30s", "timepicker": {"refresh_intervals": ["10s", "30s", "1m", "5m"]}, "tags": [],
            "description": "Basket spread of a bull and a bear ETF perp on one exchange, against its moving mean and the entry band, session by session."}
