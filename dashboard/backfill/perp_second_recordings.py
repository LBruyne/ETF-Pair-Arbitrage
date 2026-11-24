#!/usr/bin/env python3
"""Rows of etfpair.perp_second from a booktape recording.

    perp_second_recordings.py --root TREE --from DAY --to DAY --rows DIR --source interim|live

DAY is a UTC+8 calendar day (YYYY-MM-DD); --from is included, --to is not. TREE is a recording in the
layout of research/booktape.py: the output of dashboard/cmd/booktape (experiments/book_tape, source live)
or the converted interim capture (experiments/book_tape_interim_20261119/tape, source interim).

Every contract of the recording (a venue and symbol with a data file of a book topic or of its trade
topic anywhere in TREE) gets, for every day, the rows of that day's whole seconds from research/booktape.py
seconds(): the rows the recorder derives from the same frames, on the connections of TREE's events, equal
to those of one call over all the days. They are written as the row file of perp_second.py,
<rows>/<venue>/<SYMBOL>/<YYYYMMDD>.<source>.csv.gz (a file of the same name is replaced; a day without rows
writes none), which `perp_second.py load` merges into the table without overwriting; load also re-aggregates the
per-minute summary etfpair.perp_second_minute for the seconds it changed (this script writes no database).

Topics: trade columns from Bitget publicTrade and Binance aggTrade; book columns from the book topic the
contract's connections subscribe: from the recorder (source live) Bitget books5, or books50 (cut to 10 levels a side until 2026-11-22, all 50 since)
levels for a price tick finer than 0.01, and Binance depth5 (depth10); from the converted interim capture
(source interim) Bitget books5 and Binance bookTicker, one level.

The book and the trade part of a row are each final once the recording holds an event of that kind stamped
at or after its second, anything received more than 2 s after it, or the end of the connection: on a
recording still being written or cut short, the newest seconds, or one part of them, come with the next run.

Run from the repository root: .venv/bin/python -I dashboard/backfill/perp_second_recordings.py ...
"""

import argparse
import datetime as dt
import os
import sys
from concurrent.futures import ProcessPoolExecutor
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path[:0] = [str(HERE), str(HERE.parents[1])]  # python -I puts neither this directory nor the repository on sys.path
import perp_second as ps  # noqa: E402
from research import booktape  # noqa: E402

SOURCES = ('interim', 'live')
VENUES = ('bitget', 'binance')
WORKERS = 8
HOUR_MS = 3_600_000


def log(message):
    print(f'{dt.datetime.now(ps.TZ8):%Y-%m-%d %H:%M:%S} UTC+8 perp_second_recordings: {message}', flush=True)


def contracts(root):
    """(venue, symbol) of every contract with a data file of a book topic or of the trade topic in the recording."""
    found = set()
    for venue in VENUES:
        topics = set(booktape.BOOK_TOPICS[venue]) | {booktape.TRADE_TOPIC[venue]}
        for folder in (Path(root) / venue).glob('[0-9]' * 8):
            for name in os.listdir(folder):
                parsed = booktape.parse_name(name)
                if parsed:
                    symbol, _, topic = parsed[0].partition('.')
                    if topic in topics:
                        found.add((venue, symbol))
    return sorted(found)


def contract_day(job):
    """Writes the row file of one contract and UTC+8 day; returns its summary: rows, the first and last
    second, rows without trade columns and without book columns, rows per hour of the day."""
    root, venue, symbol, day, source, rows_dir = job
    start = ps.day_start_ms(day)
    rows = booktape.seconds(venue, symbol, start, start + ps.DAY_MS, root=root)
    if rows:
        ps.write_csv(ps.row_path(rows_dir, venue, symbol, day, source), rows)
    hours = [0] * 24
    for row in rows:
        hours[(row[2] - start) // HOUR_MS] += 1
    return dict(venue=venue, symbol=symbol, day=day, rows=len(rows),
                first_ms=rows[0][2] if rows else None, last_ms=rows[-1][2] if rows else None,
                no_trade=sum(row[4] is None for row in rows), no_book=sum(row[9] is None for row in rows), hours=hours)


def describe(summary):
    head = f"{summary['venue']} {summary['symbol']} {summary['day']}"
    n = summary['rows']
    if not n:
        return f'{head}: no rows'
    hours = summary['hours']
    short = ', '.join(f'{h:02d}:00 {k}' for h, k in enumerate(hours) if 0 < k < 3600)
    return (f"{head}: {n} rows, {ps.stamp(summary['first_ms'])} to {ps.stamp(summary['last_ms'])} UTC+8 in "
            f"{sum(k > 0 for k in hours)} hours{'; hours short of 3600: ' + short if short else ''}; "
            f"without trade columns {summary['no_trade']} ({summary['no_trade'] / n:.3%}), "
            f"without book columns {summary['no_book']} ({summary['no_book'] / n:.3%})")


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__.split('\n\n')[0])
    parser.add_argument('--root', required=True, help='the recording (research/booktape.py layout)')
    parser.add_argument('--from', dest='first', required=True, type=ps.day_arg, help='first UTC+8 day, YYYY-MM-DD')
    parser.add_argument('--to', dest='end', required=True, type=ps.day_arg, help='UTC+8 day after the last, YYYY-MM-DD')
    parser.add_argument('--rows', required=True, help='directory of the row files')
    parser.add_argument('--source', required=True, choices=SOURCES, help='the name of the row files: interim (the converted capture) or live (the recorder)')
    args = parser.parse_args(argv)
    if args.end <= args.first:
        parser.error('--to must be a later day than --from')
    days = [args.first]
    while ps.next_day(days[-1]) < args.end:
        days.append(ps.next_day(days[-1]))
    found = contracts(args.root)
    if not found:
        raise SystemExit(f'{args.root}: no data file of a book or trade topic')
    first_ms, end_ms = ps.day_start_ms(args.first), ps.day_start_ms(args.end)
    log(f'{args.root}: {len(found)} contracts ({", ".join(f"{v} {s}" for v, s in found)}), days {args.first} to '
        f'{args.end} (excluded), source {args.source}, into {args.rows}')
    for venue in sorted({v for v, _ in found}):
        for feed, holes in sorted(booktape.gaps(venue, first_ms, end_ms, root=args.root).items()):
            text = ', '.join(f'{ps.stamp(a)} to {ps.stamp(b)}' for a, b in holes)
            log(f'{venue} feed {feed}: ' + (f'no connection {text} UTC+8' if holes else 'connected throughout'))
    jobs = [(str(args.root), venue, symbol, day, args.source, str(args.rows)) for venue, symbol in found for day in days]
    rows = files = 0
    with ProcessPoolExecutor(min(len(jobs), WORKERS)) as pool:
        for summary in pool.map(contract_day, jobs):
            log(describe(summary))
            rows += summary['rows']
            files += summary['rows'] > 0
    log(f'{rows} rows in {files} files')
    return 0


if __name__ == '__main__':
    sys.exit(main())
