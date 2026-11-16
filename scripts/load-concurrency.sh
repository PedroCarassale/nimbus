#!/usr/bin/env bash
# Prueba real de concurrencia del compute plane.
#
# Con NIMBUS_FN_CONCURRENCY=2 y NIMBUS_FN_QUEUE_MAX=3 dispara 6 invokes
# de un handler que duerme ~2s. El resultado esperado:
#   - 2 corren enseguida (queuedMs chico, startedAt juntos)
#   - 3 esperan en la cola y arrancan cuando se libera un slot
#   - 1 recibe HTTP 429 Too Many Requests (cola llena)
#
# Al terminar restaura los límites por defecto del compose.
# Requiere el stack arriba (make up).

set -euo pipefail

API_URL="${API_URL:-http://localhost:3000}"
COMPUTE_URL="${COMPUTE_URL:-http://localhost:8080}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"
COMPOSE=(docker compose)
PARALLEL="${PARALLEL:-6}"
CONCURRENCY="${CONCURRENCY:-2}"
QUEUE_MAX="${QUEUE_MAX:-3}"
SLEEP_MS="${SLEEP_MS:-2000}"

GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m'

log() { echo -e "${YELLOW}>>>${NC} $*"; }
ok() { echo -e "${GREEN}  ✓${NC} $*"; }
fail() { echo -e "${RED}  ✗${NC} $*"; exit 1; }

wait_url() {
  local url="$1"
  local label="$2"
  local i
  for i in $(seq 1 180); do
    if curl -sf "$url" >/dev/null 2>&1; then
      ok "$label listo"
      return 0
    fi
    sleep 1
  done
  echo -e "${RED}  ✗${NC} Timeout esperando $label ($url)"
  return 1
}

restore_limits() {
  log "Restaurando límites por defecto (compose)..."
  (cd "$PROJECT_ROOT" && "${COMPOSE[@]}" up -d --no-deps --force-recreate --no-build compute-plane) || true
  (cd "$PROJECT_ROOT" && "${COMPOSE[@]}" up -d --no-deps --force-recreate --no-build control-plane) || true
  wait_url "$COMPUTE_URL/health" "Compute plane" || true
  wait_url "$API_URL/health" "Control plane" || true
}

command -v curl >/dev/null 2>&1 || fail "curl requerido"
command -v jq >/dev/null 2>&1 || fail "jq requerido"
command -v zip >/dev/null 2>&1 || fail "zip requerido"
command -v docker >/dev/null 2>&1 || fail "docker requerido"

if ! curl -sf "$API_URL/health" >/dev/null 2>&1; then
  fail "El stack no está corriendo. Ejecutá: make up"
fi

trap restore_limits EXIT

log "Aplicando concurrencia=${CONCURRENCY} cola=${QUEUE_MAX} para la prueba..."
(
  cd "$PROJECT_ROOT"
  NIMBUS_FN_CONCURRENCY="$CONCURRENCY" \
  NIMBUS_FN_QUEUE_MAX="$QUEUE_MAX" \
  NIMBUS_MAX_CONTAINERS="${NIMBUS_MAX_CONTAINERS_LOAD:-8}" \
  NIMBUS_FN_QUEUE_WAIT_SEC="${NIMBUS_FN_QUEUE_WAIT_SEC_LOAD:-45}" \
    "${COMPOSE[@]}" up -d --no-deps --force-recreate --no-build compute-plane
)
wait_url "$COMPUTE_URL/health" "Compute plane" || fail "Compute plane no volvió después de aplicar los límites"
(
  cd "$PROJECT_ROOT"
  NIMBUS_TIMEOUT_SEC="${NIMBUS_TIMEOUT_SEC_LOAD:-20}" \
    "${COMPOSE[@]}" up -d --no-deps --force-recreate --no-build control-plane
)
wait_url "$API_URL/health" "Control plane" || fail "Control plane no volvió después de aplicar el timeout"

log "Creando función 'slow'..."
CREATE_RESPONSE=$(curl -s -X POST "$API_URL/functions" \
  -H "Content-Type: application/json" \
  -d '{"name":"slow"}') || true
FN_ID=$(echo "$CREATE_RESPONSE" | jq -r '.id // empty')
if [ -z "$FN_ID" ] || [ "$FN_ID" = "null" ]; then
  FN_ID=$(curl -sf "$API_URL/functions" | jq -r '.[] | select(.name=="slow") | .id' | head -n 1)
fi
if [ -z "$FN_ID" ] || [ "$FN_ID" = "null" ]; then
  fail "No se pudo crear ni encontrar la función slow: $CREATE_RESPONSE"
fi
ok "Función $FN_ID"

log "Desplegando examples/slow (sleep ${SLEEP_MS}ms)..."
mkdir -p "$PROJECT_ROOT/dist"
(
  cd "$PROJECT_ROOT/examples/slow"
  zip -q -r "$PROJECT_ROOT/dist/slow.zip" .
)
DEPLOY_RESPONSE=$(curl -sf -X POST "$API_URL/functions/$FN_ID/deploy" \
  -F "file=@$PROJECT_ROOT/dist/slow.zip")
VERSION=$(echo "$DEPLOY_RESPONSE" | jq -r '.version.version')
if [ -z "$VERSION" ] || [ "$VERSION" = "null" ]; then
  fail "Deploy falló: $DEPLOY_RESPONSE"
fi
ok "Versión $VERSION"

log "Calentando el runtime con un invoke corto..."
WARM_CODE=$(curl -s -o /tmp/nimbus-load-warm.json -w "%{http_code}" --max-time 60 \
  -X POST "$API_URL/functions/$FN_ID/invoke" \
  -H "Content-Type: application/json" \
  -d '{"event":{"sleepMs":200}}')
if [ "$WARM_CODE" != "200" ]; then
  echo "Respuesta warmup:"
  cat /tmp/nimbus-load-warm.json || true
  fail "Warmup devolvió HTTP $WARM_CODE"
fi
ok "Warmup OK"

tmpdir=$(mktemp -d)
since=$(date -u +%Y-%m-%dT%H:%M:%SZ)
log "Disparando ${PARALLEL} invokes en paralelo (sleep ${SLEEP_MS}ms)..."
burst_start=$(date -u +%Y-%m-%dT%H:%M:%S.%3NZ)

for i in $(seq 1 "$PARALLEL"); do
  (
    sent_ms=$(date +%s%3N)
    sent_iso=$(date -u +%Y-%m-%dT%H:%M:%S.%3NZ)
    body="$tmpdir/$i.body"
    code=$(curl -s -o "$body" -w "%{http_code}" --max-time 90 \
      -X POST "$API_URL/functions/$FN_ID/invoke" \
      -H "Content-Type: application/json" \
      -d "{\"event\":{\"sleepMs\":${SLEEP_MS}}}") || code="000"
    recv_ms=$(date +%s%3N)
    recv_iso=$(date -u +%Y-%m-%dT%H:%M:%S.%3NZ)
    if ! jq -e . "$body" >/dev/null 2>&1; then
      echo '{}' > "$body"
    fi
    jq -nc \
      --argjson i "$i" \
      --argjson http "$code" \
      --arg sent "$sent_iso" \
      --arg recv "$recv_iso" \
      --argjson sentMs "$sent_ms" \
      --argjson recvMs "$recv_ms" \
      --slurpfile body "$body" \
      '{i:$i, http:$http, sent:$sent, recv:$recv, sentMs:$sentMs, recvMs:$recvMs, wallMs:($recvMs-$sentMs), body:$body[0]}' \
      > "$tmpdir/$i.json"
  ) &
done
wait
burst_end=$(date -u +%Y-%m-%dT%H:%M:%S.%3NZ)

echo ""
echo "Ráfaga: $burst_start → $burst_end"
echo ""
printf '%-4s %-6s %-8s %-10s %-28s %-28s %-28s %s\n' \
  "#" "HTTP" "wall_ms" "queued_ms" "enviado" "recibido" "handler_started" "resultado"
echo "-------------------------------------------------------------------------------------------------------------------------------"

jq -s -r '
  sort_by(.i)[] |
  [
    .i,
    .http,
    .wallMs,
    (.body.queuedMs // .body.message // "-"),
    .sent,
    .recv,
    (.body.output.startedAt // "-"),
    (if .http == 429 then (.body.error + ": " + (.body.message // ""))
     else (.body.status // "?") end)
  ] | @tsv
' "$tmpdir"/*.json | while IFS=$'\t' read -r i http wall queued sent recv started result; do
  printf '%-4s %-6s %-8s %-10s %-28s %-28s %-28s %s\n' \
    "$i" "$http" "$wall" "$queued" "$sent" "$recv" "$started" "$result"
done

echo ""
echo "Detalle (startedAt / finishedAt del handler, queuedMs del compute plane):"
jq -s -r '
  sort_by(.i)[] |
  "  #\(.i) http=\(.http) wall=\(.wallMs)ms queuedMs=\(.body.queuedMs // "n/a") started=\(.body.output.startedAt // "-") finished=\(.body.output.finishedAt // "-") status=\(.body.status // .body.error // "-") msg=\(.body.message // "-")"
' "$tmpdir"/*.json

echo ""
log "Líneas de admisión del compute plane:"
(cd "$PROJECT_ROOT" && "${COMPOSE[@]}" logs --since "$since" --no-color compute-plane 2>/dev/null | grep -E 'concurrency|Cola llena|espera en cola' || true)

echo ""
if ! jq -e -s --argjson sleep "$SLEEP_MS" '
  def oks: map(select(.http == 200 and .body.status == "SUCCESS"));
  def queued: oks | map(.body.queuedMs) | sort;
  (map(select(.http == 429)) | length) == 1
  and (map(select(.http == 429 and ((.body.message // "") | test("Cola llena")))) | length) == 1
  and (map(select(.http == 429)) | .[0].wallMs) < 1500
  and (oks | length) == 5
  and (queued | .[0] < 800 and .[1] < 800)
  and (queued | .[2] >= ($sleep - 500) and .[3] >= ($sleep - 500))
  and (queued | .[4] >= (.[2] + ($sleep - 500)))
' "$tmpdir"/*.json >/dev/null; then
  echo -e "${RED}La ráfaga no mostró 2 en paralelo, 3 en espera y 1 rechazo 429.${NC}"
  echo "JSON crudo:"
  jq -s . "$tmpdir"/*.json
  exit 1
fi

ok "2 corrieron sin esperar, 3 esperaron su turno y 1 recibió 429 Too Many Requests"
