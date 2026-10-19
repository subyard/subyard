#!/usr/bin/env bash
# Run through the existing agent controller on one disposable VM. The installed
# candidate owns ordinary creation/provisioning; only regression seeding is manual.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
RUN_ID="${SUBYARD_E2E_RUN_ID:?run through dev/agent-e2e.sh}"
[ "${SUBYARD_E2E_VM:-}" = 1 ] || exit 2
[ -f "$ROOT/.subyard-acceptance/candidate.json" ] \
  || { printf 'disk-tmp-acceptance: freeze and pass the current packaged candidate before allocation\n' >&2; exit 2; }
STATE="$(mktemp -d /var/tmp/subyard-disk-tmp.XXXXXX)"
printf '%s\n' "$RUN_ID" > "$STATE/.fixture-run"
NAMES=()
launcher=''
die() { printf 'disk-tmp-acceptance: %s\n' "$*" >&2; exit 1; }
incus() { sudo -n /usr/bin/incus "$@"; }
native_owner() {
  local command
  # sg executes /bin/sh, including when an argument contains a multiline Bash
  # program. Use POSIX quoting instead of Bash-only printf %q escapes.
  command=$(python3 -c 'import shlex, sys; print(shlex.join(sys.argv[1:]))' "$@") || return
  sg incus-admin -c "exec $command"
}
yard() { native_owner "$launcher" -Y "$name" "$@"; }
install_candidate() {
  # Platform preparation refreshes group membership only in its child process.
  # Probe activation's native observer in the installer's fresh group context,
  # after any platform daemon restart has replaced its socket.
  native_owner bash -c '
    set -euo pipefail
    . "$1/tests/helpers/release-candidate.sh"
    engine=$(release_candidate_prepare "$1")
    "$engine" config status --all-local >/dev/null
    printf "evidence: native_owner_config_preflight=PASS\n" >&2
    release_candidate_install "$1" "$2"
  ' _ "$ROOT" "$STATE/candidate"
}
write_yard_config() {
  # Bare aliases cached in project default are invisible in a newly isolated
  # yard project. Let normal creation resolve its own supported remote image.
  # Debian cloud images reserve UID 1000; use the existing P0 fixture identity.
  fixture_dev_user=dev fixture_dev_uid=1001
  cat > "$SUBYARD_CONFIG_HOME/yards/$name/config.env" <<EOF
SSH_PORT=$port
YARD_KIND=$kind
YARD_IMAGE=images:debian/13/cloud
YARD_IMAGE_FALLBACK=images:debian/13/cloud
CODING_TOOL_INTEGRATIONS=
ENVIRONMENT_PROFILES=
DEV_USER=$fixture_dev_user
DEV_UID=$fixture_dev_uid
HOST_BASE=$STATE/host-$kind
RESTRICTED_DISK_PATHS=$STATE/host-$kind
FORWARD_SSH_AGENT=0
LIMITS_CPU=1
LIMITS_MEMORY=1GiB
EOF
  chmod 0600 "$SUBYARD_CONFIG_HOME/yards/$name/config.env"
}
cleanup() {
  local original=$? cleanup_exit=0 name rc
  trap - EXIT
  set +e
  if [ -n "$launcher" ]; then
    # Retain the shared platform baseline used by the allocation's other fixtures.
    install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/platform-sentinel"
    printf 'SSH_PORT=64998\n' > "$SUBYARD_CONFIG_HOME/yards/platform-sentinel/config.env"
    chmod 0600 "$SUBYARD_CONFIG_HOME/yards/platform-sentinel/config.env"
    for name in "${NAMES[@]}"; do
      if [ "$original" != 0 ]; then
        # Fixed properties only: retain the failed guest's boot/mount boundary
        # before teardown, without config, environment, journal or user content.
        printf 'disk-tmp-acceptance: failed_guest=%s boot_mount_diagnostics\n' "$name"
        timeout --kill-after=2 10 sudo -n /usr/bin/incus exec "yard-$name" --project "subyard-$name" -- sh -c '
          systemctl show -p SystemState
          systemctl show tmp.mount -p LoadState -p ActiveState -p SubState -p Result
          findmnt -n -T /tmp -o FSTYPE,SOURCE,OPTIONS
        ' || printf 'disk-tmp-acceptance: boot_mount_diagnostics_exit=%s\n' "$?"
      fi
      rc=0
      yard teardown --yes || rc=$?
      if [ "$rc" != 0 ]; then
        printf 'disk-tmp-acceptance: cleanup_yard=%s teardown_exit=%s\n' "$name" "$rc"
        cleanup_exit=3
      fi
      if incus project show "subyard-$name" >/dev/null 2>&1; then
        printf 'disk-tmp-acceptance: cleanup_yard=%s project_still_present\n' "$name"
        cleanup_exit=3
      fi
    done
  fi
  if [ "$cleanup_exit" = 0 ] && [ ! -L "$STATE" ] \
    && [ "$(cat "$STATE/.fixture-run" 2>/dev/null)" = "$RUN_ID" ]; then
    if sudo -n find "$STATE" -depth -delete; then :; else
      printf 'disk-tmp-acceptance: cleanup_state_removal_exit=%s\n' "$?"
      cleanup_exit=3
    fi
  fi
  printf 'disk-tmp-acceptance: original_exit=%s cleanup_exit=%s\n' "$original" "$cleanup_exit"
  [ "$original" != 0 ] || original=$cleanup_exit
  exit "$original"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

for command in go incus jq sudo sg python3; do command -v "$command" >/dev/null || die "$command is required"; done
sudo -n true || die 'disposable VM requires passwordless root helpers'
test -c /dev/kvm || die 'nested VM acceptance requires /dev/kvm'
free_bytes=$(df -B1 --output=avail /var/tmp | tail -n 1)
[ "$free_bytes" -ge $((10 * 1024 * 1024 * 1024)) ] || die 'need 10 GiB free for sequential container/VM fixtures'

# Reuse the owning platform reconciler and image preflight; no second installer recipe.
bash "$ROOT/dev/e2e/p0-guest.sh" fixture-platform "$(date +%s)"
export SUBYARD_OPERATOR_HOME="$STATE/home" SUBYARD_CONFIG_HOME="$STATE/config" SUBYARD_HOME="$STATE/data"
export YARD_RUNTIME_ROOT="$STATE/candidate/runtime" SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1 MIN_DISK_GIB=1
export STORAGE_PATH
STORAGE_PATH="$(incus storage get default source)"
install -d -m 0700 "$SUBYARD_OPERATOR_HOME" "$SUBYARD_CONFIG_HOME" "$SUBYARD_HOME"
launcher="$(install_candidate)"
printf 'evidence: installed_candidate=%s\n' "$("$launcher" --version)"
sha256sum "$YARD_RUNTIME_ROOT/current/scripts/guest-disk-tmp.sh"

guest() { incus exec "$instance" --project "$project" -- "$@"; }
boot_id() { guest cat /proc/sys/kernel/random/boot_id; }
restart_identity() {
  if [ "$kind" = vm ]; then
    boot_id
  else
    incus query "/1.0/instances/$instance/state?project=$project" | jq -er '.pid | select(. > 0)'
  fi
}
disk_ready() {
  guest sh -s -- check < "$YARD_RUNTIME_ROOT/current/scripts/guest-disk-tmp.sh"
  [ "$(guest id -u "$fixture_dev_user")" = "$fixture_dev_uid" ] || die 'guest user does not match the configured fixture identity'
  guest runuser -u "$fixture_dev_user" -- sh -eu -c 'p=$(mktemp /tmp/subyard-disk-write.XXXXXX); rm "$p"'
  guest findmnt -n -T /tmp -o FSTYPE,SOURCE,OPTIONS
}
restart_yard() {
  local before
  before="$(restart_identity)"
  yard stop --yes
  yard start --yes
  [ "$(restart_identity)" != "$before" ] || die 'native restart did not change the guest restart identity'
  guest sh -s -- wait-boot < "$YARD_RUNTIME_ROOT/current/scripts/guest-disk-tmp.sh"
}
probe_holder() {
  guest python3 -c '
import socket,sys
s=socket.socket(socket.AF_UNIX);s.settimeout(5);s.connect(sys.argv[1]+"/socket");s.sendall(b"probe")
assert s.recv(128)==b"named:unlinked"
' "$holder"
}

for kind in container vm; do
  suffix="${STATE##*.}"
  name="tmp-$kind-${suffix,,}"
  project="subyard-$name" instance="yard-$name"
  ! incus project show "$project" >/dev/null 2>&1 || die 'fixture project already exists'
  install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/$name"
  port=$((34000 + $$ % 10000))
  [ "$kind" != vm ] || port=$((port + 1))
  write_yard_config
  NAMES+=("$name")
  yard init --yes
  yard start --yes
  disk_ready
  printf 'evidence: %s normal_minimal_creation=PASS\n' "$kind"

  # Roll back only the policy on the exact disposable yard. Restore the native
  # image's tmpfs unit for a genuine old-yard transition on its next boot.
  wants=/etc/systemd/system/local-fs.target.wants/tmp.mount
  wants_present=0
  if guest sh -c '[ -e "$1" ] || [ -L "$1" ]' _ "$wants"; then wants_present=1; fi
  guest sh -eu -c '
    unit=/usr/lib/systemd/system/tmp.mount
    wants=$1
    [ -f "$unit" ] && [ ! -L "$unit" ] \
      && [ "$(stat -c "%u:%g" "$unit")" = 0:0 ] \
      || { echo "disk-tmp fixture requires the native tmp.mount unit" >&2; exit 1; }
    if [ -e "$wants" ] || [ -L "$wants" ]; then
      [ -L "$wants" ] && [ "$(stat -c "%u:%g" "$wants")" = 0:0 ] \
        && [ "$(readlink -f "$wants")" = "$(readlink -f "$unit")" ] \
        || { echo "disk-tmp fixture refuses foreign tmp.mount boot policy" >&2; exit 1; }
    fi
    [ "$(readlink /etc/systemd/system/tmp.mount)" = /dev/null ]
    systemctl unmask tmp.mount
    systemctl add-wants local-fs.target tmp.mount
  ' _ "$wants"
  restart_yard
  [ "$(guest findmnt -n -T /tmp -o FSTYPE)" = tmpfs ] || die 'rollback did not restore native tmpfs'
  if [ "$wants_present" = 0 ]; then
    # Remove only the exact link this fixture added. This changes next-boot
    # dependencies without touching the running tmpfs or its native unit.
    guest sh -eu -c '
      [ -L "$1" ] && [ "$(stat -c "%u:%g" "$1")" = 0:0 ]
      [ "$(readlink -f "$1")" = "$(readlink -f /usr/lib/systemd/system/tmp.mount)" ]
      rm -- "$1"
      systemctl daemon-reload
    ' _ "$wants"
  fi
  printf 'evidence: %s native_policy_rollback=PASS\n' "$kind"
  holder="/tmp/subyard-disk-tmp-$RUN_ID"
  guest systemd-run --quiet --unit=subyard-disk-tmp-fixture -- python3 -c '
import os,socket,sys
p=sys.argv[1];os.mkdir(p,0o700)
with open(p+"/named","wb") as f:f.write(b"named")
f=open(p+"/unlinked","w+b");f.write(b"unlinked");f.flush();os.unlink(p+"/unlinked")
s=socket.socket(socket.AF_UNIX);s.bind(p+"/socket");s.listen()
while True:
 c,_=s.accept();c.recv(128);f.seek(0)
 with open(p+"/named","rb") as n:c.sendall(n.read()+b":"+f.read())
 c.close()
' "$holder"
  for _ in {1..30}; do guest test ! -S "$holder/socket" || break; sleep 1; done
  probe_holder
  before="$(restart_identity)"
  output="$STATE/repeat-$kind.log"
  rc=0
  yard init --yes > "$output" 2>&1 || rc=$?
  cat "$output"
  [ "$rc" != 0 ] || die 'active tmpfs was incorrectly reported ready'
  grep -Fq 'the active /tmp is memory-backed' "$output" || die 'reboot diagnostic was lost'
  [ "$(restart_identity)" = "$before" ] || die 'repeat provisioning restarted the active yard'
  [ "$(guest findmnt -n -T /tmp -o FSTYPE)" = tmpfs ] || die 'repeat provisioning unmounted active tmpfs'
  probe_holder
  guest systemctl stop subyard-disk-tmp-fixture
  restart_yard
  disk_ready
  yard init --yes
  disk_ready
  printf 'evidence: %s safe_repeat_reboot_idempotence=PASS\n' "$kind"
  yard teardown --yes
  ! incus project show "$project" >/dev/null 2>&1 || die 'native teardown left fixture project'
  unset 'NAMES[-1]'
done
