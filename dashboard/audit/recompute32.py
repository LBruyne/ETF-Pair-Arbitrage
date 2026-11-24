#!/usr/bin/env python3
"""Independent recompute of Combo Spread board 3.2 (experiments/board32_20261122/DESIGN.md, third version, with the
lead's changes of FIX_R2.md A3, A7 and B: the Signal's No Entry, no mean for an invalid number of minutes, ranges from
before the first close, and perp_second_minute read only for minutes at least 66 minutes old).

From the design text alone (sections 1.0-1.8, 1.11, 1.12 and the coverage counts of 3.3), in plain Python with
decimal arithmetic, this recomputes from the raw tables (perp_second, perp_minute, fx_minute, etf_phase,
combo_market) and the 3.1 functions that 3.2 keeps unchanged (combo_anchors, combo_actions, combo_sessions) what
the new database functions return, and compares the two column by column:

    combo_range    the drawn points of the Spread row and the rows of NULLs that break the lines
    combo_prices   the points of the Prices row (trade prices) and their break rows
    combo_last     the two newest seconds of the snapshot strips, kind 'prices' and kind 'basis'
    book_pair      on the recorded books of every mid point of the range (--checks books), and on hand-made books
    top2_size      on each leg's usable recorded book of every point (--checks books), and on hand-made sides
    combo_signal   through the Signal of combo_range and combo_last, and on hand-made cases (--checks vectors)
    perp_coverage  the Data Coverage counts of the combo's three contracts, and the final rows of perp_second_minute

It shares no code or SQL with those functions: their bodies were not read, only their names, arguments and
columns (DESIGN.md 3.1). Numbers compare within 1e-9 (bps; USDT for the fills); prices, anchors, USD/USDT and
the 1+2 depths (contracts) exactly; NULLs, booleans, phases and the Signal text exactly. Every read of a run, the
raw rows and the functions' results alike, goes through one psql session inside one read-only REPEATABLE READ
transaction, so both sides see one snapshot of the database.

    python3 dashboard/audit/recompute32.py --dsn 'host=127.0.0.1 port=5462 user=etfpair_reader dbname=etfpair' \\
        --combo SOXL-SOXS --venue bitget --ma sma --n 30 --range 1h --to '2026-11-23 00:00' \\
        --notional 1000,10000,100000 --entry 2 [--step 1000] [--checks range,prices,last,coverage] [--print 5]

Times are UTC+8 ('YYYY-MM-DD HH:MM[:SS]') or Unix milliseconds. --range (1h, 6h, 24h, 7d, 30d, 365d, 90m, ...) ends
at --to (default: the snapshot's now, to the second); --from overrides it. --step defaults to the board's step for the
span (1, 5, 10, 30 s, else whole minutes, at most about 5000 points). --n is a whole number or 'null' (0 and below are
accepted: no mean). --notional and --entry take comma lists (every pair is checked; 'null' is a NULL; 0 and below are
accepted: No Notional / No Entry; write --entry=-5,2 when the list starts with a minus sign). --now is combo_last's
p_now_ms (default --to). --checks: range, prices, last, coverage, books (book_pair and top2_size on the recorded rows),
vectors (the hand-made cases), mine (no database function is called: this side only). --dump DIR writes both sides'
rows as CSV. (vs31, the comparison with board 3.1's combo_points, went with that function on 2026-11-23, OWNER.md
item 24.) --schema names the schema of the tables and of combo_anchors, combo_actions and combo_sessions
(default etfpair; a scratch copy for fabricated cases), --functions-schema that of the new functions (default
--schema). The DSN is never printed.

Exit status: 0 all equal, 1 a difference, 2 a database function is missing or failed.

Readings of the design where its text leaves a choice (also in the build report):
- A level is the (price, size) pair of one index of the arrays; a NULL price is no level, a size beyond the last
  price is no level. A book is usable (1.0) when both sides have a first level with a price, every level with a
  price has a size above 0 and bid_px[1] < ask_px[1].
- The split rule of 1.0 applies to a leg (or the reference) from its T_v (combo_actions.effective_ms of its Known
  split) while the row's minute is at or after T_v in the carrier session; a NULL book_ms or trade_ms is not
  "before T_v". A leg in its halt (gap_*) has neither a book nor a trade.
- A minute in a leg's split halt or without a segment of combo_anchors has no bar (3.1 leaves those out).
- mu at a point needs the anchors and the mean through the minute before with the same run key, not the point's
  F (as 3.1's combo_points): a point without F has no spread but can have a mean.
- partial_* and filled_* are NULL wherever the real spread is NULL (no notional, no anchor or F, not a mid point);
  a database value there is reported as open in the design, not as a difference.
- A point's past_t is false without a segment (as 3.1's coalesce).
- Entry Points: Best Long is the largest Mean - Real Long Spread over the points with Real Long Spread < Mean, Best
  Short the largest Real Short Spread - Mean over Real Short Spread > Mean, Past T points left out (1.11).
- phase_sort is the 1-based position in ('Pre-Market', 'Regular', 'After-Hours', 'Overnight', 'Closed').
- combo_last kind 'prices' is compared on ts, session, F, anchors and trade prices; kind 'basis' on the point's
  other columns (its p_*, p_ref and a_ref are not defined by the design and are printed only).
- FIX_R2 A7: an Entry that is NULL or not above 0 is No Entry, after No Notional and before No Book. A number of
  minutes n that is NULL or not above 0 gives no mean at all: the points, depths and real spreads as usual, mu NULL,
  so the Signal reads No Mean (or an earlier code) and no point has a phase (Entry Points is empty).
- FIX_R2 B3: the minute bars of a range that starts where no segment of combo_anchors is (before the first close)
  begin at the next segment, since a minute without a segment has no bar; nothing before it is enumerated or read.
  The points are the step's multiples of the range as always (at the board's step about 5000 at most).
- FIX_R2 A3 / B5: perp_second_minute's rows are compared with perp_second (coverage and end rows) only for the minutes
  the board reads from it, minute_ms + 66 min <= the snapshot's now(); the newer minutes are counted as skipped.
"""
import argparse
import bisect
import csv
import os
import re
import subprocess
import sys
import uuid
from collections import Counter, deque
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone
from decimal import ROUND_CEILING, ROUND_FLOOR, Decimal, getcontext

getcontext().prec = 50
MINUTE = 60_000
LAST_WINDOW_MS = 600_000          # combo_last looks for its seconds in (now - 10 min, now]
SETTLED_MS = 66 * MINUTE          # the board reads perp_second_minute for a minute only when minute + 66 min <= now (FIX_R2 A3)
BPS = Decimal(10000)
D0 = Decimal(0)
TOLERANCE = Decimal("1e-9")
PHASES = ("Pre-Market", "Regular", "After-Hours", "Overnight", "Closed")  # 3.1's Entry Points order
PARTIAL = " · Partial"
UTC8 = timezone(timedelta(hours=8))
NULL = "<null>"                   # psql's NULL display in this session
LEGS = ("bull", "bear")
REAL_COLUMNS = ("s_long_bps", "s_short_bps", "partial_long", "partial_short", "filled_long", "filled_short")
DEPTH_COLUMNS = ("bid12_bull", "bid12_bear", "ask12_bull", "ask12_bear")
BOOK_COLUMNS = REAL_COLUMNS + DEPTH_COLUMNS   # book_pair's outputs

# ---------------------------------------------------------------------------------------------------------------
# Section 1.0: what of a leg's second counts
# ---------------------------------------------------------------------------------------------------------------


def levels_of(prices, sizes):
    """One side of a recorded book as (price, size) pairs, best first, from the numeric[] columns: None for a NULL
    price array (no book), [] for {} (an empty side); a size None where the level has none, and a level (None, size)
    for every size beyond the prices (a malformed side, which side_usable rejects)."""
    if prices is None:
        return None
    sizes = sizes or []
    return [(p, sizes[i] if i < len(sizes) else None) for i, p in enumerate(prices)] + [(None, s) for s in sizes[len(prices):]]


def priced(levels):
    """The levels that have a price (a NULL price is no level)."""
    return [(p, s) for p, s in levels if p is not None]


def side_usable(levels):
    """At least one level, and every level has a price and a size above 0: as many sizes as prices and no NULL inside
    (the rule of the SQL's etfpair.book_usable, written into SPEC; a malformed side is no book, since the sweep is
    undefined on it; none has been recorded)."""
    return bool(levels) and all(p is not None and s is not None and s > 0 for p, s in levels)


def book_usable(bids, asks):
    """Section 1.0's usable book before the split rule: both sides usable and bid_px[1] < ask_px[1] (a crossed or
    locked book counts as none)."""
    return side_usable(bids) and side_usable(asks) and bids[0][0] < asks[0][0]


def mid_of(row):
    return (row.bids[0][0] + row.asks[0][0]) / 2


def spread_bps(fx, p_bull, p_bear, a_bull, a_bear):
    """s = 10^4 (F P_bull / A_bull + F P_bear / A_bear - 2); NULL without F, an anchor or a price."""
    if fx is None or p_bull is None or p_bear is None or a_bull is None or a_bear is None:
        return None
    return BPS * (fx * p_bull / a_bull + fx * p_bear / a_bear - 2)


# ---------------------------------------------------------------------------------------------------------------
# Section 1.4: the capped two-pass sweep; section 1.7: the 1+2 depths; book_pair
# ---------------------------------------------------------------------------------------------------------------


def take(levels, q):
    """The average price of taking q contracts from levels (priced, sizes above 0), best first. Under the cap q never
    exceeds their total; should rounding put it a hair above, the whole side is the fill."""
    got = paid = D0
    for price, size in levels:
        part = min(size, q - got)
        paid += part * price
        got += part
        if got >= q:
            break
    return paid / got


def capped_sweep(side_bull, side_bear, a_bull, a_bear, notional):
    """Section 1.4 on one side of both books (priced levels, best first): the average prices (v_bull, v_bear) and the
    quantities (q_bull, q_bear) of the last pass, and whether K_want exceeded K_max in either pass."""
    k_max = min(a_bull * sum(s for _, s in side_bull), a_bear * sum(s for _, s in side_bear))
    v = (side_bull[0][0], side_bear[0][0])
    q, partial = None, False
    for _ in range(2):
        k_want = notional / (v[0] / a_bull + v[1] / a_bear)
        partial = partial or k_want > k_max
        k = min(k_want, k_max)
        q = (k / a_bull, k / a_bear)
        v = (take(side_bull, q[0]), take(side_bear, q[1]))
    return v, q, partial


def bucket(price, bid):
    """The 0.01 bucket of a price: down on bids, up on asks (the identity at a 0.01 tick)."""
    return (price * 100).to_integral_value(rounding=ROUND_FLOOR if bid else ROUND_CEILING) / 100


def top2_size(levels, bid):
    """Section 1.7 on one side of one leg's usable book (priced levels, best first): the contracts at its first two
    distinct 0.01 buckets, or None (Not Recorded) when no recorded level lies beyond the second."""
    buckets, size = [], D0
    for price, s in levels:
        b = bucket(price, bid)
        if b not in buckets:
            if len(buckets) == 2:
                return size
            buckets.append(b)
        size += s
    return None


def book_pair(bids_bull, asks_bull, bids_bear, asks_bear, a_bull, a_bear, fx, notional):
    """etfpair.book_pair on usable books of both legs (the caller filters, section 1.0): the capped real spreads of N
    on the asks (long) and the bids (short) with their partial flags and filled USDT, NULL without both anchors, F or
    a positive N; and each leg's Bid / Ask 1+2 Depth in contracts, which need none of these."""
    sides = {"bid": (priced(bids_bull), priced(bids_bear)), "ask": (priced(asks_bull), priced(asks_bear))}
    out = dict.fromkeys(REAL_COLUMNS)
    for i, leg in enumerate(LEGS):
        out["bid12_" + leg] = top2_size(sides["bid"][i], True)
        out["ask12_" + leg] = top2_size(sides["ask"][i], False)
    if a_bull is None or a_bear is None or fx is None or notional is None or not notional > 0:
        return out
    for name, (bull, bear) in (("long", sides["ask"]), ("short", sides["bid"])):
        v, q, partial = capped_sweep(bull, bear, a_bull, a_bear, notional)
        out["s_%s_bps" % name] = BPS * (fx * v[0] / a_bull + fx * v[1] / a_bear - 2)
        out["partial_" + name] = partial
        out["filled_" + name] = q[0] * v[0] + q[1] * v[1]
    return out


# ---------------------------------------------------------------------------------------------------------------
# Section 1.6: the Signal
# ---------------------------------------------------------------------------------------------------------------


def signal_of(s_bps, past_t, mu_bps, notional, mid, s_long_bps, s_short_bps, partial_long, partial_short, entry):
    """No Anchor (no s) -> Past T -> No Mean -> No Notional (N not positive) -> No Entry (d NULL, not positive or not finite,
    FIX_R2 A7) -> No Book (not a mid point) -> Long (Real Long - Mean <= -d) -> Short (Real Short - Mean >= d) -> None;
    ' · Partial' when that side is capped. The comparisons keep the sign; only the display takes absolute values (1.5)."""
    if s_bps is None:
        return "No Anchor"
    if past_t:
        return "Past T"
    if mu_bps is None:
        return "No Mean"
    if notional is None or not notional > 0:
        return "No Notional"
    if entry is None or (isinstance(entry, Decimal) and not entry.is_finite()) or not entry > 0:
        return "No Entry"
    if not mid:
        return "No Book"
    if s_long_bps is not None and s_long_bps - mu_bps <= -entry:
        return "Long" + (PARTIAL if partial_long else "")
    if s_short_bps is not None and s_short_bps - mu_bps >= entry:
        return "Short" + (PARTIAL if partial_short else "")
    return "None"


def board_step(span_ms):
    """The board's spacing of the drawn points (combo_spread_board.STEP_MS): the finest of 1, 5, 10, 30 s, else whole
    minutes, that keeps a line at or under about 5000 points."""
    r = Decimal(span_ms) / 5000
    for step in (1000, 5000, 10000, 30000):
        if r <= step:
            return step
    return int((r / MINUTE).to_integral_value(rounding=ROUND_CEILING)) * MINUTE


def phase_sort(phase):
    return PHASES.index(phase) + 1 if phase in PHASES else None


# ---------------------------------------------------------------------------------------------------------------
# The inputs of one combo on one venue
# ---------------------------------------------------------------------------------------------------------------


@dataclass(frozen=True)
class Segment:
    """A row of combo_anchors: the anchors in use in [from_ms, to_ms) of a session."""
    from_ms: int
    to_ms: object            # int, or None for the newest session's open end
    session: str
    a_bull: object = None
    a_bear: object = None
    a_ref: object = None
    gap_bull: bool = False
    gap_bear: bool = False
    gap_ref: bool = False
    past_t: bool = False

    def key(self):
        """What a run of the mean keeps: the session, the legs' anchors and past_t (3.1)."""
        return (self.session, self.a_bull, self.a_bear, self.past_t)


@dataclass(frozen=True)
class Second:
    """A row of perp_second: the last trade before T and the newest book frame before T."""
    price: object = None
    trade_ms: object = None
    bids: object = None      # levels_of(bid_px, bid_sz)
    asks: object = None
    book_ms: object = None


class Market:
    """Everything the computations read, keyed the way they read it. A leg is bull, bear or ref."""

    def __init__(self, segments, splits=None, fx=None, candles=None, seconds=None, statuses=None, phases=None):
        self.segments = sorted(segments, key=lambda g: g.from_ms)
        self.starts = [g.from_ms for g in self.segments]
        self.session_start = {}
        for g in self.segments:
            self.session_start.setdefault(g.session, g.from_ms)
        self.splits = splits or {}        # leg -> [(carrier session, T_v)]
        self.fx = fx or {}                # minute -> USD per USDT
        self.candles = candles or {}      # leg -> {minute: close}
        self.seconds = seconds or {}      # leg -> {T: Second}
        self.statuses = statuses or {}    # session -> NAV | Bell | Missing (combo_sessions)
        self.set_phases(phases or [])
        self.means_cache = {}

    def set_phases(self, phases):
        self.phases = sorted(phases)      # (start_ms, end_ms, phase)
        self.phase_starts = [p[0] for p in self.phases]

    def segment(self, minute):
        i = bisect.bisect_right(self.starts, minute) - 1
        if i < 0:
            return None
        g = self.segments[i]
        return g if g.to_ms is None or minute < g.to_ms else None

    def fx_at(self, minute):
        """F of a minute: its own, else the newest of the five minutes before it."""
        for back in range(6):
            f = self.fx.get(minute - back * MINUTE)
            if f is not None:
                return f
        return None

    def tv(self, leg, seg, minute):
        """T_v of the leg's split when the minute is at or after it in its carrier session, else None."""
        if seg is None:
            return None
        for session, tv in self.splits.get(leg, ()):
            if session == seg.session and minute >= tv:
                return tv
        return None

    def book(self, leg, ts, seg, minute):
        """The leg's row at T when its book is usable (section 1.0, the halt and the split rule included), else None."""
        row = self.seconds.get(leg, {}).get(ts)
        if row is None or (seg is not None and getattr(seg, "gap_" + leg)) or not book_usable(row.bids, row.asks):
            return None
        tv = self.tv(leg, seg, minute)
        if tv is not None and row.book_ms is not None and row.book_ms < tv:
            return None
        return row

    def trade(self, leg, ts, seg, minute):
        """The leg's usable trade price at T (section 1.0), else None."""
        row = self.seconds.get(leg, {}).get(ts)
        if row is None or row.price is None or (seg is not None and getattr(seg, "gap_" + leg)):
            return None
        tv = self.tv(leg, seg, minute)
        if tv is not None and row.trade_ms is not None and row.trade_ms < tv:
            return None
        return row.price

    def basis(self, ts, seg, minute):
        """Section 1.2 at T, in pairs: (basis, P_bull, P_bear, books) with basis 'mid' when both legs have a usable book,
        'trade' when not but both have a usable trade, None otherwise; books maps each leg to its usable book or None."""
        books = {leg: self.book(leg, ts, seg, minute) for leg in LEGS}
        if books["bull"] is not None and books["bear"] is not None:
            return "mid", mid_of(books["bull"]), mid_of(books["bear"]), books
        p_bull, p_bear = self.trade("bull", ts, seg, minute), self.trade("bear", ts, seg, minute)
        if p_bull is not None and p_bear is not None:
            return "trade", p_bull, p_bear, books
        return None, None, None, books

    def phase(self, ts):
        """The etf_phase with start_ms < T <= end_ms."""
        i = bisect.bisect_left(self.phase_starts, ts) - 1
        if i >= 0 and ts <= self.phases[i][1]:
            return self.phases[i][2]
        return None

    def lookback(self, first_minute):
        """Where the bars must start: the start of the session holding the first minute (no run crosses a session); when
        no segment holds it (before the first close, or between segments), the start of the next segment, since a minute
        without a segment has no bar: a range from long before the first close enumerates and reads no minute before it."""
        seg = self.segment(first_minute)
        if seg is not None:
            return self.session_start[seg.session]
        i = bisect.bisect_right(self.starts, first_minute)
        return self.starts[i] if i < len(self.starts) else first_minute


def halted(seg):
    return seg is not None and (seg.gap_bull or seg.gap_bear)


# ---------------------------------------------------------------------------------------------------------------
# Section 1.3: the minute bars and the runs of the mean
# ---------------------------------------------------------------------------------------------------------------


def minute_bars(market, start, stop):
    """The bars of the minutes j in [start, stop): (j, run key, (F P_bull, F P_bear) or None without F, source). P from
    the row T = j + 60 s: both legs' mids, else both legs' trades (section 1.0's filters), else both legs' candle
    closes; a minute with none of these, in a leg's split halt or without a segment has no bar."""
    out = []
    for j in range(start, stop, MINUTE):
        seg = market.segment(j)
        if seg is None or halted(seg):
            continue
        source, p_bull, p_bear, _ = market.basis(j + MINUTE, seg, j)
        if source is None:
            p_bull, p_bear = market.candles.get("bull", {}).get(j), market.candles.get("bear", {}).get(j)
            if p_bull is None or p_bear is None:
                continue
            source = "candle"
        f = market.fx_at(j)
        out.append((j, seg.key(), (f * p_bull, f * p_bear) if f is not None else None, source))
    return out


def run_means(bars, ma, n):
    """{j: (key, (m_bull, m_bear))} for each bar j whose run holds n bars: the per-leg USD mean through j (the mean of
    minute j + 1). A run restarts at a missing minute, a minute without F and a change of key; SMA over the last n
    bars, EMA with alpha = 2 / (n + 1) seeded with the run's first bar. A change of the bars' source does not restart."""
    alpha = Decimal(2) / (n + 1)
    out = {}
    run, sums, ema, prev, prev_key = deque(), [D0, D0], None, None, None
    for j, key, usd, _ in bars:
        if usd is None or prev is None or j - prev != MINUTE or key != prev_key:
            run.clear()
            sums, ema = [D0, D0], None
        prev_key = key
        if usd is None:
            prev = None
            continue
        run.append(usd)
        sums = [sums[0] + usd[0], sums[1] + usd[1]]
        if len(run) > n:
            old = run.popleft()
            sums = [sums[0] - old[0], sums[1] - old[1]]
        ema = usd if ema is None else (alpha * usd[0] + (1 - alpha) * ema[0], alpha * usd[1] + (1 - alpha) * ema[1])
        prev = j
        if len(run) >= n:
            out[j] = (key, ema if ma == "ema" else (sums[0] / n, sums[1] / n))
    return out


def mean_bps(means, seg, minute):
    """mu of a point in the minute: the mean through the minute before, of the same run key; None otherwise."""
    if seg is None or seg.a_bull is None or seg.a_bear is None:
        return None
    before = means.get(minute - MINUTE)
    if before is None or before[0] != seg.key():
        return None
    m = before[1]
    return BPS * (m[0] / seg.a_bull + m[1] / seg.a_bear - 2)


# ---------------------------------------------------------------------------------------------------------------
# combo_range, combo_prices, combo_last (sections 1.1-1.8, 1.12)
# ---------------------------------------------------------------------------------------------------------------

RANGE_COLUMNS = ("ts_ms", "session_date", "phase", "phase_sort", "basis", "past_t", "s_bps", "mu_bps", "s_long_bps",
                 "s_short_bps", "partial_long", "partial_short", "book_bull", "book_bear") + DEPTH_COLUMNS + ("a_ok", "signal")
PRICE_COLUMNS = ("ts_ms", "session_date", "fx", "p_bull", "p_bear", "p_ref", "a_bull", "a_bear", "a_ref")
LAST_COLUMNS = ("kind", "ts_ms", "session_date", "basis", "past_t", "fx", "a_bull", "a_bear", "a_ref", "p_bull", "p_bear",
                "p_ref", "s_bps", "mu_bps") + REAL_COLUMNS + ("book_bull", "book_bear") + DEPTH_COLUMNS + ("anchor_status", "signal")
# The compared columns of each kind of combo_last row; the others are printed only (the design does not define them).
LAST_DEFINED = {"prices": ("ts_ms", "session_date", "fx", "a_bull", "a_bear", "a_ref", "p_bull", "p_bear", "p_ref"),
                "basis": ("ts_ms", "session_date", "basis", "past_t", "fx", "a_bull", "a_bear", "s_bps", "mu_bps") + REAL_COLUMNS
                         + ("book_bull", "book_bear") + DEPTH_COLUMNS + ("anchor_status", "signal")}


def multiples(from_ms, to_ms, step):
    """The multiples of step in (from_ms, to_ms], ascending."""
    return range((from_ms // step + 1) * step, to_ms - to_ms % step + 1, step)


def minute_of(ts):
    """The minute a row T belongs to: the one holding T - 1 ms."""
    return (ts - 1) // MINUTE * MINUTE


def with_breaks(points, step, columns):
    """A row of NULLs 1 ms after a point when the next one is more than a step later or in another session; none
    before the first."""
    out, prev = [], None
    for p in points:
        if prev is not None and (p["ts_ms"] - prev["ts_ms"] > step or p["session_date"] != prev["session_date"]):
            row = dict.fromkeys(columns)
            row["ts_ms"] = prev["ts_ms"] + 1
            out.append(row)
        out.append(p)
        prev = p
    return out


def mean_minutes_valid(n):
    """Whether n gives a mean: a whole number above 0 (FIX_R2 A7: NULL, 0 and below draw the points without a mean)."""
    return n is not None and n > 0


def means_for(market, ma, n, from_ms, last_minute):
    """The runs' means for the points from just after from_ms to the minute last_minute (cached per market); none at
    all, and no bar made, when n gives no mean."""
    if not mean_minutes_valid(n):
        return {}
    start = market.lookback(from_ms // MINUTE * MINUTE)
    key = (ma, n, start, last_minute)
    if key not in market.means_cache:
        market.means_cache[key] = run_means(minute_bars(market, start, last_minute), ma, n)
    return market.means_cache[key]


def point_at(market, means, ts, notional, entry):
    """Every column of the point at T (sections 1.1-1.7), with the basis prices, F, the anchors and the fills for
    combo_last; None when T has no point (no basis price, which includes a leg in its halt)."""
    minute = minute_of(ts)
    seg = market.segment(minute)
    basis, p_bull, p_bear, books = market.basis(ts, seg, minute)
    if basis is None:
        return None
    fx = market.fx_at(minute)
    a_bull, a_bear = (seg.a_bull, seg.a_bear) if seg else (None, None)
    s = spread_bps(fx, p_bull, p_bear, a_bull, a_bear)
    mu = mean_bps(means, seg, minute)
    past_t = bool(seg and seg.past_t)
    row = dict.fromkeys(BOOK_COLUMNS)
    if basis == "mid":
        b, r = books["bull"], books["bear"]
        row.update(book_pair(b.bids, b.asks, r.bids, r.asks, a_bull, a_bear, fx, notional))
    else:
        for leg in LEGS:  # a leg with a usable book shows its depths on a trade point too (1.7)
            if books[leg] is not None:
                row["bid12_" + leg] = top2_size(priced(books[leg].bids), True)
                row["ask12_" + leg] = top2_size(priced(books[leg].asks), False)
    phase = market.phase(ts) if s is not None and mu is not None else None
    row.update(ts_ms=ts, session_date=seg.session if seg else None, phase=phase, phase_sort=phase_sort(phase), basis=basis,
               past_t=past_t, s_bps=s, mu_bps=mu, book_bull=books["bull"] is not None, book_bear=books["bear"] is not None,
               a_ok=a_bull is not None and a_bear is not None and fx is not None,
               signal=signal_of(s, past_t, mu, notional, basis == "mid", row["s_long_bps"], row["s_short_bps"],
                                row["partial_long"], row["partial_short"], entry),
               fx=fx, a_bull=a_bull, a_bear=a_bear, a_ref=seg.a_ref if seg else None, p_bull=p_bull, p_bear=p_bear)
    return row


def combo_range(market, ma, n, from_ms, to_ms, step, notional, entry):
    """What etfpair.combo_range returns: the points of (from, to] at the step's multiples and the break rows."""
    ts_list = multiples(from_ms, to_ms, step)
    if not ts_list:
        return []
    means = means_for(market, ma, n, from_ms, minute_of(ts_list[-1]))
    points = [p for p in (point_at(market, means, ts, notional, entry) for ts in ts_list) if p is not None]
    return [{c: r.get(c) for c in RANGE_COLUMNS} for r in with_breaks(points, step, RANGE_COLUMNS)]


def price_point(market, ts):
    """The Prices row's point at T (section 1.8): both legs' usable trades; the reference's, or NULL."""
    minute = minute_of(ts)
    seg = market.segment(minute)
    p_bull, p_bear = market.trade("bull", ts, seg, minute), market.trade("bear", ts, seg, minute)
    if p_bull is None or p_bear is None:
        return None
    return {"ts_ms": ts, "session_date": seg.session if seg else None, "fx": market.fx_at(minute), "p_bull": p_bull,
            "p_bear": p_bear, "p_ref": market.trade("ref", ts, seg, minute), "a_bull": seg.a_bull if seg else None,
            "a_bear": seg.a_bear if seg else None, "a_ref": seg.a_ref if seg else None}


def combo_prices(market, from_ms, to_ms, step):
    """What etfpair.combo_prices returns."""
    points = [p for p in (price_point(market, ts) for ts in multiples(from_ms, to_ms, step)) if p is not None]
    return with_breaks(points, step, PRICE_COLUMNS)


def combo_last(market, ma, n, notional, entry, now_ms):
    """What etfpair.combo_last returns as of now: kind 'prices', the newest second of (now - 10 min, now] at which both
    legs and the reference have usable trades; kind 'basis', the newest one at which both legs have a basis price."""
    out = []
    window = list(multiples(now_ms - LAST_WINDOW_MS, now_ms, 1000))[::-1]
    for ts in window:
        p = price_point(market, ts)
        if p is not None and p["p_ref"] is not None:
            row = dict.fromkeys(LAST_COLUMNS)
            row.update(p, kind="prices", anchor_status=market.statuses.get(p["session_date"]))
            out.append(row)
            break
    if window:
        means = means_for(market, ma, n, now_ms - LAST_WINDOW_MS, minute_of(window[0]))
        for ts in window:
            p = point_at(market, means, ts, notional, entry)
            if p is not None:
                row = dict.fromkeys(LAST_COLUMNS)
                row.update({c: p[c] for c in LAST_COLUMNS if c in p}, kind="basis",
                           anchor_status=market.statuses.get(p["session_date"]))
                out.append(row)
                break
    return out


# ---------------------------------------------------------------------------------------------------------------
# Section 1.11: Entry Points by Phase; section 3.3: the coverage counts
# ---------------------------------------------------------------------------------------------------------------

ENTRY_COLUMNS = ("phase", "long_points", "short_points", "partial_points", "best_long", "best_short", "past_t_points",
                 "no_book_points", "points")


def entry_points(rows):
    """The table of section 1.11 from combo_range's rows: the rows with a phase (the drawn points whose Deviation is
    not NULL) by phase, in 3.1's order. Best Long / Short: the largest absolute deviation of the real spread on the
    favourable side of the mean (below it for Long, above for Short), Past T points left out; None ('–') when no point
    lies on that side."""
    groups = {}
    for r in rows:
        if r.get("phase") is None:
            continue
        g = groups.setdefault(r["phase"], dict(dict.fromkeys(ENTRY_COLUMNS, 0), phase=r["phase"], best_long=None, best_short=None))
        sig = r["signal"]
        g["points"] += 1
        g["long_points"] += sig in ("Long", "Long" + PARTIAL)
        g["short_points"] += sig in ("Short", "Short" + PARTIAL)
        g["partial_points"] += sig in ("Long" + PARTIAL, "Short" + PARTIAL)
        g["past_t_points"] += sig == "Past T"
        g["no_book_points"] += sig == "No Book"
        if r["past_t"] or r["mu_bps"] is None:
            continue
        if r["s_long_bps"] is not None and r["s_long_bps"] < r["mu_bps"]:
            x = r["mu_bps"] - r["s_long_bps"]
            g["best_long"] = x if g["best_long"] is None else max(g["best_long"], x)
        if r["s_short_bps"] is not None and r["s_short_bps"] > r["mu_bps"]:
            x = r["s_short_bps"] - r["mu_bps"]
            g["best_short"] = x if g["best_short"] is None else max(g["best_short"], x)
    return [{c: g[c] for c in ENTRY_COLUMNS} for g in sorted(groups.values(), key=lambda g: phase_sort(g["phase"]) or 99)]


def minute_counts(rows):
    """Section 3.3's counts per minute of perp_second rows (second_ms, has a price, has a best bid and a best ask,
    cardinality(bid_px), cardinality(ask_px)): {minute_ms: [seconds, books, least_levels, most_levels]}, the minute of
    T being the one that holds T - 1 ms; the levels, over the rows with a book, of the thinner and the deeper side
    (3.1's Data Coverage)."""
    out = {}
    for ts, has_price, has_book, n_bid, n_ask in rows:
        c = out.setdefault(minute_of(ts), [0, 0, None, None])
        c[0] += 1 if has_price else 0
        if has_book:
            c[1] += 1
            low, high = min(n_bid, n_ask), max(n_bid, n_ask)
            c[2] = low if c[2] is None else min(c[2], low)
            c[3] = high if c[3] is None else max(c[3], high)
    return out


def coverage(minutes):
    """(seconds, books, least_levels, most_levels) over per-minute counts."""
    minutes = list(minutes)
    return (sum(m[0] for m in minutes), sum(m[1] for m in minutes), min((m[2] for m in minutes if m[2] is not None), default=None),
            max((m[3] for m in minutes if m[3] is not None), default=None))


COUNT_COLUMNS = ("seconds", "books", "least_levels", "most_levels")


def settled(minute_ms, now_ms):
    """Whether the board reads the minute from perp_second_minute (FIX_R2 A3): minute + 66 min <= now. The recorder writes
    a failed batch at most an hour late and the collector refreshes the last 65 minutes every minute; a newer minute's
    row may still be rewritten, so it is not compared (on production; the throwaway copy has no new rows)."""
    return minute_ms + SETTLED_MS <= now_ms


def final_row(minute_ms, refreshed_ms):
    """A perp_second_minute row aggregated after all its rows were final, by its watermark: minute + 70 s <= refreshed_ms."""
    return minute_ms + MINUTE + 10_000 <= refreshed_ms


def whole_minutes(lo, hi):
    """How many of the whole minutes lo, lo + 1 min, ... are at or before hi (lo a whole minute)."""
    return max(0, (hi - lo) // MINUTE + 1)


@dataclass
class TableCheck:
    """table_check's counts: over the settled minutes, except skipped and skipped_final (over the newer ones)."""
    whole: int = 0              # whole minutes of the range
    rows: int = 0               # table rows of them
    final: int = 0              # of those, final by the watermark
    differ: tuple = ()          # final rows whose counts differ from perp_second's
    missing: int = 0            # minutes with rows in perp_second and no table row
    skipped: int = 0            # whole minutes newer than now - 66 min, not compared
    skipped_final: int = 0      # final rows among them


def table_check(per_minute, table, lo, hi, now_ms):
    """The perp_second_minute rows {minute: {column: text}} of the whole minutes lo..hi against the counts made here
    (minute_counts), where the board reads them: settled minutes (minute + 66 min <= now) with a final row."""
    edge = min(hi, now_ms - SETTLED_MS)
    out = TableCheck(whole=whole_minutes(lo, edge), skipped=whole_minutes(lo, hi) - whole_minutes(lo, edge))
    differ = []
    for m, r in sorted(table.items()):
        final = final_row(m, int(r["refreshed_ms"]))
        if not settled(m, now_ms):
            out.skipped_final += final
            continue
        out.rows += 1
        if final:
            out.final += 1
            if per_minute.get(m, [0, 0, None, None]) != [parse(c, r[c]) for c in COUNT_COLUMNS]:
                differ.append(m)
    out.differ = tuple(differ)
    out.missing = sum(1 for m in per_minute if lo <= m <= hi and settled(m, now_ms) and m not in table)
    return out


def opt_int(text):
    return None if text is None else int(text)


def end_values(r):
    """What a minute's end-row columns must hold, from the perp_second row at the minute's end ({column: text}, None
    without one): its price and trade_ms, the mid of its book when the book is usable leaving the split rule aside
    (book_usable) and its book_ms; all NULL without that row."""
    if r is None:
        return (None, None, None, None)
    bids, asks = levels_of(num_array(r["bid_px"]), num_array(r["bid_sz"])), levels_of(num_array(r["ask_px"]), num_array(r["ask_sz"]))
    mid = (bids[0][0] + asks[0][0]) / 2 if bids is not None and asks is not None and book_usable(bids, asks) else None
    return (dec(r["price"]), opt_int(r["trade_ms"]), mid, opt_int(r["book_ms"]))


def end_row_check(final, ends, now_ms):
    """The end-row columns of the final perp_second_minute rows {minute: {column: text}} against end_values of the
    perp_second rows at the minutes' ends {minute: {column: text}}, on the settled minutes only. Returns the differing
    [(minute, the table's, made here)], how many rows were compared, how many of them have an end row, how many skipped."""
    wrong, compared, with_end, skipped = [], 0, 0, 0
    for m, t in sorted(final.items()):
        if not settled(m, now_ms):
            skipped += 1
            continue
        compared += 1
        with_end += m in ends
        got = (dec(t["end_price"]), opt_int(t["end_trade_ms"]), dec(t["end_mid"]), opt_int(t["end_book_ms"]))
        want = end_values(ends.get(m))
        if got != want:
            wrong.append((m, got, want))
    return wrong, compared, with_end, skipped


# ---------------------------------------------------------------------------------------------------------------
# Hand-made cases, run on both sides with --checks vectors (also inputs of the unit tests)
# ---------------------------------------------------------------------------------------------------------------


def _side(*pairs):
    return [(Decimal(p), Decimal(s)) for p, s in pairs]


# The root SPEC 7.9 books (A 40 / 60, F 1); the bids hold two levels each, so the 1+2 depths there are Not Recorded.
SPEC79 = dict(bids_bull=_side(("40.45", 300), ("40.44", 500)), asks_bull=_side(("40.47", 300)),
              bids_bear=_side(("59.43", 60), ("59.42", 200)), asks_bear=_side(("59.45", 100), ("59.46", 200)),
              a_bull=Decimal(40), a_bear=Decimal(60), fx=Decimal(1))
WHOLE = dict(bids_bull=_side(("40.45", 300), ("40.44", 500), ("40.43", 700)),
             asks_bull=_side(("40.47", 300), ("40.48", 100), ("40.49", 100)),
             bids_bear=_side(("59.43", 60), ("59.42", 200), ("59.41", 500)),
             asks_bear=_side(("59.45", 100), ("59.46", 200), ("59.47", 50)),
             a_bull=Decimal(40), a_bear=Decimal(60), fx=Decimal("1.0003"))
# A 0.001-tick bear (Bitget SOXS-like): bids 30.459 30.455 | 30.449 30.441 | 30.439, asks 30.461 30.462 | 30.471 | 30.481.
TICK = dict(bids_bull=_side(("45.01", 10), ("45.00", 20), ("44.99", 30)),
            asks_bull=_side(("45.02", 10), ("45.03", 20), ("45.04", 30)),
            bids_bear=_side(("30.459", 5), ("30.455", 7), ("30.449", 11), ("30.441", 13), ("30.439", 17)),
            asks_bear=_side(("30.461", 3), ("30.462", 4), ("30.471", 6), ("30.481", 8)),
            a_bull=Decimal("44.8"), a_bear=Decimal("30.7"), fx=Decimal("0.9998"))
BOOK_VECTORS = [
    ("spec79 N 10000", dict(SPEC79, notional=Decimal(10000))),
    ("spec79 N 30000 long capped", dict(SPEC79, notional=Decimal(30000))),
    ("spec79 N 31227 short capped in pass 2", dict(SPEC79, notional=Decimal(31227))),
    ("spec79 N 1e6 both capped", dict(SPEC79, notional=Decimal(1000000))),
    ("spec79 N NULL", dict(SPEC79, notional=None)),
    ("spec79 N 0", dict(SPEC79, notional=D0)),
    ("spec79 N -5", dict(SPEC79, notional=Decimal(-5))),
    ("whole N 1", dict(WHOLE, notional=Decimal(1))),
    ("whole N 20000", dict(WHOLE, notional=Decimal(20000))),
    ("whole N 100000", dict(WHOLE, notional=Decimal(100000))),
    ("tick N 500", dict(TICK, notional=Decimal(500))),
    ("tick N 100000", dict(TICK, notional=Decimal(100000))),
    ("tick bear 2 buckets only", dict(TICK, bids_bear=_side(("30.459", 5), ("30.449", 11)),
                                      asks_bear=_side(("30.461", 3), ("30.471", 6)), notional=Decimal(1000))),
    ("one level each (Binance temporary)", dict(SPEC79, bids_bull=_side(("40.45", 300)), bids_bear=_side(("59.43", 60)),
                                                asks_bear=_side(("59.45", 100)), notional=Decimal(5000))),
    ("whole A NULL", dict(WHOLE, a_bull=None, notional=Decimal(10000))),
    ("whole F NULL", dict(WHOLE, fx=None, notional=Decimal(10000))),
]
# One side of one leg's book and the side it is: (name, prices, sizes, bids).
TOP2_VECTORS = [
    ("0.01 bids, third level", ["40.45", "40.44", "40.43"], ["300", "500", "700"], True),
    ("0.01 bids, two levels: Not Recorded", ["40.45", "40.44"], ["300", "500"], True),
    ("0.01 asks, a price gap", ["40.47", "40.49", "40.52"], ["1", "2", "4"], False),
    ("one level: Not Recorded", ["40.45"], ["300"], True),
    ("0.001 bids floor", ["30.459", "30.455", "30.449", "30.441", "30.439"], ["5", "7", "11", "13", "17"], True),
    ("0.001 asks ceil", ["30.461", "30.462", "30.471", "30.481"], ["3", "4", "6", "8"], False),
    ("0.001 asks on the boundary", ["30.470", "30.471", "30.480", "30.481"], ["2", "3", "4", "5"], False),
    ("0.001 bids on the boundary", ["30.450", "30.449", "30.440", "30.439"], ["2", "3", "4", "5"], True),
    ("trailing zeros", ["40.45000", "40.44000", "40.43000"], ["1.50", "2.250", "3"], True),
    ("0.001 asks within two buckets: Not Recorded", ["30.461", "30.468", "30.471", "30.479"], ["1", "1", "1", "1"], False),
]

# (s, past_t, mu, N, mid, s_long, s_short, partial_long, partial_short, entry) -> the Signal
SIGNAL_VECTORS = [
    ((None, True, None, None, False, None, None, None, None, 2), "No Anchor"),
    ((5, True, None, None, False, None, None, None, None, 2), "Past T"),
    ((5, True, 1, 10000, True, -9, 9, False, False, 2), "Past T"),
    ((5, False, None, 10000, True, -9, 9, False, False, 2), "No Mean"),
    ((5, False, 1, None, True, None, None, None, None, 2), "No Notional"),
    ((5, False, 1, 0, True, None, None, None, None, 2), "No Notional"),
    ((5, False, 1, -1, True, None, None, None, None, 2), "No Notional"),
    ((5, False, 1, None, False, None, None, None, None, 2), "No Notional"),   # before No Book
    ((5, False, 1, 10000, False, None, None, None, None, 2), "No Book"),
    ((5, False, 1, 10000, True, -1, -3, False, False, 2), "Long"),
    ((5, False, 1, 10000, True, "-1.0000000001", -3, True, False, 2), "Long" + PARTIAL),
    ((5, False, 1, 10000, True, "-0.9999999999", -3, True, False, 2), "None"),
    ((5, False, 1, 10000, True, 4, 3, False, False, 2), "Short"),
    ((5, False, 1, 10000, True, 4, 3, False, True, 2), "Short" + PARTIAL),
    ((5, False, 1, 10000, True, 4, "2.9999999999", False, True, 2), "None"),
    ((5, False, 1, 10000, True, 2, 0, True, True, 2), "None"),
    ((5, False, 1, 10000, True, 4, -2, True, True, 2), "None"),      # |deviations| = 3 >= 2 on the wrong sides
    # No Entry (FIX_R2 A7): d NULL or not above 0, after No Notional and before No Book.
    ((5, False, 1, 10000, True, 1, 1, False, False, 0), "No Entry"),            # d 0 would make both sides signal
    ((5, False, 1, 10000, True, 2, 1, False, True, 0), "No Entry"),
    ((5, False, 1, 10000, True, 3, -1, False, False, None), "No Entry"),
    ((5, False, 1, 10000, True, -9, 9, True, True, -5), "No Entry"),
    ((5, False, 1, 10000, False, None, None, None, None, None), "No Entry"),   # before No Book
    ((5, False, 1, 10000, False, None, None, None, None, -5), "No Entry"),
    ((5, False, 1, None, False, None, None, None, None, None), "No Notional"),  # No Notional before No Entry
    ((5, False, 1, 0, True, None, None, None, None, 0), "No Notional"),
    ((5, False, None, 10000, True, -9, 9, False, False, None), "No Mean"),      # No Mean before No Entry
    ((5, True, 1, 10000, True, -9, 9, False, False, 0), "Past T"),
    ((None, False, None, 10000, True, None, None, None, None, -5), "No Anchor"),
    ((5, False, 1, 10000, True, 1, 1, False, False, "0.0000000001"), "None"),  # any d above 0 is an Entry
    ((5, False, 1, 10000, True, "0.9999999999", 3, False, False, "0.0000000001"), "Long"),
    ((5, False, 1, 10000, True, 1, "1.0000000001", False, False, "0.0000000001"), "Short"),
]


def signal_case(args):
    """A SIGNAL_VECTORS input with its numbers as Decimal."""
    return tuple(Decimal(str(a)) if isinstance(a, (int, str)) and not isinstance(a, bool) else a for a in args)


# ---------------------------------------------------------------------------------------------------------------
# The database: one psql session, one read-only REPEATABLE READ transaction
# ---------------------------------------------------------------------------------------------------------------


class Snapshot:
    """One psql session holding one read-only REPEATABLE READ transaction: every query sees the database as of the
    first one. The DSN goes to psql only."""

    def __init__(self, dsn):
        self.mark = "@@recompute32-%s@@" % uuid.uuid4().hex
        self.psql = subprocess.Popen(["psql", "-X", "-w", "-q", "--csv", "-v", "ON_ERROR_STOP=1", "-d", dsn],
                                     stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True)
        self.psql.stdin.write("\\pset null '%s'\n" % NULL)
        self.query("BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY")

    def stream(self, sql):
        """The rows of one statement as lists of text, None for NULL, its header first. Read to the end."""
        self.psql.stdin.write(sql.rstrip().rstrip(";") + ";\n\\echo " + self.mark + "\n")
        self.psql.stdin.flush()
        for row in csv.reader(self._lines(sql)):
            yield [None if v == NULL else v for v in row]

    def _lines(self, sql):
        for line in self.psql.stdout:
            if line.rstrip("\n") == self.mark:
                return
            yield line
        raise RuntimeError("psql exited (%s) on: %s" % (self.psql.wait(), " ".join(sql.split())[:300]))

    def query(self, sql):
        """The rows of one statement as dicts of text, None for NULL."""
        rows = self.stream(sql)
        header = next(rows, None)
        return [dict(zip(header, r)) for r in rows] if header else []

    def close(self):
        if self.psql.poll() is None:
            self.psql.stdin.write("ROLLBACK;\n")
            self.psql.stdin.close()
            self.psql.wait()


def dec(v):
    return Decimal(v) if v is not None else None


def num_array(text):
    """A numeric[] as psql prints it: None for NULL, [] for {}, a NULL element None."""
    if text is None:
        return None
    body = text.strip()[1:-1]
    return [None if x == "NULL" else Decimal(x) for x in body.split(",")] if body else []


def lit(v):
    """A SQL literal of a number (or NULL) as numeric."""
    return "NULL::numeric" if v is None else "%s::numeric" % format(Decimal(v), "f")


def int_lit(v):
    """A SQL literal of a whole number (or NULL) as integer: the functions' p_n."""
    return "NULL::integer" if v is None else "(%d)::integer" % v


def bool_lit(v):
    return "NULL::boolean" if v is None else ("true" if v else "false")


def array_lit(levels, i):
    """A numeric[] literal of one column (0 price, 1 size) of a side."""
    if levels is None:
        return "NULL::numeric[]"
    return "'{%s}'::numeric[]" % ",".join("NULL" if x[i] is None else format(x[i], "f") for x in levels)


SAFE = re.compile(r"^[A-Za-z0-9_.-]+$")


def quoted(text):
    if not SAFE.match(text):
        raise SystemExit("refusing an unusual identifier: %r" % text)
    return "'%s'" % text


class MissingFunction(Exception):
    pass


class Database:
    """Reads one combo on one venue into a Market and calls the functions under check, all in one snapshot. Tables and
    the 3.1 functions are read from `schema`, the new functions called in `functions`."""

    def __init__(self, snap, schema, functions, combo, venue):
        for name in (schema, functions):
            if not SAFE.match(name):
                raise SystemExit("refusing an unusual schema name: %r" % name)
        self.snap, self.s, self.fs, self.combo, self.venue = snap, schema, functions, combo, venue
        found = snap.query("SELECT bull_symbol, bear_symbol, ref_symbol FROM %s.combo_market WHERE combo_id = %s AND venue = %s"
                           % (schema, quoted(combo), quoted(venue)))
        if not found:
            raise SystemExit("no market %s on %s" % (combo, venue))
        self.symbols = {"bull": found[0]["bull_symbol"], "bear": found[0]["bear_symbol"], "ref": found[0]["ref_symbol"] or None}
        segments = [Segment(int(r["from_ms"]), int(r["to_ms"]) if r["to_ms"] else None, r["session_date"], dec(r["a_bull"]),
                            dec(r["a_bear"]), dec(r["a_ref"]), r["gap_bull"] == "t", r["gap_bear"] == "t", r["gap_ref"] == "t",
                            r["past_t"] == "t")
                    for r in snap.query("SELECT session_date, from_ms, to_ms, a_bull, a_bear, a_ref, gap_bull, gap_bear, gap_ref, past_t "
                                        "FROM %s.combo_anchors(%s, %s) ORDER BY from_ms" % (schema, quoted(combo), quoted(venue)))]
        splits = {}
        for r in snap.query("SELECT leg, session_date, effective_ms FROM %s.combo_actions(%s, %s) WHERE kind = 'split' "
                            "AND status = 'Known' AND effective_ms IS NOT NULL" % (schema, quoted(combo), quoted(venue))):
            splits.setdefault(r["leg"], []).append((r["session_date"], int(r["effective_ms"])))
        statuses = {r["session_date"]: r["status"] for r in snap.query("SELECT session_date, status FROM %s.combo_sessions(%s)"
                                                                         % (schema, quoted(combo)))}
        self.market = Market(segments, splits=splits, statuses=statuses)
        self.functions = {r["proname"] for r in snap.query(
            "SELECT p.proname FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname = '%s'" % functions)}

    def load_seconds(self, legs, start, stop, step):
        """perp_second rows at T = start, start + step, ..., <= stop of the legs, by primary key."""
        symbols = {self.symbols[leg]: leg for leg in legs if self.symbols.get(leg)}
        if start > stop or not symbols:
            return
        for r in self.snap.query(
                "SELECT p.symbol, p.second_ms, p.price, p.trade_ms, p.bid_px, p.bid_sz, p.ask_px, p.ask_sz, p.book_ms "
                "FROM generate_series(%d::bigint, %d::bigint, %d::bigint) t(ms) JOIN %s.perp_second p ON p.venue = %s "
                "AND p.symbol IN (%s) AND p.second_ms = t.ms" % (start, stop, step, self.s, quoted(self.venue),
                                                                ", ".join(quoted(x) for x in symbols))):
            self.market.seconds.setdefault(symbols[r["symbol"]], {})[int(r["second_ms"])] = Second(
                dec(r["price"]), int(r["trade_ms"]) if r["trade_ms"] else None,
                levels_of(num_array(r["bid_px"]), num_array(r["bid_sz"])), levels_of(num_array(r["ask_px"]), num_array(r["ask_sz"])),
                int(r["book_ms"]) if r["book_ms"] else None)

    def load_fx(self, start, stop):
        """USD/USDT of [start - 5 min, stop]."""
        for r in self.snap.query("SELECT minute_ms, price FROM %s.fx_minute WHERE pair = 'USDT-USD' AND minute_ms >= %d AND minute_ms <= %d"
                                 % (self.s, start - 5 * MINUTE, stop)):
            self.market.fx[int(r["minute_ms"])] = Decimal(r["price"])

    def load_candles(self, start, stop):
        """The legs' candle closes of [start, stop)."""
        legs = {self.symbols[leg]: leg for leg in LEGS}
        for r in self.snap.query("SELECT symbol, minute_ms, price FROM %s.perp_minute WHERE venue = %s AND symbol IN (%s) "
                                 "AND minute_ms >= %d AND minute_ms < %d" % (self.s, quoted(self.venue),
                                                                            ", ".join(quoted(x) for x in legs), start, stop)):
            self.market.candles.setdefault(legs[r["symbol"]], {})[int(r["minute_ms"])] = Decimal(r["price"])

    def load_phases(self, start, stop):
        rows = self.snap.query("SELECT start_ms, end_ms, phase FROM %s.etf_phase WHERE end_ms > %d AND start_ms < %d"
                               % (self.s, start, stop))
        self.market.set_phases(set(self.market.phases) | {(int(r["start_ms"]), int(r["end_ms"]), r["phase"]) for r in rows})

    def load_for_points(self, from_ms, to_ms, step, legs=LEGS, bars=True):
        """What combo_range (bars=True) or combo_prices (and combo_range without a mean) reads for (from, to] at the step:
        the points' rows and F from the range's first minute; for the bars, from where they start (Market.lookback, never
        before the first close), the candles and the rows T = j + 60 s. Only the points' rows are read by enumerating
        instants, at most one per step; the minutes' tables are read by range, so only the rows that exist come back."""
        ts = multiples(from_ms, to_ms, step)
        if not ts:
            return
        self.load_seconds(legs, ts[0], ts[-1], step)
        last_minute = minute_of(ts[-1])
        first_minute = from_ms // MINUTE * MINUTE
        start = self.market.lookback(first_minute) if bars else first_minute
        self.load_fx(min(start, first_minute), last_minute + MINUTE)
        if bars:
            self.load_candles(start, last_minute + MINUTE)
            self.load_seconds(LEGS, start + MINUTE, last_minute, MINUTE)  # the bar rows T = j + 60 s, j < last_minute
        self.load_phases(from_ms, to_ms)

    def coverage_rows(self, symbol, from_ms, to_ms):
        """Each perp_second row of (from, to] as (second_ms, has a price, has a best bid and a best ask, levels a side)."""
        rows = self.snap.stream(
            "SELECT second_ms, price IS NOT NULL, bid_px[1] IS NOT NULL AND ask_px[1] IS NOT NULL, cardinality(bid_px), "
            "cardinality(ask_px) FROM %s.perp_second WHERE venue = %s AND symbol = %s AND second_ms > %d AND second_ms <= %d"
            % (self.s, quoted(self.venue), quoted(symbol), from_ms, to_ms))
        next(rows, None)
        for ts, price, book, n_bid, n_ask in rows:
            yield int(ts), price == "t", book == "t", int(n_bid) if n_bid else None, int(n_ask) if n_ask else None

    def call(self, function, args, order="ts_ms"):
        if function not in self.functions:
            raise MissingFunction(function)
        return self.snap.query("SELECT * FROM %s.%s(%s)%s" % (self.fs, function, ", ".join(args), " ORDER BY " + order if order else ""))

    def range_args(self, ma, n, from_ms, to_ms, step, notional, entry):
        return [quoted(self.combo), quoted(self.venue), quoted(ma), int_lit(n), "%d::bigint" % from_ms, "%d::bigint" % to_ms,
                "%d::bigint" % step, lit(notional), lit(entry)]


# ---------------------------------------------------------------------------------------------------------------
# Comparison
# ---------------------------------------------------------------------------------------------------------------

KIND = {"ts_ms": "int", "phase_sort": "int", "session_date": "text", "phase": "text", "basis": "text", "signal": "text",
        "kind": "text", "anchor_status": "text", "past_t": "bool", "partial_long": "bool", "partial_short": "bool", "a_ok": "bool",
        "book_bull": "bool", "book_bear": "bool", "s_bps": "num", "mu_bps": "num", "s_long_bps": "num", "s_short_bps": "num",
        "filled_long": "num", "filled_short": "num", "fx": "exact", "p_bull": "exact", "p_bear": "exact", "p_ref": "exact",
        "a_bull": "exact", "a_bear": "exact", "a_ref": "exact", "bid12_bull": "exact", "bid12_bear": "exact", "ask12_bull": "exact",
        "ask12_bear": "exact", "bid12": "exact", "ask12": "exact", "seconds": "int", "books": "int", "least_levels": "int",
        "most_levels": "int", "best_long": "num", "best_short": "num"}   # Entry Points' Best columns are bps: within 1e-9
# A database value where this side has NULL because the design does not say (see the docstring): reported, not failed.
OPEN_IN_DESIGN = {"partial_long": "s_long_bps", "partial_short": "s_short_bps", "filled_long": "s_long_bps",
                  "filled_short": "s_short_bps"}


def parse(column, text):
    kind = KIND.get(column, "text")
    if text is None:
        return None
    if kind == "int":
        return int(text)
    if kind == "bool":
        return text == "t"
    if kind in ("num", "exact"):
        return Decimal(text)
    return text


def same(column, a, b):
    if a is None or b is None:
        return a is None and b is None
    if KIND.get(column) == "num":
        return abs(a - b) <= TOLERANCE
    return a == b


def when(ms):
    t = datetime.fromtimestamp(ms / 1000, UTC8)
    return t.strftime("%m-%d %H:%M:%S") + (".%03d" % (ms % 1000) if ms % 1000 else "")


def show_value(v):
    if isinstance(v, Decimal):
        return format(v.normalize() if v == v.to_integral_value() else v, "f") if abs(v) < Decimal("1e15") else str(v)
    return "NULL" if v is None else str(v)


def short(v):
    return "%.6f" % v if isinstance(v, Decimal) else str(v)


def brief(r):
    keys = [k for k in ("basis", "s_bps", "mu_bps", "signal", "p_bull", "p_bear", "kind") if r.get(k) is not None]
    return " ".join("%s=%s" % (k, short(r[k])) for k in keys) or "(break row)"


def parsed(rows):
    return [{c: parse(c, v) for c, v in r.items()} for r in rows]


class Report:
    def __init__(self, show):
        self.failures = self.open = 0
        self.show = show

    def fail(self, count):
        self.failures += count

    def columns(self, label, theirs, columns):
        """The database rows must have the design's columns: a missing one fails every row."""
        if not theirs:
            return
        missing = [c for c in columns if c not in theirs[0]]
        if missing:
            print("%s: the database rows lack %s" % (label, ", ".join(missing)))
            self.failures += len(missing) * len(theirs)

    def compare(self, label, mine, theirs, columns, key="ts_ms", defined=None):
        """Row sets by key, then each column of each common row. defined(row) -> the columns compared on that row."""
        a = {r[key]: r for r in mine}
        b = {r[key]: r for r in theirs}
        only_mine, only_theirs = sorted(set(a) - set(b), key=str), sorted(set(b) - set(a), key=str)
        diffs, opened = {}, {}
        for k in sorted(set(a) & set(b), key=str):
            for c in (defined(a[k]) if defined else columns):
                if c == key or same(c, a[k].get(c), b[k].get(c)):
                    continue
                if c in OPEN_IN_DESIGN and a[k].get(c) is None and a[k].get(OPEN_IN_DESIGN[c]) is None and b[k].get(OPEN_IN_DESIGN[c]) is None:
                    opened.setdefault(c, []).append(k)
                    continue
                diffs.setdefault(c, []).append(k)
        bad = len(only_mine) + len(only_theirs) + sum(len(v) for v in diffs.values())
        print("%s: %d rows here, %d in the database, %d only here, %d only there, %d differing cells%s"
              % (label, len(a), len(b), len(only_mine), len(only_theirs), sum(len(v) for v in diffs.values()),
                 "" if not opened else ", %d cells open in the design (%s)" % (sum(len(v) for v in opened.values()), ", ".join(sorted(opened)))))
        fmt = when if key == "ts_ms" else str
        for k in only_mine[:self.show]:
            print("   only here   %s %s" % (fmt(k), brief(a[k])))
        for k in only_theirs[:self.show]:
            print("   only there  %s %s" % (fmt(k), brief(b[k])))
        for c, keys in sorted(diffs.items()):
            print("   column %-14s %d differ" % (c, len(keys)))
            for k in keys[:self.show]:
                print("      %s here %s, there %s" % (fmt(k), show_value(a[k].get(c)), show_value(b[k].get(c))))
        for c, keys in sorted(opened.items()):
            print("   open in the design: %s at %d rows where the real spread is NULL, e.g. %s here NULL, there %s"
                  % (c, len(keys), fmt(keys[0]), show_value(b[keys[0]].get(c))))
        self.failures += bad
        self.open += sum(len(v) for v in opened.values())
        return bad


# ---------------------------------------------------------------------------------------------------------------
# The checks
# ---------------------------------------------------------------------------------------------------------------


def depth_codes(rows, leg_side):
    """How a 1+2 Depth reads on the points (1.7): a value, Not Recorded (the leg has a usable book whose levels end
    within the second bucket) or No Book."""
    leg = leg_side.split("_")[1]
    c = Counter()
    for r in rows:
        if r.get("basis") is not None:
            c["value" if r.get(leg_side) is not None else "Not Recorded" if r.get("book_" + leg) else "No Book"] += 1
    return c


NOT_READY = (None, "No Anchor", "Past T", "No Mean", "No Notional", "No Entry", "No Book")   # Signals before Long / Short


def off_signal(rows, entry):
    """Section 1.5's count: mid points (with a mean, not Past T) whose real deviation |Real - Mean| reaches Entry on the
    side that is not a signal (Real Long Spread at least Entry above the mean, Real Short at least Entry below); none
    can signal without a valid Entry (No Entry)."""
    ready = [r for r in rows if r.get("signal") not in NOT_READY and r.get("mu_bps") is not None]
    if entry is None or not entry > 0:
        return 0, 0, len(ready)
    long = sum(1 for r in ready if r.get("s_long_bps") is not None and r["s_long_bps"] - r["mu_bps"] >= entry)
    short = sum(1 for r in ready if r.get("s_short_bps") is not None and r["s_short_bps"] - r["mu_bps"] <= -entry)
    return long, short, len(ready)


def ordering_violations(rows):
    """Points of the mid basis where Real Long Spread >= Spread >= Real Short Spread does not hold (1.2)."""
    return [r for r in rows if r.get("basis") == "mid" and None not in (r.get("s_bps"), r.get("s_long_bps"), r.get("s_short_bps"))
            and not r["s_long_bps"] >= r["s_bps"] >= r["s_short_bps"]]


def summary(rows, entry):
    points = [r for r in rows if r.get("basis") is not None]
    signals = Counter(r.get("signal") for r in points)
    depths = "; ".join("%s %s" % (c, ", ".join("%s %d" % kv for kv in sorted(depth_codes(rows, c).items()))) for c in DEPTH_COLUMNS)
    above, below, ready = off_signal(points, entry)
    return ("%d points (%d mid, %d trade), %d break rows, %d with a mean, %d Past T, %d long / %d short capped; Signal %s; "
            "1+2 Depth %s; of %d points that can signal, |Real Long Dev| >= Entry with Real Long above the mean at %d, "
            "|Real Short Dev| >= Entry with Real Short below it at %d (not signals)" % (
                len(points), sum(r["basis"] == "mid" for r in points), sum(r["basis"] == "trade" for r in points),
                len(rows) - len(points), sum(r.get("mu_bps") is not None for r in points), sum(bool(r.get("past_t")) for r in points),
                sum(bool(r.get("partial_long")) for r in points), sum(bool(r.get("partial_short")) for r in points),
                ", ".join("%s %d" % kv for kv in sorted(signals.items(), key=str)), depths, ready, above, below))


def print_rows(rows, columns, count):
    if count <= 0 or not rows:
        return
    picked = rows if len(rows) <= 2 * count else rows[:count] + [None] + rows[-count:]
    for r in picked:
        if r is None:
            print("   ...")
            continue
        print("   " + " ".join("%s=%s" % (c, when(r[c]) if c == "ts_ms" else short(r[c])) for c in columns if r.get(c) is not None))


def print_entries(label, table):
    print("%s Entry Points by Phase:" % label)
    for g in table:
        print("   %-11s long %5d short %5d partial %5d best long %s best short %s past T %d no book %d points %d"
              % (g["phase"], g["long_points"], g["short_points"], g["partial_points"],
                 "%.4f" % g["best_long"] if g["best_long"] is not None else "–", "%.4f" % g["best_short"] if g["best_short"] is not None else "–",
                 g["past_t_points"], g["no_book_points"], g["points"]))


def check_ordering(report, label, rows):
    bad = ordering_violations(rows)
    if bad:
        print("%s: Real Long >= Spread >= Real Short fails at %d mid points, e.g. %s" % (label, len(bad), when(bad[0]["ts_ms"])))
    report.fail(len(bad))


def check_range(db, report, args, ma, n, from_ms, to_ms, step, notional, entry):
    mine = combo_range(db.market, ma, n, from_ms, to_ms, step, notional, entry)
    label = "combo_range N %s d %s" % (show_value(notional), show_value(entry))
    print("%s here: %s" % (label, summary(mine, entry)))
    check_ordering(report, label + " here", mine)
    print_rows(mine, RANGE_COLUMNS, args.print)
    print_entries("here", entry_points(mine))
    if "mine" in args.checks:
        return mine
    raw = db.call("combo_range", db.range_args(ma, n, from_ms, to_ms, step, notional, entry))
    report.columns(label, raw, RANGE_COLUMNS)
    theirs = parsed(raw)
    print("%s there: %s" % (label, summary(theirs, entry)))
    check_ordering(report, label + " there", theirs)
    report.compare(label, mine, theirs, RANGE_COLUMNS)
    report.compare(label + " Entry Points", entry_points(mine), entry_points(theirs), ENTRY_COLUMNS, key="phase")
    write_dump(args.dump, "range_N%s_d%s" % (notional, entry), mine, theirs, RANGE_COLUMNS)
    return mine


def check_prices(db, report, args, from_ms, to_ms, step):
    mine = combo_prices(db.market, from_ms, to_ms, step)
    points = [r for r in mine if r["p_bull"] is not None]
    print("combo_prices here: %d points, %d break rows, %d without the reference" % (len(points), len(mine) - len(points),
                                                                                     sum(r["p_ref"] is None for r in points)))
    print_rows(mine, PRICE_COLUMNS, args.print)
    if "mine" in args.checks:
        return
    raw = db.call("combo_prices", [quoted(db.combo), quoted(db.venue), "%d::bigint" % from_ms, "%d::bigint" % to_ms, "%d::bigint" % step])
    report.columns("combo_prices", raw, PRICE_COLUMNS)
    theirs = parsed(raw)
    report.compare("combo_prices", mine, theirs, PRICE_COLUMNS)
    write_dump(args.dump, "prices", mine, theirs, PRICE_COLUMNS)


def check_last(db, report, args, ma, n, notional, entry, now_ms):
    mine = combo_last(db.market, ma, n, notional, entry, now_ms)
    label = "combo_last at %s N %s d %s" % (when(now_ms), show_value(notional), show_value(entry))
    for r in mine:
        print("%s here %s: %s" % (label, r["kind"], " ".join("%s=%s" % (c, when(r[c]) if c == "ts_ms" else short(r[c]))
                                                             for c in LAST_COLUMNS if r.get(c) is not None and c != "kind")))
    if "mine" in args.checks:
        return
    args_sql = [quoted(db.combo), quoted(db.venue), quoted(ma), int_lit(n), lit(notional), lit(entry), "%d::bigint" % now_ms]
    raw = db.call("combo_last", args_sql, order="kind")
    report.columns(label, raw, LAST_COLUMNS)
    theirs = parsed(raw)
    for r in theirs:
        extra = [c for c in LAST_COLUMNS if c not in LAST_DEFINED.get(r.get("kind"), ()) and c != "kind" and r.get(c) is not None]
        if extra:
            print("   there %s also gives (not compared): %s" % (r.get("kind"), ", ".join("%s=%s" % (c, short(r[c])) for c in extra)))
    report.compare(label, mine, theirs, LAST_COLUMNS, key="kind", defined=lambda r: LAST_DEFINED[r["kind"]])
    write_dump(args.dump, "last_N%s_d%s" % (notional, entry), mine, theirs, LAST_COLUMNS)


def chunks(items, size=400):
    for i in range(0, len(items), size):
        yield items[i:i + size]


def check_books(db, report, rows, notional):
    """book_pair on the recorded books of every mid point of the range, with the point's anchors and F, and top2_size
    on each leg's usable book of every point, against this side (book_pair's fills too, which combo_range does not
    return)."""
    m = db.market
    for f in ("book_pair", "top2_size"):
        if f not in db.functions:
            raise MissingFunction(f)
    mine, theirs = [], []
    mids = sorted(r["ts_ms"] for r in rows if r.get("basis") == "mid")
    for ts in mids:
        minute = minute_of(ts)
        seg = m.segment(minute)
        b, r = m.book("bull", ts, seg, minute), m.book("bear", ts, seg, minute)
        mine.append(dict(book_pair(b.bids, b.asks, r.bids, r.asks, seg.a_bull if seg else None, seg.a_bear if seg else None,
                                   m.fx_at(minute), notional), ts_ms=ts))
    for batch in chunks(mids):
        values = ", ".join("(%d::bigint, %s, %s, %s)" % (ts, lit(m.segment(minute_of(ts)).a_bull if m.segment(minute_of(ts)) else None),
                                                          lit(m.segment(minute_of(ts)).a_bear if m.segment(minute_of(ts)) else None),
                                                          lit(m.fx_at(minute_of(ts)))) for ts in batch)
        theirs += db.snap.query(
            "SELECT v.ts_ms, x.* FROM (VALUES %s) v(ts_ms, a_bull, a_bear, fx) "
            "JOIN %s.perp_second b ON b.venue = %s AND b.symbol = %s AND b.second_ms = v.ts_ms "
            "JOIN %s.perp_second r ON r.venue = %s AND r.symbol = %s AND r.second_ms = v.ts_ms "
            "CROSS JOIN LATERAL %s.book_pair(b.bid_px, b.bid_sz, b.ask_px, b.ask_sz, r.bid_px, r.bid_sz, r.ask_px, r.ask_sz, "
            "v.a_bull, v.a_bear, v.fx, %s) x" % (values, db.s, quoted(db.venue), quoted(db.symbols["bull"]), db.s, quoted(db.venue),
                                                quoted(db.symbols["bear"]), db.fs, lit(notional)))
    report.columns("book_pair", theirs, BOOK_COLUMNS)
    report.compare("book_pair on %d mid points, N %s" % (len(mids), show_value(notional)), mine, parsed(theirs), ("ts_ms",) + BOOK_COLUMNS)
    for leg in LEGS:
        booked = []
        for r in rows:
            if r.get("basis") is None:
                continue
            minute = minute_of(r["ts_ms"])
            row = m.book(leg, r["ts_ms"], m.segment(minute), minute)
            if row is not None:
                booked.append({"ts_ms": r["ts_ms"], "bid12": top2_size(priced(row.bids), True), "ask12": top2_size(priced(row.asks), False)})
        got = []
        for batch in chunks([x["ts_ms"] for x in booked]):
            got += db.snap.query(
                "SELECT v.ts_ms, %s.top2_size(p.bid_px, p.bid_sz, true) AS bid12, %s.top2_size(p.ask_px, p.ask_sz, false) AS ask12 "
                "FROM (VALUES %s) v(ts_ms) JOIN %s.perp_second p ON p.venue = %s AND p.symbol = %s AND p.second_ms = v.ts_ms"
                % (db.fs, db.fs, ", ".join("(%d::bigint)" % ts for ts in batch), db.s, quoted(db.venue), quoted(db.symbols[leg])))
        report.compare("top2_size %s on %d points with its usable book" % (leg, len(booked)), booked,
                       [{"ts_ms": int(g["ts_ms"]), "bid12": dec(g["bid12"]), "ask12": dec(g["ask12"])} for g in got],
                       ("ts_ms", "bid12", "ask12"))


def check_vectors(db, report):
    """The hand-made books, sides and Signal cases through book_pair, top2_size and combo_signal."""
    for f in ("book_pair", "top2_size", "combo_signal"):
        if f not in db.functions:
            raise MissingFunction(f)
    mine, sql = [], []
    for i, (name, v) in enumerate(BOOK_VECTORS):
        mine.append(dict(book_pair(v["bids_bull"], v["asks_bull"], v["bids_bear"], v["asks_bear"], v["a_bull"], v["a_bear"], v["fx"],
                                   v["notional"]), ts_ms=i))
        sql.append("(%d, %s)" % (i, ", ".join([array_lit(v["bids_bull"], 0), array_lit(v["bids_bull"], 1), array_lit(v["asks_bull"], 0),
                                                array_lit(v["asks_bull"], 1), array_lit(v["bids_bear"], 0), array_lit(v["bids_bear"], 1),
                                                array_lit(v["asks_bear"], 0), array_lit(v["asks_bear"], 1), lit(v["a_bull"]),
                                                lit(v["a_bear"]), lit(v["fx"]), lit(v["notional"])])))
    theirs = parsed(db.snap.query(
        "SELECT v.ts_ms, x.* FROM (VALUES %s) v(ts_ms, bpb, bsb, apb, asb, bpr, bsr, apr, asr, a_bull, a_bear, fx, n) CROSS JOIN LATERAL "
        "%s.book_pair(v.bpb, v.bsb, v.apb, v.asb, v.bpr, v.bsr, v.apr, v.asr, v.a_bull, v.a_bear, v.fx, v.n) x ORDER BY 1"
        % (", ".join(sql), db.fs)))
    report.compare("book_pair hand-made cases (%s)" % ", ".join("%d %s" % (i, n) for i, (n, _) in enumerate(BOOK_VECTORS)),
                   mine, theirs, ("ts_ms",) + BOOK_COLUMNS)
    mine = [{"ts_ms": i, "bid12": top2_size(_side(*zip(px, sz)), bids)} for i, (_, px, sz, bids) in enumerate(TOP2_VECTORS)]
    got = db.snap.query("SELECT v.i, %s.top2_size(v.px, v.sz, v.bids) AS t FROM (VALUES %s) v(i, px, sz, bids) ORDER BY 1" % (
        db.fs, ", ".join("(%d, %s, %s, %s)" % (i, array_lit(_side(*zip(px, sz)), 0), array_lit(_side(*zip(px, sz)), 1), bool_lit(bids))
                         for i, (_, px, sz, bids) in enumerate(TOP2_VECTORS))))
    report.compare("top2_size hand-made cases", mine, [{"ts_ms": int(g["i"]), "bid12": dec(g["t"])} for g in got], ("ts_ms", "bid12"))
    rows = []
    for i, (case, _) in enumerate(SIGNAL_VECTORS):
        s, past, mu, n, mid, sl, ss, pl, ps, d = signal_case(case)
        rows.append("(%d, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s)" % (i, lit(s), bool_lit(past), lit(mu), lit(n), bool_lit(mid), lit(sl),
                                                                    lit(ss), bool_lit(pl), bool_lit(ps), lit(d)))
    got = db.snap.query("SELECT v.i, %s.combo_signal(v.s, v.past, v.mu, v.n, v.mid, v.sl, v.ss, v.pl, v.ps, v.d) AS signal "
                        "FROM (VALUES %s) v(i, s, past, mu, n, mid, sl, ss, pl, ps, d) ORDER BY 1" % (db.fs, ", ".join(rows)))
    mine_sig = [{"ts_ms": i, "signal": signal_of(*signal_case(case))} for i, (case, _) in enumerate(SIGNAL_VECTORS)]
    report.compare("combo_signal hand-made cases", mine_sig, [{"ts_ms": int(r["i"]), "signal": r["signal"]} for r in got], ("ts_ms", "signal"))


def check_coverage(db, report, args, from_ms, to_ms, now_ms):
    """perp_coverage of the combo's contracts over (from, to] against the counts of their perp_second rows, made here
    by minute (and checked against 3.1's Data Coverage query); and every row of perp_second_minute for a whole minute
    inside the range that its watermark (minute_ms + 70 s <= refreshed_ms) declares final, against that minute's, where
    the board reads it: minute_ms + 66 min <= now_ms, the snapshot's now() (FIX_R2 A3; the newer minutes are skipped)."""
    columns = COUNT_COLUMNS
    for leg in ("bull", "bear", "ref"):
        symbol = db.symbols.get(leg)
        if not symbol:
            continue
        per_minute = minute_counts(db.coverage_rows(symbol, from_ms, to_ms))
        mine = coverage(per_minute.values())
        print("coverage %s %s (%s, %s]: here seconds %d, books %d, levels %s-%s" % (db.venue, symbol, stamp(from_ms), stamp(to_ms), *mine))
        old = db.snap.query(
            "SELECT count(price) AS seconds, count(*) FILTER (WHERE bid_px[1] IS NOT NULL AND ask_px[1] IS NOT NULL) AS books, "
            "min(least(cardinality(bid_px), cardinality(ask_px))) FILTER (WHERE bid_px[1] IS NOT NULL AND ask_px[1] IS NOT NULL) AS least_levels, "
            "max(greatest(cardinality(bid_px), cardinality(ask_px))) FILTER (WHERE bid_px[1] IS NOT NULL AND ask_px[1] IS NOT NULL) AS most_levels "
            "FROM %s.perp_second WHERE venue = %s AND symbol = %s AND second_ms > %d AND second_ms <= %d"
            % (db.s, quoted(db.venue), quoted(symbol), from_ms, to_ms))[0]
        if tuple(parse(c, old[c]) for c in columns) != mine:
            print("   3.1's Data Coverage query gives %s" % [old[c] for c in columns])
            report.fail(1)
        if "mine" in args.checks:
            continue
        theirs = db.call("perp_coverage", [quoted(db.venue), quoted(symbol), "%d::bigint" % from_ms, "%d::bigint" % to_ms], order=None)
        report.columns("perp_coverage", theirs, columns)
        report.compare("perp_coverage %s %s" % (db.venue, symbol), [dict(zip(("ts_ms",) + columns, (0,) + mine))],
                       [dict({c: parse(c, r.get(c)) for c in columns}, ts_ms=0) for r in theirs], ("ts_ms",) + columns)
        if not db.snap.query("SELECT 1 WHERE to_regclass('%s.perp_second_minute') IS NOT NULL" % db.s):
            print("   no table %s.perp_second_minute" % db.s)
            continue
        lo, hi = (from_ms + MINUTE - 1) // MINUTE * MINUTE, to_ms - MINUTE  # whole minutes: from <= m and m + 60 s <= to
        table = {int(r["minute_ms"]): r for r in db.snap.query(
            "SELECT minute_ms, seconds, books, least_levels, most_levels, refreshed_ms FROM %s.perp_second_minute WHERE venue = %s "
            "AND symbol = %s AND minute_ms >= %d AND minute_ms <= %d" % (db.s, quoted(db.venue), quoted(symbol), lo, hi))}
        t = table_check(per_minute, table, lo, hi, now_ms)
        print("   perp_second_minute: %d whole minutes in the range, %d rows, %d final by the watermark, %d of them differ from "
              "perp_second; %d minutes with rows here but no table row (the minutes with minute + 66 min <= now %s; the %d newer "
              "whole minutes skipped, %d final rows among them)"
              % (t.whole, t.rows, t.final, len(t.differ), t.missing, when(now_ms), t.skipped, t.skipped_final))
        for m in t.differ[:args.print or 3]:
            print("      %s table %s, perp_second %s" % (when(m), [table[m][c] for c in columns], per_minute.get(m)))
        report.fail(len(t.differ))
        check_end_rows(db, report, args, symbol, lo, hi, now_ms)


def check_end_rows(db, report, args, symbol, lo, hi, now_ms):
    """The end-row columns of every final perp_second_minute row for the whole minutes lo..hi (what the minute bars
    read) against the row of perp_second at the minute's end, made here (end_values), where the board reads them:
    minute_ms + 66 min <= now_ms (the newer final rows are skipped)."""
    if not db.snap.query("SELECT 1 FROM pg_attribute a WHERE a.attrelid = to_regclass('%s.perp_second_minute') AND a.attname = 'end_mid' "
                         "AND NOT a.attisdropped" % db.s):
        print("   %s.perp_second_minute has no end-row columns" % db.s)
        return
    final = {int(r["minute_ms"]): r for r in db.snap.query(
        "SELECT minute_ms, end_price, end_trade_ms, end_mid, end_book_ms FROM %s.perp_second_minute WHERE venue = %s AND symbol = %s "
        "AND minute_ms >= %d AND minute_ms <= %d AND minute_ms + 70000 <= refreshed_ms" % (db.s, quoted(db.venue), quoted(symbol), lo, hi))}
    ends = {int(r["second_ms"]) - MINUTE: r for r in db.snap.query(
        "SELECT second_ms, price, trade_ms, bid_px, bid_sz, ask_px, ask_sz, book_ms FROM %s.perp_second WHERE venue = %s AND symbol = %s "
        "AND second_ms %% 60000 = 0 AND second_ms > %d AND second_ms <= %d" % (db.s, quoted(db.venue), quoted(symbol), lo, hi + MINUTE))}
    wrong, compared, with_end, skipped = end_row_check(final, ends, now_ms)
    print("   perp_second_minute end rows: %d final rows, %d with an end row here, %d differ; %d final rows newer than now - 66 min "
          "skipped" % (compared, with_end, len(wrong), skipped))
    for m, got, want in wrong[:args.print or 3]:
        print("      %s table %s, perp_second %s" % (when(m), got, want))
    report.fail(len(wrong))


def write_dump(directory, name, mine, theirs, columns):
    if not directory:
        return
    os.makedirs(directory, exist_ok=True)
    for side, rows in (("here", mine), ("there", theirs)):
        with open(os.path.join(directory, "%s.%s.csv" % (name, side)), "w", newline="") as f:
            w = csv.writer(f)
            w.writerow(columns)
            for r in rows:
                w.writerow(["" if r.get(c) is None else r.get(c) for c in columns])


# ---------------------------------------------------------------------------------------------------------------
# Command line
# ---------------------------------------------------------------------------------------------------------------

CHECKS = {"range", "prices", "last", "coverage", "books", "vectors", "mine"}


def instant(text):
    if text is None:
        return None
    if text.isdigit():
        return int(text)
    for fmt in ("%Y-%m-%d %H:%M:%S", "%Y-%m-%d %H:%M", "%Y-%m-%dT%H:%M:%S", "%Y-%m-%dT%H:%M"):
        try:
            return int(datetime.strptime(text, fmt).replace(tzinfo=UTC8).timestamp() * 1000)
        except ValueError:
            pass
    raise SystemExit("not a time: %r (UTC+8 'YYYY-MM-DD HH:MM[:SS]' or Unix ms)" % text)


def span(text):
    m = re.fullmatch(r"(\d+)([smhd])", text)
    if not m:
        raise SystemExit("not a range: %r (e.g. 90m, 1h, 7d)" % text)
    return int(m.group(1)) * {"s": 1000, "m": MINUTE, "h": 3_600_000, "d": 86_400_000}[m.group(2)]


def numbers(text):
    return [None if x.strip().lower() == "null" else Decimal(x.strip()) for x in text.split(",")]


def minutes_arg(text):
    """--n: a whole number of minutes (0 and below accepted: no mean) or null."""
    return None if text.strip().lower() == "null" else int(text)


def stamp(ms):
    """An instant with its year, UTC+8 (the ranges of a year or two)."""
    return datetime.fromtimestamp(ms / 1000, UTC8).strftime("%Y-%m-%d %H:%M:%S") + (".%03d" % (ms % 1000) if ms % 1000 else "")


def parse_args(argv):
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0], formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--dsn", required=True, help="libpq connection string (never printed); a throwaway copy or the reader role")
    ap.add_argument("--schema", default="etfpair", help="schema of the tables and of combo_anchors, combo_actions, combo_sessions")
    ap.add_argument("--functions-schema", help="schema of the new 3.2 functions (default --schema)")
    ap.add_argument("--combo", required=True)
    ap.add_argument("--venue", required=True)
    ap.add_argument("--ma", choices=("sma", "ema"), required=True)
    ap.add_argument("--n", type=minutes_arg, required=True, help="minutes of the mean; null, 0 and below give no mean")
    ap.add_argument("--range", default="1h")
    ap.add_argument("--from", dest="from_")
    ap.add_argument("--to")
    ap.add_argument("--step", type=int)
    ap.add_argument("--notional", default="10000")
    ap.add_argument("--entry", default="2")
    ap.add_argument("--now")
    ap.add_argument("--checks", default="range,prices,last,coverage")
    ap.add_argument("--print", type=int, default=0)
    ap.add_argument("--dump")
    args = ap.parse_args(argv)
    args.checks = set(args.checks.split(","))
    if args.checks - CHECKS:
        raise SystemExit("unknown checks: %s" % ", ".join(sorted(args.checks - CHECKS)))
    if args.step is not None and args.step < 1:
        raise SystemExit("--step must be at least 1")
    args.functions_schema = args.functions_schema or args.schema
    return args


def main(argv=None):
    args = parse_args(argv)
    snap = Snapshot(args.dsn)
    report = Report(args.print or 3)
    try:
        info = snap.query("SELECT floor(extract(epoch FROM now()) * 1000)::bigint AS now_ms, current_setting('transaction_isolation') AS iso, "
                          "current_setting('transaction_read_only') AS ro, current_user AS who")[0]
        db_now_ms = int(info["now_ms"])   # now() of the snapshot: the 66-minute rule of perp_second_minute (FIX_R2 A3)
        to_ms = instant(args.to) if args.to else db_now_ms // 1000 * 1000
        from_ms = instant(args.from_) if args.from_ else to_ms - span(args.range)
        step = args.step or board_step(to_ms - from_ms)
        now_ms = instant(args.now) if args.now else to_ms
        with_mean = mean_minutes_valid(args.n)
        print("snapshot: one %s transaction (read only %s) as %s at %s UTC+8" % (info["iso"], info["ro"], info["who"], stamp(db_now_ms)))
        print("%s %s %s %s%s, (%s, %s] UTC+8, step %d s, notional %s, entry %s, now %s, tables %s, functions %s"
              % (args.combo, args.venue, args.ma, "null" if args.n is None else args.n, "" if with_mean else " (no mean)", stamp(from_ms),
                 stamp(to_ms), step // 1000, args.notional, args.entry, stamp(now_ms), args.schema, args.functions_schema))
        db = Database(snap, args.schema, args.functions_schema, args.combo, args.venue)
        points_needed = args.checks & {"range", "books"}
        if points_needed:
            db.load_for_points(from_ms, to_ms, step, bars=with_mean)
        if "prices" in args.checks:
            db.load_for_points(from_ms, to_ms, step, legs=("bull", "bear", "ref"), bars=False)
        if "last" in args.checks:
            db.load_for_points(now_ms - LAST_WINDOW_MS, now_ms, 1000, legs=("bull", "bear", "ref"), bars=with_mean)
        notionals, entries = numbers(args.notional), numbers(args.entry)
        try:
            if "vectors" in args.checks and "mine" not in args.checks:
                check_vectors(db, report)
            for notional in notionals:
                for i, entry in enumerate(entries):
                    rows = None
                    if "range" in args.checks:
                        rows = check_range(db, report, args, args.ma, args.n, from_ms, to_ms, step, notional, entry)
                    elif points_needed:
                        rows = combo_range(db.market, args.ma, args.n, from_ms, to_ms, step, notional, entry)
                    if i == 0 and "books" in args.checks and "mine" not in args.checks:  # the books do not depend on the entry
                        check_books(db, report, rows, notional)
                    if "last" in args.checks:
                        check_last(db, report, args, args.ma, args.n, notional, entry, now_ms)
            if "prices" in args.checks:
                check_prices(db, report, args, from_ms, to_ms, step)
            if "coverage" in args.checks:
                check_coverage(db, report, args, from_ms, to_ms, db_now_ms)
        except MissingFunction as e:
            print("MISSING: %s.%s does not exist in this database" % (args.functions_schema, e))
            return 2
        except RuntimeError as e:
            print("FAILED: %s" % e)
            return 2
    finally:
        snap.close()
    if "mine" in args.checks:
        print("this side only: %d differences" % report.failures)
        return 1 if report.failures else 0
    print("%s: %d differences%s" % ("FAILED" if report.failures else "all equal within %s" % TOLERANCE, report.failures,
                                     "" if not report.open else ", %d cells open in the design (not failed)" % report.open))
    return 1 if report.failures else 0


if __name__ == "__main__":
    sys.exit(main())
