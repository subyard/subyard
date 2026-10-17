#!/bin/sh
# Shared yard/runner lease payload. Callers own consent, guards and lifecycle fencing.
set -eu
dir="$1"; key="$2"; me="$3"; action="$4"
for identity in "$key" "$me"; do
  case "$identity" in ''|*[!a-zA-Z0-9_-]*) echo 'invalid staging lease identity' >&2; exit 2 ;; esac
done
valid_state() {
  jq -e 'type == "object" and
    (.holder | type == "string" and test("^[a-zA-Z0-9_-]+$")) and
    (.kind == "canonical" or .kind == "ephemeral") and
    (.epoch | type == "number" and . > 0 and . < 999999999999999 and floor == .) and
    (.expires | type == "number" and . >= 0 and . <= 253402300799 and floor == .) and
    ((has("worker") | not) or .worker == null or (.worker |
      type == "object" and (.pid | type == "number" and . > 1 and . <= 2147483647 and floor == .) and
      (.started | type == "string" and test("^[0-9]+$"))))' "$st" >/dev/null 2>&1
}
validate_paths() {
  [ ! -L "$dir" ] || return 1
  for file in "$dir/$key.lock" "$dir/$key.json" "$dir/$key.epoch"; do
    [ ! -L "$file" ] && { [ ! -e "$file" ] || [ -f "$file" ]; } || return 1
  done
}
worker_identity() {
  [ -r "/proc/$pid/stat" ] || return 0
  sed 's/.*) //' "/proc/$pid/stat" 2>/dev/null | cut -d' ' -f20
}
worker_group_alive() {
  for process in /proc/[0-9]*/stat; do
    stat=$(sed 's/.*) //' "$process" 2>/dev/null) || continue
    case "$stat" in Z\ *) continue ;; esac
    [ "$(printf '%s\n' "$stat" | cut -d' ' -f3,4)" != "$pid $pid" ] || return 0
  done
  return 1
}
stop_worker() {
  pidfile="$1"
  pid=$(jq -r '.worker.pid // empty' "$st")
  started=$(jq -r '.worker.started // empty' "$st")
  if ! jq -e 'has("worker")' "$st" >/dev/null; then
    echo 'legacy gateway identity unavailable; stop its container' >&2; return 1
  fi
  if [ -z "$pid" ] || [ -z "$started" ]; then
    # A fresh reservation has no worker; a PID file would contradict that state.
    [ ! -e "$pidfile" ] || { echo 'gateway identity unavailable; stop its container' >&2; return 1; }
    return 0
  fi
  case "$pid:$started" in *[!0-9:]*) echo 'invalid gateway identity' >&2; return 1 ;; esac
  [ "${#pid}" -le 10 ] && [ "$pid" -gt 1 ] && [ "$pid" -le 2147483647 ] \
    || { echo 'invalid gateway PID' >&2; return 1; }
  current=$(worker_identity)
  [ -z "$current" ] || [ "$current" = "$started" ] || { echo 'gateway identity changed' >&2; return 1; }
  if worker_group_alive; then
    kill -TERM "-$pid" 2>/dev/null || true
    for _ in 1 2 3 4 5; do worker_group_alive || break; sleep 1; done
    if worker_group_alive; then kill -KILL "-$pid" 2>/dev/null || true; fi
    for _ in 1 2 3 4 5; do worker_group_alive || break; sleep 1; done
    if worker_group_alive; then echo 'gateway stop unconfirmed' >&2; return 1; fi
  fi
  rm -f "$pidfile"
}
case "$action" in
status)
# Read-only, including absent and malformed retained state. Whitelist public fields.
st="$dir/$key.json"; state=available; generation=0; failure=""
if ! validate_paths; then state=quarantined; failure=invalid_state
elif [ -e "$dir/$key.epoch" ]; then
  generation=$(cat "$dir/$key.epoch")
  case "$generation" in ''|0*|*[!0-9]*) state=quarantined; failure=invalid_state; generation=0 ;; esac
  if [ "${#generation}" -ge 15 ]; then state=quarantined; failure=invalid_state; generation=0; fi
fi
if [ "$state" != quarantined ] && [ -e "$st" ]; then
  if valid_state; then
    generation=$(jq -r .epoch "$st"); state=held
    [ "$(date +%s)" -lt "$(jq -r .expires "$st")" ] || state=draining
  else state=quarantined; failure=invalid_state; fi
fi
if [ "$state" = held ] || [ "$state" = draining ]; then
  jq -c --arg id "$key" --arg state "$state" '
    {holder,kind,epoch,expires,lease:{contract:"resource-lease/v1",resource_type:"staging-bot",
      resource_id:$id,slot_id:null,generation:.epoch,state:$state,
      owner:{zone:.holder,kind:.kind},expires_at:(.expires | strftime("%Y-%m-%dT%H:%M:%SZ"))}}' "$st"
else
  jq -cn --arg id "$key" --arg state "$state" --argjson generation "$generation" --arg failure "$failure" '
    {lease:{contract:"resource-lease/v1",resource_type:"staging-bot",resource_id:$id,
      slot_id:null,generation:$generation,state:$state,owner:null,expires_at:null}} |
    if $failure != "" then .lease.failure=$failure else . end'
fi
;;
setup)
uid="$5"; gid="$6"; lock="$dir/$key.lock"
[ ! -L "$dir" ] || { echo 'staging lease directory must not be a symlink' >&2; exit 1; }
# Validate all known targets even when no lock exists yet.
for file in "$lock" "$dir/$key.json" "$dir/$key.epoch"; do
  [ ! -L "$file" ] && { [ ! -e "$file" ] || [ -f "$file" ]; } \
    || { echo 'invalid staging lease file' >&2; exit 1; }
done
if [ ! -e "$lock" ]; then
  [ ! -e "$dir/$key.json" ] && [ ! -e "$dir/$key.epoch" ] \
    || { echo 'retained staging lease state requires its lock' >&2; exit 1; }
  install -d -m 0755 -o "$uid" -g "$gid" "$dir"
  exit 0
fi
install -d -m 0755 -o "$uid" -g "$gid" "$dir"
# Repair old root-created files without replacing the inode active flock users hold.
exec 9<"$lock"; flock 9
for file in "$lock" "$dir/$key.json" "$dir/$key.epoch"; do
  [ ! -L "$file" ] || { echo 'staging lease files must not be symlinks' >&2; exit 1; }
  [ ! -e "$file" ] || {
    [ -f "$file" ] || { echo 'invalid staging lease file' >&2; exit 1; }
    chown --no-dereference "$uid:$gid" "$file"
  }
done
;;
acquire)
kind="$5"; ttl="$6"; mode="$7"
case "$kind" in canonical|ephemeral) ;; *) echo 'invalid staging lease kind' >&2; exit 2 ;; esac
case "$ttl" in ''|*[!0-9]*|0*) echo 'invalid staging lease TTL' >&2; exit 2 ;; esac
[ "${#ttl}" -le 10 ] && [ "$ttl" -le 2147483647 ] || { echo 'invalid staging lease TTL' >&2; exit 2; }
[ "$mode" = normal ] || { echo 'unsafe staging lease takeover refused; stop and release the captured generation first' >&2; exit 1; }
validate_paths || { echo 'invalid staging lease path' >&2; exit 1; }
install -d -m 0755 "$dir"
lock="$dir/$key.lock"; st="$dir/$key.json"
exec 9>"$lock"
flock -n 9 || { echo 'unavailable: staging lease operation in progress' >&2; exit 1; }
now=$(date +%s)
holder=""; hk=""; epoch=0; exp=0
if [ -r "$st" ]; then
  valid_state || { echo 'invalid staging lease state' >&2; exit 1; }
  holder=$(jq -r '.holder // ""' "$st"); hk=$(jq -r '.kind // ""' "$st")
  epoch=$(jq -r '.epoch // 0' "$st");    exp=$(jq -r '.expires // 0' "$st")
fi
# JSON is removed on release; retain the generation alongside the retained lock.
# Write it first so interruption can skip an epoch, but can never reuse one.
if [ -r "$dir/$key.epoch" ]; then
  previous=$(cat "$dir/$key.epoch")
  case "$previous" in ''|0*|*[!0-9]*) echo 'invalid staging lease epoch counter' >&2; exit 1 ;; esac
  [ "${#previous}" -lt 15 ] && [ "$previous" -gt 0 ] || { echo 'invalid staging lease epoch counter' >&2; exit 1; }
fi
if [ -n "$holder" ]; then
  # TTL fences renewals, never proves a still-polling worker has stopped.
  if [ "$now" -ge "$exp" ]; then echo "STALE $holder $epoch"; exit 0; fi
  if [ "$holder" = "$me" ] && [ "$hk" = "$kind" ]; then echo "OK $epoch"; exit 0; fi
  if [ "$kind" = ephemeral ] && [ "$hk" = canonical ]; then echo "PREEMPT $holder $epoch"; exit 0; fi
  echo "BUSY $holder $hk $((exp-now))"; exit 0
fi
[ -z "${previous:-}" ] || [ "$previous" -le "$epoch" ] || epoch="$previous"
epoch=$((epoch+1))
printf '%s\n' "$epoch" > "$dir/$key.epoch"
chmod 0644 "$dir/$key.epoch"
# An old root heartbeat can publish after setup; replace its JSON as the caller.
t=$(mktemp "$st.XXXXXX")
trap 'rm -f "$t"' EXIT
printf '{"holder":"%s","kind":"%s","epoch":%d,"expires":%d,"worker":null}\n' "$me" "$kind" "$epoch" "$((now+ttl))" >"$t"
if [ -e "$st" ]; then chmod --reference="$st" "$t"; else chmod 0644 "$t"; fi
mv "$t" "$st"
echo "OK $epoch"
;;
launch|stop|release|fenced)
expected="${5:-}"
case "$expected" in ''|*[!0-9]*) echo 'captured staging lease epoch required' >&2; exit 2 ;; esac
st="$dir/$key.json"; lock="$dir/$key.lock"
validate_paths || { echo 'invalid staging lease path' >&2; exit 1; }
if [ "$action" = fenced ]; then
  install -d -m 0755 "$dir"
  expected_holder="$6"
else
  expected_holder="$me"
fi
if [ ! -d "$dir" ]; then
  [ "$action" = release ] && [ "$expected" = 0 ] && exit 0
  echo STALE; exit 1
fi
exec 9>"$lock"; flock 9
if [ "$action" = fenced ]; then chown --reference="$dir" "$lock"; fi
if [ -r "$st" ]; then
  valid_state || { echo 'invalid staging lease state' >&2; exit 1; }
  [ "$(jq -r '.holder' "$st")" = "$expected_holder" ] && [ "$(jq -r '.epoch' "$st")" = "$expected" ] \
    || { echo STALE; exit 1; }
elif [ "$action" = launch ] || [ "$expected" != 0 ] || { [ "$action" = fenced ] && [ -n "$expected_holder" ]; }; then
  echo STALE; exit 1
fi
if [ "$action" = fenced ]; then
  shift 6
  # The checked authority cannot change until the destructive operation and release finish.
  "$@" 9>&-
  [ ! -r "$st" ] || [ "$expected_holder" = "$me" ] || exit 0
fi
if [ "$action" = launch ]; then
  jq -e 'has("worker")' "$st" >/dev/null || { echo 'legacy gateway identity unavailable; stop its container' >&2; exit 1; }
  if [ "$(jq -r '.worker != null' "$st")" = true ]; then
    pid=$(jq -r .worker.pid "$st"); started=$(jq -r .worker.started "$st")
    current=$(worker_identity)
    [ -z "$current" ] || [ "$current" = "$started" ] || { echo 'gateway identity changed' >&2; exit 1; }
    if worker_group_alive; then echo 'gateway process group still running' >&2; exit 1; fi
  fi
  [ "$(date +%s)" -lt "$(jq -r .expires "$st")" ] || { echo STALE; exit 1; }
  pidfile="$6"; logfile="$7"; command="$8"; workspace="$9"
  if [ -r "$pidfile" ] && kill -0 "$(cat "$pidfile")" 2>/dev/null; then
    echo 'gateway already running' >&2; exit 1
  fi
  cd "$workspace"
  mkdir -p "$(dirname "$logfile")"
  # Do not let the worker inherit the lock: the heartbeat/stop use the same inode.
  setsid sh -c "$command" 9>&- >>"$logfile" 2>&1 &
  pid=$!
  echo "$pid" > "$pidfile"
  for _ in 1 2 3 4 5; do
    worker_group_alive && break
    sleep 0.1
  done
  worker_group_alive || { echo 'gateway process group unavailable' >&2; exit 1; }
  started=$(worker_identity)
  [ -n "$started" ] || { echo 'gateway exited before identity capture' >&2; exit 1; }
  t=$(mktemp "$st.XXXXXX")
  jq --argjson pid "$pid" --arg started "$started" '.worker={pid:$pid,started:$started}' "$st" > "$t"
  chown --reference="$st" "$t"; chmod --reference="$st" "$t"; mv "$t" "$st"
  exit 0
fi
if [ "$action" = release ] && [ "${6:-}" = reserved-only ] && [ -r "$st" ]; then
  jq -e 'has("worker") and .worker == null' "$st" >/dev/null \
    || { echo 'gateway identity exists; use stop before release' >&2; exit 1; }
fi
if [ "$action" = release ] && [ "${6:-}" != reserved-only ] && [ -n "${6:-}" ] && [ -r "$6" ] && kill -0 "$(cat "$6")" 2>/dev/null; then
  echo 'gateway still running; stop before release' >&2; exit 1
fi
if [ "$action" = stop ]; then
  if [ -r "$st" ]; then
    stop_worker "$6" || exit 1
  elif [ -e "$6" ]; then
    echo 'gateway has no lease identity; stop its container' >&2; exit 1
  fi
fi
if [ -r "$st" ] && [ ! -e "$dir/$key.epoch" ]; then
  jq -r .epoch "$st" > "$dir/$key.epoch"
  chmod 0644 "$dir/$key.epoch"
  chown --reference="$dir" "$dir/$key.epoch"
fi
rm -f "$st"
;;
heartbeat)
  lease="$1/$2.json"; lock="$1/$2.lock"; me="$3"; ttl="$5"; gwpid="$6"; hbpid="$7"; epoch="$8"
  validate_paths || exit 1
  echo $$ >"$hbpid"
  step=$((ttl/3)); [ "$step" -gt 0 ] || step=5
  while [ -f "$gwpid" ] && kill -0 "$(cat "$gwpid" 2>/dev/null)" 2>/dev/null; do
    ( exec 9>"$lock"; flock 9
      [ -r "$lease" ] && [ "$(jq -r ".holder//\"\"" "$lease")" = "$me" ] && [ "$(jq -r ".epoch//0" "$lease")" = "$epoch" ] || exit 0
      st="$lease"; valid_state || exit 0
      now=$(date +%s)
      [ "$now" -lt "$(jq -r .expires "$lease")" ] || exit 0
      t="$lease.t.$$"
      jq --argjson e "$((now+ttl))" ".expires=\$e" "$lease" >"$t" &&
      chown --reference="$lease" "$t" && chmod --reference="$lease" "$t" && mv "$t" "$lease"
    ) 2>/dev/null || true
    sleep "$step"
  done
  ( exec 9>"$lock"; flock 9
    if [ -r "$lease" ] && [ "$(jq -r ".holder//\"\"" "$lease")" = "$me" ] && [ "$(jq -r ".epoch//0" "$lease")" = "$epoch" ]; then
      st="$lease"
      stop_worker "$gwpid" && rm -f "$lease"
    fi
  ) 2>/dev/null || true
  [ "$(cat "$hbpid" 2>/dev/null)" != "$$" ] || rm -f "$hbpid"
;;
*) echo "unknown staging lease action: $action" >&2; exit 2 ;;
esac
