#!/usr/bin/env bash
# Run guest.sh in the running yard as the developer.
set -euo pipefail
profile="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "$profile/../../.." && pwd)"
# shellcheck source=scripts/lib/engine-context.sh
. "$root/scripts/lib/engine-context.sh"
# shellcheck source=scripts/lib-power.sh
. "$root/scripts/lib-power.sh"
subyard_require_engine_context || exit 2
[ "$#" = 1 ] || exit 2
case "$1" in
  --check) mode=check ;;
  --yes) mode=apply ;;
  --remove|--pause|--resume) exit 0 ;;
  *) exit 2 ;;
esac
[ "${SUBYARD_PROFILE_SELECTED:-0}" = 1 ] || exit 0
state="$(power_state "$INCUS_PROJECT" "$YARD_INSTANCE_NAME")" || exit 2
case "$state" in RUNNING) ;; ''|STOPPED) exit 0 ;; *) exit 2 ;; esac
if [ "$mode" = check ] && [ "${SUBYARD_PROFILE_POST_APPLY:-0}" = 1 ]; then mode=verify; fi
sources=()
IFS=$' \t\n' read -r -d '' -a sources <<<"${USER_SKILL_SOURCES:-}" || :
output="$(incus exec "$YARD_INSTANCE_NAME" --project "$INCUS_PROJECT" -- \
  runuser -u "$DEV_USER" -- bash -s -- "$mode" "${sources[@]}" <"$profile/guest.sh")" \
  || { printf 'user-skills: clone or link operation failed\n' >&2; exit 2; }
# A transport failure never reads as drift (exit 1).
[ -z "$output" ] || [ "$mode" = apply ] || exit 1
