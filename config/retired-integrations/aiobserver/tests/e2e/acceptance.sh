#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." && pwd)"
slot=''
mode=live
while [ "$#" -gt 0 ]; do
  case "$1" in
    --slot) [ "$#" -ge 2 ] && [ -z "$slot" ] || exit 2; slot="$2"; shift 2 ;;
    --mode) [ "$#" -ge 2 ] || exit 2; mode="$2"; shift 2 ;;
    --help) printf 'Usage: retirement acceptance --slot N [--mode live|stopped]\n'; exit ;;
    *) exit 2 ;;
  esac
done
[[ "$slot" =~ ^[1-9][0-9]*$ ]] && { [ "$mode" = live ] || [ "$mode" = stopped ]; } || exit 2
exec bash "$ROOT/dev/agent-e2e.sh" --slot "$slot" --vm-count 1 --purpose observer-retirement --vm 1 -- \
  timeout --signal=TERM --kill-after=30 3600 bash config/retired-integrations/aiobserver/tests/e2e/owner.sh "$mode"
