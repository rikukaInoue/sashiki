#!/bin/sh
# deb postinstall。sashikid.service は User=sashiki で動くため system ユーザーを
# 用意する(無いと 217/USER で起動不可、#168)。ログは sashikid(sashiki)と
# mysqld(mysql)の両方が書くので共有可能にする(#167)。
set -e

if ! getent group sashiki >/dev/null 2>&1; then
	groupadd --system sashiki
fi
if ! id -u sashiki >/dev/null 2>&1; then
	useradd --system --gid sashiki --no-create-home \
		--home-dir /var/lib/sashiki --shell /usr/sbin/nologin sashiki
fi

# ログディレクトリは sashiki と mysql の両方が書く。sticky world-writable にして
# どちらのプロセスも自分のファイルを作れるようにする(所有は各自のファイル単位)。
install -d -m 1777 /var/log/sashiki
install -d -m 1777 /var/log/sashiki/hooks
# 状態ディレクトリ(state.db 等)は sashiki 所有。
install -d -o sashiki -g sashiki -m 0755 /var/lib/sashiki

systemctl daemon-reload >/dev/null 2>&1 || true

echo "sashiki: installed. next: sudo sashiki init --pool dbpool --device <dev>"
