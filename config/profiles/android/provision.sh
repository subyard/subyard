#!/usr/bin/env bash
# subyard-provision-check-v1
# config/profiles/android/provision.sh — install the Android toolchain into the yard (run as root
# inside the yard by the Go provision workflow; idempotent).
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
ANDROID_SDK_ROOT="${ANDROID_SDK_ROOT:-/srv/cache/android-sdk}"
# profile.conf's GRADLE_USER_HOME is for L2; L1 agents share this yard-local cache.
GRADLE_USER_HOME="${YARD_GRADLE_USER_HOME:-/srv/cache/gradle}"
DEV_USER="${DEV_USER:-dev}"
JDK_HOME="/opt/jdk-${JDK_VERSION}"
PUBLIC_ROOT="${ANDROID_PUBLIC_ROOT:-$ANDROID_SDK_ROOT/.subyard}"
PROFILE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
emulator_libraries=(libpulse0 libnss3)

if [ "$check_only" -eq 1 ]; then
  root_prefix="${ANDROID_TEST_ROOT:-}"
  rooted() { printf '%s%s' "$root_prefix" "$1"; }
  changed=0
  for command in curl unzip flock setsid cage Xwayland python3 socat systemctl docker slirp4netns ip; do
    command -v "$command" >/dev/null 2>&1 || changed=1
  done
  for library in "${emulator_libraries[@]}"; do
    [ "$(dpkg-query -W -f='${Status}' "$library" 2>/dev/null)" = 'install ok installed' ] || changed=1
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
  expected_owner="$(id -u "$DEV_USER"):$(id -g "$DEV_USER")"
  [ -d "$gradle_root" ] && [ ! -L "$gradle_root" ] \
    && [ "$(stat -c '%u:%g' "$gradle_root" 2>/dev/null)" = "$expected_owner" ] || changed=1
  [ "$(docker image inspect -f '{{.Config.User}}' subyard-android-env:1 2>/dev/null)" = "$expected_owner" ] || changed=1
  profile_env="$(rooted /etc/profile.d/subyard-android.sh)"
  [ -f "$profile_env" ] && [ ! -L "$profile_env" ] \
    && grep -Fxq "export JAVA_HOME=\"$JDK_HOME\"" "$profile_env" \
    && grep -Fxq "export ANDROID_HOME=\"$ANDROID_SDK_ROOT\"" "$profile_env" \
    && grep -Fxq "export ANDROID_SDK_ROOT=\"$ANDROID_SDK_ROOT\"" "$profile_env" \
    && grep -Fxq "export GRADLE_USER_HOME=\"$GRADLE_USER_HOME\"" "$profile_env" \
    && grep -Fxq "export PATH=\"$PUBLIC_ROOT/bin:$JDK_HOME/bin:$ANDROID_SDK_ROOT/platform-tools:$ANDROID_SDK_ROOT/cmdline-tools/latest/bin:\$PATH\"" "$profile_env" \
    || changed=1
  install_root="$(rooted /usr/local/lib/subyard-android)"
  for source in pool.py client.py runtime.sh; do
    [ -f "$install_root/$source" ] && [ ! -L "$install_root/$source" ] \
      && cmp -s "$PROFILE_DIR/$source" "$install_root/$source" || changed=1
  done
  root_owner='0:0'
  [ -z "$root_prefix" ] || root_owner="$(id -u):$(id -g)"
  [ "$(stat -c '%u:%g' "$jdk_home" 2>/dev/null)" = "$root_owner" ] || changed=1
  [ "$(stat -c '%u:%g' "$sdk_root" 2>/dev/null)" = "$root_owner" ] || changed=1
  public_root="$(rooted "$PUBLIC_ROOT")"
  [ -x "$public_root/bin/android-broker" ] || changed=1
  for retired in yard yard-emu; do
    if [ -e "$public_root/bin/$retired" ] || [ -L "$public_root/bin/$retired" ]; then
      changed=1
    fi
  done
  config="$(rooted /etc/subyard-android.json)"
  python3 - "$config" "$ANDROID_SDK_ROOT" "$PUBLIC_ROOT" "${EMULATOR_POOL_SIZE:-2}" \
    "$ANDROID_API" "${EMULATOR_DEVICE:-phone}" "${ANDROID_VARIANT:-google_apis}" \
    "${ANDROID_ABI:-x86_64}" "${EMULATOR_GPU:-host}" "$DEV_USER" "$JDK_HOME" <<'PYCONFIG' || changed=1
import json, pathlib, sys
path, sdk, public, size, api, device, variant, abi, gpu, user, java = sys.argv[1:]
try:
    actual = json.loads(pathlib.Path(path).read_text())
    expected = dict(sdk_root=sdk, state_root='/var/lib/subyard-android', public_root=public,
                    size=int(size), api=int(api), device=device, variant=variant, abi=abi,
                    gpu=gpu, dev_user=user, runtime_user='subyard-android', java_home=java)
    if actual != expected:
        raise ValueError('configuration drift')
except (OSError, ValueError):
    raise SystemExit(1)
PYCONFIG
  unit="$(rooted /etc/systemd/system/subyard-android-pool.service)"
  [ -f "$unit" ] && grep -Fxq 'ExecStart=/usr/bin/python3 /usr/local/lib/subyard-android/pool.py serve' "$unit" \
    && grep -Fxq 'KillMode=control-group' "$unit" && grep -Fxq 'TimeoutStopSec=120' "$unit" || changed=1
  systemctl is-enabled --quiet subyard-android-pool.service >/dev/null 2>&1 || changed=1
  systemctl is-active --quiet subyard-android-pool.service >/dev/null 2>&1 || changed=1
  [ -S "$public_root/control.sock" ] || changed=1
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
apt-get install -y -qq curl unzip util-linux python3 socat systemd cage xwayland slirp4netns iproute2 \
  libgl1-mesa-dri libegl-mesa0 libgbm1 libglx-mesa0 "${emulator_libraries[@]}"
# Headless HW GLES for -gpu host: Mesa + a tiny wlroots compositor (cage) + Xwayland. The emulator's
# GLES uses GLX (needs an X display); the passed-through render node only does HW GL via EGL → the
# launcher bridges with a headless wlroots compositor on the render node → Xwayland → HW GLX.

# Refuse only the profile's known pre-pool runtime. User AVD/data remains untouched.
# shellcheck source=resources/emulator/process-identity.sh
. "$PROFILE_DIR/resources/emulator/process-identity.sh"
if pgrep -u "$DEV_USER" -f -- "$EMU_PROCESS_PATTERN" >/dev/null 2>&1; then
  printf 'android provision: legacy Android emulator is active; stop it explicitly before activating the pool\n' >&2
  exit 1
fi

# Exclusive SDK maintenance waits for all emulator read leases to finish.
install -d -o root -g root -m 0700 /run/lock/subyard-android
exec 9>/run/lock/subyard-android/sdk.lock
flock -w 30 9 || { printf 'android provision: SDK is in use; retry after leases finish\n' >&2; exit 1; }
export SUBYARD_ANDROID_SDK_LOCKED=1

# 1. JDK — Debian 13 has no openjdk-17; Temurin from Adoptium (redirects to the current GA).
if [ ! -x "$JDK_HOME/bin/java" ]; then
  rm -rf "$JDK_HOME"; mkdir -p "$JDK_HOME"
  curl -fsSL -o /tmp/jdk.tgz \
    "https://api.adoptium.net/v3/binary/latest/${JDK_VERSION}/ga/linux/x64/jdk/hotspot/normal/eclipse"
  tar -xzf /tmp/jdk.tgz -C "$JDK_HOME" --strip-components=1; rm -f /tmp/jdk.tgz
fi
export JAVA_HOME="$JDK_HOME" PATH="$JDK_HOME/bin:$PATH"

# 2. Shared SDK is a root-owned build input; Gradle remains the developer cache.
if [ -e "$ANDROID_SDK_ROOT" ] && { [ -L "$ANDROID_SDK_ROOT" ] || [ ! -d "$ANDROID_SDK_ROOT" ]; }; then
  printf 'android provision: SDK root is not a managed directory\n' >&2; exit 1
fi
install -d -o root -g root "$ANDROID_SDK_ROOT"
install -d -o "$DEV_USER" -g "$DEV_USER" "$GRADLE_USER_HOME"
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
packages=()
[ -x "$ANDROID_SDK_ROOT/platform-tools/adb" ] || packages+=(platform-tools)
[ -d "$ANDROID_SDK_ROOT/platforms/android-${ANDROID_API}" ] || packages+=("platforms;android-${ANDROID_API}")
[ -d "$ANDROID_SDK_ROOT/build-tools/${BUILD_TOOLS_VERSION}" ] || packages+=("build-tools;${BUILD_TOOLS_VERSION}")
[ -x "$ANDROID_SDK_ROOT/emulator/emulator" ] || packages+=(emulator)
if [ "${#packages[@]}" -ne 0 ]; then
  yes 2>/dev/null | "$SDKMANAGER" --licenses >/dev/null 2>&1 || true
  "$SDKMANAGER" "${packages[@]}" >/dev/null
fi

# 5. sdkmanager runs as root; leases download system images into private state instead.
chown -R root:root "$ANDROID_SDK_ROOT" "$JDK_HOME"
# Normalize existing SDK/JDK permissions without changing the public Unix socket's mode.
find "$ANDROID_SDK_ROOT" "$JDK_HOME" -type d -exec chmod a+rx,go-w {} +
find "$ANDROID_SDK_ROOT" "$JDK_HOME" -type f -exec chmod a+rX,go-w {} +
chown -R "$DEV_USER:$DEV_USER" "$GRADLE_USER_HOME"

# 6. System-wide toolchain env for login shells / Gradle.
cat > /etc/profile.d/subyard-android.sh <<EOF
export JAVA_HOME="$JDK_HOME"
export ANDROID_HOME="$ANDROID_SDK_ROOT"
export ANDROID_SDK_ROOT="$ANDROID_SDK_ROOT"
export GRADLE_USER_HOME="$GRADLE_USER_HOME"
export PATH="$PUBLIC_ROOT/bin:$JDK_HOME/bin:$ANDROID_SDK_ROOT/platform-tools:$ANDROID_SDK_ROOT/cmdline-tools/latest/bin:\$PATH"
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

# L2 uses a local base image. Docker is already a yard prerequisite; do not add a core branch.
docker info >/dev/null
dev_uid="$(id -u "$DEV_USER")"
dev_gid="$(id -g "$DEV_USER")"
if [ "$(docker image inspect -f '{{.Config.User}}' subyard-android-env:1 2>/dev/null || :)" != "$dev_uid:$dev_gid" ]; then
  docker build --build-arg DEV_UID="$dev_uid" --build-arg DEV_GID="$dev_gid" -t subyard-android-env:1 - <<'EOF'
FROM ubuntu:24.04
ARG DEV_UID
ARG DEV_GID
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends python3 openjdk-17-jdk libgl1 libegl1 libgbm1 && rm -rf /var/lib/apt/lists/*
RUN mkdir -p /home/dev && chown "$DEV_UID:$DEV_GID" /home/dev
ENV HOME=/home/dev
USER ${DEV_UID}:${DEV_GID}
EOF
fi
bash "$PROFILE_DIR/pool-install.sh"

echo "android provision OK: jdk=$("$JDK_HOME/bin/java" -version 2>&1 | head -1) sdk=$ANDROID_SDK_ROOT api=$ANDROID_API"
