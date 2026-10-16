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
[ "$(sh "$PAYLOAD" "$tmp/legacy" bot guest acquire ephemeral 3 normal)" = 'STALE guest 7' ]
if sh "$PAYLOAD" "$tmp/legacy" bot guest stop 7 "$tmp/absent.pid"; then
  echo 'FAIL legacy identity admitted without container stop' >&2; exit 1
fi
# The owning adapter may release after confirming container stop/removal.
sh "$PAYLOAD" "$tmp/legacy" bot guest release 7
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
  as_dev release 7
  [ "$(as_dev acquire ephemeral 3 normal)" = 'OK 8' ]
  [ "$(stat -c %d:%i "$tmp/lease/bot.lock")" = "$inode" ]
  [ "$(stat -c %u:%g:%a "$tmp/lease/bot.json")" = 1234:1235:644 ]
  chmod 0640 "$tmp/lease/bot.json"
  [ "$(as_dev acquire ephemeral 3 normal)" = 'OK 8' ]
  lease guest launch 8 "$tmp/gateway.pid" "$tmp/worker.log" 'exec sleep 30' "$tmp"
  gateway=$(cat "$tmp/gateway.pid")
  before="$(jq .expires "$tmp/lease/bot.json")"
  sh "$PAYLOAD" "$tmp/lease" bot guest heartbeat 3 "$tmp/gateway.pid" "$tmp/heartbeat.pid" 8 & heartbeat=$!
  sleep 1.2
  jq -e --argjson before "$before" '.expires > $before and .epoch==8' "$tmp/lease/bot.json" >/dev/null
  [ "$(stat -c %u:%g:%a "$tmp/lease/bot.json")" = 1234:1235:640 ]
  as_dev release 8
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
[ "$(lease guest acquire ephemeral 3 normal)" = 'PREEMPT canonical 1' ]
if lease foreign release 1; then echo "FAIL foreign release accepted" >&2; exit 1; fi
[ -f "$tmp/lease/bot.json" ]
if lease guest acquire ephemeral 3 force; then echo 'FAIL unsafe force accepted' >&2; exit 1; fi
lease canonical stop 1 "$tmp/absent.pid"
[ "$(lease guest acquire ephemeral 3 normal)" = 'OK 2' ]
[[ "$(lease other acquire ephemeral 3 normal)" = 'BUSY guest ephemeral '* ]]
if lease guest release 1; then echo "FAIL stale release accepted" >&2; exit 1; fi
jq -e '.holder=="guest" and .epoch==2' "$tmp/lease/bot.json" >/dev/null
# Expiry prevents renewal/replacement until the old worker is confirmed stopped.
jq '.expires=0' "$tmp/lease/bot.json" > "$tmp/expired"
mv "$tmp/expired" "$tmp/lease/bot.json"
[ "$(lease other acquire ephemeral 3 normal)" = 'STALE guest 2' ]
lease guest stop 2 "$tmp/absent.pid"
[ "$(lease guest acquire ephemeral 3 normal)" = 'OK 3' ]
lease guest release 3
[ ! -e "$tmp/lease/bot.json" ]
[ "$(lease guest acquire ephemeral 10 normal)" = 'OK 4' ]
if lease guest release 3; then echo "FAIL stale release accepted" >&2; exit 1; fi
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
# The matching sidecar renews, then stops its worker group before releasing.
kill "$gateway"; wait "$gateway" || true; gateway=""
lease guest launch 4 "$tmp/gateway.pid" "$tmp/worker.log" 'sleep 30 & wait' "$tmp"
gateway=$(cat "$tmp/gateway.pid")
sh "$PAYLOAD" "$tmp/lease" bot guest heartbeat 10 "$tmp/gateway.pid" "$tmp/heartbeat.pid" 4 & heartbeat=$!
sleep 1.2
jq -e --argjson before "$(jq .expires "$tmp/before")" '.expires > $before and .epoch==4' "$tmp/lease/bot.json" >/dev/null
rm "$tmp/gateway.pid"
wait "$heartbeat"; heartbeat=""
[ ! -e "$tmp/lease/bot.json" ] && [ ! -e "$tmp/heartbeat.pid" ]
# Common projection is read-only, whitelist-only and stable across retained epochs.
[ "$(sh "$PAYLOAD" "$tmp/absent" bot guest status | jq -r .lease.state)" = available ]
[ ! -e "$tmp/absent" ]
[ "$(lease guest acquire ephemeral 30 normal)" = 'OK 5' ]
jq '.secret="synthetic-secret"' "$tmp/lease/bot.json" > "$tmp/public-test"
mv "$tmp/public-test" "$tmp/lease/bot.json"
lease guest status > "$tmp/status"
jq -e '.lease.contract=="resource-lease/v1" and .lease.resource_id=="bot" and .lease.slot_id==null and
 .lease.generation==5 and .lease.state=="held" and .lease.owner.zone=="guest" and .lease.expires_at!=null and
 (has("secret")|not)' "$tmp/status" >/dev/null
if rg -q synthetic-secret "$tmp/status"; then echo "FAIL status leaked credential" >&2; exit 1; fi
lease guest launch 5 "$tmp/launched.pid" "$tmp/worker.log" 'exec sleep 30' "$tmp"
launched=$(cat "$tmp/launched.pid")
if lease guest release 5 "$tmp/launched.pid"; then echo 'FAIL live worker release accepted' >&2; exit 1; fi
if lease guest stop 4 "$tmp/launched.pid"; then echo 'FAIL stale stop accepted' >&2; exit 1; fi
kill -0 "$launched"
lease guest stop 5 "$tmp/launched.pid"
[ ! -e "$tmp/launched.pid" ] && [ ! -e "$tmp/lease/bot.json" ]
[ "$(lease guest acquire ephemeral 30 normal)" = 'OK 6' ]
if lease guest launch 5 "$tmp/launched.pid" "$tmp/worker.log" 'touch marker' "$tmp"; then
  echo 'FAIL stale launch accepted' >&2; exit 1
fi
[ ! -e "$tmp/marker" ]
lease guest launch 6 "$tmp/gateway.pid" "$tmp/worker.log" 'sleep 30 & wait' "$tmp"
gateway=$(cat "$tmp/gateway.pid")
jq '.expires=0' "$tmp/lease/bot.json" > "$tmp/expired"; mv "$tmp/expired" "$tmp/lease/bot.json"
[ "$(lease guest status | jq -r .lease.state)" = draining ]
cp "$tmp/lease/bot.json" "$tmp/expired-before"
sh "$PAYLOAD" "$tmp/lease" bot guest heartbeat 3 "$tmp/gateway.pid" "$tmp/heartbeat.pid" 6 & heartbeat=$!
sleep 1.2
cmp "$tmp/expired-before" "$tmp/lease/bot.json"
rm "$tmp/gateway.pid"
wait "$heartbeat"; heartbeat=""
[ ! -e "$tmp/lease/bot.json" ]
# Exercise the actual client wait/cancel/preempt adapter against the real payload.
python3 - "$ROOT" "$tmp" <<'CLIENT'
import fcntl, json, os, pathlib, signal, subprocess, sys, time
root, tmp = map(pathlib.Path, sys.argv[1:])
fixture = tmp / 'client'
(fixture / 'bin').mkdir(parents=True)
(fixture / 'staging/guest').mkdir(parents=True)
source = root / 'config/profiles/openclaw/resources/staging-gateway'
script = (source / 'sy-stage.sh').read_text().replace('LEASE_DIR="/srv/staging/_lease"', f'LEASE_DIR="{fixture}/lease"').replace('STAGING_ROOT="/srv/staging"', f'STAGING_ROOT="{fixture}/staging"')
(fixture / 'sy-stage').write_text(script)
(fixture / 'sy-stage').chmod(0o755)
(fixture / 'staging-lease.sh').write_text((source / 'staging-lease.sh').read_text())
(fixture / 'staging/guest/zone.env').write_text(f"CNAME=guest\nBOT_LEASE_KEY=bot\nLEASE_TTL=30\nGW_PID={fixture}/guest.pid\nHB_PID={fixture}/hb.pid\nYLOG={fixture}/log\n")
(fixture / 'bin/docker').write_text('#!/bin/sh\ncase "$1" in inspect) exit 0 ;; exec) [ "${FAIL_STOP:-}" != 1 ] || exit 1; shift 2; exec "$@" ;; esac\n')
(fixture / 'bin/docker').chmod(0o755)
env = dict(os.environ, PATH=str(fixture / 'bin')+':'+os.environ['PATH'])
def lease(holder, *args):
    return subprocess.run(['sh', str(source / 'staging-lease.sh'), str(fixture / 'lease'), 'bot', holder, *args], text=True, capture_output=True, check=True).stdout.strip()
def run(wait=0, **extra):
    return subprocess.run([str(fixture / 'sy-stage'), 'reserve', '--zone', 'guest', '--wait', str(wait)], env=dict(env, **extra), text=True, capture_output=True, timeout=5)
assert lease('other', 'acquire', 'ephemeral', '30', 'normal') == 'OK 1'
before = (fixture / 'lease/bot.json').read_bytes()
started = time.monotonic(); result = run(1)
assert result.returncode != 0 and 'timeout:' in result.stderr, result
assert time.monotonic()-started < 4
assert before == (fixture / 'lease/bot.json').read_bytes()
with (fixture / 'lease/bot.lock').open() as locked:
    fcntl.flock(locked, fcntl.LOCK_EX)
    started = time.monotonic(); result = run(30)
    assert result.returncode != 0 and 'unavailable:' in result.stderr, result
    assert time.monotonic()-started < 2
process = subprocess.Popen([str(fixture / 'sy-stage'), 'reserve', '--zone', 'guest', '--wait', '30'], env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True)
time.sleep(.2); os.killpg(process.pid, signal.SIGINT)
process.communicate(timeout=3)
assert process.returncode in (130, -signal.SIGINT), process.returncode
assert before == (fixture / 'lease/bot.json').read_bytes()
state = json.loads(before); state['expires'] = 0
(fixture / 'lease/bot.json').write_text(json.dumps(state))
started = time.monotonic(); result = run(30)
assert result.returncode != 0 and 'stale:' in result.stderr, result
assert time.monotonic()-started < 2
lease('other', 'stop', '1', str(fixture / 'absent.pid'))
assert lease('canonical', 'acquire', 'canonical', '30', 'normal') == 'OK 2'
# Failed transport/stop must retain canonical authority and forbid takeover.
result = run(FAIL_STOP='1')
assert result.returncode != 0 and 'stop unconfirmed' in result.stderr, result
assert json.loads((fixture / 'lease/bot.json').read_text())['holder'] == 'canonical'
result = run()
assert result.returncode == 0, result
assert json.loads((fixture / 'lease/bot.json').read_text())['holder'] == 'guest'
# A dead leader does not surrender the still-running worker group.
pidfile, childfile = fixture / 'orphan.pid', fixture / 'child.pid'
groups = []
try:
    lease('guest', 'launch', '3', str(pidfile), str(fixture / 'worker.log'),
          f'sleep 60 & echo $! > "{childfile}"; wait', str(fixture))
    leader = int(pidfile.read_text()); groups.append(leader)
    deadline = time.monotonic() + 3
    while not childfile.exists() and time.monotonic() < deadline:
        time.sleep(.01)
    child = int(childfile.read_text())
    os.kill(leader, signal.SIGKILL)
    pidfile.unlink()
    before = (fixture / 'lease/bot.json').read_bytes()
    result = subprocess.run(['sh', str(source / 'staging-lease.sh'), str(fixture / 'lease'),
                             'bot', 'guest', 'launch', '3', str(pidfile), str(fixture / 'worker.log'),
                             'exec sleep 60', str(fixture)], text=True, capture_output=True, timeout=5)
    current = json.loads((fixture / 'lease/bot.json').read_text())
    groups.append(current['worker']['pid'])
    assert result.returncode != 0, 'relaunch discarded a live old process group'
    assert (fixture / 'lease/bot.json').read_bytes() == before
    os.kill(child, 0)
finally:
    for group in set(groups):
        try: os.killpg(group, signal.SIGKILL)
        except ProcessLookupError: pass
print('ok: sy-stage bounded busy wait, cancellation, stale fail-fast and verified preemption')
CLIENT
# Corrupt persistent generations fail closed rather than reusing an old epoch.
printf 'invalid\n' > "$tmp/lease/bot.epoch"
if lease guest acquire ephemeral 3 normal > "$tmp/invalid.out" 2>&1; then
  printf 'FAIL: corrupt epoch counter accepted\n' >&2; exit 1
fi
[ ! -e "$tmp/lease/bot.json" ]
[ "$(lease guest status | jq -r .lease.state)" = quarantined ]
[ "$(cat "$tmp/lease/bot.epoch")" = invalid ]
printf 'ok: staging lease expiry, holders, preemption and epoch-fenced renew/release\n'
