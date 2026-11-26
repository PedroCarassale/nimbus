#!/usr/bin/env bash
# Nimbus Functions — Descarga y ejecuta una función desde S3 (LocalStack o AWS)
#
# Uso: ./scripts/run-artifact.sh <function-id> <version|latest> [event.json]
#
# Descarga el artifact y lo ejecuta con run-zip.sh
# Si version es "latest", busca la versión más reciente.
#
# Variables de entorno:
#   AWS_ACCESS_KEY_ID     - Access key (default: test)
#   AWS_SECRET_ACCESS_KEY - Secret key (default: test)
#   AWS_ENDPOINT_URL      - Endpoint S3 (default: http://localhost:4566). Vacío = AWS real.
#   S3_BUCKET             - Bucket (default: nimbus-artifacts). MINIO_BUCKET sigue como alias.
#   NIMBUS_MEMORY         - Límite de memoria (pasado a run-zip.sh)
#   NIMBUS_TIMEOUT_SEC    - Timeout en segundos (pasado a run-zip.sh)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Defaults
AWS_ACCESS_KEY_ID="${AWS_ACCESS_KEY_ID:-test}"
AWS_SECRET_ACCESS_KEY="${AWS_SECRET_ACCESS_KEY:-test}"
AWS_ENDPOINT_URL="${AWS_ENDPOINT_URL:-${S3_ENDPOINT:-http://localhost:4566}}"
MINIO_BUCKET="${S3_BUCKET:-${MINIO_BUCKET:-nimbus-artifacts}}"

export AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY

usage() {
  echo "Uso: $0 <function-id> <version|latest> [event.json]" >&2
  echo "" >&2
  echo "Ejemplos:" >&2
  echo "  $0 hello latest" >&2
  echo "  $0 hello 20240115-120000" >&2
  echo "  $0 hello latest mi-evento.json" >&2
  exit 2
}

if [[ $# -lt 2 ]]; then
  usage
fi

FN_ID="$1"
VERSION="$2"
EVENT_PATH="${3:-}"

# Crear directorio temporal para descarga
TEMP_DIR="$(mktemp -d)"
cleanup() {
  rm -rf "$TEMP_DIR"
}
trap cleanup EXIT

# Si version es "latest", buscar la más reciente
if [[ "$VERSION" == "latest" ]]; then
  echo "[run-artifact] Buscando última versión de $FN_ID..."
  
  if command -v aws &>/dev/null; then
    # Listar versiones y obtener la más reciente (ordenadas por nombre desc)
    VERSIONS=$(aws --endpoint-url "$AWS_ENDPOINT_URL" s3 ls "s3://${MINIO_BUCKET}/functions/${FN_ID}/versions/" 2>/dev/null | awk '{print $2}' | tr -d '/' | sort -r)
  elif command -v mc &>/dev/null; then
    mc alias set nimbus "$AWS_ENDPOINT_URL" "$AWS_ACCESS_KEY_ID" "$AWS_SECRET_ACCESS_KEY" >/dev/null 2>&1
    VERSIONS=$(mc ls "nimbus/${MINIO_BUCKET}/functions/${FN_ID}/versions/" 2>/dev/null | awk '{print $NF}' | tr -d '/' | sort -r)
  else
    echo "Error: Se requiere 'aws' CLI o 'mc'" >&2
    exit 1
  fi
  
  VERSION=$(echo "$VERSIONS" | head -n1)
  
  if [[ -z "$VERSION" ]]; then
    echo "Error: No se encontraron versiones para la función '$FN_ID'" >&2
    exit 1
  fi
  
  echo "[run-artifact] Usando versión: $VERSION"
fi

OBJECT_KEY="functions/${FN_ID}/versions/${VERSION}/code.zip"
S3_URI="s3://${MINIO_BUCKET}/${OBJECT_KEY}"
LOCAL_ZIP="$TEMP_DIR/code.zip"

echo "[run-artifact] Descargando $S3_URI..."

if command -v aws &>/dev/null; then
  aws --endpoint-url "$AWS_ENDPOINT_URL" s3 cp "$S3_URI" "$LOCAL_ZIP"
elif command -v mc &>/dev/null; then
  mc alias set nimbus "$AWS_ENDPOINT_URL" "$AWS_ACCESS_KEY_ID" "$AWS_SECRET_ACCESS_KEY" >/dev/null 2>&1
  mc cp "nimbus/${MINIO_BUCKET}/${OBJECT_KEY}" "$LOCAL_ZIP"
else
  echo "Error: Se requiere 'aws' CLI o 'mc'" >&2
  exit 1
fi

echo "[run-artifact] Ejecutando función..."
echo ""

# Ejecutar con run-zip.sh, pasando el evento si existe
if [[ -n "$EVENT_PATH" ]]; then
  "$SCRIPT_DIR/run-zip.sh" "$LOCAL_ZIP" "$EVENT_PATH"
else
  "$SCRIPT_DIR/run-zip.sh" "$LOCAL_ZIP"
fi
