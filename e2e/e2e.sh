#!/usr/bin/env bash
# twig E2E: 素の Ubuntu (VM / EC2) 上で root 実行する。
# ループバックファイルの zpool を使うので追加ディスク不要。
#   usage: sudo ./e2e.sh <twigd-binary> <twig-binary>
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
TWIGD_BIN=${1:?usage: e2e.sh <twigd> <twig>}
TWIG_BIN=${2:?usage: e2e.sh <twigd> <twig>}
POOL=tpool
POOL_IMG=/var/tmp/twig-e2e-zpool.img

log() { echo -e "\n=== $* ==="; }
fail() { echo "E2E FAILED: $*" >&2; exit 1; }

# HTTP アサーション用。単発 curl は稀に接続レベルで落ちる(#49)ため
# 3 回まで再試行し、失敗時は HTTP ステータスを stderr に残す。
probe() { # probe <url> [curl-args...] : 本文を stdout へ
  local url=$1; shift
  local i
  for i in 1 2 3; do
    curl -sf "$@" "$url" && return 0
    [ "$i" -lt 3 ] && sleep 1
  done
  echo "probe failed: $url (HTTP $(curl -s -o /dev/null -w '%{http_code}' "$@" "$url" 2>/dev/null))" >&2
  return 1
}

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

log "proxy: dev@<branch> ルーティング"
# 固定ポート 3306 経由で pr-1 に接続できること
val=$(mysql -udev@pr-1 -pdev -h127.0.0.1 -P3306 -N -e "SELECT COUNT(*) FROM app.items" 2>/dev/null) \
  || fail "proxy: connect via dev@pr-1 should work"
[ "$val" = "3" ] || fail "proxy: query result = $val, want 3"
# name_pattern 違反のブランチ名は拒否(lazy create の対象にもならない)
if mysql -udev@Bad_Name -pdev -h127.0.0.1 -P3306 -e "SELECT 1" 2>/dev/null; then
  fail "proxy: invalid branch name should be rejected"
fi
# パスワード誤りはバックエンドが拒否
if mysql -udev@pr-1 -pWRONG -h127.0.0.1 -P3306 -e "SELECT 1" 2>/dev/null; then
  fail "proxy: wrong password should be rejected"
fi
# ブランチ名なしユーザーは拒否
if mysql -udev -pdev -h127.0.0.1 -P3306 -e "SELECT 1" 2>/dev/null; then
  fail "proxy: user without @branch should be rejected"
fi

log "metrics & Web UI"
probe http://127.0.0.1:9100/metrics | grep -q 'twig_branches{state="running"} 2' \
  || { curl -s http://127.0.0.1:9100/metrics | head -5; fail "metrics should report 2 running"; }
probe http://127.0.0.1:8080/ | grep -q "twig" || fail "web ui should serve"

log "proxy: lazy create (未知ブランチ名で接続すると生える)"
val=$(mysql -udev@pr-lazy -pdev -h127.0.0.1 -P3306 -N -e "SELECT COUNT(*) FROM app.items" 2>/dev/null) \
  || fail "lazy create: connect should auto-create branch"
[ "$val" = "3" ] || fail "lazy create: query result = $val"
twig list | grep -q "pr-lazy" || fail "lazy create: branch should appear in list"
twig delete pr-lazy

log "wake API"
twig create pr-wake > /dev/null
systemctl stop mysqld@pr-wake
# wake は冪等(running でも 200)なので再試行してよい
probe http://127.0.0.1:8080/v1/branches/pr-wake/wake -X POST > /dev/null || fail "wake should succeed"
val=$(mysql -udev@pr-wake -pdev -h127.0.0.1 -P3306 -N -e "SELECT 1" 2>/dev/null) || fail "wake: connect after wake"
[ "$val" = "1" ] || fail "wake: query"
twig delete pr-wake

log "baseline refresh (current 切り替え)"
cat > /etc/twig/refresh.sh <<REFRESH
#!/usr/bin/env bash
set -euo pipefail
DATADIR=/$POOL/base/data
SOCK=/tmp/twig-refresh.sock
mysqld --user=mysql --datadir="\$DATADIR" --skip-networking --socket="\$SOCK" \
  --pid-file=/tmp/twig-refresh.pid --log-error=/var/log/twig/refresh.err --daemonize
for _ in \$(seq 1 60); do mysqladmin -uroot -S "\$SOCK" ping >/dev/null 2>&1 && break; sleep 1; done
mysql -uroot -S "\$SOCK" -e "INSERT INTO app.items (name) VALUES ('from-refresh')"
mysqladmin -uroot -S "\$SOCK" shutdown
sleep 2
REFRESH
chmod +x /etc/twig/refresh.sh
code=$(curl -s -o /tmp/refresh-resp -w '%{http_code}' -X POST http://127.0.0.1:8080/v1/baseline/refresh)
[ "$code" = "202" ] || { cat /tmp/refresh-resp; fail "refresh should return 202 (got $code)"; }
for _ in $(seq 1 60); do
  curl -s http://127.0.0.1:8080/v1/baseline | grep -q '"refreshing":false' && break
  sleep 1
done
probe http://127.0.0.1:8080/v1/baseline | grep -q "baseline-" || fail "current should be rotated baseline"
# 新ブランチは新 baseline(4行)、既存 pr-1 は旧 baseline(3行)のまま
twig create pr-new > /dev/null
val=$(mysql -udev@pr-new -pdev -h127.0.0.1 -P3306 -N -e "SELECT COUNT(*) FROM app.items" 2>/dev/null)
[ "$val" = "4" ] || fail "new branch should see refreshed baseline (got $val)"
val=$(mysql -udev@pr-1 -pdev -h127.0.0.1 -P3306 -N -e "SELECT COUNT(*) FROM app.items" 2>/dev/null)
[ "$val" = "3" ] || fail "existing branch should keep old baseline (got $val)"
twig delete pr-new

log "github action entrypoint (create/idempotent/delete)"
AE="$SCRIPT_DIR/../action/entrypoint.sh"
[ -f "$AE" ] || AE="$SCRIPT_DIR/action-entrypoint.sh"   # Lima はフラットコピー
[ -f "$AE" ] || fail "action entrypoint not found"
export TWIG_API_URL=http://127.0.0.1:8080 TWIG_BRANCH=pr-77
: > /tmp/twig-action-out
TWIG_EVENT=opened TWIG_OUTPUT=/tmp/twig-action-out bash "$AE" || fail "action: opened should create"
grep -q "created=true" /tmp/twig-action-out || fail "action: created=true expected"
: > /tmp/twig-action-out
TWIG_EVENT=synchronize TWIG_OUTPUT=/tmp/twig-action-out bash "$AE" || fail "action: synchronize should succeed on existing branch"
grep -q "created=false" /tmp/twig-action-out || fail "action: created=false expected for existing"
TWIG_EVENT=closed bash "$AE" || fail "action: closed should delete"
TWIG_EVENT=closed bash "$AE" || fail "action: closed should be idempotent (404 OK)"
unset TWIG_API_URL TWIG_BRANCH

log "delete"
twig delete pr-1
twig delete pr-2
zfs list -r $POOL/branches | grep -q pr- && fail "datasets should be destroyed"

log "token 管理"
out=$(twig token create --name e2e-test) || fail "token create"
tok=$(echo "$out" | grep -o "twig_[0-9a-f]*")
[ -n "$tok" ] || fail "token: 平文が表示されるべき"
twig token list | grep -q e2e-test || fail "token list"
# 非 loopback からの検証は環境上できないため、DB トークンの受理はユニットテストで担保
twig token revoke e2e-test || fail "token revoke"
twig token list | grep -q e2e-test && fail "token should be revoked"

log "idle stop & TTL (リーパー)"
# 短い閾値で twigd を再起動
kill $TWIGD_PID 2>/dev/null || true
sleep 1
cp /etc/twig/config.yaml /tmp/twig-config.bak
sed -i "s/idle_stop_after: 30m.*/idle_stop_after: 3s/; s/delete_after_idle: 168h.*/delete_after_idle: 15s/" /etc/twig/config.yaml
sed -i "/delete_after_idle: 15s/a\\  reaper_interval: 1s" /etc/twig/config.yaml
grep -A1 "delete_after_idle" /etc/twig/config.yaml
/usr/local/bin/twigd --config /etc/twig/config.yaml > /var/log/twig/twigd2.log 2>&1 &
TWIGD_PID=$!
sleep 1
twig create pr-idle > /dev/null
sleep 6   # idle_stop_after(3s) + リーパー数周期
twig list | grep pr-idle | grep -q sleeping || { twig list; fail "reaper: pr-idle should be sleeping" ; }
systemctl is-active --quiet mysqld@pr-idle && fail "reaper: mysqld should be stopped"
# 再接続で起床(sleeping → running)
val=$(mysql -udev@pr-idle -pdev -h127.0.0.1 -P3306 -N -e "SELECT COUNT(*) FROM app.items" 2>/dev/null) \
  || fail "reaper: reconnect should wake sleeping branch"
[ "$val" = "4" ] || fail "reaper: wake query = $val (refresh後のbaselineは4行)"
# TTL: 15 秒放置で自動削除
sleep 18
twig list | grep -q pr-idle && fail "reaper: pr-idle should be TTL-deleted"
# 設定を戻して再起動
kill $TWIGD_PID 2>/dev/null || true
sleep 1
cp /tmp/twig-config.bak /etc/twig/config.yaml
/usr/local/bin/twigd --config /etc/twig/config.yaml >> /var/log/twig/twigd.log 2>&1 &
TWIGD_PID=$!
sleep 1


# --- 6. 後片付け ---
log "cleanup"
kill $TWIGD_PID 2>/dev/null || true
zpool destroy $POOL
rm -f "$POOL_IMG"

echo -e "\nE2E PASSED"
