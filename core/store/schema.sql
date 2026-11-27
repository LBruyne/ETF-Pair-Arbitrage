-- Tables of the trading process, in schema trade.
--
-- The process applies this file at every start as the writer role
-- (etfpair_trade), in one transaction, so every statement is idempotent. A
-- later change to a table is appended at the end.
--
-- Another process may be writing these tables while this file is applied (a
-- paper run next to a live one). CREATE TABLE IF NOT EXISTS takes no lock on
-- a table that exists, but CREATE INDEX IF NOT EXISTS and ALTER TABLE ... IF
-- NOT EXISTS lock the table even when they change nothing, and two such locks
-- taken in this file's order can deadlock with a writer's transaction. So any
-- statement on an existing table is wrapped in a check that skips it when
-- there is nothing to do, as the indexes at the end are.
-- The schema itself, the two roles and their grants are created by the
-- installer (scripts/dev_db.sh for development): the writer role owns schema
-- trade but may not create schemas in the database.
--
-- Conventions: money, prices and quantities are NUMERIC; times are Unix
-- milliseconds in BIGINT columns named *_ms; statistics of the spread are
-- DOUBLE PRECISION; free-form detail is JSONB. Every table carries run_id (the
-- run that wrote the row, or wrote it last) and live (false = paper). Paper
-- and live rows are never mixed: every primary key holds live or run_id, so
-- the two modes cannot overwrite each other even when their ids are equal
-- (ids are made from the clock, and both modes follow the same market), and
-- every reader filters on live.
--
-- Three write rules (see store.go):
--   state   one row per business object, replaced as a whole on every write
--   fact    written once; a repeat with the same key is ignored
--   sample  a periodic measurement keyed by its time

-- process_run (state): one row per start of the process.
CREATE TABLE IF NOT EXISTS trade.process_run (
    run_id      TEXT PRIMARY KEY,           -- <start in unix ms>-<host>
    live        BOOLEAN NOT NULL,           -- exec.live of this run
    started_ms  BIGINT NOT NULL,            -- when the process started
    stopped_ms  BIGINT,                     -- when it stopped in order; NULL while running or after a crash
    host        TEXT NOT NULL,              -- the machine's host name
    config      JSONB                       -- the effective config.yaml; nothing from the secrets file
);

-- minute_bar (fact): the raw inputs of one finished minute. The same minute
-- holds the same values whichever run writes it, so runs reuse each other's
-- rows; the spread, mean and sigma depend on parameters and are not stored here.
CREATE TABLE IF NOT EXISTS trade.minute_bar (
    live        BOOLEAN NOT NULL,
    minute_ms   BIGINT NOT NULL,            -- start of the minute
    run_id      TEXT NOT NULL,              -- the run that wrote it first
    session_id  TEXT NOT NULL,              -- New York date of the close that starts the session
    mid_bull    DOUBLE PRECISION,           -- bull leg's bid1/ask1 midpoint at the minute's last usable observation, USDT; NULL when invalid
    mid_bear    DOUBLE PRECISION,           -- the same for the bear leg
    f           DOUBLE PRECISION,           -- USDT-USD rate at that observation; NULL when invalid
    valid       BOOLEAN NOT NULL,           -- false: no observation with both legs' depth usable and F fresh
    source      TEXT NOT NULL,              -- own (observed by this process) or perp_second (filled in from the recorder at start)
    PRIMARY KEY (live, minute_ms)
);

-- signal_sample (sample): what the loop saw and why it did not trade, once a second.
CREATE TABLE IF NOT EXISTS trade.signal_sample (
    ts_ms           BIGINT NOT NULL,        -- when the sample was taken
    run_id          TEXT NOT NULL,
    live            BOOLEAN NOT NULL,
    session_id      TEXT NOT NULL,          -- the current session
    session_end_ms  BIGINT,                 -- the regular close that ends it
    phase           TEXT NOT NULL,          -- off_hours, regular or close_window
    anchor_kind     TEXT,                   -- bell or nav; NULL while there is no anchor
    anchor_bull     NUMERIC,                -- bull leg's anchor, USD per share
    anchor_bear     NUMERIC,                -- bear leg's anchor, USD per share
    f               DOUBLE PRECISION,       -- USDT-USD rate in use
    mid_d           DOUBLE PRECISION,       -- the spread at the two midpoints, in spread units
    mu_d            DOUBLE PRECISION,       -- the mean of the spread
    sigma_d         DOUBLE PRECISION,       -- its standard deviation
    x_long_d        DOUBLE PRECISION,       -- executable deviation for buying both legs
    x_short_d       DOUBLE PRECISION,       -- executable deviation for selling both legs
    stats_ready     BOOLEAN NOT NULL,       -- the mean and sigma have their full windows
    idle_reason     TEXT,                   -- why no order was sent this second; NULL when one was
    depth_ok_bull   BOOLEAN NOT NULL,       -- bull leg's depth is usable
    depth_ok_bear   BOOLEAN NOT NULL,
    conflict_bull   BOOLEAN NOT NULL,       -- bull leg's best price and depth disagree
    conflict_bear   BOOLEAN NOT NULL,
    cycle_id        TEXT,                   -- the open cycle; NULL when flat
    direction       TEXT,                   -- long or short; NULL when flat
    hold_bull       NUMERIC NOT NULL,       -- bull leg's quantity held
    hold_bear       NUMERIC NOT NULL,
    quota_left      NUMERIC NOT NULL,       -- notional still available, USDT
    PRIMARY KEY (ts_ms, run_id)
);

-- anchor_fact (fact): every anchor, NAV and dividend the process read, by when it learned it.
CREATE TABLE IF NOT EXISTS trade.anchor_fact (
    live        BOOLEAN NOT NULL,
    session_id  TEXT NOT NULL,              -- the session the fact belongs to
    kind        TEXT NOT NULL,              -- bell_f, bell, nav_seen, nav or dividend
    leg         TEXT NOT NULL,              -- bull or bear
    known_ms    BIGINT NOT NULL,            -- when the process learned it
    run_id      TEXT NOT NULL,
    value       NUMERIC NOT NULL,           -- USD per share
    f           NUMERIC,                    -- the USDT-USD rate inside a bell anchor; NULL for the other kinds
    source      TEXT NOT NULL,              -- where it was read: binance, bitget or direxion
    detail      JSONB,                      -- the raw inputs: index price, its minute, trade date, ex-date
    PRIMARY KEY (live, session_id, kind, leg, known_ms)
);

-- cycle (state): one round from flat to flat.
CREATE TABLE IF NOT EXISTS trade.cycle (
    cycle_id          TEXT NOT NULL,        -- "c" + unix ms of its creation
    run_id            TEXT NOT NULL,        -- the run that wrote it last
    live              BOOLEAN NOT NULL,
    session_id        TEXT NOT NULL,        -- the session it was opened in
    direction         TEXT NOT NULL,        -- long (both legs bought) or short
    status            TEXT NOT NULL,        -- open, closed or takeover
    end_reason        TEXT,                 -- exit_line, close_window, stop, session_changed, no_fill (with the thin leg first also: its batches ended entry_failed), one_leg (also: unwound_profit or unwound_stop), dust_only (built, no exit order: what was built, after any trims, was left as dust only) or store_error (its row was not confirmed written, so it never traded); NULL while open
    pair_anchor_kind  TEXT NOT NULL,        -- the anchors that size all of its lots: their kind
    pair_anchor_bull  NUMERIC NOT NULL,     -- and their two values, USD per share
    pair_anchor_bear  NUMERIC NOT NULL,
    opened_ms         BIGINT NOT NULL,
    closed_ms         BIGINT,               -- NULL while open
    gross             NUMERIC NOT NULL,     -- realised before fees, USDT
    fees              NUMERIC NOT NULL,     -- trading fees paid, USDT
    funding           NUMERIC NOT NULL,     -- funding booked to it, USDT
    funding_known     BOOLEAN NOT NULL,     -- false while a leg it held through a settlement has a non-zero or unpublished rate and no record yet
    funding_awaited   JSONB,                -- a finished cycle's settlements and legs still without a record: [{"settlement_ms": ..., "leg": "bull"}]; NULL when none
    net               NUMERIC NOT NULL,     -- realised after fees and funding, USDT
    dust_bull         NUMERIC NOT NULL,     -- signed quantity it left on the bull leg because no order could be sent for it; recorded as dust, not traded
    dust_bear         NUMERIC NOT NULL,
    PRIMARY KEY (live, cycle_id)
);

-- lot (state): one entry batch and what has closed it. The amounts are
-- results; the orders are the evidence they are recomputed from.
CREATE TABLE IF NOT EXISTS trade.lot (
    lot_id            TEXT NOT NULL,
    run_id            TEXT NOT NULL,        -- the run that wrote it last
    live              BOOLEAN NOT NULL,
    cycle_id          TEXT NOT NULL,
    seq               INTEGER NOT NULL,     -- its order within the cycle
    status            TEXT NOT NULL,        -- building, open, closed or void
    -- end_state (a thin-leg-first batch's end) is added at the end of this file
    void_reason       TEXT,                 -- no_fill, one_leg or store_error (its row was not confirmed written, so its orders were never sent); with the thin leg first entry_failed, unwound_profit or unwound_stop (its end state); NULL unless void
    direction         TEXT NOT NULL,
    x_d               DOUBLE PRECISION,     -- executable deviation at the decision, in spread units
    mu_d              DOUBLE PRECISION,     -- the mean at the decision
    sigma_d           DOUBLE PRECISION,     -- the sigma at the decision
    fee_d             DOUBLE PRECISION,     -- round-trip fee at the planned prices, in spread units
    fee_usdt_est      NUMERIC NOT NULL,     -- the same for the planned sizes, USDT
    entry_qty_bull    NUMERIC NOT NULL,     -- filled on entry and completion
    entry_value_bull  NUMERIC NOT NULL,     -- its traded value, USDT
    entry_fee_bull    NUMERIC NOT NULL,
    exit_qty_bull     NUMERIC NOT NULL,     -- closed by trims and exits
    exit_value_bull   NUMERIC NOT NULL,
    exit_fee_bull     NUMERIC NOT NULL,
    entry_qty_bear    NUMERIC NOT NULL,
    entry_value_bear  NUMERIC NOT NULL,
    entry_fee_bear    NUMERIC NOT NULL,
    exit_qty_bear     NUMERIC NOT NULL,
    exit_value_bear   NUMERIC NOT NULL,
    exit_fee_bear     NUMERIC NOT NULL,
    opened_ms         BIGINT NOT NULL,      -- when the batch was opened
    closed_ms         BIGINT,               -- NULL until closed or void
    gross             NUMERIC NOT NULL,     -- realised before fees, USDT
    fees              NUMERIC NOT NULL,     -- exit fees plus the entry fees of the quantity closed
    net               NUMERIC NOT NULL,     -- realised after fees, USDT
    PRIMARY KEY (live, lot_id)
);

-- cex_order (state): one order as the process knows it; written before the
-- order is sent and on every change.
CREATE TABLE IF NOT EXISTS trade.cex_order (
    client_order_id      TEXT NOT NULL,     -- made by the process; the idempotency key
    run_id               TEXT NOT NULL,     -- the run that wrote it last
    live                 BOOLEAN NOT NULL,
    cycle_id             TEXT NOT NULL,
    lot_id               TEXT,              -- the entry batch it is booked on, fixed once: sent with it, or assigned when an order recognised from the venue was first booked; NULL for exits and forced exits
    leg                  TEXT NOT NULL,     -- bull or bear
    symbol               TEXT NOT NULL,
    purpose              TEXT NOT NULL,     -- entry, complete, trim, exit or forced
    side                 TEXT NOT NULL,     -- buy or sell
    reduce_only          BOOLEAN NOT NULL,
    price                NUMERIC NOT NULL,  -- the limit price
    qty                  NUMERIC NOT NULL,
    quota_price          NUMERIC,           -- the price its notional is reserved at: the limit for a buy, the decision's raw best bid for a sell; NULL reserves at the limit
    status               TEXT NOT NULL,     -- pending, acked, filled, cancelled, rejected, not_sent or never_arrived
    unknown              BOOLEAN NOT NULL,  -- the outcome is not known yet and is being queried
    order_id             TEXT,              -- the venue's id; NULL until acknowledged
    cum_exec_qty         NUMERIC NOT NULL,
    cum_exec_value       NUMERIC NOT NULL,
    avg_price            NUMERIC,           -- NULL without a fill
    fee                  NUMERIC NOT NULL,  -- positive = paid, USDT; what has been reported so far
    fee_known            BOOLEAN NOT NULL,  -- the venue has reported the fee of everything filled; an order without a fill needs none
    not_found            BOOLEAN NOT NULL,  -- unknown for 30 s, the venue says there is no such order, and no fill carries its id
    cancel_reason        TEXT,
    error_code           TEXT,
    error_message        TEXT,
    unknown_since_ms     BIGINT,            -- when the outcome became unknown
    submitted_ms         BIGINT NOT NULL,   -- local clock: registered
    sent_ms              BIGINT,            -- local clock: handed to the venue
    acked_ms             BIGINT,            -- local clock: the venue accepted it
    update_received_ms   BIGINT,            -- local clock: the last update arrived
    exchange_updated_ms  BIGINT,            -- the venue's clock: its last update
    -- three more times of the order's way are added at the end of this file:
    -- accepted_ms, exchange_created_ms, final_ms
    PRIMARY KEY (live, client_order_id)
);

-- cex_fill (fact): one execution as the venue reported it.
CREATE TABLE IF NOT EXISTS trade.cex_fill (
    exec_id          TEXT NOT NULL,         -- the venue's execution id
    run_id           TEXT NOT NULL,
    live             BOOLEAN NOT NULL,
    client_order_id  TEXT NOT NULL,
    order_id         TEXT NOT NULL,
    symbol           TEXT NOT NULL,
    side             TEXT NOT NULL,
    price            NUMERIC NOT NULL,
    qty              NUMERIC NOT NULL,
    value            NUMERIC NOT NULL,      -- price x qty as the venue reports it, USDT
    fee              NUMERIC NOT NULL,      -- positive = paid, USDT
    exec_ms          BIGINT NOT NULL,       -- the venue's clock
    received_ms      BIGINT NOT NULL,       -- local clock
    PRIMARY KEY (live, exec_id)
);

-- decision (fact): every decision of the loop, numbered by the store within a
-- run; in paper mode also one row per matched paper order, beside the decision
-- that sent it.
CREATE TABLE IF NOT EXISTS trade.decision (
    run_id     TEXT NOT NULL,
    seq        BIGINT NOT NULL,             -- one sequence per run, shared with event
    live       BOOLEAN NOT NULL,
    ts_ms      BIGINT NOT NULL,
    cycle_id   TEXT,
    lot_id     TEXT,
    kind       TEXT NOT NULL,               -- entry, exit, complete, trim, forced or at_line_blocked; with the thin leg first also pair (a step of an entry batch in pairing) and exit_pair (a step of a line exit batch's other leg), the step in detail.action; paper_match: a paper order as matched (written by cex)
    direction  TEXT,
    reason     TEXT,
    detail     JSONB,                       -- the numbers needed to recompute the decision; of a paper_match, the order, its result and the leg's book when it was taken and when it was matched
    PRIMARY KEY (run_id, seq)
);

-- funding (fact): one funding record of the two contracts.
CREATE TABLE IF NOT EXISTS trade.funding (
    record_id      TEXT NOT NULL,           -- the venue's record id
    run_id         TEXT NOT NULL,
    live           BOOLEAN NOT NULL,
    symbol         TEXT NOT NULL,
    type           TEXT NOT NULL,           -- the venue's record type
    amount         NUMERIC NOT NULL,        -- as the venue reports it, USDT
    ts_ms          BIGINT NOT NULL,         -- the record's time
    settlement_ms  BIGINT NOT NULL,         -- the settlement it belongs to
    cycle_id       TEXT,                    -- the cycle whose interval holds that settlement; NULL when none does
    PRIMARY KEY (live, record_id)
);

-- inventory_sample (sample): the exchange's positions against the ledger, every 10 seconds.
CREATE TABLE IF NOT EXISTS trade.inventory_sample (
    ts_ms        BIGINT NOT NULL,
    run_id       TEXT NOT NULL,
    live         BOOLEAN NOT NULL,
    pos_bull     NUMERIC,                   -- the exchange's position, signed; NULL while it has not been read
    pos_bear     NUMERIC,
    ledger_bull  NUMERIC NOT NULL,          -- the ledger's position including dust, signed
    ledger_bear  NUMERIC NOT NULL,
    r            NUMERIC NOT NULL,          -- realised total R, USDT
    quota_total  NUMERIC NOT NULL,
    quota_used   NUMERIC NOT NULL,
    mismatch     BOOLEAN NOT NULL,          -- positions and ledger disagree; no orders until a person checks
    PRIMARY KEY (ts_ms, run_id)
);

-- inventory_state (state): what the ledger carries from one cycle to the
-- next; one row for paper and one for live.
CREATE TABLE IF NOT EXISTS trade.inventory_state (
    live      BOOLEAN PRIMARY KEY,
    run_id    TEXT NOT NULL,                -- the run that wrote it last
    capital   NUMERIC NOT NULL,             -- the configured capital r_closed was accumulated under
    since_ms  BIGINT NOT NULL,              -- when this ledger started: at its first start and again when the capital changed; funding settled before it is not booked
    r_closed  NUMERIC NOT NULL,             -- net of all finished cycles plus booked funding, USDT
    dust      JSONB                         -- per leg (keys bull, bear; only legs with dust): qty, the net signed quantity cycles left unsendable, and avg_price, its entry average price; recorded only
);

-- event (fact): alarms and state changes of every module, numbered by the store within a run.
CREATE TABLE IF NOT EXISTS trade.event (
    run_id   TEXT NOT NULL,
    seq      BIGINT NOT NULL,               -- one sequence per run, shared with decision
    live     BOOLEAN NOT NULL,
    ts_ms    BIGINT NOT NULL,
    module   TEXT NOT NULL,                 -- the module that wrote it
    level    TEXT NOT NULL,                 -- severity
    kind     TEXT NOT NULL,
    message  TEXT NOT NULL,
    detail   JSONB,
    PRIMARY KEY (run_id, seq)
);

-- Indexes for reading a cycle's lots and orders. Created only when missing,
-- without touching the table otherwise (see the note at the top).
DO $$ BEGIN
    IF to_regclass('trade.lot_cycle_idx') IS NULL THEN
        CREATE INDEX lot_cycle_idx ON trade.lot (live, cycle_id);
    END IF;
    IF to_regclass('trade.cex_order_cycle_idx') IS NULL THEN
        CREATE INDEX cex_order_cycle_idx ON trade.cex_order (live, cycle_id);
    END IF;
    -- For the Live Monitor's per-round lookups (dashboard/SPEC.md 7, 4.4.1);
    -- the tables are small (26k decisions on 2026-11-27), so building them at
    -- the start takes well under a second.
    IF to_regclass('trade.cex_order_lot_idx') IS NULL THEN
        CREATE INDEX cex_order_lot_idx ON trade.cex_order (live, lot_id);
    END IF;
    IF to_regclass('trade.decision_cycle_idx') IS NULL THEN
        CREATE INDEX decision_cycle_idx ON trade.decision (live, cycle_id);
    END IF;
    IF to_regclass('trade.decision_lot_idx') IS NULL THEN
        CREATE INDEX decision_lot_idx ON trade.decision (live, lot_id);
    END IF;
    IF to_regclass('trade.decision_ts_idx') IS NULL THEN
        CREATE INDEX decision_ts_idx ON trade.decision (live, ts_ms);
    END IF;
    IF to_regclass('trade.event_ts_idx') IS NULL THEN
        CREATE INDEX event_ts_idx ON trade.event (live, ts_ms);
    END IF;
END $$;

-- cex_order: the rest of an order's timestamps, for measuring where its time
-- goes. With submitted_ms, sent_ms, acked_ms (local clock) and
-- exchange_updated_ms (the venue's clock) they give: sent -> the venue's
-- gateway took it -> the venue created the order -> its last change, and
-- locally sent -> acceptance in hand -> final state in hand. Added only when
-- missing (see the note at the top).
DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = 'trade' AND table_name = 'cex_order' AND column_name = 'final_ms') THEN
        ALTER TABLE trade.cex_order
            ADD COLUMN accepted_ms         BIGINT,  -- the venue's clock: its gateway took the request
            ADD COLUMN exchange_created_ms BIGINT,  -- the venue's clock: it created the order
            ADD COLUMN final_ms            BIGINT;  -- local clock: the final state arrived
    END IF;
END $$;

-- funding_rate (fact): every settled funding rate the process read of the
-- two contracts, public data of both paper and live. Recorded for the
-- board only (owner item 61); nothing reads it back.
CREATE TABLE IF NOT EXISTS trade.funding_rate (
    live           BOOLEAN NOT NULL,
    symbol         TEXT NOT NULL,
    settlement_ms  BIGINT NOT NULL,         -- the settlement the rate was applied at
    run_id         TEXT NOT NULL,           -- the run that read it first
    leg            TEXT NOT NULL,           -- bull or bear
    rate           NUMERIC NOT NULL,        -- the settled rate as the venue publishes it, signed
    PRIMARY KEY (live, symbol, settlement_ms)
);

-- signal_sample: each leg's midpoint, so the board can value an open cycle
-- in paper mode. Recorded for the board only (owner item 61). Added only
-- when missing (see the note at the top).
DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = 'trade' AND table_name = 'signal_sample' AND column_name = 'mid_bear') THEN
        ALTER TABLE trade.signal_sample
            ADD COLUMN mid_bull DOUBLE PRECISION,  -- bull leg's bid1/ask1 midpoint at the sample, USDT; NULL while it has no best bid and ask
            ADD COLUMN mid_bear DOUBLE PRECISION;  -- the same for the bear leg
    END IF;
END $$;

-- inventory_sample: what the venue reports beside the positions, for the
-- board only (owner item 61): the program takes no decision from any of it
-- (PRD 6.6). NULL when the venue did not report it, and always NULL for the
-- account's equity and margin ratio in paper mode, which reads no private
-- endpoint. Added only when missing (see the note at the top).
DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = 'trade' AND table_name = 'inventory_sample' AND column_name = 'margin_ratio') THEN
        ALTER TABLE trade.inventory_sample
            ADD COLUMN mark_bull    NUMERIC,  -- bull leg's mark price as the venue reports it with the position, USDT
            ADD COLUMN mark_bear    NUMERIC,
            ADD COLUMN upl_bull     NUMERIC,  -- bull leg's unrealised PnL as the venue reports it, USDT
            ADD COLUMN upl_bear     NUMERIC,
            ADD COLUMN liq_bull     NUMERIC,  -- bull leg's liquidation price as the venue reports it, USDT
            ADD COLUMN liq_bear     NUMERIC,
            ADD COLUMN equity       NUMERIC,  -- the whole account's equity, USDT (Bitget usdtEquity); the account also holds other programs' positions
            ADD COLUMN margin_ratio NUMERIC;  -- the whole account's margin ratio as the venue reports it (Bitget mgnRatio: maintenance margin / equity, cross margin); account-wide like equity
    END IF;
END $$;

-- lot: the end state of a thin-leg-first entry batch (owner items 89-95,
-- core/SPEC.md 12.4): built or built_partial for a lot that became a
-- position, entry_failed, unwound_profit or unwound_stop for a void one
-- (the same value as its void_reason). NULL while the batch is building and
-- for a batch whose two legs went together. Added only when missing (see
-- the note at the top).
DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = 'trade' AND table_name = 'lot' AND column_name = 'end_state') THEN
        ALTER TABLE trade.lot
            ADD COLUMN end_state TEXT;  -- built, built_partial, entry_failed, unwound_profit or unwound_stop; NULL while building and without a thin leg first
    END IF;
END $$;

-- exit_stage (state): the half target H of exit1 of a cycle (core/SPEC.md
-- 12.6, PRD 9.11): set once while closing the whole round at exit1 would
-- make a profit, closed towards by the exit1 batches, read back at a restart.
-- A batch built after it makes it void (built_lots no longer matches); the
-- next H replaces the row.
CREATE TABLE IF NOT EXISTS trade.exit_stage (
    live         BOOLEAN NOT NULL,
    cycle_id     TEXT NOT NULL,
    run_id       TEXT NOT NULL,             -- the run that wrote it last
    set_ms       BIGINT NOT NULL,           -- when H was set
    built_lots   INTEGER NOT NULL,          -- how many batches of the cycle were built when H was set
    target_bull  NUMERIC NOT NULL,          -- H per leg: half the thin leg's holding, the other leg by the pairing anchors
    target_bear  NUMERIC NOT NULL,
    closed_bull  NUMERIC NOT NULL,          -- what the exit1 batches closed towards H so far
    closed_bear  NUMERIC NOT NULL,
    done_ms      BIGINT,                    -- when H was closed in full; NULL before
    PRIMARY KEY (live, cycle_id)
);
