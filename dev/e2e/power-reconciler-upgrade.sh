#!/usr/bin/env bash
# Exact published v0.11.0 -> local candidate migration acceptance on an allocated P0 worker VM.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
MODE="${1:-run}"
TOKEN="${2:-}"
# shellcheck source=dev/e2e/lib-p0-capacity.sh
. "$ROOT/dev/e2e/lib-p0-capacity.sh"
# shellcheck source=dev/e2e/lib-power-upgrade-contracts.sh
. "$ROOT/dev/e2e/lib-power-upgrade-contracts.sh"
OLD_VERSION=0.11.0
OLD_INSTALLER_SHA256=41acb799e55cf82cdbbef8e7f75e8e17c2df344c1ceeb748181fb6aec7ea6f8d
OLD_BUNDLE_SHA256=30ff2437219dcaaa568599a9df68a62d3f9203d66579d7ee6c802e527ca92163
OLD_UNIT_FIXTURE=''
CANDIDATE_VERSION="0.19.1-power-systemd.$TOKEN"
if [ -f "$ROOT/.subyard-acceptance/candidate.json" ]; then
  CANDIDATE_VERSION="$(jq -er '.version' "$ROOT/.subyard-acceptance/candidate.json")"
fi
STATE_ROOT="/var/tmp/subyard-p0-power-systemd-$TOKEN"
MARKER="subyard-p0-power-systemd-$TOKEN"
OPERATOR="subyardpower$TOKEN"
OPERATOR_HOME="/home/$OPERATOR"
OPERATOR_HOME_MARKER="$OPERATOR_HOME/.subyard-p0-power-systemd-home"
RELEASE_ROOT="$STATE_ROOT/candidate"
INSTALLER="$STATE_ROOT/subyard-install-v0.11.0.sh"
COMPATIBLE_VERSION=0.21.0
COMPATIBLE_RELEASE_ROOT="$STATE_ROOT/published-compatible"
COMPATIBLE_RUNTIME_ROOT="$STATE_ROOT/published-compatible-runtime"
COMPATIBLE_RELEASE_TARGET=releases/0.21.0-1fee6251856e
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
ACTIVATION_LEDGER_BASELINE="$STATE_ROOT/activation-ledger.before"
ACTIVATION_JOURNAL_BASELINE="$STATE_ROOT/activation-journal.before"
ACTIVATION_FAULT_PROBE="$OPERATOR_HOME/.subyard-p0-activation-reconcile-interrupted"
ACTIVATION_SYSTEMCTL_WRAPPER="$OPERATOR_HOME/.local/bin/systemctl"
ACTIVATION_SYSTEMCTL_DELEGATE=/usr/bin/systemctl
V2_STATE_ROOT="$OPERATOR_HOME/.config/subyard/release-transition/v2"
V2_LEDGER="$V2_STATE_ROOT/ledger.json"
V2_JOURNAL="$V2_STATE_ROOT/journal.json"
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

operator_uid() { id -u "$OPERATOR"; }
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

assert_unit_matches() {
  local template="$1" expected_load="$2" contract="$3"
  local expected="$STATE_ROOT/expected.service" manager_state
  materialize_unit "$template" "$expected"
  sudo -n cmp "$expected" "$UNIT" || die "installed unit does not match $template"
  manager_state="$(sudo -n systemctl show subyard-power-reconcile.service \
    --property=LoadState --property=NeedDaemonReload --property=Type --property=Restart \
    --property=RestartForceExitStatus --property=TimeoutStartUSec --property=RuntimeMaxUSec \
    --property=StartLimitIntervalUSec --property=StartLimitBurst --property=RestartUSec)" \
    || die 'cannot observe installed power reconciler manager state'
  grep -Fxq "LoadState=$expected_load" <<<"$manager_state" \
    && grep -Fxq 'NeedDaemonReload=no' <<<"$manager_state" \
    && grep -Fxq 'Restart=no' <<<"$manager_state" \
    && grep -Fxq 'RestartForceExitStatus=75' <<<"$manager_state" \
    && grep -Fxq 'TimeoutStartUSec=2min' <<<"$manager_state" \
    || die 'installed power reconciler manager state is stale or incomplete'
  case "$contract" in
    released)
      grep -Fxq 'Type=exec' <<<"$manager_state" \
        && grep -Fxq 'RuntimeMaxUSec=2min' <<<"$manager_state" \
        && grep -Fxq 'StartLimitIntervalUSec=15min' <<<"$manager_state" \
        && grep -Fxq 'StartLimitBurst=6' <<<"$manager_state" \
        && grep -Fxq 'RestartUSec=10s' <<<"$manager_state" \
        || die "released power reconciler manager properties differ: ${manager_state//$'\n'/, }"
      ;;
    candidate)
      grep -Fxq 'Type=exec' <<<"$manager_state" \
        && grep -Fxq 'RuntimeMaxUSec=2min' <<<"$manager_state" \
        && grep -Fxq 'StartLimitIntervalUSec=0' <<<"$manager_state" \
        && grep -Fxq 'RestartUSec=30s' <<<"$manager_state" \
        || die "candidate power reconciler manager properties did not converge: ${manager_state//$'\n'/, }"
      ;;
    *) die "unsupported power reconciler manager contract: $contract" ;;
  esac
  sudo -n systemctl is-enabled --quiet subyard-power-reconcile.service \
    || die 'installed power reconciler unit is not enabled'
}

assert_runtime_state() {
  local version="$1" unit_template="$2" expected_load="$3" manager_contract="$4"
  local reconciler_target="$5"
  [ "$(operator_yard --version)" = "yard $version" ] \
    || die "active runtime is not yard $version"
  assert_unit_matches "$unit_template" "$expected_load" "$manager_contract"
  operator_env cmp \
    "$OPERATOR_HOME/.subyard/runtime/$reconciler_target/bin/yard-engine" "$RECONCILER" \
    || die 'installed power reconciler executable is stale'
  [ "$(incus config get "$INSTANCE" boot.autostart --project "$PROJECT")" = false ] \
    || die 'update changed boot.autostart=false'
  [ "$(incus config get "$INSTANCE" user.subyard.desired_power --project "$PROJECT")" = \
    running ] || die 'update changed desired=running'
  operator_yard check
}

assert_runtime_links() {
  local current="$1" previous="$2" runtime="$OPERATOR_HOME/.subyard/runtime"
  [ "$(operator_env readlink "$runtime/current")" = "$current" ] \
    && [ "$(operator_env readlink "$runtime/previous")" = "$previous" ] \
    || die "runtime links are not current=$current previous=$previous"
}

assert_candidate_state() {
  if [ -f "$STATE_ROOT/legacy-continuation.sha256" ]; then assert_legacy_continuation; fi
  assert_runtime_state "$CANDIDATE_VERSION" \
    "$OPERATOR_HOME/.subyard/runtime/$CANDIDATE_RELEASE_TARGET/config/systemd/subyard-power-reconcile.service.in" loaded candidate \
    "$CANDIDATE_RELEASE_TARGET"
  assert_v2_transition activate-target "$CANDIDATE_RELEASE_TARGET"
}

bridge_read_only_snapshot() {
  operator_env bash -c '
    set -euo pipefail
    for path in "$1" "$2"; do
      if [ ! -e "$path" ] && [ ! -L "$path" ]; then
        printf "absent %s\n" "$path"
      elif [ -f "$path" ] && [ ! -L "$path" ]; then
        sha256sum "$path"
      else
        exit 2
      fi
    done
    sha256sum "$3"
    readlink "$4"
    if [ -L "$5" ]; then readlink "$5"; else printf "absent\n"; fi
  ' _ "$V2_LEDGER" "$V2_JOURNAL" "$UNIT" \
    "$OPERATOR_HOME/.subyard/runtime/current" "$OPERATOR_HOME/.subyard/runtime/previous"
}

verify_bridge_plan_is_read_only() {
  local before after output rc
  before="$(bridge_read_only_snapshot)"
  set +e
  output="$(operator_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" \
    "$RELEASE_ROOT/subyard-install.sh" --version "$CANDIDATE_VERSION" \
      </dev/null 2>&1 >/dev/null)"
  rc=$?
  set -e
  [ "$rc" = 1 ] && grep -Fq 'confirmation required: interactive terminal required' <<<"$output" \
    || die "unconfirmed standalone bridge did not stop at its candidate-owned plan: $output"
  after="$(bridge_read_only_snapshot)"
  [ "$before" = "$after" ] \
    || die 'unconfirmed standalone bridge mutated stable links, migration state or unit'
}

verify_historical_init_boundary() {
  assert_legacy_continuation
  operator_yard init --yes
  assert_legacy_continuation
  assert_candidate_state
  ok 'plain init reused the limited continuation grant without adopting installation ownership'
  sudo -n systemctl start subyard-power-reconcile.service
  assert_post_reboot_candidate
  ok 'installed candidate boot reconciliation is repeatable with limited legacy authority'
}

load_release_targets() {
  OLD_RELEASE_TARGET="$(read_fixture_value "$OLD_RELEASE_TARGET_STATE")"
  CANDIDATE_RELEASE_TARGET="$(read_fixture_value "$CANDIDATE_RELEASE_TARGET_STATE")"
  OLD_UNIT_FIXTURE="$OPERATOR_HOME/.subyard/runtime/$OLD_RELEASE_TARGET/config/systemd/subyard-power-reconcile.service.in"
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

assert_legacy_continuation() {
  local grant="$OPERATOR_HOME/.config/subyard/ownership/legacy-continuations/default.json"
  [ -z "$(incus project get "$PROJECT" user.subyard.installation)" ] \
    || die 'legacy continuation acquired destructive installation ownership'
  [ "$(operator_env stat -c '%a' "$grant")" = 600 ] \
    && [ "$(operator_env sha256sum "$grant" | awk '{print $1}')" = \
      "$(cat "$STATE_ROOT/legacy-continuation.sha256")" ] \
    || die 'protected continuation grant changed across init, reboot or release links'
}

legacy_continuation_identity() {
  local grant="$OPERATOR_HOME/.config/subyard/ownership/legacy-continuations/default.json"
  local uuid
  incus project get "$PROJECT" user.subyard.p0-power-systemd | cmp - <(printf '%s\n' "$MARKER") || return "$?"
  incus project get "$PROJECT" user.subyard.installation | cmp - <(printf '\n') || return "$?"
  operator_env test -f "$grant" && operator_env test ! -L "$grant" \
    && [ "$(operator_env stat -c '%a:%u' "$grant")" = "600:$(operator_uid)" ] || return 1
  uuid="$(operator_env jq -er --argjson uid "$(operator_uid)" \
    'select(.schema == 1 and .scope == "normal-reconcile" and .operatorUid == $uid) | .instanceUuid | select(type == "string" and length > 0)' \
    "$grant")" || return "$?"
  incus config get "$INSTANCE" volatile.uuid --project "$PROJECT" | cmp - <(printf '%s\n' "$uuid") || return "$?"
  printf '%s ' "$uuid"
  operator_env sha256sum "$grant" | awk '{print $1}'
}

init_legacy_continuation() {
  local output output_fd output_path capture_pid rc=0 capture_rc=0 pending=0 identity after_identity script probe=0
  output="$(mktemp "$STATE_ROOT/continuation-init.XXXXXX")" || return "$?"
  chmod 0600 "$output" || return "$?"
  exec {output_fd}<>"$output" || return "$?"
  output_path="/proc/$BASHPID/fd/$output_fd"
  # Retain only a bounded terminal diagnostic while preserving ordinary stderr.
  operator_yard "$@" 2> >(
    status=0
    tee >(tail -c 16384 >&"$output_fd") >&2 || status=$?
    wait "$!" || exit "$?"
    exit "$status"
  ) || rc=$?
  capture_pid=$!
  wait "$capture_pid" || capture_rc=$?
  if [ "$rc" = 1 ] && [ "$capture_rc" = 0 ] && [ "$disk_tmp_transition" = 0 ] \
    && printf '%s\n' 'yard: init: verify init stage "provision": yard default: disk-backed /tmp policy is installed; the active /tmp is memory-backed; reboot required; close active yard sessions, then run yard -Y default stop and yard -Y default start' \
      | cmp - <(tail -n 1 "$output_path"); then
    pending=1
  fi
  exec {output_fd}>&-
  [ "$rc" != 0 ] || return 0
  printf 'legacy_continuation_init_exit=%s\n' "$rc"
  [ "$pending" = 1 ] || return "$rc"
  script="$(cat "$ROOT/scripts/guest-disk-tmp.sh")" || return "$?"
  # incus() deliberately has no stdin; pass the unchanged production leaf as argv.
  incus exec "$INSTANCE" --project "$PROJECT" -- sh -c "$script" sh check || probe=$?
  [ "$probe" = 10 ] || { [ "$probe" != 0 ] && return "$probe"; return "$rc"; }
  identity="$(legacy_continuation_identity)" || return "$rc"
  if [ ! -e "$STATE_ROOT/legacy-continuation.sha256" ]; then
    write_fixture_value "$STATE_ROOT/legacy-continuation.sha256" "${identity##* }" || return "$rc"
  fi
  assert_legacy_continuation
  verify_legacy_full_removal_refusal
  disk_tmp_transition=1
  operator_yard stop --yes || return "$?"
  operator_yard start --yes || return "$?"
  repair_legacy_activation_after_restart || return "$?"
  operator_yard init --yes || return "$?"
  incus exec "$INSTANCE" --project "$PROJECT" -- sh -c "$script" sh check || return "$?"
  after_identity="$(legacy_continuation_identity)" || return "$rc"
  [ "$after_identity" = "$identity" ] || return "$rc"
}

repair_legacy_activation_after_restart() {
  # Completed release history grants no authority for the new activation drift.
  local report="$STATE_ROOT/post-restart-migrate.json" migration_output="$STATE_ROOT/post-restart-migrate.log"
  local ledger_before journal_before links_before transaction_before migration_rc=0
  local target="${CANDIDATE_RELEASE_TARGET#releases/}" runtime="$OPERATOR_HOME/.subyard/runtime"
  local resources='["activation.materialized-config","activation.host-power","activation.core-guest-runtime","activation.key-tools-runtime","activation.test-vm-broker","activation.test-yard-route-consumers","activation.git-fetch-runtime","activation.github-runtime","activation.openlit-runtime","activation.orca-runtime"]'
  ledger_before="$(operator_env sha256sum "$V2_LEDGER" | awk '{print $1}')" || return "$?"
  journal_before="$(operator_env sha256sum "$V2_JOURNAL" | awk '{print $1}')" || return "$?"
  links_before="$(operator_env stat -c '%d:%i:%a:%u:%g:%N' "$runtime/current" "$runtime/previous")" || return "$?"
  transaction_before="$(operator_env jq -er 'select(.checkpoint == "complete") | .transaction | select(type == "string" and length > 0)' "$V2_JOURNAL")" || return "$?"
  install -m 0600 /dev/null "$report" || return "$?"
  install -m 0600 /dev/null "$migration_output" || return "$?"
  operator_yard migrate --check --json > "$report" 2> "$migration_output" || migration_rc=$?
  write_fixture_value "$report.exit" "$migration_rc" || return "$?"
  printf 'power_activation_repair_check_exit=%s\n' "$migration_rc"
  jq -c --argjson resources "$resources" '
    {status:(if .outcome.status == "migration-required" or .outcome.status == "ready" then .outcome.status else "unknown" end),
     code:(if .outcome.code == "transition-required" or .outcome.code == "ready" then .outcome.code else "unknown" end),
     activation_count:([(.decisions // [])[] | select(.scope == "activation")] | length),
     activation:[(.decisions // [])[0:256][] | select(.scope == "activation") |
       {resource:(.resource as $r | if ($r | type) != "string" then "unknown" elif $resources | index($r) then $r else "unknown" end),
        decision:(if .decision == "canonicalize" then .decision else "unknown" end)}]}
  ' "$report" 2>/dev/null | sed 's/^/power_activation_repair_inspection=/' \
    || printf 'power_activation_repair_inspection=unknown\n'
  [ "$migration_rc" = 0 ] || return "$migration_rc"
  [ "$(operator_env sha256sum "$V2_LEDGER" | awk '{print $1}')" = "$ledger_before" ] \
    && [ "$(operator_env sha256sum "$V2_JOURNAL" | awk '{print $1}')" = "$journal_before" ] \
    && [ "$(operator_env stat -c '%d:%i:%a:%u:%g:%N' "$runtime/current" "$runtime/previous")" = "$links_before" ] \
    || die 'post-restart migration inspection changed protected history or runtime links'
  assert_legacy_continuation
  # A ready fixed point preserves completed history and requires no new consent.
  if ! jq -e --arg current "$target" '
    .schemaVersion == 1 and .current == $current and .owner == $current and
    .outcome.status == "ready" and .outcome.code == "ready" and .outcome.reachedGoal == true and
    .outcome.active == $current and .outcome.target == $current and
    (if has("next") then (.next | type == "string" and . == "") else true end) and
    (.domains | type == "array" and length > 0) and all(.domains[]; .pending == []) and
    (if has("blockers") then (.blockers | type == "array" and length == 0) else true end) and
    (if has("decisions") then (.decisions | type == "array" and length <= 256 and all(.[]; .decision == "preserve")) else true end)
  ' "$report" >/dev/null 2>&1; then
    jq -e --arg current "$target" --argjson resources "$resources" '
      .schemaVersion == 1 and .current == $current and .owner == $current and .next == "yard migrate" and
      .outcome.status == "migration-required" and .outcome.code == "transition-required" and
      (.domains | type == "array" and length > 0) and all(.domains[]; .pending == []) and
      ((.blockers // []) | length == 0) and (.decisions | type == "array" and length > 0 and length <= 256) and
      any(.decisions[]; .scope == "activation") and
      all(.decisions[];
        (has("scope") | not) and .resource == "transition.source-metadata" and .decision == "preserve" or
        (.scope == "activation" and .decision == "canonicalize" and
          (.resource as $r | if ($r | type) == "string" then $resources | index($r) != null else false end)))
    ' "$report" >/dev/null 2>&1 || die 'post-restart migration inspection is not an activation-only plan'
    migration_rc=0
    operator_yard migrate --yes > "$migration_output" 2>&1 || migration_rc=$?
    write_fixture_value "$migration_output.exit" "$migration_rc" || return "$?"
    printf 'power_activation_repair_apply_exit=%s\n' "$migration_rc"
    [ "$migration_rc" = 0 ] || return "$migration_rc"
    [ "$(operator_env sha256sum "$V2_LEDGER" | awk '{print $1}')" = "$ledger_before" ] \
      && [ "$(operator_env stat -c '%d:%i:%a:%u:%g:%N' "$runtime/current" "$runtime/previous")" = "$links_before" ] \
      || die 'post-restart activation repair changed the migration ledger or runtime links'
    operator_env jq -e --arg transaction "$transaction_before" --arg target "$target" '
      .schemaVersion == 2 and .checkpoint == "complete" and .transaction != $transaction and
      (.transaction | type == "string" and length > 0) and .steps == [] and
      .goal == {target:$target,direction:"activate-target"} and .releases.target == $target and .releases.from == $target
    ' "$V2_JOURNAL" >/dev/null || die 'post-restart activation repair reused history or replayed migrations'
  fi
  assert_legacy_continuation
  verify_legacy_full_removal_refusal
}

establish_legacy_continuation() {
  local grant="$OPERATOR_HOME/.config/subyard/ownership/legacy-continuations/default.json"
  local disk_tmp_transition=0
  init_legacy_continuation init --continue-legacy --yes || return "$?"
  operator_env jq -e --argjson uid "$(operator_uid)" \
    '.schema == 1 and .scope == "normal-reconcile" and .operatorUid == $uid and (.instanceUuid | length > 0)' \
    "$grant" >/dev/null || die 'public legacy continuation did not publish its limited grant'
  if [ ! -e "$STATE_ROOT/legacy-continuation.sha256" ]; then
    write_fixture_value "$STATE_ROOT/legacy-continuation.sha256" \
      "$(operator_env sha256sum "$grant" | awk '{print $1}')" || return "$?"
  fi
  assert_legacy_continuation
  verify_legacy_full_removal_refusal
  init_legacy_continuation init --yes || return "$?"
  assert_legacy_continuation
}

verify_legacy_full_removal_refusal() {
  local refusal="$STATE_ROOT/continuation-full-removal-refusal.log"
  if operator_yard teardown --full --yes > "$refusal" 2>&1; then
    die 'continuation-only installation unexpectedly accepted full removal'
  fi
  grep -Fq 'legacy continuation grants no full removal ownership' "$refusal" \
    || die 'full removal did not identify the continuation-only boundary'
  assert_project_owned
  incus info "$INSTANCE" --project "$PROJECT" >/dev/null \
    || die 'full removal refusal changed the retained legacy instance'
  assert_legacy_continuation
}

prepare_fixture() {
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
}

prepare_candidate() {
  local old_load
  prepare_fixture

  p0_capacity_reset_build_cache
  info "packaging local candidate $CANDIDATE_VERSION"
  if [ -f "$ROOT/.subyard-acceptance/candidate.json" ]; then
    install -d "$RELEASE_ROOT"
    cp -a "$ROOT/.subyard-acceptance/release/." "$RELEASE_ROOT/"
  else
    "$ROOT/dev/package-engine.sh" --output-dir "$RELEASE_ROOT" --version "$CANDIDATE_VERSION" >/dev/null
  fi
  chmod -R a+rX "$RELEASE_ROOT"

  info 'installing exact published v0.11.0 runtime'
  prepare_compatible_release
  curl -fsSL --proto '=https' --tlsv1.2 \
    "https://github.com/Subyard/Subyard/releases/download/v$OLD_VERSION/subyard-install.sh" \
    -o "$INSTALLER"
  [ "$(sha256sum "$INSTALLER" | awk '{print $1}')" = "$OLD_INSTALLER_SHA256" ] \
    || die 'published v0.11.0 installer checksum changed'
  chmod 0755 "$INSTALLER"
  operator_env env YARD_RELEASE_VERSION="$OLD_VERSION" \
    "$INSTALLER" --version "$OLD_VERSION" --yes
  [ "$(operator_env sha256sum "$OPERATOR_HOME/.subyard/releases/$OLD_VERSION/subyard-$OLD_VERSION-linux-amd64.tar.gz" | awk '{print $1}')" = "$OLD_BUNDLE_SHA256" ] \
    || die 'published v0.11.0 runtime archive checksum changed'

  printf '%s\n' "$MARKER" > "$PROJECT_MUTATION_ARMED"
  incus project create "$PROJECT" \
    -c features.images=false -c user.subyard.p0-power-systemd="$MARKER" >/dev/null
  operator_yard init --yes
  operator_yard start --yes
  old_load=loaded
  OLD_RELEASE_TARGET="$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/current")"
  [ "$OLD_RELEASE_TARGET" = "releases/$OLD_VERSION-${OLD_BUNDLE_SHA256:0:12}" ] \
    || die 'published predecessor runtime has an unexpected artifact identity'
  write_fixture_value "$OLD_RELEASE_TARGET_STATE" "$OLD_RELEASE_TARGET"
  OLD_UNIT_FIXTURE="$OPERATOR_HOME/.subyard/runtime/$OLD_RELEASE_TARGET/config/systemd/subyard-power-reconcile.service.in"
  assert_runtime_state "$OLD_VERSION" "$OLD_UNIT_FIXTURE" "$old_load" released \
    "$OLD_RELEASE_TARGET"
  ok 'actual supported v0.11.0 runtime and live power unit verified'

  info 'running the candidate-owned supported update from the published predecessor'
  verify_bridge_plan_is_read_only
  operator_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" \
    "$RELEASE_ROOT/subyard-install.sh" --version "$CANDIDATE_VERSION" --yes
  CANDIDATE_RELEASE_TARGET="$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/current")"
  write_fixture_value "$CANDIDATE_RELEASE_TARGET_STATE" "$CANDIDATE_RELEASE_TARGET"
  assert_candidate_state
  assert_runtime_links "$CANDIDATE_RELEASE_TARGET" "$OLD_RELEASE_TARGET"
  ok 'supported update reached the V2 power fixed point'
}

prepare_compatible_release() {
  local name digest
  mkdir -p "$COMPATIBLE_RELEASE_ROOT" "$COMPATIBLE_RUNTIME_ROOT"
  # Immutable official e92 assets, not a rebuilt compatibility fixture.
  while read -r digest name; do
    curl -fsSL --proto '=https' --tlsv1.2 --connect-timeout 15 --max-time 180 \
      "https://github.com/Subyard/Subyard/releases/download/v$COMPATIBLE_VERSION/$name" \
      -o "$COMPATIBLE_RELEASE_ROOT/$name"
    [ "$(sha256sum "$COMPATIBLE_RELEASE_ROOT/$name" | awk '{print $1}')" = "$digest" ] \
      || die 'published compatible asset checksum changed'
  done <<'ASSETS'
1fee6251856ede2c1cbd0c01f48d226fbce64915f712df97085d35abb4991282 subyard-0.21.0-linux-amd64.tar.gz
76a91b2233853cd4f75d20e7d8cc3ea468c2cca2782aad08871af12269352cbf subyard-0.21.0-linux-amd64.tar.gz.sha256
fb17c012f72b45e44ed563f286e734ff38d4b3e473512c04d7fd2fde50a289e3 subyard-0.21.0-linux-amd64.tar.gz.manifest.json
287e85b4c8aae826d27c2f9560e98358ba5a0513cc992c28685dc8b6eb7475e2 subyard-0.21.0-linux-amd64.tar.gz.provenance.json
ASSETS
  tar -xzf "$COMPATIBLE_RELEASE_ROOT/subyard-0.21.0-linux-amd64.tar.gz" -C "$COMPATIBLE_RUNTIME_ROOT"
  (cd "$COMPATIBLE_RUNTIME_ROOT" && sha256sum --check runtime-files.sha256 >/dev/null) \
    || die 'published compatible runtime manifest failed'
  chmod -R a+rX "$COMPATIBLE_RELEASE_ROOT" "$COMPATIBLE_RUNTIME_ROOT"
  # This executable probe belongs to the allocated guest, before cold legacy setup.
  printf '%s\n' '{"schemaVersion":2,"mode":"runtime-activation-capabilities"}' | \
    operator_env timeout 30 bash -c 'ulimit -f 64; exec "$@"' _ \
      env SUBYARD_REPOSITORY_ROOT="$COMPATIBLE_RUNTIME_ROOT" \
      "$COMPATIBLE_RUNTIME_ROOT/bin/yard-engine" _release-transition \
      > "$STATE_ROOT/compatible-capabilities.json"
  verify_compatible_capabilities "$STATE_ROOT/compatible-capabilities.json"
}

# A supported V2 source may have no V1 state; preserve that absence as well as bytes.
assert_published_v1_history_unchanged() {
  local path="$OPERATOR_HOME/.config/subyard/migrations/state.json"
  if [ "$(cat "$STATE_ROOT/refusal-3.mode")" = absent ]; then
    operator_env test ! -e "$path" && operator_env test ! -L "$path" \
      || die 'transition created retired V1 state'
  else
    [ "$(operator_env stat -c '%a:%u:%g:%d:%i' "$path")" = "$(cat "$STATE_ROOT/refusal-3.mode")" ] \
      && operator_env cat "$path" | cmp -s "$STATE_ROOT/refusal-3.bytes" - \
      || die 'transition changed retained V1 state'
  fi
}

# The release smoke reuses the same disposable operator, durable reboot state and
# marker-guarded host-runtime restoration without the historical migration matrix.
if [ "${BASH_SOURCE[0]}" != "$0" ]; then return 0; fi

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
      printf 'ok: exact v0.11.0 power reconciler fixture is clean\n'
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
case "$MODE" in
  run|prepare)
    incus image info subyard-e2e-debian-13-cloud-container --project default >/dev/null \
      || die 'the P0 Debian image cache is required'
    prepare_candidate
    exercise_activation_only_repair
    establish_legacy_continuation
    if [ "$MODE" = prepare ]; then
      record_reboot_baseline
      write_fixture_value "$PHASE_STATE" candidate-ready
      PRESERVE_FIXTURE=1
      printf 'ok: exact published v0.11.0 update is ready for reboot verification\n'
      exit 0
    fi
    finish_candidate_flow
    printf 'ok: exact published v0.11.0 upgrade and authentic v0.21.0 rollback/roll-forward passed\n'
    ;;
  resume)
    assert_state_root
    CLEANUP_ARMED=1
    assert_fixture_phase candidate-ready
    assert_post_reboot_candidate
    info 'checking repeatable init with limited legacy authority and boot reconciliation'
    verify_historical_init_boundary
    assert_candidate_state
    record_reboot_baseline
    write_fixture_value "$PHASE_STATE" candidate-reconciled
    PRESERVE_FIXTURE=1
    printf 'ok: candidate survived reboot, retained limited legacy authority and repeated boot reconciliation; ready for second reboot\n'
    ;;
  finish)
    assert_state_root
    CLEANUP_ARMED=1
    assert_fixture_phase candidate-reconciled
    assert_post_reboot_candidate
    finish_candidate_flow
    printf 'ok: exact published v0.11.0 upgrade survived two reboots and authentic v0.21.0 rollback/roll-forward\n'
    ;;
esac
