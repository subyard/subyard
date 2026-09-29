#!/usr/bin/env bash
# Controller for Orca's declared disposable-host acceptance lanes.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." && pwd)"
slot=''
while [ "$#" -gt 0 ]; do
  case "$1" in
    --slot) [ "$#" -ge 2 ] || { printf 'acceptance: --slot needs a value\n' >&2; exit 2; }; slot="$2"; shift 2 ;;
    *) printf 'usage: acceptance.sh --slot N\n' >&2; exit 2 ;;
  esac
done
[[ "$slot" =~ ^[1-9][0-9]*$ ]] || { printf 'acceptance: --slot N is required\n' >&2; exit 2; }
runner="$ROOT/dev/agent-e2e.sh"
bootstrap=config/profiles/orca/tests/e2e/orca-bootstrap.sh
resource=config/profiles/orca/tests/e2e/orca-resource.sh
projects=config/profiles/orca/tests/e2e/orca-projects.sh
bash "$runner" --slot "$slot" --purpose orca-bootstrap --vm 1 -- \
  env SUBYARD_E2E_ORCA_BOOTSTRAP=1 bash "$bootstrap"
bash "$runner" --slot "$slot" --purpose orca-bootstrap --vm 1 -- \
  env SUBYARD_E2E_ORCA_BOOTSTRAP=1 SUBYARD_E2E_ORCA_EXISTING_YARD=1 bash "$bootstrap"
bash "$runner" --slot "$slot" --purpose orca-resource --vm 1 -- \
  env SUBYARD_E2E_ORCA_RESOURCE=1 bash "$resource"
bash "$runner" --slot "$slot" --purpose orca-projects --vm 1 -- \
  env SUBYARD_E2E_ORCA_PROJECTS=1 bash "$projects"
bash "$runner" --slot "$slot" --purpose orca-ssh-agent --vm 1 -- \
  env SUBYARD_E2E_ORCA_BOOTSTRAP=1 SUBYARD_E2E_ORCA_SSH_AGENT=1 bash "$bootstrap"
bash "$runner" --slot "$slot" --purpose codex-permissions --vm 1 -- \
  env SUBYARD_E2E_ORCA_BOOTSTRAP=1 SUBYARD_E2E_ORCA_CODEX_PERMISSIONS=1 bash "$bootstrap"
