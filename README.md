# sashiki

**開発環境向けの、ブランチできる RDS。**

本番相当のサイズ・中身の MySQL / PostgreSQL を、Git のブランチのように**数秒で作って・壊して・戻せる**。
ZFS の Copy-on-Write クローンを使うので、何 GB のデータベースでも複製は数百 KB。
PR プレビュー・CI・開発者 sandbox・マイグレーション検証——非本番で「独立した DB がすぐ欲しい」場面のためのセルフホスト基盤。

```console
$ sashiki create pr-123
branch 'pr-123' ready: mysql -udev@pr-123 -h sashiki.internal -P3306

$ sashiki reset  pr-123    # 壊しても数秒で作成時点に戻る
$ sashiki recreate pr-123  # main が進んだら最新 baseline から作り直す
$ sashiki delete pr-123    # 用が済んだら消す
```

- 複製は **CoW なのでコピーしない**。クローン直後のディスク消費は数百 KB
- ブランチは完全に分離。`DROP TABLE` しても他ブランチとベースは無傷
- 実データ量でマイグレーションをレビューできる(本番で長時間ロックする ALTER が事前に見つかる)
- 課金されるのは動いている mysqld だけ。アイドルブランチは自動停止し、再接続で起きる

背景と実測: [EBS 版 PoC](https://rikuka.dev/blog/db-branch-zfs-mysql-poc/) / [FSx 版検証](https://rikuka.dev/blog/db-branch-fsx-openzfs/)

> **これは本番 DB の置き換えではありません。** 非本番(preview / CI / dev)向けの、使い捨て DB を配る基盤です。単一ノード構成で HA/レプリカは持ちません。詳しくは [向かない用途](#向かない用途)。

---

## 何に使える

| 用途 | profile | 使い方 |
|---|---|---|
| **PR プレビュー環境** | `preview` | GitHub Action が PR open で create / close で delete |
| **CI の分離 DB** | `ci` | ジョブごとに create、短い TTL で自動回収 |
| **開発者の sandbox** | `sandbox` | 手元から `create`、長めに保持 |
| **マイグレーション検証** | 任意 | 実データ量で ALTER を試す。壊したら `reset` |
| **RDS / Aurora の非本番用置き換え** | — | Terraform モジュールで 1 apply(入出力は RDS 互換) |

profile は用途ごとの寿命(idle 停止 / 自動削除)を表す。`create --ttl 7d` や `lease renew` で期限も付けられる。

## Git との対応

| Git | sashiki |
|---|---|
| main HEAD | current baseline |
| `git branch` | `sashiki create` |
| `git reset --hard` | `sashiki reset` |
| `git rebase main` | `sashiki recreate` |
| `git branch -D` | `sashiki delete` |

---

## 使ってみる(Ubuntu 24.04)

### 1. インストール

```bash
# 最新 release を取得して導入(deb)
curl -fsSL https://raw.githubusercontent.com/rikukaInoue/sashiki/main/install.sh | sudo bash
```

### 2. 初期化

```bash
# パッケージ導入・AppArmor・sudoers・zpool/データセット・systemd・config 生成まで冪等に
sudo sashiki init --pool dbpool --device /dev/nvme1n1   # デバイス名は lsblk で確認
```

`sashiki init` は構成済みのステップをスキップするので、何度実行しても安全。

### 3. baseline(元データ)を入れる

```bash
# ダンプから baseline を作る(投入 → 正常終了 → snapshot 取得まで)
sashiki baseline import --from prod-dump.sql
```

baseline は **build → validate → publish** の 3 段階。検証に落ちた候補は current にならないので、
壊れた migration が入っても新規ブランチは無傷。`baseline set` で直前の正常版に即ロールバックできる。
本番データを使うなら **PII マスキング**を build に組み込み、`require_masked` を有効にすれば未マスクは publish できない。

### 4. ブランチを払い出す

```bash
sudo systemctl enable --now sashikid
sashiki create pr-1
mysql -udev@pr-1 -pdev -h 127.0.0.1 -P3306   # :3306 固定エンドポイント経由で接続
```

---

## 認証(トークン)

API(= sashikid)への認証は「**ローカルは素通し、外から叩くときだけトークン**」。

- **同じホストからの CLI** — 不要。sashikid は loopback からのリクエストを無認証で通す。ホスト上で `sashiki create ...` はそのまま動く。
- **ホスト外(CI / GitHub Action / リモート CLI)** — **Bearer トークンが必要**。

### 誰が発行するか

トークンは **sashiki を運用する人(sashikid ホストに root で入れる人)** が発行する。2 通り:

| 方法 | 発行者 | 保存/配布 |
|---|---|---|
| **手動** | ホスト上で root が `sudo sashiki token create --name ci` | **平文は 1 回だけ表示**(DB には SHA-256 ハッシュのみ)。表示された値を利用側へ配る |
| **Terraform** | モジュールが `random_password` で生成 | SSM SecureString(出力 `api_token_ssm_path`)に保存。利用側は SSM から取得 |

### どう使うか(利用側)

CLI / Action は次のどちらかでトークンを読む(env が優先):

```bash
export SASHIKI_API_TOKEN=sashiki_xxxxxxxx        # 環境変数、または
printf '%s' "$SASHIKI_API_TOKEN" > ~/.config/sashiki/token   # ファイル

SASHIKI_API_URL=https://sashiki.example.com sashiki list      # 外から叩く
```

GitHub Action なら `secrets.SASHIKI_API_TOKEN` を渡すだけ(下の使い方 B)。

- サーバー側は、起動時に `SASHIKI_API_TOKEN` で渡した 1 個(後方互換)か、`sashiki token` で発行した state.db のトークン(ハッシュ照合)を検証する。
- ローテーションは **新規発行 → 配布先を差し替え → 旧トークンを `sashiki token revoke`**。
- ⚠️ 認証免除は「接続元が loopback か」で判定する。**リバースプロキシ越しに公開すると接続元が 127.0.0.1 に見えて素通しになる**ため、外部公開時は sashikid を直接 listen させ、トークン必須で運用すること。

## 3 つの使い方

### A. CLI から

```bash
sashiki create demo --profile sandbox --ttl 7d
sashiki list
sashiki show demo --json
sashiki reset demo
sashiki lease renew demo --for 14d   # 期限を延ばす
sashiki drain                        # メンテ前に全ブランチを安全停止
sashiki delete demo
```

### B. GitHub Action(PR プレビュー)

```yaml
- uses: rikukaInoue/sashiki/action@main
  with:
    api_url: ${{ vars.SASHIKI_API_URL }}
    token:   ${{ secrets.SASHIKI_API_TOKEN }}
    profile: preview
    on_close: delete    # PR close でブランチ削除
    comment: "true"     # 接続先(host/port/user)を PR にコメント
```

PR open/reopen で create、close で delete。接続情報を出力するのでプレビュー環境の env にそのまま渡せる。

### C. Terraform(RDS を選ぶところで sashiki を選ぶ)

```hcl
module "db" {
  source = "github.com/rikukaInoue/sashiki//deploy/terraform?ref=v0.5.0"

  name           = "myapp-preview"
  vpc_id         = var.vpc_id
  subnet_ids     = var.private_subnet_ids
  allowed_sg_ids = [aws_security_group.app.id]

  instance_class    = "m6i.large"    # RDS と同じ変数名
  allocated_storage = 100
  engine_version    = "8.0"
}
# 出力: endpoint / port / username / password_secret_arn / api_url ...(RDS/Aurora 互換)
```

EC2 + EBS(prevent_destroy)+ SG + IAM + Route53 + Secrets/SSM を 1 apply。
apply 完了時点で sashikid が稼働する。詳細は [deploy/terraform/README.md](deploy/terraform/)。

---

## 機能

- **branch lifecycle**: create / reset / recreate / delete / retry(hook 失敗などからの再実行)
- **profile / lease**: 用途ごとの idle lifecycle(preview / ci / sandbox)+ `--ttl` / `lease renew` の絶対期限
- **proxy(:3306 固定エンドポイント)**: `mysql -udev@<branch>` でルーティング。**認証終端(方式A)**——sashiki がパスワードを検証し、**認証後に** lazy create(認証前のリソース確保を防ぐ)。TLS 終端対応(`proxy.tls_cert`)
- **アイドル管理**: 無接続で mysqld 停止(`sleeping`)、再接続で起床。engine ポーリングで接続を追跡するので proxy を通らない接続でも正しく判定。TTL / lease で自動削除
- **baseline**: build → validate → publish。PII マスキングを必須化できる。`baseline set` で即ロールバック
- **capacity 管理**: メモリ admission(不足時は新規を拒否して既存 mysqld を OOM から守る)、storage watermark、`sashiki capacity`
- **運用**: 起動時 reconciliation、`sashiki doctor`、orphan GC、`sashiki drain`、構造化ログ + Prometheus メトリクス、Web UI
- **engine**: MySQL / PostgreSQL
- **storage backend**: EBS + ZFS(既定、秒単位の UX)/ FSx for OpenZFS(storage と compute の分離)

## アーキテクチャ

```
sashiki CLI / Action / Terraform ──HTTP──▶ sashikid ──┬─▶ storage (zfs | fsx)   クローン・スナップショット・破棄
             │                                        ├─▶ engine  (mysql | postgres)  起動・停止・ready・接続数
   mysql クライアント ──:3306──▶ proxy(認証終端)──────┤
             (user@branch でルーティング)             ├─▶ hooks    on-create / on-reset / on-baseline-* ...
                                                      └─▶ state.db (SQLite)   branch / baseline / operation / token
```

- **storage** と **engine** はインターフェース。バックエンドは `Capabilities`(FastRollback / TypicalCreate / AsyncDelete)を宣言し、コアが挙動を切り替える(zfs の rollback は数秒、FSx は再クローン方式——同じ「reset」でも実装が変わる)
- **@init / @baseline スナップショットは必ず mysqld の正常終了状態でのみ取得する**。破るとブランチ起動のたびに InnoDB クラッシュリカバリが走る(設計全体で最も重要な不変条件)
- 自社固有の処理(マイグレーション適用・データマスク)はコアに入れず **hooks** に追い出す

設計仕様は [docs/SPEC.md](docs/SPEC.md)、設計判断(ADR)は [docs/DECISIONS.md](docs/DECISIONS.md)、コスト比較は [docs/COSTS.md](docs/COSTS.md)。

## 向かない用途

sashiki は**データ量・内容・schema の再現性は高いが、インフラトポロジーの再現性は目的にしない**。

- HA / failover 試験、replication topology の検証
- Aurora / RDS 固有挙動の検証
- 本番相当の I/O ベンチマーク、複数ブランチ同時実行での厳密な性能比較(共有ホスト / EBS / zpool の影響を受ける)
- **本番運用の DB**(開発・検証専用。単一ノードで HA/レプリカを持たない)

## FAQ

**Q. 本番データをそのまま使っていい?**
マスクしてから。PII マスキングは baseline build の必須ステップにでき、`require_masked` を有効にすると未マスクの baseline は publish できない。

**Q. どのくらいメモリが要る?**
ディスクは CoW でほぼ増えないが、**mysqld はブランチごとに 1 プロセス**。コストは同時稼働数で決まる(buffer pool 256MB 設定で t3.large に 8〜10 本)。アイドル停止があるので「100 ブランチ、同時稼働 5」なら小さいインスタンスで足りる。

**Q. profile と lease の違いは?**
profile は「無接続が続いたら止める/消す」寿命ポリシー(preview/ci/sandbox)。lease(`--ttl` / `lease renew`)は「使用中でも必ず期限で回収する」絶対期限。CI で「最長 1 時間で必ず消える」を保証したいとき等に使う。

**Q. PostgreSQL は?**
engine として対応(接続は直接ポート)。proxy / lazy create は MySQL のみ。idle 管理は engine ポーリングで両対応。

**Q. FSx バックエンドはいつ使う?**
「小規模→EBS、大規模→FSx」ではない。multi-host / Spot / host 使い捨て / 1 台の RAM 限界、のどれかが必要になったら FSx。詳細は [docs/COSTS.md](docs/COSTS.md)。

## 開発 / コントリビュート

```bash
make build   # bin/sashikid, bin/sashiki
make test    # ユニットテスト(ZFS 不要、モックで動く)
make lint    # golangci-lint
```

E2E(実 ZFS + mysqld)は Ubuntu ホスト / VM で `e2e/e2e.sh` を実行する(ループバックの zpool を使うので追加ディスク不要)。
Issue / PR 歓迎。設計の背景は [docs/SPEC.md](docs/SPEC.md) と [docs/DECISIONS.md](docs/DECISIONS.md) を参照。

## License

Apache-2.0([LICENSE](LICENSE) / [NOTICE](NOTICE))。
同梱する第三者 OSS の一覧とライセンスは [THIRD-PARTY-LICENSES.md](THIRD-PARTY-LICENSES.md)
(Apache-2.0 / MIT / BSD-3-Clause / MPL-2.0。強いコピーレフトは含まない)。
