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
root="$(cd "$SCRIPT_DIR/.." && pwd)"
config="$root/config/profiles/$profile/profile.conf"
hook="$root/config/profiles/$profile/provision.sh"
[ -r "$config" ] && [ -r "$hook" ] || { printf 'provision: profile hook missing\n' >&2; exit 1; }
grep -Fxq '# subyard-provision-check-v1' "$hook" \
  || { printf 'provision: profile hook does not support check protocol\n' >&2; exit 1; }
bundle="$(dirname "$hook")"
helper="$SCRIPT_DIR/lib/producer-run.py"
bootstrap="$(python3 - "$helper" <<'SOURCE'
import pathlib, sys
print("_SOURCE=" + repr(pathlib.Path(sys.argv[1]).read_text()) + "\nexec(_SOURCE)")
SOURCE
)"
shared_tmp='[]'
if [ -f "$bundle/provision-tmp-paths" ]; then
  shared_tmp="$(python3 - "$bundle/provision-tmp-paths" <<'PATHS'
import json, pathlib, sys
print(json.dumps(pathlib.Path(sys.argv[1]).read_text().splitlines()))
PATHS
)"
fi

env_args=(--env DEV_USER="${DEV_USER:-dev}" --env EXCLUSIVE_ENVIRONMENT_PROFILE="${EXCLUSIVE_ENVIRONMENT_PROFILE:-}" --env YARD_KIND="${YARD_KIND:-container}")
# shellcheck disable=SC1090
. "$config"
while IFS= read -r name; do
  [ -z "$name" ] || env_args+=(--env "$name=${!name-}")
done < <(grep -oE '^[A-Za-z_][A-Za-z0-9_]*=' "$config" | sed 's/=$//' | sort -u)
# Keep the transport stdin open after the archive; EOF signals controller loss.
exec python3 "$helper" capture "$mode" "$bundle" \
  incus exec "${YARD_INSTANCE_NAME:?}" --project "${INCUS_PROJECT:?}" \
  "${env_args[@]}" -- python3 -B -c "$bootstrap" launch profile '' "$shared_tmp" "$mode"
