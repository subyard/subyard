#!/usr/bin/env bash
# Sourced by the focused Orca bootstrap mode on a disposable lease VM.
# Run: SUBYARD_E2E_ORCA_BOOTSTRAP=1 SUBYARD_E2E_ORCA_SSH_AGENT=1 \
#   bash tests/real-host/orca-bootstrap.sh
# Variables token and pairing belong to the sourcing bootstrap fixture.
# shellcheck disable=SC2154
set -euo pipefail
[ "${SUBYARD_E2E_VM:-}" = 1 ] && [ "${SSH_AGENT:-}" = 1 ] \
  && [ -f "${STATE:-}/.marker" ] \
  && [ "$(<"$STATE/.marker")" = subyard-orca-bootstrap-e2e-v1 ] \
  || { printf 'ssh-agent-e2e: use the marked Orca bootstrap fixture\n' >&2; exit 2; }

agent_terminal=''
agent_project=$PROJECT
agent_instance=$INSTANCE
agent_seed=/tmp/subyard-ssh-agent-$token
agent_key=$STATE/ssh-agent-key
agent_passphrase=subyard-ssh-agent-public-test-fixture-only

agent_root() { incus --project "$agent_project" exec "$agent_instance" -- "$@"; }
agent_dev() { agent_root runuser -l dev -c "$1"; }
agent_client() {
  HOME="$STATE/ssh-agent-client/home" \
    XDG_CONFIG_HOME="$STATE/ssh-agent-client/config" \
    XDG_DATA_HOME="$STATE/ssh-agent-client/data" \
    XDG_STATE_HOME="$STATE/ssh-agent-client/state" \
    LIBGL_ALWAYS_SOFTWARE=1 ORCA_PAIRING_CODE="$pairing" \
    timeout 30 /usr/bin/orca-ide "$@"
}
ssh_agent_cleanup() {
  local failed=0
  if [ -n "$agent_terminal" ]; then
    agent_client terminal close --terminal "$agent_terminal" --tab --json >/dev/null 2>&1 || true
  fi
  yard ssh-agent lock >/dev/null 2>&1 || failed=1
  if [ -f "$SUBYARD_CONFIG_HOME/yards/secondary/config.env" ]; then
    yard -Y secondary ssh-agent lock >/dev/null 2>&1 || failed=1
  fi
  return "$failed"
}
agent_status() {
  yard "$@" ssh-agent status --json | jq -er '.state'
}
agent_unlock() {
  local ttl=$1 passphrase=${2:-$agent_passphrase}
  # pty.fork gives the public command and ssh-add their real controlling terminal.
  # Only this synthetic fixture passphrase is sent; captured terminal bytes are never printed.
  timeout --signal=TERM --kill-after=3s 45 python3 - "$YARD_BIN" "$agent_key" "$ttl" "$passphrase" <<'PY'
import errno
import os
import pty
import re
import select
import signal
import sys
import time

pid, terminal = pty.fork()
if pid == 0:
    os.environ["LC_ALL"] = "C"
    os.execv(sys.argv[1], [sys.argv[1], "ssh-agent", "unlock", "--key", sys.argv[2],
                         "--ttl", sys.argv[3], "--yes"])
deadline = time.monotonic() + 35
buffer = b""
attempts = 0
try:
    while time.monotonic() < deadline:
        done, status = os.waitpid(pid, os.WNOHANG)
        if done:
            if os.waitstatus_to_exitcode(status) != 0 or not attempts:
                raise RuntimeError("public terminal unlock failed")
            break
        readable, _, _ = select.select([terminal], [], [], 0.1)
        if not readable:
            continue
        try:
            chunk = os.read(terminal, 4096)
        except OSError as error:
            if error.errno == errno.EIO:
                continue
            raise
        buffer = (buffer + chunk)[-8192:]
        if b"bad passphrase" in buffer.lower():
            # Exercise ordinary operator cancellation after a rejected password.
            # SIGKILL would bypass CLI cleanup and only test the pending watchdog.
            os.write(terminal, b"\x03")
            buffer = b""
            continue
        if b"enter passphrase for" in buffer.lower():
            if attempts >= 3:
                raise RuntimeError("too many passphrase prompts")
            os.write(terminal, sys.argv[4].encode() + b"\n")
            attempts += 1
            buffer = b""
    else:
        raise RuntimeError("public terminal unlock timed out")
except (OSError, RuntimeError) as failure:
    try:
        os.killpg(pid, signal.SIGTERM)
        cleanup_deadline = time.monotonic() + 3
        while time.monotonic() < cleanup_deadline:
            if os.waitpid(pid, os.WNOHANG)[0]:
                break
            time.sleep(0.05)
        else:
            os.killpg(pid, signal.SIGKILL)
    except ProcessLookupError:
        pass
    except ChildProcessError:
        pass
    # Drain only this fixture's PTY and report bounded CLI diagnostics. Never
    # publish the raw terminal transcript or either synthetic credential value.
    while select.select([terminal], [], [], 0)[0]:
        try:
            chunk = os.read(terminal, 4096)
        except OSError:
            break
        if not chunk:
            break
        buffer = (buffer + chunk)[-8192:]
    diagnostic = re.sub(r"\x1b\[[0-9;]*[A-Za-z]", "", buffer.decode(errors="replace"))
    for value in (sys.argv[2], sys.argv[4]):
        diagnostic = diagnostic.replace(value, "<fixture credential>")
    print(f"ssh-agent-e2e: {failure}; passphrase prompts={attempts}", file=sys.stderr)
    for line in diagnostic.splitlines():
        if line.startswith(("yard:", "SSH key access:")):
            print(line[:1000], file=sys.stderr)
    sys.exit(1)
finally:
    os.close(terminal)
PY
}

agent_prepare_guest() {
  # The only key uploaded is public. Both yards authorize it so socket isolation,
  # rather than a different authorized_keys entry, determines the negative case.
  agent_root sh -eu -c '
    IFS= read -r public_key
    grep -Fxq "$public_key" /home/dev/.ssh/authorized_keys ||
      printf "%s\n" "$public_key" >> /home/dev/.ssh/authorized_keys
  ' < "$agent_key.pub"
  agent_root sh -eu -c '
    directory=$1
    install -d -m 0755 -o dev -g dev "$directory"
    awk '\''$1 == "ssh-ed25519" { print "127.0.0.1 " $1 " " $2 }'\'' \
      /etc/ssh/ssh_host_ed25519_key.pub > "$directory/known_hosts"
    chmod 0644 "$directory/known_hosts"
  ' sh "$agent_seed"
  if ! agent_root test -d "$agent_seed/source.git"; then
    agent_dev "git init -q '$agent_seed/source' &&
      git -C '$agent_seed/source' -c user.name=Fixture -c user.email=fixture@example.invalid \
        commit --allow-empty -qm initial &&
      git clone -q --bare '$agent_seed/source' '$agent_seed/source.git'"
  fi
  for branch in ordinary unlocked reunlocked; do
    agent_dev "git --git-dir='$agent_seed/source.git' update-ref -d refs/heads/$branch"
  done
}
agent_poll_terminal() {
  local expected=$1 deadline=$((SECONDS + 20))
  while ((SECONDS < deadline)); do
    if agent_client terminal read --terminal "$agent_terminal" --json \
      > "$STATE/agent-terminal-read.json" 2> "$STATE/agent-terminal-read.err" &&
      jq -e --arg expected "$expected" '.ok == true and
        (.result.terminal.tail | any(contains($expected)))' \
        "$STATE/agent-terminal-read.json" >/dev/null; then
      return
    fi
    sleep 0.2
  done
  die "Orca terminal did not report $expected"
}
agent_terminal_check() {
  local phase=$1 result=$2
  agent_client terminal send --terminal "$agent_terminal" --text "$phase" --enter --json \
    > "$STATE/agent-terminal-send.json" 2> "$STATE/agent-terminal-send.err" \
    || die 'Orca terminal input failed'
  jq -e '.ok == true and .result.send.accepted == true' "$STATE/agent-terminal-send.json" >/dev/null \
    || die 'Orca terminal rejected input'
  agent_poll_terminal "subyard-agent-$phase:$result"
}

stage 'preparing synthetic encrypted SSH identity and independent second yard'
yard ssh-agent lock >/dev/null
[ -f "$agent_key" ] || ssh-keygen -q -t ed25519 -N "$agent_passphrase" -C subyard-public-e2e -f "$agent_key"
install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/secondary"
if [ ! -f "$SUBYARD_CONFIG_HOME/yards/secondary/config.env" ]; then
cat > "$SUBYARD_CONFIG_HOME/yards/secondary/config.env" <<EOF
INCUS_PROJECT=$PROJECT-secondary
YARD_INSTANCE_NAME=$INSTANCE-secondary
SSH_PORT=$(free_port)
ENVIRONMENT_PROFILES=""
CODING_TOOL_INTEGRATIONS=""
EOF
chmod 0600 "$SUBYARD_CONFIG_HOME/yards/secondary/config.env"
fi
yard -Y secondary init --yes > "$STATE/agent-secondary-init.out" 2>&1 \
  || die 'second yard initialization failed'
# Named yards deliberately finish their first init with desired power stopped.
yard -Y secondary start --yes > "$STATE/agent-secondary-start.out" 2>&1 \
  || die 'second yard did not start for the isolation check'
agent_prepare_guest
agent_project=$PROJECT-secondary agent_instance=$INSTANCE-secondary agent_prepare_guest

agent_client repo list --json > "$STATE/agent-repos.json" 2> "$STATE/agent-repos.err"
if ! jq -e '.result.repos | any(.displayName == "ssh-agent-e2e")' "$STATE/agent-repos.json" >/dev/null; then
  yard clone "file://$agent_seed/source.git" --name ssh-agent-e2e --yes \
    > "$STATE/agent-clone.out" 2>&1 || die 'fixture worktree registration failed'
  agent_client repo list --json > "$STATE/agent-repos.json" 2> "$STATE/agent-repos.err"
fi
agent_repo_id="$(jq -er '.result.repos | map(select(.kind == "git" and
  .displayName == "ssh-agent-e2e")) | select(length == 1) | .[0].id' \
  "$STATE/agent-repos.json")" || die 'Orca did not register the fixture worktree'
agent_worktree="$(jq -er --arg id "$agent_repo_id" \
  '.result.repos[] | select(.id == $id) | .path' "$STATE/agent-repos.json")"
[[ "$agent_worktree" =~ ^/srv/workspaces/[A-Za-z0-9._-]+/src$ ]] \
  || die 'registered fixture worktree has an unsafe path'
agent_remote=ssh://dev@127.0.0.1$agent_seed/source.git
agent_ssh="ssh -F /dev/null -o BatchMode=yes -o PreferredAuthentications=publickey -o IdentityFile=none -o ControlMaster=no -o ControlPath=none -o ControlPersist=no -o StrictHostKeyChecking=yes -o UserKnownHostsFile=$agent_seed/known_hosts -o GlobalKnownHostsFile=/dev/null -o ConnectTimeout=5"
agent_dev "git -C '$agent_worktree' config core.sshCommand '$agent_ssh' &&
  git -C '$agent_worktree' remote set-url origin '$agent_remote'"
agent_fetch="git -C '$agent_worktree' ls-remote origin"
[ "$(agent_status)" = locked ] || die 'new fixture unexpectedly has an active grant'
if agent_dev "$agent_fetch" > "$STATE/agent-before.out" 2>&1; then
  die 'Git authenticated before unlock'
fi

stage 'opening an actual paired Orca terminal before unlock'
agent_root sh -eu -c 'cat > "$1/terminal-check.sh"; chmod 0644 "$1/terminal-check.sh"' sh "$agent_seed" <<'GUEST'
#!/bin/bash
set -eu
[ "${SSH_AUTH_SOCK:-}" = /home/dev/.ssh/subyard-agent.sock ] || exit 1
printf '%s%s\n' 'subyard-agent-' ready
while IFS= read -r phase; do
  case "$phase" in
    unlocked|reunlocked)
      if git push origin "HEAD:refs/heads/$phase" >/dev/null 2>&1; then result=ok; else result=failed; fi ;;
    locked|expired)
      if git ls-remote origin >/dev/null 2>&1; then result=unexpected; else result=denied; fi ;;
    *) exit 2 ;;
  esac
  printf 'subyard-agent-%s:%s\n' "$phase" "$result"
done
GUEST
agent_client terminal create --worktree "id:$agent_repo_id::$agent_worktree" \
  --title subyard-ssh-agent-e2e --command "exec bash $agent_seed/terminal-check.sh" --json \
  > "$STATE/agent-terminal-create.json" 2> "$STATE/agent-terminal-create.err" \
  || die 'stock Orca could not create the fixture terminal'
agent_terminal="$(jq -er 'select(.ok == true) | .result.terminal.handle |
  select(type == "string" and length > 0)' "$STATE/agent-terminal-create.json")"
agent_poll_terminal subyard-agent-ready
agent_orca_pid="$(guest_root systemctl show -p MainPID --value subyard-orca.service)"

stage 'unlocking in a real PTY and proving detached Git and Orca signing'
if agent_unlock 2m wrong-public-fixture-passphrase > "$STATE/agent-wrong-passphrase.out" 2>&1; then
  die 'wrong passphrase unexpectedly unlocked the key'
fi
[ "$(agent_status)" = locked ] || die 'failed passphrase left an active grant'
if agent_dev "$agent_fetch" > "$STATE/agent-wrong-passphrase-fetch.out" 2>&1; then
  die 'Git authenticated after a failed passphrase'
fi
agent_unlock 2m
[ "$(agent_status)" = unlocked ] || die 'grant did not outlive the unlock terminal'
agent_dev "$agent_fetch" > "$STATE/agent-fetch.out" 2>&1 || die 'ordinary guest shell could not authenticate'
yard shell -- git -C "$agent_worktree" ls-remote origin > "$STATE/agent-shell-fetch.out" 2>&1 \
  || die 'yard shell command did not inherit the agent socket'
agent_dev "git -C '$agent_worktree' push origin HEAD:refs/heads/ordinary" \
  > "$STATE/agent-push.out" 2>&1 || die 'ordinary guest shell could not push'
agent_terminal_check unlocked ok
agent_root git --git-dir="$agent_seed/source.git" rev-parse --verify refs/heads/unlocked >/dev/null \
  || die 'Orca terminal push did not create the remote ref'
if agent_dev 'ssh-add -D' > "$STATE/agent-admin.out" 2>&1; then
  die 'guest could modify the host agent'
fi
agent_dev "$agent_fetch" > "$STATE/agent-after-admin.out" 2>&1 \
  || die 'rejected guest mutation removed the signing identity'
[ "$(agent_status -Y secondary)" = locked ] || die 'grant leaked to another yard status'
if agent_project=$PROJECT-secondary agent_instance=$INSTANCE-secondary \
  agent_dev "git -c core.sshCommand='$agent_ssh' ls-remote '$agent_remote'" \
  > "$STATE/agent-secondary-fetch.out" 2>&1; then
  die 'independent second yard authenticated using the first yard grant'
fi
grep -Fq 'Permission denied' "$STATE/agent-secondary-fetch.out" \
  || die 'second yard negative case did not reach SSH authentication'
# The selected private file is outside HOST_BASE, and no guest-visible mount may
# cover it. The new guest also must not gain an ordinary private identity file.
incus --project "$PROJECT" list "$INSTANCE" --format json \
  | jq -e --arg key "$agent_key" --arg instance "$INSTANCE" \
    '[.[] | select(.name == $instance)] | select(length == 1) | .[0] |
    [.expanded_devices[] | select(.type == "disk") | .source // "" |
      select(startswith("/")) | select(. as $source |
        $key == $source or ($key | startswith($source + "/")))] | length == 0' >/dev/null \
  || die 'selected private key lies inside a guest-visible host mount'
agent_root sh -eu -c 'test ! -e /home/dev/.ssh/id_ed25519 && test ! -e /home/dev/.ssh/id_rsa' \
  || die 'guest unexpectedly contains a private SSH identity'
agent_private_scan=0
agent_root grep -rIlE --devices=skip '^-----BEGIN (OPENSSH|RSA|EC|DSA|ENCRYPTED) PRIVATE KEY-----$' \
  /home/dev/.ssh "$agent_seed" "$agent_worktree" > "$STATE/agent-private-key-paths.out" \
  || agent_private_scan=$?
[ "$agent_private_scan" = 1 ] || die 'private-key absence check failed in the guest fixture paths'

stage 'locking the grant and testing fresh authentication in the existing terminal'
yard ssh-agent lock > "$STATE/agent-lock.out" 2>&1
[ "$(agent_status)" = locked ] || die 'lock did not revoke the grant'
if agent_dev "$agent_fetch" > "$STATE/agent-locked-fetch.out" 2>&1; then
  die 'Git authenticated after lock'
fi
agent_terminal_check locked denied

stage 're-unlocking briefly and verifying expiry without restarting Orca'
agent_unlock 10s
agent_dev "$agent_fetch" > "$STATE/agent-reunlocked-fetch.out" 2>&1 \
  || die 'fresh short grant could not authenticate'
agent_terminal_check reunlocked ok
agent_expiry_deadline=$((SECONDS + 20))
while [ "$(agent_status)" != locked ] && ((SECONDS < agent_expiry_deadline)); do sleep 0.5; done
[ "$(agent_status)" = locked ] || die 'grant did not expire within its bound'
if agent_dev "$agent_fetch" > "$STATE/agent-expired-fetch.out" 2>&1; then
  die 'Git authenticated after expiry'
fi
agent_terminal_check expired denied
[ "$(guest_root systemctl show -p MainPID --value subyard-orca.service)" = "$agent_orca_pid" ] \
  || die 'grant lifecycle restarted Orca'
printf 'ok: encrypted owner key unlocked via PTY; ordinary and Orca Git pushes passed; mutation, cross-yard, lock and expiry checks passed\n'
