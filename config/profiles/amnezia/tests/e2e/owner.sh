#!/usr/bin/env bash
# Run only on an allocated owner; retain the fixture for same-lease data-path checks.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." && pwd)"
# shellcheck source=tests/helpers/release-candidate.sh
. "$root/tests/helpers/release-candidate.sh"
if YARD_BIN="$(release_candidate_prepare "$root")"; then
  unset YARD_ENGINE_PATH
else
  candidate_rc=$?
  [ "$candidate_rc" = 1 ] || exit "$candidate_rc"
  YARD_BIN="$root/.build/yard"
fi
phase="${1:-init}"
die() { printf 'amnezia-profile-e2e: %s\n' "$*" >&2; exit 1; }
[ -n "${SUBYARD_E2E_VM:-}" ] || die 'run through dev/agent-e2e.sh'
[ "$(id -un)" = dev ] || die 'run as the allocated dev user'
fixture=/var/tmp/subyard-amnezia-profile-e2e
if [ -e "$fixture" ]; then
  [ ! -L "$fixture" ] && [ "$(cat "$fixture/.marker")" = subyard-amnezia-e2e-v1 ] \
    || die 'foreign fixture directory'
else
  install -d -m 0700 "$fixture"
  printf '%s\n' subyard-amnezia-e2e-v1 > "$fixture/.marker"
fi
export SUBYARD_CONFIG_HOME="$fixture/config" SUBYARD_HOME="$fixture/data"
export SUBYARD_OPERATOR_HOME="$HOME" SUBYARD_NO_AUDIT=1 SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1
export MIN_DISK_GIB=1
# shellcheck source=scripts/lib/host.sh
. "$root/scripts/lib/host.sh"
# Guest source trees are disposable across reboots; keep this lease's engine on disk.
if [ "$YARD_BIN" = "$root/.build/yard" ]; then
  "$root/dev/build-engine.sh" --output "$fixture/yard"
  install -D -m 0755 "$fixture/yard" "$YARD_BIN"
else
  candidate_version="$(jq -er '.version | select(type == "string" and test("^[A-Za-z0-9._+-]+$"))' \
    "$root/.subyard-acceptance/candidate.json")"
  if [ ! -e "$fixture/bin/yard" ] && [ ! -L "$fixture/bin/yard" ]; then
    YARD_RUNTIME_ROOT="$SUBYARD_HOME/runtime" YARD_RELEASE_CACHE="$SUBYARD_HOME/releases" \
      YARD_BIN_DIR="$fixture/bin" YARD_SHELL_RC="$fixture/bashrc" YARD_LOGIN_RC="$fixture/profile" \
      YARD_RELEASE_BASE_URL="file://$root/.subyard-acceptance/release" \
      YARD_RELEASE_VERSION="$candidate_version" \
      "$root/.subyard-acceptance/release/subyard-install.sh" --yes >/dev/null
  fi
  YARD_BIN="$fixture/bin/yard"
  [ -L "$YARD_BIN" ] \
    && [ "$(readlink "$YARD_BIN")" = "$SUBYARD_HOME/runtime/current/bin/yard" ] \
    && [ "$("$YARD_BIN" --version)" = "yard $candidate_version" ] \
    || die 'installed candidate runtime is not active'
  printf 'amnezia_installed_candidate_verified=true\n'
fi
yard() { "$YARD_BIN" -Y vpn-e2e "$@"; }
guest() { incus exec yard-vpn-e2e --project subyard-vpn-e2e -- "$@"; }
verify_resources() {
  [ "$(incus config get yard-vpn-e2e limits.cpu --project subyard-vpn-e2e)" = 2 ] \
    && [ "$(incus config get yard-vpn-e2e limits.memory --project subyard-vpn-e2e)" = 2GiB ] \
    || die 'VM CPU or memory limits do not match the preset'
  guest python3 - <<'PYRESOURCES'
import os
from pathlib import Path
memory = next(int(line.split()[1]) for line in Path('/proc/meminfo').read_text().splitlines()
              if line.startswith('MemTotal:'))
assert os.cpu_count() == 2, 'guest CPU count does not match the preset'
assert 1850 * 1024 <= memory < 2048 * 1024, 'guest memory does not match the 2 GiB ceiling'
print(f'amnezia_guest_cpus=2 memory_kib={memory}')
PYRESOURCES
}
prepare_work_yard() {
  local definition="$SUBYARD_CONFIG_HOME/yards/work-e2e/config.env"
  if [ ! -f "$definition" ]; then
    install -d -m 0700 "$(dirname "$definition")"
    cat > "$definition" <<'CONFIG'
YARD_KIND=container
ENVIRONMENT_PROFILES=
CODING_TOOL_INTEGRATIONS=
HOST_MOUNTS=
HOST_LINKS=
FORWARD_SSH_AGENT=0
NESTED_E2E_VMS=0
LIMITS_CPU=3
LIMITS_MEMORY=512MiB
SSH_PORT=2227
CONFIG
    chmod 0600 "$definition"
  fi
  "$YARD_BIN" -Y work-e2e init --yes
  "$YARD_BIN" -Y work-e2e start --yes
}
verify_cpu_policy() {
  sudo -n "$YARD_BIN" _vm-cpu check subyard-vpn-e2e yard-vpn-e2e \
    || die 'VM host CPU scheduling policy is not effective'
}
signature() {
  local state
  # The VM agent can return before Docker has restored the native container.
  # Compare both files only after they are readable; never hash partial state.
  state="$(guest timeout 100 sh -ceu '
    for attempt in $(seq 1 45); do
      if docker container inspect --format "{{.State.Running}}" amnezia-awg2 2>/dev/null | grep -Fxq true \
        && server="$(docker exec amnezia-awg2 sha256sum /opt/amnezia/awg/awg0.conf 2>/dev/null)"; then
        sha256sum /srv/amnezia/admin.key
        printf "%s\n" "$server"
        printf "amnezia_native_state_wait_attempts=%s\n" "$attempt" >&2
        exit 0
      fi
      sleep 2
    done
    printf "native VPN state is unavailable\n" >&2
    exit 1
  ')" || return "$?"
  printf '%s\n' "$state" | sha256sum
}
verify_state() {
  local observed
  YARD_KIND=vm incus_wait_instance_agent subyard-vpn-e2e yard-vpn-e2e || die 'VM agent unavailable'
  verify_resources
  observed="$(signature)" || die 'VPN state is unavailable after lifecycle'
  [ "$observed" = "$(cat "$fixture/state.sha256")" ] || die 'VPN state changed across lifecycle'
}
verify_enabled() {
  verify_state
  for _ in $(seq 1 45); do
    if yard vpn status | jq -e '.ready and .network_enabled and .ingress and .vpn_running' >/dev/null; then
      yard security --require-live --quiet
      return
    fi
    sleep 2
  done
  die 'enabled VPN did not recover'
}
verify_disabled() {
  verify_state
  yard vpn status | jq -e '(.ready or .network_enabled or .ingress) | not' >/dev/null \
    || die 'disabled VPN recovered unexpectedly'
}
verify_deselected() {
  verify_state
  incus query '/1.0/instances/yard-vpn-e2e?project=subyard-vpn-e2e' \
    | jq -e '(.devices["amnezia-vpn"] == null) and (.config["user.subyard.resource.amnezia-vpn"] == null) and (.devices["amnezia-admin"] == null) and (.config["user.subyard.resource.amnezia-admin"] == null)' >/dev/null
  guest python3 /usr/local/lib/subyard-amnezia/runtime.py observe \
    | jq -e '(.running or .enabled) | not' >/dev/null \
    || die 'deselected VPN runtime remains enabled'
}
verify_pending_ingress() {
  incus query '/1.0/instances/yard-vpn-e2e?project=subyard-vpn-e2e' \
    | jq -e '(.devices["amnezia-vpn"] == null) and
             (.config["user.subyard.resource.amnezia-vpn"] | test("^v1:pending:[0-9a-f]{64}$"))' >/dev/null \
    || die 'unverified shutdown did not leave only the pending ingress marker'
}
restart_vpn_guest() {
  incus restart yard-vpn-e2e --project subyard-vpn-e2e --timeout 120 >/dev/null
  YARD_KIND=vm incus_wait_instance_agent subyard-vpn-e2e yard-vpn-e2e \
    || die 'VPN guest agent did not return after nested guest restart'
}
capture_agent_loss_state() {
  local diagnostic="$fixture/recovery-agent-loss.units"
  install -m 0600 /dev/null "$diagnostic"
  guest sh -ceu '
    systemctl show incus-agent.service \
      --property=LoadState --property=ActiveState --property=SubState --property=UnitFileState
    systemctl show subyard-amnezia-e2e-agent-loss.timer \
      --property=LoadState --property=ActiveState --property=SubState --property=Result
    systemctl show subyard-amnezia-e2e-agent-loss.service \
      --property=LoadState --property=ActiveState --property=SubState \
      --property=Result --property=ExecMainStatus
  ' >"$diagnostic" 2>&1 || true
  cat "$diagnostic"
}
wait_vpn_runtime_settled() {
  for _ in $(seq 1 45); do
    if guest python3 /usr/local/lib/subyard-amnezia/runtime.py observe \
      | jq -e '(.ready and .running and .enabled) or ((.running | not) and (.enabled | not))' >/dev/null; then
      return
    fi
    sleep 2
  done
  die 'VPN runtime did not settle before shutdown retry'
}
capture_retry_down_state() {
  guest sh -ceu '
    systemctl show subyard-amnezia.service \
      --property=ActiveState --property=SubState --property=Result --property=ExecMainStatus
    if docker container inspect amnezia-awg2 \
      --format "container_status={{.State.Status}} running={{.State.Running}} exit_code={{.State.ExitCode}}"; then
      :
    else
      printf "container_status=absent\n"
    fi
  ' || true
}
case "$phase" in
  bootstrap)
    [ ! -f "$SUBYARD_CONFIG_HOME/yards/vpn-e2e/config.env" ] || die 'expected a fresh VPN yard'
    python3 -B "$root/config/profiles/amnezia/tests/e2e/first-boot-observer.py" -- \
      "$YARD_BIN" -Y vpn-e2e init --profile amnezia --yes
    ;;
  init|init-isolated)
    if [ ! -f "$SUBYARD_CONFIG_HOME/yards/vpn-e2e/config.env" ]; then
      yard init --profile amnezia --yes
    else
      yard init --yes
    fi
    incus list yard-vpn-e2e --project subyard-vpn-e2e --format csv -c s \
      | grep -Fxq STOPPED || die 'profile init did not restore stopped power intent'
    # The allocated owner has a private address; configure it through the product
    # while stopped, before the first start publishes the endpoint.
    route="$(ip -4 -j route get 1.1.1.1)"
    address="$(jq -r '.[0].prefsrc' <<<"$route")"
    interface="$(jq -r '.[0].dev' <<<"$route")"
    yard config set RESOURCE_VPN_IPV4 "$address" --scope yard --yes
    yard config set RESOURCE_VPN_INTERFACE "$interface" --scope yard --yes
    yard config set RESOURCE_VPN_PORT 51820 --scope yard --yes
    if [ "$phase" = init-isolated ]; then
      yard network isolation on --yes
    fi
    yard start --yes
    verify_resources
    verify_cpu_policy
    yard vpn-admin up --yes
    yard vpn status | jq -e ".ready and .network_enabled and .ingress and (.vpn_installed | not)" >/dev/null
    status_started=$SECONDS
    status_output="$(yard status)"
    printf 'amnezia_status_probe_duration_seconds=%s\n' "$((SECONDS - status_started))"
    if ! grep -F 'Manage in AmneziaVPN: amnezia@' >/dev/null <<<"$status_output"; then
      # Report only the owned resources' bounded states, never their endpoints.
      awk '$1 == "amnezia" && ($2 == "vpn" || $2 == "vpn-admin") &&
           ($3 == "up" || $3 == "down" || $3 == "?") {
             printf "amnezia_status_resource=%s state=%s\n", $2, $3
           }' <<<"$status_output"
      die 'administrative status hint is missing'
    fi
    guest findmnt -n -o FSTYPE,SIZE /srv
    ;;
  admin-down)
    yard vpn-admin down --yes
    yard vpn-admin down --yes
    verify_enabled
    ;;
  admin-up)
    yard vpn-admin up --yes
    yard vpn-admin up --yes
    verify_enabled
    ;;
  native-baseline)
    signature > "$fixture/state.sha256"
    chmod 0600 "$fixture/state.sha256"
    verify_enabled
    guest df -B1 --output=size,used,avail /srv
    ;;
  up)
    before="$(signature)"
    yard vpn up --yes
    yard vpn up --yes
    [ "$(signature)" = "$before" ] || die 'repeat up changed VPN state'
    verify_enabled
    ;;
  repeat)
    before="$(signature)"
    yard init --profile amnezia --yes
    [ "$(signature)" = "$before" ] || die 'repeat reconciliation changed VPN state'
    yard vpn status
    ;;
  resources)
    # Exercise a lower CPU count on an existing native server, then converge
    # through public init without changing RAM, state or desired power.
    before="$(signature)"
    yard config set LIMITS_CPU 1 --scope yard --yes
    yard init --yes
    [ "$(incus config get yard-vpn-e2e limits.cpu --project subyard-vpn-e2e)" = 1 ] \
      && [ "$(incus config get yard-vpn-e2e limits.memory --project subyard-vpn-e2e)" = 2GiB ] \
      && [ "$(guest getconf _NPROCESSORS_ONLN)" = 1 ] \
      && [ "$(signature)" = "$before" ] || die 'prior CPU limit did not preserve the native server'
    yard config set LIMITS_CPU 2 --scope yard --yes
    yard init --yes
    verify_enabled
    before_pid="$(incus query '/1.0/instances/yard-vpn-e2e/state?project=subyard-vpn-e2e' | jq -er '.pid | select(. > 0)')"
    yard init </dev/null
    verify_cpu_policy
    after_pid="$(incus query '/1.0/instances/yard-vpn-e2e/state?project=subyard-vpn-e2e' | jq -er '.pid | select(. > 0)')"
    [ "$after_pid" = "$before_pid" ] && [ "$(signature)" = "$before" ] \
      || die 'converged resource reconciliation restarted the VM or changed native state'
    printf 'amnezia_resources_existing_upgrade=true repeat_noop=true native_state_preserved=true\n'
    ;;
  priority-neutral|priority-high)
    weight=100
    [ "$phase" != priority-high ] || weight=1000
    yard config set VM_CPU_WEIGHT "$weight" --scope yard --yes
    yard init --yes
    verify_enabled
    prepare_work_yard
    ;;
  priority-start)
    weight="${2:-}"
    case "$weight" in 100|1000) ;; *) die 'invalid contention weight' ;; esac
    unit="subyard-amnezia-priority-$weight.service"
    [ "$(systemctl show "$unit" -p LoadState --value)" = not-found ] \
      || die 'CPU contention unit already exists'
    sudo -n systemd-run --no-block --unit="$unit" --property=Type=oneshot \
      --property=RemainAfterExit=yes --property=TimeoutStartSec=150 \
      /usr/bin/python3 "$root/config/profiles/amnezia/tests/e2e/cpu-priority.py" "$fixture" "$weight"
    ready=0
    for _ in $(seq 1 45); do
      if sudo -n test -f "$fixture/priority-$weight.ready"; then ready=1; break; fi
      [ "$(systemctl show "$unit" -p ActiveState --value)" != failed ] \
        || die 'CPU contention probe failed before readiness'
      sleep 1
    done
    [ "$ready" = 1 ] || die 'CPU contention probe did not become ready'
    ;;
  priority-finish)
    weight="${2:-}"
    case "$weight" in 100|1000) ;; *) die 'invalid contention weight' ;; esac
    unit="subyard-amnezia-priority-$weight.service"
    sudo -n install -o 0 -g 0 -m 0600 /dev/null "$fixture/priority-$weight.client-done"
    finished=0
    for _ in $(seq 1 90); do
      state="$(systemctl show "$unit" -p SubState --value)"
      [ "$state" != failed ] || die 'CPU contention probe failed'
      if [ "$state" = exited ]; then finished=1; break; fi
      sleep 1
    done
    [ "$finished" = 1 ] \
      && [ "$(systemctl show "$unit" -p Result --value)" = success ] \
      && [ "$(systemctl show "$unit" -p ExecMainStatus --value)" = 0 ] \
      || die 'CPU contention probe did not finish successfully'
    sudo -n systemctl stop "$unit"
    if [ "$weight" = 1000 ]; then
      sudo -n python3 - "$fixture" <<'PYPRIORITY'
import json
from pathlib import Path
import sys
root = Path(sys.argv[1])
normal, high = (json.loads((root / f'priority-{weight}.json').read_text()) for weight in (100, 1000))
normal_ratio = normal['vm_cpu_usec'] / normal['neighbor_cpu_usec']
high_ratio = high['vm_cpu_usec'] / high['neighbor_cpu_usec']
print(f'amnezia_cpu_priority_neutral_ratio={normal_ratio:.3f} high_ratio={high_ratio:.3f}')
assert high_ratio > normal_ratio * 1.5, 'VM CPU priority did not improve CPU share under contention'
PYPRIORITY
      verify_enabled
    fi
    ;;
  priority-guard)
    [ -f /run/subyard-e2e-lease.json ] || die 'allocated owner required for CPU ceiling check'
    [ "$(systemctl show incus.service -p CPUQuotaPerSecUSec --value)" = infinity ] \
      || die 'CPU ceiling fixture requires an initially unlimited Incus service'
    yard stop --yes
    trap 'sudo -n systemctl set-property --runtime incus.service CPUQuota= >/dev/null 2>&1 || true' EXIT
    sudo -n systemctl set-property --runtime incus.service CPUQuota=50%
    diagnostic="$fixture/priority-ceiling.log"
    install -m 0600 /dev/null "$diagnostic"
    if yard start --yes >"$diagnostic" 2>&1; then die 'VM startup bypassed the Incus CPU ceiling'; fi
    grep -Fq 'cpu.max' "$diagnostic" || die 'CPU ceiling refusal lost its primary diagnostic'
    incus list yard-vpn-e2e --project subyard-vpn-e2e --format csv -c s \
      | grep -Fxq STOPPED || die 'rejected scheduling left the VM running'
    [ "$(incus config get yard-vpn-e2e user.subyard.desired_power --project subyard-vpn-e2e)" = stopped ] \
      || die 'rejected startup changed managed power intent'
    incus list yard-work-e2e --project subyard-work-e2e --format csv -c s \
      | grep -Fxq RUNNING || die 'VM scheduling refusal stopped the ordinary yard'
    sudo -n systemctl set-property --runtime incus.service CPUQuota=
    [ "$(systemctl show incus.service -p CPUQuotaPerSecUSec --value)" = infinity ] \
      || die 'Incus CPU ceiling was not restored'
    trap - EXIT
    yard start --yes
    verify_enabled
    verify_cpu_policy
    printf 'amnezia_cpu_ceiling_refusal=true stopped_intent_preserved=true neighbor_preserved=true\n'
    ;;
  down)
    before="$(signature)"
    yard vpn down --yes
    yard vpn down --yes
    [ "$(signature)" = "$before" ] || die 'shutdown changed VPN state'
    yard vpn status
    verify_disabled
    guest docker inspect --format '{{.State.Running}}' amnezia-awg2 | grep -Fxq true || die 'network shutdown stopped the application-owned VPN'
    ;;
  recovery-agent-loss)
    # The transient unit runs inside the nested VPN VM. It makes the next
    # owner-side guest call unavailable without changing the outer yard.
    guest systemctl is-active --quiet incus-agent.service \
      || die 'nested VPN guest agent service is not active before fault injection'
    agent_fragment="$(guest systemctl show incus-agent.service --property=FragmentPath --value)"
    case "$agent_fragment" in
      /run/systemd/system/*) agent_action=(/usr/bin/systemctl stop incus-agent.service) ;;
      *) agent_action=(/usr/bin/systemctl mask --runtime --now incus-agent.service) ;;
    esac
    guest systemd-run --unit=subyard-amnezia-e2e-agent-loss --on-active=1s \
      --timer-property=AccuracySec=1s \
      "${agent_action[@]}" >/dev/null
    agent_unavailable=0
    for _ in $(seq 1 20); do
      if ! guest true >/dev/null 2>&1; then
        agent_unavailable=1
        break
      fi
      sleep 1
    done
    if [ "$agent_unavailable" != 1 ]; then
      capture_agent_loss_state
      die 'nested VPN guest agent did not become unavailable'
    fi
    down_output="$fixture/recovery-agent-loss.down"
    install -m 0600 /dev/null "$down_output"
    printf 'amnezia_recovery_agent_step=lost-agent-down\n'
    if yard vpn down --yes >"$down_output" 2>&1; then
      die 'VPN shutdown unexpectedly verified after nested guest agent loss'
    fi
    if ! grep -Fq 'guest shutdown is unverified' "$down_output"; then
      tail -n 20 "$down_output" >&2
      die 'VPN shutdown failed before reporting unverified guest cleanup'
    fi
    printf 'amnezia_recovery_agent_step=pending-ingress\n'
    verify_pending_ingress
    printf 'amnezia_recovery_agent_step=nested-restart\n'
    restart_vpn_guest
    printf 'amnezia_recovery_agent_step=state-after-restart\n'
    verify_state
    printf 'amnezia_recovery_agent_step=runtime-settled-after-restart\n'
    wait_vpn_runtime_settled
    printf 'amnezia_recovery_agent_step=verified-down\n'
    retry_down_output="$fixture/recovery-agent-loss.retry-down"
    install -m 0600 /dev/null "$retry_down_output"
    retry_started=$SECONDS
    if ! yard vpn down --yes >"$retry_down_output" 2>&1; then
      printf 'amnezia_recovery_agent_retry_down_seconds=%s\n' "$((SECONDS - retry_started))" >&2
      tail -n 20 "$retry_down_output" >&2
      capture_retry_down_state >&2
      die 'VPN shutdown remained unverified after guest recovery'
    fi
    printf 'amnezia_recovery_agent_retry_down_seconds=%s\n' "$((SECONDS - retry_started))"
    printf 'amnezia_recovery_agent_step=disabled-verify\n'
    verify_disabled
    printf 'amnezia_recovery_agent_step=re-up\n'
    yard vpn up --yes
    printf 'amnezia_recovery_agent_step=enabled-verify\n'
    verify_enabled
    ;;
  recovery-state-mount)
    printf 'amnezia_recovery_mount_step=initial-down\n'
    mount_down_output="$fixture/recovery-state-mount.down"
    install -m 0600 /dev/null "$mount_down_output"
    mount_down_started=$SECONDS
    if ! yard vpn down --yes >"$mount_down_output" 2>&1; then
      printf 'amnezia_recovery_mount_down_seconds=%s\n' "$((SECONDS - mount_down_started))" >&2
      tail -n 20 "$mount_down_output" >&2
      capture_retry_down_state >&2
      die 'VPN shutdown failed before state-mount recovery'
    fi
    printf 'amnezia_recovery_mount_down_seconds=%s\n' "$((SECONDS - mount_down_started))"
    verify_disabled
    guest systemctl stop subyard-amnezia.service docker.service docker.socket containerd.service >/dev/null
    guest sh -ceu '
      findmnt -n /srv >/dev/null
      umount /srv
      test ! -e /srv/amnezia
    '
    for operation in provision start; do
      if guest python3 /usr/local/lib/subyard-amnezia/runtime.py "$operation" >/dev/null 2>&1; then
        die 'runtime accepted an unmounted state volume'
      fi
    done
    if yard vpn up --yes >/dev/null 2>&1; then
      die 'VPN up accepted an unmounted state volume'
    fi
    guest test ! -e /srv/amnezia || die 'runtime wrote VPN state into the root filesystem'
    guest systemctl start containerd.service docker.service subyard-amnezia.service
    guest findmnt -n /srv >/dev/null || die 'container service mount dependency did not restore /srv'
    yard vpn up --yes
    verify_enabled
    ;;
  restart-enabled)
    yard stop --yes
    yard start --yes
    verify_enabled
    verify_cpu_policy
    restart_vpn_guest
    # Direct Incus starts bypass product scheduling; public init restores it.
    yard init --yes
    verify_enabled
    verify_cpu_policy
    ;;
  restart-disabled)
    yard stop --yes
    yard start --yes
    yard init --yes
    yard provision amnezia --yes
    verify_disabled
    ;;
  verify-enabled) verify_enabled; verify_cpu_policy ;;
  verify-disabled) verify_disabled ;;
  isolation)
    yard network isolation on --yes
    verify_enabled
    ;;
  isolation-off)
    yard network isolation off --yes
    verify_enabled
    ;;
  deselect)
    if yard config set ENVIRONMENT_PROFILES '' --scope yard --yes; then
      die 'configuration allowed deselecting an enabled public resource'
    fi
    # Exercise recovery from an operator editing the owned settings directly.
    sed -i 's/^ENVIRONMENT_PROFILES=.*/ENVIRONMENT_PROFILES=/' \
      "$SUBYARD_CONFIG_HOME/yards/vpn-e2e/config.env"
    yard init --yes
    verify_deselected
    ;;
  reconcile-deselected)
    yard init --yes
    verify_deselected
    ;;
  reselect)
    yard config set ENVIRONMENT_PROFILES amnezia --scope yard --yes
    yard init --yes
    yard vpn up --yes
    yard vpn-admin up --yes
    verify_enabled
    ;;
  work-stop)
    "$YARD_BIN" -Y work-e2e stop --yes
    verify_enabled
    guest docker stats --no-stream --format 'vpn_cpu={{.CPUPerc}} vpn_memory={{.MemUsage}}' amnezia-awg2
    ;;
  work-load)
    prepare_work_yard
    incus exec yard-work-e2e --project subyard-work-e2e -- \
      test ! -e /usr/local/lib/subyard-amnezia/runtime.py
    # A bounded neighbor workload; no priority or throughput guarantee is asserted.
    incus exec yard-work-e2e --project subyard-work-e2e -- sh -c \
      'cat > /tmp/subyard-neighbor-load.py' <<'PY'
import hashlib
import multiprocessing
import time
def load():
    pages = bytearray(64 * 1024 * 1024)
    pages[::4096] = b'x' * (len(pages) // 4096)
    block = b'x' * (1024 * 1024)
    end = time.monotonic() + 120
    while time.monotonic() < end:
        hashlib.sha256(block).digest()
if __name__ == '__main__':
    jobs = [multiprocessing.Process(target=load) for _ in range(3)]
    for job in jobs:
        job.start()
    for job in jobs:
        job.join()
PY
    incus exec yard-work-e2e --project subyard-work-e2e -- sh -c \
      'nohup python3 /tmp/subyard-neighbor-load.py >/tmp/subyard-neighbor-load.log 2>&1 </dev/null & echo $! >/run/subyard-neighbor-load.pid'
    incus exec yard-work-e2e --project subyard-work-e2e -- python3 - <<'PY'
from pathlib import Path
import time
leader = int(Path('/run/subyard-neighbor-load.pid').read_text())
for _ in range(30):
    children = Path(f'/proc/{leader}/task/{leader}/children').read_text().split()
    resident = []
    for child in children:
        status = Path(f'/proc/{int(child)}/status').read_text().splitlines()
        resident.extend(int(line.split()[1]) for line in status if line.startswith('VmRSS:'))
    if len(resident) == 3 and min(resident) >= 60 * 1024:
        print(f'neighbor_load_verified_workers=3 resident_kib={sum(resident)}')
        break
    time.sleep(0.5)
else:
    raise SystemExit('neighbor workload did not become resident')
PY
    verify_enabled
    printf 'neighbor_load_cpu_workers=3 memory_mib=192 duration_seconds=120\n'
    ;;
  *) die 'unknown acceptance phase' ;;
esac
printf 'amnezia_profile_phase=%s result=pass\n' "$phase"
