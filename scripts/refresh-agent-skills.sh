#!/usr/bin/env bash
# shellcheck disable=SC2016
# Static skill delivery only: no service, toolchain or emulator operations.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib/engine-context.sh
. "$SCRIPT_DIR/lib/engine-context.sh"
subyard_require_engine_context
mode="${1:-apply}"
case "$mode" in apply|check) ;; *) exit 2 ;; esac
root="$(cd "$SCRIPT_DIR/.." && pwd)"
# Ship only package-local skill sources. The archive is ephemeral; installed skills
# and their receipts have no dependency on the host checkout or temporary paths.
python3 -B "$root/config/skills/package.py" archive "$root" | incus exec "$YARD_INSTANCE_NAME" --project "$INCUS_PROJECT" \
  --env DEV_USER="${DEV_USER:-dev}" --env ENVIRONMENT_PROFILES="${ENVIRONMENT_PROFILES:-}" \
  --env ALLOWS_CODING_TOOLS="${ALLOWS_CODING_TOOLS:-true}" -- bash -eu -c "$(cat "$root/config/skills/refresh.sh")" -- "$mode"
