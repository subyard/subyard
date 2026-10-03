#!/usr/bin/env bash
# Exercise the real host controller with a synthetic, host-free allocator.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
TMP="$(mktemp -d)"
chmod 0700 "$TMP"
trap 'rm -rf "$TMP"' EXIT
fail() { printf 'FAIL: page-reporting: %s\n' "$*" >&2; exit 1; }

run_fixture() (
  set -euo pipefail
  scenario="$1"
  legacy="${2:-0}"
  fixture="$TMP/$scenario-$legacy"
  install -d -m 0700 "$fixture/markers"
  marker_root="$fixture/markers"
  allocation=''
  phase_deadline=0
  project=synthetic-project
  instance=synthetic-vm
  memory=1048576
  # Extract only the generic controller. No Incus, sudo or /proc access occurs.
  eval "$(sed -n '/^cleanup() {/,/^}/p; /^wait_phase() {/,/^}/p; /^measure_round() {/,/^}/p' \
    "$ROOT/dev/e2e/vm-page-reporting.sh")"
  die() { printf '%s\n' "$*" >&2; exit 1; }
  trap cleanup EXIT
  timeout() {
    [ "${1:-}" != --foreground ] || shift
    if [ "${1:-}" = -k ]; then shift 2; fi
    printf 'timeout_seconds=%s command=%s\n' "$1" "$2" >> "$fixture/events"
    shift
    "$@"
  }
  sleep() {
    if [ "$scenario" = timeout ]; then SECONDS=$((SECONDS + 10)); fi
    /bin/sleep 0.01
  }
  diagnostics() { printf 'diagnostic:%s\n' "$1" >> "$fixture/events"; }
  total() { printf '%s\n' "$memory"; }
  vmrss() { printf '0\n'; }
  incus() {
    [ "$1" = exec ] || exit 90
    shift 4
    [ "$1" = -- ] || exit 91
    shift
    case "$1" in
      timeout)
        guest_timeout="$2"
        [ "$guest_timeout" != -k ] || guest_timeout="$4"
        printf 'allocator_timeout_seconds=%s\n' "$guest_timeout" >> "$fixture/events"
        # The controller runs this in its real background child. Delay touching
        # until its first phase probe (new) or first sampling read (legacy).
        install -m 0600 /dev/null "$fixture/allocator.py"
        cat > "$fixture/allocator.py"
        printf '%s\n' "$BASHPID" > "$fixture/child"
        if [ "$scenario" = child-failure ]; then return 7; fi
        while [ ! -f "$fixture/can-touch" ]; do /bin/sleep 0.01; done
        if [ "$legacy" = 1 ]; then
          touch "$marker_root/touched" "$marker_root/freed"
        elif [ "$scenario" = timeout ]; then
          while [ ! -f "$marker_root/release" ]; do /bin/sleep 0.01; done
        elif [[ "$scenario" = freed-ack-* ]]; then
          touch "$marker_root/touched"
          while [ ! -f "$marker_root/release" ]; do /bin/sleep 0.01; done
          touch "$fixture/ready-to-free"
          while [ ! -f "$fixture/can-free" ]; do /bin/sleep 0.01; done
          [ "$scenario" = freed-ack-no-marker ] || touch "$marker_root/freed"
          [ "$scenario" != freed-ack-failed-child ] || return 7
        else
          # Run the transmitted allocator itself, substituting only its large
          # allocation/touch operations. Its hold, release and close are real.
          python3 - "$fixture" "$marker_root" <<'PY'
import mmap
from pathlib import Path
import struct
import sys
fixture = Path(sys.argv[1])
root = sys.argv[2]
class Pages:
    def __len__(self):
        return 384 * 1024 * 1024
    def close(self):
        (fixture / 'closed').touch()
mmap.mmap = lambda *args: Pages()
struct.pack_into = lambda *args: None
sys.argv = ['allocator', root]
exec(compile((fixture / 'allocator.py').read_text(), 'allocator', 'exec'))
PY
        fi
        [ "$scenario" != failed-after-free ] || return 7
        ;;
      test)
        touch "$fixture/can-touch"
        if [[ "$scenario" = freed-ack-* && "$3" = "$marker_root/freed" ]] \
          && [ ! -f "$fixture/absent-probe" ]; then
          while [ ! -f "$fixture/ready-to-free" ]; do /bin/sleep 0.01; done
          [ ! -f "$3" ] || return 95
          touch "$fixture/can-free"
          wait "$allocation" || true
          ! kill -0 "$allocation" 2>/dev/null || return 96
          touch "$fixture/absent-probe"
          if [ "$scenario" = freed-ack-too-late ]; then SECONDS=$((phase_deadline + 1)); fi
          # Preserve the initial absent result although the real child has now
          # published its marker (unless forbidden) and exited before kill -0.
          return 1
        fi
        case "$scenario" in cold-touch|cold-touch-too-late)
          if [ -f "$marker_root/touched" ] && [ ! -f "$fixture/clock-advanced" ]; then
            # Advance only controller time after the first cold touch completes;
            # Python's hold/release protocol remains real, with no 61-second sleep.
            cold_seconds=61
            [ "$scenario" != cold-touch-too-late ] || cold_seconds=181
            SECONDS=$((SECONDS + cold_seconds))
            touch "$fixture/clock-advanced"
            printf 'synthetic_cold_touch_elapsed_seconds=%s\n' "$cold_seconds"
          fi
          ;;
        esac
        test -f "$3"
        ;;
      touch)
        if [ "$scenario" = valid ] && [ "$legacy" = 0 ]; then
          [ -f "$fixture/held-sampled" ] || return 92
        fi
        touch "$2"
        printf 'release\n' >> "$fixture/events"
        ;;
      rm) "$@" ;;
      *) exit 93 ;;
    esac
  }
  rss() {
    local current=104857600
    if [ -f "$marker_root/touched" ] && [ ! -f "$marker_root/freed" ]; then
      current=$((current + 384 * 1024 * 1024))
      # A slow sampling read must finish before release is permitted.
      /bin/sleep 0.01
      [ ! -f "$marker_root/release" ] || return 94
      touch "$fixture/held-sampled"
      printf 'held-sampled\n' >> "$fixture/events"
    elif [ "$scenario" = shortfall ] && [ -f "$marker_root/freed" ]; then
      current=$((104857600 + 384 * 1024 * 1024 - 201326592 + 4096))
    elif [ "$legacy" = 1 ] && [ -n "$allocation" ] && [ ! -f "$fixture/can-touch" ]; then
      # Snapshot before startup/touching; automatic free overtakes this slow
      # read. Every later observation sees the already freed baseline again.
      touch "$fixture/can-touch"
      while [ ! -f "$marker_root/freed" ]; do /bin/sleep 0.01; done
    fi
    printf '%s\n' "$current"
  }
  if [ "$legacy" = 1 ]; then
    baseline="$(rss)"
    incus exec "$instance" --project "$project" -- timeout 60 python3 - "$marker_root" </dev/null &
    allocation=$!
    peak="$baseline"
    for _ in $(seq 1 30); do
      current="$(rss)"
      [ "$current" -le "$peak" ] || peak="$current"
      kill -0 "$allocation" 2>/dev/null || break
      sleep 1
    done
    wait "$allocation"
    allocation=''
    [ "$((peak - baseline))" -ge 201326592 ] || die 'legacy observation missed the held peak'
  else
    measure_round 1
  fi
)

expect_failure() {
  local scenario="$1" message="$2" legacy="${3:-0}" rc=0
  set +e
  (set -e; run_fixture "$scenario" "$legacy") > "$TMP/$scenario-$legacy.log" 2>&1
  rc=$?
  set -e
  [ "$rc" -ne 0 ] || fail "$scenario unexpectedly passed"
  grep -Fq "$message" "$TMP/$scenario-$legacy.log" || fail "$scenario failed for another reason"
}
set +e
(set -e; run_fixture freed-ack-race) > "$TMP/freed-ack-race-0.log" 2>&1
race_rc=$?
set -e
[ "$race_rc" = 0 ] || { cat "$TMP/freed-ack-race-0.log"; fail 'published freed acknowledgement was rejected'; }
[ -f "$TMP/freed-ack-race-0/absent-probe" ] || fail 'freed acknowledgement race was not exercised'
expect_failure freed-ack-no-marker 'allocator exited before freed acknowledgement'
expect_failure freed-ack-too-late 'allocator freed acknowledgement timed out'
expect_failure freed-ack-failed-child 'allocator failed'
for scenario in freed-ack-race freed-ack-no-marker freed-ack-too-late freed-ack-failed-child; do
  [ -f "$TMP/$scenario-0/absent-probe" ] || fail "$scenario did not exercise a real child exit after an absent probe"
  [ ! -e "$TMP/$scenario-0/markers" ] || fail "$scenario left marker files"
  ! kill -0 "$(cat "$TMP/$scenario-0/child")" 2>/dev/null || fail "$scenario left an allocator"
done
printf 'ok: freed acknowledgement race passes; missing marker, expired deadline and failed child reject and clean up\n'
expect_failure valid 'legacy observation missed the held peak' 1
set +e
(set -e; run_fixture valid) > "$TMP/valid-0.log" 2>&1
valid_rc=$?
set -e
[ "$valid_rc" = 0 ] || { cat "$TMP/valid-0.log"; fail 'held allocation failed'; }
grep -Fq 'peak_bytes=507510784' "$TMP/valid-0.log" || fail 'held peak was not measured'
[ -f "$TMP/valid-0/closed" ] || fail 'allocator did not close its mapping'
[ "$(sed -n '/held-sampled\|release/p' "$TMP/valid-0/events")" = $'held-sampled\nrelease' ] \
  || fail 'release preceded completed held sampling'
expect_failure timeout 'allocator touched acknowledgement timed out'
expect_failure child-failure 'allocator exited before touched acknowledgement'
expect_failure failed-after-free 'allocator failed'
expect_failure shortfall 'QEMU did not return at least 192 MiB after guest free'
for scenario in valid-1 valid-0 timeout-0 child-failure-0 failed-after-free-0 shortfall-0; do
  [ ! -e "$TMP/$scenario/markers" ] || fail "$scenario left marker files"
  if [ -f "$TMP/$scenario/child" ]; then
    ! kill -0 "$(cat "$TMP/$scenario/child")" 2>/dev/null || fail "$scenario left an allocator"
  fi
done
set +e
(set -e; run_fixture cold-touch) > "$TMP/cold-touch-0.log" 2>&1
cold_rc=$?
set -e
[ "$cold_rc" = 0 ] || { cat "$TMP/cold-touch-0.log"; fail '61-second cold touch was rejected'; }
grep -Fq 'peak_bytes=507510784' "$TMP/cold-touch-0.log" || fail 'cold held peak was not measured'
[ "$(sed -n '/held-sampled\|release/p' "$TMP/cold-touch-0/events")" = $'held-sampled\nrelease' ] \
  || fail 'cold allocation was released before completed held sampling'
grep -Fxq 'timeout_seconds=210 command=incus' "$TMP/cold-touch-0/events" \
  || fail 'cold allocation did not use the bounded transport timeout'
grep -Fxq 'allocator_timeout_seconds=180' "$TMP/cold-touch-0/events" \
  || fail 'cold allocation did not use the whole guest operation timeout'
expect_failure cold-touch-too-late 'allocator touched acknowledgement timed out'
for scenario in cold-touch cold-touch-too-late; do
  [ ! -e "$TMP/$scenario-0/markers" ] || fail "$scenario left marker files"
  ! kill -0 "$(cat "$TMP/$scenario-0/child")" 2>/dev/null || fail "$scenario left an allocator"
done
printf 'ok: synthetic cold touch at 61 seconds passes; at 181 seconds fails and cleans up\n'
printf 'ok: held allocation is sampled before release; failed phases clean up\n'
