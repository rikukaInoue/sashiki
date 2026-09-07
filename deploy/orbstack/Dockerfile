# sashiki を OrbStack(や任意の docker)の軽量 Linux コンテナで動かすイメージ(#113/#140)。
# ZFS は使わず、XFS reflink を CoW 基盤にする(storage.backend=reflink)。
# mysqld は systemd 無しで直接 spawn する(engine.mode=process)。
FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update -qq \
 && apt-get install -y -qq mysql-server mysql-client xfsprogs ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && (systemctl disable mysql 2>/dev/null || true)

# sashiki バイナリはビルド済みのものを COPY する(compose がホストでクロスビルドして渡す)。
COPY sashikid /usr/local/bin/sashikid
COPY sashiki  /usr/local/bin/sashiki
RUN chmod 755 /usr/local/bin/sashikid /usr/local/bin/sashiki

COPY entrypoint.sh /usr/local/bin/sashiki-entrypoint
COPY schema.sql /opt/sashiki/schema.sql
RUN chmod 755 /usr/local/bin/sashiki-entrypoint

# 3306(proxy) / 8080(API) を公開
EXPOSE 3306 8080
ENTRYPOINT ["/usr/local/bin/sashiki-entrypoint"]
