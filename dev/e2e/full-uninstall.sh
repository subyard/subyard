#!/usr/bin/env bash
# Held-lease physical full-uninstall fixture. Never run on an operator host.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
mode="${1:-}" token="${2:-}"
[[ "$token" =~ ^[0-9]+$ ]] || { printf 'full-uninstall: numeric lease token required\n' >&2; exit 2; }
case "${SUBYARD_E2E_VM:-}" in 1|2|3) ;; *) printf 'full-uninstall: leased worker required\n' >&2; exit 2 ;; esac
sudo -n jq -e '.schema_version == 2 and (.run | type == "string")' /run/subyard-e2e-lease.json >/dev/null
state="/var/tmp/subyard-full-uninstall-$token"
marker="subyard-full-uninstall-$token"
operator="subyardfull${token:0:20}"
home="/home/$operator"
version="0.19.1-p0.full.$token"
if [ -f "$ROOT/.subyard-acceptance/candidate.json" ]; then
  version="$(jq -er '.version' "$ROOT/.subyard-acceptance/candidate.json")"
fi
pool="subyard-full-$token"
bridge="syfull${token:0:8}"
power="subyard-full-power-$token.service"
sink="subyard-full-sink-$token"
relay=subyard-ssh-relay-24226
foreign="subyard-full-foreign-$token"
foreign_unit="foreign-full-control-$token.service"
fail() { printf 'full-uninstall: %s\n' "$*" >&2; exit 1; }
assert_fixture() { [ -d "$state" ] && [ ! -L "$state" ] && [ "$(cat "$state/.marker")" = "$marker" ] || fail 'refusing unmarked fixture'; }
operator_env() {
  local uid
  uid="$(id -u "$operator")"
  sudo -n runuser -u "$operator" -- env -u SUDO_USER -u SUDO_UID -u SUDO_GID -u SUDO_COMMAND \
    HOME="$home" USER="$operator" LOGNAME="$operator" SHELL=/bin/bash \
    PATH="$state/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" \
    XDG_RUNTIME_DIR="/run/user/$uid" DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$uid/bus" \
    SUBYARD_HOME="$state/data" SUBYARD_CONFIG_HOME="$state/config" \
    YARD_RUNTIME_ROOT="$state/runtime" YARD_RELEASE_CACHE="$state/cache" \
    SUBYARD_KEYS_ROOT="$state/keys" SUBYARD_KEYS_TOOLS_DIR="$state/tools" \
    SUBYARD_KEYS_CONSUMER_ROOT="$state/consumers" \
    SUBYARD_POWER_UNIT_PATH="/etc/systemd/system/$power" \
    SUBYARD_POWER_RECONCILER_PATH="$state/host-helper/power" \
    SUBYARD_TEST_VMS_SINK_SERVICE_PATH="/etc/systemd/system/$sink.service" \
    SUBYARD_TEST_VMS_SINK_TIMER_PATH="/etc/systemd/system/$sink.timer" \
    SUBYARD_TEST_VMS_SINK_PATH="$state/host-helper/sink" \
    bash -c 'cd "$HOME"; exec "$@"' _ "$@"
}
yard() { operator_env "$state/bin/yard" "$@"; }
userctl() { operator_env systemctl --user "$@"; }
assert_project() { [ "$(incus project get "$1" user.subyard.full-fixture)" = "$marker" ] || fail "refusing unmarked project $1"; }
create_fixture_project() {
  local ownership=()
  ! incus project show "$1" >/dev/null 2>&1 || fail "fixture project already exists: $1"
  if [ "$1" != "$foreign" ]; then
    ownership=(-c user.subyard.installation=1 -c "user.subyard.operator_uid=$(id -u "$operator")" -c "user.subyard.data_home=$state/data" -c "user.subyard.config_home=$state/config")
  fi
  # Existing P0 owner fixtures share the platform's cached default-project image.
  incus project create "$1" -c features.images=false -c "user.subyard.full-fixture=$marker" "${ownership[@]}" >/dev/null
}
restore_fixture_default_profile() {
  local baseline original
  assert_fixture
  for baseline in baseline-pool baseline-network; do
    [ -f "$state/$baseline" ] && [ ! -L "$state/$baseline" ] \
      && [ "$(stat -c %U:%a "$state/$baseline")" = dev:600 ] || fail 'unsafe profile baseline'
  done
  if [ "$(incus profile device get default root pool --project default)" = "$pool" ]; then
    original="$(cat "$state/baseline-pool")"
    [ -n "$original" ] && [ "$original" != "$pool" ] || fail 'missing original pool'
    incus storage show "$original" >/dev/null
    incus profile device set default root pool "$original" --project default
  fi
  if [ "$(incus profile device get default eth0 network --project default)" = "$bridge" ]; then
    original="$(cat "$state/baseline-network")"
    [ -n "$original" ] && [ "$original" != "$bridge" ] || fail 'missing original network'
    incus network show "$original" >/dev/null
    incus profile device set default eth0 network "$original" --project default
  fi
}
verify_absence() {
  local project path
  assert_fixture
  for project in "subyard-full-$token" "subyard-full-$token-neighbor" "subyard-full-$token-stopped" "subyard-full-$token-partial"; do
    ! incus project show "$project" >/dev/null 2>&1 || fail "approved project remains: $project"
  done
  for path in runtime config data keys tools consumers cache bin/yard bin/sy; do
    [ ! -e "$state/$path" ] && [ ! -L "$state/$path" ] || fail "approved artifact remains: $path"
  done
  [ ! -e "$state/data.uninstall" ] || fail 'recovery remains after success'
  if [ -f "$state/ufw-before" ]; then
    remaining="$(incus network list --project default -f json | jq -er --arg bridge "$bridge" '[.[] | select(.name==$bridge)] | length')"
    # shellcheck disable=SC2024 # Root reads UFW; the caller owns the fixture output files.
    sudo -n awk -v bridge="$bridge" '$1=="###" && $2=="tuple" && $3=="###" && ($10=="in_" bridge || $10=="out_" bridge)' /etc/ufw/user.rules > "$state/ufw-after"
    # shellcheck disable=SC2024
    sudo -n awk '$1=="###" && $2=="tuple" && $3=="###" && ($10=="in_incusbr0" || $10=="out_incusbr0")' /etc/ufw/user.rules > "$state/ufw-foreign-after"
    chmod 0600 "$state/ufw-after" "$state/ufw-foreign-after"
    cmp -s "$state/ufw-foreign-before" "$state/ufw-foreign-after" || fail 'foreign bridge firewall policy changed'
    if [ "$remaining" = 0 ]; then
      [ ! -s "$state/ufw-after" ] || fail 'removed approved bridge retained firewall rules'
    else
      cmp -s "$state/ufw-before" "$state/ufw-after" || fail 'preserved shared bridge firewall policy changed'
    fi
  fi
  for path in "/etc/systemd/system/$power" "/etc/systemd/system/$sink.service" "/etc/systemd/system/$sink.timer" "$state/host-helper/power" "$state/host-helper/sink"; do
    [ ! -e "$path" ] && [ ! -L "$path" ] || fail "approved host service artifact remains: $path"
  done
  local unit
  for unit in "$power" "$sink.service" "$sink.timer" "$relay.service" "$relay.socket"; do
    ! systemctl is-active --quiet "$unit" || fail "approved system producer remains active: $unit"
    ! systemctl is-enabled --quiet "$unit" || fail "approved system producer remains enabled: $unit"
  done
  for unit in subyard-keys-sync.service subyard-keys-sync.timer; do
    ! userctl is-active --quiet "$unit" || fail "approved credential producer remains active: $unit"
    ! userctl is-enabled --quiet "$unit" || fail "approved credential producer remains enabled: $unit"
    operator_env test ! -e "$home/.config/systemd/user/$unit" || fail "credential unit remains: $unit"
  done
  for path in "/etc/systemd/system/$relay.service" "/etc/systemd/system/$relay.socket" "/etc/systemd/system/sockets.target.wants/$relay.socket" "/etc/systemd/system/multi-user.target.wants/$power" "/etc/systemd/system/timers.target.wants/$sink.timer"; do
    [ ! -e "$path" ] && [ ! -L "$path" ] || fail "approved enablement/relay artifact remains: $path"
  done
  operator_env test ! -e "$home/.config/systemd/user/timers.target.wants/subyard-keys-sync.timer" || fail 'credential timer enablement remains'
  [ -z "$(sudo -n ss -H -lnt '( sport = :24222 or sport = :24223 or sport = :24224 or sport = :24225 or sport = :24226 )')" ] || fail 'approved SSH listener remains'
  sudo -n python3 - "$state/producer-pids.json" "$state" <<'PY_PROCESSES'
import json,pathlib,sys
metadata=json.loads(pathlib.Path(sys.argv[1]).read_bytes())
for approved in json.loads((pathlib.Path(sys.argv[2])/"approved-host-paths.json").read_bytes()):
 path=pathlib.Path(approved)
 if path.exists() or path.is_symlink():raise SystemExit("approved host namespace reappeared")
for directory in metadata["agent_directories"]:
 if pathlib.Path(directory).exists():raise SystemExit("approved signing runtime remains")
for pid,birth in metadata["pids"].items():
 try:
  fields=pathlib.Path("/proc",pid,"stat").read_text().rsplit(")",1)[1].split()
  if fields[0]!="Z" and fields[19]==birth:raise SystemExit("approved producer process remains")
 except FileNotFoundError:pass
for path in pathlib.Path("/proc").glob("[0-9]*/cmdline"):
 try:arguments=path.read_bytes().split(b"\0")
 except (FileNotFoundError,ProcessLookupError):continue
 if any(item.startswith((sys.argv[2]+"/host-helper/").encode()) for item in arguments) or any(item == directory.encode() or item.startswith((directory+"/").encode()) for item in arguments for directory in metadata["agent_directories"]):raise SystemExit("approved producer process reappeared")
PY_PROCESSES
  incus storage volume show "$pool" "foreign-$token" --project default >/dev/null
  [ "$(incus list control --project "$foreign" -f csv -c s)" = RUNNING ] || fail 'foreign workload changed'
  userctl is-active --quiet "$foreign_unit" || fail 'foreign user service stopped'
  operator_env grep -Fxq 'foreign host content' "$state/host/foreign.control" || fail 'foreign host content removed'
  operator_env git -C "$state/host/control-project" status --porcelain >/dev/null
  operator_env grep -Fxq '# foreign shell content' "$state/shell.rc" || fail 'foreign shell content removed'
  ! operator_env grep -Fq '# Subyard CLI' "$state/shell.rc" || fail 'managed shell blocks remain'
  operator_env env PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin bash -c '. "$1"; command -v foreign-control' _ "$state/shell.rc" | grep -Fxq "$state/bin/foreign-control"
  operator_env grep -Fq "Host $foreign" "$home/.ssh/config" || fail 'foreign SSH content removed'
  ! operator_env grep -Eq '^Include subyard(-[^ ]+)?\.config$' "$home/.ssh/config" || fail 'managed SSH Includes remain'
  if [ -f "$state/legacy-result.log" ]; then
    sudo -n test -d "$state/legacy-host/backups" || fail 'unproven old host backup namespace was removed'
    operator_env grep -Fxq 'preserved old host content' "$state/legacy-host/foreign.control" || fail 'unproven old host content changed'
    ! incus project show "subyard-legacy-$token" >/dev/null 2>&1 || fail 'retired old native project reappeared'
  fi
  printf 'ok: approved artifacts absent; foreign workload, shared pool, user service, host project, SSH and shell controls preserved\n'
}
interrupt_full_removal() {
  local runner="$1" data_root="$2" config_root="$3" launcher="$4" prefix="$5" wrapper caller
  # Watch native config-directory loss and kill the original caller. Its
    # retained engine/adapters must then complete from a fresh process.
    "$runner" bash -c 'printf "%s\n" "$$" > "$1"; chmod 644 "$1"; shift; exec setsid "$@"' _ "$state/${prefix}full.pid" "$launcher" teardown --full --yes > "$state/${prefix}full.log" 2>&1 &
    wrapper=$!
    for _ in {1..100}; do [ -s "$state/${prefix}full.pid" ] && break; sleep 0.1; done
    caller="$(cat "$state/${prefix}full.pid")"
    sudo -n python3 - "$config_root" "$caller" <<'PY'
import ctypes, os, pathlib, select, signal, sys, time
libc=ctypes.CDLL(None,use_errno=True)
fd=libc.inotify_init1(os.O_NONBLOCK|os.O_CLOEXEC)
if fd<0 or libc.inotify_add_watch(fd,sys.argv[1].encode(),0x400|0x800)<0: raise SystemExit(1)
ready=select.select([fd],[],[],120)[0]
if not ready: raise SystemExit("config deletion checkpoint did not occur")
caller=int(sys.argv[2])
processes={}
for path in pathlib.Path("/proc").glob("[0-9]*/stat"):
 try:
  fields=path.read_text().rsplit(")",1)[1].split();processes[int(path.parent.name)]=(int(fields[1]),fields[19])
 except (FileNotFoundError,ProcessLookupError):pass
owned={caller}
while True:
 extra={pid for pid,(parent,birth) in processes.items() if parent in owned}-owned
 if not extra:break
 owned.update(extra)
os.kill(caller,signal.SIGKILL)
os.close(fd)
# A native child may safely retain the approved barrier after its caller dies.
# Wait for that exact recorded process generation before starting fresh retry.
deadline=time.monotonic()+30
while time.monotonic()<deadline:
 live=[]
 for pid in owned-{caller}:
  try:
   fields=pathlib.Path("/proc",str(pid),"stat").read_text().rsplit(")",1)[1].split()
   if fields[0]!="Z" and fields[19]==processes[pid][1]:live.append(pid)
  except FileNotFoundError:pass
 if not live:break
 time.sleep(0.1)
else:raise SystemExit("owned interrupted physical helpers did not settle within 30 seconds")
PY
    wait "$wrapper" 2>/dev/null || true
    [ ! -e "$config_root" ] || fail 'interruption did not remove config'
    for _ in {1..100}; do [ -x "$data_root.uninstall/runtime/bin/yard" ] && break; sleep 0.1; done
    "$runner" "$data_root.uninstall/runtime/bin/yard" _uninstall-resume "$data_root.uninstall" < /dev/null > "$state/${prefix}retry-preview.log" 2>&1 && fail 'unconfirmed retry executed'
    "$runner" "$data_root.uninstall/runtime/bin/yard" _uninstall-resume "$data_root.uninstall" --yes > "$state/${prefix}retry.log" 2>&1

}

# shellcheck source=dev/e2e/full-uninstall-legacy.sh
. "$ROOT/dev/e2e/full-uninstall-legacy.sh"

case "$mode" in
  preflight)
    for tool in incus jq python3 git systemctl ss ssh ssh-keygen timeout setsid runuser findmnt; do
      command -v "$tool" >/dev/null || fail "missing fixture prerequisite: $tool"
    done
    # The complete marker and preservation controls must survive reboot.
    # shellcheck source=dev/e2e/lib-p0-capacity.sh
    . "$ROOT/dev/e2e/lib-p0-capacity.sh"
    p0_capacity_require_persistent_path /var/tmp full-uninstall-state
    incus info >/dev/null
    incus storage show default >/dev/null
    incus network show incusbr0 >/dev/null
    incus image info subyard-e2e-debian-13-cloud-container --project default >/dev/null
    sudo -n test -d /run/systemd/system
    sudo -n ss -H -lnt >/dev/null
    printf 'ok: held leased VM has actual Incus, systemd, cached image and privilege prerequisites\n'
    ;;
  prepare)
    incus info >/dev/null
    [ ! -e "$state" ] && ! id "$operator" >/dev/null 2>&1 || fail 'fixture already exists'
    install -d -m 0700 "$state"
    printf '%s\n' "$marker" > "$state/.marker"
    sudo -n useradd --create-home --shell /bin/bash --groups incus-admin "$operator"
    sudo -n chown -R "$operator:$operator" "$state"
    sudo -n chgrp dev "$state"
    # The installed runtime validates every ancestor: the controller may read
    # this container, but cannot replace operator-owned installation roots.
    sudo -n chmod 0750 "$state"
    sudo -n install -d -o dev -g dev -m 0700 "$state/release"
    sudo -n install -d -o root -g root -m 0755 "$state/host-helper"
    for file in direct-only.log bootstrap-only.log decline.log active-refusal.log active-ssh.log full.log retry-preview.log retry.log phase baseline-pool baseline-network legacy-install.log legacy-context.log legacy-init.log legacy-refusal.log legacy-stage.log legacy-foreign-refusal.log legacy-full.log legacy-retry-preview.log legacy-retry.log legacy-clean-install.log legacy-clean-remove.log legacy-result.log; do
      sudo -n install -o dev -g dev -m 0600 /dev/null "$state/$file"
    done
    incus profile device get default root pool --project default > "$state/baseline-pool"
    incus profile device get default eth0 network --project default > "$state/baseline-network"
    sudo -n chmod 0644 "$state/.marker"
    sudo -n install -o "$operator" -g "$operator" -m 0755 "$ROOT/dev/bootstrap-runtime.sh" "$state/bootstrap.sh"
    printf '%s ALL=(ALL) NOPASSWD: ALL\n' "$operator" | sudo -n tee "/etc/sudoers.d/$marker" >/dev/null
    sudo -n chmod 0440 "/etc/sudoers.d/$marker"
    sudo -n loginctl enable-linger "$operator"
    sudo -n systemctl start "user@$(id -u "$operator").service"
    operator_env incus info >/dev/null
    userctl show-environment >/dev/null
    if [ -f "$ROOT/.subyard-acceptance/candidate.json" ]; then
      cp -a "$ROOT/.subyard-acceptance/release/." "$state/release/"
    else
      bash "$ROOT/dev/package-engine.sh" --output-dir "$state/release" --version "$version" --arch amd64 >/dev/null
    fi
    sudo -n chown -R "$operator:$operator" "$state/release"
    bundle="$state/release/subyard-$version-linux-amd64.tar.gz"
    sudo -n install -o "$operator" -g "$operator" -m 0755 "$ROOT/scripts/install-runtime-release.sh" "$state/installer.sh"
    legacy_full_uninstall
    operator_env env SUBYARD_HOME="$state/direct-data" SUBYARD_CONFIG_HOME="$state/direct-config" YARD_RELEASE_CACHE="$state/direct-cache" \
      bash "$state/installer.sh" --runtime-root "$state/direct-runtime" --bundle "$bundle" --checksum "$bundle.sha256" --manifest "$bundle.manifest.json" --provenance "$bundle.provenance.json" >/dev/null
    operator_env env SUBYARD_HOME="$state/direct-data" SUBYARD_CONFIG_HOME="$state/direct-config" YARD_RUNTIME_ROOT="$state/direct-runtime" YARD_RELEASE_CACHE="$state/direct-cache" \
      "$state/direct-runtime/current/bin/yard" teardown --full --yes > "$state/direct-only.log" 2>&1
    [ ! -e "$state/direct-runtime" ] && [ ! -e "$state/direct-data.uninstall" ] || fail 'direct installer-only uninstall left its runtime/recovery'
    operator_env env SUBYARD_HOME="$state/bootstrap-data" SUBYARD_CONFIG_HOME="$state/bootstrap-config" YARD_RUNTIME_ROOT="$state/bootstrap-runtime" YARD_RELEASE_CACHE="$state/bootstrap-cache" \
      YARD_RELEASE_BASE_URL="file://$state/release" YARD_BIN_DIR="$state/bootstrap-bin" YARD_SHELL_RC="$state/bootstrap.rc" YARD_LOGIN_RC="$state/bootstrap.login" \
      bash "$state/bootstrap.sh" --version "$version" --yes >/dev/null
    operator_env env SUBYARD_HOME="$state/bootstrap-data" SUBYARD_CONFIG_HOME="$state/bootstrap-config" YARD_RUNTIME_ROOT="$state/bootstrap-runtime" YARD_RELEASE_CACHE="$state/bootstrap-cache" \
      "$state/bootstrap-bin/yard" uninstall --full --yes > "$state/bootstrap-only.log" 2>&1
    [ ! -e "$state/bootstrap-runtime" ] && [ ! -e "$state/bootstrap-bin/yard" ] || fail 'bootstrap installer-only uninstall left an entrypoint'
    operator_env bash -c 'printf "# foreign shell content\n" > "$1/shell.rc"; mkdir -p "$1/bin"; printf "#!/bin/sh\nexit 0\n" > "$1/bin/foreign-control"; chmod 755 "$1/bin/foreign-control"' _ "$state"
    operator_env env YARD_RELEASE_BASE_URL="file://$state/release" YARD_BIN_DIR="$state/bin" \
      YARD_SHELL_RC="$state/shell.rc" YARD_LOGIN_RC="$state/login.rc" \
      bash "$state/bootstrap.sh" --version "$version" --yes >/dev/null
    operator_env mkdir -p "$state/config/yards"
    operator_env bash -c 'cat > "$1/config/config.env" <<CONFIG
INCUS_PROJECT=subyard-full-$2
YARD_INSTANCE_NAME=yard
HOST_BASE=$1/host
SUBYARD_POWER_UNIT_PATH=/etc/systemd/system/subyard-full-power-$2.service
SUBYARD_POWER_RECONCILER_PATH=$1/host-helper/power
RESTRICTED_DISK_PATHS=$1/host
SRV_POOL=subyard-full-$2
STORAGE_PATH=$1/storage
INCUS_BRIDGE=syfull${2:0:8}
SSH_HOST=full-$2
SSH_PORT=24222
CODING_TOOL_INTEGRATIONS=
ENVIRONMENT_PROFILES=
FORWARD_SSH_AGENT=0
SHIFT_MODE=acl
DEV_UID=1001
BASE_IMAGE=subyard-e2e-debian-13-cloud-container
BASE_IMAGE_FALLBACK=subyard-e2e-debian-13-cloud-container
CONFIG
chmod 600 "$1/config/config.env"
for pair in neighbor:24223 stopped:24224; do name=${pair%:*}; port=${pair#*:}; printf "SSH_PORT=%s\nINCUS_PROJECT=subyard-full-%s-%s\nYARD_INSTANCE_NAME=yard-%s\nHOST_BASE=%s/host-%s\nRESTRICTED_DISK_PATHS=%s/host-%s\n" "$port" "$2" "$name" "$name" "$1" "$name" "$1" "$name" > "$1/config/yards/$name.env"; chmod 600 "$1/config/yards/$name.env"; done' _ "$state" "$token"
    for project in "subyard-full-$token" "subyard-full-$token-neighbor" "subyard-full-$token-stopped"; do create_fixture_project "$project"; done
    operator_env sha256sum "$bundle" "$state/runtime/current/bin/yard" "$state/runtime/current/bin/yard-engine"
    if yard init --yes; then :; else
      init_status=$?
      diagnostic_status=0
      readiness_report="$(yard migrate --check --json 2>/dev/null)" || diagnostic_status=$?
      projection_status=0
      printf '%s\n' "$readiness_report" | jq -e '
        select(.schemaVersion == 1 and (.outcome | type) == "object") |
        {outcome: (.outcome | {status, code, active, previous, target, transaction, message, retry}),
         domains: [(.domains // [])[] | {domain, epoch, requiredEpoch, pending}],
         decisions: [(.decisions // [])[] | {resource, scope, decision, result}],
         blockers: [(.blockers // [])[] | {code, resource, message, retry}]}
      ' 2>/dev/null || projection_status=$?
      printf 'full-uninstall: original init exit=%s; readiness diagnostic command exit=%s projection exit=%s\n' \
        "$init_status" "$diagnostic_status" "$projection_status" >&2
      exit "$init_status"
    fi
    yard -Y neighbor init --yes
    yard -Y neighbor start --yes
    yard -Y stopped init --yes
    yard -Y stopped stop --yes
    for project in "subyard-full-$token" "subyard-full-$token-neighbor" "subyard-full-$token-stopped"; do incus project set "$project" user.subyard.full-fixture "$marker"; done
    # Recover an actual installation-marked partial project whose registration
    # was lost before the instance could be created.
    owner_context="$(incus project get "subyard-full-$token" user.subyard.context)"
    partial_context="$(python3 - "$owner_context" "$state" "$token" <<'PY_PARTIAL'
import json,sys
value=json.loads(sys.argv[1]);base,token=sys.argv[2:]
value.update(yardName="partial",incusProject="subyard-full-"+token+"-partial",yardInstanceName="yard-partial",sshHost="full-partial-"+token,sshPort=24225)
value["paths"]["hostBase"]=base+"/host-partial"
value["paths"]["stateDir"]=base+"/config/yards/partial/projects"
print(json.dumps(value,separators=(",",":")))
PY_PARTIAL
)"
    incus project create "subyard-full-$token-partial" \
      -c user.subyard.installation=1 -c "user.subyard.operator_uid=$(id -u "$operator")" \
      -c "user.subyard.data_home=$state/data" -c "user.subyard.config_home=$state/config" \
      -c "user.subyard.yard_name=partial" -c "user.subyard.instance_name=yard-partial" \
      -c "user.subyard.host_base=$state/host-partial" -c "user.subyard.bridge=$bridge" \
      -c "user.subyard.srv_pool=$pool" -c "user.subyard.storage_pool=$pool" \
      -c "user.subyard.srv_volume=yard-partial-srv" -c "user.subyard.storage_path=$state/storage" \
      -c "user.subyard.context=$partial_context" -c "user.subyard.full-fixture=$marker" >/dev/null
    # Actual default teardown must retain neighbors, runtime and shared timers.
    assert_project "subyard-full-$token-neighbor"
    neighbor_state="$(incus list yard-neighbor --project "subyard-full-$token-neighbor" -f csv -c ns)" || fail 'cannot inspect neighbor before selected teardown'
    [ "$neighbor_state" = yard-neighbor,RUNNING ] || fail 'neighbor is not running before selected teardown'
    yard teardown --yes
    yard --version >/dev/null
    assert_project "subyard-full-$token-neighbor"
    neighbor_state="$(incus list yard-neighbor --project "subyard-full-$token-neighbor" -f csv -c ns)" || fail 'cannot inspect neighbor after selected teardown'
    [ "$neighbor_state" = yard-neighbor,RUNNING ] || fail 'neighbor is missing or changed state after selected teardown'
    userctl is-active --quiet subyard-keys-sync.timer
    if command -v ufw >/dev/null && [ -f /etc/ufw/user.rules ]; then
      # shellcheck disable=SC2024 # Root reads UFW; the caller owns the fixture output files.
      sudo -n awk -v bridge="$bridge" '$1=="###" && $2=="tuple" && $3=="###" && ($10=="in_" bridge || $10=="out_" bridge)' /etc/ufw/user.rules > "$state/ufw-before"
      # shellcheck disable=SC2024
      sudo -n awk '$1=="###" && $2=="tuple" && $3=="###" && ($10=="in_incusbr0" || $10=="out_incusbr0")' /etc/ufw/user.rules > "$state/ufw-foreign-before"
      chmod 0600 "$state/ufw-before" "$state/ufw-foreign-before"
    fi
    systemctl is-enabled --quiet "$power"
    [ -f "/etc/systemd/system/$power" ] && [ ! -L "/etc/systemd/system/$power" ] || fail 'shared boot power unit is missing'
    [ -f "$state/host-helper/power" ] && [ ! -L "$state/host-helper/power" ] && [ -x "$state/host-helper/power" ] || fail 'shared boot power helper is missing'
    create_fixture_project "subyard-full-$token"
    yard init --yes
    incus project set "subyard-full-$token" user.subyard.full-fixture "$marker"
    operator_env bash -c 'printf synthetic-test-value > "$1/credential.input"; chmod 600 "$1/credential.input"' _ "$state"
    yard keys add full-fixture --kind token --consumer none --file "$state/credential.input" --yes >/dev/null
    assert_fixture
    sudo -n install -d -o "$operator" -g "$operator" -m 0700 "$state/host/control-project"
    sudo -n install -o "$operator" -g "$operator" -m 0600 /dev/null "$state/host/foreign.control"
    operator_env git -C "$state/host/control-project" init -q
    operator_env bash -c 'printf "foreign host content\n" > "$1/host/foreign.control"; printf foreign-project > "$1/host/control-project/README"' _ "$state"
    yard bind "$state/host/control-project" --yes
    # Capture installation-created host namespaces before deleting the ledger.
    # The same exact paths are checked immediately, after the sink tick and reboot.
    operator_env python3 - "$state" <<'PY_HOST_PATHS'
import json,os,pathlib,sys
state=pathlib.Path(sys.argv[1]);approved=set()
for path in (state/"data/ownership/host-paths").glob("*.json"):
 record=json.loads(path.read_bytes())
 if record["schema"]!=1 or record["uid"]!=os.getuid() or record["dataHome"]!=str(state/"data") or record["configHome"]!=str(state/"config"):raise SystemExit("foreign fixture host ownership")
 root=pathlib.Path(record["root"])
 if not root.is_relative_to(state):raise SystemExit("host fixture root escaped marker")
 for entry in record["paths"]:
  if entry["created"]:
   if not entry["identity"] or pathlib.Path(entry["name"]).name!=entry["name"]:raise SystemExit("incomplete fixture host ownership")
   approved.add(str(root/entry["name"]))
if not approved:raise SystemExit("fixture lacks installation-created host namespaces")
fd=os.open(state/"approved-host-paths.json",os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
os.fchmod(fd,0o600)
with os.fdopen(fd,"w") as handle:json.dump(sorted(approved),handle)
PY_HOST_PATHS
    # Exercise a genuine TTL signing worker using an isolated synthetic encrypted
    # key and a controlling terminal. Private key/passphrase bytes are never output.
    operator_env ssh-keygen -q -t ed25519 -N synthetic-fixture-passphrase -f "$state/signing-key"
    operator_env python3 - "$state" <<'PY_AGENT'
import errno,fcntl,json,os,pty,select,signal,sys,termios,time
state=sys.argv[1];master,slave=pty.openpty();pid=os.fork()
if pid==0:
 os.setsid();fcntl.ioctl(slave,termios.TIOCSCTTY,0)
 for target in (0,1,2):os.dup2(slave,target)
 os.close(master);os.close(slave)
 os.execv(state+"/bin/yard",[state+"/bin/yard","ssh-agent","unlock","--key",state+"/signing-key","--ttl","15m","--yes"])
os.close(slave);deadline=time.monotonic()+30;buffer=b""
facts=dict(saw_prompt=False,sent_count=0,child_exit=None,deadline_reached=False,child_reaped=False,outcome=None)
try:
 while True:
  child,status=os.waitpid(pid,os.WNOHANG)
  if child:
   facts.update(child_exit=os.waitstatus_to_exitcode(status),child_reaped=True)
   break
  if time.monotonic()>=deadline:
   facts.update(deadline_reached=True,outcome="deadline")
   break
  if not select.select([master],[],[],0.1)[0]:continue
  try:data=os.read(master,4096)
  except OSError as error:
   if error.errno!=errno.EIO:raise
   data=b""
  if not data:
   # PTY EOF can precede waitpid observing exit; retain the same deadline.
   time.sleep(0.01)
   continue
  buffer=(buffer+data)[-8192:]
  if b"Bad passphrase, try again for " in buffer:
   facts.update(saw_prompt=True,outcome="bad_passphrase_prompt")
   break
  if b"Enter passphrase for " in buffer:
   facts["saw_prompt"]=True
   if facts["sent_count"]:
    facts["outcome"]="repeated_prompt"
    break
   os.write(master,b"synthetic-fixture-passphrase\n")
   facts["sent_count"]+=1;buffer=b""
except OSError:
 facts["outcome"]="native_error"
finally:
 if not facts["child_reaped"]:
  # Until waitpid reaps our child, its PID cannot be reused by another group.
  try:os.killpg(pid,signal.SIGKILL)
  except ProcessLookupError:
   try:os.kill(pid,signal.SIGKILL)
   except ProcessLookupError:pass
  _,status=os.waitpid(pid,0)
  facts.update(child_exit=os.waitstatus_to_exitcode(status),child_reaped=True)
 os.close(master)
if facts["outcome"] is None:
 facts["outcome"]="child_failed" if facts["child_exit"]!=0 else "unlocked" if facts["sent_count"]==1 else "missing_prompt"
print("FULL_UNINSTALL_SIGNING_PTY="+json.dumps(facts,separators=(',',':')))
if facts["outcome"]!="unlocked":
 raise SystemExit("signing grant fixture exceeded 30 seconds" if facts["deadline_reached"] else "signing grant fixture did not unlock")
PY_AGENT
    yard ssh-agent status --json | jq -e '.state == "unlocked"' >/dev/null
    operator_env bash -c 'mkdir -p "$HOME/.ssh"; printf "\nHost %s\n  HostName 127.0.0.1\n  Port 1\n" "$2" >> "$HOME/.ssh/config"; chmod 600 "$HOME/.ssh/config"' _ "$state" "$foreign"
    userctl daemon-reload
    operator_env bash -c 'cat > "$HOME/.config/systemd/user/$1" <<UNIT
[Unit]
Description=Foreign full-uninstall preservation control
[Service]
ExecStart=/bin/sleep infinity
[Install]
WantedBy=default.target
UNIT
chmod 644 "$HOME/.config/systemd/user/$1"' _ "$foreign_unit"
    userctl daemon-reload
    userctl enable --now "$foreign_unit" >/dev/null
    create_fixture_project "$foreign"
    incus profile device add default root disk pool=default path=/ --project "$foreign" >/dev/null
    incus profile device add default eth0 nic network=incusbr0 --project "$foreign" >/dev/null
    incus launch subyard-e2e-debian-13-cloud-container control --project "$foreign" >/dev/null
    incus storage volume create "$pool" "foreign-$token" --project default >/dev/null
    relay_ip="$(incus list yard --project "subyard-full-$token" -f json | jq -er '.[0].state.network.eth0.addresses[] | select(.family=="inet" and .scope=="global") | .address' | head -1)"
    operator_env env SUBYARD_ENGINE_CONTEXT=1 SUBYARD_ENGINE_CONTEXT_SCHEMA=1 SUBYARD_CONFIG_DIR="$state/runtime/current/config" \
      SUBYARD_OPERATOR_HOME="$home" STORAGE_PATH="$state/storage" HOST_BASE="$state/host" RESTRICTED_DISK_PATHS="$state/host" \
      ACCESS_KIND=local YARD_KIND=container YARD_INSTANCE_NAME=yard INCUS_PROJECT="subyard-full-$token" INCUS_BRIDGE="$bridge" \
      SSH_HOST="full-$token" DEV_USER=dev DEV_UID=1000 DEV_SUDO=0 FORWARD_SSH_AGENT=0 NESTED_E2E_VMS=0 \
      bash "$state/runtime/current/scripts/install-ssh-relay.sh" --ensure 24226 "$relay_ip"
    operator_env bash -c 'exec 5<>/dev/tcp/127.0.0.1/24226; IFS= read -r -t 2 greeting <&5; exec 5<&-; exec 5>&-'
    # Establish the sink before a refused plan is saved: retry retains its exact inventory.
    operator_env env SUBYARD_ENGINE_CONTEXT=1 SUBYARD_ENGINE_CONTEXT_SCHEMA=1 SUBYARD_CONFIG_DIR="$state/runtime/current/config" \
      SUBYARD_OPERATOR_HOME="$home" STORAGE_PATH="$state/storage" HOST_BASE="$state/host" RESTRICTED_DISK_PATHS="$state/host" \
      ACCESS_KIND=local YARD_KIND=container YARD_INSTANCE_NAME=yard INCUS_PROJECT="subyard-full-$token" INCUS_BRIDGE="$bridge" \
      SSH_HOST="full-$token" DEV_USER=dev DEV_UID=1000 DEV_SUDO=0 FORWARD_SSH_AGENT=0 NESTED_E2E_VMS=0 \
      SUBYARD_TEST_VMS_SINK_ENGINE_SOURCE="$state/runtime/current/bin/yard-engine" \
      bash "$state/runtime/current/scripts/install-test-vms-host-sink.sh" --yes
    # Retained matching evidence is unreadable to the ordinary owner. The
    # confirmed owning hook proves its source without exposing record contents.
    sudo -n python3 - "$state/data" <<'PY_EVIDENCE'
import json,pathlib,sys
directory=pathlib.Path(sys.argv[1])/"logs/test-vms-broker-incidents"
directory.mkdir(parents=True,exist_ok=True);directory.chmod(0o700)
host=directory/"host";host.mkdir(mode=0o700)
incident="a"*32
records=[(directory/(incident+".json"),{"schema_version":1,"incident_id":incident,"created_at":"2026-10-13T00:00:00Z","source":"default","slot_id":"slot-001","resource_generation":1,"lease_epoch":1,"failure_reason":"owned acceptance fixture","diagnostics":{}}),
 (host/(incident+".json"),{"observed_at":"2026-10-13T00:00:00Z","timing":"after incident","kernel_oom":[]})]
for path,record in records:path.write_text(json.dumps(record)+"\n");path.chmod(0o600)
PY_EVIDENCE
    sudo -n python3 - "$state" "$(id -u "$operator")" "$power" "$sink.service" "$relay.service" <<'PY_CAPTURE'
import hashlib,json,os,pathlib,subprocess,sys
state=sys.argv[1];records={}
directories=["/tmp/subyard-ssh-"+sys.argv[2]+"/"+hashlib.sha256((os.path.normpath(state+"/data")+"\0"+yard).encode()).hexdigest()[:24] for yard in ("default","neighbor","stopped","partial")]
for unit in sys.argv[3:]:
 for pid in subprocess.check_output(["systemctl","show",unit,"--property=MainPID,ControlPID","--value"],text=True).splitlines():
  if pid.isdigit() and pid!="0":
   try:records[pid]=pathlib.Path("/proc",pid,"stat").read_text().rsplit(")",1)[1].split()[19]
   except FileNotFoundError:pass
for path in pathlib.Path("/proc").glob("[0-9]*/cmdline"):
 try:
  arguments=path.read_bytes().split(b"\0")
  if any(item.startswith((state+"/host-helper/").encode()) for item in arguments) or any(item == directory.encode() or item.startswith((directory+"/").encode()) for item in arguments for directory in directories):records[path.parent.name]=(path.parent/"stat").read_text().rsplit(")",1)[1].split()[19]
 except (FileNotFoundError,ProcessLookupError):pass
target=pathlib.Path(state)/"producer-pids.json";target.write_text(json.dumps({"pids":records,"agent_directories":directories})+"\n");target.chmod(0o600)
PY_CAPTURE
    # Read-only assessment and EOF cannot retire an approved live signing grant.
    if yard teardown --full < /dev/null > "$state/decline.log" 2>&1; then fail 'full uninstall accepted EOF'; fi
    yard ssh-agent status --json | jq -e '.state == "unlocked"' >/dev/null
    [ ! -e "$state/data.uninstall" ] || fail 'declined assessment created recovery state'
    # Installer-generated relay/session state is exercised by an actual live SSH connection.
    operator_env bash -c 'printf "%s\n" "$$" > "$1/ssh.pid"; chmod 644 "$1/ssh.pid"; exec ssh -F "$HOME/.ssh/config" -o BatchMode=yes -T "$2" -- sleep 300' _ "$state" "full-$token" > "$state/active-ssh.log" 2>&1 &
    session_pid=$!
    sleep 2
    kill -0 "$session_pid" || fail 'real SSH preservation control did not connect'
    if yard teardown --full --yes > "$state/active-refusal.log" 2>&1; then fail 'active real SSH session did not refuse full deletion'; fi
    grep -Fq 'SSH session is still connected' "$state/active-refusal.log" || fail 'full refusal did not come from the SSH guard'
    incus project show "subyard-full-$token" >/dev/null
    sudo -n kill "$(cat "$state/ssh.pid")"; wait "$session_pid" 2>/dev/null || true
    printf 'prepared\n' > "$state/phase"
    ;;
  apply)
    assert_fixture
    interrupt_full_removal operator_env "$state/data" "$state/config" "$state/bin/yard" ''
    verify_absence
    printf 'applied\n' > "$state/phase"
    ;;
  verify)
    verify_absence
    printf '  [ .. ] waiting 65 seconds for a complete retired sink tick\n'
    sleep 65
    verify_absence
    printf 'verified\n' > "$state/phase"
    ;;
  reboot-verify) verify_absence; printf 'reboot-verified\n' > "$state/phase" ;;
  cleanup)
    assert_fixture
    # The held broker lease is disposable; only marked fixture controls are cleaned.
    assert_project "$foreign"
    incus delete control --project "$foreign" --force
    incus profile device remove default root --project "$foreign" >/dev/null
    incus profile device remove default eth0 --project "$foreign" >/dev/null
    incus project delete "$foreign" >/dev/null
    incus storage volume delete "$pool" "foreign-$token" --project default >/dev/null
    userctl disable --now "$foreign_unit" >/dev/null
    restore_fixture_default_profile
    uid="$(id -u "$operator")"
    if incus storage show "$pool" >/dev/null 2>&1; then
      [ "$(incus storage get "$pool" source)" = "$state/storage" ] \
        && [ "$(incus storage get "$pool" user.subyard.operator_uid)" = "$uid" ] \
        && [ "$(incus storage get "$pool" user.subyard.data_home)" = "$state/data" ] || fail 'unproven fixture pool'
      incus storage delete "$pool" >/dev/null
    fi
    if incus network show "$bridge" >/dev/null 2>&1; then
      [ "$(incus network get "$bridge" user.subyard.operator_uid)" = "$uid" ] \
        && [ "$(incus network get "$bridge" user.subyard.data_home)" = "$state/data" ] || fail 'unproven fixture network'
      incus network delete "$bridge" >/dev/null
    fi
    sudo -n loginctl disable-linger "$operator"
    sudo -n systemctl stop "user@$uid.service"
    sudo -n userdel -r "$operator" >/dev/null
    sudo -n rm -f -- "/etc/sudoers.d/$marker"
    sudo -n rm -rf -- "$state"
    ;;
  *) fail 'use preflight, prepare, apply, verify, reboot-verify or cleanup' ;;
esac
