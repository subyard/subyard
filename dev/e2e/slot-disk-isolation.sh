#!/usr/bin/env bash
# Fill one disposable guest disk while probing its peer in the same lease.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# Reuse the same lease/transport helpers as p0-acceptance.sh.
# shellcheck source=dev/agent-e2e.sh
. "$ROOT/dev/agent-e2e.sh"

[ "$#" = 2 ] || die 'usage: slot-disk-isolation.sh SLOT NEW_OUTPUT_DIRECTORY'
set_requested_slot "$1" slot
output="$2"
mkdir -- "$output"
output="$(cd "$output" && pwd)"
LEASE_PURPOSE='slot-disk-isolation'
probe_pid=''
finish() {
  local rc=$?
  trap - EXIT INT TERM
  if [ -n "$probe_pid" ]; then
    kill "$probe_pid" 2>/dev/null || true
    wait "$probe_pid" 2>/dev/null || true
  fi
  # Preserve the test result while using the runner's normal cleanup/release trap.
  set +e
  (exit "$rc")
  cleanup_on_exit
}
trap finish EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
snapshot() { facade_request status >"$output/$1.json"; }
event() {
  local expected="$1" line
  IFS= read -r -t 360 -u "$probe_read" line || die "guest did not report $expected"
  [[ "$line" == 'ENVIRONMENT_PROBE '* ]] || die 'unexpected guest response'
  printf '%s\n' "${line#ENVIRONMENT_PROBE }" >"$output/$expected.json"
  jq -e --arg event "$expected" '.event == $event' "$output/$expected.json" >/dev/null
  printf 'slot-disk-isolation: %s\n' "$expected"
}
peer_probe() {
  guest 2 timeout 30 python3 -c '
import hashlib, json, os, pathlib, tempfile
c = json.loads(pathlib.Path("/run/subyard-e2e-lease.json").read_text())
with tempfile.TemporaryFile(dir="/var/tmp") as f:
    f.write(b"peer remains writable\n"); f.flush(); os.fsync(f.fileno())
print(json.dumps({"slot": c["slot"], "run": c["run"], "writable": True,
 "machine_id_sha256": hashlib.sha256(pathlib.Path("/etc/machine-id").read_bytes()).hexdigest()}))
' </dev/null >"$output/peer-$1.json"
}

snapshot initial
LOCAL_TEMP="$(mktemp -d "${TMPDIR:-/tmp}/subyard-agent-e2e.XXXXXX")"
acquire_lease
start_lease_keeper
snapshot held
jq -e --arg slot "$LEASE_SLOT" '
  .pool.slots[] | select(.slot_id == $slot) | .environment |
  .type == "subyard-pair" and .vm_count == 2 and
  .memory_per_vm == "4GiB" and .disk_per_vm == "20GiB"
' "$output/held.json" >/dev/null || die 'expected the standard 4GiB/20GiB pair'
git -C "$ROOT" rev-parse HEAD >"$output/source-head.txt"
printf '%s\n' "$BASE_FINGERPRINT" >"$output/base-fingerprint.txt"
# Reuse the bounded ENOSPC writer and baseline probe. Its temporary file is
# unlinked immediately and reclaimed on close or guest process termination.
probe="$(python3 - "$ROOT/dev/e2e/environment-lifecycle.py" <<'PY'
import runpy, sys
print(runpy.run_path(sys.argv[1])["GUEST"])
PY
)"
coproc DISK_PROBE { guest 1 timeout --kill-after=5 600 python3 -u -c "$probe" \
  "$LEASE_SLOT" subyard-pair "$LEASE_PURPOSE" "disk-$LEASE_RUN"; }
probe_pid="$DISK_PROBE_PID"
exec {probe_read}<&"${DISK_PROBE[0]}"
exec {probe_write}>&"${DISK_PROBE[1]}"
event ready
jq -e '.root_disk_bytes == 21474836480 and
  .memory_bytes >= 3650722201 and .memory_bytes <= 4294967296' "$output/ready.json" >/dev/null
peer_probe before
printf 'fill\n' >&"$probe_write"
event full
jq -e '.errno == 28 and .written_bytes > 0 and .written_bytes <= .root_disk_bytes' \
  "$output/full.json" >/dev/null
snapshot disk-full
peer_probe full
cmp "$output/peer-before.json" "$output/peer-full.json"
printf 'clear\n' >&"$probe_write"
event cleared
jq -e '.free_bytes > 0' "$output/cleared.json" >/dev/null
printf 'probe\n' >&"$probe_write"
event identity
jq -e '.root_writable == true' "$output/identity.json" >/dev/null
peer_probe cleared
cmp "$output/peer-before.json" "$output/peer-cleared.json"
snapshot disk-cleared
printf 'release\n' >&"$probe_write"
event released
exec {probe_write}>&-
wait "$probe_pid"
probe_pid=''
exec {probe_read}<&-
release_lease
snapshot final
jq -e --arg slot "$LEASE_SLOT" '
  ([.pool.slots[] | select(.slot_id == $slot) |
    .state == "available" and (.reserved != true)] == [true]) and
  ([.resources.slots[] | select(.slot_id == $slot)] | length == 0)
' "$output/final.json" >/dev/null
printf 'slot-disk-isolation: PASS; guest ENOSPC, peer writable, allocation released\n'
