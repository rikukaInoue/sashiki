#!/usr/bin/env bash
# twig E2E: 素の Ubuntu (VM / EC2) 上で root 実行する。
# ループバックファイルの zpool を使うので追加ディスク不要。
#   usage: sudo ./e2e.sh <twigd-binary> <twig-binary>
set -euo pipefail

TWIGD_BIN=${1:?usage: e2e.sh <twigd> <twig>}
TWIG_BIN=${2:?usage: e2e.sh <twigd> <twig>}
POOL=tpool
POOL_IMG=/var/tmp/twig-e2e-zpool.img

log() { echo -e "\n=== $* ==="; }
fail() { echo "E2E FAILED: $*" >&2; exit 1; }

# --- 0. 前提パッケージ ---
log "packages"
export DEBIAN_FRONTEND=noninteractive
apt-get update -q > /dev/null
apt-get install -y -q zfsutils-linux mysql-server-8.0 mysql-client-8.0 apparmor-utils > /dev/null
systemctl stop mysql 2>/dev/null || true
systemctl disable mysql 2>/dev/null || true
aa-complain /usr/sbin/mysqld > /dev/null 2>&1 || true

# --- 1. クリーンアップ(再実行安全) ---
log "cleanup previous run"
systemctl stop 'mysqld@*' 2>/dev/null || true
pkill -f "twigd --config" 2>/dev/null || true
zpool destroy $POOL 2>/dev/null || true
rm -f "$POOL_IMG"
rm -rf /var/lib/twig /var/log/twig /etc/twig
mkdir -p /var/lib/twig/branches /var/log/twig/hooks /etc/twig/hooks

# --- 2. loopback zpool ---
log "zpool (loopback)"
truncate -s 3G "$POOL_IMG"
zpool create -o ashift=12 $POOL "$POOL_IMG"
zfs set compression=lz4 atime=off $POOL
zfs create -o recordsize=16k -o logbias=throughput $POOL/base
zfs create $POOL/branches

# --- 3. ベース MySQL + 小さなサンプルデータ ---
log "base mysql"
mkdir -p /$POOL/base/data
chown -R mysql:mysql /$POOL/base /var/log/twig
sudo -u mysql mysqld --initialize-insecure --datadir=/$POOL/base/data > /dev/null 2>&1
sudo -u mysql mysqld --datadir=/$POOL/base/data --port=3306 \
  --socket=/tmp/mysql-e2e-base.sock --pid-file=/tmp/mysql-e2e-base.pid \
  --log-error=/var/log/twig/base.err --innodb-buffer-pool-size=128M --daemonize
for _ in $(seq 1 60); do
  mysqladmin -uroot -S /tmp/mysql-e2e-base.sock ping > /dev/null 2>&1 && break
  sleep 1
done
mysql -uroot -S /tmp/mysql-e2e-base.sock <<'SQL'
CREATE DATABASE app;
CREATE TABLE app.items (id INT PRIMARY KEY AUTO_INCREMENT, name VARCHAR(64));
INSERT INTO app.items (name) VALUES ('alpha'), ('beta'), ('gamma');
CREATE USER 'dev'@'%' IDENTIFIED BY 'dev';
GRANT ALL PRIVILEGES ON *.* TO 'dev'@'%';
FLUSH PRIVILEGES;
SQL
mysqladmin -uroot -S /tmp/mysql-e2e-base.sock shutdown
sleep 2
zfs snapshot $POOL/base@baseline

# --- 4. twig インストール ---
log "install twig"
install -m 755 "$TWIGD_BIN" /usr/local/bin/twigd
install -m 755 "$TWIG_BIN" /usr/local/bin/twig
SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
if [ -f "$SCRIPT_DIR/../deploy/systemd/mysqld@.service" ]; then
  cp "$SCRIPT_DIR/../deploy/systemd/mysqld@.service" /etc/systemd/system/
else
  # バイナリだけ持ち込むケース(EC2 等)向けに埋め込みで書く
  cat > /etc/systemd/system/mysqld@.service <<'UNIT'
[Unit]
Description=twig MySQL instance %i
After=network.target zfs-mount.service
[Service]
Type=simple
User=mysql
Group=mysql
EnvironmentFile=/etc/twig/%i.env
ExecStart=/usr/sbin/mysqld --datadir=${DATADIR} --port=${PORT} --socket=/tmp/mysql-%i.sock --pid-file=/tmp/mysql-%i.pid --log-error=/var/log/twig/%i.err --innodb-buffer-pool-size=128M --skip-mysqlx
Restart=no
LimitNOFILE=65535
[Install]
WantedBy=multi-user.target
UNIT
fi
systemctl daemon-reload

cat > /etc/twig/config.yaml <<YAML
listen:
  api: "127.0.0.1:8080"
state_db: /var/lib/twig/state.db
storage:
  backend: zfs
  zfs:
    pool: $POOL
    base_dataset: $POOL/base
    branch_parent: $POOL/branches
    baseline_snapshot: baseline
    sudo: false
engine:
  type: mysql
  mysql:
    port_range: [3401, 3410]
    env_dir: /etc/twig
    sudo: false
branches:
  name_pattern: "^[a-z0-9-]{1,32}$"
  max_branches: 5
hooks:
  dir: /etc/twig/hooks
  log_dir: /var/log/twig/hooks
YAML

log "start twigd"
/usr/local/bin/twigd --config /etc/twig/config.yaml > /var/log/twig/twigd.log 2>&1 &
TWIGD_PID=$!
trap 'kill $TWIGD_PID 2>/dev/null || true' EXIT
for _ in $(seq 1 30); do
  curl -sf http://127.0.0.1:8080/v1/healthz > /dev/null 2>&1 && break
  sleep 0.5
done
curl -sf http://127.0.0.1:8080/v1/healthz > /dev/null || fail "twigd did not start: $(tail -5 /var/log/twig/twigd.log)"

# --- 5. シナリオ ---
q() { mysql -udev -pdev -h127.0.0.1 -P"$1" -N -e "$2" 2>/dev/null; }

log "create pr-1"
time twig create pr-1
[ "$(q 3401 'SELECT COUNT(*) FROM app.items')" = "3" ] || fail "pr-1 should have 3 items"

log "create pr-2 (isolation)"
twig create pr-2
q 3401 "DELETE FROM app.items; DROP TABLE app.items" || fail "break pr-1"
[ "$(q 3402 'SELECT COUNT(*) FROM app.items')" = "3" ] || fail "pr-2 must be isolated"

log "reset pr-1"
time twig reset pr-1
[ "$(q 3401 'SELECT COUNT(*) FROM app.items')" = "3" ] || fail "reset should restore 3 items"
if grep -qi "crash recovery" /var/log/twig/pr-1.err; then
  fail "crash recovery ran (dirty @init)"
fi

log "duplicate create must fail (exit 4)"
set +e
twig create pr-1 2>/dev/null
rc=$?
set -e
[ "$rc" -eq 4 ] || fail "duplicate create: exit=$rc, want 4"

log "invalid name must fail"
if twig create "BAD_NAME" 2>/dev/null; then
  fail "invalid name should fail"
fi

log "list"
twig list

log "delete"
twig delete pr-1
twig delete pr-2
zfs list -r $POOL/branches | grep -q pr- && fail "datasets should be destroyed"

# --- 6. 後片付け ---
log "cleanup"
kill $TWIGD_PID 2>/dev/null || true
zpool destroy $POOL
rm -f "$POOL_IMG"

echo -e "\nE2E PASSED"
