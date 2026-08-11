#!/usr/bin/env bash
# Physical L0-to-L1 worker invocation; Go owns validation and policy.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib/engine-context.sh
. "$SCRIPT_DIR/../lib/engine-context.sh"
subyard_require_engine_context
WORKER=/usr/local/libexec/subyard/test-vms-inner
PROJ=(--project "${INCUS_PROJECT:?}")
WORKER_ARGS=()

incus exec "${YARD_INSTANCE_NAME:?}" "${PROJ[@]}" -- test -x "$WORKER" \
  || { printf 'test-vms: engine is not installed; run yard init\n' >&2; exit 1; }
if incus exec "${YARD_INSTANCE_NAME:?}" "${PROJ[@]}" -- sh -c \
  'magic="$(od -An -tx1 -N4 "$1" | tr -d " \n")"; [ "$magic" = 7f454c46 ]' \
  _ "$WORKER"; then
  WORKER_ARGS=(_test-vms-worker)
fi
exec incus exec "${YARD_INSTANCE_NAME:?}" "${PROJ[@]}" --mode=non-interactive -- \
  "$WORKER" "${WORKER_ARGS[@]}" "$@"
