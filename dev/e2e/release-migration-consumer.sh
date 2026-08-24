#!/usr/bin/env bash
# Standard broker client proof inside the pre-existing catch-up consumer.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ROUTE_REGISTRY="${SUBYARD_E2E_ROUTE_REGISTRY:-/var/lib/subyard/e2e-routes}"

die() { printf 'release-migration-consumer: %s\n' "$*" >&2; exit 2; }

[ "${SUBYARD_E2E_CONSUMER_FIXTURE:-0}" = 1 ] \
  || die 'fixture confirmation is required'
[ -r "$ROUTE_REGISTRY/test-yard/current/route.tsv" ] \
  && [ -r "$ROUTE_REGISTRY/test-yard/current/known_hosts" ] \
  || die 'canonical route is unavailable'
command -v jq >/dev/null 2>&1 || die 'jq is required'

# This consumer enters a distinct broker scope. Do not let the outer lease
# identity influence selection or leak into nested runner setup.
unset SUBYARD_E2E_SLOT

cd "$ROOT"
if ! git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  git init -q
  git config user.name fixture
  git config user.email fixture@example.invalid
  git add -A
  git commit -qm fixture
fi

dev/agent-e2e.sh --prepare
status="$(dev/agent-e2e.sh --status --json)" \
  || die 'cannot read broker-local status'
slot_id="$(jq -er '
  if .schema_version == 1 and .status == "ok" and
    .pool.schema_version == 1 and (.pool.slots | type) == "array"
  then first(.pool.slots[] | select(.state == "available") | .slot_id) // empty
  else empty
  end
' <<<"$status")" \
  || die 'no available broker-local slot'
[[ "$slot_id" =~ ^slot-[0-9]{3}$ ]] \
  || die 'invalid broker-local slot id'
slot_number="${slot_id#slot-}"
slot_number="$((10#$slot_number))"
[ "$slot_number" -ge 1 ] && [ "$slot_number" -le 999 ] \
  || die 'invalid broker-local slot id'

dev/agent-e2e.sh --slot "$slot_number" --wait 20m --vm both -- sh -c \
  'printf "hostname=%s uid=%s sudo=%s\n" "$(hostname)" "$(id -u)" "$(sudo -n id -u)"'
dev/agent-e2e.sh --slot "$slot_number" --wait 20m --verify-boundary
