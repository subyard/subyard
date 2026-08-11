#!/usr/bin/env bash
# subyard-provision-check-v1
# config/profiles/android/provision.sh — install the Android toolchain into the yard (run as root
# inside the yard by the Go provision workflow; idempotent). Vars: ANDROID_API, JDK_VERSION,
# BUILD_TOOLS_VERSION, SYSTEM_IMAGE, ANDROID_SDK_ROOT, GRADLE_USER_HOME, CMDLINE_TOOLS_URL, DEV_USER.
set -euo pipefail

check_only=0
case "${1:-}" in
  --check) check_only=1; shift ;;
  "") ;;
  *) printf 'android provision: unknown argument %s\n' "$1" >&2; exit 2 ;;
esac
[ "$#" -eq 0 ] || { printf 'android provision: unexpected argument\n' >&2; exit 2; }

ANDROID_API="${ANDROID_API:-36}"
JDK_VERSION="${JDK_VERSION:-17}"
BUILD_TOOLS_VERSION="${BUILD_TOOLS_VERSION:-36.0.0}"
SYSTEM_IMAGE="${SYSTEM_IMAGE:-system-images;android-${ANDROID_API};google_apis;x86_64}"
ANDROID_SDK_ROOT="${ANDROID_SDK_ROOT:-/srv/cache/android-sdk}"
GRADLE_USER_HOME="${GRADLE_USER_HOME:-/srv/cache/gradle}"
DEV_USER="${DEV_USER:-dev}"
JDK_HOME="/opt/jdk-${JDK_VERSION}"

if [ "$check_only" -eq 1 ]; then
  root_prefix="${ANDROID_TEST_ROOT:-}"
  rooted() { printf '%s%s' "$root_prefix" "$1"; }
  changed=0
  for command in curl unzip flock setsid cage Xwayland; do
    command -v "$command" >/dev/null 2>&1 || changed=1
  done
  jdk_home="$(rooted "$JDK_HOME")"
  sdk_root="$(rooted "$ANDROID_SDK_ROOT")"
  gradle_root="$(rooted "$GRADLE_USER_HOME")"
  [ -x "$jdk_home/bin/java" ] || changed=1
  [ -x "$sdk_root/cmdline-tools/latest/bin/sdkmanager" ] || changed=1
  [ -x "$sdk_root/platform-tools/adb" ] || changed=1
  [ -d "$sdk_root/platforms/android-${ANDROID_API}" ] || changed=1
  [ -d "$sdk_root/build-tools/${BUILD_TOOLS_VERSION}" ] || changed=1
  [ -x "$sdk_root/emulator/emulator" ] || changed=1
  image_path="${SYSTEM_IMAGE//;/\/}"
  [ -d "$sdk_root/$image_path" ] || changed=1
  [ -n "$(find "$sdk_root/licenses" -type f -print -quit 2>/dev/null)" ] || changed=1
  expected_owner="$(id -u "$DEV_USER"):$(id -g "$DEV_USER")"
  for directory in "$jdk_home" "$sdk_root" "$gradle_root"; do
    [ -d "$directory" ] && [ ! -L "$directory" ] \
      && [ "$(stat -c '%u:%g' "$directory" 2>/dev/null)" = "$expected_owner" ] || changed=1
  done
  profile_env="$(rooted /etc/profile.d/subyard-android.sh)"
  [ -f "$profile_env" ] && [ ! -L "$profile_env" ] \
    && grep -Fxq "export JAVA_HOME=\"$JDK_HOME\"" "$profile_env" \
    && grep -Fxq "export ANDROID_HOME=\"$ANDROID_SDK_ROOT\"" "$profile_env" \
    && grep -Fxq "export ANDROID_SDK_ROOT=\"$ANDROID_SDK_ROOT\"" "$profile_env" \
    && grep -Fxq "export GRADLE_USER_HOME=\"$GRADLE_USER_HOME\"" "$profile_env" \
    || changed=1
  shopt -s nullglob
  for lp in "$(rooted /srv/workspaces)"/*/src/local.properties; do
    d="$(sed -n 's/^[[:space:]]*sdk\.dir[[:space:]]*=[[:space:]]*//p' "$lp" | tail -n1)"
    d="${d%$'\r'}"
    case "$d" in ""|"$ANDROID_SDK_ROOT") continue ;; esac
    destination="$(rooted "$d")"
    [ -L "$destination" ] \
      && [ "$(readlink -f "$destination" 2>/dev/null)" = "$(readlink -f "$sdk_root")" ] \
      || changed=1
  done
  shopt -u nullglob
  [ "$changed" -eq 0 ] && exit 0
  exit 10
fi

export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq curl unzip util-linux
# Headless HW GLES for -gpu host: Mesa + a tiny wlroots compositor (cage) + Xwayland. The emulator's
# GLES uses GLX (needs an X display); the passed-through render node only does HW GL via EGL → the
# launcher bridges with a headless wlroots compositor on the render node → Xwayland → HW GLX.
apt-get install -y -qq \
  libgl1-mesa-dri libegl-mesa0 libgbm1 libglx-mesa0 cage xwayland 2>/dev/null || true

# 1. JDK — Debian 13 has no openjdk-17; Temurin from Adoptium (redirects to the current GA).
if [ ! -x "$JDK_HOME/bin/java" ]; then
  rm -rf "$JDK_HOME"; mkdir -p "$JDK_HOME"
  curl -fsSL -o /tmp/jdk.tgz \
    "https://api.adoptium.net/v3/binary/latest/${JDK_VERSION}/ga/linux/x64/jdk/hotspot/normal/eclipse"
  tar -xzf /tmp/jdk.tgz -C "$JDK_HOME" --strip-components=1; rm -f /tmp/jdk.tgz
fi
export JAVA_HOME="$JDK_HOME" PATH="$JDK_HOME/bin:$PATH"

# 2. Shared SDK + gradle caches (owner dev).
install -d -o "$DEV_USER" -g "$DEV_USER" "$ANDROID_SDK_ROOT" "$GRADLE_USER_HOME"
export ANDROID_HOME="$ANDROID_SDK_ROOT" ANDROID_SDK_ROOT

# 3. cmdline-tools — CMDLINE_TOOLS_URL, else newest from Google's manifest (numeric sort, not lexical).
if [ ! -x "$ANDROID_SDK_ROOT/cmdline-tools/latest/bin/sdkmanager" ]; then
  url="${CMDLINE_TOOLS_URL:-}"
  if [ -z "$url" ]; then
    rev="$(curl -fsSL https://dl.google.com/android/repository/repository2-3.xml \
      | grep -oE 'commandlinetools-linux-[0-9]+_latest\.zip' | sort -t- -k3 -n | tail -1)"
    url="https://dl.google.com/android/repository/${rev}"
  fi
  tmp="$(mktemp -d)"; curl -fsSL -o "$tmp/clt.zip" "$url"
  unzip -q "$tmp/clt.zip" -d "$tmp/x"
  install -d "$ANDROID_SDK_ROOT/cmdline-tools"
  mv "$tmp/x/cmdline-tools" "$ANDROID_SDK_ROOT/cmdline-tools/latest"; rm -rf "$tmp"
fi
SDKMANAGER="$ANDROID_SDK_ROOT/cmdline-tools/latest/bin/sdkmanager"

# 4. Licenses + components (idempotent).
yes 2>/dev/null | "$SDKMANAGER" --licenses >/dev/null 2>&1 || true
"$SDKMANAGER" "platform-tools" "platforms;android-${ANDROID_API}" \
  "build-tools;${BUILD_TOOLS_VERSION}" "emulator" "$SYSTEM_IMAGE" >/dev/null

# 5. Ownership (sdkmanager may write as root).
chown -R "$DEV_USER:$DEV_USER" "$ANDROID_SDK_ROOT" "$GRADLE_USER_HOME" "$JDK_HOME" 2>/dev/null || true

# 6. System-wide toolchain env for login shells / Gradle.
cat > /etc/profile.d/subyard-android.sh <<EOF
export JAVA_HOME="$JDK_HOME"
export ANDROID_HOME="$ANDROID_SDK_ROOT"
export ANDROID_SDK_ROOT="$ANDROID_SDK_ROOT"
export GRADLE_USER_HOME="$GRADLE_USER_HOME"
case ":\$PATH:" in
  *":\$JAVA_HOME/bin:"*) ;;
  *) PATH="\$JAVA_HOME/bin:\$ANDROID_HOME/platform-tools:\$ANDROID_HOME/cmdline-tools/latest/bin:\$PATH" ;;
esac
export PATH
EOF
chmod 0644 /etc/profile.d/subyard-android.sh

# 7. sdk.dir reconciliation. AGP precedence is sdk.dir > ANDROID_HOME and a missing sdk.dir is fatal;
#    a bind-mounted project's local.properties (host-only) pins a host path absent here → symlink that
#    host path to the in-yard SDK (discovered per project; never edit the shared file).
shopt -s nullglob
for lp in /srv/workspaces/*/src/local.properties; do
  d="$(sed -n 's/^[[:space:]]*sdk\.dir[[:space:]]*=[[:space:]]*//p' "$lp" | tail -n1)"; d="${d%$'\r'}"
  case "$d" in ""|"$ANDROID_SDK_ROOT") continue ;; esac
  if [ -L "$d" ]; then
    [ "$(readlink -f "$d" 2>/dev/null)" = "$(readlink -f "$ANDROID_SDK_ROOT")" ] && continue
  elif [ -e "$d" ]; then
    echo "android provision: '$d' exists and is not our symlink — leaving as-is" >&2; continue
  fi
  install -d "$(dirname "$d")"; ln -sfn "$ANDROID_SDK_ROOT" "$d"
  echo "android provision: sdk.dir reconciled  $d -> $ANDROID_SDK_ROOT"
done
shopt -u nullglob

echo "android provision OK: jdk=$("$JDK_HOME/bin/java" -version 2>&1 | head -1) sdk=$ANDROID_SDK_ROOT api=$ANDROID_API"
