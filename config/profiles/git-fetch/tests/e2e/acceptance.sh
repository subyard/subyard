#!/usr/bin/env bash
# Direct installed-component check in one disposable systemd guest.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." && pwd -P)"
slot=''
usage() { printf 'Usage: config/profiles/git-fetch/tests/e2e/acceptance.sh --slot N\n'; }
while [ "$#" -gt 0 ]; do
  case "$1" in
    --slot) [ "$#" -ge 2 ] && [ -z "$slot" ] || { usage >&2; exit 2; }; slot="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
  esac
done
[[ "$slot" =~ ^[1-9][0-9]*$ ]] || { usage >&2; exit 2; }
bash "$ROOT/dev/agent-e2e.sh" --slot "$slot" --vm-count 1 --purpose git-fetch-profile --vm 1 -- \
  timeout --signal=TERM --kill-after=10s 3m sudo -n python3 -B config/profiles/git-fetch/tests/e2e/guest.py
