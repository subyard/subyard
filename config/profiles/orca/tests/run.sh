#!/usr/bin/env bash
# Host-free checks owned by the orca profile.
set -euo pipefail
export TMPDIR=/tmp
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
bash "$ROOT/tests/helpers/profile-go.sh" orca
bash "$ROOT/config/profiles/orca/tests/orca-profile-resource.sh"
bash "$ROOT/config/profiles/orca/tests/ssh-agent-environment.sh"
bash "$ROOT/config/profiles/orca/tests/e2e-controller.sh"
bash "$ROOT/config/profiles/orca/tests/release-preparation.sh"
bash "$ROOT/config/profiles/orca/tests/resource-lifecycle.sh"
