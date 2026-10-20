#!/usr/bin/env bash
# Shared storage layout, capacity diagnostics and guarded cleanup for the two-VM P0 lane.

p0_capacity_die() {
  printf 'p0-capacity: %s\n' "$*" >&2
  return 2
}

p0_capacity_init() {
  local token="${1:-}"
  [[ "$token" =~ ^[0-9]+$ ]] || p0_capacity_die 'allocation token must be numeric' || return
  [ -n "${HOME:-}" ] && [[ "$HOME" = /* ]] \
    || p0_capacity_die 'HOME must be an absolute path' || return
  [ ! -L "$HOME" ] && [ ! -L "$HOME/.cache" ] \
    || p0_capacity_die 'HOME and its cache root must not be symlinks' || return

  P0_CAPACITY_TOKEN="$token"
  P0_CAPACITY_MARKER="subyard-p0-$token"
  P0_CAPACITY_STATE_ROOT="$HOME/.cache/subyard-p0-$token"
  P0_CAPACITY_PLATFORM_ROOT="$HOME/.cache/subyard-e2e-platform"
  P0_CAPACITY_BUILD_CACHE="$P0_CAPACITY_STATE_ROOT/go-build"
  P0_CAPACITY_DEFAULT_BUILD_CACHE="$(env -u GOCACHE go env GOCACHE)"
  P0_CAPACITY_MODULE_CACHE="$(env -u GOMODCACHE go env GOMODCACHE)"
  export P0_CAPACITY_TOKEN P0_CAPACITY_MARKER P0_CAPACITY_STATE_ROOT
  export P0_CAPACITY_PLATFORM_ROOT P0_CAPACITY_BUILD_CACHE
  export P0_CAPACITY_DEFAULT_BUILD_CACHE P0_CAPACITY_MODULE_CACHE
}

p0_capacity_assert_root_marker() {
  [ -d "$P0_CAPACITY_STATE_ROOT" ] && [ ! -L "$P0_CAPACITY_STATE_ROOT" ] \
    || p0_capacity_die "state root is unavailable: $P0_CAPACITY_STATE_ROOT" || return
  p0_capacity_assert_tree_marker "$P0_CAPACITY_STATE_ROOT" "$P0_CAPACITY_MARKER"
}

p0_capacity_prepare_root() {
  local marker="$P0_CAPACITY_STATE_ROOT/.subyard-p0-marker"
  install -d -m 0711 "$HOME/.cache"
  if [ -e "$P0_CAPACITY_STATE_ROOT.subyard-p0-cleanup" ] || [ -L "$P0_CAPACITY_STATE_ROOT.subyard-p0-cleanup" ]; then
    p0_capacity_delete_tree "$P0_CAPACITY_STATE_ROOT" || return
  fi
  if [ -e "$P0_CAPACITY_STATE_ROOT" ]; then
    [ -d "$P0_CAPACITY_STATE_ROOT" ] && [ ! -L "$P0_CAPACITY_STATE_ROOT" ] \
      || p0_capacity_die "state root is not a plain directory: $P0_CAPACITY_STATE_ROOT" || return
    if [ -e "$marker" ]; then
      p0_capacity_assert_root_marker || return
    elif find "$P0_CAPACITY_STATE_ROOT" -mindepth 1 -print -quit | grep -q .; then
      p0_capacity_die "refusing non-empty unmarked state root $P0_CAPACITY_STATE_ROOT"
      return
    fi
  else
    install -d -m 0711 "$P0_CAPACITY_STATE_ROOT"
  fi
  printf '%s\n' "$P0_CAPACITY_MARKER" > "$marker"
  chmod 0600 "$marker"
  chmod 0711 "$P0_CAPACITY_STATE_ROOT"
}

p0_capacity_prepare_platform_root() {
  local marker="$P0_CAPACITY_PLATFORM_ROOT/.subyard-e2e-platform-marker"
  install -d -m 0711 "$HOME/.cache"
  if [ -e "$P0_CAPACITY_PLATFORM_ROOT" ]; then
    [ -d "$P0_CAPACITY_PLATFORM_ROOT" ] && [ ! -L "$P0_CAPACITY_PLATFORM_ROOT" ] \
      || p0_capacity_die "platform root is not a plain directory: $P0_CAPACITY_PLATFORM_ROOT" \
      || return
    if [ -e "$marker" ]; then
      [ "$(cat "$marker" 2>/dev/null)" = subyard-e2e-platform-v1 ] \
        || p0_capacity_die "refusing unmarked platform root $P0_CAPACITY_PLATFORM_ROOT" || return
    elif find "$P0_CAPACITY_PLATFORM_ROOT" -mindepth 1 -print -quit | grep -q .; then
      p0_capacity_die "refusing non-empty unmarked platform root $P0_CAPACITY_PLATFORM_ROOT"
      return
    fi
  else
    install -d -m 0711 "$P0_CAPACITY_PLATFORM_ROOT"
  fi
  printf '%s\n' subyard-e2e-platform-v1 > "$marker"
  chmod 0600 "$marker"
  chmod 0711 "$P0_CAPACITY_PLATFORM_ROOT"
}

p0_capacity_prepare_subtree() {
  local path="${1:?P0 subtree path is required}"
  p0_capacity_assert_root_marker || return
  case "$path" in
    "$P0_CAPACITY_STATE_ROOT"/*) ;;
    *) p0_capacity_die "unsafe P0 subtree $path"; return ;;
  esac
  if [ -e "$path.subyard-p0-cleanup" ] || [ -L "$path.subyard-p0-cleanup" ]; then
    p0_capacity_delete_tree "$path" || return
  fi
  if [ -e "$path" ]; then
    [ -d "$path" ] && [ ! -L "$path" ] \
      || p0_capacity_die "P0 subtree is not a plain directory: $path" || return
    [ "$(cat "$path/.subyard-p0-marker" 2>/dev/null)" = "$P0_CAPACITY_MARKER" ] \
      || p0_capacity_die "refusing unmarked P0 subtree $path" || return
  else
    install -d -m 0700 "$path"
    printf '%s\n' "$P0_CAPACITY_MARKER" > "$path/.subyard-p0-marker"
    chmod 0600 "$path/.subyard-p0-marker"
  fi
}

# Call only after proving ownership of the fixture containing this data home.
# Preserve the units: the next candidate installer enables the timer again.
p0_capacity_retire_fixture_sink() {
  local data_home="${1:?fixture data home is required}" state environment execution property value
  local service=subyard-test-vms-host-sink.service timer=subyard-test-vms-host-sink.timer
  local sink=/usr/local/libexec/subyard/test-vms-host-sink
  if [ -n "${2:-}" ]; then
    [[ "$2" =~ ^[0-9]+$ ]] || p0_capacity_die "invalid fixture sink token" || return
    service="subyard-p0-$2-test-vms-host-sink.service"
    timer="subyard-p0-$2-test-vms-host-sink.timer"
    sink="/usr/local/libexec/subyard/subyard-p0-$2-test-vms-host-sink"
  fi
  local environment_pattern='^SUBYARD_HOME=(/[A-Za-z0-9_./-]+) SUBYARD_OPERATOR_GID=[0-9]+$'
  local command_pattern='^\{ path=/usr/local/libexec/subyard/test-vms-host-sink ; argv\[\]=/usr/local/libexec/subyard/test-vms-host-sink _test-vms-host-sink sync ; ignore_errors=no ; [^{}]* \}$'
  command -v systemctl >/dev/null 2>&1 || return 0
  state="$(systemctl show "$service" -p LoadState --value)" \
    || { p0_capacity_die 'cannot inspect fixture host sink'; return; }
  [ "$state" != not-found ] || return 0
  environment="$(systemctl show "$service" -p Environment --value)" \
    || { p0_capacity_die 'cannot inspect fixture host sink data home'; return; }
  [[ "$environment" =~ $environment_pattern ]] \
    || { p0_capacity_die 'fixture host sink environment has an unexpected shape'; return; }
  [ "${BASH_REMATCH[1]}" = "$data_home" ] || return 0
  [ "$state" = loaded ] \
    || { p0_capacity_die 'matching fixture host sink is not loaded safely'; return; }
  execution="$(systemctl show "$service" -p ExecStart --value)" \
    || { p0_capacity_die 'cannot inspect matching fixture host sink command'; return; }
  command_pattern="${command_pattern//\/usr\/local\/libexec\/subyard\/test-vms-host-sink/$sink}"
  [[ "$execution" =~ $command_pattern ]] \
    || { p0_capacity_die 'matching fixture host sink has an unexpected command'; return; }
  for property in DropInPaths EnvironmentFiles ExecStartPre ExecStartPost ExecStop ExecStopPost; do
    value="$(systemctl show "$service" -p "$property" --value)" \
      || { p0_capacity_die 'cannot inspect matching fixture host sink overrides'; return; }
    [ -z "$value" ] \
      || { p0_capacity_die 'matching fixture host sink has unexpected overrides'; return; }
  done
  value="$(systemctl show "$timer" -p DropInPaths --value)" \
    || { p0_capacity_die 'cannot inspect matching fixture host sink timer'; return; }
  [ -z "$value" ] \
    || { p0_capacity_die 'matching fixture host sink timer has overrides'; return; }
  value="$(systemctl show "$timer" -p Unit --value)" \
    || { p0_capacity_die 'cannot inspect matching fixture host sink timer target'; return; }
  [ "$value" = "$service" ] \
    || { p0_capacity_die 'matching fixture host sink timer has an unexpected target'; return; }
  sudo -n systemctl disable --now "$timer" || return
  sudo -n systemctl stop "$service"
}

p0_capacity_cleanup_fixture_sink() {
  local OWNER_DATA_ROOT="$1" OWNER_TEST_VMS_SINK="$2"
  local OWNER_TEST_VMS_SINK_SERVICE="$3" OWNER_TEST_VMS_SINK_TIMER="$4" OWNER_TEST_VMS_SINK_TIMER_TEMPLATE="$5"
  local OWNER_TEST_VMS_SINK_SERVICE_NAME="${3##*/}" OWNER_TEST_VMS_SINK_TIMER_NAME="${4##*/}"
  local path sidecar present=0 touched=0 unsafe=0 installed_before custody_helper
  local -a sink_homes=()
  for path in "$OWNER_TEST_VMS_SINK" "$OWNER_TEST_VMS_SINK_SERVICE" \
    "$OWNER_TEST_VMS_SINK_TIMER"; do
    if [ -e "$path" ] || [ -L "$path" ]; then
      present=1
      [ -f "$path" ] && [ ! -L "$path" ] \
        && [ "$(stat -c '%u:%g' "$path" 2>/dev/null)" = 0:0 ] \
        || unsafe=1
    fi
    for sidecar in native-install.lock native-custody.json; do
      if [ -e "${path%/*}/.${path##*/}.$sidecar" ] || [ -L "${path%/*}/.${path##*/}.$sidecar" ]; then
        present=1
      fi
    done
  done
  if [ "$present" = 1 ]; then
    [ -e "$OWNER_TEST_VMS_SINK_SERVICE" ] \
      || unsafe=1
  fi
  if [ -e "$OWNER_TEST_VMS_SINK_TIMER" ]; then
    cmp -s "$OWNER_TEST_VMS_SINK_TIMER_TEMPLATE" "$OWNER_TEST_VMS_SINK_TIMER" \
      || unsafe=1
  fi
  if [ -e "$OWNER_TEST_VMS_SINK_SERVICE" ] && [ "$unsafe" = 0 ]; then
    mapfile -t sink_homes < <(sed -n 's/^Environment="SUBYARD_HOME=\([^"[:space:]]*\)"$/\1/p' \
      "$OWNER_TEST_VMS_SINK_SERVICE")
    [ "${#sink_homes[@]}" = 1 ] && [[ "${sink_homes[0]}" =~ ^/[A-Za-z0-9_./-]+$ ]] \
      && grep -Fqx "ExecStart=$OWNER_TEST_VMS_SINK _test-vms-host-sink sync" \
        "$OWNER_TEST_VMS_SINK_SERVICE" \
      || unsafe=1
  fi
  [ "$unsafe" = 0 ] \
    || { printf 'p0-guest: refusing unsafe owner test-vms sink artifacts\n' >&2; return 1; }
  # A retained platform sink can belong to another fixture. Its valid artifacts
  # are outside this owner's cleanup scope, just as in retire_fixture_sink.
  if [ "$present" = 1 ] && [ "${sink_homes[0]}" != "$OWNER_DATA_ROOT" ]; then
    return 0
  fi
  custody_helper="$(dirname "$OWNER_TEST_VMS_SINK_TIMER_TEMPLATE")/../../scripts/lib/native-install-custody.py"
  installed_before="$(python3 - "$custody_helper" "$OWNER_TEST_VMS_SINK" "$OWNER_TEST_VMS_SINK_TIMER" "$OWNER_TEST_VMS_SINK_SERVICE" <<'PY_INSTALLED'
import runpy,sys
print(runpy.run_path(sys.argv[1])['installed_files'](sys.argv[2:]))
PY_INSTALLED
  )" || return
  if [ -e "$OWNER_TEST_VMS_SINK_TIMER" ]; then
    sudo -n systemctl disable --now "$OWNER_TEST_VMS_SINK_TIMER_NAME" >/dev/null 2>&1 \
      || return 1
    touched=1
  fi
  if [ -e "$OWNER_TEST_VMS_SINK_SERVICE" ]; then
    sudo -n systemctl stop "$OWNER_TEST_VMS_SINK_SERVICE_NAME" >/dev/null 2>&1 \
      || return 1
    touched=1
  fi
  if [ "$present" = 1 ]; then
    # Retain all exact resource locks through removal. The account owner barrier
    # is shared with normal operations and deliberately remains outside this scope.
    sudo -n python3 - "$custody_helper" "$installed_before" \
      "$OWNER_TEST_VMS_SINK" "$OWNER_TEST_VMS_SINK_TIMER" "$OWNER_TEST_VMS_SINK_SERVICE" <<'PY_REMOVE_SINK' || return
import os,pathlib,runpy,sys
native=runpy.run_path(sys.argv[1]); paths=list(map(pathlib.Path,sys.argv[3:])); locks=[]; records=[]
try:
    for path in paths:
        lock=path.with_name('.'+path.name+'.native-install.lock')
        record=path.with_name('.'+path.name+'.native-custody.json')
        native['safe_parent'](path.parent)
        if lock.exists() or lock.is_symlink():
            fd=os.open(lock,os.O_RDWR|os.O_NOFOLLOW);locks.append((lock,fd))
            native['validate_lock'](lock,fd)
            native['observe'](record,'test-vms-host-sink');records.append(record)
        elif record.exists() or record.is_symlink():
            raise ValueError('sink custody record has no resource lock')
    if native['installed_files'](list(map(str,paths))) != sys.argv[2]:
        raise ValueError('sink artifacts changed before cleanup')
    for path in paths:
        if path.exists(): path.unlink()
    for record in records:
        if record.exists(): record.unlink()
    for lock,fd in locks:
        native['validate_lock'](lock,fd);lock.unlink()
finally:
    for _,fd in locks: os.close(fd)
PY_REMOVE_SINK
    touched=1
  fi
  [ "$touched" = 0 ] || sudo -n systemctl daemon-reload >/dev/null 2>&1
}

# A receipt is only authority to finish an empty directory with this exact inode.
# Keep it outside the tree so process loss after unlinking the marker is resumable.
p0_capacity_assert_finalization() {
  local path="$1" marker="$2" receipt="$1.subyard-p0-cleanup" identity
  [ "$(realpath -m -- "$path")" = "$path" ] \
    || p0_capacity_die "unsafe P0 finalization path $path" || return
  [ -f "$receipt" ] && [ ! -L "$receipt" ] && [ -O "$receipt" ] \
    && [ "$(stat -c %a "$receipt")" = 600 ] \
    || p0_capacity_die "unsafe P0 cleanup receipt $receipt" || return
  if [ -e "$path" ] || [ -L "$path" ]; then
    [ -d "$path" ] && [ ! -L "$path" ] \
      || p0_capacity_die "unsafe P0 finalization root $path" || return
    identity="$(stat -c '%d:%i' "$path")" || return
    [ "$(cat "$receipt")" = "$(printf '%s\n%s\n%s' "$marker" "$path" "$identity")" ] \
      || p0_capacity_die "P0 finalization identity changed: $path" || return
    [ -z "$(find "$path" -mindepth 1 -maxdepth 1 ! -name .subyard-p0-marker -print -quit)" ] \
      || p0_capacity_die "P0 finalization root has unexpected contents: $path" || return
  else
    # No inode remains to inspect, but never consume an unrelated receipt.
    local recorded_marker recorded_path recorded_identity extra
    { IFS= read -r recorded_marker; IFS= read -r recorded_path; IFS= read -r recorded_identity; extra="$(cat)"; } < "$receipt"
    [ "$recorded_marker" = "$marker" ] && [ "$recorded_path" = "$path" ] \
      && [[ "$recorded_identity" =~ ^[0-9]+:[0-9]+$ ]] && [ -z "$extra" ] \
      || p0_capacity_die "invalid orphan P0 cleanup receipt $receipt" || return
  fi
}

p0_capacity_assert_tree_marker() {
  local path="$1" marker="$2"
  [ -d "$path" ] && [ ! -L "$path" ] \
    || p0_capacity_die "unsafe P0 tree $path" || return
  if [ -e "$path.subyard-p0-cleanup" ] || [ -L "$path.subyard-p0-cleanup" ]; then
    p0_capacity_assert_finalization "$path" "$marker" || return
  fi
  if [ -e "$path/.subyard-p0-marker" ] || [ -L "$path/.subyard-p0-marker" ]; then
    [ -f "$path/.subyard-p0-marker" ] && [ ! -L "$path/.subyard-p0-marker" ] \
      && [ "$(cat "$path/.subyard-p0-marker")" = "$marker" ] \
      || p0_capacity_die "refusing unmarked P0 tree $path"
  else
    p0_capacity_assert_finalization "$path" "$marker"
  fi
}

p0_capacity_delete_tree() {
  local path="${1:?tree path is required}" marker="${2:-$P0_CAPACITY_MARKER}"
  local owner receipt="$path.subyard-p0-cleanup" temporary
  if [ ! -e "$path" ] && [ ! -L "$path" ]; then
    [ ! -e "$receipt" ] && [ ! -L "$receipt" ] && return 0
    p0_capacity_assert_finalization "$path" "$marker" || return
    find "$receipt" -maxdepth 0 -delete
    return
  fi
  p0_capacity_assert_tree_marker "$path" "$marker" || return
  owner="$(id -u)"
  local -a privilege=()
  if find "$path" -xdev ! -user "$owner" -print -quit 2>/dev/null | grep -q .; then
    privilege=(sudo -n)
  fi
  if [ ! -e "$receipt" ]; then
    "${privilege[@]}" find "$path" -xdev -type d -exec chmod u+rwx {} + || return
    "${privilege[@]}" find "$path" -xdev -depth -mindepth 1 \
      ! -path "$path/.subyard-p0-marker" -delete || return
    temporary="$(mktemp "$path/.subyard-p0-finalize.XXXXXX")" || return
    printf '%s\n%s\n%s\n' "$marker" "$path" "$(stat -c '%d:%i' "$path")" > "$temporary"
    chmod 0600 "$temporary"
    mv -T -n -- "$temporary" "$receipt" || return
    [ ! -e "$temporary" ] || { find "$temporary" -delete; return 1; }
  fi
  p0_capacity_assert_finalization "$path" "$marker" || return
  "${privilege[@]}" find "$path/.subyard-p0-marker" -maxdepth 0 -delete 2>/dev/null \
    || [ ! -e "$path/.subyard-p0-marker" ] || return
  "${privilege[@]}" rmdir -- "$path" || return
  find "$receipt" -maxdepth 0 -delete
}

p0_capacity_recover_stale_roots() {
  local path name token marker
  for path in "$HOME"/.cache/subyard-p0-*; do
    path="${path%.subyard-p0-cleanup}"
    [ -e "$path" ] || [ -e "$path.subyard-p0-cleanup" ] || continue
    [ "$path" != "$P0_CAPACITY_STATE_ROOT" ] || continue
    name="${path##*/}"
    token="${name#subyard-p0-}"
    [[ "$token" =~ ^[0-9]+$ ]] \
      || p0_capacity_die "refusing stale P0 state with invalid token $path" || return
    marker="subyard-p0-$token"
    if [ -e "$path" ]; then
      p0_capacity_assert_tree_marker "$path" "$marker" || return
    else
      p0_capacity_assert_finalization "$path" "$marker" || return
    fi
    if pgrep -u "$(id -u)" -f "$path" >/dev/null 2>&1; then
      p0_capacity_die "stale P0 state still has an active process: $path"
      return
    fi
    printf '  [ .. ] recovering marker-owned stale P0 state %s\n' "$name"
    # Retire a matching pre-isolation sink too; foreign common sinks remain untouched.
    p0_capacity_retire_fixture_sink "$path/owner/subyard" || return
    p0_capacity_retire_fixture_sink "$path/owner/subyard" "$token" || return
    p0_capacity_cleanup_fixture_sink "$path/owner/subyard" \
      "/usr/local/libexec/subyard/subyard-p0-$token-test-vms-host-sink" \
      "/etc/systemd/system/subyard-p0-$token-test-vms-host-sink.service" \
      "/etc/systemd/system/subyard-p0-$token-test-vms-host-sink.timer" \
      "$(dirname "${BASH_SOURCE[0]}")/../../config/systemd/subyard-test-vms-host-sink.timer.in" || return
    p0_capacity_delete_tree "$path" "$marker" || return
  done
}

p0_capacity_remove_subtree() {
  local path="${1:?P0 subtree path is required}"
  [ ! -e "$path" ] || p0_capacity_assert_root_marker || return
  case "$path" in
    "$P0_CAPACITY_STATE_ROOT"/*) ;;
    *) p0_capacity_die "unsafe P0 subtree cleanup $path"; return ;;
  esac
  p0_capacity_delete_tree "$path"
}

p0_capacity_remove_root_if_empty() {
  local entry marker="$P0_CAPACITY_STATE_ROOT/.subyard-p0-marker"
  if [ -e "$P0_CAPACITY_STATE_ROOT" ]; then
    p0_capacity_assert_root_marker || return
    # An unpublished private receipt is scratch below the still-marked root.
    # Its publication is atomic; a published receipt allows no extra children.
    entry="$(find "$P0_CAPACITY_STATE_ROOT" -mindepth 1 -maxdepth 1 ! -path "$marker" \
      ! \( -name '.subyard-p0-finalize.*' -type f -user "$(id -u)" -perm 0600 \) -print -quit)"
    [ -z "$entry" ] || return 0
  fi
  p0_capacity_delete_tree "$P0_CAPACITY_STATE_ROOT"
}

p0_capacity_reset_build_cache() {
  p0_capacity_prepare_root || return
  p0_capacity_remove_subtree "$P0_CAPACITY_BUILD_CACHE" || return
  install -d -m 0700 "$P0_CAPACITY_BUILD_CACHE"
  printf '%s\n' "$P0_CAPACITY_MARKER" > "$P0_CAPACITY_BUILD_CACHE/.subyard-p0-marker"
  chmod 0600 "$P0_CAPACITY_BUILD_CACHE/.subyard-p0-marker"
  export GOCACHE="$P0_CAPACITY_BUILD_CACHE"
  export GOMODCACHE="$P0_CAPACITY_MODULE_CACHE"
}

p0_capacity_use_build_cache() {
  p0_capacity_prepare_root || return
  if [ -e "$P0_CAPACITY_BUILD_CACHE.subyard-p0-cleanup" ] || [ -L "$P0_CAPACITY_BUILD_CACHE.subyard-p0-cleanup" ]; then
    p0_capacity_remove_subtree "$P0_CAPACITY_BUILD_CACHE" || return
  fi
  if [ ! -e "$P0_CAPACITY_BUILD_CACHE" ]; then
    install -d -m 0700 "$P0_CAPACITY_BUILD_CACHE"
    printf '%s\n' "$P0_CAPACITY_MARKER" > "$P0_CAPACITY_BUILD_CACHE/.subyard-p0-marker"
    chmod 0600 "$P0_CAPACITY_BUILD_CACHE/.subyard-p0-marker"
  fi
  [ "$(cat "$P0_CAPACITY_BUILD_CACHE/.subyard-p0-marker" 2>/dev/null)" = \
      "$P0_CAPACITY_MARKER" ] \
    || p0_capacity_die "refusing unmarked build cache $P0_CAPACITY_BUILD_CACHE" || return
  export GOCACHE="$P0_CAPACITY_BUILD_CACHE"
  export GOMODCACHE="$P0_CAPACITY_MODULE_CACHE"
}

p0_capacity_remove_build_cache() {
  p0_capacity_remove_subtree "$P0_CAPACITY_BUILD_CACHE" || return
  p0_capacity_remove_root_if_empty
}

p0_capacity_reclaim_go_module_cache() {
  local home_path module_path module_before module_after
  [ "${SUBYARD_E2E_VM:-}" = 1 ] \
    || p0_capacity_die 'Go module-cache reclaim is restricted to P0 VM1' || return
  home_path="$(realpath -e -- "$HOME")" \
    || p0_capacity_die 'cannot resolve P0 home for Go module-cache reclaim' || return
  module_path="$(realpath -m -- "$P0_CAPACITY_MODULE_CACHE")" \
    || p0_capacity_die 'cannot resolve the Go module cache' || return
  case "$module_path" in
    "$home_path"/*) ;;
    *) p0_capacity_die "refusing Go module cache outside P0 home: $module_path"; return ;;
  esac

  module_before="$(p0_capacity_cache_bytes "$P0_CAPACITY_MODULE_CACHE")"
  go clean -modcache
  module_after="$(p0_capacity_cache_bytes "$P0_CAPACITY_MODULE_CACHE")"
  printf '  [ ok ] reclaimed disposable Go module cache module=%s->%s\n' \
    "$module_before" "$module_after"
}

p0_capacity_reclaim_default_go_build_cache() {
  local cache="${P0_CAPACITY_DEFAULT_BUILD_CACHE:?}" canonical
  [ "${SUBYARD_E2E_VM:-}" = 1 ] \
    || p0_capacity_die 'default Go cache reclaim requires disposable VM1' || return
  canonical="$(realpath -m -- "$cache")" || return
  [ "$canonical" = "$cache" ] \
    || p0_capacity_die 'default Go cache has a symbolic-link ancestor' || return
  case "$cache" in
    "$HOME/.cache/go-build"|/srv/cache/go-build) ;;
    *) p0_capacity_die 'default Go cache is outside supported disposable locations'; return ;;
  esac
  [ -e "$cache" ] || return 0
  [ -d "$cache" ] && [ ! -L "$cache" ] \
    || p0_capacity_die 'default Go cache is not a plain directory' || return
  # Run outside the module and before -modcache, avoiding a toolchain download.
  (cd /; GOTOOLCHAIN=local GOCACHE="$cache" go clean -cache)
}

p0_capacity_cache_bytes() {
  local path="$1"
  if [ -e "$path" ]; then
    du -sx -B1 "$path" 2>/dev/null | awk '{print $1}'
  else
    printf '0\n'
  fi
}

p0_capacity_require_persistent_path() {
  local path="${1:?capacity path is required}" label="${2:-$1}" fstype source target
  local -a findmnt_command=(findmnt)
  if [ ! -e "$path" ]; then
    if command -v sudo >/dev/null 2>&1 && sudo -n test -e "$path" 2>/dev/null; then
      findmnt_command=(sudo -n findmnt)
    else
      p0_capacity_die "$label does not exist: $path"
      return
    fi
  fi
  read -r fstype source target < <(
    "${findmnt_command[@]}" -n -o FSTYPE,SOURCE,TARGET -T "$path"
  ) \
    || p0_capacity_die "cannot resolve filesystem for $label: $path" || return
  case "$fstype" in
    tmpfs | ramfs)
      p0_capacity_die "$label must not use $fstype: $path"
      return
      ;;
  esac
  printf '  [ ok ] capacity path %-18s fs=%s source=%s target=%s\n' \
    "$label" "$fstype" "$source" "$target"
}

p0_capacity_query_default_pool() {
  local output_name="${1:?output variable is required}"
  local query_timeout="${2:-${P0_E2E_INCUS_QUERY_TIMEOUT:-120}}"
  local query_state='' pool_rc query_attempt
  [[ "$query_timeout" =~ ^[1-9][0-9]*$ ]] \
    || p0_capacity_die 'Incus query timeout is invalid' || return
  for query_attempt in 1 2; do
    set +e
    query_state="$(timeout --foreground "$query_timeout" \
      incus storage show default --project default 2>/dev/null)"
    pool_rc=$?
    set -e
    case "$pool_rc" in
      0)
        printf -v "$output_name" '%s' "$query_state"
        return 0
        ;;
      124|137)
        if [ "$query_attempt" -lt 2 ]; then
          printf '  [ .. ] Incus default-pool query timed out; retrying cold activation\n'
          case "${SUBYARD_E2E_VM:-}" in
            1|2)
              timeout --foreground "$query_timeout" \
                sudo -n systemctl restart incus.service \
                || p0_capacity_die 'failed to restart the stuck Incus daemon' || return
              ;;
          esac
          continue
        fi
        p0_capacity_die "Incus default-pool query exceeded ${query_timeout}s twice"
        return
        ;;
      *) return 1 ;;
    esac
  done
}

p0_capacity_preflight() {
  local root_available inode_available tmp_size tmp_available pool_source='' stale=''
  local pool_state='' pool_rc query_timeout="${P0_E2E_INCUS_QUERY_TIMEOUT:-120}"

  p0_capacity_recover_stale_roots || return
  p0_capacity_reset_build_cache || return
  stale="$(find "$P0_CAPACITY_STATE_ROOT" -mindepth 1 -maxdepth 1 \
    ! -name .subyard-p0-marker ! -name go-build -print -quit)"
  [ -z "$stale" ] \
    || p0_capacity_die "stale P0 state remains before the lane: $stale" || return
  install -d -m 0755 "$P0_CAPACITY_MODULE_CACHE"
  p0_capacity_require_persistent_path "$P0_CAPACITY_STATE_ROOT" p0-state || return
  p0_capacity_require_persistent_path "$P0_CAPACITY_MODULE_CACHE" go-module-cache || return
  if [ -e "$P0_CAPACITY_PLATFORM_ROOT" ]; then
    p0_capacity_require_persistent_path "$P0_CAPACITY_PLATFORM_ROOT" platform-state || return
  else
    p0_capacity_require_persistent_path "$HOME/.cache" platform-parent || return
  fi
  if command -v incus >/dev/null 2>&1; then
    p0_capacity_query_default_pool pool_state "$query_timeout" || {
      pool_rc=$?
      [ "$pool_rc" = 1 ] || return "$pool_rc"
      pool_state=''
    }
  fi
  if [ -n "$pool_state" ]; then
    pool_source="$(sed -n 's/^  source: //p' <<<"$pool_state")"
    case "$pool_source" in
      /*) ;;
      *) p0_capacity_die "default Incus pool has an unsafe source: $pool_source"; return ;;
    esac
    p0_capacity_require_persistent_path "$pool_source" incus-default-pool || return
  fi

  root_available="$(df -B1 --output=avail "$P0_CAPACITY_STATE_ROOT" | awk 'NR==2 {print $1}')"
  inode_available="$(df --output=iavail "$P0_CAPACITY_STATE_ROOT" | awk 'NR==2 {print $1}')"
  tmp_size="$(df -B1 --output=size /tmp | awk 'NR==2 {print $1}')"
  tmp_available="$(df -B1 --output=avail /tmp | awk 'NR==2 {print $1}')"
  # The broker owns disk admission; these samples are diagnostics only.
  printf '  [ ok ] capacity sample root_available=%s inodes_available=%s tmp_size=%s tmp_available=%s\n' \
    "$root_available" "$inode_available" "$tmp_size" "$tmp_available"
  printf '  [ ok ] Go caches disposable_build=%s reusable_modules=%s default_build_before=%s\n' \
    "$(p0_capacity_cache_bytes "$P0_CAPACITY_BUILD_CACHE")" \
    "$(p0_capacity_cache_bytes "$P0_CAPACITY_MODULE_CACHE")" \
    "$(p0_capacity_cache_bytes "$P0_CAPACITY_DEFAULT_BUILD_CACHE")"
}
