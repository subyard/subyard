#!/usr/bin/env bash
# Only the final fixed mutation wrapper can select its own marked stage cleanup.
subyard_prepared_installed_finish() {
  local result=$1
  trap - EXIT
  if [ "$result" -eq 0 ] && [ "${SUBYARD_PREPARED_INSTALLED_CLEANUP:-}" = 1 ]; then
    local -a stage_command=(python3 -B "$SCRIPT_DIR/lib/native-installed-stage.py")
    case "${SUBYARD_PREPARED_INSTALLED_OWNER:-}" in
      host-power|test-vms-host-sink)
        if [ "$(id -u)" = 0 ] && [ "${SUBYARD_USER:-root}" != root ]; then
          stage_command=(runuser -u "$SUBYARD_USER" -- "${stage_command[@]}")
        fi ;;
    esac
    "${stage_command[@]}" finish \
      "$SUBYARD_PREPARED_INSTALLED_STAGE" "$SUBYARD_PREPARED_INSTALLED_BINDING" \
      "$SUBYARD_PREPARED_INSTALLED_RECIPE" "$SUBYARD_PREPARED_INSTALLED_OWNER" || result=2
  fi
  exit "$result"
}
if [ "${SUBYARD_PREPARED_INSTALLED_CLEANUP:-}" = 1 ]; then
  trap 'subyard_prepared_installed_finish "$?"' EXIT
fi
