#!/usr/bin/env bash
# Compatibilidad con el nombre viejo. Crea el bucket y las colas en LocalStack
# (o en el endpoint que indiquen S3_ENDPOINT / AWS_ENDPOINT_URL / MINIO_ENDPOINT).

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export AWS_ENDPOINT_URL="${AWS_ENDPOINT_URL:-${S3_ENDPOINT:-${MINIO_ENDPOINT:-http://localhost:4566}}}"
export AWS_ACCESS_KEY_ID="${AWS_ACCESS_KEY_ID:-${MINIO_ROOT_USER:-${MINIO_ACCESS_KEY:-test}}}"
export AWS_SECRET_ACCESS_KEY="${AWS_SECRET_ACCESS_KEY:-${MINIO_ROOT_PASSWORD:-${MINIO_SECRET_KEY:-test}}}"
export S3_BUCKET="${S3_BUCKET:-${MINIO_BUCKET:-nimbus-artifacts}}"
exec "$SCRIPT_DIR/localstack-init.sh"
