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
  "$test_root/srv/cache/android-sdk/licenses" "$test_root/srv/cache/gradle" \
  "$test_root/srv/cache/android-sdk/.subyard/bin" "$test_root/etc/profile.d" \
  "$test_root/etc/systemd/system" "$test_root/usr/local/lib/subyard-android"

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
export PATH="/srv/cache/android-sdk/.subyard/bin:/opt/jdk-17/bin:/srv/cache/android-sdk/platform-tools:/srv/cache/android-sdk/cmdline-tools/latest/bin:$PATH"
ENV
printf '{"sdk_root":"/srv/cache/android-sdk","state_root":"/var/lib/subyard-android","public_root":"/srv/cache/android-sdk/.subyard","size":2,"api":36,"device":"phone","variant":"google_apis","abi":"x86_64","gpu":"host","dev_user":"%s","runtime_user":"subyard-android","java_home":"/opt/jdk-17"}\n' "$(id -un)" \
  > "$test_root/etc/subyard-android.json"
cat > "$test_root/etc/systemd/system/subyard-android-pool.service" <<'UNIT'
ExecStart=/usr/bin/python3 /usr/local/lib/subyard-android/pool.py serve
KillMode=control-group
TimeoutStopSec=120
UNIT
for source in pool.py client.py runtime.sh; do
  cp "$ROOT/config/profiles/android/$source" "$test_root/usr/local/lib/subyard-android/$source"
done
for command in yard yard-emu; do
  printf '#!/usr/bin/env bash\nexit 0\n' > "$test_root/srv/cache/android-sdk/.subyard/bin/$command"
  chmod +x "$test_root/srv/cache/android-sdk/.subyard/bin/$command"
done
/usr/bin/python3 -c 'import socket, sys; s=socket.socket(socket.AF_UNIX); s.bind(sys.argv[1])' \
  "$test_root/srv/cache/android-sdk/.subyard/control.sock"
for command in curl unzip flock setsid cage Xwayland socat systemctl slirp4netns ip; do
  printf '#!/usr/bin/env bash\nexit 0\n' > "$fake_bin/$command"
  chmod +x "$fake_bin/$command"
done
printf '#!/usr/bin/env bash\nprintf "%%s\\n" "%s:%s"\n' "$(id -u)" "$(id -g)" > "$fake_bin/docker"
chmod +x "$fake_bin/docker"
printf '#!/usr/bin/env bash\nexit 99\n' > "$fake_bin/apt-get"
chmod +x "$fake_bin/apt-get"
cat > "$fake_bin/dpkg-query" <<'EOF'
#!/usr/bin/env bash
[ "${@: -1}" != "${FAKE_ANDROID_MISSING_LIBRARY:-}" ] || exit 1
printf 'install ok installed\n'
EOF
chmod +x "$fake_bin/dpkg-query"

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

# Runtime dependencies must converge even when SDK binaries already exist.
for library in libpulse0 libnss3; do
  status=0
  env "${common_env[@]}" FAKE_ANDROID_MISSING_LIBRARY="$library" bash "$HOOK" --check >/dev/null || status=$?
  [ "$status" -eq 10 ] || { printf 'FAIL: missing %s status=%s, want 10\n' "$library" "$status" >&2; exit 1; }
done

# SDK command discovery remains part of ordinary builds, independent of emulator leases.
profile_env="$test_root/etc/profile.d/subyard-android.sh"
cp "$profile_env" "$tmp/profile-env"
sed -i 's@:/srv/cache/android-sdk/cmdline-tools/latest/bin@@' "$profile_env"
status=0
env "${common_env[@]}" bash "$HOOK" --check >/dev/null || status=$?
[ "$status" -eq 10 ] || { printf 'FAIL: missing SDK tools PATH status=%s, want 10\n' "$status" >&2; exit 1; }
cp "$tmp/profile-env" "$profile_env"

# Command-line Tools 23 can install components without the legacy licenses directory.
rm -f "$test_root/srv/cache/android-sdk/licenses/android-sdk-license"
env "${common_env[@]}" bash "$HOOK" --check >/dev/null

rm -f "$test_root/srv/cache/android-sdk/platform-tools/adb"
set +e
env "${common_env[@]}" bash "$HOOK" --check >/dev/null
status=$?
set -e
[ "$status" -eq 10 ] || { printf 'FAIL: Android drift status=%s, want 10\n' "$status" >&2; exit 1; }

printf 'ok: Android provision check\n'
