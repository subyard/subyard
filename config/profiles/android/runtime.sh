#!/usr/bin/env bash
# Owned exclusively by a pool transient unit, including ADB and the compositor.
set -euo pipefail
check_graphics=0
case "${1:-}" in
  '') ;;
  --check-graphics) check_graphics=1 ;;
  *) echo 'runtime: unknown argument' >&2; exit 2 ;;
esac
[ "$check_graphics" = 1 ] || : "${ANDROID_HOME:?}" "${ANDROID_AVD_HOME:?}" "${SUBYARD_NETWORK_READY:?}"
gpu="${EMULATOR_GPU:-host}"
case "$gpu" in
  host|software|software-gles) ;;
  *) echo 'runtime: EMULATOR_GPU must be host, software or software-gles' >&2; exit 2 ;;
esac
case "$(id -un)" in
  subyard-android-[0-9][0-9][0-9]) ;;
  *) echo 'runtime requires a pool slot account' >&2; exit 1 ;;
esac
export XDG_RUNTIME_DIR="$HOME/xdg"
mkdir -p "$XDG_RUNTIME_DIR"
chmod 700 "$XDG_RUNTIME_DIR"
unset DISPLAY WAYLAND_DISPLAY ADB_SERVER_SOCKET ANDROID_SERIAL
if [ "$gpu" = host ]; then
  export WLR_BACKENDS=headless WLR_RENDERER=gles2 WLR_RENDERER_ALLOW_SOFTWARE=0
  for node in /dev/dri/renderD*; do
    [ -c "$node" ] || continue
    export WLR_RENDER_DRM_DEVICE="$node"
    break
  done
  : "${WLR_RENDER_DRM_DEVICE:?graphics render node unavailable}"
  [ "$check_graphics" = 0 ] || exec cage -- /bin/true
else
  [ "$check_graphics" = 0 ] || exit 0
fi
for _ in {1..200}; do
  [ ! -f "$SUBYARD_NETWORK_READY" ] || break
  sleep 0.1
done
[ -f "$SUBYARD_NETWORK_READY" ] || { echo 'pool egress did not become ready' >&2; exit 1; }
"$ANDROID_HOME/platform-tools/adb" start-server >/dev/null
if [ "$gpu" = host ]; then
  exec cage -- "$ANDROID_HOME/emulator/emulator" -avd managed -port 5554 -accel on -gpu host \
    -no-audio -no-snapshot -no-boot-anim -wipe-data -no-metrics -logcat '*:S'
fi
graphics=()
[ "$gpu" != software-gles ] || graphics=(-feature -Vulkan)
exec "$ANDROID_HOME/emulator/emulator" -avd managed -port 5554 -accel on -gpu swiftshader "${graphics[@]}" -no-window \
  -no-audio -no-snapshot -no-boot-anim -wipe-data -no-metrics -logcat '*:S'
