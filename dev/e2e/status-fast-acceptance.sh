#!/usr/bin/env bash
# Real-Incus space/cache acceptance; selector and freshness matrices run locally.
# Build as dev, then run as root on an allocated VM; no operator group changes.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
RUN_ID="${SUBYARD_E2E_RUN_ID:?run through dev/agent-e2e.sh}"
FIXTURE="/tmp/subyard-status-$RUN_ID"
HOME_DIR="$FIXTURE/home"
CONFIG_HOME="$FIXTURE/config"
DATA_HOME="$FIXTURE/data"
DEFAULT_PROJECT=subyard
NAMED_PROJECT=subyard-demo
DEFAULT_INSTANCE=yard
NAMED_INSTANCE=yard-demo
STORAGE_SOURCE=/srv/incus-e2e/storage
STORAGE_ALIAS=/var/lib/incus/storage-pools/default

fail() {
  printf 'status-fast-acceptance: %s\n' "$*" >&2
  exit 1
}

[ "${SUBYARD_E2E_VM:?run through dev/agent-e2e.sh}" = 1 ] || fail 'requires VM1'
[ "$(id -u)" = 0 ] || fail 'run as root inside the allocated VM after dev/build-engine.sh'
command -v incus >/dev/null || fail 'incus is required'
[ -x "$ROOT/.build/yard" ] || fail 'build the current candidate before running the fixture'
incus() { /usr/bin/incus "$@" </dev/null; }
incus info >/dev/null

cleanup() {
  if incus exec "$DEFAULT_INSTANCE" --project "$DEFAULT_PROJECT" -- sh -eu -c '
    marker=$1/.subyard-status-storage-fixture
    [ -f "$marker" ] && [ "$(cat "$marker")" = "$2" ]
  ' _ "$STORAGE_SOURCE" "$RUN_ID" >/dev/null 2>&1; then
    incus exec "$DEFAULT_INSTANCE" --project "$DEFAULT_PROJECT" -- \
      umount "$STORAGE_ALIAS" >/dev/null 2>&1 || true
  fi
  incus delete -f "$NAMED_INSTANCE" --project "$NAMED_PROJECT" >/dev/null 2>&1 || true
  incus delete -f "$DEFAULT_INSTANCE" --project "$DEFAULT_PROJECT" >/dev/null 2>&1 || true
  incus project delete "$NAMED_PROJECT" >/dev/null 2>&1 || true
  incus project delete "$DEFAULT_PROJECT" >/dev/null 2>&1 || true
  if [ -f "$FIXTURE/.subyard-status-fixture" ] \
    && [ "$(cat "$FIXTURE/.subyard-status-fixture")" = "$RUN_ID" ]; then
    find "$FIXTURE" -depth -delete
  fi
}

[ ! -e "$FIXTURE" ] || fail "fixture path already exists: $FIXTURE"
for project in "$DEFAULT_PROJECT" "$NAMED_PROJECT"; do
  ! incus project show "$project" >/dev/null 2>&1 \
    || fail "refusing to reuse existing Incus project $project"
done
trap cleanup EXIT
install -d -m 0700 "$FIXTURE" "$HOME_DIR" "$CONFIG_HOME/yards" "$DATA_HOME"
printf '%s\n' "$RUN_ID" >"$FIXTURE/.subyard-status-fixture"
printf 'SSH_PORT=32223\n' >"$CONFIG_HOME/yards/demo.env"

if ! incus storage show default >/dev/null 2>&1; then
  incus storage create default dir >/dev/null
fi
pool_source="$(incus storage get default source)"
if [ ! -d "$pool_source" ]; then
  case "$pool_source" in
    /home/dev/.cache/subyard-*/owner/subyard/incus/storage)
      install -d -m 0711 "$pool_source"
      sudo systemctl restart incus.service
      incus admin waitready --timeout=60
      ;;
    *) fail "default pool source is unexpectedly absent: $pool_source" ;;
  esac
fi
[ "$(incus storage get default source)" = "$pool_source" ] || fail "default pool source changed"
if ! incus network show incusbr0 >/dev/null 2>&1; then
  incus network create incusbr0 ipv4.address=auto ipv6.address=none >/dev/null
fi

for project in "$DEFAULT_PROJECT" "$NAMED_PROJECT"; do
  incus project create "$project" -c features.images=false >/dev/null
  incus profile device add default root disk pool=default path=/ --project "$project" >/dev/null
  incus profile device add default eth0 nic network=incusbr0 --project "$project" >/dev/null
done
incus init images:debian/13 "$DEFAULT_INSTANCE" --project "$DEFAULT_PROJECT" \
  -c security.nesting=true >/dev/null
incus start "$DEFAULT_INSTANCE" --project "$DEFAULT_PROJECT"

incus init images:debian/13 "$NAMED_INSTANCE" --project "$NAMED_PROJECT" \
  -c security.nesting=true >/dev/null
incus start "$NAMED_INSTANCE" --project "$NAMED_PROJECT"

for target in "$DEFAULT_PROJECT/$DEFAULT_INSTANCE" "$NAMED_PROJECT/$NAMED_INSTANCE"; do
  project="${target%/*}"
  instance="${target#*/}"
  ready=0
  for _ in $(seq 1 120); do
    if incus exec "$instance" --project "$project" -- true >/dev/null 2>&1; then
      ready=1
      break
    fi
    sleep 1
  done
  [ "$ready" = 1 ] || fail "$target agent did not become ready"
done

# A small real mount proves du -x handling without a nested VM or custom volume.
incus exec "$NAMED_INSTANCE" --project "$NAMED_PROJECT" -- sh -eu -c \
  'mount -t tmpfs -o size=64M subyard-status /srv; fallocate -l 32M /srv/status-srv; sync'

incus exec "$DEFAULT_INSTANCE" --project "$DEFAULT_PROJECT" -- sh -eu -c '
source=$1
alias=$2
run_id=$3
[ ! -e "$source" ] && [ ! -e "$alias" ]
install -d -m 0700 "$source" "$alias"
printf "%s\n" "$run_id" > "$source/.subyard-status-storage-fixture"
fallocate -l 128M "$source/allocated-fixture"
mount --bind "$source" "$alias"
sync
' _ "$STORAGE_SOURCE" "$STORAGE_ALIAS" "$RUN_ID"

cd "$ROOT"

yard() {
  env \
    HOME="$HOME_DIR" \
    SUBYARD_OPERATOR_HOME="$HOME_DIR" \
    SUBYARD_CONFIG_HOME="$CONFIG_HOME" \
    SUBYARD_HOME="$DATA_HOME" \
    SUBYARD_HOST_ID=status-owner \
    SUBYARD_NO_AUDIT=1 \
    "$ROOT/.build/yard" "$@"
}

summary="$(yard status)"
grep -Fq 'status-owner/default' <<<"$summary" || fail "summary omitted default yard"
grep -Fq 'status-owner/demo' <<<"$summary" || fail "summary omitted named yard"
grep -Fq 'instance yard (container)' <<<"$summary" || fail "summary omitted container kind"
grep -Fq 'instance yard-demo (container)' <<<"$summary" || fail "summary omitted named container"
! grep -Fq '  desired  ' <<<"$summary" || fail "summary ran detailed status"

check_detail() {
  local name="$1" cache="$2" label="$3" output started elapsed
  shift 3
  rm -f "$cache" "$cache.lock" "$cache.tmp"
  started="$(date +%s%N)"
  output="$(yard "$@" status)"
  elapsed=$((($(date +%s%N) - started) / 1000000))
  [ "$elapsed" -lt 8000 ] || fail "$name cold detailed status took ${elapsed}ms"
  grep -Fq "$label  RUNNING" <<<"$output" || fail "$name detailed selector routed incorrectly"
  grep -Fq 'refresh started' <<<"$output" || fail "$name cold status did not start async refresh"
  for _ in $(seq 1 120); do
    [ -s "$cache" ] && [ "$(wc -w <"$cache")" -eq 2 ] && break
    sleep 0.1
  done
  [ -s "$cache" ] || fail "$name async refresh did not create cache"
  printf '%s cold detailed status <8s; async cache created\n' "$name"
}

check_detail default "$DATA_HOME/space.cache" yard -Y default
check_detail named-container "$DATA_HOME/space-demo.cache" demo -Y demo
[ ! -e "$DATA_HOME/space-default.cache" ] || fail "legacy default cache name was recreated"

refreshed="$(yard space --refresh)"
assert_cache_includes_separate_srv() {
  local name="$1" project="$2" instance="$3" cache="$4"
  local root_device srv_device root_figure total_figure cached_figure
  root_device="$(incus exec "$instance" --project "$project" -- stat -c %d /)"
  srv_device="$(incus exec "$instance" --project "$project" -- stat -c %d /srv)"
  [ "$root_device" != "$srv_device" ] \
    || fail "$name /srv fixture is not on a separate device"
  root_figure="$(measure_figure "$project" "$instance" /)"
  total_figure="$(measure_figure "$project" "$instance" / /srv)"
  [ "$total_figure" != "$root_figure" ] \
    || fail "$name fixture does not distinguish /srv: root=$root_figure total=$total_figure"
  cached_figure="$(awk 'NF == 2 { print $1 }' "$cache")"
  [ "$cached_figure" = "$total_figure" ] \
    || fail "$name cache does not include /srv: cache=$cached_figure total=$total_figure"
}

measure_figure() {
  local project="$1" instance="$2"
  shift 2
  incus exec "$instance" --project "$project" -- sh -c '
output="$(du -skx "$@" 2>/dev/null)" || exit
size="$(printf "%s\n" "$output" | awk "
  NF != 2 || \$1 !~ /^[0-9]+$/ { exit 1 }
  { total += \$1 }
  END { if (NR == 0) exit 1; print total }
")" || exit
awk -v size="$size" "
BEGIN {
  split(\"K M G T P E Z Y\", units)
  unit = 1
  while (size >= 1024 && unit < 8) {
    size /= 1024
    unit++
  }
  if (size >= 10 || size == int(size))
    printf \"%.0f%s\\n\", size, units[unit]
  else
    printf \"%.1f%s\\n\", size, units[unit]
}"' space-measure "$@"
}

assert_unique_incus_storage_alias() {
  local project="$1" instance="$2" cache="$3"
  local root_device source_identity alias_identity measurements raw_kib unique_kib
  local raw_figure unique_figure cached_figure
  root_device="$(incus exec "$instance" --project "$project" -- stat -c %d /)"
  source_identity="$(incus exec "$instance" --project "$project" -- \
    stat -c %d:%i "$STORAGE_SOURCE")"
  alias_identity="$(incus exec "$instance" --project "$project" -- \
    stat -c %d:%i "$STORAGE_ALIAS")"
  [[ "$source_identity" =~ ^[0-9]+:[0-9]+$ ]] \
    && [ "$source_identity" = "$alias_identity" ] \
    || fail "Incus storage source and alias are not the same device/inode: source=$source_identity alias=$alias_identity"
  [ "${source_identity%%:*}" = "$root_device" ] \
    || fail "Incus storage alias fixture is not on the root device"

  measurements="$(incus exec "$instance" --project "$project" -- sh -eu -c '
    raw="$(du -skx / | awk "NR == 1 { print \$1 }")"
    unique="$(du -skx --exclude="$1" / | awk "NR == 1 { print \$1 }")"
    printf "%s %s\n" "$raw" "$unique"
  ' _ "$STORAGE_ALIAS")"
  read -r raw_kib unique_kib <<<"$measurements"
  [[ "$raw_kib" =~ ^[0-9]+$ ]] && [[ "$unique_kib" =~ ^[0-9]+$ ]] \
    && [ "$raw_kib" -gt "$unique_kib" ] \
    || fail "same-inode fixture was not observable: raw=${raw_kib:-unknown} unique=${unique_kib:-unknown}"

  raw_figure="$(measure_figure "$project" "$instance" /)"
  unique_figure="$(measure_figure "$project" "$instance" \
    "--exclude=$STORAGE_ALIAS" /)"
  [ "$raw_figure" != "$unique_figure" ] \
    || fail "same-inode fixture would not distinguish double counting: raw=$raw_figure unique=$unique_figure"
  cached_figure="$(awk 'NF == 2 { print $1 }' "$cache")"
  [ "$cached_figure" = "$unique_figure" ] \
    || fail "space refresh did not report the unique allocation once: cache=$cached_figure unique=$unique_figure raw=$raw_figure"
  awk -v expected="$unique_figure" '
    $1 == "default" && $2 == "RUNNING" && $3 == expected { found = 1 }
    END { exit !found }
  ' <<<"$refreshed" \
    || fail "space refresh output did not render the unique allocation $unique_figure"
  printf 'same-inode Incus storage alias counted once (raw=%sKiB unique=%sKiB)\n' \
    "$raw_kib" "$unique_kib"
}

assert_unique_incus_storage_alias \
  "$DEFAULT_PROJECT" "$DEFAULT_INSTANCE" "$DATA_HOME/space.cache"
assert_cache_includes_separate_srv \
  named-container "$NAMED_PROJECT" "$NAMED_INSTANCE" "$DATA_HOME/space-demo.cache"

printf 'ok: real container space refresh, separate /srv and unique storage allocation\n'
