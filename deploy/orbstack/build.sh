#!/usr/bin/env bash
# sashiki を Linux にクロスビルドして deploy/orbstack/ に置く(compose の COPY 用)。
set -euo pipefail
REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
OUT="$REPO_ROOT/deploy/orbstack"
ARCH="$(docker version --format '{{.Server.Arch}}' 2>/dev/null || uname -m)"
case "$ARCH" in x86_64|amd64) GOARCH=amd64;; aarch64|arm64) GOARCH=arm64;; *) GOARCH="$ARCH";; esac
echo "==> cross-building sashikid/sashiki (linux/$GOARCH)"
( cd "$REPO_ROOT" && GOOS=linux GOARCH="$GOARCH" go build -o "$OUT/sashikid" ./cmd/sashikid \
                  && GOOS=linux GOARCH="$GOARCH" go build -o "$OUT/sashiki"  ./cmd/sashiki )
# デモ用スキーマ(無ければ最小のものを置く)
[ -f "$OUT/schema.sql" ] || cat > "$OUT/schema.sql" <<'SQL'
CREATE DATABASE IF NOT EXISTS app;
USE app;
CREATE TABLE IF NOT EXISTS items (id INT PRIMARY KEY AUTO_INCREMENT, name VARCHAR(64));
INSERT INTO items (name) VALUES ('alpha'), ('beta'), ('gamma');
SQL
echo "==> done: $OUT/{sashikid,sashiki,schema.sql}"
