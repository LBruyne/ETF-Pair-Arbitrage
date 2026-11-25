#!/usr/bin/env python3
"""Checks of the Live Monitor board (live_monitor_board.py).

    python3 dashboard/grafana/test_live_monitor.py

The board's shape needs nothing: every query reads schema trade only, through the etfpair-trade data source, keeps one mode
(each table it reads is filtered on live = ('$mode' = 'Live')), and holds no picked variable outside quotes; the tooltips
of the execution charts have a value at every point. Running the queries needs the trade database: set ETFPAIR_TRADE_DSN to
a DSN of the read-only role (core/.env.reader holds one; psql gets it through libpq's environment, never on its command
line, and it is never printed); without
it those tests are skipped. They run every query over several ranges, both modes and every scope, and compare Net, P&L and
the Round Trips table with a recomputation from the fills (PRD V19)."""
import json, os, re, subprocess, sys, time, unittest, urllib.parse
from datetime import datetime
from decimal import Decimal

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import live_monitor_board as board

DSN = os.environ.get("ETFPAIR_TRADE_DSN")
# Variables Grafana fills from the time picker (numbers) and the ones the board sets from SQL it runs itself: safe bare.
MACROS = {"__from", "__to", "__url_time_range", "__data"}
TABLES = {"process_run", "minute_bar", "signal_sample", "anchor_fact", "cycle", "lot", "cex_order", "cex_fill", "decision", "funding",
          "funding_rate", "inventory_sample", "inventory_state", "event"}


def queries(b):
    """(where, sql) of every query of the board: panel targets (folded rows' too), variables and annotations."""
    out = []

    def walk(panels):
        for p in panels:
            walk(p.get("panels", []))
            for t in p.get("targets", []):
                if "rawSql" in t:
                    out.append(("panel %s/%s" % (p["title"], t["refId"]), t["rawSql"]))
    walk(b["panels"])
    out += [("variable " + v["name"], v["query"]) for v in b["templating"]["list"] if v["type"] == "query"]
    out += [("annotation " + a["name"], a["rawQuery"]) for a in b["annotations"]["list"]]
    return out


def enclosing_rest(q, i):
    """The text of q from i to the end of the (sub)query around i: up to the parenthesis that closes it."""
    depth = 0
    for j in range(i, len(q)):
        depth += {"(": 1, ")": -1}.get(q[j], 0)
        if depth < 0:
            return q[i:j]
    return q[i:]


def fill(q, values):
    return re.sub(r"\$\{(\w+)(?::\w+)?\}|\$(\w+)", lambda m: str(values[m.group(1) or m.group(2)]).replace("'", "''"), q)


def pg_env(dsn):
    """The libpq environment for a postgres:// DSN, so that its password never sits on a command line (/proc/*/cmdline)."""
    p = urllib.parse.urlparse(dsn)
    q = dict(urllib.parse.parse_qsl(p.query))
    return dict(os.environ, PGHOST=p.hostname or "", PGPORT=str(p.port or 5432), PGUSER=urllib.parse.unquote(p.username or ""),
                PGPASSWORD=urllib.parse.unquote(p.password or ""), PGDATABASE=p.path.lstrip("/"), PGSSLMODE=q.get("sslmode", "prefer"))


def psql_json(q):
    """The rows of q as a list of dicts (one read-only statement)."""
    r = subprocess.run(["psql", "-X", "-q", "-At", "-v", "ON_ERROR_STOP=1", "-c", "SELECT coalesce(json_agg(row_to_json(t)), '[]') FROM (%s) t" % q],
                       capture_output=True, text=True, env=pg_env(DSN))
    if r.returncode:
        raise AssertionError(r.stderr.replace(DSN, "<dsn>")[:600])
    return json.loads(r.stdout, parse_float=Decimal)


class Shape(unittest.TestCase):
    def setUp(self):
        self.b = board.build()

    def test_every_table_read_keeps_one_mode(self):
        """Each reference to a trade table is followed, inside its own (sub)query, by a filter of its rows on the picked mode
        (or a join on another filtered row's live)."""
        for where, q in queries(self.b):
            if where == "variable mode":
                continue   # picks the mode itself: the newest run's, of either
            for m in re.finditer(r"\btrade\.(\w+)(?:\s+(?:AS\s+)?(\w+))?", q):
                table, alias = m.group(1), m.group(2)
                self.assertIn(table, TABLES, where)
                segment = enclosing_rest(q, m.end())
                if alias is None or alias.upper() in ("WHERE", "ON", "JOIN", "CROSS", "LEFT", "ORDER", "GROUP"):
                    self.assertRegex(segment, r"^\s*(WHERE\s+)?live = \('\$mode' = 'Live'\)", where)
                else:
                    self.assertRegex(segment, r"\b%s\.live = (\('\$mode' = 'Live'\)|\w+\.live)" % re.escape(alias), "%s: %s" % (where, m.group(0)))

    def test_reads_schema_trade_only(self):
        for where, q in queries(self.b):
            self.assertNotRegex(q, r"\betfpair\.|\bpublic\.|pg_\w+", where)
            for m in re.finditer(r"(?<!DISTINCT )\b(?:FROM|JOIN)\s+(\w+)\.(\w+)", q):
                self.assertEqual(m.group(1), "trade", where)

    def test_no_variable_outside_quotes(self):
        """A picked variable only inside a quoted literal (Grafana doubles a single quote in it): with every literal taken
        out, only the time macros are left."""
        for where, q in queries(self.b):
            bare = re.sub(r"'(?:[^']|'')*'", "''", q)
            for m in re.finditer(r"\$\{?(\w+)", bare):
                self.assertIn(m.group(1), MACROS, "%s: %s" % (where, bare[m.start() - 30:m.end() + 10]))

    def test_trade_data_source_everywhere(self):
        text = json.dumps(self.b)
        self.assertNotIn('"uid": "etfpair"', text)
        for p in self.b["panels"] + [q for r in self.b["panels"] for q in r.get("panels", [])]:
            for t in p.get("targets", []):
                self.assertIn(t["datasource"]["uid"], ("etfpair-trade", "-- Dashboard --"), p["title"])
        for v in self.b["templating"]["list"]:
            if v["type"] == "query":
                self.assertEqual(v["datasource"]["uid"], "etfpair-trade", v["name"])
        for a in self.b["annotations"]["list"]:
            self.assertEqual(a["datasource"]["uid"], "etfpair-trade", a["name"])

    def test_board_settings(self):
        self.assertEqual(self.b["refresh"], "10s")
        self.assertEqual(self.b["time"], {"from": "now-24h", "to": "now"})
        names = [v["name"] for v in self.b["templating"]["list"]]
        self.assertEqual(names[:3], ["mode", "scope", "cycle"])
        for p in self.b["panels"] + [q for r in self.b["panels"] for q in r.get("panels", [])]:
            if p["type"] != "row":
                self.assertTrue(p["description"], p["title"])
        titles = [p["title"] for p in self.b["panels"] if p["type"] == "row"]
        self.assertEqual([t.split(" · ")[0] for t in titles], ["Live Execution", "Round Trips", "Round Trip", "Risk & Account", "Diagnostics",
                                                               "Diagnostics"])
        self.assertEqual([p.get("collapsed") for p in self.b["panels"] if p["type"] == "row"], [False] * 4 + [True, True])

    def test_the_four_questions_on_top(self):
        """Health, Position, Entry, Net and P&L sit above the first row, in the top ten grid units (no scrolling at 1680 px)."""
        top = []
        for p in self.b["panels"]:
            if p["type"] == "row":
                break
            top.append(p)
        self.assertEqual([p["title"].split(" · ")[0] for p in top], ["", "Health", "Position", "Net", "P&L", "Entry"])
        self.assertLessEqual(max(p["gridPos"]["y"] + p["gridPos"]["h"] for p in top), 10)

    def test_execution_charts(self):
        """One frame for both charts; the Spread chart shows the Deviation chart's result; every tooltip row is a tooltip-only
        series (a drawn line is never in the tooltip) and the Deviation chart's tooltip hides zeros, the value of the marker
        rows on rows without a decision near them."""
        dev = next(p for p in self.b["panels"] if p["title"] == "Deviation from the Mean · σ")
        spread = next(p for p in self.b["panels"] if p["title"] == "Spread · D")
        self.assertEqual(len(dev["targets"]), 1)
        self.assertEqual(spread["targets"][0]["panelId"], dev["id"])
        self.assertTrue(dev["options"]["tooltip"]["hideZeros"])
        for p in (dev, spread):
            for o in p["fieldConfig"]["overrides"]:
                props = {x["id"]: x["value"] for x in o["properties"]}
                hide = props.get("custom.hideFrom", {})
                if not hide.get("viz") and hide.get("tooltip") is not True:
                    self.fail("%s: drawn series %s in the tooltip" % (p["title"], o["matcher"]["options"]))
        tips = [o["matcher"]["options"] for o in dev["fieldConfig"]["overrides"]
                if any(x["id"] == "custom.hideFrom" and x["value"].get("viz") for x in o["properties"])]
        self.assertEqual(tips[-len(board.MARKER_INFO):], [n for n, _ in board.MARKER_INFO])
        # Every column of the frame is kept by one chart or the other; the legend holds at most eight series.
        frame = [n for n, _ in board.ALL_COLUMNS]
        kept = set(dev["transformations"][0]["options"]["include"]["names"]) | set(spread["transformations"][0]["options"]["include"]["names"])
        self.assertEqual(set(frame) - kept, set())
        in_legend = [n for n in dev["transformations"][1]["options"]["indexByName"] if n != "time" and not any(
            o["matcher"]["options"] == n and any(x["id"] == "custom.hideFrom" and x["value"].get("legend") for x in o["properties"])
            for o in dev["fieldConfig"]["overrides"])]
        self.assertLessEqual(len(in_legend), 8, in_legend)

    def test_anchor_marks_on_every_chart(self):
        charts = [p["id"] for p in self.b["panels"] if p["type"] == "timeseries"]
        mark = next(a for a in self.b["annotations"]["list"] if a["name"] == "Anchor Changes")
        self.assertEqual(sorted(mark["filter"]["ids"]), sorted(charts))


@unittest.skipUnless(DSN, "ETFPAIR_TRADE_DSN not set: queries not run")
class Queries(unittest.TestCase):
    def values(self, hours, mode="Paper", scope="session", cycle="none"):
        end = int(time.time() * 1000)
        return {"mode": mode, "scope": scope, "cycle": cycle, "bull": "SOXL", "bear": "SOXS", "__from": int(end - hours * 3600000), "__to": end}

    def test_every_query_runs(self):
        b = board.build()
        for hours in (0.25, 6, 24, 168):
            for mode in ("Paper", "Live"):
                for scope in ("session", "run", "range"):
                    v = self.values(hours, mode, scope)
                    opts = psql_json(fill(board.Q_CYCLE_OPTIONS, v))
                    v["cycle"] = opts[0]["__value"]
                    for where, q in queries(b):
                        with self.subTest(where=where, hours=hours, mode=mode, scope=scope):
                            psql_json(fill(q, v))

    def test_plain_words(self):
        """End and idle reasons in words; a code the board does not list (a new end reason such as dust_only, before the board
        knows it) reads as its own words."""
        got = psql_json("SELECT %s AS listed, %s AS other, %s AS open, %s AS idle, %s AS sent FROM (SELECT 'dust_only' AS a, "
                        "'brand_new_reason' AS b, NULL::text AS n, 'regular_hours' AS i) t" % (
                            board.end_reason("t.a"), board.end_reason("t.b"), board.end_reason("t.n"), board.idle_why("t.i"), board.idle_state("t.n")))[0]
        self.assertEqual(got, {"listed": "Trimmed to Dust", "other": "Brand New Reason", "open": "Open", "idle": "Regular Hours", "sent": "Order Sent"})

    def test_execution_frame(self):
        """The execution charts' frame: a row is a break row (every column NULL) or has a value, possibly a hidden 0 or a code,
        in every tooltip row; a break row sits right before the point it separates from what came before, after a run's last
        decisions (none of which may follow it); each decision of the range is one dot."""
        tips = [n for n, _ in board.TOOLTIP_COLUMNS + board.STATE_COLUMNS + board.MARKER_INFO]
        dots = [n for n, _, _, _, _ in board.DOTS]
        for hours in (0.25, 6, 24, 168):
            v = self.values(hours)
            rows = psql_json(fill(board.Q_EXECUTION, v))
            drawn = 0
            for i, r in enumerate(rows):
                values = [r[n] for n, _ in board.ALL_COLUMNS]
                if all(x is None for x in values):
                    self.assertLess(i + 1, len(rows), "a break row ends the frame")
                    after = rows[i + 1]
                    self.assertIsNotNone(after["Spread · Tooltip"], "a break row right before a point")
                    self.assertFalse(after["Action"] and after["Decided"] == round(datetime.fromisoformat(after["time"]).timestamp() * 1000),
                                     "%s: a decision after a break row would join the lines across the gap" % after["time"])
                    continue
                for n in tips:
                    self.assertIsNotNone(r[n], "%s h, %s: %s NULL" % (hours, r["time"], n))
                drawn += sum(r[name] is not None for name in dots)
            decisions = psql_json(fill("SELECT count(*) AS n FROM trade.decision d WHERE d.live = ('$mode' = 'Live') AND d.kind IN "
                                       "('entry', 'exit', 'complete', 'trim', 'forced') AND d.ts_ms >= $__from AND d.ts_ms <= $__to", v))[0]["n"]
            self.assertEqual(drawn, decisions, "%s h" % hours)

    def test_pnl_and_round_trips_from_the_fills(self):
        """PRD V19: the Net and P&L figures and the Round Trips table against the fills (sells' value - buys' value of a flat
        cycle, fees, entry notional), recomputed here without cycle or lot."""
        v = self.values(24 * 30, scope="range")
        fills = {r["cycle_id"]: r for r in psql_json(
            "SELECT o.cycle_id, sum(CASE f.side WHEN 'sell' THEN f.value ELSE -f.value END) AS gross, sum(f.fee) AS fees, "
            "sum(f.value) FILTER (WHERE o.purpose IN ('entry', 'complete')) AS entry_value "
            "FROM trade.cex_fill f JOIN trade.cex_order o ON o.live = f.live AND o.client_order_id = f.client_order_id WHERE f.live = false "
            "GROUP BY o.cycle_id")}
        cycles = {r["cycle_id"]: r for r in psql_json("SELECT * FROM trade.cycle WHERE live = false")}
        rows = psql_json(fill(board.Q_BLOTTER, v))
        self.assertTrue(all(cycles[r["cycle_id"]]["end_reason"] != "no_fill" for r in rows))
        checked = 0
        for row in rows:
            c, f = cycles[row["cycle_id"]], fills.get(row["cycle_id"])
            if f is None:
                continue
            self.assertEqual(row["Notional"], f["entry_value"] or 0, row["cycle_id"])
            if c["status"] != "open" and c["funding_known"] and c["dust_bull"] == 0 and c["dust_bear"] == 0:
                self.assertEqual(row["Net"], f["gross"] - f["fees"] + c["funding"], row["cycle_id"])
                self.assertAlmostEqual(float(row["Net bps"]), float(row["Net"] / row["Notional"] * 10000), places=6)
                checked += 1
        self.assertGreater(checked, 0, "no flat closed round trip to check")
        known = [c for c in cycles.values() if c["status"] != "open" and c["funding_known"] and c["end_reason"] != "no_fill"
                 and int(v["__from"]) <= c["closed_ms"] <= int(v["__to"]) and c["cycle_id"] in fills]
        p = psql_json(fill(board.Q_PNL, v))[0]
        net = psql_json(fill(board.Q_NET, v))
        self.assertEqual(-p["Fees"], sum(fills[c["cycle_id"]]["fees"] for c in known))
        self.assertEqual(net[-1]["Net"], sum(fills[c["cycle_id"]]["gross"] - fills[c["cycle_id"]]["fees"] + c["funding"] for c in known))


if __name__ == "__main__":
    unittest.main()
