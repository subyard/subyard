#!/bin/sh
# Shared yard/runner lease payload. Callers own consent, guards and lifecycle fencing.
set -eu
dir="$1"; key="$2"; me="$3"; action="$4"
case "$action" in
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
install -d -m 0755 "$dir"
lock="$dir/$key.lock"; st="$dir/$key.json"
exec 9>"$lock"; flock 9
now=$(date +%s)
holder=""; hk=""; epoch=0; exp=0
if [ -r "$st" ]; then
  holder=$(jq -r '.holder // ""' "$st"); hk=$(jq -r '.kind // ""' "$st")
  epoch=$(jq -r '.epoch // 0' "$st");    exp=$(jq -r '.expires // 0' "$st")
fi
if [ "$mode" != force ] && [ -n "$holder" ] && [ "$holder" != "$me" ] && [ "$now" -lt "$exp" ]; then
  if [ "$kind" = ephemeral ] && [ "$hk" = canonical ]; then echo "PREEMPT $holder"; exit 0; fi
  echo "BUSY $holder $hk $((exp-now))"; exit 0
fi
# JSON is removed on release; retain the generation alongside the retained lock.
# Write it first so interruption can skip an epoch, but can never reuse one.
if [ -r "$dir/$key.epoch" ]; then
  previous=$(cat "$dir/$key.epoch")
  case "$previous" in ''|*[!0-9]*) echo 'invalid staging lease epoch counter' >&2; exit 1 ;; esac
  [ "$previous" -gt 0 ] || { echo 'invalid staging lease epoch counter' >&2; exit 1; }
  [ "$previous" -le "$epoch" ] || epoch="$previous"
fi
epoch=$((epoch+1))
printf '%s\n' "$epoch" > "$dir/$key.epoch"
chmod 0644 "$dir/$key.epoch"
# An old root heartbeat can publish after setup; replace its JSON as the caller.
t=$(mktemp "$st.XXXXXX")
trap 'rm -f "$t"' EXIT
printf '{"holder":"%s","kind":"%s","epoch":%d,"expires":%d}\n' "$me" "$kind" "$epoch" "$((now+ttl))" >"$t"
if [ -e "$st" ]; then chmod --reference="$st" "$t"; else chmod 0644 "$t"; fi
mv "$t" "$st"
echo "OK $epoch"
;;
release)
expected="${5:-}"
st="$dir/$key.json"; lock="$dir/$key.lock"
[ -r "$st" ] || exit 0
exec 9>"$lock"; flock 9
[ -r "$st" ] && [ "$(jq -r '.holder // ""' "$st")" = "$me" ] || exit 0
[ -z "$expected" ] || [ "$(jq -r '.epoch // 0' "$st")" = "$expected" ] || exit 0
rm -f "$st"
;;
heartbeat)
  lease="$1/$2.json"; lock="$1/$2.lock"; me="$3"; ttl="$5"; gwpid="$6"; hbpid="$7"; epoch="$8"
  echo $$ >"$hbpid"
  step=$((ttl/3)); [ "$step" -gt 0 ] || step=5
  while [ -f "$gwpid" ] && kill -0 "$(cat "$gwpid" 2>/dev/null)" 2>/dev/null; do
    ( exec 9>"$lock"; flock 9
      [ -r "$lease" ] && [ "$(jq -r ".holder//\"\"" "$lease")" = "$me" ] && [ "$(jq -r ".epoch//0" "$lease")" = "$epoch" ] || exit 0
      now=$(date +%s); t="$lease.t.$$"
      jq --argjson e "$((now+ttl))" ".expires=\$e" "$lease" >"$t" &&
      chown --reference="$lease" "$t" && chmod --reference="$lease" "$t" && mv "$t" "$lease"
    ) 2>/dev/null || true
    sleep "$step"
  done
  ( exec 9>"$lock"; flock 9
    [ -r "$lease" ] && [ "$(jq -r ".holder//\"\"" "$lease")" = "$me" ] && [ "$(jq -r ".epoch//0" "$lease")" = "$epoch" ] && rm -f "$lease"
  ) 2>/dev/null || true
  rm -f "$hbpid"
;;
*) echo "unknown staging lease action: $action" >&2; exit 2 ;;
esac
