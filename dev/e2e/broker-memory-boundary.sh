#!/usr/bin/env bash
# Focused physical-memory mount acceptance on VM1 of a disposable allocation.
# Run: dev/agent-e2e.sh --slot N --type android-test --purpose broker-memory-boundary --vm 1 -- \
#        bash dev/e2e/broker-memory-boundary.sh
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
cd "$ROOT"
[ "${SUBYARD_E2E_VM:-}" = 1 ] && sudo -n test -f /run/subyard-e2e-lease.json \
  || { printf 'broker-memory-boundary: run on allocated VM1 through dev/agent-e2e.sh\n' >&2; exit 2; }
TOKEN="${1:-$(date +%s)$$}"

# Reuse the marker-owned owner bootstrap without executing its P0 dispatcher.
# ROOT stays this checkout's root when the helper is sourced through /dev/fd.
# shellcheck disable=SC1090
. <(sed '/^  case "$MODE" in/,$d; /^ROOT=/d' "$ROOT/dev/e2e/p0-guest.sh") memory-boundary "$TOKEN"

PROJECT=subyard-test-yard
INSTANCE=yard-test-yard
DEVICE=subyard-host-memory
TELEMETRY=/var/lib/subyard/host-meminfo
ENGINE=/usr/local/libexec/subyard/test-vms-inner
HOLDER_PID=''
PRESSURE_PID=''
STATE_PARENT=''
HOLDER_STARTED_PID=''
# Consumed by the sourced holder cleanup functions.
# shellcheck disable=SC2034
HOLDER_STOP_GRACE_SECONDS=30
# shellcheck disable=SC2034
HOLDER_KILL_GRACE_SECONDS=10
RUNNER="$ROOT/dev/agent-e2e.sh"
# shellcheck disable=SC2034
YARD=test-yard

# These functions own the keeper, release capability and bounded process cleanup.
# shellcheck disable=SC1090
. <(sed -n '/^hold_lease() (/,/^)/p; /^start_holder_child() {/,/^}/p; /^stop_holder_child() {/,/^}/p; /^recovery_monotonic_seconds() {/,/^}/p' \
  "$ROOT/dev/e2e/p0-broker-recovery.sh")

die() { printf 'broker-memory-boundary: %s\n' "$*" >&2; exit 2; }
outer_exec() { incus exec "$INSTANCE" --project "$PROJECT" -- "$@"; }
runtime_yard() { "$SUBYARD_HOME/runtime/current/bin/yard" "$@"; }

# An installer that grants incus-admin membership must resume this narrow fixture,
# never the shared helper's broader owner lane. Keep its existing ownership token.
reexec_with_incus_group() {
  local command
  printf -v command 'exec bash %q %q' "$ROOT/dev/e2e/broker-memory-boundary.sh" "$TOKEN"
  exec sg incus-admin -c "$command"
}

cleanup() {
  local rc=$?
  trap - EXIT INT TERM
  set +e
  if [ -n "$PRESSURE_PID" ]; then
    kill "$PRESSURE_PID" >/dev/null 2>&1
    wait "$PRESSURE_PID" >/dev/null 2>&1
  fi
  if [ -n "$HOLDER_PID" ]; then
    : > "$STATE_PARENT/holder.release"
    stop_holder_child "$HOLDER_PID" || rc=3
  fi
  # owner_cleanup performs exact marker checks before teardown and file removal.
  (exit "$rc")
  owner_cleanup
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

assert_owned_device() {
  [ "$(incus config get "$INSTANCE" user.subyard.host_memory --project "$PROJECT")" = v1 ] \
    || die 'host-memory ownership marker did not converge'
  local key want
  for key in type source path readonly; do
    case "$key" in type) want=disk ;; source) want=/proc/meminfo ;; path) want="$TELEMETRY" ;; readonly) want=true ;; esac
    [ "$(incus config device get "$INSTANCE" "$DEVICE" "$key" --project "$PROJECT")" = "$want" ] \
      || die "host-memory device $key did not converge"
  done
  outer_exec "$ENGINE" _test-vms-host-memory-check
  # The checker verifies the opened descriptor's procfs mount and readonly flag.
  # A write can still fail with EACCES before EROFS in an unprivileged container.
  outer_exec sh -eu -c '
    if output=$(LC_ALL=C sh -c '\''printf test >> "$1"'\'' _ "$1" 2>&1); then
      printf "owner meminfo unexpectedly accepted a write\n" >&2
      exit 1
    fi
    printf "  [ ok ] verified readonly owner meminfo rejected a write: %s\n" "$output"
  ' _ "$TELEMETRY" || die 'guest telemetry write check failed'
}

remove_owned_device() {
  assert_owned_device
  incus config device remove "$INSTANCE" "$DEVICE" --project "$PROJECT" >/dev/null
}

assert_unverified_source_rejected() {
  local status
  if outer_exec "$ENGINE" _test-vms-host-memory-check > "$STATE_PARENT/rejected-source.log" 2>&1; then
    die 'installed engine accepted an unverified physical-memory source'
  fi
  status="$(runtime_yard -Y test-yard test-vms status --json)"
  jq -e '.resources.memory == null and
    ((.resources.errors // []) | index("memory_telemetry_unavailable") != null)' \
    <<< "$status" >/dev/null \
    || die 'public status silently fell back to container memory for an unverified source'
}

reject_snapshot_source() {
  local digest
  # Incus may leave its empty, root-owned file mountpoint after hot removal.
  # Refuse every other pre-existing object before writing this marked snapshot.
  outer_exec sh -eu -c '
    test ! -L "$1"
    if test -e "$1"; then
      test -f "$1" && test ! -s "$1"
      test "$(stat -c %u:%h "$1")" = 0:1
      test "$(stat -f -c %T "$1")" != proc
      find "$1" -maxdepth 0 -type f -delete
    fi
    set -C
    { printf "# %s\n" "$2"; cat /proc/meminfo; } > "$1"
    chmod 0600 "$1"
  ' _ "$TELEMETRY" "$MARKER"
  digest="$(outer_exec sha256sum "$TELEMETRY" | awk '{print $1}')"
  assert_unverified_source_rejected
  outer_exec sh -eu -c '
    test ! -L "$1" && test -f "$1"
    test "$(head -n 1 "$1")" = "# $2"
    test "$(sha256sum "$1" | cut -d " " -f 1)" = "$3"
    find "$1" -maxdepth 0 -type f -delete
  ' _ "$TELEMETRY" "$MARKER" "$digest"
}

outer_identity() {
  local pid desired start
  pid="$(incus query "/1.0/instances/$INSTANCE/state?project=$PROJECT" | jq -er '.pid | select(. > 0)')"
  desired="$(incus config get "$INSTANCE" user.subyard.desired_power --project "$PROJECT")"
  start="$(outer_exec awk '{print $22}' /proc/1/stat)"
  [ "$desired" = running ] || die 'fixture desired power changed'
  printf '%s:%s:%s\n' "$pid" "$start" "$desired"
}

assert_outer_unchanged() {
  [ "$(outer_identity)" = "$OUTER_BEFORE" ] \
    || die 'telemetry reconciliation restarted the fixture yard or changed desired power'
}

sample_available() {
  local first mounted last low high tolerance=$((16 * 1024))
  first="$(awk '/^MemAvailable:/ {print $2}' /proc/meminfo)"
  mounted="$(outer_exec awk '/^MemAvailable:/ {print $2}' "$TELEMETRY")"
  last="$(awk '/^MemAvailable:/ {print $2}' /proc/meminfo)"
  [[ "$first:$mounted:$last" =~ ^[0-9]+:[0-9]+:[0-9]+$ ]] \
    || die 'missing numeric memory counters'
  low="$first"; high="$last"
  if [ "$first" -gt "$last" ]; then low="$last"; high="$first"; fi
  [ "$mounted" -ge "$((low - tolerance))" ] && [ "$mounted" -le "$((high + tolerance))" ] \
    || die 'mounted memory does not track its immediate physical owner'
  printf '%s\n' "$mounted"
}

live_pressure() {
  local before after restored attempt
  before="$(sample_available)"
  python3 - "$STATE_PARENT/pressure.ready" <<'PY' &
import pathlib, signal, sys
memory = bytearray(128 * 1024 * 1024)
for offset in range(0, len(memory), 4096):
    memory[offset] = 1
pathlib.Path(sys.argv[1]).write_text("ready\n")
signal.pause()
PY
  PRESSURE_PID=$!
  for ((attempt=0; attempt<30; attempt++)); do
    [ ! -f "$STATE_PARENT/pressure.ready" ] || break
    kill -0 "$PRESSURE_PID" || die 'bounded pressure process exited'
    sleep 1
  done
  [ -f "$STATE_PARENT/pressure.ready" ] || die 'bounded pressure process did not become ready'
  after="$(sample_available)"
  [ "$((before - after))" -ge $((64 * 1024)) ] \
    || die '128MiB physical pressure was not reflected in live telemetry'
  kill "$PRESSURE_PID"
  wait "$PRESSURE_PID" || true
  PRESSURE_PID=''
  restored="$(sample_available)"
  [ "$((restored - after))" -ge $((64 * 1024)) ] \
    || die 'released physical pressure was not reflected in live telemetry'
  printf '  [ ok ] live owner memory before_kib=%s pressured_kib=%s released_kib=%s\n' \
    "$before" "$after" "$restored"
}

held_identity() {
  # Hash only the stable lease fields inside L1; no capability or controller data
  # leaves the protected store, and no lease ID is printed in fixture evidence.
  outer_exec python3 -c '
import hashlib, json, pathlib
pool = json.loads(pathlib.Path("/var/lib/subyard/test-vms/leases.json").read_text())
slot, = [slot for slot in pool["slots"] if slot["slot_id"] == "slot-001"]
assert slot["state"] == "held" and slot["lease_id"]
fields = ("slot_id", "state", "lease_id", "resource_generation", "lease_epoch", "run", "purpose", "project", "base_fingerprint", "environment")
print(hashlib.sha256(json.dumps({key: slot[key] for key in fields}, sort_keys=True).encode()).hexdigest())
'
}

held_heartbeat() {
  outer_exec python3 -c '
import json, pathlib
pool = json.loads(pathlib.Path("/var/lib/subyard/test-vms/leases.json").read_text())
slot, = [slot for slot in pool["slots"] if slot["slot_id"] == "slot-001"]
print(slot["last_heartbeat_at"])
'
}

assert_held_unchanged() {
  kill -0 "$HOLDER_PID" || die 'held lease keeper exited'
  [ "$(held_identity)" = "$HELD_BEFORE" ] || die 'update changed the held lease identity or allocation'
  assert_outer_unchanged
  local guest
  for guest in 1 2; do
    [ "$(ssh -F "$HELD_CONFIG" -T -o ConnectTimeout=10 "e2e-vm-$guest" -- \
      cat /proc/sys/kernel/random/boot_id </dev/null)" = "${GUEST_BOOT[$guest]}" ] \
      || die "held guest $guest restarted or lost access"
  done
  assert_held_ram_released
}

assert_held_ram_released() {
  runtime_yard -Y test-yard test-vms status --json | jq -e '
    .resources.reserved_vm_memory_bytes == 0 and
    (.resources.slots | length == 1) and
    .resources.slots[0].memory_commitment_bytes == 0 and
    .resources.slots[0].remaining_memory_growth_bytes == 0' >/dev/null \
    || die 'held allocation retained a startup RAM promise'
}

holder_failure() {
  # Preserve bounded controller diagnostics before marker-owned cleanup. Never
  # print the private lease/config store or arbitrary provisioning output.
  if [ -f "$STATE_PARENT/holder.log" ]; then
    awk '
      /^agent-e2e: retryable capacity refusal for slot-[0-9][0-9][0-9]: resource=(memory|disk); retry after resources become available$/ { print; next }
      /^agent-e2e: broker capability probe failed$/ { print; next }
      /^agent-e2e: test environment unavailable: no pinned host key for / { print "holder has no pinned SSH host key"; next }
      /^agent-e2e: (cannot canonicalize|E2E runner must execute|managed workspace|project metadata)/ { print "holder workspace attribution failed"; next }
      /Permission denied \(publickey\)/ { print "holder SSH authentication failed"; next }
      /^ssh:/ { print "holder SSH transport failed"; next }
      /: unbound variable$/ { print "holder shell variable was unset" }
    ' \
      "$STATE_PARENT/holder.log" >&2
  fi
  die "$1"
}

printf '  [ .. ] preparing marker-owned broker on disposable VM1\n'
prepare_owner_go_cache
STATE_PARENT="$OWNER_ROOT/memory-boundary"
p0_capacity_prepare_subtree "$STATE_PARENT"
YARD_BUILD_VERSION="$P0_OWNER_VERSION" dev/build-engine.sh --force >/dev/null
ensure_owner_incus
# Consumed by the sourced marker-owned cleanup and registration functions.
# shellcheck disable=SC2034
OWNER_BASELINE_IMAGES="$(incus image list --project default --format csv -c f)"
# shellcheck disable=SC2034
OWNER_BASELINE_CAPTURED=1
ensure_owner_base_image
# shellcheck disable=SC2034
OWNER_DIAGNOSTIC_VM_MEMORY=512MiB
install_owner_runtime
prepare_broker_recovery_update
prepare_owner_image_cache_project "$PROJECT"
write_owner_registration test-yard test-vms 2224 1
REGISTRATION="$OWNER_YARD_DIR/test-yard.env"
if [ -e "$OWNER_YARD_DIR/test-yard/config.env" ]; then
  REGISTRATION="$OWNER_YARD_DIR/test-yard/config.env"
fi
cat >> "$REGISTRATION" <<'CONFIG'
# Small budgets belong exclusively to this disposable physical-boundary fixture.
E2E_MEMORY_RESERVE=128MiB
E2E_VM_OVERHEAD=512MiB
E2E_DISK_RESERVE=512MiB
E2E_CACHE_BUDGET=4GiB
CONFIG
p0_retry_init_after_plan_stale ./bin/yard -Y test-yard init --yes
assert_owned_device
OUTER_BEFORE="$(outer_identity)"
remove_owned_device
assert_unverified_source_rejected
reject_snapshot_source
printf '  [ ok ] missing and ordinary snapshot sources fail closed in engine and public status\n'
p0_retry_init_after_plan_stale ./bin/yard -Y test-yard init --yes
assert_owned_device
assert_outer_unchanged
live_pressure
printf '  [ ok ] init and live hotplug preserve yard power and process identity\n'

# Only this nested, marker-owned fixture is restarted, before any nested lease.
runtime_yard -Y test-yard stop --yes >/dev/null
runtime_yard -Y test-yard start --yes >/dev/null
wait_for_outer_default_route "$INSTANCE" "$PROJECT"
assert_owned_device
OUTER_BEFORE="$(outer_identity)"
printf '  [ ok ] telemetry survives fixture container boot\n'

# Free compiler cache before reserving the nested pair's disks on the allocated VM.
p0_capacity_remove_build_cache
nested_free="$(outer_exec df -B1 --output=avail /srv/incus-e2e/storage | awk 'NR == 2 {print $1}')"
[[ "$nested_free" =~ ^[0-9]+$ ]] || die 'nested disk headroom unavailable'
# A fresh base builder reserves 3x the 10GiB guest disk, plus the fixture reserve.
nested_required=$((30 * 1024 * 1024 * 1024 + 512 * 1024 * 1024))
[ "$nested_free" -ge "$nested_required" ] \
  || die "fixture disk too small: available_bytes=$nested_free required_bytes=$nested_required"
printf '  [ ok ] cold nested builder disk headroom available_bytes=%s required_bytes=%s\n' \
  "$nested_free" "$nested_required"
export SUBYARD_E2E_TEST_MODE=1
export SUBYARD_E2E_WORKSPACES_ROOT
SUBYARD_E2E_WORKSPACES_ROOT="$(dirname "$(dirname "$ROOT")")"
export SUBYARD_E2E_PROJECT_LABEL=Subyard-2
export SUBYARD_E2E_PROJECT_YARD=test-yard
export SUBYARD_E2E_YARD=test-yard
SUBYARD_E2E_STATE_DIR="$STATE_PARENT/holder" "$RUNNER" --yard test-yard --prepare >/dev/null
start_holder_child hold_lease holder memory-boundary slot-001 > "$STATE_PARENT/holder.log" 2>&1
HOLDER_PID="$HOLDER_STARTED_PID"
for ((attempt=0; attempt<1800; attempt++)); do
  [ ! -s "$STATE_PARENT/holder.ready" ] || break
  kill -0 "$HOLDER_PID" 2>/dev/null || holder_failure 'nested holder failed before publishing readiness'
  [ "$((attempt % 60))" -ne 0 ] || printf '  [ .. ] preparing nested pair (%ss elapsed)\n' "$attempt"
  sleep 1
done
[ -s "$STATE_PARENT/holder.ready" ] || holder_failure 'nested holder did not become ready within 30 minutes'
IFS=$'\t' read -r _slot HELD_CONFIG _project _run _purpose _generation _base \
  < "$STATE_PARENT/holder.ready"
HELD_BEFORE="$(held_identity)"
declare -A GUEST_BOOT=()
for guest in 1 2; do
  GUEST_BOOT[$guest]="$(ssh -F "$HELD_CONFIG" -T -o ConnectTimeout=10 "e2e-vm-$guest" -- \
    cat /proc/sys/kernel/random/boot_id </dev/null)"
done
assert_held_ram_released
printf '  [ ok ] ready guests release startup RAM promises to host accounting\n'

release="$(dirname "$P0_BROKER_RECOVERY_UPDATE_ARTIFACT")"
runtime_root="$SUBYARD_HOME/runtime"
old_hash="$(outer_exec sha256sum "$ENGINE" | awk '{print $1}')"
remove_owned_device
YARD_RELEASE_BASE_URL="file://$release" runtime_yard update \
  --runtime-root "$runtime_root" --version p0-broker-recovery-update --check >/dev/null
YARD_RELEASE_BASE_URL="file://$release" runtime_yard update \
  --runtime-root "$runtime_root" --version p0-broker-recovery-update --yes >/dev/null
assert_owned_device
assert_held_unchanged
new_hash="$(outer_exec sha256sum "$ENGINE" | awk '{print $1}')"
expected_hash="$(sha256sum "$runtime_root/current/bin/yard-engine" | awk '{print $1}')"
[ "$new_hash" = "$expected_hash" ] && [ "$new_hash" != "$old_hash" ] \
  || die 'ordinary update did not install the distinct candidate engine'
printf '  [ ok ] ordinary update restores telemetry and preserves held lease, guests and yard\n'
runtime_yard migrate --check --json | jq -e '.outcome.status == "ready"' >/dev/null \
  || die 'ordinary update did not reach release readiness'

# Keep the completed release ready: removing its required device creates a new
# repair transaction, whose protected caller correctly pins the current target.
runtime_yard update --runtime-root "$runtime_root" --rollback --yes >/dev/null
assert_owned_device
assert_held_unchanged
[ "$(outer_exec sha256sum "$ENGINE" | awk '{print $1}')" = "$old_hash" ] \
  || die 'ordinary rollback did not restore the original engine'
runtime_yard migrate --check --json | jq -e '.outcome.status == "ready"' >/dev/null \
  || die 'ordinary rollback did not reach release readiness'
printf '  [ ok ] ordinary rollback preserves telemetry, held lease, guests and yard\n'

HEARTBEAT_BEFORE="$(held_heartbeat)"
for ((attempt=0; attempt<45; attempt++)); do
  [ "$(held_heartbeat)" = "$HEARTBEAT_BEFORE" ] || break
  kill -0 "$HOLDER_PID" || die 'held keeper exited before renewing the lease'
  sleep 2
done
[ "$(held_heartbeat)" != "$HEARTBEAT_BEFORE" ] \
  || die 'held heartbeat did not advance across update and rollback'
assert_held_unchanged
printf '  [ ok ] held lease renewal continues after update and rollback\n'

: > "$STATE_PARENT/holder.release"
wait "$HOLDER_PID" || die 'nested lease did not release successfully'
HOLDER_PID=''
printf 'evidence: original_engine_sha256=%s candidate_engine_sha256=%s held_identity_sha256=%s\n' \
  "$old_hash" "$new_hash" "$HELD_BEFORE"
printf 'ok: broker physical-memory mount, live counters, boot and held update/rollback\n'
