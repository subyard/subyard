#!/usr/bin/env bash
# Runs as root inside a hosted runner's disposable test yard when lane diagnostics are kept and
# prints the broker state that explains a refusal or quarantine. Lease events read by the
# controller can lack the cause, for example when a base build fails before its builder VM exists.
# Slot and image records are reduced to allowlisted fields without lease, client or controller
# identities. All output, journals and errors included, passes the broker's own redaction of SSH
# keys, secret-named fields and private keys, since failure reasons can quote command lines.
set -uo pipefail

section() { printf '\n== %s\n' "$*"; }

collect() {
  local state="${SUBYARD_CI_BROKER_STATE:-/var/lib/subyard/test-vms}" configured kind
  # SUBYARD_CI_BROKER_STATE exists only so host-free tests can use a fixture state directory.
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
BASE = ("key", "environment", "image", "upstream", "architecture", "fingerprint", "created_at",
        "last_used_at", "compressed_bytes", "unusable")


def load(name):
    try:
        return json.loads((state / name).read_text())
    except (OSError, ValueError) as error:
        print(f"{name}: unreadable ({type(error).__name__})")
        return None


def pick(record, keys):
    return {key: record[key] for key in keys if key in record} if isinstance(record, dict) else None


leases = load("leases.json")
if isinstance(leases, dict):
    print("leases.json slots:", json.dumps([pick(slot, SLOT) for slot in leases.get("slots") or []],
                                           indent=1, sort_keys=True))
images = load("images.json")
if isinstance(images, dict):
    build = pick(images.get("build"), BUILD)
    if build is not None:
        build["incident_slot"] = pick(images["build"].get("incident_slot"),
                                      ("slot_id", "resource_generation", "lease_epoch"))
    summary = {key: images.get(key) for key in ("last_error", "retry_attempt", "retry_after")}
    summary["bases"] = [pick(base, BASE) for base in images.get("bases") or []]
    summary["build"] = build
    print("images.json:", json.dumps(summary, indent=1, sort_keys=True))
PY

  for kind in events incidents; do
    section "newest spooled broker $kind"
    find "$state/spool/$kind" -maxdepth 1 -type f -name '*.json' -printf '%T@ %p\n' 2>/dev/null \
      | sort -n | tail -n 12 | while read -r _ path; do
          printf -- '-- %s\n' "${path##*/}"
          head -c 8192 -- "$path"
          printf '\n'
        done
  done

  # Each read is bounded on its own, so a hung daemon costs one section, not the rest.
  section 'Incus and broker journal'
  timeout -k 2s 10s journalctl --no-pager -o short-iso -n 300 -u incus.service \
    -u subyard-test-vms-broker.service -u subyard-test-vms-lease-reaper.service
  section 'Incus operations'
  timeout -k 2s 10s incus operation list --all-projects
  section 'disk'
  timeout -k 2s 10s df -h / /srv
  timeout -k 2s 10s incus storage info default
}

# The same patterns as sanitizeBrokerText in internal/adapters/testvmsruntime/events.go.
redact="$(cat <<'PY'
import re
import sys

NAMES = r"capability(?:_hash)?|private_key|controller_fingerprint|token|password|secret"
# Split characters from bounded reads must not depend on the locale's error handler.
text = sys.stdin.buffer.read().decode("utf-8", "surrogateescape")
for pattern, replacement in (
    (r"\bssh-(?:ed25519|rsa)\s+[A-Za-z0-9+/=]+", "ssh-key [redacted]"),
    (r"(?i)\b(" + NAMES + r")(\s*=\s*)[^\s,\"']+", r"\1\2[redacted]"),
    (r'(?i)("(?:' + NAMES + r')"\s*:\s*)"(?:\\.|[^"\\])*"', r'\1"[redacted]"'),
    (r"(?im)^([ \t]*(?:" + NAMES + r")[ \t]*:[ \t]*).*$", r"\1[redacted]"),
    (r"(?s)-----BEGIN [^-]*PRIVATE KEY-----.*?-----END [^-]*PRIVATE KEY-----", "private-key [redacted]"),
):
    text = re.sub(pattern, replacement, text)
# Keep the uploaded log bounded; the newest records come last, so keep the tail.
data = text.encode("utf-8", "surrogateescape")
if len(data) > 1 << 20:
    data = b"[earlier output omitted]\n" + data[-(1 << 20):]
sys.stdout.buffer.write(data)
PY
)"
collect 2>&1 | python3 -I -c "$redact"
