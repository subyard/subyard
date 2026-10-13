# shellcheck shell=bash
# Install the exact Android acceptance candidate through the supported bootstrap.
android_candidate_command() {
  # Enrollment changes account membership without updating the current shell.
  if ! id -nG | tr ' ' '\n' | grep -Fxq incus-admin \
    && id -nG "$(id -un)" | tr ' ' '\n' | grep -Fxq incus-admin; then
    local command
    printf -v command '%q ' "$@"
    sg incus-admin -c "exec $command"
  else
    "$@"
  fi
}

android_candidate_platform() {
  local root="$1" state="$2" runtime_source="$3"
  if ! android_candidate_command /usr/bin/incus storage show default --project default >/dev/null 2>&1; then
    (
      # Reuse the product initializer; immutable bases only install the packages.
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
  # Probe the API as the same operator/session used by migration and yard actions.
  android_candidate_command /usr/bin/incus list --project default --format=json >/dev/null || return
  android_candidate_command /usr/bin/incus storage show default --project default >/dev/null || return
  android_candidate_command /usr/bin/incus network show incusbr0 --project default >/dev/null || return
}

android_candidate_install() {
  local root="$1" state="$2" release_root release_version candidate_rc candidate_engine runtime_source runtime_file arch
  # shellcheck source=tests/helpers/release-candidate.sh
  . "$root/tests/helpers/release-candidate.sh"
  if candidate_engine="$(release_candidate_prepare "$root")"; then
    release_root="$root/.subyard-acceptance/release"
    read -r release_version runtime_file < <(jq -er '[.version, .runtime_file] |
      select(all(.[]; type == "string")) | @tsv' "$root/.subyard-acceptance/candidate.json") \
      || { printf 'Android release candidate identity is invalid\n' >&2; return 2; }
    case "$release_version" in ''|*[!A-Za-z0-9._+-]*)
      printf 'Android release candidate version is invalid\n' >&2; return 2 ;;
    esac
    case "$(uname -s)/$(uname -m)" in
      Linux/x86_64) arch=amd64 ;;
      Linux/aarch64|Linux/arm64) arch=arm64 ;;
      *) printf 'Android release candidate platform is unsupported\n' >&2; return 2 ;;
    esac
    [ "$runtime_file" = "subyard-$release_version-linux-$arch.tar.gz" ] \
      || { printf 'Android release candidate archive differs from the host bootstrap selection\n' >&2; return 2; }
    runtime_source="$(dirname "$(dirname "$candidate_engine")")"
  else
    candidate_rc=$?
    [ "$candidate_rc" = 1 ] || return "$candidate_rc"
    release_root="$state/release"
    release_version=0.1.0-android.e2e
    runtime_source="$root"
    # Packaging builds the engine and every declared profile/integration executable.
    "$root/dev/package-engine.sh" --version "$release_version" --output-dir "$release_root" >&2 || return
  fi
  android_candidate_platform "$root" "$state" "$runtime_source" || return
  android_candidate_command env YARD_RELEASE_BASE_URL="file://$release_root" YARD_RELEASE_CACHE="$state/releases" \
    YARD_RUNTIME_ROOT="$state/runtime" YARD_BIN_DIR="$state/bin" \
    YARD_SHELL_RC="$state/bashrc" YARD_LOGIN_RC="$state/profile" \
    bash "$release_root/subyard-install.sh" --version "$release_version" --yes >&2 || return
  printf '%s\n' "$state/runtime/current/bin/yard"
}
