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
  # Init can enroll this user without refreshing its parent shell groups.
  # Use the existing disposable-fixture convention; preserve every argument.
  if ! id -nG | tr ' ' '\n' | grep -Fxq incus-admin \
    && id -nG "$(id -un)" | tr ' ' '\n' | grep -Fxq incus-admin; then
    local owner_command
    printf -v owner_command '%q ' "$ROOT/.build/yard" -Y "$YARD_NAME" "$@"
    SUBYARD_CONFIG_HOME="$OWNER_CONFIG" SUBYARD_HOME="$STATE/owner-data" \
      sg incus-admin -c "exec $owner_command"
  else
    SUBYARD_CONFIG_HOME="$OWNER_CONFIG" SUBYARD_HOME="$STATE/owner-data" \
      "$ROOT/.build/yard" -Y "$YARD_NAME" "$@"
  fi
}
controller() {
  PATH="$STATE/client-bin:$PATH" SUBYARD_CONFIG_HOME="$CONTROLLER_CONFIG" SUBYARD_HOME="$STATE/controller-data" SUBYARD_HOST_ID="integration-controller-$token" \
    "$ROOT/.build/yard" -Y remote "$@"
}
report_cli_failure() {
  python3 - "$1" <<'PY'
import pathlib,re,sys
count=0
for line in pathlib.Path(sys.argv[1]).read_text(errors="replace").splitlines():
    if not re.match(r"^(?:fatal: )?(?:yard|yard-engine): ",line): continue
    line=re.sub(r"(?:https?://|/)[^\s]+","<path>",line)
    line=re.sub(r"\b(?:token|password|secret|authorization|credential)[^\s:=]*\s*[:=]\s*\S+","<redacted>",line,flags=re.I)
    line=re.sub(r"\b[A-Za-z0-9_]+=[^\s]+","<assignment>",line)
    line=re.sub(r"[a-fA-F0-9]{32,}","<identity>",line)
    print(line[:400],file=sys.stderr)
    count+=1
    if count==8: break
if not count: print("integration-remote: CLI failed without a canonical diagnostic",file=sys.stderr)
PY
}
controller_logged() {
  local log="$1" rc=0
  shift
  install -m 0600 /dev/null "$log"
  controller "$@" > "$log" 2>&1 || rc=$?
  if [ "$rc" -ne 0 ]; then report_cli_failure "$log"; fi
  return "$rc"
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
SSH_HOST=integration-guest
HOST_BASE=$STATE/host
RESTRICTED_DISK_PATHS=$STATE/host
FORWARD_SSH_AGENT=0
CODING_TOOL_INTEGRATIONS=
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
SSH_HOST=integration-guest
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
# The temporary forced command accepts only the existing framed RPC transport
# and native read-only role/admission checks. Parse nested legacy argv without
# evaluating its shell text; every execution uses this captured owner context.
python3 - "$STATE/owner-rpc" "$ROOT" "$STATE" "$YARD_NAME" "$HOME" "$STORAGE_PATH" <<'PY'
import pathlib,sys
path,root,state,yard,home,storage=sys.argv[1:]
environment={"SUBYARD_OPERATOR_HOME":home,"SUBYARD_CONFIG_HOME":state+"/owner-config",
             "SUBYARD_HOME":state+"/owner-data","SUBYARD_REPOSITORY_ROOT":root,
             "STORAGE_PATH":storage,"SUBYARD_NO_AUDIT":"1","SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE":"1"}
program="#!/usr/bin/env python3\nimport os,re,shlex,sys\n"
program += "OWNER_YARD="+repr(yard)+"\nOWNER_ENGINE="+repr(root+"/.build/yard")+"\nOWNER_ENV="+repr(environment)+"\n"
program += r'''
def reject():
    print("integration-remote: unsupported owner transport command",file=sys.stderr)
    sys.exit(2)
try:
    request=shlex.split(os.environ.get("SSH_ORIGINAL_COMMAND",""))
    operation=""
    if len(request)==3 and request[:2]==["bash","-lc"]:
        request=shlex.split(request[2])
        if request and request[0]=="exec":
            request.pop(0)
            if request not in (["yard","rpc","--stdio"],["yard","-Y",OWNER_YARD,"rpc","--stdio"]): reject()
        elif request and request[0].startswith("SUBYARD_OPERATION_ID="):
            operation=request.pop(0).split("=",1)[1]
            if operation and not re.fullmatch(r"[A-Za-z0-9_.-]{1,128}",operation): reject()
            if request!=["yard","-Y",OWNER_YARD,"_project-state","check-role"] and request[:5]!=["yard","-Y",OWNER_YARD,"_project-state","preview"]: reject()
        else: reject()
    # Even quoted shell operators are unnecessary for these fixture sources.
    if any(any(character in value for character in ";|&`$\n\r") for value in request): reject()
    if request in (["yard","rpc","--stdio"],["yard","-Y",OWNER_YARD,"rpc","--stdio"]):
        arguments=["rpc","--stdio"]
    elif request==["yard","-Y",OWNER_YARD,"_project-state","check-role"]:
        arguments=request[3:]
    elif len(request)>=9 and request[:5]==["yard","-Y",OWNER_YARD,"_project-state","preview"]:
        source,mode,name,explicit=request[5:9]
        if not source or len(source)>4096 or mode not in ("sync","git","bind") or explicit not in ("0","1"): reject()
        if any(not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]*",value) for value in [name]+request[9:]): reject()
        arguments=request[3:]
    else: reject()
except ValueError:
    reject()
environment=os.environ.copy()
for key in ("CODING_TOOL_INTEGRATIONS","AGENTS","SUBYARD_ENGINE_CONTEXT","SUBYARD_CONFIG_LOADED"):
    environment.pop(key,None)
environment.update(OWNER_ENV)
environment["SUBYARD_OPERATION_ID"]=operation
os.execve(OWNER_ENGINE,[OWNER_ENGINE,"-Y",OWNER_YARD]+arguments,environment)
'''
pathlib.Path(path).write_text(program)
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
# The project data plane uses real guest SSH, independently of owner RPC.
# Add only this synthetic fixture key inside the disposable managed guest.
guest_ip="$(guest ip -4 -o address show dev eth0 | awk '{split($4,address,"/"); print address[1]; exit}')"
[ -n "$guest_ip" ] || die 'guest has no IPv4 address for physical copy transport'
guest install -d -o dev -g dev -m 0700 /home/dev/.ssh
guest sh -eu -c 'cat >> /home/dev/.ssh/authorized_keys; chown dev:dev /home/dev/.ssh/authorized_keys; chmod 0600 /home/dev/.ssh/authorized_keys' < "$STATE/client-key.pub"
guest sh -eu -c 'cat /etc/ssh/ssh_host_ed25519_key.pub' > "$STATE/guest-host-key.pub"
printf 'integration-guest ' >> "$STATE/known_hosts"
cat "$STATE/guest-host-key.pub" >> "$STATE/known_hosts"
cat >> "$STATE/ssh_config" <<CONFIG
Host integration-guest
  HostName $guest_ip
  HostKeyAlias integration-guest
  User dev
  Port 22
  IdentityFile $STATE/client-key
  IdentitiesOnly yes
  UserKnownHostsFile $STATE/known_hosts
  StrictHostKeyChecking yes
  ConnectTimeout 5
  LogLevel ERROR
CONFIG
printf '#!/bin/sh\nexec /usr/bin/ssh -F %s "$@"\n' "$STATE/ssh_config" > "$STATE/client-bin/ssh"
chmod 0700 "$STATE/client-bin/ssh"
for _ in $(seq 1 50); do
  kill -0 "$sshd_job" 2>/dev/null || die 'ephemeral sshd exited'
  if ss -Hln "sport = :$ssh_port" | grep -q .; then break; fi
  sleep 0.1
done
# New strong-step cases use the same real owner consumer and native state probes.
# Scripts stay inside this marker-owned root; the runner records the bundle hash.
{
  printf '#!/usr/bin/env bash\nset -euo pipefail\n'
  declare -f incus owner
  cat <<'PROBE'
case "$1" in
  probe) sha256sum "$config"; incus list "$INSTANCE" --project "$PROJECT" --format json | jq -c 'map({name,status})' ;;
  drift) owner config set SSH_PORT 64992 --scope yard --yes >/dev/null ;;
esac
PROBE
} > "$STATE/exact-native-probe"
chmod 0700 "$STATE/exact-native-probe"
export ROOT STATE OWNER_CONFIG config INSTANCE PROJECT YARD_NAME
python3 - "$STATE" <<'FIXTURE'
import json, pathlib, sys
state=pathlib.Path(sys.argv[1])
base={"ssh":["ssh","-T","integration-owner","--","yard","rpc","--stdio"],
      "command":"integration","arguments":["enable","claude"],
      "probe":[str(state/"exact-native-probe"),"probe"]}
(state/"exact-integration.json").write_text(json.dumps(base))
base.update(command="config",arguments=["set","SSH_PORT","64993","--scope","yard"],
            drift=[str(state/"exact-native-probe"),"drift"])
(state/"exact-config.json").write_text(json.dumps(base))
FIXTURE
original_ssh_port="$(sed -n 's/^SSH_PORT=//p' "$config")"
PATH="$STATE/client-bin:$PATH" python3 dev/e2e/exact-operation-ssh.py "$STATE/exact-integration.json"
PATH="$STATE/client-bin:$PATH" python3 dev/e2e/exact-operation-ssh.py "$STATE/exact-config.json"
owner config set SSH_PORT "$original_ssh_port" --scope yard --yes >/dev/null
# Synthetic native resource uses the real owner resource dispatcher, filesystem
# observation, native lock and verify; it has no external workload dependency.
resource_profile="fixture-exact-$token"
resource_root="$ROOT/config/profiles/$resource_profile/resources/exact-ssh-fixture"
[ ! -e "$ROOT/config/profiles/$resource_profile" ] || die 'exact resource fixture already exists'
install -d -m 0700 "$resource_root"
printf 'PROFILE_NAME=%s\n' "$resource_profile" > "$ROOT/config/profiles/$resource_profile/profile.conf"
cat > "$ROOT/config/profiles/$resource_profile/resources/exact-ssh-fixture.res" <<'RESOURCE'
COMMAND=exact-ssh-fixture
HANDLER=resources/exact-ssh-fixture/handler.sh
TITLE="Exact SSH native fixture"
ACTION="up up host-change reversible"
BRINGUP=up
SHUTDOWN=up
RESOURCE
chmod 0600 "$ROOT/config/profiles/$resource_profile/profile.conf" "$ROOT/config/profiles/$resource_profile/resources/exact-ssh-fixture.res"
install -m 0755 dev/e2e/exact-resource-fixture.py "$resource_root/handler.sh"
owner config set ENVIRONMENT_PROFILES "$resource_profile" --scope yard --yes >/dev/null
cat > "$STATE/exact-resource-probe" <<'RESOURCE_PROBE'
#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  probe) if [ -e "$resource_root/native-state" ]; then stat -c '%d:%i:%f' "$resource_root/native-state"; sha256sum "$resource_root/native-state"; else printf 'absent\n'; fi ;;
  drift) printf 'changed before execute\n' > "$resource_root/native-state"; chmod 0600 "$resource_root/native-state" ;;
esac
RESOURCE_PROBE
chmod 0700 "$STATE/exact-resource-probe"
export resource_root
python3 - "$STATE" <<'RESOURCE_CASE'
import json,pathlib,sys
state=pathlib.Path(sys.argv[1])
fixture=json.loads((state/"exact-integration.json").read_text())
fixture.update(command="exact-ssh-fixture",arguments=["up"],
               probe=[str(state/"exact-resource-probe"),"probe"],
               drift=[str(state/"exact-resource-probe"),"drift"])
(state/"exact-resource.json").write_text(json.dumps(fixture))
RESOURCE_CASE
PATH="$STATE/client-bin:$PATH" python3 dev/e2e/exact-operation-ssh.py "$STATE/exact-resource.json"
controller exact-ssh-fixture up --yes >/dev/null
[ "$(cat "$resource_root/native-state")" = ready ] || die 'resource native apply/verify failed'
owner config set ENVIRONMENT_PROFILES '' --scope yard --yes >/dev/null
owner_before="$(sha256sum "$config")"
controller integration status --json > "$STATE/status.json"
if ! jq -e '.selection.present and .selection.requested == [] and .selection.effective == [] and .observed == "ready"' "$STATE/status.json" >/dev/null; then
  jq -c '{selection: {present: .selection.present, requested: .selection.requested, effective: .selection.effective, provenance: {scope: .selection.provenance.scope}}, observed: .observed}' "$STATE/status.json" >&2
  die 'controller selection leaked into fresh empty owner selection'
fi
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
    assert {"operation-exact-plan-v1", "operation-steps-v1"}.issubset(response["result"]["capabilities"])
params={"command":"integration","arguments":["enable","claude"],"exact":True,"stepSchema":1}
process=open_session()
try:
    negotiate(process)
    plan=call(process,"operation.plan","remote-tamper",params)["result"]
    assert plan["schema"]==1 and plan["stepSchema"]==1 and plan["plan"]["steps"] and len(plan["digest"])==64 and plan["expiresAt"]
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
# Public remote sync uses its retained controller archive and the existing
# owner admission/stream/finalize path. No fixture-owned transfer executor.
source="$STATE/source/RemoteExactCopy"
install -d -m 0700 "$source"
printf '%s\nfirst snapshot\n' "$MARKER" > "$source/result.txt"
printf '%s\n' synthetic-hidden > "$source/.hidden"
ln -s result.txt "$source/link"
# Exercise owner admission lifecycle without sending payload; use facts from a
# real private controller draft. Finalization must reject the missing stream.
{
  printf '#!/usr/bin/env bash\nset -euo pipefail\n'
  declare -f incus owner guest
  cat <<'COPY_PROBE'
owner list
if guest test -e /srv/workspaces/RemoteExactAdmission; then
  printf 'unexpected untransferred workspace\n'
  exit 1
fi
COPY_PROBE
} > "$STATE/exact-copy-probe"
chmod 0700 "$STATE/exact-copy-probe"
python3 - "$STATE" "$source" <<'COPY_CASE'
import hashlib,json,pathlib,sys,tarfile
state=pathlib.Path(sys.argv[1]); source=pathlib.Path(sys.argv[2])
archive=state/"copy-admission-draft.tar"
with tarfile.open(archive,"w") as output:
    output.add(source,arcname=".")
archive.chmod(0o600)
entries={}
with tarfile.open(archive) as draft:
    for entry in draft:
        name=entry.name.removeprefix("./").rstrip("/")
        if name in ("", "."): continue
        if entry.isfile(): entries[name]="f:"+hashlib.sha256(draft.extractfile(entry).read()).hexdigest()
        elif entry.isdir(): entries[name]="d:"
        elif entry.issym(): entries[name]="l:"+entry.linkname
        else: raise RuntimeError("unexpected draft entry type")
fixture=json.loads((state/"exact-integration.json").read_text())
fixture.update(command="sync",arguments=[],copyAdmission=True,
               probe=[str(state/"exact-copy-probe")],
               source={"schema":1,"source":str(source),
                       "archiveDigest":hashlib.sha256(archive.read_bytes()).hexdigest(),
                       "archiveSize":archive.stat().st_size,
                       "treeDigest":hashlib.sha256(json.dumps(entries,sort_keys=True,separators=(",",":")).encode()).hexdigest(),
                       "requestedName":"RemoteExactAdmission","explicitName":True,"targetProfile":"yard"})
(state/"exact-copy.json").write_text(json.dumps(fixture))
COPY_CASE
PATH="$STATE/client-bin:$PATH" python3 dev/e2e/exact-operation-ssh.py "$STATE/exact-copy.json"
projects_before="$(owner list)"
install -m 0600 /dev/null "$STATE/copy-decline.log"
if controller sync "$source" --name RemoteExactDeclined </dev/null > "$STATE/copy-decline.log" 2>&1; then
  die 'unconfirmed remote copy was accepted'
fi
[ "$(owner list)" = "$projects_before" ] || die 'declined copy reserved or committed a project'
guest test ! -e /srv/workspaces/RemoteExactDeclined
controller sync "$source" --yes >/dev/null
printf '%s\nsecond snapshot\n' "$MARKER" > "$source/result.txt"
controller sync "$source" --yes >/dev/null
for name in RemoteExactCopy RemoteExactCopy-2; do
  # shellcheck disable=SC2016 # jq variables are intentionally literal.
  guest jq --arg name "$name" --arg yard "$YARD_NAME" -e \
    '.identityVersion == 2 and .projectId == $name and .name == $name and .yard == $yard' \
    "/srv/workspaces/$name/.subyard-meta.json" >/dev/null
  guest test -L "/srv/workspaces/$name/src/link"
  guest grep -Fxq synthetic-hidden "/srv/workspaces/$name/src/.hidden"
  owner list --complete-projects | grep -Fxq "$name" || die 'owner finalized copy is absent from registry'
done
guest grep -Fxq 'first snapshot' /srv/workspaces/RemoteExactCopy/src/result.txt
guest grep -Fxq 'second snapshot' /srv/workspaces/RemoteExactCopy-2/src/result.txt
install -m 0600 /dev/null "$STATE/copy-collision.log"
if controller sync "$source" --name RemoteExactCopy --yes > "$STATE/copy-collision.log" 2>&1; then
  die 'explicit remote name collision was accepted'
fi
guest grep -Fxq 'first snapshot' /srv/workspaces/RemoteExactCopy/src/result.txt
printf 'ok: physical remote copy decline, admission, stream verification, commit, suffix and collision\n'
# A task-owned guest-local Git origin proves clone preparation never needs a
# controller repository, credential or external Git service.
git_root="/tmp/subyard-exact-git-$token"
guest install -d -o dev -g dev -m 0700 "$git_root"
guest runuser -u dev -- sh -eu -c '
  root=$1
  git init --bare "$root/origin.git" >/dev/null
  git init "$root/work" >/dev/null
  printf "original revision\n" > "$root/work/result.txt"
  git -C "$root/work" add result.txt
  git -C "$root/work" -c user.name=Fixture -c user.email=fixture@example.invalid commit -m original >/dev/null
  git -C "$root/work" push "$root/origin.git" HEAD:refs/heads/main >/dev/null
  git --git-dir="$root/origin.git" symbolic-ref HEAD refs/heads/main
' sh "$git_root"
guest runuser -u dev -- git --git-dir="$git_root/origin.git" rev-parse HEAD > "$STATE/approved-clone-revision"
export git_root
{
  printf '#!/usr/bin/env bash\nset -euo pipefail\n'
  declare -f incus owner guest
  cat <<'CLONE_NATIVE'
case "$1" in
  probe) owner list; guest test ! -e /srv/workspaces/RemoteExactPinnedClone ;;
  advance) guest runuser -u dev -- sh -eu -c '
    root=$1
    printf "advanced revision\n" >> "$root/work/result.txt"
    git -C "$root/work" add result.txt
    git -C "$root/work" -c user.name=Fixture -c user.email=fixture@example.invalid commit -m advanced >/dev/null
    git -C "$root/work" push "$root/origin.git" HEAD:refs/heads/main >/dev/null
  ' sh "$git_root" ;;
  verify)
    expected="$(cat "$STATE/approved-clone-revision")"
    [ "$(guest runuser -u dev -- git -C /srv/workspaces/RemoteExactPinnedClone/src rev-parse HEAD)" = "$expected" ]
    [ "$(guest runuser -u dev -- git --git-dir="$git_root/origin.git" rev-parse HEAD)" != "$expected" ]
    guest grep -Fxq 'original revision' /srv/workspaces/RemoteExactPinnedClone/src/result.txt
    if guest grep -Fxq 'advanced revision' /srv/workspaces/RemoteExactPinnedClone/src/result.txt; then exit 1; fi
    ;;
esac
CLONE_NATIVE
} > "$STATE/exact-clone-native"
chmod 0700 "$STATE/exact-clone-native"
python3 - "$STATE" "$git_root" <<'CLONE_CASE'
import json,pathlib,sys
state=pathlib.Path(sys.argv[1]);origin="file://"+sys.argv[2]+"/origin.git"
fixture=json.loads((state/"exact-integration.json").read_text())
fixture.update(command="clone",arguments=[origin,"--name","RemoteExactPinnedClone"],
               probe=[str(state/"exact-clone-native"),"probe"],
               pinnedClone={"advance":[str(state/"exact-clone-native"),"advance"],
                            "verify":[str(state/"exact-clone-native"),"verify"]})
(state/"exact-clone.json").write_text(json.dumps(fixture))
CLONE_CASE
PATH="$STATE/client-bin:$PATH" python3 dev/e2e/exact-operation-ssh.py "$STATE/exact-clone.json"
controller_logged "$STATE/clone.log" clone "file://$git_root/origin.git" --name RemoteExactClone --yes
[ "$(guest runuser -u dev -- git -C /srv/workspaces/RemoteExactClone/src rev-parse HEAD)" = "$(guest runuser -u dev -- git --git-dir="$git_root/origin.git" rev-parse HEAD)" ] \
  || die 'public remote clone did not verify its pinned native revision'
guest jq --arg yard "$YARD_NAME" -e '.identityVersion == 2 and .projectId == "RemoteExactClone" and .yard == $yard' \
  /srv/workspaces/RemoteExactClone/.subyard-meta.json >/dev/null
owner list --complete-projects | grep -Fxq RemoteExactClone || die 'remote clone did not finalize owner registry'
[ "$(sha256sum "$controller_registration")" = "$controller_before" ] || die 'remote clone changed controller registration'
printf 'ok: public remote clone native revision, owner metadata/registry and controller isolation\n'

# Export is bounded/never: EOF without --yes succeeds without a prompt. The
# controller keeps its prepared source and receives the patch; owner exports
# and the independent snapshot remain unchanged.
source_before="$(sha256sum "$source/result.txt")"
owner_exports_before="$(find "$STATE/owner-data/exports" -type f -exec sha256sum {} + 2>/dev/null || true)"
guest sh -eu -c 'printf "guest export mutation\n" >> /srv/workspaces/RemoteExactCopy/src/result.txt'
# Raw protocol negative cases do not introduce a prompt for bounded export.
# Descriptor facts come from a real retained controller archive; the owner
# treats the controller destination as an opaque label and never opens it.
{
  printf '#!/usr/bin/env bash\nset -euo pipefail\n'
  declare -f incus owner guest
  cat <<'EXPORT_NATIVE'
case "$1" in
  probe)
    owner list
    sha256sum "$source/result.txt"
    guest stat -c '%d:%i:%f:%u:%g' /srv/workspaces/RemoteExactCopy
    guest sha256sum /srv/workspaces/RemoteExactCopy/src/result.txt
    [ ! -e "$STATE/controller-data/exports/RemoteExactCopy-negative.patch" ]
    ;;
  drift) guest sh -eu -c 'printf "negative export source drift\n" >> /srv/workspaces/RemoteExactCopy/src/result.txt' ;;
  present|absent)
    [[ "$2" =~ ^physical-exact-[[:xdigit:]]{32}$ ]] || exit 2
    temporary="/tmp/subyard-export-$2"
    if [ "$1" = absent ]; then
      guest test ! -e "$temporary"
      guest test ! -L "$temporary"
    else
      guest test -d "$temporary"
      guest test ! -L "$temporary"
      [ "$(guest stat -c '%d:%i:%u' "$temporary")" = "$3" ]
      [ "$(guest stat -c '%a' "$temporary")" = 700 ]
    fi
    ;;
esac
EXPORT_NATIVE
} > "$STATE/exact-export-native"
chmod 0700 "$STATE/exact-export-native"
export source
python3 - "$STATE" "$source" <<'EXPORT_CASE'
import hashlib,json,pathlib,stat,sys,tarfile
state=pathlib.Path(sys.argv[1]);source=pathlib.Path(sys.argv[2])
archive=state/"export-negative-draft.tar"
with tarfile.open(archive,"w") as output: output.add(source,arcname=".")
archive.chmod(0o600)
entries={}
with tarfile.open(archive) as draft:
    for entry in draft:
        name=entry.name.removeprefix("./").rstrip("/")
        if name in ("", ".") or ".git" in name.split("/"): continue
        if entry.isfile(): entries[name]="f:"+hashlib.sha256(draft.extractfile(entry).read()).hexdigest()
        elif entry.isdir(): entries[name]="d:"
        elif entry.issym(): entries[name]="l:"+entry.linkname
        else: raise RuntimeError("unexpected export archive entry type")
directory=state/"controller-data/exports";destination=directory/"RemoteExactCopy-negative.patch"
def facts(path):
    info=path.lstat()
    if not stat.S_ISDIR(info.st_mode): raise RuntimeError("export scope is not a native directory")
    return [info.st_dev,info.st_ino,info.st_uid,info.st_gid,(1<<31)|stat.S_IMODE(info.st_mode)]
before=facts(directory) if directory.exists() else None
ancestor=directory.parent
while not ancestor.exists(): ancestor=ancestor.parent
binding=hashlib.sha256(json.dumps([str(destination),before,str(ancestor),facts(ancestor)],separators=(",",":")).encode()).hexdigest()
fixture=json.loads((state/"exact-integration.json").read_text())
fixture.update(command="export",arguments=[],exportAdmission=True,
               probe=[str(state/"exact-export-native"),"probe"],
               drift=[str(state/"exact-export-native"),"drift"],
               temporaryProbe=[str(state/"exact-export-native")],
               export={"schema":1,"projectId":"RemoteExactCopy",
                       "sourceKey":hashlib.sha256(str(source).encode()).hexdigest(),
                       "archiveDigest":hashlib.sha256(archive.read_bytes()).hexdigest(),
                       "archiveSize":archive.stat().st_size,
                       "sourceTree":hashlib.sha256(json.dumps(entries,sort_keys=True,separators=(",",":")).encode()).hexdigest(),
                       "destination":str(destination),"destinationBinding":binding})
(state/"exact-export.json").write_text(json.dumps(fixture))
EXPORT_CASE
PATH="$STATE/client-bin:$PATH" python3 dev/e2e/exact-operation-ssh.py "$STATE/exact-export.json"
controller_logged "$STATE/export.log" export RemoteExactCopy </dev/null
if grep -q 'Proceed?' "$STATE/export.log"; then die 'bounded remote export prompted'; fi
patch="$(sed -n 's/^patch: //p' "$STATE/export.log")"
case "$patch" in "$STATE/controller-data/exports/RemoteExactCopy-"*.patch) ;; *) die 'remote export did not publish at approved controller destination' ;; esac
[ -f "$patch" ] && [ ! -L "$patch" ] || die 'controller export patch is not a regular file'
[ "$(stat -c '%a' "$patch")" = 600 ] || die 'controller export patch is not private'
grep -Fxq '+guest export mutation' "$patch" || die 'controller export patch omitted guest change'
[ "$(sha256sum "$source/result.txt")" = "$source_before" ] || die 'export modified controller source'
[ "$(find "$STATE/owner-data/exports" -type f -exec sha256sum {} + 2>/dev/null || true)" = "$owner_exports_before" ] || die 'remote export wrote owner exports'
guest grep -Fxq 'second snapshot' /srv/workspaces/RemoteExactCopy-2/src/result.txt
if guest grep -Fxq 'guest export mutation' /srv/workspaces/RemoteExactCopy-2/src/result.txt; then die 'export affected independent snapshot'; fi
printf 'ok: bounded remote export native source, controller private patch and sibling retention\n'

# Resolve from the authoritative owner snapshot, then take the discovered
# canonical owner selector through the same owner exact-operation boundary.
controller_logged "$STATE/projects-before-remove.log" list --live
controller_logged "$STATE/owner-yards.json" yards --json
owner_selector="$(python3 - "$STATE/owner-yards.json" "$YARD_NAME" "$INSTANCE" <<'OWNER_SELECTOR'
import json,pathlib,re,sys
yard,instance=sys.argv[2:];matches=[]
for row in json.loads(pathlib.Path(sys.argv[1]).read_text()):
    ref=row.get("yardRef",{})
    if ref.get("yardName")!=yard: continue
    if row.get("accessKind")!="remote" or row.get("yardInstanceName")!=instance or row.get("ownerEndpoint","") not in ("","integration-owner"):
        raise SystemExit("integration-remote: discovered owner authority mismatch")
    host=ref.get("hostId","")
    if not re.fullmatch(r"[A-Za-z0-9_.][A-Za-z0-9_.-]*",host) or host in (".",".."):
        raise SystemExit("integration-remote: invalid discovered owner identity")
    matches.append(host+"/"+yard)
if len(matches)!=1: raise SystemExit("integration-remote: canonical owner identity is ambiguous or missing")
print(matches[0])
OWNER_SELECTOR
)"
PATH="$STATE/client-bin:$PATH" SUBYARD_CONFIG_HOME="$CONTROLLER_CONFIG" SUBYARD_HOME="$STATE/controller-data" SUBYARD_HOST_ID="integration-controller-$token" \
  "$ROOT/.build/yard" -Y "$owner_selector" remove RemoteExactCopy-2 --yes >/dev/null
guest test ! -e /srv/workspaces/RemoteExactCopy-2
if owner list --complete-projects | grep -Fxq RemoteExactCopy-2; then die 'removed copy remains owner-registered'; fi
controller_logged "$STATE/projects-after-remove.log" list --live
if grep -Eq '(^|[[:space:]])RemoteExactCopy-2([[:space:]]|$)' "$STATE/projects-after-remove.log"; then die 'authoritative controller inventory retained removed ghost'; fi
controller_logged "$STATE/projects-cached-after-remove.log" list
if grep -Eq '(^|[[:space:]])RemoteExactCopy-2([[:space:]]|$)' "$STATE/projects-cached-after-remove.log"; then die 'cached controller inventory retained removed ghost'; fi
[ "$(sha256sum "$source/result.txt")" = "$source_before" ] || die 'remote remove affected controller source'
guest grep -Fxq 'first snapshot' /srv/workspaces/RemoteExactCopy/src/result.txt
guest grep -Fxq 'guest export mutation' /srv/workspaces/RemoteExactCopy/src/result.txt
guest test -d /srv/workspaces/RemoteExactClone/src/.git
printf 'ok: discovered owner exact removal, native absence, refreshed inventory and sibling/source retention\n'
owner stop --yes
python3 - "$STATE" <<'LIFECYCLE'
import json, pathlib, sys
state=pathlib.Path(sys.argv[1])
fixture=json.loads((state/"exact-integration.json").read_text())
fixture.update(command="start", arguments=[])
(state/"exact-lifecycle.json").write_text(json.dumps(fixture))
LIFECYCLE
PATH="$STATE/client-bin:$PATH" python3 dev/e2e/exact-operation-ssh.py "$STATE/exact-lifecycle.json"
owner_before="$(sha256sum "$config")"
for verb in enable disable; do
  install -m 0600 /dev/null "$STATE/stopped-$verb.log"
  if controller integration "$verb" claude > "$STATE/stopped-$verb.log" 2>&1; then die "stopped remote $verb was accepted"; fi
  if ! grep -q 'running yard' "$STATE/stopped-$verb.log"; then
    report_cli_failure "$STATE/stopped-$verb.log"
    die 'stopped refusal did not explain running-yard precondition'
  fi
  if grep -q 'Proceed?' "$STATE/stopped-$verb.log"; then die 'stopped remote request prompted'; fi
  [ "$(sha256sum "$config")" = "$owner_before" ] || die 'stopped remote mutation changed owner desired'
  [ "$(incus list "$INSTANCE" --project "$PROJECT" --format json | jq -r '.[0].status')" = Stopped ] || die 'stopped remote request started yard'
done
controller integration status --json | jq -e '.observed == "stopped" and .selection.requested == []' >/dev/null
[ "$(sha256sum "$controller_registration")" = "$controller_before" ] || die 'controller registration was rewritten'
printf 'ok: real remote enable/disable/status, owner selection, no-op, data preservation and stopped refusal\n'
