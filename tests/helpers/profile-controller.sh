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

assert_owner_yard_sg_arguments() ( # <owner script>
  # Simulate membership refreshed in the account database but absent in this process.
  id() { [ "$#" -eq 2 ] && printf 'incus-admin\n' || printf 'dev\n'; }
  sg() { [ "$1:$2" = incus-admin:-c ] && dash -c "$3"; }
  # Read by the extracted yard function.
  # shellcheck disable=SC2034
  local YARD_NAME=fixture YARD_BIN="$tmp/yard stub" CANDIDATE_ENGINE="$tmp/yard stub"
  local script=$'printf "%s\\n" "$1"\n# comment with an apostrophe: \''
  cat > "$YARD_BIN" <<'SH'
#!/bin/sh
[ "$#" -eq 8 ] && [ "$1 $2 $3 $4 $5" = '-Y fixture shell -- sh' ] && [ "$6" = -c ] || exit 2
exec sh -c "$7" sh "$8"
SH
  chmod 0700 "$YARD_BIN"
  eval "$(sed -n '/^yard() {/,/^}/p' "$1")"
  [ "$(yard shell -- sh -c "$script" 'literal $(false)')" = 'literal $(false)' ]
)
