#!/usr/bin/env bash
# OrbStack(または任意の docker)上で reflink storage バックエンドを実機検証する。
#
# やること:
#   1. Mac 側で reflink バックエンドのテストを linux バイナリにクロスコンパイル
#   2. privileged コンテナ内で loopback XFS(reflink=1)を作ってマウント
#   3. TMPDIR をその XFS に向けてテストバイナリを実行(= 実 reflink CoW で検証)
#
# OrbStack のカーネルには ZFS モジュールが無いため loopback zpool は使えないが、
# XFS reflink はカーネル組み込みで使える(APFS clonefile の Linux 版)。
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
ARCH="$(docker version --format '{{.Server.Arch}}' 2>/dev/null || echo arm64)"
IMG="${IMG:-ubuntu:24.04}"

echo "==> cross-compiling reflink test binary (linux/$ARCH)"
GOOS=linux GOARCH="$ARCH" go test -c ./internal/storage/reflink/ \
  -o "$REPO_ROOT/deploy/orbstack/reflink.test"
trap 'rm -f "$REPO_ROOT/deploy/orbstack/reflink.test"' EXIT

echo "==> running the backend against a loopback XFS(reflink) inside a container"
docker run --rm --privileged \
  -v "$REPO_ROOT/deploy/orbstack/reflink.test:/reflink.test:ro" \
  "$IMG" bash -euc '
    apt-get update -qq >/dev/null 2>&1
    apt-get install -y -qq xfsprogs >/dev/null 2>&1
    dd if=/dev/zero of=/xfs.img bs=1M count=512 status=none
    mkfs.xfs -q -m reflink=1 /xfs.img
    mkdir -p /mnt/xfs && mount -o loop /xfs.img /mnt/xfs
    echo "-- kernel: $(uname -r)"
    echo "-- zpool available? $(command -v zpool || echo NO) (OrbStack kernel には zfs モジュール無し)"
    echo "-- running reflink backend tests on XFS reflink mount --"
    TMPDIR=/mnt/xfs /reflink.test -test.v
  '
echo "==> OK: reflink backend verified on a real XFS reflink filesystem"
