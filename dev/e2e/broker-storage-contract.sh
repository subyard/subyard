#!/usr/bin/env bash
# Compile as the normal runner user; elevate only the guest-local Incus test.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
[ "${SUBYARD_E2E_TYPE:-}" = subyard-pair ] && [ -r /run/subyard-e2e-lease.json ] \
  || { printf 'broker-storage-contract: requires an allocated pair guest\n' >&2; exit 2; }
binary="$(mktemp /tmp/subyard-broker-storage.XXXXXX)"
trap 'rm -f -- "$binary"' EXIT
cd "$ROOT"
go test -c -p 1 -tags realincus -o "$binary" ./internal/adapters/testvmsruntime
sudo -n env SUBYARD_REAL_BROKER_STORAGE=1 SUBYARD_E2E_TYPE="$SUBYARD_E2E_TYPE" \
  "$binary" -test.run '^TestRealBrokerStorageContract$' -test.v -test.timeout=12m
