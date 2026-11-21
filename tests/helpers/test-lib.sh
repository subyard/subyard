#!/usr/bin/env bash
# Failure reporting for host-free shell tests.

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
