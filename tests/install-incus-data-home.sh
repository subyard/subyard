#!/usr/bin/env bash
# The Incus installer must establish and preserve an operator-owned data home before elevation.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d /tmp/subyard-install-incus.XXXXXXXX)"
trap 'rm -rf "$TMP"' EXIT
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

# shellcheck source=tests/helpers/test-context.sh
. "$ROOT/tests/helpers/test-context.sh"

mkdir -p "$TMP/bin"
cat > "$TMP/bin/getent" <<'SH'
#!/usr/bin/env bash
case "${1:-}:${2:-}" in
  passwd:"$TEST_OPERATOR_USER")
    printf '%s:x:%s:%s::%s:/bin/bash\n' "$TEST_OPERATOR_USER" \
      "$(/usr/bin/id -u)" "$(/usr/bin/id -g)" "$TEST_OPERATOR_HOME"
    ;;
  group:incus-admin) printf 'incus-admin:x:999:operator\n' ;;
  *) exit 2 ;;
esac
SH
cat > "$TMP/bin/id" <<'SH'
#!/usr/bin/env bash
case "${1:-}" in
  -u) [ "$#" -gt 1 ] && exec /usr/bin/id "$@"; printf '%s\n' "${TEST_UID:-1000}" ;;
  -g|-gn) exec /usr/bin/id "$@" ;;
  -nG) printf 'incus-admin\n' ;;
  *) exec /usr/bin/id "$@" ;;
esac
SH
cat > "$TMP/bin/incus" <<'SH'
#!/usr/bin/env bash
[ -z "${TEST_EFFECT_LOG:-}" ] || printf 'incus %s\n' "$*" >> "$TEST_EFFECT_LOG"
case "${1:-}" in
  --version)
    if [ "${TEST_UID:-1000}" = 0 ] && [ "${TEST_SWAP_AFTER_VALIDATION:-0}" = 1 ]; then
      rm -rf -- "$TEST_DATA_HOME"
      ln -s -- "$TEST_REPLACEMENT_HOME" "$TEST_DATA_HOME"
    fi
    printf '%s\n' "${TEST_INCUS_VERSION:-6.0.6}"
    ;;
  version)
    # The installer must query the local daemon, not the caller's default remote.
    [ "${2:-}" = local: ] || exit 97
    server="${TEST_INCUS_SERVER_VERSION-${TEST_INCUS_VERSION:-6.0.6}}"
    [ -z "${TEST_SERVER_VERSION_FILE:-}" ] || server="$(cat "$TEST_SERVER_VERSION_FILE")"
    printf 'Client version: %s\nServer version: %s\n' "${TEST_INCUS_VERSION:-6.0.6}" "$server"
    exit "${TEST_SERVER_QUERY_RC:-0}"
    ;;
  storage|network) exit 0 ;;
  *) exit 0 ;;
esac
SH
cat > "$TMP/bin/systemctl" <<'SH'
#!/usr/bin/env bash
[ -z "${TEST_EFFECT_LOG:-}" ] || printf 'systemctl %s\n' "$*" >> "$TEST_EFFECT_LOG"
case "${1:-}" in
  is-active) printf 'inactive\n' ;;
  try-restart)
    if [ "${TEST_RESTART_RC:-0}" != 0 ]; then
      printf 'fixture restart refused: injected service failure\n' >&2
      exit "$TEST_RESTART_RC"
    fi
    [ -z "${TEST_SERVER_VERSION_FILE:-}" ] || printf '%s\n' "$TEST_INCUS_VERSION" > "$TEST_SERVER_VERSION_FILE"
    ;;
esac
exit 0
SH
cat > "$TMP/bin/stat" <<'SH'
#!/usr/bin/env bash
if [ "${*: -1}" = "${TEST_WRONG_OWNER_PATH:-}" ] && [ "${1:-}" = -c ]; then
  printf '4242\n'
  exit 0
fi
exec /usr/bin/stat "$@"
SH
cat > "$TMP/bin/chown" <<'SH'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$TEST_CHOWN_LOG"
exit 99
SH
cat > "$TMP/bin/install" <<'SH'
#!/usr/bin/env bash
if [ "${TEST_STRICT_INSTALL:-0}" = 1 ] && [ "${TEST_UID:-1000}" = 0 ]; then
  printf '%s\n' "$*" >> "$TEST_INSTALL_LOG"
  [ "$#" -eq 6 ] && [ "$1" = -d ] && [ "$2" = -o ] && [ "$3" = "$TEST_OPERATOR_USER" ] \
    && [ "$4" = -g ] && [ "$5" = "$TEST_OPERATOR_GROUP" ] && [ "$6" = "$TEST_STORAGE_PATH" ] \
    || exit 98
  exit 0
fi
exec /usr/bin/install "$@"
SH
cat > "$TMP/bin/sudo" <<'SH'
#!/usr/bin/env bash
[ "${1:-}" != -n ] || exit 1
if [ "${TEST_SUDO_MODE:-stop}" = reexec ]; then
  [ "${1:-}" = -- ] && shift
  [ "${1:-}" = env ] && shift
  while [ "$#" -gt 0 ] && [[ "$1" = *=* ]]; do export "$1"; shift; done
  export TEST_UID=0
  exec "$@"
fi
[ -d "$TEST_DATA_HOME" ] || exit 46
[ "$(/usr/bin/stat -c '%u:%g' "$TEST_DATA_HOME")" = "$(/usr/bin/id -u):$(/usr/bin/id -g)" ] || exit 47
exit 43
SH
cat > "$TMP/bin/apt-get" <<'SH'
#!/usr/bin/env bash
[ "$*" = 'install -y -qq --no-install-recommends acl' ] || exit 97
SH
cat > "$TMP/bin/dispatcher" <<'SH'
#!/usr/bin/env bash
[ "$#" = 2 ] && [ "$1" = _incus-operator-access ] && [ "$2" = "$(/usr/bin/id -u)" ] || exit 96
SH
chmod 0755 "$TMP/bin"/*

run_installer() (
  local data_home="$1" command_path installer_path operator_user operator_group
  shift
  command_path="${TEST_COMMAND_PATH:-$TMP/bin:$PATH}"
  installer_path="${TEST_INSTALLER_PATH:-$ROOT/scripts/01-install-incus.sh}"
  operator_user="$(id -un)"
  operator_group="$(id -gn)"
  export PATH="$command_path" \
  TEST_OPERATOR_HOME="$TMP/operator" \
  TEST_OPERATOR_USER="$operator_user" \
  TEST_DATA_HOME="$data_home" \
  TEST_CHOWN_LOG="$TMP/chown.log" \
  TEST_INSTALL_LOG="$TMP/install.log" \
  TEST_OPERATOR_GROUP="$operator_group" \
  TEST_INCUS_VERSION="${TEST_INCUS_VERSION:-}" \
  TEST_INCUS_FIXTURE="${TEST_INCUS_FIXTURE:-}" \
  TEST_FAKE_BIN="${TEST_FAKE_BIN:-}" \
  SUBYARD_DISPATCHER_PATH="${TEST_DISPATCHER_PATH-$TMP/bin/dispatcher}" \
  SUBYARD_ENGINE_CONTEXT=1 \
  SUBYARD_ENGINE_CONTEXT_SCHEMA=1 \
  SUBYARD_USER="$operator_user" \
  SUBYARD_OPERATOR_HOME="$TMP/operator" \
  SUBYARD_CONFIG_DIR="$TMP/config" \
  SUBYARD_CONFIG_HOME="$TMP/operator/.config/subyard" \
  SUBYARD_HOME="$data_home" \
  SUBYARD_STATE_DIR="$data_home/state" \
  STORAGE_PATH="${TEST_STORAGE_PATH:-$data_home/incus/storage}" \
  STORAGE_POOL=default \
  HOST_BASE="$TMP/host-base" \
  RESTRICTED_DISK_PATHS="$TMP/host-base" \
  ACCESS_KIND=local \
  YARD_KIND=container \
  YARD_INSTANCE_NAME=yard-test \
  INCUS_PROJECT=subyard-test \
  INCUS_BRIDGE=incusbr0 \
  SSH_HOST=yard-test \
  SSH_PORT=2222 \
  SHIFT_MODE=shift \
  DEV_USER=dev \
  DEV_UID=1000 \
  DEV_SUDO=0 \
  FORWARD_SSH_AGENT=0 \
  NESTED_E2E_VMS=0 \
  ASSUME_YES=1
  SUBYARD_INSTALLATION_CONTEXT="$(test_installation_context default)"
  export SUBYARD_INSTALLATION_CONTEXT
  if [ "${TEST_WITHOUT_INSTALLATION_CONTEXT:-0}" = 1 ]; then
    unset SUBYARD_INSTALLATION_CONTEXT
  elif [ "${TEST_WITHOUT_INSTALLATION_CONTEXT:-0}" = 2 ]; then
    export SUBYARD_INSTALLATION_CONTEXT=''
  fi
  "$installer_path" --yes "$@"
)

# Every shared broad root and the exact operator home must be rejected before host operations.
for broad_home in / /boot /dev /etc /home /opt /proc /root /run /srv /sys /usr /var "$TMP/operator"; do
  if TEST_UID=0 run_installer "$broad_home"; then
    fail "installer accepted broad data home $broad_home"
  fi
done

symlink_home="$TMP/symlink-home"
mkdir -p "$TMP/symlink-target"
ln -s "$TMP/symlink-target" "$symlink_home"
if TEST_UID=0 run_installer "$symlink_home"; then
  fail 'installer accepted a symlink data home'
fi
non_directory_home="$TMP/non-directory-home"
touch "$non_directory_home"
if TEST_UID=0 run_installer "$non_directory_home"; then
  fail 'installer accepted a non-directory data home'
fi

# The unprivileged operator creates the directory before the sudo re-exec.
operator_home="$TMP/operator-home"
set +e
TEST_UID=1000 run_installer "$operator_home/.subyard" >"$TMP/operator.out" 2>&1
status=$?
set -e
[ "$status" -eq 43 ] || fail "operator-side preparation did not reach sudo re-exec (status=$status)"
[ -d "$operator_home/.subyard" ] || fail 'operator-side preparation did not create the data home'
[ "$(stat -c '%a' "$operator_home/.subyard")" = 700 ] \
  || fail 'operator-side preparation did not create the data home with mode 0700'

# A canonical sudo re-entry accepts the pre-created operator-owned home without chown.
canonical_home="$TMP/canonical/.subyard"
rm -f "$TMP/chown.log"
TEST_UID=1000 TEST_SUDO_MODE=reexec run_installer "$canonical_home" \
  || fail 'canonical root re-entry rejected the operator-prepared data home'
[ ! -e "$TMP/chown.log" ] || fail 'canonical root re-entry attempted ownership repair'

# Standalone trusted adapters need no captured dispatcher; they retain relogin semantics.
TEST_UID=1000 TEST_SUDO_MODE=reexec TEST_DISPATCHER_PATH='' run_installer "$canonical_home" \
  || fail 'standalone trusted installer required a dispatcher'
# Captured numeric actors cannot be replaced before installation.
if TEST_UID=0 run_installer "$canonical_home" --operator-uid=424242; then
  fail 'installer accepted a changed captured numeric operator'
fi

# A direct root run cannot create a missing home or repair a foreign owner.
missing_home="$TMP/missing/.subyard"
if TEST_UID=0 run_installer "$missing_home"; then
  fail 'direct-root run accepted a missing data home'
fi
[ ! -e "$missing_home" ] || fail 'direct-root run created a missing data home'

wrong_owner_home="$TMP/wrong-owner/.subyard"
mkdir -p "$wrong_owner_home/incus/storage"
for group in $(id -G); do
  [ "$group" = "$(id -g)" ] || { chgrp "$group" "$wrong_owner_home"; break; }
done
wrong_owner_metadata="$(stat -c '%u:%g:%a' "$wrong_owner_home")"
rm -f "$TMP/chown.log"
if TEST_UID=0 TEST_WRONG_OWNER_PATH="$wrong_owner_home" run_installer "$wrong_owner_home"; then
  fail 'direct-root run accepted a data home with the wrong owner'
fi
[ "$(stat -c '%u:%g:%a' "$wrong_owner_home")" = "$wrong_owner_metadata" ] \
  || fail 'wrong-owner rejection changed data-home metadata'
[ ! -e "$TMP/chown.log" ] || fail 'wrong-owner rejection attempted ownership repair'

# A custom storage leaf under a root-owned parent is created only by the elevated branch.
custom_home="$TMP/custom/.subyard"
custom_parent="$TMP/custom-storage-parent"
custom_storage="$custom_parent/storage"
mkdir -p "$custom_parent"
chmod 0500 "$custom_parent"
rm -f "$TMP/chown.log" "$TMP/install.log"
TEST_UID=1000 TEST_SUDO_MODE=reexec TEST_STRICT_INSTALL=1 TEST_STORAGE_PATH="$custom_storage" \
  run_installer "$custom_home" || fail 'custom storage path was not prepared by the elevated branch'
[ "$(cat "$TMP/install.log")" = "-d -o $(id -un) -g $(id -gn) $custom_storage" ] \
  || fail 'elevated storage creation did not use the exact operator-owned leaf arguments'
[ ! -e "$TMP/chown.log" ] || fail 'custom storage preparation attempted data-home ownership repair'

# A root run must not create storage through a data-home replacement after validation.
swap_home="$TMP/swap/.subyard"
replacement_home="$TMP/replacement"
mkdir -p "$swap_home/incus/storage" "$replacement_home"
if TEST_UID=0 TEST_SWAP_AFTER_VALIDATION=1 TEST_REPLACEMENT_HOME="$replacement_home" \
  run_installer "$swap_home"; then
  fail 'root run accepted a data-home replacement after validation'
fi
[ ! -e "$replacement_home/incus/storage" ] \
  || fail 'root run created storage through the replaced data home'

# An upgrade-only run must leave the data-home path entirely alone.
upgrade_home="$TMP/upgrade-only/.subyard"
if TEST_UID=0 run_installer "$upgrade_home" --upgrade-only; then
  :
else
  fail 'upgrade-only install failed'
fi
[ ! -e "$upgrade_home" ] || fail 'upgrade-only install touched the data home'

# The immutable-base recipe upgrades the dependency without installation authority.
# Normal installation must still require that authority before host operations.
contextless_home="$TMP/contextless/.subyard"
contextless_output="$TMP/contextless.out"
if TEST_UID=0 TEST_WITHOUT_INSTALLATION_CONTEXT=1 \
  run_installer "$contextless_home" --upgrade-only >"$contextless_output" 2>&1; then
  grep -Fq 'Incus is' "$contextless_output" \
    || fail 'contextless dependency upgrade did not reach its success boundary'
else
  cat "$contextless_output" >&2
  fail 'upgrade-only required an installation context'
fi
[ ! -e "$contextless_home" ] || fail 'contextless upgrade touched the data home'

if TEST_UID=1000 TEST_SUDO_MODE=reexec TEST_WITHOUT_INSTALLATION_CONTEXT=1 \
  run_installer "$contextless_home" --upgrade-only >"$contextless_output" 2>&1; then
  grep -Fq 'Incus is' "$contextless_output" \
    || fail 'contextless sudo upgrade did not reach its success boundary'
else
  cat "$contextless_output" >&2
  fail 'sudo upgrade-only required an installation context'
fi
[ ! -e "$contextless_home" ] || fail 'contextless sudo upgrade touched the data home'

# Normal installation requires nonempty authority before any host writes.
for absent_context in 1 2; do
  if TEST_UID=0 TEST_WITHOUT_INSTALLATION_CONTEXT="$absent_context" \
    run_installer "$contextless_home" >"$contextless_output" 2>&1; then
    fail 'normal installation accepted missing or empty installation authority'
  fi
  grep -Fq 'SUBYARD_INSTALLATION_CONTEXT' "$contextless_output" \
    || fail 'normal installation did not reject missing or empty installation authority'
  [ ! -e "$contextless_home" ] || fail 'rejected contextless installation touched the data home'
done

# A successful package command must not let an Incus version below the required floor continue.
cp -R "$ROOT/scripts" "$TMP/version-scripts"
# Execute the real host adapter, relocating only its absolute /etc paths into
# the fixture. No host functions are replaced; PATH peers cannot mutate the host.
sed -i "s|/etc/|$TMP/etc/|g" "$TMP/version-scripts/lib/host.sh"
mkdir -p "$TMP/etc/apt/"{keyrings,sources.list.d,preferences.d}
printf 'VERSION_CODENAME=test\n' > "$TMP/etc/os-release"
printf 'fixture-key\n' > "$TMP/etc/apt/keyrings/zabbly.asc"
version_bin="$TMP/version-bin"
mkdir "$version_bin"
for command_name in bash dirname cut realpath awk find stat cp sed sort grep tr cat python3 cmp chmod rm; do
  ln -s "$(command -v "$command_name")" "$version_bin/$command_name"
done
for command_name in getent id systemctl; do
  ln -s "$TMP/bin/$command_name" "$version_bin/$command_name"
done
cat > "$version_bin/apt-cache" <<'SH'
#!/usr/bin/env bash
[ -z "${TEST_EFFECT_LOG:-}" ] || printf 'apt-cache %s\n' "$*" >> "$TEST_EFFECT_LOG"
printf '  Candidate: %s\n' "${TEST_DISTRO_CANDIDATE-6.0.6}"
SH
cat > "$version_bin/apt-get" <<'SH'
#!/usr/bin/env bash
[ -z "${TEST_EFFECT_LOG:-}" ] || printf 'apt-get %s\n' "$*" >> "$TEST_EFFECT_LOG"
if [ "${1:-}" = "${TEST_APT_FAIL_COMMAND:-install}" ]; then
  [ "${TEST_APT_RC:-0}" = 0 ] || exit "$TEST_APT_RC"
fi
case " $* " in
  *' install '*) cp "$TEST_INCUS_FIXTURE" "$TEST_FAKE_BIN/incus" ;;
esac
SH
cat > "$version_bin/curl" <<'SH'
#!/usr/bin/env bash
printf 'unexpected signing-key download\n' >&2
exit 99
SH
cat > "$version_bin/dpkg" <<'SH'
#!/usr/bin/env bash
[ -z "${TEST_EFFECT_LOG:-}" ] || printf 'dpkg %s\n' "$*" >> "$TEST_EFFECT_LOG"
exec /usr/bin/dpkg "$@"
SH
cat > "$version_bin/install" <<'SH'
#!/usr/bin/env bash
[ -z "${TEST_EFFECT_LOG:-}" ] || printf 'install %s\n' "$*" >> "$TEST_EFFECT_LOG"
case "${*: -1}" in "$TEST_FIXTURE_ROOT"/*) ;; *) exit 99 ;; esac
exec /usr/bin/install "$@"
SH
# The package replaces the initial executable, keeping installed and final
# versions separate without making package effects implicit in --version.
cat > "$TMP/incus-initial" <<'SH'
#!/usr/bin/env bash
[ -z "${TEST_EFFECT_LOG:-}" ] || printf 'incus %s\n' "$*" >> "$TEST_EFFECT_LOG"
if [ "${1:-}" = --version ]; then printf '%s\n' "${TEST_INITIAL_INCUS_VERSION:-6.0.5}"; fi
SH
chmod 0755 "$version_bin/apt-cache" "$version_bin/apt-get" "$version_bin/curl" \
  "$version_bin/dpkg" "$version_bin/install" "$TMP/incus-initial"
export TEST_FIXTURE_ROOT="$TMP"

for package_path in upgrade install; do
  for mode in normal upgrade-only; do
    for version in 6.0.5 '?' 6.0.6; do
      rm -f "$version_bin/incus"
      [ "$package_path" = install ] || cp "$TMP/bin/incus" "$version_bin/incus"
      fixture_name="$package_path-$mode-${version//[^A-Za-z0-9]/x}"
      version_home="$TMP/version-$fixture_name/.subyard"
      mkdir -p "$version_home/incus/storage"
      version_output="$TMP/version-$fixture_name.out"
      version_args=(--zabbly)
      [ "$mode" = normal ] || version_args+=(--upgrade-only)
      set +e
      TEST_UID=0 TEST_INCUS_VERSION="$version" TEST_INCUS_FIXTURE="$TMP/bin/incus" \
        TEST_FAKE_BIN="$version_bin" TEST_COMMAND_PATH="$version_bin" \
        TEST_INSTALLER_PATH="$TMP/version-scripts/01-install-incus.sh" \
        run_installer "$version_home" "${version_args[@]}" >"$version_output" 2>&1
      status=$?
      set -e
      case "$version" in
        6.0.6)
          [ "$status" -eq 0 ] || fail "$package_path $mode installer rejected the required Incus version"
          grep -Eq 'incus (installed|present) \(6\.0\.6\)' "$version_output" \
            || fail "$package_path $mode installer omitted the verified Incus success result"
          ;;
        *)
          [ "$status" -ne 0 ] \
            || fail "$package_path $mode installer continued after its package command left Incus at $version"
          grep -Fq "incus $version < 6.0.6" "$version_output" \
            || fail "$package_path $mode rejection omitted detected version $version or required version 6.0.6"
          ! grep -Eq 'incus (installed|upgraded)' "$version_output" \
            || fail "$package_path $mode installer printed a package success result for Incus $version"
          ! grep -Fq 'Phase 1 done' "$version_output" \
            || fail "$package_path $mode installer printed the phase success summary for Incus $version"
          ! grep -Fq 'Incus is' "$version_output" \
            || fail "$package_path $mode installer printed the upgrade success summary for Incus $version"
          ;;
      esac
    done
  done
done

# Every source-selection row runs in both modes, including selector/candidate
# combinations ignored for an installed version. All physical peers share one log.
source_case=0
for installed in 6.0.6 7.0.0 6.0.5 none; do
  for selector in auto zabbly; do
    for distro_candidate in 6.0.5 6.0.6 '(none)' ''; do
      case "$installed:$selector:$distro_candidate" in
        6.0.6:*|7.0.0:*) expected=none ;;
        6.0.5:*) expected=zabbly-upgrade ;;
        none:auto:6.0.6) expected=distro ;;
        *) expected=zabbly ;;
      esac
      for mode in normal upgrade-only; do
        source_case=$((source_case + 1))
        label="$installed/$selector/$distro_candidate/$mode"
        source_home="$TMP/source-$source_case/.subyard"
        mkdir -p "$source_home/incus/storage"
        rm -f "$version_bin/incus" "$TMP/etc/apt/sources.list.d/zabbly-incus-lts-6.0.sources"
        [ "$installed" = none ] || cp "$TMP/incus-initial" "$version_bin/incus"
        source_args=()
        [ "$selector" = auto ] || source_args+=(--zabbly)
        [ "$mode" = normal ] || source_args+=(--upgrade-only)
        effect_log="$TMP/source-$source_case.effects"
        : > "$effect_log"
        source_output="$TMP/source-$source_case.out"
        if ! TEST_UID=0 TEST_INITIAL_INCUS_VERSION="$installed" TEST_INCUS_VERSION=6.0.6 \
          TEST_DISTRO_CANDIDATE="$distro_candidate" \
          TEST_INCUS_FIXTURE="$TMP/bin/incus" TEST_FAKE_BIN="$version_bin" \
          TEST_COMMAND_PATH="$version_bin" TEST_EFFECT_LOG="$effect_log" TEST_DISPATCHER_PATH='' \
          TEST_INSTALLER_PATH="$TMP/version-scripts/01-install-incus.sh" \
          run_installer "$source_home" "${source_args[@]}" >"$source_output" 2>&1; then
          cat "$source_output" >&2
          fail "$label source selection failed"
        fi
        guard_line="$(sed -n '/^systemctl is-active NetworkManager$/=' "$effect_log" | sed -n '1p')"
        [ -n "$guard_line" ] || fail "$label did not run the real NetworkManager guard"
        package_line="$(sed -n '/^apt-get /=' "$effect_log" | sed -n '1p')"
        restart_line="$(sed -n '/^systemctl try-restart incus.service$/=' "$effect_log")"
        install_line="$(sed -n '/^apt-get install /=' "$effect_log")"
        repo_line="$(sed -n '/^install -d -m 0755 /=' "$effect_log" | sed -n '1p')"
        if [ "$expected" = none ]; then
          [ -z "$package_line$restart_line" ] || fail "$label changed packages for a sufficient Incus version"
          grep -Fq "incus present ($installed) >= 6.0.6" "$source_output" \
            || fail "$label did not select the present version"
        else
          [ -n "$package_line" ] && [ "$guard_line" -lt "$package_line" ] \
            || fail "$label package effect preceded NetworkManager guard"
          [ -n "$install_line" ] || fail "$label did not install Incus"
        fi
        if [ "$expected" = zabbly-upgrade ]; then
          [ -n "$restart_line" ] && [ "$install_line" -lt "$restart_line" ] \
            && [ "$guard_line" -lt "$restart_line" ] \
            || fail "$label restart did not follow guarded installation"
          grep -Fq 'incus upgraded (6.0.6)' "$source_output" || fail "$label did not select upgrade"
        else
          [ -z "$restart_line" ] || fail "$label unexpectedly restarted Incus"
        fi
        case "$expected" in
          zabbly*)
            [ -n "$repo_line" ] && [ "$guard_line" -lt "$repo_line" ] \
              || fail "$label repository effect preceded NetworkManager guard"
            grep -Fq 'URIs: https://pkgs.zabbly.com/incus/lts-6.0' \
              "$TMP/etc/apt/sources.list.d/zabbly-incus-lts-6.0.sources" \
              || fail "$label did not select Zabbly"
            ;;
          *)
            [ ! -e "$TMP/etc/apt/sources.list.d/zabbly-incus-lts-6.0.sources" ] \
              || fail "$label unexpectedly added Zabbly"
            ;;
        esac
        ! grep -Fq 'could not list existing Incus bridges' "$source_output" \
          || fail "$label emitted a spurious Incus inventory warning"
        guard_count="$(grep -c '^systemctl is-active NetworkManager$' "$effect_log")"
        if [ "$mode" = normal ]; then
          [ "$guard_count" -eq 2 ] || fail "$label omitted the post-init guard reconcile"
        else
          [ "$guard_count" -eq 1 ] || fail "$label repeated the early guard"
        fi
      done
    done
  done
done
printf 'ok: %s Incus source/mode cases preserve guarded package ordering\n' "$source_case"

# Package and restart failures must stop at the operation boundary in both modes.
for mode in normal upgrade-only; do
  for failure in repo install restart; do
    failure_home="$TMP/failure-$mode-$failure/.subyard"
    mkdir -p "$failure_home/incus/storage"
    cp "$TMP/incus-initial" "$version_bin/incus"
    failure_args=()
    [ "$mode" = normal ] || failure_args+=(--upgrade-only)
    apt_rc=0 restart_rc=0 apt_command=install
    case "$failure" in
      repo) apt_rc=9; apt_command=update; expected_error='could not set up the Zabbly LTS-6.0 repo' ;;
      install) apt_rc=9; expected_error='incus upgrade failed' ;;
      restart) restart_rc=9; expected_error='incus restart failed' ;;
    esac
    if TEST_UID=0 TEST_INCUS_VERSION=6.0.6 TEST_APT_RC="$apt_rc" TEST_RESTART_RC="$restart_rc" \
      TEST_APT_FAIL_COMMAND="$apt_command" \
      TEST_INCUS_FIXTURE="$TMP/bin/incus" TEST_FAKE_BIN="$version_bin" \
      TEST_COMMAND_PATH="$version_bin" TEST_INSTALLER_PATH="$TMP/version-scripts/01-install-incus.sh" \
      run_installer "$failure_home" "${failure_args[@]}" >"$TMP/failure.out" 2>&1; then
      fail "$mode accepted $failure failure"
    fi
    grep -Fq "$expected_error" "$TMP/failure.out" || fail "$mode omitted the $failure failure diagnostic"
    ! grep -Eq 'Phase 1 done|Incus is|incus upgraded' "$TMP/failure.out" \
      || fail "$mode printed success after $failure failure"
    if [ "$failure" = restart ]; then
      grep -Fq 'fixture restart refused: injected service failure' "$TMP/failure.out" \
        || fail "$mode hid the daemon restart failure reason"
      printf '6.0.5\n' > "$TMP/server-version"
      effect_log="$TMP/retry-$mode.effects"
      # The package was already replaced by the failed attempt. A fresh run must
      # finish the stale daemon restart, then leave the equal version alone.
      for attempt in retry completed; do
        : > "$effect_log"
        if ! TEST_UID=0 TEST_INCUS_VERSION=6.0.6 TEST_SERVER_VERSION_FILE="$TMP/server-version" \
          TEST_COMMAND_PATH="$version_bin" TEST_EFFECT_LOG="$effect_log" TEST_DISPATCHER_PATH='' \
          TEST_INSTALLER_PATH="$TMP/version-scripts/01-install-incus.sh" \
          run_installer "$failure_home" "${failure_args[@]}" >"$TMP/retry.out" 2>&1; then
          cat "$TMP/retry.out" >&2
          fail "$mode $attempt after a failed daemon restart did not succeed"
        fi
        ! grep -q '^apt-get ' "$effect_log" || fail "$mode $attempt reinstalled the package"
        if [ "$attempt" = retry ]; then
          [ "$(grep -c '^systemctl try-restart incus.service$' "$effect_log")" -eq 1 ] \
            || fail "$mode rerun did not restart the stale daemon exactly once"
          guard_line="$(sed -n '/^systemctl is-active NetworkManager$/=' "$effect_log" | sed -n '1p')"
          restart_line="$(sed -n '/^systemctl try-restart incus.service$/=' "$effect_log")"
          [ "$guard_line" -lt "$restart_line" ] || fail "$mode rerun restarted before NetworkManager guard"
          [ "$(cat "$TMP/server-version")" = 6.0.6 ] || fail "$mode rerun left the old daemon running"
        else
          ! grep -q '^systemctl try-restart ' "$effect_log" || fail "$mode restarted the current daemon"
        fi
      done
    fi
  done
done

# A sufficient installed package needs no restart for equal, newer or unknown
# daemon versions. A failed query must not use even a partially returned version.
for mode in normal upgrade-only; do
  server_home="$TMP/server-$mode/.subyard"
  mkdir -p "$server_home/incus/storage"
  server_args=()
  [ "$mode" = normal ] || server_args+=(--upgrade-only)
  for server_version in 6.0.6 7.0.0 unreachable '' query-failed; do
    cp "$TMP/bin/incus" "$version_bin/incus"
    query_rc=0
    if [ "$server_version" = query-failed ]; then server_version=6.0.5; query_rc=9; fi
    : > "$TMP/server-version.effects"
    if ! TEST_UID=0 TEST_INCUS_VERSION=6.0.6 TEST_INCUS_SERVER_VERSION="$server_version" \
      TEST_SERVER_QUERY_RC="$query_rc" TEST_COMMAND_PATH="$version_bin" \
      TEST_EFFECT_LOG="$TMP/server-version.effects" TEST_DISPATCHER_PATH='' \
      TEST_INSTALLER_PATH="$TMP/version-scripts/01-install-incus.sh" \
      run_installer "$server_home" "${server_args[@]}" >"$TMP/server-version.out" 2>&1; then
      fail "$mode rejected daemon version '$server_version' (query status $query_rc)"
    fi
    ! grep -Eq '^apt-get |^systemctl try-restart ' "$TMP/server-version.effects" \
      || fail "$mode changed packages or restarted daemon version '$server_version' (query status $query_rc)"
  done
done

# The distro candidate check must read apt-cache to the end: output beyond the pipe buffer after the
# Candidate line would otherwise end apt-cache with SIGPIPE and fail the installer with 141.
cat > "$version_bin/apt-cache" <<'SH'
#!/usr/bin/env bash
printf '  Candidate: 6.0.6\n'
filler=x
for _ in {1..20}; do filler="$filler$filler"; done
printf '%s\n' "$filler"
SH
rm -f "$version_bin/incus"
sigpipe_home="$TMP/version-sigpipe/.subyard"
mkdir -p "$sigpipe_home/incus/storage"
set +e
TEST_UID=0 TEST_INCUS_VERSION=6.0.6 TEST_INCUS_FIXTURE="$TMP/bin/incus" \
  TEST_FAKE_BIN="$version_bin" TEST_COMMAND_PATH="$version_bin" \
  TEST_INSTALLER_PATH="$TMP/version-scripts/01-install-incus.sh" \
  run_installer "$sigpipe_home" >"$TMP/version-sigpipe.out" 2>&1
status=$?
set -e
[ "$status" -eq 0 ] || fail "distro candidate check with long apt-cache output failed with $status"
grep -Eq 'incus (installed|present) \(6\.0\.6\)' "$TMP/version-sigpipe.out" \
  || fail 'distro candidate check with long apt-cache output omitted the Incus success result'

printf 'ok: Incus installer protects the operator data-home boundary\n'
