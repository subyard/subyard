#!/usr/bin/env bash
# Real SSH stdio exact-plan acceptance on one allocated disposable VM.
set -euo pipefail
umask 022
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
die() { printf 'integration-remote: %s\n' "$*" >&2; exit 1; }
[ "${SUBYARD_E2E_VM:-}" = 1 ] || die 'run through dev/agent-e2e.sh on allocated VM1'
for command in jq python3 sudo ss ssh ssh-keygen; do command -v "$command" >/dev/null || die "$command is required"; done
[ -x /usr/sbin/sshd ] || die 'OpenSSH server is required in the allocated VM'
sudo -n true || die 'passwordless sudo is required in the allocated VM'
[ -x "$ROOT/.build/yard" ] || "$ROOT/dev/build-engine.sh"
STATE="$(mktemp -d /var/tmp/subyard-integration-remote.XXXXXX)"
token="$(printf '%s' "${STATE##*.}" | tr '[:upper:]' '[:lower:]')"
MARKER="subyard-integration-remote-v1-$token"
YARD_NAME="remote-selection-$token"
PROJECT="subyard-$YARD_NAME"
INSTANCE="yard-$YARD_NAME"
OWNER_CONFIG="$STATE/owner-config"
CONTROLLER_CONFIG="$STATE/controller-config"
config="$OWNER_CONFIG/yards/$YARD_NAME/config.env"
sshd_job=''
printf '%s\n' "$MARKER" > "$STATE/.marker"
export SUBYARD_OPERATOR_HOME="$HOME"
export STORAGE_PATH="$HOME/.cache/subyard-e2e-platform/incus/incus/storage"
export SUBYARD_NO_AUDIT=1 SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1 MIN_DISK_GIB=1
incus() {
  if [ -S /var/lib/incus/unix.socket ] && [ ! -w /var/lib/incus/unix.socket ]; then
    sudo -n /usr/bin/incus "$@"
  else
    /usr/bin/incus "$@"
  fi
}
owner() {
  SUBYARD_CONFIG_HOME="$OWNER_CONFIG" SUBYARD_HOME="$STATE/owner-data" \
    "$ROOT/.build/yard" -Y "$YARD_NAME" "$@"
}
controller() {
  PATH="$STATE/client-bin:$PATH" SUBYARD_CONFIG_HOME="$CONTROLLER_CONFIG" SUBYARD_HOME="$STATE/controller-data" \
    "$ROOT/.build/yard" -Y remote "$@"
}
guest() { incus exec "$INSTANCE" --project "$PROJECT" -- "$@"; }
cleanup() {
  local rc=$? managed='' daemon=''
  trap - EXIT INT TERM
  set +e
  if [ -f "$STATE/sshd.pid" ]; then
    daemon="$(cat "$STATE/sshd.pid")"
    if [[ "$daemon" =~ ^[0-9]+$ ]] && [ -f "/proc/$daemon/cmdline" ] \
      && sudo -n python3 -c 'import sys; assert sys.argv[2].encode() in open("/proc/"+sys.argv[1]+"/cmdline","rb").read()' "$daemon" "$STATE/sshd_config"; then
      sudo -n kill -TERM "$daemon" || rc=3
    fi
  fi
  if [ -n "$sshd_job" ]; then wait "$sshd_job" 2>/dev/null || true; fi
  if command -v /usr/bin/incus >/dev/null; then
    managed="$(incus config get "$INSTANCE" user.subyard.managed --project "$PROJECT" 2>/dev/null)"
  fi
  if [ -f "$config" ] && grep -Fqx "# $MARKER" "$config"; then
    if [ -z "$managed" ] || [ "$managed" = true ]; then
      install -d -m 0700 "$OWNER_CONFIG/yards/platform-sentinel"
      printf 'SSH_PORT=64996\n' > "$OWNER_CONFIG/yards/platform-sentinel/config.env"
      chmod 0600 "$OWNER_CONFIG/yards/platform-sentinel/config.env"
      owner teardown --yes > "$STATE/cleanup.log" 2>&1 || rc=3
    else
      printf 'integration-remote: refusing to teardown unowned target\n' >&2
      rc=3
    fi
  fi
  if [ "$rc" = 0 ] && [[ "$STATE" = /var/tmp/subyard-integration-remote.* ]] \
    && [ "$(cat "$STATE/.marker")" = "$MARKER" ]; then
    sudo -n find "$STATE" -depth -delete || rc=3
  else
    printf 'integration-remote: retained evidence at %s\n' "$STATE" >&2
  fi
  exit "$rc"
}
trap cleanup EXIT INT TERM
install -d -m 0700 "$OWNER_CONFIG/yards/$YARD_NAME" "$CONTROLLER_CONFIG/yards/remote" "$STATE/client-bin"
port=$((35000 + ($$ % 1000)))
while ss -Hln "sport = :$port" 2>/dev/null | grep -q .; do port=$((port + 1)); done
cat > "$config" <<CONFIG
# $MARKER
SSH_PORT=$port
HOST_BASE=$STATE/host
RESTRICTED_DISK_PATHS=$STATE/host
FORWARD_SSH_AGENT=0
ENVIRONMENT_PROFILES=
HOST_CLAUDE_MD=
HOST_CODEX_AGENTS_MD=
HOST_OPENCODE_AGENTS_MD=
CONFIG
chmod 0600 "$config"
# Keep only the retained test platform directory operator-owned; Incus children stay root-owned.
platform_root="$HOME/.cache/subyard-e2e-platform"
platform_marker="$platform_root/.subyard-e2e-platform-marker"
[ ! -L "$HOME/.cache" ] || die 'platform parent must not be a symlink'
if [ -e "$platform_root" ] || [ -L "$platform_root" ]; then
  [ -d "$platform_root" ] && [ ! -L "$platform_root" ] || die 'platform root must be a plain directory'
fi
if [ -e "$platform_marker" ] || [ -L "$platform_marker" ]; then
  [ -f "$platform_marker" ] && [ ! -L "$platform_marker" ] || die 'platform marker must be a plain file'
  [ "$(sudo -n cat "$platform_marker")" = subyard-e2e-platform-v1 ] || die 'unexpected platform marker'
fi
sudo -n install -d -o "$(id -u)" -g "$(id -g)" -m 0711 "$platform_root"
owner init --yes
owner start --yes
incus info >/dev/null
incus storage show default --project default >/dev/null
incus network show incusbr0 --project default >/dev/null
printf '%s\n' subyard-e2e-platform-v1 > "$STATE/platform-marker"
sudo -n install -o "$(id -u)" -g "$(id -g)" -m 0600 "$STATE/platform-marker" "$platform_marker.$token"
sudo -n mv -fT -- "$platform_marker.$token" "$platform_marker"
[ "$(cat "$platform_marker")" = subyard-e2e-platform-v1 ] || die 'unexpected platform marker'
controller_registration="$CONTROLLER_CONFIG/yards/remote/config.env"
cat > "$controller_registration" <<CONFIG
ACCESS_KIND=remote
OWNER_ENDPOINT=integration-owner
OWNER_YARD_NAME=$YARD_NAME
SSH_PORT=$port
CODING_TOOL_INTEGRATIONS=codex
CONFIG
chmod 0600 "$controller_registration"
controller_before="$(sha256sum "$controller_registration")"
# Only this temporary daemon accepts the synthetic key. The account, ~/.ssh,
# host sshd and developer shell startup remain untouched.
ssh-keygen -q -t ed25519 -N '' -f "$STATE/host-key"
ssh-keygen -q -t ed25519 -N '' -f "$STATE/client-key"
cp "$STATE/client-key.pub" "$STATE/authorized_keys"
chmod 0600 "$STATE/authorized_keys"
# Python emits shell-quoted fixed paths; no remote request is evaluated as shell code.
python3 - "$STATE/owner-rpc" "$ROOT" "$STATE" "$YARD_NAME" "$HOME" "$STORAGE_PATH" <<'PY'
import pathlib, shlex, sys
path, root, state, yard, home, storage = sys.argv[1:]
environment = {
    "SUBYARD_OPERATOR_HOME": home, "SUBYARD_CONFIG_HOME": state+"/owner-config",
    "SUBYARD_HOME": state+"/owner-data", "SUBYARD_REPOSITORY_ROOT": root,
    "STORAGE_PATH": storage, "SUBYARD_NO_AUDIT": "1", "SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE": "1",
}
lines = ["#!/bin/sh", "set -eu", "unset CODING_TOOL_INTEGRATIONS AGENTS SUBYARD_ENGINE_CONTEXT SUBYARD_CONFIG_LOADED"]
lines += ["export " + key + "=" + shlex.quote(value) for key, value in environment.items()]
lines += ["exec " + shlex.join([root+"/.build/yard", "-Y", yard, "rpc", "--stdio"])]
pathlib.Path(path).write_text("\n".join(lines)+"\n")
PY
chmod 0700 "$STATE/owner-rpc"
ssh_port=$((port + 2000))
while ss -Hln "sport = :$ssh_port" 2>/dev/null | grep -q .; do ssh_port=$((ssh_port + 1)); done
cat > "$STATE/sshd_config" <<CONFIG
Port $ssh_port
ListenAddress 127.0.0.1
HostKey $STATE/host-key
PidFile $STATE/sshd.pid
AuthorizedKeysFile $STATE/authorized_keys
StrictModes no
PasswordAuthentication no
KbdInteractiveAuthentication no
UsePAM no
PermitRootLogin prohibit-password
AllowUsers $(id -un)
AllowTcpForwarding no
X11Forwarding no
PermitTTY no
ForceCommand $STATE/owner-rpc
LogLevel ERROR
CONFIG
sudo -n /usr/sbin/sshd -t -f "$STATE/sshd_config"
# The invoking user owns this marker-local log; only the daemon needs root.
# shellcheck disable=SC2024
sudo -n /usr/sbin/sshd -D -e -f "$STATE/sshd_config" > "$STATE/sshd.log" 2>&1 &
sshd_job=$!
# Pin the exact generated host key rather than trusting ssh-keyscan output.
printf '[127.0.0.1]:%s ' "$ssh_port" > "$STATE/known_hosts"
cat "$STATE/host-key.pub" >> "$STATE/known_hosts"
cat > "$STATE/ssh_config" <<CONFIG
Host integration-owner
  HostName 127.0.0.1
  User $(id -un)
  Port $ssh_port
  IdentityFile $STATE/client-key
  IdentitiesOnly yes
  UserKnownHostsFile $STATE/known_hosts
  StrictHostKeyChecking yes
  ConnectTimeout 5
  ServerAliveInterval 5
  ServerAliveCountMax 2
  LogLevel ERROR
CONFIG
printf '#!/bin/sh\nexec /usr/bin/ssh -F %s "$@"\n' "$STATE/ssh_config" > "$STATE/client-bin/ssh"
chmod 0700 "$STATE/client-bin/ssh"
for _ in $(seq 1 50); do
  kill -0 "$sshd_job" 2>/dev/null || die 'ephemeral sshd exited'
  if ss -Hln "sport = :$ssh_port" | grep -q .; then break; fi
  sleep 0.1
done
owner_before="$(sha256sum "$config")"
controller integration status --json > "$STATE/status.json"
jq -e '.selection.present and .selection.requested == [] and .selection.effective == [] and .observed == "ready"' "$STATE/status.json" >/dev/null \
  || die 'controller selection leaked into fresh empty owner selection'
[ "$(sha256sum "$config")" = "$owner_before" ] || die 'remote status wrote owner desired config'
# Test the production framed consumer directly over one real SSH connection:
# tampered binding, consumed replay and disconnect before execute. Public commands
# below then verify valid digest execution through the same production consumer.
PATH="$STATE/client-bin:$PATH" python3 - "$STATE" <<'PY'
import datetime, json, struct, subprocess
def open_session():
    return subprocess.Popen(["ssh","-T","integration-owner","--","yard","rpc","--stdio"],stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.DEVNULL)
def call(process, method, operation="", params=None):
    deadline=(datetime.datetime.now(datetime.timezone.utc)+datetime.timedelta(seconds=60)).isoformat()
    request={"version":1,"type":"request","id":method,"method":method,"operationId":operation,"params":params or {},"deadline":deadline}
    data=json.dumps(request).encode()
    process.stdin.write(struct.pack(">I",len(data))+data);process.stdin.flush()
    while True:
        header=process.stdout.read(4)
        assert len(header)==4,"owner disconnected"
        size=struct.unpack(">I",header)[0]
        assert 0<size<=1024*1024,"invalid owner frame size"
        result=json.loads(process.stdout.read(size))
        assert result["version"]==1 and result.get("operationId","")==operation,"uncorrelated owner response"
        if result["type"]=="event":continue
        assert result["id"]==method
        return result
def close(process):
    process.stdin.close()
    try:process.wait(timeout=10)
    except subprocess.TimeoutExpired:
        process.kill()
        process.wait(timeout=5)
        raise
def negotiate(process):
    response=call(process,"rpc.negotiate")
    assert "operation-exact-plan-v1" in response["result"]["capabilities"]
params={"command":"integration","arguments":["enable","claude"],"exact":True}
process=open_session()
try:
    negotiate(process)
    plan=call(process,"operation.plan","remote-tamper",params)["result"]
    assert plan["schema"]==1 and len(plan["digest"])==64 and plan["expiresAt"]
    rejected=call(process,"operation.execute","remote-tamper",{"confirmed":True,"digest":"0"*64})
    assert rejected["error"]["code"]=="plan_binding_invalid",rejected
    replay=call(process,"operation.execute","remote-tamper",{"confirmed":True,"digest":plan["digest"]})
    assert replay["error"]["code"]=="plan_not_found",replay
    disconnected=call(process,"operation.plan","remote-disconnect",params)
    assert "result" in disconnected,disconnected
finally:close(process)
process=open_session()
try:
    negotiate(process)
    replay=call(process,"operation.execute","remote-disconnect",{"confirmed":True,"digest":disconnected["result"]["digest"]})
    assert replay["error"]["code"]=="plan_not_found",replay
finally:close(process)
print("ok: real SSH exact plan rejects tampering, replay and cross-session execution")
PY
[ "$(sha256sum "$config")" = "$owner_before" ] || die 'rejected RPC changed desired config'
controller integration enable claude --yes
controller integration status claude --json | jq -e '.selection.requested == ["claude"] and .observed == "ready"' >/dev/null
inventory_before="$(guest sha256sum /var/lib/subyard/integrations/inventory.json)"
controller integration enable claude --yes
[ "$(guest sha256sum /var/lib/subyard/integrations/inventory.json)" = "$inventory_before" ] || die 'remote no-op rewrote inventory'
guest sh -eu -c 'printf "%s\n" synthetic-auth > /home/dev/.claude/auth.json; printf "%s\n" synthetic-history > /home/dev/.claude/projects/remote-history'
controller integration disable claude --yes
controller integration status --json | jq -e '.selection.requested == [] and .observed == "ready"' >/dev/null
guest sh -eu -c '
  [ "$(cat /home/dev/.claude/auth.json)" = synthetic-auth ]
  [ "$(cat /mnt/host/agent-sessions/claude/projects/remote-history)" = synthetic-history ]
  [ ! -L /home/dev/.claude/projects ]
  python3 -c '\''import json; assert "permissions" not in json.load(open("/home/dev/.claude/settings.json"))'\''
'
owner stop --yes
owner_before="$(sha256sum "$config")"
for verb in enable disable; do
  if controller integration "$verb" claude > "$STATE/stopped-$verb.log" 2>&1; then die "stopped remote $verb was accepted"; fi
  grep -q 'running yard' "$STATE/stopped-$verb.log" || die 'stopped refusal did not explain running-yard precondition'
  if grep -q 'Proceed?' "$STATE/stopped-$verb.log"; then die 'stopped remote request prompted'; fi
  [ "$(sha256sum "$config")" = "$owner_before" ] || die 'stopped remote mutation changed owner desired'
  [ "$(incus list "$INSTANCE" --project "$PROJECT" --format json | jq -r '.[0].status')" = Stopped ] || die 'stopped remote request started yard'
done
controller integration status --json | jq -e '.observed == "stopped" and .selection.requested == []' >/dev/null
[ "$(sha256sum "$controller_registration")" = "$controller_before" ] || die 'controller registration was rewritten'
printf 'ok: real remote enable/disable/status, owner selection, no-op, data preservation and stopped refusal\n'
