#!/usr/bin/env bash
# コンテナ内で XFS reflink 領域を用意し、baseline を構築して sashikid を常駐させる。
# privileged コンテナで動かす前提(loopback XFS のマウントに必要)。
set -euo pipefail

ROOT=/var/lib/sashiki-data     # reflink 対応 FS(XFS)をここにマウントする
IMG=/xfs.img

# --- 1. XFS reflink 領域を用意(既存なら再利用)---
if ! mountpoint -q "$ROOT"; then
  mkdir -p "$ROOT"
  if [ ! -f "$IMG" ]; then
    dd if=/dev/zero of="$IMG" bs=1M count="${XFS_SIZE_MB:-2048}" status=none
    mkfs.xfs -q -m reflink=1 "$IMG"
  fi
  mount -o loop "$IMG" "$ROOT"
fi
# reflink が実際に効くか確認(効かない FS だと create が失敗するので早期に落とす)
touch "$ROOT/.probe.src"
if ! cp --reflink=always "$ROOT/.probe.src" "$ROOT/.probe.dst" 2>/dev/null; then
  echo "FATAL: $ROOT は reflink 非対応です(XFS reflink=1 / Btrfs が必要)" >&2
  exit 1
fi
rm -f "$ROOT/.probe.src" "$ROOT/.probe.dst"

mkdir -p /etc/sashiki /var/log/sashiki "$ROOT/base" "$ROOT/branches"

# --- 2. config を生成(reflink backend + process engine)---
if [ ! -f /etc/sashiki/config.yaml ]; then
  cat > /etc/sashiki/config.yaml <<YAML
listen:
  api: "0.0.0.0:8080"
  proxy: "0.0.0.0:3306"
  metrics: "127.0.0.1:9100"
state_db: /var/lib/sashiki/state.db
domain: sashiki.local
storage:
  backend: reflink
  local:
    root: $ROOT
engine:
  type: mysql
  mysql:
    mode: process
    mysqld_bin: /usr/sbin/mysqld
    run_user: mysql
    app_user: dev
    app_pass: dev
branches:
  name_pattern: "^[a-z0-9-]{1,32}$"
  max_branches: 20
YAML
fi
mkdir -p /var/lib/sashiki
chown -R mysql:mysql "$ROOT" /var/log/sashiki

# --- 3. baseline を構築(初回のみ)---
if [ ! -d "$ROOT/base/snap/baseline" ]; then
  echo "==> building baseline from /opt/sashiki/schema.sql"
  sashiki baseline import --from /opt/sashiki/schema.sql
fi

# --- 4. sashikid 常駐 ---
echo "==> starting sashikid (backend=reflink, engine=process)"
exec sashikid --config /etc/sashiki/config.yaml
