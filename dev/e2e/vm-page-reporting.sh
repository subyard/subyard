#!/usr/bin/env bash
# Measure actual QEMU RSS after bounded guest allocation/free, without ballooning.
set -euo pipefail
project="${1:?expected allocated test project}"
instance="${2:?expected allocated test VM}"
die() { printf 'vm-page-reporting: %s\n' "$*" >&2; exit 1; }
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
rss() { awk '/VmRSS:/ {printf "%.0f\n", $2 * 1024}' "/proc/$pid/status"; }
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
  baseline="$(rss)"
  # Nested page faults can take longer than 30 seconds. Bound the allocation,
  # but sample its RSS until it completes so the fully touched peak is included.
  timeout --foreground 210 incus exec "$instance" --project "$project" -- \
    timeout --kill-after=5 180 python3 - <<'PY' &
import mmap
from pathlib import Path
import struct
import time
root = Path('/run/subyard-page-reporting-test')
root.mkdir(mode=0o700, exist_ok=True)
for name in ('touched', 'freed'):
    (root / name).unlink(missing_ok=True)
pages = mmap.mmap(-1, 384 * 1024 * 1024)
for offset in range(0, len(pages), 4096):
    # Distinct nonzero pages prevent deduplication from imitating reporting.
    struct.pack_into('Q', pages, offset, offset + 1)
(root / 'touched').touch()
time.sleep(20)
pages.close()
(root / 'freed').touch()
PY
  allocation=$!
  peak="$baseline"
  while kill -0 "$allocation" 2>/dev/null; do
    current="$(rss)"
    [ "$current" -le "$peak" ] || peak="$current"
    sleep 1
  done
  wait "$allocation" || die 'bounded guest allocation did not complete'
  printf 'reporting_allocation_round=%s baseline_bytes=%s peak_bytes=%s\n' "$round" "$baseline" "$peak"
  sleep 35
  reclaimed=0
  for _ in $(seq 1 60); do
    current="$(rss)"
    if [ "$((peak - current))" -ge 201326592 ]; then reclaimed=1; break; fi
    sleep 1
  done
  printf 'reporting_round=%s baseline_bytes=%s peak_bytes=%s after_bytes=%s guest_total_kib=%s\n' \
    "$round" "$baseline" "$peak" "$current" "$memory"
  [ "$((peak - baseline))" -ge 201326592 ] || die 'allocation did not produce a measurable QEMU RSS peak'
  [ "$reclaimed" = 1 ] || die 'QEMU did not return at least 192 MiB after guest free'
  [ "$(total)" = "$memory" ] || die 'guest-visible RAM changed'
done
printf 'Free Page Reporting RSS measurement passed\n'
