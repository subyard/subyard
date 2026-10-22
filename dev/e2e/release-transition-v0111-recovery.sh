#!/usr/bin/env bash
# Exact published v0.11.1 post-activation recovery on the existing P0 Incus lease.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
TOKEN="${1:-}"
OLD_VERSION=0.9.1
SOURCE_VERSION=0.11.1
CANDIDATE_VERSION=0.11.2
OLD_INSTALLER_SHA256=5bd3c61e3dd39cb2d258be5cd75237383f00eff0512c77a3a5ca75d96e6b992b
SOURCE_INSTALLER_SHA256=41acb799e55cf82cdbbef8e7f75e8e17c2df344c1ceeb748181fb6aec7ea6f8d
STATE_ROOT="/var/tmp/subyard-p0-v0111-$TOKEN"
SUCCESS_ROOT="$STATE_ROOT/success"
POST_CAS_ROOT="$STATE_ROOT/post-cas"
MARKER="subyard-p0-v0111-$TOKEN"
SUCCESS_YARD="v0111-success-$TOKEN"
POST_CAS_YARD="v0111-post-cas-$TOKEN"
RELEASE_ROOT="$STATE_ROOT/candidate-release"
OLD_INSTALLER="$STATE_ROOT/subyard-install-$OLD_VERSION.sh"
SOURCE_INSTALLER="$STATE_ROOT/subyard-install-$SOURCE_VERSION.sh"
CLEANUP_ARMED=0
SUCCESS_ARMED=0
POST_CAS_ARMED=0
POST_CAS_TRANSITION_PID=
POST_CAS_TRANSITION_STATUS=
POST_CAS_OBSERVER="$ROOT/dev/e2e/release-transition-post-cas-observer.py"
POST_CAS_OBSERVATION_MARKER=

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
LEDGER=
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
CANDIDATE_BUNDLE=
CANDIDATE_RELEASE=
CURRENT_RELEASE_ROOT=
CURRENT_VERSION=
CURRENT_RUNTIME_SHA256=
CURRENT_MANIFEST_SHA256=
CURRENT_UNINSTALL_REFUSED=

die() { printf 'release-transition-v0111-recovery: %s\n' "$*" >&2; exit 2; }
info() { printf '  [ .. ] %s\n' "$*"; }
ok() { printf '  [ ok ] %s\n' "$*"; }
valid_token() { [[ "$1" =~ ^[0-9]+$ ]]; }

assert_state_root() {
  if [ -d "$STATE_ROOT" ] && [ ! -L "$STATE_ROOT" ] \
    && [ "$(cat "$STATE_ROOT/.subyard-p0-marker" 2>/dev/null)" = "$MARKER" ]; then
    return
  fi
  die "refusing unmarked state root $STATE_ROOT"
}

select_fixture() { # success|post-cas
  FIXTURE="$1"
  case "$FIXTURE" in
    success)
      FIXTURE_ROOT="$SUCCESS_ROOT"
      YARD_NAME="$SUCCESS_YARD"
      ;;
    post-cas)
      FIXTURE_ROOT="$POST_CAS_ROOT"
      YARD_NAME="$POST_CAS_YARD"
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
  LEDGER="$CONFIG_HOME/release-transition/v2/ledger.json"
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
    && [ "$(incus project get "$PROJECT" user.subyard.p0 2>/dev/null)" = "$FIXTURE_MARKER" ]
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
    printf 'release-transition-v0111-recovery: refusing unsafe test-vms sink artifact for %s\n' \
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

cleanup_fixture() { # success|post-cas
  local fixture="$1" failed=0 armed=0 status
  select_fixture "$fixture"
  case "$fixture" in
    success) armed="$SUCCESS_ARMED" ;;
    post-cas) armed="$POST_CAS_ARMED" ;;
  esac
  [ "$armed" = 1 ] || return 0
  if [ "$CURRENT_UNINSTALL_REFUSED" = "$fixture" ]; then
    printf 'release-recovery cleanup fixture=%s stage=current-cleanup retained\n' "$fixture" >&2
    return 1
  fi
  cleanup_power_runtime || {
    printf 'release-recovery cleanup fixture=%s stage=power-runtime retained\n' "$fixture" >&2
    return 1
  }
  if incus project show "$PROJECT" >/dev/null 2>&1; then
    if ! project_is_marked; then
      printf 'release-transition-v0111-recovery: refusing unmarked project %s\n' \
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
      printf 'release-transition-v0111-recovery: refusing unmarked fixture root %s\n' \
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

reap_post_cas_transition_leader() { # bounded poll count
  local attempts="$1" state
  for _ in $(seq 1 "$attempts"); do
    state="$(ps -o stat= -p "$POST_CAS_TRANSITION_PID" 2>/dev/null \
      | awk 'NR == 1 { print substr($1, 1, 1) }')"
    if [ -z "$state" ] || [ "$state" = Z ]; then
      if wait "$POST_CAS_TRANSITION_PID" 2>/dev/null; then
        POST_CAS_TRANSITION_STATUS=0
      else
        POST_CAS_TRANSITION_STATUS=$?
      fi
      return 0
    fi
    sleep 0.05
  done
  return 1
}

stop_post_cas_transition() { # [TERM|KILL]
  local initial_signal="${1:-TERM}"
  [ -n "$POST_CAS_TRANSITION_PID" ] || return 0
  [[ "$POST_CAS_TRANSITION_PID" =~ ^[1-9][0-9]*$ ]] || return 1
  case "$initial_signal" in
    TERM)
    kill -TERM -- "-$POST_CAS_TRANSITION_PID" 2>/dev/null || true
      kill -TERM -- "$POST_CAS_TRANSITION_PID" 2>/dev/null || true
      if ! reap_post_cas_transition_leader 200; then
        kill -KILL -- "-$POST_CAS_TRANSITION_PID" 2>/dev/null || true
        kill -KILL -- "$POST_CAS_TRANSITION_PID" 2>/dev/null || true
        reap_post_cas_transition_leader 100 || return 1
      fi
      ;;
    KILL)
      kill -KILL -- "-$POST_CAS_TRANSITION_PID" 2>/dev/null || true
      kill -KILL -- "$POST_CAS_TRANSITION_PID" 2>/dev/null || true
      reap_post_cas_transition_leader 100 || return 1
      ;;
    *) return 1 ;;
  esac
  kill -KILL -- "-$POST_CAS_TRANSITION_PID" 2>/dev/null || true
  for _ in $(seq 1 100); do
    kill -0 -- "-$POST_CAS_TRANSITION_PID" 2>/dev/null || {
      POST_CAS_TRANSITION_PID=
      return 0
    }
    sleep 0.05
  done
  return 1
}

fence_reaped_post_cas_transition() { # observed-status
  local observed_status="$1"
  [[ "$observed_status" =~ ^[0-9]+$ ]] || return 1
  stop_post_cas_transition KILL || return 1
  # shellcheck disable=SC2034 # observed by the host-free contract when this helper is sourced
  POST_CAS_TRANSITION_STATUS="$observed_status"
}

cleanup() {
  local rc=$? cleanup_failed=0
  trap - EXIT INT TERM
  set +e
  if ! stop_post_cas_transition; then
    printf 'release-transition-v0111-recovery: post-CAS process group did not stop\n' >&2
    printf 'release-recovery cleanup stage=post-cas-process original_status=%s final_status=3\n' "$rc" >&2
    exit 3
  fi
  [ "$CLEANUP_ARMED" = 1 ] || exit "$rc"
  cleanup_fixture post-cas || {
    printf 'release-recovery cleanup fixture=post-cas stage=fixture failed\n' >&2
    cleanup_failed=1
  }
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

download_pinned_installer() { # version digest destination
  local version="$1" digest="$2" destination="$3"
  curl -fsSL --proto '=https' --tlsv1.2 \
    "https://github.com/Subyard/Subyard/releases/download/v$version/subyard-install.sh" \
    -o "$destination"
  [ "$(sha256sum "$destination" | awk '{print $1}')" = "$digest" ] \
    || die "official v$version installer checksum changed"
  chmod 0700 "$destination"
}

write_fixture_config() {
  local base_image="${P0_V0111_BASE_IMAGE:-subyard-e2e-debian-13-cloud-container}"
  local ssh_port=2291
  [ "$FIXTURE" = post-cas ] && ssh_port=2292
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

power_coordination_binding() {
  sudo -n python3 -B - "$ROOT/scripts/lib/native-install-custody.py" "$POWER_RECONCILER" "$POWER_UNIT" <<'PY'
import importlib.util, os, pathlib, sys
spec = importlib.util.spec_from_file_location('native_custody', sys.argv[1])
native = importlib.util.module_from_spec(spec)
spec.loader.exec_module(native)
held, paths, records = [], [], []
try:
    resources = sorted(map(pathlib.Path, sys.argv[2:]))
    if len(resources) != 2 or len(set(resources)) != 2:
        raise ValueError('invalid power coordination roles')
    for resource in resources:
        native.safe_parent(resource.parent)
        lock = resource.parent / ('.' + resource.name + '.native-install.lock')
        record = resource.parent / ('.' + resource.name + '.native-custody.json')
        fd = os.open(lock, os.O_RDWR | os.O_NOFOLLOW)
        held.append((lock, fd))
        native.validate_lock(lock, fd)
        if os.fstat(fd).st_size != 0:
            raise ValueError('nonempty coordination lock')
        generation = native.observe(record, 'host-power')
        if generation == '0':
            raise ValueError('missing coordination generation')
        records.append((record, generation))
        paths.extend((str(lock), str(record)))
    binding = native.installed_files(paths)
    for lock, fd in held:
        native.validate_lock(lock, fd)
    for record, generation in records:
        if native.observe(record, 'host-power') != generation:
            raise ValueError('coordination generation changed')
    if native.installed_files(paths) != binding:
        raise ValueError('coordination identity changed')
    print(binding)
except (OSError, ValueError, KeyError, TypeError):
    print('release-recovery: unsafe, busy or changed power coordination state', file=sys.stderr)
    sys.exit(1)
finally:
    for _, fd in held:
        os.close(fd)
PY
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
        || die 'historical recovery requires absent default power artifacts and custody'
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

assert_native_resources_absent() {
  local kind path unit target artifact
  for kind in power-helper power-unit sink-helper sink-service sink-timer; do
    case "$kind" in
      power-helper) path="$POWER_RECONCILER" ;;
      power-unit) path="$POWER_UNIT" ;;
      sink-helper) path="$TEST_VMS_SINK" ;;
      sink-service) path="$TEST_VMS_SINK_SERVICE" ;;
      sink-timer) path="$TEST_VMS_SINK_TIMER" ;;
    esac
    if [ -e "$path" ] || [ -L "$path" ]; then
      printf 'release-recovery cleanup resource_kind=%s artifact_kind=installed-resource present=true\n' "$kind" >&2
      die 'successful current uninstall retained an installed fixture resource'
    fi
    case "$kind" in
      sink-*)
        for artifact in native-install.lock native-custody.json; do
          if [ -e "${path%/*}/.${path##*/}.$artifact" ] || [ -L "${path%/*}/.${path##*/}.$artifact" ]; then
            printf 'release-recovery cleanup resource_kind=%s artifact_kind=%s present=true\n' "$kind" "$artifact" >&2
            die 'successful current uninstall retained token-specific sink coordination state'
          fi
        done
        ;;
    esac
    case "$kind" in
      *-helper) continue ;;
    esac
    unit="${path##*/}"
    for target in multi-user.target.wants timers.target.wants sockets.target.wants; do
      if [ -e "${path%/*}/$target/$unit" ] || [ -L "${path%/*}/$target/$unit" ]; then
        printf 'release-recovery cleanup resource_kind=%s artifact_kind=enablement present=true\n' "$kind" >&2
        die 'successful current uninstall retained fixture enablement'
      fi
    done
  done
}

prepare_fixture() { # success|post-cas
  select_fixture "$1"
  require_absent_legacy_power
  [ ! -e "$FIXTURE_ROOT" ] || die "fixture state already exists: $FIXTURE_ROOT"
  install -d -m 0700 "$FIXTURE_ROOT"
  printf '%s\n' "$FIXTURE_MARKER" > "$FIXTURE_ROOT/.subyard-p0-marker"
  chmod 0600 "$FIXTURE_ROOT/.subyard-p0-marker"
  case "$1" in
    success) SUCCESS_ARMED=1 ;;
    post-cas) POST_CAS_ARMED=1 ;;
  esac
  assert_fixture_root
  write_fixture_config
  prepare_operator_user_bus
}

install_official() { # installer version; nonzero allowed by caller
  local installer="$1" version="$2"
  fixture_env env YARD_RELEASE_VERSION="$version" "$installer" --version "$version" --yes
}

check_recovery_update() { # output version
  local output="$1" version="$2" status
  fixture_env "$RUNTIME_ROOT/current/bin/yard" update --check --offline \
    --version "$version" --runtime-root "$RUNTIME_ROOT" > "$output" || {
      status=$?
      recovery_inspection_diagnostics "$output"
      return "$status"
    }
}

assert_source_dead_end() {
  local output="$FIXTURE_ROOT/source-check.json"
  [ "$(yard --version)" = "yard $SOURCE_VERSION" ] \
    || die 'official v0.11.1 is not active'
  [ "$("$RUNTIME_ROOT/previous/bin/yard" --version)" = "yard $OLD_VERSION" ] \
    || die 'official v0.9.1 is not the retained previous release'
  jq -e '.checkpoint == "reconciling" and (.steps | length) > 0 and
    all(.steps[]; .checkpoint == "verified")' "$JOURNAL" >/dev/null \
    || die 'official v0.11.1 did not stop at the verified reconciling checkpoint'
  check_recovery_update "$output" "$SOURCE_VERSION" || return
  jq -s -e 'map(select(type == "object") | (.inspection // .)) |
    map(select(has("blockers"))) as $reports |
    ($reports | length) == 1 and ($reports[0].blockers | length) == 1 and
    $reports[0].blockers[0].resource == "transition.observation-scope"' \
    "$output" >/dev/null \
    || {
      recovery_inspection_diagnostics "$output"
      die 'official v0.11.1 scope blocker is not repeatable'
    }
}

reproduce_official_dead_end() { # success|post-cas
  local source_rc
  prepare_fixture "$1"
  info "creating independent official v0.9.1 -> v0.11.1 $FIXTURE history"
  install_official "$OLD_INSTALLER" "$OLD_VERSION" >/dev/null
  incus project create "$PROJECT" \
    -c features.images=false -c user.subyard.p0="$FIXTURE_MARKER" >/dev/null
  assert_project_marker
  yard -Y "$YARD_NAME" init --yes >/dev/null
  assert_project_marker
  yard -Y "$YARD_NAME" start --yes >/dev/null
  yard -Y "$YARD_NAME" check >/dev/null
  sed -i 's/^AGENTS=$/AGENTS=codex/' "$CONFIG_HOME/yards/$YARD_NAME/config.env"
  printf 'HOST_CODEX_AGENTS_MD=%s\n' "$HOME_ROOT/host/AGENTS.md" \
    >> "$CONFIG_HOME/yards/$YARD_NAME/config.env"
  set +e
  install_official "$SOURCE_INSTALLER" "$SOURCE_VERSION" \
    > "$FIXTURE_ROOT/source-install.log" 2>&1
  source_rc=$?
  set -e
  printf 'release-recovery fixture=%s stage=source-install native_status=%s\n' "$FIXTURE" "$source_rc"
  [ "$source_rc" -ne 0 ] || die 'official v0.11.1 unexpectedly escaped the known dead-end'
  assert_source_dead_end
  cp "$LEDGER" "$FIXTURE_ROOT/source-ledger.json"
}

prepare_recovery_bridge() {
  local source_root digest bridge_registry
  source_root="$RUNTIME_ROOT/$(readlink "$RUNTIME_ROOT/current")"
  (cd "$source_root" && sha256sum -c runtime-files.sha256 >/dev/null 2>&1) \
    || die 'official recovery source runtime manifest changed'
  digest="$(sha256sum "$source_root/config/release-transition.json" | awk '{print $1}')"
  jq -e --arg digest "$digest" '.registryDigest == $digest' "$JOURNAL" >/dev/null \
    || die 'official source registry does not match its protected journal'
  # The authenticated published patch owns this closed historical recovery.
  # Current installers and metadata writers require later migration epochs.
  release_candidate_historical "$ROOT" "$STATE_ROOT/historical-cache" \
    "$CANDIDATE_VERSION" "$RELEASE_ROOT"
  bridge_registry="$(tar -xOzf "$CANDIDATE_BUNDLE" ./config/release-transition.json | sha256sum | awk '{print $1}')"
  [ "$bridge_registry" = "$digest" ] || die 'published bridge registry differs from the exact recovery source'
  cp "$CURRENT_RELEASE_ROOT/subyard-install.sh" \
    "$CURRENT_RELEASE_ROOT/subyard-install-runtime-release.sh" \
    "$CURRENT_RELEASE_ROOT/subyard-install-runtime-release.sh.sha256" "$RELEASE_ROOT/"
  chmod -R a+rX "$RELEASE_ROOT"
  printf 'release-recovery artifact role=historical-bridge engine_source=official-published version=%s runtime_sha256=%s registry_sha256=%s\n' \
    "$CANDIDATE_VERSION" "$(sha256sum "$CANDIDATE_BUNDLE" | awk '{print $1}')" "$digest"
}

prepare_current_candidate() {
  local runtime candidate_status runtime_file
  # shellcheck source=tests/helpers/release-candidate.sh
  . "$ROOT/tests/helpers/release-candidate.sh"
  if runtime="$(release_candidate_prepare "$ROOT")"; then
    CURRENT_RELEASE_ROOT="$ROOT/.subyard-acceptance/release"
    CURRENT_VERSION="$(jq -er '.version' "$ROOT/.subyard-acceptance/candidate.json")"
    CURRENT_RUNTIME_SHA256="$(jq -er '.runtime_sha256' "$ROOT/.subyard-acceptance/candidate.json")"
    CURRENT_MANIFEST_SHA256="$(sha256sum "$(dirname "$runtime")/../runtime-files.sha256" | awk '{print $1}')"
  else
    candidate_status=$?
    [ "$candidate_status" = 1 ] || die 'frozen current candidate validation failed'
    CURRENT_RELEASE_ROOT="$STATE_ROOT/current-release"
    CURRENT_VERSION=1.0.0-candidate.e2e
    "$ROOT/dev/package-engine.sh" --output-dir "$CURRENT_RELEASE_ROOT" \
      --version "$CURRENT_VERSION" >/dev/null
    runtime_file="$CURRENT_RELEASE_ROOT/subyard-$CURRENT_VERSION-linux-amd64.tar.gz"
    CURRENT_RUNTIME_SHA256="$(sha256sum "$runtime_file" | awk '{print $1}')"
    CURRENT_MANIFEST_SHA256="$(tar -xOzf "$runtime_file" ./runtime-files.sha256 | sha256sum | awk '{print $1}')"
  fi
}

prepare_current_fixture_inputs() {
  local source_root expected actual
  assert_fixture_root
  [ "$("$RUNTIME_ROOT/current/bin/yard" --version)" = "yard $CANDIDATE_VERSION" ] \
    && jq -e '.checkpoint == "complete"' "$JOURNAL" >/dev/null \
    || die 'current fixture selection requires the completed historical bridge'
  source_root="$RUNTIME_ROOT/$(readlink "$RUNTIME_ROOT/current")"
  (cd "$source_root" && sha256sum -c runtime-files.sha256 >/dev/null 2>&1) \
    || die 'historical bridge manifest changed before current selection'
  expected="$(sha256sum "$source_root/config/agents/codex/rules/repo.rules" | awk '{print $1}')"
  actual="$(incus exec "$INSTANCE" --project "$PROJECT" -- \
    sha256sum /home/dev/.codex/rules/repo.rules | awk '{print $1}')"
  [ "$actual" = "$expected" ] || die 'historical rules differ from the authenticated bridge'
  require_absent_historical_config
  # Fresh settings after the completed bridge keep exact initial adoption honest.
  install -m 0600 "$source_root/config/agents/codex/rules/repo.rules" "$HOME_ROOT/host/repo.rules"
  printf '%s\n' "AGENT_codex_RULES=$HOME_ROOT/host/repo.rules" \
    >> "$CONFIG_HOME/yards/$YARD_NAME/config.env"
  printf 'release-recovery fixture=%s stage=current-selection rules_match_authenticated_bridge=true config_artifact=plain structured_toml_migration=false\n' "$FIXTURE"
}

require_absent_historical_config() {
  incus exec "$INSTANCE" --project "$PROJECT" -- sh -c \
    'test ! -e "$1" && test ! -L "$1"' _ /home/dev/.codex/config.toml \
    || die 'unexpected historical structured config; selected plain artifact was required'
}

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

upgrade_to_current_candidate() {
  local output active manifest status power_coordination
  prepare_current_fixture_inputs
  printf 'release-recovery artifact role=current-candidate version=%s runtime_sha256=%s\n' \
    "$CURRENT_VERSION" "$CURRENT_RUNTIME_SHA256"
  fixture_env env YARD_RELEASE_BASE_URL="file://$CURRENT_RELEASE_ROOT" \
    YARD_RELEASE_VERSION="$CURRENT_VERSION" \
    bash "$CURRENT_RELEASE_ROOT/subyard-install.sh" --version "$CURRENT_VERSION" --yes \
    > "$FIXTURE_ROOT/current-upgrade.log"
  [ "$("$RUNTIME_ROOT/current/bin/yard" --version)" = "yard $CURRENT_VERSION" ] \
    || die 'true current candidate is not active after the historical bridge'
  manifest="$(sha256sum "$RUNTIME_ROOT/current/runtime-files.sha256" | awk '{print $1}')"
  [ "$manifest" = "$CURRENT_MANIFEST_SHA256" ] \
    || die 'installed current runtime differs from the unmodified candidate'
  require_absent_historical_config
  printf 'release-recovery fixture=%s stage=current-upgrade historical_structured_config_absent=true\n' "$FIXTURE"
  jq -e --slurpfile original "$FIXTURE_ROOT/source-ledger.json" '
    .domains as $current | all($original[0].domains | to_entries[];
      . as $old | $current[$old.key].epoch >= $old.value.epoch and
      (($old.value.applied - $current[$old.key].applied) | length) == 0)' \
    "$LEDGER" >/dev/null || die 'current upgrade lost already-applied migration history'
  active="$(readlink "$RUNTIME_ROOT/current")"
  output="$FIXTURE_ROOT/current-repeat-check.json"
  check_recovery_update "$output" "$CURRENT_VERSION" || return
  assert_ready_check "$output" "${active#releases/}"
  assert_materialized_fixed_point
  yard -Y "$YARD_NAME" status >/dev/null
  ok 'unmodified current candidate upgraded the historical bridge and reached ready'
  prepare_operator_user_bus verify
  assert_fixture_root
  assert_project_marker
  [ "$(incus config get "$INSTANCE" user.subyard.managed --project "$PROJECT")" = true ] \
    && [ "$(incus config get "$INSTANCE" user.subyard.name --project "$PROJECT")" = "$YARD_NAME" ] \
    || die 'current cleanup requires the exact fixture instance'
  CURRENT_UNINSTALL_REFUSED="$FIXTURE"
  power_coordination="$(power_coordination_binding)" \
    || die 'current power coordination state could not be bound before cleanup'
  if fixture_env "$RUNTIME_ROOT/current/bin/yard" -Y "$YARD_NAME" teardown --yes \
    > "$FIXTURE_ROOT/current-yard-teardown.log" 2>&1; then
    status=0
  else
    status=$?
  fi
  printf 'release-recovery cleanup fixture=%s stage=current-yard-teardown native_status=%s\n' "$FIXTURE" "$status"
  if [ "$status" != 0 ]; then
    cleanup_failure_diagnostics "$FIXTURE_ROOT/current-yard-teardown.log"
    return "$status"
  fi
  incus info >/dev/null || die 'Incus is unavailable after selected yard teardown'
  ! incus project show "$PROJECT" >/dev/null 2>&1 \
    && ! incus info "$INSTANCE" --project "$PROJECT" >/dev/null 2>&1 \
    || die 'selected current teardown retained the fixture project or instance'
  if fixture_env "$RUNTIME_ROOT/current/bin/yard" teardown --full --yes \
    > "$FIXTURE_ROOT/current-uninstall.log" 2>&1; then
    status=0
  else
    status=$?
  fi
  printf 'release-recovery cleanup fixture=%s stage=current-full-uninstall native_status=%s\n' "$FIXTURE" "$status"
  if [ "$status" != 0 ]; then
    cleanup_failure_diagnostics "$FIXTURE_ROOT/current-uninstall.log"
    return "$status"
  fi
  assert_native_resources_absent
  [ "$(power_coordination_binding)" = "$power_coordination" ] \
    || die 'public cleanup changed the bound power coordination state'
  POWER_COORDINATION_BINDING="$power_coordination"
  require_absent_legacy_power
  printf 'release-recovery fixture=%s stage=current-cleanup resources_absent=true coordination_unchanged=true\n' "$FIXTURE"
  CURRENT_UNINSTALL_REFUSED=
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

recovery_inspection_diagnostics() { # bounded public facts; never print plans or raw documents
  local output="$1" summary
  if [ "$output" = "$FIXTURE_ROOT/source-check.json" ]; then
    python3 - "$FIXTURE_ROOT/source-install.log" <<'PY'
import json, pathlib, re, sys
with pathlib.Path(sys.argv[1]).open('rb') as source:
    source.seek(0, 2)
    size = source.tell()
    source.seek(max(0, size - 65536))
    text = source.read(65536).decode(errors='replace')
ids = 'materialized-config|host-power|test-vm-broker|test-yard-route-consumers'
stages = [dict(component=m[0], stage=m[1]) for m in re.findall(
    rf'activation reconciler "({ids})" (?:failed during |reached an unknown state during )?(prepare|verify|commit|reconcile)', text)]
phrases = {
    'scope-stale': 'the authorized transition observation scope differs from this engine',
    'desired-stale': 'desired configuration or target changed after confirmation',
    'materialized-stale': 'materialized configuration changed after confirmation',
    'selector-stale': 'selected local yard set changed after confirmation',
    'config-apply-failed': 'materialized config reconcile returned status',
    'verification-failed': 'did not reach its fixed point during verification',
    'aggregate-verification-failed': 'did not retain its fixed point during aggregate verification',
}

codes = ['plan-stale', 'dependency-unavailable', 'recovery-ambiguous', 'activation-ambiguous', 'recovery-pending']
print('release-recovery source-install ' + json.dumps({
    'bounded_bytes': 65536, 'truncated': size > 65536, 'activation_stages': stages[-8:],
    'known_failures': [code for code, phrase in phrases.items() if phrase in text],
    'known_codes': [code for code in codes if code in text]}, separators=(',', ':')))
PY
  fi
  if [ ! -f "$output" ] || [ "$(stat -Lc %s "$output")" -gt 65536 ] \
    || [ ! -f "$JOURNAL" ] || [ "$(stat -Lc %s "$JOURNAL")" -gt 65536 ]; then
    printf 'release-recovery inspection parse_valid=unavailable size_limit=65536\n' >&2
    return
  fi
  if summary="$(jq -cs --slurpfile source "$JOURNAL" '
    def enum($values): . as $value | if $values | index($value) then $value else "other" end;
    def code: enum(["ready", "transition-required", "recovery-pending", "registry-invalid",
      "unsupported-epoch", "unsupported-kind", "confirmation-required", "operation-declined",
      "plan-stale", "migration-stale", "resource-conflict", "precondition-blocked",
      "dependency-unavailable", "verification-failed", "activation-ambiguous",
      "recovery-ambiguous", "journal-invalid", "rollback-incompatible", "rollback-expired"]);
    map(select(type == "object") | (.inspection // .) | select(type == "object") |
      select(has("plan") or has("assessment") or has("blockers") or has("outcome"))) as $reports |
    $source[0] as $journal |
    {parse_valid: true, report_count: ($reports | length), reports: [$reports[:4][] |
      (.outcome | if type == "object" then . else {} end) as $outcome |
      {changed: (.assessment | if type == "object" then .changed else null end |
          if type == "boolean" then . else null end),
       plan_present: has("plan"),
       plan_format_valid: (.plan | if type == "string" then test("^plan-v1-[a-f0-9]{64}$") else false end),
       resume_matches_source_transaction: (.resume != null and .resume == $journal.transaction),
       plan_matches_source_resume: (.plan != null and .plan == $journal.resumePlan),
       status: ($outcome.status | enum(["ready", "migration-required", "recovering", "operator-action-required"])),
       outcome_code: ($outcome.code | code),
       blockers: (.blockers | if type == "array" then . else [] end |
         map(select(type == "object")) | .[:8] | map({code: (.code | code),
           resource_kind: (.resource | if . as $resource |
               ["transition.observation-scope", "transition.active", "transition.bindings", "transition.links",
                "transition.rollback-compatibility", "transition.source-checkpoint-compatibility"] | index($resource) then .
             elif type == "string" then split(".")[0] | enum(["transition", "migration", "activation", "runtime", "yard"])
             else "other" end)}))}]}' "$output" 2>/dev/null)"; then
    printf 'release-recovery inspection %s\n' "$summary" >&2
  else
    printf 'release-recovery inspection parse_valid=false\n' >&2
  fi
  if summary="$(jq -c '{reconciling: (.checkpoint == "reconciling"),
      activate_target: (.goal.direction == "activate-target"),
      source_ingress_present: (.sourceIngress != null),
      resume_plan_present: (.resumePlan != null)}' "$JOURNAL" 2>/dev/null)"; then
    printf 'release-recovery source-journal %s\n' "$summary" >&2
  else
    printf 'release-recovery source-journal parse_valid=false\n' >&2
  fi
}

prepare_recovery_logs() { # <descriptor-variable> <fixed-log-name>
  local destination="$1" name="$2" directory_fd log_fd rc=0 restore_clobber=0
  case "$name" in resume.log|resume.stderr|first-process.log) ;; *) return 2 ;; esac
  exec {directory_fd}< "$FIXTURE_ROOT/." || return
  python3 - "$STATE_ROOT" "$MARKER" "$FIXTURE_ROOT" "$FIXTURE_MARKER" "$BASHPID" "$directory_fd" <<'PY' || rc=$?
import os, stat, sys
assert os.path.dirname(sys.argv[3]) == sys.argv[1]
for path, marker in ((sys.argv[1], sys.argv[2]), (sys.argv[3], sys.argv[4])):
    fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        info = os.fstat(fd)
        info_root = info
        assert info.st_uid == os.getuid() and stat.S_IMODE(info.st_mode) == 0o700
        with os.fdopen(os.open('.subyard-p0-marker', os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=fd), 'rb') as stream:
            info = os.fstat(stream.fileno())
            assert stat.S_ISREG(info.st_mode) and info.st_uid == os.getuid()
            assert stat.S_IMODE(info.st_mode) == 0o600 and info.st_nlink == 1
            assert stream.read(256) == (marker + '\n').encode()
        if path == sys.argv[3]:
            held = os.stat('/proc/' + sys.argv[5] + '/fd/' + sys.argv[6])
            assert (held.st_dev, held.st_ino) == (info_root.st_dev, info_root.st_ino)
    finally:
        os.close(fd)
PY
  if [ "$rc" = 0 ]; then
    [ ! -e "/proc/self/fd/$directory_fd/$name" ] && [ ! -L "/proc/self/fd/$directory_fd/$name" ] || rc=2
  fi
  if [ "$rc" = 0 ]; then
    case "$-" in *C*) ;; *) set -C; restore_clobber=1 ;; esac
    exec {log_fd}> "/proc/self/fd/$directory_fd/$name" || rc=$?
    [ "$restore_clobber" = 0 ] || set +C
  fi
  exec {directory_fd}<&-
  [ "$rc" = 0 ] || return "$rc"
  python3 - "$BASHPID" "$log_fd" <<'PY' || rc=$?
import os, stat, sys
with open('/proc/' + sys.argv[1] + '/fd/' + sys.argv[2], 'rb') as stream:
    info = os.fstat(stream.fileno())
    assert stat.S_ISREG(info.st_mode) and info.st_uid == os.getuid() and info.st_nlink == 1
    os.fchmod(stream.fileno(), 0o600)
    assert stat.S_IMODE(os.fstat(stream.fileno()).st_mode) == 0o600
PY
  if [ "$rc" != 0 ]; then exec {log_fd}>&-; return "$rc"; fi
  printf -v "$destination" '%s' "$log_fd"
}

recovery_log_artifacts() { # replay|export, then held stdout/stderr/first-process descriptors
  python3 - "$STATE_ROOT" "$MARKER" "$FIXTURE_ROOT" "$FIXTURE_MARKER" "$BASHPID" "$@" <<'PY'
import base64, hashlib, json, os, stat, sys

def private_root(path, marker):
    fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        info = os.fstat(fd)
        assert info.st_uid == os.getuid() and stat.S_IMODE(info.st_mode) == 0o700
        with os.fdopen(os.open('.subyard-p0-marker', os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=fd), 'rb') as stream:
            info = os.fstat(stream.fileno())
            assert stat.S_ISREG(info.st_mode) and info.st_uid == os.getuid()
            assert stat.S_IMODE(info.st_mode) == 0o600 and info.st_nlink == 1
            assert stream.read(256) == (marker + '\n').encode()
        return fd
    except BaseException:
        os.close(fd)
        raise

failed = False
if len(sys.argv) != 10 or not all(value.isdecimal() for value in sys.argv[5:6] + sys.argv[7:10]):
    print('RELEASE_RECOVERY_CAPTURE {"state":"refused"}', flush=True)
    sys.exit(23)
action = sys.argv[6]
assert action in ('replay', 'export')
held = dict(zip(('resume-stdout', 'resume-stderr', 'first-process'), sys.argv[7:10]))
try:
    os.close(private_root(sys.argv[1], sys.argv[2]))
    root = private_root(sys.argv[3], sys.argv[4])
except (OSError, AssertionError):
    print('RELEASE_RECOVERY_CAPTURE {"state":"refused"}', flush=True)
    sys.exit(23)
try:
    for name, relative, limit in (
        ('resume-stdout', 'resume.log', 65536), ('resume-stderr', 'resume.stderr', 16384),
        ('journal', 'config/release-transition/v2/journal.json', 65536),
        ('cas-marker', 'post-cas-link-observed.json', 4096),
        ('first-process', 'first-process.log', 65536),
    ):
        if action == 'replay' and name != 'resume-stderr':
            continue
        opened = []
        try:
            parent = root
            parts = relative.split('/')
            for part in parts[:-1]:
                parent = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=parent)
                opened.append(parent)
                assert os.fstat(parent).st_uid == os.getuid()
            descriptor = ('/proc/' + sys.argv[5] + '/fd/' + held[name]) if name in held else None
            file_fd = os.open(descriptor, os.O_RDONLY | os.O_NONBLOCK) if descriptor else os.open(
                parts[-1], os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=parent)
            with os.fdopen(file_fd, 'rb') as stream:
                before = os.fstat(stream.fileno())
                assert stat.S_ISREG(before.st_mode) and before.st_uid == os.getuid()
                assert stat.S_IMODE(before.st_mode) == 0o600 and before.st_nlink == 1
                current = os.stat(parts[-1], dir_fd=parent, follow_symlinks=False)
                assert (before.st_dev, before.st_ino, before.st_mode) == (current.st_dev, current.st_ino, current.st_mode)
                if action == 'replay':
                    remaining = before.st_size
                    while remaining:
                        data = stream.read(min(65536, remaining))
                        assert data
                        sys.stderr.buffer.write(data)
                        remaining -= len(data)
                    continue
                data = stream.read(limit)
                after = os.fstat(stream.fileno())
                assert all(getattr(before, key) == getattr(after, key) for key in
                           ('st_dev', 'st_ino', 'st_mode', 'st_uid', 'st_nlink', 'st_size', 'st_mtime_ns', 'st_ctime_ns'))
            encoded = base64.b64encode(data).decode('ascii')
            print('RELEASE_RECOVERY_ARTIFACT ' + json.dumps(dict(name=name, state='retained',
                source_bytes=before.st_size, retained_bytes=len(data), truncated=len(data) < before.st_size,
                sha256=hashlib.sha256(data).hexdigest(), chunks=(len(encoded) + 2047) // 2048)), flush=True)
            for index, offset in enumerate(range(0, len(encoded), 2048)):
                print('RELEASE_RECOVERY_DATA ' + json.dumps(dict(name=name, index=index,
                    base64=encoded[offset:offset + 2048])), flush=True)
        except (OSError, AssertionError) as error:
            failed = True
            state = 'unavailable' if isinstance(error, FileNotFoundError) else 'refused'
            prefix = 'RELEASE_RECOVERY_CAPTURE ' if action == 'replay' else 'RELEASE_RECOVERY_ARTIFACT '
            print(prefix + json.dumps(dict(name=name, state=state)), flush=True)
        finally:
            for fd in reversed(opened):
                os.close(fd)
finally:
    os.close(root)
sys.exit(23 if failed else 0)
PY
}

capture_post_cas_resume_failure() {
  recovery_log_artifacts export "$@"
}

resume_post_cas_candidate() {
  local output="$1" first_fd="$2" output_fd errors_fd rc=0 capture_rc=0 replay_rc=0
  [ "$output" = "$FIXTURE_ROOT/resume.log" ] || return 2
  prepare_recovery_logs output_fd resume.log || return
  prepare_recovery_logs errors_fd resume.stderr || rc=$?
  if [ "$rc" != 0 ]; then exec {output_fd}>&-; return "$rc"; fi
  fixture_env "$RUNTIME_ROOT/current/bin/yard" update --offline \
    --version "$CANDIDATE_VERSION" --runtime-root "$RUNTIME_ROOT" </dev/null \
    1>&"$output_fd" 2>&"$errors_fd" || rc=$?
  recovery_log_artifacts replay "$output_fd" "$errors_fd" "$first_fd" || replay_rc=$?
  if [ "$rc" != 0 ]; then
    recovery_inspection_diagnostics "/proc/$BASHPID/fd/$output_fd" || true
    cleanup_failure_diagnostics "/proc/$BASHPID/fd/$output_fd" || true
    capture_post_cas_resume_failure "$output_fd" "$errors_fd" "$first_fd" || capture_rc=$?
    printf 'release-recovery resume native_status=%s capture_status=%s replay_status=%s\n' "$rc" "$capture_rc" "$replay_rc" >&2
  elif [ "$replay_rc" != 0 ]; then
    printf 'release-recovery resume native_status=0 replay_status=%s\n' "$replay_rc" >&2
  fi
  exec {output_fd}>&- {errors_fd}>&-
  return "$rc"
}
capture_recovery_plan() {
  local candidate_path output before after plan inspection_status
  assert_fixture_root
  candidate_path="$(fixture_env "$RELEASE_ROOT/subyard-install-runtime-release.sh" \
    --runtime-root "$RUNTIME_ROOT" --publish-only \
    --bundle "$CANDIDATE_BUNDLE" --checksum "$CANDIDATE_BUNDLE.sha256" \
    --manifest "$CANDIDATE_BUNDLE.manifest.json" \
    --provenance "$CANDIDATE_BUNDLE.provenance.json")"
  case "$candidate_path" in
    releases/0.11.2-*) ;;
    *) die "publish-only returned an invalid candidate identity: $candidate_path" ;;
  esac
  CANDIDATE_RELEASE="${candidate_path#releases/}"
  output="$FIXTURE_ROOT/recovery-check.json"
  before="$(sha256sum "$JOURNAL" | awk '{print $1}')"
  fixture_env "$RUNTIME_ROOT/$candidate_path/bin/yard" update --check --offline \
    --version "$CANDIDATE_VERSION" --runtime-root "$RUNTIME_ROOT" > "$output" || {
      inspection_status=$?
      recovery_inspection_diagnostics "$output"
      return "$inspection_status"
    }
  after="$(sha256sum "$JOURNAL" | awk '{print $1}')"
  [ "$before" = "$after" ] || {
    recovery_inspection_diagnostics "$output"
    die 'candidate recovery inspection changed the source journal'
  }
  plan="$(jq -sr 'map(select(type == "object") | (.inspection // .)) |
    map(select(has("plan") and .assessment.changed == true and
      ((.blockers // []) | length) == 0)) |
    select(length == 1) | .[0].plan' "$output" 2>/dev/null)" || {
      inspection_status=$?
      recovery_inspection_diagnostics "$output"
      return "$inspection_status"
    }
  case "$plan" in
    plan-v1-*) ;;
    *)
      recovery_inspection_diagnostics "$output"
      die 'candidate recovery inspection returned no exact Plan'
      ;;
  esac
  printf '%s\n' "$plan" > "$FIXTURE_ROOT/recovery-plan"
}

assert_ready_check() { # json-stream expected-active
  local output="$1" expected="$2"
  jq -s -e --arg active "$expected" '
    map(select(type == "object") | (.inspection // .)) |
    map(select(has("outcome") and has("assessment"))) as $reports |
    ($reports | length) == 1 and
    $reports[0].outcome.status == "ready" and
    $reports[0].outcome.reachedGoal == true and
    $reports[0].outcome.active == $active and
    $reports[0].outcome.target == $active and
    $reports[0].assessment.changed == false and
    (($reports[0].blockers // []) | length) == 0' "$output" >/dev/null \
    || {
      recovery_inspection_diagnostics "$output"
      die 'repeat update check did not report the structured ready fixed point'
    }
}

assert_materialized_fixed_point() {
  local expected actual status stage="${1:-current}"
  expected="$(printf '# %s\nThe v0.11.1 %s fixture owns this file.\n' \
    "$FIXTURE_MARKER" "$FIXTURE" | sha256sum | awk '{print $1}')"
  actual="$(sha256sum "$HOME_ROOT/host/AGENTS.md" | awk '{print $1}')"
  [ "$actual" = "$expected" ] \
    || die 'candidate recovery changed the configured host instructions'
  # Independent byte oracle: selected instructions preserve the configured text
  # and append this documented managed block, not the raw host digest.
  if [ "$stage" = current ]; then
    expected="$({
    cat "$HOME_ROOT/host/AGENTS.md"
    printf '\n\n'
    cat <<'PREVIEW'
<!-- subyard-preview -->
For a static web preview, run `subyard-preview <relative-static-dir>` from the Git workspace and keep it running with your background/async process mechanism. Share the printed URL. Owner URLs require network reachability (and Tailnet policy access for Tailscale). A loopback URL is local to the yard; provide the operator-machine tunnel recipe from `subyard-preview --help` only when the operator requests it. The helper must stay running.
<!-- /subyard-preview -->
PREVIEW
    } | sha256sum | awk '{print $1}')"
  fi
  actual="$(incus exec "$INSTANCE" --project "$PROJECT" -- \
    sha256sum /home/dev/.codex/AGENTS.md | awk '{print $1}')" \
    || die 'candidate recovery Codex target is unavailable'
  [ "$actual" = "$expected" ] \
    || die 'candidate recovery did not materialize the configured Codex target'
  [ "$stage" = current ] || return 0
  status="$(fixture_env "$RUNTIME_ROOT/current/bin/yard" -Y "$YARD_NAME" \
    integration status codex --json)" \
    || die 'candidate recovery integration status is unavailable'
  jq -e '.observed == "ready" and (.selection.effective | index("codex") != null)' \
    <<<"$status" >/dev/null \
    || die 'candidate recovery Codex integration is not at its native fixed point'
}

assert_terminal() { # expected-ledger recovery-plan
  local expected_ledger="$1" plan_file="$2" current candidate transaction archive
  local source_transaction archive_count
  current="$(readlink "$RUNTIME_ROOT/current")"
  candidate="${current#releases/}"
  [ "$("$RUNTIME_ROOT/current/bin/yard" --version)" = "yard $CANDIDATE_VERSION" ] \
    || die 'recovery candidate is not active'
  [ "$("$RUNTIME_ROOT/previous/bin/yard" --version)" = "yard $SOURCE_VERSION" ] \
    || die 'v0.11.1 is not the retained recovery predecessor'
  transaction="$(jq -er '.transaction' "$JOURNAL")"
  archive="$CONFIG_HOME/release-transition/v2/transactions/$transaction/superseded-journal.json"
  jq -e --rawfile plan "$plan_file" '
    ($plan | rtrimstr("\n")) as $plan |
    .checkpoint == "complete" and (.steps | length) == 0 and
    .authorizationPlan == $plan' "$JOURNAL" >/dev/null \
    || die 'candidate journal is not terminal or bound to the inspected Plan'
  jq -e --rawfile plan "$plan_file" '
    ($plan | rtrimstr("\n")) as $plan |
    .journal.checkpoint == "reconciling" and
    .replacement.reason == "post-activation-scope-v0.11.1" and
    .authorizationPlan == $plan' "$archive" >/dev/null \
    || die 'source journal archive is absent or not bound to the inspected Plan'
  source_transaction="$(jq -er '.journal.transaction' "$archive")"
  [ -d "$CONFIG_HOME/release-transition/v2/transactions/$source_transaction/evidence" ] \
    || die 'source transaction evidence is not retained'
  archive_count="$(find "$CONFIG_HOME/release-transition/v2/transactions" \
    -type f -name superseded-journal.json | wc -l)"
  [ "$archive_count" -eq 1 ] \
    || die 'recovery did not create exactly one replacement transaction archive'
  cmp -s "$LEDGER" "$expected_ledger" \
    || die 'candidate recovery changed the verified migration ledger'
  case "$current" in releases/0.11.2-*) ;; *) die "unexpected candidate link $current" ;; esac
  [ "$candidate" != "$(readlink "$RUNTIME_ROOT/previous" | sed 's|^releases/||')" ] \
    || die 'candidate and previous release identities are equal'
}

run_candidate_recovery() { # success
  local output active
  select_fixture "$1"
  capture_recovery_plan
  info 'recovering the official v0.11.1 dead-end with the local packaged candidate'
  fixture_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" \
    YARD_RELEASE_VERSION="$CANDIDATE_VERSION" \
    "$RELEASE_ROOT/subyard-install.sh" --yes > "$FIXTURE_ROOT/candidate-recovery.log"
  assert_terminal "$FIXTURE_ROOT/source-ledger.json" "$FIXTURE_ROOT/recovery-plan"
  output="$FIXTURE_ROOT/repeat-check.json"
  check_recovery_update "$output" "$CANDIDATE_VERSION" || return
  active="$(readlink "$RUNTIME_ROOT/current")"
  assert_ready_check "$output" "${active#releases/}"
  assert_materialized_fixed_point historical
  yard -Y "$YARD_NAME" status >/dev/null
  ok 'local patch recovered the exact published v0.11.1 dead-end'
  upgrade_to_current_candidate
}

run_post_cas_resume() { # post-cas
  local transition_pid transition_rc=0 output active source_transaction marker_metadata first_process_fd
  select_fixture "$1"
  capture_recovery_plan
  source_transaction="$(jq -er '.transaction' "$JOURNAL")"
  POST_CAS_OBSERVATION_MARKER="$FIXTURE_ROOT/post-cas-link-observed.json"
  [ -f "$POST_CAS_OBSERVER" ] && [ ! -L "$POST_CAS_OBSERVER" ] \
    || die 'post-CAS link observer is unavailable'
  sh -c 'test ! -e "$1" && test ! -L "$1"' _ "$POST_CAS_OBSERVATION_MARKER" \
    || die 'refusing an existing post-CAS observation marker'
  output="$FIXTURE_ROOT/first-process.log"
  prepare_recovery_logs first_process_fd first-process.log
  setsid env \
    HOME="$HOME_ROOT" USER="$(id -un)" LOGNAME="$(id -un)" SHELL=/bin/bash \
    PATH="$BIN_ROOT:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" \
    SUBYARD_OPERATOR_HOME="$HOME_ROOT" SUBYARD_HOME="$DATA_ROOT" \
    SUBYARD_CONFIG_HOME="$CONFIG_HOME" SUBYARD_YARD="$YARD_NAME" \
    YARD_RUNTIME_ROOT="$RUNTIME_ROOT" YARD_RELEASE_CACHE="$CACHE_ROOT" \
    YARD_BIN_DIR="$BIN_ROOT" YARD_SHELL_RC="$HOME_ROOT/.bashrc" \
    YARD_LOGIN_RC="$HOME_ROOT/.profile" YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" \
    YARD_RELEASE_VERSION="$CANDIDATE_VERSION" \
    SUBYARD_POWER_LIBEXEC_DIR=/usr/local/libexec/subyard \
    SUBYARD_POWER_RECONCILER_PATH="$POWER_RECONCILER" \
    SUBYARD_POWER_UNIT_PATH="$POWER_UNIT" \
    python3 "$POST_CAS_OBSERVER" \
      --runtime-root "$RUNTIME_ROOT" --journal "$JOURNAL" \
      --source-transaction "$source_transaction" \
      --candidate-target "$CANDIDATE_RELEASE" \
      --marker "$POST_CAS_OBSERVATION_MARKER" --timeout 120 -- \
      "$RELEASE_ROOT/subyard-install.sh" --yes 1>&"$first_process_fd" 2>&1 &
  transition_pid=$!
  POST_CAS_TRANSITION_PID="$transition_pid"
  if wait "$POST_CAS_TRANSITION_PID"; then
    transition_rc=0
  else
    transition_rc=$?
  fi
  fence_reaped_post_cas_transition "$transition_rc" \
    || die 'post-CAS transition process group did not stop after observer exit'
  [ "$transition_rc" = 137 ] \
    || die "post-CAS transition exited with $transition_rc instead of SIGKILL status 137"
  [ -f "$POST_CAS_OBSERVATION_MARKER" ] \
    && [ ! -L "$POST_CAS_OBSERVATION_MARKER" ] \
    || die 'post-CAS observer did not publish a regular observation marker'
  marker_metadata="$(stat -c '%u:%g:%a' -- "$POST_CAS_OBSERVATION_MARKER")"
  [ "$marker_metadata" = "$(id -u):$(id -g):600" ] \
    || die 'post-CAS observation marker is not an operator-owned regular file'
  jq -e --arg source "$source_transaction" --arg active "releases/$CANDIDATE_RELEASE" \
    '.transaction != $source and
      (.checkpoint == "activation-intent" or .checkpoint == "target-active" or
        .checkpoint == "reconciling") and .active == $active' \
    "$POST_CAS_OBSERVATION_MARKER" >/dev/null \
    || die 'post-CAS observer did not bind the journal CAS to candidate activation'
  jq -e --arg source "$source_transaction" \
    '.transaction != $source and .checkpoint != "complete"' "$JOURNAL" >/dev/null \
    || die 'post-CAS interruption did not retain resumable candidate state'
  output="$FIXTURE_ROOT/resume.log"
  resume_post_cas_candidate "$output" "$first_process_fd"
  exec {first_process_fd}>&-
  ! grep -Eiq 'confirm|proceed' "$output" \
    || die 'ordinary post-CAS resume requested a second confirmation'
  assert_terminal "$FIXTURE_ROOT/source-ledger.json" "$FIXTURE_ROOT/recovery-plan"
  output="$FIXTURE_ROOT/repeat-check.json"
  check_recovery_update "$output" "$CANDIDATE_VERSION" || return
  active="$(readlink "$RUNTIME_ROOT/current")"
  assert_ready_check "$output" "${active#releases/}"
  assert_materialized_fixed_point historical
  yard -Y "$YARD_NAME" status >/dev/null
  ok 'ordinary active-candidate update resumed after the exact post-CAS interruption'
  upgrade_to_current_candidate
}

main() {
  trap cleanup EXIT
  trap 'handle_signal 130' INT
  trap 'handle_signal 143' TERM
  valid_token "$TOKEN" || die 'numeric P0 allocation token is required'
  [ -n "${SUBYARD_E2E_VM:-}" ] || die 'run only inside an allocated P0 guest'
  [ "$SUBYARD_E2E_VM" = 1 ] || die 'v0.11.1 release recovery is pinned to the P0 owner VM'
  for dependency in curl go incus jq ps python3 setsid sha256sum sudo systemctl tar; do
    command -v "$dependency" >/dev/null 2>&1 || die "$dependency is required"
  done
  [ ! -e "$STATE_ROOT" ] || die "fixture state already exists: $STATE_ROOT"
  install -d -m 0700 "$STATE_ROOT"
  printf '%s\n' "$MARKER" > "$STATE_ROOT/.subyard-p0-marker"
  chmod 0600 "$STATE_ROOT/.subyard-p0-marker"
  CLEANUP_ARMED=1
  download_pinned_installer "$OLD_VERSION" "$OLD_INSTALLER_SHA256" "$OLD_INSTALLER"
  download_pinned_installer "$SOURCE_VERSION" "$SOURCE_INSTALLER_SHA256" "$SOURCE_INSTALLER"
  CANDIDATE_BUNDLE="$RELEASE_ROOT/subyard-$CANDIDATE_VERSION-linux-amd64.tar.gz"
  prepare_current_candidate
  reproduce_official_dead_end success
  prepare_recovery_bridge
  run_candidate_recovery success
  reproduce_official_dead_end post-cas
  run_post_cas_resume post-cas
  printf 'ok: two official v0.9.1 -> v0.11.1 histories and v0.11.2 recovery are covered\n'
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  main "$@"
fi
