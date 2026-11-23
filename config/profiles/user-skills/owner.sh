#!/usr/bin/env bash
# Owner service: deliver the profile skill, then clone configured skill sources
# and link their skills as the developer user inside the yard.
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
# A guest init never provisioned has no links or skill to remove.
if [ "${SUBYARD_GUEST_NEVER_PROVISIONED:-0}" = 1 ] && [ "$removal" = 1 ]; then
  exit 0
fi
sources=()
if [ "$selected" = 1 ]; then
  # The list is whitespace-separated and may span lines; read -a never globs.
  IFS=$' \t\n' read -r -d '' -a sources <<<"${USER_SKILL_SOURCES:-}" || :
fi
# A check's stderr may carry only the readiness record; apply keeps diagnostics.
if [ "$mode" = check ]; then
  exec {guest_stderr}>/dev/null
else
  exec {guest_stderr}>&2
fi
rc=0
# The guest prints one token for a check; Git output and source URLs never leave it.
output="$(tar -C "$profile" --exclude=__pycache__ -cf - skills/subyard-user-skills \
    -C "$root/config/skills" --transform='s,^deliver.py$,agent-skills.py,' deliver.py \
  | incus exec "$YARD_INSTANCE_NAME" --project "$INCUS_PROJECT" -- bash -euo pipefail -c '
    mode="$1" selected="$2" user="$3"
    shift 3
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
    sync_helper() {
      setpriv --reuid "$uid" --regid "$gid" --init-groups --no-new-privs \
        env -i HOME="$home" PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
        python3 -B "$bundle/skills/subyard-user-skills/sync.py" "$@"
    }
    agent_skills() {
      python3 -B "$bundle/agent-skills.py" "$1" --provider profile-user-skills --source "$bundle/skills" \
        --home "$home" --uid "$uid" --gid "$gid" --extra
    }
    if [ "$mode" = check ]; then
      skill_rc=0
      if [ "$selected" = 1 ]; then agent_skills check-registered || skill_rc=$?; else agent_skills check-removed || skill_rc=$?; fi
      case "$skill_rc" in 0|10) ;; *) exit 2 ;; esac
      token="$(sync_helper check "$@")"
      case "$token" in current|work|unavailable) ;; *) exit 2 ;; esac
      [ "$skill_rc" = 0 ] || token=work
      printf "%s\n" "$token"
      exit 0
    fi
    if [ "$selected" = 1 ]; then
      agent_skills apply >/dev/null || exit 2
      sync_rc=0
      sync_helper apply "$@" >/dev/null || sync_rc=$?
      case "$sync_rc" in 0|3) exit 0 ;; 4) exit 4 ;; *) exit 2 ;; esac
    fi
    sync_rc=0
    sync_helper unlink >/dev/null || sync_rc=$?
    case "$sync_rc" in 0) ;; 4) exit 4 ;; *) exit 2 ;; esac
    agent_skills remove >/dev/null || exit 2
  ' subyard "$mode" "$selected" "$DEV_USER" "${sources[@]}" 2>&"$guest_stderr")" || rc=$?
exec {guest_stderr}>&-
if [ "$mode" = check ]; then
  [ "$rc" = 0 ] || exit 2
  case "$output" in
    current) exit 0 ;;
    work) exit 1 ;;
    unavailable)
      printf 'subyard-profile-readiness-v1 source-unavailable\n' >&2
      exit 1 ;;
    *) exit 2 ;;
  esac
fi
case "$rc" in
  0) exit 0 ;;
  4) printf 'user-skills: another user-skills sync holds the lock; retry\n' >&2; exit 1 ;;
  *) printf 'user-skills: skill delivery or source sync failed\n' >&2; exit 2 ;;
esac
