#!/usr/bin/env bash
# Compila examples/hello-go a un binario estático, lo despliega e invoca con runtime=go1.x.
# Nest no reenvía runtime: el invoke va a POST :8080/executions. El handler se omite
# y el compute plane usa el binario /var/task/bootstrap.

set -euo pipefail

API_URL="${API_URL:-http://localhost:3000}"
COMPUTE_URL="${COMPUTE_URL:-http://localhost:8080}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

command -v curl >/dev/null && command -v jq >/dev/null && command -v zip >/dev/null && command -v docker >/dev/null && command -v go >/dev/null

docker image inspect nimbus-go >/dev/null 2>&1 || docker build -t nimbus-go "$ROOT/runtime-go"

ARCH="$(docker info -f '{{.Architecture}}')"
case "$ARCH" in
  x86_64|amd64) GOARCH_BIN=amd64 ;;
  aarch64|arm64) GOARCH_BIN=arm64 ;;
  *) GOARCH_BIN=amd64 ;;
esac

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT
(
  cd "$ROOT/examples/hello-go"
  CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH_BIN" go build -ldflags='-s -w' -o "$WORKDIR/bootstrap" .
)
(cd "$WORKDIR" && zip -q fn.zip bootstrap)

NAME="hello-go-$(date +%H%M%S)"
CREATE="$(curl -sf -X POST "$API_URL/functions" -H 'Content-Type: application/json' -d "{\"name\":\"$NAME\"}")"
FN="$(echo "$CREATE" | jq -r '.id')"
DEPLOY="$(curl -sf -X POST "$API_URL/functions/$FN/deploy" -F "file=@$WORKDIR/fn.zip")"
VER="$(echo "$DEPLOY" | jq -r '.version.version')"

invoke() {
  local id="$1"
  curl -sf --max-time 40 -X POST "$COMPUTE_URL/executions" -H 'Content-Type: application/json' \
    -d "{\"executionId\":\"$id\",\"functionId\":\"$FN\",\"version\":\"$VER\",\"runtime\":\"go1.x\",\"event\":{\"name\":\"Nimbus\"}}"
}

echo "función $NAME  id=$FN  version=$VER  (binario linux/$GOARCH_BIN, handler bootstrap)"
echo "--- cold ---"
invoke "hello-go-cold-$RANDOM" | jq '{success,coldStart,initMs,durationMs,output,error}'
echo "--- warm ---"
invoke "hello-go-warm-$RANDOM" | jq '{success,coldStart,initMs,durationMs,output,error}'
