#!/usr/bin/env bash
# PostgreSQL エンジンの E2E。素の Ubuntu (VM/EC2) 上で root 実行。
# loopback zpool + postgres 16 で create/reset/delete を検証する。
#   usage: sudo ./e2e.sh <twigd> <twig>
set -euo pipefail

TWIGD_BIN=${1:?usage: e2e.sh <twigd> <twig>}
TWIG_BIN=${2:?usage: e2e.sh <twigd> <twig>}
POOL=tpgpool
POOL_IMG=/var/tmp/twig-pg-zpool.img

log() { echo -e "\n=== $* ==="; }
fail() { echo "PG E2E FAILED: $*" >&2; tail -20 /var/log/twig-pg/twigd.log 2>/dev/null; exit 1; }

log "packages"
export DEBIAN_FRONTEND=noninteractive
apt-get update -q > /dev/null
apt-get install -y -q zfsutils-linux postgresql postgresql-client > /dev/null 2>&1
systemctl stop postgresql 2>/dev/null || true
systemctl disable postgresql 2>/dev/null || true
PGBIN=$(ls -d /usr/lib/postgresql/*/bin | sort -V | tail -1)

log "cleanup previous run"
systemctl stop 'postgres-twig@*' 2>/dev/null || true
pkill -f "twigd --config /etc/twig-pg" 2>/dev/null || true
zpool destroy $POOL 2>/dev/null || true
rm -f "$POOL_IMG"
rm -rf /etc/twig-pg /var/lib/twig-pg /var/log/twig-pg
mkdir -p /etc/twig-pg/hooks /var/lib/twig-pg/branches /var/log/twig-pg/hooks

log "zpool (recordsize=8k for postgres)"
truncate -s 3G "$POOL_IMG"
zpool create -o ashift=12 $POOL "$POOL_IMG"
zfs set compression=lz4 atime=off $POOL
zfs create -o recordsize=8k -o logbias=throughput $POOL/base
zfs create $POOL/branches

log "base postgres"
mkdir -p /$POOL/base/data
chown -R postgres:postgres /$POOL/base
chmod 700 /$POOL/base/data
sudo -u postgres $PGBIN/initdb -D /$POOL/base/data --auth-host=trust --auth-local=trust > /dev/null
sudo -u postgres $PGBIN/pg_ctl start -D /$POOL/base/data -o "-p 5499 -c listen_addresses=127.0.0.1 -c unix_socket_directories=/tmp" -w > /dev/null
sudo -u postgres psql -h 127.0.0.1 -p 5499 -d postgres <<'SQL' > /dev/null
CREATE ROLE dev LOGIN SUPERUSER;
CREATE DATABASE app OWNER dev;
SQL
sudo -u postgres psql -h 127.0.0.1 -p 5499 -d app <<'SQL' > /dev/null
CREATE TABLE items (id SERIAL PRIMARY KEY, name TEXT);
INSERT INTO items (name) VALUES ('alpha'), ('beta'), ('gamma');
SQL
# 正常終了してから snapshot を取得する(不変条件)
sudo -u postgres $PGBIN/pg_ctl stop -D /$POOL/base/data -m fast -w > /dev/null
zfs snapshot $POOL/base@baseline

log "install unit + config + start twigd"
install -m 755 "$TWIGD_BIN" /usr/local/bin/twigd-pg
install -m 755 "$TWIG_BIN" /usr/local/bin/twig-pg
SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
UNIT_SRC="$SCRIPT_DIR/../../deploy/systemd/postgres-twig@.service"
[ -f "$UNIT_SRC" ] || UNIT_SRC="$SCRIPT_DIR/postgres-twig@.service"
sed "s|/etc/twig/%i.env|/etc/twig-pg/%i.env|" "$UNIT_SRC" > /etc/systemd/system/postgres-twig@.service
systemctl daemon-reload

cat > /etc/twig-pg/config.yaml <<YAML
listen:
  api: "127.0.0.1:8090"
  proxy: ""
state_db: /var/lib/twig-pg/state.db
storage:
  backend: zfs
  zfs:
    pool: $POOL
    base_dataset: $POOL/base
    branch_parent: $POOL/branches
    baseline_snapshot: baseline
    sudo: false
engine:
  type: postgres
  postgres:
    bin_dir: $PGBIN
    env_dir: /etc/twig-pg
    sudo: false
branches:
  name_pattern: "^[a-z0-9-]{1,32}$"
  max_branches: 5
hooks:
  dir: /etc/twig-pg/hooks
  log_dir: /var/log/twig-pg/hooks
YAML
/usr/local/bin/twigd-pg --config /etc/twig-pg/config.yaml > /var/log/twig-pg/twigd.log 2>&1 &
TWIGD_PID=$!
trap 'kill $TWIGD_PID 2>/dev/null || true' EXIT
for _ in $(seq 1 30); do curl -sf http://127.0.0.1:8090/v1/healthz > /dev/null 2>&1 && break; sleep 0.5; done
curl -sf http://127.0.0.1:8090/v1/healthz > /dev/null || fail "twigd did not start"

export TWIG_API_URL=http://127.0.0.1:8090
q() { sudo -u postgres psql -h 127.0.0.1 -p "$1" -d app -t -A -c "$2" 2>/dev/null; }

log "create pg-1"
time twig-pg create pg-1
PORT=$(twig-pg show pg-1 --json | python3 -c 'import json,sys;print(json.load(sys.stdin)["port"])')
[ "$(q $PORT 'SELECT COUNT(*) FROM items')" = "3" ] || fail "pg-1 should have 3 items"

log "破壊 → reset"
q $PORT "DELETE FROM items" > /dev/null
time twig-pg reset pg-1
[ "$(q $PORT 'SELECT COUNT(*) FROM items')" = "3" ] || fail "reset should restore"
grep -qi "database system was not properly shut down" /$POOL/branches/pg-1/data/log/*.log 2>/dev/null \
  && fail "crash recovery ran (dirty @init)"

log "delete"
time twig-pg delete pg-1
zfs list -r $POOL/branches | grep -q pg- && fail "dataset should be destroyed"

log "cleanup"
kill $TWIGD_PID 2>/dev/null || true
zpool destroy $POOL
rm -f "$POOL_IMG"
echo "PG E2E PASSED"
