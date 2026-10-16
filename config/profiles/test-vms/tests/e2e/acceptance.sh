#!/usr/bin/env bash
# Reuse the held-lease full-uninstall acceptance for the host-sink lifecycle owner.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." && pwd)"
slot=''
usage() { printf 'usage: acceptance.sh --slot N\n' >&2; exit 2; }
while [ "$#" -gt 0 ]; do
  case "$1" in
    --slot)
      [ "$#" -ge 2 ] && [ -z "$slot" ] || usage
      slot="$2"
      shift 2
      ;;
    *) usage ;;
  esac
done
[[ "$slot" =~ ^[1-9][0-9]*$ ]] || usage
exec bash "$ROOT/dev/e2e/p0-acceptance.sh" --slot "$slot" --lane full-uninstall
