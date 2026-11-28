#!/usr/bin/env bash
# Runs as root inside a hosted runner's disposable test yard after a failed check and prints the
# broker state that explains a refusal or quarantine. Lease events read by the controller can
# lack the cause, for example when a base build fails before its builder VM exists. Prints only
# non-secret fields: no lease, client or controller identities, keys or worker credentials.
set -uo pipefail

section() { printf '\n== %s\n' "$*"; }

# Overridable only so host-free tests can use a fixture state directory.
state="${SUBYARD_CI_BROKER_STATE:-/var/lib/subyard/test-vms}"
if [ -z "${SUBYARD_CI_BROKER_STATE:-}" ] && [ -r /etc/subyard/test-vms.env ]; then
  configured="$(sed -n 's/^E2E_VM_STATE_DIR=//p' /etc/subyard/test-vms.env | tail -n 1)"
  # provision.sh writes the value with printf %q; a plain absolute path needs no unquoting.
  case "$configured" in /*) state="$configured" ;; esac
fi
printf 'state directory: %s\n' "$state"

section 'lease slots and image registry'
python3 -I - "$state" <<'PY'
import json
import sys
from pathlib import Path

state = Path(sys.argv[1])
SLOT = ("slot_id", "state", "failure_reason", "last_failure_event_id", "incident_id",
        "recovery_attempt", "next_recovery_at", "recovery_started_at", "provisioning_started_at",
        "lease_epoch", "resource_generation", "environment", "base_fingerprint", "reserved")
BUILD = ("key", "environment", "image", "project", "memory_bytes", "disk_peak_bytes",
         "readiness_failure", "incident_id")


def load(name):
    try:
        return json.loads((state / name).read_text())
    except (OSError, ValueError) as error:
        print(f"{name}: unreadable ({type(error).__name__})")
        return None


leases = load("leases.json")
if isinstance(leases, dict):
    slots = [{key: slot[key] for key in SLOT if key in slot}
             for slot in leases.get("slots") or [] if isinstance(slot, dict)]
    print("leases.json slots:", json.dumps(slots, indent=1, sort_keys=True))
images = load("images.json")
if isinstance(images, dict):
    build = images.get("build")
    if isinstance(build, dict):
        incident_slot = build.get("incident_slot")
        build = {key: build[key] for key in BUILD if key in build}
        if isinstance(incident_slot, dict):
            build["incident_slot"] = {key: incident_slot.get(key)
                                      for key in ("slot_id", "resource_generation", "lease_epoch")}
    summary = {key: images.get(key) for key in ("last_error", "retry_attempt", "retry_after", "bases")}
    summary["build"] = build
    print("images.json:", json.dumps(summary, indent=1, sort_keys=True))
PY

# The spool holds events and incidents the broker already redacted for export.
for kind in events incidents; do
  section "newest spooled broker $kind"
  find "$state/spool/$kind" -maxdepth 1 -type f -name '*.json' -printf '%T@ %p\n' 2>/dev/null \
    | sort -n | tail -n 12 | while read -r _ path; do
        printf -- '-- %s\n' "${path##*/}"
        head -c 8192 -- "$path"
        printf '\n'
      done
done

section 'Incus and broker journal'
journalctl --no-pager -o short-iso -n 300 -u incus.service -u subyard-test-vms-broker.service \
  -u subyard-test-vms-lease-reaper.service 2>&1

section 'Incus operations'
incus operation list --all-projects 2>&1

section 'disk'
df -h / /srv 2>&1
incus storage info default 2>&1
