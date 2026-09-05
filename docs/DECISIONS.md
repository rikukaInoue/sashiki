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

**理由**: 仕様 13-3。ホスト上の CLI 利用を摩擦なしにし、GitHub Action など外部からの呼び出しだけ守る。

**更新(v0.3)**: `twig token` サブコマンドを実装し、state.db の tokens テーブル(SHA-256 ハッシュ保存・last_used_at 記録)との照合を追加した。env トークンは後方互換として残る。DB 照合のエラーは認証失敗と区別してログに残す(無言の 401 にしない)。

## ADR-006: プロキシは「バックエンド起点の AuthSwitch 転送」方式、proxy_user は mysql_native_password

**背景**: プロキシはユーザー名 `<user>@<branch>` を見てからバックエンドを選ぶが、MySQL はサーバーが先にハンドシェイク(salt 含む)を送るため、クライアントの最初の認証応答は twig の salt に対するもので転送できない。当初は twig 自身が AuthSwitchRequest を送る設計だったが、バックエンドも(申告プラグインとユーザーの実プラグインが違うと)AuthSwitch を返すため、クライアントが「2 回目の AuthSwitch」をプロトコル違反として切断することが実機で判明した。

**決定**:
1. twig は合成ハンドシェイクでユーザー名だけ取得し、バックエンドには**わざとユーザーの実プラグインと異なる caching_sha2 を名乗って**接続する。バックエンドが必ず返す AuthSwitchRequest(新しい salt 付き)を**そのままクライアントへ転送**して認証させる。twig はパスワードを保存せず、正否はバックエンドが判断する(仕様 14-4)
2. この方式では両側の会話が handshake(0) → response(1) → switch(2) → reply(3) → result(4) と対称になり、**sequence 番号の書き換えが不要**
3. バックエンドへ申告する capability は「クライアント ∩ バックエンド ∩ **twig が合成ハンドシェイクで広告した集合(synthCaps)**」に絞る。広告していない機能(QUERY_ATTRIBUTES 等)を申告すると、クライアントの送るパケットとバックエンドの期待がずれて Malformed packet になる(実機で確認)
4. baseline import が作る proxy_user は **mysql_native_password**。AuthSwitch 1 回で認証が完結し決定的になる(caching_sha2 の full-auth は RSA 鍵交換が挟まる)。8.4 以降は native がデフォルト無効のため、caching_sha2 対応は TLS 終端(sni ルーティング)導入時に再検討

**理由**: 実機の挙動(2 回目 AuthSwitch 拒否・caps 不一致の Malformed packet)に合わせた最小の設計。認証フェーズは素直な双方向転送ループだけで済む。
