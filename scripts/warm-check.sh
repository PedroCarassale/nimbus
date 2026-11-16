#!/usr/bin/env bash
# Mide cold start contra warm start y comprueba que el pool suelta
# los containers inactivos, y que un timeout o un OOM no se reutilizan.
#
# Pega directo a POST /executions. Nest todavía no reenvía coldStart.
# Requiere el stack arriba (make up). Al terminar restaura el compute plane.

set -euo pipefail

API_URL="${API_URL:-http://localhost:3000}"
COMPUTE_URL="${COMPUTE_URL:-http://localhost:8080}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$ROOT"

IDLE_SEC="${NIMBUS_WARM_IDLE_SEC_CHECK:-6}"
ROUNDS="${WARM_ROUNDS:-3}"
WARMS_PER_ROUND="${WARM_REPEATS:-4}"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

log_info() { echo -e "${YELLOW}[warm-check]${NC} $1"; }
log_ok() { echo -e "${GREEN}[✓]${NC} $1"; }
log_fail() { echo -e "${RED}[✗]${NC} $1"; exit 1; }

command -v curl >/dev/null 2>&1 || log_fail "curl requerido"
command -v jq >/dev/null 2>&1 || log_fail "jq requerido"
command -v zip >/dev/null 2>&1 || log_fail "zip requerido"
command -v docker >/dev/null 2>&1 || log_fail "docker requerido"

wait_url() {
  local url="$1"
  local name="$2"
  for _ in $(seq 1 120); do
    if curl -sf "$url" >/dev/null 2>&1; then
      log_ok "$name listo"
      return
    fi
    sleep 1
  done
  log_fail "$name no responde en $url"
}

restore_compute() {
  log_info "Restaurando el compute plane (compose)..."
  (cd "$ROOT" && docker compose up -d --no-deps --force-recreate --no-build compute-plane) || true
  wait_url "$COMPUTE_URL/health" "Compute plane" || true
}

trap restore_compute EXIT

if ! curl -sf "$API_URL/health" >/dev/null 2>&1; then
  log_fail "El stack no está corriendo. Ejecutá: make up"
fi

log_info "Compute plane con warm activo e idle=${IDLE_SEC}s"
(
  cd "$ROOT"
  NIMBUS_WARM=1 \
  NIMBUS_WARM_IDLE_SEC="$IDLE_SEC" \
  NIMBUS_FN_CONCURRENCY="${NIMBUS_FN_CONCURRENCY_CHECK:-8}" \
  NIMBUS_MAX_CONTAINERS="${NIMBUS_MAX_CONTAINERS_CHECK:-8}" \
    docker compose up -d --no-deps --force-recreate --no-build compute-plane
)
wait_url "$COMPUTE_URL/health" "Compute plane"

WORKDIR="$(mktemp -d)"
# shellcheck disable=SC2064
trap "rm -rf '$WORKDIR'; restore_compute" EXIT

cat >"$WORKDIR/index.js" <<'EOF'
let n = 0;
exports.handler = async (event, context) => {
  n += 1;
  const mark = event && event.mark ? String(event.mark) : "";
  if (mark) {
    console.log("mark " + mark);
  }
  const mode = event && event.mode;
  if (mode === "sleep") {
    const ms = Number(event.ms) || 5000;
    await new Promise((resolve) => setTimeout(resolve, ms));
    return { slept: true, ms, n, pid: process.pid };
  }
  if (mode === "mem") {
    const chunks = [];
    const block = 8 * 1024 * 1024;
    const target = (Number(event.mb) || 512) * 1024 * 1024;
    let got = 0;
    while (got < target) {
      chunks.push(Buffer.alloc(block, 1));
      got += block;
    }
    return { bytes: got, n, pid: process.pid };
  }
  return {
    n,
    pid: process.pid,
    mark,
    memoryLimitInMB: context.memoryLimitInMB,
  };
};
EOF
(cd "$WORKDIR" && zip -q fn.zip index.js)

FN_NAME="warm-check-$(date +%s)"
log_info "Creando función $FN_NAME"
CREATE="$(curl -sf -X POST "$API_URL/functions" \
  -H 'Content-Type: application/json' \
  -d "{\"name\":\"$FN_NAME\"}")"
FN_ID="$(echo "$CREATE" | jq -r '.id')"
[[ -n "$FN_ID" && "$FN_ID" != "null" ]] || log_fail "No se pudo crear la función: $CREATE"
DEPLOY="$(curl -sf -X POST "$API_URL/functions/$FN_ID/deploy" -F "file=@$WORKDIR/fn.zip")"
VERSION="$(echo "$DEPLOY" | jq -r '.version.version')"
[[ -n "$VERSION" && "$VERSION" != "null" ]] || log_fail "Deploy falló: $DEPLOY"
log_ok "Artifact $FN_ID versión $VERSION"

call() {
  local id="$1"
  local extra="$2"
  local maxtime="$3"
  local out="$4"
  local payload
  payload="$(jq -n --arg id "$id" --arg fn "$FN_ID" --arg ver "$VERSION" --argjson extra "$extra" \
    '{executionId:$id,functionId:$fn,version:$ver} + $extra')"
  curl -sS -o "$out" -w '%{http_code}' -X POST "$COMPUTE_URL/executions" \
    -H 'Content-Type: application/json' \
    --max-time "$maxtime" \
    -d "$payload"
}

COLD_FILE="$WORKDIR/cold.txt"
WARM_FILE="$WORKDIR/warm.txt"
INIT_FILE="$WORKDIR/init.txt"
: >"$COLD_FILE"
: >"$WARM_FILE"
: >"$INIT_FILE"

log_info "Primera invocación (descarga el zip y arranca un container)"
PRIME_OUT="$WORKDIR/prime.json"
PRIME_CODE="$(call "prime-$RANDOM" '{"event":{"mark":"prime"},"timeoutSec":20,"memoryMb":128,"handler":"index.handler"}' 30 "$PRIME_OUT")"
echo "  HTTP $PRIME_CODE $(cat "$PRIME_OUT")"
[[ "$PRIME_CODE" == "200" ]] || log_fail "prime: HTTP $PRIME_CODE"
jq -e '.success == true and .coldStart == true and (.initMs > 0)' "$PRIME_OUT" >/dev/null \
  || log_fail "prime: se esperaba coldStart y initMs $(cat "$PRIME_OUT")"
PRIME_MS="$(jq -r '.durationMs' "$PRIME_OUT")"
PRIME_INIT="$(jq -r '.initMs' "$PRIME_OUT")"
log_ok "Primera invocación: durationMs=${PRIME_MS} initMs=${PRIME_INIT} (incluye la descarga)"

log_info "Series cold/warm (${ROUNDS} cold, $((ROUNDS * WARMS_PER_ROUND)) warm). El código ya está en disco."
for round in $(seq 1 "$ROUNDS"); do
  local_pid=""
  for i in $(seq 0 "$WARMS_PER_ROUND"); do
    id="r${round}-${i}-$RANDOM"
    mark="r${round}i${i}"
    extra="$(jq -n --arg round "$round" --arg mark "$mark" \
      '{event:{mark:$mark},timeoutSec:20,memoryMb:128,handler:"index.handler",envVars:{ROUND:$round}}')"
    out="$WORKDIR/${id}.json"
    code="$(call "$id" "$extra" 30 "$out")"
    [[ "$code" == "200" ]] || log_fail "ronda $round invoke $i: HTTP $code $(cat "$out")"
    cold="$(jq -r '.coldStart' "$out")"
    dur="$(jq -r '.durationMs' "$out")"
    init="$(jq -r '.initMs' "$out")"
    n="$(jq -r '.output.n' "$out")"
    pid="$(jq -r '.output.pid' "$out")"
    echo "  ronda $round #$i coldStart=$cold durationMs=$dur initMs=$init n=$n pid=$pid"
    if [[ "$i" == "0" ]]; then
      [[ "$cold" == "true" ]] || log_fail "ronda $round: el primero no fue cold"
      [[ "$init" -gt 0 ]] || log_fail "ronda $round: initMs vacío"
      [[ "$n" == "1" ]] || log_fail "ronda $round: el container nuevo no arrancó en n=1 (n=$n)"
      echo "$dur" >>"$COLD_FILE"
      echo "$init" >>"$INIT_FILE"
      local_pid="$pid"
    else
      [[ "$cold" == "false" ]] || log_fail "ronda $round invoke $i: se esperaba warm"
      [[ "$init" == "0" ]] || log_fail "ronda $round invoke $i: initMs=$init en un warm"
      expect=$((i + 1))
      [[ "$n" == "$expect" ]] || log_fail "ronda $round: n=$n, se esperaba $expect (el estado del módulo no se conservó)"
      [[ "$pid" == "$local_pid" ]] || log_fail "ronda $round: el pid cambió ($local_pid → $pid)"
      echo "$dur" >>"$WARM_FILE"
    fi
  done
done

median() {
  jq -s 'sort | if length == 0 then 0 elif (length % 2) == 1 then .[(length / 2) | floor] else (.[(length / 2) - 1] + .[(length / 2)]) / 2 end'
}

COLD_MED="$(median <"$COLD_FILE")"
WARM_MED="$(median <"$WARM_FILE")"
INIT_MED="$(median <"$INIT_FILE")"
echo ""
echo "durationMs cold: $(paste -sd, "$COLD_FILE")"
echo "durationMs warm: $(paste -sd, "$WARM_FILE")"
echo "initMs cold:     $(paste -sd, "$INIT_FILE")"
echo "mediana cold=${COLD_MED} ms  warm=${WARM_MED} ms  initMs=${INIT_MED} ms"
echo "primera invocación (con descarga)=${PRIME_MS} ms"
awk -v c="$COLD_MED" -v w="$WARM_MED" 'BEGIN { exit !(c > w) }' \
  || log_fail "la mediana warm (${WARM_MED}) no fue menor que la cold (${COLD_MED})"
log_ok "Mediana cold ${COLD_MED} ms, warm ${WARM_MED} ms, init ${INIT_MED} ms"

log_info "Separación de logs entre dos invokes del mismo container"
A_ID="logs-a-$RANDOM"
B_ID="logs-b-$RANDOM"
A_OUT="$WORKDIR/logs-a.json"
B_OUT="$WORKDIR/logs-b.json"
A_CODE="$(call "$A_ID" "$(jq -n '{event:{mark:"alpha-only"},timeoutSec:20,memoryMb:128,handler:"index.handler",envVars:{ROUND:"logs"}}')" 30 "$A_OUT")"
B_CODE="$(call "$B_ID" "$(jq -n '{event:{mark:"beta-only"},timeoutSec:20,memoryMb:128,handler:"index.handler",envVars:{ROUND:"logs"}}')" 30 "$B_OUT")"
[[ "$A_CODE" == "200" && "$B_CODE" == "200" ]] || log_fail "logs: HTTP $A_CODE / $B_CODE"
[[ "$(jq -r '.coldStart' "$A_OUT")" == "true" ]] || log_fail "logs: el primero no fue cold"
[[ "$(jq -r '.coldStart' "$B_OUT")" == "false" ]] || log_fail "logs: el segundo no fue warm"
stream_has() {
  local id="$1"
  local text="$2"
  docker exec nimbus-redis redis-cli --raw XRANGE "nimbus:logs:${id}" - + | grep -F "$text" >/dev/null
}
stream_has "$A_ID" "alpha-only" || log_fail "el stream de $A_ID no tiene su marca"
stream_has "$B_ID" "beta-only" || log_fail "el stream de $B_ID no tiene su marca"
if stream_has "$A_ID" "beta-only"; then
  log_fail "el stream de $A_ID mezcló la marca de $B_ID"
fi
if stream_has "$B_ID" "alpha-only"; then
  log_fail "el stream de $B_ID mezcló la marca de $A_ID"
fi
log_ok "Cada ejecución tiene su stream y no el del otro"

log_info "Containers warm antes de vencer el idle"
docker ps --filter label=nimbus.warm=1 --format 'table {{.Names}}\t{{.Status}}\t{{.Label "nimbus.functionId"}}'
BEFORE="$(docker ps -q --filter label=nimbus.warm=1 | wc -l | tr -d ' ')"
[[ "$BEFORE" -ge 1 ]] || log_fail "no quedó ningún container warm para esperar el TTL"
log_info "Esperando $((IDLE_SEC + 3))s para el TTL"
sleep $((IDLE_SEC + 3))
log_info "Containers warm después del idle"
docker ps -a --filter label=nimbus.warm=1 --format 'table {{.Names}}\t{{.Status}}'
AFTER="$(docker ps -aq --filter label=nimbus.warm=1 | wc -l | tr -d ' ')"
[[ "$AFTER" == "0" ]] || log_fail "el TTL no borró los containers (quedan $AFTER)"
log_ok "El idle de ${IDLE_SEC}s eliminó los containers"

log_info "El invoke siguiente al TTL vuelve a ser cold"
AFTER_OUT="$WORKDIR/after-ttl.json"
AFTER_CODE="$(call "after-ttl-$RANDOM" '{"event":{"mark":"after-ttl"},"timeoutSec":20,"memoryMb":128,"handler":"index.handler","envVars":{"ROUND":"after"}}' 30 "$AFTER_OUT")"
echo "  HTTP $AFTER_CODE $(cat "$AFTER_OUT")"
[[ "$AFTER_CODE" == "200" ]] || log_fail "post-TTL: HTTP $AFTER_CODE"
jq -e '.coldStart == true and .success == true' "$AFTER_OUT" >/dev/null \
  || log_fail "post-TTL: se esperaba coldStart $(cat "$AFTER_OUT")"
log_ok "Después del TTL la siguiente invocación es cold"

log_info "Timeout sobre el container warm: se descarta y la siguiente es cold"
TO_OUT="$WORKDIR/timeout.json"
TO_CODE="$(call "timeout-$RANDOM" '{"event":{"mode":"sleep","ms":5000},"timeoutSec":2,"memoryMb":128,"handler":"index.handler","envVars":{"ROUND":"after"}}' 20 "$TO_OUT")"
echo "  HTTP $TO_CODE $(cat "$TO_OUT")"
[[ "$TO_CODE" == "504" ]] || log_fail "timeout: se esperaba 504, llegó $TO_CODE"
[[ "$(jq -r '.exitCode' "$TO_OUT")" == "124" ]] || log_fail "timeout: exitCode $(cat "$TO_OUT")"
[[ "$(jq -r '.coldStart' "$TO_OUT")" == "false" ]] || log_fail "timeout: el container warm no se reutilizó $(cat "$TO_OUT")"
DUR="$(jq -r '.durationMs' "$TO_OUT")"
[[ "$DUR" -lt 5000 ]] || log_fail "timeout: durationMs=$DUR no cortó el sleep"
sleep 1
LEFT="$(docker ps -aq --filter label=nimbus.warm=1 | wc -l | tr -d ' ')"
[[ "$LEFT" == "0" ]] || log_fail "el timeout dejó $LEFT containers warm"
NEXT_OUT="$WORKDIR/after-timeout.json"
NEXT_CODE="$(call "after-timeout-$RANDOM" '{"event":{"mark":"after-timeout"},"timeoutSec":20,"memoryMb":128,"handler":"index.handler","envVars":{"ROUND":"after"}}' 30 "$NEXT_OUT")"
echo "  HTTP $NEXT_CODE $(cat "$NEXT_OUT")"
[[ "$NEXT_CODE" == "200" ]] || log_fail "post-timeout: HTTP $NEXT_CODE"
jq -e '.coldStart == true and .output.n == 1' "$NEXT_OUT" >/dev/null \
  || log_fail "post-timeout: se esperaba un container nuevo $(cat "$NEXT_OUT")"
log_ok "Timeout descartó el container; el siguiente invoke es cold (n=1)"

log_info "OOM: el container no se reutiliza"
OOM_OUT="$WORKDIR/oom.json"
OOM_CODE="$(call "oom-$RANDOM" '{"event":{"mode":"mem","mb":512},"timeoutSec":25,"memoryMb":128,"handler":"index.handler","envVars":{"ROUND":"oom"}}' 40 "$OOM_OUT")"
echo "  HTTP $OOM_CODE $(cat "$OOM_OUT")"
OOM_EXIT="$(jq -r '.exitCode' "$OOM_OUT")"
[[ "$OOM_EXIT" == "137" || "$OOM_EXIT" == "134" ]] || log_fail "oom: exitCode=$OOM_EXIT $(cat "$OOM_OUT")"
echo "$(jq -r '.error // empty' "$OOM_OUT")" | grep -qi 'memoria' || log_fail "oom: el error no explica el límite"
sleep 1
echo "  containers warm después del OOM (el de ROUND=after puede seguir; el de este OOM no):"
docker ps -a --filter label=nimbus.warm=1 --format 'table {{.Names}}\t{{.Status}}\t{{.Label "nimbus.functionId"}}' || true
AFTER_OOM="$WORKDIR/after-oom.json"
AFTER_OOM_CODE="$(call "after-oom-$RANDOM" '{"event":{"mark":"after-oom"},"timeoutSec":20,"memoryMb":128,"handler":"index.handler","envVars":{"ROUND":"oom"}}' 30 "$AFTER_OOM")"
echo "  HTTP $AFTER_OOM_CODE $(cat "$AFTER_OOM")"
[[ "$AFTER_OOM_CODE" == "200" ]] || log_fail "post-oom: HTTP $AFTER_OOM_CODE"
jq -e '.coldStart == true and .output.n == 1' "$AFTER_OOM" >/dev/null \
  || log_fail "post-oom: se esperaba un container nuevo $(cat "$AFTER_OOM")"
log_ok "OOM descartó el container; el siguiente invoke es cold (n=1)"

echo ""
log_ok "Warm check listo. Mediana cold ${COLD_MED} ms, warm ${WARM_MED} ms, init ${INIT_MED} ms. Primera (con descarga) ${PRIME_MS} ms."
