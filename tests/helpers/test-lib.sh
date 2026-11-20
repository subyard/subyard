#!/usr/bin/env bash
# Failure reporting and exit cleanup for host-free shell tests.

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
  TMP="$(mktemp -d)" || return
  on_exit rm -rf -- "$TMP"
}

on_exit() {
  local command
  # Quote each argument when registering so later expansion cannot change it.
  printf -v command '%q ' "$@"
  if [ "${_test_lib_trap_pid:-}" != "$BASHPID" ]; then
    _test_lib_trap_pid=$BASHPID
    _test_lib_exit_commands=()
    trap '_test_lib_exit "$?"' EXIT
    trap 'exit 130' INT
    trap 'exit 143' TERM
  fi
  _test_lib_exit_commands+=("$command")
}

_test_lib_status() { return "$1"; }

_test_lib_exit() {
  local status=$1 index errexit=
  [[ $- != *e* ]] || errexit=1
  set +e
  for ((index = ${#_test_lib_exit_commands[@]} - 1; index >= 0; index--)); do
    # A handler may fail or call exit. Give each one the original status and
    # shell options without letting it prevent the remaining cleanup.
    (
      if [ -n "$errexit" ]; then set -e; fi
      if _test_lib_status "$status"; then
        eval "${_test_lib_exit_commands[index]}"
      else
        eval "${_test_lib_exit_commands[index]}"
      fi
    )
  done
  exit "$status"
}
