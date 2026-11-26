#!/usr/bin/env bash
# Prueba el scheduler con dos workers de compute (make up-multi).
#
# 1. GET /workers lista 2 workers healthy y /health es el scheduler.
# 2. Un invoke de Nest (create → deploy → invoke) termina bien: el control
#    plane no cambió de contrato, solo apunta a http://scheduler:8080.
# 3. Invokes en paralelo de otra función se reparten entre los dos workers
#    (header X-Nimbus-Worker).
# 4. Afinidad warm: el segundo invoke de la misma función y versión cae en
#    el worker que publicó la clave idle y responde coldStart=false.
# 5. Un burst async lo consumen los dos workers desde la misma cola SQS.
# 6. Cancelar por el scheduler corta la ejecución en el worker que la corre.
# 7. SIGKILL de un worker: pasado el TTL, el scheduler lo saca y los invokes
#    nuevos van al que queda, sin errores.
#
# Al terminar, el worker 2 queda detenido. make down-multi limpia el stack.

set -euo pipefail

API_URL="${API_URL:-http://localhost:3000}"
COMPUTE_URL="${COMPUTE_URL:-http://localhost:8080}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORKDIR="${TMPDIR:-/tmp}/nimbus-cluster-check"
REDIS_CONTAINER="${REDIS_CONTAINER:-nimbus-redis}"
WORKER1="${WORKER1_CONTAINER:-nimbus-compute-worker-1}"
WORKER2="${WORKER2_CONTAINER:-nimbus-compute-worker-2}"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

log_info() { echo -e "${YELLOW}[cluster-check]${NC} $1"; }
log_ok() { echo -e "${GREEN}[✓]${NC} $1"; }
log_fail() { echo -e "${RED}[✗]${NC} $1" >&2; exit 1; }

command -v curl >/dev/null || log_fail "curl requerido"
command -v jq >/dev/null || log_fail "jq requerido"
command -v zip >/dev/null || log_fail "zip requerido"
command -v docker >/dev/null || log_fail "docker requerido"

rm -rf "$WORKDIR"
mkdir -p "$WORKDIR"

health="$(curl -sf "$COMPUTE_URL/health" || true)"
svc="$(echo "$health" | jq -r '.service // empty' 2>/dev/null || true)"
[[ "$svc" == "scheduler" ]] || log_fail "GET /health no es el scheduler ($health). Ejecutá: make up-multi"

workers_json="$(curl -sf "$COMPUTE_URL/workers")"
healthy="$(echo "$workers_json" | jq '[.workers[] | select(.state=="healthy")] | length')"
[[ "$healthy" == "2" ]] || log_fail "se esperaban 2 workers healthy, hay $healthy: $workers_json"
echo "$workers_json" | jq -c '.workers[] | {workerId,state,activeExecutions,maxContainers,url}'
log_ok "GET /workers lista 2 workers healthy"

deploy_fn() {
  local name="$1" src="$2"
  local create id deploy ver
  create="$(curl -sf -X POST "$API_URL/functions" -H 'Content-Type: application/json' -d "{\"name\":\"$name\"}")"
  id="$(echo "$create" | jq -r '.id')"
  [[ -n "$id" && "$id" != "null" ]] || log_fail "no se pudo crear $name: $create"
  (cd "$src" && zip -qr "$WORKDIR/${name}.zip" .) || log_fail "no se pudo empaquetar $name"
  if ! deploy="$(curl -sf -X POST "$API_URL/functions/$id/deploy" -F "file=@$WORKDIR/${name}.zip")"; then
    log_fail "deploy de $name falló (HTTP)"
  fi
  ver="$(echo "$deploy" | jq -r '.version.version')"
  [[ -n "$ver" && "$ver" != "null" ]] || log_fail "deploy de $name falló: $deploy"
  echo "$id $ver"
}

stamp="$(date +%s)"
log_info "Desplegando hello y slow"
read -r HELLO_ID HELLO_VER < <(deploy_fn "cluster-hello-$stamp" "$ROOT/examples/hello")
read -r SLOW_ID SLOW_VER < <(deploy_fn "cluster-slow-$stamp" "$ROOT/examples/slow")
[[ -n "${HELLO_ID:-}" && -n "${HELLO_VER:-}" && -n "${SLOW_ID:-}" && -n "${SLOW_VER:-}" ]] \
  || log_fail "deploy incompleto hello=$HELLO_ID@$HELLO_VER slow=$SLOW_ID@$SLOW_VER"
log_ok "hello=$HELLO_ID@$HELLO_VER slow=$SLOW_ID@$SLOW_VER"

exec_payload() {
  local id="$1" fn="$2" ver="$3" extra="$4"
  jq -n --arg id "$id" --arg fn "$fn" --arg ver "$ver" --argjson extra "$extra" \
    '{executionId:$id,functionId:$fn,version:$ver,timeoutSec:30,memoryMb:128,handler:"index.handler"} + $extra'
}

post_exec() {
  local id="$1" fn="$2" ver="$3" extra="$4" out="$5" hdr="$6"
  local payload code
  payload="$(exec_payload "$id" "$fn" "$ver" "$extra")"
  code="$(curl -sS -D "$hdr" -o "$out" -w '%{http_code}' -X POST "$COMPUTE_URL/executions" \
    -H 'Content-Type: application/json' --max-time 90 -d "$payload")"
  echo "$code"
}

worker_of() {
  awk 'BEGIN{IGNORECASE=1} /^X-Nimbus-Worker:/ {print $2}' "$1" | tr -d '\r'
}

log_info "8 invokes en paralelo de slow (reparto por carga, sin clave warm todavía)"
for i in $(seq 1 8); do
  (
    id="$(cat /proc/sys/kernel/random/uuid)"
    extra="$(jq -n --argjson n "$i" '{event:{sleepMs:1200,i:$n}}')"
    code="$(post_exec "$id" "$SLOW_ID" "$SLOW_VER" "$extra" "$WORKDIR/par-$i.json" "$WORKDIR/par-$i.hdr")"
    echo "$code" >"$WORKDIR/par-$i.code"
  ) &
done
wait || true

declare -A dist=()
fail_n=0
for i in $(seq 1 8); do
  code="$(cat "$WORKDIR/par-$i.code" 2>/dev/null || echo missing)"
  wid="$(worker_of "$WORKDIR/par-$i.hdr" || true)"
  echo "  #$i HTTP $code worker=${wid:-?} $(jq -c '{success,coldStart,durationMs,error}' "$WORKDIR/par-$i.json" 2>/dev/null || echo '{}')"
  if [[ "$code" != "200" ]]; then
    fail_n=$((fail_n + 1))
    continue
  fi
  dist["$wid"]=$(( ${dist["$wid"]:-0} + 1 ))
done
[[ "$fail_n" == "0" ]] || log_fail "$fail_n invokes paralelos no respondieron 200"
[[ -n "${dist[compute-worker-1]:-}" && -n "${dist[compute-worker-2]:-}" ]] \
  || log_fail "los invokes no se repartieron entre los dos workers: $(declare -p dist)"
summary=""
for k in "${!dist[@]}"; do
  summary+="$k=${dist[$k]} "
done
log_ok "reparto: $summary"

log_info "Afinidad warm de hello (misma función, versión y config)"
prime_id="$(cat /proc/sys/kernel/random/uuid)"
prime_code="$(post_exec "$prime_id" "$HELLO_ID" "$HELLO_VER" '{"event":{"nombre":"prime"}}' "$WORKDIR/prime.json" "$WORKDIR/prime.hdr")"
prime_worker="$(worker_of "$WORKDIR/prime.hdr")"
prime_cold="$(jq -r '.coldStart' "$WORKDIR/prime.json")"
echo "  prime HTTP $prime_code worker=$prime_worker coldStart=$prime_cold $(jq -c '{durationMs,initMs}' "$WORKDIR/prime.json")"
[[ "$prime_code" == "200" && "$prime_cold" == "true" ]] || log_fail "el primer invoke directo no fue cold: $(cat "$WORKDIR/prime.json")"

warm_worker=""
for _ in $(seq 1 30); do
  listed="$(curl -sf "$COMPUTE_URL/workers")"
  warm_worker="$(echo "$listed" | jq -r --arg fn "$HELLO_ID" '[.workers[] | select(.state=="healthy" and ([.idleWarmKeys[] | select(startswith($fn + ":"))] | length) > 0)][0].workerId // empty')"
  if [[ -n "$warm_worker" ]]; then
    break
  fi
  sleep 0.5
done
[[ -n "$warm_worker" ]] || log_fail "ningún worker publicó la clave warm de hello: $(curl -s "$COMPUTE_URL/workers")"
[[ "$warm_worker" == "$prime_worker" ]] || log_fail "la clave warm está en $warm_worker pero el invoke corrió en $prime_worker"
log_ok "clave idle publicada por $warm_worker"

for i in $(seq 1 3); do
  id="$(cat /proc/sys/kernel/random/uuid)"
  code="$(post_exec "$id" "$HELLO_ID" "$HELLO_VER" '{"event":{"nombre":"warm"}}' "$WORKDIR/warm-$i.json" "$WORKDIR/warm-$i.hdr")"
  wid="$(worker_of "$WORKDIR/warm-$i.hdr")"
  cold="$(jq -r '.coldStart' "$WORKDIR/warm-$i.json")"
  echo "  warm #$i HTTP $code worker=$wid coldStart=$cold"
  [[ "$code" == "200" && "$cold" == "false" && "$wid" == "$warm_worker" ]] \
    || log_fail "invoke $i no reutilizó el sandbox de $warm_worker: $(cat "$WORKDIR/warm-$i.json")"
done
log_ok "3 invokes siguientes: coldStart=false en $warm_worker"

log_info "Invoke sync por Nest (el control plane habla con el scheduler)"
nest="$(curl -sf -X POST "$API_URL/functions/$HELLO_ID/invoke" -H 'Content-Type: application/json' -d '{"event":{"nombre":"cluster"}}')"
echo "$nest" | jq '{status,durationMs,error}'
[[ "$(echo "$nest" | jq -r '.status')" == "SUCCESS" ]] || log_fail "Nest no llegó a SUCCESS: $nest"
log_ok "Nest → scheduler → worker respondió SUCCESS"

log_info "Burst async: 10 jobs, los dos workers leen la misma cola SQS"
declare -a async_ids=()
for i in $(seq 1 10); do
  resp="$(curl -sf -X POST "$API_URL/functions/$SLOW_ID/invoke?async=true" \
    -H 'Content-Type: application/json' \
    -d '{"event":{"sleepMs":700}}')"
  iid="$(echo "$resp" | jq -r '.invocationId')"
  [[ -n "$iid" && "$iid" != "null" ]] || log_fail "async no devolvió id: $resp"
  async_ids+=("$iid")
done
deadline=$((SECONDS + 90))
pending=10
while (( SECONDS < deadline )); do
  pending=0
  for iid in "${async_ids[@]}"; do
    st="$(curl -sf "$API_URL/invocations/$iid" | jq -r '.status')"
    case "$st" in
      SUCCESS) ;;
      ERROR|TIMEOUT) log_fail "async $iid terminó en $st" ;;
      *) pending=$((pending + 1)) ;;
    esac
  done
  if (( pending == 0 )); then
    break
  fi
  sleep 1
done
(( pending == 0 )) || log_fail "quedaron $pending jobs async sin SUCCESS"

async_n() {
  docker exec "$1" wget -qO- http://127.0.0.1:8080/metrics 2>/dev/null \
    | awk '/^nimbus_invocations_total\{/ && /mode="async"/ {s+=$NF} END {printf "%d", s+0}'
}
n1="$(async_n "$WORKER1")"
n2="$(async_n "$WORKER2")"
echo "  async invocations worker-1=$n1 worker-2=$n2"
[[ "$n1" -gt 0 && "$n2" -gt 0 ]] || log_fail "un worker no procesó jobs async (1=$n1 2=$n2)"
log_ok "los dos workers procesaron jobs de SQS ($n1 y $n2)"

log_info "Cancel de una ejecución lenta a través del scheduler"
cancel_id="$(cat /proc/sys/kernel/random/uuid)"
cancel_extra="$(jq -n '{event:{sleepMs:8000}}')"
post_exec "$cancel_id" "$SLOW_ID" "$SLOW_VER" "$cancel_extra" "$WORKDIR/cancel.json" "$WORKDIR/cancel.hdr" >"$WORKDIR/cancel.code" &
cancel_pid=$!
owner=""
for _ in $(seq 1 50); do
  owner="$(docker exec "$REDIS_CONTAINER" redis-cli HGET "nimbus:exec:$cancel_id" workerId 2>/dev/null | tr -d '\r' || true)"
  if [[ -n "$owner" && "$owner" != "(nil)" ]]; then
    break
  fi
  sleep 0.1
done
[[ -n "$owner" && "$owner" != "(nil)" ]] || log_fail "nimbus:exec:$cancel_id no tiene workerId"
echo "  workerId en Redis: $owner"
cancel_resp="$(curl -sS -X POST "$COMPUTE_URL/executions/$cancel_id/cancel" --max-time 10)"
echo "  cancel $cancel_resp"
echo "$cancel_resp" | jq -e '.cancelled == true' >/dev/null || log_fail "el scheduler no confirmó el cancel"
wait "$cancel_pid" || true
cancel_code="$(tr -d '[:space:]' <"$WORKDIR/cancel.code")"
cancel_dur="$(jq -r '.durationMs // 0' "$WORKDIR/cancel.json")"
cancel_err="$(jq -r '.error // empty' "$WORKDIR/cancel.json")"
echo "  ejecución HTTP $cancel_code durationMs=$cancel_dur error=$cancel_err"
[[ "$cancel_code" == "409" && "$cancel_err" == "Ejecución cancelada" ]] || log_fail "la ejecución no volvió 409: $(cat "$WORKDIR/cancel.json")"
[[ "$cancel_dur" -lt 8000 ]] || log_fail "el sleep de 8s no se cortó (durationMs=$cancel_dur)"
routes="$(curl -sf "$COMPUTE_URL/metrics")"
echo "$routes" | grep -E "nimbus_scheduler_routes_total\\{reason=\"cancel\",worker=\"$owner\"\\}|nimbus_scheduler_routes_total\\{worker=\"$owner\",reason=\"cancel\"\\}" >/dev/null \
  || log_fail "no está la métrica de cancel hacia $owner"
log_ok "cancel cortó la ejecución en $owner (durationMs=$cancel_dur)"

log_info "SIGKILL de $WORKER2 y espera del TTL"
docker kill --signal=KILL "$WORKER2" >/dev/null
left=""
for _ in $(seq 1 20); do
  listed="$(curl -sf "$COMPUTE_URL/workers" || echo '{"workers":[]}')"
  left="$(echo "$listed" | jq -r '[.workers[] | select(.state=="healthy") | .workerId] | join(",")')"
  count="$(echo "$listed" | jq '[.workers[] | select(.state=="healthy")] | length')"
  if [[ "$count" == "1" && "$left" != *compute-worker-2* ]]; then
    break
  fi
  sleep 1
done
[[ "$left" == "compute-worker-1" ]] || log_fail "después del TTL quedaron: $left ($(curl -s "$COMPUTE_URL/workers"))"
log_ok "el scheduler ya no lista a compute-worker-2; queda $left"

fo_fail=0
for i in $(seq 1 4); do
  id="$(cat /proc/sys/kernel/random/uuid)"
  code="$(post_exec "$id" "$HELLO_ID" "$HELLO_VER" '{"event":{"nombre":"failover"}}' "$WORKDIR/fo-$i.json" "$WORKDIR/fo-$i.hdr")"
  wid="$(worker_of "$WORKDIR/fo-$i.hdr")"
  ok="$(jq -r '.success' "$WORKDIR/fo-$i.json")"
  echo "  failover #$i HTTP $code worker=$wid success=$ok"
  if [[ "$code" != "200" || "$ok" != "true" || "$wid" != "compute-worker-1" ]]; then
    fo_fail=$((fo_fail + 1))
  fi
done
[[ "$fo_fail" == "0" ]] || log_fail "failover con errores o enrutado al worker caído"
log_ok "4 invokes después del TTL fueron al worker vivo, sin errores"

echo
log_ok "cluster-check completo"
echo "  reparto paralelo: $summary"
echo "  warm: $warm_worker coldStart=false"
echo "  async: worker-1=$n1 worker-2=$n2"
echo "  cancel: $owner durationMs=$cancel_dur"
echo "  failover: compute-worker-2 killed, invokes en compute-worker-1"
echo "El worker 2 sigue detenido. Para bajar el stack: make down-multi"
