#!/usr/bin/env bash
# Periodic E2E on a hosted runner with the candidate pool: lease one system container, reach
# it over the lease's SSH route, release it, then confirm through the pool's own Incus that
# the slot project is empty.
set -euo pipefail
[ "$#" = 2 ] && [ "$1" = --slot ] && [[ "$2" =~ ^[1-9]$ ]] || { echo 'usage: container-pool.sh --slot N' >&2; exit 2; }
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
SUBYARD_E2E_TIER=system-container bash dev/agent-e2e.sh --slot "$2" --vm-count 1 --vm 1 --purpose container-pool -- \
  bash -ec 'systemd-detect-virt --container --quiet; sudo -n test -s /run/subyard-e2e-lease.json; incus --version'
sudo -n /usr/bin/incus exec yard-test-yard --project subyard-test-yard -- \
  incus list --project "subyard-e2e-vms-slot-$2" --format json </dev/null | jq -e 'length == 0' >/dev/null
printf 'ok: system-container lease acquired, reached and released; slot project is empty\n'
