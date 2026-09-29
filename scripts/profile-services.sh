#!/usr/bin/env bash
# Dispatch approved owner-service work to shipped profile handlers.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib/engine-context.sh
. "$SCRIPT_DIR/lib/engine-context.sh"
subyard_require_engine_context
root="$(cd "$SCRIPT_DIR/.." && pwd)"
engine="${SUBYARD_DISPATCHER_PATH:-$root/bin/yard}"
[ -n "$engine" ] && [ -x "$engine" ] || { printf 'profile services: native engine is missing\n' >&2; exit 1; }
exec "$engine" _profile-services "$root" "$@"
