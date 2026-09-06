# Changelog

本プロジェクトのバージョニングは [Semantic Versioning](https://semver.org/lang/ja/) に従う。
v0.x の間は API / config が安定しておらず、マイナー版で破壊的変更があり得る。

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
