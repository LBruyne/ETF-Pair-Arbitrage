-- The per-second rows of the Combo Spread board and of backtests, written by dashboard/cmd/booktape from the
-- venues' WebSocket frames and backfilled by dashboard/backfill/perp_second.py. Every statement is idempotent:
-- the recorder applies the file before its first write.
CREATE SCHEMA IF NOT EXISTS etfpair;

CREATE TABLE IF NOT EXISTS etfpair.perp_second (
    venue     text      NOT NULL,
    symbol    text      NOT NULL,
    second_ms bigint    NOT NULL,
    price     numeric,
    trade_ms  bigint,
    bid_px    numeric[],
    bid_sz    numeric[],
    ask_px    numeric[],
    ask_sz    numeric[],
    book_ms   bigint,
    PRIMARY KEY (venue, symbol, second_ms)
);

COMMENT ON TABLE etfpair.perp_second IS
    'One row per perpetual contract and whole second T: the API-executable book levels and the last trade as of T. '
    'The book and the trade columns are each NULL while that kind is not known: before its first event on the '
    'recorder''s connection, after the connection ended, or when no event stamped before T has arrived; no row when '
    'neither is known. History rows from trade archives have trade columns only. The book arrays hold 5 levels a side; a contract whose '
    'price tick is finer than 0.01 (read from the venue at the recorder''s start) holds Bitget books50''s 50 levels (10 until 2026-11-22) or Binance depth10; every '
    'change of the book is in the raw files.';
COMMENT ON COLUMN etfpair.perp_second.venue IS 'bitget or binance';
COMMENT ON COLUMN etfpair.perp_second.symbol IS 'the venue''s contract, e.g. SOXLUSDT';
COMMENT ON COLUMN etfpair.perp_second.second_ms IS 'T, a whole second in Unix milliseconds (T % 1000 = 0)';
COMMENT ON COLUMN etfpair.perp_second.price IS
    'price of the newest trade with exchange time < T, ties broken by the larger trade id (Bitget publicTrade fills, RPI fills included; Binance aggTrade p)';
COMMENT ON COLUMN etfpair.perp_second.trade_ms IS 'exchange time of that trade, Unix ms (Bitget fill T; Binance aggTrade T, the time of its first trade)';
COMMENT ON COLUMN etfpair.perp_second.bid_px IS
    'bid prices an API taker can hit (RPI orders excluded), best first, of the newest book frame with exchange time < T '
    '(Bitget books5, or books50, cut to 10 levels until 2026-11-22: b; Binance depth5: b); {} when that side was empty';
COMMENT ON COLUMN etfpair.perp_second.bid_sz IS 'sizes at those bids, contracts = units of the ETF share, in the order of bid_px';
COMMENT ON COLUMN etfpair.perp_second.ask_px IS 'ask prices of that frame, best first (Bitget a; Binance depth5 a); {} when that side was empty';
COMMENT ON COLUMN etfpair.perp_second.ask_sz IS 'sizes at those asks, in the order of ask_px';
COMMENT ON COLUMN etfpair.perp_second.book_ms IS 'exchange time of that frame, Unix ms (Bitget books5 / books50 data[0].ts; Binance depth5 T)';

-- Every live row is inserted and then updated once (book, then trade), which leaves a dead tuple per row: with the
-- default scale factors autovacuum would wait for about 24 M of them (three weeks). Vacuum and analyze about once a day.
ALTER TABLE etfpair.perp_second SET (autovacuum_vacuum_scale_factor = 0, autovacuum_vacuum_threshold = 1000000,
    autovacuum_vacuum_insert_scale_factor = 0, autovacuum_vacuum_insert_threshold = 1000000,
    autovacuum_analyze_scale_factor = 0, autovacuum_analyze_threshold = 1000000);
