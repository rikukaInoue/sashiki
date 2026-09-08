# Changelog

本プロジェクトのバージョニングは [Semantic Versioning](https://semver.org/lang/ja/) に従う。
v0.x の間は API / config が安定しておらず、マイナー版で破壊的変更があり得る。

> v0.6.0 以降のエントリは [release-please](https://github.com/googleapis/release-please) が
> [Conventional Commits](https://www.conventionalcommits.org/) から自動生成する(手動編集不要)。
> リリース手順は [docs/RELEASING.md](docs/RELEASING.md) を参照。v0.5.0 までは手書き。

## [0.5.2](https://github.com/rikukaInoue/sashiki/compare/v0.5.1...v0.5.2) (2026-09-08)


### Bug Fixes

* **engine:** don't pick caching_sha2 for MariaDB backends ([#219](https://github.com/rikukaInoue/sashiki/issues/219)) ([ece7b38](https://github.com/rikukaInoue/sashiki/commit/ece7b38a55588f6fc93b5db6032890805c2a0bb4))

## [0.5.1](https://github.com/rikukaInoue/sashiki/compare/v0.5.0...v0.5.1) (2026-09-08)


### Bug Fixes

* **engine/proxy:** support MySQL 8.4/9.x via caching_sha2 for proxy-&gt;backend auth ([#197](https://github.com/rikukaInoue/sashiki/issues/197) follow-up) ([#209](https://github.com/rikukaInoue/sashiki/issues/209)) ([c94de34](https://github.com/rikukaInoue/sashiki/commit/c94de3498dbbf726e0010a7e22b0e9fa9548fa58))
* **engine:** pick app_user auth plugin by backend version (MySQL 5.7..9.x) ([#211](https://github.com/rikukaInoue/sashiki/issues/211)) ([23bb406](https://github.com/rikukaInoue/sashiki/commit/23bb4069c9222636dd8ba1904b7ed38918597341))
* **security:** bump Go toolchain to 1.26.6 to clear stdlib TLS/x509 advisories ([#212](https://github.com/rikukaInoue/sashiki/issues/212)) ([02550ad](https://github.com/rikukaInoue/sashiki/commit/02550adcdba23ad0d1bbc59f1044c92e9972f8c2))

## v0.5.0 — (2026-09-08)

### Added
- **caching_sha2_password 対応(#197)** — これまで proxy(認証終端方式)が `mysql_native_password` しか話せず「MySQL 8.0 で native を有効にした構成限定」だったのを解消。ハンドシェイクで `caching_sha2_password` を advertise し、クライアントの応答長(32B=caching_sha2 / 20B=native)で判定して検証、caching_sha2 成功時は `AuthMoreData(fast_auth_success)` + `OK` を返す。これで **MySQL 8.0 のデフォルト認証や 9.x(native 廃止)** のクライアント/baseline でそのまま繋がる。go-sql-driver / PyMySQL / mysql CLI(8.0)で実機検証。
- **`auth.trust_loopback`(#198)** — 既定では loopback(127.0.0.1)からの API はトークン無しで通す(CLI 用)が、これを `false` にすると **loopback でも Bearer トークンを必須**にできる。同一ホストに信頼できない同居プロセスがいる環境向け。

### Changed
- **`baseline import` の高速化(#194)**: バルク投入セッションで `unique_checks` / `foreign_key_checks` / `sql_log_bin` を自動的に無効化する(セッション限定なので、import 後の実行時は FK/一意制約は通常どおり有効)。あわせて `--import-cnf <my.cnf>` を追加し、投入中だけ buffer pool や `innodb_flush_log_at_trx_commit` を緩めた mysqld で流し込める。実測(3M 行 / UNIQUE 索引 + FK、Apple Silicon): 約 25s → セッション変数のみ約 14.5s(-42%)→ `--import-cnf`(2G pool / trx_commit=0 / doublewrite off)併用で約 10s(-60%)。データ件数・FK 整合は不変。
- **`baseline import` の並列投入(#194)**: `--from` にディレクトリを渡すと `*.sql` を並列投入する(mydumper 出力やテーブル単位の分割ダンプ向け)。名前に `schema` を含むファイルを先に順次投入して全テーブルを作り、残りのデータファイルを `--threads N`(既定 = CPU 数)の接続で並列に流す。実測(8 テーブル / 280 万行、Apple Silicon): 単一ファイル逐次 約 14s → ディレクトリ 8 並列 約 7s(約 2 倍)。件数・テーブル数は不変。
- **baseline build の publish 経路も高速化(#194)**: `baseline build` が migration/seed を流し込む経路(`ApplyFile`)にも import と同じバルクロード用フラグを適用。大きな migration の投入が速くなる(build 用の使い捨て mysqld なのでセッション限定で安全)。
- **Terraform: `sashiki_ref` を VERSION から導出(#199)**: モジュールに `deploy/terraform/VERSION` を同梱し、`sashiki_ref` 未指定時はそこから解決する。利用側は `source` の `?ref=vX.Y.Z` を固定するだけでよく、バイナリ版がモジュールと自動一致(ref の二重指定 #193 が不要に)。

### Fixed
- **reconcile: 再起動で中断された遷移中ブランチを回収する(#206)**。sashikid が create/reset/delete の途中でクラッシュすると、ブランチが `creating` / `resetting` / `deleting` 状態のまま取り残され、reaper(running/sleeping しか触らない)にも起動時 reconcile(running→sleeping と dataset 欠損のみ扱う)にも回収されず永久に使えなくなっていた。reconcile が `creating`→error(op=create, recoverable)/ `resetting`→error(op=reset, recoverable)/ `deleting`→削除を完了、として回収するようにした(error 状態は `sashiki retry` で再駆動できる)。`ReconcileReport.Interrupted` と起動ログに件数を追加。
- **Terraform / install.sh の実戦修正(#192, #193)**: `apt-get` の dpkg ロック競合を待つ(`DPkg::Lock::Timeout`)/ `sashiki_ref` のピン留めを強調(module の `?ref=` と VERSION 由来値を一致させる)。

### Docs
- **apfs / reflink の per-branch quota ギャップ(#200)**: ZFS の `refquota` 相当が無いため `storage.default_storage_quota` が効かず、暴走ブランチへの storage admission は pool 使用率 watermark のみになることを `docs/LOCAL-DEV.md` に明記。
- README にコンテナ利用手順を追記し、フルコンテナ対応を「完了」に(#191)。

## v0.4.2 — (2026-09-07)

> v0.3.0〜v0.4.1 の詳細な差分は各 [GitHub Release](https://github.com/rikukaInoue/sashiki/releases)(自動生成ノート)を参照。ここでは主な追加・修正をまとめる。

### Added
- **`baseline promote <branch>`**: 検証済みブランチの現在の datadir をそのまま次の current baseline に昇格する(git の branch→main 相当)。snapshot 不変条件のため対象ブランチを graceful stop してから snapshot し、昇格後に再起動する。既存の他ブランチの origin は変えない。
- **VM レスのコンテナ実行(macOS / OrbStack)**: XFS reflink(CoW)+ process モード mysqld で、フル VM 無しに `create → reset → delete` が動く(`deploy/orbstack/`)。
- **VM レスの macOS ネイティブ実行が正式サポート(#113)**: `sashiki init --platform darwin` が Homebrew mysql 検出→APFS clonefile で baseline 構築→config 生成→launchd 常駐まで一括。`create → reset → delete` と proxy lazy create を実機 E2E で検証(`make e2e-darwin`、使い捨て temp root)。`docs/LOCAL-DEV.md` を実験的→正式サポートに更新(#139/#141/#142)。
- **macOS バイナリ配布**: goreleaser で darwin(arm64/amd64)を配布、`install.sh` が macOS 対応(`curl | bash`)。
- **`engine.mysql.extra_cnf`**: プロジェクト固有 my.cnf を `--defaults-file` で渡す(import / process / systemd の全経路に反映)。
- **`proxy.allowed_user`**: 接続許可ユーザーを設定可能に(未設定=app_user のみ / `""`=任意 / 名前=限定)。管理ユーザー接続向け。
- **`baseline import --db <name>`**: USE を含まない単体 DB ダンプの投入先を指定。
- **stale 表示**: origin が current baseline より古いブランチを `list` / `show` で示し、`reset` 時に警告(最新化は recreate)。

### Fixed
- **proxy DEPRECATE_EOF**: クライアントの capability に追従するよう修正。DEPRECATE_EOF を要求しないドライバ(PHP mysqlnd / Node / PyMySQL 等)で結果セットが空/エラーになる不具合を解消。
- **apfs/reflink の USED / capacity**: CoW 差分を正確に取れない値は 0 でなく「-」(不明)で表示。
- **extra_cnf の PERSIST 乗っ取り**: `--defaults-extra-file` → `--defaults-file` にし、起動前に `mysqld-auto.cnf`(SET PERSIST 残骸)を除去。
- **Terraform デプロイ実戦修正**: data device を EBS volume id から by-id で解決(Nitro nvme)/ awscli v2 zip 導入(Ubuntu 24.04)/ SSM・Secrets の平文ログ残留を `set +x` で防止 / `install.sh` のタグ直リンク取得 / instance profile のタグ除去(`iam:TagInstanceProfile` 不要)/ Route53 A レコード / `root_volume_size` / deb postinstall で `sashiki` ユーザー作成 + `/var/log` 権限。
- **systemd モードの extra_cnf**: ブランチの mysqld にも `--defaults-file` を反映(env の `MYSQLD_DEFAULTS` → ExecStart)。AppArmor が `/etc/sashiki/*.cnf` を読めるように。
- `baseline promote` が snapshot 後の baseline 登録に失敗すると、対象ブランチの mysqld を停止したまま抜けていた。成否に関わらず再起動するよう修正。
- `baseline import` が `mysql` / `mysqladmin` を PATH から引いており、`mysqld_bin` が PATH 外(Homebrew 等)だと失敗し得た。mysqld と同じディレクトリから解決するよう統一。
- `baseline import` 成功後の baseline 台帳登録エラーを握りつぶしていた(`baseline list` / GC から漏れる)。失敗時に警告を出すよう修正。
- systemd デプロイ整合性(authense 実戦 #134): `sashiki init` が生成する config を `sudo: true` に修正(sashikid は `User=sashiki` で動き zfs/systemctl を sudoers 経由で叩くため。`sudo: false` だとブランチ作成が permission denied で全滅していた、#176)。
- per-branch の `<branch>.env` を `/etc/sashiki`(root 所有で書けない)から `/run/sashiki` に移動。`sashikid.service` に `RuntimeDirectory=sashiki` を追加し、mysqld@/postgres-sashiki@ の `EnvironmentFile` も追随(#177)。
- sudoers に `baseline promote` の snapshot(`branches/*@baseline-*`)と、promote 済み baseline からの clone を追加(promote 後の運用が sudo で止まらないように、#178)。
- promote 元ブランチの `delete` が baseline snapshot ごと破棄し current baseline を宙吊りにしていたのを、dataset 上に baseline がある間は delete を拒否するよう修正(#179)。
- `baseline import` が常に root を要求し macOS ネイティブ(ログインユーザー)で使えなかったのを、apfs/reflink では非 root 実行を許可(root 時のみ mysql ユーザーへ降格)(#138)。
- macOS の `sashiki init --platform darwin` が baseline snapshot 取得前に `auto.cnf` を削除するよう修正(全ブランチが同一 server_uuid になるのを防ぐ、#80 と同趣旨)。

## v0.2.0 — public preview (2026-09-07)

初の公開リリース。設計仕様 v2 の機能を実装し、MySQL + GitHub PR プレビューの経路を実機で検証した。

### Highlights
- **branch lifecycle**: create / reset / recreate / delete / retry
- **proxy(:3306 固定)**: `dev@<branch>` ルーティング + 認証終端(方式A)+ 認証後 lazy create + TLS 終端
- **baseline**: build → validate → publish、`baseline set` ロールバック、GC(keep_last / retention)、
  組み込みローダー(`source_dir` に SQL を置くだけで refresh)
- **profile / lease**: preview / ci / sandbox + `--ttl` / `lease renew`
- **capacity**: メモリ admission、storage watermark、`sashiki capacity`
- **運用**: 起動時 reconciliation、`sashiki doctor`、orphan GC、`sashiki drain`、
  構造化ログ + Prometheus メトリクス、Web UI(データブラウザ)
- **engine**: MySQL / PostgreSQL(Postgres は直接ポート接続)
- **storage**: EBS-ZFS(既定)/ FSx-ZFS
- **配布**: GitHub Action、Terraform モジュール(RDS 互換 I/O)、deb / install.sh

### Security / correctness(実運用で検出・修正)
- proxy: 認証終端時にクライアント指定の DB を backend へ引き継ぐ(No database selected の修正)
- proxy: データフェーズの half-close + TCP keepalive
- sudoers のパス制限、AppArmor enforce プロファイル、auto.cnf 削除(server_uuid 重複防止)

### Known limitations
- API / config は未固定(v0.x)
- PostgreSQL は proxy / lazy create 非対応(直接ポート)
- FSx / multi-host / Spot は実装済みだが本番運用実績なし
- 本番 DB 用途は対象外(単一ノード、HA/レプリカなし)
