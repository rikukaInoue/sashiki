# 設計判断の記録 (ADR)

実装中に決めたことを追記していく。フォーマット: 背景 → 決定 → 理由。

## ADR-001: @init スナップショットとフックの順序

**背景**: 仕様 14-2 は「on-create の結果(マイグレーション適用済み)が reset の戻り先になる」ため @init をフック後に取得すると定めるが、仕様 15-6 は「@init は clone 直後・mysqld 起動前(クリーン状態)でのみ取得する」と定める。稼働中スナップショットは reset のたびにクラッシュリカバリを引き起こす(FSx 検証で実証済み)。

**決定**: フックの有無で経路を分ける。
- フックなし: clone → @init → start(最速。@init はベースと同一のクリーン状態)
- フックあり: clone → start → ready → on-create → **正常終了** → @init → start

**理由**: 両方の要求(フック結果を reset に含める / @init は常にクリーン)を満たす唯一の順序。フックあり時の追加コストは stop/start 1 回(数秒)で、マイグレーション適用時間に比べ誤差。

## ADR-002: 遅いバックエンドの reset は v0.1 では未実装

**背景**: 仕様 15-3 は reset の共通経路を「新クローン + 付け替え」とし、zfs rollback をその最適化と位置づける。

**決定**: v0.1 は FastRollback=true(zfs)の rollback 経路のみ実装。FastRollback=false のバックエンドでは明示的なエラーを返す。「新クローン + 付け替え」は fsx バックエンド実装(v1.0)と同時に入れる。

**理由**: v0.1 は zfs 専用であり、付け替え(ポート・接続先の切り替え)は実バックエンドなしにテスト不能。インターフェース(Capabilities.FastRollback)だけ v0.1 で切っておく。

## ADR-003: SQLite ドライバは modernc.org/sqlite

**決定**: cgo 不要の pure Go 実装を使う。

**理由**: クロスコンパイル(linux/amd64・arm64 の release ビルド)が単純になる。性能は twigd の書き込み頻度(ブランチ操作時のみ)では問題にならない。MaxOpenConns=1 で直列化。

## ADR-004: mysqld は systemd テンプレートユニットで管理

**決定**: twigd が直接 mysqld プロセスを孵化させず、`systemctl start mysqld@<branch>` を経由する。datadir とポートは `/etc/twig/<branch>.env` の EnvironmentFile で渡す。

**理由**: twigd の再起動・クラッシュとブランチ mysqld の生存を分離できる。プロセス監督(異常終了の記録)を systemd に任せられる。PoC で実証済みの構成。

## ADR-005: API 認証は「loopback 無認証 + 外部は Bearer」

**決定**: 127.0.0.1 からのリクエストは無認証、それ以外は Bearer トークン(SHA-256 定数時間比較)。トークンは環境変数から読む。

**理由**: 仕様 13-3。ホスト上の CLI 利用を摩擦なしにし、GitHub Action など外部からの呼び出しだけ守る。v0.1 ではトークン 1 本(env)。`twig token` サブコマンド(state.db 管理)は v0.3。
