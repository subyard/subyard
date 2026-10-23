#!/usr/bin/env bash
# Controller for Orca's declared disposable-host acceptance lanes.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." && pwd)"
slot=''
lane=full
native_loss=''
while [ "$#" -gt 0 ]; do
  case "$1" in
    --slot) [ "$#" -ge 2 ] || { printf 'acceptance: --slot needs a value\n' >&2; exit 2; }; slot="$2"; shift 2 ;;
    --lane) [ "$#" -ge 2 ] || { printf 'acceptance: --lane needs a value\n' >&2; exit 2; }; lane="$2"; shift 2 ;;
    --native-loss) [ "$#" -ge 2 ] && [ -z "$native_loss" ] || exit 2; native_loss="$2"; shift 2 ;;
    *) printf 'usage: acceptance.sh --slot N [--lane full|bootstrap|existing-yard|resource|projects|ssh-agent|codex-permissions|codex-approvals] [--native-loss owner|helper|group]\n' >&2; exit 2 ;;
  esac
done
[[ "$slot" =~ ^[1-9][0-9]*$ ]] || { printf 'acceptance: --slot N is required\n' >&2; exit 2; }
case "$lane" in
  full) lanes=(bootstrap existing-yard resource projects ssh-agent codex-permissions) ;;
  bootstrap|existing-yard|resource|projects|ssh-agent|codex-permissions|codex-approvals) lanes=("$lane") ;;
  *) printf 'acceptance: invalid lane: %s\n' "$lane" >&2; exit 2 ;;
esac
case "$native_loss" in ''|owner|helper|group) ;; *) exit 2 ;; esac
if [ -n "$native_loss" ]; then
  [ "$lane" = bootstrap ] || { printf 'acceptance: native loss uses a separate bootstrap fixture\n' >&2; exit 2; }
  python3 -B "$ROOT/tests/helpers/profile-native-loss.py" --preflight \
    --bundle "${SUBYARD_E2E_CANDIDATE_BUNDLE:-}" --bundle-sha256 "${SUBYARD_E2E_CANDIDATE_SHA256:-}"
fi
runner="$ROOT/dev/agent-e2e.sh"
bootstrap=config/profiles/orca/tests/e2e/orca-bootstrap.sh
resource=config/profiles/orca/tests/e2e/orca-resource.sh
projects=config/profiles/orca/tests/e2e/orca-projects.sh
loss_environment=()
[ -z "$native_loss" ] || loss_environment=("SUBYARD_E2E_ORCA_NATIVE_LOSS=$native_loss")
# Each owner-only lane acquires its own fresh generic singleton baseline.
for lane in "${lanes[@]}"; do
  case "$lane" in
    bootstrap)
      bash "$runner" --slot "$slot" --vm-count 1 --purpose orca-bootstrap --vm 1 -- \
        env SUBYARD_E2E_ORCA_BOOTSTRAP=1 "${loss_environment[@]}" bash "$bootstrap" ;;
    existing-yard)
      bash "$runner" --slot "$slot" --vm-count 1 --purpose orca-bootstrap --vm 1 -- \
        env SUBYARD_E2E_ORCA_BOOTSTRAP=1 SUBYARD_E2E_ORCA_EXISTING_YARD=1 bash "$bootstrap" ;;
    resource)
      bash "$runner" --slot "$slot" --vm-count 1 --purpose orca-resource --vm 1 -- \
        env SUBYARD_E2E_ORCA_RESOURCE=1 bash "$resource" ;;
    projects)
      bash "$runner" --slot "$slot" --vm-count 1 --purpose orca-projects --vm 1 -- \
        env SUBYARD_E2E_ORCA_PROJECTS=1 bash "$projects" ;;
    ssh-agent)
      bash "$runner" --slot "$slot" --vm-count 1 --purpose orca-ssh-agent --vm 1 -- \
        env SUBYARD_E2E_ORCA_BOOTSTRAP=1 SUBYARD_E2E_ORCA_SSH_AGENT=1 bash "$bootstrap" ;;
    codex-approvals)
      bash "$runner" --slot "$slot" --vm-count 1 --purpose codex-approvals --vm 1 -- \
        env SUBYARD_E2E_ORCA_BOOTSTRAP=1 SUBYARD_E2E_ORCA_CODEX_PERMISSIONS=1 \
        SUBYARD_E2E_ORCA_CODEX_APPROVALS_ONLY=1 bash "$bootstrap" ;;
    codex-permissions)
      bash "$runner" --slot "$slot" --vm-count 1 --purpose codex-permissions --vm 1 -- \
        env SUBYARD_E2E_ORCA_BOOTSTRAP=1 SUBYARD_E2E_ORCA_CODEX_PERMISSIONS=1 bash "$bootstrap" ;;
  esac
done
