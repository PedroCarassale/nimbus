#!/usr/bin/env bash
# Nimbus Functions — Demo completa: S3 (LocalStack) → upload → run
#
# Uso: ./scripts/run-hello-from-minio.sh [event.json]
#
# Este script:
# 1. Levanta LocalStack si no está corriendo
# 2. Empaqueta y sube hello al bucket
# 3. Ejecuta la función desde S3

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

EVENT_PATH="${1:-$PROJECT_ROOT/examples/hello/event.json}"
VERSION="demo-$(date +%Y%m%d-%H%M%S)"

echo "=========================================="
echo " Nimbus Functions — Demo S3 / LocalStack"
echo "=========================================="
echo ""

if ! curl -sf http://localhost:4566/_localstack/health >/dev/null 2>&1; then
  echo "[demo] LocalStack no está corriendo. Iniciando localstack y el init..."
  docker compose -f "$PROJECT_ROOT/docker-compose.yml" up -d localstack localstack-init

  echo "[demo] Esperando LocalStack..."
  for i in {1..60}; do
    if curl -sf http://localhost:4566/_localstack/health >/dev/null 2>&1; then
      echo "[demo] LocalStack listo"
      break
    fi
    sleep 1
  done
  sleep 2
fi

echo ""
echo "[1/3] Empaquetando y subiendo hello a S3..."
echo "---------------------------------------------"
"$SCRIPT_DIR/upload-hello.sh" "$VERSION"

echo ""
echo "[2/3] Descargando y ejecutando desde S3..."
echo "----------------------------------------------"
"$SCRIPT_DIR/run-artifact.sh" hello "$VERSION" "$EVENT_PATH"

echo ""
echo "[3/3] Demo completada"
echo "====================="
echo ""
echo "Bucket: nimbus-artifacts en http://localhost:4566"
echo "  Access key: test"
echo "  Secret key: test"
