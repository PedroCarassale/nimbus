#!/usr/bin/env bash
# Nimbus Functions — Smoke Test del Control Plane (Slice 3-4)
#
# Uso: ./scripts/smoke-test.sh
#
# Este script realiza el flujo completo:
# 1. Crea una función via API
# 2. Despliega un zip (hello)
# 3. Invoca la función (Nest → Go compute plane)
#
# Requiere:
# - docker compose up (postgres + redis + localstack + compute-plane + control-plane)
# - curl y jq instalados

set -euo pipefail

API_URL="${API_URL:-http://localhost:3000}"
COMPUTE_URL="${COMPUTE_URL:-http://localhost:8080}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Colores para output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

log_info() { echo -e "${YELLOW}[smoke-test]${NC} $1"; }
log_ok() { echo -e "${GREEN}[✓]${NC} $1"; }
log_fail() { echo -e "${RED}[✗]${NC} $1"; exit 1; }

# Verificar dependencias
command -v curl >/dev/null 2>&1 || log_fail "curl requerido"
command -v jq >/dev/null 2>&1 || log_fail "jq requerido"
command -v zip >/dev/null 2>&1 || log_fail "zip requerido"

# Esperar a que el compute-plane esté listo
log_info "Esperando compute-plane en $COMPUTE_URL..."
for i in {1..30}; do
  if curl -sf "$COMPUTE_URL/health" >/dev/null 2>&1; then
    log_ok "Compute plane listo"
    break
  fi
  if [ $i -eq 30 ]; then
    log_fail "Compute plane no responde después de 30s"
  fi
  sleep 1
done

# Esperar a que el control-plane esté listo
log_info "Esperando control-plane en $API_URL..."
for i in {1..30}; do
  if curl -sf "$API_URL/health" >/dev/null 2>&1; then
    log_ok "Control plane listo"
    break
  fi
  if [ $i -eq 30 ]; then
    log_fail "Control plane no responde después de 30s"
  fi
  sleep 1
done

# 1. Crear función
FN_NAME="smoke-test-$(date +%s)"
log_info "Creando función: $FN_NAME"

CREATE_RESPONSE=$(curl -sf -X POST "$API_URL/functions" \
  -H "Content-Type: application/json" \
  -d "{\"name\": \"$FN_NAME\"}")

FN_ID=$(echo "$CREATE_RESPONSE" | jq -r '.id')
if [ "$FN_ID" == "null" ] || [ -z "$FN_ID" ]; then
  log_fail "Error creando función: $CREATE_RESPONSE"
fi
log_ok "Función creada: $FN_ID"

# 2. Empaquetar y desplegar hello
log_info "Empaquetando ejemplo hello..."
"$SCRIPT_DIR/pack-hello.sh"

ZIP_PATH="$SCRIPT_DIR/../dist/hello.zip"
if [ ! -f "$ZIP_PATH" ]; then
  log_fail "No se encontró $ZIP_PATH"
fi

log_info "Desplegando zip..."
DEPLOY_RESPONSE=$(curl -sf -X POST "$API_URL/functions/$FN_ID/deploy" \
  -F "file=@$ZIP_PATH")

VERSION=$(echo "$DEPLOY_RESPONSE" | jq -r '.version.version')
if [ "$VERSION" == "null" ] || [ -z "$VERSION" ]; then
  log_fail "Error desplegando: $DEPLOY_RESPONSE"
fi
log_ok "Desplegado versión: $VERSION"

# 3. Invocar función
log_info "Invocando función..."
INVOKE_RESPONSE=$(curl -sf -X POST "$API_URL/functions/$FN_ID/invoke" \
  -H "Content-Type: application/json" \
  -d '{"event": {"nombre": "Smoke Test"}}')

STATUS=$(echo "$INVOKE_RESPONSE" | jq -r '.status')
REQUEST_ID=$(echo "$INVOKE_RESPONSE" | jq -r '.requestId')
INVOCATION_ID=$(echo "$INVOKE_RESPONSE" | jq -r '.invocationId')
EXECUTION_ID=$(echo "$INVOKE_RESPONSE" | jq -r '.executionId')
DURATION=$(echo "$INVOKE_RESPONSE" | jq -r '.durationMs')

echo ""
echo "=== Resultado de Invocación ==="
echo "$INVOKE_RESPONSE" | jq '.'
echo ""

if [ "$STATUS" != "SUCCESS" ]; then
  log_fail "Invocación falló con status: $STATUS — $INVOKE_RESPONSE"
fi

MESSAGE=$(echo "$INVOKE_RESPONSE" | jq -r '.output.body.message // empty')
if ! echo "$MESSAGE" | grep -q "Hola"; then
  log_fail "La salida no es el JSON del handler: $INVOKE_RESPONSE"
fi
log_ok "Invocación exitosa (${DURATION}ms, requestId: $REQUEST_ID, mensaje: $MESSAGE)"

# 4. Verificar logs SSE (snapshot y follow)
log_info "Verificando logs SSE para invocación $INVOCATION_ID..."

LOGS_RESPONSE=$(curl -sf --max-time 20 "$API_URL/invocations/$INVOCATION_ID/logs?follow=false")
if ! echo "$LOGS_RESPONSE" | grep -q "event: log"; then
  log_fail "No hay eventos de log SSE: $LOGS_RESPONSE"
fi
if ! echo "$LOGS_RESPONSE" | grep -q "event: end"; then
  log_fail "El stream SSE no cerró con event: end: $LOGS_RESPONSE"
fi
log_ok "Logs SSE (follow=false) disponibles (stream key: nimbus:logs:$EXECUTION_ID)"

log_info "Siguiendo logs con curl -N ..."
LIVE_LOGS=$(curl -sf -N --max-time 20 "$API_URL/invocations/$INVOCATION_ID/logs")
if ! echo "$LIVE_LOGS" | grep -q "event: log"; then
  log_fail "curl -N no recibió logs: $LIVE_LOGS"
fi
log_ok "Logs SSE en vivo (curl -N) OK"

# 5. Caso de error del handler
log_info "Probando un handler que falla..."
ERR_NAME="smoke-error-$(date +%s)"
ERR_DIR=$(mktemp -d)
cat > "$ERR_DIR/index.js" <<'EOF'
exports.handler = async () => {
  throw new Error("fallo de prueba");
};
EOF
(cd "$ERR_DIR" && zip -q -r "$ERR_DIR/code.zip" index.js)
ERR_CREATE=$(curl -sf -X POST "$API_URL/functions" \
  -H "Content-Type: application/json" \
  -d "{\"name\": \"$ERR_NAME\"}")
ERR_FN=$(echo "$ERR_CREATE" | jq -r '.id')
curl -sf -X POST "$API_URL/functions/$ERR_FN/deploy" -F "file=@$ERR_DIR/code.zip" >/dev/null
ERR_INVOKE=$(curl -sf -X POST "$API_URL/functions/$ERR_FN/invoke" \
  -H "Content-Type: application/json" \
  -d '{"event": {}}')
ERR_STATUS=$(echo "$ERR_INVOKE" | jq -r '.status')
ERR_TEXT=$(echo "$ERR_INVOKE" | jq -r '.error // empty')
if [ "$ERR_STATUS" != "ERROR" ]; then
  log_fail "Se esperaba ERROR y llegó $ERR_STATUS: $ERR_INVOKE"
fi
if ! echo "$ERR_TEXT" | grep -q "fallo de prueba"; then
  log_fail "El error no incluye el mensaje del handler: $ERR_INVOKE"
fi
log_ok "Handler con error devuelve ERROR"
rm -rf "$ERR_DIR"

# 6. Caso de timeout (el timeout del stack incluye el arranque del container)
log_info "Probando timeout..."
TO_NAME="smoke-timeout-$(date +%s)"
TO_DIR=$(mktemp -d)
cat > "$TO_DIR/index.js" <<'EOF'
exports.handler = async () => {
  await new Promise((resolve) => setTimeout(resolve, 30000));
  return { ok: true };
};
EOF
(cd "$TO_DIR" && zip -q -r "$TO_DIR/code.zip" index.js)
TO_CREATE=$(curl -sf -X POST "$API_URL/functions" \
  -H "Content-Type: application/json" \
  -d "{\"name\": \"$TO_NAME\"}")
TO_FN=$(echo "$TO_CREATE" | jq -r '.id')
curl -sf -X POST "$API_URL/functions/$TO_FN/deploy" -F "file=@$TO_DIR/code.zip" >/dev/null
TO_INVOKE=$(curl -sf --max-time 60 -X POST "$API_URL/functions/$TO_FN/invoke" \
  -H "Content-Type: application/json" \
  -d '{"event": {}}')
TO_STATUS=$(echo "$TO_INVOKE" | jq -r '.status')
if [ "$TO_STATUS" != "TIMEOUT" ]; then
  log_fail "Se esperaba TIMEOUT y llegó $TO_STATUS: $TO_INVOKE"
fi
log_ok "Timeout devuelve TIMEOUT"
rm -rf "$TO_DIR"

echo ""
echo -e "${GREEN}=== SMOKE TEST PASSED ===${NC}"
echo ""
echo "Para ver logs en vivo de futuras invocaciones:"
echo "  curl -N $API_URL/invocations/{invocationId}/logs"
