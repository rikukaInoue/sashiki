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
# AppArmor: aa-complain が環境によって効かないことがあるため、
# mysqld プロファイルを disable 登録 + カーネルからアンロードする(両方やる)。
if [ -f /etc/apparmor.d/usr.sbin.mysqld ]; then
  mkdir -p /etc/apparmor.d/disable
  ln -sf /etc/apparmor.d/usr.sbin.mysqld /etc/apparmor.d/disable/ || true
  apparmor_parser -R /etc/apparmor.d/usr.sbin.mysqld 2>&1 || true
fi
aa-complain /usr/sbin/mysqld 2>&1 || true
aa-status 2>/dev/null | grep -i mysqld || echo "apparmor: mysqld profile not loaded (OK)"

# --- 1. クリーンアップ(再実行安全) ---
log "cleanup previous run"
systemctl stop 'mysqld@*' 2>/dev/null || true
pkill -f "twigd --config" 2>/dev/null || true
zpool destroy $POOL 2>/dev/null || true
rm -f "$POOL_IMG"
rm -rf /var/lib/twig /var/log/twig /etc/twig
mkdir -p /var/lib/twig/branches /var/log/twig/hooks /etc/twig/hooks

# --- 2. twig init (zpool/データセット/unit/config を作る) ---
log "twig install + init"
install -m 755 "$TWIGD_BIN" /usr/local/bin/twigd
install -m 755 "$TWIG_BIN" /usr/local/bin/twig
truncate -s 3G "$POOL_IMG"
twig init --pool $POOL --device "$POOL_IMG" --skip-packages --yes
zfs list $POOL/base $POOL/branches > /dev/null || fail "init should create datasets"
# 再実行安全であること(主要ステップがスキップされ成功する)
init2=$(twig init --pool $POOL --skip-packages --yes) || fail "init re-run should succeed"
echo "$init2" | grep -q "スキップ" || fail "init should be idempotent"
# E2E 用にポートレンジと上限を絞る
sed -i 's/port_range: \[3401, 3600\]/port_range: [3401, 3410]/' /etc/twig/config.yaml
sed -i 's/max_branches: 50/max_branches: 5/' /etc/twig/config.yaml

# --- 3. ベースライン: twig baseline import ---
log "twig baseline import"
cat > /tmp/twig-e2e-sample.sql <<'SQL'
CREATE DATABASE app;
CREATE TABLE app.items (id INT PRIMARY KEY AUTO_INCREMENT, name VARCHAR(64));
INSERT INTO app.items (name) VALUES ('alpha'), ('beta'), ('gamma');
SQL
twig baseline import --from /tmp/twig-e2e-sample.sql
zfs list $POOL/base@baseline > /dev/null || fail "baseline snapshot should exist"
# 二重 import は拒否されること
if twig baseline import --from /tmp/twig-e2e-sample.sql 2>/dev/null; then
  fail "second import should fail (baseline exists)"
fi

# --- 4. twigd 起動 ---
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
