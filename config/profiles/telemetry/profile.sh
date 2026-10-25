#!/usr/bin/env bash
set -euo pipefail
profile="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "$profile/../../.." && pwd)"
# shellcheck source=scripts/lib/engine-context.sh
. "$root/scripts/lib/engine-context.sh"
subyard_require_engine_context
exec python3 -B "$profile/owner.py" "$@"
