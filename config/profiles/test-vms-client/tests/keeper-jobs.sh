#!/usr/bin/env bash
# Keeper cleanup reaps every background job, including one whose PID was never recorded.
set -euo pipefail
PROFILE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CLIENT="$PROFILE/skills/subyard-test-vms/client.sh"
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

# A stop signal between a timer's fork and its PID assignment must not orphan the timer,
# which would hold the caller's captured streams until it expires.
rc=0
output="$(timeout 5 bash -c '
  set -euo pipefail
  . "$1"
  set -m
  keeper_timer_pid="" keeper_job_pid=""
  /bin/sleep 300 </dev/null >/dev/null 2>&1 &
  untracked=$!
  trap "kill -KILL \"\$untracked\" >/dev/null 2>&1 || true" EXIT
  lease_keeper_stop_jobs
  ! kill -0 "$untracked" >/dev/null 2>&1
' _ "$CLIENT" 2>&1)" || rc=$?
[ "$rc" = 0 ] || fail "keeper stop left an untracked timer: $rc $output"
printf 'ok: keeper cleanup reaps untracked timers\n'
