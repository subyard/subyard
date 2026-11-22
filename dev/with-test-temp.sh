#!/usr/bin/env bash
# Also used by Makefile so separate recipes share one temporary run.
set -euo pipefail
helper="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)/test-run-temp.py"
if ! python3 "$helper" check; then
  exec python3 "$helper" enter bash "$0" "$@"
fi
finish() {
  local rc="$1"
  trap - EXIT
  # Release this shell's lock reference; the root goes only when no user remains.
  exec {SUBYARD_RUN_TEMP_FD}<&-
  python3 "$helper" clean || { [ "$rc" -ne 0 ] || rc=1; }
  exit "$rc"
}
trap 'finish "$?"' EXIT
(trap - EXIT INT QUIT; exec "$@") <&0 &
command_pid=$!
trap 'kill -INT "$command_pid" 2>/dev/null || :; wait "$command_pid" || :; exit 130' INT
trap 'kill -TERM "$command_pid" 2>/dev/null || :; wait "$command_pid" || :; exit 143' TERM
wait "$command_pid"
