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
TEST_VMS_SINK=
TEST_VMS_SINK_SERVICE_NAME=
TEST_VMS_SINK_SERVICE=
TEST_VMS_SINK_TIMER_NAME=
TEST_VMS_SINK_TIMER=
CANDIDATE_BUNDLE=
CANDIDATE_RELEASE=

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
  POWER_RECONCILER="/usr/local/libexec/subyard/subyard-p0-v0111-$FIXTURE-$TOKEN"
  POWER_UNIT_NAME="subyard-p0-v0111-$FIXTURE-$TOKEN.service"
  POWER_UNIT="/etc/systemd/system/subyard-p0-v0111-$FIXTURE-$TOKEN.service"
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
  local unsafe=0 touched=0
  if [ -e "$POWER_UNIT" ] || [ -L "$POWER_UNIT" ]; then
    [ -f "$POWER_UNIT" ] && [ ! -L "$POWER_UNIT" ] \
      && [ "$(stat -c '%u:%g' "$POWER_UNIT" 2>/dev/null)" = 0:0 ] \
      && grep -Fqx "ExecStart=$POWER_RECONCILER _power-reconcile" "$POWER_UNIT" \
      || unsafe=1
  fi
  if [ -e "$POWER_RECONCILER" ] || [ -L "$POWER_RECONCILER" ]; then
    [ -f "$POWER_RECONCILER" ] && [ ! -L "$POWER_RECONCILER" ] \
      && [ "$(stat -c '%u:%g' "$POWER_RECONCILER" 2>/dev/null)" = 0:0 ] \
      || unsafe=1
  fi
  if [ "$unsafe" -ne 0 ]; then
    printf 'release-transition-v0111-recovery: refusing unsafe power artifact for %s\n' \
      "$FIXTURE" >&2
    return 1
  fi
  if [ -e "$POWER_UNIT" ]; then
    sudo -n systemctl disable --now "$POWER_UNIT_NAME" >/dev/null 2>&1 || return 1
    sudo -n find "$POWER_UNIT" -maxdepth 0 -type f -delete || return 1
    touched=1
  fi
  if [ -e "$POWER_RECONCILER" ]; then
    sudo -n find "$POWER_RECONCILER" -maxdepth 0 -type f -delete || return 1
    touched=1
  fi
  if [ "$touched" -eq 1 ]; then
    sudo -n systemctl daemon-reload >/dev/null 2>&1 || return 1
  fi
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
  local fixture="$1" failed=0 armed=0
  select_fixture "$fixture"
  case "$fixture" in
    success) armed="$SUCCESS_ARMED" ;;
    post-cas) armed="$POST_CAS_ARMED" ;;
  esac
  [ "$armed" = 1 ] || return 0
  if incus project show "$PROJECT" >/dev/null 2>&1; then
    if ! project_is_marked; then
      printf 'release-transition-v0111-recovery: refusing unmarked project %s\n' \
        "$PROJECT" >&2
      failed=1
    elif [ -x "$BIN_ROOT/yard" ]; then
      yard -Y "$YARD_NAME" teardown --yes >/dev/null 2>&1 || true
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
        incus delete "$name" --project "$PROJECT" --force >/dev/null || failed=1
      done < <(incus list --project "$PROJECT" --format csv -c n)
      [ "$failed" -ne 0 ] || incus project delete "$PROJECT" >/dev/null 2>&1 || failed=1
    fi
  fi
  cleanup_test_vms_sink || failed=1
  cleanup_power_runtime || failed=1
  if [ -e "$FIXTURE_ROOT" ]; then
    if ! fixture_root_is_marked; then
      printf 'release-transition-v0111-recovery: refusing unmarked fixture root %s\n' \
        "$FIXTURE_ROOT" >&2
      failed=1
    elif [ "$failed" -eq 0 ]; then
      sudo -n find "$FIXTURE_ROOT" -depth -delete || failed=1
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
    exit 3
  fi
  [ "$CLEANUP_ARMED" = 1 ] || exit "$rc"
  cleanup_fixture post-cas || cleanup_failed=1
  cleanup_fixture success || cleanup_failed=1
  if [ -e "$STATE_ROOT" ]; then
    assert_state_root || cleanup_failed=1
    [ "$cleanup_failed" -ne 0 ] || find "$STATE_ROOT" -depth -delete || cleanup_failed=1
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
  printf '%s\n' \
    'YARD_TEMPLATE=test-vms' \
    "SSH_PORT=$ssh_port" \
    'AGENTS=' \
    'DEV_UID=1001' \
    'E2E_VM_CPU=1' \
    'E2E_VM_MEMORY=700MiB' \
    'E2E_VM_DISK=10GiB' \
    'E2E_VM_SLOT_COUNT=1' \
    "BASE_IMAGE=$base_image" \
    "BASE_IMAGE_FALLBACK=$base_image" \
    > "$CONFIG_HOME/yards/$YARD_NAME/config.env"
  printf '# %s\nThe v0.11.1 %s fixture owns this file.\n' "$FIXTURE_MARKER" "$FIXTURE" \
    > "$HOME_ROOT/host/AGENTS.md"
  chmod 0600 "$CONFIG_HOME/config.env" \
    "$CONFIG_HOME/yards/$YARD_NAME/config.env" "$HOME_ROOT/host/AGENTS.md"
}

prepare_fixture() { # success|post-cas
  select_fixture "$1"
  [ ! -e "$FIXTURE_ROOT" ] || die "fixture state already exists: $FIXTURE_ROOT"
  install -d -m 0700 "$FIXTURE_ROOT"
  printf '%s\n' "$FIXTURE_MARKER" > "$FIXTURE_ROOT/.subyard-p0-marker"
  case "$1" in
    success) SUCCESS_ARMED=1 ;;
    post-cas) POST_CAS_ARMED=1 ;;
  esac
  assert_fixture_root
  write_fixture_config
}

install_official() { # installer version; nonzero allowed by caller
  local installer="$1" version="$2"
  fixture_env env YARD_RELEASE_VERSION="$version" "$installer" --version "$version" --yes
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
  fixture_env "$RUNTIME_ROOT/current/bin/yard" update --check --offline \
    --version "$SOURCE_VERSION" --runtime-root "$RUNTIME_ROOT" > "$output"
  jq -s -e 'map(select(type == "object") | (.inspection // .)) |
    map(select(has("blockers"))) as $reports |
    ($reports | length) == 1 and ($reports[0].blockers | length) == 1 and
    $reports[0].blockers[0].resource == "transition.observation-scope"' \
    "$output" >/dev/null \
    || die 'official v0.11.1 scope blocker is not repeatable'
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
  [ "$source_rc" -ne 0 ] || die 'official v0.11.1 unexpectedly escaped the known dead-end'
  assert_source_dead_end
  cp "$LEDGER" "$FIXTURE_ROOT/source-ledger.json"
}

capture_recovery_plan() {
  local candidate_path output before after plan
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
    --version "$CANDIDATE_VERSION" --runtime-root "$RUNTIME_ROOT" > "$output"
  after="$(sha256sum "$JOURNAL" | awk '{print $1}')"
  [ "$before" = "$after" ] || die 'candidate recovery inspection changed the source journal'
  plan="$(jq -sr 'map(select(type == "object") | (.inspection // .)) |
    map(select(has("plan") and .assessment.changed == true and
      ((.blockers // []) | length) == 0)) |
    select(length == 1) | .[0].plan' "$output")"
  case "$plan" in plan-v1-*) ;; *) die 'candidate recovery inspection returned no exact Plan' ;; esac
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
    || die 'repeat update check did not report the structured ready fixed point'
}

assert_materialized_fixed_point() {
  local expected actual
  expected="$(sha256sum "$HOME_ROOT/host/AGENTS.md" | awk '{print $1}')"
  actual="$(incus exec "$INSTANCE" --project "$PROJECT" -- \
    sha256sum /home/dev/.codex/AGENTS.md | awk '{print $1}')"
  [ "$actual" = "$expected" ] \
    || die 'candidate recovery did not materialize the configured Codex target'
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
  fixture_env "$RUNTIME_ROOT/current/bin/yard" update --check --offline \
    --version "$CANDIDATE_VERSION" --runtime-root "$RUNTIME_ROOT" > "$output"
  active="$(readlink "$RUNTIME_ROOT/current")"
  assert_ready_check "$output" "${active#releases/}"
  assert_materialized_fixed_point
  yard -Y "$YARD_NAME" status >/dev/null
  ok 'local patch recovered the exact published v0.11.1 dead-end'
}

run_post_cas_resume() { # post-cas
  local transition_pid transition_rc=0 output active source_transaction marker_metadata
  select_fixture "$1"
  capture_recovery_plan
  source_transaction="$(jq -er '.transaction' "$JOURNAL")"
  POST_CAS_OBSERVATION_MARKER="$FIXTURE_ROOT/post-cas-link-observed.json"
  [ -f "$POST_CAS_OBSERVER" ] && [ ! -L "$POST_CAS_OBSERVER" ] \
    || die 'post-CAS link observer is unavailable'
  sh -c 'test ! -e "$1" && test ! -L "$1"' _ "$POST_CAS_OBSERVATION_MARKER" \
    || die 'refusing an existing post-CAS observation marker'
  output="$FIXTURE_ROOT/first-process.log"
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
      "$RELEASE_ROOT/subyard-install.sh" --yes > "$output" 2>&1 &
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
  fixture_env "$RUNTIME_ROOT/current/bin/yard" update --offline \
    --version "$CANDIDATE_VERSION" --runtime-root "$RUNTIME_ROOT" </dev/null > "$output"
  ! grep -Eiq 'confirm|proceed' "$output" \
    || die 'ordinary post-CAS resume requested a second confirmation'
  assert_terminal "$FIXTURE_ROOT/source-ledger.json" "$FIXTURE_ROOT/recovery-plan"
  output="$FIXTURE_ROOT/repeat-check.json"
  fixture_env "$RUNTIME_ROOT/current/bin/yard" update --check --offline \
    --version "$CANDIDATE_VERSION" --runtime-root "$RUNTIME_ROOT" > "$output"
  active="$(readlink "$RUNTIME_ROOT/current")"
  assert_ready_check "$output" "${active#releases/}"
  assert_materialized_fixed_point
  yard -Y "$YARD_NAME" status >/dev/null
  ok 'ordinary active-candidate update resumed after the exact post-CAS interruption'
}

main() {
  trap cleanup EXIT
  trap 'handle_signal 130' INT
  trap 'handle_signal 143' TERM
  valid_token "$TOKEN" || die 'numeric P0 allocation token is required'
  [ -n "${SUBYARD_E2E_VM:-}" ] || die 'run only inside an allocated P0 guest'
  [ "$SUBYARD_E2E_VM" = 1 ] || die 'v0.11.1 release recovery is pinned to the P0 owner VM'
  for dependency in curl go incus jq ps setsid sha256sum sudo systemctl; do
    command -v "$dependency" >/dev/null 2>&1 || die "$dependency is required"
  done
  [ ! -e "$STATE_ROOT" ] || die "fixture state already exists: $STATE_ROOT"
  install -d -m 0700 "$STATE_ROOT"
  printf '%s\n' "$MARKER" > "$STATE_ROOT/.subyard-p0-marker"
  CLEANUP_ARMED=1
  download_pinned_installer "$OLD_VERSION" "$OLD_INSTALLER_SHA256" "$OLD_INSTALLER"
  download_pinned_installer "$SOURCE_VERSION" "$SOURCE_INSTALLER_SHA256" "$SOURCE_INSTALLER"
  CANDIDATE_BUNDLE="$RELEASE_ROOT/subyard-$CANDIDATE_VERSION-linux-amd64.tar.gz"
  "$ROOT/dev/package-engine.sh" --output-dir "$RELEASE_ROOT" \
    --version "$CANDIDATE_VERSION" >/dev/null
  chmod -R a+rX "$RELEASE_ROOT"

  reproduce_official_dead_end success
  run_candidate_recovery success
  reproduce_official_dead_end post-cas
  run_post_cas_resume post-cas
  printf 'ok: two official v0.9.1 -> v0.11.1 histories and v0.11.2 recovery are covered\n'
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  main "$@"
fi
