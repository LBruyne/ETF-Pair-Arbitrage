#!/usr/bin/env python3
"""Unit tests of recompute32.py on hand-made cases (no database).

    python3 dashboard/audit/test_recompute32.py

Each expected number is worked out by hand from DESIGN.md (experiments/board32_20261122, third version) and FIX_R2.md
(A3, A7, B), or with exact fractions where noted, never by running the code under test.
"""
import argparse
import io
import os
import re
import sys
import unittest
from contextlib import redirect_stderr, redirect_stdout
from decimal import Decimal as D
from fractions import Fraction

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import recompute32 as rc  # noqa: E402

M = rc.MINUTE
LAST_WINDOW = rc.LAST_WINDOW_MS
T0 = 1_795_000_000_000 // M * M     # a whole minute, the start of the test session
S1, S2 = "2026-11-20", "2026-11-21"
A_BULL, A_BEAR = D(100), D(50)


def side(*pairs):
    return [(D(str(p)), D(str(s))) for p, s in pairs]


def row(mid=None, price=None, trade_ms=None, book_ms=None, size=10, levels=3):
    """A perp_second row: a usable 0.01-tick book with its best bid and ask 0.01 around mid, levels a side of size
    each, and / or a last trade."""
    bids = asks = None
    if mid is not None:
        mid = D(str(mid))
        bids = [(mid - D("0.01") * (i + 1), D(size)) for i in range(levels)]
        asks = [(mid + D("0.01") * (i + 1), D(size)) for i in range(levels)]
    return rc.Second(price=D(str(price)) if price is not None else None, trade_ms=trade_ms, bids=bids, asks=asks, book_ms=book_ms)


def market(bull, bear, ref=None, segments=None, fx=None, candles=None, phases=None, splits=None, statuses=None):
    segments = segments or [rc.Segment(T0, None, S1, A_BULL, A_BEAR, D(200))]
    fx = {T0 + k * M: D(1) for k in range(-10, 400)} if fx is None else fx
    seconds = {"bull": bull, "bear": bear}
    if ref is not None:
        seconds["ref"] = ref
    return rc.Market(segments, splits=splits, fx=fx, candles=candles, seconds=seconds, statuses=statuses, phases=phases)


def bar_rows(count, skip=(), trade_only=(), start=0):
    """Rows at T = j + 60 s for the minutes j = T0 + k min, k in [start, start + count): bull mid 100 + k, bear mid
    50 + k / 2 (trades of those prices instead for k in trade_only), none for k in skip."""
    bull, bear = {}, {}
    for k in range(start, start + count):
        if k in skip:
            continue
        p_bull, p_bear = D(100 + k), D(50) + D(k) / 2
        if k in trade_only:
            bull[T0 + (k + 1) * M], bear[T0 + (k + 1) * M] = row(price=p_bull), row(price=p_bear)
        else:
            bull[T0 + (k + 1) * M], bear[T0 + (k + 1) * M] = row(mid=p_bull), row(mid=p_bear)
    return bull, bear


def mu_at(m, ma, n, minute):
    means = rc.means_for(m, ma, n, minute, minute)
    return rc.mean_bps(means, m.segment(minute), minute)


def signal_market():
    """SMA3 of the bars 0-2 (101 / 50.5 on the anchors 100 / 50: mu 200) for the points of minute 3: at +1 s mids 100 / 50
    (s 0, Real Long about 3: a Long at Entry 2), at +2 s trades only (No Book at Entry 2), at +3 s mids 103 / 52 (s 700,
    Real Short about 697: a Short). Returns the market and the start t of minute 3; one phase, Regular."""
    bull, bear = bar_rows(3)
    t = T0 + 3 * M
    bull.update({t + 1000: row(mid=100), t + 2000: row(price=100), t + 3000: row(mid=103)})
    bear.update({t + 1000: row(mid=50), t + 2000: row(price=50), t + 3000: row(mid=52)})
    return market(bull, bear, phases=[(T0, T0 + 10 * M, "Regular")]), t


class FakeSnap:
    """Stands in for the psql session: records each query and answers it with the rows of the first pattern it contains."""

    def __init__(self, answers=()):
        self.answers, self.sql = list(answers), []

    def query(self, sql):
        self.sql.append(sql)
        return next((rows for pattern, rows in self.answers if pattern in sql), [])


def fake_db(snap, segments=(), symbols=None):
    """An rc.Database on a FakeSnap, without the queries of its constructor."""
    db = rc.Database.__new__(rc.Database)
    db.snap, db.s, db.fs, db.combo, db.venue = snap, "etfpair", "etfpair", "SOXL-SOXS", "bitget"
    db.symbols = symbols or {"bull": "SOXLUSDT", "bear": "SOXSUSDT", "ref": "SOXXUSDT"}
    db.market = rc.Market(list(segments))
    db.functions = {"perp_coverage"}
    return db


class UsableBook(unittest.TestCase):
    """Section 1.0: which books and trades of a leg's second count."""

    def test_usable_book(self):
        self.assertTrue(rc.book_usable(side((40.45, 1)), side((40.47, 1))))

    def test_empty_or_missing_side(self):
        self.assertFalse(rc.book_usable([], side((40.47, 1))))
        self.assertFalse(rc.book_usable(side((40.45, 1)), None))

    def test_first_level_without_a_price(self):
        self.assertFalse(rc.book_usable([(None, D(1)), (D("40.44"), D(1))], side((40.47, 1))))

    def test_crossed_and_locked_books_count_as_none(self):
        self.assertFalse(rc.book_usable(side((40.48, 1)), side((40.47, 1))))
        self.assertFalse(rc.book_usable(side((40.47, 1)), side((40.47, 1))))

    def test_a_priced_level_without_a_positive_size(self):
        self.assertFalse(rc.book_usable(side((40.45, 1), (40.44, 0)), side((40.47, 1))))
        self.assertFalse(rc.book_usable([(D("40.45"), D(1)), (D("40.44"), None)], side((40.47, 1))))

    def test_malformed_sides_are_no_book(self):
        # As the SQL's etfpair.book_usable: a size array longer than the prices, or a NULL price inside the levels.
        self.assertFalse(rc.book_usable(rc.levels_of([D("40.45")], [D(1), D(2)]), side((40.47, 1))))
        self.assertFalse(rc.book_usable(rc.levels_of([D("40.45"), None, D("40.43")], [D(1), D(1), D(1)]), side((40.47, 1))))
        self.assertFalse(rc.book_usable(rc.levels_of([D("40.45")], None), side((40.47, 1))))
        self.assertTrue(rc.book_usable(rc.levels_of([D("40.45"), D("40.44")], [D(1), D(2)]), side((40.47, 1))))

    def test_levels_of_pairs_prices_and_sizes(self):
        self.assertIsNone(rc.levels_of(None, [D(1)]))
        self.assertEqual(rc.levels_of([], []), [])
        self.assertEqual(rc.levels_of([D(1), D(2)], [D(5)]), [(D(1), D(5)), (D(2), None)])   # a level without a size
        self.assertEqual(rc.levels_of([D(1)], [D(5), D(6)]), [(D(1), D(5)), (None, D(6))])  # a size beyond the prices
        self.assertEqual(rc.num_array("{1.5,NULL,2}"), [D("1.5"), None, D(2)])
        self.assertEqual(rc.num_array("{}"), [])
        self.assertIsNone(rc.num_array(None))

    def test_halted_leg_has_neither_book_nor_trade(self):
        seg = rc.Segment(T0, None, S1, A_BULL, A_BEAR, gap_bull=True)
        m = market({T0 + 1000: row(mid=100, price=100)}, {T0 + 1000: row(mid=50, price=50)}, segments=[seg])
        self.assertIsNone(m.book("bull", T0 + 1000, seg, T0))
        self.assertIsNone(m.trade("bull", T0 + 1000, seg, T0))
        self.assertIsNotNone(m.book("bear", T0 + 1000, seg, T0))
        self.assertIsNone(rc.point_at(m, {}, T0 + 1000, D(1000), D(2)))

    def test_split_rule_rejects_the_frozen_book_and_trade(self):
        tv = T0 + 2 * M
        m = market({tv + 61000: row(mid=50, price=50, trade_ms=tv - 1, book_ms=tv - 1),
                    tv + 62000: row(mid=50, price=50, trade_ms=tv, book_ms=tv - 1),
                    tv + 63000: row(mid=50, price=50, trade_ms=None, book_ms=tv)},
                   {ts: row(mid=50, price=50) for ts in (tv + 61000, tv + 62000, tv + 63000)},
                   segments=[rc.Segment(T0, None, S1, D(50), A_BEAR)], splits={"bull": [(S1, tv)]})
        seg, minute = m.segment(tv + M), tv + M
        self.assertIsNone(m.book("bull", tv + 61000, seg, minute))
        self.assertIsNone(m.trade("bull", tv + 61000, seg, minute))
        self.assertIsNone(rc.point_at(m, {}, tv + 61000, D(1000), D(2)))       # neither counts: no point
        p = rc.point_at(m, {}, tv + 62000, D(1000), D(2))                     # the trade counts: trade basis
        self.assertEqual((p["basis"], p["book_bull"], p["bid12_bull"]), ("trade", False, None))
        self.assertEqual(rc.point_at(m, {}, tv + 63000, D(1000), D(2))["basis"], "mid")   # book_ms = T_v, NULL trade_ms count

    def test_split_rule_only_from_tv_in_the_carrier_session(self):
        tv = T0 + 2 * M
        m = market({}, {}, segments=[rc.Segment(T0, None, S1, D(50), A_BEAR)], splits={"bull": [(S1, tv)]})
        self.assertIsNone(m.tv("bull", m.segment(tv - M), tv - M))
        self.assertEqual(m.tv("bull", m.segment(tv), tv), tv)
        other = rc.Segment(T0, None, S2, D(50), A_BEAR)
        self.assertIsNone(m.tv("bull", other, tv + M))


class BasisAndSpread(unittest.TestCase):
    """Sections 1.1 and 1.2: the point's basis, in pairs, and the spread."""

    def test_mid_basis_and_spread(self):
        m = market({T0 + 1000: row(mid=101, price=99)}, {T0 + 1000: row(mid=51, price=49)})
        p = rc.point_at(m, {}, T0 + 1000, D(1000), D(2))
        self.assertEqual(p["basis"], "mid")
        self.assertEqual(p["s_bps"], D(300))        # 10^4 (101/100 + 51/50 - 2)
        self.assertTrue(p["a_ok"] and p["book_bull"] and p["book_bear"])

    def test_trade_basis_when_a_book_is_unusable(self):
        crossed = rc.Second(price=D(101), bids=side((101.02, 1)), asks=side((101.01, 1)))
        m = market({T0 + 1000: crossed}, {T0 + 1000: row(mid=51, price=D("50.5"))})
        p = rc.point_at(m, {}, T0 + 1000, D(1000), D(2))
        self.assertEqual(p["basis"], "trade")
        self.assertEqual(p["s_bps"], D(200))        # 10^4 (101/100 + 50.5/50 - 2)
        self.assertEqual((p["book_bull"], p["book_bear"]), (False, True))
        self.assertIsNone(p["s_long_bps"])
        self.assertEqual(p["signal"], "No Mean")

    def test_no_mixed_basis(self):
        m = market({T0 + 1000: row(mid=101)}, {T0 + 1000: row(price=51)})
        self.assertIsNone(rc.point_at(m, {}, T0 + 1000, D(1000), D(2)))

    def test_fx_is_the_minutes_own_or_the_newest_of_the_five_before(self):
        m = market({}, {}, fx={T0: D(2), T0 + 5 * M: D(3)})
        self.assertEqual(m.fx_at(T0 + 4 * M), D(2))
        self.assertEqual(m.fx_at(T0 + 5 * M), D(3))
        self.assertEqual(m.fx_at(T0 + 10 * M), D(3))
        self.assertIsNone(m.fx_at(T0 + 11 * M))

    def test_point_belongs_to_the_minute_of_t_minus_1_ms(self):
        segments = [rc.Segment(T0, T0 + 5 * M, S1, A_BULL, A_BEAR), rc.Segment(T0 + 5 * M, None, S1, D(99), A_BEAR)]
        ts = T0 + 5 * M
        m = market({ts: row(mid=99), ts + 1000: row(mid=99)}, {ts: row(mid=50), ts + 1000: row(mid=50)}, segments=segments)
        self.assertEqual(rc.point_at(m, {}, ts, None, None)["s_bps"], D(-100))     # 99 / 100: the old anchor
        self.assertEqual(rc.point_at(m, {}, ts + 1000, None, None)["s_bps"], D(0))  # 99 / 99 from the next second

    def test_no_anchor_or_no_fx(self):
        m = market({T0 + 1000: row(mid=101)}, {T0 + 1000: row(mid=51)}, segments=[rc.Segment(T0, None, S1, None, None)])
        p = rc.point_at(m, {}, T0 + 1000, D(1000), D(2))
        self.assertEqual((p["s_bps"], p["a_ok"], p["signal"], p["s_long_bps"]), (None, False, "No Anchor", None))
        self.assertEqual(p["bid12_bull"], D(20))     # 100.99 and 100.98 of 10 each: the 1+2 depths need no anchor
        m = market({T0 + 1000: row(mid=101)}, {T0 + 1000: row(mid=51)}, fx={})
        p = rc.point_at(m, {}, T0 + 1000, D(1000), D(2))
        self.assertEqual((p["s_bps"], p["a_ok"], p["signal"]), (None, False, "No Anchor"))

    def test_ordering_of_the_real_spreads_around_the_mid_spread(self):
        m = market({T0 + 1000: row(mid=101, levels=5)}, {T0 + 1000: row(mid=51, levels=5)})
        for n in (D(1), D(1000), D(100000)):
            p = rc.point_at(m, {}, T0 + 1000, n, D(2))
            self.assertGreaterEqual(p["s_long_bps"], p["s_bps"])
            self.assertGreaterEqual(p["s_bps"], p["s_short_bps"])
            self.assertEqual(rc.ordering_violations([p]), [])


class BarsAndMeans(unittest.TestCase):
    """Section 1.3: the minute bars' fallback chain and the runs of SMA and EMA."""

    def test_sma_and_ema_through_the_minute_before(self):
        bull, bear = bar_rows(10)
        m = market(bull, bear)
        # Bars k = 0, 1, 2: bull 100, 101, 102 and bear 50, 50.5, 51 (USD, F = 1).
        self.assertEqual(mu_at(m, "sma", 3, T0 + 3 * M), D(200))     # means 101 / 50.5
        self.assertEqual(mu_at(m, "ema", 3, T0 + 3 * M), D(250))     # alpha 1/2 from 100 / 50: 101.25 / 50.625
        self.assertIsNone(mu_at(m, "sma", 3, T0 + 2 * M))            # two bars only
        self.assertIsNone(mu_at(m, "ema", 3, T0 + 2 * M))
        self.assertEqual(mu_at(m, "sma", 1, T0 + M), D(0))           # the bar of the minute before: 100 / 50

    def test_restart_at_a_missing_minute(self):
        bull, bear = bar_rows(10, skip={1})
        m = market(bull, bear)
        self.assertIsNone(mu_at(m, "sma", 3, T0 + 4 * M))            # run 2, 3
        self.assertEqual(mu_at(m, "sma", 3, T0 + 5 * M), D(600))     # bars 2, 3, 4: 103 / 51.5

    def test_fallbacks_do_not_restart(self):
        bull, bear = bar_rows(10, skip={1}, trade_only={2})
        m = market(bull, bear, candles={"bull": {T0 + M: D(101)}, "bear": {T0 + M: D("50.5")}})
        bars = rc.minute_bars(m, T0, T0 + 4 * M)
        self.assertEqual([b[3] for b in bars], ["mid", "candle", "trade", "mid"])
        self.assertEqual(mu_at(m, "sma", 3, T0 + 3 * M), D(200))

    def test_candle_needs_both_legs(self):
        bull, bear = bar_rows(10, skip={1})
        m = market(bull, bear, candles={"bull": {T0 + M: D(101)}})
        self.assertEqual([b[0] for b in rc.minute_bars(m, T0, T0 + 3 * M)], [T0, T0 + 2 * M])

    def test_restart_without_fx(self):
        bull, bear = bar_rows(12)
        m = market(bull, bear, fx={T0: D(1), T0 + 7 * M: D(1)})   # F of the minutes 6 is missing (more than 5 back)
        self.assertEqual(mu_at(m, "sma", 3, T0 + 6 * M), D(800))     # bars 3, 4, 5: 104 / 52
        self.assertIsNone(mu_at(m, "sma", 3, T0 + 7 * M))            # bar 6 has no F
        self.assertIsNone(mu_at(m, "sma", 3, T0 + 9 * M))            # run 7, 8
        self.assertEqual(mu_at(m, "sma", 3, T0 + 10 * M), D(1600))   # bars 7, 8, 9: 108 / 54

    def test_restart_where_a_legs_anchor_changes(self):
        bull, bear = bar_rows(10)
        segments = [rc.Segment(T0, T0 + 2 * M, S1, A_BULL, A_BEAR), rc.Segment(T0 + 2 * M, None, S1, D(99), A_BEAR)]
        m = market(bull, bear, segments=segments)
        self.assertIsNone(mu_at(m, "sma", 1, T0 + 2 * M))            # the bar before is on the old anchors
        self.assertIsNone(mu_at(m, "sma", 3, T0 + 4 * M))
        want = 10000 * (Fraction(103, 99) + Fraction(103, 2) / 50 - 2)   # bars 2, 3, 4: 103 / 51.5 on 99 / 50
        self.assertAlmostEqual(Fraction(mu_at(m, "sma", 3, T0 + 5 * M)), want, delta=Fraction(1, 10 ** 30))

    def test_restart_at_past_t_and_at_a_session(self):
        bull, bear = bar_rows(10)
        for later in (rc.Segment(T0 + 2 * M, None, S1, A_BULL, A_BEAR, past_t=True), rc.Segment(T0 + 2 * M, None, S2, A_BULL, A_BEAR)):
            m = market(bull, bear, segments=[rc.Segment(T0, T0 + 2 * M, S1, A_BULL, A_BEAR), later])
            self.assertIsNone(mu_at(m, "sma", 3, T0 + 4 * M))
            self.assertEqual(mu_at(m, "sma", 3, T0 + 5 * M), D(600))

    def test_split_halt_and_the_frozen_rows_in_the_bars(self):
        tv = T0 + 4 * M
        bull, bear = bar_rows(8)
        bull[tv + M] = row(mid=200, price=200, trade_ms=tv - 1000, book_ms=tv - 1000)   # frozen in the halt: old basis
        segments = [rc.Segment(T0, T0 + 2 * M, S1, A_BULL, A_BEAR), rc.Segment(T0 + 2 * M, tv, S1, A_BULL, A_BEAR, gap_bull=True),
                    rc.Segment(tv, None, S1, A_BULL / 2, A_BEAR)]
        candles = {leg: {T0 + 2 * M: D(60), T0 + 3 * M: D(60), tv: D(52)} for leg in ("bull", "bear")}   # the halt's closes: no bars
        m = market(bull, bear, segments=segments, splits={"bull": [(S1, tv)]}, candles=candles)
        bars = rc.minute_bars(m, T0, T0 + 6 * M)
        self.assertEqual([(b[0], b[3]) for b in bars], [(T0, "mid"), (T0 + M, "mid"), (tv, "candle"), (tv + M, "mid")])
        self.assertEqual(bars[2][2], (D(52), D(52)))

    def test_mean_ignores_the_points_own_fx(self):
        bull, bear = bar_rows(6)
        bull[T0 + 6 * M + 1000], bear[T0 + 6 * M + 1000] = row(mid=106), row(mid=53)
        m = market(bull, bear, fx={T0 + k * M: D(1) for k in range(-5, 1)})   # minutes 1 to 5 reach back to minute 0's F
        p = rc.point_at(m, rc.means_for(m, "sma", 3, T0 + 6 * M, T0 + 6 * M), T0 + 6 * M + 1000, D(1000), D(2))
        self.assertIsNone(p["s_bps"])                             # minute 6 is 6 minutes after the last F
        self.assertEqual(p["mu_bps"], D(800))                     # bars 3, 4, 5 have F: 104 / 52
        self.assertEqual(p["signal"], "No Anchor")
        self.assertIsNone(p["phase"])


class CappedSweep(unittest.TestCase):
    """Section 1.4: the capped two-pass sweep, partial flags and filled USDT."""

    def real(self, bull, bear, a, fx, n, side_name="long"):
        empty = side((1, 1))
        if side_name == "long":
            out = rc.book_pair(empty, bull, empty, bear, a[0], a[1], fx, n)
        else:
            out = rc.book_pair(bull, empty, bear, empty, a[0], a[1], fx, n)
        return out["s_%s_bps" % side_name], out["partial_" + side_name], out["filled_" + side_name]

    def test_uncapped_one_level(self):
        s, partial, filled = self.real(side((101, 1000)), side((51, 1000)), (D(100), D(50)), D("1.0003"), D(1000))
        self.assertAlmostEqual(s, D("306.09"), delta=D("1e-30"))   # 10^4 (1.0003 x 2.03 - 2)
        self.assertFalse(partial)
        self.assertAlmostEqual(filled, D(1000), delta=D("1e-40"))

    def test_capped_in_the_first_pass(self):
        s, partial, filled = self.real(side((101, 2)), side((51, 1000)), (D(100), D(50)), D("1.0003"), D(1000))
        self.assertEqual(s, D("306.09"))
        self.assertTrue(partial)
        self.assertEqual(filled, D(406))               # K_max 200: 2 x 101 + 4 x 51

    def test_two_levels_uncapped(self):
        s, partial, filled = self.real(side((100, 1), (102, 10)), side((100, 10)), (D(100), D(100)), D(1), D(400))
        self.assertAlmostEqual(s, D("99.5"), delta=D("1e-30"))     # pass 2: q = 400 / 201 each, bull average 100.995
        self.assertFalse(partial)
        self.assertAlmostEqual(Fraction(filled), Fraction(80398, 201), delta=Fraction(1, 10 ** 40))

    def test_capped_only_in_the_second_pass(self):
        s, partial, filled = self.real(side((100, 1), (98, 1)), side((100, 100)), (D(100), D(100)), D(1), D(399), "short")
        self.assertEqual(s, D(-100))                   # pass 1 K 199.5 <= 200, pass 2 wants 200.49...: capped at 200
        self.assertTrue(partial)
        self.assertEqual(filled, D(398))
        v, q, partial = rc.capped_sweep(side((100, 1), (98, 1)), side((100, 100)), D(100), D(100), D(399))
        self.assertEqual(q, (D(2), D(2)))

    def test_root_spec_7_9_book(self):
        v = rc.SPEC79
        out = rc.book_pair(v["bids_bull"], v["asks_bull"], v["bids_bear"], v["asks_bear"], v["a_bull"], v["a_bear"], v["fx"], D(10000))
        # Exact fractions (the root SPEC 7.9 gives s_exec 17.03 with quantities floored to 0.01).
        self.assertAlmostEqual(Fraction(out["s_short_bps"]), Fraction(51103066189, 3000000000), delta=Fraction(1, 10 ** 30))
        self.assertAlmostEqual(Fraction(out["s_long_bps"]), Fraction(155, 6), delta=Fraction(1, 10 ** 30))   # 25.8333...
        self.assertEqual((out["partial_long"], out["partial_short"]), (False, False))
        mid = rc.spread_bps(D(1), D("40.46"), D("59.44"), D(40), D(60))
        self.assertTrue(out["s_long_bps"] >= mid >= out["s_short_bps"])
        self.assertEqual((out["bid12_bull"], out["ask12_bear"]), (None, None))   # two levels: Not Recorded

    def test_quantities_keep_the_basket_neutral_and_fills_within_n(self):
        for name, v in rc.BOOK_VECTORS:
            if v["notional"] is None or not v["notional"] > 0 or v["a_bull"] is None or v["fx"] is None:
                continue
            for bull, bear in ((v["asks_bull"], v["asks_bear"]), (v["bids_bull"], v["bids_bear"])):
                (vb, vr), (qb, qr), partial = rc.capped_sweep(bull, bear, v["a_bull"], v["a_bear"], v["notional"])
                self.assertAlmostEqual(qb * v["a_bull"], qr * v["a_bear"], delta=D("1e-40"), msg=name)
                self.assertLessEqual(qb * vb + qr * vr, v["notional"] * (1 + D("1e-40")), msg=name)
                self.assertLessEqual(qb, sum(s for _, s in bull) * (1 + D("1e-40")), msg=name)

    def test_no_value_without_a_positive_notional_an_anchor_or_fx(self):
        w = rc.WHOLE
        for n, a_bull, fx in ((None, w["a_bull"], w["fx"]), (D(0), w["a_bull"], w["fx"]), (D(-5), w["a_bull"], w["fx"]),
                              (D(1000), None, w["fx"]), (D(1000), w["a_bull"], None)):
            out = rc.book_pair(w["bids_bull"], w["asks_bull"], w["bids_bear"], w["asks_bear"], a_bull, w["a_bear"], fx, n)
            self.assertEqual([out[c] for c in rc.REAL_COLUMNS], [None] * 6)
            self.assertEqual([out[c] for c in rc.DEPTH_COLUMNS], [D(800), D(260), D(400), D(300)])

    def test_value_stops_moving_once_capped(self):
        w = rc.WHOLE
        a = rc.book_pair(w["bids_bull"], w["asks_bull"], w["bids_bear"], w["asks_bear"], w["a_bull"], w["a_bear"], w["fx"], D(10 ** 6))
        b = rc.book_pair(w["bids_bull"], w["asks_bull"], w["bids_bear"], w["asks_bear"], w["a_bull"], w["a_bear"], w["fx"], D(10 ** 8))
        self.assertEqual(a, b)
        self.assertTrue(a["partial_long"] and a["partial_short"])


class Depths(unittest.TestCase):
    """Section 1.7: each leg's Bid / Ask 1+2 Depth in contracts."""

    EXPECTED = {"0.01 bids, third level": D(800), "0.01 bids, two levels: Not Recorded": None, "0.01 asks, a price gap": D(3),
                "one level: Not Recorded": None, "0.001 bids floor": D(36), "0.001 asks ceil": D(13),
                "0.001 asks on the boundary": D(9), "0.001 bids on the boundary": D(9), "trailing zeros": D("3.75"),
                "0.001 asks within two buckets: Not Recorded": None}

    def test_hand_made_sides(self):
        self.assertEqual(set(self.EXPECTED), {name for name, *_ in rc.TOP2_VECTORS})
        for name, px, sz, bids in rc.TOP2_VECTORS:
            self.assertEqual(rc.top2_size(side(*zip(px, sz)), bids), self.EXPECTED[name], name)

    def test_bucket_direction(self):
        self.assertEqual(rc.bucket(D("30.459"), True), D("30.45"))
        self.assertEqual(rc.bucket(D("30.459"), False), D("30.46"))
        self.assertEqual(rc.bucket(D("0.29"), True), D("0.29"))     # exact in decimal (float8 floors 0.29 x 100 to 28)
        self.assertEqual(rc.bucket(D("40.47000"), False), D("40.47"))

    def test_design_example_bitget_2330(self):
        # Bitget 2026-11-22 23:30:00 UTC+8 bids as recorded (DESIGN.md 1.7 example: SOXL 3,363.6, SOXS 91.7).
        soxl = side((33.9, 455.3), (33.89, 2908.3), (33.88, 2319.63), (33.87, 1589.49), (33.86, 2136.93))
        soxs = side((75.177, 6.19), (75.175, 5), (75.171, 0.07), (75.169, 18.23), (75.168, 20.12), (75.166, 5), (75.165, 13.72),
                    (75.164, 5.07), (75.163, 0.07), (75.162, 13.23), (75.16, 5), (75.159, 11.31))
        self.assertEqual(rc.top2_size(soxl, True), D("3363.6"))
        self.assertEqual(rc.top2_size(soxs, True), D("91.70"))

    def test_depths_on_a_trade_point_with_one_book(self):
        m = market({T0 + 1000: row(mid=101, price=101)}, {T0 + 1000: row(price=51)})
        p = rc.point_at(m, {}, T0 + 1000, D(1000), D(2))
        self.assertEqual((p["basis"], p["book_bull"], p["book_bear"]), ("trade", True, False))
        self.assertEqual((p["bid12_bull"], p["ask12_bull"], p["bid12_bear"], p["ask12_bear"]), (D(20), D(20), None, None))
        self.assertEqual(rc.depth_codes([p], "bid12_bull"), {"value": 1})
        self.assertEqual(rc.depth_codes([p], "bid12_bear"), {"No Book": 1})

    def test_not_recorded_on_a_usable_book(self):
        m = market({T0 + 1000: row(mid=101, levels=2)}, {T0 + 1000: row(mid=51, levels=1)})
        p = rc.point_at(m, {}, T0 + 1000, D(1000), D(2))
        self.assertEqual([p[c] for c in rc.DEPTH_COLUMNS], [None] * 4)
        self.assertEqual(rc.depth_codes([p], "ask12_bear"), {"Not Recorded": 1})
        self.assertIsNotNone(p["s_long_bps"])           # the real spreads take what was recorded


class Signals(unittest.TestCase):
    """Section 1.6: the Signal text, its precedence and ' · Partial'."""

    def test_hand_made_cases(self):
        for case, want in rc.SIGNAL_VECTORS:
            self.assertEqual(rc.signal_of(*rc.signal_case(case)), want, case)

    def test_partial_long_on_a_point(self):
        bull = {T0 + 3 * M + 1000: rc.Second(bids=side((99.5, 1000)), asks=side((100, 1)))}   # 1 contract on the asks
        bear = {T0 + 3 * M + 1000: rc.Second(bids=side((49.9, 1000)), asks=side((50, 1000)))}
        b, r = bar_rows(3)
        bull.update(b)
        bear.update(r)
        m = market(bull, bear)
        means = rc.means_for(m, "sma", 3, T0 + 3 * M, T0 + 3 * M)    # mu 200 (101 / 50.5)
        p = rc.point_at(m, means, T0 + 3 * M + 1000, D(1000), D(2))
        self.assertEqual((p["mu_bps"], p["s_long_bps"], p["partial_long"]), (D(200), D(0), True))
        self.assertEqual(p["signal"], "Long" + rc.PARTIAL)
        # Real Short Spread -70 is 270 below the mean: its absolute deviation passes Entry without a Short (1.5).
        self.assertAlmostEqual(p["s_short_bps"], D(-70), delta=D("1e-30"))
        self.assertEqual(rc.off_signal([p], D(2)), (0, 1, 1))


class PointsAndBreaks(unittest.TestCase):
    """Section 1.1: the points at the step's multiples in (from, to] and the break rows."""

    def test_multiples(self):
        self.assertEqual(list(rc.multiples(1000, 4000, 1000)), [2000, 3000, 4000])
        self.assertEqual(list(rc.multiples(1500, 4999, 1000)), [2000, 3000, 4000])
        self.assertEqual(list(rc.multiples(4000, 4000, 1000)), [])

    def test_breaks(self):
        pts = [{"ts_ms": t, "session_date": s} for t, s in ((1000, S1), (2000, S1), (5000, S1), (6000, S2))]
        out = rc.with_breaks(pts, 1000, ("ts_ms", "session_date"))
        self.assertEqual([(r["ts_ms"], r["session_date"]) for r in out],
                         [(1000, S1), (2000, S1), (2001, None), (5000, S1), (5001, None), (6000, S2)])

    def test_combo_range_columns_phase_and_breaks(self):
        bull, bear = bar_rows(4)
        for t in (2, 3, 6):    # seconds of the minute 4 (k = 4), with a gap
            bull[T0 + 4 * M + t * 1000], bear[T0 + 4 * M + t * 1000] = row(mid=104), row(mid=52)
        m = market(bull, bear, phases=[(T0, T0 + 4 * M + 3000, "Regular"), (T0 + 4 * M + 3000, T0 + 10 * M, "After-Hours")])
        rows = rc.combo_range(m, "sma", 3, T0 + 4 * M, T0 + 4 * M + 6000, 1000, D(1000), D(2))
        self.assertTrue(all(tuple(r) == rc.RANGE_COLUMNS for r in rows))
        self.assertEqual([r["ts_ms"] - T0 - 4 * M for r in rows], [2000, 3000, 3001, 6000])
        self.assertEqual([r["phase"] for r in rows], ["Regular", "Regular", None, "After-Hours"])   # ts <= end_ms is in
        self.assertEqual([r["phase_sort"] for r in rows], [2, 2, None, 3])
        self.assertEqual(rows[0]["mu_bps"], D(400))     # bars 1, 2, 3: 102 / 51
        self.assertEqual([c for c in rc.RANGE_COLUMNS if rows[2][c] is not None], ["ts_ms"])

    def test_phase_only_with_a_deviation(self):
        bull, bear = bar_rows(1)
        bull[T0 + M + 1000], bear[T0 + M + 1000] = row(mid=100), row(mid=50)
        m = market(bull, bear, phases=[(T0, T0 + 10 * M, "Overnight")])
        rows = rc.combo_range(m, "sma", 3, T0 + M, T0 + M + 1000, 1000, D(1000), D(2))
        self.assertEqual((rows[0]["signal"], rows[0]["phase"]), ("No Mean", None))

    def test_board_step(self):
        hour, day = 3_600_000, 86_400_000
        self.assertEqual([rc.board_step(x) for x in (hour, 6 * hour, 24 * hour, 7 * day, 30 * day)], [1000, 5000, 30000, 180000, 540000])
        self.assertEqual([rc.board_step(x) for x in (5_000_000, 5_000_001, 150_000_000, 150_000_001)], [1000, 5000, 30000, 60000])
        # Last 1 year / 2 years: ceil(span / 5000 / 60 s) whole minutes, 105.12 -> 106 and 210.24 -> 211 (STEP_MS).
        self.assertEqual([rc.board_step(x) for x in (3 * hour, 365 * day, 730 * day)], [5000, 106 * M, 211 * M])


class PricesAndLast(unittest.TestCase):
    """Sections 1.8 and 1.12: the trade-price points and the two newest seconds."""

    def test_price_points_need_both_legs_trades(self):
        ts = T0 + 1000
        m = market({ts: row(mid=101), ts + 1000: row(price=101)}, {ts: row(price=51), ts + 1000: row(price=51)},
                   ref={ts + 1000: row(mid=200)})
        out = rc.combo_prices(m, T0, ts + 1000, 1000)
        self.assertEqual([r["ts_ms"] for r in out], [ts + 1000])
        self.assertIsNone(out[0]["p_ref"])                       # the reference has a book, no trade
        self.assertEqual((out[0]["fx"], out[0]["a_ref"]), (D(1), D(200)))

    def test_reference_in_its_halt_or_frozen_after_its_split(self):
        ts, tv = T0 + 3 * M + 1000, T0 + 2 * M
        segments = [rc.Segment(T0, T0 + 3 * M, S1, A_BULL, A_BEAR, D(200), gap_ref=True), rc.Segment(T0 + 3 * M, None, S1, A_BULL, A_BEAR, D(100))]
        legs = {T0 + 1000: row(price=100), ts: row(price=100)}
        m = market(legs, legs, ref={T0 + 1000: row(price=200), ts: row(price=100, trade_ms=tv - 1)}, segments=segments,
                   splits={"ref": [(S1, tv)]})
        out = rc.combo_prices(m, T0, ts, 1000)
        self.assertEqual([(r["ts_ms"], r["p_ref"]) for r in out if r["p_bull"] is not None], [(T0 + 1000, None), (ts, None)])

    def test_two_newest_seconds(self):
        now = T0 + 20 * M
        bull, bear = bar_rows(20)
        bull.update({now: row(mid=101), now - 1000: row(mid=101, price=101), now - 2000: row(price=101)})
        bear.update({now: row(mid=51), now - 1000: row(mid=51, price=51), now - 2000: row(price=51)})
        ref = {now - 1000: row(mid=200), now - 2000: row(price=200), now - LAST_WINDOW - 1000: row(price=200)}
        m = market(bull, bear, ref=ref, statuses={S1: "Bell"})
        out = {r["kind"]: r for r in rc.combo_last(m, "sma", 3, D(1000), D(2), now)}
        self.assertEqual(out["prices"]["ts_ms"], now - 2000)    # the newest second with the reference's trade too
        self.assertEqual(out["basis"]["ts_ms"], now)            # the newest second with a basis price
        self.assertEqual((out["basis"]["basis"], out["basis"]["anchor_status"], out["basis"]["s_bps"]), ("mid", "Bell", D(300)))
        self.assertEqual(out["basis"]["filled_long"] is not None, True)
        self.assertTrue(all(tuple(r) == rc.LAST_COLUMNS for r in out.values()))
        # A second exactly 10 minutes back is outside (now - 10 min, now]: no 'prices' row.
        bull[now - LAST_WINDOW], bear[now - LAST_WINDOW] = row(mid=91, price=91), row(mid=46, price=46)
        m = market(bull, bear, ref={now - LAST_WINDOW: row(price=200)})
        self.assertEqual([r["kind"] for r in rc.combo_last(m, "sma", 3, D(1000), D(2), now)], ["basis"])
        m = market(bull, bear, ref={now - LAST_WINDOW + 1000: row(price=200)})
        bull[now - LAST_WINDOW + 1000], bear[now - LAST_WINDOW + 1000] = row(price=91), row(price=46)
        self.assertEqual(rc.combo_last(m, "sma", 3, D(1000), D(2), now)[0]["ts_ms"], now - LAST_WINDOW + 1000)


class EntryPoints(unittest.TestCase):
    """Section 1.11: the table by phase from combo_range's rows."""

    def test_table(self):
        def r(phase, signal, mu, s_long, s_short, past_t=False):
            return {"phase": phase, "signal": signal, "past_t": past_t, "mu_bps": None if mu is None else D(mu),
                    "s_long_bps": None if s_long is None else D(s_long), "s_short_bps": None if s_short is None else D(s_short)}
        rows = [r("Regular", "Long", 10, 5, 3), r("Regular", "Long" + rc.PARTIAL, 10, 2, 1), r("Regular", "Short", 10, 20, 16),
                r("Regular", "None", 10, 11, 9), r("Regular", "Past T", 10, 0, 30, True), r("Regular", "No Book", 10, None, None),
                r("Regular", "None", 10, 10, 10), r("After-Hours", "None", 0, 1, -1), r("After-Hours", "Short" + rc.PARTIAL, 0, 5, 4),
                r(None, None, None, None, None), r(None, "No Mean", None, 5, 3), r("Overnight", "No Book", 0, None, None)]
        table = rc.entry_points(rows)
        self.assertEqual([g["phase"] for g in table], ["Regular", "After-Hours", "Overnight"])
        self.assertEqual(table[0], {"phase": "Regular", "long_points": 2, "short_points": 1, "partial_points": 1, "best_long": D(8),
                                    "best_short": D(6), "past_t_points": 1, "no_book_points": 1, "points": 7})
        self.assertEqual(table[1], {"phase": "After-Hours", "long_points": 0, "short_points": 1, "partial_points": 1, "best_long": None,
                                    "best_short": D(4), "past_t_points": 0, "no_book_points": 0, "points": 2})
        self.assertEqual((table[2]["best_long"], table[2]["points"]), (None, 1))


class Coverage(unittest.TestCase):
    """Section 3.3: the counts per minute of T - 1 ms and over a range."""

    def test_minute_counts(self):
        rows = [(T0, True, True, 1, 1), (T0 + 1000, True, True, 5, 5), (T0 + M, True, False, None, None),
                (T0 + M + 1000, False, True, 10, 5), (T0 + 2 * M, True, True, 50, 50)]
        counts = rc.minute_counts(rows)
        self.assertEqual(counts, {T0 - M: [1, 1, 1, 1], T0: [2, 1, 5, 5], T0 + M: [1, 2, 5, 50]})
        self.assertEqual(rc.coverage(counts.values()), (4, 4, 1, 50))
        self.assertEqual(rc.coverage([]), (0, 0, None, None))
        self.assertEqual(rc.coverage(rc.minute_counts([(T0, True, False, 0, 3)]).values()), (1, 0, None, None))


class Comparison(unittest.TestCase):
    """The comparison rules: 1e-9 for computed numbers, exact for prices and sizes, NULLs by position."""

    def test_same(self):
        self.assertTrue(rc.same("s_bps", D("1.0000000005"), D(1)))
        self.assertFalse(rc.same("s_bps", D("1.000000002"), D(1)))
        self.assertFalse(rc.same("bid12_bull", D("3363.6000000001"), D("3363.6")))
        self.assertTrue(rc.same("bid12_bull", D("3363.60"), D("3363.6")))
        self.assertFalse(rc.same("mu_bps", None, D(0)))
        self.assertTrue(rc.same("best_long", D("8.00000000001"), D(8)))   # Entry Points' Best columns are bps: within 1e-9
        self.assertFalse(rc.same("best_short", D("8.000000002"), D(8)))
        self.assertTrue(rc.same("signal", None, None))

    def test_report_counts_and_open_cells(self):
        report = rc.Report(3)
        mine = [{"ts_ms": 1, "s_long_bps": None, "partial_long": None, "signal": "No Notional"}]
        theirs = [{"ts_ms": 1, "s_long_bps": None, "partial_long": False, "signal": "No Notional"},
                  {"ts_ms": 2, "s_long_bps": None, "partial_long": None, "signal": None}]
        with redirect_stdout(io.StringIO()):
            bad = report.compare("t", mine, theirs, ("ts_ms", "s_long_bps", "partial_long", "signal"))
            report.columns("t", [{"ts_ms": "1"}], ("ts_ms", "signal"))
        self.assertEqual((bad, report.open), (1, 1))     # row 2 only there; partial_long is open in the design
        self.assertEqual(report.failures, 2)             # and the missing signal column of the one row

    def test_literals(self):
        self.assertEqual(rc.lit(D("1E+1")), "10::numeric")
        self.assertEqual(rc.lit(None), "NULL::numeric")
        self.assertEqual(rc.array_lit(side((40.45, 1)), 0), "'{40.45}'::numeric[]")
        with self.assertRaises(SystemExit):
            rc.quoted("x'; DROP TABLE y; --")


class CommandLine(unittest.TestCase):
    def test_times_ranges_and_lists(self):
        self.assertEqual(rc.instant("2026-11-23 00:00"), 1795363200000)
        self.assertEqual(rc.instant("2026-11-22 23:30:00"), 1795361400000)
        self.assertEqual(rc.instant("1795361400000"), 1795361400000)
        self.assertEqual([rc.span(x) for x in ("90m", "1h", "7d")], [5_400_000, 3_600_000, 604_800_000])
        self.assertEqual(rc.numbers("1000, null,1e5"), [D(1000), None, D(100000)])

    def test_arguments(self):
        args = rc.parse_args(["--dsn", "x", "--combo", "SOXL-SOXS", "--venue", "bitget", "--ma", "ema", "--n", "240"])
        self.assertEqual((args.schema, args.functions_schema, args.checks), ("etfpair", "etfpair", {"range", "prices", "last", "coverage"}))
        self.assertEqual(args.n, 240)
        with redirect_stdout(io.StringIO()), self.assertRaises(SystemExit):
            rc.parse_args(["--dsn", "x", "--combo", "c", "--venue", "v", "--ma", "sma", "--n", "3", "--checks", "range,nope"])

    def test_invalid_mean_minutes_and_entries(self):
        # FIX_R2 B4: --n null, 0 and below; --entry null, 0 and -5 (alone, or in a list written with '=').
        base = ["--dsn", "x", "--combo", "c", "--venue", "v", "--ma", "sma"]
        self.assertEqual([rc.parse_args(base + ["--n", n]).n for n in ("null", "NULL", "0", "-5")], [None, None, 0, -5])
        self.assertEqual(rc.numbers(rc.parse_args(base + ["--n", "30", "--entry", "null,0,-5"]).entry), [None, D(0), D(-5)])
        self.assertEqual(rc.numbers(rc.parse_args(base + ["--n", "30", "--entry", "-5"]).entry), [D(-5)])
        self.assertEqual(rc.numbers(rc.parse_args(base + ["--n", "30", "--entry=-5,2"]).entry), [D(-5), D(2)])
        with redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            rc.parse_args(base + ["--n", "30.5"])
        self.assertEqual([rc.int_lit(n) for n in (None, 0, -5, 240)], ["NULL::integer", "(0)::integer", "(-5)::integer", "(240)::integer"])
        self.assertEqual(rc.stamp(1795363200000), "2026-11-23 00:00:00")
        self.assertEqual(rc.stamp(1795363200250), "2026-11-23 00:00:00.250")

    def test_calls_pass_invalid_minutes_and_entries(self):
        m, t = signal_market()
        db, calls = fake_db(FakeSnap()), []
        db.market = m
        db.call = lambda function, args, order="ts_ms": calls.append((function, args)) or []
        args = argparse.Namespace(checks={"range", "last"}, print=0, dump=None)
        with redirect_stdout(io.StringIO()):
            rc.check_range(db, rc.Report(3), args, "sma", None, t, t + 3000, 1000, D(1000), D(-5))
            rc.check_last(db, rc.Report(3), args, "ema", 0, None, None, t + 3000)
        self.assertEqual(calls, [("combo_range", ["'SOXL-SOXS'", "'bitget'", "'sma'", "NULL::integer", "%d::bigint" % t, "%d::bigint" % (t + 3000),
                                                  "1000::bigint", "1000::numeric", "-5::numeric"]),
                                 ("combo_last", ["'SOXL-SOXS'", "'bitget'", "'ema'", "(0)::integer", "NULL::numeric", "NULL::numeric",
                                                 "%d::bigint" % (t + 3000)])])


class NoEntry(unittest.TestCase):
    """FIX_R2 A7 / B1: an Entry that is NULL or not above 0 is No Entry, after No Notional and before No Book."""

    BAD = (None, D(0), D(-5))

    def test_position_in_the_order(self):
        # (s, past_t, mu, N, mid, s_long, s_short, partial_long, partial_short) -> the Signal at Entry 2, at a bad Entry
        cases = [
            ((None, True, None, None, False, None, None, None, None), "No Anchor", "No Anchor"),
            ((D(5), True, D(1), D(1000), True, D(-9), D(9), False, False), "Past T", "Past T"),
            ((D(5), False, None, D(1000), True, D(-9), D(9), False, False), "No Mean", "No Mean"),
            ((D(5), False, D(1), None, True, None, None, None, None), "No Notional", "No Notional"),
            ((D(5), False, D(1), D(-1), False, None, None, None, None), "No Notional", "No Notional"),
            ((D(5), False, D(1), D(1000), False, None, None, None, None), "No Book", "No Entry"),
            ((D(5), False, D(1), D(1000), True, D(-9), D(9), True, False), "Long" + rc.PARTIAL, "No Entry"),
            ((D(5), False, D(1), D(1000), True, D(9), D(9), False, False), "Short", "No Entry"),
            ((D(5), False, D(1), D(1000), True, D(2), D(0), False, False), "None", "No Entry"),
        ]
        for args, want, want_bad in cases:
            self.assertEqual(rc.signal_of(*args, D(2)), want, args)
            for d in self.BAD:
                self.assertEqual(rc.signal_of(*args, d), want_bad, (args, d))
        self.assertEqual(rc.signal_of(D(5), False, D(1), D(1000), True, D(1), D(1), False, False, D("1e-12")), "None")

    def test_points_entry_points_and_latest_second(self):
        m, t = signal_market()
        good = rc.combo_range(m, "sma", 3, t, t + 3000, 1000, D(1000), D(2))
        self.assertEqual([(r["mu_bps"], r["signal"]) for r in good], [(D(200), "Long"), (D(200), "No Book"), (D(200), "Short")])
        table = rc.entry_points(good)
        self.assertEqual([(g["phase"], g["long_points"], g["short_points"], g["no_book_points"], g["points"]) for g in table],
                         [("Regular", 1, 1, 1, 3)])
        self.assertEqual(rc.off_signal(good, D(2)), (1, 1, 2))   # the Short's Real Long and the Long's Real Short pass Entry
        for d in self.BAD:
            rows = rc.combo_range(m, "sma", 3, t, t + 3000, 1000, D(1000), d)
            self.assertEqual([r["signal"] for r in rows], ["No Entry"] * 3, d)
            self.assertEqual([dict(r, signal=None) for r in rows], [dict(r, signal=None) for r in good])   # all else unchanged
            # No Entry hides No Book too (it comes first); Best Long / Short do not depend on the Entry.
            self.assertEqual(rc.entry_points(rows), [dict(table[0], long_points=0, short_points=0, partial_points=0, no_book_points=0)])
            self.assertEqual(rc.off_signal(rows, d), (0, 0, 0))
            self.assertEqual([r["signal"] for r in rc.combo_range(m, "sma", 3, t, t + 3000, 1000, None, d)], ["No Notional"] * 3)
            self.assertEqual([r["signal"] for r in rc.combo_range(m, "sma", 3, t - M, t - M + 1000, 1000, D(1000), d)], [])
            last = {r["kind"]: r for r in rc.combo_last(m, "sma", 3, D(1000), d, t + 3000)}
            self.assertEqual((last["basis"]["ts_ms"], last["basis"]["mu_bps"], last["basis"]["signal"]), (t + 3000, D(200), "No Entry"))


class NoMeanMinutes(unittest.TestCase):
    """FIX_R2 A7 / B2: a number of minutes that is NULL or not above 0 draws the points, their depths and real spreads
    without a mean."""

    def test_validity(self):
        self.assertEqual([rc.mean_minutes_valid(n) for n in (None, -5, 0, 1, 240)], [False, False, False, True, True])

    def test_points_without_a_mean(self):
        m, t = signal_market()
        good = rc.combo_range(m, "sma", 3, t, t + 3000, 1000, D(1000), D(2))
        kept = [c for c in rc.RANGE_COLUMNS if c not in ("mu_bps", "phase", "phase_sort", "signal")]
        for ma in ("sma", "ema"):
            for n in (None, 0, -5):
                rows = rc.combo_range(m, ma, n, t, t + 3000, 1000, D(1000), D(2))
                self.assertEqual([[r[c] for c in kept] for r in rows], [[r[c] for c in kept] for r in good], (ma, n))
                self.assertEqual([(r["mu_bps"], r["phase"], r["phase_sort"], r["signal"]) for r in rows],
                                 [(None, None, None, "No Mean")] * 3, (ma, n))
                self.assertIsNotNone(rows[0]["s_long_bps"])
                self.assertEqual(rows[0]["bid12_bull"], D(20))
                self.assertEqual(rc.entry_points(rows), [])          # no point has a Deviation
                self.assertEqual([r["signal"] for r in rc.combo_range(m, ma, n, t, t + 1000, 1000, None, None)], ["No Mean"])
                last = {r["kind"]: r for r in rc.combo_last(m, ma, n, D(1000), D(2), t + 3000)}["basis"]
                self.assertEqual((last["ts_ms"], last["mu_bps"], last["signal"]), (t + 3000, None, "No Mean"))
                self.assertTrue(last["filled_long"] is not None and last["s_short_bps"] is not None and last["ask12_bear"] == D(20))
        anchorless = market(*bar_rows(4), segments=[rc.Segment(T0, None, S1, None, None)])
        self.assertEqual([r["signal"] for r in rc.combo_range(anchorless, "sma", None, T0 + 3 * M, T0 + 4 * M, M, D(1000), D(2))],
                         ["No Anchor"])

    def test_no_bar_is_made(self):
        m, t = signal_market()
        calls, original = [], rc.minute_bars
        rc.minute_bars = lambda *a: calls.append(a) or original(*a)
        try:
            for n in (None, 0, -5):
                self.assertEqual(rc.means_for(m, "ema", n, t, t), {})
            self.assertEqual(calls, [])
            self.assertTrue(rc.means_for(m, "ema", 3, t, t))
            self.assertEqual(len(calls), 1)
        finally:
            rc.minute_bars = original


class BeforeTheFirstClose(unittest.TestCase):
    """FIX_R2 B3 (A1, A2): ranges that start before the first close, long before the data, compare as the range from the
    data on, and nothing before the first close is enumerated or read by the minute."""

    C = T0 + 30 * M          # the first close: the start of the first segment
    DAY = 86_400_000

    def make(self):
        """Trades every second from T0 + 10 min to T0 + 60 min, priced 100 + k / 50 + k / 2 in minute k of T0; books (mid =
        the trade) from T0 + 25 min to T0 + 35 min, across the close; one session from the close, anchors 100 / 50."""
        bull, bear = {}, {}
        for ts in range(T0 + 10 * M + 1000, T0 + 60 * M + 1, 1000):
            k = (rc.minute_of(ts) - T0) // M
            book = T0 + 25 * M < ts <= T0 + 35 * M
            bull[ts] = row(mid=100 + k if book else None, price=100 + k)
            bear[ts] = row(mid=D(50) + D(k) / 2 if book else None, price=D(50) + D(k) / 2)
        return market(bull, bear, segments=[rc.Segment(self.C, None, S1, A_BULL, A_BEAR, D(200))],
                      phases=[(T0, T0 + 70 * M, "Overnight")])

    def test_lookback(self):
        segments = [rc.Segment(self.C, self.C + 60 * M, S1, A_BULL, A_BEAR), rc.Segment(self.C + 70 * M, None, S2, A_BULL, A_BEAR)]
        m = rc.Market(segments)
        self.assertEqual(m.lookback(T0 - 730 * self.DAY), self.C)          # before the first close: its start
        self.assertEqual(m.lookback(self.C - M), self.C)
        self.assertEqual(m.lookback(self.C + 10 * M), self.C)              # in a session: the session's start
        self.assertEqual(m.lookback(self.C + 65 * M), self.C + 70 * M)     # between segments: the next one
        self.assertEqual(m.lookback(self.C + 80 * M), self.C + 70 * M)
        closed = rc.Market(segments[:1])
        self.assertEqual(closed.lookback(self.C + 61 * M), self.C + 61 * M)   # after the last one there is no bar anyway

    def test_points_before_and_after_the_first_close(self):
        rows = {r["ts_ms"]: r for r in rc.combo_range(self.make(), "ema", 3, T0, T0 + 60 * M, 1000, D(1000), D(2))}
        self.assertEqual(min(rows), T0 + 10 * M + 1000)                      # the first data
        before = rows[self.C]                                               # T = the close is in the minute before it
        self.assertEqual((before["basis"], before["session_date"], before["s_bps"], before["mu_bps"], before["a_ok"],
                          before["past_t"], before["phase"], before["signal"]), ("mid", None, None, None, False, False, None, "No Anchor"))
        self.assertEqual((before["s_long_bps"], before["bid12_bull"], before["ask12_bear"]), (None, D(20), D(20)))
        self.assertEqual([c for c in rc.RANGE_COLUMNS if rows[self.C + 1][c] is not None], ["ts_ms"])   # a break: a new session
        first = rows[self.C + 1000]
        self.assertEqual((first["session_date"], first["s_bps"], first["mu_bps"], first["signal"]), (S1, D(6000), None, "No Mean"))
        # EMA3 of the bars 30, 31, 32 only (130 / 65 ... 132 / 66, alpha 1/2: 131.25 / 65.625): seeded at the first close.
        self.assertEqual(rows[self.C + 3 * M + 1000]["mu_bps"], D(6250))
        self.assertIsNone(rows[self.C + 3 * M]["mu_bps"])                    # minute 32 has two bars before it
        sma = rc.combo_range(self.make(), "sma", 3, self.C + 3 * M, self.C + 3 * M + 1000, 1000, D(1000), D(2))
        self.assertEqual(sma[0]["mu_bps"], D(6200))                          # 131 / 65.5

    def test_long_range_gives_the_rows_from_the_data(self):
        for ma in ("sma", "ema"):
            for step in (1000, 5000, 60000):
                m = self.make()
                far = rc.combo_range(m, ma, 3, T0 - 2 * self.DAY, T0 + 60 * M, step, D(1000), D(2))
                near = rc.combo_range(m, ma, 3, T0 + 10 * M, T0 + 60 * M, step, D(1000), D(2))
                self.assertTrue(near)
                self.assertEqual(far, near, (ma, step))
            m = self.make()
            last = rc.combo_last(m, ma, 3, D(1000), D(2), self.C + 1)    # now in the first second of the first session
            self.assertEqual([(r["kind"], r["ts_ms"], r["session_date"]) for r in last], [("basis", self.C, None)])

    def test_bars_start_at_the_first_close(self):
        calls, original = [], rc.minute_bars
        rc.minute_bars = lambda market_, start, stop: calls.append(start) or original(market_, start, stop)
        try:
            for days in (365, 730):
                far = T0 + 60 * M - days * self.DAY
                rc.combo_range(self.make(), "ema", 240, far, T0 + 60 * M, rc.board_step(days * self.DAY), D(1000), D(2))
                rc.combo_last(self.make(), "sma", 30, D(1000), D(2), self.C - 5 * M)
        finally:
            rc.minute_bars = original
        self.assertEqual(calls, [self.C] * 4)

    def test_loader_reads_no_minute_before_the_first_close(self):
        db = fake_db(FakeSnap(), segments=[rc.Segment(self.C, None, S1, A_BULL, A_BEAR)])
        far = T0 - 730 * self.DAY
        db.load_for_points(far, T0 + 60 * M, 5000)
        sql = "\n".join(db.snap.sql)
        series = [tuple(int(x) for x in g) for g in re.findall(r"generate_series\((-?\d+)::bigint, (-?\d+)::bigint, (\d+)::bigint\)", sql)]
        self.assertEqual(series, [(far + 5000, T0 + 60 * M, 5000), (self.C + M, T0 + 59 * M, M)])   # the points, then the bar rows
        self.assertIn("perp_minute WHERE venue = 'bitget' AND symbol IN ('SOXLUSDT', 'SOXSUSDT') AND minute_ms >= %d AND" % self.C, sql)
        # F by range from the first minute: the points before the first close show it (combo_prices, combo_last).
        self.assertIn("fx_minute WHERE pair = 'USDT-USD' AND minute_ms >= %d AND minute_ms <= %d" % (far - 5 * M, T0 + 60 * M), sql)
        db = fake_db(FakeSnap(), segments=[rc.Segment(self.C, None, S1, A_BULL, A_BEAR)])
        db.load_for_points(far, T0 + 60 * M, 5000, bars=False)              # without a mean: no candles, no bar rows
        self.assertEqual(len(re.findall("generate_series", "\n".join(db.snap.sql))), 1)
        self.assertNotIn("perp_minute", "\n".join(db.snap.sql))


class SettledMinutes(unittest.TestCase):
    """FIX_R2 A3 / B5: perp_second_minute's rows are compared only where the board reads them, minute + 66 min <= now
    (pinned here); the newer minutes are counted as skipped."""

    NOW = T0 + 200 * M + 12_345      # the newest settled minute is T0 + 134 min

    def table(self):
        """One row a minute for T0 ... T0 + 199 min, final, equal to perp_second's count [1, 1, 5, 5] but at 20 (old),
        134 (the newest settled) and 135 (newer); 190 newer and not final; no row for 30 (old) and 180 (newer)."""
        rows = {T0 + k * M: {"minute_ms": str(T0 + k * M), "seconds": "1", "books": "1", "least_levels": "5", "most_levels": "5",
                             "refreshed_ms": str(T0 + (k + 2) * M)} for k in range(200)}
        for k, seconds in ((20, "0"), (134, "2"), (135, "3"), (190, "9")):
            rows[T0 + k * M]["seconds"] = seconds
        rows[T0 + 190 * M]["refreshed_ms"] = str(T0 + 191 * M)      # aggregated before minute + 70 s
        del rows[T0 + 30 * M], rows[T0 + 180 * M]
        return rows

    def test_boundary(self):
        self.assertTrue(rc.settled(T0 + 134 * M, T0 + 200 * M))
        self.assertFalse(rc.settled(T0 + 134 * M, T0 + 200 * M - 1))
        self.assertTrue(rc.settled(T0 + 134 * M, self.NOW) and not rc.settled(T0 + 135 * M, self.NOW))
        self.assertTrue(rc.final_row(T0, T0 + 70_000) and not rc.final_row(T0, T0 + 69_999))
        self.assertEqual([rc.whole_minutes(T0, hi) for hi in (T0 - 1, T0, T0 + M - 1, T0 + 199 * M + 30_000)], [0, 1, 1, 200])

    def test_table_rows(self):
        per_minute = {T0 + k * M: [1, 1, 5, 5] for k in range(200)}
        t = rc.table_check(per_minute, self.table(), T0, T0 + 199 * M, self.NOW)
        self.assertEqual((t.whole, t.rows, t.final, t.differ, t.missing, t.skipped, t.skipped_final),
                         (135, 134, 134, (T0 + 20 * M, T0 + 134 * M), 1, 65, 63))
        t = rc.table_check(per_minute, self.table(), T0, T0 + 199 * M, T0 + 300 * M)      # all settled (the copy)
        self.assertEqual((t.whole, t.rows, t.final, len(t.differ), t.missing, t.skipped), (200, 198, 197, 3, 2, 0))
        t = rc.table_check(per_minute, self.table(), T0, T0 + 199 * M, T0)               # none settled
        self.assertEqual((t.whole, t.rows, t.differ, t.missing, t.skipped, t.skipped_final), (0, 0, (), 0, 200, 197))

    def end_rows(self):
        final = {T0 + k * M: {"minute_ms": str(T0 + k * M), "end_price": "100", "end_trade_ms": str(T0 + k * M + 59_000),
                              "end_mid": None, "end_book_ms": None} for k in (10, 20, 134, 135, 150)}
        ends = {m: {"second_ms": str(m + M), "price": "100", "trade_ms": r["end_trade_ms"], "bid_px": None, "bid_sz": None,
                    "ask_px": None, "ask_sz": None, "book_ms": None} for m, r in final.items()}
        final[T0 + 10 * M].update(end_mid="100.000", end_book_ms=str(T0 + 10 * M + 59_500))
        ends[T0 + 10 * M].update(bid_px="{99.99,99.98}", bid_sz="{1,2}", ask_px="{100.01}", ask_sz="{3}", book_ms=str(T0 + 10 * M + 59_500))
        ends[T0 + 20 * M]["price"] = "101"                    # old: differs
        final[T0 + 135 * M]["end_price"] = "99"                # newer: skipped
        del ends[T0 + 150 * M]                                 # newer, without its end row: skipped
        return final, ends

    def test_end_rows(self):
        final, ends = self.end_rows()
        wrong, compared, with_end, skipped = rc.end_row_check(final, ends, self.NOW)
        self.assertEqual(([w[0] for w in wrong], compared, with_end, skipped), ([T0 + 20 * M], 3, 3, 2))
        self.assertEqual(wrong[0][1:], ((D(100), T0 + 20 * M + 59_000, None, None), (D(101), T0 + 20 * M + 59_000, None, None)))
        wrong, compared, with_end, skipped = rc.end_row_check(final, ends, T0 + 300 * M)
        self.assertEqual(([w[0] for w in wrong], compared, with_end, skipped), ([T0 + 20 * M, T0 + 135 * M, T0 + 150 * M], 5, 4, 0))
        self.assertEqual(rc.end_values(None), (None, None, None, None))
        crossed = dict(ends[T0 + 10 * M], ask_px="{99.99}")
        self.assertEqual(rc.end_values(crossed)[2], None)       # a locked book has no mid

    def test_check_coverage_with_a_pinned_now(self):
        final, ends = self.end_rows()
        perp = [{"seconds": "200", "books": "200", "least_levels": "5", "most_levels": "5"}]
        snap = FakeSnap([("count(price) AS seconds", perp), ("IS NOT NULL", [{"x": "1"}]), ("FROM pg_attribute", [{"x": "1"}]),
                         ("least_levels, most_levels, refreshed_ms FROM", list(self.table().values())),
                         ("end_price, end_trade_ms, end_mid, end_book_ms FROM", list(final.values())),
                         ("second_ms % 60000 = 0", list(ends.values()))])
        db = fake_db(snap, symbols={"bull": "SOXLUSDT"})
        db.coverage_rows = lambda symbol, f, t: iter([(T0 + (k + 1) * M, True, True, 5, 5) for k in range(200)])
        db.call = lambda function, args, order="ts_ms": perp
        for now, failures, skipped in ((self.NOW, 3, "the 65 newer whole minutes skipped, 63 final rows among them"),
                                       (T0 + 300 * M, 6, "the 0 newer whole minutes skipped, 0 final rows among them")):
            report, out = rc.Report(3), io.StringIO()
            with redirect_stdout(out):
                rc.check_coverage(db, report, argparse.Namespace(checks={"coverage"}, print=0), T0, T0 + 200 * M, now)
            self.assertEqual(report.failures, failures, out.getvalue())
            self.assertIn(skipped, out.getvalue())
        self.assertIn("perp_second_minute: 200 whole minutes in the range, 198 rows, 197 final by the watermark, 3 of them differ",
                      out.getvalue())
        self.assertIn("end rows: 5 final rows, 4 with an end row here, 3 differ; 0 final rows newer than now - 66 min skipped", out.getvalue())


if __name__ == "__main__":
    unittest.main()
