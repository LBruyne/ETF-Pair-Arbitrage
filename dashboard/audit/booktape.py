#!/usr/bin/env python3
"""Integrity audit of a booktape recording (the layout of research/booktape.py).

    .venv/bin/python -I dashboard/audit/booktape.py [--from ISO --to ISO] [--root DIR] [--config FILE] [--rest DIR]

Default window: the last complete hour (UTC+8); --from / --to take an ISO time with
its offset (2026-11-20T00:00+08:00). Reports per stream the files, records, first
and last receive time (UTC+8), longest silence and receive-minus-exchange lag (p50,
p95, max), and the events and gaps of every feed. Checks, exit status 1 when one
fails:

  expected   every expected stream of every contract in the config has records:
             its book topic, the one the connections subscribe for it (Bitget books5,
             or books50 for a price tick finer than 0.01; Binance depth5 or depth10),
             and Bitget books1, publicTrade, ticker, funding-rate; Binance bookTicker,
             aggTrade, markPrice
  book       every book frame (Bitget books5 / books50, Binance depth5 / depth10)
             holds at most the topic's levels a side (books50 frames were stored cut
             to 10 before 2026-11-22), bids strictly descending and asks strictly ascending, and is
             never locked or crossed; a Bitget frame is a snapshot (action snapshot):
             the recorder treats every frame as the whole book; Binance pu =
             previous u inside a connection; frames with fewer levels are counted,
             not failures
  fills      Bitget publicTrade fill ids unique; with --rest DIR also equal to the
             public REST fills-history (GET /api/v2/mix/market/fills-history) over the
             parts of the window inside one connection; the raw answers are saved
             under DIR
  aggTrade   Binance a = previous a + 1 inside a connection; trade ids between two
             consecutive aggregates (f != previous l + 1 with a contiguous) are counted,
             not failures: aggTrade leaves out insurance-fund and ADL trades
  markPrice  Binance E steps 1 s (900 to 1100 ms) inside a connection

Gaps reported by the events are listed, not failures. Public market data only:
--rest sends at most 2 requests a second and stops on HTTP 429 or 418.
"""
import argparse
import json
import sys
import time
import urllib.error
import urllib.request
from datetime import datetime, timedelta, timezone
from pathlib import Path

import numpy as np
import yaml

REPO = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(REPO))
from research import booktape  # noqa: E402

EXPECTED = {'bitget': ('books1', 'publicTrade', 'ticker', 'funding-rate'),
            'binance': ('bookTicker', 'aggTrade', 'markPrice')}  # besides the book
BOOK = {'bitget': ('books5', 'books50'), 'binance': ('depth5', 'depth10')}  # the recorder's book topics
UTC8 = timezone(timedelta(hours=8))
REST = 'https://api.bitget.com/api/v2/mix/market/fills-history'
AGENT = 'Mozilla/5.0 (compatible; booktape-audit)'


def utc8(ms):
    return datetime.fromtimestamp(ms / 1000, UTC8).strftime('%Y-%m-%d %H:%M:%S.%f')[:-3]


def contracts(config):
    """{venue: [symbols]} of every combo market, in configuration order."""
    out = {}
    for combo in yaml.safe_load(Path(config).read_text())['combos']:
        for market in combo['markets']:
            if market['venue'] not in EXPECTED:
                raise SystemExit(f'unknown venue {market["venue"]}')
            symbols = out.setdefault(market['venue'], [])
            for symbol in (market['bull_symbol'], market['bear_symbol'], market['ref_symbol']):
                if symbol not in symbols:
                    symbols.append(symbol)
    return out


def stamps(venue, topic, frame):
    """The venue's stamps of a frame: one per fill for Bitget publicTrade."""
    if venue == 'bitget':
        if topic == 'publicTrade':
            return [int(f['T']) for f in frame['data']]
        if topic in ('ticker', 'funding-rate'):
            return [frame['ts']]
        return [int(frame['data'][0]['ts'])]
    data = frame['data']
    return [data['E'] if topic == 'markPrice' else data['T']]


class Stream:
    """Statistics and checks of one stream over the window."""

    def __init__(self, venue, stream, spans):
        self.venue, self.stream, self.spans = venue, stream, spans
        self.symbol, self.topic = stream.split('.', 1)
        self.files = 0
        self.records = 0
        self.first = self.last = None
        self.silence = 0
        self.lags = []
        self.failures = []
        self.notes = []
        self.counts = {}

    def count(self, key, n=1):
        self.counts[key] = self.counts.get(key, 0) + n

    def seen(self, received, frame):
        self.records += 1
        if self.last is not None:
            self.silence = max(self.silence, received - self.last)
        self.first = received if self.first is None else self.first
        self.last = received
        self.lags.extend(received - stamp for stamp in stamps(self.venue, self.topic, frame))

    def connection(self, received):
        return booktape.connection_at(self.spans, received)

    def report(self):
        lag = np.asarray(self.lags)
        text = (f'  {self.stream:<24} files {self.files:>3}  records {self.records:>9}  '
                f'{utc8(self.first) if self.first else "-":>23} .. {utc8(self.last) if self.last else "-":>23}  '
                f'silence {self.silence / 1000:>7.3f} s')
        if len(lag):
            text += f'  lag p50 {np.percentile(lag, 50):.0f} p95 {np.percentile(lag, 95):.0f} max {lag.max()} ms'
        for key, value in sorted(self.counts.items()):
            text += f'\n      {key}: {value}'
        for note in self.notes:
            text += f'\n      note: {note}'
        for failure in self.failures:
            text += f'\n      FAIL: {failure}'
        return text


def window_frames(venue, stream, lo, hi, root, start=None):
    """(received, frame, inside the window, file path) of the records from start (or lo) to hi."""
    symbol, topic = stream.split('.', 1)
    begin = lo if start is None else start
    for path in booktape.files(venue, symbol, topic, begin, hi, root):
        for line in booktape.lines(path):
            received, payload = booktape.split_line(line)
            if received >= hi:
                return
            if received >= begin:
                yield received, json.loads(payload), received >= lo, path


def audit_plain(s, venue, lo, hi, root):
    """Streams without a check of their own: statistics only."""
    files = set()
    for received, frame, inside, path in window_frames(venue, s.stream, lo, hi, root):
        files.add(path)
        s.seen(received, frame)
    s.files = len(files)


def audit_book(s, venue, lo, hi, root):
    """Bitget books5 / books50 and Binance depth5 / depth10: self-contained snapshots."""
    levels = booktape.LEVELS[s.topic]
    files = set()
    previous, conn = None, None
    for received, frame, inside, path in window_frames(venue, s.stream, lo, hi, root):
        files.add(path)
        s.seen(received, frame)
        _, bids, asks, _ = booktape.book_frame(venue, s.topic, frame)
        bid = [float(level[0]) for level in bids]
        ask = [float(level[0]) for level in asks]
        if len(bid) > levels or len(ask) > levels:
            s.count(f'frames with more than {levels} levels a side')
        elif len(bid) < levels or len(ask) < levels:
            s.count(f'frames with fewer than {levels} levels a side')
        if any(b <= c for b, c in zip(bid, bid[1:])) or any(a >= c for a, c in zip(ask, ask[1:])):
            s.count('frames whose levels are not best first')
        if bid and ask and bid[0] >= ask[0]:
            s.count('locked or crossed frames')
        if venue == 'bitget' and frame.get('action') != 'snapshot':
            s.count('frames that are not snapshots')
        if venue == 'binance':
            here = s.connection(received)
            if here != conn:
                conn, previous = here, None
                s.count('connections')
            if previous is not None and frame['data']['pu'] != previous:
                s.count('pu != previous u inside a connection')
            previous = frame['data']['u']
    s.files = len(files)
    for key, value in s.counts.items():
        if key not in ('connections', f'frames with fewer than {levels} levels a side') and value:
            s.failures.append(f'{key}: {value}')


def audit_fills(s, lo, hi, root):
    ids = set()
    fills = {}
    files = set()
    for received, frame, inside, path in window_frames('bitget', s.stream, lo, hi, root):
        files.add(path)
        s.seen(received, frame)
        for f in frame['data']:
            if f['i'] in ids:
                s.count('duplicate fill ids')
            ids.add(f['i'])
            fills[f['i']] = f
    s.files = len(files)
    s.count('fills', len(ids))
    if s.counts.get('duplicate fill ids'):
        s.failures.append(f'duplicate fill ids: {s.counts["duplicate fill ids"]}')
    return fills


def audit_chain(s, venue, lo, hi, root):
    """Binance aggTrade and markPrice."""
    files = set()
    previous, conn = None, None
    for received, frame, inside, path in window_frames(venue, s.stream, lo, hi, root):
        files.add(path)
        s.seen(received, frame)
        d = frame['data']
        here = s.connection(received)
        if here != conn:
            conn, previous = here, None
            s.count('connections')
        if s.topic == 'aggTrade':
            if previous is not None:
                if d['a'] != previous['a'] + 1:
                    s.count('a != previous a + 1 inside a connection')
                elif d['f'] != previous['l'] + 1:
                    s.count('trade ids in no aggregate (f != previous l + 1 with a contiguous)', d['f'] - previous['l'] - 1)
        elif s.topic == 'markPrice':
            if previous is not None and not 900 <= d['E'] - previous['E'] <= 1100:
                s.count('E step outside 900-1100 ms inside a connection')
        previous = d
    s.files = len(files)
    for key, value in s.counts.items():
        if key != 'connections' and not key.startswith('trade ids in no aggregate') and value:
            s.failures.append(f'{key}: {value}')


def rest_fills(symbol, a, b, out):
    """Fills of the public REST history with a <= ts <= b, paging back by idLessThan;
    every answer is saved with its URL and retrieval time."""
    rows, cursor = {}, None
    path = Path(out) / f'fills-history-{symbol}-{a}-{b}.jsonl'
    path.parent.mkdir(parents=True, exist_ok=True)
    with open(path, 'w') as saved:
        while True:
            url = f'{REST}?symbol={symbol}&productType=usdt-futures&limit=1000&startTime={a - 1000}&endTime={b + 1000}'
            if cursor:
                url += f'&idLessThan={cursor}'
            request = urllib.request.Request(url, headers={'User-Agent': AGENT})
            retrieved = int(time.time() * 1000)
            try:
                with urllib.request.urlopen(request, timeout=15) as answer:
                    status, body = answer.status, answer.read().decode()
            except urllib.error.HTTPError as error:
                status, body = error.code, error.read().decode(errors='replace')
            saved.write(json.dumps(dict(url=url, retrieved_ms=retrieved, status=status, body=body)) + '\n')
            if status != 200:
                raise RuntimeError(f'REST {status} for {url}: {body[:200]}')
            data = json.loads(body).get('data') or []
            for row in data:
                rows[row['tradeId']] = row
            time.sleep(0.5)
            if len(data) < 1000 or min(int(row['ts']) for row in data) < a:
                break
            cursor = min(data, key=lambda row: int(row['tradeId']))['tradeId']
    return {i: row for i, row in rows.items() if a <= int(row['ts']) <= b}, path


def compare_rest(s, fills, lo, hi, out):
    for start, end in s.spans:
        a, b = max(start, lo) + 1000, min(end, hi) - 1000
        if b <= a:
            continue
        try:
            rest, path = rest_fills(s.symbol, a, b, out)
        except (RuntimeError, OSError) as error:
            s.failures.append(f'REST fills-history not compared over {utc8(a)} .. {utc8(b)}: {error}')
            return
        ws = {i: f for i, f in fills.items() if a <= int(f['T']) <= b}
        missing_rest = [i for i in ws if i not in rest]
        missing_ws = [i for i in rest if i not in ws]
        differ = [i for i, f in ws.items() if i in rest and (
            float(rest[i]['price']) != float(f['p']) or float(rest[i]['size']) != float(f['v'])
            or rest[i]['side'].lower() != f['S'].lower() or rest[i]['ts'] != f['T'])]
        s.notes.append(f'REST fills-history {utc8(a)} .. {utc8(b)}: websocket {len(ws)}, REST {len(rest)}, '
                       f'missing in REST {len(missing_rest)}, missing in websocket {len(missing_ws)}, '
                       f'different {len(differ)} ({path})')
        if missing_rest or missing_ws or differ:
            s.failures.append(f'REST fills-history differs over {utc8(a)} .. {utc8(b)}')


def main():
    parser = argparse.ArgumentParser(description=__doc__.split('\n')[0])
    parser.add_argument('--from', dest='lo')
    parser.add_argument('--to', dest='hi')
    parser.add_argument('--root', default=str(booktape.ROOT))
    parser.add_argument('--config', default=str(REPO / 'dashboard' / 'config.yaml'))
    parser.add_argument('--rest', metavar='DIR', help='compare Bitget fills with the public REST history, saving the answers here')
    a = parser.parse_args()
    ms = lambda text: int(datetime.fromisoformat(text).timestamp() * 1000)
    hour = booktape.hour_start(int(time.time() * 1000))
    lo = ms(a.lo) if a.lo else hour - booktape.HOUR
    hi = ms(a.hi) if a.hi else hour
    started = time.time()
    print(f'booktape audit {utc8(lo)} .. {utc8(hi)} UTC+8, root {a.root}')
    failed = []
    for venue, symbols in contracts(a.config).items():
        conns = booktape.connections(venue, root=a.root)
        present = booktape.listing(venue, lo, hi, a.root)
        print(f'\n{venue}')
        found = booktape.events(venue, lo, hi, a.root)
        kinds = {}
        for event in found:
            kinds[event['event']] = kinds.get(event['event'], 0) + 1
        print('  events: ' + (', '.join(f'{k} {v}' for k, v in sorted(kinds.items())) or 'none'))
        for event in found:
            payload = event.get('payload')
            if event['event'] != 'control' or isinstance(payload, dict) and payload.get('event') == 'error':
                detail = {k: v for k, v in event.items() if k not in ('received_ms', 'feed', 'event', 'request')}
                print(f'    {utc8(event["received_ms"])} {event["feed"]} {event["event"]} {json.dumps(detail)[:160]}')
        for feed, holes in sorted(booktape.gaps(venue, lo, hi, a.root).items()):
            print(f'  gaps {feed}: ' + (', '.join(f'{utc8(x)} .. {utc8(y)} ({(y - x) / 1000:.1f} s)' for x, y in holes) or 'none'))
        missing = []
        for symbol in symbols:
            try:
                topic = booktape.book_topic_of(venue, symbol, lo, hi, a.root, conns)
            except ValueError as error:
                failed.append(str(error))
                topic = None
            if topic not in BOOK[venue]:
                failed.append(f'{venue} {symbol}: the connections subscribe no book topic of {", ".join(BOOK[venue])}'
                              + (f' (but {topic})' if topic else ''))
            missing += [name for name in ([f'{symbol}.{topic}'] if topic in BOOK[venue] else []) + [f'{symbol}.{t}' for t in EXPECTED[venue]]
                        if name not in present]
        streams = {}
        for stream in sorted(present):
            s = streams[stream] = Stream(venue, stream, booktape.carriers(venue, stream, a.root, conns))
            if s.topic in BOOK[venue]:
                audit_book(s, venue, lo, hi, a.root)
            elif venue == 'bitget' and s.topic == 'publicTrade':
                fills = audit_fills(s, lo, hi, a.root)
                if a.rest:
                    compare_rest(s, fills, lo, hi, a.rest)
            elif venue == 'binance' and s.topic in ('aggTrade', 'markPrice'):
                audit_chain(s, venue, lo, hi, a.root)
            else:
                audit_plain(s, venue, lo, hi, a.root)
            if s.records == 0:
                missing.append(stream)
        print('  streams:')
        for s in streams.values():
            print(s.report())
            failed += [f'{venue} {s.stream}: {f}' for f in s.failures]
        if missing:
            failed.append(f'{venue}: expected streams without records: {", ".join(missing)}')
    print(f'\nchecked in {time.time() - started:.1f} s')
    if failed:
        print('FAILED:\n  ' + '\n  '.join(failed))
        sys.exit(1)
    print('PASSED')


if __name__ == '__main__':
    main()
