#!/usr/bin/env bash
# Nimbus Functions — Demo completa: MinIO → upload → run
#
# Uso: ./scripts/run-hello-from-minio.sh [event.json]
#
# Este script:
# 1. Levanta MinIO si no está corriendo
# 2. Empaqueta y sube hello a MinIO
# 3. Ejecuta la función desde MinIO
#
# Demuestra el flujo completo de Slice 2.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

EVENT_PATH="${1:-$PROJECT_ROOT/examples/hello/event.json}"
VERSION="demo-$(date +%Y%m%d-%H%M%S)"

echo "=========================================="
echo " Nimbus Functions — Demo MinIO (Slice 2)"
echo "=========================================="
echo ""

# Verificar si MinIO está corriendo
if ! curl -s http://localhost:9000/minio/health/live >/dev/null 2>&1; then
  echo "[demo] MinIO no está corriendo. Iniciando con docker compose..."
  docker compose -f "$PROJECT_ROOT/docker-compose.yml" up -d
  
  echo "[demo] Esperando que MinIO esté listo..."
  for i in {1..30}; do
    if curl -s http://localhost:9000/minio/health/live >/dev/null 2>&1; then
      echo "[demo] MinIO listo"
      break
    fi
    sleep 1
  done
  
  # Dar tiempo extra para que minio-init cree el bucket
  sleep 2
fi

echo ""
echo "[1/3] Empaquetando y subiendo hello a MinIO..."
echo "---------------------------------------------"
"$SCRIPT_DIR/upload-hello.sh" "$VERSION"

echo ""
echo "[2/3] Descargando y ejecutando desde MinIO..."
echo "----------------------------------------------"
"$SCRIPT_DIR/run-artifact.sh" hello "$VERSION" "$EVENT_PATH"

echo ""
echo "[3/3] Demo completada"
echo "====================="
echo ""
echo "Puedes ver el bucket en: http://localhost:9001"
echo "  Usuario: minioadmin"
echo "  Password: minioadmin"
