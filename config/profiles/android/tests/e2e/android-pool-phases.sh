# shellcheck shell=bash
# shellcheck disable=SC2154 # owning scripts assign android_fixture before use
# Shared bounded phase markers for the Android E2E fixtures.
android_phase_now() { local up rest; read -r up rest < /proc/uptime; printf '%s\n' "${up%%.*}"; }
android_phase_end() {
  [ -n "${android_phase:-}" ] || return 0
  printf 'E2E_PHASE phase=fixture/%s-%s state=end duration_seconds=%s exit_code=%s\n' \
    "$android_fixture" "$android_phase" "$(( $(android_phase_now) - android_phase_started ))" "$1"
  android_phase=''
}
android_phase_begin() {
  android_phase_end 0
  android_phase="$1" android_phase_started="$(android_phase_now)"
  printf 'E2E_PHASE phase=fixture/%s-%s state=start duration_seconds=0 exit_code=0\n' "$android_fixture" "$android_phase"
}
