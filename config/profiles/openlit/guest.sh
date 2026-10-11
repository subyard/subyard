#!/usr/bin/env bash
# Reconcile only the marked yard-local OpenLIT stack and native telemetry leaves.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
state=/var/lib/subyard/openlit
unit=/etc/systemd/system/subyard-openlit.service
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
owned() {
  [ -d "$state" ] && [ ! -L "$state" ] && [ "$(stat -c '%u:%g:%a' "$state")" = 0:0:700 ] \
    && [ -f "$state/marker" ] && [ ! -L "$state/marker" ] \
    && [ "$(cat "$state/marker")" = subyard-openlit-v1 ]
}
safe_file() {
  [ ! -L "$1" ] && { [ ! -e "$1" ] || { [ -f "$1" ] && [ "$(stat -c %u "$1")" = 0 ]; }; }
}
compose() { timeout 240 docker compose --project-directory "$state" --env-file "$state/database.env" -f "$root/compose.yaml" "$@"; }
native() { python3 "$root/native.py" "$1" --home "$home" --state "$state/native.json" --uid "$uid" --gid "$gid" --agents "$2"; }
runtime() {
  python3 "$root/runtime.py" "$1" --source "$root" --state "$state" --unit "$unit" --home "$home" \
    --user "$user" --selected "$selected" --agents "$agents" "${@:2}"
}
if [ -e "$state" ] || [ -L "$state" ]; then owned || die 'unmanaged runtime directory'; fi
safe_file "$unit" || die 'unsafe service unit'
if [ -e "$unit" ]; then
  [ "$(head -n 1 "$unit")" = '# Managed by Subyard OpenLIT profile.' ] || die 'unmanaged service unit'
fi
if [ "$mode" = observe ]; then runtime observe; exit; fi
if [ "$mode" = check ]; then
  if ! owned; then [ "$selected" = 0 ]; exit; fi
  runtime observe | python3 -c 'import json,sys; sys.exit(0 if json.load(sys.stdin)["state"] == "current" else 1)'
  exit
fi
umask 0077
safe_file /run/lock/subyard-openlit.lock || die 'unsafe runtime lock'
exec 9>/run/lock/subyard-openlit.lock
chmod 0600 /run/lock/subyard-openlit.lock
flock -w 5 9 || die 'runtime is being reconciled'
if [ "$mode" = refresh ]; then
  [ "$#" = 6 ] || exit 2
  # Source recipe, native metadata and actual installed/container identities must still
  # match the approved observation before service or file mutations.
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
    case "$agent" in claude|codex) command -v "$agent" >/dev/null || die 'selected native coding integration is not installed' ;; *) die 'unsupported native coding integration' ;; esac
  done
fi
command -v docker >/dev/null && docker compose version >/dev/null || die 'core yard Docker Compose provisioning is required'
if ! owned; then
  [ -z "$(docker ps -aq --filter label=com.docker.compose.project=subyard-openlit)" ] || die 'unmanaged Docker project'
  [ -z "$(docker volume ls -q --filter label=com.docker.compose.project=subyard-openlit)" ] || die 'unmanaged Docker volumes'
  install -d -m 0700 -o 0 -g 0 "$state"
  printf 'subyard-openlit-v1\n' > "$state/marker"
  chmod 0600 "$state/marker"
fi
for file in marker database.env guest.sh native.py runtime.py tomli_w.py compose.yaml collector.yaml native.json receipt.json pull.log; do
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
  mapfile -t images < <(compose config --images)
  [ "${#images[@]}" = 3 ] || die 'unexpected Compose image inventory'
  missing=0
  for image in "${images[@]}"; do docker image inspect "$image" >/dev/null 2>&1 || missing=1; done
  if [ "$missing" = 1 ]; then
    pull_limit=240
    [ "$mode" != refresh ] || pull_limit=20
    if ! timeout "$pull_limit" docker compose --project-directory "$state" --env-file "$state/database.env" -f "$root/compose.yaml" pull --quiet > "$state/pull.log" 2>&1; then
      die 'bounded pinned image pull failed; inspect root-private pull.log inside the yard'
    fi
  fi
fi
publish_asset() {
  local source="$1" destination="$2" permissions="$3" temporary
  if [ -f "$destination" ] && cmp -s "$source" "$destination" && [ "$(stat -c '%u:%g:%a' "$destination")" = "0:0:$permissions" ]; then return; fi
  temporary="$(mktemp "${destination}.XXXXXX")"
  install -m "$permissions" -o 0 -g 0 "$source" "$temporary"
  mv -f -- "$temporary" "$destination"
}
for source in guest.sh native.py runtime.py tomli_w.py compose.yaml; do
  publish_asset "$root/$source" "$state/$source" 600
done
# The pinned Collector runs as 10001:10001. Only this nonsecret mounted asset
# is world-readable; its host parent and all credentials remain root-private.
publish_asset "$root/collector.yaml" "$state/collector.yaml" 644
publish_asset "$root/subyard-openlit.service" "$unit" 644
systemctl daemon-reload
if [ "$selected" = 0 ]; then
  native apply ''
  systemctl disable --now subyard-openlit.service >/dev/null
  compose down --timeout 20 >/dev/null
else
  wait_limit=180
  [ "$mode" != refresh ] || wait_limit=60
  # Recreate after every assessed repair, including equal Compose hashes with a changed
  # collector bind inode. Receipt publication is separate and occurs only after verification.
  compose up -d --force-recreate --pull never --wait --wait-timeout "$wait_limit" >/dev/null
  systemctl enable subyard-openlit.service >/dev/null
  systemctl start subyard-openlit.service
  native apply "$agents"
fi
published="$(runtime publish)"
if [ "$mode" = refresh ]; then printf '%s\n' "$published"; else printf 'openlit: yard-local collector and OpenLIT are ready\n'; fi
