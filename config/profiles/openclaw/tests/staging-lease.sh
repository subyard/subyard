#!/usr/bin/env bash
# Exercise the actual host/guest payload, including replacement epochs.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
PAYLOAD="$ROOT/config/profiles/openclaw/resources/staging-gateway/staging-lease.sh"
if [ "${1:-}" = --ownership ]; then
  exec unshare -Ur --map-auto bash "$0" --ownership-inside
fi
tmp="$(mktemp -d /tmp/subyard-staging-lease.XXXXXX)"
gateway="" heartbeat=""
cleanup() {
  [ -z "$heartbeat" ] || { kill "$heartbeat" 2>/dev/null || true; wait "$heartbeat" 2>/dev/null || true; }
  [ -z "$gateway" ] || { kill "$gateway" 2>/dev/null || true; wait "$gateway" 2>/dev/null || true; }
  rm -rf -- "$tmp"
}
trap cleanup EXIT
lease() { sh "$PAYLOAD" "$tmp/lease" bot "$@"; }
mkdir "$tmp/legacy"
printf '{"holder":"guest","kind":"ephemeral","epoch":7,"expires":0}\n' > "$tmp/legacy/bot.json"
[ "$(sh "$PAYLOAD" "$tmp/legacy" bot guest acquire ephemeral 3 normal)" = 'OK 8' ]
[ "$(stat -c %a "$tmp/legacy/bot.epoch")" = 644 ]
# Optional real multi-user ownership regression, confined to a user namespace.
if [ "${1:-}" = --ownership-inside ]; then
  chmod 0755 "$tmp"
  cp "$PAYLOAD" "$tmp/payload.sh"; chmod 0644 "$tmp/payload.sh"
  PAYLOAD="$tmp/payload.sh"
  mkdir "$tmp/lease"
  printf '{"holder":"guest","kind":"ephemeral","epoch":7,"expires":0}\n' > "$tmp/lease/bot.json"
  printf '7\n' > "$tmp/lease/bot.epoch"
  : > "$tmp/lease/bot.lock"
  chmod 0644 "$tmp/lease/"*
  inode="$(stat -c %d:%i "$tmp/lease/bot.lock")"
  as_dev() { setpriv --reuid 1234 --regid 1235 --clear-groups sh "$PAYLOAD" "$tmp/lease" bot guest "$@"; }
  if as_dev acquire ephemeral 3 normal > "$tmp/denied" 2>&1; then
    printf 'FAIL: synthetic dev wrote root-owned lease files\n' >&2; exit 1
  fi
  lease guest setup 1234 1235
  [ "$(stat -c %d:%i "$tmp/lease/bot.lock")" = "$inode" ]
  for file in "$tmp/lease/"*; do [ "$(stat -c %u:%g:%a "$file")" = 1234:1235:644 ]; done
  # Privileged reconciliation must not follow a planted symlink.
  printf 'sentinel\n' > "$tmp/outside"
  mv "$tmp/lease/bot.epoch" "$tmp/counter"
  ln -s "$tmp/outside" "$tmp/lease/bot.epoch"
  if lease guest setup 1234 1235 > "$tmp/symlink-denied" 2>&1; then
    printf 'FAIL: lease setup accepted symlink counter\n' >&2; exit 1
  fi
  [ "$(stat -c %u:%g "$tmp/outside")" = 0:0 ] && [ "$(cat "$tmp/outside")" = sentinel ]
  rm "$tmp/lease/bot.epoch"; mv "$tmp/counter" "$tmp/lease/bot.epoch"
  # Execute the old root heartbeat's publish path after ownership repair.
  (
    exec 9>"$tmp/lease/bot.lock"; flock 9
    jq --argjson expiry "$(($(date +%s)+3))" '.expires=$expiry' "$tmp/lease/bot.json" > "$tmp/lease/old-root-renewal"
    chmod 0644 "$tmp/lease/old-root-renewal"
    mv "$tmp/lease/old-root-renewal" "$tmp/lease/bot.json"
  )
  [ "$(stat -c %u:%g:%a "$tmp/lease/bot.json")" = 0:0:644 ]
  [ "$(as_dev acquire ephemeral 3 normal)" = 'OK 8' ]
  [ "$(stat -c %d:%i "$tmp/lease/bot.lock")" = "$inode" ]
  [ "$(stat -c %u:%g:%a "$tmp/lease/bot.json")" = 1234:1235:644 ]
  chmod 0640 "$tmp/lease/bot.json"
  [ "$(as_dev acquire ephemeral 3 normal)" = 'OK 9' ]
  sleep 30 & gateway=$!
  printf '%s\n' "$gateway" > "$tmp/gateway.pid"
  before="$(jq .expires "$tmp/lease/bot.json")"
  sh "$PAYLOAD" "$tmp/lease" bot guest heartbeat 3 "$tmp/gateway.pid" "$tmp/heartbeat.pid" 9 & heartbeat=$!
  sleep 1.2
  jq -e --argjson before "$before" '.expires > $before and .epoch==9' "$tmp/lease/bot.json" >/dev/null
  [ "$(stat -c %u:%g:%a "$tmp/lease/bot.json")" = 1234:1235:640 ]
  as_dev release 9
  [ ! -e "$tmp/lease/bot.json" ]
  rm "$tmp/gateway.pid"; wait "$heartbeat"; heartbeat=""
  printf 'ok: root-to-dev lease migration preserves lock inode and root renewal preserves dev ownership/mode\n'
  exit 0
fi
# Setup admits a fresh empty directory and rejects unsafe or orphaned targets.
sh "$PAYLOAD" "$tmp/fresh" bot guest setup "$(id -u)" "$(id -g)"
[ -d "$tmp/fresh" ] && [ ! -e "$tmp/fresh/bot.lock" ]
for target in bot.json bot.epoch; do
  for kind in file symlink directory; do
    rm -rf "$tmp/missing-lock"; mkdir "$tmp/missing-lock"
    case "$kind" in
      file) printf 'retained state\n' > "$tmp/missing-lock/$target" ;;
      symlink) ln -s "$tmp/absent-target" "$tmp/missing-lock/$target" ;;
      directory) mkdir "$tmp/missing-lock/$target" ;;
    esac
    if sh "$PAYLOAD" "$tmp/missing-lock" bot guest setup "$(id -u)" "$(id -g)" > "$tmp/setup-rejected" 2>&1; then
      printf 'FAIL: setup accepted missing-lock %s %s\n' "$target" "$kind" >&2; exit 1
    fi
    [ ! -e "$tmp/missing-lock/bot.lock" ]
  done
done
[ "$(lease canonical acquire canonical 3 normal)" = 'OK 1' ]
[ "$(lease guest acquire ephemeral 3 normal)" = 'PREEMPT canonical' ]
lease foreign release
[ -f "$tmp/lease/bot.json" ]
[ "$(lease guest acquire ephemeral 3 force)" = 'OK 2' ]
[[ "$(lease other acquire ephemeral 3 normal)" = 'BUSY guest ephemeral '* ]]
lease guest release 1
jq -e '.holder=="guest" and .epoch==2' "$tmp/lease/bot.json" >/dev/null
# Expiry allows another holder without force and still advances the epoch.
jq '.expires=0' "$tmp/lease/bot.json" > "$tmp/expired"
mv "$tmp/expired" "$tmp/lease/bot.json"
[ "$(lease guest acquire ephemeral 3 normal)" = 'OK 3' ]
lease guest release 3
[ ! -e "$tmp/lease/bot.json" ]
[ "$(lease guest acquire ephemeral 3 normal)" = 'OK 4' ]
lease guest release 3
jq -e '.epoch==4' "$tmp/lease/bot.json" >/dev/null
sleep 30 & gateway=$!
printf '%s\n' "$gateway" > "$tmp/gateway.pid"
# A stale sidecar must neither renew nor release a same-holder replacement.
cp "$tmp/lease/bot.json" "$tmp/before"
sh "$PAYLOAD" "$tmp/lease" bot guest heartbeat 3 "$tmp/gateway.pid" "$tmp/heartbeat.pid" 3 & heartbeat=$!
sleep 1.2
cmp "$tmp/before" "$tmp/lease/bot.json"
rm "$tmp/gateway.pid"
wait "$heartbeat"; heartbeat=""
cmp "$tmp/before" "$tmp/lease/bot.json"
# The matching sidecar renews, then removes only its own generation.
printf '%s\n' "$gateway" > "$tmp/gateway.pid"
sh "$PAYLOAD" "$tmp/lease" bot guest heartbeat 3 "$tmp/gateway.pid" "$tmp/heartbeat.pid" 4 & heartbeat=$!
sleep 1.2
jq -e --argjson before "$(jq .expires "$tmp/before")" '.expires > $before and .epoch==4' "$tmp/lease/bot.json" >/dev/null
rm "$tmp/gateway.pid"
wait "$heartbeat"; heartbeat=""
[ ! -e "$tmp/lease/bot.json" ] && [ ! -e "$tmp/heartbeat.pid" ]
# Corrupt persistent generations fail closed rather than reusing an old epoch.
printf 'invalid\n' > "$tmp/lease/bot.epoch"
if lease guest acquire ephemeral 3 normal > "$tmp/invalid.out" 2>&1; then
  printf 'FAIL: corrupt epoch counter accepted\n' >&2; exit 1
fi
[ ! -e "$tmp/lease/bot.json" ]
printf 'ok: staging lease expiry, holders, preemption and epoch-fenced renew/release\n'
