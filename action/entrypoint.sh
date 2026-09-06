#!/usr/bin/env bash
# sashiki GitHub Action の本体。action.yml から呼ばれるが、単体でも実行できる
# (E2E はローカル sashikid 相手にこのスクリプトを直接検証する)。
#
# 入力(環境変数):
#   SASHIKI_API_URL   sashikid の API URL (必須)
#   SASHIKI_API_TOKEN Bearer トークン (sashikid がリモートの場合必須)
#   SASHIKI_BRANCH    ブランチ名 (必須, 例: pr-123)
#   SASHIKI_EVENT     PR イベント (opened|reopened|synchronize|closed)
#   SASHIKI_PROFILE   lifecycle profile (省略可)
#   SASHIKI_SOURCE    provenance の source JSON (省略可。未指定なら github_pr を自動生成)
#   SASHIKI_ON_CLOSE  closed 時の動作 delete|keep (省略時 delete)
#   SASHIKI_PR        PR 番号 (source 自動生成用, 省略可)
#   SASHIKI_OUTPUT    GITHUB_OUTPUT のパス (省略可)
#
# 出力(GITHUB_OUTPUT): host / port / user / created (true|false)
set -euo pipefail

: "${SASHIKI_API_URL:?SASHIKI_API_URL is required}"
: "${SASHIKI_BRANCH:?SASHIKI_BRANCH is required}"
: "${SASHIKI_EVENT:?SASHIKI_EVENT is required}"

auth=()
if [ -n "${SASHIKI_API_TOKEN:-}" ]; then
  auth=(-H "Authorization: Bearer ${SASHIKI_API_TOKEN}")
fi

# 応答ボディの書き先。固定パスだと別ユーザーの残骸ファイルで書き込みに
# 失敗する(curl exit 23)ため mktemp を使う。
RESP=$(mktemp "${TMPDIR:-/tmp}/sashiki-action-resp.XXXXXX")
trap 'rm -f "$RESP"' EXIT

api() {
  local method=$1 path=$2 body=${3:-}
  local args=(-sS -o "$RESP" -w '%{http_code}' -X "$method" "${auth[@]}" \
    -H "Content-Type: application/json" "${SASHIKI_API_URL}${path}")
  if [ -n "$body" ]; then args+=(-d "$body"); fi
  curl "${args[@]}"
}

emit() {
  [ -n "${SASHIKI_OUTPUT:-}" ] && echo "$1=$2" >> "$SASHIKI_OUTPUT"
  echo "  $1: $2"
}

# create の body を組み立てる。source は JSON として埋め込むため python で安全に構築する
build_body() {
  python3 - <<'PY'
import json, os, sys
body = {"name": os.environ["SASHIKI_BRANCH"]}
if os.environ.get("SASHIKI_PROFILE"):
    body["profile"] = os.environ["SASHIKI_PROFILE"]
src = os.environ.get("SASHIKI_SOURCE", "")
if src:
    try:
        body["source"] = json.loads(src)
    except ValueError as e:
        print(f"sashiki: source is not valid JSON: {e}", file=sys.stderr)
        sys.exit(1)
elif os.environ.get("SASHIKI_PR") and os.environ.get("GITHUB_REPOSITORY"):
    body["source"] = {
        "type": "github_pr",
        "repository": os.environ["GITHUB_REPOSITORY"],
        "ref": os.environ["SASHIKI_PR"],
    }
print(json.dumps(body))
PY
}

case "$SASHIKI_EVENT" in
  closed)
    if [ "${SASHIKI_ON_CLOSE:-delete}" = "keep" ]; then
      echo "sashiki: on_close=keep のため削除しない (TTL 回収に任せる)"
      exit 0
    fi
    code=$(api DELETE "/v1/branches/${SASHIKI_BRANCH}")
    case "$code" in
      204) echo "sashiki: branch '${SASHIKI_BRANCH}' deleted" ;;
      404) echo "sashiki: branch '${SASHIKI_BRANCH}' not found (already deleted)" ;;  # 冪等
      *)   echo "sashiki: delete failed (HTTP $code)"; cat "$RESP"; exit 1 ;;
    esac
    ;;
  opened|reopened|synchronize)
    # exist_ok=true で冪等: 201=新規作成 / 200=既存。
    # synchronize でも create のみ(TTL 削除後の復活を兼ねる)。migration の
    # 再適用は利用者が recreate を明示的に選ぶ(v2 仕様 22-1)。
    # profile は #34 マージ前のサーバーでは未知フィールドとして無視される(前方互換)。
    body=$(build_body) || exit 1
    code=$(api POST "/v1/branches?exist_ok=true" "$body")
    case "$code" in
      201) created=true ;;
      200) created=false ;;
      *)   echo "sashiki: create failed (HTTP $code)"; cat "$RESP"; exit 1 ;;
    esac
    host=$(RESP="$RESP" python3 -c 'import json,os;print(json.load(open(os.environ["RESP"]))["host"])')
    port=$(RESP="$RESP" python3 -c 'import json,os;print(json.load(open(os.environ["RESP"]))["port"])')
    user=$(RESP="$RESP" python3 -c 'import json,os;print(json.load(open(os.environ["RESP"]))["user"])')
    echo "sashiki: branch '${SASHIKI_BRANCH}' ready (created=${created})"
    emit host "$host"
    emit port "$port"
    emit user "$user"
    emit created "$created"
    ;;
  *)
    echo "sashiki: event '${SASHIKI_EVENT}' is not handled (noop)"
    ;;
esac
