"""Tests of perp_second.py (history rows, row files, loader) and perp_second_recordings.py.

    .venv/bin/python -I -m unittest discover -s dashboard/backfill -p 'test_*.py'

PerpSecondLoadTest writes into the database named by the environment variable PERP_SECOND_TEST_DSN and is
skipped without it: point it at a throwaway database only. One of its tests applies the recorder's and the
collector's schema files (dashboard/internal/booktape/schema.sql, dashboard/internal/scan/schema.sql) to that
database, to load against the real etfpair.refresh_perp_second_minute and etfpair.perp_coverage.
"""
import asyncio
import bisect
import csv
import datetime as dt
import gzip
import io
import json
import os
import random
import tempfile
import unittest
import urllib.parse
import zipfile
from pathlib import Path

import numpy as np

import perp_second as ps

DAY = '20261118'
DAY_MS = ps.day_start_ms(DAY)


def reference_rows(day_ms, trades, intervals, carry):
    """Rows by the definition, one second at a time: the newest covered trade c before T (larger id on equal
    time), or the carry when the day has none; the row exists when [c, T) lies inside the coverage (for the
    carry, [day start, T) with a valid carry)."""
    merged = ps.merge(intervals)

    def inside(start, end):
        return start >= end or any(a <= start and end <= b for a, b in merged)

    ordered = sorted(trades, key=lambda x: (x[0], x[1]))
    times = [x[0] for x in ordered]
    out = []
    for k in range(86_400):
        t = day_ms + 1000 * k
        before = bisect.bisect_left(times, t)  # trades [0, before) are earlier than t
        if before:
            newest = ordered[before - 1]  # the latest time, and of equal times the larger id
            if inside(newest[0], t):
                out.append((t, newest[0], newest[2]))
        elif carry is not None and inside(day_ms, t):
            out.append((t, carry[0], carry[2]))
    return out


def snowflake(ts_ms, sequence):
    return ((ts_ms - ps.SNOWFLAKE_OFFSET_MS) << 22) | sequence


class PerpSecondDayRowsTest(unittest.TestCase):
    def rows(self, trades, intervals, carry=None):
        trades = sorted(trades, key=lambda x: (x[0], x[1]))
        ts = np.array([x[0] for x in trades], dtype=np.int64)
        ids = np.array([x[1] for x in trades], dtype=np.int64)
        prices = np.array([x[2] for x in trades], dtype=np.float64)
        seconds, trade_ms, price, following = ps.day_rows(DAY_MS, ts, ids, prices, ps.merge(intervals), carry)
        return list(zip(seconds.tolist(), trade_ms.tolist(), price.tolist())), following

    def test_row_is_newest_trade_strictly_before_the_second(self):
        rows, _ = self.rows([(DAY_MS + 999, 1, 10.0), (DAY_MS + 1000, 2, 11.0), (DAY_MS + 1500, 3, 12.0)],
                            [(DAY_MS, DAY_MS + ps.DAY_MS)])
        self.assertEqual(rows[0], (DAY_MS + 1000, DAY_MS + 999, 10.0))  # the trade at T itself is not before T
        self.assertEqual(rows[1], (DAY_MS + 2000, DAY_MS + 1500, 12.0))
        self.assertEqual(len(rows), 86_399)  # dense from the first whole second after the first trade

    def test_equal_times_take_the_larger_id(self):
        rows, _ = self.rows([(DAY_MS + 500, 7, 10.0), (DAY_MS + 500, 9, 10.5), (DAY_MS + 500, 8, 10.2)],
                            [(DAY_MS, DAY_MS + 5000)])
        self.assertEqual(rows[0], (DAY_MS + 1000, DAY_MS + 500, 10.5))

    def test_carry_starts_the_day_and_a_hole_stops_it(self):
        carry = (DAY_MS - 300, 1, 9.0)
        rows, following = self.rows([(DAY_MS + 70_500, 5, 11.0)], [(DAY_MS, DAY_MS + 60_000), (DAY_MS + 70_000, DAY_MS + 90_000)], carry)
        seconds = [r[0] for r in rows]
        self.assertEqual(rows[0], (DAY_MS, DAY_MS - 300, 9.0))
        self.assertEqual(seconds[:61], [DAY_MS + 1000 * k for k in range(61)])  # through the end of the first interval
        self.assertEqual(seconds[61], DAY_MS + 71_000)  # after the hole: the first whole second after a trade
        self.assertEqual(seconds[-1], DAY_MS + 90_000)
        self.assertIsNone(following)  # the coverage does not reach the next day

    def test_carry_alone_gives_the_first_second(self):
        rows, _ = self.rows([], [(DAY_MS + 5000, DAY_MS + 9000)], (DAY_MS - 1, 1, 9.0))
        self.assertEqual(rows, [(DAY_MS, DAY_MS - 1, 9.0)])

    def test_no_row_without_any_trade(self):
        rows, following = self.rows([], [(DAY_MS, DAY_MS + ps.DAY_MS)])
        self.assertEqual(rows, [])
        self.assertIsNone(following)

    def test_carry_into_the_next_day(self):
        _, following = self.rows([(DAY_MS + 10, 3, 10.0), (DAY_MS + 86_399_999, 4, 10.25)], [(DAY_MS, DAY_MS + ps.DAY_MS)])
        self.assertEqual(following, (DAY_MS + 86_399_999, 4, 10.25))
        rows, following = self.rows([], [(DAY_MS, DAY_MS + ps.DAY_MS)], (DAY_MS - 5, 2, 9.0))
        self.assertEqual(len(rows), 86_400)
        self.assertEqual(following, (DAY_MS - 5, 2, 9.0))  # a covered day without trades passes the carry on

    def test_matches_the_definition_on_random_days(self):
        generator = random.Random(7)
        for case in range(40):
            cuts = sorted(generator.sample(range(0, 86_400_001, 250), 6))
            intervals = [(DAY_MS + cuts[k], DAY_MS + cuts[k + 1]) for k in range(0, 6, 2)]
            if case % 3 == 0:
                intervals[0] = (DAY_MS, intervals[0][1])
            if case % 4 == 0:
                intervals = [(DAY_MS, DAY_MS + ps.DAY_MS)]
            trades = []
            for a, b in intervals:
                for _ in range(generator.randrange(0, 40)):
                    t = generator.randrange(a, b)
                    trades.append((t, generator.randrange(1, 10**9), float(generator.randrange(1000, 2000)) / 100))
                    if generator.random() < 0.3:  # same millisecond, other id
                        trades.append((t, generator.randrange(1, 10**9), float(generator.randrange(1000, 2000)) / 100))
            carry = (DAY_MS - 10, 0, 1.0) if case % 2 else None
            got, _ = self.rows(trades, intervals, carry)
            self.assertEqual(got, reference_rows(DAY_MS, trades, intervals, carry), f'case {case}')


class PerpSecondCombineTest(unittest.TestCase):
    def test_first_source_wins_and_differences_are_counted(self):
        archive = (np.array([5, 3]), np.array([2, 1]), np.array([10.0, 9.0]))
        rest = (np.array([5, 3, 7]), np.array([2, 1, 3]), np.array([10.0, 9.5, 11.0]))
        ts, ids, prices, repeated, differ = ps.combine([archive, rest])
        self.assertEqual(ts.tolist(), [3, 5, 7])
        self.assertEqual(ids.tolist(), [1, 2, 3])
        self.assertEqual(prices.tolist(), [9.0, 10.0, 11.0])
        self.assertEqual((repeated, differ), (2, 1))

    def test_merge_joins_touching_intervals(self):
        self.assertEqual(ps.merge([(5, 9), (0, 5), (12, 15), (14, 20), (30, 30)]), [(0, 9), (12, 20)])


class PerpSecondConfigTest(unittest.TestCase):
    def test_repository_configuration(self):
        contracts, start = ps.read_config()
        self.assertEqual(len(contracts), len(set(contracts)))
        self.assertIn(('bitget', 'SOXLUSDT'), contracts)
        self.assertIn(('binance', 'QQQUSDT'), contracts)
        self.assertEqual(start % 86_400_000, 0)  # midnight UTC

    def test_order_duplicates_and_errors(self):
        text = ('dsn_env: X\ncombos:\n  - id: A\n    markets:\n'
                '      - { venue: binance, bull_symbol: AUSDT, bear_symbol: BUSDT, ref_symbol: CUSDT }\n'
                '      - { venue: bitget,  bull_symbol: AUSDT, bear_symbol: BUSDT, ref_symbol: "" }\n'
                '  - id: B\n    markets:\n      - { venue: binance, bull_symbol: BUSDT, bear_symbol: DUSDT, ref_symbol: CUSDT }\n'
                'fee_levels:\n  - { venue: bitget, level: account }\nhistory_start: 2026-07-16    # comment\n')
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder) / 'config.yaml'
            path.write_text(text)
            contracts, start = ps.read_config(path)
            self.assertEqual(contracts, [('binance', 'AUSDT'), ('binance', 'BUSDT'), ('binance', 'CUSDT'), ('bitget', 'AUSDT'),
                                         ('bitget', 'BUSDT'), ('binance', 'DUSDT')])
            self.assertEqual(start, int(dt.datetime(2026, 7, 16, tzinfo=dt.timezone.utc).timestamp() * 1000))
            path.write_text(text.replace('venue: bitget,  bull', 'venue: okx, bull'))
            with self.assertRaises(SystemExit):
                ps.read_config(path)
            path.write_text(text + 'more:\n  - venue: binance\n    bull_symbol: EUSDT\n')
            with self.assertRaises(SystemExit):
                ps.read_config(path)


class PerpSecondRowFileTest(unittest.TestCase):
    def test_write_read_and_layout(self):
        with tempfile.TemporaryDirectory() as folder:
            rows = [('bitget', 'AUSDT', DAY_MS + 2000, '10.5', DAY_MS + 1500, None, None, None, None, None),
                    ('bitget', 'AUSDT', DAY_MS - 1000, '10', DAY_MS - 1200, ('9.9', '9.8'), ('2', '1'), ('10.1',), ('3',), DAY_MS - 1100),
                    ('bitget', 'AUSDT', DAY_MS + 1000, None, None, ('9.8',), ('1',), (), (), DAY_MS + 900)]  # an empty ask side
            paths = ps.write_rows(folder, 'live', rows)
            names = sorted(str(Path(p).relative_to(folder)) for p in paths)
            self.assertEqual(names, ['bitget/AUSDT/20261117.live.csv.gz', 'bitget/AUSDT/20261118.live.csv.gz'])
            back = ps.read_csv(Path(folder) / 'bitget' / 'AUSDT' / '20261118.live.csv.gz')
            self.assertEqual(back, [('bitget', 'AUSDT', str(DAY_MS + 1000), None, None, ('9.8',), ('1',), (), (), str(DAY_MS + 900)),
                                    ('bitget', 'AUSDT', str(DAY_MS + 2000), '10.5', str(DAY_MS + 1500), None, None, None, None, None)])
            self.assertEqual(ps.read_csv(Path(folder) / 'bitget' / 'AUSDT' / '20261117.live.csv.gz')[0][5:9],
                             (('9.9', '9.8'), ('2', '1'), ('10.1',), ('3',)))
            with gzip.open(Path(folder) / 'bitget' / 'AUSDT' / '20261117.live.csv.gz', 'rt') as handle:
                self.assertEqual(handle.read().splitlines(), [
                    'venue,symbol,second_ms,price,trade_ms,bid_px,bid_sz,ask_px,ask_sz,book_ms',
                    f'bitget,AUSDT,{DAY_MS - 1000},10,{DAY_MS - 1200},"{{9.9,9.8}}","{{2,1}}",{{10.1}},{{3}},{DAY_MS - 1100}'])
            with self.assertRaises(ValueError):
                ps.write_rows(folder, 'other', rows)

    def test_price_text(self):
        self.assertEqual(ps.price_text(np.array([143.0, 55.55, 0.001, 55.55, 1234.5678])), ['143', '55.55', '0.001', '55.55', '1234.5678'])

    def test_load_order_and_days(self):
        with tempfile.TemporaryDirectory() as folder:
            for name in ['binance/B/20261118.live.csv.gz', 'binance/A/20261119.archive.csv.gz', 'bitget/A/20261117.interim.csv.gz',
                         'bitget/A/20261118.archive.csv.gz', 'bitget/A/20261118.other.csv.gz', 'bitget/A/notes.txt']:
                (Path(folder) / name).parent.mkdir(parents=True, exist_ok=True)
                (Path(folder) / name).write_text('')
            order = [str(Path(p).relative_to(folder)) for p in ps.row_files(folder)]
            self.assertEqual(order, ['binance/A/20261119.archive.csv.gz', 'bitget/A/20261118.archive.csv.gz',
                                     'bitget/A/20261117.interim.csv.gz', 'binance/B/20261118.live.csv.gz'])
            order = [str(Path(p).relative_to(folder)) for p in ps.row_files(folder, '20261118', '20261119')]
            self.assertEqual(order, ['bitget/A/20261118.archive.csv.gz', 'binance/B/20261118.live.csv.gz'])


class FakeVenue:
    """Answers fills-history and aggTrades requests from a list of trades, like the venues page them."""

    def __init__(self, trades):
        self.trades = sorted(trades)  # (ts, id, price)
        self.requests = []

    def request(self, url, body=None):
        self.requests.append(url)
        query = dict(urllib.parse.parse_qsl(urllib.parse.urlsplit(url).query))
        limit = int(query['limit'])
        if 'bitget' in url:
            rows = [t for t in reversed(self.trades) if int(query['startTime']) <= t[0] <= int(query['endTime'])
                    and ('idLessThan' not in query or t[1] < int(query['idLessThan']))][:limit]
            data = [dict(tradeId=str(i), price=str(p), size='1', side='Buy', ts=str(t), symbol='AUSDT') for t, i, p in rows]
            return 200, {}, json.dumps(dict(code='00000', msg='success', data=data)).encode()
        if 'fromId' in query:
            rows = [t for t in sorted(self.trades, key=lambda x: x[1]) if t[1] >= int(query['fromId'])][:limit]
        else:
            rows = [t for t in sorted(self.trades, key=lambda x: x[1]) if int(query['startTime']) <= t[0] <= int(query['endTime'])][:limit]
        data = [dict(a=i, p=str(p), q='1', f=i, l=i, T=t, m=False) for t, i, p in rows]
        return 200, {}, json.dumps(data).encode()


class PerpSecondSourcesTest(unittest.TestCase):
    def setUp(self):
        self.folder = tempfile.TemporaryDirectory()
        root = Path(self.folder.name)
        self.saved = ps.RAW, ps.MANIFEST, ps.TICK_STORE, ps.REPO
        ps.RAW, ps.MANIFEST, ps.TICK_STORE, ps.REPO = root / 'raw', root / 'manifest.csv', root / 'ticks', root

    def tearDown(self):
        ps.RAW, ps.MANIFEST, ps.TICK_STORE, ps.REPO = self.saved
        self.folder.cleanup()

    def test_bitget_rest_run_pages_back_and_covers_its_window(self):
        now = ps.now_ms()
        trades = [(now - 3_000_000 + 997 * k, snowflake(now - 3_000_000 + 997 * k, k % 4), 10 + k % 7) for k in range(2500)]
        venue = FakeVenue(trades)
        ps.bitget_rest(venue, 'AUSDT', now - 2_000_000)
        piece, = [p for p in ps.pieces_of('bitget', 'AUSDT') if p.priority == ps.REST]
        self.assertEqual(piece.covered_from_ms, now - 2_000_000)
        self.assertLessEqual(piece.covered_to_ms, ps.now_ms() - ps.REST_MARGIN_MS)
        ts, ids, prices, holes = ps.piece_trades(piece, 'bitget')
        self.assertEqual(holes, [])
        wanted = {t[1] for t in trades if t[0] >= now - 2_000_000}
        self.assertTrue(wanted <= set(ids.tolist()))
        self.assertIn('idLessThan', venue.requests[-1])

    def test_binance_rest_run_pages_forward_to_now(self):
        now = ps.now_ms()
        trades = [(now - 5_000_000 + 1999 * k, 100 + k, 20 + k % 3) for k in range(2400)]
        venue = FakeVenue(trades)
        ps.binance_rest(venue, 'AUSDT', now - 4_000_000)
        piece, = ps.pieces_of('binance', 'AUSDT')
        end = json.loads(Path(piece.path[:-len('.jsonl.gz')] + '.json').read_text())
        self.assertEqual(end['id_breaks'], 0)
        ts, ids, prices, holes = ps.piece_trades(piece, 'binance')
        self.assertEqual(holes, [])
        self.assertEqual(ids.min(), min(t[1] for t in trades if t[0] >= now - 4_000_000))
        self.assertEqual(ids.max(), trades[-1][1])

    def test_archive_and_rest_rows_of_a_contract(self):
        day_ms = ps.day_start_ms(DAY)
        folder = ps.RAW / 'bitget' / 'trades' / 'AUSDT'
        folder.mkdir(parents=True)
        lines = ['trade_id,timestamp,price,side,volume(quote),size(base)']
        archive = [(day_ms + 400, 1, '10.10'), (day_ms + 400, 2, '10.2'), (day_ms + 86_399_500, 0, '10.3')]
        for t, s, p in archive:
            lines.append(f'{snowflake(t, s)},{t // 1000 * 1000},{p},buy,1,1')
        with zipfile.ZipFile(folder / f'AUSDT_{DAY}_001.zip', 'w') as z:
            z.writestr(f'AUSDT_UMCBL_{DAY}_001.csv', '\n'.join(lines) + '\n')
        (folder / f'{DAY}.complete').write_text(json.dumps(dict(day=DAY, files=[f'AUSDT_{DAY}_001.zip'])))
        # A REST run over the end of the archived day and the first minute of the next: the repeated fill is kept once.
        rest = ps.RAW / 'bitget' / 'rest_fills'
        rest.mkdir(parents=True)
        fills = [(day_ms + 86_399_500, 0, '10.3'), (day_ms + 86_430_000, 5, '10.4')]
        page = dict(payload=dict(code='00000', data=[dict(tradeId=str(snowflake(t, s)), price=p, size='1', side='buy', ts=str(t))
                                                     for t, s, p in reversed(fills)]))
        with gzip.open(rest / 'AUSDT-1.jsonl.gz', 'wt') as handle:
            handle.write(json.dumps(page) + '\n')
        (rest / 'AUSDT-1.json').write_text(json.dumps(dict(covered_from_ms=day_ms + 86_000_000, covered_to_ms=day_ms + 86_460_000)))
        rows_dir = Path(self.folder.name) / 'rows'
        summary = ps.contract_rows(('bitget', 'AUSDT', day_ms, DAY, '20261120', str(rows_dir)))
        self.assertEqual(summary['repeated_ids'], 1)
        self.assertEqual(summary['repeats_differ'], 0)
        self.assertEqual(summary['overlap_missing'], 0)
        first = ps.read_csv(rows_dir / 'bitget' / 'AUSDT' / f'{DAY}.archive.csv.gz')
        self.assertEqual(first[0], ('bitget', 'AUSDT', str(day_ms + 1000), '10.2', str(day_ms + 400), None, None, None, None, None))
        self.assertEqual(len(first), 86_399)
        second = ps.read_csv(rows_dir / 'bitget' / 'AUSDT' / '20261119.archive.csv.gz')
        self.assertEqual(second[0][2:5], (str(day_ms + 86_400_000), '10.3', str(day_ms + 86_399_500)))
        self.assertEqual(second[31][2:5], (str(day_ms + 86_431_000), '10.4', str(day_ms + 86_430_000)))
        self.assertEqual(second[-1][2], str(day_ms + 86_460_000))
        self.assertEqual(summary['gaps'], [])

    def test_binance_archive_rest_junction_and_id_breaks(self):
        utc_ms = int(dt.datetime(2026, 11, 18, tzinfo=dt.timezone.utc).timestamp() * 1000)  # 08:00 UTC+8
        folder = ps.RAW / 'binance' / 'aggTrades' / 'AUSDT'
        folder.mkdir(parents=True)
        lines = ['agg_trade_id,price,quantity,first_trade_id,last_trade_id,transact_time,is_buyer_maker']
        lines += [f'{i},{p},1,{i},{i},{t},false' for i, p, t in [(10, '5.5', utc_ms + 100), (11, '5.6', utc_ms + 100), (12, '5.7', utc_ms + 86_399_000)]]
        with zipfile.ZipFile(folder / 'AUSDT-aggTrades-2026-11-18.zip', 'w') as z:
            z.writestr('AUSDT-aggTrades-2026-11-18.csv', '\n'.join(lines) + '\n')
        rest = ps.RAW / 'binance' / 'rest_aggtrades'
        rest.mkdir(parents=True)
        page = dict(payload=[dict(a=i, p=p, q='1', f=i, l=i, T=t, m=True) for i, p, t in [(14, '5.8', utc_ms + 86_400_500), (15, '5.9', utc_ms + 86_401_000)]])
        with gzip.open(rest / 'AUSDT-2.jsonl.gz', 'wt') as handle:
            handle.write(json.dumps(page) + '\n')
        (rest / 'AUSDT-2.json').write_text(json.dumps(dict(covered_from_ms=utc_ms + ps.DAY_MS, covered_to_ms=utc_ms + ps.DAY_MS + 5000)))
        rows_dir = Path(self.folder.name) / 'rows'
        summary = ps.contract_rows(('binance', 'AUSDT', utc_ms, '20261101', '20261201', str(rows_dir)))
        self.assertEqual(summary['id_breaks'], [(12, 14, utc_ms + 86_400_500)])  # aggregate 13 is in neither source
        # The UTC day starts at 08:00 UTC+8: its rows fall in two UTC+8 files.
        rows = (ps.read_csv(rows_dir / 'binance' / 'AUSDT' / '20261118.archive.csv.gz')
                + ps.read_csv(rows_dir / 'binance' / 'AUSDT' / '20261119.archive.csv.gz'))
        self.assertEqual(rows[0][2:5], (str(utc_ms + 1000), '5.6', str(utc_ms + 100)))  # 11 is the later of two at 100 ms
        self.assertEqual(rows[-1][2:5], (str(utc_ms + ps.DAY_MS + 5000), '5.9', str(utc_ms + 86_401_000)))
        self.assertEqual(summary['rows'], 86_400 + 5)

    def test_skipped_trade_ids_cut_the_coverage(self):
        utc_ms = int(dt.datetime(2026, 10, 27, tzinfo=dt.timezone.utc).timestamp() * 1000)
        folder = ps.RAW / 'binance' / 'aggTrades' / 'AUSDT'
        folder.mkdir(parents=True)
        # Aggregate 2 skips 1 trade id (routine), aggregate 3 skips 500: no rows between its neighbours' times.
        rows = [(1, '5.0', 10, 10, utc_ms + 100), (2, '5.1', 12, 13, utc_ms + 2_000), (3, '5.2', 514, 514, utc_ms + 10_500),
                (4, '5.3', 515, 515, utc_ms + 20_000)]
        lines = ['agg_trade_id,price,quantity,first_trade_id,last_trade_id,transact_time,is_buyer_maker']
        lines += [f'{a},{p},1,{f},{l},{t},false' for a, p, f, l, t in rows]
        with zipfile.ZipFile(folder / 'AUSDT-aggTrades-2026-10-27.zip', 'w') as z:
            z.writestr('AUSDT-aggTrades-2026-10-27.csv', '\n'.join(lines) + '\n')
        rows_dir = Path(self.folder.name) / 'rows'
        summary = ps.contract_rows(('binance', 'AUSDT', utc_ms, '20261001', '20261101', str(rows_dir)))
        self.assertEqual(summary['holes'], [(utc_ms + 2_000, utc_ms + 10_500, 500)])
        seconds = [int(r[2]) - utc_ms for r in ps.read_csv(rows_dir / 'binance' / 'AUSDT' / '20261027.archive.csv.gz')]
        self.assertEqual(seconds[:3], [1000, 2000, 11_000])  # 2000 is the last row before the hole, 11000 the first after
        self.assertEqual(summary['gaps'], [(utc_ms + 2_000, utc_ms + 11_000)])

    def test_subtract(self):
        self.assertEqual(ps.subtract([(0, 10), (20, 30)], [(5, 7), (8, 22), (29, 40)]), [(0, 5), (7, 8), (22, 29)])

    @staticmethod
    def zip_file(path, header, lines):
        Path(path).parent.mkdir(parents=True, exist_ok=True)
        with zipfile.ZipFile(path, 'w') as z:
            z.writestr(Path(path).name[:-4] + '.csv', '\n'.join([header] + lines) + '\n')
        return Path(path).read_bytes()

    def test_a_trades_file_replaces_the_aggregates_of_its_day(self):
        utc_ms = int(dt.datetime(2026, 10, 27, tzinfo=dt.timezone.utc).timestamp() * 1000)
        header = 'agg_trade_id,price,quantity,first_trade_id,last_trade_id,transact_time,is_buyer_maker'
        days = {'2026-10-26': [(1, '5.0', 10, 10, utc_ms - 50_000), (2, '5.1', 11, 11, utc_ms - 20_000)],
                '2026-10-27': [(3, '5.2', 12, 12, utc_ms + 1_000), (4, '5.3', 513, 513, utc_ms + 40_500)],  # skips 500 trade ids
                '2026-10-28': [(5, '5.4', 514, 514, utc_ms + ps.DAY_MS + 1_000)]}
        for day, rows in days.items():
            self.zip_file(ps.RAW / 'binance' / 'aggTrades' / 'AUSDT' / f'AUSDT-aggTrades-{day}.zip', header,
                          [f'{a},{p},1,{f},{l},{t},false' for a, p, f, l, t in rows])
        # The day's trades, every id from 12 to 513: those of aggregates 3 and 4 and the 500 between them.
        trades = ([(12, '5.2', utc_ms + 1_000)] + [(12 + k, f'5.{20 + k % 9}', utc_ms + 1_000 + 79 * k) for k in range(1, 501)]
                  + [(513, '5.3', utc_ms + 40_500)])
        self.zip_file(ps.RAW / 'binance' / 'trades' / 'AUSDT' / 'AUSDT-trades-2026-10-27.zip', 'id,price,qty,quote_qty,time,is_buyer_maker',
                      [f'{i},{p},1,{p},{t},false' for i, p, t in trades])
        kinds = {Path(p.path).name: p.kind for p in ps.pieces_of('binance', 'AUSDT')}
        self.assertEqual(kinds, {'AUSDT-aggTrades-2026-10-26.zip': 'binance archive', 'AUSDT-trades-2026-10-27.zip': 'binance trades archive',
                                 'AUSDT-aggTrades-2026-10-28.zip': 'binance archive'})
        rows_dir = Path(self.folder.name) / 'rows'
        summary = ps.contract_rows(('binance', 'AUSDT', utc_ms - ps.DAY_MS, '20261001', '20261101', str(rows_dir)))
        self.assertEqual((summary['holes'], summary['id_breaks'], summary['gaps'], summary['overlap_missing']), ([], [], [], 0))
        rows = [r for day in ('20261027', '20261028', '20261029') for r in ps.read_csv(rows_dir / 'binance' / 'AUSDT' / f'{day}.archive.csv.gz')]
        self.assertEqual(int(rows[0][2]), utc_ms - 49_000)  # the first whole second after aggregate 1
        self.assertEqual(int(rows[-1][2]), utc_ms + 2 * ps.DAY_MS)  # the end of the coverage
        known = [(t, i, p) for i, p, t in trades] + [(utc_ms - 50_000, 1, '5.0'), (utc_ms - 20_000, 2, '5.1'), (utc_ms + ps.DAY_MS + 1_000, 5, '5.4')]
        for row in rows[:120]:  # through the hole: the newest trade before T, of equal times the larger id
            t, _, p = max(k for k in known if k[0] < int(row[2]))
            self.assertEqual((float(row[3]), row[4]), (float(p), str(t)), row)  # a row file holds the shortest decimal: 5 for 5.0

    def test_binance_trades_day_download(self):
        day = dt.date(2026, 10, 27)
        good = self.zip_file(Path(self.folder.name) / 'src' / 'AUSDT-trades-2026-10-27.zip', 'id,price,qty,quote_qty,time,is_buyer_maker',
                             ['1,5.0,1,5.0,1793059200100,false'])

        class Archive:
            def __init__(self, checksum):
                self.checksum, self.requests = checksum, []

            def request(self, url, body=None):
                self.requests.append(url)
                if '/AUSDT/' not in url:
                    return 404, {}, b'<Error><Code>NoSuchKey</Code></Error>'
                if url.endswith('.CHECKSUM'):
                    return 200, {}, f'{self.checksum}  AUSDT-trades-2026-10-27.zip\n'.encode()
                return 200, {'content-length': str(len(good))}, good

        self.assertFalse(ps.binance_trades_day(Archive('0' * 64), 'AUSDT', day))  # a sha256 not the published one
        self.assertTrue((ps.RAW / 'binance' / 'trades' / 'AUSDT' / 'AUSDT-trades-2026-10-27.zip.mismatch').exists())
        self.assertEqual([p.kind for p in ps.pieces_of('binance', 'AUSDT')], [])
        archive = Archive(ps.sha256(good))
        self.assertTrue(ps.binance_trades_day(archive, 'AUSDT', day))
        self.assertEqual(archive.requests, [ps.BINANCE_SITE + 'data/futures/um/daily/trades/AUSDT/AUSDT-trades-2026-10-27.zip',
                                            ps.BINANCE_SITE + 'data/futures/um/daily/trades/AUSDT/AUSDT-trades-2026-10-27.zip.CHECKSUM'])
        self.assertTrue(ps.binance_trades_day(archive, 'AUSDT', day))  # already in: not asked again
        self.assertEqual(len(archive.requests), 2)
        self.assertFalse(ps.binance_trades_day(archive, 'BUSDT', day))  # not published
        self.assertEqual([p.kind for p in ps.pieces_of('binance', 'AUSDT')], ['binance trades archive'])
        with open(ps.MANIFEST) as handle:
            rows = list(csv.DictReader(handle))
        self.assertEqual([(r['symbol'], r['kind'], r['checksum_ok'], r['error'][:8]) for r in rows],
                         [('AUSDT', 'trades', 'False', 'publishe'), ('AUSDT', 'trades', 'True', ''), ('BUSDT', 'trades', '', 'HTTP 404')])

    def test_no_rest_skips_the_rest_runs(self):
        calls = []
        saved = ps.bitget_rest, ps.binance_rest
        ps.bitget_rest = lambda client, symbol, start_ms: calls.append(('bitget', symbol))
        ps.binance_rest = lambda client, symbol, start_ms: calls.append(('binance', symbol))
        try:
            now = ps.now_ms()  # history from now: no archive day is due, nothing is listed
            contracts = [('bitget', 'AUSDT'), ('binance', 'AUSDT')]
            ps.download(contracts, now, rest=False, client=FakeVenue([]))
            self.assertEqual(calls, [])
            ps.download(contracts, now, client=FakeVenue([]))
            self.assertEqual(sorted(calls), [('binance', 'AUSDT'), ('bitget', 'AUSDT')])
        finally:
            ps.bitget_rest, ps.binance_rest = saved


class PerpSecondStretchesTest(unittest.TestCase):
    def test_stretches_of_changed_seconds(self):
        self.assertEqual(ps.stretches([]), [])
        self.assertEqual(ps.stretches([5000]), [(4999, 5000)])
        # 60 s apart stays one stretch, 61 s apart starts another
        self.assertEqual(ps.stretches([5000, 6000, 66_000, 127_000, 128_000]), [(4999, 66_000), (126_999, 128_000)])

    def test_ranges_of_several_files_joined(self):
        self.assertEqual(ps.joined([]), [])
        # overlapping or one inside another, in any order: one range
        self.assertEqual(ps.joined([(65_999, 70_000), (4999, 66_000), (9999, 10_000)]), [(4999, 70_000)])
        # a range whose first second is 60 s after the end of the one before joins it, 61 s after does not (as stretches())
        self.assertEqual(ps.joined([(4999, 5000), (65_000 - 1, 65_000)]), [(4999, 65_000)])
        self.assertEqual(ps.joined([(4999, 5000), (66_000 - 1, 66_000)]), [(4999, 5000), (65_999, 66_000)])


@unittest.skipUnless(os.environ.get('PERP_SECOND_TEST_DSN'), 'PERP_SECOND_TEST_DSN names no throwaway database')
class PerpSecondLoadTest(unittest.TestCase):
    """load: COPY, then fill only what is missing; archive files before interim before live; idempotent; the
    row files written before the book arrays load when their book fields are empty."""

    SCHEMA = """
        CREATE SCHEMA IF NOT EXISTS etfpair;
        CREATE TABLE IF NOT EXISTS etfpair.perp_second (
          venue text NOT NULL, symbol text NOT NULL, second_ms bigint NOT NULL,
          price numeric, trade_ms bigint,
          bid_px numeric[], bid_sz numeric[], ask_px numeric[], ask_sz numeric[], book_ms bigint,
          PRIMARY KEY (venue, symbol, second_ms));"""

    @staticmethod
    def scalar_file(path, lines):
        """A row file as written before the book arrays (header bid,bid_size,ask,ask_size)."""
        Path(path).parent.mkdir(parents=True, exist_ok=True)
        with gzip.open(path, 'wt') as handle:
            handle.write('\n'.join([','.join(ps.SCALAR_COLUMNS)] + lines) + '\n')

    def run_sql(self, sql, *args, fetch=False):
        import asyncpg

        async def go():
            connection = await asyncpg.connect(os.environ['PERP_SECOND_TEST_DSN'])
            try:
                return await (connection.fetch(sql, *args) if fetch else connection.execute(sql, *args))
            finally:
                await connection.close()
        return asyncio.run(go())

    def test_fill_without_overwriting(self):
        self.run_sql(self.SCHEMA)
        self.run_sql("DELETE FROM etfpair.perp_second WHERE venue = 'test'")
        with tempfile.TemporaryDirectory() as folder:
            t = DAY_MS + 5000
            # An archive file of the earlier layout: prices only.
            self.scalar_file(ps.row_path(folder, 'test', 'AUSDT', DAY, 'archive'), [f'test,AUSDT,{t},10.5,{t - 300},,,,,'])
            ps.write_rows(folder, 'live', [('test', 'AUSDT', t, '99', t - 1, ('10.4',), ('2',), ('10.6',), ('3',), t - 20),
                                           ('test', 'AUSDT', t + 1000, '10.7', t + 400, ('10.6', '10.5'), ('1', '4'), (), (), t + 900)])
            ps.write_rows(folder, 'interim', [('test', 'AUSDT', t, '98', t - 2, ('10.3', '10.2'), ('5', '1'), ('10.7',), ('6',), t - 40)])
            first = asyncio.run(ps.load(os.environ['PERP_SECOND_TEST_DSN'], folder))
            self.assertEqual((first['inserted'], first['filled']), (2, 1))  # archive inserts, interim fills the book, live adds a row
            rows = self.run_sql("SELECT second_ms, price::text, trade_ms, bid_px::text, bid_sz::text, ask_px::text, ask_sz::text, book_ms "
                                "FROM etfpair.perp_second WHERE venue = 'test' ORDER BY second_ms", fetch=True)
            self.assertEqual([tuple(r) for r in rows], [(t, '10.5', t - 300, '{10.3,10.2}', '{5,1}', '{10.7}', '{6}', t - 40),
                                                        (t + 1000, '10.7', t + 400, '{10.6,10.5}', '{1,4}', '{}', '{}', t + 900)])
            again = asyncio.run(ps.load(os.environ['PERP_SECOND_TEST_DSN'], folder))
            self.assertEqual((again['inserted'], again['filled']), (0, 0))
        with tempfile.TemporaryDirectory() as folder:  # an earlier-layout file with book values is refused, nothing merged
            path = ps.row_path(folder, 'test', 'AUSDT', DAY, 'interim')
            self.scalar_file(path, [f'test,AUSDT,{t + 2000},10.9,{t + 1500},10.8,1,11,2,{t + 1900}'])
            with self.assertRaisesRegex(ValueError, 'malformed array literal'):
                asyncio.run(ps.load(os.environ['PERP_SECOND_TEST_DSN'], folder))
            self.assertEqual(self.run_sql("SELECT count(*) FROM etfpair.perp_second WHERE venue = 'test'", fetch=True)[0][0], 2)
        self.run_sql("DELETE FROM etfpair.perp_second WHERE venue = 'test'")

    # Stand-ins for etfpair.refresh_perp_second_minute (dashboard/internal/scan/schema.sql), in a schema of their own so
    # that a database holding the real function keeps it: one records each call with the rows it sees, one fails.
    STUB = """
        CREATE SCHEMA IF NOT EXISTS perp_second_test;
        CREATE TABLE IF NOT EXISTS perp_second_test.refresh_calls (from_ms bigint, to_ms bigint, rows_seen bigint);
        TRUNCATE perp_second_test.refresh_calls;
        CREATE OR REPLACE FUNCTION perp_second_test.refresh(p_from_ms bigint, p_to_ms bigint) RETURNS integer LANGUAGE sql AS $$
            INSERT INTO perp_second_test.refresh_calls
            SELECT p_from_ms, p_to_ms, count(*) FROM etfpair.perp_second WHERE venue = 'test' AND second_ms > p_from_ms AND second_ms <= p_to_ms
            RETURNING 2 $$;
        CREATE OR REPLACE FUNCTION perp_second_test.failing(p_from_ms bigint, p_to_ms bigint) RETURNS integer LANGUAGE plpgsql AS $$
            BEGIN RAISE EXCEPTION 'refresh failed on purpose'; END $$;"""

    def load_with(self, refresh, folder):
        saved, ps.REFRESH = ps.REFRESH, refresh
        out = io.StringIO()
        try:
            with contextlib.redirect_stdout(out):
                totals = asyncio.run(ps.load(os.environ['PERP_SECOND_TEST_DSN'], folder))
        finally:
            ps.REFRESH = saved
        return totals, out.getvalue()

    def calls(self):
        return [tuple(r) for r in self.run_sql('SELECT from_ms, to_ms, rows_seen FROM perp_second_test.refresh_calls ORDER BY from_ms', fetch=True)]

    def test_refresh_of_the_changed_seconds(self):
        self.run_sql(self.SCHEMA)
        self.run_sql(self.STUB)
        self.run_sql("DELETE FROM etfpair.perp_second WHERE venue = 'test'")
        t = DAY_MS + 5000
        trade = lambda s: ('test', 'AUSDT', s, '10', s - 1, None, None, None, None, None)
        with tempfile.TemporaryDirectory() as folder:
            # 60 s after the previous second stays one stretch, 61 s after starts another
            ps.write_rows(folder, 'archive', [trade(t), trade(t + 1000), trade(t + 61_000), trade(t + 122_000)])
            totals, _ = self.load_with('perp_second_test.refresh', folder)
            self.assertEqual((totals['inserted'], totals['refreshed'], totals['refresh_result']), (4, 2, 4))
            # called after the merge in the file's transaction: it sees the new rows
            self.assertEqual(self.calls(), [(t - 1, t + 61_000, 3), (t + 121_999, t + 122_000, 1)])
            totals, _ = self.load_with('perp_second_test.refresh', folder)  # nothing changes, nothing is refreshed
            self.assertEqual((totals['inserted'], totals['filled'], totals.get('refreshed', 0)), (0, 0, 0))
            ps.write_rows(folder, 'live', [('test', 'AUSDT', t + 1000, '99', t, ('9.9',), ('1',), ('10.1',), ('1',), t + 900)])
            totals, _ = self.load_with('perp_second_test.refresh', folder)  # a filled book is a change too
            self.assertEqual((totals['filled'], totals['refreshed']), (1, 1))
            self.assertEqual(self.calls()[1], (t + 999, t + 1000, 1))
        self.run_sql("DELETE FROM etfpair.perp_second WHERE venue = 'test'")

    def test_refresh_missing_or_failing(self):
        import asyncpg

        self.run_sql(self.SCHEMA)
        self.run_sql(self.STUB)
        self.run_sql("DELETE FROM etfpair.perp_second WHERE venue = 'test'")
        t = DAY_MS + 5000
        count = "SELECT count(*) FROM etfpair.perp_second WHERE venue = 'test'"
        with tempfile.TemporaryDirectory() as folder:
            ps.write_rows(folder, 'archive', [('test', 'AUSDT', t, '10', t - 1, None, None, None, None, None)])
            totals, output = self.load_with('perp_second_test.absent', folder)  # not created yet: merged, said, range named
            self.assertEqual((totals['inserted'], totals['unrefreshed']), (1, 1))
            self.assertIn('perp_second_test.absent(bigint, bigint) does not exist yet', output)
            self.assertIn(f'SELECT perp_second_test.absent({t - 1}, {t});', output)
            self.assertEqual(self.run_sql(count, fetch=True)[0][0], 1)
        with tempfile.TemporaryDirectory() as folder:  # the stretches of two files: joined where they meet, one SELECT a range
            ps.write_rows(folder, 'archive', [('test', 'AUSDT', t + 2000, '10', t, None, None, None, None, None),
                                              ('test', 'BUSDT', t + 3000, '10', t, None, None, None, None, None),
                                              ('test', 'BUSDT', t + 900_000, '10', t, None, None, None, None, None)])
            totals, output = self.load_with('perp_second_test.absent', folder)
            self.assertEqual((totals['inserted'], totals['unrefreshed']), (3, 2))
            selects = [line.split('UTC+8 perp_second: ', 1)[1] for line in output.splitlines() if 'SELECT' in line]
            self.assertEqual([s.split('  --')[0] for s in selects], [f'  SELECT perp_second_test.absent({t + 1999}, {t + 3000});',
                                                                    f'  SELECT perp_second_test.absent({t + 899_999}, {t + 900_000});'])
            self.assertEqual(self.calls(), [])  # nothing was called
            self.assertEqual(self.run_sql(count, fetch=True)[0][0], 4)
        with tempfile.TemporaryDirectory() as folder:  # a failing refresh rolls back its file's rows
            ps.write_rows(folder, 'archive', [('test', 'AUSDT', t + 1000, '10', t, None, None, None, None, None)])
            with self.assertRaisesRegex(asyncpg.RaiseError, 'refresh failed on purpose'):
                self.load_with('perp_second_test.failing', folder)
            self.assertEqual(self.run_sql(count, fetch=True)[0][0], 4)  # the four rows of the blocks above, not this one
        self.run_sql("DELETE FROM etfpair.perp_second WHERE venue = 'test'")

    # The summary's counts of venue 'test' from its rows, as etfpair.refresh_perp_second_minute defines them, against the
    # rows of etfpair.perp_second_minute: (minute, symbol, seconds, books, least, most, final) of each, and whether equal.
    SUMMARY_VS_ROWS = """
        WITH d AS (SELECT symbol, (second_ms - 1) / 60000 * 60000 AS minute_ms, count(price)::int AS seconds,
                          count(*) FILTER (WHERE bid_px[1] IS NOT NULL AND ask_px[1] IS NOT NULL)::int AS books,
                          min(least(cardinality(bid_px), cardinality(ask_px))) FILTER (WHERE bid_px[1] IS NOT NULL AND ask_px[1] IS NOT NULL) AS lo,
                          max(greatest(cardinality(bid_px), cardinality(ask_px))) FILTER (WHERE bid_px[1] IS NOT NULL AND ask_px[1] IS NOT NULL) AS hi
                   FROM etfpair.perp_second WHERE venue = 'test' GROUP BY 1, 2)
        SELECT t.minute_ms, t.symbol, t.seconds, t.books, t.least_levels, t.most_levels, etfpair.perp_minute_final(t.minute_ms, t.refreshed_ms),
               (d.seconds, d.books, d.lo, d.hi) IS NOT DISTINCT FROM (t.seconds, t.books, t.least_levels, t.most_levels)
        FROM etfpair.perp_second_minute t LEFT JOIN d USING (symbol, minute_ms) WHERE t.venue = 'test' ORDER BY 1, 2"""
    COVERAGE_VS_ROWS = """
        SELECT (SELECT row(c.seconds, c.books, c.least_levels, c.most_levels)::text FROM etfpair.perp_coverage('test', $1, $2, $3) c),
               (SELECT row(count(price), count(*) FILTER (WHERE bid_px[1] IS NOT NULL AND ask_px[1] IS NOT NULL),
                           min(least(cardinality(bid_px), cardinality(ask_px))) FILTER (WHERE bid_px[1] IS NOT NULL AND ask_px[1] IS NOT NULL),
                           max(greatest(cardinality(bid_px), cardinality(ask_px))) FILTER (WHERE bid_px[1] IS NOT NULL AND ask_px[1] IS NOT NULL))::text
                FROM etfpair.perp_second WHERE venue = 'test' AND symbol = $1 AND second_ms > $2 AND second_ms <= $3)"""

    def test_refresh_with_the_real_summary(self):
        """load against etfpair.refresh_perp_second_minute and etfpair.perp_coverage as dashboard/internal/scan/schema.sql
        defines them (it and the recorder's schema applied to the test database, as the collector does): every minute
        the files changed is aggregated again for every contract of the combos, also the minute two day files share
        (T = 00:00:00 belongs to the minute before), on an empty summary and on one whose rows are final already; the
        summary then holds the counts of the rows and perp_coverage gives the counts of perp_second over any range."""
        repo = Path(ps.__file__).resolve().parents[2]
        for name in ('booktape', 'scan'):
            self.run_sql((repo / 'dashboard' / 'internal' / name / 'schema.sql').read_text())
        clean = """DELETE FROM etfpair.perp_second WHERE venue = 'test'; DELETE FROM etfpair.perp_second_minute WHERE venue = 'test';
                   DELETE FROM etfpair.combo_market WHERE combo_id = 'TEST'; DELETE FROM etfpair.combo WHERE combo_id = 'TEST'"""
        self.run_sql(clean)
        midnight = DAY_MS + ps.DAY_MS  # 2026-11-19 00:00:00 UTC+8: a second of minute 23:59 of DAY, in the next day's file
        last = midnight - 60_000       # the minute 23:59 of DAY
        try:
            self.run_sql("""INSERT INTO etfpair.combo VALUES ('TEST', 'test', 'A', 'B', '');
                            INSERT INTO etfpair.combo_market VALUES ('TEST', 'test', 'AUSDT', 'BUSDT', '')""")
            # BUSDT's rows are in the table already: the refresh of AUSDT's stretches aggregates them too.
            self.run_sql("""INSERT INTO etfpair.perp_second VALUES
                                ('test', 'BUSDT', $1::bigint, 20, $1::bigint - 5, '{19.9,19.8}', '{1,2}', '{20.1}', '{3}', $1::bigint - 9),
                                ('test', 'BUSDT', $2::bigint, 20, $2::bigint - 5, NULL, NULL, NULL, NULL, NULL)""", midnight - 30_000, midnight)
            trade = lambda s: ('test', 'AUSDT', s, '10', s - 1, None, None, None, None, None)
            with tempfile.TemporaryDirectory() as folder:
                ps.write_rows(folder, 'archive', [trade(midnight - 50_000), trade(midnight - 40_000), trade(midnight), trade(midnight + 10_000)])
                self.assertEqual(sorted(p.name for p in Path(folder, 'test', 'AUSDT').iterdir()), [f'{DAY}.archive.csv.gz', '20261119.archive.csv.gz'])
                totals, output = self.load_with(ps.REFRESH, folder)
                # file DAY: minute 23:59 of AUSDT and BUSDT written; file 20261119: AUSDT's 23:59 (2 -> 3 seconds) and 00:00
                self.assertEqual((totals['inserted'], totals['refreshed'], totals['refresh_result']), (4, 2, 4))
                self.assertIn(f'{ps.REFRESH} called for 2 stretches of changed seconds, returning 4 in all', output)
                summary = [tuple(r) for r in self.run_sql(self.SUMMARY_VS_ROWS, fetch=True)]
                self.assertEqual(summary, [(last, 'AUSDT', 3, 0, None, None, True, True), (last, 'BUSDT', 2, 1, 1, 2, True, True),
                                           (midnight, 'AUSDT', 1, 0, None, None, True, True)])
                # a book filled into an existing row changes a final row of the summary: rewritten, BUSDT's left as it is
                ps.write_rows(folder, 'live', [('test', 'AUSDT', midnight, '99', midnight - 2, ('9.9', '9.8', '9.7'), ('1', '1', '1'),
                                                ('10.1',), ('4',), midnight - 3)])
                totals, _ = self.load_with(ps.REFRESH, folder)
                self.assertEqual((totals['inserted'], totals['filled'], totals['refreshed'], totals['refresh_result']), (0, 1, 1, 1))
                summary = [tuple(r) for r in self.run_sql(self.SUMMARY_VS_ROWS, fetch=True)]
                self.assertEqual(summary[0], (last, 'AUSDT', 3, 1, 1, 3, True, True))
                self.assertEqual(summary[1:], [(last, 'BUSDT', 2, 1, 1, 2, True, True), (midnight, 'AUSDT', 1, 0, None, None, True, True)])
                again, _ = self.load_with(ps.REFRESH, folder)  # nothing changes, nothing is refreshed
                self.assertEqual((again['inserted'], again['filled'], again.get('refreshed', 0)), (0, 0, 0))
            for symbol in ('AUSDT', 'BUSDT'):  # whole minutes from the summary, partial ones and the rest counted in perp_second
                for lo, hi in [(last, midnight), (last - 1, midnight + 1), (last + 15_000, midnight + 10_000), (DAY_MS, midnight + ps.DAY_MS),
                               (last, midnight + 60_000), (midnight, midnight + 60_000), (midnight - 10_000, midnight - 10_000)]:
                    got, want = self.run_sql(self.COVERAGE_VS_ROWS, symbol, lo, hi, fetch=True)[0]
                    self.assertEqual(got, want, (symbol, lo, hi))
        finally:
            self.run_sql(clean)


# ---------------------------------------------------------------- perp_second_recordings.py

import contextlib  # noqa: E402

import perp_second_recordings as psr  # noqa: E402

URL_BITGET = 'wss://ws.bitget.com/v3/ws/public'
URL_BINANCE_PUBLIC = 'wss://fstream.binance.com/public/stream?streams=soxlusdt@depth5@100ms/soxsusdt@depth5@100ms'
URL_BINANCE_MARKET = 'wss://fstream.binance.com/market/stream?streams=soxlusdt@aggTrade/soxsusdt@aggTrade'
URL_INTERIM_BOOKS = 'wss://fstream.binance.com/public/stream?streams=soxlusdt@bookTicker/soxsusdt@bookTicker'


class PerpSecondRecordingsTest(unittest.TestCase):
    """perp_second_recordings.py on a recording built by hand across midnight UTC+8, times in ms from midnight:
    Bitget books5 and publicTrade of SOXLUSDT and books50 (cut to 10 levels, as stored until 2026-11-22) of SOXSUSDT on one connection;
    Binance depth5 of SOXLUSDT and SOXSUSDT on one feed, aggTrade of SOXLUSDT on another that reconnects. The
    expected rows are worked out by hand from the per-second rules."""

    M = ps.day_start_ms('20261120')
    STAMPS = [(-4950, -4960), (-1500, -1510), (1200, 1190), (5000, 4990)]  # Bitget book frames: received, data[0].ts
    TOPS = [('10.00', '1', '10.20', '2'), ('10.10', '3', '10.30', '4'), ('10.20', '5', '10.40', '6'), ('10.30', '7', '10.50', '8')]
    FILL5, FILL6, FILL7 = ('10.15', -3010), ('10.18', -20), ('10.25', 480)
    DEPTH = [(-4900, -4905, '1.10000', '5', '1.20000', '6'), (3990, 3985, '1.11000', '7', '1.21000', '8'),
             (6000, 5995, '1.12000', '9', '1.22000', '9')]  # Binance SOXLUSDT depth5: received, T, best levels
    AGG100, AGG101, AGG102 = ('1.15000', -4100), ('1.16000', 900), ('1.17000', 2550)

    def setUp(self):
        self.folder = tempfile.TemporaryDirectory()
        self.tree = Path(self.folder.name) / 'tape'
        self.rows = Path(self.folder.name) / 'rows'
        m = self.M
        args = [dict(instType='usdt-futures', topic=topic, symbol=symbol)
                for symbol, topic in (('SOXLUSDT', 'books5'), ('SOXLUSDT', 'publicTrade'), ('SOXSUSDT', 'books50'), ('SOXSUSDT', 'publicTrade'))]
        self.event(self.tree, 'bitget', m - 5000, 'bitget-soxl-soxs', 'connected', url=URL_BITGET, request=dict(op='subscribe', args=args))
        self.stream(self.tree, 'bitget', 'SOXLUSDT.books5', [(m + received, self.bitget_book('books5', 'SOXLUSDT', m + ts, *top))
                                                             for (received, ts), top in zip(self.STAMPS, self.TOPS)])
        self.stream(self.tree, 'bitget', 'SOXSUSDT.books50', [(m + received, self.bitget_book('books50', 'SOXSUSDT', m + ts, *top))
                                                              for (received, ts), top in zip(self.STAMPS, self.TOPS)])
        self.stream(self.tree, 'bitget', 'SOXLUSDT.publicTrade', [
            (m - 3000, self.bitget_fills((5, m - 3010, '10.15'))),
            (m + 500, self.bitget_fills((7, m + 480, '10.25'), (6, m - 20, '10.18')))])  # newest first, as Bitget sends
        self.event(self.tree, 'binance', m - 5000, 'binance-public', 'connected', url=URL_BINANCE_PUBLIC, request=None)
        self.binance_market(self.tree)
        self.stream(self.tree, 'binance', 'SOXLUSDT.depth5', [(m + r, self.depth5('SOXLUSDT', m + t, k, b, B, a, A))
                                                              for k, (r, t, b, B, a, A) in enumerate(self.DEPTH)])
        self.stream(self.tree, 'binance', 'SOXSUSDT.depth5', [
            (m + 100, self.depth5('SOXSUSDT', m + 95, 1, '2.00000', '1', '2.10000', '1')),
            (m + 2200, self.depth5('SOXSUSDT', m + 2190, 2, '2.01000', '1', '2.11000', '1'))])

    def tearDown(self):
        self.folder.cleanup()

    def binance_market(self, tree):
        m = self.M
        self.event(tree, 'binance', m - 5000, 'binance-market', 'connected', url=URL_BINANCE_MARKET, request=None)
        self.event(tree, 'binance', m + 1500, 'binance-market', 'disconnected', error='read: timeout', frames=2)
        self.event(tree, 'binance', m + 2500, 'binance-market', 'connected', url=URL_BINANCE_MARKET, request=None)
        self.stream(tree, 'binance', 'SOXLUSDT.aggTrade', [
            (m - 4000, self.agg('SOXLUSDT', m - 4100, 100, '1.15000')),
            (m + 1000, self.agg('SOXLUSDT', m + 900, 101, '1.16000')),
            (m + 2600, self.agg('SOXLUSDT', m + 2550, 102, '1.17000'))])

    @staticmethod
    def event(tree, venue, received, feed, kind, **fields):
        folder = tree / venue / ps.day_of_ms(received)
        folder.mkdir(parents=True, exist_ok=True)
        with open(folder / 'events.jsonl', 'a') as handle:
            handle.write(json.dumps(dict(received_ms=received, feed=feed, event=kind, **fields)) + '\n')

    @staticmethod
    def stream(tree, venue, stream, records):
        """Data files as the recorder writes them: one per UTC+8 hour, named by its first record."""
        hours = {}
        for received, frame in records:
            hours.setdefault(received // 3_600_000, []).append((received, frame))
        for items in hours.values():
            folder = tree / venue / ps.day_of_ms(items[0][0])
            folder.mkdir(parents=True, exist_ok=True)
            with gzip.open(folder / f'{stream}.{items[0][0]}.jsonl.gz', 'wb') as handle:
                for received, frame in items:
                    handle.write(b'{"received_ms":%d,"payload":%s}\n' % (received, json.dumps(frame, separators=(',', ':')).encode()))

    @staticmethod
    def bitget_book(topic, symbol, ts, bid, bid_size, ask, ask_size):
        """books5: 5 levels a side; books50 as stored, cut to 10."""
        deeper = 4 if topic == 'books5' else 9
        return dict(action='snapshot', arg=dict(instType='usdt-futures', topic=topic, symbol=symbol), ts=ts + 2,
                    data=[dict(a=[[ask, ask_size]] + [[f'11.0{k}', '50'] for k in range(1, deeper + 1)],
                               b=[[bid, bid_size]] + [[f'9.0{9 - k}', '50'] for k in range(1, deeper + 1)],
                               seq=ts, pseq=0, ts=str(ts))])

    @staticmethod
    def bitget_fills(*fills):
        """publicTrade frame of fills (id, T, price)."""
        return dict(action='update', arg=dict(instType='usdt-futures', topic='publicTrade', symbol='SOXLUSDT'), ts=fills[0][1] + 2,
                    data=[dict(i=str(i), p=p, v='0.5', S='buy', T=str(t), L=str(i + 1), isRPI='no') for i, t, p in fills])

    @staticmethod
    def depth5(symbol, t, u, bid, bid_size, ask, ask_size):
        """depth5 frame whose bids hold a second level 0.5 x 1."""
        return dict(stream=symbol.lower() + '@depth5@100ms', data=dict(e='depthUpdate', E=t + 3, T=t, s=symbol, ps=symbol, U=u, u=u, pu=u - 1,
                                                                       b=[[bid, bid_size], ['0.50000', '1']], a=[[ask, ask_size]], st=1))

    @staticmethod
    def ticker(symbol, t, bid, bid_size, ask, ask_size):
        """bookTicker frame of the interim capture."""
        return dict(stream=symbol.lower() + '@bookTicker', data=dict(e='bookTicker', u=t, s=symbol, ps=symbol, b=bid, B=bid_size,
                                                                      a=ask, A=ask_size, T=t, E=t + 1, st=1))

    @staticmethod
    def agg(symbol, t, a, p):
        return dict(stream=symbol.lower() + '@aggTrade', data=dict(e='aggTrade', E=t + 150, a=a, s=symbol, p=p, q='1.00', nq='1.00',
                                                                    f=10 * a, l=10 * a, T=t, m=False, st=1))

    def run_main(self, source, tree=None, first='2026-11-18', end='2026-11-21'):
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            code = psr.main(['--root', str(tree or self.tree), '--from', first, '--to', end, '--rows', str(self.rows), '--source', source])
        return code, out.getvalue()

    def read(self, venue, symbol, day, source):
        return ps.read_csv(ps.row_path(self.rows, venue, symbol, day, source))

    def row(self, venue, symbol, second, trade, book):
        """A row as read back from a row file: the second and times given in ms from midnight, None for NULL."""
        price, trade_ms = trade or (None, None)
        bid_px, bid_sz, ask_px, ask_sz, book_ms = book or (None,) * 5
        return (venue, symbol, str(self.M + second), price, None if trade_ms is None else str(self.M + trade_ms),
                bid_px, bid_sz, ask_px, ask_sz, None if book_ms is None else str(self.M + book_ms))

    def top(self, k, levels=5):
        """The book columns of Bitget book frame k: its best level and the deeper ones of bitget_book."""
        bid, bid_size, ask, ask_size = self.TOPS[k]
        deeper = levels - 1
        return ((bid,) + tuple(f'9.0{9 - j}' for j in range(1, deeper + 1)), (bid_size,) + ('50',) * deeper,
                (ask,) + tuple(f'11.0{j}' for j in range(1, deeper + 1)), (ask_size,) + ('50',) * deeper, self.STAMPS[k][1])

    def depth(self, k, levels=2):
        _, t, bid, bid_size, ask, ask_size = self.DEPTH[k]
        return (bid, '0.50000')[:levels], (bid_size, '1')[:levels], (ask,), (ask_size,), t

    def test_rows_of_each_day_and_venue(self):
        code, output = self.run_main('live')
        self.assertEqual(code, 0)
        self.assertEqual(sorted(str(p.relative_to(self.rows)) for p in self.rows.rglob('*.csv.gz')), [
            'binance/SOXLUSDT/20261119.live.csv.gz', 'binance/SOXLUSDT/20261120.live.csv.gz',
            'binance/SOXSUSDT/20261120.live.csv.gz',  # no rows before midnight, no file; none for 2026-11-18 either
            'bitget/SOXLUSDT/20261119.live.csv.gz', 'bitget/SOXLUSDT/20261120.live.csv.gz',
            'bitget/SOXSUSDT/20261119.live.csv.gz', 'bitget/SOXSUSDT/20261120.live.csv.gz'])
        bitget = lambda second, trade, book: self.row('bitget', 'SOXLUSDT', second, trade, book)
        self.assertEqual(self.read('bitget', 'SOXLUSDT', '20261119', 'live'), [
            bitget(-4000, None, self.top(0)),  # books5 known from -4950, trades from -3000
            bitget(-3000, self.FILL5, self.top(0)),
            bitget(-2000, self.FILL5, self.top(0)),
            bitget(-1000, self.FILL5, self.top(1))])
        self.assertEqual(self.read('bitget', 'SOXLUSDT', '20261120', 'live'), [
            bitget(0, self.FILL6, self.top(1)),  # fill 6 (T -20) arrived at +500 in one frame with fill 7 (T 480)
            bitget(1000, self.FILL7, self.top(1)),  # the book frame stamped 1190 is not before 1000
            bitget(2000, self.FILL7, self.top(2)),
            bitget(3000, None, self.top(2)),  # trades not final at the end: nothing stamped >= 3000, clock not past 5000
            bitget(4000, None, self.top(2))])
        soxs = self.read('bitget', 'SOXSUSDT', '20261120', 'live')
        self.assertEqual(soxs[0], self.row('bitget', 'SOXSUSDT', 0, None, self.top(1, levels=10)))  # 10 levels, no trades subscribed
        self.assertEqual([len(r[5]) for r in soxs], [10] * 5)
        binance = lambda second, trade, book: self.row('binance', 'SOXLUSDT', second, trade, book)
        self.assertEqual(self.read('binance', 'SOXLUSDT', '20261119', 'live'),
                         [binance(s, self.AGG100, self.depth(0)) for s in (-4000, -3000, -2000, -1000)])
        self.assertEqual(self.read('binance', 'SOXLUSDT', '20261120', 'live'), [
            binance(0, self.AGG100, self.depth(0)),
            binance(1000, self.AGG101, self.depth(0)),
            binance(2000, None, self.depth(0)),  # the trade feed had no connection from 1500 to 2500
            binance(3000, self.AGG102, self.depth(0)),  # known again from 2600
            binance(4000, None, self.depth(1)),
            binance(5000, None, self.depth(1))])
        self.assertEqual(self.read('binance', 'SOXSUSDT', '20261120', 'live'),
                         [self.row('binance', 'SOXSUSDT', s, None, (('2.00000', '0.50000'), ('1', '1'), ('2.10000',), ('1',), 95))
                          for s in (1000, 2000)])
        gaps = [line for line in output.splitlines() if 'feed binance-market:' in line]
        self.assertEqual(len(gaps), 1)
        self.assertIn('2026-11-20 00:00:01 to 2026-11-20 00:00:02', gaps[0])

    def test_interim_capture_reads_binance_bookticker(self):
        interim = Path(self.folder.name) / 'interim'
        m = self.M
        for path in (self.tree / 'bitget').rglob('*'):  # Bitget as recorded: books5 and publicTrade
            target = interim / path.relative_to(self.tree)
            if path.is_dir():
                target.mkdir(parents=True, exist_ok=True)
            else:
                target.parent.mkdir(parents=True, exist_ok=True)
                target.write_bytes(path.read_bytes())
        self.event(interim, 'binance', m - 5000, 'binance-books', 'connected', url=URL_INTERIM_BOOKS, request=None)
        self.binance_market(interim)
        self.stream(interim, 'binance', 'SOXLUSDT.bookTicker', [(m + r, self.ticker('SOXLUSDT', m + t, b, B, a, A)) for r, t, b, B, a, A in self.DEPTH])
        self.assertEqual(self.run_main('live')[0], 0)
        self.assertEqual(self.run_main('interim', tree=interim)[0], 0)
        for symbol, day in (('SOXLUSDT', '20261119'), ('SOXLUSDT', '20261120'), ('SOXSUSDT', '20261120')):
            self.assertEqual(self.read('bitget', symbol, day, 'interim'), self.read('bitget', symbol, day, 'live'))
        for day in ('20261119', '20261120'):  # the same rows with one level a side
            one = [row[:5] + (row[5][:1], row[6][:1]) + row[7:] for row in self.read('binance', 'SOXLUSDT', day, 'live')]
            self.assertEqual(self.read('binance', 'SOXLUSDT', day, 'interim'), one)

    def test_day_files_equal_one_replay_across_midnight(self):
        self.run_main('live')
        for venue, symbol in psr.contracts(self.tree):
            got = [row for path in sorted((self.rows / venue / symbol).glob('*.live.csv.gz')) for row in ps.read_csv(path)]
            whole = psr.booktape.seconds(venue, symbol, self.M - ps.DAY_MS, self.M + ps.DAY_MS, root=self.tree)
            self.assertEqual(got, [tuple(v if v is None or isinstance(v, tuple) else str(v) for v in row) for row in whole], (venue, symbol))
            self.assertTrue(got)

    def test_contracts_and_arguments(self):
        self.stream(self.tree, 'bitget', 'SOXXUSDT.books50', [(self.M + 10, self.bitget_book('books50', 'SOXXUSDT', self.M + 5, '1', '1', '2', '1'))])
        self.stream(self.tree, 'bitget', 'QQQUSDT.ticker', [(self.M + 10, dict(action='snapshot', arg=dict(topic='ticker', symbol='QQQUSDT'), data=[{}], ts=1))])
        self.assertEqual(psr.contracts(self.tree), [('binance', 'SOXLUSDT'), ('binance', 'SOXSUSDT'), ('bitget', 'SOXLUSDT'),
                                                    ('bitget', 'SOXSUSDT'), ('bitget', 'SOXXUSDT')])
        self.assertEqual(psr.booktape.SECOND_COLUMNS, ps.COLUMNS)  # seconds() rows are row-file rows
        common = ['--rows', str(self.rows), '--from', '2026-11-19']
        with contextlib.redirect_stderr(io.StringIO()):
            with self.assertRaises(SystemExit):
                psr.main(common + ['--root', str(self.tree), '--to', '2026-11-20', '--source', 'archive'])
            with self.assertRaises(SystemExit):
                psr.main(common + ['--root', str(self.tree), '--to', '2026-11-19', '--source', 'live'])
        with self.assertRaises(SystemExit):
            psr.main(common + ['--root', str(Path(self.folder.name) / 'missing'), '--to', '2026-11-20', '--source', 'live'])
        self.assertFalse(self.rows.exists())


if __name__ == '__main__':
    unittest.main()
