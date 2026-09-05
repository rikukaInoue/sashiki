#!/usr/bin/env bash
# twig インストーラ: 最新 release の deb を取得して dpkg -i する。
#   curl -fsSL https://raw.githubusercontent.com/rikukaInoue/twig/main/install.sh | sudo bash
# private リポジトリの間は GITHUB_TOKEN(repo 読み取り)が必要:
#   curl -fsSL -H "Authorization: Bearer $GITHUB_TOKEN" .../install.sh | sudo -E bash
set -euo pipefail

REPO=rikukaInoue/twig
ARCH=$(dpkg --print-architecture 2>/dev/null || uname -m)
case "$ARCH" in
  x86_64) ARCH=amd64 ;;
  aarch64) ARCH=arm64 ;;
esac
case "$ARCH" in
  amd64|arm64) ;;
  *) echo "install.sh: unsupported arch: $ARCH" >&2; exit 1 ;;
esac

auth=()
if [ -n "${GITHUB_TOKEN:-}" ]; then
  auth=(-H "Authorization: Bearer ${GITHUB_TOKEN}")
fi

api="https://api.github.com/repos/${REPO}/releases/latest"
asset_url=$(curl -fsSL "${auth[@]}" "$api" \
  | grep -o "\"url\": *\"[^\"]*assets/[0-9]*\"\|\"name\": *\"[^\"]*\"" \
  | paste - - \
  | grep "_${ARCH}.deb" \
  | head -1 \
  | grep -o "https://[^\"]*")
if [ -z "$asset_url" ]; then
  echo "install.sh: ${ARCH} の deb が見つかりません" >&2
  exit 1
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
echo "downloading twig (${ARCH}) ..."
curl -fsSL "${auth[@]}" -H "Accept: application/octet-stream" -o "$tmp/twig.deb" "$asset_url"
dpkg -i "$tmp/twig.deb"
echo "installed: $(twig version)"
echo "next: sudo twig init --pool dbpool --device <dev>  (lsblk でデバイス確認)"
