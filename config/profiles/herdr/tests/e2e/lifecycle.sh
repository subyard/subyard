#!/usr/bin/env bash
# Physical Herdr lifecycle on one installed container yard. Disposable E2E VM only.
set -Eeuo pipefail
umask 022
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." && pwd -P)"
fixture="$root/config/profiles/herdr/tests/e2e"
run_id="${SUBYARD_E2E_RUN_ID:-}"
stage() { printf 'herdr-lifecycle: %s\n' "$*"; }
fail() { stage "$*" >&2; exit 1; }
[[ "$run_id" =~ ^[a-f0-9]+$ ]] && [ "${SUBYARD_E2E_VM:-}" = 1 ] && [ "$(id -un)" = dev ] ||
  fail 'run on VM1 through dev/agent-e2e.sh'
for command in git incus ip jq python3 sg ssh ssh-keygen sudo timeout; do
  command -v "$command" >/dev/null || fail "required VM tool is missing: $command"
done
sudo -n true || fail 'passwordless sudo is required on the disposable VM'
in_session_group() { id -nG | tr ' ' '\n' | grep -Fxq incus-admin; }
in_account_group() { id -nG "$(id -un)" | tr ' ' '\n' | grep -Fxq incus-admin; }
if ! in_session_group && in_account_group; then
  printf -v reexec 'exec bash %q' "$fixture/lifecycle.sh"
  exec sg incus-admin -c "$reexec"
fi
platform="$HOME/.cache/subyard-e2e-platform"
[ "$(cat "$platform/.subyard-e2e-platform-marker" 2>/dev/null)" = subyard-e2e-platform-v1 ] ||
  fail 'the shared E2E platform marker is missing or invalid'

state="$(mktemp -d /var/tmp/subyard-herdr-e2e.XXXXXX)"
name="herdr-e2e-${run_id:0:8}"
project="subyard-$name"
instance="yard-$name"
address=100.64.12.20
trap 'stage "failed at line $LINENO (exit $?)" >&2' ERR
on_exit() {
  local rc=$? log
  trap - EXIT
  if [ "$rc" != 0 ]; then
    for log in "$state"/*.log; do
      [ -s "$log" ] || continue
      stage "${log##*/}:" >&2
      tail -n 40 "$log" >&2
    done
    if [ -n "${project_ready:-}" ]; then
      stage 'guest Herdr diagnostics:' >&2
      incus --project "$project" exec "$instance" -- runuser -u dev -- sh -c '
        cd "$HOME/.local/state/herdr-core/default" 2>/dev/null && tail -n 40 core.log holder.log
        XDG_RUNTIME_DIR=/run/user/$(id -u) systemctl --user list-units --all --no-pager "hc-*"' >&2 || true
    fi
  fi
  stage "evidence: $state" >&2
  exit "$rc"
}
trap on_exit EXIT

incus() {
  if [ -S /var/lib/incus/unix.socket ] && [ ! -w /var/lib/incus/unix.socket ]; then
    sudo -n /usr/bin/incus "$@"
  else
    /usr/bin/incus "$@"
  fi
}
export SUBYARD_OPERATOR_HOME="$HOME" SUBYARD_CONFIG_HOME="$state/config" SUBYARD_HOME="$state/data"
export SUBYARD_NO_AUDIT=1 SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1 MIN_DISK_GIB=1
export STORAGE_PATH="$platform/incus/incus/storage"
install -d -m 0700 "$state/config/yards/$name" "$state/data" "$state/host"

# shellcheck source=tests/helpers/release-candidate.sh
. "$root/tests/helpers/release-candidate.sh"
if [ -e "$root/.subyard-acceptance/candidate.json" ]; then
  dispatcher="$(release_candidate_prepare "$root")"
else
  bash "$root/dev/build-engine.sh" >/dev/null
  dispatcher="$root/.build/yard"
fi

stage 'prepare the owner Incus foundation'
(
  # shellcheck source=tests/helpers/test-context.sh
  . "$root/tests/helpers/test-context.sh"
  setup_test_context "$state/incus-foundation"
  export SUBYARD_USER=dev SUBYARD_CONFIG_DIR="$root/config" SUBYARD_DISPATCHER_PATH="$dispatcher"
  set -a
  # shellcheck source=config/host.env
  . "$root/config/host.env"
  set +a
  bash "$root/scripts/01-install-incus.sh" --yes --zabbly
) > "$state/incus.log" 2>&1
timeout 10 /usr/bin/incus storage show default --project default >/dev/null 2>&1 ||
  timeout 10 sudo -n /usr/bin/incus storage show default --project default >/dev/null
incus project show "$project" >/dev/null 2>&1 && fail 'fixture project already exists'
printf 'STORAGE_PATH=%s\n' "$STORAGE_PATH" > "$state/config/config.env"
chmod 0600 "$state/config/config.env"

# The address, proxy, sshd and Core RPC are real; only discovery of the owner address is synthetic.
ip -o -4 addr show | awk '{print $4}' | grep -Fxq "$address/32" && fail 'fixture address already exists'
[ ! -e /usr/local/bin/tailscale ] && [ ! -L /usr/local/bin/tailscale ] && ! command -v tailscale >/dev/null ||
  fail 'the synthetic tailnet needs a VM without tailscale'
sudo -n ip address add "$address/32" dev lo
cat > "$state/tailscale" <<EOF
#!/bin/sh
case "\$*" in
  'ip -4') printf '%s\n' $address ;;
  'status --json') printf '{"BackendState":"Running","Self":{"TailscaleIPs":["%s"]}}\n' $address ;;
  *) exit 2 ;;
esac
EOF
sudo -n install -o root -g root -m 0755 "$state/tailscale" /usr/local/bin/tailscale
read -r ssh_port port < <(python3 - "$address" <<'PY'
import socket
import sys

with socket.socket() as ssh, socket.socket() as herdr:
    ssh.bind(("127.0.0.1", 0))
    herdr.bind((sys.argv[1], 0))
    print(ssh.getsockname()[1], herdr.getsockname()[1])
PY
)

stage 'install the candidate and initialize a container yard'
export YARD_RUNTIME_ROOT="$state/runtime"
unset YARD_ENGINE_PATH
yard_bin="$(release_candidate_install "$root" "$state")"
export YARD_RELEASE_CACHE="$state/releases" PATH="$state/bin:$PATH"
cat > "$state/config/yards/$name/config.env" <<EOF
SSH_PORT=$ssh_port
YARD_KIND=container
LIMITS_MEMORY=1GiB
LIMITS_CPU=2
HOST_BASE=$state/host
RESTRICTED_DISK_PATHS=$state/host
CODING_TOOL_INTEGRATIONS=
ENVIRONMENT_PROFILES=herdr
FORWARD_SSH_AGENT=0
HOST_CLAUDE_MD=
HOST_CODEX_AGENTS_MD=
HOST_OPENCODE_AGENTS_MD=
HERDR_ADVERTISE_HOST=$address
HERDR_HOST_PORT=$port
EOF
chmod 0600 "$state/config/yards/$name/config.env"
yard() {
  local command
  # Init can add the account to incus-admin after this session started.
  if ! in_session_group && in_account_group; then
    printf -v command '%q ' timeout --foreground 900 "$yard_bin" -Y "$name" "$@"
    sg incus-admin -c "exec $command"
  else
    timeout --foreground 900 "$yard_bin" -Y "$name" "$@"
  fi
}
guest() { incus --project "$project" exec "$instance" -- "$@"; }
guest_dev() { guest runuser -u dev -- "$@"; }
yard init --yes > "$state/init.log" 2>&1
yard start --yes > "$state/start.log" 2>&1
project_ready=1

stage 'verify a fresh up: kick, key-only SSH and ClientAlive convergence'
yard herdr up --yes > "$state/up.log" 2>&1
for project_name in live pristine; do
  mkdir "$state/host/$project_name"
  git -C "$state/host/$project_name" init -q
  printf 'fixture\n' > "$state/host/$project_name/README"
  git -C "$state/host/$project_name" add README
  git -C "$state/host/$project_name" -c user.name=Fixture -c user.email=fixture@example.test \
    commit -qm 'Fixture project'
  yard sync "$state/host/$project_name" --name "$project_name" --yes > "$state/sync-$project_name.log" 2>&1
done
verify_ready() {
  local status attempt
  for ((attempt=0; attempt<30; attempt++)); do
    if yard herdr is-up >/dev/null 2>&1; then break; fi
    sleep 1
  done
  yard herdr is-up
  status="$(yard herdr status)"
  grep -Fxq 'Clean startup marker: yes' <<< "$status"
  grep -Fxq "SSH endpoint: dev@$address:$port" <<< "$status"
  [ "$(guest loginctl show-user dev -p Linger --value)" = yes ]
  guest systemctl is-enabled --quiet subyard-herdr-core.service
  # The oneshot kick finished; Core outlives it in the user manager.
  [ "$(guest systemctl show subyard-herdr-core.service -p ActiveState --value)" = inactive ]
  [ "$(guest systemctl show subyard-herdr-core.service -p Result --value)" = success ]
}
verify_policy() {
  local config
  config="$(guest /usr/sbin/sshd -T)"
  grep -Fxq 'passwordauthentication no' <<< "$config"
  grep -Fxq 'kbdinteractiveauthentication no' <<< "$config"
}
client_alive() { guest sh -eu -s -- check < "$root/scripts/ssh-client-alive.sh"; }
verify_ready
verify_policy
client_alive
yard init --yes > "$state/reinit.log" 2>&1
client_alive
verify_ready

host_key="$(guest cat /etc/ssh/ssh_host_ed25519_key.pub)"
printf '[%s]:%s %s\n' "$address" "$port" "$host_key" > "$state/known-hosts"
printf 'dev:herdr-fixture-password\n' | guest chpasswd
printf '#!/bin/sh\nprintf "herdr-fixture-password\\n"\n' > "$state/askpass"
chmod 0700 "$state/askpass"
ssh_options=(-F /dev/null -T -p "$port" -o ConnectTimeout=5 -o StrictHostKeyChecking=yes
  -o "UserKnownHostsFile=$state/known-hosts" -o IdentitiesOnly=yes)
if SSH_ASKPASS_REQUIRE=force SSH_ASKPASS="$state/askpass" ssh "${ssh_options[@]}" \
  -o PreferredAuthentications=password -o PubkeyAuthentication=no -o NumberOfPasswordPrompts=1 \
  "dev@$address" true </dev/null > "$state/password.log" 2>&1; then
  fail 'password authenticated through the Herdr proxy'
fi
grep -Fq 'Permission denied (publickey)' "$state/password.log"

for client in a b; do
  ssh-keygen -q -t ed25519 -N '' -f "$state/$client"
done
key_login() { ssh "${ssh_options[@]}" -o BatchMode=yes -i "$state/$1" "dev@$address" true </dev/null; }
reject_key() {
  if key_login "$1" > "$state/rejected.log" 2>&1; then fail "unexpected login for client $1"; fi
  grep -Fq 'Permission denied (publickey)' "$state/rejected.log"
}
client() { python3 -I "$fixture/client.py" "$1" "$state" "$address" "$port"; }

stage 'verify client pairing and the A/B/A raw RPC reconnect'
reject_key a
for client in a b; do
  yard herdr pair --key "$(cat "$state/$client.pub")" --name "$client" --yes > "$state/pair-$client.log" 2>&1
done
client reconnect
fingerprint="$(ssh-keygen -E sha256 -lf "$state/b.pub" | awk '{print $2}')"
yard herdr pair --revoke "$fingerprint" --yes > "$state/revoke.log" 2>&1
reject_key b

stage 'verify update, forced pin and restart with a live terminal'
terminal_pid="$(cat "$state/terminal-pid")"
core_pid() { yard herdr status | sed -nE 's/^Core: .*\(PID ([0-9]+), .*/\1/p'; }
before="$(core_pid)"
[ -n "$before" ]
yard herdr update --yes > "$state/update.log" 2>&1
client live
current="$(core_pid)"
yard herdr update --yes > "$state/update-again.log" 2>&1
[ -n "$current" ] && [ "$(core_pid)" = "$current" ] || fail 'a repeated update restarted Core'
client live
yard herdr update --force --yes > "$state/update-force.log" 2>&1
client live
current="$(core_pid)"
yard herdr restart --yes > "$state/restart.log" 2>&1
[ -n "$current" ] && [ "$(core_pid)" != "$current" ] || fail 'restart kept the Core PID'
verify_ready
client live
guest_dev kill -0 "$terminal_pid"
stage "terminal PID $terminal_pid survived the handovers (initial Core PID $before)"
yard herdr logs > "$state/core-journal.log" 2>&1
# The kick records Core's startup status: human text on a terminal, JSON otherwise.
grep -Eq "running, pid $before|\"pid\":${before}[,}]" "$state/core-journal.log" ||
  fail 'logs lack the Core startup status'

stage 'verify project removal and pending layout'
yard remove pristine --yes > "$state/remove-pristine.log" 2>&1
yard remove live --yes > "$state/remove-live.log" 2>&1
client removed
status="$(yard herdr status)"
grep -Fq 'space contains user layout' <<< "$status"

stage 'verify stop/start restores the proxy and starts Core from the kick'
yard stop --yes > "$state/stop.log" 2>&1
yard start --yes > "$state/restart-yard.log" 2>&1
verify_ready
client start
terminal_pid="$(cat "$state/terminal-pid")"
guest_dev kill -0 "$terminal_pid"

stage 'verify down with a live terminal and that yard update leaves Core stopped'
verify_down() {
  local status
  if incus --project "$project" config device get "$instance" herdr-ssh listen >/dev/null 2>&1; then
    fail 'Herdr proxy device is still published'
  fi
  guest test ! -e /etc/ssh/sshd_config.d/00-subyard-herdr.conf
  if guest systemctl is-enabled --quiet subyard-herdr-core.service; then fail 'kick is still enabled'; fi
  status="$(yard herdr status)"
  grep -Fxq 'Core: stopped' <<< "$status"
  if yard herdr is-up >/dev/null 2>&1; then fail 'is-up reports a downed Herdr'; fi
}
yard herdr down --yes > "$state/down.log" 2>&1
verify_down
if guest_dev kill -0 "$terminal_pid" 2>/dev/null; then fail 'terminal survived down'; fi
version="$("$yard_bin" version | awk '{print $2}')"
yard update --version "$version" --offline --yes > "$state/yard-update.log" 2>&1
verify_down
yard herdr up --yes > "$state/up-again.log" 2>&1
verify_ready
verify_policy
client_alive
key_login a
stage 'PASS profile:herdr/lifecycle'
