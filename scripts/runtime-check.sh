#!/usr/bin/env bash
# Prueba python3.12 y go1.x contra el compute plane.
# El zip entra por el control plane (MinIO). El invoke va a POST :8080/executions
# con el campo runtime, porque Nest todavía no lo reenvía.

set -euo pipefail

API_URL="${API_URL:-http://localhost:3000}"
COMPUTE_URL="${COMPUTE_URL:-http://localhost:8080}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$ROOT"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

log_info() { echo -e "${YELLOW}[runtime-check]${NC} $1"; }
log_ok() { echo -e "${GREEN}[✓]${NC} $1"; }
log_fail() { echo -e "${RED}[✗]${NC} $1"; exit 1; }

command -v curl >/dev/null || log_fail "curl requerido"
command -v jq >/dev/null || log_fail "jq requerido"
command -v zip >/dev/null || log_fail "zip requerido"
command -v docker >/dev/null || log_fail "docker requerido"
command -v go >/dev/null || log_fail "go requerido"
command -v python3 >/dev/null || log_fail "python3 requerido"

wait_url() {
  local url="$1" name="$2"
  for _ in $(seq 1 40); do
    if curl -sf "$url" >/dev/null 2>&1; then
      return
    fi
    sleep 1
  done
  log_fail "$name no responde en $url"
}

wait_url "$COMPUTE_URL/health" "Compute plane"
wait_url "$API_URL/health" "Control plane"

log_info "Imágenes nimbus-python y nimbus-go"
docker build -t nimbus-python "$ROOT/runtime-python" >/dev/null
docker build -t nimbus-go "$ROOT/runtime-go" >/dev/null
log_ok "Imágenes listas"

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

reject() {
  local code
  code="$(curl -sS -o "$WORKDIR/reject.json" -w '%{http_code}' -X POST "$COMPUTE_URL/executions" \
    -H 'Content-Type: application/json' --max-time 15 \
    -d '{"executionId":"bad-rt","functionId":"fn","version":"v","runtime":"ruby"}')"
  echo "  ruby → HTTP $code $(cat "$WORKDIR/reject.json")"
  [[ "$code" == "400" ]] || log_fail "runtime ruby: se esperaba 400, llegó $code"
  [[ "$(jq -r '.field' "$WORKDIR/reject.json")" == "runtime" ]] || log_fail "field runtime ausente"
  log_ok "runtime desconocido responde 400"
}
reject

deploy() {
  local name="$1" zip="$2"
  local create id deploy ver
  create="$(curl -sf -X POST "$API_URL/functions" -H 'Content-Type: application/json' -d "{\"name\":\"$name\"}")"
  id="$(echo "$create" | jq -r '.id')"
  [[ -n "$id" && "$id" != "null" ]] || log_fail "No se pudo crear $name: $create"
  deploy="$(curl -sf -X POST "$API_URL/functions/$id/deploy" -F "file=@$zip")"
  ver="$(echo "$deploy" | jq -r '.version.version')"
  [[ -n "$ver" && "$ver" != "null" ]] || log_fail "Deploy de $name falló: $deploy"
  echo "$id $ver"
}

call() {
  local payload="$1" out="$2" maxtime="$3"
  curl -sS -o "$out" -w '%{http_code}' -X POST "$COMPUTE_URL/executions" \
    -H 'Content-Type: application/json' --max-time "$maxtime" -d "$payload"
}

# $1 runtime $2 function id $3 version $4 handler json fragment (may be empty) $5 extra json $6 outfile $7 maxtime
invoke() {
  local rt="$1" fn="$2" ver="$3" handler="$4" extra="$5" out="$6" maxtime="$7"
  local payload
  payload="$(jq -n --arg id "$8" --arg fn "$fn" --arg ver "$ver" --arg rt "$rt" --argjson extra "$extra" \
    --arg handler "$handler" \
    '{executionId:$id, functionId:$fn, version:$ver, runtime:$rt} + $extra + (if $handler == "" then {} else {handler:$handler} end)')"
  call "$payload" "$out" "$maxtime"
}

STAMP="$(date +%H%M%S)-$RANDOM"

log_info "Empaquetando hello-python"
(cd "$ROOT/examples/hello-python" && zip -q "$WORKDIR/hello-python.zip" index.py)
read -r PY_FN PY_VER <<<"$(deploy "rt-py-$STAMP" "$WORKDIR/hello-python.zip")"
log_ok "python $PY_FN $PY_VER"

log_info "Compilando hello-go (binario estático linux)"
ARCH="$(docker info -f '{{.Architecture}}')"
case "$ARCH" in
  x86_64|amd64) GOARCH_BIN=amd64 ;;
  aarch64|arm64) GOARCH_BIN=arm64 ;;
  *) GOARCH_BIN=amd64 ;;
esac
(
  cd "$ROOT/examples/hello-go"
  CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH_BIN" go build -ldflags='-s -w' -o "$WORKDIR/bootstrap" .
)
(cd "$WORKDIR" && zip -q hello-go.zip bootstrap)
read -r GO_FN GO_VER <<<"$(deploy "rt-go-$STAMP" "$WORKDIR/hello-go.zip")"
log_ok "go $GO_FN $GO_VER (GOARCH=$GOARCH_BIN)"

run_runtime() {
  local label="$1" rt="$2" fn="$3" ver="$4" handler="$5"
  local out code

  log_info "$label cold"
  out="$WORKDIR/${label}-cold.json"
  code="$(invoke "$rt" "$fn" "$ver" "$handler" '{"event":{"name":"Ada"}}' "$out" 40 "cold-${label}-$RANDOM")"
  echo "  HTTP $code $(jq -c '{success,coldStart,initMs,durationMs,output,error,exitCode}' "$out")"
  [[ "$code" == "200" ]] || log_fail "$label cold HTTP $code"
  jq -e --arg rt "$rt" '.success == true and .coldStart == true and .output.runtime == $rt and .output.uid == 1000 and (.output.message | test("Ada"))' "$out" >/dev/null \
    || log_fail "$label cold inesperado: $(cat "$out")"
  local cold_ms cold_init
  cold_ms="$(jq -r '.durationMs' "$out")"
  cold_init="$(jq -r '.initMs' "$out")"
  log_ok "$label cold durationMs=$cold_ms initMs=$cold_init uid=1000"

  log_info "$label warm"
  out="$WORKDIR/${label}-warm.json"
  code="$(invoke "$rt" "$fn" "$ver" "$handler" '{"event":{"name":"Ada"}}' "$out" 30 "warm-${label}-$RANDOM")"
  echo "  HTTP $code $(jq -c '{success,coldStart,initMs,durationMs,output}' "$out")"
  [[ "$code" == "200" ]] || log_fail "$label warm HTTP $code"
  jq -e --arg rt "$rt" '.success == true and .coldStart == false and .output.runtime == $rt and .output.uid == 1000' "$out" >/dev/null \
    || log_fail "$label warm inesperado: $(cat "$out")"
  local warm_ms
  warm_ms="$(jq -r '.durationMs' "$out")"
  log_ok "$label warm durationMs=$warm_ms coldStart=false"

  log_info "$label probe (trace, fs, red)"
  out="$WORKDIR/${label}-probe.json"
  code="$(invoke "$rt" "$fn" "$ver" "$handler" '{"event":{"mode":"probe"}}' "$out" 20 "probe-${label}-$RANDOM")"
  echo "  HTTP $code $(jq -c '.output' "$out")"
  [[ "$code" == "200" ]] || log_fail "$label probe HTTP $code"
  jq -e '.success == true and .output.uid == 1000 and (.output.traceId | length) > 0 and .output.taskWrite != "ok" and .output.tmpWrite == "ok" and .output.net != "ok"' "$out" >/dev/null \
    || log_fail "$label probe inesperado: $(cat "$out")"
  log_ok "$label uid=1000, trace, /var/task ro, /tmp rw, red bloqueada"

  log_info "$label timeout"
  out="$WORKDIR/${label}-timeout.json"
  code="$(invoke "$rt" "$fn" "$ver" "$handler" '{"event":{"mode":"sleep","ms":5000},"timeoutSec":2,"memoryMb":128}' "$out" 30 "to-${label}-$RANDOM")"
  echo "  HTTP $code $(jq -c '{exitCode,durationMs,error}' "$out")"
  [[ "$code" == "504" ]] || log_fail "$label timeout HTTP $code"
  local dur
  dur="$(jq -r '.durationMs' "$out")"
  [[ "$(jq -r '.exitCode' "$out")" == "124" && "$dur" -lt 5000 ]] || log_fail "$label timeout no cortó: $(cat "$out")"
  log_ok "$label timeout exit 124 en ${dur}ms"

  log_info "$label error de handler"
  out="$WORKDIR/${label}-error.json"
  code="$(invoke "$rt" "$fn" "$ver" "$handler" '{"event":{"mode":"error","message":"boom"}}' "$out" 30 "err-${label}-$RANDOM")"
  echo "  HTTP $code $(jq -c '{success,exitCode,error}' "$out")"
  [[ "$code" == "200" ]] || log_fail "$label error HTTP $code"
  jq -e '.success == false and (.error | test("boom"))' "$out" >/dev/null \
    || log_fail "$label error inesperado: $(cat "$out")"
  log_ok "$label error del handler (sin ser timeout ni OOM)"

  log_info "$label OOM"
  out="$WORKDIR/${label}-oom.json"
  code="$(invoke "$rt" "$fn" "$ver" "$handler" '{"event":{"mode":"mem","mb":512},"timeoutSec":25,"memoryMb":128}' "$out" 45 "oom-${label}-$RANDOM")"
  echo "  HTTP $code $(jq -c '{success,exitCode,error,durationMs}' "$out")"
  local exitc
  exitc="$(jq -r '.exitCode // empty' "$out")"
  [[ "$exitc" == "137" || "$exitc" == "134" ]] || log_fail "$label OOM exit=$exitc $(cat "$out")"
  echo "$(jq -r '.error // empty' "$out")" | grep -qi 'memoria' || log_fail "$label OOM sin texto de memoria"
  log_ok "$label OOM exit $exitc"

  echo "NUM $label cold_ms=$cold_ms cold_init=$cold_init warm_ms=$warm_ms"
}

# handler vacío = el default del runtime (bootstrap para go)
run_runtime python python3.12 "$PY_FN" "$PY_VER" "index.handler"
run_runtime go go1.x "$GO_FN" "$GO_VER" ""

log_info "Métrica runtime"
METRICS="$(curl -sf "$COMPUTE_URL/metrics")"
echo "$METRICS" | grep -F 'runtime="python3.12"' >/dev/null || log_fail "falta la etiqueta runtime=python3.12"
echo "$METRICS" | grep -F 'runtime="go1.x"' >/dev/null || log_fail "falta la etiqueta runtime=go1.x"
echo "$METRICS" | grep -E 'nimbus_invocations_total\{[^}]*runtime="python3.12"' >/dev/null \
  || log_fail "nimbus_invocations_total no tiene runtime=python3.12"
log_ok "nimbus_invocations_total lleva runtime (python3.12 y go1.x)"

log_ok "Runtimes python3.12 y go1.x verificados"
