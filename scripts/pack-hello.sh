#!/usr/bin/env bash
# Nimbus Functions — Empaqueta el ejemplo "hello" en un zip
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

HELLO_DIR="$PROJECT_ROOT/examples/hello"
DIST_DIR="${DIST_DIR:-$PROJECT_ROOT/dist}"
OUTPUT_FILE="$DIST_DIR/hello.zip"

if [[ ! -d "$HELLO_DIR" ]]; then
  echo "Error: No se encuentra el directorio $HELLO_DIR" >&2
  exit 1
fi

mkdir -p "$DIST_DIR"

# Eliminar zip anterior si existe
rm -f "$OUTPUT_FILE"

# Crear zip desde el directorio hello
(cd "$HELLO_DIR" && zip -r "$OUTPUT_FILE" .)

echo "Zip creado: $OUTPUT_FILE"
