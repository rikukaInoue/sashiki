# sashiki

**PR を作ると、本番に近いサイズの独立した MySQL DB が数秒で生える。**
壊しても `sashiki reset` で戻せる。main が進んだら `sashiki recreate` で追従できる。

```
$ sashiki create pr-123
branch 'pr-123' ready: mysql -udev@pr-123 -h sashiki.internal -P3306

$ sashiki reset pr-123      # 壊しても数秒で作成時点に戻る
$ sashiki recreate pr-123   # main が進んだら最新 baseline から作り直す
$ sashiki delete pr-123     # PR を閉じたら消す
```

ZFS の Copy-on-Write クローンを使い、何 GB のデータベースでも数秒で複製・リセット・削除する。
sashiki(挿し木)は、preview / CI / migration 検証 / デバッグ / 個人 sandbox のための
**使い捨て database workspace** を、immutable な baseline から秒単位で払い出す control plane。

- 複製は **CoW なのでコピーしない**。クローン直後のディスク消費は数百 KB
- ブランチは完全に分離。`DROP TABLE` しても他のブランチとベースは無傷
- 実データ量でマイグレーションをレビューできる(本番で長時間ロックする ALTER が事前に見つかる)
- 課金されるのは動いている mysqld だけ。アイドルブランチは自動停止し、再接続で起きる

背景と実測: [EBS 版 PoC](https://rikuka.dev/blog/db-branch-zfs-mysql-poc/) / [FSx 版検証](https://rikuka.dev/blog/db-branch-fsx-openzfs/)

## Git との対応

| Git | sashiki |
|---|---|
| main HEAD | current baseline |
| `git branch` | `sashiki create` |
| `git reset --hard` | `sashiki reset` |
| `git rebase main` | `sashiki recreate` |
| `git branch -D` | `sashiki delete` |

## 機能

- **branch lifecycle**: create / reset / recreate / delete / retry(hook 失敗などからの再実行)
- **lazy create**: `mysql -udev@pr-999` — 存在しないブランチ名への接続でその場で生える(プロキシ経由、:3306 固定エンドポイント)
- **アイドル管理**: 無接続が続くと mysqld を停止(`sleeping`)、再接続で起床。TTL で自動削除(PR close 取りこぼしの保険)
- **baseline**: build → validate → publish の 3 段階。検証に落ちた candidate は current にならないので、壊れた migration が main に入っても新規ブランチは無傷。`baseline set` で直前の正常版に即ロールバック
- **capacity 管理**: メモリ admission(不足時は新規を拒否して既存 mysqld を OOM から守る)、storage watermark、`sashiki capacity`
- **運用**: 起動時 reconciliation(state.db と実体の突き合わせ)、`sashiki doctor`、orphan GC、Prometheus メトリクス、Web UI(ブランチ一覧 + データブラウザ)
- **engine**: MySQL / PostgreSQL(直接ポート接続)
- **storage backend**: EBS + ZFS(default、秒単位の UX)/ FSx for OpenZFS(storage と compute の分離)
- **GitHub Action**: PR open/close に連動して create / delete、接続先を PR にコメント

## Quick Start (Ubuntu 24.04)

```bash
sudo sashiki init --pool dbpool --device /dev/nvme1n1   # デバイス名は lsblk で確認
# ベースデータを投入して baseline の snapshot を取得する(init 完了時のガイダンス参照)
sudo systemctl enable --now sashikid
sashiki create pr-1
```

`sashiki init` はパッケージ導入・AppArmor プロファイル・sudoers・zpool / データセット作成・
systemd ユニット・config 生成・API トークン発行までを冪等に行う(構成済みステップはスキップ)。

GitHub Actions との連携:

```yaml
- uses: rikukaInoue/sashiki/action@main
  with:
    api_url: ${{ vars.SASHIKI_API_URL }}
    token: ${{ secrets.SASHIKI_API_TOKEN }}
    comment: "true"   # PR に接続先をコメント
```

## アーキテクチャ

```
sashiki CLI / Action ──HTTP──▶ sashikid ──┬─▶ storage (zfs | fsx)   クローン・スナップショット・破棄
             │                            ├─▶ engine  (mysql | postgres)
   mysql クライアント ──:3306──▶ proxy ────┤                          プロセスの起動・停止・ready
             (user@branch でルーティング)  ├─▶ hooks                  on-create / on-reset / on-delete / on-baseline-*
                                          └─▶ state.db (SQLite)      branch / baseline / operation / token
```

- **storage** と **engine** はインターフェース。バックエンドは `Capabilities` で性格(FastRollback / TypicalCreate / AsyncDelete)を宣言し、コアが挙動を切り替える(zfs の rollback は数秒、FSx は再クローン方式——同じ「reset」でも実装が変わる)
- **@init / @baseline スナップショットは必ず mysqld の正常終了状態でのみ取得する**。これを破るとブランチ起動のたびに InnoDB クラッシュリカバリが走る(設計全体で最も重要な不変条件)
- 自社固有の処理(マイグレーション適用・データマスク)はコアに入れず **hooks** に追い出す

設計仕様は [docs/SPEC.md](docs/SPEC.md)、設計判断の記録(ADR)は [docs/DECISIONS.md](docs/DECISIONS.md)、
コスト比較は [docs/COSTS.md](docs/COSTS.md)。

## 向かない用途

sashiki は**データ量・内容・schema の再現性は高いが、インフラトポロジーの再現性は目的にしない**。

- HA / failover 試験、replication topology の検証
- Aurora / RDS 固有挙動の検証
- 本番相当の I/O ベンチマーク、複数ブランチ同時実行での厳密な性能比較(共有ホスト / EBS / zpool の影響を受ける)
- 本番運用の DB(開発・検証専用)

## FAQ

**Q. 本番データをそのまま使っていい?**
マスクしてから。PII マスキングは baseline build パイプラインの必須ステップで、`require_masked` を有効にすると未マスクの baseline は publish できない。

**Q. どのくらいメモリが要る?**
ディスクは CoW でほぼ増えないが、**mysqld はブランチごとに 1 プロセス**。コストは同時稼働数で決まる(buffer pool 256MB 設定で t3.large に 8〜10 本)。アイドル停止があるので「100 ブランチ、同時稼働 5」なら小さいインスタンスで足りる。

**Q. PostgreSQL は?**
engine として対応済み(接続はプロキシでなく直接ポート)。プロキシ / lazy create は現状 MySQL のみ。

**Q. FSx バックエンドはいつ使う?**
「小規模→EBS、大規模→FSx」ではない。multi-host / Spot / host 使い捨て / 1 台の RAM 限界、のどれかが必要になったら FSx。詳細は [docs/COSTS.md](docs/COSTS.md)。

## 開発

```bash
make build   # bin/sashikid, bin/sashiki
make test    # ユニットテスト(ZFS 不要、モックで動く)
make lint    # golangci-lint
```

E2E(実 ZFS + mysqld)は Ubuntu ホストで: `truncate -s 4G /tmp/zpool.img && sudo zpool create tpool /tmp/zpool.img`

## License

Apache-2.0
