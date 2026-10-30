#!/usr/bin/env bash
# Seed only marker-owned disposable yards; install and exercise the current candidate.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
if [ "${1:-}" = --preflight ]; then
  for command in go python3 bash; do command -v "$command" >/dev/null; done
  bash -n "$0"
  go test ./internal/cli ./internal/adapters/incusclient ./internal/config \
    -run 'TestYardAdoption|TestProjectAdoption|TestTeardownInventoryProves|TestRemovePersistentFileSnapshot|TestFullTeardownClassifiesNativeOwnership' -count=1
  exit
fi
RUN_ID="${SUBYARD_E2E_RUN_ID:?run through dev/agent-e2e.sh}"
[ "${SUBYARD_E2E_VM:-}" = 1 ] || exit 2
[ -f "$ROOT/.subyard-acceptance/candidate.json" ] || exit 2
STATE="$(mktemp -d /var/tmp/subyard-adoption.XXXXXX)"
printf '%s\n' "$RUN_ID" > "$STATE/.fixture-run"
NAMES=()
fixture_pool='' fixture_bridge=''
launcher=''
die() { printf 'yard-adoption: %s\n' "$*" >&2; exit 1; }
incus() { sudo -n /usr/bin/incus "$@"; }
native_owner() {
  local command
  command=$(python3 -c 'import shlex,sys; print(shlex.join(sys.argv[1:]))' "$@") || return
  sg incus-admin -c "exec $command"
}
yard() { native_owner "$launcher" -Y "$name" "$@"; }
guest() { incus exec "$instance" --project "$project" -- "$@"; }
persist_storage_path() {
  python3 - "$SUBYARD_CONFIG_HOME/config.env" "$1" <<'PY'
import os,shlex,sys
fd=os.open(sys.argv[1],os.O_WRONLY|os.O_CREAT|os.O_TRUNC|os.O_NOFOLLOW,0o600)
with os.fdopen(fd,"w") as f:
 os.fchmod(f.fileno(),0o600)
 f.write("STORAGE_PATH="+shlex.quote(sys.argv[2])+"\n")
PY
}
prepare_operator_user_bus() {
  local operator uid binding
  operator="$(id -un)" uid="$(id -u)"
  binding="${SUBYARD_USER:-${SUDO_USER:-$operator}}"
  [ "$(id -u "$binding")" = "$uid" ] || die 'fixture inherited a different teardown operator'
  if [ ! -S "/run/user/$uid/bus" ]; then
    if systemctl is-active --quiet "user@$uid.service"; then
      die 'operator user manager is active without its bus'
    fi
    sudo -n systemctl start "user@$uid.service"
  fi
  [ -S "/run/user/$uid/bus" ] || die 'operator user bus did not start'
  env XDG_RUNTIME_DIR="/run/user/$uid" DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$uid/bus" \
    systemctl --user show-environment >/dev/null || die 'operator user bus is unavailable for producer cleanup'
}
cleanup() {
  local original=$? cleanup_exit=0 name
  trap - EXIT
  set +e
  if [ -n "$launcher" ]; then
    install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/platform-sentinel"
    printf 'SSH_PORT=64998\n' > "$SUBYARD_CONFIG_HOME/yards/platform-sentinel/config.env"
    chmod 0600 "$SUBYARD_CONFIG_HOME/yards/platform-sentinel/config.env"
    for name in "${NAMES[@]}"; do
      yard teardown --yes || cleanup_exit=3
      if incus project show "subyard-$name" >/dev/null 2>&1; then cleanup_exit=3; fi
    done
  fi
  if [ -n "$fixture_bridge" ]; then incus network delete "$fixture_bridge" || cleanup_exit=3; fi
  if [ -n "$fixture_pool" ]; then incus storage delete "$fixture_pool" || cleanup_exit=3; fi
  if [ "$cleanup_exit" = 0 ] && [ ! -L "$STATE" ] && [ "$(cat "$STATE/.fixture-run" 2>/dev/null)" = "$RUN_ID" ]; then
    sudo -n find "$STATE" -depth -delete || cleanup_exit=3
  fi
  printf 'yard-adoption: original_exit=%s cleanup_exit=%s\n' "$original" "$cleanup_exit"
  [ "$original" != 0 ] || original=$cleanup_exit
  exit "$original"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP
for command in incus jq sudo sg python3 systemctl; do command -v "$command" >/dev/null || die "$command is required"; done
sudo -n true || die 'disposable VM requires passwordless root helpers'
prepare_operator_user_bus
bash "$ROOT/dev/e2e/p0-guest.sh" fixture-platform "$(date +%s)"
export SUBYARD_OPERATOR_HOME="$STATE/home" SUBYARD_CONFIG_HOME="$STATE/config" SUBYARD_HOME="$STATE/data"
export YARD_RUNTIME_ROOT="$STATE/candidate/runtime" SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1 MIN_DISK_GIB=1
default_storage_path="$(incus storage get default source)"
unset STORAGE_PATH
install -d -m 0700 "$SUBYARD_OPERATOR_HOME" "$SUBYARD_CONFIG_HOME" "$SUBYARD_HOME"
persist_storage_path "$default_storage_path"
launcher="$(native_owner bash -c '
  set -euo pipefail
  . "$1/tests/helpers/release-candidate.sh"
  engine=$(release_candidate_prepare "$1")
  "$engine" config status --all-local >/dev/null
  release_candidate_install "$1" "$2"
' _ "$ROOT" "$STATE/candidate")"
for kind in container vm; do
  suffix="${STATE##*.}"
  name="adopt-$kind-${suffix,,}"
  project="subyard-$name" instance="yard-$name"
  ! incus project show "$project" >/dev/null 2>&1 || die 'fixture project already exists'
  install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/$name"
  port=$((35000 + $$ % 10000))
  [ "$kind" != vm ] || port=$((port + 1))
  cat > "$SUBYARD_CONFIG_HOME/yards/$name/config.env" <<EOF
SSH_PORT=$port
YARD_KIND=$kind
YARD_IMAGE=images:debian/13/cloud
YARD_IMAGE_FALLBACK=images:debian/13/cloud
CODING_TOOL_INTEGRATIONS=
ENVIRONMENT_PROFILES=
DEV_USER=dev
DEV_UID=1001
HOST_BASE=$STATE/host-$kind
RESTRICTED_DISK_PATHS=$STATE/host-$kind
FORWARD_SSH_AGENT=0
LIMITS_CPU=1
LIMITS_MEMORY=1GiB
EOF
  chmod 0600 "$SUBYARD_CONFIG_HOME/yards/$name/config.env"
  if [ "$kind" = vm ]; then
    pool_target="adopt-${suffix,,}" bridge_target="ad-${suffix,,}"
    ! incus storage show "$pool_target" >/dev/null 2>&1 || die 'fixture pool already exists'
    ! incus network show "$bridge_target" >/dev/null 2>&1 || die 'fixture bridge already exists'
    install -d -m 0700 "$STATE/storage-vm"
    incus storage create "$pool_target" dir source="$STATE/storage-vm"
    fixture_pool=$pool_target
    incus network create "$bridge_target" ipv4.address=auto ipv6.address=none
    fixture_bridge=$bridge_target
    # Storage backing paths are host settings, never per-yard scalar overrides.
    persist_storage_path "$STATE/storage-vm"
    cat >> "$SUBYARD_CONFIG_HOME/yards/$name/config.env" <<EOF
SRV_POOL=$fixture_pool
INCUS_BRIDGE=$fixture_bridge
EOF
  fi
  NAMES+=("$name")
  yard init --yes
  yard start --yes
  guest sh -eu -c 'printf preserved > /srv/adoption-sentinel'
  uuid="$(incus config get "$instance" volatile.uuid --project "$project")"
  pid="$(incus query "/1.0/instances/$instance/state?project=$project" | jq -r .pid)"
  # Model the unmarked native boundary without claiming an authenticated old release.
  project_seed_config="$(incus query "/1.0/projects/$project" | jq -c '{description,config:(.config|with_entries(select(.key|startswith("user.subyard.")|not)))}')"
  incus query --request PUT "/1.0/projects/$project" --data "$project_seed_config" >/dev/null
  if yard init --yes > "$STATE/plain-$kind.log" 2>&1; then die 'plain init silently adopted project'; fi
  yard init --adopt --yes
  [ "$(incus config get "$instance" volatile.uuid --project "$project")" = "$uuid" ] || die 'adoption replaced instance'
  [ "$(incus query "/1.0/instances/$instance/state?project=$project" | jq -r .pid)" = "$pid" ] || die 'adoption restarted running workload'
  [ "$(guest cat /srv/adoption-sentinel)" = preserved ] || die 'adoption lost disk data'
  [ "$(incus project get "$project" user.subyard.installation)" = adopted-v1 ] || die 'missing versioned adoption provenance'
  yard init --yes
  [ "$(incus project get "$project" user.subyard.installation)" = adopted-v1 ] || die 'ordinary init erased adoption provenance'
  yard stop --yes
  yard init --adopt --yes
  [ "$(incus config get "$instance" user.subyard.desired_power --project "$project")" = stopped ] || die 'adoption changed desired stopped state'
  [ "$(incus query "/1.0/instances/$instance/state?project=$project" | jq -r .status)" = Stopped ] || die 'adoption started stopped yard'
  printf 'evidence: %s metadata_adoption_identity_data_power_and_ordinary_init=PASS\n' "$kind"
  if [ "$kind" = vm ]; then
    # Erase only this marker-owned fixture's modern host creation receipt to
    # model an unrecorded historical namespace; retain its data as the oracle.
    printf preserved | sudo -n tee "$STATE/host-vm/backups/adoption-sentinel" >/dev/null
    python3 - "$SUBYARD_HOME" "$STATE/host-vm" <<'PY'
import hashlib,json,os,sys
path=sys.argv[1]+"/ownership/host-paths/"+hashlib.sha256(sys.argv[2].encode()).hexdigest()+".json"
if os.path.exists(path):
 with open(path) as f: record=json.load(f)
 assert record['root']==sys.argv[2]
 os.unlink(path)
PY
    # No foreign consumer or platform sentinel refers to these resources.
    [ "$(incus query "/1.0/storage-pools/$fixture_pool" | jq '[.used_by[]? | select(contains("project=")) | select(contains("project=" + $p)|not)] | length' --arg p "$project")" = 0 ] || die 'foreign fixture pool consumer'
    yard teardown --full --yes
    [ "$(sudo -n cat "$STATE/host-vm/backups/adoption-sentinel")" = preserved ] || die 'full removal deleted unrecorded adopted host data'
    incus storage show "$fixture_pool" >/dev/null || die 'full removal inferred pool creation ownership'
    incus network show "$fixture_bridge" >/dev/null || die 'full removal inferred bridge creation ownership'
    ! incus project show "$project" >/dev/null 2>&1 || die 'full removal left adopted project'
    printf 'evidence: adopted_full_removal_preserves_unowned_pool_bridge_and_host_namespace=PASS\n'
    launcher=''
  else
    yard teardown --yes
  fi
  unset 'NAMES[-1]'
done
