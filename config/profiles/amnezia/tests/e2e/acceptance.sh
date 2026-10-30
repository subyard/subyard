#!/usr/bin/env bash
# Exercise the owner and client on one disposable two-VM lease and one source bundle.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." && pwd -P)"
# shellcheck source=dev/agent-e2e.sh
. "$ROOT/dev/agent-e2e.sh"
VM_COUNT_REQUESTED=2
VM_COUNT=2

lane=full
usage() { printf 'Usage: config/profiles/amnezia/tests/e2e/acceptance.sh --slot N [--lane full|setup|share|resources|reboot|recovery|startup|disabled|reconnect]\n'; }
while [ "$#" -gt 0 ]; do
  case "$1" in
    --slot)
      [ "$#" -ge 2 ] && [ -z "$LEASE_REQUESTED_SLOT" ] || die '--slot N is required once'
      set_requested_slot "$2" --slot
      shift 2
      ;;
    --lane)
      [ "$#" -ge 2 ] || die '--lane requires full, setup, share, resources, reboot, recovery, startup, disabled or reconnect'
      case "$2" in full|setup|share|resources|reboot|recovery|startup|disabled|reconnect) lane="$2" ;; *) die '--lane requires full, setup, share, resources, reboot, recovery, startup, disabled or reconnect' ;; esac
      shift 2
      ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown argument '$1'" ;;
  esac
done
[ -n "$LEASE_REQUESTED_SLOT" ] || die '--slot N is required'

LOCAL_TEMP="$(mktemp -d "${TMPDIR:-/tmp}/subyard-agent-e2e.XXXXXX")"
LEASE_PURPOSE=amnezia-acceptance
# The enclosing runner keeps its fixed work deadline and 300-second TERM grace.
# Capture (110+2 seconds) and cleanup (under 95 seconds) fit inside that grace.
install -d -m 0700 "$ROOT/.build/test-runs"
OUTPUT="$(mktemp -d "$ROOT/.build/test-runs/amnezia.XXXXXX")"
chmod 0700 "$OUTPUT"
diagnose_failure() {
  local original="$1" command rc=0 started=$SECONDS
  local -a codes
  [ "$original" -ne 0 ] && [ -n "${GUEST_DIRS[1]:-}" ] || return 0
  install -m 0600 /dev/null "$OUTPUT/failure.tar"
  install -m 0600 /dev/null "$OUTPUT/capture-transport.log"
  install -m 0600 /dev/null "$OUTPUT/capture-result"
  command="$(quote_ssh_command python3 "${GUEST_DIRS[1]}/src/config/profiles/amnezia/tests/e2e/failure-capture.py" collect)" || return 2
  (
    ulimit -f 128
    timeout --signal=TERM --kill-after=2 110 ssh -F "$CLIENT_CONFIG" -T e2e-vm-1 -- "$command" \
      </dev/null 2>"$OUTPUT/capture-transport.log"
  ) | head -c 3145728 >"$OUTPUT/failure.tar"
  codes=("${PIPESTATUS[@]}")
  printf 'transport_exit_code=%s\narchive_pipe_exit_code=%s\nduration_seconds=%s\narchive_bytes=%s\n' \
    "${codes[0]}" "${codes[1]}" "$((SECONDS - started))" "$(stat -c %s "$OUTPUT/failure.tar")" >"$OUTPUT/capture-result"
  [ "${codes[0]}" = 0 ] && [ "${codes[1]}" = 0 ] || rc=1
  timeout --signal=TERM --kill-after=2 5 python3 \
    "$ROOT/config/profiles/amnezia/tests/e2e/failure-capture.py" validate "$OUTPUT/failure.tar" || rc=1
  return "$rc"
}
cleanup_acceptance_resources() {
  local cleanup_rc=0 vm command
  if [ -n "$LEASE_KEEPER_PID" ]; then
    kill "$LEASE_KEEPER_PID" >/dev/null 2>&1
    timeout --foreground --signal=KILL 2 tail --pid="$LEASE_KEEPER_PID" -f /dev/null >/dev/null 2>&1
    kill -KILL "$LEASE_KEEPER_PID" >/dev/null 2>&1
  fi
  for vm in "${!GUEST_DIRS[@]}"; do
    command="$(quote_ssh_command sudo -n find "${GUEST_DIRS[$vm]}" -depth -delete)"
    case "${GUEST_DIRS[$vm]}" in
      /var/tmp/subyard-worktree.*)
        timeout --foreground --signal=TERM --kill-after=2 8 ssh -F "$CLIENT_CONFIG" -T "e2e-vm-$vm" -- "$command" \
          </dev/null >/dev/null 2>&1 || cleanup_rc=3 ;;
      *) cleanup_rc=3 ;;
    esac
  done
  # The original facade retains release authentication and response handling.
  # shellcheck disable=SC2317
  ssh() { timeout --foreground --signal=TERM --kill-after=2 65 ssh "$@"; }
  release_lease || cleanup_rc=3
  unset -f ssh
  find "$LOCAL_TEMP" -depth -delete >/dev/null 2>&1 || cleanup_rc=3
  return "$cleanup_rc"
}
cleanup_acceptance() {
  local original=$? capture_rc=0 cleanup_rc=0 cleanup_pid started=$SECONDS
  trap - EXIT
  # A second signal (including the enclosing work deadline) must leave the
  # bounded capture/cleanup grace intact. Their native KILL limits still apply.
  trap '' INT TERM
  set +e
  diagnose_failure "$original" || capture_rc=$?
  # One owned process group gives the entire cleanup a hard 95-second ceiling.
  # Native cleanup timeouts stay in this group so the ceiling also fences children.
  set -m
  cleanup_acceptance_resources &
  cleanup_pid=$!
  set +m
  if timeout --signal=KILL 95 tail --pid="$cleanup_pid" -f /dev/null >/dev/null 2>&1; then
    wait "$cleanup_pid" 2>/dev/null || cleanup_rc=3
  else
    kill -KILL -- "-$cleanup_pid" >/dev/null 2>&1
    wait "$cleanup_pid" 2>/dev/null
    cleanup_rc=3
  fi
  install -m 0600 /dev/null "$OUTPUT/result"
  printf 'original_exit_code=%s\ncapture_exit_code=%s\ncleanup_exit_code=%s\n' \
    "$original" "$capture_rc" "$cleanup_rc" >"$OUTPUT/result"
  printf 'AMNEZIA_RESULT original_exit_code=%s capture_exit_code=%s cleanup_exit_code=%s duration_seconds=%s evidence=%s\n' \
    "$original" "$capture_rc" "$cleanup_rc" "$((SECONDS - started))" "$OUTPUT"
  [ "$original" -eq 0 ] || exit "$original"
  [ "$capture_rc" -eq 0 ] || exit 1
  exit "$cleanup_rc"
}
trap cleanup_acceptance EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
acquire_lease
start_lease_keeper

bundle="$LOCAL_TEMP/worktree.tar.gz"
build_bundle "$ROOT" "$bundle"
bundle_hash="$(sha256sum "$bundle" | awk '{print $1}')"
printf 'amnezia_source_bundle_sha256=%s base_fingerprint=%s lane=%s\n' "$bundle_hash" "$BASE_FINGERPRINT" "$lane"
prepare_guest 1 "$bundle" "$bundle_hash"
prepare_guest 2 "$bundle" "$bundle_hash"

owner_phase() {
  local phase="$1" directory="${GUEST_DIRS[1]}" command
  shift
  printf 'amnezia_acceptance_stage=owner-%s\n' "$phase"
  write_guest_command 1 "$directory" bash config/profiles/amnezia/tests/e2e/owner.sh "$phase" "$@" \
    | guest 1 dd "of=$directory/run.sh" status=none
  guest 1 chmod 0700 "$directory/run.sh" </dev/null
  if [ "$phase" = recovery-state-mount ]; then
    # Bound the native controller transport, including an unresponsive SSH session.
    command="$(quote_ssh_command timeout --signal=TERM --kill-after=5 1800 "$directory/run.sh")"
    timeout --signal=TERM --kill-after=5 1810 ssh -F "$CLIENT_CONFIG" -T e2e-vm-1 -- "$command" </dev/null
  else
    guest 1 "$directory/run.sh" </dev/null
  fi
  printf 'amnezia_acceptance_stage=owner-%s result=pass\n' "$phase"
}

client_probe() {
  printf 'amnezia_acceptance_stage=client-probe\n'
  guest 2 env SUBYARD_E2E_VM=2 bash "${GUEST_DIRS[2]}/src/config/profiles/amnezia/tests/e2e/client.sh" \
    probe --client "${1:-two}" --owner-ip "${VM_IP[1]}" --owner-port 22 --private-ip "${VM_IP[2]}" </dev/null
}

client_denied() {
  printf 'amnezia_acceptance_stage=client-denied\n'
  guest 2 env SUBYARD_E2E_VM=2 bash "${GUEST_DIRS[2]}/src/config/profiles/amnezia/tests/e2e/client.sh" \
    denied --client "${1:-two}" </dev/null
}

client_reboot_traffic() {
  printf 'amnezia_acceptance_stage=client-reboot-traffic\n'
  guest 2 env SUBYARD_E2E_VM=2 bash "${GUEST_DIRS[2]}/src/config/profiles/amnezia/tests/e2e/client.sh" \
    reboot-traffic </dev/null
}

native_app() {
  printf 'amnezia_acceptance_stage=native-app-%s\n' "$1"
  guest 2 env SUBYARD_E2E_VM=2 bash "${GUEST_DIRS[2]}/src/config/profiles/amnezia/tests/e2e/native-app.sh" "$@" </dev/null
}

prepare_native_server() {
  printf 'amnezia_acceptance_stage=protected-admin-transfer\n'
  guest 2 bash -ceu '
    directory=/var/tmp/subyard-amnezia-client
    [ ! -e "$directory" ] && [ ! -L "$directory" ]
    install -d -o 0 -g 0 -m 0700 "$directory"
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -qq
    apt-get install -y -qq docker.io curl >/dev/null
    systemctl start docker
  ' </dev/null
  guest 1 incus exec yard-vpn-e2e --project subyard-vpn-e2e -- \
    cat /srv/amnezia/admin.key </dev/null \
    | guest 2 bash -ceu '
        destination=/var/tmp/subyard-amnezia-client/admin.key
        install -o 0 -g 0 -m 0600 /dev/stdin "$destination"
        test -s "$destination"
      '
  native_app install
  native_app setup --host "${VM_IP[1]}" --ssh-port 2226 --user amnezia \
    --key-file /var/tmp/subyard-amnezia-client/admin.key --udp-port 51820
}

prepare_native_exports() {
  prepare_native_server
  native_app share --name acceptance-one --output-file /var/tmp/subyard-amnezia-client/client-one.conf
  native_app share --name acceptance-two --output-file /var/tmp/subyard-amnezia-client/client-two.conf
  guest 2 python3 - <<'PYCLIENTS'
from pathlib import Path
root = Path('/var/tmp/subyard-amnezia-client')
def identity(name):
    values = dict((key.strip(), value.strip()) for key, value in
                  (line.split('=', 1) for line in (root / ('client-' + name + '.conf')).read_text().splitlines() if '=' in line))
    return tuple(values[key].strip() for key in ('PrivateKey', 'Address'))
one, two = identity('one'), identity('two')
assert all(left != right for left, right in zip(one, two)), 'native clients do not have independent identities'
print('ok: two native clients have independent keys and addresses')
PYCLIENTS
}

prepare_native_clients() {
  prepare_native_exports
  # Transfer the exact native image by a temporary tag: image IDs depend on the
  # source daemon's image store and need not resolve in the destination daemon.
  guest 1 incus exec yard-vpn-e2e --project subyard-vpn-e2e -- sh -ceu '
    reference=subyard-amnezia-e2e/native:source
    if docker image inspect "$reference" >/dev/null 2>&1; then
      printf "native image transfer reference already exists\n" >&2
      exit 1
    fi
    image="$(docker inspect --format "{{.Image}}" amnezia-awg2)"
    docker image tag "$image" "$reference"
    cleanup_reference() { docker image rm "$reference" >/dev/null; }
    trap cleanup_reference EXIT
    [ "$(docker image inspect --format "{{.Id}}" "$reference")" = "$image" ]
    docker image save "$reference"
  ' </dev/null | guest 2 docker image load >/dev/null
  guest 2 bash -ceu '
    docker image inspect --format "{{.Id}}" subyard-amnezia-e2e/native:source \
      | install -o 0 -g 0 -m 0600 /dev/stdin /var/tmp/subyard-amnezia-client/image
  ' </dev/null
  guest 1 incus exec yard-vpn-e2e --project subyard-vpn-e2e -- \
    docker exec amnezia-awg2 ip -4 -j address show dev awg0 </dev/null \
    | guest 2 python3 -c 'import json,os,sys; path="/var/tmp/subyard-amnezia-client/tunnel-ip"; fd=os.open(path,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600); os.fchmod(fd,0o600); os.write(fd,(json.load(sys.stdin)[0]["addr_info"][0]["local"]+"\n").encode()); os.close(fd)'
  client_probe one
  client_probe two
  native_app revoke --name acceptance-one
  client_denied one
  client_probe two
  owner_phase native-baseline
}

reboot_owner() {
  local before after='' down=0 ready=0
  printf 'amnezia_acceptance_stage=owner-host-reboot\n'
  before="$(guest 1 cat /proc/sys/kernel/random/boot_id </dev/null)"
  cleanup_guest 1
  timeout --foreground 20 ssh -F "$CLIENT_CONFIG" -T \
    -o ConnectTimeout=3 -o ConnectionAttempts=1 \
    e2e-vm-1 -- sudo -n systemctl reboot </dev/null >/dev/null 2>&1 || true
  for _ in $(seq 1 60); do
    if ! timeout --foreground 5 ssh -F "$CLIENT_CONFIG" -T \
      -o ConnectTimeout=2 -o ConnectionAttempts=1 e2e-vm-1 -- true \
      </dev/null >/dev/null 2>&1; then down=1; break; fi
    sleep 1
  done
  [ "$down" = 1 ] || die 'owner host did not go down for reboot'
  for _ in $(seq 1 180); do
    after="$(timeout --foreground 6 ssh -F "$CLIENT_CONFIG" -T \
      -o ConnectTimeout=3 -o ConnectionAttempts=1 e2e-vm-1 -- \
      cat /proc/sys/kernel/random/boot_id </dev/null 2>/dev/null)" || after=''
    [ -n "$after" ] && [ "$after" != "$before" ] && { ready=1; break; }
    sleep 1
  done
  [ "$ready" = 1 ] || die 'owner host did not return with a new boot ID'
  prepare_guest 1 "$bundle" "$bundle_hash"
  verify_boot_result
}

verify_boot_result() {
  local state started rc command uptime _ now remaining active sub result status
  local policy_deadline=0 local_deadline=0
  # Match P0's 900-second fixture observation policy, anchored to the first
  # current-boot service start. The native 120-second attempt can retry exit 75.
  IFS=' ' read -r uptime _ < /proc/uptime || return 2
  # SSH can return while the unit is still queued behind network-online/Incus.
  # Match P0's separate, fixed 30-second wait for its first current-boot start.
  local_deadline=$((${uptime%%.*} + 30))
  command="$(quote_ssh_command sh -ceu '
    systemctl show subyard-power-reconcile.service \
      --property=LoadState --property=ActiveState --property=SubState \
      --property=Result --property=ExecMainStatus \
      --property=ExecMainStartTimestampMonotonic
    printf "BootUptimeSeconds="
    cut -d. -f1 /proc/uptime
  ')" || return 2
  printf 'amnezia_acceptance_stage=verify-boot-result\n'
  while :; do
    rc=0
    IFS=' ' read -r uptime _ < /proc/uptime || return 2
    now="${uptime%%.*}"
    remaining=$((local_deadline - now))
    [ "$remaining" -gt 0 ] || die 'boot power reconciliation observation deadline expired'
    [ "$remaining" -le 10 ] || remaining=10
    state="$(timeout --foreground --signal=KILL "$remaining" ssh -F "$CLIENT_CONFIG" -T \
      -o ConnectTimeout=3 -o ConnectionAttempts=1 -o ServerAliveInterval=2 -o ServerAliveCountMax=2 \
      e2e-vm-1 -- "$command" </dev/null)" || rc=$?
    if [ "$rc" -ne 0 ]; then
      printf 'amnezia_acceptance_boot_state_read_exit=%s\n' "$rc" >&2
      return "$rc"
    fi
    started="$(sed -n 's/^ExecMainStartTimestampMonotonic=//p' <<<"$state")"
    uptime="$(sed -n 's/^BootUptimeSeconds=//p' <<<"$state")"
    if [ "$policy_deadline" -eq 0 ] && [ "$started" = 0 ] \
      && [[ "$uptime" =~ ^(0|[1-9][0-9]{0,11})$ ]] \
      && [ "$(sed -n 's/^LoadState=//p' <<<"$state")" = loaded ] \
      && [ "$(sed -n 's/^ActiveState=//p' <<<"$state")" = inactive ] \
      && [ "$(sed -n 's/^SubState=//p' <<<"$state")" = dead ] \
      && [ "$(sed -n 's/^Result=//p' <<<"$state")" = success ] \
      && [ "$(sed -n 's/^ExecMainStatus=//p' <<<"$state")" = 0 ]; then
      IFS=' ' read -r uptime _ < /proc/uptime || return 2
      [ "${uptime%%.*}" -lt "$local_deadline" ] \
        || die 'boot power reconciliation did not start before its observation deadline'
      sleep 1
      continue
    fi
    grep -Fxq 'LoadState=loaded' <<<"$state" \
      && [[ "$started" =~ ^[1-9][0-9]{0,17}$ ]] && [[ "$uptime" =~ ^(0|[1-9][0-9]{0,11})$ ]] \
      && [ "$((started / 1000000))" -le "$uptime" ] \
      || die 'boot power reconciliation current-boot state is unavailable or invalid'
    if [ "$policy_deadline" -eq 0 ]; then
      policy_deadline=$(((started + 999999) / 1000000 + 900))
      remaining=$((policy_deadline - uptime))
      [ "$remaining" -ge 0 ] || remaining=0
      # This never resets when exit 75 schedules another native attempt.
      IFS=' ' read -r uptime _ < /proc/uptime || return 2
      local_deadline=$((${uptime%%.*} + remaining))
      uptime="$(sed -n 's/^BootUptimeSeconds=//p' <<<"$state")"
    fi
    if grep -Fxq 'LoadState=loaded' <<<"$state" \
      && grep -Fxq 'ActiveState=inactive' <<<"$state" \
      && grep -Fxq 'SubState=dead' <<<"$state" \
      && grep -Fxq 'Result=success' <<<"$state" \
      && grep -Fxq 'ExecMainStatus=0' <<<"$state" \
      && [[ "$started" =~ ^[1-9][0-9]*$ ]]; then
      printf '%s\n' "$state"
      return 0
    fi
    if [ "$uptime" -ge "$policy_deadline" ]; then
      printf '%s\n' "$state"
      die 'boot power reconciliation did not succeed in the current boot'
    fi
    active="$(sed -n 's/^ActiveState=//p' <<<"$state")"
    sub="$(sed -n 's/^SubState=//p' <<<"$state")"
    result="$(sed -n 's/^Result=//p' <<<"$state")"
    status="$(sed -n 's/^ExecMainStatus=//p' <<<"$state")"
    case "$active/$sub/$result/$status" in
      activating/start/success/0 | active/running/success/0 | activating/auto-restart/success/75 | activating/auto-restart/exit-code/75) ;;
      *) printf '%s\n' "$state"; die 'boot power reconciliation has no pending retry or terminal success' ;;
    esac
    sleep 1
  done
}

# A fresh dev session receives product platform enrollment for the remaining commands.
owner_phase bootstrap
if [ "$lane" = disabled ] || [ "$lane" = reconnect ]; then
  owner_phase init-isolated
else
  owner_phase init
fi
if [ "$lane" = setup ]; then
  prepare_native_server
  native_app close
  printf 'amnezia_acceptance=result-pass source_bundle_sha256=%s lane=%s\n' "$bundle_hash" "$lane"
  exit 0
fi
if [ "$lane" = share ]; then
  prepare_native_exports
  native_app discover --name acceptance-two
  native_app close
  printf 'amnezia_acceptance=result-pass source_bundle_sha256=%s lane=%s\n' "$bundle_hash" "$lane"
  exit 0
fi
prepare_native_clients
if [ "$lane" = resources ] || [ "$lane" = full ]; then
  owner_phase resources
  client_probe
  owner_phase priority-neutral
  owner_phase priority-start 100
  client_probe
  owner_phase priority-finish 100
  owner_phase priority-high
  owner_phase priority-start 1000
  client_probe
  owner_phase priority-finish 1000
  owner_phase priority-guard
  client_probe
fi
if [ "$lane" = resources ]; then
  printf 'amnezia_acceptance_stage=free-page-reporting-before-reboot\n'
  guest 1 env SUBYARD_E2E_VM=1 bash "${GUEST_DIRS[1]}/src/dev/e2e/vm-page-reporting.sh" \
    subyard-vpn-e2e yard-vpn-e2e </dev/null
  owner_phase restart-enabled
  client_probe
  client_reboot_traffic
  reboot_owner
  owner_phase verify-enabled
  client_probe
  printf 'amnezia_acceptance_stage=free-page-reporting-after-reboot\n'
  guest 1 env SUBYARD_E2E_VM=1 bash "${GUEST_DIRS[1]}/src/dev/e2e/vm-page-reporting.sh" \
    subyard-vpn-e2e yard-vpn-e2e </dev/null
  for client in one two; do
    guest 2 env SUBYARD_E2E_VM=2 bash "${GUEST_DIRS[2]}/src/config/profiles/amnezia/tests/e2e/client.sh" cleanup --client "$client" </dev/null
  done
  native_app close
  printf 'amnezia_acceptance=result-pass source_bundle_sha256=%s lane=%s\n' "$bundle_hash" "$lane"
  exit 0
fi
owner_phase admin-down
client_probe
owner_phase admin-up
native_app discover --name acceptance-two
if [ "$lane" = full ]; then
  owner_phase deselect
  client_denied
  owner_phase reselect
  client_probe
fi
owner_phase isolation
client_probe
if [ "$lane" = recovery ] || [ "$lane" = full ]; then
  owner_phase recovery-state-mount
  client_probe
  owner_phase recovery-agent-loss
  client_probe
fi
if [ "$lane" = recovery ]; then
  for client in one two; do
    guest 2 env SUBYARD_E2E_VM=2 bash "${GUEST_DIRS[2]}/src/config/profiles/amnezia/tests/e2e/client.sh" cleanup --client "$client" </dev/null
  done
  native_app close
  printf 'amnezia_acceptance=result-pass source_bundle_sha256=%s lane=%s\n' "$bundle_hash" "$lane"
  exit 0
fi
if [ "$lane" = full ]; then
  printf 'amnezia_acceptance_stage=free-page-reporting-before-reboot\n'
  guest 1 env SUBYARD_E2E_VM=1 bash "${GUEST_DIRS[1]}/src/dev/e2e/vm-page-reporting.sh" \
    subyard-vpn-e2e yard-vpn-e2e </dev/null
  owner_phase repeat
  client_probe
  owner_phase work-load
  client_probe
  owner_phase work-stop
  client_probe
fi
if [ "$lane" != disabled ] && [ "$lane" != reconnect ]; then
  if [ "$lane" = startup ]; then
    owner_phase repeat
  fi
  owner_phase restart-enabled
  client_probe
  client_reboot_traffic
  reboot_owner
  owner_phase verify-enabled
  native_app discover --name acceptance-two
  client_probe
  if [ "$lane" != startup ]; then
    printf 'amnezia_acceptance_stage=free-page-reporting\n'
    guest 1 env SUBYARD_E2E_VM=1 bash "${GUEST_DIRS[1]}/src/dev/e2e/vm-page-reporting.sh" \
      subyard-vpn-e2e yard-vpn-e2e </dev/null
    owner_phase isolation-off
    client_probe
    client_reboot_traffic
    reboot_owner
    owner_phase verify-enabled
    native_app discover --name acceptance-two
    client_probe
  fi
fi
owner_phase down
client_denied
if [ "$lane" != reconnect ]; then
  owner_phase restart-disabled
fi
client_reboot_traffic
reboot_owner
owner_phase verify-disabled
client_denied
if [ "$lane" = reconnect ] || [ "$lane" = full ]; then
  # Prove that packets sent while disabled created the stale pre-NAT flow.
  # Print only its presence, never connection tuples.
  guest 1 bash -ceu '
    endpoint="$1"
    entries="$(conntrack -L -f ipv4 -p udp --orig-dst "$endpoint" \
      --orig-port-dst 51820 --reply-src "$endpoint" 2>/dev/null)"
    [ -n "$entries" ] || { printf "expected stale untranslated UDP flow\n" >&2; exit 1; }
    printf "ok: stale untranslated UDP flow present before explicit up\n"
  ' -- "${VM_IP[1]}" </dev/null
fi
owner_phase up
native_app discover --name acceptance-two
client_probe
for client in one two; do
  guest 2 env SUBYARD_E2E_VM=2 bash "${GUEST_DIRS[2]}/src/config/profiles/amnezia/tests/e2e/client.sh" cleanup --client "$client" </dev/null
done
native_app close
printf 'amnezia_acceptance=result-pass source_bundle_sha256=%s lane=%s\n' "$bundle_hash" "$lane"
