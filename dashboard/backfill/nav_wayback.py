#!/usr/bin/env python3
"""Direxion's published NAVs of its funds from saved Wayback Machine captures, into the board's NAV tables.

    .venv/bin/python -I dashboard/backfill/nav_wayback.py --dsn-env NAME [--config dashboard/config.yaml] [--snapshots DIR ...]

A one-off load of the sessions the collector cannot fill itself: Direxion's data API serves the last thirty trading days
only, and its product pages answer this host with a challenge. Reads every saved raw capture (the archive's id_ form,
<page>_<14-digit capture time UTC>.html, the body gzip-compressed or not) in the snapshot directories. A Direxion page's
__NEXT_DATA__ carries ssrData.funds[].Pricing (each fund's latest trade date) and, on a fund's product page,
ssrData.thirtyDayPricing (its last thirty trade dates); TradeDate is MMDDYYYY, Nav a number to the cent. The tickers are
those of the combos with nav_source direxion in the configuration. Every ticker and trade date must have one NAV across
all captures, or nothing is written.

Writes, for each ticker and trade date from history_start that is a session of etfpair.etf_session in the database whose
DSN is in the environment variable NAME: an etf_nav_obs row (issuer direxion, via wayback) and, where the ticker has no
NAV anchor for that session, an etf_nav row. Both carry the load time (received_ms = seen_ms = confirmed_ms): when
Direxion published a figure is not known. Rows already there are left as they are; running it again changes nothing.
Prints each day with the captures that showed it, and the days where an anchor already in place differs.
"""

import argparse
import asyncio
import datetime as dt
import glob
import gzip
import json
import os
import re
import sys
import time
from decimal import ROUND_HALF_UP, Decimal

import asyncpg
import yaml

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
# The two research folders that saved captures; the first was moved out of experiments/ on 2026-11-20.
SNAPSHOTS = [os.path.join(os.path.dirname(ROOT), "etf-pair-archives/experiments-before-20261120-082031/official_close_20261119/wayback"),
             os.path.join(ROOT, "experiments/anchor_check_20261120/wayback")]
NEXT_DATA = re.compile(rb'<script id="__NEXT_DATA__" type="application/json"[^>]*>(.*?)</script>', re.S)
CAPTURE = re.compile(r"_(\d{14})\.html$")


def read_captures(dirs, tickers):
    """{(ticker, session date): {nav: [capture file, ...]}} over every capture with Direxion's page data."""
    found = {}
    for path in sorted(p for d in dirs for p in glob.glob(os.path.join(d, "*.html")) if CAPTURE.search(p)):
        raw = open(path, "rb").read()
        if raw[:2] == b"\x1f\x8b":  # kept as served, content-encoding gzip
            raw = gzip.decompress(raw)
        m = NEXT_DATA.search(raw)
        if not m:
            continue
        ssr = json.loads(m.group(1), parse_float=Decimal)["props"]["pageProps"].get("ssrData") or {}
        rows = [f["Pricing"] for f in ssr.get("funds") or [] if (f.get("Pricing") or {}).get("Ticker") in tickers]
        rows += [r for t in tickers for r in (ssr.get("thirtyDayPricing") or {}).get(t) or []]
        for r in rows:
            if r.get("Nav") is None or not r.get("TradeDate"):
                continue
            day = dt.datetime.strptime(r["TradeDate"], "%m%d%Y").date()
            nav = Decimal(r["Nav"]).quantize(Decimal("0.01"), ROUND_HALF_UP)
            captures = found.setdefault((r["Ticker"], day), {}).setdefault(nav, [])
            if os.path.relpath(path, ROOT) not in captures:  # a page can show a day in both lists
                captures.append(os.path.relpath(path, ROOT))
    return found


async def load(dsn, found, start):
    now_ms = int(time.time() * 1000)
    db = await asyncpg.connect(dsn)
    try:
        sessions = {r["session_date"] for r in await db.fetch("SELECT session_date FROM etfpair.etf_session WHERE session_date >= $1", start)}
        rows = sorted((day, ticker, next(iter(navs)), navs[next(iter(navs))]) for (ticker, day), navs in found.items() if day in sessions)
        anchored = {(r["ticker"], r["session_date"]): r["nav"] for r in await db.fetch(
            "SELECT DISTINCT ON (ticker, session_date) ticker, session_date, nav FROM etfpair.etf_nav ORDER BY ticker, session_date, confirmed_ms DESC")}
        added_obs = added_nav = 0
        async with db.transaction():
            for day, ticker, nav, captures in rows:
                status = await db.execute("""INSERT INTO etfpair.etf_nav_obs (issuer, ticker, session_date, nav, received_ms, seen_ms, via)
                    VALUES ('direxion', $1, $2::date, $3::numeric, $4::bigint, $4::bigint, 'wayback') ON CONFLICT DO NOTHING""", ticker, day, nav, now_ms)
                added_obs += status == "INSERT 0 1"
                status = await db.execute("""INSERT INTO etfpair.etf_nav (ticker, session_date, nav, confirmed_ms, issuer, via)
                    SELECT $1, $2::date, $3::numeric, $4::bigint, 'direxion', 'wayback'
                    WHERE NOT EXISTS (SELECT 1 FROM etfpair.etf_nav WHERE ticker = $1 AND session_date = $2::date)""", ticker, day, nav, now_ms)
                added_nav += status == "INSERT 0 1"
                note = ""
                if (ticker, day) in anchored and anchored[(ticker, day)] != nav:
                    note = f"  [differs: the anchor in place {anchored[(ticker, day)]} is kept]"
                print(f"{day} {ticker} {nav}  {len(captures)} capture(s): {', '.join(os.path.basename(c) for c in captures)}{note}")
    finally:
        await db.close()
    left_out = len(found) - len(rows)
    print(f"{len(rows)} ticker-days from {start} written as etf_nav_obs {added_obs} new, etf_nav {added_nav} new; "
          f"{left_out} ticker-days before {start} or outside etf_session left out; load time {now_ms}")


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--dsn-env", required=True, help="environment variable holding the PostgreSQL DSN")
    ap.add_argument("--config", default=os.path.join(ROOT, "dashboard/config.yaml"), help="collector configuration: history_start, combos")
    ap.add_argument("--snapshots", action="append", help="directory of saved captures (repeatable; default: the two research folders)")
    a = ap.parse_args()
    dsn = os.environ.get(a.dsn_env)
    if not dsn:
        sys.exit(f"the environment variable {a.dsn_env} is empty")
    cfg = yaml.safe_load(open(a.config))
    tickers = {t for c in cfg["combos"] if c.get("nav_source") == "direxion" for t in (c["bull_ticker"], c["bear_ticker"])}
    if not tickers:
        sys.exit(f"{a.config}: no combo has nav_source direxion")
    dirs = a.snapshots or SNAPSHOTS
    missing = [d for d in dirs if not os.path.isdir(d)]
    if missing:
        sys.exit(f"no snapshot directory {', '.join(missing)}")
    found = read_captures(dirs, tickers)
    conflicts = {k: v for k, v in found.items() if len(v) > 1}
    for (ticker, day), navs in sorted(conflicts.items()):
        print(f"CONFLICT {day} {ticker}: " + "; ".join(f"{nav} in {', '.join(c)}" for nav, c in navs.items()))
    if conflicts:
        sys.exit(f"{len(conflicts)} ticker-days with different NAVs across captures: nothing written")
    start = cfg["history_start"] if isinstance(cfg["history_start"], dt.date) else dt.date.fromisoformat(str(cfg["history_start"]))
    asyncio.run(load(dsn, found, start))


if __name__ == "__main__":
    main()
