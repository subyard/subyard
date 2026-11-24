#!/usr/bin/env bash
# One leased VM: the installed candidate creates a named yard with the profile, then
# checks source access retry, disable/enable, deselection and named-yard enable.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." && pwd -P)"
slot=''
usage() { printf 'Usage: config/profiles/user-skills/tests/e2e/acceptance.sh --slot N | --preflight\n'; }
preflight() {
  bash -n "$ROOT/config/profiles/user-skills/tests/e2e/"*.sh
  shellcheck -S warning "$ROOT/config/profiles/user-skills/tests/e2e/"*.sh
  printf 'ok: user-skills lane syntax and ShellCheck; Incus and the installed candidate are checked on the allocated VM\n'
}
while [ "$#" -gt 0 ]; do
  case "$1" in
    --slot) [ "$#" -ge 2 ] && [ -z "$slot" ] || { usage >&2; exit 2; }; slot="$2"; shift 2 ;;
    --preflight) preflight; exit 0 ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
  esac
done
[[ "$slot" =~ ^[1-9][0-9]*$ ]] || { usage >&2; exit 2; }
preflight
route=()
if [ -n "${SUBYARD_E2E_ROLLOUT_YARD:-}" ]; then
  [[ "$SUBYARD_E2E_ROLLOUT_YARD" =~ ^[a-z0-9][a-z0-9_-]*$ ]] || { usage >&2; exit 2; }
  route=(--yard "$SUBYARD_E2E_ROLLOUT_YARD")
fi
bash "$ROOT/dev/agent-e2e.sh" "${route[@]}" --slot "$slot" --vm-count 1 --purpose user-skills-yard --vm 1 -- \
  timeout --signal=TERM --kill-after=120 3600 bash config/profiles/user-skills/tests/e2e/yard-lane.sh
