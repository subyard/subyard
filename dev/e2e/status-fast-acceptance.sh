#!/usr/bin/env bash
# Real-Incus acceptance for status summary routing and cache-first detailed status.
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
  incus storage volume delete default yard-srv-demo --project "$NAMED_PROJECT" >/dev/null 2>&1 || true
  incus project delete "$NAMED_PROJECT" >/dev/null 2>&1 || true
  incus project delete "$DEFAULT_PROJECT" >/dev/null 2>&1 || true
  if [ -f "$FIXTURE/.subyard-status-fixture" ] \
    && [ "$(cat "$FIXTURE/.subyard-status-fixture")" = "$RUN_ID" ]; then
    find "$FIXTURE" -depth -delete
  fi
}
trap cleanup EXIT

[ ! -e "$FIXTURE" ] || fail "fixture path already exists: $FIXTURE"
for project in "$DEFAULT_PROJECT" "$NAMED_PROJECT"; do
  ! incus project show "$project" >/dev/null 2>&1 \
    || fail "refusing to reuse existing Incus project $project"
done
install -d -m 0700 "$FIXTURE" "$HOME_DIR" "$CONFIG_HOME/yards" "$DATA_HOME"
printf '%s\n' "$RUN_ID" >"$FIXTURE/.subyard-status-fixture"
printf 'SSH_PORT=32223\nINSTANCE_TYPE=vm\n' >"$CONFIG_HOME/yards/demo.env"

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
incus storage volume create default yard-srv-demo --project "$NAMED_PROJECT" >/dev/null

incus init images:debian/13 "$DEFAULT_INSTANCE" --project "$DEFAULT_PROJECT" \
  -c security.nesting=true >/dev/null
incus start "$DEFAULT_INSTANCE" --project "$DEFAULT_PROJECT"

incus init images:alpine/3.22 "$NAMED_INSTANCE" --vm --project "$NAMED_PROJECT" \
  -c security.secureboot=false -c limits.cpu=2 -c limits.memory=1024MiB >/dev/null
incus config device add "$NAMED_INSTANCE" srv disk --project "$NAMED_PROJECT" \
  pool=default source=yard-srv-demo path=/srv >/dev/null
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
  incus exec "$instance" --project "$project" -- sh -c \
    'dd if=/dev/zero of=/root/status-root bs=1M count=2 2>/dev/null
     fallocate -l 128M /srv/status-srv
     sync'
done

incus exec "$DEFAULT_INSTANCE" --project "$DEFAULT_PROJECT" -- sh -eu -c '
source=$1
alias=$2
run_id=$3
[ ! -e "$source" ] && [ ! -e "$alias" ]
install -d -m 0700 "$source" "$alias"
printf "%s\n" "$run_id" > "$source/.subyard-status-storage-fixture"
fallocate -l 1G "$source/allocated-fixture"
mount --bind "$source" "$alias"
sync
' _ "$STORAGE_SOURCE" "$STORAGE_ALIAS" "$RUN_ID"

cd "$ROOT"
./dev/build-engine.sh

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
grep -Fq 'status-owner/demo' <<<"$summary" || fail "summary omitted named VM yard"
grep -Fq 'instance yard (container)' <<<"$summary" || fail "summary omitted container kind"
grep -Fq 'instance yard-demo (vm)' <<<"$summary" || fail "summary omitted VM kind"
! grep -Fq '  desired  ' <<<"$summary" || fail "summary ran detailed status"

override="$(yard -Y demo status --all)"
grep -Fq 'status-owner/default' <<<"$override" || fail "--all did not override selector"
grep -Fq 'status-owner/demo' <<<"$override" || fail "--all omitted selected yard"
! grep -Fq '  desired  ' <<<"$override" || fail "--all ran detailed status"

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
  started="$(date +%s%N)"
  output="$(yard "$@" status)"
  elapsed=$((($(date +%s%N) - started) / 1000000))
  [ "$elapsed" -lt 8000 ] || fail "$name warm detailed status took ${elapsed}ms"
  grep -Fq 'in-yard rootfs' <<<"$output" || fail "$name warm status did not reuse cache"
  ! grep -Fq 'refresh started' <<<"$output" || fail "$name fresh cache was refreshed"
  printf '%s cold/warm detailed status <8s\n' "$name"
}

check_detail default "$DATA_HOME/space.cache" yard -Y default
check_detail named-vm "$DATA_HOME/space-demo.cache" demo -Y demo
[ ! -e "$DATA_HOME/space-default.cache" ] || fail "legacy default cache name was recreated"

space_output="$(yard space)"
grep -Fq 'YARD' <<<"$space_output" || fail "space table omitted header"
grep -Eq '^default[[:space:]]+RUNNING[[:space:]]+' <<<"$space_output" \
  || fail "space table omitted default container"
grep -Eq '^demo[[:space:]]+RUNNING[[:space:]]+' <<<"$space_output" \
  || fail "space table omitted named VM"

default_before="$(awk 'NF == 2 { print $2 }' "$DATA_HOME/space.cache")"
named_before="$(awk 'NF == 2 { print $2 }' "$DATA_HOME/space-demo.cache")"
while [ "$(date +%s)" -le "$default_before" ] || [ "$(date +%s)" -le "$named_before" ]; do
  sleep 0.1
done
refreshed="$(yard space --refresh)"
grep -Eq '^default[[:space:]]+RUNNING[[:space:]]+' <<<"$refreshed" \
  || fail "space refresh omitted default container"
grep -Eq '^demo[[:space:]]+RUNNING[[:space:]]+' <<<"$refreshed" \
  || fail "space refresh omitted named VM"
[ "$(awk 'NF == 2 { print $2 }' "$DATA_HOME/space.cache")" -gt "$default_before" ] \
  || fail "space refresh did not update default cache"
[ "$(awk 'NF == 2 { print $2 }' "$DATA_HOME/space-demo.cache")" -gt "$named_before" ] \
  || fail "space refresh did not update named cache"

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
  named-vm "$NAMED_PROJECT" "$NAMED_INSTANCE" "$DATA_HOME/space-demo.cache"

selected_space="$(yard @demo space)"
grep -Eq '^demo[[:space:]]+RUNNING[[:space:]]+' <<<"$selected_space" \
  || fail "@demo space omitted selected yard"
! grep -Eq '^default[[:space:]]' <<<"$selected_space" \
  || fail "@demo space included an unselected yard"

grep -Fq 'demo  RUNNING' < <(yard @demo status) || fail "@demo selector did not stay detailed"
grep -Fq 'demo  RUNNING' < <(yard -Y status-owner/demo status) \
  || fail "canonical HostID/yard selector did not stay detailed"
grep -Fq 'demo  RUNNING' < <(SUBYARD_YARD=demo yard status) \
  || fail "inherited named yard did not stay detailed"

python3 - "$ROOT/.build/yard" "$HOME_DIR" "$CONFIG_HOME" "$DATA_HOME" <<'PY'
import json
import os
import struct
import subprocess
import sys

engine, home, config_home, data_home = sys.argv[1:]
environment = os.environ.copy()
environment.update({
    "HOME": home,
    "SUBYARD_OPERATOR_HOME": home,
    "SUBYARD_CONFIG_HOME": config_home,
    "SUBYARD_HOME": data_home,
    "SUBYARD_HOST_ID": "status-owner",
    "SUBYARD_NO_AUDIT": "1",
})
process = subprocess.Popen(
    [engine, "-Y", "demo", "rpc", "--stdio"],
    stdin=subprocess.PIPE,
    stdout=subprocess.PIPE,
    stderr=subprocess.PIPE,
    env=environment,
)

def write(request):
    payload = json.dumps(request, separators=(",", ":")).encode()
    process.stdin.write(struct.pack(">I", len(payload)) + payload)
    process.stdin.flush()

def read():
    header = process.stdout.read(4)
    if len(header) != 4:
        raise RuntimeError(process.stderr.read().decode())
    size = struct.unpack(">I", header)[0]
    return json.loads(process.stdout.read(size))

write({"version": 1, "type": "request", "id": "negotiate", "method": "rpc.negotiate"})
while read().get("id") != "negotiate":
    pass
write({"version": 1, "type": "request", "id": "status", "method": "yard.status"})
while True:
    response = read()
    if response.get("id") == "status":
        break
result = response.get("result", {})
if response.get("error") or result.get("context", {}).get("yardName") != "demo":
    raise RuntimeError(f"yard.status escaped selected context: {response!r}")
process.stdin.close()
if process.wait(timeout=5) != 0:
    raise RuntimeError(process.stderr.read().decode())
PY

install -d -m 0700 \
  "$DATA_HOME/owner-inventory/connections" \
  "$DATA_HOME/owner-inventory/owners" \
  "$FIXTURE/fake-bin"
cat >"$DATA_HOME/owner-inventory/connections/remote-owner.json" <<'JSON'
{"hostId":"remote-owner","destination":"unreachable.invalid"}
JSON
cat >"$DATA_HOME/owner-inventory/owners/remote-owner.json" <<'JSON'
{"fetchedAt":"2020-01-01T00:00:00Z","inventory":{"schema":1,"hostId":"remote-owner","observedAt":"2020-01-01T00:00:00Z","yards":[{"name":"remote","kind":"container","instance":"yard-remote","state":"RUNNING","sshPort":2222,"devUser":"dev","projects":[]}]}}
JSON
printf '#!/bin/sh\nexit 42\n' >"$FIXTURE/fake-bin/ssh"
chmod 0700 "$FIXTURE/fake-bin/ssh"
started="$(date +%s%N)"
set +e
env \
  PATH="$FIXTURE/fake-bin:$PATH" \
  HOME="$HOME_DIR" \
  SUBYARD_OPERATOR_HOME="$HOME_DIR" \
  SUBYARD_CONFIG_HOME="$CONFIG_HOME" \
  SUBYARD_HOME="$DATA_HOME" \
  SUBYARD_HOST_ID=status-owner \
  SUBYARD_NO_AUDIT=1 \
  "$ROOT/.build/yard" status >"$FIXTURE/stale.out" 2>"$FIXTURE/stale.err"
stale_code=$?
set -e
elapsed=$((($(date +%s%N) - started) / 1000000))
[ "$stale_code" -eq 1 ] || fail "stale remote inventory exit code changed: $stale_code"
[ "$elapsed" -lt 3000 ] || fail "unavailable remote owner was not bounded: ${elapsed}ms"
grep -Fq 'remote-owner/remote' "$FIXTURE/stale.out" \
  || fail "stale remote inventory was not rendered"
grep -Fq 'Warning: owner inventory refresh:' "$FIXTURE/stale.err" \
  || fail "stale remote inventory warning was lost"

printf 'ok: status routing, unique-allocation space refresh, cache-first container/VM detail and scoped RPC\n'
