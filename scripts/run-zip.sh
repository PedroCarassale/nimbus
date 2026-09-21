#!/usr/bin/env bash
# Nimbus Functions — Ejecuta una función desde un zip
#
# Uso: ./scripts/run-zip.sh path/to/fn.zip [event.json]
#
# Variables de entorno opcionales:
#   NIMBUS_MEMORY      - Límite de memoria (default: 128m)
#   NIMBUS_TIMEOUT_SEC - Timeout en segundos (default: 5)
#   NIMBUS_HANDLER     - Nombre de la función handler (default: handler)
#   NIMBUS_IMAGE       - Nombre de la imagen Docker (default: nimbus-node)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# Valores por defecto
NIMBUS_MEMORY="${NIMBUS_MEMORY:-128m}"
NIMBUS_TIMEOUT_SEC="${NIMBUS_TIMEOUT_SEC:-5}"
NIMBUS_HANDLER="${NIMBUS_HANDLER:-handler}"
NIMBUS_IMAGE="${NIMBUS_IMAGE:-nimbus-node}"

# Códigos de salida
EXIT_SUCCESS=0
EXIT_TIMEOUT=124
EXIT_USAGE=2
EXIT_BUILD_FAILED=3
EXIT_UNZIP_FAILED=4

usage() {
  echo "Uso: $0 <path/to/fn.zip> [event.json]" >&2
  echo "" >&2
  echo "Variables de entorno:" >&2
  echo "  NIMBUS_MEMORY       Límite de memoria (default: 128m)" >&2
  echo "  NIMBUS_TIMEOUT_SEC  Timeout en segundos (default: 5)" >&2
  echo "  NIMBUS_HANDLER      Nombre del handler (default: handler)" >&2
  exit $EXIT_USAGE
}

cleanup() {
  if [[ -n "${WORK_DIR:-}" && -d "$WORK_DIR" ]]; then
    rm -rf "$WORK_DIR"
  fi
}

trap cleanup EXIT

# Validar argumentos
if [[ $# -lt 1 ]]; then
  usage
fi

ZIP_PATH="$1"
EVENT_PATH="${2:-}"

if [[ ! -f "$ZIP_PATH" ]]; then
  echo "Error: No se encuentra el archivo zip: $ZIP_PATH" >&2
  exit $EXIT_USAGE
fi

# Construir imagen si no existe
if ! docker image inspect "$NIMBUS_IMAGE" >/dev/null 2>&1; then
  echo "[nimbus] Construyendo imagen $NIMBUS_IMAGE..." >&2
  if ! docker build -t "$NIMBUS_IMAGE" "$PROJECT_ROOT/runtime-node"; then
    echo "Error: Falló la construcción de la imagen" >&2
    exit $EXIT_BUILD_FAILED
  fi
fi

# Crear directorio temporal y extraer zip
WORK_DIR="$(mktemp -d)"
echo "[nimbus] Extrayendo zip a $WORK_DIR..." >&2

if ! unzip -q "$ZIP_PATH" -d "$WORK_DIR"; then
  echo "Error: Falló la extracción del zip" >&2
  exit $EXIT_UNZIP_FAILED
fi

# Preparar volúmenes y variables
DOCKER_ARGS=(
  --rm
  --memory="$NIMBUS_MEMORY"
  --pids-limit=256
  --network=none
  --read-only
  -v "$WORK_DIR:/var/task:ro"
  -e "NIMBUS_HANDLER_FUNCTION=$NIMBUS_HANDLER"
)

# Si hay evento, copiarlo al workdir y montar
if [[ -n "$EVENT_PATH" && -f "$EVENT_PATH" ]]; then
  cp "$EVENT_PATH" "$WORK_DIR/event.json"
  DOCKER_ARGS+=(-e "NIMBUS_EVENT_PATH=/var/task/event.json")
elif [[ -f "$WORK_DIR/event.json" ]]; then
  DOCKER_ARGS+=(-e "NIMBUS_EVENT_PATH=/var/task/event.json")
fi

# Ejecutar con timeout
echo "[nimbus] Ejecutando función (timeout: ${NIMBUS_TIMEOUT_SEC}s, memoria: ${NIMBUS_MEMORY})..." >&2

set +e
timeout "${NIMBUS_TIMEOUT_SEC}s" docker run "${DOCKER_ARGS[@]}" "$NIMBUS_IMAGE"
EXIT_CODE=$?
set -e

if [[ $EXIT_CODE -eq 124 ]]; then
  echo "" >&2
  echo "Error: La función excedió el timeout de ${NIMBUS_TIMEOUT_SEC}s" >&2
  exit $EXIT_TIMEOUT
fi

exit $EXIT_CODE
