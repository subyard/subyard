#!/usr/bin/env bash
# Explicit extended/manual Orca diagnostic, never ordinary acceptance.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." && pwd)"
slot=''
roots=1000
lane=load
usage() { printf 'usage: load-diagnostic.sh --slot N [--roots 1000|1400] [--lane load|cleanup] [--help]\n'; }
while [ "$#" -gt 0 ]; do
  case "$1" in
    --help) usage; exit 0 ;;
    --slot|--roots|--lane)
      [ "$#" -ge 2 ] || { usage >&2; exit 2; }
      case "$1" in --slot) slot="$2" ;; --roots) roots="$2" ;; --lane) lane="$2" ;; esac
      shift 2 ;;
    *) usage >&2; exit 2 ;;
  esac
done
[[ "$slot" =~ ^[1-9][0-9]*$ ]] || { usage >&2; exit 2; }
case "$roots" in 1000|1400) ;; *) usage >&2; exit 2 ;; esac
case "$lane" in load|cleanup) ;; *) usage >&2; exit 2 ;; esac
exec bash "$ROOT/dev/agent-e2e.sh" --slot "$slot" --vm-count 1 \
  --purpose orca-load-diagnostic --vm 1 -- env SUBYARD_E2E_ORCA_PROJECTS=1 \
  SUBYARD_E2E_ORCA_LOAD_DIAGNOSTIC=1 SUBYARD_E2E_ORCA_LOAD_ROOTS="$roots" \
  SUBYARD_E2E_ORCA_LOAD_LANE="$lane" \
  timeout --signal=TERM --kill-after=30s 45m bash config/profiles/orca/tests/e2e/orca-projects.sh
