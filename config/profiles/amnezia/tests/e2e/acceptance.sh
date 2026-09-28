#!/usr/bin/env bash
# Exercise the owner and client on one disposable two-VM lease and one source bundle.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." && pwd -P)"
# shellcheck source=dev/agent-e2e.sh
. "$ROOT/dev/agent-e2e.sh"

lane=full
usage() { printf 'Usage: config/profiles/amnezia/tests/e2e/acceptance.sh --slot N [--lane full|reboot|recovery]\n'; }
while [ "$#" -gt 0 ]; do
  case "$1" in
    --slot)
      [ "$#" -ge 2 ] && [ -z "$LEASE_REQUESTED_SLOT" ] || die '--slot N is required once'
      set_requested_slot "$2" --slot
      shift 2
      ;;
    --lane)
      [ "$#" -ge 2 ] || die '--lane requires full, reboot or recovery'
      case "$2" in full|reboot|recovery) lane="$2" ;; *) die '--lane requires full, reboot or recovery' ;; esac
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
    probe --owner-ip "${VM_IP[1]}" --owner-port 22 --private-ip "${VM_IP[2]}" </dev/null
}

client_denied() {
  printf 'amnezia_acceptance_stage=client-denied\n'
  guest 2 env SUBYARD_E2E_VM=2 bash "${GUEST_DIRS[2]}/src/config/profiles/amnezia/tests/e2e/client.sh" \
    denied </dev/null
}

client_reboot_traffic() {
  printf 'amnezia_acceptance_stage=client-reboot-traffic\n'
  guest 2 env SUBYARD_E2E_VM=2 bash "${GUEST_DIRS[2]}/src/config/profiles/amnezia/tests/e2e/client.sh" \
    reboot-traffic </dev/null
}

transfer_client_config() {
  printf 'amnezia_acceptance_stage=protected-client-transfer\n'
  guest 1 incus exec yard-vpn-e2e --project subyard-vpn-e2e -- \
    cat /srv/amnezia/client.conf </dev/null \
    | guest 2 bash -ceu '
        directory=/var/tmp/subyard-amnezia-client
        [ ! -L "$directory" ]
        install -d -m 0700 "$directory"
        temporary="$(mktemp "$directory/.client.conf.XXXXXX")"
        trap '\''rm -f -- "$temporary"'\'' EXIT
        cat > "$temporary"
        [ -s "$temporary" ]
        chmod 0600 "$temporary"
        mv -f -- "$temporary" "$directory/client.conf"
      '
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
owner_phase init
owner_phase up
transfer_client_config
client_probe
if [ "$lane" = full ]; then
  owner_phase deselect
  client_denied
  owner_phase reselect
  client_probe
fi
owner_phase isolation
client_probe
if [ "$lane" = recovery ]; then
  owner_phase recovery-state-mount
  client_probe
  owner_phase recovery-agent-loss
  client_probe
  guest 2 env SUBYARD_E2E_VM=2 bash "${GUEST_DIRS[2]}/src/config/profiles/amnezia/tests/e2e/client.sh" cleanup </dev/null
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
owner_phase restart-enabled
client_probe
client_reboot_traffic
reboot_owner
owner_phase verify-enabled
verify_boot_result
client_probe
printf 'amnezia_acceptance_stage=free-page-reporting\n'
guest 1 env SUBYARD_E2E_VM=1 bash "${GUEST_DIRS[1]}/src/dev/e2e/vm-page-reporting.sh" \
  subyard-vpn-e2e yard-vpn-e2e </dev/null
owner_phase isolation-off
client_probe
client_reboot_traffic
reboot_owner
owner_phase verify-enabled
verify_boot_result
client_probe
owner_phase down
client_denied
owner_phase restart-disabled
reboot_owner
owner_phase verify-disabled
verify_boot_result
client_denied
guest 2 env SUBYARD_E2E_VM=2 bash "${GUEST_DIRS[2]}/src/config/profiles/amnezia/tests/e2e/client.sh" cleanup </dev/null
printf 'amnezia_acceptance=result-pass source_bundle_sha256=%s lane=%s\n' "$bundle_hash" "$lane"
