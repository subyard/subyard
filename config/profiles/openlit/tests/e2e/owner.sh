#!/usr/bin/env bash
# Real-host provisioning acceptance for openlit on the allocated owner VM.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." && pwd -P)"
STATE=''
YARD_NAME=''
CANDIDATE_ENGINE=''
tunnel_pid=''
synthetic_address=''
occupied_pid=''
secondary_name=''
kind=${1:-container}
[ "$#" -le 2 ] && { [ "$kind" = container ] || [ "$kind" = vm ]; } || exit 2
auth_only=${2:-}
case "$auth_only" in ""|auth-only|teardown-only) ;; *) exit 2 ;; esac
mapfile -t diagnostic_images < <(sed -n 's/^    image: //p' "$ROOT/config/profiles/openlit/compose.yaml")

die() { printf 'openlit-e2e: %s\n' "$*" >&2; exit 2; }
[ "${SUBYARD_E2E_VM:-}" = 1 ] || die 'run on VM1 through dev/agent-e2e.sh'
[ "$(id -un)" = dev ] || die 'run as the allocated dev user'
for command in go incus jq sudo ip ssh ssh-keygen unshare nsenter python3; do
  command -v "$command" >/dev/null 2>&1 || die "$command is required"
done
sudo -n true || die 'passwordless sudo is required on the disposable VM'
[ -x /usr/sbin/sshd ] || die 'synthetic owner requires OpenSSH server'
sudo -n unshare --net --fork bash -euc 'ip link set lo up; nsenter --net=/proc/self/ns/net -- true' || die 'isolated client namespace is unavailable'
# Two live stacks and nested VM routing need the supported larger Linux allocation.
python3 - "${SUBYARD_E2E_MEMORY_PER_VM:-}" <<'MEMORY'
import re,sys
value=re.fullmatch(r'([1-9][0-9]*)(MiB|GiB)',sys.argv[1])
assert value and int(value[1])*(1024 if value[2]=='GiB' else 1)>=8192, 'OpenLIT acceptance requires at least 8GiB'
MEMORY


incus() {
  if [ -S /var/lib/incus/unix.socket ] && [ ! -w /var/lib/incus/unix.socket ]; then
    sudo -n /usr/bin/incus "$@"
  else
    /usr/bin/incus "$@"
  fi
}
candidate() {
  local engine="${CANDIDATE_ENGINE:-$ROOT/.build/yard}" command
  # Init refreshes its child session; the fixture's parent keeps the old groups.
  if ! id -nG | tr ' ' '\n' | grep -Fxq incus-admin \
    && id -nG "$(id -un)" | tr ' ' '\n' | grep -Fxq incus-admin; then
    printf -v command '%q ' "$engine" "$@"
    sg incus-admin -c "exec $command"
  else
    "$engine" "$@"
  fi
}
yard() { candidate -Y "$YARD_NAME" "$@"; }

retained_data() {
  incus exec "$instance" --project "$project" -- python3 - <<'PYCODE'
import json, subprocess
from pathlib import Path
state = Path('/var/lib/subyard/openlit')
compose = ['docker', 'compose', '--env-file', str(state/'database.env'), '-f', str(state/'compose.yaml')]
def count(table):
    return int(subprocess.check_output([*compose, 'exec', '-T', 'clickhouse', 'bash', '-c',
        'exec clickhouse-client --user openlit --password "$CLICKHOUSE_PASSWORD" --database openlit --query "$1"',
        'fixture', 'SELECT count() FROM '+table], timeout=10, text=True))
volumes = subprocess.check_output(['docker', 'volume', 'ls', '-q', '--filter',
    'label=com.docker.compose.project=subyard-openlit'], timeout=10, text=True).split()
print(json.dumps({'volumes': sorted(volumes), 'logs': count('otel_logs'),
                  'metrics': count('otel_metrics_sum')+count('otel_metrics_histogram')}))
PYCODE
}

cleanup() {
  local rc=$?
  trap - EXIT INT TERM
  set +e
  if [ "$rc" -ne 0 ] && [ -n "$YARD_NAME" ]; then
    printf 'openlit_e2e_failure_exit=%s kind=%s\n' "$rc" "$kind" >&2
    incus list "yard-$YARD_NAME" --project "subyard-$YARD_NAME" -f csv -c ns >&2
    incus exec "yard-$YARD_NAME" --project "subyard-$YARD_NAME" -- systemctl show subyard-openlit.service --property=ActiveState,UnitFileState >&2
    incus exec "yard-$YARD_NAME" --project "subyard-$YARD_NAME" -- docker ps -a --format '{{.Names}} {{.Status}}' >&2
    incus exec "yard-$YARD_NAME" --project "subyard-$YARD_NAME" -- python3 - failure "${diagnostic_images[@]}" \
      < "$ROOT/config/profiles/openlit/tests/e2e/failure-facts.py" >&2
  fi
  if [ -n "$secondary_name" ] && grep -Fqx '# openlit-profile-second-yard' "$SUBYARD_CONFIG_HOME/yards/$secondary_name/config.env"; then
    (YARD_NAME="$secondary_name"; yard teardown --yes) > "$STATE/secondary-cleanup.log" 2>&1 || rc=3
  fi
  if [ -n "$occupied_pid" ]; then kill "$occupied_pid" 2>/dev/null; wait "$occupied_pid" 2>/dev/null; fi
  if [ -n "$tunnel_pid" ]; then kill "$tunnel_pid" 2>/dev/null; wait "$tunnel_pid" 2>/dev/null; fi
  if [ -n "$synthetic_address" ]; then sudo -n ip address del "$synthetic_address/32" dev lo || rc=3; fi
  if [ -n "$YARD_NAME" ] && [ -f "${SUBYARD_CONFIG_HOME:-}/yards/$YARD_NAME/config.env" ]; then
    install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/platform-sentinel"
    printf 'SSH_PORT=64998\n' > "$SUBYARD_CONFIG_HOME/yards/platform-sentinel/config.env"
    chmod 0600 "$SUBYARD_CONFIG_HOME/yards/platform-sentinel/config.env"
    printf 'openlit_e2e_cleanup_stage=primary-teardown\n' >&2
    if yard teardown --yes > "$STATE/primary-cleanup.log" 2>&1; then
      printf 'openlit_e2e_cleanup_stage=primary-teardown result=passed\n' >&2
    else
      rc=3
      printf 'openlit_e2e_cleanup_stage=primary-teardown result=failed\n' >&2
      tail -30 "$STATE/primary-cleanup.log" >&2
    fi
  fi
  if [ -n "$STATE" ] && [[ "$STATE" = /var/tmp/subyard-openlit.* ]] \
    && [ -f "$STATE/.marker" ] \
    && [ "$(<"$STATE/.marker")" = openlit-profile-e2e-v1 ]; then
    if sudo -n find "$STATE" -depth -delete; then
      printf 'openlit_e2e_cleanup_stage=state-removal result=passed\n' >&2
    else
      printf 'openlit_e2e_cleanup_stage=state-removal result=failed\n' >&2
      rc=3
    fi
  fi
  exit "$rc"
}
trap cleanup EXIT INT TERM

STATE="$(mktemp -d /var/tmp/subyard-openlit.XXXXXX)"
printf '%s\n' openlit-profile-e2e-v1 > "$STATE/.marker"
token="$(printf '%s' "${STATE##*.}" | tr '[:upper:]' '[:lower:]')"
YARD_NAME="dev-e2e-$token"
export SUBYARD_OPERATOR_HOME="$HOME"
export SUBYARD_CONFIG_HOME="$STATE/config"
export SUBYARD_HOME="$STATE/data"
export STORAGE_PATH="$HOME/.cache/subyard-e2e-platform/incus/incus/storage"
export SUBYARD_NO_AUDIT=1 SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1 MIN_DISK_GIB=1
install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME"

# A release run must exercise the frozen packaged runtime. Ordinary development
# acceptance has no metadata and builds the current development engine instead.
# shellcheck source=tests/helpers/release-candidate.sh
. "$ROOT/tests/helpers/release-candidate.sh"
set +e
CANDIDATE_ENGINE="$(release_candidate_prepare "$ROOT")"
candidate_status=$?
set -e
case "$candidate_status" in
  0) ;;
  1) "$ROOT/dev/build-engine.sh" --force; CANDIDATE_ENGINE="$ROOT/.build/yard" ;;
  *) die 'frozen release candidate is invalid' ;;
esac
port=$((35000 + ($$ % 15000)))
cat > "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME/config.env" <<EOF
YARD_KIND=$kind
SSH_PORT=$port
CODING_TOOL_INTEGRATIONS="claude codex"
ENVIRONMENT_PROFILES=
HOST_BASE=$STATE/host
RESTRICTED_DISK_PATHS=$STATE/host
FORWARD_SSH_AGENT=0
EOF
if [ "$kind" = vm ]; then printf 'HOST_MOUNTS=\nHOST_LINKS=\nLIMITS_MEMORY=4GiB\nLIMITS_CPU=2\n' >> "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME/config.env"; fi
chmod 0600 "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME/config.env"

printf 'openlit_e2e_stage=default-off-init\n'
yard init --yes
instance="yard-$YARD_NAME"
project="subyard-$YARD_NAME"
[ "$(incus list "$instance" --project "$project" --format csv -c s)" = STOPPED ] \
  || die 'named yard did not finish init stopped'
[ "$(incus config get "$instance" user.subyard.desired_power --project "$project")" = stopped ] \
  || die 'named yard did not preserve stopped power intent'
yard start --yes
incus exec "$instance" --project "$project" -- test ! -e /var/lib/subyard/openlit
printf 'openlit_e2e_stage=native-cli-prerequisites\n'
# Claude selection materializes configuration but has no shipped installer hook.
# Pin official native artifacts verified against the signed release manifest:
# https://downloads.claude.ai/claude-code-releases/2.1.287/manifest.json
incus exec "$instance" --project "$project" -- bash -euo pipefail -s <<'CLAUDE_CLI'
version=2.1.287
case "$(dpkg --print-architecture)" in
  amd64) platform=linux-x64; checksum=3920489a5109cff5786a1a392c25277408ff22bc796d5edb9c16a60e5a1718f0 ;;
  arm64) platform=linux-arm64; checksum=e4daf793d1e74fb0d9874dd09e98690bbfd7be515f78a87fd05b9e2b4bb33b03 ;;
  *) echo 'openlit-e2e: unsupported Claude native architecture' >&2; exit 1 ;;
esac
[ ! -e /usr/local/bin/claude ]
[ ! -L /usr/local/bin/claude ]
stage="$(mktemp -d /tmp/subyard-openlit-claude.XXXXXX)"
trap 'rm -rf -- "$stage"' EXIT
curl --proto '=https' --proto-redir '=https' --tlsv1.2 --fail --silent --show-error --location \
  --connect-timeout 10 --max-time 180 \
  "https://downloads.claude.ai/claude-code-releases/$version/$platform/claude" -o "$stage/claude"
printf '%s  %s\n' "$checksum" "$stage/claude" | sha256sum --check --status
install -m 0755 -o root -g root "$stage/claude" /usr/local/bin/claude
cd /tmp
# Match the real native signals' HOME/PATH; also verify the root profile guard's lookup.
export PATH=/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
for agent in claude codex; do
  command -v "$agent" >/dev/null
  actual="$(runuser -u dev -- env -i HOME=/home/dev PATH="$PATH" DISABLE_AUTOUPDATER=1 \
    timeout 15 "$agent" --version)"
  [ "$agent" != claude ] || [ "$actual" = "$version (Claude Code)" ]
  printf 'native_prerequisite=%s version=%s\n' "$agent" "$actual"
done
CLAUDE_CLI
# Read-only transport/cache facts do not pull images or replace the native cold
# enable deadline. HTTP reachability alone does not prove image/blob downloads.
incus exec "$instance" --project "$project" -- python3 - preflight "${diagnostic_images[@]}" \
  < "$ROOT/config/profiles/openlit/tests/e2e/failure-facts.py"
printf 'openlit_e2e_stage=enable\n'
yard profile enable openlit --yes
if [ "$auth_only" = teardown-only ]; then printf 'ok: enabled profile ready for exact teardown regression\n'; exit 0; fi
printf 'openlit_e2e_stage=published-dashboard-origin\n'
dashboard_port=$((1024 + (port+20000-1024)%64512))
origin="http://127.0.0.1:$dashboard_port"
yard status > "$STATE/dashboard-status"
grep -Fq "$origin/" "$STATE/dashboard-status" || die 'status omitted the published OpenLIT dashboard'
if [ "$kind" = vm ]; then
  ! incus config device list "$instance" --project "$project" | grep -Fxq openlit-dashboard || die 'VM profile published an owner proxy'
  grep -Fq "$dashboard_port:127.0.0.1:3000" "$STATE/dashboard-status" || die 'VM status omitted guest SSH forward'
  ssh -N -o BatchMode=yes -o ExitOnForwardFailure=yes -L "$dashboard_port:127.0.0.1:3000" "yard-$YARD_NAME" > "$STATE/ui-tunnel.log" 2>&1 &
  tunnel_pid=$!
  for ((attempt=0;attempt<40;attempt++)); do
    if curl --silent --fail --max-time 1 "$origin/api/auth/csrf" >/dev/null; then break; fi
    kill -0 "$tunnel_pid" 2>/dev/null || die 'VM SSH dashboard tunnel exited'
    sleep 0.25
  done
  python3 "$ROOT/config/profiles/openlit/tests/e2e/ui-origin.py" --origin "$origin" --state "$STATE/ui-cookies" --identity "$token"
else
incus query "/1.0/instances/$instance?project=$project" | jq -e --arg listen "tcp:127.0.0.1:$dashboard_port"   '.devices["openlit-dashboard"] == {type:"proxy",bind:"host",listen:$listen,connect:"tcp:127.0.0.1:3000"}' >/dev/null   || die 'profile enable did not publish exactly the owned UI route'
python3 "$ROOT/config/profiles/openlit/tests/e2e/ui-origin.py" --origin "$origin" --state "$STATE/ui-cookies" --identity "$token"
printf 'openlit_e2e_stage=private-owner-address-and-ssh-origin\n'
synthetic_address=100.101.102.103
sudo -n ip address add "$synthetic_address/32" dev lo
mkdir -p "$STATE/bin"
printf '#!/bin/sh\nprintf "100.101.102.103\\n"\n' > "$STATE/bin/tailscale"
chmod 0755 "$STATE/bin/tailscale"
export PATH="$STATE/bin:$PATH"
yard init --yes
private_origin="http://$synthetic_address:$dashboard_port"
yard status > "$STATE/private-dashboard-status"
grep -Fq "$private_origin/" "$STATE/private-dashboard-status" || die 'owner address drift was not reconciled'
python3 "$ROOT/config/profiles/openlit/tests/e2e/ui-origin.py" --origin "$private_origin" --state "$STATE/private-ui-cookies" --identity "$token-private"
# A real guest SSH connection proves Host-based auth through a second browser origin.
tunnel_port=$((1024 + (dashboard_port+1-1024)%64512))
ssh -N -o BatchMode=yes -o ExitOnForwardFailure=yes -L "$tunnel_port:127.0.0.1:3000" "yard-$YARD_NAME" > "$STATE/ui-tunnel.log" 2>&1 &
tunnel_pid=$!
for ((attempt=0;attempt<40;attempt++)); do
  if curl --silent --fail --max-time 1 "http://127.0.0.1:$tunnel_port/api/auth/csrf" >/dev/null; then break; fi
  kill -0 "$tunnel_pid" 2>/dev/null || die 'SSH dashboard tunnel exited'
  sleep 0.25
done
python3 "$ROOT/config/profiles/openlit/tests/e2e/ui-origin.py" --origin "http://127.0.0.1:$tunnel_port" --state "$STATE/tunnel-ui-cookies" --identity "$token-tunnel"
kill "$tunnel_pid"; wait "$tunnel_pid" 2>/dev/null || true
tunnel_pid=''
sudo -n ip address del "$synthetic_address/32" dev lo
printf 'openlit_e2e_stage=deferred-publication-and-restoration\n'
candidate _publications "$project" "$instance" > "$STATE/deferred-publications"
incus query "/1.0/instances/$instance?project=$project" | jq -e --arg marker "v2:$synthetic_address:$dashboard_port" \
  '.config["user.subyard.openlit_dashboard"] == $marker and .devices["openlit-dashboard"] == null
   and (.config["user.subyard.deferred_publication.openlit-dashboard"] | fromjson | .version == 1 and .ownership == $marker)' >/dev/null \
  || die 'native publication did not preserve the exact deferred dashboard ownership'
yard init --yes
yard status > "$STATE/deferred-dashboard-status"
! grep -Fq "$private_origin/" "$STATE/deferred-dashboard-status" || die 'deferred dashboard exposed a working URL'
sudo -n ip address add "$synthetic_address/32" dev lo
candidate _publications "$project" "$instance" > "$STATE/restored-publications"
yard status | grep -Fq "$private_origin/" || die 'exact dashboard restoration did not publish its original URL'
python3 "$ROOT/config/profiles/openlit/tests/e2e/ui-origin.py" --origin "$private_origin" --state "$STATE/private-ui-cookies" --identity "$token-private" --resume
# Explicit owning cancellation revokes restoration before choosing a different
# origin. A valid deferred recipe is never silently replaced by loopback.
sudo -n ip address del "$synthetic_address/32" dev lo
candidate _publications "$project" "$instance" > "$STATE/cancel-publications"
yard profile disable openlit --yes
[ -z "$(incus config get "$instance" user.subyard.deferred_publication.openlit-dashboard --project "$project")" ] \
  || die 'profile cancellation retained dashboard restoration authority'
synthetic_address=''
yard profile enable openlit --yes
yard init --yes
yard status | grep -Fq "$origin/" || die 'loopback fallback drift was not reconciled'
printf 'openlit_e2e_stage=port-drift-and-refusal\n'
native_before="$(incus exec "$instance" --project "$project" -- sha256sum /var/lib/subyard/openlit/receipt.json)"
alternate_port=$((1024 + (dashboard_port+2-1024)%64512))
yard config set OPENLIT_HOST_PORT "$alternate_port" --scope yard --local --yes
yard init --yes
yard status | grep -Fq "http://127.0.0.1:$alternate_port/" || die 'explicit dashboard port drift was not reconciled'
[ "$(incus exec "$instance" --project "$project" -- sha256sum /var/lib/subyard/openlit/receipt.json)" = "$native_before" ] || die 'route-only drift recreated the native stack'
yard config unset OPENLIT_HOST_PORT --scope yard --local --yes
yard init --yes
incus config device set "$instance" openlit-dashboard connect=tcp:127.0.0.1:39999 --project "$project"
if yard init --yes > "$STATE/foreign-route.log" 2>&1; then die 'foreign/divergent dashboard route was accepted'; fi
[ "$(incus exec "$instance" --project "$project" -- sha256sum /var/lib/subyard/openlit/receipt.json)" = "$native_before" ] || die 'foreign route refusal changed native runtime'
incus config device set "$instance" openlit-dashboard connect=tcp:127.0.0.1:3000 --project "$project"
yard init --yes
incus config device remove "$instance" openlit-dashboard --project "$project"
python3 - "$dashboard_port" "$STATE/occupied-ready" <<'PYPORT' &
import pathlib,socket,sys,time
with socket.socket() as listener:
    listener.bind(('127.0.0.1',int(sys.argv[1])))
    listener.listen()
    pathlib.Path(sys.argv[2]).write_text('ready')
    time.sleep(60)
PYPORT
occupied_pid=$!
for ((attempt=0;attempt<40;attempt++)); do
  [ ! -f "$STATE/occupied-ready" ] || break
  kill -0 "$occupied_pid" 2>/dev/null || die 'occupied listener fixture exited'
  sleep 0.1
done
[ -f "$STATE/occupied-ready" ] || die 'occupied listener fixture did not become ready'
if yard init --yes > "$STATE/occupied-route.log" 2>&1; then die 'occupied dashboard listener was accepted'; fi
[ "$(incus exec "$instance" --project "$project" -- sha256sum /var/lib/subyard/openlit/receipt.json)" = "$native_before" ] || die 'occupied route refusal changed native runtime'
kill "$occupied_pid"; wait "$occupied_pid" 2>/dev/null || true
occupied_pid=''
yard init --yes
yard status | grep -Fq "$origin/" || die 'owned missing-device recovery did not converge'

fi
if [ "$kind" = container ]; then
  printf 'openlit_e2e_stage=two-live-yards-and-port-reservation\n'
  secondary_name="$YARD_NAME-second"
  secondary_instance="yard-$secondary_name"
  secondary_project="subyard-$secondary_name"
  secondary_ssh=$((port+3))
  secondary_port=$((1024 + (secondary_ssh+20000-1024)%64512))
  install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/$secondary_name"
  cat > "$SUBYARD_CONFIG_HOME/yards/$secondary_name/config.env" <<SECOND
# openlit-profile-second-yard
YARD_KIND=container
SSH_PORT=$secondary_ssh
CODING_TOOL_INTEGRATIONS=
ENVIRONMENT_PROFILES=
HOST_BASE=$STATE/second-host
RESTRICTED_DISK_PATHS=$STATE/second-host
HOST_LINKS=
FORWARD_SSH_AGENT=0
OPENLIT_HOST_PORT=$dashboard_port
SECOND
  chmod 0600 "$SUBYARD_CONFIG_HOME/yards/$secondary_name/config.env"
  (YARD_NAME="$secondary_name"; yard init --yes; yard start --yes)
  if (YARD_NAME="$secondary_name"; yard profile enable openlit --yes) > "$STATE/second-collision.log" 2>&1; then die 'second yard claimed the first dashboard listener'; fi
  incus exec "$secondary_instance" --project "$secondary_project" -- test ! -e /var/lib/subyard/openlit
  printf 'openlit_e2e_stage=stopped-peer-port-reservation\n'
  first_marker="$(incus config get "$instance" user.subyard.openlit_dashboard --project "$project")"
  yard stop --yes
  [ "$(incus list "$instance" --project "$project" -f csv -c s)" = STOPPED ] || die 'first dashboard owner did not stop'
  [ "$(incus config get "$instance" user.subyard.openlit_dashboard --project "$project")" = "$first_marker" ] || die 'stopping the first yard lost its reservation'
  python3 - "$dashboard_port" <<'FREE_LISTENER'
import socket,sys
with socket.socket() as listener:
    listener.bind(('127.0.0.1',int(sys.argv[1])))
FREE_LISTENER
  if (YARD_NAME="$secondary_name"; yard profile enable openlit --yes) > "$STATE/second-stopped-collision.log" 2>&1; then die 'second project claimed a stopped peer dashboard listener'; fi
  grep -Fq 'openlit: failure stage=observe category=ValueError' "$STATE/second-stopped-collision.log" || die 'stopped-peer refusal did not precede guest reconciliation'
  incus exec "$secondary_instance" --project "$secondary_project" -- test ! -e /var/lib/subyard/openlit
  yard start --yes
  for ((attempt=0;attempt<60;attempt++)); do
    if curl --silent --fail --max-time 1 "$origin/api/auth/csrf" >/dev/null; then break; fi
    sleep 1
  done
  curl --silent --fail --max-time 2 "$origin/api/auth/csrf" >/dev/null || die 'reserved first dashboard failed to restart'
  [ "$(incus config get "$instance" user.subyard.openlit_dashboard --project "$project")" = "$first_marker" ] || die 'peer refusal changed the first reservation'
  [ "$(incus exec "$instance" --project "$project" -- sha256sum /var/lib/subyard/openlit/receipt.json)" = "$native_before" ] || die 'peer refusal changed the first native stack'
  python3 "$ROOT/config/profiles/openlit/tests/e2e/ui-origin.py" --origin "$origin" --state "$STATE/ui-cookies" --identity "$token" --resume
  if [ "$auth_only" = auth-only ]; then
    (YARD_NAME="$secondary_name"; yard teardown --yes)
    secondary_name=''
    printf 'ok: published owner/SSH auth and stopped cross-project reservation regression\n'
    exit 0
  fi
  (YARD_NAME="$secondary_name"; yard config unset OPENLIT_HOST_PORT --scope yard --local --yes)
  # Reuse exactly the pinned image bytes, without copying settings, credentials or history.
  mapfile -t stack_images < <(sed -n 's/^    image: //p' "$ROOT/config/profiles/openlit/compose.yaml")
  [ "${#stack_images[@]}" = 3 ] || die 'unexpected pinned image inventory'
  incus exec "$instance" --project "$project" -- docker image save "${stack_images[@]}" | \
    incus exec "$secondary_instance" --project "$secondary_project" -- docker image load >/dev/null
  (YARD_NAME="$secondary_name"; yard profile enable openlit --yes; yard status) > "$STATE/second-status"
  grep -Fq "http://127.0.0.1:$secondary_port/" "$STATE/second-status" || die 'second yard dashboard is missing'
  python3 "$ROOT/config/profiles/openlit/tests/e2e/ui-origin.py" --origin "http://127.0.0.1:$secondary_port" --state "$STATE/second-cookies" --identity "$token-second"
  python3 "$ROOT/config/profiles/openlit/tests/e2e/ui-origin.py" --origin "$origin" --state "$STATE/ui-cookies" --identity "$token" --resume
  for route_instance in "$instance" "$secondary_instance"; do
    route_project="subyard-${route_instance#yard-}"
    incus query "/1.0/instances/$route_instance?project=$route_project" | jq -e \
      '[.devices[] | select(.type=="proxy") | .connect | select(test(":(4317|4318|13133|8123|9000)$"))] | length==0' >/dev/null \
      || die 'owner-side telemetry or database publication appeared'
  done
  (YARD_NAME="$secondary_name"; yard teardown --yes)
  secondary_name=''
fi
printf 'openlit_e2e_stage=remote-owner-rpc-and-browser-tunnel\n'
if [ -n "$tunnel_pid" ]; then kill "$tunnel_pid"; wait "$tunnel_pid" 2>/dev/null || true; tunnel_pid=''; fi
python3 "$ROOT/config/profiles/openlit/tests/e2e/remote-owner.py" "$STATE" "$CANDIDATE_ENGINE" "$YARD_NAME" "$dashboard_port" "$token"
if [ "$kind" = vm ]; then
  ssh -N -o BatchMode=yes -o ExitOnForwardFailure=yes -L "$dashboard_port:127.0.0.1:3000" "yard-$YARD_NAME" > "$STATE/ui-tunnel.log" 2>&1 &
  tunnel_pid=$!
fi
# The guest uses only synthetic prompts and local fake model APIs; no account credentials.
tar -C "$ROOT/config/profiles/openlit/tests/e2e" -cf - native-signals.py runtime-refresh.py | \
  incus exec "$instance" --project "$project" -- bash -euo pipefail -c '
    stage="$(mktemp -d /tmp/subyard-openlit-signals.XXXXXX)"
    trap '\''rm -rf -- "$stage"'\'' EXIT
    tar -xf - -C "$stage"
    python3 "$stage/native-signals.py"
    python3 "$stage/runtime-refresh.py"
  '
printf 'openlit_e2e_stage=repeat-init-and-configs\n'
yard init --yes
yard config apply --yes
yard profile status openlit --json >/dev/null
retained_data > "$STATE/retained-before.json"
chmod 0600 "$STATE/retained-before.json"
printf 'openlit_e2e_stage=disable\n'
yard profile disable openlit --yes
[ -z "$(incus config get "$instance" user.subyard.openlit_dashboard --project "$project")" ] || die 'disable retained dashboard receipt'
! incus config device list "$instance" --project "$project" | grep -Fxq openlit-dashboard || die 'disable retained dashboard publication'
incus exec "$instance" --project "$project" -- bash -euo pipefail -c '
  ! systemctl is-active --quiet subyard-openlit.service
  ! systemctl is-enabled --quiet subyard-openlit.service
  python3 - <<"PYCODE"
import json,tomllib
from pathlib import Path
state=json.loads(Path("/var/lib/subyard/openlit/native.json").read_text())
assert state["agents"] == {}
claude=json.loads(Path("/home/dev/.claude/settings.json").read_text())
codex=tomllib.loads(Path("/home/dev/.codex/config.toml").read_text())
assert "CLAUDE_CODE_ENABLE_TELEMETRY" not in claude.get("env",{})
assert "exporter" not in codex.get("otel",{})
assert codex["model"] == "fixture-custom-preserved"
assert claude["env"]["OPENLIT_E2E_CUSTOM"] == "preserved"
PYCODE
'
yard init --yes
printf 'openlit_e2e_stage=re-enable-retained-data\n'
yard profile enable openlit --yes
python3 "$ROOT/config/profiles/openlit/tests/e2e/ui-origin.py" --origin "$origin" --state "$STATE/ui-cookies" --identity "$token" --resume
retained_data > "$STATE/retained-after.json"
chmod 0600 "$STATE/retained-after.json"
python3 - "$STATE/retained-before.json" "$STATE/retained-after.json" <<'PYCODE'
import json, sys
from pathlib import Path
before, after = [json.loads(Path(path).read_text()) for path in sys.argv[1:]]
assert len(before['volumes']) == 2 and after['volumes'] == before['volumes']
assert after['logs'] >= before['logs'] > 0
assert after['metrics'] >= before['metrics'] > 0
PYCODE
yard profile disable openlit --yes
printf 'ok: OpenLIT default off, enable, receiver CAS update/rollback, repeat init/config apply, native logs+metrics ingestion, disable and retained data after re-enable\n'
