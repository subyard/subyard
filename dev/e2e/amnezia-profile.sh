#!/usr/bin/env bash
# Run only on an allocated owner; retain the fixture for same-lease data-path checks.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
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
"$root/dev/build-engine.sh"
yard() { "$root/.build/yard" -Y vpn-e2e "$@"; }
guest() { incus exec yard-vpn-e2e --project subyard-vpn-e2e -- "$@"; }
signature() {
  guest sh -c 'cd /srv/amnezia && sha256sum awg0.conf client.conf settings.json' | sha256sum
}
verify_state() {
  YARD_KIND=vm incus_wait_instance_agent subyard-vpn-e2e yard-vpn-e2e || die 'VM agent unavailable'
  [ "$(signature)" = "$(cat "$fixture/state.sha256")" ] || die 'VPN state changed across lifecycle'
}
verify_enabled() {
  verify_state
  for _ in $(seq 1 45); do
    if yard vpn status | jq -e '.ready and .running and .enabled and .ingress' >/dev/null; then
      yard security --require-live --quiet
      return
    fi
    sleep 2
  done
  die 'enabled VPN did not recover'
}
verify_disabled() {
  verify_state
  yard vpn status | jq -e '(.ready or .running or .enabled or .ingress) | not' >/dev/null \
    || die 'disabled VPN recovered unexpectedly'
}
verify_deselected() {
  verify_state
  incus query '/1.0/instances/yard-vpn-e2e?project=subyard-vpn-e2e' \
    | jq -e '(.devices["amnezia-vpn"] == null) and (.config["user.subyard.resource.amnezia-vpn"] == null)' >/dev/null
  guest python3 /usr/local/lib/subyard-amnezia/runtime.py observe \
    | jq -e '(.ready or .running or .enabled) | not' >/dev/null \
    || die 'deselected VPN runtime remains enabled'
}
case "$phase" in
  bootstrap)
    [ ! -f "$SUBYARD_CONFIG_HOME/yards/vpn-e2e/config.env" ] || die 'expected a fresh VPN yard'
    yard init --profile amnezia --yes
    ;;
  init)
    if [ ! -f "$SUBYARD_CONFIG_HOME/yards/vpn-e2e/config.env" ]; then
      yard init --profile amnezia --yes
    else
      yard init --yes
    fi
    yard start --yes
    yard init --yes
    yard provision amnezia --yes
    yard vpn status
    guest python3 -c '
import json, subprocess
s = json.loads(subprocess.check_output(["python3", "/usr/local/lib/subyard-amnezia/runtime.py", "observe"]))
assert not s["running"] and not s["enabled"] and not s["state_present"], s
'
    guest findmnt -n -o FSTYPE,SIZE /srv
    yard security --require-live --quiet
    ;;
  up)
    route="$(ip -4 -j route get 1.1.1.1)"
    address="$(jq -r '.[0].prefsrc' <<<"$route")"
    interface="$(jq -r '.[0].dev' <<<"$route")"
    yard config set RESOURCE_VPN_IPV4 "$address" --scope yard --yes
    yard config set RESOURCE_VPN_INTERFACE "$interface" --scope yard --yes
    yard config set RESOURCE_VPN_PORT 51820 --scope yard --yes
    yard vpn up --yes
    before="$(signature)"
    yard vpn up --yes
    [ "$(signature)" = "$before" ] || die 'repeat up changed VPN state'
    printf '%s\n' "$before" > "$fixture/state.sha256"
    chmod 0600 "$fixture/state.sha256"
    yard vpn status
    yard security --require-live --quiet
    ;;
  repeat)
    before="$(signature)"
    yard init --yes
    yard provision amnezia --yes
    [ "$(signature)" = "$before" ] || die 'repeat reconciliation changed VPN state'
    yard vpn status
    ;;
  down)
    before="$(signature)"
    yard vpn down --yes
    yard vpn down --yes
    [ "$(signature)" = "$before" ] || die 'shutdown changed VPN state'
    yard vpn status
    guest systemctl is-enabled subyard-amnezia.service && die 'service remains enabled'
    ;;
  restart-enabled)
    yard stop --yes
    yard start --yes
    verify_enabled
    incus restart yard-vpn-e2e --project subyard-vpn-e2e --timeout 120
    verify_enabled
    ;;
  restart-disabled)
    yard stop --yes
    yard start --yes
    yard init --yes
    yard provision amnezia --yes
    verify_disabled
    ;;
  verify-enabled) verify_enabled ;;
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
    verify_enabled
    ;;
  work-stop)
    "$root/.build/yard" -Y work-e2e stop --yes
    verify_enabled
    guest docker stats --no-stream --format 'vpn_cpu={{.CPUPerc}} vpn_memory={{.MemUsage}}' subyard-amnezia
    ;;
  work-load)
    definition="$SUBYARD_CONFIG_HOME/yards/work-e2e/config.env"
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
SSH_PORT=2226
CONFIG
      chmod 0600 "$definition"
    fi
    "$root/.build/yard" -Y work-e2e init --yes
    "$root/.build/yard" -Y work-e2e start --yes
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
