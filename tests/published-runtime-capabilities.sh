#!/usr/bin/env bash
# Frozen v0.28.0 output, reproduced by building cmd/yard from the release tag.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=dev/e2e/lib-power-upgrade-contracts.sh
. "$ROOT/dev/e2e/lib-power-upgrade-contracts.sh"
die() { printf '%s\n' "$*" >&2; exit 2; }
response="${1:-$ROOT/tests/fixtures/published-v0.28.0-capabilities.json}"
verify_compatible_capabilities "$response"
tmp="$(mktemp -d /tmp/subyard-published-capabilities.XXXXXX)"
trap 'rm -rf -- "$tmp"' EXIT
for mutation in '.activationOwners -= ["openlit-runtime"]' '.activationReconciliationOwned = true' '.activationOwners += ["foreign-runtime"]'; do
  jq "$mutation" "$response" > "$tmp/invalid.json"
  if (verify_compatible_capabilities "$tmp/invalid.json") > "$tmp/output" 2>&1; then
    die 'published compatible runtime accepted changed capabilities'
  fi
  grep -Fxq 'published compatible runtime lacks exact activation capabilities' "$tmp/output"
done
printf 'ok: published v0.28.0 capabilities retain every frozen activation owner\n'
