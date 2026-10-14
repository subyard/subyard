#!/usr/bin/env bash
# Existing owner-service transport; the fetch timer itself lives only in the yard.
set -euo pipefail
profile="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "$profile/../../.." && pwd)"
# shellcheck source=scripts/lib/engine-context.sh
. "$root/scripts/lib/engine-context.sh"
subyard_require_engine_context || exit 2
selected=${SUBYARD_PROFILE_SELECTED:-0}
mode=apply
[ "$#" = 1 ] || exit 2
case "$1" in
  --yes) ;;
  --check) mode=check ;;
  --remove) selected=0 ;;
  --pause) mode=pause ;;
  --resume) mode=resume ;;
  *) exit 2 ;;
esac
state="$(incus list "$YARD_INSTANCE_NAME" --project "$INCUS_PROJECT" -f csv -c s 2>/dev/null)" || exit 2
case "$state" in
  RUNNING) ;;
  ''|STOPPED) exit 0 ;;
  *) printf 'git-fetch: yard is not ready for profile reconciliation\n' >&2; exit 2 ;;
esac
rc=0
output="$(tar -C "$profile" -cf - guest.py fetch.py subyard-git-fetch.service.in subyard-git-fetch.timer \
  | incus exec "$YARD_INSTANCE_NAME" --project "$INCUS_PROJECT" -- bash -euo pipefail -c '
    bundle="$(mktemp -d /tmp/subyard-git-fetch.XXXXXX)"
    trap '\''rm -rf -- "$bundle"'\'' EXIT
    tar -xf - -C "$bundle"
    python3 -B "$bundle/guest.py" "$@"
  ' subyard "$mode" "$selected" "$DEV_USER" "${SUBYARD_PROFILE_STOPPED:-0}")" || rc=$?
# A successful guest check emits only a measured-drift token. Transport failures
# never share expected-drift exit1, even when Incus itself exits1.
[ "$rc" = 0 ] || exit 2
if [ "$mode" = check ]; then
  case "$output" in '' ) exit 0 ;; git-fetch-drift) exit 1 ;; *) exit 2 ;; esac
fi
[ -z "$output" ] || printf '%s\n' "$output"
