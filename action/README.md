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
          profile: preview          # lifecycle profile(省略時はサーバー既定)
          on_close: delete          # delete | keep(keep は TTL 回収に任せる)
          comment: "true"           # PR に接続先をコメント(マーカー付きで冪等更新)
```

- ブランチ名は既定で `pr-<PR番号>`(`branch:` で上書き可)
- **冪等**: create は `exist_ok=true`(既存なら 200)、closed の delete は 404 も成功扱い
- `synchronize`(push)でも create を呼ぶだけ(TTL 削除後の復活を兼ねる)。**migration の再適用は自動では行わない** — 利用者が `sashiki recreate` を明示的に選ぶ(v2 仕様 22-1)
- **provenance**: `source` を省略すると `{"type":"github_pr","repository":"<repo>","ref":"<PR番号>"}` を自動生成して渡す(`sashiki show <name> --json` の `source` に残る)。任意の JSON で上書き可
- outputs: `host` / `port` / `user` — 後続 step でプレビュー環境に渡せる(`DB_HOST` / `DB_PORT` / `DB_USER` へ)

## close イベントの取りこぼしについて

Actions の closed イベントは取りこぼすことがあるため、削除はこの action だけに依存せず
TTL 自動回収(issue #8)と併用する。
