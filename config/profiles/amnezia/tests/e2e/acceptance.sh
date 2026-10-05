#!/usr/bin/env bash
# Exercise the owner and client on one disposable two-VM lease and one source bundle.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." && pwd -P)"
# shellcheck source=dev/agent-e2e.sh
. "$ROOT/dev/agent-e2e.sh"
VM_COUNT_REQUESTED=2
VM_COUNT=2

lane=full
usage() { printf 'Usage: config/profiles/amnezia/tests/e2e/acceptance.sh --slot N [--lane full|reboot|recovery|startup|disabled|reconnect]\n'; }
while [ "$#" -gt 0 ]; do
  case "$1" in
    --slot)
      [ "$#" -ge 2 ] && [ -z "$LEASE_REQUESTED_SLOT" ] || die '--slot N is required once'
      set_requested_slot "$2" --slot
      shift 2
      ;;
    --lane)
      [ "$#" -ge 2 ] || die '--lane requires full, reboot, recovery, startup, disabled or reconnect'
      case "$2" in full|reboot|recovery|startup|disabled|reconnect) lane="$2" ;; *) die '--lane requires full, reboot, recovery, startup, disabled or reconnect' ;; esac
      shift 2
      ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown argument '$1'" ;;
  esac
done
[ -n "$LEASE_REQUESTED_SLOT" ] || die '--slot N is required'

LOCAL_TEMP="$(mktemp -d "${TMPDIR:-/tmp}/subyard-agent-e2e.XXXXXX")"
LEASE_PURPOSE=amnezia-acceptance
diagnose_failure() {
  local rc="$1"
  if [ "$rc" -ne 0 ] && [ -n "${GUEST_DIRS[1]:-}" ]; then
    printf 'amnezia_acceptance_failure_exit=%s\n' "$rc" >&2
    # Bound read-only probes and report timings only, never runtime/config contents.
    guest 1 timeout 100 bash -c '
      probe() {
        local label="$1" started=$SECONDS rc=0
        shift
        timeout 20 "$@" >/dev/null 2>&1 || rc=$?
        printf "startup_probe=%s duration_seconds=%s exit=%s\n" "$label" "$((SECONDS - started))" "$rc"
      }
      probe guest-observe incus exec yard-vpn-e2e --project subyard-vpn-e2e -- \
        python3 /usr/local/lib/subyard-amnezia/runtime.py observe
      probe owner-config incus query "/1.0/instances?recursion=1&all-projects=true"
      probe owner-state incus list --all-projects --format=json
      probe guest-projects incus exec yard-vpn-e2e --project subyard-vpn-e2e -- \
        find /srv/workspaces -mindepth 1 -maxdepth 1 -print -quit
    ' >&2 || true
    guest 1 timeout 15 free -b >&2 || true
    guest 1 timeout 15 sh -c '
      journalctl -k -b --no-pager -o short-iso \
        | grep -iE "out of memory|oom-kill|killed process" | tail -n 30
      journalctl -b -u systemd-oomd --no-pager -n 20
    ' >&2 || true
    guest 1 timeout 15 systemctl show subyard-power-reconcile.service \
      --property=LoadState --property=ActiveState --property=SubState \
      --property=Result --property=ExecMainStatus \
      --property=ExecMainStartTimestampMonotonic --property=NRestarts >&2 || true
    guest 1 timeout 15 journalctl -b -u subyard-power-reconcile.service \
      --no-pager -n 60 >&2 || true
    guest 1 timeout 15 incus list yard-vpn-e2e --project subyard-vpn-e2e \
      --format csv -c ns >&2 || true
    guest 1 timeout 15 incus info yard-vpn-e2e --project subyard-vpn-e2e \
      --show-log >&2 || true
    guest 1 timeout 15 incus console yard-vpn-e2e --project subyard-vpn-e2e \
      --show-log | tail -n 100 >&2 || true
  fi
  return "$rc"
}
cleanup_acceptance() {
  local rc=$?
  set +e
  diagnose_failure "$rc"
  cleanup_on_exit
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
  local phase="$1" directory="${GUEST_DIRS[1]}"
  printf 'amnezia_acceptance_stage=owner-%s\n' "$phase"
  write_guest_command 1 "$directory" bash config/profiles/amnezia/tests/e2e/owner.sh "$phase" \
    | guest 1 dd "of=$directory/run.sh" status=none
  guest 1 chmod 0700 "$directory/run.sh" </dev/null
  guest 1 "$directory/run.sh" </dev/null
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

prepare_native_clients() {
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
  # Use precisely the native server image for independent protocol-compatible probes.
  guest 1 incus exec yard-vpn-e2e --project subyard-vpn-e2e -- sh -ceu '
    docker image save "$(docker inspect --format "{{.Image}}" amnezia-awg2)"
  ' </dev/null | guest 2 docker image load >/dev/null
  guest 1 incus exec yard-vpn-e2e --project subyard-vpn-e2e -- \
    docker inspect --format '{{.Image}}' amnezia-awg2 </dev/null \
    | guest 2 install -o 0 -g 0 -m 0600 /dev/stdin /var/tmp/subyard-amnezia-client/image
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
}

verify_boot_result() {
  local state started rc
  printf 'amnezia_acceptance_stage=verify-boot-result\n'
  state="$(guest 1 systemctl show subyard-power-reconcile.service \
    --property=LoadState --property=ActiveState --property=SubState \
    --property=Result --property=ExecMainStatus \
    --property=ExecMainStartTimestampMonotonic </dev/null)" || {
      rc=$?
      printf 'amnezia_acceptance_boot_state_read_exit=%s\n' "$rc" >&2
      return "$rc"
    }
  printf '%s\n' "$state"
  started="$(sed -n 's/^ExecMainStartTimestampMonotonic=//p' <<<"$state")"
  grep -Fxq 'LoadState=loaded' <<<"$state" \
    && grep -Fxq 'ActiveState=inactive' <<<"$state" \
    && grep -Fxq 'SubState=dead' <<<"$state" \
    && grep -Fxq 'Result=success' <<<"$state" \
    && grep -Fxq 'ExecMainStatus=0' <<<"$state" \
    && [[ "$started" =~ ^[1-9][0-9]*$ ]] \
    || die 'boot power reconciliation did not succeed in the current boot'
}

# First init grants Incus access through the product's normal bootstrap. A new
# dev session then receives that group membership for the remaining commands.
owner_phase bootstrap
if [ "$lane" = disabled ] || [ "$lane" = reconnect ]; then
  owner_phase init-isolated
else
  owner_phase init
fi
prepare_native_clients
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
  verify_boot_result
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
    verify_boot_result
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
verify_boot_result
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
