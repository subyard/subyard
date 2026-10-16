#!/usr/bin/env bash
# Cooperative cache contract for provisioned OpenClaw tools and participating L2 images.
set -euo pipefail
LOCK="${SY_CACHE_LOCK:-/srv/cache/.sy-cache.lock}"
die() { printf 'sy-cache: %s\n' "$*" >&2; exit 1; }
key() {
  local kind="${1:?}" version="${2:?}" abi="${3:-}"
  [[ "$version" =~ ^[a-zA-Z0-9._-]+$ ]] || die 'invalid cache version'
  [[ "$abi" =~ ^[a-zA-Z0-9._-]*$ ]] || die 'invalid cache ABI'
  printf '%s/%s-%s/%s%s\n' "$kind" "$(uname -s | tr '[:upper:]' '[:lower:]')" "$(uname -m)" "$version" "${abi:+-$abi}"
}
run_locked() {
  local mode="$1"; shift
  [ "$#" -gt 0 ] || die 'missing command'
  [ -f "$LOCK" ] && [ ! -L "$LOCK" ] || die 'missing or unsafe cache lock; provision the profile'
  [ "$(stat -c '%u:%g' "$LOCK")" = "$(id -u):$(id -g)" ] || die 'cache lock UID/GID mismatch'
  if [ -n "${_SY_CACHE_FD:-}" ]; then
    [[ "$_SY_CACHE_FD" =~ ^[0-9]+$ ]] || die 'invalid inherited cache lock'
    if [ "$(stat -Lc '%d:%i' "/proc/self/fd/$_SY_CACHE_FD" 2>/dev/null)" = "$(stat -c '%d:%i' "$LOCK")" ]; then
      local expected_lock
      case "${_SY_CACHE_MODE:-}" in
        shared) expected_lock=READ ;;
        exclusive) expected_lock=WRITE ;;
        *) die 'invalid inherited cache lock mode' ;;
      esac
      # fdinfo observes the held flock without converting or downgrading it. An open FD and
      # environment marker alone do not establish ownership of any lock.
      awk -v expected="$expected_lock" '
        $1 == "lock:" && $3 == "FLOCK" && $4 == "ADVISORY" && $5 == expected && $8 == "0" && $9 == "EOF" { held=1 }
        END { exit !held }
      ' "/proc/self/fdinfo/$_SY_CACHE_FD" || die 'inherited cache lock is not held in the declared mode'
      case "${_SY_CACHE_MODE:-}:$mode" in
        exclusive:*|shared:shared) exec "$@" ;;
        shared:exclusive) die 'cannot mutate caches from a shared operation; run maintenance separately' ;;
        *) die 'invalid inherited cache lock mode' ;;
      esac
    elif [ "${_SY_CACHE_MODE:-}:$mode" != shared:shared ]; then
      die 'invalid inherited cache lock'
    fi
    # Node/Python child launchers may close extra FDs while retaining env. Reacquire a SHARED
    # lock in that case; never trust the marker alone or attempt an unsafe exclusive upgrade.
  fi
  local cache_fd
  exec {cache_fd}<"$LOCK"
  [ "$(stat -Lc '%d:%i' "/proc/self/fd/$cache_fd")" = "$(stat -c '%d:%i' "$LOCK")" ] || die 'cache lock changed'
  case "$mode" in
    shared) flock -s -w "${SY_CACHE_WAIT_SECONDS:-600}" "$cache_fd" ;;
    exclusive) flock -x -w "${SY_CACHE_WAIT_SECONDS:-600}" "$cache_fd" ;;
    *) die 'invalid lock mode' ;;
  esac
  export _SY_CACHE_FD="$cache_fd" _SY_CACHE_MODE="$mode"
  exec "$@"
}
sub="${1:-}"; [ "$#" -eq 0 ] || shift
case "$sub" in
  key) key "$@" ;;
  shared|exclusive|lock)
    [ "${1:-}" != -- ] || shift
    [ "$sub" != lock ] || sub=exclusive
    run_locked "$sub" "$@" ;;
  prune) die 'pnpm pruning can empty a copied shared store; use sy-cache exclusive -- pnpm store prune only for intentional maintenance' ;;
  clean) run_locked exclusive npm cache clean --force ;;
  purge) run_locked exclusive /opt/venv/bin/python -m pip cache purge ;;
  all) die 'select explicit maintenance: clean, purge, or exclusive -- CMD' ;;
  browser)
    version="${1:?Playwright version required}"; shift
    [ "${1:-}" != -- ] || shift
    PLAYWRIGHT_BROWSERS_PATH="${SY_CACHE_BROWSER_ROOT:-/srv/cache/playwright}/$(key browser "$version")"
    export PLAYWRIGHT_BROWSERS_PATH
    export PLAYWRIGHT_SKIP_BROWSER_GC=1
    run_locked exclusive "$@" ;;
  -h|--help|help)
    printf '%s\n' 'sy-cache shared|exclusive|lock -- CMD...' 'sy-cache clean|purge|prune|all' \
      'sy-cache browser PLAYWRIGHT_VERSION -- CMD... (exclusive browser install; GC disabled)' \
      'sy-cache key KIND VERSION [ABI]' ;;
  *) die 'usage: sy-cache shared|exclusive -- CMD; clean; purge; browser VERSION -- CMD' ;;
esac
