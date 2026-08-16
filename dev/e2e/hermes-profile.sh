#!/usr/bin/env bash
# Real Hermes substrate, persistence, isolation and owner-route acceptance.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
STATE=""
YARD=""
TAILSCALE_IP=""

die() { printf 'hermes-profile-e2e: %s\n' "$*" >&2; exit 2; }
[ -n "${SUBYARD_E2E_VM:-}" ] || die "run through dev/agent-e2e.sh"
for command in curl ip python3 sg sudo tar timeout; do
  command -v "$command" >/dev/null 2>&1 || die "$command is required"
done
if [ ! -x "$ROOT/.build/yard" ]; then
  command -v go >/dev/null 2>&1 || die "Go is required in the leased VM"
  "$ROOT/dev/build-engine.sh"
fi

yard() {
  local name="$1"
  shift
  "$ROOT/.build/yard" -Y "$name" "$@"
}

setting() {
  local name="$1" key="$2" output
  output="$(yard "$name" config show "$key")"
  printf '%s\n' "$output" | sed -n 's/^effective: //p'
}

reexec_with_incus_group() {
  local command
  printf -v command 'exec env SUBYARD_E2E_VM=%q bash %q' \
    "$SUBYARD_E2E_VM" "$ROOT/dev/e2e/hermes-profile.sh"
  exec sg incus-admin -c "$command"
}

ensure_owner_incus() {
  local platform_root="$HOME/.cache/subyard-hermes-profile-platform"
  if command -v incus >/dev/null 2>&1 \
    && ! id -nG | tr ' ' '\n' | grep -qx incus-admin \
    && id -nG "$(id -un)" | tr ' ' '\n' | grep -qx incus-admin; then
    reexec_with_incus_group
  fi
  if command -v incus >/dev/null 2>&1 && incus info >/dev/null 2>&1; then
    return
  fi
  printf '  [ .. ] preparing the leased VM owner Incus API\n'
  install -d -m 0700 "$platform_root"
  (
    # shellcheck source=tests/helpers/test-context.sh
    . "$ROOT/tests/helpers/test-context.sh"
    setup_test_context "$platform_root/bootstrap"
    export SUBYARD_USER
    SUBYARD_USER="$(id -un)"
    export SUBYARD_OPERATOR_HOME="$HOME"
    export SUBYARD_CONFIG_DIR="$ROOT/config"
    export SUBYARD_CONFIG_HOME="$platform_root/config"
    export SUBYARD_HOME="$platform_root/data"
    export STORAGE_PATH="$platform_root/storage"
    export HOST_BASE="$platform_root/host-data"
    export RESTRICTED_DISK_PATHS="$HOST_BASE"
    set -a
    # shellcheck source=config/host.env
    . "$ROOT/config/host.env"
    set +a
    bash "$ROOT/scripts/01-install-incus.sh" --yes --zabbly
  )
  if ! id -nG | tr ' ' '\n' | grep -qx incus-admin \
    && id -nG "$(id -un)" | tr ' ' '\n' | grep -qx incus-admin; then
    reexec_with_incus_group
  fi
  command -v incus >/dev/null 2>&1 && incus info >/dev/null 2>&1 \
    || die "leased VM owner Incus API is unavailable after preparation"
  printf '  [ ok ] leased VM owner Incus API is ready\n'
}

next_port() {
  local candidate="$1"
  while ss -H -ltn "sport = :$candidate" 2>/dev/null | grep -q .; do
    candidate=$((candidate + 1))
  done
  printf '%s\n' "$candidate"
}

recover_stale_fixture_pool() {
  local state source status used_by fingerprint
  state="$(timeout --foreground 30 incus storage show default --project default 2>/dev/null)" \
    || return 0
  source="$(printf '%s\n' "$state" | sed -n 's/^  source: //p')"
  case "$source" in
    /tmp/subyard-hermes-profile.*/storage|/var/tmp/subyard-hermes-profile.*/storage) ;;
    *) return 0 ;;
  esac
  status="$(printf '%s\n' "$state" | sed -n 's/^status: //p')"
  [ "$status" = Unavailable ] || return 0
  used_by="$(printf '%s\n' "$state" | sed -n '/^used_by:/,/^status:/p' | sed '$d')"
  case "$(printf '%s' "$used_by" | tr -d '[:space:]')" in
    'used_by:[]') ;;
    'used_by:-/1.0/profiles/default')
      incus profile device remove default root --project default >/dev/null
      ;;
    *) die "refusing to recover stale Hermes test pool with consumers: $used_by" ;;
  esac
  while IFS= read -r fingerprint; do
    [ -n "$fingerprint" ] || continue
    incus image delete "$fingerprint" --project default >/dev/null
  done < <(incus image list --project default --format csv -c f)
  timeout --foreground 120 incus storage delete default --project default >/dev/null
}

cleanup_fixture_pool() {
  local state source fingerprint
  [ -n "$STATE" ] || return 0
  [ -f "$STATE/.marker" ] \
    && [ "$(<"$STATE/.marker")" = subyard-hermes-profile-e2e-v3 ] || return 1
  state="$(incus storage show default --project default 2>/dev/null)" || return 0
  source="$(printf '%s\n' "$state" | sed -n 's/^  source: //p')"
  [ "$source" = "$STORAGE_PATH" ] || return 0
  case "$source" in
    /tmp/subyard-hermes-profile.*/storage|/var/tmp/subyard-hermes-profile.*/storage) ;;
    *) return 1 ;;
  esac
  [ -z "$(incus list --all-projects --format csv -c n)" ] || return 1
  if incus profile device list default --project default 2>/dev/null | grep -qx root; then
    incus profile device remove default root --project default >/dev/null || return 1
  fi
  while IFS= read -r fingerprint; do
    [ -n "$fingerprint" ] || continue
    incus image delete "$fingerprint" --project default >/dev/null || return 1
  done < <(incus image list --project default --format csv -c f)
  incus storage delete default --project default >/dev/null
}

cleanup() {
  local rc=$?
  trap - EXIT INT TERM
  set +e
  if [ -n "$YARD" ] && [ -f "$SUBYARD_CONFIG_HOME/yards/$YARD/config.env" ]; then
    yard "$YARD" teardown --yes >/dev/null 2>&1 || rc=3
  fi
  if [ -n "$TAILSCALE_IP" ]; then
    sudo -n ip address del "$TAILSCALE_IP/32" dev lo >/dev/null 2>&1 || true
  fi
  cleanup_fixture_pool || rc=3
  if [ -n "$STATE" ] && [[ "$STATE" = /var/tmp/subyard-hermes-profile.* ]] \
    && [ -f "$STATE/.marker" ] \
    && [ "$(<"$STATE/.marker")" = subyard-hermes-profile-e2e-v3 ]; then
    sudo -n find "$STATE" -depth -delete || rc=3
  fi
  exit "$rc"
}
trap cleanup EXIT INT TERM

ensure_owner_incus
recover_stale_fixture_pool
STATE="$(mktemp -d /var/tmp/subyard-hermes-profile.XXXXXX)"
printf '%s\n' subyard-hermes-profile-e2e-v3 >"$STATE/.marker"
token="$(printf '%s' "${STATE##*.}" | tr '[:upper:]' '[:lower:]')"
YARD="hermes-e2e-$token"

export SUBYARD_OPERATOR_HOME="$HOME"
export SUBYARD_CONFIG_HOME="$STATE/config"
export SUBYARD_HOME="$STATE/data"
export STORAGE_PATH="$STATE/storage"
export SUBYARD_NO_AUDIT=1
export SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1
export MIN_DISK_GIB=1
install -d -m 0700 "$SUBYARD_CONFIG_HOME"
{
  printf '%s\n' \
    'ENVIRONMENT_PROFILES=openclaw' \
    'CODING_TOOL_INTEGRATIONS=claude' \
    'YARD_CAPABILITIES=android' \
    'YARD_CAPS=fuse' \
    'YARD_DEVICES=gpu' \
    'FORWARD_SSH_AGENT=1' \
    'DEV_SUDO=1' \
    'NESTED_E2E_VMS=1'
  printf 'HOST_MOUNTS=host-fixture:/mnt/host-fixture:ro:0755\n'
  printf 'HOST_LINKS=.claude/sessions:/mnt/host/agent-sessions/claude/sessions\n'
} >"$SUBYARD_CONFIG_HOME/config.env"
chmod 0600 "$SUBYARD_CONFIG_HOME/config.env"

ssh_port="$(next_port "$((32000 + ($$ % 10000)))")"
dashboard_port="$(next_port "$((42000 + ($$ % 10000)))")"
replacement_dashboard_port="$(next_port "$((dashboard_port + 1))")"
[ "$replacement_dashboard_port" != "$dashboard_port" ] \
  || die "dashboard replacement port allocation collided"

printf '  [ .. ] creating a fresh isolated yard from the Hermes preset\n'
yard "$YARD" init --profile hermes --yes
definition="$SUBYARD_CONFIG_HOME/yards/$YARD/config.env"
cmp "$ROOT/config/profiles/hermes/yard.env" "$definition" \
  || die "profile bootstrap did not persist the shipped preset"
[ "$(stat -c %a "$definition")" = 600 ] || die "yard definition is not mode 0600"
yard "$YARD" config set SSH_PORT "$ssh_port" --scope yard --yes
yard "$YARD" start --yes
yard "$YARD" init --yes

printf '  [ .. ] delegating installation to the latest stable official installer\n'
yard "$YARD" provision --yes
yard "$YARD" start --yes
instance="$(setting "$YARD" YARD_INSTANCE_NAME)"
project="$(setting "$YARD" INCUS_PROJECT)"
dev_uid="$(incus exec "$instance" --project "$project" -- id -u dev)" \
  || die "could not resolve the guest dev uid"
dev_gid="$(incus exec "$instance" --project "$project" -- id -g dev)" \
  || die "could not resolve the guest dev primary gid"
[[ "$dev_uid" =~ ^[0-9]+$ && "$dev_gid" =~ ^[0-9]+$ ]] \
  || die "guest dev identity is invalid"

incus exec "$instance" --project "$project" --user "$dev_uid" --group "$dev_gid" \
  --env HOME=/home/dev -- sh -euc '
fail() { printf "Hermes substrate assertion failed: %s\n" "$*" >&2; exit 1; }
state=$HOME/.hermes
source=$state/hermes-agent
launcher=$HOME/.local/bin/hermes
test -d "$state" && test ! -L "$state" || fail "canonical state directory"
test "$(stat -c %a "$state")" = 700 || fail "canonical state mode"
test "$(stat -c %u:%g "$state")" = "$(id -u dev):$(id -g dev)" \
  || fail "canonical state owner"
test -d "$source/.git" && test ! -L "$source" || fail "official source checkout"
test -x "$source/venv/bin/python" || fail "source-local virtual environment"
test -f "$source/hermes" || fail "official source entrypoint"
test "$(cat "$source/.install_method")" = git || fail "official git install marker"
case "$(git -C "$source" remote get-url origin)" in
  https://github.com/NousResearch/hermes-agent.git|git@github.com:NousResearch/hermes-agent.git) ;;
  *) fail "official source origin" ;;
esac
release_tag="$(git -C "$source" describe --tags --exact-match HEAD)" \
  || fail "checkout HEAD is not an exact release tag"
printf "%s\n" "$release_tag" \
  | grep -Eq "^v[0-9]{4}\.[0-9]{1,2}\.[0-9]{1,2}(\.[0-9]+)?$" \
  || fail "checkout tag is not a stable release tag"
test "$(git -C "$source" rev-list -n 1 "$release_tag^{commit}")" \
  = "$(git -C "$source" rev-parse HEAD)" \
  || fail "stable release tag does not resolve to checkout HEAD"
test -x "$launcher" || fail "canonical launcher"
! command -v tailscale >/dev/null 2>&1 || fail "Tailscale leaked into the guest"
test ! -S /run/host-services/ssh-auth.sock || fail "host SSH agent leaked into the guest"
if command -v sudo >/dev/null 2>&1 && sudo -n true; then
  fail "passwordless sudo enabled"
fi
if systemctl list-unit-files --no-legend 2>/dev/null | awk "{print \$1}" | grep -Eq "^subyard-hermes"; then
  fail "Subyard-owned Hermes service installed"
fi
'
printf '  [ ok ] canonical upstream layout and guest isolation verified\n'

expanded="$(incus config show "$instance" --project "$project" --expanded)"
printf '%s\n' "$expanded" | grep -Fq 'security.privileged: "true"' \
  && die "Hermes yard is privileged"
printf '%s\n' "$expanded" | grep -Fq 'security.nesting: "true"' \
  || die "Hermes yard is missing core container nesting"
printf '%s\n' "$expanded" | grep -Fq 'source: host-fixture' \
  && die "host-level mount leaked into the Hermes yard"
printf '%s\n' "$expanded" | grep -Fq '/mnt/host/agent-sessions' \
  && die "host agent state leaked into the Hermes yard"
[ "$(setting "$YARD" NESTED_E2E_VMS)" = 0 ] \
  || die "Hermes yard inherited the nested-VM backend"
project_state="$(incus project show "$project")"
printf '%s\n' "$project_state" | grep -Fq 'restricted.containers.interception: block' \
  || die "Hermes project allows syscall interception"
local_config="$(incus config show "$instance" --project "$project")"
if printf '%s\n' "$local_config" | grep -Fq 'security.syscalls.intercept.bpf'; then
  die "Hermes yard retained device-cgroup BPF interception"
fi
local_devices="$(incus config device list "$instance" --project "$project")"
for nested_device in e2e-vsock e2e-vhost-vsock e2e-tun; do
  printf '%s\n' "$local_devices" | grep -Fxq "$nested_device" \
    && die "Hermes yard retained nested-VM device $nested_device"
done
yard "$YARD" security --require-live --quiet \
  || die "live isolation policy rejected the Hermes yard"

printf '  [ .. ] creating opaque state at the canonical upstream path\n'
incus exec "$instance" --project "$project" --user "$dev_uid" --group "$dev_gid" \
  --env HOME=/home/dev --env OPAQUE_TOKEN="$token" -- sh -euc '
install -d -m 0700 "$HOME/.hermes/operator-opaque"
printf "opaque-substrate-e2e-%s\n" "$OPAQUE_TOKEN" >"$HOME/.hermes/operator-opaque/state.bin"
chmod 0600 "$HOME/.hermes/operator-opaque/state.bin"
'

state_signature() {
  timeout --foreground 300 incus exec "$instance" --project "$project" \
    --user "$dev_uid" --group "$dev_gid" \
    --env HOME=/home/dev -- sh -euc '
file=$HOME/.hermes/operator-opaque/state.bin
test -f "$file" && test ! -L "$file"
test -x "$HOME/.hermes/hermes-agent/venv/bin/python"
test -x "$HOME/.local/bin/hermes"
cd "$HOME"
LC_ALL=C tar --sort=name --numeric-owner --one-file-system --format=gnu \
  -cf - .hermes .local/bin/hermes
' | sha256sum
}

before="$(state_signature)"
printf '  [ .. ] proving repeat provision preserves installation and opaque state\n'
yard "$YARD" provision --yes
[ "$(state_signature)" = "$before" ] \
  || die "repeat provision changed the installation or opaque state"

printf '  [ .. ] proving stop/start and container restart preserve opaque state\n'
yard "$YARD" stop --yes
yard "$YARD" start --yes
[ "$(state_signature)" = "$before" ] || die "stop/start changed opaque state"
incus restart "$instance" --project "$project" --timeout 120
[ "$(state_signature)" = "$before" ] || die "container restart changed opaque state"
yard "$YARD" security --require-live --quiet

printf '  [ .. ] validating the typed owner route with an application-neutral fixture\n'
incus exec "$instance" --project "$project" --user "$dev_uid" --group "$dev_gid" \
  --env HOME=/home/dev -- sh -euc '
nohup python3 -m http.server 9119 --bind 127.0.0.1 \
  >"$HOME/.hermes/operator-opaque/http-fixture.log" 2>&1 &
printf "%s\n" "$!" >"$HOME/.hermes/operator-opaque/http-fixture.pid"
'
for _ in $(seq 1 30); do
  incus exec "$instance" --project "$project" -- \
    bash -c 'exec 3<>/dev/tcp/127.0.0.1/9119' >/dev/null 2>&1 && break
  sleep 1
done
incus exec "$instance" --project "$project" -- \
  bash -c 'exec 3<>/dev/tcp/127.0.0.1/9119' >/dev/null 2>&1 \
  || die "controlled guest loopback fixture did not start"

guest_ip="$(incus list "$instance" --project "$project" -f csv -c 4 \
  | tr ',' '\n' | awk '/^[0-9]+\./ {print; exit}')"
[ -n "$guest_ip" ] || die "Hermes yard has no L1 IPv4"
if curl --noproxy '*' -fsS --connect-timeout 2 "http://$guest_ip:9119/" >/dev/null 2>&1; then
  die "guest loopback fixture is reachable through the L1 address"
fi

octet=$((20 + ($$ % 200)))
TAILSCALE_IP="100.127.254.$octet"
if ip -4 address show | grep -Fq "$TAILSCALE_IP/"; then
  die "selected test Tailscale address already exists"
fi
sudo -n ip address add "$TAILSCALE_IP/32" dev lo
install -d -m 0700 "$STATE/fakebin"
{
  printf '#!/usr/bin/env sh\n'
  printf '[ "$1" = ip ] && [ "$2" = -4 ] || exit 2\n'
  printf 'printf "%%s\\n" %s\n' "$TAILSCALE_IP"
} >"$STATE/fakebin/tailscale"
chmod 0700 "$STATE/fakebin/tailscale"

yard "$YARD" config set HERMES_DASHBOARD_ADVERTISE_HOST "$TAILSCALE_IP" --scope yard --yes
yard "$YARD" config set HERMES_DASHBOARD_HOST_PORT "$dashboard_port" --scope yard --yes

cat >"$STATE/fakebin/timeout" <<'TIMEOUT'
#!/usr/bin/env sh
exit 1
TIMEOUT
cat >"$STATE/fakebin/sleep" <<'SLEEP'
#!/usr/bin/env sh
exit 0
SLEEP
chmod 0700 "$STATE/fakebin/timeout" "$STATE/fakebin/sleep"
if PATH="$STATE/fakebin:$PATH" yard "$YARD" dashboard up --yes \
  >"$STATE/readiness-failure.out" 2>&1; then
  die "injected owner-endpoint readiness failure was accepted"
fi
if incus config device list "$instance" --project "$project" | grep -qx hermes-dashboard; then
  die "readiness failure left the owned browser proxy attached"
fi
if [ -n "$(incus config get "$instance" user.subyard.resource.hermes-dashboard \
  --project "$project")" ]; then
  die "readiness failure left owner-side route metadata"
fi
rm -f -- "$STATE/fakebin/timeout" "$STATE/fakebin/sleep"

PATH="$STATE/fakebin:$PATH" yard "$YARD" dashboard up --yes

device_state="$(incus config device show "$instance" --project "$project")"
printf '%s\n' "$device_state" | grep -Fq "listen: tcp:$TAILSCALE_IP:$dashboard_port" \
  || die "browser proxy is not bound to the selected owner address"
printf '%s\n' "$device_state" | grep -Fq 'connect: tcp:127.0.0.1:9119' \
  || die "browser proxy does not target guest loopback"
printf '%s\n' "$device_state" | grep -Fq 'bind: host' \
  || die "browser proxy is not host-bound"
curl --noproxy '*' -fsS --max-time 5 "http://$TAILSCALE_IP:$dashboard_port/" \
  | grep -Fq 'Directory listing'
if curl --noproxy '*' -fsS --connect-timeout 2 "http://$guest_ip:9119/" >/dev/null 2>&1; then
  die "typed owner route made the fixture directly reachable over L1"
fi
PATH="$STATE/fakebin:$PATH" yard "$YARD" security --require-live --quiet

printf '  [ .. ] proving route ownership survives mutable setting changes\n'
yard "$YARD" config set HERMES_DASHBOARD_HOST_PORT "$replacement_dashboard_port" --scope yard --yes
PATH="$STATE/fakebin:$PATH" yard "$YARD" dashboard up --yes
device_state="$(incus config device show "$instance" --project "$project")"
printf '%s\n' "$device_state" \
  | grep -Fq "listen: tcp:$TAILSCALE_IP:$replacement_dashboard_port" \
  || die "owned browser proxy was not replaced after its port setting changed"
curl --noproxy '*' -fsS --max-time 5 \
  "http://$TAILSCALE_IP:$replacement_dashboard_port/" | grep -Fq 'Directory listing'
if curl --noproxy '*' -fsS --connect-timeout 2 \
  "http://$TAILSCALE_IP:$dashboard_port/" >/dev/null 2>&1; then
  die "old owner endpoint survived route replacement"
fi
PATH="$STATE/fakebin:$PATH" yard "$YARD" security --require-live --quiet

yard "$YARD" config unset HERMES_DASHBOARD_ADVERTISE_HOST --scope yard --yes
yard "$YARD" config unset HERMES_DASHBOARD_HOST_PORT --scope yard --yes
PATH="$STATE/fakebin:$PATH" yard "$YARD" dashboard down --yes
if incus config device list "$instance" --project "$project" | grep -qx hermes-dashboard; then
  die "dashboard down left the owned proxy device"
fi
if [ -n "$(incus config get "$instance" user.subyard.resource.hermes-dashboard \
  --project "$project")" ]; then
  die "dashboard down left owner-side route metadata"
fi
if curl --noproxy '*' -fsS --connect-timeout 2 \
  "http://$TAILSCALE_IP:$replacement_dashboard_port/" >/dev/null 2>&1; then
  die "owner endpoint survived route withdrawal"
fi

printf '  [ .. ] tearing down only the marked disposable yard\n'
definition_before_teardown="$(sha256sum "$definition")"
state_dir="$SUBYARD_CONFIG_HOME/yards/$YARD/projects"
[ -d "$state_dir" ] && [ ! -L "$state_dir" ] \
  || die "Hermes project state is missing before teardown"
yard "$YARD" teardown --yes
[ -f "$definition" ] && [ ! -L "$definition" ] \
  || die "reusable yard definition disappeared during teardown"
[ "$(stat -c %a "$definition")" = 600 ] \
  || die "yard definition mode changed during teardown"
[ "$(sha256sum "$definition")" = "$definition_before_teardown" ] \
  || die "yard definition changed during teardown"
if incus project show "$project" >/dev/null 2>&1; then
  die "Hermes Incus project survived teardown"
fi
[ ! -e "$state_dir" ] && [ ! -L "$state_dir" ] \
  || die "Hermes project state survived teardown"
YARD=""
printf '  [ ok ] official layout, isolation, persistence and typed Tailscale route verified\n'
