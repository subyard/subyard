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
    if [ "${TEST_CLIENT_INIT:-0}" = 1 ]; then
      # Model Incus 6.0.6 PreRun: config and cache initialize even if the
      # subsequent server query fails; both have independent environment knobs.
      conf="${INCUS_CONF:-$HOME/.config/incus}"
      cache="${INCUS_CACHE:-$HOME/.cache/incus}"
      printf '%s|%s|%s\n' "$conf" "$cache" "$(stat -c '%a' "$(dirname "$conf")")" >> "$TEST_CLIENT_PATH_LOG"
      mkdir -p "$conf" "$cache"
      printf 'initialized\n' > "$conf/config.yml"
      printf 'initialized\n' > "$cache/cache-marker"
    fi
    [ "${TEST_SERVER_ROOT_ONLY:-0}" != 1 ] || [ "${TEST_UID:-1000}" = 0 ] || exit 7
    server="${TEST_INCUS_SERVER_VERSION-${TEST_INCUS_VERSION:-6.0.6}}"
    [ -z "${TEST_SERVER_VERSION_FILE:-}" ] || server="$(cat "$TEST_SERVER_VERSION_FILE")"
    printf 'Client version: %s\nServer version: %s\n' "${TEST_INCUS_VERSION:-6.0.6}" "$server"
    exit "${TEST_SERVER_QUERY_RC:-0}"
    ;;
  network)
    [ "${2:-}" != list ] || printf '[{"name":"incusbr0","type":"bridge","managed":true}]\n'
    exit 0 ;;
  storage) exit 0 ;;
  *) exit 0 ;;
esac
SH
cat > "$TMP/bin/systemctl" <<'SH'
#!/usr/bin/env bash
[ -z "${TEST_EFFECT_LOG:-}" ] || printf 'systemctl %s\n' "$*" >> "$TEST_EFFECT_LOG"
if [ "${TEST_EFFECT_STDOUT:-0}" = 1 ] && [ "$*" != 'is-active --quiet NetworkManager' ] \
  && [ "$*" != 'is-active --quiet incus.service' ]; then
  printf 'TEST_EFFECT systemctl %s\n' "$*" >&3
fi
case "${1:-}" in
  is-active)
    if [ "${2:-}" = --quiet ]; then
      case "${3:-}" in
        NetworkManager) [ "${TEST_NM_STATE:-inactive}" = active ]; exit $? ;;
        incus.service) [ "${TEST_INCUS_SERVICE_STATE:-active}" = active ]; exit $? ;;
        *) exit 99 ;;
      esac
    fi
    printf '%s\n' "${TEST_NM_STATE:-inactive}"
    ;;
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

# NetworkManager read this guard when it started in an earlier boot, so active-NetworkManager cases
# continue instead of stopping for the first reboot, which tests/lib-power-network.sh covers.
seed_read_guard() {
  mkdir -p "$TMP/etc/NetworkManager/conf.d" "$TMP/var/lib/subyard"
  printf '%s\n' '# Subyard: keep yard veths, Incus tap devices and Incus bridges unmanaged.' '[device-subyard]' \
    'match-device=type:veth,driver:veth,interface-name:tap????????,interface-name:incusbr0' 'managed=0' \
    > "$TMP/etc/NetworkManager/conf.d/zz-subyard-unmanaged.conf"
  printf 'earlier-boot %s held init\n' \
    "$(bash -c '. "$1"; power_nm_guard_static_hash "$2"' _ "$ROOT/scripts/lib-power.sh" "$TMP/etc/NetworkManager/conf.d/zz-subyard-unmanaged.conf")" \
    > "$TMP/var/lib/subyard/nm-guard-state"
}

run_installer() (
  local data_home="$1" command_path installer_path operator_user operator_group
  shift
  [ "${TEST_NM_STATE:-inactive}" != active ] || seed_read_guard
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
  if [ "${TEST_DECLINE:-0}" = 1 ]; then
    export ASSUME_YES=0
    local command_line
    printf -v command_line '%q ' "$installer_path" "$@"
    printf 'n\n' | SHELL=/bin/bash script --echo never -qefc "$command_line 3>&1" /dev/null
  else
    "$installer_path" --yes "$@" 3>&1
  fi
)

# Assert the actual announce block, including --yes retention and absence of
# misleading restart lines. Effect markers bypass command-substitution stdout.
assert_restart_plan() { # <output> <none|upgrade|required|conditional> <NM state>
  local output="$1" kind="$2" nm_state="$3" line expected='' plan last_plan first_effect prompt
  plan="$(sed -n '/This will:/,/^\r*$/p' "$output" | tr -d '\r')"
  [ -n "$plan" ] || fail 'installer omitted its plan'
  case "$kind" in
    upgrade) expected='upgrade incus from the Zabbly LTS-6.0 repository and restart the Incus daemon (below the supported minimum 6.0.6)' ;;
    required) expected='restart the Incus daemon to finish the required upgrade' ;;
    conditional) expected='restart the Incus daemon if it still runs a version below 6.0.6' ;;
  esac
  if [ -n "$expected" ]; then
    grep -Fxq "  • $expected" <<< "$plan" || fail "$kind plan omitted its exact restart line"
    [ "$(grep -c '  • .*restart.*daemon' <<< "$plan")" -eq 1 ] || fail "$kind plan added misleading restart lines"
  else
    ! grep -q '  • .*restart.*daemon' <<< "$plan" || fail 'steady/fresh plan promised a daemon restart'
  fi
  line='  • mark Incus devices unmanaged in NetworkManager before the change'
  if [ "$kind" != none ] && [ "$nm_state" = active ]; then
    grep -Fxq "$line" <<< "$plan" || fail "$kind plan omitted the active NetworkManager guard"
  else
    ! grep -Fq "$line" <<< "$plan" || fail "$kind plan added an unnecessary NetworkManager guard line"
  fi
  last_plan="$(grep -n '^  • ' "$output" | tail -n 1 | cut -d: -f1)"
  first_effect="$(sed -n '/^TEST_EFFECT /=' "$output" | sed -n '1p')"
  [ -z "$first_effect" ] || [ "$last_plan" -lt "$first_effect" ] || fail 'plan followed a guard/package/restart effect'
  prompt="$(grep -n 'Proceed?' "$output" | cut -d: -f1 | sed -n '1p' || true)"
  [ -z "$prompt" ] || [ "$last_plan" -lt "$prompt" ] || fail 'plan followed consent'
}

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
for command_name in bash dirname cut realpath awk find stat cp sed sort grep tr cat python3 cmp chmod rm tail script mktemp mkdir mv sha256sum join; do
  ln -s "$(command -v "$command_name")" "$version_bin/$command_name"
done
for command_name in getent id systemctl sudo; do
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
[ "${TEST_EFFECT_STDOUT:-0}" != 1 ] || printf 'TEST_EFFECT apt-get %s\n' "$*" >&3
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
[ "${TEST_EFFECT_STDOUT:-0}" != 1 ] || printf 'TEST_EFFECT install %s\n' "$*" >&3
case "${*: -1}" in "$TEST_FIXTURE_ROOT"/*) ;; *) exit 99 ;; esac
exec /usr/bin/install "$@"
SH
cat > "$version_bin/nmcli" <<'SH'
#!/usr/bin/env bash
case "$*" in
  "-t -f DEVICE,STATE device") printf 'incusbr0:unmanaged\n' ;;
  "-t -f DEVICE,TYPE,STATE device") printf 'incusbr0:bridge:unmanaged\n' ;;
  "-g GENERAL.STATE device show "*) printf '10 (unmanaged)\n' ;;
  "device set "*) [ -z "${TEST_EFFECT_LOG:-}" ] || printf 'nmcli %s\n' "$*" >> "$TEST_EFFECT_LOG" ;;
  general\ reload*) [ -z "${TEST_EFFECT_LOG:-}" ] || printf 'nmcli %s\n' "$*" >> "$TEST_EFFECT_LOG" ;;
  *) exit 99 ;;
esac
SH
cat > "$version_bin/NetworkManager" <<'SH'
#!/usr/bin/env bash
dir="$TEST_FIXTURE_ROOT/etc/NetworkManager/conf.d"
if [ "${1:-}" = --config-dir ]; then dir="$2"; shift 2; fi
[ "$*" = --print-config ] || exit 99
cat "$dir/zz-subyard-unmanaged.conf"
SH
# Retain the real reader/guard logic, redirecting only its absolute binary peer.
sed -i "s|/usr/sbin/NetworkManager|$version_bin/NetworkManager|g; s|/usr/bin/NetworkManager|$version_bin/NetworkManager|g; s|/sbin/NetworkManager|$version_bin/NetworkManager|g; s|/var/lib/subyard/|$TMP/var/lib/subyard/|g; s|/etc/NetworkManager/|$TMP/etc/NetworkManager/|g" \
  "$TMP/version-scripts/lib-power.sh"
# The package replaces the initial executable, keeping installed and final
# versions separate without making package effects implicit in --version.
cat > "$TMP/incus-initial" <<'SH'
#!/usr/bin/env bash
[ -z "${TEST_EFFECT_LOG:-}" ] || printf 'incus %s\n' "$*" >> "$TEST_EFFECT_LOG"
if [ "${1:-}" = --version ]; then printf '%s\n' "${TEST_INITIAL_INCUS_VERSION:-6.0.5}"; fi
if [ "${1:-} ${2:-}" = "network list" ]; then printf '[{"name":"incusbr0","type":"bridge","managed":true}]\n'; fi
SH
chmod 0755 "$version_bin/apt-cache" "$version_bin/apt-get" "$version_bin/curl" \
  "$version_bin/dpkg" "$version_bin/install" "$version_bin/NetworkManager" "$version_bin/nmcli" "$TMP/incus-initial"
export TEST_FIXTURE_ROOT="$TMP"

for package_path in upgrade install; do
  for mode in normal upgrade-only; do
    for version in 6.0.5 '?' 6.0.6 6.0.7; do
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
        6.0.6|6.0.7)
          [ "$status" -eq 0 ] || fail "$package_path $mode installer rejected the required Incus version"
          grep -Eq "incus (installed|present) \($version\)" "$version_output" \
            || fail "$package_path $mode installer omitted the verified Incus success result"
          if [ "$version" = 6.0.6 ]; then
            grep -Fq 'incus 6.0.6 is supported; 6.0.7 is recommended' "$version_output" \
              || fail 'supported Incus omitted the recommendation note'
          else
            ! grep -Fq 'is recommended' "$version_output" \
              || fail 'recommended Incus printed an unnecessary recommendation note'
          fi
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
        for nm_state in inactive active; do
          source_case=$((source_case + 1))
          label="$installed/$selector/$distro_candidate/$mode/$nm_state"
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
          run_source_case() {
            TEST_UID=0 TEST_INITIAL_INCUS_VERSION="$installed" TEST_INCUS_VERSION=6.0.6 \
              TEST_DISTRO_CANDIDATE="$distro_candidate" TEST_NM_STATE="$nm_state" TEST_EFFECT_STDOUT=1 \
              TEST_INCUS_FIXTURE="$TMP/bin/incus" TEST_FAKE_BIN="$version_bin" \
              TEST_COMMAND_PATH="$version_bin" TEST_EFFECT_LOG="$effect_log" TEST_DISPATCHER_PATH='' \
              TEST_INSTALLER_PATH="$TMP/version-scripts/01-install-incus.sh" \
              run_installer "$source_home" "${source_args[@]}"
          }
          plan_kind=none
          [ "$expected" != zabbly-upgrade ] || plan_kind=upgrade
          if TEST_DECLINE=1 run_source_case >"$source_output" 2>&1; then
            fail "$label ignored declined consent"
          fi
          assert_restart_plan "$source_output" "$plan_kind" "$nm_state"
          grep -Fq 'Proceed?' "$source_output" || fail "$label did not request consent"
          ! grep -Eq '^systemctl (is-active NetworkManager|try-restart)|^apt-get |^install ' "$effect_log" \
            || fail "$label ran a guard/package/restart effect before consent"
          : > "$effect_log"
          if ! run_source_case >"$source_output" 2>&1; then
            cat "$source_output" >&2
            fail "$label source selection failed"
          fi
          assert_restart_plan "$source_output" "$plan_kind" "$nm_state"
          guard_line="$(sed -n '/^systemctl is-active NetworkManager$/=' "$effect_log" | sed -n '1p')"
          [ -n "$guard_line" ] || fail "$label did not run the real NetworkManager guard"
          package_line="$(sed -n '/^apt-get /=' "$effect_log" | sed -n '1p')"
          restart_line="$(sed -n '/^systemctl try-restart incus.service$/=' "$effect_log")"
          install_line="$(sed -n '/^apt-get install /=' "$effect_log")"
          repo_line="$(sed -n '/^install -d -m 0755 .*\/apt\/keyrings$/=' "$effect_log" | sed -n '1p')"
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
          # Each guard run checks NetworkManager's state, before and again after a file write.
          expected_guards=1
          [ "$mode" != normal ] || expected_guards=2
          [ "$guard_count" -ge "$expected_guards" ] && [ "$guard_count" -le $((expected_guards * 2)) ] \
            || fail "$label omitted or repeated a guard reconcile"
          ! grep -Eq '^(systemctl (reload|restart|try-restart|reload-or-restart) NetworkManager|nmcli general reload)' "$effect_log" \
            || fail "$label reloaded or restarted NetworkManager"
        done
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

# Daemon plans in both modes, with exact consent/--yes text and real active-NM
# guard effects. Supported daemons may lag a newer package without restarting.
daemon_case=0
while IFS='|' read -r client server plan_kind query_rc root_only service_state; do
  [ "$server" != empty ] || server=''
  for mode in normal upgrade-only; do
    for nm_state in inactive active; do
      daemon_case=$((daemon_case + 1))
      label="$client/$server/$mode/$nm_state/root-only=$root_only/query=$query_rc"
      server_home="$TMP/server-$daemon_case/.subyard"
      mkdir -p "$server_home/incus/storage"
      server_args=()
      [ "$mode" = normal ] || server_args+=(--upgrade-only)
      cp "$TMP/bin/incus" "$version_bin/incus"
      effect_log="$TMP/server-$daemon_case.effects"
      server_output="$TMP/server-$daemon_case.out"
      : > "$effect_log"
      uid=0
      [ "$root_only" != 1 ] || uid=1000
      run_daemon_case() {
        TEST_UID="$uid" TEST_INCUS_VERSION="$client" TEST_INCUS_SERVER_VERSION="$server" \
          TEST_SERVER_QUERY_RC="$query_rc" TEST_SERVER_ROOT_ONLY="$root_only" TEST_SUDO_MODE=reexec \
          TEST_INCUS_SERVICE_STATE="${service_state:-active}" \
          TEST_NM_STATE="$nm_state" TEST_EFFECT_STDOUT=1 TEST_COMMAND_PATH="$version_bin" \
          TEST_EFFECT_LOG="$effect_log" TEST_DISPATCHER_PATH='' \
          TEST_INSTALLER_PATH="$TMP/version-scripts/01-install-incus.sh" \
          run_installer "$server_home" "${server_args[@]}"
      }
      if TEST_DECLINE=1 run_daemon_case >"$server_output" 2>&1; then fail "$label ignored declined consent"; fi
      assert_restart_plan "$server_output" "$plan_kind" "$nm_state"
      grep -Fq 'Proceed?' "$server_output" || fail "$label did not request consent"
      ! grep -Eq '^systemctl (is-active NetworkManager|try-restart)|^apt-get |^install ' "$effect_log" \
        || fail "$label ran a guard/package/restart effect before consent"
      : > "$effect_log"
      if ! run_daemon_case >"$server_output" 2>&1; then
        cat "$server_output" >&2
        fail "$label daemon plan failed"
      fi
      assert_restart_plan "$server_output" "$plan_kind" "$nm_state"
      ! grep -q '^apt-get ' "$effect_log" || fail "$label changed packages"
      if [ "$service_state" = inactive ]; then
        ! grep -q '^incus version local:$' "$effect_log" || fail "$label queried a dormant daemon socket"
      fi
      if [ "$plan_kind" = required ] || { [ "$plan_kind" = conditional ] && [ "$server" = 6.0.5 ]; }; then
        [ "$(grep -c '^systemctl try-restart incus.service$' "$effect_log")" -eq 1 ] \
          || fail "$label did not restart the unsupported daemon exactly once"
      else
        ! grep -q '^systemctl try-restart ' "$effect_log" \
          || fail "$label restarted a supported/unknown daemon"
      fi
    done
  done
done <<'CASES'
6.0.6|6.0.5|required|0|0
6.0.7|6.0.5|required|0|0
6.0.7|6.0.6|none|0|0
6.0.7|6.0.7|none|0|0
6.0.7|7.0.0|none|0|0
6.0.7|unreachable|none|0|0
6.0.7|empty|none|0|0
6.0.7|6.0.5|none|9|0
6.0.7|6.0.5|conditional|0|1
6.0.7|6.0.6|conditional|0|1
6.0.7|unreachable|conditional|0|1
6.0.7|6.0.5|none|0|0|inactive
6.0.7|6.0.5|none|0|1|inactive
CASES
printf 'ok: %s daemon/mode/NM cases disclose only required restarts before consent/effects\n' "$daemon_case"

# Refusing consent must not initialize the caller's Incus config/cache, including
# custom existing paths. Verify the temporary state is private and already gone
# after both successful and failed version reads.
client_case=0
for mode in normal upgrade-only; do
  for layout in default custom; do
    for query_rc in 0 9; do
      client_case=$((client_case + 1))
      client_home="$TMP/client-$client_case/home"
      mkdir -p "$client_home"
      if [ "$layout" = custom ]; then
        mkdir -p "$client_home/settings" "$client_home/cache"
        printf 'retain\n' > "$client_home/settings/sentinel"
        printf 'retain\n' > "$client_home/cache/sentinel"
      fi
      find "$client_home" -printf '%P %y\n' | sort > "$TMP/client-before"
      client_paths="$TMP/client-$client_case.paths"
      client_output="$TMP/client-$client_case.out"
      client_args=()
      [ "$mode" = normal ] || client_args+=(--upgrade-only)
      cp "$TMP/bin/incus" "$version_bin/incus"
      if (
        unset INCUS_CONF INCUS_CACHE
        export HOME="$client_home"
        if [ "$layout" = custom ]; then
          export INCUS_CONF="$client_home/settings" INCUS_CACHE="$client_home/cache"
        fi
        TEST_UID=1000 TEST_DECLINE=1 TEST_CLIENT_INIT=1 TEST_CLIENT_PATH_LOG="$client_paths" \
          TEST_INCUS_VERSION=6.0.7 TEST_INCUS_SERVER_VERSION=6.0.5 TEST_SERVER_QUERY_RC="$query_rc" \
          TEST_COMMAND_PATH="$version_bin" TEST_INSTALLER_PATH="$TMP/version-scripts/01-install-incus.sh" \
          run_installer "$TMP/client-$client_case/data" "${client_args[@]}"
      ) >"$client_output" 2>&1; then
        fail "$mode $layout query=$query_rc ignored declined consent"
      fi
      plan_kind=required
      [ "$query_rc" = 0 ] || plan_kind=conditional
      assert_restart_plan "$client_output" "$plan_kind" inactive
      grep -Fq 'aborted by user' "$client_output" || fail 'client probe did not reach consent refusal'
      find "$client_home" -printf '%P %y\n' | sort > "$TMP/client-after"
      cmp -s "$TMP/client-before" "$TMP/client-after" || fail 'declined plan initialized caller client state'
      [ "$(wc -l < "$client_paths")" -eq 1 ] || fail 'client-initialization fixture did not exercise the probe'
      IFS='|' read -r conf cache private_mode < "$client_paths"
      case "$conf" in /tmp/subyard-incus-plan.*/config) ;; *) fail 'probe used caller configuration' ;; esac
      [ "$cache" = "${conf%/config}/cache" ] || fail 'probe used caller cache'
      [ "$private_mode" = 700 ] || fail 'probe client state was not private'
      [ ! -e "${conf%/config}" ] || fail 'probe leaked temporary client state'
    done
  done
done
printf 'ok: %s refused plans preserve caller config/cache and remove private probe state\n' "$client_case"

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
