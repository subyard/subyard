#!/usr/bin/env bash
# Controller for Hermes' single-VM disposable acceptance.
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
bash "$ROOT/dev/agent-e2e.sh" --slot "$slot" --vm-count 1 --purpose hermes-profile --vm 1 -- \
  bash config/profiles/hermes/tests/e2e/hermes-profile.sh
