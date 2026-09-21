#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(dirname "$SCRIPT_DIR")"

EVENT_FILE=$(mktemp)
trap 'rm -f "$EVENT_FILE"' EXIT

echo '{"name":"mundo","ts":'"$(date +%s)"'}' > "$EVENT_FILE"

if ! docker image inspect nimbus-node >/dev/null 2>&1; then
  echo ">> Building nimbus-node image..."
  docker build -t nimbus-node "$REPO_ROOT/runtime-node"
fi

echo ">> Running hello function..."
timeout 5s docker run --rm \
  -v "$REPO_ROOT/examples/hello:/var/task:ro" \
  -v "$EVENT_FILE:/tmp/event.json:ro" \
  -e NIMBUS_HANDLER_PATH=/var/task/index.js \
  nimbus-node
