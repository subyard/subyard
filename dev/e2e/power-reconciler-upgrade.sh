#!/usr/bin/env bash
# Exact published v0.11.0 -> local candidate migration acceptance on an allocated P0 worker VM.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
MODE="${1:-run}"
TOKEN="${2:-}"
# shellcheck source=dev/e2e/lib-p0-capacity.sh
. "$ROOT/dev/e2e/lib-p0-capacity.sh"
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
    -u SUDO_USER -u SUDO_UID -u SUDO_GID -u SUDO_COMMAND \
    HOME="$OPERATOR_HOME" USER="$OPERATOR" LOGNAME="$OPERATOR" SHELL=/bin/bash \
    PATH="$OPERATOR_HOME/.local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" \
    GOCACHE="$OPERATOR_HOME/.cache/go-build" GOMODCACHE="$OPERATOR_HOME/go/pkg/mod" \
    XDG_RUNTIME_DIR="/run/user/$uid" \
    DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$uid/bus" \
    bash -c 'cd "$HOME" && exec "$@"' _ "$@"
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
  local template="$1" output="$2" runtime_root contracts
  local -a reader=()
  case "$template" in
    "$OPERATOR_HOME/.subyard/runtime/releases/"*/config/systemd/subyard-power-reconcile.service.in)
      reader=(operator_env)
      runtime_root="${template%/config/systemd/subyard-power-reconcile.service.in}" ;;
    "$OLD_UNIT_FIXTURE") runtime_root='' ;;
    "$ROOT/config/systemd/subyard-power-reconcile.service.in")
      runtime_root="$OPERATOR_HOME/.subyard/runtime/${CANDIDATE_RELEASE_TARGET:-}" ;;
    *) die 'unsupported power unit template context' ;;
  esac
  "${reader[@]}" sed "s|@SUBYARD_POWER_RECONCILER@|$RECONCILER|g" "$template" > "$output"
  [ "$template" != "$OLD_UNIT_FIXTURE" ] || return 0
  # Current templates bind to the captured immutable candidate, including when
  # the controller-owned source template is used to verify rollback's boot unit.
  case "$runtime_root" in
    "$OPERATOR_HOME/.subyard/runtime/releases/"?*) ;;
    *) die 'candidate power unit runtime is unavailable' ;;
  esac
  operator_env test -f "$runtime_root/bin/yard-engine" \
    && operator_env test ! -L "$runtime_root/bin/yard-engine" \
    && operator_env test -x "$runtime_root/bin/yard-engine" \
    || die 'candidate power unit engine is missing or unsafe'
  contracts="$(operator_env env SUBYARD_REPOSITORY_ROOT="$runtime_root" \
    SUBYARD_CONFIG_HOME="$OPERATOR_HOME/.config/subyard" \
    "$runtime_root/bin/yard-engine" _publication-contracts)" \
    || die 'candidate power unit publication snapshot is unavailable'
  [[ "$contracts" =~ ^[A-Za-z0-9+/]+={0,2}$ ]] || die 'candidate power unit publication snapshot is invalid'
  printf '\n[Service]\nEnvironment=SUBYARD_PUBLICATION_CONTRACTS=%s\n' "$contracts" >> "$output"
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

assert_v2_transition() {
  local direction="$1" target="${2#releases/}" expected_catalog="${3:-}"
  local runtime="$OPERATOR_HOME/.subyard/runtime" artifact_digest registry_digest record
  artifact_digest="$(operator_env sha256sum "$runtime/releases/$target/runtime-files.sha256" | awk '{print $1}')" \
    || die 'target artifact manifest is unavailable'
  registry_digest="$(operator_env sha256sum "$runtime/$CANDIDATE_RELEASE_TARGET/config/release-transition.json" | awk '{print $1}')" \
    || die 'candidate transition owner registry is unavailable'
  operator_env jq -e '
    .schemaVersion == 2 and
    .domains["owner-registration"] == {
      "epoch": 2, "applied": ["canonicalize-test-yard-owner-v2"]
    } and
    .domains["power-metadata"] == {"epoch": 2, "applied": ["introduce-owner-publication-state-v1"]} and
    .domains["project-state"] == {"epoch": 1, "applied": []} and
    .domains.settings == {
      "epoch": 3, "applied": ["canonicalize-test-vms-settings-v2", "localize-ssh-port-settings-v3"]
    }
  ' "$OPERATOR_HOME/.config/subyard/release-transition/v2/ledger.json" >/dev/null \
    || {
      # Capture allowlisted migration facts before marker-owned cleanup removes them.
      for record in ledger.json history-checkpoint.json; do
        printf 'power-reconciler-upgrade: migration record=%s\n' "$record" >&2
        operator_env jq -c '{schemaVersion, registryDigest, legacyProjection, domains}' \
          "$OPERATOR_HOME/.config/subyard/release-transition/v2/$record" >&2 \
          || printf 'power-reconciler-upgrade: migration record unavailable=%s\n' "$record" >&2
      done
      die 'candidate v2 migration ledger is not at its exact per-domain fixed point'
    }
  operator_env jq -e \
    --arg direction "$direction" --arg target "$target" \
    --arg artifact "$artifact_digest" --arg registry "$registry_digest" \
    --arg catalog "$expected_catalog" '
    .schemaVersion == 2 and .checkpoint == "complete" and
    .goal == {"target": $target, "direction": $direction} and
    .releases.target == $target and
    (has("rollbackTarget") | not) and
    .artifactDigest == $artifact and .registryDigest == $registry and
    (.catalogDigest | type == "string" and test("^[a-f0-9]{64}$")) and
    ($direction != "activate-previous" or .catalogDigest == $catalog) and
    (.steps | type == "array") and all(.steps[]; .checkpoint == "verified")
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
  if [ -f "$STATE_ROOT/legacy-continuation.sha256" ]; then assert_legacy_continuation; fi
  assert_runtime_state "$CANDIDATE_VERSION" \
    "$OPERATOR_HOME/.subyard/runtime/$CANDIDATE_RELEASE_TARGET/config/systemd/subyard-power-reconcile.service.in" loaded candidate \
    "$CANDIDATE_RELEASE_TARGET"
  assert_v2_transition activate-target "$CANDIDATE_RELEASE_TARGET"
}

assert_activation_only_journal() {
  local checkpoint="$1" transaction="$2" authorization="$3"
  local target="${CANDIDATE_RELEASE_TARGET#releases/}"
  operator_env jq -e \
    --arg checkpoint "$checkpoint" \
    --arg transaction "$transaction" \
    --arg authorization "$authorization" \
    --arg target "$target" '
      .schemaVersion == 2 and
      .checkpoint == $checkpoint and
      .transaction == $transaction and
      .authorizationDigest == $authorization and
      .goal == {"target": $target, "direction": "activate-target"} and
      .releases.from == $target and .releases.target == $target and
      (.steps | length) == 0
    ' "$V2_JOURNAL" >/dev/null \
    || die "activation-only journal is not at checkpoint $checkpoint"
}

assert_activation_ledger_unchanged() {
  sudo -n cmp "$ACTIVATION_LEDGER_BASELINE" "$V2_LEDGER" \
    || die 'activation-only repair changed the exact v2 ledger bytes'
}

install_activation_reconcile_fault() {
  local temporary
  temporary="$(mktemp "$STATE_ROOT/.activation-systemctl.XXXXXX")"
  printf '%s\n' \
    '#!/bin/sh' \
    'set -eu' \
    "journal=$V2_JOURNAL" \
    "probe=$ACTIVATION_FAULT_PROBE" \
    "delegate=$ACTIVATION_SYSTEMCTL_DELEGATE" \
    'if [ "${1:-}" = show ] &&' \
    '  [ "${2:-}" = subyard-power-reconcile.service ] &&' \
    '  /usr/bin/jq -e ".schemaVersion == 2 and .checkpoint == \"reconciling\"" "$journal" >/dev/null 2>&1; then' \
    '  failures=0' \
    '  [ ! -r "$probe" ] || failures=$(wc -l < "$probe")' \
    '  if [ "$failures" = 0 ]; then' \
    '    printf "%s\n" "injected activation reconciler post-apply interruption" >&2' \
    '    printf "%s\n" "reconciling post-apply failure" >> "$probe"' \
    '    exit 75' \
    '  fi' \
    'fi' \
    'exec "$delegate" "$@"' \
    > "$temporary"
  sudo -n install -o "$OPERATOR" -g "$OPERATOR" -m 0755 \
    "$temporary" "$ACTIVATION_SYSTEMCTL_WRAPPER"
  find "$temporary" -delete
}

exercise_activation_only_repair() {
  local completed_transaction repair_transaction repair_authorization
  local candidate_unit="$STATE_ROOT/candidate-activation.service"
  local drift_unit="$STATE_ROOT/drifted-activation.service"
  local unconfirmed_output unconfirmed_rc interrupted_output interrupted_rc
  local fault_count resumed_output resumed_rc

  load_release_targets
  completed_transaction="$(operator_env jq -er '.transaction' "$V2_JOURNAL")" \
    || die 'completed candidate transaction is unavailable'
  sudo -n install -m 0600 "$V2_LEDGER" "$ACTIVATION_LEDGER_BASELINE"
  materialize_unit "$OPERATOR_HOME/.subyard/runtime/$CANDIDATE_RELEASE_TARGET/config/systemd/subyard-power-reconcile.service.in" "$candidate_unit"
  materialize_unit "$OLD_UNIT_FIXTURE" "$drift_unit"
  sudo -n install -o root -g root -m 0644 "$drift_unit" "$UNIT"
  sudo -n systemctl daemon-reload
  sudo -n cmp "$drift_unit" "$UNIT" >/dev/null \
    || die 'release-owned activation drift was not installed exactly'
  if sudo -n cmp -s "$candidate_unit" "$UNIT"; then
    die 'release-owned activation resource did not enter drift'
  fi

  sudo -n install -m 0600 "$V2_JOURNAL" "$ACTIVATION_JOURNAL_BASELINE"
  set +e
  unconfirmed_output="$(
    operator_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" \
      "$OPERATOR_HOME/.local/bin/yard" update --version "$CANDIDATE_VERSION" \
      </dev/null 2>&1
  )"
  unconfirmed_rc=$?
  set -e
  [ "$unconfirmed_rc" = 1 ] \
    && [ "$(grep -Fc 'confirmation required: interactive terminal required' \
      <<<"$unconfirmed_output")" = 1 ] \
    || die "unconfirmed activation repair did not stop at confirmation: $unconfirmed_output"
  sudo -n cmp "$ACTIVATION_JOURNAL_BASELINE" "$V2_JOURNAL" \
    || die 'unconfirmed activation repair changed the exact journal bytes'
  sudo -n cmp "$drift_unit" "$UNIT" \
    || die 'unconfirmed activation repair changed the release-owned resource'
  assert_activation_ledger_unchanged

  install_activation_reconcile_fault
  set +e
  interrupted_output="$(
    operator_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" \
      "$OPERATOR_HOME/.local/bin/yard" update --version "$CANDIDATE_VERSION" --yes \
      </dev/null 2>&1
  )"
  interrupted_rc=$?
  set -e
  [ "$interrupted_rc" -ne 0 ] \
    || die 'injected activation reconciliation unexpectedly completed'
  if ! operator_env test -f "$ACTIVATION_FAULT_PROBE"; then
    printf '%s\n' "$interrupted_output" >&2
    die 'activation reconciliation did not reach the injected post-apply checkpoint'
  fi
  fault_count="$(operator_env wc -l "$ACTIVATION_FAULT_PROBE" | awk '{print $1}')"
  [ "$fault_count" = 1 ] \
    || die "activation reconciliation injected failures=$fault_count, want exactly 1"
  ! grep -Fq 'confirmation required: interactive terminal required' <<<"$interrupted_output" \
    || die 'authorized activation reconciliation requested confirmation again'
  operator_env find "$ACTIVATION_SYSTEMCTL_WRAPPER" -delete

  repair_transaction="$(operator_env jq -er '.transaction' "$V2_JOURNAL")" \
    || die 'activation repair transaction is unavailable after interruption'
  repair_authorization="$(operator_env jq -er '.authorizationDigest' "$V2_JOURNAL")" \
    || die 'activation repair authorization is unavailable after interruption'
  [ "$repair_transaction" != "$completed_transaction" ] \
    || die 'activation drift reused the completed migration transaction'
  assert_activation_only_journal reconciling "$repair_transaction" "$repair_authorization"
  assert_activation_ledger_unchanged
  sudo -n cmp "$candidate_unit" "$UNIT" >/dev/null \
    || die 'interrupted reconciler did not restore the exact candidate activation resource'

  if resumed_output="$(
    operator_yard update --version "$CANDIDATE_VERSION" </dev/null 2>&1
  )"; then
    :
  else
    resumed_rc=$?
    printf '%s\n' "$resumed_output" >&2
    die "activation-only same-version update did not resume (exit=$resumed_rc)"
  fi
  ! grep -Eq 'Proceed\?|confirmation required' <<<"$resumed_output" \
    || die 'activation-only resume requested a second confirmation'
  assert_activation_only_journal complete "$repair_transaction" "$repair_authorization"
  assert_activation_ledger_unchanged
  assert_candidate_state
  assert_runtime_links "$CANDIDATE_RELEASE_TARGET" "$OLD_RELEASE_TARGET"
  ok 'same-version activation-only repair resumed without replaying migration history'
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

assert_legacy_continuation() {
  local grant="$OPERATOR_HOME/.config/subyard/ownership/legacy-continuations/default.json"
  [ -z "$(incus project get "$PROJECT" user.subyard.installation)" ] \
    || die 'legacy continuation acquired destructive installation ownership'
  [ "$(operator_env stat -c '%a' "$grant")" = 600 ] \
    && [ "$(operator_env sha256sum "$grant" | awk '{print $1}')" = \
      "$(cat "$STATE_ROOT/legacy-continuation.sha256")" ] \
    || die 'protected continuation grant changed across init, reboot or release links'
}

establish_legacy_continuation() {
  local grant="$OPERATOR_HOME/.config/subyard/ownership/legacy-continuations/default.json"
  operator_yard init --continue-legacy --yes
  operator_env jq -e --argjson uid "$(operator_uid)" \
    '.schema == 1 and .scope == "normal-reconcile" and .operatorUid == $uid and (.instanceUuid | length > 0)' \
    "$grant" >/dev/null || die 'public legacy continuation did not publish its limited grant'
  write_fixture_value "$STATE_ROOT/legacy-continuation.sha256" \
    "$(operator_env sha256sum "$grant" | awk '{print $1}')"
  assert_legacy_continuation
  verify_legacy_full_removal_refusal
  operator_yard init --yes
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

verify_compatible_capabilities() {
  local response="$1"
  [ "$(wc -c < "$response")" -le 65536 ] \
    && jq -e 'keys == ["activationOwners", "activationReconciliationOwned", "schemaVersion"] and
      .activationReconciliationOwned == false and .schemaVersion == 2 and
      (.activationOwners | sort) == ["core-guest-runtime", "git-fetch-runtime", "github-runtime",
        "host-power", "key-tools-runtime", "openlit-runtime", "orca-runtime",
        "test-vm-broker", "test-yard-route-consumers"]' \
      "$response" >/dev/null \
    || die 'published compatible runtime lacks exact activation capabilities'
}

capture_legacy_native_policy() {
  local output="$1" revisions
  revisions="$(jq -s '[.[].instance_publication.revision_key // empty]' "$ROOT"/config/profiles/*/profile.json)"
  incus query "/1.0/projects/$PROJECT" > "$output.project"
  incus query "/1.0/profiles/default?project=$PROJECT" > "$output.profile"
  incus query "/1.0/instances/$INSTANCE?project=$PROJECT" > "$output.instance"
  jq -S -n --argjson revisions "$revisions" \
    --slurpfile project "$output.project" --slurpfile profile "$output.profile" \
    --slurpfile instance "$output.instance" '
      def canonical: with_entries(
        if (.key as $key | $revisions | index($key)) != null then
          if (.value | test("^0:[a-f0-9]{64}:")) then .value |= sub("^0:[a-f0-9]{64}:"; "0:RELEASE:")
          else error("invalid disabled publication") end else . end);
      $instance[0].config as $config |
      if ($config["volatile.uuid"] | type != "string" or length == 0) then error("missing native UUID") else
      {project: $project[0].config, profile: {config: $profile[0].config, devices: $profile[0].devices},
       instance: {profiles: $instance[0].profiles, config: ($config | canonical),
         expanded_config: ($instance[0].expanded_config | canonical),
         devices: $instance[0].devices, expanded_devices: $instance[0].expanded_devices}}
      end' > "$output"
  chmod 0600 "$output" "$output.project" "$output.profile" "$output.instance"
}

assert_legacy_native_policy() {
  capture_legacy_native_policy "$STATE_ROOT/native-policy.current"
  cmp -s "$STATE_ROOT/native-policy.before" "$STATE_ROOT/native-policy.current" \
    || die 'compatible transition changed the exact legacy native policy or identity'
  assert_legacy_continuation
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

verify_legacy_rollback_refusal() {
  local path index=0 grant="$OPERATOR_HOME/.config/subyard/ownership/legacy-continuations/default.json"
  local -a paths=("$V2_STATE_ROOT/history-checkpoint.json" "$V2_LEDGER" "$V2_JOURNAL"
    "$OPERATOR_HOME/.config/subyard/migrations/state.json" "$grant")
  capture_legacy_native_policy "$STATE_ROOT/native-policy.before"
  for path in "${paths[@]}"; do
    # Fresh v0.11.0 is V2: retain V1 evidence if present, never manufacture it.
    if [ "$index" = 3 ] && operator_env test ! -e "$path" && operator_env test ! -L "$path"; then
      printf 'absent\n' > "$STATE_ROOT/refusal-$index.mode"
    else
      operator_env stat -c '%a:%u:%g:%d:%i' "$path" > "$STATE_ROOT/refusal-$index.mode"
      operator_env cat "$path" > "$STATE_ROOT/refusal-$index.bytes"
    fi
    index=$((index + 1))
  done
  if operator_yard update --rollback --yes > "$STATE_ROOT/predecessor-rollback.log" 2>&1; then
    die 'incompatible published v0.11.0 rollback unexpectedly succeeded'
  fi
  grep -Fq 'code=rollback-incompatible' "$STATE_ROOT/predecessor-rollback.log" \
    || die 'published predecessor rollback failed for an unexpected reason'
  assert_runtime_links "$CANDIDATE_RELEASE_TARGET" "$OLD_RELEASE_TARGET"
  index=0
  for path in "${paths[@]}"; do
    if [ "$index" = 3 ]; then
      assert_published_v1_history_unchanged
    else
      [ "$(operator_env stat -c '%a:%u:%g:%d:%i' "$path")" = "$(cat "$STATE_ROOT/refusal-$index.mode")" ] \
        && operator_env cat "$path" | cmp -s "$STATE_ROOT/refusal-$index.bytes" - \
        || die 'refused rollback changed protected history or grant'
    fi
    index=$((index + 1))
  done
  assert_legacy_native_policy
  ok 'incompatible published v0.11.0 rollback refused without mutation'
}

check_compatible_transition() {
  local version="$1" release_root="$2" target="${3#releases/}" active="${4#releases/}"
  operator_env env YARD_RELEASE_BASE_URL="file://$release_root" \
    "$OPERATOR_HOME/.local/bin/yard" update --version "$version" --check --json \
    > "$STATE_ROOT/compatible-check.json"
  jq -e --arg target "$target" --arg active "$active" '
    .outcome.target == $target and .outcome.active == $active and
    .outcome.status == "migration-required" and .outcome.code == "transition-required" and
    .outcome.reachedGoal == false and .assessment.changed == true and
    (.plan | type == "string" and startswith("plan-v1-")) and
    ((.blockers // []) | type == "array" and length == 0)
  ' "$STATE_ROOT/compatible-check.json" >/dev/null \
    || die 'public update inspection did not authorize the exact compatible target'
}

finish_candidate_flow() {
  local rollback_catalog rollback_ledger runtime="$OPERATOR_HOME/.subyard/runtime"
  load_release_targets
  verify_legacy_rollback_refusal
  info 'retaining the authentic published v0.21.0 runtime through verified public updates'
  check_compatible_transition "$COMPATIBLE_VERSION" "$COMPATIBLE_RELEASE_ROOT" \
    "$COMPATIBLE_RELEASE_TARGET" "$CANDIDATE_RELEASE_TARGET"
  operator_env env YARD_RELEASE_BASE_URL="file://$COMPATIBLE_RELEASE_ROOT" \
    "$OPERATOR_HOME/.local/bin/yard" update --version "$COMPATIBLE_VERSION" --yes
  assert_runtime_links "$COMPATIBLE_RELEASE_TARGET" "$CANDIDATE_RELEASE_TARGET"
  assert_runtime_state "$COMPATIBLE_VERSION" \
    "$runtime/$COMPATIBLE_RELEASE_TARGET/config/systemd/subyard-power-reconcile.service.in" loaded candidate \
    "$COMPATIBLE_RELEASE_TARGET"
  assert_legacy_native_policy
  assert_published_v1_history_unchanged
  check_compatible_transition "$CANDIDATE_VERSION" "$RELEASE_ROOT" \
    "$CANDIDATE_RELEASE_TARGET" "$COMPATIBLE_RELEASE_TARGET"
  operator_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" \
    "$OPERATOR_HOME/.local/bin/yard" update --version "$CANDIDATE_VERSION" --yes
  assert_runtime_links "$CANDIDATE_RELEASE_TARGET" "$COMPATIBLE_RELEASE_TARGET"
  assert_candidate_state
  assert_legacy_native_policy
  rollback_catalog="$(operator_env jq -er '.catalogDigest | select(type == "string" and test("^[a-f0-9]{64}$"))' "$V2_JOURNAL")"
  rollback_ledger="$(operator_env sha256sum "$V2_LEDGER" | awk '{print $1}')"
  info 'rolling back to the exact compatible published v0.21.0 runtime'
  operator_yard update --rollback --yes
  assert_runtime_links "$COMPATIBLE_RELEASE_TARGET" "$CANDIDATE_RELEASE_TARGET"
  assert_runtime_state "$COMPATIBLE_VERSION" \
    "$runtime/$COMPATIBLE_RELEASE_TARGET/config/systemd/subyard-power-reconcile.service.in" loaded candidate \
    "$COMPATIBLE_RELEASE_TARGET"
  assert_published_v1_history_unchanged
  assert_v2_transition activate-previous "$COMPATIBLE_RELEASE_TARGET" "$rollback_catalog"
  [ "$(operator_env sha256sum "$V2_LEDGER" | awk '{print $1}')" = "$rollback_ledger" ] \
    || die 'rollback changed the exact v2 migration ledger bytes'
  assert_legacy_native_policy
  check_compatible_transition "$CANDIDATE_VERSION" "$RELEASE_ROOT" \
    "$CANDIDATE_RELEASE_TARGET" "$COMPATIBLE_RELEASE_TARGET"
  operator_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" \
    "$OPERATOR_HOME/.local/bin/yard" update --version "$CANDIDATE_VERSION" --yes
  assert_runtime_links "$CANDIDATE_RELEASE_TARGET" "$COMPATIBLE_RELEASE_TARGET"
  assert_candidate_state
  assert_legacy_native_policy
  operator_yard init --yes
  assert_legacy_native_policy
  verify_legacy_full_removal_refusal
  assert_legacy_native_policy
  assert_published_v1_history_unchanged
  ok 'authentic compatible rollback and candidate roll-forward retained limited legacy authority'
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
