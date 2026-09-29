#!/usr/bin/env bash
# Host-free checks owned by the subyard-dev profile.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
bash "$ROOT/config/profiles/subyard-dev/tests/subyard-dev-provision.sh"
bash "$ROOT/config/profiles/subyard-dev/tests/e2e/acceptance.sh" --help >/dev/null
