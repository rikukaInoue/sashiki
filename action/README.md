# sashiki branch action

PR の open/close に連動して sashiki の DB ブランチを作成・削除する composite action。

## 使い方

```yaml
# .github/workflows/sashiki.yml
name: sashiki
on:
  pull_request:
    types: [opened, reopened, synchronize, closed]

jobs:
  branch:
    runs-on: [self-hosted, vpc]   # sashikid の API に届くランナー
    steps:
      - uses: rikukaInoue/sashiki/action@main
        with:
          api_url: http://sashiki.internal:8080
          token: ${{ secrets.SASHIKI_API_TOKEN }}
          comment: "true"           # PR に接続先をコメント(マーカー付きで冪等更新)
```

- ブランチ名は既定で `pr-<PR番号>`(`branch:` で上書き可)
- **冪等**: create は `exist_ok=true`(既存なら 200)、closed の delete は 404 も成功扱い
- `synchronize`(push)でも create を呼ぶだけ。reset + マイグレーション再適用はフック連携(v0.3)で扱う
- outputs: `host` / `port` / `user` — 後続 step でプレビュー環境に渡せる

## close イベントの取りこぼしについて

Actions の closed イベントは取りこぼすことがあるため、削除はこの action だけに依存せず
TTL 自動回収(issue #8)と併用する。
