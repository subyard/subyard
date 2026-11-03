#!/usr/bin/env bash
# Run through the existing agent controller on one disposable VM. The installed
# candidate owns ordinary creation/provisioning; only regression seeding is manual.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cleanup_mode=container
scenarios=(named default)
usage() { printf 'Usage: cleanup-acceptance.sh [--vm-recovery]\n'; }
[ "$#" -le 1 ] || { usage >&2; exit 2; }
case "${1:-}" in
  '') ;;
  --vm-recovery) cleanup_mode=vm; scenarios=(named) ;;
  --help) usage; exit 0 ;;
  *) usage >&2; exit 2 ;;
esac
RUN_ID="${SUBYARD_E2E_RUN_ID:?run through dev/agent-e2e.sh}"
[ "${SUBYARD_E2E_VM:-}" = 1 ] || exit 2
[ -f "$ROOT/.subyard-acceptance/candidate.json" ] \
  || { printf 'cleanup-acceptance: freeze and pass the current packaged candidate before allocation\n' >&2; exit 2; }
STATE="$(mktemp -d /var/tmp/subyard-cleanup.XXXXXX)"
printf '%s\n' "$RUN_ID" > "$STATE/.fixture-run"
NAMES=()
launcher=''
die() { printf 'cleanup-acceptance: %s\n' "$*" >&2; exit 1; }
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
  if [ "$kind" = vm ]; then
    printf 'SRV_VOLUME_TYPE=block\nSRV_VOLUME_SIZE=4GiB\n' >> "$SUBYARD_CONFIG_HOME/yards/$name/config.env"
  fi
  chmod 0600 "$SUBYARD_CONFIG_HOME/yards/$name/config.env"
}
cleanup() {
  local original=$? cleanup_exit=0 name rc project instance
  trap - EXIT
  set +e
  if [ -n "$launcher" ]; then
    # Retain the shared platform baseline used by the allocation's other fixtures.
    install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/platform-sentinel"
    printf 'SSH_PORT=64998\n' > "$SUBYARD_CONFIG_HOME/yards/platform-sentinel/config.env"
    chmod 0600 "$SUBYARD_CONFIG_HOME/yards/platform-sentinel/config.env"
    for name in "${NAMES[@]}"; do
      native_names "$name"
      rc=0
      yard teardown --yes || rc=$?
      if [ "$rc" != 0 ]; then
        printf 'cleanup-acceptance: cleanup_yard=%s teardown_exit=%s\n' "$name" "$rc"
        cleanup_exit=3
      fi
      if incus project show "$project" >/dev/null 2>&1; then
        printf 'cleanup-acceptance: cleanup_yard=%s project_still_present\n' "$name"
        cleanup_exit=3
      fi
    done
  fi
  if [ "$cleanup_exit" = 0 ] && [ ! -L "$STATE" ] \
    && [ "$(cat "$STATE/.fixture-run" 2>/dev/null)" = "$RUN_ID" ]; then
    if sudo -n find "$STATE" -depth -delete; then :; else
      printf 'cleanup-acceptance: cleanup_state_removal_exit=%s\n' "$?"
      cleanup_exit=3
    fi
  fi
  printf 'cleanup-acceptance: original_exit=%s cleanup_exit=%s\n' "$original" "$cleanup_exit"
  [ "$original" != 0 ] || original=$cleanup_exit
  exit "$original"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

for command in incus jq sudo sg python3; do command -v "$command" >/dev/null || die "$command is required"; done
sudo -n true || die 'disposable VM requires passwordless root helpers'
if [ "$cleanup_mode" = vm ]; then
  test -c /dev/kvm || die 'nested VM recovery acceptance requires /dev/kvm'
fi
free_bytes=$(df -B1 --output=avail /var/tmp | tail -n 1)
[ "$free_bytes" -ge $((10 * 1024 * 1024 * 1024)) ] || die 'need 10 GiB free for independent data backups'

# Reuse the owning platform reconciler and image preflight; no second installer recipe.
bash "$ROOT/dev/e2e/p0-guest.sh" fixture-platform "$(date +%s)"
export SUBYARD_OPERATOR_HOME="$STATE/home" SUBYARD_CONFIG_HOME="$STATE/config" SUBYARD_HOME="$STATE/data"
export YARD_RUNTIME_ROOT="$STATE/candidate/runtime" SUBYARD_KEYS_SYSTEMD_SKIP_ENABLE=1 MIN_DISK_GIB=1
export STORAGE_PATH
STORAGE_PATH="$(incus storage get default source)"
install -d -m 0700 "$SUBYARD_OPERATOR_HOME" "$SUBYARD_CONFIG_HOME" "$SUBYARD_HOME"
launcher="$(install_candidate)"
printf 'evidence: installed_candidate=%s\n' "$("$launcher" --version)"

guest() { incus exec "$instance" --project "$project" -- "$@"; }
native_names() {
  project="subyard-$1" instance="yard-$1" state_dir="$SUBYARD_CONFIG_HOME/yards/$1/projects"
  [ "$1" != default ] || { project=subyard; instance=yard; state_dir="$SUBYARD_CONFIG_HOME/projects"; }
}
project_oracle() { guest tar --sort=name --numeric-owner --mtime=@0 --format=gnu -C /srv/workspaces -cf - . | sha256sum | cut -d' ' -f1; }
registry_oracle() { tar --sort=name --numeric-owner --mtime=@0 --format=gnu -C "$state_dir" -cf - . | sha256sum | cut -d' ' -f1; }
neighbor_oracle() {
  incus query "/1.0/instances/$neighbor_instance?project=$neighbor_project" | jq -Sc '{config,devices,profiles}' | sha256sum
  incus query "/1.0/instances/$neighbor_instance/state?project=$neighbor_project" | jq -Sc '{status,pid}' | sha256sum
}
interrupt_cleanup() {
  local wanted=$1 command process phase interrupted=0 result=0
  command=$(python3 -c 'import shlex,sys; print(shlex.join(sys.argv[1:]))' "$launcher" -Y "$name" cleanup --hard --yes)
  setsid sg incus-admin -c "exec $command" > "$STATE/interrupted-$name-$wanted.log" 2>&1 &
  process=$!
  for ((attempt=0;attempt<6000;attempt++)); do
    phase=$(python3 - "$SUBYARD_HOME" <<'PY'
import glob,json,sys
paths=glob.glob(sys.argv[1]+'/cleanup/*/journal.json')
try: print(json.load(open(paths[0])).get('Phase','') if paths else '')
except (OSError,ValueError): print('')
PY
)
    if [ "$phase" = "$wanted" ]; then kill -TERM -- "-$process"; interrupted=1; break; fi
    kill -0 "$process" 2>/dev/null || break
    sleep .05
  done
  [ "$interrupted" = 1 ] || kill -TERM -- "-$process" 2>/dev/null || true
  wait "$process" || result=$?
  if [ "$interrupted" != 1 ] || [ "$result" = 0 ]; then cat "$STATE/interrupted-$name-$wanted.log"; die "missing native interruption at $wanted"; fi
  # The backup must be a separate native volume outside selected teardown.
  backup=$(python3 - "$SUBYARD_HOME" <<'PY'
import glob,json,sys
j=json.load(open(glob.glob(sys.argv[1]+'/cleanup/*/journal.json')[0]))
assert j['Native']['BackupVolume'] != j['Native']['Volume']
print(j['Native']['BackupVolume'])
PY
)
  incus storage volume show "$srv_pool" "$backup" --project default >/dev/null
  yard cleanup --hard --yes
  printf 'evidence: fresh_process_recovery_after_%s=PASS\n' "$wanted"
}

for scenario in "${scenarios[@]}"; do
  name="cleanup-${STATE##*.}"; name="${name,,}"
  [ "$scenario" != default ] || name=default
  kind="$cleanup_mode"; port=$((33000 + $$ % 10000))
  if [ "$scenario" = default ]; then port=$((port + 1)); neighbor_before="$(neighbor_oracle)"; fi
  install -d -m 0700 "$SUBYARD_CONFIG_HOME/yards/$name"
  write_yard_config
  PROJECT_ROOT="$(mktemp -d /tmp/subyard-cleanup-projects.XXXXXX)"
  printf '%s\n' "$RUN_ID" > "$PROJECT_ROOT/.fixture-run"
  printf 'HOST_BASE=%s\nRESTRICTED_DISK_PATHS=%s\n' "$PROJECT_ROOT" "$PROJECT_ROOT" >> "$SUBYARD_CONFIG_HOME/yards/$name/config.env"
  NAMES+=("$name")
  yard init --yes
  yard start --yes
  native_names "$name"
  source="$PROJECT_ROOT/copy"; bindsource="$PROJECT_ROOT/bind"
  for directory in "$source" "$bindsource"; do
    install -d -m 0755 "$directory"
    git -C "$directory" init -q
    printf 'baseline\n' > "$directory/tracked"
    git -C "$directory" add tracked
    git -C "$directory" -c user.name=Fixture -c user.email=fixture@invalid commit -qm baseline
    printf 'dirty\n' > "$directory/tracked"
    printf 'untracked private\n' > "$directory/untracked"
    chmod 0600 "$directory/untracked"
    ln -s tracked "$directory/link"
  done
  sudo -n chown -R "$fixture_dev_uid:$fixture_dev_uid" "$bindsource"
  yard sync "$source" --name alpha --yes
  yard bind "$bindsource" --name beta --yes
  guest sh -eu -c 'printf retained >/tmp/cleanup-sentinel; printf undeclared >/srv/cache/cleanup-sentinel; printf rootfs >/home/dev/cleanup-sentinel'
  original_uuid="$(incus config get "$instance" volatile.uuid --project "$project")"
  project_before="$(project_oracle)"; registry_before="$(registry_oracle)"
  config_before=$(sha256sum "$SUBYARD_CONFIG_HOME/yards/$name/config.env")
  srv_pool=$(incus config device get "$instance" srv pool --project "$project")
  disk_before=$(guest findmnt -n -o UUID /srv)
  yard cleanup --soft --yes
  guest test -f /tmp/cleanup-sentinel
  guest test -f /srv/cache/cleanup-sentinel
  if [ "$scenario" = named ] && [ "$cleanup_mode" = container ] && [ -n "${SUBYARD_E2E_CLEANUP_CACHE_HELPER:-}" ]; then
    # Optional profile-owned physical assertions; core has no profile branches.
    # shellcheck source=/dev/null
    source "$ROOT/$SUBYARD_E2E_CLEANUP_CACHE_HELPER"
  fi
  [ "$(project_oracle)" = "$project_before" ] || die 'soft cleanup changed projects'
  [ "$(incus config get "$instance" volatile.uuid --project "$project")" = "$original_uuid" ] || die 'soft cleanup recreated yard'
  config_before=$(sha256sum "$SUBYARD_CONFIG_HOME/yards/$name/config.env")
  if [ "$scenario" = default ]; then yard stop --yes; fi
  # Deliberately break only selected native migration metadata. Hard cleanup
  # must use native ownership/reset guards without requiring old convergence.
  incus config set "$instance" user.subyard.disk_tmp=incompatible --project "$project"
  if [ "$scenario" = named ]; then interrupt_cleanup reset; else yard cleanup --hard --yes; fi
  if [ "$scenario" = default ]; then
    [ "$(incus list "$instance" --project "$project" -f csv -c s)" = STOPPED ] || die 'stopped power intent lost'
    yard start --yes
  fi
  [ "$(project_oracle)" = "$project_before" ] || die 'hard cleanup changed project bytes/modes/links'
  [ "$(registry_oracle)" = "$registry_before" ] || die 'hard cleanup changed registrations/reservations'
  [ "$(sha256sum "$SUBYARD_CONFIG_HOME/yards/$name/config.env")" = "$config_before" ] || die 'known configuration changed'
  [ "$(incus config get "$instance" volatile.uuid --project "$project")" != "$original_uuid" ] || die 'rootfs was not recreated'
  if [ "$cleanup_mode" = vm ]; then [ "$(guest findmnt -n -o UUID /srv)" != "$disk_before" ] || die 'old project volume reused'; fi
  guest test ! -e /home/dev/cleanup-sentinel
  guest test ! -e /srv/cache/cleanup-sentinel
  if [ "$scenario" = named ]; then
    interrupt_cleanup recreate
    [ "$(project_oracle)" = "$project_before" ] || die 'post-recreation retry changed projects'
    [ "$(registry_oracle)" = "$registry_before" ] || die 'post-recreation retry changed registrations'
    neighbor_project="$project"; neighbor_instance="$instance"
  else
    [ "$(neighbor_oracle)" = "$neighbor_before" ] || die 'selected cleanup changed neighbor'
  fi
  printf 'evidence: %s_%s_project_bind_registry_config_power_preservation=PASS\n' "$cleanup_mode" "$scenario"
done
