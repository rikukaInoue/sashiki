# Changelog

本プロジェクトのバージョニングは [Semantic Versioning](https://semver.org/lang/ja/) に従う。
v0.x の間は API / config が安定しておらず、マイナー版で破壊的変更があり得る。

## Unreleased

### Added
- **`baseline promote <branch>`**: 検証済みブランチの現在の datadir をそのまま次の current baseline に昇格する(git の branch→main 相当)。snapshot 不変条件のため対象ブランチを graceful stop してから snapshot し、昇格後に再起動する。既存の他ブランチの origin は変えない。
- **VM レスのコンテナ実行(macOS / OrbStack)**: XFS reflink(CoW)+ process モード mysqld で、フル VM 無しに `create → reset → delete` が動く(`deploy/orbstack/`)。

### Fixed
- `baseline promote` が snapshot 後の baseline 登録に失敗すると、対象ブランチの mysqld を停止したまま抜けていた。成否に関わらず再起動するよう修正。
- `baseline import` が `mysql` / `mysqladmin` を PATH から引いており、`mysqld_bin` が PATH 外(Homebrew 等)だと失敗し得た。mysqld と同じディレクトリから解決するよう統一。
- `baseline import` 成功後の baseline 台帳登録エラーを握りつぶしていた(`baseline list` / GC から漏れる)。失敗時に警告を出すよう修正。

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
