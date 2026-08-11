#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TEST_CACHE_ROOT="${P0_CAPACITY_TEST_ROOT:-$HOME/.cache}"
install -d -m 0700 "$TEST_CACHE_ROOT"
TMP="$(mktemp -d "$TEST_CACHE_ROOT/subyard-p0-capacity-test.XXXXXX")"
EXTERNAL_TEST_ROOT=''
cleanup() {
  [ -z "$EXTERNAL_TEST_ROOT" ] \
    || [ ! -e "$EXTERNAL_TEST_ROOT" ] \
    || find "$EXTERNAL_TEST_ROOT" -depth -delete
  rm -rf "$TMP"
}
trap cleanup EXIT
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

export HOME="$TMP/home"
install -d -m 0700 "$HOME"
install -d -m 0700 "$TMP/bin"
PATH="$TMP/bin:$PATH"
export PATH
export P0_FAKE_TIMEOUT_LOG="$TMP/timeout.log"
export P0_FAKE_GO_LOG="$TMP/go.log"
export P0_FAKE_SUDO_LOG="$TMP/sudo.log"
printf '%s\n' \
  '#!/bin/bash' \
  'set -euo pipefail' \
  'case "$*" in' \
  '  "env GOCACHE") printf "%s/.cache/go-build\n" "$HOME" ;;' \
  '  "env GOMODCACHE") printf "%s/go/pkg/mod\n" "$HOME" ;;' \
  '  "clean -modcache")' \
  '    printf "%s\n" "$*" >> "$P0_FAKE_GO_LOG"' \
  '    [ ! -e "$P0_CAPACITY_MODULE_CACHE" ] || find "$P0_CAPACITY_MODULE_CACHE" -depth -delete' \
  '    ;;' \
  '  *) exit 2 ;;' \
  'esac' > "$TMP/bin/go"
chmod 0700 "$TMP/bin/go"
printf '%s\n' \
  '#!/bin/bash' \
  'set -euo pipefail' \
  'case "$*" in' \
  '  "-n apt-get clean"|"-n find /var/lib/apt/lists -xdev -type f -delete"|"-n systemctl restart incus.service")' \
  '    printf "%s\n" "$*" >> "$P0_FAKE_SUDO_LOG"' \
  '    ;;' \
  '  *) exit 2 ;;' \
  'esac' > "$TMP/bin/sudo"
chmod 0700 "$TMP/bin/sudo"
printf '%s\n' \
  '#!/bin/bash' \
  'set -euo pipefail' \
  '[ "${1:-}" = --foreground ] || exit 2' \
  'shift' \
  'printf "%s\n" "$*" >> "$P0_FAKE_TIMEOUT_LOG"' \
  'if [ "${P0_FAKE_TIMEOUT_FAIL_ONCE:-0}" = 1 ] && [ ! -e "$P0_FAKE_TIMEOUT_FAIL_MARKER" ]; then' \
  '  : > "$P0_FAKE_TIMEOUT_FAIL_MARKER"' \
  '  exit 124' \
  'fi' \
  'shift' \
  'exec "$@"' \
  > "$TMP/bin/timeout"
chmod 0700 "$TMP/bin/timeout"
printf '%s\n' \
  '#!/usr/bin/env bash' \
  '# Keep this host-free unit isolated from any retained real-host Incus daemon.' \
  'exit 1' > "$TMP/bin/incus"
chmod 0700 "$TMP/bin/incus"

# shellcheck source=dev/e2e/lib-p0-capacity.sh
. "$ROOT/dev/e2e/lib-p0-capacity.sh"

p0_capacity_init 123
P0_E2E_MIN_ROOT_AVAILABLE_BYTES=1 \
P0_E2E_MIN_AVAILABLE_INODES=1 \
P0_E2E_MIN_TMP_SIZE_BYTES=1 \
P0_E2E_MIN_TMP_AVAILABLE_BYTES=1 \
  p0_capacity_preflight >/dev/null
grep -Fxq '120 incus storage show default --project default' "$P0_FAKE_TIMEOUT_LOG" \
  || fail "P0 Incus cold-start query is not bounded at 120 seconds"
[ "$GOCACHE" = "$HOME/.cache/subyard-p0-123/go-build" ] \
  || fail "P0 build cache is not allocation-scoped"
[ "$GOMODCACHE" = "$(env -u GOMODCACHE go env GOMODCACHE)" ] \
  || fail "P0 module cache is not the reusable Go cache"
[ "$(cat "$GOCACHE/.subyard-p0-marker")" = subyard-p0-123 ] \
  || fail "P0 build cache is not marker-owned"

install -d -m 0755 "$P0_CAPACITY_MODULE_CACHE/cache/download"
printf 'module\n' > "$P0_CAPACITY_MODULE_CACHE/cache/download/fixture"
: > "$P0_FAKE_GO_LOG"
: > "$P0_FAKE_SUDO_LOG"
SUBYARD_E2E_VM=1 p0_capacity_reclaim_dependency_caches >/dev/null
[ ! -e "$P0_CAPACITY_MODULE_CACHE" ] \
  || fail "P0 dependency reclaim left the reusable module cache behind"
grep -Fxq 'clean -modcache' "$P0_FAKE_GO_LOG" \
  || fail "P0 dependency reclaim did not use the Go module-cache cleanup"
grep -Fxq -- '-n apt-get clean' "$P0_FAKE_SUDO_LOG" \
  && grep -Fxq -- '-n find /var/lib/apt/lists -xdev -type f -delete' \
    "$P0_FAKE_SUDO_LOG" \
  || fail "P0 dependency reclaim did not remove disposable APT caches"

: > "$P0_FAKE_TIMEOUT_LOG"
export P0_FAKE_TIMEOUT_FAIL_ONCE=1
export P0_FAKE_TIMEOUT_FAIL_MARKER="$TMP/timeout-failed-once"
p0_capacity_init 135
if ! SUBYARD_E2E_VM=2 \
  P0_E2E_MIN_ROOT_AVAILABLE_BYTES=1 \
  P0_E2E_MIN_AVAILABLE_INODES=1 \
  P0_E2E_MIN_TMP_SIZE_BYTES=1 \
  P0_E2E_MIN_TMP_AVAILABLE_BYTES=1 \
  p0_capacity_preflight >/dev/null; then
  fail "P0 Incus cold-start query did not recover after one timeout"
fi
[ "$(grep -Fc '120 incus storage show default --project default' "$P0_FAKE_TIMEOUT_LOG")" = 2 ] \
  || fail "P0 Incus cold-start query did not make exactly one recovery retry"
grep -Fxq -- '-n systemctl restart incus.service' "$P0_FAKE_SUDO_LOG" \
  || fail "P0 Incus cold-start retry did not restart the stuck daemon"
unset P0_FAKE_TIMEOUT_FAIL_ONCE P0_FAKE_TIMEOUT_FAIL_MARKER
p0_capacity_remove_build_cache
p0_capacity_init 123
p0_capacity_reset_build_cache

subtree="$P0_CAPACITY_STATE_ROOT/fixture"
p0_capacity_prepare_subtree "$subtree"
printf 'payload\n' > "$subtree/data"
p0_capacity_remove_subtree "$subtree"
[ ! -e "$subtree" ] || fail "marker-owned subtree survived cleanup"

readonly_subtree="$P0_CAPACITY_STATE_ROOT/readonly-fixture"
p0_capacity_prepare_subtree "$readonly_subtree"
install -d -m 0755 "$readonly_subtree/modules"
printf 'readonly\n' > "$readonly_subtree/modules/data"
chmod 0444 "$readonly_subtree/modules/data"
chmod 0555 "$readonly_subtree/modules"
p0_capacity_remove_subtree "$readonly_subtree"
[ ! -e "$readonly_subtree" ] || fail "read-only Go-style cache survived cleanup"

p0_capacity_remove_build_cache
[ ! -e "$P0_CAPACITY_STATE_ROOT" ] || fail "empty marker-owned root survived cleanup"

stale_root="$HOME/.cache/subyard-p0-122"
install -d -m 0700 "$stale_root/cache"
printf '%s\n' subyard-p0-122 > "$stale_root/.subyard-p0-marker"
chmod 0555 "$stale_root/cache"
p0_capacity_recover_stale_roots >/dev/null
[ ! -e "$stale_root" ] || fail "marker-owned stale allocation cache survived recovery"

install -d -m 0700 "$P0_CAPACITY_STATE_ROOT"
printf 'foreign\n' > "$P0_CAPACITY_STATE_ROOT/data"
if (p0_capacity_prepare_root) >/dev/null 2>&1; then
  fail "non-empty unmarked P0 state was accepted"
fi
find "$P0_CAPACITY_STATE_ROOT" -depth -delete

if (p0_capacity_require_persistent_path /dev/shm fixture-tmpfs) >/dev/null 2>&1; then
  fail "tmpfs product state was accepted"
fi

export P0_FAKE_INCUS_STATE="$TMP/incus-state"
export P0_FAKE_INCUS_LOG="$TMP/incus-log"
export P0_FAKE_INCUS_PROJECT="$TMP/incus-project"
export P0_FAKE_INCUS_MARKER=
export P0_FAKE_INCUS_INSTANCES=yard-e2e-yard
export P0_FAKE_INCUS_VOLUMES=$'container,yard-e2e-yard\ncustom,yard-srv-e2e-yard'
printf '%s\n' \
  '#!/usr/bin/env bash' \
  'set -euo pipefail' \
  'source_path() { sed -n "s/^  source: //p" "$P0_FAKE_INCUS_STATE"; }' \
  'case "$*" in' \
  '  "storage show default --project default")' \
  '    [ -r "$P0_FAKE_INCUS_STATE" ] || exit 1' \
  '    cat "$P0_FAKE_INCUS_STATE"' \
  '    ;;' \
  '  "storage delete default --project default")' \
  '    printf "%s\\n" "$*" >> "$P0_FAKE_INCUS_LOG"' \
  '    find "$P0_FAKE_INCUS_STATE" -delete' \
  '    ;;' \
  '  "project show subyard-e2e-yard") [ -e "$P0_FAKE_INCUS_PROJECT" ] ;;' \
  '  "project show subyard-test-yard") exit 1 ;;' \
  '  "project get subyard-e2e-yard user.subyard.p0-image-cache")' \
  '    printf "%s\\n" "$P0_FAKE_INCUS_MARKER"' \
  '    ;;' \
  '  "list --project subyard-e2e-yard --format csv -c n")' \
  '    printf "%s\\n" "$P0_FAKE_INCUS_INSTANCES"' \
  '    ;;' \
  '  "storage volume list default --project subyard-e2e-yard --format csv -c t,n")' \
  '    printf "%s\\n" "$P0_FAKE_INCUS_VOLUMES"' \
  '    ;;' \
  '  "config show yard-e2e-yard --project subyard-e2e-yard") exit 0 ;;' \
  '  "delete yard-e2e-yard --project subyard-e2e-yard --force")' \
  '    printf "%s\\n" "$*" >> "$P0_FAKE_INCUS_LOG"' \
  '    ;;' \
  '  "storage volume show default yard-srv-e2e-yard --project subyard-e2e-yard") exit 0 ;;' \
  '  "storage volume delete default yard-srv-e2e-yard --project subyard-e2e-yard")' \
  '    printf "%s\\n" "$*" >> "$P0_FAKE_INCUS_LOG"' \
  '    ;;' \
  '  "project delete subyard-e2e-yard")' \
  '    printf "%s\\n" "$*" >> "$P0_FAKE_INCUS_LOG"' \
  '    source="$(source_path)"' \
  '    printf "config:\\n  source: %s\\nused_by:\\n- /1.0/profiles/default\\nstatus: Unavailable\\n" "$source" > "$P0_FAKE_INCUS_STATE"' \
  '    find "$P0_FAKE_INCUS_PROJECT" -delete' \
  '    ;;' \
  '  "profile device get default root pool --project default") printf "%s\\n" default ;;' \
  '  "profile device remove default root --project default")' \
  '    printf "%s\\n" "$*" >> "$P0_FAKE_INCUS_LOG"' \
  '    source="$(source_path)"' \
  '    printf "config:\\n  source: %s\\nused_by: []\\nstatus: Unavailable\\n" "$source" > "$P0_FAKE_INCUS_STATE"' \
  '    ;;' \
  '  *) exit 1 ;;' \
  'esac' > "$TMP/bin/incus"
chmod 0700 "$TMP/bin/incus"

run_recovery_preflight() {
  local token="$1" source="$2"
  printf 'config:\n  source: %s\nstatus: Unavailable\nused_by: []\n' \
    "$source" > "$P0_FAKE_INCUS_STATE"
  : > "$P0_FAKE_INCUS_LOG"
  SUBYARD_E2E_VM=1 \
  P0_E2E_MIN_ROOT_AVAILABLE_BYTES=1 \
  P0_E2E_MIN_AVAILABLE_INODES=1 \
  P0_E2E_MIN_TMP_SIZE_BYTES=1 \
  P0_E2E_MIN_TMP_AVAILABLE_BYTES=1 \
    bash "$ROOT/dev/e2e/p0-guest.sh" capacity-preflight "$token" >/dev/null
  grep -Fxq 'storage delete default --project default' "$P0_FAKE_INCUS_LOG" \
    || fail "stale test pool at $source was not deleted"
  [ ! -e "$P0_FAKE_INCUS_STATE" ] || fail "stale test pool state survived recovery"
  find "$HOME/.cache/subyard-p0-$token" -depth -delete
}

run_recovery_preflight 124 /var/tmp/subyard-nested-teardown.fixture/storage

stale_pool_root="$HOME/.cache/subyard-p0-121"
install -d -m 0700 "$stale_pool_root"
printf '%s\n' subyard-p0-121 > "$stale_pool_root/.subyard-p0-marker"
run_recovery_preflight 125 "$stale_pool_root/owner/subyard/incus/storage"
[ ! -e "$stale_pool_root" ] || fail "recovered stale P0 root survived preflight"

existing_source_root="/var/tmp/subyard-nested-teardown.existing-$$"
EXTERNAL_TEST_ROOT="$existing_source_root"
existing_source="$existing_source_root/storage"
install -d -m 0700 "$existing_source"
printf 'config:\n  source: %s\nstatus: Unavailable\nused_by: []\n' \
  "$existing_source" > "$P0_FAKE_INCUS_STATE"
: > "$P0_FAKE_INCUS_LOG"
SUBYARD_E2E_VM=1 \
P0_E2E_MIN_ROOT_AVAILABLE_BYTES=1 \
P0_E2E_MIN_AVAILABLE_INODES=1 \
P0_E2E_MIN_TMP_SIZE_BYTES=1 \
P0_E2E_MIN_TMP_AVAILABLE_BYTES=1 \
  bash "$ROOT/dev/e2e/p0-guest.sh" capacity-preflight 129 >/dev/null
[ ! -s "$P0_FAKE_INCUS_LOG" ] || fail "pool with an existing source caused cleanup mutations"
[ -e "$P0_FAKE_INCUS_STATE" ] || fail "pool with an existing source was deleted"
find "$existing_source_root" -depth -delete
EXTERNAL_TEST_ROOT=''
find "$HOME/.cache/subyard-p0-129" -depth -delete

write_active_pool_state() {
  local root="$1" status="$2"
  printf '%s\n' \
  'config:' \
  "  source: $root/owner/subyard/incus/storage" \
  'used_by:' \
  '- /1.0/instances/yard-e2e-yard?project=subyard-e2e-yard' \
  '- /1.0/profiles/default' \
  '- /1.0/profiles/default?project=subyard-e2e-yard' \
  '- /1.0/storage-pools/default/volumes/custom/yard-srv-e2e-yard?project=subyard-e2e-yard' \
    "status: $status" > "$P0_FAKE_INCUS_STATE"
}

assert_unsafe_active_residue() {
  local token="$1" root="$2" expected_message="$3" before
  write_active_pool_state "$root" Unavailable
  before="$(cat "$P0_FAKE_INCUS_STATE")"
  : > "$P0_FAKE_INCUS_LOG"
  : > "$P0_FAKE_INCUS_PROJECT"
  if SUBYARD_E2E_VM=1 \
    P0_E2E_MIN_ROOT_AVAILABLE_BYTES=1 \
    P0_E2E_MIN_AVAILABLE_INODES=1 \
    P0_E2E_MIN_TMP_SIZE_BYTES=1 \
    P0_E2E_MIN_TMP_AVAILABLE_BYTES=1 \
      bash "$ROOT/dev/e2e/p0-guest.sh" capacity-preflight "$token" >/dev/null 2>&1; then
    fail "$expected_message was accepted"
  fi
  [ ! -s "$P0_FAKE_INCUS_LOG" ] || fail "$expected_message caused cleanup mutations"
  [ -e "$P0_FAKE_INCUS_PROJECT" ] || fail "$expected_message caused project deletion"
  [ "$(cat "$P0_FAKE_INCUS_STATE")" = "$before" ] \
    || fail "$expected_message changed pool state"
  find "$P0_FAKE_INCUS_PROJECT" "$P0_FAKE_INCUS_STATE" -delete
  [ ! -e "$HOME/.cache/subyard-p0-$token" ] \
    || find "$HOME/.cache/subyard-p0-$token" -depth -delete
}

wrong_root_marker="$HOME/.cache/subyard-p0-118"
install -d -m 0700 "$wrong_root_marker"
printf '%s\n' foreign-marker > "$wrong_root_marker/.subyard-p0-marker"
export P0_FAKE_INCUS_MARKER=subyard-p0-118
assert_unsafe_active_residue 130 "$wrong_root_marker" 'pool with a wrong root marker'
find "$wrong_root_marker" -depth -delete

foreign_project_root="$HOME/.cache/subyard-p0-117"
install -d -m 0700 "$foreign_project_root"
printf '%s\n' subyard-p0-117 > "$foreign_project_root/.subyard-p0-marker"
export P0_FAKE_INCUS_MARKER=foreign-marker
assert_unsafe_active_residue 131 "$foreign_project_root" 'pool with a foreign project marker'
find "$foreign_project_root" -depth -delete

unexpected_instance_root="$HOME/.cache/subyard-p0-116"
install -d -m 0700 "$unexpected_instance_root"
printf '%s\n' subyard-p0-116 > "$unexpected_instance_root/.subyard-p0-marker"
export P0_FAKE_INCUS_MARKER=subyard-p0-116
export P0_FAKE_INCUS_INSTANCES=$'yard-e2e-yard\nforeign-instance'
assert_unsafe_active_residue 132 "$unexpected_instance_root" 'pool with an unexpected instance'
export P0_FAKE_INCUS_INSTANCES=yard-e2e-yard
find "$unexpected_instance_root" -depth -delete

unexpected_volume_root="$HOME/.cache/subyard-p0-115"
install -d -m 0700 "$unexpected_volume_root"
printf '%s\n' subyard-p0-115 > "$unexpected_volume_root/.subyard-p0-marker"
export P0_FAKE_INCUS_MARKER=subyard-p0-115
export P0_FAKE_INCUS_VOLUMES=$'container,yard-e2e-yard\ncustom,yard-srv-e2e-yard\ncustom,foreign-volume'
assert_unsafe_active_residue 133 "$unexpected_volume_root" 'pool with an unexpected storage volume'
export P0_FAKE_INCUS_VOLUMES=$'container,yard-e2e-yard\ncustom,yard-srv-e2e-yard'
find "$unexpected_volume_root" -depth -delete

active_stale_root="$HOME/.cache/subyard-p0-120"
install -d -m 0700 "$active_stale_root"
printf '%s\n' subyard-p0-120 > "$active_stale_root/.subyard-p0-marker"
export P0_FAKE_INCUS_MARKER=subyard-p0-120
: > "$P0_FAKE_INCUS_PROJECT"
write_active_pool_state "$active_stale_root" Unavailable
: > "$P0_FAKE_INCUS_LOG"
if ! SUBYARD_E2E_VM=1 \
  P0_E2E_MIN_ROOT_AVAILABLE_BYTES=1 \
  P0_E2E_MIN_AVAILABLE_INODES=1 \
  P0_E2E_MIN_TMP_SIZE_BYTES=1 \
  P0_E2E_MIN_TMP_AVAILABLE_BYTES=1 \
    bash "$ROOT/dev/e2e/p0-guest.sh" capacity-preflight 127 >/dev/null; then
  fail "marker-owned active P0 residue did not converge"
fi
grep -Fxq 'storage delete default --project default' "$P0_FAKE_INCUS_LOG" \
  || fail "marker-owned active P0 pool was not deleted"
grep -Fxq 'delete yard-e2e-yard --project subyard-e2e-yard --force' "$P0_FAKE_INCUS_LOG" \
  || fail "marker-owned stale P0 instance was not deleted"
grep -Fxq 'profile device remove default root --project default' "$P0_FAKE_INCUS_LOG" \
  || fail "stale P0 default profile still references the pool"
[ ! -e "$active_stale_root" ] || fail "marker-owned active P0 root survived recovery"
find "$HOME/.cache/subyard-p0-127" -depth -delete

online_stale_root="$HOME/.cache/subyard-p0-119"
install -d -m 0700 "$online_stale_root"
printf '%s\n' subyard-p0-119 > "$online_stale_root/.subyard-p0-marker"
export P0_FAKE_INCUS_MARKER=subyard-p0-119
: > "$P0_FAKE_INCUS_PROJECT"
write_active_pool_state "$online_stale_root" Created
: > "$P0_FAKE_INCUS_LOG"
if SUBYARD_E2E_VM=1 \
  P0_E2E_MIN_ROOT_AVAILABLE_BYTES=1 \
  P0_E2E_MIN_AVAILABLE_INODES=1 \
  P0_E2E_MIN_TMP_SIZE_BYTES=1 \
  P0_E2E_MIN_TMP_AVAILABLE_BYTES=1 \
    bash "$ROOT/dev/e2e/p0-guest.sh" capacity-preflight 128 >/dev/null 2>&1; then
  fail "available P0 pool was recovered as stale"
fi
[ ! -s "$P0_FAKE_INCUS_LOG" ] || fail "available P0 pool caused cleanup mutations"
[ -e "$P0_FAKE_INCUS_PROJECT" ] || fail "available P0 project was deleted"
find "$online_stale_root" -depth -delete
[ ! -e "$HOME/.cache/subyard-p0-128" ] \
  || find "$HOME/.cache/subyard-p0-128" -depth -delete
find "$P0_FAKE_INCUS_PROJECT" "$P0_FAKE_INCUS_STATE" -delete
export P0_FAKE_INCUS_MARKER=

foreign_source="$HOME/foreign/storage"
printf 'config:\n  source: %s\nstatus: Unavailable\nused_by: []\n' \
  "$foreign_source" > "$P0_FAKE_INCUS_STATE"
: > "$P0_FAKE_INCUS_LOG"
if SUBYARD_E2E_VM=1 \
  P0_E2E_MIN_ROOT_AVAILABLE_BYTES=1 \
  P0_E2E_MIN_AVAILABLE_INODES=1 \
  P0_E2E_MIN_TMP_SIZE_BYTES=1 \
  P0_E2E_MIN_TMP_AVAILABLE_BYTES=1 \
    bash "$ROOT/dev/e2e/p0-guest.sh" capacity-preflight 126 >/dev/null 2>&1; then
  fail "foreign unavailable pool passed capacity preflight"
fi
[ ! -s "$P0_FAKE_INCUS_LOG" ] || fail "foreign unavailable pool was deleted"
find "$HOME/.cache/subyard-p0-126" -depth -delete

nested_bin="$TMP/nested-bin"
nested_probe="$TMP/nested-cache-probe"
install -d -m 0700 "$nested_bin"
printf '%s\n' \
  '#!/bin/bash' \
  'set -euo pipefail' \
  'case "$*" in' \
  '  info) ;;' \
  '  --version) printf "6.0.6\n" ;;' \
  '  "storage show default --project default") ;;' \
  '  "network show incusbr0 --project default") ;;' \
  '  *) exit 2 ;;' \
  'esac' \
  > "$nested_bin/incus"
chmod 0700 "$nested_bin/incus"
printf '%s\n' \
  '#!/bin/bash' \
  'set -euo pipefail' \
  '[ "$*" = dev/e2e/nested-teardown-data-boundary.sh ] || exit 2' \
  'expected="$HOME/.cache/subyard-p0-134/go-build"' \
  '[ "${GOCACHE:-}" = "$expected" ] || exit 3' \
  '[ "${GOMODCACHE:-}" = "$HOME/go/pkg/mod" ] || exit 4' \
  '[ "$(cat "$GOCACHE/.subyard-p0-marker" 2>/dev/null)" = subyard-p0-134 ] || exit 5' \
  'printf "ok\n" > "$P0_NESTED_CACHE_PROBE"' \
  > "$nested_bin/bash"
chmod 0700 "$nested_bin/bash"
if ! env -u GOCACHE -u GOMODCACHE \
  PATH="$nested_bin:$PATH" P0_NESTED_CACHE_PROBE="$nested_probe" SUBYARD_E2E_VM=2 \
  /bin/bash "$ROOT/dev/e2e/p0-guest.sh" nested-teardown 134 >/dev/null; then
  fail "nested teardown did not use its allocation-scoped Go cache"
fi
[ "$(cat "$nested_probe" 2>/dev/null)" = ok ] \
  || fail "nested teardown cache probe did not run"
find "$HOME/.cache/subyard-p0-134" -depth -delete

printf 'ok: P0 capacity layout is persistent and marker-guarded\n'
