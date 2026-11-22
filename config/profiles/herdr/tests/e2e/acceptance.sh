#!/usr/bin/env bash
# Physical lifecycle of an installed Herdr profile on a disposable VM.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." && pwd -P)"
fixture=config/profiles/herdr/tests/e2e
slot=''
while [ "$#" -gt 0 ]; do
  case "$1" in
    --slot) slot="${2:?missing slot}"; shift 2 ;;
    --lane) [ "${2:-}" = lifecycle ] || exit 2; shift 2 ;;
    --preflight)
      [ "$#" -eq 1 ] || exit 2
      cd "$root"
      bash -n "$fixture/lifecycle.sh"
      shellcheck -x -S warning "$fixture/acceptance.sh" "$fixture/lifecycle.sh"
      python3 -c 'import ast, pathlib, sys; ast.parse(pathlib.Path(sys.argv[1]).read_text())' "$fixture/client.py"
      exec bash config/profiles/herdr/tests/run.sh ;;
    *) exit 2 ;;
  esac
done
[[ "$slot" =~ ^[1-9][0-9]*$ ]] || { printf 'Usage: acceptance.sh --slot N [--lane lifecycle] | --preflight\n' >&2; exit 2; }
cd "$root"
exec bash dev/agent-e2e.sh --slot "$slot" --vm-count 1 --purpose herdr-lifecycle --vm 1 -- bash "$fixture/lifecycle.sh"
