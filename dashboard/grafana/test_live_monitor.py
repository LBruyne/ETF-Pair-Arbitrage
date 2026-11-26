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
    for attempt in range(5):   # the shared database refuses a connection now and then when its slots are full: try again
        r = subprocess.run(["psql", "-X", "-q", "-At", "-v", "ON_ERROR_STOP=1", "-c", "SELECT coalesce(json_agg(row_to_json(t)), '[]') FROM (%s) t" % q],
                           capture_output=True, text=True, env=pg_env(DSN))
        if "remaining connection slots" not in r.stderr:
            break
        time.sleep(1 + attempt)
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
        self.assertEqual([t.split(" · ")[0] for t in titles], ["Status", "Execution Summary", "Position and Market", "Live Execution", "Round Trips",
                                                               "Round Trip Detail", "Inventory", "Settings", "Execution Statistics", "System"])
        self.assertEqual(titles[0], "Status · Running Session · ${runtext}")
        self.assertEqual([p.get("collapsed") for p in self.b["panels"] if p["type"] == "row"], [False] * 8 + [True, True])

    def test_status_first(self):
        """Owner items 74-75: the board starts with Status · Running Session holding Execution, Margin and Alerts, in the top
        fourteen grid units (no scrolling at 1680 px); Execution and Margin are word tiles coloured by STATUS_WORDS."""
        top, rows = [], 0
        for p in self.b["panels"]:
            rows += p["type"] == "row"
            if rows > 1:
                break
            top.append(p)
        self.assertEqual([p["title"] for p in top], ["Status · Running Session · ${runtext}", "Execution", "Margin", "Alerts"])
        self.assertLessEqual(max(p["gridPos"]["y"] + p["gridPos"]["h"] for p in top), 14)
        for p in top[1:3]:
            self.assertEqual(p["options"]["colorMode"], "background")
            self.assertEqual([m["options"]["pattern"] for m in p["fieldConfig"]["defaults"]["mappings"]], [w for w, _ in board.STATUS_WORDS])

    def test_every_word_has_its_colour(self):
        """Each word the Status tiles can show takes the colour of what it means (the first STATUS_WORDS pattern it matches):
        the program's entry answers by their state (ready / order sent green, the strategy's ordinary answers grey, held back
        amber, a person needed red), the rest as listed."""
        def colour(word):
            for pattern, c in board.STATUS_WORDS:
                if re.match(pattern, word):
                    return c
        state = {c: s for c, s, _ in board.IDLE}
        want = {"Ready": board.WELL, "Blocked": board.GREY, "Held Back": board.WARN, "Needs a Person": board.BAD}
        for code, word in board.ENTRY_WORDS.items():
            w = word.replace("$bull", "SOXL").replace("$bear", "SOXS")
            self.assertEqual(colour(w), want[state[code]], "%s: %s" % (code, w))
        self.assertEqual(colour("order sent"), board.WELL)
        nb = board.NBSP
        for word, c in [("LIVE", "#8a1f1f"), ("PAPER", "#5a5953"), ("up 2h" + nb + "05m", board.WELL), ("stale · 25s", board.BAD), ("lost", board.BAD),
                        ("starting", board.WARN), ("stopped", board.GREY), ("11‑26" + nb + "12:57", "transparent"), ("off hours", board.WELL),
                        ("us regular", board.GREY), ("close window", board.WARN), ("in 1d" + nb + "12h", "transparent"), ("passed", board.GREY),
                        ("nav", board.WELL), ("bell", board.WELL), ("missing", board.WARN), ("ok", board.WELL), ("SOXS conflict", board.WARN),
                        ("SOXL unusable · SOXS conflict", board.WARN), ("none open", board.WELL), ("2 working", board.WELL), ("1 unknown", board.BAD),
                        ("2 left open", board.BAD), ("flat", board.GREY), ("short · 55m", board.WELL), ("long left open", board.BAD),
                        ("match", board.WELL), ("mismatch", board.BAD), ("not read", board.WARN), ("5,586.40 U", "transparent"), ("paper", board.GREY),
                        ("ok · 0.23 %", board.WELL), ("warn · 35.00 %", board.WARN), ("high · 70.00 %", board.BAD), ("ok · SOXL 1,386 %", board.WELL),
                        ("warn · SOXS 20 %", board.WARN), ("near · SOXL 8 %", board.BAD), ("76 % used", "transparent"), ("+0.22 U", "transparent"),
                        ("−1.05 U", "transparent"), ("2 awaited", board.WARN), ("no data", board.GREY), ("no run", board.GREY)]:
            self.assertEqual(colour(word), c, word)

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
        self.assertEqual(psql_json(fill(board.Q_STATUS_EXEC, dict(self.values(1), combo="SOXL / SOXX")))[0]["Mode"], "no run")
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
            running = psql_json("SELECT stopped_ms FROM trade.process_run WHERE run_id = '%s'" % run)[0]["stopped_ms"] is None
            self.assertAlmostEqual(got, want, delta=5 if running else 0)   # a running Run adds seconds between the two reads
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

    # ------------------------------------------------------------------------------------- Status · Running Session
    def raw_run(self, run):
        """The picked Run's own records the Status row reads, straight from the tables."""
        r = psql_json("SELECT * FROM trade.process_run WHERE run_id = '%s'" % run)[0]
        end = r["stopped_ms"] or int(time.time() * 1000)
        s = psql_json("SELECT * FROM trade.signal_sample WHERE run_id = '%s' AND live = %s AND ts_ms <= %d AND ts_ms > %d ORDER BY ts_ms DESC LIMIT 1"
                      % (run, r["live"], end, end - 172800000))
        i = psql_json("SELECT * FROM trade.inventory_sample WHERE run_id = '%s' AND live = %s AND ts_ms <= %d AND ts_ms > %d ORDER BY ts_ms DESC LIMIT 1"
                      % (run, r["live"], end, end - 172800000))
        cycles = psql_json("SELECT * FROM trade.cycle c WHERE c.live = %s AND (c.run_id = '%s' OR c.cycle_id IN (SELECT d.cycle_id FROM trade.decision d "
                           "WHERE d.run_id = '%s' AND d.cycle_id IS NOT NULL))" % (r["live"], run, run))
        open_lots = psql_json("SELECT DISTINCT l.cycle_id FROM trade.lot l WHERE l.live = %s AND l.status IN ('building', 'open')" % r["live"])
        orders = psql_json("SELECT * FROM trade.cex_order WHERE run_id = '%s' AND live = %s" % (run, r["live"]))
        return r, (s or [None])[0], (i or [None])[0], cycles, {x["cycle_id"] for x in open_lots}, orders

    def expected_status(self, run):
        r, s, i, cycles, open_lots, orders = self.raw_run(run)
        nb, stopped = board.NBSP, r["stopped_ms"] is not None
        hm = lambda ms: utc8(ms).strftime("%m-%d %H:%M").replace("-", board.NBHY).replace(" ", nb)
        held = [c for c in cycles if c["status"] == "open" and c["cycle_id"] in open_lots]
        tick = {"bull": "SOXL", "bear": "SOXS"}
        books = [("%s conflict" % tick[l]) if s["conflict_" + l] else ("%s unusable" % tick[l]) if not s["depth_ok_" + l] else None
                 for l in ("bull", "bear")] if s else []
        working = [o for o in orders if o["unknown"] or o["status"] in ("pending", "acked")]
        ex = {"Mode": "LIVE" if r["live"] else "PAPER", "Start / End": hm(r["stopped_ms"] or r["started_ms"]),
              "Phase": "stopped" if stopped else {"off_hours": "off hours", "regular": "us regular", "close_window": "close window"}[s["phase"]],
              "Anchor": "stopped" if stopped else s["anchor_kind"] or "missing",
              "Books": "stopped" if stopped else " · ".join(b for b in books if b) or "ok",
              "Orders": "none open" if not working else "%d left open" % len(working) if stopped else
                        "%d unknown" % sum(o["unknown"] for o in working) if any(o["unknown"] for o in working) else "%d working" % len(working),
              "Entry": "stopped" if stopped else "order sent" if s["idle_reason"] is None else
                       board.ENTRY_WORDS[s["idle_reason"]].replace("$bull", "SOXL").replace("$bear", "SOXS")}
        pct = lambda x, q: str(Decimal(x).quantize(Decimal(q), rounding="ROUND_HALF_UP"))
        awaited = sum(len(c["funding_awaited"]) for c in cycles if isinstance(c["funding_awaited"], list))
        booked = sum((c["funding"] for c in cycles if c["funding_known"]), Decimal(0))
        mg = {"Cycle": "flat" if not held else ("%s left open" % held[0]["direction"]) if stopped else held[0]["direction"],
              "Positions": "mismatch" if i["mismatch"] else "not read" if i["pos_bull"] is None or i["pos_bear"] is None else "match",
              "Equity": "paper" if not r["live"] else "stopped" if stopped else "{:,.2f} U".format(i["equity"]),
              "Margin Ratio": "paper" if not r["live"] else "stopped" if stopped else "%s · %s %%" % (
                  "ok" if i["margin_ratio"] < Decimal("0.3") else "warn" if i["margin_ratio"] < Decimal("0.6") else "high", pct(100 * i["margin_ratio"], "0.01")),
              "Quota": "stopped" if stopped else "%s %% used" % pct(100 * i["quota_used"] / i["quota_total"], "1"),
              "Funding": "%d awaited" % awaited if awaited else "%s%s U" % ("−" if booked < 0 else "+", "{:,.2f}".format(abs(booked)))}
        if r["live"] and not stopped:
            legs = [(l, (i["liq_" + l] - i["mark_" + l]) / i["mark_" + l] if i["pos_" + l] < 0 else (i["mark_" + l] - i["liq_" + l]) / i["mark_" + l])
                    for l in ("bull", "bear") if i["pos_" + l] and i["liq_" + l] is not None]
            if not i["pos_bull"] and not i["pos_bear"]:
                mg["Liq Distance"] = "flat"
            elif legs:
                l, d = min(legs, key=lambda x: x[1])
                mg["Liq Distance"] = "%s · %s %s %%" % ("near" if d < Decimal("0.1") else "warn" if d < Decimal("0.25") else "ok", tick[l], "{:,.0f}".format(100 * d))
        else:
            mg["Liq Distance"] = "paper" if not r["live"] else "stopped"
        return r, s, i, ex, mg

    def test_status_words_from_the_schema(self):
        """Every Execution and Margin word of every Run against its own records read straight from the tables (process_run, its
        newest signal_sample and inventory_sample, its orders not final, its open round trip, its funding): Mode, Start / End,
        Phase, Anchor, Books, Orders, Entry; Cycle, Positions, Equity, Margin Ratio, Liq Distance, Quota, Funding; Run as
        stopped or up / stale / lost. A running Run is read again until no new sample came in between."""
        for run in self.runs():
            for _ in range(5):
                r, s, i, ex, mg = self.expected_status(run)
                v = self.values(1, run)
                got = psql_json(fill(board.Q_STATUS_EXEC, v))[0], psql_json(fill(board.Q_STATUS_MARGIN, v))[0]
                if self.expected_status(run)[1:3] == (s, i):
                    break
            with self.subTest(run=run):
                for k, want in ex.items():
                    self.assertEqual(got[0][k], want, k)
                for k, want in mg.items():
                    self.assertEqual(got[1][k], want, k)
                if r["stopped_ms"]:
                    self.assertEqual(got[0]["Run"], "stopped")
                    self.assertEqual((got[0]["Close"], got[1]["Open P&L"]), ("stopped", "stopped"))
                else:
                    self.assertRegex(got[0]["Run"], r"^(up |stale · |lost$|starting$)")
                    self.assertRegex(got[0]["Close"], r"^(in |passed$)")

    def expected_alerts(self, run):
        """The Alerts of a Run recomputed in Python from its records (SPEC 7, Alerts)."""
        r, s, i, cycles, open_lots, orders = self.raw_run(run)
        running = r["stopped_ms"] is None
        tick = {"bull": "SOXL", "bear": "SOXS"}
        hms = lambda ms: utc8(ms).strftime("%H:%M:%S")
        words = {c: w.replace("Bull ", "SOXL ").replace("Bear ", "SOXS ") for c, _, w in board.IDLE}
        cfg = r["config"] if isinstance(r["config"], dict) else json.loads(r["config"])
        d_in = float(cfg["strategy"]["d_in"])
        samples = psql_json("SELECT ts_ms, idle_reason, stats_ready, x_long_d, x_short_d, sigma_d FROM trade.signal_sample WHERE run_id = '%s' AND live = %s "
                            "ORDER BY ts_ms" % (run, r["live"]))
        out, eps, prev = [], [], None
        for x in samples:
            sig = float(x["sigma_d"]) if x["sigma_d"] else None
            xl, xs = x["x_long_d"], x["x_short_d"]   # SQL's NULL compares as unknown: only a known side counts
            at = bool(x["stats_ready"] and sig and ((xl is not None and float(xl) / sig <= -d_in) or (xs is not None and float(xs) / sig >= d_in)))
            new = prev is None or x["idle_reason"] != prev["idle_reason"] or x["ts_ms"] - prev["ts_ms"] > 3000
            if prev is not None and x["ts_ms"] - prev["ts_ms"] > board.GAP_MS:
                out.append((prev["ts_ms"], "serious", "Recovered", "Run", "Sample Gap", None))
            if x["idle_reason"] in board.HELD_CODES:
                if new:
                    eps.append({"code": x["idle_reason"], "t0": x["ts_ms"], "n": 0, "at": 0})
                eps[-1]["n"] += 1
                eps[-1]["at"] += at
            prev = x
        for code in {e["code"] for e in eps}:
            mine = [e for e in eps if e["code"] == code]
            n, at = sum(e["n"] for e in mine), sum(e["at"] for e in mine)
            crit = code in board.CRITICAL_CODES
            status = ("Open" if crit else "In Progress") if running and s and s["idle_reason"] == code else "Recovered"
            detail = "%d %s since %s, %d s in all, longest %d s%s" % (len(mine), "time" if len(mine) == 1 else "times", hms(mine[0]["t0"]), n,
                                                                       max(e["n"] for e in mine), "; %d s of them past an entry line" % at if at else "")
            out.append((mine[-1]["t0"], "critical" if crit else "warning", status, "Entry", words[code], detail))
        inv = psql_json("SELECT ts_ms, mismatch FROM trade.inventory_sample WHERE run_id = '%s' AND live = %s ORDER BY ts_ms" % (run, r["live"]))
        for k, x in enumerate(inv):
            if x["mismatch"] and (k == 0 or not inv[k - 1]["mismatch"]):
                last = next((y for y in inv[k:] if not y["mismatch"]), None)
                out.append((x["ts_ms"], "critical", "Open" if last is None and running else "Recovered", "Positions", "Position Mismatch", None))
        what = {"one_leg": "One Leg Only", "stop": "Stopped While Holding", "session_changed": "Session Ended While Holding", "store_error": "Not Recorded"}
        for c in cycles:
            dust = c["dust_bull"] != 0 or c["dust_bear"] != 0
            if c["end_reason"] in what or c["end_reason"] == "dust_only" or dust:
                out.append((c["closed_ms"] or c["opened_ms"], "serious" if c["end_reason"] == "one_leg" else "warning",
                            "Recorded" if c["end_reason"] == "dust_only" or dust else "Recovered", "Round Trip " + hms(c["opened_ms"]),
                            what.get(c["end_reason"], "Dust Left"), None))
            if isinstance(c["funding_awaited"], list) and c["funding_awaited"]:
                out.append((c["closed_ms"], "warning", "In Progress" if running else "Open", "Round Trip " + hms(c["opened_ms"]), "Funding Awaited", None))
        by_id = {c["cycle_id"]: c for c in psql_json("SELECT * FROM trade.cycle WHERE live = %s" % r["live"])}
        for d in psql_json("SELECT * FROM trade.decision WHERE run_id = '%s' AND live = %s AND kind = 'forced'" % (run, r["live"])):
            c = by_id.get(d["cycle_id"])
            out.append((d["ts_ms"], "serious", "In Progress" if c and c["status"] == "open" and running else "Recovered",
                        "Round Trip " + (hms(c["opened_ms"]) if c else "–"), "Forced Exit", None))
        for o in orders:
            if o["unknown"] or o["not_found"] or o["status"] in ("rejected", "not_sent", "never_arrived"):
                w = ("Order Outcome Unknown" if o["unknown"] else "Order Not Found" if o["not_found"] else "Order Rejected" if o["status"] == "rejected"
                     else "Order Not Sent" if o["status"] == "not_sent" else "Order Never Arrived")
                out.append((o["submitted_ms"], "warning" if o["status"] == "not_sent" else "serious",
                            ("In Progress" if running else "Open") if o["unknown"] else "Recovered", "Order · " + tick[o["leg"]], w, None))
        for e in psql_json("SELECT * FROM trade.event WHERE run_id = '%s' AND live = %s AND level IN ('error', 'warn')" % (run, r["live"])):
            out.append((e["ts_ms"], "critical" if e["level"] == "error" else "warning", "Open", "Program · " + e["module"],
                        e["kind"].replace("_", " ").title(), e["message"]))
        if running and s is None or running and int(time.time() * 1000) - s["ts_ms"] > board.SAMPLE_AGE_S * 1000:
            out.append((None, "critical", "Open", "Run", None, None))
        return r, out

    def test_alerts_from_the_schema(self):
        """Every Alerts row of every Run against a recomputation from its records in Python: each Held Back / Needs a Person
        reason as one row (how often, seconds in all, longest, the seconds past an entry line, from its first time), sample
        gaps, mismatch episodes, round trips ended on one leg / on a stop / with dust / waiting for funding, forced exits, orders
        rejected or unknown, the program's events; Severity, Status and Source of each; the unfinished first, then the newest.
        A stopped Run's rows are compared whole; a running Run's (its newest seconds move) by everything but the Entry rows'
        counts."""
        for run in self.runs():
            with self.subTest(run=run):
                r, want = self.expected_alerts(run)
                got = psql_json(fill(board.Q_ALERTS, self.values(1, run)))
                if not want:
                    self.assertEqual([(g["Detail"], g["What"]) for g in got], [("No alert in this Run: nothing stopped or held trading", None)])
                    continue
                rank = {"Open": 0, "In Progress": 1}
                self.assertEqual([rank.get(g["Status"], 2) for g in got], sorted(rank.get(g["Status"], 2) for g in got), "unfinished first")
                key = lambda x: (x[1], x[2], x[3], x[4]) if x[4] is not None else (x[1], x[2], x[3])
                got_rows = [(round(datetime.fromisoformat(g["time"]).timestamp() * 1000), g["Severity"], g["Status"], g["Source"], g["What"], g["Detail"])
                            for g in got]
                self.assertEqual(sorted(map(key, got_rows), key=str), sorted(map(key, want), key=str))
                if r["stopped_ms"]:
                    for w in want:
                        match = [g for g in got_rows if g[1:5] == w[1:5] and g[0] == w[0]]
                        self.assertTrue(match, w)
                        if w[5] is not None:
                            self.assertEqual(match[0][5], w[5], w[4])

    def test_positions_now_are_the_newest_inventory_sample(self):
        """Inventory · Positions Now shows each leg of the Run's newest inventory_sample: the venue's and the ledger's position,
        mark, liquidation price, the venue's unrealised P&L, and the distance to liquidation in % of the mark on its side."""
        for run in self.runs():
            for _ in range(5):
                r, s, i, *_ = self.raw_run(run)
                got = psql_json(fill(board.Q_POSITIONS, self.values(1, run)))
                if self.raw_run(run)[2] == i:
                    break
            with self.subTest(run=run):
                self.assertEqual([g["Leg"] for g in got], ["SOXL", "SOXS"])
                for g, l in zip(got, ("bull", "bear")):
                    self.assertEqual((g["Venue"], g["Ledger"], g["Mark"], g["Liq Price"], g["Venue UPL"]),
                                     (i["pos_" + l], i["ledger_" + l], i["mark_" + l], i["liq_" + l], i["upl_" + l]))
                    pos, mark, liq = i["pos_" + l], i["mark_" + l], i["liq_" + l]
                    want = None if not pos or mark is None or liq is None else 100 * ((liq - mark) if pos < 0 else (mark - liq)) / mark
                    self.assertEqual(g["Liq Distance"], want)
                    self.assertEqual(g["Age"] is None, r["stopped_ms"] is not None)

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
            "sum(f.value) FILTER (WHERE o.purpose IN ('entry', 'complete')) AS entry_value, count(*) AS fills "
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
                # Bitget reports an order's cumulative value and fee rounded on their own, not as the sum of its fills' rounded
                # ones: the live round trip c1795665649133 booked 359.9798 for an exit whose three fills sum to 359.9797. So a
                # live Net may differ from the fills' by up to 0.0001 USDT per fill (value and fee); paper's match exactly.
                tol = Decimal("0.0002") * f["fills"] if c["live"] else 0
                self.assertAlmostEqual(row["Net"], f["gross"] - f["fees"] + c["funding"], delta=tol, msg=row["cycle_id"])
                self.assertAlmostEqual(float(row["Net bps"]), float(row["Net"] / row["Notional"] * 10000), places=6)
                checked += 1
        known = [c for c in cycles.values() if c["status"] != "open" and c["funding_known"] and c["end_reason"] != "no_fill" and c["cycle_id"] in fills]
        p = psql_json(fill(board.Q_PNL, v))[0]
        net = psql_json(fill(board.Q_CUM_NET, v))
        want = sum(fills[c["cycle_id"]]["gross"] - fills[c["cycle_id"]]["fees"] + c["funding"] for c in known)
        tol = sum(Decimal("0.0002") * fills[c["cycle_id"]]["fills"] for c in known if c["live"])
        self.assertAlmostEqual(-(p["Fees"] or 0), sum(fills[c["cycle_id"]]["fees"] for c in known), delta=tol)
        self.assertAlmostEqual(p["Net"], want, delta=tol)
        self.assertEqual(net[-1]["Net"], p["Net"])
        self.assertEqual(p["Trips"], len([c for c in cycles.values() if c["status"] != "open" and c["end_reason"] != "no_fill"]))
        points = psql_json(fill(board.Q_TRIP_GAIN, v)) + psql_json(fill(board.Q_TRIP_LOSS, v))
        self.assertEqual(sorted(r["cycle_id"] for r in points), sorted(c["cycle_id"] for c in known))


if __name__ == "__main__":
    unittest.main()
