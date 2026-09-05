# Contributing

## 開発フロー

1. Issue を立てる(バグ報告・提案どちらも歓迎)
2. ブランチを切って PR を出す。1 PR に修正と改善を混ぜない
3. `make test` が通ること。ロジック変更にはテストを付ける
4. 実装中に下した設計判断は `docs/DECISIONS.md` に ADR として追記する

## テスト

- `make test` — ユニット(ZFS 不要、モック)
- `make e2e-local` — macOS から Lima VM で実 ZFS + mysqld の E2E
- `sudo ./e2e/e2e.sh bin/twigd bin/twig` — Ubuntu ホスト上で直接

## 不変条件(壊さないこと)

- **@init / @baseline スナップショットは必ず mysqld の正常終了状態で撮る**
- storage / engine の実装依存をコア(internal/branch)に持ち込まない
- 自社固有処理はコアに入れず hooks に置く
