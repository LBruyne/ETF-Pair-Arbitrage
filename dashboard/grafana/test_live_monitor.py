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
from datetime import datetime, time as clock_time, timedelta, timezone
from zoneinfo import ZoneInfo
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


def entry_in(cfg):
    """The Run's entry line: strategy.entry_in, d_in before PRD 4.0 (core/SPEC.md 12.8)."""
    return cfg["strategy"].get("entry_in", cfg["strategy"].get("d_in"))


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
        self.assertEqual([t.split(" · ")[0] for t in titles], ["Status", "Execution Summary", "Position", "Live Execution", "Round Trips",
                                                               "Round Trip Detail", "Every Order", "Inventory", "Settings", "Execution Statistics", "System"])
        self.assertEqual(titles[0], "Status · Running Session · ${runtext}")
        self.assertEqual([p.get("collapsed") for p in self.b["panels"] if p["type"] == "row"], [False] * 6 + [True] + [False] * 2 + [True, True])
        # 4.5: every order of a round trip only in the folded row (queried while it is open), the visible Orders folds the runs
        every = next(p for p in self.b["panels"] if p["type"] == "row" and p["title"].startswith("Every Order"))
        self.assertEqual([q["targets"][0]["rawSql"] for q in every["panels"]], [board.Q_ORDERS_EVERY])

    def test_status_first(self):
        """Owner items 74-76: the board starts with Status · Running Session holding Execution, Account, Market (two strips, as
        Combo Spread's) and Alerts, in the top twenty grid units; Execution and Account are word tiles coloured by STATUS_WORDS."""
        top, rows = [], 0
        for p in self.b["panels"]:
            rows += p["type"] == "row"
            if rows > 1:
                break
            top.append(p)
        self.assertEqual([p["title"] for p in top], ["Status · Running Session · ${runtext}", "Execution", "Account", "Market · Prices",
                                                     "Market · Real Spread and Signal", "Alerts"])
        self.assertLessEqual(max(p["gridPos"]["y"] + p["gridPos"]["h"] for p in top), 21)
        self.assertNotIn('AS "Close"', board.Q_STATUS_EXEC)   # owner item 76: no Close
        self.assertNotIn('AS "Anchor"', board.Q_STATUS_EXEC)   # the Anchor is in Market
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
        want = {"Ready": board.WELL, "Blocked": board.GREY, "Pairing": board.WARN, "Held Back": board.WARN, "Needs a Person": board.BAD}
        for code, word in board.ENTRY_WORDS.items():
            w = word.replace("$bull", "SOXL").replace("$bear", "SOXS")
            self.assertEqual(colour(w), want[state[code]], "%s: %s" % (code, w))
        self.assertEqual(colour("order sent"), board.WELL)
        nb = board.NBSP
        for word, c in [("LIVE", "#8a1f1f"), ("PAPER", "#5a5953"), ("up 2h" + nb + "05m", board.WELL), ("stale · 25s", board.BAD), ("lost", board.BAD),
                        ("starting", board.WARN), ("stopped", board.GREY), ("11‑26" + nb + "12:57", "transparent"),
                        ("Pre‑Mkt", "#b88a0a"), ("Regular", board.GREY), ("After‑Hrs", "#d55181"), ("Overnight", "#4f8fe6"), ("Closed", board.GREY),
                        ("residual · SOXL +0.01", board.WARN), ("residual · SOXL −0.01 SOXS +0.02", board.WARN), ("13.01 U", "transparent"),
                        ("ok", board.WELL), ("SOXS conflict", board.WARN),
                        ("SOXL unusable · SOXS conflict", board.WARN), ("none open", board.WELL), ("2 working", board.WELL), ("1 unknown", board.BAD),
                        ("2 left open", board.BAD), ("flat", board.GREY), ("short · 55m", board.WELL), ("long left open", board.BAD),
                        ("match", board.WELL), ("mismatch", board.BAD), ("not read", board.WARN), ("5,586.40 U", "transparent"), ("paper", board.GREY),
                        ("ok · 0.23 %", board.WELL), ("warn · 35.00 %", board.WARN), ("high · 70.00 %", board.BAD), ("ok · SOXL 1,386 %", board.WELL),
                        ("warn · SOXS 20 %", board.WARN), ("near · SOXL 8 %", board.BAD), ("+0.22 U", "transparent"),
                        ("−1.05 U", "transparent"), ("2 awaited", board.WARN), ("no data", board.GREY), ("no run", board.GREY)]:
            self.assertEqual(colour(word), c, word)

    def test_phase_as_combo_spread(self):
        """Owner item 76: the Phase tile in Combo Spread's words and colours (combo_spread_board.PHASE and PHASE_TEXT)."""
        import combo_spread_board as combo
        words = {name: o.get("text", name) for name, o in combo.PHASE["options"].items()}
        self.assertEqual(board.PHASE_WORDS, words)
        self.assertEqual(board.PHASE_COLORS, dict(combo.PHASE_TEXT, Closed=combo.PHASE["options"]["Closed"]["color"]))
        for name, word in board.PHASE_WORDS.items():
            self.assertIn(("^%s$" % word, board.PHASE_COLORS[name]), board.STATUS_WORDS)

    def test_execution_charts(self):
        """Owner items 80, 81: two charts, Spread · bps and Deviation · bps, after Combo Spread's; one frame (the Deviation chart
        shows the Spread chart's result); no plain spread line on Spread (the Real Long / Short Spread lines); every tooltip row is
        a tooltip-only series (a drawn line is never in the tooltip), the tooltips hide zeros; the decision rows come last but
        each chart's own At; each long entry is a green triangle (a filled band), each short entry a red one, exits circles;
        every frame column is kept by one chart or the other; a legend holds at most eight series."""
        spread = next(p for p in self.b["panels"] if p["title"] == "Spread · bps")
        dev = next(p for p in self.b["panels"] if p["title"] == "Deviation · bps")
        self.assertEqual(len(spread["targets"]), 1)
        self.assertEqual(dev["targets"][0]["panelId"], spread["id"])
        kept = set()
        for p in (spread, dev):
            self.assertTrue(p["options"]["tooltip"]["hideZeros"])
            names = p["transformations"][0]["options"]["include"]["names"]
            kept |= set(names)
            self.assertNotIn("Spread", names)   # no line on the mids' spread
            props = {o["matcher"]["options"]: {x["id"]: x["value"] for x in o["properties"]} for o in p["fieldConfig"]["overrides"]}
            for name, pr in props.items():
                hide = pr.get("custom.hideFrom", {})
                if not hide.get("viz") and hide.get("tooltip") is not True:
                    self.fail("%s: drawn series %s in the tooltip" % (p["title"], name))
            chart = "" if p is spread else " · Dev"
            self.assertEqual(props["Long Entry" + chart]["color"]["fixedColor"], board.GOOD)
            self.assertEqual(props["Long Entry" + chart]["custom.fillBelowTo"], "Long Entry · Base" + chart)
            self.assertEqual(props["Short Entry" + chart]["color"]["fixedColor"], board.BAD)
            # owner item 84: exits are circles in the colour of the position they close; undone entries grey triangles; the
            # stretch past an entry line drawn thick; Combo Spread's σ lines 0.5-3σ, grey, in neither legend nor tooltip;
            # no line on the mids on Deviation.
            self.assertEqual((props["Long Exit" + chart]["custom.drawStyle"], props["Long Exit" + chart]["color"]["fixedColor"]), ("points", board.GOOD))
            self.assertEqual((props["Short Exit" + chart]["custom.drawStyle"], props["Short Exit" + chart]["color"]["fixedColor"]), ("points", board.BAD))
            # 4.4: a failed entry (the thin leg filled nothing) the grey triangle, an unwound one amber; the second exit line drawn
            self.assertTrue(props["Failed Long Entry" + chart]["color"]["fixedColor"].startswith(board.GREY))
            self.assertEqual(props["Unwound Long Entry" + chart]["color"]["fixedColor"], board.UNWOUND)
            self.assertEqual(props["Unwound Long Entry" + chart]["custom.fillBelowTo"], "Unwound Long Entry · Base" + chart)
            self.assertNotEqual(board.UNWOUND, board.GREY)
            self.assertIn("Long Out 2" + chart, names)
            self.assertNotIn("Deviation", names)
            self.assertNotIn("Other" + chart, names)
            sig = [n for n in names if n.endswith("σ")]
            self.assertEqual(len(sig), 12, sig)
            for n in sig:
                self.assertEqual(props[n]["custom.hideFrom"], {"legend": True, "tooltip": True, "viz": False})
            at = [n for n in names if n.endswith("at Entry")]
            self.assertEqual(len(at), 2)
            for n in at:
                self.assertEqual(props[n]["custom.lineWidth"], 4)
            in_legend = [n for n in p["transformations"][1]["options"]["indexByName"] if n != "time" and not (props.get(n, {}).get("custom.hideFrom", {}) or {}).get("legend")]
            self.assertLessEqual(len(in_legend), 11, (p["title"], in_legend))
        self.assertEqual(set(n for n, _ in board.ALL_COLUMNS) - kept, set())

    def test_no_spread_unit_d(self):
        """Owner item 80: no "D" unit on the board: every spread figure in bps (titles, descriptions, units, tooltips, the texts
        the queries write); F labelled USD/USDT."""
        text = json.dumps(self.b, ensure_ascii=False)
        for m in re.finditer(r"(?<![A-Za-z_\\$])D(?![A-Za-z_0-9])", text):
            self.fail("a D at: %s" % text[max(0, m.start() - 60):m.end() + 20])
        self.assertIn("F · USD/USDT", text)
        self.assertNotIn("PnL per Round Trip", text)
        self.assertNotIn("Entry · σ", text)

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
        """The Run options: Newest Live Run, Newest Paper Run, then every Run newest first, named start–end · Bitget · pair · mode
        without a number (owner item 76); Newest resolves to the newest Run of that mode; a wrong Combo or an unknown Run shows
        nothing (No Run)."""
        runs = psql_json("SELECT run_id, live, started_ms, stopped_ms FROM trade.process_run ORDER BY started_ms, run_id")
        opts = psql_json(fill(board.Q_RUN_OPTIONS, self.values(1)))
        heads = [o["__value"] for o in opts[:2]]
        self.assertEqual(heads, [x for x, live in (("__live", True), ("__paper", False)) if any(r["live"] == live for r in runs)])
        rest = opts[len(heads):]
        self.assertEqual([o["__value"] for o in rest], [r["run_id"] for r in reversed(runs)])
        md = lambda ms: utc8(ms).strftime("%m-%d %H:%M").replace("-", board.NBHY).replace(" ", board.NBSP)
        for r in runs:
            text = next(o["__text"] for o in rest if o["__value"] == r["run_id"])
            end = "now" if not r["stopped_ms"] else md(r["stopped_ms"])[6:] if md(r["stopped_ms"])[:5] == md(r["started_ms"])[:5] else md(r["stopped_ms"])
            want = "%s–%s · Bitget · SOXL/SOXS · %s" % (md(r["started_ms"]), end, "Live" if r["live"] else "Paper")
            self.assertIn(text, (want, want + " · lost", want + " · starting"))
            self.assertNotRegex(text, r"Run \d")
        self.assertTrue(opts[0]["__text"].startswith("Newest Live Run · ") and opts[0]["__text"].endswith(" · Live"))
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
            for moved in ("Mean", "σ Window", "Long In → Out", "Short In → Out", "Capital M", "Leverage L", "Notional N"):
                self.assertNotIn(moved, got, run)   # owner item 80: in Parameters, not twice
            par = psql_json(fill(board.Q_PARAMS, self.values(1, run)))[0]
            sigma = psql_json("SELECT sigma_d FROM trade.signal_sample WHERE run_id = '%s' ORDER BY ts_ms DESC LIMIT 1" % run)
            sigma = sigma[0]["sigma_d"] if sigma else None
            num = lambda x: format(Decimal(str(x)).normalize(), "f").replace("-", "−")
            bps = lambda d: ("{:,.1f}".format(Decimal(str(d)) * Decimal(str(sigma))) + board.NBSP + "bps").replace("-", "−") if sigma is not None else "–" + board.NBSP + "bps"
            # PRD 4.0 renamed d_in / d_out to entry_in / entry_out: a Run before it keeps the old keys and shows the same
            d_in, d_out, x2 = s_.get("entry_in", s_.get("d_in")), s_.get("entry_out", s_.get("d_out")), s_.get("exit2")
            if psql_json("SELECT stopped_ms FROM trade.process_run WHERE run_id = '%s'" % run)[0]["stopped_ms"]:   # a running Run's σ moves
                self.assertEqual(par["Entry"], "%sσ · %s" % (num(d_in), bps(d_in)), run)
                self.assertEqual(par["Exit 1"], "%sσ · %s" % (num(d_out), bps(d_out)), run)
                self.assertEqual(par["Exit 2"], "%sσ · %s" % (num(x2), bps(x2)) if x2 is not None else "none", run)
            ex = cfg.get("exec", {})
            pair = {k: par[k] for k in ("Thin Leg", "Pair Window", "Pair Stop", "Top-Up Max", "Exit Pair Window")}
            if ex.get("thin_leg"):
                self.assertEqual(pair["Thin Leg"], cfg["cex"]["pair"][ex["thin_leg"]]["ticker"] + board.NBSP + "first", run)
                self.assertEqual((pair["Pair Window"], pair["Exit Pair Window"]), (ex["pair_window"], ex["exit_pair_window"]), run)
                self.assertEqual(pair["Pair Stop"], num(ex["pair_stop_d"]) + board.NBSP + "bps", run)
                self.assertEqual(pair["Top-Up Max"], num(ex["thin_topup_max"]) + board.NBSP + "×", run)
            else:
                self.assertEqual(list(pair.values()), ["both" + board.NBSP + "together", "—", "—", "—", "—"], run)
            self.assertEqual((par["Mean Model"], par["Volatility Scale"]), ("%s · %s%smin" % (s_["mean_model"].upper(), s_["mean_minutes"], board.NBSP),
                                                                            "%s%smin" % (s_["sigma_minutes"], board.NBSP)), run)
            self.assertEqual((par["Capital M"], par["Leverage L"], par["Notional N"]), (m, lev, m * lev), run)
            want = {"Mode": "Live" if c["live"] else "Paper", "Order Channel": cex["order_transport"].upper(),
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
            self.assertLessEqual({r["cycle_id"] for r in psql_json(fill(board.Q_BLOTTER, v)) if r["cycle_id"]}, mine, run)

    def test_market_is_the_newest_sample(self):
        """Market and Position show the picked Run's newest signal_sample (its last second): both legs' mid and anchor, the
        anchor's kind, Spread, Mean, σ, F, and the Signal in Combo Spread's words on the executable deviations (No Book on a
        conflicting or unusable book, owner item 70)."""
        for run in ["__live", "__paper"] + self.runs():
            v = self.values(1, run)
            rid = psql_json(fill("WITH %s SELECT run_id, end_ms, config FROM sel" % board.SEL, v))[0]
            newest = "SELECT * FROM trade.signal_sample WHERE run_id = '%s' AND ts_ms <= %d ORDER BY ts_ms DESC LIMIT 1" % (rid["run_id"], rid["end_ms"])
            s = psql_json(newest)[0]
            m = psql_json(fill(board.Q_MARKET, v))[0]
            now = psql_json(fill(board.Q_NOW, v))[0]
            if psql_json(newest)[0]["ts_ms"] != s["ts_ms"]:
                continue   # a new second came in between the reads
            one = lambda x: Decimal(str(math.floor(float(x) * 10 + 0.5) / 10)) if x is not None else None
            fmt = lambda x: ("{:,.1f}".format(Decimal(str(x)).quantize(Decimal("0.1"), rounding="ROUND_HALF_UP") + 0) + board.NBSP + "bps").replace("-", "−")
            self.assertEqual((m["Mean"], m["Volatility"]), (fmt(s["mu_d"]) if s["mu_d"] is not None else None, one(s["sigma_d"])))
            cfg = rid["config"] if isinstance(rid["config"], dict) else json.loads(rid["config"])
            ok = s["depth_ok_bull"] and s["depth_ok_bear"] and not (s["conflict_bull"] or s["conflict_bear"])
            if s["stats_ready"]:
                rl = fmt(float(s["mu_d"]) + float(s["x_long_d"])) if s["x_long_d"] is not None else "–"
                rs = fmt(float(s["mu_d"]) + float(s["x_short_d"])) if s["x_short_d"] is not None else "–"
                if ok:   # owner item 84: the side at or past its entry line marked (and lit by the tile's mapping)
                    d_in_, sig_ = float(entry_in(cfg)), float(s["sigma_d"])
                    hit_l = s["x_long_d"] is not None and float(s["x_long_d"]) / sig_ <= -d_in_
                    hit_s = s["x_short_d"] is not None and float(s["x_short_d"]) / sig_ >= d_in_
                    self.assertEqual(m["Real Long"], rl + (" · Entry" if hit_l else ""))
                    self.assertEqual(m["Real Short"], rs + (" · Entry" if hit_s else ""))
                else:
                    self.assertEqual((m["Real Long"], m["Real Short"]), ("—", "—"))
                dev = float(s["mid_d"]) - float(s["mu_d"])
                self.assertEqual(m["Deviation"], "%s · %s%sσ" % (fmt(dev), "{:.2f}".format(Decimal(str(dev / float(s["sigma_d"]))).quantize(Decimal("0.01"), rounding="ROUND_HALF_UP") + 0).replace("-", "−"), board.NBSP))
            self.assertEqual(float(m["F"]), float(s["f"]))
            self.assertEqual((m["Bull Anchor"], m["Bear Anchor"]), (s["anchor_bull"], s["anchor_bear"]))
            if s["mid_bull"] is not None:
                self.assertAlmostEqual(float(m["Bull Price"]), float(s["mid_bull"]), delta=0.0005)
            self.assertEqual(m["Anchor"], {"nav": "NAV", "bell": "Bell"}.get(s["anchor_kind"], "Missing"))
            cfg = rid["config"] if isinstance(rid["config"], dict) else json.loads(rid["config"])
            d_in, sig = float(entry_in(cfg)), float(s["sigma_d"] or 0)
            ok = s["depth_ok_bull"] and s["depth_ok_bear"] and not (s["conflict_bull"] or s["conflict_bear"])
            want = ("No Anchor" if s["anchor_kind"] is None else "No Mean" if not s["stats_ready"] or not sig else "No Book" if not ok
                    else "Long" if s["x_long_d"] is not None and float(s["x_long_d"]) / sig <= -d_in
                    else "Short" if s["x_short_d"] is not None and float(s["x_short_d"]) / sig >= d_in else "None")
            self.assertEqual(m["Signal"], want, run)
            self.assertEqual(now["Side"], "Flat" if s["direction"] is None else s["direction"].title())

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
              "Phase": "stopped" if stopped else board.PHASE_WORDS[self.us_phase(s)],
              "Books": "stopped" if stopped else " · ".join(b for b in books if b) or "ok",
              "Orders": "none open" if not working else "%d left open" % len(working) if stopped else
                        "%d unknown" % sum(o["unknown"] for o in working) if any(o["unknown"] for o in working) else "%d working" % len(working),
              "Entry": "stopped" if stopped else "order sent" if s["idle_reason"] is None else
                       board.ENTRY_WORDS[s["idle_reason"]].replace("$bull", "SOXL").replace("$bear", "SOXS")}
        pct = lambda x, q: str(Decimal(x).quantize(Decimal(q), rounding="ROUND_HALF_UP"))
        awaited = sum(len(c["funding_awaited"]) for c in cycles if isinstance(c["funding_awaited"], list))
        booked = sum((c["funding"] for c in cycles if c["funding_known"]), Decimal(0))
        signed = lambda q: ("+" if q > 0 else "−" if q < 0 else "") + format(abs(q).normalize(), "f")
        residual = " ".join("%s %s" % (tick[l], signed(i["pos_" + l])) for l in ("bull", "bear") if i["pos_" + l])
        mg = {"Position": ("%s left open" % held[0]["direction"]) if held and stopped else held[0]["direction"] if held
                          else "flat" if not residual else "residual · " + residual,
              "Ledger": "mismatch" if i["mismatch"] else "not read" if i["pos_bull"] is None or i["pos_bear"] is None else "match",
              "Equity": "paper" if not r["live"] else "stopped" if stopped else "{:,.2f} U".format(i["equity"]),
              "Maint. Margin": "paper" if not r["live"] else "stopped" if stopped else "{:,.2f} U".format(
                  (i["margin_ratio"] * i["equity"]).quantize(Decimal("0.01"), rounding="ROUND_HALF_UP")),
              "Margin Ratio": "paper" if not r["live"] else "stopped" if stopped else "%s · %s %%" % (
                  "ok" if i["margin_ratio"] < Decimal("0.3") else "warn" if i["margin_ratio"] < Decimal("0.6") else "high", pct(100 * i["margin_ratio"], "0.01")),
              "Funding": "%d awaited" % awaited if awaited else "%s%s U" % ("−" if booked < 0 else "+", "{:,.2f}".format(abs(booked)))}
        if r["live"] and not stopped:
            legs = [(l, (i["liq_" + l] - i["mark_" + l]) / i["mark_" + l] if i["pos_" + l] < 0 else (i["mark_" + l] - i["liq_" + l]) / i["mark_" + l])
                    for l in ("bull", "bear") if i["pos_" + l] and i["liq_" + l] is not None]
            if not i["pos_bull"] and not i["pos_bear"]:
                mg["Liquidation"] = "flat"
            elif legs:
                l, d = min(legs, key=lambda x: x[1])
                mg["Liquidation"] = "%s · %s %s %%" % ("near" if d < Decimal("0.1") else "warn" if d < Decimal("0.25") else "ok", tick[l], "{:,.0f}".format(100 * d))
        else:
            mg["Liquidation"] = "paper" if not r["live"] else "stopped"
        return r, s, i, ex, mg

    def us_phase(self, s):
        """The US market phase of a sample by dashboard/SPEC.md section 3's rule (Combo Spread's etf_phase), in Python."""
        if s["phase"] in ("regular", "close_window"):
            return "Regular"
        ny = ZoneInfo("America/New_York")
        bell = psql_json("SELECT min((detail ->> 'minute_ms')::bigint) AS m FROM trade.anchor_fact WHERE live = %s AND session_id = '%s' AND kind = 'bell'"
                         % (s["live"], s["session_id"]))[0]["m"]
        start_close = (bell + 60000) if bell else int(datetime.combine(datetime.fromisoformat(s["session_id"]).date(), clock_time(16), ny).timestamp() * 1000)
        t = datetime.fromtimestamp(s["ts_ms"] / 1000, ny).replace(tzinfo=None)
        d = datetime.fromtimestamp(s["session_end_ms"] / 1000, ny).date()
        at = lambda day, h, m: datetime.combine(day, clock_time(h, m))
        if start_close <= s["ts_ms"] < start_close + 4 * 3600000:
            return "After-Hours"
        if at(d - timedelta(days=1), 20, 0) <= t < at(d, 4, 0):
            return "Overnight"
        if at(d, 4, 0) <= t < at(d, 9, 30):
            return "Pre-Market"
        return "Closed"

    def test_phase_rule(self):
        """The Phase SQL against the Python rule on hand-made seconds of 2026-11-25/27/30 (New York): Thanksgiving closed, the
        overnight before the early close of 11-27, its pre-market, a Friday evening and Sunday's overnight before Monday."""
        ny = ZoneInfo("America/New_York")
        ms = lambda y, mo, d, h, mi: int(datetime(y, mo, d, h, mi, tzinfo=ny).timestamp() * 1000)
        cases = [  # (sample time, program phase, session_id, session end, want)
            (ms(2026, 11, 25, 17, 0), "off_hours", "2026-11-24", ms(2026, 11, 25, 16, 0), None),   # placeholder replaced below
            (ms(2026, 11, 26, 0, 30), "off_hours", "2026-11-25", ms(2026, 11, 27, 13, 0), "Closed"),
            (ms(2026, 11, 26, 21, 0), "off_hours", "2026-11-25", ms(2026, 11, 27, 13, 0), "Overnight"),
            (ms(2026, 11, 27, 5, 0), "off_hours", "2026-11-25", ms(2026, 11, 27, 13, 0), "Pre-Market"),
            (ms(2026, 11, 27, 10, 0), "regular", "2026-11-25", ms(2026, 11, 27, 13, 0), "Regular"),
            (ms(2026, 11, 28, 21, 0), "off_hours", "2026-11-27", ms(2026, 11, 30, 16, 0), "Closed"),
            (ms(2026, 11, 29, 21, 0), "off_hours", "2026-11-27", ms(2026, 11, 30, 16, 0), "Overnight"),
            (ms(2026, 11, 30, 9, 0), "off_hours", "2026-11-27", ms(2026, 11, 30, 16, 0), "Pre-Market")][1:]
        for ts, phase, sid, end, want in cases:
            s = {"ts_ms": ts, "phase": phase, "session_id": sid, "session_end_ms": end, "live": True}
            got = psql_json("SELECT %s AS p FROM (SELECT %d::bigint AS ts_ms, '%s'::text AS phase, '%s'::text AS session_id, %d::bigint AS session_end_ms, "
                            "true AS live) x" % (board.us_phase("x"), ts, phase, sid, end))[0]["p"]
            self.assertEqual((got, self.us_phase(s)), (want, want), datetime.fromtimestamp(ts / 1000, ny))

    def test_phase_of_every_runs_seconds(self):
        """The Phase SQL against the Python rule on a sample of every Run's own seconds (one every 10 minutes)."""
        for run in self.runs():
            rows = psql_json("SELECT x.*, %s AS p FROM (SELECT * FROM trade.signal_sample WHERE run_id = '%s' AND ts_ms %% 600000 < 1000) x"
                             % (board.us_phase("x"), run))
            for s in rows:
                self.assertEqual(s["p"], self.us_phase(s), (run, s["ts_ms"]))

    def test_status_words_from_the_schema(self):
        """Every Execution and Account word of every Run against its own records read straight from the tables (process_run, its
        newest signal_sample and inventory_sample, its orders not final, its open round trip, its funding): Mode, Start / End,
        Phase, Books, Orders, Entry; Position, Ledger, Equity, Maint. Margin, Margin Ratio, Liquidation, Funding; Run as stopped
        or up / stale / lost. A running Run is read again until no new sample came in between."""
        for run in self.runs():
            for _ in range(5):
                r, s, i, ex, mg = self.expected_status(run)
                v = self.values(1, run)
                got = psql_json(fill(board.Q_STATUS_EXEC, v))[0], psql_json(fill(board.Q_STATUS_ACCOUNT, v))[0]
                if self.expected_status(run)[1:3] == (s, i):
                    break
            with self.subTest(run=run):
                for k, want in ex.items():
                    self.assertEqual(got[0][k], want, k)
                for k, want in mg.items():
                    if k == "Position" and want in ("long", "short"):
                        self.assertTrue(got[1][k].startswith(want + " · "), got[1][k])
                    else:
                        self.assertEqual(got[1][k], want, k)
                if r["stopped_ms"]:
                    self.assertEqual(got[0]["Run"], "stopped")
                else:
                    self.assertRegex(got[0]["Run"], r"^(up |stale · |lost$|starting$)")
                if not mg["Position"].startswith(("flat", "residual", "left")) and not r["stopped_ms"]:
                    self.assertRegex(got[1]["Position"], r"^(long|short) · ")

    @staticmethod
    def refusal_words(e, alarms):
        """The Detail of an orders_rejected alarm (core/exec rejectAlarm's detail) as Python writes it."""
        d = e["detail"] if isinstance(e["detail"], dict) else json.loads(e["detail"] or "{}")
        tick = {"bull": "SOXL", "bear": "SOXS"}

        def counts(x, word):
            items = sorted(((k, int(v)) for k, v in (x or {}).items() if re.fullmatch(r"[0-9]+", str(v))), key=lambda kv: (-kv[1], kv[0]))
            return ", ".join("%s × %s" % (word(k), format(v, ",")) for k, v in items) or None

        def span(ms):
            ms = int(round(ms))
            if ms < 60000:
                return "%ds" % (ms // 1000)
            if ms < 3600000:
                return "%dm\u00a0%02ds" % (ms // 60000, ms // 1000 % 60)
            if ms < 86400000:
                return "%dh\u00a0%02dm" % (ms // 3600000, ms // 60000 % 60)
            return "%dd\u00a0%02dh" % (ms // 86400000, ms // 3600000 % 24)
        parts = []
        if d.get("count") is not None:
            lasted = float(d["lasted_s"]) if d.get("lasted_s") is not None else None
            parts.append(format(int(d["count"]), ",") + " orders refused in a row" + (" over " + span(lasted * 1000) if lasted is not None and lasted >= 1 else "")
                         + (" since " + utc8(int(d["since_ms"])).strftime("%m-%d %H:%M:%S").replace("-", board.NBHY).replace(" ", board.NBSP)
                            if d.get("since_ms") is not None else ""))
        p_ = counts(d.get("purposes"), lambda k: k.title())
        c_ = counts(d.get("codes"), lambda k: k or "(none)")
        inner = "; ".join(x for x in (p_, c_ and "codes " + c_) if x)
        if parts and inner:
            parts[0] += " (" + inner + ")"
        if d.get("purpose") is not None:
            parts.append("last " + " ".join(x for x in (d["purpose"].title(), d.get("side"), d.get("qty") and d["qty"] + " ct",
                                                       d.get("price") and "@ " + d["price"] + " USDT") if x))
        if all(d.get(k) is not None for k in ("venue", "ledger", "hold")):
            parts.append("venue %s / ledger %s / round trip %s ct" % (d["venue"], d["ledger"], d["hold"]))
        if alarms > 1:
            parts.append("alarm raised %d times" % alarms)
        parts.append(re.sub(r"^[^:]*:\s*", "", e["message"]))
        return "; ".join(parts)

    def expected_alerts(self, run):
        """The Alerts of a Run recomputed in Python from its records (SPEC 7, Alerts)."""
        r, s, i, cycles, open_lots, orders = self.raw_run(run)
        running = r["stopped_ms"] is None
        tick = {"bull": "SOXL", "bear": "SOXS"}
        hms = lambda ms: utc8(ms).strftime("%H:%M:%S")
        md = lambda ms: utc8(ms).strftime("%m-%d %H:%M:%S").replace("-", board.NBHY).replace(" ", board.NBSP)
        words = {c: w.replace("Bull ", "SOXL ").replace("Bear ", "SOXS ") for c, _, w in board.IDLE}
        cfg = r["config"] if isinstance(r["config"], dict) else json.loads(r["config"])
        d_in = float(entry_in(cfg))
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
            # 4.4: a thin-leg-first round trip on one leg paired nothing (every batch unwound): a warning, Nothing Paired
            thin = bool(psql_json("SELECT 1 AS x FROM trade.lot l WHERE l.live = %s AND l.cycle_id = '%s' AND to_jsonb(l) ->> 'end_state' IS NOT NULL"
                                  % (r["live"], c["cycle_id"])))
            if c["end_reason"] in what or c["end_reason"] == "dust_only" or dust:
                out.append((c["closed_ms"] or c["opened_ms"], "serious" if c["end_reason"] == "one_leg" and not thin else "warning",
                            "Recorded" if c["end_reason"] == "dust_only" or dust else "Recovered", "Execution " + md(c["opened_ms"]),
                            "Nothing Paired" if c["end_reason"] == "one_leg" and thin else what.get(c["end_reason"], "Dust Left"), None))
            if isinstance(c["funding_awaited"], list) and c["funding_awaited"]:
                out.append((c["closed_ms"], "warning", "In Progress" if running else "Open", "Execution " + md(c["opened_ms"]), "Funding Awaited", None))
        by_id = {c["cycle_id"]: c for c in psql_json("SELECT * FROM trade.cycle WHERE live = %s" % r["live"])}
        # 4.5: a round trip's forced-exit decisions with the same cause and slip are one row, at the newest
        forced = {}
        for d in psql_json("SELECT * FROM trade.decision WHERE run_id = '%s' AND live = %s AND kind = 'forced'" % (run, r["live"])):
            det = d["detail"] if isinstance(d["detail"], dict) else json.loads(d["detail"] or "{}")
            k = (d["cycle_id"], det.get("cause"), None if det.get("slip_bps") is None else str(det.get("slip_bps")))
            forced[k] = max(forced.get(k, 0), d["ts_ms"])
        for (cid, _, _), ts in forced.items():
            c = by_id.get(cid)
            out.append((ts, "serious", "In Progress" if c and c["status"] == "open" and running else "Recovered",
                        "Execution " + (md(c["opened_ms"]) if c else "–"), "Forced Exit", None))
        # 4.4: a batch's unwind of its unpaired thin leg and an exit's hard close of its thick leg (risk exits), an exit below
        # break-even (its pre-check failed, D1)
        for d in psql_json("SELECT * FROM trade.decision WHERE run_id = '%s' AND live = %s AND (kind IN ('pair', 'exit_pair') AND detail ->> 'action' = 'risk_exit' "
                           "OR kind = 'exit' AND reason = 'd1_loss')" % (run, r["live"])):
            c = by_id.get(d["cycle_id"])
            w = {"pair": "Thin Leg Unwound", "exit_pair": "Exit Hard Close", "exit": "Exit Below Break-Even"}[d["kind"]]
            out.append((d["ts_ms"], "warning", "In Progress" if c and c["status"] == "open" and running else "Recovered",
                        "Execution " + (md(c["opened_ms"]) if c else "–"), w, None))
        # 4.5: a run of consecutive orders (the Run's, in their order) rejected without a fill on the same round trip, leg, purpose,
        # side, quantity and error code is one row at its last order's time
        runs_, prev = [], None
        for o in sorted(orders, key=lambda o: (o["submitted_ms"], o["client_order_id"])):
            fold = ((o["cycle_id"], o["leg"], o["purpose"], o["side"], Decimal(str(o["qty"])), o["error_code"] or "")
                    if o["status"] == "rejected" and Decimal(str(o["cum_exec_qty"])) == 0 and not o["unknown"] and not o["not_found"] else None)
            if fold is not None and fold == prev:
                runs_[-1].append(o)
            else:
                runs_.append([o])
            prev = fold
        for run_ in runs_:
            o = run_[0]
            if o["unknown"] or o["not_found"] or o["status"] in ("rejected", "not_sent", "never_arrived"):
                w = ("Order Outcome Unknown" if o["unknown"] else "Order Not Found" if o["not_found"] else "Orders Rejected" if len(run_) > 1
                     else "Order Rejected" if o["status"] == "rejected" else "Order Not Sent" if o["status"] == "not_sent" else "Order Never Arrived")
                out.append((run_[-1]["submitted_ms"], "warning" if o["status"] == "not_sent" else "serious",
                            ("In Progress" if running else "Open") if o["unknown"] else "Recovered", "Order · " + tick[o["leg"]], w,
                            None if len(run_) == 1 else ("contains", " · %s in a row, " % format(len(run_), ","))))
        # 4.5: the program's alarm on a leg's orders refused in a row (orders_rejected) in words, the alarms of one run one row
        alarms = {}
        for e in psql_json("SELECT * FROM trade.event WHERE run_id = '%s' AND live = %s AND level IN ('error', 'warn')" % (run, r["live"])):
            if e["kind"] != "orders_rejected":
                out.append((e["ts_ms"], "critical" if e["level"] == "error" else "warning", "Open", "Program · " + e["module"],
                            e["kind"].replace("_", " ").title(), e["message"]))
                continue
            det = e["detail"] if isinstance(e["detail"], dict) else json.loads(e["detail"] or "{}")
            k = (det.get("leg") or "", str(det.get("since_ms")) if det.get("since_ms") is not None else str(e["seq"]))
            alarms.setdefault(k, []).append(e)
        for (leg, _), es in alarms.items():
            e = max(es, key=lambda e: (e["ts_ms"], e["seq"]))
            out.append((e["ts_ms"], "critical" if e["level"] == "error" else "warning", "Open",
                        "Order · " + tick[leg] if leg in tick else "Program · " + e["module"], "Refused in a Row", self.refusal_words(e, len(es))))
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
                key = lambda x: (x[1], x[2], x[3], x[4]) if x[4] is not None else (x[1], x[2], x[3])
                for _ in range(5):   # a running Run's book conflicts start and end every few seconds: read both at one second
                    r, want = self.expected_alerts(run)
                    got = psql_json(fill(board.Q_ALERTS, self.values(1, run)))
                    rows = [(0, g["Severity"], g["Status"], g["Source"], g["What"], None) for g in got]
                    if r["stopped_ms"] or sorted(map(key, rows), key=str) == sorted(map(key, want), key=str):
                        break
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
                        if isinstance(w[5], tuple):
                            self.assertIn(w[5][1], match[0][5], w[4])
                        elif w[5] is not None:
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
                    if want is None:
                        self.assertIsNone(g["Liq Distance"])
                    else:
                        self.assertAlmostEqual(float(g["Liq Distance"]), float(want), places=9)
                    self.assertEqual(g["Age"] is None, r["stopped_ms"] is not None)

    def test_empty_tables_say_so(self):
        """Owner item 78: a table with nothing to show has one row that says so (Round Trips, Orders, End Reasons, At the Line,
        Latency), not an empty panel; a Run with round trips has no such row."""
        v = self.values(1, "__none")
        for q, col, text in ((board.Q_BLOTTER, "End Reason", "No round trip in the Range"), (board.Q_ORDERS, "Status", "No order: no round trip in the Range"),
                             (board.Q_ORDERS_EVERY, "Status", "No order: no round trip in the Range"),
                             (board.Q_END_REASONS, "End Reason", "None in the Range"), (board.Q_BLOCKED, "Reason", "None in the Range"),
                             (board.Q_LATENCY, "Stage", "No order in the Range")):
            self.assertEqual([r[col] for r in psql_json(fill(q, v))], [text])
        self.assertNotIn("No round trip in the Range", [r["End Reason"] for r in psql_json(fill(board.Q_BLOTTER, self.values(1, self.PAPER_RUN)))])

    def test_orders_fold_runs_of_refusals(self):
        """4.5 (the auditor's review of the first thin-first live run): Orders shows the same orders as Every Order (4.4.1's
        Orders), each run of consecutive orders without a fill that were refused or cancelled, on the same leg, purpose, step,
        side, quantity and status (code), as one row: its first order's figures, Until its last one's time, Orders how many,
        Limit the range of their limits, no latency. Every order with a fill keeps its own row; no two rows that could fold
        sit next to each other."""
        same = ["Purpose", "Step", "Leg", "Side", "Qty", "Filled", "Avg", "Fee", "Status"]
        seen = set()
        for run in self.runs():
            r = psql_json("SELECT live FROM trade.process_run WHERE run_id = '%s'" % run)
            live = r[0]["live"] if r else None
            for c in psql_json("SELECT DISTINCT o.cycle_id FROM trade.cex_order o WHERE o.run_id = '%s'" % run):
                if (live, c["cycle_id"]) in seen:
                    continue
                seen.add((live, c["cycle_id"]))
                v = dict(self.values(1, run), cycle=c["cycle_id"])
                every, folded = psql_json(fill(board.Q_ORDERS_EVERY, v)), psql_json(fill(board.Q_ORDERS, v))
                with self.subTest(run=run, cycle=c["cycle_id"]):
                    self.assertEqual(sum(f["Orders"] for f in folded), len(every))
                    i, prev = 0, None
                    for f in folded:
                        part = every[i:i + f["Orders"]]
                        first = part[0]
                        self.assertEqual([f[k] for k in same], [first[k] for k in same])
                        self.assertEqual(f["Time"], first["Time"])
                        foldable = lambda o: o["Filled"] == 0 and o["Status"].startswith(("Rejected", "No Fill"))
                        key = lambda o: tuple(o[k] for k in ("Leg", "Purpose", "Step", "Side", "Qty", "Status"))
                        if f["Orders"] == 1:
                            self.assertEqual((f["Until"], f["Limit"], f["Latency"]), (None, first["Limit"], first["Latency"]))
                        else:
                            self.assertTrue(all(foldable(o) and key(o) == key(first) for o in part), f)
                            self.assertEqual(f["Until"], part[-1]["Time"])
                            self.assertIsNone(f["Latency"])
                            limits = sorted({Decimal(o["Limit"]) for o in part})
                            self.assertEqual(f["Limit"], first["Limit"] if len(limits) == 1 else "%s–%s" % (
                                min(part, key=lambda o: Decimal(o["Limit"]))["Limit"], max(part, key=lambda o: Decimal(o["Limit"]))["Limit"]))
                        if prev is not None and foldable(prev) and foldable(first):
                            self.assertNotEqual(key(prev), key(first), "two rows that fold side by side")
                        prev, i = part[-1], i + f["Orders"]
                    if c["cycle_id"] == "c1795718904692":   # 11-27 02:48: 21,802 orders, 21,796 refused, in a handful of rows
                        self.assertEqual(len(every), 21802)
                        self.assertLessEqual(len(folded), 12)

    def test_plain_words(self):
        """End and idle reasons in words; a code the board does not list (a new end reason such as dust_only, before the board
        knows it) reads as its own words."""
        got = psql_json("SELECT %s AS listed, %s AS other, %s AS open, %s AS idle, %s AS sent FROM (SELECT 'dust_only' AS a, "
                        "'brand_new_reason' AS b, NULL::text AS n, 'regular_hours' AS i) t" % (
                            board.end_reason("t.a"), board.end_reason("t.b"), board.end_reason("t.n"), board.idle_why("t.i"), board.idle_state("t.n")))[0]
        self.assertEqual(got, {"listed": "Trimmed to Dust", "other": "Brand New Reason", "open": "Open", "idle": "Regular Hours", "sent": "Order Sent"})

    def test_execution_frame(self):
        """The charts' frame: a row is a break row (every column NULL) or has a value, possibly a hidden 0 or a code, in every
        tooltip row; a break row sits right before the point it separates from what came before, after a run's last decisions;
        each decision of the range shows (its tooltip rows carry its instant) and its marker sits at its executable spread; a
        triangle's apex is at the decision's instant, its corners half a width either side."""
        tips = [n for n, _ in board.TOOLTIP_COLUMNS + board.STATE_COLUMNS + board.MARKER_INFO]
        for hours in (0.25, 6, 24, 168):
            v = self.values(hours, self.PAPER_RUN)
            rows = psql_json(fill(board.Q_EXECUTION, v))
            decided = set()
            for i, r in enumerate(rows):
                values = [r[n] for n, _ in board.ALL_COLUMNS]
                if all(x is None for x in values):
                    self.assertLess(i + 1, len(rows), "a break row ends the frame")
                    self.assertIsNotNone(rows[i + 1]["Mean · Tooltip"], "a break row right before a point")
                    continue
                for n in tips:
                    self.assertIsNotNone(r[n], "%s h, %s: %s NULL" % (hours, r["time"], n))
                if r["Action"]:
                    decided.add(r["Decided"])
                # owner item 84: the thick stretch is the real line itself, only where it is at or past its entry line
                for at, line_ in (("Real Long at Entry", "Real Long Spread"), ("Real Short at Entry", "Real Short Spread"),
                                  ("Real Long Deviation at Entry", "Real Long Deviation"), ("Real Short Deviation at Entry", "Real Short Deviation")):
                    if r[at] is not None:
                        self.assertEqual(r[at], r[line_], (r["time"], at))
                for side, _, _ in board.TRIANGLES:
                    if r[side] is not None:
                        self.assertIsNotNone(r[side + " · Base"])
            decisions = psql_json(fill("SELECT d.ts_ms FROM trade.decision d WHERE d.run_id = '%s' AND d.kind IN "
                                       "('entry', 'exit', 'forced') AND d.ts_ms >= $__from AND d.ts_ms <= $__to" % self.PAPER_RUN, v))
            self.assertEqual(decided, {d["ts_ms"] for d in decisions}, "%s h" % hours)
        # 4.2.1 (the lead's item 78 audit): a marker counts its whole step. The live exit of 2026-11-26 15:17:00.138 closed SOXL
        # by its own order and SOXS by the exit of the remaining leg 57 ms later: its marker shows both legs, never 0 / No Fill
        # for a leg that was filled; fees are a positive cost everywhere.
        live = "1795676364501-backup-tk-huichao"
        lv = dict(self.values(1, live), __from=1795676100000, __to=1795677900000)
        marks = {}
        for r in psql_json(fill(board.Q_EXECUTION, lv)):
            if r["Action"]:
                marks.setdefault(r["Decided"], r)
        exit_ = marks[1795677420138]
        self.assertEqual(board.ACTIONS[exit_["Action"]], "Short Exit")
        self.assertEqual((exit_["Bull Filled"], exit_["Bull Avg"], exit_["Bear Filled"], exit_["Bear Avg"]),
                         (Decimal("15.55"), Decimal("33.01"), Decimal("8.01"), Decimal("60.355")))
        # 4.3: the follow-ups (completions, trims, the remaining leg's exit) have no marker of their own; an undone entry shows
        # what was trimmed back (15:00:18.990: SOXL 15.58 filled, SOXS no fill, SOXL 15.58 trimmed back).
        self.assertNotIn(1795677420195, marks)
        undone = marks[1795676418990]
        self.assertEqual(board.ACTIONS[undone["Action"]], "Unwound Short Entry")   # 4.4: 4.3's undone entry is an unwound one
        self.assertEqual((undone["Bull Filled"], undone["Bear Filled"], undone["Bear Avg"], undone["Bull Trimmed"]),
                         (Decimal("15.58"), board.ZERO, board.NO_FILL, Decimal("15.58")))
        for decided, r in marks.items():
            self.assertTrue(r["Fees"] == board.FEE_PENDING or r["Fees"] == 0 or r["Fees"] > 0, decided)
        # Every main marker's legs equal its step's orders recomputed here: its own and the non-trim orders of the decisions
        # sent for it (batch_sent_ms); a leg it sent no order on is "—".
        steps = psql_json(
            "SELECT m.ts_ms, o.leg, count(*) AS n, sum(o.cum_exec_qty) AS q FROM trade.decision m JOIN trade.cex_order o ON o.live = m.live "
            "AND (o.client_order_id IN (SELECT jsonb_array_elements_text(m.detail -> 'client_order_ids')) OR o.purpose <> 'trim' AND "
            "o.client_order_id IN (SELECT f.detail ->> 'client_order_id' FROM trade.decision f WHERE f.live = m.live AND f.cycle_id = m.cycle_id "
            "AND f.kind IN ('trim', 'complete') AND (f.detail ->> 'batch_sent_ms')::bigint = m.ts_ms)) "
            "WHERE m.run_id = '%s' AND m.kind IN ('entry', 'exit', 'forced') GROUP BY 1, 2" % live)
        got = {(s["ts_ms"], s["leg"]): s["q"] for s in steps}
        for decided, r in marks.items():
            for leg, col in (("bull", "Bull Filled"), ("bear", "Bear Filled")):
                want = got.get((decided, leg))
                shown_ = r[col]
                if want is None:
                    self.assertEqual(shown_, board.NOT_IN, (decided, leg))
                else:
                    self.assertEqual(Decimal("0") if shown_ == board.ZERO else shown_, round(want, 2), (decided, leg))
        # Owner item 83: every marker of a round trip carries its Execution ID, the round trip's opening (its row in Round Trips).
        v = self.values(168, self.PAPER_RUN)
        rows = psql_json(fill(board.Q_EXECUTION, v))
        cycles = {r["ts_ms"]: r["opened_ms"] for r in psql_json(
            "SELECT d.ts_ms, c.opened_ms FROM trade.decision d JOIN trade.cycle c ON c.live = d.live AND c.cycle_id = d.cycle_id "
            "WHERE d.run_id = '%s' AND d.kind IN ('entry', 'exit', 'forced')" % self.PAPER_RUN)}
        seen = {(r["Decided"], r["Execution"]) for r in rows if r["Action"]}
        self.assertTrue(seen)
        for decided, execution in seen:
            self.assertEqual(execution, cycles[decided], decided)
        trips = [r for r in psql_json(fill(board.Q_BLOTTER, v)) if r["cycle_id"]]
        opened = {round(datetime.fromisoformat(r["Opened"]).timestamp() * 1000) for r in trips}
        self.assertLessEqual({e for _, e in seen if e in opened}, opened)
        self.assertTrue({e for _, e in seen} & opened)
        # Each triangle is whole (no NULL row inside it breaks its two series) and none overlaps another: the entries of a
        # triangle's width after its start are drawn by it (the round trip of 08:14:01-08:43:15 UTC+8 has short entries at
        # 08:14:01, 08:14:03 and three at 08:17:14).
        entries = psql_json(
            "SELECT d.ts_ms, l.status = 'void' AS undone FROM trade.decision d JOIN trade.lot l ON l.live = d.live AND l.lot_id = d.lot_id "
            "WHERE d.run_id = '%s' AND d.kind = 'entry' ORDER BY d.ts_ms" % self.PAPER_RUN)
        # (an undone entry is a grey triangle, 4.3, drawn only where no entry's triangle is within a width of it)
        for f0, f1 in ((1795651500000, 1795654200000), (1795651980000, 1795652340000), (1795629729414, 1795666000000)):
            v = dict(self.values(1, self.PAPER_RUN), __from=f0, __to=f1)
            half = psql_json(fill("SELECT greatest(%s, ($__to - $__from) / 130)::bigint AS half" % board.STEP_MS, v))[0]["half"]
            starts = []
            for e in entries:
                if not e["undone"] and f0 <= e["ts_ms"] <= f1 and (not starts or e["ts_ms"] > starts[-1] + 2 * half):
                    starts.append(e["ts_ms"])
            grey = []
            for e in entries:
                if e["undone"] and f0 <= e["ts_ms"] <= f1 and all(abs(e["ts_ms"] - s) > 2 * half for s in starts) and (not grey or e["ts_ms"] > grey[-1] + 2 * half):
                    grey.append(e["ts_ms"])
            starts = sorted(starts + grey)
            rows = psql_json(fill(board.Q_EXECUTION, v))
            for r in rows:
                ms = round(datetime.fromisoformat(r["time"]).timestamp() * 1000)
                inside = [s for s in starts if abs(ms - s) <= half and f0 <= s <= f1]
                drawn = any(r[n] is not None for n, _, _ in board.TRIANGLES)
                if all(r[n] is None for n, _ in board.ALL_COLUMNS):
                    continue
                self.assertEqual(bool(inside), drawn, (f0, r["time"]))
                self.assertLessEqual(len(inside), 1)
        # The entry of 2026-11-26 10:59:32.154 (a Short, the first of three in 843 ms): its triangle's apex at its instant, at
        # mu_d + x_d, pointing down.
        d = psql_json("SELECT ts_ms, (detail ->> 'mu_d')::float8 + (detail ->> 'x_d')::float8 AS ys FROM trade.decision WHERE run_id = '%s' "
                      "AND kind = 'entry' AND direction = 'short' AND ts_ms = 1795661972154" % self.PAPER_RUN)[0]
        v = dict(self.values(1, self.PAPER_RUN), __from=d["ts_ms"] - 15 * 60000, __to=d["ts_ms"] + 15 * 60000)
        apex = [r for r in psql_json(fill(board.Q_EXECUTION, v)) if r["Short Entry"] is not None
                and round(datetime.fromisoformat(r["time"]).timestamp() * 1000) == d["ts_ms"]]
        self.assertTrue(apex)
        self.assertLess(float(apex[0]["Short Entry"]), float(d["ys"]))          # the apex below the decision's place: pointing down
        self.assertGreater(float(apex[0]["Short Entry · Base"]), float(d["ys"]))

    def test_thin_leg_first(self):
        """4.4 (core/SPEC.md 12, step 10.9) on every Run whose config sends the thin leg first (none in production before the
        program trades that way: the dev database's synthetic Run, v44/scripts/synth.py): each entry's marker is its lot's end state
        (built and built partial an entry, entry failed the grey triangle, unwound the amber one) and counts every order of its lot,
        trims apart (the thick leg's pairing orders are purpose complete, the unwinds and profit closes trim); each exit's marker
        counts its thin order and its thick leg's exit_pair orders up to the next exit, with its stage, pre-check, expected net and
        net after the batch; Round Trips counts the batches, Orders names every order's step, Batch Ends counts the lots."""
        runs = [r["run_id"] for r in psql_json("SELECT run_id FROM trade.process_run WHERE coalesce(config #>> '{exec,thin_leg}', '') <> ''")]
        if not runs:
            self.skipTest("no Run with the thin leg first in this database")
        ends = list(board.BATCH_ENDS)
        for run in runs:
            r = psql_json("SELECT started_ms, stopped_ms FROM trade.process_run WHERE run_id = '%s'" % run)[0]
            v = dict(self.values(1, run), __from=r["started_ms"], __to=r["stopped_ms"] or int(time.time() * 1000))
            marks = {}
            for row in psql_json(fill(board.Q_EXECUTION, v)):
                if row["Action"]:
                    marks.setdefault(row["Decided"], row)
            decisions = psql_json("SELECT d.ts_ms, d.kind, d.cycle_id, d.lot_id, d.direction, d.reason, d.detail, to_jsonb(l) ->> 'end_state' AS es, l.status, "
                                  "(SELECT min(n.ts_ms) FROM trade.decision n WHERE n.live = d.live AND n.cycle_id = d.cycle_id AND n.kind IN ('exit', 'forced') "
                                  "AND n.ts_ms > d.ts_ms) AS next_ms FROM trade.decision d LEFT JOIN trade.lot l ON l.live = d.live AND l.lot_id = d.lot_id "
                                  "WHERE d.run_id = '%s' AND d.kind IN ('entry', 'exit')" % run)
            if not decisions:   # a live Run that has not traded yet (production's first thin-leg-first Run while it warms up)
                continue
            seen = set()
            for d in decisions:
                m = marks[d["ts_ms"]]
                detail = json.loads(d["detail"]) if isinstance(d["detail"], str) else d["detail"]
                if d["kind"] == "entry":
                    want = {"built": "Entry", "built_partial": "Entry", "entry_failed": "Failed Entry", "unwound_profit": "Unwound Entry",
                            "unwound_stop": "Unwound Entry"}[d["es"]]
                    self.assertEqual(board.ACTIONS[m["Action"]], want.replace("Entry", ("Long " if d["direction"] == "long" else "Short ") + "Entry"), d["ts_ms"])
                    self.assertEqual(m["Batch End"], ends.index(d["es"]) + 1, d["ts_ms"])
                    seen.add(d["es"])
                    orders = psql_json("SELECT leg, purpose, cum_exec_qty AS q FROM trade.cex_order WHERE live AND lot_id = '%s'" % d["lot_id"])
                else:
                    orders = psql_json(
                        "SELECT o.leg, o.purpose, o.cum_exec_qty AS q FROM trade.cex_order o WHERE o.live AND o.client_order_id IN ("
                        "SELECT jsonb_array_elements_text(detail -> 'client_order_ids') FROM trade.decision WHERE run_id = '{r}' AND ts_ms = {t} AND kind = 'exit' "
                        "UNION SELECT detail ->> 'client_order_id' FROM trade.decision WHERE run_id = '{r}' AND cycle_id = '{c}' AND kind = 'exit_pair' "
                        "AND ts_ms >= {t} AND ts_ms < {n})".format(r=run, t=d["ts_ms"], c=d["cycle_id"], n=d["next_ms"] or 10 ** 14))
                    self.assertEqual(m["Exit Stage"], list(board.EXIT_STAGES).index(detail["line"]) + 1, d["ts_ms"])
                    self.assertEqual(m["Pre-Check"], 2 if detail["d1"] else 1, d["ts_ms"])
                    self.assertEqual(m["Expected Net"] if m["Expected Net"] != board.ZERO else 0, round(Decimal(detail["expected_net"]), 2), d["ts_ms"])
                    end = psql_json("SELECT detail ->> 'actual_net' AS a, reason FROM trade.decision WHERE run_id = '{r}' AND cycle_id = '{c}' AND kind = 'exit_pair' "
                                    "AND detail ->> 'action' = 'done' AND ts_ms >= {t} AND ts_ms < {n}".format(r=run, t=d["ts_ms"], c=d["cycle_id"], n=d["next_ms"] or 10 ** 14))
                    self.assertEqual(m["Actual Net"], round(Decimal(end[0]["a"]), 2), d["ts_ms"])
                    self.assertEqual(m["Thick Leg Close"], list(board.THICK_CLOSES).index(end[0]["reason"] or "bound") + 1, d["ts_ms"])
                for leg, col, trim in (("bull", "Bull Filled", "Bull Trimmed"), ("bear", "Bear Filled", "Bear Trimmed")):
                    got = [o["q"] for o in orders if o["leg"] == leg and o["purpose"] != "trim"]
                    shown_ = m[col]
                    if not got:
                        self.assertEqual(shown_, board.NOT_IN, (d["ts_ms"], leg))
                    else:
                        self.assertEqual(Decimal(0) if shown_ == board.ZERO else shown_, round(sum(got), 2), (d["ts_ms"], leg))
                    self.assertEqual(m[trim], round(sum((o["q"] for o in orders if o["leg"] == leg and o["purpose"] == "trim"), Decimal(0)), 2), (d["ts_ms"], leg))
            if run.endswith("-synthetic-thin"):   # the dev database's synthetic Run (v44/scripts/synth.py) has every kind of batch
                self.assertLessEqual({"built", "built_partial", "entry_failed", "unwound_profit", "unwound_stop"}, seen)
            # Round Trip Detail · Batches: each lot of each round trip in its order, its end in words, its legs' fills, what was closed
            # back, its net; Round Trips says Nothing Paired for a round trip whose every batch unwound
            for c in psql_json("SELECT cycle_id, end_reason FROM trade.cycle WHERE run_id = '%s'" % run):
                rows = [b_ for b_ in psql_json(fill(board.Q_BATCH_ROWS, dict(v, cycle=c["cycle_id"])))]
                lots = psql_json("SELECT l.seq, l.status, l.net, to_jsonb(l) ->> 'end_state' AS es, "
                                 "(SELECT sum(cum_exec_qty) FROM trade.cex_order o WHERE o.live AND o.lot_id = l.lot_id AND o.purpose = 'trim') AS back "
                                 "FROM trade.lot l WHERE l.live AND l.cycle_id = '%s' ORDER BY l.seq" % c["cycle_id"])
                self.assertEqual([(b_["Batch"], b_["End"], b_["Back"], b_["Net"]) for b_ in rows],
                                 [("Batch %d" % l["seq"], board.BATCH_WORDS[l["es"]], l["back"], l["net"]) for l in lots], c["cycle_id"])
            trips = {t_["cycle_id"]: t_["End Reason"] for t_ in psql_json(fill(board.Q_BLOTTER, v)) if t_["cycle_id"]}
            for c in psql_json("SELECT cycle_id FROM trade.cycle WHERE run_id = '%s' AND end_reason = 'one_leg'" % run):
                self.assertEqual(trips[c["cycle_id"]], "Nothing Paired")
            # Orders: every order's step; the thick leg's pairing orders Pair, the thin leg's closes Unwind / Profit Close, an exit's
            # thick leg Thick or Hard Close
            for c in psql_json("SELECT cycle_id FROM trade.cycle WHERE run_id = '%s'" % run):
                for o in psql_json(fill(board.Q_ORDERS_EVERY, dict(v, cycle=c["cycle_id"]))):
                    step = o["Step"]
                    self.assertTrue(step, o)
                    if o["Purpose"] == "Complete":
                        self.assertRegex(step, r"^Batch \d+ · Pair$")
                    elif o["Purpose"] == "Trim":
                        self.assertRegex(step, r"^Batch \d+ · (Unwind|Profit Close)$")
                    elif o["Purpose"] == "Entry":
                        self.assertRegex(step, r"^Batch \d+ · (Thin Leg|Top-Up)$")
                    elif o["Purpose"] == "Exit":
                        self.assertRegex(step, r"^Exit [12]? ?· (Thin|Thick|Hard Close)$".replace("Exit [12]? ?", "(Exit [12]|Exit) "))
            # Batch Ends: one row per end state, the lots' count
            got = {b["Batch End"]: b["Batches"] for b in psql_json(fill(board.Q_BATCHES, v))}
            lots = psql_json("SELECT to_jsonb(l) ->> 'end_state' AS es, count(*) AS n FROM trade.lot l WHERE l.run_id = '%s' GROUP BY 1" % run)
            self.assertEqual(got, {board.BATCH_WORDS[l["es"]]: l["n"] for l in lots})

    def test_no_entry_signal_on_an_unusable_book(self):
        """Owner item 70 (the auditor's review): a sample whose leg's book conflicts (or whose depth is unusable) gives no entry
        signal and no Buy Both / Sell Both point, as the executor enters on none. The real paper sample of 2026-11-26
        10:59:47.002 UTC+8: SOXS in conflict, Sell Both +2.0862σ past d_in 2: Market's Signal reads No Book; the same sample with
        SOXS's book usable reads Short; the chart's Sell Both there is no point but the code of Book Not OK."""
        ts = 1795661987002
        cols = ("s.ts_ms, s.anchor_kind, s.stats_ready, s.sigma_d, s.x_long_d, s.x_short_d, s.depth_ok_bull, s.depth_ok_bear, "
                "s.conflict_bull")
        q = ("SELECT %s AS d FROM (SELECT {cols}, {bear} AS conflict_bear FROM trade.signal_sample s WHERE s.live = false AND s.ts_ms = %d) s"
             % (board.market_signal("s", "2"), ts))
        sample = psql_json("SELECT conflict_bear, round((x_short_d / sigma_d)::numeric, 4) AS xs FROM trade.signal_sample "
                           "WHERE live = false AND ts_ms = %d" % ts)
        self.assertEqual(sample, [{"conflict_bear": True, "xs": Decimal("2.0862")}])
        self.assertEqual(psql_json(q.format(cols=cols, bear="s.conflict_bear")), [{"d": "No Book"}])
        self.assertEqual(psql_json(q.format(cols=cols, bear="false")), [{"d": "Short"}])
        # (a range of 10 min: the tooltip window around the entries of 10:59:32, a 90th of the range, ends before 10:59:47)
        v = dict(self.values(0.25, self.PAPER_RUN), __from=ts - 5 * 60000, __to=ts + 5 * 60000)
        row = [r for r in psql_json(fill(board.Q_EXECUTION, v)) if r["time"].startswith("2026-11-26T02:59:47") and r["Mean · Tooltip"] is not None]
        self.assertTrue(row, "the point of 10:59:47")
        self.assertIsNone(row[0]["Real Short Spread"])
        self.assertEqual(row[0]["Real Short Spread · Tooltip"], board.CODE["Book Not OK"])

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
            "sum(f.value) FILTER (WHERE o.purpose IN ('entry', 'complete')) AS entry_value, count(*) AS fills, sum(f.value) AS traded "
            "FROM trade.cex_fill f JOIN trade.cex_order o ON o.live = f.live AND o.client_order_id = f.client_order_id WHERE f.run_id = '%s' "
            "GROUP BY o.cycle_id" % run)}
        cycles = {r["cycle_id"]: r for r in psql_json("SELECT * FROM trade.cycle WHERE run_id = '%s'" % run)}
        rows = [r for r in psql_json(fill(board.Q_BLOTTER, v)) if r["cycle_id"]]   # a Run without one has the row that says so
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
        gross = psql_json(fill(board.Q_CUM_GROSS, v))
        net = psql_json(fill(board.Q_CUM_PNL, v))
        want = sum(fills[c["cycle_id"]]["gross"] - fills[c["cycle_id"]]["fees"] + c["funding"] for c in known)
        tol = sum(Decimal("0.0002") * fills[c["cycle_id"]]["fills"] for c in known if c["live"])
        self.assertAlmostEqual(p["Fee"] or 0, sum(fills[c["cycle_id"]]["fees"] for c in known), delta=tol)   # a positive cost (4.2.1)
        self.assertAlmostEqual(p["PnL"], want, delta=tol)
        self.assertAlmostEqual(p["Gross"], sum(fills[c["cycle_id"]]["gross"] for c in known), delta=tol)
        self.assertEqual((net[-1]["PnL"], gross[-1]["Gross"]), (p["PnL"], p["Gross"]))   # owner items 77, 80: each chart ends on the Summary
        self.assertEqual((len(net) - 2, len(gross) - 2), (2 * len(known), 2 * len(known)))   # two points at each close: before and after
        closed = [c for c in cycles.values() if c["status"] != "open" and c["end_reason"] != "no_fill"]
        self.assertAlmostEqual(p["Notional"], sum((fills[c["cycle_id"]]["traded"] for c in closed if c["cycle_id"] in fills), Decimal(0)), delta=tol)
        peak = psql_json(fill("WITH %s SELECT max(quota_used) AS m FROM trade.inventory_sample i WHERE i.run_id = '%s' AND i.ts_ms >= (SELECT rg.from_ms FROM rg) "
                              "AND i.ts_ms < (SELECT rg.to_ms FROM rg)" % (board.SEL, run), v))[0]["m"]
        self.assertEqual(p["Peak Used"], peak)
        if peak:
            self.assertAlmostEqual(float(p["Return"]), float(10000 * p["PnL"] / peak), places=6)


if __name__ == "__main__":
    unittest.main()
