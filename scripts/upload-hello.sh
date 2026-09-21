#!/usr/bin/env bash
# Nimbus Functions — Empaqueta y sube el ejemplo "hello" a MinIO
#
# Uso: ./scripts/upload-hello.sh [version]
#
# Si no se especifica version, usa timestamp

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

VERSION="${1:-$(date +%Y%m%d-%H%M%S)}"

echo "[upload-hello] Empaquetando hello..."
"$SCRIPT_DIR/pack-hello.sh"

echo ""
echo "[upload-hello] Subiendo a MinIO..."
"$SCRIPT_DIR/upload-artifact.sh" hello "$SCRIPT_DIR/../dist/hello.zip" "$VERSION"
