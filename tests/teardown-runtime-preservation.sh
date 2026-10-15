#!/usr/bin/env bash
# Last-yard teardown never recursively removes a shared Subyard data root.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

SUBYARD_OPERATOR_HOME="$TMP/operator"
# shellcheck source=scripts/lib/engine-context.sh
. "$ROOT/scripts/lib/engine-context.sh"
# shellcheck source=scripts/lib/host.sh
. "$ROOT/scripts/lib/host.sh"

incus() {
  [ "$*" = "project get fixture features.images" ] || return 1
  printf '%s\n' "$INCUS_FEATURES_IMAGES"
}
INCUS_FEATURES_IMAGES=false
! incus_project_has_isolated_images fixture \
  || fail 'shared default-project images were treated as yard-owned'
INCUS_FEATURES_IMAGES=true
incus_project_has_isolated_images fixture \
  || fail 'isolated project images were not treated as yard-owned'

grep -Fq 'STORAGE_POOL="${STORAGE_POOL:-$SRV_POOL}"' \
  "$ROOT/scripts/teardown-physical.sh" \
  || fail 'teardown does not inherit the configured yard storage pool'

data_home="$TMP/shared-home"
install -d "$data_home/workspaces" "$data_home/runtime/current/bin" "$data_home/logs"
printf 'outer workspace\n' > "$data_home/workspaces/active.code-workspace"
printf 'outer log\n' > "$data_home/logs/yard.log"

[ "$(subyard_home_remove_if_empty "$data_home")" = retained ] || fail 'shared data-root cleanup failed'
[ -f "$data_home/workspaces/active.code-workspace" ] || fail 'outer workspace descriptor was removed'
[ -f "$data_home/logs/yard.log" ] || fail 'outer log was removed'

empty_home="$TMP/empty-home"
install -d "$empty_home"
[ "$(subyard_home_remove_if_empty "$empty_home")" = removed ] && [ ! -e "$empty_home" ] \
  || fail 'empty data root was not removed'

absent_home="$TMP/absent-home"
[ "$(subyard_home_remove_if_empty "$absent_home")" = absent ] \
  || fail 'absent data root was reported as retained'

outside="$TMP/outside"
link="$TMP/data-link"
install -d "$outside"
ln -s "$outside" "$link"
if subyard_home_remove_if_empty "$link"; then
  fail 'symlink data root was accepted'
fi
[ -d "$outside" ] || fail 'symlink target was removed'

if subyard_home_remove_if_empty /; then
  fail 'broad data root was accepted'
fi

config_home="$TMP/config"
default_state="$config_home/projects"
named_state="$config_home/yards/demo/projects"
custom_state="$TMP/custom-state"
install -d "$default_state" "$named_state" "$custom_state"
printf 'default\n' > "$default_state/demo.json"
printf 'named\n' > "$named_state/demo.json"
printf 'custom\n' > "$custom_state/demo.json"

[ "$(subyard_state_remove_canonical "$default_state" "$config_home" '')" = removed ] \
  || fail 'canonical default-yard state was not removed'
[ ! -e "$default_state" ] || fail 'canonical default-yard state remained'
[ "$(subyard_state_remove_canonical "$named_state" "$config_home" demo)" = removed ] \
  || fail 'canonical named-yard state was not removed'
[ ! -e "$named_state" ] || fail 'canonical named-yard state remained'
[ "$(subyard_state_remove_canonical "$custom_state" "$config_home" '')" = preserved ] \
  || fail 'custom state path was not preserved'
[ -f "$custom_state/demo.json" ] || fail 'custom state was removed'

state_target="$TMP/state-target"
state_link="$config_home/projects"
install -d "$state_target" "$(dirname "$state_link")"
printf 'foreign\n' > "$state_target/foreign.json"
ln -s "$state_target" "$state_link"
[ "$(subyard_state_remove_canonical "$state_link" "$config_home" '')" = preserved ] \
  || fail 'symlink state path was not preserved'
[ -f "$state_target/foreign.json" ] || fail 'symlink state target was removed'

if grep -RE 'rm -rf.*SUBYARD_HOME|subyard_home_remove_preserving_runtime' \
  "$ROOT/scripts" >/dev/null; then
  fail 'production teardown still contains broad Subyard-home cleanup'
fi

printf 'ok: last-yard teardown preserves every non-empty shared data root\n'
