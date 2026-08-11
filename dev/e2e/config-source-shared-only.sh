#!/usr/bin/env bash
# Real-host acceptance for a shared-only versioned configuration source.
set -euo pipefail

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

case "${SUBYARD_E2E_VM:-}" in
  1 | 2) ;;
  *) fail "run this check on an allocated E2E VM" ;;
esac
if [ "$(id -u)" != 0 ]; then
  exec sudo -n env "SUBYARD_E2E_VM=$SUBYARD_E2E_VM" bash "$0"
fi
for command in git ssh ssh-keygen ssh-keyscan sshd; do
  command -v "$command" >/dev/null 2>&1 || fail "$command is required"
done
sshd_path="$(command -v sshd)"

umask 077
test_root="$(mktemp -d /var/tmp/subyard-config-source-e2e.XXXXXX)"
marker="$test_root/.subyard-config-source-e2e"
touch "$marker"
sshd_pid=""

cleanup() {
  local rc=$?
  trap - EXIT INT TERM
  if [ -n "$sshd_pid" ]; then
    kill "$sshd_pid" >/dev/null 2>&1 || true
    wait "$sshd_pid" >/dev/null 2>&1 || true
  fi
  case "$test_root" in
    /var/tmp/subyard-config-source-e2e.*)
      [ -f "$marker" ] && find "$test_root" -depth -delete
      ;;
  esac
  exit "$rc"
}
trap cleanup EXIT INT TERM

./dev/build-engine.sh --force
yard="$PWD/.build/yard"
[ -x "$yard" ] || fail "candidate yard binary was not built"

operator_home="$test_root/operator"
config_home="$operator_home/.config/subyard"
data_home="$operator_home/.subyard"
checkout="$operator_home/.local/share/subyard-config"
seed="$test_root/seed"
bare="$test_root/source.git"
install -d -m 0700 "$operator_home" "$config_home" "$data_home" "$seed"
install -m 0600 /dev/null "$config_home/config.env"
printf 'local sentinel\n' >"$config_home/local-only.keep"

git init -q --bare "$bare"
git -C "$bare" symbolic-ref HEAD refs/heads/main
git -C "$seed" init -q -b main
printf '%s\n' '{"schemaVersion":1}' >"$seed/subyard-config.json"
install -d -m 0700 "$seed/shared"
printf '%s\n' 'YARD_IMAGE=images:debian/13' >"$seed/shared/config.env"
git -C "$seed" add subyard-config.json shared/config.env
git -C "$seed" \
  -c user.name='Subyard E2E' -c user.email='e2e@invalid' \
  commit -q -m 'Add shared-only source'
git -C "$seed" remote add origin "$bare"
git -C "$seed" push -q -u origin main

host_key="$test_root/ssh-host-key"
client_key="$test_root/ssh-client-key"
authorized_keys="$test_root/authorized_keys"
known_hosts="$test_root/known_hosts"
sshd_config="$test_root/sshd_config"
sshd_log="$test_root/sshd.log"
ssh-keygen -q -t ed25519 -N '' -f "$host_key"
ssh-keygen -q -t ed25519 -N '' -f "$client_key"
cp "$client_key.pub" "$authorized_keys"
port=$((22000 + RANDOM % 10000))
cat >"$sshd_config" <<EOF
Port $port
ListenAddress 127.0.0.1
HostKey $host_key
PidFile $test_root/sshd.pid
AuthorizedKeysFile $authorized_keys
StrictModes no
PubkeyAuthentication yes
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitRootLogin prohibit-password
AllowUsers root
AllowTcpForwarding no
X11Forwarding no
PermitTTY no
UsePAM no
UseDNS no
LogLevel ERROR
EOF
install -d -m 0755 /run/sshd
"$sshd_path" -D -f "$sshd_config" -E "$sshd_log" &
sshd_pid=$!
for _ in $(seq 1 50); do
  if ssh-keyscan -T 1 -p "$port" -t ed25519 127.0.0.1 >"$known_hosts" 2>/dev/null &&
    [ -s "$known_hosts" ]; then
    break
  fi
  kill -0 "$sshd_pid" >/dev/null 2>&1 || {
    sed -n '1,80p' "$sshd_log" >&2
    fail "temporary SSH Git server exited"
  }
  sleep 0.1
done
[ -s "$known_hosts" ] || fail "temporary SSH Git server did not become ready"

export GIT_SSH_COMMAND="ssh -i $client_key -o IdentitiesOnly=yes -o BatchMode=yes -o StrictHostKeyChecking=yes -o UserKnownHostsFile=$known_hosts"
origin="ssh://root@127.0.0.1:$port$bare"
yard_environment=(
  env
  "HOME=$operator_home"
  "SUBYARD_OPERATOR_HOME=$operator_home"
  "SUBYARD_CONFIG_HOME=$config_home"
  "SUBYARD_HOME=$data_home"
  "SUBYARD_REPOSITORY_ROOT=$PWD"
  "SUBYARD_NO_AUDIT=1"
  "GIT_TERMINAL_PROMPT=0"
  "GIT_SSH_COMMAND=$GIT_SSH_COMMAND"
)
run_yard() {
  "${yard_environment[@]}" "$yard" "$@"
}

decline_output="$test_root/decline.out"
if printf 'n\n' | run_yard config source connect "$origin" \
  --host-id e2e-shared-only >"$decline_output" 2>&1; then
  fail "declined source connect succeeded"
fi
grep -Fq 'operation declined' "$decline_output" ||
  fail "declined source connect returned the wrong diagnostic"
[ ! -e "$checkout" ] || fail "declined source connect left its checkout"
[ ! -e "$config_home/host-id" ] || fail "declined source connect saved host identity"
[ ! -e "$config_home/overrides/shared/config.env" ] ||
  fail "declined source connect changed shared settings"
if run_yard config source path >/dev/null 2>&1; then
  fail "declined source connect registered a source"
fi

connect_output="$test_root/connect.out"
printf 'y\n' | run_yard config source connect "$origin" \
  --host-id e2e-shared-only >"$connect_output" 2>&1
[ "$(grep -Fc 'Proceed? [y/N]' "$connect_output")" = 1 ] ||
  fail "source connect did not ask exactly once"
grep -Fq 'overrides/shared/config.env' "$connect_output" ||
  fail "source connect omitted the exact shared managed path"
grep -Fq 'config source: connected' "$connect_output" ||
  fail "source connect did not report registration"
if grep -Fq 'images:debian/13' "$connect_output"; then
  fail "source connect printed a configuration value"
fi
[ "$(cat "$config_home/host-id")" = e2e-shared-only ] ||
  fail "source connect did not persist local HostID"
[ "$(cat "$config_home/overrides/shared/config.env")" = 'YARD_IMAGE=images:debian/13' ] ||
  fail "source connect did not apply shared settings"
[ ! -e "$checkout/hosts" ] || fail "source connect created a host entry in Git"
[ -z "$(git -C "$checkout" status --porcelain=v1 --untracked-files=all)" ] ||
  fail "source connect changed its Git worktree"
[ "$(run_yard config source path)" = "$checkout" ] ||
  fail "registered source path is not authoritative"

show_output="$test_root/show-shared.out"
run_yard config show YARD_IMAGE >"$show_output"
grep -Fq 'effective: images:debian/13' "$show_output" ||
  fail "shared setting is not effective"
grep -Fq "$config_home/overrides/shared/config.env:1" "$show_output" ||
  fail "shared setting provenance is missing"
run_yard config sync --check >"$test_root/check-converged.out"
run_yard config sync >"$test_root/sync-converged.out"
grep -Fq 'already converged' "$test_root/sync-converged.out" ||
  fail "repeated shared-only sync was not idempotent"

initial_checkout_commit="$(git -C "$checkout" rev-parse HEAD)"
install -d -m 0700 "$seed/hosts/e2e-shared-only"
printf '%s\n' 'YARD_IMAGE=images:ubuntu/24.04' \
  >"$seed/hosts/e2e-shared-only/config.env"
git -C "$seed" add hosts/e2e-shared-only/config.env
git -C "$seed" \
  -c user.name='Subyard E2E' -c user.email='e2e@invalid' \
  commit -q -m 'Add selected host overlay'
git -C "$seed" push -q
[ "$(git -C "$checkout" rev-parse HEAD)" = "$initial_checkout_commit" ] ||
  fail "checkout changed before explicit Git transport"
run_yard config sync --check >"$test_root/check-before-pull.out"
[ "$(git -C "$checkout" rev-parse HEAD)" = "$initial_checkout_commit" ] ||
  fail "config sync fetched or pulled Git"

git -C "$checkout" pull -q --ff-only
host_check="$test_root/check-host.out"
if run_yard config sync --check --adopt >"$host_check" 2>&1; then
  fail "host overlay --check reported convergence"
fi
grep -Fq 'changes required' "$host_check" ||
  fail "host overlay --check omitted its non-converged result"
grep -Fq 'config.env' "$host_check" ||
  fail "host overlay --check omitted its exact managed path"
printf 'y\n' | run_yard config sync --adopt >"$test_root/sync-host.out" 2>&1
run_yard config show YARD_IMAGE >"$test_root/show-host.out"
grep -Fq 'effective: images:ubuntu/24.04' "$test_root/show-host.out" ||
  fail "selected host overlay did not override shared settings"
grep -Fq "$config_home/config.env:1" "$test_root/show-host.out" ||
  fail "host setting provenance is missing"
[ -z "$(git -C "$checkout" status --porcelain=v1 --untracked-files=all)" ] ||
  fail "host overlay sync changed its Git worktree"

rm -rf -- "$seed/hosts/e2e-shared-only"
git -C "$seed" add -A
git -C "$seed" \
  -c user.name='Subyard E2E' -c user.email='e2e@invalid' \
  commit -q -m 'Remove selected host overlay'
git -C "$seed" push -q
host_checkout_commit="$(git -C "$checkout" rev-parse HEAD)"
run_yard config sync --check >"$test_root/check-remove-before-pull.out"
[ "$(git -C "$checkout" rev-parse HEAD)" = "$host_checkout_commit" ] ||
  fail "config sync pulled host overlay removal"

git -C "$checkout" pull -q --ff-only
remove_check="$test_root/check-remove.out"
if run_yard config sync --check >"$remove_check" 2>&1; then
  fail "removed host overlay --check reported convergence"
fi
grep -Fq 'delete' "$remove_check" ||
  fail "removed host overlay did not produce an explicit delete plan"
grep -Fq 'config.env' "$remove_check" ||
  fail "removed host overlay delete plan omitted its path"
printf 'y\n' | run_yard config sync >"$test_root/sync-remove.out" 2>&1
[ ! -e "$config_home/config.env" ] ||
  fail "previously managed host settings survived source removal"
[ "$(cat "$config_home/local-only.keep")" = 'local sentinel' ] ||
  fail "sync removed unmanaged local data"
run_yard config show YARD_IMAGE >"$test_root/show-restored-shared.out"
grep -Fq 'effective: images:debian/13' "$test_root/show-restored-shared.out" ||
  fail "host overlay removal did not restore shared precedence"
run_yard config sync --check >"$test_root/check-final.out"
[ -z "$(git -C "$checkout" status --porcelain=v1 --untracked-files=all)" ] ||
  fail "final config sync changed its Git worktree"
[ ! -e "$checkout/hosts/e2e-shared-only" ] ||
  fail "Yard recreated the removed host entry"

printf 'ok: shared-only Git source connects, converges and safely adds/removes host overlay\n'
