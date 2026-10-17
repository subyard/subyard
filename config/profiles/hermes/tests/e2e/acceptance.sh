#!/usr/bin/env bash
# Controller for Hermes' single-VM disposable acceptance.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." && pwd)"
slot=''
while [ "$#" -gt 0 ]; do
  case "$1" in
    --slot) [ "$#" -ge 2 ] || { printf 'acceptance: --slot needs a value\n' >&2; exit 2; }; slot="$2"; shift 2 ;;
    *) printf 'usage: acceptance.sh --slot N\n' >&2; exit 2 ;;
  esac
done
[[ "$slot" =~ ^[1-9][0-9]*$ ]] || { printf 'acceptance: --slot N is required\n' >&2; exit 2; }
# Use the existing lease lifecycle so private bytes can leave the VM before disposal.
# shellcheck source=dev/agent-e2e.sh
. "$ROOT/dev/agent-e2e.sh"
# shellcheck source=config/profiles/hermes/tests/e2e/failure-capture.sh
. "$ROOT/config/profiles/hermes/tests/e2e/failure-capture.sh"
VM_COUNT_REQUESTED=1
VM_COUNT=1
LEASE_PURPOSE=hermes-profile
set_requested_slot "$slot" --slot
install -d -m 0700 "$ROOT/.build/test-runs"
OUTPUT="$(mktemp -d "$ROOT/.build/test-runs/hermes.XXXXXX")"
chmod 0700 "$OUTPUT"
LOCAL_TEMP="$(mktemp -d /tmp/subyard-agent-e2e.XXXXXX)"
finish() {
  local original=$? capture_rc=0 cleanup_rc=0 started
  trap - EXIT INT TERM
  set +e
  phase_end "$original"
  if [ "$original" -ne 0 ] && [ -n "${GUEST_DIRS[1]:-}" ]; then
    hermes_retain_failure "${GUEST_DIRS[1]}/src/.build/hermes-failure" "$OUTPUT" || capture_rc=$?
  fi
  started=$SECONDS
  phase_start cleanup/release
  cleanup_guest 1 || cleanup_rc=3
  if [ -n "$LEASE_KEEPER_PID" ]; then
    kill "$LEASE_KEEPER_PID" >/dev/null 2>&1 || true
    wait "$LEASE_KEEPER_PID" >/dev/null 2>&1 || true
  fi
  find "$LOCAL_TEMP" -depth -delete >/dev/null 2>&1 || cleanup_rc=3
  release_lease || cleanup_rc=3
  phase_marker cleanup/release end "$((SECONDS - started))" "$cleanup_rc"
  install -m 0600 /dev/null "$OUTPUT/result"
  printf 'original_exit_code=%s\ncapture_exit_code=%s\ncleanup_exit_code=%s\n' \
    "$original" "$capture_rc" "$cleanup_rc" >"$OUTPUT/result"
  printf 'HERMES_RESULT original_exit_code=%s capture_exit_code=%s cleanup_exit_code=%s evidence=%s\n' \
    "$original" "$capture_rc" "$cleanup_rc" "$OUTPUT"
  [ "$original" -eq 0 ] || exit "$original"
  exit "$cleanup_rc"
}
trap finish EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
phase_start allocation
acquire_lease
phase_end 0
start_lease_keeper
bundle="$LOCAL_TEMP/worktree.tar.gz"
phase_start packing
build_bundle "$ROOT" "$bundle"
bundle_hash="$(sha256sum "$bundle" | awk '{print $1}')"
phase_end 0
printf 'Hermes worktree bundle ready (sha256=%s)\n' "$bundle_hash"
guest_rc=0
run_guest 1 "$bundle" "$bundle_hash" bash config/profiles/hermes/tests/e2e/hermes-profile.sh || guest_rc=$?
exit "$guest_rc"
