# shellcheck shell=bash
# Source-only provision boundaries; callers own installation and shell options.
# Downloads are bounded independently of the provision operation. Curl resets file
# output between retries, so a partial manifest/archive cannot reach its consumer.
android_download() {
  local status=0 stage="$3" started=$SECONDS output_bytes=unknown
  printf 'android provision: download stage=%s state=start\n' "$stage" >&2
  timeout --signal=TERM --kill-after=1s 300s \
    curl --fail --silent --location --retry 3 --retry-all-errors --retry-delay 1 \
      --connect-timeout 15 --max-time 120 --retry-max-time 300 \
      --output "$2" "$1" || status=$?
  # This is the final attempt's output, not aggregate progress across retries.
  if [ -f "$2" ] && [ ! -L "$2" ]; then
    output_bytes="$(stat -c %s -- "$2" 2>/dev/null)" || output_bytes=unknown
    case "$output_bytes" in ''|*[!0-9]*) output_bytes=unknown ;; esac
  fi
  if [ "$status" -eq 0 ]; then
    printf 'android provision: download stage=%s state=complete elapsed_seconds=%s archive_bytes=%s\n' \
      "$stage" "$((SECONDS - started))" "$output_bytes" >&2
    return 0
  fi
  printf 'android provision: download stage=%s state=failed exit=%s elapsed_seconds=%s last_output_bytes=%s\n' \
    "$stage" "$status" "$((SECONDS - started))" "$output_bytes" >&2
  rm -f -- "$2"
  return "$status"
}


android_refresh_legacy_helpers() {
  local legacy_root="$1" profile_root="$2"
  if [ -d "$legacy_root" ] && [ ! -L "$legacy_root" ]; then
    install -m 0755 "$profile_root/emulator-run.sh" "$legacy_root"/emulator-run.sh
    install -m 0755 "$profile_root/emulator-control.sh" "$legacy_root"/emulator-control.sh
  fi
}
