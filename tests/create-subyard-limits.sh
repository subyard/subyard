#!/usr/bin/env bash
# Explicit resource limits converge on existing container and VM yards.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
cleanup() {
  local rc=$?
  if [ "$rc" -ne 0 ]; then
    [ ! -f "$TMP/output" ] || cat "$TMP/output" >&2
    [ ! -f "$TMP/incus.log" ] || cat "$TMP/incus.log" >&2
  fi
  rm -rf "$TMP"
  exit "$rc"
}
trap cleanup EXIT
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

# shellcheck source=tests/helpers/test-context.sh
. "$ROOT/tests/helpers/test-context.sh"
setup_test_context "$TMP"
export ASSUME_YES=1 SUBYARD_NO_AUDIT=1 SUBYARD_POWER_DESIRED=running
export PATH="$TMP/bin:$PATH"
export MOCK_STATE_DIR="$TMP/state" MOCK_INCUS_LOG="$TMP/incus.log"
export MOCK_STOP_EXIT=0 MOCK_LIMIT_SET_EXIT=0 MOCK_LIMIT_NOOP=0 MOCK_STATE_EXIT=0
export MOCK_DEVICE_LIST_PADDING=0
export LIMITS_CPU=2 LIMITS_MEMORY=4GiB
install -d -m 0700 "$TMP/bin" "$MOCK_STATE_DIR"

cat > "$TMP/bin/systemctl" <<'MOCK'
#!/usr/bin/env bash
case "$*" in
  'is-active NetworkManager') printf 'inactive\n'; exit 3 ;;
  'show incus.service -p Environment --value') ;;
  *) exit 90 ;;
esac
MOCK
cat > "$TMP/bin/ip" <<'MOCK'
#!/usr/bin/env bash
[ "$*" = '-4 route show default' ] || exit 90
MOCK
cat > "$TMP/bin/dpkg" <<'MOCK'
#!/bin/sh
exit 0
MOCK
cat > "$TMP/bin/incus" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$MOCK_INCUS_LOG"
case "${1:-} ${2:-} ${3:-}" in
  'info  ' | 'info yard --project' | 'project show subyard' | 'storage volume show') ;;
  'config device list')
    printf 'srv\nsubyard-e2e-routes\nkvm\n'
    if [ "$MOCK_DEVICE_LIST_PADDING" = 1 ]; then printf 'fixture-device-%05d\n' {1..10000}; fi
    ;;
  'config device get')
    case "$5:$6" in
      srv:pool) printf 'default\n' ;;
      srv:source) printf 'yard-srv\n' ;;
      srv:path) printf '/srv\n' ;;
      subyard-e2e-routes:type) printf 'disk\n' ;;
      subyard-e2e-routes:source) printf '%s/e2e/routes\n' "$SUBYARD_HOME" ;;
      subyard-e2e-routes:path) printf '/var/lib/subyard/e2e-routes\n' ;;
      subyard-e2e-routes:readonly) printf 'true\n' ;;
      *) exit 90 ;;
    esac ;;
  'config get yard')
    case "$4" in
      limits.cpu | limits.memory) cat "$MOCK_STATE_DIR/$4" ;;
      security.nesting) printf 'true\n' ;;
      security.syscalls.intercept.bpf | security.syscalls.intercept.bpf.devices) ;;
      *) exit 90 ;;
    esac ;;
  'config set yard')
    case "$4" in
      limits.cpu | limits.memory)
        [ "$(cat "$MOCK_STATE_DIR/power")" = STOPPED ] || exit 91
        [ "$MOCK_LIMIT_SET_EXIT" = 0 ] || exit "$MOCK_LIMIT_SET_EXIT"
        [ "$MOCK_LIMIT_NOOP" = 1 ] || printf '%s\n' "$5" > "$MOCK_STATE_DIR/$4"
        ;;
      *) exit 90 ;;
    esac ;;
  'config unset yard') ;;
  'list yard --project') cat "$MOCK_STATE_DIR/power"; exit "$MOCK_STATE_EXIT" ;;
  'stop yard --project')
    [ "$MOCK_STOP_EXIT" = 0 ] || exit "$MOCK_STOP_EXIT"
    printf 'STOPPED\n' > "$MOCK_STATE_DIR/power" ;;
  'start yard --project') printf 'RUNNING\n' > "$MOCK_STATE_DIR/power" ;;
  *) printf 'unexpected incus call: %s\n' "$*" >&2; exit 90 ;;
esac
MOCK
chmod 0700 "$TMP/bin/"{systemctl,ip,dpkg,incus}

reset_state() {
  printf '%s\n' "$1" > "$MOCK_STATE_DIR/power"
  printf '%s\n' "$2" > "$MOCK_STATE_DIR/limits.cpu"
  printf '%s\n' "$3" > "$MOCK_STATE_DIR/limits.memory"
  : > "$MOCK_INCUS_LOG"
}
run_create() {
  bash "$ROOT/scripts/03-create-subyard.sh" --yes > "$TMP/output" 2>&1
}
assert_limits() {
  [ "$(cat "$MOCK_STATE_DIR/limits.cpu")" = "$1" ] || fail "$YARD_KIND CPU limit did not converge"
  [ "$(cat "$MOCK_STATE_DIR/limits.memory")" = "$2" ] || fail "$YARD_KIND memory limit did not converge"
}
assert_no_limit_writes() {
  ! grep -Eq '^config (set|unset) yard limits\.' "$MOCK_INCUS_LOG" \
    || fail "$YARD_KIND wrote limits unnecessarily"
}

for YARD_KIND in container vm; do
  reset_state RUNNING 1 2GiB
  run_create
  assert_limits 2 4GiB
  [ "$(grep -c '^stop ' "$MOCK_INCUS_LOG")" = 1 ] || fail "$YARD_KIND did not stop once before applying limits"
  [ "$(grep -c '^start ' "$MOCK_INCUS_LOG")" = 1 ] || fail "$YARD_KIND did not restore running power"

  : > "$MOCK_INCUS_LOG"
  run_create
  assert_no_limit_writes
  ! grep -Eq '^(start|stop) ' "$MOCK_INCUS_LOG" || fail "$YARD_KIND restarted an already matching yard"

  # Change one limit without rewriting the matching one.
  reset_state RUNNING 1 4GiB
  run_create
  assert_limits 2 4GiB
  ! grep -q '^config set yard limits.memory ' "$MOCK_INCUS_LOG" || fail "$YARD_KIND rewrote matching memory"

  # The instance stage temporarily boots an intentionally stopped yard for later
  # provisioning. Finalization uses the same reconcile stop to restore its intent.
  SUBYARD_POWER_DESIRED=stopped
  reset_state STOPPED 1 2GiB
  run_create
  assert_limits 2 4GiB
  ! grep -q '^stop ' "$MOCK_INCUS_LOG" || fail "$YARD_KIND tried to stop an already stopped yard"
  bash "$ROOT/scripts/lifecycle-guard.sh" stop --reconcile > "$TMP/output" 2>&1
  [ "$(cat "$MOCK_STATE_DIR/power")" = STOPPED ] || fail "$YARD_KIND failed to restore stopped intent"
  SUBYARD_POWER_DESIRED=running

  # Empty and absent settings must leave existing (including inherited) values.
  reset_state RUNNING 3 6GiB
  LIMITS_CPU='' LIMITS_MEMORY='' run_create
  assert_limits 3 6GiB
  assert_no_limit_writes
  ! grep -Eq '^(start|stop) ' "$MOCK_INCUS_LOG" || fail "$YARD_KIND restarted for empty limits"
  (unset LIMITS_CPU LIMITS_MEMORY; run_create)
  assert_limits 3 6GiB
  assert_no_limit_writes

  # A failed lifecycle guard must abort before either limit is written.
  reset_state RUNNING 1 2GiB
  MOCK_STOP_EXIT=1
  if run_create; then fail "$YARD_KIND accepted a failed guarded stop"; fi
  assert_limits 1 2GiB
  assert_no_limit_writes
  MOCK_STOP_EXIT=0

  for state in STOPPED UNKNOWN; do
    reset_state "$state" 1 2GiB
    [ "$state" != STOPPED ] || MOCK_STATE_EXIT=1
    if run_create; then fail "$YARD_KIND accepted unreadable or unknown power state"; fi
    assert_limits 1 2GiB
    assert_no_limit_writes
    MOCK_STATE_EXIT=0
  done

  reset_state STOPPED 1 2GiB
  MOCK_LIMIT_SET_EXIT=1
  if run_create; then fail "$YARD_KIND accepted a failed limit write"; fi
  assert_limits 1 2GiB
  ! grep -q '^start ' "$MOCK_INCUS_LOG" || fail "$YARD_KIND booted after a failed limit write"
  MOCK_LIMIT_SET_EXIT=0

  reset_state STOPPED 1 2GiB
  MOCK_LIMIT_NOOP=1
  if run_create; then fail "$YARD_KIND accepted a nonconverged limit write"; fi
  assert_limits 1 2GiB
  ! grep -q '^start ' "$MOCK_INCUS_LOG" || fail "$YARD_KIND booted with nonconverged limits"
  MOCK_LIMIT_NOOP=0
done

# A long device list must not report an attached volume as missing when the
# producer is still writing after its first matching device.
reset_state RUNNING 2 4GiB
MOCK_DEVICE_LIST_PADDING=1
run_create
assert_no_limit_writes
! grep -Eq '^config device (add|remove) ' "$MOCK_INCUS_LOG" \
  || fail 'complete device-list inspection mutated an already attached device'

printf 'ok: existing container and VM limits converge safely and preserve empty settings\n'
