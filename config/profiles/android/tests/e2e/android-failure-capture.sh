#!/usr/bin/env bash
# Keep the producer status outside the redacted, byte-limited payload.
# --foreground keeps a single-process probe in the caller's process group, which a lease owner
# stops before release; without it timeout stops the probe's own process group.
android_capture_evidence() (
  set -o pipefail
  local capture_exit=0 group=()
  if [ "${1:-}" = --foreground ]; then
    group=(--foreground)
    shift
  fi
  # Drain the probe and omit any record that crosses the redaction input cap.
  timeout "${group[@]}" --kill-after=2 5 "$@" </dev/null 2>&1 | { head -c 65536 && cat >/dev/null; } |
    python3 -c '
import sys

data = sys.stdin.buffer.read()
if len(data) == 65536:
    data = data[:data.rfind(b"\n") + 1]
sys.stdout.buffer.write(data)
' | sed -E \
    -e 's/(token|credential|secret|password|authorization|api[_-]?key)[=: ][^[:space:]]+/[redacted]/Ig' \
    -e 's/\b[0-9a-fA-F]{64}\b/[redacted]/g' \
    -e 's/([0-9]{1,3}\.){3}[0-9]{1,3}/[address]/g' \
    -e 's/([0-9a-fA-F]{1,4}:){7}[0-9a-fA-F]{1,4}/[address]/g' \
    -e 's/([0-9a-fA-F]{1,4}:)*[0-9a-fA-F]{0,4}::[0-9a-fA-F:.]*/[address]/g' \
    -e 's/([0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2}/[address]/g' |
    { head -c 65536 && cat >/dev/null; } >&2 || capture_exit=${PIPESTATUS[0]}
  printf '\ncapture_exit=%s\n' "$capture_exit" >&2
  exit "$capture_exit"
)
