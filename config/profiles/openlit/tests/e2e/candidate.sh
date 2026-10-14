# shellcheck shell=bash
# Complete the supported release transition before fixture power publication.
openlit_candidate_command() {
  # The initializer refreshes its child session; this shell may retain old groups.
  if ! id -nG | tr ' ' '\n' | grep -Fxq incus-admin \
    && id -nG "$(id -un)" | tr ' ' '\n' | grep -Fxq incus-admin; then
    local command
    printf -v command '%q ' "$@"
    sg incus-admin -c "exec $command"
  else
    "$@"
  fi
}

openlit_candidate_platform() {
  local root="$1" state="$2" runtime_source="$3"
  [ "${SUBYARD_E2E_VM:-}" = 1 ] && [ -r /run/subyard-e2e-lease.json ] \
    || { printf 'OpenLIT candidate platform requires an allocated disposable VM\n' >&2; return 2; }
  if ! openlit_candidate_command /usr/bin/incus storage show default --project default >/dev/null 2>&1; then
    (
      # Product setup owns Incus reconciliation; do not duplicate platform setup.
      # shellcheck source=tests/helpers/test-context.sh
      . "$root/tests/helpers/test-context.sh"
      setup_test_context "$state/platform"
      export SUBYARD_USER SUBYARD_OPERATOR_HOME="$HOME" SUBYARD_CONFIG_DIR="$runtime_source/config"
      SUBYARD_USER="$(id -un)"
      export STORAGE_PATH="$HOME/.cache/subyard-e2e-platform/incus/incus/storage"
      set -a
      # shellcheck source=config/host.env
      . "$runtime_source/config/host.env"
      set +a
      bash "$runtime_source/scripts/01-install-incus.sh" --yes
    ) >&2 || return
  fi
  # Use the same operator and refreshed group session as migration/yard commands.
  openlit_candidate_command /usr/bin/incus list --project default --format=json >/dev/null || return
  openlit_candidate_command /usr/bin/incus storage show default --project default >/dev/null || return
  openlit_candidate_command /usr/bin/incus network show incusbr0 --project default >/dev/null || return
}

openlit_candidate_install() {
  local root="$1" state="$2" release_root release_version candidate_rc candidate_engine runtime_source runtime_file arch installed
  # All bootstrap destinations must belong to the marked fixture before packaging,
  # platform setup or installer execution can mutate anything.
  python3 - "$state" <<'PY' || return 2
import os, pathlib, stat, sys
root = pathlib.Path(sys.argv[1])
def reject():
    raise SystemExit('OpenLIT candidate requires a private marked fixture root')
if not root.is_absolute() or not root.exists() or any(p.is_symlink() for p in (root, *root.parents)):
    reject()
info = root.lstat()
if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != 0o700:
    reject()
marker = root / '.marker'
if not marker.exists() or marker.is_symlink():
    reject()
info = marker.lstat()
if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_nlink != 1 or info.st_mode & 0o022 or marker.read_text() != 'openlit-profile-e2e-v1\n':
    reject()
for name in ('release', 'releases', 'runtime', 'bin', 'platform', 'config', 'data', 'bashrc', 'profile'):
    path = root / name
    if path.is_symlink():
        reject()
    if path.exists():
        info = path.lstat()
        kind = stat.S_ISREG if name in ('bashrc', 'profile') else stat.S_ISDIR
        if not kind(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o022:
            reject()
for name in ('current', 'previous'):
    path = root / 'runtime' / name
    if (path.exists() or path.is_symlink()) and not path.resolve().is_relative_to(root / 'runtime'):
        reject()
PY
  # shellcheck source=tests/helpers/release-candidate.sh
  . "$root/tests/helpers/release-candidate.sh"
  if candidate_engine="$(release_candidate_prepare "$root")"; then
    release_root="$root/.subyard-acceptance/release"
    read -r release_version runtime_file < <(jq -er '[.version, .runtime_file] |
      select(all(.[]; type == "string")) | @tsv' "$root/.subyard-acceptance/candidate.json") \
      || { printf 'OpenLIT release candidate identity is invalid\n' >&2; return 2; }
    case "$release_version" in ''|*[!A-Za-z0-9._+-]*)
      printf 'OpenLIT release candidate version is invalid\n' >&2; return 2 ;;
    esac
    case "$(uname -s)/$(uname -m)" in
      Linux/x86_64) arch=amd64 ;;
      Linux/aarch64|Linux/arm64) arch=arm64 ;;
      *) printf 'OpenLIT release candidate platform is unsupported\n' >&2; return 2 ;;
    esac
    [ "$runtime_file" = "subyard-$release_version-linux-$arch.tar.gz" ] \
      || { printf 'OpenLIT release candidate archive differs from host bootstrap selection\n' >&2; return 2; }
    runtime_source="$(dirname "$(dirname "$candidate_engine")")"
  else
    candidate_rc=$?
    [ "$candidate_rc" = 1 ] || return "$candidate_rc"
    release_root="$state/release"
    release_version=0.1.0-openlit.e2e
    runtime_source="$root"
    # An engine-only build cannot commit the protected release transition.
    "$root/dev/package-engine.sh" --version "$release_version" --output-dir "$release_root" >&2 || return
  fi
  [ -f "$release_root/subyard-install.sh" ] && [ ! -L "$release_root/subyard-install.sh" ] \
    || { printf 'OpenLIT candidate bootstrap is missing or unsafe\n' >&2; return 2; }
  openlit_candidate_platform "$root" "$state" "$runtime_source" || return
  openlit_candidate_command env -u YARD_ENGINE_PATH YARD_RELEASE_BASE_URL="file://$release_root" YARD_RELEASE_CACHE="$state/releases" \
    SUBYARD_CONFIG_HOME="$state/config" SUBYARD_HOME="$state/data" \
    YARD_RUNTIME_ROOT="$state/runtime" YARD_BIN_DIR="$state/bin" \
    YARD_SHELL_RC="$state/bashrc" YARD_LOGIN_RC="$state/profile" \
    bash "$release_root/subyard-install.sh" --version "$release_version" --yes >&2 || return
  installed="$state/runtime/current/bin/yard"
  [ -x "$installed" ] || { printf 'OpenLIT candidate installed launcher is missing\n' >&2; return 2; }
  python3 - "$state/runtime" "$installed" <<'PY' || return 2
import os, pathlib, stat, sys
runtime, launcher = map(pathlib.Path, sys.argv[1:])
target = launcher.resolve()
info = target.stat()
if not target.is_relative_to(runtime) or not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o022:
    raise SystemExit('OpenLIT candidate installed launcher is outside the owned runtime')
PY
  openlit_candidate_command env -u YARD_ENGINE_PATH YARD_RUNTIME_ROOT="$state/runtime" \
    SUBYARD_CONFIG_HOME="$state/config" SUBYARD_HOME="$state/data" \
    "$installed" migrate --check --json | jq -e '.outcome.status == "ready"' >/dev/null || return
  openlit_candidate_command env -u YARD_ENGINE_PATH YARD_RUNTIME_ROOT="$state/runtime" \
    SUBYARD_CONFIG_HOME="$state/config" SUBYARD_HOME="$state/data" \
    "$installed" _publication-contracts >/dev/null || return
  printf '%s\n' "$installed"
}
