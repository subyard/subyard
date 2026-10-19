#!/usr/bin/env bash
# Generic retained-lease controller; component assertions belong to the owning payload.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
if [ "${1:-}" = --help ]; then
  printf 'Usage: dev/e2e/disabled-components-release.sh --slot N\nRequires a frozen packaged candidate transport bundle.\n'
  exit 0
fi
[ "$#" = 2 ] && [ "$1" = --slot ] || { printf 'expected --slot N\n' >&2; exit 2; }
# shellcheck source=dev/agent-e2e.sh
. "$ROOT/dev/agent-e2e.sh"
set_requested_slot "$2" --slot
# shellcheck disable=SC2034 # consumed by sourced lease client
VM_COUNT_REQUESTED=1 LEASE_PURPOSE=disabled-components-release
[ -n "${SUBYARD_E2E_CANDIDATE_BUNDLE:-}" ] && [ -n "${SUBYARD_E2E_CANDIDATE_SHA256:-}" ] \
  || die 'frozen candidate bundle and checksum are required'
LOCAL_TEMP="$(mktemp -d /tmp/subyard-agent-e2e.XXXXXXXX)"
install -d -m 0755 "$ROOT/.build/disabled-components-release"
evidence="$(mktemp -d "$ROOT/.build/disabled-components-release/run.XXXXXXXX")"
payload=config/agents/paseo/tests/e2e/disabled-components-release.sh
prepared=0
# Match the existing P0 controller clock without sourcing its execution dispatch.
reboot_monotonic_seconds() {
  local uptime
  IFS=' ' read -r uptime _ < /proc/uptime || die 'cannot read the kernel monotonic clock'
  uptime="${uptime%%.*}"
  [[ "$uptime" =~ ^[0-9]+$ ]] || die 'kernel monotonic clock returned an invalid value'
  printf '%s\n' "$uptime"
}
controller_cleanup() {
  local rc=$? failed=0
  trap - EXIT INT TERM
  set +e
  if [ "$prepared" = 1 ]; then
    if [ -n "${GUEST_DIRS[1]:-}" ]; then
      guest 1 sudo -n cat "${GUEST_DIRS[1]}/src/.build/disabled-components-release/failure.tar.gz" \
        > "$evidence/failure.tar.gz" 2>/dev/null || true
    fi
    cleanup_guest 1 quiet >/dev/null 2>&1 || failed=1
    run_guest 1 "$bundle" "$bundle_hash" timeout --signal=TERM --kill-after=30 180 \
      bash "$payload" clean "$token" || failed=1
  fi
  [ "$failed" = 0 ] || [ "$rc" != 0 ] || rc=3
  cleanup_on_exit '' "$rc"
}
trap controller_cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
bundle="$LOCAL_TEMP/worktree.tar.gz"
build_bundle "$ROOT" "$bundle" .subyard-acceptance/candidate.json .subyard-acceptance/release
bundle_hash="$(sha256sum "$bundle" | awk '{print $1}')"
printf 'evidence: public_bundle_sha256=%s\n' "$bundle_hash"
acquire_lease
start_lease_keeper
token="$LEASE_GENERATION"
[[ "$token" =~ ^[1-9][0-9]*$ ]] || die 'invalid lease generation'
prepared=1
run_guest 1 "$bundle" "$bundle_hash" timeout --signal=TERM --kill-after=30 3600 \
  bash "$payload" prepare "$token"
cleanup_guest 1
before_boot="$(guest 1 cat /proc/sys/kernel/random/boot_id </dev/null)"
timeout --foreground 20 ssh -F "$CLIENT_CONFIG" -T -o ConnectTimeout=3 \
  -o ConnectionAttempts=1 -o ServerAliveInterval=2 -o ServerAliveCountMax=2 \
  e2e-vm-1 -- sudo -n systemctl reboot </dev/null >/dev/null 2>&1 || true
down=0
deadline=$(($(reboot_monotonic_seconds) + 60))
while true; do
  remaining=$((deadline - $(reboot_monotonic_seconds)))
  [ "$remaining" -gt 0 ] || break
  probe_budget="$remaining"
  [ "$probe_budget" -le 5 ] || probe_budget=5
  if ! timeout --foreground --signal=KILL "$probe_budget" ssh -F "$CLIENT_CONFIG" -T -o ConnectTimeout=2 \
    -o ConnectionAttempts=1 e2e-vm-1 -- true </dev/null >/dev/null 2>&1; then down=1; break; fi
  [ "$(reboot_monotonic_seconds)" -lt "$deadline" ] || break
  sleep 1
done
[ "$down" = 1 ] || die 'VM did not go down for reboot'
up=0
deadline=$(($(reboot_monotonic_seconds) + 180))
while true; do
  remaining=$((deadline - $(reboot_monotonic_seconds)))
  [ "$remaining" -gt 0 ] || break
  probe_budget="$remaining"
  [ "$probe_budget" -le 6 ] || probe_budget=6
  after_boot="$(timeout --foreground --signal=KILL "$probe_budget" ssh -F "$CLIENT_CONFIG" -T -o ConnectTimeout=3 \
    -o ConnectionAttempts=1 e2e-vm-1 -- cat /proc/sys/kernel/random/boot_id 2>/dev/null)" || after_boot=''
  if [ -n "$after_boot" ] && [ "$after_boot" != "$before_boot" ]; then up=1; break; fi
  [ "$(reboot_monotonic_seconds)" -lt "$deadline" ] || break
  sleep 1
done
[ "$up" = 1 ] || die 'VM did not return with a new boot ID'
printf 'evidence: preboot=%s postboot=%s\n' "$before_boot" "$after_boot"
run_guest 1 "$bundle" "$bundle_hash" timeout --signal=TERM --kill-after=30 600 \
  bash "$payload" resume "$token"
guest 1 /usr/sbin/runuser -u dev -- env HOME=/home/dev USER=dev LOGNAME=dev SUBYARD_E2E_VM=1 \
  bash "${GUEST_DIRS[1]}/src/$payload" evidence "$token" > "$evidence/fixture.tar.gz"
printf 'evidence: fixture=%s sha256=%s\n' "$evidence/fixture.tar.gz" "$(sha256sum "$evidence/fixture.tar.gz" | awk '{print $1}')"
guest 1 timeout --signal=TERM --kill-after=30 180 /usr/sbin/runuser -u dev -- env HOME=/home/dev USER=dev LOGNAME=dev SUBYARD_E2E_VM=1 \
  bash "${GUEST_DIRS[1]}/src/$payload" clean "$token"
cleanup_guest 1
prepared=0
ok 'installed candidate retained disabled components through upgrade, recovery and reboot'
