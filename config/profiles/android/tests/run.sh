#!/usr/bin/env bash
# Host-free checks owned by the android profile.
set -euo pipefail
umask 0022
export TMPDIR=/tmp
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
python3 -B "$ROOT/config/profiles/android/tests/skills-test.py"
python3 -B "$ROOT/config/profiles/android/tests/toolchain-test.py"
bash "$ROOT/tests/helpers/profile-go.sh" android
bash "$ROOT/config/profiles/android/tests/android-provision-check.sh"
polluted_tmp="$(mktemp -d /tmp/subyard-android-ambient.XXXXXX)"
trap 'rmdir "$polluted_tmp"' EXIT
env ANDROID_SDK_ROOT=/foreign-sdk ANDROID_HOME=/foreign-sdk JAVA_HOME=/foreign-jdk \
  JDK_VERSION=21 ANDROID_API=99 BUILD_TOOLS_VERSION=99.0.0 TMPDIR="$polluted_tmp" \
  bash "$ROOT/config/profiles/android/tests/android-provision-check.sh"
rmdir "$polluted_tmp"
trap - EXIT
bash "$ROOT/config/profiles/android/tests/emulator-process-identity.sh"
bash "$ROOT/config/profiles/android/tests/emulator-process-control.sh"
bash "$ROOT/config/profiles/android/tests/emulator-resource-protocol.sh"
bash "$ROOT/config/profiles/android/tests/e2e-controller.sh"
bash "$ROOT/config/profiles/android/tests/e2e-candidate.sh"
bash "$ROOT/config/profiles/android/tests/resource-lifecycle.sh"
