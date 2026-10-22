#!/usr/bin/env bash
# Run one Go-selected profile hook inside the yard.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib/engine-context.sh
. "$SCRIPT_DIR/lib/engine-context.sh"
subyard_require_engine_context
mode=apply
if [ "${1:-}" = --check ]; then
  mode=check
  shift
fi
profile="${1:-}"
shift || true
case "$profile" in '' | *[!a-zA-Z0-9_-]*) printf 'provision: invalid profile\n' >&2; exit 2 ;; esac
[ "$#" -eq 0 ] || { printf 'provision: unexpected argument\n' >&2; exit 2; }
helper="$SCRIPT_DIR/lib/producer-run.py"
bootstrap="$(python3 - "$helper" <<'SOURCE'
import pathlib, sys
print("_SOURCE=" + repr(pathlib.Path(sys.argv[1]).read_text()) + "\nexec(_SOURCE)")
SOURCE
)"
# The native metadata carries the already evaluated public profile environment
# and complete source binding. Capture validates it before invoking Incus.
exec python3 "$helper" capture "$mode" "$profile" \
  incus exec "${YARD_INSTANCE_NAME:?}" --project "${INCUS_PROJECT:?}" \
  --env DEV_USER="${DEV_USER:-dev}" --env EXCLUSIVE_ENVIRONMENT_PROFILE="${EXCLUSIVE_ENVIRONMENT_PROFILE:-}" --env YARD_KIND="${YARD_KIND:-container}" \
  -- python3 -B -c "$bootstrap" launch profile '' '[]' "$mode"
