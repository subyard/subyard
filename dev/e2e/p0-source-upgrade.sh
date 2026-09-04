#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
MODE="${1:-}"
TOKEN="${2:-}"
ARCHIVE="${3:-}"
ARCHIVE_SHA256="${4:-}"
SOURCE_REVISION="${5:-}"
# shellcheck source=dev/e2e/lib-p0-capacity.sh
. "$ROOT/dev/e2e/lib-p0-capacity.sh"
# shellcheck source=dev/e2e/lib-p0-init-retry.sh
. "$ROOT/dev/e2e/lib-p0-init-retry.sh"
p0_capacity_init "$TOKEN"

MARKER="subyard-p0-source-$TOKEN"
OPERATOR="subyardp0$TOKEN"
SOURCE_STATE_ROOT="$P0_CAPACITY_STATE_ROOT/source-upgrade"
OPERATOR_HOME="/home/$OPERATOR"
OPERATOR_HOME_MARKER="$OPERATOR_HOME/.subyard-p0-source-home"
SOURCE_ROOT="$OPERATOR_HOME/src"
SHARED_ROOT="/var/tmp/subyard-p0-source-$TOKEN"
RELEASE_ROOT="$SHARED_ROOT/releases"
SUDOERS="/etc/sudoers.d/subyard-p0-source-$TOKEN"
YARD_NAME="e2e-yard"
PROJECT="subyard-e2e-yard"
INSTANCE="yard-e2e-yard"
DEFAULT_PROJECT="subyard"
DEFAULT_INSTANCE="yard"
BASE_IMAGE="${P0_REAL_INCUS_CONTAINER_CACHE_ALIAS:-subyard-e2e-debian-13-cloud-container}"
VERSION_A="p0-source-a-$TOKEN"
VERSION_B="p0-source-b-$TOKEN"
POWER_RETRY_RUNTIME="subyard-p0-source-power-retry-$TOKEN"
POWER_RETRY_PROBE="/run/$POWER_RETRY_RUNTIME/failed-once"
POWER_RETRY_DROPIN="/etc/systemd/system/subyard-power-reconcile.service.d/p0-source-$TOKEN.conf"
POWER_RETRY_WRAPPER="/usr/local/libexec/subyard/p0-source-power-retry-$TOKEN"

die() { printf 'p0-source-upgrade: %s\n' "$*" >&2; exit 2; }
[[ "$TOKEN" =~ ^[0-9]+$ ]] || die 'allocation token must be numeric'
case "${SUBYARD_E2E_VM:-}" in
  1|2) ;;
  *) die 'run on an allocated P0 worker VM through dev/agent-e2e.sh' ;;
esac

# A pristine VM adds the current agent user to incus-admin while this shell is
# already running. Use the disposable VM's passwordless sudo until the next
# login observes that supplementary group; fixture operators still use Incus
# directly through their fresh runuser sessions.
incus() {
  local binary=/usr/bin/incus socket=/var/lib/incus/unix.socket
  [ -x "$binary" ] || return 127
  if [ -S "$socket" ] && [ ! -w "$socket" ]; then
    sudo -n "$binary" "$@"
  else
    "$binary" "$@"
  fi
}

select_current_test_yard() {
  YARD_NAME=test-yard
  PROJECT=subyard-test-yard
  INSTANCE=yard-test-yard
}

operator_uid() { id -u "$OPERATOR"; }
operator_env() {
  local uid
  uid="$(operator_uid)"
  sudo -n /usr/sbin/runuser -u "$OPERATOR" -- bash -c '
    cd "$1"
    shift
    exec "$@"
  ' _ "$OPERATOR_HOME" env \
      HOME="$OPERATOR_HOME" USER="$OPERATOR" LOGNAME="$OPERATOR" SHELL=/bin/bash \
      GOCACHE="$OPERATOR_HOME/.cache/go-build" GOMODCACHE="$OPERATOR_HOME/go/pkg/mod" \
      XDG_RUNTIME_DIR="/run/user/$uid" \
      DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$uid/bus" \
      "$@"
}
operator_no_go() {
  local uid fake="$OPERATOR_HOME/no-go"
  uid="$(operator_uid)"
  sudo -n unshare --mount --fork -- bash -c '
    set -e
    mount --make-rprivate /
    mount --bind "$1" /usr/bin/go
    shift
    cd "$2"
    exec /usr/sbin/runuser -u "$1" -- env \
      HOME="$2" USER="$1" LOGNAME="$1" SHELL=/bin/bash \
      GOCACHE="$2/.cache/go-build" GOMODCACHE="$2/go/pkg/mod" \
      PATH=/usr/sbin:/usr/bin:/sbin:/bin \
      XDG_RUNTIME_DIR="/run/user/$3" \
      DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$3/bus" \
      "${@:4}"
  ' _ "$fake" "$OPERATOR" "$OPERATOR_HOME" "$uid" "$@"
}
operator_yard() {
  operator_no_go "$OPERATOR_HOME/.local/bin/yard" "$@"
}

relax_fixture_init_deadline() {
  local timeout_file="$SOURCE_ROOT/internal/cli/cli.go"
  [ "$(operator_env grep -Fc $'\t\t\tTimeout:        10 * time.Minute,' "$timeout_file")" = 1 ] \
    && [ "$(operator_env grep -Fc $'\t\t\tTimeout:        30 * time.Minute,' "$timeout_file")" = 0 ] \
    || die 'source-upgrade adapter timeout fixture no longer matches its source'
  operator_env sed -i \
    $'s/^\t\t\tTimeout:        10 \\* time.Minute,$/\t\t\tTimeout:        30 * time.Minute,/' \
    "$timeout_file"
  [ "$(operator_env grep -Fc $'\t\t\tTimeout:        10 * time.Minute,' "$timeout_file")" = 0 ] \
    && [ "$(operator_env grep -Fc $'\t\t\tTimeout:        30 * time.Minute,' "$timeout_file")" = 1 ] \
    || die 'source-upgrade adapter timeout fixture was not applied'
}

assert_fixture_project() {
  local project="${1:-$PROJECT}" marker
  marker="$(incus project get "$project" user.subyard.p0-source 2>/dev/null)"
  [ "$marker" = "$MARKER" ] && return 0
  [ -z "$marker" ] && is_markerless_migrated_fixture_project "$project" \
    || die "refusing unmarked Incus project $project"
}

is_markerless_migrated_fixture_project() {
  local project="$1" registration uid initialized revision instances volumes
  registration="$OPERATOR_HOME/.config/subyard/yards/test-yard/config.env"
  [ "$project" = subyard-test-yard ] \
    && [ -d "$SHARED_ROOT" ] && [ ! -L "$SHARED_ROOT" ] && [ -O "$SHARED_ROOT" ] \
    && [ "$(cat "$SHARED_ROOT/.subyard-p0-marker" 2>/dev/null)" = "$MARKER" ] \
    && id "$OPERATOR" >/dev/null 2>&1 \
    && sudo -n test -d "$OPERATOR_HOME" && ! sudo -n test -L "$OPERATOR_HOME" \
    && [ "$(sudo -n cat "$OPERATOR_HOME_MARKER" 2>/dev/null)" = "$MARKER" ] \
    && sudo -n test -f "$registration" && ! sudo -n test -L "$registration" \
    || return 1
  uid="$(id -u "$OPERATOR")"
  [ "$(sudo -n stat -c %u "$registration")" = "$uid" ] \
    && [ "$(sudo -n grep -Fxc 'YARD_TEMPLATE=test-vms' "$registration")" = 1 ] \
    && [ "$(incus project get "$project" restricted 2>/dev/null)" = true ] \
    && [ "$(incus project get "$project" features.images 2>/dev/null)" = false ] \
    || return 1
  instances="$(incus list --project "$project" --format csv -c n)"
  [ -z "$(awk '$0 != "yard-test-yard" { print }' <<<"$instances")" ] \
    || return 1
  volumes="$(incus storage volume list default --project "$project" --format csv -c t,n)"
  [ -z "$(awk -F, '
    !(($1 == "container" && $2 == "yard-test-yard") ||
      ($1 == "custom" && $2 == "yard-srv-test-yard")) { print }
  ' <<<"$volumes")" ] || return 1
  if incus config show yard-test-yard --project "$project" >/dev/null 2>&1; then
    [ "$(incus config get yard-test-yard user.subyard.managed \
      --project "$project" 2>/dev/null)" = true ] \
      && [ "$(incus config get yard-test-yard user.subyard.name \
        --project "$project" 2>/dev/null)" = test-yard ] \
      || return 1
    initialized="$(incus config get yard-test-yard user.subyard.initialized \
      --project "$project" 2>/dev/null)"
    revision="$(incus config get yard-test-yard user.subyard.test_vms_revision \
      --project "$project" 2>/dev/null)"
    case "$initialized:$revision" in
      true:1:*:test-yard | false:) ;;
      *) return 1 ;;
    esac
  fi
}

cleanup_owned_project() {
  local project="$1" instance="$2" fingerprint instance_marker='' type volume
  incus project show "$project" >/dev/null 2>&1 || return 0
  assert_fixture_project "$project"
  if incus config show "$instance" --project "$project" >/dev/null 2>&1; then
    instance_marker="$(incus config get "$instance" user.subyard.managed \
      --project "$project" 2>/dev/null)"
    [ "$instance_marker" = true ] || die "refusing unmarked instance $project/$instance"
    incus delete "$instance" --project "$project" --force >/dev/null
  fi
  while IFS=, read -r type volume; do
    [ -n "$volume" ] || continue
    [ "$type" = custom ] || continue
    incus storage volume delete default "$volume" --project "$project" >/dev/null
  done < <(incus storage volume list default --project "$project" --format csv -c t,n)
  if [ "$(incus project get "$project" features.images 2>/dev/null)" != false ]; then
    while IFS= read -r fingerprint; do
      [ -n "$fingerprint" ] || continue
      incus image delete "$fingerprint" --project "$project" >/dev/null
    done < <(incus image list --project "$project" --format csv -c f)
  fi
  incus project delete "$project" >/dev/null
  sudo -n find "/srv/$project" -depth -delete 2>/dev/null || true
}

cleanup_fixture() {
  local power_unit_changed=0
  if sudo -n test -f "$POWER_RETRY_DROPIN"; then
    sudo -n find "$POWER_RETRY_DROPIN" -delete
    power_unit_changed=1
  fi
  sudo -n rmdir "$(dirname "$POWER_RETRY_DROPIN")" 2>/dev/null || true
  sudo -n find "$POWER_RETRY_WRAPPER" -delete 2>/dev/null || true
  sudo -n find "$POWER_RETRY_PROBE" -delete 2>/dev/null || true
  sudo -n rmdir "$(dirname "$POWER_RETRY_PROBE")" 2>/dev/null || true
  [ "$power_unit_changed" = 0 ] || sudo -n systemctl daemon-reload
  if incus project show subyard-test-yard >/dev/null 2>&1; then
    select_current_test_yard
  fi
  if incus project show "$PROJECT" >/dev/null 2>&1; then
    assert_fixture_project
    if id "$OPERATOR" >/dev/null 2>&1 \
      && sudo -n test -x "$OPERATOR_HOME/.local/bin/yard"; then
      operator_yard -Y "$YARD_NAME" teardown --yes >/dev/null 2>&1 \
        || printf '  [warn] fixture yard teardown failed; using marker-guarded cleanup\n' >&2
    fi
  fi
  cleanup_owned_project "$PROJECT" "$INSTANCE"
  if incus project show "$DEFAULT_PROJECT" >/dev/null 2>&1; then
    if id "$OPERATOR" >/dev/null 2>&1 \
      && sudo -n test -x "$OPERATOR_HOME/.local/bin/yard"; then
      operator_yard teardown --yes >/dev/null 2>&1 \
        || printf '  [warn] default fixture yard teardown failed; using marker-guarded cleanup\n' >&2
    fi
  fi
  cleanup_owned_project "$DEFAULT_PROJECT" "$DEFAULT_INSTANCE"
  if id "$OPERATOR" >/dev/null 2>&1; then
    sudo -n loginctl disable-linger "$OPERATOR" >/dev/null 2>&1 || true
    sudo -n systemctl stop "user@$(operator_uid).service" >/dev/null 2>&1 || true
  fi
  sudo -n find "$SUDOERS" -delete 2>/dev/null || true
  if id "$OPERATOR" >/dev/null 2>&1; then
    sudo -n test -d "$OPERATOR_HOME" && ! sudo -n test -L "$OPERATOR_HOME" \
      && [ "$(sudo -n cat "$OPERATOR_HOME_MARKER" 2>/dev/null)" = "$MARKER" ] \
      || die "refusing unmarked fixture operator cleanup: $OPERATOR_HOME"
    sudo -n userdel -r "$OPERATOR" >/dev/null
  fi
  if [ -e "$OPERATOR_HOME" ]; then
    sudo -n test -d "$OPERATOR_HOME" && ! sudo -n test -L "$OPERATOR_HOME" \
      && [ "$(sudo -n cat "$OPERATOR_HOME_MARKER" 2>/dev/null)" = "$MARKER" ] \
      || die "refusing unmarked fixture home cleanup: $OPERATOR_HOME"
    sudo -n find "$OPERATOR_HOME" -xdev -depth -delete
  fi
  if [ -e "$SHARED_ROOT" ]; then
    [ -d "$SHARED_ROOT" ] && [ ! -L "$SHARED_ROOT" ] \
      && [ "$(cat "$SHARED_ROOT/.subyard-p0-marker" 2>/dev/null)" = "$MARKER" ] \
      || die "refusing unmarked shared fixture root $SHARED_ROOT"
    find "$SHARED_ROOT" -xdev -depth -delete
  fi
  p0_capacity_remove_subtree "$SOURCE_STATE_ROOT"
  p0_capacity_remove_build_cache
  p0_capacity_remove_root_if_empty
}

set_operator_sudo_policy() {
  local policy="$1" rule sudoers_tmp
  case "$policy" in
    passwordless) rule='NOPASSWD: ALL' ;;
    password) rule='ALL' ;;
    *) die "invalid operator sudo policy: $policy" ;;
  esac
  sudoers_tmp="$(mktemp /tmp/subyard-p0-sudoers.XXXXXX)"
  printf '%s ALL=(root) %s\n' "$OPERATOR" "$rule" > "$sudoers_tmp"
  sudo -n visudo -cf "$sudoers_tmp" >/dev/null
  sudo -n install -o root -g root -m 0440 "$sudoers_tmp" "$SUDOERS"
  find "$sudoers_tmp" -delete
}

assert_operator_password_sudo() {
  local rc
  operator_env sudo -k
  set +e
  operator_env sudo -n true >/dev/null 2>&1
  rc=$?
  set -e
  [ "$rc" -ne 0 ] || die 'operator unexpectedly retained passwordless sudo'
}

require_operator_password_sudo() {
  printf '%s:%s\n' "$OPERATOR" 'subyard-disposable-power-fixture' | sudo -n chpasswd
  set_operator_sudo_policy password
  assert_operator_password_sudo
  printf '  [ ok ] fixture operator requires a sudo password across reboot\n'
}

restore_operator_passwordless_sudo() {
  set_operator_sudo_policy passwordless
  operator_env sudo -k
  operator_env sudo -n true >/dev/null 2>&1 \
    || die 'fixture operator passwordless sudo was not restored'
}

prepare_operator() {
  local uid
  ! id "$OPERATOR" >/dev/null 2>&1 || die "fixture user $OPERATOR already exists"
  [ ! -e "$OPERATOR_HOME" ] || die "fixture home already exists: $OPERATOR_HOME"
  [ ! -e "$RELEASE_ROOT" ] || die "fixture release root already exists"
  sudo -n useradd --create-home --home-dir "$OPERATOR_HOME" --shell /bin/bash "$OPERATOR"
  operator_env bash -c 'printf "%s\n" "$2" > "$1"; chmod 0600 "$1"' _ \
    "$OPERATOR_HOME_MARKER" "$MARKER"
  sudo -n usermod -aG incus-admin "$OPERATOR"
  set_operator_sudo_policy passwordless
  sudo -n loginctl enable-linger "$OPERATOR"
  uid="$(operator_uid)"
  sudo -n systemctl start "user@$uid.service"
  for _ in $(seq 1 30); do
    sudo -n test -S "/run/user/$uid/bus" && break
    sleep 1
  done
  sudo -n test -S "/run/user/$uid/bus" || die 'fixture user bus did not start'
  operator_env install -d -m 0700 "$SOURCE_ROOT"
  sudo -n install -o "$OPERATOR" -g "$OPERATOR" -m 0600 \
    "$ARCHIVE" "$OPERATOR_HOME/source.tar.gz"
  operator_env tar -xzf "$OPERATOR_HOME/source.tar.gz" -C "$SOURCE_ROOT"
  operator_env find "$OPERATOR_HOME/source.tar.gz" -delete
  operator_env install -d -m 0700 \
    "$SOURCE_ROOT/private/yards" "$SOURCE_ROOT/private/agents/codex" \
    "$SOURCE_ROOT/config/profiles/openclaw" "$SOURCE_ROOT/config/staging" \
    "$SOURCE_ROOT/config/qa-pool" \
    "$OPERATOR_HOME/.local/bin"
  # This fixture verifies the private Codex asset migration without downloading
  # unrelated agent CLIs from the network.
  operator_env bash -c 'printf "%s" "$2" > "$1"' _ "$SOURCE_ROOT/private/config.env" \
    $'DEV_SUDO=1\nAGENTS=codex\nCODING_TOOL_INTEGRATIONS=codex\nAGENT_codex_RULES="$SUBYARD_CONFIG_DIR/../private/agents/codex/repo.rules"\n'
  operator_env bash -c \
    'printf "YARD_TEMPLATE=e2e-vms\nSSH_PORT=2223\nDEV_UID=1001\nYARD_IMAGE=%s\nYARD_IMAGE_FALLBACK=%s\n" "$2" "$2" > "$1"' \
    _ "$SOURCE_ROOT/private/yards/e2e-yard.env" "$BASE_IMAGE"
  operator_env bash -c 'printf "source-upgrade-fixture\n" > "$1"' _ \
    "$SOURCE_ROOT/private/agents/codex/repo.rules"
  operator_env bash -c 'printf "PROFILE_TOKEN=source-profile-fixture\n" > "$1"' _ \
    "$SOURCE_ROOT/config/profiles/openclaw/profile.env"
  operator_env bash -c \
    'printf "PROFILE=openclaw\n" > "$1"; printf "STAGING_TOKEN=source-staging-fixture\n" > "$2"' _ \
    "$SOURCE_ROOT/config/staging/canonical.conf" "$SOURCE_ROOT/config/staging/canonical.env"
  operator_env bash -c \
    'printf "source-fingerprint\n" > "$1"; printf "CLOUD_PORT=3210\n" > "$2"; printf "QA_SECRET=source-qa-fixture\n" > "$3"; printf "{\"fixture\":true}\n" > "$4"; printf "retain-me\n" > "$5"' _ \
    "$SOURCE_ROOT/config/prod-fingerprints" \
    "$SOURCE_ROOT/config/qa-pool/broker.conf" \
    "$SOURCE_ROOT/config/qa-pool/secrets.env" \
    "$SOURCE_ROOT/config/qa-pool/pool.jsonl" \
    "$SOURCE_ROOT/config/qa-pool/operator-note.local"
  operator_env chmod 0600 \
    "$SOURCE_ROOT/private/config.env" "$SOURCE_ROOT/private/yards/e2e-yard.env" \
    "$SOURCE_ROOT/private/agents/codex/repo.rules" \
    "$SOURCE_ROOT/config/profiles/openclaw/profile.env" \
    "$SOURCE_ROOT/config/staging/canonical.conf" "$SOURCE_ROOT/config/staging/canonical.env" \
    "$SOURCE_ROOT/config/prod-fingerprints" \
    "$SOURCE_ROOT/config/qa-pool/broker.conf" \
    "$SOURCE_ROOT/config/qa-pool/secrets.env" \
    "$SOURCE_ROOT/config/qa-pool/pool.jsonl" \
    "$SOURCE_ROOT/config/qa-pool/operator-note.local"
  # Clean network provisioning can exceed the production ten-minute adapter
  # deadline under the full parallel matrix. Relax only this disposable source
  # build; the shipped CLI keeps its production deadline.
  relax_fixture_init_deadline
  operator_env env YARD_BUILD_VERSION="source-$SOURCE_REVISION" \
    "$SOURCE_ROOT/scripts/build-engine.sh" --force
  operator_env ln -s "$SOURCE_ROOT/bin/yard" "$OPERATOR_HOME/.local/bin/yard"
  operator_env ln -s "$SOURCE_ROOT/bin/yard" "$OPERATOR_HOME/.local/bin/sy"
  operator_env bash -c \
    'printf "# Subyard CLI\nexport PATH=\"%s/.local/bin:\\$PATH\"\n# Subyard CLI completion\n[ -f \"%s/completions/yard.bash\" ] && source \"%s/completions/yard.bash\"\n" "$1" "$2" "$2" > "$1/.bashrc"; printf "# Subyard CLI login PATH\nexport PATH=\"%s/.local/bin:\\$PATH\"\n" "$1" > "$1/.profile"' \
    _ "$OPERATOR_HOME" "$SOURCE_ROOT" "$OPERATOR_HOME"
  operator_env bash -c \
    'printf "#!/bin/sh\nprintf invoked > \"%s/go-invoked\"\nexit 127\n" "$1" > "$1/no-go"; chmod 0700 "$1/no-go"' \
    _ "$OPERATOR_HOME"
}

seed_previous_migration_inputs() {
  operator_env install -d -m 0700 \
    "$OPERATOR_HOME/.subyard/operator-overlay/private/agents/codex" \
    "$OPERATOR_HOME/.subyard/operator-overlay/private/agents/claude"
  operator_env cp "$SOURCE_ROOT/private/config.env" "$OPERATOR_HOME/.subyard/config.env"
  operator_env cp "$SOURCE_ROOT/private/agents/codex/repo.rules" \
    "$OPERATOR_HOME/.subyard/operator-overlay/private/agents/codex/repo.rules"
  operator_env bash -c 'printf "{\"fixture\":true}\n" > "$1"' _ \
    "$OPERATOR_HOME/.subyard/operator-overlay/private/agents/claude/settings.json"
  operator_env chmod 0600 "$OPERATOR_HOME/.subyard/config.env" \
    "$OPERATOR_HOME/.subyard/operator-overlay/private/agents/codex/repo.rules" \
    "$OPERATOR_HOME/.subyard/operator-overlay/private/agents/claude/settings.json"
}

package_candidates() {
  local fstype
  install -d -m 0700 "$RELEASE_ROOT/a" "$RELEASE_ROOT/b"
  fstype="$(findmnt -n -o FSTYPE --target "$RELEASE_ROOT")" \
    || die "cannot identify release fixture filesystem"
  case "$fstype" in
    tmpfs | ramfs) die "release fixture must survive reboot" ;;
  esac
  printf '%s\n' "$MARKER" > "$RELEASE_ROOT/.subyard-p0-marker"
  printf '%s\n' "$SOURCE_REVISION" > "$RELEASE_ROOT/source-revision"
  "$ROOT/dev/package-engine.sh" --output-dir "$RELEASE_ROOT/a" --version "$VERSION_A" >/dev/null
  "$ROOT/dev/package-engine.sh" --output-dir "$RELEASE_ROOT/b" --version "$VERSION_B" >/dev/null
  chmod -R a+rX "$RELEASE_ROOT"
}

bootstrap_candidate() {
  local release="$1" version="$2"
  operator_no_go env \
    YARD_RELEASE_BASE_URL="file://$release" YARD_RELEASE_VERSION="$version" \
    "$release/subyard-install.sh" --yes
  operator_env test ! -e "$OPERATOR_HOME/go-invoked" \
    || die 'standalone installer invoked Go'
}

verify_migration() {
  local runtime="$OPERATOR_HOME/.subyard/runtime/current/bin/yard"
  [ "$(operator_env readlink "$OPERATOR_HOME/.local/bin/yard")" = "$runtime" ] \
    && [ "$(operator_env readlink "$OPERATOR_HOME/.local/bin/sy")" = "$runtime" ] \
    || die 'source entrypoints did not switch to the immutable runtime'
  operator_env test -x "$SOURCE_ROOT/.build/yard" \
    || die 'source checkout was changed or removed'
  operator_env cmp "$SOURCE_ROOT/private/config.env" "$OPERATOR_HOME/.config/subyard/config.env" \
    || die 'host settings were not migrated'
  operator_env bash -c \
    'sed "s/^YARD_TEMPLATE=e2e-vms$/YARD_TEMPLATE=test-vms/" "$1" | cmp - "$2"' _ \
    "$SOURCE_ROOT/private/yards/e2e-yard.env" \
    "$OPERATOR_HOME/.config/subyard/yards/test-yard/config.env" \
    || die 'named test yard was normalized and renamed'
  operator_env cmp "$SOURCE_ROOT/private/agents/codex/repo.rules" \
    "$OPERATOR_HOME/.config/subyard/overrides/host/agents/codex/repo.rules" \
    || die 'private agent asset was not migrated'
  operator_env test -f \
    "$OPERATOR_HOME/.config/subyard/overrides/host/agents/claude/settings.json" \
    || die 'transitional operator overlay was not migrated'
  operator_env cmp "$SOURCE_ROOT/config/profiles/openclaw/profile.env" \
    "$OPERATOR_HOME/.config/subyard/secrets/profiles/openclaw/profile.env" \
    || die 'profile secret was not migrated'
  operator_env cmp "$SOURCE_ROOT/config/staging/canonical.conf" \
    "$OPERATOR_HOME/.config/subyard/overrides/host/staging/canonical.conf" \
    || die 'staging config was not migrated'
  operator_env cmp "$SOURCE_ROOT/config/staging/canonical.env" \
    "$OPERATOR_HOME/.config/subyard/secrets/legacy/staging/canonical.env" \
    || die 'legacy staging secret was not retained'
  operator_env cmp "$SOURCE_ROOT/config/qa-pool/operator-note.local" \
    "$OPERATOR_HOME/.config/subyard/secrets/legacy/unclassified/qa-pool/operator-note.local" \
    || die 'unclassified ignored input was not retained'
  operator_env test ! -e "$OPERATOR_HOME/.subyard/config.env" \
    || die 'legacy host settings remained under the data home'
  operator_env test ! -e "$OPERATOR_HOME/.subyard/operator-overlay" \
    || die 'legacy operator overlay remained under the data home'
  operator_env test -x "$OPERATOR_HOME/.subyard/recovery/pre-go-source/restore.sh" \
    || die 'guarded source recovery was not retained'
}

verify_v2_release_transition() { # <version>
  local expected="$1"
  [ "$(operator_yard --version)" = "yard $expected" ] \
    || die "release transition did not activate $expected"
  jq -e '
    .schemaVersion == 2 and .domains.settings.epoch == 2 and
    .domains.settings.applied == ["canonicalize-test-vms-settings-v2"]
  ' < <(operator_env cat \
    "$OPERATOR_HOME/.config/subyard/release-transition/v2/ledger.json") >/dev/null \
    || die 'release transition domain ledger is not at the verified fixed point'
  jq -e --arg version "$expected" '
    .schemaVersion == 2 and .checkpoint == "complete" and
    (.goal.target | startswith($version + "-"))
  ' < <(operator_env cat \
    "$OPERATOR_HOME/.config/subyard/release-transition/v2/journal.json") >/dev/null \
    || die 'release transition journal is not complete for the expected runtime'
}

verify_config_workflow() {
  local default_show paths show_output host_hash guest_hash status_output status_rc
  default_show="$(operator_yard config show DEV_SUDO)"
  grep -Fq 'effective: 1' <<<"$default_show" \
    && grep -Fq "$OPERATOR_HOME/.config/subyard/config.env" <<<"$default_show" \
    || die 'default-yard config did not consume migrated host settings'
  paths="$(operator_yard -Y "$YARD_NAME" config paths)"
  grep -Fq "configuration-root: $OPERATOR_HOME/.config/subyard" <<<"$paths" \
    || die 'config paths did not report the persistent configuration root'
  grep -Fq \
    "$OPERATOR_HOME/.config/subyard/overrides/host/agents/codex/repo.rules (scope=host, role=file settings, consumer=" \
    <<<"$paths" || die 'config paths did not resolve the migrated Codex asset'
  ! grep -Fq 'source-staging-fixture' <<<"$paths" \
    || die 'config paths printed a secret value'
  show_output="$(operator_yard -Y "$YARD_NAME" config show SSH_PORT)"
  grep -Fq 'effective: 2223' <<<"$show_output" \
    && grep -Fq "$OPERATOR_HOME/.config/subyard/yards/test-yard/config.env" <<<"$show_output" \
    && grep -Fq 'effective' <<<"$show_output" \
    || die 'config show did not explain the effective yard setting'
  ! grep -Eq 'source-(staging|qa|profile)-fixture' <<<"$show_output" \
    || die 'config show printed a secret value'
  set +e
  status_output="$(operator_yard -Y "$YARD_NAME" config status --all-local 2>&1)"
  status_rc=$?
  set -e
  printf '%s\n' "$status_output"
  if [ "$status_rc" -ne 0 ]; then
    [ "$status_rc" -eq 1 ] \
      && grep -Fq 'yard test-yard materialized-config: drift' <<<"$status_output" \
      && grep -Fq 'config status: materialized agent config drift in yards: test-yard' \
        <<<"$status_output" \
      || die 'config status failed for a reason other than expected agent drift'
  fi
  ! grep -Eq 'source-(staging|qa|profile)-fixture' <<<"$status_output" \
    || die 'config status printed a secret value'
  operator_yard -Y "$YARD_NAME" config apply --all-local --yes
  operator_yard -Y "$YARD_NAME" config status --all-local
  host_hash="$(sudo -n sha256sum \
    "$OPERATOR_HOME/.config/subyard/overrides/host/agents/codex/repo.rules" | awk '{print $1}')"
  guest_hash="$(incus exec "$INSTANCE" --project "$PROJECT" --user 1001 --group 1001 -- \
    sha256sum /home/dev/.codex/rules/repo.rules | awk '{print $1}')"
  [ "$host_hash" = "$guest_hash" ] || die 'migrated Codex rules were not applied to the yard'
}

verify_without_source_checkout() {
  local unavailable="$OPERATOR_HOME/src.unavailable"
  operator_env mv "$SOURCE_ROOT" "$unavailable"
  if ! operator_yard -Y "$YARD_NAME" config paths >/dev/null \
    || ! operator_yard -Y "$YARD_NAME" config status --all-local \
    || ! operator_yard -Y "$YARD_NAME" check; then
    operator_env mv "$unavailable" "$SOURCE_ROOT"
    die 'installed runtime still depends on the source checkout'
  fi
  operator_env mv "$unavailable" "$SOURCE_ROOT"
}

wait_for_desired_yards() {
  local expected_named="$1" expected_default="$2" _ named_state='' default_state=''
  for _ in $(seq 1 60); do
    named_state="$(incus list "$INSTANCE" --project "$PROJECT" -f csv -c s 2>/dev/null)" \
      || named_state=''
    default_state="$(incus list "$DEFAULT_INSTANCE" --project "$DEFAULT_PROJECT" \
      -f csv -c s 2>/dev/null)" || default_state=''
    [ "$named_state" = "$expected_named" ] \
      && [ "$default_state" = "$expected_default" ] \
      && return 0
    sleep 1
  done
  printf 'named project=%s instance=%s state=%s expected=%s\n' \
    "$PROJECT" "$INSTANCE" "${named_state:-unavailable}" "$expected_named" >&2
  printf 'default project=%s instance=%s state=%s expected=%s\n' \
    "$DEFAULT_PROJECT" "$DEFAULT_INSTANCE" "${default_state:-unavailable}" \
    "$expected_default" >&2
  sudo -n systemctl --no-pager --full status subyard-power-reconcile.service >&2 || true
  sudo -n journalctl -u subyard-power-reconcile.service -b --no-pager -n 120 >&2 || true
  return 1
}

install_power_retry_probe() {
  local dropin wrapper
  dropin="$(mktemp)"
  wrapper="$(mktemp)"
  printf '%s\n' \
    '#!/bin/sh' \
    'set -eu' \
    "probe=$POWER_RETRY_PROBE" \
    'if [ ! -e "$probe" ]; then' \
    '  echo "subyard-p0-source: injected transient power readiness failure" >&2' \
    '  : > "$probe"' \
    '  exit 75' \
    'fi' \
    'exec /usr/local/libexec/subyard/yard-boot-reconcile _power-reconcile' \
    > "$wrapper"
  printf '%s\n' \
    '[Service]' \
    "RuntimeDirectory=$POWER_RETRY_RUNTIME" \
    'RuntimeDirectoryPreserve=restart' \
    'ExecStart=' \
    "ExecStart=$POWER_RETRY_WRAPPER" \
    > "$dropin"
  sudo -n install -d -o root -g root -m 0755 "$(dirname "$POWER_RETRY_WRAPPER")"
  sudo -n install -o root -g root -m 0755 "$wrapper" "$POWER_RETRY_WRAPPER"
  sudo -n install -d -o root -g root -m 0755 "$(dirname "$POWER_RETRY_DROPIN")"
  sudo -n install -o root -g root -m 0644 "$dropin" "$POWER_RETRY_DROPIN"
  find "$dropin" "$wrapper" -delete
  sudo -n systemctl daemon-reload
}

verify_power_retry_probe() {
  local failures restarts
  failures="$(sudo -n journalctl -b -u subyard-power-reconcile.service --no-pager -o cat \
    | grep -Fc 'subyard-p0-source: injected transient power readiness failure' || true)"
  [ "$failures" = 1 ] \
    || die "boot power reconciler retry probe failures=$failures, want 1"
  restarts="$(sudo -n systemctl show subyard-power-reconcile.service \
    --property=NRestarts --value)"
  [[ "$restarts" =~ ^[0-9]+$ ]] && [ "$restarts" -ge 1 ] \
    || die "boot power reconciler automatic restarts=$restarts, want at least 1"
  sudo -n find "$POWER_RETRY_DROPIN" -delete
  sudo -n rmdir "$(dirname "$POWER_RETRY_DROPIN")" 2>/dev/null || true
  sudo -n systemctl daemon-reload
}

prepare_project() {
  local project="${1:-$PROJECT}"
  incus project show "$project" >/dev/null 2>&1 \
    && die "refusing to replace existing project $project"
  incus image info "$BASE_IMAGE" --project default >/dev/null 2>&1 \
    || die "test base image $BASE_IMAGE is unavailable"
  incus project create "$project" \
    -c features.images=false -c user.subyard.p0-source="$MARKER" >/dev/null
}

prepare_default_yard() {
  prepare_project "$DEFAULT_PROJECT"
  p0_retry_init_after_plan_stale operator_yard init --yes
  operator_yard start --yes
  operator_yard check
  [ "$(incus config get "$DEFAULT_INSTANCE" user.subyard.desired_power \
    --project "$DEFAULT_PROJECT")" = running ] \
    || die 'default yard did not persist desired=running before update'
}

run_incus_installer() {
  p0_capacity_prepare_platform_root
  (
    # shellcheck source=tests/helpers/test-context.sh
    . "$ROOT/tests/helpers/test-context.sh"
    setup_test_context "$P0_CAPACITY_PLATFORM_ROOT/incus/p0-source-bootstrap-$TOKEN"
    export SUBYARD_USER
    SUBYARD_USER="$(id -un)"
    export SUBYARD_OPERATOR_HOME="$HOME"
    export SUBYARD_CONFIG_DIR="$ROOT/config"
    export SUBYARD_CONFIG_HOME="$HOME/.config/subyard"
    export SUBYARD_HOME="$P0_CAPACITY_PLATFORM_ROOT/incus"
    export STORAGE_PATH="$SUBYARD_HOME/incus/storage"
    export HOST_BASE="$SUBYARD_HOME/p0-source-host-data-$TOKEN"
    export RESTRICTED_DISK_PATHS="$HOST_BASE"
    set -a
    # shellcheck source=config/host.env
    . "$ROOT/config/host.env"
    set +a
    bash "$ROOT/scripts/01-install-incus.sh" "$@"
  )
}

prepare() {
  [[ "$ARCHIVE" =~ ^/tmp/subyard-p0-source-[0-9]+\.tar\.gz$ ]] \
    || die 'source archive path is invalid'
  [[ "$ARCHIVE_SHA256" =~ ^[0-9a-f]{64}$ ]] || die 'source archive hash is invalid'
  [[ "$SOURCE_REVISION" =~ ^[0-9a-f]{40}$ ]] || die 'source revision is invalid'
  [ "$(sha256sum "$ARCHIVE" | cut -d' ' -f1)" = "$ARCHIVE_SHA256" ] \
    || die 'source archive checksum mismatch'
  command -v go >/dev/null 2>&1 || die 'fixture preparation needs Go'
  command -v unshare >/dev/null 2>&1 || die 'unshare is required'
  p0_capacity_reset_build_cache
  if ! incus info >/dev/null 2>&1 \
    || ! incus storage show default --project default >/dev/null 2>&1 \
    || ! incus network show incusbr0 --project default >/dev/null 2>&1; then
    run_incus_installer --yes --zabbly
  fi
  if ! incus image info "$BASE_IMAGE" --project default >/dev/null 2>&1; then
    bash "$ROOT/dev/e2e/p0-real-incus.sh"
  fi
  cleanup_fixture
  p0_capacity_reset_build_cache
  p0_capacity_prepare_subtree "$SOURCE_STATE_ROOT"
  chmod 0711 "$SOURCE_STATE_ROOT"
  install -d -m 0711 "$SHARED_ROOT"
  printf '%s\n' "$MARKER" > "$SHARED_ROOT/.subyard-p0-marker"
  prepare_operator
  package_candidates
  prepare_project

  [ "$(operator_no_go "$SOURCE_ROOT/bin/yard" --version)" = "yard source-$SOURCE_REVISION" ] \
    || die 'exact source-linked CLI is not operational without Go'
  p0_retry_init_after_plan_stale operator_yard -Y "$YARD_NAME" init --yes
  operator_yard -Y "$YARD_NAME" start --yes
  operator_yard -Y "$YARD_NAME" check
  seed_previous_migration_inputs
  [ "$(incus config get "$INSTANCE" user.subyard.desired_power --project "$PROJECT")" = running ] \
    || die 'legacy yard did not persist desired=running before the stopped-upgrade fixture'
  incus stop "$INSTANCE" --project "$PROJECT"
  [ "$(incus list "$INSTANCE" --project "$PROJECT" -f csv -c s)" = STOPPED ] \
    || die 'legacy yard did not enter the stopped desired-running upgrade fixture'

  bootstrap_candidate "$RELEASE_ROOT/a" "$VERSION_A"
  select_current_test_yard
  incus project set "$PROJECT" user.subyard.p0-source="$MARKER"
  verify_migration
  [ "$(operator_yard --version)" = "yard $VERSION_A" ] \
    || die 'first candidate runtime is not active'
  bootstrap_candidate "$RELEASE_ROOT/a" "$VERSION_A"
  [ "$(operator_env grep -Fc '# Subyard CLI completion' "$OPERATOR_HOME/.bashrc")" = 1 ] \
    || die 'repeated bootstrap duplicated shell integration'
  [ "$(incus list "$INSTANCE" --project "$PROJECT" -f csv -c s)" = RUNNING ] \
    || die 'runtime upgrade did not recreate test-yard as running'
  [ "$(incus config get "$INSTANCE" user.subyard.desired_power --project "$PROJECT")" = running ] \
    || die 'test-yard migration did not establish desired=running'
  operator_yard -Y "$YARD_NAME" check
  p0_retry_init_after_plan_stale operator_yard -Y "$YARD_NAME" init --yes
  verify_config_workflow
  verify_without_source_checkout
  prepare_default_yard
  operator_yard stop --yes

  verify_v2_release_transition "$VERSION_A"
  [ "$(incus config get "$INSTANCE" user.subyard.desired_power --project "$PROJECT")" = running ] \
    && [ "$(incus config get "$DEFAULT_INSTANCE" user.subyard.desired_power \
      --project "$DEFAULT_PROJECT")" = stopped ] \
    && [ "$(incus config get "$INSTANCE" boot.autostart --project "$PROJECT")" = false ] \
    && [ "$(incus config get "$DEFAULT_INSTANCE" boot.autostart \
      --project "$DEFAULT_PROJECT")" = false ] \
    || die 'named running and default stopped desired power is not persisted before reboot'
  operator_env test ! -e "$OPERATOR_HOME/go-invoked" \
    || die 'production operator cycle invoked Go'
  require_operator_password_sudo
  printf 'ok: exact source-linked %s upgraded without Go; migration is prepared for reboot\n' \
    "$SOURCE_REVISION"
}

load_rebooted_fixture() {
  select_current_test_yard
  SOURCE_REVISION="$(cat "$RELEASE_ROOT/source-revision" 2>/dev/null)" \
    || die 'source revision metadata disappeared after reboot'
  [[ "$SOURCE_REVISION" =~ ^[0-9a-f]{40}$ ]] \
    || die 'source revision metadata is invalid'
  id "$OPERATOR" >/dev/null 2>&1 || die 'fixture operator disappeared after reboot'
  assert_fixture_project
  assert_fixture_project "$DEFAULT_PROJECT"
  assert_operator_password_sudo
}

resume() {
  load_rebooted_fixture
  wait_for_desired_yards RUNNING STOPPED \
    || die 'boot reconciler did not restore named running/default stopped states'
  [ "$(incus config get "$INSTANCE" boot.autostart --project "$PROJECT")" = false ] \
    && [ "$(incus config get "$DEFAULT_INSTANCE" boot.autostart \
      --project "$DEFAULT_PROJECT")" = false ] \
    || die 'boot reconciler changed boot.autostart during the first reboot'
  restore_operator_passwordless_sudo
  verify_v2_release_transition "$VERSION_A"

  operator_env chmod 0755 "$OPERATOR_HOME/.config/subyard/yards/test-yard"
  operator_env chmod 0640 \
    "$OPERATOR_HOME/.config/subyard/yards/test-yard/config.env"

  operator_no_go env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT/b" \
    "$OPERATOR_HOME/.local/bin/yard" update --version "$VERSION_B" --yes
  verify_v2_release_transition "$VERSION_B"
  verify_config_workflow
  operator_env chmod 0600 \
    "$OPERATOR_HOME/.config/subyard/yards/test-yard/config.env"
  operator_yard -Y "$YARD_NAME" stop --yes
  operator_yard start --yes
  [ "$(incus config get "$INSTANCE" user.subyard.desired_power --project "$PROJECT")" = stopped ] \
    && [ "$(incus config get "$DEFAULT_INSTANCE" user.subyard.desired_power \
      --project "$DEFAULT_PROJECT")" = running ] \
    && [ "$(incus config get "$INSTANCE" boot.autostart --project "$PROJECT")" = false ] \
    && [ "$(incus config get "$DEFAULT_INSTANCE" boot.autostart \
      --project "$DEFAULT_PROJECT")" = false ] \
    || die 'named stopped and default running desired power is not persisted before committed-state reboot'
  operator_env test ! -e "$OPERATOR_HOME/go-invoked" \
    || die 'v0.1-style update invoked Go'
  install_power_retry_probe
  require_operator_password_sudo
  printf 'ok: prepared migration resumed through the v0.1 installer and is committed for reboot\n'
}

finish() {
  load_rebooted_fixture
  [ "$(operator_yard --version)" = "yard $VERSION_B" ] \
    || die 'runtime entrypoint did not survive reboot'
  wait_for_desired_yards STOPPED RUNNING \
    || die 'updated boot reconciler did not restore named stopped/default running states'
  [ "$(incus config get "$INSTANCE" user.subyard.desired_power --project "$PROJECT")" = stopped ] \
    && [ "$(incus config get "$DEFAULT_INSTANCE" user.subyard.desired_power \
      --project "$DEFAULT_PROJECT")" = running ] \
    && [ "$(incus config get "$INSTANCE" boot.autostart --project "$PROJECT")" = false ] \
    && [ "$(incus config get "$DEFAULT_INSTANCE" boot.autostart \
      --project "$DEFAULT_PROJECT")" = false ] \
    || die 'default/named desired power or boot.autostart changed across reboot'
  [ "$(sudo -n systemctl show subyard-power-reconcile.service --property=Result --value)" = success ] \
    || die 'updated boot power reconciliation did not finish successfully'
  verify_power_retry_probe
  restore_operator_passwordless_sudo
  operator_yard -Y "$YARD_NAME" start --yes
  operator_yard status >/dev/null
  operator_yard check
  operator_yard -Y "$YARD_NAME" status >/dev/null
  operator_yard -Y "$YARD_NAME" check
  p0_retry_init_after_plan_stale operator_yard -Y "$YARD_NAME" init --yes
  verify_v2_release_transition "$VERSION_B"
  verify_config_workflow

  operator_yard update --rollback --yes
  verify_v2_release_transition "$VERSION_A"
  verify_config_workflow
  operator_no_go env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT/b" \
    "$OPERATOR_HOME/.local/bin/yard" update --version "$VERSION_B" --yes
  verify_v2_release_transition "$VERSION_B"
  verify_config_workflow

  if operator_no_go \
    "$OPERATOR_HOME/.subyard/recovery/pre-go-source/restore.sh" >/dev/null 2>&1; then
    die 'committed source import allowed a restore that invalidates v2 history'
  fi
  [ "$(operator_yard --version)" = "yard $VERSION_B" ] \
    || die 'rejected source restore changed the active runtime'
  verify_migration
  p0_retry_init_after_plan_stale operator_yard -Y "$YARD_NAME" init --yes
  operator_yard -Y "$YARD_NAME" check
  verify_config_workflow
  operator_yard -Y "$YARD_NAME" teardown --yes
  ! incus project show "$PROJECT" >/dev/null 2>&1 \
    || die 'upgraded yard remains after teardown'
  operator_yard teardown --yes
  ! incus project show "$DEFAULT_PROJECT" >/dev/null 2>&1 \
    || die 'upgraded default yard remains after teardown'
  operator_env test ! -e "$OPERATOR_HOME/go-invoked" \
    || die 'post-reboot operator cycle invoked Go'
  cleanup_fixture
  printf 'ok: source upgrade survived reboot, rollback and sealed recovery\n'
}

case "$MODE" in
  prepare) prepare ;;
  resume) resume ;;
  finish) finish ;;
  clean) cleanup_fixture ;;
  *) die 'expected prepare, resume, finish or clean' ;;
esac
