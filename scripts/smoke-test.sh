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
# - docker compose up (postgres + redis + minio + compute-plane + control-plane)
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
DURATION=$(echo "$INVOKE_RESPONSE" | jq -r '.durationMs')

echo ""
echo "=== Resultado de Invocación ==="
echo "$INVOKE_RESPONSE" | jq '.'
echo ""

if [ "$STATUS" == "SUCCESS" ]; then
  log_ok "Invocación exitosa (${DURATION}ms, requestId: $REQUEST_ID)"
  echo ""
  echo -e "${GREEN}=== SMOKE TEST PASSED ===${NC}"
  exit 0
else
  log_fail "Invocación falló con status: $STATUS"
fi
