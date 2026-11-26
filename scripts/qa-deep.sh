#!/usr/bin/env bash
# QA profundo de los pasos 0–5, encima de los checks de make.
# Requiere el stack arriba (make up). Al terminar deja el compute plane
# y el control plane con los defaults del compose.
set -uo pipefail

export API_URL="${API_URL:-http://localhost:3000}"
export COMPUTE_URL="${COMPUTE_URL:-http://localhost:8080}"
export CONSOLE_URL="${CONSOLE_URL:-http://localhost:8088}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
OUT="${QA_OUT:-/tmp/nimbus-qa}"
mkdir -p "$OUT/shots"
STAMP="$(date +%Y%m%d-%H%M%S)"
LOG="$OUT/qa-$STAMP.log"
PASS=0
FAIL=0

exec > >(tee -a "$LOG") 2>&1

ok() { echo "  PASS $*"; PASS=$((PASS+1)); }
bad() { echo "  FAIL $*"; FAIL=$((FAIL+1)); }

wait_url() {
  local url="$1" name="$2" i
  for i in $(seq 1 90); do
    if curl -sf "$url" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "timeout esperando $name ($url)"
  return 1
}

recreate_compute() {
  (
    cd "$ROOT"
    docker compose up -d --no-deps --force-recreate --no-build compute-plane
  )
  wait_url "$COMPUTE_URL/health" "compute"
}

restore_planes() {
  echo ">>> restaurando compute y control plane"
  (
    cd "$ROOT"
    env -u NIMBUS_WARM -u NIMBUS_TRACING -u NIMBUS_FN_CONCURRENCY -u NIMBUS_FN_QUEUE_MAX \
      -u NIMBUS_MAX_CONTAINERS -u NIMBUS_ASYNC_BACKOFF_BASE_MS -u NIMBUS_ASYNC_MAX_ATTEMPTS \
      docker compose up -d --no-deps --force-recreate --no-build compute-plane control-plane
  ) || true
  wait_url "$COMPUTE_URL/health" "compute" || true
  wait_url "$API_URL/health" "control" || true
}
trap restore_planes EXIT

if ! curl -sf "$API_URL/health" >/dev/null; then
  echo "el stack no está arriba"
  exit 1
fi

echo "=== consola healthy ==="
# El healthcheck viejo pegaba a localhost (::1). Recreamos para tomar el
# chequeo de 127.0.0.1 si el compose de esta rama ya está aplicado.
docker compose up -d --no-deps --force-recreate --no-build console >/dev/null
sleep 12
HEALTH="$(docker inspect nimbus-console --format '{{.State.Health.Status}}' 2>/dev/null || echo missing)"
echo "  console health=$HEALTH"
if [[ "$HEALTH" == "healthy" ]]; then ok "consola healthy"; else bad "consola health=$HEALTH"; fi

echo "=== flujo curl create/deploy/invoke/logs ==="
HELLO_ZIP="$OUT/hello.zip"
rm -f "$HELLO_ZIP"
(cd examples/hello && zip -q "$HELLO_ZIP" index.js)
FN_NAME="qa-curl-$STAMP"
CREATE="$(curl -sf -X POST "$API_URL/functions" -H 'Content-Type: application/json' -d "{\"name\":\"$FN_NAME\"}")"
FN="$(echo "$CREATE" | jq -r .id)"
DEPLOY="$(curl -sf -X POST "$API_URL/functions/$FN/deploy" -F "file=@$HELLO_ZIP")"
VER="$(echo "$DEPLOY" | jq -r .version.version)"
INV="$(curl -sf -X POST "$API_URL/functions/$FN/invoke" -H 'Content-Type: application/json' -d '{"event":{"name":"Curl"}}')"
echo "  $INV"
MSG="$(echo "$INV" | jq -r '.output.body.message // empty')"
IID="$(echo "$INV" | jq -r .invocationId)"
if [[ "$MSG" == "¡Hola, Curl!" ]]; then ok "invoke curl ($MSG)"; else bad "invoke curl: $INV"; fi
LOGS="$(curl -sf "$API_URL/invocations/$IID/logs?follow=false")"
if echo "$LOGS" | grep -q 'event: end'; then ok "SSE follow=false"; else bad "SSE sin end"; fi

echo "=== consola headless ==="
PUP_DIR="$OUT/node_modules"
if [[ ! -d "$PUP_DIR/puppeteer-core" ]]; then
  mkdir -p "$OUT"
  npm install --prefix "$OUT" puppeteer-core@24 --silent
fi
NODE_PATH="$PUP_DIR" node scripts/qa-console.mjs "$HELLO_ZIP" "$OUT/shots" "qa-ui-$STAMP"
if [[ $? -eq 0 ]]; then ok "consola crear/deploy/invoke/logs"; else bad "consola headless"; fi

echo "=== env y versión no comparten container ==="
ISO="$OUT/iso"
rm -rf "$ISO"
mkdir -p "$ISO/v1" "$ISO/v2"
cat >"$ISO/v1/index.js" <<'EOF'
let n = 0;
exports.handler = async () => {
  n += 1;
  return { n, pid: process.pid, greeting: process.env.GREETING || "" };
};
EOF
cat >"$ISO/v2/index.js" <<'EOF'
let n = 0;
exports.handler = async () => ({ n: n += 1, pid: process.pid, mark: "v2", greeting: process.env.GREETING || "" });
EOF
(cd "$ISO/v1" && zip -q "$ISO/v1.zip" index.js)
(cd "$ISO/v2" && zip -q "$ISO/v2.zip" index.js)
ISO_FN="$(curl -sf -X POST "$API_URL/functions" -H 'Content-Type: application/json' -d "{\"name\":\"qa-iso-$STAMP\"}" | jq -r .id)"
V1="$(curl -sf -X POST "$API_URL/functions/$ISO_FN/deploy" -F "file=@$ISO/v1.zip" | jq -r .version.version)"
call() {
  curl -sf -X POST "$COMPUTE_URL/executions" -H 'Content-Type: application/json' -d "$1"
}
A1="$(call "$(jq -n --arg id "a1-$RANDOM" --arg fn "$ISO_FN" --arg ver "$V1" '{executionId:$id,functionId:$fn,version:$ver,event:{},envVars:{GREETING:"alpha"}}')")"
A2="$(call "$(jq -n --arg id "a2-$RANDOM" --arg fn "$ISO_FN" --arg ver "$V1" '{executionId:$id,functionId:$fn,version:$ver,event:{},envVars:{GREETING:"alpha"}}')")"
echo "  alpha1 $(echo "$A1" | jq -c '{coldStart,output}')"
echo "  alpha2 $(echo "$A2" | jq -c '{coldStart,output}')"
# v2 se despliega con un solo warm ocupado. El tope por función es 2: si beta
# ya ocupó el segundo slot, v2 echa al warm de v1 y el chequeo de n=3 miente.
V2="$(curl -sf -X POST "$API_URL/functions/$ISO_FN/deploy" -F "file=@$ISO/v2.zip" | jq -r .version.version)"
V2R="$(call "$(jq -n --arg id "v2-$RANDOM" --arg fn "$ISO_FN" --arg ver "$V2" '{executionId:$id,functionId:$fn,version:$ver,event:{},envVars:{GREETING:"alpha"}}')")"
V1R="$(call "$(jq -n --arg id "v1b-$RANDOM" --arg fn "$ISO_FN" --arg ver "$V1" '{executionId:$id,functionId:$fn,version:$ver,event:{},envVars:{GREETING:"alpha"}}')")"
echo "  v2 $(echo "$V2R" | jq -c '{coldStart,output}')"
echo "  v1 again $(echo "$V1R" | jq -c '{coldStart,output}')"
if [[ "$(echo "$V2R" | jq -r .output.mark)" == "v2" && "$(echo "$V2R" | jq -r .output.n)" == "1" && "$(echo "$V2R" | jq -r .coldStart)" == "true" && "$(echo "$V1R" | jq -r .output.n)" == "3" && "$(echo "$V1R" | jq -r .coldStart)" == "false" ]]; then
  ok "v2 arranca cold (no corre el warm de v1) y v1 sigue en n=3"
else
  bad "versión v1=$V1R v2=$V2R"
fi
B1="$(call "$(jq -n --arg id "b1-$RANDOM" --arg fn "$ISO_FN" --arg ver "$V1" '{executionId:$id,functionId:$fn,version:$ver,event:{},envVars:{GREETING:"beta"}}')")"
echo "  beta1 $(echo "$B1" | jq -c '{coldStart,output}')"
AN="$(echo "$A2" | jq -r .output.n)"; AG="$(echo "$A2" | jq -r .output.greeting)"; AC="$(echo "$A2" | jq -r .coldStart)"
BN="$(echo "$B1" | jq -r .output.n)"; BG="$(echo "$B1" | jq -r .output.greeting)"
# Node es PID 1 en cada container. El aislamiento se ve en el contador y el cold start.
if [[ "$AG" == "alpha" && "$AC" == "false" && "$AN" == "2" && "$BG" == "beta" && "$BN" == "1" && "$(echo "$B1" | jq -r .coldStart)" == "true" ]]; then
  ok "env distinto no comparte container (alpha n=$AN cold=false, beta n=$BN cold=true)"
else
  bad "aislamiento de env alpha=$A2 beta=$B1"
fi

echo "=== async encolado vs 429 ==="
export NIMBUS_FN_CONCURRENCY=1 NIMBUS_FN_QUEUE_MAX=0 NIMBUS_ASYNC_MAX_ATTEMPTS=3 NIMBUS_ASYNC_BACKOFF_BASE_MS=200
recreate_compute || bad "no recreó compute para queue 0"
HOLD="$OUT/hold"
mkdir -p "$HOLD"
cat >"$HOLD/index.js" <<'EOF'
exports.handler = async (event) => {
  const ms = Number(event.sleepMs) || 0;
  if (ms) await new Promise((r) => setTimeout(r, ms));
  return { slept: ms, pid: process.pid };
};
EOF
(cd "$HOLD" && zip -q "$HOLD/fn.zip" index.js)
HOLD_FN="$(curl -sf -X POST "$API_URL/functions" -H 'Content-Type: application/json' -d "{\"name\":\"qa-hold-$STAMP\"}" | jq -r .id)"
HOLD_VER="$(curl -sf -X POST "$API_URL/functions/$HOLD_FN/deploy" -F "file=@$HOLD/fn.zip" | jq -r .version.version)"
curl -sS --max-time 25 -X POST "$COMPUTE_URL/executions" -H 'Content-Type: application/json' \
  -d "{\"executionId\":\"hold-$RANDOM\",\"functionId\":\"$HOLD_FN\",\"version\":\"$HOLD_VER\",\"timeoutSec\":20,\"event\":{\"sleepMs\":8000}}" \
  >"$OUT/hold.json" &
HOLD_PID=$!
sleep 0.8
ASYNC429="$(curl -sf -X POST "$API_URL/functions/$HOLD_FN/invoke?async=true" -H 'Content-Type: application/json' -d '{"event":{"sleepMs":10}}')"
echo "  202 con cola 0: $ASYNC429"
A429_ID="$(echo "$ASYNC429" | jq -r .invocationId)"
A429_CODE="$(echo "$ASYNC429" | jq -r .status)"
if [[ "$A429_CODE" == "PENDING" ]]; then ok "async responde 202 aunque la cola de concurrencia esté llena"; else bad "async no dio 202: $ASYNC429"; fi
FINAL429=""
for _ in $(seq 1 30); do
  FINAL429="$(curl -sf "$API_URL/invocations/$A429_ID")"
  ST="$(echo "$FINAL429" | jq -r .status)"
  if [[ "$ST" == "ERROR" || "$ST" == "SUCCESS" || "$ST" == "TIMEOUT" ]]; then break; fi
  sleep 0.5
done
echo "  resultado async con cola 0: $(echo "$FINAL429" | jq -c '{status,attempts,error}')"
DLQ_HIT="$(curl -sf "$COMPUTE_URL/dlq" | jq -r --arg id "$A429_ID" '[.entries[] | select(.invocationId==$id)] | length')"
if [[ "$(echo "$FINAL429" | jq -r .status)" == "ERROR" && "$(echo "$FINAL429" | jq -r .attempts)" == "3" && "$DLQ_HIT" == "1" ]]; then
  ok "async + cola llena (queue max 0) gasta 3 intentos y cae en la DLQ"
else
  bad "async cola llena no fue a DLQ: $FINAL429 dlq=$DLQ_HIT"
fi
wait "$HOLD_PID" || true

export NIMBUS_FN_QUEUE_MAX=4
recreate_compute || bad "no recreó compute para cola 4"
curl -sS --max-time 25 -X POST "$COMPUTE_URL/executions" -H 'Content-Type: application/json' \
  -d "{\"executionId\":\"hold2-$RANDOM\",\"functionId\":\"$HOLD_FN\",\"version\":\"$HOLD_VER\",\"timeoutSec\":20,\"event\":{\"sleepMs\":3000}}" \
  >"$OUT/hold2.json" &
HOLD2_PID=$!
sleep 0.6
ASYNCQ="$(curl -sf -X POST "$API_URL/functions/$HOLD_FN/invoke?async=true" -H 'Content-Type: application/json' -d '{"event":{"sleepMs":10}}')"
AQ_ID="$(echo "$ASYNCQ" | jq -r .invocationId)"
FINALQ=""
for _ in $(seq 1 40); do
  FINALQ="$(curl -sf "$API_URL/invocations/$AQ_ID")"
  ST="$(echo "$FINALQ" | jq -r .status)"
  if [[ "$ST" == "SUCCESS" || "$ST" == "ERROR" || "$ST" == "TIMEOUT" ]]; then break; fi
  sleep 0.4
done
echo "  resultado async encolado: $(echo "$FINALQ" | jq -c '{status,attempts,output}')"
if [[ "$(echo "$FINALQ" | jq -r .status)" == "SUCCESS" && "$(echo "$FINALQ" | jq -r .attempts)" == "1" ]]; then
  ok "async espera en la cola FIFO y termina SUCCESS sin ir a la DLQ"
else
  bad "async encolado: $FINALQ"
fi
wait "$HOLD2_PID" || true

echo "=== cancelar en cola, en curso y async pendiente ==="
export NIMBUS_FN_CONCURRENCY=1 NIMBUS_FN_QUEUE_MAX=2
recreate_compute
QID="q-$RANDOM"
curl -sS --max-time 25 -X POST "$COMPUTE_URL/executions" -H 'Content-Type: application/json' \
  -d "{\"executionId\":\"run-$RANDOM\",\"functionId\":\"$HOLD_FN\",\"version\":\"$HOLD_VER\",\"timeoutSec\":20,\"event\":{\"sleepMs\":5000}}" \
  >"$OUT/run.json" &
RUN_PID=$!
sleep 0.5
curl -sS --max-time 25 -X POST "$COMPUTE_URL/executions" -H 'Content-Type: application/json' \
  -d "{\"executionId\":\"$QID\",\"functionId\":\"$HOLD_FN\",\"version\":\"$HOLD_VER\",\"timeoutSec\":20,\"event\":{\"sleepMs\":200}}" \
  >"$OUT/queued.json" &
Q_PID=$!
sleep 0.4
CANCELQ="$(curl -sS -X POST "$COMPUTE_URL/executions/$QID/cancel")"
echo "  cancel queued: $CANCELQ"
wait "$RUN_PID" "$Q_PID" || true
if echo "$CANCELQ" | jq -e '.cancelled == true' >/dev/null && grep -q 'cancelada mientras esperaba' "$OUT/queued.json"; then
  ok "cancel de una ejecución en la cola FIFO responde 409 al que esperaba"
else
  bad "cancel queued: $CANCELQ body=$(cat "$OUT/queued.json")"
fi

RUNID="runlive-$RANDOM"
curl -sS --max-time 25 -X POST "$COMPUTE_URL/executions" -H 'Content-Type: application/json' \
  -d "{\"executionId\":\"$RUNID\",\"functionId\":\"$HOLD_FN\",\"version\":\"$HOLD_VER\",\"timeoutSec\":15,\"event\":{\"sleepMs\":2500}}" \
  >"$OUT/running.json" &
LIVE_PID=$!
sleep 0.4
CANCELR="$(curl -sS -w ' HTTP:%{http_code}' -X POST "$COMPUTE_URL/executions/$RUNID/cancel")"
wait "$LIVE_PID" || true
echo "  cancel running: $CANCELR result=$(cat "$OUT/running.json")"
RUN_DUR="$(jq -r '.durationMs // 99999' "$OUT/running.json")"
RUN_LEFT="$(docker ps -aq --filter "name=nimbus-${RUNID}" | wc -l | tr -d ' ')"
if echo "$CANCELR" | grep -q '"cancelled":true' \
  && jq -e '.success == false and .exitCode == 130 and (.error | test("Ejecución cancelada"))' "$OUT/running.json" >/dev/null \
  && [[ "$RUN_DUR" -lt 1800 && "$RUN_LEFT" == "0" ]]; then
  ok "cancel de una ejecución warm que ya corre mata el container (durationMs=$RUN_DUR, sin container)"
else
  bad "cancel running: $CANCELR dur=$RUN_DUR containers=$RUN_LEFT $(cat "$OUT/running.json")"
fi
AFTER="$(curl -sS --max-time 20 -X POST "$COMPUTE_URL/executions" -H 'Content-Type: application/json' \
  -d "{\"executionId\":\"after-$RANDOM\",\"functionId\":\"$HOLD_FN\",\"version\":\"$HOLD_VER\",\"timeoutSec\":10,\"event\":{\"sleepMs\":10}}")"
echo "  siguiente tras cancel warm: $(echo "$AFTER" | jq -c '{success,coldStart,durationMs}')"
if echo "$AFTER" | jq -e '.success == true and .coldStart == true' >/dev/null; then
  ok "el container warm cancelado no se reutiliza (el siguiente es cold)"
else
  bad "warm cancelado se reutilizó: $AFTER"
fi

ASYNC_RUN="$(curl -sf -X POST "$API_URL/functions/$HOLD_FN/invoke?async=true" -H 'Content-Type: application/json' -d '{"event":{"sleepMs":2500}}')"
AR_ID="$(echo "$ASYNC_RUN" | jq -r .invocationId)"
AR_EID="$(echo "$ASYNC_RUN" | jq -r .executionId)"
AR_ST=""
for _ in $(seq 1 40); do
  AR_ST="$(curl -sf "$API_URL/invocations/$AR_ID" | jq -r .status)"
  [[ "$AR_ST" == "RUNNING" ]] && break
  sleep 0.1
done
if [[ "$AR_ST" != "RUNNING" ]]; then
  bad "el async no llegó a RUNNING antes del cancel (status=$AR_ST)"
fi
sleep 0.3
CANCELA="$(curl -sS -X POST "$COMPUTE_URL/executions/$AR_EID/cancel")"
AR_FINAL=""
for _ in $(seq 1 40); do
  AR_FINAL="$(curl -sf "$API_URL/invocations/$AR_ID")"
  AR_ST="$(echo "$AR_FINAL" | jq -r .status)"
  if [[ "$AR_ST" == "ERROR" || "$AR_ST" == "SUCCESS" || "$AR_ST" == "TIMEOUT" ]]; then break; fi
  sleep 0.2
done
AR_DLQ="$(curl -sf "$COMPUTE_URL/dlq" | jq -r --arg id "$AR_ID" '[.entries[] | select(.invocationId==$id)] | length')"
echo "  cancel async en curso: $CANCELA fila=$(echo "$AR_FINAL" | jq -c '{status,attempts,error,durationMs}') dlq=$AR_DLQ"
AR_DUR="$(echo "$AR_FINAL" | jq -r '.durationMs // 99999')"
if echo "$CANCELA" | grep -q '"cancelled":true' \
  && [[ "$(echo "$AR_FINAL" | jq -r .status)" == "ERROR" && "$(echo "$AR_FINAL" | jq -r .attempts)" == "1" && "$AR_DLQ" == "0" ]] \
  && [[ "$(echo "$AR_FINAL" | jq -r .error)" == "Ejecución cancelada" ]] \
  && [[ "$AR_DUR" -lt 1800 ]]; then
  ok "cancel de un async que ya corre termina en ERROR, sin reintento ni DLQ (durationMs=$AR_DUR)"
else
  bad "cancel async en curso: $CANCELA $AR_FINAL dlq=$AR_DLQ"
fi

export NIMBUS_WARM=0
recreate_compute
COLDID="coldlive-$RANDOM"
curl -sS --max-time 25 -X POST "$COMPUTE_URL/executions" -H 'Content-Type: application/json' \
  -d "{\"executionId\":\"$COLDID\",\"functionId\":\"$HOLD_FN\",\"version\":\"$HOLD_VER\",\"timeoutSec\":15,\"event\":{\"sleepMs\":2500}}" \
  >"$OUT/cold-running.json" &
COLD_PID=$!
sleep 0.5
CANCELC="$(curl -sS -w ' HTTP:%{http_code}' -X POST "$COMPUTE_URL/executions/$COLDID/cancel")"
wait "$COLD_PID" || true
COLD_DUR="$(jq -r '.durationMs // 99999' "$OUT/cold-running.json")"
COLD_LEFT="$(docker ps -aq --filter "name=nimbus-${COLDID}" | wc -l | tr -d ' ')"
echo "  cancel cold: $CANCELC dur=$COLD_DUR containers=$COLD_LEFT $(cat "$OUT/cold-running.json")"
if echo "$CANCELC" | grep -q '"cancelled":true' \
  && jq -e '.success == false and .exitCode == 130 and (.error | test("Ejecución cancelada"))' "$OUT/cold-running.json" >/dev/null \
  && [[ "$COLD_DUR" -lt 1800 && "$COLD_LEFT" == "0" ]]; then
  ok "cancel de una ejecución fría (NIMBUS_WARM=0) mata el container (durationMs=$COLD_DUR)"
else
  bad "cancel cold: $CANCELC dur=$COLD_DUR containers=$COLD_LEFT $(cat "$OUT/cold-running.json")"
fi
unset NIMBUS_WARM

docker exec nimbus-redis redis-cli ZADD nimbus:slots:machine:dead-host-qa 1 ghost-exec >/dev/null
NOWMS="$(python3 -c 'import time; print(int(time.time()*1000)+60000)')"
docker exec nimbus-redis redis-cli ZADD nimbus:slots:machine:live-peer-qa "$NOWMS" live-exec >/dev/null
recreate_compute
DEAD_LEFT="$(docker exec nimbus-redis redis-cli EXISTS nimbus:slots:machine:dead-host-qa | tr -d '[:space:]')"
LIVE_LEFT="$(docker exec nimbus-redis redis-cli ZSCORE nimbus:slots:machine:live-peer-qa live-exec | tr -d '[:space:]')"
echo "  slots muertos exists=$DEAD_LEFT peer=$LIVE_LEFT"
if [[ "$DEAD_LEFT" == "0" && -n "$LIVE_LEFT" && "$LIVE_LEFT" != "(nil)" ]]; then
  ok "el arranque borra nimbus:slots:machine de un host muerto y deja el de un peer vivo"
else
  bad "barrido de slots exists=$DEAD_LEFT peer=$LIVE_LEFT"
fi

# Ocupar los 4 workers para que el siguiente async quede en el stream.
for i in 1 2 3 4; do
  curl -sf -X POST "$API_URL/functions/$HOLD_FN/invoke?async=true" -H 'Content-Type: application/json' \
    -d '{"event":{"sleepMs":6000}}' >"$OUT/busy-$i.json"
done
for _ in $(seq 1 40); do
  READY=0
  for i in 1 2 3 4; do
    ST="$(curl -sf "$API_URL/invocations/$(jq -r .invocationId "$OUT/busy-$i.json")" | jq -r .status)"
    if [[ "$ST" == "RUNNING" ]]; then READY=$((READY+1)); fi
  done
  [[ "$READY" -eq 4 ]] && break
  sleep 0.2
done
echo "  workers ocupados: $READY/4"
PEND="$(curl -sf -X POST "$API_URL/functions/$HOLD_FN/invoke?async=true" -H 'Content-Type: application/json' -d '{"event":{"sleepMs":50}}')"
PEND_EID="$(echo "$PEND" | jq -r .executionId)"
PEND_IID="$(echo "$PEND" | jq -r .invocationId)"
CANCELP="$(curl -sS -w '\nHTTP:%{http_code}' -X POST "$COMPUTE_URL/executions/$PEND_EID/cancel")"
echo "  cancel async pendiente: $CANCELP"
PEND_FINAL=""
for _ in $(seq 1 40); do
  PEND_FINAL="$(curl -sf "$API_URL/invocations/$PEND_IID")"
  ST="$(echo "$PEND_FINAL" | jq -r .status)"
  if [[ "$ST" == "ERROR" || "$ST" == "SUCCESS" || "$ST" == "TIMEOUT" ]]; then break; fi
  sleep 0.4
done
echo "  async cancelado: $(echo "$PEND_FINAL" | jq -c '{status,attempts,error}')"
if echo "$CANCELP" | grep -q '"cancelled":true' && [[ "$(echo "$PEND_FINAL" | jq -r .status)" == "ERROR" ]] && echo "$PEND_FINAL" | grep -q 'cancelada'; then
  ok "cancel de un async que todavía no arrancó termina en ERROR y no corre el handler"
else
  bad "cancel async pendiente: $CANCELP $PEND_FINAL"
fi

echo "=== timeout y OOM en warm, bajo concurrencia ==="
unset NIMBUS_FN_CONCURRENCY NIMBUS_FN_QUEUE_MAX NIMBUS_ASYNC_MAX_ATTEMPTS NIMBUS_ASYNC_BACKOFF_BASE_MS
export NIMBUS_FN_CONCURRENCY=2 NIMBUS_MAX_CONTAINERS=8
recreate_compute
TO="$OUT/to"
mkdir -p "$TO"
cat >"$TO/index.js" <<'EOF'
let n = 0;
exports.handler = async (event) => {
  n += 1;
  if (event.mode === 'sleep') {
    await new Promise((r) => setTimeout(r, Number(event.ms) || 5000));
  }
  if (event.mode === 'mem') {
    const chunks = [];
    const target = (Number(event.mb) || 512) * 1024 * 1024;
    let got = 0;
    while (got < target) { chunks.push(Buffer.alloc(8 * 1024 * 1024, 1)); got += 8 * 1024 * 1024; }
    return { bytes: got };
  }
  return { n, pid: process.pid };
};
EOF
(cd "$TO" && zip -q "$TO/fn.zip" index.js)
TO_FN="$(curl -sf -X POST "$API_URL/functions" -H 'Content-Type: application/json' -d "{\"name\":\"qa-to-$STAMP\"}" | jq -r .id)"
TO_VER="$(curl -sf -X POST "$API_URL/functions/$TO_FN/deploy" -F "file=@$TO/fn.zip" | jq -r .version.version)"
# Prime two warm containers.
call "$(jq -n --arg id "p1-$RANDOM" --arg fn "$TO_FN" --arg ver "$TO_VER" '{executionId:$id,functionId:$fn,version:$ver,event:{}}')" >/dev/null
call "$(jq -n --arg id "p2-$RANDOM" --arg fn "$TO_FN" --arg ver "$TO_VER" '{executionId:$id,functionId:$fn,version:$ver,event:{}}')" >/dev/null
TCODE="$(curl -sS -o "$OUT/timeout.json" -w '%{http_code}' -X POST "$COMPUTE_URL/executions" -H 'Content-Type: application/json' \
  -d "$(jq -n --arg id "to-$RANDOM" --arg fn "$TO_FN" --arg ver "$TO_VER" '{executionId:$id,functionId:$fn,version:$ver,timeoutSec:2,event:{mode:"sleep",ms:8000}}')")"
OCODE="$(curl -sS -o "$OUT/oom.json" -w '%{http_code}' -X POST "$COMPUTE_URL/executions" -H 'Content-Type: application/json' \
  -d "$(jq -n --arg id "oom-$RANDOM" --arg fn "$TO_FN" --arg ver "$TO_VER" '{executionId:$id,functionId:$fn,version:$ver,memoryMb:128,event:{mode:"mem",mb:400}}')")"
NEXT="$(call "$(jq -n --arg id "next-$RANDOM" --arg fn "$TO_FN" --arg ver "$TO_VER" '{executionId:$id,functionId:$fn,version:$ver,event:{}}')")"
echo "  timeout HTTP $TCODE $(jq -c '{exitCode,coldStart}' "$OUT/timeout.json")"
echo "  oom HTTP $OCODE $(jq -c '{exitCode,error}' "$OUT/oom.json")"
echo "  siguiente $(echo "$NEXT" | jq -c '{coldStart,output}')"
if [[ "$TCODE" == "504" && "$(jq -r .exitCode "$OUT/timeout.json")" == "124" && "$OCODE" == "200" && "$(jq -r .exitCode "$OUT/oom.json")" == "137" && "$(echo "$NEXT" | jq -r .coldStart)" == "true" && "$(echo "$NEXT" | jq -r .output.n)" == "1" ]]; then
  ok "timeout y OOM descartan el warm; el siguiente es cold (n=1)"
else
  bad "timeout/OOM timeout=$TCODE oom=$OCODE next=$NEXT"
fi

echo "=== tope NIMBUS_MAX_CONTAINERS ==="
export NIMBUS_MAX_CONTAINERS=3 NIMBUS_FN_CONCURRENCY=3 NIMBUS_FN_QUEUE_MAX=0
recreate_compute
user_containers() {
  docker ps --format '{{.Names}}' | grep -E '^nimbus-' \
    | grep -vE '^nimbus-(postgres|redis|localstack|localstack-init|minio|minio-init|compute-plane|control-plane|console|grafana|prometheus|jaeger|otel-collector)$' \
    || true
}
PEAK=0
CAP_PIDS=()
for i in $(seq 1 8); do
  curl -sS --max-time 25 -o "$OUT/cap-$i.json" -w '%{http_code}' -X POST "$COMPUTE_URL/executions" -H 'Content-Type: application/json' \
    -d "{\"executionId\":\"cap-$i-$RANDOM\",\"functionId\":\"$HOLD_FN\",\"version\":\"$HOLD_VER\",\"timeoutSec\":15,\"maxConcurrency\":3,\"event\":{\"sleepMs\":2500}}" \
    >"$OUT/cap-$i.code" &
  CAP_PIDS+=($!)
done
for _ in 1 2 3 4 5 6 7 8; do
  NOW="$(user_containers | wc -l)"
  if [[ "$NOW" -gt "$PEAK" ]]; then PEAK=$NOW; fi
  sleep 0.3
done
wait "${CAP_PIDS[@]}" || true
OKS=0
REJ=0
for i in $(seq 1 8); do
  CODE="$(cat "$OUT/cap-$i.code")"
  if [[ "$CODE" == "200" ]]; then OKS=$((OKS+1)); fi
  if [[ "$CODE" == "429" ]]; then REJ=$((REJ+1)); fi
done
echo "  peak user containers=$PEAK success=$OKS rejected=$REJ"
if [[ "$PEAK" -le 3 && "$OKS" -ge 1 && "$REJ" -ge 1 ]]; then
  ok "NIMBUS_MAX_CONTAINERS=3: pico $PEAK containers, $OKS corrieron y $REJ recibieron 429"
else
  bad "tope global peak=$PEAK ok=$OKS rej=$REJ"
fi

echo "=== stress 80 invokes ==="
unset NIMBUS_MAX_CONTAINERS NIMBUS_FN_CONCURRENCY NIMBUS_FN_QUEUE_MAX
recreate_compute
FAST="$OUT/fast"
mkdir -p "$FAST"
cat >"$FAST/index.js" <<'EOF'
exports.handler = async (event) => ({ ok: true, i: event.i, pid: process.pid });
EOF
(cd "$FAST" && zip -q "$FAST/fn.zip" index.js)
declare -a FNS VERS
for i in 1 2 3 4; do
  FNS[$i]="$(curl -sf -X POST "$API_URL/functions" -H 'Content-Type: application/json' -d "{\"name\":\"qa-fast-$STAMP-$i\"}" | jq -r .id)"
  VERS[$i]="$(curl -sf -X POST "$API_URL/functions/${FNS[$i]}/deploy" -F "file=@$FAST/fn.zip" | jq -r .version.version)"
done
MET_BEFORE="$(curl -sf "$COMPUTE_URL/metrics" | awk '/^nimbus_invocations_total/ {s+=$NF} END {print s+0}')"
python3 - << PY
import json, os, urllib.request, concurrent.futures
api = os.environ["API_URL"]
fns = """${FNS[1]} ${FNS[2]} ${FNS[3]} ${FNS[4]}""".split()
def one(n):
    fn = fns[n % 4]
    async_q = "&async=true" if n % 5 == 0 else ""
    # query string only when async
    url = f"{api}/functions/{fn}/invoke" + ("?async=true" if n % 5 == 0 else "")
    data = json.dumps({"event": {"i": n}}).encode()
    req = urllib.request.Request(url, data=data, headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=30) as res:
            body = json.loads(res.read().decode())
            return res.status, body.get("invocationId"), body.get("status")
    except Exception as e:
        if hasattr(e, "read"):
            return getattr(e, "code", 0), None, e.read().decode()[:180]
        return 0, None, str(e)
with concurrent.futures.ThreadPoolExecutor(24) as ex:
    rows = list(ex.map(one, range(80)))
open("$OUT/stress.json","w").write(json.dumps(rows))
print("stress http", {c: sum(1 for r in rows if r[0]==c) for c in sorted({r[0] for r in rows})})
PY
# Esperar terminales.
python3 - << 'PY'
import json, os, time, urllib.request
api = os.environ["API_URL"]
rows = json.load(open(os.environ["QA_STRESS"] if False else "/tmp/nimbus-qa/stress.json"))
ids = [r[1] for r in rows if r[1]]
pending = set(ids)
last = {}
deadline = time.time() + 40
while pending and time.time() < deadline:
    done = []
    for iid in list(pending):
        try:
            with urllib.request.urlopen(f"{api}/invocations/{iid}", timeout=10) as res:
                body = json.loads(res.read().decode())
        except Exception:
            continue
        last[iid] = body.get("status")
        if body.get("status") in ("SUCCESS", "ERROR", "TIMEOUT"):
            done.append(iid)
    for iid in done:
        pending.discard(iid)
    time.sleep(0.3)
from collections import Counter
print("terminal", Counter(last.values()), "still", len(pending))
open("/tmp/nimbus-qa/stress-status.txt","w").write(f"{Counter(last.values())} pending={len(pending)}\n")
PY
echo "  $(cat "$OUT/stress-status.txt" 2>/dev/null || echo 'sin resumen')"
if grep -q "pending=0" "$OUT/stress-status.txt" && grep -q "SUCCESS" "$OUT/stress-status.txt"; then
  ok "80 invokes mixtos terminaron en un estado terminal"
else
  bad "stress no cerró: $(cat "$OUT/stress-status.txt" 2>/dev/null)"
fi
sleep 1
HOST="$(docker exec nimbus-compute-plane hostname)"
SLOTS="$(docker exec nimbus-redis redis-cli ZCARD "nimbus:slots:machine:$HOST")"
EXITED="$(docker ps -a --filter status=exited --format '{{.Names}}' | grep -E '^nimbus-' | grep -vE '^nimbus-(postgres|redis|localstack|localstack-init|minio|minio-init|compute-plane|control-plane|console|grafana|prometheus|jaeger|otel-collector)$' || true)"
WARM_NOW="$(docker ps -q --filter label=nimbus.warm=1 | wc -l)"
echo "  slots máquina actual=$SLOTS warm vivos=$WARM_NOW"
echo "  exited usuario: ${EXITED:-ninguno}"
MET_AFTER="$(curl -sf "$COMPUTE_URL/metrics" | awk '/^nimbus_invocations_total/ {s+=$NF} END {print s+0}')"
echo "  invocations_total $MET_BEFORE -> $MET_AFTER"
DELTA="$(python3 - << PY
print(int(float("${MET_AFTER:-0}")) - int(float("${MET_BEFORE:-0}")))
PY
)"
echo "  delta métricas=$DELTA warm=$WARM_NOW"
# Los warm ociosos son el pool, no una fuga. Un exited con nombre de ejecución sí.
if [[ "$SLOTS" == "0" && -z "$EXITED" && "$WARM_NOW" -le 8 && "$DELTA" -ge 70 ]]; then
  ok "sin slots vencidos ni containers exited; warm=$WARM_NOW (<=8); invocations_total +$DELTA"
else
  bad "fugas slots=$SLOTS exited=$(echo "$EXITED" | wc -w) warm=$WARM_NOW delta=$DELTA"
fi

echo "=== restart de compute, control y redis en medio de async ==="
RST_PIDS=()
for i in $(seq 1 8); do
  curl -sf --max-time 15 -X POST "$API_URL/functions/$HOLD_FN/invoke?async=true" -H 'Content-Type: application/json' \
    -d '{"event":{"sleepMs":2500}}' >"$OUT/rst-$i.json" &
  RST_PIDS+=($!)
done
wait "${RST_PIDS[@]}" || true
BEFORE_WARM=""
for _ in 1 2 3 4 5 6 7 8; do
  BEFORE_WARM="$(docker ps -q --filter 'label=nimbus.warm=1' | sort | tr '\n' ' ')"
  [[ -n "${BEFORE_WARM// }" ]] && break
  sleep 0.3
done
echo "  warm antes del restart: ${BEFORE_WARM:-ninguno}"
docker restart nimbus-redis nimbus-compute-plane nimbus-control-plane >/dev/null
wait_url "$API_URL/health" "control tras restart" || bad "control no volvió"
wait_url "$COMPUTE_URL/health" "compute tras restart" || bad "compute no volvió"
sleep 1
STILL=""
for id in $BEFORE_WARM; do
  if docker ps -aq --no-trunc | grep -q "$id"; then
    STILL="$STILL $id"
  fi
done
echo "  ids viejos que siguen: ${STILL:-ninguno}"
RST_LEFT=0
RST_OK=0
for i in $(seq 1 8); do
  IID="$(jq -r .invocationId "$OUT/rst-$i.json")"
  ST=""
  # XAUTOCLAIM espera NIMBUS_ASYNC_CLAIM_IDLE_MS (20s) y este handler duerme
  # 2.5s. 20s de sondeo cerraba antes de que el intento recuperado terminara.
  for _ in $(seq 1 80); do
    ST="$(curl -sf "$API_URL/invocations/$IID" | jq -r .status)"
    if [[ "$ST" == "SUCCESS" || "$ST" == "ERROR" || "$ST" == "TIMEOUT" ]]; then break; fi
    sleep 0.5
  done
  echo "  restart #$i $ST"
  if [[ "$ST" == "SUCCESS" || "$ST" == "ERROR" || "$ST" == "TIMEOUT" ]]; then RST_OK=$((RST_OK+1)); else RST_LEFT=$((RST_LEFT+1)); fi
done
WARM_LEFT="$(docker ps -q --filter 'label=nimbus.warm=1' | wc -l)"
echo "  warm vivos tras los async=$WARM_LEFT terminal=$RST_OK no-terminal=$RST_LEFT"
# Los jobs que se reintentan después del restart arman un pool nuevo. El tope
# global de esta sección es 3. Un warm vivo acá no es un huérfano del proceso anterior.
if [[ "$RST_LEFT" == "0" && -z "$STILL" && "$WARM_LEFT" -le 3 ]]; then
  ok "8 async terminaron; los containers de antes del restart desaparecieron (pool nuevo=$WARM_LEFT)"
else
  bad "restart terminal=$RST_OK left=$RST_LEFT warm=$WARM_LEFT still=${STILL:-ninguno}"
fi

echo "=== seguridad ==="
SEC="$OUT/sec"
mkdir -p "$SEC"
cat >"$SEC/index.js" <<'EOF'
const fs = require("fs");
const net = require("net");
exports.handler = async () => {
  const out = { uid: process.getuid() };
  for (const p of ["/var/task/pwn.txt", "/etc/pwn.txt", "/tmp/pwn.txt"]) {
    try { fs.writeFileSync(p, "x"); out[p] = "ok"; }
    catch (e) { out[p] = e.code || String(e.message); }
  }
  try {
    await new Promise((resolve, reject) => {
      const s = net.connect(80, "1.1.1.1");
      s.on("connect", () => resolve());
      s.on("error", reject);
      setTimeout(() => reject(new Error("timeout")), 800);
    });
    out.net = "connected";
  } catch (e) { out.net = String(e.code || e.message); }
  return out;
};
EOF
(cd "$SEC" && zip -q "$SEC/fn.zip" index.js)
SEC_FN="$(curl -sf -X POST "$API_URL/functions" -H 'Content-Type: application/json' -d "{\"name\":\"qa-sec-$STAMP\"}" | jq -r .id)"
SEC_VER="$(curl -sf -X POST "$API_URL/functions/$SEC_FN/deploy" -F "file=@$SEC/fn.zip" | jq -r .version.version)"
SEC_OUT="$(call "$(jq -n --arg id "sec-$RANDOM" --arg fn "$SEC_FN" --arg ver "$SEC_VER" '{executionId:$id,functionId:$fn,version:$ver,timeoutSec:10,event:{}}')")"
echo "  $SEC_OUT"
UIDN="$(echo "$SEC_OUT" | jq -r .output.uid)"
if [[ "$UIDN" == "1000" && "$(echo "$SEC_OUT" | jq -r '.output["/var/task/pwn.txt"]')" == "EROFS" && "$(echo "$SEC_OUT" | jq -r '.output["/etc/pwn.txt"]')" == "EROFS" && "$(echo "$SEC_OUT" | jq -r .output.net)" != "connected" ]]; then
  ok "uid=$UIDN, fs de task/etc read-only, red no conecta ($(echo "$SEC_OUT" | jq -r .output.net))"
else
  bad "aislamiento $SEC_OUT"
fi
SECRET="qa-secret-$RANDOM-no-loguear"
ENVOUT="$(call "$(jq -n --arg id "envsec-$RANDOM" --arg fn "$SEC_FN" --arg ver "$SEC_VER" --arg s "$SECRET" '{executionId:$id,functionId:$fn,version:$ver,event:{},envVars:{GREETING:$s}}')")"
if docker logs nimbus-compute-plane 2>&1 | grep -F "$SECRET" >/dev/null; then
  bad "el valor de envVars apareció en el log del compute plane"
else
  ok "el valor de envVars no está en el log del compute plane"
fi

python3 - << PY
import zipfile, os
base = "$OUT/mal"
os.makedirs(base, exist_ok=True)
slip = os.path.join(base, "slip.zip")
with zipfile.ZipFile(slip, "w") as z:
    z.writestr("../evil-qa.txt", "pwned")
    z.writestr("index.js", "exports.handler=async()=>({ok:true});")
sym = os.path.join(base, "sym.zip")
info = zipfile.ZipInfo("escape")
info.create_system = 3
info.external_attr = (0o120777) << 16
with zipfile.ZipFile(sym, "w") as z:
    z.writestr(info, "/tmp")
    z.writestr("index.js", "exports.handler=async()=>({ok:true});")
bomb = os.path.join(base, "bomb.zip")
with zipfile.ZipFile(bomb, "w", compression=zipfile.ZIP_DEFLATED) as z:
    z.writestr("index.js", "exports.handler=async()=>({bomb:true});")
    z.writestr("big.bin", b"\\0" * (40 * 1024 * 1024))
print("bomb zip bytes", os.path.getsize(bomb))
PY
for kind in slip sym bomb; do
  MFN="$(curl -sf -X POST "$API_URL/functions" -H 'Content-Type: application/json' -d "{\"name\":\"qa-$kind-$STAMP\"}" | jq -r .id)"
  MVER="$(curl -sf -X POST "$API_URL/functions/$MFN/deploy" -F "file=@$OUT/mal/$kind.zip" | jq -r .version.version)"
  MOUT="$(curl -sS -X POST "$COMPUTE_URL/executions" -H 'Content-Type: application/json' --max-time 60 \
    -d "{\"executionId\":\"$kind-$RANDOM\",\"functionId\":\"$MFN\",\"version\":\"$MVER\",\"event\":{}}")"
  echo "  $kind $MOUT"
  echo "$MOUT" >"$OUT/$kind.json"
done
if jq -e '.success == false' "$OUT/slip.json" >/dev/null && ! find /tmp/nimbus-work -name 'evil-qa.txt' | grep -q .; then
  ok "path traversal rechazado y sin archivo fuera del directorio"
else
  bad "path traversal $(cat "$OUT/slip.json")"
fi
if jq -e '.success == false' "$OUT/sym.json" >/dev/null && ! find /tmp/nimbus-work -type l | grep -q .; then
  ok "symlink del zip rechazado"
else
  bad "symlink $(cat "$OUT/sym.json")"
fi
if jq -e '.success == false' "$OUT/bomb.json" >/dev/null; then
  ok "zip de 40 MiB descomprimidos rechazado"
else
  bad "zip bomb aceptado $(cat "$OUT/bomb.json")"
fi
BAD="$(curl -sS -o "$OUT/bad.json" -w '%{http_code}' -X POST "$COMPUTE_URL/executions" -H 'Content-Type: application/json' -d 'no-es-json')"
HUGE_CODE="$(python3 - << PY
import json, urllib.request
body = {"executionId":"huge","functionId":"fn","version":"v","event":{"pad":"x"*1200000}}
req = urllib.request.Request("$COMPUTE_URL/executions", data=json.dumps(body).encode(), headers={"Content-Type":"application/json"})
try:
    with urllib.request.urlopen(req, timeout=20) as res:
        print(res.status)
except Exception as e:
    print(getattr(e, "code", 0))
    open("$OUT/huge.json","wb").write(e.read() if hasattr(e,"read") else b"")
PY
)"
echo "  bad json HTTP $BAD $(cat "$OUT/bad.json")"
echo "  huge HTTP $HUGE_CODE $(cat "$OUT/huge.json" 2>/dev/null | head -c 200)"
if [[ "$BAD" == "400" && "$HUGE_CODE" == "400" ]]; then
  ok "JSON inválido y cuerpo >1MB responden 400"
else
  bad "bad=$BAD huge=$HUGE_CODE"
fi

echo "=== compatibilidad: body mínimo, WARM=0, TRACING=0 ==="
MIN="$(curl -sS -o "$OUT/min.json" -w '%{http_code}' -X POST "$COMPUTE_URL/executions" -H 'Content-Type: application/json' \
  -d "{\"executionId\":\"min-$RANDOM\",\"functionId\":\"$FN\",\"version\":\"$VER\",\"event\":{\"name\":\"Min\"}}")"
echo "  minimal HTTP $MIN $(jq -c '{success,output}' "$OUT/min.json")"
if [[ "$MIN" == "200" ]] && jq -e '.success == true' "$OUT/min.json" >/dev/null; then
  ok "POST /executions mínimo sigue en 200"
else
  bad "body mínimo $MIN $(cat "$OUT/min.json")"
fi

export NIMBUS_WARM=0
recreate_compute
BEFORE_WARM="$(docker ps -a --filter 'label=nimbus.warm=1' -q | wc -l)"
WARM0="$(curl -sS -X POST "$COMPUTE_URL/executions" -H 'Content-Type: application/json' \
  -d "{\"executionId\":\"warm0-$RANDOM\",\"functionId\":\"$FN\",\"version\":\"$VER\",\"event\":{\"name\":\"Frio\"}}")"
AFTER_WARM="$(docker ps -a --filter 'label=nimbus.warm=1' -q | wc -l)"
echo "  WARM=0 $WARM0 warm_containers $BEFORE_WARM->$AFTER_WARM"
if echo "$WARM0" | jq -e '.success == true and .coldStart == true' >/dev/null && [[ "$AFTER_WARM" == "$BEFORE_WARM" ]]; then
  ok "NIMBUS_WARM=0 ejecuta y no deja containers warm"
else
  bad "WARM=0 $WARM0"
fi
unset NIMBUS_WARM

export NIMBUS_TRACING=0
recreate_compute
TR="$(curl -sS -o "$OUT/tr.json" -w '%{http_code}' -X POST "$COMPUTE_URL/executions" -H 'Content-Type: application/json' \
  -d "{\"executionId\":\"tr-$RANDOM\",\"functionId\":\"$FN\",\"version\":\"$VER\",\"event\":{\"name\":\"SinTraza\"}}")"
echo "  TRACING=0 HTTP $TR $(jq -c '{success}' "$OUT/tr.json")"
if [[ "$TR" == "200" ]] && jq -e '.success == true' "$OUT/tr.json" >/dev/null; then
  ok "NIMBUS_TRACING=0 no impide la invocación"
else
  bad "TRACING=0 $TR $(cat "$OUT/tr.json")"
fi
unset NIMBUS_TRACING

echo
echo "=== resumen PASS=$PASS FAIL=$FAIL ==="
echo "log: $LOG"
if [[ "$FAIL" -eq 0 ]]; then
  exit 0
fi
exit 1
