#!/usr/bin/env bash
# Sourced host-free fixture for acceptance-controller dispatch tests.
setup_profile_controller() { # <repository root> <controller path>
  tmp="$(mktemp -d /tmp/subyard-profile-controller.XXXXXX)"
  trap 'rm -rf -- "$tmp"' EXIT
  controller="$2"
  mkdir -p "$tmp/$(dirname "$controller")" "$tmp/dev"
  cp "$1/$controller" "$tmp/$controller"
  export PROFILE_CONTROLLER_LOG="$tmp/invocations"
  : > "$PROFILE_CONTROLLER_LOG"
  cat > "$tmp/dev/agent-e2e.sh" <<'SH'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "${PROFILE_CONTROLLER_LOG:?}"
printf '%s\n' "$*"
exit "${PROFILE_CONTROLLER_EXIT:-0}"
SH
}

assert_controller_rejects() { # <expected status> [arguments...]
  local expected="$1" rc=0
  shift
  : > "$PROFILE_CONTROLLER_LOG"
  bash "$tmp/$controller" "$@" > "$tmp/invalid" 2>&1 || rc=$?
  [ "$rc" -eq "$expected" ] && [ ! -s "$PROFILE_CONTROLLER_LOG" ]
}

assert_controller_missing_slot() {
  assert_controller_rejects 2
}

assert_controller_failure() { # [arguments...]
  local rc=0
  : > "$PROFILE_CONTROLLER_LOG"
  PROFILE_CONTROLLER_EXIT=23 bash "$tmp/$controller" "$@" > "$tmp/failure" 2>&1 || rc=$?
  [ "$rc" -eq 23 ] && [ "$(wc -l < "$PROFILE_CONTROLLER_LOG")" -eq 1 ]
}
