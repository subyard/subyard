#!/usr/bin/env bash
# Also used by Makefile so separate recipes share one temporary run.
set -euo pipefail
helper="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)/test-run-temp.py"
if ! python3 "$helper" check; then
  exec python3 "$helper" enter bash "$0" "$@"
fi
trap 'rc=$?; python3 "$helper" clean || { [ "$rc" -ne 0 ] || rc=1; }; exit "$rc"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
"$@"
