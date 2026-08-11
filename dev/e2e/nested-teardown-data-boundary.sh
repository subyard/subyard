#!/usr/bin/env bash
# Real outer+nested teardown boundary acceptance. Run only on a disposable leased VM.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
STATE=''
OUTER_YARD=''
OUTER_PROJECT=''
OUTER_INSTANCE=''
OUTER_POOL=''
OUTER_BRIDGE=''
DEFAULT_POOL_BEFORE=''

die() { printf 'nested-teardown-boundary: %s\n' "$*" >&2; exit 2; }

[ -n "${SUBYARD_E2E_VM:-}" ] || die 'run through dev/agent-e2e.sh'
for command in go incus jq sudo timeout; do
  command -v "$command" >/dev/null 2>&1 || die "$command is required"
done
COMMAND_TIMEOUT="${NESTED_TEARDOWN_COMMAND_TIMEOUT:-1800}"
if ! command -v qemu-system-x86_64 >/dev/null 2>&1; then
  command -v apt-get >/dev/null 2>&1 || die 'qemu-system-x86_64 or apt-get is required'
  printf '  [ .. ] installing QEMU for the outer VM fixture\n'
  sudo -n apt-get update >/dev/null
  sudo -n env DEBIAN_FRONTEND=noninteractive apt-get install -y qemu-system-x86 >/dev/null
fi

if [ ! -x "$ROOT/.build/yard" ]; then
  "$ROOT/dev/build-engine.sh"
fi

yard() {
  timeout --foreground "$COMMAND_TIMEOUT" "$ROOT/.build/yard" -Y "$OUTER_YARD" "$@"
}

setting() {
  local key="$1" value
  value="$(yard config show "$key" | sed -n 's/^effective: //p')"
  [ -n "$value" ] || die "could not resolve $key"
  printf '%s\n' "$value"
}

default_pool_snapshot() {
  local state rc inventory
  set +e
  state="$(incus storage show default --project default 2>/dev/null)"
  rc=$?
  set -e
  if [ "$rc" = 0 ]; then
    printf 'present\n%s\n' "$state"
    return
  fi
  inventory="$(incus storage list --project default --format csv -c n)" \
    || die 'could not inventory the default Incus pool'
  ! grep -Fxq default <<<"$inventory" \
    || die 'default Incus pool inventory and state query disagree'
  printf 'absent\n'
}

assert_default_pool_unchanged() {
  local after before_hash after_hash
  after="$(default_pool_snapshot)"
  [ "$after" = "$DEFAULT_POOL_BEFORE" ] && return
  before_hash="$(printf '%s' "$DEFAULT_POOL_BEFORE" | sha256sum | awk '{print $1}')"
  after_hash="$(printf '%s' "$after" | sha256sum | awk '{print $1}')"
  die "candidate lifecycle changed the default Incus pool: before=$before_hash after=$after_hash"
}

assert_outer_vm_ssh_address() {
  local stage="$1" primary_interface eth0 foreign pinned connect
  primary_interface="$(incus exec "$OUTER_INSTANCE" --project "$OUTER_PROJECT" -- \
    ip -4 route show default \
    | awk 'NR == 1 { for (i=1; i<=NF; i++) if ($i == "dev") { print $(i+1); exit } }')"
  eth0="$(incus exec "$OUTER_INSTANCE" --project "$OUTER_PROJECT" -- \
    ip -4 -o address show dev "$primary_interface" scope global \
    | awk 'NR == 1 { split($4, address, "/"); print address[1] }')"
  foreign="$(incus exec "$OUTER_INSTANCE" --project "$OUTER_PROJECT" -- \
    ip -4 -o address show dev aaa0 scope global \
    | awk 'NR == 1 { split($4, address, "/"); print address[1] }')"
  pinned="$(incus config device get "$OUTER_INSTANCE" eth0 ipv4.address \
    --project "$OUTER_PROJECT")"
  connect="$(incus config device get "$OUTER_INSTANCE" ssh connect \
    --project "$OUTER_PROJECT")"
  [ -n "$eth0" ] || die "$stage: outer VM eth0 has no global IPv4"
  [ -n "$foreign" ] || die "$stage: foreign IPv4 fixture is missing"
  [ "$eth0" != "$foreign" ] || die "$stage: primary and foreign interfaces share an IPv4"
  [ "$pinned" = "$eth0" ] || die "$stage: eth0 pinned $pinned instead of $eth0"
  [ "$connect" = "tcp:$eth0:22" ] \
    || die "$stage: SSH proxy connects to $connect instead of tcp:$eth0:22"
  incus exec "$OUTER_INSTANCE" --project "$OUTER_PROJECT" -- true >/dev/null \
    || die "$stage: outer VM agent is unreachable"
}

ensure_outer_foreign_ipv4() {
  incus exec "$OUTER_INSTANCE" --project "$OUTER_PROJECT" -- sh -eu -c '
    ip link show aaa0 >/dev/null 2>&1 || ip link add aaa0 type dummy
    ip -4 address show dev aaa0 | grep -q "172.31.255.1/24" \
      || ip address add 172.31.255.1/24 dev aaa0
    ip link set aaa0 up
  '
}

cleanup() {
  local rc=$?
  trap - EXIT INT TERM
  set +e
  if [ -n "$OUTER_PROJECT" ] && incus project show "$OUTER_PROJECT" >/dev/null 2>&1; then
    yard teardown --yes >/dev/null 2>&1 || rc=3
  fi
  if [ -n "$OUTER_POOL" ] && incus storage show "$OUTER_POOL" --project default >/dev/null 2>&1 \
    && [ "$(incus storage get "$OUTER_POOL" user.subyard.owner --project default 2>/dev/null)" = \
      nested-teardown-e2e-v1 ]; then
    incus storage delete "$OUTER_POOL" --project default >/dev/null 2>&1 || rc=3
  fi
  if [ -n "$OUTER_BRIDGE" ] \
    && incus network show "$OUTER_BRIDGE" --project default >/dev/null 2>&1; then
    if [ "$(incus network get "$OUTER_BRIDGE" user.subyard.owner \
      --project default 2>/dev/null)" = nested-teardown-e2e-v1 ] \
      && [ "$(incus network show "$OUTER_BRIDGE" --project default \
        | sed -n 's/^used_by: //p')" = '[]' ]; then
      incus network delete "$OUTER_BRIDGE" --project default >/dev/null 2>&1 || rc=3
    else
      rc=3
    fi
  fi
  if [ -n "$STATE" ] && [[ "$STATE" = /var/tmp/subyard-nested-teardown.* ]] \
    && [ -f "$STATE/.marker" ] && [ "$(<"$STATE/.marker")" = nested-teardown-e2e-v1 ]; then
    sudo -n find "$STATE" -depth -delete || rc=3
  fi
  exit "$rc"
}
trap cleanup EXIT INT TERM

STATE="$(mktemp -d /var/tmp/subyard-nested-teardown.XXXXXX)"
printf '%s\n' nested-teardown-e2e-v1 > "$STATE/.marker"
token="$(printf '%s' "${STATE##*.}" | tr '[:upper:]' '[:lower:]')"
OUTER_YARD="nested-e2e-$token"
OUTER_PROJECT="subyard-$OUTER_YARD"
OUTER_INSTANCE="yard-$OUTER_YARD"
OUTER_POOL="nested-e2e-$token"
OUTER_BRIDGE="ne${token:0:8}br0"
DEFAULT_POOL_BEFORE="$(default_pool_snapshot)"

incus project show "$OUTER_PROJECT" >/dev/null 2>&1 \
  && die "refusing existing project $OUTER_PROJECT"
incus storage show "$OUTER_POOL" --project default >/dev/null 2>&1 \
  && die "refusing existing pool $OUTER_POOL"
incus network show "$OUTER_BRIDGE" --project default >/dev/null 2>&1 \
  && die "refusing existing network $OUTER_BRIDGE"
incus storage create "$OUTER_POOL" dir --project default >/dev/null
incus storage set "$OUTER_POOL" user.subyard.owner=nested-teardown-e2e-v1 \
  --project default >/dev/null

export SUBYARD_OPERATOR_HOME="$STATE/operator"
export SUBYARD_CONFIG_HOME="$STATE/config"
export SUBYARD_HOME="$STATE/data"
export STORAGE_PATH="$STATE/storage"
export SUBYARD_NO_AUDIT=1
export SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1
export MIN_DISK_GIB=1
export REC_DISK_GIB=1
export HOST_CLAUDE_MD=
export HOST_CODEX_AGENTS_MD=
export HOST_OPENCODE_AGENTS_MD=
install -d -m 0700 "$SUBYARD_OPERATOR_HOME" "$SUBYARD_CONFIG_HOME/yards/$OUTER_YARD"

ssh_port=$((32000 + ($$ % 10000)))
while ss -H -ltn "sport = :$ssh_port" 2>/dev/null | grep -q .; do
  ssh_port=$((ssh_port + 1))
done
cat > "$SUBYARD_CONFIG_HOME/yards/$OUTER_YARD/config.env" <<EOF
SSH_PORT=$ssh_port
CODING_TOOL_INTEGRATIONS=
YARD_KIND=vm
LIMITS_CPU=4
LIMITS_MEMORY=3GiB
SRV_POOL=$OUTER_POOL
INCUS_BRIDGE=$OUTER_BRIDGE
HOST_MOUNTS=
HOST_LINKS=
HOST_BASE=$STATE/host
RESTRICTED_DISK_PATHS=$STATE/host
FORWARD_SSH_AGENT=0
DEV_SUDO=1
NESTED_E2E_VMS=0
EOF

printf '  [ .. ] creating the outer yard\n'
yard init --yes
incus network set "$OUTER_BRIDGE" user.subyard.owner=nested-teardown-e2e-v1 \
  --project default >/dev/null
yard start --yes
[ "$(incus config get "$OUTER_INSTANCE" user.subyard.managed --project "$OUTER_PROJECT")" = true ] \
  || die 'outer instance is not marker-owned'
ensure_outer_foreign_ipv4
assert_outer_vm_ssh_address fresh-init
docker_pid_before="$(incus exec "$OUTER_INSTANCE" --project "$OUTER_PROJECT" -- \
  systemctl show docker.service --property=MainPID --value)"
incus exec "$OUTER_INSTANCE" --project "$OUTER_PROJECT" -- \
  iptables -P FORWARD DROP
yard init --yes
assert_outer_vm_ssh_address repeated-init
[ "$(incus exec "$OUTER_INSTANCE" --project "$OUTER_PROJECT" -- \
  iptables -S FORWARD | head -n 1)" = '-P FORWARD ACCEPT' ] \
  || die 'repeated init retained stale FORWARD DROP'
[ "$(incus exec "$OUTER_INSTANCE" --project "$OUTER_PROJECT" -- \
  systemctl show docker.service --property=MainPID --value)" = "$docker_pid_before" ] \
  || die 'repeated init restarted Docker without config drift'
yard stop --yes
yard start --yes
ensure_outer_foreign_ipv4
yard init --yes
assert_outer_vm_ssh_address restart-init

printf '  [ .. ] syncing the exact candidate into the outer yard\n'
yard sync "$ROOT" --name NestedBoundary --target yard --yes >/dev/null
project_state="$(find "$SUBYARD_CONFIG_HOME/yards/$OUTER_YARD/projects" \
  -maxdepth 1 -type f -name '*.json' -print -quit)"
[ -n "$project_state" ] || die 'controller project state is missing'
project_id="$(jq -r '.projectId' "$project_state")"
outer_source="/srv/workspaces/$project_id/src"

yard code "$project_id" --yes > "$STATE/code.out"
outer_ssh="$(setting SSH_HOST)"
outer_ssh_namespace="$(printf '%s' "$outer_ssh" | base64 -w0 | tr '+/' '-_' | tr -d '=')"
descriptor="$SUBYARD_CONFIG_HOME/workspaces/$outer_ssh_namespace.$project_id/NestedBoundary.code-workspace"
[ -f "$descriptor" ] || die 'controller-local workspace descriptor is missing'
IFS= read -r outer_host_id < "$SUBYARD_CONFIG_HOME/host-id"
[ -n "$outer_host_id" ] || die 'controller HostID is missing'
expected_title="\${rootNameShort} — Yard SSH: $outer_host_id/$OUTER_YARD"
jq -e \
  --arg authority "ssh-remote+$outer_ssh" \
  --arg uri "vscode-remote://ssh-remote+$outer_ssh$outer_source" \
  --arg title "$expected_title" \
  '.remoteAuthority == $authority and .folders == [{name:"NestedBoundary", uri:$uri}] and
    .settings["window.title"] == $title' \
  "$descriptor" >/dev/null \
  || die 'workspace descriptor does not select the expected Remote-SSH workspace'

outer_dev() {
  timeout --foreground "$COMMAND_TIMEOUT" incus exec "$OUTER_INSTANCE" --project "$OUTER_PROJECT" \
    --user 1000 --group 1000 --env HOME=/home/dev --env USER=dev --env LOGNAME=dev -- "$@"
}

printf '  [ .. ] creating and tearing down a source inner yard\n'
outer_dev sh -euc '
  source=$1
  install -d "$HOME/.subyard/workspaces"
  printf "outer sentinel\n" > "$HOME/.subyard/workspaces/active.code-workspace"
  cd "$source"
  env \
    CODING_TOOL_INTEGRATIONS= HOST_MOUNTS= HOST_LINKS= FORWARD_SSH_AGENT=0 DEV_SUDO=0 \
    NESTED_E2E_VMS=0 SSH_PORT=23222 MIN_DISK_GIB=1 REC_DISK_GIB=1 \
    SUBYARD_NO_AUDIT=1 SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1 \
    HOST_CLAUDE_MD= HOST_CODEX_AGENTS_MD= HOST_OPENCODE_AGENTS_MD= \
    ./bin/yard init --yes
  sudo -n incus project show subyard >/dev/null
  sg incus-admin -c "env SUBYARD_NO_AUDIT=1 ./bin/yard teardown --yes"
  ! sudo -n incus project show subyard >/dev/null 2>&1
  [ -f "$HOME/.subyard/workspaces/active.code-workspace" ]
  [ ! -e "$HOME/.config/subyard/projects" ]
  sg incus-admin -c "env SUBYARD_NO_AUDIT=1 ./bin/yard teardown --yes" >/dev/null
' _ "$outer_source"

incus info "$OUTER_INSTANCE" --project "$OUTER_PROJECT" >/dev/null \
  || die 'inner teardown removed the outer instance'
[ -f "$descriptor" ] || die 'inner teardown removed the controller descriptor'
outer_dev sudo -n rm -rf -- /home/dev/.subyard
[ -f "$descriptor" ] || die 'agent data deletion removed the controller descriptor'
incus info "$OUTER_INSTANCE" --project "$OUTER_PROJECT" >/dev/null \
  || die 'agent data deletion stopped the outer instance'
yard shell "$project_id" --yes -- true \
  || die 'inner teardown or agent data deletion broke outer SSH transport'

printf '  [ .. ] tearing down the outer yard and checking the default pool\n'
yard teardown --yes
! incus project show "$OUTER_PROJECT" >/dev/null 2>&1 \
  || die 'outer project remains after teardown'
assert_default_pool_unchanged

printf 'ok: nested teardown preserves the outer yard boundary, foreign data and default pool\n'
