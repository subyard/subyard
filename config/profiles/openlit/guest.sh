#!/usr/bin/env bash
# Reconcile only the marked yard-local OpenLIT stack and native telemetry leaves.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
state=/var/lib/subyard/openlit
unit=/etc/systemd/system/subyard-openlit.service
cutover_unit=/etc/systemd/system/subyard-openlit-trace-cutover.service
cutover_timer=/etc/systemd/system/subyard-openlit-trace-cutover.timer
lock_fd=
if [ "${1-}" = --runtime-lock-fd ]; then lock_fd=${2:?expected inherited descriptor}; shift 2; fi
mode=${1:?expected lifecycle mode}
selected=${2:?expected selected bit}
user=${3:?expected developer user}
agents=${4-}
case "$mode" in apply|check|pause|resume|observe|refresh) ;; *) exit 2 ;; esac
case "$selected" in 0|1) ;; *) exit 2 ;; esac
[[ "$user" =~ ^[a-zA-Z_][a-zA-Z0-9_-]*$ ]] || exit 2
[ "$(id -u)" = 0 ] || { echo 'openlit: guest root required' >&2; exit 1; }
home="$(getent passwd "$user" | cut -d: -f6)"
[ -n "$home" ] || exit 1
uid="$(id -u "$user")"; gid="$(id -g "$user")"
die() { echo "openlit: $*" >&2; exit 1; }
stage() { printf 'openlit: guest stage=%s\n' "$1" >&2; }
owned() {
  [ -d "$state" ] && [ ! -L "$state" ] && [ "$(stat -c '%u:%g:%a' "$state")" = 0:0:700 ] \
    && [ -f "$state/marker" ] && [ ! -L "$state/marker" ] \
    && [ "$(cat "$state/marker")" = subyard-openlit-v1 ]
}
safe_file() {
  [ ! -L "$1" ] && { [ ! -e "$1" ] || { [ -f "$1" ] && [ "$(stat -c %u "$1")" = 0 ]; }; }
}
compose() {
  local bound=(timeout 240)
  # Cold phases stay in the aggregate supervisor's group. GNU timeout's
  # default separate group would let them escape caller cancellation.
  [ "$mode" != apply ] || bound=(timeout --foreground 240)
  "${bound[@]}" docker compose --project-directory "$state" --env-file "$state/database.env" -f "$root/compose.yaml" "$@"
}
native() { python3 "$root/native.py" "$1" --home "$home" --state "$state/native.json" --uid "$uid" --gid "$gid" --agents "$2"; }
cutover() { python3 "$root/trace_cutover.py" "$1" --home "$home" --state "$state" --uid "$uid" --agents "$agents"; }
runtime() {
  python3 "$root/runtime.py" "$1" --source "$root" --state "$state" --unit "$unit" --home "$home" \
    --user "$user" --selected "$selected" --agents "$agents" "${@:2}"
}
account() { python3 "$root/account.py" "$1" --state "$state" --source "$root" >/dev/null; }
if [ -e "$state" ] || [ -L "$state" ]; then owned || die 'unmanaged runtime directory'; fi
safe_file "$unit" || die 'unsafe service unit'
if [ -e "$unit" ]; then
  [ "$(head -n 1 "$unit")" = '# Managed by Subyard OpenLIT profile.' ] || die 'unmanaged service unit'
fi
for timer_asset in "$cutover_unit" "$cutover_timer"; do
  safe_file "$timer_asset" || die 'unsafe trace migration unit'
  if [ -e "$timer_asset" ]; then
    [ "$(head -n 1 "$timer_asset")" = '# Managed by Subyard OpenLIT profile.' ] || die 'unmanaged trace migration unit'
  fi
done
if [ "$mode" = observe ]; then runtime observe; exit; fi
if [ "$mode" = check ]; then
  if ! owned; then [ "$selected" = 0 ]; exit; fi
  runtime observe | python3 -c 'import json,sys; sys.exit(0 if json.load(sys.stdin)["state"] == "current" else 1)'
  exit
fi
umask 0077
if [ -z "$lock_fd" ]; then
  exec python3 "$root/trace_cutover.py" locked-guest "$root/guest.sh" "$@"
fi
python3 "$root/trace_cutover.py" verify-lock "$lock_fd" || die 'unsafe runtime lock'
if [ "$mode" = refresh ] || [ "$#" = 6 ]; then
  [ "$#" = 6 ] || exit 2
  # Source recipe, native metadata and actual installed/container identities must still
  # match the approved observation before service or file mutations.
  stage guard
  runtime guard --actual "$5" --desired "$6" >/dev/null
fi
if [ "$mode" = pause ]; then
  owned || exit 0
  if systemctl is-active --quiet subyard-openlit.service; then
    systemctl stop subyard-openlit.service
    printf 'paused\n'
  fi
  exit
fi
if [ "$mode" = resume ]; then
  owned || exit 0
  [ "$selected" = 1 ] || exit 0
  systemctl start subyard-openlit.service
  runtime publish >/dev/null
  exit
fi
[ "$selected" != 0 ] || owned || exit 0
if [ "$selected" = 1 ]; then
  for agent in $agents; do
    # Selection owns configuration, not installation or execution of native agents.
    case "$agent" in claude|codex) ;; *) die 'unsupported native coding integration' ;; esac
  done
fi
command -v docker >/dev/null || die 'Docker Engine is missing; run yard init to reconcile core provisioning, then retry OpenLIT enable'
docker compose version >/dev/null || die 'Docker Compose is missing; run yard init to reconcile core provisioning, then retry OpenLIT enable'
if ! owned; then
  [ -z "$(docker ps -aq --filter label=com.docker.compose.project=subyard-openlit)" ] || die 'unmanaged Docker project'
  [ -z "$(docker volume ls -q --filter label=com.docker.compose.project=subyard-openlit)" ] || die 'unmanaged Docker volumes'
  install -d -m 0700 -o 0 -g 0 "$state"
  printf 'subyard-openlit-v1\n' > "$state/marker"
  chmod 0600 "$state/marker"
fi
for file in marker database.env guest.sh native.py runtime.py trace_cutover.py trace-cutover.env trace-cutover.json trace-cutover-backup.json trace-admission.json account.py account.json operator.json tomli_w.py compose.yaml collector.yaml openlit-collector.yaml native.json receipt.json pull.log; do
  safe_file "$state/$file" || die 'unsafe runtime file'
done
if [ ! -e "$state/database.env" ]; then
  python3 - "$state/database.env" <<'PY'
import os,secrets,sys
fd=os.open(sys.argv[1],os.O_CREAT|os.O_EXCL|os.O_WRONLY,0o600)
with os.fdopen(fd,'w') as out: out.write('OPENLIT_DB_PASSWORD='+secrets.token_hex(32)+'\n')
PY
fi
[ "$(stat -c '%u:%g:%a' "$state/database.env")" = 0:0:600 ] || die 'database environment must be root-private'
# Native conflicts fail before service or asset changes.
native validate "$([ "$selected" = 1 ] && printf '%s' "$agents")"
if [ "$selected" = 1 ]; then
  # The image digests are immutable. A complete local cache permits offline re-enable;
  # downloads finish before desired files replace the installed recipe.
  stage image-inventory
  mapfile -t images < <(compose config --images)
  [ "${#images[@]}" = 3 ] || die 'unexpected Compose image inventory'
  missing=0
  for image in "${images[@]}"; do docker image inspect "$image" >/dev/null 2>&1 || missing=1; done
  if [ "$missing" = 1 ]; then
    # First delivery retains its own limit inside the aggregate cold deadline.
    # Foreground timeout keeps cold children in the supervisor's owned group.
    pull_command=(docker compose --project-directory "$state" --env-file "$state/database.env" -f "$root/compose.yaml" pull --quiet)
    if [ "$mode" = refresh ]; then
      pull_limit=20
      pull_command=(timeout "$pull_limit" "${pull_command[@]}")
    else
      pull_limit=900
      pull_command=(timeout --foreground "$pull_limit" "${pull_command[@]}")
    fi
    stage image-pull
    if "${pull_command[@]}" > "$state/pull.log" 2>&1; then
      :
    else
      pull_status=$?
      printf 'openlit: pinned image pull exit=%s\n' "$pull_status" >&2
      die 'bounded pinned image pull failed; inspect root-private pull.log inside the yard'
    fi
  fi
fi
if [ "$selected" = 1 ]; then account prepare; fi
if [ "$selected" = 1 ]; then cutover prepare; fi
publish_asset() {
  local source="$1" destination="$2" permissions="$3" temporary
  if [ -f "$destination" ] && cmp -s "$source" "$destination" && [ "$(stat -c '%u:%g:%a' "$destination")" = "0:0:$permissions" ]; then return; fi
  temporary="$(mktemp "${destination}.XXXXXX")"
  install -m "$permissions" -o 0 -g 0 "$source" "$temporary"
  mv -f -- "$temporary" "$destination"
}
stage assets
for source in guest.sh native.py runtime.py trace_cutover.py account.py tomli_w.py compose.yaml openlit-collector.yaml subyard-openlit-trace-cutover.service subyard-openlit-trace-cutover.timer; do
  publish_asset "$root/$source" "$state/$source" 600
done
# The pinned Collector runs as 10001:10001. Only this nonsecret mounted asset
# is world-readable; its host parent and all credentials remain root-private.
publish_asset "$root/collector.yaml" "$state/collector.yaml" 644
publish_asset "$root/subyard-openlit.service" "$unit" 644
publish_asset "$root/subyard-openlit-trace-cutover.service" "$cutover_unit" 644
publish_asset "$root/subyard-openlit-trace-cutover.timer" "$cutover_timer" 644
systemctl daemon-reload
if [ "$selected" = 0 ]; then
  stage native-disable
  cutover cancel
  native apply ''
  systemctl disable --now subyard-openlit.service >/dev/null
  systemctl disable --now subyard-openlit-trace-cutover.timer >/dev/null
  # Retain the shared network for compatibility or foreign attached receivers.
  # Remove only the three services defined by this protected owning recipe.
  compose stop --timeout 20 clickhouse openlit collector >/dev/null
  compose rm --force clickhouse openlit collector >/dev/null
else
  wait_limit=180
  [ "$mode" != refresh ] || wait_limit=60
  # Recreate after every assessed repair, including equal Compose hashes with a changed
  # collector bind inode. Receipt publication is separate and occurs only after verification.
  stage compose-start
  compose up -d --force-recreate --pull never --wait --wait-timeout "$wait_limit" >/dev/null
  stage account-bootstrap
  account apply
  systemctl enable subyard-openlit.service >/dev/null
  stage unit-start
  systemctl start subyard-openlit.service
  stage native-enable
  native apply "$agents"
  # Unavailable process metadata defers fencing; unsafe protected state fails.
  cutover fence >/dev/null
  systemctl enable --now subyard-openlit-trace-cutover.timer >/dev/null
fi
stage publication
published="$(runtime publish)"
if [ "$mode" = refresh ]; then printf '%s\n' "$published"; else printf 'openlit: yard-local collector and OpenLIT are ready\n'; fi
