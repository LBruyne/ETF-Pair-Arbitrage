#!/usr/bin/env python3
"""Checks of the generated boards.

    python3 dashboard/grafana/test_boards.py

The address rules, the board's shape, the panels that share a query, the tooltips and legends, and the board's description
in dashboard/SPEC.md need nothing. Running the queries needs a database with the board's schema: set ETFPAIR_BOARD_PSQL to
psql connection arguments ("-h 127.0.0.1 -p 5432 -U etfpair_reader -d etfpair", the password in PGPASSWORD); without it
those tests are skipped. "Now" is the newest second the database holds (Bitget SOXLUSDT), so that a copy of the database
reads as the live one did at that second. The Live Monitor's checks (test_live_monitor.py) run here too; its queries need
ETFPAIR_TRADE_DSN (the trade reader's DSN).
"""
import csv, io, math, os, re, shlex, subprocess, sys, time, types, unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import boardkit
import combo_spread_board as board
import live_monitor_board
# The Live Monitor's own checks (test_live_monitor.py) run with these.
from test_live_monitor import Queries as LiveMonitorQueries, Shape as LiveMonitorShape  # noqa: F401

PSQL = os.environ.get("ETFPAIR_BOARD_PSQL")
# The board's full description; the panels' notes point to it.
SPEC = os.path.join(os.path.dirname(os.path.abspath(__file__)), os.pardir, "SPEC.md")
# The selections the queries are run with: each combo and exchange, both
# means, a short and a long range, a fractional entry, notionals from the
# list and typed with a separator of thousands or spaces.
SOXL = {"combo": "SOXL-SOXS", "bull": "SOXL", "bear": "SOXS", "ref": "SOXX"}
TQQQ = {"combo": "TQQQ-SQQQ", "bull": "TQQQ", "bear": "SQQQ", "ref": "QQQ"}
SELECTIONS = [
    dict(SOXL, venue="binance", ma="sma", ma_minutes="60", sigma_minutes="60", d="10", notional="10000", hours=6),
    dict(SOXL, venue="bitget", ma="ema", ma_minutes="240", sigma_minutes="30", d="7.5", notional="100000", hours=168),
    dict(SOXL, venue="bitget", ma="sma", ma_minutes="10", sigma_minutes="120", d="5", notional="500", hours=1),
    dict(TQQQ, venue="binance", ma="ema", ma_minutes="30", sigma_minutes="30min", d="8", notional="20,000", hours=24),
    dict(TQQQ, venue="bitget", ma="sma", ma_minutes="120", sigma_minutes="5", d="6", notional=" 50000 ", hours=48),
]
TEXT = {"venue": {"binance": "Binance", "bitget": "Bitget"}, "ma": {"sma": "SMA", "ema": "EMA"}}
ZWSP = "\u200b"
# What each panel that shows another's result takes it from.
SHARED = {"Spread · Latest Second": "Prices · Latest Second", "Signal · Latest Second · Notional ${n_text} USDT": "Prices · Latest Second",
          "Book · Latest Second · Pricing Levels": "Prices · Latest Second", "Deviation · Bps": "Spread · Bps", "Entry Points · Bps": "Spread · Bps",
          "Entry Points by Phase · Bps": "Spread · Bps",
          "${bear} Price · USDT": "${bull} Price · USDT", "${ref} Price · USDT": "${bull} Price · USDT", "Implied Return · %": "${bull} Price · USDT"}
# The Spread chart's tooltip rows in order (DESIGN 1.9) and the Deviation and Entry Points charts', as shown (the zero-width
# space left out).
SPREAD_TOOLTIP = ["Spread", "Real Long Spread", "Real Short Spread", "Mean", "Sigma", "Deviation", "Real Long Deviation", "Real Short Deviation",
                  "Signal"] + ["%s 1–${%s_levels} · ${%s} · %s" % (side, leg, leg, unit) for unit in ("ct", "USDT") for side, leg in
                               (("Bid", "bull"), ("Bid", "bear"), ("Ask", "bull"), ("Ask", "bear"))]
DEVIATION_TOOLTIP = ["Deviation", "Real Long Deviation", "Real Short Deviation", "Signal"]
SIGNED_TOOLTIP = ["Deviation", "Real Long Deviation", "Real Short Deviation", "Sigma", "Signal"]
LEGENDS = {"Spread · Bps": ["Spread", "Mean", "Mean + Entry", "Sigma"],
           "Deviation · Bps": ["Deviation", "Real Long Deviation", "Real Short Deviation", "Sigma"],
           "Entry Points · Bps": ["Long Points", "Short Points", "Deviation", "Real Long Deviation", "Real Short Deviation"]}
# The Deviation chart's columns of the Spread row's frame (OWNER.md item 22): the signed deviations, drawn and for the tooltip.
SIGNED = ["Signed Deviation", "Signed Real Long Deviation", "Signed Real Short Deviation", "Signed Past T", "−Entry", "Signed Deviation · Tooltip",
          "Signed Real Long Deviation · Tooltip", "Signed Real Short Deviation · Tooltip"]
PHASE_ORDER = ["Pre-Market", "Regular", "After-Hours", "Overnight", "Closed"]


def substitute(q, sel, now_ms, end_ms=None):
    """Fills a query's variables the way Grafana does for this board, in one pass, a single quote in a value doubled (Grafana's
    escaping of a single-value variable for an SQL data source); the range ending at end_ms (now_ms by default, moved ahead
    hours later by sel["ahead"]) and now() read as now_ms where the board takes now in milliseconds."""
    to_ms = (end_ms if end_ms is not None else now_ms) + sel.get("ahead", 0) * 3600_000
    values = dict(dict(sigma_minutes="60"), **sel, __from=str(int(to_ms - sel["hours"] * 3600_000)), __to=str(int(to_ms)))
    q = q.replace(board.NOW_MS, "%d::bigint" % now_ms)

    def fill(m):
        name, form = (m.group(1), m.group(2)) if m.group(1) else (m.group(3), None)
        if form == "text":
            return TEXT.get(name, {}).get(values[name], values[name])
        return str(values[name]).replace("'", "''")
    return re.sub(r"\$\{(\w+)(?::(\w+))?\}|\$(\w+)", fill, q)


def typed(q, var, text):
    """q with one textbox variable filled as Grafana fills it (a single quote doubled)."""
    return q.replace("$" + var, text.replace("'", "''"))


def psql(q, *args):
    return subprocess.run(["psql", "-X", *shlex.split(PSQL), "-v", "ON_ERROR_STOP=1", *args, "-c", q], capture_output=True, text=True, check=True).stdout


def rows_of(q):
    return list(csv.DictReader(io.StringIO(psql(q, "--csv"))))


# How far before the newest second a test that compares runs pins its "now": by then the recorder's rows, the minute's
# USD/USDT and its candle closes are final.
SETTLED_MS = 300_000


def newest_ms():
    """The newest second the database holds: "now" for the queries that read the newest seconds."""
    return int(psql("SELECT max(second_ms) FROM etfpair.perp_second WHERE venue = 'bitget' AND symbol = 'SOXLUSDT' "
                    "AND second_ms <= extract(epoch FROM now()) * 1000", "-At"))


def queries(b):
    for p in b["panels"]:
        for t in p.get("targets", []):
            if "rawSql" in t:
                yield p["title"], t["rawSql"]
    for v in b["templating"]["list"]:
        if v["type"] == "query":
            yield "variable " + v["name"], v["query"]
    for a in b["annotations"]["list"]:
        yield "annotation " + a["name"], a["rawQuery"]


def columns(q):
    """The columns a panel query returns, in order: its time column and its quoted names."""
    return (["time"] if re.search(r"AS time\b", q) else []) + re.findall(r'AS "([^"]+)"\s*(?:,|$|\n)', q)


def by_title(b, title):
    return next(p for p in b["panels"] if p["title"] == title)


def source_of(b, p):
    t = p["targets"][0]
    return next(s for s in b["panels"] if s["id"] == t["panelId"]) if "panelId" in t else p


def fields(b, p):
    """The fields a panel draws: its source's query columns through the panel's own transformations (keep, copy, organize).
    No binary calculation: Grafana reads a NULL operand as 0, so a line made that way is drawn at 0 where it should break
    (3.5's sigma lines, fixed in 3.5.1)."""
    names = columns(source_of(b, p)["targets"][0]["rawSql"])
    for t in p.get("transformations", []):
        o = t["options"]
        if t["id"] == "filterFieldsByName":
            names = [n for n in names if n in o["include"]["names"]]
        elif t["id"] == "calculateField" and o["mode"] == "reduceRow" and len(o["reduce"]["include"]) == 1 and not o["replaceFields"]:
            assert o["reduce"]["include"][0] in names, o
            names = names + [o["alias"]]
        elif t["id"] == "filterByValue":
            pass  # rows only
        elif t["id"] == "organize":
            names = sorted((n for n in names if not o["excludeByName"].get(n)), key=lambda n: o["indexByName"].get(n, len(names)))
            names = [o["renameByName"].get(n, n) for n in names]
        else:
            raise AssertionError("not a field transformation: %s" % t["id"])
    return names


def props_of(p):
    return {o["matcher"]["options"]: {x["id"]: x["value"] for x in o["properties"]} for o in p["fieldConfig"]["overrides"]}


def utc8_ms(text):
    """A UTC+8 wall time "YYYY-MM-DD HH:MM" in milliseconds."""
    import datetime
    return int(datetime.datetime.strptime(text + " +0800", "%Y-%m-%d %H:%M %z").timestamp() * 1000)


def js_round(v):
    """JavaScript's Math.round: to the nearest integer, halves up."""
    f = math.floor(v)
    return f + 1 if v - f >= 0.5 else f


def shown_text(value, decimals):
    """A number as Grafana's toFixed shows it with that many decimals."""
    if value == 0:
        return "%.*f" % (decimals, 0)
    factor = 10 ** decimals
    r = js_round(value * factor) / factor
    text = str(int(r)) if r == int(r) else repr(r)
    point = text.find(".")
    precision = 0 if point < 0 else len(text) - point - 1
    return text + ("." if not precision and decimals else "") + "0" * (decimals - precision) if precision < decimals else text


def unrounded_module():
    """The board module with shown() taking no decimals off: the queries as they would be without the board's rounding."""
    with open(board.__file__, encoding="utf-8") as f:
        src = f.read()
    line = '    return "(floor((%s)::float8 * 1e%d::float8 + 0.5) / 1e%d::float8)" % (x, decimals, decimals)'
    assert line in src
    mod = types.ModuleType("board_unrounded")
    mod.__file__ = board.__file__
    exec(compile(src.replace(line, '    return "(%s)" % x'), board.__file__, "exec"), mod.__dict__)
    return mod


def decimals_of(sql_text):
    """The decimals shown() rounds a column's value to, None when it does not."""
    found = set(re.findall(r"\* 1e(\d)::float8 \+ 0\.5", sql_text))
    assert len(found) <= 1, sql_text
    return int(found.pop()) if found else None


class Boards(unittest.TestCase):
    def test_addresses(self):
        for name in boardkit.BOARDS:
            uid = boardkit.board_uid(name)
            self.assertRegex(uid, r"^arb-etf-pair-[a-z0-9-]+$")
            self.assertLessEqual(len(uid), 40)  # Grafana's limit
            self.assertEqual(boardkit.board_path(name), "/dashboards/arbitrage/etf-pair/" + boardkit.board_slug(name))
            self.assertIn("location = %s " % boardkit.board_path(name), boardkit.board_paths_conf())
            self.assertIn("return 302 /d/%s/%s$is_args$args" % (uid, boardkit.board_slug(name)), boardkit.board_paths_conf())

    def test_board_shape(self):
        b = board.build()
        ids = [p["id"] for p in b["panels"]]
        self.assertEqual(len(ids), len(set(ids)))
        names = [v["name"] for v in b["templating"]["list"]]
        self.assertEqual(names[:7], ["combo", "venue", "ma", "ma_minutes", "sigma_minutes", "d", "notional"])
        # A list's default is one of its options: Grafana replaces one that is not by the first option (3.5 opened on Entry 0.5σ).
        for v in b["templating"]["list"]:
            if v["type"] == "custom":
                self.assertIn(v["current"]["value"], [o["value"] for o in v["options"]], v["name"])
        self.assertEqual(next(v for v in b["templating"]["list"] if v["name"] == "d")["current"]["value"], "10")
        for title, q in queries(b):
            # Every variable a query uses is one the board defines; the typed settings only as a whole quoted literal (the text
            # parsed in SQL, never pasted into it).
            for var in re.findall(r"\$\{?(\w+)", q):
                self.assertIn(var, names + ["__from", "__to"], title)
            for var in ("notional", "d", "ma_minutes", "sigma_minutes"):
                self.assertEqual(len(re.findall(r"\$\{?%s\b" % var, q)), q.count("'$%s'" % var), (title, var))
        # No panel overlaps another on the grid.
        cells = set()
        for p in b["panels"]:
            g = p["gridPos"]
            for x in range(g["x"], g["x"] + g["w"]):
                for y in range(g["y"], g["y"] + g["h"]):
                    self.assertNotIn((x, y), cells, p["title"])
                    cells.add((x, y))
        self.assertEqual(boardkit.SCHEMA_VERSION, 42)

    def test_shared_queries(self):
        # A page computes each range once: the panels in SHARED show their source's result through Grafana's -- Dashboard -- data
        # source, untransformed (withTransforms false), each through its own transformations. A source runs a query of its own,
        # is the first panel of an expanded row (a source in a collapsed row runs twice on load) and comes before the panels
        # that read it. A shared frame's column names hold no variable (its shown names may).
        b = board.build()
        got = {}
        for p in b["panels"]:
            if p.get("datasource") == boardkit.DASHBOARD:
                (t,) = p["targets"]
                self.assertEqual((t["datasource"], t["withTransforms"]), (boardkit.DASHBOARD, False), p["title"])
                src = source_of(b, p)
                got[p["title"]] = src["title"]
                self.assertEqual(src["datasource"], boardkit.PG, src["title"])
                self.assertLess(src["gridPos"]["y"], p["gridPos"]["y"], p["title"])
                rows = [r for r in b["panels"] if r["type"] == "row" and r["gridPos"]["y"] < src["gridPos"]["y"]]
                self.assertFalse(rows[-1]["collapsed"], src["title"])
                self.assertEqual(rows[-1]["gridPos"]["y"] + 1, src["gridPos"]["y"], src["title"])
                for name in columns(src["targets"][0]["rawSql"]):
                    self.assertNotIn("${", name, src["title"])
                keep = [t["options"]["include"]["names"] for t in p.get("transformations", []) if t["id"] == "filterFieldsByName"]
                for names in keep:
                    self.assertLessEqual(set(names), set(columns(src["targets"][0]["rawSql"])), p["title"])
        self.assertEqual(got, SHARED)
        # Entry Points by Phase: exactly Filter data by values (points with a phase) -> Group by -> Sort by the phase's place -> Organize,
        # whose names are those the Group by gives (before its own renaming).
        entries = by_title(b, "Entry Points by Phase · Bps")
        self.assertEqual([t["id"] for t in entries["transformations"]], ["filterByValue", "groupBy", "sortBy", "organize"])
        flt, grp, srt, org = (t["options"] for t in entries["transformations"])
        self.assertEqual(flt["filters"], [{"fieldName": "Phase", "config": {"id": "isNotNull", "options": {}}}])
        self.assertEqual((flt["type"], srt["sort"]), ("include", [{"field": "Phase", "desc": False}]))
        grouped = [k for k, v in grp["fields"].items() if v["operation"] == "groupby"] + [
            "%s (%s)" % (k, r) for k, v in grp["fields"].items() if v["operation"] == "aggregate" for r in v["aggregations"]]
        self.assertEqual(sorted(org["indexByName"]), sorted(grouped))
        self.assertEqual(org["excludeByName"], {})
        self.assertLessEqual(set(grp["fields"]), set(columns(source_of(b, entries)["targets"][0]["rawSql"])))
        shown = [org["renameByName"].get(n, n) for n in sorted(org["indexByName"], key=org["indexByName"].get)]
        self.assertEqual(shown, ["Phase", "Long Points", "Short Points", "Partial Points", "Best Long", "Best Short", "Past T Points", "No Book Points",
                                 "Points"])
        self.assertEqual(sorted(props_of(entries)), sorted(shown))
        # The frame holds the phase's place (combo_range's phase_sort), the table shows its name in its band's colour.
        self.assertEqual({k: v["text"] for k, v in props_of(entries)["Phase"]["mappings"][0]["options"].items()},
                         {str(i + 1): name for i, name in enumerate(PHASE_ORDER)})

    def test_tooltips_and_legends(self):
        # The two charts' tooltips hold only tooltip-only series (not drawn, not in the legend), in DESIGN 1.9's order: each
        # point's value or the reason it has none as text, the Signal coded too. A twin of a line drawn in the same panel is
        # shown under the line's name and a zero-width space (a twin of the very same name would keep its line drawn when the
        # legend isolates another). Every series in the legend shows – where it has no value; tooltip-only series need
        # schemaVersion 42 (Grafana 13 migrates an older board's viz-hidden series out of the tooltip).
        b = board.build()
        codes = {str(k): v for k, v in board.NO_VALUE.items()}
        signals = {str(v): k for k, v in board.SIGNALS.items()}
        self.assertEqual(set(signals.values()), {"Long", "Short", "Long · Partial", "Short · Partial", "None", "No Anchor", "Past T", "No Mean",
                                                 "No Notional", "No Entry", "No Book", "No Sigma"})
        # Every code is a value of its own, and every Signal text has a colour (No Entry amber, as No Notional).
        self.assertEqual(len(set(board.NO_VALUE) | {board.FULL} | {v for v in board.SIGNALS.values() if v >= 0}), len(board.NO_VALUE) + 1 + 5)
        self.assertEqual(set(board.SIGNAL["options"]), set(board.SIGNALS))
        self.assertEqual(board.SIGNAL["options"]["No Entry"]["color"], boardkit.WARN)
        self.assertEqual(board.WHY["options"][str(board.CODE["Other Side"])], {"text": "Other Side", "color": boardkit.GREY,
                                                                              "index": list(board.NO_VALUE).index(board.CODE["Other Side"])})
        for title, tooltip in (("Spread · Bps", SPREAD_TOOLTIP), ("Deviation · Bps", SIGNED_TOOLTIP), ("Entry Points · Bps", DEVIATION_TOOLTIP)):
            chart = by_title(b, title)
            props, names = props_of(chart), fields(b, chart)
            self.assertEqual(sorted(props), sorted(n for n in names if n != "time"), title)
            drawn = [n for n in names if n != "time" and not props[n].get("custom.hideFrom", {}).get("viz")]
            in_tooltip = [n for n in names if n != "time" and not props[n].get("custom.hideFrom", {}).get("tooltip")]
            self.assertEqual([props[n].get("displayName", n).replace(ZWSP, "") for n in in_tooltip], tooltip, title)
            for name in in_tooltip:
                self.assertEqual(props[name]["custom.hideFrom"], {"legend": True, "tooltip": False, "viz": True}, name)
                self.assertEqual({k: v["text"] for k, v in props[name]["mappings"][0]["options"].items()}, signals if name == "Signal" else codes, name)
                text = props[name].get("displayName", name)
                self.assertEqual(text.endswith(ZWSP), text.replace(ZWSP, "") in drawn, (title, name))
            legend = [n for n in names if n != "time" and not props[n].get("custom.hideFrom", {}).get("legend")]
            self.assertEqual(legend, LEGENDS[title], title)
            for name in legend:
                self.assertEqual(props[name]["noValue"], "–", name)
        # The Deviation chart (OWNER.md item 22): the Spread chart with its Mean at zero: Deviation purple and 2 px as the Spread,
        # drawn first, both real lines over it at every second (1 px, no faded twin, no fill: their inner edges decide a signal,
        # UI review r1 S1); the band filled teal as Mean ± Entry on two copies made in the browser (no line, drawn first) and its
        # edges dashed in the colour of the line each triggers, −Entry blue (Long), +Entry orange (Short) (review r1 M1); the
        # band and the amber Past T out of the legend; the zero line in the middle of the axis; the legend's Last, Min and Max,
        # in the frame's order; every figure with its sign, the real lines on both sides of the Mean.
        chart = by_title(b, "Deviation · Bps")
        props, names = props_of(chart), fields(b, chart)
        sigma_lines = [name for name, _ in board.DEVIATION_SIGMAS]
        self.assertEqual([n for n in names if n != "time"][:18], ["Band Top", "Band Bottom"] + sigma_lines + [
            "Deviation", "Real Long Deviation", "Real Short Deviation", "+Entry", "−Entry", "Past T"])
        self.assertNotIn("sortBy", chart["options"]["legend"])
        for name, color, width in (("Deviation", boardkit.PURPLE, 2), ("Real Long Deviation", boardkit.BLUE, 1), ("Real Short Deviation", boardkit.ORANGE, 1),
                                   ("Past T", boardkit.WARN, 2), ("Band Top", boardkit.TEAL, 0), ("Band Bottom", boardkit.TEAL, 0)):
            self.assertEqual((props[name]["color"]["fixedColor"], props[name]["custom.lineWidth"]), (color, width), name)
        self.assertEqual((props["Band Top"]["custom.fillBelowTo"], props["Band Top"]["custom.fillOpacity"]), ("Band Bottom", 8))
        for name, color in (("+Entry", boardkit.ORANGE), ("−Entry", boardkit.BLUE)):
            self.assertEqual((props[name]["color"]["fixedColor"], props[name]["custom.lineStyle"]["fill"], props[name]["custom.lineWidth"]),
                             (color, "dash", 1.5), name)
        for name in ("Deviation", "Real Long Deviation", "Real Short Deviation", "Past T", "+Entry", "−Entry", "Band Bottom"):
            self.assertNotIn("custom.fillBelowTo", props[name], name)
        for name in ("Band Top", "Band Bottom", "+Entry", "−Entry", "Past T"):
            self.assertEqual(props[name]["custom.hideFrom"], {"legend": True, "tooltip": True, "viz": False}, name)
        copies = [t["options"] for t in chart["transformations"] if t["id"] == "calculateField" and t["options"]["mode"] == "reduceRow"]
        self.assertEqual([(c["reduce"]["include"], c["alias"]) for c in copies], [(["+Entry"], "Band Top"), (["−Entry"], "Band Bottom")])
        defaults = chart["fieldConfig"]["defaults"]["custom"]
        self.assertEqual((defaults["thresholdsStyle"], defaults["axisCenteredZero"]), ({"mode": "dashed"}, True))
        self.assertNotIn("axisSoftMin", defaults)
        self.assertEqual(chart["options"]["legend"]["calcs"], ["lastNotNull", "min", "max"])
        for name, q in board.DEVIATION_CHART:
            self.assertNotIn("abs(", q, name)
            self.assertNotIn("< r.mu_bps", q, name)
            self.assertNotIn("> r.mu_bps", q, name)
        self.assertEqual([name for name, _ in board.DEVIATION_CHART], SIGNED)
        # 3.4 (owner item 61): the sigma lines ±1, 1.5, 2, 2.5, 3 σ (Mean ± k σ on the Spread chart, + k σ on Entry Points), columns
        # of the Spread query since 3.5.1 (NULL where Sigma or the Mean is), grey, whole σ dashed and half σ dotted,
        # drawn before the data (behind it), in neither the legend nor the tooltip; Sigma itself a legend row without a line.
        for title, expected in (("Spread · Bps", [name for name, _ in board.SPREAD_SIGMAS]), ("Deviation · Bps", sigma_lines),
                                ("Entry Points · Bps", [name for name, _ in board.ENTRY_SIGMAS])):
            chart_s = by_title(b, title)
            props_s, names_s = props_of(chart_s), fields(b, chart_s)
            self.assertEqual([n for n in names_s if "σ" in n and "Step" not in n], expected, title)
            self.assertEqual(len(expected), 10 if title != "Entry Points · Bps" else 5)
            first_data = min(names_s.index(n) for n in ("Spread", "Deviation") if n in names_s)
            for name in expected:
                k = float(name.split(" ")[-1].lstrip("+−").rstrip("σ"))
                self.assertLess(names_s.index(name), first_data, name)
                self.assertEqual(props_s[name]["custom.hideFrom"], {"legend": True, "tooltip": True, "viz": False}, name)
                self.assertEqual(props_s[name]["custom.lineStyle"]["dash"], [6, 4] if k == int(k) else [2, 3], name)
                self.assertIn(name, dict(board.SIGMA_COLUMNS), name)
            if "Sigma" in props_s and title != "Entry Points · Bps":
                self.assertEqual((props_s["Sigma"]["custom.lineWidth"], props_s["Sigma"]["custom.axisPlacement"]), (0, "hidden"))
        self.assertEqual(dict(board.SPREAD_CHART)["Sigma"], board.shown("r.sigma_bps", 2))
        # The Entry Points chart (the Deviation chart until OWNER.md item 22): two real lines without a fill and a 1 px grey
        # Deviation line; each real line bright (in the legend)
        # on the side of the mean its real spread can fill on and faded (a quarter of the opacity, neither in the legend nor in the
        # tooltip) at every second, the faded one first so that the bright one is drawn over it; Past T, +Entry and the dots drawn
        # but out of the legend; a 9 px dot series at the signal seconds of each side, in its line's colour; the zero line; 0 on
        # the axis; the legend's Last and Max.
        chart = by_title(b, "Entry Points · Bps")
        props, names = props_of(chart), fields(b, chart)
        for name, color in (("Real Long Deviation", boardkit.BLUE), ("Real Short Deviation", boardkit.ORANGE)):
            self.assertNotIn("custom.fillBelowTo", props[name])
            self.assertEqual((props[name]["color"]["fixedColor"], props[name]["custom.lineWidth"]), (color, 1.5), name)
            faded = name + " · Faded"
            self.assertEqual((props[faded]["color"]["fixedColor"], props[faded]["custom.lineWidth"]), (color + "40", 1.5), faded)
            self.assertEqual(props[faded]["custom.hideFrom"], {"legend": True, "tooltip": True, "viz": False}, faded)
            self.assertLess(names.index(faded), names.index(name), faded)
        self.assertEqual((props["Deviation"]["color"]["fixedColor"], props["Deviation"]["custom.lineWidth"]), (boardkit.GREY, 1))
        for name in ("Past T", "+Entry", "Long Dots", "Short Dots"):
            self.assertEqual((props[name]["custom.hideFrom"]["legend"], props[name]["custom.hideFrom"]["viz"]), (True, False), name)
        self.assertEqual(props["+Entry"]["custom.lineStyle"]["fill"], "dash")
        for name, color in (("Long Dots", boardkit.BLUE), ("Short Dots", boardkit.ORANGE)):
            self.assertEqual((props[name]["custom.drawStyle"], props[name]["color"]["fixedColor"], props[name]["custom.pointSize"]),
                             ("points", color, 9), name)
        self.assertEqual(chart["fieldConfig"]["defaults"]["custom"]["thresholdsStyle"], {"mode": "dashed"})
        self.assertEqual(chart["fieldConfig"]["defaults"]["custom"]["axisSoftMin"], 0)
        self.assertEqual(chart["options"]["legend"]["calcs"], ["max"])
        self.assertEqual(by_title(b, "Spread · Bps")["options"]["legend"]["calcs"], ["lastNotNull", "min", "max"])
        # The deviations are absolute everywhere; a bright line only on its favourable side, the faded one at every second; the dots
        # sit on their lines; the signal keeps its side (combo_signal).
        for name, q in board.SPREAD_CHART + board.ENTRY_CHART:
            if "Deviation" in name or name in ("Long Dots", "Short Dots", "Past T"):
                self.assertIn("abs(", q, name)
        lines = dict(board.ENTRY_CHART)
        self.assertIn("r.s_long_bps < r.mu_bps", lines["Real Long Deviation"])
        self.assertIn("r.s_short_bps > r.mu_bps", lines["Real Short Deviation"])
        for name in ("Real Long Deviation · Faded", "Real Short Deviation · Faded"):
            self.assertNotIn("< r.mu_bps", lines[name])
            self.assertNotIn("> r.mu_bps", lines[name])
        self.assertIn(" - ", dict(board.ENTRY_COLUMNS)["Best Long"])
        # The Signal strip's real deviations: a value on the side its real spread can fill on, Other Side on the other.
        last = dict(board.LAST_COLUMNS)
        for name, side in (("Real Long Deviation", "b.s_long_bps < b.mu_bps"), ("Real Short Deviation", "b.s_short_bps > b.mu_bps")):
            self.assertIn("WHEN %s THEN" % side, last[name])
            self.assertIn("THEN %d" % board.CODE["Other Side"], last[name])

    def test_layout(self):
        # Round 2 of the UI review: the strips' text sizes (Trading Phases 18 px figures, Prices 12 px names, the rest 13 / 20),
        # each leg's 1+2 depth with its unit (ct), Next Actions and Calendar Check six rows high, the Deviation and the Entry
        # Points charts thirteen as the Spread chart, Entry Points by Phase eight (the header and five phases) and at most
        # 740 px wide (an 880 px window); the Spread row's panels in the order Spread, Deviation, Entry Points, Entry Points by Phase.
        b = board.build()
        sizes = {p["title"]: p["options"]["text"] for p in b["panels"] if p["type"] == "stat"}
        self.assertEqual(sizes, {"Prices · Latest Second": {"titleSize": 12, "valueSize": 20}, "Spread · Latest Second": {"titleSize": 13, "valueSize": 20},
                                 "Signal · Latest Second · Notional ${n_text} USDT": {"titleSize": 13, "valueSize": 20},
                                 "Trading Phases · UTC+8": {"titleSize": 13, "valueSize": 18}})
        # 3.4: the Book table, a row a leg (Q_LAST's two rows): tick, levels, each side's depth in ct (two decimals) and USDT
        # (none), separators of thousands; the tooltip's depth rows likewise, the unit in their names.
        book_panel = by_title(b, "Book · Latest Second · Pricing Levels")
        self.assertEqual(fields(b, book_panel), ["Leg", "Tick", "Levels", "Bid · ct", "Bid · USDT", "Ask · ct", "Ask · USDT"])
        book, spread = props_of(book_panel), props_of(by_title(b, "Spread · Bps"))
        for name in ("Bid · ct", "Ask · ct", "Bid · USDT", "Ask · USDT"):
            self.assertEqual((book[name]["unit"], book[name]["decimals"]), ("locale", 2 if name.endswith("ct") else 0), name)
        self.assertNotIn("unit", book["Tick"])
        for name, display, _, _ in board.DEPTHS:
            self.assertEqual((spread[name]["unit"], spread[name]["decimals"], spread[name]["displayName"]),
                             ("locale", 2 if name.endswith("ct") else 0, display), name)
        heights = {p["title"]: p["gridPos"]["h"] for p in b["panels"]}
        self.assertEqual([heights[t] for t in ("Book · Latest Second · Pricing Levels", "Next Actions · UTC+8", "Calendar Check · UTC+8", "Deviation · Bps",
                                               "Entry Points · Bps", "Entry Points by Phase · Bps")], [5, 6, 6, 13, 13, 8])
        tops = {p["title"]: p["gridPos"]["y"] for p in b["panels"]}
        order = ["Spread · Bps", "Deviation · Bps", "Entry Points · Bps", "Entry Points by Phase · Bps"]
        self.assertEqual([tops[t] for t in order[1:]], [tops[t] + heights[t] for t in order[:-1]])
        widths = {k: v["custom.width"] for k, v in props_of(by_title(b, "Entry Points by Phase · Bps")).items()}
        self.assertEqual(widths["Phase"], 96)
        self.assertLessEqual(sum(widths.values()), 740)
        # The Trading Phases figures keep their date and time, and the Calendar's words, together: no-break spaces.
        phases = board.Q_PHASES.split(",\n       ")
        self.assertIn("'MM-DD\u00a0HH24:MI'", phases[-2])
        self.assertNotIn(" to ", phases[-1])
        self.assertIn("'OK\u00a0to\u00a0'", phases[-1])
        self.assertRegex("Not\u00a0compared", board.CALENDAR["options"]["pattern"])
        self.assertRegex("Not compared", board.CALENDAR["options"]["pattern"])

    def test_shown_decimals(self):
        # Each figure is rounded once, in SQL, to the decimals its panel shows it with (shown()); the codes and the raw prices are
        # not rounded; a figure two panels show with different decimals would need two columns.
        b = board.build()
        sources = {"Spread · Bps": dict(board.SPREAD_CHART + board.DEVIATION_CHART + board.ENTRY_CHART + board.ENTRY_COLUMNS),
                   "${bull} Price · USDT": dict(board.PRICE_COLUMNS),
                   "Prices · Latest Second": dict(board.LAST_COLUMNS)}
        seen = 0
        for p in b["panels"]:
            if p["type"] == "row" or source_of(b, p)["title"] not in sources:
                continue
            src = source_of(b, p)
            sql_of, props = sources[src["title"]], props_of(p)
            defaults = p["fieldConfig"]["defaults"].get("decimals")
            names = fields(b, p) if p["title"] != "Entry Points by Phase · Bps" else ["Best Long", "Best Short"]
            back = {}
            for t in p.get("transformations", []):
                if t["id"] == "organize":
                    back = {v: k for k, v in t["options"]["renameByName"].items()}
            for name in names:
                original = back.get(name, name)
                if name == "time" or original not in sql_of:
                    continue
                want = decimals_of(sql_of[original])
                if want is None:
                    continue
                self.assertEqual(props.get(name, {}).get("decimals", defaults), want, (p["title"], name))
                seen += 1
        self.assertGreater(seen, 40)
        for name in ("Bull Price", "Bear Price", "Ref Price"):
            self.assertIsNone(decimals_of(dict(board.PRICE_COLUMNS)[name]), name)

    def test_notional_setting(self):
        # Notional (USDT): the list of the owner's common sizes, 10000 by default, any other number typed; Reset sets it back
        # to 10000; the Prices row's query passes no notional.
        b = board.build()
        var = next(v for v in b["templating"]["list"] if v["name"] == "notional")
        self.assertEqual((var["label"], var["type"], var["current"]["value"], var["allowCustomValue"]), ("Notional (USDT)", "custom", "10000", True))
        self.assertEqual([(o["text"], o["value"]) for o in var["options"]], [("100", "100"), ("500", "500"), ("1k", "1000"), ("5k", "5000"),
                                                                             ("10k", "10000"), ("50k", "50000"), ("100k", "100000")])
        self.assertIn("var-notional=10000", b["links"][0]["url"])
        # The typed settings are parsed once per query (SETTINGS, one materialized row v, not a parse per row or per use) and
        # passed from it.
        for q in (board.Q_SPREAD, board.Q_LAST):
            self.assertEqual(q.count(board.SETTINGS), 1)
            self.assertIn("WITH v AS MATERIALIZED (SELECT * FROM %s)" % board.SETTINGS, q)
            self.assertEqual((q.count(board.NOTIONAL), q.count(board.ENTRY), q.count(board.MEAN_SCALE), q.count(board.SIGMA_SCALE)), (1, 1, 1, 1))
        self.assertEqual((board.Q_FEES.count(board.ENTRY), board.Q_FEES.count(board.NOTIONAL)), (1, 0))
        self.assertIn("v.ma_n, v.sig_n, $__from, $__to, %s, v.n, v.d) x" % board.STEP_MS, board.RANGE)
        self.assertIn(board.RANGE_CALL, board.RANGE)
        self.assertIn("v.ma_n, v.sig_n, v.n, v.d, %s) x" % board.NOW_MS, board.LAST)
        self.assertNotIn(board.NOTIONAL, board.Q_PRICES)

    @unittest.skipUnless(PSQL, "set ETFPAIR_BOARD_PSQL to run the queries")
    def test_notional_typed(self):
        # What a viewer may type into Notional (USDT), and the N the titles then show: commas in groups of three, spaces,
        # underscores, k, w or 万, a trailing U or USDT, full-width characters; anything else is no notional, shown as ?.
        cases = {"10000": ("10000", "10,000"), "20,000": ("20000", "20,000"), " 50000 ": ("50000", "50,000"), "20k": ("20000", "20,000"),
                 "20K": ("20000", "20,000"), "2万": ("20000", "20,000"), "2w": ("20000", "20,000"), "20000U": ("20000", "20,000"),
                 "20000 usdt": ("20000", "20,000"), "10000 USDT ": ("10000", "10,000"), "10.5k": ("10500.0", "10,500"),
                 "1,000.5": ("1000.5", "1,000.5"), "100_000": ("100000", "100,000"), "10\u00a0000": ("10000", "10,000"),
                 "１００００": ("10000", "10,000"), "10ｋ": ("10000", "10,000"), "10，000": ("10000", "10,000"), "100U": ("100", "100"),
                 "0": ("0", "0"), "-1": ("", "?"), "abc": ("", "?"), "": ("", "?"), "1e5": ("", "?"), "10kk": ("", "?"),
                 "10,5k": ("", "?"), "2,5万": ("", "?"), "1,0,0,0": ("", "?"), "10000.": ("", "?")}
        query = "SELECT (%s)::text, (%s)" % (board.NOTIONAL, board.N_TEXT)
        got = {typed: tuple(psql(query.replace("$notional", typed), "-At", "-F", "|").strip().split("|")) for typed in cases}
        self.assertEqual(got, cases)

    @unittest.skipUnless(PSQL, "set ETFPAIR_BOARD_PSQL to run the queries")
    def test_entry_and_mean_scale_typed(self):
        # What a viewer may type into Entry (bps) and Mean Scale (min), read as Notional is (inside quotes, NFKC, spaces and
        # underscores out): Entry a number without a sign, bps after it allowed, Mean Scale a whole number of minutes (six digits
        # at most), min or m after it allowed; anything else, and 0, is NULL. A quote typed stays inside its literal.
        entries = {"10": "10", "7.5": "7.5", "10bps": "10", "10 BPS": "10", "１０": "10", "0.25": "0.25", "1_0": "10", "abc": "", "": "", "0": "",
                   "0.0": "", "-5": "", "+5": "", "1'; SELECT 1; --": "", "10%": "", "1e1": "", ".5": "", "5.": "", "10 bp": ""}
        scales = {"60": "60", "30min": "30", "30 m": "30", "３０": "30", "030": "30", "999999": "999999", "abc": "", "": "", "0": "", "30.5": "",
                  "-30": "", "1234567": "", "30h": "", "30 minutes": "", "1'; SELECT 1; --": ""}
        got = {t: psql("SELECT coalesce((%s)::text, '')" % typed(board.ENTRY, "d", t), "-At").strip() for t in entries}
        self.assertEqual(got, entries)
        got = {t: psql("SELECT coalesce((%s)::text, '')" % typed(board.MEAN_SCALE, "ma_minutes", t), "-At").strip() for t in scales}
        self.assertEqual(got, scales)
        # Sigma Scale (3.4) reads as Mean Scale.
        got = {t: psql("SELECT coalesce((%s)::text, '')" % typed(board.SIGMA_SCALE, "sigma_minutes", t), "-At").strip() for t in scales}
        self.assertEqual(got, scales)

    @unittest.skipUnless(PSQL, "set ETFPAIR_BOARD_PSQL to run the queries")
    def test_queries_with_entry_and_mean_scale_not_numbers(self):
        # Every query runs with each input of the round-2 review (FIX_R2 C8) and with the same results as the board defines them:
        # an Entry that is not a positive number gives no Mean ± Entry and no +Entry line, Signal No Entry where a point would have
        # had a side (combo_signal: after No Anchor, Past T, No Mean, No Notional), no Long or Short point, no Net at Entry, and
        # every other figure as with a valid Entry; "10bps" is 10; a Mean Scale that is not a whole number of minutes gives no
        # mean (No Mean) and the same spreads, real spreads and depths; "30min" is 30.
        # The range: the US open hour of Friday 11-20 (signals on both sides at Entry 10); the strips at the newest second.
        b = board.build()
        # "now" pinned once for every run of the test, five minutes before the newest second: on a live database the recorder
        # writes a second's parts up to about 7 s late, and a minute's USD/USDT and candle closes are rewritten for up to about
        # 61 s after it ends (SPEC section 3), so a "now" only 15 s back let two runs see different minute bars (US hours,
        # 2026-11-25: Mean -8.02 against -8.22). Five minutes back every input of the strips' newest seconds is final.
        now_ms, end_ms = newest_ms() - SETTLED_MS, utc8_ms("2026-11-20 23:30")
        base = dict(SOXL, venue="bitget", ma="sma", ma_minutes="30", d="10", notional="10000", hours=1)
        runs = {}

        def run_all(sel):
            out = {}
            for title, q in queries(b):
                result = subprocess.run(["psql", "-X", *shlex.split(PSQL), "-v", "ON_ERROR_STOP=1", "--csv", "-c", substitute(q, sel, now_ms, end_ms)],
                                        capture_output=True, text=True)
                self.assertEqual(result.returncode, 0, "%s with %s:\n%s" % (title, sel, result.stderr))
                # Book Age counts from the clock and Half Spreads reads the newest book, both moving between the runs on a live
                # database.
                out[title] = [{k: v for k, v in r.items() if k not in ("Book Age s", "Half Spreads")}
                              for r in csv.DictReader(io.StringIO(result.stdout))]
            return out
        for key, value in [("d", v) for v in ("10", "10bps", "abc", "", "0", "-5", "1'; SELECT 1; --")] + \
                          [("ma_minutes", v) for v in ("30", "abc", "", "0", "30.5", "30min")] + \
                          [("sigma_minutes", v) for v in ("60", "60min", "abc", "", "0", "30.5")]:
            runs[key, value] = run_all(dict(base, **{key: value}))
        ok, codes = runs["d", "10"], {v: k for k, v in board.SIGNALS.items()}
        spread, last, fees = "Spread · Bps", "Prices · Latest Second", "Fee Levels · ${venue:text} · Bps"
        entry_columns = ("Mean + Entry", "Mean − Entry", "+Entry", "−Entry")
        self.assertEqual(runs["d", "10bps"], ok)
        self.assertTrue(any(r["Signal"] in ("1", "2", "3", "4") for r in ok[spread]))
        for value in ("abc", "", "0", "-5", "1'; SELECT 1; --"):
            got = runs["d", value]
            self.assertEqual({r[c] for r in got[spread] for c in entry_columns}, {""}, value)
            for a, w in zip(got[spread], ok[spread]):
                self.assertEqual({k: v for k, v in a.items() if k not in entry_columns + ("Signal", "Long Points", "Short Points", "Long Dots",
                                                                                          "Short Dots", "Long Point", "Short Point", "Partial Point",
                                                                                          "No Book Point")},
                                 {k: v for k, v in w.items() if k not in entry_columns + ("Signal", "Long Points", "Short Points", "Long Dots",
                                                                                          "Short Dots", "Long Point", "Short Point", "Partial Point",
                                                                                          "No Book Point")}, value)
                if w["Signal"] and codes[int(w["Signal"])] in ("No Anchor", "Past T", "No Mean", "No Notional"):
                    self.assertEqual(a["Signal"], w["Signal"], value)
                elif w["Signal"]:
                    self.assertEqual(a["Signal"], str(board.CODE["No Entry"]), value)
                self.assertEqual((a["Long Dots"], a["Short Dots"]), ("", ""), value)
                if a["Phase"]:
                    self.assertEqual((a["Long Point"], a["Short Point"], a["Partial Point"], a["No Book Point"]), ("0", "0", "0", "0"), value)
            self.assertEqual({(r["Long Points"], r["Short Points"]) for r in got[spread]}, {("0", "0")}, value)
            self.assertEqual(got[last][0]["Signal"], "No Entry", value)
            self.assertEqual({k: v for k, v in got[last][0].items() if k != "Signal"}, {k: v for k, v in ok[last][0].items() if k != "Signal"}, value)
            self.assertEqual({r["Net at Entry"] for r in got[fees]}, {""}, value)
            self.assertTrue(all(r["Net at Entry"] for r in ok[fees]))
        # Mean Scale: "30min" is 30; anything else no mean, the rest as with a valid one.
        mean = runs["ma_minutes", "30"]
        self.assertEqual(runs["ma_minutes", "30min"], mean)
        meanless = ("Mean", "Mean + Entry", "Mean − Entry", "Deviation", "Mean · Tooltip", "Deviation · Tooltip", "Real Long Deviation · Tooltip",
                    "Real Short Deviation · Tooltip", "Signal", "Long Points", "Short Points", "Deviation · Line", "Real Long Deviation · Faded",
                    "Real Short Deviation · Faded", "Real Long Deviation", "Real Short Deviation", "Past T", "Long Dots", "Short Dots", "Phase",
                    "Long Point", "Short Point", "Partial Point", "Past T Point", "No Book Point", "Best Long", "Best Short", *SIGNED,
                    *[name for name, _ in board.SPREAD_SIGMAS])
        no_mean, no_anchor = str(board.CODE["No Mean"]), str(board.CODE["No Anchor"])
        for value in ("abc", "", "0", "30.5"):
            got = runs["ma_minutes", value]
            self.assertEqual(len(got[spread]), len(mean[spread]), value)
            for a, w in zip(got[spread], mean[spread]):
                self.assertEqual({k: v for k, v in a.items() if k not in meanless}, {k: v for k, v in w.items() if k not in meanless}, value)
                self.assertEqual({a[c] for c in ("Mean", "Signed Deviation", "Phase", "Best Long", "Best Short", "Real Long Deviation", "Long Dots",
                                                 *[name for name, _ in board.SPREAD_SIGMAS])}, {""}, value)
                if a["Spread · Tooltip"]:
                    self.assertIn(a["Mean · Tooltip"], (no_mean, no_anchor, str(board.CODE["Past T"])), value)
                    self.assertIn(codes[int(a["Signal"])], ("No Mean", "No Anchor", "Past T"), value)
            row = got[last][0]
            self.assertEqual((row["Mean"], row["Deviation"], row["Real Long Deviation"], row["Real Short Deviation"], row["Signal"]),
                             (no_mean, no_mean, no_mean, no_mean, "No Mean"), value)
            self.assertEqual((row["Spread"], row["Long Fill"], row["Bid · ct"]), (mean[last][0]["Spread"], mean[last][0]["Long Fill"],
                                                                                              mean[last][0]["Bid · ct"]), value)
            self.assertEqual(got[fees], mean[fees], value)

        # Entry as a multiple of sigma (3.5, owner item 64): "2σ" and "2sigma" read the same; every point's Entry (+Entry) is 2 ×
        # its Sigma as drawn (to the rounding of the two decimals); with no Sigma Scale the Signal reads No Sigma where it would
        # have had a side or None, and no Entry line is drawn.
        two = run_all(dict(base, d="2σ"))
        self.assertEqual(run_all(dict(base, d="2sigma")), two)
        checked_entries = 0
        for r in two[spread]:
            if r["Sigma"] and r["+Entry"]:
                self.assertLessEqual(abs(float(r["+Entry"]) - 2 * float(r["Sigma"])), 0.0101, r)
                checked_entries += 1
        self.assertGreater(checked_entries, 100)
        self.assertTrue(any(r["Signal"] in ("1", "2", "3", "4") for r in two[spread]))
        nosig = run_all(dict(base, d="2σ", sigma_minutes="abc"))
        self.assertEqual({r["+Entry"] for r in nosig[spread]}, {""})
        for a, w in zip(nosig[spread], two[spread]):
            if w["Signal"] in ("0", "1", "2", "3", "4", str(board.CODE["No Book"])):
                self.assertEqual(a["Signal"], str(board.CODE["No Sigma"]), a)
        self.assertIn(nosig[last][0]["Signal"], ("No Sigma", "No Mean", "No Anchor", "Past T"))
        # The sigma lines (3.5.1): k σ and Mean ± k σ from the Sigma and Mean as shown, to one decimal, and NULL wherever Sigma
        # (or, for Mean ± k σ, the Mean) is NULL: never 0 there (3.5 drew them at 0 through each warm-up and gap). With no
        # Sigma Scale every line is NULL at every row, the spread drawn.
        lines = dict(board.SPREAD_SIGMAS + board.DEVIATION_SIGMAS)
        checked = 0
        for r in two[spread]:
            for name, k in lines.items():
                base_v = 0.0 if name in dict(board.DEVIATION_SIGMAS) else (float(r["Mean"]) if r["Mean"] else None)
                if not r["Sigma"] or base_v is None:
                    self.assertEqual(r[name], "", (name, r["time"]))
                    continue
                sign = -1 if "−" in name else 1
                self.assertAlmostEqual(float(r[name]), base_v + sign * k * float(r["Sigma"]), delta=0.0501, msg=(name, r["time"]))
                checked += 1
        self.assertGreater(checked, 1000)
        self.assertTrue(any(r["Spread"] for r in nosig[spread]))
        for r in nosig[spread]:
            self.assertEqual({r[name] for name in lines}, {""}, r["time"])
        # Sigma Scale (3.4): "60min" is 60; anything else no sigma (Sigma empty, its tooltip and strip No Sigma where there is a
        # spread and no Past T), every other figure as with a valid one.
        sig = runs["sigma_minutes", "60"]
        self.assertEqual(runs["sigma_minutes", "60min"], sig)
        self.assertTrue(any(r["Sigma"] for r in sig[spread]))
        no_sigma = str(board.CODE["No Sigma"])
        sigmaless = ("Sigma", "Sigma · Tooltip", *[name for name, _ in board.SPREAD_SIGMAS + board.DEVIATION_SIGMAS])
        for value in ("abc", "", "0", "30.5"):
            got = runs["sigma_minutes", value]
            for name in sigmaless[:1] + sigmaless[2:]:
                self.assertEqual({r[name] for r in got[spread]}, {""}, (value, name))
            self.assertEqual({r["Sigma · Tooltip"] for r in got[spread]} - {"", no_sigma, no_anchor, str(board.CODE["Past T"])}, set(), value)
            for a, w in zip(got[spread], sig[spread]):
                self.assertEqual({k: v for k, v in a.items() if k not in sigmaless}, {k: v for k, v in w.items() if k not in sigmaless}, value)
            self.assertEqual(got[last][0]["Sigma"], no_sigma, value)
            self.assertEqual({k: v for k, v in got[last][0].items() if k != "Sigma"}, {k: v for k, v in sig[last][0].items() if k != "Sigma"}, value)

    def test_staging_is_generated(self):
        # The staged board is the generator's output, byte for byte (python3 dashboard/grafana/gen_dashboard.py writes it).
        with open(boardkit.board_file("combo-spread"), encoding="utf-8") as f:
            self.assertEqual(f.read(), boardkit.board_json("combo-spread", board.build()))

    def test_live_monitor_staging_is_generated(self):
        with open(boardkit.board_file("live-monitor"), encoding="utf-8") as f:
            self.assertEqual(f.read(), boardkit.board_json("live-monitor", live_monitor_board.build()))

    def test_price_panels_draw_the_anchor_in_usdt(self):
        # The drawn anchor is A / F; A itself, in USD, is in the legend and the tooltip only: no line, a hidden axis.
        b = board.build()
        prices = [p for p in b["panels"] if p["title"].endswith("Price · USDT")]
        self.assertEqual(len(prices), 3)
        sql_of = dict(board.PRICE_COLUMNS)
        for p, leg in zip(prices, ("bull", "bear", "ref")):
            self.assertEqual(fields(b, p), ["time", "Price", "Anchor (USDT)", "Anchor (USD)", "Return"], p["title"])
            name = leg.capitalize()
            self.assertIn("p.a_%s / p.fx" % leg, sql_of[name + " Anchor (USDT)"])
            self.assertIn("p.p_%s" % leg, sql_of[name + " Price"])
            props = props_of(p)
            self.assertEqual((props["Anchor (USD)"]["custom.lineWidth"], props["Anchor (USD)"]["custom.axisPlacement"]), (0, "hidden"), p["title"])
            self.assertIn("custom.lineStyle", props["Anchor (USDT)"], p["title"])
        self.assertEqual(fields(b, by_title(b, "Implied Return · %")), ["time", "Sum", "Reference", "Bull Leg", "Bear Leg"])

    def test_spec_describes_the_board(self):
        # SPEC.md names every panel, the price charts' series and the Anchors table's columns as the board shows them
        # (variables as <bull>, <cex> ...), and lists exactly the board's anchor statuses.
        with open(SPEC, encoding="utf-8") as f:
            spec = f.read()
        shown = {"${bull}": "<bull>", "${bear}": "<bear>", "${ref}": "<ref>", "${combo:text}": "<combo>", "${venue:text}": "<cex>",
                 "${ma:text} ${ma_minutes}": "<EMA|SMA> <n>", "${d}": "<d>", "${n_text}": "<N>"}

        def doc(name):
            for var, text in shown.items():
                name = name.replace(var, text)
            return name
        b = board.build()
        names = [p["title"] for p in b["panels"]] + fields(b, by_title(b, "${bull} Price · USDT"))[1:] + columns(board.Q_ANCHORS)
        self.assertEqual([doc(name) for name in names if doc(name) not in spec], [], "names missing from SPEC.md")
        # The status table under combo_sessions' paragraph, up to the next function's.
        statuses = re.split(r"\n\*\*`etfpair\.", spec.split("**`etfpair.combo_sessions(combo)`**")[1])[0]
        self.assertEqual(re.findall(r"^\| `(\w+)` \|", statuses, re.M), list(board.STATUS["options"]))

    @unittest.skipUnless(PSQL, "set ETFPAIR_BOARD_PSQL to run the queries")
    def test_anchors_table(self):
        # The columns of the design, the three statuses, and the anchor in use: the confirmed NAVs on a NAV basis (the row of each
        # leg confirmed last), the bells on a Bell basis; an action reads "<ticker> ex|split ... · <instant> · <status>".
        nav = {(r["ticker"], r["session_date"]): r["nav"] for r in csv.DictReader(io.StringIO(psql(
            "SELECT DISTINCT ON (ticker, session_date) ticker, session_date, nav FROM etfpair.etf_nav ORDER BY ticker, session_date, confirmed_ms DESC",
            "--csv")))}
        for sel in (dict(SOXL, venue="binance"), dict(TQQQ, venue="bitget")):
            table = rows_of(substitute(board.Q_ANCHORS, dict(sel, hours=1), int(time.time() * 1000)))
            b, s, r = sel["bull"], sel["bear"], sel["ref"]
            self.assertEqual(list(table[0]), ["Session", "Close Time", b + " Bell", s + " Bell", r + " Bell", b + " NAV", s + " NAV",
                                              b + " Anchor", s + " Anchor", "Known Time", "Status", "Actions"])
            for row in table:
                self.assertIn(row["Status"], ("NAV", "Bell", "Missing"), row)
                if row["Status"] == "Bell":
                    self.assertEqual((row[b + " Anchor"], row[s + " Anchor"]), (row[b + " Bell"], row[s + " Bell"]), row)
                if row["Status"] == "NAV":
                    self.assertEqual((row[b + " Anchor"], row[s + " Anchor"]), (nav[(b, row["Session"])], nav[(s, row["Session"])]), row)
                if row["Status"] != "Missing":
                    self.assertTrue(row["Known Time"], row)
                for action in filter(None, row["Actions"].split("; ")):
                    self.assertRegex(action, r"^(%s|%s|%s) (ex [0-9.?]+|split [0-9.]+:[0-9.]+)( · \d\d-\d\d \d\d:\d\d)? · "
                                             r"(Scheduled|Known|None|Applied|Verified|Mismatch|Scheduled · Past T)( · known \d\d-\d\d \d\d:\d\d)?$" % (b, s, r))

    @unittest.skipUnless(PSQL, "set ETFPAIR_BOARD_PSQL to run the queries")
    def test_action_marks(self):
        # Each instant at which actions take effect on a venue is marked once on the Spread chart for the actions that leave the
        # anchors (a dividend past T without its amount, a date only; not None), and once on every chart, as an Anchor Changes
        # mark, for the Known ones (3.5): its text names each of them after the reason.
        instants = {}
        for combo, venue, at, ticker, status, leg in [r.split("|") for r in psql(
                "SELECT c.combo_id, m.venue, a.effective_ms, a.ticker, a.status, a.leg FROM etfpair.combo c JOIN etfpair.combo_market m USING (combo_id), "
                "LATERAL etfpair.combo_actions(c.combo_id, m.venue) a WHERE a.effective_ms IS NOT NULL", "-At").split()]:
            kinds = instants.setdefault((combo, venue, at), ([], []))
            if status == "Known":
                kinds[1].append(ticker)
            elif status != "None":
                kinds[0].append(ticker)
        self.assertTrue(any(legs for _, legs in instants.values()))
        for (combo, venue, at), (others, legs) in instants.items():
            sel = dict(SOXL if combo == "SOXL-SOXS" else TQQQ, venue=venue, ma="sma", ma_minutes="60", d="10", hours=2)
            for q, tickers in ((board.Q_MARKS, others), (board.Q_ANCHOR_CHANGES, legs)):
                texts = [m[1] for m in (r.split("|", 1) for r in psql(substitute(q, sel, int(at) + 3600_000), "-At").splitlines()) if m[0] == at]
                self.assertEqual(len(texts), 1 if tickers else 0, (combo, venue, at, texts))
                for ticker in tickers:
                    self.assertIn(ticker + " ", texts[0])
                if q is board.Q_ANCHOR_CHANGES and tickers:
                    self.assertTrue(texts[0].startswith("Anchor change · a dividend or split takes effect"), texts[0])

    @unittest.skipUnless(PSQL, "set ETFPAIR_BOARD_PSQL to run the queries")
    def test_anchor_change_marks(self):
        # 3.5 (owner item 62): on every chart, a mark at each change of the legs' anchor in use: a session's close, a leg's Known
        # action, both legs' NAV confirmed (within four days of the close); each text the reason, then each leg's anchor before ->
        # after in USD and in USDT (A ÷ F) and the F, on lines of their own (<br>). Recomputed here from combo_sessions,
        # combo_actions and fx_minute for the last 30 days of SOXL / SOXS on Bitget.
        b = board.build()
        ann = next(a for a in b["annotations"]["list"] if a["name"] == "Anchor Changes")
        self.assertEqual(sorted(ann["filter"]["ids"]), sorted(p["id"] for p in b["panels"] if p["type"] == "timeseries"))
        self.assertEqual(len(ann["filter"]["ids"]), 7)
        now_ms = newest_ms()
        sel = dict(SOXL, venue="bitget", ma="sma", ma_minutes="60", d="10", hours=24 * 30)
        rows = [r.split("|", 1) for r in psql(substitute(board.Q_ANCHOR_CHANGES, sel, now_ms, now_ms), "-At").splitlines()]
        self.assertTrue(rows)
        sessions = {r["session_date"]: r for r in rows_of("SELECT * FROM etfpair.combo_sessions('SOXL-SOXS') WHERE close_ms <= %d" % now_ms)}
        closes = {int(r["close_ms"]): r for r in sessions.values() if int(r["close_ms"]) > now_ms - 30 * 86400_000}
        navs = {int(r["known_ms"]) for r in sessions.values() if r["basis"] == "NAV" and int(r["close_ms"]) < int(r["known_ms"]) < int(r["close_ms"]) + 4 * 86400_000
                and int(r["known_ms"]) > now_ms - 30 * 86400_000}
        got = {}
        for t, text in rows:
            got.setdefault(int(t), []).append(text)
            parts = text.split("<br>")
            if not text.startswith("Anchor change · NAV"):
                self.assertRegex(parts.pop(-2), r"^SOXX [0-9.,–]+ → [0-9.,–]+ USD · [0-9.,–]+ → [0-9.,–]+ USDT \(reference, bell only\)$")
            self.assertRegex(parts[-3], r"^SOXL [0-9.,–]+ → [0-9.,–]+ USD · [0-9.,–]+ → [0-9.,–]+ USDT$")
            self.assertRegex(parts[-2], r"^SOXS [0-9.,–]+ → [0-9.,–]+ USD · [0-9.,–]+ → [0-9.,–]+ USDT$")
            self.assertRegex(parts[-1], r"^USDT = USD ÷ F, F = [0-9.]+ USD/USDT of that minute$")
        # Every close in the range and every NAV confirmation is marked, and nothing else but the legs' actions.
        self.assertTrue(set(closes) <= set(got), sorted(set(closes) - set(got)))
        self.assertTrue(navs <= set(got), sorted(navs - set(got)))
        for t, texts in got.items():
            for text in texts:
                if t in closes and "new session" in text:
                    r = closes[t]
                    fx = float(psql("SELECT price FROM etfpair.fx_minute WHERE pair = 'USDT-USD' AND minute_ms <= %d AND minute_ms >= %d - 300000 "
                                    "ORDER BY minute_ms DESC LIMIT 1" % (t, t), "-At"))
                    bull = text.split("<br>")[-4]
                    self.assertEqual(bull.split(" → ")[1].split(" ")[0], "%.2f" % float(r["a_bull"]), text)
                    self.assertIn("· %s → %s USDT" % (bull.split("· ")[1].split(" → ")[0], "{:,.2f}".format(float(r["a_bull"]) / fx)), text)
                    self.assertIn("F = %s " % ("%g" % fx), text.replace("F = %s " % text.split("F = ")[1].split(" ")[0], "F = %g " % float(text.split("F = ")[1].split(" ")[0])))
                elif t in navs:
                    self.assertRegex(text, r"^Anchor change · NAV (confirmed for|of) session")
                else:
                    self.assertRegex(text, r"^Anchor change · a dividend or split takes effect")

    @unittest.skipUnless(PSQL, "set ETFPAIR_BOARD_PSQL to run the queries")
    def test_mark_texts(self):
        # The marks' text on made-up actions in combo_actions' shape: an amount not yet known after T (no expected step) next to a
        # Known one at the same instant, one mark for both; a date only, beside a None that has no mark; a split; a Known
        # dividend of a session without anchors (no expected step) and a checked one.
        made_up = ("(VALUES (1000, 'SOXL', 'dividend', NULL::numeric, 'Scheduled', NULL::numeric, NULL::numeric, NULL::numeric, 'Scheduled · Past T', 'bull'), "
                   "(1000, 'SOXS', 'dividend', 0.5, 'Known', -79.24, NULL, NULL, 'Applied', 'bear'), "
                   "(2000, 'TQQQ', 'dividend', NULL, 'None', NULL, NULL, NULL, 'None', 'bull'), "
                   "(2000, 'SQQQ', 'dividend', NULL, 'Scheduled', NULL, NULL, NULL, 'Scheduled', 'bear'), "
                   "(3000, 'SOXX', 'split', 3, 'Known', -1.0986, -1.0996, NULL, 'Verified', 'ref'), "
                   "(4000, 'SOXL', 'dividend', 0.0929, 'Known', NULL, NULL, NULL, 'Applied', 'bull'), "
                   "(4000, 'SOXS', 'dividend', 0.43641, 'Known', -130.13, -96.49, 2.15, 'Verified', 'bear')) "
                   "a(effective_ms, ticker, kind, value, status, expected, observed, nav_bps, shown, leg)")
        # Each action's words (MARK), as both kinds of mark write them.
        words = boardkit.sql("SELECT a.effective_ms, string_agg(@MARK@, '; ' ORDER BY a.ticker, a.kind) FROM @A@ WHERE a.status <> 'None' "
                             "GROUP BY 1 ORDER BY 1", MARK=board.MARK, ACTION=board.ACTION, A=made_up)
        self.assertEqual(psql(words, "-At").splitlines(), [
            "1000|SOXL ex ? · Scheduled · Past T; SOXS ex 0.5 · expected -79.2 bps · measured – · Applied",
            "2000|SQQQ ex ? · Scheduled",
            "3000|SOXX split 3:1 · expected -1.0986 log · measured -1.0996 log · Verified",
            "4000|SOXL ex 0.0929 · measured – · Applied; SOXS ex 0.43641 · expected -130.1 bps · measured -96.5 bps · NAV check 2.2 bps · Verified"])
        # The Corporate Actions marks (Spread chart) leave out the Known actions: those are Anchor Changes marks (3.5).
        call = "etfpair.combo_actions('$combo', '$venue') a"
        self.assertIn(call, board.Q_MARKS)
        q = substitute(board.Q_MARKS.replace(call, made_up), dict(SOXL, venue="binance", hours=1), 10_000)
        self.assertEqual(psql("SELECT * FROM (%s) m ORDER BY 1" % q, "-At").splitlines(), [
            "1000|SOXL ex ? · Scheduled · Past T",
            "2000|SQQQ ex ? · Scheduled"])

    @unittest.skipUnless(PSQL, "set ETFPAIR_BOARD_PSQL to run the queries")
    def test_calendar_texts(self):
        # The Calendar figure and the Calendar Check table on made-up checks: agreeing through the schedule's last day; two dates
        # that differ, named in the figure, the detail in the table; a schedule that could not be compared.
        made_up = {
            "ok": "(VALUES (1795000000000, 1794445200000::bigint, 1795654800000::bigint, true, 'OK'))",
            "differ": "(VALUES (1795000000000, 1794445200000::bigint, 1795654800000::bigint, false, '2026-11-27: close 11-28 02:00 here, 11-28 05:00 on "
                      "Binance; 2026-11-30: a trading day here (close 12-01 05:00), not on Binance'))",
            "not": "(VALUES (1795000000000, NULL::bigint, NULL::bigint, false, 'not compared: unknown session type \"X\"'))"}
        cols = " c(checked_ms, window_from_ms, window_to_ms, agree, detail)"
        calendar = board.Q_PHASES.split(",\n       ")[-1].replace(' AS "Calendar"', "")
        self.assertIn("FROM etfpair.calendar_check", calendar)
        got = {}
        for name, rows in made_up.items():
            figure = psql("SELECT " + calendar.replace("FROM etfpair.calendar_check", "FROM " + rows + cols), "-At").strip()
            table = psql(board.Q_CALENDAR_CHECK.replace("FROM etfpair.calendar_check", "FROM " + rows + cols), "-At").strip()
            got[name] = (figure, table.split("|")[1:])
        # The figure's spaces are no-break spaces (one line in a narrow window), the table's plain ones.
        nb = "\u00a0"
        self.assertEqual(got["ok"], ("OK%sto%s11-25" % (nb, nb), ["11-25", "OK", "OK"]))
        self.assertEqual(got["differ"][0], "Differ%s·%s11-27%s+1" % (nb, nb, nb))
        self.assertEqual(got["differ"][1][1], "Differ")
        self.assertEqual(got["not"], ("Not%scompared" % nb, ["", "Not compared", 'not compared: unknown session type "X"']))

    @unittest.skipUnless(PSQL, "set ETFPAIR_BOARD_PSQL to run the queries")
    def test_codes_on_made_up_points(self):
        # Made-up points in combo_range's shape (signed figures, as the function returns them) through the Spread row's query: the
        # tooltip rows carry each value, absolute for the three deviations and rounded once, or the code of the reason there is
        # none, in DESIGN 1.9's order; the Deviation chart's lines leave Past T to the amber line and dot the Long and Short
        # seconds; the counts run; Entry Points' columns mark the points with a phase. A break row stays NULL but for the counts
        # and Entry.
        cols = ("ts_ms, session_date, phase, phase_sort, basis, past_t, s_bps, mu_bps, sigma_bps, s_long_bps, s_short_bps, partial_long, "
                "partial_short, book_bull, book_bear, bid_ct_bull, bid_ct_bear, ask_ct_bull, ask_ct_bear, bid_usdt_bull, bid_usdt_bear, ask_usdt_bull, "
                "ask_usdt_bear, a_ok, signal")
        book = "true, true, 100.5, 20.25, 90.0, 30.0, 4040.1, 1210.5, 3600.4, 1800.0"
        points = [
            "(1000::bigint, NULL::date, 'Overnight', 4, 'mid', true, -120.04, -53.0, 2.5, -119.0, -121.0, false, false, %s, true, 'Past T')" % book,
            "(2000, NULL, 'Overnight', 4, 'mid', false, -120.04, -53.0, 2.5, -119.0, -121.0, false, false, %s, true, 'Long')" % book,
            "(3000, NULL, NULL, NULL, 'mid', true, 5.0, NULL, NULL, 6.0, 4.0, false, false, %s, true, 'Past T')" % book,
            "(4000, NULL, NULL, NULL, 'mid', false, 5.0, NULL, NULL, 6.0, 4.0, false, false, %s, true, 'No Mean')" % book,
            "(5000, NULL, 'Regular', 2, 'trade', false, -130.0, -60.0, 2.5, NULL, NULL, NULL, NULL, true, false, 50.0, NULL, NULL, NULL, 2010.0, NULL, NULL, NULL, true, 'No Book')",
            "(6000, NULL, 'Regular', 2, 'mid', false, -70.0, -55.0, 2.5, -66.0, -75.0, true, false, %s, true, 'Long · Partial')" % book,
            "(7000, NULL, 'Regular', 2, 'mid', false, 20.0, 0.0, 2.5, 25.0, 12.0, false, true, %s, true, 'Short · Partial')" % book,
            "(8000, NULL, 'Regular', 2, 'mid', false, 15.0, 0.0, 2.5, 16.0, 11.0, false, false, %s, true, 'Short')" % book,
            "(9000, NULL, 'Regular', 2, 'mid', false, 10.0, 0.0, 2.5, 12.0, 8.0, false, false, %s, true, 'None')" % book,
            "(10000, NULL, NULL, NULL, 'mid', false, NULL, NULL, NULL, NULL, NULL, NULL, NULL, %s, false, 'No Anchor')" % book,
            "(11000, NULL, 'Regular', 2, 'mid', false, 1.0, 0.0, 2.5, 3.0, -2.0, false, false, true, true, 7.0, NULL, 8.0, NULL, 280.0, NULL, 320.0, NULL, true, 'None')",
            "(11001, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL)",
            "(12000, NULL, 'Regular', 2, 'mid', false, -5.0, 0.0, 2.5, -3.0, -8.0, false, false, %s, true, 'None')" % book,
            "(13000, NULL, 'Regular', 2, 'mid', false, 2.0, 0.0, 2.5, 4.0, -1.0, false, false, %s, true, 'No Entry')" % book]
        made_up = "(VALUES %s) x(%s)" % (", ".join(points), cols)
        self.assertIn(board.RANGE_CALL, board.Q_SPREAD)
        sel = dict(SOXL, venue="binance", ma="sma", ma_minutes="60", d="10", notional="10000", hours=1)
        q = substitute(board.Q_SPREAD.replace(board.RANGE_CALL, made_up), sel, 20_000).replace("to_timestamp(r.ts_ms / 1000.0) AS time", "r.ts_ms AS time")
        got = list(csv.reader(io.StringIO(psql(q, "--csv"))))
        header = got[0]
        self.assertEqual(header, columns(board.Q_SPREAD))
        table = {int(r[0]): dict(zip(header, r)) for r in got[1:]}
        nb, nn, na, pt, nm, nr, ne, ns = (str(board.CODE[t]) for t in ("No Book", "No Notional", "No Anchor", "Past T", "No Mean", "Few Levels",
                                                                         "No Entry", "No Sigma"))
        spread_rows = ["Spread · Tooltip", "Real Long Spread", "Real Short Spread", "Mean · Tooltip", "Deviation · Tooltip",
                       "Real Long Deviation · Tooltip", "Real Short Deviation · Tooltip", "Signal"] + [name for name, _, _, _ in board.DEPTHS]
        depths = ["100.5", "20.25", "90", "30", "4040", "1211", "3600", "1800"]
        want = {
            1000: ["-120", "-119", "-121", "-53", "67", pt, pt, pt] + depths,
            2000: ["-120", "-119", "-121", "-53", "67", "66", "68", "1"] + depths,
            3000: ["5", "6", "4", pt, pt, pt, pt, pt] + depths,
            4000: ["5", "6", "4", nm, nm, nm, nm, nm] + depths,
            5000: ["-130", nb, nb, "-60", "70", nb, nb, nb, "50", nb, nr, nb, "2010", nb, nr, nb],
            6000: ["-70", "-66", "-75", "-55", "15", "11", "20", "3"] + depths,
            7000: ["20", "25", "12", "0", "20", "25", "12", "4"] + depths,
            8000: ["15", "16", "11", "0", "15", "16", "11", "2"] + depths,
            9000: ["10", "12", "8", "0", "10", "12", "8", "0"] + depths,
            10000: [na, na, na, na, na, na, na, na] + depths,
            11000: ["1", "3", "-2", "0", "1", "3", "2", "0", "7", nr, "8", nr, "280", nr, "320", nr],
            12000: ["-5", "-3", "-8", "0", "5", "3", "8", "0"] + depths,
            13000: ["2", "4", "-1", "0", "2", "4", "1", "0"] + depths}
        self.assertEqual({t: [table[t][c] for c in spread_rows] for t in want}, want)
        # 3.5 (owner item 64): the Signal is the board's, on each point's Entry (combo_range's own Signal column is not read: the
        # made-up 13000 says No Entry, the board None). Entry 2σ: each point's Entry is 2 × its sigma of 2.5, 5 bps: 9000's
        # Real Short Spread 8 above the Mean of 0 is then a Short, 12000's Real Long Spread −3 still none; ±Entry 5.
        sig2 = {int(r["time"]): r for r in rows_of(substitute(board.Q_SPREAD.replace(board.RANGE_CALL, made_up), dict(sel, d="2σ"), 20_000)
                                                    .replace("to_timestamp(r.ts_ms / 1000.0) AS time", "r.ts_ms AS time"))}
        self.assertEqual([(sig2[t]["Signal"], sig2[t]["+Entry"], sig2[t]["−Entry"]) for t in (9000, 12000, 2000)],
                         [("2", "5", "-5"), ("0", "5", "-5"), ("1", "5", "-5")])
        # 3.4: Sigma, the drawn column and the tooltip's (its value, else No Anchor, Past T, No Sigma).
        self.assertEqual({t: (table[t]["Sigma"], table[t]["Sigma · Tooltip"]) for t in (1000, 3000, 4000, 5000, 10000, 11001)},
                         {1000: ("2.5", "2.5"), 3000: ("", pt), 4000: ("", ns), 5000: ("2.5", "2.5"), 10000: ("", na), 11001: ("", "")})
        # Rounded once to the tooltip's decimals: -120.04 shows as -120.0, 67.04 as 67.0.
        self.assertEqual((table[2000]["Spread"], table[2000]["Deviation · Line"], table[2000]["Signed Deviation"]), ("-120", "67", "-67"))
        # Each real line faded at every point but past T, bright only where its real spread lies on the side it fills on (Real Long
        # Spread below the Mean, Real Short Spread above it): 9000's Real Long Deviation of 12 is past Entry on the wrong side, faded.
        lines = ["Long Points", "Short Points", "Deviation · Line", "Real Long Deviation · Faded", "Real Short Deviation · Faded", "Real Long Deviation",
                 "Real Short Deviation", "Past T", "+Entry", "Long Dots", "Short Dots"]
        self.assertEqual({t: [table[t][c] for c in lines] for t in (1000, 2000, 5000, 6000, 7000, 9000, 11000, 11001, 12000, 13000)}, {
            1000: ["0", "0", "", "", "", "", "", "67", "10", "", ""],
            2000: ["1", "0", "67", "66", "68", "66", "", "", "10", "66", ""],
            5000: ["1", "0", "70", "", "", "", "", "", "10", "", ""],
            6000: ["2", "0", "15", "11", "20", "11", "", "", "10", "11", ""],
            7000: ["2", "1", "20", "25", "12", "", "12", "", "10", "", "12"],
            9000: ["2", "2", "10", "12", "8", "", "8", "", "10", "", ""],
            11000: ["2", "2", "1", "3", "2", "", "", "", "10", "", ""],
            11001: ["2", "2", "", "", "", "", "", "", "10", "", ""],
            12000: ["2", "2", "5", "3", "8", "3", "", "", "10", "", ""],
            13000: ["2", "2", "2", "4", "1", "", "", "", "10", "", ""]})
        # The Deviation chart's columns with their signs: drawn but past T (the amber Past T line then), the real lines on both
        # sides of the Mean; the tooltip's twins the signed value or the reason, as the Spread chart's absolute ones.
        self.assertEqual({t: [table[t][c] for c in SIGNED] for t in want if t != 8000}, {
            1000: ["", "", "", "-67", "-10", "-67", pt, pt],
            2000: ["-67", "-66", "-68", "", "-10", "-67", "-66", "-68"],
            3000: ["", "", "", "", "-10", pt, pt, pt],
            4000: ["", "", "", "", "-10", nm, nm, nm],
            5000: ["-70", "", "", "", "-10", "-70", nb, nb],
            6000: ["-15", "-11", "-20", "", "-10", "-15", "-11", "-20"],
            7000: ["20", "25", "12", "", "-10", "20", "25", "12"],
            9000: ["10", "12", "8", "", "-10", "10", "12", "8"],
            10000: ["", "", "", "", "-10", na, na, na],
            11000: ["1", "3", "-2", "", "-10", "1", "3", "-2"],
            12000: ["-5", "-3", "-8", "", "-10", "-5", "-3", "-8"],
            13000: ["2", "4", "-1", "", "-10", "2", "4", "-1"]})
        entry = ["Phase", "Long Point", "Short Point", "Partial Point", "Past T Point", "No Book Point", "Best Long", "Best Short"]
        self.assertEqual({t: [table[t][c] for c in entry] for t in (1000, 2000, 3000, 5000, 6000, 7000, 9000, 11001, 12000, 13000)}, {
            1000: ["4", "0", "0", "0", "1", "0", "", ""],
            2000: ["4", "1", "0", "0", "0", "0", "66", ""],
            3000: ["", "", "", "", "", "", "", ""],
            5000: ["2", "0", "0", "0", "0", "1", "", ""],
            6000: ["2", "1", "0", "1", "0", "0", "11", ""],
            7000: ["2", "0", "1", "1", "0", "0", "", "12"],
            9000: ["2", "0", "0", "0", "0", "0", "", "8"],
            11001: ["", "", "", "", "", "", "", ""],
            12000: ["2", "0", "0", "0", "0", "0", "3", ""],
            13000: ["2", "0", "0", "0", "0", "0", "", ""]})
        self.assertEqual({k: v for k, v in table[11001].items() if v and k not in ("time", "Long Points", "Short Points", "+Entry", "−Entry")}, {})
        # The bright lines are the Best of Entry Points, point by point (the legend's Max is the table's largest Best).
        for t, r in table.items():
            self.assertEqual((r["Real Long Deviation"], r["Real Short Deviation"]), (r["Best Long"], r["Best Short"]), t)
        # A typed notional that is not a positive number: the real spreads (NULL then in combo_range) and the real deviations
        # read No Notional, after No Anchor, Past T and No Mean; so does the Signal (the board's, 3.5).
        q = substitute(board.Q_SPREAD.replace(board.RANGE_CALL, made_up.replace("-119.0, -121.0", "NULL, NULL")), dict(sel, notional="abc"), 20_000)
        row = rows_of(q.replace("to_timestamp(r.ts_ms / 1000.0) AS time", "r.ts_ms AS time"))[1]
        self.assertEqual([row[c] for c in spread_rows[:8]], ["-120", nn, nn, "-53", "67", nn, nn, nn])
        # An Entry that is not a positive number: no Mean ± Entry and no +Entry line, the Signal No Entry where it had a side or
        # None (no Long or Short point, no dot); every other column as with Entry 10.
        signal_cols = ("Signal", "Long Points", "Short Points", "Long Dots", "Short Dots", "Long Point", "Short Point", "Partial Point", "No Book Point")
        for d in ("abc", "", "0", "-5", "1'; SELECT 1; --"):
            q = substitute(board.Q_SPREAD.replace(board.RANGE_CALL, made_up), dict(sel, d=d), 20_000)
            got = {int(r["time"]): r for r in rows_of(q.replace("to_timestamp(r.ts_ms / 1000.0) AS time", "r.ts_ms AS time"))}
            band = ("Mean + Entry", "Mean − Entry", "+Entry", "−Entry")
            self.assertEqual({r[c] for r in got.values() for c in band}, {""}, d)
            self.assertEqual({t: {c: v for c, v in r.items() if c not in band + signal_cols} for t, r in got.items()},
                             {t: {c: v for c, v in r.items() if c not in band + signal_cols} for t, r in table.items()}, d)
            for t, r in got.items():
                if table[t]["Signal"] in ("0", "1", "2", "3", "4", nb):  # No Entry comes before No Book
                    self.assertEqual(r["Signal"], ne, (d, t))
                else:
                    self.assertEqual(r["Signal"], table[t]["Signal"], (d, t))
                self.assertEqual((r["Long Dots"], r["Short Dots"]), ("", ""), (d, t))
        self.assertEqual((table[2000]["Mean + Entry"], table[2000]["Mean − Entry"], table[2000]["+Entry"], table[2000]["−Entry"]), ("-43", "-63", "10", "-10"))
        # The strips' query on a made-up pair of newest seconds (combo_last's shape): the anchors and the reference's return
        # rounded once; the basis second's absolute deviations, a real one only on the side its real spread can fill on (Real
        # Short Spread here below the Mean: Other Side), a capped side's fill in USDT and Full for the other, the Signal's text and
        # each leg's depths or their reasons. The Signal is the board's on Entry 10 (3.5): Real Long Spread 9.59 under the Mean is
        # None. Without those seconds every figure is empty (–) but the Phase.
        last_cols = ("kind, ts_ms, session_date, basis, past_t, fx, a_bull, a_bear, a_ref, p_bull, p_bear, p_ref, s_bps, mu_bps, sigma_bps, s_long_bps, "
                     "s_short_bps, partial_long, partial_short, filled_long, filled_short, book_bull, book_bear, bid_ct_bull, bid_ct_bear, ask_ct_bull, "
                     "ask_ct_bear, bid_usdt_bull, bid_usdt_bear, ask_usdt_bull, ask_usdt_bear, anchor_status, signal")
        last = ("(VALUES ('prices'::text, 1000::bigint, NULL::date, NULL::text, NULL::boolean, 1.0::numeric, 30.756, 83.5051, 123.18, 33.74, 75.599, "
                "127.56, NULL::numeric, NULL::numeric, NULL::numeric, NULL::numeric, NULL::numeric, NULL::boolean, NULL::boolean, NULL::numeric, "
                "NULL::numeric, NULL::boolean, NULL::boolean, NULL::numeric, NULL::numeric, NULL::numeric, NULL::numeric, NULL::numeric, NULL::numeric, "
                "NULL::numeric, NULL::numeric, NULL::text, NULL::text), "
                "('basis', 2000, NULL, 'mid', false, 0.998525, 30.756, 83.505, 123.18, 33.735, 75.604, NULL, -7.364, -2.408, 3.4567, -12.0, -9.9, true, "
                "false, 6234.4, 10000.0000000000000002, true, true, 2415.33, NULL, 2094.72, 54.93, 81481.7, NULL, 70653.6, 4152.65, 'Bell', "
                "'Long · Partial')) "
                "x(%s)") % last_cols
        self.assertIn("CROSS JOIN LATERAL " + board.LAST, board.Q_LAST)
        rows = rows_of(substitute(board.Q_LAST.replace(board.LAST, last), sel, 20_000))
        row = rows[0]
        other = str(board.CODE["Other Side"])
        book = ("Leg", "Tick", "Levels", "Bid · ct", "Bid · USDT", "Ask · ct", "Ask · USDT")
        self.assertEqual({k: v for k, v in row.items() if k not in book},
                         {"Phase": "", "Bull Price": "33.74", "Bull Anchor": "30.76", "Bear Price": "75.599", "Bear Anchor": "83.51",
                          "Ref Price": "127.56", "Ref Return": "3.56", "Spread": "-7.36", "Mean": "-2.41", "Sigma": "3.46", "Deviation": "4.96",
                          "Anchor Status": "Bell", "USD/USDT": "0.998525", "Real Long Deviation": "9.59", "Real Short Deviation": other,
                          "Long Fill": "6234", "Short Fill": str(board.FULL), "Signal": "None"})
        # The Book table's two rows (sel is Binance SOXL / SOXS: both ticks 0.01, levels 1-2); the strips' figures only in row 1.
        self.assertEqual([[r[c] for c in book] for r in rows], [["SOXL", "0.01", "1–2", "2415.33", "81482", "2094.72", "70654"],
                                                                 ["SOXS", "0.01", "1–2", nr, nr, "54.93", "4153"]])
        self.assertEqual({k: v for k, v in rows[1].items() if v and k not in book}, {})
        # The basis second's past_t, Spread, Real Long Spread and Real Short Spread changed (the Mean stays -2.408): both real
        # spreads above the Mean (Real Long Deviation on the other side, Real Short Deviation |3.1 + 2.408|); both at the Mean
        # (neither side); both below it; past T (Past T on both, whatever the side).
        basis = "'mid', false, 0.998525, 30.756, 83.505, 123.18, 33.735, 75.604, NULL, -7.364, -2.408, 3.4567, -12.0, -9.9,"
        self.assertIn(basis, last)
        for past_t, s, s_long, s_short, want in (("false", 1.6, 5.0, 3.1, (other, "5.51")), ("false", -2.408, -2.408, -2.408, (other, other)),
                                                 ("false", -12.0, -12.0, -12.0, ("9.59", other)), ("true", -7.364, -12.0, -9.9, (pt, pt))):
            changed = "'mid', %s, 0.998525, 30.756, 83.505, 123.18, 33.735, 75.604, NULL, %s, -2.408, 3.4567, %s, %s," % (past_t, s, s_long, s_short)
            row = rows_of(substitute(board.Q_LAST.replace(board.LAST, last.replace(basis, changed)), sel, 20_000))[0]
            self.assertEqual((row["Real Long Deviation"], row["Real Short Deviation"]), want, changed)
        row = rows_of(substitute(board.Q_LAST.replace(board.LAST, "(SELECT * FROM %s WHERE false) x" % last.replace(") x(", ") l(")), sel, 20_000))[0]
        self.assertEqual({k: v for k, v in row.items() if v and k not in ("Leg", "Tick", "Levels")}, {})

    @unittest.skipUnless(PSQL, "set ETFPAIR_BOARD_PSQL to run the queries")
    def test_steps(self):
        # The spacing of the drawn points: the finest of 1, 5, 10, 30 s and whole minutes with at most about 2000 points a line
        # (3.5, owner item 63; 5000 until 3.4): every second to 33 min, 5 s to 2 h 46 min, 10 s to 5 h 33 min, 30 s to 16 h 40 min.
        got = {}
        for minutes in (30, 33, 34, 60, 166, 167, 333, 334, 360, 1000, 1001, 1440, 10080):
            got[minutes] = int(psql("SELECT " + substitute(board.STEP_MS, dict(SOXL, hours=minutes / 60), 10 ** 12), "-At"))
        self.assertEqual(got, {30: 1000, 33: 1000, 34: 5000, 60: 5000, 166: 5000, 167: 10000, 333: 10000, 334: 30000, 360: 30000, 1000: 30000,
                               1001: 60000, 1440: 60000, 10080: 360000})

    @unittest.skipUnless(PSQL, "set ETFPAIR_BOARD_PSQL to run the queries")
    def test_next_actions(self):
        # One row per ETF of the combo, bull, bear, reference: the newest reading of its next action whose ex-date (New York) has not
        # passed, as etf_action holds it; a dividend takes effect at 20:00 New York of the day before the ex-date.
        for sel in (SOXL, TQQQ):
            table = rows_of(substitute(board.Q_NEXT_ACTIONS, dict(sel, hours=1), int(time.time() * 1000)))
            self.assertEqual([r["ETF"] for r in table], [sel["bull"], sel["bear"], sel["ref"]])
            self.assertEqual(list(table[0]), ["ETF", "Ex-Date", "Action", "Takes Effect", "Status", "Source"])
            for r in table:
                want = psql("SELECT to_char(ex_date, 'YYYY-MM-DD') || '|' || status || '|' || source || '|' || kind || '|' || "
                            "to_char(((ex_date - 1) + time '20:00') AT TIME ZONE 'America/New_York' AT TIME ZONE 'Etc/GMT-8', 'MM-DD HH24:MI') "
                            "FROM (SELECT DISTINCT ON (ex_date, kind) * FROM etfpair.etf_action WHERE ticker = '%s' "
                            "AND ex_date >= (now() AT TIME ZONE 'America/New_York')::date ORDER BY ex_date, kind, known_ms DESC) a "
                            "ORDER BY ex_date, kind LIMIT 1" % r["ETF"], "-At").strip()
                if not want:
                    self.assertEqual((r["Ex-Date"], r["Status"]), ("", ""), r)
                    continue
                ex, status, source, kind, at = want.split("|")
                self.assertEqual((r["Ex-Date"], r["Status"].split(" · ")[0], r["Source"]), (ex, status, source), r)
                self.assertEqual(r["Takes Effect"], "after its halt" if kind == "split" else "" if status == "None" else at, r)
                self.assertRegex(r["Action"], r"^(ex [0-9.?]+|split [0-9.]+:[0-9.]+)$", r)

    @unittest.skipUnless(PSQL, "set ETFPAIR_BOARD_PSQL to run the queries")
    def test_every_query_runs(self):
        b = board.build()
        now_ms = newest_ms()
        for sel in SELECTIONS:
            for title, q in queries(b):
                started = time.time()
                run = subprocess.run(["psql", "-X", *shlex.split(PSQL), "-v", "ON_ERROR_STOP=1", "-At", "-c", substitute(q, sel, now_ms)],
                                     capture_output=True, text=True)
                self.assertEqual(run.returncode, 0, "%s with %s:\n%s" % (title, sel, run.stderr))
                self.assertLess(time.time() - started, 5, "%s with %s is slow" % (title, sel))

    @unittest.skipUnless(PSQL, "set ETFPAIR_BOARD_PSQL to run the queries")
    def test_latest_figures_with_a_range_into_the_future(self):
        # "Today" and "This week" end hours after now: the latest figures are still the newest ones stored.
        sel = dict(SELECTIONS[0], hours=24, ahead=6)
        row = rows_of(substitute(board.Q_LAST, sel, newest_ms()))[0]
        self.assertTrue(row["Bull Price"] and row["Bear Price"] and row["Spread"], "no latest figures: %r" % row)

    @unittest.skipUnless(PSQL, "set ETFPAIR_BOARD_PSQL to run the queries")
    def test_every_line_breaks_at_a_session_start(self):
        # Between the last point at or before each close of the range and the first point after it, a row without values (the
        # lines break there), in the Spread row's frame and in the Prices row's. Every such row is 1 ms after a point and none
        # opens the range: Grafana's tooltip over the row names its second, and shows the values of the point nearest the
        # cursor, which is then the point of that second. The range ends an hour after the newest close at least an hour old,
        # so that it holds a close and points after it at any time of the week (no close from Saturday 05:00 to Tuesday 05:00 UTC+8).
        sel = dict(SELECTIONS[0], hours=48)
        end_ms = int(psql("SELECT max(close_ms) + 3600000 FROM etfpair.etf_session WHERE close_ms <= %d - 3600000" % newest_ms(), "-At"))
        closes = [int(c) for c in psql("SELECT close_ms FROM etfpair.etf_session WHERE close_ms BETWEEN %d AND %d" % (end_ms - 47 * 3600_000, end_ms),
                                       "-At").split()]
        self.assertTrue(closes)
        for chart, column in ((board.Q_SPREAD, "Spread · Tooltip"), (board.Q_PRICES, "Bull Price")):
            q = 'SELECT (extract(epoch FROM time) * 1000)::bigint, "%s" IS NULL FROM (%s) t ORDER BY 1' % (column, substitute(chart, sel, end_ms))
            rows = [(int(at), gap == "t") for at, gap in (line.split("|") for line in psql(q, "-At").split())]
            self.assertFalse(rows[0][1], "the range opens with a row without values")
            for (at0, gap0), (at1, gap1) in zip(rows, rows[1:]):
                if gap1:
                    self.assertEqual((at1 - at0, gap0), (1, False), "a row without values not 1 ms after a point: %d" % at1)
            for close in closes:
                before = max(at for at, gap in rows if not gap and at <= close)
                after = min(at for at, gap in rows if not gap and at > close)
                self.assertTrue(any(gap and before < at < after for at, gap in rows), "no break at the close %d (%s)" % (close, column))

    @unittest.skipUnless(PSQL, "set ETFPAIR_BOARD_PSQL to run the queries")
    def test_entry_points_group_by(self):
        # Entry Points by Phase as Grafana builds it from the Spread row's frame (Filter data by values -> Group by -> Sort by ->
        # Organize, emulated as Grafana 13.2.2 does them) equals DESIGN 1.11 written in SQL on combo_range, cell by cell as shown:
        # on a range with a close (a break, the next session's phases), the No Mean stretch after it, and a recorder restart.
        b = board.build()
        org = next(t for t in by_title(b, "Entry Points by Phase · Bps")["transformations"] if t["id"] == "organize")["options"]
        grp = next(t for t in by_title(b, "Entry Points by Phase · Bps")["transformations"] if t["id"] == "groupBy")["options"]["fields"]
        cases = [(dict(SOXL, venue="binance", ma="sma", ma_minutes="60", d="5", notional="10000", hours=6), utc8_ms("2026-11-21 08:00")),
                 (dict(SOXL, venue="bitget", ma="ema", ma_minutes="30", d="4", notional="100000", hours=1), utc8_ms("2026-11-22 12:40")),
                 (dict(TQQQ, venue="bitget", ma="sma", ma_minutes="30", d="3", notional="50000", hours=24), utc8_ms("2026-11-22 00:00"))]
        seen = set()
        for sel, end_ms in cases:
            frame = rows_of(substitute(board.Q_SPREAD, sel, end_ms))
            seen |= {"break"} if any(not r["Spread · Tooltip"] for r in frame) else set()
            seen |= {"no mean"} if any(r["Mean · Tooltip"] == str(board.CODE["No Mean"]) for r in frame) else set()
            groups = {}
            for r in (r for r in frame if r["Phase"]):
                g = groups.setdefault(r["Phase"], {})
                for name, spec in grp.items():
                    for reducer in spec["aggregations"]:
                        values = g.setdefault("%s (%s)" % (name, reducer), [])
                        values.append(r[name])
            table = []
            for place, g in sorted(groups.items(), key=lambda kv: int(kv[0])):
                row = {"Phase": board.PHASE_PLACES[int(place)]}
                for name, values in g.items():
                    numbers = [float(v) for v in values if v != ""]
                    row[name] = {"sum": sum(numbers), "count": len(values), "max": max(numbers) if numbers else None}[name.rsplit("(", 1)[1][:-1]]
                table.append({org["renameByName"].get(k, k): v for k, v in row.items()})
            want = rows_of(substitute("""
SELECT ph.phase, count(*) FILTER (WHERE r.signal LIKE 'Long%') AS long, count(*) FILTER (WHERE r.signal LIKE 'Short%') AS short,
       count(*) FILTER (WHERE r.signal LIKE '% · Partial') AS partial,
       max(r.mu_bps - r.s_long_bps) FILTER (WHERE NOT r.past_t AND r.s_long_bps < r.mu_bps) AS best_long,
       max(r.s_short_bps - r.mu_bps) FILTER (WHERE NOT r.past_t AND r.s_short_bps > r.mu_bps) AS best_short,
       count(*) FILTER (WHERE r.signal = 'Past T') AS past_t, count(*) FILTER (WHERE r.signal = 'No Book') AS no_book, count(*) AS points
FROM @RANGE@
JOIN etfpair.etf_phase ph ON r.ts_ms > ph.start_ms AND r.ts_ms <= ph.end_ms
WHERE r.s_bps IS NOT NULL AND r.mu_bps IS NOT NULL
GROUP BY ph.phase ORDER BY array_position(ARRAY['Pre-Market', 'Regular', 'After-Hours', 'Overnight', 'Closed'], ph.phase)""".replace(
                "@RANGE@", "%s CROSS JOIN LATERAL %s" % (board.SETTINGS, board.RANGE)),
                                    sel, end_ms))
            self.assertTrue(want, sel)
            seen |= {"phases"} if len(want) > 1 else set()
            self.assertEqual([r["Phase"] for r in table], [r["phase"] for r in want], sel)
            for got, w in zip(table, want):
                self.assertEqual([int(got[k]) for k in ("Long Points", "Short Points", "Partial Points", "Past T Points", "No Book Points", "Points")],
                                 [int(w[k]) for k in ("long", "short", "partial", "past_t", "no_book", "points")], (sel, w["phase"]))
                for name, key in (("Best Long", "best_long"), ("Best Short", "best_short")):
                    self.assertEqual(None if got[name] is None else shown_text(got[name], 1), shown_text(float(w[key]), 1) if w[key] else None, (sel, name))
        self.assertEqual(seen, {"break", "no mean", "phases"})

    @unittest.skipUnless(PSQL, "set ETFPAIR_BOARD_PSQL to run the queries")
    def test_rounding_keeps_the_shown_values(self):
        # Each rounded figure shows exactly as its unrounded value would (Grafana's toFixed of either), in every frame the board
        # rounds: the Spread row's, the Prices row's and the strips'.
        raw = unrounded_module()
        now_ms = newest_ms() - SETTLED_MS  # the same inputs for both runs
        checked = 0
        for sel, end_ms in ((dict(SOXL, venue="bitget", ma="sma", ma_minutes="30", d="7.25", notional="100000", hours=1), now_ms),
                            (dict(TQQQ, venue="binance", ma="ema", ma_minutes="60", d="10", notional="10000", hours=6), utc8_ms("2026-11-21 08:00")),
                            (dict(SOXL, venue="binance", ma="sma", ma_minutes="60", d="10", notional="5000", hours=168), now_ms)):
            for rounded_q, raw_q, sql_of in ((board.Q_SPREAD, raw.Q_SPREAD, dict(board.SPREAD_CHART + board.DEVIATION_CHART + board.ENTRY_CHART +
                                                                                board.ENTRY_COLUMNS)),
                                             (board.Q_PRICES, raw.Q_PRICES, dict(board.PRICE_COLUMNS)),
                                             (board.Q_LAST, raw.Q_LAST, dict(board.LAST_COLUMNS))):
                rounded = rows_of(substitute(rounded_q, sel, now_ms, end_ms))
                unrounded = rows_of(substitute(raw_q, sel, now_ms, end_ms))
                self.assertEqual(len(rounded), len(unrounded))
                for name in rounded[0]:
                    d = decimals_of(sql_of[name]) if name in sql_of else None
                    if d is None:
                        continue
                    for a, b in zip(rounded, unrounded):
                        if a[name] == "" or b[name] == "" or float(a[name]) <= -1e9:
                            self.assertEqual(a[name], b[name], name)
                            continue
                        self.assertEqual(shown_text(float(a[name]), d), shown_text(float(b[name]), d), (name, a[name], b[name]))
                        self.assertEqual(float(a[name]), float(shown_text(float(b[name]), d)), (name, a[name], b[name]))
                        checked += 1
        self.assertGreater(checked, 100000)

    @unittest.skipUnless(PSQL, "set ETFPAIR_BOARD_PSQL to run the queries")
    def test_sides_and_absolute_values(self):
        # On real ranges: wherever both legs' books give the point, Real Long Spread >= Spread >= Real Short Spread on the signed
        # values, also as the tooltip shows them; the three deviations the board draws are |x - Mean| >= 0; a Long point has its
        # Real Long Spread at least Entry below the Mean and a Short point its Real Short Spread at least Entry above it.
        now_ms = newest_ms() - SETTLED_MS  # the same inputs for both runs
        for sel, end_ms in ((dict(SOXL, venue="bitget", ma="sma", ma_minutes="30", d="5", notional="100000", hours=6), now_ms),
                            (dict(SOXL, venue="binance", ma="ema", ma_minutes="60", d="3", notional="10000", hours=6), utc8_ms("2026-11-21 08:00")),
                            (dict(TQQQ, venue="bitget", ma="sma", ma_minutes="30", d="3", notional="50000", hours=24), utc8_ms("2026-11-22 00:00"))):
            d = float(sel["d"])
            signed = rows_of(substitute("SELECT r.* FROM %s CROSS JOIN LATERAL %s ORDER BY r.ts_ms" % (board.SETTINGS, board.RANGE), sel, now_ms, end_ms))
            frame = rows_of(substitute(board.Q_SPREAD, sel, now_ms, end_ms))
            self.assertEqual(len(signed), len(frame))
            mids = signals = 0
            for r, f in zip(signed, frame):
                if r["basis"] == "mid" and r["s_bps"]:
                    mids += 1
                    self.assertGreaterEqual(float(r["s_long_bps"]), float(r["s_bps"]), r)
                    self.assertGreaterEqual(float(r["s_bps"]), float(r["s_short_bps"]), r)
                    self.assertGreaterEqual(float(f["Real Long Spread"]), float(f["Spread · Tooltip"]), f)
                    self.assertGreaterEqual(float(f["Spread · Tooltip"]), float(f["Real Short Spread"]), f)
                for name in ("Deviation · Line", "Real Long Deviation", "Real Short Deviation", "Real Long Deviation · Faded",
                             "Real Short Deviation · Faded", "Long Dots", "Short Dots", "Past T"):
                    if f[name]:
                        self.assertGreaterEqual(float(f[name]), 0, (name, f))
                # The Deviation chart: the signed deviations, each of x - Mean rounded once; where both legs' books give the point the
                # blue line at or above the purple one and the orange at or below (rounding keeps the order); the drawn lines and
                # their tooltip twins agree; the absolute ones are their magnitudes.
                for name, real in (("Signed Deviation", "s_bps"), ("Signed Real Long Deviation", "s_long_bps"), ("Signed Real Short Deviation", "s_short_bps")):
                    if r["mu_bps"] and r[real] and r["past_t"] == "f":
                        self.assertEqual(float(f[name]), float(shown_text(float(r[real]) - float(r["mu_bps"]), 1)), (name, r))
                        self.assertEqual(float(f[name + " · Tooltip"]), float(f[name]), (name, r))
                    else:
                        self.assertEqual(f[name], "", (name, r))
                if f["Signed Deviation"]:
                    self.assertEqual(abs(float(f["Signed Deviation"])), float(f["Deviation · Line"]), f)
                if r["basis"] == "mid" and f["Signed Real Long Deviation"]:
                    self.assertGreaterEqual(float(f["Signed Real Long Deviation"]), float(f["Signed Deviation"]), f)
                    self.assertGreaterEqual(float(f["Signed Deviation"]), float(f["Signed Real Short Deviation"]), f)
                # Each real line faded at every second with a value, bright where its real spread lies on the side it fills on.
                for side, real, favourable in (("Long", "s_long_bps", lambda x, mu: x < mu), ("Short", "s_short_bps", lambda x, mu: x > mu)):
                    if r["mu_bps"] and r[real] and r["past_t"] == "f":
                        absolute = shown_text(abs(float(r[real]) - float(r["mu_bps"])), 1)
                        self.assertEqual(float(f["Real %s Deviation · Faded" % side]), float(absolute), r)
                        bright = f["Real %s Deviation" % side]
                        self.assertEqual(bright and float(bright), float(absolute) if favourable(float(r[real]), float(r["mu_bps"])) else "", r)
                if r["signal"].startswith("Long"):
                    signals += 1
                    self.assertLessEqual(float(r["s_long_bps"]) - float(r["mu_bps"]), -d, r)
                    self.assertLessEqual(float(f["Signed Real Long Deviation"]), -d, f)
                if r["signal"].startswith("Short"):
                    signals += 1
                    self.assertGreaterEqual(float(r["s_short_bps"]) - float(r["mu_bps"]), d, r)
                    self.assertGreaterEqual(float(f["Signed Real Short Deviation"]), d, f)
            self.assertGreater(mids, 100, sel)
            self.assertGreater(signals, 0, sel)
            # The legend's Max of each bright line (Grafana's max over its values) is the largest Best of Entry Points by Phase.
            for side in ("Long", "Short"):
                bright = [float(f["Real %s Deviation" % side]) for f in frame if f["Real %s Deviation" % side]]
                best = [float(f["Best " + side]) for f in frame if f["Best " + side] and f["Phase"]]
                self.assertTrue(bright, (sel, side))
                self.assertEqual(max(bright), max(best), (sel, side))


if __name__ == "__main__":
    unittest.main()
