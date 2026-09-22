#!/usr/bin/env bash
# Real-host regression for Incus AppArmor capability probe failures and mask convergence.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
STATE=''
YARD_NAME=''
PROJECT=''
INSTANCE=''
MARKER=''
APPARMOR_DROPIN=''
DROPIN_ACTIVE=0
HOST_RUNTIME_CHANGED=0
SYSTEMCTL_BIN=''

die() { printf 'incus-apparmor-probe-e2e: %s\n' "$*" >&2; exit 2; }
info() { printf '  [ .. ] %s\n' "$*"; }
ok() { printf '  [ ok ] %s\n' "$*"; }

[ "${SUBYARD_E2E_VM:-}" = 1 ] \
  || die 'run on VM1 through dev/agent-e2e.sh'
for command in awk go sg sha256sum ss sudo systemctl timeout; do
  command -v "$command" >/dev/null 2>&1 || die "$command is required"
done
sudo -n true || die 'passwordless sudo is required on the disposable VM'
SYSTEMCTL_BIN="$(command -v systemctl)"

incus() {
  if [ -S /var/lib/incus/unix.socket ] && [ ! -w /var/lib/incus/unix.socket ]; then
    sudo -n /usr/bin/incus "$@"
  else
    /usr/bin/incus "$@"
  fi
}
yard() { "$ROOT/.build/yard" -Y "$YARD_NAME" "$@"; }
guest() { incus exec "$INSTANCE" --project "$PROJECT" -- "$@"; }

incus_apparmor_is_disabled() {
  local main_pid rc
  main_pid="$("$SYSTEMCTL_BIN" show incus.service -p MainPID --value 2>/dev/null)" \
    || return 2
  [[ "$main_pid" =~ ^[1-9][0-9]*$ ]] || return 2
  if sudo -n grep -zqx 'INCUS_SECURITY_APPARMOR=false' \
    "/proc/$main_pid/environ" >/dev/null 2>&1; then
    return 0
  else
    rc=$?
  fi
  [ "$rc" -eq 1 ] && return 1
  return 2
}

wait_incus_ready() {
  if [ -S /var/lib/incus/unix.socket ] && [ ! -w /var/lib/incus/unix.socket ]; then
    timeout --foreground 120 sudo -n /usr/bin/incus admin waitready >/dev/null
  else
    timeout --foreground 120 /usr/bin/incus admin waitready >/dev/null
  fi
}

reexec_with_incus_group() {
  local command
  [ -n "$STATE" ] && [[ "$STATE" = /var/tmp/subyard-apparmor-probe.* ]] \
    && [ -f "$STATE/.marker" ] && [ "$(<"$STATE/.marker")" = "$MARKER" ] \
    || die 'refusing to re-exec with ambiguous temporary state'
  sudo -n find "$STATE" -depth -delete \
    || die 'could not remove pre-project state before incus-admin re-exec'
  trap - EXIT INT TERM
  printf -v command 'exec env SUBYARD_E2E_VM=%q bash %q' \
    "$SUBYARD_E2E_VM" "$ROOT/dev/e2e/incus-apparmor-probe.sh"
  exec sg incus-admin -c "$command"
}

ensure_incus_platform() {
  local platform_root="$HOME/.cache/subyard-e2e-platform" marker temporary
  marker="$platform_root/.subyard-e2e-platform-marker"
  if ! command -v incus >/dev/null 2>&1 \
    || ! incus info >/dev/null 2>&1 \
    || ! incus storage show default --project default >/dev/null 2>&1 \
    || ! incus network show incusbr0 --project default >/dev/null 2>&1; then
    info 'reconciling the real Incus platform with the product installer'
    (
      # shellcheck source=tests/helpers/test-context.sh
      . "$ROOT/tests/helpers/test-context.sh"
      setup_test_context "$STATE/platform-bootstrap"
      export SUBYARD_USER
      SUBYARD_USER="$(id -un)"
      export SUBYARD_OPERATOR_HOME="$HOME"
      export SUBYARD_CONFIG_DIR="$ROOT/config"
      export SUBYARD_CONFIG_HOME="$STATE/platform-bootstrap-config"
      export SUBYARD_HOME="$platform_root"
      export STORAGE_PATH="$platform_root/incus/incus/storage"
      export HOST_BASE="$STATE/platform-host-data"
      export RESTRICTED_DISK_PATHS="$HOST_BASE"
      set -a
      # shellcheck source=config/host.env
      . "$ROOT/config/host.env"
      set +a
      bash "$ROOT/scripts/01-install-incus.sh" --yes --zabbly
    )
  fi
  command -v incus >/dev/null 2>&1 || die 'product installer did not install Incus'
  incus info >/dev/null 2>&1 || die 'Incus owner API is unavailable after reconciliation'
  incus storage show default --project default >/dev/null 2>&1 \
    || die 'Incus default storage pool is unavailable after reconciliation'
  incus network show incusbr0 --project default >/dev/null 2>&1 \
    || die 'Incus managed bridge is unavailable after reconciliation'
  if [ -e "$marker" ]; then
    [ "$(<"$marker")" = subyard-e2e-platform-v1 ] \
      || die "unexpected shared E2E platform marker: $marker"
  else
    install -d -m 0711 "$platform_root"
    temporary="$(mktemp "$platform_root/.platform-marker.XXXXXX")"
    printf '%s\n' subyard-e2e-platform-v1 > "$temporary"
    chmod 0600 "$temporary"
    mv -f -- "$temporary" "$marker"
  fi
}

remove_test_dropin() {
  local current=''
  [ "$HOST_RUNTIME_CHANGED" = 1 ] || return 0
  if [ "$DROPIN_ACTIVE" = 1 ]; then
    if ! current="$(sudo -n cat "$APPARMOR_DROPIN" 2>/dev/null)"; then
      printf 'incus-apparmor-probe-e2e: test-owned AppArmor drop-in disappeared\n' >&2
      return 1
    fi
    [ "$current" = "# $MARKER
[Service]
Environment=INCUS_SECURITY_APPARMOR=false" ] || {
      printf 'incus-apparmor-probe-e2e: refusing changed drop-in %s\n' \
        "$APPARMOR_DROPIN" >&2
      return 1
    }
    sudo -n find "$APPARMOR_DROPIN" -delete || return 1
    DROPIN_ACTIVE=0
  fi
  sudo -n "$SYSTEMCTL_BIN" daemon-reload || return 1
  timeout --foreground 120 sudo -n "$SYSTEMCTL_BIN" restart incus.service || return 1
  wait_incus_ready || return 1
  HOST_RUNTIME_CHANGED=0
}

cleanup() {
  local rc=$? instance_marker='' project_marker=''
  trap - EXIT INT TERM
  set +e
  if ! remove_test_dropin; then
    rc=3
  fi
  if [ -n "$PROJECT" ] && incus project show "$PROJECT" >/dev/null 2>&1; then
    project_marker="$(incus project get "$PROJECT" user.subyard.e2e 2>/dev/null)"
    if [ "$project_marker" = "$MARKER" ]; then
      instance_marker="$(incus config get "$INSTANCE" user.subyard.e2e \
        --project "$PROJECT" 2>/dev/null)"
      if [ "$instance_marker" = "$MARKER" ]; then
        yard teardown --yes >/dev/null 2>&1 || rc=3
      else
        printf 'incus-apparmor-probe-e2e: refusing to remove unmarked instance %s/%s\n' \
          "$PROJECT" "$INSTANCE" >&2
        rc=3
      fi
    elif [ -n "$project_marker" ]; then
      printf 'incus-apparmor-probe-e2e: refusing to remove unmarked project %s\n' \
        "$PROJECT" >&2
      rc=3
    elif [ -n "$YARD_NAME" ] \
      && [ -f "${SUBYARD_CONFIG_HOME:-}/yards/$YARD_NAME/config.env" ] \
      && grep -Fqx "# $MARKER" \
        "${SUBYARD_CONFIG_HOME:-}/yards/$YARD_NAME/config.env"; then
      # A failed initial init can create the uniquely named managed project before the test can
      # add its second ownership marker. The marker-owned yard config remains the teardown fence.
      yard teardown --yes >/dev/null 2>&1 || rc=3
    else
      printf 'incus-apparmor-probe-e2e: refusing to remove ambiguous project %s\n' \
        "$PROJECT" >&2
      rc=3
    fi
  fi
  if [ -n "$STATE" ] && [[ "$STATE" = /var/tmp/subyard-apparmor-probe.* ]] \
    && [ -f "$STATE/.marker" ] && [ "$(<"$STATE/.marker")" = "$MARKER" ]; then
    sudo -n find "$STATE" -depth -delete || rc=3
  fi
  exit "$rc"
}
trap cleanup EXIT INT TERM

STATE="$(mktemp -d /var/tmp/subyard-apparmor-probe.XXXXXX)"
token="$(printf '%s' "${STATE##*.}" | tr '[:upper:]' '[:lower:]')"
MARKER="subyard-apparmor-probe-e2e-v1-$token"
printf '%s\n' "$MARKER" > "$STATE/.marker"
YARD_NAME="apparmor-probe-$token"
PROJECT="subyard-$YARD_NAME"
INSTANCE="yard-$YARD_NAME"
APPARMOR_DROPIN="/etc/systemd/system/incus.service.d/90-$MARKER.conf"

ensure_incus_platform
if ! id -nG | tr ' ' '\n' | grep -Fxq incus-admin \
  && id -nG "$(id -un)" | tr ' ' '\n' | grep -Fxq incus-admin; then
  reexec_with_incus_group
fi
[ ! -e "$APPARMOR_DROPIN" ] \
  || die "test-owned drop-in path already exists: $APPARMOR_DROPIN"
if incus_apparmor_is_disabled; then
  die 'Incus already has effective INCUS_SECURITY_APPARMOR=false; restore the retained VM baseline first'
else
  probe_rc=$?
  [ "$probe_rc" -eq 1 ] || die 'could not read the effective Incus service environment'
fi

export SUBYARD_OPERATOR_HOME="$HOME"
export SUBYARD_CONFIG_HOME="$STATE/config"
export SUBYARD_HOME="$STATE/data"
export STORAGE_PATH="$HOME/.cache/subyard-e2e-platform/incus/incus/storage"
export SUBYARD_NO_AUDIT=1
export SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1
export MIN_DISK_GIB=1
export HOST_CLAUDE_MD=
export HOST_CODEX_AGENTS_MD=
export HOST_OPENCODE_AGENTS_MD=
install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME"

ssh_port=$((36000 + ($$ % 12000)))
for _ in $(seq 1 100); do
  ss -Hln "sport = :$ssh_port" 2>/dev/null | grep -q . || break
  ssh_port=$((ssh_port + 1))
done
ss -Hln "sport = :$ssh_port" 2>/dev/null | grep -q . \
  && die 'could not reserve an unused loopback SSH port'

cat > "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME/config.env" <<EOF
# $MARKER
SSH_PORT=$ssh_port
CODING_TOOL_INTEGRATIONS=
ENVIRONMENT_PROFILES=
HOST_BASE=$STATE/host
HOST_MOUNTS=
RESTRICTED_DISK_PATHS=$STATE/host
FORWARD_SSH_AGENT=0
EOF
chmod 0600 "$SUBYARD_CONFIG_HOME/yards/$YARD_NAME/config.env"

info 'building the current candidate and initializing a restored-AppArmor container yard'
"$ROOT/dev/build-engine.sh" --force
yard init --yes
yard start --yes

incus project set "$PROJECT" user.subyard.e2e="$MARKER"
incus config set "$INSTANCE" user.subyard.e2e="$MARKER" --project "$PROJECT"

wait_guest_ready() {
  local _
  for _ in $(seq 1 120); do
    if guest true >/dev/null 2>&1 \
      && guest systemctl is-active --quiet docker >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  incus info "$INSTANCE" --project "$PROJECT" --show-log >&2 || true
  die 'yard or Docker did not become ready within 120 seconds'
}

device_present() {
  incus config device list "$INSTANCE" --project "$PROJECT" \
    | grep -Fxq subyard-docker-apparmor
}

assert_mask_absent() {
  ! device_present || die 'Docker AppArmor compatibility mask is unexpectedly present'
  [ "$(guest cat /sys/module/apparmor/parameters/enabled)" = Y ] \
    || die 'restored yard does not see the host AppArmor indicator'
}

assert_exact_mask() {
  device_present || die 'Docker AppArmor compatibility mask is missing'
  [ "$(incus config device get "$INSTANCE" subyard-docker-apparmor type \
    --project "$PROJECT")" = disk ] || die 'compatibility mask type drifted'
  [ "$(incus config device get "$INSTANCE" subyard-docker-apparmor source \
    --project "$PROJECT")" = /dev/null ] || die 'compatibility mask source drifted'
  [ "$(incus config device get "$INSTANCE" subyard-docker-apparmor path \
    --project "$PROJECT")" = /sys/module/apparmor/parameters/enabled ] \
    || die 'compatibility mask path drifted'
  [ "$(incus config device get "$INSTANCE" subyard-docker-apparmor readonly \
    --project "$PROJECT")" = true ] || die 'compatibility mask is writable'
  [ -z "$(guest cat /sys/module/apparmor/parameters/enabled)" ] \
    || die 'compatibility mask did not hide the AppArmor indicator'
}

docker_probe() {
  local phase="$1"
  wait_guest_ready
  if ! guest docker run --rm --pull=never alpine:3.22 true >/dev/null; then
    guest journalctl -u docker.service --no-pager -n 80 >&2 || true
    die "Docker failed after $phase"
  fi
  [ "$(guest docker inspect -f '{{.State.Running}}' apparmor-probe-sentinel)" = true ] \
    || die "Docker sentinel is not running after $phase"
  ok "Docker runs after $phase"
}

wait_guest_ready
guest docker pull alpine:3.22 >/dev/null
guest docker run -d --name apparmor-probe-sentinel --restart unless-stopped \
  alpine:3.22 sleep infinity >/dev/null
assert_mask_absent
docker_probe 'restored-state initialization'

instance_snapshot() {
  local config_hash instance_pid sentinel_started state
  state="$(incus list "$INSTANCE" --project "$PROJECT" -f csv -c s)"
  config_hash="$(incus config show "$INSTANCE" --project "$PROJECT" | sha256sum \
    | awk '{print $1}')"
  instance_pid="$(guest awk '{print $22}' /proc/1/stat)"
  sentinel_started="$(guest docker inspect -f '{{.State.StartedAt}}' \
    apparmor-probe-sentinel)"
  printf '%s:%s:%s:%s\n' "$state" "$config_hash" "$instance_pid" "$sentinel_started"
}

assert_unknown_diagnostic() {
  local output="$1"
  grep -Fqi 'AppArmor' "$output" \
    && grep -Eqi 'unknown|determine|probe' "$output" \
    || { tail -n 80 "$output" >&2; die 'failed probe omitted an actionable AppArmor diagnostic'; }
  ! grep -Fq 'INCUS_SECURITY_APPARMOR=' "$output" \
    || die 'failed probe diagnostic exposed the raw Incus environment'
}

run_failed_probe() {
  local output="$1"
  shift
  set +e
  env PATH="$STATE/bin:$PATH" \
    SUBYARD_E2E_REAL_SYSTEMCTL="$SYSTEMCTL_BIN" "$@" >"$output" 2>&1
  local rc=$?
  set -e
  [ "$rc" -ne 0 ] || die 'failed AppArmor capability probe reported success'
  assert_unknown_diagnostic "$output"
}

run_create_stage_failed_probe() {
  local output="$1"
  run_failed_probe "$output" env -u SUBYARD_PREPARED_INCUS_APPARMOR \
    ASSUME_YES=1 \
    SUBYARD_OPERATOR_HOME="$HOME" \
    SUBYARD_CONFIG_DIR="$ROOT/config" \
    SUBYARD_CONFIG_HOME="$SUBYARD_CONFIG_HOME" \
    SUBYARD_HOME="$SUBYARD_HOME" \
    SUBYARD_STATE_DIR="$SUBYARD_CONFIG_HOME/yards/$YARD_NAME/projects" \
    SUBYARD_ENGINE_CONTEXT=1 \
    SUBYARD_ENGINE_CONTEXT_SCHEMA=1 \
    SUBYARD_CONFIG_LOADED=1 \
    SUBYARD_POWER_DESIRED=running \
    STORAGE_PATH="$STORAGE_PATH" \
    HOST_BASE="$STATE/host" \
    RESTRICTED_DISK_PATHS="$STATE/host" \
    ACCESS_KIND=local \
    YARD_KIND=container \
    YARD_NAME="$YARD_NAME" \
    YARD_INSTANCE_NAME="$INSTANCE" \
    INCUS_PROJECT="$PROJECT" \
    INCUS_BRIDGE=incusbr0 \
    SSH_HOST="yard-$YARD_NAME" \
    DEV_USER=dev \
    DEV_UID=1000 \
    DEV_SUDO=0 \
    FORWARD_SSH_AGENT=0 \
    NESTED_E2E_VMS=0 \
    SHIFT_MODE=shift \
    SRV_POOL=default \
    SRV_VOLUME="yard-srv-$YARD_NAME" \
    bash "$ROOT/scripts/03-create-subyard.sh" --yes
}

assert_failed_probe_preserves_runtime() {
  local label="$1" before after
  before="$(instance_snapshot)"
  run_failed_probe "$STATE/$label-init.out" \
    "$ROOT/.build/yard" -Y "$YARD_NAME" init --yes
  after="$(instance_snapshot)"
  [ "$after" = "$before" ] || die "yard init mutated the $label fixture"

  run_create_stage_failed_probe "$STATE/$label-stage.out"
  after="$(instance_snapshot)"
  [ "$after" = "$before" ] || die "create stage mutated the $label fixture"
  [ "$(incus list "$INSTANCE" --project "$PROJECT" -f csv -c s)" = RUNNING ] \
    || die "failed probe stopped the $label fixture"
  ok "failed Go planning and shell-stage probes preserve the running $label fixture"
}

install -d -m 0755 "$STATE/bin"
cat > "$STATE/bin/systemctl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [ "$#" -eq 5 ] \
  && [ "$1" = show ] && [ "$2" = incus.service ] \
  && [ "$3" = -p ] && [ "$4" = Environment ] && [ "$5" = --value ]; then
  if [ "${SUBYARD_E2E_MALFORMED_APPARMOR:-0}" = 1 ]; then
    printf '%s\n' '"INCUS_SECURITY_APPARMOR=fa\lse"'
    exit 0
  fi
  printf 'synthetic command failure\n' >&2
  exit 75
fi
exec "${SUBYARD_E2E_REAL_SYSTEMCTL:?}" "$@"
EOF
chmod 0755 "$STATE/bin/systemctl"

assert_failed_probe_preserves_runtime missing-mask
export SUBYARD_E2E_MALFORMED_APPARMOR=1
assert_failed_probe_preserves_runtime malformed-missing-mask
unset SUBYARD_E2E_MALFORMED_APPARMOR
assert_mask_absent
docker_probe 'a failed probe with no mask'

info 'disabling Incus AppArmor through a marker-owned systemd override'
printf '%s\n' \
  "# $MARKER" \
  '[Service]' \
  'Environment=INCUS_SECURITY_APPARMOR=false' > "$STATE/incus-apparmor.conf"
sudo -n install -d -m 0755 "$(dirname "$APPARMOR_DROPIN")"
sudo -n install -m 0644 "$STATE/incus-apparmor.conf" "$APPARMOR_DROPIN"
DROPIN_ACTIVE=1
HOST_RUNTIME_CHANGED=1
sudo -n "$SYSTEMCTL_BIN" daemon-reload
timeout --foreground 120 sudo -n "$SYSTEMCTL_BIN" restart incus.service
wait_incus_ready || die 'Incus did not become ready with its AppArmor compatibility environment'
incus_apparmor_is_disabled \
  || die 'Incus did not expose the disabled AppArmor environment after restart'

transition_pid="$(guest awk '{print $22}' /proc/1/stat)"
yard init --yes
wait_guest_ready
[ "$(guest awk '{print $22}' /proc/1/stat)" != "$transition_pid" ] \
  || die 'restored-to-disabled convergence did not cross the guarded restart'
assert_exact_mask
docker_probe 'restored-to-disabled convergence'

assert_failed_probe_preserves_runtime existing-mask
export SUBYARD_E2E_MALFORMED_APPARMOR=1
assert_failed_probe_preserves_runtime malformed-existing-mask
unset SUBYARD_E2E_MALFORMED_APPARMOR
assert_exact_mask
docker_probe 'a failed probe with the exact mask'

info 'restoring the real Incus AppArmor runtime and converging away the compatibility mask'
remove_test_dropin || die 'could not restore the Incus AppArmor runtime'
if incus_apparmor_is_disabled; then
  die 'Incus retained the disabled AppArmor environment after restoration'
else
  probe_rc=$?
  [ "$probe_rc" -eq 1 ] || die 'could not verify the restored Incus service environment'
fi
transition_pid="$(guest awk '{print $22}' /proc/1/stat)"
yard init --yes
wait_guest_ready
[ "$(guest awk '{print $22}' /proc/1/stat)" != "$transition_pid" ] \
  || die 'disabled-to-restored convergence did not cross the guarded restart'
assert_mask_absent
docker_probe 'disabled-to-restored convergence'
converged_snapshot="$(instance_snapshot)"
yard init --yes
[ "$(instance_snapshot)" = "$converged_snapshot" ] \
  || die 'repeated restored-state init restarted or mutated the converged yard'

printf 'ok: Incus AppArmor probe failures preserve a running Docker yard and valid transitions converge\n'
