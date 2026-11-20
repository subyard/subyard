#!/usr/bin/env bash
# Failure reporting and exit cleanup for host-free shell tests.

# Set a test-specific prefix after sourcing this file.
unset TEST_LIB_PREFIX

fail() {
  printf 'FAIL: %s%s\n' "${TEST_LIB_PREFIX:+$TEST_LIB_PREFIX: }" "$*" >&2
  exit 1
}

die() {
  if [ "$#" -gt 0 ]; then
    printf '%s\n' "$*" >&2
  fi
  exit "${TEST_LIB_DIE_STATUS:-2}"
}

test_tmp() {
  _test_lib_install_traps
  TMP="$(mktemp -d)" || return
  on_exit rm -rf -- "$TMP"
}

# Register all same-shell cleanup here. A later raw EXIT, INT, or TERM trap
# replaces the library's trap for that signal.
on_exit() {
  local command
  # Quote each argument when registering so later expansion cannot change it.
  printf -v command '%q ' "$@"
  _test_lib_install_traps
  _test_lib_exit_commands+=("$command")
}

_test_lib_install_traps() {
  [ "${_test_lib_trap_pid:-}" != "$BASHPID" ] || return 0
  local existing_traps
  existing_traps="$(trap -p EXIT INT TERM)"
  # Bash can report the parent's traps in a subshell before its first trap
  # assignment. A new stack can take over those inherited library traps.
  if [ -n "$existing_traps" ] && [ "$existing_traps" != "${_test_lib_owned_traps:-}" ]; then
    fail 'on_exit: an EXIT, INT, or TERM trap is already set'
  fi
  _test_lib_trap_pid=$BASHPID
  _test_lib_exit_commands=()
  trap '_test_lib_exit "$?"' EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  _test_lib_owned_traps="$(trap -p EXIT INT TERM)"
}

_test_lib_status() { return "$1"; }

_test_lib_exit() {
  local _test_lib_rc=$1 _test_lib_result=$1 _test_lib_handler_rc
  local _test_lib_index _test_lib_errexit=
  [[ $- != *e* ]] || _test_lib_errexit=1
  trap - ERR
  set +e
  for ((_test_lib_index = ${#_test_lib_exit_commands[@]} - 1; _test_lib_index >= 0; _test_lib_index--)); do
    # A handler may fail or call exit. Give each one the original status and
    # errexit setting without letting it prevent the remaining cleanup.
    (
      trap - ERR
      set +E
      if [ -n "$_test_lib_errexit" ]; then set -e; fi
      if _test_lib_status "$_test_lib_rc"; then
        eval "${_test_lib_exit_commands[_test_lib_index]}"
      else
        eval "${_test_lib_exit_commands[_test_lib_index]}"
      fi
    )
    _test_lib_handler_rc=$?
    if [ "$_test_lib_result" -eq 0 ]; then
      _test_lib_result=$_test_lib_handler_rc
    fi
  done
  exit "$_test_lib_result"
}
