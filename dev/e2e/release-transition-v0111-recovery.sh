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
MARKER="subyard-p0-v0111-$TOKEN"
SUCCESS_YARD="v0111-success-$TOKEN"
RELEASE_ROOT="$STATE_ROOT/candidate-release"
OLD_INSTALLER="$STATE_ROOT/subyard-install-$OLD_VERSION.sh"
SOURCE_INSTALLER="$STATE_ROOT/subyard-install-$SOURCE_VERSION.sh"
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

cleanup_fixture() { # success
  local fixture="$1" failed=0 armed=0 status
  select_fixture "$fixture"
  case "$fixture" in
    success) armed="$SUCCESS_ARMED" ;;
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

sink_coordination_binding() {
  native_coordination_binding test-vms-host-sink "$TEST_VMS_SINK" "$TEST_VMS_SINK_SERVICE" "$TEST_VMS_SINK_TIMER"
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

assert_native_resources_absent() {
  local kind path unit target
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

reproduce_official_dead_end() { # success
  local source_rc
  prepare_fixture "$1"
  info "creating independent official v0.9.1 -> v0.11.1 $FIXTURE history"
  install_official "$OLD_INSTALLER" "$OLD_VERSION" >/dev/null
  incus project create "$PROJECT" \
    -c features.images=false -c user.p0-release-recovery="$FIXTURE_MARKER" >/dev/null
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
  # v0.11.2 published the same installer bytes as v0.11.1; a current installer
  # requires the installation inventory that this bridge does not have.
  cp "$SOURCE_INSTALLER" "$RELEASE_ROOT/subyard-install.sh"
  cp "$CURRENT_RELEASE_ROOT/subyard-install-runtime-release.sh" \
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
  local output active manifest status power_coordination sink_coordination
  prepare_current_fixture_inputs
  printf 'release-recovery artifact role=current-candidate version=%s runtime_sha256=%s\n' \
    "$CURRENT_VERSION" "$CURRENT_RUNTIME_SHA256"
  status=0
  fixture_env env YARD_RELEASE_BASE_URL="file://$CURRENT_RELEASE_ROOT" \
    YARD_RELEASE_VERSION="$CURRENT_VERSION" \
    bash "$CURRENT_RELEASE_ROOT/subyard-install.sh" --version "$CURRENT_VERSION" --yes \
    > "$FIXTURE_ROOT/current-upgrade.log" 2>&1 || status=$?
  if [ "$status" != 0 ]; then
    printf 'release-recovery fixture=%s stage=current-upgrade native_status=%s\n' "$FIXTURE" "$status" >&2
    capture_fixture_failure_logs || printf 'release-recovery failure-log capture_failed=true\n' >&2
    return "$status"
  fi
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
  sink_coordination="$(sink_coordination_binding)" \
    || die 'current sink coordination state could not be bound before cleanup'
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
  [ "$(sink_coordination_binding)" = "$sink_coordination" ] \
    || die 'public cleanup changed the bound sink coordination state'
  POWER_COORDINATION_BINDING="$power_coordination"
  require_absent_legacy_power
  printf 'release-recovery fixture=%s stage=current-cleanup resources_absent=true coordination_unchanged=true\n' "$FIXTURE"
  CURRENT_UNINSTALL_REFUSED=
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
             else "other" end),
           resource: (.resource | if type == "string" and test("^activation\\.[a-z0-9][a-z0-9.-]{0,63}$") then . else null end)}))}]}' "$output" 2>/dev/null)"; then
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
  # Independent byte oracle: this fixture's literal two-line source is delivered
  # unchanged. Current preview guidance is a separate shared capability skill.
  actual="$(incus exec "$INSTANCE" --project "$PROJECT" -- \
    sha256sum /home/dev/.codex/AGENTS.md | awk '{print $1}')" \
    || die 'candidate recovery Codex target is unavailable'
  if [ "$actual" != "$expected" ]; then
    printf 'release-recovery materialized-codex stage=%.16s expected_sha256=%.64s actual_sha256=%.64s\n' \
      "$stage" "$expected" "$actual" >&2
    die 'candidate recovery did not materialize the configured Codex target'
  fi
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

main() {
  trap cleanup EXIT
  trap 'handle_signal 130' INT
  trap 'handle_signal 143' TERM
  valid_token "$TOKEN" || die 'numeric P0 allocation token is required'
  [ -n "${SUBYARD_E2E_VM:-}" ] || die 'run only inside an allocated P0 guest'
  [ "$SUBYARD_E2E_VM" = 1 ] || die 'v0.11.1 release recovery is pinned to the P0 owner VM'
  for dependency in curl go incus jq python3 sha256sum sudo systemctl tar; do
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
  printf 'ok: the official v0.9.1 -> v0.11.1 history and its v0.11.2 recovery are covered\n'
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  main "$@"
fi
