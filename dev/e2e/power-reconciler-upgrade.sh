#!/usr/bin/env bash
# Exact published v0.8.0 -> local candidate migration acceptance on an allocated P0 worker VM.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
MODE="${1:-run}"
TOKEN="${2:-}"
# shellcheck source=dev/e2e/lib-p0-capacity.sh
. "$ROOT/dev/e2e/lib-p0-capacity.sh"
OLD_VERSION=0.8.0
OLD_INSTALLER_SHA256=5bd3c61e3dd39cb2d258be5cd75237383f00eff0512c77a3a5ca75d96e6b992b
OLD_UNIT_FIXTURE="$ROOT/tests/fixtures/systemd/subyard-power-reconcile-v0.8.0.service.in"
CANDIDATE_VERSION="p0-power-systemd-$TOKEN"
STATE_ROOT="/var/tmp/subyard-p0-power-systemd-$TOKEN"
MARKER="subyard-p0-power-systemd-$TOKEN"
OPERATOR="subyardpower$TOKEN"
OPERATOR_HOME="/home/$OPERATOR"
OPERATOR_HOME_MARKER="$OPERATOR_HOME/.subyard-p0-power-systemd-home"
RELEASE_ROOT="$STATE_ROOT/candidate"
INSTALLER="$STATE_ROOT/subyard-install-v0.8.0.sh"
SUDOERS="/etc/sudoers.d/$MARKER"
SUDOERS_TEMP=''
PROJECT=subyard
INSTANCE=yard
RECONCILER=/usr/local/libexec/subyard/yard-boot-reconcile
UNIT=/etc/systemd/system/subyard-power-reconcile.service
UNIT_LINK=/etc/systemd/system/multi-user.target.wants/subyard-power-reconcile.service
BEFORE_ROOT="$STATE_ROOT/before"
SNAPSHOT_COMPLETE="$BEFORE_ROOT/.complete"
PROJECT_MUTATION_ARMED="$STATE_ROOT/.project-mutation-armed"
PHASE_STATE="$STATE_ROOT/phase"
BOOT_ID_STATE="$STATE_ROOT/boot-id"
DEFAULT_ROUTE_STATE="$STATE_ROOT/default-route"
OLD_RELEASE_TARGET_STATE="$STATE_ROOT/old-release-target"
CANDIDATE_RELEASE_TARGET_STATE="$STATE_ROOT/candidate-release-target"
LEGACY_STATE_SHA_STATE="$STATE_ROOT/legacy-state.sha256"
CLEANUP_ARMED=0
PRESERVE_FIXTURE=0
OLD_RELEASE_TARGET=''
CANDIDATE_RELEASE_TARGET=''
INCUS_COMMAND_TIMEOUT="${SUBYARD_POWER_SYSTEMD_INCUS_TIMEOUT_SECONDS:-60}"
INCUS_KILL_AFTER_SECONDS=10

die() { printf 'power-reconciler-upgrade: %s\n' "$*" >&2; exit 2; }
info() { printf '  [ .. ] %s\n' "$*"; }
ok() { printf '  [ ok ] %s\n' "$*"; }

[[ "$TOKEN" =~ ^[0-9]+$ ]] || die 'allocation token must be numeric'
[[ "$INCUS_COMMAND_TIMEOUT" =~ ^[1-9][0-9]*$ ]] \
  || die 'SUBYARD_POWER_SYSTEMD_INCUS_TIMEOUT_SECONDS must be a positive integer'
case "${SUBYARD_E2E_VM:-}" in
  1|2) ;;
  *) die 'run on an allocated P0 worker VM through the retained P0 lease' ;;
esac
p0_capacity_init "$TOKEN"

incus() {
  local binary=/usr/bin/incus socket=/var/lib/incus/unix.socket
  [ -x "$binary" ] || return 127
  if [ -S "$socket" ] && [ ! -w "$socket" ]; then
    timeout --signal=TERM --kill-after="$INCUS_KILL_AFTER_SECONDS" \
      "$INCUS_COMMAND_TIMEOUT" sudo -n "$binary" "$@" </dev/null
  else
    timeout --signal=TERM --kill-after="$INCUS_KILL_AFTER_SECONDS" \
      "$INCUS_COMMAND_TIMEOUT" "$binary" "$@" </dev/null
  fi
}

project_presence() {
  local projects
  projects="$(incus project list --format csv -c n)" || return 2
  grep -Fxq "$PROJECT" <<<"$projects" && return 0
  return 1
}

operator_uid() { id -u "$OPERATOR"; }
operator_env() {
  local uid
  uid="$(operator_uid)"
  sudo -n /usr/sbin/runuser -u "$OPERATOR" -- env \
    HOME="$OPERATOR_HOME" USER="$OPERATOR" LOGNAME="$OPERATOR" SHELL=/bin/bash \
    PATH="$OPERATOR_HOME/.local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" \
    GOCACHE="$OPERATOR_HOME/.cache/go-build" GOMODCACHE="$OPERATOR_HOME/go/pkg/mod" \
    XDG_RUNTIME_DIR="/run/user/$uid" \
    DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$uid/bus" \
    "$@"
}
operator_yard() { operator_env "$OPERATOR_HOME/.local/bin/yard" "$@"; }

assert_state_root() {
  [ -d "$STATE_ROOT" ] && [ ! -L "$STATE_ROOT" ] \
    && [ "$(cat "$STATE_ROOT/.marker" 2>/dev/null)" = "$MARKER" ] \
    || die "refusing unmarked fixture root $STATE_ROOT"
}

assert_project_owned() {
  [ "$(incus project get "$PROJECT" user.subyard.p0-power-systemd 2>/dev/null)" = \
    "$MARKER" ] || die "refusing unmarked project $PROJECT"
}

remove_fixture_sudoers() {
  if sudo -n test ! -e "$SUDOERS" && sudo -n test ! -L "$SUDOERS"; then
    return 0
  fi
  sudo -n test -f "$SUDOERS" && sudo -n test ! -L "$SUDOERS" \
    && [ "$(sudo -n stat -c %u:%g "$SUDOERS")" = 0:0 ] \
    && [ "$(sudo -n cat "$SUDOERS")" = "$OPERATOR ALL=(root) NOPASSWD: ALL" ] \
    || return 1
  sudo -n find "$SUDOERS" -delete
}

restore_host_runtime() {
  local path name state enabled_state restored_enabled restored_active
  enabled_state="$(cat "$BEFORE_ROOT/unit-enabled" 2>/dev/null || true)"
  case "$enabled_state" in
    enabled|disabled|absent) ;;
    *) return 1 ;;
  esac
  if [ "$enabled_state" = absent ]; then
    # Teardown may already have removed the candidate unit. A nonzero disable is harmless only
    # when no persistent enablement link remains; verify that invariant before deleting backups.
    sudo -n systemctl disable subyard-power-reconcile.service >/dev/null 2>&1 || true
    sudo -n test ! -e "$UNIT_LINK" && sudo -n test ! -L "$UNIT_LINK" || return 1
  fi
  for path in "$RECONCILER" "$UNIT"; do
    name="$(basename "$path")"
    state="$(cat "$BEFORE_ROOT/$name.state" 2>/dev/null || true)"
    case "$state" in
      present)
        sudo -n install -D -o root -g root \
          -m "$(stat -c %a "$BEFORE_ROOT/$name")" "$BEFORE_ROOT/$name" "$path" \
          || return 1
        ;;
      absent)
        if sudo -n test -e "$path" || sudo -n test -L "$path"; then
          sudo -n test -f "$path" && sudo -n test ! -L "$path" \
            || return 1
          sudo -n find "$path" -delete || return 1
        fi
        ;;
      *) return 1 ;;
    esac
  done
  sudo -n systemctl daemon-reload || return 1
  case "$enabled_state" in
    enabled)
      sudo -n systemctl enable subyard-power-reconcile.service >/dev/null || return 1
      ;;
    disabled)
      sudo -n systemctl disable subyard-power-reconcile.service >/dev/null || return 1
      ;;
    absent) ;;
    *) return 1 ;;
  esac
  case "$(cat "$BEFORE_ROOT/unit-active" 2>/dev/null || true)" in
    active)
      sudo -n systemctl start subyard-power-reconcile.service >/dev/null || return 1
      ;;
    inactive) ;;
    failed)
      sudo -n systemctl reset-failed subyard-power-reconcile.service >/dev/null \
        || return 1
      # Restoring a failed service is intentionally best-effort at start: the exact
      # pre-fixture unit must fail again, and the state assertion below proves it did.
      sudo -n systemctl start subyard-power-reconcile.service >/dev/null 2>&1 || true
      ;;
    *) return 1 ;;
  esac
  restored_enabled="$(sudo -n systemctl is-enabled subyard-power-reconcile.service \
    2>/dev/null || true)"
  case "$enabled_state:$restored_enabled" in
    enabled:enabled|disabled:disabled|absent:not-found) ;;
    *) return 1 ;;
  esac
  restored_active="$(sudo -n systemctl show subyard-power-reconcile.service \
    --property=ActiveState --value 2>/dev/null)" || return 1
  case "$(cat "$BEFORE_ROOT/unit-active"):$restored_active" in
    active:active|inactive:inactive|failed:failed) ;;
    *) return 1 ;;
  esac
  for path in "$RECONCILER" "$UNIT"; do
    name="$(basename "$path")"
    [ "$(cat "$BEFORE_ROOT/$name.state")" != absent ] \
      || { sudo -n test ! -e "$path" && sudo -n test ! -L "$path"; } \
      || return 1
  done
}

delete_owned_project() {
  local type volume project_presence_rc=0
  project_presence || project_presence_rc=$?
  [ "$project_presence_rc" != 1 ] || return 0
  [ "$project_presence_rc" = 0 ] || return 1
  assert_project_owned
  if incus config show "$INSTANCE" --project "$PROJECT" >/dev/null 2>&1; then
    [ "$(incus config get "$INSTANCE" user.subyard.managed --project "$PROJECT" 2>/dev/null)" = \
      true ] || die "refusing foreign instance $PROJECT/$INSTANCE"
    incus delete "$INSTANCE" --project "$PROJECT" --force >/dev/null
  fi
  while IFS=, read -r type volume; do
    [ "$type" = custom ] && [ -n "$volume" ] || continue
    incus storage volume delete default "$volume" --project "$PROJECT" >/dev/null
  done < <(incus storage volume list default --project "$PROJECT" --format csv -c t,n)
  [ -z "$(incus list --project "$PROJECT" --format csv -c n)" ] \
    || die "unexpected instance remains in $PROJECT"
  incus project delete "$PROJECT" >/dev/null
}

cleanup() {
  local rc=$? cleanup_failed=0 home_owned=0 project_owned=0 project_presence_rc=0
  trap - EXIT INT TERM
  if [ "$rc" = 0 ] && [ "$PRESERVE_FIXTURE" = 1 ]; then
    exit 0
  fi
  set +e
  if [ "$CLEANUP_ARMED" != 1 ]; then
    p0_capacity_remove_build_cache || cleanup_failed=1
    p0_capacity_remove_root_if_empty || cleanup_failed=1
    [ "$cleanup_failed" = 0 ] || rc=3
    exit "$rc"
  fi
  if ! [ -d "$STATE_ROOT" ] || [ -L "$STATE_ROOT" ] \
    || [ "$(cat "$STATE_ROOT/.marker" 2>/dev/null)" != "$MARKER" ]; then
    printf 'power-reconciler-upgrade: refusing cleanup without marked state root %s\n' \
      "$STATE_ROOT" >&2
    exit 3
  fi
  if incus info >/dev/null 2>&1; then
    project_presence || project_presence_rc=$?
    if [ "$project_presence_rc" = 0 ]; then
      if [ "$(incus project get "$PROJECT" user.subyard.p0-power-systemd 2>/dev/null)" = \
        "$MARKER" ]; then
        project_owned=1
      else
        cleanup_failed=1
      fi
    else
      [ "$project_presence_rc" = 1 ] || cleanup_failed=1
    fi
  elif [ -e "$PROJECT_MUTATION_ARMED" ]; then
    cleanup_failed=1
  fi
  if id "$OPERATOR" >/dev/null 2>&1 \
    && sudo -n test -x "$OPERATOR_HOME/.local/bin/yard" \
    && [ "$project_owned" = 1 ]; then
    operator_yard teardown --yes >/dev/null 2>&1 || true
  fi
  [ "$project_owned" = 0 ] || delete_owned_project || cleanup_failed=1
  if [ "$(cat "$SNAPSHOT_COMPLETE" 2>/dev/null)" = "$MARKER" ]; then
    sudo -n systemctl stop subyard-power-reconcile.service >/dev/null 2>&1 || true
    restore_host_runtime || cleanup_failed=1
  fi
  if id "$OPERATOR" >/dev/null 2>&1; then
    if sudo -n test -d "$OPERATOR_HOME" && sudo -n test ! -L "$OPERATOR_HOME" \
      && [ "$(sudo -n cat "$OPERATOR_HOME_MARKER" 2>/dev/null)" = "$MARKER" ]; then
      home_owned=1
      sudo -n loginctl disable-linger "$OPERATOR" >/dev/null 2>&1 || true
      sudo -n systemctl stop "user@$(operator_uid).service" >/dev/null 2>&1 || true
    else
      cleanup_failed=1
    fi
  elif sudo -n test -e "$OPERATOR_HOME" || sudo -n test -L "$OPERATOR_HOME"; then
    if sudo -n test -d "$OPERATOR_HOME" && sudo -n test ! -L "$OPERATOR_HOME" \
      && [ "$(sudo -n cat "$OPERATOR_HOME_MARKER" 2>/dev/null)" = "$MARKER" ]; then
      home_owned=1
    else
      cleanup_failed=1
    fi
  fi
  if [ -n "$SUDOERS_TEMP" ]; then
    case "$SUDOERS_TEMP" in
      "/tmp/subyard-p0-power-sudoers-$TOKEN."*)
        find "$SUDOERS_TEMP" -delete || cleanup_failed=1
        ;;
      *) cleanup_failed=1 ;;
    esac
  fi
  remove_fixture_sudoers || cleanup_failed=1
  if id "$OPERATOR" >/dev/null 2>&1 && [ "$home_owned" = 1 ]; then
    sudo -n userdel -r "$OPERATOR" >/dev/null 2>&1 || cleanup_failed=1
  fi
  if { sudo -n test -e "$OPERATOR_HOME" || sudo -n test -L "$OPERATOR_HOME"; } \
    && [ "$home_owned" = 1 ]; then
    sudo -n find "$OPERATOR_HOME" -xdev -depth -delete || cleanup_failed=1
  fi
  if [ "$cleanup_failed" = 0 ]; then
    find "$STATE_ROOT" -xdev -depth -delete || cleanup_failed=1
  fi
  p0_capacity_remove_build_cache || cleanup_failed=1
  p0_capacity_remove_root_if_empty || cleanup_failed=1
  [ "$cleanup_failed" = 0 ] || rc=3
  exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

snapshot_host_runtime() {
  local path name unit_path_state enabled_state active_state
  install -d -m 0700 "$BEFORE_ROOT"
  for path in "$RECONCILER" "$UNIT"; do
    name="$(basename "$path")"
    if sudo -n test -e "$path" || sudo -n test -L "$path"; then
      sudo -n test -f "$path" && sudo -n test ! -L "$path" \
        || die "host power runtime path is unsafe: $path"
      [ "$(sudo -n stat -c %u:%g "$path")" = 0:0 ] \
        || die "host power runtime path is not root-owned: $path"
      sudo -n install -m "$(sudo -n stat -c %a "$path")" "$path" "$BEFORE_ROOT/$name"
      printf 'present\n' > "$BEFORE_ROOT/$name.state"
    else
      printf 'absent\n' > "$BEFORE_ROOT/$name.state"
    fi
  done
  unit_path_state="$(cat "$BEFORE_ROOT/$(basename "$UNIT").state")"
  enabled_state="$(sudo -n systemctl is-enabled subyard-power-reconcile.service \
    2>/dev/null || true)"
  case "$unit_path_state:$enabled_state" in
    present:enabled|present:disabled|absent:not-found) ;;
    *) die "unsupported initial power reconciler enablement: ${enabled_state:-query-error}" ;;
  esac
  [ "$unit_path_state" != absent ] || enabled_state=absent
  printf '%s\n' "$enabled_state" > "$BEFORE_ROOT/unit-enabled"
  active_state="$(sudo -n systemctl show subyard-power-reconcile.service \
    --property=ActiveState --value 2>/dev/null)" \
    || die 'cannot query initial power reconciler active state'
  case "$active_state" in
    active|inactive|failed) ;;
    *) die "unsupported initial power reconciler active state: ${active_state:-query-error}" ;;
  esac
  printf '%s\n' "$active_state" > "$BEFORE_ROOT/unit-active"
  printf '%s\n' "$MARKER" > "$SNAPSHOT_COMPLETE"
}

prepare_operator() {
  local uid
  sudo -n useradd --create-home --home-dir "$OPERATOR_HOME" --shell /bin/bash "$OPERATOR"
  sudo -n install -o "$OPERATOR" -g "$OPERATOR" -m 0600 /dev/null "$OPERATOR_HOME_MARKER"
  operator_env bash -c 'printf "%s\n" "$2" > "$1"' _ "$OPERATOR_HOME_MARKER" "$MARKER"
  sudo -n usermod -aG incus-admin "$OPERATOR"
  SUDOERS_TEMP="$(mktemp "/tmp/subyard-p0-power-sudoers-$TOKEN.XXXXXX")"
  printf '%s ALL=(root) NOPASSWD: ALL\n' "$OPERATOR" > "$SUDOERS_TEMP"
  sudo -n install -o root -g root -m 0440 "$SUDOERS_TEMP" "$SUDOERS"
  find "$SUDOERS_TEMP" -delete
  SUDOERS_TEMP=''
  sudo -n loginctl enable-linger "$OPERATOR"
  uid="$(operator_uid)"
  sudo -n systemctl start "user@$uid.service"
  for _ in $(seq 1 30); do
    sudo -n test -S "/run/user/$uid/bus" && break
    sleep 1
  done
  sudo -n test -S "/run/user/$uid/bus" || die 'fixture operator user bus did not start'
  operator_env install -d -m 0700 "$OPERATOR_HOME/.config/subyard" "$OPERATOR_HOME/.local/bin"
  operator_env bash -c 'printf "%s" "$2" > "$1"' _ \
    "$OPERATOR_HOME/.config/subyard/config.env" \
    $'AGENTS=none\nBASE_IMAGE=subyard-e2e-debian-13-cloud-container\nBASE_IMAGE_FALLBACK=subyard-e2e-debian-13-cloud-container\nDEV_UID=2000\n'
  operator_env chmod 0600 "$OPERATOR_HOME/.config/subyard/config.env"
}

materialize_unit() {
  local template="$1" output="$2"
  sed "s|@SUBYARD_POWER_RECONCILER@|$RECONCILER|g" "$template" > "$output"
}

assert_unit_matches() {
  local template="$1" expected_load="$2" layout="$3"
  local expected="$STATE_ROOT/expected.service" manager_state
  materialize_unit "$template" "$expected"
  sudo -n cmp "$expected" "$UNIT" || die "installed unit does not match $template"
  manager_state="$(sudo -n systemctl show subyard-power-reconcile.service \
    --property=LoadState --property=NeedDaemonReload --property=Type --property=Restart \
    --property=RestartForceExitStatus --property=TimeoutStartUSec --property=RuntimeMaxUSec \
    --property=StartLimitIntervalUSec --property=StartLimitBurst)" \
    || die 'cannot observe installed power reconciler manager state'
  grep -Fxq "LoadState=$expected_load" <<<"$manager_state" \
    && grep -Fxq 'NeedDaemonReload=no' <<<"$manager_state" \
    && grep -Fxq 'Restart=no' <<<"$manager_state" \
    && grep -Fxq 'RestartForceExitStatus=75' <<<"$manager_state" \
    && grep -Fxq 'TimeoutStartUSec=2min' <<<"$manager_state" \
    || die 'installed power reconciler manager state is stale or incomplete'
  case "$layout" in
    4)
      grep -Fxq 'Type=oneshot' <<<"$manager_state" \
        && grep -Fxq 'RuntimeMaxUSec=infinity' <<<"$manager_state" \
        && grep -Fxq 'StartLimitIntervalUSec=0' <<<"$manager_state" \
        && grep -Fxq 'StartLimitBurst=5' <<<"$manager_state" \
        || die "published v0.8.0 manager properties changed: ${manager_state//$'\n'/, }"
      ;;
    5)
      grep -Fxq 'Type=exec' <<<"$manager_state" \
        && grep -Fxq 'RuntimeMaxUSec=2min' <<<"$manager_state" \
        && grep -Fxq 'StartLimitIntervalUSec=15min' <<<"$manager_state" \
        && grep -Fxq 'StartLimitBurst=6' <<<"$manager_state" \
        || die "candidate power reconciler manager properties did not converge: ${manager_state//$'\n'/, }"
      ;;
    *) die "unsupported power reconciler layout assertion: $layout" ;;
  esac
  sudo -n systemctl is-enabled --quiet subyard-power-reconcile.service \
    || die 'installed power reconciler unit is not enabled'
}

assert_runtime_state() {
  local version="$1" unit_template="$2" expected_load="$3" manager_layout="$4"
  local reconciler_target="$5"
  [ "$(operator_yard --version)" = "yard $version" ] \
    || die "active runtime is not yard $version"
  assert_unit_matches "$unit_template" "$expected_load" "$manager_layout"
  operator_env cmp \
    "$OPERATOR_HOME/.subyard/runtime/$reconciler_target/bin/yard-engine" "$RECONCILER" \
    || die 'installed power reconciler executable is stale'
  [ "$(incus config get "$INSTANCE" boot.autostart --project "$PROJECT")" = false ] \
    || die 'update changed boot.autostart=false'
  [ "$(incus config get "$INSTANCE" user.subyard.desired_power --project "$PROJECT")" = \
    running ] || die 'update changed desired=running'
  operator_yard check
}

assert_published_v1_history() {
  operator_env jq -e '
    .layout == 4 and
    .applied == ["migrate-test-yard-owner", "refresh-test-vm-broker", "refresh-power-reconciler"]
  ' "$OPERATOR_HOME/.config/subyard/migrations/state.json" >/dev/null \
    || die 'published v0.8.0 migration history is not the exact layout-4 fixed point'
}

record_published_v1_history() {
  local digest
  assert_published_v1_history
  digest="$(operator_env sha256sum \
    "$OPERATOR_HOME/.config/subyard/migrations/state.json" | awk '{print $1}')"
  write_fixture_value "$LEGACY_STATE_SHA_STATE" "$digest"
}

assert_published_v1_history_unchanged() {
  local expected actual
  assert_published_v1_history
  expected="$(read_fixture_value "$LEGACY_STATE_SHA_STATE")"
  actual="$(operator_env sha256sum \
    "$OPERATOR_HOME/.config/subyard/migrations/state.json" | awk '{print $1}')"
  [ "$actual" = "$expected" ] \
    || die 'candidate-owned v2 transition rewrote the superseded v1 migration history'
}

assert_v2_transition() {
  local direction="$1" target="${2#releases/}"
  operator_env jq -e '
    .schemaVersion == 2 and
    .domains["owner-registration"] == {
      "epoch": 2, "applied": ["canonicalize-test-yard-owner-v2"]
    } and
    .domains["power-metadata"] == {"epoch": 1, "applied": []} and
    .domains["project-state"] == {"epoch": 1, "applied": []} and
    .domains.settings == {
      "epoch": 2, "applied": ["canonicalize-test-vms-settings-v2"]
    }
  ' "$OPERATOR_HOME/.config/subyard/release-transition/v2/ledger.json" >/dev/null \
    || die 'candidate v2 migration ledger is not at its exact per-domain fixed point'
  operator_env jq -e --arg direction "$direction" --arg target "$target" '
    .schemaVersion == 2 and .checkpoint == "complete" and
    .goal == {"target": $target, "direction": $direction} and
    .releases.target == $target and all(.steps[]; .checkpoint == "verified")
  ' "$OPERATOR_HOME/.config/subyard/release-transition/v2/journal.json" >/dev/null \
    || die 'candidate v2 transition journal is not complete for the exact release goal'
}

assert_runtime_links() {
  local current="$1" previous="$2" runtime="$OPERATOR_HOME/.subyard/runtime"
  [ "$(operator_env readlink "$runtime/current")" = "$current" ] \
    && [ "$(operator_env readlink "$runtime/previous")" = "$previous" ] \
    || die "runtime links are not current=$current previous=$previous"
}

assert_candidate_state() {
  assert_runtime_state "$CANDIDATE_VERSION" \
    "$ROOT/config/systemd/subyard-power-reconcile.service.in" loaded 5 \
    "$CANDIDATE_RELEASE_TARGET"
  assert_published_v1_history_unchanged
  assert_v2_transition activate-target "$CANDIDATE_RELEASE_TARGET"
}

verify_bridge_plan_is_read_only() {
  local before after output rc
  before="$(operator_env bash -c '
    set -euo pipefail
    sha256sum "$1" "$2"
    readlink "$3"
    if [ -L "$4" ]; then readlink "$4"; else printf "absent\n"; fi
  ' _ "$OPERATOR_HOME/.config/subyard/migrations/state.json" "$UNIT" \
    "$OPERATOR_HOME/.subyard/runtime/current" "$OPERATOR_HOME/.subyard/runtime/previous")"
  set +e
  output="$(operator_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" \
    "$RELEASE_ROOT/subyard-install.sh" --version "$CANDIDATE_VERSION" \
      </dev/null 2>&1 >/dev/null)"
  rc=$?
  set -e
  [ "$rc" = 1 ] && grep -Fq 'confirmation required: interactive terminal required' <<<"$output" \
    || die "unconfirmed standalone bridge did not stop at its candidate-owned plan: $output"
  after="$(operator_env bash -c '
    set -euo pipefail
    sha256sum "$1" "$2"
    readlink "$3"
    if [ -L "$4" ]; then readlink "$4"; else printf "absent\n"; fi
  ' _ "$OPERATOR_HOME/.config/subyard/migrations/state.json" "$UNIT" \
    "$OPERATOR_HOME/.subyard/runtime/current" "$OPERATOR_HOME/.subyard/runtime/previous")"
  [ "$before" = "$after" ] \
    || die 'unconfirmed standalone bridge mutated stable links, migration state or unit'
}

write_fixture_value() {
  local path="$1" value="$2" temporary
  case "$path" in
    "$STATE_ROOT"/*) ;;
    *) die "refusing fixture state outside $STATE_ROOT: $path" ;;
  esac
  temporary="$(mktemp "$STATE_ROOT/.state.XXXXXX")"
  printf '%s\n' "$value" > "$temporary"
  chmod 0600 "$temporary"
  mv -f -- "$temporary" "$path"
}

read_fixture_value() {
  local path="$1" value lines
  [ -f "$path" ] && [ ! -L "$path" ] \
    || die "fixture state is missing or unsafe: $path"
  lines="$(wc -l < "$path")"
  [ "$lines" = 1 ] || die "fixture state is not one line: $path"
  IFS= read -r value < "$path"
  [ -n "$value" ] || die "fixture state is empty: $path"
  printf '%s\n' "$value"
}

assert_fixture_phase() {
  local expected="$1" actual
  actual="$(read_fixture_value "$PHASE_STATE")"
  [ "$actual" = "$expected" ] \
    || die "fixture phase is $actual, expected $expected"
}

load_release_targets() {
  OLD_RELEASE_TARGET="$(read_fixture_value "$OLD_RELEASE_TARGET_STATE")"
  CANDIDATE_RELEASE_TARGET="$(read_fixture_value "$CANDIDATE_RELEASE_TARGET_STATE")"
}

record_reboot_baseline() {
  local boot_id route_temporary
  boot_id="$(cat /proc/sys/kernel/random/boot_id)"
  [ -n "$boot_id" ] || die 'host boot ID is empty'
  write_fixture_value "$BOOT_ID_STATE" "$boot_id"
  route_temporary="$(mktemp "$STATE_ROOT/.default-route.XXXXXX")"
  ip -4 route show default > "$route_temporary"
  [ -s "$route_temporary" ] || die 'host default route is empty before reboot'
  chmod 0600 "$route_temporary"
  mv -f -- "$route_temporary" "$DEFAULT_ROUTE_STATE"
}

assert_post_reboot_candidate() {
  local previous_boot current_boot current_route manager_state started instance_state
  previous_boot="$(read_fixture_value "$BOOT_ID_STATE")"
  current_boot="$(cat /proc/sys/kernel/random/boot_id)"
  [ -n "$current_boot" ] && [ "$current_boot" != "$previous_boot" ] \
    || die 'power reconciler fixture did not cross a host reboot'
  [ -f "$DEFAULT_ROUTE_STATE" ] && [ ! -L "$DEFAULT_ROUTE_STATE" ] \
    || die 'pre-reboot default-route state is missing or unsafe'
  current_route="$(mktemp "$STATE_ROOT/.default-route-current.XXXXXX")"
  ip -4 route show default > "$current_route"
  if ! cmp -s "$DEFAULT_ROUTE_STATE" "$current_route"; then
    printf 'power-reconciler-upgrade: default route changed across reboot\n' >&2
    diff -u "$DEFAULT_ROUTE_STATE" "$current_route" >&2 || true
    find "$current_route" -delete
    return 2
  fi
  find "$current_route" -delete

  load_release_targets
  assert_candidate_state
  assert_runtime_links "$CANDIDATE_RELEASE_TARGET" "$OLD_RELEASE_TARGET"
  manager_state="$(sudo -n systemctl show subyard-power-reconcile.service \
    --property=LoadState --property=NeedDaemonReload \
    --property=ActiveState --property=SubState --property=Result \
    --property=ExecMainStatus --property=ExecMainStartTimestampMonotonic)" \
    || die 'cannot observe current-boot power reconciler result'
  started="$(sed -n 's/^ExecMainStartTimestampMonotonic=//p' <<<"$manager_state")"
  grep -Fxq 'LoadState=loaded' <<<"$manager_state" \
    && grep -Fxq 'NeedDaemonReload=no' <<<"$manager_state" \
    && grep -Fxq 'ActiveState=inactive' <<<"$manager_state" \
    && grep -Fxq 'SubState=dead' <<<"$manager_state" \
    && grep -Fxq 'Result=success' <<<"$manager_state" \
    && grep -Fxq 'ExecMainStatus=0' <<<"$manager_state" \
    && [[ "$started" =~ ^[1-9][0-9]*$ ]] \
    || die "candidate did not reconcile successfully in the current boot: ${manager_state//$'\n'/, }"
  instance_state="$(incus list "$INSTANCE" --project "$PROJECT" --format csv -c s)"
  [ "$instance_state" = RUNNING ] \
    || die "desired-running yard is $instance_state after reboot"
}

prepare_candidate() {
  local systemd_version old_load
  [ ! -e "$STATE_ROOT" ] && [ ! -L "$STATE_ROOT" ] \
    || die "fixture state already exists: $STATE_ROOT"
  ! id "$OPERATOR" >/dev/null 2>&1 || die "fixture operator already exists: $OPERATOR"
  [ ! -e "$OPERATOR_HOME" ] && [ ! -L "$OPERATOR_HOME" ] \
    || die "fixture operator home already exists: $OPERATOR_HOME"
  sudo -n test ! -e "$SUDOERS" && sudo -n test ! -L "$SUDOERS" \
    || die "fixture sudoers path already exists: $SUDOERS"
  ! incus project show "$PROJECT" >/dev/null 2>&1 \
    || die "fixture project already exists: $PROJECT"

  install -d -m 0711 "$STATE_ROOT"
  printf '%s\n' "$MARKER" > "$STATE_ROOT/.marker"
  snapshot_host_runtime
  CLEANUP_ARMED=1
  prepare_operator

  p0_capacity_reset_build_cache
  info "packaging local layout-5 candidate $CANDIDATE_VERSION"
  "$ROOT/dev/package-engine.sh" --output-dir "$RELEASE_ROOT" \
    --version "$CANDIDATE_VERSION" >/dev/null
  chmod -R a+rX "$RELEASE_ROOT"

  info 'installing exact published v0.8.0 runtime'
  curl -fsSL --proto '=https' --tlsv1.2 \
    "https://github.com/Subyard/Subyard/releases/download/v$OLD_VERSION/subyard-install.sh" \
    -o "$INSTALLER"
  [ "$(sha256sum "$INSTALLER" | awk '{print $1}')" = "$OLD_INSTALLER_SHA256" ] \
    || die 'published v0.8.0 installer checksum changed'
  chmod 0755 "$INSTALLER"
  operator_env env YARD_RELEASE_VERSION="$OLD_VERSION" \
    "$INSTALLER" --version "$OLD_VERSION" --yes

  printf '%s\n' "$MARKER" > "$PROJECT_MUTATION_ARMED"
  incus project create "$PROJECT" \
    -c features.images=false -c user.subyard.p0-power-systemd="$MARKER" >/dev/null
  operator_yard init --yes
  operator_yard start --yes
  systemd_version="$(systemd-analyze --version | awk 'NR == 1 {print $2}')"
  if [ "$systemd_version" -lt 256 ]; then old_load=bad-setting; else old_load=loaded; fi
  OLD_RELEASE_TARGET="$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/current")"
  write_fixture_value "$OLD_RELEASE_TARGET_STATE" "$OLD_RELEASE_TARGET"
  assert_runtime_state "$OLD_VERSION" "$OLD_UNIT_FIXTURE" "$old_load" 4 \
    "$OLD_RELEASE_TARGET"
  record_published_v1_history
  ok 'published v0.8.0 layout and installed incompatible unit reproduced'

  info 'running the candidate-owned standalone bridge from the published predecessor'
  verify_bridge_plan_is_read_only
  operator_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" \
    "$RELEASE_ROOT/subyard-install.sh" --version "$CANDIDATE_VERSION" --yes
  CANDIDATE_RELEASE_TARGET="$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/current")"
  write_fixture_value "$CANDIDATE_RELEASE_TARGET_STATE" "$CANDIDATE_RELEASE_TARGET"
  assert_candidate_state
  assert_runtime_links "$CANDIDATE_RELEASE_TARGET" "$OLD_RELEASE_TARGET"
  ok 'candidate-owned bridge reached the v2 fixed point without rewriting v1 history'
}

finish_candidate_flow() {
  load_release_targets
  info 'rolling back to the exact published v0.8.0 runtime under the active v2 owner'
  operator_yard update --rollback --yes
  assert_runtime_state "$OLD_VERSION" \
    "$ROOT/config/systemd/subyard-power-reconcile.service.in" loaded 5 \
    "$CANDIDATE_RELEASE_TARGET"
  assert_published_v1_history_unchanged
  assert_v2_transition activate-previous "$OLD_RELEASE_TARGET"
  assert_runtime_links "$OLD_RELEASE_TARGET" "$CANDIDATE_RELEASE_TARGET"
  ok 'rollback activated v0.8.0 while retaining the compatible v2-owned power runtime'

  info 'rolling forward to the local candidate again'
  operator_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" \
    "$RELEASE_ROOT/subyard-install.sh" --version "$CANDIDATE_VERSION" --yes
  assert_candidate_state
  assert_runtime_links "$CANDIDATE_RELEASE_TARGET" "$OLD_RELEASE_TARGET"
  ok 'roll-forward restored the compatible candidate runtime and unit'
}

for command in sudo systemctl timeout; do
  command -v "$command" >/dev/null 2>&1 || die "$command is required"
done
sudo -n true || die 'passwordless sudo is required on the disposable VM'
case "$MODE" in
  clean)
    if [ ! -e "$STATE_ROOT" ] && [ ! -L "$STATE_ROOT" ]; then
      ! id "$OPERATOR" >/dev/null 2>&1 \
        && [ ! -e "$OPERATOR_HOME" ] && [ ! -L "$OPERATOR_HOME" ] \
        && sudo -n test ! -e "$SUDOERS" \
        && sudo -n test ! -L "$SUDOERS" \
        || die 'power reconciler fixture residue exists without its state marker'
      if incus info >/dev/null 2>&1; then
        project_presence_rc=0
        project_presence || project_presence_rc=$?
        [ "$project_presence_rc" != 0 ] \
          || die 'power reconciler project exists without its state marker'
        [ "$project_presence_rc" = 1 ] \
          || die 'cannot verify power reconciler project absence'
      fi
      printf 'ok: exact v0.8.0 power reconciler fixture is clean\n'
      exit 0
    fi
    assert_state_root
    CLEANUP_ARMED=1
    exit 0
    ;;
  run|prepare|resume|finish) ;;
  *) die 'expected run, prepare, resume, finish or clean mode' ;;
esac
for command in cmp curl diff go incus ip jq sed sha256sum systemd-analyze; do
  command -v "$command" >/dev/null 2>&1 || die "$command is required"
done
incus info >/dev/null || die 'initialized Incus is required'
[ "$(sha256sum "$OLD_UNIT_FIXTURE" | awk '{print $1}')" = \
  36692bddc0de036c9ec7393b86a6883e7cde59f33a25e352ec2ee7b694890aaf ] \
  || die 'the exact v0.8.0 unit fixture changed'

case "$MODE" in
  run|prepare)
    incus image info subyard-e2e-debian-13-cloud-container --project default >/dev/null \
      || die 'the P0 Debian image cache is required'
    prepare_candidate
    if [ "$MODE" = prepare ]; then
      record_reboot_baseline
      write_fixture_value "$PHASE_STATE" candidate-ready
      PRESERVE_FIXTURE=1
      printf 'ok: exact published v0.8.0 update is ready for reboot verification\n'
      exit 0
    fi
    finish_candidate_flow
    printf 'ok: exact published v0.8.0 power reconciler update, rollback and roll-forward passed\n'
    ;;
  resume)
    assert_state_root
    CLEANUP_ARMED=1
    assert_fixture_phase candidate-ready
    assert_post_reboot_candidate
    info 're-running yard init to prove idempotent boot reconciliation convergence'
    operator_yard init --yes
    assert_candidate_state
    record_reboot_baseline
    write_fixture_value "$PHASE_STATE" candidate-reconciled
    PRESERVE_FIXTURE=1
    printf 'ok: candidate survived reboot and idempotent init; ready for second reboot\n'
    ;;
  finish)
    assert_state_root
    CLEANUP_ARMED=1
    assert_fixture_phase candidate-reconciled
    assert_post_reboot_candidate
    finish_candidate_flow
    printf 'ok: exact published v0.8.0 update survived two reboots, rollback and roll-forward\n'
    ;;
esac
