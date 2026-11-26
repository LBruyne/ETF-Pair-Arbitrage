#!/usr/bin/env python3
"""Checks of the Live Monitor board (live_monitor_board.py).

    python3 dashboard/grafana/test_live_monitor.py

The board's shape needs nothing: every query reads schema trade only, through the etfpair-trade data source, keeps the picked
Run (each table of the program's records it reads is filtered on the Run's run_id and live, the market facts on its live), and
holds no picked variable outside quotes; the tooltips of the execution charts have a value at every point. Running the queries needs the trade database: set ETFPAIR_TRADE_DSN to
a DSN of the read-only role (core/.env.reader holds one; psql gets it through libpq's environment, never on its command
line, and it is never printed); without
it those tests are skipped. They run every query over several ranges, every Run and several Since / Until, compare Net, P&L
and the Round Trips table with a recomputation from the fills (PRD V19), the Settings with process_run.config and the last
second's figures with the Run's newest signal_sample."""
import json, math, os, re, subprocess, sys, time, unittest, urllib.parse
from datetime import datetime, timezone
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


def utc8(ms):
    """A Unix millisecond time as a naive UTC+8 datetime."""
    return datetime.fromtimestamp((ms + 8 * 3600000) / 1000, timezone.utc).replace(tzinfo=None)


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

    def test_every_table_read_keeps_the_picked_run(self):
        """Each reference to a trade table is followed, inside its own (sub)query, by a filter of its rows on the picked Run
        (run_id and live of sel), a cycle also by the Run's decisions on it (a new start takes an open cycle over), an order and
        the market facts it read (funding rates, anchors) on its mode, or a join on another filtered row's live; process_run is
        read only to resolve and list the Runs."""
        run = r"\b{a}\.run_id = \(SELECT sel\.run_id FROM sel\) AND {a}\.live = \(SELECT sel\.live FROM sel\)"
        mode = r"\b{a}\.live = (\(SELECT sel\.live FROM sel\)|\w+\.live)"
        for where, q in queries(self.b):
            for m in re.finditer(r"\btrade\.(\w+)(?:\s+(?:AS\s+)?(\w+))?", q):
                table, alias = m.group(1), m.group(2)
                self.assertIn(table, TABLES, where)
                self.assertTrue(alias and alias.upper() not in ("WHERE", "ON", "JOIN", "CROSS", "LEFT", "ORDER", "GROUP"), "%s: %s" % (where, m.group(0)))
                if table == "process_run":
                    continue
                segment = enclosing_rest(q, m.end())
                joined = mode.replace("(\\(SELECT sel\\.live FROM sel\\)|", "(")
                cycle = (r"\b{a}\.live = \(SELECT sel\.live FROM sel\) AND \({a}\.run_id = \(SELECT sel\.run_id FROM sel\) OR {a}\.cycle_id IN "
                         r"\(SELECT dr\.cycle_id FROM trade\.decision dr WHERE dr\.run_id = \(SELECT sel\.run_id FROM sel\)")
                pattern = {"funding_rate": mode, "anchor_fact": mode, "cex_order": run + "|" + mode, "cycle": cycle + "|" + joined}.get(
                    table, run + "|" + joined).format(a=re.escape(alias))
                self.assertRegex(segment, pattern, "%s: %s" % (where, m.group(0)))

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
        self.assertEqual(self.b["time"], {"from": "now-3h", "to": "now"})
        shown = [(v["name"], v["label"]) for v in self.b["templating"]["list"] if not v.get("hide")]
        self.assertEqual(shown, [("run", "Run"), ("since", "Since"), ("since_hour", "At"), ("until", "Until"), ("until_hour", "At"), ("combo", "Combo")])
        for p in self.b["panels"] + [q for r in self.b["panels"] for q in r.get("panels", [])]:
            if p["type"] != "row":
                self.assertTrue(p["description"], p["title"])
        titles = [p["title"] for p in self.b["panels"] if p["type"] == "row"]
        self.assertEqual([t.split(" · ")[0] for t in titles], ["Live Execution", "Round Trips", "Round Trip", "Risk & Account", "Settings",
                                                               "Diagnostics", "Diagnostics"])
        self.assertEqual([p.get("collapsed") for p in self.b["panels"] if p["type"] == "row"], [False] * 5 + [True, True])

    def test_the_trader_questions_on_top(self):
        """Health, Position, Net and P&L, the last second's Legs and Spread, and Entry sit above the first row, in the top
        fourteen grid units (no scrolling at 1680 px)."""
        top = []
        for p in self.b["panels"]:
            if p["type"] == "row":
                break
            top.append(p)
        self.assertEqual([p["title"].split(" · ")[0] for p in top], ["", "Health", "Position", "Net", "P&L", "Legs", "Spread", "Entry"])
        self.assertLessEqual(max(p["gridPos"]["y"] + p["gridPos"]["h"] for p in top), 14)

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
    # The paper run of 2026-11-26 02:02:09 UTC+8 (Run 06, the one the auditor's sample of item 70 is from).
    PAPER_RUN = "1795629729414-backup-tk-huichao"

    def values(self, hours, run="__paper", since="0", since_hour="00", until=board.NO_END_MS, until_hour="24", cycle="none"):
        end = int(time.time() * 1000)
        return {"run": run, "since": since, "since_hour": since_hour, "until": until, "until_hour": until_hour, "combo": "SOXL / SOXS",
                "cycle": cycle, "bull": "SOXL", "bear": "SOXS", "__from": int(end - hours * 3600000), "__to": end}

    def runs(self):
        return [r["run_id"] for r in psql_json("SELECT run_id FROM trade.process_run ORDER BY started_ms")]

    def test_every_query_runs(self):
        """Every query, for every Run and both Newest, over several time ranges, with Since / Until at Run start / Run end and at
        a date and hour (UTC+8 midnight of the Run's first day plus 3 h, to that day 24:00)."""
        b = board.build()
        for run in ["__live", "__paper"] + self.runs():
            days = psql_json(fill(board.RUN_DAYS, self.values(1, run)))
            ranges = [("0", "00", board.NO_END_MS, "24")] + ([(str(days[0]["day_ms"]), "03", str(days[0]["day_ms"]), "24")] if days else [])
            for hours in (0.25, 24, 168):
                for since, sh, until, uh in ranges:
                    v = self.values(hours, run, since, sh, until, uh)
                    v["cycle"] = psql_json(fill(board.Q_CYCLE_OPTIONS, v))[0]["__value"]
                    for where, q in queries(b):
                        with self.subTest(where=where, run=run, hours=hours, since=since):
                            psql_json(fill(q, v))

    def test_run_picker(self):
        """The Run options: Newest Live Run, Newest Paper Run, then every Run newest first, numbered by start from 01 across both
        modes, with its mode, start, end and state; Newest resolves to the newest Run of that mode; a wrong Combo or an unknown
        Run shows nothing (No Run)."""
        runs = psql_json("SELECT run_id, live, started_ms, stopped_ms FROM trade.process_run ORDER BY started_ms, run_id")
        opts = psql_json(fill(board.Q_RUN_OPTIONS, self.values(1)))
        heads = [o["__value"] for o in opts[:2]]
        self.assertEqual(heads, [x for x, live in (("__live", True), ("__paper", False)) if any(r["live"] == live for r in runs)])
        rest = opts[len(heads):]
        self.assertEqual([o["__value"] for o in rest], [r["run_id"] for r in reversed(runs)])
        for n, r in enumerate(runs, 1):
            text = next(o["__text"] for o in rest if o["__value"] == r["run_id"])
            self.assertTrue(text.startswith("Run %02d · %s · " % (n, "Live" if r["live"] else "Paper")), text)
            self.assertTrue(text.endswith(" · stopped") if r["stopped_ms"] else text.endswith((" · running", " · lost")), text)
        for newest, live in (("__live", True), ("__paper", False)):
            want = [r["run_id"] for r in runs if r["live"] == live][-1:]
            got = psql_json(fill("WITH %s SELECT run_id FROM sel" % board.SEL, self.values(1, newest)))
            self.assertEqual([g["run_id"] for g in got], want)
        self.assertEqual(psql_json(fill(board.Q_MODE, dict(self.values(1), combo="SOXL / SOXX")))[0]["Mode"], "NO RUN · pick a Run (or the Combo it trades)")
        self.assertEqual(psql_json(fill(board.Q_RUN_TEXT, self.values(1, "nonsense'; --"))), [{"__text": "No Run"}])

    def test_range(self):
        """The Range: Run start to Run end is the Run's whole life; a date at an hour starts at that UTC+8 hour; Until a date
        at 24 ends at the next midnight; Until before Since leaves it empty and says so."""
        r = psql_json("SELECT started_ms, stopped_ms FROM trade.process_run WHERE run_id = '%s'" % self.PAPER_RUN)[0]
        rg = "WITH %s SELECT rg.from_ms, rg.to_ms FROM rg" % board.SEL
        whole = psql_json(fill(rg, self.values(1, self.PAPER_RUN)))[0]
        self.assertEqual(whole["from_ms"], r["started_ms"])
        self.assertGreaterEqual(whole["to_ms"], (r["stopped_ms"] or int(time.time() * 1000) - 5000) + 1)
        day = psql_json(fill(board.RUN_DAYS, self.values(1, self.PAPER_RUN)))[-1]["day_ms"]
        self.assertEqual(utc8(day).strftime("%H:%M"), "00:00")
        cut = psql_json(fill(rg, self.values(1, self.PAPER_RUN, str(day), "05", str(day), "24")))[0]
        self.assertEqual(cut["from_ms"], max(r["started_ms"], day + 5 * 3600000))
        self.assertAlmostEqual(cut["to_ms"], min(whole["to_ms"], day + 24 * 3600000), delta=10000)   # now moves between the reads
        empty = psql_json(fill(board.Q_SPAN, self.values(1, self.PAPER_RUN, str(day), "09", str(day), "08")))
        self.assertEqual(empty, [{"__text": "empty: Until not after Since"}])

    def test_settings_are_the_run_config(self):
        """Every Settings value is read from process_run.config of the picked Run: the ones below are checked against the
        JSON for every Run (M, L, N = M × L, d_in / d_out as lines, mean and σ, pricing levels, transport, live)."""
        for run in self.runs():
            c = psql_json("SELECT config, live, host FROM trade.process_run WHERE run_id = '%s'" % run)[0]
            cfg, st = json.loads(c["config"]) if isinstance(c["config"], str) else c["config"], c
            got = {}
            for group, _ in board.SETTINGS:
                for row in psql_json(fill(board.q_settings(group), self.values(1, run))):
                    got[row["Setting"]] = row["Value"]
            s_, inv, cex = cfg["strategy"], cfg["inventory"], cfg["cex"]
            sig = lambda x: ("−" if Decimal(str(x)) < 0 else "+") + format(abs(Decimal(str(x))).normalize(), "f") + "σ"
            m, lev = Decimal(inv["capital"]), Decimal(inv["leverage"])
            want = {"Mean": "%s %sm" % (s_["mean_model"].upper(), s_["mean_minutes"]), "σ Window": "%sm" % s_["sigma_minutes"],
                    "Long In → Out": sig(-Decimal(str(s_["d_in"]))) + " → " + sig(-Decimal(str(s_["d_out"]))),
                    "Short In → Out": sig(s_["d_in"]) + " → " + sig(s_["d_out"]),
                    "Capital M": "{:,.0f}".format(m), "Leverage L": format(lev.normalize(), "f") + "×", "Notional N": "{:,.0f}".format(m * lev),
                    "Mode": "Live" if c["live"] else "Paper", "Order Channel": cex["order_transport"].upper(),
                    "Pricing Level": "%s / %s" % tuple("%d%s" % (n, {1: "st", 2: "nd", 3: "rd"}.get(n, "th")) for n in (
                        s_["pricing_level"]["bull"], s_["pricing_level"]["bear"])),
                    "Taker Fee": format(Decimal(str(s_["taker_fee_bps"])).normalize(), "f") + " bps", "Host": c["host"], "Run ID": run}
            if c["live"]:
                self.assertNotIn("Paper Latency", got, run)   # a live Run simulates nothing: the row is left out
            for k, v in want.items():
                self.assertEqual(got[k], v, "%s: %s" % (run, k))

    def test_only_the_picked_runs_rows(self):
        """Paper and live run side by side (Run 06 and Run 07 since 11:36:45): every second the Entry Answers table counts is
        one of the picked Run's own samples in the Range, and every round trip listed is one the Run wrote or decided on."""
        for run in self.runs():
            v = self.values(24 * 30, run)
            got = sum(r["Seconds"] for r in psql_json(fill(board.Q_IDLE, v)))
            want = psql_json(fill("WITH %s SELECT count(*) AS n FROM trade.signal_sample s WHERE s.run_id = '%s' AND s.ts_ms >= "
                                  "(SELECT rg.from_ms FROM rg) AND s.ts_ms < (SELECT rg.to_ms FROM rg)" % (board.SEL, run), v))[0]["n"]
            self.assertAlmostEqual(got, want, delta=5 if run in (self.runs()[-1], self.PAPER_RUN) else 0)   # a running Run adds seconds
            mine = {r["cycle_id"] for r in psql_json("SELECT cycle_id FROM trade.cycle WHERE run_id = '%s' UNION SELECT cycle_id FROM "
                                                     "trade.decision WHERE run_id = '%s' AND cycle_id IS NOT NULL" % (run, run))}
            self.assertLessEqual({r["cycle_id"] for r in psql_json(fill(board.Q_BLOTTER, v))}, mine, run)

    def test_last_second_is_the_newest_sample(self):
        """Health / Position / Spread / Entry and the Legs table show the picked Run's newest signal_sample (its last second)."""
        for run in ("__live", "__paper", self.runs()[0]):
            v = self.values(1, run)
            rid = psql_json(fill("WITH %s SELECT run_id, end_ms FROM sel" % board.SEL, v))[0]
            now = psql_json(fill(board.Q_NOW, v))[0]
            legs = psql_json(fill(board.Q_LEGS, v))
            s = psql_json("SELECT * FROM trade.signal_sample WHERE run_id = '%s' AND ts_ms <= %d ORDER BY ts_ms DESC LIMIT 1" % (rid["run_id"], rid["end_ms"]))[0]
            if now["As Of"] is None or utc8(s["ts_ms"]).strftime("%H:%M:%S") != now["As Of"]:
                continue   # a new second came in between the two reads; the next run checks it
            self.assertEqual(now["Spread"], Decimal(str(math.floor(float(s["mid_d"]) * 10 + 0.5) / 10)))
            self.assertEqual(now["Mean"], Decimal(str(math.floor(float(s["mu_d"]) * 10 + 0.5) / 10)) if s["mu_d"] is not None else None)
            self.assertEqual(float(now["F"]), float(s["f"]))
            self.assertEqual([l["Leg"] for l in legs], ["SOXL", "SOXS"])
            self.assertEqual(legs[0]["USD"], s["anchor_bull"])
            self.assertEqual(legs[1]["USD"], s["anchor_bear"])
            if s["mid_bull"] is None:
                self.assertIsNone(legs[0]["Mid"])
            else:
                self.assertAlmostEqual(float(legs[0]["Mid"]), float(s["mid_bull"]), delta=0.0005)   # to 0.001
            self.assertAlmostEqual(float(legs[1]["USDT"]), float(s["anchor_bear"]) / float(s["f"]), places=9)
            book = "Conflict" if s["conflict_bear"] else "Unusable" if not s["depth_ok_bear"] else "OK"
            self.assertEqual(legs[1]["Book"], book)
            side = "Flat" if s["direction"] is None else s["direction"].title()
            self.assertEqual(now["Side"], side)

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
            v = self.values(hours, self.PAPER_RUN)
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
            decisions = psql_json(fill("SELECT count(*) AS n FROM trade.decision d WHERE d.run_id = '%s' AND d.kind IN "
                                       "('entry', 'exit', 'complete', 'trim', 'forced') AND d.ts_ms >= $__from AND d.ts_ms <= $__to" % self.PAPER_RUN, v))[0]["n"]
            self.assertEqual(drawn, decisions, "%s h" % hours)

    def test_no_executable_distance_on_an_unusable_book(self):
        """Owner item 70 (the auditor's review): a sample whose leg's book conflicts (or whose depth is unusable) gives no
        distance to an entry line and no Buy Both / Sell Both point, as the executor enters on none. The real paper sample of
        2026-11-26 10:59:47.002 UTC+8: SOXS in conflict, Sell Both +2.0862σ past d_in 2: the distance reads — (it read At the
        Line); the same sample with SOXS's book usable reads At the Line; the chart's Sell Both there is no point but the code
        of Book Not OK, its raw deviation kept in the Entry strip's Sell Both."""
        ts = 1795661987002
        cols = "s.run_id, s.stats_ready, s.sigma_d, s.x_long_d, s.x_short_d, s.depth_ok_bull, s.depth_ok_bear, s.conflict_bull"
        q = ("SELECT %s AS d FROM (SELECT {cols}, {bear} AS conflict_bear FROM trade.signal_sample s WHERE s.live = false AND s.ts_ms = %d) s "
             "JOIN trade.process_run sr ON sr.run_id = s.run_id AND sr.live = false") % (board.distance(board.TO_SHORT), ts)
        sample = psql_json("SELECT conflict_bear, round((x_short_d / sigma_d)::numeric, 4) AS xs FROM trade.signal_sample "
                           "WHERE live = false AND ts_ms = %d" % ts)
        self.assertEqual(sample, [{"conflict_bear": True, "xs": Decimal("2.0862")}])
        self.assertEqual(psql_json(q.format(cols=cols, bear="s.conflict_bear")), [{"d": "—"}])
        self.assertEqual(psql_json(q.format(cols=cols, bear="false")), [{"d": "At the Line"}])
        v = dict(self.values(0.25, self.PAPER_RUN), __from=ts - 20 * 60000, __to=ts + 5 * 60000)
        row = [r for r in psql_json(fill(board.Q_EXECUTION, v)) if r["time"].startswith("2026-11-26T02:59:47") and r["Spread · Tooltip"] is not None]
        self.assertTrue(row, "the point of 10:59:47")
        self.assertIsNone(row[0]["Sell Both"])
        self.assertEqual(row[0]["Sell Both · Tooltip"], board.CODE["Book Not OK"])

    def test_pnl_and_round_trips_from_the_fills(self):
        """PRD V19: the Net and P&L figures and the Round Trips table against the fills (sells' value - buys' value of a flat
        cycle, fees, entry notional), recomputed here without cycle or lot; for every Run over its whole Range."""
        for run in self.runs():
            with self.subTest(run=run):
                self.pnl_of(run)

    def pnl_of(self, run):
        v = self.values(24 * 30, run)
        fills = {r["cycle_id"]: r for r in psql_json(
            "SELECT o.cycle_id, sum(CASE f.side WHEN 'sell' THEN f.value ELSE -f.value END) AS gross, sum(f.fee) AS fees, "
            "sum(f.value) FILTER (WHERE o.purpose IN ('entry', 'complete')) AS entry_value "
            "FROM trade.cex_fill f JOIN trade.cex_order o ON o.live = f.live AND o.client_order_id = f.client_order_id WHERE f.run_id = '%s' "
            "GROUP BY o.cycle_id" % run)}
        cycles = {r["cycle_id"]: r for r in psql_json("SELECT * FROM trade.cycle WHERE run_id = '%s'" % run)}
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
        known = [c for c in cycles.values() if c["status"] != "open" and c["funding_known"] and c["end_reason"] != "no_fill" and c["cycle_id"] in fills]
        p = psql_json(fill(board.Q_PNL, v))[0]
        net = psql_json(fill(board.Q_NET, v))
        self.assertEqual(-(p["Fees"] or 0), sum(fills[c["cycle_id"]]["fees"] for c in known))
        self.assertEqual(net[-1]["Net"], sum(fills[c["cycle_id"]]["gross"] - fills[c["cycle_id"]]["fees"] + c["funding"] for c in known))


if __name__ == "__main__":
    unittest.main()
