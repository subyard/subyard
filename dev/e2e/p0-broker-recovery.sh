#!/usr/bin/env bash
# Real disposable-host acceptance for broker logging and quarantine rebuild.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
RUNNER="$ROOT/dev/agent-e2e.sh"
YARD="${SUBYARD_E2E_YARD:-test-yard}"
OUTER_PROJECT="subyard-$YARD"
OUTER_INSTANCE="yard-$YARD"
STATE_PARENT=''
NEIGHBOR_PID=''
VICTIM_PID=''
NEIGHBOR_CONFIG=''
RECLAIM_MARKER=''
RECLAIM_FIXTURE=/var/tmp/subyard-p0-release-reclaim
RECLAIM_FIXTURE_BYTES=$((512 * 1024 * 1024))
FAULT_ROOT=/run/subyard-p0-incus-fault
FAULT_INSTALLED=0
REAPER_MASKED=0
REAPER_TIMER_STOPPED=0
# The 100-minute default contains the 90-minute provisioning lease plus a
# 10-minute cleanup/reaper margin. The 120-minute override ceiling remains bounded.
RECOVERY_POLL_SECONDS=2
RECOVERY_WAIT_SECONDS="${P0_BROKER_RECOVERY_WAIT_SECONDS:-6000}"
RECOVERY_STATUS_TIMEOUT_SECONDS="${P0_BROKER_RECOVERY_STATUS_TIMEOUT_SECONDS:-30}"
RECOVERY_STATUS_KILL_AFTER_SECONDS=10
HOLDER_STOP_GRACE_SECONDS=30
HOLDER_KILL_GRACE_SECONDS=10
HOLDER_STARTED_PID=''

die() { printf 'p0-broker-recovery: %s\n' "$*" >&2; exit 2; }

outer_root() {
  incus exec "$OUTER_INSTANCE" --project "$OUTER_PROJECT" -- "$@"
}

restore_targeted_incus_failure() {
  [ "$FAULT_INSTALLED" = 1 ] || return 0
  outer_root sh -eu -c '
    root=$1
    config=/etc/subyard/test-vms.env
    backup=$root/test-vms.env.backup
    if [ -f "$backup" ] && [ ! -L "$backup" ]; then
      cp --preserve=mode,ownership,timestamps -- "$backup" "$config"
    fi
    find "$root" -depth -delete
  ' _ "$FAULT_ROOT"
  FAULT_INSTALLED=0
}

install_targeted_incus_failure() {
  local real_incus
  real_incus="$(outer_root sh -eu -c 'command -v incus')"
  case "$real_incus" in
    /usr/bin/incus | /usr/local/bin/incus) ;;
    *) die "unsafe inner Incus path $real_incus" ;;
  esac
  FAULT_INSTALLED=1
  outer_root sh -eu -s -- "$FAULT_ROOT" "$real_incus" <<'EOF'
root=$1
real_incus=$2
config=/etc/subyard/test-vms.env
backup=$root/test-vms.env.backup
wrapper=$root/incus
trigger=$root/trigger
candidate=$root/test-vms.env.candidate

[ -f "$config" ] && [ ! -L "$config" ]
install -d -m 0700 "$root"
cp --preserve=mode,ownership,timestamps -- "$config" "$backup"
{
  printf '%s\n' \
    '#!/bin/sh' \
    'set -eu' \
    'trigger=/run/subyard-p0-incus-fault/trigger' \
    'if [ -e "$trigger" ] && [ "$#" -eq 6 ] &&' \
    '  [ "$1" = project ] && [ "$2" = list ] &&' \
    '  [ "$3" = --format ] && [ "$4" = csv ] &&' \
    '  [ "$5" = -c ] && [ "$6" = n ]; then' \
    '  rm -f -- "$trigger"' \
    '  printf "%s\n" "Error: Failed to connect to local daemon: p0 targeted inventory fault" >&2' \
    '  exit 1' \
    'fi'
  printf 'exec %s "$@"\n' "$real_incus"
} > "$wrapper"
chmod 0700 "$wrapper"
awk -v value="SUBYARD_INNER_INCUS=$wrapper" '
  BEGIN { replaced = 0 }
  /^SUBYARD_INNER_INCUS=/ {
    if (!replaced) {
      print value
      replaced = 1
    }
    next
  }
  { print }
  END {
    if (!replaced) {
      print value
    }
  }
' "$config" > "$candidate"
chown --reference="$config" "$candidate"
chmod --reference="$config" "$candidate"
mv -f -- "$candidate" "$config"
: > "$trigger"
chmod 0600 "$trigger"
EOF
}

stop_slot_pair() {
  local slot="$1" vm project
  project="subyard-e2e-vms-slot-$slot"
  for vm in e2e-vm-1 e2e-vm-2; do
    outer_root incus stop "$vm" --project "$project" --force
  done
}

start_slot_pair() {
  local slot="$1" vm project
  project="subyard-e2e-vms-slot-$slot"
  for vm in e2e-vm-1 e2e-vm-2; do
    outer_root incus start "$vm" --project "$project"
  done
}

cleanup() {
  local rc=$?
  trap - EXIT INT TERM
  set +e
  if [ -n "$NEIGHBOR_CONFIG" ] && [ -n "$RECLAIM_MARKER" ]; then
    remove_reclaim_fixture "$NEIGHBOR_CONFIG" >/dev/null 2>&1 || rc=3
  fi
  restore_targeted_incus_failure >/dev/null 2>&1
  if [ "$REAPER_MASKED" = 1 ]; then
    outer_root systemctl unmask --runtime \
      subyard-test-vms-lease-reaper.service >/dev/null 2>&1
    outer_root systemctl start --no-block \
      subyard-test-vms-lease-reaper.service >/dev/null 2>&1
  fi
  if [ "$REAPER_TIMER_STOPPED" = 1 ]; then
    outer_root systemctl start \
      subyard-test-vms-lease-reaper.timer >/dev/null 2>&1
  fi
  for client in victim neighbor; do
    [ -z "$STATE_PARENT" ] || : > "$STATE_PARENT/$client.release"
  done
  for pid in "$VICTIM_PID" "$NEIGHBOR_PID"; do
    [ -z "$pid" ] || stop_holder_child "$pid" >/dev/null 2>&1 || rc=3
  done
  if [ -n "$STATE_PARENT" ]; then
    case "$STATE_PARENT" in
      /tmp/subyard-p0-broker-recovery.*)
        find "$STATE_PARENT" -depth -delete >/dev/null 2>&1
        ;;
      *) rc=3 ;;
    esac
  fi
  exit "$rc"
}
trap cleanup EXIT INT TERM

status() {
  local request_timeout="${1:-$RECOVERY_STATUS_TIMEOUT_SECONDS}"
  timeout --signal=TERM --kill-after="$RECOVERY_STATUS_KILL_AFTER_SECONDS" \
    "$request_timeout" env SUBYARD_E2E_STATE_DIR="$STATE_PARENT/probe" \
    "$RUNNER" --yard "$YARD" --status --json
}

wait_for_ready() {
  local client="$1" pid="$2" attempts="${P0_BROKER_READY_ATTEMPTS:-1200}"
  # A cold remote image import can consume more than five minutes before the
  # retained pair reaches its separately bounded P0 boot and SSH checks. Keep
  # this outer acceptance watchdog large enough for both phases.
  for _ in $(seq 1 "$attempts"); do
    [ ! -s "$STATE_PARENT/$client.ready" ] || return 0
    if [ -s "$STATE_PARENT/$client.failed" ] || ! kill -0 "$pid" >/dev/null 2>&1; then
      tail -n 240 "$STATE_PARENT/$client.log" >&2
      return 1
    fi
    sleep 1
  done
  tail -n 240 "$STATE_PARENT/$client.log" >&2
  return 1
}

start_holder_child() {
  set -m
  (set +m; "$@") &
  HOLDER_STARTED_PID=$!
  set +m
}

stop_holder_child() {
  local root="$1" now stop_deadline kill_deadline
  if ! kill -0 -- "-$root" >/dev/null 2>&1; then
    wait "$root" >/dev/null 2>&1 || true
    return 0
  fi
  kill -TERM -- "-$root" >/dev/null 2>&1 || true
  now="$(recovery_monotonic_seconds)"
  stop_deadline=$((now + HOLDER_STOP_GRACE_SECONDS))
  while kill -0 -- "-$root" >/dev/null 2>&1; do
    now="$(recovery_monotonic_seconds)"
    [ "$now" -lt "$stop_deadline" ] || break
    sleep 1
  done
  if kill -0 -- "-$root" >/dev/null 2>&1; then
    kill -KILL -- "-$root" >/dev/null 2>&1 || true
  fi
  now="$(recovery_monotonic_seconds)"
  kill_deadline=$((now + HOLDER_KILL_GRACE_SECONDS))
  while kill -0 -- "-$root" >/dev/null 2>&1; do
    now="$(recovery_monotonic_seconds)"
    [ "$now" -lt "$kill_deadline" ] || break
    sleep 1
  done
  if kill -0 -- "-$root" >/dev/null 2>&1; then
    printf 'p0-broker-recovery: holder process group %s survived bounded shutdown\n' \
      "$root" >&2
    return 1
  fi
  wait "$root" >/dev/null 2>&1 || true
}

hold_lease() (
  local client="$1" purpose="$2" requested_slot="$3" ready_temp
  export SUBYARD_E2E_STATE_DIR="$STATE_PARENT/$client"
  export SUBYARD_E2E_YARD="$YARD"
  # shellcheck source=dev/agent-e2e.sh
  . "$RUNNER"

  LOCAL_TEMP="$(mktemp -d "$STATE_PARENT/$client-runtime.XXXXXX")"
  LEASE_PURPOSE="$purpose"
  LEASE_REQUESTED_SLOT="$requested_slot"
  holder_cleanup() {
    local rc=$?
    trap - EXIT INT TERM
    set +e
    if [ -n "$LEASE_KEEPER_PID" ]; then
      kill "$LEASE_KEEPER_PID" >/dev/null 2>&1 || true
      wait "$LEASE_KEEPER_PID" >/dev/null 2>&1 || true
      LEASE_KEEPER_PID=''
    fi
    release_lease || rc=1
    exit "$rc"
  }
  trap holder_cleanup EXIT INT TERM

  acquire_lease || { : > "$STATE_PARENT/$client.failed"; exit 1; }
  start_lease_keeper
  ready_temp="$(mktemp "$STATE_PARENT/.$client.ready.XXXXXX")"
  printf '%s\t%s\t%s\t%s\t%s\n' \
    "$LEASE_SLOT" "$CLIENT_CONFIG" "$LEASE_PROJECT" "$LEASE_RUN" "$LEASE_PURPOSE" \
    > "$ready_temp"
  mv -f "$ready_temp" "$STATE_PARENT/$client.ready"
  while [ ! -e "$STATE_PARENT/$client.release" ]; do sleep 1; done
)

reclaim_held_pair_capacity() {
  local config="$1" label="$2" vm available
  for vm in 1 2; do
    ssh -F "$config" -T "e2e-vm-$vm" -- \
      "sh -eu -c 'sync; fstrim -av >/dev/null 2>&1 || true'"
  done
  available="$(outer_root \
    df -B1 --output=avail /var/lib/subyard/test-vms/slots \
    | awk 'NR == 2 {print $1}')"
  [[ "$available" =~ ^[0-9]+$ ]] \
    || die "could not measure nested pool reserve after trimming held $label pair"
  # A full two-slot pool can legitimately sit below the create headroom while
  # all four retained disks exist. Recovery deletes the quarantined pair before
  # its authoritative capacity preflight, so record the reserve here without
  # rejecting the healthy held neighbor prematurely.
  printf '  [ ok ] held %s pair trimmed; nested pool reserve=%s\n' \
    "$label" "$available"
}

wait_for_pair_ssh() {
  local config="$1" vm attempt ready
  for vm in 1 2; do
    ready=0
    for attempt in $(seq 1 120); do
      if ssh -F "$config" -T -o ConnectTimeout=3 "e2e-vm-$vm" -- true \
        </dev/null >/dev/null 2>&1; then
        ready=1
        break
      fi
      sleep 1
    done
    [ "$ready" = 1 ] || die "held slot VM$vm did not return after restart"
  done
}

resolve_root_image() {
  local slot="$1" vm="$2" project
  [[ "$slot" =~ ^[1-9][0-9]*$ ]] && [[ "$vm" =~ ^e2e-vm-[12]$ ]] \
    || die 'refusing unsafe retained VM path inputs'
  project="subyard-e2e-vms-slot-$slot"
  outer_root sh -eu -s -- "$project" "$vm" <<'EOF'
project=$1
vm=$2
source=/srv/incus-e2e/storage
alias=/var/lib/incus/storage-pools/default
[ "$(incus storage get default source)" = "$source" ]
[ "$(incus project get "$project" user.subyard.managed)" = test-vms-v1 ]
[ "$(incus config get "$vm" user.subyard.managed --project "$project")" = test-vms-v1 ]
[ "$(stat -c %d:%i "$source")" = "$(stat -c %d:%i "$alias")" ]
relative="virtual-machines/${project}_${vm}/root.img"
candidate=$source/$relative
published=$alias/$relative
[ -f "$candidate" ] && [ ! -L "$candidate" ]
[ -f "$published" ] && [ ! -L "$published" ]
[ "$(readlink -f -- "$candidate")" = "$candidate" ]
[ "$(stat -c %d:%i "$candidate")" = "$(stat -c %d:%i "$published")" ]
printf '%s\n' "$candidate"
EOF
}

root_image_allocated_bytes() {
  local path="$1" metadata blocks block_size
  [[ "$path" =~ ^/srv/incus-e2e/storage/virtual-machines/subyard-e2e-vms-slot-[1-9][0-9]*_e2e-vm-[12]/root\.img$ ]] \
    || die "refusing unsafe retained root image path $path"
  metadata="$(outer_root stat -c '%b %B' -- "$path")"
  read -r blocks block_size <<<"$metadata"
  [[ "$blocks" =~ ^[0-9]+$ ]] && [[ "$block_size" =~ ^[1-9][0-9]*$ ]] \
    || die "could not measure allocated blocks for $path"
  [ "$blocks" -le $((9223372036854775807 / block_size)) ] \
    || die "allocated block measurement overflow for $path"
  printf '%s\n' "$((blocks * block_size))"
}

stage_reclaim_fixture() {
  local config="$1" vm="$2"
  [[ "$vm" =~ ^[12]$ ]] || die 'refusing an unsafe reclaim fixture VM selector'
  [[ "$RECLAIM_MARKER" =~ ^subyard-p0-release-reclaim-v1:[0-9a-f]{8}$ ]] \
    || die 'refusing an unsafe reclaim fixture marker'
  ssh -F "$config" -T "e2e-vm-$vm" -- \
    sh -eu -s -- "$RECLAIM_FIXTURE" "$RECLAIM_MARKER" "$RECLAIM_FIXTURE_BYTES" <<'EOF'
root=$1
expected=$2
bytes=$3
[ ! -e "$root" ] && [ ! -L "$root" ]
install -d -m 0700 "$root"
printf '%s\n' "$expected" > "$root/.subyard-p0-release-reclaim"
dd if=/dev/urandom of="$root/allocated-fixture" bs=1M count="$((bytes / 1024 / 1024))" status=none
sync "$root/allocated-fixture" "$root/.subyard-p0-release-reclaim"
EOF
}

remove_reclaim_fixture() {
  local config="$1" vm rc=0
  [[ "$RECLAIM_MARKER" =~ ^subyard-p0-release-reclaim-v1:[0-9a-f]{8}$ ]] \
    || return 1
  for vm in 1 2; do
    if ! ssh -F "$config" -T "e2e-vm-$vm" -- \
      sh -eu -s -- "$RECLAIM_FIXTURE" "$RECLAIM_MARKER" <<'EOF'
root=$1
expected=$2
[ -e "$root" ] || exit 0
marker=$root/.subyard-p0-release-reclaim
[ -d "$root" ] && [ ! -L "$root" ]
[ -f "$marker" ] && [ ! -L "$marker" ]
[ "$(cat "$marker")" = "$expected" ]
find "$root" -xdev -depth -delete
sync
EOF
    then
      rc=1
    fi
  done
  return "$rc"
}

slot_pair_identity() {
  local slot="$1" vm project uuid
  project="subyard-e2e-vms-slot-$slot"
  for vm in e2e-vm-1 e2e-vm-2; do
    uuid="$(outer_root incus config get "$vm" volatile.uuid --project "$project")"
    [[ "$uuid" =~ ^[0-9a-f-]{36}$ ]] \
      || die "retained $vm has no stable VM identity"
    printf '%s=%s\n' "$vm" "$uuid"
  done
}

assert_slot_pair_stopped() {
  local slot="$1" project inventory
  project="subyard-e2e-vms-slot-$slot"
  inventory="$(outer_root incus list --project "$project" --format json)"
  jq -e '
    length == 2 and
    ([.[].name] | sort) == ["e2e-vm-1", "e2e-vm-2"] and
    all(.[]; .status == "Stopped")
  ' <<<"$inventory" >/dev/null \
    || die "slot-$slot retained pair was deleted, replaced or left running"
}

install_candidate_update() {
	local arch release artifact runtime_root release_cache
	artifact="${P0_BROKER_RECOVERY_UPDATE_ARTIFACT:-}"
	if [ -z "$artifact" ]; then
		arch="$(go env GOARCH)"
    release="$ROOT/.build/p0-broker-recovery-update"
    artifact="$release/subyard-p0-broker-recovery-update-linux-$arch"
    dev/package-engine.sh \
      --output-dir "$release" \
      --version p0-broker-recovery-update \
      --arch "$arch" >/dev/null
  fi
  for input in \
    "$artifact.tar.gz" \
    "$artifact.tar.gz.sha256" \
    "$artifact.tar.gz.manifest.json" \
    "$artifact.tar.gz.provenance.json"; do
		[ -f "$input" ] && [ ! -L "$input" ] \
			|| die "prepared candidate update input is unavailable: $input"
	done
	release="$(dirname "$artifact")"
	runtime_root="${SUBYARD_HOME:-$HOME/.subyard}/runtime"
	release_cache="${SUBYARD_HOME:-$HOME/.subyard}/releases"
	YARD_RELEASE_BASE_URL="file://$release" YARD_RELEASE_CACHE="$release_cache" \
		"$runtime_root/current/bin/yard" update \
		--runtime-root "$runtime_root" --version p0-broker-recovery-update \
		--check >/dev/null
	YARD_RELEASE_BASE_URL="file://$release" YARD_RELEASE_CACHE="$release_cache" \
		"$runtime_root/current/bin/yard" update \
		--runtime-root "$runtime_root" --version p0-broker-recovery-update \
		--yes >/dev/null
}

rollback_candidate_update() {
	local runtime_root="${SUBYARD_HOME:-$HOME/.subyard}/runtime"
	"$runtime_root/current/bin/yard" update \
		--runtime-root "$runtime_root" --rollback --yes >/dev/null
}

wait_for_slot_state() {
  local slot="$1" wanted="$2" wait_seconds="$3" payload='' state
  local now deadline remaining request_timeout sleep_seconds
  now="$(recovery_monotonic_seconds)"
  deadline=$((now + wait_seconds))
  while true; do
    now="$(recovery_monotonic_seconds)"
    remaining=$((deadline - now))
    [ "$remaining" -gt "$RECOVERY_STATUS_KILL_AFTER_SECONDS" ] || break
    request_timeout="$RECOVERY_STATUS_TIMEOUT_SECONDS"
    if [ "$request_timeout" -gt "$((remaining - RECOVERY_STATUS_KILL_AFTER_SECONDS))" ]; then
      request_timeout=$((remaining - RECOVERY_STATUS_KILL_AFTER_SECONDS))
    fi
    if payload="$(status "$request_timeout")"; then
      state="$(jq -r --arg slot "$slot" \
        '.pool.slots[] | select(.slot_id == $slot) | .state' <<<"$payload")"
      if [ "$state" = "$wanted" ]; then
        printf '%s\n' "$payload"
        return 0
      fi
    fi
    now="$(recovery_monotonic_seconds)"
    remaining=$((deadline - now))
    [ "$remaining" -gt 0 ] || break
    sleep_seconds="$RECOVERY_POLL_SECONDS"
    [ "$sleep_seconds" -le "$remaining" ] || sleep_seconds="$remaining"
    sleep "$sleep_seconds"
  done
  printf '%s\n' "$payload" >&2
  return 1
}

recovery_monotonic_seconds() {
  local uptime _
  IFS=' ' read -r uptime _ < /proc/uptime \
    || die 'cannot read the kernel monotonic clock'
  uptime="${uptime%%.*}"
  [[ "$uptime" =~ ^[0-9]+$ ]] \
    || die 'kernel monotonic clock returned an invalid value'
  printf '%s\n' "$uptime"
}

report_slot_diagnostics() {
  local slot="$1" label="$2" payload incident_id incident slot_number
  printf 'p0-broker-recovery: diagnostics for %s (%s)\n' "$slot" "$label" >&2
  if payload="$(status 2>&1)"; then
    printf '%s\n' "$payload" >&2
    incident_id="$(jq -r --arg slot "$slot" '
      .pool.slots[] | select(.slot_id == $slot) | .incident_id // empty
    ' <<<"$payload")"
  else
    printf '%s\n' "$payload" >&2
    incident_id=''
  fi
  sudo -n systemctl start subyard-test-vms-host-sink.service >/dev/null 2>&1 || true
  slot_number="${slot#slot-}"
  slot_number="$((10#$slot_number))"
  ./bin/yard test-vms logs -n 200 --slot "$slot_number" >&2 || true
  if [[ "$incident_id" =~ ^[0-9]{20}-[0-9a-f]{16}$ ]]; then
    incident="$SUBYARD_HOME/logs/test-vms-broker-incidents/$incident_id.json"
    if [ -f "$incident" ] && [ ! -L "$incident" ]; then
      jq '{
        schema_version,
        incident_id,
        created_at,
        slot_id,
        resource_generation,
        lease_epoch,
        failure_reason,
        context,
        command
      }' "$incident" >&2 || true
    fi
  fi
  outer_root journalctl \
    -u subyard-test-vms-broker.service \
    -u subyard-test-vms-lease-reaper.service \
    -u "subyard-test-vms-recover@$slot.service" \
    -n 160 --no-pager >&2 || true
}

[ "${SUBYARD_E2E_VM:-}" = 1 ] \
  || die 'run on VM1 through dev/agent-e2e.sh'
command -v jq >/dev/null 2>&1 || die 'jq is required'
command -v incus >/dev/null 2>&1 || die 'Incus is required'
command -v go >/dev/null 2>&1 || die 'Go is required'
command -v timeout >/dev/null 2>&1 || die 'timeout is required'
[[ "$RECOVERY_WAIT_SECONDS" =~ ^[1-9][0-9]{1,4}$ ]] \
  && [ "$RECOVERY_WAIT_SECONDS" -ge 60 ] \
  && [ "$RECOVERY_WAIT_SECONDS" -le 7200 ] \
  || die 'broker recovery wait must be an integer from 60 through 7200 seconds'
[[ "$RECOVERY_STATUS_TIMEOUT_SECONDS" =~ ^[1-9][0-9]?$ ]] \
  && [ "$RECOVERY_STATUS_TIMEOUT_SECONDS" -le 60 ] \
  || die 'broker status timeout must be an integer from 1 through 60 seconds'
STATE_PARENT="$(mktemp -d /tmp/subyard-p0-broker-recovery.XXXXXX)"
for client in neighbor victim probe next reuse; do
  SUBYARD_E2E_STATE_DIR="$STATE_PARENT/$client" \
    "$RUNNER" --yard "$YARD" --prepare >/dev/null
done

initial="$(status)"
jq -e '
  (.pool.slots | length) == 2 and
  all(.pool.slots[]; .state == "available")
' <<<"$initial" >/dev/null \
  || die 'run only against an empty two-slot candidate broker'
initial_generation="$(jq -r '
  .pool.slots[] | select(.slot_id == "slot-001") | .resource_generation
' <<<"$initial")"

victim_attempt=1
while true; do
  start_holder_child hold_lease victim quarantine-victim slot-001 \
    >"$STATE_PARENT/victim.log" 2>&1
  VICTIM_PID="$HOLDER_STARTED_PID"
  if wait_for_ready victim "$VICTIM_PID"; then
    break
  fi
  stop_holder_child "$VICTIM_PID" \
    || die 'victim holder did not stop after its readiness timeout'
  VICTIM_PID=''
  report_slot_diagnostics slot-001 \
    "victim provisioning attempt $victim_attempt failed"
  [ "$victim_attempt" -lt 3 ] \
    || die 'victim lease did not become ready after 3 automatic rebuilds'
  wait_for_slot_state slot-001 available "$RECOVERY_WAIT_SECONDS" >/dev/null \
    || { report_slot_diagnostics slot-001 'victim automatic rebuild timed out';
         die 'victim provisioning quarantine did not recover'; }
  for marker in \
    "$STATE_PARENT/victim.ready" \
    "$STATE_PARENT/victim.failed" \
    "$STATE_PARENT/victim.release"; do
    [ ! -e "$marker" ] || find "$marker" -delete
  done
  victim_attempt=$((victim_attempt + 1))
done

IFS=$'\t' read -r VICTIM_SLOT VICTIM_CONFIG VICTIM_PROJECT _victim_run _victim_purpose \
  < "$STATE_PARENT/victim.ready"
[ "$VICTIM_SLOT" = slot-001 ] || die "victim received $VICTIM_SLOT"
for vm in 1 2; do
  ssh -F "$VICTIM_CONFIG" -T "e2e-vm-$vm" -- \
    touch /var/tmp/subyard-quarantine-sentinel
done
reclaim_held_pair_capacity "$VICTIM_CONFIG" victim
stop_slot_pair 1

neighbor_attempt=1
while true; do
  start_holder_child hold_lease neighbor held-neighbor slot-002 \
    >"$STATE_PARENT/neighbor.log" 2>&1
  NEIGHBOR_PID="$HOLDER_STARTED_PID"
  if wait_for_ready neighbor "$NEIGHBOR_PID"; then
    break
  fi
  stop_holder_child "$NEIGHBOR_PID" \
    || die 'neighbor holder did not stop after its readiness timeout'
  NEIGHBOR_PID=''
  report_slot_diagnostics slot-002 \
    "neighbor provisioning attempt $neighbor_attempt failed"
  [ "$neighbor_attempt" -lt 3 ] \
    || die 'neighbor lease did not become ready after 3 automatic rebuilds'
  wait_for_slot_state slot-002 available "$RECOVERY_WAIT_SECONDS" >/dev/null \
    || { report_slot_diagnostics slot-002 'neighbor automatic rebuild timed out';
         die 'neighbor provisioning quarantine did not recover'; }
  for marker in \
    "$STATE_PARENT/neighbor.ready" \
    "$STATE_PARENT/neighbor.failed" \
    "$STATE_PARENT/neighbor.release"; do
    [ ! -e "$marker" ] || find "$marker" -delete
  done
  neighbor_attempt=$((neighbor_attempt + 1))
done
IFS=$'\t' read -r NEIGHBOR_SLOT NEIGHBOR_CONFIG NEIGHBOR_PROJECT \
  neighbor_run _neighbor_purpose \
  < "$STATE_PARENT/neighbor.ready"
[ "$NEIGHBOR_SLOT" = slot-002 ] || die "neighbor received $NEIGHBOR_SLOT"
[ -n "$NEIGHBOR_PROJECT" ] && [ "$NEIGHBOR_PROJECT" = "$VICTIM_PROJECT" ] \
  || die 'nested broker holders did not use one canonical candidate project'
reclaim_held_pair_capacity "$NEIGHBOR_CONFIG" neighbor

# Mask only the recovery worker while the quarantine evidence is inspected.
# Masking Incus itself would stop the broker through Requires=incus.service and
# run its drain-all ExecStop, changing the held neighbor before the targeted
# release can exercise its failure boundary.
outer_root systemctl stop subyard-test-vms-lease-reaper.timer
REAPER_TIMER_STOPPED=1
outer_root systemctl mask --runtime --now \
  subyard-test-vms-lease-reaper.service >/dev/null
REAPER_MASKED=1
install_targeted_incus_failure
: > "$STATE_PARENT/victim.release"
victim_release_rc=0
wait "$VICTIM_PID" || victim_release_rc=$?
VICTIM_PID=''
restore_targeted_incus_failure
[ "$victim_release_rc" -ne 0 ] || {
  die 'victim release unexpectedly succeeded while inner Incus was unavailable'
}

quarantined="$(wait_for_slot_state slot-001 quarantined 30)" \
  || { report_slot_diagnostics slot-001 'failed release did not quarantine';
       die 'failed release did not quarantine slot-001'; }
jq -e --arg project "$NEIGHBOR_PROJECT" '
  (.pool.slots[] | select(.slot_id == "slot-001")) as $victim |
  (.pool.slots[] | select(.slot_id == "slot-002")) as $neighbor |
  $victim.state == "quarantined" and
  ($victim.incident_id | test("^[0-9]{20}-[0-9a-f]{16}$")) and
  ($victim.last_failure_event_id | test("^[0-9]{20}-[0-9a-f]{16}$")) and
  ($victim | has("failure_reason") | not) and
  $neighbor.state == "held" and
  $neighbor.project == $project and
  $neighbor.purpose == "held-neighbor"
' <<<"$quarantined" >/dev/null \
  || die 'quarantine status was not bounded or changed the held neighbor'
incident_id="$(jq -r '
  .pool.slots[] | select(.slot_id == "slot-001") | .incident_id
' <<<"$quarantined")"
neighbor_heartbeat="$(jq -r '
  .pool.slots[] | select(.slot_id == "slot-002") | .last_heartbeat_at
' <<<"$quarantined")"

# Keep recovery paused until the held neighbor's guests are stopped. Otherwise
# the constrained acceptance host can start a doomed first rebuild attempt,
# consume the diagnostic recovery attempt before the deterministic failure
# boundary is fully staged. The lease and its heartbeat remain held throughout.
stop_slot_pair 2

# Exercise the release owner while the neighbor remains held and the incident
# and recovery schedule already exist.
install_candidate_update
rollback_candidate_update
after_rollback="$(status)"
jq -e --arg project "$NEIGHBOR_PROJECT" '
  (.pool.slots[] | select(.slot_id == "slot-002")) as $neighbor |
  $neighbor.state == "held" and
  $neighbor.project == $project and
  $neighbor.purpose == "held-neighbor"
' <<<"$after_rollback" >/dev/null \
  || die 'active broker update/rollback revoked or unattributed the held neighbor'
kill -0 "$NEIGHBOR_PID" >/dev/null 2>&1 \
  || { sed -n '1,240p' "$STATE_PARENT/neighbor.log" >&2;
       die 'held neighbor lost its heartbeat process during update/rollback'; }

# Start recovery only after release maintenance has finished. Running the
# worker concurrently with broker replacement can turn one deterministic
# incident into a failed attempt followed by a second incident.
outer_root systemctl unmask --runtime \
  subyard-test-vms-lease-reaper.service >/dev/null
REAPER_MASKED=0
outer_root systemctl start subyard-test-vms-lease-reaper.timer
REAPER_TIMER_STOPPED=0
outer_root systemctl start --no-block subyard-test-vms-lease-reaper.service

available="$(wait_for_slot_state slot-001 available "$RECOVERY_WAIT_SECONDS")" \
  || { report_slot_diagnostics slot-001 'automatic rebuild timed out';
       die 'root reaper did not automatically rebuild slot-001'; }
new_generation="$(jq -r '
  .pool.slots[] | select(.slot_id == "slot-001") | .resource_generation
' <<<"$available")"
[ "$new_generation" -eq "$((initial_generation + 1))" ] \
  || die "resource generation changed from $initial_generation to $new_generation"
jq -e --arg project "$NEIGHBOR_PROJECT" '
  (.pool.slots[] | select(.slot_id == "slot-002")) as $neighbor |
  $neighbor.state == "held" and
  $neighbor.project == $project
' <<<"$available" >/dev/null \
  || die 'automatic rebuild changed the held neighbor'

# Force immediate host-wide collection rather than waiting for the one-minute
# timer, then use the public global command without -Y.
sudo -n systemctl start subyard-test-vms-host-sink.service
global_log="$(./bin/yard test-vms logs -n 100000 --slot 1)"
jq -s -e --arg incident "$incident_id" '
  any(.[]; .kind == "slot.quarantined" and .incident_id == $incident) and
  any(.[]; .kind == "rebuild.delete" and .incident_id == $incident) and
  any(.[]; .kind == "rebuild.create" and .incident_id == $incident) and
  any(.[]; .kind == "recovery.available" and .incident_id == $incident)
' <<<"$global_log" >/dev/null || {
  jq -s '
    [.[] |
      select(.slot_id == "slot-001") |
      {kind, incident_id, recovery_attempt}]
  ' <<<"$global_log" >&2
  die 'global broker log omitted the quarantine/rebuild timeline'
}

incident="$SUBYARD_HOME/logs/test-vms-broker-incidents/$incident_id.json"
[ -f "$incident" ] && [ ! -L "$incident" ] \
  || die 'host-wide immutable incident artifact is missing'
jq -e \
  --arg command "$FAULT_ROOT/incus project list --format csv -c n" \
  --arg project "$VICTIM_PROJECT" '
  (.failure_reason | contains("Failed to connect to local daemon")) and
  .context.schema_version == 2 and
  .context.yard == "test-yard" and
  .context.project == $project and
  (.context | has("checkout") | not) and
  .command.command == $command and
  .command.exit_code != 0 and
  (.diagnostics | type == "object")
' "$incident" >/dev/null \
  || die 'incident did not preserve the exact original command failure'
incident_hash="$(sha256sum "$incident" | awk '{print $1}')"
sudo -n systemctl start subyard-test-vms-host-sink.service
[ "$(sha256sum "$incident" | awk '{print $1}')" = "$incident_hash" ] \
  || die 'replayed sink delivery overwrote the immutable incident'

SUBYARD_E2E_STATE_DIR="$STATE_PARENT/next" \
  "$RUNNER" --yard "$YARD" --slot 1 --purpose clean-next-lease --vm both -- \
  test ! -e /var/tmp/subyard-quarantine-sentinel

sleep 65
after_renew="$(status)"
new_neighbor_heartbeat="$(jq -r '
  .pool.slots[] | select(.slot_id == "slot-002") | .last_heartbeat_at
' <<<"$after_renew")"
[ "$new_neighbor_heartbeat" != "$neighbor_heartbeat" ] \
  || die 'held neighbor heartbeat did not advance across update/recovery'

# The held neighbor was stopped above to preserve recovery headroom. Restart it
# without changing the lease, then stage reclaimable data after all manual
# capacity trims so only the normal product release can satisfy this assertion.
start_slot_pair 2
wait_for_pair_ssh "$NEIGHBOR_CONFIG"
neighbor_generation="$(jq -r '
  .pool.slots[] | select(.slot_id == "slot-002") | .resource_generation
' <<<"$after_renew")"
pair_identity_before="$(slot_pair_identity 2)"
vm1_root_image="$(resolve_root_image 2 e2e-vm-1)"
vm2_root_image="$(resolve_root_image 2 e2e-vm-2)"
vm1_baseline="$(root_image_allocated_bytes "$vm1_root_image")"
vm2_baseline="$(root_image_allocated_bytes "$vm2_root_image")"
RECLAIM_MARKER="subyard-p0-release-reclaim-v1:$neighbor_run"
stage_reclaim_fixture "$NEIGHBOR_CONFIG" 1
stage_reclaim_fixture "$NEIGHBOR_CONFIG" 2
vm1_with_fixture="$(root_image_allocated_bytes "$vm1_root_image")"
vm2_with_fixture="$(root_image_allocated_bytes "$vm2_root_image")"
vm1_fixture_delta=$((vm1_with_fixture - vm1_baseline))
vm2_fixture_delta=$((vm2_with_fixture - vm2_baseline))
# Random writes must materialize most of each guest file in its own sparse
# root.img. Requiring half the requested bytes rejects an unobservable fixture
# while allowing for filesystem and image-cluster accounting differences.
minimum_observable_delta=$((RECLAIM_FIXTURE_BYTES / 2))
[ "$vm1_fixture_delta" -ge "$minimum_observable_delta" ] \
  || die "VM1 fixture allocation was not observable: baseline=$vm1_baseline fixture=$vm1_with_fixture requested=$RECLAIM_FIXTURE_BYTES"
[ "$vm2_fixture_delta" -ge "$minimum_observable_delta" ] \
  || die "VM2 fixture allocation was not observable: baseline=$vm2_baseline fixture=$vm2_with_fixture requested=$RECLAIM_FIXTURE_BYTES"
remove_reclaim_fixture "$NEIGHBOR_CONFIG"
RECLAIM_MARKER=''
vm1_before_release="$(root_image_allocated_bytes "$vm1_root_image")"
vm2_before_release="$(root_image_allocated_bytes "$vm2_root_image")"
# Derive each release threshold from that VM's observed growth. The one-half
# allowance covers bounded guest shutdown writes after fstrim while remaining
# large enough that an unrelated metadata decrease cannot satisfy the check.
vm1_minimum_reclaim=$(((vm1_fixture_delta + 1) / 2))
vm2_minimum_reclaim=$(((vm2_fixture_delta + 1) / 2))
[ "$((vm1_before_release - vm1_baseline))" -ge "$vm1_minimum_reclaim" ] \
  || die "VM1 deleted fixture left too few observable blocks for release: baseline=$vm1_baseline fixture=$vm1_with_fixture before=$vm1_before_release minimum=$vm1_minimum_reclaim"
[ "$((vm2_before_release - vm2_baseline))" -ge "$vm2_minimum_reclaim" ] \
  || die "VM2 deleted fixture left too few observable blocks for release: baseline=$vm2_baseline fixture=$vm2_with_fixture before=$vm2_before_release minimum=$vm2_minimum_reclaim"

: > "$STATE_PARENT/neighbor.release"
wait "$NEIGHBOR_PID" \
  || { report_slot_diagnostics slot-002 'held neighbor release failed';
       sed -n '1,240p' "$STATE_PARENT/neighbor.log" >&2;
       die 'held neighbor did not release cleanly'; }
NEIGHBOR_PID=''
released="$(wait_for_slot_state slot-002 available 60)" \
  || { report_slot_diagnostics slot-002 'held neighbor did not return to the pool';
       die 'held neighbor did not return to the pool'; }
jq -e --argjson generation "$neighbor_generation" '
  (.pool.slots[] | select(.slot_id == "slot-002")) as $slot |
  all(.pool.slots[]; .state == "available") and
  $slot.resource_generation == $generation
' <<<"$released" >/dev/null \
  || die 'normal release rebuilt the retained pair or left the pool unavailable'
vm1_after_release="$(root_image_allocated_bytes "$vm1_root_image")"
vm2_after_release="$(root_image_allocated_bytes "$vm2_root_image")"
vm1_release_delta=$((vm1_before_release - vm1_after_release))
vm2_release_delta=$((vm2_before_release - vm2_after_release))
[ "$vm1_release_delta" -ge "$vm1_minimum_reclaim" ] \
  || die "normal release reclaimed too few VM1 root.img blocks: before=$vm1_before_release after=$vm1_after_release observed_fixture=$vm1_fixture_delta minimum=$vm1_minimum_reclaim"
[ "$vm2_release_delta" -ge "$vm2_minimum_reclaim" ] \
  || die "normal release reclaimed too few VM2 root.img blocks: before=$vm2_before_release after=$vm2_after_release observed_fixture=$vm2_fixture_delta minimum=$vm2_minimum_reclaim"
[ "$(slot_pair_identity 2)" = "$pair_identity_before" ] \
  || die 'normal release replaced the retained VM pair'
assert_slot_pair_stopped 2

SUBYARD_E2E_STATE_DIR="$STATE_PARENT/reuse" \
  "$RUNNER" --yard "$YARD" --slot 2 --purpose retained-pair-reuse --vm both -- \
  test ! -e "$RECLAIM_FIXTURE"
final="$(wait_for_slot_state slot-002 available 60)" \
  || { report_slot_diagnostics slot-002 'retained pair was not reusable';
       die 'retained pair was not reusable'; }
jq -e --argjson generation "$neighbor_generation" '
  (.pool.slots[] | select(.slot_id == "slot-002")) as $slot |
  all(.pool.slots[]; .state == "available") and
  $slot.resource_generation == $generation
' <<<"$final" >/dev/null \
  || die 'candidate pool was not fully reusable after acceptance'
assert_slot_pair_stopped 2
[ "$(slot_pair_identity 2)" = "$pair_identity_before" ] \
  || die 'retained VM pair identity changed during reuse'

printf '  [ ok ] release reclaimed VM1=%s/observed-%s and VM2=%s/observed-%s allocated root.img bytes without replacing retained VMs\n' \
  "$vm1_release_delta" "$vm1_fixture_delta" \
  "$vm2_release_delta" "$vm2_fixture_delta"
printf 'ok: host-wide broker log, immutable incident, held rollback, automatic clean rebuild and retained-disk reclaim\n'
