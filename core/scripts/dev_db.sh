#!/usr/bin/env bash
# A private PostgreSQL 18 cluster for developing and testing core's store.
#
#   scripts/dev_db.sh start    create the cluster if needed, start it, create the
#                              database, the two roles and the schema (idempotent)
#   scripts/dev_db.sh stop     stop it; the data stays
#   scripts/dev_db.sh status   say whether it runs
#
# The cluster lives under experiments/core_dev_db/ of this repository and
# listens on 127.0.0.1:5441 only. Every command below names that port, so the
# production instance on 5432 is never touched. Authentication is "trust" on
# localhost: this is a throwaway development cluster and holds no real data.
#
# It mirrors what production gives the trading process: database etfpair with
# CONNECT revoked from PUBLIC, role etfpair_trade owning schema trade (the
# process creates its tables there at start), and the read-only role
# etfpair_trade_reader for the board.
set -euo pipefail

PG_BIN=/usr/lib/postgresql/18/bin
PORT=5441
HOST=127.0.0.1
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
DIR="$REPO/experiments/core_dev_db"
DATA="$DIR/data"
LOG="$DIR/postgres.log"

# psql and pg_ctl read these when a flag is missing; set them so that no
# command can fall back to the default port.
export PGHOST="$HOST" PGPORT="$PORT" PGUSER=postgres PGDATABASE=postgres

running() { "$PG_BIN/pg_ctl" -D "$DATA" status >/dev/null 2>&1; }

# sql DATABASE: runs the SQL on stdin as the cluster's superuser.
sql() { "$PG_BIN/psql" -h "$HOST" -p "$PORT" -U postgres -d "$1" -v ON_ERROR_STOP=1 -q -f -; }

start() {
  mkdir -p "$DIR"
  if [ ! -f "$DATA/PG_VERSION" ]; then
    "$PG_BIN/initdb" -D "$DATA" -U postgres --auth-local=trust --auth-host=trust -E UTF8 --locale=C >/dev/null
  fi
  if ! running; then
    "$PG_BIN/pg_ctl" -D "$DATA" -l "$LOG" -w \
      -o "-p $PORT -c listen_addresses=$HOST -c unix_socket_directories=$DIR" start >/dev/null
  fi

  # Roles: the writer the process connects as, and the board's reader.
  sql postgres <<'SQL'
SET client_min_messages = warning;
DO $$ BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'etfpair_trade') THEN CREATE ROLE etfpair_trade LOGIN; END IF;
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'etfpair_trade_reader') THEN CREATE ROLE etfpair_trade_reader LOGIN; END IF;
END $$;
ALTER ROLE etfpair_trade_reader CONNECTION LIMIT 10;
ALTER ROLE etfpair_trade_reader SET default_transaction_read_only = on;
ALTER ROLE etfpair_trade_reader SET statement_timeout = '15s';
SELECT 'CREATE DATABASE etfpair' WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'etfpair') \gexec
SQL

  # The database: nobody connects without an explicit grant; the writer owns
  # schema trade, and the reader sees every table the writer creates in it.
  sql etfpair <<'SQL'
SET client_min_messages = warning;
REVOKE CONNECT, TEMPORARY ON DATABASE etfpair FROM PUBLIC;
GRANT CONNECT ON DATABASE etfpair TO etfpair_trade, etfpair_trade_reader;
CREATE SCHEMA IF NOT EXISTS trade AUTHORIZATION etfpair_trade;
GRANT USAGE ON SCHEMA trade TO etfpair_trade_reader;
GRANT SELECT ON ALL TABLES IN SCHEMA trade TO etfpair_trade_reader;
ALTER DEFAULT PRIVILEGES FOR ROLE etfpair_trade IN SCHEMA trade GRANT SELECT ON TABLES TO etfpair_trade_reader;
SQL

  echo "core dev database is running on $HOST:$PORT (data: $DATA)"
  echo "  export ETFPAIR_TRADE_TEST_DSN=postgres://etfpair_trade@$HOST:$PORT/etfpair"
}

stop() {
  if running; then
    "$PG_BIN/pg_ctl" -D "$DATA" -m fast -w stop >/dev/null
    echo "core dev database stopped"
  else
    echo "core dev database is not running"
  fi
}

status() {
  if running; then
    echo "core dev database is running on $HOST:$PORT (data: $DATA)"
  else
    echo "core dev database is not running"
  fi
}

case "${1:-}" in
  start) start ;;
  stop) stop ;;
  status) status ;;
  *) echo "usage: $0 start|stop|status" >&2; exit 2 ;;
esac
