#!/usr/bin/env bash
# sashiki GitHub Action の本体。action.yml から呼ばれるが、単体でも実行できる
# (E2E はローカル sashikid 相手にこのスクリプトを直接検証する)。
#
# 入力(環境変数):
#   SASHIKI_API_URL   sashikid の API URL (必須)
#   SASHIKI_API_TOKEN Bearer トークン (sashikid がリモートの場合必須)
#   SASHIKI_BRANCH    ブランチ名 (必須, 例: pr-123)
#   SASHIKI_EVENT     PR イベント (opened|reopened|synchronize|closed)
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

api() {
  local method=$1 path=$2 body=${3:-}
  local args=(-sS -o /tmp/sashiki-action-resp.json -w '%{http_code}' -X "$method" "${auth[@]}" \
    -H "Content-Type: application/json" "${SASHIKI_API_URL}${path}")
  if [ -n "$body" ]; then args+=(-d "$body"); fi
  curl "${args[@]}"
}

emit() {
  [ -n "${SASHIKI_OUTPUT:-}" ] && echo "$1=$2" >> "$SASHIKI_OUTPUT"
  echo "  $1: $2"
}

case "$SASHIKI_EVENT" in
  closed)
    code=$(api DELETE "/v1/branches/${SASHIKI_BRANCH}")
    case "$code" in
      204) echo "sashiki: branch '${SASHIKI_BRANCH}' deleted" ;;
      404) echo "sashiki: branch '${SASHIKI_BRANCH}' not found (already deleted)" ;;  # 冪等
      *)   echo "sashiki: delete failed (HTTP $code)"; cat /tmp/sashiki-action-resp.json; exit 1 ;;
    esac
    ;;
  opened|reopened|synchronize)
    # exist_ok=true で冪等: 201=新規作成 / 200=既存
    code=$(api POST "/v1/branches?exist_ok=true" "{\"name\":\"${SASHIKI_BRANCH}\"}")
    case "$code" in
      201) created=true ;;
      200) created=false ;;
      *)   echo "sashiki: create failed (HTTP $code)"; cat /tmp/sashiki-action-resp.json; exit 1 ;;
    esac
    host=$(python3 -c 'import json;print(json.load(open("/tmp/sashiki-action-resp.json"))["host"])')
    port=$(python3 -c 'import json;print(json.load(open("/tmp/sashiki-action-resp.json"))["port"])')
    user=$(python3 -c 'import json;print(json.load(open("/tmp/sashiki-action-resp.json"))["user"])')
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
