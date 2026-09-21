#!/usr/bin/env bash
# Nimbus Functions — Inicializa MinIO y crea el bucket de artifacts
#
# Uso: ./scripts/minio-init.sh
#
# Este script es alternativo al servicio minio-init del docker-compose.
# Útil si necesitas reinicializar el bucket manualmente.
#
# Variables de entorno:
#   MINIO_ENDPOINT    - URL de MinIO (default: http://localhost:9000)
#   MINIO_ROOT_USER   - Usuario root (default: minioadmin)
#   MINIO_ROOT_PASSWORD - Password root (default: minioadmin)
#   MINIO_BUCKET      - Nombre del bucket (default: nimbus-artifacts)

set -euo pipefail

MINIO_ENDPOINT="${MINIO_ENDPOINT:-http://localhost:9000}"
MINIO_ROOT_USER="${MINIO_ROOT_USER:-minioadmin}"
MINIO_ROOT_PASSWORD="${MINIO_ROOT_PASSWORD:-minioadmin}"
MINIO_BUCKET="${MINIO_BUCKET:-nimbus-artifacts}"

echo "[minio-init] Configurando alias..."

if command -v mc &>/dev/null; then
  mc alias set nimbus "$MINIO_ENDPOINT" "$MINIO_ROOT_USER" "$MINIO_ROOT_PASSWORD"
  mc mb "nimbus/$MINIO_BUCKET" --ignore-existing
  mc anonymous set download "nimbus/$MINIO_BUCKET"
  echo "[minio-init] Bucket $MINIO_BUCKET listo"
elif command -v aws &>/dev/null; then
  export AWS_ACCESS_KEY_ID="$MINIO_ROOT_USER"
  export AWS_SECRET_ACCESS_KEY="$MINIO_ROOT_PASSWORD"
  aws --endpoint-url "$MINIO_ENDPOINT" s3 mb "s3://$MINIO_BUCKET" 2>/dev/null || true
  echo "[minio-init] Bucket $MINIO_BUCKET listo (via aws cli)"
else
  echo "Error: Se requiere 'mc' (MinIO Client) o 'aws' CLI" >&2
  exit 1
fi
