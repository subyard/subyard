#!/usr/bin/env bash
# Published v0.11.0 owner recreation, retained ordinary yard and installed-runtime recovery.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
MODE="${1:-}"
TOKEN="${2:-}"
# shellcheck source=dev/e2e/lib-p0-capacity.sh
. "$ROOT/dev/e2e/lib-p0-capacity.sh"
# shellcheck source=dev/e2e/lib-p0-init-retry.sh
. "$ROOT/dev/e2e/lib-p0-init-retry.sh"
p0_capacity_init "$TOKEN"

MARKER="subyard-p0-source-$TOKEN"
OPERATOR="subyardp0$TOKEN"
SOURCE_STATE_ROOT="$P0_CAPACITY_STATE_ROOT/source-upgrade"
CONFIG_CHECKOUT=''
OPERATOR_HOME="/home/$OPERATOR"
OPERATOR_HOME_MARKER="$OPERATOR_HOME/.subyard-p0-source-home"
SHARED_ROOT="/var/tmp/subyard-p0-source-$TOKEN"
RELEASE_ROOT="$SHARED_ROOT/releases"
CANDIDATE_A_REPOSITORY="$SHARED_ROOT/candidate-a-runtime"
CANDIDATE_A_ENGINE="$CANDIDATE_A_REPOSITORY/bin/yard-engine"
SUDOERS="/etc/sudoers.d/subyard-p0-source-$TOKEN"
YARD_NAME="e2e-yard"
PROJECT="subyard-e2e-yard"
INSTANCE="yard-e2e-yard"
DEFAULT_PROJECT="subyard"
DEFAULT_INSTANCE="yard"
# Reuse the exact published-release download/verification helper.
# shellcheck source=dev/e2e/published-baseline.sh
. "$ROOT/dev/e2e/published-baseline.sh"
STATE_ROOT="$SHARED_ROOT"
IFS=$'\t' read -r BASELINE_VERSION BASELINE_INSTALLER_SHA256 BASELINE_AMD64_SHA256 < <(
  awk -F '\t' '$1 == "0.11.0"' "$ROOT/dev/e2e/published-upgrade-baselines.tsv"
)
# The existing published-upgrade pins and local P0 VM matrix are amd64.
BASELINE_ARM64_SHA256=''
BASE_IMAGE="${P0_REAL_INCUS_CONTAINER_CACHE_ALIAS:-subyard-e2e-debian-13-cloud-container}"
FIXTURE_DEV_UID=1001
source_next_version() {
  local core="${1%%[-+]*}" major minor patch
  IFS=. read -r major minor patch <<<"$core"
  printf '%s.%s.%s-p0.next\n' "$major" "$minor" "$((10#$patch + 1))"
}

VERSION_A='' VERSION_B=''
if [ "$MODE" != clean ]; then
  VERSION_A="$(jq -er '.version' "$ROOT/.subyard-acceptance/candidate.json")"
  VERSION_B="$(source_next_version "$VERSION_A")"
fi
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
  YARD_NAME="test-yard"
  PROJECT="subyard-test-yard"
  INSTANCE="yard-test-yard"
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

assert_fixture_project() {
  local project="${1:-$PROJECT}" marker
  marker="$(incus project get "$project" user.p0-source 2>/dev/null)"
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
    p0_capacity_retire_fixture_sink "$OPERATOR_HOME/.subyard" || return
    sudo -n userdel -r "$OPERATOR" >/dev/null
  fi
  if [ -e "$OPERATOR_HOME" ]; then
    sudo -n test -d "$OPERATOR_HOME" && ! sudo -n test -L "$OPERATOR_HOME" \
      && [ "$(sudo -n cat "$OPERATOR_HOME_MARKER" 2>/dev/null)" = "$MARKER" ] \
      || die "refusing unmarked fixture home cleanup: $OPERATOR_HOME"
    p0_capacity_retire_fixture_sink "$OPERATOR_HOME/.subyard" || return
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
  operator_env install -d -m 0700 "$OPERATOR_HOME/.local/bin"
  operator_env bash -c \
    'printf "#!/bin/sh\nprintf invoked > \"%s/go-invoked\"\nexit 127\n" "$1" > "$1/no-go"; chmod 0700 "$1/no-go"' \
    _ "$OPERATOR_HOME"
}

prepare_baseline_yards() {
  local checkout
  install_baseline
  [ "$(operator_yard --version)" = "yard $BASELINE_VERSION" ] || die 'published baseline is not active'
  # v0.11.0 has no yard-add command. Its config sync creates the local named
  # registration from a committed configuration checkout; no runtime state is seeded.
  checkout="$(operator_env mktemp -d /tmp/subyard-p0-owner-config.XXXXXX)"
  # Direct v0.11.0 sync does not register this source. Keep the real checkout
  # for the candidate's later, explicitly confirmed ownership migration.
  CONFIG_CHECKOUT="$checkout"
  operator_env bash -s -- "$checkout" <<'BASELINE_CONFIG'
set -euo pipefail
cd "$1"
install -d -m 0700 hosts/p0-owner/yards/e2e-yard
printf '{"schemaVersion":1}\n' > subyard-config.json
printf 'SSH_PORT=2223\nYARD_TEMPLATE=test-vms\n' > hosts/p0-owner/yards/e2e-yard/config.env
chmod 0600 subyard-config.json hosts/p0-owner/yards/e2e-yard/config.env
git init -q
git add subyard-config.json hosts
git -c user.name=Fixture -c user.email=fixture@example.invalid -c core.hooksPath=/dev/null commit -qm 'Configure fixture test owner'
BASELINE_CONFIG
  operator_env env SUBYARD_HOST_ID=p0-owner "$OPERATOR_HOME/.local/bin/yard" config sync "$checkout" --yes
  operator_env test -f "$OPERATOR_HOME/.config/subyard/yards/e2e-yard/config.env"
  # These are normal published CLI settings, not hand-authored registration state.
  operator_yard config set DEV_SUDO 1 --scope host --yes
  operator_yard config set DEV_UID "$FIXTURE_DEV_UID" --scope host --yes
  operator_yard config set YARD_IMAGE "$BASE_IMAGE" --scope host --yes
  operator_yard config set YARD_IMAGE_FALLBACK "$BASE_IMAGE" --scope host --yes
  operator_yard config set CODING_TOOL_INTEGRATIONS codex --scope host --yes
  operator_yard -Y e2e-yard config set E2E_VM_SLOT_COUNT 1 --scope yard --yes
  operator_yard -Y e2e-yard config set E2E_VM_CPU 1 --scope yard --yes
  operator_yard -Y e2e-yard config set E2E_VM_MEMORY 1GiB --scope yard --yes
  operator_yard -Y e2e-yard config set E2E_VM_DISK 10GiB --scope yard --yes
  # An authored file-setting override survives upgrade and later activation repair.
  operator_env install -d -m 0700 "$OPERATOR_HOME/.config/subyard/overrides/host/agents/codex/rules"
  operator_env bash -c 'printf "# installed owner fixture\n" > "$1"; chmod 0600 "$1"' _ \
    "$OPERATOR_HOME/.config/subyard/overrides/host/agents/codex/rules/repo.rules"
  prepare_project
  operator_yard -Y e2e-yard init --yes
  operator_yard -Y e2e-yard start --yes
  operator_yard -Y e2e-yard check
  prepare_project "$DEFAULT_PROJECT"
  operator_yard init --yes
  operator_yard start --yes
  operator_yard check
  incus exec "$DEFAULT_INSTANCE" --project "$DEFAULT_PROJECT" -- sh -c \
    'printf "ordinary retained data\n" > /srv/source-upgrade-retained'
  ORDINARY_BEFORE="$(ordinary_yard_facts)"
  install -m 0600 /dev/null "$SHARED_ROOT/ordinary-before"
  printf '%s\n' "$ORDINARY_BEFORE" > "$SHARED_ROOT/ordinary-before"
  # Preserve the stopped legacy owner until the explicitly advertised teardown.
  incus stop "$INSTANCE" --project "$PROJECT"
}

package_candidates() {
  local candidate_a_artifact candidate_a_bundle candidate_a_platform fstype
  install -d -m 0700 "$RELEASE_ROOT/a" "$RELEASE_ROOT/b"
  fstype="$(findmnt -n -o FSTYPE --target "$RELEASE_ROOT")" \
    || die "cannot identify release fixture filesystem"
  case "$fstype" in
    tmpfs | ramfs) die "release fixture must survive reboot" ;;
  esac
  printf '%s\n' "$MARKER" > "$RELEASE_ROOT/.subyard-p0-marker"
  cp -a "$ROOT/.subyard-acceptance/release/." "$RELEASE_ROOT/a/"
  candidate_a_artifact="$RELEASE_ROOT/a/yard-$VERSION_A-linux-amd64"
  [ -f "$candidate_a_artifact" ] && [ ! -L "$candidate_a_artifact" ] \
    && [ -x "$candidate_a_artifact" ] \
    || die 'candidate A artifact is not an executable regular file'
  candidate_a_platform="${candidate_a_artifact##*/yard-$VERSION_A-}"
  case "$candidate_a_platform" in
    linux-amd64 | linux-arm64) ;;
    *) die 'candidate A artifact has an unexpected platform suffix' ;;
  esac
  candidate_a_bundle="$RELEASE_ROOT/a/subyard-$VERSION_A-$candidate_a_platform.tar.gz"
  [ -f "$candidate_a_bundle" ] && [ ! -L "$candidate_a_bundle" ] \
    || die 'candidate A runtime bundle is not a regular file'
  install -d -m 0755 "$CANDIDATE_A_REPOSITORY"
  tar -xzf "$candidate_a_bundle" -C "$CANDIDATE_A_REPOSITORY"
  # Public runtime files may retain owner-only modes from the source checkout.
  chmod -R a+rX "$CANDIDATE_A_REPOSITORY"
  [ -f "$CANDIDATE_A_ENGINE" ] && [ ! -L "$CANDIDATE_A_ENGINE" ] \
    && [ -x "$CANDIDATE_A_ENGINE" ] \
    && [ -f "$CANDIDATE_A_REPOSITORY/config/commands.registry" ] \
    && [ ! -L "$CANDIDATE_A_REPOSITORY/config/commands.registry" ] \
    || die 'candidate A runtime root is incomplete'
  (cd "$CANDIDATE_A_REPOSITORY" && sha256sum -c runtime-files.sha256 >/dev/null) \
    || die 'candidate A runtime root failed its file manifest'
  operator_env test -x "$CANDIDATE_A_REPOSITORY" \
    && operator_env test -x "$CANDIDATE_A_ENGINE" \
    && operator_env test -r "$CANDIDATE_A_REPOSITORY/config/commands.registry" \
    || die 'candidate A runtime root is inaccessible to the fixture operator'
  "$ROOT/dev/package-engine.sh" --output-dir "$RELEASE_ROOT/b" --version "$VERSION_B" >/dev/null
  chmod -R a+rX "$RELEASE_ROOT"
}

bootstrap_failure_diagnostics() {
  # The active predecessor may predate migrate --check; inspect with the
  # retained candidate even when bootstrap failed before publishing its launcher.
  local launcher="$CANDIDATE_A_REPOSITORY/bin/yard" report status=0
  if ! operator_env test -x "$launcher" 2>/dev/null; then
    launcher="$OPERATOR_HOME/.subyard/runtime/current/bin/yard"
  fi
  if ! operator_env test -x "$launcher" 2>/dev/null; then
    printf '  [ .. ] bootstrap failure readiness inspection unavailable: no runtime launcher\n' >&2
    return 0
  fi
  report="$(mktemp "$SHARED_ROOT/bootstrap-readiness.XXXXXX")" || return 0
  chmod 0600 "$report"
  # Inspect once without extending the failed operation's authorization or
  # printing raw command output, configuration, or probe stderr.
  operator_no_go timeout --signal=TERM --kill-after=5s 30s \
    "$launcher" migrate --check --json > "$report" 2>/dev/null || status=$?
  printf '  [ .. ] bootstrap failure readiness inspection exit=%s\n' "$status" >&2
  if [ "$status" = 0 ]; then
    jq -ces '
      def member($values): . as $value | type == "string" and ($values | index($value)) != null;
      def optional_string: . == null or (type == "string" and length <= 4096);
      ["ready", "transition-required", "recovery-pending", "registry-invalid",
       "unsupported-epoch", "unsupported-kind", "confirmation-required", "operation-declined", "plan-stale",
       "migration-stale", "resource-conflict", "precondition-blocked", "dependency-unavailable",
       "verification-failed", "activation-ambiguous", "recovery-ambiguous", "journal-invalid",
       "rollback-incompatible", "rollback-expired"] as $codes |
      {"activation.materialized-config":"materialized-config", "activation.host-power":"host-power",
       "activation.test-vm-broker":"test-vm-broker", "activation.test-yard-route-consumers":"route-consumers",
       "activation.orca-runtime":"orca-runtime", "activation.openlit-runtime":"openlit-runtime",
       "source-install.config":"source-config", "source-install.entrypoints":"source-entrypoints",
       "test-yard-owner":"owner-registration", "migration-history.checkpoint":"migration-history",
       "transition.source-metadata":"source-metadata"} as $resources |
      select(length == 1) | .[0] |
      select(type == "object" and .schemaVersion == 1 and
        (.current | type == "string" and length > 0 and length <= 512)) |
      select(.outcome.status | member(["ready", "migration-required", "recovering", "operator-action-required"])) |
      select(.outcome.code | member($codes)) |
      select(.decisions == null or (.decisions | type) == "array") |
      (.decisions // []) as $decisions | select($decisions | length <= 64) |
      select(all($decisions[]; type == "object" and
        (.resource | type == "string" and length > 0 and length <= 256) and
        (.decision | member(["preserve", "transform", "canonicalize", "reset", "quarantine", "block"])) and
        (.scope | optional_string) and (.result | optional_string))) |
      select(.blockers == null or (.blockers | type) == "array") |
      (.blockers // []) as $blockers | select($blockers | length <= 64) |
      select(all($blockers[]; type == "object" and (.code | member($codes)) and
        (.resource | optional_string) and (.message | optional_string) and (.retry | optional_string))) |
      ([$decisions[], $blockers[] | .resource // "" |
        select(. != "" and $resources[.] == null)] | length) as $redacted |
      {status: .outcome.status, code: .outcome.code,
       projection_complete: ($redacted == 0), redacted_resources: $redacted,
       decisions: [$decisions[] | {resource_class: ($resources[.resource] // "redacted"), decision}],
       blockers: [$blockers[] | {code, resource_class:
         (if (.resource // "") == "" then "unspecified" else $resources[.resource] // "redacted" end)}]}' \
      "$report" >&2 2>/dev/null \
      || printf '  [ .. ] bootstrap failure readiness inspection unavailable: invalid public report\n' >&2
  fi
  find "$report" -delete
  return 0
}

bootstrap_candidate() {
  local release="$1" version="$2" status=0
  operator_no_go env \
    YARD_RELEASE_BASE_URL="file://$release" YARD_RELEASE_VERSION="$version" \
    "$release/subyard-install.sh" --yes || status=$?
  if [ "$status" -ne 0 ]; then
    bootstrap_failure_diagnostics || true
    return "$status"
  fi
  operator_env test ! -e "$OPERATOR_HOME/go-invoked" \
    || die 'standalone installer invoked Go'
}

verify_v2_release_transition() { # <version>
  local expected="$1"
  [ "$(operator_yard --version)" = "yard $expected" ] \
    || die "release transition did not activate $expected"
  operator_env jq -e --slurpfile registry \
    "$OPERATOR_HOME/.subyard/runtime/current/config/release-transition.json" '
    .schemaVersion == 1 and
    (.domains | with_entries(.value |= .epoch)) == $registry[0].currentEpochs
  ' "$OPERATOR_HOME/.config/subyard/release-transition/v2/history-checkpoint.json" >/dev/null \
    || die 'release transition checkpoint is not at the verified fixed point'
  jq -e --arg version "$expected" '
    .schemaVersion == 2 and .checkpoint == "complete" and
    (.goal.target | startswith($version + "-"))
  ' < <(operator_env cat \
    "$OPERATOR_HOME/.config/subyard/release-transition/v2/journal.json") >/dev/null \
    || die 'release transition journal is not complete for the expected runtime'
}

verify_config_workflow() {
  local host_hash guest_hash status_output status_rc
  set +e
  status_output="$(operator_yard config status --all-local 2>&1)"
  status_rc=$?
  set -e
  printf '%s\n' "$status_output"
  if [ "$status_rc" -ne 0 ]; then
    [ "$status_rc" -eq 1 ] \
      && grep -Fq 'yard default materialized-config: drift' <<<"$status_output" \
      && grep -Fq 'config status: materialized agent config drift in yards: default' \
        <<<"$status_output" \
      || die 'config status failed for a reason other than expected agent drift'
  fi
  operator_yard config apply --all-local --yes
  operator_yard config status --all-local
  host_hash="$(sudo -n sha256sum \
    "$OPERATOR_HOME/.config/subyard/overrides/host/agents/codex/rules/repo.rules" | awk '{print $1}')"
  guest_hash="$(incus exec "$DEFAULT_INSTANCE" --project "$DEFAULT_PROJECT" --user "$FIXTURE_DEV_UID" --group "$FIXTURE_DEV_UID" -- \
    sha256sum /home/dev/.codex/rules/repo.rules | awk '{print $1}')"
  [ "$host_hash" = "$guest_hash" ] || die 'configured Codex rules were not applied to the yard'
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

wait_for_test_vm_broker() {
  local engine_hash diagnostic='' _
  engine_hash="$(operator_env sha256sum \
    "$OPERATOR_HOME/.subyard/runtime/current/bin/yard-engine" | awk '{print $1}')" || return
  for _ in $(seq 1 60); do
    if diagnostic="$(incus exec "$INSTANCE" --project "$PROJECT" -- \
      env WANT_ENABLED=1 WANT_ENGINE_HASH="$engine_hash" \
      /usr/local/libexec/subyard/test-vms-inner _test-vms-worker doctor 2>&1)"; then
      return 0
    fi
    sleep 1
  done
  printf '%s\n' "$diagnostic" >&2
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

# shellcheck disable=SC2120 # Retain the historical fixture's optional project argument.
prepare_project() {
  local project="${1:-$PROJECT}"
  incus project show "$project" >/dev/null 2>&1 \
    && die "refusing to replace existing project $project"
  incus image info "$BASE_IMAGE" --project default >/dev/null 2>&1 \
    || die "test base image $BASE_IMAGE is unavailable"
  incus project create "$project" \
    -c features.images=false -c user.p0-source="$MARKER" >/dev/null
}

read_default_project_property() { # <output-variable> <property>
  local encoded
  # Compare opaque CLI bytes, including its one framing LF; pipefail retains query errors.
  encoded="$(incus project get "$DEFAULT_PROJECT" "$2" | base64 -w0)" || return
  printf -v "$1" '%s' "$encoded"
}

mark_created_default_project() {
  local uid key expected actual marker empty own yard="${1:-default}"
  uid="$(operator_uid)" || return
  [[ "$uid" =~ ^[0-9]+$ ]] || return 1
  while IFS='=' read -r key expected; do
    expected="$(printf '%s\n' "$expected" | base64 -w0)" || return
    read_default_project_property actual "$key" || return
    [ "$actual" = "$expected" ] || return 1
  done <<OWNER
user.subyard.installation=1
user.subyard.operator_uid=$uid
user.subyard.data_home=$OPERATOR_HOME/.subyard
user.subyard.config_home=$OPERATOR_HOME/.config/subyard
user.subyard.yard_name=$yard
user.subyard.instance_name=$DEFAULT_INSTANCE
OWNER
  empty="$(printf '\n' | base64 -w0)" || return
  own="$(printf '%s\n' "$MARKER" | base64 -w0)" || return
  read_default_project_property marker user.p0-source || return
  [ "$marker" != "$own" ] || return 0
  [ "$marker" = "$empty" ] || return 1
  incus project set "$DEFAULT_PROJECT" user.p0-source="$MARKER" >/dev/null || return
  read_default_project_property marker user.p0-source || return
  [ "$marker" = "$own" ]
}

install_current_test_yard() {
  local inventory init_rc=0 marker_rc=0
  inventory="$(incus project list --format csv -c n)" \
    || die 'cannot inspect fresh fixture project inventory'
  ! grep -Fxq "$DEFAULT_PROJECT" <<<"$inventory" \
    || die 'fresh fixture requires an absent project'
  # Service init creates ownership before any fixture cache fallback is allowed.
  p0_retry_init_after_plan_stale operator_yard service install test-vms --yes || init_rc=$?
  mark_created_default_project "${YARD_NAME:-test-yard}" || marker_rc=$?
  [ "$marker_rc" = 0 ] || die 'fresh fixture project ownership could not be proved'
  [ "$init_rc" = 0 ] || return "$init_rc"
}

ordinary_yard_facts() {
  incus config get "$DEFAULT_INSTANCE" volatile.uuid --project "$DEFAULT_PROJECT"
  incus config get "$DEFAULT_INSTANCE" user.subyard.desired_power --project "$DEFAULT_PROJECT"
  incus list "$DEFAULT_INSTANCE" --project "$DEFAULT_PROJECT" -f csv -c s
  incus exec "$DEFAULT_INSTANCE" --project "$DEFAULT_PROJECT" -- sha256sum /srv/source-upgrade-retained
}

recreate_legacy_test_owner() {
  local before after report init_rc=0
  before="$(operator_env tar -C "$OPERATOR_HOME/.config/subyard" -cf - . | sha256sum)"
  before+="$(incus config show "$INSTANCE" --project "$PROJECT" --expanded)"
  # Init has no check mode. Inspect the active release's public warning before
  # executing its exact recreation commands, without mutating either yard.
  report="$(operator_yard migrate --check --json)"
  jq -e '
    .schemaVersion == 1 and .outcome.status == "ready" and .outcome.reachedGoal == true and
    any(.outcome.warnings[]?; contains("test-yard requires recreation") and
      contains("yard config sync <checkout>, then yard -Y e2e-yard teardown --reset-config, then yard service install test-vms"))
  ' <<<"$report" >/dev/null || {
    printf '%s\n' "$report" >&2
    die 'legacy owner readiness did not name executable recreation commands'
  }
  after="$(operator_env tar -C "$OPERATOR_HOME/.config/subyard" -cf - . | sha256sum)"
  after+="$(incus config show "$INSTANCE" --project "$PROJECT" --expanded)"
  [ "$before" = "$after" ] || die 'owner inspection mutated protected configuration or native state'
  [ "$(ordinary_yard_facts)" = "$ORDINARY_BEFORE" ] || die 'owner inspection changed the ordinary yard'

  # Supply the original checkout for the advertised argument; sync's --yes is
  # fresh consent for its own guarded plan. No --apply or guest refresh is needed.
  operator_yard config sync "$CONFIG_CHECKOUT" --yes
  [ "$(ordinary_yard_facts)" = "$ORDINARY_BEFORE" ] || die 'settings migration changed the ordinary yard'
  report="$(operator_yard migrate --check --json)"
  jq -e '
    .outcome.status == "ready" and .outcome.reachedGoal == true and
    any(.outcome.warnings[]?; contains("test-yard requires recreation") and
      contains("yard -Y e2e-yard teardown --reset-config, then yard service install test-vms") and
      (contains("config sync") | not))
  ' <<<"$report" >/dev/null || die 'settings migration did not clear its recreation prerequisite'
  # Direct sync needs no registered checkout for later offline cache use.
  operator_env find "$CONFIG_CHECKOUT" -depth -delete
  CONFIG_CHECKOUT=''

  # Execute the advertised commands. Fresh fixture configuration selects its cached
  # image; it does not carry the legacy broker's identities, data or registration.
  operator_yard -Y e2e-yard teardown --reset-config --yes
  [ "$(ordinary_yard_facts)" = "$ORDINARY_BEFORE" ] || die 'legacy teardown changed the ordinary yard'
  select_current_test_yard
  # Modern init owns project creation. Its private image namespace may not have
  # the fixture alias yet; share the cache only after proving this exact ownership.
  DEFAULT_PROJECT="$PROJECT" DEFAULT_INSTANCE="$INSTANCE" install_current_test_yard || init_rc=$?
  if [ "$init_rc" != 0 ]; then
    [ -z "$(incus list --project "$PROJECT" --format csv -c n)" ] \
      || die 'fresh service install failed after image lookup'
    incus project set "$PROJECT" features.images=false
    incus image info "$BASE_IMAGE" --project "$PROJECT" >/dev/null \
      || die 'fresh test-yard cannot access the shared fixture image'
    p0_retry_init_after_plan_stale operator_yard service install test-vms --yes
  fi
  [ "$(ordinary_yard_facts)" = "$ORDINARY_BEFORE" ] || die 'fresh test-yard installation changed the ordinary yard'
  operator_yard test-vms status --json
  operator_yard -Y test-yard check
  operator_yard migrate --check --json | jq -e '
    .schemaVersion == 1 and .outcome.status == "ready" and
    all(.outcome.warnings[]?; contains("test-yard requires recreation") | not)' >/dev/null \
    || die 'fresh test-yard is not ready'
}

run_incus_installer() {
  p0_capacity_prepare_platform_root
  (
    local bootstrap_root="$P0_CAPACITY_STATE_ROOT/incus-bootstrap" bootstrap_rc=0
    p0_capacity_prepare_subtree "$bootstrap_root"
    trap 'bootstrap_rc=$?; p0_capacity_remove_subtree "$bootstrap_root" || bootstrap_rc=3; exit "$bootstrap_rc"' EXIT
    # shellcheck source=tests/helpers/test-context.sh
    . "$ROOT/tests/helpers/test-context.sh"
    setup_test_context "$bootstrap_root"
    export SUBYARD_USER
    SUBYARD_USER="$(id -un)"
    export SUBYARD_OPERATOR_HOME="$HOME"
    export SUBYARD_CONFIG_DIR="$ROOT/config"
    # Preserve the retained pool path without writing config/data into its root-owned parents.
    export STORAGE_PATH="$P0_CAPACITY_PLATFORM_ROOT/incus/incus/storage"
    set -a
    # shellcheck source=config/host.env
    . "$ROOT/config/host.env"
    set +a
    bash "$ROOT/scripts/01-install-incus.sh" "$@"
  )
}

prepare() {
  [ "$(uname -m)" = x86_64 ] || die 'the published v0.11.0 pin requires an amd64 P0 worker'
  command -v git >/dev/null 2>&1 || die 'Git is required for the published configuration import'
  command -v unshare >/dev/null 2>&1 || die 'unshare is required'
  p0_capacity_reset_build_cache
  if ! incus info >/dev/null 2>&1 \
    || ! incus storage show default --project default >/dev/null 2>&1 \
    || ! incus network show incusbr0 --project default >/dev/null 2>&1; then
    run_incus_installer --yes --zabbly
  fi
  if ! incus image info "$BASE_IMAGE" --project default >/dev/null 2>&1; then
    bash "$ROOT/dev/e2e/p0-real-incus.sh" --prepare-only
  fi
  cleanup_fixture
  p0_capacity_reset_build_cache
  p0_capacity_prepare_subtree "$SOURCE_STATE_ROOT"
  chmod 0711 "$SOURCE_STATE_ROOT"
  install -d -m 0711 "$SHARED_ROOT"
  printf '%s\n' "$MARKER" > "$SHARED_ROOT/.subyard-p0-marker"
  prepare_operator
  package_candidates
  prepare_baseline_yards
  local update_rc=0 legacy_before
  legacy_before="$(incus config get "$INSTANCE" volatile.uuid --project "$PROJECT")"
  if bootstrap_candidate "$RELEASE_ROOT/a" "$VERSION_A" > "$SHARED_ROOT/first-update.log" 2>&1; then
    cat "$SHARED_ROOT/first-update.log"
  else
    update_rc=$?
    cat "$SHARED_ROOT/first-update.log" >&2
    return "$update_rc"
  fi
  grep -Fq 'yard config sync <checkout>, then yard -Y e2e-yard teardown --reset-config, then yard service install test-vms' \
    "$SHARED_ROOT/first-update.log" || die 'update did not print its exact recreation commands'
  [ "$(ordinary_yard_facts)" = "$ORDINARY_BEFORE" ] || die 'update changed ordinary yard data, identity or power'
  [ "$(incus config get "$INSTANCE" volatile.uuid --project "$PROJECT")" = "$legacy_before" ] \
    && [ "$(incus list "$INSTANCE" --project "$PROJECT" -f csv -c s)" = STOPPED ] \
    || die 'update changed the retained legacy owner'
  recreate_legacy_test_owner
  incus project set "$PROJECT" user.p0-source="$MARKER"
  [ "$(operator_yard --version)" = "yard $VERSION_A" ] \
    || die 'first candidate runtime is not active'
  verify_v2_release_transition "$VERSION_A"
  bootstrap_candidate "$RELEASE_ROOT/a" "$VERSION_A"
  [ "$(operator_env grep -Fc '# Subyard CLI completion' "$OPERATOR_HOME/.bashrc")" = 1 ] \
    || die 'repeated bootstrap duplicated shell integration'
  [ "$(incus list "$INSTANCE" --project "$PROJECT" -f csv -c s)" = RUNNING ] \
    || die 'runtime upgrade did not recreate test-yard as running'
  [ "$(incus config get "$INSTANCE" user.subyard.desired_power --project "$PROJECT")" = running ] \
    || die 'test-yard migration did not establish desired=running'
  operator_yard -Y "$YARD_NAME" check
  p0_retry_init_after_plan_stale operator_yard -Y "$YARD_NAME" init --yes
  [ "$(ordinary_yard_facts)" = "$ORDINARY_BEFORE" ] || die 'repeated init changed the ordinary yard'
  verify_config_workflow
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
  printf 'ok: published v%s owner recreation preserved the ordinary yard; prepared for reboot\n' "$BASELINE_VERSION"
}

load_rebooted_fixture() {
  select_current_test_yard
  ORDINARY_BEFORE="$(cat "$SHARED_ROOT/ordinary-before")" || die 'ordinary yard baseline disappeared after reboot'
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
  operator_yard -Y "$YARD_NAME" stop --yes
  operator_yard start --yes
  # Start waits for the agent, but profile services can become ready between
  # planning and convergence. Every plan-stale retry prepares a fresh plan.
  local attempt report migrate_rc
  for attempt in $(seq 1 60); do
    if report="$(operator_yard migrate --yes 2>&1)"; then
      printf '%s\n' "$report"
      break
    else
      migrate_rc=$?
    fi
    printf '%s\n' "$report" >&2
    if ! grep -Eq '^yard: migrate: .*release transition [^:]+: code=plan-stale([[:space:];]|$)' \
        <<<"${report##*$'\n'}"; then
      return "$migrate_rc"
    fi
    printf 'source-upgrade: migrate after start plan-stale attempt=%s/60 native_exit=%s\n' \
      "$attempt" "$migrate_rc" >&2
    if [ "$attempt" = 60 ]; then return "$migrate_rc"; fi
    sleep 1
  done
  verify_config_workflow
  operator_env chmod 0600 \
    "$OPERATOR_HOME/.config/subyard/yards/test-yard/config.env"
  [ "$(incus config get "$INSTANCE" user.subyard.desired_power --project "$PROJECT")" = stopped ] \
    && [ "$(incus config get "$DEFAULT_INSTANCE" user.subyard.desired_power \
      --project "$DEFAULT_PROJECT")" = running ] \
    && [ "$(incus config get "$INSTANCE" boot.autostart --project "$PROJECT")" = false ] \
    && [ "$(incus config get "$DEFAULT_INSTANCE" boot.autostart \
      --project "$DEFAULT_PROJECT")" = false ] \
    || die 'named stopped and default running desired power is not persisted before committed-state reboot'
  operator_env test ! -e "$OPERATOR_HOME/go-invoked" \
    || die 'installed update invoked Go'
  install_power_retry_probe
  require_operator_password_sudo
  printf 'ok: second candidate update is committed for reboot\n'
}

current_migration_ready() {
  # The public ready outcome already excludes pending migrations and blockers.
  # Informational decisions describe observations, not remaining work.
  jq -es --arg current "$1" '
    length == 1 and (.[0] |
      .schemaVersion == 1 and .current == $current and .outcome.status == "ready")
  ' >/dev/null 2>&1
}

current_migration_readiness_diagnostics() {
  # Project closed semantic fields; omit release IDs, bindings and messages.
  jq -ces --arg current "$1" '
    def known($values): . as $value | if ($values | index($value)) != null then . else "unknown" end;
    select(length == 1) | .[0] |
    {schema_valid: (.schemaVersion == 1), current_matches: (.current == $current),
     target_matches: (.outcome.target == $current),
     status: (.outcome.status | known(["ready", "migration-required", "recovering", "operator-action-required"])),
     reached_goal: (.outcome.reachedGoal | known([true, false]))}
  ' >&2 2>/dev/null || printf 'current migration readiness report is invalid\n' >&2
}

finish() {
  local transaction_before target_before previous_before report releases_before host_hash guest_hash
  local ready=0 attempt
	local transition_root="$OPERATOR_HOME/.config/subyard/release-transition/v2"
  local ledger_before="$SHARED_ROOT/post-reboot-ledger.before"
  local journal_before="$SHARED_ROOT/post-reboot-journal.before"
  local journal_ready="$SHARED_ROOT/post-reboot-journal.ready"
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
  # Yard start waits for its agent; inner Incus may still be starting. Establish
  # broker readiness before binding a plan to the intentional config drift.
  wait_for_test_vm_broker || die 'test VM broker did not become ready after yard start'
  # Starting the yard does not wait for every broker service. Establish the
  # release fixed point before injecting drift into the default workspace.
  for attempt in $(seq 1 60); do
    report="$(operator_yard migrate --check --json)"
    if jq -e '.schemaVersion == 1 and .current == .outcome.target and
        .outcome.status == "ready" and .outcome.reachedGoal == true' \
        <<<"$report" >/dev/null; then
      ready=1
      break
    fi
    if ! jq -e '.schemaVersion == 1 and .current == .outcome.target and
        .outcome.status == "migration-required" and .outcome.code == "transition-required"' \
        <<<"$report" >/dev/null; then
      printf '%s\n' "$report" >&2
      die 'unexpected current-release readiness failure after yard start'
    fi
    sleep 1
  done
  if [ "$ready" != 1 ]; then
    printf '%s\n' "$report" >&2
    die 'current release did not reach readiness after yard start'
  fi
  # Remove an owned artifact to exercise repair without replaying completed migrations.
  # Edited artifacts must instead fail closed to preserve user changes.
  incus exec "$DEFAULT_INSTANCE" --project "$DEFAULT_PROJECT" --user "$FIXTURE_DEV_UID" --group "$FIXTURE_DEV_UID" -- \
    rm /home/dev/.codex/rules/repo.rules
  verify_v2_release_transition "$VERSION_B"
  operator_env cat "$transition_root/ledger.json" > "$ledger_before"
  operator_env cat "$transition_root/journal.json" > "$journal_before"
  transaction_before="$(operator_env jq -er '.transaction' "$transition_root/journal.json")"
  target_before="$(operator_env jq -er '.goal.target' "$transition_root/journal.json")"
  previous_before="$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/previous")"
  releases_before="$(operator_env ls -1 "$OPERATOR_HOME/.subyard/runtime/releases")"
  report="$(operator_no_go env YARD_RELEASE_BASE_URL="file://$SHARED_ROOT/forbidden-release" \
    YARD_RELEASE_CACHE="$SHARED_ROOT/forbidden-cache" YARD_RELEASE_VERSION=invalid/selected/version \
    "$OPERATOR_HOME/.local/bin/yard" migrate --check --json)"
  jq -e --arg current "$target_before" '
    .schemaVersion == 1 and .current == $current and .owner == $current and
    .outcome.status == "migration-required" and .next == "yard migrate" and
    all(.domains[]; .pending == []) and
    any(.decisions[]; .scope == "activation" and .resource == "activation.materialized-config")
  ' <<<"$report" >/dev/null || {
    printf '%s\n' "$report" >&2
    die 'current migration check did not identify runtime drift'
  }
  operator_env cat "$transition_root/journal.json" | cmp "$journal_before" - \
    || die 'current migration check changed the journal'
  incus exec "$DEFAULT_INSTANCE" --project "$DEFAULT_PROJECT" --user "$FIXTURE_DEV_UID" --group "$FIXTURE_DEV_UID" -- \
    test ! -e /home/dev/.codex/rules/repo.rules \
    || die 'current migration check repaired materialized configuration'
  operator_no_go env YARD_RELEASE_BASE_URL="file://$SHARED_ROOT/forbidden-release" \
    YARD_RELEASE_CACHE="$SHARED_ROOT/forbidden-cache" YARD_RELEASE_VERSION=invalid/selected/version \
    "$OPERATOR_HOME/.local/bin/yard" migrate --yes
  verify_v2_release_transition "$VERSION_B"
  operator_env cat "$transition_root/ledger.json" | cmp "$ledger_before" - \
    || die 'post-reboot activation repair changed the one-time migration ledger'
  operator_env jq -e --arg transaction "$transaction_before" --arg target "$target_before" '
    .goal == {target:$target,direction:"activate-target"} and .releases.target == $target and
    .transaction != $transaction and .steps == [] and .releases.from == $target
  ' "$transition_root/journal.json" >/dev/null \
    || die 'post-reboot repair replayed migrations or changed the release target'
  [ "$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/current")" = "releases/$target_before" ] \
    && [ "$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/previous")" = "$previous_before" ] \
    || die 'post-reboot activation repair changed the runtime pair'
  host_hash="$(sudo -n sha256sum \
    "$OPERATOR_HOME/.config/subyard/overrides/host/agents/codex/rules/repo.rules" | awk '{print $1}')"
  guest_hash="$(incus exec "$DEFAULT_INSTANCE" --project "$DEFAULT_PROJECT" --user "$FIXTURE_DEV_UID" --group "$FIXTURE_DEV_UID" -- \
    sha256sum /home/dev/.codex/rules/repo.rules | awk '{print $1}')"
  [ "$host_hash" = "$guest_hash" ] || die 'current migration did not repair the materialized rules'
  report="$(operator_yard migrate --check --json)"
  current_migration_ready "$target_before" <<<"$report" || {
    current_migration_readiness_diagnostics "$target_before" <<<"$report"
    die 'current migration did not reach readiness'
  }
  operator_env cat "$transition_root/journal.json" > "$journal_ready"
  operator_no_go env -u ASSUME_YES YARD_RELEASE_BASE_URL="file://$SHARED_ROOT/forbidden-release" \
    YARD_RELEASE_CACHE="$SHARED_ROOT/forbidden-cache" YARD_RELEASE_VERSION=invalid/selected/version \
    "$OPERATOR_HOME/.local/bin/yard" migrate </dev/null
  operator_env cat "$transition_root/journal.json" | cmp "$journal_ready" - \
    || die 'ready migration rewrote its journal'
  operator_env cat "$transition_root/ledger.json" | cmp "$ledger_before" - \
    || die 'ready migration rewrote its ledger'
  [ "$(operator_env ls -1 "$OPERATOR_HOME/.subyard/runtime/releases")" = "$releases_before" ] \
    && [ "$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/current")" = "releases/$target_before" ] \
    && [ "$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/previous")" = "$previous_before" ] \
    && [ ! -e "$SHARED_ROOT/forbidden-cache" ] \
    || die 'current migration selected or published a release'
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

  [ "$(ordinary_yard_facts)" = "$ORDINARY_BEFORE" ] || die 'updates, rollback or reboots changed the ordinary yard'
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
  printf 'ok: published owner upgrade survived two reboots, rollback and activation repair\n'
}

case "$MODE" in
  prepare) prepare ;;
  resume) resume ;;
  finish) finish ;;
  clean) cleanup_fixture ;;
  *) die 'expected prepare, resume, finish or clean' ;;
esac
