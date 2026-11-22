#!/usr/bin/env bash
# Also used by Makefile so separate recipes share one temporary run.
set -euo pipefail
helper="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)/test-run-temp.py"
if [ "${SUBYARD_RUN_TEMP_ENTERED:-}" != "$$" ]; then
  exec python3 "$helper" enter bash "$0" "$@"
fi
trap 'python3 "$helper" clean' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
"$@"
