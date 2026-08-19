#!/usr/bin/env bash
# Shared storage layout, capacity checks and guarded cleanup for the two-VM P0 lane.

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
  [ "$(cat "$P0_CAPACITY_STATE_ROOT/.subyard-p0-marker" 2>/dev/null)" = \
      "$P0_CAPACITY_MARKER" ] \
    || p0_capacity_die "refusing unmarked state root $P0_CAPACITY_STATE_ROOT"
}

p0_capacity_prepare_root() {
  local marker="$P0_CAPACITY_STATE_ROOT/.subyard-p0-marker"
  install -d -m 0711 "$HOME/.cache"
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

p0_capacity_delete_tree() {
  local path="${1:?tree path is required}" owner
  owner="$(id -u)"
  if find "$path" -xdev ! -user "$owner" -print -quit 2>/dev/null | grep -q .; then
    sudo -n find "$path" -xdev -type d -exec chmod u+rwx {} +
    sudo -n find "$path" -depth -delete
  else
    find "$path" -xdev -type d -exec chmod u+rwx {} +
    find "$path" -depth -delete
  fi
}

p0_capacity_recover_stale_roots() {
  local path name token marker
  for path in "$HOME"/.cache/subyard-p0-*; do
    [ -e "$path" ] || continue
    [ "$path" != "$P0_CAPACITY_STATE_ROOT" ] || continue
    [ -d "$path" ] && [ ! -L "$path" ] \
      || p0_capacity_die "refusing stale non-directory P0 state $path" || return
    name="${path##*/}"
    token="${name#subyard-p0-}"
    [[ "$token" =~ ^[0-9]+$ ]] \
      || p0_capacity_die "refusing stale P0 state with invalid token $path" || return
    marker="subyard-p0-$token"
    [ "$(cat "$path/.subyard-p0-marker" 2>/dev/null)" = "$marker" ] \
      || p0_capacity_die "refusing unmarked stale P0 state $path" || return
    if pgrep -u "$(id -u)" -f "$path" >/dev/null 2>&1; then
      p0_capacity_die "stale P0 state still has an active process: $path"
      return
    fi
    printf '  [ .. ] recovering marker-owned stale P0 state %s\n' "$name"
    p0_capacity_delete_tree "$path" || return
  done
}

p0_capacity_remove_subtree() {
  local path="${1:?P0 subtree path is required}"
  [ ! -e "$path" ] || p0_capacity_assert_root_marker || return
  case "$path" in
    "$P0_CAPACITY_STATE_ROOT"/*) ;;
    *) p0_capacity_die "unsafe P0 subtree cleanup $path"; return ;;
  esac
  [ ! -e "$path" ] \
    || [ "$(cat "$path/.subyard-p0-marker" 2>/dev/null)" = "$P0_CAPACITY_MARKER" ] \
    || p0_capacity_die "refusing unmarked P0 subtree cleanup $path" || return
  [ ! -e "$path" ] || p0_capacity_delete_tree "$path"
}

p0_capacity_remove_root_if_empty() {
  local entry marker="$P0_CAPACITY_STATE_ROOT/.subyard-p0-marker"
  [ -e "$P0_CAPACITY_STATE_ROOT" ] || return 0
  p0_capacity_assert_root_marker || return
  entry="$(find "$P0_CAPACITY_STATE_ROOT" -mindepth 1 ! -path "$marker" -print -quit)"
  [ -z "$entry" ] || return 0
  find "$marker" -delete
  rmdir "$P0_CAPACITY_STATE_ROOT"
}

p0_capacity_reset_build_cache() {
  p0_capacity_prepare_root || return
  if [ -e "$P0_CAPACITY_BUILD_CACHE" ]; then
    [ "$(cat "$P0_CAPACITY_BUILD_CACHE/.subyard-p0-marker" 2>/dev/null)" = \
        "$P0_CAPACITY_MARKER" ] \
      || p0_capacity_die "refusing unmarked build cache $P0_CAPACITY_BUILD_CACHE" || return
    find "$P0_CAPACITY_BUILD_CACHE" -depth -delete
  fi
  install -d -m 0700 "$P0_CAPACITY_BUILD_CACHE"
  printf '%s\n' "$P0_CAPACITY_MARKER" > "$P0_CAPACITY_BUILD_CACHE/.subyard-p0-marker"
  chmod 0600 "$P0_CAPACITY_BUILD_CACHE/.subyard-p0-marker"
  export GOCACHE="$P0_CAPACITY_BUILD_CACHE"
  export GOMODCACHE="$P0_CAPACITY_MODULE_CACHE"
}

p0_capacity_use_build_cache() {
  p0_capacity_prepare_root || return
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
  [ -e "$P0_CAPACITY_BUILD_CACHE" ] || return 0
  p0_capacity_assert_root_marker || return
  [ "$(cat "$P0_CAPACITY_BUILD_CACHE/.subyard-p0-marker" 2>/dev/null)" = \
      "$P0_CAPACITY_MARKER" ] \
    || p0_capacity_die "refusing unmarked build cache cleanup $P0_CAPACITY_BUILD_CACHE" || return
  find "$P0_CAPACITY_BUILD_CACHE" -depth -delete
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

p0_capacity_preflight() {
  local root_available inode_available tmp_size tmp_available pool_source='' stale=''
  local pool_state='' pool_rc query_attempt query_timeout="${P0_E2E_INCUS_QUERY_TIMEOUT:-120}"
  local min_root="${P0_E2E_MIN_ROOT_AVAILABLE_BYTES:-3221225472}"
  local min_inodes="${P0_E2E_MIN_AVAILABLE_INODES:-100000}"
  local min_tmp_size="${P0_E2E_MIN_TMP_SIZE_BYTES:-536870912}"
  local min_tmp_available="${P0_E2E_MIN_TMP_AVAILABLE_BYTES:-268435456}"

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
  [[ "$query_timeout" =~ ^[1-9][0-9]*$ ]] \
    || p0_capacity_die 'Incus query timeout is invalid' || return
  if command -v incus >/dev/null 2>&1; then
    for query_attempt in 1 2; do
      set +e
      pool_state="$(timeout --foreground "$query_timeout" \
        incus storage show default --project default 2>/dev/null)"
      pool_rc=$?
      set -e
      case "$pool_rc" in
        0) break ;;
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
        *) pool_state=''; break ;;
      esac
    done
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
  [[ "$root_available" =~ ^[0-9]+$ ]] && [ "$root_available" -ge "$min_root" ] \
    || p0_capacity_die "root filesystem needs at least $min_root available bytes; have ${root_available:-unknown}" \
    || return
  [[ "$inode_available" =~ ^[0-9]+$ ]] && [ "$inode_available" -ge "$min_inodes" ] \
    || p0_capacity_die "root filesystem needs at least $min_inodes available inodes; have ${inode_available:-unknown}" \
    || return
  [[ "$tmp_size" =~ ^[0-9]+$ ]] && [ "$tmp_size" -ge "$min_tmp_size" ] \
    || p0_capacity_die "/tmp needs at least $min_tmp_size total bytes; have ${tmp_size:-unknown}" \
    || return
  [[ "$tmp_available" =~ ^[0-9]+$ ]] && [ "$tmp_available" -ge "$min_tmp_available" ] \
    || p0_capacity_die "/tmp needs at least $min_tmp_available available bytes; have ${tmp_available:-unknown}" \
    || return

  printf '  [ ok ] capacity reserve root_available=%s inodes_available=%s tmp_size=%s tmp_available=%s\n' \
    "$root_available" "$inode_available" "$tmp_size" "$tmp_available"
  printf '  [ ok ] Go caches disposable_build=%s reusable_modules=%s default_build_before=%s\n' \
    "$(p0_capacity_cache_bytes "$P0_CAPACITY_BUILD_CACHE")" \
    "$(p0_capacity_cache_bytes "$P0_CAPACITY_MODULE_CACHE")" \
    "$(p0_capacity_cache_bytes "$P0_CAPACITY_DEFAULT_BUILD_CACHE")"
}
