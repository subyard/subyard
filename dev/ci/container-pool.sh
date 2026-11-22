#!/usr/bin/env bash
# Periodic E2E on a hosted runner with the candidate pool: lease a system container, then a VM,
# then a container again on the same slot. Each lease is reached over its SSH route and released,
# and the pool's own Incus must report the slot project empty after each release.
set -euo pipefail
[ "$#" = 2 ] && [ "$1" = --slot ] && [[ "$2" =~ ^[1-9]$ ]] || { echo 'usage: container-pool.sh --slot N' >&2; exit 2; }
slot="$2"
cd "$(dirname "${BASH_SOURCE[0]}")/../.."

lease() { # tier, guest probe
  SUBYARD_E2E_TIER="$1" bash dev/agent-e2e.sh --slot "$slot" --vm-count 1 --vm 1 --purpose "container-pool-$1" -- \
    bash -ec "sudo -n test -s /run/subyard-e2e-lease.json; $2"
  sudo -n /usr/bin/incus exec yard-test-yard --project subyard-test-yard -- \
    incus list --project "subyard-e2e-vms-slot-$slot" --format json </dev/null | jq -e 'length == 0' >/dev/null
}

lease system-container 'systemd-detect-virt --container --quiet; incus --version'
lease vm 'systemd-detect-virt --vm --quiet'
lease system-container 'systemd-detect-virt --container --quiet'
printf 'ok: container, VM and container leases alternated on slot %s; each release left the slot project empty\n' "$slot"
