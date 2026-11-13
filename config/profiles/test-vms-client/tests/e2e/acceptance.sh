#!/usr/bin/env bash
# Live shared-client lease from an installed copy outside any Subyard checkout:
# acquire, root exec in two separate processes of one lease, release, then the
# transport boundary through the runner built on the same client.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." && pwd)"
PROFILE="$ROOT/config/profiles/test-vms-client"
slot=''
usage() { printf 'usage: acceptance.sh --slot N\n' >&2; exit 2; }
while [ "$#" -gt 0 ]; do
  case "$1" in
    --slot) [ "$#" -ge 2 ] && [ -z "$slot" ] || usage; slot="$2"; shift 2 ;;
    *) usage ;;
  esac
done
[[ "$slot" =~ ^[1-9][0-9]*$ ]] || usage
fail() { printf 'test-vms-client acceptance: %s\n' "$*" >&2; exit 1; }
bash "$PROFILE/tests/run.sh"
route=()
if [ -n "${SUBYARD_E2E_ROLLOUT_YARD:-}" ]; then
  [[ "$SUBYARD_E2E_ROLLOUT_YARD" =~ ^[a-z0-9][a-z0-9_-]*$ ]] || usage
  route=(--yard "$SUBYARD_E2E_ROLLOUT_YARD")
fi

work="$(mktemp -d /tmp/subyard-test-vms-client.XXXXXX)"
handle=''
finish() {
  local rc=$?
  trap - EXIT
  set +e
  if [ -n "$handle" ]; then bash "$client" release "$handle" || rc=3; fi
  find "$work" -depth -delete
  printf 'TEST_VMS_CLIENT_RESULT exit_code=%s\n' "$rc"
  exit "$rc"
}
trap finish EXIT
# Install the shipped skill content the way profile provisioning delivers it.
mkdir -p "$work/home"
python3 -B "$ROOT/config/skills/deliver.py" apply --provider profile-test-vms-client \
  --source "$PROFILE/skills" --home "$work/home" --uid "$(id -u)" --gid "$(id -g)" --root "$work/root" --extra
client="$work/root/usr/local/share/subyard/skills/subyard-test-vms/client.sh"
cd "$work"

bash "$client" status "${route[@]}"
handle="$(bash "$client" acquire "${route[@]}" --slot "$slot" --purpose test-vms-client)"
token="$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')"
bash "$client" exec "$handle" -- sh -c 'id -u > /root/client-check && printf "%s\n" "$1" >> /root/client-check' check "$token"
[ "$(bash "$client" exec "$handle" -- cat /root/client-check)" = "0"$'\n'"$token" ] \
  || fail 'the second exec did not run as root in the same leased VM'
bash "$client" leases | grep -E "^$handle .* active " >/dev/null || fail 'leases does not show the active handle'
bash "$client" release "$handle"
released="$handle"
handle=''
if bash "$client" exec "$released" -- true 2>/dev/null; then fail 'exec succeeded after release'; fi

bash "$ROOT/dev/agent-e2e.sh" "${route[@]}" --slot "$slot" --vm-count 1 --purpose test-vms-client-boundary --verify-boundary
printf 'ok: installed client leased, ran root commands across calls, released; boundary verified\n'
