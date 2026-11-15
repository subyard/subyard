#!/usr/bin/env bash
# Marker-owned runtime, native custody and cleanup helpers for the P0 newer-release lane.
# shellcheck disable=SC2034 # Fixture variables are consumed by the sourcing lane.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
TOKEN="${1:-}"
STATE_ROOT="/var/tmp/subyard-p0-v0111-$TOKEN"
SUCCESS_ROOT="$STATE_ROOT/success"
MARKER="subyard-p0-v0111-$TOKEN"
SUCCESS_YARD="v0111-success-$TOKEN"
CLEANUP_ARMED=0
SUCCESS_ARMED=0

# select_fixture populates these fixture-scoped paths.
FIXTURE=
FIXTURE_ROOT=
FIXTURE_MARKER=
YARD_NAME=
PROJECT=
INSTANCE=
HOME_ROOT=
DATA_ROOT=
CONFIG_HOME=
RUNTIME_ROOT=
CACHE_ROOT=
BIN_ROOT=
JOURNAL=
POWER_RECONCILER=
POWER_UNIT_NAME=
POWER_UNIT=
POWER_UNIT_ENABLEMENT=
POWER_COORDINATION_BINDING=
TEST_VMS_SINK=
TEST_VMS_SINK_SERVICE_NAME=
TEST_VMS_SINK_SERVICE=
TEST_VMS_SINK_TIMER_NAME=
TEST_VMS_SINK_TIMER=

die() { printf 'release-transition-fixture: %s\n' "$*" >&2; exit 2; }
valid_token() { [[ "$1" =~ ^[0-9]+$ ]]; }

assert_state_root() {
  if [ -d "$STATE_ROOT" ] && [ ! -L "$STATE_ROOT" ] \
    && [ "$(cat "$STATE_ROOT/.subyard-p0-marker" 2>/dev/null)" = "$MARKER" ]; then
    return 0
  fi
  die "refusing unmarked state root $STATE_ROOT"
}

select_fixture() { # success
  FIXTURE="$1"
  case "$FIXTURE" in
    success)
      FIXTURE_ROOT="$SUCCESS_ROOT"
      YARD_NAME="$SUCCESS_YARD"
      ;;
    *) die "unknown fixture $FIXTURE" ;;
  esac
  FIXTURE_MARKER="$MARKER:$FIXTURE"
  PROJECT="subyard-$YARD_NAME"
  INSTANCE="yard-$YARD_NAME"
  HOME_ROOT="$FIXTURE_ROOT/home"
  DATA_ROOT="$FIXTURE_ROOT/data"
  CONFIG_HOME="$FIXTURE_ROOT/config"
  RUNTIME_ROOT="$DATA_ROOT/runtime"
  CACHE_ROOT="$DATA_ROOT/releases"
  BIN_ROOT="$HOME_ROOT/.local/bin"
  JOURNAL="$CONFIG_HOME/release-transition/v2/journal.json"
  POWER_RECONCILER=/usr/local/libexec/subyard/yard-boot-reconcile
  POWER_UNIT_NAME=subyard-power-reconcile.service
  POWER_UNIT="/etc/systemd/system/$POWER_UNIT_NAME"
  POWER_UNIT_ENABLEMENT="/etc/systemd/system/multi-user.target.wants/$POWER_UNIT_NAME"
  TEST_VMS_SINK="/usr/local/libexec/subyard/subyard-p0-v0111-$FIXTURE-$TOKEN-test-vms-sink"
  TEST_VMS_SINK_SERVICE_NAME="subyard-p0-v0111-$FIXTURE-$TOKEN-test-vms-host-sink.service"
  TEST_VMS_SINK_SERVICE="/etc/systemd/system/$TEST_VMS_SINK_SERVICE_NAME"
  TEST_VMS_SINK_TIMER_NAME="subyard-p0-v0111-$FIXTURE-$TOKEN-test-vms-host-sink.timer"
  TEST_VMS_SINK_TIMER="/etc/systemd/system/$TEST_VMS_SINK_TIMER_NAME"
}

fixture_root_is_marked() {
  [ -d "$FIXTURE_ROOT" ] && [ ! -L "$FIXTURE_ROOT" ] \
    && [ "$(cat "$FIXTURE_ROOT/.subyard-p0-marker" 2>/dev/null)" = "$FIXTURE_MARKER" ]
}

assert_fixture_root() {
  assert_state_root
  fixture_root_is_marked || die "refusing unmarked $FIXTURE fixture root $FIXTURE_ROOT"
}

fixture_env() { # command [args...]
  env \
    HOME="$HOME_ROOT" USER="$(id -un)" LOGNAME="$(id -un)" SHELL=/bin/bash \
    PATH="$BIN_ROOT:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" \
    SUBYARD_OPERATOR_HOME="$HOME_ROOT" SUBYARD_HOME="$DATA_ROOT" \
    SUBYARD_KEYS_SYSTEMD_DIR="$HOME_ROOT/.config/systemd/user" \
    SUBYARD_CONFIG_HOME="$CONFIG_HOME" SUBYARD_YARD="$YARD_NAME" \
    YARD_RUNTIME_ROOT="$RUNTIME_ROOT" YARD_RELEASE_CACHE="$CACHE_ROOT" \
    YARD_BIN_DIR="$BIN_ROOT" YARD_SHELL_RC="$HOME_ROOT/.bashrc" \
    YARD_LOGIN_RC="$HOME_ROOT/.profile" \
    SUBYARD_POWER_LIBEXEC_DIR=/usr/local/libexec/subyard \
    SUBYARD_POWER_RECONCILER_PATH="$POWER_RECONCILER" \
    SUBYARD_POWER_UNIT_PATH="$POWER_UNIT" \
    SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1 \
    SUBYARD_TEST_VMS_SINK_LIBEXEC_DIR=/usr/local/libexec/subyard \
    SUBYARD_TEST_VMS_SINK_PATH="$TEST_VMS_SINK" \
    SUBYARD_TEST_VMS_SINK_SERVICE_PATH="$TEST_VMS_SINK_SERVICE" \
    SUBYARD_TEST_VMS_SINK_TIMER_PATH="$TEST_VMS_SINK_TIMER" \
    "$@"
}

yard() { fixture_env "$BIN_ROOT/yard" "$@"; }

project_is_marked() {
  incus project show "$PROJECT" >/dev/null 2>&1 \
    && [ "$(incus project get "$PROJECT" user.p0-release-recovery 2>/dev/null)" = "$FIXTURE_MARKER" ]
}

assert_project_marker() {
  project_is_marked || die "refusing unmarked project $PROJECT"
}

cleanup_power_runtime() {
  local path state property
  # These are global defaults. Only the successful public teardown plans own
  # their removal; a failed history must retain them and its marked data.
  for path in "$POWER_RECONCILER" "$POWER_UNIT" "$POWER_UNIT_ENABLEMENT"; do
    [ ! -e "$path" ] && [ ! -L "$path" ] || return 1
  done
  for property in LoadState ActiveState MainPID ControlPID; do
    state="$(systemctl show "$POWER_UNIT_NAME" --property="$property" --value)" || return 1
    case "$property:$state" in
      LoadState:not-found|ActiveState:inactive|MainPID:0|ControlPID:0) ;;
      *) return 1 ;;
    esac
  done
}

cleanup_test_vms_sink() {
  local unsafe=0 touched=0 path
  for path in "$TEST_VMS_SINK" "$TEST_VMS_SINK_SERVICE" "$TEST_VMS_SINK_TIMER"; do
    if [ -e "$path" ] || [ -L "$path" ]; then
      [ -f "$path" ] && [ ! -L "$path" ] \
        && [ "$(stat -c '%u:%g' "$path" 2>/dev/null)" = 0:0 ] \
        || unsafe=1
    fi
  done
  if [ -e "$TEST_VMS_SINK_SERVICE" ]; then
    grep -Fqx "ExecStart=$TEST_VMS_SINK _test-vms-host-sink sync" \
      "$TEST_VMS_SINK_SERVICE" || unsafe=1
  fi
  if [ "$unsafe" -ne 0 ]; then
    printf 'release-transition-fixture: refusing unsafe test-vms sink artifact for %s\n' \
      "$FIXTURE" >&2
    return 1
  fi
  if [ -e "$TEST_VMS_SINK_TIMER" ]; then
    sudo -n systemctl disable --now "$TEST_VMS_SINK_TIMER_NAME" >/dev/null 2>&1 \
      || return 1
    touched=1
  fi
  if [ -e "$TEST_VMS_SINK_SERVICE" ]; then
    sudo -n systemctl stop "$TEST_VMS_SINK_SERVICE_NAME" >/dev/null 2>&1 || return 1
    touched=1
  fi
  for path in "$TEST_VMS_SINK_TIMER" "$TEST_VMS_SINK_SERVICE" "$TEST_VMS_SINK"; do
    if [ -e "$path" ]; then
      sudo -n find "$path" -maxdepth 0 -type f -delete || return 1
      touched=1
    fi
  done
  if [ "$touched" -eq 1 ]; then
    sudo -n systemctl daemon-reload >/dev/null 2>&1 || return 1
  fi
}

cleanup_fixture() { # success
  local fixture="$1" failed=0 armed=0 status
  select_fixture "$fixture"
  case "$fixture" in
    success) armed="$SUCCESS_ARMED" ;;
  esac
  [ "$armed" = 1 ] || return 0
  cleanup_power_runtime || {
    printf 'release-recovery cleanup fixture=%s stage=power-runtime retained\n' "$fixture" >&2
    return 1
  }
  if incus project show "$PROJECT" >/dev/null 2>&1; then
    if ! project_is_marked; then
      printf 'release-transition-fixture: refusing unmarked project %s\n' \
        "$PROJECT" >&2
      failed=1
    elif [ -x "$BIN_ROOT/yard" ]; then
      if yard -Y "$YARD_NAME" teardown --yes > "$FIXTURE_ROOT/cleanup-yard.log" 2>&1; then status=0; else status=$?; fi
      printf 'release-recovery cleanup fixture=%s stage=yard-teardown native_status=%s\n' "$fixture" "$status" >&2
      [ "$status" = 0 ] || cleanup_failure_diagnostics "$FIXTURE_ROOT/cleanup-yard.log"
    fi
  fi
  if incus project show "$PROJECT" >/dev/null 2>&1; then
    if ! project_is_marked; then
      failed=1
    else
      while IFS= read -r name; do
        [ -n "$name" ] || continue
        if [ "$(incus config get "$name" user.subyard.managed \
          --project "$PROJECT" 2>/dev/null)" != true ]; then
          failed=1
          break
        fi
        if incus delete "$name" --project "$PROJECT" --force >/dev/null 2>&1; then status=0; else status=$?; failed=1; fi
        printf 'release-recovery cleanup fixture=%s stage=instance-delete native_status=%s\n' "$fixture" "$status" >&2
      done < <(incus list --project "$PROJECT" --format csv -c n)
      if [ "$failed" = 0 ]; then
        if incus project delete "$PROJECT" >/dev/null 2>&1; then status=0; else status=$?; failed=1; fi
        printf 'release-recovery cleanup fixture=%s stage=project-delete native_status=%s\n' "$fixture" "$status" >&2
      fi
    fi
  fi
  [ "$failed" = 0 ] || printf 'release-recovery cleanup fixture=%s stage=project failed\n' "$fixture" >&2
  cleanup_test_vms_sink || {
    printf 'release-recovery cleanup fixture=%s stage=test-vms-sink failed\n' "$fixture" >&2
    failed=1
  }
  cleanup_power_runtime || {
    printf 'release-recovery cleanup fixture=%s stage=power-runtime retained\n' "$fixture" >&2
    failed=1
  }
  if [ -e "$FIXTURE_ROOT" ]; then
    if ! fixture_root_is_marked; then
      printf 'release-transition-fixture: refusing unmarked fixture root %s\n' \
        "$FIXTURE_ROOT" >&2
      failed=1
    elif [ "$failed" -eq 0 ]; then
      sudo -n find "$FIXTURE_ROOT" -depth -delete || {
        printf 'release-recovery cleanup fixture=%s stage=fixture-root failed\n' "$fixture" >&2
        failed=1
      }
    fi
  fi
  return "$failed"
}

cleanup() {
  local rc=$? cleanup_failed=0
  trap - EXIT INT TERM
  set +e
  if [ "$rc" != 0 ]; then
    select_fixture success
    if fixture_root_is_marked; then
      capture_fixture_failure_logs || printf 'release-recovery fixture=success failure-log capture_failed=true\n' >&2
    fi
  fi
  [ "$CLEANUP_ARMED" = 1 ] || exit "$rc"
  cleanup_fixture success || {
    printf 'release-recovery cleanup fixture=success stage=fixture failed\n' >&2
    cleanup_failed=1
  }
  if [ -e "$STATE_ROOT" ]; then
    assert_state_root || cleanup_failed=1
    [ "$cleanup_failed" -ne 0 ] || find "$STATE_ROOT" -depth -delete || {
      printf 'release-recovery cleanup stage=state-root failed\n' >&2
      cleanup_failed=1
    }
  fi
  if [ "$rc" -ne 0 ] || [ "$cleanup_failed" -ne 0 ]; then
    printf 'release-recovery cleanup original_status=%s cleanup_failed=%s\n' "$rc" "$cleanup_failed" >&2
  fi
  [ "$cleanup_failed" = 0 ] || rc=3
  exit "$rc"
}

handle_signal() { # <128+signal>
  local signal_rc="$1"
  trap - INT TERM
  exit "$signal_rc"
}

write_fixture_config() {
  local base_image="${P0_V0111_BASE_IMAGE:-subyard-e2e-debian-13-cloud-container}"
  local ssh_port=2291
  install -d -m 0700 "$CONFIG_HOME/yards/$YARD_NAME" "$BIN_ROOT" "$HOME_ROOT/host"
  printf 'AGENTS=\n' > "$CONFIG_HOME/config.env"
  # This workspace selects Codex to reproduce materialized-config drift. The
  # test-vms broker role forbids coding tools and is covered by the owner lane.
  printf '%s\n' \
    'YARD_KIND=container' \
    "SSH_PORT=$ssh_port" \
    'AGENTS=' \
    'DEV_UID=1001' \
    "BASE_IMAGE=$base_image" \
    "BASE_IMAGE_FALLBACK=$base_image" \
    "AGENT_codex_CONFIG=$HOME_ROOT/host/recovery-config" \
    'AGENT_codex_CONFIG_DEST=.codex/recovery-fixture-config' \
    > "$CONFIG_HOME/yards/$YARD_NAME/config.env"
  # Keep the selected plain artifact stable; shipped rules retain the genuine
  # historical source-path scope change during release activation.
  printf '# Fixture-owned recovery artifact.\n' > "$HOME_ROOT/host/recovery-config"
  chmod 0600 "$HOME_ROOT/host/recovery-config"
  printf 'release-recovery fixture=%s config_artifact=plain structured_toml_migration=false\n' "$FIXTURE"
  printf '# %s\nThe v0.11.1 %s fixture owns this file.\n' "$FIXTURE_MARKER" "$FIXTURE" \
    > "$HOME_ROOT/host/AGENTS.md"
  chmod 0600 "$CONFIG_HOME/config.env" \
    "$CONFIG_HOME/yards/$YARD_NAME/config.env" "$HOME_ROOT/host/AGENTS.md"
}

native_coordination_binding() { # owner resource...
  sudo -n python3 -B - "$ROOT/scripts/lib/native-install-custody.py" "$@" <<'PY'
import importlib.util, os, pathlib, sys
spec = importlib.util.spec_from_file_location('native_custody', sys.argv[1])
native = importlib.util.module_from_spec(spec)
spec.loader.exec_module(native)
held, paths, records = [], [], []
try:
    owner = sys.argv[2]
    resources = sorted(map(pathlib.Path, sys.argv[3:]))
    count = {'host-power': 2, 'test-vms-host-sink': 3}[owner]
    if len(resources) != count or len(set(resources)) != count:
        raise ValueError('invalid native coordination roles')
    for resource in resources:
        lock = resource.parent / ('.' + resource.name + '.native-install.lock')
        record = resource.parent / ('.' + resource.name + '.native-custody.json')
        paths.extend((str(lock), str(record)))
        try:
            native.safe_parent(resource.parent)
        except FileNotFoundError:
            if owner == 'host-power':
                raise
            continue
        if owner == 'test-vms-host-sink' and not os.path.lexists(lock) and not os.path.lexists(record):
            continue
        fd = os.open(lock, os.O_RDWR | os.O_NOFOLLOW)
        held.append((lock, fd))
        native.validate_lock(lock, fd)
        if os.fstat(fd).st_size != 0:
            raise ValueError('nonempty coordination lock')
        generation = native.observe(record, owner)
        if generation == '0':
            raise ValueError('missing coordination generation')
        records.append((record, generation))
    binding = native.installed_files(paths)
    for lock, fd in held:
        native.validate_lock(lock, fd)
    for record, generation in records:
        if native.observe(record, owner) != generation:
            raise ValueError('coordination generation changed')
    if native.installed_files(paths) != binding:
        raise ValueError('coordination identity changed')
    print(binding)
except (OSError, ValueError, KeyError, TypeError):
    print('release-recovery: unsafe, busy or changed native coordination state', file=sys.stderr)
    sys.exit(1)
finally:
    for _, fd in held:
        os.close(fd)
PY
}

power_coordination_binding() {
  native_coordination_binding host-power "$POWER_RECONCILER" "$POWER_UNIT"
}

require_absent_legacy_power() {
  local path artifact enabled
  # The published updater reloads shipped paths. Borrow only an empty default
  # power boundary; a preexisting service belongs to another fixture or owner.
  for path in "$POWER_RECONCILER" "$POWER_UNIT"; do
    [ ! -e "$path" ] && [ ! -L "$path" ] \
      || die 'historical recovery requires absent default power artifacts'
    [ -z "$POWER_COORDINATION_BINDING" ] || continue
    for artifact in "${path%/*}/.${path##*/}.native-install.lock" "${path%/*}/.${path##*/}.native-custody.json"; do
      [ ! -e "$artifact" ] && [ ! -L "$artifact" ] \
        || die "historical recovery requires absent default power artifacts and custody: $artifact"
    done
  done
  if [ -n "$POWER_COORDINATION_BINDING" ]; then
    [ "$(power_coordination_binding)" = "$POWER_COORDINATION_BINDING" ] \
      || die 'prior fixture power coordination state changed before reuse'
  fi
  [ ! -e "$POWER_UNIT_ENABLEMENT" ] && [ ! -L "$POWER_UNIT_ENABLEMENT" ] \
    || die 'historical recovery requires absent default power enablement'
  enabled="$(systemctl is-enabled "$POWER_UNIT_NAME" 2>/dev/null || true)"
  [ "$enabled" = not-found ] || die 'historical recovery requires an absent default power unit'
  printf 'release-recovery fixture=%s stage=default-power-absence passed\n' "$FIXTURE"
}

prepare_fixture() { # success
  select_fixture "$1"
  require_absent_legacy_power
  [ ! -e "$FIXTURE_ROOT" ] || die "fixture state already exists: $FIXTURE_ROOT"
  install -d -m 0700 "$FIXTURE_ROOT"
  printf '%s\n' "$FIXTURE_MARKER" > "$FIXTURE_ROOT/.subyard-p0-marker"
  chmod 0600 "$FIXTURE_ROOT/.subyard-p0-marker"
  case "$1" in
    success) SUCCESS_ARMED=1 ;;
  esac
  assert_fixture_root
  write_fixture_config
  prepare_operator_user_bus
}

# shellcheck disable=SC2120 # Host-free safety probes also pass the check-only mode.
prepare_operator_user_bus() {
  local operator uid binding manager_active=false bus_present=false mode="${1:-prepare}"
  operator="$(id -un)"
  uid="$(id -u)"
  binding="${SUBYARD_USER:-${SUDO_USER:-$operator}}"
  [ "$(id -u "$binding")" = "$uid" ] \
    || die 'fixture inherited a different teardown operator'
  systemctl is-active --quiet "user@$uid.service" && manager_active=true
  [ ! -S "/run/user/$uid/bus" ] || bus_present=true
  printf 'release-recovery fixture=%s stage=operator-user-bus mode=%s operator_uid=%s manager_active=%s bus_present=%s\n' \
    "$FIXTURE" "$mode" "$uid" "$manager_active" "$bus_present"
  if [ "$bus_present" = false ]; then
    [ "$mode" = prepare ] || die 'prepared operator user bus disappeared before cleanup'
    [ "$manager_active" = false ] || die 'operator user manager is active without its bus'
    # A synchronous disposable-VM prerequisite, preserving existing managers.
    sudo -n systemctl start "user@$uid.service"
  fi
  [ -S "/run/user/$uid/bus" ] || die 'operator user bus did not start'
  env XDG_RUNTIME_DIR="/run/user/$uid" DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$uid/bus" \
    systemctl --user show-environment >/dev/null \
    || die 'operator user bus is unavailable for producer cleanup'
  printf 'release-recovery fixture=%s stage=operator-user-bus operator_uid=%s user_bus_ready=true\n' "$FIXTURE" "$uid"
}

capture_fixture_failure_logs() {
  local name path failed=0
  fixture_root_is_marked || return 1
  for name in source-install.log candidate-recovery.log current-upgrade.log \
    cleanup-yard.log current-yard-teardown.log current-uninstall.log; do
    path="$FIXTURE_ROOT/$name"
    [ -f "$path" ] && [ ! -L "$path" ] || continue
    if [ "$(stat -c %h "$path")" != 1 ]; then
      printf 'release-recovery fixture=%s failure-log=%s unsafe=true\n' "$FIXTURE" "$name" >&2
      failed=1
      continue
    fi
    printf 'release-recovery fixture=%s failure-log=%s bounded_tail=true\n' "$FIXTURE" "$name" >&2
    cleanup_failure_diagnostics "$path" >&2 || failed=1
  done
  return "$failed"
}

cleanup_failure_diagnostics() { # bounded operation errors, never plans or config contents
  python3 - "$1" "$FIXTURE_ROOT" 2>/dev/null <<'PY' || printf 'release-recovery cleanup-error capture_failed=true\n' >&2
import json, pathlib, re, sys
with pathlib.Path(sys.argv[1]).open('rb') as source:
    source.seek(0, 2)
    source.seek(max(0, source.tell() - 8192))
    text = source.read(8192).decode(errors='replace')
lines = re.sub(r'\x1b\[[0-?]*[ -/]*[@-~]', '', text).splitlines()
# Native Python leaves also report plain errors/tracebacks. Preserve their
# diagnostics while excluding JSON inventories/plans and environment assignments.
errors = [line for line in lines if line.strip()
          and not line.lstrip().startswith(('{', '}', '"', '[{', '["'))
          and line.strip() not in ('[', ']', '],')
          and not re.match(r'^\s*[A-Z][A-Z0-9_]*=', line)
          and not re.match(r'^\s*(?:guard|observed|desired|verify):|^\s*\[(?:skip|[0-9]+)\]', line)]
for line in '\n'.join(errors[-20:])[-4096:].splitlines():
    line = line.replace(sys.argv[2], '<fixture>').replace('/home/dev', '<guest-home>')
    line = re.sub(r'plan-v1-[a-f0-9]{64}', '<plan>', line)
    print('release-recovery cleanup-error ' + json.dumps(line[:2048]))
if not errors:
    print('release-recovery cleanup-error operation_error_line_present=false')
PY
}
