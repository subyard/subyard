#!/usr/bin/env bash
# Real-host regression for the first named init after incus-admin membership changes.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
STATE=''
MARKER=''
OPERATOR=''
OPERATOR_HOME=''
SUDOERS=''
YARD_NAME=''
PROJECT=''
INSTANCE=''
POWER_UNIT=''
POWER_RECONCILER=''
SG_PROOF=''
CANDIDATE=''

die() { printf 'incus-group-reexec-e2e: %s\n' "$*" >&2; exit 2; }
info() { printf '  [ .. ] %s\n' "$*"; }
ok() { printf '  [ ok ] %s\n' "$*"; }

[ "${SUBYARD_E2E_VM:-}" = 1 ] \
  || die 'run on VM1 through dev/agent-e2e.sh'

if [ "$(id -u)" -ne 0 ]; then
  for command in go sudo; do
    command -v "$command" >/dev/null 2>&1 || die "$command is required"
  done
  info 'building the current development candidate'
  "$ROOT/dev/build-engine.sh" --force
  exec sudo -n env \
    SUBYARD_E2E_VM="$SUBYARD_E2E_VM" \
    SUBYARD_GROUP_REEXEC_ROOT=1 \
    SUBYARD_E2E_CALLER="$(id -un)" \
    bash "$ROOT/dev/e2e/incus-group-reexec.sh"
fi

[ "${SUBYARD_GROUP_REEXEC_ROOT:-}" = 1 ] \
  || die 'run as the disposable VM operator, not directly as root'
for command in getent grep runuser sg sha256sum ss systemctl timeout useradd userdel visudo; do
  command -v "$command" >/dev/null 2>&1 || die "$command is required"
done
[ -x "$ROOT/.build/yard" ] || die 'development candidate was not built'
[ -x /usr/bin/sg ] || die '/usr/bin/sg is required'

incus() { /usr/bin/incus "$@"; }

operator_env() {
  timeout --foreground 1800 /usr/sbin/runuser -u "$OPERATOR" -- bash -c '
    cd "$1"
    shift
    exec "$@"
  ' _ "$OPERATOR_HOME" env -i \
      PATH="$STATE/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" \
      LANG=C.UTF-8 \
      HOME="$OPERATOR_HOME" \
      USER="$OPERATOR" \
      LOGNAME="$OPERATOR" \
      SHELL=/bin/bash \
      SUBYARD_OPERATOR_HOME="$OPERATOR_HOME" \
      SUBYARD_CONFIG_HOME="$OPERATOR_HOME/.config/subyard" \
      SUBYARD_HOME="$OPERATOR_HOME/.subyard" \
      STORAGE_PATH="$OPERATOR_HOME/.subyard/incus/storage" \
      SUBYARD_NO_AUDIT=1 \
      SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1 \
      SUBYARD_POWER_LIBEXEC_DIR="$STATE/power" \
      SUBYARD_POWER_RECONCILER_PATH="$POWER_RECONCILER" \
      SUBYARD_POWER_UNIT_PATH="$POWER_UNIT" \
      MIN_DISK_GIB=1 \
      HOST_CLAUDE_MD= \
      HOST_CODEX_AGENTS_MD= \
      HOST_OPENCODE_AGENTS_MD= \
      "$@"
}

operator_yard() {
  operator_env "$CANDIDATE/.build/yard" "$@"
}

setting() {
  local yard_name="$1" name="$2" output
  output="$(operator_yard -Y "$yard_name" config show "$name")"
  printf '%s\n' "$output" | sed -n 's/^effective: //p'
}

cleanup_power_fixture() {
  local expected unit_name
  [ -n "$POWER_UNIT" ] || return 0
  case "$POWER_UNIT" in
    /etc/systemd/system/subyard-group-reexec-e2e-v1-*.service) ;;
    *) return 1 ;;
  esac
  if [ -e "$POWER_UNIT" ]; then
    [ -f "$POWER_UNIT" ] && [ ! -L "$POWER_UNIT" ] || return 1
    expected="$(sed "s|@SUBYARD_POWER_RECONCILER@|$POWER_RECONCILER|g" \
      "$ROOT/config/systemd/subyard-power-reconcile.service.in")"
    [ "$(cat "$POWER_UNIT")" = "$expected" ] || return 1
    unit_name="${POWER_UNIT##*/}"
    systemctl disable --now "$unit_name" >/dev/null 2>&1 || true
    find "$POWER_UNIT" -maxdepth 0 -type f -delete || return 1
    systemctl daemon-reload || return 1
  fi
  [ ! -e "$POWER_UNIT" ]
}

cleanup() {
  local rc=$? project_marker='' account_home='' expected_sudoers keep_state=0
  trap - EXIT INT TERM
  set +e

  if [ -n "$PROJECT" ] && command -v incus >/dev/null 2>&1 \
    && incus project show "$PROJECT" >/dev/null 2>&1; then
    project_marker="$(incus project get "$PROJECT" user.subyard.e2e 2>/dev/null)"
    if { [ -z "$project_marker" ] || [ "$project_marker" = "$MARKER" ]; } \
      && [ -f "$OPERATOR_HOME/.config/subyard/yards/$YARD_NAME/config.env" ] \
      && grep -Fqx "# $MARKER" \
        "$OPERATOR_HOME/.config/subyard/yards/$YARD_NAME/config.env"; then
      operator_yard -Y "$YARD_NAME" teardown --yes >/dev/null 2>&1 || rc=3
    else
      printf 'incus-group-reexec-e2e: refusing to remove ambiguous project %s\n' \
        "$PROJECT" >&2
      rc=3
    fi
  fi

  if [ -n "$PROJECT" ] && command -v incus >/dev/null 2>&1 \
    && incus project show "$PROJECT" >/dev/null 2>&1; then
    printf 'incus-group-reexec-e2e: retaining fixture after incomplete yard teardown\n' >&2
    rc=3
    keep_state=1
  fi

  if [ "$keep_state" = 0 ]; then
    if ! cleanup_power_fixture; then
      printf 'incus-group-reexec-e2e: retaining fixture after incomplete power cleanup\n' >&2
      rc=3
      keep_state=1
    fi
  fi
  if [ -n "$SUDOERS" ] && [ -e "$SUDOERS" ]; then
    expected_sudoers="$OPERATOR ALL=(root) NOPASSWD: ALL"
    case "$SUDOERS" in /etc/sudoers.d/subyard-group-reexec-*) ;;
      *) rc=3; expected_sudoers='' ;;
    esac
    if [ -n "$expected_sudoers" ] && [ -f "$SUDOERS" ] && [ ! -L "$SUDOERS" ] \
      && [ "$(cat "$SUDOERS")" = "$expected_sudoers" ] \
      && [ "$(stat -c '%u:%g:%a' "$SUDOERS")" = 0:0:440 ]; then
      find "$SUDOERS" -maxdepth 0 -type f -delete || rc=3
    else
      printf 'incus-group-reexec-e2e: refusing to remove changed sudoers file %s\n' \
        "$SUDOERS" >&2
      rc=3
    fi
  fi

  if [ "$keep_state" = 0 ] && [ -n "$OPERATOR" ] \
    && getent passwd "$OPERATOR" >/dev/null 2>&1; then
    account_home="$(getent passwd "$OPERATOR" | cut -d: -f6)"
    if [[ "$OPERATOR" = sy-group-* ]] && [ "$account_home" = "$OPERATOR_HOME" ] \
      && [ -f "$OPERATOR_HOME/.subyard-group-reexec-home" ] \
      && [ "$(cat "$OPERATOR_HOME/.subyard-group-reexec-home")" = "$MARKER" ]; then
      userdel "$OPERATOR" >/dev/null 2>&1 || rc=3
    else
      printf 'incus-group-reexec-e2e: refusing to remove ambiguous fixture account %s\n' \
        "$OPERATOR" >&2
      rc=3
    fi
  fi

  if [ "$keep_state" = 0 ] && [ -n "$STATE" ] \
    && [[ "$STATE" = /var/tmp/subyard-group-reexec.* ]] \
    && [ -f "$STATE/.marker" ] && [ "$(cat "$STATE/.marker")" = "$MARKER" ] \
    && ! getent passwd "$OPERATOR" >/dev/null 2>&1; then
    find "$STATE" -depth -delete || rc=3
  fi
  exit "$rc"
}
trap cleanup EXIT INT TERM

ensure_incus_platform() {
  local base_operator base_home platform_root marker temporary
  if command -v incus >/dev/null 2>&1 \
    && incus info >/dev/null 2>&1 \
    && incus storage show default --project default >/dev/null 2>&1 \
    && incus network show incusbr0 --project default >/dev/null 2>&1; then
    return
  fi

  base_operator="${SUBYARD_E2E_CALLER:-}"
  getent passwd "$base_operator" >/dev/null 2>&1 \
    || die 'could not resolve the disposable VM operator for platform setup'
  base_home="$(getent passwd "$base_operator" | cut -d: -f6)"
  platform_root="$base_home/.cache/subyard-e2e-platform"
  marker="$platform_root/.subyard-e2e-platform-marker"
  info 'reconciling the real Incus platform with the product installer'
  install -d -o "$base_operator" -g "$(id -gn "$base_operator")" -m 0700 \
    "$platform_root"
  /usr/sbin/runuser -u "$base_operator" -- env -i \
    PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    LANG=C.UTF-8 HOME="$base_home" USER="$base_operator" LOGNAME="$base_operator" \
    SHELL=/bin/bash ROOT="$ROOT" PLATFORM_ROOT="$platform_root" \
    SUBYARD_USER="$base_operator" bash -c '
      set -euo pipefail
      . "$ROOT/tests/helpers/test-context.sh"
      setup_test_context "$PLATFORM_ROOT/bootstrap"
      export SUBYARD_USER SUBYARD_OPERATOR_HOME="$HOME"
      export SUBYARD_CONFIG_DIR="$ROOT/config"
      export SUBYARD_CONFIG_HOME="$PLATFORM_ROOT/bootstrap-config"
      export SUBYARD_HOME="$PLATFORM_ROOT"
      export STORAGE_PATH="$PLATFORM_ROOT/incus/incus/storage"
      export HOST_BASE="$PLATFORM_ROOT/bootstrap-host-data"
      export RESTRICTED_DISK_PATHS="$HOST_BASE"
      set -a
      . "$ROOT/config/host.env"
      set +a
      bash "$ROOT/scripts/01-install-incus.sh" --yes --zabbly
    '
  [ -x /usr/bin/incus ] || die 'product installer did not install Incus'
  incus info >/dev/null 2>&1 || die 'Incus owner API is unavailable after reconciliation'
  incus storage show default --project default >/dev/null 2>&1 \
    || die 'Incus default storage pool is unavailable after reconciliation'
  incus network show incusbr0 --project default >/dev/null 2>&1 \
    || die 'Incus managed bridge is unavailable after reconciliation'
  if [ -e "$marker" ]; then
    [ "$(cat "$marker")" = subyard-e2e-platform-v1 ] \
      || die "unexpected shared E2E platform marker: $marker"
  else
    temporary="$(mktemp "$platform_root/.platform-marker.XXXXXX")"
    printf '%s\n' subyard-e2e-platform-v1 > "$temporary"
    chown "$base_operator:$(id -gn "$base_operator")" "$temporary"
    chmod 0600 "$temporary"
    mv -f -- "$temporary" "$marker"
  fi
}

STATE="$(mktemp -d /var/tmp/subyard-group-reexec.XXXXXX)"
chmod 0711 "$STATE"
token="$(printf '%s' "${STATE##*.}" | tr '[:upper:]' '[:lower:]')"
MARKER="subyard-group-reexec-e2e-v1-$token"
printf '%s\n' "$MARKER" > "$STATE/.marker"
chmod 0600 "$STATE/.marker"
OPERATOR="sy-group-$token"
OPERATOR_HOME="$STATE/home"
SUDOERS="/etc/sudoers.d/subyard-group-reexec-$token"
YARD_NAME="group-reexec-$token"
PROJECT="subyard-$YARD_NAME"
INSTANCE="yard-$YARD_NAME"
POWER_UNIT="/etc/systemd/system/$MARKER.service"
POWER_RECONCILER="$STATE/power/yard-boot-reconcile"
SG_PROOF="$STATE/sg-invocations"
CANDIDATE="$STATE/candidate"

ensure_incus_platform

getent passwd "$OPERATOR" >/dev/null 2>&1 \
  && die "refusing existing fixture account $OPERATOR"
[ ! -e "$SUDOERS" ] || die "refusing existing sudoers fixture $SUDOERS"
[ ! -e "$POWER_UNIT" ] || die "refusing existing power unit fixture $POWER_UNIT"
incus project show "$PROJECT" >/dev/null 2>&1 \
  && die "refusing existing Incus project $PROJECT"

useradd --user-group --no-create-home --home-dir "$OPERATOR_HOME" \
  --shell /bin/bash "$OPERATOR"
install -d -o "$OPERATOR" -g "$OPERATOR" -m 0700 \
  "$OPERATOR_HOME" "$OPERATOR_HOME/.config/subyard/yards/$YARD_NAME" \
  "$OPERATOR_HOME/.subyard" "$OPERATOR_HOME/host"
printf '%s\n' "$MARKER" > "$OPERATOR_HOME/.subyard-group-reexec-home"
chown "$OPERATOR:$OPERATOR" "$OPERATOR_HOME/.subyard-group-reexec-home"
chmod 0600 "$OPERATOR_HOME/.subyard-group-reexec-home"
[ "$(cat "$OPERATOR_HOME/.subyard-group-reexec-home")" = "$MARKER" ] \
  || die 'fixture home ownership marker changed before initialization'
chown -R "$OPERATOR:$OPERATOR" "$OPERATOR_HOME"
[ -z "$(find "$OPERATOR_HOME" \! -user "$OPERATOR" -print -quit)" ] \
  || die 'fixture operator does not own its fresh home tree'

install -d -m 0755 "$CANDIDATE" "$CANDIDATE/.build"
cp -a "$ROOT/config" "$ROOT/scripts" "$CANDIDATE/"
install -m 0755 "$ROOT/.build/yard" "$CANDIDATE/.build/yard"
chown -R "$OPERATOR:$OPERATOR" "$CANDIDATE"

printf '%s ALL=(root) NOPASSWD: ALL\n' "$OPERATOR" > "$STATE/sudoers"
chmod 0440 "$STATE/sudoers"
visudo -cf "$STATE/sudoers" >/dev/null
install -o root -g root -m 0440 "$STATE/sudoers" "$SUDOERS"

install -d -m 0755 "$STATE/bin"
cat > "$STATE/bin/sg" <<EOF
#!/usr/bin/env bash
set -euo pipefail
if [ "\$#" -eq 3 ] && [ "\$1" = incus-admin ] && [ "\$2" = -c ]; then
  printf 'incus-admin\n' >> '$SG_PROOF'
fi
exec /usr/bin/sg "\$@"
EOF
chmod 0755 "$STATE/bin/sg"
install -o "$OPERATOR" -g "$OPERATOR" -m 0600 /dev/null "$SG_PROOF"

id -nG "$OPERATOR" | tr ' ' '\n' | grep -Fxq incus-admin \
  && die 'fresh fixture account unexpectedly belongs to incus-admin'
operator_env id -nG | tr ' ' '\n' | grep -Fxq incus-admin \
  && die 'fresh fixture process unexpectedly has incus-admin active'
if operator_env /usr/bin/incus info >/dev/null 2>&1; then
  die 'fresh fixture process can already reach Incus without incus-admin'
fi
ok 'isolated operator starts without Incus group membership or socket access'

ssh_port=$((36000 + ($$ % 12000)))
for _ in $(seq 1 100); do
  ss -Hln "sport = :$ssh_port" 2>/dev/null | grep -q . || break
  ssh_port=$((ssh_port + 1))
done
ss -Hln "sport = :$ssh_port" 2>/dev/null | grep -q . \
  && die 'could not reserve an unused loopback SSH port'

base_image=subyard-e2e-debian-13-cloud-container
if ! incus image info "$base_image" --project default >/dev/null 2>&1; then
  base_image=images:debian/13
fi
cat > "$OPERATOR_HOME/.config/subyard/yards/$YARD_NAME/config.env" <<EOF
# $MARKER
SSH_PORT=$ssh_port
YARD_IMAGE=$base_image
YARD_IMAGE_FALLBACK=images:ubuntu/24.04
CODING_TOOL_INTEGRATIONS=
ENVIRONMENT_PROFILES=
HOST_BASE=$OPERATOR_HOME/host
HOST_MOUNTS=
RESTRICTED_DISK_PATHS=$OPERATOR_HOME/host
FORWARD_SSH_AGENT=0
EOF
chown "$OPERATOR:$OPERATOR" \
  "$OPERATOR_HOME/.config/subyard/yards/$YARD_NAME/config.env"
chmod 0600 "$OPERATOR_HOME/.config/subyard/yards/$YARD_NAME/config.env"

[ "$(setting default SSH_PORT)" = 2222 ] \
  || die 'fresh default yard did not start with shipped SSH_PORT 2222'
[ "$(setting "$YARD_NAME" SSH_PORT)" = "$ssh_port" ] \
  || die 'named yard did not resolve its registered SSH port'

info 'running the first named init through the stale-group process'
first_output="$STATE/first-init.out"
if ! operator_env DEV_UID=1357 "$CANDIDATE/.build/yard" \
  -Y "$YARD_NAME" init --yes > "$first_output" 2>&1; then
  tail -n 160 "$first_output" >&2 || true
  die 'first named init did not survive incus-admin activation'
fi
grep -Fq "added $OPERATOR to incus-admin" "$first_output" \
  || die 'product installer did not add the fixture operator to incus-admin'
grep -Fq "SSH_PORT $ssh_port is unique across configured yards" "$first_output" \
  || die 'resumed init did not validate the named SSH port in isolated contexts'
grep -Fq 'Subyard initialized' "$first_output" \
  || die 'resumed named init did not complete'
[ "$(cat "$SG_PROOF")" = incus-admin ] \
  || die 'first named init did not use exactly one real sg incus-admin re-exec'
id -nG "$OPERATOR" | tr ' ' '\n' | grep -Fxq incus-admin \
  || die 'product installer did not persist incus-admin membership'
incus project show "$PROJECT" >/dev/null 2>&1 \
  || die 'first named init did not create its Incus project'
incus info "$INSTANCE" --project "$PROJECT" >/dev/null 2>&1 \
  || die 'first named init did not create its Incus instance'
incus project set "$PROJECT" user.subyard.e2e="$MARKER"
incus config set "$INSTANCE" user.subyard.e2e="$MARKER" --project "$PROJECT"
initial_state="$(incus list "$INSTANCE" --project "$PROJECT" -f csv -c s)"
[ "$initial_state" = STOPPED ] \
  || die "first init left unexpected instance state $initial_state instead of STOPPED"
info 'starting the initialized yard to inspect its provisioned operator identity'
start_output="$STATE/start.out"
if ! operator_yard -Y "$YARD_NAME" start --yes > "$start_output" 2>&1; then
  tail -n 120 "$start_output" >&2 || true
  die 'could not start the initialized yard for explicit-override verification'
fi
running_state="$(incus list "$INSTANCE" --project "$PROJECT" -f csv -c s)"
[ "$running_state" = RUNNING ] \
  || die "yard start completed with unexpected instance state $running_state"
if ! actual_dev_uid="$(incus exec "$INSTANCE" --project "$PROJECT" -- id -u dev)"; then
  die 'running yard did not expose its provisioned dev account for UID verification'
fi
[ "$actual_dev_uid" = 1357 ] \
  || die "explicit DEV_UID launch override resolved to $actual_dev_uid instead of 1357"
[ "$(setting default SSH_PORT)" = 2222 ] \
  || die 'named init polluted the synthetic default SSH port'
[ "$(setting "$YARD_NAME" SSH_PORT)" = "$ssh_port" ] \
  || die 'named init changed the named SSH port'
ok 'real sg re-exec preserved the explicit override and named-yard boundary'

instance_snapshot() {
  local state config_hash init_start
  state="$(incus list "$INSTANCE" --project "$PROJECT" -f csv -c s)"
  config_hash="$(incus config show "$INSTANCE" --project "$PROJECT" | sha256sum \
    | awk '{print $1}')"
  init_start="$(incus exec "$INSTANCE" --project "$PROJECT" -- awk '{print $22}' \
    /proc/1/stat)"
  printf '%s:%s:%s\n' "$state" "$config_hash" "$init_start"
}

before="$(instance_snapshot)"
retry_output="$STATE/retry-init.out"
info 're-running the named init to verify convergence'
if ! operator_env DEV_UID=1357 "$CANDIDATE/.build/yard" \
  -Y "$YARD_NAME" init --yes > "$retry_output" 2>&1; then
  tail -n 160 "$retry_output" >&2 || true
  die 'repeated named init failed'
fi
grep -Fq 'Everything is already set up' "$retry_output" \
  || die 'repeated named init did not report convergence'
[ "$(instance_snapshot)" = "$before" ] \
  || die 'repeated named init restarted or mutated the converged yard'
[ "$(cat "$SG_PROOF")" = incus-admin ] \
  || die 'converged retry unexpectedly invoked sg again'
[ "$(setting default SSH_PORT)" = 2222 ] \
  || die 'converged retry polluted the synthetic default SSH port'

printf 'ok: first named init survives real incus-admin re-exec without crossing config boundaries\n'
