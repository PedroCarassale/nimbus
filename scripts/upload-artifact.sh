#!/usr/bin/env bash
# Nimbus Functions — Sube un artifact (zip) a MinIO
#
# Uso: ./scripts/upload-artifact.sh <function-id> <path/to/fn.zip> [version]
#
# Sube el zip a: functions/{fnId}/versions/{version}/code.zip
# Si no se especifica version, usa timestamp (YYYYMMDD-HHMMSS)
#
# Variables de entorno:
#   AWS_ACCESS_KEY_ID     - Access key (default: minioadmin)
#   AWS_SECRET_ACCESS_KEY - Secret key (default: minioadmin)
#   AWS_ENDPOINT_URL      - Endpoint MinIO (default: http://localhost:9000)
#   MINIO_BUCKET          - Bucket name (default: nimbus-artifacts)

set -euo pipefail

# Defaults
AWS_ACCESS_KEY_ID="${AWS_ACCESS_KEY_ID:-minioadmin}"
AWS_SECRET_ACCESS_KEY="${AWS_SECRET_ACCESS_KEY:-minioadmin}"
AWS_ENDPOINT_URL="${AWS_ENDPOINT_URL:-http://localhost:9000}"
MINIO_BUCKET="${MINIO_BUCKET:-nimbus-artifacts}"

export AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY

usage() {
  echo "Uso: $0 <function-id> <path/to/fn.zip> [version]" >&2
  echo "" >&2
  echo "Ejemplos:" >&2
  echo "  $0 hello dist/hello.zip" >&2
  echo "  $0 hello dist/hello.zip v1.0.0" >&2
  exit 2
}

if [[ $# -lt 2 ]]; then
  usage
fi

FN_ID="$1"
ZIP_PATH="$2"
VERSION="${3:-$(date +%Y%m%d-%H%M%S)}"

if [[ ! -f "$ZIP_PATH" ]]; then
  echo "Error: No se encuentra el archivo: $ZIP_PATH" >&2
  exit 1
fi

OBJECT_KEY="functions/${FN_ID}/versions/${VERSION}/code.zip"
S3_URI="s3://${MINIO_BUCKET}/${OBJECT_KEY}"

echo "[upload] Subiendo $ZIP_PATH → $S3_URI"

if command -v aws &>/dev/null; then
  aws --endpoint-url "$AWS_ENDPOINT_URL" s3 cp "$ZIP_PATH" "$S3_URI"
elif command -v mc &>/dev/null; then
  mc alias set nimbus "$AWS_ENDPOINT_URL" "$AWS_ACCESS_KEY_ID" "$AWS_SECRET_ACCESS_KEY" >/dev/null 2>&1
  mc cp "$ZIP_PATH" "nimbus/${MINIO_BUCKET}/${OBJECT_KEY}"
else
  echo "Error: Se requiere 'aws' CLI o 'mc' (MinIO Client)" >&2
  exit 1
fi

echo ""
echo "✓ Artifact subido exitosamente"
echo "  Function: $FN_ID"
echo "  Version:  $VERSION"
echo "  Key:      $OBJECT_KEY"
