#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
MODE="${1:-}"
TOKEN="${2:-}"
PEER_IP="${3:-}"
PEER_PUBLIC_KEY="${4:-}"
PEER_HOST_KEY="${5:-}"
# shellcheck source=dev/e2e/lib-p0-capacity.sh
. "$ROOT/dev/e2e/lib-p0-capacity.sh"
# shellcheck source=dev/e2e/lib-p0-init-retry.sh
. "$ROOT/dev/e2e/lib-p0-init-retry.sh"
p0_capacity_init "$TOKEN"

PEER_ROOT="/tmp/subyard-p0-peer-$TOKEN"
PEER_STATE_ROOT="$P0_CAPACITY_STATE_ROOT/peer"
PEER_DATA_ROOT="$PEER_STATE_ROOT/subyard"
PEER_HOST_BASE="$PEER_STATE_ROOT/host-data"
PEER_KEYS_ROOT="$PEER_STATE_ROOT/keys"
PEER_KEYS_TOOLS="$PEER_STATE_ROOT/tools"
PEER_KEYS_CONSUMER_ROOT="$PEER_STATE_ROOT/consumer"
MARKER="subyard-p0-$TOKEN"
PEER_INCUS_MARKER="$PEER_STATE_ROOT/.subyard-p0-incus-init"
PEER_INCUS_POOL="subyard-p0-$TOKEN"
OWNER_ROOT="$P0_CAPACITY_STATE_ROOT/owner"
OWNER_DATA_ROOT="$OWNER_ROOT/subyard"
OWNER_CONFIG_HOME="$OWNER_ROOT/config"
OWNER_YARD_DIR="$OWNER_CONFIG_HOME/yards"
RENAME_BASE_REVISION="7c67ee3f423cf9f1596c2f5191f462d2b70adcdc"
RENAME_BASE_ROOT="$OWNER_ROOT/rename-base"
PEER_SSH_DIR="$PEER_ROOT/ssh"
PEER_YARD_ENTRY="$HOME/.local/bin/yard"
PEER_YARD_BACKUP="$PEER_ROOT/user-yard-entry.backup"
PEER_YARD_STATE="$PEER_ROOT/.user-yard-entry-state"
PEER_PROFILE_BACKUP="$PEER_ROOT/user-profile.backup"
PEER_PROFILE_STATE="$PEER_ROOT/.user-profile-state"
PEER_AUTH_STATE="$PEER_ROOT/.authorized-keys-state"
PEER_CONFIG_STATE="$PEER_ROOT/.ssh-config-state"
PEER_REAL_YARD_MARKER="$PEER_ROOT/.subyard-p0-real-yard"
OWNER_BASELINE_IMAGES=''
OWNER_BASELINE_CAPTURED=0
OWNER_BASE_IMAGE="${P0_REAL_INCUS_CONTAINER_CACHE_ALIAS:-subyard-e2e-debian-13-cloud-container}"
OWNER_BASE_IMAGE_CREATED=0
OWNER_DIAGNOSTIC_VM_MEMORY="${P0_E2E_DIAGNOSTIC_VM_MEMORY:-700MiB}"
OWNER_DIAGNOSTIC_VM_BOOT_TIMEOUT="${P0_E2E_DIAGNOSTIC_VM_BOOT_TIMEOUT:-600}"
OWNER_DIAGNOSTIC_DEV_UID="${P0_E2E_DIAGNOSTIC_DEV_UID:-1001}"

die() { printf 'p0-guest: %s\n' "$*" >&2; exit 2; }
valid_token() { [[ "$1" =~ ^[0-9]+$ ]]; }
valid_ip() { [[ "$1" =~ ^[0-9a-fA-F:.]+$ ]]; }
normalized_ed25519() {
  local value="$1" type blob rest
  read -r type blob rest <<<"$value"
  [ "$type" = ssh-ed25519 ] && [[ "$blob" =~ ^[A-Za-z0-9+/=]+$ ]] \
    || return 1
  printf '%s %s\n' "$type" "$blob"
}
valid_token "$TOKEN" || die 'allocation token must be numeric'
[ -n "${SUBYARD_E2E_VM:-}" ] || die 'run through dev/agent-e2e.sh'

clean_tree() { # guarded path marker
  local path="$1" marker="$2"
  case "$path" in /tmp/subyard-p0-*) ;; *) die "unsafe cleanup path $path" ;; esac
  [ ! -e "$path" ] || [ "$(cat "$path/.subyard-p0-marker" 2>/dev/null)" = "$marker" ] \
    || die "refusing to clean unmarked path $path"
  [ ! -e "$path" ] || sudo -n find "$path" -depth -delete
}

clean_peer_data() {
  p0_capacity_remove_subtree "$PEER_STATE_ROOT"
  p0_capacity_remove_build_cache
  p0_capacity_remove_root_if_empty
}

owner_project_contract() {
  local root="/tmp/subyard-p0-project-$TOKEN"
  local source="$root/one/P0Project"
  local bound="$root/two/P0Project"
  local rejected="$root/three/P0Project"
  local git_url='file:///tmp/P0Project.git'
  local completions patch projects reservation replay retried
  clean_tree "$root" "$MARKER"
  install -d -m 0700 "$source" "$bound" "$rejected"
  printf '%s\n' "$MARKER" > "$root/.subyard-p0-marker"
  printf '%s\nbase\n' "$MARKER" > "$source/result.txt"
  printf 'bound\n' > "$bound/result.txt"
  printf 'rejected\n' > "$rejected/result.txt"
  # The disposable outer VM has dev=1000, while its Debian cloud image reserves
  # 1000 and the nested diagnostic yard therefore uses dev=1001. A real bind
  # source must belong to the configured yard UID for shift=true to preserve
  # private permissions; stage this test-owned tree the same way.
  sudo -n chown -R "$OWNER_DIAGNOSTIC_DEV_UID:$OWNER_DIAGNOSTIC_DEV_UID" "$bound"
  incus exec yard-test-yard --project subyard-test-yard -- \
    runuser -u dev -- git init --bare /tmp/P0Project.git >/dev/null
  ./bin/yard -Y test-yard bind "$bound" --yes >/dev/null
  ./bin/yard -Y test-yard clone "$git_url" --yes >/dev/null
  ./bin/yard -Y test-yard sync "$source" --target openclaw --yes >/dev/null
  projects="$(./bin/yard -Y test-yard list)"
  [ "$(awk '$1 == "P0Project" { count++ } END { print count+0 }' <<<"$projects")" = 1 ] \
    && [ "$(awk '$1 == "P0Project-2" { count++ } END { print count+0 }' <<<"$projects")" = 1 ] \
    && [ "$(awk '$1 == "P0Project-3" { count++ } END { print count+0 }' <<<"$projects")" = 1 ] \
    || die 'same-basename bind, clone and sync did not receive three canonical names'
  completions="$(./bin/yard -Y test-yard list --complete-projects)"
  for project in P0Project P0Project-2 P0Project-3; do
    grep -Fxq "$project" <<<"$completions" \
      || die "project completion omitted $project"
  done
  incus exec yard-test-yard --project subyard-test-yard -- \
    jq -e '
      .identityVersion == 2 and .projectId == "P0Project-3" and
      .name == "P0Project-3" and .yard == "test-yard"
    ' /srv/workspaces/P0Project-3/.subyard-meta.json >/dev/null \
    || die 'canonical project metadata was not published'
  if ./bin/yard -Y test-yard sync "$rejected" --name P0Project --yes \
    >/dev/null 2>&1; then
    die 'explicit colliding project name reached physical mutation'
  fi
  [ "$(./bin/yard -Y test-yard list | awk '
    $1 == "P0Project" || $1 == "P0Project-2" || $1 == "P0Project-3" { count++ }
    END { print count+0 }
  ')" = 3 ] \
    || die 'explicit collision changed the project inventory'
  ./bin/yard -Y test-yard bind "$bound" --yes >/dev/null
  ./bin/yard -Y test-yard sync "$source" --target openclaw --yes >/dev/null
  if ./bin/yard -Y test-yard clone "$git_url" --yes >/dev/null 2>&1; then
    die 'repeat clone of the same source created another identity'
  fi
  if ./bin/yard -Y test-yard sync "$bound" --yes >/dev/null 2>&1; then
    die 'same source changed mode from bind to sync'
  fi
  [ "$(./bin/yard -Y test-yard list | awk '
    $1 == "P0Project" || $1 == "P0Project-2" || $1 == "P0Project-3" { count++ }
    END { print count+0 }
  ')" = 3 ] \
    || die 'same-source retries changed the project inventory'
  reservation="$(./bin/yard -Y test-yard _project-state reserve \
    "p0-interrupted-$TOKEN" "/tmp/p0-interrupted-$TOKEN" sync P0Interrupted 0)"
  replay="$(./bin/yard -Y test-yard _project-state reserve \
    "p0-interrupted-$TOKEN" "/tmp/p0-interrupted-$TOKEN" sync P0Interrupted 0)"
  [ "$reservation" = "$replay" ] \
    && jq -e '.projectId == "P0Interrupted" and .reserved == true' <<<"$reservation" >/dev/null \
    || die 'owner reservation replay changed canonical identity'
  ./bin/yard -Y test-yard _project-state abort "p0-interrupted-$TOKEN"
  retried="$(./bin/yard -Y test-yard _project-state reserve \
    "p0-retried-$TOKEN" "/tmp/p0-interrupted-$TOKEN" sync P0Interrupted 0)"
  jq -e '.projectId == "P0Interrupted" and .reserved == true' <<<"$retried" >/dev/null \
    || die 'owner reservation abort did not release canonical identity'
  ./bin/yard -Y test-yard _project-state abort "p0-retried-$TOKEN"
  ./bin/yard -Y test-yard shell P0Project --yes -- \
    grep -Fxq bound result.txt
  ./bin/yard -Y test-yard remove P0Project --yes >/dev/null
  ./bin/yard -Y test-yard shell P0Project-2 --yes -- \
    test -d .git
  ./bin/yard -Y test-yard remove P0Project-2 --yes >/dev/null
  ./bin/yard -Y test-yard shell P0Project-3 --yes -- \
    grep -Fxq base result.txt
  ./bin/yard -Y test-yard up "$source" --yes >/dev/null
  ./bin/yard -Y test-yard info "$source" | grep -Fq '"profile": "openclaw"'
  ./bin/yard -Y test-yard down "$source" --yes >/dev/null
  env PATH=/usr/local/sbin:/usr/sbin:/usr/bin:/sbin:/bin ./bin/yard -Y test-yard code "$source" --yes >/dev/null
  ./bin/yard -Y test-yard shell "$source" --yes -- sh -c 'printf "mutated\n" >> result.txt'
  ./bin/yard -Y test-yard export "$source" --yes >/dev/null
  patch="$(grep -RIl -- 'mutated' "${SUBYARD_HOME:-$HOME/.subyard}/exports" | head -n1)"
  [ -n "$patch" ] || die 'project export did not contain the guest change'
  ./bin/yard -Y test-yard remove "$source" --yes >/dev/null
  find "$patch" -delete
  clean_tree "$root" "$MARKER"
}

owner_cleanup() {
  local rc=$? source="/tmp/subyard-p0-project-$TOKEN" patch fingerprint yard registration
  local cleanup_failed=0
  trap - EXIT
  set +e
  clean_tree "$source" "$MARKER" || cleanup_failed=1
  if [ -d "${SUBYARD_HOME:-$HOME/.subyard}/exports" ]; then
    while IFS= read -r patch; do find "$patch" -delete || cleanup_failed=1; done \
      < <(grep -RIl -- "$MARKER" "${SUBYARD_HOME:-$HOME/.subyard}/exports" 2>/dev/null)
  fi
  for yard in e2e-yard test-yard; do
    registration="$OWNER_YARD_DIR/$yard.env"
    [ -f "$registration" ] || continue
    grep -Fqx "# $MARKER" "$registration" || { cleanup_failed=1; continue; }
    if grep -Fqx 'YARD_TEMPLATE=e2e-vms' "$registration"; then
      sed -i 's/^YARD_TEMPLATE=e2e-vms$/YARD_TEMPLATE=test-vms/' "$registration" \
        || cleanup_failed=1
    fi
    if incus project show "subyard-$yard" >/dev/null 2>&1; then
      if ! ./bin/yard -Y "$yard" teardown --yes >/dev/null 2>&1; then
        cleanup_failed=1
        continue
      fi
    fi
    find "$registration" -delete || cleanup_failed=1
  done
  for project in subyard-e2e-yard subyard-test-yard; do
    reclaim_owner_project_if_present "$project" || cleanup_failed=1
  done
  if [ "$OWNER_BASELINE_CAPTURED" = 1 ]; then
    while IFS= read -r fingerprint; do
      [ -n "$fingerprint" ] || continue
      printf '%s\n' "$OWNER_BASELINE_IMAGES" | grep -Fxq "$fingerprint" \
        || incus image delete "$fingerprint" --project default >/dev/null 2>&1 \
        || cleanup_failed=1
    done < <(incus image list --project default --format csv -c f)
  fi
  p0_capacity_remove_subtree "$OWNER_ROOT" || cleanup_failed=1
  p0_capacity_remove_build_cache || cleanup_failed=1
  p0_capacity_remove_root_if_empty || cleanup_failed=1
  [ "$cleanup_failed" = 0 ] || rc=3
  exit "$rc"
}

prepare_owner_go_cache() {
  p0_capacity_reset_build_cache
  p0_capacity_prepare_subtree "$OWNER_ROOT"
  export SUBYARD_HOME="$OWNER_DATA_ROOT"
  export SUBYARD_CONFIG_HOME="$OWNER_CONFIG_HOME"
}

write_owner_registration() { # <yard> <template> <ssh-port> [slot-count]
  local yard="$1" template="$2" port="$3" slots="${4:-2}" registration
  [[ "$slots" =~ ^[1-9][0-9]{0,2}$ ]] \
    || die "invalid diagnostic slot count $slots"
  registration="$OWNER_YARD_DIR/$yard.env"
  install -d -m 0700 "$OWNER_YARD_DIR"
  if [ -e "$registration" ]; then
    grep -Fqx "# $MARKER" "$registration" \
      || die "refusing to replace unrelated registration $registration"
  fi
  printf '# %s\nYARD_TEMPLATE=%s\nSSH_PORT=%s\nAGENTS=none\nDEV_UID=%s\nE2E_VM_CPU=1\nE2E_VM_MEMORY=%s\nE2E_VM_DISK=10GiB\nE2E_VM_SLOT_COUNT=%s\nE2E_VM_BOOT_TIMEOUT=%s\nBASE_IMAGE=%s\nBASE_IMAGE_FALLBACK=%s\n' \
    "$MARKER" "$template" "$port" "$OWNER_DIAGNOSTIC_DEV_UID" "$OWNER_DIAGNOSTIC_VM_MEMORY" \
    "$slots" "$OWNER_DIAGNOSTIC_VM_BOOT_TIMEOUT" \
    "$OWNER_BASE_IMAGE" "$OWNER_BASE_IMAGE" \
    > "$registration"
}

install_rename_base_runtime() {
  local arch release bundle timeout_file
  [ ! -e "$RENAME_BASE_ROOT" ] \
    || { p0_capacity_assert_root_marker; sudo -n find "$RENAME_BASE_ROOT" -depth -delete; }
  install -d -m 0700 "$RENAME_BASE_ROOT"
  git -C "$RENAME_BASE_ROOT" init -q
  git -C "$RENAME_BASE_ROOT" remote add origin https://github.com/Subyard/Subyard.git
  git -C "$RENAME_BASE_ROOT" fetch -q --depth 1 origin "$RENAME_BASE_REVISION"
  git -C "$RENAME_BASE_ROOT" checkout -q --detach FETCH_HEAD
  [ "$(git -C "$RENAME_BASE_ROOT" rev-parse HEAD)" = "$RENAME_BASE_REVISION" ] \
    || die 'rename-base checkout resolved to the wrong revision'
  # Clean network provisioning can exceed the retired runtime's fixed adapter
  # deadline. Relax only that synthetic build; HEAD still identifies the exact
  # pre-rename source under migration test.
  timeout_file="$RENAME_BASE_ROOT/internal/cli/cli.go"
  [ "$(grep -Fc 'Timeout:        10 * time.Minute,' "$timeout_file")" -eq 1 ] \
    || die 'rename-base adapter timeout fixture no longer matches its source'
  sed -i 's/Timeout:        10 \* time.Minute,/Timeout:        30 * time.Minute,/' \
    "$timeout_file"
  grep -Fqx $'\t\t\tTimeout:        30 * time.Minute,' "$timeout_file" \
    || die 'rename-base adapter timeout fixture was not applied'
  git -C "$RENAME_BASE_ROOT" diff --check
  arch="$(go env GOARCH)"
  release="$RENAME_BASE_ROOT/.build/p0-rename-base-release"
  bundle="$release/subyard-p0-rename-base-linux-$arch.tar.gz"
  "$RENAME_BASE_ROOT/scripts/package-engine.sh" \
    --output-dir "$release" --version p0-rename-base --arch "$arch" >/dev/null
  "$RENAME_BASE_ROOT/scripts/install-runtime-release.sh" \
    --bundle "$bundle" \
    --checksum "$bundle.sha256" \
    --manifest "$bundle.manifest.json" \
    --provenance "$bundle.provenance.json" >/dev/null
}

install_current_base_runtime() {
  local arch release artifact
  arch="$(go env GOARCH)"
  release="$ROOT/.build/p0-current-base-release"
  artifact="$release/subyard-p0-current-base-linux-$arch"
  dev/package-engine.sh \
    --output-dir "$release" \
    --version p0-current-base \
    --arch "$arch" \
    --migration-registry \
      "$ROOT/tests/fixtures/migrations/layout-2-production.json" >/dev/null
  scripts/install-runtime-release.sh \
    --bundle "$artifact.tar.gz" \
    --checksum "$artifact.tar.gz.sha256" \
    --manifest "$artifact.tar.gz.manifest.json" \
    --provenance "$artifact.tar.gz.provenance.json" >/dev/null
}

install_owner_runtime() {
  local arch release artifact
  arch="$(go env GOARCH)"
  release="$ROOT/.build/p0-owner-release"
  artifact="$release/subyard-p0-owner-linux-$arch"
  dev/package-engine.sh --output-dir "$release" --version p0-owner --arch "$arch" >/dev/null
  scripts/install-runtime-release.sh \
    --bundle "$artifact.tar.gz" \
    --checksum "$artifact.tar.gz.sha256" \
    --manifest "$artifact.tar.gz.manifest.json" \
    --provenance "$artifact.tar.gz.provenance.json" >/dev/null
}

prepare_broker_recovery_update() {
  local arch release artifact
  arch="$(go env GOARCH)"
  release="$ROOT/.build/p0-broker-recovery-update"
  artifact="$release/subyard-p0-broker-recovery-update-linux-$arch"
  dev/package-engine.sh \
    --output-dir "$release" \
    --version p0-broker-recovery-update \
    --arch "$arch" >/dev/null
  P0_BROKER_RECOVERY_UPDATE_ARTIFACT="$artifact"
  export P0_BROKER_RECOVERY_UPDATE_ARTIFACT
}

canonical_broker_release_migration_contract() {
  local runtime_root="${SUBYARD_HOME:-$HOME/.subyard}/runtime"
  local active_old_hash active_new_hash candidate_runtime expected_hash inactive_marker
  local rolled_back_hash
  local current_registration old_yard state

  [ "$("$runtime_root/current/bin/yard" --version)" = 'yard p0-current-base' ] \
    || die 'broker migration fixture requires the canonical layout-2 runtime'
  old_yard="$runtime_root/current/bin/yard"

  # Exercise the dedicated layout 2 -> 3 broker operation independently from
  # the owner rename, once stopped and once active.
  write_owner_registration test-yard test-vms 2224
  p0_retry_init_after_plan_stale "$old_yard" -Y test-yard init --yes
  "$old_yard" -Y test-yard stop --yes
  state="$(incus list yard-test-yard --project subyard-test-yard -f csv -c s)"
  [ "$state" = STOPPED ] || die 'inactive broker fixture did not remain stopped'
  inactive_marker="$(incus config get yard-test-yard \
    user.subyard.test_vms_revision --project subyard-test-yard)"

  install_owner_runtime
  state="$(incus list yard-test-yard --project subyard-test-yard -f csv -c s)"
  [ "$state" = STOPPED ] \
    || die 'release migration started an inactive broker'
  [ "$(incus config get yard-test-yard user.subyard.test_vms_revision \
    --project subyard-test-yard)" = "$inactive_marker" ] \
    || die 'release migration rewrote an inactive broker'

  "$runtime_root/current/scripts/install-runtime-release.sh" \
    --runtime-root "$runtime_root" --rollback >/dev/null
  [ "$("$runtime_root/current/bin/yard" --version)" = 'yard p0-current-base' ] \
    || die 'active broker fixture did not restore the previous runtime'
  old_yard="$runtime_root/current/bin/yard"
  candidate_runtime="$runtime_root/previous"
  SUBYARD_REPOSITORY_ROOT="$candidate_runtime" \
    "$candidate_runtime/bin/yard-engine" _migrate cleanup >/dev/null
  "$old_yard" -Y test-yard start --yes
  p0_retry_init_after_plan_stale "$old_yard" -Y test-yard init --yes
  active_old_hash="$(incus exec yard-test-yard --project subyard-test-yard -- \
    sha256sum /usr/local/libexec/subyard/test-vms-inner | awk '{print $1}')"

  install_owner_runtime
  expected_hash="$(sha256sum "$runtime_root/current/bin/yard-engine" | awk '{print $1}')"
  active_new_hash="$(incus exec yard-test-yard --project subyard-test-yard -- \
    sha256sum /usr/local/libexec/subyard/test-vms-inner | awk '{print $1}')"
  [ "$active_new_hash" = "$expected_hash" ] && [ "$active_new_hash" != "$active_old_hash" ] \
    || die 'release migration did not replace the active broker engine'
  incus exec yard-test-yard --project subyard-test-yard -- \
    systemctl is-active --quiet subyard-test-vms-broker.service \
    || die 'release migration did not restore the active broker service'
  "$runtime_root/current/bin/yard" -Y test-yard test-vms status >/dev/null \
    || die 'release migration did not restore broker facade status'

  "$runtime_root/current/scripts/install-runtime-release.sh" \
    --runtime-root "$runtime_root" --rollback >/dev/null
  [ "$("$runtime_root/current/bin/yard" --version)" = 'yard p0-current-base' ] \
    || die 'canonical fixture did not restore its layout-2 runtime'
  rolled_back_hash="$(incus exec yard-test-yard --project subyard-test-yard -- \
    sha256sum /usr/local/libexec/subyard/test-vms-inner | awk '{print $1}')"
  [ "$rolled_back_hash" = "$active_old_hash" ] \
    || die 'runtime rollback did not restore the previous active broker engine'
  candidate_runtime="$runtime_root/previous"
  SUBYARD_REPOSITORY_ROOT="$candidate_runtime" \
    "$candidate_runtime/bin/yard-engine" _migrate cleanup >/dev/null
  old_yard="$runtime_root/current/bin/yard"
  "$old_yard" -Y test-yard teardown --yes
  current_registration="$OWNER_YARD_DIR/test-yard.env"
  grep -Fqx "# $MARKER" "$current_registration" \
    || die 'broker migration fixture lost its owned canonical registration'
  find "$current_registration" -delete
  printf 'ok: active broker auto-upgraded and inactive broker stayed inactive\n'
}

owner_profile_migration_contract() {
  local old_yard runtime_root="${SUBYARD_HOME:-$HOME/.subyard}/runtime" yard_info project_marker
  install_current_base_runtime
  old_yard="$runtime_root/current/bin/yard"
  [ "$("$old_yard" --version)" = 'yard p0-current-base' ] \
    || die 'canonical layout-2 runtime was not installed'

  canonical_broker_release_migration_contract
  install_rename_base_runtime
  old_yard="$runtime_root/current/bin/yard"
  [ "$("$old_yard" --version)" = 'yard p0-rename-base' ] \
    || die 'pre-rename runtime was not installed'
  prepare_owner_image_cache_project subyard-e2e-yard
  write_owner_registration e2e-yard e2e-vms 2224
  p0_retry_init_after_plan_stale "$old_yard" -Y e2e-yard init --yes
  "$old_yard" -Y e2e-yard check
  "$old_yard" -Y e2e-yard start --yes
  "$old_yard" -Y e2e-yard status >/dev/null

  # Source migration normalizes the retired profile before runtime activation.
  write_owner_registration e2e-yard test-vms 2224
  install_owner_runtime
  [ "$("$runtime_root/current/bin/yard" --version)" = 'yard p0-owner' ] \
    || die 'current runtime was not installed over the pre-rename runtime'
  [ ! -e "$OWNER_YARD_DIR/e2e-yard.env" ] \
    || die 'runtime activation retained the old e2e-yard registration'
  [ -f "$OWNER_YARD_DIR/test-yard.env" ] \
    || die 'runtime activation did not create the test-yard registration'
  yard_info="$(./bin/yard -Y test-yard _info)"
  jq -e '.yardName == "test-yard" and .accessKind == "local" and
    .yardInstanceName == "yard-test-yard" and .incusProject == "subyard-test-yard" and
    .sshHost == "yard-test-yard"' \
    <<<"$yard_info" >/dev/null \
    || die "migrated test-yard context is wrong: $yard_info"
  project_marker="$(incus project get subyard-test-yard \
    user.subyard.p0-image-cache 2>/dev/null)"
  [ -z "$project_marker" ] || [ "$project_marker" = "$MARKER" ] \
    || die 'migrated test-yard project has a foreign P0 marker'
  incus project set subyard-test-yard user.subyard.p0-image-cache="$MARKER"
  [ "$(incus project get subyard-test-yard user.subyard.p0-image-cache)" = "$MARKER" ] \
    || die 'migrated test-yard project lost its P0 marker'
  ! incus project show subyard-e2e-yard >/dev/null 2>&1 \
    || die 'old e2e-yard project remains after migrated teardown'
  [ ! -e "${SUBYARD_CONFIG_HOME:-$HOME/.config/subyard}/yards/e2e-yard/projects" ] \
    || die 'old e2e-yard state remains after teardown'
  [ ! -e "$HOME/.ssh/subyard-e2e-yard.config" ] \
    || die 'old e2e-yard route remains after teardown'
  ./bin/yard -Y test-yard check
  ./bin/yard -Y test-yard status >/dev/null
  printf 'ok: runtime upgrade recreated e2e-yard as test-yard automatically\n'
}

is_markerless_migrated_owner_project() {
  local project="$1" instance="$2" expected_marker="$3" registration="$4" revision
  [ "$project" = subyard-test-yard ] && [ "$instance" = yard-test-yard ] \
    && [ -f "$registration" ] && [ ! -L "$registration" ] && [ -O "$registration" ] \
    && [ "$(grep -Fxc -- "# $expected_marker" "$registration")" -eq 1 ] \
    && [ "$(grep -Fxc 'YARD_TEMPLATE=test-vms' "$registration")" -eq 1 ] \
    && [ "$(incus project get "$project" restricted 2>/dev/null)" = true ] \
    && [ "$(incus config get "$instance" user.subyard.managed \
      --project "$project" 2>/dev/null)" = true ] \
    && [ "$(incus config get "$instance" user.subyard.name \
      --project "$project" 2>/dev/null)" = test-yard ] \
    && [ "$(incus config get "$instance" user.subyard.initialized \
      --project "$project" 2>/dev/null)" = true ] \
    || return 1
  revision="$(incus config get "$instance" user.subyard.test_vms_revision \
    --project "$project" 2>/dev/null)"
  case "$revision" in
    1:*:test-yard) return 0 ;;
    *) return 1 ;;
  esac
}

reclaim_owner_project_residue() {
  local project="$1" expected_marker="${2:-}" markerless_registration="${3:-}"
  local instance volume marker unexpected
  case "$project" in
    subyard-e2e-yard)
      instance='yard-e2e-yard'
      volume='yard-srv-e2e-yard'
      ;;
    subyard-test-yard)
      instance='yard-test-yard'
      volume='yard-srv-test-yard'
      ;;
    *) die "unsafe owner residue project $project" ;;
  esac
  marker="$(incus project get "$project" user.subyard.p0-image-cache 2>/dev/null)"
  if [ -n "$expected_marker" ]; then
    if [ "$marker" != "$expected_marker" ]; then
      [ -z "$marker" ] && [ -n "$markerless_registration" ] \
        && is_markerless_migrated_owner_project \
          "$project" "$instance" "$expected_marker" "$markerless_registration" \
        || die "refusing non-P0 owner project $project"
      marker="$expected_marker:markerless-migrated"
    fi
  else
    [[ "$marker" =~ ^subyard-p0-[0-9]+$ ]] \
      || die "refusing non-P0 owner project $project"
  fi
  unexpected="$(incus list --project "$project" --format csv -c n \
    | awk -v expected="$instance" '$0 != expected { print }')"
  [ -z "$unexpected" ] \
    || die "refusing owner project $project with an unexpected instance"
  unexpected="$(incus storage volume list default --project "$project" \
    --format csv -c t,n | awk -F, -v expected_instance="$instance" \
    -v expected_volume="$volume" '
      ! (($1 == "container" && $2 == expected_instance) ||
         ($1 == "custom" && $2 == expected_volume)) { print }
    ')"
  [ -z "$unexpected" ] \
    || die "refusing owner project $project with an unexpected storage volume"
  if incus config show "$instance" --project "$project" >/dev/null 2>&1; then
    incus delete "$instance" --project "$project" --force >/dev/null
  fi
  if incus storage volume show default "$volume" --project "$project" \
    >/dev/null 2>&1; then
    incus storage volume delete default "$volume" --project "$project" >/dev/null
  fi
  incus project delete "$project" >/dev/null
  printf '  [ ok ] removed interrupted P0 owner project %s (%s)\n' \
    "$project" "$marker"
}

reclaim_owner_project_if_present() {
  local project="$1"
  incus project show "$project" >/dev/null 2>&1 || return 0
  if incus project delete "$project" >/dev/null 2>&1; then
    printf '  [ ok ] removed empty owner project residue %s\n' "$project"
    return
  fi
  reclaim_owner_project_residue "$project"
}

prepare_owner_image_cache_project() {
  local project="${1:?owner image-cache project is required}" sibling
  case "$project" in
    subyard-e2e-yard) sibling=subyard-test-yard ;;
    subyard-test-yard) sibling=subyard-e2e-yard ;;
    *) die "unsafe owner image-cache project $project" ;;
  esac
  reclaim_owner_project_if_present "$sibling"
  reclaim_owner_project_if_present "$project"
  incus image info "$OWNER_BASE_IMAGE" --project default >/dev/null 2>&1 \
    || die "test-owned base image alias $OWNER_BASE_IMAGE is unavailable"
  incus project create "$project" \
    -c features.images=false -c user.subyard.p0-image-cache="$MARKER" >/dev/null
}

ensure_owner_base_image() {
  local source="${P0_REAL_INCUS_CONTAINER_IMAGE:-images:debian/13/cloud}"
  if incus image info "$OWNER_BASE_IMAGE" --project default >/dev/null 2>&1; then
    return
  fi
  printf '  [ .. ] caching disposable owner base image %s\n' "$source"
  timeout --foreground "${P0_REAL_INCUS_COMMAND_TIMEOUT:-900}" \
    incus image copy "$source" local: --alias "$OWNER_BASE_IMAGE" </dev/null >/dev/null
  incus image info "$OWNER_BASE_IMAGE" --project default >/dev/null 2>&1 \
    || die "failed to cache owner base image $OWNER_BASE_IMAGE"
  OWNER_BASE_IMAGE_CREATED=1
}

reclaim_broker_recovery_capacity() {
  local available capacity_path=/var/lib/subyard/test-vms/slots
  local minimum=$((7 * 1024 * 1024 * 1024))
  if [ "$OWNER_BASE_IMAGE_CREATED" = 1 ]; then
    incus image delete "$OWNER_BASE_IMAGE" --project default >/dev/null
    OWNER_BASE_IMAGE_CREATED=0
  fi
  p0_capacity_remove_build_cache
  p0_capacity_use_build_cache
  while ! incus exec yard-test-yard --project subyard-test-yard -- \
    test -e "$capacity_path"; do
    [ "$capacity_path" != / ] || break
    capacity_path="$(dirname "$capacity_path")"
  done
  available="$(incus exec yard-test-yard --project subyard-test-yard -- \
    df -B1 --output=avail "$capacity_path" | awk 'NR == 2 {print $1}')"
  [[ "$available" =~ ^[0-9]+$ ]] && [ "$available" -ge "$minimum" ] \
    || die "broker recovery fixture needs at least $minimum pool bytes; have ${available:-unknown}"
  printf '  [ ok ] broker recovery fixture pool reserve available=%s required=%s\n' \
    "$available" "$minimum"
}

reclaim_owner_lease_capacity() {
  local available capacity_path=/var/lib/subyard/test-vms/slots fingerprint path
  local default_build_before default_build_after
  local minimum=$((7 * 1024 * 1024 * 1024))

  # The migration and real-Incus contracts have completed. Reclaim only their
  # marker-owned source/release outputs and images that were absent from the
  # pre-lane baseline before retaining four nested VM disks concurrently.
  for path in \
    "$RENAME_BASE_ROOT" \
    "$ROOT/.build/p0-current-base-release" \
    "$ROOT/.build/p0-owner-release"; do
    [ ! -e "$path" ] || {
      case "$path" in
        "$P0_CAPACITY_STATE_ROOT"/* | "$ROOT"/.build/p0-*-release) ;;
        *) die "unsafe owner lease-capacity cleanup path $path" ;;
      esac
      p0_capacity_delete_tree "$path"
    }
  done
  while IFS= read -r fingerprint; do
    [ -n "$fingerprint" ] || continue
    printf '%s\n' "$OWNER_BASELINE_IMAGES" | grep -Fxq "$fingerprint" \
      || incus image delete "$fingerprint" --project default >/dev/null
  done < <(incus image list --project default --format csv -c f)
  # P0 builds use the marker-owned cache above. The outer VM is a disposable
  # lease, so its reproducible build and dependency caches need not compete
  # with the four retained nested VM disks used by the isolation contract.
  default_build_before="$(p0_capacity_cache_bytes "$P0_CAPACITY_DEFAULT_BUILD_CACHE")"
  env -u GOCACHE go clean -cache
  default_build_after="$(p0_capacity_cache_bytes "$P0_CAPACITY_DEFAULT_BUILD_CACHE")"
  p0_capacity_reclaim_go_module_cache
  p0_capacity_remove_build_cache
  p0_capacity_use_build_cache
  while ! incus exec yard-test-yard --project subyard-test-yard -- \
    test -e "$capacity_path"; do
    [ "$capacity_path" != / ] || break
    capacity_path="$(dirname "$capacity_path")"
  done
  available="$(incus exec yard-test-yard --project subyard-test-yard -- \
    df -B1 --output=avail "$capacity_path" | awk 'NR == 2 {print $1}')"
  [[ "$available" =~ ^[0-9]+$ ]] && [ "$available" -ge "$minimum" ] \
    || die "owner lease fixture needs at least $minimum pool bytes; have ${available:-unknown}"
  printf '  [ ok ] owner lease fixture pool reserve available=%s required=%s default_build=%s->%s\n' \
    "$available" "$minimum" "$default_build_before" "$default_build_after"
}

run_nested_broker_acceptance() {
  local script="$1"
  env \
    SUBYARD_E2E_TEST_MODE=1 \
    SUBYARD_E2E_WORKSPACES_ROOT="$(dirname "$(dirname "$ROOT")")" \
    SUBYARD_E2E_PROJECT_LABEL=Subyard-2 \
    SUBYARD_E2E_PROJECT_YARD=test-yard \
    SUBYARD_E2E_YARD=test-yard \
    bash "$ROOT/$script"
}

owner() (
  [ "$SUBYARD_E2E_VM" = 1 ] || die 'owner lane requires VM1'
	trap owner_cleanup EXIT
  prepare_owner_go_cache
	YARD_BUILD_VERSION=p0-owner dev/build-engine.sh --force >/dev/null
	ensure_owner_incus
	OWNER_BASELINE_IMAGES="$(incus image list --project default --format csv -c f)"
  OWNER_BASELINE_CAPTURED=1
	bash dev/e2e/p0-real-incus.sh
	profile_resource
  prepare_owner_image_cache_project subyard-test-yard
  owner_profile_migration_contract
  write_owner_registration test-yard test-vms 2224 1
  ./bin/yard -Y test-yard start --yes
  SUBYARD_E2E_LEGACY_FIXTURE=1 \
    bash dev/e2e/seed-test-vms-legacy-state.sh subyard-test-yard yard-test-yard
  p0_retry_init_after_plan_stale ./bin/yard -Y test-yard init --yes
  ./bin/yard -Y test-yard check
  p0_retry_init_after_plan_stale ./bin/yard -Y test-yard init --yes
  ! incus exec yard-test-yard --project subyard-test-yard -- \
    test -s /var/lib/subyard/e2e-agent/.ssh/authorized_keys \
    || die 'legacy static controller key survived test-vms reconciliation'
  [ "$(incus exec yard-test-yard --project subyard-test-yard -- stat -c '%U:%G:%a' /var/lib/subyard/test-vms)" = root:root:700 ] \
    || die 'nested state permissions did not converge'
  ! incus exec yard-test-yard --project subyard-test-yard -- id -nG dev | tr ' ' '\n' \
    | grep -Eq '^(incus-admin|yard)$' || die 'dev retained a privileged L1 group'
  # All migration and recovery fixtures have finished compiling. Drop only
  # this run's disposable Go cache before retaining both nested VM pairs;
  # production broker memory and capacity defaults remain unchanged.
  prepare_broker_recovery_update
  p0_capacity_reset_build_cache
  reclaim_owner_lease_capacity
  run_nested_broker_acceptance dev/e2e/p1-lease-acceptance.sh
  write_owner_registration test-yard test-vms 2224 3
  p0_retry_init_after_plan_stale ./bin/yard -Y test-yard init --yes
  run_nested_broker_acceptance dev/e2e/p1-lease-acceptance.sh
  write_owner_registration test-yard test-vms 2224 2
  p0_retry_init_after_plan_stale ./bin/yard -Y test-yard init --yes
  run_nested_broker_acceptance dev/e2e/p0-broker-recovery.sh
  owner_project_contract
  env PATH=/usr/local/sbin:/usr/sbin:/usr/bin:/sbin:/bin ./bin/yard --version >/dev/null
  env PATH=/usr/local/sbin:/usr/sbin:/usr/bin:/sbin:/bin ./bin/yard -Y test-yard list >/dev/null
  env PATH=/usr/local/sbin:/usr/sbin:/usr/bin:/sbin:/bin ./bin/yard -Y test-yard status >/dev/null
  bash tests/engine-release.sh
  ./bin/yard -Y test-yard teardown --yes
  ! incus project show subyard-test-yard >/dev/null 2>&1 || die 'candidate project remains after teardown'
  [ -x "$SUBYARD_HOME/runtime/current/bin/yard" ] \
    || die 'last-yard teardown removed the installed candidate runtime'
  env PATH=/usr/local/sbin:/usr/sbin:/usr/bin:/sbin:/bin \
    "$SUBYARD_HOME/runtime/current/bin/yard" --version >/dev/null \
    || die 'installed candidate runtime is unusable after last-yard teardown'
  printf 'ok: VM1 owner, legacy upgrade, lifecycle, Incus and rollback\n'
)

owner_migration() (
  [ "$SUBYARD_E2E_VM" = 1 ] || die 'owner migration lane requires VM1'
  trap owner_cleanup EXIT
  prepare_owner_go_cache
  YARD_BUILD_VERSION=p0-owner dev/build-engine.sh --force >/dev/null
  ensure_owner_incus
  OWNER_BASELINE_IMAGES="$(incus image list --project default --format csv -c f)"
  OWNER_BASELINE_CAPTURED=1
  incus image info "$OWNER_BASE_IMAGE" --project default >/dev/null 2>&1 \
    || die "cached owner base image $OWNER_BASE_IMAGE is required"
  prepare_owner_image_cache_project subyard-test-yard
  owner_profile_migration_contract
  printf 'ok: VM1 e2e-yard release upgrade migrated to test-yard\n'
)

broker_recovery_owner() (
  [ "$SUBYARD_E2E_VM" = 1 ] || die 'broker recovery owner lane requires VM1'
  trap owner_cleanup EXIT
  prepare_owner_go_cache
  YARD_BUILD_VERSION=p0-owner dev/build-engine.sh --force >/dev/null
  ensure_owner_incus
  OWNER_BASELINE_IMAGES="$(incus image list --project default --format csv -c f)"
  OWNER_BASELINE_CAPTURED=1
  ensure_owner_base_image
  OWNER_DIAGNOSTIC_VM_MEMORY="${P0_BROKER_RECOVERY_VM_MEMORY:-700MiB}"
  install_owner_runtime
  prepare_broker_recovery_update
  prepare_owner_image_cache_project subyard-test-yard
  write_owner_registration test-yard test-vms 2224
  ./bin/yard -Y test-yard init --yes
  ./bin/yard -Y test-yard start --yes
  reclaim_broker_recovery_capacity
  run_nested_broker_acceptance dev/e2e/p0-broker-recovery.sh
  ./bin/yard -Y test-yard teardown --yes
  printf 'ok: VM1 broker logging and quarantine rebuild acceptance\n'
)

controller() (
  local temp='' rc
  [ "$SUBYARD_E2E_VM" = 2 ] || die 'controller lane requires VM2'
  p0_capacity_reset_build_cache
  p0_capacity_prepare_subtree "$P0_CAPACITY_STATE_ROOT/controller"
  trap 'rc=$?; set +e; [ -z "$temp" ] || find "$temp" -depth -delete; p0_capacity_remove_subtree "$P0_CAPACITY_STATE_ROOT/controller"; p0_capacity_remove_build_cache; p0_capacity_remove_root_if_empty; exit "$rc"' EXIT
  shellcheck -x -S warning dev/e2e/p0-acceptance.sh dev/e2e/p0-guest.sh \
    dev/e2e/lib-p0-capacity.sh dev/e2e/p1-lease-acceptance.sh \
    dev/e2e/p0-broker-recovery.sh \
    dev/e2e/p0-real-incus.sh dev/e2e/p0-source-upgrade.sh \
    dev/e2e/power-reconciler-systemd-255.sh \
    dev/e2e/power-reconciler-systemd.sh dev/e2e/power-reconciler-upgrade.sh \
    dev/build-engine.sh tests/build-engine.sh \
    tests/agent-e2e.sh tests/real-host/incus-contract.sh
  ./tests/run.sh
  bash tests/real-host/ssh-rpc.sh
  temp="$(mktemp -d "$P0_CAPACITY_STATE_ROOT/controller/tools.XXXXXX")"
  (
    # shellcheck source=tests/helpers/test-context.sh
    . tests/helpers/test-context.sh
    setup_test_context "$temp/context"
    set -a
    # shellcheck source=config/host.env
    . config/host.env
    set +a
    SUBYARD_HOME="$temp/state" SUBYARD_KEYS_TOOLS_DIR="$temp/tools" \
      bash scripts/install-key-tools.sh -y >/dev/null
  )
  SUBYARD_REAL_KEYS_TOOLS_DIR="$temp/tools" bash tests/real-host/credential-tools.sh
  SUBYARD_REAL_KEYS_TOOLS_DIR="$temp/tools" bash tests/real-host/ssh-credential-peer.sh
  find "$temp" -depth -delete
  temp=''
  printf 'ok: VM2 suite, SSH RPC and real credential adapters\n'
)

install_peer_wrapper() {
  local wrapper="$PEER_ROOT/yard-wrapper" profile="$HOME/.profile"
  [ ! -e "$PEER_YARD_STATE" ] && [ ! -e "$PEER_YARD_BACKUP" ] \
    && [ ! -L "$PEER_YARD_BACKUP" ] || die 'peer yard entry backup is already staged'
  [ ! -d "$PEER_YARD_ENTRY" ] || die 'peer yard entry is a directory'
  install -d -m 0755 "$(dirname "$PEER_YARD_ENTRY")"
  if [ -e "$PEER_YARD_ENTRY" ] || [ -L "$PEER_YARD_ENTRY" ]; then
    printf 'saving\n' > "$PEER_YARD_STATE"
    mv "$PEER_YARD_ENTRY" "$PEER_YARD_BACKUP"
    printf 'saved\n' > "$PEER_YARD_STATE"
  else
    printf 'absent\n' > "$PEER_YARD_STATE"
  fi
  {
    printf '#!/usr/bin/env bash\n# %s\n' "$MARKER"
    printf 'export HOME=%q SUBYARD_OPERATOR_HOME=%q SUBYARD_CONFIG_HOME=%q\n' \
      "$PEER_ROOT/home" "$PEER_ROOT/home" "$PEER_ROOT/config"
    printf 'export SUBYARD_HOME=%q HOST_BASE=%q RESTRICTED_DISK_PATHS=%q\n' \
      "$PEER_DATA_ROOT" "$PEER_HOST_BASE" "$PEER_HOST_BASE"
    printf 'export SUBYARD_KEYS_ROOT=%q SUBYARD_KEYS_TOOLS_DIR=%q SUBYARD_KEYS_CONSUMER_ROOT=%q\n' \
      "$PEER_KEYS_ROOT" "$PEER_KEYS_TOOLS" "$PEER_KEYS_CONSUMER_ROOT"
    printf 'export SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1 SUBYARD_NO_AUDIT=1\n'
    printf 'exec %q/yard "$@"\n' "$PEER_ROOT/bin"
  } > "$wrapper"
  chmod 0755 "$wrapper"
  install -m 0755 "$wrapper" "$PEER_YARD_ENTRY"
  if [ -e "$profile" ]; then
    [ -f "$profile" ] && [ ! -L "$profile" ] || die 'peer login profile is unsafe'
    cp -p "$profile" "$PEER_PROFILE_BACKUP"
    printf 'file\n' > "$PEER_PROFILE_STATE"
  else
    printf 'absent\n' > "$PEER_PROFILE_STATE"
  fi
  printf '\n# %s\nPATH="$HOME/.local/bin:$PATH"\nexport PATH\n' "$MARKER" >> "$profile"
  [ "$(bash -lc 'command -v yard')" = "$PEER_YARD_ENTRY" ] \
    || die "login shell does not resolve the peer CLI through $PEER_YARD_ENTRY"
}

remove_peer_wrapper() {
  local state profile="$HOME/.profile"
  [ -e "$PEER_YARD_STATE" ] || return 0
  state="$(cat "$PEER_YARD_STATE")"
  case "$state" in
    saving)
      if [ -e "$PEER_YARD_BACKUP" ] || [ -L "$PEER_YARD_BACKUP" ]; then
        [ ! -e "$PEER_YARD_ENTRY" ] && [ ! -L "$PEER_YARD_ENTRY" ] \
          || die 'refusing to overwrite the user yard entry during interrupted restore'
        mv "$PEER_YARD_BACKUP" "$PEER_YARD_ENTRY"
      fi
      ;;
    saved|absent)
      if [ -e "$PEER_YARD_ENTRY" ] || [ -L "$PEER_YARD_ENTRY" ]; then
        [ -f "$PEER_YARD_ENTRY" ] && grep -Fqx "# $MARKER" "$PEER_YARD_ENTRY" \
          || die 'refusing to remove a non-fixture user yard entry'
        find "$PEER_YARD_ENTRY" -delete
      fi
      if [ "$state" = saved ]; then
        [ -e "$PEER_YARD_BACKUP" ] || [ -L "$PEER_YARD_BACKUP" ] \
          || die 'saved user yard entry is missing'
        mv "$PEER_YARD_BACKUP" "$PEER_YARD_ENTRY"
      else
        [ ! -e "$PEER_YARD_BACKUP" ] && [ ! -L "$PEER_YARD_BACKUP" ] \
          || die 'unexpected user yard entry backup exists'
      fi
      ;;
    *) die 'peer yard entry backup state is invalid' ;;
  esac
  find "$PEER_YARD_STATE" -delete
  if [ -e "$PEER_PROFILE_STATE" ]; then
    state="$(cat "$PEER_PROFILE_STATE")"
    case "$state" in
      absent) find "$profile" -delete ;;
      file) mv -f "$PEER_PROFILE_BACKUP" "$profile" ;;
      *) die 'peer login profile restore state is invalid' ;;
    esac
    find "$PEER_PROFILE_STATE" -delete
  fi
}

bootstrap_peer_keys() {
  HOME="$PEER_ROOT/home" SUBYARD_OPERATOR_HOME="$PEER_ROOT/home" \
    SUBYARD_CONFIG_HOME="$PEER_ROOT/config" SUBYARD_HOME="$PEER_DATA_ROOT" \
    HOST_BASE="$PEER_HOST_BASE" RESTRICTED_DISK_PATHS="$PEER_HOST_BASE" \
    SUBYARD_KEYS_ROOT="$PEER_KEYS_ROOT" SUBYARD_KEYS_TOOLS_DIR="$PEER_KEYS_TOOLS" \
    "$PEER_ROOT/bin/yard" _keys-init >/dev/null
}

reexec_with_incus_group() {
	local resume_mode="$1" command resume_script="$ROOT/dev/e2e/p0-guest.sh"
	command -v sg >/dev/null 2>&1 || die 'sg is required to activate incus-admin membership'
	if [ "$resume_mode" = peer-prepare-resume ]; then
		resume_script="$PEER_ROOT/src/dev/e2e/p0-guest.sh"
		[ -r "$resume_script" ] || die 'stable peer source is unavailable for incus-admin resume'
	fi
	printf -v command 'exec env SUBYARD_E2E_VM=%q bash %q %q %q %q' \
		"$SUBYARD_E2E_VM" "$resume_script" "$resume_mode" "$TOKEN" "$PEER_IP"
	exec sg incus-admin -c "$command"
}

run_incus_installer() {
  local state_root="$1"; shift
  (
    # shellcheck source=tests/helpers/test-context.sh
    . "$ROOT/tests/helpers/test-context.sh"
    setup_test_context "$state_root/e2e-bootstrap"
    export SUBYARD_USER
    SUBYARD_USER="$(id -un)"
    export SUBYARD_OPERATOR_HOME="$HOME"
    export SUBYARD_CONFIG_DIR="$ROOT/config"
    export SUBYARD_CONFIG_HOME="$state_root/e2e-bootstrap-config"
    export SUBYARD_HOME="$state_root"
    export STORAGE_PATH="$state_root/incus/storage"
    export HOST_BASE="$state_root/host-data"
    export RESTRICTED_DISK_PATHS="$HOST_BASE"
    set -a
    # shellcheck source=config/host.env
    . "$ROOT/config/host.env"
    set +a
    bash "$ROOT/scripts/01-install-incus.sh" "$@"
  )
}

ensure_incus() {
	local state_root="$1" install_marker="${2:-}" resume_mode="$3"
	if command -v incus >/dev/null 2>&1 \
		&& ! id -nG | tr ' ' '\n' | grep -qx incus-admin \
		&& id -nG "$(id -un)" | tr ' ' '\n' | grep -qx incus-admin; then
		reexec_with_incus_group "$resume_mode"
	fi
	if incus info >/dev/null 2>&1; then
    if ! dpkg --compare-versions "$(incus --version)" ge 6.0.6; then
      printf '  [ .. ] VM%s: upgrading Incus to the supported LTS\n' "$SUBYARD_E2E_VM"
      run_incus_installer "$state_root" --yes --zabbly --upgrade-only
      dpkg --compare-versions "$(incus --version)" ge 6.0.6 \
        || die 'Incus upgrade did not reach 6.0.6'
    fi
    if incus storage show default --project default >/dev/null 2>&1 \
      && incus network show incusbr0 --project default >/dev/null 2>&1; then
      return 0
    fi
		[ -z "$install_marker" ] || printf '%s\n' "$MARKER" > "$install_marker"
    printf '  [ .. ] VM%s: restoring the Incus owner API\n' "$SUBYARD_E2E_VM"
    run_incus_installer "$state_root" --yes --zabbly
    return
  fi
  if command -v incus >/dev/null 2>&1 || [ -S /var/lib/incus/unix.socket ]; then
    [ -z "$install_marker" ] || printf '%s\n' "$MARKER" > "$install_marker"
    printf '  [ .. ] VM%s: reconciling a partial Incus installation\n' "$SUBYARD_E2E_VM"
    run_incus_installer "$state_root" --yes --zabbly
    id -nG | tr ' ' '\n' | grep -qx incus-admin || reexec_with_incus_group "$resume_mode"
    return
  fi
	[ -z "$install_marker" ] || printf '%s\n' "$MARKER" > "$install_marker"
	printf '  [ .. ] VM%s: initializing the Incus owner API\n' "$SUBYARD_E2E_VM"
	run_incus_installer "$state_root" --yes --zabbly
	id -nG | tr ' ' '\n' | grep -qx incus-admin || reexec_with_incus_group "$resume_mode"
}

ensure_owner_incus() {
  local resume_mode="${1:-owner}"
  p0_capacity_prepare_platform_root
  ensure_incus "$P0_CAPACITY_PLATFORM_ROOT/incus" '' "$resume_mode"
}
ensure_peer_incus() {
  ensure_incus "$PEER_STATE_ROOT/incus-home" "$PEER_INCUS_MARKER" peer-prepare-resume
}

ensure_peer_snapshot_fixture() {
  if incus project show subyard >/dev/null 2>&1; then
    [ "$(incus project get subyard user.subyard.p0)" = "$MARKER" ] \
      || die "Incus project 'subyard' is not the peer fixture"
    [ "$(incus config get yard user.subyard.p0 --project subyard)" = "$MARKER" ] \
      || die "Incus instance 'subyard/yard' is not the peer fixture"
    return
  fi
  install -d -m 0700 "$PEER_STATE_ROOT/incus-pool"
  incus storage create "$PEER_INCUS_POOL" dir source="$PEER_STATE_ROOT/incus-pool" \
    --project default >/dev/null
  incus project create subyard -c features.images=false -c features.profiles=false \
    -c features.storage.volumes=false -c user.subyard.p0="$MARKER" >/dev/null
  incus init --empty yard --project subyard --storage "$PEER_INCUS_POOL" --no-profiles \
    -c user.subyard.p0="$MARKER" >/dev/null
}

cleanup_peer_snapshot_fixture() {
  incus project show subyard >/dev/null 2>&1 || return 0
  [ "$(incus project get subyard user.subyard.p0)" = "$MARKER" ] \
    || die "refusing to clean unrelated Incus project 'subyard'"
  [ "$(incus config get yard user.subyard.p0 --project subyard)" = "$MARKER" ] \
    || die "refusing to clean unrelated Incus instance 'subyard/yard'"
  incus delete yard --project subyard --force >/dev/null
  incus project delete subyard >/dev/null
  incus storage delete "$PEER_INCUS_POOL" --project default >/dev/null
}

cleanup_peer_incus() {
	local fingerprint source
  [ -e "$PEER_INCUS_MARKER" ] || return 0
  [ "$(cat "$PEER_INCUS_MARKER" 2>/dev/null)" = "$MARKER" ] \
    || die 'refusing to clean unmarked peer Incus state'
  [ -z "$(incus list --all-projects --format csv -c n)" ] \
    || die 'peer Incus still has instances'
	if incus storage show default --project default >/dev/null 2>&1; then
		source="$(incus storage get default source --project default)"
		case "$source" in
			"$PEER_STATE_ROOT/incus-home/storage"|"$PEER_STATE_ROOT/incus-home/incus/storage") ;;
			*)
				p0_capacity_require_persistent_path "$source" incus-default-pool
				return
				;;
		esac
		if incus profile device list default --project default 2>/dev/null | grep -qx eth0; then
			incus profile device remove default eth0 --project default >/dev/null
		fi
		if incus profile device list default --project default 2>/dev/null | grep -qx root; then
			incus profile device remove default root --project default >/dev/null
		fi
		while IFS= read -r fingerprint; do
			[ -n "$fingerprint" ] || continue
			incus image delete "$fingerprint" --project default >/dev/null
		done < <(incus image list --project default --format csv -c f)
		incus network show incusbr0 --project default >/dev/null 2>&1 \
			&& incus network delete incusbr0 --project default >/dev/null
		incus storage delete default --project default >/dev/null
	fi
	[ ! -e "$PEER_STATE_ROOT/incus-home" ] \
		|| sudo -n find "$PEER_STATE_ROOT/incus-home" -depth -delete
}

peer_prepare() {
  valid_ip "$PEER_IP" || die 'peer IP is invalid'
  peer_clean
  p0_capacity_reset_build_cache
  p0_capacity_prepare_subtree "$PEER_STATE_ROOT"
  install -d -m 0700 "$PEER_ROOT/src" "$PEER_ROOT/home" "$PEER_ROOT/config/yards"
	printf '%s\n' "$MARKER" > "$PEER_ROOT/.subyard-p0-marker"
  install -d -m 0700 "$PEER_DATA_ROOT" "$PEER_HOST_BASE" "$PEER_KEYS_ROOT" \
    "$PEER_KEYS_TOOLS" "$PEER_KEYS_CONSUMER_ROOT"
	cp -a "$ROOT/." "$PEER_ROOT/src/"
	ensure_peer_incus
	peer_prepare_finish
}

peer_ssh() {
	ssh -i "$PEER_SSH_DIR/id_ed25519" \
		-o BatchMode=yes -o IdentitiesOnly=yes -o StrictHostKeyChecking=yes \
		-o UserKnownHostsFile="$PEER_SSH_DIR/known_hosts" -o GlobalKnownHostsFile=/dev/null \
		-o ConnectTimeout=8 -o ConnectionAttempts=3 \
		-o ServerAliveInterval=5 -o ServerAliveCountMax=2 "$@"
}

peer_prepare_finish() {
  local release="$PEER_STATE_ROOT/releases" version="p0-peer-vm-$SUBYARD_E2E_VM"
  p0_capacity_use_build_cache
	[ -r "$PEER_ROOT/src/tests/helpers/source-control-plane.sh" ] \
		|| die 'stable peer source is incomplete after incus-admin resume'
	ensure_peer_snapshot_fixture
  install -d -m 0700 "$PEER_SSH_DIR"
  if [ ! -e "$PEER_SSH_DIR/id_ed25519" ] && [ ! -e "$PEER_SSH_DIR/id_ed25519.pub" ]; then
    ssh-keygen -q -t ed25519 -N '' -C "$MARKER-vm$SUBYARD_E2E_VM" \
      -f "$PEER_SSH_DIR/id_ed25519"
  fi
  [ -s "$PEER_SSH_DIR/id_ed25519" ] && [ -s "$PEER_SSH_DIR/id_ed25519.pub" ] \
    || die 'synthetic peer SSH identity is incomplete'
  install -d -m 0700 "$release" "$PEER_ROOT/bin"
  "$PEER_ROOT/src/dev/package-engine.sh" --output-dir "$release" --version "$version" >/dev/null
  HOME="$PEER_ROOT/home" SUBYARD_HOME="$PEER_DATA_ROOT" \
    SUBYARD_CONFIG_HOME="$PEER_ROOT/config" YARD_BIN_DIR="$PEER_ROOT/bin" \
    YARD_SHELL_RC="$PEER_ROOT/home/.bashrc" YARD_LOGIN_RC="$PEER_ROOT/home/.profile" \
    YARD_RELEASE_BASE_URL="file://$release" YARD_RELEASE_VERSION="$version" \
    PATH=/usr/local/sbin:/usr/sbin:/usr/bin:/sbin:/bin \
    "$release/subyard-install.sh" --yes >/dev/null
  [ "$(readlink "$PEER_ROOT/bin/yard")" = "$PEER_DATA_ROOT/runtime/current/bin/yard" ] \
    && [ "$("$PEER_ROOT/bin/yard" --version)" = "yard $version" ] \
    || die 'peer standalone release is not active'
  (
    # shellcheck source=tests/helpers/test-context.sh
    . "$PEER_ROOT/src/tests/helpers/test-context.sh"
    setup_test_context "$PEER_ROOT"
    set -a
    # shellcheck source=config/host.env
    . "$PEER_ROOT/src/config/host.env"
    set +a
    SUBYARD_HOME="$PEER_DATA_ROOT" SUBYARD_KEYS_TOOLS_DIR="$PEER_KEYS_TOOLS" \
      bash "$PEER_DATA_ROOT/runtime/current/scripts/install-key-tools.sh" -y >/dev/null
  )
  bootstrap_peer_keys
  install_peer_wrapper
  printf 'ok: VM%s local cross-owner fixture staged\n' "$SUBYARD_E2E_VM"
}

peer_yard_start() {
  local base_image=images:debian/13/cloud
  exec </dev/null
  [ "$SUBYARD_E2E_VM" = 2 ] || die 'remote project target requires VM2'
  if [ -e "$PEER_REAL_YARD_MARKER" ]; then
    [ "$(cat "$PEER_REAL_YARD_MARKER" 2>/dev/null)" = "$MARKER" ] \
      || die 'real peer yard resume marker is invalid'
  else
    cleanup_peer_snapshot_fixture
    printf '%s\n' "$MARKER" > "$PEER_REAL_YARD_MARKER"
  fi
  if ! incus project show subyard >/dev/null 2>&1; then
    incus project create subyard -c features.images=false >/dev/null
  fi
  if incus image info "$OWNER_BASE_IMAGE" --project default >/dev/null 2>&1; then
    base_image="$OWNER_BASE_IMAGE"
  fi
  install -d -m 0700 "$PEER_ROOT/config"
  printf 'SSH_PORT=3222\nDEV_UID=1001\nBASE_IMAGE=%s\nBASE_IMAGE_FALLBACK=%s\n' \
    "$base_image" "$base_image" > "$PEER_ROOT/config/config.env"
  timeout --foreground "${P0_PEER_YARD_TIMEOUT:-1800}" "$PEER_YARD_ENTRY" init --yes
  "$PEER_YARD_ENTRY" start --yes
  printf 'ok: VM2 release-installed remote yard is running\n'
}

peer_info() {
  local identity host type blob comment extra
  [ "$(cat "$PEER_ROOT/.subyard-p0-marker" 2>/dev/null)" = "$MARKER" ] \
    || die 'cross-owner fixture marker is missing'
  read -r type blob comment extra < "$PEER_SSH_DIR/id_ed25519.pub"
  [ "$type" = ssh-ed25519 ] && [[ "$blob" =~ ^[A-Za-z0-9+/=]+$ ]] \
    && [ "$comment" = "$MARKER-vm$SUBYARD_E2E_VM" ] && [ -z "$extra" ] \
    || die 'synthetic peer public key is invalid'
  identity="$type $blob $comment"
  host="$(normalized_ed25519 "$(sudo -n cat /etc/ssh/ssh_host_ed25519_key.pub)")" \
    || die 'VM SSH host key is unavailable or invalid'
  printf 'identity\t%s\nhost\t%s\n' "$identity" "$host"
}

peer_authorize() {
  local type blob comment extra normalized_host authorized="$HOME/.ssh/authorized_keys"
  valid_ip "$PEER_IP" || die 'peer IP is invalid'
  read -r type blob comment extra <<<"$PEER_PUBLIC_KEY"
  [ "$type" = ssh-ed25519 ] && [[ "$blob" =~ ^[A-Za-z0-9+/=]+$ ]] \
    && [[ "$comment" =~ ^$MARKER-vm[12]$ ]] && [ -z "$extra" ] \
    || die 'peer synthetic public key is invalid'
  normalized_host="$(normalized_ed25519 "$PEER_HOST_KEY")" \
    || die 'peer SSH host key is invalid'
  [ ! -L "$HOME/.ssh" ] && [ ! -L "$authorized" ] \
    || die 'refusing symlinked SSH authorization paths'
  install -d -m 0700 "$HOME/.ssh" "$PEER_SSH_DIR"
  if [ ! -e "$PEER_AUTH_STATE" ]; then
    if [ -e "$authorized" ]; then
      [ -f "$authorized" ] || die 'SSH authorization target is not a regular file'
      printf 'file\t%s\n' "$(stat -c '%a' "$authorized")" > "$PEER_AUTH_STATE"
    else
      printf 'absent\n' > "$PEER_AUTH_STATE"
    fi
  fi
  touch "$authorized"
  chmod 0600 "$authorized"
  grep -Fqx "$PEER_PUBLIC_KEY" "$authorized" || printf '%s\n' "$PEER_PUBLIC_KEY" >> "$authorized"
  printf '%s\n' "$PEER_PUBLIC_KEY" > "$PEER_SSH_DIR/authorized-peer.pub"
  printf '%s %s\n' "$PEER_IP" "$normalized_host" > "$PEER_SSH_DIR/known_hosts"
  chmod 0600 "$PEER_SSH_DIR/authorized-peer.pub" "$PEER_SSH_DIR/known_hosts"
  install -d -m 0700 "$PEER_ROOT/home/.ssh"
  install -m 0600 "$PEER_SSH_DIR/id_ed25519" "$PEER_ROOT/home/.ssh/id_ed25519"
  install -m 0644 "$PEER_SSH_DIR/id_ed25519.pub" "$PEER_ROOT/home/.ssh/id_ed25519.pub"
  printf '%s %s\n' "$PEER_IP" "$normalized_host" > "$PEER_ROOT/home/.ssh/known_hosts"
  chmod 0600 "$PEER_ROOT/home/.ssh/known_hosts"
}

peer_probe() {
  valid_ip "$PEER_IP" || die 'peer IP is invalid'
  printf '  [ .. ] VM%s: probing synthetic SSH path to %s\n' "$SUBYARD_E2E_VM" "$PEER_IP"
  peer_ssh "dev@$PEER_IP" -- true \
    || die "synthetic SSH path to $PEER_IP failed"
  printf 'ok: VM%s synthetic cross-owner SSH path verified\n' "$SUBYARD_E2E_VM"
}

remove_peer_authorization() {
  local authorized="$HOME/.ssh/authorized_keys" peer_key state mode extra temp
  [ -r "$PEER_SSH_DIR/authorized-peer.pub" ] || return 0
  read -r state mode extra < "$PEER_AUTH_STATE" \
    || die 'SSH authorization restore state is missing'
  [ -z "$extra" ] || die 'SSH authorization restore state is invalid'
  case "$state" in
    absent) [ -z "$mode" ] || die 'SSH authorization restore state is invalid' ;;
    file) [[ "$mode" =~ ^[0-7]{3,4}$ ]] || die 'SSH authorization restore mode is invalid' ;;
    *) die 'SSH authorization restore state is invalid' ;;
  esac
  peer_key="$(cat "$PEER_SSH_DIR/authorized-peer.pub")"
  [ -f "$authorized" ] && [ ! -L "$authorized" ] \
    || die 'synthetic peer authorization target is unavailable or unsafe'
  temp="$(mktemp "$HOME/.ssh/.authorized-keys.XXXXXX")"
  awk -v key="$peer_key" '$0 != key' "$authorized" > "$temp"
  chmod 0600 "$temp"
  mv -f "$temp" "$authorized"
  if [ "$state" = absent ] && [ ! -s "$authorized" ]; then
    find "$authorized" -delete
  elif [ "$state" = file ]; then
    chmod "$mode" "$authorized"
  fi
}

append_frame() { # json file
  local payload="$1" file="$2" hex
  hex="$(printf '%08x' "${#payload}")"
  { printf '%b' "\\x${hex:0:2}\\x${hex:2:2}\\x${hex:4:2}\\x${hex:6:2}"; printf '%s' "$payload"; } >> "$file"
}

decode_frames() { # framed-input json-lines-output
  local input="$1" output="$2" offset=0 total header size
  total="$(stat -c '%s' "$input")"; : > "$output"
  while [ "$offset" -lt "$total" ]; do
    header="$(dd if="$input" bs=1 skip="$offset" count=4 status=none | od -An -tx1 | tr -d ' \n')"
    [ "${#header}" = 8 ] || die 'RPC frame header is truncated'
    size=$((16#$header)); [ $((offset + 4 + size)) -le "$total" ] || die 'RPC frame body is truncated'
    dd if="$input" bs=1 skip=$((offset + 4)) count="$size" status=none >> "$output"
    printf '\n' >> "$output"
    offset=$((offset + 4 + size))
  done
}

peer_rpc() {
  local request response body remote_engine remote_root
  valid_ip "$PEER_IP" || die 'peer IP is invalid'
  remote_root="/home/dev/.cache/subyard-p0-$TOKEN/peer/subyard/runtime/current"
  remote_engine="$remote_root/bin/yard-engine"
  request="$PEER_ROOT/rpc-request"; response="$PEER_ROOT/rpc-response"; body="$PEER_ROOT/rpc-body"
  : > "$request"
  append_frame '{"version":1,"type":"request","id":"negotiate","method":"rpc.negotiate"}' "$request"
  peer_ssh "dev@$PEER_IP" -- env SUBYARD_REPOSITORY_ROOT="$remote_root" \
    "$remote_engine" rpc --stdio \
    < "$request" > "$response"
  decode_frames "$response" "$body"
  jq -e --arg version "p0-peer-vm-$((3 - SUBYARD_E2E_VM))" \
    'select(.id=="negotiate" and .error==null and .result.version==1 and .result.engineVersion==$version)' \
    "$body" >/dev/null \
    || die 'cross-owner negotiation failed'

  : > "$request"
  append_frame '{"version":2,"type":"request","id":"bad","method":"rpc.negotiate"}' "$request"
  peer_ssh "dev@$PEER_IP" -- env SUBYARD_REPOSITORY_ROOT="$remote_root" \
    "$remote_engine" rpc --stdio \
    < "$request" > "$response"
  decode_frames "$response" "$body"
  jq -e 'select(.id=="bad" and .error.code=="incompatible_version")' "$body" >/dev/null \
    || die 'version skew was not rejected'

  : > "$request"
  append_frame '{"version":1,"type":"request","id":"negotiate","method":"rpc.negotiate"}' "$request"
  append_frame '{"version":1,"type":"request","id":"events","operationId":"operation-events","method":"incus.events"}' "$request"
  append_frame '{"version":1,"type":"cancel","id":"cancel","operationId":"operation-events"}' "$request"
  peer_ssh "dev@$PEER_IP" -- env SUBYARD_REPOSITORY_ROOT="$remote_root" \
    "$remote_engine" rpc --stdio < "$request" > "$response"
  decode_frames "$response" "$body"
  jq -s -e 'any(.[]; .id=="cancel" and .result.cancelled=="operation-events") and
    any(.[]; .id=="events" and .operationId=="operation-events" and .error.code=="cancelled")' \
    "$body" >/dev/null || die 'live RPC cancellation failed'

  printf '\0\0\0\20broken' | peer_ssh "dev@$PEER_IP" -- \
    env SUBYARD_REPOSITORY_ROOT="$remote_root" "$remote_engine" rpc --stdio >/dev/null 2>&1 || true
  : > "$request"
  append_frame '{"version":1,"type":"request","id":"negotiate","method":"rpc.negotiate"}' "$request"
  append_frame '{"version":1,"type":"request","id":"snapshot","method":"system.snapshot"}' "$request"
  peer_ssh "dev@$PEER_IP" -- env SUBYARD_REPOSITORY_ROOT="$remote_root" \
    "$remote_engine" rpc --stdio \
    < "$request" > "$response"
  decode_frames "$response" "$body"
  if ! jq -s -e 'any(.[]; .id=="negotiate" and .error==null) and
    any(.[]; .id=="snapshot" and .type=="event" and .event=="snapshot.ready") and
    any(.[]; .id=="snapshot" and .type=="response" and .error==null and .result.revision>=1)' \
    "$body" >/dev/null; then
    printf 'p0-guest: resync frames:\n' >&2
    sed -n '1,12p' "$body" >&2
    die 'RPC did not renegotiate and resync after disconnect'
  fi
  printf 'ok: VM%s cross-owner RPC, cancellation, skew and resync\n' "$SUBYARD_E2E_VM"
}

peer_credentials() {
  local expected credential remote_hash source_hash
  [ "$SUBYARD_E2E_VM" = 1 ] || die 'credential controller requires VM1'
  valid_ip "$PEER_IP" || die 'peer IP is invalid'
  expected="$PEER_ROOT/synthetic-credential"
  printf 'subyard-synthetic-p0-cross-owner\n' > "$expected"; chmod 0600 "$expected"
  "$PEER_YARD_ENTRY" keys trust @peer --yes >/dev/null
  "$PEER_YARD_ENTRY" keys add p0-cross-owner --kind file --zone p0-cross-owner \
    --consumer staging-env --file "$expected" --yes >/dev/null
  credential="$("$PEER_YARD_ENTRY" keys list | awk -F '\t' '$8=="p0-cross-owner" {print $1}')"
  [ -n "$credential" ] || die 'cross-owner credential was not created'
  "$PEER_YARD_ENTRY" keys sync @peer --now --yes >/dev/null
  peer_ssh "dev@$PEER_IP" -- bash -lc \
    "$(printf '%q' 'yard keys materialize p0-cross-owner --yes')" >/dev/null
  source_hash="$(sha256sum "$expected" | awk '{print $1}')"
  remote_hash="$(peer_ssh "dev@$PEER_IP" -- sha256sum \
    "/home/dev/.cache/subyard-p0-$TOKEN/peer/consumer/staging/p0-cross-owner.env" \
    | awk '{print $1}')"
  [ "$source_hash" = "$remote_hash" ] || die 'cross-owner credential materialization differs'
  "$PEER_YARD_ENTRY" keys revoke "$credential" --yes >/dev/null
  "$PEER_YARD_ENTRY" keys sync @peer --now --yes >/dev/null
  peer_ssh "dev@$PEER_IP" -- bash -lc \
    "$(printf '%q' 'yard keys materialize p0-cross-owner --yes')" >/dev/null
  ! peer_ssh "dev@$PEER_IP" -- test -e \
    "/home/dev/.cache/subyard-p0-$TOKEN/peer/consumer/staging/p0-cross-owner.env" \
    || die 'revoked cross-owner credential remains materialized'
  "$PEER_YARD_ENTRY" remote remove peer --yes >/dev/null
  printf 'ok: real cross-owner credential trust, sync and revoke\n'
}

peer_projects() {
  local source="$PEER_ROOT/project" remote_pwd inventory owner_selector="e2e-vm-2/default"
  local ssh_config="$HOME/.ssh/config"
  local include="Include $PEER_ROOT/home/.ssh/subyard-peer.config"
  [ "$SUBYARD_E2E_VM" = 1 ] || die 'remote project controller requires VM1'
  valid_ip "$PEER_IP" || die 'peer IP is invalid'
  install -d -m 0700 "$source"
  printf '%s\nbase\n' "$MARKER" > "$source/result.txt"
  [ ! -L "$HOME/.ssh" ] && [ ! -L "$ssh_config" ] \
    || die 'refusing symlinked SSH config paths'
  install -d -m 0700 "$HOME/.ssh"
  if [ ! -e "$PEER_CONFIG_STATE" ]; then
    if [ -e "$ssh_config" ]; then
      [ -f "$ssh_config" ] || die 'SSH config target is not a regular file'
      printf 'file\t%s\n' "$(stat -c '%a' "$ssh_config")" > "$PEER_CONFIG_STATE"
    else
      printf 'absent\n' > "$PEER_CONFIG_STATE"
    fi
  fi
  touch "$ssh_config"
  chmod 0600 "$ssh_config"
  grep -Fqx "$include" "$ssh_config" || printf '%s\n' "$include" >> "$ssh_config"
  "$PEER_YARD_ENTRY" remote add peer "dev@$PEER_IP" --yes >/dev/null
  "$PEER_YARD_ENTRY" -Y peer _project-state upsert project project sync yard >/dev/null
  "$PEER_YARD_ENTRY" -Y peer sync "$source" --yes >/dev/null
  inventory="$("$PEER_YARD_ENTRY" list)"
  grep -Eq '(^|[[:space:]])project-2([[:space:]]|$)' <<<"$inventory" \
    || die 'owner allocation did not override the stale controller inventory'
  remote_pwd="$("$PEER_YARD_ENTRY" -Y "$owner_selector" shell project-2 --yes -- pwd)"
  case "$remote_pwd" in /srv/workspaces/*/src) ;; *) die 'remote shell did not enter the synced project' ;; esac
  [ "$remote_pwd" = /srv/workspaces/project-2/src ] \
    || die "remote shell entered unexpected canonical path $remote_pwd"
  "$PEER_YARD_ENTRY" -Y "$owner_selector" shell project-2 --yes -- \
    sh -c 'printf "remote-mutated\n" >> result.txt'
  "$PEER_YARD_ENTRY" -Y "$owner_selector" shell project-2 --yes -- \
    grep -Fqx remote-mutated result.txt
  printf 'ok: owner allocation overrode stale controller inventory\n'
}

peer_projects_offline() {
  local source="$PEER_ROOT/project" inventory rc
  local owner_config="$PEER_ROOT/home/.ssh/subyard-peer.config"
  local owner_backup="$PEER_ROOT/home/.ssh/subyard-peer.config.online"
  [ "$SUBYARD_E2E_VM" = 1 ] || die 'remote project controller requires VM1'
  ssh -O exit yard-peer >/dev/null 2>&1 || true
  cp -p "$owner_config" "$owner_backup"
  {
    printf 'Host *\n    HostName 127.0.0.1\n    Port 9\n'
    cat "$owner_backup"
  } > "$owner_config"
  set +e
  inventory="$("$PEER_YARD_ENTRY" list --live 2>&1)"
  rc=$?
  set -e
  mv -f "$owner_backup" "$owner_config"
  if [ "$rc" != 0 ]; then
    printf 'p0-guest: offline aggregate inventory (status=%s):\n%s\n' \
      "$rc" "$inventory" >&2
    die "aggregate list returned $rc for an offline owner"
  fi
  grep -Fqi stale <<<"$inventory" \
    || die "offline owner did not expose its last snapshot as stale: $inventory"
  grep -Eq '(^|[[:space:]])project-2([[:space:]]|$)' <<<"$inventory" \
    || die 'offline owner lost its explicit stale project snapshot'
  printf 'ok: offline owner returned zero with an explicit stale snapshot\n'
}

peer_projects_finish() {
  local inventory owner_selector="e2e-vm-2/default"
  [ "$SUBYARD_E2E_VM" = 1 ] || die 'remote project controller requires VM1'
  inventory="$("$PEER_YARD_ENTRY" list --live)"
  grep -Eq '(^|[[:space:]])project-2([[:space:]]|$)' <<<"$inventory" \
    || die 'force refresh did not recover the remote owner inventory'
  "$PEER_YARD_ENTRY" -Y "$owner_selector" remove project-2 --yes >/dev/null
  "$PEER_YARD_ENTRY" -Y peer _project-state unregister project
  inventory="$("$PEER_YARD_ENTRY" list --live)"
  ! grep -Eq '(^|[[:space:]])project(-2)?([[:space:]]|$)' <<<"$inventory" \
    || die 'authoritative replacement retained a removed ghost project'
  printf 'ok: force refresh recovered and authoritative deletion left no ghost project\n'
  printf 'ok: release-installed remote add, sync and two project shells\n'
}

peer_deny() {
  local authorized="$HOME/.ssh/authorized_keys" peer_key temp
  [ "$SUBYARD_E2E_VM" = 2 ] || die 'peer denial target requires VM2'
  peer_key="$(cat "$PEER_SSH_DIR/authorized-peer.pub")"
  temp="$(mktemp "$HOME/.ssh/.authorized-keys.XXXXXX")"
  awk -v key="$peer_key" '$0 != key' "$authorized" > "$temp"
  chmod 0600 "$temp"
  mv -f "$temp" "$authorized"
}

peer_allow() {
  local authorized="$HOME/.ssh/authorized_keys" peer_key
  [ "$SUBYARD_E2E_VM" = 2 ] || die 'peer authorization target requires VM2'
  peer_key="$(cat "$PEER_SSH_DIR/authorized-peer.pub")"
  grep -Fqx "$peer_key" "$authorized" || printf '%s\n' "$peer_key" >> "$authorized"
  chmod 0600 "$authorized"
}

remove_peer_ssh_include() {
  local ssh_config="$HOME/.ssh/config"
  local include="Include $PEER_ROOT/home/.ssh/subyard-peer.config"
  local state mode extra temporary
  [ -e "$PEER_CONFIG_STATE" ] || return 0
  read -r state mode extra < "$PEER_CONFIG_STATE" \
    || die 'SSH config restore state is missing'
  [ -z "$extra" ] || die 'SSH config restore state is invalid'
  case "$state" in
    absent) [ -z "$mode" ] || die 'SSH config restore state is invalid' ;;
    file) [[ "$mode" =~ ^[0-7]{3,4}$ ]] || die 'SSH config restore mode is invalid' ;;
    *) die 'SSH config restore state is invalid' ;;
  esac
  [ "$state" != absent ] || [ -e "$ssh_config" ] || return 0
  [ -f "$ssh_config" ] && [ ! -L "$ssh_config" ] \
    || die 'SSH config restore target is unavailable or unsafe'
  temporary="$(mktemp "$HOME/.ssh/.config.XXXXXX")"
  grep -vxF "$include" "$ssh_config" > "$temporary" || true
  chmod 0600 "$temporary"
  mv -f "$temporary" "$ssh_config"
  if [ "$state" = absent ] && [ ! -s "$ssh_config" ]; then
    find "$ssh_config" -delete
  elif [ "$state" = file ]; then
    chmod "$mode" "$ssh_config"
  fi
}

cleanup_peer_yard() {
  [ -e "$PEER_REAL_YARD_MARKER" ] || return 0
  [ "$(cat "$PEER_REAL_YARD_MARKER" 2>/dev/null)" = "$MARKER" ] \
    || die 'refusing to clean unmarked real peer yard'
  "$PEER_YARD_ENTRY" teardown --yes >/dev/null
  find "$PEER_REAL_YARD_MARKER" -delete
}

peer_clean() {
  remove_peer_authorization
  remove_peer_ssh_include
  cleanup_peer_yard
  remove_peer_wrapper
  cleanup_peer_snapshot_fixture
  cleanup_peer_incus
  clean_peer_data
  clean_tree "$PEER_ROOT" "$MARKER"
}

source_upgrade_project_inventory() {
  local query_timeout="${P0_E2E_INCUS_QUERY_TIMEOUT:-30}" inventory rc
  [[ "$query_timeout" =~ ^[1-9][0-9]{0,2}$ ]] && [ "$query_timeout" -le 120 ] \
    || die 'Incus query timeout is invalid'
  set +e
  inventory="$(timeout --foreground "$query_timeout" \
    incus project list --format csv -c n 2>/dev/null)"
  rc=$?
  set -e
  case "$rc" in
    0) printf '%s\n' "$inventory" ;;
    124|137) die "Incus project inventory exceeded ${query_timeout}s" ;;
    *) return 0 ;;
  esac
}

source_upgrade_project_marker() {
  local project="$1" query_timeout="${P0_E2E_INCUS_QUERY_TIMEOUT:-30}" marker rc
  set +e
  marker="$(timeout --foreground "$query_timeout" \
    incus project get "$project" user.subyard.p0-source 2>/dev/null)"
  rc=$?
  set -e
  case "$rc" in
    0) printf '%s\n' "$marker" ;;
    124|137) die "Incus source marker query exceeded ${query_timeout}s for $project" ;;
    *) die "cannot inspect the source-upgrade marker on $project" ;;
  esac
}

source_upgrade_process_matches() {
  local pattern="$1" query_timeout="${P0_SOURCE_RECOVERY_QUERY_TIMEOUT_SECONDS:-10}" rc
  [[ "$query_timeout" =~ ^[1-9][0-9]?$ ]] && [ "$query_timeout" -le 60 ] \
    || die 'source-upgrade recovery query timeout must be an integer from 1 through 60 seconds'
  if timeout --foreground "$query_timeout" pgrep -f -- "$pattern" >/dev/null 2>&1; then
    return 0
  else
    rc=$?
  fi
  case "$rc" in
    1) return 1 ;;
    *) return 2 ;;
  esac
}

source_upgrade_fixture_active() {
  local token="$1" pattern rc
  local patterns=(
    "p0-source-upgrade[.]sh (prepare|resume|finish|clean) $token([[:space:]]|$)"
    "$HOME/[.]cache/subyard-p0-$token/source-upgrade(/|[[:space:]]|$)"
    "/home/subyardp0$token(/|[[:space:]]|$)"
    "/var/tmp/subyard-p0-source-$token(/|[[:space:]]|$)"
  )
  [[ "$token" =~ ^[0-9]+$ ]] || return 2
  for pattern in "${patterns[@]}"; do
    if source_upgrade_process_matches "$pattern"; then
      return 0
    else
      rc=$?
    fi
    [ "$rc" = 1 ] || return 2
  done
  return 1
}

p0_source_fixture_cleanup_token() {
  local token="$1" cleanup_timeout="${P0_SOURCE_RECOVERY_TIMEOUT_SECONDS:-900}"
  local kill_after="${P0_SOURCE_RECOVERY_KILL_AFTER_SECONDS:-10}"
  [[ "$token" =~ ^[0-9]+$ ]] || die 'source-upgrade recovery token is invalid'
  [[ "$cleanup_timeout" =~ ^[1-9][0-9]{0,3}$ ]] && [ "$cleanup_timeout" -le 1800 ] \
    || die 'source-upgrade recovery timeout must be an integer from 1 through 1800 seconds'
  [[ "$kill_after" =~ ^[1-9][0-9]?$ ]] && [ "$kill_after" -le 60 ] \
    || die 'source-upgrade recovery kill grace must be an integer from 1 through 60 seconds'
  timeout --signal=TERM --kill-after="$kill_after" "$cleanup_timeout" \
    bash "$ROOT/dev/e2e/p0-source-upgrade.sh" clean "$token"
}

recover_stale_source_upgrade_fixture() {
  local inventory project marker candidate token='' active_rc
  command -v incus >/dev/null 2>&1 || return 0
  inventory="$(source_upgrade_project_inventory)"
  for project in subyard-e2e-yard subyard-test-yard subyard; do
    grep -Fxq "$project" <<<"$inventory" || continue
    marker="$(source_upgrade_project_marker "$project")"
    [ -n "$marker" ] || continue
    case "$marker" in
      subyard-p0-source-*)
        candidate="${marker#subyard-p0-source-}"
        [[ "$candidate" =~ ^[0-9]+$ ]] \
          || die "refusing malformed source-upgrade marker on $project"
        ;;
      *) die "refusing foreign source-upgrade marker on $project" ;;
    esac
    [ -z "$token" ] || [ "$token" = "$candidate" ] \
      || die 'refusing source-upgrade projects with conflicting fixture markers'
    token="$candidate"
  done
  [ -n "$token" ] || return 0
  if source_upgrade_fixture_active "$token"; then
    die "source-upgrade fixture still has an active process: subyard-p0-source-$token"
  else
    active_rc=$?
  fi
  [ "$active_rc" = 1 ] \
    || die "cannot determine whether source-upgrade fixture is active: subyard-p0-source-$token"
  printf '  [ .. ] VM%s: recovering marker-owned stale source-upgrade fixture %s\n' \
    "$SUBYARD_E2E_VM" "subyard-p0-source-$token"
  p0_source_fixture_cleanup_token "$token"
}

recover_stale_test_default_pool() {
  local source state status used_by used_by_entries rc project
  local stale_root='' marker='' expected_marker='' markerless_registration=''
  local name token=''
  local recover_existing_p0=0
  local query_timeout="${P0_E2E_INCUS_QUERY_TIMEOUT:-30}"
  command -v incus >/dev/null 2>&1 || return 0
  [[ "$query_timeout" =~ ^[1-9][0-9]*$ ]] || die 'Incus query timeout is invalid'
  set +e
  state="$(timeout --foreground "$query_timeout" \
    incus storage show default --project default 2>/dev/null)"
  rc=$?
  set -e
  case "$rc" in
    0) ;;
    124|137) die "Incus default-pool query exceeded ${query_timeout}s" ;;
    *) return 0 ;;
  esac
  source="$(sed -n 's/^  source: //p' <<<"$state")"
  case "$source" in
    /tmp/subyard-hermes-profile.*/storage)
      [ ! -e "$source" ] || return 0
      ;;
    /var/tmp/subyard-nested-teardown.*/storage)
      [ ! -e "$source" ] || return 0
      stale_root="${source%/storage}"
      marker="$stale_root/.marker"
      expected_marker=nested-teardown-e2e-v1
      ;;
    "$HOME"/.cache/subyard-p0-*/owner/subyard/incus/storage)
      stale_root="${source%/owner/subyard/incus/storage}"
      name="${stale_root##*/}"
      token="${name#subyard-p0-}"
      [[ "$token" =~ ^[0-9]+$ ]] \
        || die "refusing stale P0 pool with an invalid allocation path at $source"
      [ "$token" != "$P0_CAPACITY_TOKEN" ] || return 0
      marker="$stale_root/.subyard-p0-marker"
      expected_marker="subyard-p0-$token"
      markerless_registration="$stale_root/owner/config/yards/test-yard.env"
      if [ -e "$source" ] || [ -L "$source" ]; then
        [ -d "$source" ] && [ ! -L "$source" ] \
          || die "refusing unsafe stale P0 pool source at $source"
        ! pgrep -u "$(id -u)" -f "$stale_root" >/dev/null 2>&1 \
          || die "stale P0 pool still has an active process: $stale_root"
        recover_existing_p0=1
      fi
      ;;
    *) return 0 ;;
  esac
  if [ -n "$stale_root" ] && { [ -e "$stale_root" ] || [ -L "$stale_root" ]; }; then
    [ -d "$stale_root" ] && [ ! -L "$stale_root" ] \
      && [ "$(cat "$marker" 2>/dev/null)" = "$expected_marker" ] \
      || die "refusing unmarked stale test pool root at $stale_root"
  fi
  status="$(sed -n 's/^status: //p' <<<"$state")"
  case "$status" in
    Unavailable) ;;
    Created) [ "$recover_existing_p0" = 1 ] \
      || die "refusing to recover an active stale test pool at $source" ;;
    *) die "refusing to recover an active stale test pool at $source" ;;
  esac
  if [ -n "$token" ]; then
    for project in subyard-e2e-yard subyard-test-yard; do
      incus project show "$project" >/dev/null 2>&1 || continue
      reclaim_owner_project_residue \
        "$project" "$expected_marker" "$markerless_registration"
    done
    state="$(timeout --foreground "$query_timeout" \
      incus storage show default --project default 2>/dev/null)"
    used_by_entries="$(sed -n '/^used_by:$/,/^status:/s/^- //p' <<<"$state")"
    if [ "$used_by_entries" = /1.0/profiles/default ]; then
      [ "$(incus profile device get default root pool --project default 2>/dev/null)" = default ] \
        || die 'refusing stale P0 pool with a foreign default root device'
      incus profile device remove default root --project default >/dev/null
      state="$(timeout --foreground "$query_timeout" \
        incus storage show default --project default 2>/dev/null)"
    fi
  fi
  status="$(sed -n 's/^status: //p' <<<"$state")"
  used_by="$(sed -n 's/^used_by: //p' <<<"$state")"
  [ "$used_by" = '[]' ] \
    || die "refusing to recover an active stale test pool at $source"
  case "$status" in
    Unavailable) ;;
    Created) [ "$recover_existing_p0" = 1 ] \
      || die "refusing to recover an active stale test pool at $source" ;;
    *) die "refusing to recover an active stale test pool at $source" ;;
  esac
  printf '  [ .. ] VM%s: removing unused stale test pool at %s\n' \
    "$SUBYARD_E2E_VM" "$source"
  timeout --foreground 120 incus storage delete default --project default >/dev/null
}

capacity_preflight() {
  recover_stale_source_upgrade_fixture
  recover_stale_test_default_pool
  p0_capacity_preflight
}

dependency_verify() {
  local command revision='legacy'
  local -a required=(curl git go jq rg shellcheck ssh sudo tar timeout zsh)
  if [ -r /var/lib/subyard/e2e-dependencies.revision ]; then
    revision="$(cat /var/lib/subyard/e2e-dependencies.revision)"
    [[ "$revision" =~ ^subyard-test-vms-dependencies-v[0-9]+$ ]] \
      || die 'dependency baseline revision is invalid'
    required+=(make)
  elif ! command -v make >/dev/null; then
    printf '  [warn] VM%s legacy broker baseline has no make; candidate reconciliation is required\n' \
      "$SUBYARD_E2E_VM"
  fi
  for command in "${required[@]}"; do
    command -v "$command" >/dev/null || die "dependency baseline misses $command"
  done
  printf 'ok: VM%s dependency baseline revision=%s go=%s shellcheck=%s\n' \
    "$SUBYARD_E2E_VM" "$revision" "$(go env GOVERSION)" "$(shellcheck --version | awk 'NR == 2 {print $2}')"
}

dependency_bootstrap() (
  local cold_root="$P0_CAPACITY_STATE_ROOT/dependency-bootstrap" rc
  local download_pid='' deadline="${P0_DEPENDENCY_BOOTSTRAP_TIMEOUT:-1200}"
  local heartbeat="${P0_DEPENDENCY_HEARTBEAT_SECONDS:-30}" started
  cleanup_dependency_bootstrap() {
    local cleanup_failed=0
    rc=$?
    trap - EXIT INT TERM
    set +e
    if [ -n "$download_pid" ]; then
      kill -TERM "$download_pid" >/dev/null 2>&1 || true
      wait "$download_pid" >/dev/null 2>&1 || true
    fi
    p0_capacity_remove_subtree "$cold_root" || cleanup_failed=1
    p0_capacity_remove_build_cache || cleanup_failed=1
    p0_capacity_remove_root_if_empty || cleanup_failed=1
    [ "$cleanup_failed" = 0 ] || rc=3
    exit "$rc"
  }
  [[ "$deadline" =~ ^[1-9][0-9]*$ ]] || die 'dependency bootstrap timeout is invalid'
  [[ "$heartbeat" =~ ^[1-9][0-9]*$ ]] || die 'dependency heartbeat interval is invalid'
  dependency_verify
  p0_capacity_reset_build_cache
  p0_capacity_prepare_subtree "$cold_root"
  trap cleanup_dependency_bootstrap EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  install -d -m 0700 "$cold_root/modules"
  printf '%s\n' "$P0_CAPACITY_MARKER" > "$cold_root/modules/.subyard-p0-marker"
  started=$SECONDS
  printf '  [ .. ] VM%s cold Go dependency download started deadline=%ss\n' \
    "$SUBYARD_E2E_VM" "$deadline"
  GOMODCACHE="$cold_root/modules" GOCACHE="$P0_CAPACITY_BUILD_CACHE" \
    timeout --signal=TERM --kill-after=10 "$deadline" bash -c '
      for attempt in 1 2 3; do
        go mod download && exit 0
        [ "$attempt" = 3 ] && break
        delay=$((attempt * 5))
        printf "dependency download failed (attempt %s/3); retrying in %ss\n" \
          "$attempt" "$delay" >&2
        sleep "$delay"
      done
      exit 1
    ' &
  download_pid=$!
  while kill -0 "$download_pid" >/dev/null 2>&1; do
    sleep "$heartbeat"
    kill -0 "$download_pid" >/dev/null 2>&1 || break
    printf '  [ .. ] VM%s cold Go dependency download heartbeat elapsed=%ss\n' \
      "$SUBYARD_E2E_VM" "$((SECONDS - started))"
  done
  wait "$download_pid"
  download_pid=''
  printf 'ok: VM%s cold Go dependency bootstrap passed\n' "$SUBYARD_E2E_VM"
)

profile_resource() (
  local fixture="$P0_CAPACITY_STATE_ROOT/profile-resource" handler rc setup output
  p0_capacity_prepare_root
  p0_capacity_prepare_subtree "$fixture"
  trap 'rc=$?; set +e; p0_capacity_remove_subtree "$fixture"; p0_capacity_remove_root_if_empty; exit "$rc"' EXIT
  cp -a "$ROOT/config" "$fixture/config"
  install -d -m 0700 "$fixture/config/profiles/p0-smoke/resources/p0-smoke"
  cat > "$fixture/config/profiles/p0-smoke/resources/p0-smoke.res" <<'EOF'
COMMAND=p0-smoke
HANDLER=resources/p0-smoke/handler.sh
TITLE=Dependency-free P0 smoke
ACTION="up up yard-change recreatable"
ACTION="is-up is-up read-only not-needed"
ACTION="status status read-only not-needed"
ACTION="down down runtime-destruction recreatable"
ACTION="purge purge persistent-data-destruction irreversible"
BRINGUP=up
SHUTDOWN=down
EOF
  handler="$fixture/config/profiles/p0-smoke/resources/p0-smoke/handler.sh"
  cat > "$handler" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
state="${SUBYARD_HOME:?}/p0-profile-resource.state"
data="${SUBYARD_HOME:?}/p0-profile-resource.data"
verb="${1:-}"; [ $# -eq 0 ] || shift
[ $# -eq 0 ] || { printf 'unexpected resource argument\n' >&2; exit 2; }

emit() { # <action> <changed> [consequence]
  if [ "$2" = true ]; then
    printf '{"schema":"yard.resource-action-assessment.v1","action":"%s","changed":true,"consequences":["%s"]}\n' "$1" "$3"
  else
    printf '{"schema":"yard.resource-action-assessment.v1","action":"%s","changed":false,"consequences":[]}\n' "$1"
  fi
}
require_apply() { # <action>
  [ "${SUBYARD_RESOURCE_MODE:-}" = apply ] || exit 2
  [ "${SUBYARD_RESOURCE_ACTION:-}" = "$1" ] || exit 2
  [ -n "${SUBYARD_OPERATION_ID:-}" ] || exit 2
}

case "${SUBYARD_RESOURCE_MODE:-}" in
  prepare)
    case "$verb" in
      up)
        if [ "$(cat "$state" 2>/dev/null)" = up ]; then emit up false; else emit up true 'start the marker-owned P0 resource runtime'; fi
        ;;
      is-up) emit is-up false ;;
      status) emit status false ;;
      down)
        if [ -e "$state" ]; then emit down true 'stop the marker-owned P0 resource runtime'; else emit down false; fi
        ;;
      purge)
        if [ -e "$data" ]; then emit purge true 'irreversibly delete marker-owned P0 persistent data'; else emit purge false; fi
        ;;
      *) exit 2 ;;
    esac
    ;;
  apply)
    require_apply "$verb"
    case "$verb" in
      up)
        install -d -m 0700 "$(dirname "$state")"
        printf 'up\n' > "$state"
        printf 'persistent\n' > "$data"
        ;;
      is-up) [ "$(cat "$state" 2>/dev/null)" = up ] ;;
      status) [ "$(cat "$state" 2>/dev/null)" = up ] && printf 'up\n' || { printf 'down\n'; exit 1; } ;;
      down) find "$state" -delete 2>/dev/null || true ;;
      purge) find "$data" -delete 2>/dev/null || true ;;
      *) exit 2 ;;
    esac
    ;;
  '')
    case "$verb" in
      is-up) [ "$(cat "$state" 2>/dev/null)" = up ] ;;
      -h|--help|help|'') printf 'P0 typed resource fixture\n' ;;
      *) exit 2 ;;
    esac
    ;;
  *) exit 2 ;;
esac
EOF
  chmod 0700 "$handler"
  ./dev/build-engine.sh
  setup="$fixture/context"
  # shellcheck source=tests/helpers/test-context.sh
  . "$ROOT/tests/helpers/test-context.sh"
  setup_test_context "$setup"
  export HOME="$setup/home" SUBYARD_NO_AUDIT=1 SUBYARD_CONFIG_DIR="$fixture/config"
  output="$fixture/default-yes.out"
  printf '\n' | script -qefc \
    "SUBYARD_REPOSITORY_ROOT='$fixture' '$ROOT/.build/yard' p0-smoke up" \
    /dev/null >"$output"
  grep -Fq 'Proceed? [Y/n]' "$output" || die 'P0 Default-Yes resource prompt is missing'
  SUBYARD_REPOSITORY_ROOT="$fixture" "$ROOT/.build/yard" p0-smoke is-up
  [ "$(SUBYARD_REPOSITORY_ROOT="$fixture" "$ROOT/.build/yard" p0-smoke status)" = up ] \
    || die 'dependency-free profile resource status failed'
  output="$(SUBYARD_REPOSITORY_ROOT="$fixture" "$ROOT/.build/yard" p0-smoke up)"
  ! grep -Fq 'Proceed?' <<<"$output" || die 'converged P0 resource prompted'
  SUBYARD_REPOSITORY_ROOT="$fixture" "$ROOT/.build/yard" p0-smoke down --yes >/dev/null
  if SUBYARD_REPOSITORY_ROOT="$fixture" "$ROOT/.build/yard" p0-smoke is-up; then
    die 'dependency-free profile resource survived reverse lifecycle'
  fi
  [ -e "$setup/subyard/p0-profile-resource.data" ] \
    || die 'P0 resource down removed persistent data'

  output="$fixture/default-no.out"
  set +e
  printf '\n' | script -qefc \
    "SUBYARD_REPOSITORY_ROOT='$fixture' '$ROOT/.build/yard' p0-smoke purge" \
    /dev/null >"$output" 2>&1
  rc=$?
  set -e
  [ "$rc" -ne 0 ] || die 'P0 Default-No purge accepted empty Enter'
  grep -Fq 'Proceed? [y/N]' "$output" || die 'P0 Default-No resource prompt is missing'
  [ -e "$setup/subyard/p0-profile-resource.data" ] \
    || die 'declined P0 purge removed persistent data'
  SUBYARD_REPOSITORY_ROOT="$fixture" "$ROOT/.build/yard" p0-smoke purge --yes >/dev/null
  [ ! -e "$setup/subyard/p0-profile-resource.data" ] \
    || die 'explicit P0 purge kept marker-owned persistent data'
  printf 'ok: VM%s typed profile resource covers no-op, Default Yes and marker-owned Default No purge\n' "$SUBYARD_E2E_VM"
)

capacity_verify_cleanup() {
  local path project leftover
  [ ! -e "$P0_CAPACITY_STATE_ROOT" ] \
    || die "P0 state root remains after cleanup: $P0_CAPACITY_STATE_ROOT"
  [ ! -e "$HOME/.cache/subyard-p0-peer-$TOKEN" ] \
    || die 'legacy peer data root remains after cleanup'
  for path in \
    "/tmp/subyard-p0-peer-$TOKEN" \
    "/tmp/subyard-p0-project-$TOKEN" \
    "/tmp/subyard-p0-rename-base-$TOKEN" \
    "/tmp/subyard-p0-go-cache-$TOKEN" \
    "/tmp/subyard-p0-source-$TOKEN.tar.gz" \
    "/var/tmp/subyard-p0-source-release-$TOKEN" \
    "/var/tmp/subyard-p0-power-systemd-$TOKEN"; do
    [ ! -e "$path" ] && [ ! -L "$path" ] \
      || die "legacy or transient P0 state remains after cleanup: $path"
  done
  [ -z "$(find /tmp -maxdepth 1 -name 'subyard-p0-incus.*' -print -quit)" ] \
    || die 'real-Incus transient directory remains after cleanup'
  [ -z "$(find /var/tmp -maxdepth 1 -type d -name 'subyard-nested-teardown.*' -print -quit)" ] \
    || die 'nested-teardown transient directory remains after cleanup'
  [ -z "$(find /tmp -maxdepth 1 -type d -name 'subyard-e2e-systemd255.*' -print -quit)" ] \
    || die 'systemd 255 transient directory remains after cleanup'
  [ -z "$(find /tmp -maxdepth 1 \
    -name "subyard-p0-power-sudoers-$TOKEN.*" -print -quit)" ] \
    || die 'power-systemd temporary sudoers file remains after cleanup'
  [ -z "$(find /run/systemd/system -maxdepth 1 \
    -name 'subyard-e2e-power-reconciler-systemd-*' -print -quit)" ] \
    || die 'power reconciler systemd unit residue remains after cleanup'
  [ -z "$(find /run -maxdepth 1 \
    \( -name 'subyard-e2e-power-reconciler-systemd-*' \
       -o -name 'subyard-e2e-power-reconciler-systemd-*.sh' \) -print -quit)" ] \
    || die 'power reconciler runtime residue remains after cleanup'
  [ -z "$(find /usr/local/libexec -maxdepth 1 \
    -name 'subyard-e2e-power-reconciler-systemd-*.sh' -print -quit 2>/dev/null)" ] \
    || die 'power reconciler executable residue remains after cleanup'
  if command -v incus >/dev/null 2>&1 && incus info >/dev/null 2>&1; then
    leftover="$(incus project list --format csv -c n \
      | awk '/^subyard-nested-e2e-/ { print; exit }')"
    [ -z "$leftover" ] || die "nested-teardown project remains after cleanup: $leftover"
    leftover="$(incus storage list --project default --format csv -c n \
      | awk '/^nested-e2e-/ { print; exit }')"
    [ -z "$leftover" ] || die "nested-teardown pool remains after cleanup: $leftover"
    leftover="$(incus network list --project default --format csv -c n \
      | awk '/^ne[a-z0-9]{6,8}br0$/ { print; exit }')"
    [ -z "$leftover" ] || die "nested-teardown network remains after cleanup: $leftover"
    ! incus storage show "$PEER_INCUS_POOL" --project default >/dev/null 2>&1 \
      || die "P0 Incus pool remains after cleanup: $PEER_INCUS_POOL"
    for project in subyard-p0-real-incus subyard-e2e-yard subyard-test-yard subyard; do
      ! incus project show "$project" >/dev/null 2>&1 \
        || die "P0 Incus project remains after cleanup: $project"
    done
    leftover="$(incus project list --format csv -c n \
      | awk '/^subyard-systemd255-/ { print; exit }')"
    [ -z "$leftover" ] || die "systemd 255 project remains after cleanup: $leftover"
    if incus storage show default --project default >/dev/null 2>&1; then
      p0_capacity_require_persistent_path \
        "$(incus storage get default source --project default)" incus-default-pool
    fi
  fi
  ! id "subyardp0$TOKEN" >/dev/null 2>&1 \
    || die "source-upgrade fixture user remains after cleanup: subyardp0$TOKEN"
  [ ! -e "/etc/sudoers.d/subyard-p0-source-$TOKEN" ] \
    || die 'source-upgrade sudoers fixture remains after cleanup'
  ! id "subyardpower$TOKEN" >/dev/null 2>&1 \
    || die "power-systemd fixture user remains after cleanup: subyardpower$TOKEN"
  [ ! -e "/etc/sudoers.d/subyard-p0-power-systemd-$TOKEN" ] \
    && [ ! -L "/etc/sudoers.d/subyard-p0-power-systemd-$TOKEN" ] \
    || die 'power-systemd sudoers fixture remains after cleanup'
  [ ! -e "/home/subyardpower$TOKEN" ] && [ ! -L "/home/subyardpower$TOKEN" ] \
    || die 'power-systemd fixture home remains after cleanup'
  if [ -L /etc/systemd/system/multi-user.target.wants/subyard-power-reconcile.service ]; then
    [ -e /etc/systemd/system/multi-user.target.wants/subyard-power-reconcile.service ] \
      || die 'dangling power reconciler enablement remains after cleanup'
  fi
  printf '  [ ok ] Go caches after lane reusable_modules=%s default_build=%s\n' \
    "$(p0_capacity_cache_bytes "$P0_CAPACITY_MODULE_CACHE")" \
    "$(p0_capacity_cache_bytes "$P0_CAPACITY_DEFAULT_BUILD_CACHE")"
  printf 'ok: VM%s exact P0 state, pools, projects and releases removed\n' \
    "$SUBYARD_E2E_VM"
}

  case "$MODE" in
  capacity-preflight) capacity_preflight ;;
  capacity-verify-cleanup) capacity_verify_cleanup ;;
  dependency-verify) dependency_verify ;;
  dependency-bootstrap) dependency_bootstrap ;;
  nested-teardown)
    p0_capacity_use_build_cache
    ensure_owner_incus nested-teardown
    bash dev/e2e/nested-teardown-data-boundary.sh
    ;;
  real-incus)
    p0_capacity_use_build_cache
    ensure_owner_incus real-incus
    bash dev/e2e/p0-real-incus.sh
    ;;
  profile-resource)
    profile_resource
    bash dev/e2e/bind-resource-profile.sh
    ;;
  owner) owner ;;
  owner-migration) owner_migration ;;
  broker-recovery-owner) broker_recovery_owner ;;
  controller) controller ;;
  peer-prepare) peer_prepare ;;
  peer-prepare-resume) peer_prepare_finish ;;
  peer-info) peer_info ;;
  peer-authorize) peer_authorize ;;
  peer-probe) peer_probe ;;
  peer-yard-start) peer_yard_start ;;
  peer-projects) peer_projects ;;
  peer-projects-offline) peer_projects_offline ;;
  peer-projects-finish) peer_projects_finish ;;
  peer-deny) peer_deny ;;
  peer-allow) peer_allow ;;
  peer-rpc) peer_rpc ;;
  peer-credentials) peer_credentials ;;
  peer-clean) peer_clean ;;
  *) die 'unknown P0 guest mode' ;;
esac
