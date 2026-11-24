#!/usr/bin/env python3
"""Checks the stored rows against their sources again: candle closes of
random stored minutes against a fresh read from each venue, USD per USDT
against Coinbase candles, the bell anchors against the index klines of their
venue (Binance's; Bitget's where Binance had no index kline at the bell
minute, which is checked too) and USD per USDT of their minute, the NAV
anchors in use against the issuers (ProShares' NAV files; Direxion's site
API when DIREXION_SITE_API_KEY is set, the key going in the request header
only), and the corporate actions on record against the source that gave
each (ProShares' distributions, Nasdaq's dividends, Direxion's distributions
with the same key, Bitget's split records; the configuration's entries are
listed, not re-read).

    python3 dashboard/audit/sources.py [psql connection args ...]
"""
import csv, io, json, os, random, subprocess, sys, time, urllib.request
from datetime import datetime, timezone
from decimal import ROUND_HALF_UP, Decimal

CONN = sys.argv[1:] or ["-h", "127.0.0.1", "-p", "5432", "-U", "etfpair_reader", "-d", "etfpair"]
CENT = Decimal("0.01")
DIREXION = "https://p6vg2dlkpverjbr2afvpbqupky.appsync-api.us-east-1.amazonaws.com/graphql"
# The site's own query text.
THIRTY_DAY = """query GetThirtyDayPrice($Ticker: String!, $TradeDate: String!) {
  getThirtyDayPrice(Ticker: $Ticker, TradeDate: $TradeDate) {
    ItemKey
    TradeDate
    Ticker
    Nav
    MarketClosingPrice
  }
}
"""
# The product page's query of a fund's data, its distributions only (as the collector asks).
KURTOSYS = """query GetKurtosysFunds($Ticker: String!) {
  getKurtosysFunds(Ticker: $Ticker) {
    Distributions {
      IncomeDividend
      ShortTermCapitalGain
      LongTermCapitalGain
      ReturnOfCapital
      ExDate
    }
  }
}
"""


def rows(sql):
    out = subprocess.run(["psql", *CONN, "--csv", "-c", sql], check=True, capture_output=True, text=True).stdout
    return list(csv.DictReader(io.StringIO(out)))


def get(url):
    req = urllib.request.Request(url, headers={"User-Agent": "Mozilla/5.0", "Accept": "application/json"})
    return json.load(urllib.request.urlopen(req, timeout=20))


def candles():
    bad = 0
    stored = rows("SELECT venue, symbol, minute_ms, price FROM etfpair.perp_minute WHERE minute_ms < (extract(epoch FROM now()) * 1000)::bigint - 600000")
    newest = max(int(r["minute_ms"]) for r in stored)
    for venue, symbol in sorted({(r["venue"], r["symbol"]) for r in stored}):
        mine = [r for r in stored if r["venue"] == venue and r["symbol"] == symbol]
        # Half from the newest two hours: the rows the running collector wrote right after each minute ended.
        recent = [r for r in mine if int(r["minute_ms"]) >= newest - 7200_000]
        sample = random.sample(mine, min(8, len(mine))) + random.sample(recent, min(8, len(recent)))
        diff = 0
        for r in sample:
            m = int(r["minute_ms"])
            if venue == "binance":
                k = get(f"https://fapi.binance.com/fapi/v1/klines?symbol={symbol}&interval=1m&startTime={m}&endTime={m}&limit=1")
                fresh = k[0][4] if k and k[0][0] == m else None
            elif m >= time.time() * 1000 - 25 * 86400_000:
                # Bitget's candles keep about a month: the newest ceil((end-start)/1m) candles starting at or before endTime.
                k = get(f"https://api.bitget.com/api/v3/market/candles?category=USDT-FUTURES&symbol={symbol}&interval=1m&startTime={m}&endTime={m + 59999}&limit=5")["data"]
                fresh = next((c[4] for c in k if int(c[0]) == m), None)
            else:
                # Older minutes: the history endpoint gives the candles that have ended by endTime.
                k = get(f"https://api.bitget.com/api/v3/market/history-candles?category=USDT-FUTURES&symbol={symbol}&interval=1m&startTime={m}&endTime={m + 60000}&limit=5")["data"]
                fresh = next((c[4] for c in k if int(c[0]) == m), None)
            time.sleep(1)  # one request a second per host
            if fresh is None or Decimal(fresh) != Decimal(r["price"]):
                diff += 1
                print("  DIFF", venue, symbol, m, "stored", r["price"], "fresh", fresh)
        print(f"{venue:8} {symbol}: {len(sample)} random candle minutes, {diff} differ")
        bad += diff

    fx = random.sample(rows("SELECT minute_ms, price FROM etfpair.fx_minute WHERE source = 'candle'"), 15)
    diff = 0
    for r in fx:
        m = int(r["minute_ms"])
        iso = datetime.fromtimestamp(m / 1000, timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
        k = get(f"https://api.exchange.coinbase.com/products/USDT-USD/candles?granularity=60&start={iso}&end={iso}")
        fresh = next((c[4] for c in k if c[0] * 1000 == m), None)
        time.sleep(1)
        if fresh is None or Decimal(str(fresh)) != Decimal(r["price"]):
            diff += 1
            print("  DIFF fx", m, "stored", r["price"], "fresh", fresh)
    print(f"coinbase USDT-USD: {len(fx)} random candle minutes, {diff} differ")
    return bad + diff


def binance_index(symbol, minute):
    """Binance's first index kline opening at or after the minute, as [open, close], or None."""
    k = get(f"https://fapi.binance.com/fapi/v1/indexPriceKlines?pair={symbol}&interval=1m&startTime={minute}&limit=1")
    time.sleep(1)
    return (k[0][0], k[0][4]) if k else None


def bitget_index(symbol, minute):
    """Bitget's index close of the minute from its history endpoint (the minutes that have ended by endTime), or None."""
    k = get(f"https://api.bitget.com/api/v3/market/history-candles?category=USDT-FUTURES&symbol={symbol}&interval=1m&type=index"
            f"&startTime={minute}&endTime={minute + 60000}&limit=5")["data"]
    time.sleep(1)
    return next((c[4] for c in k if int(c[0]) == minute), None)


def bells():
    """Each sampled bell row: its kline opens one minute before the close, its index is its venue's close of that
    kline (and on a Bitget row Binance had no index kline at that minute), its fx is USD per USDT of that minute as
    the board's functions take it, and price = fx x index."""
    symbols = {(r["ticker"], r["venue"]): r["symbol"] for r in rows(
        "SELECT c.bull_ticker AS ticker, m.venue, m.bull_symbol AS symbol FROM etfpair.combo c JOIN etfpair.combo_market m USING (combo_id) "
        "UNION SELECT c.bear_ticker, m.venue, m.bear_symbol FROM etfpair.combo c JOIN etfpair.combo_market m USING (combo_id) "
        "UNION SELECT c.ref_ticker, m.venue, m.ref_symbol FROM etfpair.combo c JOIN etfpair.combo_market m USING (combo_id)")}
    stored = rows("""SELECT b.ticker, b.session_date, b.venue, b.price, b.index_usdt, b.fx, b.kline_ms, s.close_ms,
        (SELECT f.price FROM etfpair.fx_minute f WHERE f.pair = 'USDT-USD' AND f.minute_ms <= b.kline_ms AND f.minute_ms >= b.kline_ms - 300000
         ORDER BY f.minute_ms DESC LIMIT 1) AS fx_now
        FROM etfpair.etf_bell b JOIN etfpair.etf_session s USING (session_date)""")
    bad = 0
    for ticker, venue in sorted({(r["ticker"], r["venue"]) for r in stored}):
        mine = sorted((r for r in stored if r["ticker"] == ticker and r["venue"] == venue), key=lambda r: r["session_date"])
        sample = mine[-3:] + random.sample(mine[:-3], min(5, len(mine[:-3])))
        diff = 0
        for r in sample:
            minute = int(r["kline_ms"])
            problems = []
            if venue == "binance":
                k = binance_index(symbols[(ticker, "binance")], minute)
                fresh = k[1] if k and k[0] == minute else None
            else:
                fresh = bitget_index(symbols[(ticker, venue)], minute)
                k = binance_index(symbols[(ticker, "binance")], minute)
                if k and k[0] == minute:
                    problems.append("Binance has an index kline at the minute")
            if int(r["close_ms"]) - int(r["kline_ms"]) != 60000:
                problems.append("kline is not the minute before the close")
            if fresh is None or Decimal(fresh) != Decimal(r["index_usdt"]):
                problems.append(f"index {r['index_usdt']}, fresh {fresh}")
            if Decimal(r["fx"]) != Decimal(r["fx_now"] or "NaN"):
                problems.append(f"fx {r['fx']}, fx_minute now {r['fx_now']}")
            if Decimal(r["price"]) != Decimal(r["fx"]) * Decimal(r["index_usdt"]):
                problems.append("price is not fx x index")
            if problems:
                diff += 1
                print("  DIFF bell", ticker, r["session_date"], "; ".join(problems))
        print(f"bell {ticker} {venue}: {len(sample)} of {len(mine)} sessions against the venue's index klines, {diff} differ")
        bad += diff
    return bad


def navs():
    """Each NAV anchor in use against the issuer: ProShares' NAV file to the cent for every session; Direxion's API
    for the newest five sessions (it serves the last thirty trading days)."""
    in_use = rows("SELECT DISTINCT ON (ticker, session_date) ticker, session_date, nav, issuer, via FROM etfpair.etf_nav "
                  "ORDER BY ticker, session_date, confirmed_ms DESC")
    bad = 0
    key = os.environ.get("DIREXION_SITE_API_KEY", "")
    for issuer, ticker in sorted({(r["issuer"], r["ticker"]) for r in in_use}):
        mine = sorted((r for r in in_use if r["ticker"] == ticker and r["issuer"] == issuer), key=lambda r: r["session_date"])
        fresh = {}
        if issuer == "proshares":
            req = urllib.request.Request(f"https://accounts.profunds.com/etfdata/ByFund/{ticker}-historical_nav.csv", headers={"User-Agent": "Mozilla/5.0"})
            for row in csv.DictReader(io.StringIO(urllib.request.urlopen(req, timeout=30).read().decode())):
                day = datetime.strptime(row["Date"], "%m/%d/%Y").strftime("%Y-%m-%d")
                fresh[day] = Decimal(row["NAV"]).quantize(CENT, ROUND_HALF_UP)
            time.sleep(1)
            checked = mine
        elif issuer == "direxion" and key:
            checked = mine[-5:]
            for r in checked:
                body = json.dumps({"query": THIRTY_DAY, "variables": {"Ticker": ticker, "TradeDate": datetime.strptime(r["session_date"], "%Y-%m-%d").strftime("%m%d%Y")}})
                req = urllib.request.Request(DIREXION, data=body.encode(), headers={"User-Agent": "Mozilla/5.0", "Content-Type": "application/json", "X-Api-Key": key})
                item = json.load(urllib.request.urlopen(req, timeout=20))["data"]["getThirtyDayPrice"]
                if item:
                    fresh[r["session_date"]] = Decimal(str(item["Nav"])).quantize(CENT, ROUND_HALF_UP)
                time.sleep(1)  # one request a second
        else:
            print(f"nav {issuer} {ticker}: {len(mine)} sessions not checked (no DIREXION_SITE_API_KEY)")
            continue
        diff = 0
        for r in checked:
            if fresh.get(r["session_date"]) != Decimal(r["nav"]):
                diff += 1
                print("  DIFF nav", issuer, ticker, r["session_date"], "stored", r["nav"], f"({r['via']})", "issuer", fresh.get(r["session_date"]))
        print(f"nav {issuer} {ticker}: {len(checked)} of {len(mine)} sessions against the issuer, {diff} differ")
        bad += diff
    return bad


def actions():
    """The newest reading of each action from each programmatic source, against that source read again: the same
    amount (or ratio) on the same ex-date, or, for an issuer's None, still no distribution listed. A past amount a
    source has restated since (ProShares after a split) is a difference: the collector keeps the first reading.
    Direxion's are read only with DIREXION_SITE_API_KEY."""
    stored = {}
    for r in rows("SELECT DISTINCT ON (ticker, ex_date, kind, source) ticker, ex_date, kind, coalesce(value::text, '') AS value, status, source "
                  "FROM etfpair.etf_action ORDER BY ticker, ex_date, kind, source, known_ms DESC"):
        stored.setdefault(r["source"], []).append(r)
    for r in stored.get("config", []) + stored.get("proshares-schedule", []):
        print(f"action {r['ticker']} {r['ex_date']} {r['kind']} {r['status']} {r['value']}: from {r['source']}, not re-read")
    fresh = {}  # (source, ticker, ex_date, kind) -> value
    # The tables that may list a round: its year's, and for a December ex-date the next year's too.
    years = lambda ex: {int(ex[:4])} | ({int(ex[:4]) + 1} if ex[5:7] == "12" else set())
    for ticker, year in sorted({(r["ticker"], y) for r in stored.get("proshares-api", []) for y in years(r["ex_date"])}):
        for d in get(f"https://www.proshares.com/api/distributionsummary?fund={ticker}&year={year}"):
            fresh[("proshares-api", ticker, d["ExDate"][:10], "dividend")] = Decimal(str(d["CashDividendPerShare"]))
        time.sleep(1)
    for ticker in sorted({r["ticker"] for r in stored.get("nasdaq", [])}):
        for d in (get(f"https://api.nasdaq.com/api/quote/{ticker}/dividends?assetclass=etf")["data"]["dividends"]["rows"] or []):
            key = ("nasdaq", ticker, datetime.strptime(d["exOrEffDate"], "%m/%d/%Y").strftime("%Y-%m-%d"), "dividend")
            fresh[key] = fresh.get(key, Decimal(0)) + Decimal(d["amount"].lstrip("$"))
        time.sleep(1)
    key = os.environ.get("DIREXION_SITE_API_KEY", "")
    for ticker in sorted({r["ticker"] for r in stored.get("direxion-api", [])}) if key else []:
        body = json.dumps({"query": KURTOSYS, "variables": {"Ticker": ticker}})
        req = urllib.request.Request(DIREXION, data=body.encode(), headers={"User-Agent": "Mozilla/5.0", "Content-Type": "application/json", "X-Api-Key": key})
        for d in json.load(urllib.request.urlopen(req, timeout=20))["data"]["getKurtosysFunds"]["Distributions"]:
            if all(d[k] is None for k in ("IncomeDividend", "ShortTermCapitalGain", "LongTermCapitalGain", "ReturnOfCapital")):
                continue  # a round listed before its amounts: no reading
            ex = datetime.strptime(d["ExDate"], "%m%d%Y").strftime("%Y-%m-%d")
            cash = sum((Decimal(str(d[k])) for k in ("IncomeDividend", "ShortTermCapitalGain", "LongTermCapitalGain", "ReturnOfCapital") if d[k] is not None),
                       Decimal(0))
            fresh[("direxion-api", ticker, ex, "dividend")] = fresh.get(("direxion-api", ticker, ex, "dividend"), Decimal(0)) + cash
        time.sleep(1)
    if stored.get("direxion-api") and not key:
        print(f"actions direxion-api: {len(stored['direxion-api'])} on record not checked (no DIREXION_SITE_API_KEY)")
    if stored.get("bitget-split-records"):
        for d in get("https://api.bitget.com/api/v3/market/split-records")["data"]:
            ex = datetime.strptime(d["exDividendDate"], "%Y%m%d").strftime("%Y-%m-%d")
            fresh[("bitget-split-records", d["symbol"].removesuffix("USDT"), ex, "split")] = Decimal(d["adjustmentRatio"])
    bad = 0
    for source in ("proshares-api", "nasdaq", "bitget-split-records") + (("direxion-api",) if key else ()):
        for r in stored.get(source, []):
            now = fresh.get((source, r["ticker"], r["ex_date"], r["kind"]))
            if source == "direxion-api" and now == 0:
                now = None  # a row of nothing: None
            want = None if r["status"] == "None" else Decimal(r["value"])
            same = now == want if source != "bitget-split-records" or now is not None else True  # Bitget lists a recent window only
            if not same:
                bad += 1
                print("  DIFF action", r["ticker"], r["ex_date"], r["kind"], source, "stored", r["status"], r["value"], "now", now)
        print(f"actions {source}: {len(stored.get(source, []))} on record against the source, "
              f"{sum(1 for r in stored.get(source, []) if (source, r['ticker'], r['ex_date'], r['kind']) not in fresh)} no longer listed")
    return bad


def main():
    random.seed(20261118)
    bad = candles() + bells() + navs() + actions()
    print("FAILED" if bad else "all stored rows match their sources")
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
