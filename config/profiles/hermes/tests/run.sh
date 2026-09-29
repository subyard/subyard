#!/usr/bin/env bash
# Host-free checks owned by the hermes profile.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
bash "$ROOT/tests/helpers/profile-go.sh" hermes
bash "$ROOT/config/profiles/hermes/tests/hermes-dashboard-resource.sh"
bash "$ROOT/config/profiles/hermes/tests/hermes-e2e-contract.sh"
bash "$ROOT/config/profiles/hermes/tests/hermes-provision.sh"
bash "$ROOT/config/profiles/hermes/tests/e2e-controller.sh"
