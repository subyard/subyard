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
selected=${SUBYARD_PROFILE_SELECTED:-0}
case "$1" in
  --check) mode=check ;;
  --yes) mode=apply ;;
  --remove) mode=apply; selected=0 ;;
  --pause|--resume) exit 0 ;;
  *) exit 2 ;;
esac
# A guest never provisioned has nothing installed.
if [ "$selected" != 1 ] && [ "${SUBYARD_GUEST_NEVER_PROVISIONED:-0}" = 1 ]; then exit 0; fi
state="$(power_state "$INCUS_PROJECT" "$YARD_INSTANCE_NAME")" || exit 2
case "$state" in RUNNING) ;; ''|STOPPED) exit 0 ;; *) exit 2 ;; esac
if [ "$mode" = check ] && [ "${SUBYARD_PROFILE_POST_APPLY:-0}" = 1 ]; then mode=verify; fi
# Deselected or removed: no source, so guest.sh uninstalls and keeps the clone.
source=''
command=''
if [ "$selected" = 1 ]; then
  source="${USER_DOTFILES_SOURCE:-}"
  command="${USER_DOTFILES_INSTALL_COMMAND:-}"
fi
output="$(incus exec "$YARD_INSTANCE_NAME" --project "$INCUS_PROJECT" -- \
  runuser -u "$DEV_USER" -- bash -s -- "$mode" "$source" "$command" <"$profile/guest.sh")" \
  || { printf 'user-dotfiles: clone or install operation failed\n' >&2; exit 2; }
# A transport failure never reads as drift (exit 1).
[ -z "$output" ] || [ "$mode" = apply ] || exit 1
