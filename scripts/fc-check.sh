#!/usr/bin/env bash
# Firecracker proof against a stack started with `make up-fc`.
# FC_BENCH_DOCKER=1 measures the same hello cold/warm on the Docker backend
# and writes /tmp/nimbus-bench-docker.tsv. It does not switch isolation.

set -euo pipefail

API_URL="${API_URL:-http://localhost:3000}"
COMPUTE_URL="${COMPUTE_URL:-http://localhost:8080}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
WORKDIR="$(mktemp -d)"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'
log_info() { echo -e "${YELLOW}[fc-check]${NC} $1"; }
log_ok() { echo -e "${GREEN}[✓]${NC} $1"; }
log_fail() { echo -e "${RED}[✗]${NC} $1"; exit 1; }

command -v curl >/dev/null || log_fail "curl requerido"
command -v jq >/dev/null || log_fail "jq requerido"
command -v zip >/dev/null || log_fail "zip requerido"
command -v python3 >/dev/null || log_fail "python3 requerido"
command -v docker >/dev/null || log_fail "docker requerido"

COMPOSE=(docker compose -f "$ROOT/docker-compose.yml" -f "$ROOT/docker-compose.firecracker.yml")
CHANGED=0
restore_fc() {
  if [[ "$CHANGED" != "1" ]]; then
    return
  fi
  log_info "restaurando NIMBUS_WARM=1 NIMBUS_FC_JAILER=0"
  NIMBUS_WARM=1 NIMBUS_FC_JAILER=0 "${COMPOSE[@]}" up -d --no-deps --force-recreate --no-build compute-plane >/dev/null || true
}
trap 'restore_fc; rm -rf "$WORKDIR"' EXIT

wait_url() {
  local url="$1" name="$2"
  for _ in $(seq 1 90); do
    if curl -sf "$url" >/dev/null 2>&1; then
      return
    fi
    sleep 1
  done
  log_fail "$name no responde en $url"
}

median() {
  python3 -c 'import sys; xs=sorted(int(x) for x in sys.argv[1:]); n=len(xs); print(xs[n//2] if n % 2 else (xs[n//2-1]+xs[n//2])//2)' "$@"
}

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

invoke() {
  local rt="$1" fn="$2" ver="$3" handler="$4" extra="$5" out="$6" maxtime="$7" eid="$8"
  local payload
  payload="$(jq -n \
    --arg id "$eid" --arg fn "$fn" --arg ver "$ver" --arg rt "$rt" \
    --arg handler "$handler" --argjson extra "$extra" \
    '{executionId:$id, functionId:$fn, version:$ver, runtime:$rt} + $extra + (if $handler == "" then {} else {handler:$handler} end)')"
  curl -sS -o "$out" -w '%{http_code}' -X POST "$COMPUTE_URL/executions" \
    -H 'Content-Type: application/json' --max-time "$maxtime" -d "$payload"
}

wait_url "$COMPUTE_URL/health" "Compute plane"
wait_url "$API_URL/health" "Control plane"
ISO="$(curl -sf "$COMPUTE_URL/health" | jq -r '.isolation // empty')"
log_info "health.isolation=$ISO"

if [[ "${FC_BENCH_DOCKER:-}" == "1" ]]; then
  [[ "$ISO" == "docker" ]] || log_fail "FC_BENCH_DOCKER pide isolation=docker (hay $ISO)"
else
  [[ "$ISO" == "firecracker" ]] || log_fail "hace falta make up-fc (isolation=$ISO)"
fi

STAMP="$(date +%H%M%S)-$RANDOM"

log_info "Empaquetando hello node"
cat > "$WORKDIR/index.js" <<'EOF'
exports.handler = async (event) => {
  const mode = event && event.mode;
  if (mode === 'sleep') {
    console.log('sleep-start');
    await new Promise((r) => setTimeout(r, Number(event.ms) || 5000));
    return { slept: true };
  }
  if (mode === 'mem') {
    const mb = Number(event.mb) || 512;
    const chunks = [];
    const block = Buffer.alloc(8 * 1024 * 1024, 1);
    let got = 0;
    while (got < mb * 1024 * 1024) {
      chunks.push(Buffer.from(block));
      got += block.length;
    }
    return { bytes: got };
  }
  if (mode === 'error') {
    throw new Error((event && event.message) || 'boom');
  }
  if (mode === 'log') {
    console.log('linea-visible');
    await new Promise((r) => setTimeout(r, Number(event.ms) || 3000));
    return { logged: true };
  }
  if (mode === 'cpus') {
    return { cpus: require('os').cpus().length };
  }
  if (mode === 'probe') {
    const fs = require('fs');
    const os = require('os');
    const net = require('net');
    const write = (p) => {
      try { fs.writeFileSync(p, 'x'); return 'ok'; }
      catch (e) { return e.code || e.message; }
    };
    let netErr = 'ok';
    try {
      await new Promise((resolve, reject) => {
        const s = net.connect(443, '1.1.1.1', () => { s.end(); resolve(); });
        s.setTimeout(1000, () => { s.destroy(); reject(new Error('timeout')); });
        s.on('error', reject);
      });
    } catch (e) {
      netErr = e.code || e.message;
    }
    return {
      uid: process.getuid(),
      ifaces: Object.keys(os.networkInterfaces()),
      taskWrite: write('/var/task/pwn.txt'),
      tmpWrite: write('/tmp/pwn.txt'),
      net: netErr,
    };
  }
  console.log('hola desde node');
  return {
    message: 'Hola, ' + ((event && event.name) || 'mundo'),
    runtime: 'nodejs20',
    uid: process.getuid(),
  };
};
EOF
(cd "$WORKDIR" && zip -q hello-node.zip index.js)
read -r NODE_FN NODE_VER <<<"$(deploy "fc-node-$STAMP" "$WORKDIR/hello-node.zip")"

log_info "Empaquetando hello-python"
(cd "$ROOT/examples/hello-python" && zip -q "$WORKDIR/hello-python.zip" index.py)
read -r PY_FN PY_VER <<<"$(deploy "fc-py-$STAMP" "$WORKDIR/hello-python.zip")"

log_info "Compilando hello-go"
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
read -r GO_FN GO_VER <<<"$(deploy "fc-go-$STAMP" "$WORKDIR/hello-go.zip")"
log_ok "funciones $NODE_FN $PY_FN $GO_FN"

bench() {
  local rt="$1" fn="$2" ver="$3" handler="$4" label="$5"
  local i out code
  local colds=() inits=() warms=()
  log_info "$label prime (descarga, no entra en la tabla)"
  out="$WORKDIR/${label}-prime.json"
  code="$(invoke "$rt" "$fn" "$ver" "$handler" '{"event":{"name":"Ada"},"timeoutSec":40,"memoryMb":128,"envVars":{"POOL_BUST":"prime"}}' "$out" 60 "prime-${label}-$RANDOM")"
  [[ "$code" == "200" ]] || log_fail "$label prime HTTP $code $(cat "$out")"
  jq -e '.success == true and (.output.message | test("Ada"))' "$out" >/dev/null \
    || log_fail "$label prime: $(cat "$out")"

  for i in 1 2 3; do
    out="$WORKDIR/${label}-cold-$i.json"
    code="$(invoke "$rt" "$fn" "$ver" "$handler" "{\"event\":{\"name\":\"Ada\"},\"timeoutSec\":40,\"memoryMb\":128,\"envVars\":{\"POOL_BUST\":\"cold-$i\"}}" "$out" 60 "cold-${label}-$i-$RANDOM")"
    echo "  cold $i HTTP $code $(jq -c '{success,coldStart,initMs,durationMs,output}' "$out")"
    [[ "$code" == "200" ]] || log_fail "$label cold $i HTTP $code"
    jq -e '.success == true and .coldStart == true and (.output.message | test("Ada")) and .output.uid == 1000' "$out" >/dev/null \
      || log_fail "$label cold $i: $(cat "$out")"
    colds+=("$(jq -r '.durationMs' "$out")")
    inits+=("$(jq -r '.initMs' "$out")")
  done
  out="$WORKDIR/${label}-warm-setup.json"
  code="$(invoke "$rt" "$fn" "$ver" "$handler" '{"event":{"name":"Ada"},"timeoutSec":40,"memoryMb":128,"envVars":{"POOL_BUST":"warm"}}' "$out" 60 "warm-setup-${label}-$RANDOM")"
  [[ "$code" == "200" ]] || log_fail "$label warm setup HTTP $code $(cat "$out")"
  for i in 1 2 3; do
    out="$WORKDIR/${label}-warm-$i.json"
    code="$(invoke "$rt" "$fn" "$ver" "$handler" '{"event":{"name":"Ada"},"timeoutSec":40,"memoryMb":128,"envVars":{"POOL_BUST":"warm"}}' "$out" 30 "warm-${label}-$i-$RANDOM")"
    echo "  warm $i HTTP $code $(jq -c '{success,coldStart,initMs,durationMs}' "$out")"
    [[ "$code" == "200" ]] || log_fail "$label warm $i HTTP $code"
    jq -e '.success == true and .coldStart == false and .initMs == 0' "$out" >/dev/null \
      || log_fail "$label warm $i: $(cat "$out")"
    warms+=("$(jq -r '.durationMs' "$out")")
  done
  local cmed imed wmed
  cmed="$(median "${colds[@]}")"
  imed="$(median "${inits[@]}")"
  wmed="$(median "${warms[@]}")"
  log_ok "$label cold duration ${colds[*]} (mediana $cmed) init ${inits[*]} (mediana $imed) warm ${warms[*]} (mediana $wmed)"
  echo "$label $cmed $imed $wmed" >> "$BENCH_FILE"
}

if [[ "${FC_BENCH_DOCKER:-}" == "1" ]]; then
  BENCH_FILE="/tmp/nimbus-bench-docker.tsv"
else
  BENCH_FILE="/tmp/nimbus-bench-firecracker.tsv"
fi
: > "$BENCH_FILE"
bench nodejs20 "$NODE_FN" "$NODE_VER" "index.handler" nodejs20
bench python3.12 "$PY_FN" "$PY_VER" "index.handler" python3.12
bench go1.x "$GO_FN" "$GO_VER" "" go1.x
log_ok "tabla en $BENCH_FILE"
column -t "$BENCH_FILE" || cat "$BENCH_FILE"

if [[ "${FC_BENCH_DOCKER:-}" == "1" ]]; then
  log_ok "bench docker listo"
  exit 0
fi

log_info "timeout 124 (python, VM ya warm)"
TO_OUT="$WORKDIR/timeout.json"
TO_CODE="$(invoke python3.12 "$PY_FN" "$PY_VER" "index.handler" \
  '{"event":{"mode":"sleep","ms":8000},"timeoutSec":2,"memoryMb":128,"envVars":{"POOL_BUST":"timeout"}}' \
  "$TO_OUT" 30 "to-$RANDOM")"
echo "  HTTP $TO_CODE $(jq -c '{exitCode,durationMs,error,coldStart}' "$TO_OUT")"
[[ "$TO_CODE" == "504" ]] || log_fail "timeout HTTP $TO_CODE"
DUR="$(jq -r '.durationMs' "$TO_OUT")"
[[ "$(jq -r '.exitCode' "$TO_OUT")" == "124" && "$DUR" -lt 7000 ]] || log_fail "timeout no cortó: $(cat "$TO_OUT")"
NEXT="$WORKDIR/timeout-next.json"
invoke python3.12 "$PY_FN" "$PY_VER" "index.handler" \
  '{"event":{"name":"Ada"},"timeoutSec":40,"memoryMb":128,"envVars":{"POOL_BUST":"timeout"}}' \
  "$NEXT" 60 "to-next-$RANDOM" >/dev/null
jq -e '.success == true and .coldStart == true' "$NEXT" >/dev/null \
  || log_fail "después del timeout el VM siguió en el pool: $(cat "$NEXT")"
log_ok "timeout exit 124 en ${DUR}ms y el siguiente es cold"

log_info "OOM memoria 128 (python pide 400 MB)"
OOM_OUT="$WORKDIR/oom.json"
OOM_CODE="$(invoke python3.12 "$PY_FN" "$PY_VER" "index.handler" \
  '{"event":{"mode":"mem","mb":400},"timeoutSec":25,"memoryMb":128,"envVars":{"POOL_BUST":"oom"}}' \
  "$OOM_OUT" 40 "oom-$RANDOM")"
echo "  HTTP $OOM_CODE $(jq -c '{success,exitCode,error,durationMs}' "$OOM_OUT")"
EXITC="$(jq -r '.exitCode // empty' "$OOM_OUT")"
[[ "$EXITC" == "137" || "$EXITC" == "134" ]] || log_fail "OOM exit=$EXITC $(cat "$OOM_OUT")"
echo "$(jq -r '.error // empty' "$OOM_OUT")" | grep -qi 'memoria' || log_fail "OOM sin texto de memoria"
NEXT="$WORKDIR/oom-next.json"
invoke python3.12 "$PY_FN" "$PY_VER" "index.handler" \
  '{"event":{"name":"Ada"},"timeoutSec":40,"memoryMb":128,"envVars":{"POOL_BUST":"oom"}}' \
  "$NEXT" 60 "oom-next-$RANDOM" >/dev/null
jq -e '.coldStart == true' "$NEXT" >/dev/null || log_fail "después del OOM el VM volvió al pool: $(cat "$NEXT")"
log_ok "OOM python exit $EXITC"

log_info "OOM memoria 128 (go pide 400 MB)"
GOOM_OUT="$WORKDIR/go-oom.json"
GOOM_CODE="$(invoke go1.x "$GO_FN" "$GO_VER" "" \
  '{"event":{"mode":"mem","mb":400},"timeoutSec":25,"memoryMb":128,"envVars":{"POOL_BUST":"go-oom"}}' \
  "$GOOM_OUT" 40 "goom-$RANDOM")"
echo "  HTTP $GOOM_CODE $(jq -c '{success,exitCode,error,durationMs}' "$GOOM_OUT")"
GEXIT="$(jq -r '.exitCode // empty' "$GOOM_OUT")"
[[ "$GEXIT" == "137" || "$GEXIT" == "134" ]] || log_fail "OOM go exit=$GEXIT $(cat "$GOOM_OUT")"
echo "$(jq -r '.error // empty' "$GOOM_OUT")" | grep -qi 'memoria' || log_fail "OOM go sin texto de memoria"
log_ok "OOM go exit $GEXIT"

log_info "error de handler y reutilización (node y python)"
for pair in "nodejs20|$NODE_FN|$NODE_VER|index.handler" "python3.12|$PY_FN|$PY_VER|index.handler"; do
  IFS='|' read -r rt fn ver handler <<<"$pair"
  ERR_OUT="$WORKDIR/err-$rt.json"
  ERR_CODE="$(invoke "$rt" "$fn" "$ver" "$handler" \
    '{"event":{"mode":"error","message":"boom"},"timeoutSec":20,"memoryMb":128,"envVars":{"POOL_BUST":"err"}}' \
    "$ERR_OUT" 30 "err-$rt-$RANDOM")"
  echo "  $rt HTTP $ERR_CODE $(jq -c '{success,error,exitCode}' "$ERR_OUT")"
  [[ "$ERR_CODE" == "200" ]] || log_fail "$rt error HTTP $ERR_CODE"
  jq -e '.success == false and (.error | test("boom"))' "$ERR_OUT" >/dev/null \
    || log_fail "$rt error: $(cat "$ERR_OUT")"
  NEXT="$WORKDIR/err-next-$rt.json"
  invoke "$rt" "$fn" "$ver" "$handler" \
    '{"event":{"name":"Ada"},"timeoutSec":40,"memoryMb":128,"envVars":{"POOL_BUST":"err"}}' \
    "$NEXT" 60 "err-next-$rt-$RANDOM" >/dev/null
  jq -e '.success == true and .coldStart == false' "$NEXT" >/dev/null \
    || log_fail "$rt no reutilizó el VM tras un error de handler: $(cat "$NEXT")"
  log_ok "$rt error boom y el VM sigue"
done

log_info "cancel (node, sleep 8s, corte después de sleep-start)"
CAN_EID="can-$RANDOM"
CAN_OUT="$WORKDIR/cancel.json"
CAN_CODE_FILE="$WORKDIR/cancel.code"
invoke nodejs20 "$NODE_FN" "$NODE_VER" "index.handler" \
  '{"event":{"mode":"sleep","ms":8000},"timeoutSec":20,"memoryMb":128,"envVars":{"POOL_BUST":"cancel"}}' \
  "$CAN_OUT" 25 "$CAN_EID" > "$CAN_CODE_FILE" &
CAN_PID=$!
SEEN=0
for _ in $(seq 1 80); do
  if docker exec nimbus-redis redis-cli XRANGE "nimbus:logs:$CAN_EID" - + 2>/dev/null | grep -q 'sleep-start'; then
    SEEN=1
    break
  fi
  if ! kill -0 "$CAN_PID" 2>/dev/null; then
    break
  fi
  sleep 0.2
done
sleep 0.3
curl -sS -X POST "$COMPUTE_URL/executions/$CAN_EID/cancel" >/dev/null || true
wait "$CAN_PID" || true
CAN_CODE="$(cat "$CAN_CODE_FILE")"
echo "  HTTP $CAN_CODE $(jq -c '{success,exitCode,error,durationMs}' "$CAN_OUT" 2>/dev/null || echo '{}')"
[[ "$SEEN" == "1" ]] || log_fail "el handler no llegó a correr antes del cancel"
[[ "$CAN_CODE" == "409" ]] || log_fail "cancel HTTP $CAN_CODE"
[[ "$(jq -r '.exitCode' "$CAN_OUT")" == "130" ]] || log_fail "cancel exit $(cat "$CAN_OUT")"
echo "$(jq -r '.error' "$CAN_OUT")" | grep -q 'Ejecución cancelada' || log_fail "cancel sin el mensaje"
DUR="$(jq -r '.durationMs' "$CAN_OUT")"
[[ "$DUR" -lt 6000 ]] || log_fail "cancel tardó ${DUR}ms (el sleep era 8s)"
NEXT="$WORKDIR/cancel-next.json"
invoke nodejs20 "$NODE_FN" "$NODE_VER" "index.handler" \
  '{"event":{"name":"Ada"},"timeoutSec":40,"memoryMb":128,"envVars":{"POOL_BUST":"cancel"}}' \
  "$NEXT" 60 "can-next-$RANDOM" >/dev/null
jq -e '.coldStart == true' "$NEXT" >/dev/null || log_fail "el VM cancelado volvió al pool: $(cat "$NEXT")"
log_ok "cancel 409 exit 130 en ${DUR}ms y el VM no volvió"

log_info "sin red y filesystem (python)"
PR_OUT="$WORKDIR/probe.json"
PR_CODE="$(invoke python3.12 "$PY_FN" "$PY_VER" "index.handler" \
  '{"event":{"mode":"probe"},"timeoutSec":20,"memoryMb":128,"envVars":{"POOL_BUST":"probe"}}' \
  "$PR_OUT" 30 "probe-$RANDOM")"
echo "  HTTP $PR_CODE $(jq -c '.output' "$PR_OUT")"
[[ "$PR_CODE" == "200" ]] || log_fail "probe HTTP $PR_CODE"
jq -e '.success == true and .output.uid == 1000 and (.output.traceId | length) > 0 and .output.taskWrite != "ok" and .output.tmpWrite == "ok" and .output.net != "ok"' "$PR_OUT" >/dev/null \
  || log_fail "probe: $(cat "$PR_OUT")"
log_ok "uid 1000, trace, /var/task ro, /tmp rw, dial 1.1.1.1 falló"

log_info "interfaces del guest (node)"
IF_OUT="$WORKDIR/ifaces.json"
IF_CODE="$(invoke nodejs20 "$NODE_FN" "$NODE_VER" "index.handler" \
  '{"event":{"mode":"probe"},"timeoutSec":20,"memoryMb":128,"envVars":{"POOL_BUST":"ifaces"}}' \
  "$IF_OUT" 40 "ifaces-$RANDOM")"
echo "  HTTP $IF_CODE $(jq -c '.output' "$IF_OUT")"
[[ "$IF_CODE" == "200" ]] || log_fail "ifaces HTTP $IF_CODE"
jq -e '.success == true and (.output.ifaces | index("eth0") | not) and (.output.net != "ok")' "$IF_OUT" >/dev/null \
  || log_fail "el guest tiene red: $(cat "$IF_OUT")"
log_ok "sin eth0 ($(jq -c '.output.ifaces' "$IF_OUT"))"

log_info "logs en Redis mientras el invoke sigue"
LOG_EID="log-$RANDOM"
LOG_OUT="$WORKDIR/log.json"
invoke nodejs20 "$NODE_FN" "$NODE_VER" "index.handler" \
  '{"event":{"mode":"log","ms":3000},"timeoutSec":20,"memoryMb":128,"envVars":{"POOL_BUST":"log"}}' \
  "$LOG_OUT" 25 "$LOG_EID" >/dev/null &
LOG_PID=$!
STREAMED=0
for _ in $(seq 1 40); do
  if docker exec nimbus-redis redis-cli XRANGE "nimbus:logs:$LOG_EID" - + 2>/dev/null | grep -q 'linea-visible'; then
    if kill -0 "$LOG_PID" 2>/dev/null; then
      STREAMED=1
    fi
    break
  fi
  if ! kill -0 "$LOG_PID" 2>/dev/null; then
    break
  fi
  sleep 0.15
done
wait "$LOG_PID" || true
[[ "$STREAMED" == "1" ]] || log_fail "la línea no apareció en Redis con el POST todavía en curso"
log_ok "linea-visible llegó a nimbus:logs:$LOG_EID antes de que terminara el POST"

log_info "cpus 2 → 2 vCPU"
CPU_OUT="$WORKDIR/cpus.json"
CPU_CODE="$(invoke nodejs20 "$NODE_FN" "$NODE_VER" "index.handler" \
  '{"event":{"mode":"cpus"},"timeoutSec":40,"memoryMb":128,"cpus":2,"envVars":{"POOL_BUST":"cpus"}}' \
  "$CPU_OUT" 60 "cpus-$RANDOM")"
echo "  HTTP $CPU_CODE $(jq -c '{coldStart,output}' "$CPU_OUT")"
[[ "$CPU_CODE" == "200" ]] || log_fail "cpus HTTP $CPU_CODE"
jq -e '.success == true and .output.cpus == 2' "$CPU_OUT" >/dev/null \
  || log_fail "vCPU: $(cat "$CPU_OUT")"
log_ok "os.cpus().length == 2"

log_info "memoryMb 64 (el mínimo de la plataforma; Firecracker puede rechazarlo)"
M64_OUT="$WORKDIR/mem64.json"
M64_CODE="$(invoke nodejs20 "$NODE_FN" "$NODE_VER" "index.handler" \
  '{"event":{"name":"Ada"},"timeoutSec":30,"memoryMb":64,"envVars":{"POOL_BUST":"m64"}}' \
  "$M64_OUT" 40 "m64-$RANDOM")" || true
echo "  HTTP $M64_CODE $(jq -c '{success,error,exitCode}' "$M64_OUT" 2>/dev/null || true)"
if jq -e '.success == true' "$M64_OUT" >/dev/null 2>&1; then
  log_ok "64 MiB arrancó"
else
  log_ok "64 MiB no arrancó: $(jq -r '.error // empty' "$M64_OUT")"
fi

log_info "métrica isolation"
METRICS="$(curl -sf "$COMPUTE_URL/metrics")"
echo "$METRICS" | grep -F 'isolation="firecracker"' >/dev/null \
  || log_fail "falta isolation=firecracker en /metrics"
log_ok "nimbus_invocations_total lleva isolation=firecracker"

log_info "one-shot NIMBUS_WARM=0"
CHANGED=1
NIMBUS_WARM=0 NIMBUS_FC_JAILER=0 "${COMPOSE[@]}" up -d --no-deps --force-recreate --no-build compute-plane
wait_url "$COMPUTE_URL/health" "compute one-shot"
[[ "$(curl -sf "$COMPUTE_URL/health" | jq -r '.isolation')" == "firecracker" ]] || log_fail "one-shot perdió firecracker"
ONCE_OUT="$WORKDIR/once.json"
ONCE_CODE="$(invoke nodejs20 "$NODE_FN" "$NODE_VER" "index.handler" \
  '{"event":{"name":"Ada"},"timeoutSec":40,"memoryMb":128}' \
  "$ONCE_OUT" 60 "once-$RANDOM")"
echo "  HTTP $ONCE_CODE $(jq -c '{success,coldStart,initMs,durationMs,output}' "$ONCE_OUT")"
[[ "$ONCE_CODE" == "200" ]] || log_fail "one-shot HTTP $ONCE_CODE"
jq -e '.success == true and .coldStart == true and (.initMs > 0) and .output.uid == 1000' "$ONCE_OUT" >/dev/null \
  || log_fail "one-shot: $(cat "$ONCE_OUT")"
log_ok "one-shot initMs=$(jq -r '.initMs' "$ONCE_OUT") durationMs=$(jq -r '.durationMs' "$ONCE_OUT") uid=1000"

log_info "jailer (NIMBUS_FC_JAILER=1). Si el cgroup no delega, el error queda y se vuelve a 0."
NIMBUS_WARM=1 NIMBUS_FC_JAILER=1 "${COMPOSE[@]}" up -d --no-deps --force-recreate --no-build compute-plane
wait_url "$COMPUTE_URL/health" "compute jailer"
JAIL_OUT="$WORKDIR/jail.json"
JAIL_CODE="$(invoke nodejs20 "$NODE_FN" "$NODE_VER" "index.handler" \
  '{"event":{"name":"Ada"},"timeoutSec":40,"memoryMb":128,"envVars":{"POOL_BUST":"jail"}}' \
  "$JAIL_OUT" 60 "jail-$RANDOM")" || true
echo "  HTTP $JAIL_CODE $(jq -c '{success,error,coldStart,initMs,durationMs}' "$JAIL_OUT" 2>/dev/null || echo '{}')"
if jq -e '.success == true' "$JAIL_OUT" >/dev/null 2>&1; then
  log_ok "jailer arrancó initMs=$(jq -r '.initMs' "$JAIL_OUT") durationMs=$(jq -r '.durationMs' "$JAIL_OUT")"
else
  log_info "jailer no arrancó: $(jq -r '.error // empty' "$JAIL_OUT")"
fi

NIMBUS_WARM=1 NIMBUS_FC_JAILER=0 "${COMPOSE[@]}" up -d --no-deps --force-recreate --no-build compute-plane
wait_url "$COMPUTE_URL/health" "compute restaurado"
CHANGED=0
[[ "$(curl -sf "$COMPUTE_URL/health" | jq -r '.isolation')" == "firecracker" ]] || log_fail "el restore no quedó en firecracker"
log_ok "compute plane otra vez con jailer apagado y warm"

if [[ -f /tmp/nimbus-bench-docker.tsv ]]; then
  log_info "comparación (mediana ms)"
  echo "runtime docker_cold docker_init docker_warm fc_cold fc_init fc_warm"
  while read -r name c i w; do
    docker_line="$(awk -v n="$name" '$1==n {print $2, $3, $4}' /tmp/nimbus-bench-docker.tsv)"
    echo "$name $docker_line $c $i $w"
  done < "$BENCH_FILE"
fi

log_ok "fc-check completo"
