#!/usr/bin/env bash
# Gives the trading process its place in the production database: the local
# PostgreSQL on 127.0.0.1:5432, database etfpair (made by the board's
# dashboard/deploy/install.sh, which this script leaves alone together with
# its roles etfpair_writer and etfpair_reader and every table in schema
# etfpair). Idempotent; needs sudo for `sudo -u postgres psql`.
#
#   scripts/install_trade_db.sh
#
# What it sets up is what scripts/dev_db.sh gives the development cluster,
# except that the trading role may only read the recorder's perp_second:
#   - role etfpair_trade (LOGIN): owns schema trade, where the process
#     creates its tables at start; CONNECT on etfpair, USAGE on schema
#     etfpair, SELECT on etfpair.perp_second and nothing else there;
#   - role etfpair_trade_reader (LOGIN, for the board): reads every table in
#     trade, read-only by default, at most 10 connections, 15 s per statement.
#
# Passwords are made once and kept: a later run reads them back from the
# files below and sets the same ones again. They go to psql on stdin, never
# on a command line, and psql's output is discarded, so they reach neither a
# process list nor a log. Nothing secret is printed.
#   - core/.env (JSON, mode 600): store.dsn is set to the trading role's DSN;
#     every other key of the file is kept as it was.
#   - core/.env.reader (JSON, mode 600): {"store": {"dsn": "<reader DSN>"}},
#     the same shape as the store section of .env, for the board's data source.
set -euo pipefail

CORE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ENV_FILE="$CORE/.env"
READER_FILE="$CORE/.env.reader"

# psql as the cluster's superuser over the local socket; SQL on stdin.
pg() { sudo -n -u postgres psql -X -q -v ON_ERROR_STOP=1 "$@"; }

if [ "$(pg -Atc "SELECT 1 FROM pg_database WHERE datname = 'etfpair'")" != "1" ]; then
  echo "database etfpair does not exist; run dashboard/deploy/install.sh first" >&2
  exit 1
fi

# Roles. CREATE ROLE without a password: the passwords are set further down.
pg -d postgres <<'SQL'
SET client_min_messages = warning;
DO $$ BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'etfpair_trade') THEN CREATE ROLE etfpair_trade LOGIN; END IF;
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'etfpair_trade_reader') THEN CREATE ROLE etfpair_trade_reader LOGIN; END IF;
END $$;
ALTER ROLE etfpair_trade LOGIN;
ALTER ROLE etfpair_trade_reader LOGIN CONNECTION LIMIT 10;
ALTER ROLE etfpair_trade_reader SET default_transaction_read_only = on;
ALTER ROLE etfpair_trade_reader SET statement_timeout = '15s';
SQL

# The database: CONNECT is revoked from PUBLIC (the board's install does it
# too; repeating it is harmless), so the two roles are granted it. The trading
# role owns schema trade; the reader sees every table it creates there.
pg -d etfpair <<'SQL'
SET client_min_messages = warning;
REVOKE CONNECT, TEMPORARY ON DATABASE etfpair FROM PUBLIC;
GRANT CONNECT ON DATABASE etfpair TO etfpair_trade, etfpair_trade_reader;
CREATE SCHEMA IF NOT EXISTS trade AUTHORIZATION etfpair_trade;
ALTER SCHEMA trade OWNER TO etfpair_trade;
GRANT USAGE ON SCHEMA trade TO etfpair_trade_reader;
GRANT SELECT ON ALL TABLES IN SCHEMA trade TO etfpair_trade_reader;
ALTER DEFAULT PRIVILEGES FOR ROLE etfpair_trade IN SCHEMA trade GRANT SELECT ON TABLES TO etfpair_trade_reader;
-- The recorder's per-second table, read at a restart to fill missed minutes.
-- Read only: the recorder is its one writer.
GRANT USAGE ON SCHEMA etfpair TO etfpair_trade;
GRANT SELECT ON etfpair.perp_second TO etfpair_trade;
SQL

# Passwords: read back from the two files when they hold a DSN for this
# database, otherwise made new; the files are written (mode 600, replaced in
# one rename) and the ALTER ROLE statements go to psql on stdin. The Python
# runs isolated (-I) and prints SQL only.
python3 -I - "$ENV_FILE" "$READER_FILE" <<'PY' | pg -d postgres >/dev/null 2>&1 \
  || { echo "setting the role passwords failed (output suppressed)" >&2; exit 1; }
import json, os, secrets, string, sys, tempfile
from urllib.parse import urlsplit

env_path, reader_path = sys.argv[1], sys.argv[2]
HOST, PORT, DB = "127.0.0.1", 5432, "etfpair"
ALPHABET = string.ascii_letters + string.digits  # no character a DSN would need escaped

def load(path):
    try:
        with open(path) as f:
            return json.load(f)
    except FileNotFoundError:
        return {}

def kept_password(doc, user):
    """The password of doc's store.dsn when it names user on this database."""
    dsn = (doc.get("store") or {}).get("dsn") or ""
    try:
        u = urlsplit(dsn)
        ok = u.username == user and u.hostname == HOST and u.port == PORT and u.path == "/" + DB
    except ValueError:
        return None
    pw = u.password if ok else None
    return pw if pw and all(c in ALPHABET for c in pw) else None

def write(path, doc):
    fd, tmp = tempfile.mkstemp(dir=os.path.dirname(path), prefix=".tmp-")
    try:
        os.fchmod(fd, 0o600)
        with os.fdopen(fd, "w") as f:
            json.dump(doc, f, indent=2)
            f.write("\n")
        os.replace(tmp, path)
    except BaseException:
        os.unlink(tmp)
        raise

def dsn(user, pw):
    return f"postgres://{user}:{pw}@{HOST}:{PORT}/{DB}?sslmode=disable"

env, reader = load(env_path), load(reader_path)
pw = {}
for user, doc in (("etfpair_trade", env), ("etfpair_trade_reader", reader)):
    pw[user] = kept_password(doc, user) or "".join(secrets.choice(ALPHABET) for _ in range(32))

env.setdefault("store", {})
if not isinstance(env["store"], dict):
    env["store"] = {}
env["store"]["dsn"] = dsn("etfpair_trade", pw["etfpair_trade"])
write(env_path, env)
write(reader_path, {"store": {"dsn": dsn("etfpair_trade_reader", pw["etfpair_trade_reader"])}})

print("SET log_statement = none;")
print("SET log_min_error_statement = panic;")
for user, p in pw.items():
    print(f"ALTER ROLE {user} PASSWORD '{p}';")
PY

echo "trade database ready: roles etfpair_trade and etfpair_trade_reader, schema trade in etfpair"
echo "  $ENV_FILE: store.dsn set (mode 600)"
echo "  $READER_FILE: the reader's DSN (mode 600)"
