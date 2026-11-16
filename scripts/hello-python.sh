#!/usr/bin/env bash
# Crea, despliega e invoca examples/hello-python.
# El invoke va al compute plane con runtime=python3.12: Nest no reenvía ese campo.

set -euo pipefail

API_URL="${API_URL:-http://localhost:3000}"
COMPUTE_URL="${COMPUTE_URL:-http://localhost:8080}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

command -v curl >/dev/null && command -v jq >/dev/null && command -v zip >/dev/null && command -v docker >/dev/null

docker image inspect nimbus-python >/dev/null 2>&1 || docker build -t nimbus-python "$ROOT/runtime-python"

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT
(cd "$ROOT/examples/hello-python" && zip -q "$WORKDIR/fn.zip" index.py)

NAME="hello-python-$(date +%H%M%S)"
CREATE="$(curl -sf -X POST "$API_URL/functions" -H 'Content-Type: application/json' -d "{\"name\":\"$NAME\"}")"
FN="$(echo "$CREATE" | jq -r '.id')"
DEPLOY="$(curl -sf -X POST "$API_URL/functions/$FN/deploy" -F "file=@$WORKDIR/fn.zip")"
VER="$(echo "$DEPLOY" | jq -r '.version.version')"

invoke() {
  local id="$1"
  curl -sf --max-time 40 -X POST "$COMPUTE_URL/executions" -H 'Content-Type: application/json' \
    -d "{\"executionId\":\"$id\",\"functionId\":\"$FN\",\"version\":\"$VER\",\"runtime\":\"python3.12\",\"handler\":\"index.handler\",\"event\":{\"name\":\"Nimbus\"}}"
}

echo "función $NAME  id=$FN  version=$VER"
echo "--- cold ---"
invoke "hello-py-cold-$RANDOM" | jq '{success,coldStart,initMs,durationMs,output,error}'
echo "--- warm ---"
invoke "hello-py-warm-$RANDOM" | jq '{success,coldStart,initMs,durationMs,output,error}'
