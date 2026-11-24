#!/usr/bin/env python3
"""History rows of etfpair.perp_second from trade archives, the row files, and their loader.

    perp_second.py archives --from DAY --to DAY --rows DIR [--no-rest] [--binance-trades UTCDAY ...]
    perp_second.py load --dsn-env NAME --rows DIR [--from DAY --to DAY]

DAY is a UTC+8 calendar day (YYYY-MM-DD); --from is included, --to is not.

archives downloads the trade history that is missing for every contract of every combo in
dashboard/config.yaml, on both venues, into experiments/perp_second_history/raw/ (every file listed in
experiments/perp_second_history/manifest.csv with URL, bytes, sha256 and retrieval time), then writes the
price and trade_ms rows of the days asked for: for every whole second T (Unix ms, T % 1000 == 0) from the
contract's first trade on or after history_start, the newest trade with exchange time < T, ties broken
by the larger trade id. No history of executable quotes exists, so the book columns are empty.
--no-rest downloads the venues' daily archive files only, without the REST runs (Bitget fills-history
paged back from now, about 6000 pages for two days; Binance aggTrades paged forward): the rows then end
where the archives end. --binance-trades also downloads Binance's daily trades file of each UTC day
named (YYYY-MM-DD) for every Binance contract; such a file replaces that day's aggTrades (see below).

load COPYs each row file into a temporary table and merges it into etfpair.perp_second of the database
whose DSN is in the environment variable NAME: a column is set only where the table holds NULL, nothing
is overwritten, and a row is rewritten only when it gains a value. Archive files first, then interim,
then live. Idempotent; the table must exist (dashboard/internal/booktape/schema.sql). In the transaction
of each file, the per-minute summary of the rows the file inserted or filled is brought up to date:
etfpair.refresh_perp_second_minute(from_ms, to_ms) (dashboard/internal/scan/schema.sql) is called for
(T1 - 1, T2] of every stretch of changed seconds T1..T2 without a gap of more than a minute. Where the
function does not exist yet, load says so, merges without it and prints one SELECT of the function for every
range of changed seconds (the stretches of all files joined), to run as the writer role once it exists.
Refreshes take turns (the function locks the summary table first): one that meets another refresh's open
transaction, a file's of this load among them, waits for it to end and then counts its rows, so a load may
run while the collector refreshes or the summary's history is filled day by day. Run one load at a time.

Row files, also written by perp_second_recordings.py: <rows>/<venue>/<SYMBOL>/<YYYYMMDD>.<source>.csv.gz,
one gzip file per venue, contract, UTC+8 day of second_ms and source (archive, interim or live), header
venue,symbol,second_ms,price,trade_ms,bid_px,bid_sz,ask_px,ask_sz,book_ms, rows in second_ms order. An
empty field is NULL; the book arrays (prices and sizes best first, decimal strings as sent) are
PostgreSQL array literals such as "{54.15,54.14}", quoted as CSV quotes a field with commas; an empty
side is {}. The files written before the book arrays (header ...,bid,bid_size,ask,ask_size,book_ms) load
the same way when their book fields are empty, as in every archive file; a book value in one is refused
by the array columns: such a file is rebuilt from its recording with perp_second_recordings.py.

Inputs of archives:
  Bitget   download-centre trade archives, UTC+8 days, published about 35 hours after a day ends:
           listing POST https://www.bitget.com/v1/statistics/public/download/getPublicDataV2 (7 days a
           request), files https://img.bitgetimg.com/online/trades/UMCBL/<SYMBOL>/...zip holding CSV
           trade_id,timestamp,price,side,volume(quote),size(base). The timestamp is floored to the second;
           the millisecond comes from the id, a snowflake: ts_ms = (trade_id >> 22) + 1435725920677,
           checked against the second on every row. Newer fills: GET
           https://api.bitget.com/api/v2/mix/market/fills-history (tradeId, price, size, side, ts), paged
           back from now with idLessThan; it serves 90 days, 7 days a query.
  Binance  data.binance.vision daily aggTrades, UTC days, each checked against its published sha256
           (.CHECKSUM): CSV agg_trade_id,price,quantity,first_trade_id,last_trade_id,transact_time,
           is_buyer_maker. Newer aggregates: GET https://fapi.binance.com/fapi/v1/aggTrades (a, p, q, f,
           l, T, m), paged forward with fromId to now; it serves the last 2 days only. Daily trades
           files of the days named by --binance-trades (.../daily/trades/<SYMBOL>/<SYMBOL>-trades-<day>.zip,
           CSV id,price,qty,quote_qty,time,is_buyer_maker, checked against .CHECKSUM likewise) replace
           the aggTrades of their day: they hold the trades of 2026-10-27 00:02:45-00:30:18 UTC that its
           aggregates lack. Their rows have the prices of the aggregates' rows wherever both are complete
           (2026-10-27, five contracts: no second differs, and both agree with every one-minute close), but
           trade_ms is the trade's own time: an aggregate carries the time of its first trade, its other
           trades follow by up to 100 ms, so 9-18 % of that day's seconds have a trade_ms 1-100 ms later
           (median 2-7 ms). load never overwrites: where the table holds the day, only the seconds it lacks
           take these values.
  The validated tick store of SOXL, SOXS and SOXX (<ARCHIVE>/soxl_bitget_study_20261119/data_tick/, read
  its README.md) is used where it covers: its Bitget day files (ticks/bitget/) and its Binance daily zips
  (raw/binance/aggTrades/).
A source covers an interval of exchange time in which it holds every trade: an archive day, a REST run
from its start to ten seconds before its newest request, less the holes where Binance aggregates (or the
trades of a trades file) skip HOLE_TRADE_IDS or more trade ids. A row exists where every trade from the
newest one before it is covered; after a hole in the coverage rows resume one whole second after the
first trade.
<ARCHIVE> is ../etf-pair-archives/experiments-before-20261120-082031 next to the repository, where the tick store
was moved on 2026-11-20 (read only).

Run from the repository root: .venv/bin/python -I dashboard/backfill/perp_second.py ...
"""

import argparse
import asyncio
import csv
import datetime as dt
import glob
import gzip
import hashlib
import http.client
import io
import json
import os
import re
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
import zipfile
from collections import Counter, namedtuple
from concurrent.futures import ProcessPoolExecutor, ThreadPoolExecutor
from pathlib import Path

import numpy as np

REPO = Path(__file__).resolve().parents[2]
CONFIG = REPO / 'dashboard' / 'config.yaml'
ARCHIVE = REPO.parent / 'etf-pair-archives' / 'experiments-before-20261120-082031'
HISTORY = REPO / 'experiments' / 'perp_second_history'
RAW = HISTORY / 'raw'
MANIFEST = HISTORY / 'manifest.csv'
TICK_STORE = ARCHIVE / 'soxl_bitget_study_20261119' / 'data_tick' / 'ticks'

TZ8 = dt.timezone(dt.timedelta(hours=8))
DAY_MS = 86_400_000
SNOWFLAKE_OFFSET_MS = 1435725920677
# A REST run holds every trade up to this long before its newest request; fresher ones may not be served yet.
REST_MARGIN_MS = 10_000
# A Bitget REST run reaches this far into the coverage it continues, so that the junction is compared.
REST_OVERLAP_MS = 600_000
# Binance aggregates chain their trades (first_trade_id = previous last_trade_id + 1) except where trade ids
# are not public trades: 1 to 16 ids skipped at a time, which the exchange's klines do not count either. A
# skip of this many ids is trades the source lacks: a hole in its coverage. The only one, 2026-10-27
# 00:02:45-00:30:18 UTC, skips 1,655 to 58,900 ids on every contract, trades the klines count.
HOLE_TRADE_IDS = 100
# The ids of a Binance trades file count trades, not aggregates: they are moved up by TRADE_ID_SPACE so that the two
# kinds never meet, neither in combine(), which keeps one copy of an id, nor in the aggregate id chain.
TRADE_ID_SPACE = 1 << 62
# The per-minute summary that load brings up to date (dashboard/internal/scan/schema.sql), called once for every
# stretch of changed seconds without a gap of more than REFRESH_GAP_MS.
REFRESH = 'etfpair.refresh_perp_second_minute'
REFRESH_GAP_MS = 60_000
COLUMNS =('venue', 'symbol', 'second_ms', 'price', 'trade_ms', 'bid_px', 'bid_sz', 'ask_px', 'ask_sz', 'book_ms')
ARRAYS = ('bid_px', 'bid_sz', 'ask_px', 'ask_sz')
# The header of the row files written before the book arrays; they load column by column into COLUMNS.
SCALAR_COLUMNS = ('venue', 'symbol', 'second_ms', 'price', 'trade_ms', 'bid', 'bid_size', 'ask', 'ask_size', 'book_ms')
SOURCES = ('archive', 'interim', 'live')  # load merges the row files in this order
MANIFEST_FIELDS = ('venue', 'kind', 'symbol', 'day', 'path', 'url', 'bytes', 'sha256', 'checksum_ok', 'retrieved_ms', 'error')

BITGET_LISTING = 'https://www.bitget.com/v1/statistics/public/download/getPublicDataV2'
BITGET_FILLS = 'https://api.bitget.com/api/v2/mix/market/fills-history'
BINANCE_BUCKET = 'https://s3-ap-northeast-1.amazonaws.com/data.binance.vision'
BINANCE_SITE = 'https://data.binance.vision/'
BINANCE_AGGTRADES = 'https://fapi.binance.com/fapi/v1/aggTrades'

ARCHIVE, REST = 0, 1  # piece priorities: an archive's copy of a trade is kept over a REST copy
Piece = namedtuple('Piece', 'priority covered_from_ms covered_to_ms kind path')


def log(message):
    print(f'{dt.datetime.now(TZ8):%Y-%m-%d %H:%M:%S} UTC+8 perp_second: {message}', flush=True)


def now_ms():
    return time.time_ns() // 1_000_000


def day_start_ms(day):
    """00:00 UTC+8 of a day given as YYYYMMDD, in Unix ms."""
    return int(dt.datetime.strptime(day, '%Y%m%d').replace(tzinfo=TZ8).timestamp() * 1000)


def day_of_ms(ms):
    """The UTC+8 calendar day (YYYYMMDD) of a Unix ms instant."""
    return dt.datetime.fromtimestamp(ms / 1000, TZ8).strftime('%Y%m%d')


def next_day(day):
    return (dt.datetime.strptime(day, '%Y%m%d') + dt.timedelta(days=1)).strftime('%Y%m%d')


def day_arg(text):
    return dt.date.fromisoformat(text).strftime('%Y%m%d')


def stamp(ms):
    return dt.datetime.fromtimestamp(ms / 1000, TZ8).strftime('%Y-%m-%d %H:%M:%S')


def sha256(data):
    return hashlib.sha256(data).hexdigest()


def relative(path):
    return str(Path(path).resolve().relative_to(REPO))


# ---------------------------------------------------------------- configuration

def read_config(path=CONFIG):
    """The contracts of every combo's markets as (venue, symbol), in configuration order without
    duplicates, and history_start in Unix ms (a date: midnight UTC, as pairscan's YAML decoder reads it).

    Reads the flow mappings of dashboard/config.yaml's markets ({ venue: ..., bull_symbol: ..., ... });
    a market written in another form is an error."""
    text = Path(path).read_text()
    markets = re.findall(r'\{([^{}]*\bbull_symbol\s*:[^{}]*)\}', text)
    if not markets or len(markets) != len(re.findall(r'\bbull_symbol\s*:', text)):
        raise SystemExit(f'{path}: every market must be a one-line {{ venue: ..., bull_symbol: ..., ... }} mapping')
    contracts = []
    for body in markets:
        fields = {}
        for part in body.split(','):
            key, _, value = part.partition(':')
            fields[key.strip()] = value.strip().strip('"\'')
        if fields.get('venue') not in ('bitget', 'binance'):
            raise SystemExit(f'{path}: unknown venue {fields.get("venue")!r}')
        for key in ('bull_symbol', 'bear_symbol', 'ref_symbol'):
            if fields.get(key) and (fields['venue'], fields[key]) not in contracts:
                contracts.append((fields['venue'], fields[key]))
    found = re.search(r'^history_start:\s*["\']?(\d{4}-\d{2}-\d{2})', text, re.M)
    if not found:
        raise SystemExit(f'{path}: history_start is required')
    start = dt.datetime.fromisoformat(found.group(1)).replace(tzinfo=dt.timezone.utc)
    return contracts, int(start.timestamp() * 1000)


# ---------------------------------------------------------------- downloads

class Http:
    """Public requests: at most one start per host every GAP_S seconds; HTTP 429 and 418 pause the host."""

    DEFAULT_GAP_S = 0.6
    # aggTrades weighs 20 of the 2400 a minute this IP has on fapi, which pairscan shares.
    GAP_S = {'fapi.binance.com': 1.0}
    WEIGHT_LIMIT = 1800

    def __init__(self):
        self.lock = threading.Lock()
        self.next_start = {}

    def pause(self, host, seconds):
        with self.lock:
            self.next_start[host] = max(self.next_start.get(host, 0.0), time.monotonic() + seconds)

    def request(self, url, body=None):
        """(status, headers, bytes). A client error (4xx but 418 and 429) is an answer; 418 and 429 pause the host
        and are tried again, like server and network errors; the last failure is raised."""
        host = urllib.parse.urlsplit(url).hostname
        gap = self.GAP_S.get(host, self.DEFAULT_GAP_S)
        failure = None
        for attempt in range(8):
            with self.lock:
                start = max(time.monotonic(), self.next_start.get(host, 0.0))
                self.next_start[host] = start + gap
            time.sleep(max(0.0, start - time.monotonic()))
            headers = {'User-Agent': 'research-client'}
            data = None
            if body is not None:
                data = json.dumps(body).encode()
                headers['Content-Type'] = 'application/json'
            try:
                with urllib.request.urlopen(urllib.request.Request(url, data=data, headers=headers), timeout=120) as response:
                    payload = response.read()
                    answer = {k.lower(): v for k, v in response.headers.items()}
                    used = answer.get('x-mbx-used-weight-1m')
                    if used is not None and int(used) > self.WEIGHT_LIMIT:
                        log(f'{host}: weight {used} used this minute, waiting for the next one')
                        self.pause(host, 61 - time.time() % 60)
                    return response.status, answer, payload
            except urllib.error.HTTPError as error:
                answer = {k.lower(): v for k, v in error.headers.items()} if error.headers else {}
                if 400 <= error.code < 500 and error.code not in (418, 429):
                    return error.code, answer, error.read()
                failure = f'HTTP {error.code}'
                if error.code in (418, 429):
                    wait = float(answer.get('retry-after') or 60)
                    log(f'{host}: HTTP {error.code}, pausing {wait:.0f} s')
                    self.pause(host, wait)
                    continue
            except (urllib.error.URLError, http.client.HTTPException, OSError) as error:
                failure = repr(error)
            log(f'{host}: {failure}, trying again')
            time.sleep(5 * (attempt + 1))
        raise RuntimeError(f'{url}: {failure}')


MANIFEST_LOCK = threading.Lock()


def manifest(**row):
    with MANIFEST_LOCK:
        MANIFEST.parent.mkdir(parents=True, exist_ok=True)
        new = not MANIFEST.exists()
        with open(MANIFEST, 'a', newline='') as handle:
            writer = csv.DictWriter(handle, fieldnames=MANIFEST_FIELDS)
            if new:
                writer.writeheader()
            writer.writerow({key: row.get(key, '') for key in MANIFEST_FIELDS})


def save(path, data):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + '.part')
    temporary.write_bytes(data)
    os.replace(temporary, path)


def bitget_listing(client, wanted):
    """Download-centre rows of the trade archives of {symbol: UTC+8 days (YYYYMMDD)}, asked 7 days at a
    time for the symbols that want a day of the window."""
    days = sorted(d for ds in wanted.values() for d in ds)
    rows, day, last = [], dt.datetime.strptime(days[0], '%Y%m%d').date(), dt.datetime.strptime(days[-1], '%Y%m%d').date()
    while day <= last:
        end = min(day + dt.timedelta(days=6), last)
        window = {(day + dt.timedelta(days=k)).strftime('%Y%m%d') for k in range((end - day).days + 1)}
        symbols = sorted(s for s, ds in wanted.items() if ds & window)
        if not symbols:
            day = end + dt.timedelta(days=1)
            continue
        body = dict(displaySymbol=symbols, businessLine=2, businessType=2, dateType=1,
                    beginTimeStr=day.isoformat(), endTimeStr=end.isoformat())
        status, _, data = client.request(BITGET_LISTING, body)
        received = now_ms()
        path = RAW / 'bitget' / 'listing' / f'trades-{day:%Y%m%d}_{end:%Y%m%d}-{received}.json'
        save(path, json.dumps(dict(url=BITGET_LISTING, request=body, status=status, received_ms=received,
                                   payload=data.decode(errors='replace'))).encode())
        manifest(venue='bitget', kind='listing', symbol=' '.join(symbols), day=f'{day}..{end}', path=relative(path),
                 url=BITGET_LISTING, bytes=len(data), sha256=sha256(data), retrieved_ms=received)
        payload = json.loads(data) if status == 200 else {}
        if payload.get('code') != '200':
            raise RuntimeError(f'bitget listing {day}..{end}: HTTP {status} {data[:200]!r}')
        rows.extend(payload.get('data') or [])
        day = end + dt.timedelta(days=1)
    return rows


def bitget_archive_day(client, symbol, day, urls):
    """Downloads the listed archive files of one contract and UTC+8 day (YYYYMMDD); when all are in, the
    day's marker <day>.complete lists them. Returns whether the day is complete."""
    folder = RAW / 'bitget' / 'trades' / symbol
    names = []
    for url in sorted(set(urls)):  # the listing can name a file twice (2026-09-15 of TQQQ, SQQQ, QQQ)
        name = url.rsplit('/', 1)[1]
        names.append(name)
        if (folder / name).exists():
            continue
        status, headers, data = client.request(url)
        retrieved = now_ms()
        expected = headers.get('content-length')
        error = '' if status == 200 and (expected is None or int(expected) == len(data)) else f'HTTP {status}, {len(data)} of {expected} bytes'
        if not error:
            try:
                damaged = zipfile.ZipFile(io.BytesIO(data)).testzip()
                error = f'bad zip member {damaged}' if damaged else ''
            except zipfile.BadZipFile as bad:
                error = f'bad zip: {bad}'
        if not error:
            save(folder / name, data)
        manifest(venue='bitget', kind='trades', symbol=symbol, day=day, path=relative(folder / name) if not error else '',
                 url=url, bytes=len(data), sha256=sha256(data), retrieved_ms=retrieved, error=error)
        if error:
            log(f'bitget {symbol} {day}: {url}: {error}')
            return False
    save(folder / f'{day}.complete', json.dumps(dict(day=day, files=names)).encode())
    return True


def bitget_rest(client, symbol, start_ms):
    """One REST run: fills-history paged back from now to start_ms, saved page by page; its coverage goes to a
    sidecar <run>.json written once the run is complete."""
    run_ms = now_ms()
    if run_ms - start_ms > 7 * DAY_MS:
        raise RuntimeError(f'bitget {symbol}: a fills-history query spans 7 days at most, {stamp(start_ms)} is older')
    path = RAW / 'bitget' / 'rest_fills' / f'{symbol}-{run_ms}.jsonl.gz'
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + '.part')
    cursor, pages, fills, oldest = None, 0, 0, None
    with gzip.open(temporary, 'wt') as out:
        while True:
            params = dict(symbol=symbol, productType='usdt-futures', limit=1000, startTime=start_ms, endTime=run_ms)
            if cursor is not None:
                params['idLessThan'] = cursor
            status, _, data = client.request(BITGET_FILLS + '?' + urllib.parse.urlencode(params))
            payload = json.loads(data) if status == 200 else {}
            if payload.get('code') != '00000':
                raise RuntimeError(f'bitget {symbol} fills-history: HTTP {status} {data[:200]!r}')
            out.write(json.dumps(dict(url=BITGET_FILLS, request=params, received_ms=now_ms(), payload=payload)) + '\n')
            rows = payload.get('data') or []
            pages += 1
            fills += len(rows)
            if not rows:
                break
            cursor, oldest = rows[-1]['tradeId'], int(rows[-1]['ts'])
            if oldest < start_ms:
                break
            if pages % 100 == 0:
                log(f'bitget {symbol}: {pages} pages, {fills} fills, back to {stamp(oldest)}')
    os.replace(temporary, path)
    end = dict(covered_from_ms=start_ms, covered_to_ms=run_ms - REST_MARGIN_MS, pages=pages, fills=fills)
    save(path.with_suffix('').with_suffix('.json'), json.dumps(end).encode())
    manifest(venue='bitget', kind='rest_fills', symbol=symbol, day=f'{stamp(start_ms)}..{stamp(run_ms - REST_MARGIN_MS)}',
             path=relative(path), url=BITGET_FILLS, bytes=path.stat().st_size, sha256=sha256(path.read_bytes()), retrieved_ms=now_ms())
    log(f'bitget {symbol}: REST {pages} pages, {fills} fills, {stamp(start_ms)} to {stamp(run_ms - REST_MARGIN_MS)}')


def binance_listing(client, symbol):
    """{UTC day (date): bucket key} of the symbol's daily aggTrades archives."""
    prefix = f'data/futures/um/daily/aggTrades/{symbol}/'
    days, marker, page = {}, '', 0
    while True:
        url = BINANCE_BUCKET + '?' + urllib.parse.urlencode(dict(delimiter='/', prefix=prefix, marker=marker))
        status, _, data = client.request(url)
        received = now_ms()
        path = RAW / 'binance' / 'listing' / f'aggTrades-{symbol}-{received}-page{page}.xml'
        save(path, data)
        manifest(venue='binance', kind='listing', symbol=symbol, path=relative(path), url=url, bytes=len(data),
                 sha256=sha256(data), retrieved_ms=received, error='' if status == 200 else f'HTTP {status}')
        if status != 200:
            raise RuntimeError(f'binance listing {symbol}: HTTP {status}')
        text = data.decode()
        keys = re.findall(r'<Key>([^<]+)</Key>', text)
        for key in keys:
            found = re.search(r'-(\d{4}-\d{2}-\d{2})\.zip$', key)
            if found:
                days[dt.date.fromisoformat(found.group(1))] = key
        if '<IsTruncated>true</IsTruncated>' not in text or not keys:
            return days
        marker, page = keys[-1], page + 1


def binance_archive_day(client, symbol, day, key):
    """Downloads one UTC day's aggTrades zip and its CHECKSUM; the zip is kept under its name only when its
    sha256 is the published one (otherwise as <name>.mismatch). Returns whether the day is in."""
    path = RAW / 'binance' / 'aggTrades' / symbol / key.rsplit('/', 1)[1]
    if path.exists():
        return True
    status, headers, data = client.request(BINANCE_SITE + key)
    retrieved = now_ms()
    expected_bytes = headers.get('content-length')
    if status != 200 or (expected_bytes is not None and int(expected_bytes) != len(data)):
        manifest(venue='binance', kind='aggTrades', symbol=symbol, day=day.isoformat(), url=BINANCE_SITE + key, bytes=len(data),
                 retrieved_ms=retrieved, error=f'HTTP {status}, {len(data)} of {expected_bytes} bytes')
        log(f'binance {symbol} {day}: HTTP {status}')
        return False
    check_status, _, check = client.request(BINANCE_SITE + key + '.CHECKSUM')
    published = check.decode(errors='replace').split()[0] if check_status == 200 and check.strip() else ''
    digest = sha256(data)
    good = published == digest
    if good:
        save(path.with_name(path.name + '.CHECKSUM'), check)
        save(path, data)
    else:
        save(path.with_name(path.name + '.mismatch'), data)
        log(f'binance {symbol} {day}: sha256 {digest} is not the published {published!r}')
    manifest(venue='binance', kind='aggTrades', symbol=symbol, day=day.isoformat(),
             path=relative(path if good else path.with_name(path.name + '.mismatch')), url=BINANCE_SITE + key, bytes=len(data),
             sha256=digest, checksum_ok=str(good), retrieved_ms=retrieved, error='' if good else f'published sha256 {published!r}')
    return good


def binance_trades_day(client, symbol, day):
    """Downloads one UTC day's (date) trades zip and its CHECKSUM, kept under its name only when its sha256 is the
    published one (otherwise as <name>.mismatch). Returns whether the day is in; a day not published is not."""
    key = f'data/futures/um/daily/trades/{symbol}/{symbol}-trades-{day.isoformat()}.zip'
    path = RAW / 'binance' / 'trades' / symbol / key.rsplit('/', 1)[1]
    if path.exists():
        return True
    status, headers, data = client.request(BINANCE_SITE + key)
    retrieved = now_ms()
    expected_bytes = headers.get('content-length')
    if status != 200 or (expected_bytes is not None and int(expected_bytes) != len(data)):
        manifest(venue='binance', kind='trades', symbol=symbol, day=day.isoformat(), url=BINANCE_SITE + key, bytes=len(data),
                 retrieved_ms=retrieved, error=f'HTTP {status}, {len(data)} of {expected_bytes} bytes')
        log(f'binance {symbol} trades {day}: HTTP {status}' + (' (not published)' if status == 404 else ''))
        return False
    check_status, _, check = client.request(BINANCE_SITE + key + '.CHECKSUM')
    published = check.decode(errors='replace').split()[0] if check_status == 200 and check.strip() else ''
    digest = sha256(data)
    good = published == digest
    if good:
        save(path.with_name(path.name + '.CHECKSUM'), check)
        save(path, data)
    else:
        save(path.with_name(path.name + '.mismatch'), data)
        log(f'binance {symbol} trades {day}: sha256 {digest} is not the published {published!r}')
    manifest(venue='binance', kind='trades', symbol=symbol, day=day.isoformat(),
             path=relative(path if good else path.with_name(path.name + '.mismatch')), url=BINANCE_SITE + key, bytes=len(data),
             sha256=digest, checksum_ok=str(good), retrieved_ms=retrieved, error='' if good else f'published sha256 {published!r}')
    return good


def binance_first_id(client, symbol, start_ms):
    """Id of the first aggregate trade at or after start_ms, searched an hour at a time up to now; None if none."""
    at = start_ms
    while at < now_ms():
        params = dict(symbol=symbol, startTime=at, endTime=at + 3_599_999, limit=1)
        status, _, data = client.request(BINANCE_AGGTRADES + '?' + urllib.parse.urlencode(params))
        if status != 200:
            raise RuntimeError(f'binance {symbol} aggTrades: HTTP {status} {data[:200]!r}')
        rows = json.loads(data)
        if rows:
            return int(rows[0]['a'])
        at += 3_600_000
    return None


def binance_rest(client, symbol, start_ms):
    """One REST run: aggTrades from the first aggregate at or after start_ms, paged forward with fromId until a
    page comes back short; it covers up to ten seconds before that last request (sidecar <run>.json)."""
    run_ms = now_ms()
    path = RAW / 'binance' / 'rest_aggtrades' / f'{symbol}-{run_ms}.jsonl.gz'
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + '.part')
    first = from_id = binance_first_id(client, symbol, start_ms)
    pages, fills, breaks, last, covered_to = 0, 0, 0, None, now_ms() - REST_MARGIN_MS
    with gzip.open(temporary, 'wt') as out:
        while from_id is not None:
            params = dict(symbol=symbol, fromId=from_id, limit=1000)
            sent = now_ms()
            status, _, data = client.request(BINANCE_AGGTRADES + '?' + urllib.parse.urlencode(params))
            if status != 200:
                raise RuntimeError(f'binance {symbol} aggTrades: HTTP {status} {data[:200]!r}')
            rows = json.loads(data)
            out.write(json.dumps(dict(url=BINANCE_AGGTRADES, request=params, sent_ms=sent, received_ms=now_ms(), payload=rows)) + '\n')
            pages += 1
            fills += len(rows)
            for row in rows:
                breaks += last is not None and int(row['a']) != last + 1
                last = int(row['a'])
            if len(rows) < 1000:
                covered_to = sent - REST_MARGIN_MS
                break
            from_id = last + 1
            if pages % 100 == 0:
                log(f'binance {symbol}: {pages} pages, {fills} aggregates, up to {stamp(int(rows[-1]["T"]))}')
    os.replace(temporary, path)
    end = dict(covered_from_ms=start_ms, covered_to_ms=covered_to, pages=pages, fills=fills, first_id=first, last_id=last, id_breaks=breaks)
    save(path.with_suffix('').with_suffix('.json'), json.dumps(end).encode())
    manifest(venue='binance', kind='rest_aggtrades', symbol=symbol, day=f'{stamp(start_ms)}..{stamp(covered_to)}',
             path=relative(path), url=BINANCE_AGGTRADES, bytes=path.stat().st_size, sha256=sha256(path.read_bytes()), retrieved_ms=now_ms())
    log(f'binance {symbol}: REST {pages} pages, {fills} aggregates (ids {first}..{last}, {breaks} breaks), '
        f'{stamp(start_ms)} to {stamp(covered_to)}')


def utc_days(first_ms, last_ms):
    """UTC dates of the days that start in [first_ms, last_ms)."""
    day = dt.datetime.fromtimestamp(first_ms / 1000, dt.timezone.utc).date()
    out = []
    while int(dt.datetime(day.year, day.month, day.day, tzinfo=dt.timezone.utc).timestamp() * 1000) < last_ms:
        out.append(day)
        day += dt.timedelta(days=1)
    return out


def covered(intervals, start, end):
    return any(a <= start and end <= b for a, b in intervals)


def download_bitget(client, symbols, history_start_ms, rest=True):
    today = dt.datetime.now(TZ8).date()
    first = dt.datetime.fromtimestamp(history_start_ms / 1000, TZ8).date()
    wanted = {}
    for symbol in symbols:
        archived = merge([(p.covered_from_ms, p.covered_to_ms) for p in pieces_of('bitget', symbol) if p.priority == ARCHIVE])
        days = [first + dt.timedelta(days=k) for k in range((today - first).days)]
        wanted[symbol] = {d.strftime('%Y%m%d') for d in days
                          if not covered(archived, day_start_ms(d.strftime('%Y%m%d')), day_start_ms(d.strftime('%Y%m%d')) + DAY_MS)}
    asking = sorted(s for s in symbols if wanted[s])
    if asking:
        rows = bitget_listing(client, {s: wanted[s] for s in asking})
        listed = {}
        for row in rows:
            listed.setdefault((row['displayName'], row['dateTimeStr'].replace('-', '')), []).append(row['fileUrl'])
        for (symbol, day), urls in sorted(listed.items(), key=lambda item: (item[0][1], item[0][0])):
            if symbol in wanted and day in wanted[symbol]:
                bitget_archive_day(client, symbol, day, urls)
        for symbol in asking:
            got = sorted(day for (s, day) in listed if s == symbol and day in wanted[symbol])
            log(f'bitget {symbol}: {len(wanted[symbol])} days not archived here, {len(got)} listed'
                + (f' ({got[0]}..{got[-1]})' if got else ''))
    for symbol in symbols if rest else ():
        end = max((p.covered_to_ms for p in pieces_of('bitget', symbol)), default=history_start_ms)
        bitget_rest(client, symbol, max(history_start_ms, end - REST_OVERLAP_MS))


def download_binance(client, symbols, history_start_ms, rest=True, trades_days=()):
    today_ms = int(dt.datetime.now(dt.timezone.utc).replace(hour=0, minute=0, second=0, microsecond=0).timestamp() * 1000)
    for symbol in symbols:
        got = [day for day in trades_days if binance_trades_day(client, symbol, day)]
        if trades_days:
            log(f'binance {symbol}: trades files of {len(got)} of the {len(trades_days)} UTC days asked for'
                + (f' ({", ".join(str(day) for day in got)})' if got else ''))
    for symbol in symbols:
        archived = merge([(p.covered_from_ms, p.covered_to_ms) for p in pieces_of('binance', symbol) if p.priority == ARCHIVE])
        wanted = [d for d in utc_days(history_start_ms, today_ms)
                  if not covered(archived, int(dt.datetime(d.year, d.month, d.day, tzinfo=dt.timezone.utc).timestamp() * 1000),
                                 int(dt.datetime(d.year, d.month, d.day, tzinfo=dt.timezone.utc).timestamp() * 1000) + DAY_MS)]
        if wanted:
            listed = binance_listing(client, symbol)
            got = [d for d in wanted if d in listed and binance_archive_day(client, symbol, d, listed[d])]
            log(f'binance {symbol}: {len(wanted)} UTC days not archived here, {len(got)} downloaded'
                + (f' ({got[0]}..{got[-1]})' if got else ''))
    for symbol in symbols if rest else ():
        end = max((p.covered_to_ms for p in pieces_of('binance', symbol)), default=history_start_ms)
        binance_rest(client, symbol, max(history_start_ms, end))


def download(contracts, history_start_ms, rest=True, trades_days=(), client=None):
    """Fetches what the contracts lack, the two venues side by side: without rest the daily archives only;
    trades_days (UTC dates) also the Binance trades files of those days."""
    client = client or Http()
    by_venue = {venue: [s for v, s in contracts if v == venue] for venue in ('bitget', 'binance')}
    with ThreadPoolExecutor(2) as pool:
        jobs = [pool.submit(download_bitget, client, by_venue['bitget'], history_start_ms, rest),
                pool.submit(download_binance, client, by_venue['binance'], history_start_ms, rest, tuple(trades_days))]
        for job in jobs:
            job.result()


# ---------------------------------------------------------------- trades of the sources

def pieces_of(venue, symbol):
    """Every source of one contract's trades, with the interval of exchange time it covers. Of the tick store,
    Bitget's day files are read; for Binance its downloaded daily zips, which hold the trade ids that show
    holes (aggregate_holes), like the zips downloaded here."""
    out = []
    if venue == 'bitget':
        for name in sorted(glob.glob(str(TICK_STORE / venue / symbol / '*.npz'))):
            with np.load(name) as z:
                source = str(z['source'])
                out.append(Piece(REST if source.startswith('rest') else ARCHIVE, int(z['covered_from_ms']), int(z['covered_to_ms']),
                                 'tick store ' + source, name))
        for name in sorted(glob.glob(str(RAW / 'bitget' / 'trades' / symbol / '*.complete'))):
            start = day_start_ms(Path(name).name[:8])
            out.append(Piece(ARCHIVE, start, start + DAY_MS, 'bitget archive', name))
        runs = sorted(glob.glob(str(RAW / 'bitget' / 'rest_fills' / f'{symbol}-*.json')))
        kind = 'bitget rest'
    else:
        zips = {}
        for kind, folder in (('tick store binance archive', TICK_STORE.parent / 'raw' / 'binance' / 'aggTrades'),
                             ('binance archive', RAW / 'binance' / 'aggTrades')):
            for name in glob.glob(str(folder / symbol / '*.zip')):
                zips[Path(name).name] = (kind, name)
        by_day = {}  # a day's trades file replaces its aggTrades
        for kind, name in sorted(zips.values(), key=lambda item: Path(item[1]).name):
            by_day[re.search(r'(\d{4}-\d{2}-\d{2})\.zip$', name).group(1)] = (kind, name)
        for name in glob.glob(str(RAW / 'binance' / 'trades' / symbol / '*.zip')):
            by_day[re.search(r'(\d{4}-\d{2}-\d{2})\.zip$', name).group(1)] = ('binance trades archive', name)
        for day, (kind, name) in sorted(by_day.items()):
            day = dt.date.fromisoformat(day)
            start = int(dt.datetime(day.year, day.month, day.day, tzinfo=dt.timezone.utc).timestamp() * 1000)
            out.append(Piece(ARCHIVE, start, start + DAY_MS, kind, name))
        runs = sorted(glob.glob(str(RAW / 'binance' / 'rest_aggtrades' / f'{symbol}-*.json')))
        kind = 'binance rest'
    for name in runs:
        end = json.loads(Path(name).read_text())
        if end['covered_to_ms'] > end['covered_from_ms']:
            out.append(Piece(REST, end['covered_from_ms'], end['covered_to_ms'], kind, name[:-len('.json')] + '.jsonl.gz'))
    return out


def bitget_archive_trades(paths):
    ids, stamps, prices = [], [], []
    for path in paths:
        with zipfile.ZipFile(path) as archive:
            for member in archive.namelist():
                with archive.open(member) as stream:
                    reader = csv.reader(io.TextIOWrapper(stream, 'utf-8'))
                    column = {name: k for k, name in enumerate(next(reader))}
                    i, t, p = column['trade_id'], column['timestamp'], column['price']
                    for row in reader:
                        ids.append(int(row[i]))
                        stamps.append(int(row[t]))
                        prices.append(float(row[p]))
    ids = np.array(ids, dtype=np.int64)
    ts = (ids >> 22) + SNOWFLAKE_OFFSET_MS
    wrong = int((ts // 1000 * 1000 != np.array(stamps, dtype=np.int64)).sum())
    if wrong:
        raise ValueError(f'{paths[0]}: {wrong} trade ids do not decode to the archived second')
    return ts, ids, np.array(prices, dtype=np.float64)


def aggregate_holes(ts, ids, first, last):
    """[a, b) stretches of exchange time where aggregates skip at least HOLE_TRADE_IDS trade ids: from the
    aggregate before the skip to the one after it. Rows up to a and after b stay exact: the missing trades
    lie between the two in id order, so in time within [a, b]."""
    order = np.argsort(ids, kind='stable')
    ts, first, last = ts[order], first[order], last[order]
    skips = np.flatnonzero(first[1:] - last[:-1] - 1 >= HOLE_TRADE_IDS)
    return [(int(ts[k]), int(ts[k + 1]), int(first[k + 1] - last[k] - 1)) for k in skips]


def binance_archive_trades(path):
    ids, ts, prices, first, last = [], [], [], [], []
    with zipfile.ZipFile(path) as archive:
        with archive.open(archive.namelist()[0]) as stream:
            for row in csv.reader(io.TextIOWrapper(stream, 'utf-8')):
                if not row[0].isdigit():
                    continue  # the header line
                ids.append(int(row[0]))
                prices.append(float(row[1]))
                first.append(int(row[3]))
                last.append(int(row[4]))
                ts.append(int(row[5]))
    ts, ids = np.array(ts, dtype=np.int64), np.array(ids, dtype=np.int64)
    holes = aggregate_holes(ts, ids, np.array(first, dtype=np.int64), np.array(last, dtype=np.int64))
    return ts, ids, np.array(prices, dtype=np.float64), holes


def binance_trades_file(path):
    """A daily trades file: ids moved up by TRADE_ID_SPACE; its holes are where the trade ids skip HOLE_TRADE_IDS
    or more."""
    ids, ts, prices = [], [], []
    with zipfile.ZipFile(path) as archive:
        with archive.open(archive.namelist()[0]) as stream:
            for row in csv.reader(io.TextIOWrapper(stream, 'utf-8')):
                if not row[0].isdigit():
                    continue  # the header line
                ids.append(int(row[0]))
                prices.append(float(row[1]))
                ts.append(int(row[4]))
    ts, ids = np.array(ts, dtype=np.int64), np.array(ids, dtype=np.int64)
    holes = aggregate_holes(ts, ids, ids, ids)
    return ts, ids + TRADE_ID_SPACE, np.array(prices, dtype=np.float64), holes


def rest_trades(path, venue):
    ids, ts, prices, first, last = [], [], [], [], []
    with gzip.open(path, 'rt') as handle:
        for line in handle:
            page = json.loads(line)
            if venue == 'bitget':
                for row in page['payload'].get('data') or []:
                    ids.append(int(row['tradeId']))
                    ts.append(int(row['ts']))
                    prices.append(float(row['price']))
            else:
                for row in page['payload']:
                    ids.append(int(row['a']))
                    ts.append(int(row['T']))
                    prices.append(float(row['p']))
                    first.append(int(row['f']))
                    last.append(int(row['l']))
    ts, ids = np.array(ts, dtype=np.int64), np.array(ids, dtype=np.int64)
    holes = aggregate_holes(ts, ids, np.array(first, dtype=np.int64), np.array(last, dtype=np.int64)) if venue == 'binance' else []
    return ts, ids, np.array(prices, dtype=np.float64), holes


def piece_trades(piece, venue):
    """(ts_ms, trade_id, price, holes) of a piece, trades in any order; holes are [a, b, missing ids] inside
    its coverage where it lacks trades."""
    if piece.path.endswith('.npz'):
        with np.load(piece.path) as z:
            return z['ts_ms'], z['trade_id'], z['price'], []
    if piece.path.endswith('.complete'):
        files = sorted(set(json.loads(Path(piece.path).read_text())['files']))
        return bitget_archive_trades([str(Path(piece.path).parent / name) for name in files]) + ([],)
    if piece.path.endswith('.zip'):
        return binance_trades_file(piece.path) if piece.kind == 'binance trades archive' else binance_archive_trades(piece.path)
    return rest_trades(piece.path, venue)


# ---------------------------------------------------------------- rows

def merge(intervals):
    """Union of [a, b) intervals as a sorted list of disjoint ones; touching intervals join."""
    out = []
    for a, b in sorted(i for i in intervals if i[1] > i[0]):
        if out and a <= out[-1][1]:
            out[-1][1] = max(out[-1][1], b)
        else:
            out.append([a, b])
    return [(a, b) for a, b in out]


def subtract(intervals, holes):
    """Sorted disjoint [a, b) intervals without the [a, b) holes."""
    out = []
    for a, b in intervals:
        at = a
        for h0, h1 in sorted(h for h in holes if h[0] < b and h[1] > a):
            if h0 > at:
                out.append((at, h0))
            at = max(at, h1)
        if at < b:
            out.append((at, b))
    return out


def combine(parts):
    """One trade list from several sources' copies, given in priority order: the first copy of an id is
    kept. Returns ts, ids, prices sorted by (ts, id), the number of repeated ids and of repeats that differ."""
    if not parts:
        empty = np.zeros(0, dtype=np.int64)
        return empty, empty, np.zeros(0), 0, 0
    ts = np.concatenate([p[0] for p in parts])
    ids = np.concatenate([p[1] for p in parts])
    prices = np.concatenate([p[2] for p in parts])
    unique, first, inverse = np.unique(ids, return_index=True, return_inverse=True)
    repeated = len(ids) - len(unique)
    differ = int(((ts != ts[first][inverse]) | (prices != prices[first][inverse])).sum()) if repeated else 0
    ts, ids, prices = ts[first], ids[first], prices[first]
    order = np.lexsort((ids, ts))
    return ts[order], ids[order], prices[order], repeated, differ


def day_rows(day_ms, ts, ids, prices, intervals, carry):
    """The rows of the whole seconds T of the UTC+8 day that starts at day_ms.

    ts, ids, prices: the day's covered trades sorted by (ts, id). intervals: sorted disjoint [a, b) inside
    the day in which every trade is known. carry: (ts, id, price) of the newest trade before the day when
    every trade from it to the day's start is known, else None. A row at T is the newest trade with ts < T
    (of equal ts, the larger id); it exists while that trade is known: T in [a, b] of an interval that
    continues known coverage, T in (first trade, b] of one that does not.
    Returns second_ms, trade_ms, price arrays and the carry into the next day."""
    grid = day_ms + 1000 * np.arange(DAY_MS // 1000, dtype=np.int64)
    known = np.zeros(len(grid), dtype=bool)
    known[0] = carry is not None  # the carry is the newest trade before the day's first second
    state, at = carry is not None, day_ms
    for a, b in intervals:
        if state and a == at:
            low = a
        else:
            k = int(np.searchsorted(ts, a, 'left'))
            state = k < len(ts) and ts[k] < b
            if not state:
                at = b
                continue
            low = int(ts[k]) + 1
        known |= (grid >= low) & (grid <= b)
        at = b
    seconds = grid[known]
    index = np.searchsorted(ts, seconds, 'left') - 1
    from_carry = index < 0
    if from_carry.any() and carry is None:
        raise AssertionError('a row without a trade before it')
    trade_ms = np.where(from_carry, carry[0] if carry else 0, ts[np.maximum(index, 0)] if len(ts) else 0)
    price = np.where(from_carry, carry[2] if carry else 0.0, prices[np.maximum(index, 0)] if len(ts) else 0.0)
    following = None
    if state and at == day_ms + DAY_MS:
        following = (int(ts[-1]), int(ids[-1]), float(prices[-1])) if len(ts) else carry
    return seconds, trade_ms.astype(np.int64), price.astype(np.float64), following


def price_text(values):
    """Decimal strings of float prices: the shortest that reads back to the same float, without exponent."""
    unique, inverse = np.unique(values, return_inverse=True)
    text = [np.format_float_positional(v, unique=True, trim='-') for v in unique]
    return [text[k] for k in inverse.tolist()]


def row_path(rows_dir, venue, symbol, day, source):
    return Path(rows_dir) / venue / symbol / f'{day}.{source}.csv.gz'


def field(value):
    """The CSV field of a row value: None is empty (NULL), a tuple or list of decimal strings an array literal."""
    if value is None:
        return ''
    if isinstance(value, (tuple, list)):
        return '{' + ','.join(value) + '}'
    return str(value)


def write_csv(path, rows):
    """Writes rows (sequences in COLUMNS order; None is NULL, the book arrays sequences of decimal strings) as
    one gzip CSV file, through a temporary name."""
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    text = io.StringIO()
    writer = csv.writer(text, lineterminator='\n')
    writer.writerow(COLUMNS)
    writer.writerows([field(v) for v in row] for row in rows)
    temporary = path.with_name(path.name + '.part')
    with gzip.open(temporary, 'wb', compresslevel=6) as out:
        out.write(text.getvalue().encode())
    os.replace(temporary, path)
    return path


def write_rows(rows_dir, source, rows):
    """Writes rows of any venues, contracts and days as their row files (one per venue, contract and
    UTC+8 day of second_ms, in second_ms order); a file of the same name is replaced. Returns the paths."""
    if source not in SOURCES:
        raise ValueError(f'source {source!r} is not one of {SOURCES}')
    groups = {}
    for row in rows:
        groups.setdefault((row[0], row[1], day_of_ms(int(row[2]))), []).append(row)
    return [write_csv(row_path(rows_dir, venue, symbol, day, source), sorted(group, key=lambda r: int(r[2])))
            for (venue, symbol, day), group in sorted(groups.items())]


def read_csv(path):
    """The rows of a row file (COLUMNS header) as tuples of strings, None for an empty field and the book
    arrays as tuples of decimal strings."""
    arrays = [COLUMNS.index(name) for name in ARRAYS]
    with gzip.open(path, 'rt', newline='') as handle:
        reader = csv.reader(handle)
        if tuple(next(reader)) != COLUMNS:
            raise ValueError(f'{path}: not a row file')
        rows = []
        for row in reader:
            row = [v if v != '' else None for v in row]
            for k in arrays:
                if row[k] is not None:
                    row[k] = tuple(row[k][1:-1].split(',')) if row[k] != '{}' else ()
            rows.append(tuple(row))
        return rows


def contract_rows(job):
    """Writes the archive row files of one contract for the UTC+8 days [first_day, end_day); returns a summary."""
    venue, symbol, history_start_ms, first_day, end_day, rows_dir = job
    pieces = pieces_of(venue, symbol)
    summary = dict(venue=venue, symbol=symbol, files=0, rows=0, first_ms=None, last_ms=None, gaps=[], longest_quiet_ms=0,
                   longest_quiet_until_ms=None, repeated_ids=0, repeats_differ=0, overlap_missing=0, id_breaks=[], holes=[],
                   pieces=dict(Counter(p.kind for p in pieces)))
    if not pieces:
        return summary
    day = day_of_ms(max(history_start_ms, min(p.covered_from_ms for p in pieces)))
    stop = max(p.covered_to_ms for p in pieces)
    carry, cache, previous, last_id = None, {}, None, None
    while day_start_ms(day) < stop and day < end_day:
        start = day_start_ms(day)
        low, high = max(start, history_start_ms), start + DAY_MS
        todays = sorted((p for p in pieces if p.covered_from_ms < high and p.covered_to_ms > low), key=lambda p: (p.priority, p.path))
        for path in [path for path in cache if path not in {p.path for p in todays}]:
            del cache[path]
        parts, held, holes = [], [], []
        for piece in todays:
            if piece.path not in cache:
                cache[piece.path] = piece_trades(piece, venue)
                summary['holes'].extend(cache[piece.path][3])
            ts, ids, prices, piece_holes = cache[piece.path]
            a, b = max(piece.covered_from_ms, low), min(piece.covered_to_ms, high)
            keep = (ts >= a) & (ts < b)
            parts.append((ts[keep], ids[keep], prices[keep]))
            held.append((a, b, ids[keep], ts[keep], piece.kind == 'binance trades archive'))
            holes.extend((h[0], h[1]) for h in piece_holes)
        # Two sources covering the same stretch must hold the same trades there (trade ids and aggregate ids do not compare).
        for i in range(len(held)):
            for j in range(i + 1, len(held)):
                a, b = max(held[i][0], held[j][0]), min(held[i][1], held[j][1])
                if a < b and held[i][4] == held[j][4]:
                    one = held[i][2][(held[i][3] >= a) & (held[i][3] < b)]
                    two = held[j][2][(held[j][3] >= a) & (held[j][3] < b)]
                    summary['overlap_missing'] += len(np.setdiff1d(one, two)) + len(np.setdiff1d(two, one))
        ts, ids, prices, repeated, differ = combine(parts)
        summary['repeated_ids'] += repeated
        summary['repeats_differ'] += differ
        intervals = subtract(merge([(max(p.covered_from_ms, low), min(p.covered_to_ms, high)) for p in todays]), holes)
        if venue == 'binance' and len(ids):
            # Aggregate ids count up by one per contract: a jump is a missing aggregate, or a junction that does not join.
            # The ids of a trades file are no aggregates: the chain leaves them out and starts again after them.
            aggregate = ids < TRADE_ID_SPACE
            by_id = np.argsort(ids[aggregate])
            chain = ids[aggregate][by_id] if last_id is None else np.concatenate(([last_id], ids[aggregate][by_id]))
            stamps = ts[aggregate][by_id] if last_id is None else np.concatenate(([-1], ts[aggregate][by_id]))
            summary['id_breaks'].extend((int(chain[k]), int(chain[k + 1]), int(stamps[k + 1])) for k in np.flatnonzero(np.diff(chain) != 1))
            last_id = int(chain[-1]) if aggregate[-1] else None
        seconds, trade_ms, price, carry = day_rows(start, ts, ids, prices, intervals, carry)
        if day >= first_day and len(seconds):
            write_csv(row_path(rows_dir, venue, symbol, day, 'archive'),
                      zip([venue] * len(seconds), [symbol] * len(seconds), seconds.tolist(), price_text(price), trade_ms.tolist(),
                          *([[None] * len(seconds)] * 5)))
            summary['files'] += 1
            summary['rows'] += len(seconds)
            summary['first_ms'] = summary['first_ms'] or int(seconds[0])
            summary['last_ms'] = int(seconds[-1])
            chain = seconds if previous is None else np.concatenate(([previous], seconds))
            summary['gaps'].extend((int(chain[k]), int(chain[k + 1])) for k in np.flatnonzero(np.diff(chain) > 1000))
            quiet = seconds - trade_ms
            k = int(np.argmax(quiet))
            if quiet[k] > summary['longest_quiet_ms']:
                summary['longest_quiet_ms'], summary['longest_quiet_until_ms'] = int(quiet[k]), int(seconds[k])
            previous = int(seconds[-1])
        day = next_day(day)
    return summary


def archives(first_day, end_day, rows_dir, rest=True, trades_days=()):
    contracts, history_start_ms = read_config()
    log(f'{len(contracts)} contracts from {stamp(history_start_ms)} UTC+8; days {first_day} to {end_day} (excluded) into {rows_dir}'
        + ('' if rest else '; daily archives only, no REST runs')
        + (f'; Binance trades files of the UTC days {", ".join(str(day) for day in trades_days)}' if trades_days else ''))
    download(contracts, history_start_ms, rest, trades_days)
    jobs = [(venue, symbol, history_start_ms, first_day, end_day, str(rows_dir)) for venue, symbol in contracts]
    total = 0
    with ProcessPoolExecutor(min(len(jobs), 12)) as pool:
        for summary in pool.map(contract_rows, jobs):
            total += summary['rows']
            readable = dict(summary, first=summary['first_ms'] and stamp(summary['first_ms']),
                            last=summary['last_ms'] and stamp(summary['last_ms']),
                            gaps=[(stamp(a), stamp(b)) for a, b in summary['gaps']],
                            id_breaks=[(a, b, stamp(t)) for a, b, t in summary['id_breaks']],
                            holes=[(stamp(a), stamp(b), n) for a, b, n in summary['holes']],
                            longest_quiet_until=summary['longest_quiet_until_ms'] and stamp(summary['longest_quiet_until_ms']))
            log('rows ' + json.dumps(readable))
    log(f'{total} rows written')


# ---------------------------------------------------------------- load

def row_files(rows_dir, first_day=None, end_day=None):
    """The row files under rows_dir of the days [first_day, end_day), in load order: archive, interim, live,
    then by venue, contract and day."""
    found = []
    for path in glob.glob(str(Path(rows_dir) / '*' / '*' / '*.csv.gz')):
        match = re.fullmatch(r'(\d{8})\.(\w+)\.csv\.gz', Path(path).name)
        if not match or match.group(2) not in SOURCES:
            continue
        day = match.group(1)
        if (first_day and day < first_day) or (end_day and day >= end_day):
            continue
        parts = Path(path).parts
        found.append((SOURCES.index(match.group(2)), parts[-3], parts[-2], day, path))
    return [item[-1] for item in sorted(found)]


VALUE_COLUMNS = COLUMNS[3:]
MERGE_SQL = f"""
WITH merged AS (
    INSERT INTO etfpair.perp_second AS t ({', '.join(COLUMNS)})
    SELECT {', '.join(COLUMNS)} FROM perp_second_load
    ON CONFLICT (venue, symbol, second_ms) DO UPDATE SET
        {', '.join(f'{c} = COALESCE(t.{c}, EXCLUDED.{c})' for c in VALUE_COLUMNS)}
    WHERE {' OR '.join(f'(t.{c} IS NULL AND EXCLUDED.{c} IS NOT NULL)' for c in VALUE_COLUMNS)}
    RETURNING second_ms, xmax = 0 AS inserted
)
SELECT count(*) FILTER (WHERE inserted) AS inserted, count(*) FILTER (WHERE NOT inserted) AS filled,
       array_agg(second_ms ORDER BY second_ms) AS seconds FROM merged
"""


def stretches(seconds, gap_ms=REFRESH_GAP_MS):
    """(T1 - 1, T2] of every stretch T1..T2 of the sorted seconds without a gap of more than gap_ms: the ranges
    whose minutes the summary re-aggregates."""
    out = []
    for t in seconds:
        if out and t - out[-1][1] <= gap_ms:
            out[-1][1] = t
        else:
            out.append([t - 1, t])
    return [(a, b) for a, b in out]


def joined(ranges, gap_ms=REFRESH_GAP_MS):
    """The union of (a, b] ranges (of several files) as sorted ranges, two joined when the later starts at most gap_ms
    after the earlier ends, as stretches() joins seconds."""
    out = []
    for a, b in sorted(ranges):
        if out and a + 1 - out[-1][1] <= gap_ms:
            out[-1][1] = max(out[-1][1], b)
        else:
            out.append([a, b])
    return [(a, b) for a, b in out]


async def load(dsn, rows_dir, first_day=None, end_day=None):
    import asyncpg  # only load needs the database driver

    files = row_files(rows_dir, first_day, end_day)
    log(f'loading {len(files)} files from {rows_dir}')
    started = time.monotonic()
    totals = Counter()
    unrefreshed = []
    connection = await asyncpg.connect(dsn)
    try:
        await connection.execute('CREATE TEMPORARY TABLE perp_second_load (LIKE etfpair.perp_second) ON COMMIT DELETE ROWS')
        refresh = await connection.fetchval('SELECT to_regprocedure($1) IS NOT NULL', REFRESH + '(bigint, bigint)')
        if not refresh:
            log(f'{REFRESH}(bigint, bigint) does not exist yet (it comes with the schema of board 3.2): '
                'merging without refreshing the per-minute summary')
        for k, path in enumerate(files, 1):
            async with connection.transaction():
                with gzip.open(path, 'rb') as stream:
                    header = tuple(stream.readline().decode().rstrip('\r\n').split(','))
                    if header not in (COLUMNS, SCALAR_COLUMNS):
                        raise ValueError(f'{path}: not a row file')
                    try:  # the fields go to COLUMNS by position: a scalar book value is no array literal
                        status = await connection.copy_to_table('perp_second_load', source=stream, columns=COLUMNS, format='csv')
                    except asyncpg.DataError as error:
                        raise ValueError(f'{path}: {error}') from error
                merged = await connection.fetchrow(MERGE_SQL)
                ranges = stretches(merged['seconds'] or [])
                if refresh:  # in the file's transaction: the rows and their summary commit together
                    for a, b in ranges:
                        totals['refresh_result'] += await connection.fetchval(f'SELECT {REFRESH}($1::bigint, $2::bigint)', a, b) or 0
                    totals['refreshed'] += len(ranges)
                else:
                    unrefreshed.extend(ranges)
            copied = int(status.split()[-1])
            totals.update(files=1, copied=copied, inserted=merged['inserted'], filled=merged['filled'])
            if k % 100 == 0 or k == len(files):
                log(f'{k} of {len(files)} files: {totals["copied"]} rows read, {totals["inserted"]} inserted, '
                    f'{totals["filled"]} filled, {time.monotonic() - started:.0f} s; last {path}')
    finally:
        await connection.close()
    log(f'loaded {totals["files"]} files in {time.monotonic() - started:.1f} s: {totals["copied"]} rows read, '
        f'{totals["inserted"]} inserted, {totals["filled"]} filled, {totals["copied"] - totals["inserted"] - totals["filled"]} unchanged')
    if refresh:
        log(f'{REFRESH} called for {totals["refreshed"]} stretches of changed seconds, returning {totals["refresh_result"]} in all')
    elif unrefreshed:
        ranges = joined(unrefreshed)
        totals['unrefreshed'] = len(ranges)
        log(f'not refreshed: {len(unrefreshed)} stretches of changed seconds in {len(ranges)} ranges, {stamp(ranges[0][0] + 1)} to '
            f'{stamp(ranges[-1][1])} UTC+8; once {REFRESH} exists, run as the writer role:')
        for a, b in ranges:
            log(f'  SELECT {REFRESH}({a}, {b});  -- {stamp(a + 1)} to {stamp(b)} UTC+8')
    return dict(totals)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__.split('\n\n')[0])
    commands = parser.add_subparsers(dest='command', required=True)
    history = commands.add_parser('archives', help='download the missing trade history and write archive row files')
    history.add_argument('--from', dest='first', required=True, type=day_arg, help='first UTC+8 day, YYYY-MM-DD')
    history.add_argument('--to', dest='end', required=True, type=day_arg, help='UTC+8 day after the last, YYYY-MM-DD')
    history.add_argument('--rows', required=True, help='directory of the row files')
    history.add_argument('--no-rest', dest='rest', action='store_false', help='daily archive files only, no REST runs')
    history.add_argument('--binance-trades', metavar='UTCDAY', action='append', default=[], type=dt.date.fromisoformat,
                         help='also download the Binance daily trades files of this UTC day, YYYY-MM-DD (repeatable)')
    loader = commands.add_parser('load', help='merge row files into etfpair.perp_second without overwriting')
    loader.add_argument('--dsn-env', required=True, help='environment variable holding the database DSN')
    loader.add_argument('--rows', required=True, help='directory of the row files')
    loader.add_argument('--from', dest='first', type=day_arg, help='first UTC+8 day, YYYY-MM-DD')
    loader.add_argument('--to', dest='end', type=day_arg, help='UTC+8 day after the last, YYYY-MM-DD')
    args = parser.parse_args(argv)
    if args.command == 'archives':
        archives(args.first, args.end, Path(args.rows), args.rest, sorted(set(args.binance_trades)))
        return 0
    dsn = os.environ.get(args.dsn_env)
    if not dsn:
        raise SystemExit(f'environment variable {args.dsn_env} is not set')
    asyncio.run(load(dsn, args.rows, args.first, args.end))
    return 0


if __name__ == '__main__':
    sys.exit(main())
