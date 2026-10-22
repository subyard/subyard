#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
PROJECT=subyard-p0-real-incus
MARKER=agent-e2e-p0
CONTAINER_IMAGE="${P0_REAL_INCUS_CONTAINER_IMAGE:-images:debian/13/cloud}"
VM_IMAGE="${P0_REAL_INCUS_VM_IMAGE:-images:debian/13/cloud}"
CONTAINER_CACHE_ALIAS="${P0_REAL_INCUS_CONTAINER_CACHE_ALIAS:-subyard-e2e-debian-13-cloud-container}"
VM_CACHE_ALIAS="${P0_REAL_INCUS_VM_CACHE_ALIAS:-subyard-e2e-debian-13-cloud-vm}"
TMP=''

die() { printf 'p0-real-incus: %s\n' "$*" >&2; exit 2; }
CLEANUP_ONLY=0
PREPARE_ONLY=0
case "$#:${1:-}" in
  0:) ;;
  1:--cleanup-only) CLEANUP_ONLY=1 ;;
  1:--prepare-only) PREPARE_ONLY=1 ;;
  *) die 'usage: p0-real-incus.sh [--cleanup-only|--prepare-only]' ;;
esac
# Incus create/init/launch may consume YAML from stdin. The P0 lane is reached through SSH, so an
# inherited non-TTY stream can stay open forever after the operation itself succeeds.
real_incus() { timeout --foreground "${P0_REAL_INCUS_COMMAND_TIMEOUT:-900}" incus "$@" </dev/null; }
real_incus_quiet() { real_incus "$@" >/dev/null; }
project_exists() { real_incus project show "$PROJECT" >/dev/null 2>&1; }

prepare_container_image() {
  local cache_info
  if ! real_incus image info "$CONTAINER_CACHE_ALIAS" --project default >/dev/null 2>&1; then
    run_with_progress 'caching the P0 container image' \
      real_incus_quiet image copy "$CONTAINER_IMAGE" local: \
        --alias "$CONTAINER_CACHE_ALIAS" --project default || return
  fi
  cache_info="$(real_incus image info "$CONTAINER_CACHE_ALIAS" --project default)" \
    || die "container image alias $CONTAINER_CACHE_ALIAS is unavailable after preparation"
  grep -Fqx 'Type: container' <<<"$cache_info" \
    || die "provisioned image alias $CONTAINER_CACHE_ALIAS is not a container image"
}

real_incus_observe() {
  P0_REAL_INCUS_COMMAND_TIMEOUT="${P0_REAL_INCUS_OBSERVE_TIMEOUT:-30}" real_incus "$@"
}

p0_monotonic_seconds() {
  local uptime
  read -r uptime _ < /proc/uptime
  printf '%s\n' "${uptime%%.*}"
}

cleanup_real_incus() {
  local deadline="$1" now remaining
  shift
  now="$(p0_monotonic_seconds)"
  remaining=$((deadline - now))
  [ "$remaining" -gt 0 ] || return 124
  if [ "$remaining" -lt "${P0_REAL_INCUS_OBSERVE_TIMEOUT:-30}" ]; then
    P0_REAL_INCUS_COMMAND_TIMEOUT="$remaining" real_incus "$@"
  else
    real_incus_observe "$@"
  fi
}

cleanup_sleep() {
  local deadline="$1" requested="$2" now remaining delay
  [ "$requested" -gt 0 ] || return 0
  now="$(p0_monotonic_seconds)"
  remaining=$((deadline - now))
  [ "$remaining" -gt 0 ] || return 124
  delay="$requested"
  [ "$delay" -le "$remaining" ] || delay="$remaining"
  sleep "$delay"
}

active_instance_operation_ids() {
  local deadline="$1" name="$2" operations resource
  resource="/1.0/instances/$name?project=$PROJECT"
  if operations="$(
    cleanup_real_incus "$deadline" operation list --project "$PROJECT" --format json
  )"; then
    :
  else
    return
  fi
  jq -r --arg resource "$resource" '
    if type != "array" or any(.[];
      type != "object" or
      (.id | type) != "string" or .id == "" or
      (.status_code | type) != "number" or .status_code < 100 or
      (.resources | type) != "object" or
      ((.resources.instances? // []) | type) != "array" or
      ((.resources.instances? // []) | any(.[]; type != "string")))
    then error("unexpected Incus operation inventory")
    else
      .[]
      | select(.status_code < 200)
      | select(any((.resources.instances? // [])[]; . == $resource))
      | .id
    end
  ' <<<"$operations"
}

delete_marked_instance() {
  local name="$1" instance_marker instance_names operation_ids project_marker now
  local delete_attempt=0
  local wait_seconds="${P0_REAL_INCUS_CLEANUP_WAIT_SECONDS:-900}"
  local poll_seconds="${P0_REAL_INCUS_CLEANUP_POLL_SECONDS:-5}"
  local delete_retry_seconds="${P0_REAL_INCUS_DELETE_RETRY_SECONDS:-2}"
  local deadline
  [[ "$wait_seconds" =~ ^[1-9][0-9]*$ ]] && [ "$wait_seconds" -le 900 ] \
    || die "invalid real-Incus cleanup wait: $wait_seconds"
  [[ "$poll_seconds" =~ ^[0-9]+$ ]] && [ "$poll_seconds" -le 30 ] \
    || die "invalid real-Incus cleanup poll interval: $poll_seconds"
  [[ "$delete_retry_seconds" =~ ^[0-9]+$ ]] && [ "$delete_retry_seconds" -le 30 ] \
    || die "invalid real-Incus delete retry interval: $delete_retry_seconds"
  now="$(p0_monotonic_seconds)"
  deadline=$((now + wait_seconds))
  if project_marker="$(
    cleanup_real_incus "$deadline" project get "$PROJECT" user.subyard.p0
  )"; then
    [ "$project_marker" = "$MARKER" ] \
      || die "refusing to clean unmarked project $PROJECT"
  else
    return
  fi
  while :; do
    now="$(p0_monotonic_seconds)"
    [ "$now" -lt "$deadline" ] \
      || die "could not settle marked instance $name within ${wait_seconds}s"
    if instance_names="$(
      cleanup_real_incus "$deadline" list "$name" --project "$PROJECT" -f csv -c n
    )"; then
      case "$instance_names" in
        '') ;;
        "$name") ;;
        *) return 2 ;;
      esac
    else
      return
    fi
    if operation_ids="$(active_instance_operation_ids "$deadline" "$name")"; then
      :
    else
      return
    fi
    if [ -n "$operation_ids" ]; then
      printf '  [warn] waiting for active Incus operation on %s: %s\n' \
        "$name" "$(tr '\n' ' ' <<<"$operation_ids")"
      cleanup_sleep "$deadline" "$poll_seconds" || return
      continue
    fi
    [ -n "$instance_names" ] || return 0
    if instance_marker="$(
      cleanup_real_incus "$deadline" config get "$name" user.subyard.p0 --project "$PROJECT"
    )"; then
      [ "$instance_marker" = "$MARKER" ] \
        || die "refusing to delete unmarked instance $name"
    else
      return
    fi
    delete_attempt=$((delete_attempt + 1))
    if cleanup_real_incus "$deadline" delete \
      "$name" --project "$PROJECT" --force >/dev/null; then
      continue
    fi
    [ "$delete_attempt" -lt 3 ] || die "could not delete marked instance $name after 3 attempts"
    # A failed Incus VM stop can leave AppArmor teardown/reload briefly in
    # flight. Keep recovery bounded and revalidate ownership before every try.
    printf '  [warn] cleanup delete of %s failed; retrying (%s/3)\n' \
      "$name" "$((delete_attempt + 1))"
    cleanup_sleep "$deadline" "$delete_retry_seconds" || return
  done
}

run_with_progress() {
  local label="$1" interval="${E2E_PROGRESS_INTERVAL:-10}" ticker rc started=$SECONDS
  shift
  printf '  [ .. ] %s\n' "$label"
  (
    while sleep "$interval"; do
      printf '  [ .. ] %s (still working, %ss elapsed)\n' "$label" "$((SECONDS - started))"
    done
  ) &
  ticker=$!
  if "$@"; then rc=0; else rc=$?; fi
  kill "$ticker" 2>/dev/null || true
  wait "$ticker" 2>/dev/null || true
  return "$rc"
}

launch_with_retry() {
  local name="$1" label="$2" attempt=1
  shift 2
  while ! run_with_progress "$label" "$@"; do
    real_incus_observe info "$name" --project "$PROJECT" --show-log >&2 || true
    [ "$attempt" -lt 3 ] || return 1
    printf '  [warn] %s failed; cleaning the marked instance and retrying (%s/3)\n' \
      "$label" "$((attempt + 1))"
    delete_marked_instance "$name"
    attempt=$((attempt + 1))
  done
}

cleanup() {
  local name inventory
  inventory="$(real_incus_observe project list --format csv -c n)" \
    || die 'cannot inspect real-Incus cleanup inventory'
  if grep -Fxq "$PROJECT" <<<"$inventory"; then
    [ "$(real_incus project get "$PROJECT" user.subyard.p0 2>/dev/null)" = "$MARKER" ] \
      || die "refusing to clean unmarked project $PROJECT"
    for name in p0-container p0-vm; do delete_marked_instance "$name"; done
    [ -z "$(real_incus list --project "$PROJECT" -f csv -c n)" ] \
      || die "unexpected instance remains in $PROJECT"
    real_incus project delete "$PROJECT" >/dev/null
  fi
  if [ -n "$TMP" ] && [[ "$TMP" = /tmp/subyard-p0-incus.* ]] && [ -d "$TMP" ]; then
    find "$TMP" -depth -delete
  fi
}
[ -n "${SUBYARD_E2E_VM:-}" ] || die 'run through dev/agent-e2e.sh'
for command in incus jq sudo; do command -v "$command" >/dev/null || die "$command is required"; done
sudo -n true || die 'passwordless sudo is required in a disposable test VM'
[ -S /var/lib/incus/unix.socket ] || die 'Incus socket is unavailable'
if [ "$PREPARE_ONLY" = 1 ]; then
  prepare_container_image
  printf 'ok: P0 container image prepared\n'
  exit 0
fi
if [ "$CLEANUP_ONLY" = 1 ]; then
  cleanup
  exit 0
fi
command -v go >/dev/null || die 'go is required'
trap 'trap - EXIT; trap "" INT TERM HUP; cleanup' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP
project_exists && cleanup
if cache_info="$(real_incus image info "$CONTAINER_CACHE_ALIAS" --project default 2>/dev/null)"; then
  printf '%s\n' "$cache_info" | grep -Fqx 'Type: container' \
    || die "provisioned image alias $CONTAINER_CACHE_ALIAS is not a container image"
  CONTAINER_IMAGE="$CONTAINER_CACHE_ALIAS"
  printf '  [ ok ] using provisioned real-Incus container image %s\n' "$CONTAINER_CACHE_ALIAS"
fi
if cache_info="$(real_incus image info "$VM_CACHE_ALIAS" --project default 2>/dev/null)"; then
  printf '%s\n' "$cache_info" | grep -Fqx 'Type: virtual-machine' \
    || die "provisioned image alias $VM_CACHE_ALIAS is not a VM image"
  VM_IMAGE="$VM_CACHE_ALIAS"
  printf '  [ ok ] using provisioned real-Incus VM image %s\n' "$VM_CACHE_ALIAS"
fi

real_incus project create "$PROJECT" \
  -c features.images=false -c features.profiles=false -c features.storage.volumes=false \
  -c user.subyard.p0="$MARKER" >/dev/null
launch_real_container() {
  real_incus_quiet launch "$CONTAINER_IMAGE" p0-container --project "$PROJECT" --storage default \
    -c user.subyard.p0="$MARKER"
}
launch_with_retry p0-container "launching real Incus container (first use may download an image)" \
  launch_real_container
if ! real_incus image info "$CONTAINER_CACHE_ALIAS" --project default >/dev/null 2>&1; then
  container_fingerprint="$(real_incus config get p0-container volatile.base_image --project "$PROJECT")"
  [[ "$container_fingerprint" =~ ^[0-9a-f]{64}$ ]] \
    || die 'real-Incus container base image fingerprint is invalid'
  real_incus image alias create "$CONTAINER_CACHE_ALIAS" "$container_fingerprint" --project default
  printf '  [ ok ] retained test-owned container image alias %s\n' "$CONTAINER_CACHE_ALIAS"
fi
launch_real_vm() {
  real_incus_quiet launch "$VM_IMAGE" p0-vm --vm --project "$PROJECT" \
    --storage default \
    -c limits.cpu=1 -c limits.memory=1GiB -c security.secureboot=false \
    -c user.subyard.p0="$MARKER" \
    -d root,size=5GiB
}
launch_with_retry p0-vm "launching real Incus VM (a clean allocation may download an image)" \
  launch_real_vm

qemu_process_identity() {
  local name="$1" marker instance state pid comm stat start
  local -a fields
  marker="$(real_incus_observe project get "$PROJECT" user.subyard.p0)" || return
  [ "$marker" = "$MARKER" ] || return 2
  instance="$(real_incus_observe query "/1.0/instances/$name?project=$PROJECT")" || return
  jq -e --arg name "$name" --arg marker "$MARKER" '
    .name == $name and .type == "virtual-machine" and .config["user.subyard.p0"] == $marker
  ' <<<"$instance" >/dev/null || return 2
  state="$(real_incus_observe query "/1.0/instances/$name/state?project=$PROJECT")" || return
  pid="$(jq -er 'select(.status == "Running") | .pid
    | select(type == "number" and . > 0 and floor == .)' <<<"$state")" || return 2
  [[ "$pid" =~ ^[1-9][0-9]*$ ]] || return 2
  comm="$(timeout --foreground 5 sudo -n cat "/proc/$pid/comm")" || return
  [ "$comm" = qemu-system-x86 ] || return 2
  stat="$(timeout --foreground 5 sudo -n cat "/proc/$pid/stat")" || return
  read -r -a fields <<<"${stat##*) }"
  start="${fields[19]:-}"
  [[ "$start" =~ ^[1-9][0-9]*$ ]] || return 2
  printf '%s %s\n' "$pid" "$start"
}

capture_qemu_facts() {
  local name="$1" before after pid start targets dev_kvm kvm_vm kvm_vcpu
  before="$(qemu_process_identity "$name")" || return
  read -r pid start <<<"$before"
  # Read only this API-selected process. Never expose other descriptor targets or argv.
  targets="$(timeout --foreground 5 sudo -n find "/proc/$pid/fd" \
    -maxdepth 1 -type l -printf '%l\n')" || return
  after="$(qemu_process_identity "$name")" || return
  [ "$before" = "$after" ] || return 2
  dev_kvm="$(grep -Fxc /dev/kvm <<<"$targets" || true)"
  kvm_vm="$(grep -Exc 'anon_inode:(kvm-vm|\[kvm-vm\])' <<<"$targets" || true)"
  kvm_vcpu="$(grep -Exc 'anon_inode:(kvm-vcpu|\[kvm-vcpu\])' <<<"$targets" || true)"
  jq -nc --argjson pid "$pid" --argjson start "$start" \
    --argjson device "$dev_kvm" --argjson vm "$kvm_vm" --argjson vcpu "$kvm_vcpu" '
    {pid: $pid, start_ticks: $start, comm: "qemu-system-x86",
     kvm_fd_counts: {dev_kvm: $device, kvm_vm: $vm, kvm_vcpu: $vcpu},
     acceleration: (if $vm > 0 or $vcpu > 0 then "kvm-fds-present" else "unknown" end)}
  '
}

emit_vga_screenshot() {
  python3 - "$1" "$2" <<'PY'
import base64
import hashlib
import json
import os
import stat
import struct
import sys

with open(sys.argv[1], 'rb') as stream:
    info = os.fstat(stream.fileno())
    assert stat.S_ISREG(info.st_mode) and info.st_uid == os.getuid()
    assert stat.S_IMODE(info.st_mode) == 0o600 and info.st_nlink == 1
    data = stream.read(2 * 1024 * 1024 + 1)
assert 45 <= len(data) <= 2 * 1024 * 1024
assert data[:16] == b'\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR'
assert data[-12:] == b'\x00\x00\x00\x00IEND\xaeB`\x82'
width, height = struct.unpack('>II', data[16:24])
assert 0 < width <= 4096 and 0 < height <= 4096
encoded = base64.b64encode(data).decode('ascii')
print('P0_INCUS_VGA ' + json.dumps({
    'name': sys.argv[2], 'bytes': len(data), 'sha256': hashlib.sha256(data).hexdigest(),
    'width': width, 'height': height, 'chunks': (len(encoded) + 2047) // 2048,
}, separators=(',', ':')))
for index, offset in enumerate(range(0, len(encoded), 2048)):
    print('P0_INCUS_VGA_DATA ' + json.dumps({
        'name': sys.argv[2], 'index': index, 'base64': encoded[offset:offset + 2048],
    }, separators=(',', ':')))
PY
}

capture_vga_screenshot() {
  local name="$1" before after status
  local image="$TMP/$name-vga.png" error="$TMP/$name-vga.stderr"
  before="$(qemu_process_identity "$name")" || return
  : > "$image"
  : > "$error"
  chmod 0600 "$image" "$error"
  if real_incus_observe query "/1.0/instances/$name/console?project=$PROJECT&type=vga" \
    >"$image" 2>"$error"; then status=0; else status=$?; fi
  printf 'P0_INCUS_DIAGNOSTIC instance=%s query=vga exit_status=%s\n' \
    "$name" "$status" >&2
  [ "$status" = 0 ] || return "$status"
  after="$(qemu_process_identity "$name")" || return
  [ "$before" = "$after" ] || return 2
  emit_vga_screenshot "$image" "$name"
}

wait_ready() {
  local name="$1" kind="$2" state='' replaced=0
  local wait_timeout=240 deadline
  local probe_status=0 probe_error="$TMP/$name-agent.stderr" detail status bytes limit
  local query_output="$TMP/$name-instance.json" query_error="$TMP/$name-instance.stderr"
  local restart_grace="${P0_REAL_INCUS_RESTART_GRACE_ATTEMPTS:-30}"
  : > "$probe_error"
  chmod 0600 "$probe_error"
  # A cloud VM's first boot may reboot before its agent starts, including under nested KVM.
  [ "$kind" != virtual-machine ] || wait_timeout=600
  deadline=$((SECONDS + wait_timeout))
  printf '  [ .. ] waiting for %s\n' "$name"
  while [ "$SECONDS" -lt "$deadline" ]; do
    if real_incus_observe exec "$name" --project "$PROJECT" -- true \
      >/dev/null 2>"$probe_error"; then
      return 0
    else
      probe_status=$?
    fi
    state="$(real_incus_observe list "$name" --project "$PROJECT" --format csv -c s)"
    if [ "$state" = STOPPED ]; then
      printf '  [ .. ] %s is stopped; waiting for a bounded first-boot restart\n' "$name"
      for _ in $(seq 1 "$restart_grace"); do
        [ "$SECONDS" -lt "$deadline" ] || break
        sleep 1
        if real_incus_observe exec "$name" --project "$PROJECT" -- true \
          >/dev/null 2>"$probe_error"; then
          return 0
        else
          probe_status=$?
        fi
        state="$(real_incus_observe list "$name" --project "$PROJECT" --format csv -c s)"
        [ "$state" = STOPPED ] || break
      done
      [ "$SECONDS" -lt "$deadline" ] || break
      [ "$state" = STOPPED ] || continue
      if [ "$kind" = virtual-machine ] && [ "$replaced" = 0 ]; then
        printf '  [warn] %s stopped during first boot; replacing it once\n' "$name"
        delete_marked_instance "$name"
        run_with_progress "relaunching real Incus VM after first-boot stop" launch_real_vm
        replaced=1
      else
        die "$name stopped before becoming ready"
      fi
    fi
    sleep 2
  done
  # Keep the last actual probe, without retrying beyond the readiness deadline. Diagnostics
  # must finish before EXIT deletes the marked instance and must preserve the timeout outcome.
  bytes="$(wc -c < "$probe_error")"
  printf 'P0_INCUS_DIAGNOSTIC instance=%s query=agent exit_status=%s bytes=%s limit=4096\n' \
    "$name" "$probe_status" "$bytes" >&2
  head -c 4096 "$probe_error" >&2 || true
  printf '\n' >&2
  : > "$query_output"
  : > "$query_error"
  chmod 0600 "$query_output" "$query_error"
  if real_incus_observe query "/1.0/instances/$name?project=$PROJECT" \
    >"$query_output" 2>"$query_error"; then status=0; else status=$?; fi
  printf 'P0_INCUS_DIAGNOSTIC instance=%s query=instance exit_status=%s\n' \
    "$name" "$status" >&2
  if [ "$status" = 0 ]; then
    if jq -ce --arg name "$name" --arg marker "$MARKER" '
      def field($pattern):
        if . == null then null
        elif type == "string" and test($pattern) then .
        else error("invalid diagnostic field") end;
      if .name != $name or .config["user.subyard.p0"] != $marker then
        error("unexpected diagnostic instance identity")
      else {
        name: $name,
        image: (.config["volatile.base_image"] | field("^[0-9a-f]{64}$")),
        cpu: (.expanded_config["limits.cpu"] | field("^[0-9,-]{1,64}$")),
        memory: (.expanded_config["limits.memory"] | field("^[0-9]+([KMGTPE]i?B|%)?$")),
        root_size: (.expanded_devices.root.size | field("^[0-9]+([KMGTPE]i?B)?$")),
        secureboot: (.expanded_config["security.secureboot"] | field("^(true|false)$"))
      } end
    ' "$query_output" >&2; then status=0; else status=$?; fi
    printf 'P0_INCUS_DIAGNOSTIC instance=%s query=instance-projection exit_status=%s\n' \
      "$name" "$status" >&2
  else
    head -c 4096 "$query_error" >&2 || true
    printf '\n' >&2
  fi
  for detail in info console; do
    limit=65536
    : > "$query_output"
    if real_incus_observe "$detail" "$name" --project "$PROJECT" --show-log \
      >"$query_output" 2>&1; then status=0; else status=$?; fi
    bytes="$(wc -c < "$query_output")"
    printf 'P0_INCUS_DIAGNOSTIC instance=%s query=%s exit_status=%s bytes=%s limit=%s\n' \
      "$name" "$detail" "$status" "$bytes" "$limit" >&2
    head -c "$limit" "$query_output" >&2 || true
    printf '\n' >&2
  done
  if [ "$kind" = virtual-machine ]; then
    if capture_qemu_facts "$name" >"$query_output" 2>"$query_error"; then
      status=0
      printf 'P0_INCUS_QEMU %s\n' "$(cat "$query_output")" >&2
    else
      status=$?
    fi
    printf 'P0_INCUS_DIAGNOSTIC instance=%s query=qemu-binding exit_status=%s\n' \
      "$name" "$status" >&2
    if capture_vga_screenshot "$name" >&2; then status=0; else status=$?; fi
    printf 'P0_INCUS_DIAGNOSTIC instance=%s query=vga-capture exit_status=%s\n' \
      "$name" "$status" >&2
  fi
  die "$name did not become ready (last state: ${state:-unknown})"
}
TMP="$(mktemp -d /tmp/subyard-p0-incus.XXXXXX)"
wait_ready p0-container container
wait_ready p0-vm virtual-machine

go test -c -tags realincus -o "$TMP/incusclient-real.test" ./internal/adapters/incusclient
sudo -n env \
  SUBYARD_REAL_INCUS_SOCKET=/var/lib/incus/unix.socket \
  SUBYARD_REAL_INCUS_CONTAINER_PROJECT="$PROJECT" \
  SUBYARD_REAL_INCUS_CONTAINER_INSTANCE=p0-container \
  SUBYARD_REAL_INCUS_VM_PROJECT="$PROJECT" \
  SUBYARD_REAL_INCUS_VM_INSTANCE=p0-vm \
  SUBYARD_REAL_INCUS_TEST_BINARY="$TMP/incusclient-real.test" \
  bash "$ROOT/tests/real-host/incus-contract.sh"
cleanup
trap - EXIT
project_exists && die "$PROJECT remains after cleanup"
printf 'ok: real Incus resources passed and were removed\n'
