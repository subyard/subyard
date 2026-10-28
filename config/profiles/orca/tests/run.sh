#!/usr/bin/env bash
# Host-free checks owned by the orca profile.
set -euo pipefail
umask 0022
export TMPDIR=/tmp
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
shellcheck -s bash -S warning "$ROOT/config/profiles/orca/resources/orca/registration-observer.bash"
bash "$ROOT/tests/helpers/profile-go.sh" orca
bash "$ROOT/config/profiles/orca/tests/orca-profile-resource.sh"
bash "$ROOT/config/profiles/orca/tests/ssh-agent-environment.sh"
bash "$ROOT/config/profiles/orca/tests/e2e-controller.sh"
bash "$ROOT/config/profiles/orca/tests/release-preparation.sh"
bash "$ROOT/config/profiles/orca/tests/resource-lifecycle.sh"

python3 -B "$ROOT/config/profiles/orca/tests/native-loss-test.py"
python3 -B "$ROOT/config/profiles/orca/tests/release-observation-test.py"
