#!/usr/bin/env bash
# Run one core VM release check declared in dev/e2e/release-vm-checks.json on an explicit slot.
# dev/release-acceptance.py runs these as vm:<id> checks and records their results in the receipt.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CATALOG="$ROOT/dev/e2e/release-vm-checks.json"

die() { printf 'release-vm-check: %s\n' "$*" >&2; exit 2; }

[ "$#" = 3 ] && [ "$2" = --slot ] && [[ "$3" =~ ^[1-9][0-9]{0,2}$ ]] \
  || die 'usage: release-vm-check.sh ID --slot N'
command -v jq >/dev/null 2>&1 || die 'jq is required'
id="$1" slot="$3"
entry="$(jq -ce --arg id "$id" '.checks[] | select(.id == $id)' "$CATALOG")" \
  || die "unknown check '$id'"
timeout_seconds="$(jq -r '.timeout_seconds' <<<"$entry")"
mapfile -t argv < <(jq -r '.command[]' <<<"$entry")
# A numeric fixture token unique to this attempt: UTC seconds and slot.
token="$(date -u +%s)$slot"
argv=("${argv[@]//@TOKEN@/$token}")

cd "$ROOT"
if jq -e 'has("lease")' <<<"$entry" >/dev/null; then
  type="$(jq -r '.lease.type // empty' <<<"$entry")"
  lease=(--vm-count 1 --vm 1)
  [ -z "$type" ] || lease=(--type "$type" --vm 1)
  exec dev/agent-e2e.sh --slot "$slot" "${lease[@]}" --purpose "release-${id//\//-}" -- \
    timeout --signal=TERM --kill-after=60 "$timeout_seconds" "${argv[@]}"
fi
exec timeout --signal=TERM --kill-after=120 "$timeout_seconds" "${argv[@]}" --slot "$slot"
