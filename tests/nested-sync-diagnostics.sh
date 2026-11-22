#!/usr/bin/env bash
# Exercise only extracted diagnostic helpers; no live Incus or VM operations.
# Variables and command fakes are consumed by the extracted functions.
# shellcheck disable=SC2034,SC2317
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
eval "$(sed -n '/^sync_capacity_diagnostics() {/,/^}/p;
  /^sync_candidate() {/,/^}/p' "$ROOT/dev/e2e/nested-teardown-data-boundary.sh")"

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

for sync_rc in 0 23 124; do
  for diagnostic_rc in 0 19; do
    output="$(
      STATE=/fixture/state OUTER_INSTANCE=fixture OUTER_PROJECT=fixture
      FIXTURE_STARTED='2026-01-01 00:00:00 UTC'
      calls=()
      sync_timestamp='2026-01-01 01:02:03 UTC'
      date() {
        [ "$*" = '-u +%Y-%m-%d %H:%M:%S UTC' ] || fail 'sync timestamp format changed'
        printf '%s\n' "$sync_timestamp"
      }
      timeout() {
        [ "$1 $2" = '--kill-after=2 10' ] || fail 'diagnostic is not bounded'
        shift 2
        case "$*" in
          'df -B1 '*) calls+=(host-bytes) ;;
          'df -i '*) calls+=(host-inodes) ;;
          'du -sb '*) calls+=(source-size) ;;
          'incus exec '*journalctl*) calls+=(guest-kernel) ;;
          'incus exec '*) calls+=(guest-capacity) ;;
          "sudo -n journalctl -u incus --no-pager --lines=100 --since $sync_timestamp")
            calls+=(host-incus-journal) ;;
          *) fail "unexpected diagnostic: $*" ;;
        esac
        return "$diagnostic_rc"
      }
      yard() {
        [ "$*" = "sync $ROOT --name NestedBoundary --target yard --yes" ] \
          || fail 'sync arguments changed'
        calls+=(sync)
        printf 'archive sha256 fixture-digest bytes 12345\n'
        return "$sync_rc"
      }
      rc=0
      sync_candidate || rc=$?
      [ "$rc" = "$sync_rc" ] || fail 'diagnostics replaced the sync result'
      capacity='host-bytes host-inodes source-size guest-capacity'
      expected="$capacity sync"
      if [ "$sync_rc" != 0 ]; then
        expected+=" $capacity guest-kernel host-incus-journal"
      fi
      [ "${calls[*]}" = "$expected" ] \
        || fail "diagnostics or sync ran out of order: ${calls[*]}"
    )"
    [[ "$output" = *'archive sha256 fixture-digest bytes 12345'* ]] \
      || fail 'prepared archive identity was suppressed'
    [[ "$output" = *"candidate sync status=$sync_rc duration="* ]] \
      || fail 'sync result or timing was suppressed'
  done
done
printf 'PASS: candidate sync diagnostics preserve output and native exit without retry\n'
