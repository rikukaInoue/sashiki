<!--
PR タイトルは Conventional Commits で:
  feat(scope): ...   → minor (機能)
  fix(scope): ...    → patch (修正)
  perf/refactor/docs → patch
  chore/ci/test      → 版に影響しない
Issue 参照は本文に (#123) の形で。リリースは release-please が自動化します
(docs/RELEASING.md 参照)。CHANGELOG は手で書かなくて OK。
-->

## 何を / なぜ

<!-- 変更内容と背景。関連 Issue: #___ -->

## テスト

<!-- 何をどう確認したか(unit / e2e / 実機) -->

## チェックリスト

- [ ] PR タイトルが Conventional Commits になっている
- [ ] `go test ./...` が通る(CI: test / lint / e2e)
- [ ] ユーザー影響がある変更はドキュメント(README / docs)も更新した
- [ ] 破壊的変更がある場合は本文に `BREAKING CHANGE:` を書いた
