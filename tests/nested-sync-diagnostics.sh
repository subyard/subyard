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
      sync_calls=0 diagnostic_calls=0 kernel_calls=0
      timeout() {
        [ "$1 $2" = '--kill-after=2 10' ] || fail 'diagnostic is not bounded'
        diagnostic_calls=$((diagnostic_calls + 1))
        case "$*" in *journalctl*) kernel_calls=$((kernel_calls + 1)) ;; esac
        return "$diagnostic_rc"
      }
      yard() {
        [ "$*" = "sync $ROOT --name NestedBoundary --target yard --yes" ] \
          || fail 'sync arguments changed'
        sync_calls=$((sync_calls + 1))
        printf 'archive sha256 fixture-digest bytes 12345\n'
        return "$sync_rc"
      }
      rc=0
      sync_candidate || rc=$?
      [ "$rc" = "$sync_rc" ] || fail 'diagnostics replaced the sync result'
      [ "$sync_calls" = 1 ] || fail 'sync was retried'
      expected_diagnostics=4 expected_kernel=0
      if [ "$sync_rc" != 0 ]; then expected_diagnostics=9; expected_kernel=1; fi
      [ "$diagnostic_calls:$kernel_calls" = "$expected_diagnostics:$expected_kernel" ] \
        || fail 'capacity or failure diagnostics were skipped'
    )"
    [[ "$output" = *'archive sha256 fixture-digest bytes 12345'* ]] \
      || fail 'prepared archive identity was suppressed'
    [[ "$output" = *"candidate sync status=$sync_rc duration="* ]] \
      || fail 'sync result or timing was suppressed'
  done
done
printf 'PASS: candidate sync diagnostics preserve output and native exit without retry\n'
