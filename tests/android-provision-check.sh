#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
HOOK="$ROOT/config/profiles/android/provision.sh"
tmp="$(mktemp -d)"
trap 'rm -rf -- "$tmp"' EXIT
test_root="$tmp/root"
fake_bin="$tmp/bin"
mkdir -p "$fake_bin" "$test_root/opt/jdk-17/bin" \
  "$test_root/srv/cache/android-sdk/cmdline-tools/latest/bin" \
  "$test_root/srv/cache/android-sdk/platform-tools" \
  "$test_root/srv/cache/android-sdk/platforms/android-36" \
  "$test_root/srv/cache/android-sdk/build-tools/36.0.0" \
  "$test_root/srv/cache/android-sdk/emulator" \
  "$test_root/srv/cache/android-sdk/system-images/android-36/google_apis/x86_64" \
  "$test_root/srv/cache/android-sdk/licenses" "$test_root/srv/cache/gradle" \
  "$test_root/etc/profile.d"

for command in java sdkmanager adb emulator; do
  case "$command" in
    java) path="$test_root/opt/jdk-17/bin/java" ;;
    sdkmanager) path="$test_root/srv/cache/android-sdk/cmdline-tools/latest/bin/sdkmanager" ;;
    adb) path="$test_root/srv/cache/android-sdk/platform-tools/adb" ;;
    emulator) path="$test_root/srv/cache/android-sdk/emulator/emulator" ;;
  esac
  printf '#!/usr/bin/env bash\nexit 0\n' > "$path"
  chmod +x "$path"
done
printf 'accepted\n' > "$test_root/srv/cache/android-sdk/licenses/android-sdk-license"
cat > "$test_root/etc/profile.d/subyard-android.sh" <<'ENV'
export JAVA_HOME="/opt/jdk-17"
export ANDROID_HOME="/srv/cache/android-sdk"
export ANDROID_SDK_ROOT="/srv/cache/android-sdk"
export GRADLE_USER_HOME="/srv/cache/gradle"
ENV
for command in curl unzip flock setsid cage Xwayland; do
  printf '#!/usr/bin/env bash\nexit 0\n' > "$fake_bin/$command"
  chmod +x "$fake_bin/$command"
done
printf '#!/usr/bin/env bash\nexit 99\n' > "$fake_bin/apt-get"
chmod +x "$fake_bin/apt-get"

common_env=(
  PATH="$fake_bin:$PATH"
  ANDROID_TEST_ROOT="$test_root"
  DEV_USER="$(id -un)"
  ANDROID_API=36
  JDK_VERSION=17
  BUILD_TOOLS_VERSION=36.0.0
  SYSTEM_IMAGE='system-images;android-36;google_apis;x86_64'
  ANDROID_SDK_ROOT=/srv/cache/android-sdk
  GRADLE_USER_HOME=/srv/cache/gradle
)

before="$(find "$tmp" -type f -exec sha256sum {} + | sort | sha256sum)"
env "${common_env[@]}" bash "$HOOK" --check >/dev/null
after="$(find "$tmp" -type f -exec sha256sum {} + | sort | sha256sum)"
[ "$before" = "$after" ] || { printf 'FAIL: Android check mutated state\n' >&2; exit 1; }

rm -f "$test_root/srv/cache/android-sdk/platform-tools/adb"
set +e
env "${common_env[@]}" bash "$HOOK" --check >/dev/null
status=$?
set -e
[ "$status" -eq 10 ] || { printf 'FAIL: Android drift status=%s, want 10\n' "$status" >&2; exit 1; }

printf 'ok: Android provision check\n'
