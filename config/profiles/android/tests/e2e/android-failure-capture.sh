#!/usr/bin/env bash
# Keep the producer status outside the redacted, byte-limited payload.
android_capture_evidence() (
  set -o pipefail
  local capture_exit=0
  # Drain the probe even when only its first 64 KiB need redaction.
  timeout --kill-after=2 5 "$@" </dev/null 2>&1 | { head -c 65536 && cat >/dev/null; } | sed -E \
    -e 's/(token|credential|secret|password|authorization|api[_-]?key)[=: ][^[:space:]]+/[redacted]/Ig' \
    -e 's/\b[0-9a-fA-F]{64}\b/[redacted]/g' \
    -e 's/([0-9]{1,3}\.){3}[0-9]{1,3}/[address]/g' \
    -e 's/([0-9a-fA-F]{1,4}:){7}[0-9a-fA-F]{1,4}/[address]/g' \
    -e 's/([0-9a-fA-F]{1,4}:)*[0-9a-fA-F]{0,4}::[0-9a-fA-F:.]*/[address]/g' \
    -e 's/([0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2}/[address]/g' |
    { head -c 65536 && cat >/dev/null; } >&2 || capture_exit=${PIPESTATUS[0]}
  printf '\ncapture_exit=%s\n' "$capture_exit" >&2
)
