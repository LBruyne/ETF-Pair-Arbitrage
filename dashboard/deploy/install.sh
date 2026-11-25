#!/usr/bin/env bash
# Publishes the ETF-Pair boards on this host: the database etfpair and its
# roles in the local PostgreSQL, the collectors pairscan and booktape as
# systemd services,
# and the data sources (etfpair; etfpair-trade for the Live Monitor, whose
# role core/scripts/install_trade_db.sh makes), the boards and their addresses in the Grafana and
# nginx that already serve the other boards (this module has none of its
# own). Idempotent; re-run after changing the collector or a board. Needs
# sudo for the PostgreSQL roles, systemd and nginx.
#
# The shared Grafana is restarted only when this module's data sources,
# provider or password file changed, and those are taken back if it does
# not come up; nginx is reloaded only when the address rules changed.
set -euo pipefail
ROOT=/home/ubuntu/code/etf-pair-arbitrage
REPO=$ROOT/dashboard
GRAFANA_PROVISIONING=/home/ubuntu/code/dex-cex-arbitrage/dashboard/grafana/provisioning
GRAFANA_UNIT=dashboard-grafana.service
UNIT=etf-pair-pairscan.service

mkdir -p "$REPO/run"
[ -f "$REPO/config.yaml" ] || { cp "$REPO/config.example.yaml" "$REPO/config.yaml"; echo "wrote $REPO/config.yaml from the example"; }

# Secrets live in dashboard/.env (mode 600); a key is written once and kept.
# Grafana gets only the read-only role's password, in dashboard/grafana.env.
touch "$REPO/.env" && chmod 600 "$REPO/.env"
secret() { head -c 48 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 28; }
env_get() { grep -m1 "^$1=" "$REPO/.env" | cut -d= -f2- || true; }
env_set_once() { grep -q "^$1=" "$REPO/.env" || { echo "$1=$2" >> "$REPO/.env"; echo "wrote $1 to $REPO/.env"; }; }
env_set_once ETFPAIR_DB_WRITER_PASSWORD "$(secret)"
env_set_once ETFPAIR_DB_READER_PASSWORD "$(secret)"
w=$(env_get ETFPAIR_DB_WRITER_PASSWORD); r=$(env_get ETFPAIR_DB_READER_PASSWORD)
env_set_once ETFPAIR_BOARD_DSN "postgres://etfpair_writer:$w@127.0.0.1:5432/etfpair?sslmode=disable"

# Roles: the collector owns and writes the database, Grafana reads it
# (read-only, bounded). Passwords go in on stdin and the output is
# discarded, so they reach neither a command line nor a log.
sudo -u postgres psql -X -q -v ON_ERROR_STOP=1 >/dev/null 2>&1 <<SQL || { echo "creating or updating the database roles failed (psql output suppressed)" >&2; exit 1; }
SET log_min_error_statement = panic;
SET log_statement = none;
DO \$\$ BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'etfpair_writer') THEN CREATE ROLE etfpair_writer LOGIN; END IF;
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'etfpair_reader') THEN CREATE ROLE etfpair_reader LOGIN; END IF;
END \$\$;
ALTER ROLE etfpair_writer PASSWORD '$w';
ALTER ROLE etfpair_reader PASSWORD '$r';
ALTER ROLE etfpair_reader CONNECTION LIMIT 10;
ALTER ROLE etfpair_reader SET default_transaction_read_only = on;
ALTER ROLE etfpair_reader SET statement_timeout = '15s';
SQL
if [ "$(sudo -u postgres psql -X -Atc "SELECT 1 FROM pg_database WHERE datname = 'etfpair'")" != "1" ]; then
  sudo -u postgres createdb -O etfpair_writer -T template0 --locale=C --encoding=UTF8 etfpair
  echo "created the database etfpair"
fi
# The schemas are the recorder's and the collector's own files, the recorder's first (the board's functions read its
# perp_second); the reader may read what they hold.
PGPASSWORD=$w psql -X -q -v ON_ERROR_STOP=1 -h 127.0.0.1 -U etfpair_writer -d etfpair -f "$REPO/internal/booktape/schema.sql" \
  -f "$REPO/internal/scan/schema.sql" 2>&1 | { grep -v NOTICE || true; }
sudo -u postgres psql -X -q -v ON_ERROR_STOP=1 -d etfpair <<'SQL'
REVOKE CONNECT, TEMPORARY ON DATABASE etfpair FROM PUBLIC;
GRANT CONNECT ON DATABASE etfpair TO etfpair_reader;
GRANT USAGE ON SCHEMA etfpair TO etfpair_reader;
GRANT SELECT ON ALL TABLES IN SCHEMA etfpair TO etfpair_reader;
ALTER DEFAULT PRIVILEGES FOR ROLE etfpair_writer IN SCHEMA etfpair GRANT SELECT ON TABLES TO etfpair_reader;
SQL
(umask 077 && echo "ETFPAIR_DB_READER_PASSWORD=$r" > "$REPO/grafana.env.new")
# The Live Monitor's data source etfpair-trade reads schema trade as etfpair_trade_reader, a role that
# core/scripts/install_trade_db.sh creates; its password is the one in core/.env.reader (JSON), copied
# into grafana.env without passing through a command line or the terminal.
if [ -f "$ROOT/core/.env.reader" ]; then
  (umask 077 && python3 -I -c 'import json, sys, urllib.parse as u
try:
    pw = u.unquote(u.urlparse(json.load(open(sys.argv[1]))["store"]["dsn"]).password or "")
except Exception:
    pw = ""
if pw:
    print("ETFPAIR_TRADE_READER_PASSWORD=" + pw)
else:
    sys.stderr.write("core/.env.reader holds no store.dsn with a password: the Live Monitor data source will not connect\n")' \
    "$ROOT/core/.env.reader" >> "$REPO/grafana.env.new")
else
  echo "core/.env.reader is missing: the Live Monitor's data source will not connect (run core/scripts/install_trade_db.sh)" >&2
fi

# Build first; nothing running is touched until the binaries and the boards exist.
(cd "$ROOT" && python3 dashboard/grafana/gen_dashboard.py >/dev/null \
  && go build -buildvcs=false -o dashboard/run/pairscan.new ./dashboard/cmd/pairscan \
  && go build -buildvcs=false -o dashboard/run/booktape.new ./dashboard/cmd/booktape)

# install_service NAME: the binary dashboard/run/NAME.new and the unit
# etf-pair-NAME.service go in place; the service is restarted only when its
# binary or its unit changed, or when it is not running.
install_service() {
  local name=$1 unit="etf-pair-$1.service" changed=0
  cmp -s "$REPO/run/$name.new" "$REPO/run/$name" 2>/dev/null || changed=1
  cmp -s "$REPO/deploy/$unit" "/etc/systemd/system/$unit" 2>/dev/null || changed=1
  if [ "$changed" = 1 ]; then
    mv "$REPO/run/$name.new" "$REPO/run/$name"
    sudo cp "$REPO/deploy/$unit" /etc/systemd/system/
    sudo systemctl daemon-reload
  else
    rm -f "$REPO/run/$name.new"
  fi
  touch "$REPO/run/$name.log"
  sudo systemctl enable "$unit" >/dev/null 2>&1
  if [ "$changed" = 1 ] || ! systemctl is-active --quiet "$unit"; then
    sudo systemctl restart "$unit"
  else
    echo "$name binary and unit unchanged; $name not restarted"
  fi
}
install_service pairscan
install_service booktape

# The boards Grafana serves: a copy of the generated ones.
mkdir -p "$REPO/grafana/dashboards/published"
rsync -a --delete "$REPO/grafana/dashboards/staging/" "$REPO/grafana/dashboards/published/"

# Grafana: this module's data sources and board provider go next to the other
# modules', and its unit also reads this module's grafana.env.
DROPIN_DIR="/etc/systemd/system/$GRAFANA_UNIT.d"
BACKUP=$(mktemp -d)
trap 'rm -rf "$BACKUP"' EXIT
GRAFANA_CHANGED=0
# place SRC DST [sudo]: copy SRC over DST when they differ, keeping what was there for a rollback.
place() {
  if ! cmp -s "$1" "$2" 2>/dev/null; then
    if [ -e "$2" ]; then cp "$2" "$BACKUP/$(basename "$2")"; else touch "$BACKUP/$(basename "$2").absent"; fi
    ${3:-} cp "$1" "$2"
    GRAFANA_CHANGED=1
  fi
}
restore() {
  local name; name=$(basename "$1")
  if [ -e "$BACKUP/$name" ]; then ${2:-} cp "$BACKUP/$name" "$1"; elif [ -e "$BACKUP/$name.absent" ]; then ${2:-} rm -f "$1"; fi
}
cmp -s "$REPO/grafana.env.new" "$REPO/grafana.env" 2>/dev/null || GRAFANA_CHANGED=1
mv "$REPO/grafana.env.new" "$REPO/grafana.env"
sudo mkdir -p "$DROPIN_DIR"
place "$REPO/grafana/provisioning/datasources/etfpair.yml" "$GRAFANA_PROVISIONING/datasources/etfpair.yml"
place "$REPO/grafana/provisioning/datasources/etfpair-trade.yml" "$GRAFANA_PROVISIONING/datasources/etfpair-trade.yml"
place "$REPO/grafana/provisioning/dashboards/etf-pair.yml" "$GRAFANA_PROVISIONING/dashboards/etf-pair.yml"
place "$REPO/deploy/grafana-etf-pair.conf" "$DROPIN_DIR/etf-pair.conf" sudo
sudo systemctl daemon-reload
grafana_up() {
  local port; port=$(awk -F= '/^http_port/{gsub(/ /, "", $2); print $2}' "$(dirname "$GRAFANA_PROVISIONING")/grafana.ini")
  for _ in $(seq 1 30); do
    if curl -fsS -o /dev/null "http://127.0.0.1:${port:-3000}/api/health" 2>/dev/null; then return 0; fi
    sleep 1
  done
  return 1
}
if [ "$GRAFANA_CHANGED" = 1 ]; then
  sudo systemctl restart "$GRAFANA_UNIT"
  if ! grafana_up; then
    echo "Grafana did not come up with this module's files; taking them back" >&2
    restore "$GRAFANA_PROVISIONING/datasources/etfpair.yml"
    restore "$GRAFANA_PROVISIONING/datasources/etfpair-trade.yml"
    restore "$GRAFANA_PROVISIONING/dashboards/etf-pair.yml"
    restore "$DROPIN_DIR/etf-pair.conf" sudo
    sudo systemctl daemon-reload
    sudo systemctl restart "$GRAFANA_UNIT"
    exit 1
  fi
  echo "Grafana restarted with the etfpair and etfpair-trade data sources and the etf-pair provider"
else
  echo "Grafana files unchanged; Grafana not restarted"
fi

# Addresses: the rules that redirect /dashboards/arbitrage/etf-pair/<board>
# go next to the other modules'; nginx reads every dashboard-board-paths*.conf.
RULES=/etc/nginx/dashboard-board-paths-etf-pair.conf
if ! sudo cmp -s "$REPO/grafana/nginx-board-paths.conf" "$RULES"; then
  BEFORE="$RULES.before"   # not read by nginx: it includes *.conf only
  sudo rm -f "$BEFORE"
  if sudo test -f "$RULES"; then sudo cp -p "$RULES" "$BEFORE"; fi
  sudo install -o root -g root -m 644 "$REPO/grafana/nginx-board-paths.conf" "$RULES"
  if sudo nginx -t 2>/dev/null; then
    sudo rm -f "$BEFORE"
    sudo systemctl reload nginx
    echo "board address rules installed; nginx reloaded"
  else
    if sudo test -f "$BEFORE"; then sudo mv "$BEFORE" "$RULES"; else sudo rm -f "$RULES"; fi
    echo "nginx refuses its configuration with the new rules; the earlier state is back (run: sudo nginx -t)" >&2
    exit 1
  fi
fi

sleep 3
systemctl --no-pager --lines=0 status "$UNIT" "$GRAFANA_UNIT" | grep -E 'service|Active'
