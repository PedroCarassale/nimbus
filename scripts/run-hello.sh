#!/usr/bin/env bash
# Nimbus Functions — Demo rápida: empaqueta y ejecuta el ejemplo "hello"
#
# Uso: ./scripts/run-hello.sh [event.json]
#
# Si no se proporciona evento, usa examples/hello/event.json

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

EVENT_PATH="${1:-$PROJECT_ROOT/examples/hello/event.json}"

# Empaquetar hello
"$SCRIPT_DIR/pack-hello.sh"

# Ejecutar desde zip
"$SCRIPT_DIR/run-zip.sh" "$PROJECT_ROOT/dist/hello.zip" "$EVENT_PATH"
