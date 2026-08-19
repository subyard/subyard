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
CLEANUP_ARMED=0
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
        sudo -n install -o root -g root \
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
  [ "$restored_active" = "$(cat "$BEFORE_ROOT/unit-active")" ] || return 1
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
    active|inactive) ;;
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

assert_release_state() {
  local version="$1" layout="$2" unit_template="$3" expected_load="$4"
  [ "$(operator_yard --version)" = "yard $version" ] \
    || die "active runtime is not yard $version"
  operator_env jq -e --argjson layout "$layout" '
    .layout == $layout and
    .applied == (if $layout == 4 then
      ["migrate-test-yard-owner", "refresh-test-vm-broker", "refresh-power-reconciler"]
    else
      ["migrate-test-yard-owner", "refresh-test-vm-broker", "refresh-power-reconciler",
       "repair-power-reconciler-systemd-compat"]
    end)
  ' "$OPERATOR_HOME/.config/subyard/migrations/state.json" >/dev/null \
    || die "migration state did not converge to layout $layout"
  assert_unit_matches "$unit_template" "$expected_load" "$layout"
  operator_env cmp "$OPERATOR_HOME/.subyard/runtime/current/bin/yard-engine" "$RECONCILER" \
    || die 'installed power reconciler executable is stale'
  [ "$(incus config get "$INSTANCE" boot.autostart --project "$PROJECT")" = false ] \
    || die 'update changed boot.autostart=false'
  [ "$(incus config get "$INSTANCE" user.subyard.desired_power --project "$PROJECT")" = \
    running ] || die 'update changed desired=running'
  operator_yard check
}

assert_candidate_transaction() {
  local phase="$1" transaction
  transaction="$(operator_env bash -c '
    set -euo pipefail
    for journal in "$1"/*/transaction.json; do
      [ -f "$journal" ] || continue
      [ "$(jq -r .toRelease "$journal")" = "$2" ] || continue
      printf "%s\n" "$journal"
    done
  ' _ "$OPERATOR_HOME/.config/subyard/migrations/transactions" "$CANDIDATE_VERSION")"
  [ "$(wc -l <<<"$transaction")" = 1 ] || die 'candidate migration journal is missing or ambiguous'
  if operator_env jq -e --arg phase "$phase" '
    .phase == $phase and .fromLayout == 4 and .toLayout == 5 and
    (.operations | map([.migrationId, .operationId, .kind])) == [
      ["migrate-test-yard-owner", "test-yard-owner", "test-yard-owner-v1"],
      ["migrate-test-yard-owner", "test-yard-route-consumers", "test-yard-route-consumers-v1"],
      ["refresh-test-vm-broker", "test-vm-broker-runtime", "test-vm-broker-runtime-v1"],
      ["refresh-power-reconciler", "power-reconciler-runtime", "power-reconciler-runtime-v1"],
      ["repair-power-reconciler-systemd-compat", "power-reconciler-systemd-compat",
       "power-reconciler-systemd-compat-v1"]
    ] and
    if $phase == "committed" then
      all(.operations[]; .phase == "committed")
    else
      $phase == "rolled-back" and
      all(.operations[0:4][]; .phase == "committed") and
      .operations[4].phase == "rolled-back"
    end
  ' "$transaction" >/dev/null; then
    return
  fi
  operator_env jq '{
    phase, fromLayout, toLayout,
    operations: [.operations[] | {migrationId, operationId, kind, phase}]
  }' "$transaction" >&2 || true
  die 'candidate migration journal does not match the compatibility-repair transaction'
}

assert_runtime_links() {
  local current="$1" previous="$2" runtime="$OPERATOR_HOME/.subyard/runtime"
  [ "$(operator_env readlink "$runtime/current")" = "$current" ] \
    && [ "$(operator_env readlink "$runtime/previous")" = "$previous" ] \
    || die "runtime links are not current=$current previous=$previous"
}

verify_update_check_is_read_only() {
  local before after
  before="$(operator_env bash -c '
    set -euo pipefail
    sha256sum "$1" "$2"
    readlink "$3"
    if [ -L "$4" ]; then readlink "$4"; else printf "absent\n"; fi
  ' _ "$OPERATOR_HOME/.config/subyard/migrations/state.json" "$UNIT" \
    "$OPERATOR_HOME/.subyard/runtime/current" "$OPERATOR_HOME/.subyard/runtime/previous")"
  operator_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" \
    "$OPERATOR_HOME/.local/bin/yard" update --check \
      --version "$CANDIDATE_VERSION" --yes >/dev/null
  after="$(operator_env bash -c '
    set -euo pipefail
    sha256sum "$1" "$2"
    readlink "$3"
    if [ -L "$4" ]; then readlink "$4"; else printf "absent\n"; fi
  ' _ "$OPERATOR_HOME/.config/subyard/migrations/state.json" "$UNIT" \
    "$OPERATOR_HOME/.subyard/runtime/current" "$OPERATOR_HOME/.subyard/runtime/previous")"
  [ "$before" = "$after" ] || die 'yard update --check mutated runtime, migration state or unit'
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
  run) ;;
  *) die 'expected run or clean mode' ;;
esac
for command in curl go incus jq sed sha256sum systemd-analyze; do
  command -v "$command" >/dev/null 2>&1 || die "$command is required"
done
incus info >/dev/null || die 'initialized Incus is required'
incus image info subyard-e2e-debian-13-cloud-container --project default >/dev/null \
  || die 'the P0 Debian image cache is required'
[ "$(sha256sum "$OLD_UNIT_FIXTURE" | awk '{print $1}')" = \
  36692bddc0de036c9ec7393b86a6883e7cde59f33a25e352ec2ee7b694890aaf ] \
  || die 'the exact v0.8.0 unit fixture changed'
[ ! -e "$STATE_ROOT" ] && [ ! -L "$STATE_ROOT" ] \
  || die "fixture state already exists: $STATE_ROOT"
! id "$OPERATOR" >/dev/null 2>&1 || die "fixture operator already exists: $OPERATOR"
[ ! -e "$OPERATOR_HOME" ] && [ ! -L "$OPERATOR_HOME" ] \
  || die "fixture operator home already exists: $OPERATOR_HOME"
sudo -n test ! -e "$SUDOERS" && sudo -n test ! -L "$SUDOERS" \
  || die "fixture sudoers path already exists: $SUDOERS"
! incus project show "$PROJECT" >/dev/null 2>&1 || die "fixture project already exists: $PROJECT"

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
assert_release_state "$OLD_VERSION" 4 "$OLD_UNIT_FIXTURE" "$old_load"
OLD_RELEASE_TARGET="$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/current")"
ok 'published v0.8.0 layout and installed incompatible unit reproduced'

info 'running ordinary yard update to the local candidate'
verify_update_check_is_read_only
operator_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" \
  "$OPERATOR_HOME/.local/bin/yard" update --version "$CANDIDATE_VERSION" --yes
assert_release_state "$CANDIDATE_VERSION" 5 \
  "$ROOT/config/systemd/subyard-power-reconcile.service.in" loaded
CANDIDATE_RELEASE_TARGET="$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/current")"
assert_runtime_links "$CANDIDATE_RELEASE_TARGET" "$OLD_RELEASE_TARGET"
assert_candidate_transaction committed
ok 'ordinary update applied the append-only compatibility migration'

info 'rolling back to the exact published v0.8.0 runtime and unit'
operator_yard update --rollback --yes
assert_release_state "$OLD_VERSION" 4 "$OLD_UNIT_FIXTURE" "$old_load"
assert_runtime_links "$OLD_RELEASE_TARGET" "$CANDIDATE_RELEASE_TARGET"
assert_candidate_transaction rolled-back
ok 'rollback restored the exact v0.8.0 runtime and unit'

info 'rolling forward to the local candidate again'
operator_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" \
  "$OPERATOR_HOME/.local/bin/yard" update --version "$CANDIDATE_VERSION" --yes
assert_release_state "$CANDIDATE_VERSION" 5 \
  "$ROOT/config/systemd/subyard-power-reconcile.service.in" loaded
assert_runtime_links "$CANDIDATE_RELEASE_TARGET" "$OLD_RELEASE_TARGET"
assert_candidate_transaction committed
ok 'roll-forward restored the compatible candidate runtime and unit'

printf 'ok: exact published v0.8.0 power reconciler update, rollback and roll-forward passed\n'
