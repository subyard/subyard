#!/usr/bin/env bash
# Published pre-policy yards: a stopped published update, its replacement with one upfront
# confirmation, approved session interruption and marker-only retry.
set -Eeuo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
TOKEN="${1:?fixture token required}"
set -- run "$TOKEN"
# shellcheck source=dev/e2e/power-reconciler-upgrade.sh
. "$ROOT/dev/e2e/power-reconciler-upgrade.sh"
NAMES=(default weighted z-last)
project_for() { if [ "$1" = default ]; then printf subyard; else printf 'subyard-%s' "$1"; fi; }
instance_for() { if [ "$1" = default ]; then printf yard; else printf 'yard-%s' "$1"; fi; }
yard_guest() { local name="$1"; shift; incus exec "$(instance_for "$name")" --project "$(project_for "$name")" -- "$@"; }
yard_boot() { yard_guest "$1" cat /proc/sys/kernel/random/boot_id; }
yard_uuid() { incus config get "$(instance_for "$1")" volatile.uuid --project "$(project_for "$1")"; }
marker() { incus config get "$(instance_for "$1")" user.subyard.disk_tmp --project "$(project_for "$1")"; }
update_candidate() { operator_env env YARD_RELEASE_VERSION="$CANDIDATE_VERSION" YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" "$OPERATOR_HOME/.local/bin/yard" update "$@"; }
maintenance_cleanup() {
  local original=$? failed=0 name
  trap - EXIT ERR
  for name in weighted z-last; do
    PROJECT="$(project_for "$name")" INSTANCE="$(instance_for "$name")"
    if project_presence; then
      assert_project_owned
      operator_yard -Y "$name" teardown --yes > "$STATE_ROOT/cleanup-$name.log" 2>&1 || { tail -c 16384 "$STATE_ROOT/cleanup-$name.log" >&2; failed=1; }
    fi
    delete_owned_project || failed=1
  done
  PROJECT=subyard INSTANCE=yard
  [ "$failed" = 0 ] || original=3
  if (exit "$original"); then cleanup; else cleanup; fi
}
trap maintenance_cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
protected_state() {
  operator_env python3 - "$OPERATOR_HOME/.config/subyard/release-transition" "$OPERATOR_HOME/.subyard/runtime" <<'PY'
import hashlib,json,pathlib,sys
root,runtime=map(pathlib.Path,sys.argv[1:])
paths=[root]+list(root.rglob('*'))+[runtime/'current',runtime/'previous']
print(json.dumps([(str(p),p.lstat().st_ino,p.lstat().st_mode,p.lstat().st_mtime_ns,
                  str(p.readlink()) if p.is_symlink() else hashlib.sha256(p.read_bytes()).hexdigest() if p.is_file() else '')
                 for p in sorted(paths) if p.exists() or p.is_symlink()]))
PY
}
prepare_fixture
install -d "$RELEASE_ROOT"
cp -a "$ROOT/.subyard-acceptance/release/." "$RELEASE_ROOT/"
chmod -R a+rX "$RELEASE_ROOT"
operator_env bash -c 'printf "%s\n" "AGENTS=none" "CODING_TOOL_INTEGRATIONS=" "ENVIRONMENT_PROFILES=" "YARD_IMAGE=subyard-e2e-debian-13-cloud-container" "YARD_IMAGE_FALLBACK=subyard-e2e-debian-13-cloud-container" "DEV_UID=2000" > "$HOME/.config/subyard/config.env"; chmod 0600 "$HOME/.config/subyard/config.env"'
read -r creation installer_sha archive_sha < <(awk -F '\t' '$1=="0.21.3"' "$ROOT/dev/e2e/published-upgrade-baselines.tsv")
[ "$creation" = 0.21.3 ] || die 'creation baseline missing'
curl -fsSL --proto '=https' --tlsv1.2 "https://github.com/Subyard/Subyard/releases/download/v$creation/subyard-install.sh" -o "$INSTALLER"
[ "$(sha256sum "$INSTALLER" | awk '{print $1}')" = "$installer_sha" ] || die 'creation installer changed'
operator_env env YARD_RELEASE_VERSION="$creation" bash "$INSTALLER" --version "$creation" --yes
[ "$(operator_env sha256sum "$OPERATOR_HOME/.subyard/releases/$creation/subyard-$creation-linux-amd64.tar.gz" | awk '{print $1}')" = "$archive_sha" ] || die 'creation archive changed'
incus image info subyard-e2e-debian-13-cloud-vm --project default >/dev/null 2>&1 \
  || incus image copy images:debian/13/cloud local: --vm --alias subyard-e2e-debian-13-cloud-vm --project default
declare -A UUID=() BOOT=()
printf '%s\n' "$MARKER" > "$PROJECT_MUTATION_ARMED"
for name in "${NAMES[@]}"; do
  if [ "$name" != default ]; then
    operator_env install -d -m 0700 "$OPERATOR_HOME/.config/subyard/yards/$name"
    port=2243
    [ "$name" != weighted ] || port=2242
    # Both VM yards predate the VM start policy; the stopped published update leaves z-last untouched.
    settings="SSH_PORT=$port"$'\nYARD_KIND=vm\nYARD_IMAGE=subyard-e2e-debian-13-cloud-vm\nYARD_IMAGE_FALLBACK=subyard-e2e-debian-13-cloud-vm\nHOST_MOUNTS=\nHOST_LINKS=\nLIMITS_CPU=1\nLIMITS_MEMORY=1GiB\nROOT_DISK_SIZE=10GiB\nSRV_VOLUME_TYPE=block\nSRV_VOLUME_SIZE=2GiB\n'
    operator_env bash -c 'printf "%s" "$2" > "$1"; chmod 0600 "$1"' _ "$OPERATOR_HOME/.config/subyard/yards/$name/config.env" "$settings"
  fi
  if ! operator_yard -Y "$name" init --yes; then
    [ -z "$(incus list --project "$(project_for "$name")" --format csv -c n)" ] || die 'init failed after image lookup'
    incus project set "$(project_for "$name")" features.images=false
    operator_yard -Y "$name" init --yes
  fi
  incus project set "$(project_for "$name")" user.p0-power-systemd="$MARKER"
  operator_yard -Y "$name" start --yes
  yard_guest "$name" sh -c 'printf "%s\n" "$1" > /var/lib/yard-maintenance-canary' _ "$MARKER-$name"
  UUID[$name]="$(yard_uuid "$name")" BOOT[$name]="$(yard_boot "$name")"
done
lowlevel() { incus project get "$(project_for "$1")" restricted.virtual-machines.lowlevel; }
[ -z "$(lowlevel weighted)" ] && [ -z "$(lowlevel z-last)" ] || die 'VM yards already carry the VM start policy grant'
# Published 0.28.0 pins the project grant that its own VM start policy writes, so its
# direct update stops at the first restarted VM yard and keeps the old release active.
read -r stuck _ archive_sha < <(awk -F '\t' '$1=="0.28.0"' "$ROOT/dev/e2e/published-upgrade-baselines.tsv")
[ "$stuck" = 0.28.0 ] || die 'stuck baseline missing'
rc=0
operator_yard update --version "$stuck" --yes > "$STATE_ROOT/stuck.log" 2>&1 || rc=$?
[ "$(operator_env sha256sum "$OPERATOR_HOME/.subyard/releases/$stuck/subyard-$stuck-linux-amd64.tar.gz" | awk '{print $1}')" = "$archive_sha" ] || die 'stuck baseline changed'
[ "$rc" != 0 ] && grep -Fq 'code=recovery-ambiguous' "$STATE_ROOT/stuck.log" || { cat "$STATE_ROOT/stuck.log" >&2; die 'published 0.28.0 update did not stop'; }
case "$(operator_env readlink "$OPERATOR_HOME/.subyard/runtime/current")" in releases/"$creation"-*) ;; *) die 'stopped update changed the active release' ;; esac
operator_env jq -e --arg target "$stuck-" '.checkpoint != "complete" and (.goal.target | startswith($target))' "$V2_JOURNAL" >/dev/null || die 'stopped update left no unfinished journal'
[ "$(marker default)" = v1 ] && [ -z "$(marker weighted)" ] && [ "$(lowlevel weighted)" = allow ] \
  && [ -z "$(marker z-last)" ] && [ -z "$(lowlevel z-last)" ] && [ "$(yard_boot z-last)" = "${BOOT[z-last]}" ] || die 'stopped update reached an unexpected native state'
BOOT[default]="$(yard_boot default)" BOOT[weighted]="$(yard_boot weighted)"
# The active 0.21.3 launcher asks only the journal-selected engine, even for a newer release.
before="$(protected_state)"
update_candidate --check --offline --version "$CANDIDATE_VERSION" > "$STATE_ROOT/stuck-check.log" 2>&1 || true
grep -Fq 'the journal-selected release cannot provide a valid transition inspection' "$STATE_ROOT/stuck-check.log" || { cat "$STATE_ROOT/stuck-check.log" >&2; die 'old launcher did not keep the journal-selected owner'; }
[ "$(protected_state)" = "$before" ] || die 'old launcher check changed protected state'
# An owned guest preflight refusal leaves the VM running. This tests isolation
# without host shims, a fabricated exit code or an extra guest mount.
yard_guest weighted sh -eu -c 'test ! -e /etc/systemd/system/tmp.mount; test ! -L /etc/systemd/system/tmp.mount; printf "fixture foreign policy\n" > /etc/systemd/system/tmp.mount'
# The candidate's own launcher replaces the unfinished journal; rerunning the installer is the operator route.
operator_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" bash "$RELEASE_ROOT/subyard-install.sh" --publish-only --version "$CANDIDATE_VERSION" > "$STATE_ROOT/stage.log"
LAUNCHER="$(sed -n 's/^staged release=.* candidate=//p' "$STATE_ROOT/stage.log")/bin/yard"
operator_env test -x "$LAUNCHER" || die 'candidate launcher was not staged'
before="$(protected_state)"
operator_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" "$LAUNCHER" update --check --offline --version "$CANDIDATE_VERSION" --json > "$STATE_ROOT/plan.json"
[ "$(protected_state)" = "$before" ] || die 'read-only plan changed protected state'
jq -e '.resume==null and (.blockers|length)==0 and any(.assessment.consequences[]; startswith("replace the unfinished release transition"))' "$STATE_ROOT/plan.json" >/dev/null || die 'check omitted the journal replacement'
for name in weighted z-last; do
  jq -e --arg name "yard $name:" 'any(.decisions[]?; (.result | contains($name) and contains("sessions") and contains("persistent data")))' "$STATE_ROOT/plan.json" >/dev/null || die 'check omitted restart disclosure'
done
! jq -e 'any(.decisions[]?; .result | contains("yard default:"))' "$STATE_ROOT/plan.json" >/dev/null || die 'check restarts a finished yard'
before="$(protected_state)"
printf -v decline_command '%q ' "$LAUNCHER" update --offline --version "$CANDIDATE_VERSION"
rc=0
printf 'n\n' | operator_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" script -q -e -c "$decline_command" /dev/null > "$STATE_ROOT/decline.log" 2>&1 || rc=$?
[ "$rc" != 0 ] && grep -Fq 'operation declined' "$STATE_ROOT/decline.log" || die 'ordinary confirmation was not declined'
[ "$(protected_state)" = "$before" ] || die 'decline changed protected state'
for name in "${NAMES[@]}"; do [ "$(yard_boot "$name")" = "${BOOT[$name]}" ] && [ "$(yard_uuid "$name")" = "${UUID[$name]}" ] || die 'decline changed native state'; done
[ -z "$(marker weighted)" ] && [ -z "$(marker z-last)" ] || die 'decline changed native state'
# The approved restart interrupts a real live SSH session instead of refusing it.
operator_env ssh -F "$OPERATOR_HOME/.ssh/config" -o BatchMode=yes -T yard-z-last -- true || die 'fixture SSH transport to the z-last yard is unavailable'
operator_env ssh -F "$OPERATOR_HOME/.ssh/config" -o BatchMode=yes -T yard-z-last -- sleep 900 > "$STATE_ROOT/live-ssh.log" 2>&1 &
# operator_env runs through sudo, so liveness checks of this root-owned PID need sudo too.
session_pid=$!
# The fixture's incus wrapper closes stdin, so the probe travels as an argument.
ssh_probe() { yard_guest z-last sh -c "$(cat "$ROOT/scripts/vscode-remote-maintenance.sh")" vscode-remote-maintenance check-ssh; }
for _ in {1..40}; do [ "$(ssh_probe)" = active ] && break; sleep 0.5; done
probe="$(ssh_probe)"
sudo -n kill -0 "$session_pid" && [ "$probe" = active ] \
  || die "live SSH session did not connect: probe=$probe ssh=$(tr '\n' ' ' < "$STATE_ROOT/live-ssh.log")"
operator_env env YARD_RELEASE_BASE_URL="file://$RELEASE_ROOT" bash "$RELEASE_ROOT/subyard-install.sh" --version "$CANDIDATE_VERSION" --yes > "$STATE_ROOT/update.log" 2>&1 || { rc=$?; tail -c 16384 "$STATE_ROOT/update.log" >&2; exit "$rc"; }
cat "$STATE_ROOT/update.log"
grep -Fq "interrupting the SSH and VS Code Remote-SSH sessions still connected to 'yard-z-last'" "$STATE_ROOT/update.log" || die 'approved restart did not report the interrupted session'
for _ in {1..60}; do sudo -n kill -0 "$session_pid" 2>/dev/null || break; sleep 0.5; done
! sudo -n kill -0 "$session_pid" 2>/dev/null || die 'approved restart left the live SSH session connected'
wait "$session_pid" 2>/dev/null || true
grep -Fq 'yard weighted: disk-/tmp migration pending' "$STATE_ROOT/update.log" || die 'failure warning missing'
before="$(protected_state)"
update_candidate --check --offline --version "$CANDIDATE_VERSION" --json > "$STATE_ROOT/ready.json"
[ "$(protected_state)" = "$before" ] || die 'read-only check changed protected state'
jq -e '.outcome.status=="ready" and any(.outcome.warnings[]; contains("yard weighted:"))' "$STATE_ROOT/ready.json" >/dev/null || die 'ready warning missing'
[ -z "$(marker weighted)" ] && [ "$(yard_uuid weighted)" = "${UUID[weighted]}" ] || die 'failure lost identity or pending marker'
[ "$(yard_boot weighted)" = "${BOOT[weighted]}" ] && [ "$(incus list yard-weighted --project subyard-weighted --format csv -c s)" = RUNNING ] || die 'preflight refusal stopped the yard'
# z-last first received the VM start policy inside this transition.
[ "$(marker z-last)" = v1 ] && [ "$(yard_boot z-last)" != "${BOOT[z-last]}" ] && [ "$(lowlevel z-last)" = allow ] || die 'remaining yard did not finish'
[ "$(marker default)" = v1 ] && [ "$(yard_boot default)" = "${BOOT[default]}" ] || die 'replacement restarted a finished yard'
BOOT[z-last]="$(yard_boot z-last)"
operator_yard -Y weighted config set VM_CPU_WEIGHT 250 --scope yard --local --yes
incus config set yard-weighted user.subyard.vm_cpu_weight=250 --project subyard-weighted
yard_guest weighted rm -- /etc/systemd/system/tmp.mount
# A required raw disk is valid at attachment; its removed source makes the
# actual Incus start fail after the guarded stop, without changing desired power.
truncate -s 4M "$STATE_ROOT/restart-disk.raw"
chmod 0644 "$STATE_ROOT/restart-disk.raw"
incus config device add yard-weighted maintenance-fault disk source="$STATE_ROOT/restart-disk.raw" readonly=true --project subyard-weighted
mv "$STATE_ROOT/restart-disk.raw" "$STATE_ROOT/restart-disk.held"
update_candidate --offline --yes > "$STATE_ROOT/start-failure.log" 2>&1
cat "$STATE_ROOT/start-failure.log"
grep -Fq 'yard -Y weighted start; then run yard migrate' "$STATE_ROOT/start-failure.log" || die 'physical failure lost its executable retry sequence'
[ "$(incus list yard-weighted --project subyard-weighted --format csv -c s)" = STOPPED ] \
  && [ -z "$(marker weighted)" ] && [ "$(yard_uuid weighted)" = "${UUID[weighted]}" ] \
  && [ "$(incus config get yard-weighted user.subyard.desired_power --project subyard-weighted)" = running ] || die 'failed start changed identity, marker or desired power'
update_candidate --offline --yes
[ "$(incus list yard-weighted --project subyard-weighted --format csv -c s)" = STOPPED ] && [ -z "$(marker weighted)" ] || die 'update booted a stopped pending yard'
mv "$STATE_ROOT/restart-disk.held" "$STATE_ROOT/restart-disk.raw"
operator_yard -Y weighted start --yes
incus config device remove yard-weighted maintenance-fault --project subyard-weighted
[ "$(marker weighted)" = v1 ] && [ "$(yard_boot weighted)" != "${BOOT[weighted]}" ] || die 'explicit start did not complete the pending migration'
operator_yard migrate --yes
for name in "${NAMES[@]}"; do
  [ "$(yard_uuid "$name")" = "${UUID[$name]}" ] && [ "$(yard_guest "$name" cat /var/lib/yard-maintenance-canary)" = "$MARKER-$name" ] || die 'data or identity changed'
  [ "$name" = weighted ] || [ "$(yard_boot "$name")" = "${BOOT[$name]}" ] || die 'retry restarted a finished yard'
done
pid="$(incus query '/1.0/instances/yard-weighted/state?project=subyard-weighted' | jq -er .pid)"
cgroup="$(sudo -n awk -F: '$1=="0"{print $3}' "/proc/$pid/cgroup")"
case "$cgroup" in /subyardvm-*.scope) ;; *) die 'VM is outside its CPU scope' ;; esac
[ "$(sudo -n cat "/sys/fs/cgroup$cgroup/cgroup.procs")" = "$pid" ] || die 'CPU scope contains a foreign process'
[ "$(sudo -n cat "/sys/fs/cgroup$cgroup/cpu.weight")" = 250 ] || die 'CPU weight was not applied'
update_candidate --check --offline --json > "$STATE_ROOT/retried.json"
jq -e '.outcome.status=="ready" and .outcome.reachedGoal' "$STATE_ROOT/retried.json" >/dev/null || die 'retry did not finish ready'
operator_env jq -e '.checkpoint=="complete"' "$V2_JOURNAL" >/dev/null || die 'retry journal incomplete'
printf 'ok: one upfront confirmation, isolated pending yard and marker-only selective retry\n'
