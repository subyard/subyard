#!/usr/bin/env bash
set -euo pipefail
RESOURCE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT_DIR="$(cd "$RESOURCE_DIR/../../../../.." && pwd)/scripts"
# shellcheck source=scripts/lib/engine-context.sh
. "$SCRIPT_DIR/lib/engine-context.sh"
subyard_require_engine_context
exec python3 "$RESOURCE_DIR/../vpn/handler.py" --admin "$@"
