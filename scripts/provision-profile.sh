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
error_file="$(mktemp)"
trap 'rm -f -- "$error_file"' EXIT

env_args=(--env DEV_USER="${DEV_USER:-dev}")
# shellcheck disable=SC1090
. "$config"
while IFS= read -r name; do
  [ -z "$name" ] || env_args+=(--env "$name=${!name-}")
done < <(grep -oE '^[A-Za-z_][A-Za-z0-9_]*=' "$config" | sed 's/=$//' | sort -u)
set +e
tar -C "$bundle" -cf - . | incus exec "${INSTANCE_NAME:?}" --project "${INCUS_PROJECT:?}" \
  "${env_args[@]}" -- bash -euo pipefail -c '
    profile_dir="$(mktemp -d /tmp/subyard-profile.XXXXXX)"
    trap '\''rm -rf -- "$profile_dir"'\'' EXIT
    tar -xf - -C "$profile_dir"
    if [ "$1" = check ]; then
      bash "$profile_dir/provision.sh" --check >/dev/null
    else
      bash "$profile_dir/provision.sh"
    fi
  ' subyard "$mode" 2>"$error_file"
pipeline_status=("${PIPESTATUS[@]}")
set -e
[ "${pipeline_status[0]}" -eq 0 ] || exit "${pipeline_status[0]}"
status="${pipeline_status[1]}"
if [ "$mode" = check ]; then
  case "$status" in
    0) printf 'converged\n' ;;
    10) printf 'changed\n' ;;
    *) cat "$error_file" >&2; exit "$status" ;;
  esac
else
  cat "$error_file" >&2
  exit "$status"
fi
