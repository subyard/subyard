#!/usr/bin/env bash
# Real-host provisioning acceptance for openlit on the allocated owner VM.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." && pwd -P)"
STATE=''
YARD_NAME=''
CANDIDATE_ENGINE=''
tunnel_pid=''
synthetic_address=''
synthetic_address_active=''
occupied_pid=''
secondary_name=''
kind=${1:-container}
[ "$#" -le 2 ] && { [ "$kind" = container ] || [ "$kind" = vm ]; } || exit 2
auth_only=${2:-}
case "$auth_only" in ""|auth-only|teardown-only|dependency-only|network-only|pull-diagnostic) ;; *) exit 2 ;; esac
[ "$auth_only" != network-only ] || [ "$kind" = container ] || exit 2
[ "$auth_only" != pull-diagnostic ] || [ "$kind" = container ] || exit 2
mapfile -t diagnostic_images < <(sed -n 's/^    image: //p' "$ROOT/config/profiles/openlit/compose.yaml")
# shellcheck source=config/profiles/openlit/tests/e2e/candidate.sh
. "$ROOT/config/profiles/openlit/tests/e2e/candidate.sh"

die() { printf 'openlit-e2e: %s\n' "$*" >&2; exit 2; }
[ "${SUBYARD_E2E_VM:-}" = 1 ] || die 'run on VM1 through dev/agent-e2e.sh'
[ "$(id -un)" = dev ] || die 'run as the allocated dev user'
for command in go incus jq sg sudo ip ssh ssh-keygen unshare nsenter python3; do
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
  # Keep file pulls owned by this operator even when enrollment refreshed only
  # the account database and this shell still has its original groups.
  openlit_candidate_command /usr/bin/incus "$@"
}
reexec_with_incus_group() {
  local command
  printf -v command 'exec env SUBYARD_E2E_VM=%q bash %q %q %q' \
    "$SUBYARD_E2E_VM" "$ROOT/config/profiles/openlit/tests/e2e/owner.sh" "$kind" "$auth_only"
  exec sg incus-admin -c "$command"
}

ensure_owner_incus() {
  local platform_root="$HOME/.cache/subyard-e2e-platform/incus"
  if ! id -nG | tr ' ' '\n' | grep -Fxq incus-admin \
    && id -nG "$(id -un)" | tr ' ' '\n' | grep -Fxq incus-admin; then
    reexec_with_incus_group
  fi
  if ! /usr/bin/incus query '/1.0/instances?project=default' >/dev/null 2>&1 \
    || ! /usr/bin/incus storage show default --project default >/dev/null 2>&1; then
    (
      # Reuse the supported platform initializer before release migration.
      # shellcheck source=tests/helpers/test-context.sh
      . "$ROOT/tests/helpers/test-context.sh"
      setup_test_context "$platform_root/bootstrap"
      export SUBYARD_USER SUBYARD_OPERATOR_HOME="$HOME"
      SUBYARD_USER="$(id -un)"
      export SUBYARD_CONFIG_DIR="$ROOT/config"
      export SUBYARD_CONFIG_HOME="$platform_root/config" SUBYARD_HOME="$platform_root/data"
      export STORAGE_PATH="$platform_root/incus/storage" YARD_KIND="$kind"
      export HOST_BASE="$platform_root/host-data" RESTRICTED_DISK_PATHS="$platform_root/host-data"
      set -a
      # shellcheck source=config/host.env
      . "$ROOT/config/host.env"
      set +a
      bash "$ROOT/scripts/01-install-incus.sh" --yes
    )
  fi
  if ! id -nG | tr ' ' '\n' | grep -Fxq incus-admin \
    && id -nG "$(id -un)" | tr ' ' '\n' | grep -Fxq incus-admin; then
    reexec_with_incus_group
  fi
  # The installer uses the native API, without the fixture's sudo wrapper.
  /usr/bin/incus query '/1.0/instances?project=default' >/dev/null \
    || die 'owner Incus API is unavailable in the actual execution session'
  /usr/bin/incus storage show default --project default >/dev/null \
    || die 'owner default storage is unavailable after platform preparation'
  printf 'openlit owner native-Incus-preflight=PASS\n'
}
candidate() {
  openlit_candidate_command "${CANDIDATE_ENGINE:-$ROOT/.build/yard}" "$@"
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
                  'metrics': count('otel_metrics_sum')+count('otel_metrics_histogram'), 'traces': count('otel_traces')}))
PYCODE
}

cleanup() {
  local rc=$?
  local original_rc=$rc
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
  if [ -n "$synthetic_address" ] && [ "$synthetic_address_active" = 1 ]; then
    sudo -n ip address del "$synthetic_address/32" dev lo || rc=3
  fi
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
  # Diagnostic recovery and cleanup cannot replace the ordinary enable failure.
  if [ "$auth_only" = pull-diagnostic ] && [ "$original_rc" -ne 0 ]; then rc=$original_rc; fi
  exit "$rc"
}
trap cleanup EXIT INT TERM

ensure_owner_incus
STATE="$(mktemp -d /var/tmp/subyard-openlit.XXXXXX)"
printf '%s\n' openlit-profile-e2e-v1 > "$STATE/.marker"
token="$(printf '%s' "${STATE##*.}" | tr '[:upper:]' '[:lower:]')"
YARD_NAME="dev-e2e-$token"
export SUBYARD_OPERATOR_HOME="$HOME"
export SUBYARD_CONFIG_HOME="$STATE/config"
export SUBYARD_HOME="$STATE/data"
export STORAGE_PATH="$HOME/.cache/subyard-e2e-platform/incus/incus/storage"
export SUBYARD_NO_AUDIT=1 SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1 MIN_DISK_GIB=1
# Complete the supported fresh release transition before power publication.
# Keep the runtime, shell configuration and links inside this fixture's root.
# Persist the host storage source before candidate bootstrap.
install -d -m 0700 "$SUBYARD_CONFIG_HOME"
printf 'STORAGE_PATH=%s\n' "$STORAGE_PATH" > "$SUBYARD_CONFIG_HOME/config.env"
chmod 0600 "$SUBYARD_CONFIG_HOME/config.env"
unset YARD_ENGINE_PATH
export YARD_RUNTIME_ROOT="$STATE/runtime"
printf 'openlit_e2e_stage=candidate-release-transition\n'
CANDIDATE_ENGINE="$(openlit_candidate_install "$ROOT" "$STATE")"
printf 'openlit_e2e_release_preflight=PASS\n'
install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME"
port=$((35000 + ($$ % 15000)))
integration_selection='claude codex'
case "$auth_only" in dependency-only|network-only) integration_selection='' ;; esac
cat > "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME/config.env" <<EOF
YARD_KIND=$kind
SSH_PORT=$port
CODING_TOOL_INTEGRATIONS="$integration_selection"
ENVIRONMENT_PROFILES=
HOST_BASE=$STATE/host
RESTRICTED_DISK_PATHS=$STATE/host
FORWARD_SSH_AGENT=0
EOF
if [ "$kind" = vm ]; then printf 'HOST_MOUNTS=\nHOST_LINKS=\nLIMITS_MEMORY=4GiB\nLIMITS_CPU=2\n' >> "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME/config.env"; fi
chmod 0600 "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME/config.env"
if [ -n "$integration_selection" ]; then
  # This fixture owns selected CONFIG/RULES and OpenLIT, not the stock Codex
  # package contract. Declare configuration-only provisioning before init so
  # absent native CLIs do not create unrelated release-activation drift.
  printf '#!/bin/sh\nexit 0\n' > "$STATE/codex-config-provision.sh"
  chmod 0600 "$STATE/codex-config-provision.sh"
  {
    printf 'AGENT_codex_PROVISION=%q\n' "$STATE/codex-config-provision.sh"
    printf 'AGENT_codex_COMMAND=true\nAGENT_codex_CHECK=true\n'
  } >> "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME/config.env"
  printf 'openlit_e2e_integration_contract=configuration-only native-package-convergence=not-tested\n'
fi

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
if [ "$auth_only" = network-only ]; then
  # Compare owner and this actual standard nested container in the same lease.
  # Five-second shutdown grace is included in the explicit 180-second bound.
  printf 'openlit_e2e_stage=same-lease-network-only\n'
  timeout --signal=TERM --kill-after=5 175 bash \
    "$ROOT/config/profiles/openlit/tests/e2e/network-only.sh" "$ROOT" "$STATE" "$instance" "$project"
  exit 0
fi
if [ "$auth_only" = dependency-only ]; then
  # Prove actual cold blob delivery in the owning guest before another acceptance
  # payload. This diagnostic lease is discarded; it never warms the next lease.
  printf 'openlit_e2e_stage=pinned-image-delivery\n'
  # The public recipe travels separately from the Python program on stdin.
  # Capture and allowlisted projection happen inside its root-private disposable
  # directory before cleanup; the helper preserves the actual native pull exit.
  compose_payload="$(base64 -w0 "$ROOT/config/profiles/openlit/compose.yaml")"
  incus exec "$instance" --project "$project" -- python3 - dependency \
    "${diagnostic_images[@]}" "$compose_payload" \
    < "$ROOT/config/profiles/openlit/tests/e2e/failure-facts.py"
  for image in "${diagnostic_images[@]}"; do
    incus exec "$instance" --project "$project" -- docker image inspect --format '{{.Id}}' "$image"
  done
  printf 'ok: all pinned OpenLIT image blobs delivered within native900s\n'
  exit 0
fi
printf 'openlit_e2e_stage=absent-native-clis\n'
# Both selected configurations exist before either native CLI is installed.
incus exec "$instance" --project "$project" -- bash -euo pipefail -c '
  install -d -m 0700 -o dev -g dev /home/dev/.local/bin
  for agent in claude codex; do
    if PATH=/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin command -v "$agent" >/dev/null; then
      printf "openlit-e2e: unexpected native CLI in root PATH\n" >&2
      exit 1
    fi
  done
  runuser -u dev -- env -i HOME=/home/dev PATH=/home/dev/.local/bin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    bash -euc '\''for agent in claude codex; do
      if command -v "$agent" >/dev/null; then
        printf "openlit-e2e: unexpected native CLI in developer PATH\n" >&2
        exit 1
      fi
    done'\''
'
# Read-only transport/cache facts do not pull images or replace the native cold
# enable deadline. HTTP reachability alone does not prove image/blob downloads.
incus exec "$instance" --project "$project" -- python3 - preflight "${diagnostic_images[@]}" \
  < "$ROOT/config/profiles/openlit/tests/e2e/failure-facts.py"
printf 'openlit_e2e_stage=enable\n'
if [ "$auth_only" = pull-diagnostic ]; then
  # shellcheck source=config/profiles/openlit/tests/e2e/pull-diagnostic.sh
  . "$ROOT/config/profiles/openlit/tests/e2e/pull-diagnostic.sh"
  diagnostic_rc=0
  openlit_pull_diagnostic "$ROOT" "$STATE" "$instance" "$project" "${diagnostic_images[@]}" || diagnostic_rc=$?
  exit "$diagnostic_rc"
fi
yard profile enable openlit --yes
if [ "$auth_only" = teardown-only ]; then printf 'ok: enabled profile ready for exact teardown regression\n'; exit 0; fi
# Both selected exporters are configured before either CLI appears in a user PATH.
incus exec "$instance" --project "$project" -- bash -euo pipefail -c '
  for agent in claude codex; do
    if command -v "$agent" >/dev/null; then
      printf "openlit-e2e: native CLI appeared before configuration acceptance\n" >&2
      exit 1
    fi
  done
  python3 - <<"PYCODE"
import json,tomllib
from pathlib import Path
assert json.loads(Path("/home/dev/.claude/settings.json").read_text())["env"]["CLAUDE_CODE_ENABLE_TELEMETRY"] == "1"
assert tomllib.loads(Path("/home/dev/.codex/config.toml").read_text())["otel"]["trace_exporter"]["otlp-http"]["endpoint"] == "http://127.0.0.1:4318/v1/traces"
PYCODE
'
# Fresh random credentials stay within this marker-owned disposable fixture.
incus file pull "$instance/var/lib/subyard/openlit/operator.json" "$STATE/operator.json" --project "$project" >/dev/null
chmod 0600 "$STATE/operator.json"
printf 'openlit_e2e_stage=published-dashboard-origin\n'
dashboard_port=$((1024 + (port+20000-1024)%64512))
origin="http://127.0.0.1:$dashboard_port"
yard status > /dev/null
if [ "$kind" = vm ]; then
  ! incus config device list "$instance" --project "$project" | grep -Fxq openlit-dashboard || die 'VM profile published an owner proxy'
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
synthetic_address_active=1
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
synthetic_address_active=''
printf 'openlit_e2e_stage=deferred-publication-and-restoration\n'
candidate _publications "$project" "$instance" > "$STATE/deferred-publications"
incus query "/1.0/instances/$instance?project=$project" | jq -e --arg marker "v2:$synthetic_address:$dashboard_port" \
  '.config["user.subyard.openlit_dashboard"] == $marker and .devices["openlit-dashboard"] == null
   and (.config["user.subyard.deferred_publication.openlit-dashboard"] | fromjson | .version == 1 and .ownership == $marker)' >/dev/null \
  || die 'native publication did not preserve the exact deferred dashboard ownership'
incus query "/1.0/instances/$instance?project=$project" | jq -c \
  '{marker:.config["user.subyard.openlit_dashboard"],receipt:.config["user.subyard.deferred_publication.openlit-dashboard"],device:.devices["openlit-dashboard"]}' > "$STATE/deferred-route-before"
deferred_native_before="$(incus exec "$instance" --project "$project" -- sha256sum /var/lib/subyard/openlit/receipt.json)"
retained_data > "$STATE/deferred-data-before"
# The exact deferred publication is converged; status withholds its unavailable URL.
yard init --yes
incus query "/1.0/instances/$instance?project=$project" | jq -c \
  '{marker:.config["user.subyard.openlit_dashboard"],receipt:.config["user.subyard.deferred_publication.openlit-dashboard"],device:.devices["openlit-dashboard"]}' > "$STATE/deferred-route-after"
cmp "$STATE/deferred-route-before" "$STATE/deferred-route-after" \
  || die 'deferred init changed dashboard ownership'
[ "$(incus exec "$instance" --project "$project" -- sha256sum /var/lib/subyard/openlit/receipt.json)" = "$deferred_native_before" ] \
  || die 'deferred init changed native runtime receipt'
[ "$(incus list "$instance" --project "$project" --format csv -c s)" = RUNNING ] \
  || die 'deferred dashboard stopped its local workload'
incus exec "$instance" --project "$project" -- systemctl is-active --quiet subyard-openlit.service
retained_data > "$STATE/deferred-data-after"
python3 - "$STATE/deferred-data-before" "$STATE/deferred-data-after" <<'DEFERRED_DATA'
import json,sys
before,after=(json.load(open(path)) for path in sys.argv[1:])
assert before['volumes']==after['volumes'], 'deferred init replaced native volumes'
assert after['logs']>=before['logs'] and after['metrics']>=before['metrics'], 'deferred init lost native telemetry'
DEFERRED_DATA
yard status > "$STATE/deferred-dashboard-status"
! grep -Fq "$private_origin/" "$STATE/deferred-dashboard-status" || die 'deferred dashboard exposed a working URL'
sudo -n ip address add "$synthetic_address/32" dev lo
synthetic_address_active=1
candidate _publications "$project" "$instance" > "$STATE/restored-publications"
yard init --yes
yard status | grep -Fq "$private_origin/" || die 'exact dashboard restoration did not publish its original URL'
python3 "$ROOT/config/profiles/openlit/tests/e2e/ui-origin.py" --origin "$private_origin" --state "$STATE/private-ui-cookies" --identity "$token-private" --resume
# Explicit owning cancellation revokes restoration before choosing a different
# origin. A valid deferred recipe is never silently replaced by loopback.
sudo -n ip address del "$synthetic_address/32" dev lo
synthetic_address_active=''
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
  stopped_collision_state() {
    local name
    for name in "$YARD_NAME" "$secondary_name"; do
      # Config bytes include the persisted profile selection and listener settings.
      sha256sum "$SUBYARD_CONFIG_HOME/yards/$name/config.env"
      incus query "/1.0/instances/yard-$name?project=subyard-$name" | jq -S \
        '{marker:.config["user.subyard.openlit_dashboard"],receipt:.config["user.subyard.deferred_publication.openlit-dashboard"],device:.devices["openlit-dashboard"]}'
    done
  }
  stopped_collision_state > "$STATE/stopped-collision-before"
  if (YARD_NAME="$secondary_name"; yard profile enable openlit --yes) > "$STATE/second-stopped-collision.log" 2>&1; then die 'second project claimed a stopped peer dashboard listener'; fi
  if ! grep -Fq 'observe profile runtime openlit-runtime: profile runtime handler observe failed: exit status 1' "$STATE/second-stopped-collision.log"; then
    printf 'openlit_e2e_stopped_peer_refusal=unclassified\n' >&2
    die 'stopped-peer refusal did not precede guest reconciliation'
  fi
  incus exec "$secondary_instance" --project "$secondary_project" -- test ! -e /var/lib/subyard/openlit
  [ "$(incus list "$instance" --project "$project" -f csv -c s)" = STOPPED ] || die 'peer refusal woke the stopped dashboard owner'
  stopped_collision_state > "$STATE/stopped-collision-after"
  cmp "$STATE/stopped-collision-before" "$STATE/stopped-collision-after" \
    || die 'stopped-peer refusal changed profile selection, configuration or dashboard ownership'
  yard start --yes
  for ((attempt=0;attempt<60;attempt++)); do
    if curl --silent --fail --max-time 1 "$origin/api/auth/csrf" >/dev/null; then break; fi
    sleep 1
  done
  curl --silent --fail --max-time 2 "$origin/api/auth/csrf" >/dev/null || die 'reserved first dashboard failed to restart'
  [ "$(incus config get "$instance" user.subyard.openlit_dashboard --project "$project")" = "$first_marker" ] || die 'peer refusal changed the first reservation'
  [ "$(incus exec "$instance" --project "$project" -- sha256sum /var/lib/subyard/openlit/receipt.json)" = "$native_before" ] || die 'peer refusal changed the first native stack'
  python3 "$ROOT/config/profiles/openlit/tests/e2e/ui-origin.py" --origin "$origin" --state "$STATE/ui-cookies" --identity "$token" --resume
  # Ordinary start waits for the yard agent; the UI can return before every
  # profile readiness predicate settles. Observe without repairing the stack.
  if openlit_candidate_command python3 "$ROOT/config/profiles/openlit/tests/e2e/profile-readiness.py" \
      wait --engine "$CANDIDATE_ENGINE" --yard "$YARD_NAME" --timeout 90; then
    :
  else
    readiness_rc=$?
    openlit_candidate_command python3 "$ROOT/config/profiles/openlit/tests/e2e/profile-readiness.py" \
      diagnose --engine "$CANDIDATE_ENGINE" --timeout 30 --failed-exit "$readiness_rc" || true
    exit "$readiness_rc"
  fi
  if [ "$auth_only" = auth-only ]; then
    (YARD_NAME="$secondary_name"; yard teardown --yes)
    secondary_name=''
    printf 'ok: published owner/SSH auth and stopped cross-project reservation regression\n'
    exit 0
  fi
  if (YARD_NAME="$secondary_name"; yard config unset OPENLIT_HOST_PORT --scope yard --local --yes); then
    :
  else
    unset_rc=$?
    openlit_candidate_command python3 "$ROOT/config/profiles/openlit/tests/e2e/profile-readiness.py" \
      diagnose --engine "$CANDIDATE_ENGINE" --timeout 30 --failed-exit "$unset_rc" || true
    exit "$unset_rc"
  fi
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
python3 "$ROOT/config/profiles/openlit/tests/e2e/ui-origin.py" --origin "$origin" --state "$STATE/operator-cookies" --identity "$token" \
  --credentials "$STATE/operator.json" --telemetry empty
printf 'openlit_e2e_stage=remote-owner-rpc-and-browser-tunnel\n'
if [ -n "$tunnel_pid" ]; then kill "$tunnel_pid"; wait "$tunnel_pid" 2>/dev/null || true; tunnel_pid=''; fi
python3 "$ROOT/config/profiles/openlit/tests/e2e/remote-owner.py" "$STATE" "$CANDIDATE_ENGINE" "$YARD_NAME" "$dashboard_port" "$token"
if [ "$kind" = vm ]; then
  ssh -N -o BatchMode=yes -o ExitOnForwardFailure=yes -L "$dashboard_port:127.0.0.1:3000" "yard-$YARD_NAME" > "$STATE/ui-tunnel.log" 2>&1 &
  tunnel_pid=$!
fi
# Install the genuine native CLI only after the no-CLI service and empty UI
# contracts. Keep its command and policy check in the developer's local bin;
# the declared configuration-only integration still owns no native package.
printf 'openlit_e2e_stage=user-local-codex-install\n'
incus exec "$instance" --project "$project" -- env \
  CODEX_BIN_LINK=/home/dev/.local/bin/codex \
  CODEX_CHECK_PATH=/home/dev/.local/bin/codex-policy-check \
  timeout --signal=TERM --kill-after=30 1200 bash -euo pipefail -s \
  < "$ROOT/config/agents/codex/provision.sh"
incus exec "$instance" --project "$project" -- bash -euo pipefail -c '
  if command -v codex >/dev/null; then
    printf "openlit-e2e: user-local Codex appeared in root PATH\n" >&2
    exit 1
  fi
  /home/dev/.local/bin/codex-policy-check >/dev/null
  runuser -u dev -- env HOME=/home/dev PATH=/home/dev/.local/bin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    codex --version
'
# Migration acceptance can insert a legacy receiver/old backend here, before
# the first native emission; the ordinary native fixture remains independent.
# The guest uses only synthetic prompts and local fake model APIs; no account credentials.
tar -C "$ROOT/config/profiles/openlit/tests/e2e" -cf - native-signals.py runtime-refresh.py | \
  incus exec "$instance" --project "$project" -- bash -euo pipefail -c '
    stage="$(mktemp -d /tmp/subyard-openlit-signals.XXXXXX)"
    trap '\''rm -rf -- "$stage"'\'' EXIT
    tar -xf - -C "$stage"
    python3 "$stage/native-signals.py"
    python3 "$stage/runtime-refresh.py"
  '
python3 "$ROOT/config/profiles/openlit/tests/e2e/ui-origin.py" --origin "$origin" --state "$STATE/operator-cookies" --identity "$token" \
  --credentials "$STATE/operator.json" --telemetry codex --resume
printf 'openlit_e2e_stage=rendered-native-telemetry\n'
if ! command -v chromium >/dev/null; then
  sudo -n timeout --signal=TERM --kill-after=30 240 apt-get install -y -qq chromium
fi
python3 "$ROOT/config/profiles/openlit/tests/e2e/ui-browser.py" --origin "$origin" \
  --credentials "$STATE/operator.json" --output "$STATE/browser" --timeout 120
printf 'openlit_e2e_stage=late-native-cli-appearance\n'
# First accept no-Claude readiness and real user-local Codex data. Only then
# install a user-local Claude to retain the independent native exporter contract
# and prove late appearance uses the configuration prepared at enable.
tar -C "$ROOT/config/profiles/openlit/tests/e2e" -cf - native-signals.py | \
  incus exec "$instance" --project "$project" -- bash -euo pipefail -c '
    stage="$(mktemp -d /tmp/subyard-openlit-late-cli.XXXXXX)"
    trap '\''rm -rf -- "$stage"'\'' EXIT
    tar -xf - -C "$stage"
    version=2.1.287
    case "$(dpkg --print-architecture)" in
      amd64) platform=linux-x64; checksum=3920489a5109cff5786a1a392c25277408ff22bc796d5edb9c16a60e5a1718f0 ;;
      arm64) platform=linux-arm64; checksum=e4daf793d1e74fb0d9874dd09e98690bbfd7be515f78a87fd05b9e2b4bb33b03 ;;
      *) exit 2 ;;
    esac
    curl --proto "=https" --proto-redir "=https" --tlsv1.2 --fail --silent --show-error --location \
      --connect-timeout 10 --max-time 180 \
      "https://downloads.claude.ai/claude-code-releases/$version/$platform/claude" -o "$stage/claude"
    printf "%s  %s\n" "$checksum" "$stage/claude" | sha256sum --check --status
    install -m 0755 -o dev -g dev "$stage/claude" /home/dev/.local/bin/claude
    if command -v claude >/dev/null; then
      printf "openlit-e2e: user-local Claude appeared in root PATH\n" >&2
      exit 1
    fi
    python3 "$stage/native-signals.py" --agents claude
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
  if systemctl is-active --quiet subyard-openlit.service; then
    printf "openlit-e2e: disabled service is still active\n" >&2
    exit 1
  fi
  if systemctl is-enabled --quiet subyard-openlit.service; then
    printf "openlit-e2e: disabled service is still enabled\n" >&2
    exit 1
  fi
  python3 - <<"PYCODE"
import json,tomllib
from pathlib import Path
state=json.loads(Path("/var/lib/subyard/openlit/native.json").read_text())
assert state["agents"] == {}
claude=json.loads(Path("/home/dev/.claude/settings.json").read_text())
codex=tomllib.loads(Path("/home/dev/.codex/config.toml").read_text())
assert "CLAUDE_CODE_ENABLE_TELEMETRY" not in claude.get("env",{})
assert "exporter" not in codex.get("otel",{})
assert "trace_exporter" not in codex.get("otel",{})
assert codex["model"] == "fixture-custom-preserved"
assert claude["env"]["OPENLIT_E2E_CUSTOM"] == "preserved"
PYCODE
'
yard init --yes
printf 'openlit_e2e_stage=re-enable-retained-data\n'
yard profile enable openlit --yes
python3 "$ROOT/config/profiles/openlit/tests/e2e/ui-origin.py" --origin "$origin" --state "$STATE/ui-cookies" --identity "$token" --resume
python3 "$ROOT/config/profiles/openlit/tests/e2e/ui-origin.py" --origin "$origin" --state "$STATE/operator-cookies" --identity "$token" \
  --credentials "$STATE/operator.json" --telemetry codex --resume
retained_data > "$STATE/retained-after.json"
chmod 0600 "$STATE/retained-after.json"
python3 - "$STATE/retained-before.json" "$STATE/retained-after.json" <<'PYCODE'
import json, sys
from pathlib import Path
before, after = [json.loads(Path(path).read_text()) for path in sys.argv[1:]]
assert len(before['volumes']) == 2 and after['volumes'] == before['volumes']
assert after['logs'] >= before['logs'] > 0
assert after['metrics'] >= before['metrics'] > 0
assert after['traces'] >= before['traces'] > 0
PYCODE
yard profile disable openlit --yes
printf 'ok: OpenLIT default off, enable without root-visible CLIs, receiver CAS update/rollback, repeat init/config apply, native logs+metrics+traces ingestion, disable and retained data after re-enable\n'
