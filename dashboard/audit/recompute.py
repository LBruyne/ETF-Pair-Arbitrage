#!/usr/bin/env python3
"""Independent check of etfpair.combo_sessions, combo_actions and
combo_anchors, the anchor functions board 3.2's combo_range, combo_prices and
combo_last build on (dashboard/audit/recompute32.py checks those and takes the
anchors as given): recomputes each session's anchor in use, when each
corporate action takes effect on each venue and the step it should make, and
the anchors in use at every minute (A - D from a Known dividend's T, A / r from
a Known split's T_v, both of the two legs and of the reference; which legs are
in a split's halt; the anchors' known time; which minutes are past T of a
leg's dividend still without its amount) from the raw tables, in plain
Python, and compares every session, every action and every minute of the last
7 days (and of the whole carrier session of every action in effect or past T
without its amount) with what the database functions return. It shares no
code or SQL with them. It also checks the venues of the bell anchors:
Bitget's index only before a ticker's first Binance bell.

Board 3.1's combo_series and combo_points, which this script also compared
(spread, mean, drawn points, real spreads, Signal), were dropped on 2026-11-23
(OWNER.md item 24); the anchors they carried are now compared on combo_anchors
directly.

Every read of a run, the raw rows and the functions' results alike, goes
through one psql session inside one read-only REPEATABLE READ transaction, so
both sides of every comparison see the same snapshot of the database: a candle
the collector rewrites during the run (about 6 s after each minute, a few
up to about 61 s), which a split's T_v is found on, is the same version on
both sides. The end of the windows is that transaction's now().

    python3 dashboard/audit/recompute.py [psql connection args ...]

Default connection: the published database (-h 127.0.0.1 -p 5432 -U etfpair_reader -d etfpair; the password in PGPASSWORD).
Exit status 1 when any session, action or minute differs.
"""
import csv, io, math, subprocess, sys, uuid
from bisect import bisect_right
from datetime import date, datetime, timedelta, timezone
from decimal import Decimal, getcontext
from zoneinfo import ZoneInfo

getcontext().prec = 40
CONN = sys.argv[1:] or ["-h", "127.0.0.1", "-p", "5432", "-U", "etfpair_reader", "-d", "etfpair"]
TOLERANCE = Decimal("1e-9")  # bps for a step, USD for an anchor; the two sides only differ by NUMERIC division rounding
MINUTE = 60_000
NY = ZoneInfo("America/New_York")
UTC8 = timezone(timedelta(hours=8))
WINDOW_HOURS = 168  # the last 7 days, besides every carrier session


class Snapshot:
    """One psql session holding one read-only REPEATABLE READ transaction: every query sees the database as of the first one."""

    def __init__(self, conn):
        self.mark = "@@recompute-%s@@" % uuid.uuid4().hex  # ends each result on psql's output
        self.psql = subprocess.Popen(["psql", "-X", "-w", "-q", "--csv", "-v", "ON_ERROR_STOP=1", *conn],
                                     stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True)
        self.query("BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY")

    def query(self, sql):
        self.psql.stdin.write(sql.rstrip().rstrip(";") + ";\n\\echo " + self.mark + "\n")
        self.psql.stdin.flush()
        out = []
        for line in self.psql.stdout:
            if line.rstrip("\n") == self.mark:
                return list(csv.DictReader(io.StringIO("".join(out))))
            out.append(line)
        raise RuntimeError(f"psql exited ({self.psql.wait()}) on: {sql.strip()[:200]}")

    def close(self):
        self.psql.stdin.write("ROLLBACK;\n")  # read only: nothing to keep
        self.psql.stdin.close()
        self.psql.wait()


SNAPSHOT = None


def rows(sql):
    global SNAPSHOT
    if SNAPSHOT is None:
        SNAPSHOT = Snapshot(CONN)
    return SNAPSHOT.query(sql)


def utc8(ms_):
    return datetime.fromtimestamp(ms_ / 1000, UTC8).strftime("%m-%d %H:%M")


def dec(v):
    return Decimal(v) if v != "" else None


def anchors_by_minute(combo, venue, minutes):
    """The anchors in use at each of the minutes by the stated rules, from the raw tables:
    the session's (the session of the newest close at or before the minute; both legs' confirmed NAVs, else both legs'
    valid bells, else none; the reference's bell), A - D from a Known dividend's T, then / r from a Known split's T_v
    (D is per pre-split share); whether each leg is in a split's halt (from its start to T_v); the known time (the
    session's, then the latest of the Known actions of the two legs in effect; none without the legs' anchors); past T
    from T of a leg's dividend still Scheduled. Only a Known action changes an anchor: a Scheduled one (no amount yet,
    also past its instant) and a None do not. {minute: (a_bull, a_bear, a_ref, session, known_ms, gap_bull, gap_bear,
    gap_ref, past_t)}; a minute before the first close has none."""
    ref_ticker = rows(f"SELECT ref_ticker FROM etfpair.combo WHERE combo_id = '{combo}'")[0]["ref_ticker"]
    sessions = sorted((int(r["close_ms"]), r["session_date"]) for r in rows("SELECT session_date, close_ms FROM etfpair.etf_session"))
    closes = [close_ms for close_ms, _ in sessions]
    base, bells = base_anchors(combo), valid_bells()
    every = actions_of(combo, venue)
    acts = [x for x in every if x["status"] == "Known" and x["t"] is not None]
    pending = [x for x in every if x["status"] == "Scheduled" and x["kind"] == "dividend" and x["leg"] != "ref"]

    def adjusted(session, leg, a, minute):
        gap, known = False, None
        for x in acts:
            if x["session"] == session and x["leg"] == leg and x["kind"] == "dividend" and minute >= x["t"]:
                a, known = (a - x["value"] if a is not None else None), max(known or 0, x["known"])
        for x in acts:
            if x["session"] == session and x["leg"] == leg and x["kind"] == "split":
                gap = gap or x["halt"] <= minute < x["t"]
                if minute >= x["t"]:
                    a, known = (a / x["value"] if a is not None else None), max(known or 0, x["known"])
        return a, gap, known

    out = {}
    for minute in minutes:
        i = bisect_right(closes, minute) - 1  # the newest close at or before the minute
        if i < 0:
            continue
        session = sessions[i][1]
        b = base.get(session) or {"bull": None}
        legs = {leg: adjusted(session, leg, b[leg] if b["bull"] is not None else None, minute) for leg in ("bull", "bear")}
        ref = adjusted(session, "ref", bells.get((ref_ticker, session), (None,))[0], minute)
        known = None
        if b["bull"] is not None:
            known = max([b["known"]] + [legs[leg][2] for leg in ("bull", "bear") if legs[leg][2] is not None])
        past = any(x["session"] == session and minute >= x["t"] for x in pending)
        out[minute] = (legs["bull"][0], legs["bear"][0], ref[0], session, known, legs["bull"][1], legs["bear"][1], ref[1], past)
    return out


def stretches_by_minute(combo, venue, minutes):
    """combo_anchors' row for each of the minutes, the stretch [from_ms, to_ms) holding it (to_ms NULL: open), in the
    same shape as anchors_by_minute."""
    got = [(int(r["from_ms"]), int(r["to_ms"]) if r["to_ms"] else None,
            (dec(r["a_bull"]), dec(r["a_bear"]), dec(r["a_ref"]), r["session_date"], int(r["known_ms"]) if r["known_ms"] else None,
             r["gap_bull"] == "t", r["gap_bear"] == "t", r["gap_ref"] == "t", r["past_t"] == "t"))
           for r in rows(f"SELECT session_date, from_ms, to_ms, a_bull, a_bear, a_ref, gap_bull, gap_bear, gap_ref, known_ms, past_t "
                         f"FROM etfpair.combo_anchors('{combo}', '{venue}') ORDER BY from_ms")]
    starts = [g[0] for g in got]
    out = {}
    for minute in minutes:
        i = bisect_right(starts, minute) - 1
        if i >= 0 and (got[i][1] is None or minute < got[i][1]):
            out[minute] = got[i][2]
    return out, len(got)


def valid_bells():
    """A bell anchor is valid for its session only when read at its close: the kline one minute before the close."""
    closes = {r["session_date"]: int(r["close_ms"]) for r in rows("SELECT session_date, close_ms FROM etfpair.etf_session")}
    return {(r["ticker"], r["session_date"]): (Decimal(r["price"]), int(r["known_ms"]))
            for r in rows("SELECT ticker, session_date, price, known_ms, kline_ms FROM etfpair.etf_bell")
            if r["session_date"] in closes and int(r["kline_ms"]) == closes[r["session_date"]] - MINUTE}


def base_anchors(combo):
    """Per session: both legs' confirmed NAVs (the newest confirmation), else both legs' valid bells, else none; when it became known."""
    c = rows(f"SELECT bull_ticker, bear_ticker FROM etfpair.combo WHERE combo_id = '{combo}'")[0]
    bells, navs = valid_bells(), {}
    for r in rows("SELECT ticker, session_date, nav, confirmed_ms FROM etfpair.etf_nav"):
        key = (r["ticker"], r["session_date"])
        navs[key] = max(navs.get(key, (0, None)), (int(r["confirmed_ms"]), Decimal(r["nav"])))
    out = {}
    for r in rows("SELECT session_date FROM etfpair.etf_session"):
        d = r["session_date"]
        legs = [navs.get((c[k], d)) for k in ("bull_ticker", "bear_ticker")]
        if None not in legs:
            out[d] = {"bull": legs[0][1], "bear": legs[1][1], "known": max(legs[0][0], legs[1][0])}
            continue
        legs = [bells.get((c[k], d)) for k in ("bull_ticker", "bear_ticker")]
        out[d] = {"bull": legs[0][0], "bear": legs[1][0], "known": max(legs[0][1], legs[1][1])} if None not in legs else {"bull": None}
    return out


def ms(dt):
    return int(dt.timestamp() * 1000)


def actions_of(combo, venue):
    """The combo's actions in effect on the venue, by the stated rules: the newest reading of each action, Known; its carrier session
    (the session holding 20:00 New York of the day before the ex-date, with the session after it in the calendar); a dividend's T is that
    20:00, a split's T_v the first minute of the carrier session on the venue whose close is nearer the minute before's close / r than to
    it, its halt the start of the run of unchanged closes before T_v."""
    m = rows(f"SELECT c.bull_ticker, c.bear_ticker, c.ref_ticker, m.bull_symbol, m.bear_symbol, m.ref_symbol FROM etfpair.combo c "
             f"JOIN etfpair.combo_market m USING (combo_id) WHERE combo_id = '{combo}' AND venue = '{venue}'")
    if not m:
        return []
    m = m[0]
    legs = {m[leg + "_ticker"]: (leg, m[leg + "_symbol"]) for leg in ("bull", "bear", "ref") if m[leg + "_ticker"]}
    sessions = sorted((int(r["close_ms"]), r["session_date"]) for r in rows("SELECT session_date, close_ms FROM etfpair.etf_session"))
    newest = {}
    for r in rows("SELECT ticker, ex_date, kind, value, status, known_ms FROM etfpair.etf_action"):
        key = (r["ticker"], r["ex_date"], r["kind"])
        if r["ticker"] in legs and (key not in newest or int(r["known_ms"]) > newest[key]["known"]):
            newest[key] = {"ticker": r["ticker"], "ex": r["ex_date"], "kind": r["kind"], "status": r["status"], "known": int(r["known_ms"]),
                           "value": Decimal(r["value"]) if r["value"] else None}
    out = []
    for x in newest.values():
        ex = date.fromisoformat(x["ex"])
        start = ms(datetime(ex.year, ex.month, ex.day, 20, tzinfo=NY) - timedelta(days=1))
        carrier = [(c, d, nxt) for (c, d), (nxt, _) in zip(sessions, sessions[1:]) if c <= start < nxt]
        if not carrier:
            continue
        close_ms, session, next_ms = carrier[0]
        x.update(session=session, close=close_ms, next=next_ms, leg=legs[x["ticker"]][0], start=start, halt=None, t=None)
        if x["kind"] == "dividend":
            x["t"] = start
        elif x["status"] == "Known":
            closes = [(int(r["minute_ms"]), Decimal(r["price"])) for r in rows(
                f"SELECT minute_ms, price FROM etfpair.perp_minute WHERE venue = '{venue}' AND symbol = '{legs[x['ticker']][1]}' "
                f"AND minute_ms >= {close_ms} AND minute_ms < {next_ms} ORDER BY minute_ms")]
            for (_, before), (minute, price) in zip(closes, closes[1:]):
                # Not before the ex-date's Overnight start: no split takes effect earlier.
                if minute >= start and abs(math.log(price * x["value"] / before)) < abs(math.log(price / before)):
                    x["t"] = minute
                    break
            if x["t"] is not None:
                changed = [minute for (_, before), (minute, price) in zip(closes, closes[1:]) if minute < x["t"] and price != before]
                x["halt"] = changed[-1] + MINUTE if changed else close_ms
        out.append(x)
    return [x for x in out if x["status"] == "Known" and x["t"] is not None] + [x for x in out if x["status"] != "Known" or x["t"] is None]


def expected_steps(combo, acts):
    """The step each action should make at its instant, by the stated rule: a split -ln r; a reference's dividend -10^4 D / A; a
    leg's dividend the basket's, -10^4 x the sum of D / A over the legs with an amount going ex that day; None without an amount or
    an anchor. A is the ETF's anchor at the carrier session's start."""
    base, bells = base_anchors(combo), valid_bells()
    ref = rows(f"SELECT ref_ticker FROM etfpair.combo WHERE combo_id = '{combo}'")[0]["ref_ticker"]

    def anchor(x):
        if x["leg"] == "ref":
            return bells.get((ref, x["session"]), (None,))[0]
        return (base.get(x["session"]) or {}).get(x["leg"])

    basket = {}
    for x in acts:
        if x["kind"] == "dividend" and x["leg"] != "ref" and x["value"] is not None and anchor(x) is not None:
            basket[(x["session"], x["ex"])] = basket.get((x["session"], x["ex"]), Decimal(0)) + x["value"] / anchor(x)
    out = {}
    for x in acts:
        key = (x["ticker"], x["ex"], x["kind"])
        if x["value"] is None:
            out[key] = None
        elif x["kind"] == "split":
            out[key] = -x["value"].ln()
        elif x["leg"] == "ref":
            out[key] = -10000 * x["value"] / anchor(x) if anchor(x) is not None else None
        else:
            out[key] = -10000 * basket[(x["session"], x["ex"])] if (x["session"], x["ex"]) in basket else None
    return out


def check_actions(combo, venue):
    """Every action's carrier session, instants and expected step, recomputed, against combo_actions."""
    acts = actions_of(combo, venue)
    steps = expected_steps(combo, acts)
    mine = {(x["ticker"], x["ex"], x["kind"]): (x["session"], x["t"], x["halt"], steps[(x["ticker"], x["ex"], x["kind"])]) for x in acts}
    theirs = {(r["ticker"], r["ex_date"], r["kind"]): (r["session_date"], int(r["effective_ms"]) if r["effective_ms"] else None,
                                                       int(r["halt_ms"]) if r["halt_ms"] else None, dec(r["expected"]))
              for r in rows(f"SELECT ticker, ex_date, kind, session_date, effective_ms, halt_ms, expected FROM etfpair.combo_actions('{combo}', '{venue}')")}

    def same(a, b):
        return a is not None and b is not None and a[:3] == b[:3] and (
            (a[3] is None and b[3] is None) or (a[3] is not None and b[3] is not None and abs(a[3] - b[3]) <= TOLERANCE))
    bad = sorted(k for k in set(mine) | set(theirs) if not same(mine.get(k), theirs.get(k)))
    for k in bad[:3]:
        print("  DIFF action", combo, venue, k, "python", mine.get(k), "sql", theirs.get(k))
    print(f"{combo} {venue:8} actions: {len(theirs)}, {len(bad)} differ")
    # The actions in effect (Known, with an instant) and the legs' dividends still Scheduled: each changes its carrier session.
    return bad, [x for x in acts if x["t"] is not None and (x["status"] == "Known" or x["status"] == "Scheduled" and x["kind"] == "dividend"
                                                          and x["leg"] != "ref")]


def check_sessions(combo):
    """The anchor in use of every session by the stated rule: both legs' confirmed NAVs (the row of each ticker
    confirmed last), else both legs' valid bell anchors (read at the session's close), else none; the reference has its bell anchor."""
    c = rows(f"SELECT bull_ticker, bear_ticker, ref_ticker FROM etfpair.combo WHERE combo_id = '{combo}'")[0]
    bells = valid_bells()
    navs = {}
    for r in rows("SELECT ticker, session_date, nav, confirmed_ms FROM etfpair.etf_nav"):
        key = (r["ticker"], r["session_date"])
        navs[key] = max(navs.get(key, (0, None)), (int(r["confirmed_ms"]), Decimal(r["nav"])))
    bad = 0
    theirs = rows(f"SELECT session_date, a_bull, a_bear, a_ref, basis, known_ms, status FROM etfpair.combo_sessions('{combo}')")
    for r in theirs:
        d = r["session_date"]
        legs = [navs.get((c[k], d)) for k in ("bull_ticker", "bear_ticker")]
        bell = [bells.get((c[k], d)) for k in ("bull_ticker", "bear_ticker")]
        if None not in legs:
            want = ("NAV", legs[0][1], legs[1][1], max(legs[0][0], legs[1][0]))
        elif None not in bell:
            want = ("Bell", bell[0][0], bell[1][0], max(bell[0][1], bell[1][1]))
        else:
            want = ("Missing", None, None, None)
        ref = bells.get((c["ref_ticker"], d))
        got = (r["status"], dec(r["a_bull"]), dec(r["a_bear"]), int(r["known_ms"]) if r["known_ms"] else None)
        if got != want or dec(r["a_ref"]) != (ref[0] if ref else None) or (r["basis"] or "Missing") != want[0]:
            bad += 1
            if bad <= 3:
                print("  DIFF", combo, "session", d, "python", want, ref, "sql", got, r["a_ref"], r["basis"])
    counts = {s: sum(1 for r in theirs if r["status"] == s) for s in ("NAV", "Bell", "Missing")}
    print(f"{combo} sessions: {len(theirs)} ({counts['NAV']} NAV, {counts['Bell']} Bell, {counts['Missing']} Missing), {bad} differ")
    return len(theirs), bad


def check_bell_venues():
    """A ticker's bell anchors come from Binance's index, from Bitget's only for sessions before Binance lists the
    contract: no Bitget bell after the ticker's first Binance bell."""
    by_ticker = {}
    for r in rows("SELECT ticker, session_date, venue FROM etfpair.etf_bell"):
        by_ticker.setdefault(r["ticker"], {}).setdefault(r["venue"], []).append(r["session_date"])
    bad = 0
    for ticker, venues in sorted(by_ticker.items()):
        binance, bitget = sorted(venues.get("binance", [])), sorted(venues.get("bitget", []))
        late = [d for d in bitget if binance and d > binance[0]]
        other = sorted(set(venues) - {"binance", "bitget"})
        bad += len(late) + len(other)
        print(f"bell {ticker}: {len(binance)} binance{' from ' + binance[0] if binance else ''}, {len(bitget)} bitget"
              f"{' to ' + bitget[-1] if bitget else ''}, {len(late)} bitget after the first binance{', other venues ' + str(other) if other else ''}")
    return bad


def compare(label, mine, theirs, numeric=3):
    """Values (numbers..., session, ...): the first `numeric` within the tolerance, the session and the rest equal."""
    bad = 0
    for key in sorted(set(mine) | set(theirs)):
        a, b = mine.get(key), theirs.get(key)
        same = a is not None and b is not None and a[numeric] == (b[numeric] or None) and a[numeric + 1:] == b[numeric + 1:] and all(
            (x is None and y is None) or (x is not None and y is not None and abs(x - y) <= TOLERANCE) for x, y in zip(a[:numeric], b[:numeric]))
        if not same:
            bad += 1
            if bad <= 3:
                print("  DIFF", label, key, "python", a, "sql", b)
    return bad


def main():
    # The first query takes the snapshot; the windows end at the start of the minute of the transaction's now().
    snap = rows("SELECT (extract(epoch FROM now()) * 1000)::bigint AS now_ms, current_setting('transaction_isolation') AS isolation, "
                "current_setting('transaction_read_only') AS read_only, pg_snapshot_xmin(pg_current_snapshot()) AS xmin")[0]
    end = int(snap["now_ms"]) // MINUTE * MINUTE
    print(f"snapshot: one {snap['isolation']} transaction (read only {snap['read_only']}, xmin {snap['xmin']}) from "
          f"{datetime.fromtimestamp(int(snap['now_ms']) / 1000, UTC8):%Y-%m-%d %H:%M:%S.%f}"[:-3] + f" UTC+8; windows end {utc8(end)} UTC+8")
    failures = check_bell_venues()
    checked = 0
    for combo in rows("SELECT combo_id FROM etfpair.combo ORDER BY 1"):
        n, bad = check_sessions(combo["combo_id"])
        checked += n
        failures += bad
    for market in rows("SELECT combo_id, venue FROM etfpair.combo_market ORDER BY 1, 2"):
        combo, venue = market["combo_id"], market["venue"]
        bad_actions, in_effect = check_actions(combo, venue)
        failures += len(bad_actions)
        # The last 7 days and the whole carrier session of every action in effect or past T without its amount (2.9 review
        # round 3: a check of 6 hours around each action missed an adjustment that silently lapsed 7 hours after T).
        windows = [(end - WINDOW_HOURS * 3600_000, end, f"last {WINDOW_HOURS} h")]
        carriers = {x["session"]: x for x in in_effect}
        windows += [(x["close"], min(x["next"], end), f"session {session} ({x['ticker']} {x['kind']} {x['ex']})")
                    for session, x in sorted(carriers.items()) if x["close"] < end]
        minutes = sorted({m for start, stop, _ in windows for m in range(start, stop, MINUTE)})
        mine = anchors_by_minute(combo, venue, minutes)
        theirs, stretches = stretches_by_minute(combo, venue, minutes)
        for start, stop, label in windows:
            keys = range(start, stop, MINUTE)
            bad = compare(f"{combo} {venue} anchors at", {m: mine[m] for m in keys if m in mine}, {m: theirs[m] for m in keys if m in theirs})
            gaps = sum(1 for m in keys if m in mine and True in mine[m][5:8])
            past = sum(1 for m in keys if m in mine and mine[m][8])
            print(f"{combo} {venue:8} anchors {label} [{utc8(start)}, {utc8(stop)}): {len(keys):>6} minutes in "
                  f"{len({mine[m][3] for m in keys if m in mine})} sessions, {gaps} in a split's halt, {past} past T without the amount, "
                  f"{bad} differ")
            failures += bad
        print(f"{combo} {venue:8} combo_anchors: {stretches} stretches; {len(minutes)} distinct minutes checked")
        checked += len(minutes)
    SNAPSHOT.close()
    print("checked", checked, "values;", "FAILED" if failures else "all equal within", "" if failures else f"{TOLERANCE} (bps for a step, USD for an anchor)")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
