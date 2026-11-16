#!/usr/bin/env bash
# Prueba la configuración por función contra el compute plane (Paso 2).
#
# Despliega un zip por el control plane (solo para dejar el artifact en
# MinIO) y llama POST /executions directo, porque Nest todavía no manda
# envVars ni cpus. Ver docs/configuracion.md.

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

log_info() { echo -e "${YELLOW}[config-check]${NC} $1"; }
log_ok() { echo -e "${GREEN}[✓]${NC} $1"; }
log_fail() { echo -e "${RED}[✗]${NC} $1"; exit 1; }

command -v curl >/dev/null 2>&1 || log_fail "curl requerido"
command -v jq >/dev/null 2>&1 || log_fail "jq requerido"
command -v zip >/dev/null 2>&1 || log_fail "zip requerido"
command -v docker >/dev/null 2>&1 || log_fail "docker requerido"

wait_url() {
  local url="$1"
  local name="$2"
  for _ in $(seq 1 30); do
    if curl -sf "$url" >/dev/null 2>&1; then
      log_ok "$name listo"
      return
    fi
    sleep 1
  done
  log_fail "$name no responde en $url"
}

wait_url "$COMPUTE_URL/health" "Compute plane"
wait_url "$API_URL/health" "Control plane"

SECRET="nimbus-secret-$(date +%s)-${RANDOM}"

log_info "Rechazos 400 (no ejecutan el container)"
reject() {
  local label="$1"
  local payload="$2"
  local field="$3"
  local out
  out="$(mktemp)"
  local code
  code="$(curl -sS -o "$out" -w '%{http_code}' -X POST "$COMPUTE_URL/executions" \
    -H 'Content-Type: application/json' \
    --max-time 15 \
    -d "$payload")"
  echo "  $label → HTTP $code $(cat "$out")"
  if [[ "$code" != "400" ]]; then
    log_fail "$label: se esperaba 400, llegó $code"
  fi
  local got
  got="$(jq -r '.field // empty' "$out")"
  if [[ "$got" != "$field" ]]; then
    log_fail "$label: field=$got, se esperaba $field"
  fi
  if grep -F "$SECRET" "$out" >/dev/null; then
    log_fail "$label: la respuesta incluye el valor de la variable"
  fi
  rm -f "$out"
}

reject "timeout 301" \
  '{"executionId":"bad-timeout","functionId":"fn","version":"v","timeoutSec":301}' \
  "timeoutSec"
reject "memory 8" \
  '{"executionId":"bad-memory","functionId":"fn","version":"v","memoryMb":8}' \
  "memoryMb"
reject "cpus 9" \
  '{"executionId":"bad-cpus","functionId":"fn","version":"v","cpus":9}' \
  "cpus"
reject "cpus 0.05" \
  '{"executionId":"bad-cpus-low","functionId":"fn","version":"v","cpus":0.05}' \
  "cpus"
reject "handler .." \
  '{"executionId":"bad-handler","functionId":"fn","version":"v","handler":"../x.main"}' \
  "handler"
reject "env NIMBUS_" \
  "$(jq -n --arg secret "$SECRET" '{executionId:"bad-env",functionId:"fn",version:"v",envVars:{NIMBUS_SECRET:$secret}}')" \
  "envVars"
log_ok "Los campos inválidos responden 400"

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT
mkdir -p "$WORKDIR/src"
cat >"$WORKDIR/index.js" <<'EOF'
exports.handler = async (event, context) => {
  const mode = event && event.mode;
  if (mode === 'sleep') {
    const ms = Number(event.ms) || 5000;
    await new Promise((resolve) => setTimeout(resolve, ms));
    return { slept: true, ms };
  }
  if (mode === 'env') {
    return {
      greeting: process.env.GREETING == null ? null : process.env.GREETING,
      memoryLimitInMB: context.memoryLimitInMB,
    };
  }
  if (mode === 'mem') {
    const chunks = [];
    const block = 8 * 1024 * 1024;
    const target = (Number(event.mb) || 512) * 1024 * 1024;
    let got = 0;
    while (got < target) {
      chunks.push(Buffer.alloc(block, 1));
      got += block;
    }
    return { bytes: got };
  }
  return { ok: true, handler: 'index.handler', memoryLimitInMB: context.memoryLimitInMB };
};
EOF
cat >"$WORKDIR/src/app.js" <<'EOF'
exports.main = async (event, context) => ({
  ok: true,
  handler: 'src/app.main',
  memoryLimitInMB: context.memoryLimitInMB,
});
EOF

ZIP="$WORKDIR/fn.zip"
(cd "$WORKDIR" && zip -qr "$ZIP" index.js src)

FN_NAME="config-check-$(date +%s)"
log_info "Creando función $FN_NAME y desplegando el zip"
CREATE="$(curl -sf -X POST "$API_URL/functions" \
  -H 'Content-Type: application/json' \
  -d "{\"name\":\"$FN_NAME\"}")"
FN_ID="$(echo "$CREATE" | jq -r '.id')"
[[ -n "$FN_ID" && "$FN_ID" != "null" ]] || log_fail "No se pudo crear la función: $CREATE"

DEPLOY="$(curl -sf -X POST "$API_URL/functions/$FN_ID/deploy" -F "file=@$ZIP")"
VERSION="$(echo "$DEPLOY" | jq -r '.version.version')"
[[ -n "$VERSION" && "$VERSION" != "null" ]] || log_fail "Deploy falló: $DEPLOY"
log_ok "Artifact $FN_ID versión $VERSION"

call() {
  local payload="$1"
  local out="$2"
  local maxtime="$3"
  curl -sS -o "$out" -w '%{http_code}' -X POST "$COMPUTE_URL/executions" \
    -H 'Content-Type: application/json' \
    --max-time "$maxtime" \
    -d "$payload"
}

base_payload() {
  local id="$1"
  local extra="$2"
  jq -n --arg id "$id" --arg fn "$FN_ID" --arg ver "$VERSION" --argjson extra "$extra" \
    '{executionId:$id,functionId:$fn,version:$ver} + $extra'
}

log_info "Invoke sin campos opcionales (defaults)"
OUT="$(mktemp)"
PAYLOAD="$(base_payload "defaults-$RANDOM" '{"event":{"name":"Nimbus"}}')"
CODE="$(call "$PAYLOAD" "$OUT" 30)"
echo "  HTTP $CODE $(cat "$OUT")"
[[ "$CODE" == "200" ]] || log_fail "defaults: HTTP $CODE"
jq -e '.success == true and .output.handler == "index.handler" and .output.memoryLimitInMB == 128' "$OUT" >/dev/null \
  || log_fail "defaults: salida inesperada $(cat "$OUT")"
log_ok "Defaults: handler index.handler y memoria 128 MB"

log_info "envVars visibles en process.env"
OUT="$(mktemp)"
PAYLOAD="$(jq -n --arg id "env-$RANDOM" --arg fn "$FN_ID" --arg ver "$VERSION" --arg secret "$SECRET" \
  '{executionId:$id,functionId:$fn,version:$ver,event:{mode:"env"},timeoutSec:15,memoryMb:128,handler:"index.handler",envVars:{GREETING:$secret}}')"
CODE="$(call "$PAYLOAD" "$OUT" 30)"
echo "  HTTP $CODE $(cat "$OUT")"
[[ "$CODE" == "200" ]] || log_fail "env: HTTP $CODE"
GOT="$(jq -r '.output.greeting // empty' "$OUT")"
[[ "$GOT" == "$SECRET" ]] || log_fail "env: el handler no vio GREETING"
log_ok "process.env.GREETING coincide con envVars"

log_info "Los logs del compute plane no incluyen el valor"
LOGS="$(docker compose logs --no-color compute-plane 2>/dev/null || true)"
if echo "$LOGS" | grep -F "$SECRET" >/dev/null; then
  log_fail "el valor de GREETING apareció en los logs del compute plane"
fi
if ! echo "$LOGS" | grep -F 'GREETING=***' >/dev/null; then
  log_fail "no apareció GREETING=*** en los logs (la redacción no se aplicó)"
fi
log_ok "Logs con GREETING=*** y sin el valor"

log_info "Handler src/app.main con memoryMb 256"
OUT="$(mktemp)"
PAYLOAD="$(base_payload "custom-$RANDOM" '{"event":{},"timeoutSec":15,"memoryMb":256,"handler":"src/app.main"}')"
CODE="$(call "$PAYLOAD" "$OUT" 30)"
echo "  HTTP $CODE $(cat "$OUT")"
[[ "$CODE" == "200" ]] || log_fail "handler: HTTP $CODE"
jq -e '.success == true and .output.handler == "src/app.main" and .output.memoryLimitInMB == 256' "$OUT" >/dev/null \
  || log_fail "handler: salida inesperada $(cat "$OUT")"
log_ok "src/app.main corrió con memoryLimitInMB 256"

log_info "timeoutSec 2 corta un sleep de 5s"
OUT="$(mktemp)"
PAYLOAD="$(base_payload "timeout-$RANDOM" '{"event":{"mode":"sleep","ms":5000},"timeoutSec":2,"memoryMb":128,"handler":"index.handler"}')"
CODE="$(call "$PAYLOAD" "$OUT" 30)"
echo "  HTTP $CODE $(cat "$OUT")"
[[ "$CODE" == "504" ]] || log_fail "timeout: se esperaba HTTP 504, llegó $CODE"
EXIT_CODE="$(jq -r '.exitCode' "$OUT")"
DURATION="$(jq -r '.durationMs' "$OUT")"
[[ "$EXIT_CODE" == "124" ]] || log_fail "timeout: exitCode=$EXIT_CODE"
[[ "$DURATION" =~ ^[0-9]+$ ]] || log_fail "timeout: durationMs inválido ($DURATION)"
if [[ "$DURATION" -ge 5000 ]]; then
  log_fail "timeout: durationMs=$DURATION no cortó el sleep de 5s"
fi
log_ok "Timeout: exit 124 en ${DURATION}ms"

log_info "memoryMb 128 y un handler que pide 512 MB"
OUT="$(mktemp)"
PAYLOAD="$(base_payload "mem-$RANDOM" '{"event":{"mode":"mem","mb":512},"timeoutSec":25,"memoryMb":128,"handler":"index.handler"}')"
CODE="$(call "$PAYLOAD" "$OUT" 45)"
echo "  HTTP $CODE $(cat "$OUT")"
MEM_EXIT="$(jq -r '.exitCode // empty' "$OUT")"
MEM_ERR="$(jq -r '.error // empty' "$OUT")"
if [[ "$MEM_EXIT" != "137" && "$MEM_EXIT" != "134" ]]; then
  log_fail "memoria: exitCode=$MEM_EXIT (se esperaba 137 o 134)"
fi
echo "$MEM_ERR" | grep -qi 'memoria' || log_fail "memoria: el error no explica el límite: $MEM_ERR"
log_ok "OOM: exit $MEM_EXIT — $MEM_ERR"

log_info "cpus 0.5 visible en docker inspect durante un sleep"
EXEC="cpu-$(date +%s)-$RANDOM"
OUT="$(mktemp)"
PAYLOAD="$(base_payload "$EXEC" '{"event":{"mode":"sleep","ms":8000},"timeoutSec":20,"memoryMb":128,"cpus":0.5,"handler":"index.handler"}')"
call "$PAYLOAD" "$OUT" 40 >"$WORKDIR/cpu.http" &
CURL_PID=$!
CNAME="nimbus-$EXEC"
NANO=""
QUOTA=""
PERIOD=""
for _ in $(seq 1 60); do
  if docker inspect "$CNAME" >/dev/null 2>&1; then
    NANO="$(docker inspect -f '{{.HostConfig.NanoCpus}}' "$CNAME" 2>/dev/null || true)"
    QUOTA="$(docker inspect -f '{{.HostConfig.CpuQuota}}' "$CNAME" 2>/dev/null || true)"
    PERIOD="$(docker inspect -f '{{.HostConfig.CpuPeriod}}' "$CNAME" 2>/dev/null || true)"
    if [[ -n "$NANO" && "$NANO" != "0" && "$NANO" != "<no value>" ]]; then
      break
    fi
    if [[ -n "$QUOTA" && "$QUOTA" != "0" && "$QUOTA" != "<no value>" ]]; then
      break
    fi
  fi
  sleep 0.25
done
wait "$CURL_PID" || true
CPU_CODE="$(jq -r '.success // false' "$OUT")"
echo "  container=$CNAME NanoCpus=${NANO:-vacío} CpuQuota=${QUOTA:-vacío} CpuPeriod=${PERIOD:-vacío}"
echo "  respuesta $(cat "$OUT")"
if [[ "$NANO" != "500000000" ]]; then
  if [[ -z "$PERIOD" || "$PERIOD" == "0" || "$QUOTA" == "0" ]]; then
    log_fail "cpus: no se aplicó --cpus (NanoCpus=$NANO quota=$QUOTA period=$PERIOD)"
  fi
  # quota/period ≈ 0.5
  RATIO="$(awk -v q="$QUOTA" -v p="$PERIOD" 'BEGIN { if (p+0 == 0) print 0; else printf "%.3f", q/p }')"
  echo "  quota/period=$RATIO"
  awk -v r="$RATIO" 'BEGIN { exit !(r > 0.45 && r < 0.55) }' || log_fail "cpus: ratio $RATIO no es 0.5"
fi
[[ "$CPU_CODE" == "true" ]] || log_fail "cpus: la ejecución no terminó bien: $(cat "$OUT")"
log_ok "cpus 0.5 aplicado (NanoCpus=${NANO:-$QUOTA/$PERIOD})"

if docker compose logs --no-color compute-plane 2>/dev/null | grep -F "$SECRET" >/dev/null; then
  log_fail "el valor de GREETING apareció en los logs al final"
fi

log_ok "Configuración por función verificada"
