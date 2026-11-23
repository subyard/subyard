#!/usr/bin/env bash
# Clone missing skill sources and link their skills as the yard developer.
set -euo pipefail
profile="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "$profile/../../.." && pwd)"
# shellcheck source=scripts/lib/engine-context.sh
. "$root/scripts/lib/engine-context.sh"
# shellcheck source=scripts/lib-power.sh
. "$root/scripts/lib-power.sh"
subyard_require_engine_context || exit 2
selected=${SUBYARD_PROFILE_SELECTED:-0}
removal=0
[ "$#" = 1 ] || exit 2
case "$1" in
  --check) mode=check ;;
  --yes) mode=apply ;;
  --remove) mode=apply; selected=0; removal=1 ;;
  --pause|--resume) exit 0 ;;
  *) exit 2 ;;
esac
[ "$selected" = 1 ] || selected=0
state="$(power_state "$INCUS_PROJECT" "$YARD_INSTANCE_NAME")" || exit 2
case "$state" in
  RUNNING) ;;
  ''|STOPPED) exit 0 ;;
  *) printf 'user-skills: yard is not ready for profile reconciliation\n' >&2; exit 2 ;;
esac
# A guest init never provisioned has no links to remove.
if [ "${SUBYARD_GUEST_NEVER_PROVISIONED:-0}" = 1 ] && [ "$removal" = 1 ]; then
  exit 0
fi
sources=()
if [ "$selected" = 1 ]; then
  # The list is whitespace-separated and may span lines; read -a never globs.
  IFS=$' \t\n' read -r -d '' -a sources <<<"${USER_SKILL_SOURCES:-}" || :
fi
# Checks are quiet; apply keeps the source warning and next step.
if [ "$mode" = check ]; then
  exec {guest_stderr}>/dev/null
else
  exec {guest_stderr}>&2
fi
rc=0
# Checks return one token; Git output and source URLs stay inside the guest.
output="$(tar -C "$profile" -cf - clone.py \
  | incus exec "$YARD_INSTANCE_NAME" --project "$INCUS_PROJECT" -- bash -euo pipefail -c '
    mode="$1" selected="$2" user="$3" post_apply="$4"
    shift 4
    bundle="$(mktemp -d /tmp/subyard-user-skills.XXXXXX)"
    trap '\''rm -rf -- "$bundle"'\'' EXIT
    tar --no-same-owner --no-same-permissions -xf - -C "$bundle"
    chmod -R u=rwX,go=rX "$bundle"
    home="$(getent passwd "$user" | cut -d: -f6 || :)"
    if [ -z "$home" ]; then
      # Without the developer account nothing of the profile can exist yet.
      case "$mode:$selected" in
        check:1) printf "work\n" ;;
        check:0) printf "current\n" ;;
        apply:1) exit 2 ;;
      esac
      exit 0
    fi
    uid="$(id -u "$user")" gid="$(id -g "$user")"
    cd /
    setpriv --reuid "$uid" --regid "$gid" --init-groups --no-new-privs \
      env -i HOME="$home" PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
      python3 -B "$bundle/clone.py" "$mode" "$post_apply" "$@"
  ' subyard "$mode" "$selected" "$DEV_USER" "${SUBYARD_PROFILE_POST_APPLY:-0}" "${sources[@]}" 2>&"$guest_stderr")" || rc=$?
exec {guest_stderr}>&-
if [ "$mode" = check ]; then
  [ "$rc" = 0 ] || exit 2
  case "$output" in
    current) exit 0 ;;
    work) exit 1 ;;
    *) exit 2 ;;
  esac
fi
case "$rc" in
  0) exit 0 ;;
  *) printf 'user-skills: clone or link operation failed\n' >&2; exit 2 ;;
esac
