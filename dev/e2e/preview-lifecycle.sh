#!/usr/bin/env bash
# Synthetic static projects and real initialized yards, only on allocated VMs.
set -euo pipefail
umask 022
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
RUN_ID="${SUBYARD_E2E_RUN_ID:?run through preview-acceptance.sh}"
VM="${SUBYARD_E2E_VM:?allocated VM is required}"
[[ "$RUN_ID" =~ ^[0-9a-f]+$ ]] || { printf 'preview-lifecycle: invalid run identity\n' >&2; exit 2; }
[ "${SUBYARD_E2E_TYPE:-}" = subyard-pair ] || { printf 'preview-lifecycle: requires a disposable pair\n' >&2; exit 2; }
PHASE="${1:-}"
REMOTE_ONLY="${SUBYARD_PREVIEW_REMOTE_ONLY:-0}"
case "$REMOTE_ONLY" in 0|1) ;; *) printf 'preview-lifecycle: invalid fixture scope\n' >&2; exit 2 ;; esac
case "$VM:$PHASE" in 1:controller|2:owner-setup|2:owner-cleanup) ;; *) printf 'preview-lifecycle: invalid phase or VM\n' >&2; exit 2 ;; esac
STATE="/var/tmp/subyard-preview-$RUN_ID"
MARKER="subyard-preview-acceptance-v1:$RUN_ID:$VM"
NAME="pv-$RUN_ID"
NORMAL_PID='' HELPER_PID='' BUSY_PID=''
fail() {
  printf 'preview-lifecycle: %s\n' "$*" >&2
  # These fixture logs contain only synthetic project and SSH diagnostics.
  local log
  for log in "$STATE/busy-code.log" "$STATE"/code-*.log "$STATE/helper.err"; do
    [ ! -f "$log" ] || [ ! -s "$log" ] || {
      printf 'preview-lifecycle: %s\n' "${log##*/}" >&2
      tail -n 30 "$log" >&2
    }
  done
  exit 1
}
export SUBYARD_OPERATOR_HOME="$HOME" SUBYARD_CONFIG_HOME="$STATE/config" SUBYARD_HOME="$STATE/data"
export SUBYARD_REPOSITORY_ROOT="$ROOT" SUBYARD_NO_AUDIT=1 SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1 MIN_DISK_GIB=1
export STORAGE_PATH="$HOME/.cache/subyard-e2e-platform/incus/incus/storage"
export PATH="$STATE/bin:$ROOT/bin:/usr/local/bin:/usr/bin:/bin"

incus() {
  if [ -S /var/lib/incus/unix.socket ] && [ ! -w /var/lib/incus/unix.socket ]; then
    sudo -n /usr/bin/incus "$@"
  else
    /usr/bin/incus "$@"
  fi
}
yard() {
  if ! id -nG | tr ' ' '\n' | grep -Fxq incus-admin \
    && id -nG "$(id -un)" | tr ' ' '\n' | grep -Fxq incus-admin; then
    local command
    printf -v command '%q ' timeout --foreground 1800 "$ROOT/.build/yard" "$@"
    sg incus-admin -c "exec $command"
  else
    timeout --foreground 1800 "$ROOT/.build/yard" "$@"
  fi
}
peer() {
  local argument quoted command=''
  for argument in "$@"; do
    printf -v quoted '%q' "$argument"
    command+="${command:+ }$quoted"
  done
  ssh -F "${SUBYARD_PREVIEW_PEER_CONFIG:?}" peer-admin -- "$command"
}
stop_sessions() {
  local pid
  if [ -f "$STATE/code.pid" ]; then
    pid="$(cat "$STATE/code.pid")"
    [[ "$pid" =~ ^[0-9]+$ ]] && kill "$pid" 2>/dev/null || true
    rm -f "$STATE/code.pid"
  fi
  for pid in "$HELPER_PID" "$NORMAL_PID" "$BUSY_PID"; do
    [ -z "$pid" ] || kill "$pid" 2>/dev/null || true
    [ -z "$pid" ] || wait "$pid" 2>/dev/null || true
  done
  NORMAL_PID='' HELPER_PID='' BUSY_PID=''
}
cleanup_state() {
  local failed=0 name instance project managed
  [ -e "$STATE" ] || return 0
  [ -d "$STATE" ] && [ ! -L "$STATE" ] && [ "$(cat "$STATE/.marker" 2>/dev/null)" = "$MARKER" ] \
    || { printf 'preview-lifecycle: refusing unowned cleanup\n' >&2; return 1; }
  stop_sessions
  if [ -f "$STATE/sshd.pid" ]; then
    sudo -n python3 - "$STATE" <<'PY' || failed=1
import os, pathlib, signal, sys
root = pathlib.Path(sys.argv[1])
pid = int((root / "sshd.pid").read_text())
try:
    command = pathlib.Path("/proc", str(pid), "cmdline").read_bytes()
except FileNotFoundError:
    pass
else:
    assert str(root / "sshd_config").encode() in command, "unowned sshd"
    os.kill(pid, signal.SIGTERM)
PY
  fi
  # A sentinel retains the reconciled host substrate while the temporary yards go.
  install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/platform-sentinel"
  printf 'SSH_PORT=64995\n' > "$SUBYARD_CONFIG_HOME/yards/platform-sentinel/config.env"
  chmod 0600 "$SUBYARD_CONFIG_HOME/yards/platform-sentinel/config.env"
  for name in default "$NAME"; do
    [ -f "$SUBYARD_CONFIG_HOME/yards/$name/config.env" ] || continue
    grep -Fqx "# $MARKER" "$SUBYARD_CONFIG_HOME/yards/$name/config.env" || { failed=1; continue; }
    if [ "$name" = default ]; then instance=yard; project=subyard; else instance="yard-$name"; project="subyard-$name"; fi
    managed="$(incus config get "$instance" user.subyard.managed --project "$project" 2>/dev/null || true)"
    if [ -z "$managed" ] || [ "$managed" = true ]; then
      yard -Y "$name" teardown --yes > "$STATE/cleanup-$name.log" 2>&1 || failed=1
    else
      failed=1
    fi
  done
  if [ -f "$SUBYARD_CONFIG_HOME/yards/preview-remote/config.env" ]; then
    yard remote remove preview-remote --yes > "$STATE/cleanup-remote.log" 2>&1 || failed=1
  fi
  if [ -f "$STATE/ssh-config.backup" ]; then
    install -m 0600 "$STATE/ssh-config.backup" "$HOME/.ssh/config" || failed=1
  elif [ -f "$STATE/ssh-config.absent" ]; then
    rm -f "$HOME/.ssh/config"
  fi
  if [ "$failed" = 0 ]; then
    sudo -n find "$STATE" -depth -delete || return 1
  else
    printf 'preview-lifecycle: cleanup failed; marker-owned evidence retained\n' >&2
  fi
  return "$failed"
}
if [ "$PHASE" = owner-cleanup ]; then cleanup_state; exit; fi
[ ! -e "$STATE" ] && [ ! -L "$STATE" ] || fail 'fixture state already exists'
sudo -n true || fail 'disposable VM requires passwordless sudo'
[ -x "$ROOT/.build/yard" ] || make -C "$ROOT" build > /dev/null
install -d -m 0700 "$STATE" "$STATE/bin" "$STATE/config/yards" "$STATE/data" "$HOME/.ssh"
printf '%s\n' "$MARKER" > "$STATE/.marker"
if [ -f "$HOME/.ssh/config" ]; then
  cp "$HOME/.ssh/config" "$STATE/ssh-config.backup"
else
  touch "$STATE/ssh-config.absent"
fi
cleanup() {
  local rc=$?
  trap - EXIT INT TERM
  set +e
  cleanup_state || rc=3
  exit "$rc"
}
trap cleanup EXIT INT TERM

setup_yard() {
  local name="$1" port config instance project platform marker temporary
  if [ "$name" = default ]; then instance=yard; project=subyard; else instance="yard-$name"; project="subyard-$name"; fi
  ! incus info "$instance" --project "$project" >/dev/null 2>&1 || fail 'fixture target already exists'
  port="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])')"
  install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/$name"
  config="$SUBYARD_CONFIG_HOME/yards/$name/config.env"
  cat > "$config" <<EOF
# $MARKER
SSH_PORT=$port
HOST_BASE=$STATE/host-$name
RESTRICTED_DISK_PATHS=$STATE/host-$name
CODING_TOOL_INTEGRATIONS=''
ENVIRONMENT_PROFILES=
FORWARD_SSH_AGENT=0
HOST_CLAUDE_MD=
HOST_CODEX_AGENTS_MD=
HOST_OPENCODE_AGENTS_MD=
EOF
  chmod 0600 "$config"
  platform="$HOME/.cache/subyard-e2e-platform"
  marker="$platform/.subyard-e2e-platform-marker"
  [ ! -L "$HOME/.cache" ] && [ ! -L "$platform" ] || fail 'unsafe platform directory'
  if [ -e "$marker" ] || [ -L "$marker" ]; then
    [ -f "$marker" ] && [ ! -L "$marker" ] \
      && [ "$(sudo -n cat "$marker")" = subyard-e2e-platform-v1 ] || fail 'unexpected platform marker'
  fi
  sudo -n install -d -o "$(id -u)" -g "$(id -g)" -m 0711 "$platform"
  yard -Y "$name" init --yes > "$STATE/init-$name.log" 2>&1 || {
    tail -n 80 "$STATE/init-$name.log" >&2
    fail 'real yard init failed'
  }
  yard -Y "$name" start --yes > "$STATE/start-$name.log" 2>&1 || fail 'real yard start failed'
  incus storage show default --project default >/dev/null
  incus network show incusbr0 --project default >/dev/null
  temporary="$(mktemp "$platform/.platform-marker.XXXXXX")"
  printf '%s\n' subyard-e2e-platform-v1 > "$temporary"
  chmod 0600 "$temporary"
  mv -fT "$temporary" "$marker"
  incus exec "$instance" --project "$project" -- sh -eu -c '
    test -x /usr/local/bin/subyard-preview
    test "$(stat -c %u:%a /usr/local/bin/subyard-preview)" = 0:755
  ' || fail 'provisioned helper ownership or mode is wrong'
  yard -Y "$name" init --yes > "$STATE/reinit-$name.log" 2>&1 || fail 'repeat init failed'
}

if [ "$PHASE" = owner-setup ]; then
  setup_yard "$NAME"
  printf '%s\n' "$ROOT" > "$STATE/source-root"
  trap - EXIT INT TERM
  printf 'ok: remote owner yard initialized with installed preview helper\n'
  exit 0
fi

if [ "$REMOTE_ONLY" = 0 ]; then
  setup_yard default
  setup_yard "$NAME"
fi
install -d -m 0700 "$STATE/PreviewFixture/site"
git -C "$STATE/PreviewFixture" init -q
printf 'initial preview\n' > "$STATE/PreviewFixture/site/index.html"
# Match ordinary user multiplexing so a normal session is already connected.
cat >> "$HOME/.ssh/config" <<'EOF'

Host *
    BatchMode yes
    ConnectTimeout 5
    LogLevel ERROR
    ControlMaster auto
    ControlPath ~/.ssh/subyard-cm-%C
    ControlPersist no
EOF

cat > "$STATE/bin/code" <<'PY'
#!/usr/bin/env python3
import json, os, pathlib, subprocess, sys
from urllib.parse import urlsplit, unquote
state = pathlib.Path(os.environ["PREVIEW_TEST_STATE"])
(state / "code.called").touch()
assert len(sys.argv) == 2, "unexpected code arguments"
workspace = json.loads(pathlib.Path(sys.argv[1]).read_text())
alias = os.environ["PREVIEW_EXPECTED_ALIAS"]
authority = "ssh-remote+" + alias
assert workspace["remoteAuthority"] == authority, "incorrect resolved code authority"
folder = urlsplit(workspace["folders"][0]["uri"])
assert folder.scheme == "vscode-remote" and folder.netloc == authority, "incorrect folder authority"
(state / "project.path").write_text(unquote(folder.path))
with (state / "code-ssh.log").open("wb") as output:
    process = subprocess.Popen(["ssh", "-N", "-T", alias], stdout=output, stderr=output, start_new_session=True)
(state / "code.pid").write_text(str(process.pid))
PY
chmod 0755 "$STATE/bin/code"
export PREVIEW_TEST_STATE="$STATE"

assert_no_listener() {
  ! ss -Hltn 'sport = :8765' | grep -q . || fail 'unexpected preview listener remains'
}
check_preview() {
  local name="$1" alias="$2" command path
  export PREVIEW_EXPECTED_ALIAS="$alias.code"
  yard -Y "$name" sync "$STATE/PreviewFixture" --yes > "$STATE/sync-$name.log" 2>&1 || {
    tail -n 80 "$STATE/sync-$name.log" >&2
    fail 'fixture sync failed'
  }
  ssh -G "$alias" > "$STATE/normal.options" 2>/dev/null
  ssh -G "$alias.code" > "$STATE/code.options" 2>/dev/null
  ! grep -q '^localforward ' "$STATE/normal.options" || fail 'normal alias has a preview forward'
  sed 's/\[127\.0\.0\.1\]/127.0.0.1/g' "$STATE/code.options" \
    | grep -Fqx 'localforward 127.0.0.1:8765 127.0.0.1:8765' || fail 'code alias has wrong forwarding'
  grep -Fqx 'exitonforwardfailure yes' "$STATE/code.options" || fail 'code alias permits silent forwarding failure'
  grep -q 'subyard-code-cm-' "$STATE/code.options" || fail 'code alias lacks isolated multiplexing'
  ! cmp -s <(grep '^controlpath ' "$STATE/normal.options") <(grep '^controlpath ' "$STATE/code.options") \
    || fail 'normal and code aliases share a control socket'
  ssh -T "$alias" -- sleep 120 > "$STATE/normal-ssh.log" 2>&1 & NORMAL_PID=$!
  sleep 0.2
  kill -0 "$NORMAL_PID" || { cat "$STATE/normal-ssh.log" >&2; fail 'ordinary SSH session failed'; }
  assert_no_listener
  python3 - "$STATE/busy.ready" <<'PY' > "$STATE/busy.log" 2>&1 &
import pathlib, socket, sys, time
listener = socket.socket()
listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
listener.bind(("127.0.0.1", 8765)); listener.listen()
pathlib.Path(sys.argv[1]).touch()
time.sleep(120)
PY
  BUSY_PID=$!
  for _ in {1..50}; do [ ! -f "$STATE/busy.ready" ] || break; sleep 0.1; done
  [ -f "$STATE/busy.ready" ] || fail 'collision listener failed'
  rm -f "$STATE/code.called"
  if yard -Y "$name" code PreviewFixture > "$STATE/busy-code.log" 2>&1; then fail 'yard code accepted an occupied preview port'; fi
  [ ! -f "$STATE/code.called" ] || fail 'VS Code was invoked before the collision check'
  grep -Fq 'preview port 127.0.0.1:8765 is unavailable' "$STATE/busy-code.log" || fail 'collision diagnostic is unclear'
  kill "$BUSY_PID"; wait "$BUSY_PID" 2>/dev/null || true; BUSY_PID=''
  rm -f "$STATE/busy.ready"
  yard -Y "$name" code PreviewFixture > "$STATE/code-$name.log" 2>&1 || fail 'yard code failed'
  [ -f "$STATE/code.called" ] && [ -s "$STATE/project.path" ] || fail 'VS Code did not receive the workspace'
  path="$(cat "$STATE/project.path")"
  printf -v command 'cd %q; printf "%%s\\n" "$$" > .preview-test.pid; exec subyard-preview site' "$path"
  ssh -T "$alias" -- "$command" > "$STATE/helper.log" 2> "$STATE/helper.err" & HELPER_PID=$!
  for _ in {1..100}; do
    grep -Fqx 'Preview: http://127.0.0.1:8765/' "$STATE/helper.log" && break
    kill -0 "$HELPER_PID" || fail 'foreground helper exited before readiness'
    sleep 0.1
  done
  [ "$(cat "$STATE/helper.log")" = 'Preview: http://127.0.0.1:8765/' ] || fail 'helper readiness URL is incorrect'
  for _ in {1..50}; do
    curl -fsS --max-time 3 -D "$STATE/headers" http://127.0.0.1:8765/ > "$STATE/page" 2>/dev/null && break
    sleep 0.1
  done
  [ "$(cat "$STATE/page")" = 'initial preview' ] || fail 'forwarded initial page is incorrect'
  grep -Fqi 'Cache-Control: no-store' "$STATE/headers" || fail 'forwarded response lacks no-store'
  [ "$(ss -Hltn 'sport = :8765' | awk '{print $4}')" = 127.0.0.1:8765 ] || fail 'controller preview listener is not loopback-only'
  printf -v command 'cd %q; python3 -c %q' "$path" 'import pathlib; rows=pathlib.Path("/proc/net/tcp").read_text().splitlines()[1:]; listeners=[r.split()[1] for r in rows if r.split()[3]=="0A" and r.split()[1].endswith(":223D")]; assert listeners==["0100007F:223D"]'
  ssh -T "$alias" -- "$command" || fail 'yard preview listener is not loopback-only'
  if [ "$name" = preview-remote ]; then
    peer python3 -c 'import pathlib; rows=pathlib.Path("/proc/net/tcp").read_text().splitlines()[1:]; assert not any(r.split()[3]=="0A" and r.split()[1].endswith(":223D") for r in rows)' \
      || fail 'remote owner host has a preview listener'
  fi
  printf -v command 'cd %q; printf "edited preview\\n" > site/index.html' "$path"
  ssh -T "$alias" -- "$command"
  curl -fsS --max-time 3 http://127.0.0.1:8765/ > "$STATE/page" 2>/dev/null
  [ "$(cat "$STATE/page")" = 'edited preview' ] || fail 'live edit did not reach the controller'
  printf -v command 'cd %q; kill "$(cat .preview-test.pid)"; rm -f .preview-test.pid' "$path"
  ssh -T "$alias" -- "$command"
  wait "$HELPER_PID" 2>/dev/null || true; HELPER_PID=''
  if curl -fsS --max-time 3 http://127.0.0.1:8765/ >/dev/null 2>&1; then fail 'preview survived helper termination'; fi
  [ ! -s "$STATE/helper.err" ] || fail 'helper emitted unexpected diagnostics'
  stop_sessions
  for _ in {1..50}; do
    ss -Hltn 'sport = :8765' | grep -q . || break
    sleep 0.1
  done
  assert_no_listener
  printf 'ok: %s preview, live edits, collision gate and foreground shutdown\n' "$name"
}
if [ "$REMOTE_ONLY" = 0 ]; then
  check_preview default yard
  check_preview "$NAME" "yard-$NAME"
fi

# The peer owner SSH server accepts only a temporary synthetic key. Its root
# administration route is used only to create this fixture inside allocated VM2.
PEER_IP="${SUBYARD_PREVIEW_PEER_IP:?}"
[[ "$PEER_IP" =~ ^[0-9a-fA-F:.]+$ ]] || fail 'invalid peer address'
ssh-keygen -q -t ed25519 -N '' -f "$STATE/owner-key"
peer dd "of=$STATE/owner-authorized-key" status=none < "$STATE/owner-key.pub"
peer bash -s -- "$STATE" "$RUN_ID" <<'EOS'
set -euo pipefail
state="$1" run="$2"
test "$(cat "$state/.marker")" = "subyard-preview-acceptance-v1:$run:2"
root="$(cat "$state/source-root")"
case "$root" in /tmp/subyard-worktree.*/src) ;; *) exit 2 ;; esac
ssh-keygen -q -t ed25519 -N '' -f "$state/owner-host-key"
python3 - "$state" "$root" <<'PY'
import pathlib, shlex, sys
state, root = sys.argv[1:]
environment = {
    "HOME": "/home/dev", "SUBYARD_OPERATOR_HOME": "/home/dev",
    "SUBYARD_CONFIG_HOME": state+"/config", "SUBYARD_HOME": state+"/data",
    "SUBYARD_REPOSITORY_ROOT": root, "SUBYARD_NO_AUDIT": "1",
    "SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE": "1",
    "STORAGE_PATH": "/home/dev/.cache/subyard-e2e-platform/incus/incus/storage",
    "PATH": root+"/bin:/usr/local/bin:/usr/bin:/bin",
}
pathlib.Path(state, "owner-env").write_text("\n".join(
    "export "+key+"="+shlex.quote(value) for key,value in environment.items())+"\n")
pathlib.Path(state, "owner-shell").write_text(
    "#!/bin/bash\nset -euo pipefail\nexport BASH_ENV="+shlex.quote(state+"/owner-env")+
    '\n. "$BASH_ENV"\nexec /usr/bin/bash -c "$SSH_ORIGINAL_COMMAND"\n')
PY
chmod 0700 "$state/owner-shell"
chmod 0600 "$state/owner-env" "$state/owner-authorized-key"
chown dev:dev "$state/owner-shell" "$state/owner-env" "$state/owner-authorized-key"
cat > "$state/sshd_config" <<EOF
Port 25222
ListenAddress 0.0.0.0
HostKey $state/owner-host-key
PidFile $state/sshd.pid
AuthorizedKeysFile $state/owner-authorized-key
StrictModes no
PasswordAuthentication no
KbdInteractiveAuthentication no
UsePAM no
AllowUsers dev
AllowTcpForwarding yes
PermitOpen 127.0.0.1:*
PermitTTY no
ForceCommand $state/owner-shell
LogLevel ERROR
EOF
chmod 0600 "$state/sshd_config"
/usr/sbin/sshd -t -f "$state/sshd_config"
nohup /usr/sbin/sshd -D -e -f "$state/sshd_config" > "$state/sshd.log" 2>&1 &
for _ in {1..50}; do
  [ ! -s "$state/sshd.pid" ] || exit 0
  sleep 0.1
done
exit 1
EOS
peer cat "$STATE/owner-host-key.pub" > "$STATE/owner-host-key.pub"
printf 'preview-owner-key ' > "$STATE/owner-known-hosts"
cat "$STATE/owner-host-key.pub" >> "$STATE/owner-known-hosts"
OWNER_ALIAS="preview-owner-$RUN_ID"
cat > "$STATE/owner-ssh" <<EOF
Host $OWNER_ALIAS
    HostName $PEER_IP
    Port 25222
    User dev
    IdentityFile $STATE/owner-key
    IdentitiesOnly yes
    BatchMode yes
    StrictHostKeyChecking yes
    HostKeyAlias preview-owner-key
    UserKnownHostsFile $STATE/owner-known-hosts
    GlobalKnownHostsFile /dev/null
    ConnectTimeout 5
    LogLevel ERROR
EOF
chmod 0600 "$STATE/owner-ssh" "$STATE/owner-known-hosts"
{ printf 'Include %s\n' "$STATE/owner-ssh"; cat "$HOME/.ssh/config"; } > "$STATE/ssh-config.next"
install -m 0600 "$STATE/ssh-config.next" "$HOME/.ssh/config"
yard host add "$OWNER_ALIAS" --yes > "$STATE/host-add.log" 2>&1 || {
  tail -n 80 "$STATE/host-add.log" >&2
  fail 'canonical owner registration failed'
}
yard remote add preview-remote "$OWNER_ALIAS" --yard "$NAME" --yes \
  > "$STATE/remote-add.log" 2>&1 || {
    tail -n 80 "$STATE/remote-add.log" >&2
    fail 'production remote registration failed'
  }
check_preview preview-remote yard-preview-remote
grep -Fqx "proxyjump $OWNER_ALIAS" "$STATE/code.options" || fail 'remote code alias lost ProxyJump'
printf 'ok: remote owner stays free of preview listeners\n'
