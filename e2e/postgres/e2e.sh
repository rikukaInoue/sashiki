#!/usr/bin/env bash
# PostgreSQL エンジンの E2E。素の Ubuntu (VM/EC2) 上で root 実行。
# loopback zpool + postgres 16 で create/reset/delete を検証する。
#   usage: sudo ./e2e.sh <sashikid> <sashiki>
set -euo pipefail

SASHIKID_BIN=${1:?usage: e2e.sh <sashikid> <sashiki>}
SASHIKI_BIN=${2:?usage: e2e.sh <sashikid> <sashiki>}
POOL=tpgpool
POOL_IMG=/var/tmp/sashiki-pg-zpool.img

log() { echo -e "\n=== $* ==="; }
fail() { echo "PG E2E FAILED: $*" >&2; tail -20 /var/log/sashiki-pg/sashikid.log 2>/dev/null; exit 1; }

log "packages"
export DEBIAN_FRONTEND=noninteractive
apt-get update -q > /dev/null
apt-get install -y -q zfsutils-linux postgresql postgresql-client > /dev/null 2>&1
systemctl stop postgresql 2>/dev/null || true
systemctl disable postgresql 2>/dev/null || true
PGBIN=$(ls -d /usr/lib/postgresql/*/bin | sort -V | tail -1)

log "cleanup previous run"
systemctl stop 'postgres-sashiki@*' 2>/dev/null || true
pkill -f "sashikid --config /etc/sashiki-pg" 2>/dev/null || true
zpool destroy $POOL 2>/dev/null || true
rm -f "$POOL_IMG"
rm -rf /etc/sashiki-pg /var/lib/sashiki-pg /var/log/sashiki-pg
mkdir -p /etc/sashiki-pg/hooks /var/lib/sashiki-pg/branches /var/log/sashiki-pg/hooks

log "zpool (recordsize=8k for postgres)"
truncate -s 3G "$POOL_IMG"
zpool create -o ashift=12 $POOL "$POOL_IMG"
zfs set compression=lz4 atime=off $POOL
zfs create -o recordsize=8k -o logbias=throughput $POOL/base
# クローンのプロパティは origin ではなく名前空間上の親から継承されるため、
# branch_parent にも recordsize=8k が必要
zfs create -o recordsize=8k -o logbias=throughput $POOL/branches

log "install unit + config + start sashikid"
install -m 755 "$SASHIKID_BIN" /usr/local/bin/sashikid-pg
install -m 755 "$SASHIKI_BIN" /usr/local/bin/sashiki-pg
SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
UNIT_SRC="$SCRIPT_DIR/../../deploy/systemd/postgres-sashiki@.service"
[ -f "$UNIT_SRC" ] || UNIT_SRC="$SCRIPT_DIR/postgres-sashiki@.service"
# EnvironmentFile を e2e 専用ディレクトリ(config の env_dir)へ向ける。unit 側の
# 既定パスは変わり得る(#177 で /etc/sashiki → /run/sashiki へ移動した)ので、
# 特定パスではなく行ごと置換し、置換できたことを検証する。パターン不一致で
# 無言の no-op になると env が読まれず systemd の起動が謎に失敗するため。
sed -E "s|^EnvironmentFile=.*|EnvironmentFile=/etc/sashiki-pg/%i.env|" "$UNIT_SRC" \
  > /etc/systemd/system/postgres-sashiki@.service
grep -q '^EnvironmentFile=/etc/sashiki-pg/%i.env$' /etc/systemd/system/postgres-sashiki@.service \
  || fail "unit の EnvironmentFile を書き換えられなかった ($UNIT_SRC)"
systemctl daemon-reload

cat > /etc/sashiki-pg/config.yaml <<YAML
listen:
  api: "127.0.0.1:8090"
  proxy: "127.0.0.1:15432"
state_db: /var/lib/sashiki-pg/state.db
storage:
  backend: ebs-zfs
  ebs-zfs:
    pool: $POOL
    base_dataset: $POOL/base
    branch_parent: $POOL/branches
    baseline_snapshot: baseline
    sudo: false
engine:
  type: postgres
  postgres:
    bin_dir: $PGBIN
    env_dir: /etc/sashiki-pg
    sudo: false
    app_user: dev
    app_pass: dev
branches:
  name_pattern: "^[a-z0-9-]{1,32}$"
  max_branches: 5
hooks:
  dir: /etc/sashiki-pg/hooks
  log_dir: /var/log/sashiki-pg/hooks
YAML

log "baseline import (#223)"
# 手で initdb する代わりに sashiki baseline import を使う。initdb → ダンプ投入 →
# ロール作成 → 正常終了 → snapshot までを実コマンドで通し、これ自体を検証する。
cat > /var/tmp/sashiki-pg-dump.sql <<'SQL'
CREATE TABLE items (id SERIAL PRIMARY KEY, name TEXT);
INSERT INTO items (name) VALUES ('alpha'), ('beta'), ('gamma');
SQL
mkdir -p /$POOL/base
chown postgres:postgres /$POOL/base
sashiki-pg baseline import --config /etc/sashiki-pg/config.yaml \
  --from /var/tmp/sashiki-pg-dump.sql --db app || fail "baseline import が失敗した"
zfs list -t snapshot $POOL/base@baseline > /dev/null || fail "@baseline が取得されていない"
echo "  @baseline を取得済み"

/usr/local/bin/sashikid-pg --config /etc/sashiki-pg/config.yaml > /var/log/sashiki-pg/sashikid.log 2>&1 &
SASHIKID_PID=$!
trap 'kill $SASHIKID_PID 2>/dev/null || true' EXIT
for _ in $(seq 1 30); do curl -sf http://127.0.0.1:8090/v1/healthz > /dev/null 2>&1 && break; sleep 0.5; done
curl -sf http://127.0.0.1:8090/v1/healthz > /dev/null || fail "sashikid did not start"

export SASHIKI_API_URL=http://127.0.0.1:8090
# import が host 認証を scram-sha-256 にするので、直ポート接続もパスワードが要る
# (以前の手動 initdb は trust だった)。dev ロールのパスワードは app_pass。
q() { PGPASSWORD=dev psql -h 127.0.0.1 -p "$1" -U dev -d app -t -A -c "$2" 2>/dev/null; }

log "create pg-1"
time sashiki-pg create pg-1
PORT=$(sashiki-pg show pg-1 --json | python3 -c 'import json,sys;print(json.load(sys.stdin)["port"])')
[ "$(q $PORT 'SELECT COUNT(*) FROM items')" = "3" ] || fail "pg-1 should have 3 items"

log "破壊 → reset"
q $PORT "DELETE FROM items" > /dev/null
time sashiki-pg reset pg-1
[ "$(q $PORT 'SELECT COUNT(*) FROM items')" = "3" ] || fail "reset should restore"
# initdb 既定では logging_collector が無効でサーバーログは journald に行く。
# data/log を grep しても常にパスしてしまうので journal 側を確認する。
# パイプで grep -q に流すと pipefail × SIGPIPE で判定が化けるため変数に受ける(#49)。
pg_journal=$(journalctl -u 'postgres-sashiki@pg-1' --no-pager 2>/dev/null || true)
grep -qi "database system was not properly shut down" <<<"$pg_journal" \
  && fail "crash recovery ran (dirty @init)"
# チェック自体が生きていることの確認: 正常起動ログは journal に必ず出る
grep -qi "database system is ready to accept connections" <<<"$pg_journal" \
  || fail "journal に postgres のログが見つからない(crash recovery チェックが機能していない)"

log "pgproxy: 固定エンドポイント経由 + lazy create (#222)"
# 未作成の pg-2 へ dev@pg-2 で接続すると、認証(SCRAM-SHA-256)が通ってから
# lazy create されてそのブランチに繋がる。psql は既定で SSL を試すので、
# proxy が 'N' を返して平文へ落ちる経路も同時に確認できる。
pq() { PGPASSWORD=dev psql -h 127.0.0.1 -p 15432 -U "$1" -d app -t -A -c "$2" 2>&1; }
# 「本当に lazy create されたか」を言えるように、接続前に存在しないことを確かめる。
sashiki-pg show pg-2 > /dev/null 2>&1 && fail "pg-2 は接続前には存在しないはず"
echo "  接続前: pg-2 は存在しない"

proxy_count=$(pq 'dev@pg-2' 'SELECT COUNT(*) FROM items') \
  || fail "proxy 経由の接続に失敗: $proxy_count"
echo "  proxy 経由 SELECT COUNT(*) FROM items => $proxy_count"
[ "$proxy_count" = "3" ] || fail "proxy 経由で 3 件見えるはず (got: $proxy_count)"
sashiki-pg show pg-2 > /dev/null || fail "lazy create で pg-2 が作られるはず"
echo "  接続後: pg-2 が lazy create されている"

# 認証終端: パスワードが違えば失敗し、かつブランチは作られない(#7/#51)
bad=$(PGPASSWORD=wrong psql -h 127.0.0.1 -p 15432 -U 'dev@pg-3' -d app -t -A -c 'SELECT 1' 2>&1 || true)
grep -qi "authentication failed" <<<"$bad" || fail "誤パスワードは弾かれるはず (got: $bad)"
sashiki-pg show pg-3 > /dev/null 2>&1 && fail "認証前に lazy create してはいけない (#7/#51)"
echo "  誤パスワードは拒否され、pg-3 は作られていない"

sashiki-pg delete pg-2 > /dev/null

log "delete"
time sashiki-pg delete pg-1
grep -q pg- <<<"$(zfs list -r $POOL/branches)" && fail "dataset should be destroyed"

log "cleanup"
kill $SASHIKID_PID 2>/dev/null || true
zpool destroy $POOL
rm -f "$POOL_IMG"
echo "PG E2E PASSED"
