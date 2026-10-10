#!/usr/bin/env bash
# Host-free checks owned by the openclaw profile.
set -euo pipefail
export TMPDIR=/tmp
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
bash "$ROOT/tests/helpers/profile-go.sh" openclaw
bash "$ROOT/config/profiles/openclaw/tests/openclaw-provision-check.sh"
bash "$ROOT/config/profiles/openclaw/tests/openclaw-resource-protocol.sh"
bash "$ROOT/config/profiles/openclaw/tests/qa-lifecycle-exact.sh"
bash "$ROOT/config/profiles/openclaw/tests/e2e-controller.sh"
bash "$ROOT/config/profiles/openclaw/tests/credentials.sh"
bash "$ROOT/config/profiles/openclaw/tests/consumer-stop.sh"
bash "$ROOT/config/profiles/openclaw/tests/resource-lifecycle.sh"
bash "$ROOT/config/profiles/openclaw/tests/staging-exact.sh"
