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
export MOCK_AGENT_READY_AFTER=1
export MOCK_AGENT_EXIT=0 MOCK_REPORTING_EXIT=0
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
    printf 'srv\nsubyard-e2e-routes\nkvm\neth0\n'
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
  'query /1.0/instances/yard?project=subyard ')
    printf '{"devices":{"eth0":{"type":"nic","network":"incusbr0","ipv4.address":"%s"}}}\n' "$(cat "$MOCK_STATE_DIR/ipv4-pin")" ;;
  'config device set')
    [ "$5" = eth0 ] && [ "$6" = ipv4.address=10.80.0.10 ] || exit 90
    printf '10.80.0.10\n' > "$MOCK_STATE_DIR/ipv4-pin" ;;
  'config get yard')
    case "$4" in
      limits.cpu | limits.memory) cat "$MOCK_STATE_DIR/$4" ;;
      security.nesting) printf 'true\n' ;;
      security.syscalls.intercept.bpf | security.syscalls.intercept.bpf.devices) ;;
      user.subyard.vm_cpu_weight) printf '%s\n' "${VM_CPU_WEIGHT:-}" ;;
      raw.qemu.conf) printf '[device "qemu_balloon"]\nfree-page-reporting = "on"\n' ;;
      raw.qemu*) ;;
      *) exit 90 ;;
    esac ;;
  'config set yard')
    case "$4" in
      limits.cpu | limits.memory)
        [ "$(cat "$MOCK_STATE_DIR/power")" = STOPPED ] || exit 91
        [ "$MOCK_LIMIT_SET_EXIT" = 0 ] || exit "$MOCK_LIMIT_SET_EXIT"
        [ "$MOCK_LIMIT_NOOP" = 1 ] || printf '%s\n' "$5" > "$MOCK_STATE_DIR/$4"
        ;;
      raw.qemu.conf) ;;
      *) exit 90 ;;
    esac ;;
  'config unset yard') ;;
  'profile get default') ;;
  'exec yard --project')
    case "$*" in
      *' -- true')
        printf '200\n' > "$MOCK_STATE_DIR/qemu-pid"
        attempts="$(cat "$MOCK_STATE_DIR/agent-attempts")"
        attempts=$((attempts + 1))
        printf '%s\n' "$attempts" > "$MOCK_STATE_DIR/agent-attempts"
        [ "$MOCK_AGENT_EXIT" = 0 ] || exit "$MOCK_AGENT_EXIT"
        [ "$attempts" -ge "$MOCK_AGENT_READY_AFTER" ]
        ;;
      *'/virtio_balloon/'*) printf '000001\n' ;;
      *'systemd-tmpfiles --create'*) cat >/dev/null; exit "$MOCK_REPORTING_EXIT" ;;
      *'rule-missing-or-unsafe'*) printf 'ready\n' ;;
      *'ip -4 -o address show'*) printf '2: eth0    inet 10.80.0.10/24 brd 10.80.0.255 scope global eth0\n' ;;
      *) exit 90 ;;
    esac ;;
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
  printf '0\n' > "$MOCK_STATE_DIR/agent-attempts"
  : > "$MOCK_STATE_DIR/ipv4-pin"
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

# Profile IPv4 pinning runs after this stage and needs the booted guest's NIC state,
# including when Free Page Reporting is disabled.
reset_state STOPPED 2 4GiB
YARD_KIND=vm VM_PIN_IPV4=1 VM_FREE_PAGE_REPORTING=0 MOCK_AGENT_READY_AFTER=2 run_create
[ "$(cat "$MOCK_STATE_DIR/agent-attempts")" = 2 ] \
  || fail 'VM IPv4 pinning stage completed before its agent was ready'
grep -Fq 'Phase 2 (instance) done.' "$TMP/output" || fail 'ready pinned VM did not complete'
reset_state STOPPED 2 4GiB
if YARD_KIND=vm VM_PIN_IPV4=1 VM_FREE_PAGE_REPORTING=0 MOCK_AGENT_READY_AFTER=9999 \
  SUBYARD_INCUS_AGENT_WAIT_TIMEOUT=1 run_create; then
  fail 'VM IPv4 pinning stage accepted an unavailable guest agent'
fi
grep -Fq 'VM agent did not become ready' "$TMP/output" || fail 'missing VM agent lost its diagnostic'
! grep -Fq 'Phase 2 (instance) done.' "$TMP/output" || fail 'failed pinned VM reported completion'

# Ordinary VMs reserve once in the instance stage; consumers never repin.
reset_state STOPPED 2 4GiB
YARD_KIND=vm VM_PIN_IPV4=0 VM_FREE_PAGE_REPORTING=0 run_create
[ "$(cat "$MOCK_STATE_DIR/ipv4-pin")" = 10.80.0.10 ] || fail 'ordinary VM was not pinned'
: > "$MOCK_INCUS_LOG"
YARD_KIND=vm VM_PIN_IPV4=0 VM_FREE_PAGE_REPORTING=0 run_create
! grep -q '^config device set yard eth0' "$MOCK_INCUS_LOG" || fail 'ordinary VM was repinned'
printf '10.80.0.11\n' > "$MOCK_STATE_DIR/ipv4-pin"
: > "$MOCK_INCUS_LOG"
if YARD_KIND=vm VM_PIN_IPV4=0 VM_FREE_PAGE_REPORTING=0 run_create; then
  fail 'divergent VM pin accepted'
fi
! grep -q '^config device set yard eth0' "$MOCK_INCUS_LOG" || fail 'divergent VM pin replaced'
# Cloud-image seeding replaces QEMU before the agent becomes ready. Both init
# and explicit start must apply CPU scheduling to that replacement process.
cat > "$TMP/bin/qemu-system-x86_64" <<'MOCK'
#!/usr/bin/env bash
case "$*" in
  --version) printf 'QEMU emulator version 8.2.0\n' ;;
  '-device virtio-balloon-pci,help') printf 'free-page-reporting\n' ;;
  *) exit 90 ;;
esac
MOCK
cat > "$TMP/bin/cpu-dispatcher" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
[ "$#" = 5 ] && [ "$1 $2 $3 $4" = '_vm-cpu apply subyard yard' ] || exit 90
pid="$(cat "$MOCK_STATE_DIR/qemu-pid")"
[ "$pid" != 200 ] || [ "$MOCK_CPU_REAPPLY_EXIT" = 0 ] || exit "$MOCK_CPU_REAPPLY_EXIT"
printf '%s\n' "$pid" > "$MOCK_STATE_DIR/cpu-applied-pid"
MOCK
chmod 0700 "$TMP/bin/"{qemu-system-x86_64,cpu-dispatcher}
export VM_CPU_WEIGHT=1000 VM_FREE_PAGE_REPORTING=1 MOCK_CPU_REAPPLY_EXIT=0
export SUBYARD_DISPATCHER_PATH="$TMP/bin/cpu-dispatcher"
run_start() { bash "$ROOT/scripts/lifecycle-guard.sh" start > "$TMP/output" 2>&1; }
for operation in run_create run_start; do
  for MOCK_CPU_REAPPLY_EXIT in 0 1; do
    reset_state STOPPED 2 4GiB
    printf '100\n' > "$MOCK_STATE_DIR/qemu-pid"
    if [ "$MOCK_CPU_REAPPLY_EXIT" = 0 ]; then
      "$operation" || fail "$operation failed after cloud-image reboot"
      [ "$(cat "$MOCK_STATE_DIR/cpu-applied-pid")" = 200 ] \
        || fail "$operation left replacement QEMU outside its CPU scope"
      [ "$(cat "$MOCK_STATE_DIR/power")" = RUNNING ] || fail "$operation did not leave the VM running"
      ! grep -q '^stop ' "$MOCK_INCUS_LOG" || fail "$operation stopped a successfully scoped VM"
    else
      if "$operation"; then fail "$operation accepted failed replacement CPU scheduling"; fi
      [ "$(cat "$MOCK_STATE_DIR/power")" = STOPPED ] || fail "$operation did not stop fail-closed"
    fi
    [ "$(grep -c '^start ' "$MOCK_INCUS_LOG")" = 1 ] || fail "$operation restarted the ready VM"
  done
done

# A failed agent must not leave replacement QEMU running without its CPU scope.
# Reporting preparation runs only after that scope has converged.
export SUBYARD_INCUS_AGENT_WAIT_TIMEOUT=1
MOCK_CPU_REAPPLY_EXIT=0
for operation in run_create run_start; do
  for fault in agent stop-refusal unweighted reporting; do
    [ "$fault:$operation" != reporting:run_start ] || continue
    VM_CPU_WEIGHT=1000 MOCK_AGENT_EXIT=0 MOCK_REPORTING_EXIT=0 MOCK_STOP_EXIT=0
    case "$fault" in
      agent) MOCK_AGENT_EXIT=1 ;;
      stop-refusal) MOCK_AGENT_EXIT=1 MOCK_STOP_EXIT=1 ;;
      unweighted) MOCK_AGENT_EXIT=1 VM_CPU_WEIGHT='' ;;
      reporting) MOCK_REPORTING_EXIT=1 ;;
    esac
    reset_state STOPPED 2 4GiB
    printf '100\n' > "$MOCK_STATE_DIR/qemu-pid"
    : > "$MOCK_STATE_DIR/cpu-applied-pid"
    if "$operation"; then fail "$operation accepted $fault guest failure"; fi
    diagnostic='agent did not become ready'
    [ "$fault" != reporting ] || diagnostic='VM Free Page Reporting guest configuration failed'
    grep -Fq "$diagnostic" "$TMP/output" || fail "$operation lost its $fault diagnostic"
    case "$fault" in
      agent)
        [ "$(cat "$MOCK_STATE_DIR/power")" = STOPPED ] || fail "$operation left unscoped QEMU running"
        grep -Fxq 'stop yard --project subyard --force' "$MOCK_INCUS_LOG" \
          || fail "$operation did not stop the exact VM fail-closed"
        ;;
      stop-refusal)
        [ "$(cat "$MOCK_STATE_DIR/power")" = RUNNING ] || fail 'stop refusal fixture did not retain the VM'
        grep -Fq 'FAILED to stop subyard/yard' "$TMP/output" || fail "$operation hid its failed stop"
        ;;
      unweighted|reporting)
        [ "$(cat "$MOCK_STATE_DIR/power")" = RUNNING ] || fail "$operation unexpectedly stopped $fault VM"
        ! grep -q '^stop ' "$MOCK_INCUS_LOG" || fail "$operation attempted an unnecessary stop"
        if [ "$fault" = reporting ]; then
          [ "$(cat "$MOCK_STATE_DIR/cpu-applied-pid")" = 200 ] || fail 'reporting failure left QEMU unscoped'
        fi
        ;;
    esac
    [ "$(grep -c '^start ' "$MOCK_INCUS_LOG")" = 1 ] || fail "$operation restarted the failed VM"
  done
done
VM_CPU_WEIGHT=1000 MOCK_AGENT_EXIT=0 MOCK_REPORTING_EXIT=0 MOCK_STOP_EXIT=0

VM_FREE_PAGE_REPORTING=0 MOCK_CPU_REAPPLY_EXIT=0
reset_state STOPPED 2 4GiB
printf '100\n' > "$MOCK_STATE_DIR/qemu-pid"
run_create
[ "$(cat "$MOCK_STATE_DIR/cpu-applied-pid")" = 200 ] \
  || fail 'CPU-only init did not wait for the replacement QEMU process'

printf 'ok: existing container and VM limits converge safely and preserve empty settings\n'
printf 'ok: VM IPv4 pinning waits for the guest agent and rejects readiness timeout\n'
