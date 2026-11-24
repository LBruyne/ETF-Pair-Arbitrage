#!/usr/bin/env bash
# A private preview of the ETF-Pair boards: its own PostgreSQL cluster, the
# pairscan collector and its own Grafana, all under experiments/dashboard_run
# and all on loopback. It shares nothing with the Grafana, nginx or database
# that serve the other boards: it runs the same Grafana program files with its
# own data and plugin directories and installs no plugin by itself.
#
#   dashboard/deploy/preview.sh start|stop|status
#
# Board:  http://127.0.0.1:3002/d/arb-etf-pair-combo-spread
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
RUN=$ROOT/experiments/dashboard_run
PGBIN=${PGBIN:-/usr/lib/postgresql/18/bin}
GRAFANA_HOME=${GRAFANA_HOME:-/home/ubuntu/dashboard-run/grafana-13.2.2}
# The PostgreSQL data source plugin is copied from here once; this directory is never written.
GRAFANA_PLUGIN_SOURCE=${GRAFANA_PLUGIN_SOURCE:-/home/ubuntu/dashboard-run/plugins}
PG_PORT=5440
GRAFANA_PORT=3002
DSN="postgres://etfpair_owner@127.0.0.1:$PG_PORT/etfpair"
PSQL="psql -h 127.0.0.1 -p $PG_PORT -U etfpair_owner -q -At"

running() { [ -f "$1" ] && kill -0 "$(cat "$1")" 2>/dev/null; }

start_postgres() {
    if [ ! -d "$RUN/pg" ]; then
        # Loopback only and trust authentication: any local user can connect, which
        # is acceptable for market data on a preview and nothing else.
        "$PGBIN/initdb" -D "$RUN/pg" -U etfpair_owner -A trust --locale=C.UTF-8 -E UTF8 >/dev/null
        printf "port = %s\nlisten_addresses = '127.0.0.1'\nunix_socket_directories = '%s'\nmax_connections = 30\n" "$PG_PORT" "$RUN" >> "$RUN/pg/postgresql.conf"
    fi
    "$PGBIN/pg_ctl" -D "$RUN/pg" status >/dev/null 2>&1 || "$PGBIN/pg_ctl" -D "$RUN/pg" -l "$RUN/logs/postgres.log" -w start >/dev/null
    $PSQL -d postgres -c "SELECT 1 FROM pg_database WHERE datname = 'etfpair'" | grep -q 1 || $PSQL -d postgres -c "CREATE DATABASE etfpair"
    $PSQL -d postgres -c "SELECT 1 FROM pg_roles WHERE rolname = 'etfpair_reader'" | grep -q 1 || $PSQL -d postgres -c "CREATE ROLE etfpair_reader LOGIN"
    $PSQL -d etfpair -f "$ROOT/dashboard/internal/booktape/schema.sql" -f "$ROOT/dashboard/internal/scan/schema.sql" 2>/dev/null
    $PSQL -d etfpair -c "GRANT USAGE ON SCHEMA etfpair TO etfpair_reader; GRANT SELECT ON ALL TABLES IN SCHEMA etfpair TO etfpair_reader;
        GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA etfpair TO etfpair_reader; ALTER ROLE etfpair_reader SET statement_timeout = '15s'"
}

start_pairscan() {
    running "$RUN/pairscan.pid" && return
    (cd "$ROOT" && go build -o "$RUN/pairscan" ./dashboard/cmd/pairscan)
    [ -f "$ROOT/dashboard/config.yaml" ] || cp "$ROOT/dashboard/config.example.yaml" "$ROOT/dashboard/config.yaml"
    cd "$ROOT"
    ETFPAIR_BOARD_DSN=$DSN nohup "$RUN/pairscan" -config dashboard/config.yaml >> "$RUN/logs/pairscan.log" 2>&1 &
    echo $! > "$RUN/pairscan.pid"
}

start_grafana() {
    running "$RUN/grafana.pid" && return
    mkdir -p "$RUN/grafana/provisioning/datasources" "$RUN/grafana/provisioning/dashboards" "$RUN/grafana/data" "$RUN/grafana/plugins"
    [ -d "$RUN/grafana/plugins/grafana-postgresql-datasource" ] || cp -r "$GRAFANA_PLUGIN_SOURCE/grafana-postgresql-datasource" "$RUN/grafana/plugins/"
    cat > "$RUN/grafana/provisioning/datasources/etfpair.yml" <<YML
apiVersion: 1
datasources:
  - name: etfpair
    uid: etfpair
    type: grafana-postgresql-datasource
    access: proxy
    isDefault: true
    editable: false
    url: 127.0.0.1:$PG_PORT
    user: etfpair_reader
    jsonData: { database: etfpair, sslmode: disable, maxOpenConns: 10, postgresVersion: 1500 }
YML
    cat > "$RUN/grafana/provisioning/dashboards/etf-pair.yml" <<YML
apiVersion: 1
providers:
  - name: etf-pair
    type: file
    disableDeletion: true
    updateIntervalSeconds: 10
    allowUiUpdates: false
    options:
      path: $ROOT/dashboard/grafana/dashboards/staging
      foldersFromFilesStructure: true
YML
    cat > "$RUN/grafana/grafana.ini" <<INI
[paths]
data = $RUN/grafana/data
logs = $RUN/logs
plugins = $RUN/grafana/plugins
provisioning = $RUN/grafana/provisioning
[server]
http_addr = 127.0.0.1
http_port = $GRAFANA_PORT
root_url = http://127.0.0.1:$GRAFANA_PORT/
[auth.anonymous]
enabled = true
org_role = Viewer
[users]
default_theme = dark
[date_formats]
default_timezone = Etc/GMT-8
[analytics]
reporting_enabled = false
check_for_updates = false
check_for_plugin_updates = false
[news]
news_feed_enabled = false
[plugins]
preinstall_disabled = true
[dashboards]
min_refresh_interval = 10s
INI
    # Grafana writes its own log file under [paths] logs.
    nohup "$GRAFANA_HOME/bin/grafana" server --homepath "$GRAFANA_HOME" --config "$RUN/grafana/grafana.ini" >/dev/null 2>&1 &
    echo $! > "$RUN/grafana.pid"
}

case "${1:-}" in
start)
    mkdir -p "$RUN/logs"
    start_postgres
    start_pairscan
    start_grafana
    echo "board: http://127.0.0.1:$GRAFANA_PORT/d/arb-etf-pair-combo-spread"
    ;;
stop)
    for name in grafana pairscan; do
        running "$RUN/$name.pid" && kill "$(cat "$RUN/$name.pid")" || true
        rm -f "$RUN/$name.pid"
    done
    "$PGBIN/pg_ctl" -D "$RUN/pg" -m fast stop >/dev/null 2>&1 || true
    ;;
status)
    "$PGBIN/pg_ctl" -D "$RUN/pg" status >/dev/null 2>&1 && echo "postgres: running on $PG_PORT" || echo "postgres: stopped"
    for name in pairscan grafana; do
        running "$RUN/$name.pid" && echo "$name: running (pid $(cat "$RUN/$name.pid"))" || echo "$name: stopped"
    done
    ;;
*)
    echo "usage: $0 start|stop|status" >&2
    exit 2
    ;;
esac
