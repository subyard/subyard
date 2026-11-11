#!/usr/bin/env bash
# Fake artifact transport shared by the Codex and Claude provision fixtures.
set -euo pipefail
fixture="$1"
shift
out='' resume='' speed='' seconds='' connect='' proto='' redir='' fail=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    --output) out="$2"; shift 2 ;;
    --continue-at) resume="$2"; shift 2 ;;
    --speed-limit) speed="$2"; shift 2 ;;
    --speed-time) seconds="$2"; shift 2 ;;
    --connect-timeout) connect="$2"; shift 2 ;;
    --proto) proto="$2"; shift 2 ;;
    --proto-redir) redir="$2"; shift 2 ;;
    --fail) fail=1; shift ;;
    # A progressing transfer may take arbitrarily long. Reject any total deadline
    # or internal curl retry that could restart the partial behind the shell loop.
    --max-time|--retry-max-time|--retry) exit 91 ;;
    *) shift ;;
  esac
done
[ "$resume:$speed:$seconds:$connect:$proto:$redir:$fail" = '-:1024:120:20:=https:=https:1' ]
offset=0
[ ! -f "$out" ] || offset="$(stat -c %s "$out")"
printf '%s\n' "$offset" >> "$SUBYARD_TEST_TRANSFER_LOG"
scenario="${SUBYARD_TEST_TRANSFER_SCENARIO:-progress}"
if [ "$scenario" != progress ] && [ "$offset" = 0 ]; then
  head -c 64 "$fixture" > "$out"
  if [ "$scenario" = corrupt ]; then
    printf 'bad' | dd of="$out" conv=notrunc status=none
  fi
  exit 28 # Low-speed timeout after retaining a partial file.
fi
case "$scenario" in
  exhausted) exit 28 ;;
  range-refused) exit 33 ;; # Native curl refuses a 200 response to a resume request.
  ignore-range) cat "$fixture" >> "$out" ;; # Even an unsafe transport cannot bypass SHA-256.
  *) tail -c "+$((offset + 1))" "$fixture" >> "$out" ;;
esac
