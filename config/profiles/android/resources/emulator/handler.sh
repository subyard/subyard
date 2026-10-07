#!/usr/bin/env bash
# Profile-owned lease client; declared viewer sessions run on the controller.
set -euo pipefail
RESOURCE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SUBYARD_ROOT="$(cd "$RESOURCE_DIR/../../../../.." && pwd)"
SCRIPT_DIR="$SUBYARD_ROOT/scripts"
# shellcheck source=scripts/lib/engine-context.sh
. "$SCRIPT_DIR/lib/engine-context.sh"
subyard_require_engine_context
# shellcheck source=scripts/lib/ui.sh
. "$SCRIPT_DIR/lib/ui.sh"
# shellcheck source=scripts/lib/host.sh
. "$SCRIPT_DIR/lib/host.sh"
# shellcheck source=scripts/lib-service.sh
. "$SCRIPT_DIR/lib-service.sh"
CLIENT="$RESOURCE_DIR/../../client.py"
if [ "${SUBYARD_RESOURCE_MODE:-}" = wire ] && [ "$#" -eq 2 ] && [ "$1" = --session-wire ] && [ "$2" = view ]; then
  exec incus exec "$YARD_INSTANCE_NAME" --project "$INCUS_PROJECT" --user "$DEV_UID" -- \
    python3 /usr/local/lib/subyard-android/client.py _wire
fi
if [ -n "${SUBYARD_RESOURCE_SESSION_TRANSPORT:-}" ]; then
  export SUBYARD_YARD_NAME="$OWNER_YARD_NAME"
fi
export SUBYARD_EMU_INSTANCE="$YARD_INSTANCE_NAME" SUBYARD_EMU_PROJECT="$INCUS_PROJECT"

if [ "${1:-}" = is-up ]; then
  yexec systemctl is-active --quiet subyard-android-pool.service >/dev/null 2>&1
  exit "$?"
fi
if [ "$#" -eq 0 ] || [ "$1" = --help ] || [ "$1" = -h ]; then
  exec python3 "$CLIENT" --help
fi
# Parse before any pool operation. Invalid input must never reach reserve or SDK download.
python3 "$CLIENT" _validate "$@"
verb="$1"
case "${SUBYARD_RESOURCE_MODE:-}" in
  prepare)
    if [ -n "${SUBYARD_RESOURCE_SESSION_TRANSPORT:-}" ]; then
      python3 "$CLIENT" _ready
    else
      svc_require_yard_running
    fi
    exec python3 "$CLIENT" _assessment "$@"
    ;;
  apply)
    [ "${SUBYARD_RESOURCE_ACTION:-}" = "$verb" ] && [ -n "${SUBYARD_OPERATION_ID:-}" ] \
      || die 'prepared Android resource action mismatch'
    exec python3 "$CLIENT" "$@"
    ;;
  *) die 'typed resource dispatcher is required' ;;
esac
