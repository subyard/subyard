#!/usr/bin/env bash
# The Incus installer must establish and preserve an operator-owned data home before elevation.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

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
case "${1:-}" in
  --version)
    if [ "${TEST_UID:-1000}" = 0 ] && [ "${TEST_SWAP_AFTER_VALIDATION:-0}" = 1 ]; then
      rm -rf -- "$TEST_DATA_HOME"
      ln -s -- "$TEST_REPLACEMENT_HOME" "$TEST_DATA_HOME"
    fi
    printf '%s\n' "${TEST_INCUS_VERSION:-6.0.6}"
    ;;
  storage|network) exit 0 ;;
  *) exit 0 ;;
esac
SH
cat > "$TMP/bin/systemctl" <<'SH'
#!/usr/bin/env bash
case "${1:-}" in
  is-active) printf 'inactive\n' ;;
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
chmod 0755 "$TMP/bin"/*

run_installer() {
  local data_home="$1" command_path installer_path
  shift
  command_path="${TEST_COMMAND_PATH:-$TMP/bin:$PATH}"
  installer_path="${TEST_INSTALLER_PATH:-$ROOT/scripts/01-install-incus.sh}"
  PATH="$command_path" \
  TEST_OPERATOR_HOME="$TMP/operator" \
  TEST_OPERATOR_USER="$(id -un)" \
  TEST_DATA_HOME="$data_home" \
  TEST_CHOWN_LOG="$TMP/chown.log" \
  TEST_INSTALL_LOG="$TMP/install.log" \
  TEST_OPERATOR_GROUP="$(id -gn)" \
  TEST_INCUS_VERSION="${TEST_INCUS_VERSION:-}" \
  TEST_INCUS_FIXTURE="${TEST_INCUS_FIXTURE:-}" \
  TEST_FAKE_BIN="${TEST_FAKE_BIN:-}" \
  SUBYARD_ENGINE_CONTEXT=1 \
  SUBYARD_ENGINE_CONTEXT_SCHEMA=1 \
  SUBYARD_USER="$(id -un)" \
  SUBYARD_OPERATOR_HOME="$TMP/operator" \
  SUBYARD_CONFIG_DIR="$TMP/config" \
  SUBYARD_CONFIG_HOME="$TMP/operator/.config/subyard" \
  SUBYARD_HOME="$data_home" \
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
  DEV_USER=dev \
  DEV_UID=1000 \
  DEV_SUDO=0 \
  FORWARD_SSH_AGENT=0 \
  NESTED_E2E_VMS=0 \
  ASSUME_YES=1 \
  "$installer_path" --yes "$@"
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

# A successful package command must not let an Incus version below the required floor continue.
cp -R "$ROOT/scripts" "$TMP/version-scripts"
cat > "$TMP/version-scripts/lib/host.sh" <<'SH'
#!/usr/bin/env bash
require_root() { [ "$(id -u)" -eq 0 ]; }
nm_unmanaged_guard() { :; }
add_zabbly_lts_repo() { :; }
subyard_home_validate_root() {
  SUBYARD_VALIDATED_HOME="$1"
}
SH
version_bin="$TMP/version-bin"
mkdir "$version_bin"
for command_name in bash dirname cut realpath awk dpkg find stat cp sed sort grep tr install cat; do
  ln -s "$(command -v "$command_name")" "$version_bin/$command_name"
done
for command_name in getent id systemctl; do
  ln -s "$TMP/bin/$command_name" "$version_bin/$command_name"
done
cat > "$version_bin/apt-cache" <<'SH'
#!/usr/bin/env bash
printf '  Candidate: 6.0.6\n'
SH
cat > "$version_bin/apt-get" <<'SH'
#!/usr/bin/env bash
case " $* " in
  *' install '*) cp "$TEST_INCUS_FIXTURE" "$TEST_FAKE_BIN/incus" ;;
esac
SH
chmod 0755 "$version_bin/apt-cache" "$version_bin/apt-get"

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

printf 'ok: Incus installer protects the operator data-home boundary\n'
