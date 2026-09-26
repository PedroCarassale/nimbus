#!/usr/bin/env bash
# Nimbus Functions — Demo MVP: Hello World end-to-end
#
# Uso: ./scripts/hello-demo.sh
#
# Este script demuestra el flujo completo del MVP:
# 1. Crea función via API
# 2. Empaqueta y despliega código
# 3. Invoca la función
# 4. Muestra resultado y hint de logs SSE
#
# Requiere: stack corriendo (make up)

set -euo pipefail

API_URL="${API_URL:-http://localhost:3000}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"

# Colores
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
RED='\033[0;31m'
BOLD='\033[1m'
NC='\033[0m'

log_step() { echo -e "\n${YELLOW}▸${NC} $1"; }
log_ok() { echo -e "  ${GREEN}✓${NC} $1"; }
log_info() { echo -e "  ${CYAN}ℹ${NC} $1"; }
log_fail() { echo -e "  ${RED}✗${NC} $1"; exit 1; }

# Verificar dependencias
command -v curl >/dev/null 2>&1 || log_fail "curl requerido"
command -v jq >/dev/null 2>&1 || log_fail "jq requerido"
command -v zip >/dev/null 2>&1 || log_fail "zip requerido"

# Verificar que el stack esté corriendo
log_step "Verificando stack..."
if ! curl -sf "$API_URL/health" >/dev/null 2>&1; then
  log_fail "El stack no está corriendo. Ejecuta: make up"
fi
log_ok "Stack healthy"

# 1. Crear función
log_step "Creando función 'hello'..."

CREATE_RESPONSE=$(curl -sf -X POST "$API_URL/functions" \
  -H "Content-Type: application/json" \
  -d '{"name": "hello"}' 2>&1) || {
    # Si ya existe, intentar obtener la función existente
    EXISTING=$(curl -sf "$API_URL/functions" | jq -r '.[] | select(.name=="hello") | .id')
    if [ -n "$EXISTING" ]; then
      FN_ID="$EXISTING"
      log_ok "Función existente encontrada: $FN_ID"
    else
      log_fail "Error creando función: $CREATE_RESPONSE"
    fi
}

if [ -z "${FN_ID:-}" ]; then
  FN_ID=$(echo "$CREATE_RESPONSE" | jq -r '.id')
  if [ "$FN_ID" == "null" ] || [ -z "$FN_ID" ]; then
    log_fail "Error creando función: $CREATE_RESPONSE"
  fi
  log_ok "Función creada: $FN_ID"
fi

# 2. Empaquetar código
log_step "Empaquetando código..."
mkdir -p "$PROJECT_ROOT/dist"
cd "$PROJECT_ROOT/examples/hello"
zip -q -r "$PROJECT_ROOT/dist/hello.zip" .
cd "$PROJECT_ROOT"
log_ok "Creado dist/hello.zip"

# 3. Desplegar
log_step "Desplegando a Nimbus..."
DEPLOY_RESPONSE=$(curl -sf -X POST "$API_URL/functions/$FN_ID/deploy" \
  -F "file=@dist/hello.zip")

VERSION=$(echo "$DEPLOY_RESPONSE" | jq -r '.version.version')
if [ "$VERSION" == "null" ] || [ -z "$VERSION" ]; then
  log_fail "Error desplegando: $DEPLOY_RESPONSE"
fi
log_ok "Versión desplegada: $VERSION"

# 4. Invocar
log_step "Invocando función..."
INVOKE_RESPONSE=$(curl -sf -X POST "$API_URL/functions/$FN_ID/invoke" \
  -H "Content-Type: application/json" \
  -d '{"event": {"name": "Nimbus MVP"}}')

STATUS=$(echo "$INVOKE_RESPONSE" | jq -r '.status')
INVOCATION_ID=$(echo "$INVOKE_RESPONSE" | jq -r '.invocationId')
EXECUTION_ID=$(echo "$INVOKE_RESPONSE" | jq -r '.executionId')
DURATION=$(echo "$INVOKE_RESPONSE" | jq -r '.durationMs')
OUTPUT=$(echo "$INVOKE_RESPONSE" | jq -r '.output')

if [ "$STATUS" != "SUCCESS" ]; then
  log_fail "Invocación falló: $INVOKE_RESPONSE"
fi

log_ok "Invocación exitosa (${DURATION}ms)"

# 5. Mostrar resultado
echo ""
echo -e "${BOLD}═══════════════════════════════════════════════════════════════${NC}"
echo -e "${GREEN}${BOLD}                    ¡Función ejecutada con éxito!${NC}"
echo -e "${BOLD}═══════════════════════════════════════════════════════════════${NC}"
echo ""
echo -e "${CYAN}Resultado:${NC}"
echo "$OUTPUT" | jq '.'
echo ""
echo -e "${CYAN}Metadata:${NC}"
echo "  • Function ID:   $FN_ID"
echo "  • Version:       $VERSION"
echo "  • Invocation ID: $INVOCATION_ID"
echo "  • Execution ID:  $EXECUTION_ID"
echo "  • Duración:      ${DURATION}ms"
echo ""
echo -e "${BOLD}═══════════════════════════════════════════════════════════════${NC}"
echo ""

# 6. Hint de logs SSE
echo -e "${YELLOW}💡 Ver logs en tiempo real:${NC}"
echo ""
echo "  curl -N $API_URL/invocations/$INVOCATION_ID/logs"
echo ""
echo -e "${YELLOW}💡 Invocar nuevamente:${NC}"
echo ""
echo "  curl -X POST $API_URL/functions/$FN_ID/invoke \\"
echo "    -H 'Content-Type: application/json' \\"
echo "    -d '{\"event\": {\"name\": \"Tu Nombre\"}}'"
echo ""
