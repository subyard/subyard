#!/usr/bin/env bash
# shellcheck disable=SC2034 # Sourced adapter reads its controller globals.
# Exercise runner admission, pinned SSH config, grant and teardown without a hosted VM.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
# shellcheck source=dev/agent-e2e.sh
. "$ROOT/dev/agent-e2e.sh"
STATE="$(mktemp -d "${TMPDIR:-/tmp}/subyard-runner-lease-test.XXXXXXXX")"
trap 'find "$STATE" -depth -delete' EXIT
mkdir "$STATE/controller" "$STATE/run" "$STATE/run/subyard-e2e-runner"
printf '%s\n' 123/1/checks > "$STATE/run/subyard-e2e-runner/job"
printf 'image manifest' > "$STATE/image"
ssh-keygen -q -t ed25519 -N '' -f "$STATE/hostkey"
sed -e "s|/run/subyard-e2e|$STATE/run/subyard-e2e|g" -e "s|/imagegeneration/imagedata.json|$STATE/image|g" \
  -e "s|/etc/ssh/ssh_host_ed25519_key|$STATE/hostkey|g" "$ROOT/dev/ci/runner-lease.sh" > "$STATE/adapter"
# shellcheck disable=SC1091
. "$STATE/adapter"
unset GITHUB_ACTIONS RUNNER_ENVIRONMENT
if runner_job >/dev/null || (acquire_lease) >/dev/null 2>&1; then exit 1; fi
runner_job() { printf '123/1/checks\n'; }
lease_attribution() { printf 'unknown\tSubyard\n'; }
# All write paths in the sourced adapter are confined to the private fixture.
sudo() {
  [ "$1" = -n ] && shift
  case "$1" in
    systemd-run) printf 'started\n' > "$STATE/unit" ;;
    systemctl) [ "$2" = stop ] && printf 'stopped\n' > "$STATE/unit" ;;
    mkdir|install|rm) "$@" ;;
    *) return 2 ;;
  esac
}
systemctl() { [ "$1" = show ] && printf 'loaded\n'; }
guest() { [ "$1" = 1 ] && printf 'image=ubuntu-24.04 arch=amd64'; }
SUBYARD_E2E_CANDIDATE_BUNDLE="$STATE/image"
SUBYARD_E2E_CANDIDATE_SHA256="$(sha256sum "$STATE/image" | cut -d' ' -f1)"
LOCAL_TEMP="$STATE/controller" LEASE_REQUESTED_SLOT=slot-001
VM_COUNT_REQUESTED=2
if (acquire_lease) >/dev/null 2>&1; then exit 1; fi
VM_COUNT_REQUESTED=1 IMAGE_REQUESTED=debian-13
if (acquire_lease) >/dev/null 2>&1; then exit 1; fi
test ! -e "$STATE/run/subyard-e2e-runner/used"
LEASE_PURPOSE=runner-test IMAGE_REQUESTED=ubuntu-24.04 SUBYARD_E2E_LANE=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
acquire_lease 2>"$STATE/evidence"
grep -Fq "grant type=runner vms=1 base=$SUBYARD_E2E_CANDIDATE_SHA256 image=ubuntu-24.04 arch=amd64" "$STATE/evidence"
test "$(cat "$STATE/unit")" = started
test "$(stat -c %a "$STATE/run/subyard-e2e-lease.json")" = 444
jq -e --arg run "$LEASE_RUN" '.schema_version == 2 and .run == $run and .slot == "slot-001"' "$STATE/run/subyard-e2e-lease.json" >/dev/null
ssh -F "$CLIENT_CONFIG" -G e2e-vm-1 > "$STATE/effective"
grep -qx 'hostname 127.0.0.1' "$STATE/effective"
grep -qx 'stricthostkeychecking true' "$STATE/effective"
grep -qx 'forwardagent no' "$STATE/effective"
ssh-keygen -F e2e-vm-1 -f "$BASTION_KNOWN_HOSTS" >/dev/null
release_lease 2>>"$STATE/evidence"
grep -q 'event=release exit_code=0$' "$STATE/evidence"
test "$(cat "$STATE/unit")" = stopped
test ! -e "$STATE/run/subyard-e2e-lease.json"
test -z "$(ls -A "$STATE/run/subyard-e2e-runner/used")"
if (acquire_lease) >/dev/null 2>&1; then exit 1; fi
printf 'ok: runner lease refuses unsupported hosts and reuse, pins SSH, records the measured grant and fences release\n'
