#!/usr/bin/env bash
# twig GitHub Action の本体。action.yml から呼ばれるが、単体でも実行できる
# (E2E はローカル twigd 相手にこのスクリプトを直接検証する)。
#
# 入力(環境変数):
#   TWIG_API_URL   twigd の API URL (必須)
#   TWIG_API_TOKEN Bearer トークン (twigd がリモートの場合必須)
#   TWIG_BRANCH    ブランチ名 (必須, 例: pr-123)
#   TWIG_EVENT     PR イベント (opened|reopened|synchronize|closed)
#   TWIG_OUTPUT    GITHUB_OUTPUT のパス (省略可)
#
# 出力(GITHUB_OUTPUT): host / port / user / created (true|false)
set -euo pipefail

: "${TWIG_API_URL:?TWIG_API_URL is required}"
: "${TWIG_BRANCH:?TWIG_BRANCH is required}"
: "${TWIG_EVENT:?TWIG_EVENT is required}"

auth=()
if [ -n "${TWIG_API_TOKEN:-}" ]; then
  auth=(-H "Authorization: Bearer ${TWIG_API_TOKEN}")
fi

api() {
  local method=$1 path=$2 body=${3:-}
  local args=(-sS -o /tmp/twig-action-resp.json -w '%{http_code}' -X "$method" "${auth[@]}" \
    -H "Content-Type: application/json" "${TWIG_API_URL}${path}")
  if [ -n "$body" ]; then args+=(-d "$body"); fi
  curl "${args[@]}"
}

emit() {
  [ -n "${TWIG_OUTPUT:-}" ] && echo "$1=$2" >> "$TWIG_OUTPUT"
  echo "  $1: $2"
}

case "$TWIG_EVENT" in
  closed)
    code=$(api DELETE "/v1/branches/${TWIG_BRANCH}")
    case "$code" in
      204) echo "twig: branch '${TWIG_BRANCH}' deleted" ;;
      404) echo "twig: branch '${TWIG_BRANCH}' not found (already deleted)" ;;  # 冪等
      *)   echo "twig: delete failed (HTTP $code)"; cat /tmp/twig-action-resp.json; exit 1 ;;
    esac
    ;;
  opened|reopened|synchronize)
    # exist_ok=true で冪等: 201=新規作成 / 200=既存
    code=$(api POST "/v1/branches?exist_ok=true" "{\"name\":\"${TWIG_BRANCH}\"}")
    case "$code" in
      201) created=true ;;
      200) created=false ;;
      *)   echo "twig: create failed (HTTP $code)"; cat /tmp/twig-action-resp.json; exit 1 ;;
    esac
    host=$(python3 -c 'import json;print(json.load(open("/tmp/twig-action-resp.json"))["host"])')
    port=$(python3 -c 'import json;print(json.load(open("/tmp/twig-action-resp.json"))["port"])')
    user=$(python3 -c 'import json;print(json.load(open("/tmp/twig-action-resp.json"))["user"])')
    echo "twig: branch '${TWIG_BRANCH}' ready (created=${created})"
    emit host "$host"
    emit port "$port"
    emit user "$user"
    emit created "$created"
    ;;
  *)
    echo "twig: event '${TWIG_EVENT}' is not handled (noop)"
    ;;
esac
