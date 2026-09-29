#!/usr/bin/env bash
# Host-free checks owned by the android profile.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
bash "$ROOT/tests/helpers/profile-go.sh" android
bash "$ROOT/config/profiles/android/tests/android-provision-check.sh"
bash "$ROOT/config/profiles/android/tests/emulator-process-identity.sh"
bash "$ROOT/config/profiles/android/tests/emulator-process-control.sh"
bash "$ROOT/config/profiles/android/tests/emulator-resource-protocol.sh"
bash "$ROOT/config/profiles/android/tests/e2e-controller.sh"
