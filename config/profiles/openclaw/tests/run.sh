#!/usr/bin/env bash
# Host-free checks owned by the openclaw profile.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
bash "$ROOT/tests/helpers/profile-go.sh" openclaw
bash "$ROOT/config/profiles/openclaw/tests/openclaw-provision-check.sh"
bash "$ROOT/config/profiles/openclaw/tests/openclaw-resource-protocol.sh"
