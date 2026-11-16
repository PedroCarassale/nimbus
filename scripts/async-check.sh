#!/usr/bin/env bash
# Invocación async: 202, resultado en Postgres, reintentos con backoff,
# DLQ, redrive y reclaim después de matar el compute plane.
#
# Requiere el stack arriba (make up) con esta versión del control plane.
# Al terminar restaura el compute plane del compose.

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

log_info() { echo -e "${YELLOW}[async-check]${NC} $1"; }
log_ok() { echo -e "${GREEN}[✓]${NC} $1"; }
log_fail() { echo -e "${RED}[✗]${NC} $1" >&2; exit 1; }

command -v curl >/dev/null 2>&1 || log_fail "curl requerido"
command -v jq >/dev/null 2>&1 || log_fail "jq requerido"
command -v zip >/dev/null 2>&1 || log_fail "zip requerido"
command -v docker >/dev/null 2>&1 || log_fail "docker requerido"

wait_url() {
  local url="$1"
  local name="$2"
  local i
  for i in $(seq 1 90); do
    if curl -sf "$url" >/dev/null 2>&1; then
      log_ok "$name listo"
      return 0
    fi
    sleep 1
  done
  log_fail "$name no responde en $url"
}

restore_compute() {
  log_info "Restaurando el compute plane (compose)..."
  (
    cd "$ROOT"
    env -u NIMBUS_ASYNC_ENABLED \
      -u NIMBUS_ASYNC_MAX_ATTEMPTS \
      -u NIMBUS_ASYNC_BACKOFF_BASE_MS \
      -u NIMBUS_ASYNC_BACKOFF_MAX_MS \
      -u NIMBUS_ASYNC_BACKOFF_JITTER_MS \
      -u NIMBUS_ASYNC_CLAIM_IDLE_MS \
      -u NIMBUS_ASYNC_LOCK_MS \
      -u NIMBUS_ASYNC_WORKERS \
      docker compose up -d --no-deps --force-recreate --no-build compute-plane
  ) || true
  wait_url "$COMPUTE_URL/health" "Compute plane" || true
}

trap restore_compute EXIT

if ! curl -sf "$API_URL/health" >/dev/null 2>&1; then
  log_fail "El stack no está corriendo. Ejecutá: make up"
fi

WORKDIR="$(mktemp -d)"
# shellcheck disable=SC2064
trap "rm -rf '$WORKDIR'; restore_compute" EXIT

recreate_compute() {
  log_info "Compute plane con backoff corto y claim de ${NIMBUS_ASYNC_CLAIM_IDLE_MS:-1500}ms"
  (
    cd "$ROOT"
    NIMBUS_ASYNC_ENABLED=1 \
    NIMBUS_ASYNC_MAX_ATTEMPTS=3 \
    NIMBUS_ASYNC_BACKOFF_BASE_MS=400 \
    NIMBUS_ASYNC_BACKOFF_MAX_MS=5000 \
    NIMBUS_ASYNC_BACKOFF_JITTER_MS=80 \
    NIMBUS_ASYNC_CLAIM_IDLE_MS=1500 \
    NIMBUS_ASYNC_LOCK_MS=1000 \
    NIMBUS_ASYNC_WORKERS=2 \
    NIMBUS_WARM=1 \
    NIMBUS_SLOT_LEASE_SEC=8 \
      docker compose up -d --no-deps --force-recreate --no-build compute-plane
  )
  wait_url "$COMPUTE_URL/health" "Compute plane"
  wait_url "$API_URL/health" "Control plane"
}

deploy_fn() {
  local name="$1"
  local src="$2"
  local create
  create="$(curl -sf -X POST "$API_URL/functions" \
    -H 'Content-Type: application/json' \
    -d "{\"name\":\"$name\"}")"
  local id
  id="$(echo "$create" | jq -r '.id')"
  if [[ -z "$id" || "$id" == "null" ]]; then
    log_fail "No se pudo crear $name: $create"
  fi
  (cd "$src" && zip -qr "$WORKDIR/$name.zip" .)
  local deploy
  deploy="$(curl -sf -X POST "$API_URL/functions/$id/deploy" -F "file=@$WORKDIR/$name.zip")"
  local version
  version="$(echo "$deploy" | jq -r '.version.version')"
  if [[ -z "$version" || "$version" == "null" ]]; then
    log_fail "No se pudo desplegar $name: $deploy"
  fi
  printf '%s' "$id"
}

poll_until() {
  local id="$1"
  local want="$2"
  local timeout="$3"
  local start now body status
  start="$(date +%s)"
  while true; do
    body="$(curl -sf "$API_URL/invocations/$id")" || log_fail "GET /invocations/$id falló"
    status="$(echo "$body" | jq -r '.status')"
    if [[ "$status" == "$want" ]]; then
      printf '%s' "$body"
      return 0
    fi
    now="$(date +%s)"
    if (( now - start >= timeout )); then
      log_fail "Timeout esperando $want en $id. Último: $body"
    fi
    sleep 0.2
  done
}

recreate_compute

STAMP="$(date +%s)"
mkdir -p "$WORKDIR/hello" "$WORKDIR/boom" "$WORKDIR/slow"
cat >"$WORKDIR/hello/index.js" <<'EOF'
exports.handler = async (event) => {
  return { message: "async-" + (event.name || "ok") };
};
EOF
cat >"$WORKDIR/boom/index.js" <<'EOF'
exports.handler = async () => {
  throw new Error("boom-async");
};
EOF
# 3s is inside the default 5s timeout, and long enough to SIGKILL after RUNNING.
cat >"$WORKDIR/slow/index.js" <<'EOF'
exports.handler = async () => {
  await new Promise((resolve) => setTimeout(resolve, 3000));
  return { slept: true };
};
EOF

log_info "Deploy hello / boom / slow"
HELLO_ID="$(deploy_fn "async-hello-$STAMP" "$WORKDIR/hello")"
BOOM_ID="$(deploy_fn "async-boom-$STAMP" "$WORKDIR/boom")"
SLOW_ID="$(deploy_fn "async-slow-$STAMP" "$WORKDIR/slow")"
log_ok "Funciones $HELLO_ID $BOOM_ID $SLOW_ID"

log_info "Config inválida no entra a la cola ni a la DLQ"
BAD_CODE="$(curl -sS -o "$WORKDIR/bad.json" -w '%{http_code}' -X POST "$COMPUTE_URL/async/jobs" \
  -H 'Content-Type: application/json' \
  -d '{"invocationId":"inv-bad","executionId":"exec-bad","functionId":"fn","version":"v","timeoutSec":9999}')"
[[ "$BAD_CODE" == "400" ]] || log_fail "Se esperaba 400, llegó $BAD_CODE: $(cat "$WORKDIR/bad.json")"
if jq -e '.entries[] | select(.id=="inv-bad")' <<<"$(curl -sf "$COMPUTE_URL/dlq")" >/dev/null; then
  log_fail "Un 400 de validación quedó en la DLQ"
fi
log_ok "400 de timeoutSec y la DLQ no tiene inv-bad"

log_info "Invoke async (query) debe responder 202 rápido"
META="$(curl -sS -o "$WORKDIR/accepted.json" -w '%{http_code} %{time_total}' \
  -X POST "$API_URL/functions/$HELLO_ID/invoke?async=true" \
  -H 'Content-Type: application/json' \
  -d '{"event":{"name":"Nimbus"}}')"
CODE="$(awk '{print $1}' <<<"$META")"
ELAPSED="$(awk '{print $2}' <<<"$META")"
echo "202 en ${ELAPSED}s: $(cat "$WORKDIR/accepted.json")"
[[ "$CODE" == "202" ]] || log_fail "Se esperaba 202, llegó $CODE"
awk -v t="$ELAPSED" 'BEGIN{ if (t >= 1) exit 1 }' || log_fail "El 202 tardó ${ELAPSED}s (límite 1s)"
INV="$(jq -r '.invocationId' "$WORKDIR/accepted.json")"
EXEC="$(jq -r '.executionId' "$WORKDIR/accepted.json")"
[[ -n "$INV" && "$INV" != "null" && -n "$EXEC" && "$EXEC" != "null" ]] || log_fail "Falta id en el 202"
log_ok "202 en ${ELAPSED}s invocation=$INV"

log_info "Esperando SUCCESS en GET /invocations/$INV"
DONE="$(poll_until "$INV" SUCCESS 40)"
echo "$DONE" | jq '{status,attempts,durationMs,output,error,executionId}'
ATTEMPTS="$(jq -r '.attempts' <<<"$DONE")"
MESSAGE="$(jq -r '.output.message' <<<"$DONE")"
[[ "$ATTEMPTS" == "1" ]] || log_fail "attempts=$ATTEMPTS, se esperaba 1"
[[ "$MESSAGE" == "async-Nimbus" ]] || log_fail "output inesperado: $MESSAGE"
log_ok "SUCCESS attempts=1 output=$MESSAGE durationMs=$(jq -r '.durationMs' <<<"$DONE")"

log_info "Header X-Invocation-Type: Event"
META="$(curl -sS -o "$WORKDIR/header.json" -w '%{http_code} %{time_total}' \
  -X POST "$API_URL/functions/$HELLO_ID/invoke" \
  -H 'Content-Type: application/json' \
  -H 'X-Invocation-Type: Event' \
  -d '{"event":{"name":"Header"}}')"
CODE="$(awk '{print $1}' <<<"$META")"
ELAPSED="$(awk '{print $2}' <<<"$META")"
[[ "$CODE" == "202" ]] || log_fail "Header Event: se esperaba 202, llegó $CODE $(cat "$WORKDIR/header.json")"
HEADER_INV="$(jq -r '.invocationId' "$WORKDIR/header.json")"
HEADER_DONE="$(poll_until "$HEADER_INV" SUCCESS 40)"
[[ "$(jq -r '.output.message' <<<"$HEADER_DONE")" == "async-Header" ]] || log_fail "Header no ejecutó: $HEADER_DONE"
log_ok "Header Event 202 en ${ELAPSED}s y SUCCESS"

log_info "El invoke síncrono sigue respondiendo 200"
META="$(curl -sS -o "$WORKDIR/sync.json" -w '%{http_code} %{time_total}' \
  -X POST "$API_URL/functions/$HELLO_ID/invoke" \
  -H 'Content-Type: application/json' \
  -d '{"event":{"name":"Sync"}}')"
CODE="$(awk '{print $1}' <<<"$META")"
echo "sync $META $(cat "$WORKDIR/sync.json")"
[[ "$CODE" == "200" ]] || log_fail "Sync devolvió $CODE"
[[ "$(jq -r '.status' "$WORKDIR/sync.json")" == "SUCCESS" ]] || log_fail "Sync no fue SUCCESS"
[[ "$(jq -r '.output.message' "$WORKDIR/sync.json")" == "async-Sync" ]] || log_fail "Sync output distinto"
SYNC_INV="$(jq -r '.invocationId' "$WORKDIR/sync.json")"
SYNC_GET="$(curl -sf "$API_URL/invocations/$SYNC_INV")"
[[ "$(jq -r '.attempts' <<<"$SYNC_GET")" == "1" ]] || log_fail "Sync no guardó attempts=1: $SYNC_GET"
[[ "$(jq -r '.status' <<<"$SYNC_GET")" == "SUCCESS" ]] || log_fail "GET sync no es SUCCESS"
log_ok "Sync 200 en $(awk '{print $2}' <<<"$META")s y attempts=1"

log_info "Handler que siempre falla: 3 intentos y DLQ"
META="$(curl -sS -o "$WORKDIR/boom-accepted.json" -w '%{http_code} %{time_total}' \
  -X POST "$API_URL/functions/$BOOM_ID/invoke?async=1" \
  -H 'Content-Type: application/json' \
  -d '{"event":{}}')"
[[ "$(awk '{print $1}' <<<"$META")" == "202" ]] || log_fail "Boom no aceptó: $(cat "$WORKDIR/boom-accepted.json")"
BOOM_INV="$(jq -r '.invocationId' "$WORKDIR/boom-accepted.json")"
BOOM_START="$(date +%s)"
BOOM_DONE="$(poll_until "$BOOM_INV" ERROR 40)"
BOOM_END="$(date +%s)"
echo "$BOOM_DONE" | jq '{status,attempts,error,durationMs}'
[[ "$(jq -r '.attempts' <<<"$BOOM_DONE")" == "3" ]] || log_fail "Se esperaban 3 intentos: $BOOM_DONE"
echo "$BOOM_DONE" | jq -e '.error | test("boom-async")' >/dev/null || log_fail "El error no es boom-async: $BOOM_DONE"
log_ok "ERROR en $((BOOM_END - BOOM_START))s con 3 intentos"

DLQ="$(curl -sf "$COMPUTE_URL/dlq")"
ENTRY="$(jq -c --arg id "$BOOM_INV" '.entries[] | select(.id==$id)' <<<"$DLQ")"
[[ -n "$ENTRY" ]] || log_fail "No está en la DLQ: $DLQ"
echo "$ENTRY" | jq '{id,attempts,lastError,exitCode,enqueuedAt,attemptTimes,failedAt,deadAt}'
TIMES="$(jq -c '.attemptTimes' <<<"$ENTRY")"
python3 - <<PY
import json
times = json.loads('''$TIMES''')
if len(times) != 3:
    raise SystemExit(f"se esperaban 3 timestamps, llegó {times}")
gap1 = times[1] - times[0]
gap2 = times[2] - times[1]
print(f"backoff timestamps ms={times} gap1={gap1} gap2={gap2}")
if gap1 < 300:
    raise SystemExit(f"gap1 {gap1} ms no refleja el backoff base")
if gap2 < 600:
    raise SystemExit(f"gap2 {gap2} ms no refleja el segundo backoff")
PY
log_ok "DLQ con payload, error y timestamps de backoff"

log_info "Redrive"
REDRIVE="$(curl -sS -w '\n%{http_code}' -X POST "$COMPUTE_URL/dlq/$BOOM_INV/redrive")"
REDRIVE_CODE="$(tail -n1 <<<"$REDRIVE")"
REDRIVE_BODY="$(sed '$d' <<<"$REDRIVE")"
echo "$REDRIVE_BODY"
[[ "$REDRIVE_CODE" == "200" ]] || log_fail "Redrive HTTP $REDRIVE_CODE"
sleep 0.3
if jq -e --arg id "$BOOM_INV" '.entries[] | select(.id==$id)' <<<"$(curl -sf "$COMPUTE_URL/dlq")" >/dev/null; then
  log_fail "Sigue en la DLQ justo después del redrive"
fi
SEEN=""
REDRIVE_START="$(date +%s)"
FINAL=""
while true; do
  BODY="$(curl -sf "$API_URL/invocations/$BOOM_INV")"
  ST="$(jq -r '.status' <<<"$BODY")"
  case ",$SEEN," in
    *",$ST,"*) ;;
    *) SEEN="${SEEN:+$SEEN,}$ST" ;;
  esac
  if [[ "$ST" == "ERROR" ]] && jq -e --arg id "$BOOM_INV" '.entries[] | select(.id==$id)' <<<"$(curl -sf "$COMPUTE_URL/dlq")" >/dev/null; then
    FINAL="$BODY"
    break
  fi
  NOW="$(date +%s)"
  if (( NOW - REDRIVE_START >= 40 )); then
    log_fail "Redrive no volvió a la DLQ. Visto: $SEEN último: $BODY"
  fi
  sleep 0.2
done
echo "estados tras redrive: $SEEN"
echo "$FINAL" | jq '{status,attempts,error}'
case ",$SEEN," in
  *",PENDING,"*|*",RUNNING,"*) ;;
  *) log_fail "No se vio PENDING ni RUNNING tras el redrive ($SEEN)" ;;
esac
[[ "$(jq -r '.attempts' <<<"$FINAL")" == "3" ]] || log_fail "Tras redrive attempts=$(jq -r '.attempts' <<<"$FINAL")"
log_ok "Redrive salió de la DLQ ($SEEN) y volvió después de 3 intentos"

log_info "Matar el compute plane a mitad de un sleep y reclamar el mensaje"
META="$(curl -sS -o "$WORKDIR/slow-accepted.json" -w '%{http_code} %{time_total}' \
  -X POST "$API_URL/functions/$SLOW_ID/invoke?async=true" \
  -H 'Content-Type: application/json' \
  -d '{"event":{}}')"
[[ "$(awk '{print $1}' <<<"$META")" == "202" ]] || log_fail "Slow no aceptó"
SLOW_INV="$(jq -r '.invocationId' "$WORKDIR/slow-accepted.json")"
log_info "Esperando RUNNING de $SLOW_INV"
RUNNING_BODY="$(poll_until "$SLOW_INV" RUNNING 30)"
echo "$RUNNING_BODY" | jq '{status,attempts,executionId}'
KILL_AT="$(date +%s%3N)"
log_info "SIGKILL al compute plane en $KILL_AT"
docker kill --signal KILL nimbus-compute-plane >/dev/null
ORPHANS="$(docker ps -aq --filter label=nimbus.warm=1 || true)"
if [[ -n "$ORPHANS" ]]; then
  # shellcheck disable=SC2086
  docker rm -f $ORPHANS >/dev/null || true
fi
RESTART_START="$(date +%s%3N)"
recreate_compute
RESTART_END="$(date +%s%3N)"
log_info "Compute plane otra vez arriba en $((RESTART_END - RESTART_START))ms"
SLOW_DONE="$(poll_until "$SLOW_INV" SUCCESS 50)"
echo "$SLOW_DONE" | jq '{status,attempts,output,error,durationMs,executionId}'
SLOW_ATTEMPTS="$(jq -r '.attempts' <<<"$SLOW_DONE")"
[[ "$(jq -r '.output.slept' <<<"$SLOW_DONE")" == "true" ]] || log_fail "El sleep no completó: $SLOW_DONE"
if (( SLOW_ATTEMPTS < 2 )); then
  log_fail "Se esperaba el intento reclamado (attempts>=2), llegó $SLOW_ATTEMPTS"
fi
log_ok "Reclaim: SUCCESS attempts=$SLOW_ATTEMPTS tras reiniciar el compute plane"

log_ok "async-check completo"
