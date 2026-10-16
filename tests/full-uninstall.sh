#!/usr/bin/env bash
# Actual installed-runtime cold uninstall with Incus absent from the tool path.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fixture="$(mktemp -d /tmp/subyard-full-uninstall.XXXXXX)"
trap '/bin/rm -rf -- "$fixture"' EXIT
version=0.17.0-test.full-uninstall
release="$fixture/release"
bash "$ROOT/dev/package-engine.sh" --output-dir "$release" --version "$version" --arch amd64 >/dev/null
bundle="$release/subyard-$version-linux-amd64.tar.gz"
export HOME="$fixture/home" SUBYARD_OPERATOR_HOME="$fixture/home"
# A sibling runtime/cache must not rely on audit logging to create DataHome.
export SUBYARD_NO_AUDIT=1
export SUBYARD_HOME="$fixture/data" SUBYARD_CONFIG_HOME="$fixture/config"
export YARD_RELEASE_CACHE="$fixture/cache"
export SUBYARD_POWER_UNIT_PATH="$fixture/units/subyard-power-reconcile.service"
export SUBYARD_POWER_RECONCILER_PATH="$fixture/helper"
export SUBYARD_KEYS_SYSTEMD_DIR="$fixture/user-units"
export SUBYARD_TEST_VMS_SINK_SERVICE_PATH="$fixture/units/subyard-test-vms-host-sink.service"
export SUBYARD_TEST_VMS_SINK_TIMER_PATH="$fixture/units/subyard-test-vms-host-sink.timer"
export SUBYARD_TEST_VMS_SINK_PATH="$fixture/sink"
mkdir -p "$HOME" "$fixture/tools"
runtime="$fixture/custom-runtime"
bash "$ROOT/scripts/install-runtime-release.sh" --runtime-root "$runtime" \
  --bundle "$bundle" --checksum "$bundle.sha256" --manifest "$bundle.manifest.json" \
  --provenance "$bundle.provenance.json" >/dev/null
# Keep only the two actual shell/Python entrypoint dependencies. No incus,
# systemctl, sudo or status binary is available to accidentally mask this case.
for command in bash dirname readlink python3 env; do
  ln -s "$(command -v "$command")" "$fixture/tools/$command"
done
unset YARD_ENGINE_PATH SUBYARD_INCUS_SOCKET
export PATH="$fixture/tools" INCUS_DIR="$fixture/never-created-incus" INCUS_SOCKET=''
if ! "$runtime/current/bin/yard" uninstall --full --yes > "$fixture/result.log" 2>&1; then
  /bin/cat "$fixture/result.log" >&2
  exit 1
fi
[ ! -e "$runtime" ] && [ ! -e "$SUBYARD_HOME" ] && [ ! -e "$SUBYARD_CONFIG_HOME" ] \
  && [ ! -e "$SUBYARD_HOME.uninstall" ] \
  || { /bin/cat "$fixture/result.log" >&2; exit 1; }
/bin/grep -Fq 'Fully removed this Subyard installation' "$fixture/result.log"
printf 'ok: current installed custom runtime fully uninstalls before Incus or host prerequisites exist\n'
# Supported bootstrap owns exact versioned cache artifacts. Foreign content
# beside them, including inside the same version directory, must survive.
for command in jq curl tar gzip stat uname sha256sum awk cut sed grep install mkdir mktemp mv rm ln chmod sync cp cmp sort tr cat getent readlink dirname basename flock find xargs touch date wc head id sleep; do
  for directory in /usr/bin /bin /usr/local/bin; do
    if [ -x "$directory/$command" ]; then /bin/ln -sf "$directory/$command" "$fixture/tools/$command"; break; fi
  done
done
export SUBYARD_HOME="$fixture/bootstrap-data" SUBYARD_CONFIG_HOME="$fixture/bootstrap-config"
export YARD_RUNTIME_ROOT="$fixture/bootstrap-runtime" YARD_RELEASE_CACHE="$fixture/bootstrap-cache"
export YARD_BIN_DIR="$fixture/bootstrap-bin" YARD_SHELL_RC="$fixture/bootstrap.rc" YARD_LOGIN_RC="$fixture/bootstrap.login"
export YARD_RELEASE_BASE_URL="file://$release"
# Existing unsafe targets must refuse before activating or creating inventory.
mkdir -m 0700 "$fixture/foreign-data-target"
ln -s "$fixture/foreign-data-target" "$SUBYARD_HOME"
if bash "$ROOT/dev/bootstrap-runtime.sh" --version "$version" --yes > "$fixture/unsafe-data.log" 2>&1; then exit 1; fi
grep -Fq 'unsafe installation data directory' "$fixture/unsafe-data.log"
/bin/unlink "$SUBYARD_HOME"
mkdir -m 0777 "$SUBYARD_HOME"
if bash "$ROOT/dev/bootstrap-runtime.sh" --version "$version" --yes > "$fixture/unsafe-data.log" 2>&1; then exit 1; fi
grep -Fq 'unsafe installation data directory' "$fixture/unsafe-data.log"
/bin/rmdir "$SUBYARD_HOME"
if ! bash "$ROOT/dev/bootstrap-runtime.sh" --version "$version" --yes > "$fixture/bootstrap.log" 2>&1; then
  /bin/cat "$fixture/bootstrap.log" >&2; exit 1
fi
[ "$(stat -c '%a' "$SUBYARD_HOME")" = 700 ]
printf 'foreign cache control\n' > "$YARD_RELEASE_CACHE/$version/foreign.control"
if ! "$YARD_BIN_DIR/yard" teardown --full --yes > "$fixture/bootstrap-full.log" 2>&1; then
  /bin/cat "$fixture/bootstrap-full.log" >&2; exit 1
fi
[ ! -e "$YARD_RUNTIME_ROOT" ] && [ ! -e "$SUBYARD_HOME" ] && [ ! -e "$SUBYARD_CONFIG_HOME" ] && [ ! -e "$YARD_BIN_DIR/yard" ] \
  || { /bin/cat "$fixture/bootstrap-full.log" >&2; exit 1; }
/bin/grep -Fxq 'foreign cache control' "$YARD_RELEASE_CACHE/$version/foreign.control"
[ "$(/usr/bin/find "$YARD_RELEASE_CACHE" -type f | /usr/bin/wc -l)" = 1 ] || { /bin/cat "$fixture/bootstrap-full.log" >&2; exit 1; }
printf 'ok: supported bootstrap cache artifacts removed; foreign file in the same version preserved\n'
