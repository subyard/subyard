#!/usr/bin/env bash
# Real released-runtime catch-up acceptance on an allocated disposable VM.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
MODE="${1:-auto}"
VM="${SUBYARD_E2E_VM:-}"
RUN_ID="${SUBYARD_E2E_RUN_ID:-}"
OLD_VERSION=0.3.1
MISSED_VERSION=0.4.0
BROKEN_VERSION=0.4.1
FAILED_HOTFIX_VERSION=0.4.2
OLD_INSTALLER_SHA256=3d578aa7200a55973d5e638c249511af949c461a29ee0d148af77d3514449371
CANDIDATE_VERSION="0.4.1-catchup-vm${VM:-unknown}"
RELEASE_040_TARGET=releases/0.4.0-68b9925f6880
RELEASE_041_TARGET=releases/0.4.1-fc5b03078508
RELEASE_042_TARGET=releases/0.4.2-17608894ab09
HOTFIX_TRANSACTION_ID=0c236d864db6a03cb9daac14e09ad97e
FAILED_HOTFIX_TRANSACTION_ID=58d43e4a63e9ceeb4de10005bc5b9b20
if [ "$MODE" = hotfix ] || [ "$MODE" = hotfix-clean ] ||
  [ "$MODE" = hotfix-legacy ] || [ "$MODE" = hotfix-broken-042 ]; then
  CANDIDATE_VERSION=0.4.3
fi
STATE_ROOT="/var/tmp/subyard-release-catchup-vm${VM:-unknown}"
MARKER="subyard-release-catchup-vm${VM:-unknown}"
OPERATOR="subyardmigrate${VM:-x}"
OPERATOR_HOME="$STATE_ROOT/operator-home"
OPERATOR_RUNTIME="$OPERATOR_HOME/.subyard/runtime"
HOTFIX_BACKUP="$OPERATOR_HOME/.subyard/recovery/0.4.1-transaction.before-repair.json"
FAILED_HOTFIX_BACKUP="$OPERATOR_HOME/.subyard/recovery/0.4.2-transaction.before-repair.json"
CANDIDATE_RELEASE="$STATE_ROOT/candidate"
INSTALLER="$STATE_ROOT/subyard-install-0.3.1.sh"
SUDOERS="/etc/sudoers.d/$MARKER"
CONSUMER_PROJECT=subyard
CONSUMER_INSTANCE=yard
LEGACY_PROJECT=subyard-e2e-yard
LEGACY_INSTANCE='yard-e2e-yard'
CURRENT_PROJECT=subyard-test-yard
CURRENT_INSTANCE='yard-test-yard'
IMAGE_ALIAS=subyard-e2e-debian-13-cloud-container
POWER_RECONCILER=/usr/local/libexec/subyard/yard-boot-reconcile
POWER_UNIT=/etc/systemd/system/subyard-power-reconcile.service
PLATFORM_ROOT="$HOME/.cache/subyard-release-catchup-platform-vm${VM:-unknown}"
PLATFORM_STORAGE="$PLATFORM_ROOT/data/incus/storage"
PLATFORM_OWNED=0
CLEANUP_ARMED=0

die() { printf 'release-migration-catch-up: %s\n' "$*" >&2; exit 2; }
info() { printf '  [ .. ] %s\n' "$*"; }
ok() { printf '  [ ok ] %s\n' "$*"; }
host_incus() { sudo -n incus "$@" </dev/null; }
operator_uid() { id -u "$OPERATOR"; }
operator_env() {
  local uid
  uid="$(operator_uid)"
  sudo -n /usr/sbin/runuser -u "$OPERATOR" -- env \
    HOME="$OPERATOR_HOME" USER="$OPERATOR" LOGNAME="$OPERATOR" SHELL=/bin/bash \
    PATH="$OPERATOR_HOME/.local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" \
    XDG_RUNTIME_DIR="/run/user/$uid" \
    DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$uid/bus" \
    "$@"
}
operator_yard() { operator_env "$OPERATOR_HOME/.local/bin/yard" "$@"; }
operator_test_vms_status() {
  operator_yard -Y test-yard test-vms status 2>&1
}

ensure_host_incus() {
  local source=''
  if command -v incus >/dev/null 2>&1 \
    && sudo -n incus info >/dev/null 2>&1 \
    && sudo -n incus storage show default --project default >/dev/null 2>&1; then
    source="$(sudo -n incus storage get default source --project default)"
    if [ "$source" = "$PLATFORM_STORAGE" ]; then
      [ -d "$PLATFORM_ROOT" ] && [ ! -L "$PLATFORM_ROOT" ] \
        && [ "$(cat "$PLATFORM_ROOT/.marker" 2>/dev/null)" = "$MARKER" ] \
        || die "refusing unmarked Incus platform root $PLATFORM_ROOT"
      sudo -n test -d "$PLATFORM_STORAGE" \
        || die "owned Incus storage source disappeared: $PLATFORM_STORAGE"
      PLATFORM_OWNED=1
    fi
    sudo -n incus network show incusbr0 --project default >/dev/null 2>&1 \
      && return
  fi
  if [ -e "$PLATFORM_ROOT" ]; then
    [ -d "$PLATFORM_ROOT" ] && [ ! -L "$PLATFORM_ROOT" ] \
      && [ "$(cat "$PLATFORM_ROOT/.marker" 2>/dev/null)" = "$MARKER" ] \
      || die "refusing unmarked Incus platform root $PLATFORM_ROOT"
  else
    install -d -m 0700 "$PLATFORM_ROOT"
    printf '%s\n' "$MARKER" > "$PLATFORM_ROOT/.marker"
  fi
  PLATFORM_OWNED=1
  info "installing and initializing Incus on clean VM$VM"
  (
    # shellcheck source=tests/helpers/test-context.sh
    . "$ROOT/tests/helpers/test-context.sh"
    setup_test_context "$PLATFORM_ROOT/bootstrap"
    export SUBYARD_USER
    SUBYARD_USER="$(id -un)"
    export SUBYARD_OPERATOR_HOME="$HOME"
    export SUBYARD_CONFIG_DIR="$ROOT/config"
    export SUBYARD_CONFIG_HOME="$PLATFORM_ROOT/config"
    export SUBYARD_HOME="$PLATFORM_ROOT/data"
    export STORAGE_PATH="$SUBYARD_HOME/incus/storage"
    export HOST_BASE="$SUBYARD_HOME/host-data"
    export RESTRICTED_DISK_PATHS="$HOST_BASE"
    bash "$ROOT/scripts/01-install-incus.sh" --yes --zabbly
  )
  command -v incus >/dev/null 2>&1 \
    && sudo -n incus info >/dev/null \
    && sudo -n incus storage show default --project default >/dev/null \
    && sudo -n incus network show incusbr0 --project default >/dev/null \
    || die "Incus bootstrap did not converge"
  ok "Incus owner API is ready on VM$VM"
}

cleanup_owned_host_incus() {
  local device fingerprint source
  [ "$PLATFORM_OWNED" = 1 ] || return 0
  [ -d "$PLATFORM_ROOT" ] && [ ! -L "$PLATFORM_ROOT" ] \
    && [ "$(cat "$PLATFORM_ROOT/.marker" 2>/dev/null)" = "$MARKER" ] \
    || {
      printf 'release-migration-catch-up: refusing unmarked platform cleanup %s\n' \
        "$PLATFORM_ROOT" >&2
      return 1
    }
  host_incus storage show default --project default >/dev/null 2>&1 || return 0
  source="$(host_incus storage get default source --project default)"
  [ "$source" = "$PLATFORM_STORAGE" ] || {
    printf 'release-migration-catch-up: refusing foreign storage cleanup %s\n' \
      "$source" >&2
    return 1
  }
  [ -z "$(host_incus list --all-projects --format csv -c n)" ] || {
    printf 'release-migration-catch-up: owned platform still has instances\n' >&2
    return 1
  }
  while IFS= read -r fingerprint; do
    [ -n "$fingerprint" ] || continue
    host_incus image delete "$fingerprint" --project default >/dev/null || return
  done < <(host_incus image list --project default --format csv -c f)
  for device in eth0 root; do
    if host_incus profile device list default --project default 2>/dev/null \
      | grep -qx "$device"; then
      host_incus profile device remove default "$device" --project default >/dev/null \
        || return
    fi
  done
  if host_incus network show incusbr0 --project default >/dev/null 2>&1; then
    host_incus network delete incusbr0 --project default >/dev/null || return
  fi
  host_incus storage delete default --project default >/dev/null || return
  sudo -n find "$PLATFORM_ROOT" -depth -delete || return
}

assert_state_root() {
  [ -d "$STATE_ROOT" ] && [ ! -L "$STATE_ROOT" ] \
    && [ "$(cat "$STATE_ROOT/.marker" 2>/dev/null)" = "$MARKER" ] \
    || die "refusing unmarked state root $STATE_ROOT"
}

seal_state_root() {
  assert_state_root
  touch "$STATE_ROOT/public-worktree.tar.gz"
  sudo -n chown root:root "$STATE_ROOT" "$STATE_ROOT/.marker"
  sudo -n chmod 0644 "$STATE_ROOT/.marker"
}

mark_outer_project_for_cleanup() {
  local project="$1" instance="$2" yard="$3" registration
  host_incus project show "$project" >/dev/null 2>&1 || return 0
  if [ "$(host_incus project get "$project" user.subyard.release-catchup 2>/dev/null)" = \
    "$MARKER" ]; then
    return 0
  fi
  registration="$OPERATOR_HOME/.config/subyard/yards/$yard/config.env"
  operator_env test -f "$registration" \
    && operator_env grep -qx 'YARD_TEMPLATE=test-vms' "$registration" \
    || die "refusing unregistered outer project $project"
  [ "$(host_incus config get "$instance" user.subyard.managed \
    --project "$project" 2>/dev/null)" = true ] \
    || die "refusing foreign outer instance $project/$instance"
  host_incus project set "$project" user.subyard.release-catchup="$MARKER"
}

delete_marked_project() {
  local project="$1" instance="$2" volume type
  host_incus project show "$project" >/dev/null 2>&1 || return 0
  [ "$(host_incus project get "$project" user.subyard.release-catchup 2>/dev/null)" = \
    "$MARKER" ] \
    || die "refusing unmarked project $project"
  if host_incus config show "$instance" --project "$project" >/dev/null 2>&1; then
    [ "$(host_incus config get "$instance" user.subyard.managed \
      --project "$project" 2>/dev/null)" = true ] \
      || die "refusing foreign instance $project/$instance"
    host_incus delete "$instance" --project "$project" --force >/dev/null
  fi
  while IFS=, read -r type volume; do
    [ "$type" = custom ] && [ -n "$volume" ] || continue
    host_incus storage volume delete default "$volume" --project "$project" >/dev/null
  done < <(host_incus storage volume list default --project "$project" --format csv -c t,n)
  [ -z "$(host_incus list --project "$project" --format csv -c n)" ] \
    || die "unexpected instance remains in $project"
  host_incus project delete "$project" >/dev/null
}

cleanup() {
  local rc=$? cleanup_failed=0
  trap - EXIT INT TERM
  set +e
  [ "$CLEANUP_ARMED" = 1 ] || exit "$rc"
  if id "$OPERATOR" >/dev/null 2>&1; then
    if host_incus project show "$CURRENT_PROJECT" >/dev/null 2>&1; then
      mark_outer_project_for_cleanup "$CURRENT_PROJECT" "$CURRENT_INSTANCE" test-yard
      operator_yard -Y test-yard teardown --yes >/dev/null 2>&1 || true
    elif host_incus project show "$LEGACY_PROJECT" >/dev/null 2>&1; then
      mark_outer_project_for_cleanup "$LEGACY_PROJECT" "$LEGACY_INSTANCE" e2e-yard
      operator_yard -Y e2e-yard teardown --yes >/dev/null 2>&1 || true
    fi
  fi
  delete_marked_project "$CURRENT_PROJECT" "$CURRENT_INSTANCE"
  delete_marked_project "$LEGACY_PROJECT" "$LEGACY_INSTANCE"
  delete_marked_project "$CONSUMER_PROJECT" "$CONSUMER_INSTANCE"
  cleanup_owned_host_incus || cleanup_failed=1
  sudo -n find /srv/subyard-test-yard -depth -delete 2>/dev/null || true
  sudo -n find /srv/subyard-e2e-yard -depth -delete 2>/dev/null || true
  sudo -n find /srv/subyard -depth -delete 2>/dev/null || true
  if id "$OPERATOR" >/dev/null 2>&1; then
    sudo -n loginctl disable-linger "$OPERATOR" >/dev/null 2>&1 || true
    sudo -n systemctl stop "user@$(operator_uid).service" >/dev/null 2>&1 || true
    sudo -n userdel -r "$OPERATOR" >/dev/null 2>&1 || true
  fi
  sudo -n find "$SUDOERS" -delete 2>/dev/null || true
  if [ -d "$STATE_ROOT" ]; then
    assert_state_root
    sudo -n find "$STATE_ROOT" -depth -delete
  fi
  [ "$cleanup_failed" = 0 ] || rc=3
  exit "$rc"
}
trap cleanup EXIT INT TERM

prepare_host() {
  [ "$VM" = 1 ] || [ "$VM" = 2 ] \
    || die "run through dev/agent-e2e.sh on VM1 or VM2"
  [[ "$RUN_ID" =~ ^[0-9a-f]{8}$ ]] \
    || die 'run through an allocated lease with an eight-character run ID'
  case "$MODE" in
    auto) [ "$VM" = 1 ] && MODE=direct || MODE=missed ;;
    direct) [ "$VM" = 1 ] || die "the direct lane is pinned to VM1" ;;
    missed) [ "$VM" = 2 ] || die "the missed lane is pinned to VM2" ;;
    hotfix) [ "$VM" = 1 ] || die "the hotfix lane is pinned to VM1" ;;
    hotfix-clean) [ "$VM" = 2 ] || die "the hotfix-clean lane is pinned to VM2" ;;
    hotfix-legacy) [ "$VM" = 1 ] || die "the hotfix-legacy lane is pinned to VM1" ;;
    hotfix-broken-042)
      [ "$VM" = 1 ] || die "the hotfix-broken-042 lane is pinned to VM1"
      ;;
    *)
      die "expected auto, direct, missed, hotfix, hotfix-clean, hotfix-legacy or hotfix-broken-042"
      ;;
  esac
  for command in curl git go jq ssh-keygen sudo tar; do
    command -v "$command" >/dev/null 2>&1 || die "$command is required"
  done
  sudo -n true || die "passwordless sudo is required"
  if ! command -v expect >/dev/null 2>&1; then
    info "installing expect for the disposable operator PTY"
    sudo -n apt-get update -qq
    sudo -n env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq expect
  fi
  ensure_host_incus
  host_incus info >/dev/null || die "initialized Incus is required"
  host_incus storage show default --project default >/dev/null \
    || die "Incus default storage is unavailable"
  host_incus network show incusbr0 --project default >/dev/null \
    || die "Incus incusbr0 is unavailable"
  for project in \
    "$CONSUMER_PROJECT" "$LEGACY_PROJECT" "$CURRENT_PROJECT"; do
    ! host_incus project show "$project" >/dev/null 2>&1 \
      || die "fixture target project already exists: $project"
  done
  [ ! -e "$STATE_ROOT" ] || die "fixture state already exists: $STATE_ROOT"
  install -d -m 0711 "$STATE_ROOT"
  printf '%s\n' "$MARKER" > "$STATE_ROOT/.marker"
  CLEANUP_ARMED=1
}

prepare_operator() {
  local base_image sudoers_tmp uid
  ! id "$OPERATOR" >/dev/null 2>&1 || die "fixture user $OPERATOR already exists"
  sudo -n useradd --create-home --home-dir "$OPERATOR_HOME" --shell /bin/bash "$OPERATOR"
  sudo -n usermod -aG incus-admin "$OPERATOR"
  sudoers_tmp="$(mktemp /tmp/subyard-release-catchup-sudoers.XXXXXX)"
  printf '%s ALL=(root) NOPASSWD: ALL\n' "$OPERATOR" > "$sudoers_tmp"
  sudo -n install -o root -g root -m 0440 "$sudoers_tmp" "$SUDOERS"
  find "$sudoers_tmp" -delete
  sudo -n loginctl enable-linger "$OPERATOR"
  uid="$(operator_uid)"
  sudo -n systemctl start "user@$uid.service"
  for _ in $(seq 1 30); do
    sudo -n test -S "/run/user/$uid/bus" && break
    sleep 1
  done
  sudo -n test -S "/run/user/$uid/bus" || die "fixture user bus is unavailable"
  operator_env install -d -m 0700 \
    "$OPERATOR_HOME/.config/subyard/yards/e2e-yard" \
    "$OPERATOR_HOME/.local/bin"
  operator_env bash -c 'printf "%s" "$2" > "$1"' _ \
    "$OPERATOR_HOME/.config/subyard/config.env" \
    $'AGENTS=\n'
  base_image="$IMAGE_ALIAS"
  if ! host_incus image info "$base_image" --project default >/dev/null 2>&1; then
    base_image=images:debian/13
  fi
  operator_env bash -c 'printf "%s" "$2" > "$1"' _ \
    "$OPERATOR_HOME/.config/subyard/yards/e2e-yard/config.env" \
    "$(printf '%s\n' \
      'YARD_TEMPLATE=test-vms' \
      'SSH_PORT=2223' \
      'AGENTS=' \
      'DEV_UID=1001' \
      'E2E_VM_CPU=1' \
      'E2E_VM_MEMORY=1GiB' \
      'E2E_VM_DISK=10GiB' \
      "BASE_IMAGE=$base_image" \
      "BASE_IMAGE_FALLBACK=$base_image")"
  operator_env chmod 0600 \
    "$OPERATOR_HOME/.config/subyard/config.env" \
    "$OPERATOR_HOME/.config/subyard/yards/e2e-yard/config.env"
}

prepare_candidate() {
  info "packaging exact local candidate $CANDIDATE_VERSION"
  "$ROOT/dev/package-engine.sh" \
    --output-dir "$CANDIDATE_RELEASE" \
    --version "$CANDIDATE_VERSION" >/dev/null
  chmod -R a+rX "$CANDIDATE_RELEASE"
  [ -f "$CANDIDATE_RELEASE/subyard-install.sh" ] \
    || die "candidate installer is unavailable"
}

install_old_runtime() {
  info "installing published yard $OLD_VERSION"
  curl -fsSL --proto '=https' --tlsv1.2 \
    "https://github.com/Subyard/Subyard/releases/download/v$OLD_VERSION/subyard-install.sh" \
    -o "$INSTALLER"
  [ "$(sha256sum "$INSTALLER" | awk '{print $1}')" = "$OLD_INSTALLER_SHA256" ] \
    || die "published $OLD_VERSION installer checksum changed"
  chmod 0755 "$INSTALLER"
  operator_env env YARD_RELEASE_VERSION="$OLD_VERSION" \
    "$INSTALLER" --version "$OLD_VERSION" --yes
  [ "$(operator_yard --version)" = "yard $OLD_VERSION" ] \
    || die "published $OLD_VERSION runtime is not active"
}

prepare_consumer() {
  local image="$IMAGE_ALIAS"
  if ! host_incus image info "$image" --project default >/dev/null 2>&1; then
    image=images:debian/13/cloud
  fi
  host_incus project create "$CONSUMER_PROJECT" \
    -c features.images=false \
    -c features.profiles=true \
    -c features.storage.volumes=true >/dev/null
  host_incus project set "$CONSUMER_PROJECT" user.subyard.release-catchup="$MARKER"
  host_incus profile device add default root disk \
    path=/ pool=default --project "$CONSUMER_PROJECT" >/dev/null
  host_incus profile device add default eth0 nic \
    name=eth0 network=incusbr0 --project "$CONSUMER_PROJECT" >/dev/null
  host_incus launch "$image" "$CONSUMER_INSTANCE" \
    --project "$CONSUMER_PROJECT" \
    -c user.subyard.managed=true \
    -c user.subyard.name=default \
    -c user.subyard.initialized=true \
    -c user.subyard.desired_power=running \
    -c user.subyard.bridge=incusbr0 \
    -c boot.autostart=false >/dev/null
  for _ in $(seq 1 120); do
    host_incus exec "$CONSUMER_INSTANCE" --project "$CONSUMER_PROJECT" -- true \
      >/dev/null 2>&1 && break
    sleep 1
  done
  host_incus exec "$CONSUMER_INSTANCE" --project "$CONSUMER_PROJECT" -- true \
    >/dev/null 2>&1 || die "consumer container did not become ready"
  host_incus exec "$CONSUMER_INSTANCE" --project "$CONSUMER_PROJECT" -- \
    /bin/sh -c 'command -v git >/dev/null 2>&1 &&
      command -v jq >/dev/null 2>&1 &&
      command -v ssh >/dev/null 2>&1 || {
        apt-get update -qq
        DEBIAN_FRONTEND=noninteractive apt-get install -y -qq git jq openssh-client
      }'
  ! host_incus config device get "$CONSUMER_INSTANCE" subyard-e2e-routes type \
    --project "$CONSUMER_PROJECT" >/dev/null 2>&1 \
    || die "legacy consumer unexpectedly has the route mount"
  host_incus exec "$CONSUMER_INSTANCE" --project "$CONSUMER_PROJECT" -- \
    awk '{print $22}' /proc/1/stat > "$STATE_ROOT/consumer-starttime"
  ok "running legacy consumer exists without the route mount"
}

prepare_legacy_owner() {
  host_incus project create "$LEGACY_PROJECT" \
    -c features.images=false \
    -c user.subyard.release-catchup="$MARKER" >/dev/null
  info "initializing published $OLD_VERSION e2e-yard"
  operator_yard -Y e2e-yard init --yes
  operator_yard -Y e2e-yard start --yes
  operator_yard -Y e2e-yard check
  [ "$(host_incus config get "$LEGACY_INSTANCE" user.subyard.managed \
    --project "$LEGACY_PROJECT")" = true ] \
    || die "legacy owner instance is not managed"
  operator_yard -Y e2e-yard test-vms status >/dev/null
  ok "published $OLD_VERSION legacy owner is ready"
}

assert_hotfix_runtime_links() {
  local current="$1" previous="$2"
  [ "$(operator_env readlink "$OPERATOR_RUNTIME/current")" = "$current" ] \
    || die "unexpected current runtime link"
  [ "$(operator_env readlink "$OPERATOR_RUNTIME/previous")" = "$previous" ] \
    || die "unexpected previous runtime link"
}

assert_hotfix_route_ready() {
  operator_env bash -c '
    set -euo pipefail
    root="$1"
    [ -d "$root" ] && [ ! -L "$root" ]
    [ -L "$root/current" ]
    target="$(readlink "$root/current")"
    case "$target" in .route-*) ;; *) exit 1 ;; esac
    case "$target" in */*) exit 1 ;; esac
    [ -d "$root/$target" ] && [ ! -L "$root/$target" ]
    for file in route.tsv known_hosts; do
      [ -f "$root/$target/$file" ] && [ ! -L "$root/$target/$file" ]
    done
  ' _ "$OPERATOR_HOME/.subyard/e2e/routes/test-yard" \
    || die "canonical test-yard route is unavailable or unsafe"
}

assert_hotfix_static_key() {
  local expectation="$1"
  case "$expectation" in
    present)
      host_incus exec "$CURRENT_INSTANCE" --project "$CURRENT_PROJECT" -- \
        test -s /var/lib/subyard/e2e-agent/.ssh/authorized_keys \
        || die "legacy static controller key is unavailable"
      ;;
    absent)
      host_incus exec "$CURRENT_INSTANCE" --project "$CURRENT_PROJECT" -- \
        bash -euo pipefail -c '
          keys=/var/lib/subyard/e2e-agent/.ssh/authorized_keys
          [ -f "$keys" ] && [ ! -L "$keys" ] && [ ! -s "$keys" ]
          group="$(id -g subyard-e2e-agent)"
          [ "$(stat -c "%u:%g:%a" "$keys")" = "0:$group:640" ]
        ' || die "candidate did not safely retire the static controller key"
      ;;
    *) die "unknown static-key expectation $expectation" ;;
  esac
}

hotfix_migrate() {
  operator_env env SUBYARD_REPOSITORY_ROOT="$OPERATOR_RUNTIME/current" \
    "$OPERATOR_RUNTIME/current/bin/yard-engine" _migrate "$1"
}

hotfix_transaction_directory() {
  local version="${1:?transaction version is required}"
  operator_env bash -c '
    set -euo pipefail
    root="$1"
    version="$2"
    [ -d "$root" ] && [ ! -L "$root" ]
    selected=
    shopt -s nullglob
    for transaction in "$root"/*; do
      [ -d "$transaction" ] && [ ! -L "$transaction" ]
      journal="$transaction/transaction.json"
      [ -f "$journal" ] && [ ! -L "$journal" ]
      to_release="$(jq -er \
        ".toRelease | select(type == \"string\" and length > 0)" "$journal")"
      [ "$to_release" = "$version" ] || continue
      [ -z "$selected" ]
      selected="$transaction"
    done
    [ -n "$selected" ]
    printf "%s\n" "$selected"
  ' _ "$OPERATOR_HOME/.config/subyard/migrations/transactions" "$version"
}

validate_hotfix_transaction() {
  local phase="$1" owner_phase="$2" transaction journal
  transaction="$(hotfix_transaction_directory "$BROKEN_VERSION")" \
    || die "0.4.1 migration transaction is missing or ambiguous"
  [ "$(basename "$transaction")" = "$HOTFIX_TRANSACTION_ID" ] \
    || die "0.4.1 migration transaction has an unexpected identity"
  journal="$transaction/transaction.json"
  operator_env bash -c '
    set -euo pipefail
    owner="$(id -u)"
    root="$1"
    transaction="$2"
    for directory in "$root" "$root/transactions" "$transaction"; do
      [ -d "$directory" ] && [ ! -L "$directory" ]
      [ "$(stat -c "%a:%u" "$directory")" = "700:$owner" ]
    done
    for file in "$root/state.json" "$transaction/transaction.json"; do
      [ -f "$file" ] && [ ! -L "$file" ]
      [ "$(stat -c "%a:%u:%h" "$file")" = "600:$owner:1" ]
    done
    ! find "$root" -xdev -type l -print -quit | grep -q .
  ' _ "$OPERATOR_HOME/.config/subyard/migrations" "$transaction" \
    || die "0.4.1 migration metadata is unsafe"
  operator_env jq -e \
    --arg phase "$phase" \
    --arg owner_phase "$owner_phase" \
    --arg from "$RELEASE_040_TARGET" \
    --arg to "$RELEASE_041_TARGET" '
      ([
        "schemaVersion", "fromLayout", "toLayout", "fromRuntime",
        "toRelease", "toRuntime", "phase", "migrations", "operations",
        "rollbackOperations"
      ] - keys | length) == 0 and
      (keys - [
        "schemaVersion", "fromLayout", "toLayout", "fromRuntime",
        "toRelease", "toRuntime", "phase", "migrations", "entries",
        "operations", "rollbackOperations"
      ] | length) == 0 and
      .schemaVersion == 1 and .fromLayout == 1 and .toLayout == 2 and
      .fromRuntime == $from and .toRelease == "0.4.1" and
      .toRuntime == $to and .phase == $phase and
      .migrations == ["migrate-test-yard-owner"] and
      ((.entries // []) | length) == 0 and .rollbackOperations == true and
      (.operations | length) == 2 and
      (.operations[0] | (keys | sort)) ==
        (["migrationId", "operationId", "kind", "before", "phase"] | sort) and
      .operations[0] == {
        migrationId: "migrate-test-yard-owner",
        operationId: "test-yard-owner",
        kind: "test-yard-owner-v1",
        before: "current",
        phase: $owner_phase
      } and
      (.operations[1] | (keys | sort)) ==
        (["migrationId", "operationId", "kind", "before", "phase"] | sort) and
      .operations[1].migrationId == "migrate-test-yard-owner" and
      .operations[1].operationId == "test-yard-route-consumers" and
      .operations[1].kind == "test-yard-route-consumers-v1" and
      .operations[1].phase == "rolled-back" and
      (.operations[1].before | fromjson) == {
        schemaVersion: 1,
        active: true,
        consumers: [{
          project: "subyard",
          instance: "yard",
          yard: "default",
          mounted: false
        }]
      }
    ' "$journal" >/dev/null \
    || die "0.4.1 migration transaction does not match the guarded recovery shape"
}

validate_failed_hotfix_transaction() {
  local phase="$1" owner_phase="$2" route_phase="$3" broker_phase="$4"
  local transaction journal
  transaction="$(hotfix_transaction_directory "$FAILED_HOTFIX_VERSION")" \
    || die "0.4.2 migration transaction is missing or ambiguous"
  [ "$(basename "$transaction")" = "$FAILED_HOTFIX_TRANSACTION_ID" ] \
    || die "0.4.2 migration transaction has an unexpected identity"
  journal="$transaction/transaction.json"
  operator_env bash -c '
    set -euo pipefail
    owner="$(id -u)"
    root="$1"
    transaction="$2"
    for directory in "$root" "$root/transactions" "$transaction"; do
      [ -d "$directory" ] && [ ! -L "$directory" ]
      [ "$(stat -c "%a:%u" "$directory")" = "700:$owner" ]
    done
    for file in "$root/state.json" "$transaction/transaction.json"; do
      [ -f "$file" ] && [ ! -L "$file" ]
      [ "$(stat -c "%a:%u:%h" "$file")" = "600:$owner:1" ]
    done
    ! find "$root" -xdev -type l -print -quit | grep -q .
  ' _ "$OPERATOR_HOME/.config/subyard/migrations" "$transaction" \
    || die "0.4.2 migration metadata is unsafe"
  operator_env jq -e \
    --arg phase "$phase" \
    --arg owner_phase "$owner_phase" \
    --arg route_phase "$route_phase" \
    --arg broker_phase "$broker_phase" \
    --arg from "$RELEASE_040_TARGET" \
    --arg to "$RELEASE_042_TARGET" '
      .schemaVersion == 1 and .fromLayout == 1 and .toLayout == 3 and
      .fromRuntime == $from and .toRelease == "0.4.2" and
      .toRuntime == $to and .phase == $phase and
      .migrations == [
        "migrate-test-yard-owner",
        "refresh-test-vm-broker"
      ] and
      ((.entries // []) | length) == 0 and .rollbackOperations == true and
      (.operations | length) == 3 and
      .operations[0] == {
        migrationId: "migrate-test-yard-owner",
        operationId: "test-yard-owner",
        kind: "test-yard-owner-v1",
        before: "current",
        phase: $owner_phase
      } and
      .operations[1].migrationId == "migrate-test-yard-owner" and
      .operations[1].operationId == "test-yard-route-consumers" and
      .operations[1].kind == "test-yard-route-consumers-v1" and
      .operations[1].phase == $route_phase and
      (.operations[1].before | fromjson) == {
        schemaVersion: 1,
        active: true,
        consumers: [{
          project: "subyard",
          instance: "yard",
          yard: "default",
          mounted: false
        }]
      } and
      .operations[2] == {
        migrationId: "refresh-test-vm-broker",
        operationId: "test-vm-broker-runtime",
        kind: "test-vm-broker-runtime-v1",
        before: "active",
        phase: $broker_phase
      }
    ' "$journal" >/dev/null \
    || die "0.4.2 migration transaction does not match the guarded recovery shape"
}

prepare_hotfix_current_owner() {
  info "updating published $OLD_VERSION to published $MISSED_VERSION"
  operator_yard update --version "$MISSED_VERSION" --yes
  [ "$(operator_yard --version)" = "yard $MISSED_VERSION" ] \
    || die "published $MISSED_VERSION runtime is not active"
  [ "$(operator_env readlink "$OPERATOR_RUNTIME/current")" = "$RELEASE_040_TARGET" ] \
    || die "published $MISSED_VERSION release identity changed"
  operator_env test ! -e \
    "$OPERATOR_HOME/.config/subyard/yards/test-yard" \
    || die "canonical registration already exists"
  operator_env mv \
    "$OPERATOR_HOME/.config/subyard/yards/e2e-yard" \
    "$OPERATOR_HOME/.config/subyard/yards/test-yard"
  host_incus project create "$CURRENT_PROJECT" \
    -c features.images=false \
    -c user.subyard.release-catchup="$MARKER" >/dev/null
  info "initializing canonical test-yard with published $MISSED_VERSION"
  operator_yard -Y test-yard init --yes
  operator_yard -Y test-yard start --yes
  operator_yard -Y test-yard check
  [ "$(host_incus config get "$CURRENT_INSTANCE" user.subyard.managed \
    --project "$CURRENT_PROJECT")" = true ] \
    || die "canonical owner instance is not managed"
  operator_yard -Y test-yard test-vms status >/dev/null
  assert_hotfix_route_ready
  ! host_incus config device get "$CONSUMER_INSTANCE" subyard-e2e-routes type \
    --project "$CONSUMER_PROJECT" >/dev/null 2>&1 \
    || die "0.4.0 unexpectedly mounted the canonical route in the consumer"
  ok "published 0.4.0 canonical owner and route are ready"
}

seed_hotfix_static_key() {
  info "seeding the legacy static controller admission"
  operator_env env \
    SUBYARD_E2E_VM=1 \
    SUBYARD_E2E_LEGACY_FIXTURE=1 \
    bash -s -- "$CURRENT_PROJECT" "$CURRENT_INSTANCE" \
    < "$ROOT/dev/e2e/seed-test-vms-legacy-state.sh"
  assert_hotfix_static_key present
}

remove_hotfix_route() {
  operator_env bash -c '
    set -euo pipefail
    root="$1"
    [ -L "$root/current" ]
    target="$(readlink "$root/current")"
    case "$target" in .route-*) ;; *) exit 1 ;; esac
    case "$target" in */*) exit 1 ;; esac
    [ -d "$root/$target" ] && [ ! -L "$root/$target" ]
    unlink "$root/current"
  ' _ "$OPERATOR_HOME/.subyard/e2e/routes/test-yard" \
    || die "refusing to remove an unsafe canonical route link"
  operator_env test ! -e \
    "$OPERATOR_HOME/.subyard/e2e/routes/test-yard/current" \
    || die "canonical route link remains before the broken update"
  ok "route-missing/static-key precondition matches the affected host"
}

reproduce_broken_update() {
  local doctor_output doctor_rc engine_hash output report rc
  info "reproducing the released $BROKEN_VERSION migration failure"
  set +e
  output="$(operator_yard update --version "$BROKEN_VERSION" --yes 2>&1)"
  rc=$?
  set -e
  printf '%s\n' "$output"
  [ "$rc" -ne 0 ] || die "published $BROKEN_VERSION unexpectedly succeeded"
  for expected in \
    "publish canonical test-yard route" \
    "test-yard rollback expected registration state current, found current" \
    "migration commit and recovery both failed"; do
    grep -Fq "$expected" <<<"$output" \
      || die "published $BROKEN_VERSION failure omitted: $expected"
  done
  assert_hotfix_runtime_links "$RELEASE_041_TARGET" "$RELEASE_040_TARGET"
  assert_hotfix_static_key present
  engine_hash="$(operator_env sha256sum \
    "$OPERATOR_RUNTIME/current/bin/yard-engine" | awk '{print $1}')"
  set +e
  doctor_output="$(
    host_incus exec "$CURRENT_INSTANCE" --project "$CURRENT_PROJECT" -- \
      env WANT_ENABLED=1 WANT_ENGINE_HASH="$engine_hash" \
      /usr/local/libexec/subyard/test-vms-inner _test-vms-worker doctor 2>&1
  )"
  doctor_rc=$?
  set -e
  [ "$doctor_rc" -ne 0 ] \
    && grep -Fq "test-vms: static controller key remains active" <<<"$doctor_output" \
    || die "published 0.4.1 doctor did not confirm the static-key failure"
  assert_hotfix_route_ready
  ! host_incus config device get "$CONSUMER_INSTANCE" subyard-e2e-routes type \
    --project "$CONSUMER_PROJECT" >/dev/null 2>&1 \
    || die "failed route operation retained the consumer mount"
  report="$(hotfix_migrate check)"
  jq -e '
    .schemaVersion == 1 and .layout == 1 and .targetLayout == 2 and
    .requiredMigrations == ["migrate-test-yard-owner"] and
    .affectedResources == ["test-yard-owner", "test-yard-route-consumers"] and
    .phase == "rolling-back"
  ' <<<"$report" >/dev/null \
    || die "released $BROKEN_VERSION did not retain the expected rollback report"
  validate_hotfix_transaction rolling-back rolling-back
  ok "published 0.4.1 reproduced the exact rolling-back transaction"
}

repair_broken_update() {
  local before_hash report transaction journal
  transaction="$(hotfix_transaction_directory "$BROKEN_VERSION")"
  journal="$transaction/transaction.json"
  before_hash="$(operator_env sha256sum "$journal" | awk '{print $1}')"
  operator_env install -d -m 0700 "$(dirname "$HOTFIX_BACKUP")"
  operator_env test ! -e "$HOTFIX_BACKUP" \
    || die "refusing to replace an existing migration recovery backup"
  operator_env install -m 0600 "$journal" "$HOTFIX_BACKUP"
  [ "$(operator_env sha256sum "$HOTFIX_BACKUP" | awk '{print $1}')" = "$before_hash" ] \
    || die "migration recovery backup changed during copy"
  operator_env bash -c '
    set -euo pipefail
    journal="$1"
    temporary="$(mktemp "$(dirname "$journal")/.transaction.XXXXXX")"
    trap "rm -f -- \"$temporary\"" EXIT
    jq "
      (.operations[] |
        select(.migrationId == \"migrate-test-yard-owner\" and
          .operationId == \"test-yard-owner\" and
          .kind == \"test-yard-owner-v1\" and
          .before == \"current\" and
          .phase == \"rolling-back\") |
        .phase) = \"rolled-back\"
    " "$journal" > "$temporary"
    chmod 0600 "$temporary"
    sync -f "$temporary"
    mv -fT -- "$temporary" "$journal"
    sync -f "$(dirname "$journal")"
    trap - EXIT
  ' _ "$journal"
  validate_hotfix_transaction rolling-back rolled-back
  operator_env jq -e --slurpfile original "$HOTFIX_BACKUP" '
    . == ($original[0] | (.operations[0].phase) = "rolled-back")
  ' "$journal" >/dev/null \
    || die "journal repair changed more than the canonical owner phase"
  report="$(hotfix_migrate rollback)"
  jq -e '
    .layout == 1 and .targetLayout == 2 and
    .phase == "rolled-back" and .changed == true
  ' <<<"$report" >/dev/null \
    || die "guarded journal repair did not finish migration rollback"
  report="$(hotfix_migrate rollback)"
  jq -e '
    .layout == 1 and .targetLayout == 2 and
    (has("phase") | not) and .changed == false
  ' <<<"$report" >/dev/null \
    || die "completed migration rollback is not idempotent"
  validate_hotfix_transaction rolled-back rolled-back
  operator_env "$OPERATOR_RUNTIME/current/scripts/install-runtime-release.sh" \
    --runtime-root "$OPERATOR_RUNTIME" --rollback
  assert_hotfix_runtime_links "$RELEASE_040_TARGET" "$RELEASE_041_TARGET"
  [ "$(operator_yard --version)" = "yard $MISSED_VERSION" ] \
    || die "runtime rollback did not restore published $MISSED_VERSION"
  operator_yard -Y test-yard check
  assert_hotfix_route_ready
  assert_hotfix_static_key present
  ! host_incus config device get "$CONSUMER_INSTANCE" subyard-e2e-routes type \
    --project "$CONSUMER_PROJECT" >/dev/null 2>&1 \
    || die "manual recovery unexpectedly mounted the consumer route"
  report="$(hotfix_migrate check)"
  jq -e '
    .layout == 1 and .targetLayout == 1 and
    ((.requiredMigrations // []) | length) == 0
  ' <<<"$report" >/dev/null \
    || die "restored 0.4.0 runtime does not accept the recovered layout"
  ok "guarded journal repair and runtime rollback restored usable 0.4.0"
}

reproduce_failed_hotfix_update() {
  local current_hash current_output current_rc previous_hash previous_output
  local previous_rc report rc transcript
  info "reproducing the released $FAILED_HOTFIX_VERSION migration failure"
  transcript="$OPERATOR_HOME/failed-hotfix-update.typescript"
  operator_env sudo -k
  set +e
  operator_env env \
    SUBYARD_FIXTURE_PASSWORD='subyard-disposable-migration-fixture' \
    SUBYARD_UPDATE_TRANSCRIPT="$transcript" \
    SUBYARD_FAILED_HOTFIX_VERSION="$FAILED_HOTFIX_VERSION" \
    SUBYARD_YARD_BIN="$OPERATOR_HOME/.local/bin/yard" \
    expect <<'EXPECT'
set timeout 1200
log_file -noappend $env(SUBYARD_UPDATE_TRANSCRIPT)
spawn -noecho $env(SUBYARD_YARD_BIN) update \
  --version $env(SUBYARD_FAILED_HOTFIX_VERSION) --yes
set password_sent 0
expect {
  -re {\[sudo\] password for [^:]+:} {
    if {$password_sent} {
      exit 125
    }
    set password_sent 1
    send -- "$env(SUBYARD_FIXTURE_PASSWORD)\r"
    exp_continue
  }
  eof {}
  timeout { exit 124 }
}
set result [wait]
exit [lindex $result 3]
EXPECT
  rc=$?
  set -e
  [ "$rc" -ne 0 ] || die "published $FAILED_HOTFIX_VERSION unexpectedly succeeded"
  [ "$(operator_env grep -Fc '[sudo] password' "$transcript")" = 1 ] \
    || die "published $FAILED_HOTFIX_VERSION rollback did not request sudo exactly once"
  for expected in \
    "sudo authorization is required for root steps in init" \
    "init stage \"test-vms\" completed but did not converge" \
    "migration commit and recovery both failed"; do
    operator_env grep -Fq "$expected" "$transcript" \
      || die "published $FAILED_HOTFIX_VERSION failure omitted: $expected"
  done
  assert_hotfix_runtime_links "$RELEASE_042_TARGET" "$RELEASE_040_TARGET"
  assert_hotfix_static_key present
  host_incus config device get \
    "$CONSUMER_INSTANCE" subyard-e2e-routes type \
    --project "$CONSUMER_PROJECT" >/dev/null \
    || die "published $FAILED_HOTFIX_VERSION did not commit the route consumer"
  current_hash="$(operator_env sha256sum \
    "$OPERATOR_RUNTIME/current/bin/yard-engine" | awk '{print $1}')"
  set +e
  current_output="$(
    host_incus exec "$CURRENT_INSTANCE" --project "$CURRENT_PROJECT" -- \
      env WANT_ENABLED=1 WANT_ENGINE_HASH="$current_hash" \
      /usr/local/libexec/subyard/test-vms-inner _test-vms-worker doctor 2>&1
  )"
  current_rc=$?
  set -e
  [ "$current_rc" -ne 0 ] \
    && grep -Fq "test-vms: installed test-vms engine hash differs" <<<"$current_output" \
    || die "published $FAILED_HOTFIX_VERSION doctor did not confirm candidate drift"
  previous_hash="$(operator_env sha256sum \
    "$OPERATOR_RUNTIME/previous/bin/yard-engine" | awk '{print $1}')"
  set +e
  previous_output="$(
    host_incus exec "$CURRENT_INSTANCE" --project "$CURRENT_PROJECT" -- \
      env WANT_ENABLED=1 WANT_ENGINE_HASH="$previous_hash" \
      /usr/local/libexec/subyard/test-vms-inner _test-vms-worker doctor 2>&1
  )"
  previous_rc=$?
  set -e
  [ "$previous_rc" -ne 0 ] \
    && grep -Fq "test-vms: static controller key remains active" <<<"$previous_output" \
    || die "published 0.4.0 doctor did not confirm the static-key rollback failure"
  report="$(hotfix_migrate check)"
  jq -e '
    .schemaVersion == 1 and .layout == 1 and .targetLayout == 3 and
    .requiredMigrations == [
      "migrate-test-yard-owner",
      "refresh-test-vm-broker"
    ] and
    .affectedResources == [
      "test-yard-owner",
      "test-yard-route-consumers",
      "test-vm-broker-runtime"
    ] and
    .phase == "rolling-back"
  ' <<<"$report" >/dev/null \
    || die "released $FAILED_HOTFIX_VERSION did not retain the expected rollback report"
  validate_failed_hotfix_transaction \
    rolling-back committed committed rolling-back
  ok "published 0.4.2 reproduced the server's rolling-back broker transaction"
}

repair_failed_hotfix_operation() {
  local operation_id="$1" transaction journal
  transaction="$(hotfix_transaction_directory "$FAILED_HOTFIX_VERSION")"
  journal="$transaction/transaction.json"
  operator_env bash -c '
    set -euo pipefail
    journal="$1"
    operation_id="$2"
    temporary="$(mktemp "$(dirname "$journal")/.transaction.XXXXXX")"
    trap "rm -f -- \"$temporary\"" EXIT
    jq -e --arg operation_id "$operation_id" "
      if ([.operations[] |
        select(.operationId == \$operation_id and
          .phase == \"rolling-back\")] | length) == 1 then
        (.operations[] |
          select(.operationId == \$operation_id) |
          .phase) = \"rolled-back\"
      else
        error(\"unexpected operation phase\")
      end
    " "$journal" > "$temporary"
    chmod 0600 "$temporary"
    sync -f "$temporary"
    mv -fT -- "$temporary" "$journal"
    sync -f "$(dirname "$journal")"
    trap - EXIT
  ' _ "$journal" "$operation_id"
}

repair_failed_hotfix_update() {
  local before_hash report transaction journal
  transaction="$(hotfix_transaction_directory "$FAILED_HOTFIX_VERSION")"
  journal="$transaction/transaction.json"
  before_hash="$(operator_env sha256sum "$journal" | awk '{print $1}')"
  operator_env install -d -m 0700 "$(dirname "$FAILED_HOTFIX_BACKUP")"
  operator_env test ! -e "$FAILED_HOTFIX_BACKUP" \
    || die "refusing to replace an existing 0.4.2 migration recovery backup"
  operator_env install -m 0600 "$journal" "$FAILED_HOTFIX_BACKUP"
  [ "$(operator_env sha256sum "$FAILED_HOTFIX_BACKUP" | awk '{print $1}')" = "$before_hash" ] \
    || die "0.4.2 migration recovery backup changed during copy"

  repair_failed_hotfix_operation test-vm-broker-runtime
  validate_failed_hotfix_transaction \
    rolling-back committed committed rolled-back
  operator_env jq -e --slurpfile original "$FAILED_HOTFIX_BACKUP" '
    . == ($original[0] | (.operations[2].phase) = "rolled-back")
  ' "$journal" >/dev/null \
    || die "broker journal repair changed more than its operation phase"

  report="$(hotfix_migrate rollback)"
  jq -e '
    .layout == 1 and .targetLayout == 3 and
    .phase == "rolled-back" and .changed == true
  ' <<<"$report" >/dev/null \
    || die "guarded 0.4.2 journal repair did not finish migration rollback"
  report="$(hotfix_migrate rollback)"
  jq -e '
    .layout == 1 and .targetLayout == 3 and
    (has("phase") | not) and .changed == false
  ' <<<"$report" >/dev/null \
    || die "completed 0.4.2 migration rollback is not idempotent"
  validate_failed_hotfix_transaction \
    rolled-back rolled-back rolled-back rolled-back

  operator_env "$OPERATOR_RUNTIME/current/scripts/install-runtime-release.sh" \
    --runtime-root "$OPERATOR_RUNTIME" --rollback
  assert_hotfix_runtime_links "$RELEASE_040_TARGET" "$RELEASE_042_TARGET"
  [ "$(operator_yard --version)" = "yard $MISSED_VERSION" ] \
    || die "runtime rollback did not restore published $MISSED_VERSION"
  operator_yard -Y test-yard check
  assert_hotfix_route_ready
  assert_hotfix_static_key present
  ! host_incus config device get "$CONSUMER_INSTANCE" subyard-e2e-routes type \
    --project "$CONSUMER_PROJECT" >/dev/null 2>&1 \
    || die "manual 0.4.2 recovery retained the consumer route"
  report="$(hotfix_migrate check)"
  jq -e '
    .layout == 1 and .targetLayout == 1 and
    ((.requiredMigrations // []) | length) == 0
  ' <<<"$report" >/dev/null \
    || die "restored 0.4.0 runtime does not accept the recovered 0.4.2 layout"
  ok "guarded 0.4.2 journal repair and runtime rollback restored usable 0.4.0"
}

verify_hotfix_boundary() {
  local engine_hash old_transaction
  [ "$(operator_yard --version)" = "yard $CANDIDATE_VERSION" ] \
    || die "hotfix candidate runtime is not active"
  case "$(operator_env readlink "$OPERATOR_RUNTIME/current")" in
    releases/0.4.3-*) ;;
    *) die "hotfix candidate release identity is unexpected" ;;
  esac
  [ "$(operator_env readlink "$OPERATOR_RUNTIME/previous")" = "$RELEASE_040_TARGET" ] \
    || die "hotfix candidate did not retain 0.4.0 as previous"
  assert_hotfix_static_key absent
  engine_hash="$(operator_env sha256sum \
    "$OPERATOR_RUNTIME/current/bin/yard-engine" | awk '{print $1}')"
  host_incus exec "$CURRENT_INSTANCE" --project "$CURRENT_PROJECT" -- \
    env WANT_ENABLED=1 WANT_ENGINE_HASH="$engine_hash" \
    /usr/local/libexec/subyard/test-vms-inner _test-vms-worker doctor \
    || die "candidate test-vms doctor did not converge"
  old_transaction="$OPERATOR_HOME/.config/subyard/migrations/transactions/$HOTFIX_TRANSACTION_ID"
  operator_env test ! -e "$old_transaction" \
    || die "candidate cleanup retained the rolled-back 0.4.1 transaction"
  ok "candidate broker migration retired static admission and converged the broker"
}

upgrade_through_missed_release() {
  [ "$MODE" = missed ] || return 0
  info "updating through published yard $MISSED_VERSION"
  operator_yard update --version "$MISSED_VERSION" --yes
  [ "$(operator_yard --version)" = "yard $MISSED_VERSION" ] \
    || die "published $MISSED_VERSION runtime is not active"
  operator_env test -f \
    "$OPERATOR_HOME/.config/subyard/yards/e2e-yard/config.env" \
    || die "$MISSED_VERSION unexpectedly migrated the legacy registration"
  host_incus project show "$LEGACY_PROJECT" >/dev/null \
    || die "$MISSED_VERSION unexpectedly migrated the legacy project"
  ! host_incus config device get "$CONSUMER_INSTANCE" subyard-e2e-routes type \
    --project "$CONSUMER_PROJECT" >/dev/null 2>&1 \
    || die "$MISSED_VERSION unexpectedly reconciled the legacy consumer"
  ok "published $MISSED_VERSION reproduced the missed migration"
}

upgrade_candidate() {
  local expected_prompt="${1:-absent}" transcript
  info "running ordinary yard update to $CANDIDATE_VERSION"
  transcript="$OPERATOR_HOME/candidate-update.typescript"
  operator_env sudo -k
  operator_env env \
    YARD_RELEASE_BASE_URL="file://$CANDIDATE_RELEASE" \
    SUBYARD_FIXTURE_PASSWORD='subyard-disposable-migration-fixture' \
    SUBYARD_UPDATE_TRANSCRIPT="$transcript" \
    SUBYARD_CANDIDATE_VERSION="$CANDIDATE_VERSION" \
    SUBYARD_YARD_BIN="$OPERATOR_HOME/.local/bin/yard" \
    expect <<'EXPECT'
set timeout 1200
log_file -noappend $env(SUBYARD_UPDATE_TRANSCRIPT)
spawn -noecho $env(SUBYARD_YARD_BIN) update \
  --version $env(SUBYARD_CANDIDATE_VERSION) --yes
set password_sent 0
expect {
  -re {\[sudo\] password for [^:]+:} {
    if {$password_sent} {
      exit 125
    }
    set password_sent 1
    send -- "$env(SUBYARD_FIXTURE_PASSWORD)\r"
    exp_continue
  }
  eof {}
  timeout { exit 124 }
}
set result [wait]
exit [lindex $result 3]
EXPECT
  case "$expected_prompt" in
    absent)
      ! operator_env grep -Fq '[sudo] password' "$transcript" \
        || die "bounded candidate migration unexpectedly requested sudo"
      ;;
    present)
      [ "$(operator_env grep -Fc '[sudo] password' "$transcript")" = 1 ] \
        || die "root-bearing candidate migration did not request sudo exactly once"
      ;;
    *) die "invalid candidate sudo prompt expectation" ;;
  esac
  [ "$(operator_yard --version)" = "yard $CANDIDATE_VERSION" ] \
    || die "candidate runtime is not active"
}

require_operator_password_sudo() {
  local rc sudoers_tmp
  info "removing passwordless sudo before the checked operator update"
  sudoers_tmp="$(mktemp /tmp/subyard-release-catchup-sudoers.XXXXXX)"
  printf '%s ALL=(root) ALL\n' "$OPERATOR" > "$sudoers_tmp"
  sudo -n install -o root -g root -m 0440 "$sudoers_tmp" "$SUDOERS"
  find "$sudoers_tmp" -delete
  printf '%s:%s\n' "$OPERATOR" 'subyard-disposable-migration-fixture' \
    | sudo -n chpasswd
  operator_env sudo -k
  set +e
  operator_env sudo -n true >/dev/null 2>&1
  rc=$?
  set -e
  [ "$rc" -ne 0 ] || die "operator unexpectedly retained passwordless sudo"
  ok "checked update has a cold password-required sudo boundary"
}

verify_control_plane() {
  local actual_start source="$OPERATOR_HOME/.subyard/e2e/routes"
  operator_env test ! -e \
    "$OPERATOR_HOME/.config/subyard/yards/e2e-yard" \
    || die "legacy registration directory remains"
  operator_env test -f \
    "$OPERATOR_HOME/.config/subyard/yards/test-yard/config.env" \
    || die "canonical registration is unavailable"
  ! host_incus project show "$LEGACY_PROJECT" >/dev/null 2>&1 \
    || die "legacy owner project remains"
  host_incus project show "$CURRENT_PROJECT" >/dev/null \
    || die "canonical owner project is unavailable"
  host_incus project set "$CURRENT_PROJECT" user.subyard.release-catchup="$MARKER"
  [ "$(host_incus config device get "$CONSUMER_INSTANCE" subyard-e2e-routes type \
    --project "$CONSUMER_PROJECT")" = disk ] \
    && [ "$(host_incus config device get "$CONSUMER_INSTANCE" subyard-e2e-routes source \
      --project "$CONSUMER_PROJECT")" = "$source" ] \
    && [ "$(host_incus config device get "$CONSUMER_INSTANCE" subyard-e2e-routes path \
      --project "$CONSUMER_PROJECT")" = /var/lib/subyard/e2e-routes ] \
    && [ "$(host_incus config device get "$CONSUMER_INSTANCE" subyard-e2e-routes readonly \
      --project "$CONSUMER_PROJECT")" = true ] \
    || die "consumer route device did not converge"
  actual_start="$(host_incus exec "$CONSUMER_INSTANCE" --project "$CONSUMER_PROJECT" -- \
    awk '{print $22}' /proc/1/stat)"
  [ "$actual_start" = "$(cat "$STATE_ROOT/consumer-starttime")" ] \
    || die "consumer restarted during route reconciliation"
  host_incus exec "$CONSUMER_INSTANCE" --project "$CONSUMER_PROJECT" -- \
    test -r /var/lib/subyard/e2e-routes/test-yard/current/route.tsv
  host_incus exec "$CONSUMER_INSTANCE" --project "$CONSUMER_PROJECT" -- \
    test -r /var/lib/subyard/e2e-routes/test-yard/current/known_hosts
  operator_yard -Y test-yard status >/dev/null
  operator_yard -Y test-yard check
  operator_test_vms_status \
    | jq -e '.pool.slots | length == 2 and all(.state == "available")' >/dev/null
  operator_env jq -e \
    '.layout == 5 and
      .applied == [
        "migrate-test-yard-owner",
        "refresh-test-vm-broker",
        "refresh-power-reconciler",
        "repair-power-reconciler-systemd-compat"
      ]' \
    "$OPERATOR_HOME/.config/subyard/migrations/state.json" >/dev/null
  ok "owner, route publication, live consumer and layout converged without restart"
}

assert_power_runtime_matches_current_release() {
  operator_env cmp "$OPERATOR_RUNTIME/current/bin/yard-engine" "$POWER_RECONCILER" \
    || die 'installed power reconciler payload does not match the active release'
  operator_env sed "s|@SUBYARD_POWER_RECONCILER@|$POWER_RECONCILER|g" \
    "$OPERATOR_RUNTIME/current/config/systemd/subyard-power-reconcile.service.in" \
    | sudo -n cmp - "$POWER_UNIT" \
    || die 'installed power reconciler unit does not match the active release'
  sudo -n systemctl is-enabled --quiet subyard-power-reconcile.service \
    || die 'installed power reconciler unit is not enabled'
}

assert_candidate_power_transaction() {
  local phase="$1" transaction
  transaction="$(hotfix_transaction_directory "$CANDIDATE_VERSION")" \
    || die 'candidate catch-up transaction is missing or ambiguous'
  operator_env jq -e --arg phase "$phase" '
    .fromLayout == 1 and .toLayout == 5 and .phase == $phase and
    .migrations == [
      "migrate-test-yard-owner",
      "refresh-test-vm-broker",
      "refresh-power-reconciler",
      "repair-power-reconciler-systemd-compat"
    ] and
    (.operations | map([.migrationId, .operationId, .kind])) == [
      ["migrate-test-yard-owner", "test-yard-owner", "test-yard-owner-v1"],
      ["migrate-test-yard-owner", "test-yard-route-consumers",
       "test-yard-route-consumers-v1"],
      ["refresh-test-vm-broker", "test-vm-broker-runtime", "test-vm-broker-runtime-v1"],
      ["refresh-power-reconciler", "power-reconciler-runtime",
       "power-reconciler-runtime-v1"],
      ["repair-power-reconciler-systemd-compat", "power-reconciler-systemd-compat",
       "power-reconciler-systemd-compat-v1"]
    ] and
    all(.operations[]; .phase == $phase)
  ' "$transaction/transaction.json" >/dev/null \
    || die "candidate catch-up transaction is not $phase"
}

verify_legacy_power_rollback_cycle() {
  local candidate_target previous_target actual_start
  [ "$MODE" = direct ] || return 0
  candidate_target="$(operator_env readlink "$OPERATOR_RUNTIME/current")"
  previous_target="$(operator_env readlink "$OPERATOR_RUNTIME/previous")"
  assert_candidate_power_transaction committed

  info "rolling the layout-1 catch-up back to published $OLD_VERSION"
  operator_yard update --rollback --yes
  [ "$(operator_yard --version)" = "yard $OLD_VERSION" ] \
    || die 'ordinary catch-up rollback did not restore the published runtime'
  assert_hotfix_runtime_links "$previous_target" "$candidate_target"
  if ! operator_env jq -e --arg release "$previous_target" '
    .layout == 1 and (.applied // []) == [] and .currentRelease == $release
  ' "$OPERATOR_HOME/.config/subyard/migrations/state.json" >/dev/null; then
    operator_env jq -c '{layout, applied, currentRelease}' \
      "$OPERATOR_HOME/.config/subyard/migrations/state.json" >&2 || true
    die 'ordinary catch-up rollback did not restore layout 1'
  fi
  assert_candidate_power_transaction rolled-back
  assert_power_runtime_matches_current_release
  operator_env test -f \
    "$OPERATOR_HOME/.config/subyard/yards/e2e-yard/config.env" \
    && operator_env test ! -e \
      "$OPERATOR_HOME/.config/subyard/yards/test-yard" \
    && host_incus project show "$LEGACY_PROJECT" >/dev/null \
    && ! host_incus project show "$CURRENT_PROJECT" >/dev/null 2>&1 \
    || die 'ordinary catch-up rollback did not restore the legacy owner'
  ! host_incus config device get "$CONSUMER_INSTANCE" subyard-e2e-routes type \
    --project "$CONSUMER_PROJECT" >/dev/null 2>&1 \
    || die 'ordinary catch-up rollback retained the canonical route mount'
  actual_start="$(host_incus exec "$CONSUMER_INSTANCE" --project "$CONSUMER_PROJECT" -- \
    awk '{print $22}' /proc/1/stat)"
  [ "$actual_start" = "$(cat "$STATE_ROOT/consumer-starttime")" ] \
    || die 'ordinary catch-up rollback restarted the existing consumer'
  operator_yard -Y e2e-yard status >/dev/null
  operator_yard -Y e2e-yard check
  [ "$(host_incus config get "$LEGACY_INSTANCE" user.subyard.desired_power \
    --project "$LEGACY_PROJECT")" = running ] \
    || die 'ordinary catch-up rollback did not restore legacy desired power'

  info 'rolling the same candidate forward after layout-1 rollback'
  upgrade_candidate
  [ "$(operator_env readlink "$OPERATOR_RUNTIME/current")" = "$candidate_target" ] \
    || die 'catch-up roll-forward selected a different candidate runtime'
  assert_hotfix_runtime_links "$candidate_target" "$previous_target"
  assert_candidate_power_transaction committed
  verify_control_plane
  assert_power_runtime_matches_current_release
}

verify_data_plane() {
  local bundle="$STATE_ROOT/public-worktree.tar.gz"
  local guest_bundle=/tmp/subyard-release-catchup.tar.gz
  local guest_project="/srv/workspaces/Subyard-release-catchup-${RUN_ID}-vm${VM:-unknown}"
  local guest_source="$guest_project/src"
  local guest_marker="$guest_project/.subyard-release-catchup-owner"
  local guest_marker_value="$MARKER run=$RUN_ID"
  info "running standard broker acquire from the pre-existing consumer"
  host_incus exec "$CONSUMER_INSTANCE" --project "$CONSUMER_PROJECT" -- \
    sh -c 'id dev >/dev/null 2>&1 || useradd --create-home --shell /bin/bash dev'
  (
    cd "$ROOT"
    git ls-files --cached --others --exclude-standard -z \
      | sort -z \
      | tar --null -T - -czf "$bundle"
  )
  host_incus file push "$bundle" "$CONSUMER_INSTANCE$guest_bundle" \
    --project "$CONSUMER_PROJECT" >/dev/null
  host_incus exec "$CONSUMER_INSTANCE" --project "$CONSUMER_PROJECT" -- \
    sh -c '
      test ! -e "$1" && test ! -L "$1" || exit 1
      install -d -m 0755 "$2"
      printf "%s\n" "$3" > "$4"
      chmod 0444 "$4"
    ' _ "$guest_project" "$guest_source" "$guest_marker_value" "$guest_marker"
  host_incus exec "$CONSUMER_INSTANCE" --project "$CONSUMER_PROJECT" -- \
    tar -xzf "$guest_bundle" -C "$guest_source"
  host_incus exec "$CONSUMER_INSTANCE" --project "$CONSUMER_PROJECT" -- \
    find "$guest_bundle" -delete
  host_incus exec "$CONSUMER_INSTANCE" --project "$CONSUMER_PROJECT" -- \
    sh -c '
      test -d "$1" && test ! -L "$1" \
        && test -f "$2" && test ! -L "$2" \
        && test "$(cat "$2")" = "$3"
    ' _ "$guest_project" "$guest_marker" "$guest_marker_value"
  host_incus exec "$CONSUMER_INSTANCE" --project "$CONSUMER_PROJECT" -- \
    find "$guest_source" -xdev -exec chown -h dev:dev '{}' +
  host_incus exec "$CONSUMER_INSTANCE" --project "$CONSUMER_PROJECT" -- \
    runuser -u dev -- env \
      HOME=/home/dev USER=dev LOGNAME=dev SUBYARD_E2E_CONSUMER_FIXTURE=1 \
      bash "$guest_source/dev/e2e/release-migration-consumer.sh"
  host_incus exec "$CONSUMER_INSTANCE" --project "$CONSUMER_PROJECT" -- \
    sh -c '
      test -d "$1" && test ! -L "$1" \
        && test -f "$2" && test ! -L "$2" \
        && test "$(cat "$2")" = "$3"
    ' _ "$guest_project" "$guest_marker" "$guest_marker_value"
  host_incus exec "$CONSUMER_INSTANCE" --project "$CONSUMER_PROJECT" -- \
    find "$guest_project" -xdev -depth -delete
  operator_test_vms_status \
    | jq -e '.pool.slots | all(.state == "available")' >/dev/null
  host_incus exec "$CURRENT_INSTANCE" --project "$CURRENT_PROJECT" -- \
    incus list --all-projects --format json \
    | jq -e '
        [.[] | select(.name == "e2e-vm-1" or .name == "e2e-vm-2")] as $vms |
        ($vms | length) == 2 and all($vms[]; .status == "Stopped")
      ' >/dev/null
  ok "standard acquire, boundary and retained stopped pair passed"
}

prepare_host
prepare_operator
prepare_candidate
install_old_runtime
prepare_consumer
seal_state_root

if [ "$MODE" = hotfix ]; then
  prepare_hotfix_current_owner
  seed_hotfix_static_key
  remove_hotfix_route
  reproduce_broken_update
  repair_broken_update
  require_operator_password_sudo
  upgrade_candidate present
  verify_control_plane
  verify_hotfix_boundary
  info "reinstalling the same hotfix candidate"
  upgrade_candidate
  verify_control_plane
  verify_hotfix_boundary
  verify_data_plane
  printf 'ok: published 0.4.0 -> broken 0.4.1 -> recovered 0.4.3 hotfix lane passed\n'
  exit 0
fi
if [ "$MODE" = hotfix-clean ]; then
  prepare_hotfix_current_owner
  assert_hotfix_route_ready
  require_operator_password_sudo
  upgrade_candidate present
  verify_control_plane
  verify_hotfix_boundary
  info "reinstalling the same hotfix candidate"
  upgrade_candidate
  verify_control_plane
  verify_hotfix_boundary
  verify_data_plane
  printf 'ok: clean published 0.4.0 -> 0.4.3 hotfix lane passed\n'
  exit 0
fi
if [ "$MODE" = hotfix-legacy ]; then
  prepare_legacy_owner
  info "updating the legacy owner to published $MISSED_VERSION"
  operator_yard update --version "$MISSED_VERSION" --yes
  [ "$(operator_yard --version)" = "yard $MISSED_VERSION" ] \
    || die "published $MISSED_VERSION runtime is not active"
  operator_env test -f \
    "$OPERATOR_HOME/.config/subyard/yards/e2e-yard/config.env" \
    || die "published $MISSED_VERSION unexpectedly migrated the legacy registration"
  host_incus project show "$LEGACY_PROJECT" >/dev/null \
    || die "published $MISSED_VERSION unexpectedly migrated the legacy project"
  require_operator_password_sudo
  upgrade_candidate present
  verify_control_plane
  verify_hotfix_boundary
  info "reinstalling the same hotfix candidate"
  upgrade_candidate
  verify_control_plane
  verify_hotfix_boundary
  verify_data_plane
  printf 'ok: legacy published 0.4.0 owner -> 0.4.3 hotfix lane passed\n'
  exit 0
fi
if [ "$MODE" = hotfix-broken-042 ]; then
  prepare_hotfix_current_owner
  seed_hotfix_static_key
  require_operator_password_sudo
  reproduce_failed_hotfix_update
  repair_failed_hotfix_update
  upgrade_candidate present
  verify_control_plane
  verify_hotfix_boundary
  operator_env test ! -e \
    "$OPERATOR_HOME/.config/subyard/migrations/transactions/$FAILED_HOTFIX_TRANSACTION_ID" \
    || die "candidate cleanup retained the rolled-back 0.4.2 transaction"
  verify_data_plane
  printf 'ok: published 0.4.0 -> broken 0.4.2 -> recovered 0.4.3 hotfix lane passed\n'
  exit 0
fi
prepare_legacy_owner
upgrade_through_missed_release
upgrade_candidate
verify_control_plane
verify_legacy_power_rollback_cycle
verify_data_plane
printf 'ok: published %s %s lane converged through the candidate\n' "$OLD_VERSION" "$MODE"
