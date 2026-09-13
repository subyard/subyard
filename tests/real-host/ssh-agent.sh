#!/usr/bin/env bash
# Production owner-host agent, standard SSH transport and cross-session Git acceptance.
# Run only on a disposable VM through dev/agent-e2e.sh.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ENGINE="$ROOT/.build/yard"
OWNER_HOME="$HOME"
PLATFORM="$OWNER_HOME/.cache/subyard-e2e-platform"
SECOND_KIND="${SUBYARD_SSH_AGENT_SECOND_KIND:-container}"
case "$SECOND_KIND" in container|vm) ;; *) printf 'invalid second yard kind\n' >&2; exit 2;; esac
STATE=''; SHELL_PID=''; YARD_A=''; YARD_B=''
die() { printf 'ssh-agent-e2e: %s\n' "$*" >&2; exit 1; }
stage() { printf 'ssh-agent-e2e: %s\n' "$*" >&2; }
[ "${SUBYARD_E2E_VM:-}" = 1 ] && [ "$(id -u)" = 0 ] || die 'requires allocated VM1 root'
free_port() { python3 -c 'import socket; s=socket.socket(); s.bind(("0.0.0.0",0)); print(s.getsockname()[1]); s.close()'; }
run_yard() {
  env HOME="$STATE/home" SUBYARD_OPERATOR_HOME="$STATE/home" \
    SUBYARD_CONFIG_HOME="$STATE/config" SUBYARD_HOME="$STATE/data" \
    STORAGE_PATH="$PLATFORM/incus/incus/storage" HOST_BASE="$STATE/host" \
    RESTRICTED_DISK_PATHS="$STATE/host" SUBYARD_NO_AUDIT=1 \
    SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1 MIN_DISK_GIB=1 \
    "$ENGINE" -Y "$1" "${@:2}"
}
guest_dev() {
  local name="$1"; shift
  incus --project "subyard-$name" exec "yard-$name" --user 1000 --group 1000 \
    --env HOME=/home/dev -- "$@"
}
cleanup() {
  local rc=$?
  trap - EXIT INT TERM
  set +e
  [ -z "$SHELL_PID" ] || kill "$SHELL_PID" 2>/dev/null
  for name in "$YARD_A" "$YARD_B"; do
    [ -n "$name" ] || continue
    run_yard "$name" ssh-agent stop --yes >/dev/null 2>&1
    run_yard "$name" teardown --yes >/dev/null 2>&1
  done
  if [[ "$STATE" = /var/tmp/subyard-ssh-agent.* ]] &&
    [ "$(cat "$STATE/.marker" 2>/dev/null)" = subyard-ssh-agent-e2e-v1 ]; then
    find "$STATE" -depth -delete
  fi
  exit "$rc"
}
STATE="$(mktemp -d /var/tmp/subyard-ssh-agent.XXXXXX)"
printf '%s\n' subyard-ssh-agent-e2e-v1 > "$STATE/.marker"
trap cleanup EXIT INT TERM
stage 'reconciling disposable host baseline with product installer'
install -d -m 0700 "$PLATFORM"
if ! command -v incus >/dev/null || ! incus info >/dev/null 2>&1 ||
  ! incus storage show default --project default >/dev/null 2>&1 ||
  [ ! -d "$PLATFORM/incus" ]; then
  (
    # shellcheck source=tests/helpers/test-context.sh
    . "$ROOT/tests/helpers/test-context.sh"
    setup_test_context "$STATE/bootstrap"
    export SUBYARD_USER=root SUBYARD_OPERATOR_HOME="$OWNER_HOME"
    export SUBYARD_CONFIG_DIR="$ROOT/config" SUBYARD_HOME="$PLATFORM"
    export STORAGE_PATH="$PLATFORM/incus/incus/storage"
    set -a
    # shellcheck source=config/host.env
    . "$ROOT/config/host.env"
    set +a
    bash "$ROOT/scripts/01-install-incus.sh" --yes --zabbly
  )
fi
incus info >/dev/null
incus storage show default --project default >/dev/null
[ -d "$PLATFORM/incus" ] || die 'product platform storage is not present'
if [ -e "$PLATFORM/.subyard-e2e-platform-marker" ]; then
  [ "$(cat "$PLATFORM/.subyard-e2e-platform-marker")" = subyard-e2e-platform-v1 ] || die 'unexpected platform marker'
else
  printf '%s\n' subyard-e2e-platform-v1 > "$PLATFORM/.subyard-e2e-platform-marker"
fi
# The fixture isolates timer files from the normal root profile. Supply the actual
# user manager/linger prerequisites without enabling a second keys-sync timer.
loginctl enable-linger root
systemctl start user@0.service
# VM-yard mode has an explicit QEMU prerequisite; install it only in this
# allocated disposable host, as directed by the product's VM preflight.
if [ "$SECOND_KIND" = vm ] && ! command -v qemu-system-x86_64 >/dev/null; then
  apt-get install -y qemu-system-x86 qemu-utils ovmf
fi
stage 'building candidate and preparing two named yards'
bash "$ROOT/dev/build-engine.sh"
token="$(printf '%s' "${STATE##*.}" | tr '[:upper:]' '[:lower:]')"
YARD_A="ssh-agent-a-$token"; YARD_B="ssh-agent-b-$token"
SSH_PORT_A="$(free_port)"; SSH_PORT_B="$(free_port)"
[ "$SSH_PORT_A" != "$SSH_PORT_B" ] || die 'ports collided; retry fixture'
install -d -m 0700 "$STATE/home" "$STATE/data" "$STATE/config" "$STATE/host" \
  "$STATE/config/yards/$YARD_A" "$STATE/config/yards/$YARD_B" "$STATE/git/seed"
for name in "$YARD_A" "$YARD_B"; do
  port="$SSH_PORT_A"; [ "$name" != "$YARD_B" ] || port="$SSH_PORT_B"
  kind=container; [ "$name" != "$YARD_B" ] || kind="$SECOND_KIND"
  memory=512MiB; [ "$kind" != vm ] || memory=2GiB
  cat > "$STATE/config/yards/$name/config.env" <<CFG
SSH_PORT=$port
YARD_KIND=$kind
LIMITS_CPU=1
LIMITS_MEMORY=$memory
CODING_TOOL_INTEGRATIONS=
ENVIRONMENT_PROFILES=
HOST_MOUNTS=
HOST_LINKS=
FORWARD_SSH_AGENT=0
CFG
  chmod 0600 "$STATE/config/yards/$name/config.env"
  stage "initializing $kind yard"
  run_yard "$name" init --yes > "$STATE/init-$name.log" 2>&1 || {
    tail -60 "$STATE/init-$name.log" >&2
    incus --project "subyard-$name" info "yard-$name" --show-log >&2 || true
    if [ "$kind" = vm ]; then incus --project "subyard-$name" console "yard-$name" --show-log >&2 || true; fi
    die 'yard init failed'
  }
  run_yard "$name" start --yes
done
stage 'serving disposable Git over loopback SSH inside each yard'
for name in git-client-key other-client-key; do ssh-keygen -q -t ed25519 -N '' -f "$STATE/$name"; done
public_key="$(cat "$STATE/git-client-key.pub")"
for name in "$YARD_A" "$YARD_B"; do
  incus --project "subyard-$name" exec "yard-$name" -- bash -se -- "$public_key" <<'GUEST'
set -euo pipefail
fixture=/var/tmp/subyard-agent-git
install -d -m 0755 "$fixture"
printf '%s\n' "$1" > "$fixture/authorized_keys"
chmod 0644 "$fixture/authorized_keys"
ssh-keygen -q -t ed25519 -N '' -f "$fixture/host-key"
install -d -m 0755 -o dev -g dev "$fixture/seed"
runuser -u dev -- bash -se -- "$fixture" <<'SEED'
fixture=$1
git -C "$fixture/seed" init -q -b main
printf 'ssh-agent fixture\n' > "$fixture/seed/fixture.txt"
git -C "$fixture/seed" add fixture.txt
git -C "$fixture/seed" -c user.name=Fixture -c user.email=fixture@example.invalid commit -qm initial
SEED
install -d -m 0755 -o dev -g dev "$fixture/repo.git"
runuser -u dev -- git clone -q --bare "$fixture/seed" "$fixture/repo.git"
cat > "$fixture/sshd_config" <<CFG
Port 22222
ListenAddress 127.0.0.1
HostKey $fixture/host-key
AuthorizedKeysFile $fixture/authorized_keys
StrictModes no
PubkeyAuthentication yes
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitRootLogin no
AllowUsers dev
AllowTcpForwarding no
X11Forwarding no
PermitTTY no
UsePAM yes
UseDNS no
LogLevel ERROR
CFG
cat > /etc/systemd/system/subyard-agent-git.service <<CFG
[Unit]
Description=Disposable SSH-agent Git test
After=network.target
[Service]
ExecStart=/usr/sbin/sshd -D -e -f $fixture/sshd_config
[Install]
WantedBy=multi-user.target
CFG
systemctl daemon-reload
systemctl enable --now subyard-agent-git.service
runuser -u dev -- bash -se <<'CLIENT'
install -d -m 0700 "$HOME/.ssh"
for _ in $(seq 1 50); do
  ssh-keyscan -T 1 -p 22222 127.0.0.1 > "$HOME/.ssh/agent-test-known_hosts" 2>/dev/null && break
  sleep 0.1
done
test -s "$HOME/.ssh/agent-test-known_hosts"
cat > "$HOME/.ssh/config" <<CFG
Host agent-test-git
    HostName 127.0.0.1
    Port 22222
    User dev
    BatchMode yes
    StrictHostKeyChecking yes
    UserKnownHostsFile ~/.ssh/agent-test-known_hosts
CFG
chmod 0600 "$HOME/.ssh/config" "$HOME/.ssh/agent-test-known_hosts"
CLIENT
GUEST
done
repo="ssh://agent-test-git/var/tmp/subyard-agent-git/repo.git"
can_read() { guest_dev "$1" git ls-remote "$repo" HEAD >/dev/null 2>&1; }
! can_read "$YARD_A" || die 'Git unexpectedly worked without a grant'
# Keep a real guest shell open across start, with no SSH_AUTH_SOCK in its environment.
mkfifo "$STATE/existing-shell.in"
exec 9<> "$STATE/existing-shell.in"
run_yard "$YARD_A" shell -- bash -s < "$STATE/existing-shell.in" > "$STATE/existing-shell.out" 2>&1 &
SHELL_PID=$!
printf 'printf "shell-ready\\n"\n' >&9
for _ in $(seq 1 50); do grep -q shell-ready "$STATE/existing-shell.out" && break; sleep 0.1; done
grep -q shell-ready "$STATE/existing-shell.out" || die 'existing shell did not start'
stage 'granting access without any existing host agent'
if ! run_yard "$YARD_A" ssh-agent start --key "$STATE/git-client-key" --ttl 3m --yes; then
  stage 'checking dedicated login and shared guest prerequisites'
  ssh -T -S none -F /dev/null -o BatchMode=yes -o IdentityAgent=none -o IdentitiesOnly=yes \
    -o StrictHostKeyChecking=yes -o "UserKnownHostsFile=$STATE/data/ssh/known_hosts" \
    -i "$STATE/data/ssh/id_ed25519" -p "$SSH_PORT_A" dev@127.0.0.1 true || true
  guest_dev "$YARD_A" sh -c '
    test ! -f /etc/ssh/ssh_config.d/50-subyard-agent.conf || echo shared-config-present
    for directory in "$HOME/.subyard" "$HOME/.subyard/run"; do
      if test -e "$directory"; then stat -c "guest-directory mode=%a uid=%u" "$directory"; fi
    done
  ' || true
  die 'SSH-agent grant failed'
fi
run_yard "$YARD_A" ssh-agent status --json | jq -e '.state == "active"' >/dev/null
printf 'set -e\nunset SSH_AUTH_SOCK\ngit ls-remote %q HEAD\nprintf "git-ready\\n"\nexit\n' "$repo" >&9
exec 9>&-
wait "$SHELL_PID" || { cat "$STATE/existing-shell.out" >&2; die 'existing shell lost agent access'; }
SHELL_PID=''
grep -q git-ready "$STATE/existing-shell.out" || die 'existing shell did not finish Git'
can_read "$YARD_A" || die 'new dev session could not read Git'
run_yard "$YARD_A" clone "$repo" --name agent-clone --yes
run_yard "$YARD_A" shell agent-clone -- test -s fixture.txt
stage 'checking duplicate grant refusal and independent second-yard access'
if run_yard "$YARD_A" ssh-agent start --key "$STATE/other-client-key" --ttl 1m --yes >/dev/null 2>&1; then die 'active grant was silently replaced'; fi
! can_read "$YARD_B" || die 'ungranted second yard borrowed access'
run_yard "$YARD_B" ssh-agent start --key "$STATE/other-client-key" --ttl 3m --yes
! can_read "$YARD_B" || die 'second yard borrowed the first key'
can_read "$YARD_A" || die 'second grant disrupted first yard'
run_yard "$YARD_B" ssh-agent stop --yes
run_yard "$YARD_B" ssh-agent start --key "$STATE/git-client-key" --ttl 3m --yes
can_read "$YARD_B" || die 'second yard could not use its own grant'
run_yard "$YARD_A" security > "$STATE/security.out" 2>&1 || true
grep -q 'Temporary SSH-agent access is granted' "$STATE/security.out" || die 'security omitted granted access'
stage 'checking reconnect after yard stop/start preserves the absolute deadline'
before="$(run_yard "$YARD_A" ssh-agent status --json | jq -er '.expires_at')"
run_yard "$YARD_A" stop --force --yes
run_yard "$YARD_A" start --yes
for _ in $(seq 1 30); do can_read "$YARD_A" && break; sleep 1; done
can_read "$YARD_A" || die 'agent did not reconnect'
after="$(run_yard "$YARD_A" ssh-agent status --json | jq -er '.expires_at')"
[ "$before" = "$after" ] || die 'reconnect extended the deadline'
stage 'checking immediate stop and short TTL with real Git authentications'
run_yard "$YARD_A" ssh-agent stop --yes
run_yard "$YARD_A" ssh-agent stop --yes
! can_read "$YARD_A" || die 'stop left authentication available'
run_yard "$YARD_A" ssh-agent start --key "$STATE/git-client-key" --ttl 5s --yes
can_read "$YARD_A" || die 'short grant never worked'
sleep 6
! can_read "$YARD_A" || die 'expired key still authenticated'
run_yard "$YARD_A" ssh-agent status --json | jq -e '.state == "expired"' >/dev/null
stage 'checking teardown revokes a live grant'
agent_runtime="$(jq -er '.runtime_dir' "$STATE/data/ssh-agent/$YARD_B/session.json")"
agent_unit="${agent_runtime##*/}.service"
run_yard "$YARD_B" teardown --yes
unit_state="$(env XDG_RUNTIME_DIR=/run/user/0 DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/0/bus \
  systemctl --user show "$agent_unit" --property=ActiveState --value)"
case "$unit_state" in inactive|failed) ;; *) die 'teardown left the agent service running';; esac
printf 'ok: owner SSH-agent, existing/new sessions, clone, stop, TTL, reconnect, container/%s isolation and teardown\n' "$SECOND_KIND"
