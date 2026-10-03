#!/usr/bin/env bash
# Measure precise QEMU RSS after bounded allocation/free at the configured guest RAM size.
set -euo pipefail
project="${1:?expected allocated test project}"
instance="${2:?expected allocated test VM}"
die() { printf 'vm-page-reporting: %s\n' "$*" >&2; exit 1; }
allocation=''
marker_root=''
cleanup() {
  local rc=$?
  trap - EXIT
  if [ -n "$allocation" ]; then
    timeout --foreground -k 2 5 incus exec "$instance" --project "$project" -- \
      touch "$marker_root/release" >/dev/null 2>&1 || true
    wait "$allocation" || true
  fi
  if [ -n "$marker_root" ]; then
    timeout --foreground -k 2 5 incus exec "$instance" --project "$project" -- \
      rm -rf -- "$marker_root" >/dev/null 2>&1 || rc=1
  fi
  exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

wait_phase() {
  local phase="$1"
  while [ "$SECONDS" -lt "$phase_deadline" ]; do
    if timeout --foreground -k 2 5 incus exec "$instance" --project "$project" -- \
      test -f "$marker_root/$phase"; then
      [ "$SECONDS" -lt "$phase_deadline" ] || die "allocator $phase acknowledgement timed out"
      return 0
    fi
    if ! kill -0 "$allocation" 2>/dev/null; then
      # The allocator may publish the marker and exit after the first probe.
      timeout --foreground -k 2 5 incus exec "$instance" --project "$project" -- \
        test -f "$marker_root/$phase" || die "allocator exited before $phase acknowledgement"
      [ "$SECONDS" -lt "$phase_deadline" ] || die "allocator $phase acknowledgement timed out"
      return 0
    fi
    sleep 0.1
  done
  die "allocator $phase acknowledgement timed out"
}

diagnostics() {
  local phase="$1"
  sudo -n awk -v phase="$phase" -v round="$round" \
    '/^(Rss|Pss|Anonymous|Shared_Clean|Shared_Dirty|Private_Clean|Private_Dirty):/ {
      printf "reporting_smaps_round=%s phase=%s metric=%s bytes=%.0f\n", round, phase, substr($1,1,length($1)-1), $2*1024
    }' "/proc/$pid/smaps_rollup"
  timeout --foreground -k 2 5 incus exec "$instance" --project "$project" -- \
    awk -v phase="$phase" -v round="$round" '
      FILENAME == "/proc/meminfo" && /^(MemTotal|MemFree|MemAvailable|Buffers|Cached|AnonPages|Shmem|Slab):/ {
        printf "reporting_guest_round=%s phase=%s metric=%s bytes=%.0f\n", round, phase, substr($1,1,length($1)-1), $2*1024
      }
      FILENAME == "/proc/buddyinfo" {
        for (i=5;i<=NF;i++) printf "reporting_buddy_round=%s phase=%s node=%s zone=%s order=%s blocks=%s\n", round, phase, $2+0, $4, i-5, $i+0
      }' /proc/meminfo /proc/buddyinfo
}

measure_round() {
  local round="$1" baseline baseline_vmrss peak peak_vmrss current reclaimed
  baseline="$(rss)"
  baseline_vmrss="$(vmrss)"
  diagnostics baseline
  # Bound cold touching, held measurement and release within one operation.
  phase_deadline=$((SECONDS + 180))
  timeout --foreground -k 2 210 incus exec "$instance" --project "$project" -- \
    timeout -k 2 180 python3 - "$marker_root" <<'PY' &
import mmap
from pathlib import Path
import struct
import sys
import time
root = Path(sys.argv[1])
pages = mmap.mmap(-1, 384 * 1024 * 1024)
try:
    for offset in range(0, len(pages), 4096):
        # Distinct nonzero pages prevent deduplication from imitating reporting.
        struct.pack_into('Q', pages, offset, offset + 1)
    (root / 'touched').touch()
    deadline = time.monotonic() + 30
    while not (root / 'release').exists():
        if time.monotonic() >= deadline:
            raise SystemExit('allocator release acknowledgement timed out')
        time.sleep(0.1)
finally:
    pages.close()
(root / 'freed').touch()
PY
  allocation=$!
  wait_phase touched
  # The allocation stays resident throughout the bounded host measurement.
  peak="$(rss)"
  peak_vmrss="$(vmrss)"
  diagnostics held
  [ "$SECONDS" -lt "$phase_deadline" ] || die 'held allocation measurement timed out'
  timeout --foreground -k 2 5 incus exec "$instance" --project "$project" -- \
    touch "$marker_root/release"
  wait_phase freed
  wait "$allocation" || die 'allocator failed'
  allocation=''
  diagnostics freed
  printf 'reporting_allocation_round=%s baseline_bytes=%s peak_bytes=%s baseline_vmrss_bytes=%s peak_vmrss_bytes=%s\n' \
    "$round" "$baseline" "$peak" "$baseline_vmrss" "$peak_vmrss"
  sleep 35
  reclaimed=0
  for _ in $(seq 1 60); do
    current="$(rss)"
    if [ "$((peak - current))" -ge 201326592 ]; then reclaimed=1; break; fi
    sleep 1
  done
  diagnostics after
  printf 'reporting_round=%s baseline_bytes=%s peak_bytes=%s after_bytes=%s guest_total_kib=%s after_vmrss_bytes=%s\n' \
    "$round" "$baseline" "$peak" "$current" "$memory" "$(vmrss)"
  [ "$((peak - baseline))" -ge 201326592 ] || die 'allocation did not produce a measurable QEMU RSS peak'
  [ "$reclaimed" = 1 ] || die 'QEMU did not return at least 192 MiB after guest free'
  [ "$(total)" = "$memory" ] || die 'guest-visible RAM changed'
}
[ -n "${SUBYARD_E2E_VM:-}" ] || die 'run only in an allocated E2E guest'
[[ "$project" =~ ^[a-zA-Z0-9_-]+$ && "$instance" =~ ^[a-zA-Z0-9_-]+$ ]] || die 'unsafe target'
guest() { incus exec "$instance" --project "$project" -- "$@"; }
incus version
qemu-system-x86_64 --version | sed -n '1p'
info="$(incus query "/1.0/instances/$instance?project=$project")"
[ "$(jq -r .type <<<"$info")" = virtual-machine ] || die 'target is not a VM'
[[ "$(jq -r '.expanded_config["raw.qemu.conf"]' <<<"$info")" == *'free-page-reporting = "on"'* ]] \
  || die 'fixed reporting override is absent'
pid="$(incus query "/1.0/instances/$instance/state?project=$project" | jq -r .pid)"
[[ "$pid" =~ ^[1-9][0-9]*$ ]] && [ -r "/proc/$pid/status" ] || die 'QEMU PID unavailable'
[[ "$(cat "/proc/$pid/comm")" == qemu* ]] || die 'reported PID is not QEMU'
# VmRSS uses approximate counters; smaps_rollup walks the page tables. The
# allocated guest's dev user needs sudo to inspect root-owned QEMU mappings.
sudo -n test -r "/proc/$pid/smaps_rollup" || die 'precise QEMU RSS unavailable'
rss() { sudo -n awk '/^Rss:/ {printf "%.0f\n", $2 * 1024}' "/proc/$pid/smaps_rollup"; }
vmrss() { awk '/^VmRSS:/ {printf "%.0f\n", $2 * 1024}' "/proc/$pid/status"; }
total() { guest awk '/MemTotal:/ {print $2}' /proc/meminfo; }
guest bash -ceu '
  test -d /sys/module/virtio_balloon
  found=0
  for features in /sys/bus/virtio/drivers/virtio_balloon/virtio*/features; do
    test -f "$features" || continue
    value="$(cat "$features")"
    test "${value:5:1}" = 1
    found=1
  done
  test "$found" = 1
  uname -r
  grep -E "^CONFIG_(VIRTIO_BALLOON|PAGE_REPORTING)=" /boot/config-"$(uname -r)"
  order="$(cat /sys/module/page_reporting/parameters/page_reporting_order)"
  printf "guest_reporting_order=%s\n" "$order"
  test "$order" = 1
'
memory="$(total)"
# Provisioning leaves reclaimable package/image cache resident. Free that cache
# before measuring anonymous memory so the first allocation has a useful baseline.
guest sh -c 'sync; echo 3 > /proc/sys/vm/drop_caches'
# Linux budgets reporting passes over roughly 30 seconds on an idle system.
sleep 35
for round in 1 2; do
  candidate_root="$(timeout --foreground -k 2 5 incus exec "$instance" --project "$project" -- \
    sh -ec 'root=$(mktemp -d /run/subyard-page-reporting.XXXXXX); chmod 0700 "$root"; printf "%s\n" "$root"')"
  [[ "$candidate_root" =~ ^/run/subyard-page-reporting\.[a-zA-Z0-9]+$ ]] \
    || die 'invalid allocator marker directory'
  marker_root="$candidate_root"
  measure_round "$round"
  timeout --foreground -k 2 5 incus exec "$instance" --project "$project" -- rm -rf -- "$marker_root"
  marker_root=''
done
printf 'Free Page Reporting RSS measurement passed\n'
