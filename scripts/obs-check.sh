#!/usr/bin/env bash
# Genera tráfico real y comprueba Prometheus, Grafana y Jaeger.
#
# Requiere el stack de `make up`. Este script levanta el perfil `obs`
# (Prometheus, Grafana, Jaeger y el collector). No forma parte de CI:
# el job de compose no baja esas imágenes.
#
# Uso: make obs-check

set -euo pipefail

API_URL="${API_URL:-http://localhost:3000}"
COMPUTE_URL="${COMPUTE_URL:-http://localhost:8080}"
PROM_URL="${PROM_URL:-http://localhost:9090}"
GRAFANA_URL="${GRAFANA_URL:-http://localhost:3001}"
JAEGER_URL="${JAEGER_URL:-http://localhost:16686}"
OTLP_URL="${OTLP_URL:-http://localhost:4318}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"
WORK="$(mktemp -d)"
STAMP="$(date +%H%M%S)"

GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m'

log() { echo -e "${YELLOW}>>>${NC} $*"; }
ok() { echo -e "${GREEN}  ✓${NC} $*"; }
fail() { echo -e "${RED}  ✗${NC} $*"; exit 1; }

command -v curl >/dev/null 2>&1 || fail "curl requerido"
command -v jq >/dev/null 2>&1 || fail "jq requerido"
command -v zip >/dev/null 2>&1 || fail "zip requerido"
command -v docker >/dev/null 2>&1 || fail "docker requerido"

if ! curl -sf "$API_URL/health" >/dev/null 2>&1; then
  fail "El stack no está corriendo. Ejecutá: make up"
fi

uuid() { cat /proc/sys/kernel/random/uuid; }

wait_url() {
  local url="$1"
  local label="$2"
  local i
  for i in $(seq 1 90); do
    if curl -sf "$url" >/dev/null 2>&1; then
      ok "$label listo"
      return 0
    fi
    sleep 1
  done
  fail "Timeout esperando $label ($url)"
}

prom() {
  curl -sf --get "$PROM_URL/api/v1/query" --data-urlencode "query=$1"
}

prom_value() {
  local raw
  raw="$(prom "$1" | jq -r '.data.result[0].value[1] // "0"')"
  echo "$raw"
}

ge() {
  awk -v a="$1" -v b="$2" 'BEGIN { exit !(a+0 >= b+0) }'
}

create_fn() {
  local name="$1"
  local resp
  resp="$(curl -sf -X POST "$API_URL/functions" \
    -H 'Content-Type: application/json' \
    -d "{\"name\":\"$name\"}")"
  echo "$resp" | jq -r '.id'
}

deploy_dir() {
  local fn_id="$1"
  local dir="$2"
  local zip="$WORK/$(basename "$dir").zip"
  (cd "$dir" && zip -q -r "$zip" .)
  curl -sf -X POST "$API_URL/functions/$fn_id/deploy" -F "file=@$zip"
}

write_fn() {
  local dir="$1"
  local body="$2"
  mkdir -p "$dir"
  printf '%s\n' "$body" > "$dir/index.js"
}

log "Levantando Prometheus, Grafana, Jaeger y el collector (perfil obs)..."
(cd "$PROJECT_ROOT" && docker compose --profile obs up -d)
wait_url "$PROM_URL/-/ready" "Prometheus"
wait_url "$GRAFANA_URL/api/health" "Grafana"
wait_url "$JAEGER_URL/" "Jaeger"
for i in $(seq 1 60); do
  code="$(curl -s -o /dev/null -w '%{http_code}' -X POST "$OTLP_URL/v1/traces" || true)"
  if [ "$code" != "000" ]; then
    ok "Collector OTLP listo (HTTP $code)"
    break
  fi
  if [ "$i" -eq 60 ]; then
    fail "El collector no abrió $OTLP_URL"
  fi
  sleep 1
done

log "Desplegando funciones de la prueba..."
HELLO_ID="$(create_fn "obs-hello-$STAMP")"
ERR_ID="$(create_fn "obs-err-$STAMP")"
SLOW_ID="$(create_fn "obs-slow-$STAMP")"
OOM_ID="$(create_fn "obs-oom-$STAMP")"
[ -n "$HELLO_ID" ] && [ "$HELLO_ID" != "null" ] || fail "No se pudo crear obs-hello"

HELLO_DEPLOY="$(deploy_dir "$HELLO_ID" "$PROJECT_ROOT/examples/hello")"
HELLO_VER="$(echo "$HELLO_DEPLOY" | jq -r '.version.version')"
write_fn "$WORK/err" 'exports.handler = async () => { throw new Error("obs-error"); };'
ERR_DEPLOY="$(deploy_dir "$ERR_ID" "$WORK/err")"
ERR_VER="$(echo "$ERR_DEPLOY" | jq -r '.version.version')"
SLOW_DEPLOY="$(deploy_dir "$SLOW_ID" "$PROJECT_ROOT/examples/slow")"
SLOW_VER="$(echo "$SLOW_DEPLOY" | jq -r '.version.version')"
write_fn "$WORK/oom" 'exports.handler = async () => { const chunks = []; while (chunks.length < 80) { chunks.push(Buffer.alloc(8 * 1024 * 1024, 1)); } return { n: chunks.length }; };'
OOM_DEPLOY="$(deploy_dir "$OOM_ID" "$WORK/oom")"
OOM_VER="$(echo "$OOM_DEPLOY" | jq -r '.version.version')"
ok "hello=$HELLO_VER err=$ERR_VER slow=$SLOW_VER oom=$OOM_VER"

invoke_sync() {
  local fn_id="$1"
  local event="$2"
  curl -s -X POST "$API_URL/functions/$fn_id/invoke" \
    -H 'Content-Type: application/json' \
    -d "{\"event\":$event}"
}

log "Sync cold y warm..."
FIRST="$(invoke_sync "$HELLO_ID" '{"name":"Obs"}')"
echo "$FIRST" | jq -e '.status == "SUCCESS"' >/dev/null || fail "Primer invoke no fue SUCCESS: $FIRST"
FIRST_INV="$(echo "$FIRST" | jq -r '.invocationId')"
SECOND="$(invoke_sync "$HELLO_ID" '{"name":"Obs"}')"
echo "$SECOND" | jq -e '.status == "SUCCESS"' >/dev/null || fail "Segundo invoke no fue SUCCESS: $SECOND"
ok "dos invokes sync de hello"

log "Async success..."
ASYNC="$(curl -s -X POST "$API_URL/functions/$HELLO_ID/invoke?async=true" \
  -H 'Content-Type: application/json' \
  -d '{"event":{"name":"Async"}}')"
echo "$ASYNC" | jq -e '.status == "PENDING"' >/dev/null || fail "Async no respondió PENDING: $ASYNC"
ASYNC_INV="$(echo "$ASYNC" | jq -r '.invocationId')"
ASYNC_STATUS=""
for i in $(seq 1 60); do
  ASYNC_STATUS="$(curl -sf "$API_URL/invocations/$ASYNC_INV" | jq -r '.status')"
  if [ "$ASYNC_STATUS" = "SUCCESS" ] || [ "$ASYNC_STATUS" = "ERROR" ] || [ "$ASYNC_STATUS" = "TIMEOUT" ]; then
    break
  fi
  sleep 0.5
done
[ "$ASYNC_STATUS" = "SUCCESS" ] || fail "Async terminó en $ASYNC_STATUS"
ok "async SUCCESS"

log "Error de handler (sync)..."
ERR_RESP="$(invoke_sync "$ERR_ID" '{}')"
echo "$ERR_RESP" | jq -e '.status == "ERROR"' >/dev/null || fail "El error sync no quedó en ERROR: $ERR_RESP"
ok "sync ERROR"

log "Timeout directo al compute plane (timeoutSec=1, sleep 4s)..."
TIMEOUT_RESP="$(curl -s -X POST "$COMPUTE_URL/executions" \
  -H 'Content-Type: application/json' \
  -d "$(jq -n --arg id "$(uuid)" --arg fn "$SLOW_ID" --arg ver "$SLOW_VER" \
    '{executionId:$id,functionId:$fn,version:$ver,event:{sleepMs:4000},timeoutSec:1}')")"
echo "$TIMEOUT_RESP" | jq -e '.exitCode == 124' >/dev/null || fail "Timeout no devolvió exit 124: $TIMEOUT_RESP"
ok "timeout exit 124"

log "OOM directo al compute plane (memoryMb=128)..."
OOM_RESP="$(curl -s -X POST "$COMPUTE_URL/executions" \
  -H 'Content-Type: application/json' \
  -d "$(jq -n --arg id "$(uuid)" --arg fn "$OOM_ID" --arg ver "$OOM_VER" \
    '{executionId:$id,functionId:$fn,version:$ver,event:{},memoryMb:128,timeoutSec:15}')")"
OOM_EXIT="$(echo "$OOM_RESP" | jq -r '.exitCode')"
if [ "$OOM_EXIT" != "137" ] && [ "$OOM_EXIT" != "134" ]; then
  fail "OOM no devolvió 137/134: $OOM_RESP"
fi
ok "oom exit $OOM_EXIT"

log "Async que agota reintentos y cae en la DLQ..."
DEAD="$(curl -s -X POST "$API_URL/functions/$ERR_ID/invoke?async=true" \
  -H 'Content-Type: application/json' \
  -d '{"event":{}}')"
DEAD_INV="$(echo "$DEAD" | jq -r '.invocationId')"
DEAD_STATUS=""
DEAD_ATTEMPTS="0"
for i in $(seq 1 80); do
  BODY="$(curl -sf "$API_URL/invocations/$DEAD_INV")"
  DEAD_STATUS="$(echo "$BODY" | jq -r '.status')"
  DEAD_ATTEMPTS="$(echo "$BODY" | jq -r '.attempts // 0')"
  if [ "$DEAD_STATUS" = "ERROR" ] || [ "$DEAD_STATUS" = "TIMEOUT" ]; then
    break
  fi
  sleep 0.5
done
[ "$DEAD_STATUS" = "ERROR" ] || fail "La invocación async no terminó en ERROR ($DEAD_STATUS, attempts=$DEAD_ATTEMPTS)"
ge "$DEAD_ATTEMPTS" 3 || fail "attempts=$DEAD_ATTEMPTS, se esperaban al menos 3"
for i in $(seq 1 20); do
  if curl -sf "$COMPUTE_URL/dlq" | jq -e --arg id "$DEAD_INV" '.entries[] | select(.invocationId == $id)' >/dev/null; then
    break
  fi
  if [ "$i" -eq 20 ]; then
    fail "La invocación $DEAD_INV no apareció en la DLQ"
  fi
  sleep 0.5
done
ok "DLQ con $DEAD_INV (attempts=$DEAD_ATTEMPTS)"

log "Un invoke más de hello para dejar un container warm vivo..."
REFRESH="$(invoke_sync "$HELLO_ID" '{"name":"Pool"}')"
echo "$REFRESH" | jq -e '.status == "SUCCESS"' >/dev/null || fail "El invoke de refresco no fue SUCCESS: $REFRESH"
ok "pool warm refrescado"

log "Esperando el export de trazas y un scrape de Prometheus..."
sleep 8

LOGS="$(curl -sf "$API_URL/invocations/$FIRST_INV/logs?follow=false" || true)"
TRACE_ID="$(echo "$LOGS" | grep -oE 'traceId=[0-9a-f]{32}' | head -1 | cut -d= -f2 || true)"
[ -n "$TRACE_ID" ] || fail "Los logs de $FIRST_INV no tienen traceId: $LOGS"
ok "traceId en logs: $TRACE_ID"

TRACE_JSON=""
for i in $(seq 1 20); do
  TRACE_JSON="$(curl -sf "$JAEGER_URL/api/traces/$TRACE_ID" || true)"
  if echo "$TRACE_JSON" | jq -e '.data[0].spans | length > 0' >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
echo "$TRACE_JSON" | jq -e '.data[0].spans | length > 0' >/dev/null || fail "Jaeger no tiene la traza $TRACE_ID"

echo "$TRACE_JSON" | jq -r '
  .data[0] as $t
  | $t.spans
  | sort_by(.startTime)
  | .[]
  | "\($t.processes[.processID].serviceName)\t\(.operationName)"
' | tee "$WORK/spans.txt"

need_span() {
  local name="$1"
  if ! grep -q "$name" "$WORK/spans.txt"; then
    fail "La traza no tiene el span $name"
  fi
}
need_span "nimbus.invoke"
need_span "nimbus.compute.call"
need_span "nimbus.executions"
need_span "queue.wait"
need_span "artifact.download"
need_span "container.acquire"
need_span "container.run"
if ! grep -q "nimbus-control-plane" "$WORK/spans.txt"; then
  fail "La traza no incluye al control plane"
fi
if ! grep -q "nimbus-compute-plane" "$WORK/spans.txt"; then
  fail "La traza no incluye al compute plane"
fi
ok "traza completa Nest → Go → container.run"

log "Consultando Prometheus..."
print_query() {
  local q="$1"
  echo "  $q"
  prom "$q" | jq -r '.data.result[]? | "    \(.metric | tojson) \(.value[1])"' || true
}

SUCCESS="$(prom_value 'sum(nimbus_invocations_total{status="success"})')"
ERRORS="$(prom_value 'sum(nimbus_errors_total)')"
TIMEOUTS="$(prom_value 'sum(nimbus_timeouts_total)')"
OOMS="$(prom_value 'sum(nimbus_ooms_total)')"
COLD="$(prom_value 'sum(nimbus_cold_starts_total)')"
WARM="$(prom_value 'sum(nimbus_warm_starts_total)')"
RETRIES="$(prom_value 'sum(nimbus_async_retries_total)')"
DLQ="$(prom_value 'nimbus_dlq_entries')"
WARM_POOL="$(prom_value 'sum(nimbus_warm_pool_size)')"
HTTP_REQ="$(prom_value 'sum(nimbus_http_requests_total)')"
DURATION="$(prom_value 'sum(nimbus_invocation_duration_seconds_count)')"

ge "$SUCCESS" 3 || fail "success=$SUCCESS (se esperaban al menos 3: 2 sync + 1 async)"
ge "$ERRORS" 1 || fail "errors=$ERRORS"
ge "$TIMEOUTS" 1 || fail "timeouts=$TIMEOUTS"
ge "$OOMS" 1 || fail "ooms=$OOMS"
ge "$COLD" 1 || fail "cold=$COLD"
ge "$WARM" 1 || fail "warm=$WARM"
ge "$RETRIES" 2 || fail "retries=$RETRIES"
ge "$DLQ" 1 || fail "dlq=$DLQ"
ge "$WARM_POOL" 1 || fail "warm_pool=$WARM_POOL"
ge "$HTTP_REQ" 1 || fail "http del control plane=$HTTP_REQ"
ge "$DURATION" 1 || fail "duration samples=$DURATION"
ok "métricas de invocación, cold/warm, timeout, oom, retries y DLQ"

log "Ráfaga para un 429 (cola llena, sin reiniciar el proceso)..."
mkdir -p "$WORK/burst"
for i in $(seq 1 40); do
  curl -s -o "$WORK/burst/$i.body" -w '%{http_code}' --max-time 3 \
    -X POST "$API_URL/functions/$SLOW_ID/invoke" \
    -H 'Content-Type: application/json' \
    -d '{"event":{"sleepMs":5000}}' > "$WORK/burst/$i.code" &
done
for i in $(seq 1 15); do
  if grep -qx '429' "$WORK/burst"/*.code 2>/dev/null; then
    break
  fi
  sleep 1
done
wait || true
REJECTS="$(grep -l '429' "$WORK/burst"/*.code 2>/dev/null | wc -l | tr -d ' ')"
ge "$REJECTS" 1 || fail "Ningún invoke devolvió 429 (códigos: $(cat "$WORK/burst"/*.code | sort | uniq -c | tr '\n' ' '))"
ok "$REJECTS respuestas 429"

sleep 6
REJECTIONS="$(prom_value 'sum(nimbus_rejections_total)')"
ge "$REJECTIONS" 1 || fail "nimbus_rejections_total=$REJECTIONS"
ok "nimbus_rejections_total=$REJECTIONS"

log "Dashboard de Grafana..."
DASH="$(curl -sf "$GRAFANA_URL/api/dashboards/uid/nimbus" || true)"
echo "$DASH" | jq -e '.dashboard.uid == "nimbus"' >/dev/null || fail "Grafana no tiene el dashboard nimbus: $DASH"
TITLES="$(echo "$DASH" | jq -r '.dashboard.panels[].title' | paste -sd ', ' -)"
echo "$TITLES" | grep -q "Invocaciones" || fail "El dashboard no trae el panel de invocaciones: $TITLES"
echo "$TITLES" | grep -q "Cold vs warm" || fail "El dashboard no trae cold vs warm"
echo "$TITLES" | grep -q "DLQ" || fail "El dashboard no trae DLQ"
ok "dashboard provisionado ($TITLES)"

echo ""
log "Valores de Prometheus"
print_query 'nimbus_invocations_total'
print_query 'nimbus_errors_total'
print_query 'nimbus_timeouts_total'
print_query 'nimbus_ooms_total'
print_query 'nimbus_cold_starts_total'
print_query 'nimbus_warm_starts_total'
print_query 'nimbus_rejections_total'
print_query 'nimbus_async_retries_total'
print_query 'nimbus_dlq_entries'
print_query 'nimbus_queue_depth'
print_query 'nimbus_warm_pool_size'
print_query 'nimbus_containers_in_use'
print_query 'nimbus_active_executions'
print_query 'sum(nimbus_http_requests_total)'

echo ""
ok "obs-check listo"
echo "  Grafana  $GRAFANA_URL/d/nimbus/nimbus-functions"
echo "  Jaeger   $JAEGER_URL/trace/$TRACE_ID"
echo "  Prom     $PROM_URL"
echo "$TRACE_ID" > "$WORK/trace-id"
cp "$WORK/spans.txt" /tmp/nimbus-obs-spans.txt 2>/dev/null || true
echo "$TRACE_ID" > /tmp/nimbus-obs-trace-id
