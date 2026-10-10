#!/usr/bin/env bash
# Release-version GUI matrix inside an existing disposable owner fixture.
set -Eeuo pipefail
umask 077
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
die() { printf 'veranda-release-gui: %s\n' "$*" >&2; exit 1; }
[ "${SUBYARD_E2E_VM:-}" = 1 ] || die 'requires an allocated owner fixture'
{ [ "$#" = 4 ] && [ "$1" != --wayland-growth ]; } || { [ "$#" = 5 ] && [ "$1" = --wayland-growth ]; } || die 'expected base/next DEBs and owner fixture; or --growth DEB YARD OWNER; or --wayland-growth DEB ENGINE YARD OWNER'
growth=0
wayland=0
engine_input=()
if [ "$1" = --wayland-growth ]; then
  growth=1; wayland=1; BASE="$2" NEXT="$2" ENGINE="$3" YARD_NAME="$4" OWNER="$5"
  engine_input=("$ENGINE")
  [ "${SUBYARD_E2E_TYPE:-}" = android-test ] || die 'Wayland growth requires android-test singleton VM1'
elif [ "$1" = --growth ]; then
  growth=1; BASE="$2" NEXT="$2" YARD_NAME="$3" OWNER="$4"
else
  BASE="$1" NEXT="$2" YARD_NAME="$3" OWNER="$4"
fi
growth_cycles="${SUBYARD_E2E_VERANDA_GUI_GROWTH_CYCLES-20}"
case "$growth_cycles" in 20|100) ;; *) die 'SUBYARD_E2E_VERANDA_GUI_GROWTH_CYCLES must be 20 or 100' ;; esac
[ "$growth" = 1 ] || [ -z "${SUBYARD_E2E_VERANDA_GUI_GROWTH_CYCLES+x}" ] \
  || die 'growth cycle selection requires a growth mode'
[[ "$OWNER" =~ ^/var/tmp/subyard-veranda-owner\.[a-zA-Z0-9]+$ ]] || die 'invalid owner root'
[[ "$YARD_NAME" =~ ^vo-[a-z0-9]+$ ]] || die 'invalid named yard'
[ "${SUBYARD_CONFIG_HOME:-}" = "$OWNER/config" ] && [ "${SUBYARD_HOME:-}" = "$OWNER/data" ] || die 'owner configuration mismatch'
for command in python3 dpkg-deb sudo apt-get; do
  command -v "$command" >/dev/null || die 'missing GUI or SSH fixture prerequisite'
done
/usr/bin/python3 - "$OWNER" "$BASE" "$NEXT" "${engine_input[@]}" 2>/dev/null <<'PY' || die 'unsafe fixture inputs'
import os,pathlib,stat,sys
root=pathlib.Path(sys.argv[1]); info=root.lstat()
assert stat.S_ISDIR(info.st_mode) and info.st_uid==os.geteuid() and stat.S_IMODE(info.st_mode)==0o700
marker=root/'.marker'; fd=os.open(marker,os.O_RDONLY|os.O_NOFOLLOW)
with os.fdopen(fd) as stream:
    info=os.fstat(stream.fileno())
    assert stat.S_ISREG(info.st_mode) and info.st_uid==os.geteuid() and info.st_nlink==1 and stat.S_IMODE(info.st_mode)==0o600
    assert stream.read(128)=='subyard-veranda-owner-v1-'+root.name.split('.')[-1].lower()+'\n'
for name in sys.argv[2:]:
    info=pathlib.Path(name).lstat()
    assert stat.S_ISREG(info.st_mode) and info.st_uid==os.geteuid() and info.st_nlink==1
if len(sys.argv)==5:
    assert os.access(sys.argv[4],os.X_OK) and not stat.S_IMODE(pathlib.Path(sys.argv[4]).lstat().st_mode)&0o022
PY
FIXTURE="$(mktemp -d "$OWNER/release-gui.XXXXXX")"
chmod 0700 "$FIXTURE"
printf 'subyard-veranda-release-gui-v1\n' > "$FIXTURE/.marker"
chmod 0600 "$FIXTURE/.marker"
for path in home config cache runtime operator owner-home base next; do
  install -d -m 0700 "$FIXTURE/$path"
done
for name in build-base.log build-next.log gui.log prerequisites.log package-cleanup.log; do
  install -m 0600 /dev/null "$FIXTURE/$name"
done
installed_version=0.1.0
[ "$growth" = 0 ] || installed_version=0.1.1
if [ "$wayland" = 1 ]; then
  [ "$(timeout 5s "$ENGINE" --version)" = "$(basename -- "$ENGINE") 0.1.1" ] || die 'frozen engine release version mismatch'
fi
[ "$(dpkg-deb -f "$BASE" Package Version)" = "Package: subyard-veranda"$'\n'"Version: $installed_version" ] || die 'preparation package identity mismatch'
[ "$(dpkg-deb -f "$NEXT" Package Version)" = $'Package: subyard-veranda\nVersion: 0.1.1' ] || die 'next package identity mismatch'
[ "$(dpkg-deb -f "$BASE" Architecture)" = "$(dpkg --print-architecture)" ] \
  && [ "$(dpkg-deb -f "$NEXT" Architecture)" = "$(dpkg --print-architecture)" ] || die 'package architecture differs from VM'
[ "$(dpkg-query -W -f='${Status}' subyard-veranda 2>/dev/null || true)" != 'install ok installed' ] || die 'requires a fresh VM without Veranda installed'
package_owned=0
cleanup() {
  local rc=$?
  trap - EXIT INT TERM
  if [ "$package_owned" = 1 ]; then
    if [ "$(stat -c '%a:%u' "$FIXTURE")" = "700:$(id -u)" ] && [ ! -L "$FIXTURE" ] \
      && [ "$(stat -c '%a:%u:%h' "$FIXTURE/.marker")" = "600:$(id -u):1" ] \
      && [ ! -L "$FIXTURE/.marker" ] && [ "$(cat "$FIXTURE/.marker")" = subyard-veranda-release-gui-v1 ]; then
      if [ "$(dpkg-query -W -f='${Version}' subyard-veranda 2>/dev/null || true)" = "$installed_version" ]; then
        # The invoking user owns this exact private log; sudo must not create it.
        # shellcheck disable=SC2024
        sudo -n timeout 90s apt-get remove -y subyard-veranda > "$FIXTURE/package-cleanup.log" 2>&1 || rc=3
      elif [ "$(dpkg-query -W -f='${Status}' subyard-veranda 2>/dev/null || true)" = 'install ok installed' ]; then rc=3; fi
    else rc=3; fi
    [ "$rc" != 3 ] || printf 'veranda-release-gui: owned package cleanup failed\n' >&2
  fi
  exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
prepare_started=$SECONDS
package_owned=1
display_packages=()
[ "$wayland" = 0 ] || display_packages=(weston wayland-utils libegl-mesa0 libgl1-mesa-dri)
# Use the actual DEB dependency graph; this is preparation, not package acceptance.
# The invoking user owns this exact private log; sudo must not create it.
# shellcheck disable=SC2024
sudo -n env DEBIAN_FRONTEND=noninteractive timeout 300s apt-get install --no-install-recommends -y \
  "$BASE" xvfb xdotool dbus python3-gi gir1.2-atspi-2.0 at-spi2-core openssh-server libglib2.0-bin "${display_packages[@]}" \
  > "$FIXTURE/prerequisites.log" 2>&1 || die 'GUI prerequisite installation failed'
printf 'veranda-release-gui: prerequisite seconds=%s\n' "$((SECONDS - prepare_started))"
command -v xdotool >/dev/null || die 'missing GUI input fixture prerequisite'
[ "$growth" = 1 ] || dpkg-deb -x "$BASE" "$FIXTURE/base"
dpkg-deb -x "$NEXT" "$FIXTURE/next"
chmod 0700 "$FIXTURE/base" "$FIXTURE/next"
engine_started=$SECONDS
if [ "$growth" = 0 ]; then
  YARD_BUILD_VERSION=0.1.0 timeout 120s "$ROOT/dev/build-engine.sh" --force --output "$FIXTURE/yard-0.1.0" > "$FIXTURE/build-base.log" 2>&1 || die 'base engine build failed'
fi
if [ "$wayland" = 1 ]; then
  # The controller binds this staged executable to the frozen source/build receipt.
  install -m 0700 "$ENGINE" "$FIXTURE/yard-0.1.1"
else
  YARD_BUILD_VERSION=0.1.1 timeout 120s "$ROOT/dev/build-engine.sh" --force --output "$FIXTURE/yard-0.1.1" > "$FIXTURE/build-next.log" 2>&1 || die 'next engine build failed'
fi
printf 'veranda-release-gui: engine preparation seconds=%s\n' "$((SECONDS - engine_started))"
# The Python supervisor owns all display/agent/listener/app children and their identities.
# Only its static status goes to the controller; private runtime diagnostics stay in this root.
set +e
growth_args=()
[ "$growth" = 0 ] || growth_args=(--growth)
[ "$wayland" = 0 ] || growth_args=(--wayland-growth)
[ "$growth" = 0 ] || growth_args+=(--growth-cycles "$growth_cycles")
env -i PATH=/usr/sbin:/usr/bin:/bin LANG=C.UTF-8 HOME="$FIXTURE/home" \
  XDG_CONFIG_HOME="$FIXTURE/config" XDG_CACHE_HOME="$FIXTURE/cache" XDG_RUNTIME_DIR="$FIXTURE/runtime" \
  /usr/bin/python3 "$ROOT/dev/e2e/veranda-release-gui.py" --session \
  "$ROOT" "$FIXTURE" "$OWNER" "$YARD_NAME" "${STORAGE_PATH:?fixture storage is required}" "${growth_args[@]}" \
  > "$FIXTURE/gui.log" 2>&1
rc=$?
set -e
# Print only the supervisor's fixed, bounded JSON summary after private-file validation.
/usr/bin/python3 - "$ROOT" "$FIXTURE" "$rc" "$growth" "$wayland" "$growth_cycles" 2>/dev/null <<'PY' || die 'private GUI summary unavailable'
import base64,importlib.util,json,os,pathlib,stat,sys
sys.dont_write_bytecode=True
root=pathlib.Path(sys.argv[2]); info=root.lstat()
assert stat.S_ISDIR(info.st_mode) and info.st_uid==os.geteuid() and stat.S_IMODE(info.st_mode)==0o700
for name in ['.marker','summary.json']:
    fd=os.open(root/name,os.O_RDONLY|os.O_NOFOLLOW)
    with os.fdopen(fd) as stream:
        info=os.fstat(stream.fileno())
        assert stat.S_ISREG(info.st_mode) and info.st_uid==os.geteuid() and info.st_nlink==1 and stat.S_IMODE(info.st_mode)==0o600
        text=stream.read(4097); assert len(text)<=4096
        if name=='.marker': assert text=='subyard-veranda-release-gui-v1\n'
        else: result=json.loads(text)
spec=importlib.util.spec_from_file_location('release_gui',pathlib.Path(sys.argv[1])/'dev/e2e/veranda-release-gui.py')
module=importlib.util.module_from_spec(spec); spec.loader.exec_module(module)
growth = sys.argv[4]=='1'
wayland = sys.argv[5]=='1'
module.validate_summary(result,int(sys.argv[3]),growth,wayland,int(sys.argv[6]) if growth else None)
print(('veranda-wayland-growth: ' if wayland else 'veranda-gui-growth: ' if growth else 'veranda-release-gui: ')+json.dumps(result,separators=(',',':')))
if int(sys.argv[3]) != 0:
    diagnostic=module.failure_diagnostic(root)
    if diagnostic is not None and (diagnostic['failedCase'],diagnostic['failureStage'],diagnostic['failureCategory']) == (result['failedCase'],result['failureStage'],result['failureCategory']):
        print('veranda-release-gui-diagnostic: '+json.dumps(diagnostic,separators=(',',':')))
    image=module.failure_png(root)
    if image is not None:
        print('veranda-release-gui-screenshot: '+base64.b64encode(image).decode('ascii'))
PY
exit "$rc"
