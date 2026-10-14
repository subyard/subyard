#!/usr/bin/env bash
# Controller for openlit provisioning acceptance on one disposable owner VM.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." && pwd -P)"
slot=''
kind=''
auth_only=''
usage() { printf 'Usage: config/profiles/openlit/tests/e2e/acceptance.sh --slot N [--kind container|vm] [--auth-only | --teardown-only | --dependency-only | --network-only | --pull-diagnostic | --migration]\n'; }
while [ "$#" -gt 0 ]; do
  case "$1" in
    --slot)
      if [ "$#" -lt 2 ] || [ -n "$slot" ]; then usage >&2; exit 2; fi
      slot="$2"
      shift 2
      ;;
    --kind)
      if [ "$#" -lt 2 ] || [ -n "$kind" ]; then usage >&2; exit 2; fi
      case "$2" in container|vm) ;; *) usage >&2; exit 2 ;; esac
      kind="$2"; shift 2 ;;
    --auth-only|--teardown-only|--dependency-only|--network-only|--pull-diagnostic|--migration) [ -z "$auth_only" ] || exit 2; auth_only=${1#--}; shift ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
  esac
done
[[ "$slot" =~ ^[1-9][0-9]*$ ]] || { usage >&2; exit 2; }
[ "$auth_only" != network-only ] || [ "${kind:-container}" = container ] || { usage >&2; exit 2; }
[ "$auth_only" != pull-diagnostic ] || [ "${kind:-container}" = container ] || { usage >&2; exit 2; }
[ "$auth_only" != migration ] || [ -z "$kind" ] || { usage >&2; exit 2; }

if [ "$auth_only" = migration ]; then
  # One independent owner/stack avoids copying nested Docker stores into the
  # ordinary profile lane's fixed disk budget.
  exec bash "$ROOT/dev/agent-e2e.sh" --slot "$slot" --type android-test --vm-count 1 --purpose openlit-trace-migration --vm 1 -- \
    timeout --signal=TERM --kill-after=30 3600 bash config/profiles/openlit/tests/e2e/migration.sh
fi

arguments=()
[ -z "$kind" ] || arguments=("$kind")
[ -z "$auth_only" ] || arguments=("${kind:-container}" "$auth_only")
# The generic larger Linux singleton keeps two live stacks/nested VM routing in budget.
bash "$ROOT/dev/agent-e2e.sh" --slot "$slot" --type android-test --vm-count 1 --purpose openlit-native-telemetry --vm 1 -- \
  timeout --signal=TERM --kill-after=30 3600 bash config/profiles/openlit/tests/e2e/owner.sh "${arguments[@]}"
