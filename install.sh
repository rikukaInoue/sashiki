#!/usr/bin/env bash
# sashiki インストーラ。
#   Linux : 最新 release の deb を取得して dpkg -i する。
#   macOS : 最新 release の darwin tar.gz を /usr/local/bin へ展開する(#118)。
#     curl -fsSL https://raw.githubusercontent.com/rikukadev/sashiki/main/install.sh | sudo bash
# private リポジトリの間は GITHUB_TOKEN(repo 読み取り)が必要:
#     curl -fsSL -H "Authorization: Bearer $GITHUB_TOKEN" .../install.sh | sudo -E bash
set -euo pipefail

REPO=rikukadev/sashiki
OS=$(uname -s)

auth=()
if [ -n "${GITHUB_TOKEN:-}" ]; then
  auth=(-H "Authorization: Bearer ${GITHUB_TOKEN}")
fi
api="https://api.github.com/repos/${REPO}/releases/latest"

# SASHIKI_RELEASE がタグ(例 v0.4.1)なら releases/download の直リンクで取得する。
# latest API 経由の JSON パースで非 deb を掴む事故を避ける(#166)。
# "latest" / "main" / 空 のときは従来どおり latest API を使う。
REL="${SASHIKI_RELEASE:-}"

# direct_url SUFFIX : 直リンク URL を返す(タグ未指定なら空を返す)。
#   SUFFIX 例: "_linux_amd64.deb" / "_darwin_arm64.tar.gz"
direct_url() {
  if [ -z "$REL" ] || [ "$REL" = latest ] || [ "$REL" = main ]; then
    return 0
  fi
  local ver="${REL#v}"
  printf 'https://github.com/%s/releases/download/%s/sashiki_%s%s' "$REPO" "$REL" "$ver" "$1"
}

# asset_url PATTERN : 最新 release から PATTERN を名前に含む asset の URL を返す。
asset_url() {
  local pattern="$1"
  curl -fsSL "${auth[@]}" "$api" \
    | grep -o "\"url\": *\"[^\"]*assets/[0-9]*\"\|\"name\": *\"[^\"]*\"" \
    | paste - - \
    | grep "$pattern" \
    | head -1 \
    | grep -o "https://[^\"]*"
}

case "$OS" in
Linux)
  ARCH=$(dpkg --print-architecture 2>/dev/null || uname -m)
  case "$ARCH" in
    x86_64) ARCH=amd64 ;;
    aarch64) ARCH=arm64 ;;
  esac
  case "$ARCH" in amd64 | arm64) ;; *) echo "install.sh: unsupported arch: $ARCH" >&2; exit 1 ;; esac

  url=$(direct_url "_linux_${ARCH}.deb")
  [ -z "$url" ] && url=$(asset_url "_${ARCH}.deb")
  if [ -z "$url" ]; then
    echo "install.sh: ${ARCH} の deb が見つかりません" >&2
    exit 1
  fi
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  echo "downloading sashiki (linux/${ARCH}) ..."
  curl -fsSL "${auth[@]}" -H "Accept: application/octet-stream" -o "$tmp/sashiki.deb" "$url"
  # 起動直後は dpkg ロックを他プロセスが握っていることがある。apt-get 経由なら
  # DPkg::Lock::Timeout でロック解放を待てる(#192)。古い apt では dpkg にフォールバック。
  apt-get -o DPkg::Lock::Timeout=300 install -y "$tmp/sashiki.deb" || dpkg -i "$tmp/sashiki.deb"
  echo "installed: $(sashiki version)"
  echo "next: sudo sashiki init --pool dbpool --device <dev>  (lsblk でデバイス確認)"
  ;;

Darwin)
  ARCH=$(uname -m)
  case "$ARCH" in
    x86_64) ARCH=amd64 ;;
    arm64) ARCH=arm64 ;;
  esac
  case "$ARCH" in amd64 | arm64) ;; *) echo "install.sh: unsupported arch: $ARCH" >&2; exit 1 ;; esac

  url=$(direct_url "_darwin_${ARCH}.tar.gz")
  [ -z "$url" ] && url=$(asset_url "_darwin_${ARCH}.tar.gz")
  if [ -z "$url" ]; then
    echo "install.sh: darwin/${ARCH} の tar.gz が見つかりません" >&2
    exit 1
  fi
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  echo "downloading sashiki (darwin/${ARCH}) ..."
  curl -fsSL "${auth[@]}" -H "Accept: application/octet-stream" -o "$tmp/sashiki.tar.gz" "$url"
  tar -xzf "$tmp/sashiki.tar.gz" -C "$tmp"
  # /usr/local/bin へ設置(書けなければ sudo を促す)。
  dest=/usr/local/bin
  install_bin() { install -m 0755 "$tmp/$1" "$dest/$1"; }
  if [ -w "$dest" ]; then
    install_bin sashiki
    install_bin sashikid
  else
    echo "install.sh: $dest への書き込みに sudo が要ります"
    sudo install -m 0755 "$tmp/sashiki" "$dest/sashiki"
    sudo install -m 0755 "$tmp/sashikid" "$dest/sashikid"
  fi
  echo "installed: $(sashiki version)"
  echo "next: sashiki init --platform darwin  (Homebrew mysql + apfs + launchd。詳細は docs/LOCAL-DEV.md)"
  ;;

*)
  echo "install.sh: unsupported OS: $OS (Linux / Darwin のみ)" >&2
  exit 1
  ;;
esac
