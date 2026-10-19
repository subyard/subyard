#!/usr/bin/env bash
# Ordinary installed-release activation in one marker-owned native Incus yard.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." && pwd -P)"
die() { printf 'git-fetch-activation: %s\n' "$*" >&2; exit 2; }
require_unclaimed_key_units() {
  local directory="$1" parent="$1" name mode
  while [ "$parent" != / ]; do
    [ ! -L "$parent" ] && { [ ! -e "$parent" ] || [ -d "$parent" ]; } \
      || die 'unsafe credential unit directory'
    parent="${parent%/*}"
    [ -n "$parent" ] || parent=/
  done
  if [ -d "$directory" ]; then
    mode="$(stat -c '%a' "$directory")"
    [ "$(stat -c '%u' "$directory")" = "$(id -u)" ] && (( (8#$mode & 8#022) == 0 )) \
      || die 'unsafe credential unit directory'
  fi
  for name in subyard-keys-sync.service subyard-keys-sync.timer .subyard-keys-sync.json; do
    [ ! -e "$directory/$name" ] && [ ! -L "$directory/$name" ] \
      || die 'credential unit path already exists'
  done
}
initialize_yard() {
  if [ "$initialized" = 0 ]; then
    [ "$(incus project get "$PROJECT" user.subyard.git-fetch-fixture)" = "$STATE" ] \
      || die 'UID probe project ownership changed'
    # Retire the empty probe; ordinary init must create its own installation.
    incus project delete "$PROJECT" || return "$?"
    claimed=0
  fi
  init_attempted=1
  yard init --yes || return "$?"
  initialized=1
  incus project set "$PROJECT" user.subyard.git-fetch-fixture "$STATE" || return "$?"
  claimed=1
  incus config set "$INSTANCE" user.subyard.git-fetch-fixture "$STATE" --project "$PROJECT"
}
TOKEN="$(date +%s)$$"
case "${1:-}:$#" in :0|--preflight:1|--native-loss:2) ;; *) exit 2 ;; esac
NATIVE_LOSS=''
if [ "${1:-}" = --native-loss ]; then
  [ "$#" = 2 ] || exit 2
  NATIVE_LOSS="$2"
  case "$NATIVE_LOSS" in owner|helper|group) ;; *) exit 2 ;; esac
fi
[[ "$TOKEN" =~ ^[0-9]+$ ]] || die 'allocation token must be numeric'
if [ "${1:-}" = --preflight ]; then
  probe="$(mktemp -d /tmp/subyard-git-fetch-preflight.XXXXXX)"
  trap 'rm -rf -- "$probe"' EXIT
  require_unclaimed_key_units "$probe/absent-units"
  mkdir -m 0700 "$probe/units"
  require_unclaimed_key_units "$probe/units"
  for name in subyard-keys-sync.service subyard-keys-sync.timer .subyard-keys-sync.json; do
    touch "$probe/units/$name"
    if (require_unclaimed_key_units "$probe/units") 2>/dev/null; then die 'existing credential unit accepted'; fi
    rm -- "$probe/units/$name"
    ln -s "$probe/absent" "$probe/units/$name"
    if (require_unclaimed_key_units "$probe/units") 2>/dev/null; then die 'dangling credential unit accepted'; fi
    rm -- "$probe/units/$name"
  done
  ln -s "$probe/units" "$probe/link"
  if (require_unclaimed_key_units "$probe/link") 2>/dev/null; then die 'symlink unit directory accepted'; fi
  touch "$probe/file"
  if (require_unclaimed_key_units "$probe/file") 2>/dev/null; then die 'non-directory unit root accepted'; fi
  chmod 0777 "$probe/units"
  if (require_unclaimed_key_units "$probe/units") 2>/dev/null; then die 'writable unit directory accepted'; fi
  printf 'ok: git-fetch credential unit absence and collision guards\n'
  printf 'ok: git-fetch numeric allocation token=%s\n' "$TOKEN"
  exit 0
fi
[ "${SUBYARD_E2E_VM:-}" = 1 ] && [ -r /run/subyard-e2e-lease.json ] || die 'requires a disposable allocated VM'
[ "$(id -un)" = dev ] || die 'requires the allocated dev operator'
for command in incus jq sudo python3 sha256sum; do command -v "$command" >/dev/null || die "$command is required"; done
sudo -n true || die 'disposable VM sudo is unavailable'
STATE="$(mktemp -d /var/tmp/subyard-git-fetch-activation.XXXXXX)"
PROJECT="subyard-fetch-$TOKEN"
INSTANCE=yard
NAME="fetch-$TOKEN"
printf '%s\n' git-fetch-activation-v1 > "$STATE/.marker"
chmod 0600 "$STATE/.marker"
claimed=0
initialized=0
init_attempted=0
loss_unknown=0
incus() { sudo -n /usr/bin/incus "$@"; }
fresh() {
  if ! id -nG | tr ' ' '\n' | grep -Fxq incus-admin \
    && id -nG "$(id -un)" | tr ' ' '\n' | grep -Fxq incus-admin; then
    local command
    printf -v command '%q ' "$@"
    sg incus-admin -c "exec $command"
  else "$@"; fi
}
yard() { fresh "$YARD_BIN" -Y "$NAME" "$@"; }
migrate() { fresh "$YARD_BIN" migrate "$@"; }
guest() { incus exec "$INSTANCE" --project "$PROJECT" -- "$@"; }
cleanup() {
  local original=$? failed=0 names instance
  trap - EXIT INT TERM
  set +e
  if [ "$loss_unknown" = 1 ]; then
    printf 'git_fetch_pending_fixture_cleanup=allocation-release original_exit=%s\n' "$original" >&2
    exit "$original"
  fi
  if [ "$claimed" = 0 ] && [ "$init_attempted" = 1 ] && incus project show "$PROJECT" >/dev/null 2>&1; then
    printf 'git-fetch-activation: preserving unclaimed partial init project\n' >&2
    failed=1
  fi
  if [ "$claimed" = 1 ]; then
    if [ "$(incus project get "$PROJECT" user.subyard.git-fetch-fixture 2>/dev/null)" = "$STATE" ]; then
      names="$(incus list --project "$PROJECT" --format=json | jq -er '[.[].name] | join(" ")')" || failed=1
      for instance in $names; do
        [ "$(incus config get "$instance" user.subyard.git-fetch-fixture --project "$PROJECT" 2>/dev/null)" = "$STATE" ] || failed=1
      done
      if [ "$failed" = 0 ]; then
        if [ "$initialized" = 1 ]; then
          fresh timeout --signal=TERM --kill-after=10s 75s "$YARD_BIN" -Y "$NAME" teardown --yes || failed=1
        else
          for instance in $names; do incus delete "$instance" --force --project "$PROJECT" || failed=1; done
          [ "$failed" != 0 ] || incus project delete "$PROJECT" || failed=1
        fi
      fi
    else failed=1; fi
  fi
  printf 'git_fetch_activation_cleanup original_exit=%s cleanup_exit=%s\n' "$original" "$failed" >&2
  # Preserve diagnostic files until the enclosing disposable VM is released.
  [ "$failed" = 0 ] || exit 3
  exit "$original"
}
trap cleanup EXIT
trap 'exit 143' TERM
trap 'exit 130' INT
# Existing capacity and product platform preparation run before expensive init.
bash "$ROOT/dev/e2e/p0-guest.sh" capacity-preflight "$TOKEN"
bash "$ROOT/dev/e2e/p0-guest.sh" fixture-platform "$TOKEN"
# shellcheck source=tests/helpers/release-candidate.sh
. "$ROOT/tests/helpers/release-candidate.sh"
ENGINE="$(release_candidate_prepare "$ROOT")" || die 'verified transported candidate is required'
SOURCE="${ENGINE%/bin/yard-engine}"
export SUBYARD_OPERATOR_HOME="$HOME" SUBYARD_CONFIG_HOME="$STATE/config" SUBYARD_HOME="$STATE/data"
export STORAGE_PATH="$HOME/.cache/subyard-e2e-platform/incus/incus/storage"
export SUBYARD_NO_AUDIT=1 SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1 MIN_DISK_GIB=1
# These settings are shipped/command-only; fresh release readiness uses their defaults.
export SUBYARD_KEYS_SYSTEMD_DIR="$SUBYARD_OPERATOR_HOME/.config/systemd/user"
export SUBYARD_KEYS_TOOLS_DIR="$SUBYARD_CONFIG_HOME/tools"
require_unclaimed_key_units "$SUBYARD_KEYS_SYSTEMD_DIR"
export YARD_RUNTIME_ROOT="$STATE/runtime" YARD_RELEASE_CACHE="$STATE/releases"
export YARD_BIN_DIR="$STATE/bin" YARD_SHELL_RC="$STATE/bashrc" YARD_LOGIN_RC="$STATE/profile"
unset YARD_ENGINE_PATH ENVIRONMENT_PROFILES
install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/$NAME"
cat > "$SUBYARD_CONFIG_HOME/config.env" <<EOF
ENVIRONMENT_PROFILES=git-fetch
CODING_TOOL_INTEGRATIONS=
STORAGE_PATH=$STORAGE_PATH
DEV_UID=2000
HOST_BASE=$STATE/host
HOST_MOUNTS=
HOST_LINKS=
RESTRICTED_DISK_PATHS=$STATE/host
FORWARD_SSH_AGENT=0
HOST_CLAUDE_MD=
HOST_CODEX_AGENTS_MD=
HOST_OPENCODE_AGENTS_MD=
BASE_IMAGE=images:debian/13/cloud
BASE_IMAGE_FALLBACK=images:debian/13/cloud
EOF
printf 'INCUS_PROJECT=%s\nYARD_INSTANCE_NAME=%s\nSSH_PORT=%s\n' "$PROJECT" "$INSTANCE" "$((35000 + $$ % 10000))" \
  > "$SUBYARD_CONFIG_HOME/yards/$NAME/config.env"
chmod 0600 "$SUBYARD_CONFIG_HOME/config.env" "$SUBYARD_CONFIG_HOME/yards/$NAME/config.env"
! incus project show "$PROJECT" >/dev/null 2>&1 || die 'fixture project already exists'
incus project create "$PROJECT" -c features.images=false -c "user.subyard.git-fetch-fixture=$STATE"
claimed=1
incus launch images:debian/13/cloud uid-probe --project "$PROJECT" --storage default \
  -c "user.subyard.git-fetch-fixture=$STATE" -c limits.memory=512MiB -c limits.cpu=1
incus exec uid-probe --project "$PROJECT" -- sh -eu -c '
  command -v getent >/dev/null
  for key in dev 2000; do
    rc=0; entry=$(getent passwd "$key") || rc=$?
    if [ "$rc" = 0 ]; then
      [ "$(printf "%s" "$entry" | cut -d: -f1,3)" = dev:2000 ] || exit 1
    else [ "$rc" = 2 ] || exit 1; fi
  done' || die 'selected image DEV_UID prerequisite is unavailable or occupied'
[ "$(incus config get uid-probe user.subyard.git-fetch-fixture --project "$PROJECT")" = "$STATE" ] || die 'UID probe ownership changed'
incus delete uid-probe --force --project "$PROJECT"
YARD_BIN="$(fresh bash -c '. "$1/tests/helpers/release-candidate.sh"; release_candidate_install "$1" "$2"' fixture "$ROOT" "$STATE")"
printf 'git_fetch_activation_stage=normal-init-start\n'
initialize_yard
yard start --yes
[ "$(guest id -u dev)" = 2000 ] || die 'unexpected installed developer UID'
worker=/var/lib/subyard/git-fetch/fetch.py
expected="$(sha256sum "$SOURCE/config/profiles/git-fetch/fetch.py" | cut -d ' ' -f1)"
assert_current() {
  [ "$(guest sha256sum "$worker" | cut -d ' ' -f1)" = "$expected" ] || die 'installed worker differs from release'
  migrate --check --json | jq -e '.outcome.status=="ready"' >/dev/null
}
drift() { guest sh -c 'printf "\n# owning activation fixture drift\n" >> "$1"' fixture "$worker"; }
timer_state() { guest systemctl show subyard-git-fetch.timer --property=ActiveState,UnitFileState; }
printf 'git_fetch_activation_stage=ordinary-migrate-drift\n'
drift
migrate --yes
assert_current
inode="$(guest stat -c '%i' "$worker")"
migrate --yes
[ "$(guest stat -c '%i' "$worker")" = "$inode" ] || die 'repeat migrate rewrote current worker'
guest systemctl disable --now subyard-git-fetch.timer
drift
migrate --yes
assert_current
timer_state | grep -Fxq ActiveState=inactive
timer_state | grep -Fxq UnitFileState=disabled
printf 'git_fetch_activation_stage=deselected-installed-refresh\n'
yard profile disable git-fetch --yes
selection="$(sha256sum "$SUBYARD_CONFIG_HOME/yards/$NAME/config.env" | cut -d ' ' -f1)"
drift
migrate --yes
[ "$(guest sha256sum "$worker" | cut -d ' ' -f1)" = "$expected" ] || die 'deselected installed worker was not refreshed'
[ "$(sha256sum "$SUBYARD_CONFIG_HOME/yards/$NAME/config.env" | cut -d ' ' -f1)" = "$selection" ] || die 'ordinary migrate changed selection'
timer_state | grep -Fxq ActiveState=inactive
timer_state | grep -Fxq UnitFileState=disabled
migrate --check --json | jq -e '.outcome.status=="ready"' >/dev/null
printf 'PASS: installed candidate normal init/start/migrate, worker identity, repeat, selected disabled and deselected intent\n'
if [ -n "$NATIVE_LOSS" ]; then
  loss_case="$STATE/native-loss"
  guest_loss="/tmp/subyard-fetch-loss-$TOKEN"
  install -d -m 0700 "$loss_case" "$loss_case/bin"
  printf 'subyard-profile-native-loss-v1\n' > "$loss_case/.marker"
  # BASH_ENV pauses the actual shipped Python entry only after its archive has
  # arrived, before its original generation claim and lifecycle-lock acquisition.
  guest bash -se -- "$guest_loss" <<'SH'
set -euo pipefail
root=$1
install -d -m 0700 "$root"
printf 'subyard-git-fetch-loss-v1\n' > "$root/.marker"
printf -v quoted '%q' "$root"
printf 'loss_root=%s\n' "$quoted" > "$root/environment"
cat >> "$root/environment" <<'BASH_ENV'
python3() {
  local argument refresh=0 status
  for argument in "$@"; do [ "$argument" != custody-refresh ] || refresh=1; done
  if [ "$refresh" = 1 ]; then
    printf '%s %s\n' "$$" "$(awk '{print $22}' /proc/$$/stat)" > "$loss_root/paused"
    while [ ! -e "$loss_root/release" ]; do sleep .05; done
    if command python3 "$@"; then status=0; else status=$?; fi
    printf '%s\n' "$status" > "$loss_root/terminal"
    return "$status"
  fi
  command python3 "$@"
}
BASH_ENV
chmod 0600 "$root/environment"
SH
  python3 - "$loss_case" "$guest_loss" "$INSTANCE" "$PROJECT" <<'PY'
from pathlib import Path
import shlex,sys
case,guest,instance,project=sys.argv[1:]
script='#!/bin/bash\nset -euo pipefail\n'+''.join(key+'='+shlex.quote(value)+'\n' for key,value in
    [('case',case),('guest',guest),('instance',instance),('project',project)])+r'''
refresh=0
for argument in "$@"; do [ "$argument" != custody-refresh ] || refresh=1; done
if [ "$refresh" = 1 ]; then
  printf 'custody-refresh\n' >> "$case/calls"
  if [ ! -e "$case/paused" ]; then
    arguments=("$@")
    for index in "${!arguments[@]}"; do
      if [ "${arguments[$index]}" = -- ]; then
        arguments=("${arguments[@]:0:$index}" --env "BASH_ENV=$guest/environment" "${arguments[@]:$index}")
        break
      fi
    done
    /usr/bin/incus "${arguments[@]}" <&0 & client=$!
    while ! /usr/bin/incus exec "$instance" --project "$project" -- test -f "$guest/paused" </dev/null; do sleep .05; done
    printf '%s\n' "$$" > "$case/paused"
    while [ ! -e "$case/release" ]; do sleep .05; done
    /usr/bin/incus exec "$instance" --project "$project" -- touch "$guest/release" </dev/null
    wait "$client"
    exit "$?"
  fi
fi
exec /usr/bin/incus "$@"
'''
path=Path(case)/'bin/incus';path.write_text(script);path.chmod(0o700)
PY
  drift
  generation_before="$(guest cat /var/lib/subyard/.git-fetch.native-custody.json | jq -er .generation)"
  loss_observe() {
    fresh /usr/bin/python3 -B "$ROOT/tests/helpers/profile-native-loss.py" \
      --runtime "$(readlink -f "$STATE/runtime/current")" \
      --manifest-sha256 "$(sha256sum "$SOURCE/runtime-files.sha256" | cut -d' ' -f1)" \
      --case-root "$loss_case" --mode "$NATIVE_LOSS" --successor "$ROOT/.subyard-acceptance/fixture-b" \
      --current-version "$(jq -r .version "$ROOT/.subyard-acceptance/candidate.json")" --activation-id git-fetch-runtime "$@"
  }
  successor_version="$(loss_observe --preflight | jq -er .version)"
  PATH="$loss_case/bin:$PATH" loss_observe | tee "$STATE/native-loss.log"
  loss_resources() {
    guest sha256sum "$worker"
    guest cat /var/lib/subyard/.git-fetch.native-custody.json
    timer_state
    readlink "$STATE/runtime/current"
  }
  loss_resources > "$loss_case/resources.before"
  cp "$loss_case/calls" "$loss_case/calls.drained"
  generation_drained="$(guest cat /var/lib/subyard/.git-fetch.native-custody.json | jq -er .generation)"
  # A guest generation is evidence only through the product's supported fence;
  # no fixture writes a generation, terminal receipt or replacement journal.
  if PATH="$loss_case/bin:$PATH" migrate --yes > "$STATE/native-loss-fence.log" 2>&1; then
    generation_after="$(guest cat /var/lib/subyard/.git-fetch.native-custody.json | jq -er .generation)"
    if [ "$NATIVE_LOSS" = owner ]; then
      cmp "$loss_case/calls.drained" "$loss_case/calls" || die 'owner recovery replayed the drained git-fetch mutation'
      [ "$generation_after" = "$generation_drained" ] || die 'owner recovery replaced the drained git-fetch generation'
      loss_resources > "$loss_case/resources.after"
      cmp "$loss_case/resources.before" "$loss_case/resources.after" || die 'owner recovery changed drained git-fetch resources'
    else
      [ "$generation_after" != "$generation_before" ] || die 'native recovery lacked a fresh guest generation'
    fi
    if [ "$NATIVE_LOSS" = group ]; then
      # The original guest request must still be alive at its first claim.
      guest bash -se -- "$guest_loss" <<'SH'
set -euo pipefail
read -r pid start < "$1/paused"
[ "$(awk '{print $22}' "/proc/$pid/stat")" = "$start" ]
touch "$1/release"
SH
      for _ in {1..200}; do
        guest test ! -f "$guest_loss/terminal" || break
        sleep .05
      done
      original_guest_exit="$(guest cat "$guest_loss/terminal")"
      [ "$original_guest_exit" != 0 ] || die 'delayed original native generation was accepted after fencing'
      [ "$(guest cat /var/lib/subyard/.git-fetch.native-custody.json | jq -er .generation)" = "$generation_after" ] \
        || die 'delayed original request changed the successor generation'
    fi
    assert_current
    printf 'git_fetch_native_loss=%s supported_fence=closed delayed_claim=%s\n' "$NATIVE_LOSS" "${original_guest_exit:-not-delayed}"
  else
    loss_unknown=1
    loss_observe --verify-unknown
    loss_observe --verify-unchanged
    loss_resources > "$loss_case/resources.after"
    cmp "$loss_case/resources.before" "$loss_case/resources.after" || die 'UNKNOWN refusal changed git-fetch resources'
    if PATH="$loss_case/bin:$PATH" YARD_RELEASE_BASE_URL="file://$ROOT/.subyard-acceptance/fixture-b" \
      fresh "$YARD_BIN" update --offline --version "$successor_version" --yes > "$STATE/native-loss-new-goal.log" 2>&1; then
      die 'unclosed native git-fetch work admitted a replacement goal'
    fi
    loss_observe --verify-unchanged
    loss_resources > "$loss_case/resources.after"
    cmp "$loss_case/resources.before" "$loss_case/resources.after" || die 'UNKNOWN refusal changed git-fetch resources'
    loss_observe --verify-unknown
    printf 'git_fetch_native_loss=%s supported_fence=unproved outcome=UNKNOWN delayed_claim_rejection=not-proved\n' "$NATIVE_LOSS"
  fi
fi
