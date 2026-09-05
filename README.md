# twig

**PR ごとに使い捨ての本物 MySQL が生える。** ZFS の Copy-on-Write クローンで、何 GB のデータベースでも数秒で複製・リセット・削除する。

```
$ twig create pr-123
branch 'pr-123' ready: mysql -udev@pr-123 -h twig.internal -P3401

$ twig reset pr-123     # 壊しても数秒で作成時点に戻る
$ twig delete pr-123    # PR を閉じたら消す
```

- 複製は **CoW なのでコピーしない**。クローン直後のディスク消費は数百 KB
- ブランチは完全に分離。`DROP TABLE` しても他のブランチとベースは無傷
- 実データ量でマイグレーションをレビューできる(本番で長時間ロックする ALTER が事前に見つかる)

背景と実測: [EBS 版 PoC](https://rikuka.dev/blog/db-branch-zfs-mysql-poc/) / [FSx 版検証](https://rikuka.dev/blog/db-branch-fsx-openzfs/)

## Status

**v0.1 開発中**(private)。zfs バックエンド + MySQL + REST API + CLI。
ロードマップ: v0.2 でプロキシ + lazy create(`mysql -udev@pr-123` で存在しないブランチが生える)、v0.3 で hooks / アイドル停止 / ベースライン自動更新、v1.0 で FSx バックエンド。

## Quick Start (Ubuntu 24.04)

```bash
sudo twig init --pool dbpool --device /dev/nvme1n1   # デバイス名は lsblk で確認
# ベースデータを投入して baseline の snapshot を取得する(init 完了時のガイダンス参照)
sudo systemctl enable --now twigd
twig create pr-1
```

`twig init` は パッケージ導入・AppArmor 無効化・zpool/データセット作成・
systemd ユニット・config 生成までを冪等に行う(構成済みステップはスキップ)。

## アーキテクチャ

```
twig CLI ──HTTP──▶ twigd ──┬─▶ storage (zfs | fsx)   クローン・スナップショット・破棄
                           ├─▶ engine  (mysql)        mysqld@<branch> の起動・停止・ready
                           ├─▶ hooks                  on-create / on-reset / on-delete
                           └─▶ state.db (SQLite)      name / port / state / origin
```

- **storage** と **engine** はインターフェース。バックエンドは `Capabilities` で性格(FastRollback / TypicalCreate / AsyncDelete)を宣言し、コアが挙動を切り替える(zfs の rollback は 6 秒、FSx の restore は 10 分——同じ「reset」でも実装が変わる)
- **@init スナップショットは必ず mysqld の正常終了状態で撮る**。これを破るとブランチ起動のたびに InnoDB クラッシュリカバリが走る(設計全体で最も重要な不変条件)
- 自社固有の処理(マイグレーション適用・データマスク)はコアに入れず **hooks** に追い出す

## 開発

```bash
make build   # bin/twigd, bin/twig
make test    # ユニットテスト(ZFS 不要、モックで動く)
make lint    # golangci-lint
```

E2E(実 ZFS + mysqld)は Ubuntu ホストで: `truncate -s 4G /tmp/zpool.img && sudo zpool create tpool /tmp/zpool.img`

設計判断の記録は [docs/DECISIONS.md](docs/DECISIONS.md)。

## License

Apache-2.0
