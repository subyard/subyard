#!/usr/bin/env bash
# Exercise the live lane's host-free preflight with the hosted image setting.
set -euo pipefail
tests="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SUBYARD_E2E_LEASE_IMAGE=ubuntu-24.04 bash "$tests/e2e/acceptance.sh" --preflight
