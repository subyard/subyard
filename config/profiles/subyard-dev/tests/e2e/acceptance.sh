#!/usr/bin/env bash
# Controller for subyard-dev provisioning acceptance on one disposable owner VM.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." && pwd -P)"
slot=''
usage() { printf 'Usage: config/profiles/subyard-dev/tests/e2e/acceptance.sh --slot N\n'; }
while [ "$#" -gt 0 ]; do
  case "$1" in
    --slot)
      if [ "$#" -lt 2 ] || [ -n "$slot" ]; then usage >&2; exit 2; fi
      slot="$2"
      shift 2
      ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
  esac
done
[[ "$slot" =~ ^[1-9][0-9]*$ ]] || { usage >&2; exit 2; }

# This owner-only lane uses the generic singleton baseline.
bash "$ROOT/dev/agent-e2e.sh" --slot "$slot" --vm-count 1 --purpose subyard-dev-provision --vm 1 -- \
  bash config/profiles/subyard-dev/tests/e2e/owner.sh
