#!/usr/bin/env bash
# Prepare the host route before network policy and guest provisioning.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib/engine-context.sh
. "$SCRIPT_DIR/lib/engine-context.sh"
subyard_require_engine_context
# shellcheck source=scripts/lib/preview-proxy.sh
. "$SCRIPT_DIR/lib/preview-proxy.sh"

[ "$#" -eq 1 ] || exit 2
PROJ=(--project "$INCUS_PROJECT")
subyard_preview_route "$1"
